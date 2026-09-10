package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 回归：后端 client 的连接建立阶段不能无限挂起。
//
// 原先的构造是裸 &http.Transport{}：DialContext 落到 zeroDialer（无超时），
// ResponseHeaderTimeout / TLSHandshakeTimeout 全为 0。配合 MaxConnsPerHost == Workers，
// 主程序侧只要 accept 后不回包，一次挂起就永久占住一个 worker，挂满即整网关死亡。
func TestBackendClientCannotHangDuringConnect(t *testing.T) {
	client := newBackendClient(config{Workers: 4})
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("期望 *http.Transport，实际 %T", client.Transport)
	}
	if transport.DialContext == nil {
		t.Fatal("缺少 DialContext：连接建立阶段没有任何超时，accept 后不回包即永久挂起 worker")
	}
	if transport.TLSHandshakeTimeout <= 0 {
		t.Fatal("缺少 TLSHandshakeTimeout")
	}
	if transport.MaxConnsPerHost <= 0 {
		t.Fatal("缺少 MaxConnsPerHost")
	}
	// Client.Timeout 必须保持 0：生图请求可以跑满 BackendTimeout（默认 900s），
	// 全局超时会把它腰斩。超时由每个调用点的 ctx 施加。
	if client.Timeout != 0 {
		t.Fatalf("Client.Timeout=%v 会腰斩长生图请求，该值必须保持 0", client.Timeout)
	}
}

// 回归：抓取用户 image_url 的 client 必须短超时，且不得用于调调度器。
func TestRemoteFetchClientIsBounded(t *testing.T) {
	client := newRemoteFetchClient()
	if client.Timeout <= 0 {
		t.Fatal("抓取用户 URL 的 client 必须有全局超时")
	}
	if client.Timeout > 60*time.Second {
		t.Fatalf("抓取超时 %v 过长，黑洞地址会长时间占住 handler", client.Timeout)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("期望 *http.Transport，实际 %T", client.Transport)
	}
	if transport.ResponseHeaderTimeout <= 0 {
		t.Fatal("缺少 ResponseHeaderTimeout")
	}
}

// 回归：SSRF 防护必须拦住内网、回环与云元数据地址。
func TestIsBlockedRemoteAddress(t *testing.T) {
	blocked := []string{
		"127.0.0.1",       // 回环
		"127.0.0.53",      // 回环段内任意地址
		"::1",             // IPv6 回环
		"169.254.169.254", // 云元数据端点 —— 这条是 SSRF 的首要目标
		"169.254.1.1",     // 链路本地
		"fe80::1",         // IPv6 链路本地
		"10.0.0.1",        // RFC1918
		"172.16.5.4",      // RFC1918
		"192.168.1.1",     // RFC1918
		"fc00::1",         // RFC4193 唯一本地
		"fd12:3456::1",    // RFC4193
		"100.64.0.1",      // CGNAT
		"100.127.255.254", // CGNAT 上界
		"192.0.0.1",       // IETF 保留
		"198.18.0.1",      // 基准测试段
		"240.0.0.1",       // 保留
		"255.255.255.255", // 广播
		"0.0.0.0",         // 未指定
		"224.0.0.1",       // 组播
	}
	for _, raw := range blocked {
		ip := net.ParseIP(raw)
		if ip == nil {
			t.Fatalf("测试用例本身有误，无法解析 %q", raw)
		}
		if !isBlockedRemoteAddress(ip) {
			t.Errorf("%s 必须被拦截", raw)
		}
	}

	allowed := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:4700::1111"}
	for _, raw := range allowed {
		ip := net.ParseIP(raw)
		if ip == nil {
			t.Fatalf("测试用例本身有误，无法解析 %q", raw)
		}
		if isBlockedRemoteAddress(ip) {
			t.Errorf("%s 是公网地址，不应被拦截", raw)
		}
	}
}

// 回归：SSRF 防护必须真的接在出网链路上，而不只是一个没人调用的纯函数。
//
// httptest 的服务端监听在 127.0.0.1，因此这一次抓取必须失败——
// 若它成功了，说明 Control 校验没有生效，攻击者可以拿同样的方式读内网与云元数据。
func TestRemoteFetchClientBlocksLoopbackInPractice(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("pretend-image-bytes"))
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, _, err := fetchRemoteImage(ctx, newRemoteFetchClient(), server.URL)
	if err == nil {
		t.Fatal("抓取回环地址必须失败——SSRF 防护未接在出网链路上")
	}
	if !strings.Contains(err.Error(), "blocked remote address") {
		t.Fatalf("期望被地址校验拦下，实际错误：%v", err)
	}
}

// 抓取逻辑本身（大小上限、状态码、MIME 归一化）用不受地址限制的 client 验证，
// 把「取数逻辑」与「SSRF 防护」两件事分开测。
func TestFetchRemoteImageEnforcesLimits(t *testing.T) {
	oversized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		chunk := make([]byte, 1<<20)
		for i := 0; i < (maxRemoteImageBytes>>20)+2; i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer oversized.Close()

	ctx := context.Background()
	plain := &http.Client{}

	if _, _, err := fetchRemoteImage(ctx, plain, oversized.URL); err == nil {
		t.Fatal("超过体积上限的远程图片必须被拒绝，否则会被读进内存并转成 base64")
	}

	erroring := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer erroring.Close()
	if _, _, err := fetchRemoteImage(ctx, plain, erroring.URL); err == nil {
		t.Fatal("上游非 2xx 必须报错")
	}

	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg; charset=binary")
		_, _ = w.Write([]byte("jpeg-bytes"))
	}))
	defer ok.Close()
	data, mime, err := fetchRemoteImage(ctx, plain, ok.URL)
	if err != nil {
		t.Fatalf("正常抓取不应报错：%v", err)
	}
	if string(data) != "jpeg-bytes" {
		t.Fatalf("响应体读取有误：%q", data)
	}
	if mime != "image/jpeg" {
		t.Fatalf("MIME 应去掉参数部分，实际 %q", mime)
	}
}
