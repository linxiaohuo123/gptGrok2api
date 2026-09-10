// [INPUT]: 仅标准库（encoding/json、strings）
// [OUTPUT]: 协议转换：ResponsesInputMessages、ExtractMessage、contentText、UpstreamError
// [POS]: 四套输入形态归一为内部 Message；contentText 只认带字符串 text 字段的内容块。
// [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md

package protocol

import (
	"fmt"
	"strings"
)

type Message struct {
	Role      string `json:"role"`
	Content   any    `json:"content"`
	ToolCalls []any  `json:"tool_calls,omitempty"`
}

type ChatRequest struct {
	Model           string           `json:"model"`
	Messages        []Message        `json:"messages"`
	Stream          bool             `json:"stream"`
	Size            string           `json:"size,omitempty"`
	ReasoningEffort string           `json:"reasoning_effort"`
	Temperature     *float64         `json:"temperature"`
	TopP            *float64         `json:"top_p"`
	MaxTokens       *int             `json:"max_tokens"`
	Tools           []map[string]any `json:"tools"`
	ToolChoice      any              `json:"tool_choice"`
}

type ResponsesRequest struct {
	Model           string           `json:"model"`
	Input           any              `json:"input"`
	Instructions    string           `json:"instructions"`
	Stream          bool             `json:"stream"`
	Reasoning       map[string]any   `json:"reasoning"`
	Temperature     *float64         `json:"temperature"`
	TopP            *float64         `json:"top_p"`
	MaxOutputTokens *int             `json:"max_output_tokens"`
	Tools           []map[string]any `json:"tools"`
	ToolChoice      any              `json:"tool_choice"`
}

type UpstreamError struct {
	Status  int
	Message string
	Body    string
}

func (e *UpstreamError) Error() string {
	return e.Message
}

func ResponsesInputMessages(input any, instructions string) []Message {
	messages := make([]Message, 0, 2)
	if text := strings.TrimSpace(instructions); text != "" {
		messages = append(messages, Message{Role: "system", Content: text})
	}
	switch value := input.(type) {
	case string:
		if text := strings.TrimSpace(value); text != "" {
			messages = append(messages, Message{Role: "user", Content: text})
		}
	case []any:
		for _, raw := range value {
			object, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			role := strings.TrimSpace(fmt.Sprint(object["role"]))
			if role == "" || role == "<nil>" {
				role = "user"
			}
			content := object["content"]
			if contentText(content) != "" {
				messages = append(messages, Message{Role: role, Content: content})
			}
		}
	case map[string]any:
		role := strings.TrimSpace(fmt.Sprint(value["role"]))
		if role == "" || role == "<nil>" {
			role = "user"
		}
		if content := value["content"]; contentText(content) != "" {
			messages = append(messages, Message{Role: role, Content: content})
		}
	}
	return messages
}

func ExtractMessage(messages []Message) string {
	parts := make([]string, 0, len(messages))
	for _, message := range messages {
		role := strings.TrimSpace(message.Role)
		if role == "" {
			role = "user"
		}
		text := contentText(message.Content)
		if text != "" {
			parts = append(parts, fmt.Sprintf("[%s]: %s", role, text))
		}
	}
	return strings.Join(parts, "\n\n")
}

func contentText(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case []any:
		parts := make([]string, 0, len(typed))
		for _, raw := range typed {
			// 只认"带字符串 text 字段"这一件事，不列 type 白名单：
			// Chat Completions 的 text、Responses API 的 input_text/output_text
			// 以及后续新增的同类块都自动落进来，不会再被静默丢弃。
			if object, ok := raw.(map[string]any); ok {
				if text, ok := object["text"].(string); ok {
					parts = append(parts, strings.TrimSpace(text))
				}
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	default:
		return ""
	}
}
