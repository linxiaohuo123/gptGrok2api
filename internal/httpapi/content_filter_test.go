package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/auucoder/gptgrok2api-go/internal/protocol"
	"github.com/auucoder/gptgrok2api-go/internal/store"
)

func newTestFilterServer(t *testing.T, cfgMap map[string]any) (*Server, http.Handler) {
	t.Helper()
	root := t.TempDir()
	cfgPath := filepath.Join(root, "config.json")
	accountsPath := filepath.Join(root, "accounts.json")
	authKeysPath := filepath.Join(root, "auth_keys.json")

	if cfgMap == nil {
		cfgMap = map[string]any{}
	}
	cfgRaw, err := json.Marshal(cfgMap)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, cfgRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(accountsPath, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authKeysPath, []byte(`{"items":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := testConfig()
	cfg.RootDir = root
	cfg.DataDir = filepath.Join(root, "data")
	cfg.ConfigPath = cfgPath
	cfg.AccountsPath = accountsPath
	cfg.AuthKeysPath = authKeysPath
	srv := New(cfg)
	return srv, srv.Handler()
}

func TestCheckSensitiveWords(t *testing.T) {
	srv, _ := newTestFilterServer(t, map[string]any{
		"sensitive_words": []any{"badword", "违法词", "BLOCKME"},
	})

	// Clean text passes
	if err := srv.checkSensitiveWords("Hello world, this is fine"); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}

	// Substring and case-insensitive match triggers rejection
	if err := srv.checkSensitiveWords("Here is BadWord included"); err != ErrSensitiveWordDetected {
		t.Fatalf("expected ErrSensitiveWordDetected, got %v", err)
	}

	// Chinese sensitive word triggers rejection
	if err := srv.checkSensitiveWords("包含违法词的内容"); err != ErrSensitiveWordDetected {
		t.Fatalf("expected ErrSensitiveWordDetected for Chinese word, got %v", err)
	}

	// Multiple text pieces
	if err := srv.checkSensitiveWords("part1", "part2 blockme now"); err != ErrSensitiveWordDetected {
		t.Fatalf("expected ErrSensitiveWordDetected for joined texts, got %v", err)
	}
}

func TestApplyGlobalSystemPrompt(t *testing.T) {
	srv, _ := newTestFilterServer(t, map[string]any{
		"global_system_prompt": "You are a helpful and harmless assistant.",
	})

	req := protocol.ChatRequest{
		Model: "gpt-5",
		Messages: []protocol.Message{
			{Role: "user", Content: "Hello!"},
		},
	}

	injected := srv.applyGlobalSystemPrompt(req)
	if len(injected.Messages) != 2 {
		t.Fatalf("expected 2 messages after injection, got %d", len(injected.Messages))
	}
	if injected.Messages[0].Role != "system" || injected.Messages[0].Content != "You are a helpful and harmless assistant." {
		t.Fatalf("unexpected injected system message: %+v", injected.Messages[0])
	}
	if injected.Messages[1].Role != "user" || injected.Messages[1].Content != "Hello!" {
		t.Fatalf("unexpected user message: %+v", injected.Messages[1])
	}

	// Idempotency: calling again does not double inject
	doubleInjected := srv.applyGlobalSystemPrompt(injected)
	if len(doubleInjected.Messages) != 2 {
		t.Fatalf("expected idempotency to keep 2 messages, got %d", len(doubleInjected.Messages))
	}
}

func TestSensitiveWordRejectionEndpoints(t *testing.T) {
	_, handler := newTestFilterServer(t, map[string]any{
		"sensitive_words": []any{"illegal_topic"},
	})

	tests := []struct {
		name     string
		endpoint string
		body     string
	}{
		{
			name:     "chat completions",
			endpoint: "/v1/chat/completions",
			body:     `{"model":"gpt-5","messages":[{"role":"user","content":"tell me about illegal_topic"}]}`,
		},
		{
			name:     "anthropic messages",
			endpoint: "/v1/messages",
			body:     `{"model":"gpt-5","messages":[{"role":"user","content":"tell me about ILLEGAL_TOPIC"}]}`,
		},
		{
			name:     "openai responses",
			endpoint: "/v1/responses",
			body:     `{"model":"gpt-5","input":"how to do illegal_topic"}`,
		},
		{
			name:     "image generations",
			endpoint: "/v1/images/generations",
			body:     `{"model":"gpt-image-2","prompt":"draw illegal_topic"}`,
		},
		{
			name:     "image tasks",
			endpoint: "/api/image-tasks",
			body:     `{"client_task_id":"task-1","prompt":"render illegal_topic"}`,
		},
		{
			name:     "search api",
			endpoint: "/v1/search",
			body:     `{"prompt":"search for illegal_topic"}`,
		},
		{
			name:     "ppt generations",
			endpoint: "/v1/ppt/generations",
			body:     `{"prompt":"generate ppt for illegal_topic"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tt.endpoint, strings.NewReader(tt.body))
			req.Header.Set("Authorization", "Bearer api-secret")
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("[%s] expected 400 Bad Request, got %d, body: %s", tt.name, rec.Code, rec.Body.String())
			}

			var resp map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("[%s] failed to decode response JSON: %v", tt.name, err)
			}

			// Verify detail.error contains the exact Chinese message
			detail, ok := resp["detail"].(map[string]any)
			if !ok {
				t.Fatalf("[%s] expected detail object in response: %s", tt.name, rec.Body.String())
			}
			if detail["error"] != "检测到敏感词，拒绝本次任务" {
				t.Fatalf("[%s] unexpected detail.error: %v", tt.name, detail["error"])
			}
		})
	}
}

func TestStoreConfigCachePerformance(t *testing.T) {
	root := t.TempDir()
	cfgPath := filepath.Join(root, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"sensitive_words":["apple","banana"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := store.New(filepath.Join(root, "accounts.json"), filepath.Join(root, "auth_keys.json"), cfgPath)

	// First load
	cfg1, err := s.Config()
	if err != nil {
		t.Fatal(err)
	}
	words := cfg1["sensitive_words"].([]any)
	if len(words) != 2 {
		t.Fatalf("expected 2 words, got %d", len(words))
	}

	// Repeated loads use memory cache (check speed / correctness)
	for i := 0; i < 1000; i++ {
		cfg, err := s.Config()
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg["sensitive_words"].([]any)) != 2 {
			t.Fatalf("expected cached words")
		}
	}
}
