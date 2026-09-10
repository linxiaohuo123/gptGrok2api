package httpapi

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/auth"
	"github.com/auucoder/gptgrok2api-go/internal/config"
)

func adminTestConfig(root string) config.Config {
	return config.Config{
		RootDir: root, DataDir: filepath.Join(root, "data"), StaticDir: filepath.Join(root, "web_dist"),
		ConfigPath: filepath.Join(root, "config.json"), AccountsPath: filepath.Join(root, "data", "accounts.json"),
		AuthKeysPath: filepath.Join(root, "data", "auth_keys.json"), APIKey: "api-secret", AdminKey: "admin-secret", Version: "test",
		ImageDataDir: filepath.Join(root, "data", "files", "images"), QueuePath: filepath.Join(root, "data", "tasks.json"),
	}
}

func adminRequest(handler http.Handler, method, path string, body io.Reader) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, body)
	request.Header.Set("Authorization", "Bearer admin-secret")
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestRuntimeMonitorLifecycle(t *testing.T) {
	monitor := newRuntimeMonitor()
	monitor.start("call-1", "/v1/images/generations", "gpt-image-2", "hello")
	monitor.update("call-1", "in_progress", 50, "")
	item, ok := monitor.detail("call-1")
	if !ok || item.Progress != 50 || item.Status != "running" {
		t.Fatalf("unexpected active item: %#v %v", item, ok)
	}
	monitor.finish("call-1", "success", "", "", "")
	item, ok = monitor.detail("call-1")
	if !ok || item.Status != "success" || item.Progress != 100 || item.Duration < 0 {
		t.Fatalf("unexpected completed item: %#v %v", item, ok)
	}
}

func TestRequestMonitorEnrichmentUpdatesLiveEgressAndAccount(t *testing.T) {
	server := &Server{monitor: newRuntimeMonitor()}
	server.monitor.start("call-egress", "/v1/images/generations", "gpt-image-2", "test")
	request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	request = request.WithContext(context.WithValue(request.Context(), monitorCallIDKey{}, "call-egress"))

	server.enrichMonitorAccount(request, accounts.Account{Pool: "basic", Fields: map[string]any{
		"email":     "image@example.test",
		"proxy_url": "http://proxy-user:proxy-pass@203.0.113.8:8080",
	}})
	server.enrichRequestMonitor(request, map[string]any{
		"egress_label": "http://203.0.113.8:8080",
		"has_proxy":    true,
	})

	record, ok := server.monitor.detail("call-egress")
	if !ok {
		t.Fatal("active monitor record missing")
	}
	if record.AccountEmail != "image@example.test" {
		t.Fatalf("unexpected account email: %q", record.AccountEmail)
	}
	if record.ProxySource != "account" || record.EgressLabel != "http://203.0.113.8:8080" || !record.HasProxy {
		t.Fatalf("unexpected egress metadata: %#v", record)
	}
	if strings.Contains(record.EgressLabel, "proxy-user") || strings.Contains(record.EgressLabel, "proxy-pass") {
		t.Fatalf("proxy credentials leaked into monitor label: %q", record.EgressLabel)
	}
}

func TestRequestMonitorDoesNotCountHandlerExecutionAsQueueTime(t *testing.T) {
	// 中间件要先判鉴权再决定读不读 body，所以这里必须给出与生产一致的 auth。
	server := &Server{monitor: newRuntimeMonitor(), auth: auth.New("api-secret", "admin-secret", "", false, nil)}
	request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"gpt-image-2","prompt":"test"}`))
	request.Header.Set("X-API-Key", "api-secret")
	response := httptest.NewRecorder()
	server.withRequestMonitor(response, request, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(20 * time.Millisecond)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))
	records := server.monitor.completed
	if len(records) != 1 {
		t.Fatalf("expected one completed monitor record, got %d", len(records))
	}
	if queue := monitorNumber(records[0].Metrics["handler_queue_ms"]); queue != 0 {
		t.Fatalf("handler execution was mislabeled as queue time: %vms", queue)
	}
	if records[0].Duration < 20 {
		t.Fatalf("test handler duration was not captured: %dms", records[0].Duration)
	}
}

func TestMonitorSnapshotWithHistorySummary(t *testing.T) {
	root := t.TempDir()
	cfg := adminTestConfig(root)
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	history := map[string]any{
		"type":    "call",
		"id":      "call-2",
		"summary": "history prompt",
		"detail": map[string]any{
			"call_id":     "call-2",
			"endpoint":    "/v1/images/generations",
			"model":       "gpt-image-2",
			"status":      "success",
			"started_at":  now.Add(-2 * time.Second).Format(time.RFC3339),
			"ended_at":    now.Format(time.RFC3339),
			"duration_ms": 2000,
			"monitor": map[string]any{
				"stage": "download",
				"metrics": map[string]any{
					"handler_queue_ms":      100,
					"stream_first_queue_ms": 120,
					"account_wait_ms":       140,
					"egress_wait_ms":        160,
					"download_ms":           180,
					"total_ms":              3000,
				},
				"perf": map[string]any{
					"response_ms": 220,
				},
				"events": []map[string]any{
					{"time": now.Format(time.RFC3339), "event": "download", "label": "下载", "download_ms": 180},
				},
			},
		},
	}
	raw, err := json.Marshal(history)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.DataDir, "logs.jsonl"), append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	server := &Server{cfg: cfg, monitor: newRuntimeMonitor()}
	server.monitor.start("call-1", "/v1/chat/completions", "gpt-4o", "hello")
	server.monitor.update("call-1", "running", 35, "")

	snapshot := server.monitorSnapshotWithHistory()
	summary, ok := snapshot["summary"].(map[string]any)
	if !ok {
		t.Fatalf("summary missing: %#v", snapshot["summary"])
	}
	if got := monitorNumber(summary["active"]); got != 1 {
		t.Fatalf("unexpected active count: %v", got)
	}
	if got := monitorNumber(summary["completed"]); got != 1 {
		t.Fatalf("unexpected completed count: %v", got)
	}
	if got := monitorNumber(summary["p95_duration_ms"]); got <= 0 {
		t.Fatalf("p95 duration missing: %#v", summary)
	}
	metricP95, ok := summary["metric_p95"].(map[string]any)
	if !ok || monitorNumber(metricP95["handler_queue_ms"]) <= 0 || monitorNumber(metricP95["total_ms"]) <= 0 {
		t.Fatalf("metric p95 missing: %#v", summary["metric_p95"])
	}
	bottleneck, ok := summary["bottleneck"].(map[string]any)
	if !ok || stringValue(bottleneck["label"]) == "" || monitorNumber(bottleneck["value_ms"]) <= 0 {
		t.Fatalf("bottleneck missing: %#v", summary["bottleneck"])
	}
	activeByModel, ok := summary["active_by_model"].(map[string]any)
	if !ok || monitorNumber(activeByModel["gpt-4o"]) != 1 {
		t.Fatalf("active_by_model missing: %#v", summary["active_by_model"])
	}
	activeByStage, ok := summary["active_by_stage"].(map[string]any)
	if !ok || monitorNumber(activeByStage["running"]) != 1 {
		t.Fatalf("active_by_stage missing: %#v", summary["active_by_stage"])
	}
}

func TestDashboardAccountAndLogSummary(t *testing.T) {
	accountStats := dashboardAccountStats([]map[string]any{
		{"access_token": "header.payload.signature", "status": "正常", "enabled": true, "quota": 25, "type": "plus", "success": 3},
		{"access_token": "second.jwt.token", "status": "限流", "enabled": true, "quota": 10, "type": "free", "fail": 1},
	})
	if intValue(accountStats["total"]) != 2 || intValue(accountStats["active"]) != 1 || intValue(accountStats["limited"]) != 1 {
		t.Fatalf("unexpected account totals: %#v", accountStats)
	}
	if intValue(accountStats["abnormal"]) != 0 || intValue(accountStats["disabled"]) != 0 || intValue(accountStats["total_quota"]) != 25 {
		t.Fatalf("unexpected account categories: %#v", accountStats)
	}
	providers := mapValue(accountStats["providers"])
	if intValue(mapValue(providers["gpt"])["total"]) != 2 || len(providers) != 1 {
		t.Fatalf("unexpected provider totals: %#v", providers)
	}

	now := time.Date(2026, 8, 30, 13, 45, 0, 0, time.FixedZone("CST", 8*60*60))
	callLog := func(id, status string, statusCode int, startedAt time.Time, duration int) map[string]any {
		return map[string]any{
			"id": id, "type": "call", "time": startedAt.Format(time.RFC3339), "summary": id,
			"detail": map[string]any{
				"status": status, "status_code": statusCode, "started_at": startedAt.Format(time.RFC3339),
				"endpoint": "/v1/images/generations", "model": "gpt-image-2", "duration_ms": duration,
				"monitor": map[string]any{"metrics": map[string]any{"http_ttfb_ms": 500, "total_ms": duration}},
			},
		}
	}
	logs := []map[string]any{
		callLog("success", "success", 200, now.Add(-30*time.Minute), 2000),
		callLog("limited", "failed", 429, now.Add(-10*time.Minute), 3000),
		callLog("old", "success", 200, now.Add(-48*time.Hour), 1000),
	}
	summary := dashboardLogSummary(logs, "24h", now)
	if intValue(summary["total"]) != 2 || intValue(summary["success"]) != 1 || intValue(summary["failed"]) != 1 {
		t.Fatalf("unexpected log totals: %#v", summary)
	}
	trend := mapValue(summary["trend"])
	labels, ok := trend["labels"].([]string)
	if !ok || len(labels) != 24 {
		t.Fatalf("unexpected trend labels: %#v", trend["labels"])
	}
	modelSeries, ok := trend["model_requests"].(map[string][]int)
	if !ok || len(modelSeries["gpt-image-2"]) != 24 || modelSeries["gpt-image-2"][23] != 2 {
		t.Fatalf("unexpected model series: %#v", trend["model_requests"])
	}
	rateLimited := trend["rate_limited_requests"].([]int)
	if rateLimited[23] != 1 {
		t.Fatalf("rate limited request missing: %#v", rateLimited)
	}
}

func TestDashboardRouteDisablesCaching(t *testing.T) {
	server := New(adminTestConfig(t.TempDir()))
	response := adminRequest(server.Handler(), http.MethodGet, "/api/dashboard?time_range=24h", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("unexpected dashboard status: %d %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("dashboard response can be cached: %#v", response.Header())
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["generated_at"] == nil || mapValue(payload["accounts"])["providers"] == nil || mapValue(payload["logs"])["trend"] == nil {
		t.Fatalf("dashboard payload is incomplete: %#v", payload)
	}
}

func TestRequestMonitorWritesMultipartCallLog(t *testing.T) {
	root := t.TempDir()
	cfg := adminTestConfig(root)
	server := &Server{cfg: cfg, monitor: newRuntimeMonitor(), auth: auth.New("api-secret", "admin-secret", "", false, nil)}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("model", "gpt-image-2"); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("prompt", "make a blue square"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/images/edits", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("X-API-Key", "api-secret")
	response := httptest.NewRecorder()
	server.withRequestMonitor(response, request, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))

	raw, err := os.ReadFile(filepath.Join(cfg.DataDir, "logs.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(raw), []byte{'\n'})
	if len(lines) != 1 {
		t.Fatalf("unexpected log line count: %d", len(lines))
	}
	var item map[string]any
	if err := json.Unmarshal(lines[0], &item); err != nil {
		t.Fatal(err)
	}
	detail := mapValue(item["detail"])
	if stringValue(detail["model"]) != "gpt-image-2" {
		t.Fatalf("model not logged: %#v", detail)
	}
	if !strings.Contains(stringValue(detail["request_text"]), "blue square") {
		t.Fatalf("prompt not logged: %#v", detail)
	}
	if stringValue(mapValue(detail["request_shape"])["content_type"]) != "multipart/form-data" {
		t.Fatalf("request shape not logged: %#v", detail)
	}
	monitor := mapValue(detail["monitor"])
	if len(anyList(monitor["events"])) < 2 {
		t.Fatalf("monitor events missing: %#v", detail)
	}
}

func TestResponseImageOutputsIgnoresMalformedURLs(t *testing.T) {
	raw := []byte(`{"choices":[{"message":{"content":"![image](http://%zz/v1/files/image?id=bad)"}}]}`)
	outputs := responseImageOutputs(raw)
	if len(outputs) != 0 {
		t.Fatalf("malformed image URL should be ignored: %#v", outputs)
	}

	raw = []byte(`{"choices":[{"message":{"content":"![image](http://127.0.0.1:8000/v1/files/image?id=ok)"}}]}`)
	outputs = responseImageOutputs(raw)
	if len(outputs) != 1 || outputs[0]["filename"] != "ok" {
		t.Fatalf("valid image URL was not recorded: %#v", outputs)
	}
}

func TestCleanupExpiredImagesUsesRetentionDays(t *testing.T) {
	root := t.TempDir()
	cfg := adminTestConfig(root)
	cfg.ImageRetentionDays = 1
	server := &Server{cfg: cfg}
	if err := os.MkdirAll(cfg.ImageDataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(cfg.ImageDataDir, "old.png")
	newPath := filepath.Join(cfg.ImageDataDir, "new.png")
	metaPath := filepath.Join(cfg.ImageDataDir, "old.png.meta.json")
	for _, path := range []string{oldPath, newPath, metaPath} {
		if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(oldPath, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(metaPath, old, old); err != nil {
		t.Fatal(err)
	}
	removed, bytes := server.cleanupExpiredImages()
	if removed != 2 || bytes != 8 {
		t.Fatalf("unexpected cleanup result: removed=%d bytes=%d", removed, bytes)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old image was not removed: %v", err)
	}
	if _, err := os.Stat(metaPath); !os.IsNotExist(err) {
		t.Fatalf("old metadata was not removed: %v", err)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("new image should remain: %v", err)
	}
}

func TestAdminImagesTagsAndBackup(t *testing.T) {
	root := t.TempDir()
	cfg := adminTestConfig(root)
	if err := os.MkdirAll(cfg.ImageDataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.ImageDataDir, "image-one.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	handler := New(cfg).Handler()

	list := adminRequest(handler, http.MethodGet, "/api/images", nil)
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), "image-one.png") {
		t.Fatalf("unexpected image list: %d %s", list.Code, list.Body.String())
	}

	tagsBody := strings.NewReader(`{"path":"image-one.png","tags":["work","work"]}`)
	tags := adminRequest(handler, http.MethodPost, "/api/images/tags", tagsBody)
	if tags.Code != http.StatusOK || !strings.Contains(tags.Body.String(), "work") {
		t.Fatalf("unexpected tags response: %d %s", tags.Code, tags.Body.String())
	}
	allTags := adminRequest(handler, http.MethodGet, "/api/images/tags", nil)
	if allTags.Code != http.StatusOK || !strings.Contains(allTags.Body.String(), "work") {
		t.Fatalf("unexpected tag list: %d %s", allTags.Code, allTags.Body.String())
	}

	backup := adminRequest(handler, http.MethodPost, "/api/backups/run", nil)
	if backup.Code != http.StatusOK {
		t.Fatalf("backup failed: %d %s", backup.Code, backup.Body.String())
	}
	var backupResponse map[string]any
	if err := json.Unmarshal(backup.Body.Bytes(), &backupResponse); err != nil {
		t.Fatal(err)
	}
	result, _ := backupResponse["result"].(map[string]any)
	key, _ := result["key"].(string)
	if key == "" {
		t.Fatalf("backup key missing: %#v", backupResponse)
	}
	archivePath := filepath.Join(cfg.DataDir, "backups", key)
	archive, err := zip.OpenReader(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	_ = archive.Close()

	delete := adminRequest(handler, http.MethodPost, "/api/images/delete", strings.NewReader(`{"paths":["image-one.png"]}`))
	if delete.Code != http.StatusOK || strings.Contains(delete.Body.String(), `"removed":0`) {
		t.Fatalf("image deletion failed: %d %s", delete.Code, delete.Body.String())
	}
	if _, err := os.Stat(filepath.Join(cfg.ImageDataDir, "image-one.png")); !os.IsNotExist(err) {
		t.Fatalf("image was not deleted: %v", err)
	}
}

func TestRemovedRegistrationEndpoints(t *testing.T) {
	root := t.TempDir()
	cfg := adminTestConfig(root)
	handler := New(cfg).Handler()

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/register", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("register endpoint should require admin key: %d", unauthorized.Code)
	}

	for _, endpoint := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/register"},
		{http.MethodPost, "/api/register/start"},
		{http.MethodGet, "/api/register/runtime"},
		{http.MethodGet, "/api/register/grok/accounts"},
		{http.MethodPost, "/api/register/grok/accounts/oauth/authorize"},
	} {
		response := adminRequest(handler, endpoint.method, endpoint.path, nil)
		if response.Code != http.StatusNotFound {
			t.Fatalf("removed registration endpoint should return 404: %s %s => %d %s", endpoint.method, endpoint.path, response.Code, response.Body.String())
		}
	}
}

func TestImportedAbnormalAccountCleanup(t *testing.T) {
	root := t.TempDir()
	cfg := adminTestConfig(root)
	if err := os.MkdirAll(filepath.Dir(cfg.AccountsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	accounts := `[
  {"access_token":"abnormal-token","status":"异常","enabled":true},
  {"access_token":"normal-token","status":"正常","enabled":true}
]`
	if err := os.WriteFile(cfg.AccountsPath, []byte(accounts), 0o600); err != nil {
		t.Fatal(err)
	}
	handler := New(cfg).Handler()

	preview := adminRequest(handler, http.MethodPost, "/api/accounts/import-cleanup", strings.NewReader(`{"access_tokens":["abnormal-token","normal-token","abnormal-token"],"remove":false}`))
	if preview.Code != http.StatusOK || !strings.Contains(preview.Body.String(), `"checked":2`) || !strings.Contains(preview.Body.String(), `"abnormal":1`) || !strings.Contains(preview.Body.String(), `"removed":0`) {
		t.Fatalf("unexpected cleanup preview: %d %s", preview.Code, preview.Body.String())
	}

	removed := adminRequest(handler, http.MethodPost, "/api/accounts/import-cleanup", strings.NewReader(`{"access_tokens":["abnormal-token","normal-token"],"remove":true}`))
	if removed.Code != http.StatusOK || !strings.Contains(removed.Body.String(), `"abnormal":1`) || !strings.Contains(removed.Body.String(), `"removed":1`) {
		t.Fatalf("unexpected cleanup result: %d %s", removed.Code, removed.Body.String())
	}
	items, err := New(cfg).store.AccountList()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || stringValue(items[0]["access_token"]) != "normal-token" {
		t.Fatalf("unexpected accounts after cleanup: %#v", items)
	}
}

func TestDashboardSchemaV5Contract(t *testing.T) {
	root := t.TempDir()
	cfg := adminTestConfig(root)
	server := New(cfg)
	handler := server.Handler()

	res := adminRequest(handler, http.MethodGet, "/api/dashboard", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("dashboard failed: %d %s", res.Code, res.Body.String())
	}

	var data map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &data); err != nil {
		t.Fatalf("unmarshal dashboard: %v", err)
	}

	if v, _ := data["schema_version"].(float64); int(v) != 5 {
		t.Fatalf("expected top-level schema_version 5, got %v", data["schema_version"])
	}

	meta, _ := data["meta"].(map[string]any)
	if meta == nil {
		t.Fatal("missing meta in dashboard response")
	}
	if v, _ := meta["schema_version"].(float64); int(v) != 5 {
		t.Fatalf("expected meta.schema_version 5, got %v", meta["schema_version"])
	}

	ranges, _ := data["ranges"].(map[string]any)
	if ranges == nil {
		t.Fatal("missing ranges in dashboard response")
	}

	for _, key := range []string{"24h", "7d", "30d"} {
		r, ok := ranges[key].(map[string]any)
		if !ok {
			t.Fatalf("missing range %s", key)
		}
		expectedBuckets := 24
		if key == "7d" {
			expectedBuckets = 7
		} else if key == "30d" {
			expectedBuckets = 30
		}
		window, _ := r["window"].(map[string]any)
		if window == nil {
			t.Fatalf("range %s: missing window", key)
		}
		if count, _ := window["bucket_count"].(float64); int(count) != expectedBuckets {
			t.Fatalf("range %s: expected window.bucket_count %d, got %v", key, expectedBuckets, window["bucket_count"])
		}
		trend, _ := r["trend"].(map[string]any)
		if trend == nil {
			t.Fatalf("range %s: missing trend", key)
		}
		labels, _ := trend["labels"].([]any)
		buckets, _ := r["buckets"].([]any)
		if len(labels) != expectedBuckets || len(buckets) != expectedBuckets {
			t.Fatalf("range %s: expected %d labels and buckets, got %d labels and %d buckets",
				key, expectedBuckets, len(labels), len(buckets))
		}
	}

	runtimeVal, _ := data["runtime"].(map[string]any)
	if runtimeVal == nil {
		t.Fatal("missing runtime in dashboard response")
	}
	mode := stringValue(runtimeVal["runtime_mode"])
	if mode != "docker" && mode != "native" {
		t.Fatalf("unexpected runtime_mode: %v", mode)
	}
	cpuCap, _ := runtimeVal["cpu_capacity"].(float64)
	if cpuCap <= 0 {
		t.Fatalf("unexpected cpu_capacity: %v", cpuCap)
	}
	uptime, _ := runtimeVal["service_uptime_seconds"].(float64)
	if uptime < 0 {
		t.Fatalf("unexpected service_uptime_seconds: %v", uptime)
	}
	scope := stringValue(runtimeVal["memory_scope"])
	if scope != "container" && scope != "system" && scope != "visible" {
		t.Fatalf("unexpected memory_scope: %v", scope)
	}

	storageVal, _ := data["storage"].(map[string]any)
	if storageVal == nil {
		t.Fatal("missing storage in dashboard response")
	}
	imageStorage, _ := storageVal["image_storage"].(map[string]any)
	if imageStorage == nil || imageStorage["status"] != "not_checked" {
		t.Fatalf("expected image_storage.status 'not_checked', got %#v", imageStorage)
	}
}

func TestLogDetailAPIAndPresentation(t *testing.T) {
	root := t.TempDir()
	cfg := adminTestConfig(root)
	if err := os.MkdirAll(cfg.ImageDataDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create a test image file
	testImgPath := filepath.Join(cfg.ImageDataDir, "img123.png")
	// Minimal 1x1 PNG bytes
	pngHeader := []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, 0x89,
	}
	if err := os.WriteFile(testImgPath, pngHeader, 0o644); err != nil {
		t.Fatal(err)
	}

	logLine := map[string]any{
		"id":      "call-test-123",
		"time":    time.Now().UTC().Format(time.RFC3339),
		"type":    "call",
		"summary": "画一只猫",
		"detail": map[string]any{
			"call_id":  "call-test-123",
			"endpoint": "/v1/images/generations",
			"model":    "gpt-image-2",
			"status":   "success",
			"request_meta": map[string]any{
				"size":            "1024x1024",
				"quality":         "standard",
				"response_format": "url",
			},
			"result_images": []any{
				map[string]any{"width": 1024, "height": 1024},
			},
		},
	}
	lineBytes, _ := json.Marshal(logLine)
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.DataDir, "logs.jsonl"), append(lineBytes, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	server := New(cfg)
	handler := server.Handler()

	// 1. Test single log detail endpoint /api/logs/{log_id}
	res := adminRequest(handler, http.MethodGet, "/api/logs/call-test-123", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("get log detail failed: %d %s", res.Code, res.Body.String())
	}
	var detailResp map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &detailResp); err != nil {
		t.Fatalf("unmarshal log detail: %v", err)
	}
	if stringValue(detailResp["id"]) != "call-test-123" {
		t.Fatalf("unexpected id in log detail: %v", detailResp["id"])
	}
	presentation, _ := detailResp["presentation"].(map[string]any)
	if presentation == nil {
		t.Fatal("missing presentation in log detail")
	}
	resObj, _ := presentation["result"].(map[string]any)
	if resObj == nil || stringValue(resObj["resolution"]) != "1024×1024" {
		t.Fatalf("unexpected resolution in presentation: %#v", resObj)
	}

	// 2. Test log list aggregation /api/logs
	listRes := adminRequest(handler, http.MethodGet, "/api/logs", nil)
	if listRes.Code != http.StatusOK {
		t.Fatalf("get logs list failed: %d %s", listRes.Code, listRes.Body.String())
	}
	var listResp map[string]any
	if err := json.Unmarshal(listRes.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("unmarshal logs list: %v", err)
	}
	if _, ok := listResp["facets"].(map[string]any); !ok {
		t.Fatal("missing facets in logs list response")
	}
	if _, ok := listResp["stats"].(map[string]any); !ok {
		t.Fatal("missing stats in logs list response")
	}
	if _, ok := listResp["has_more"].(bool); !ok {
		t.Fatal("missing has_more in logs list response")
	}

	// 3. Test update status endpoint
	updateRes := adminRequest(handler, http.MethodGet, "/api/system/update-status", nil)
	if updateRes.Code != http.StatusOK {
		t.Fatalf("get update status failed: %d %s", updateRes.Code, updateRes.Body.String())
	}
	var updateResp map[string]any
	if err := json.Unmarshal(updateRes.Body.Bytes(), &updateResp); err != nil {
		t.Fatalf("unmarshal update status: %v", err)
	}
	if updateResp["current_tag"] == nil {
		t.Fatal("missing current_tag in update status")
	}
}

func TestSearchAPIEndpoint(t *testing.T) {
	root := t.TempDir()
	cfg := adminTestConfig(root)
	server := New(cfg)
	handler := server.Handler()

	// 1. Missing auth
	req1 := httptest.NewRequest(http.MethodPost, "/v1/search", strings.NewReader(`{"prompt":"hello"}`))
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthorized search, got %d", rec1.Code)
	}

	// 2. Empty prompt
	req2 := httptest.NewRequest(http.MethodPost, "/v1/search", strings.NewReader(`{"prompt":""}`))
	req2.Header.Set("Authorization", "Bearer api-secret")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty prompt, got %d %s", rec2.Code, rec2.Body.String())
	}

	// 3. Valid search with no accounts
	req3 := httptest.NewRequest(http.MethodPost, "/v1/search", strings.NewReader(`{"prompt":"test search query"}`))
	req3.Header.Set("Authorization", "Bearer api-secret")
	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when no account available, got %d %s", rec3.Code, rec3.Body.String())
	}
}

func TestDynamicPollTimeoutSettings(t *testing.T) {
	root := t.TempDir()
	cfg := adminTestConfig(root)
	server := New(cfg)
	handler := server.Handler()

	body := `{"image_poll_timeout_secs": 45, "image_poll_interval_secs": 3, "image_poll_initial_wait_secs": 2, "console_request_timeout_secs": 300}`
	res := adminRequest(handler, http.MethodPost, "/api/settings", strings.NewReader(body))
	if res.Code != http.StatusOK {
		t.Fatalf("update settings failed: %d %s", res.Code, res.Body.String())
	}

	if server.cfg.ImagePollTimeout != 45*time.Second {
		t.Fatalf("expected cfg.ImagePollTimeout 45s, got %v", server.cfg.ImagePollTimeout)
	}
	if server.cfg.ImagePollInterval != 3*time.Second {
		t.Fatalf("expected cfg.ImagePollInterval 3s, got %v", server.cfg.ImagePollInterval)
	}
	if server.cfg.ImagePollInitialWait != 2*time.Second {
		t.Fatalf("expected cfg.ImagePollInitialWait 2s, got %v", server.cfg.ImagePollInitialWait)
	}
	if server.cfg.ConsoleRequestTimeout != 300*time.Second {
		t.Fatalf("expected cfg.ConsoleRequestTimeout 300s, got %v", server.cfg.ConsoleRequestTimeout)
	}

	if server.openAIImage.PollTimeout != 45*time.Second {
		t.Fatalf("expected openAIImage.PollTimeout 45s, got %v", server.openAIImage.PollTimeout)
	}
	if server.openAIImage.PollInterval != 3*time.Second {
		t.Fatalf("expected openAIImage.PollInterval 3s, got %v", server.openAIImage.PollInterval)
	}
	if server.openAIImage.InitialWait != 2*time.Second {
		t.Fatalf("expected openAIImage.InitialWait 2s, got %v", server.openAIImage.InitialWait)
	}
}

func TestImageDimensionsExtraction(t *testing.T) {
	root := t.TempDir()
	cfg := adminTestConfig(root)
	if err := os.MkdirAll(cfg.ImageDataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	server := New(cfg)

	// Test missing file
	w, h := server.lookupImageDimensions("non_existent")
	if w != 0 || h != 0 {
		t.Fatalf("expected 0,0 for missing image, got %d,%d", w, h)
	}

	// Test valid 1x1 PNG
	pngHeader := []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, 0x89,
	}
	testImgPath := filepath.Join(cfg.ImageDataDir, "sample_1x1.png")
	if err := os.WriteFile(testImgPath, pngHeader, 0o644); err != nil {
		t.Fatal(err)
	}

	w, h = server.lookupImageDimensions("sample_1x1")
	if w != 1 || h != 1 {
		t.Fatalf("expected 1,1 for sample_1x1, got %d,%d", w, h)
	}
}
