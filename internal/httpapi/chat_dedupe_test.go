package httpapi

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/protocol"
)

func TestChatCacheKeyDeterminism(t *testing.T) {
	req1 := protocol.ChatRequest{
		Model: "gpt-4o",
		Messages: []protocol.Message{
			{Role: "user", Content: "hello"},
		},
	}
	req2 := protocol.ChatRequest{
		Model: "gpt-4o",
		Messages: []protocol.Message{
			{Role: "user", Content: "hello"},
		},
	}
	reqDiff := protocol.ChatRequest{
		Model: "gpt-4o-mini",
		Messages: []protocol.Message{
			{Role: "user", Content: "hello"},
		},
	}

	k1 := chatCacheKey(req1)
	k2 := chatCacheKey(req2)
	kDiff := chatCacheKey(reqDiff)

	if k1 != k2 {
		t.Fatalf("expected identical keys for identical requests, got %q vs %q", k1, k2)
	}
	if k1 == kDiff {
		t.Fatalf("expected different keys for different models")
	}
}

func TestChatDeduplicatorCoalesceAndCache(t *testing.T) {
	d := newChatDeduplicator(100*time.Millisecond, true)
	key := "test-key-1"

	call1, owner1 := d.getOrStart(key)
	if !owner1 {
		t.Fatalf("first caller must be owner")
	}

	call2, owner2 := d.getOrStart(key)
	if owner2 {
		t.Fatalf("second concurrent caller must NOT be owner")
	}
	if call1 != call2 {
		t.Fatalf("second caller must share the same inflight call")
	}

	var wg sync.WaitGroup
	wg.Add(1)
	var secondResult map[string]any
	var secondErr error

	go func() {
		defer wg.Done()
		<-call2.done
		secondResult = call2.response
		secondErr = call2.err
	}()

	mockResp := map[string]any{"id": "test-123", "content": "mock answer"}
	d.finishResponse(key, call1, mockResp, nil)
	wg.Wait()

	if secondErr != nil {
		t.Fatalf("unexpected error on second caller: %v", secondErr)
	}
	if secondResult["content"] != "mock answer" {
		t.Fatalf("second caller did not receive correct payload: %+v", secondResult)
	}

	// Immediate follow-up should hit the short cache
	cached, hit := d.getCachedResponse(key)
	if !hit || cached["content"] != "mock answer" {
		t.Fatalf("expected cache hit, got hit=%v, val=%+v", hit, cached)
	}

	// After TTL, cache expires
	time.Sleep(120 * time.Millisecond)
	_, hitAfter := d.getCachedResponse(key)
	if hitAfter {
		t.Fatalf("expected cache to expire after TTL")
	}
}

func TestChatDeduplicatorCancelIfInflight(t *testing.T) {
	d := newChatDeduplicator(time.Second, true)
	key := "test-key-cancel"

	call1, owner1 := d.getOrStart(key)
	if !owner1 {
		t.Fatalf("expected owner")
	}

	call2, owner2 := d.getOrStart(key)
	if owner2 {
		t.Fatalf("expected non-owner")
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-call2.done
	}()

	d.cancelIfInflight(key, call1, errors.New("upstream failed"))
	wg.Wait()

	if call2.err == nil || call2.err.Error() != "upstream failed" {
		t.Fatalf("expected waiter to receive cancel error, got %v", call2.err)
	}

	// Ensure in-flight map is cleared so new request can start cleanly
	call3, owner3 := d.getOrStart(key)
	if !owner3 {
		t.Fatalf("after cancel, new caller should become owner")
	}
	d.finishResponse(key, call3, nil, errors.New("aborted"))
}

func TestChatDeduplicatorConcurrentRace(t *testing.T) {
	d := newChatDeduplicator(time.Second, true)
	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)

	key := "concurrent-test"
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			call, owner := d.getOrStart(key)
			if owner {
				time.Sleep(10 * time.Millisecond)
				d.finishResponse(key, call, map[string]any{"idx": idx}, nil)
			} else {
				<-call.done
			}
		}(i)
	}
	wg.Wait()
}
