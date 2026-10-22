package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

var CurrentIndexName = "index"

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

func EnsureConfigDir() error {
	dir, err := GetConfigDir()
	if err != nil {
		return err
	}
	return os.MkdirAll(dir, 0755)
}

func LoadConfig() (*Config, error) {
	path, err := GetConfigFilePath()
	if err != nil {
		return nil, err
	}
	return loadConfigFile(path)
}

// LoadConfigFor loads the collection config for a named index without
// consulting or mutating the package-level CurrentIndexName. It is used by
// long-running processes (e.g. an MCP server exposing several indexes at once)
// where the global would be wrong for all but one index.
func LoadConfigFor(indexName string) (*Config, error) {
	dir, err := GetConfigDir()
	if err != nil {
		return nil, err
	}
	if indexName == "" {
		indexName = "index"
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
	if cfg, err := LoadConfigFor(indexName); err == nil && cfg.DBPath != "" {
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
