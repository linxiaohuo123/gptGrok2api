// [INPUT]: accounts/auth/config/model/oauth/protocol/provider/proxy/store/tasks 全部内部包
// [OUTPUT]: 路由表与组装：New、Server、withMiddleware、withRequestMonitor、cloneMap（委托 store.CloneMap）、pageBounds
// [POS]: HTTP 层的组装根：路由注册、中间件、Server 结构、通用工具函数。其余 httpapi 文件都挂在它提供的 Server 上。
// [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md

package httpapi

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/agentidentity"
	"github.com/auucoder/gptgrok2api-go/internal/auth"
	"github.com/auucoder/gptgrok2api-go/internal/config"
	"github.com/auucoder/gptgrok2api-go/internal/model"
	"github.com/auucoder/gptgrok2api-go/internal/oauth"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
	"github.com/auucoder/gptgrok2api-go/internal/store"
	"github.com/auucoder/gptgrok2api-go/internal/tasks"
)

type Server struct {
	cfg                config.Config
	auth               *auth.Validator
	store              *store.Store
	catalog            []model.Spec
	client             *http.Client
	requestClient      *http.Client
	accountPool        *accounts.Pool
	openAIImage        *provider.OpenAIImage
	openAIChat         *provider.OpenAIChat
	proxyManager       *proxyruntime.Manager
	openAILogin        *oauth.OpenAILogin
	agentIdentityStore *agentidentity.Store
	taskQueue          tasks.QueueAPI
	monitor            *runtimeMonitor
	logMu              sync.Mutex
	imageTaskMu        sync.RWMutex
	imageTasks         map[string]*imageTaskState
	imageSlots         chan struct{}
	fileTaskMu         sync.RWMutex
	fileTasks          map[string]*editableFileTaskState
	schedulerMu        sync.Mutex
	schedulerLeases    map[string]map[string]any
	external           *externalManager
	importJobMu        sync.Mutex
	refreshMu          sync.RWMutex
	refreshProgress    map[string]*accountRefreshProgress
	survivalMu         sync.RWMutex
	survivalStatus     map[string]any
	survivalRunning    bool
	survivalWake       chan struct{}
	proxyProbeURL      string
	hourlyMetrics      *HourlyMetricsStore
	startTime          time.Time
	loginMu            sync.Mutex
	loginAttempts      map[string]loginAttemptState
	chatDedupe         *chatDeduplicator
}

type loginAttemptState struct {
	count    int
	lockedTo time.Time
}

func New(cfg config.Config) *Server {
	repository := store.New(cfg.AccountsPath, cfg.AuthKeysPath, cfg.ConfigPath)
	proxyManager := proxyruntime.NewManager(cfg.ProxyURL, cfg.ProxyPool)
	groups := runtimeProxyGroups(cfg.ProxyGroups)
	proxyManager.ConfigureImageGroups(cfg.FallbackProxy, groups)
	proxyManager.SetResource(cfg.ResourceProxyURL, cfg.ResourceProxyPool)
	proxyManager.SetUpstreamsFile(cfg.ProxyUpstreamsFile)
	baseTransport := http.DefaultTransport.(*http.Transport).Clone()
	if baseTransport.MaxIdleConns < 1000 {
		baseTransport.MaxIdleConns = 1000
	}
	if baseTransport.MaxIdleConnsPerHost < 100 {
		baseTransport.MaxIdleConnsPerHost = 100
	}
	if baseTransport.IdleConnTimeout == 0 {
		baseTransport.IdleConnTimeout = 90 * time.Second
	}
	proxyTransport := proxyruntime.NewTransport(baseTransport)
	requestClient := &http.Client{Transport: proxyTransport, Timeout: cfg.RequestTimeout}
	var taskQueue tasks.QueueAPI = tasks.New(cfg.QueuePath)
	if cfg.QueueBackend == "redis" {
		redisQueue := tasks.NewRedis(cfg.RedisAddr, cfg.RedisPassword, cfg.RedisDB, "gptgrok2api")
		if err := redisQueue.Ping(); err != nil {
			log.Printf("Redis queue unavailable, using JSON queue: %v", err)
		} else {
			taskQueue = redisQueue
		}
	}
	metricsPath := filepath.Join(cfg.DataDir, "hourly_metrics.json")
	logsPath := filepath.Join(cfg.DataDir, "logs.jsonl")
	server := &Server{
		cfg:                cfg,
		auth:               auth.New(cfg.APIKey, cfg.AdminKey, cfg.AuthKeysPath, cfg.AllowAnonymous, repository),
		store:              repository,
		catalog:            model.Catalog(),
		client:             &http.Client{Timeout: 0},
		requestClient:      requestClient,
		accountPool:        accounts.New(repository),
		openAIImage:        provider.NewOpenAIImage(cfg.OpenAIBaseURL, requestClient, proxyManager, cfg.RequestTimeout),
		proxyManager:       proxyManager,
		imageTasks:         map[string]*imageTaskState{},
		imageSlots:         makeImageSlots(cfg.ImageMaxConcurrency),
		fileTasks:          map[string]*editableFileTaskState{},
		schedulerLeases:    map[string]map[string]any{},
		external:           newExternalManager(cfg.DataDir),
		refreshProgress:    map[string]*accountRefreshProgress{},
		survivalStatus:     map[string]any{"running": false, "last_started_at": "", "last_finished_at": "", "last_error": "", "last_summary": map[string]any{}, "next_run_at": ""},
		survivalWake:       make(chan struct{}, 1),
		openAILogin:        oauth.NewOpenAILogin(cfg.OpenAIAuthBaseURL, cfg.OpenAIPlatformBaseURL, cfg.OpenAILoginTokenURL, requestClient),
		agentIdentityStore: agentidentity.NewStore(cfg.DataDir, cfg.OpenAIAgentRegisterURL, requestClient),
		taskQueue:          taskQueue,
		monitor:            newRuntimeMonitor(),
		hourlyMetrics:      NewHourlyMetricsStore(metricsPath, logsPath),
		startTime:          time.Now(),
		loginAttempts:      map[string]loginAttemptState{},
		chatDedupe:         newChatDeduplicator(cfg.ChatDedupeTTL, cfg.ChatDedupeEnabled),
	}
	if server.openAIImage != nil {
		server.openAIImage.PollTimeout = cfg.ImagePollTimeout
		server.openAIImage.PollInterval = cfg.ImagePollInterval
		server.openAIImage.InitialWait = cfg.ImagePollInitialWait
	}
	server.openAIChat = provider.NewOpenAIChat(server.openAIImage)
	proxyManager.SetImageNodeResultCallback(server.persistProxyGroupRuntimeResult)
	server.accountPool.SetInvalidCallback(server.maybeAutoRemoveInvalidAccount)
	server.loadEditableFileTasks()
	if cfg.Version != "test" {
		server.taskQueue.Start(2)
		go server.imageRetentionScheduler()
		go server.openAISurvivalScheduler()
	}
	return server
}

func runtimeProxyGroups(groups []config.ProxyGroup) []proxyruntime.GroupConfig {
	result := make([]proxyruntime.GroupConfig, 0, len(groups))
	for _, group := range groups {
		item := proxyruntime.GroupConfig{ID: group.ID, Name: group.Name, Enabled: group.Enabled, Strategy: group.Strategy}
		for _, node := range group.Nodes {
			item.Nodes = append(item.Nodes, proxyruntime.NodeConfig{
				ID: node.ID, Name: node.Name, URL: node.URL, Enabled: node.Enabled,
				ImageConcurrencyLimit: node.ImageConcurrencyLimit, LastStatus: node.LastStatus, LastError: node.LastError,
				RuntimeFailures: node.RuntimeFailures, RuntimeSuccesses: node.RuntimeSuccesses, RuntimeLatencyMS: node.RuntimeLatencyMS,
			})
		}
		result = append(result, item)
	}
	return result
}

func (s *Server) refreshProxyRuntime() error {
	if s == nil || s.store == nil || s.proxyManager == nil {
		return nil
	}
	values, err := s.store.Config()
	if err != nil {
		return err
	}
	runtimeConfig := config.Config{}
	config.ApplyProxyConfig(&runtimeConfig, values)
	// Environment variables retain their normal precedence over persisted UI
	// settings, matching config.Load at process startup.
	if strings.TrimSpace(os.Getenv("GO_PROXY_URL")) != "" {
		runtimeConfig.ProxyURL = s.cfg.ProxyURL
	}
	if strings.TrimSpace(os.Getenv("GO_PROXY_POOL")) != "" {
		runtimeConfig.ProxyPool = s.cfg.ProxyPool
	}
	if strings.TrimSpace(os.Getenv("GO_RESOURCE_PROXY_URL")) != "" {
		runtimeConfig.ResourceProxyURL = s.cfg.ResourceProxyURL
	}
	if strings.TrimSpace(os.Getenv("GO_RESOURCE_PROXY_POOL")) != "" {
		runtimeConfig.ResourceProxyPool = s.cfg.ResourceProxyPool
	}
	s.proxyManager.SetDefault(runtimeConfig.ProxyURL, runtimeConfig.ProxyPool)
	s.proxyManager.SetResource(runtimeConfig.ResourceProxyURL, runtimeConfig.ResourceProxyPool)
	s.proxyManager.ConfigureImageGroups(runtimeConfig.FallbackProxy, runtimeProxyGroups(runtimeConfig.ProxyGroups))
	return nil
}

func makeImageSlots(limit int) chan struct{} {
	if limit <= 0 {
		return nil
	}
	return make(chan struct{}, limit)
}

// acquireImageSlot bounds the number of expensive browser-image flows in the
// process. Waiting here is real handler queue time, unlike provider stages that
// may overlap later in the request.
func (s *Server) acquireImageSlot(ctx context.Context, r *http.Request) (func(), error) {
	if s == nil || s.imageSlots == nil {
		return func() {}, nil
	}
	started := time.Now()
	select {
	case s.imageSlots <- struct{}{}:
		waited := time.Since(started)
		if r != nil {
			s.stageRequestMonitor(r, "handler_queue_done", 10, map[string]any{"handler_queue_ms": waited.Milliseconds()})
		}
		var once sync.Once
		return func() { once.Do(func() { <-s.imageSlots }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// maybeAutoRemoveInvalidAccount removes browser/session accounts only after a
// provider explicitly rejects their credential. OAuth accounts with a refresh
// token remain available for the normal AT refresh flow, and the setting is
// checked on every event so changes take effect without restarting the server.
func (s *Server) maybeAutoRemoveInvalidAccount(account accounts.Account) {
	if strings.TrimSpace(account.Token) == "" ||
		strings.TrimSpace(stringValue(account.Fields["refresh_token"])) != "" ||
		(strings.TrimSpace(stringValue(account.Fields["login_password"])) != "" &&
			strings.TrimSpace(firstNonEmpty(stringValue(account.Fields["two_factor_secret"]), stringValue(account.Fields["totp_secret"]))) != "") {
		return
	}
	settings, err := s.store.Config()
	if err != nil || !boolValue(settings["auto_remove_invalid_accounts"], false) {
		return
	}
	removed, _, err := s.store.DeleteAccounts([]string{account.Token})
	if err != nil {
		log.Printf("auto remove invalid account failed: %v", err)
		return
	}
	if removed > 0 {
		log.Printf("auto removed invalid account %s", accountPublicRef(account.Fields))
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.health)
	mux.HandleFunc("/v1/models", s.listModels)
	mux.HandleFunc("/v1/models/", s.getModel)
	mux.HandleFunc("/v1/chat/completions", s.chatCompletions)
	mux.HandleFunc("/v1/responses", s.responses)
	mux.HandleFunc("/v1/messages", s.messages)
	mux.HandleFunc("/v1/search", s.searchAPI)
	mux.HandleFunc("/v1/images/generations", s.imageGenerations)
	mux.HandleFunc("/v1/images/edits", s.imageEdits)
	mux.HandleFunc("/v1/files/image", s.imageFile)
	mux.HandleFunc("/upimg/v1/files/image", s.imageFile)
	mux.HandleFunc("/v1/", s.v1)
	mux.HandleFunc("/auth/login", s.login)
	mux.HandleFunc("/auth/status", s.authStatus)
	mux.HandleFunc("/version", s.version)
	mux.HandleFunc("/meta/update", s.metaUpdate)
	mux.HandleFunc("/api/system/update", s.updateTriggerAPI)
	mux.HandleFunc("/api/system/update-status", s.updateStatusAPI)
	mux.HandleFunc("/api/system/update-task", s.updateTaskAPI)
	mux.HandleFunc("/updates/VERSION", s.versionFile)
	mux.HandleFunc("/updates/CHANGELOG.md", s.changelogFile)
	mux.HandleFunc("/api/auth/users", s.userKeys)
	mux.HandleFunc("/api/auth/users/", s.userKeyByID)
	mux.HandleFunc("/api/accounts", s.accounts)
	mux.HandleFunc("/api/accounts/", s.singleAccountAPI)
	mux.HandleFunc("/api/accounts/token", s.accountToken)
	mux.HandleFunc("/api/accounts/sync", s.accountRefreshStart)
	mux.HandleFunc("/api/accounts/refresh", s.accountRefreshStart)
	mux.HandleFunc("/api/accounts/refresh-at", s.accountAccessTokenRefresh)
	mux.HandleFunc("/api/accounts/refresh-access-token", s.accountAccessTokenRefresh)
	mux.HandleFunc("/api/accounts/refresh/progress/", s.accountRefreshProgressAPI)
	mux.HandleFunc("/api/accounts/operations/", s.accountRefreshProgressAPI)
	mux.HandleFunc("/api/accounts/selection-preview", s.accountSelectionPreviewAPI)
	mux.HandleFunc("/api/accounts/oauth/start", s.accountOAuthStart)
	mux.HandleFunc("/api/accounts/oauth/finish", s.accountOAuthFinish)
	mux.HandleFunc("/api/accounts/export", s.accountExport)
	mux.HandleFunc("/api/accounts/import-api", s.importAccountsAPI)
	mux.HandleFunc("/api/accounts/agent-identities", s.agentIdentities)
	mux.HandleFunc("/api/accounts/import-cleanup", s.cleanupImportedAbnormalAccounts)
	mux.HandleFunc("/api/accounts/update", s.updateAccount)
	mux.HandleFunc("/api/accounts/batch-update", s.batchUpdateAccounts)
	mux.HandleFunc("/api/accounts/group", s.bindAccountGroup)
	mux.HandleFunc("/api/account-groups", s.accountGroups)
	mux.HandleFunc("/api/account-groups/", s.accountGroupByID)
	mux.HandleFunc("/api/settings", s.settings)
	mux.HandleFunc("/api/settings/retention-cleanup/preview", s.retentionCleanup)
	mux.HandleFunc("/api/settings/retention-cleanup/run", s.retentionCleanup)
	mux.HandleFunc("/api/settings/account-cleanup/preview", s.accountCleanup)
	mux.HandleFunc("/api/settings/account-cleanup/run", s.accountCleanup)
	mux.HandleFunc("/api/third-party-apps", s.thirdPartyApps)
	mux.HandleFunc("/api/model-catalog", s.modelCatalog)
	mux.HandleFunc("/api/logs", s.logsAPI)
	mux.HandleFunc("/api/logs/", s.logDetailAPI)
	mux.HandleFunc("/api/logs/delete", s.deleteLogs)
	mux.HandleFunc("/api/runtime-logs", s.runtimeLogs)
	mux.HandleFunc("/api/proxy/runtime", s.proxyRuntime)
	mux.HandleFunc("/api/proxy/view", s.proxyViewAPI)
	mux.HandleFunc("/api/proxy/defaults", s.proxyDefaultsAPI)
	mux.HandleFunc("/api/proxy/nodes/import", s.proxyNodesImportAPI)
	mux.HandleFunc("/api/prompts", s.prompts)
	mux.HandleFunc("/api/admin/prompt-sources", s.promptSources)
	mux.HandleFunc("/api/admin/prompt-sources/", s.promptSource)
	mux.HandleFunc("/api/image-tasks", s.imageTasksAPI)
	mux.HandleFunc("/api/image-tasks/quota", s.imageTaskQuota)
	mux.HandleFunc("/api/image-tasks/generations", s.imageTasksAPI)
	mux.HandleFunc("/api/image-tasks/edits", s.imageTaskEdits)
	mux.HandleFunc("/api/image-tasks/", s.imageTaskByID)
	mux.HandleFunc("/api/storage/info", s.storageInfo)
	mux.HandleFunc("/api/proxy/profiles", s.proxyProfiles)
	mux.HandleFunc("/api/proxy/profiles/", s.proxyProfileByID)
	mux.HandleFunc("/api/proxy/groups", s.proxyGroups)
	mux.HandleFunc("/api/proxy/groups/", s.proxyGroupByID)
	mux.HandleFunc("/api/proxy/health", s.proxyHealth)
	mux.HandleFunc("/api/proxy/test", s.proxyTest)
	mux.HandleFunc("/api/proxy/sample-test", s.proxyTest)
	mux.HandleFunc("/api/proxy/profiles/test", s.proxyTest)
	mux.HandleFunc("/api/proxy/groups/test", s.proxyGroupTest)
	mux.HandleFunc("/api/proxy/clearance/test", s.proxyTest)
	mux.HandleFunc("/api/backup/test", s.backupTest)
	mux.HandleFunc("/api/image-storage/test", s.imageStorageTest)
	mux.HandleFunc("/api/image-storage/sync", s.imageStorageSync)
	mux.HandleFunc("/api/icloud/claim-status/sync", s.iCloudClaimStatusSync)
	mux.HandleFunc("/api/images", s.adminImages)
	mux.HandleFunc("/api/images/", s.adminImages)
	mux.HandleFunc("/images/", s.publicImage)
	mux.HandleFunc("/image-thumbnails/", s.publicImage)
	mux.HandleFunc("/api/dashboard", s.dashboard)
	mux.HandleFunc("/api/monitor/realtime", s.monitorAPI)
	mux.HandleFunc("/api/monitor/realtime/", s.monitorAPI)
	mux.HandleFunc("/api/backups", s.backupsAPI)
	mux.HandleFunc("/api/backups/", s.backupsAPI)
	mux.HandleFunc("/api/accounts/survival", s.openAISurvivalAPI)
	mux.HandleFunc("/api/accounts/survival/", s.openAISurvivalAPI)
	mux.HandleFunc("/api/cpa/pools", s.cpaPoolsAPI)
	mux.HandleFunc("/api/cpa/pools/", s.cpaPoolAPI)
	mux.HandleFunc("/api/sub2api/servers", s.sub2APIServersAPI)
	mux.HandleFunc("/api/sub2api/servers/", s.sub2APIServerAPI)
	mux.HandleFunc("/api/tasks", s.taskAPI)
	mux.HandleFunc("/api/tasks/", s.taskAPI)
	mux.HandleFunc("/internal/image-monitor/", s.internalImageMonitor)
	mux.HandleFunc("/internal/image-scheduler/", s.internalImageScheduler)
	mux.HandleFunc("/internal/logs/call", s.internalCallLog)
	mux.HandleFunc("/v1/editable-file-tasks", s.editableFileTasksAPI)
	mux.HandleFunc("/v1/editable-file-tasks/", s.editableFileTaskByID)
	mux.HandleFunc("/v1/ppt/generations", s.pptGenerations)
	mux.HandleFunc("/v1/psd/generations", s.psdGenerations)
	mux.HandleFunc("/files/", s.downloadEditableFile)
	mux.HandleFunc("/api/", s.adminAPI)
	mux.HandleFunc("/admin/api/", s.adminAPI)
	mux.HandleFunc("/", s.static)
	return s.withMiddleware(mux)
}

func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		w.Header().Set("Access-Control-Allow-Methods", "*")
		w.Header().Set("Access-Control-Expose-Headers", "X-Export-Requested, X-Exported, X-Skipped")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if s.shouldMonitorRequest(r) {
			s.withRequestMonitor(w, r, next)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type statusCaptureWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

const maxJSONBodyBytes = 64 << 20

func (w *statusCaptureWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusCaptureWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.body.Len() < 8<<10 {
		remain := 8<<10 - w.body.Len()
		if len(data) > remain {
			w.body.Write(data[:remain])
		} else {
			w.body.Write(data)
		}
	}
	return w.ResponseWriter.Write(data)
}

func (w *statusCaptureWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (s *Server) shouldMonitorRequest(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	path := strings.TrimRight(r.URL.Path, "/")
	return path == "/v1/images/generations" || path == "/v1/images/edits" || path == "/v1/chat/completions" || path == "/v1/search"
}

type decodedBodyKey struct{}

func (s *Server) withRequestMonitor(w http.ResponseWriter, r *http.Request, next http.Handler) {
	// 未授权的请求马上就会被 401 拒掉，没必要先把它的 body 整读进内存：
	// 上限 64MB、还会复制两三份，匿名并发即可打爆。
	modelName, summary, requestShape, decodedBytes := monitorRequestShape(r, s.auth.ValidAPIRequest(r))
	id := newChatID()
	s.monitor.start(id, r.URL.Path, modelName, summary)
	proxySnapshot := s.proxyManager.Snapshot()
	meta := map[string]any{"model": modelName, "endpoint": r.URL.Path, "has_proxy": boolValue(proxySnapshot["proxy_configured"], false), "egress_mode": stringValue(proxySnapshot["mode"])}
	if identity, ok := s.auth.Identity(s.auth.APIKey(r)); ok {
		meta["key_id"] = identity.ID
		meta["key_name"] = identity.Name
		meta["role"] = identity.Role
	}
	if meta["has_proxy"] == true {
		meta["proxy_source"] = "default"
	} else {
		meta["proxy_source"] = "direct"
	}
	s.monitor.enrich(id, meta)
	s.monitor.update(id, "handler_started", 10, "")
	s.monitor.enrich(id, map[string]any{"metrics": map[string]any{"handler_queue_ms": 0}})
	capture := &statusCaptureWriter{ResponseWriter: w}
	ctx := context.WithValue(r.Context(), monitorCallIDKey{}, id)
	if len(decodedBytes) > 0 {
		ctx = context.WithValue(ctx, decodedBodyKey{}, decodedBytes)
	}
	next.ServeHTTP(capture, r.WithContext(ctx))
	status := capture.status
	if status == 0 {
		status = http.StatusOK
	}
	errText := monitorResponseErrorText(capture.body.Bytes(), status)
	if status >= 400 {
		s.monitor.finish(id, "failed", modelName, summary, errText)
	} else {
		s.monitor.finish(id, "success", modelName, summary, "")
	}
	if record, ok := s.monitor.detail(id); ok {
		s.appendCallLog(record, status, requestShape, capture.body.Bytes(), errText)
	}
}

type monitorCallIDKey struct{}

func (s *Server) enrichRequestMonitor(r *http.Request, meta map[string]any) {
	if r == nil {
		return
	}
	id, _ := r.Context().Value(monitorCallIDKey{}).(string)
	if id == "" {
		return
	}
	// Keep live monitor columns in sync with request metadata. Previously these
	// values were only stored under request_meta, so the active-request table
	// could not show the selected account or actual egress proxy.
	s.monitor.enrich(id, meta)
	s.monitor.mu.Lock()
	defer s.monitor.mu.Unlock()
	if item := s.monitor.active[id]; item != nil {
		if item.RequestMeta == nil {
			item.RequestMeta = map[string]any{}
		}
		for key, value := range meta {
			item.RequestMeta[key] = value
		}
		if model := stringValue(meta["model"]); model != "" {
			item.Model = model
		}
	}
}

func (s *Server) stageRequestMonitor(r *http.Request, stage string, progress int, metrics map[string]any) {
	if r == nil {
		return
	}
	id, _ := r.Context().Value(monitorCallIDKey{}).(string)
	if id == "" {
		return
	}
	s.monitor.update(id, stage, progress, "")
	s.monitor.enrich(id, map[string]any{"metrics": metrics})
}

func (s *Server) requestMonitorElapsed(r *http.Request) int64 {
	if s == nil || s.monitor == nil || r == nil {
		return 0
	}
	id, _ := r.Context().Value(monitorCallIDKey{}).(string)
	if id == "" {
		return 0
	}
	if record, ok := s.monitor.detail(id); ok && record.StartedAt > 0 {
		return time.Now().UnixMilli() - record.StartedAt
	}
	return 0
}

func (s *Server) enrichMonitorAccount(r *http.Request, account accounts.Account) {
	if r == nil {
		return
	}
	meta := map[string]any{}
	for _, key := range []string{"email", "account_email", "profile.email", "user.email"} {
		if value := strings.TrimSpace(accountFieldValue(account.Fields, key)); value != "" {
			meta["account_email"] = value
			break
		}
	}
	for _, key := range []string{"chatgpt_account_id", "chatgpt_account_user_id", "user_id", "account_id", "id"} {
		if value := strings.TrimSpace(accountFieldValue(account.Fields, key)); value != "" {
			meta["account_id"] = value
			break
		}
	}
	for _, key := range []string{"key_name", "token_name", "name"} {
		if value := strings.TrimSpace(accountFieldValue(account.Fields, key)); value != "" {
			meta["key_name"] = value
			break
		}
	}
	for _, key := range []string{"proxy", "proxy_url", "proxyUrl"} {
		proxyURL := strings.TrimSpace(accountFieldValue(account.Fields, key))
		if proxyURL == "" || strings.HasPrefix(strings.ToLower(proxyURL), "group:") {
			continue
		}
		if label := sanitizedEgressLabel(proxyURL); label != "" {
			meta["proxy_source"] = "account"
			meta["egress_label"] = label
			meta["has_proxy"] = true
		}
		break
	}
	if _, ok := meta["account_id"]; !ok {
		// The token is intentionally never logged; a stable pool label is a
		// useful last-resort account identifier for older imports.
		if pool := strings.TrimSpace(account.Pool); pool != "" {
			meta["account_id"] = pool
		}
	}
	if len(meta) > 0 {
		s.enrichRequestMonitor(r, meta)
	}
}

// accountFieldValue supports both the flat account JSON used by current
// imports and the nested profile/user objects used by older imports.
func accountFieldValue(fields map[string]any, key string) string {
	if fields == nil {
		return ""
	}
	if value := stringValue(fields[key]); value != "" {
		return value
	}
	parts := strings.Split(key, ".")
	var current any = fields
	for _, part := range parts {
		object, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = object[part]
	}
	return stringValue(current)
}

func monitorRequestShape(r *http.Request, readBody bool) (string, string, any, []byte) {
	if r == nil {
		return "", "", "", nil
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]))
	// 只看头就够了：请求即将被拒绝，body 一个字节都不该读。
	if !readBody {
		return "", "", map[string]any{"content_type": contentType}, nil
	}
	if strings.HasPrefix(contentType, "multipart/form-data") {
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			return "", "", "multipart/form-data", nil
		}
		values := r.MultipartForm.Value
		modelName := strings.TrimSpace(firstFormValue(values, "model"))
		summary := strings.TrimSpace(firstFormValue(values, "prompt"))
		if summary == "" {
			summary = strings.TrimSpace(firstFormValue(values, "input"))
		}
		if summary == "" {
			summary = strings.TrimSpace(firstFormValue(values, "message"))
		}
		if len(summary) > 180 {
			summary = summary[:180]
		}
		count := 0
		for _, key := range imageEditReferenceFields {
			count += len(r.MultipartForm.File[key]) + len(values[key])
		}
		return modelName, summary, map[string]any{
			"content_type":    "multipart/form-data",
			"image_url_parts": count,
			"data_url_images": count,
			"size":            firstFormValue(values, "size"),
			"quality":         firstFormValue(values, "quality"),
			"response_format": firstFormValue(values, "response_format"),
			"n":               firstFormValue(values, "n"),
		}, nil
	}
	if r.Body == nil || (r.ContentLength > maxJSONBodyBytes && r.ContentLength != -1) {
		return "", "", "application/json", nil
	}
	if contentType != "application/json" && contentType != "" {
		return "", "", contentType, nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxJSONBodyBytes))
	if err != nil {
		r.Body = io.NopCloser(strings.NewReader(""))
		return "", "", "application/json", nil
	}
	r.Body = io.NopCloser(strings.NewReader(string(raw)))
	decoded, ok := normalizeJSONBytes(raw, r.Header.Get("Content-Encoding"))
	if !ok {
		return "", "", "application/json", nil
	}
	r.Body = io.NopCloser(bytes.NewReader(decoded))
	var payload map[string]any
	if json.Unmarshal(decoded, &payload) != nil {
		return "", "", "application/json", nil
	}
	modelName := stringValue(payload["model"])
	summary := stringValue(payload["prompt"])
	if summary == "" {
		summary = protocol.ExtractMessage(chatMessagesFromAny(payload["messages"]))
	}
	if len(summary) > 180 {
		summary = summary[:180]
	}
	urlParts, dataURLs := imageReferenceStats(payload)
	return modelName, summary, map[string]any{
		"content_type":    "application/json",
		"image_url_parts": urlParts,
		"data_url_images": dataURLs,
		"size":            stringValue(payload["size"]),
		"quality":         stringValue(payload["quality"]),
		"response_format": stringValue(payload["response_format"]),
		"n":               payload["n"],
	}, decoded
}

func imageReferenceStats(value any) (int, int) {
	urls, data := 0, 0
	switch v := value.(type) {
	case string:
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(v)), "data:image/") {
			return 1, 1
		}
		if strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://") {
			return 1, 0
		}
	case []any:
		for _, x := range v {
			a, b := imageReferenceStats(x)
			urls += a
			data += b
		}
	case map[string]any:
		for k, x := range v {
			if k == "image_url" || k == "image_url_parts" || k == "image" || k == "images" || k == "images[]" || k == "content" || k == "messages" {
				a, b := imageReferenceStats(x)
				urls += a
				data += b
			}
		}
	}
	return urls, data
}

func monitorResponseErrorText(raw []byte, status int) string {
	if status < 400 {
		return ""
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return http.StatusText(status)
	}
	var payload map[string]any
	if json.Unmarshal(raw, &payload) == nil {
		if message := stringValue(mapValue(payload["error"])["message"]); message != "" {
			return message
		}
		if message := stringValue(payload["message"]); message != "" {
			return message
		}
	}
	if len(trimmed) > 1000 {
		return trimmed[:1000]
	}
	return trimmed
}

func chatMessagesFromAny(value any) []protocol.Message {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	messages := make([]protocol.Message, 0, len(items))
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		messages = append(messages, protocol.Message{
			Role:    stringValue(entry["role"]),
			Content: entry["content"],
		})
	}
	return messages
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":              "ok",
		"runtime":             "go",
		"version":             s.cfg.Version,
		"upstream_configured": s.cfg.UpstreamURL != "",
		"queue_backend":       s.cfg.QueueBackend,
		"proxy":               s.proxyManager.Snapshot(),
		"timestamp":           time.Now().UTC(),
	})
}

func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	data := make([]map[string]any, 0, len(s.catalog))
	for _, item := range s.catalog {
		if item.Enabled {
			data = append(data, item.Public())
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *Server) getModel(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/models/")
	item, ok := model.Find(s.catalog, id)
	if !ok {
		writeError(w, http.StatusNotFound, "model not found", "invalid_request_error")
		return
	}
	writeJSON(w, http.StatusOK, item.Public())
}

func (s *Server) v1(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	writeError(w, http.StatusNotFound, "API endpoint not found", "not_found")
}

func (s *Server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	var request protocol.ChatRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	if strings.TrimSpace(request.Model) == "" {
		writeError(w, http.StatusBadRequest, "model is required", "invalid_request_error")
		return
	}
	if len(request.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "messages cannot be empty", "invalid_request_error")
		return
	}
	for index, message := range request.Messages {
		switch strings.ToLower(strings.TrimSpace(message.Role)) {
		case "system", "developer", "user", "assistant", "tool":
		default:
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid message role at index %d", index), "invalid_request_error")
			return
		}
	}
	if err := s.checkSensitiveWords(protocol.ExtractMessage(request.Messages)); err != nil {
		writeSensitiveWordError(w)
		return
	}
	request = s.applyGlobalSystemPrompt(request)
	if request.Temperature != nil && (*request.Temperature < 0 || *request.Temperature > 2) {
		writeError(w, http.StatusBadRequest, "temperature must be between 0 and 2", "invalid_request_error")
		return
	}
	if request.TopP != nil && (*request.TopP < 0 || *request.TopP > 1) {
		writeError(w, http.StatusBadRequest, "top_p must be between 0 and 1", "invalid_request_error")
		return
	}
	route, ok := model.ResolveChat(request.Model)
	if !ok {
		writeError(w, http.StatusNotFound, "model not found", "invalid_request_error")
		return
	}
	if route.Image {
		if request.Stream {
			s.streamOpenAIImageChat(w, r, request)
		} else {
			s.completeOpenAIImageChat(w, r, request)
		}
		return
	}
	if request.Stream {
		s.streamOpenAIChat(w, r, request, route)
	} else {
		s.completeOpenAIChat(w, r, request, route)
	}
}

func (s *Server) shouldRetry(status, attempt int) bool {
	return attempt < s.cfg.ChatMaxRetries && s.cfg.ChatRetryCodes[status]
}

func writeSSE(w http.ResponseWriter, value any) {
	raw, _ := json.Marshal(value)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", raw)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func newChatID() string {
	return fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
}

func usageFor(prompt, completion, reasoning string) map[string]any {
	promptTokens := len([]rune(prompt)) / 4
	completionTokens := len([]rune(completion)) / 4
	reasoningTokens := len([]rune(reasoning)) / 4
	return map[string]any{
		"prompt_tokens":     promptTokens,
		"completion_tokens": completionTokens + reasoningTokens,
		"total_tokens":      promptTokens + completionTokens + reasoningTokens,
	}
}

func (s *Server) adminAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	writeError(w, http.StatusNotFound, "admin endpoint not found", "not_found")
}

func (s *Server) proxyUpstream(w http.ResponseWriter, r *http.Request) {
	target := s.cfg.UpstreamURL + r.URL.Path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error(), "upstream_error")
		return
	}
	for key, values := range r.Header {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	req.Header.Del("Host")
	if s.cfg.UpstreamAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.cfg.UpstreamAPIKey)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error(), "upstream_error")
		return
	}
	defer resp.Body.Close()
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		log.Printf("upstream response copy: %v", err)
	}
}

func clientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		parts := strings.Split(fwd, ",")
		if ip := strings.TrimSpace(parts[0]); ip != "" {
			return ip
		}
	}
	if realIP := strings.TrimSpace(r.Header.Get("X-Real-IP")); realIP != "" {
		return realIP
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && host != "" {
		return host
	}
	return strings.TrimSpace(r.RemoteAddr)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	ip := clientIP(r)
	now := time.Now()
	s.loginMu.Lock()
	if s.loginAttempts == nil {
		s.loginAttempts = map[string]loginAttemptState{}
	}
	if state, exists := s.loginAttempts[ip]; exists && state.lockedTo.After(now) {
		remaining := int(time.Until(state.lockedTo).Seconds())
		s.loginMu.Unlock()
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": map[string]any{
				"message": fmt.Sprintf("登录失败次数过多，请在 %d 秒后再试", remaining),
				"type":    "rate_limit_error",
			},
		})
		return
	}
	s.loginMu.Unlock()

	token := s.auth.APIKey(r)
	identity, ok := s.auth.Identity(token)
	if !ok {
		s.loginMu.Lock()
		state := s.loginAttempts[ip]
		state.count++
		if state.count >= 5 {
			state.lockedTo = now.Add(5 * time.Minute)
			state.count = 0
		}
		s.loginAttempts[ip] = state
		if len(s.loginAttempts) > 2000 {
			for k, v := range s.loginAttempts {
				if v.lockedTo.Before(now) {
					delete(s.loginAttempts, k)
				}
			}
		}
		s.loginMu.Unlock()

		writeError(w, http.StatusUnauthorized, "invalid authentication token", "authentication_error")
		return
	}

	s.loginMu.Lock()
	delete(s.loginAttempts, ip)
	s.loginMu.Unlock()

	writeJSON(w, http.StatusOK, s.authViewResponse(true, identity))
}

func (s *Server) authViewResponse(authenticated bool, identity store.Identity) map[string]any {
	if !authenticated {
		return map[string]any{
			"ok":             false,
			"authenticated":  false,
			"schema_version": 1,
			"version":        s.cfg.Version,
			"subject":        nil,
			"capabilities": map[string]any{
				"admin_console": false,
				"studio":        false,
			},
			"home_route": "/login",
		}
	}
	role := strings.ToLower(strings.TrimSpace(identity.Role))
	if role != "admin" && role != "user" {
		role = "unknown"
	}
	isAdmin := role == "admin"
	homeRoute := "/studio"
	if isAdmin {
		homeRoute = "/"
	}
	subjectID := strings.TrimSpace(identity.ID)
	if subjectID == "" {
		subjectID = "authenticated"
	}
	subjectName := strings.TrimSpace(identity.Name)
	if subjectName == "" {
		subjectName = subjectID
	}
	return map[string]any{
		"ok":             true,
		"authenticated":  true,
		"schema_version": 1,
		"runtime":        "go",
		"version":        s.cfg.Version,
		"role":           role,
		"subject_id":     subjectID,
		"name":           subjectName,
		"subject": map[string]any{
			"id":   subjectID,
			"name": subjectName,
			"role": role,
		},
		"capabilities": map[string]any{
			"admin_console": isAdmin,
			"studio":        true,
		},
		"home_route": homeRoute,
	}
}

func (s *Server) authStatus(w http.ResponseWriter, r *http.Request) {
	token := s.auth.APIKey(r)
	identity, ok := s.auth.Identity(token)
	writeJSON(w, http.StatusOK, s.authViewResponse(ok, identity))
}

func (s *Server) userKeys(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		items, err := s.store.ListPublicKeys("user")
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	case http.MethodPost:
		var body struct {
			Name string `json:"name"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		item, rawKey, err := s.store.CreateKey("user", body.Name, firstNonEmpty(s.cfg.AdminKey, s.cfg.APIKey))
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
			return
		}
		items, _ := s.store.ListPublicKeys("user")
		writeJSON(w, http.StatusOK, map[string]any{"item": item, "key": rawKey, "items": items})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
	}
}

func (s *Server) userKeyByID(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/auth/users/")
	if id == "" || strings.Contains(id, "/") {
		writeError(w, http.StatusNotFound, "user key not found", "not_found")
		return
	}
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		var updates map[string]any
		if !decodeJSON(w, r, &updates) {
			return
		}
		filtered := map[string]any{}
		for _, key := range []string{"name", "enabled", "key"} {
			if value, ok := updates[key]; ok {
				filtered[key] = value
			}
		}
		if len(filtered) == 0 {
			writeError(w, http.StatusBadRequest, "no updates provided", "invalid_request_error")
			return
		}
		item, err := s.store.UpdateKey(id, "user", filtered, s.cfg.AdminKey)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				writeError(w, http.StatusNotFound, "user key not found", "not_found")
			} else {
				writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
			}
			return
		}
		items, _ := s.store.ListPublicKeys("user")
		writeJSON(w, http.StatusOK, map[string]any{"item": item, "items": items})
	case http.MethodDelete:
		if err := s.store.DeleteKey(id, "user"); err != nil {
			writeError(w, http.StatusNotFound, "user key not found", "not_found")
			return
		}
		items, _ := s.store.ListPublicKeys("user")
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
	}
}

func (s *Server) accounts(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.listAccounts(w, r)
	case http.MethodPost:
		s.addAccounts(w, r)
	case http.MethodDelete:
		s.deleteAccounts(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
	}
}

func (s *Server) accountToken(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		AccessToken string `json:"access_token"`
		AccountRef  string `json:"account_ref"`
		ID          string `json:"id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	ref := firstNonEmpty(body.AccountRef, body.ID, body.AccessToken)
	if strings.TrimSpace(ref) == "" {
		writeError(w, http.StatusBadRequest, "account_ref is required", "invalid_request_error")
		return
	}
	items, err := s.store.AccountList()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	tokens, _ := resolveAccountRefTokens(items, []string{ref})
	if len(tokens) == 0 || strings.TrimSpace(tokens[0]) == "" {
		writeError(w, http.StatusNotFound, "account not found", "not_found")
		return
	}
	var account map[string]any
	for _, item := range items {
		if accountToken(item) == tokens[0] {
			account = item
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":      tokens[0],
		"account_ref":       accountRefForToken(items, tokens[0]),
		"token_preview":     tokenPreview(tokens[0]),
		"has_access_token":  true,
		"user_id":           stringValue(account["user_id"]),
		"email":             stringValue(account["email"]),
		"login_password":    firstNonEmpty(stringValue(account["login_password"]), stringValue(account["password"])),
		"two_factor_secret": firstNonEmpty(stringValue(account["two_factor_secret"]), stringValue(account["totp_secret"])),
	})
}

func (s *Server) listAccounts(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.AccountList()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	query := r.URL.Query()
	page := positiveInt(query.Get("page"), 1)
	pageSize := positiveInt(query.Get("page_size"), 500)
	if pageSize > 500 {
		pageSize = 500
	}
	keyword := strings.ToLower(strings.TrimSpace(query.Get("keyword")))
	status := strings.ToLower(strings.TrimSpace(query.Get("status")))
	groupID := strings.TrimSpace(query.Get("group_id"))
	filtered := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if keyword != "" && !accountContains(item, keyword) {
			continue
		}
		if !accountStatusMatches(item, status) {
			continue
		}
		if !accountGroupMatches(item, groupID) {
			continue
		}
		filtered = append(filtered, accountForAPI(item))
	}
	start, end := pageBounds(page, pageSize, len(filtered))
	writeJSON(w, http.StatusOK, map[string]any{
		"items":     filtered[start:end],
		"accounts":  filtered[start:end],
		"total":     len(filtered),
		"all_total": len(items),
		"page":      page,
		"page_size": pageSize,
	})
}

func (s *Server) addAccounts(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Tokens      []string         `json:"tokens"`
		Accounts    []map[string]any `json:"accounts"`
		Refresh     *bool            `json:"refresh"`
		ReturnItems *bool            `json:"return_items"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	added, skipped, items, err := s.store.AddAccounts(body.Tokens, body.Accounts)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	returnItems := body.ReturnItems == nil || *body.ReturnItems
	response := map[string]any{
		"added":     added,
		"skipped":   skipped,
		"refreshed": 0,
		"errors":    []string{},
	}
	if returnItems {
		response["items"] = accountsForAPI(items)
	}
	// 前端 accountOperationPresentation 强校验这组投影。缺失时抛出点位于返回
	// 对象字面量处——账号其实已经入库，用户看到的却是"导入失败"。
	mergeAccountMutation(response, fmt.Sprintf("新增 %d 个，跳过 %d 个", added, skipped), map[string]int{
		"added":   added,
		"skipped": skipped,
	})
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) cleanupImportedAbnormalAccounts(w http.ResponseWriter, r *http.Request) {
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
		Remove       bool     `json:"remove"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	refs := uniqueAccountRefs(append(append([]string{}, body.AccessTokens...), body.refs()...))
	if len(refs) == 0 {
		writeError(w, http.StatusBadRequest, "access_tokens or account_ids is required", "invalid_request_error")
		return
	}
	items, err := s.store.AccountList()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	tokens, missing := resolveAccountRefTokens(items, refs)
	targets := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		targets[token] = struct{}{}
	}
	abnormalTokens := make([]string, 0, len(tokens))
	seen := make(map[string]struct{}, len(tokens))
	for _, item := range items {
		token := accountToken(item)
		if _, ok := targets[token]; !ok || accountStatusCategory(item) != "abnormal" {
			continue
		}
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}
		abnormalTokens = append(abnormalTokens, token)
	}
	removed := 0
	if body.Remove && len(abnormalTokens) > 0 {
		removed, _, err = s.store.DeleteAccounts(abnormalTokens)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"checked":  len(refs),
		"abnormal": len(abnormalTokens),
		"removed":  removed,
		"errors":   missing,
	})
}

func (s *Server) deleteAccounts(w http.ResponseWriter, r *http.Request) {
	var body struct {
		accountSelectionBody
		Tokens []string `json:"tokens"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	// 前端发的是 account_ids / selection，不是 tokens。
	refs := uniqueAccountRefs(append(append([]string{}, body.Tokens...), body.refs()...))
	if len(refs) == 0 {
		writeError(w, http.StatusBadRequest, "tokens or account_ids is required", "invalid_request_error")
		return
	}
	items, err := s.store.AccountList()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	tokens, missing := resolveAccountRefTokens(items, refs)
	removed, items, err := s.store.DeleteAccounts(tokens)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"removed": removed,
		"errors":  missing,
		"items":   accountsForAPI(items),
	})
}

func (s *Server) updateAccount(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var body map[string]any
	if !decodeJSON(w, r, &body) {
		return
	}
	// 前端用 id 标识目标账号：改配额或分组时根本不带 access_token（它是可选字段），
	// 而这里历来只认 access_token。两者都接受——否则"编辑账号"一律 400，
	// 用户看到"保存失败"，实际上什么都没保存。
	// resolveAccountRefTokens 认得 token / id / account_ref / email 等各种引用。
	// 前端用 id 标识目标账号：改配额或分组时根本不带 access_token（它是可选字段），
	// 而这里历来只认 access_token。两者都接受——否则"编辑账号"一律 400，
	// 用户看到"保存失败"，实际上什么都没保存。
	// resolveAccountRefTokens 认得 token / id / account_ref / email 等各种引用。
	ref := firstNonEmpty(
		stringValue(body["access_token"]),
		stringValue(body["id"]),
		stringValue(body["account_ref"]),
		stringValue(body["account_id"]),
	)
	if ref == "" {
		writeError(w, http.StatusBadRequest, "id or access_token is required", "invalid_request_error")
		return
	}
	current, err := s.store.AccountList()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	tokens, _ := resolveAccountRefTokens(current, []string{ref})
	if len(tokens) == 0 {
		writeError(w, http.StatusNotFound, "account not found", "not_found")
		return
	}
	token := tokens[0]
	updates := map[string]any{}
	for _, key := range []string{"type", "source_type", "status", "quota", "proxy", "group_id", "enabled", "email", "user_id", "login_password", "two_factor_secret"} {
		if value, ok := body[key]; ok {
			updates[key] = value
		}
	}
	if len(updates) == 0 {
		writeError(w, http.StatusBadRequest, "no updates provided", "invalid_request_error")
		return
	}
	item, items, err := s.store.UpdateAccount(token, updates)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "account not found", "not_found")
		} else {
			writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		}
		return
	}
	response := map[string]any{"item": accountForAPI(item), "items": accountsForAPI(items)}
	mergeAccountMutation(response, "账号已更新", map[string]int{"updated": 1})
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) batchUpdateAccounts(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var body struct {
		accountSelectionBody
		AccessTokens []string `json:"access_tokens"`
		Status       string   `json:"status"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	// 前端发的是 account_ids / selection，不是 access_tokens。
	refs := uniqueAccountRefs(append(append([]string{}, body.AccessTokens...), body.refs()...))
	if len(refs) == 0 || strings.TrimSpace(body.Status) == "" {
		writeError(w, http.StatusBadRequest, "access_tokens or account_ids, and status, are required", "invalid_request_error")
		return
	}
	current, err := s.store.AccountList()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	tokens, missing := resolveAccountRefTokens(current, refs)
	updated := 0
	errItems := make([]string, 0, len(missing))
	for _, ref := range missing {
		errItems = append(errItems, tokenPreview(ref)+"... not found")
	}
	var all []map[string]any
	for _, token := range tokens {
		item, items, err := s.store.UpdateAccount(token, map[string]any{"status": body.Status})
		if err != nil {
			errItems = append(errItems, tokenPreview(token)+"... not found")
			continue
		}
		_ = item
		all = items
		updated++
	}
	if all == nil {
		all, _ = s.store.AccountList()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"updated": updated,
		"errors":  errItems,
		"items":   accountsForAPI(all),
	})
}

func (s *Server) bindAccountGroup(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var body struct {
		accountSelectionBody
		AccessTokens []string `json:"access_tokens"`
		GroupID      string   `json:"group_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	groupID := strings.TrimSpace(body.GroupID)
	if groupID == "__ungrouped__" {
		groupID = ""
	}
	// 这里原本既不认 account_ids、也不检查空值：前端发 account_ids 时
	// refs 解析为空，循环一次不跑，接口却返回 200 —— 一个纯粹的静默 no-op。
	refs := uniqueAccountRefs(append(append([]string{}, body.AccessTokens...), body.refs()...))
	if len(refs) == 0 {
		writeError(w, http.StatusBadRequest, "access_tokens or account_ids is required", "invalid_request_error")
		return
	}
	updated := 0
	current, err := s.store.AccountList()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	tokens, missing := resolveAccountRefTokens(current, refs)
	errItems := make([]string, 0, len(missing))
	for _, ref := range missing {
		errItems = append(errItems, tokenPreview(ref)+"... not found")
	}
	var all []map[string]any
	for _, token := range tokens {
		item, items, err := s.store.UpdateAccount(token, map[string]any{"group_id": groupID})
		if err != nil {
			errItems = append(errItems, tokenPreview(token)+"... not found")
			continue
		}
		_ = item
		all = items
		updated++
	}
	if all == nil {
		all, _ = s.store.AccountList()
	}
	response := map[string]any{
		"updated":  updated,
		"errors":   errItems,
		"group_id": groupID,
		"items":    accountsForAPI(all),
	}
	mergeAccountMutation(response, fmt.Sprintf("已绑定 %d 个账号", updated), map[string]int{
		"updated": updated,
		"errors":  len(errItems),
	})
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) accountGroups(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.accountGroupsPayload())
	case http.MethodPost:
		var body map[string]any
		if !decodeJSON(w, r, &body) {
			return
		}
		id := slugID(stringValue(body["id"]))
		if id == "" {
			id = slugID(stringValue(body["name"]))
		}
		if id == "" {
			writeError(w, http.StatusBadRequest, "account group id is required", "invalid_request_error")
			return
		}
		item := map[string]any{
			"id":      id,
			"name":    firstNonEmpty(stringValue(body["name"]), id),
			"proxy":   stringValue(body["proxy"]),
			"enabled": boolValue(body["enabled"], true),
			"notes":   stringValue(body["notes"]),
		}
		cfg, err := s.store.Config()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
			return
		}
		groups := mapList(cfg["account_groups"])
		next := make([]map[string]any, 0, len(groups)+1)
		for _, group := range groups {
			if slugID(stringValue(group["id"])) != id {
				next = append(next, group)
			}
		}
		next = append(next, item)
		updated, err := s.store.UpdateConfig("account_groups", next)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"group": item, "groups": accountGroupsFromConfig(updated, s.accountCountByGroup())})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
	}
}

func (s *Server) accountGroupByID(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	id := slugID(strings.TrimPrefix(r.URL.Path, "/api/account-groups/"))
	cfg, err := s.store.Config()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	groups := mapList(cfg["account_groups"])
	next := make([]map[string]any, 0, len(groups))
	found := false
	for _, group := range groups {
		if slugID(stringValue(group["id"])) == id {
			found = true
			continue
		}
		next = append(next, group)
	}
	if !found {
		writeError(w, http.StatusNotFound, "account group not found", "not_found")
		return
	}
	updated, err := s.store.UpdateConfig("account_groups", next)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	accounts, _ := s.store.AccountList()
	for _, account := range accounts {
		if stringValue(account["group_id"]) == id {
			_, _, _ = s.store.UpdateAccount(accountToken(account), map[string]any{"group_id": ""})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted": id,
		"groups":  accountGroupsFromConfig(updated, s.accountCountByGroup()),
	})
}

func (s *Server) version(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": s.cfg.Version})
}

func (s *Server) versionFile(w http.ResponseWriter, _ *http.Request) {
	serveTextFile(w, s.cfg.RelativePath("VERSION"), "text/plain; charset=utf-8")
}

func (s *Server) changelogFile(w http.ResponseWriter, _ *http.Request) {
	serveTextFile(w, s.cfg.RelativePath("CHANGELOG.md"), "text/markdown; charset=utf-8")
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		configValue, err := s.store.Config()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"schema_version": 1,
			"generated_at":   time.Now().UTC().Format(time.RFC3339),
			"revision":       settingsRevision(configValue),
			"settings":       cleanSettingsView(configValue),
			"fields":         map[string]any{},
			"runtime":        "go",
			"config":         configValue,
		})
	case http.MethodPost, http.MethodPatch:
		var updates map[string]any
		if !decodeJSON(w, r, &updates) {
			return
		}
		current, err := s.store.Config()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
			return
		}
		changedKeys := make([]string, 0, len(updates))
		for key, value := range updates {
			if key == "revision" {
				continue
			}
			changedKeys = append(changedKeys, key)
			if srcMap, ok := value.(map[string]any); ok {
				if dstMap, ok := current[key].(map[string]any); ok {
					for subKey, subVal := range srcMap {
						dstMap[subKey] = subVal
					}
					current[key] = dstMap
					continue
				}
			}
			current[key] = value
		}
		if err := s.store.ReplaceConfig(current); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
			return
		}
		if err := s.refreshProxyRuntime(); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
			return
		}
		if val := intValue(current["image_poll_timeout_secs"]); val > 0 {
			s.cfg.ImagePollTimeout = time.Duration(val) * time.Second
			if s.openAIImage != nil {
				s.openAIImage.PollTimeout = s.cfg.ImagePollTimeout
			}
		}
		if val := intValue(current["image_poll_interval_secs"]); val > 0 {
			s.cfg.ImagePollInterval = time.Duration(val) * time.Second
			if s.openAIImage != nil {
				s.openAIImage.PollInterval = s.cfg.ImagePollInterval
			}
		}
		if current["image_poll_initial_wait_secs"] != nil {
			val := intValue(current["image_poll_initial_wait_secs"])
			if val >= 0 {
				s.cfg.ImagePollInitialWait = time.Duration(val) * time.Second
				if s.openAIImage != nil {
					s.openAIImage.InitialWait = s.cfg.ImagePollInitialWait
				}
			}
		}
		if val := intValue(current["console_request_timeout_secs"]); val > 0 {
			s.cfg.ConsoleRequestTimeout = time.Duration(val) * time.Second
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"schema_version":   1,
			"generated_at":     time.Now().UTC().Format(time.RFC3339),
			"revision":         settingsRevision(current),
			"settings":         cleanSettingsView(current),
			"fields":           map[string]any{},
			"changed_fields":   changedKeys,
			"restart_required": false,
			"runtime":          "go",
			"config":           current,
		})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
	}
}

func (s *Server) storageInfo(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"backend":        "json",
		"data_dir":       s.cfg.DataDir,
		"accounts_path":  s.cfg.AccountsPath,
		"auth_keys_path": s.cfg.AuthKeysPath,
		"config_path":    s.cfg.ConfigPath,
	})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	if decoded, ok := r.Context().Value(decodedBodyKey{}).([]byte); ok && len(decoded) > 0 {
		if err := json.Unmarshal(decoded, target); err != nil {
			var wrapped string
			if json.Unmarshal(decoded, &wrapped) == nil {
				if inner := strings.TrimSpace(wrapped); inner != "" && json.Unmarshal([]byte(inner), target) == nil {
					return true
				}
			}
			writeError(w, http.StatusBadRequest, describeJSONBodyError(err), "invalid_request_error")
			return false
		}
		return true
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			writeError(w, http.StatusRequestEntityTooLarge, "JSON body exceeds 64MB limit", "invalid_request_error")
		} else {
			writeError(w, http.StatusBadRequest, "invalid JSON body: request body could not be read", "invalid_request_error")
		}
		return false
	}
	decoded, ok := normalizeJSONBytes(raw, r.Header.Get("Content-Encoding"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid JSON body: body is empty, truncated, or has invalid content encoding", "invalid_request_error")
		return false
	}
	if err := json.Unmarshal(decoded, target); err != nil {
		// A few reverse proxies forward an already JSON-encoded body as a
		// quoted string (for example, {\"name\":\"demo\"}). Accept that
		// representation for object payloads before rejecting the request.
		var wrapped string
		if json.Unmarshal(decoded, &wrapped) == nil {
			if inner := strings.TrimSpace(wrapped); inner != "" && json.Unmarshal([]byte(inner), target) == nil {
				return true
			}
		}
		writeError(w, http.StatusBadRequest, describeJSONBodyError(err), "invalid_request_error")
		return false
	}
	return true
}

func describeJSONBodyError(err error) string {
	if err == nil {
		return "invalid JSON body"
	}
	var syntaxError *json.SyntaxError
	if errors.As(err, &syntaxError) {
		if strings.Contains(strings.ToLower(err.Error()), "unexpected end") {
			return fmt.Sprintf("invalid JSON body: request body is truncated near byte %d", syntaxError.Offset)
		}
		return fmt.Sprintf("invalid JSON body: malformed JSON near byte %d", syntaxError.Offset)
	}
	var typeError *json.UnmarshalTypeError
	if errors.As(err, &typeError) {
		return fmt.Sprintf("invalid JSON body: field %q has the wrong value type", typeError.Field)
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || strings.Contains(strings.ToLower(err.Error()), "unexpected end") {
		return "invalid JSON body: request body is truncated"
	}
	return "invalid JSON body: malformed JSON"
}

// normalizeJSONBytes accepts the encodings emitted by common API gateways.
// Some clients prepend a UTF-8 BOM and some compress JSON request bodies.
func normalizeJSONBytes(raw []byte, contentEncoding string) ([]byte, bool) {
	encoding := strings.ToLower(strings.TrimSpace(strings.Split(contentEncoding, ",")[0]))
	// A few reverse proxies strip Content-Encoding while forwarding the body.
	// The gzip magic bytes let us still recognize and decode those requests.
	isGzip := len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b
	if encoding == "gzip" || encoding == "x-gzip" || isGzip {
		reader, err := gzip.NewReader(bytes.NewReader(raw))
		if err == nil {
			decompressed, readErr := io.ReadAll(io.LimitReader(reader, maxJSONBodyBytes))
			_ = reader.Close()
			if readErr != nil {
				return nil, false
			}
			raw = decompressed
		} else if isGzip {
			// A malformed gzip body must not be treated as JSON. If only the
			// header was retained by a proxy, however, the body is already plain.
			return nil, false
		}
	}
	raw = bytes.TrimSpace(raw)
	raw = bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, false
	}
	return raw, true
}

func positiveInt(raw string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value < 1 {
		return fallback
	}
	return value
}

// pageBounds 把外部传入的页码换算成绝对安全的切片边界。
//
// 越界判定用除法而非乘法：(page-1)*pageSize 在 page 取 int64 极大值时
// 会回绕成负数，而负数既躲过 "start > len" 的钳制，又能让 "end > len"
// 同样失效，最终以 slice bounds out of range 的形式 panic 掉整个请求。
// 先做 page-1 > total/pageSize 的比较，乘积便不可能溢出。
//
// 返回值恒满足 0 <= start <= end <= total，调用方无需再判。
func pageBounds(page, pageSize, total int) (int, int) {
	if page < 1 || pageSize < 1 || total <= 0 {
		return 0, 0
	}
	if page-1 > total/pageSize {
		return total, total
	}
	start := (page - 1) * pageSize
	end := start + pageSize
	if end > total {
		end = total
	}
	return start, end
}

func accountForAPI(account map[string]any) map[string]any {
	item := cloneMap(account)
	ref := accountPublicRef(account)
	if ref != "" {
		item["id"] = ref
		item["account_ref"] = ref
	}
	if token := accountToken(account); token != "" {
		item["has_access_token"] = true
		item["token_preview"] = tokenPreview(token)
	} else {
		item["has_access_token"] = false
		item["token_preview"] = ""
	}
	for _, key := range []string{"access_token", "accessToken", "token", "cookie_header", "session_token", "refresh_token", "id_token", "login_password", "password", "two_factor_secret", "totp_secret"} {
		delete(item, key)
	}
	category := accountStatusCategory(item)
	item["status_category"] = category
	item["status_label"] = map[string]string{
		"normal":   "正常",
		"limited":  "限流",
		"abnormal": "异常",
		"disabled": "禁用",
	}[category]
	return item
}

func accountsForAPI(items []map[string]any) []map[string]any {
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		result = append(result, accountForAPI(item))
	}
	return result
}

func accountStatusCategory(account map[string]any) string {
	status := strings.ToLower(strings.TrimSpace(stringValue(account["status"])))
	reason := strings.ToLower(strings.TrimSpace(stringValue(account["status_reason_code"])))
	errorKind := strings.ToLower(strings.TrimSpace(stringValue(account["last_error_kind"])))
	if !boolValue(account["enabled"], true) || status == "disabled" || status == "auto_disabled" || status == "禁用" || reason == "disabled" {
		return "disabled"
	}
	if status == "limited" || status == "rate_limited" || status == "cooling" || status == "backoff" || status == "限流" {
		return "limited"
	}
	switch reason {
	case "pro_cooldown", "lane_backoff", "lane_degraded", "image_generation_unavailable", "image_quota_exhausted", "text_pending":
		return "limited"
	}
	switch errorKind {
	case "quota_exhausted", "media_pending", "media_generation_unavailable", "media_degraded", "lane_degraded", "text_pending":
		return "limited"
	case "auth_invalid", "parse_failure":
		return "abnormal"
	}
	if status == "abnormal" || status == "invalid" || status == "error" || status == "incomplete" || status == "异常" {
		if intValue(account["quota"]) > 0 || boolValue(account["survival_alive"], false) {
			return "normal"
		}
		return "abnormal"
	}
	return "normal"
}

// accountAutoRemoveInvalid reports whether an abnormal account is definitely
// unrecoverable by the automated token refresh path. An account with a
// refresh token is retained so it can still be rotated, while browser/session
// accounts with an expired or explicitly rejected access token are removable.
func accountAutoRemoveInvalid(account map[string]any) bool {
	if accountStatusCategory(account) != "abnormal" {
		return false
	}
	if strings.TrimSpace(stringValue(account["refresh_token"])) != "" {
		return false
	}
	if strings.TrimSpace(stringValue(account["login_password"])) != "" &&
		strings.TrimSpace(firstNonEmpty(stringValue(account["two_factor_secret"]), stringValue(account["totp_secret"]))) != "" {
		return false
	}

	status := strings.ToLower(strings.TrimSpace(stringValue(account["status"])))
	reason := strings.ToLower(strings.TrimSpace(stringValue(account["status_reason_code"])))
	errorKind := strings.ToLower(strings.TrimSpace(stringValue(account["last_error_kind"])))
	errorStatus := intValue(account["last_error_status"])
	if reason == "auth_invalid" || reason == "account_invalid" {
		return true
	}
	if status == "invalid" || status == "expired" || status == "unauthorized" {
		return true
	}
	return errorKind == "auth_invalid" && errorStatus == http.StatusUnauthorized
}

func accountStatusMatches(account map[string]any, filter string) bool {
	filter = strings.ToLower(strings.TrimSpace(filter))
	if filter == "" || filter == "all" {
		return true
	}
	return accountStatusCategory(account) == filter || strings.ToLower(stringValue(account["status"])) == filter
}

func accountContains(account map[string]any, keyword string) bool {
	for _, key := range []string{"access_token", "email", "user_id", "type", "source_type", "status", "proxy", "group_id"} {
		if strings.Contains(strings.ToLower(stringValue(account[key])), keyword) {
			return true
		}
	}
	return false
}

func accountGroupMatches(account map[string]any, groupID string) bool {
	groupID = strings.TrimSpace(groupID)
	if groupID == "" || groupID == "all" {
		return true
	}
	current := stringValue(account["group_id"])
	if groupID == "__ungrouped__" {
		return current == ""
	}
	return current == groupID
}

func accountToken(account map[string]any) string {
	if token := stringValue(account["access_token"]); token != "" {
		return token
	}
	if token := stringValue(account["accessToken"]); token != "" {
		return token
	}
	return stringValue(account["token"])
}

func accountPublicRef(account map[string]any) string {
	if ref := firstNonEmpty(
		stringValue(account["id"]),
		stringValue(account["account_ref"]),
		stringValue(account["account_id"]),
		stringValue(account["chatgpt_account_id"]),
		stringValue(account["user_id"]),
		strings.ToLower(stringValue(account["email"])),
	); ref != "" {
		return ref
	}
	token := accountToken(account)
	if token == "" {
		token = firstNonEmpty(stringValue(account["sso"]), stringValue(account["session_token"]), stringValue(account["token"]))
	}
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return "acct_" + hex.EncodeToString(sum[:8])
}

func accountRefForToken(items []map[string]any, token string) string {
	token = strings.TrimSpace(token)
	for _, item := range items {
		if accountToken(item) == token {
			return accountPublicRef(item)
		}
	}
	return ""
}

func accountRefMatches(account map[string]any, ref string) bool {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return false
	}
	refLower := strings.ToLower(ref)
	for _, candidate := range []string{
		accountToken(account),
		accountPublicRef(account),
		stringValue(account["id"]),
		stringValue(account["account_ref"]),
		stringValue(account["account_id"]),
		stringValue(account["chatgpt_account_id"]),
		stringValue(account["user_id"]),
		stringValue(account["email"]),
	} {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if candidate == ref || strings.ToLower(candidate) == refLower {
			return true
		}
	}
	return false
}

func uniqueAccountRefs(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		ref := strings.TrimSpace(value)
		if ref == "" {
			continue
		}
		key := strings.ToLower(ref)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, ref)
	}
	return result
}

func resolveAccountRefTokens(items []map[string]any, refs []string) ([]string, []string) {
	refs = uniqueAccountRefs(refs)
	tokens := make([]string, 0, len(refs))
	missing := []string{}
	seen := map[string]struct{}{}
	for _, ref := range refs {
		found := false
		for _, item := range items {
			if !accountRefMatches(item, ref) {
				continue
			}
			found = true
			if token := accountToken(item); token != "" {
				if _, ok := seen[token]; !ok {
					seen[token] = struct{}{}
					tokens = append(tokens, token)
				}
			}
			break
		}
		if !found {
			missing = append(missing, ref)
		}
	}
	return tokens, missing
}

func uniqueAccountTokens(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		token := strings.TrimSpace(value)
		if token == "" {
			continue
		}
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}
		result = append(result, token)
	}
	return result
}

func (s *Server) accountGroupsPayload() map[string]any {
	cfg, err := s.store.Config()
	if err != nil {
		return map[string]any{"groups": []map[string]any{}, "proxy_groups": []map[string]any{}}
	}
	return map[string]any{
		"groups":       accountGroupsFromConfig(cfg, s.accountCountByGroup()),
		"proxy_groups": mapList(cfg["proxy_groups"]),
	}
}

func (s *Server) accountCountByGroup() map[string]int {
	counts := map[string]int{}
	items, err := s.store.AccountList()
	if err != nil {
		return counts
	}
	for _, item := range items {
		if id := stringValue(item["group_id"]); id != "" {
			counts[id]++
		}
	}
	return counts
}

func accountGroupsFromConfig(cfg map[string]any, counts map[string]int) []map[string]any {
	groups := mapList(cfg["account_groups"])
	result := make([]map[string]any, 0, len(groups))
	for _, group := range groups {
		id := slugID(stringValue(group["id"]))
		if id == "" {
			continue
		}
		proxy := stringValue(group["proxy"])
		item := map[string]any{
			"id":             id,
			"name":           firstNonEmpty(stringValue(group["name"]), id),
			"proxy":          proxy,
			"proxy_group_id": strings.TrimPrefix(proxy, "group:"),
			"enabled":        boolValue(group["enabled"], true),
			"notes":          stringValue(group["notes"]),
			"account_count":  counts[id],
		}
		result = append(result, item)
	}
	return result
}

func mapList(value any) []map[string]any {
	raw, ok := value.([]any)
	if !ok {
		if typed, ok := value.([]map[string]any); ok {
			return typed
		}
		return []map[string]any{}
	}
	result := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if object, ok := item.(map[string]any); ok {
			result = append(result, object)
		}
	}
	return result
}

// cloneMap 深拷贝一份 JSON 形状的数据，委托给 store.CloneMap。
//
// 本包曾自带一份逐字相同的浅拷贝副本，凡把嵌套容器带出锁再改的调用点
// （代理组的 nodes、监控记录的 Metrics/Events、去重缓存）都在裸奔。
// 复制语义只允许有一处实现，所以这里不再保留第二份函数体。
func cloneMap(input map[string]any) map[string]any {
	return store.CloneMap(input)
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func boolValue(value any, fallback bool) bool {
	if value == nil {
		return fallback
	}
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	}
	return fallback
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func tokenPreview(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	if len(token) <= 12 {
		return "********"
	}
	return token[:6] + "..." + token[len(token)-4:]
}

func slugID(value string) string {
	var builder strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_':
			builder.WriteRune(r)
		case unicode.IsSpace(r):
			builder.WriteRune('-')
		}
	}
	return strings.Trim(builder.String(), "-_")
}

func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	staticDir := s.cfg.StaticDir
	relative := strings.TrimPrefix(filepath.ToSlash(filepath.Clean("/"+r.URL.Path)), "/")
	if relative == "" || relative == "." {
		relative = "index.html"
	}
	candidate := filepath.Join(staticDir, filepath.FromSlash(relative))
	if !isWithin(staticDir, candidate) {
		http.NotFound(w, r)
		return
	}
	if info, err := os.Stat(candidate); err != nil || info.IsDir() {
		candidate = filepath.Join(staticDir, "index.html")
	}
	if _, err := os.Stat(candidate); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "frontend assets are not built",
			"hint":  "run npm --prefix web-vue run build:server",
		})
		return
	}
	if filepath.Base(candidate) == "index.html" {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	}
	http.ServeFile(w, r, candidate)
}

func (s *Server) requireAPI(w http.ResponseWriter, r *http.Request) bool {
	if s.auth.ValidAPIRequest(r) {
		return true
	}
	writeError(w, http.StatusUnauthorized, "invalid or missing API key", "authentication_error")
	return false
}

func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if s.auth.ValidAdminRequest(r) {
		return true
	}
	writeError(w, http.StatusUnauthorized, "invalid or missing admin key", "authentication_error")
	return false
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message, kind string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"message": message, "type": kind},
	})
}

func serveTextFile(w http.ResponseWriter, filename, contentType string) {
	raw, err := os.ReadFile(filename)
	if err != nil {
		writeError(w, http.StatusNotFound, "file not found", "not_found")
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

func isWithin(root, candidate string) bool {
	root, _ = filepath.Abs(root)
	candidate, _ = filepath.Abs(candidate)
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))
}

// Shutdown flushes in-memory dirty state to disk during graceful process exit.
func (s *Server) Shutdown() {
	if s == nil {
		return
	}
	if s.store != nil {
		_ = s.store.FlushAccounts()
		s.store.FlushAuthKeys()
	}
	if s.hourlyMetrics != nil {
		s.hourlyMetrics.Flush()
	}
}
