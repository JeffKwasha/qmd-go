package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestUpdateTargetsWithoutIndexWalksEveryIndex(t *testing.T) {
	dir := writeIndexConfigs(t, "personal", "index", "work")
	if err := os.WriteFile(filepath.Join(dir, "server.yml"), []byte("servers: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := updateTargets(false, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"index", "personal", "work"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("targets = %v, want %v", got, want)
	}
}

func TestUpdateTargetsWithIndexStaysNarrow(t *testing.T) {
	writeIndexConfigs(t, "index", "personal")
	got, err := updateTargets(true, "personal")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "personal" {
		t.Errorf("targets = %v, want [personal]", got)
	}
}

func TestUpdateTargetsMissingIndexNamesPath(t *testing.T) {
	dir := writeIndexConfigs(t, "index")
	_, err := updateTargets(true, "ghost")
	if err == nil {
		t.Fatal("missing index accepted")
	}
	for _, want := range []string{filepath.Join(dir, "ghost.yml"), "QMD_CONFIG_DIR"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestUpdateTargetsRejectsServerConfigAsIndex(t *testing.T) {
	dir := writeIndexConfigs(t, "index")
	if err := os.WriteFile(filepath.Join(dir, "server.yml"), []byte("servers: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := updateTargets(true, "server")
	if err == nil || !strings.Contains(err.Error(), "server.yml") {
		t.Fatalf("want server-config error, got %v", err)
	}
}

func TestUpdateTargetsEmptyConfigDir(t *testing.T) {
	dir := writeIndexConfigs(t)
	_, err := updateTargets(false, "")
	if err == nil {
		t.Fatal("empty config dir accepted")
	}
	for _, want := range []string{dir, "QMD_CONFIG_DIR"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestUpdateTargetsRejectsEmptyExplicitIndex(t *testing.T) {
	writeIndexConfigs(t, "index")
	if _, err := updateTargets(true, ""); err == nil {
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
	if want := filepath.Join(dir, "server.yml"); got != want {
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
