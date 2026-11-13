package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ba0f3/qmd-go/internal/llm"
)

func TestEmbedModelPrecedence(t *testing.T) {
	t.Setenv("QMD_EMBED_MODEL", "env-model")
	if got := EmbedModel(&Config{}); got != "env-model" {
		t.Errorf("empty index file: got %q, want the environment's env-model", got)
	}
	if got := EmbedModel(&Config{EmbedModel: "file-model"}); got != "file-model" {
		t.Errorf("index file with embed_model: got %q, want file-model", got)
	}
	if got := EmbedModelSource(&Config{EmbedModel: "file-model"}); !strings.Contains(got, "index file") {
		t.Errorf("source = %q, want the index file", got)
	}

	t.Setenv("QMD_EMBED_MODEL", "")
	if got, want := EmbedModel(nil), llm.DefaultEmbedModel(); got != want {
		t.Errorf("nothing configured: got %q, want the build default %q", got, want)
	}
	if got := EmbedModelSource(nil); !strings.Contains(got, "build default") {
		t.Errorf("source = %q, want the build default", got)
	}
}

func TestSetEmbedModelPreservesCommentsAndOrder(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("QMD_CONFIG_DIR", dir)
	src := `# header comment
db_path: /tmp/index.sqlite   # pinned on purpose
# collections below
collections:
  notes:                     # a's comment
    path: /tmp/notes
    pattern: "*.md"
`
	path := filepath.Join(dir, "index.yml")
	if err := os.WriteFile(path, []byte(src), 0o640); err != nil {
		t.Fatal(err)
	}

	changed, err := SetEmbedModel("index", "m1")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("first write reported no change")
	}
	got := readFile(t, path)
	for _, want := range []string{
		"# header comment",
		"# pinned on purpose",
		"# collections below",
		"# a's comment",
		"embed_model: m1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("file lost %q:\n%s", want, got)
		}
	}
	if strings.Index(got, "embed_model: m1") > strings.Index(got, "collections:") {
		t.Errorf("embed_model was appended after collections:\n%s", got)
	}

	cfg, err := LoadConfigFor("index")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EmbedModel != "m1" || cfg.DBPath != "/tmp/index.sqlite" || len(cfg.Collections) != 1 {
		t.Errorf("round trip changed the config: %+v", cfg)
	}

	changed, err = SetEmbedModel("index", "m1")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("rewriting the same value reported a change")
	}

	changed, err = SetEmbedModel("index", "m2")
	if err != nil {
		t.Fatal(err)
	}
	if !changed || !strings.Contains(readFile(t, path), "embed_model: m2") {
		t.Errorf("model not replaced:\n%s", readFile(t, path))
	}
	if strings.Count(readFile(t, path), "embed_model:") != 1 {
		t.Errorf("embed_model recorded twice:\n%s", readFile(t, path))
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Errorf("mode = %o, want 640", got)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
