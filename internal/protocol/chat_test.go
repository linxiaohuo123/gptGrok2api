package protocol

import (
	"strings"
	"testing"
)

func TestExtractMessage(t *testing.T) {
	message := ExtractMessage([]Message{
		{Role: "system", Content: "Be concise"},
		{Role: "user", Content: []any{
			map[string]any{"type": "text", "text": "Hello"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "ignored"}},
		}},
	})
	if !strings.Contains(message, "[system]: Be concise") || !strings.Contains(message, "[user]: Hello") {
		t.Fatalf("unexpected message: %q", message)
	}
}
