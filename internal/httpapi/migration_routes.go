// [INPUT]: internal/provider，标准库（os、net/http 等）
// [OUTPUT]: 迁移兼容路由：版本元数据、/internal/* 调度器与监控、备份/存储测试、iCloud 代理转发
// [POS]: Go 版补齐 Python 版接口的兼容层。支持调度器 execute 真实生图闭环与内部监控。
// [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md

package httpapi

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

const (
	projectRepositoryURL = "https://github.com/lichao199208/gptGrok2api"
	projectVersionURL    = "https://raw.githubusercontent.com/lichao199208/gptGrok2api/main/VERSION"
	projectChangelogURL  = "https://raw.githubusercontent.com/lichao199208/gptGrok2api/main/CHANGELOG.md"
)

func (s *Server) metaUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	currentVersion := strings.TrimSpace(s.cfg.Version)
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	latestVersion, versionErr := fetchProjectText(ctx, s.requestClient, projectVersionURL, 256)
	changelog, changelogErr := fetchProjectText(ctx, s.requestClient, projectChangelogURL, 2<<20)
	response := map[string]any{
		"current_version":  currentVersion,
		"latest_version":   strings.TrimSpace(latestVersion),
		"release_name":     strings.TrimSpace(latestVersion),
		"release_url":      projectRepositoryURL,
		"changelog":        changelog,
		"release_notes":    changelog,
		"runtime":          "go",
		"update_available": projectVersionNewer(latestVersion, currentVersion),
		"status":           "ok",
	}
	if versionErr != nil {
		response["latest_version"] = currentVersion
		response["release_name"] = currentVersion
		response["update_available"] = false
		response["status"] = "error"
		response["error"] = "GitHub VERSION 读取失败"
	}
	if changelogErr != nil {
		if local, err := os.ReadFile(s.cfg.RelativePath("CHANGELOG.md")); err == nil {
			response["changelog"] = string(local)
			response["release_notes"] = string(local)
		} else if versionErr == nil {
			response["status"] = "error"
			response["error"] = "GitHub 更新日志读取失败"
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func fetchProjectText(ctx context.Context, client *http.Client, endpoint string, limit int64) (string, error) {
	if client == nil {
		client = http.DefaultClient
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "text/plain, text/markdown, */*")
	request.Header.Set("User-Agent", "gptgrok2api-go-version-check")
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("GitHub returned HTTP %d", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, limit))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

func projectVersionNewer(latest, current string) bool {
	left := projectVersionParts(latest)
	right := projectVersionParts(current)
	for index := 0; index < max(len(left), len(right)); index++ {
		var leftValue, rightValue int
		if index < len(left) {
			leftValue = left[index]
		}
		if index < len(right) {
			rightValue = right[index]
		}
		if leftValue != rightValue {
			return leftValue > rightValue
		}
	}
	return false
}

func projectVersionParts(value string) []int {
	value = strings.TrimSpace(strings.TrimPrefix(strings.ToLower(value), "v"))
	parts := []int{}
	for _, segment := range strings.FieldsFunc(value, func(r rune) bool { return r < '0' || r > '9' }) {
		parsed, err := strconv.Atoi(segment)
		if err == nil {
			parts = append(parts, parsed)
		}
	}
	return parts
}

func (s *Server) importAccountsAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAccountImport(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	var request struct {
		URL      string            `json:"url"`
		Headers  map[string]string `json:"headers"`
		Tokens   []string          `json:"tokens"`
		Accounts []map[string]any  `json:"accounts"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	if len(request.Accounts) > 0 || len(request.Tokens) > 0 {
		accounts := make([]map[string]any, 0, len(request.Accounts))
		for _, account := range request.Accounts {
			accounts = append(accounts, normalizeImportedAccount(account))
		}
		added, skipped, items, err := s.store.AddAccounts(request.Tokens, accounts)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"added": added, "skipped": skipped, "items": accountsForAPI(items)})
		return
	}
	target, err := url.Parse(strings.TrimSpace(request.URL))
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") {
		writeError(w, http.StatusBadRequest, "valid import URL is required", "invalid_request_error")
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target.String(), nil)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	for key, value := range request.Headers {
		req.Header.Set(key, value)
	}
	resp, err := s.requestClient.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error(), "upstream_error")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		writeError(w, http.StatusBadGateway, "account import endpoint returned "+resp.Status, "upstream_error")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error(), "upstream_error")
		return
	}
	var payload any
	if err := json.Unmarshal(raw, &payload); err != nil {
		writeError(w, http.StatusBadGateway, "account import response is not JSON", "upstream_error")
		return
	}
	tokens, accounts := importedAccountValues(payload)
	for index := range accounts {
		accounts[index] = normalizeImportedAccount(accounts[index])
	}
	added, skipped, items, err := s.store.AddAccounts(tokens, accounts)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"added": added, "skipped": skipped, "items": accountsForAPI(items)})
}

func (s *Server) requireAccountImport(w http.ResponseWriter, r *http.Request) bool {
	if s.auth.ValidAdminRequest(r) {
		return true
	}
	config, err := s.store.Config()
	if err == nil {
		settings := mapValue(config["account_import_api"])
		expected := strings.TrimSpace(stringValue(settings["key"]))
		provided := strings.TrimSpace(r.Header.Get("X-API-Key"))
		if boolValue(settings["enabled"], false) && expected != "" && len(expected) == len(provided) && subtle.ConstantTimeCompare([]byte(expected), []byte(provided)) == 1 {
			return true
		}
	}
	writeError(w, http.StatusUnauthorized, "账号导入 API 未启用或密钥无效", "authentication_error")
	return false
}

func normalizeImportedAccount(account map[string]any) map[string]any {
	result := cloneMap(account)
	if token := firstNonEmpty(stringValue(result["access_token"]), stringValue(result["accessToken"]), stringValue(result["token"])); token != "" {
		result["access_token"] = token
	}
	if password := firstNonEmpty(stringValue(result["login_password"]), stringValue(result["password"]), stringValue(result["account_password"])); password != "" {
		result["login_password"] = password
	}
	if secret := firstNonEmpty(stringValue(result["two_factor_secret"]), stringValue(result["totp_secret"]), stringValue(result["two_fa"]), stringValue(result["2fa"]), stringValue(result["2fa_secret"])); secret != "" {
		result["two_factor_secret"] = secret
	}
	delete(result, "accessToken")
	delete(result, "account_password")
	delete(result, "totp_secret")
	delete(result, "two_fa")
	delete(result, "2fa")
	delete(result, "2fa_secret")
	return result
}

func importedAccountValues(value any) ([]string, []map[string]any) {
	tokens := []string{}
	accounts := []map[string]any{}
	var walk func(any)
	walk = func(item any) {
		switch typed := item.(type) {
		case []any:
			for _, child := range typed {
				walk(child)
			}
		case map[string]any:
			if token := firstNonEmpty(stringValue(typed["access_token"]), stringValue(typed["token"])); token != "" {
				accounts = append(accounts, typed)
				return
			}
			for _, key := range []string{"items", "accounts", "data", "results"} {
				if child, ok := typed[key]; ok {
					walk(child)
				}
			}
		case string:
			if clean := strings.TrimSpace(typed); clean != "" {
				tokens = append(tokens, clean)
			}
		}
	}
	walk(value)
	return tokens, accounts
}

func (s *Server) backupTest(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	if err := os.MkdirAll(s.backupDir(), 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	file, err := os.CreateTemp(s.backupDir(), ".probe-*")
	if err == nil {
		name := file.Name()
		err = file.Close()
		_ = os.Remove(name)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": map[string]any{"ok": true, "status": http.StatusOK, "backend": "local", "directory": s.backupDir()}})
}

func (s *Server) imageStorageTest(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	if err := os.MkdirAll(s.cfg.ImageDataDir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	// Go 版只有本地目录，没有 WebDAV。这里如实报失败，
	// 否则界面会在什么都没测的情况下显示"WebDAV 测试通过"。
	writeJSON(w, http.StatusOK, map[string]any{"result": map[string]any{
		"ok":      false,
		"status":  0,
		"backend": "local",
		"error":   "Go 版未实现 WebDAV 图片存储，图片仅保存在本地目录",
	}})
}

func (s *Server) imageStorageSync(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	// 没有远端可传：本地已有的图片一律计入 skipped，绝不虚报 uploaded。
	items := listMediaItems(s.cfg.ImageDataDir, "image", "")
	writeJSON(w, http.StatusOK, map[string]any{"result": map[string]any{
		"uploaded":   0,
		"skipped":    len(items),
		"failed":     0,
		"backend":    "local",
		"total_size": mediaItemsSize(items),
	}})
}

func (s *Server) proxyProfileByID(w http.ResponseWriter, r *http.Request) {
	s.proxyResourceByID(w, r, "proxy_profiles", "/api/proxy/profiles/", "profiles")
}

func (s *Server) proxyGroupByID(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/proxy/groups/"), "/")
	if strings.HasSuffix(path, "/subscription/refresh") {
		id := strings.TrimSuffix(path, "/subscription/refresh")
		s.refreshProxyGroupSubscription(w, r, id)
		return
	}
	s.proxyResourceByID(w, r, "proxy_groups", "/api/proxy/groups/", "groups")
}

func (s *Server) proxyResourceByID(w http.ResponseWriter, r *http.Request, configKey, prefix, responseKey string) {
	if !s.requireAdmin(w, r) {
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, prefix), "/")
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	cfg, err := s.store.Config()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	values := mapList(cfg[configKey])
	next := make([]map[string]any, 0, len(values))
	found := false
	for _, item := range values {
		if stringValue(item["id"]) == id {
			found = true
			continue
		}
		next = append(next, item)
	}
	if !found {
		writeError(w, http.StatusNotFound, "proxy resource not found", "not_found")
		return
	}
	updated, err := s.store.UpdateConfig(configKey, next)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	if configKey == "proxy_groups" {
		if err := s.refreshProxyRuntime(); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id, responseKey: mapList(updated[configKey])})
}

func (s *Server) iCloudClaimStatusSync(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("ICLOUD_PRIVACY_MAIL_BASE_URL")), "/")
	if base == "" {
		writeError(w, http.StatusServiceUnavailable, "iCloud privacy mail service is not configured", "not_configured")
		return
	}
	var body map[string]any
	if !decodeJSON(w, r, &body) {
		return
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, base+"/api/v1/mailboxes/claim-status", bytes.NewReader(raw))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if key := strings.TrimSpace(os.Getenv("ICLOUD_PRIVACY_MAIL_API_KEY")); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("X-API-Key", key)
	}
	resp, err := s.requestClient.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error(), "upstream_error")
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 8<<20))
}

func (s *Server) internalImageMonitor(w http.ResponseWriter, r *http.Request) {
	if !s.requireInternal(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	var body map[string]any
	if !decodeJSON(w, r, &body) {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/internal/image-monitor/")
	callID := stringValue(body["call_id"])
	switch path {
	case "start":
		s.monitor.start(callID, stringValue(body["endpoint"]), stringValue(body["model"]), stringValue(body["summary"]))
	case "stage":
		s.monitor.update(callID, stringValue(body["event"]), intValue(body["progress"]), stringValue(body["error"]))
		s.monitor.enrich(callID, body)
	case "finish":
		s.monitor.finish(callID, firstNonEmpty(stringValue(body["status"]), "success"), stringValue(body["model"]), stringValue(body["summary"]), stringValue(body["error"]))
	default:
		writeError(w, http.StatusNotFound, "monitor endpoint not found", "not_found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) internalImageScheduler(w http.ResponseWriter, r *http.Request) {
	if !s.requireInternal(w, r) {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/internal/image-scheduler/")
	if path == "status" && r.Method == http.MethodGet {
		// 必须在锁内拷贝：reserve/execute/release 都在改这些 map，
		// 出锁后才序列化等于没加锁。
		s.schedulerMu.Lock()
		items := make([]map[string]any, 0, len(s.schedulerLeases))
		for _, item := range s.schedulerLeases {
			items = append(items, cloneMap(item))
		}
		s.schedulerMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"runtime": "go", "active": len(items), "reservations": items})
		return
	}
	if path == "reserve" && r.Method == http.MethodPost {
		var body map[string]any
		if !decodeJSON(w, r, &body) {
			return
		}
		id := externalID("reservation")
		item := map[string]any{"id": id, "reservation_id": id, "model": firstNonEmpty(stringValue(body["model"]), "gpt-image-2"), "status": "reserved", "created_at": time.Now().UTC()}
		s.schedulerMu.Lock()
		s.schedulerLeases[id] = item
		s.schedulerMu.Unlock()
		// 交出的是快照：这个 map 已经进了 leases，随时会被 execute 改写。
		writeJSON(w, http.StatusOK, cloneMap(item))
		return
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 2 || r.Method != http.MethodPost {
		writeError(w, http.StatusNotFound, "scheduler endpoint not found", "not_found")
		return
	}
	id, action := parts[0], parts[1]
	s.schedulerMu.Lock()
	item := s.schedulerLeases[id]
	if item == nil {
		s.schedulerMu.Unlock()
		writeError(w, http.StatusNotFound, "reservation not found", "not_found")
		return
	}
	if action == "release" {
		delete(s.schedulerLeases, id)
		s.schedulerMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "reservation_id": id, "status": "released"})
		return
	}
	if action == "execute" || action == "execute-edit" {
		item["status"] = "claimed"
		item["claimed_at"] = time.Now().UTC()
		lease := cloneMap(item)
		s.schedulerMu.Unlock()

		if s.openAIImage == nil || s.accountPool == nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "lease": lease, "execution": "handled directly by Go public image endpoint"})
			return
		}

		if action == "execute-edit" {
			parsed, err := s.parseImageEditRequest(r)
			if err != nil {
				writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
				return
			}
			// 与公开的 /v1/images/edits 同一套上界。此前这条内部路径完全不校验 n，
			// 一个 n=100000 的请求足以让分配与 goroutine 数量线性膨胀。
			if !validImageCount(parsed.N, maxImageEditCount) {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("n must be between 1 and %d", maxImageEditCount), "invalid_request_error")
				return
			}
			data, err := s.generateOpenAIImageData(r, r.Context(), parsed.Prompt, parsed.Model, parsed.Size, parsed.Quality, parsed.Inputs, parsed.ResponseFormat, requestPublicBase(r), parsed.N)
			if err != nil {
				writeError(w, upstreamStatus(err), err.Error(), "upstream_error")
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"created": time.Now().Unix(), "data": data})
			return
		}

		var payload struct {
			Request map[string]any `json:"request"`
			Model   string         `json:"model"`
			Prompt  string         `json:"prompt"`
			N       int            `json:"n"`
			Size    string         `json:"size"`
			Quality string         `json:"quality"`
			Format  string         `json:"response_format"`
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body", "invalid_request_error")
			return
		}
		_ = json.Unmarshal(raw, &payload)
		prompt := payload.Prompt
		modelName := payload.Model
		size := payload.Size
		quality := payload.Quality
		format := payload.Format
		count := payload.N
		if payload.Request != nil {
			if p := stringValue(payload.Request["prompt"]); p != "" {
				prompt = p
			}
			if m := stringValue(payload.Request["model"]); m != "" {
				modelName = m
			}
			if sz := stringValue(payload.Request["size"]); sz != "" {
				size = sz
			}
			if q := stringValue(payload.Request["quality"]); q != "" {
				quality = q
			}
			if f := stringValue(payload.Request["response_format"]); f != "" {
				format = f
			}
			if n := intValue(payload.Request["n"]); n > 0 {
				count = n
			}
		}
		if strings.TrimSpace(prompt) == "" {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "lease": lease, "execution": "handled directly by Go public image endpoint"})
			return
		}
		if strings.TrimSpace(modelName) == "" {
			modelName = "gpt-image-2"
		}
		if count <= 0 {
			count = 1
		}
		// 与公开的 /v1/images/generations 同一套上界，理由同 execute-edit。
		if !validImageCount(count, maxImageGenerateCount) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("n must be between 1 and %d", maxImageGenerateCount), "invalid_request_error")
			return
		}
		if size == "" {
			size = "1024x1024"
		}
		size = provider.NormalizeOpenAIImageSize(size)
		if format == "" {
			format = "url"
		}
		if quality == "" {
			quality = "auto"
		}

		data, err := s.generateOpenAIImageData(r, r.Context(), prompt, modelName, size, quality, nil, format, requestPublicBase(r), count)
		if err != nil {
			writeError(w, upstreamStatus(err), err.Error(), "upstream_error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"created": time.Now().Unix(), "data": data})
		return
	}
	s.schedulerMu.Unlock()
	writeError(w, http.StatusNotFound, "scheduler endpoint not found", "not_found")
}

func (s *Server) internalCallLog(w http.ResponseWriter, r *http.Request) {
	if !s.requireInternal(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	var body map[string]any
	if !decodeJSON(w, r, &body) {
		return
	}
	path := filepath.Join(s.cfg.RootDir, "logs", "calls.log")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	defer file.Close()
	line, _ := json.Marshal(map[string]any{"created_at": time.Now().UTC(), "payload": body})
	_, err = file.Write(append(line, '\n'))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// 内部端点一律 fail-closed：密钥没配就拒绝服务，而不是敞开。
// 原实现写成 expected != "" && ...，于是"我忘了配"和"我不需要鉴权"
// 变成了同一件事，而这两个端点对公网可达。
func (s *Server) requireInternal(w http.ResponseWriter, r *http.Request) bool {
	expected := strings.TrimSpace(os.Getenv("GO_IMAGE_SCHEDULER_KEY"))
	if expected == "" {
		writeError(w, http.StatusServiceUnavailable, "internal scheduler key is not configured", "not_configured")
		return false
	}
	provided := strings.TrimSpace(r.Header.Get("X-Image-Scheduler-Key"))
	if subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
		writeError(w, http.StatusUnauthorized, "invalid internal scheduler key", "authentication_error")
		return false
	}
	return true
}
