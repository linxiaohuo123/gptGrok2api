package httpapi

import (
	"encoding/json"
	"testing"
	"time"
)

// toWireShape 把 Go 值往返一次 JSON，得到前端真正看到的形状。
//
// 直接断言 Go 值会误判：int 与 float64、[]map[string]any 与 []any 的差异
// 只存在于进程内，序列化后统一成 JSON 的数字与数组。契约说的就是线上形状，
// 所以守卫必须建立在这一层。
func toWireShape(t *testing.T, value any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// assertMonitorPresentationContract 逐字段核对前端 MonitorRecordPresentation。
//
// web-vue/src/api/monitor.ts 把 presentation 声明为**必填**，而 MonitorActiveRow /
// MonitorRecentRow / MonitorSlowCard 以及 monitorView 的签名函数都直接点属性。
// 整个对象缺失时，Vue 的渲染错误处理会把整棵子树渲染成空——监控页白屏，
// 控制台只留一句报错，用户什么也看不到。
func assertMonitorPresentationContract(t *testing.T, raw map[string]any, path string) {
	t.Helper()
	presentation, ok := raw["presentation"].(map[string]any)
	if !ok {
		t.Fatalf("%s: 缺 presentation 对象，实际 %T —— 监控页会整页白屏", path, raw["presentation"])
	}
	for _, key := range []string{
		"status_label", "status_tone", "stage_text", "error_text", "duration_text",
		"metric_digest", "egress_text", "account_attempt_text", "account_egress_text",
		"tracked_duration_ms", "untracked_duration_ms", "slow_metrics",
		"slow_reason_code", "slow_reason",
	} {
		if _, ok := presentation[key]; !ok {
			t.Errorf("%s.presentation 缺字段 %q", path, key)
		}
	}
	tone, ok := presentation["status_tone"].(string)
	if !ok {
		t.Fatalf("%s.presentation.status_tone 必须是字符串，实际 %T", path, presentation["status_tone"])
	}
	switch tone {
	case "success", "danger", "warning", "info", "muted":
	default:
		t.Fatalf("%s.presentation.status_tone = %q 不在 MonitorTone 枚举内", path, tone)
	}
	for _, key := range []string{"status_label", "stage_text", "error_text", "duration_text", "metric_digest", "egress_text", "account_attempt_text", "account_egress_text", "slow_reason_code", "slow_reason"} {
		if _, ok := presentation[key].(string); !ok {
			t.Errorf("%s.presentation.%s 必须是字符串，实际 %T", path, key, presentation[key])
		}
	}
	// slow_metrics 被 MonitorSlowCard 直接 v-for，必须是数组且元素形状完整。
	slowMetrics, ok := presentation["slow_metrics"].([]any)
	if !ok {
		t.Fatalf("%s.presentation.slow_metrics 必须是数组，实际 %T", path, presentation["slow_metrics"])
	}
	for index, item := range slowMetrics {
		entry, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("%s.presentation.slow_metrics[%d] 必须是对象", path, index)
		}
		for _, key := range []string{"key", "label", "value_text"} {
			if _, ok := entry[key].(string); !ok {
				t.Fatalf("%s.presentation.slow_metrics[%d].%s 必须是字符串", path, index, key)
			}
		}
		if _, ok := entry["value_ms"].(float64); !ok {
			t.Fatalf("%s.presentation.slow_metrics[%d].value_ms 必须是数字", path, index)
		}
		if _, ok := entry["important"].(bool); !ok {
			t.Fatalf("%s.presentation.slow_metrics[%d].important 必须是布尔", path, index)
		}
	}
}

// 回归：每条监控记录都必须带完整的 presentation。
func TestMonitorRecordCarriesPresentation(t *testing.T) {
	cases := []monitorRecord{
		{CallID: "c-running", Status: "running", Stage: "image_generating", Endpoint: "/v1/images/generations", Model: "gpt-image-2", StartedAt: time.Now().UnixMilli(), Duration: 1200},
		{CallID: "c-success", Status: "success", Stage: "completed", Endpoint: "/v1/chat/completions", StartedAt: time.Now().Add(-3 * time.Second).UnixMilli(), EndedAt: time.Now().UnixMilli(), Duration: 3000},
		{CallID: "c-failed", Status: "failed", Stage: "failed", Error: "no available accounts", StartedAt: time.Now().Add(-time.Second).UnixMilli(), EndedAt: time.Now().UnixMilli(), Duration: 1000},
		// 慢请求：应当产出 slow_metrics 与 slow_reason。
		{CallID: "c-slow", Status: "success", Stage: "completed", StartedAt: time.Now().Add(-40 * time.Second).UnixMilli(), EndedAt: time.Now().UnixMilli(), Duration: 40_000,
			Metrics: map[string]any{"account_wait_ms": 31_000, "upstream_ms": 8_000}},
	}
	for _, record := range cases {
		assertMonitorPresentationContract(t, toWireShape(t, monitorRecordMap(record)), record.CallID)
	}

	// 慢请求必须真的挑出慢因，而不是给出空串。
	slow := toWireShape(t, monitorRecordMap(cases[3]))
	slowMetrics := slow["presentation"].(map[string]any)["slow_metrics"].([]any)
	if len(slowMetrics) == 0 {
		t.Fatal("40 秒的请求未产出 slow_metrics")
	}
	if reason := slow["presentation"].(map[string]any)["slow_reason"].(string); reason == "" {
		t.Fatal("慢请求必须给出 slow_reason，否则 MonitorSlowCard 只显示一个空段落")
	}

	// 失败记录的错误必须出现在 error_text 上——详情抽屉读的就是它。
	failed := toWireShape(t, monitorRecordMap(cases[2]))
	if got := failed["presentation"].(map[string]any)["error_text"]; got != "no available accounts" {
		t.Fatalf("error_text 应为失败原因，实际 %#v", got)
	}
	if tone := failed["presentation"].(map[string]any)["status_tone"]; tone != "danger" {
		t.Fatalf("失败记录的 status_tone 应为 danger，实际 %#v", tone)
	}
}

// 回归：空的监控快照里 slow 必须是 []，不能是 null。
//
// 前端写的是 monitorData.value?.slow.slice(0, 8) —— `?.` 只护住了
// monitorData.value，没护住 .slow。Go 侧 append([]T(nil), 空...) 返回 nil，
// 序列化成 null，null.slice 直接抛 TypeError，整个 computed 失败、页面空白。
func TestMonitorSnapshotNeverReturnsNullSlices(t *testing.T) {
	server := New(adminTestConfig(t.TempDir()))
	snapshot := toWireShape(t, server.monitorSnapshotWithHistory())

	if snapshot["slow"] == nil {
		t.Fatal("slow 是 null —— 前端 monitorData.value?.slow.slice() 会抛 TypeError")
	}
	for _, key := range []string{"slow", "active", "recent", "events"} {
		if _, ok := snapshot[key].([]any); !ok {
			t.Fatalf("%s 必须是数组，实际 %T", key, snapshot[key])
		}
	}

	// 快照里的每条记录也必须带 presentation。
	for index, item := range snapshot["recent"].([]any) {
		record, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("recent[%d] 必须是对象", index)
		}
		assertMonitorPresentationContract(t, record, "recent")
	}
}

// assertCallPresentationContract 核对前端 CallPresentation 的必填字段。
//
// 前端 logDurationDisplay 读 presentation.duration.text / .breakdown，
// 而它在 systemLogRowSignature 里被调用——那是渲染路径上的函数。缺了它，
// 日志表格**每一行**都抛 TypeError，整张表渲染不出来。
func assertCallPresentationContract(t *testing.T, raw map[string]any, path string) {
	t.Helper()
	presentation, ok := raw["presentation"].(map[string]any)
	if !ok {
		t.Fatalf("%s: 缺 presentation 对象，实际 %T", path, raw["presentation"])
	}
	for _, key := range []string{"request", "execution", "status", "result"} {
		if _, ok := presentation[key].(map[string]any); !ok {
			t.Fatalf("%s.presentation.%s 必须是对象，实际 %T", path, key, presentation[key])
		}
	}
	if _, ok := presentation["summary_text"].(string); !ok {
		t.Fatalf("%s.presentation.summary_text 必须是字符串，实际 %T —— summaryText() 直接读它", path, presentation["summary_text"])
	}
	if _, ok := presentation["is_failure"].(bool); !ok {
		t.Fatalf("%s.presentation.is_failure 必须是布尔，实际 %T", path, presentation["is_failure"])
	}
	duration, ok := presentation["duration"].(map[string]any)
	if !ok {
		t.Fatalf("%s.presentation.duration 必须是对象，实际 %T —— logDurationDisplay() 直接读它", path, presentation["duration"])
	}
	for _, key := range []string{"text", "breakdown", "tone"} {
		if _, ok := duration[key]; !ok {
			t.Fatalf("%s.presentation.duration 缺字段 %q", path, key)
		}
	}
	if _, ok := duration["text"].(string); !ok {
		t.Fatalf("%s.presentation.duration.text 必须是字符串", path)
	}
	tone, ok := duration["tone"].(string)
	if !ok {
		t.Fatalf("%s.presentation.duration.tone 必须是字符串", path)
	}
	switch tone {
	case "success", "danger", "warning", "info", "muted":
	default:
		t.Fatalf("%s.presentation.duration.tone = %q 不在 PresentationTone 枚举内", path, tone)
	}
	status, _ := presentation["status"].(map[string]any)
	if _, ok := status["label"].(string); !ok {
		t.Fatalf("%s.presentation.status.label 必须是字符串", path)
	}
	if _, ok := status["tone"].(string); !ok {
		t.Fatalf("%s.presentation.status.tone 必须是字符串", path)
	}
}

// 回归：日志列表的每一条都必须带齐 CallPresentation。
func TestLogRowCarriesCallPresentation(t *testing.T) {
	cases := []map[string]any{
		{"id": "log-1", "time": "2026-09-10T00:00:00Z", "type": "call", "summary": "生图请求",
			"detail": map[string]any{"endpoint": "/v1/images/generations", "model": "gpt-image-2", "status": "success", "duration_ms": 4200,
				"timings_ms": map[string]any{"account_wait_ms": 900, "upstream_ms": 3300}}},
		{"id": "log-2", "time": "2026-09-10T00:01:00Z", "type": "call", "summary": "失败请求",
			"detail": map[string]any{"endpoint": "/v1/chat/completions", "status": "failed", "error": "boom", "duration_ms": 0}},
	}
	for _, item := range cases {
		assertCallPresentationContract(t, toWireShape(t, formatCallSummary(item)), stringValue(item["id"]))
	}

	// 有 timings_ms 时 breakdown 必须有内容，而不是空串。
	first := toWireShape(t, formatCallSummary(cases[0]))
	breakdown := first["presentation"].(map[string]any)["duration"].(map[string]any)["breakdown"].(string)
	if breakdown == "" {
		t.Fatal("提供了 timings_ms 却没有生成 duration.breakdown")
	}
	// duration_ms = 4200 落在 5 秒以内，语气应为 success。
	if tone := first["presentation"].(map[string]any)["duration"].(map[string]any)["tone"]; tone != "success" {
		t.Fatalf("4.2 秒的请求 tone 应为 success，实际 %#v", tone)
	}
}

// formatDurationText 是日志与监控共用的时长渲染，钉住它的边界。
func TestFormatDurationText(t *testing.T) {
	cases := map[int]string{0: "—", -5: "—", 1: "1ms", 999: "999ms", 1500: "1.5s", 59_999: "60.0s", 60_000: "1m00s", 125_000: "2m05s"}
	for input, want := range cases {
		if got := formatDurationText(input); got != want {
			t.Errorf("formatDurationText(%d) = %q，期望 %q", input, got, want)
		}
	}
}
