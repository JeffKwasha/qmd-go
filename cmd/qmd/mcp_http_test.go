package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testServerConfig() *serverConfig {
	return &serverConfig{
		Servers: map[string]string{
			"/mcp":          "index",
			"/mcp/shared":   "index",
			"/mcp/personal": "personal",
		},
		Auth: authSection{
			Public: []string{"/mcp", "/mcp/shared"},
			Groups: map[string]authGroup{
				"personal": {
					Keys:  []string{"plain-text-key"},
					Allow: []string{"/mcp/personal", "/mcp"},
				},
				"laptop": {
					KeyHashes: []keyHash{{Hash: hashKey("hashed-key")}},
					Allow:     []string{"/mcp/personal"},
				},
			},
		},
	}
}

func TestNormalizePath(t *testing.T) {
	cases := map[string]string{
		"/mcp/":     "/mcp",
		"mcp":       "/mcp",
		" /mcp ":    "/mcp",
		"/":         "/",
		"/mcp/work": "/mcp/work",
	}
	for in, want := range cases {
		if got := normalizePath(in); got != want {
			t.Errorf("normalizePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestKeyAllows(t *testing.T) {
	sc := testServerConfig()
	cases := []struct {
		name string
		key  string
		path string
		want bool
	}{
		{"plaintext key, allowed path", "plain-text-key", "/mcp/personal", true},
		{"plaintext key, alias path", "plain-text-key", "/mcp", true},
		{"plaintext key, denied path", "plain-text-key", "/mcp/shared", false},
		{"hashed key, allowed path", "hashed-key", "/mcp/personal", true},
		{"hashed key, denied path", "hashed-key", "/mcp/shared", false},
		{"no key", "", "/mcp/personal", false},
		{"wrong key", "nope", "/mcp/personal", false},
	}
	for _, c := range cases {
		if got := keyAllows(sc, c.key, c.path); got != c.want {
			t.Errorf("%s: keyAllows(%q, %q) = %v, want %v", c.name, c.key, c.path, got, c.want)
		}
	}
}

func TestKeyAllowsSharedAcrossGroups(t *testing.T) {
	sc := &serverConfig{
		Servers: map[string]string{"/a": "index", "/b": "index"},
		Auth: authSection{Groups: map[string]authGroup{
			"one": {Keys: []string{"shared"}, Allow: []string{"/a"}},
			"two": {Keys: []string{"shared"}, Allow: []string{"/b"}},
		}},
	}
	for _, p := range []string{"/a", "/b"} {
		if !keyAllows(sc, "shared", p) {
			t.Errorf("key shared should reach %s through one of its groups", p)
		}
	}
	if keyAllows(sc, "shared", "/c") {
		t.Error("key shared should not reach an unmounted path")
	}
}

func TestAuthMiddleware(t *testing.T) {
	sc := testServerConfig()
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := authMiddleware(staticAuthState(sc), ok)

	cases := []struct {
		name string
		path string
		auth string
		want int
	}{
		{"public path, no key", "/mcp/shared", "", http.StatusOK},
		{"public alias, no key", "/mcp", "", http.StatusOK},
		{"gated path, no key", "/mcp/personal", "", http.StatusUnauthorized},
		{"gated path, good key", "/mcp/personal", "Bearer plain-text-key", http.StatusOK},
		{"gated path, hashed key", "/mcp/personal", "Bearer hashed-key", http.StatusOK},
		{"gated path, bad key", "/mcp/personal", "Bearer wrong", http.StatusUnauthorized},
		{"unmounted path", "/mcp/nope", "", http.StatusNotFound},
		{"healthz", "/healthz", "", http.StatusOK},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodPost, c.path, nil)
		if c.auth != "" {
			req.Header.Set("Authorization", c.auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: %s got %d, want %d", c.name, c.path, rec.Code, c.want)
		}
	}
}

const serverYMLFixture = `# mounts and auth
servers:
  /mcp: index          # alias
  /mcp/personal: personal
auth:
  public: [/mcp]
  groups:
    personal:
      keys: ["legacy-key"]
      allow: [/mcp/personal, /mcp]
`

func writeFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "server.yml")
	if err := os.WriteFile(p, []byte(serverYMLFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAddServerKeyStoresOnlyHash(t *testing.T) {
	p := writeFixture(t)
	key, fp, err := addServerKey(p, "personal", "laptop", nil)
	if err != nil {
		t.Fatal(err)
	}
	if key == "" || len(key) != 48 {
		t.Fatalf("key = %q, want 48 hex chars", key)
	}
	if !strings.HasPrefix(fp, "sha256:") {
		t.Fatalf("fingerprint = %q", fp)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	if strings.Contains(body, key) {
		t.Fatal("plaintext key written to server.yml")
	}
	if !strings.Contains(body, hashKey(key)) {
		t.Fatalf("hash for new key missing from server.yml:\n%s", body)
	}
	if !strings.Contains(body, "# mounts and auth") {
		t.Error("comments were dropped")
	}
	sc, err := loadServerConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if !keyAllows(sc, key, "/mcp/personal") {
		t.Error("new key cannot reach its group's path")
	}
	if !keyAllows(sc, "legacy-key", "/mcp/personal") {
		t.Error("existing plaintext key stopped working")
	}
}

func TestAddServerKeySetsAllow(t *testing.T) {
	p := writeFixture(t)
	key, _, err := addServerKey(p, "personal", "limited", []string{"/mcp"})
	if err != nil {
		t.Fatal(err)
	}
	sc, err := loadServerConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if !keyAllows(sc, key, "/mcp") {
		t.Error("new key cannot reach /mcp")
	}
	if keyAllows(sc, key, "/mcp/personal") {
		t.Error("key reaches a path outside its group")
	}
}

func TestRemoveServerKey(t *testing.T) {
	p := writeFixture(t)
	key, fp, err := addServerKey(p, "personal", "laptop", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := removeServerKey(p, "", fp[:len("sha256:")+8]); err != nil {
		t.Fatal(err)
	}
	sc, err := loadServerConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if keyAllows(sc, key, "/mcp/personal") {
		t.Error("removed key still works")
	}
	if !keyAllows(sc, "legacy-key", "/mcp/personal") {
		t.Error("removal broke an unrelated key")
	}
}

func TestListServerKeys(t *testing.T) {
	p := writeFixture(t)
	key, _, err := addServerKey(p, "personal", "laptop", nil)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := listServerKeys(p)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range entries {
		if e.Key == hashKey(key) && e.Label == "laptop" && e.Group == "personal" {
			found = true
		}
	}
	if !found {
		t.Errorf("new key not listed: %+v", entries)
	}
}

func TestCheckServerConfig(t *testing.T) {
	p := writeFixture(t)
	body := strings.Replace(serverYMLFixture, "  /mcp/personal: personal", "  /mcp/personal: personal\n  /mcp/stale: missing", 1)
	body = strings.Replace(body, "allow: [/mcp/personal, /mcp]", "allow: [/mcp/personal, /mcp/nope]", 1)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	findings, err := checkServerConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(findings, "\n")
	for _, want := range []string{"missing", "/mcp/nope", "plaintext"} {
		if !strings.Contains(joined, want) {
			t.Errorf("findings missing %q:\n%s", want, joined)
		}
	}
}
