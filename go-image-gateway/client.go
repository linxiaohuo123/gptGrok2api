// [INPUT]: 仅标准库（net、net/http、syscall）
// [OUTPUT]: newBackendClient、newRemoteFetchClient、isBlockedRemoteAddress、
//           fetchRemoteImage——出网客户端与 SSRF 防护
// [POS]: 网关所有出网流量的构造点。两类流量必须分开：调调度器/主程序用的是
//         后端 client（长超时），抓用户提供的 image_url 用的是受限 client（短超时 + 私网拦截）。
// [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
)

const (
	// remoteImageTimeout 限制单张远程图片的抓取总时长。
	remoteImageTimeout = 30 * time.Second
	// maxRemoteImageBytes 是远程图片的体积上限，与主程序侧 50MiB 的口径一致。
	maxRemoteImageBytes = 50 << 20
	// maxRemoteImageRedirects 限制重定向跳数。每一跳都会重新过 Control 校验，
	// 所以限制跳数只是防止被无限重定向拖住。
	maxRemoteImageRedirects = 3
	// schedulerReleaseTimeout 限制一次租约释放的时长。释放是尽力而为的收尾动作，
	// 失败也只是让主程序侧的租约多留一会儿，不值得让 worker 为它挂住。
	schedulerReleaseTimeout = 5 * time.Second
)

// newBackendClient 构造访问调度器与主程序的 client。
//
// 这里**刻意不设 Client.Timeout**：生图请求本身可能跑满 BackendTimeout（默认 900s），
// 全局超时会把它腰斩。超时由每个调用点的 ctx 施加，本函数只负责让**连接建立**
// 阶段不可能无限挂起——缺了 DialContext 与 TLSHandshakeTimeout 时，一个 accept
// 后不回包的连接就能永久占住一个 worker，而 MaxConnsPerHost == Workers，
// 挂满 Workers 次即整网关死亡。
func newBackendClient(cfg config) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
			MaxIdleConns:          cfg.Workers * 2,
			MaxIdleConnsPerHost:   cfg.Workers,
			MaxConnsPerHost:       cfg.Workers,
			IdleConnTimeout:       90 * time.Second,
		},
	}
}

// newRemoteFetchClient 构造抓取用户提供 URL 的 client。
//
// 这个 URL 完全来自请求体，因此必须同时防两件事：
//   - 无限挂起：短超时 + 各阶段独立超时；
//   - SSRF：不校验的话可以打到 169.254.169.254（云元数据）、127.0.0.1、内网段，
//     而响应体会被转成 data URL 存进任务结果并读回，等于完整回显。
//
// 拦截放在 Dialer.Control 而不是只校验 URL 主机名：Control 拿到的是**已解析**的
// 目标地址，每次实际建连前都会跑一遍，因此连 DNS rebinding 也一并拦住。
func newRemoteFetchClient() *http.Client {
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil {
				return fmt.Errorf("unresolvable remote address %q", host)
			}
			if isBlockedRemoteAddress(ip) {
				return fmt.Errorf("blocked remote address %s", ip)
			}
			return nil
		},
	}
	return &http.Client{
		Timeout: remoteImageTimeout,
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 15 * time.Second,
			MaxIdleConns:          8,
			IdleConnTimeout:       30 * time.Second,
		},
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= maxRemoteImageRedirects {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
}

// isBlockedRemoteAddress 判定目标地址是否落在回环、私网、链路本地或保留段。
//
// 169.254.0.0/16 由 IsLinkLocalUnicast 覆盖——那正是云元数据端点所在段。
func isBlockedRemoteAddress(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return true
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	switch {
	case ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127: // 100.64.0.0/10 CGNAT
		return true
	case ip4[0] == 192 && ip4[1] == 0 && ip4[2] == 0: // 192.0.0.0/24 IETF 保留
		return true
	case ip4[0] == 198 && (ip4[1] == 18 || ip4[1] == 19): // 198.18.0.0/15 基准测试
		return true
	case ip4[0] >= 240: // 240.0.0.0/4 保留（含广播地址）
		return true
	}
	return false
}

// fetchRemoteImage 抓取用户提交的远程图片，返回字节与归一化后的 MIME。
func fetchRemoteImage(ctx context.Context, client *http.Client, raw string) ([]byte, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, "", err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	if response.StatusCode >= 400 {
		return nil, "", fmt.Errorf("remote image returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxRemoteImageBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > maxRemoteImageBytes {
		return nil, "", errors.New("remote image exceeds size limit")
	}
	mime := strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0])
	return data, mime, nil
}
