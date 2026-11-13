package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

var CurrentIndexName = "index"

// ServerConfigName is the file in the config dir holding mounts and auth. It is
// not an index config, so index discovery skips it. The uppercase name is what
// puts it above the indexes in a listing: `C` is below every lowercase letter
// both in byte order and at the primary collation level, while `_` is ignored
// by collation and lands after the letters.
const ServerConfigName = "CONFIG.yml"

// legacyServerConfigName is the name this file had before it was CONFIG.yml. A
// copy left behind would be walked as an index named "server", so discovery
// recognises its content and asks for the rename.
const legacyServerConfigName = "server.yml"

func GetConfigDir() (string, error) {
	if dir := os.Getenv("QMD_CONFIG_DIR"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "qmd"), nil
}

func GetConfigFilePath() (string, error) {
	dir, err := GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, fmt.Sprintf("%s.yml", CurrentIndexName)), nil
}

// IndexNames lists the indexes defined in the config dir: one <name>.yml per
// index, skipping the server config, dotfiles and directories. Reindexing with
// no explicit index walks this list.
func IndexNames() ([]string, error) {
	dir, err := GetConfigDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	reserved := strings.TrimSuffix(ServerConfigName, ".yml")
	legacy := strings.TrimSuffix(legacyServerConfigName, ".yml")
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".yml") {
			continue
		}
		name = strings.TrimSuffix(name, ".yml")
		if name == reserved {
			continue
		}
		if name == legacy && holdsServerConfig(filepath.Join(dir, e.Name())) {
			return nil, fmt.Errorf("%s holds mounts and keys under the old name; rename it to %s",
				filepath.Join(dir, legacyServerConfigName), filepath.Join(dir, ServerConfigName))
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// holdsServerConfig reports whether a file is a server config rather than an
// index config. Presence of the `servers` key decides, so an index legitimately
// named "server" (a `collections` key) is still an index.
func holdsServerConfig(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var top map[string]any
	if err := yaml.Unmarshal(data, &top); err != nil {
		return false
	}
	_, ok := top["servers"]
	return ok
}

func EnsureConfigDir() error {
	dir, err := GetConfigDir()
	if err != nil {
		return err
	}
	return os.MkdirAll(dir, 0755)
}

// IsReservedIndexName reports whether name is the server config file, or the
// pre-rename name of it. Neither can be an index: reading one as an index would
// create an empty database behind mounts and keys.
func IsReservedIndexName(name string) bool {
	name = strings.TrimSuffix(name, ".yml")
	return name == strings.TrimSuffix(ServerConfigName, ".yml") || name == strings.TrimSuffix(legacyServerConfigName, ".yml")
}

func LoadConfig() (*Config, error) {
	if IsReservedIndexName(CurrentIndexName) {
		return nil, fmt.Errorf("%s is the server config, not an index; choose --index <name>", CurrentIndexName+".yml")
	}
	path, err := GetConfigFilePath()
	if err != nil {
		return nil, err
	}
	return loadConfigFile(path)
}

// LoadIndexConfig loads the config a read needs. A named index that has no
// <name>.yml is an error, not an empty index: answering "no collections" from a
// name nobody created hides the typo, and invites `collection add`, which would
// create that name. Writers use LoadConfig and may create the file.
//
// A config directory with no indexes at all still returns the empty config, so
// the first run can bootstrap.
func LoadIndexConfig(indexName string) (*Config, error) {
	if indexName == "" {
		indexName = "index"
	}
	if IsReservedIndexName(indexName) {
		return nil, fmt.Errorf("%s.yml is the server config, not an index; choose --index <name>", indexName)
	}
	dir, err := GetConfigDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, indexName+".yml")
	if _, err := os.Stat(path); err != nil {
		names, err := IndexNames()
		if err != nil {
			return nil, err
		}
		if len(names) == 0 {
			return &Config{Collections: make(map[string]Collection)}, nil
		}
		return nil, fmt.Errorf("no %s.yml in %s; choose --index %s",
			indexName, dir, strings.Join(names, " or --index "))
	}
	return loadConfigFile(path)
}

// HasIndexConfig reports whether <name>.yml exists in the config dir, without
// treating its absence as an error.
func HasIndexConfig(indexName string) bool {
	if indexName == "" {
		indexName = "index"
	}
	dir, err := GetConfigDir()
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(dir, indexName+".yml"))
	return err == nil
}

// LoadConfigFor loads the collection config for a named index without
// consulting or mutating the package-level CurrentIndexName. It is used by
// long-running processes (e.g. an MCP server exposing several indexes at once)
// where the global would be wrong for all but one index.
func LoadConfigFor(indexName string) (*Config, error) {
	if indexName == "" {
		indexName = "index"
	}
	if IsReservedIndexName(indexName) {
		return nil, fmt.Errorf("%s.yml is the server config, not an index; choose --index <name>", indexName)
	}
	dir, err := GetConfigDir()
	if err != nil {
		return nil, err
	}
	return loadConfigFile(filepath.Join(dir, indexName+".yml"))
}

func loadConfigFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &Config{Collections: make(map[string]Collection)}, nil
	}
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	if cfg.Collections == nil {
		cfg.Collections = make(map[string]Collection)
	}
	if cfg.DBPath != "" && !filepath.IsAbs(cfg.DBPath) {
		return nil, fmt.Errorf("%s: db_path %q is relative; use an absolute path so every user and service opens one database",
			path, cfg.DBPath)
	}
	return &cfg, nil
}

// DBPathFor resolves the SQLite file for a named index. Precedence:
//  1. db_path in that index's config file  (explicit, per index — CLI and server agree)
//  2. QMD_DB_DIR env                       (shared directory for services/tests)
//  3. INDEX_PATH env                       (legacy single-index override)
//  4. $XDG_CACHE_HOME/qmd/<index>.sqlite
func DBPathFor(indexName string) (string, error) {
	if indexName == "" {
		indexName = "index"
	}
	cfg, err := LoadConfigFor(indexName)
	if err != nil {
		return "", err
	}
	if cfg.DBPath != "" {
		return cfg.DBPath, nil
	}
	if dir := os.Getenv("QMD_DB_DIR"); dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return "", err
		}
		return filepath.Join(dir, indexName+".sqlite"), nil
	}
	if path := os.Getenv("INDEX_PATH"); path != "" {
		return path, nil
	}

	cacheDir := os.Getenv("XDG_CACHE_HOME")
	if cacheDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		cacheDir = filepath.Join(home, ".cache")
	}
	qmdCacheDir := filepath.Join(cacheDir, "qmd")
	if err := os.MkdirAll(qmdCacheDir, 0755); err != nil {
		return "", err
	}
	return filepath.Join(qmdCacheDir, indexName+".sqlite"), nil
}

func SaveConfig(cfg *Config) error {
	if err := EnsureConfigDir(); err != nil {
		return err
	}
	path, err := GetConfigFilePath()
	if err != nil {
		return err
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0644)
}

// SetGlobalContext sets or clears the global context (applies to all collections).
func SetGlobalContext(cfg *Config, text string) {
	cfg.GlobalContext = text
}

// AddContext adds or updates context for a path prefix in a collection.
// pathPrefix can be "" or "/" for collection root. Returns false if collection does not exist.
func AddContext(cfg *Config, collectionName, pathPrefix, text string) bool {
	col, ok := cfg.Collections[collectionName]
	if !ok {
		return false
	}
	if col.Context == nil {
		col.Context = make(map[string]string)
	}
	if pathPrefix == "" {
		pathPrefix = "/"
	}
	col.Context[pathPrefix] = text
	cfg.Collections[collectionName] = col
	return true
}

// RemoveContext removes context for a path prefix. Returns false if not found.
func RemoveContext(cfg *Config, collectionName, pathPrefix string) bool {
	col, ok := cfg.Collections[collectionName]
	if !ok || col.Context == nil {
		return false
	}
	if pathPrefix == "" {
		pathPrefix = "/"
	}
	if _, ok := col.Context[pathPrefix]; !ok {
		return false
	}
	delete(col.Context, pathPrefix)
	if len(col.Context) == 0 {
		col.Context = nil
	}
	cfg.Collections[collectionName] = col
	return true
}

// ContextEntry is a single context listing entry.
type ContextEntry struct {
	Collection string
	Path       string
	Context    string
}

// ListAllContexts returns all configured contexts (global first, then per collection).
func ListAllContexts(cfg *Config) []ContextEntry {
	var out []ContextEntry
	if cfg.GlobalContext != "" {
		out = append(out, ContextEntry{Collection: "*", Path: "/", Context: cfg.GlobalContext})
	}
	for name, col := range cfg.Collections {
		if col.Context == nil {
			continue
		}
		for path, ctx := range col.Context {
			p := path
			if p == "" {
				p = "/"
			}
			out = append(out, ContextEntry{Collection: name, Path: p, Context: ctx})
		}
	}
	return out
}

// FindContextForPath returns the most specific context for a collection and file path.
func FindContextForPath(cfg *Config, collectionName, filePath string) string {
	if filePath != "" && filePath[0] != '/' {
		filePath = "/" + filePath
	}
	bestLen := 0
	var bestCtx string
	col, ok := cfg.Collections[collectionName]
	if ok && col.Context != nil {
		for prefix, ctx := range col.Context {
			p := prefix
			if p == "" {
				p = "/"
			}
			matches := p == "/" || (len(filePath) >= len(p) && filePath[:len(p)] == p && (len(filePath) == len(p) || filePath[len(p)] == '/'))
			if matches && len(p) > bestLen {
				bestLen = len(p)
				bestCtx = ctx
			}
		}
		if bestCtx != "" {
			return bestCtx
		}
	}
	return cfg.GlobalContext
}
