package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadIndexConfigRefusesMissingIndex(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("QMD_CONFIG_DIR", dir)

	// Nothing to read yet: a first run may still bootstrap.
	cfg, err := LoadIndexConfig("index")
	if err != nil {
		t.Fatalf("empty config dir: %v", err)
	}
	if len(cfg.Collections) != 0 {
		t.Errorf("empty config dir returned %d collections", len(cfg.Collections))
	}

	if err := os.WriteFile(filepath.Join(dir, "public.yml"), []byte("collections: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = LoadIndexConfig("index")
	if err == nil {
		t.Fatal("missing default index accepted while public.yml exists")
	}
	for _, want := range []string{"no index.yml in " + dir, "--index public"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}

	if _, err := LoadIndexConfig("public"); err != nil {
		t.Errorf("existing index refused: %v", err)
	}

	_, err = LoadIndexConfig("ghost")
	if err == nil || !strings.Contains(err.Error(), "no ghost.yml in ") {
		t.Errorf("missing named index: got %v", err)
	}

	// Writers may still create the file that reads refuse.
	if _, err := LoadConfig(); err != nil {
		t.Errorf("LoadConfig must tolerate a missing file: %v", err)
	}
}
