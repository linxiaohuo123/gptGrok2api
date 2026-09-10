package proxy

import (
	"context"
	"testing"
)

// 回归：非法代理串绝不能被降级成"直连"。
//
// normalizeURL 曾用 "" 同时表示"解析失败"和"直连"，而 cache[""] 就是
// http.DefaultTransport。于是一个手误的代理串（订阅导出常见的
// ip:port:user:pass、密码含裸 %）会让请求从服务器真实 IP 出网，
// 账号与机房 IP 被上游关联，且全程没有任何告警。
func TestNormalizeURLRejectsInsteadOfFallingBackToDirect(t *testing.T) {
	for _, invalid := range []string{
		"1.2.3.4:8080:user:pass",
		"47.1.2.3:8080:u:p",
		"http://user:pa%ss@host:1",
		"http://",
		"http:///path",
		"%%%%",
	} {
		normalized, err := normalizeURL(invalid)
		if err == nil {
			t.Fatalf("%q 应返回 error，实际得到 (%q, nil)——非法串不能与直连共用零值", invalid, normalized)
		}
		if normalized != "" {
			t.Fatalf("%q 出错时必须返回空串，实际 %q", invalid, normalized)
		}
	}
}

// 空串与 "direct" 是**显式**直连，必须返回 ("", nil) 而不是 error。
func TestNormalizeURLExplicitDirect(t *testing.T) {
	for _, direct := range []string{"", "  ", "direct", "DIRECT", "Direct"} {
		normalized, err := normalizeURL(direct)
		if err != nil {
			t.Fatalf("%q 是显式直连，不应报错：%v", direct, err)
		}
		if normalized != "" {
			t.Fatalf("%q 应归一化为空串（直连），实际 %q", direct, normalized)
		}
	}
}

// 合法代理串正常归一化，且缺 scheme 时补 http://。
func TestNormalizeURLAcceptsValidProxies(t *testing.T) {
	cases := map[string]string{
		"1.2.3.4:8080":                  "http://1.2.3.4:8080",
		"http://user:pass@1.2.3.4:8080": "http://user:pass@1.2.3.4:8080",
		"socks5://1.2.3.4:1080":         "socks5://1.2.3.4:1080",
		"http://[::1]:8080":             "http://[::1]:8080",
	}
	for input, want := range cases {
		normalized, err := normalizeURL(input)
		if err != nil {
			t.Fatalf("%q 应可解析：%v", input, err)
		}
		if normalized != want {
			t.Fatalf("%q 归一化为 %q，期望 %q", input, normalized, want)
		}
	}
}

// 非法串在 forProxy 处必须报错，绝不允许命中 cache[""] 拿到直连传输。
func TestForProxyRejectsInvalidInsteadOfDirect(t *testing.T) {
	transport := NewTransport(nil)

	direct, err := transport.forProxy("")
	if err != nil {
		t.Fatalf("空串是显式直连，不应报错：%v", err)
	}
	if direct == nil {
		t.Fatal("空串应返回直连传输")
	}

	invalid, err := transport.forProxy("1.2.3.4:8080:user:pass")
	if err == nil {
		t.Fatalf("非法代理串必须报错，实际返回了 %T —— 这正是真实 IP 泄漏的入口", invalid)
	}
	if invalid != nil {
		t.Fatal("报错时必须返回 nil 传输")
	}
}

// 账号声明了非法代理时，必须显式失败，而不是穿透到全局池换一个出口。
//
// 穿透的后果是 A 账号的请求从 B 账号的出口出去——身份串号，
// 上游侧两个账号被关联到同一代理。
func TestAccountProxyInvalidDoesNotFallThroughToPool(t *testing.T) {
	manager := NewManager("http://global-pool.invalid:8080", nil)
	fields := map[string]any{"proxy": "1.2.3.4:8080:user:pass"}

	lease, err := manager.AcquireImageContext(context.Background(), fields)
	if err != nil {
		t.Fatalf("AcquireImageContext 不应返回 error：%v", err)
	}
	if lease.Source != "unavailable" {
		t.Fatalf("账号代理非法时必须返回 unavailable 哨兵，实际 Source=%q URL=%q", lease.Source, lease.URL)
	}
	if lease.URL != "" {
		t.Fatalf("unavailable 租约不得携带出口 URL，实际 %q", lease.URL)
	}
}

// 账号显式写 "direct" 时必须直连，不得穿透到全局池。
func TestAccountExplicitDirectDoesNotFallThroughToPool(t *testing.T) {
	manager := NewManager("http://global-pool.invalid:8080", nil)
	fields := map[string]any{"proxy": "direct"}

	if resolved := manager.Resolve(fields, false); resolved != "" {
		t.Fatalf("账号显式 direct 必须解析为空串（直连），实际 %q", resolved)
	}
}

// Resolve 对非法账号代理必须原样返回，让下游 forProxy 拒绝它——而不是返回 ""。
func TestResolveReturnsInvalidProxyVerbatim(t *testing.T) {
	manager := NewManager("", nil)
	fields := map[string]any{"proxy": "1.2.3.4:8080:user:pass"}

	resolved := manager.Resolve(fields, false)
	if resolved == "" {
		t.Fatal("非法账号代理不能解析成空串——空串语义是直连")
	}
	if _, err := NewTransport(nil).forProxy(resolved); err == nil {
		t.Fatalf("Resolve 返回的 %q 必须能被 forProxy 拒绝", resolved)
	}
}
