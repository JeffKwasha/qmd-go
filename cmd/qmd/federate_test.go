package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ba0f3/qmd-go/internal/store"
)

func TestFuseListsKeepsSingleIndexScores(t *testing.T) {
	one := []fusedHit{{Index: "public", Filepath: "qmd://pub/a.md", Score: 7.5}}
	got := fuseLists([][]fusedHit{one})
	if len(got) != 1 || got[0].Score != 7.5 {
		t.Fatalf("single list was altered: %+v", got)
	}
}

func TestFuseListsMergesByRankAcrossIndexes(t *testing.T) {
	pub := []fusedHit{
		{Index: "public", Filepath: "qmd://pub/a.md"},
		{Index: "public", Filepath: "qmd://pub/b.md"},
	}
	priv := []fusedHit{
		{Index: "private", Filepath: "qmd://priv/b.md"},
		{Index: "private", Filepath: "qmd://pub/a.md"},
	}
	got := fuseLists([][]fusedHit{pub, priv})
	// Four entries: the same path under two indexes is two documents, so the
	// index belongs in the key.
	if len(got) != 4 {
		t.Fatalf("got %d hits, want 4: %+v", len(got), got)
	}
	// Both rank-one entries tie at the normalized top.
	if got[0].Score != 1.0 || got[1].Score != 1.0 {
		t.Errorf("winner scores = %v, %v; want 1.0 each", got[0].Score, got[1].Score)
	}
	seen := make(map[string]bool)
	for _, h := range got {
		if h.Index == "" {
			t.Errorf("hit lost its index: %+v", h)
		}
		seen[h.Index+"|"+h.Filepath] = true
	}
	if len(seen) != 4 {
		t.Errorf("hits were merged across indexes: %+v", got)
	}

	// Within one index the same path from two lists (hybrid) is one result.
	fts := []fusedHit{{Index: "public", Filepath: "qmd://pub/a.md"}}
	vec := []fusedHit{{Index: "public", Filepath: "qmd://pub/a.md"}}
	if merged := fuseLists([][]fusedHit{fts, vec}); len(merged) != 1 {
		t.Fatalf("hybrid lists were not merged: %+v", merged)
	}
}

func TestFuseListsScoresFallWithRank(t *testing.T) {
	var list []fusedHit
	for i := 0; i < 5; i++ {
		list = append(list, fusedHit{Index: "public", Filepath: "qmd://pub/doc" + string(rune('a'+i)) + ".md"})
	}
	other := []fusedHit{{Index: "private", Filepath: "qmd://priv/x.md"}}
	got := fuseLists([][]fusedHit{list, other})
	for i := 1; i < len(got); i++ {
		if got[i].Score > got[i-1].Score {
			t.Fatalf("scores are not descending: %+v", got)
		}
	}
	if got[len(got)-1].Score <= 0 {
		t.Errorf("last hit scored %v", got[len(got)-1].Score)
	}
}

func TestReachableIndexesFollowGrants(t *testing.T) {
	body := `servers:
  /mcp: public
  /mcp/pub: public
  /mcp/private: private
auth:
  public: [/mcp, /mcp/pub]
  groups:
    hermes-private:
      allow: [/mcp, /mcp/pub, /mcp/private]
  keys:
    - hash: "` + hashKey("key") + `"
      group: hermes-private
`
	sc := parseYAML(t, body)

	anon := reachableIndexes(sc.Servers, sc, "")
	if len(anon) != 1 || anon[0] != "public" {
		t.Errorf("anonymous indexes = %v, want [public]", anon)
	}

	keyed := reachableIndexes(sc.Servers, sc, "key")
	if len(keyed) != 2 || keyed[0] != "private" || keyed[1] != "public" {
		t.Errorf("keyed indexes = %v, want [private public]", keyed)
	}

	// A key for another group reaches only the public indexes.
	other := parseYAML(t, `servers:
  /mcp: public
  /mcp/private: private
auth:
  public: [/mcp]
  groups:
    hr:
      allow: [/mcp]
  keys:
    - hash: "`+hashKey("hr-key")+`"
      group: hr
`)
	got := reachableIndexes(other.Servers, other, "hr-key")
	if len(got) != 1 || got[0] != "public" {
		t.Errorf("limited key indexes = %v, want [public]", got)
	}
}

func TestCatchAllMountSearchesGrantsOnly(t *testing.T) {
	body := `servers:
  /mcp: all
  /mcp/private: private
auth:
  public: [/mcp]
  groups:
    holder:
      allow: [/mcp, /mcp/private]
  keys:
    - hash: "` + hashKey("holder-key") + `"
      group: holder
`
	sc := parseYAML(t, body)

	if got := indexesForMount(sc.Servers, sc, "/mcp", ""); len(got) != 0 {
		t.Errorf("anonymous catchall = %v, want nothing (no public index mount)", got)
	}
	if got := indexesForMount(sc.Servers, sc, "/mcp", "holder-key"); len(got) != 1 || got[0] != "private" {
		t.Errorf("keyed catchall = %v, want [private]", got)
	}
	if got := indexesForMount(sc.Servers, sc, "/mcp/private", "holder-key"); len(got) != 1 || got[0] != "private" {
		t.Errorf("specific mount = %v, want [private]", got)
	}
}

func TestCatchAllMountStatusCodes(t *testing.T) {
	body := `servers:
  /mcp: all
  /mcp/private: private
auth:
  public: [/mcp]
  groups:
    holder:
      allow: [/mcp, /mcp/private]
  keys:
    - hash: "` + hashKey("live") + `"
      group: holder
    - hash: "` + hashKey("stale") + `"
      group: holder
      expires: 2020-01-01
`
	sc := parseYAML(t, body)
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := authMiddleware(staticAuthState(sc), ok)

	call := func(path, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if rec := call("/mcp", ""); rec.Code != http.StatusForbidden {
		t.Errorf("anonymous catchall with no public index = %d, want 403", rec.Code)
	}
	if rec := call("/mcp/private", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous private mount = %d, want 401", rec.Code)
	}
	rec := call("/mcp/private", "stale")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expired key = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(got, "expired") {
		t.Errorf("expired key challenge = %q, want it to say the key expired", got)
	}
	if rec := call("/mcp", "live"); rec.Code != http.StatusOK {
		t.Errorf("keyed catchall = %d, want 200", rec.Code)
	}
	if rec := call("/mcp/private", "live"); rec.Code != http.StatusOK {
		t.Errorf("keyed specific mount = %d, want 200", rec.Code)
	}
}

func TestReloadKeepsStartupMounts(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("QMD_CONFIG_DIR", dir)
	p := filepath.Join(dir, "CONFIG.yml")
	startup := `servers:
  /mcp: all
  /mcp/hr: hr
  /mcp/pub: pub
auth:
  public: [/mcp/hr, /mcp/pub]
`
	if err := os.WriteFile(p, []byte(startup), 0o600); err != nil {
		t.Fatal(err)
	}
	sc, err := loadServerConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	h := authMiddleware(newAuthState(p, sc), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = indexesFromRequest(r)
	}))
	serve := func(path string) []string {
		got = nil
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, path, nil))
		return got
	}

	if indexes := serve("/mcp/hr"); len(indexes) != 1 || indexes[0] != "hr" {
		t.Fatalf("startup mount searched %v, want [hr]", indexes)
	}

	// Drop the mount from the file. The router still serves the path until a
	// restart, so it must keep hr rather than fall back to the default index.
	edited := `servers:
  /mcp: all
  /mcp/pub: pub
auth:
  public: [/mcp/hr, /mcp/pub]
`
	if err := os.WriteFile(p, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	if indexes := serve("/mcp/hr"); len(indexes) != 1 || indexes[0] != "hr" {
		t.Fatalf("after removing the mount, /mcp/hr searched %v; want the startup index [hr]", indexes)
	}
}

func TestVSearchRefusesStaleModel(t *testing.T) {
	fakeEmbedBackend(t)
	dir := t.TempDir()
	t.Setenv("QMD_CONFIG_DIR", dir)
	db := filepath.Join(dir, "x.sqlite")
	yml := "db_path: " + db + "\nembed_model: new-model\ncollections: {}\n"
	if err := os.WriteFile(filepath.Join(dir, "x.yml"), []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureEmbeddingBlobTable(); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertEmbedding("h", 0, 0, []float32{1, 0}, "old-model", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	sources, err := openSources([]string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	defer closeSources(sources)

	_, err = runVectors(sources, "query", 5, 0)
	if err == nil {
		t.Fatal("a stale model produced results instead of an error")
	}
	var mm *modelMismatch
	if !errors.As(err, &mm) {
		t.Fatalf("error is not a model mismatch: %v", err)
	}
	for _, want := range []string{"old-model", "new-model", "--force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestFindDocSkipsIndexesWithoutTheFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("QMD_CONFIG_DIR", dir)
	makeIndex := func(name, collection, docPath string) {
		db := filepath.Join(dir, name+".sqlite")
		yml := "db_path: " + db + "\ncollections:\n  " + collection + ":\n    path: /tmp\n    pattern: \"*.md\"\n"
		if err := os.WriteFile(filepath.Join(dir, name+".yml"), []byte(yml), 0o600); err != nil {
			t.Fatal(err)
		}
		st, err := store.NewStore(db)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		if docPath == "" {
			return
		}
		now := time.Now()
		if err := st.InsertContent(store.HashContent("body"), "body", now); err != nil {
			t.Fatal(err)
		}
		if err := st.InsertDocument(collection, docPath, "title", store.HashContent("body"), now, now); err != nil {
			t.Fatal(err)
		}
	}
	makeIndex("alpha", "alpha", "") // no documents at all
	makeIndex("beta", "beta", "report.md")

	sources, err := openSources([]string{"alpha", "beta"})
	if err != nil {
		t.Fatal(err)
	}
	defer closeSources(sources)

	src, collection, path, ok := findDoc(sources, "qmd://beta/report.md")
	if !ok || src.Name != "beta" || collection != "beta" || path != "report.md" {
		t.Fatalf("findDoc = %q %q %q %v, want beta/report.md in beta", src.Name, collection, path, ok)
	}
	if _, _, _, ok := findDoc(sources, "qmd://beta/missing.md"); ok {
		t.Error("a missing file resolved to an index that does not hold it")
	}
}

func TestHealthzIsExactPathOnly(t *testing.T) {
	sc := parseYAML(t, `servers:
  /: all
auth:
  public: [/]
`)
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := authMiddleware(staticAuthState(sc), ok)

	for path, want := range map[string]int{
		"/healthz":   http.StatusOK,
		"/healthz/":  http.StatusNotFound,
		"/healthz/x": http.StatusNotFound,
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("%s = %d, want %d", path, rec.Code, want)
		}
	}
}
