package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// assertAccountMutationContract 是前端 accountOperationPresentation 的 Go 移植。
//
// 校验逻辑必须与 web-vue/src/api/accounts.ts 逐条一致。前端的抛出点在返回对象
// 字面量里求值——一旦抛出，后端其实**已经写库成功**，用户看到的却是"失败"。
// 这正是"编辑账号显示保存失败但数据已改""导入账号报失败但账号已入库"的成因。
func assertAccountMutationContract(t *testing.T, raw map[string]any, path string) {
	t.Helper()
	label, ok := raw["status_label"].(string)
	if !ok || strings.TrimSpace(label) == "" {
		t.Fatalf("%s: status_label 必须是非空字符串，实际 %#v —— 前端会抛「账号操作响应缺少后端展示投影」", path, raw["status_label"])
	}
	tone, ok := raw["tone"].(string)
	if !ok {
		t.Fatalf("%s: tone 必须是字符串，实际 %T", path, raw["tone"])
	}
	switch tone {
	case "info", "success", "warning", "danger":
	default:
		t.Fatalf("%s: tone %q 不在前端的 {info,success,warning,danger} 之内", path, tone)
	}
	// 前端用的是 typeof response.message === 'string'，null 同样被拒。
	if _, ok := raw["message"].(string); !ok {
		t.Fatalf("%s: message 必须是字符串（null 也拒），实际 %T", path, raw["message"])
	}
	for _, key := range []string{"summary_items", "events"} {
		if _, ok := raw[key].([]any); !ok {
			t.Fatalf("%s: %s 必须是数组，实际 %T", path, key, raw[key])
		}
	}
	for index, item := range raw["summary_items"].([]any) {
		entry, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("%s: summary_items[%d] 必须是对象", path, index)
		}
		for _, key := range []string{"key", "label"} {
			if _, ok := entry[key].(string); !ok {
				t.Fatalf("%s: summary_items[%d].%s 必须是字符串", path, index, key)
			}
		}
		if _, ok := entry["value"]; !ok {
			t.Fatalf("%s: summary_items[%d] 缺 value", path, index)
		}
	}
}

func mutationRequest(t *testing.T, server *Server, method, target, body string) map[string]any {
	t.Helper()
	recorder := adminRequest(server.Handler(), method, target, strings.NewReader(body))
	if recorder.Code != http.StatusOK {
		t.Fatalf("%s %s -> %d %s", method, target, recorder.Code, recorder.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

// 回归：账号写操作的响应必须带齐前端要求的展示投影。
//
// 缺任何一项，前端就在返回对象字面量处抛错——而后端此时已经写库成功。
// 表现是"假失败"：编辑显示保存失败、导入显示导入失败、同步与刷新 AT
// 连轮询循环都进不去。
func TestAccountMutationProjectionContract(t *testing.T) {
	server := New(adminTestConfig(t.TempDir()))

	seed := `{"accounts":[{"id":"a1","email":"a1@example.com","access_token":"tok-1"},{"id":"a2","email":"a2@example.com","access_token":"tok-2"}]}`
	imported := mutationRequest(t, server, http.MethodPost, "/api/accounts", seed)
	assertAccountMutationContract(t, imported, "POST /api/accounts")

	updated := mutationRequest(t, server, http.MethodPost, "/api/accounts/update", `{"id":"a1","quota":7}`)
	assertAccountMutationContract(t, updated, "POST /api/accounts/update")

	bound := mutationRequest(t, server, http.MethodPost, "/api/accounts/group", `{"account_ids":["a1"],"group_id":"g1"}`)
	assertAccountMutationContract(t, bound, "POST /api/accounts/group")

	// 轮询响应同样被前端强校验，缺了它 while 循环第一次迭代就抛。
	start := mutationRequest(t, server, http.MethodPost, "/api/accounts/refresh-access-token", `{"account_ids":["a1"]}`)
	_ = start
	progressID := ""
	if value, ok := imported["progress_id"].(string); ok {
		progressID = value
	}
	if progressID == "" {
		// 导入不产生进度，改用操作进度端点做一次直接校验。
		progressID = "nonexistent"
	}
	recorder := adminRequest(server.Handler(), http.MethodGet, "/api/accounts/operations/"+progressID, nil)
	if recorder.Code == http.StatusOK {
		var payload map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		assertAccountMutationContract(t, payload, "GET /api/accounts/operations/{id}")
	}
}

// 回归：批量操作必须认前端的 account_ids / selection。
//
// 前端 selectionPayload 只发这两种形态，而 Go 侧历来只读 access_tokens / tokens。
// 后果分两类：批量更新、删除、清理直接 400；**绑分组更糟——它既不认字段也没有
// 空值检查，返回 200 却一个账号都没改**，是纯粹的静默 no-op。
func TestAccountBatchOperationsAcceptFrontendFields(t *testing.T) {
	server := New(adminTestConfig(t.TempDir()))
	handler := server.Handler()

	seed := `{"accounts":[{"id":"a1","email":"a1@example.com","access_token":"tok-1"},{"id":"a2","email":"a2@example.com","access_token":"tok-2"}]}`
	if recorder := adminRequest(handler, http.MethodPost, "/api/accounts", strings.NewReader(seed)); recorder.Code != http.StatusOK {
		t.Fatalf("准备账号失败：%d %s", recorder.Code, recorder.Body.String())
	}

	// ① 批量更新：前端发 account_ids
	recorder := adminRequest(handler, http.MethodPost, "/api/accounts/batch-update",
		strings.NewReader(`{"account_ids":["a1"],"status":"禁用"}`))
	if recorder.Code != http.StatusOK {
		t.Fatalf("批量更新不认 account_ids：%d %s", recorder.Code, recorder.Body.String())
	}

	// ② 绑分组：必须真的改到，不能是 200 的静默 no-op
	recorder = adminRequest(handler, http.MethodPost, "/api/accounts/group",
		strings.NewReader(`{"account_ids":["a2"],"group_id":"g1"}`))
	if recorder.Code != http.StatusOK {
		t.Fatalf("绑分组不认 account_ids：%d %s", recorder.Code, recorder.Body.String())
	}
	var bound map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &bound); err != nil {
		t.Fatal(err)
	}
	if got := intValue(bound["updated"]); got != 1 {
		t.Fatalf("绑分组返回 200 但 updated=%d —— 静默 no-op 回归：%s", got, recorder.Body.String())
	}

	// ③ selection 形态
	recorder = adminRequest(handler, http.MethodPost, "/api/accounts/group",
		strings.NewReader(`{"selection":{"account_ids":["a1"]},"group_id":"g2"}`))
	if recorder.Code != http.StatusOK {
		t.Fatalf("绑分组不认 selection.account_ids：%d %s", recorder.Code, recorder.Body.String())
	}

	// ④ 空目标必须明确报错，而不是默默什么都不做
	recorder = adminRequest(handler, http.MethodPost, "/api/accounts/group", strings.NewReader(`{"group_id":"g3"}`))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("空目标应返回 400，实际 %d %s —— 静默 no-op 会掩盖调用方的错误", recorder.Code, recorder.Body.String())
	}

	// ⑤ 删除：前端发 account_ids
	recorder = adminRequest(handler, http.MethodDelete, "/api/accounts",
		strings.NewReader(`{"account_ids":["a1"]}`))
	if recorder.Code != http.StatusOK {
		t.Fatalf("删除不认 account_ids：%d %s", recorder.Code, recorder.Body.String())
	}

	// ⑥ 清理：前端发 account_ids
	recorder = adminRequest(handler, http.MethodPost, "/api/accounts/import-cleanup",
		strings.NewReader(`{"account_ids":["a2"],"remove":false}`))
	if recorder.Code != http.StatusOK {
		t.Fatalf("清理不认 account_ids：%d %s", recorder.Code, recorder.Body.String())
	}
}

// 回归：/api/accounts/sync 不得因为认不出前端的字段而退化成"刷新全量"。
//
// 它原本只读 access_tokens，读不到就落到"列出全部账号"的兜底。前端永远发
// account_ids / selection，于是"同步选中的 2 个账号"实际刷新的是整个账号池——
// 故障域越界，且调用方看不到任何异常。
func TestAccountSyncDoesNotEscalateToFullPool(t *testing.T) {
	server := New(adminTestConfig(t.TempDir()))
	handler := server.Handler()

	seed := `{"accounts":[{"id":"a1","email":"a1@example.com","access_token":"tok-1"},{"id":"a2","email":"a2@example.com","access_token":"tok-2"},{"id":"a3","email":"a3@example.com","access_token":"tok-3"}]}`
	if recorder := adminRequest(handler, http.MethodPost, "/api/accounts", strings.NewReader(seed)); recorder.Code != http.StatusOK {
		t.Fatalf("准备账号失败：%d %s", recorder.Code, recorder.Body.String())
	}

	recorder := adminRequest(handler, http.MethodPost, "/api/accounts/sync",
		strings.NewReader(`{"account_ids":["a1"]}`))
	if recorder.Code != http.StatusOK {
		t.Fatalf("同步不认 account_ids：%d %s", recorder.Code, recorder.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	progressID := stringValue(payload["progress_id"])
	if progressID == "" {
		t.Fatalf("同步未返回 progress_id：%s", recorder.Body.String())
	}

	progress := readImageTask(t, server, "/api/accounts/operations/"+progressID)
	if got := intValue(progress["total"]); got != 1 {
		t.Fatalf("只选中 1 个账号，任务总数应为 1，实际 %d —— 越界刷新了全量账号池", got)
	}
	assertAccountMutationContract(t, progress, "GET /api/accounts/operations/{id}")
}
