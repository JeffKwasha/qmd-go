package main

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// testServerYAML builds a config with two groups and the keys given as the
// body of auth.keys (each line indented four spaces).
func testServerYAML(keys string) string {
	return `# mounts and auth
servers:
  /mcp: index          # alias
  /mcp/shared: index
  /mcp/personal: personal
auth:
  public: [/mcp, /mcp/shared]
  groups:
    personal:
      allow: ["/mcp/personal", "/mcp"]
    laptop:
      allow: ["/mcp/personal"]
  keys:
` + keys
}

func testKeysYAML() string {
	return fmt.Sprintf(`    - hash: %q
      group: personal
      label: desktop
    - hash: %q
      group: laptop
      label: laptop
`, hashKey("plain-text-key"), hashKey("hashed-key"))
}

func writeServerYAML(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "srv.yml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func parseYAML(t *testing.T, body string) *serverConfig {
	t.Helper()
	sc, err := parseServerConfig([]byte(body), "test.yml")
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

func TestNormalizePath(t *testing.T) {
	cases := map[string]string{
		"/mcp/":       "/mcp",
		"mcp":         "/mcp",
		" /mcp ":      "/mcp",
		"/":           "/",
		"/mcp/work":   "/mcp/work",
		"/mcp//":      "/mcp",
		"/mcp/work//": "/mcp/work",
		"///":         "/",
	}
	for in, want := range cases {
		got := normalizePath(in)
		if got != want {
			t.Errorf("normalizePath(%q) = %q, want %q", in, got, want)
		}
		if again := normalizePath(got); again != got {
			t.Errorf("normalizePath is not idempotent: %q -> %q -> %q", in, got, again)
		}
	}
}

func TestMountsThatNormalizeAlikeRejected(t *testing.T) {
	for _, body := range []string{
		`servers:
  /mcp: index
  /mcp/: personal
`,
		`servers:
  /mcp: index
  mcp: personal
`,
		`servers:
  " /mcp": index
  /mcp: personal
`,
		// Trailing slashes are collapsed in full, so these are one mount. Left
		// distinct they would register the same http.ServeMux pattern twice and
		// panic the process.
		`servers:
  /mcp: index
  /mcp//: personal
`,
		`servers:
  /a//: index
  /a///: personal
`,
		`servers:
  /: index
  "///": personal
`,
	} {
		_, err := parseServerConfig([]byte(body), "test.yml")
		if err == nil || !strings.Contains(err.Error(), "a path is mounted once") {
			t.Errorf("config %q: want duplicate-mount error, got %v", body, err)
		}
	}
	_, err := parseServerConfig([]byte("servers:\n  \"\": index\n"), "test.yml")
	if err == nil || !strings.Contains(err.Error(), "empty mount path") {
		t.Errorf("want empty-mount error, got %v", err)
	}
}

func TestEmptyServersRejected(t *testing.T) {
	for _, body := range []string{"servers: {}\n", "servers:\n", "auth:\n  public: [/mcp]\n"} {
		if _, err := parseServerConfig([]byte(body), "test.yml"); err == nil ||
			!strings.Contains(err.Error(), "no servers defined") {
			t.Errorf("config %q accepted, or wrong error: %v", body, err)
		}
	}
}

func TestUncleanMountPathsRejected(t *testing.T) {
	cases := map[string]string{
		"/a/../b": "/b",
		"/a/./b":  "/a/b",
		"/a//b":   "/a/b",
		"//mcp":   "/mcp",
		"/mcp/.":  "/mcp",
	}
	for mount, want := range cases {
		body := fmt.Sprintf("servers:\n  %q: index\n", mount)
		_, err := parseServerConfig([]byte(body), "test.yml")
		if err == nil || !strings.Contains(err.Error(), "not a clean path") {
			t.Errorf("mount %q accepted, or wrong error: %v", mount, err)
			continue
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("%q", want)) {
			t.Errorf("mount %q: error does not suggest %q: %v", mount, want, err)
		}
	}
}

// The router matches the escaped request path, while the auth middleware
// normalizes the decoded one. A mount that is not its own escaped form would be
// authorized as the decoded path and routed as the escaped one, so a public
// path or a key's allow list for /a/b would reach an index mounted at /a%2Fb.
func TestMountPathsMustBeTheirOwnEscapedForm(t *testing.T) {
	for _, mount := range []string{"/a%2Fb", "/mcp/%2e%2e/x", "/100%", "/grüße", "/a?b#c"} {
		body := fmt.Sprintf("servers:\n  %q: index\n", mount)
		_, err := parseServerConfig([]byte(body), "test.yml")
		if err == nil || !strings.Contains(err.Error(), "URL-escaped") {
			t.Errorf("mount %q accepted, or wrong error: %v", mount, err)
		}
	}
	// The bypass needs a second mount whose decoded form is a mount of its own:
	// the request is authorized as /a/b and routed to /a%2Fb.
	body := "servers:\n  /a/b: index\n  \"/a%2Fb\": secret\nauth:\n  public: [/a/b]\n"
	if _, err := parseServerConfig([]byte(body), "test.yml"); err == nil ||
		!strings.Contains(err.Error(), "URL-escaped") {
		t.Errorf("config with a percent-encoded mount accepted: %v", err)
	}

	// Every mount the loader accepts is its own escaped form, which is what
	// makes the authorized path and the routed path the same string.
	accepted := 0
	for _, body := range []string{
		"servers:\n  /: index\n",
		"servers:\n  /mcp: index\n  /mcp/v1.0: personal\n",
		"servers:\n  /a/b//: index\n",
		"servers:\n  \"//\": index\n",
	} {
		sc, err := parseServerConfig([]byte(body), "test.yml")
		if err != nil {
			continue
		}
		accepted++
		for p := range sc.Servers {
			if escaped := (&url.URL{Path: p}).EscapedPath(); escaped != p {
				t.Errorf("accepted mount %q escapes to %q", p, escaped)
			}
		}
	}
	if accepted == 0 {
		t.Error("no config was accepted; the test proves nothing")
	}
}

// Anything the loader accepts must register, since runMCPHTTP registers a
// pattern per mount and a panic there kills the process instead of the load.
func TestAcceptedMountsRegisterInServeMux(t *testing.T) {
	bodies := []string{
		"servers:\n  /mcp: index\n",
		"servers:\n  /: index\n",
		"servers:\n  /mcp: index\n  /mcp/x: personal\n",
		"servers:\n  /mcp/x: index\n  /mcp: personal\n",
		"servers:\n  /a//: index\n",
		"servers:\n  /a//: index\n  /a///: personal\n",
		"servers:\n  /: index\n  \"///\": personal\n",
		"servers:\n  /a/../b: index\n",
		"servers:\n  /mcp/v1.0: index\n",
	}
	registered := 0
	for _, body := range bodies {
		sc, err := parseServerConfig([]byte(body), "test.yml")
		if err != nil {
			continue
		}
		registered++
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("accepted config %q panics at startup: %v", body, r)
				}
			}()
			mux := http.NewServeMux()
			h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
			for p := range sc.Servers {
				mux.Handle(p, h)
				mux.Handle(p+"/", h)
			}
		}()
	}
	if registered == 0 {
		t.Error("every config was rejected; the test proves nothing")
	}
}

// The router matches the escaped request path, the key check the decoded one.
// An encoded slash decodes to a mount but routes to the subtree of an enclosing
// mount, so a key allowed only the child mount would be served the parent index.
func TestEncodedRequestPathServesNoOtherIndex(t *testing.T) {
	childOnly := parseYAML(t, fmt.Sprintf(`servers:
  /a: index
  /a/b/c: secret
auth:
  groups:
    child:
      allow: ["/a/b/c"]
  keys:
    - hash: %q
      group: child
`, hashKey("child-key")))
	withRoot := parseYAML(t, fmt.Sprintf(`servers:
  /: index
  /mcp/personal: personal
auth:
  groups:
    personal:
      allow: ["/mcp/personal"]
  keys:
    - hash: %q
      group: personal
`, hashKey("personal-key")))

	// A real mux, registered the way runMCPHTTP registers it, answering with the
	// name of the index it dispatched to.
	serve := func(sc *serverConfig, target, key string) (int, string) {
		paths := make([]string, 0, len(sc.Servers))
		for p := range sc.Servers {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		mux := http.NewServeMux()
		for _, p := range paths {
			name := sc.Servers[p]
			h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, name)
			})
			mux.Handle(p, h)
			mux.Handle(p+"/", h)
		}
		req := httptest.NewRequest(http.MethodPost, target, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		authMiddleware(staticAuthState(sc), mux).ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	cases := []struct {
		sc     *serverConfig
		key    string
		target string
		code   int
		index  string
	}{
		{childOnly, "child-key", "/a/b/c", http.StatusOK, "secret"},
		{childOnly, "child-key", "/a/b/c/", http.StatusOK, "secret"},
		{childOnly, "child-key", "/a/b%2Fc", http.StatusNotFound, ""},
		{childOnly, "child-key", "/a/b%2fc", http.StatusNotFound, ""},
		{childOnly, "child-key", "/a/%62/c", http.StatusNotFound, ""},
		{withRoot, "personal-key", "/mcp/personal", http.StatusOK, "personal"},
		{withRoot, "personal-key", "/mcp%2Fpersonal", http.StatusNotFound, ""},
		{withRoot, "personal-key", "/mcp%2fpersonal", http.StatusNotFound, ""},
		{withRoot, "personal-key", "/mcp%2Fpersonal/", http.StatusNotFound, ""},
	}
	for _, c := range cases {
		code, body := serve(c.sc, c.target, c.key)
		if code != c.code {
			t.Errorf("%s with %s: got %d, want %d (body %q)", c.target, c.key, code, c.code, body)
			continue
		}
		if c.index != "" && body != c.index {
			t.Errorf("%s: answered by index %q, want %q", c.target, body, c.index)
		}
		if c.index == "" && body != "404 page not found\n" {
			t.Errorf("%s: reached an index instead of 404: %q", c.target, body)
		}
	}
}

func TestEmptyPublicPathRejected(t *testing.T) {
	for _, p := range []string{`""`, `" "`} {
		body := fmt.Sprintf("servers:\n  \"/\": index\nauth:\n  public: [%s]\n", p)
		if _, err := parseServerConfig([]byte(body), "test.yml"); err == nil || !strings.Contains(err.Error(), "empty path") {
			t.Errorf("public %s accepted or wrong error: %v", p, err)
		}
	}
}

func TestParseServerConfigIndexesKeys(t *testing.T) {
	sc := parseYAML(t, testServerYAML(testKeysYAML()))
	if len(sc.byHash) != 2 {
		t.Fatalf("byHash has %d entries, want 2", len(sc.byHash))
	}
	e, ok := sc.byHash[hashKey("hashed-key")]
	if !ok {
		t.Fatal("hashed key not indexed")
	}
	if e.Group != "laptop" || e.Label != "laptop" {
		t.Errorf("entry = %+v, want group laptop, label laptop", e)
	}
	if !sc.Auth.Groups["laptop"].allowsPath("/mcp/personal") {
		t.Error("group paths were not loaded")
	}
}

func TestKeyAllows(t *testing.T) {
	sc := parseYAML(t, testServerYAML(testKeysYAML()))
	cases := []struct {
		name string
		key  string
		path string
		want bool
	}{
		{"group path", "plain-text-key", "/mcp/personal", true},
		{"second path of same group", "plain-text-key", "/mcp", true},
		{"other group's path", "plain-text-key", "/mcp/shared", false},
		{"other key's group", "hashed-key", "/mcp/personal", true},
		{"no key", "", "/mcp/personal", false},
		{"unknown key", "nope", "/mcp/personal", false},
	}
	for _, c := range cases {
		if got := keyAllows(sc, c.key, c.path); got != c.want {
			t.Errorf("%s: keyAllows(%q, %q) = %v, want %v", c.name, c.key, c.path, got, c.want)
		}
	}
}

func TestKeyAllowsExpiry(t *testing.T) {
	keys := fmt.Sprintf(`    - hash: %q
      group: personal
      label: expired
      expires: 2020-01-01
    - hash: %q
      group: personal
      label: live
      expires: 2999-01-01T00:00:00Z
    - hash: %q
      group: personal
      label: never
`, hashKey("expired"), hashKey("live"), hashKey("never"))
	sc := parseYAML(t, testServerYAML(keys))
	cases := map[string]bool{"expired": false, "live": true, "never": true}
	for key, want := range cases {
		if got := keyAllows(sc, key, "/mcp/personal"); got != want {
			t.Errorf("key %q: keyAllows = %v, want %v", key, got, want)
		}
	}
}

func TestExpiryBoundary(t *testing.T) {
	instant := time.Date(2027, 2, 10, 0, 0, 0, 0, time.UTC)
	e := keyEntry{Expires: instant}
	if e.expired(instant.Add(-time.Second)) {
		t.Error("key expired before its expiry")
	}
	if !e.expired(instant) {
		t.Error("key live at its expiry instant")
	}
	if (keyEntry{}).expired(instant) {
		t.Error("key with no expiry must never lapse")
	}
}

func TestAuthMiddleware(t *testing.T) {
	sc := parseYAML(t, testServerYAML(testKeysYAML()))
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

func TestExpiredKeyIsUnauthorized(t *testing.T) {
	keys := fmt.Sprintf(`    - hash: %q
      group: personal
      label: old
      expires: 2020-01-01
`, hashKey("old-key"))
	sc := parseYAML(t, testServerYAML(keys))
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)
	req := httptest.NewRequest(http.MethodPost, "/mcp/personal", nil)
	req.Header.Set("Authorization", "Bearer old-key")
	rec := httptest.NewRecorder()
	authMiddleware(staticAuthState(sc), ok).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expired key got %d, want 401", rec.Code)
	}
	if !strings.Contains(logged.String(), "expired") {
		t.Errorf("log does not report the expiry: %q", logged.String())
	}
	if strings.Contains(logged.String(), "old-key") {
		t.Errorf("log contains the plaintext key: %q", logged.String())
	}

	// A public path needs no key at all, so an expired key is irrelevant there.
	req = httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer old-key")
	rec = httptest.NewRecorder()
	authMiddleware(staticAuthState(sc), ok).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expired key on a public path got %d, want 200", rec.Code)
	}
}

func TestDuplicateKeyRejected(t *testing.T) {
	body := testServerYAML(fmt.Sprintf(`    - hash: %q
      group: personal
    - hash: %q
      group: laptop
`, hashKey("dup"), hashKey("dup")))
	_, err := parseServerConfig([]byte(body), "test.yml")
	if err == nil {
		t.Fatal("duplicate key accepted")
	}
	for _, want := range []string{"twice", "personal", "laptop", shortHash(hashKey("dup"))} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestDuplicateWithinOneGroupRejected(t *testing.T) {
	body := testServerYAML(fmt.Sprintf(`    - hash: %q
      group: personal
    - hash: %q
      group: personal
`, hashKey("dup"), hashKey("dup")))
	_, err := parseServerConfig([]byte(body), "test.yml")
	if err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("want duplicate error, got %v", err)
	}
}

func TestKeyGroupMustExist(t *testing.T) {
	body := testServerYAML(fmt.Sprintf(`    - hash: %q
      group: nope
`, hashKey("k")))
	_, err := parseServerConfig([]byte(body), "test.yml")
	if err == nil || !strings.Contains(err.Error(), `"nope"`) {
		t.Fatalf("want undefined-group error naming nope, got %v", err)
	}
	// Group names are keys, not a case-folded namespace: one spelling exists.
	body = testServerYAML(fmt.Sprintf(`    - hash: %q
      group: Personal
`, hashKey("k")))
	if _, err := parseServerConfig([]byte(body), "test.yml"); err == nil ||
		!strings.Contains(err.Error(), `"Personal"`) {
		t.Fatalf("a group name differing only in case was matched, or wrong error: %v", err)
	}
}

func TestKeyDoesNotReachNestedMount(t *testing.T) {
	body := fmt.Sprintf(`servers:
  /mcp: index
  /mcp/hr: hr
auth:
  groups:
    general:
      allow: ["/mcp"]
    hr:
      allow: ["/mcp/hr"]
  keys:
    - hash: %q
      group: general
    - hash: %q
      group: hr
`, hashKey("general-key"), hashKey("hr-key"))
	sc := parseYAML(t, body)
	cases := []struct {
		key, path string
		want      bool
	}{
		{"general-key", "/mcp", true},
		{"general-key", "/mcp/hr", false},
		{"hr-key", "/mcp/hr", true},
		{"hr-key", "/mcp", false},
	}
	for _, c := range cases {
		if got := keyAllows(sc, c.key, c.path); got != c.want {
			t.Errorf("keyAllows(%q, %q) = %v, want %v", c.key, c.path, got, c.want)
		}
	}

	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := authMiddleware(staticAuthState(sc), ok)
	for _, c := range []struct {
		path string
		want int
	}{{"/mcp", http.StatusOK}, {"/mcp/hr", http.StatusUnauthorized}, {"/mcp/hr/x", http.StatusNotFound}} {
		req := httptest.NewRequest(http.MethodPost, c.path, nil)
		req.Header.Set("Authorization", "Bearer general-key")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s with a key allowed on /mcp got %d, want %d", c.path, rec.Code, c.want)
		}
	}
}

func TestKeyGroupRequired(t *testing.T) {
	body := testServerYAML(fmt.Sprintf(`    - hash: %q
`, hashKey("k")))
	_, err := parseServerConfig([]byte(body), "test.yml")
	if err == nil || !strings.Contains(err.Error(), "no group") {
		t.Fatalf("want missing-group error, got %v", err)
	}
}

func TestBadHashRejected(t *testing.T) {
	for _, bad := range []string{"nope", "sha256:abc", "sha256:" + strings.Repeat("z", 64)} {
		body := testServerYAML(fmt.Sprintf("    - hash: %q\n      group: personal\n", bad))
		if _, err := parseServerConfig([]byte(body), "test.yml"); err == nil {
			t.Errorf("hash %q accepted", bad)
		}
	}
}

func TestBareHexHashNormalized(t *testing.T) {
	h := strings.TrimPrefix(hashKey("k"), "sha256:")
	body := testServerYAML(fmt.Sprintf("    - hash: %q\n      group: personal\n", h))
	sc := parseYAML(t, body)
	if _, ok := sc.byHash[hashKey("k")]; !ok {
		t.Error("bare hex hash not normalized to sha256: form")
	}
}

func TestBadTimestampRejected(t *testing.T) {
	body := testServerYAML(fmt.Sprintf(`    - hash: %q
      group: personal
      expires: tomorrow
`, hashKey("k")))
	_, err := parseServerConfig([]byte(body), "test.yml")
	if err == nil || !strings.Contains(err.Error(), "expires") {
		t.Fatalf("want expires error, got %v", err)
	}
	// A zero-year expiry parses as the zero time, which keyEntry reads as "no
	// expiry", so the loader must refuse it rather than issue a key that never
	// lapses.
	for _, zero := range []string{"0001-01-01", "0001-01-01T00:00:00Z"} {
		for _, field := range []string{"expires", "issued"} {
			body := testServerYAML(fmt.Sprintf("    - hash: %q\n      group: personal\n      %s: %s\n",
				hashKey("k"), field, zero))
			_, err := parseServerConfig([]byte(body), "test.yml")
			if err == nil || !strings.Contains(err.Error(), field) || !strings.Contains(err.Error(), "zero time") {
				t.Errorf("%s: %q accepted, or wrong error: %v", field, zero, err)
			}
		}
	}
}

func TestLegacyKeyFormatRejected(t *testing.T) {
	body := `servers:
  /mcp: index
auth:
  groups:
    personal:
      allow: [/mcp]
      keys: ["legacy-key"]
`
	_, err := parseServerConfig([]byte(body), "test.yml")
	if err == nil {
		t.Fatal("legacy keys accepted")
	}
	for _, want := range []string{"personal", "auth.keys"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestLegacyKeyHashesRejected(t *testing.T) {
	body := `servers:
  /mcp: index
auth:
  groups:
    personal:
      allow: [/mcp]
      key_hashes:
        - hash: "sha256:ab12"
          label: laptop
`
	if _, err := parseServerConfig([]byte(body), "test.yml"); err == nil {
		t.Fatal("legacy key_hashes accepted")
	}
}

func TestParseKeyTime(t *testing.T) {
	cases := []struct {
		in   string
		want time.Time
		ok   bool
	}{
		{"", time.Time{}, true},
		{"2027-02-10", time.Date(2027, 2, 10, 0, 0, 0, 0, time.UTC), true},
		{"2028-02-29", time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC), true},
		{"2027-02-29", time.Time{}, false},
		{"2027-02-10T12:00:00Z", time.Date(2027, 2, 10, 12, 0, 0, 0, time.UTC), true},
		{"2027-02-10T12:00:00+02:00", time.Date(2027, 2, 10, 10, 0, 0, 0, time.UTC), true},
		{"10/02/2027", time.Time{}, false},
		{"never", time.Time{}, false},
		// The zero time is what "unset" holds, so these must not read as "no
		// expiry".
		{"0001-01-01", time.Time{}, false},
		{"0001-01-01T00:00:00Z", time.Time{}, false},
	}
	for _, c := range cases {
		got, err := parseKeyTime(c.in)
		if c.ok && err != nil {
			t.Errorf("parseKeyTime(%q): %v", c.in, err)
			continue
		}
		if !c.ok {
			if err == nil {
				t.Errorf("parseKeyTime(%q) accepted", c.in)
			}
			continue
		}
		if !got.Equal(c.want) {
			t.Errorf("parseKeyTime(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestGroupNamesWithWhitespaceRejected(t *testing.T) {
	h := hashKey("k")
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "group key",
			body: `servers:
  /mcp: index
auth:
  groups:
    " work ":
      allow: [/mcp]
`,
			want: "whitespace",
		},
		{
			name: "empty group key",
			body: `servers:
  /mcp: index
auth:
  groups:
    "":
      allow: [/mcp]
`,
			want: "empty group name",
		},
		{
			name: "key record",
			body: `servers:
  /mcp: index
auth:
  groups:
    work:
      allow: [/mcp]
  keys:
    - hash: "` + h + `"
      group: " work"
`,
			want: "whitespace",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseServerConfig([]byte(c.body), "test.yml")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q error, got %v", c.want, err)
			}
		})
	}
}

func TestEmptyAllowPathRejected(t *testing.T) {
	body := `servers:
  /mcp: index
auth:
  groups:
    personal:
      allow: [""]
  keys:
    - hash: "` + hashKey("k") + `"
      group: personal
`
	_, err := parseServerConfig([]byte(body), "test.yml")
	if err == nil || !strings.Contains(err.Error(), "empty allow path") {
		t.Fatalf("want empty-allow error, got %v", err)
	}
}

func TestDuplicateHashVariantsRejected(t *testing.T) {
	h := hashKey("dup")
	bare := strings.TrimPrefix(h, "sha256:")
	for _, variant := range []string{strings.ToUpper(bare), "sha256:" + strings.ToUpper(bare), "  " + h + " "} {
		body := testServerYAML(fmt.Sprintf(`    - hash: %q
      group: personal
    - hash: %q
      group: laptop
`, h, variant))
		if _, err := parseServerConfig([]byte(body), "test.yml"); err == nil || !strings.Contains(err.Error(), "twice") {
			t.Errorf("variant %q accepted or wrong error: %v", variant, err)
		}
	}
}

func TestMixedCaseHashNormalized(t *testing.T) {
	h := strings.TrimPrefix(hashKey("k"), "sha256:")
	body := testServerYAML(fmt.Sprintf("    - hash: %q\n      group: personal\n", "sha256:"+strings.ToUpper(h)))
	sc := parseYAML(t, body)
	if _, ok := sc.byHash[hashKey("k")]; !ok {
		t.Error("mixed-case hash was not normalized")
	}
}

func TestAuthStateReloadAppliesKeys(t *testing.T) {
	p := writeServerYAML(t, testServerYAML(testKeysYAML()))
	sc, err := loadServerConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	state := newAuthState(p, sc)
	if !keyAllows(state.current(), "plain-text-key", "/mcp/personal") {
		t.Fatal("initial key denied")
	}
	body := testServerYAML(fmt.Sprintf(`    - hash: %q
      group: personal
`, hashKey("new-key")))
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded := state.current()
	if keyAllows(reloaded, "plain-text-key", "/mcp/personal") {
		t.Error("removed key still works after reload")
	}
	if !keyAllows(reloaded, "new-key", "/mcp/personal") {
		t.Error("new key denied after reload")
	}
}

func TestAuthStateReloadKeepsPreviousOnInvalid(t *testing.T) {
	p := writeServerYAML(t, testServerYAML(testKeysYAML()))
	sc, err := loadServerConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	state := newAuthState(p, sc)
	body := testServerYAML(fmt.Sprintf(`    - hash: %q
      group: personal
    - hash: %q
      group: laptop
`, hashKey("dup"), hashKey("dup")))
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded := state.current()
	if !keyAllows(reloaded, "plain-text-key", "/mcp/personal") {
		t.Error("invalid reload discarded the previous config")
	}
	if keyAllows(reloaded, "dup", "/mcp/personal") {
		t.Error("invalid reload widened access")
	}
}

func TestAuthStateReloadWithoutKeysDeniesPrivate(t *testing.T) {
	p := writeServerYAML(t, testServerYAML(testKeysYAML()))
	sc, err := loadServerConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	state := newAuthState(p, sc)
	if err := os.WriteFile(p, []byte(testServerYAML("    []\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded := state.current()
	if keyAllows(reloaded, "plain-text-key", "/mcp/personal") {
		t.Error("key still works after auth.keys became empty")
	}
}

func TestLegacyKeyShapesRejected(t *testing.T) {
	for _, field := range []string{
		`keys: "legacy"`,
		`keys: {a: b}`,
		`key_hashes: "sha256:ab12"`,
		`key_hashes: {a: b}`,
	} {
		body := fmt.Sprintf(`servers:
  /mcp: index
auth:
  groups:
    personal:
      allow: [/mcp]
      %s
`, field)
		_, err := parseServerConfig([]byte(body), "test.yml")
		if err == nil || !strings.Contains(err.Error(), `group "personal"`) || !strings.Contains(err.Error(), "auth.keys") {
			t.Errorf("legacy field %q: want group and auth.keys error, got %v", field, err)
		}
	}
}

func TestLegacyKeysThroughIndirectionRejected(t *testing.T) {
	cases := map[string]string{
		"auth alias": `servers:
  /mcp: index
legacy: &legacy
  groups:
    personal:
      allow: [/mcp]
      keys: ["legacy"]
auth: *legacy
`,
		"groups alias": `servers:
  /mcp: index
legacy: &legacy
  personal:
    allow: [/mcp]
    keys: ["legacy"]
auth:
  groups: *legacy
`,
		"group alias": `servers:
  /mcp: index
legacy: &legacy
  allow: [/mcp]
  keys: ["legacy"]
auth:
  groups:
    personal: *legacy
`,
		"merge key": `servers:
  /mcp: index
legacy: &legacy
  keys: ["legacy"]
auth:
  groups:
    personal:
      <<: *legacy
      allow: [/mcp]
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parseServerConfig([]byte(body), "test.yml")
			if err == nil || !strings.Contains(err.Error(), `group "personal"`) || !strings.Contains(err.Error(), "auth.keys") {
				t.Fatalf("want legacy error naming the group, got %v", err)
			}
		})
	}
}

func TestNewAuthStateUsesCurrentFile(t *testing.T) {
	p := writeServerYAML(t, testServerYAML(testKeysYAML()))
	sc, err := loadServerConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	body := testServerYAML(fmt.Sprintf(`    - hash: %q
      group: personal
`, hashKey("new-key")))
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	state := newAuthState(p, sc)
	got := state.current()
	if keyAllows(got, "plain-text-key", "/mcp/personal") {
		t.Error("state kept the startup config after the file changed")
	}
	if !keyAllows(got, "new-key", "/mcp/personal") {
		t.Error("state did not pick up the file contents")
	}
}

func TestAuthMiddlewareSeesReloadedKeys(t *testing.T) {
	p := writeServerYAML(t, testServerYAML(testKeysYAML()))
	sc, err := loadServerConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := authMiddleware(newAuthState(p, sc), ok)
	body := testServerYAML(fmt.Sprintf(`    - hash: %q
      group: personal
`, hashKey("new-key")))
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		key  string
		want int
	}{
		{"new-key", http.StatusOK},
		{"plain-text-key", http.StatusUnauthorized},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodPost, "/mcp/personal", nil)
		req.Header.Set("Authorization", "Bearer "+c.key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("key %q got %d, want %d", c.key, rec.Code, c.want)
		}
	}
}

func TestAuthMiddlewareKeepsStartupMounts(t *testing.T) {
	p := writeServerYAML(t, fmt.Sprintf(`servers:
  /mcp: index
  /mcp/hr: hr
auth:
  groups:
    hr:
      allow: ["/mcp/hr"]
  keys:
    - hash: %q
      group: hr
`, hashKey("hr-key")))
	sc, err := loadServerConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	// The router is built once, from the startup mounts.
	mux := http.NewServeMux()
	for path, idx := range sc.Servers {
		name := idx
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, name) })
		mux.Handle(path, h)
		mux.Handle(path+"/", h)
	}
	mx := authMiddleware(newAuthState(p, sc), mux)

	// Move the hr mount under the index mount. Reloaded, /mcp/x is in the new
	// servers map and the group allows it, but /mcp/ is routed to the index
	// index, so honouring the new map would serve index to an hr key.
	body := fmt.Sprintf(`servers:
  /mcp: index
  /mcp/x: hr
auth:
  groups:
    hr:
      allow: ["/mcp/x"]
  keys:
    - hash: %q
      group: hr
`, hashKey("hr-key"))
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		path string
		want int
	}{{"/mcp/x", http.StatusNotFound}, {"/mcp/hr", http.StatusUnauthorized}}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodPost, c.path, nil)
		req.Header.Set("Authorization", "Bearer hr-key")
		rec := httptest.NewRecorder()
		mx.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s got %d, want %d (body %q)", c.path, rec.Code, c.want, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "index") {
			t.Errorf("%s was routed to the index index: %q", c.path, rec.Body.String())
		}
	}
}

func TestAuthMiddlewareSeesReloadedPublicPaths(t *testing.T) {
	p := writeServerYAML(t, testServerYAML(testKeysYAML()))
	sc, err := loadServerConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := authMiddleware(newAuthState(p, sc), ok)
	probe := func(path string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
		return rec.Code
	}
	if got := probe("/mcp/personal"); got != http.StatusUnauthorized {
		t.Fatalf("gated path before reload: %d", got)
	}
	body := strings.Replace(testServerYAML(testKeysYAML()),
		"public: [/mcp, /mcp/shared]", "public: [/mcp, /mcp/shared, /mcp/personal]", 1)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := probe("/mcp/personal"); got != http.StatusOK {
		t.Errorf("added public path after reload: %d, want 200", got)
	}
	body = strings.Replace(testServerYAML(testKeysYAML()),
		"public: [/mcp, /mcp/shared]", "public: [/mcp/shared]", 1)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := probe("/mcp"); got != http.StatusUnauthorized {
		t.Errorf("removed public path after reload: %d, want 401", got)
	}
}

func TestReservedAndUnregisterableMountsRejected(t *testing.T) {
	for _, mount := range []string{"/healthz", "/healthz/", "/healthz//", "healthz", "/a b", "/a\tb", "/a\x00b", "/a{b", "/a}b"} {
		body := fmt.Sprintf("servers:\n  %q: index\n", mount)
		_, err := parseServerConfig([]byte(body), "test.yml")
		if err == nil {
			t.Errorf("mount %q accepted; http.ServeMux cannot register it", mount)
			continue
		}
		if !strings.Contains(err.Error(), "cannot use") && !strings.Contains(err.Error(), "reserved") {
			t.Errorf("mount %q: unexpected error %v", mount, err)
		}
	}
	sc := parseYAML(t, "servers:\n  /mcp/v1.0: index\n")
	if _, ok := sc.Servers["/mcp/v1.0"]; !ok {
		t.Errorf("plain path rejected: %v", sc.Servers)
	}
}

func TestAuthStateConcurrentReload(t *testing.T) {
	p := writeServerYAML(t, testServerYAML(testKeysYAML()))
	sc, err := loadServerConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	state := newAuthState(p, sc)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(stop)
		body := []byte(testServerYAML(testKeysYAML()))
		for i := 0; i < 100; i++ {
			_ = os.Remove(p)
			_ = os.WriteFile(p, body, 0o600)
		}
	}()
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					if state.current() == nil {
						t.Error("current returned nil")
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	final := []byte(testServerYAML(fmt.Sprintf(`    - hash: %q
      group: personal
`, hashKey("final-key"))))
	if err := os.WriteFile(p, final, 0o600); err != nil {
		t.Fatal(err)
	}
	if !keyAllows(state.current(), "final-key", "/mcp/personal") {
		t.Error("state did not pick up the last write")
	}
}
