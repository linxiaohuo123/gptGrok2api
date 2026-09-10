// [INPUT]: model/protocol/provider
// [OUTPUT]: OpenAI /v1/responses、responseFromChat、writeEventSSE
// [POS]: Responses 协议适配层；writeEventSSE 是全项目 SSE 的统一出口（设头 + flush）。
// [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md

package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/model"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
)

func (s *Server) responses(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	var request protocol.ResponsesRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	if strings.TrimSpace(request.Model) == "" {
		writeError(w, http.StatusBadRequest, "model is required", "invalid_request_error")
		return
	}
	route, ok := model.ResolveChat(request.Model)
	if !ok || route.Image {
		writeError(w, http.StatusNotFound, "model not found", "invalid_request_error")
		return
	}
	messages := protocol.ResponsesInputMessages(request.Input, request.Instructions)
	if len(messages) == 0 {
		writeError(w, http.StatusBadRequest, "input cannot be empty", "invalid_request_error")
		return
	}
	if err := s.checkSensitiveWords(protocol.ExtractMessage(messages)); err != nil {
		writeSensitiveWordError(w)
		return
	}
	chatRequest := protocol.ChatRequest{
		Model: request.Model, Messages: messages, Stream: false,
		Temperature: request.Temperature, TopP: request.TopP,
		MaxTokens: request.MaxOutputTokens, Tools: request.Tools,
		ToolChoice: request.ToolChoice,
	}
	chatRequest = s.applyGlobalSystemPrompt(chatRequest)
	chatRequest.ReasoningEffort = stringValue(request.Reasoning["effort"])
	if request.Stream {
		s.streamWebResponses(w, r, chatRequest, route)
	} else {
		s.completeWebResponses(w, r, chatRequest, route)
	}
}

func (s *Server) completeWebResponses(w http.ResponseWriter, r *http.Request, request protocol.ChatRequest, route model.ChatRoute) {
	recorder := &responseCapture{header: make(http.Header)}
	s.completeOpenAIChat(recorder, r, request, route)
	if recorder.status >= 400 {
		w.WriteHeader(recorder.status)
		_, _ = w.Write(recorder.body.Bytes())
		return
	}
	var chat map[string]any
	if err := json.Unmarshal(recorder.body.Bytes(), &chat); err != nil {
		writeError(w, http.StatusBadGateway, "invalid internal response", "server_error")
		return
	}
	writeJSON(w, http.StatusOK, responseFromChat(chat, request.Model))
}

func (s *Server) streamWebResponses(w http.ResponseWriter, r *http.Request, request protocol.ChatRequest, route model.ChatRoute) {
	recorder := &responseCapture{header: make(http.Header)}
	s.completeOpenAIChat(recorder, r, request, route)
	if recorder.status >= 400 {
		w.WriteHeader(recorder.status)
		_, _ = w.Write(recorder.body.Bytes())
		return
	}
	var chat map[string]any
	if err := json.Unmarshal(recorder.body.Bytes(), &chat); err != nil {
		writeError(w, http.StatusBadGateway, "invalid internal response", "server_error")
		return
	}
	response := responseFromChat(chat, request.Model)
	responseID := stringValue(response["id"])
	writeEventSSE(w, "response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": responseID, "object": "response", "status": "in_progress", "model": request.Model}})
	writeEventSSE(w, "response.in_progress", map[string]any{"type": "response.in_progress", "response": map[string]any{"id": responseID, "object": "response", "status": "in_progress", "model": request.Model}})
	writeResponseOutputEvents(w, response["output"])
	response["status"] = "completed"
	writeEventSSE(w, "response.completed", map[string]any{"type": "response.completed", "response": response})
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// added/delta/done 按 item 逐个下发：item 必须是单个对象，
// 且 done 不能藏在"有文本内容"的判断里，否则工具调用永远收不到 done。
func writeResponseOutputEvents(w http.ResponseWriter, output any) {
	items, _ := output.([]any)
	for index, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		writeEventSSE(w, "response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": index, "item": item})
		if content, ok := item["content"].([]any); ok {
			for contentIndex, rawPart := range content {
				part, ok := rawPart.(map[string]any)
				if !ok {
					continue
				}
				text, ok := part["text"].(string)
				if !ok {
					continue
				}
				writeEventSSE(w, "response.output_text.delta", map[string]any{"type": "response.output_text.delta", "item_id": item["id"], "output_index": index, "content_index": contentIndex, "delta": text})
			}
		}
		writeEventSSE(w, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": index, "item": item})
	}
}

func responseFromChat(chat map[string]any, modelName string) map[string]any {
	responseID := "resp_" + strings.TrimPrefix(stringValue(chat["id"]), "chatcmpl-")
	outputID := "msg_" + strings.TrimPrefix(stringValue(chat["id"]), "chatcmpl-")
	content := ""
	output := []any{}
	if choices, ok := chat["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if message, ok := choice["message"].(map[string]any); ok {
				if calls, ok := message["tool_calls"].([]any); ok {
					for _, raw := range calls {
						call, _ := raw.(map[string]any)
						fn, _ := call["function"].(map[string]any)
						output = append(output, map[string]any{"id": call["id"], "type": "function_call", "call_id": call["id"], "name": fn["name"], "arguments": fn["arguments"], "status": "completed"})
					}
				} else {
					content = stringValue(message["content"])
				}
			}
		}
	}
	if len(output) == 0 {
		output = []any{map[string]any{"id": outputID, "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": content, "annotations": []any{}}}}}
	}
	return map[string]any{"id": responseID, "object": "response", "created_at": time.Now().Unix(), "status": "completed", "model": modelName, "output": output, "usage": chat["usage"]}
}

type responseCapture struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (r *responseCapture) Header() http.Header    { return r.header }
func (r *responseCapture) WriteHeader(status int) { r.status = status }
func (r *responseCapture) Write(data []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(data)
}

func writeEventSSE(w http.ResponseWriter, event string, value any) {
	// 事件流必须声明类型并及时 flush：否则客户端按普通文本解析，
	// 且所有事件要等 handler 返回才一次性到达，"流式"名存实亡。
	header := w.Header()
	header.Set("Content-Type", "text/event-stream; charset=utf-8")
	header.Set("Cache-Control", "no-cache")
	header.Set("X-Accel-Buffering", "no")
	raw, _ := json.Marshal(value)
	_, _ = io.WriteString(w, "event: "+event+"\n")
	_, _ = io.WriteString(w, "data: "+string(raw)+"\n\n")
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}
