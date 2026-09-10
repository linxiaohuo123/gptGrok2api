package provider

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
)

// 回归：图片下载首读被 4 MiB 硬截断，且不报错（图片 > 4 MiB 时返回半张图）。
func TestProofImageDownloadTruncatesAt4MiB(t *testing.T) {
	const size = 5 << 20
	body := bytes.Repeat([]byte{0x89}, size)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"image/png"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
	image := &OpenAIImage{}
	raw, mime, err := image.readImageDownloadResponse(context.Background(), accounts.Account{}, response)
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if len(raw) == size {
		t.Logf("完整返回 %d 字节，未截断", len(raw))
		return
	}
	t.Fatalf("静默截断确认：请求 %d 字节，返回 %d 字节，mime=%s，err=nil", size, len(raw), mime)
}

// 回归：图片下载完全不看 HTTP 状态码 —— 403 的 HTML 错误页被当作图片字节返回。
func TestProofImageDownloadAccepts403HTML(t *testing.T) {
	html := []byte("<html><head><title>403 Forbidden</title></head><body>Cloudflare challenge</body></html>")
	response := &http.Response{
		StatusCode: http.StatusForbidden,
		Header:     http.Header{"Content-Type": []string{"text/html"}},
		Body:       io.NopCloser(bytes.NewReader(html)),
	}
	image := &OpenAIImage{}
	raw, mime, err := image.readImageDownloadResponse(context.Background(), accounts.Account{}, response)
	if err != nil {
		t.Logf("返回错误（说明有状态码校验）: %v", err)
		return
	}
	t.Fatalf("确认：403 错误页被当作图片返回 —— %d 字节，mime=%q，err=nil；调用方会把它写成 .png 并写入图库", len(raw), mime)
}

// 设备/会话 ID 必须稳定：同一账号连续多次请求不得变换设备指纹。
// 账号自带 fp 时仍然优先使用账号里的值。
func TestDeviceIDIsStableAcrossRequests(t *testing.T) {
	account := map[string]any{"access_token": "token-without-fingerprint"}
	first := buildOpenAIFingerprint(account)
	for i := 0; i < 5; i++ {
		next := buildOpenAIFingerprint(account)
		if next.DeviceID != first.DeviceID || next.SessionID != first.SessionID {
			t.Fatalf("第 %d 次调用指纹变了: device=%s→%s session=%s→%s", i+1, first.DeviceID, next.DeviceID, first.SessionID, next.SessionID)
		}
	}

	// 账号自带指纹仍应覆盖默认值（导入账号的既有行为不能丢）。
	custom := buildOpenAIFingerprint(map[string]any{"access_token": "t", "oai_device_id": "device-from-account"})
	if custom.DeviceID != "device-from-account" {
		t.Fatalf("账号自带 oai_device_id 未被采用: %s", custom.DeviceID)
	}
}

// 回归：上游 detail 错误一律硬编码成 400，账号池的 401/429 处置逻辑收不到真实状态码。
func TestProofDetailErrorHardcodedAs400(t *testing.T) {
	state := &openAIChatState{}
	for _, detail := range []string{"Unauthorized", "Too many requests, please slow down", "invalid_api_key"} {
		_, err := state.event(map[string]any{"detail": detail})
		upstream, ok := err.(interface{ Error() string })
		if !ok || err == nil {
			t.Fatalf("detail=%q 未产生错误", detail)
		}
		status := upstreamStatus(err)
		if status != 400 {
			t.Logf("detail=%q → status=%d", detail, status)
			continue
		}
		t.Fatalf("确认：detail=%q（语义应为 401/429）被硬编码为 status=400 —— 账号池按 401/429 分类的逻辑失效；%v", detail, upstream)
	}
}

func upstreamStatus(err error) int {
	if typed, ok := err.(*protocol.UpstreamError); ok {
		return typed.Status
	}
	return -1
}
