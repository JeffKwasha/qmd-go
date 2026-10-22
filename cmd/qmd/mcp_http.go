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
//	      keys:  ["<token>"]
//	      allow: ["/mcp/personal", "/mcp"]
//
// A request without a key reaches only public paths. A key reaches the paths
// its group allows. Unknown paths are 404; denied requests are 401.
//
// Index SQLite files are resolved with config.DBPathFor, so the server and the
// CLI always agree on where a named index lives (db_path in the index config,
// then QMD_DB_DIR, INDEX_PATH, XDG cache dir).

import (
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
	"time"

	"github.com/ba0f3/qmd-go/internal/config"
	"github.com/ba0f3/qmd-go/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"
)

type authGroup struct {
	Keys  []string `yaml:"keys"`
	Allow []string `yaml:"allow"`
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

func loadServerConfig(path string) (*serverConfig, error) {
	if path == "" {
		dir, err := config.GetConfigDir()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(dir, "server.yml")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("server config %s: %w", path, err)
	}
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

func keyFingerprint(key string) string {
	if key == "" {
		return "none"
	}
	return "sha256:" + sha256Hex(key)[:8]
}

func keyAllows(sc *serverConfig, key, path string) bool {
	if key == "" {
		return false
	}
	kh := sha256Hex(key)
	for name, g := range sc.Auth.Groups {
		for _, k := range g.Keys {
			if k == "" {
				continue
			}
			if subtle.ConstantTimeCompare([]byte(kh), []byte(sha256Hex(k))) != 1 {
				continue
			}
			for _, allowed := range g.Allow {
				if normalizePath(allowed) == path {
					return true
				}
			}
			log.Printf("qmd mcp auth: key group %q may not use %s", name, path)
			return false
		}
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

func authMiddleware(sc *serverConfig, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			log.Printf("qmd mcp auth: allow %s (public) %s", r.Method, path)
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
	sc, err := loadServerConfig(serverCfgPath)
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
		Handler:           authMiddleware(sc, mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return httpSrv.ListenAndServe()
}
