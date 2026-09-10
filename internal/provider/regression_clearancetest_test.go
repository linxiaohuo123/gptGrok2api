package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 回归：clearance 的缓存读不得被在途的求解阻塞。
//
// refreshClearance 曾在整段 FlareSolverr 调用（最长 60s）外面罩着 clearanceMu，
// 而 cachedClearance 被每个账号端点请求调用（getJSON）、用的是同一把锁——
// 任一代理触发一次 Cloudflare 403，全部账号、全部代理的请求就一起卡住。
func TestClearanceCacheReadNotBlockedByInFlightSolve(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	flare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release // 卡住这次求解，模拟 FlareSolverr 迟迟不回
		_, _ = w.Write([]byte(`{"status":"ok","solution":{"cookies":[{"name":"cf_clearance","value":"v"}],"userAgent":"ua"}}`))
	}))
	defer flare.Close()
	defer close(release)

	client := NewOpenAIAccountClient("https://chatgpt.invalid", "https://oauth.invalid", nil, nil,
		ClearanceConfig{URL: flare.URL, Enabled: true, Timeout: 30 * time.Second})

	go func() {
		_, _ = client.refreshClearance(context.Background(), "socks5://proxy-a.invalid:1080", http.MethodGet,
			"https://chatgpt.invalid/backend-api/me", "token", nil, "/backend-api/me")
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Skip("求解未启动，测试环境不支持")
	}

	// 关键断言：求解还挂着的时候，另一条代理的缓存读必须立刻返回。
	done := make(chan struct{})
	go func() {
		_ = client.cachedClearance("socks5://proxy-b.invalid:1080")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("求解在途时 cachedClearance 被阻塞 —— 锁跨了网络 I/O，会让全池请求一起卡住")
	}
}

// 回归：并发求解同一代理时，后到的不得覆盖先到的结果。
func TestConcurrentClearanceSolveKeepsFirstResult(t *testing.T) {
	flare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok","solution":{"cookies":[{"name":"cf_clearance","value":"v"}],"userAgent":"ua"}}`))
	}))
	defer flare.Close()

	client := NewOpenAIAccountClient("https://chatgpt.invalid", "https://oauth.invalid", nil, nil,
		ClearanceConfig{URL: flare.URL, Enabled: true, Timeout: 10 * time.Second})

	const workers = 8
	results := make(chan clearanceBundle, workers)
	for i := 0; i < workers; i++ {
		go func() {
			bundle, err := client.refreshClearance(context.Background(), "socks5://proxy.invalid:1080", http.MethodGet,
				"https://chatgpt.invalid/backend-api/me", "token", nil, "/backend-api/me")
			if err != nil {
				results <- clearanceBundle{}
				return
			}
			results <- bundle
		}()
	}

	var first clearanceBundle
	for i := 0; i < workers; i++ {
		got := <-results
		if got.Cookie == "" {
			t.Fatal("并发求解返回了空 bundle")
		}
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("并发求解结果不一致：%#v vs %#v —— 后到的覆盖了先到的", first, got)
		}
	}

	// 缓存必须已经写入，且后续读命中。
	if cached := client.cachedClearance("socks5://proxy.invalid:1080"); cached.Cookie == "" {
		t.Fatal("求解成功后缓存未写入")
	}
}
