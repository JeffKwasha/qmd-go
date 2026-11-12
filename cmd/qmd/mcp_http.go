package main

// Streamable-HTTP transport for the qmd MCP server.
//
// One process serves several named indexes, each mounted at its own URL path,
// and gates access with bearer keys grouped in the server config
// (~/.config/qmd/server.yml):
//
//	servers:
//	  /mcp: index             # conventional bare endpoint (alias of shared)
//	  /mcp/personal: personal
//	auth:
//	  public: [/mcp]          # no key required
//	  groups:
//	    personal:
//	      allow: ["/mcp/personal", "/mcp"]
//	      keys:  ["<token>"]              # plaintext, legacy
//	      key_hashes:
//	        - hash: sha256:<hex>
//	          label: laptop
//
// A request without a key reaches only public paths. A key reaches the paths
// its group allows; a key listed in several groups is allowed if any of them
// does. Unknown paths are 404; denied requests are 401.
//
// The config is re-read when its content changes, so keys and public paths
// take effect without a restart. The set of mounts is fixed at startup.
//
// Index SQLite files are resolved with config.DBPathFor, so the server and the
// CLI always agree on where a named index lives (db_path in the index config,
// then QMD_DB_DIR, INDEX_PATH, XDG cache dir).

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ba0f3/qmd-go/internal/config"
	"github.com/ba0f3/qmd-go/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"
)

// keyHash is one hashed key. In YAML it may be a bare string or a mapping with
// a label, so older and hand-written configs both parse.
type keyHash struct {
	Hash  string `yaml:"hash"`
	Label string `yaml:"label"`
}

func (k *keyHash) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		k.Hash = node.Value
		return nil
	}
	type plain keyHash
	var p plain
	if err := node.Decode(&p); err != nil {
		return err
	}
	*k = keyHash(p)
	return nil
}

type authGroup struct {
	Keys      []string  `yaml:"keys"`
	KeyHashes []keyHash `yaml:"key_hashes"`
	Allow     []string  `yaml:"allow"`
}

// Has reports whether key belongs to this group, comparing hashes in constant
// time and accepting legacy plaintext entries.
func (g authGroup) Has(key string) bool {
	if key == "" {
		return false
	}
	keyDigest := hashKey(key)
	for _, k := range g.Keys {
		if k == "" {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(keyDigest), []byte(hashKey(k))) == 1 {
			return true
		}
	}
	for _, kh := range g.KeyHashes {
		if kh.Hash == "" {
			continue
		}
		presented := keyDigest
		if !strings.HasPrefix(kh.Hash, "sha256:") {
			presented = sha256Hex(key)
		}
		if subtle.ConstantTimeCompare([]byte(presented), []byte(kh.Hash)) == 1 {
			return true
		}
	}
	return false
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
}

// serverConfig is the HTTP-mode config: which indexes are exposed where, and
// who may reach them. Index content configs (index.yml, personal.yml) stay
// separate — they are read by the CLI too.
type serverConfig struct {
	Servers map[string]string `yaml:"servers"` // URL path -> named index ("" = default)
	Auth    authSection       `yaml:"auth"`
}

func normalizePath(p string) string {
	p = strings.TrimSpace(p)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if len(p) > 1 {
		p = strings.TrimSuffix(p, "/")
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
	normalized := make(map[string]string, len(sc.Servers))
	for p, idx := range sc.Servers {
		normalized[normalizePath(p)] = idx
	}
	sc.Servers = normalized
	for i, p := range sc.Auth.Public {
		sc.Auth.Public[i] = normalizePath(p)
	}
	return &sc, nil
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

// resolveServerConfigPath turns "" into the default location.
func resolveServerConfigPath(path string) (string, error) {
	if path != "" {
		return path, nil
	}
	dir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "server.yml"), nil
}

func isPublic(sc *serverConfig, path string) bool {
	for _, p := range sc.Auth.Public {
		if p == path {
			return true
		}
	}
	return false
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

func keyAllows(sc *serverConfig, key, path string) bool {
	if key == "" {
		return false
	}
	for name, g := range sc.Auth.Groups {
		if !g.Has(key) {
			continue
		}
		if g.allowsPath(path) {
			return true
		}
		log.Printf("qmd mcp auth: key group %q may not use %s", name, path)
	}
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
	path    string
	mounted map[string]string

	mu   sync.Mutex
	hash string
	sc   *serverConfig
}

func newAuthState(path string, sc *serverConfig) *authState {
	data, err := os.ReadFile(path)
	sum := ""
	if err == nil {
		sum = sha256Hex(string(data))
	}
	return &authState{path: path, mounted: sc.Servers, hash: sum, sc: sc}
}

func staticAuthState(sc *serverConfig) *authState {
	return &authState{mounted: sc.Servers, sc: sc}
}

func (a *authState) current() *serverConfig {
	if a.path == "" {
		return a.sc
	}
	data, err := os.ReadFile(a.path)
	if err != nil {
		return a.sc
	}
	sum := sha256Hex(string(data))

	a.mu.Lock()
	defer a.mu.Unlock()
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
	log.Printf("qmd mcp auth: reloaded %s (public: %v, %d groups)", a.path, sc.Auth.Public, len(sc.Auth.Groups))
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
		if path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		if _, ok := sc.Servers[path]; !ok {
			http.NotFound(w, r)
			return
		}
		if isPublic(sc, path) {
			next.ServeHTTP(w, r)
			return
		}
		key := bearerToken(r)
		if keyAllows(sc, key, path) {
			log.Printf("qmd mcp auth: allow %s key=%s %s", r.Method, keyFingerprint(key), path)
			next.ServeHTTP(w, r)
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

	paths := make([]string, 0, len(sc.Servers))
	for p := range sc.Servers {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	mux := http.NewServeMux()
	// One MCP server (and one store) per named index, shared by every path
	// that mounts it — an alias like /mcp must not open a second DB handle.
	byIndex := make(map[string]*mcp.Server)
	var stores []*store.Store
	defer func() {
		for _, s := range stores {
			_ = s.Close()
		}
	}()

	for _, p := range paths {
		idx := sc.Servers[p]
		if idx == "" {
			idx = "index"
		}
		srv, ok := byIndex[idx]
		if !ok {
			dbPath, err := config.DBPathFor(idx)
			if err != nil {
				return err
			}
			s, err := store.NewStore(dbPath)
			if err != nil {
				return fmt.Errorf("index %q (%s): %w", idx, dbPath, err)
			}
			stores = append(stores, s)
			cfg, _ := config.LoadConfigFor(idx)
			srv = newServer(s, cfg)
			byIndex[idx] = srv
			log.Printf("qmd mcp: index %q -> %s", idx, dbPath)
		}
		h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{Stateless: true})
		mux.Handle(p, h)
		mux.Handle(p+"/", h)
		log.Printf("qmd mcp: mount %s -> index %q", p, idx)
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
