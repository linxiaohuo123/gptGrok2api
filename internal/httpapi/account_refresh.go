// [INPUT]: provider
// [OUTPUT]: 账号 AT 刷新：accountRefreshStart、进度查询、单账号刷新
// [POS]: 异步刷新任务调度与进度汇报。保留最近有限历史（50条），杜绝长期运行内存泄漏。
// [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md

package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/provider"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

type accountRefreshProgress struct {
	Total        int            `json:"total"`
	Processed    int            `json:"processed"`
	Done         bool           `json:"done"`
	Error        string         `json:"error,omitempty"`
	StatusCounts map[string]int `json:"status_counts"`
	TotalQuota   int            `json:"total_quota"`
	Result       map[string]any `json:"result,omitempty"`
	createdAt    time.Time
}

func (s *Server) accountAccessTokenRefresh(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		AccessTokens []string `json:"access_tokens"`
		AccountIDs   []string `json:"account_ids"`
		Selection    *struct {
			AccountIDs         []string `json:"account_ids"`
			ExcludedAccountIDs []string `json:"excluded_account_ids"`
			Mode               string   `json:"mode"`
		} `json:"selection"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	rawRefs := append([]string{}, body.AccessTokens...)
	rawRefs = append(rawRefs, body.AccountIDs...)
	if body.Selection != nil {
		rawRefs = append(rawRefs, body.Selection.AccountIDs...)
	}
	refs := uniqueAccountRefs(rawRefs)
	if len(refs) == 0 {
		writeError(w, http.StatusBadRequest, "access_tokens or account_ids is required", "invalid_request_error")
		return
	}
	updated := 0
	errorsOut := make([]map[string]any, 0)
	for _, ref := range refs {
		account, err := s.accountByRef(ref)
		if err != nil {
			errorsOut = append(errorsOut, map[string]any{"token": tokenPreview(ref), "error": safeRefreshError(err)})
			continue
		}
		oldToken := accountToken(account)
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		result, refreshErr := s.openAIAccountClient().RefreshAccessToken(ctx, account)
		cancel()
		if refreshErr != nil {
			errorsOut = append(errorsOut, map[string]any{"token": tokenPreview(ref), "error": safeRefreshError(refreshErr)})
			continue
		}
		if _, _, err = s.store.RotateAccountTokens(oldToken, result.AccessToken, result.RefreshToken, result.IDToken, result.Fields); err != nil {
			errorsOut = append(errorsOut, map[string]any{"token": tokenPreview(ref), "error": safeRefreshError(err)})
			continue
		}
		updated++
	}
	items, _ := s.store.AccountList()
	writeJSON(w, http.StatusOK, map[string]any{"updated": updated, "success_count": updated, "errors": errorsOut, "items": accountsForAPI(items)})
}

func (s *Server) accountRefreshStart(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		accountSelectionBody
		AccessTokens []string `json:"access_tokens"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	// 必须认 account_ids / selection：前端永远发这两种形态之一，而这里原本只读
	// access_tokens。读不到就落进下面的"全量"兜底，于是"同步选中的 3 个账号"
	// 实际刷新的是**整个账号池**——故障域越界，且用户看不到任何异常。
	refs := uniqueAccountRefs(append(append([]string{}, body.AccessTokens...), body.refs()...))
	if len(refs) == 0 {
		// 只有请求里确实没带任何目标时，才把它理解为"全量同步"。
		items, err := s.store.AccountList()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
			return
		}
		for _, item := range items {
			if ref := accountPublicRef(item); ref != "" && accountToken(item) != "" {
				refs = append(refs, ref)
			}
		}
		refs = uniqueAccountRefs(refs)
	}
	if len(refs) == 0 {
		writeError(w, http.StatusBadRequest, "access_tokens or account_ids is required", "invalid_request_error")
		return
	}

	const maxRefreshProgressHistory = 50
	progressID := newChatID()
	s.refreshMu.Lock()
	s.pruneRefreshProgressLocked(maxRefreshProgressHistory)
	s.refreshProgress[progressID] = &accountRefreshProgress{
		Total:        len(refs),
		StatusCounts: map[string]int{"正常": 0, "限流": 0, "异常": 0, "禁用": 0},
		createdAt:    time.Now(),
	}
	s.refreshMu.Unlock()

	go s.runAccountRefresh(progressID, refs)
	writeJSON(w, http.StatusOK, map[string]any{"progress_id": progressID})
}

func (s *Server) pruneRefreshProgressLocked(maxCapacity int) {
	if len(s.refreshProgress) < maxCapacity {
		return
	}
	var candidateID string
	var candidateTime time.Time
	for id, p := range s.refreshProgress {
		if p.Done {
			if candidateID == "" || p.createdAt.Before(candidateTime) {
				candidateID = id
				candidateTime = p.createdAt
			}
		}
	}
	if candidateID == "" {
		for id, p := range s.refreshProgress {
			if candidateID == "" || p.createdAt.Before(candidateTime) {
				candidateID = id
				candidateTime = p.createdAt
			}
		}
	}
	if candidateID != "" {
		delete(s.refreshProgress, candidateID)
	}
}

func (s *Server) accountRefreshProgressAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/accounts/refresh/progress/")
	id = strings.TrimPrefix(id, "/api/accounts/operations/")
	id = strings.Trim(id, "/")
	if id == "" || strings.Contains(id, "/") {
		writeError(w, http.StatusNotFound, "progress not found", "not_found")
		return
	}
	s.refreshMu.RLock()
	progress, ok := s.refreshProgress[id]
	if ok {
		copy := *progress
		copy.StatusCounts = cloneIntMap(progress.StatusCounts)
		copy.Result = cloneMap(progress.Result)
		progress = &copy
	}
	s.refreshMu.RUnlock()
	if !ok {
		writeError(w, http.StatusNotFound, "progress not found", "not_found")
		return
	}
	counts := map[string]int{
		"updated": progress.Processed,
		"errors":  progress.StatusCounts["failed"] + progress.StatusCounts["error"],
	}
	message := fmt.Sprintf("已处理 %d/%d", progress.Processed, progress.Total)
	if progress.Done {
		message = fmt.Sprintf("已完成 %d 个账号", progress.Processed)
	}
	response := map[string]any{
		"total":         progress.Total,
		"processed":     progress.Processed,
		"done":          progress.Done,
		"status_counts": progress.StatusCounts,
		"total_quota":   progress.TotalQuota,
	}
	if progress.Error != "" {
		response["error"] = progress.Error
	}
	if progress.Result != nil {
		response["result"] = progress.Result
	}
	// 轮询响应同样要带展示投影：前端 accountOperationPresentation 对它强校验，
	// 缺了就在 while 循环的第一次迭代抛错——同步与刷新 AT 因此根本进不去。
	mergeAccountMutation(response, message, counts)
	if !progress.Done {
		// 未完成的轮询既不是成功也不是失败，语气必须是 info。
		response["tone"] = "info"
		response["status_label"] = accountMutationStatusLabel("info")
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) runAccountRefresh(progressID string, refs []string) {
	ctx := context.Background()
	var wg sync.WaitGroup
	sem := make(chan struct{}, accountRefreshConcurrency())
	for _, ref := range refs {
		ref := ref
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			s.refreshOneAccount(ctx, progressID, ref)
		}()
	}
	wg.Wait()
	items, err := s.store.AccountList()
	result := map[string]any{"refreshed": 0, "errors": []any{}, "items": accountsForAPI(items)}
	if err != nil {
		s.finishAccountRefresh(progressID, nil, err.Error())
		return
	}
	s.refreshMu.Lock()
	if progress := s.refreshProgress[progressID]; progress != nil {
		if progress.Result != nil {
			result = progress.Result
		}
		result["items"] = accountsForAPI(items)
		progress.Result = result
		progress.Done = true
	}
	s.refreshMu.Unlock()
}

func (s *Server) refreshOneAccount(parent context.Context, progressID, ref string) {
	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	defer cancel()
	items, err := s.store.AccountList()
	if err != nil {
		s.recordRefreshError(progressID, ref, err)
		return
	}
	var account map[string]any
	for _, item := range items {
		if accountRefMatches(item, ref) {
			account = item
			break
		}
	}
	if account == nil {
		s.recordRefreshError(progressID, ref, errors.New("account not found"))
		return
	}
	token := accountToken(account)
	result, err := s.openAIAccountClient().RefreshAccount(ctx, account)
	if err != nil {
		if updates := accountRefreshFailureUpdates(err); len(updates) > 0 {
			_, _, _ = s.store.UpdateAccount(token, updates)
		}
		s.recordRefreshError(progressID, ref, err)
		return
	}
	updated, _, updateErr := s.store.RotateAccountTokens(token, result.AccessToken, result.RefreshToken, result.IDToken, result.Fields)
	if updateErr != nil {
		s.recordRefreshError(progressID, ref, updateErr)
		return
	}
	if updated == nil {
		s.recordRefreshError(progressID, ref, errors.New("account update returned empty result"))
		return
	}
	s.recordRefreshSuccess(progressID)
	s.updateRefreshStatus(progressID, updated)
}

func (s *Server) openAIAccountClient() *provider.OpenAIAccountClient {
	client := s.requestClient
	if client == nil {
		client = &http.Client{
			Transport: proxyruntime.NewTransport(http.DefaultTransport),
			Timeout:   45 * time.Second,
		}
	}
	return provider.NewOpenAIAccountClient(
		s.cfg.OpenAIBaseURL,
		s.cfg.OpenAIOAuthURL,
		client,
		s.proxyManager,
		provider.ClearanceConfig{URL: s.cfg.FlareSolverrURL, Enabled: s.cfg.ClearanceEnabled, Timeout: s.cfg.ClearanceTimeout},
	)
}

func (s *Server) recordRefreshSuccess(progressID string) {
	s.refreshMu.Lock()
	if progress := s.refreshProgress[progressID]; progress != nil {
		progress.Processed++
		if progress.Result == nil {
			progress.Result = map[string]any{"refreshed": 0, "errors": []any{}}
		}
		progress.Result["refreshed"] = intValue(progress.Result["refreshed"]) + 1
	}
	s.refreshMu.Unlock()
}

func (s *Server) recordRefreshError(progressID, ref string, err error) {
	s.refreshMu.Lock()
	if progress := s.refreshProgress[progressID]; progress != nil {
		progress.Processed++
		if progress.Result == nil {
			progress.Result = map[string]any{}
		}
		errorsValue, _ := progress.Result["errors"].([]any)
		errorsValue = append(errorsValue, map[string]any{"token": tokenPreview(ref), "error": safeRefreshError(err)})
		progress.Result["errors"] = errorsValue
	}
	s.refreshMu.Unlock()
	// Update status counts from the persisted account after the error marker is
	// written, so progress reflects what the next account selection sees.
	if account, lookupErr := s.accountByRef(ref); lookupErr == nil {
		s.updateRefreshStatus(progressID, account)
	}
}

func accountRefreshConcurrency() int {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv("GO_ACCOUNT_REFRESH_CONCURRENCY")))
	if err != nil || value < 1 {
		return 3
	}
	if value > 10 {
		return 10
	}
	return value
}

func (s *Server) updateRefreshStatus(progressID string, account map[string]any) {
	category := accountStatusCategory(account)
	label := map[string]string{"normal": "正常", "limited": "限流", "abnormal": "异常", "disabled": "禁用"}[category]
	if label == "" {
		label = "异常"
	}
	s.refreshMu.Lock()
	if progress := s.refreshProgress[progressID]; progress != nil {
		progress.StatusCounts[label]++
		progress.TotalQuota += intValue(account["quota"])
	}
	s.refreshMu.Unlock()
}

func (s *Server) finishAccountRefresh(progressID string, result map[string]any, message string) {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	if progress := s.refreshProgress[progressID]; progress != nil {
		progress.Done = true
		progress.Error = message
		progress.Result = result
	}
}

func (s *Server) accountByRef(ref string) (map[string]any, error) {
	items, err := s.store.AccountList()
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if accountRefMatches(item, ref) {
			return item, nil
		}
	}
	return nil, errors.New("account not found")
}

func accountRefreshFailureUpdates(err error) map[string]any {
	if err == nil {
		return nil
	}
	message := safeRefreshError(err)
	lower := strings.ToLower(message)
	now := time.Now().UTC().Format(time.RFC3339)
	if errors.Is(err, provider.ErrInvalidAccessToken) ||
		strings.Contains(lower, "unauthorized") ||
		strings.Contains(lower, "http 401") ||
		strings.Contains(lower, "invalid_grant") ||
		strings.Contains(lower, "invalid access token") ||
		strings.Contains(lower, "access token invalid") {
		return map[string]any{
			"status":                "异常",
			"last_refresh_error":    message,
			"last_refresh_error_at": now,
			"last_error_kind":       "auth_invalid",
			"last_error_status":     401,
			"status_reason_code":    "account_invalid",
		}
	}
	if strings.Contains(lower, "http 429") ||
		strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "quota") ||
		strings.Contains(lower, "额度") {
		return map[string]any{
			"status":                "限流",
			"last_refresh_error":    message,
			"last_refresh_error_at": now,
			"last_error_kind":       "quota_exhausted",
			"status_reason_code":    "image_quota_exhausted",
		}
	}
	return map[string]any{
		"last_refresh_warning":    message,
		"last_refresh_warning_at": now,
		"last_refresh_error":      nil,
		"last_refresh_error_at":   nil,
		"last_error_kind":         nil,
		"last_error_status":       nil,
		"last_error_message":      nil,
		"status_reason_code":      nil,
	}
}

func cloneIntMap(value map[string]int) map[string]int {
	copy := map[string]int{}
	for key, item := range value {
		copy[key] = item
	}
	return copy
}

func safeRefreshError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.TrimSpace(err.Error())
	if len(message) > 300 {
		message = message[:300]
	}
	return message
}
