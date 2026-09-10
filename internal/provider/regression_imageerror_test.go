package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
)

// 回归：图片创建流的 SSE 帧里，上游错误必须还原成 *protocol.UpstreamError。
//
// 此前 start() 只收集图片引用、从不读 detail/error，于是审核拦截（400）与
// 凭据失效（401）被降级成 "no conversation id" 这句普通错误，httpapi 的
// upstreamStatus 把它兜底成 502——而 502 在默认重试码表内，于是请求域错误
// 触发跨账号轮换，并给健康账号累计失败、打入 Cooldown。
//
// 对话链路一直有这个还原（openai_chat.go 的 upstreamErrorFromFrame），
// 图片链路漏了；同一仓库的不对称即是缺陷证据。
func TestImageStreamRestoresUpstreamErrorFromSSEFrame(t *testing.T) {
	cases := []struct {
		name       string
		frame      string
		wantStatus int
	}{
		{"审核拦截", `{"detail": "Your request was rejected as a result of our safety system"}`, http.StatusBadRequest},
		{"凭据失效", `{"detail": "Unauthorized"}`, http.StatusUnauthorized},
		{"限流", `{"detail": "rate limit exceeded"}`, http.StatusTooManyRequests},
		{"错误对象", `{"error": {"message": "boom", "type": "rate_limit_error"}}`, http.StatusTooManyRequests},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				body := "data: " + tc.frame + "\n\n"
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader(body)),
					Request:    request,
				}, nil
			})}
			image := NewOpenAIImage("https://chatgpt.invalid", client, nil, 5*time.Second)

			_, _, err := image.start(context.Background(), accounts.Account{Token: "account-token"},
				openAIRequirements{}, "conduit-token", "prompt", "gpt-image-2", "1024x1024", "auto", nil)
			if err == nil {
				t.Fatal("SSE 帧里的上游错误必须被还原，而不是被吞掉")
			}
			var upstream *protocol.UpstreamError
			if !errors.As(err, &upstream) {
				t.Fatalf("期望 *protocol.UpstreamError，实际 %T: %v —— 普通错误会被 upstreamStatus 兜底成 502", err, err)
			}
			if upstream.Status != tc.wantStatus {
				t.Fatalf("状态码还原为 %d，期望 %d", upstream.Status, tc.wantStatus)
			}
		})
	}
}

// 正常的 SSE 帧不应被误判成错误：没有 detail/error 的帧照常收集引用。
func TestImageStreamIgnoresNonErrorFrames(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := "data: {\"conversation_id\": \"conv-123\"}\n\n" +
			"data: {\"v\": \"file-service://file-abc\"}\n\n"
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    request,
		}, nil
	})}
	image := NewOpenAIImage("https://chatgpt.invalid", client, nil, 5*time.Second)

	conversationID, fileIDs, err := image.start(context.Background(), accounts.Account{Token: "account-token"},
		openAIRequirements{}, "conduit-token", "prompt", "gpt-image-2", "1024x1024", "auto", nil)
	if err != nil {
		t.Fatalf("正常帧不应报错：%v", err)
	}
	if conversationID != "conv-123" {
		t.Fatalf("conversation id 未收集到：%q", conversationID)
	}
	if len(fileIDs) != 1 || fileIDs[0] != "file-abc" {
		t.Fatalf("file id 未收集到：%#v", fileIDs)
	}
}

// upstreamErrorFromFrame 是两条链路共用的还原逻辑，直接钉住它的契约。
func TestUpstreamErrorFromFrameContract(t *testing.T) {
	if err := upstreamErrorFromFrame(map[string]any{"v": "hello"}); err != nil {
		t.Fatalf("无错误的帧必须返回 nil，实际 %v", err)
	}
	if err := upstreamErrorFromFrame("not-an-object"); err != nil {
		t.Fatalf("非对象载荷必须返回 nil，实际 %v", err)
	}
	err := upstreamErrorFromFrame(map[string]any{"detail": "Unauthorized"})
	var upstream *protocol.UpstreamError
	if !errors.As(err, &upstream) || upstream.Status != http.StatusUnauthorized {
		t.Fatalf("detail 必须还原成 401，实际 %v", err)
	}
}
