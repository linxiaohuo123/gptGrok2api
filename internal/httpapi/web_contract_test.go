package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestWebContractSettings(t *testing.T) {
	root := t.TempDir()
	cfg := adminTestConfig(root)
	server := New(cfg)
	handler := server.Handler()

	// 1. GET /api/settings
	res := adminRequest(handler, http.MethodGet, "/api/settings", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("GET /api/settings failed: %d %s", res.Code, res.Body.String())
	}
	var getView struct {
		SchemaVersion int            `json:"schema_version"`
		Revision      string         `json:"revision"`
		Settings      map[string]any `json:"settings"`
		Fields        map[string]any `json:"fields"`
		Runtime       string         `json:"runtime"`
	}
	if err := json.NewDecoder(res.Body).Decode(&getView); err != nil {
		t.Fatalf("decode settings view failed: %v", err)
	}
	if getView.SchemaVersion != 1 {
		t.Fatalf("expected schema_version 1, got %d", getView.SchemaVersion)
	}
	if getView.Runtime != "go" {
		t.Fatalf("expected runtime go, got %s", getView.Runtime)
	}
	if intValue(getView.Settings["console_request_timeout_secs"]) <= 0 {
		t.Fatalf("expected console_request_timeout_secs > 0, got %v", getView.Settings["console_request_timeout_secs"])
	}

	// 2. PATCH /api/settings
	patchBody := `{"console_request_timeout_secs": 180, "revision": "rev1"}`
	patchRes := adminRequest(handler, http.MethodPatch, "/api/settings", strings.NewReader(patchBody))
	if patchRes.Code != http.StatusOK {
		t.Fatalf("PATCH /api/settings failed: %d %s", patchRes.Code, patchRes.Body.String())
	}
	var patchResult struct {
		ChangedFields []string       `json:"changed_fields"`
		Settings      map[string]any `json:"settings"`
	}
	if err := json.NewDecoder(patchRes.Body).Decode(&patchResult); err != nil {
		t.Fatalf("decode patch result failed: %v", err)
	}
	if intValue(patchResult.Settings["console_request_timeout_secs"]) != 180 {
		t.Fatalf("expected patched console_request_timeout_secs 180, got %v", patchResult.Settings["console_request_timeout_secs"])
	}

	// 3. GET /api/third-party-apps
	appsRes := adminRequest(handler, http.MethodGet, "/api/third-party-apps", nil)
	if appsRes.Code != http.StatusOK {
		t.Fatalf("GET /api/third-party-apps failed: %d %s", appsRes.Code, appsRes.Body.String())
	}
	var appsView struct {
		APIBaseURL                string         `json:"api_base_url"`
		ConsoleRequestTimeoutSecs int            `json:"console_request_timeout_secs"`
		ThirdPartyApps            map[string]any `json:"third_party_apps"`
	}
	if err := json.NewDecoder(appsRes.Body).Decode(&appsView); err != nil {
		t.Fatalf("decode third party apps failed: %v", err)
	}
	if appsView.ConsoleRequestTimeoutSecs <= 0 || appsView.ThirdPartyApps == nil {
		t.Fatalf("unexpected third party apps view: %+v", appsView)
	}
}

func TestWebContractSystemUpdate(t *testing.T) {
	root := t.TempDir()
	cfg := adminTestConfig(root)
	server := New(cfg)
	handler := server.Handler()

	res := adminRequest(handler, http.MethodPost, "/api/system/update", nil)
	if res.Code != http.StatusAccepted {
		t.Fatalf("POST /api/system/update expected 202, got %d %s", res.Code, res.Body.String())
	}
	var taskView struct {
		TaskID string `json:"task_id"`
		State  string `json:"state"`
	}
	if err := json.NewDecoder(res.Body).Decode(&taskView); err != nil {
		t.Fatalf("decode update task failed: %v", err)
	}
	if taskView.TaskID == "" {
		t.Fatalf("expected non-empty task_id")
	}
}

func TestWebContractAccountAliasesAndEndpoints(t *testing.T) {
	root := t.TempDir()
	cfg := adminTestConfig(root)
	server := New(cfg)
	handler := server.Handler()

	// Add an account first
	addBody := `{"accounts": [{"id": "acc_001", "email": "test@example.com", "access_token": "test_token_123", "refresh_token": "rt_test_456"}]}`
	addRes := adminRequest(handler, http.MethodPost, "/api/accounts", strings.NewReader(addBody))
	if addRes.Code != http.StatusOK {
		t.Fatalf("add account failed: %d %s", addRes.Code, addRes.Body.String())
	}

	// 1. GET /api/accounts/acc_001
	detailRes := adminRequest(handler, http.MethodGet, "/api/accounts/acc_001", nil)
	if detailRes.Code != http.StatusOK {
		t.Fatalf("GET /api/accounts/acc_001 failed: %d %s", detailRes.Code, detailRes.Body.String())
	}
	var detailResult struct {
		Item map[string]any `json:"item"`
	}
	if err := json.NewDecoder(detailRes.Body).Decode(&detailResult); err != nil {
		t.Fatalf("decode detail failed: %v", err)
	}
	if stringValue(detailResult.Item["email"]) != "test@example.com" {
		t.Fatalf("expected email test@example.com, got %v", detailResult.Item["email"])
	}

	// 2. GET /api/accounts/acc_001/access-token
	atRes := adminRequest(handler, http.MethodGet, "/api/accounts/acc_001/access-token", nil)
	if atRes.Code != http.StatusOK {
		t.Fatalf("GET /api/accounts/acc_001/access-token failed: %d %s", atRes.Code, atRes.Body.String())
	}
	var atResult struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(atRes.Body).Decode(&atResult); err != nil || atResult.AccessToken != "test_token_123" {
		t.Fatalf("unexpected access token result: %v", atResult)
	}

	// 3. GET /api/accounts/acc_001/refresh-token
	rtRes := adminRequest(handler, http.MethodGet, "/api/accounts/acc_001/refresh-token", nil)
	if rtRes.Code != http.StatusOK {
		t.Fatalf("GET /api/accounts/acc_001/refresh-token failed: %d %s", rtRes.Code, rtRes.Body.String())
	}
	var rtResult struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(rtRes.Body).Decode(&rtResult); err != nil || rtResult.RefreshToken != "rt_test_456" {
		t.Fatalf("unexpected refresh token result: %v", rtResult)
	}

	// 4. POST /api/accounts/acc_001/test
	//
	// 账号"测试"必须真的打一次上游。这条断言刻意反向写：
	// 本测试的服务端没有任何可用的上游出口，所以唯一正确的结果是失败。
	// 一旦它变成 success，就说明硬编码的桩又回来了——那正是历史上
	// "死号在控制台上全员测试通过"的根因，也是铁律一禁止的假成功。
	testBody := `{"mode": "chat", "model": "gpt-4o", "prompt": "hi"}`
	testRes := adminRequest(handler, http.MethodPost, "/api/accounts/acc_001/test", strings.NewReader(testBody))
	if testRes.Code != http.StatusOK {
		t.Fatalf("POST /api/accounts/acc_001/test failed: %d %s", testRes.Code, testRes.Body.String())
	}
	var testResult struct {
		Status       string `json:"status"`
		ErrorCode    string `json:"error_code"`
		ErrorMessage string `json:"error_message"`
	}
	if err := json.NewDecoder(testRes.Body).Decode(&testResult); err != nil {
		t.Fatalf("unexpected test result: %v", err)
	}
	if testResult.Status == "success" {
		t.Fatalf("账号测试在无法触达上游的情况下返回了 success——桩函数回归：%s", testRes.Body.String())
	}
	if testResult.ErrorCode == "" || testResult.ErrorMessage == "" {
		t.Fatalf("失败结果必须携带 error_code 与 error_message：%s", testRes.Body.String())
	}

	// 5. POST /api/accounts/sync (alias for /refresh) - test method not allowed on GET
	syncRes := adminRequest(handler, http.MethodGet, "/api/accounts/sync", nil)
	if syncRes.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for GET /api/accounts/sync, got %d", syncRes.Code)
	}

	// 6. POST /api/accounts/refresh-access-token (alias for /refresh-at)
	refAtRes := adminRequest(handler, http.MethodGet, "/api/accounts/refresh-access-token", nil)
	if refAtRes.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for GET /api/accounts/refresh-access-token, got %d", refAtRes.Code)
	}

	// 7. GET /api/accounts/operations/{id}
	server.refreshMu.Lock()
	server.refreshProgress["test_progress_1"] = &accountRefreshProgress{
		Total: 1,
		Done:  true,
	}
	server.refreshMu.Unlock()
	opRes := adminRequest(handler, http.MethodGet, "/api/accounts/operations/test_progress_1", nil)
	if opRes.Code != http.StatusOK {
		t.Fatalf("GET /api/accounts/operations/test_progress_1 expected 200, got %d %s", opRes.Code, opRes.Body.String())
	}

	// 8. POST /api/accounts/selection-preview
	prevBody := `{"selection": {"mode": "all", "account_ids": [], "excluded_account_ids": []}}`
	prevRes := adminRequest(handler, http.MethodPost, "/api/accounts/selection-preview", strings.NewReader(prevBody))
	if prevRes.Code != http.StatusOK {
		t.Fatalf("POST /api/accounts/selection-preview failed: %d %s", prevRes.Code, prevRes.Body.String())
	}
	var prevResult struct {
		MatchingCount int `json:"matching_count"`
		SelectedCount int `json:"selected_count"`
	}
	if err := json.NewDecoder(prevRes.Body).Decode(&prevResult); err != nil || prevResult.MatchingCount == 0 {
		t.Fatalf("unexpected preview result: %v", prevResult)
	}
}

func TestWebContractProxyAndGallery(t *testing.T) {
	root := t.TempDir()
	cfg := adminTestConfig(root)
	server := New(cfg)
	handler := server.Handler()

	// 1. GET /api/proxy/view
	viewRes := adminRequest(handler, http.MethodGet, "/api/proxy/view", nil)
	if viewRes.Code != http.StatusOK {
		t.Fatalf("GET /api/proxy/view failed: %d %s", viewRes.Code, viewRes.Body.String())
	}
	var proxyView struct {
		SchemaVersion    int            `json:"schema_version"`
		EffectiveDefault map[string]any `json:"effective_default"`
	}
	if err := json.NewDecoder(viewRes.Body).Decode(&proxyView); err != nil || proxyView.SchemaVersion != 1 {
		t.Fatalf("unexpected proxy view: %v", proxyView)
	}

	// 2. POST /api/proxy/defaults
	defBody := `{"default_reference": {"mode": "direct", "group_id": "", "url": ""}}`
	defRes := adminRequest(handler, http.MethodPost, "/api/proxy/defaults", strings.NewReader(defBody))
	if defRes.Code != http.StatusOK {
		t.Fatalf("POST /api/proxy/defaults failed: %d %s", defRes.Code, defRes.Body.String())
	}

	// 3. POST /api/proxy/nodes/import
	importBody := `{"text": "http://127.0.0.1:7890\nsocks5://127.0.0.1:1080\ninvalid_line"}`
	importRes := adminRequest(handler, http.MethodPost, "/api/proxy/nodes/import", strings.NewReader(importBody))
	if importRes.Code != http.StatusOK {
		t.Fatalf("POST /api/proxy/nodes/import failed: %d %s", importRes.Code, importRes.Body.String())
	}
	var importResult struct {
		Nodes        []map[string]any `json:"nodes"`
		InvalidItems []map[string]any `json:"invalid_items"`
	}
	if err := json.NewDecoder(importRes.Body).Decode(&importResult); err != nil || len(importResult.Nodes) != 2 || len(importResult.InvalidItems) != 1 {
		t.Fatalf("unexpected node import result: %v", importResult)
	}

	// 4. POST /api/images/retention-cleanup
	cleanRes := adminRequest(handler, http.MethodPost, "/api/images/retention-cleanup", nil)
	if cleanRes.Code != http.StatusOK {
		t.Fatalf("POST /api/images/retention-cleanup failed: %d %s", cleanRes.Code, cleanRes.Body.String())
	}

	// 5. POST /api/images/genbox-push
	pushRes := adminRequest(handler, http.MethodPost, "/api/images/genbox-push", strings.NewReader(`{"path": "images/test.png"}`))
	if pushRes.Code != http.StatusOK {
		t.Fatalf("POST /api/images/genbox-push failed: %d %s", pushRes.Code, pushRes.Body.String())
	}
}

func TestWebContractModelCatalog(t *testing.T) {
	root := t.TempDir()
	cfg := adminTestConfig(root)
	server := New(cfg)
	handler := server.Handler()

	res := adminRequest(handler, http.MethodGet, "/api/model-catalog", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("GET /api/model-catalog failed: %d %s", res.Code, res.Body.String())
	}
	var catalog struct {
		Object        string         `json:"object"`
		SchemaVersion int            `json:"schema_version"`
		GeneratedAt   string         `json:"generated_at"`
		Revision      string         `json:"revision"`
		ChatModels    []string       `json:"chat_models"`
		ImageModels   []string       `json:"image_models"`
		AllModels     []string       `json:"all_models"`
		Defaults      map[string]any `json:"defaults"`
		Capabilities  map[string]any `json:"capabilities"`
		Source        map[string]any `json:"source"`
	}
	if err := json.NewDecoder(res.Body).Decode(&catalog); err != nil {
		t.Fatalf("decode model catalog failed: %v", err)
	}
	if catalog.Object != "model_catalog" || catalog.SchemaVersion != 1 {
		t.Fatalf("unexpected catalog object/schema_version: %+v", catalog)
	}
	if len(catalog.Defaults) == 0 || catalog.Defaults["chat_model"] == "" {
		t.Fatalf("missing defaults: %+v", catalog.Defaults)
	}
	if len(catalog.Capabilities) == 0 {
		t.Fatalf("missing capabilities: %+v", catalog.Capabilities)
	}
	if len(catalog.Source) == 0 {
		t.Fatalf("missing source: %+v", catalog.Source)
	}
}
