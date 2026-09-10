// [INPUT]: store/protocol
// [OUTPUT]: 敏感词检测与全局提示词注入：checkSensitiveWords、applyGlobalSystemPrompt、promptWithGlobalSystem、writeSensitiveWordError
// [POS]: 请求进入大模型前进行合规拦截与全局提示词注入。
// [PROTOCOL]: 变更时更新此头部，然后检查 CLAUDE.md

package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/auucoder/gptgrok2api-go/internal/protocol"
)

var ErrSensitiveWordDetected = errors.New("检测到敏感词，拒绝本次任务")

func writeSensitiveWordError(w http.ResponseWriter) {
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"detail": map[string]string{"error": "检测到敏感词，拒绝本次任务"},
		"error":  map[string]string{"message": "检测到敏感词，拒绝本次任务", "type": "invalid_request_error"},
	})
}

// parseSensitiveWords extracts sensitive word list from config value (supports []string, []any, string).
func parseSensitiveWords(value any) []string {
	var words []string
	switch typed := value.(type) {
	case []string:
		words = append(words, typed...)
	case []any:
		for _, item := range typed {
			if s := strings.TrimSpace(fmt.Sprint(item)); s != "" {
				words = append(words, s)
			}
		}
	case string:
		for _, line := range strings.FieldsFunc(typed, func(r rune) bool {
			return r == '\n' || r == '\r' || r == ','
		}) {
			if s := strings.TrimSpace(line); s != "" {
				words = append(words, s)
			}
		}
	}
	return words
}

// checkSensitiveWords inspects candidate texts against configured sensitive words.
func (s *Server) checkSensitiveWords(texts ...string) error {
	if s == nil || s.store == nil {
		return nil
	}
	cfg, err := s.store.Config()
	if err != nil {
		return nil
	}
	words := parseSensitiveWords(cfg["sensitive_words"])
	if len(words) == 0 {
		return nil
	}
	fullText := strings.ToLower(strings.Join(texts, " "))
	for _, rawWord := range words {
		word := strings.ToLower(strings.TrimSpace(rawWord))
		if word != "" && strings.Contains(fullText, word) {
			return ErrSensitiveWordDetected
		}
	}
	return nil
}

// applyGlobalSystemPrompt prepends global_system_prompt into ChatRequest messages if configured.
func (s *Server) applyGlobalSystemPrompt(req protocol.ChatRequest) protocol.ChatRequest {
	if s == nil || s.store == nil {
		return req
	}
	cfg, err := s.store.Config()
	if err != nil {
		return req
	}
	prompt := strings.TrimSpace(stringValue(cfg["global_system_prompt"]))
	if prompt == "" {
		return req
	}
	// Do not duplicate if already starts with this system prompt
	if len(req.Messages) > 0 && strings.EqualFold(req.Messages[0].Role, "system") {
		if contentStr, ok := req.Messages[0].Content.(string); ok && strings.Contains(contentStr, prompt) {
			return req
		}
	}
	messages := make([]protocol.Message, 0, len(req.Messages)+1)
	messages = append(messages, protocol.Message{
		Role:    "system",
		Content: prompt,
	})
	messages = append(messages, req.Messages...)
	req.Messages = messages
	return req
}

// promptWithGlobalSystem prepends global_system_prompt into a standalone prompt string if configured.
func (s *Server) promptWithGlobalSystem(prompt string) string {
	if s == nil || s.store == nil {
		return prompt
	}
	cfg, err := s.store.Config()
	if err != nil {
		return prompt
	}
	globalPrompt := strings.TrimSpace(stringValue(cfg["global_system_prompt"]))
	if globalPrompt == "" || strings.HasPrefix(prompt, globalPrompt) {
		return prompt
	}
	return globalPrompt + "\n\n" + prompt
}
