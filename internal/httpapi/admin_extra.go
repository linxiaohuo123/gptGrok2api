// [INPUT]: accounts
// [OUTPUT]: 管理端杂项：日志查询筛选、图片任务、提示词、模型目录、代理运行时
// [POS]: 日志行的业务字段都在 detail 里，筛选必须先看顶层再看 detail。
//         凡是租约出错后调用 accountPool.Feedback 的地方，状态码必须由
//         upstreamStatus(err) 还原，不得写死 5xx——写死会把请求域错误
//         （审核拦截、参数非法）记成账号故障，累计失败并打入 Cooldown。
//         图片任务的响应形状由 imageTaskPublic 一处定义，以
//         web-vue/src/api/imageTasks.ts 的 parseImageTask 为准：它强校验
//         status 枚举、terminal 自洽、succeeded_count == results.length、
//         width/height 为正数或 null。投影必须在 imageTaskMu 锁内完成——
//         runImageTask 会并发改写同一个 task。
// [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md

package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/model"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

// The Python service stores these resources as small JSON documents. Keep the
// Go implementation deliberately file based so the endpoints remain useful
// in the same volume layout and do not need an additional database.

type imageTaskState struct {
	ID         string           `json:"id"`
	OwnerID    string           `json:"-"`
	Status     string           `json:"status"`
	Mode       string           `json:"mode"`
	Model      string           `json:"model"`
	N          int              `json:"n"`
	Size       string           `json:"size,omitempty"`
	Quality    string           `json:"quality,omitempty"`
	Prompt     string           `json:"-"`
	Images     [][]byte         `json:"-"`
	ImageNames []string         `json:"-"`
	Data       []map[string]any `json:"data,omitempty"`
	Error      string           `json:"error,omitempty"`
	CreatedAt  string           `json:"created_at"`
	UpdatedAt  string           `json:"updated_at"`
}

type editableFileTaskState struct {
	ID        string         `json:"id"`
	TaskID    string         `json:"taskId,omitempty"`
	Owner     string         `json:"owner_id"`
	Status    string         `json:"status"`
	Kind      string         `json:"kind"`
	CreatedAt string         `json:"created_at"`
	UpdatedAt string         `json:"updated_at"`
	Error     string         `json:"error,omitempty"`
	Result    map[string]any `json:"result,omitempty"`
	Prompt    string         `json:"-"`
	Images    []string       `json:"-"`
	StartedAt time.Time      `json:"-"`
}

func (s *Server) thirdPartyApps(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	cfg, _ := s.store.Config()
	apps := map[string]any{
		"infinite_canvas": map[string]any{
			"enabled": false,
			"url":     "",
		},
	}
	if configured, ok := cfg["third_party_apps"].(map[string]any); ok {
		if canvas, ok := configured["infinite_canvas"].(map[string]any); ok {
			apps["infinite_canvas"] = map[string]any{
				"enabled": boolValue(canvas["enabled"], false),
				"url":     stringValue(canvas["url"]),
			}
		}
	}
	timeoutSecs := int(s.cfg.ConsoleRequestTimeout / time.Second)
	if timeoutSecs <= 0 {
		timeoutSecs = 600
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"api_base_url":                 stringValue(cfg["base_url"]),
		"console_request_timeout_secs": timeoutSecs,
		"third_party_apps":             apps,
	})
}

func (s *Server) modelCatalog(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	chat, images := []string{}, []string{}
	for _, item := range s.catalog {
		if !item.Enabled {
			continue
		}
		switch {
		case item.Capability&model.Chat != 0:
			chat = append(chat, item.ID)
		case item.Capability&model.Image != 0 || item.Capability&model.ImageEdit != 0:
			images = append(images, item.ID)
		}
	}
	defaultChat := "auto"
	if len(chat) > 0 {
		defaultChat = chat[0]
		for _, m := range chat {
			if m == "auto" {
				defaultChat = "auto"
				break
			}
		}
	}
	defaultImage := "gpt-image-2"
	if len(images) > 0 {
		defaultImage = images[0]
		for _, m := range images {
			if m == "gpt-image-2" {
				defaultImage = "gpt-image-2"
				break
			}
		}
	}
	highRes := []string{}
	for _, m := range images {
		if m == "gpt-image-2" || strings.HasSuffix(m, "-gpt-image-2") || strings.Contains(m, "codex") {
			highRes = append(highRes, m)
		}
	}
	allModels := append(append([]string{}, chat...), images...)
	revision := fmt.Sprintf("rev-%d", len(allModels))

	writeJSON(w, http.StatusOK, map[string]any{
		"object":            "model_catalog",
		"schema_version":    1,
		"generated_at":      time.Now().UTC().Format(time.RFC3339),
		"revision":          revision,
		"chat_models":       chat,
		"image_models":      images,
		"image_edit_models": images,
		"all_models":        allModels,
		"defaults": map[string]any{
			"chat_model":  defaultChat,
			"image_model": defaultImage,
		},
		"capabilities": map[string]any{
			"image_upscale":                true,
			"high_resolution_image_models": highRes,
		},
		"source": map[string]string{
			"chat":  "config",
			"image": "config",
		},
		"openai_models_endpoint": "/v1/models",
		"models":                 []any{},
	})
}

func (s *Server) proxyRuntime(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method == http.MethodPost {
		var updates map[string]any
		if !decodeJSON(w, r, &updates) {
			return
		}
		if _, err := s.store.UpdateConfig("proxy_runtime", updates); err != nil {
			writeError(w, 500, err.Error(), "server_error")
			return
		}
		if err := s.refreshProxyRuntime(); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
			return
		}
	}
	cfg, _ := s.store.Config()
	runtime := map[string]any{}
	if value, ok := cfg["proxy_runtime"].(map[string]any); ok {
		runtime = value
	}
	writeJSON(w, http.StatusOK, map[string]any{"runtime": runtime, "status": s.proxyManager.Snapshot()})
}

func (s *Server) logsAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	limit := queryInt(r, "limit", 200, 1, 20000)
	offset := queryInt(r, "offset", 0, 0, 100000000)
	items := s.loadCallLogs()
	query := r.URL.Query()

	statusesCount := make(map[string]int)
	endpointsCount := make(map[string]int)
	modelsCount := make(map[string]int)
	accountsCount := make(map[string]int)

	statsSuccess := 0
	statsTextReview := 0
	statsFailed := 0
	statsLimited := 0
	statsImage := 0

	filtered := make([]map[string]any, 0, len(items))
	for _, item := range items {
		detail, _ := item["detail"].(map[string]any)
		if detail == nil {
			detail = map[string]any{}
		}
		status := firstNonEmpty(stringValue(detail["status"]), "success")
		displayStatus := firstNonEmpty(stringValue(detail["display_status"]), status)
		endpoint := stringValue(detail["endpoint"])
		modelName := stringValue(detail["model"])
		account := stringValue(detail["account_email"])

		if !logMatches(item, query) {
			continue
		}
		filtered = append(filtered, item)

		if displayStatus != "" {
			statusesCount[displayStatus]++
		}
		if endpoint != "" {
			endpointsCount[endpoint]++
		}
		if modelName != "" {
			modelsCount[modelName]++
		}
		if account != "" {
			accountsCount[account]++
		}

		lowerStatus := strings.ToLower(displayStatus)
		if strings.Contains(lowerStatus, "success") {
			statsSuccess++
		} else if strings.Contains(lowerStatus, "limit") {
			statsLimited++
		} else if strings.Contains(lowerStatus, "review") {
			statsTextReview++
		} else if strings.Contains(lowerStatus, "fail") || strings.Contains(lowerStatus, "err") {
			statsFailed++
		}
		if strings.Contains(endpoint, "image") {
			statsImage++
		}
	}

	// The Python endpoint returns newest entries first.
	sort.SliceStable(filtered, func(i, j int) bool { return fmt.Sprint(filtered[i]["time"]) > fmt.Sprint(filtered[j]["time"]) })
	total := len(filtered)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}

	rawSlice := filtered[offset:end]
	formattedItems := make([]map[string]any, len(rawSlice))
	for i, item := range rawSlice {
		formattedItems[i] = formatCallSummary(item)
	}

	hasMore := end < total
	writeJSON(w, http.StatusOK, map[string]any{
		"items":        formattedItems,
		"total":        total,
		"limit":        limit,
		"offset":       offset,
		"has_more":     hasMore,
		"facets_scope": "all",
		"stats_scope":  "all",
		"total_scope":  "all",
		"facets": map[string]any{
			"statuses":  statusesCount,
			"endpoints": endpointsCount,
			"models":    modelsCount,
			"accounts":  accountsCount,
		},
		"stats": map[string]any{
			"total":       total,
			"success":     statsSuccess,
			"text_review": statsTextReview,
			"failed":      statsFailed,
			"limited":     statsLimited,
			"image":       statsImage,
		},
	})
}

func statusTone(status string) string {
	s := strings.ToLower(strings.TrimSpace(status))
	switch {
	case strings.Contains(s, "success") || strings.Contains(s, "ok"):
		return "success"
	case strings.Contains(s, "limit"):
		return "warning"
	case strings.Contains(s, "fail") || strings.Contains(s, "err"):
		return "danger"
	default:
		return "info"
	}
}

func formatCallSummary(item map[string]any) map[string]any {
	detail, _ := item["detail"].(map[string]any)
	if detail == nil {
		detail = map[string]any{}
	}
	id := stringValue(item["id"])
	timeVal := stringValue(item["time"])
	typeVal := firstNonEmpty(stringValue(item["type"]), "call")
	summary := stringValue(item["summary"])

	endpoint := stringValue(detail["endpoint"])
	model := stringValue(detail["model"])
	accountEmail := stringValue(detail["account_email"])
	status := firstNonEmpty(stringValue(detail["status"]), "success")
	displayStatus := firstNonEmpty(stringValue(detail["display_status"]), status)
	outcome := firstNonEmpty(stringValue(detail["outcome"]), status)
	business := firstNonEmpty(stringValue(detail["business"]), "chat")
	if strings.Contains(endpoint, "image") {
		business = "image_generation"
	}

	previewImage := ""
	if images, ok := detail["output_images"].([]any); ok && len(images) > 0 {
		if first, ok := images[0].(map[string]any); ok {
			previewImage = stringValue(first["url"])
		}
	} else if urls, ok := detail["image_urls"].([]any); ok && len(urls) > 0 {
		previewImage = fmt.Sprint(urls[0])
	}

	parameters := stringValue(detail["parameters"])
	if parameters == "" {
		if meta, ok := detail["request_meta"].(map[string]any); ok {
			var parts []string
			if size := stringValue(meta["size"]); size != "" {
				parts = append(parts, "请求 "+size)
			}
			if quality := stringValue(meta["quality"]); quality != "" {
				parts = append(parts, "质量 "+quality)
			}
			if format := stringValue(meta["response_format"]); format != "" {
				parts = append(parts, "返回 "+format)
			}
			parameters = strings.Join(parts, " · ")
		}
	}

	resolution := stringValue(detail["actual_resolution"])
	if resolution == "" {
		if resultImages, ok := detail["result_images"].([]any); ok && len(resultImages) > 0 {
			var resolutions []string
			seen := make(map[string]bool)
			for _, imgVal := range resultImages {
				if imgMap, ok := imgVal.(map[string]any); ok {
					w := intValue(imgMap["width"])
					h := intValue(imgMap["height"])
					resStr := "未知"
					if w > 0 && h > 0 {
						resStr = fmt.Sprintf("%d×%d", w, h)
					}
					if !seen[resStr] {
						seen[resStr] = true
						resolutions = append(resolutions, resStr)
					}
				}
			}
			resolution = strings.Join(resolutions, " / ")
		}
	}

	presentation := mapValue(detail["presentation"])
	if len(presentation) == 0 {
		presentation = map[string]any{
			"request": map[string]any{
				"kind":       firstNonEmpty(stringValue(detail["kind"]), endpoint),
				"primary":    firstNonEmpty(model, endpoint),
				"secondary":  summary,
				"parameters": parameters,
			},
			"execution": map[string]any{
				"primary":   firstNonEmpty(accountEmail, "系统账号"),
				"secondary": stringValue(detail["proxy_source"]),
			},
			"status": map[string]any{
				"label": displayStatus,
				"tone":  statusTone(displayStatus),
			},
			"result": map[string]any{
				"text":        stringValue(detail["result_text"]),
				"diagnostics": stringValue(detail["error"]),
				"resolution":  resolution,
			},
			"is_failure": strings.Contains(status, "fail") || strings.Contains(displayStatus, "fail") || strings.Contains(outcome, "fail"),
		}
	}

	// 前端 logDurationDisplay 直接读 presentation.duration.text / .breakdown，
	// 而它在 systemLogRowSignature 里被调用——那是渲染路径上的函数。
	// 缺这两个字段，日志表格**每一行**都抛 TypeError，整张表渲染不出来。
	// summary_text 同样被 summaryText() 直接读取。
	durationMS := intValue(detail["duration_ms"])
	presentation["summary_text"] = firstNonEmpty(summary, stringValue(detail["result_text"]), endpoint)
	presentation["duration"] = map[string]any{
		"text":      formatDurationText(durationMS),
		"breakdown": formatDurationBreakdown(detail),
		"tone":      durationTone(durationMS),
	}

	result := make(map[string]any, len(item)+len(detail)+10)
	for k, v := range detail {
		result[k] = v
	}
	for k, v := range item {
		result[k] = v
	}
	result["id"] = id
	result["time"] = timeVal
	result["type"] = typeVal
	result["summary"] = summary
	result["business"] = business
	result["outcome"] = outcome
	result["endpoint"] = endpoint
	result["model"] = model
	result["status"] = status
	result["display_status"] = displayStatus
	result["duration_ms"] = fmt.Sprint(detail["duration_ms"])
	result["preview_image_url"] = previewImage
	result["presentation"] = presentation
	return result
}

// formatDurationText 把毫秒渲染成人读时长。前端只做展示，不做解析。
func formatDurationText(ms int) string {
	switch {
	case ms <= 0:
		return "—"
	case ms < 1000:
		return fmt.Sprintf("%dms", ms)
	case ms < 60_000:
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	default:
		return fmt.Sprintf("%dm%02ds", ms/60000, (ms%60000)/1000)
	}
}

// durationTone 给时长挑语气：超过 30 秒算慢，超过 5 秒给提示。
func durationTone(ms int) string {
	switch {
	case ms >= 30_000:
		return "danger"
	case ms >= 5_000:
		return "warning"
	case ms > 0:
		return "success"
	default:
		return "muted"
	}
}

// formatDurationBreakdown 用 timings_ms 拼一行阶段分解；没有就留空。
func formatDurationBreakdown(detail map[string]any) string {
	timings, _ := detail["timings_ms"].(map[string]any)
	if len(timings) == 0 {
		return ""
	}
	keys := make([]string, 0, len(timings))
	for key := range timings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s %s", key, formatDurationText(intValue(timings[key]))))
	}
	return strings.Join(parts, " · ")
}

func (s *Server) deleteLogs(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		IDs []string `json:"ids"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	targets := map[string]bool{}
	for _, id := range body.IDs {
		if id = strings.TrimSpace(id); id != "" {
			targets[id] = true
		}
	}
	if len(targets) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"removed": 0})
		return
	}
	s.logMu.Lock()
	defer s.logMu.Unlock()
	path := filepath.Join(s.cfg.DataDir, "logs.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, map[string]any{"removed": 0})
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	kept := make([]string, 0)
	removed := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var item map[string]any
		if json.Unmarshal([]byte(line), &item) == nil && targets[stringValue(item["id"])] {
			removed++
			continue
		}
		kept = append(kept, line)
	}
	if err := os.WriteFile(path, []byte(strings.Join(kept, "\n")+"\n"), 0600); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": removed})
}

func (s *Server) loadCallLogs() []map[string]any {
	path := filepath.Join(s.cfg.DataDir, "logs.jsonl")
	file, err := os.Open(path)
	if err != nil {
		return []map[string]any{}
	}
	defer file.Close()
	items := []map[string]any{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	for scanner.Scan() {
		var item map[string]any
		if json.Unmarshal(scanner.Bytes(), &item) == nil && item != nil {
			items = append(items, item)
		}
	}
	return items
}

// 日志行是 {id,time,type,summary,detail}：通用字段在顶层，业务字段全在 detail 里。
// 只查顶层会让 status/endpoint/model/account 这些筛选永远匹配不到（读到的是 "<nil>"）。
var logFilterFields = map[string][]string{
	"type":            {"type"},
	"status":          {"status"},
	"endpoint":        {"endpoint"},
	"model":           {"model"},
	"account":         {"account_email", "provider_account_id", "key_name"},
	"conversation_id": {"conversation_id", "call_id"},
}

func logMatches(item map[string]any, query url.Values) bool {
	detail, _ := item["detail"].(map[string]any)
	for key, fields := range logFilterFields {
		want := strings.ToLower(strings.TrimSpace(query.Get(key)))
		if want == "" {
			continue
		}
		if !strings.Contains(logFieldText(item, detail, fields), want) {
			return false
		}
	}
	if !logMatchesDateRange(query, logRecordDay(item, detail)) {
		return false
	}
	if search := strings.ToLower(strings.TrimSpace(query.Get("search"))); search != "" {
		raw, _ := json.Marshal(item)
		if !strings.Contains(strings.ToLower(string(raw)), search) {
			return false
		}
	}
	return true
}

func logFieldText(item, detail map[string]any, fields []string) string {
	parts := make([]string, 0, len(fields))
	for _, field := range fields {
		for _, source := range []map[string]any{item, detail} {
			if value := strings.TrimSpace(fmt.Sprint(source[field])); value != "" && value != "<nil>" {
				parts = append(parts, strings.ToLower(value))
			}
		}
	}
	return strings.Join(parts, " ")
}

// logRecordDay 取记录所在日期（YYYY-MM-DD）：详情里的 started_at 更准，回退到顶层 time。
func logRecordDay(item, detail map[string]any) string {
	for _, value := range []any{detail["started_at"], item["time"]} {
		if text := strings.TrimSpace(fmt.Sprint(value)); len(text) >= 10 {
			return text[:10]
		}
	}
	return ""
}

// 日期区间按天比较：前端传的是日期，记录是 RFC3339，逐字符比会把当天的记录漏掉。
func logMatchesDateRange(query url.Values, day string) bool {
	start := strings.TrimSpace(query.Get("start_date"))
	end := strings.TrimSpace(query.Get("end_date"))
	if start == "" && end == "" {
		return true
	}
	if day == "" {
		return false
	}
	if start != "" && day < start[:min(10, len(start))] {
		return false
	}
	return end == "" || day <= end[:min(10, len(end))]
}

func (s *Server) runtimeLogs(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	limit := queryInt(r, "limit", 300, 1, 2000)
	paths := []string{filepath.Join(s.cfg.DataDir, "runtime.log"), filepath.Join(s.cfg.DataDir, "app.log"), filepath.Join(s.cfg.RootDir, "logs", "runtime.log"), filepath.Join(s.cfg.RootDir, "logs", "app.log")}
	items := []map[string]any{}
	for _, path := range paths {
		items = append(items, tailRuntimeLog(path, limit)...)
		if len(items) >= limit {
			break
		}
	}
	if len(items) > limit {
		items = items[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": len(items), "limit": limit, "sources": map[string]any{"memory": false, "files": paths}})
}

func tailRuntimeLog(path string, limit int) []map[string]any {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(raw), "\n")
	result := []map[string]any{}
	for i := len(lines) - 1; i >= 0 && len(result) < limit; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		level := "info"
		upper := strings.ToUpper(line)
		for _, candidate := range []string{"DEBUG", "INFO", "WARNING", "ERROR", "CRITICAL"} {
			if strings.HasPrefix(upper, "["+candidate+"]") {
				level = strings.ToLower(candidate)
				line = strings.TrimSpace(line[len(candidate)+2:])
				break
			}
		}
		result = append(result, map[string]any{"id": fmt.Sprintf("file-%d", i), "time": "", "level": level, "message": line, "source": "file", "path": path})
	}
	return result
}

func (s *Server) prompts(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	items := s.loadPromptItems()
	sources := s.loadPromptSources()
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "prompt_count": len(items), "sources": sources, "source_count": len(sources), "synced": len(items) > 0, "cached_source_count": len(sources), "enabled_source_count": len(sources)})
}

func (s *Server) loadPromptItems() []map[string]any {
	path := filepath.Join(s.cfg.RootDir, "services", "default_prompt_library.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return []map[string]any{}
	}
	var doc struct {
		Prompts []map[string]any `json:"prompts"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return []map[string]any{}
	}
	return doc.Prompts
}

func (s *Server) loadPromptSources() []map[string]any {
	path := filepath.Join(s.cfg.DataDir, "prompt_sources.json")
	raw, err := os.ReadFile(path)
	if err == nil {
		var doc map[string]any
		if json.Unmarshal(raw, &doc) == nil {
			if list, ok := doc["sources"].([]any); ok {
				result := []map[string]any{}
				for _, value := range list {
					if item, ok := value.(map[string]any); ok {
						result = append(result, item)
					}
				}
				if len(result) > 0 {
					return result
				}
			}
		}
	}
	return []map[string]any{{"id": "banana-prompt-quicker", "name": "Banana Prompt Quicker", "url": "local://default_prompt_library.json", "adapter": "json", "enabled": true, "built_in": true, "prompt_count": len(s.loadPromptItems())}}
}

func (s *Server) promptSources(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	sources := s.loadPromptSources()
	writeJSON(w, 200, map[string]any{"sources": sources, "source_count": len(sources)})
}

func (s *Server) promptSource(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/admin/prompt-sources/"), "/")
	if id == "refresh" && r.Method == http.MethodPost {
		items := s.loadPromptItems()
		sources := s.loadPromptSources()
		writeJSON(w, 200, map[string]any{"items": items, "prompt_count": len(items), "sources": sources, "source_count": len(sources), "source_error_count": 0, "source_errors": []any{}})
		return
	}
	if strings.HasSuffix(id, "/refresh") {
		id = strings.TrimSuffix(id, "/refresh")
		if r.Method != http.MethodPost {
			writeError(w, 405, "method not allowed", "invalid_request_error")
			return
		}
		writeJSON(w, 200, map[string]any{"items": s.loadPromptItems(), "prompt_count": len(s.loadPromptItems()), "sources": s.loadPromptSources(), "source_count": len(s.loadPromptSources()), "source_error_count": 0, "source_errors": []any{}})
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	sources := s.loadPromptSources()
	found := false
	for _, source := range sources {
		if stringValue(source["id"]) == id {
			found = true
			if body.Enabled != nil {
				source["enabled"] = *body.Enabled
			}
			source["updated_at"] = time.Now().UTC().Format(time.RFC3339)
		}
	}
	if !found {
		writeError(w, 404, "prompt source not found", "not_found")
		return
	}
	path := filepath.Join(s.cfg.DataDir, "prompt_sources.json")
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	raw, _ := json.MarshalIndent(map[string]any{"sources": sources}, "", "  ")
	if err := os.WriteFile(path, append(raw, '\n'), 0600); err != nil {
		writeError(w, 500, err.Error(), "server_error")
		return
	}
	for _, source := range sources {
		if stringValue(source["id"]) == id {
			writeJSON(w, 200, map[string]any{"sources": sources, "source_count": len(sources), "source": source})
			return
		}
	}
}

func queryInt(r *http.Request, key string, fallback, min, max int) int {
	value := fallback
	if raw := r.URL.Query().Get(key); raw != "" {
		if _, err := fmt.Sscanf(raw, "%d", &value); err != nil {
			value = fallback
		}
	}
	if value < min {
		value = min
	}
	if value > max {
		value = max
	}
	return value
}

func (s *Server) retentionCleanup(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		LogRetentionDays   int `json:"log_retention_days"`
		ImageRetentionDays int `json:"image_retention_days"`
	}
	if r.Body != nil && r.ContentLength != 0 && !decodeJSON(w, r, &body) {
		return
	}
	logDays, imageDays := body.LogRetentionDays, body.ImageRetentionDays
	if logDays <= 0 {
		logDays = 30
	}
	if imageDays <= 0 {
		imageDays = 15
	}
	dryRun := strings.HasSuffix(r.URL.Path, "/preview")
	result := s.cleanupRetentionFiles(logDays, imageDays, dryRun)
	writeJSON(w, 200, result)
}

func (s *Server) cleanupRetentionFiles(logDays, imageDays int, dryRun bool) map[string]any {
	now := time.Now()
	logCount, imageCount := 0, 0
	var logBytes, imageBytes int64
	visit := func(root string, older time.Duration, remove bool) (int, int64) {
		count := 0
		var size int64
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() || now.Sub(info.ModTime()) <= older {
				return nil
			}
			count++
			size += info.Size()
			if remove {
				_ = os.Remove(path)
			}
			return nil
		})
		return count, size
	}
	logCount, logBytes = visit(filepath.Join(s.cfg.DataDir, "logs.jsonl"), time.Duration(logDays)*24*time.Hour, !dryRun)
	imageCount, imageBytes = visit(s.cfg.ImageDataDir, time.Duration(imageDays)*24*time.Hour, !dryRun)
	return map[string]any{"dry_run": dryRun, "logs": map[string]any{"removed": logCount, "removed_size_bytes": logBytes, "retention_days": logDays}, "images": map[string]any{"removed": imageCount, "removed_size_bytes": imageBytes, "retention_days": imageDays}, "total_removed": logCount + imageCount, "total_size_bytes": logBytes + imageBytes}
}

func (s *Server) accountCleanup(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		AutoRemoveInvalidAccounts     *bool `json:"auto_remove_invalid_accounts"`
		AutoRemoveRateLimitedAccounts *bool `json:"auto_remove_rate_limited_accounts"`
	}
	if r.Body != nil && r.ContentLength != 0 && !decodeJSON(w, r, &body) {
		return
	}
	removeInvalid, removeLimited := true, false
	if body.AutoRemoveInvalidAccounts != nil {
		removeInvalid = *body.AutoRemoveInvalidAccounts
	}
	if body.AutoRemoveRateLimitedAccounts != nil {
		removeLimited = *body.AutoRemoveRateLimitedAccounts
	}
	accounts, _ := s.store.AccountList()
	candidates := []string{}
	invalidCandidates := 0
	limitedCandidates := 0
	for _, account := range accounts {
		category := accountStatusCategory(account)
		// Only remove an abnormal account when the upstream has explicitly
		// rejected its credential (or marked the access token expired) and no
		// refresh token is available. Transient upstream errors must not delete
		// an otherwise recoverable account.
		invalid := removeInvalid && accountAutoRemoveInvalid(account)
		limited := removeLimited && category == "limited"
		if invalid || limited {
			candidates = append(candidates, accountToken(account))
			if invalid {
				invalidCandidates++
			}
			if limited {
				limitedCandidates++
			}
		}
	}
	dryRun := strings.HasSuffix(r.URL.Path, "/preview")
	removed := 0
	if !dryRun {
		removed, _, _ = s.store.DeleteAccounts(candidates)
	}
	total := len(candidates)
	if !dryRun {
		total = removed
	}
	writeJSON(w, 200, map[string]any{
		"dry_run":             dryRun,
		"checked":             len(accounts),
		"candidates":          len(candidates),
		"removed":             removed,
		"total_removed":       total,
		"invalid":             invalidCandidates,
		"rate_limited":        limitedCandidates,
		"remove_invalid":      removeInvalid,
		"remove_rate_limited": removeLimited,
	})
}

func (s *Server) imageTasksAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	if r.URL.Path == "/api/image-tasks/generations" && r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	switch r.Method {
	case http.MethodGet:
		owner := s.authIdentity(r)
		ids := strings.Split(r.URL.Query().Get("ids"), ",")
		items := []map[string]any{}
		missing := []string{}
		s.imageTaskMu.RLock()
		if strings.TrimSpace(r.URL.Query().Get("ids")) == "" {
			for _, task := range s.imageTasks {
				if task.OwnerID == owner {
					items = append(items, imageTaskPublic(task))
				}
			}
			sort.SliceStable(items, func(i, j int) bool { return fmt.Sprint(items[i]["updated_at"]) > fmt.Sprint(items[j]["updated_at"]) })
		} else {
			for _, id := range ids {
				id = strings.TrimSpace(id)
				if id == "" {
					continue
				}
				task, ok := s.imageTasks[id]
				if !ok || task.OwnerID != owner {
					missing = append(missing, id)
				} else {
					items = append(items, imageTaskPublic(task))
				}
			}
		}
		s.imageTaskMu.RUnlock()
		writeJSON(w, 200, map[string]any{"items": items, "missing_ids": missing, "quota_summary": s.imageQuota()})
	case http.MethodPost:
		var body struct {
			ClientTaskID string `json:"client_task_id"`
			Prompt       string `json:"prompt"`
			Model        string `json:"model"`
			N            int    `json:"n"`
			Size         string `json:"size"`
			Quality      string `json:"quality"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		if strings.TrimSpace(body.ClientTaskID) == "" || strings.TrimSpace(body.Prompt) == "" {
			writeError(w, 400, "client_task_id and prompt are required", "invalid_request_error")
			return
		}
		if err := s.checkSensitiveWords(body.Prompt); err != nil {
			writeSensitiveWordError(w)
			return
		}
		if body.Model == "" {
			body.Model = "gpt-image-2"
		}
		if body.N == 0 {
			body.N = 1
		}
		// 与前端 normalizeImageCount 的 1..4 保持一致。此前只兜 0 -> 1、没有上界，
		// 一个 n=1000 的任务会被记成 requested_count=1000，而 parseImageTask
		// 见到 >4 就整个响应解析失败——一条坏数据让整页打不开。
		if body.N < 1 || body.N > maxImageTaskCount {
			writeError(w, 400, fmt.Sprintf("n must be between 1 and %d", maxImageTaskCount), "invalid_request_error")
			return
		}
		if body.Quality == "" {
			body.Quality = "auto"
		}
		owner := s.authIdentity(r)
		task := &imageTaskState{ID: body.ClientTaskID, OwnerID: owner, Status: "queued", Mode: "generate", Model: body.Model, N: body.N, Size: body.Size, Quality: body.Quality, Prompt: body.Prompt, CreatedAt: time.Now().UTC().Format(time.RFC3339), UpdatedAt: time.Now().UTC().Format(time.RFC3339)}
		s.imageTaskMu.Lock()
		if previous := s.imageTasks[task.ID]; previous != nil && previous.OwnerID == owner {
			task = previous
		} else {
			s.imageTasks[task.ID] = task
			go s.runImageTask(task, r.Header.Get("Authorization"), r.Header.Get("X-API-Key"))
		}
		// 投影必须在锁内完成：runImageTask 已在本行之前派生，正持锁改写同一个
		// task 的 Status/Data。出锁后再遍历它，就是无同步的并发读写。
		public := imageTaskPublic(task)
		s.imageTaskMu.Unlock()
		writeJSON(w, 202, public)
	default:
		writeError(w, 405, "method not allowed", "invalid_request_error")
	}
}

func (s *Server) imageTaskByID(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/image-tasks/"), "/")
	if strings.HasSuffix(path, "/resume-poll") {
		path = strings.TrimSuffix(path, "/resume-poll")
	}
	s.imageTaskMu.RLock()
	task, ok := s.imageTasks[path]
	var public map[string]any
	owned := false
	if ok {
		// 这里曾写成 `copyValue := *task` 再出锁投影——那只是浅拷贝，
		// Data 切片与元素 map 仍与 worker 共享，等于没加锁。
		// 投影必须在锁内完成。
		owned = task.OwnerID == s.authIdentity(r)
		public = imageTaskPublic(task)
	}
	s.imageTaskMu.RUnlock()
	if !ok || !owned {
		writeError(w, 404, "image task not found", "not_found")
		return
	}
	writeJSON(w, 200, public)
}

func (s *Server) imageTaskQuota(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	writeJSON(w, 200, s.imageQuota())
}

func (s *Server) imageTaskEdits(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	if err := r.ParseMultipartForm(112 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid multipart form", "invalid_request_error")
		return
	}
	clientID := strings.TrimSpace(r.FormValue("client_task_id"))
	prompt := strings.TrimSpace(r.FormValue("prompt"))
	if clientID == "" || prompt == "" {
		writeError(w, http.StatusBadRequest, "client_task_id and prompt are required", "invalid_request_error")
		return
	}
	if err := s.checkSensitiveWords(prompt); err != nil {
		writeSensitiveWordError(w)
		return
	}
	modelName := strings.TrimSpace(r.FormValue("model"))
	if modelName == "" {
		modelName = "gpt-image-2"
	}
	files := r.MultipartForm.File["image[]"]
	if len(files) == 0 {
		files = r.MultipartForm.File["image"]
	}
	if len(files) == 0 {
		writeError(w, http.StatusBadRequest, "at least one image is required", "invalid_request_error")
		return
	}
	if len(files) > 7 {
		files = files[len(files)-7:]
	}
	images := make([][]byte, 0, len(files))
	names := make([]string, 0, len(files))
	for _, header := range files {
		file, err := header.Open()
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid image upload", "invalid_request_error")
			return
		}
		raw, readErr := io.ReadAll(io.LimitReader(file, 16<<20))
		_ = file.Close()
		if readErr != nil || len(raw) == 0 {
			writeError(w, http.StatusBadRequest, "invalid image upload", "invalid_request_error")
			return
		}
		images = append(images, raw)
		names = append(names, filepath.Base(header.Filename))
	}
	owner := s.authIdentity(r)
	now := time.Now().UTC().Format(time.RFC3339)
	task := &imageTaskState{ID: clientID, OwnerID: owner, Status: "queued", Mode: "edit", Model: modelName, N: minInt(positiveInt(r.FormValue("n"), 1), 2), Size: "1024x1024", Quality: firstNonEmpty(r.FormValue("quality"), "auto"), Prompt: prompt, Images: images, ImageNames: names, CreatedAt: now, UpdatedAt: now}
	s.imageTaskMu.Lock()
	if previous := s.imageTasks[clientID]; previous != nil && previous.OwnerID == owner {
		task = previous
	} else {
		s.imageTasks[clientID] = task
		go s.runImageTask(task, r.Header.Get("Authorization"), r.Header.Get("X-API-Key"))
	}
	// 锁内投影：runImageTask 已在本行之前派生，正持锁改写同一个 task。
	public := imageTaskPublic(task)
	s.imageTaskMu.Unlock()
	writeJSON(w, http.StatusAccepted, public)
}

func (s *Server) runImageTask(task *imageTaskState, authHeader, apiKey string) {
	var req *http.Request
	var target string
	var body io.Reader
	contentType := "application/json"
	if task.Mode == "edit" {
		var buffer bytes.Buffer
		writer := multipart.NewWriter(&buffer)
		_ = writer.WriteField("model", task.Model)
		_ = writer.WriteField("prompt", task.Prompt)
		_ = writer.WriteField("n", fmt.Sprint(task.N))
		_ = writer.WriteField("size", task.Size)
		_ = writer.WriteField("response_format", "url")
		for index, raw := range task.Images {
			name := "image.png"
			if index < len(task.ImageNames) && task.ImageNames[index] != "" {
				name = task.ImageNames[index]
			}
			part, err := writer.CreateFormFile("image[]", name)
			if err != nil {
				s.finishImageTaskError(task, err.Error())
				return
			}
			_, _ = part.Write(raw)
		}
		_ = writer.Close()
		body = bytes.NewReader(buffer.Bytes())
		contentType = writer.FormDataContentType()
		target = "http://internal/v1/images/edits"
	} else {
		raw, _ := json.Marshal(map[string]any{"model": task.Model, "prompt": task.Prompt, "n": task.N, "size": task.Size, "response_format": "url"})
		body = bytes.NewReader(raw)
		target = "http://internal/v1/images/generations"
	}
	parsed, _ := url.Parse(target)
	req = &http.Request{Method: http.MethodPost, URL: parsed, Header: make(http.Header), Body: io.NopCloser(body), ContentLength: -1}
	req.Header.Set("Content-Type", contentType)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	recorder := &responseCapture{header: make(http.Header)}
	s.imageGenerations(recorder, req)
	s.imageTaskMu.Lock()
	defer s.imageTaskMu.Unlock()
	task.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if recorder.status >= 200 && recorder.status < 300 {
		var result map[string]any
		if json.Unmarshal(recorder.body.Bytes(), &result) == nil {
			if data, ok := result["data"].([]any); ok {
				for _, value := range data {
					if item, ok := value.(map[string]any); ok {
						task.Data = append(task.Data, item)
					}
				}
			}
			task.Status = "success"
		} else {
			task.Status = "error"
			task.Error = "invalid image response"
		}
	} else {
		task.Status = "error"
		var result map[string]any
		if json.Unmarshal(recorder.body.Bytes(), &result) == nil {
			if value, ok := result["error"].(map[string]any); ok {
				task.Error = stringValue(value["message"])
			}
		}
		if task.Error == "" {
			task.Error = strings.TrimSpace(recorder.body.String())
		}
		if task.Error == "" {
			task.Error = "image generation failed"
		}
	}
}

func (s *Server) finishImageTaskError(task *imageTaskState, message string) {
	s.imageTaskMu.Lock()
	defer s.imageTaskMu.Unlock()
	task.Status = "error"
	task.Error = message
	task.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
}

// maxImageTaskCount 是 /api/image-tasks 单次可请求的张数。
//
// 前端 normalizeImageCount 把 n 钳在 1..4，且 parseImageTask 拒绝
// requested_count > 4。后端若放行更大的值，那条任务一旦进入列表，
// 整个 ImageTasksResponse 的解析都会失败——一条坏数据炸掉整页。
const maxImageTaskCount = 4

// imageTaskPublic 把内部任务投影成前端 ImageTask 契约。
//
// 字段清单以 web-vue/src/api/imageTasks.ts 为准。前端的 parseImageTask 是
// 强校验：status 必须在枚举内、terminal 必须与 status 自洽、
// succeeded_count 必须等于 results.length、width/height 为 0 直接判违约。
// 因此这里不是"多加几个字段"，而是必须逐条满足那些不变量。
func imageTaskPublic(task *imageTaskState) map[string]any {
	results := imageTaskAssets(task.Data)
	requested := task.N
	if requested < 1 {
		requested = 1
	}
	if requested > maxImageTaskCount {
		requested = maxImageTaskCount
	}
	status := imageTaskStatusForAPI(task.Status, len(results), requested)
	terminal := status != "queued" && status != "running"

	succeeded := len(results)
	failed, pending := 0, 0
	if terminal {
		// 终态：pending 必须为 0，未产出的部分计入失败。
		if failed = requested - succeeded; failed < 0 {
			failed = 0
		}
	} else {
		// 非终态：failed 必须为 0，未产出的部分计入等待。
		if pending = requested - succeeded; pending < 0 {
			pending = 0
		}
	}

	created, hasCreated := parseTaskTime(task.CreatedAt)
	updated, hasUpdated := parseTaskTime(task.UpdatedAt)
	return map[string]any{
		"id":              task.ID,
		"status":          status,
		"terminal":        terminal,
		"mode":            firstNonEmpty(task.Mode, "generate"),
		"model":           task.Model,
		"size":            task.Size,
		"quality":         task.Quality,
		"stage_code":      status,
		"stage_label":     imageTaskStageLabel(status),
		"created_at":      task.CreatedAt,
		"updated_at":      task.UpdatedAt,
		"requested_count": requested,
		"succeeded_count": succeeded,
		"failed_count":    failed,
		"pending_count":   pending,
		"duration_ms":     imageTaskDuration(created, hasCreated, updated, hasUpdated),
		"elapsed_ms":      imageTaskElapsed(created, hasCreated),
		"error_code":      imageTaskErrorCode(task),
		"public_error":    task.Error,
		"results":         results,
		"actions":         map[string]any{"resume_poll": !terminal},
	}
}

// imageTaskStatusForAPI 把内部状态映射到前端枚举。
//
// 内部用 "error"，而前端 ImageTaskStatus 只认 queued / running / success /
// partial_success / failed / text_review。直接透传 "error" 会让 parseImageTask
// 在状态校验处抛错——一个失败的任务反而让整页显示不出来。
func imageTaskStatusForAPI(status string, succeeded, requested int) string {
	switch status {
	case "queued", "running":
		return status
	case "success":
		if succeeded < requested {
			return "partial_success"
		}
		return "success"
	default:
		if succeeded > 0 {
			return "partial_success"
		}
		return "failed"
	}
}

func imageTaskStageLabel(status string) string {
	switch status {
	case "queued":
		return "排队中"
	case "running":
		return "生成中"
	case "success":
		return "已完成"
	case "partial_success":
		return "部分完成"
	default:
		return "失败"
	}
}

func imageTaskErrorCode(task *imageTaskState) string {
	if task.Status == "success" || task.Status == "" {
		return ""
	}
	return "image_generation_failed"
}

// imageTaskAssets 把内部结果投影成前端 ImageTaskAsset。
//
// 内部一条结果只带 url 或 b64_json 之一，而契约要求四个字符串字段都存在，
// 缺的补空串。width/height 必须是正整数或 null —— 0 会被前端判为非法尺寸。
func imageTaskAssets(data []map[string]any) []any {
	assets := make([]any, 0, len(data))
	for _, item := range data {
		assets = append(assets, map[string]any{
			"url":            stringValue(item["url"]),
			"path":           stringValue(item["path"]),
			"b64_json":       stringValue(item["b64_json"]),
			"revised_prompt": stringValue(item["revised_prompt"]),
			"width":          positiveOrNil(item["width"]),
			"height":         positiveOrNil(item["height"]),
		})
	}
	return assets
}

// positiveOrNil 只接受正整数，其余（缺失、0、负数、非数字）一律 null。
func positiveOrNil(value any) any {
	var number int
	switch typed := value.(type) {
	case int:
		number = typed
	case int64:
		number = int(typed)
	case float64:
		number = int(typed)
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(typed))
		if err != nil {
			return nil
		}
		number = parsed
	default:
		return nil
	}
	if number <= 0 {
		return nil
	}
	return number
}

func parseTaskTime(value string) (time.Time, bool) {
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}

// imageTaskDuration 只在终态给出耗时；进行中的任务没有确定的时长。
func imageTaskDuration(created time.Time, hasCreated bool, updated time.Time, hasUpdated bool) any {
	if !hasCreated || !hasUpdated || updated.Before(created) {
		return nil
	}
	return updated.Sub(created).Milliseconds()
}

func imageTaskElapsed(created time.Time, hasCreated bool) any {
	if !hasCreated {
		return nil
	}
	elapsed := time.Since(created).Milliseconds()
	if elapsed < 0 {
		return int64(0)
	}
	return elapsed
}

func (s *Server) imageQuota() map[string]any {
	accounts, _ := s.store.AccountList()
	active, abnormal, disabled := 0, 0, 0
	for _, account := range accounts {
		switch accountStatusCategory(account) {
		case "abnormal":
			abnormal++
		case "disabled":
			disabled++
		default:
			active++
		}
	}
	return map[string]any{"total_quota": 0, "unlimited_quota_count": 0, "unknown_quota_count": active, "active_accounts": active, "limited_accounts": 0, "abnormal_accounts": abnormal, "disabled_accounts": disabled, "providers": map[string]any{}, "available": active > 0}
}

func (s *Server) editableFileTasksAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	owner := s.authIdentity(r)
	if r.Method == http.MethodGet {
		ids := strings.Split(r.URL.Query().Get("ids"), ",")
		items := []map[string]any{}
		missing := []string{}
		s.fileTaskMu.RLock()
		if strings.TrimSpace(r.URL.Query().Get("ids")) == "" {
			for _, task := range s.fileTasks {
				if task.OwnerID() == owner {
					items = append(items, editableTaskPublic(task))
				}
			}
		} else {
			for _, id := range ids {
				id = strings.TrimSpace(id)
				if id == "" {
					continue
				}
				task, ok := s.fileTasks[editableTaskKey(owner, id)]
				if !ok || task.OwnerID() != owner {
					missing = append(missing, id)
				} else {
					items = append(items, editableTaskPublic(task))
				}
			}
		}
		s.fileTaskMu.RUnlock()
		writeJSON(w, 200, map[string]any{"items": items, "missing_ids": missing})
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		ClientTaskID string   `json:"client_task_id"`
		Prompt       string   `json:"prompt"`
		Kind         string   `json:"kind"`
		Base64Images []string `json:"base64_images"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Kind == "" {
		body.Kind = "ppt"
	}
	if body.Kind != "ppt" && body.Kind != "psd" {
		writeError(w, 400, "kind must be ppt or psd", "invalid_request_error")
		return
	}
	if strings.TrimSpace(body.Prompt) == "" {
		writeError(w, 400, "prompt is required", "invalid_request_error")
		return
	}
	if body.ClientTaskID == "" {
		body.ClientTaskID = externalID("file")
	}
	task := &editableFileTaskState{ID: body.ClientTaskID, TaskID: body.ClientTaskID, Owner: owner, Status: "queued", Kind: body.Kind, CreatedAt: time.Now().UTC().Format(time.RFC3339), UpdatedAt: time.Now().UTC().Format(time.RFC3339)}
	if body.Kind == "psd" && len(body.Base64Images) == 0 {
		writeError(w, 400, "base64_images is empty", "invalid_request_error")
		return
	}
	task.Prompt = body.Prompt
	task.Images = body.Base64Images
	s.fileTaskMu.Lock()
	key := editableTaskKey(owner, task.ID)
	if old := s.fileTasks[key]; old != nil {
		task = old
	} else {
		s.fileTasks[key] = task
		s.saveEditableFileTasksLocked()
	}
	// 锁内投影，理由同上。
	public := editableTaskPublic(task)
	s.fileTaskMu.Unlock()
	go s.runEditableFileTask(task)
	writeJSON(w, 202, public)
}

func (s *Server) editableFileTaskByID(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/editable-file-tasks/"), "/")
	s.fileTaskMu.RLock()
	task, ok := s.fileTasks[editableTaskKey(s.authIdentity(r), id)]
	var public map[string]any
	owned := false
	if ok {
		// 投影必须在锁内完成：出锁后 worker 会持锁改写同一个 task 的
		// Status / Error / Result，那时候再读就是无同步的并发访问。
		owned = task.OwnerID() == s.authIdentity(r)
		public = editableTaskPublic(task)
	}
	s.fileTaskMu.RUnlock()
	if !ok || !owned {
		writeError(w, 404, "editable file task not found", "not_found")
		return
	}
	writeJSON(w, 200, public)
}

func (s *Server) pptGenerations(w http.ResponseWriter, r *http.Request) {
	s.editableGeneration(w, r, "ppt")
}

func (s *Server) psdGenerations(w http.ResponseWriter, r *http.Request) {
	s.editableGeneration(w, r, "psd")
}

func (s *Server) editableGeneration(w http.ResponseWriter, r *http.Request, kind string) {
	if !s.requireAPI(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		ClientTaskID string   `json:"client_task_id"`
		Prompt       string   `json:"prompt"`
		Base64Images []string `json:"base64_images"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Prompt) == "" {
		writeError(w, http.StatusBadRequest, "prompt is required", "invalid_request_error")
		return
	}
	if err := s.checkSensitiveWords(body.Prompt); err != nil {
		writeSensitiveWordError(w)
		return
	}
	if kind == "psd" && len(body.Base64Images) == 0 {
		writeError(w, http.StatusBadRequest, "base64_images is empty", "invalid_request_error")
		return
	}
	if strings.TrimSpace(body.ClientTaskID) == "" {
		body.ClientTaskID = externalID("file")
	}
	owner := s.authIdentity(r)
	task := &editableFileTaskState{ID: body.ClientTaskID, TaskID: body.ClientTaskID, Owner: owner, Status: "queued", Kind: kind, Prompt: body.Prompt, Images: body.Base64Images, CreatedAt: time.Now().UTC().Format(time.RFC3339), UpdatedAt: time.Now().UTC().Format(time.RFC3339)}
	s.fileTaskMu.Lock()
	key := editableTaskKey(owner, task.ID)
	if old := s.fileTasks[key]; old != nil {
		task = old
	} else {
		s.fileTasks[key] = task
		s.saveEditableFileTasksLocked()
	}
	// 锁内投影：runEditableFileTask 一旦派生就会持锁改写同一个 task。
	public := editableTaskPublic(task)
	s.fileTaskMu.Unlock()
	go s.runEditableFileTask(task)
	writeJSON(w, http.StatusAccepted, public)
}

func (s *Server) runEditableFileTask(task *editableFileTaskState) {
	s.fileTaskMu.Lock()
	if task.Status != "queued" {
		s.fileTaskMu.Unlock()
		return
	}
	task.Status = "running"
	task.StartedAt = time.Now()
	task.UpdatedAt = task.StartedAt.UTC().Format(time.RFC3339)
	s.saveEditableFileTasksLocked()
	s.fileTaskMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	lease, err := s.accountPool.ReserveMatching(ctx, []string{"basic", "super", "heavy"}, nil, func(account accounts.Account) bool {
		if !isOpenAIAccount(account) {
			return false
		}
		plan := strings.ToLower(firstNonEmpty(stringValue(account.Fields["plan_type"]), stringValue(account.Fields["account_plan_type"]), stringValue(account.Fields["subscription_plan"])))
		return plan == "" || strings.Contains(plan, "plus") || strings.Contains(plan, "team") || strings.Contains(plan, "pro") || strings.Contains(plan, "enterprise")
	})
	if err == nil {
		defer s.accountPool.Release(lease)
	}
	inputs := []provider.OpenAIImageInput{}
	if err == nil {
		inputs, err = provider.DecodeEditableInputs(task.Images)
	}
	var exported provider.EditableExportResult
	if err == nil {
		exported, err = s.openAIImage.ExportEditable(ctx, lease.Account, task.Kind, task.Prompt, inputs)
	}
	if err == nil {
		result, saveErr := s.saveEditableExport(task, exported)
		if saveErr != nil {
			err = saveErr
		} else {
			s.fileTaskMu.Lock()
			task.Status = "success"
			task.Error = ""
			task.Result = result
			task.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
			s.saveEditableFileTasksLocked()
			s.fileTaskMu.Unlock()
			s.accountPool.Feedback(lease.Account, http.StatusOK, nil)
			return
		}
	}
	if lease != nil {
		s.accountPool.Feedback(lease.Account, upstreamStatus(err), err)
	}
	s.fileTaskMu.Lock()
	task.Status = "error"
	task.Error = firstNonEmpty(errorString(err), "editable file task failed")
	task.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	s.saveEditableFileTasksLocked()
	s.fileTaskMu.Unlock()
}

func (s *Server) saveEditableExport(task *editableFileTaskState, exported provider.EditableExportResult) (map[string]any, error) {
	ownerDigest := sha256.Sum256([]byte(task.Owner))
	taskDigest := sha256.Sum256([]byte(task.ID))
	relativeDir := filepath.ToSlash(filepath.Join(task.Kind, hex.EncodeToString(ownerDigest[:16]), hex.EncodeToString(taskDigest[:16])))
	root := filepath.Join(s.cfg.DataDir, "files")
	directory := filepath.Join(root, filepath.FromSlash(relativeDir))
	if !isWithin(root, directory) {
		return nil, fmt.Errorf("invalid editable file output path")
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, err
	}
	primaryName := safeEditableOutputName(exported.Primary.Name, "."+task.Kind)
	if task.Kind == "ppt" {
		primaryName = safeEditableOutputName(exported.Primary.Name, ".pptx")
	}
	archiveName := safeEditableOutputName(exported.Archive.Name, ".zip")
	primaryPath := filepath.Join(directory, primaryName)
	archivePath := filepath.Join(directory, archiveName)
	if err := writeEditableOutput(primaryPath, exported.Primary.Data); err != nil {
		return nil, err
	}
	if err := writeEditableOutput(archivePath, exported.Archive.Data); err != nil {
		return nil, err
	}
	primaryRelative := filepath.ToSlash(filepath.Join(relativeDir, primaryName))
	archiveRelative := filepath.ToSlash(filepath.Join(relativeDir, archiveName))
	return map[string]any{
		"conversation_id": exported.ConversationID,
		"primary_url":     editableDownloadURL(primaryRelative, s.cfg.APIKey),
		"zip_url":         editableDownloadURL(archiveRelative, s.cfg.APIKey),
		"primary_name":    primaryName,
		"zip_name":        archiveName,
	}, nil
}

func safeEditableOutputName(name, fallbackExtension string) string {
	name = strings.TrimSpace(strings.ReplaceAll(filepath.Base(name), "\x00", ""))
	if name == "" {
		name = "artifact" + fallbackExtension
	}
	if filepath.Ext(name) == "" {
		name += fallbackExtension
	}
	return name
}

func writeEditableOutput(path string, data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("editable output is empty")
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func editableDownloadURL(relative, secret string) string {
	return "/files/" + strings.ReplaceAll(url.PathEscape(relative), "%2F", "/") + "?signature=" + url.QueryEscape(editableFileSignature(secret, relative))
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (s *Server) downloadEditableFile(w http.ResponseWriter, r *http.Request) {
	relative := strings.TrimPrefix(r.URL.Path, "/files/")
	providedSignature := strings.TrimSpace(r.URL.Query().Get("signature"))
	expectedSignature := editableFileSignature(s.cfg.APIKey, relative)
	if providedSignature == "" || expectedSignature == "" || !hmac.Equal([]byte(providedSignature), []byte(expectedSignature)) {
		writeError(w, http.StatusNotFound, "file not found", "not_found")
		return
	}
	root := filepath.Join(s.cfg.DataDir, "files")
	path := filepath.Clean(filepath.Join(root, filepath.FromSlash(relative)))
	if !isWithin(root, path) {
		writeError(w, http.StatusNotFound, "file not found", "not_found")
		return
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		writeError(w, http.StatusNotFound, "file not found", "not_found")
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(path)+`"`)
	http.ServeFile(w, r, path)
}

func editableFileSignature(secret, relative string) string {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return ""
	}
	digest := hmac.New(sha256.New, []byte(secret))
	_, _ = digest.Write([]byte(relative))
	return hex.EncodeToString(digest.Sum(nil))
}

// editableTaskPublic 把内部文件任务投影成响应体。
//
// 调用方必须持 fileTaskMu —— 它读的每个字段都在被 worker 改写。
// Result 必须深拷贝：即使调用方在锁内调用本函数，writeJSON 也是在解锁之后
// 才序列化，把活 map 交出去等于没加锁。
func editableTaskPublic(task *editableFileTaskState) map[string]any {
	elapsed := 0
	if task.StartedAt.IsZero() {
		if created, err := time.Parse(time.RFC3339, task.CreatedAt); err == nil {
			elapsed = maxInt(0, int(time.Since(created).Seconds()))
		}
	} else {
		elapsed = maxInt(0, int(time.Since(task.StartedAt).Seconds()))
	}
	value := map[string]any{"id": task.ID, "taskId": task.TaskID, "status": task.Status, "kind": task.Kind, "created_at": task.CreatedAt, "updated_at": task.UpdatedAt, "elapsed_seconds": elapsed}
	if task.Error != "" {
		value["error"] = task.Error
	}
	if task.Result != nil {
		value["result"] = cloneMap(task.Result)
	}
	return value
}

func (t *editableFileTaskState) OwnerID() string { return t.Owner }

func editableTaskKey(owner, id string) string {
	return strings.TrimSpace(owner) + ":" + strings.TrimSpace(id)
}

func (s *Server) editableFileTasksPath() string {
	return filepath.Join(s.cfg.DataDir, "editable_file_tasks.json")
}

func (s *Server) loadEditableFileTasks() {
	raw, err := os.ReadFile(s.editableFileTasksPath())
	if err != nil {
		return
	}
	var envelope struct {
		Tasks []*editableFileTaskState `json:"tasks"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	changed := false
	s.fileTaskMu.Lock()
	for _, task := range envelope.Tasks {
		if task == nil || strings.TrimSpace(task.ID) == "" || strings.TrimSpace(task.Owner) == "" {
			continue
		}
		if task.TaskID == "" {
			task.TaskID = task.ID
		}
		if task.Status == "queued" || task.Status == "running" {
			task.Status = "error"
			task.Error = "服务已重启，未完成的任务已中断"
			task.UpdatedAt = now
			changed = true
		}
		s.fileTasks[editableTaskKey(task.Owner, task.ID)] = task
	}
	if changed {
		s.saveEditableFileTasksLocked()
	}
	s.fileTaskMu.Unlock()
}

func (s *Server) saveEditableFileTasksLocked() {
	items := make([]*editableFileTaskState, 0, len(s.fileTasks))
	for _, task := range s.fileTasks {
		items = append(items, task)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].UpdatedAt > items[j].UpdatedAt })
	raw, err := json.MarshalIndent(map[string]any{"tasks": items}, "", "  ")
	if err != nil {
		return
	}
	path := s.editableFileTasksPath()
	if os.MkdirAll(filepath.Dir(path), 0o755) != nil {
		return
	}
	temporary := path + ".tmp"
	if os.WriteFile(temporary, raw, 0o600) == nil {
		_ = os.Rename(temporary, path)
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (s *Server) authIdentity(r *http.Request) string {
	token := s.auth.APIKey(r)
	if identity, ok := s.auth.Identity(token); ok && identity.ID != "" {
		return identity.ID
	}
	return "anonymous"
}

func (s *Server) searchAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		Prompt string `json:"prompt"`
		Model  string `json:"model"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	prompt := strings.TrimSpace(body.Prompt)
	if prompt == "" {
		writeError(w, http.StatusBadRequest, "prompt is required", "invalid_request_error")
		return
	}
	if err := s.checkSensitiveWords(prompt); err != nil {
		writeSensitiveWordError(w)
		return
	}

	modelID := firstNonEmpty(body.Model, "auto")
	ctx, cancel := context.WithTimeout(r.Context(), 180*time.Second)
	defer cancel()

	lease, err := s.accountPool.ReserveMatching(ctx, []string{"basic", "super", "heavy"}, nil, isOpenAIAccount)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "no available account for search: "+err.Error(), "server_error")
		return
	}
	defer s.accountPool.Release(lease)

	req := protocol.ChatRequest{
		Model: modelID,
		Messages: []protocol.Message{
			{Role: "user", Content: prompt},
		},
	}
	req = s.applyGlobalSystemPrompt(req)
	text, _, err := s.openAIChat.Complete(ctx, lease.Account, req)
	if err != nil {
		// 状态码必须由 upstreamStatus 还原，不能写死 502：提示词触犯上游审核
		// 返回的是 400，属于请求域。把它记成 502 会让 pool.Feedback 走 5xx 分支，
		// 给一个健康账号累计失败、标异常并打入指数退避 Cooldown——
		// 几次违规请求就能把整池账号冷却掉。
		status := upstreamStatus(err)
		s.accountPool.Feedback(lease.Account, status, err)
		writeError(w, status, "search execution failed: "+err.Error(), "upstream_error")
		return
	}
	s.accountPool.Feedback(lease.Account, http.StatusOK, nil)

	email := stringValue(lease.Account.Fields["email"])
	writeJSON(w, http.StatusOK, map[string]any{
		"object":         "search",
		"prompt":         prompt,
		"text":           text,
		"sources":        []any{},
		"_account_email": email,
	})
}

func (s *Server) updateStatusAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	currentTag := s.cfg.Version
	if !strings.HasPrefix(currentTag, "v") {
		currentTag = "v" + currentTag
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"current_tag":      currentTag,
		"latest_tag":       currentTag,
		"update_available": false,
		"release_url":      "https://github.com/auucoder/gptgrok2api-go",
		"status_label":     "已是最新",
		"status_message":   "当前已运行最新 Go 版本",
		"tone":             "success",
		"changelog":        "Go 高性能后端稳定运行中",
		"can_update":       false,
	})
}

func (s *Server) updateTaskAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	currentTag := s.cfg.Version
	if !strings.HasPrefix(currentTag, "v") {
		currentTag = "v" + currentTag
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"task_id":      "idle",
		"state":        "idle",
		"stage":        "idle",
		"current":      0,
		"total":        100,
		"status_label": "就绪",
		"message":      "暂无进行中的升级任务",
		"tone":         "info",
		"busy":         false,
		"current_tag":  currentTag,
		"latest_tag":   currentTag,
		"error":        "",
		"updated_at":   time.Now().UTC().Format(time.RFC3339),
		"events":       []any{},
	})
}
