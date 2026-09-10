package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// flushRecorder 记录是否有人调用过 Flush。
type flushRecorder struct {
	*httptest.ResponseRecorder
	flushed bool
}

func (f *flushRecorder) Flush() {
	f.flushed = true
	f.ResponseRecorder.Flush()
}

func captureWithChat(payload map[string]any) *responseCapture {
	recorder := &responseCapture{header: make(http.Header)}
	raw, _ := json.Marshal(payload)
	recorder.WriteHeader(http.StatusOK)
	_, _ = recorder.Write(raw)
	return recorder
}

// 回归：/v1/messages 的“流式”响应不设 Content-Type: text/event-stream，也从不 Flush。
func TestProofAnthropicStreamHasNoSSEHeaderNorFlush(t *testing.T) {
	chat := map[string]any{
		"id": "chatcmpl-proof",
		"choices": []any{map[string]any{
			"message": map[string]any{"role": "assistant", "content": "你好"},
		}},
		"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 2},
	}
	server := &Server{}
	recorder := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	server.writeAnthropicStream(recorder, captureWithChat(chat), "claude-sonnet-5")

	contentType := recorder.Header().Get("Content-Type")
	body := recorder.Body.String()
	if !strings.Contains(body, "event: message_start") {
		t.Fatalf("没有写出 SSE 事件，测试前提不成立: %q", body)
	}
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		t.Logf("Content-Type 正确: %q", contentType)
	} else {
		t.Fatalf("确认：写出了 SSE 事件但 Content-Type=%q（不是 text/event-stream），且 flushed=%v", contentType, recorder.flushed)
	}
	if recorder.flushed {
		t.Log("有过 Flush")
	} else {
		t.Fatalf("确认：全程从未 Flush，事件只能等 handler 返回后一次性到达")
	}
}

// 回归：output_item.added 的 item 必须是单个对象，且 done 必须发出——
// 包括没有 content 的工具调用项。
func TestResponseOutputEventsUseSingleItemObjects(t *testing.T) {
	cases := []struct {
		name    string
		chat    map[string]any
		items   int
		hasDone bool
	}{
		{
			name:  "文本回复",
			chat:  map[string]any{"id": "chatcmpl-1", "choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "你好"}}}},
			items: 1, hasDone: true,
		},
		{
			name: "工具调用（无 content）",
			chat: map[string]any{"id": "chatcmpl-2", "choices": []any{map[string]any{"message": map[string]any{
				"role":       "assistant",
				"tool_calls": []any{map[string]any{"id": "call_1", "function": map[string]any{"name": "get_weather", "arguments": "{}"}}},
			}}}},
			items: 1, hasDone: true,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			response := responseFromChat(testCase.chat, "gpt-5")
			recorder := httptest.NewRecorder()
			writeResponseOutputEvents(recorder, response["output"])

			added, done := 0, 0
			for _, line := range strings.Split(recorder.Body.String(), "\n") {
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				var event map[string]any
				if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) != nil {
					continue
				}
				switch stringValue(event["type"]) {
				case "response.output_item.added":
					added++
					if _, ok := event["item"].(map[string]any); !ok {
						t.Fatalf("added 的 item 不是对象: %T → %s", event["item"], line)
					}
				case "response.output_item.done":
					done++
				}
			}
			if added != testCase.items {
				t.Fatalf("added 事件 %d 条，期望 %d", added, testCase.items)
			}
			if testCase.hasDone && done != testCase.items {
				t.Fatalf("done 事件 %d 条，期望 %d（工具调用路径此前永远不发送 done）", done, testCase.items)
			}
		})
	}
}

// countingReader 统计真正从 body 里拉走的字节数。
// 注意：中间件读完会把 r.Body 还原，所以下游仍能读到内容 —— 必须数“读了多少”，不能数“剩了多少”。
type countingReader struct {
	reader io.Reader
	read   int
}

func (c *countingReader) Read(buffer []byte) (int, error) {
	n, err := c.reader.Read(buffer)
	c.read += n
	return n, err
}

// 回归：监控中间件排在鉴权之前 —— 未授权请求的 body 在鉴权前就被整读进内存。
func TestProofBodyReadBeforeAuth(t *testing.T) {
	payload := bytes.Repeat([]byte("a"), 200<<10)
	counter := &countingReader{reader: bytes.NewReader(payload)}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	request.Body = io.NopCloser(counter)
	request.ContentLength = int64(len(payload))
	// 不带任何 API key：模拟未授权请求。

	readBeforeNext := -1
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		readBeforeNext = counter.read
	})

	server := proofServer(t)
	server.withRequestMonitor(httptest.NewRecorder(), request, next)

	if readBeforeNext < 0 {
		t.Fatal("下游 handler 未被调用，测试前提不成立")
	}
	if readBeforeNext >= len(payload) {
		t.Fatalf("确认：未授权请求在鉴权前，监控中间件已从 body 拉走 %d/%d 字节（全量读入内存后才轮到鉴权）",
			readBeforeNext, len(payload))
	}
	t.Logf("鉴权前只读了 %d/%d 字节（未全量消费）", readBeforeNext, len(payload))
}
