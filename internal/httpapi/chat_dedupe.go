// [INPUT]: accounts/protocol
// [OUTPUT]: 聊天在途去重与短缓存：chatDeduplicator、chatCacheKey
// [POS]: 对话请求入口处拦截并发连击与重试风暴，同上下文请求在途合并，防多账号白烧额度。
//         关键约束：结果一经 finish* 定稿（completed=true）便不得再改写。
//         finish* 先写 err 再 close(done)，等待方读 err 依赖这条 happens-before 链；
//         owner 的 defer 在 close(done) 之后执行，若仍允许改写，成功会被覆盖成假错误。
// [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md

package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/protocol"
)

type chatCacheEntry struct {
	expiresAt time.Time
	response  map[string]any
	chunks    [][]byte
}

type inflightChatCall struct {
	done     chan struct{}
	once     sync.Once
	response map[string]any
	chunks   [][]byte
	err      error
	// completed 标记结果是否已由 finish* 定稿。
	//
	// 它必须存在：finish* 会先写入 err、再 close(done)，等待方从 <-done
	// 醒来后读到的值有明确的 happens-before 保证。而 owner 的 defer 在
	// close(done) 之后才执行，若此时还允许改写 err，就等于在结果发布之后
	// 再去改它——等待方会读到"成功"被覆盖成的假错误，且这次读写之间
	// 没有任何同步。定稿之后不再改写，这条链才成立。
	completed bool
}

type chatDeduplicator struct {
	mu         sync.Mutex
	inflight   map[string]*inflightChatCall
	cache      map[string]chatCacheEntry
	ttl        time.Duration
	maxEntries int
	enabled    bool
}

func newChatDeduplicator(ttl time.Duration, enabled bool) *chatDeduplicator {
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	return &chatDeduplicator{
		inflight:   make(map[string]*inflightChatCall),
		cache:      make(map[string]chatCacheEntry),
		ttl:        ttl,
		maxEntries: 512,
		enabled:    enabled,
	}
}

func chatCacheKey(request protocol.ChatRequest) string {
	hasher := sha256.New()
	hasher.Write([]byte(request.Model))
	hasher.Write([]byte(fmt.Sprintf("|stream:%v|", request.Stream)))
	if request.Temperature != nil {
		hasher.Write([]byte(fmt.Sprintf("t:%.2f|", *request.Temperature)))
	}
	if request.TopP != nil {
		hasher.Write([]byte(fmt.Sprintf("p:%.2f|", *request.TopP)))
	}
	if request.ReasoningEffort != "" {
		hasher.Write([]byte(request.ReasoningEffort + "|"))
	}
	for _, m := range request.Messages {
		hasher.Write([]byte(m.Role))
		hasher.Write([]byte(":"))
		switch c := m.Content.(type) {
		case string:
			hasher.Write([]byte(c))
		default:
			raw, _ := json.Marshal(c)
			hasher.Write(raw)
		}
		hasher.Write([]byte("\n"))
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func (d *chatDeduplicator) getCachedResponse(key string) (map[string]any, bool) {
	if d == nil || !d.enabled {
		return nil, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	entry, ok := d.cache[key]
	if !ok || time.Now().After(entry.expiresAt) {
		if ok {
			delete(d.cache, key)
		}
		return nil, false
	}
	return cloneMap(entry.response), true
}

func (d *chatDeduplicator) getCachedStream(key string) ([][]byte, bool) {
	if d == nil || !d.enabled {
		return nil, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	entry, ok := d.cache[key]
	if !ok || time.Now().After(entry.expiresAt) || len(entry.chunks) == 0 {
		if ok {
			delete(d.cache, key)
		}
		return nil, false
	}
	chunksCopy := make([][]byte, len(entry.chunks))
	for i, c := range entry.chunks {
		b := make([]byte, len(c))
		copy(b, c)
		chunksCopy[i] = b
	}
	return chunksCopy, true
}

func (d *chatDeduplicator) getOrStart(key string) (*inflightChatCall, bool) {
	if d == nil || !d.enabled {
		return &inflightChatCall{done: make(chan struct{})}, true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if existing, ok := d.inflight[key]; ok {
		return existing, false
	}
	call := &inflightChatCall{done: make(chan struct{})}
	d.inflight[key] = call
	return call, true
}

func (d *chatDeduplicator) finishResponse(key string, call *inflightChatCall, response map[string]any, err error) {
	if d == nil || !d.enabled || call == nil {
		return
	}
	d.mu.Lock()
	delete(d.inflight, key)
	if err == nil && len(response) > 0 {
		d.pruneOldLocked()
		d.cache[key] = chatCacheEntry{
			expiresAt: time.Now().Add(d.ttl),
			response:  cloneMap(response),
		}
	}
	call.response = response
	call.err = err
	call.completed = true
	d.mu.Unlock()
	call.once.Do(func() {
		close(call.done)
	})
}

func (d *chatDeduplicator) finishStream(key string, call *inflightChatCall, chunks [][]byte, err error) {
	if d == nil || !d.enabled || call == nil {
		return
	}
	d.mu.Lock()
	delete(d.inflight, key)
	if err == nil && len(chunks) > 0 {
		d.pruneOldLocked()
		chunksCopy := make([][]byte, len(chunks))
		for i, c := range chunks {
			b := make([]byte, len(c))
			copy(b, c)
			chunksCopy[i] = b
		}
		d.cache[key] = chatCacheEntry{
			expiresAt: time.Now().Add(d.ttl),
			chunks:    chunksCopy,
		}
	}
	call.chunks = chunks
	call.err = err
	call.completed = true
	d.mu.Unlock()
	call.once.Do(func() {
		close(call.done)
	})
}

func (d *chatDeduplicator) cancelIfInflight(key string, call *inflightChatCall, err error) {
	if d == nil || !d.enabled || call == nil {
		return
	}
	d.mu.Lock()
	if d.inflight[key] == call {
		delete(d.inflight, key)
	}
	// 只在结果尚未定稿时兜底：owner 中途 return（例如客户端断开）才需要
	// 让等待方拿到一个错误而不是永久挂住。已经 finish 过的调用不能被改写，
	// 否则一次成功的上游请求会被标成失败。
	if !call.completed {
		call.err = err
		call.completed = true
	}
	d.mu.Unlock()
	call.once.Do(func() {
		close(call.done)
	})
}

func (d *chatDeduplicator) pruneOldLocked() {
	now := time.Now()
	for k, v := range d.cache {
		if now.After(v.expiresAt) {
			delete(d.cache, k)
		}
	}
	if len(d.cache) > d.maxEntries {
		for k := range d.cache {
			delete(d.cache, k)
			if len(d.cache) <= d.maxEntries {
				break
			}
		}
	}
}
