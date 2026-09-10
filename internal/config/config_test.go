package config

import (
	"path/filepath"
	"testing"
)

func TestLoadSupportsLegacyRootAndDataEnvironment(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GO_ROOT_DIR", "")
	t.Setenv("GROK_ROOT_DIR", root)
	t.Setenv("GO_DATA_DIR", "")
	t.Setenv("GROK_DATA_DIR", "legacy-data")

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RootDir != root {
		t.Fatalf("root dir = %q, want %q", cfg.RootDir, root)
	}
	wantDataDir := filepath.Join(root, "legacy-data")
	if cfg.DataDir != wantDataDir {
		t.Fatalf("data dir = %q, want %q", cfg.DataDir, wantDataDir)
	}
}

func TestLoadPrefersCurrentDataEnvironment(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GO_DATA_DIR", "current-data")
	t.Setenv("GROK_DATA_DIR", "legacy-data")

	cfg, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	wantDataDir := filepath.Join(root, "current-data")
	if cfg.DataDir != wantDataDir {
		t.Fatalf("data dir = %q, want %q", cfg.DataDir, wantDataDir)
	}
}
