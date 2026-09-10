package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// 回归：redactProxyError 对含 "scheme://...@" 的输入永不返回（纯 CPU 死循环）。
func TestProofRedactProxyErrorHangs(t *testing.T) {
	done := make(chan string, 1)
	go func() {
		done <- redactProxyError(`dial tcp: socks5://user:pass@10.0.0.1:1080: connect: connection refused`)
	}()
	select {
	case got := <-done:
		t.Logf("正常返回: %s", got)
	case <-time.After(2 * time.Second):
		t.Fatal("redactProxyError 2 秒未返回 —— 死循环确认")
	}
}

// 回归：/status 与 /{id}/execute 并发时，对 schedulerLeases 内层 map 的读写竞争。
func TestProofSchedulerLeasesRace(t *testing.T) {
	t.Setenv("GO_IMAGE_SCHEDULER_KEY", "internal-secret")
	s := &Server{schedulerLeases: map[string]map[string]any{}}

	reserve := httptest.NewRequest(http.MethodPost, "/internal/image-scheduler/reserve", strings.NewReader(`{"model":"gpt-image-2"}`))
	reserve.Header.Set("Content-Type", "application/json")
	reserve.Header.Set("X-Image-Scheduler-Key", "internal-secret")
	recorder := httptest.NewRecorder()
	s.internalImageScheduler(recorder, reserve)
	if recorder.Code != http.StatusOK {
		t.Fatalf("reserve 失败: %d %s", recorder.Code, recorder.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	id := stringValue(payload["reservation_id"])
	if id == "" {
		t.Fatalf("reservation_id 为空: %s", recorder.Body.String())
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				req := httptest.NewRequest(http.MethodGet, "/internal/image-scheduler/status", nil)
				req.Header.Set("X-Image-Scheduler-Key", "internal-secret")
				s.internalImageScheduler(httptest.NewRecorder(), req)
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				req := httptest.NewRequest(http.MethodPost, "/internal/image-scheduler/"+id+"/execute", strings.NewReader(`{}`))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-Image-Scheduler-Key", "internal-secret")
				s.internalImageScheduler(httptest.NewRecorder(), req)
			}
		}()
	}
	wg.Wait()
}

// 回归：去重器在成功路径上把 call.err 从 nil 改写成 "upstream request terminated"。
//
// owner 的 defer 在 finish* 之后执行，而 finish* 已经 close(done) 发布了结果。
// 等待方从 <-done 醒来后读到的 err 若是这个假错误，一次成功的上游请求会被
// 回成 5xx；且这次改写发生在发布之后，读写之间没有任何同步。
func TestDedupeSuccessIsNotOverwrittenByOwnerDefer(t *testing.T) {
	d := newChatDeduplicator(60*time.Second, true)
	call, owner := d.getOrStart("dedupe-success-key")
	if !owner {
		t.Fatal("首个调用者必须是 owner")
	}

	d.finishResponse("dedupe-success-key", call, map[string]any{"id": "chatcmpl-1"}, nil)

	// 模拟 handler 返回时执行的 defer
	d.cancelIfInflight("dedupe-success-key", call, errors.New("upstream request terminated"))

	if call.err != nil {
		t.Fatalf("成功结果被 owner 的 defer 改写：%v", call.err)
	}
	if call.response == nil {
		t.Fatal("成功响应不应被清空")
	}
}

// owner 中途退出（从未 finish）时，等待方必须收到错误，而不是永久阻塞在 <-done。
func TestDedupeOwnerAbortStillReportsError(t *testing.T) {
	d := newChatDeduplicator(60*time.Second, true)
	call, _ := d.getOrStart("dedupe-abort-key")

	d.cancelIfInflight("dedupe-abort-key", call, errors.New("client disconnected"))

	if call.err == nil {
		t.Fatal("owner 未 finish 时，等待方必须拿到错误而不是永久挂住")
	}
	select {
	case <-call.done:
	default:
		t.Fatal("done 必须被关闭以唤醒等待方")
	}
}

// 失败路径同样不能被改写：上游报 429，等待方必须看到 429 而不是被覆盖成通用错误。
func TestDedupeFailureKeepsOriginalError(t *testing.T) {
	d := newChatDeduplicator(60*time.Second, true)
	call, _ := d.getOrStart("dedupe-failure-key")

	upstream := errors.New("rate limit exceeded")
	d.finishResponse("dedupe-failure-key", call, nil, upstream)
	d.cancelIfInflight("dedupe-failure-key", call, errors.New("upstream request terminated"))

	if call.err != upstream {
		t.Fatalf("原始错误被改写：期望 %v，实际 %v", upstream, call.err)
	}
}
