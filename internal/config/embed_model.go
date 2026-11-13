package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ba0f3/qmd-go/internal/llm"
	"gopkg.in/yaml.v3"
)

// EmbedModelDefault is the model a new index starts from: $QMD_EMBED_MODEL,
// then the build's own default.
func EmbedModelDefault() string {
	if m := strings.TrimSpace(os.Getenv("QMD_EMBED_MODEL")); m != "" {
		return m
	}
	return llm.DefaultEmbedModel()
}

// EmbedModel is the model this index's vectors must come from. The index file
// wins because the vectors belong to that file: a database moved to another
// host, or read after the unit's environment changed, still knows what made it.
// The environment only supplies the value an index starts from.
func EmbedModel(cfg *Config) string {
	if cfg != nil {
		if m := strings.TrimSpace(cfg.EmbedModel); m != "" {
			return m
		}
	}
	return EmbedModelDefault()
}

// EmbedModelSource names where EmbedModel's answer came from, so a refusal can
// say which setting to change.
func EmbedModelSource(cfg *Config) string {
	if cfg != nil && strings.TrimSpace(cfg.EmbedModel) != "" {
		return "embed_model in the index file"
	}
	if strings.TrimSpace(os.Getenv("QMD_EMBED_MODEL")) != "" {
		return "$QMD_EMBED_MODEL"
	}
	return "the build default"
}

// SetEmbedModel records model as embed_model in <name>.yml, leaving comments
// and key order alone: the field is edited on the YAML node tree, and inserted
// above collections when the file has no such key yet. It reports whether the
// file changed, so a caller can say so once.
func SetEmbedModel(indexName, model string) (bool, error) {
	dir, err := GetConfigDir()
	if err != nil {
		return false, err
	}
	path := filepath.Join(dir, indexName+".yml")
	data, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	if len(doc.Content) == 0 {
		return false, fmt.Errorf("%s: empty file", path)
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return false, fmt.Errorf("%s: top level is not a mapping", path)
	}

	key := func(i int) string { return root.Content[2*i].Value }
	value := func(i int) *yaml.Node { return root.Content[2*i+1] }
	pairs := len(root.Content) / 2

	insertAt := len(root.Content)
	for i := 0; i < pairs; i++ {
		switch key(i) {
		case "embed_model":
			if strings.TrimSpace(value(i).Value) == model {
				return false, nil
			}
			value(i).Kind = yaml.ScalarNode
			value(i).Tag = "!!str"
			value(i).Value = model
			return true, writeNode(path, &doc)
		case "collections":
			if insertAt == len(root.Content) {
				insertAt = 2 * i
			}
		}
	}

	pair := []*yaml.Node{
		{Kind: yaml.ScalarNode, Tag: "!!str", Value: "embed_model"},
		{Kind: yaml.ScalarNode, Tag: "!!str", Value: model},
	}
	root.Content = append(root.Content[:insertAt], append(pair, root.Content[insertAt:]...)...)
	return true, writeNode(path, &doc)
}

// WriteFileAtomic replaces a file through a temporary sibling: a reader (the
// running server re-reads configs) never sees a half-written file, and a failed
// write never truncates the old one. A symlinked config is written through the
// link, not replaced by a regular file.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	target := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		target = resolved
	} else if info, lerr := os.Lstat(path); lerr == nil && info.Mode()&os.ModeSymlink != 0 {
		// A dangling symlink is still a link: write at its target rather than
		// replacing it with a regular file.
		if link, lerr := os.Readlink(path); lerr == nil {
			if !filepath.IsAbs(link) {
				link = filepath.Join(filepath.Dir(path), link)
			}
			target = link
		}
	}
	if info, err := os.Stat(target); err == nil {
		mode = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, target)
}

// writeNode routes an index config rewrite through WriteFileAtomic.
func writeNode(path string, doc *yaml.Node) error {
	out, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, out, 0o644)
}
