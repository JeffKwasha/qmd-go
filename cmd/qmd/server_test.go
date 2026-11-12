package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestAddServerKeyStoresOnlyHash(t *testing.T) {
	p := writeServerYAML(t, testServerYAML(testKeysYAML()))
	key, fp, err := addServerKey(p, keyAddOptions{Group: "personal", Label: "phone"})
	if err != nil {
		t.Fatal(err)
	}
	if key == "" || len(key) != 48 {
		t.Fatalf("key = %q, want 48 hex chars", key)
	}
	if !strings.HasPrefix(fp, "sha256:") {
		t.Fatalf("fingerprint = %q", fp)
	}
	body, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if strings.Contains(text, key) {
		t.Fatal("plaintext key written to server.yml")
	}
	if !strings.Contains(text, "# mounts and auth") {
		t.Error("comments were dropped")
	}
	if strings.Contains(text, "key_hashes") {
		t.Error("new key written in the legacy layout")
	}
	sc, err := loadServerConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	e := sc.byHash[hashKey(key)]
	if e == nil {
		t.Fatal("new key not in the config")
	}
	if e.Group != "personal" || e.Label != "phone" {
		t.Errorf("entry = %+v, want group personal, label phone", e)
	}
	if !keyAllows(sc, key, "/mcp/personal") {
		t.Error("new key cannot reach its group's path")
	}
	if !e.Issued.Equal(e.Issued.Truncate(time.Second)) || e.Issued.IsZero() {
		t.Errorf("issued = %s, want a timestamp", e.Issued)
	}
	if !e.Expires.IsZero() {
		t.Errorf("expires = %s, want never", e.Expires)
	}
	if !keyAllows(sc, "plain-text-key", "/mcp/personal") {
		t.Error("existing key stopped working")
	}
}

func TestAddServerKeyWithExpiry(t *testing.T) {
	p := writeServerYAML(t, testServerYAML(testKeysYAML()))
	expiry := time.Now().Add(30 * 24 * time.Hour).UTC().Truncate(time.Second)
	key, _, err := addServerKey(p, keyAddOptions{Group: "laptop", Expires: expiry})
	if err != nil {
		t.Fatal(err)
	}
	sc, err := loadServerConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	e := sc.byHash[hashKey(key)]
	if e == nil {
		t.Fatal("new key not in the config")
	}
	if !e.Expires.Equal(expiry) {
		t.Errorf("expires = %s, want %s", e.Expires, expiry)
	}
	if !keyAllows(sc, key, "/mcp/personal") {
		t.Error("live key denied")
	}
}

func TestAddServerKeySetsAllow(t *testing.T) {
	p := writeServerYAML(t, testServerYAML(testKeysYAML()))
	key, _, err := addServerKey(p, keyAddOptions{Group: "phone", Allow: []string{"/mcp"}})
	if err != nil {
		t.Fatal(err)
	}
	sc, err := loadServerConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if !keyAllows(sc, key, "/mcp") {
		t.Error("new key cannot reach the path given to --allow")
	}
	if keyAllows(sc, key, "/mcp/personal") {
		t.Error("key reaches a path outside its group")
	}
}

func TestAddServerKeySeveralAllowPaths(t *testing.T) {
	p := writeServerYAML(t, testServerYAML(testKeysYAML()))
	key, _, err := addServerKey(p, keyAddOptions{
		Group: "phone",
		Allow: []string{"/mcp", "/mcp/personal//"},
	})
	if err != nil {
		t.Fatal(err)
	}
	sc, err := loadServerConfig(p)
	if err != nil {
		t.Fatalf("config written by key add does not load: %v", err)
	}
	if got := sc.Auth.Groups["phone"].Allow; len(got) != 2 {
		t.Errorf("allow = %v, want both paths", got)
	}
	for _, path := range []string{"/mcp", "/mcp/personal"} {
		if !keyAllows(sc, key, path) {
			t.Errorf("new key cannot reach %s", path)
		}
	}
	if keyAllows(sc, key, "/mcp/shared") {
		t.Error("key reaches a path outside its group")
	}
}

func TestAddServerKeyRefusesLegacyConfig(t *testing.T) {
	p := writeServerYAML(t, `servers:
  /mcp: index
auth:
  groups:
    personal:
      allow: [/mcp]
      keys: ["legacy"]
`)
	_, _, err := addServerKey(p, keyAddOptions{Group: "personal"})
	if err == nil || !strings.Contains(err.Error(), "auth.keys") {
		t.Fatalf("want auth.keys hint, got %v", err)
	}
}

func TestRemoveServerKey(t *testing.T) {
	p := writeServerYAML(t, testServerYAML(testKeysYAML()))
	key, fp, err := addServerKey(p, keyAddOptions{Group: "personal", Label: "temp"})
	if err != nil {
		t.Fatal(err)
	}
	n, err := removeServerKey(p, fp[:len("sha256:")+8])
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("removed %d, want 1", n)
	}
	sc, err := loadServerConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if keyAllows(sc, key, "/mcp/personal") {
		t.Error("removed key still works")
	}
	if !keyAllows(sc, "plain-text-key", "/mcp/personal") {
		t.Error("removal broke an unrelated key")
	}
}

func TestRemoveServerKeyAmbiguousPrefix(t *testing.T) {
	p := writeServerYAML(t, testServerYAML(testKeysYAML()))
	before, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	_, err = removeServerKey(p, "sha256:")
	if err == nil || !strings.Contains(err.Error(), "matches 2 keys") {
		t.Fatalf("want ambiguous-prefix error, got %v", err)
	}
	after, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("ambiguous removal changed the file")
	}
}

func TestRemoveServerKeyUnknown(t *testing.T) {
	p := writeServerYAML(t, testServerYAML(testKeysYAML()))
	if _, err := removeServerKey(p, "sha256:deadbeef"); err == nil {
		t.Fatal("removing a missing key succeeded")
	}
}

func TestAddServerKeyRejectsPastExpiry(t *testing.T) {
	p := writeServerYAML(t, testServerYAML(testKeysYAML()))
	before, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = addServerKey(p, keyAddOptions{Group: "personal", Expires: time.Now().Add(-time.Hour)})
	if err == nil || !strings.Contains(err.Error(), "past") {
		t.Fatalf("want past-expiry error, got %v", err)
	}
	after, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("rejected key add changed the file")
	}
}

func TestRemoveServerKeyRepairsDuplicate(t *testing.T) {
	dup := hashKey("dup")
	body := testServerYAML(fmt.Sprintf(`    - hash: %q
      group: personal
    - hash: %q
      group: laptop
    - hash: %q
      group: personal
`, dup, dup, hashKey("other")))
	p := writeServerYAML(t, body)
	if _, err := loadServerConfig(p); err == nil {
		t.Fatal("duplicate config loaded")
	}
	n, err := removeServerKey(p, shortHash(dup))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("removed %d records, want 2", n)
	}
	sc, err := loadServerConfig(p)
	if err != nil {
		t.Fatalf("repaired config does not load: %v", err)
	}
	if len(sc.byHash) != 1 || sc.byHash[hashKey("other")] == nil {
		t.Errorf("wrong keys survived: %+v", sc.byHash)
	}
}

// key remove is the documented repair for a duplicate the loader refuses, so it
// must see hashes the loader sees through aliases and merge keys.
func TestRemoveServerKeyRepairsDuplicateWrittenIndirectly(t *testing.T) {
	dup := hashKey("dup")
	other := hashKey("other")
	cases := map[string]string{
		"merge key": fmt.Sprintf(`servers:
  /mcp: index
auth:
  groups:
    personal:
      allow: [/mcp]
    laptop:
      allow: [/mcp]
  keys:
    - <<: &rec
        hash: %q
        group: personal
      label: first
    - <<: *rec
      group: laptop
      label: second
    - hash: %q
      group: personal
`, dup, other),
		"alias entry": fmt.Sprintf(`servers:
  /mcp: index
auth:
  groups:
    personal:
      allow: [/mcp]
  keys:
    - &rec
      hash: %q
      group: personal
      label: first
    - *rec
    - hash: %q
      group: personal
`, dup, other),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p := writeServerYAML(t, body)
			if _, err := loadServerConfig(p); err == nil || !strings.Contains(err.Error(), "twice") {
				t.Fatalf("want a duplicate error before repair, got %v", err)
			}
			n, err := removeServerKey(p, shortHash(dup))
			if err != nil {
				t.Fatalf("cannot repair a duplicate written with %s: %v", name, err)
			}
			if n != 2 {
				t.Errorf("removed %d records, want 2", n)
			}
			sc, err := loadServerConfig(p)
			if err != nil {
				t.Fatalf("repaired config does not load: %v", err)
			}
			out, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(out), strings.TrimPrefix(dup, "sha256:")) {
				t.Errorf("target hash survived removal:\n%s", out)
			}
			if len(sc.byHash) != 1 || sc.byHash[other] == nil {
				t.Errorf("wrong keys survived: %+v", sc.byHash)
			}
		})
	}
}

func TestRemoveServerKeyAmbiguousPrefixListsCandidates(t *testing.T) {
	p := writeServerYAML(t, testServerYAML(testKeysYAML()))
	_, err := removeServerKey(p, "sha256:")
	if err == nil || !strings.Contains(err.Error(), `group "personal"`) {
		t.Fatalf("want candidates with groups, got %v", err)
	}
}

func TestListServerKeys(t *testing.T) {
	p := writeServerYAML(t, testServerYAML(testKeysYAML()))
	entries, err := listServerKeys(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	for _, e := range entries {
		if e.Hash == hashKey("hashed-key") {
			if e.Group != "laptop" || e.Label != "laptop" {
				t.Errorf("entry = %+v, want group and label laptop", e)
			}
			if len(e.Allow) != 1 || e.Allow[0] != "/mcp/personal" {
				t.Errorf("allow = %v, want [/mcp/personal]", e.Allow)
			}
		}
	}
}

func TestCheckServerConfig(t *testing.T) {
	t.Setenv("QMD_CONFIG_DIR", t.TempDir())
	expired := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	soon := time.Now().Add(7 * 24 * time.Hour).UTC().Format(time.RFC3339)
	body := fmt.Sprintf(`servers:
  /mcp: index
  /mcp/stale: missing
auth:
  groups:
    empty:
      allow: [/mcp]
    expired:
      allow: [/mcp]

  keys:
    - hash: %q
      group: expired
      expires: %s
    - hash: %q
      group: expired
      expires: %s
`, hashKey("a"), expired, hashKey("b"), soon)
	p := writeServerYAML(t, body)
	findings, err := checkServerConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(findings, "\n")
	for _, want := range []string{"missing", "expired on", "expires on", "no keys"} {
		if !strings.Contains(joined, want) {
			t.Errorf("findings missing %q:\n%s", want, joined)
		}
	}
	if !sort.StringsAreSorted(findings) {
		t.Errorf("findings are not sorted:\n%s", joined)
	}
}

func TestCheckServerConfigFindings(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("QMD_CONFIG_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "index.yml"), []byte("collections: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`servers:
  /mcp: index
  /mcp/work: work
auth:
  public: [/mcp/typo]
  groups:
    used:
      allow: [/mcp]
    unused:
      allow: [/mcp]
  keys:
    - hash: %q
      group: used
      issued: 2027-06-01
      expires: 2027-01-01
`, hashKey("k"))
	p := writeServerYAML(t, body)
	findings, err := checkServerConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(findings, "\n")
	if strings.Contains(joined, `index "index"`) {
		t.Errorf("index.yml exists, but it is reported missing:\n%s", joined)
	}
	for _, want := range []string{
		`index "work"`,
		"public path /mcp/typo is not mounted",
		`group "unused" has no keys`,
		"expires on 2027-01-01, before it was issued on 2027-06-01",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("findings missing %q:\n%s", want, joined)
		}
	}
}

func TestCheckReportsDuplicateAsError(t *testing.T) {
	body := testServerYAML(fmt.Sprintf(`    - hash: %q
      group: personal
    - hash: %q
      group: laptop
`, hashKey("dup"), hashKey("dup")))
	p := writeServerYAML(t, body)
	_, err := checkServerConfig(p)
	if err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("want duplicate error from check, got %v", err)
	}
}

func TestCheckEqualIssuedAndExpires(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("QMD_CONFIG_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "index.yml"), []byte("collections: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`servers:
  /mcp: index
auth:
  groups:
    work:
      allow: [/mcp]
  keys:
    - hash: %q
      group: work
      issued: 2020-01-01
      expires: 2020-01-01
`, hashKey("zero-life"))
	p := writeServerYAML(t, body)
	findings, err := checkServerConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(findings, "\n")
	if !strings.Contains(joined, "expired on 2020-01-01") {
		t.Errorf("a key whose expiry equals its issue date is not reported expired:\n%s", joined)
	}
	if strings.Contains(joined, "before it was issued") {
		t.Errorf("equal timestamps reported as expired-before-issued:\n%s", joined)
	}
}

func TestCheckReportsUnreachableConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("QMD_CONFIG_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "index.yml"), []byte("collections: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := writeServerYAML(t, `servers:
  /mcp: index
auth:
  groups:
    work:
      allow: [/mcp]
`)
	findings, err := checkServerConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(findings, "\n")
	if !strings.Contains(joined, "every mount answers 401") {
		t.Errorf("findings do not report an unreachable server:\n%s", joined)
	}

	p = writeServerYAML(t, `servers:
  /mcp: index
auth:
  public: [/mcp]
`)
	findings, err = checkServerConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(findings, "\n"), "every mount answers 401") {
		t.Error("public-only config reported as unreachable")
	}
}

func TestParseExpires(t *testing.T) {
	now := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		in   string
		want time.Time
		ok   bool
	}{
		{"", time.Time{}, true},
		{"never", time.Time{}, true},
		{"30d", now.Add(30 * 24 * time.Hour), true},
		{"12h", now.Add(12 * time.Hour), true},
		{"2027-03-01", time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC), true},
		{"2027-03-01T06:00:00Z", time.Date(2027, 3, 1, 6, 0, 0, 0, time.UTC), true},
		{"soon", time.Time{}, false},
		{"30", time.Time{}, false},
		{"0d", time.Time{}, false},
		{"-1d", time.Time{}, false},
		{"d", time.Time{}, false},
		// Parsed as the zero time, which addServerKey stores as "never": the
		// opposite of the past date the user asked for.
		{"0001-01-01", time.Time{}, false},
		{"0001-01-01T00:00:00Z", time.Time{}, false},
	}
	for _, c := range cases {
		got, err := parseExpires(c.in, now)
		if c.ok != (err == nil) {
			t.Errorf("parseExpires(%q) error = %v, want ok=%v", c.in, err, c.ok)
			continue
		}
		if c.ok && !got.Equal(c.want) {
			t.Errorf("parseExpires(%q) = %s, want %s", c.in, got, c.want)
		}
	}
	if _, err := parseExpires("2020-01-01", now); err != nil {
		t.Errorf("past expiry rejected: %v", err)
	}
}

func TestAddServerKeyToNullSections(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"auth null", `servers:
  /mcp: index
auth:
`},
		{"groups null", `servers:
  /mcp: index
auth:
  groups:
`},
		{"allow null", `servers:
  /mcp: index
auth:
  groups:
    personal:
      allow:
  keys: []
`},
		{"keys null", `servers:
  /mcp: index
auth:
  groups:
    personal:
      allow: [/mcp]
  keys: # key list
`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := writeServerYAML(t, c.body)
			key, _, err := addServerKey(p, keyAddOptions{Group: "personal", Allow: []string{"/mcp"}})
			if err != nil {
				t.Fatal(err)
			}
			sc, err := loadServerConfig(p)
			if err != nil {
				t.Fatalf("config written by key add does not load: %v", err)
			}
			if !keyAllows(sc, key, "/mcp") {
				t.Error("new key cannot reach its group's path")
			}
			body, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(c.body, "# key list") && !strings.Contains(string(body), "# key list") {
				t.Error("comment on keys was dropped")
			}
		})
	}
}

func TestAddServerKeyQuotesYAMLNullGroup(t *testing.T) {
	for _, group := range []string{"null", "~"} {
		t.Run(group, func(t *testing.T) {
			p := writeServerYAML(t, `servers:
  /mcp: index
auth:
  groups: {}
  keys: []
`)
			key, _, err := addServerKey(p, keyAddOptions{Group: group, Label: "null", Allow: []string{"/mcp"}})
			if err != nil {
				t.Fatal(err)
			}
			sc, err := loadServerConfig(p)
			if err != nil {
				t.Fatalf("config written by key add does not load: %v", err)
			}
			e := sc.byHash[hashKey(key)]
			if e == nil || e.Group != group || e.Label != "null" {
				t.Fatalf("entry = %+v, want group %q and label null", e, group)
			}
			if !keyAllows(sc, key, "/mcp") {
				t.Error("new key cannot reach its group's path")
			}
		})
	}
}

func TestAddServerKeyRejectsBadGroup(t *testing.T) {
	p := writeServerYAML(t, testServerYAML(testKeysYAML()))
	before, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range []string{"", " work "} {
		if _, _, err := addServerKey(p, keyAddOptions{Group: group}); err == nil {
			t.Errorf("group %q accepted", group)
		}
	}
	after, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("rejected key add changed the file")
	}
}

func TestAddServerKeyRejectsEmptyAllow(t *testing.T) {
	p := writeServerYAML(t, testServerYAML(testKeysYAML()))
	before, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, allow := range [][]string{{""}, {}} {
		if _, _, err := addServerKey(p, keyAddOptions{Group: "personal", Allow: allow}); err == nil {
			t.Errorf("allow %#v accepted", allow)
		}
	}
	after, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("rejected key add changed the file")
	}
}

func TestRemoveServerKeyFingerprintForms(t *testing.T) {
	if _, err := removeServerKey(writeServerYAML(t, testServerYAML(testKeysYAML())), ""); err == nil ||
		!strings.Contains(err.Error(), "fingerprint required") {
		t.Errorf("empty fingerprint accepted: %v", err)
	}
	for _, form := range []string{"full", "spaced uppercase full"} {
		p := writeServerYAML(t, testServerYAML(testKeysYAML()))
		key, _, err := addServerKey(p, keyAddOptions{Group: "personal"})
		if err != nil {
			t.Fatal(err)
		}
		fp := hashKey(key)
		if form == "spaced uppercase full" {
			fp = "  " + strings.ToUpper(fp) + " "
		}
		n, err := removeServerKey(p, fp)
		if err != nil || n != 1 {
			t.Fatalf("%s fingerprint: removed %d, err %v", form, n, err)
		}
		out, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(out), strings.TrimPrefix(hashKey(key), "sha256:")) {
			t.Errorf("%s fingerprint did not remove the record", form)
		}
	}
}

func TestAddServerKeyRefusesAliasedSections(t *testing.T) {
	cases := map[string]string{
		"auth": `servers:
  /mcp: index
base: &base
  groups:
    personal:
      allow: [/mcp]
  keys: []
auth: *base
`,
		"groups": `servers:
  /mcp: index
base: &base
  personal:
    allow: [/mcp]
auth:
  groups: *base
  keys: []
`,
		"keys": `servers:
  /mcp: index
base: &base
  - hash: "` + hashKey("old") + `"
    group: personal
auth:
  groups:
    personal:
      allow: [/mcp]
  keys: *base
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p := writeServerYAML(t, body)
			before, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := addServerKey(p, keyAddOptions{Group: "personal", Allow: []string{"/mcp"}}); err == nil {
				t.Fatal("key add rewrote a section written as an alias")
			}
			after, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Error("refused key add changed the file")
			}
			sc, err := loadServerConfig(p)
			if err != nil {
				t.Fatalf("refused key add broke the config: %v", err)
			}
			if len(sc.Servers) != 1 {
				t.Errorf("mounts changed: %v", sc.Servers)
			}
		})
	}
}

func TestRemoveServerKeyUppercaseFingerprint(t *testing.T) {
	p := writeServerYAML(t, testServerYAML(testKeysYAML()))
	key, fp, err := addServerKey(p, keyAddOptions{Group: "personal"})
	if err != nil {
		t.Fatal(err)
	}
	n, err := removeServerKey(p, strings.ToUpper(fp))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("removed %d, want 1", n)
	}
	sc, err := loadServerConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if keyAllows(sc, key, "/mcp/personal") {
		t.Error("removed key still works")
	}
}

func TestRemoveServerKeyNullKeys(t *testing.T) {
	p := writeServerYAML(t, `servers:
  /mcp: index
auth:
  groups:
    personal:
      allow: [/mcp]
  keys:
`)
	if _, err := removeServerKey(p, "sha256:dead"); err == nil || !strings.Contains(err.Error(), "no key matches") {
		t.Fatalf("want no-key-matches error, got %v", err)
	}
}

func TestAddServerKeyPreservesMode(t *testing.T) {
	p := writeServerYAML(t, testServerYAML(testKeysYAML()))
	if err := os.Chmod(p, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, _, err := addServerKey(p, keyAddOptions{Group: "personal"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640", info.Mode().Perm())
	}
}

func TestAddServerKeyRoundTripsScalarNames(t *testing.T) {
	groups := []string{"on", "no", "1e3", "0x10", "null", "~", "grüße", "a.b-c_d", "-x"}
	labels := []string{"", "laptop", "a #b: c", " leading", "1e3"}
	for _, g := range groups {
		for _, l := range labels {
			p := writeServerYAML(t, `servers:
  /mcp: index
auth:
  groups: {}
  keys: []
`)
			key, _, err := addServerKey(p, keyAddOptions{Group: g, Label: l, Allow: []string{"/mcp"}})
			if err != nil {
				t.Fatalf("group %q label %q: %v", g, l, err)
			}
			sc, err := loadServerConfig(p)
			if err != nil {
				t.Fatalf("group %q label %q: written config does not load: %v", g, l, err)
			}
			e := sc.byHash[hashKey(key)]
			if e == nil || e.Group != g || e.Label != l {
				t.Fatalf("group %q label %q: entry = %+v", g, l, e)
			}
			if !keyAllows(sc, key, "/mcp") {
				t.Errorf("group %q label %q: new key cannot reach its path", g, l)
			}
		}
	}
}

func TestAddServerKeyRefusesMergedSections(t *testing.T) {
	cases := map[string]string{
		"auth": `servers:
  /mcp: index
base: &base
  groups:
    personal:
      allow: [/mcp]
  keys:
    - hash: "` + hashKey("old") + `"
      group: personal
auth:
  <<: *base
`,
		"groups": `servers:
  /mcp: index
base: &base
  personal:
    allow: [/mcp]
auth:
  groups:
    <<: *base
  keys:
    - hash: "` + hashKey("old") + `"
      group: personal
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p := writeServerYAML(t, body)
			before, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := addServerKey(p, keyAddOptions{Group: "personal"}); err == nil || !strings.Contains(err.Error(), "merge key") {
				t.Fatalf("want merge-key error, got %v", err)
			}
			after, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Error("refused key add changed the file")
			}
			sc, err := loadServerConfig(p)
			if err != nil {
				t.Fatal(err)
			}
			if sc.byHash[hashKey("old")] == nil {
				t.Error("merged key disappeared")
			}
		})
	}
}

func TestAddServerKeyRefusesMergedGroupAllow(t *testing.T) {
	body := `servers:
  /mcp: index
  /mcp/work: work
base: &base
  allow: ["/mcp", "/mcp/work"]
auth:
  groups:
    personal:
      <<: *base
  keys: []
`
	p := writeServerYAML(t, body)
	before, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := addServerKey(p, keyAddOptions{Group: "personal", Allow: []string{"/mcp"}}); err == nil || !strings.Contains(err.Error(), "merge key") {
		t.Fatalf("want merge-key error, got %v", err)
	}
	after, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("refused key add changed the file")
	}

	key, _, err := addServerKey(p, keyAddOptions{Group: "personal"})
	if err != nil {
		t.Fatalf("key add without --allow: %v", err)
	}
	sc, err := loadServerConfig(p)
	if err != nil {
		t.Fatalf("config written by key add does not load: %v", err)
	}
	if got := sc.Auth.Groups["personal"].Allow; len(got) != 2 {
		t.Errorf("allow = %v, want the merged paths", got)
	}
	if !keyAllows(sc, key, "/mcp/work") {
		t.Error("key lost a path its group inherits from the merge key")
	}
}

func TestAddServerKeyEditsExplicitSectionsUnderMerge(t *testing.T) {
	body := `servers:
  /mcp: index
shared: &shared
  public: [/mcp]
auth:
  <<: *shared
  groups: {}
  keys: []
`
	p := writeServerYAML(t, body)
	key, _, err := addServerKey(p, keyAddOptions{Group: "personal", Allow: []string{"/mcp"}})
	if err != nil {
		t.Fatalf("auth merge key with explicit groups and keys refused: %v", err)
	}
	sc, err := loadServerConfig(p)
	if err != nil {
		t.Fatalf("config written by key add does not load: %v", err)
	}
	if !isPublic(sc, "/mcp") {
		t.Error("merged public path was dropped")
	}
	if !keyAllows(sc, key, "/mcp") {
		t.Error("new key cannot reach its group's path")
	}
}

func TestRemoveServerKeyKeepsMalformedRecords(t *testing.T) {
	body := testServerYAML(fmt.Sprintf(`    - hash: "not-a-hash"
      group: personal
    - hash: %q
      group: personal
`, hashKey("target")))
	p := writeServerYAML(t, body)
	n, err := removeServerKey(p, shortHash(hashKey("target")))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("removed %d, want 1", n)
	}
	out, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "not-a-hash") {
		t.Errorf("removal dropped an unrelated record:\n%s", out)
	}
	if strings.Contains(string(out), strings.TrimPrefix(hashKey("target"), "sha256:")) {
		t.Errorf("target hash survived:\n%s", out)
	}
}
