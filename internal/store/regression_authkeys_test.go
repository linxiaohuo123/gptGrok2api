package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 鉴权在请求主路径上：连续鉴权不得同步重写 auth_keys.json，
// 但 last_used_at 最终必须落盘（延迟合并写，不是不写）。
func TestAuthenticateDefersAuthKeyWrite(t *testing.T) {
	root := t.TempDir()
	authKeysPath := filepath.Join(root, "auth_keys.json")
	repository := New(filepath.Join(root, "accounts.json"), authKeysPath, filepath.Join(root, "config.json"))

	token := "proof-token-0123456789"
	document := map[string]any{"items": []any{map[string]any{
		"id":       "key-1",
		"name":     "proof",
		"role":     "admin",
		"enabled":  true,
		"key_hash": hashKey(token),
	}}}
	raw, _ := json.Marshal(document)
	if err := os.WriteFile(authKeysPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, ok := repository.Authenticate(token); !ok {
		t.Fatal("首次鉴权失败")
	}
	first, err := os.Stat(authKeysPath)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(20 * time.Millisecond)
	if _, ok := repository.Authenticate(token); !ok {
		t.Fatal("第二次鉴权失败")
	}
	second, err := os.Stat(authKeysPath)
	if err != nil {
		t.Fatal(err)
	}
	if second.ModTime().After(first.ModTime()) {
		t.Fatalf("鉴权仍在同步重写文件（mtime %v → %v）", first.ModTime(), second.ModTime())
	}

	// 合并窗口过后必须落盘：既证明写没有丢，也证明它不是同步写。
	time.Sleep(1500 * time.Millisecond)
	third, err := os.Stat(authKeysPath)
	if err != nil {
		t.Fatal(err)
	}
	if !third.ModTime().After(first.ModTime()) {
		t.Fatal("延迟合并写没有落盘，last_used_at 会永久丢失")
	}
	persisted, err := os.ReadFile(authKeysPath)
	if err != nil {
		t.Fatal(err)
	}
	if !containsLastUsed(persisted) {
		t.Fatalf("落盘内容里没有 last_used_at: %s", persisted)
	}
}

func containsLastUsed(raw []byte) bool {
	var document struct {
		Items []map[string]any `json:"items"`
	}
	if json.Unmarshal(raw, &document) != nil {
		return false
	}
	for _, item := range document.Items {
		if value, ok := item["last_used_at"].(string); ok && value != "" {
			return true
		}
	}
	return false
}
