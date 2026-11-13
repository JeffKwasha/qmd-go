package main

// Streamable-HTTP transport for the qmd MCP server.
//
// One process serves several named indexes, each mounted at its own URL path,
// and gates access with bearer keys described in the server config
// (~/.config/qmd/0_qmd_server.yml):
//
//	servers:
//	  /mcp: index             # conventional bare endpoint (alias of shared)
//	  /mcp/personal: personal
//	auth:
//	  public: [/mcp]          # no key required
//	  groups:                 # mounted paths, no keys
//	    personal:
//	      allow: ["/mcp/personal", "/mcp"]
//	  keys:                   # one record per key, exactly one group
//	    - hash: "sha256:<hex>"
//	      group: personal
//	      label: laptop
//	      expires: 2027-02-10T00:00:00Z
//
// A request without a key reaches only public paths. A key reaches the paths of
// its own group, and only while it is live. Unknown paths are 404; denied
// requests are 401.
//
// The file is the single record of which keys exist. The loader refuses a hash
// recorded twice, and refuses a key that names no group, so no request can
// depend on which of several records is consulted first.
//
// The config is re-read when its content changes, so keys and public paths
// take effect without a restart. The set of mounts is fixed at startup.
//
// Index SQLite files are resolved with config.DBPathFor, so the server and the
// CLI always agree on where a named index lives (db_path in the index config,
// then QMD_DB_DIR, INDEX_PATH, XDG cache dir).

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	urlpath "path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/ba0f3/qmd-go/internal/config"
	"github.com/ba0f3/qmd-go/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"
)

// keyRecord is one entry under auth.keys, as written in YAML.
type keyRecord struct {
	Hash    string `yaml:"hash"`
	Group   string `yaml:"group"`
	Label   string `yaml:"label"`
	Issued  string `yaml:"issued"`
	Expires string `yaml:"expires"`
}

// keyEntry is a validated key: one hash, one group, optional lifetime.
type keyEntry struct {
	Hash    string
	Group   string
	Label   string
	Issued  time.Time
	Expires time.Time
}

// expired reports whether the key has reached its expiry. A zero Expires never
// lapses.
func (e keyEntry) expired(now time.Time) bool {
	return !e.Expires.IsZero() && !now.Before(e.Expires)
}

// authGroup holds the mounts a group may use. Keys live under auth.keys.
type authGroup struct {
	Allow []string `yaml:"allow"`
}

func (g authGroup) allowsPath(path string) bool {
	for _, allowed := range g.Allow {
		if normalizePath(allowed) == path {
			return true
		}
	}
	return false
}

type authSection struct {
	Public []string             `yaml:"public"`
	Groups map[string]authGroup `yaml:"groups"`
	Keys   []keyRecord          `yaml:"keys"`
}

// serverConfig is the HTTP-mode config: which indexes are exposed where, and
// who may reach them. Index content configs (index.yml, personal.yml) stay
// separate — they are read by the CLI too.
type serverConfig struct {
	Servers map[string]string `yaml:"servers"` // URL path -> named index ("" = default)
	Auth    authSection       `yaml:"auth"`

	byHash map[string]*keyEntry
}

// normalizePath is idempotent: one path has one stored spelling, so two mounts
// that differ only in trailing slashes collide instead of registering the same
// http.ServeMux pattern twice.
func normalizePath(p string) string {
	p = strings.TrimSpace(p)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if p = strings.TrimRight(p, "/"); p == "" {
		p = "/"
	}
	return p
}

func parseServerConfig(data []byte, path string) (*serverConfig, error) {
	var sc serverConfig
	if err := yaml.Unmarshal(data, &sc); err != nil {
		return nil, fmt.Errorf("server config %s: %w", path, err)
	}
	if len(sc.Servers) == 0 {
		return nil, fmt.Errorf("server config %s: no servers defined", path)
	}
	if err := rejectLegacyKeys(data, path); err != nil {
		return nil, err
	}
	if err := validateGroups(sc.Auth.Groups, path); err != nil {
		return nil, err
	}
	raw := make([]string, 0, len(sc.Servers))
	for p := range sc.Servers {
		raw = append(raw, p)
	}
	sort.Strings(raw)
	normalized := make(map[string]string, len(sc.Servers))
	seen := make(map[string]string, len(sc.Servers))
	for _, p := range raw {
		if strings.TrimSpace(p) == "" {
			return nil, fmt.Errorf("server config %s: servers has an empty mount path; use \"/\" for the root", path)
		}
		n := normalizePath(p)
		if err := validateMountPath(n, path); err != nil {
			return nil, err
		}
		if other, ok := seen[n]; ok {
			return nil, fmt.Errorf("server config %s: mounts %q and %q are both %s; a path is mounted once",
				path, other, p, n)
		}
		seen[n] = p
		normalized[n] = sc.Servers[p]
	}
	sc.Servers = normalized
	for i, p := range sc.Auth.Public {
		if strings.TrimSpace(p) == "" {
			return nil, fmt.Errorf("server config %s: auth.public has an empty path", path)
		}
		sc.Auth.Public[i] = normalizePath(p)
	}
	if len(sc.Auth.Keys) > 0 {
		sc.byHash = make(map[string]*keyEntry, len(sc.Auth.Keys))
	}
	for _, rec := range sc.Auth.Keys {
		e, err := rec.entry(&sc, path)
		if err != nil {
			return nil, err
		}
		if prev, ok := sc.byHash[e.Hash]; ok {
			if prev.Group == e.Group {
				return nil, fmt.Errorf("server config %s: key %s is recorded twice in group %q; a key has exactly one record",
					path, shortHash(e.Hash), e.Group)
			}
			return nil, fmt.Errorf("server config %s: key %s is recorded twice, in groups %q and %q; a key has exactly one group",
				path, shortHash(e.Hash), prev.Group, e.Group)
		}
		sc.byHash[e.Hash] = e
	}
	return &sc, nil
}

// validateMountPath rejects a mount http.ServeMux cannot register, and the
// liveness path, which the server registers itself. Either would panic the
// process at startup instead of failing the load. It also rejects a path the
// mux would rewrite before routing: those mounts answer a redirect, never the
// index, and register a pattern that can collide with another mount's. Finally
// it requires the mount to be its own URL-escaped form, because the mux matches
// the escaped request path while the key check matches the decoded one: a
// mount like /a%2Fb would be authorized as /a/b and routed as /a%2Fb.
func validateMountPath(n, path string) error {
	bad := func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }
	if strings.ContainsFunc(n, bad) || strings.ContainsAny(n, "{}") {
		return fmt.Errorf("server config %s: mount %q contains a character an HTTP route cannot use", path, n)
	}
	if clean := urlpath.Clean(n); clean != n {
		return fmt.Errorf("server config %s: mount %q is not a clean path; mount it at %q", path, n, clean)
	}
	if escaped := (&url.URL{Path: n}).EscapedPath(); escaped != n {
		return fmt.Errorf("server config %s: mount %q is not its own URL-escaped form %q; the router matches the escaped path while the key check matches the decoded path, so mount a path with no percent-encoding", path, n, escaped)
	}
	if n == "/healthz" {
		return fmt.Errorf("server config %s: mount %q is reserved for liveness; mount the index at another path", path, n)
	}
	return nil
}

func validateGroups(groups map[string]authGroup, path string) error {
	for name, g := range groups {
		if name == "" {
			return fmt.Errorf("server config %s: auth.groups has an empty group name", path)
		}
		if strings.TrimSpace(name) != name {
			return fmt.Errorf("server config %s: group %q has leading or trailing whitespace", path, name)
		}
		for _, a := range g.Allow {
			if strings.TrimSpace(a) == "" {
				return fmt.Errorf("server config %s: group %q has an empty allow path", path, name)
			}
		}
	}
	return nil
}

func (r keyRecord) entry(sc *serverConfig, path string) (*keyEntry, error) {
	hash, err := canonicalKeyHash(r.Hash)
	if err != nil {
		return nil, fmt.Errorf("server config %s: %w", path, err)
	}
	group := r.Group
	if group == "" {
		return nil, fmt.Errorf("server config %s: key %s has no group", path, shortHash(hash))
	}
	if strings.TrimSpace(group) != group {
		return nil, fmt.Errorf("server config %s: key %s names group %q, which has leading or trailing whitespace",
			path, shortHash(hash), group)
	}
	if _, ok := sc.Auth.Groups[group]; !ok {
		return nil, fmt.Errorf("server config %s: key %s names group %q, which auth.groups does not define",
			path, shortHash(hash), group)
	}
	issued, err := parseKeyTime(r.Issued)
	if err != nil {
		return nil, fmt.Errorf("server config %s: key %s issued: %w", path, shortHash(hash), err)
	}
	expires, err := parseKeyTime(r.Expires)
	if err != nil {
		return nil, fmt.Errorf("server config %s: key %s expires: %w", path, shortHash(hash), err)
	}
	return &keyEntry{Hash: hash, Group: group, Label: r.Label, Issued: issued, Expires: expires}, nil
}

// legacyProbe reads only the fields the old layout used, so the loader can name
// the group that still needs converting. The fields are any so a scalar, list or
// mapping all reach the check, and yaml resolves aliases and merge keys for us.
type legacyProbe struct {
	Auth struct {
		Groups map[string]struct {
			Keys      any `yaml:"keys"`
			KeyHashes any `yaml:"key_hashes"`
		} `yaml:"groups"`
	} `yaml:"auth"`
}

func rejectLegacyKeys(data []byte, path string) error {
	var probe legacyProbe
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return fmt.Errorf("server config %s: %w", path, err)
	}
	names := make([]string, 0, len(probe.Auth.Groups))
	for name, g := range probe.Auth.Groups {
		if legacyValueHasContent(g.Keys) || legacyValueHasContent(g.KeyHashes) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	return fmt.Errorf("server config %s: group %q stores keys under groups.%s, which this version does not read; keys belong in auth.keys",
		path, names[0], names[0])
}

func legacyValueHasContent(v any) bool {
	switch v := v.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(v) != ""
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	case map[any]any:
		return len(v) > 0
	default:
		return true
	}
}

// canonicalKeyHash accepts sha256:<hex> or a bare sha256 digest and returns the
// single stored form.
func canonicalKeyHash(s string) (string, error) {
	s = strings.TrimSpace(s)
	hexPart := s
	if trimmed, ok := strings.CutPrefix(s, "sha256:"); ok {
		hexPart = trimmed
	}
	if len(hexPart) != 64 || !isHexDigest(hexPart) {
		return "", fmt.Errorf("hash %q is not sha256:<64 hex>", s)
	}
	return "sha256:" + strings.ToLower(hexPart), nil
}

func isHexDigest(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}

// parseKeyTime accepts RFC3339 and YYYY-MM-DD, which is read as midnight UTC.
// An empty string means the timestamp is unset. The zero time is refused: it is
// what "unset" holds, so a key written with 0001-01-01 would read as never.
func parseKeyTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return usableTime(t, s)
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return usableTime(t, s)
	}
	return time.Time{}, fmt.Errorf("%q is not RFC3339 or YYYY-MM-DD", s)
}

func usableTime(t time.Time, s string) (time.Time, error) {
	if t.IsZero() {
		return time.Time{}, fmt.Errorf("%q is the zero time, which reads as unset", s)
	}
	return t.UTC(), nil
}

// parseExpires turns a CLI expiry into an absolute time. Empty and "never" mean
// no expiry; a bare number with a d suffix counts calendar days from now.
func parseExpires(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "", strings.EqualFold(s, "never"):
		return time.Time{}, nil
	case timeHasDayUnit(s):
		days, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil || days <= 0 {
			return time.Time{}, fmt.Errorf("expiry %q is not a positive number of days", s)
		}
		return now.Add(time.Duration(days) * 24 * time.Hour).UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return usableTime(t, s)
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return usableTime(t, s)
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return now.Add(d).UTC(), nil
	}
	return time.Time{}, fmt.Errorf("expiry %q is not a duration (30d, 720h) or a date (2027-03-01)", s)
}

func timeHasDayUnit(s string) bool {
	return len(s) > 1 && strings.HasSuffix(s, "d") && s[:len(s)-1] != ""
}

func loadServerConfig(path string) (*serverConfig, error) {
	path, err := resolveServerConfigPath(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("server config %s: %w", path, err)
	}
	return parseServerConfig(data, path)
}

// validateMountIndexes holds the config against the filesystem: every mount
// must name an index whose <name>.yml exists. It runs where the mounts decide
// something — server startup and `server check` — and not in the loader, so the
// CLI can still edit a config it would refuse to serve.
//
// A mount to a missing index is not an empty index. DBPathFor would invent a
// database path and NewStore would create a fresh database there, so the mount
// would answer from an index that can never hold a document while the index it
// was meant to expose sits elsewhere. The server refuses to start instead.
func validateMountIndexes(sc *serverConfig, path string) error {
	dir, err := config.GetConfigDir()
	if err != nil {
		return err
	}
	mounts := make([]string, 0, len(sc.Servers))
	for p := range sc.Servers {
		mounts = append(mounts, p)
	}
	sort.Strings(mounts)
	for _, p := range mounts {
		name := sc.Servers[p]
		if name == "" {
			name = "index"
		}
		cfgPath := filepath.Join(dir, name+".yml")
		if _, err := os.Stat(cfgPath); err != nil {
			return fmt.Errorf("server config %s: mount %s points at index %q, which has no config at %s",
				path, p, name, cfgPath)
		}
	}
	return nil
}

// resolveServerConfigPath resolves the server config: the explicit flag, then
// $QMD_SERVER_CONFIG, then the config dir. The env var lets a service pin the
// file without repeating a flag in every command.
func resolveServerConfigPath(path string) (string, error) {
	if path != "" {
		return path, nil
	}
	if env := strings.TrimSpace(os.Getenv("QMD_SERVER_CONFIG")); env != "" {
		return env, nil
	}
	dir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, config.ServerConfigName), nil
}

func isPublic(sc *serverConfig, path string) bool {
	for _, p := range sc.Auth.Public {
		if p == path {
			return true
		}
	}
	return false
}

// reachableIndexes is the set of indexes one caller may search: the indexes of
// every public mount, plus the indexes of the mounts its key's group allows.
// A federated answer is built from this set, so access is decided per caller
// rather than per mount: an anonymous request to a mount that also names a
// private index still sees only the public indexes.
func reachableIndexes(sc *serverConfig, key string) []string {
	seen := make(map[string]bool)
	add := func(path string) {
		name, ok := sc.Servers[normalizePath(path)]
		if !ok {
			return
		}
		if name == "" {
			name = "index"
		}
		seen[name] = true
	}
	for _, p := range sc.Auth.Public {
		add(p)
	}
	if e, ok := sc.lookup(key); ok {
		if g, ok := sc.Auth.Groups[e.Group]; ok {
			for _, p := range g.Allow {
				add(p)
			}
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// indexSetKey carries the caller's index set from the middleware to the MCP
// handler, which is chosen per request from it.
type indexSetKey struct{}

func withIndexSet(r *http.Request, indexes []string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), indexSetKey{}, indexes))
}

func indexesFromRequest(r *http.Request) []string {
	indexes, _ := r.Context().Value(indexSetKey{}).([]string)
	return indexes
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// hashKey is the stored form of a key. Only this value touches the disk.
func hashKey(key string) string {
	return "sha256:" + sha256Hex(key)
}

func keyFingerprint(key string) string {
	if key == "" {
		return "none"
	}
	return hashKey(key)[:len("sha256:")+8]
}

// newKey returns 24 random bytes as hex, the same shape openssl rand -hex 24
// produces.
func newKey() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// lookup resolves a presented key to its single record. The hash is compared by
// map lookup, which is not a timing oracle for the key itself.
func (sc *serverConfig) lookup(key string) (*keyEntry, bool) {
	if key == "" || sc.byHash == nil {
		return nil, false
	}
	e, ok := sc.byHash[hashKey(key)]
	return e, ok
}

func keyAllows(sc *serverConfig, key, path string) bool {
	e, ok := sc.lookup(key)
	if !ok {
		return false
	}
	if e.expired(time.Now()) {
		log.Printf("qmd mcp auth: key %s expired %s", keyFingerprint(key), e.Expires.Format(time.RFC3339))
		return false
	}
	g, ok := sc.Auth.Groups[e.Group]
	if !ok {
		return false
	}
	if g.allowsPath(path) {
		return true
	}
	log.Printf("qmd mcp auth: key %s group %q may not use %s", keyFingerprint(key), e.Group, path)
	return false
}

func bearerToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		parts := strings.SplitN(h, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			return strings.TrimSpace(parts[1])
		}
	}
	if k := r.Header.Get("X-Api-Key"); k != "" {
		return strings.TrimSpace(k)
	}
	return ""
}

// authState holds the parsed server config and reloads it when the file
// content changes. Reads are cheap enough for the request rate a local tool
// sees, and content hashing avoids mtime granularity problems.
type authState struct {
	path string
	// mounted is the set of mounts the process started with, and is never
	// replaced: the router keeps those handlers for its whole life, so the
	// middleware must decide 404 from the same set the mux routes from. A
	// reloaded servers map is only compared against it to warn about the
	// restart a mount change needs.
	mounted map[string]string

	mu   sync.Mutex
	hash string
	sc   *serverConfig
}

func newAuthState(path string, sc *serverConfig) *authState {
	a := &authState{path: path, mounted: sc.Servers, sc: sc}
	data, err := os.ReadFile(path)
	if err != nil {
		return a
	}
	parsed, err := parseServerConfig(data, path)
	if err != nil {
		log.Printf("qmd mcp auth: reload failed, keeping the previous config: %v", err)
		return a
	}
	a.sc = parsed
	a.hash = sha256Hex(string(data))
	if !sameMounts(a.mounted, parsed.Servers) {
		log.Printf("qmd mcp auth: mounts changed in %s; restart to apply them", path)
	}
	return a
}

func staticAuthState(sc *serverConfig) *authState {
	return &authState{mounted: sc.Servers, sc: sc}
}

func (a *authState) current() *serverConfig {
	if a.path == "" {
		return a.sc
	}
	// The read is under the lock too. Reading first would let a goroutine that
	// saw older bytes parse them after a newer config was already published,
	// which would undo a revocation until the next request.
	a.mu.Lock()
	defer a.mu.Unlock()
	data, err := os.ReadFile(a.path)
	if err != nil {
		return a.sc
	}
	sum := sha256Hex(string(data))
	if sum == a.hash {
		return a.sc
	}
	sc, err := parseServerConfig(data, a.path)
	if err != nil {
		log.Printf("qmd mcp auth: reload failed, keeping the previous config: %v", err)
		return a.sc
	}
	if !sameMounts(a.mounted, sc.Servers) {
		log.Printf("qmd mcp auth: mounts changed in %s; restart to apply them", a.path)
	}
	a.sc = sc
	a.hash = sum
	log.Printf("qmd mcp auth: reloaded %s (public: %v, %d groups, %d keys)", a.path, sc.Auth.Public, len(sc.Auth.Groups), len(sc.byHash))
	return sc
}

func sameMounts(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

func authMiddleware(state *authState, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sc := state.current()
		path := normalizePath(r.URL.Path)
		// The router matches the escaped request path; the checks below use the
		// decoded one. Refusing requests where the two disagree keeps the index
		// that answers the one the key was checked for: /a/b%2Fc decodes to the
		// mount /a/b/c but routes to the subtree of /a, and /mcp%2Fpersonal/ to
		// the root mount.
		if r.URL.EscapedPath() != r.URL.Path {
			http.NotFound(w, r)
			return
		}
		if path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		if _, ok := state.mounted[path]; !ok {
			http.NotFound(w, r)
			return
		}
		if isPublic(sc, path) {
			// A public path needs no key, but a valid key still widens the
			// answer to the indexes it was granted: that is what "search every
			// database my key allows" means when the endpoint is also public.
			indexes := reachableIndexes(sc, "")
			if key := bearerToken(r); key != "" {
				if e, ok := sc.lookup(key); ok && !e.expired(time.Now()) {
					indexes = reachableIndexes(sc, key)
				}
			}
			next.ServeHTTP(w, withIndexSet(r, indexes))
			return
		}
		key := bearerToken(r)
		if keyAllows(sc, key, path) {
			log.Printf("qmd mcp auth: allow %s key=%s %s", r.Method, keyFingerprint(key), path)
			next.ServeHTTP(w, withIndexSet(r, reachableIndexes(sc, key)))
			return
		}
		log.Printf("qmd mcp auth: deny %s key=%s %s from %s", r.Method, keyFingerprint(key), path, r.RemoteAddr)
		w.Header().Set("WWW-Authenticate", `Bearer realm="qmd"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

// runMCPHTTP serves the MCP endpoint(s) over streamable HTTP.
func runMCPHTTP(addr, serverCfgPath string) error {
	resolved, err := resolveServerConfigPath(serverCfgPath)
	if err != nil {
		return err
	}
	sc, err := loadServerConfig(resolved)
	if err != nil {
		return err
	}
	if err := validateMountIndexes(sc, resolved); err != nil {
		return err
	}
	paths := make([]string, 0, len(sc.Servers))
	for p := range sc.Servers {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	mux := http.NewServeMux()
	// One store per index, shared by every mount that names it, and one MCP
	// server per index set. The set is the caller's, computed per request from
	// the mounts it may reach, so a single mount can answer from several indexes
	// for a key and from one for an anonymous client.
	stores := make(map[string]*store.Store)
	defer func() {
		for _, st := range stores {
			_ = st.Close()
		}
	}()
	openIndex := func(name string) error {
		if _, ok := stores[name]; ok {
			return nil
		}
		dbPath, err := config.DBPathFor(name)
		if err != nil {
			return err
		}
		st, err := store.NewStore(dbPath)
		if err != nil {
			return fmt.Errorf("index %q (%s): %w", name, dbPath, err)
		}
		stores[name] = st
		log.Printf("qmd mcp: index %q -> %s", name, dbPath)
		return nil
	}
	for _, name := range sc.Servers {
		if name == "" {
			name = "index"
		}
		if err := openIndex(name); err != nil {
			return err
		}
	}

	var mu sync.Mutex
	servers := make(map[string]*mcp.Server)
	serverFor := func(indexes []string) *mcp.Server {
		mu.Lock()
		defer mu.Unlock()
		key := strings.Join(indexes, "\x00")
		if srv, ok := servers[key]; ok {
			return srv
		}
		sources := make([]indexSource, 0, len(indexes))
		for _, name := range indexes {
			st, ok := stores[name]
			if !ok {
				continue
			}
			cfg, _ := config.LoadConfigFor(name)
			sources = append(sources, indexSource{Name: name, Store: st, Cfg: cfg})
		}
		srv := newServer(sources)
		servers[key] = srv
		return srv
	}

	for _, p := range paths {
		own := sc.Servers[p]
		if own == "" {
			own = "index"
		}
		h := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
			indexes := indexesFromRequest(r)
			if len(indexes) == 0 {
				indexes = []string{own}
			}
			return serverFor(indexes)
		}, &mcp.StreamableHTTPOptions{Stateless: true})
		mux.Handle(p, h)
		mux.Handle(p+"/", h)
		log.Printf("qmd mcp: mount %s -> index %q", p, own)
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	log.Printf("qmd mcp: listening on %s (public: %v)", addr, sc.Auth.Public)
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           authMiddleware(newAuthState(resolved, sc), mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return httpSrv.ListenAndServe()
}
