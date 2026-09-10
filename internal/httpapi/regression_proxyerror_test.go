package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
)

// 决定性实验：真实测量“带凭据代理”的失败错误串里，是否同时出现 scheme:// 与 @。
// 只有同时出现两者，redactProxyError 才会死循环。
func TestProofProxyErrorTextContainsSchemeAndAt(t *testing.T) {
	cases := []string{
		"http://user:pass@127.0.0.1:1",
		"http://user:pass@no-such-host.invalid:8080",
		"socks5://user:pass@127.0.0.1:1",
		"socks5h://user:pass@no-such-host.invalid:1080",
		"socks4://user:pass@127.0.0.1:1",
	}
	client := &http.Client{Transport: proxyruntime.NewTransport(http.DefaultTransport), Timeout: 5 * time.Second}
	dangerous := 0
	for _, candidate := range cases {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		request, err := http.NewRequestWithContext(proxyruntime.WithURL(ctx, candidate), http.MethodGet, "https://chatgpt.com/api/auth/csrf", nil)
		if err != nil {
			cancel()
			t.Fatalf("构造请求失败: %v", err)
		}
		response, err := client.Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		cancel()
		if err == nil {
			t.Logf("%-42s → 无错误", candidate)
			continue
		}
		text := err.Error()
		hit := strings.Contains(strings.ToLower(text), "://") && strings.Contains(text, "@")
		if hit {
			dangerous++
		}
		t.Logf("%-42s → 含 scheme+@ = %v | %s", candidate, hit, text)
	}
	if dangerous > 0 {
		t.Fatalf("%d/%d 个错误串同时含 scheme:// 与 @ —— redactProxyError 在生产路径上可达", dangerous, len(cases))
	}
	t.Log("结论：以上失败模式的错误串均不同时含 scheme:// 与 @，未找到可达触发路径")
}
