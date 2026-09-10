package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// assertImageTaskContract 是前端 parseImageTask 的 Go 移植。
//
// 契约测试的价值全在于此：校验逻辑必须与 web-vue/src/api/imageTasks.ts
// 里那些抛错的分支逐条一致。若这里放宽了，测试通过而页面照旧打不开。
func assertImageTaskContract(t *testing.T, raw map[string]any, path string) {
	t.Helper()
	fail := func(format string, args ...any) {
		t.Helper()
		t.Fatalf(path+": "+format, args...)
	}

	status, ok := raw["status"].(string)
	if !ok {
		fail("status 必须是字符串，实际 %T", raw["status"])
	}
	switch status {
	case "queued", "running", "success", "partial_success", "failed", "text_review":
	default:
		fail("status %q 不在前端枚举内 —— parseImageTask 会在这里抛错", status)
	}

	terminal, ok := raw["terminal"].(bool)
	if !ok {
		fail("terminal 必须是布尔，实际 %T", raw["terminal"])
	}
	results, ok := raw["results"].([]any)
	if !ok {
		fail("results 必须是数组，实际 %T", raw["results"])
	}
	if expected := status != "queued" && status != "running"; terminal != expected {
		fail("terminal=%v 与 status=%q 不自洽（前端要求 %v）", terminal, status, expected)
	}

	requested := contractInt(t, raw, "requested_count", path)
	if requested < 1 || requested > 4 {
		fail("requested_count=%d 超出前端允许的 1..4", requested)
	}
	succeeded := contractInt(t, raw, "succeeded_count", path)
	failedCount := contractInt(t, raw, "failed_count", path)
	pending := contractInt(t, raw, "pending_count", path)
	if succeeded != len(results) {
		fail("succeeded_count=%d 必须等于 results.length=%d", succeeded, len(results))
	}
	if terminal && pending != 0 {
		fail("终态任务的 pending_count 必须为 0，实际 %d", pending)
	}
	if !terminal && failedCount != 0 {
		fail("进行中任务的 failed_count 必须为 0，实际 %d", failedCount)
	}

	mode := stringValue(raw["mode"])
	if mode != "generate" && mode != "edit" {
		fail("mode %q 必须是 generate | edit", mode)
	}
	actions, ok := raw["actions"].(map[string]any)
	if !ok {
		fail("actions 必须是对象，实际 %T", raw["actions"])
	}
	if _, ok := actions["resume_poll"].(bool); !ok {
		fail("actions.resume_poll 必须是布尔，实际 %T", actions["resume_poll"])
	}

	for _, key := range []string{"id", "model", "size", "quality", "stage_code", "stage_label", "created_at", "updated_at", "error_code", "public_error"} {
		if _, ok := raw[key].(string); !ok {
			fail("%s 必须是字符串，实际 %T", key, raw[key])
		}
	}
	for _, key := range []string{"duration_ms", "elapsed_ms"} {
		value, exists := raw[key]
		if !exists {
			fail("缺字段 %s", key)
		}
		if value != nil {
			if _, ok := value.(float64); !ok {
				fail("%s 必须是数字或 null，实际 %T", key, value)
			}
		}
	}

	for index, item := range results {
		asset, ok := item.(map[string]any)
		if !ok {
			fail("results[%d] 必须是对象，实际 %T", index, item)
		}
		for _, key := range []string{"url", "path", "b64_json", "revised_prompt"} {
			if _, ok := asset[key].(string); !ok {
				fail("results[%d].%s 必须是字符串，实际 %T", index, key, asset[key])
			}
		}
		for _, key := range []string{"width", "height"} {
			value := asset[key]
			if value == nil {
				continue
			}
			number, ok := value.(float64)
			if !ok {
				fail("results[%d].%s 必须是正数与 null 之一，实际 %T", index, key, value)
			}
			// 前端明确拒绝 0：parseTaskAsset 的 'positive dimensions or null'。
			if number <= 0 {
				fail("results[%d].%s = %v，前端判为非法尺寸", index, key, number)
			}
		}
	}
}

func contractInt(t *testing.T, raw map[string]any, key, path string) int {
	t.Helper()
	value, exists := raw[key]
	if !exists {
		t.Fatalf("%s: 缺字段 %s", path, key)
	}
	number, ok := value.(float64)
	if !ok {
		t.Fatalf("%s: %s 必须是整数，实际 %T", path, key, value)
	}
	return int(number)
}

func taskServerWith(t *testing.T, tasks ...*imageTaskState) *Server {
	t.Helper()
	server := New(adminTestConfig(t.TempDir()))
	server.imageTaskMu.Lock()
	for _, task := range tasks {
		if task.OwnerID == "" {
			task.OwnerID = "admin"
		}
		server.imageTasks[task.ID] = task
	}
	server.imageTaskMu.Unlock()
	return server
}

func readImageTask(t *testing.T, server *Server, target string) map[string]any {
	t.Helper()
	recorder := adminRequest(server.Handler(), http.MethodGet, target, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("%s 状态码 %d: %s", target, recorder.Code, recorder.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

// 回归：任务详情与列表的每一条都必须满足前端 parseImageTask 的全部不变量。
//
// 覆盖三种状态：进行中、成功、失败。此前 imageTaskPublic 只吐
// id/status/mode/model/n/size/quality/created_at/updated_at —— 前端在
// expectBoolean(raw.terminal) 处直接抛 "Image task response contract mismatch"，
// Studio 的生图链路整体不可用（后端其实在正常出图）。
func TestImageTaskContractAcrossStates(t *testing.T) {
	now := time.Now().UTC()
	stamp := now.Format(time.RFC3339)
	later := now.Add(3 * time.Second).Format(time.RFC3339)

	queued := &imageTaskState{ID: "task-queued", Status: "queued", Mode: "generate", Model: "gpt-image-2", N: 2, Size: "1024x1024", Quality: "auto", CreatedAt: stamp, UpdatedAt: stamp}
	running := &imageTaskState{ID: "task-running", Status: "running", Mode: "generate", Model: "gpt-image-2", N: 2, Size: "1024x1024", Quality: "auto", CreatedAt: stamp, UpdatedAt: stamp}
	success := &imageTaskState{ID: "task-success", Status: "success", Mode: "generate", Model: "gpt-image-2", N: 1, Size: "1024x1024", Quality: "auto", CreatedAt: stamp, UpdatedAt: later,
		Data: []map[string]any{{"url": "/v1/files/image?id=abc", "width": "1024", "height": "1024"}}}
	failed := &imageTaskState{ID: "task-failed", Status: "error", Mode: "generate", Model: "gpt-image-2", N: 1, Size: "1024x1024", Quality: "auto", CreatedAt: stamp, UpdatedAt: later,
		Error: "no available accounts"}

	server := taskServerWith(t, queued, running, success, failed)

	for _, task := range []*imageTaskState{queued, running, success, failed} {
		payload := readImageTask(t, server, "/api/image-tasks/"+task.ID)
		assertImageTaskContract(t, payload, "/api/image-tasks/"+task.ID)
	}

	// 列表响应：前端 parseImageTasksResponse 要求 items 与 missing_ids 都存在。
	list := readImageTask(t, server, "/api/image-tasks")
	items, ok := list["items"].([]any)
	if !ok {
		t.Fatalf("items 必须是数组，实际 %T", list["items"])
	}
	if _, ok := list["missing_ids"].([]any); !ok {
		t.Fatalf("missing_ids 必须是数组，实际 %T", list["missing_ids"])
	}
	if len(items) != 4 {
		t.Fatalf("应有 4 条任务，实际 %d", len(items))
	}
	for index, item := range items {
		row, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("items[%d] 必须是对象", index)
		}
		assertImageTaskContract(t, row, "items["+stringValue(row["id"])+"]")
	}

	// 状态语义必须如实：内部 "error" 不得原样透出。
	detail := readImageTask(t, server, "/api/image-tasks/task-failed")
	if got := stringValue(detail["status"]); got != "failed" {
		t.Errorf("内部 error 应映射为 failed，实际 %q —— 前端枚举里没有 error", got)
	}
	if terminal, _ := detail["terminal"].(bool); !terminal {
		t.Error("失败任务必须是终态")
	}
}

// 回归：未产出的张数在终态计入失败、在非终态计入等待——两者不可同时非零。
func TestImageTaskCountsStaySelfConsistent(t *testing.T) {
	now := time.Now().UTC()
	stamp := now.Format(time.RFC3339)
	partial := &imageTaskState{ID: "task-partial", Status: "success", Mode: "generate", Model: "gpt-image-2", N: 3, Size: "1024x1024", Quality: "auto", CreatedAt: stamp, UpdatedAt: stamp,
		Data: []map[string]any{{"url": "/v1/files/image?id=one"}}}

	server := taskServerWith(t, partial)
	payload := readImageTask(t, server, "/api/image-tasks/task-partial")
	assertImageTaskContract(t, payload, "task-partial")

	if got := intValue(payload["requested_count"]); got != 3 {
		t.Errorf("requested_count 应为 3，实际 %d", got)
	}
	if got := intValue(payload["succeeded_count"]); got != 1 {
		t.Errorf("succeeded_count 应为 1，实际 %d", got)
	}
	if got := intValue(payload["failed_count"]); got != 2 {
		t.Errorf("终态下未产出的 2 张应计入 failed_count，实际 %d", got)
	}
	if got := intValue(payload["pending_count"]); got != 0 {
		t.Errorf("终态任务的 pending_count 必须为 0，实际 %d", got)
	}
	if got := stringValue(payload["status"]); got != "partial_success" {
		t.Errorf("部分产出应为 partial_success，实际 %q", got)
	}
}

// 回归：任务创建必须拒绝超出 1..4 的张数。
//
// 前端 normalizeImageCount 把 n 钳在 1..4，且 parseImageTask 拒绝
// requested_count > 4。后端此前只兜 0 -> 1、没有上界，一条 n=1000 的任务
// 进入列表就让整个 ImageTaskResponse 解析失败——一条坏数据炸掉整页。
func TestImageTaskCreationRejectsOutOfRangeCount(t *testing.T) {
	server := New(adminTestConfig(t.TempDir()))
	handler := server.Handler()

	for _, body := range []string{
		`{"client_task_id":"t1","prompt":"p","n":5}`,
		`{"client_task_id":"t2","prompt":"p","n":1000}`,
		`{"client_task_id":"t3","prompt":"p","n":-3}`,
	} {
		recorder := adminRequest(handler, http.MethodPost, "/api/image-tasks/generations", strings.NewReader(body))
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("n 越界必须返回 400，body=%s 实际 %d %s", body, recorder.Code, recorder.Body.String())
		}
	}

	// 边界内必须放行（会进入异步执行，因此只断言不是参数错误）。
	for _, body := range []string{
		`{"client_task_id":"ok-1","prompt":"p","n":1}`,
		`{"client_task_id":"ok-4","prompt":"p","n":4}`,
	} {
		recorder := adminRequest(handler, http.MethodPost, "/api/image-tasks/generations", strings.NewReader(body))
		if recorder.Code == http.StatusBadRequest {
			t.Errorf("边界内的 n 不应被拒，body=%s 实际 %d %s", body, recorder.Code, recorder.Body.String())
		}
	}
}

// 回归：任务资产必须补齐契约要求的四个字符串字段。
//
// provider 的 Resolve 每条只产出 url 或 b64_json 之一，而前端
// parseTaskAsset 对四个字段都调 expectString——缺一个就抛错。
func TestImageTaskAssetsFillMissingFields(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)
	task := &imageTaskState{ID: "task-assets", Status: "success", Mode: "generate", Model: "gpt-image-2", N: 2, Size: "1024x1024", Quality: "auto", CreatedAt: now, UpdatedAt: now,
		Data: []map[string]any{
			{"url": "/v1/files/image?id=one"},
			{"b64_json": "aGVsbG8="},
		}}

	server := taskServerWith(t, task)
	payload := readImageTask(t, server, "/api/image-tasks/task-assets")
	assertImageTaskContract(t, payload, "task-assets")

	items, _ := payload["results"].([]any)
	if len(items) != 2 {
		t.Fatalf("应有 2 条结果，实际 %d", len(items))
	}
	second, _ := items[1].(map[string]any)
	if got := stringValue(second["url"]); got != "" {
		t.Errorf("只有 b64_json 的结果，url 应补空串，实际 %q", got)
	}
	if got := stringValue(second["b64_json"]); got != "aGVsbG8=" {
		t.Errorf("b64_json 丢失：%q", got)
	}
	// 缺 width/height 时必须是 null，不能是 0。
	if value := second["width"]; value != nil {
		t.Errorf("缺尺寸时 width 应为 null，实际 %v", value)
	}
}
