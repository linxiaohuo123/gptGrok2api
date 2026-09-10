package httpapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemovedGrokModelsReturnNotFound(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig()
	cfg.RootDir = root
	cfg.DataDir = filepath.Join(root, "data")
	cfg.ConfigPath = filepath.Join(root, "config.json")
	cfg.AccountsPath = filepath.Join(root, "accounts.json")
	cfg.AuthKeysPath = filepath.Join(root, "auth_keys.json")
	handler := New(cfg).Handler()
	for _, endpoint := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		request := httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(`{"model":"grok-4.20-fast","messages":[{"role":"user","content":"hi"}],"input":"hi"}`))
		request.Header.Set("Authorization", "Bearer api-secret")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("removed Grok model should return 404: %s => %d %s", endpoint, response.Code, response.Body.String())
		}
	}
}
