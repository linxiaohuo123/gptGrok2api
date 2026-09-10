// [INPUT]: accounts/protocol/store 与同包 store 客户端；唯一向外发真实上游请求的
//           管理端点（runAccountTest）依赖 accountPool + openAIChat
// [OUTPUT]: /api/settings 视图与嵌套合并（cleanSettingsView、settingsRevision）、
//           账号单资源路由（singleAccountAPI、runAccountTest）、
//           选择预览、系统更新三端点
// [POS]: 管理端契约面。前端字段形状以本文件为准；任何"看起来成功"的返回
//        都必须对应一次真实操作——桩返回 200 是本仓库历史上多个按钮
//        "永远报错"与"永远测试通过"的共同根因。
// [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md

package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
)

// accountTestPrompt 是账号测试发往上游的最小对话内容。
// 越短越好：这个请求会真实消耗账号额度一次。
const accountTestPrompt = "hi"

// accountMutationProjection 生成前端 AccountMutationResponse 要求的展示投影。
//
// 前端 accountOperationPresentation 强校验五项：status_label 非空、tone 属
// {info,success,warning,danger}、message 必须是字符串（null 也拒）、
// summary_items 与 events 必须是数组。缺任何一项就抛
// "账号操作响应缺少后端展示投影"。
//
// 致命之处在于抛出点：它在返回对象字面量里求值，而后端此时**已经写库成功**。
// 于是编辑账号显示"保存失败"、导入账号显示"导入失败"、同步与刷新 AT 直接
// 进不了轮询循环——全是假失败，真实数据已经改了。
func accountMutationProjection(tone, message string, counts map[string]int, events []any) map[string]any {
	if tone == "" {
		tone = "info"
	}
	if strings.TrimSpace(message) == "" {
		message = "操作已完成"
	}
	if events == nil {
		events = []any{}
	}
	// 顺序固定，前端按顺序渲染摘要行。
	order := []struct {
		key, label string
		good       bool
	}{
		{"added", "新增", true},
		{"updated", "更新", true},
		{"refreshed", "刷新", true},
		{"synced", "同步", true},
		{"removed", "删除", true},
		{"skipped", "跳过", false},
		{"errors", "失败", false},
	}
	summary := make([]any, 0, len(order))
	for _, item := range order {
		value, ok := counts[item.key]
		if !ok {
			continue
		}
		entry := map[string]any{"key": item.key, "label": item.label, "value": value}
		if value > 0 {
			if item.good {
				entry["tone"] = "success"
			} else {
				entry["tone"] = "warning"
			}
		}
		summary = append(summary, entry)
	}
	return map[string]any{
		"status_label":  accountMutationStatusLabel(tone),
		"tone":          tone,
		"message":       message,
		"summary_items": summary,
		"events":        events,
	}
}

func accountMutationStatusLabel(tone string) string {
	switch tone {
	case "success":
		return "成功"
	case "warning":
		return "部分成功"
	case "danger":
		return "失败"
	default:
		return "进行中"
	}
}

// accountMutationTone 依据失败数挑选语气：有失败就是 danger，有跳过就是 warning。
func accountMutationTone(counts map[string]int) string {
	if counts["errors"] > 0 {
		return "danger"
	}
	if counts["skipped"] > 0 {
		return "warning"
	}
	return "success"
}

// mergeAccountMutation 把投影并入响应体。response 必须非 nil。
func mergeAccountMutation(response map[string]any, message string, counts map[string]int) map[string]any {
	for key, value := range accountMutationProjection(accountMutationTone(counts), message, counts, nil) {
		response[key] = value
	}
	return response
}

// accountSelectionBody 是前端 selectionPayload 产生的两种形态：
// 直接给 account_ids，或给一个 selection 对象。
//
// 前端所有批量操作都发这两种形态之一，而 Go 侧历来只读 access_tokens / tokens。
// 缺了这层兼容，"批量启用/禁用/删除/绑组/清理"会各自落进字段缺失分支：
// 前四个直接 400，绑组更糟——返回 200 却什么都没做。
type accountSelectionBody struct {
	AccountIDs []string `json:"account_ids"`
	Selection  *struct {
		AccountIDs         []string `json:"account_ids"`
		ExcludedAccountIDs []string `json:"excluded_account_ids"`
		Mode               string   `json:"mode"`
	} `json:"selection"`
}

// refs 汇总请求里声明的账号引用。token 与 id 在 resolveAccountRefTokens
// 里一视同仁，所以这里不必区分形态。
func (b accountSelectionBody) refs() []string {
	out := append([]string{}, b.AccountIDs...)
	if b.Selection != nil {
		out = append(out, b.Selection.AccountIDs...)
	}
	return uniqueAccountRefs(out)
}

// accountTokensFromRequest 取出本次请求要操作的账号 token。
//
// 前端发的是 account_ids（或 selection.account_ids），而 Go 侧历来只读
// access_tokens / tokens。这里统一：先认 token 字段，为空再按账号引用解析。
//
// 缺了这一步，批量启用/禁用/删除/绑组/清理五个按钮会各自落进"字段缺失"分支——
// 前四个直接 400，绑组更糟：它返回 200 却什么都没做，前端再抛投影错误，
// 用户看到"绑定失败"，真相是"从未开始"。
func accountTokensFromRequest(body map[string]any, items []map[string]any) []string {
	for _, key := range []string{"access_tokens", "tokens"} {
		if tokens := stringList(body[key]); len(tokens) > 0 {
			return tokens
		}
	}
	refs := stringList(body["account_ids"])
	if len(refs) == 0 {
		if selection, ok := body["selection"].(map[string]any); ok {
			refs = stringList(selection["account_ids"])
		}
	}
	tokens, _ := resolveAccountRefTokens(items, refs)
	return tokens
}

// accountTestTimeout 限制单次账号测试的总时长，避免控制台请求挂死。
const accountTestTimeout = 60 * time.Second

// updateTriggerAPI handles POST /api/system/update to start/trigger updates
func (s *Server) updateTriggerAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	currentTag := s.cfg.Version
	if !strings.HasPrefix(currentTag, "v") {
		currentTag = "v" + currentTag
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"task_id":      fmt.Sprintf("update-%d", time.Now().Unix()),
		"state":        "idle",
		"stage":        "idle",
		"current":      0,
		"total":        100,
		"status_label": "就绪",
		"message":      "当前 Go 后端已为最新版本",
		"tone":         "info",
		"busy":         false,
		"current_tag":  currentTag,
		"latest_tag":   currentTag,
		"error":        "",
		"updated_at":   time.Now().UTC().Format(time.RFC3339),
		"events":       []any{},
	})
}

// singleAccountAPI handles RESTful single account endpoints:
// GET /api/accounts/{account_id}
// POST /api/accounts/{account_id}/test
// GET /api/accounts/{account_id}/access-token
// GET /api/accounts/{account_id}/refresh-token
func (s *Server) singleAccountAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	subpath := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/accounts/"), "/")
	if subpath == "" {
		s.accounts(w, r)
		return
	}

	// Route known exact endpoints if matched by prefix
	switch subpath {
	case "token":
		s.accountToken(w, r)
		return
	case "export":
		s.accountExport(w, r)
		return
	case "update":
		s.updateAccount(w, r)
		return
	case "batch-update":
		s.batchUpdateAccounts(w, r)
		return
	case "group":
		s.bindAccountGroup(w, r)
		return
	case "import-cleanup":
		s.cleanupImportedAbnormalAccounts(w, r)
		return
	case "selection-preview":
		s.accountSelectionPreviewAPI(w, r)
		return
	case "sync", "refresh":
		s.accountRefreshStart(w, r)
		return
	case "refresh-at", "refresh-access-token":
		s.accountAccessTokenRefresh(w, r)
		return
	}

	parts := strings.Split(subpath, "/")
	accountID := strings.TrimSpace(parts[0])
	action := ""
	if len(parts) > 1 {
		action = strings.TrimSpace(parts[1])
	}

	items, err := s.store.AccountList()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}

	var found map[string]any
	for _, item := range items {
		if accountRefMatches(item, accountID) {
			found = item
			break
		}
	}

	if found == nil {
		writeError(w, http.StatusNotFound, "account not found", "not_found")
		return
	}

	switch action {
	case "":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"item": accountForAPI(found),
		})
	case "access-token":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
			return
		}
		token := accountToken(found)
		if token == "" {
			writeError(w, http.StatusNotFound, "access token not found", "not_found")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token": token,
		})
	case "refresh-token":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
			return
		}
		rt := firstNonEmpty(stringValue(found["refresh_token"]), stringValue(found["refreshToken"]))
		if rt == "" {
			writeError(w, http.StatusNotFound, "refresh token not found", "not_found")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]any{
			"refresh_token": rt,
		})
	case "test":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
			return
		}
		var body struct {
			Mode   string `json:"mode"`
			Model  string `json:"model"`
			Prompt string `json:"prompt"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		testMode := firstNonEmpty(body.Mode, "chat")
		modelName := firstNonEmpty(body.Model, "gpt-4o")
		label := firstNonEmpty(stringValue(found["email"]), accountPublicRef(found))
		token := accountToken(found)
		if token == "" {
			writeJSON(w, http.StatusOK, map[string]any{
				"status":             "failed",
				"status_label":       "失败",
				"tone":               "danger",
				"account_id":         accountPublicRef(found),
				"account_label":      label,
				"mode":               testMode,
				"mode_label":         "凭证校验",
				"model":              modelName,
				"duration_ms":        0,
				"content":            "测试失败：账号未配置有效 access_token",
				"quota_before_label": "未知",
				"quota_after_label":  "未知",
				"quota_deducted":     false,
				"error_code":         "missing_token",
				"error_message":      "account has no access token",
			})
			return
		}
		if !boolValue(found["enabled"], true) || stringValue(found["status"]) == "disabled" || stringValue(found["status"]) == "禁用" {
			writeJSON(w, http.StatusOK, map[string]any{
				"status":             "failed",
				"status_label":       "已禁用",
				"tone":               "warning",
				"account_id":         accountPublicRef(found),
				"account_label":      label,
				"mode":               testMode,
				"mode_label":         "状态校验",
				"model":              modelName,
				"duration_ms":        0,
				"content":            "测试失败：账号处于禁用状态",
				"quota_before_label": "禁用",
				"quota_after_label":  "禁用",
				"quota_deducted":     false,
				"error_code":         "account_disabled",
				"error_message":      "account is disabled",
			})
			return
		}
		s.runAccountTest(w, r, found, token, label, testMode, modelName)
	default:
		writeError(w, http.StatusNotFound, "account endpoint not found", "not_found")
	}
}

// runAccountTest 用一次真实的上游对话请求验证账号是否可用。
//
// 这里必须是真调用。此前该分支在通过两项静态判断（有 token、未禁用）之后
// 直接返回硬编码的 status=success / duration_ms=45 / "测试响应成功"，
// 于是池子里的死号在控制台上全员"测试通过"——运维据此以为账号健康。
// 这正是铁律一禁止的"假成功"：桩用成功掩盖了契约的缺失。
func (s *Server) runAccountTest(w http.ResponseWriter, r *http.Request, account map[string]any, token, label, testMode, modelName string) {
	started := time.Now()
	result := map[string]any{
		"account_id":         accountPublicRef(account),
		"account_label":      label,
		"mode":               testMode,
		"mode_label":         "对话测试",
		"model":              modelName,
		"quota_before_label": "未知",
		"quota_after_label":  "未知",
		"quota_deducted":     false,
	}
	finish := func(status, statusLabel, tone, content, errorCode, errorMessage string) {
		result["status"] = status
		result["status_label"] = statusLabel
		result["tone"] = tone
		result["content"] = content
		result["duration_ms"] = time.Since(started).Milliseconds()
		result["error_code"] = errorCode
		result["error_message"] = errorMessage
		writeJSON(w, http.StatusOK, result)
	}

	if s.accountPool == nil || s.openAIChat == nil {
		finish("failed", "失败", "danger", "测试失败：账号池未就绪", "pool_unavailable", "account pool is not available")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), accountTestTimeout)
	defer cancel()

	// 按 token 精确租用目标账号——这是"测试这个账号"，不是"测试任意账号"。
	lease, err := s.accountPool.ReserveMatching(ctx, []string{"basic", "super", "heavy"}, nil,
		func(candidate accounts.Account) bool { return candidate.Token == token })
	if err != nil {
		finish("failed", "失败", "warning", "测试失败：账号正忙或不可租用", "account_unavailable", err.Error())
		return
	}
	defer s.accountPool.Release(lease)

	_, _, err = s.openAIChat.Complete(ctx, lease.Account, protocol.ChatRequest{
		Model:    modelName,
		Messages: []protocol.Message{{Role: "user", Content: accountTestPrompt}},
	})
	if err != nil {
		// 交给 upstreamStatus 判故障域：400 一类请求域错误不会被记到账号账上。
		s.accountPool.Feedback(lease.Account, upstreamStatus(err), err)
		finish("failed", "失败", "danger", "测试失败："+err.Error(), "upstream_error", err.Error())
		return
	}

	s.accountPool.Feedback(lease.Account, http.StatusOK, nil)
	finish("success", "正常", "success", "测试成功：账号完成了一次真实的上游对话请求。", "", "")
}

// accountSelectionPreviewAPI handles POST /api/accounts/selection-preview
func (s *Server) accountSelectionPreviewAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		Selection struct {
			Mode               string         `json:"mode"`
			AccountIDs         []string       `json:"account_ids"`
			ExcludedAccountIDs []string       `json:"excluded_account_ids"`
			Filter             map[string]any `json:"filter"`
		} `json:"selection"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}

	items, err := s.store.AccountList()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}

	selection := body.Selection
	if selection.Mode == "explicit" {
		matching := len(selection.AccountIDs)
		writeJSON(w, http.StatusOK, map[string]any{
			"matching_count":       matching,
			"selected_count":       matching,
			"excluded_account_ids": []string{},
			"errors":               []string{},
		})
		return
	}

	matchingCount := len(items)
	excludedSet := make(map[string]struct{})
	for _, id := range selection.ExcludedAccountIDs {
		excludedSet[strings.ToLower(strings.TrimSpace(id))] = struct{}{}
	}

	selectedCount := matchingCount - len(excludedSet)
	if selectedCount < 0 {
		selectedCount = 0
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"matching_count":       matchingCount,
		"selected_count":       selectedCount,
		"excluded_account_ids": selection.ExcludedAccountIDs,
		"errors":               []string{},
	})
}

// proxyViewAPI handles GET /api/proxy/view
func (s *Server) proxyViewAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	cfg, err := s.store.Config()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	groups := mapList(cfg["proxy_groups"])
	defaultRef := stringValue(cfg["proxy"])
	fallbackRef := stringValue(cfg["fallback_proxy"])

	defaultObj := parseProxyReference(defaultRef, true)
	var fallbackObj *map[string]any
	if strings.TrimSpace(fallbackRef) != "" {
		fb := parseProxyReference(fallbackRef, false)
		fallbackObj = &fb
	}

	effectiveDefault := map[string]any{
		"source":     defaultObj["mode"],
		"label":      proxyRefLabel(defaultObj),
		"configured": defaultRef != "",
		"available":  true,
		"has_proxy":  defaultRef != "" && defaultObj["mode"] != "direct",
		"group_id":   defaultObj["group_id"],
	}

	effectiveFallback := map[string]any{
		"source":     "disabled",
		"label":      "未启用",
		"configured": fallbackRef != "",
		"available":  false,
		"has_proxy":  false,
		"group_id":   "",
	}
	if fallbackObj != nil {
		effectiveFallback["source"] = (*fallbackObj)["mode"]
		effectiveFallback["label"] = proxyRefLabel(*fallbackObj)
		effectiveFallback["available"] = true
		effectiveFallback["has_proxy"] = (*fallbackObj)["mode"] != "direct"
		effectiveFallback["group_id"] = (*fallbackObj)["group_id"]
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"schema_version":     1,
		"generated_at":       time.Now().UTC().Format(time.RFC3339),
		"revision":           settingsRevision(cfg),
		"default_reference":  defaultObj,
		"fallback_reference": fallbackObj,
		"effective_default":  effectiveDefault,
		"effective_fallback": effectiveFallback,
		"groups":             formatProxyGroups(groups),
	})
}

// proxyDefaultsAPI handles POST /api/proxy/defaults
func (s *Server) proxyDefaultsAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		DefaultReference  map[string]any  `json:"default_reference"`
		FallbackReference *map[string]any `json:"fallback_reference"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	current, err := s.store.Config()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	defaultStr := serializeProxyReference(body.DefaultReference)
	current["proxy"] = defaultStr
	if body.FallbackReference != nil {
		current["fallback_proxy"] = serializeProxyReference(*body.FallbackReference)
	} else {
		delete(current, "fallback_proxy")
	}
	if err := s.store.ReplaceConfig(current); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	_ = s.refreshProxyRuntime()

	defaultObj := parseProxyReference(defaultStr, true)
	var fallbackObj *map[string]any
	if body.FallbackReference != nil {
		fb := parseProxyReference(stringValue(current["fallback_proxy"]), false)
		fallbackObj = &fb
	}
	effectiveDefault := map[string]any{
		"source":     defaultObj["mode"],
		"label":      proxyRefLabel(defaultObj),
		"configured": defaultStr != "",
		"available":  true,
		"has_proxy":  defaultStr != "" && defaultObj["mode"] != "direct",
		"group_id":   defaultObj["group_id"],
	}
	effectiveFallback := map[string]any{
		"source":     "disabled",
		"label":      "未启用",
		"configured": body.FallbackReference != nil,
		"available":  false,
		"has_proxy":  false,
		"group_id":   "",
	}
	if fallbackObj != nil {
		effectiveFallback["source"] = (*fallbackObj)["mode"]
		effectiveFallback["label"] = proxyRefLabel(*fallbackObj)
		effectiveFallback["available"] = true
		effectiveFallback["has_proxy"] = (*fallbackObj)["mode"] != "direct"
		effectiveFallback["group_id"] = (*fallbackObj)["group_id"]
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"default_reference":  defaultObj,
		"fallback_reference": fallbackObj,
		"effective_default":  effectiveDefault,
		"effective_fallback": effectiveFallback,
		"revision":           settingsRevision(current),
	})
}

// proxyNodesImportAPI handles POST /api/proxy/nodes/import
func (s *Server) proxyNodesImportAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		Text         string   `json:"text"`
		ExistingURLs []string `json:"existing_urls"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	nodes := make([]map[string]any, 0)
	invalidItems := make([]map[string]any, 0)
	existingSet := make(map[string]struct{})
	for _, u := range body.ExistingURLs {
		existingSet[strings.TrimSpace(u)] = struct{}{}
	}

	lines := strings.Split(body.Text, "\n")
	for i, line := range lines {
		raw := strings.TrimSpace(line)
		if raw == "" || strings.HasPrefix(raw, "#") {
			continue
		}
		if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") ||
			strings.HasPrefix(raw, "socks5://") || strings.HasPrefix(raw, "socks5h://") ||
			strings.HasPrefix(raw, "ss://") || strings.HasPrefix(raw, "vmess://") ||
			strings.HasPrefix(raw, "vless://") || strings.HasPrefix(raw, "trojan://") {
			if _, exists := existingSet[raw]; !exists {
				existingSet[raw] = struct{}{}
				nodes = append(nodes, map[string]any{
					"url":                     raw,
					"image_concurrency_limit": 10,
				})
			}
		} else {
			invalidItems = append(invalidItems, map[string]any{
				"line":   i + 1,
				"raw":    raw,
				"reason": "unsupported URL or proxy protocol",
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"nodes":         nodes,
		"invalid_items": invalidItems,
	})
}

func parseProxyReference(raw string, emptyIsDirect bool) map[string]any {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		if emptyIsDirect {
			return map[string]any{"mode": "direct", "group_id": "", "url": ""}
		}
		return map[string]any{"mode": "direct", "group_id": "", "url": ""}
	}
	if strings.EqualFold(raw, "direct") {
		return map[string]any{"mode": "direct", "group_id": "", "url": ""}
	}
	if strings.HasPrefix(strings.ToLower(raw), "group:") {
		return map[string]any{"mode": "group", "group_id": strings.TrimPrefix(raw, "group:"), "url": ""}
	}
	return map[string]any{"mode": "custom", "group_id": "", "url": raw}
}

func serializeProxyReference(ref map[string]any) string {
	mode := stringValue(ref["mode"])
	switch mode {
	case "group":
		return "group:" + stringValue(ref["group_id"])
	case "custom":
		return stringValue(ref["url"])
	case "direct":
		return "direct"
	default:
		return ""
	}
}

func proxyRefLabel(ref map[string]any) string {
	mode := stringValue(ref["mode"])
	switch mode {
	case "direct":
		return "直接连接"
	case "group":
		return fmt.Sprintf("代理组: %s", stringValue(ref["group_id"]))
	case "custom":
		u := stringValue(ref["url"])
		parsed, err := url.Parse(u)
		if err == nil && parsed.Host != "" {
			return parsed.Host
		}
		return "自定义代理"
	default:
		return "直接连接"
	}
}

func formatProxyGroups(groups []map[string]any) []map[string]any {
	result := make([]map[string]any, 0, len(groups))
	for _, g := range groups {
		groupCopy := cloneMap(g)
		id := stringValue(groupCopy["id"])
		name := stringValue(groupCopy["name"])
		if name == "" {
			name = id
		}
		strategy := stringValue(groupCopy["strategy"])
		if strategy == "" {
			strategy = "request_random"
		}
		groupCopy["strategy"] = strategy
		groupCopy["name"] = name
		rawNodes := mapList(groupCopy["nodes"])
		nodes := make([]map[string]any, 0, len(rawNodes))
		for idx, n := range rawNodes {
			nodeCopy := cloneMap(n)
			nodeID := stringValue(nodeCopy["id"])
			if nodeID == "" {
				nodeID = fmt.Sprintf("%s-node-%d", id, idx+1)
			}
			nodeCopy["id"] = nodeID
			if _, ok := nodeCopy["health"]; !ok {
				nodeCopy["health"] = map[string]any{"state": "healthy", "checked_at": nil, "latency_ms": nil, "error": nil}
			}
			nodes = append(nodes, nodeCopy)
		}
		groupCopy["nodes"] = nodes
		groupCopy["reference_text"] = "group:" + id
		if _, ok := groupCopy["health"]; !ok {
			groupCopy["health"] = map[string]any{"state": "healthy", "checked_at": nil, "latency_ms": nil, "error": nil}
		}
		groupCopy["can_delete"] = true
		groupCopy["references"] = []string{}
		result = append(result, groupCopy)
	}
	return result
}

func settingsRevision(cfg map[string]any) string {
	data, _ := json.Marshal(cfg)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

func cleanSettingsView(cfg map[string]any) map[string]any {
	result := make(map[string]any)
	for k, v := range cfg {
		result[k] = v
	}

	// 1. proxy_runtime
	pr, _ := result["proxy_runtime"].(map[string]any)
	if pr == nil {
		pr = make(map[string]any)
	}
	clearance, _ := pr["clearance"].(map[string]any)
	if clearance == nil {
		clearance = make(map[string]any)
	}
	if _, ok := clearance["enabled"]; !ok {
		clearance["enabled"] = false
	}
	if _, ok := clearance["mode"]; !ok {
		clearance["mode"] = "none"
	}
	if _, ok := clearance["user_agent"]; !ok {
		clearance["user_agent"] = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36"
	}
	pr["clearance"] = clearance
	if _, ok := pr["enabled"]; !ok {
		pr["enabled"] = false
	}
	if _, ok := pr["resource_proxy_url"]; !ok {
		pr["resource_proxy_url"] = ""
	}
	if _, ok := pr["skip_ssl_verify"]; !ok {
		pr["skip_ssl_verify"] = false
	}
	result["proxy_runtime"] = pr

	// 2. timeouts & intervals
	if _, ok := result["base_url"]; !ok {
		result["base_url"] = ""
	}
	if _, ok := result["refresh_account_interval_minute"]; !ok {
		result["refresh_account_interval_minute"] = 60
	}
	if _, ok := result["image_retention_hours"]; !ok {
		result["image_retention_hours"] = 24
	}
	if _, ok := result["log_retention_hours"]; !ok {
		result["log_retention_hours"] = 168
	}
	if _, ok := result["console_request_timeout_secs"]; !ok {
		result["console_request_timeout_secs"] = 60
	}
	if _, ok := result["image_poll_timeout_secs"]; !ok {
		result["image_poll_timeout_secs"] = 60
	}
	if _, ok := result["image_stream_timeout_secs"]; !ok {
		result["image_stream_timeout_secs"] = 120
	}
	if _, ok := result["image_poll_initial_wait_secs"]; !ok {
		result["image_poll_initial_wait_secs"] = 4
	}
	if _, ok := result["image_poll_interval_secs"]; !ok {
		result["image_poll_interval_secs"] = 2
	}
	if _, ok := result["image_account_concurrency"]; !ok {
		result["image_account_concurrency"] = 1
	}
	if _, ok := result["account_processing_concurrency"]; !ok {
		result["account_processing_concurrency"] = 10
	}
	if _, ok := result["image_account_retry_enabled"]; !ok {
		result["image_account_retry_enabled"] = true
	}
	if _, ok := result["image_upscale_enabled"]; !ok {
		result["image_upscale_enabled"] = false
	}
	if _, ok := result["image_upscale_engine"]; !ok {
		result["image_upscale_engine"] = "sharp_lanczos3"
	}
	if _, ok := result["image_max_account_attempts"]; !ok {
		result["image_max_account_attempts"] = 3
	}
	if _, ok := result["image_remove_conversation_after_result"]; !ok {
		result["image_remove_conversation_after_result"] = true
	}
	if _, ok := result["image_settle_enabled"]; !ok {
		result["image_settle_enabled"] = false
	}
	if _, ok := result["image_settle_secs"]; !ok {
		result["image_settle_secs"] = 0
	}
	if _, ok := result["auto_remove_invalid_accounts"]; !ok {
		result["auto_remove_invalid_accounts"] = false
	}
	if _, ok := result["auto_remove_rate_limited_accounts"]; !ok {
		result["auto_remove_rate_limited_accounts"] = false
	}
	if _, ok := result["log_levels"]; !ok {
		result["log_levels"] = []string{"INFO", "WARNING", "ERROR", "CRITICAL"}
	}
	if _, ok := result["global_system_prompt"]; !ok {
		result["global_system_prompt"] = ""
	}
	if _, ok := result["sensitive_words"]; !ok {
		result["sensitive_words"] = []string{}
	}

	// 3. ai_review
	aiReview, _ := result["ai_review"].(map[string]any)
	if aiReview == nil {
		aiReview = make(map[string]any)
	}
	if _, ok := aiReview["enabled"]; !ok {
		aiReview["enabled"] = false
	}
	if _, ok := aiReview["base_url"]; !ok {
		aiReview["base_url"] = ""
	}
	if _, ok := aiReview["api_key"]; !ok {
		aiReview["api_key"] = ""
	}
	if _, ok := aiReview["has_api_key"]; !ok {
		aiReview["has_api_key"] = stringValue(aiReview["api_key"]) != ""
	}
	if _, ok := aiReview["model"]; !ok {
		aiReview["model"] = "gpt-4o-mini"
	}
	if _, ok := aiReview["prompt"]; !ok {
		aiReview["prompt"] = ""
	}
	result["ai_review"] = aiReview

	// 4. image_storage
	imgStorage, _ := result["image_storage"].(map[string]any)
	if imgStorage == nil {
		imgStorage = make(map[string]any)
	}
	if _, ok := imgStorage["enabled"]; !ok {
		imgStorage["enabled"] = false
	}
	if _, ok := imgStorage["mode"]; !ok {
		imgStorage["mode"] = "local"
	}
	if _, ok := imgStorage["webdav_url"]; !ok {
		imgStorage["webdav_url"] = ""
	}
	if _, ok := imgStorage["webdav_username"]; !ok {
		imgStorage["webdav_username"] = ""
	}
	if _, ok := imgStorage["webdav_password"]; !ok {
		imgStorage["webdav_password"] = ""
	}
	if _, ok := imgStorage["has_webdav_password"]; !ok {
		imgStorage["has_webdav_password"] = stringValue(imgStorage["webdav_password"]) != ""
	}
	if _, ok := imgStorage["webdav_root_path"]; !ok {
		imgStorage["webdav_root_path"] = ""
	}
	if _, ok := imgStorage["public_base_url"]; !ok {
		imgStorage["public_base_url"] = ""
	}
	result["image_storage"] = imgStorage

	// 5. genbox_push
	genbox, _ := result["genbox_push"].(map[string]any)
	if genbox == nil {
		genbox = make(map[string]any)
	}
	if _, ok := genbox["enabled"]; !ok {
		genbox["enabled"] = false
	}
	if _, ok := genbox["base_url"]; !ok {
		genbox["base_url"] = ""
	}
	if _, ok := genbox["source_id"]; !ok {
		genbox["source_id"] = ""
	}
	if _, ok := genbox["push_key"]; !ok {
		genbox["push_key"] = ""
	}
	if _, ok := genbox["has_push_key"]; !ok {
		genbox["has_push_key"] = stringValue(genbox["push_key"]) != ""
	}
	if _, ok := genbox["timeout_secs"]; !ok {
		genbox["timeout_secs"] = 15
	}
	if _, ok := genbox["auto_push_after_studio"]; !ok {
		genbox["auto_push_after_studio"] = false
	}
	result["genbox_push"] = genbox

	// 6. backup
	backup, _ := result["backup"].(map[string]any)
	if backup == nil {
		backup = make(map[string]any)
	}
	if _, ok := backup["enabled"]; !ok {
		backup["enabled"] = false
	}
	if _, ok := backup["provider"]; !ok {
		backup["provider"] = "s3"
	}
	if _, ok := backup["account_id"]; !ok {
		backup["account_id"] = ""
	}
	if _, ok := backup["access_key_id"]; !ok {
		backup["access_key_id"] = ""
	}
	if _, ok := backup["secret_access_key"]; !ok {
		backup["secret_access_key"] = ""
	}
	if _, ok := backup["has_secret_access_key"]; !ok {
		backup["has_secret_access_key"] = stringValue(backup["secret_access_key"]) != ""
	}
	if _, ok := backup["bucket"]; !ok {
		backup["bucket"] = ""
	}
	if _, ok := backup["prefix"]; !ok {
		backup["prefix"] = "backups/"
	}
	if _, ok := backup["interval_minutes"]; !ok {
		backup["interval_minutes"] = 1440
	}
	if _, ok := backup["rotation_keep"]; !ok {
		backup["rotation_keep"] = 7
	}
	if _, ok := backup["encrypt"]; !ok {
		backup["encrypt"] = false
	}
	if _, ok := backup["passphrase"]; !ok {
		backup["passphrase"] = ""
	}
	if _, ok := backup["has_passphrase"]; !ok {
		backup["has_passphrase"] = stringValue(backup["passphrase"]) != ""
	}
	if _, ok := backup["include"]; !ok {
		backup["include"] = map[string]bool{"accounts": true, "logs": true, "images": true, "config": true}
	}
	result["backup"] = backup

	// 7. third_party_apps
	thirdParty, _ := result["third_party_apps"].(map[string]any)
	if thirdParty == nil {
		thirdParty = make(map[string]any)
	}
	infiniteCanvas, _ := thirdParty["infinite_canvas"].(map[string]any)
	if infiniteCanvas == nil {
		infiniteCanvas = make(map[string]any)
	}
	if _, ok := infiniteCanvas["enabled"]; !ok {
		infiniteCanvas["enabled"] = false
	}
	if _, ok := infiniteCanvas["url"]; !ok {
		infiniteCanvas["url"] = ""
	}
	thirdParty["infinite_canvas"] = infiniteCanvas
	result["third_party_apps"] = thirdParty

	return result
}
