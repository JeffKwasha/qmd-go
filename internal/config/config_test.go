package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSaveConfig(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "qmd-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	os.Setenv("QMD_CONFIG_DIR", tmpDir)
	defer os.Unsetenv("QMD_CONFIG_DIR")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if len(cfg.Collections) != 0 {
		t.Errorf("Expected empty collections, got %d", len(cfg.Collections))
	}

	cfg.GlobalContext = "test global context"
	cfg.Collections["test"] = Collection{
		Path:    "/tmp/test",
		Pattern: "*.md",
		Context: map[string]string{
			"/": "root context",
		},
	}

	if err := SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig failed: %v", err)
	}

	// Verify file exists
	path, _ := GetConfigFilePath()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Errorf("Config file not created at %s", path)
	}

	// Reload
	cfg2, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if cfg2.GlobalContext != "test global context" {
		t.Errorf("Expected global context 'test global context', got '%s'", cfg2.GlobalContext)
	}
	if col, ok := cfg2.Collections["test"]; !ok {
		t.Errorf("Collection 'test' not found")
	} else {
		if col.Path != "/tmp/test" {
			t.Errorf("Expected path '/tmp/test', got '%s'", col.Path)
		}
		if col.Context["/"] != "root context" {
			t.Errorf("Expected context 'root context', got '%s'", col.Context["/"])
		}
	}
}

func TestIndexNames(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"index.yml", "personal.yml", "work.yml", "notes.md", "server.yml", ".hidden.yml"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("collections: {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "sub.yml"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QMD_CONFIG_DIR", dir)

	got, err := IndexNames()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"index", "personal", "work"}
	if len(got) != len(want) {
		t.Fatalf("IndexNames() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("IndexNames() = %v, want %v", got, want)
		}
	}
}

func TestIndexNamesMissingDir(t *testing.T) {
	t.Setenv("QMD_CONFIG_DIR", filepath.Join(t.TempDir(), "absent"))
	got, err := IndexNames()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("IndexNames() = %v, want none", got)
	}
}
