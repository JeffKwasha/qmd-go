package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ba0f3/qmd-go/internal/config"
	"github.com/ba0f3/qmd-go/internal/store"
)

func writeIndexConfigs(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name+".yml"), []byte("collections: {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("QMD_CONFIG_DIR", dir)
	return dir
}

func TestIndexTargetsWithoutIndexWalksEveryIndex(t *testing.T) {
	dir := writeIndexConfigs(t, "personal", "index", "work")
	if err := os.WriteFile(filepath.Join(dir, config.ServerConfigName), []byte("servers: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := indexTargets(false, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"index", "personal", "work"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("targets = %v, want %v", got, want)
	}
}

func TestIndexTargetsWithIndexStaysNarrow(t *testing.T) {
	writeIndexConfigs(t, "index", "personal")
	got, err := indexTargets(true, "personal")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "personal" {
		t.Errorf("targets = %v, want [personal]", got)
	}
}

func TestIndexTargetsMissingIndexNamesPath(t *testing.T) {
	dir := writeIndexConfigs(t, "index")
	_, err := indexTargets(true, "ghost")
	if err == nil {
		t.Fatal("missing index accepted")
	}
	for _, want := range []string{filepath.Join(dir, "ghost.yml"), "QMD_CONFIG_DIR"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestIndexTargetsRejectsServerConfigAsIndex(t *testing.T) {
	dir := writeIndexConfigs(t, "index")
	if err := os.WriteFile(filepath.Join(dir, config.ServerConfigName), []byte("servers: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := indexTargets(true, strings.TrimSuffix(config.ServerConfigName, ".yml"))
	if err == nil || !strings.Contains(err.Error(), config.ServerConfigName) {
		t.Fatalf("want server-config error, got %v", err)
	}
}

func TestIndexTargetsEmptyConfigDir(t *testing.T) {
	dir := writeIndexConfigs(t)
	_, err := indexTargets(false, "")
	if err == nil {
		t.Fatal("empty config dir accepted")
	}
	for _, want := range []string{dir, "QMD_CONFIG_DIR"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestIndexTargetsRejectsEmptyExplicitIndex(t *testing.T) {
	writeIndexConfigs(t, "index")
	if _, err := indexTargets(true, ""); err == nil {
		t.Fatal("--index with no name accepted")
	}
}

func TestResolveServerConfigPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("QMD_CONFIG_DIR", dir)
	t.Setenv("QMD_SERVER_CONFIG", "")

	got, err := resolveServerConfigPath("")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, config.ServerConfigName); got != want {
		t.Errorf("default = %q, want %q", got, want)
	}

	envPath := filepath.Join(dir, "env.yml")
	t.Setenv("QMD_SERVER_CONFIG", envPath)
	got, err = resolveServerConfigPath("")
	if err != nil {
		t.Fatal(err)
	}
	if got != envPath {
		t.Errorf("env = %q, want %q", got, envPath)
	}

	flagPath := filepath.Join(dir, "flag.yml")
	got, err = resolveServerConfigPath(flagPath)
	if err != nil {
		t.Fatal(err)
	}
	if got != flagPath {
		t.Errorf("flag = %q, want %q", got, flagPath)
	}
}

func TestEmbedRefusesConfiguredModelChange(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "index.sqlite")
	yml := "db_path: " + dbPath + "\ncollections: {}\n"
	if err := os.WriteFile(filepath.Join(dir, "index.yml"), []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QMD_CONFIG_DIR", dir)
	t.Setenv("QMD_EMBED_MODEL", "new-model")

	s, err := store.NewStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.EnsureEmbeddingBlobTable(); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertEmbedding("hash1", 0, 0, []float32{1, 0}, "old-model", time.Now()); err != nil {
		t.Fatal(err)
	}

	err = embedIndex("index", false, "")
	if err == nil {
		t.Fatal("model change accepted")
	}
	for _, want := range []string{"old-model", "new-model", "--force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}

	if err := embedIndex("index", true, ""); err != nil {
		t.Fatalf("force rebuild: %v", err)
	}
	models, err := s.EmbeddingModels()
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 0 {
		t.Errorf("vectors survived --force: %v", models)
	}
}

func TestEmbedRecordsModelInIndexFile(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "index.sqlite")
	yml := "db_path: " + dbPath + "\ncollections: {}\n"
	if err := os.WriteFile(filepath.Join(dir, "index.yml"), []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QMD_CONFIG_DIR", dir)
	t.Setenv("QMD_EMBED_MODEL", "env-model")

	// Nothing to embed, but the run still records what it would have used.
	if err := embedIndex("index", false, ""); err != nil {
		t.Fatal(err)
	}
	if got := loadEmbedModel(t, dir); got != "env-model" {
		t.Errorf("embed_model = %q, want env-model", got)
	}

	// The file now wins over the environment.
	t.Setenv("QMD_EMBED_MODEL", "some-other-model")
	if err := embedIndex("index", false, ""); err != nil {
		t.Fatal(err)
	}
	if got := loadEmbedModel(t, dir); got != "env-model" {
		t.Errorf("a later run changed the recorded model to %q", got)
	}

	// --model overrides the file and records itself.
	if err := embedIndex("index", false, "flag-model"); err != nil {
		t.Fatal(err)
	}
	if got := loadEmbedModel(t, dir); got != "flag-model" {
		t.Errorf("embed_model = %q, want flag-model", got)
	}
}

func loadEmbedModel(t *testing.T, dir string) string {
	t.Helper()
	cfg, err := config.LoadConfigFor("index")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DBPath != filepath.Join(dir, "index.sqlite") {
		t.Errorf("db_path changed to %q", cfg.DBPath)
	}
	return cfg.EmbedModel
}
