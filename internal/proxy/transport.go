// [INPUT]: 仅标准库（net、net/http、golang.org/x/…)
// [OUTPUT]: 出网传输：DialContext、Transport、Manager、Lease、ConfigureImageGroups
// [POS]: HTTP/SOCKS4/SOCKS5 出网与图片节点租约。**代理组可能为 0 节点**，任何按节点数计算的容量都要先判空。
//         核心不变量：normalizeURL 的 ("", nil) 唯一表示**显式直连**，解析失败一律返回
//         error。非法串若与直连共用零值，会命中 Transport.cache[""]（http.DefaultTransport），
//         让账号带着服务器真实 IP 出网；账号级代理非法时必须返回 Lease{Source:"unavailable"}，
//         严禁穿透到全局池——那会让 A 账号的请求从 B 账号的出口出去。
//         健康状态按 URL 归一到 Manager.imageHealth，不得寄生在 imageNode 上：
//         配置重载会重建节点对象，而在途 lease 握的是旧指针。
// [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md

package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type contextKey struct{}

// WithURL attaches one egress proxy to a request. An empty URL means direct.
func WithURL(ctx context.Context, value string) context.Context {
	return context.WithValue(ctx, contextKey{}, strings.TrimSpace(value))
}

func URLFromContext(ctx context.Context) string {
	value, _ := ctx.Value(contextKey{}).(string)
	return strings.TrimSpace(value)
}

func URLSelectionFromContext(ctx context.Context) (string, bool) {
	value, ok := ctx.Value(contextKey{}).(string)
	return strings.TrimSpace(value), ok
}

func DialContext(ctx context.Context, target, proxyURL string) (net.Conn, error) {
	normalized, err := normalizeURL(proxyURL)
	if err != nil {
		return nil, err
	}
	if normalized == "" {
		return (&net.Dialer{}).DialContext(ctx, "tcp", target)
	}
	parsed, err := url.Parse(normalized)
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(parsed.Scheme) {
	case "socks4", "socks4a":
		return (&socks4Dialer{proxy: parsed}).DialContext(ctx, "tcp", target)
	case "socks", "socks5", "socks5h":
		return (&socks5Dialer{proxy: parsed}).DialContext(ctx, "tcp", target)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("unsupported proxy scheme %q", parsed.Scheme)
	}
	proxyAddress := parsed.Host
	if _, _, err := net.SplitHostPort(proxyAddress); err != nil {
		proxyAddress = net.JoinHostPort(proxyAddress, "8080")
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", proxyAddress)
	if err != nil {
		return nil, err
	}
	if parsed.Scheme == "https" {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: parsed.Hostname(), MinVersion: tls.VersionTLS12})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, err
		}
		conn = tlsConn
	}
	connect := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	if parsed.User != nil {
		password, _ := parsed.User.Password()
		credentials := base64.StdEncoding.EncodeToString([]byte(parsed.User.Username() + ":" + password))
		connect += "Proxy-Authorization: Basic " + credentials + "\r\n"
	}
	connect += "\r\n"
	if _, err := io.WriteString(conn, connect); err != nil {
		_ = conn.Close()
		return nil, err
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("HTTP proxy CONNECT returned HTTP %d", response.StatusCode)
	}
	return conn, nil
}

// Manager resolves an account-specific proxy first, then rotates the global pool.
type Manager struct {
	mu              sync.Mutex
	url             string
	pool            []string
	cursor          int
	resourceURL     string
	resourcePool    []string
	resourceCursor  int
	upstreamRouter  *UpstreamRouter
	imageGroups     map[string]*imageGroup
	imageGroupID    string
	imageCursor     int
	imageSelections uint64
	imageWake       chan struct{}
	onImageResult   func(ImageNodeRuntimeResult)
	// imageHealth 是跨配置重载保留的健康状态，键为归一化后的代理 URL。
	// 它才是权威来源：配置里的 runtime_failure_count 只是首次见到该 URL 时的种子。
	imageHealth map[string]*imageNodeHealth
}

// healthLocked 取出（必要时创建）某个 URL 的健康状态；调用方必须持 m.mu。
// seed 仅在首次见到该 URL 时生效——之后运行期状态说了算，重载不得把它打回快照。
func (m *Manager) healthLocked(url string, seed imageNodeHealth) *imageNodeHealth {
	if m.imageHealth == nil {
		m.imageHealth = map[string]*imageNodeHealth{}
	}
	health, ok := m.imageHealth[url]
	if !ok {
		health = &imageNodeHealth{}
		*health = seed
		m.imageHealth[url] = health
	}
	return health
}

type GroupConfig struct {
	ID, Name, Strategy string
	Enabled            bool
	Nodes              []NodeConfig
}

type NodeConfig struct {
	ID, Name, URL         string
	Enabled               bool
	ImageConcurrencyLimit int
	LastStatus            int
	LastError             string
	RuntimeFailures       int
	RuntimeSuccesses      int
	RuntimeLatencyMS      int64
}

type imageGroup struct {
	id, name, strategy string
	nodes              []*imageNode
}

// imageNodeHealth 是按 URL 归一的健康状态，**独立于 imageNode 对象的身份**。
//
// 必须独立存在：ConfigureImageGroups 每次重载都会重建 imageNode，而在途 Lease
// 握的是旧指针。健康计数若寄生在节点对象上，那次成功就写进了没人引用的对象——
// successes 只累计到 1、2 时既不触发持久化事件、又会在下一次重载被打回配置快照，
// 于是节点永远攒不够 imageNodeStableSuccess，pickStableNodeLocked 永远选不中它。
// 驱逐同理：按指针从组里移除，重载后一个都匹配不上，静默失效。
type imageNodeHealth struct {
	failures      int
	successes     int
	latencyMS     int64
	cooldownUntil time.Time
	evicted       bool
}

type imageNode struct {
	id, name, url   string
	limit, inFlight int
	health          *imageNodeHealth
}

type Lease struct {
	manager   *Manager
	node      *imageNode
	URL       string
	Source    string
	GroupID   string
	GroupName string
	NodeID    string
	NodeName  string
	once      sync.Once
	failed    atomic.Bool
	slow      atomic.Bool
	latencyMS atomic.Int64
}

type EgressInfo struct {
	Source, GroupID, GroupName, NodeID, NodeName string
}

type ImageNodeRuntimeResult struct {
	GroupID, GroupName, NodeID, NodeName, URL string
	Failures, Successes                       int
	LatencyMS                                 int64
	Removed                                   bool
}

const (
	imageNodeFailureLimit  = 3
	imageNodeStableSuccess = 3
	imageNodeCanaryEvery   = 20
	defaultImageNodeLimit  = 3
	imageNodeSlowCooldown  = time.Minute
)

type imageLeaseContextKey struct{}

func WithImageLease(ctx context.Context, lease *Lease) context.Context {
	if lease == nil {
		return ctx
	}
	return context.WithValue(ctx, imageLeaseContextKey{}, lease)
}

// MarkImageLeaseFailure remembers a transport failure even when a later
// stage-level retry succeeds through a different proxy.
func MarkImageLeaseFailure(ctx context.Context) {
	lease, _ := ctx.Value(imageLeaseContextKey{}).(*Lease)
	if lease != nil {
		lease.failed.Store(true)
	}
}

// ObserveImageLeaseStage records only proxy-sensitive stage latency. The
// upstream image-generation duration must not be included in this score.
func ObserveImageLeaseStage(ctx context.Context, elapsed, slowAfter time.Duration) {
	lease, _ := ctx.Value(imageLeaseContextKey{}).(*Lease)
	if lease == nil || elapsed <= 0 {
		return
	}
	lease.latencyMS.Add(max(elapsed.Milliseconds(), 1))
	if slowAfter > 0 && elapsed > slowAfter {
		lease.slow.Store(true)
	}
}

func (m *Manager) SetImageNodeResultCallback(callback func(ImageNodeRuntimeResult)) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.onImageResult = callback
	m.mu.Unlock()
}

// normalizePool 过滤掉代理池里无法解析的条目。
//
// 单个坏条目不该让整个池不可用，所以这里丢弃而非报错；但丢弃是刻意的策略，
// 不是 normalizeURL 失败时的默认结果——两者在类型上已经分开。
func normalizePool(pool []string) []string {
	clean := make([]string, 0, len(pool))
	for _, item := range pool {
		value, err := normalizeURL(item)
		if err != nil || value == "" {
			continue
		}
		clean = append(clean, value)
	}
	return clean
}

// defaultProxyURL 归一化全局默认代理；非法值退化为未配置（直连）。
// 全局默认是显式配置，写错时不影响账号身份，但仍然只记为空而不报错。
func defaultProxyURL(single string) string {
	value, err := normalizeURL(single)
	if err != nil {
		return ""
	}
	return value
}

func NewManager(single string, pool []string) *Manager {
	return &Manager{url: defaultProxyURL(single), pool: normalizePool(pool), imageGroups: map[string]*imageGroup{}, imageWake: make(chan struct{})}
}

func (m *Manager) SetDefault(single string, pool []string) {
	if m == nil {
		return
	}
	clean := normalizePool(pool)
	m.mu.Lock()
	m.url = defaultProxyURL(single)
	m.pool = clean
	m.cursor = 0
	m.mu.Unlock()
}

// ConfigureImageGroups enables request-scoped image egress selection. A
// fallback group is used as the active image pool; the default proxy remains
// available when every group node is busy or cooling down.
func (m *Manager) ConfigureImageGroups(fallback string, groups []GroupConfig) {
	if m == nil {
		return
	}
	next := map[string]*imageGroup{}
	seeds := map[*imageNode]imageNodeHealth{}
	live := map[string]bool{}
	for _, group := range groups {
		id := strings.TrimSpace(group.ID)
		if id == "" || !group.Enabled {
			continue
		}
		item := &imageGroup{id: id, name: strings.TrimSpace(group.Name), strategy: strings.TrimSpace(group.Strategy)}
		for _, node := range group.Nodes {
			proxyURL, normalizeErr := normalizeURL(node.URL)
			if normalizeErr != nil || !node.Enabled || proxyURL == "" || !imageProxyCompatible(proxyURL) || !probeAllowsRuntimeValidation(node.LastStatus, node.LastError) {
				continue
			}
			limit := node.ImageConcurrencyLimit
			if limit < 1 {
				limit = defaultImageNodeLimit
			}
			created := &imageNode{id: strings.TrimSpace(node.ID), name: strings.TrimSpace(node.Name), url: proxyURL, limit: limit}
			// 配置值只作种子，权威来源是跨重载保留的健康表。
			seeds[created] = imageNodeHealth{
				failures:  max(node.RuntimeFailures, 0),
				successes: max(node.RuntimeSuccesses, 0),
				latencyMS: max(node.RuntimeLatencyMS, 0),
			}
			live[proxyURL] = true
			item.nodes = append(item.nodes, created)
		}
		next[id] = item
	}
	groupID := ""
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(fallback)), "group:") {
		groupID = strings.TrimSpace(strings.TrimSpace(fallback)[len("group:"):])
	}

	m.mu.Lock()
	for node, seed := range seeds {
		node.health = m.healthLocked(node.url, seed)
	}
	// 配置里已消失的 URL 不再保留状态，避免这张表随订阅轮换无限增长。
	for url := range m.imageHealth {
		if !live[url] {
			delete(m.imageHealth, url)
		}
	}
	// 过滤必须在绑定健康状态之后做：配置里的 runtime_failure_count 可能过期，
	// 权威来源是健康表——否则刚判定驱逐的节点会在下一次重载原样复活。
	for _, group := range next {
		nodes := make([]*imageNode, 0, len(group.nodes))
		for _, node := range group.nodes {
			if node.health.evicted || node.health.failures >= imageNodeFailureLimit {
				continue
			}
			nodes = append(nodes, node)
		}
		group.nodes = nodes
	}
	m.imageGroups = next
	m.imageGroupID = groupID
	m.imageCursor = 0
	m.imageSelections = 0
	m.signalImageLocked()
	m.mu.Unlock()
}

func probeAllowsRuntimeValidation(status int, lastError string) bool {
	if status == http.StatusForbidden || (status >= 200 && status < 400) {
		return true
	}
	return status == 0 && strings.TrimSpace(lastError) == ""
}

// AcquireImage chooses one proxy for the complete multi-stage image request.
// The caller must release the lease so node concurrency and cooldown state stay accurate.
func (m *Manager) AcquireImage(fields map[string]any) *Lease {
	if m == nil {
		return &Lease{}
	}
	if fields != nil {
		for _, key := range []string{"proxy", "proxy_url", "proxyUrl"} {
			value := strings.TrimSpace(stringValue(fields[key]))
			if value == "" {
				continue
			}
			if strings.HasPrefix(strings.ToLower(value), "group:") {
				if lease := m.acquireGroup(strings.TrimSpace(value[len("group:"):])); lease != nil {
					return lease
				}
				break
			}
			normalized, normalizeErr := normalizeURL(value)
			if normalizeErr != nil {
				// 账号声明了代理却写错了——绝不能穿透到全局池（A 账号的请求会从
				// B 账号的出口出去）或退化成直连（暴露服务器真实 IP）。
				// "unavailable" 是既有哨兵，调用方据此让本次请求显式失败。
				return &Lease{Source: "unavailable"}
			}
			if normalized != "" && imageProxyCompatible(normalized) {
				return &Lease{URL: normalized, Source: "account"}
			}
		}
	}
	m.mu.Lock()
	groupID := m.imageGroupID
	m.mu.Unlock()
	if groupID != "" {
		if lease := m.acquireGroup(groupID); lease != nil {
			return lease
		}
	}
	proxyURL, configured := m.resolveImageFallback()
	if proxyURL == "" && configured {
		return &Lease{Source: "unavailable"}
	}
	return &Lease{URL: proxyURL, Source: "default"}
}

// AcquireImageContext waits for configured proxy-group capacity instead of
// overflowing concurrent requests onto one default proxy. It falls back only
// when the requested group has no usable runtime nodes at all.
func (m *Manager) AcquireImageContext(ctx context.Context, fields map[string]any) (*Lease, error) {
	if m == nil {
		return &Lease{}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	groupID := ""
	if fields != nil {
		for _, key := range []string{"proxy", "proxy_url", "proxyUrl"} {
			value := strings.TrimSpace(stringValue(fields[key]))
			if value == "" {
				continue
			}
			if strings.HasPrefix(strings.ToLower(value), "group:") {
				groupID = strings.TrimSpace(value[len("group:"):])
				break
			}
			normalized, normalizeErr := normalizeURL(value)
			if normalizeErr != nil {
				// 同 AcquireImageContext：账号级代理非法必须显式失败，
				// 不允许穿透到全局池或退化成直连。
				return &Lease{Source: "unavailable"}, nil
			}
			if normalized != "" && imageProxyCompatible(normalized) {
				return &Lease{URL: normalized, Source: "account"}, nil
			}
			break
		}
	}
	if groupID == "" {
		m.mu.Lock()
		groupID = m.imageGroupID
		m.mu.Unlock()
	}
	if groupID != "" {
		lease, _, err := m.acquireGroupContext(ctx, groupID)
		if err != nil {
			return nil, err
		}
		if lease != nil {
			return lease, nil
		}
	}
	proxyURL, configured := m.resolveImageFallback()
	if proxyURL == "" && configured {
		return &Lease{Source: "unavailable"}, nil
	}
	return &Lease{URL: proxyURL, Source: "default"}, nil
}

func (m *Manager) acquireGroupContext(ctx context.Context, groupID string) (*Lease, bool, error) {
	for {
		m.mu.Lock()
		lease, available, retryAt := m.acquireGroupLocked(groupID)
		wake := m.imageWake
		m.mu.Unlock()
		if lease != nil || !available {
			return lease, available, nil
		}

		var timer *time.Timer
		var timerC <-chan time.Time
		if !retryAt.IsZero() {
			delay := time.Until(retryAt)
			if delay < 0 {
				delay = 0
			}
			timer = time.NewTimer(delay)
			timerC = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return nil, true, ctx.Err()
		case <-wake:
			if timer != nil {
				timer.Stop()
			}
		case <-timerC:
		}
	}
}

func (m *Manager) acquireGroupLocked(groupID string) (*Lease, bool, time.Time) {
	group := m.imageGroups[groupID]
	if group == nil || len(group.nodes) == 0 {
		return nil, false, time.Time{}
	}
	now := time.Now()
	var retryAt time.Time
	stableExists := false
	for _, node := range group.nodes {
		if node.health.evicted {
			continue
		}
		if node.health.successes >= imageNodeStableSuccess && node.health.failures == 0 {
			stableExists = true
		}
		if now.Before(node.health.cooldownUntil) && (retryAt.IsZero() || node.health.cooldownUntil.Before(retryAt)) {
			retryAt = node.health.cooldownUntil
		}
	}
	canary := stableExists && (m.imageSelections+1)%imageNodeCanaryEvery == 0
	index := -1
	if canary {
		index = m.pickProbeNodeLocked(group, now, "")
	}
	if index < 0 && stableExists {
		index = m.pickStableNodeLocked(group, now, "")
	}
	// When every stable node is busy or cooling down, use spare validation
	// capacity instead of consuming the caller's entire request deadline in
	// the egress queue.
	if index < 0 {
		index = m.pickProbeNodeLocked(group, now, "")
	}
	if index >= 0 {
		node := group.nodes[index]
		node.inFlight++
		m.imageSelections++
		m.imageCursor = (index + 1) % len(group.nodes)
		return newImageLease(m, group, node), true, time.Time{}
	}
	return nil, true, retryAt
}

func (m *Manager) pickStableNodeLocked(group *imageGroup, now time.Time, excludedURL string) int {
	best := -1
	for offset := 0; offset < len(group.nodes); offset++ {
		index := (m.imageCursor + offset) % len(group.nodes)
		node := group.nodes[index]
		if node.health.evicted || node.url == excludedURL || node.health.successes < imageNodeStableSuccess || node.health.failures > 0 ||
			node.inFlight >= node.limit || now.Before(node.health.cooldownUntil) {
			continue
		}
		if best < 0 || imageNodeBetter(node, group.nodes[best]) {
			best = index
		}
	}
	return best
}

func (m *Manager) pickProbeNodeLocked(group *imageGroup, now time.Time, excludedURL string) int {
	for offset := 0; offset < len(group.nodes); offset++ {
		index := (m.imageCursor + offset) % len(group.nodes)
		node := group.nodes[index]
		if node.health.evicted || node.url == excludedURL || (node.health.successes >= imageNodeStableSuccess && node.health.failures == 0) ||
			node.inFlight >= node.limit || now.Before(node.health.cooldownUntil) {
			continue
		}
		return index
	}
	return -1
}

func imageNodeBetter(candidate, current *imageNode) bool {
	left := candidate.inFlight * current.limit
	right := current.inFlight * candidate.limit
	if left != right {
		return left < right
	}
	return effectiveImageNodeLatency(candidate) < effectiveImageNodeLatency(current)
}

func effectiveImageNodeLatency(node *imageNode) int64 {
	if node == nil || node.health.latencyMS <= 0 {
		return int64((60 * time.Second) / time.Millisecond)
	}
	return node.health.latencyMS
}

func newImageLease(manager *Manager, group *imageGroup, node *imageNode) *Lease {
	return &Lease{manager: manager, node: node, URL: node.url, Source: "group", GroupID: group.id, GroupName: group.name, NodeID: node.id, NodeName: node.name}
}

func (m *Manager) resolveImageFallback() (string, bool) {
	if m == nil {
		return "", false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	configured := m.url != "" || len(m.pool) > 0
	for offset := 0; offset < len(m.pool); offset++ {
		index := (m.cursor + offset) % len(m.pool)
		if imageProxyCompatible(m.pool[index]) {
			m.cursor = (index + 1) % len(m.pool)
			return m.pool[index], true
		}
	}
	if imageProxyCompatible(m.url) {
		return m.url, true
	}
	if m.upstreamRouter != nil {
		if upstream := m.upstreamRouter.Resolve(); upstream != "" {
			configured = true
			if imageProxyCompatible(upstream) {
				return upstream, true
			}
		}
	}
	return "", configured
}

// The authenticated ChatGPT image flow uses tls-client for browser TLS
// fingerprinting. That client supports HTTP(S) and SOCKS5, but not SOCKS4.
func imageProxyCompatible(proxyURL string) bool {
	normalized, err := normalizeURL(proxyURL)
	if err != nil {
		return false
	}
	parsed, err := url.Parse(normalized)
	if err != nil || parsed.Host == "" {
		return false
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "socks5", "socks5h":
		return true
	default:
		return false
	}
}

func (m *Manager) acquireGroup(groupID string) *Lease {
	m.mu.Lock()
	defer m.mu.Unlock()
	lease, _, _ := m.acquireGroupLocked(groupID)
	return lease
}

// AcquireStableImage selects a different group node that has already
// completed several real image requests. It never falls back to direct mode
// or the default proxy.
func (m *Manager) AcquireStableImage(fields map[string]any, excludedURL string) *Lease {
	if m == nil {
		return nil
	}
	groupID := ""
	for _, key := range []string{"proxy", "proxy_url", "proxyUrl"} {
		value := strings.TrimSpace(stringValue(fields[key]))
		if strings.HasPrefix(strings.ToLower(value), "group:") {
			groupID = strings.TrimSpace(value[len("group:"):])
			break
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if groupID == "" {
		groupID = m.imageGroupID
	}
	group := m.imageGroups[groupID]
	if group == nil || len(group.nodes) == 0 {
		return nil
	}
	now := time.Now()
	// 排除项只是"本轮别再选它"的提示，无法解析等价于没有排除项，按空处理。
	excludedURL, _ = normalizeURL(excludedURL)
	index := m.pickStableNodeLocked(group, now, excludedURL)
	if index < 0 {
		return nil
	}
	node := group.nodes[index]
	node.inFlight++
	m.imageCursor = (index + 1) % len(group.nodes)
	return newImageLease(m, group, node)
}

func (l *Lease) ObserveLatency(elapsed time.Duration, slowAfter time.Duration) {
	if l == nil || elapsed <= 0 {
		return
	}
	l.latencyMS.Add(max(elapsed.Milliseconds(), 1))
	if slowAfter > 0 && elapsed > slowAfter {
		l.slow.Store(true)
	}
}

func (l *Lease) Release(runtimeFailure bool) {
	if l == nil {
		return
	}
	l.once.Do(func() {
		if l.manager == nil || l.node == nil {
			return
		}
		var event *ImageNodeRuntimeResult
		l.manager.mu.Lock()
		if l.node.inFlight > 0 {
			l.node.inFlight--
		}
		runtimeFailure = runtimeFailure || l.failed.Load()
		observedLatencyMS := l.latencyMS.Load()
		if runtimeFailure && !l.node.health.evicted {
			l.node.health.failures++
			cooldown := time.Duration(1<<min(l.node.health.failures-1, 4)) * time.Minute
			l.node.health.cooldownUntil = time.Now().Add(cooldown)
			removed := l.node.health.failures >= imageNodeFailureLimit
			if removed {
				l.node.health.evicted = true
				// 配置重载可能把组过滤成 0 节点，此时 len(group.nodes)-1 为 -1，
				// make 的负容量会 panic；空组没有可驱逐的节点，跳过。
				if group := l.manager.imageGroups[l.GroupID]; group != nil && len(group.nodes) > 0 {
					nodes := make([]*imageNode, 0, len(group.nodes)-1)
					for _, node := range group.nodes {
						if node != l.node {
							nodes = append(nodes, node)
						}
					}
					group.nodes = nodes
					if len(nodes) == 0 {
						l.manager.imageCursor = 0
					} else {
						l.manager.imageCursor %= len(nodes)
					}
				}
			}
			event = &ImageNodeRuntimeResult{GroupID: l.GroupID, GroupName: l.GroupName, NodeID: l.NodeID, NodeName: l.NodeName, URL: l.URL, Failures: l.node.health.failures, Successes: l.node.health.successes, LatencyMS: l.node.health.latencyMS, Removed: removed}
		} else if !runtimeFailure && !l.node.health.evicted {
			hadFailures := l.node.health.failures > 0
			l.node.health.failures = 0
			l.node.health.successes++
			if observedLatencyMS > 0 {
				if l.node.health.latencyMS <= 0 {
					l.node.health.latencyMS = observedLatencyMS
				} else {
					l.node.health.latencyMS = (l.node.health.latencyMS*7 + observedLatencyMS*3) / 10
				}
			}
			if l.slow.Load() {
				l.node.health.cooldownUntil = time.Now().Add(imageNodeSlowCooldown)
			} else {
				l.node.health.cooldownUntil = time.Time{}
			}
			if hadFailures || l.slow.Load() || l.node.health.successes == imageNodeStableSuccess || l.node.health.successes%25 == 0 {
				event = &ImageNodeRuntimeResult{GroupID: l.GroupID, GroupName: l.GroupName, NodeID: l.NodeID, NodeName: l.NodeName, URL: l.URL, Successes: l.node.health.successes, LatencyMS: l.node.health.latencyMS}
			}
		}
		callback := l.manager.onImageResult
		l.manager.signalImageLocked()
		l.manager.mu.Unlock()
		if event != nil && callback != nil {
			callback(*event)
		}
	})
}

func (m *Manager) signalImageLocked() {
	if m.imageWake != nil {
		close(m.imageWake)
	}
	m.imageWake = make(chan struct{})
}

func (m *Manager) DescribeImageEgress(proxyURL string) EgressInfo {
	if m == nil {
		return EgressInfo{Source: "direct"}
	}
	// 纯描述用途：非法串不参与匹配，落到下面的直连分支即可。
	normalized, _ := normalizeURL(proxyURL)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, group := range m.imageGroups {
		for _, node := range group.nodes {
			if node.url == normalized {
				return EgressInfo{Source: "group", GroupID: group.id, GroupName: group.name, NodeID: node.id, NodeName: node.name}
			}
		}
	}
	if normalized == "" {
		return EgressInfo{Source: "direct"}
	}
	return EgressInfo{Source: "default"}
}

func (m *Manager) SetResource(single string, pool []string) {
	if m == nil {
		return
	}
	clean := normalizePool(pool)
	m.mu.Lock()
	m.resourceURL = defaultProxyURL(single)
	m.resourcePool = clean
	m.resourceCursor = 0
	m.mu.Unlock()
}

func (m *Manager) SetUpstreamsFile(path string) {
	if m == nil {
		return
	}
	router := NewUpstreamRouter(path)
	m.mu.Lock()
	m.upstreamRouter = router
	m.mu.Unlock()
}

func (m *Manager) Resolve(fields map[string]any, resource bool) string {
	if fields != nil {
		for _, key := range []string{"proxy", "proxy_url", "proxyUrl"} {
			if value := stringValue(fields[key]); value != "" && !strings.HasPrefix(strings.ToLower(value), "group:") {
				normalized, err := normalizeURL(value)
				if err != nil {
					// 账号声明了代理却写错了：原样返回，由 forProxy 以
					// "invalid proxy URL" 拒掉本次请求。在这里返回 ""
					// （= 直连）或继续往下穿透到全局池，都会把故障藏起来。
					return value
				}
				// 显式 "direct" 归一化为空串即直连，同样不得穿透到全局池。
				return normalized
			}
		}
	}
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	pool := m.pool
	baseURL := m.url
	if resource && len(m.resourcePool) > 0 {
		pool = m.resourcePool
		baseURL = m.resourceURL
	}
	if len(pool) > 0 {
		cursor := &m.cursor
		if resource {
			cursor = &m.resourceCursor
		}
		value := pool[*cursor%len(pool)]
		*cursor++
		return value
	}
	if resource && m.resourceURL != "" {
		return m.resourceURL
	}
	if m.upstreamRouter != nil {
		if upstream := m.upstreamRouter.Resolve(); upstream != "" {
			return upstream
		}
	}
	return baseURL
}

func (m *Manager) Snapshot() map[string]any {
	if m == nil {
		return map[string]any{"mode": "direct", "count": 0}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	mode := "direct"
	if len(m.pool) > 0 {
		mode = "proxy_pool"
	} else if m.url != "" {
		mode = "single_proxy"
	}
	snapshot := map[string]any{"mode": mode, "count": len(m.pool), "resource_count": len(m.resourcePool), "proxy_configured": m.url != "" || len(m.pool) > 0 || m.resourceURL != "" || len(m.resourcePool) > 0}
	if group := m.imageGroups[m.imageGroupID]; group != nil {
		snapshot["mode"] = "proxy_group"
		snapshot["image_group_id"] = group.id
		snapshot["image_group_count"] = len(group.nodes)
		snapshot["proxy_configured"] = len(group.nodes) > 0 || snapshot["proxy_configured"] == true
	}
	if m.upstreamRouter != nil {
		snapshot["upstreams"] = m.upstreamRouter.Snapshot()
	}
	return snapshot
}

// Transport selects a transport per request so account proxy affinity does not
// require rebuilding all providers. HTTP(S) proxies use net/http; SOCKS5 is
// implemented locally to keep the main Go module dependency-free.
type Transport struct {
	base  http.RoundTripper
	mu    sync.Mutex
	cache map[string]http.RoundTripper
}

func NewTransport(base http.RoundTripper) *Transport {
	if base == nil {
		base = http.DefaultTransport
	}
	return &Transport{base: base, cache: map[string]http.RoundTripper{"": base}}
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	proxyURL := URLFromContext(req.Context())
	roundTripper, err := t.forProxy(proxyURL)
	if err != nil {
		return nil, err
	}
	return roundTripper.RoundTrip(req)
}

func (t *Transport) forProxy(proxyURL string) (http.RoundTripper, error) {
	// 非法串必须在这里以错误的形式被拒。若沿用"解析失败就返回空串"的旧语义，
	// 它会命中 cache[""] ——也就是直连传输，于是账号带着服务器真实 IP 出网。
	normalized, err := normalizeURL(proxyURL)
	if err != nil {
		return nil, err
	}
	proxyURL = normalized
	t.mu.Lock()
	if roundTripper, ok := t.cache[proxyURL]; ok {
		t.mu.Unlock()
		return roundTripper, nil
	}
	t.mu.Unlock()
	// proxyURL 为空即显式直连，由 NewTransport 预置的 cache[""] 命中上面的分支。
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return nil, err
	}
	var roundTripper http.RoundTripper
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		transport := cloneHTTPTransport(t.base)
		transport.Proxy = http.ProxyURL(parsed)
		roundTripper = transport
	case "socks4", "socks4a":
		transport := cloneHTTPTransport(t.base)
		transport.Proxy = nil
		transport.DialContext = (&socks4Dialer{proxy: parsed}).DialContext
		roundTripper = transport
	case "socks5", "socks5h", "socks":
		transport := cloneHTTPTransport(t.base)
		transport.Proxy = nil
		transport.DialContext = (&socks5Dialer{proxy: parsed}).DialContext
		roundTripper = transport
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q", parsed.Scheme)
	}
	t.mu.Lock()
	if current, exists := t.cache[proxyURL]; exists {
		roundTripper = current
	} else {
		t.cache[proxyURL] = roundTripper
	}
	t.mu.Unlock()
	return roundTripper, nil
}

func cloneHTTPTransport(base http.RoundTripper) *http.Transport {
	var t *http.Transport
	if transport, ok := base.(*http.Transport); ok {
		t = transport.Clone()
	} else {
		t = http.DefaultTransport.(*http.Transport).Clone()
	}
	tuneTransport(t)
	return t
}

func tuneTransport(t *http.Transport) {
	if t == nil {
		return
	}
	if t.MaxIdleConns < 1000 {
		t.MaxIdleConns = 1000
	}
	if t.MaxIdleConnsPerHost < 100 {
		t.MaxIdleConnsPerHost = 100
	}
	if t.IdleConnTimeout == 0 {
		t.IdleConnTimeout = 90 * time.Second
	}
}

type socks5Dialer struct{ proxy *url.URL }

type socks4Dialer struct{ proxy *url.URL }

func (d *socks4Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("SOCKS4 only supports TCP, got %s", network)
	}
	proxyAddress := d.proxy.Host
	if _, _, err := net.SplitHostPort(proxyAddress); err != nil {
		proxyAddress = net.JoinHostPort(proxyAddress, "1080")
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", proxyAddress)
	if err != nil {
		return nil, err
	}
	if err := socks4Connect(conn, address, d.proxy.User); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func socks4Connect(conn io.ReadWriter, address string, user *url.Userinfo) error {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid target port %q", portText)
	}
	packet := []byte{0x04, 0x01, byte(port >> 8), byte(port)}
	ip := net.ParseIP(host).To4()
	if ip == nil {
		packet = append(packet, 0x00, 0x00, 0x00, 0x01)
	} else {
		packet = append(packet, ip...)
	}
	if user != nil {
		packet = append(packet, []byte(user.Username())...)
	}
	packet = append(packet, 0x00)
	if ip == nil {
		packet = append(packet, []byte(host)...)
		packet = append(packet, 0x00)
	}
	if _, err := conn.Write(packet); err != nil {
		return err
	}
	var reply [8]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		return err
	}
	if reply[1] != 0x5a {
		return fmt.Errorf("SOCKS4 connect failed with code 0x%02x", reply[1])
	}
	return nil
}

func (d *socks5Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("SOCKS5 only supports TCP, got %s", network)
	}
	proxyAddress := d.proxy.Host
	if _, _, err := net.SplitHostPort(proxyAddress); err != nil {
		proxyAddress = net.JoinHostPort(proxyAddress, "1080")
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", proxyAddress)
	if err != nil {
		return nil, err
	}
	if err := socks5Handshake(conn, d.proxy.User); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := socks5Connect(conn, address); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func socks5Handshake(conn io.ReadWriter, user *url.Userinfo) error {
	methods := []byte{0x00}
	if user != nil {
		methods = append(methods, 0x02)
	}
	if _, err := conn.Write(append([]byte{0x05, byte(len(methods))}, methods...)); err != nil {
		return err
	}
	var selected [2]byte
	if _, err := io.ReadFull(conn, selected[:]); err != nil {
		return err
	}
	if selected[0] != 0x05 {
		return errors.New("invalid SOCKS5 version")
	}
	if selected[1] == 0xff {
		return errors.New("SOCKS5 proxy rejected authentication methods")
	}
	if selected[1] == 0x02 {
		if user == nil {
			return errors.New("SOCKS5 requested username authentication")
		}
		username := user.Username()
		password, _ := user.Password()
		if len(username) > 255 || len(password) > 255 {
			return errors.New("SOCKS5 credentials are too long")
		}
		packet := append([]byte{0x01, byte(len(username))}, []byte(username)...)
		packet = append(packet, byte(len(password)))
		packet = append(packet, []byte(password)...)
		if _, err := conn.Write(packet); err != nil {
			return err
		}
		var authReply [2]byte
		if _, err := io.ReadFull(conn, authReply[:]); err != nil {
			return err
		}
		if authReply[1] != 0x00 {
			return errors.New("SOCKS5 username authentication failed")
		}
	}
	return nil
}

func socks5Connect(conn io.ReadWriter, address string) error {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid target port %q", portText)
	}
	packet := []byte{0x05, 0x01, 0x00}
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			packet = append(packet, 0x01)
			packet = append(packet, ip4...)
		} else {
			packet = append(packet, 0x04)
			packet = append(packet, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return errors.New("SOCKS5 target hostname is too long")
		}
		packet = append(packet, 0x03, byte(len(host)))
		packet = append(packet, []byte(host)...)
	}
	var portBytes [2]byte
	binary.BigEndian.PutUint16(portBytes[:], uint16(port))
	packet = append(packet, portBytes[:]...)
	if _, err := conn.Write(packet); err != nil {
		return err
	}
	var reply [4]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		return err
	}
	if reply[0] != 0x05 || reply[1] != 0x00 {
		return fmt.Errorf("SOCKS5 connect failed with code 0x%02x", reply[1])
	}
	length := 0
	switch reply[3] {
	case 0x01:
		length = 4
	case 0x04:
		length = 16
	case 0x03:
		var size [1]byte
		if _, err := io.ReadFull(conn, size[:]); err != nil {
			return err
		}
		length = int(size[0])
	default:
		return errors.New("SOCKS5 returned an invalid address type")
	}
	discard := make([]byte, length+2)
	_, err = io.ReadFull(conn, discard)
	return err
}

// normalizeURL 归一化一个代理串。
//
// 返回 ("", nil) 表示显式直连（空串或 "direct"）；返回非空串表示一个可用的代理。
// 无法解析的输入一律返回 error。
//
// "非法"绝不能与"直连"共用同一个返回值：代理池/账号字段里一个手误的串
// （订阅导出常见的 ip:port:user:pass、密码含裸 % 等）会因此静默退化成直连，
// 请求从服务器真实 IP 出网，账号与机房 IP 被上游关联，而全程没有任何告警。
func normalizeURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, "direct") {
		return "", nil
	}
	if !strings.Contains(value, "://") {
		value = "http://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("invalid proxy URL %q", value)
	}
	return parsed.String(), nil
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}
