package agentidentity

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// makeTestJWT 造一个只关心中段的 JWT——jwtPayload 不验签，只解 payload。
func makeTestJWT(t *testing.T, accountID, userID string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"sub":                         userID,
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": accountID},
	})
	if err != nil {
		t.Fatal(err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

// 回归：注册请求不得在锁内执行。
//
// Ensure 曾在整段（生成密钥 + 最长 30s 的注册请求）外面罩着 s.mu，而 s.mu 是全局锁：
// Summary / AuthJSON 都会被它挡住，而全量导出是 N 个账号串行调用本函数——
// 那意味着管理面冻结 N×30 秒。
func TestEnsureDoesNotHoldLockDuringRegister(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release // 卡住注册，模拟上游迟迟不回
		_, _ = w.Write([]byte(`{"agent_runtime_id":"rt-1"}`))
	}))
	defer server.Close()
	defer close(release)

	store := NewStore(t.TempDir(), server.URL, nil)
	account := map[string]any{"access_token": makeTestJWT(t, "acc-1", "user-1")}

	go func() {
		_, _ = store.Ensure(context.Background(), account)
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Skip("注册未启动，测试环境不支持")
	}

	// 关键断言：注册还挂着的时候，Summary（同一把锁）必须立刻返回。
	done := make(chan struct{})
	go func() {
		_, _ = store.Summary()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("注册在途时 Summary 被阻塞 —— 锁跨了网络 I/O，全量导出会冻结管理面")
	}
}

// 回归：并发 Ensure 不同账号时，回填必须重新读盘再追加。
//
// 网络段移到锁外之后，每个调用手里都攥着一份**注册前**读到的快照。若回填时
// 直接 append + save（save 是全量替换），后写的会用旧快照覆盖先写的——
// N 个账号并发导出，最后只剩一个的身份。
//
// 用不同账号而非同一账号：同一账号无论有没有 double-check 都只剩 1 条
// （后者靠覆盖、前者靠提前返回），区分不出来。
func TestConcurrentEnsureKeepsEveryAccount(t *testing.T) {
	const workers = 5
	var arrived sync.WaitGroup
	arrived.Add(workers)
	release := make(chan struct{})

	var counter int
	var counterMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		arrived.Done()
		<-release // 等 N 个调用全部走过 fast-path 再放行，制造"快照全部过期"的时序
		counterMu.Lock()
		counter++
		id := counter
		counterMu.Unlock()
		_, _ = fmt.Fprintf(w, `{"agent_runtime_id":"rt-%d"}`, id)
	}))
	defer server.Close()
	go func() {
		arrived.Wait()
		close(release)
	}()

	store := NewStore(t.TempDir(), server.URL, nil)

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		account := map[string]any{"access_token": makeTestJWT(t, fmt.Sprintf("acc-%d", i), fmt.Sprintf("user-%d", i))}
		wg.Add(1)
		go func(account map[string]any) {
			defer wg.Done()
			if _, err := store.Ensure(context.Background(), account); err != nil {
				t.Errorf("并发 Ensure 失败：%v", err)
			}
		}(account)
	}
	wg.Wait()

	items, err := store.Summary()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != workers {
		t.Fatalf("%d 个账号并发导出后只剩 %d 条身份 —— 回填时用了过期的快照，先写的被覆盖",
			workers, len(items))
	}
}
