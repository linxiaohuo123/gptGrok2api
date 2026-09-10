// [INPUT]: accounts/proxy
// [OUTPUT]: 运行时监控：runtimeMonitor、snapshot/detail/enrich、redactProxyError、调用日志落盘
// [POS]: 监控记录与代理测试的后端。快照必须深拷贝，脱敏必须一次替换到位。
// [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md

package httpapi

import (
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

type runtimeMonitor struct {
	mu        sync.Mutex
	active    map[string]*monitorRecord
	completed []monitorRecord
	limit     int
}

type monitorRecord struct {
	CallID       string           `json:"call_id"`
	Endpoint     string           `json:"endpoint"`
	Model        string           `json:"model"`
	Summary      string           `json:"summary,omitempty"`
	Status       string           `json:"status"`
	Stage        string           `json:"stage"`
	StartedAt    int64            `json:"started_ts"`
	UpdatedAt    int64            `json:"updated_ts"`
	EndedAt      int64            `json:"ended_ts,omitempty"`
	Duration     int64            `json:"duration_ms,omitempty"`
	Progress     int              `json:"progress,omitempty"`
	Error        string           `json:"error,omitempty"`
	Metrics      map[string]any   `json:"metrics,omitempty"`
	Perf         map[string]any   `json:"perf,omitempty"`
	Events       []map[string]any `json:"events,omitempty"`
	RequestMeta  map[string]any   `json:"request_meta,omitempty"`
	AccountEmail string           `json:"account_email,omitempty"`
	AccountID    string           `json:"account_id,omitempty"`
	KeyName      string           `json:"key_name,omitempty"`
	KeyID        string           `json:"key_id,omitempty"`
	ProxySource  string           `json:"proxy_source,omitempty"`
	EgressMode   string           `json:"egress_mode,omitempty"`
	EgressLabel  string           `json:"egress_label,omitempty"`
	HasProxy     bool             `json:"has_proxy"`
}

func newRuntimeMonitor() *runtimeMonitor {
	return &runtimeMonitor{active: map[string]*monitorRecord{}, limit: 200}
}

func (m *runtimeMonitor) activeCount() int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.active)
}

func (m *runtimeMonitor) start(id, endpoint, model, summary string) {
	if m == nil || strings.TrimSpace(id) == "" {
		return
	}
	now := time.Now().UnixMilli()
	m.mu.Lock()
	m.active[id] = &monitorRecord{CallID: id, Endpoint: endpoint, Model: model, Summary: summary, Status: "running", Stage: "handler_submitted", StartedAt: now, UpdatedAt: now, Events: []map[string]any{{"event": "handler_submitted", "label": monitorStageLabel("handler_submitted"), "stage": "handler_submitted", "status": "running", "time": time.Now().UTC()}}}
	m.mu.Unlock()
}

func (m *runtimeMonitor) update(id, stage string, progress int, errText string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	item := m.active[id]
	if item == nil {
		return
	}
	item.Stage = stage
	item.UpdatedAt = time.Now().UnixMilli()
	if progress >= 0 {
		item.Progress = progress
	}
	if errText != "" {
		item.Error = errText
	}
	item.Events = append(item.Events, map[string]any{"event": stage, "label": monitorStageLabel(stage), "stage": stage, "progress": item.Progress, "status": item.Status, "time": time.Now().UTC()})
}

func (m *runtimeMonitor) enrich(id string, body map[string]any) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	item := m.active[id]
	if item == nil {
		return
	}
	if v, ok := body["metrics"].(map[string]any); ok {
		if item.Metrics == nil {
			item.Metrics = map[string]any{}
		}
		for k, x := range v {
			item.Metrics[k] = x
		}
	}
	if v, ok := body["perf"].(map[string]any); ok {
		if item.Perf == nil {
			item.Perf = map[string]any{}
		}
		for k, x := range v {
			item.Perf[k] = x
		}
	}
	for k, v := range body {
		if strings.HasSuffix(k, "_ms") {
			if item.Metrics == nil {
				item.Metrics = map[string]any{}
			}
			item.Metrics[k] = v
		}
	}
	if value := stringValue(body["proxy_source"]); value != "" {
		item.ProxySource = value
	}
	if value := stringValue(body["egress_mode"]); value != "" {
		item.EgressMode = value
	}
	if value := stringValue(body["egress_label"]); value != "" {
		item.EgressLabel = value
	}
	if value, ok := body["has_proxy"].(bool); ok {
		item.HasProxy = value
	}
	if value := stringValue(body["account_email"]); value != "" {
		item.AccountEmail = value
	}
	if value := stringValue(body["account_id"]); value != "" {
		item.AccountID = value
	}
	if value := stringValue(body["key_name"]); value != "" {
		item.KeyName = value
	}
	if value := stringValue(body["key_id"]); value != "" {
		item.KeyID = value
	}
	if len(item.Events) > 0 {
		for k, v := range body {
			if k != "call_id" {
				item.Events[len(item.Events)-1][k] = v
			}
		}
	}
}

func (m *runtimeMonitor) finish(id, status, model, summary, errText string) {
	if m == nil {
		return
	}
	now := time.Now().UnixMilli()
	m.mu.Lock()
	item := m.active[id]
	if item == nil {
		item = &monitorRecord{CallID: id, StartedAt: now}
	}
	item.Status = status
	item.Stage = map[string]string{"success": "completed", "failed": "failed", "cancelled": "cancelled"}[status]
	if item.Stage == "" {
		item.Stage = status
	}
	item.Model = firstNonEmpty(model, item.Model)
	item.Summary = firstNonEmpty(summary, item.Summary)
	item.Error = firstNonEmpty(errText, item.Error)
	item.EndedAt = now
	item.UpdatedAt = now
	item.Duration = now - item.StartedAt
	if status == "success" {
		item.Progress = 100
	}
	item.Events = append(item.Events, map[string]any{"event": "completed", "label": monitorStageLabel(item.Stage), "stage": item.Stage, "status": status, "duration_ms": item.Duration, "time": time.Now().UTC()})
	copy := *item
	delete(m.active, id)
	m.completed = append(m.completed, copy)
	if len(m.completed) > m.limit {
		m.completed = m.completed[len(m.completed)-m.limit:]
	}
	m.mu.Unlock()
}

// 快照必须深拷贝：Metrics/Perf/RequestMeta 是 map，Events 是 map 切片，
// 只复制结构体的话出锁后序列化仍会与 enrich 的写入并发。
func cloneMonitorRecord(item *monitorRecord) monitorRecord {
	snapshot := *item
	snapshot.Metrics = cloneMap(item.Metrics)
	snapshot.Perf = cloneMap(item.Perf)
	snapshot.RequestMeta = cloneMap(item.RequestMeta)
	snapshot.Events = make([]map[string]any, 0, len(item.Events))
	for _, event := range item.Events {
		snapshot.Events = append(snapshot.Events, cloneMap(event))
	}
	return snapshot
}

func (m *runtimeMonitor) snapshot() map[string]any {
	if m == nil {
		return map[string]any{"active": []monitorRecord{}, "recent": []monitorRecord{}}
	}
	m.mu.Lock()
	active := make([]monitorRecord, 0, len(m.active))
	for _, item := range m.active {
		snapshot := cloneMonitorRecord(item)
		snapshot.Duration = time.Now().UnixMilli() - snapshot.StartedAt
		active = append(active, snapshot)
	}
	recent := make([]monitorRecord, 0, len(m.completed))
	for index := range m.completed {
		recent = append(recent, cloneMonitorRecord(&m.completed[index]))
	}
	m.mu.Unlock()
	sort.Slice(active, func(i, j int) bool { return active[i].StartedAt < active[j].StartedAt })
	sort.Slice(recent, func(i, j int) bool { return recent[i].EndedAt > recent[j].EndedAt })
	return map[string]any{
		"updated_at": time.Now().UTC(),
		"summary":    map[string]any{"active": len(active), "completed": len(recent), "failed": countMonitorStatus(recent, "failed")},
		"active":     active,
		"recent":     recent,
		"slow":       recent,
	}
}

func (m *runtimeMonitor) detail(id string) (monitorRecord, bool) {
	if m == nil {
		return monitorRecord{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if item := m.active[id]; item != nil {
		snapshot := cloneMonitorRecord(item)
		snapshot.Duration = time.Now().UnixMilli() - snapshot.StartedAt
		return snapshot, true
	}
	for index := len(m.completed) - 1; index >= 0; index-- {
		if m.completed[index].CallID == id {
			return cloneMonitorRecord(&m.completed[index]), true
		}
	}
	return monitorRecord{}, false
}

func (m *runtimeMonitor) cancel(id string) (monitorRecord, bool) {
	item, ok := m.detail(id)
	if !ok || item.Status != "running" {
		return monitorRecord{}, false
	}
	m.finish(id, "cancelled", item.Model, item.Summary, "cancelled by administrator")
	item, _ = m.detail(id)
	return item, true
}

func countMonitorStatus(items []monitorRecord, status string) int {
	count := 0
	for _, item := range items {
		if item.Status == status {
			count++
		}
	}
	return count
}

func (s *Server) proxyTest(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	var request struct {
		URL string `json:"url"`
	}
	if r.Body != nil && r.ContentLength != 0 && !decodeJSON(w, r, &request) {
		return
	}
	candidate := strings.TrimSpace(request.URL)
	if candidate == "" {
		candidate = s.proxyManager.Resolve(nil, false)
	}
	if candidate == "" {
		writeError(w, http.StatusBadRequest, "proxy url is required", "invalid_request_error")
		return
	}
	parsed, err := url.Parse(candidate)
	if err != nil || parsed.Host == "" {
		writeJSON(w, http.StatusOK, map[string]any{"result": map[string]any{"ok": false, "status": 0, "latency_ms": 0, "error": "invalid proxy url", "has_proxy": true}})
		return
	}
	client := &http.Client{Transport: proxyruntime.NewTransport(http.DefaultTransport), Timeout: 15 * time.Second}
	started := time.Now()
	req, err := http.NewRequestWithContext(proxyruntime.WithURL(r.Context(), candidate), http.MethodGet, "https://chatgpt.com/api/auth/csrf", nil)
	result := map[string]any{"proxy_source": "input", "has_proxy": true}
	if err == nil {
		var response *http.Response
		response, err = client.Do(req)
		if response != nil {
			defer response.Body.Close()
			result["status"] = response.StatusCode
			result["ok"] = proxyTestStatusOK(response.StatusCode)
			if !proxyTestStatusOK(response.StatusCode) {
				result["error"] = fmt.Sprintf("HTTP %d", response.StatusCode)
			}
		}
	}
	result["latency_ms"] = time.Since(started).Milliseconds()
	if err != nil {
		result["ok"] = false
		result["status"] = 0
		result["error"] = redactProxyError(err.Error())
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": result})
}

// 代理 URL 里的凭据必须抹掉，且必须一次替换到位。
// 旧实现用 for 循环反复从字符串开头查找 scheme，替换文本里又塞回了 "@"，
// 第二轮命中的正是刚插入的那个 "@"，替换结果与上一轮逐字节相同 → 永不退出。
var proxyCredentialPattern = regexp.MustCompile(`(?i)\b(https?|socks4a?|socks5h?)://[^/\s@]*@`)

func redactProxyError(value string) string {
	return proxyCredentialPattern.ReplaceAllString(value, "$1://[REDACTED]@")
}

func (s *Server) monitorAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/monitor/realtime")
	if path == "" || path == "/" {
		writeJSON(w, http.StatusOK, s.monitorSnapshotWithHistory())
		return
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	itemID := parts[0]
	if len(parts) == 2 && parts[1] == "cancel" && r.Method == http.MethodPost {
		item, ok := s.monitor.cancel(itemID)
		if !ok {
			writeError(w, http.StatusNotFound, "request not found", "not_found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "record": item})
		return
	}
	item, ok := s.monitor.detail(itemID)
	if !ok {
		writeError(w, http.StatusNotFound, "request not found", "not_found")
		return
	}
	// 直接返回记录本身。此前包了一层 {"detail": ...}，而前端把这个响应的类型
	// 声明为 RealtimeMonitorRecordDetail —— 于是 record.presentation 取到的是
	// undefined，详情抽屉一点开就抛。
	writeJSON(w, http.StatusOK, monitorRecordMap(item))
}

func (s *Server) monitorSnapshotWithHistory() map[string]any {
	snapshot := s.monitor.snapshot()
	active := monitorRecordMaps(snapshot["active"])
	recent := monitorRecordMaps(snapshot["recent"])
	history, events := s.historicalMonitorRecords(200)

	seen := map[string]bool{}
	for _, item := range recent {
		seen[stringValue(item["call_id"])] = true
	}
	for _, item := range history {
		id := stringValue(item["call_id"])
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		recent = append(recent, item)
	}
	sort.SliceStable(recent, func(i, j int) bool {
		return stringValue(recent[i]["ended_at"]) > stringValue(recent[j]["ended_at"])
	})
	if len(recent) > 200 {
		recent = recent[:200]
	}

	// 必须 make 出非 nil 切片：append([]T(nil), 空...) 返回 nil，会序列化成
	// JSON null。而前端写的是 monitorData.value?.slow.slice(0, 8) —— `?.`
	// 只护住了 monitorData.value，没护住 .slow，null.slice 直接抛 TypeError，
	// 整个监控页的 computed 失败、页面空白。
	slow := make([]map[string]any, 0, len(recent))
	slow = append(slow, recent...)
	sort.SliceStable(slow, func(i, j int) bool {
		return monitorNumber(slow[i]["duration_ms"]) > monitorNumber(slow[j]["duration_ms"])
	})
	if len(slow) > 50 {
		slow = slow[:50]
	}

	recentCounts := monitorCountsByField(recent, func(item map[string]any) string {
		return stringValue(item["model"])
	})
	activeCounts := monitorCountsByField(active, func(item map[string]any) string {
		return stringValue(item["model"])
	})
	if len(activeCounts) == 0 {
		activeCounts = recentCounts
	}

	activeStageCounts := monitorCountsByField(active, func(item map[string]any) string {
		stage := strings.TrimSpace(stringValue(item["stage"]))
		if stage == "" {
			stage = strings.TrimSpace(stringValue(item["stage_label"]))
		}
		return stage
	})
	if len(activeStageCounts) == 0 {
		activeStageCounts = monitorCountsByField(recent, func(item map[string]any) string {
			stage := strings.TrimSpace(stringValue(item["stage"]))
			if stage == "" {
				stage = strings.TrimSpace(stringValue(item["stage_label"]))
			}
			return stage
		})
	}

	activeEgressCounts := monitorCountsByField(active, monitorEgressKey)
	if len(activeEgressCounts) == 0 {
		activeEgressCounts = monitorCountsByField(recent, monitorEgressKey)
	}

	durationValues := monitorMetricValues(recent, "duration_ms")
	metricValues := monitorMetricValuesMap(recent, monitorMonitorMetricKeys)
	metricP95 := map[string]any{}
	metricLabels := map[string]string{}
	bottleneckKey := ""
	bottleneckLabel := ""
	var bottleneckValue int64
	for _, key := range monitorMonitorMetricKeys {
		values := metricValues[key]
		if len(values) == 0 {
			continue
		}
		p95 := monitorPercentile(values, 0.95)
		if p95 <= 0 {
			continue
		}
		metricP95[key] = p95
		metricLabels[key] = monitorMetricLabel(key)
		if p95 > bottleneckValue {
			bottleneckValue = p95
			bottleneckKey = key
			bottleneckLabel = monitorMetricLabel(key)
		}
	}

	summary := map[string]any{
		"active":          len(active),
		"completed":       len(recent),
		"success":         0,
		"failed":          0,
		"success_rate":    0,
		"avg_duration_ms": 0,
		"p95_duration_ms": monitorPercentile(durationValues, 0.95),
		"metric_p95":      metricP95,
		"slow_counts": map[string]any{
			"handler_queue": 0, "stream_first_queue": 0, "account_wait": 0,
			"egress_wait": 0, "total_over_120s": 0, "local_reject_or_busy": 0,
		},
		"bottleneck":       map[string]any{"key": bottleneckKey, "label": bottleneckLabel, "value_ms": bottleneckValue},
		"by_model":         recentCounts,
		"active_by_model":  activeCounts,
		"active_by_stage":  activeStageCounts,
		"active_by_egress": activeEgressCounts,
	}
	var totalDuration int64
	for _, item := range recent {
		status := strings.ToLower(stringValue(item["status"]))
		switch status {
		case "success", "completed":
			summary["success"] = monitorNumber(summary["success"]) + 1
		case "failed", "error":
			summary["failed"] = monitorNumber(summary["failed"]) + 1
		}
		duration := monitorNumber(item["duration_ms"])
		totalDuration += duration
		if duration >= 120000 {
			slowCounts := summary["slow_counts"].(map[string]any)
			slowCounts["total_over_120s"] = monitorNumber(slowCounts["total_over_120s"]) + 1
		}
		if monitorNumber(item["handler_queue_ms"]) >= 1000 {
			slowCounts := summary["slow_counts"].(map[string]any)
			slowCounts["handler_queue"] = monitorNumber(slowCounts["handler_queue"]) + 1
		}
		if monitorNumber(item["stream_first_queue_ms"]) >= 1000 {
			slowCounts := summary["slow_counts"].(map[string]any)
			slowCounts["stream_first_queue"] = monitorNumber(slowCounts["stream_first_queue"]) + 1
		}
		if monitorNumber(item["account_wait_ms"]) >= 1000 {
			slowCounts := summary["slow_counts"].(map[string]any)
			slowCounts["account_wait"] = monitorNumber(slowCounts["account_wait"]) + 1
		}
		if monitorNumber(item["egress_wait_ms"]) >= 1000 {
			slowCounts := summary["slow_counts"].(map[string]any)
			slowCounts["egress_wait"] = monitorNumber(slowCounts["egress_wait"]) + 1
		}
		if monitorLooksLocallyBusy(item) {
			slowCounts := summary["slow_counts"].(map[string]any)
			slowCounts["local_reject_or_busy"] = monitorNumber(slowCounts["local_reject_or_busy"]) + 1
		}
	}
	success := monitorNumber(summary["success"])
	if len(recent) > 0 {
		summary["avg_duration_ms"] = totalDuration / int64(len(recent))
		summary["success_rate"] = float64(success) * 100 / float64(len(recent))
	}
	snapshot["active"] = active
	snapshot["recent"] = recent
	snapshot["slow"] = slow
	snapshot["events"] = events
	snapshot["summary"] = summary
	snapshot["threadpool"] = map[string]any{"tokens": 0, "previous_tokens": 0}
	snapshot["window"] = map[string]any{"completed": len(recent), "completed_capacity": 200, "events": len(events), "event_capacity": 1000}
	snapshot["metric_labels"] = metricLabels
	return snapshot
}

// monitorEventMaps 补齐前端 RealtimeMonitorEvent 在渲染中用到的可选字段。
//
// timing_text / detail_text 都被模板里的 v-if 守着，缺失只会少显示一行而不会抛，
// 所以这里是锦上添花；call_id 与 model 同理。补上后详情抽屉的时间线才有内容。
func monitorEventMaps(events []map[string]any, callID, modelName string) []any {
	out := make([]any, 0, len(events))
	for _, event := range events {
		item := cloneMap(event)
		if _, ok := item["call_id"]; !ok && callID != "" {
			item["call_id"] = callID
		}
		if _, ok := item["model"]; !ok && modelName != "" {
			item["model"] = modelName
		}
		if _, ok := item["timing_text"]; !ok {
			if ms := intValue(event["duration_ms"]); ms > 0 {
				item["timing_text"] = formatDurationText(ms)
			}
		}
		out = append(out, item)
	}
	return out
}

func monitorRecordMaps(value any) []map[string]any {
	result := []map[string]any{}
	switch typed := value.(type) {
	case []monitorRecord:
		for _, item := range typed {
			result = append(result, monitorRecordMap(item))
		}
	case []map[string]any:
		result = append(result, typed...)
	case []any:
		for _, item := range typed {
			if record, ok := item.(map[string]any); ok {
				result = append(result, record)
			}
		}
	}
	return result
}

// monitorMetricLabels 给监控指标一个中文名；未收录的键回退为键名本身。
var monitorMetricLabels = map[string]string{
	"duration_ms":           "总时长",
	"total_ms":              "总时长",
	"http_ttfb_ms":          "上游首字节",
	"sse_first_event_ms":    "SSE 首事件",
	"stream_first_queue_ms": "入口排队",
	"account_wait_ms":       "等待账号",
	"egress_wait_ms":        "等待出口",
	"handler_queue_ms":      "入口队列",
	"upstream_ms":           "上游耗时",
	"download_ms":           "下载耗时",
}

// monitorSlowThresholdMS 超过这个耗时的指标会被列进 slow_metrics。
const monitorSlowThresholdMS = 5000

// monitorRecordPresentation 生成前端 MonitorRecordPresentation 契约。
//
// 前端 MonitorActiveRow / MonitorRecentRow / MonitorSlowCard 与 monitorView 的
// 签名函数都直接读 row.presentation.*。整个对象缺失时，Vue 的渲染错误处理会把
// 整棵子树渲染成空——监控页白屏，而控制台只留一句报错。
//
// 字段清单以 web-vue/src/api/monitor.ts 的 MonitorRecordPresentation 为准。
func monitorRecordPresentation(item monitorRecord) map[string]any {
	durationMS := int(item.Duration)
	if durationMS <= 0 && item.StartedAt > 0 {
		end := item.EndedAt
		if end == 0 {
			end = time.Now().UnixMilli()
		}
		durationMS = int(end - item.StartedAt)
	}

	// 指标按耗时降序，便于挑出最慢的一项作为慢因。
	type metricEntry struct {
		key   string
		label string
		value int
	}
	entries := make([]metricEntry, 0, len(item.Metrics))
	tracked := 0
	for key, raw := range item.Metrics {
		value := intValue(raw)
		if value <= 0 || key == "progress" {
			continue
		}
		label, ok := monitorMetricLabels[key]
		if !ok {
			label = key
		}
		entries = append(entries, metricEntry{key: key, label: label, value: value})
		if key != "duration_ms" && key != "total_ms" {
			tracked += value
		}
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].value > entries[j].value })

	digest := make([]string, 0, 3)
	slowMetrics := make([]any, 0)
	slowReasonCode, slowReason := "", ""
	for _, entry := range entries {
		if len(digest) < 3 {
			digest = append(digest, fmt.Sprintf("%s %s", entry.label, formatDurationText(entry.value)))
		}
		if entry.value < monitorSlowThresholdMS {
			continue
		}
		slowMetrics = append(slowMetrics, map[string]any{
			"key":        entry.key,
			"label":      entry.label,
			"value_ms":   entry.value,
			"value_text": formatDurationText(entry.value),
			"important":  entry.value >= monitorSlowThresholdMS*3,
		})
		if slowReason == "" {
			slowReasonCode = entry.key
			slowReason = fmt.Sprintf("%s耗时 %s，超过 %s 的阈值", entry.label, formatDurationText(entry.value), formatDurationText(monitorSlowThresholdMS))
		}
	}

	egressText := firstNonEmpty(item.EgressLabel, item.EgressMode, item.ProxySource)
	if egressText == "" {
		if item.HasProxy {
			egressText = "已配置代理"
		} else {
			egressText = "直连"
		}
	}
	accountAttempt := firstNonEmpty(stringValue(item.Metrics["account_attempts"]), "")
	if accountAttempt != "" {
		accountAttempt = "尝试 " + accountAttempt + " 次"
	}
	accountEgress := strings.TrimSpace(strings.Join(nonEmptyStrings(item.AccountEmail, egressText), " · "))

	untracked := durationMS - tracked
	if untracked < 0 {
		untracked = 0
	}
	return map[string]any{
		"status_label":          monitorStatusLabel(item),
		"status_tone":           monitorStatusTone(item),
		"stage_text":            monitorStageLabel(item.Stage),
		"error_text":            item.Error,
		"duration_text":         formatDurationText(durationMS),
		"metric_digest":         strings.Join(digest, " · "),
		"egress_text":           egressText,
		"account_attempt_text":  accountAttempt,
		"account_egress_text":   accountEgress,
		"tracked_duration_ms":   tracked,
		"untracked_duration_ms": untracked,
		"slow_metrics":          slowMetrics,
		"slow_reason_code":      slowReasonCode,
		"slow_reason":           slowReason,
	}
}

func monitorStatusLabel(item monitorRecord) string {
	switch strings.ToLower(strings.TrimSpace(item.Status)) {
	case "success", "completed":
		return "成功"
	case "failed", "error":
		return "失败"
	case "cancelled", "canceled":
		return "已取消"
	case "running":
		return "进行中"
	case "queued":
		return "排队中"
	default:
		return firstNonEmpty(item.Status, "进行中")
	}
}

// monitorStatusTone 只返回前端 MonitorTone 允许的取值。
func monitorStatusTone(item monitorRecord) string {
	if item.Error != "" {
		return "danger"
	}
	switch strings.ToLower(strings.TrimSpace(item.Status)) {
	case "success", "completed":
		return "success"
	case "failed", "error":
		return "danger"
	case "cancelled", "canceled":
		return "muted"
	case "running":
		return "info"
	case "queued":
		return "muted"
	default:
		return "info"
	}
}

func nonEmptyStrings(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, strings.TrimSpace(value))
		}
	}
	return out
}

func monitorRecordMap(item monitorRecord) map[string]any {
	modelName := item.Model
	if modelName == "" && item.RequestMeta != nil {
		modelName = stringValue(item.RequestMeta["model"])
	}
	result := map[string]any{
		"call_id": item.CallID, "endpoint": item.Endpoint, "model": modelName,
		"summary": item.Summary, "status": item.Status, "stage": item.Stage,
		"duration_ms": item.Duration, "elapsed_ms": item.Duration, "progress": item.Progress,
	}
	result["stage_label"] = monitorStageLabel(item.Stage)
	if item.StartedAt > 0 {
		result["started_at"] = time.UnixMilli(item.StartedAt).UTC().Format(time.RFC3339)
	}
	if item.UpdatedAt > 0 {
		result["updated_at"] = time.UnixMilli(item.UpdatedAt).UTC().Format(time.RFC3339)
	}
	if item.EndedAt > 0 {
		result["ended_at"] = time.UnixMilli(item.EndedAt).UTC().Format(time.RFC3339)
	}
	if item.Error != "" {
		result["error"] = item.Error
	}
	result["metrics"] = item.Metrics
	result["perf"] = item.Perf
	result["events"] = monitorEventMaps(item.Events, item.CallID, modelName)
	result["request_meta"] = item.RequestMeta
	result["account_email"] = item.AccountEmail
	result["account_id"] = item.AccountID
	result["key_name"] = item.KeyName
	result["key_id"] = item.KeyID
	result["proxy_source"] = item.ProxySource
	result["egress_mode"] = item.EgressMode
	result["egress_label"] = item.EgressLabel
	result["has_proxy"] = item.HasProxy
	// 前端每一个监控组件都直接读 row.presentation.*，必须随每条记录一起给出。
	result["presentation"] = monitorRecordPresentation(item)
	return result
}

func (s *Server) historicalMonitorRecords(limit int) ([]map[string]any, []map[string]any) {
	items := s.loadCallLogs()
	records := []map[string]any{}
	events := []map[string]any{}
	for index := len(items) - 1; index >= 0 && len(records) < limit; index-- {
		item := items[index]
		if !strings.EqualFold(stringValue(item["type"]), "call") {
			continue
		}
		detail := mapValue(item["detail"])
		callID := firstNonEmpty(stringValue(detail["call_id"]), stringValue(item["id"]))
		if callID == "" {
			continue
		}
		monitor := mapValue(detail["monitor"])
		// Legacy Go records only contained start/completed and no timings. They
		// cannot drive the server-compatible slow-request visualization, so keep
		// them in log management but exclude them from realtime history.
		if len(mapValue(monitor["metrics"])) == 0 && len(mapValue(monitor["perf"])) == 0 && len(anyList(monitor["events"])) <= 2 {
			continue
		}
		record := map[string]any{
			"call_id":       callID,
			"endpoint":      stringValue(detail["endpoint"]),
			"model":         stringValue(detail["model"]),
			"summary":       stringValue(item["summary"]),
			"status":        firstNonEmpty(stringValue(detail["status"]), "success"),
			"stage":         firstNonEmpty(stringValue(monitor["stage"]), "completed"),
			"started_at":    stringValue(detail["started_at"]),
			"ended_at":      stringValue(detail["ended_at"]),
			"duration_ms":   monitorNumber(detail["duration_ms"]),
			"account_email": stringValue(detail["account_email"]),
			"account_id":    firstNonEmpty(stringValue(detail["provider_account_id"]), stringValue(monitor["account_id"])),
			"key_name":      firstNonEmpty(stringValue(detail["key_name"]), stringValue(monitor["key_name"])),
			"key_id":        firstNonEmpty(stringValue(detail["key_id"]), stringValue(monitor["key_id"])),
			"metrics":       mapValue(monitor["metrics"]),
			"perf":          mapValue(monitor["perf"]),
		}
		for _, key := range []string{"proxy_source", "proxy_hash", "egress_key", "egress_label", "egress_mode", "has_proxy", "error", "raw_error", "upstream_error", "upstream_message"} {
			if value, ok := monitor[key]; ok {
				record[key] = value
			} else if value, ok := detail[key]; ok {
				record[key] = value
			}
		}
		records = append(records, record)
		for _, rawEvent := range anyList(monitor["events"]) {
			event := mapValue(rawEvent)
			if len(event) == 0 {
				continue
			}
			event["call_id"] = callID
			if stringValue(event["model"]) == "" {
				event["model"] = record["model"]
			}
			events = append(events, event)
		}
	}
	return records, events
}

func anyList(value any) []any {
	if result, ok := value.([]any); ok {
		return result
	}
	return []any{}
}

func monitorNumber(value any) int64 {
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int32:
		return int64(typed)
	case int64:
		return typed
	case float64:
		return int64(typed)
	case float32:
		return int64(typed)
	default:
		return 0
	}
}

func monitorPercentile(values []int64, percentile float64) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	index := int(math.Ceil(percentile*float64(len(sorted)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

func monitorMetricValues(items []map[string]any, key string) []int64 {
	values := make([]int64, 0, len(items))
	for _, item := range items {
		value := monitorMetricValue(item, key)
		if value > 0 {
			values = append(values, value)
		}
	}
	return values
}

func monitorMetricValuesMap(items []map[string]any, keys []string) map[string][]int64 {
	result := make(map[string][]int64, len(keys))
	for _, key := range keys {
		result[key] = monitorMetricValues(items, key)
	}
	return result
}

func monitorMetricValue(item map[string]any, key string) int64 {
	if item == nil {
		return 0
	}
	if value := monitorNumber(item[key]); value > 0 {
		return value
	}
	if metrics := mapValue(item["metrics"]); len(metrics) > 0 {
		if value := monitorNumber(metrics[key]); value > 0 {
			return value
		}
	}
	if perf := mapValue(item["perf"]); len(perf) > 0 {
		if value := monitorNumber(perf[key]); value > 0 {
			return value
		}
	}
	return 0
}

func monitorCountsByField(items []map[string]any, valueFn func(map[string]any) string) map[string]any {
	result := map[string]any{}
	for _, item := range items {
		key := strings.TrimSpace(valueFn(item))
		if key == "" {
			continue
		}
		result[key] = monitorNumber(result[key]) + 1
	}
	return result
}

func monitorEgressKey(item map[string]any) string {
	if item == nil {
		return ""
	}
	if key := strings.TrimSpace(stringValue(item["active_egress_key"])); key != "" {
		return key
	}
	source := strings.TrimSpace(stringValue(item["proxy_source"]))
	label := strings.TrimSpace(firstNonEmpty(
		stringValue(item["egress_label"]),
		stringValue(item["proxy_group_id"]),
		stringValue(item["proxy_node_name"]),
		stringValue(item["proxy_node_id"]),
		stringValue(item["proxy_hash"]),
		stringValue(item["egress_key"]),
	))
	if source == "" && label == "" {
		return ""
	}
	if source == "" {
		source = "direct"
	}
	if label == "" {
		return source
	}
	return source + ":" + label
}

func monitorMetricLabel(key string) string {
	if label, ok := monitorMetricLabelMap[key]; ok {
		return label
	}
	return key
}

func monitorLooksLocallyBusy(item map[string]any) bool {
	if item == nil {
		return false
	}
	text := strings.ToLower(strings.Join([]string{
		stringValue(item["status"]),
		stringValue(item["error"]),
		stringValue(item["raw_error"]),
		stringValue(item["upstream_error"]),
		stringValue(item["upstream_message"]),
		stringValue(item["local_reason"]),
	}, " "))
	return strings.Contains(text, "busy") || strings.Contains(text, "reject") || strings.Contains(text, "too many open connections")
}

func (s *Server) appendCallLog(record monitorRecord, statusCode int, requestShape any, responseBody []byte, errorText string) {
	if s == nil || strings.TrimSpace(record.CallID) == "" {
		return
	}
	if errText := strings.TrimSpace(errorText); errText != "" {
		errorText = SanitizeDiagnosticText(errText)
	}
	startedAt := time.UnixMilli(record.StartedAt).UTC()
	endedAt := time.UnixMilli(record.EndedAt).UTC()
	if endedAt.IsZero() {
		endedAt = time.Now().UTC()
	}
	detail := map[string]any{
		"call_id":       record.CallID,
		"endpoint":      record.Endpoint,
		"model":         record.Model,
		"status":        record.Status,
		"stage":         record.Stage,
		"duration_ms":   record.Duration,
		"status_code":   statusCode,
		"started_at":    startedAt.Format(time.RFC3339),
		"ended_at":      endedAt.Format(time.RFC3339),
		"progress":      record.Progress,
		"request_shape": requestShape,
		"monitor":       map[string]any{"stage": record.Stage, "metrics": record.Metrics, "perf": record.Perf, "events": record.Events, "proxy_source": record.ProxySource, "egress_mode": record.EgressMode, "egress_label": record.EgressLabel, "has_proxy": record.HasProxy, "account_email": record.AccountEmail, "account_id": record.AccountID, "key_name": record.KeyName, "key_id": record.KeyID},
	}
	if record.AccountEmail != "" {
		detail["account_email"] = record.AccountEmail
	}
	if record.AccountID != "" {
		detail["provider_account_id"] = record.AccountID
	}
	if record.KeyName != "" {
		detail["key_name"] = record.KeyName
	}
	if record.KeyID != "" {
		detail["key_id"] = record.KeyID
	}
	if shape, ok := requestShape.(map[string]any); ok {
		detail["request_meta"] = map[string]any{
			"size":            shape["size"],
			"quality":         shape["quality"],
			"response_format": shape["response_format"],
			"n":               shape["n"],
			"image_url_parts": shape["image_url_parts"],
			"data_url_images": shape["data_url_images"],
		}
	}
	if record.RequestMeta != nil {
		detail["request_meta"] = record.RequestMeta
	}
	if summary := strings.TrimSpace(record.Summary); summary != "" {
		sanitizedSummary := SanitizeDiagnosticText(summary)
		detail["request_text"] = sanitizedSummary
		detail["request_text_full"] = sanitizedSummary
		if len(sanitizedSummary) > 180 {
			detail["request_text_truncated"] = true
		}
	}
	if errorText != "" {
		detail["error"] = errorText
		detail["raw_error"] = errorText
		detail["upstream_error"] = errorText
	}
	if outputs := responseImageOutputs(responseBody); len(outputs) > 0 {
		detail["output_images"] = outputs
		detail["image_urls"] = outputs
		var resultImages []map[string]any
		var resolutions []string
		seen := make(map[string]bool)
		for _, out := range outputs {
			fn := out["filename"]
			w, h := s.lookupImageDimensions(fn)
			imgMeta := map[string]any{}
			resText := "未知"
			if w > 0 && h > 0 {
				imgMeta["width"] = w
				imgMeta["height"] = h
				resText = fmt.Sprintf("%d×%d", w, h)
			}
			resultImages = append(resultImages, imgMeta)
			if !seen[resText] {
				seen[resText] = true
				resolutions = append(resolutions, resText)
			}
		}
		detail["result_images"] = resultImages
		detail["result_data_count"] = len(resultImages)
		detail["actual_resolution"] = strings.Join(resolutions, " / ")
	}
	entry := map[string]any{
		"id":      record.CallID,
		"time":    endedAt.Format(time.RFC3339),
		"type":    "call",
		"summary": firstNonEmpty(SanitizeDiagnosticText(strings.TrimSpace(record.Summary)), record.Endpoint, record.Model, record.CallID),
		"detail":  detail,
	}
	if s.hourlyMetrics != nil {
		s.hourlyMetrics.RecordCall(entry)
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return
	}
	if err := os.MkdirAll(s.cfg.DataDir, 0o755); err != nil {
		return
	}
	path := filepath.Join(s.cfg.DataDir, "logs.jsonl")
	s.logMu.Lock()
	defer s.logMu.Unlock()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.Write(append(raw, '\n'))
}

func (s *Server) lookupImageDimensions(idOrFilename string) (int, int) {
	if s == nil || s.cfg.ImageDataDir == "" || idOrFilename == "" {
		return 0, 0
	}
	id := strings.TrimSuffix(idOrFilename, filepath.Ext(idOrFilename))
	var targetFile string

	// 优先以 O(1) 路径直查，避免高并发和海量图片下的全目录 ReadDir 扫盘
	candidates := []string{idOrFilename}
	for _, ext := range []string{".png", ".webp", ".jpg", ".jpeg", ".gif"} {
		candidates = append(candidates, id+ext)
	}
	for _, cand := range candidates {
		fullPath := filepath.Join(s.cfg.ImageDataDir, cand)
		if info, err := os.Stat(fullPath); err == nil && !info.IsDir() {
			targetFile = fullPath
			break
		}
	}

	if targetFile != "" {
		if meta := mediaMetadata(targetFile); meta != nil {
			w := intValue(meta["width"])
			h := intValue(meta["height"])
			if w > 0 && h > 0 {
				return w, h
			}
		}
	} else {
		// 备选降级：仅在非标扩展名时兜底遍历
		entries, err := os.ReadDir(s.cfg.ImageDataDir)
		if err != nil {
			return 0, 0
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			if strings.HasSuffix(name, ".meta.json") {
				continue
			}
			if name == idOrFilename || strings.HasPrefix(name, id+".") {
				targetFile = filepath.Join(s.cfg.ImageDataDir, name)
				if meta := mediaMetadata(targetFile); meta != nil {
					w := intValue(meta["width"])
					h := intValue(meta["height"])
					if w > 0 && h > 0 {
						return w, h
					}
				}
				break
			}
		}
	}
	if targetFile == "" {
		return 0, 0
	}
	f, err := os.Open(targetFile)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

func responseImageOutputs(raw []byte) []map[string]string {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	out := []map[string]string{}
	appendURL := func(value string) {
		u := strings.TrimSpace(value)
		if u == "" {
			return
		}
		parsed, err := url.Parse(u)
		if err != nil || parsed == nil {
			return
		}
		name := filepath.Base(parsed.Path)
		if id := parsed.Query().Get("id"); id != "" {
			name = id
		}
		out = append(out, map[string]string{"url": u, "filename": name})
	}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, item := range x {
				if k == "url" {
					appendURL(stringValue(item))
				} else {
					walk(item)
				}
			}
		case []any:
			for _, item := range x {
				walk(item)
			}
		case string:
			for _, part := range strings.Fields(x) {
				if strings.Contains(part, "/v1/files/image?id=") || strings.Contains(part, "/images/") {
					u := strings.Trim(part, "\"")
					if marker := strings.Index(u, "]("); marker >= 0 {
						u = u[marker+2:]
						u = strings.TrimSuffix(u, ")")
					} else {
						u = strings.Trim(u, "![]()")
					}
					appendURL(u)
				}
			}
		}
	}
	walk(value)
	return out
}

func monitorStageLabel(stage string) string {
	switch strings.ToLower(strings.TrimSpace(stage)) {
	case "handler_submitted":
		return "等待入口"
	case "handler_started":
		return "入口执行"
	case "stream_first_item":
		return "读取首包"
	case "image_getting_account", "image_account_lookup", "image_account_wait_slow":
		return "等待账号"
	case "image_egress_waiting":
		return "等待出口"
	case "image_egress_ready":
		return "出口就绪"
	case "image_uploading":
		return "上传图片"
	case "image_bootstrapping":
		return "初始化上游"
	case "image_getting_token":
		return "获取令牌"
	case "image_preparing_conversation":
		return "准备会话"
	case "image_starting_generation":
		return "等待上游首包"
	case "image_generating":
		return "上游生成中"
	case "image_stream_resolve_start":
		return "解析上游结果"
	case "image_resolving", "image_resolve_done":
		return "解析图片"
	case "image_download_done":
		return "下载图片"
	case "image_retry_wait":
		return "重试等待"
	case "image_single_done":
		return "单图完成"
	case "queued":
		return "排队"
	case "running":
		return "运行中"
	case "completed", "success":
		return "完成"
	case "failed", "error":
		return "失败"
	case "cancelled", "canceled":
		return "取消"
	default:
		return stage
	}
}

var monitorMetricLabelMap = map[string]string{
	"handler_queue_ms":        "等待入口",
	"stream_first_queue_ms":   "首包",
	"account_wait_ms":         "等待账号",
	"egress_wait_ms":          "等待出口",
	"egress_acquire_ms":       "出口租约",
	"upload_ms":               "上传",
	"bootstrap_ms":            "初始化",
	"requirements_ms":         "令牌",
	"prepare_conversation_ms": "准备",
	"generation_start_ms":     "启动",
	"http_dns_ms":             "HTTP DNS",
	"http_tcp_ms":             "HTTP TCP",
	"http_tls_ms":             "HTTP TLS",
	"http_wait_ms":            "HTTP 等待",
	"http_ttfb_ms":            "HTTP 首包",
	"sse_first_event_ms":      "SSE 首事件",
	"sse_max_gap_ms":          "SSE 最大空窗",
	"sse_last_gap_ms":         "SSE 收尾空窗",
	"conversation_stream_ms":  "上游生成",
	"stream_error_ms":         "上游断流",
	"resolve_ms":              "解析/轮询",
	"download_ms":             "下载",
	"retry_wait_ms":           "重试等待",
	"response_ms":             "响应整理",
	"stream_ms":               "单图内部",
	"total_ms":                "单图总耗时",
}

var monitorMonitorMetricKeys = []string{
	"handler_queue_ms",
	"stream_first_queue_ms",
	"account_wait_ms",
	"egress_wait_ms",
	"egress_acquire_ms",
	"upload_ms",
	"bootstrap_ms",
	"requirements_ms",
	"prepare_conversation_ms",
	"generation_start_ms",
	"http_dns_ms",
	"http_tcp_ms",
	"http_tls_ms",
	"http_wait_ms",
	"http_ttfb_ms",
	"sse_first_event_ms",
	"sse_max_gap_ms",
	"sse_last_gap_ms",
	"conversation_stream_ms",
	"stream_error_ms",
	"resolve_ms",
	"download_ms",
	"retry_wait_ms",
	"response_ms",
	"stream_ms",
	"total_ms",
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	accounts, _ := s.store.AccountList()
	accountStats := dashboardAccountStats(accounts)
	healthy := boolValue(accountStats["healthy"], false)
	timeRange := r.URL.Query().Get("time_range")
	now := time.Now()
	var logsSummary map[string]any
	var ranges map[string]any
	if s.hourlyMetrics != nil {
		logsSummary = s.hourlyMetrics.Summary(timeRange, now)
		ranges = s.hourlyMetrics.BuildRangesSchemaV5(now)
	} else {
		logsSummary = dashboardLogSummary(s.loadCallLogs(), timeRange, now)
	}

	runtimeMode := "native"
	if _, err := os.Stat("/.dockerenv"); err == nil {
		runtimeMode = "docker"
	}
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "gptgrok2api"
	}

	startTime := s.startTime
	if startTime.IsZero() {
		startTime = now
	}
	uptimeSecs := int(time.Since(startTime).Seconds())
	if uptimeSecs < 0 {
		uptimeSecs = 0
	}

	var mStats runtime.MemStats
	runtime.ReadMemStats(&mStats)

	totalDisk, usedDisk, _ := diskUsage(s.cfg.DataDir)
	var storagePercent any = nil
	if totalDisk > 0 {
		storagePercent = float64(usedDisk) / float64(totalDisk) * 100.0
	}

	mMedia := mediaStats(s.cfg.ImageDataDir)
	imageCount := intValue(mMedia["count"])
	imageBytes := int64(intValue(mMedia["size_bytes"]))

	activeRequests := 0
	if s.monitor != nil {
		activeRequests = s.monitor.activeCount()
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"schema_version": 5,
		"status":         map[bool]string{true: "ok", false: "degraded"}[healthy],
		"healthy":        healthy,
		"version":        s.cfg.Version,
		"generated_at":   now.UTC().Format(time.RFC3339),
		"meta": map[string]any{
			"schema_version":   5,
			"generated_at":     now.UTC().Format(time.RFC3339),
			"available_ranges": []string{"24h", "7d", "30d"},
		},
		"metrics": map[string]any{
			"status":           "ready",
			"ready":            true,
			"stale":            false,
			"source":           "memory",
			"source_revision":  nil,
			"last_ingested_at": now.UTC().Format(time.RFC3339),
			"freshness_ms":     0,
			"checkpoint_at":    now.UTC().Format(time.RFC3339),
			"failure_reason":   nil,
			"retention_days":   30,
		},
		"runtime": map[string]any{
			"runtime_mode":             runtimeMode,
			"instance_name":            hostname,
			"distribution":             runtime.GOOS,
			"kernel_version":           runtime.GOOS + "/" + runtime.GOARCH,
			"architecture":             runtime.GOARCH,
			"python_version":           "go1.23",
			"cpu_capacity":             math.Max(1.0, float64(runtime.NumCPU())),
			"service_started_at":       startTime.UTC().Format(time.RFC3339),
			"service_uptime_seconds":   uptimeSecs,
			"process_cpu_percent":      nil,
			"process_memory_bytes":     int(mStats.Sys),
			"process_memory_percent":   nil,
			"memory_scope":             "system",
			"memory_percent":           nil,
			"storage_percent":          storagePercent,
			"network_rx_bytes_per_sec": nil,
			"network_tx_bytes_per_sec": nil,
		},
		"operations": map[string]any{
			"active_requests": activeRequests,
		},
		"accounts": accountStats,
		"storage": map[string]any{
			"backend":              "json",
			"health":               "ok",
			"images":               mMedia,
			"application_database": map[string]any{"engine": "json", "status": "ok"},
			"image_storage": map[string]any{
				"enabled":          true,
				"mode":             "local",
				"status":           "not_checked",
				"available":        true,
				"image_count":      imageCount,
				"image_size_bytes": imageBytes,
			},
		},
		"ranges": ranges,
		"logs":   logsSummary,
	})
}

func (s *Server) logDetailAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	logID := strings.TrimPrefix(r.URL.Path, "/api/logs/")
	if logID == "" {
		writeError(w, http.StatusBadRequest, "log id is required", "invalid_request_error")
		return
	}
	items := s.loadCallLogs()
	var matched map[string]any
	for i := len(items) - 1; i >= 0; i-- {
		if stringValue(items[i]["id"]) == logID {
			matched = items[i]
			break
		}
	}
	if matched == nil {
		writeError(w, http.StatusNotFound, "log record not found", "not_found")
		return
	}

	detail := mapValue(matched["detail"])
	requestText := stringValue(detail["request_text"])
	requestMeta := mapValue(detail["request_meta"])
	requestShape := mapValue(detail["request_shape"])
	timings := mapValue(detail["timings_ms"])
	attempts := detail["attempts"]
	if attempts == nil {
		attempts = []any{}
	}
	imageURLs := detail["image_urls"]
	if imageURLs == nil {
		imageURLs = []any{}
	}

	result := map[string]any{
		"id":                     stringValue(matched["id"]),
		"time":                   stringValue(matched["time"]),
		"type":                   firstNonEmpty(stringValue(matched["type"]), "call"),
		"summary":                stringValue(matched["summary"]),
		"business":               firstNonEmpty(stringValue(detail["business"]), "chat"),
		"outcome":                firstNonEmpty(stringValue(detail["outcome"]), "success"),
		"endpoint":               stringValue(detail["endpoint"]),
		"model":                  stringValue(detail["model"]),
		"status":                 firstNonEmpty(stringValue(detail["status"]), "success"),
		"display_status":         firstNonEmpty(stringValue(detail["display_status"]), stringValue(detail["status"]), "success"),
		"key_id":                 stringValue(detail["key_id"]),
		"key_name":               stringValue(detail["key_name"]),
		"role":                   stringValue(detail["role"]),
		"account_email":          stringValue(detail["account_email"]),
		"conversation_id":        stringValue(detail["conversation_id"]),
		"duration_ms":            fmt.Sprint(detail["duration_ms"]),
		"started_at":             stringValue(detail["started_at"]),
		"ended_at":               stringValue(detail["ended_at"]),
		"status_code":            intValue(detail["status_code"]),
		"error_code":             stringValue(detail["error_code"]),
		"public_error":           stringValue(detail["public_error"]),
		"request_text":           requestText,
		"request_text_full":      firstNonEmpty(stringValue(detail["request_text_full"]), requestText),
		"request_text_truncated": boolValue(detail["request_text_truncated"], false),
		"request_shape":          requestShape,
		"request_meta":           requestMeta,
		"upstream_error":         stringValue(detail["upstream_error"]),
		"upstream_text":          stringValue(detail["upstream_text"]),
		"image_urls":             imageURLs,
		"attempts":               attempts,
		"timings_ms":             timings,
		"perf":                   mapValue(detail["perf"]),
		"metrics":                mapValue(detail["metrics"]),
		"monitor":                mapValue(detail["monitor"]),
		"detail_presentation":    mapValue(detail["detail_presentation"]),
		"raw_detail":             detail,
	}
	summaryView := formatCallSummary(matched)
	if presentation, ok := summaryView["presentation"].(map[string]any); ok {
		result["presentation"] = presentation
	}
	if resultImages := detail["result_images"]; resultImages != nil {
		result["result_images"] = resultImages
	}
	if actualRes := detail["actual_resolution"]; actualRes != nil {
		result["actual_resolution"] = actualRes
	}
	writeJSON(w, http.StatusOK, result)
}

func newDashboardProviderStats(provider string) map[string]any {
	return map[string]any{
		"provider": provider, "total": 0, "cumulative_total": 0,
		"active": 0, "limited": 0, "abnormal": 0, "disabled": 0,
		"total_quota": 0, "unlimited_quota_count": 0, "unknown_quota_count": 0,
		"total_success": 0, "total_fail": 0, "by_type": map[string]any{},
		"source_available": true, "healthy": false,
	}
}

func dashboardAccountStats(items []map[string]any) map[string]any {
	total := newDashboardProviderStats("all")
	providers := map[string]map[string]any{"gpt": newDashboardProviderStats("gpt")}
	for _, item := range items {
		if !isOpenAIAccount(accounts.Account{Token: accountToken(item), Fields: item}) {
			continue
		}
		for _, stats := range []map[string]any{total, providers["gpt"]} {
			stats["total"] = intValue(stats["total"]) + 1
			stats["cumulative_total"] = intValue(stats["cumulative_total"]) + 1
			category := accountStatusCategory(item)
			switch category {
			case "limited", "abnormal", "disabled":
				stats[category] = intValue(stats[category]) + 1
			default:
				stats["active"] = intValue(stats["active"]) + 1
				quota := maxInt(0, intValue(item["quota"]))
				stats["total_quota"] = intValue(stats["total_quota"]) + quota
				accountType := strings.ToLower(strings.TrimSpace(firstNonEmpty(stringValue(item["type"]), stringValue(item["plan_type"]))))
				unlimited := boolValue(item["image_quota_unknown"], false) && (accountType == "pro" || accountType == "prolite")
				if unlimited {
					stats["unlimited_quota_count"] = intValue(stats["unlimited_quota_count"]) + 1
				} else if boolValue(item["image_quota_unknown"], false) || quota <= 0 {
					stats["unknown_quota_count"] = intValue(stats["unknown_quota_count"]) + 1
				}
			}
			stats["total_success"] = intValue(stats["total_success"]) + maxInt(0, intValue(item["success"]))
			stats["total_fail"] = intValue(stats["total_fail"]) + maxInt(0, intValue(item["fail"]))
			accountType := firstNonEmpty(stringValue(item["type"]), stringValue(item["source_type"]), "unknown")
			byType := mapValue(stats["by_type"])
			byType[accountType] = intValue(byType[accountType]) + 1
		}
	}
	for _, stats := range []map[string]any{total, providers["gpt"]} {
		stats["healthy"] = intValue(stats["active"]) > 0 || intValue(stats["unlimited_quota_count"]) > 0 || intValue(stats["unknown_quota_count"]) > 0
	}
	total["providers"] = map[string]any{"gpt": providers["gpt"]}
	return total
}

func dashboardLogSummary(items []map[string]any, timeRange string, now time.Time) map[string]any {
	timeRange = strings.ToLower(strings.TrimSpace(timeRange))
	if timeRange != "7d" && timeRange != "30d" {
		timeRange = "24h"
	}
	location := now.Location()
	now = now.In(location)
	bucketCount := 24
	bucketStep := time.Hour
	currentStart := now.Truncate(time.Hour)
	labelFormat := "15:00"
	if timeRange != "24h" {
		bucketCount = 7
		if timeRange == "30d" {
			bucketCount = 30
		}
		bucketStep = 24 * time.Hour
		currentStart = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, location)
		labelFormat = "01-02"
	}
	firstStart := currentStart.Add(-time.Duration(bucketCount-1) * bucketStep)
	labels := make([]string, bucketCount)
	totalRequests := make([]int, bucketCount)
	successRequests := make([]int, bucketCount)
	failedRequests := make([]int, bucketCount)
	rateLimitedRequests := make([]int, bucketCount)
	modelRequests := map[string][]int{}
	modelTotalSums := map[string][]float64{}
	modelTotalCounts := map[string][]int{}
	modelTTFBSums := map[string][]float64{}
	modelTTFBCounts := map[string][]int{}
	for index := range labels {
		labels[index] = firstStart.Add(time.Duration(index) * bucketStep).Format(labelFormat)
	}

	byEndpoint := map[string]any{}
	byModel := map[string]any{}
	byStatus := map[string]any{}
	byErrorCode := map[string]any{}
	recentFailures := []map[string]any{}
	total, success, failed := 0, 0, 0
	if len(items) > 20000 {
		items = items[len(items)-20000:]
	}
	for _, item := range items {
		if !strings.EqualFold(stringValue(item["type"]), "call") {
			continue
		}
		detail := mapValue(item["detail"])
		startedAt, ok := dashboardLogTime(firstNonEmpty(stringValue(detail["started_at"]), stringValue(item["time"])), location)
		if !ok || startedAt.Before(firstStart) || !startedAt.Before(currentStart.Add(bucketStep)) {
			continue
		}
		bucket := int(startedAt.Sub(firstStart) / bucketStep)
		if bucket < 0 || bucket >= bucketCount {
			continue
		}
		status := strings.ToLower(strings.TrimSpace(firstNonEmpty(stringValue(detail["status"]), stringValue(item["status"]))))
		endpoint := strings.TrimSpace(firstNonEmpty(stringValue(detail["endpoint"]), stringValue(item["endpoint"])))
		modelName := strings.TrimSpace(firstNonEmpty(stringValue(detail["model"]), stringValue(item["model"])))
		errorText := strings.TrimSpace(firstNonEmpty(stringValue(detail["error"]), stringValue(detail["raw_error"])))
		errorCode := strings.TrimSpace(firstNonEmpty(stringValue(detail["error_code"]), stringValue(item["error_code"])))
		statusCode := intValue(detail["status_code"])
		isFailed := status == "failed" || status == "error" || status == "fail" || errorText != "" || statusCode >= 400
		isRateLimited := statusCode == http.StatusTooManyRequests || strings.Contains(strings.ToLower(errorCode), "rate_limit") || errorCode == "429"
		if errorCode == "" && statusCode >= 400 {
			errorCode = fmt.Sprintf("%d", statusCode)
		}

		total++
		totalRequests[bucket]++
		if isFailed {
			failed++
			if isRateLimited {
				rateLimitedRequests[bucket]++
			} else {
				failedRequests[bucket]++
			}
			recentFailures = append(recentFailures, map[string]any{
				"id": item["id"], "time": firstNonEmpty(stringValue(item["time"]), stringValue(detail["started_at"])),
				"summary": item["summary"], "endpoint": endpoint, "error_code": errorCode,
				"stage": detail["stage"], "reason": firstNonEmpty(errorText, stringValue(detail["reason"])),
				"conversation_id": detail["conversation_id"],
			})
		} else {
			success++
			successRequests[bucket]++
		}
		if status != "" {
			byStatus[status] = intValue(byStatus[status]) + 1
		}
		if strings.HasPrefix(endpoint, "/") {
			byEndpoint[endpoint] = intValue(byEndpoint[endpoint]) + 1
		}
		if modelName != "" {
			byModel[modelName] = intValue(byModel[modelName]) + 1
			ensureDashboardIntSeries(modelRequests, modelName, bucketCount)[bucket]++
			duration := dashboardLogMetric(detail, "duration_ms", "total_ms")
			if duration > 0 {
				ensureDashboardFloatSeries(modelTotalSums, modelName, bucketCount)[bucket] += duration
				ensureDashboardIntSeries(modelTotalCounts, modelName, bucketCount)[bucket]++
			}
			ttfb := dashboardLogMetric(detail, "http_ttfb_ms", "sse_first_event_ms", "stream_first_queue_ms")
			if ttfb > 0 {
				ensureDashboardFloatSeries(modelTTFBSums, modelName, bucketCount)[bucket] += ttfb
				ensureDashboardIntSeries(modelTTFBCounts, modelName, bucketCount)[bucket]++
			}
		}
		if errorCode != "" {
			byErrorCode[errorCode] = intValue(byErrorCode[errorCode]) + 1
		}
	}
	if len(recentFailures) > 10 {
		recentFailures = recentFailures[len(recentFailures)-10:]
	}
	return map[string]any{
		"total": total, "success": success, "failed": failed,
		"by_endpoint": byEndpoint, "by_model": byModel, "by_status": byStatus,
		"by_error_code": byErrorCode, "recent_failures": recentFailures,
		"trend": map[string]any{
			"labels": labels, "total_requests": totalRequests, "success_requests": successRequests,
			"failed_requests": failedRequests, "rate_limited_requests": rateLimitedRequests,
			"model_requests":    modelRequests,
			"model_total_times": dashboardAverageSeries(modelTotalSums, modelTotalCounts),
			"model_ttfb_times":  dashboardAverageSeries(modelTTFBSums, modelTTFBCounts),
		},
	}
}

func dashboardLogTime(value string, location *time.Location) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		parsed, err := time.ParseInLocation(layout, value, location)
		if err == nil {
			return parsed.In(location), true
		}
	}
	return time.Time{}, false
}

func dashboardLogMetric(detail map[string]any, keys ...string) float64 {
	monitor := mapValue(detail["monitor"])
	metrics := mapValue(monitor["metrics"])
	perf := mapValue(monitor["perf"])
	for _, key := range keys {
		for _, source := range []map[string]any{detail, metrics, perf, monitor} {
			value := float64(monitorNumber(source[key]))
			if value > 0 {
				return value
			}
		}
	}
	return 0
}

func ensureDashboardIntSeries(values map[string][]int, key string, length int) []int {
	if values[key] == nil {
		values[key] = make([]int, length)
	}
	return values[key]
}

func ensureDashboardFloatSeries(values map[string][]float64, key string, length int) []float64 {
	if values[key] == nil {
		values[key] = make([]float64, length)
	}
	return values[key]
}

func dashboardAverageSeries(sums map[string][]float64, counts map[string][]int) map[string][]float64 {
	result := map[string][]float64{}
	for modelName, totals := range sums {
		averages := make([]float64, len(totals))
		for index, total := range totals {
			if counts[modelName][index] > 0 {
				averages[index] = math.Round(total/float64(counts[modelName][index])*100) / 100
			}
		}
		result[modelName] = averages
	}
	return result
}

func mediaStats(root string) map[string]any {
	var count int
	var size int64
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() {
			count++
			size += info.Size()
		}
		return nil
	})
	return map[string]any{"count": count, "size_bytes": size}
}
