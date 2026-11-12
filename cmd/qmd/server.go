package main

// Management commands for the HTTP MCP server config. They edit server.yml in
// place, preserving comments and key order by operating on the YAML node tree
// rather than re-marshalling a struct.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ba0f3/qmd-go/internal/config"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

type keyEntry struct {
	Group string
	Key   string
	Label string
	Allow []string
}

func documentRoot(doc *yaml.Node) *yaml.Node {
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		return doc.Content[0]
	}
	return doc
}

func mappingChild(m *yaml.Node, key string) (*yaml.Node, bool) {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil, false
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1], true
		}
	}
	return nil, false
}

func ensureMapping(m *yaml.Node, key string) (*yaml.Node, error) {
	if n, ok := mappingChild(m, key); ok {
		if n.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("%s is not a mapping", key)
		}
		return n, nil
	}
	n := &yaml.Node{Kind: yaml.MappingNode}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, n)
	return n, nil
}

func ensureSequence(m *yaml.Node, key string) (*yaml.Node, error) {
	if n, ok := mappingChild(m, key); ok {
		if n.Kind != yaml.SequenceNode {
			return nil, fmt.Errorf("%s is not a list", key)
		}
		return n, nil
	}
	n := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, n)
	return n, nil
}

// editServerYAML rewrites path through fn, keeping the original file mode.
func editServerYAML(path string, fn func(root *yaml.Node) error) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if err := fn(documentRoot(&doc)); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), info.Mode().Perm())
}

func hashEntryValue(n *yaml.Node) (hash, label string) {
	if n.Kind == yaml.ScalarNode {
		return n.Value, ""
	}
	h, _ := mappingChild(n, "hash")
	l, _ := mappingChild(n, "label")
	if h != nil {
		hash = h.Value
	}
	if l != nil {
		label = l.Value
	}
	return hash, label
}

// addServerKey appends a freshly generated key to a group and returns the
// plaintext key exactly once. Only its hash is written.
func addServerKey(path, group, label string, allow []string) (string, string, error) {
	key, err := newKey()
	if err != nil {
		return "", "", err
	}
	h := hashKey(key)
	err = editServerYAML(path, func(root *yaml.Node) error {
		auth, err := ensureMapping(root, "auth")
		if err != nil {
			return err
		}
		groups, err := ensureMapping(auth, "groups")
		if err != nil {
			return err
		}
		g, err := ensureMapping(groups, group)
		if err != nil {
			return err
		}
		seq, err := ensureSequence(g, "key_hashes")
		if err != nil {
			return err
		}
		entry := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Value: "hash"},
			{Kind: yaml.ScalarNode, Value: h, Style: yaml.DoubleQuotedStyle},
		}}
		if label != "" {
			entry.Content = append(entry.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Value: "label"},
				&yaml.Node{Kind: yaml.ScalarNode, Value: label})
		}
		seq.Content = append(seq.Content, entry)
		if allow != nil {
			allowSeq, err := ensureSequence(g, "allow")
			if err != nil {
				return err
			}
			allowSeq.Content = nil
			for _, p := range allow {
				allowSeq.Content = append(allowSeq.Content, &yaml.Node{
					Kind: yaml.ScalarNode, Value: normalizePath(p), Style: yaml.DoubleQuotedStyle,
				})
			}
		}
		return nil
	})
	if err != nil {
		return "", "", err
	}
	return key, keyFingerprint(key), nil
}

// removeServerKey deletes every key in group whose stored hash starts with
// fingerprint. An empty group matches every group.
func removeServerKey(path, group, fingerprint string) (int, error) {
	if fingerprint == "" {
		return 0, fmt.Errorf("fingerprint required")
	}
	removed := 0
	err := editServerYAML(path, func(root *yaml.Node) error {
		auth, ok := mappingChild(root, "auth")
		if !ok {
			return fmt.Errorf("no auth section in %s", path)
		}
		groups, ok := mappingChild(auth, "groups")
		if !ok {
			return fmt.Errorf("no auth.groups in %s", path)
		}
		for i := 0; i+1 < len(groups.Content); i += 2 {
			name := groups.Content[i].Value
			if group != "" && name != group {
				continue
			}
			g := groups.Content[i+1]
			seq, ok := mappingChild(g, "key_hashes")
			if !ok || seq.Kind != yaml.SequenceNode {
				continue
			}
			kept := make([]*yaml.Node, 0, len(seq.Content))
			for _, entry := range seq.Content {
				h, _ := hashEntryValue(entry)
				if h != "" && strings.HasPrefix(h, fingerprint) {
					removed++
					continue
				}
				kept = append(kept, entry)
			}
			seq.Content = kept
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if removed == 0 {
		return 0, fmt.Errorf("no key matched %q", fingerprint)
	}
	return removed, nil
}

func listServerKeys(path string) ([]keyEntry, error) {
	sc, err := loadServerConfig(path)
	if err != nil {
		return nil, err
	}
	var out []keyEntry
	for name, g := range sc.Auth.Groups {
		for _, kh := range g.KeyHashes {
			out = append(out, keyEntry{Group: name, Key: kh.Hash, Label: kh.Label, Allow: g.Allow})
		}
		for _, k := range g.Keys {
			out = append(out, keyEntry{Group: name, Key: keyFingerprint(k), Label: "plaintext (rotate)", Allow: g.Allow})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Group != out[j].Group {
			return out[i].Group < out[j].Group
		}
		return out[i].Key < out[j].Key
	})
	return out, nil
}

// checkServerConfig reports configuration problems that do not stop the server
// from starting: dead mounts, allow paths that reach nothing, groups that can
// authenticate but not read anything, duplicate keys, and plaintext keys.
func checkServerConfig(path string) ([]string, error) {
	sc, err := loadServerConfig(path)
	if err != nil {
		return nil, err
	}
	dir, err := config.GetConfigDir()
	if err != nil {
		return nil, err
	}
	var findings []string
	for mount, idx := range sc.Servers {
		name := idx
		if name == "" {
			name = "index"
		}
		if _, err := os.Stat(filepath.Join(dir, name+".yml")); err != nil {
			findings = append(findings, fmt.Sprintf("mount %s points at index %q, which has no %s.yml", mount, name, name))
		}
	}
	owners := map[string][]string{}
	for name, g := range sc.Auth.Groups {
		if len(g.Keys) == 0 && len(g.KeyHashes) == 0 {
			findings = append(findings, fmt.Sprintf("group %q has no keys and cannot be used", name))
		}
		if len(g.Allow) == 0 {
			findings = append(findings, fmt.Sprintf("group %q allows no paths", name))
		}
		for _, a := range g.Allow {
			if _, ok := sc.Servers[normalizePath(a)]; !ok {
				findings = append(findings, fmt.Sprintf("group %q allows %s, which is not mounted", name, a))
			}
		}
		if len(g.Keys) > 0 {
			findings = append(findings, fmt.Sprintf("group %q has %d plaintext key(s); rotate with qmd server key add", name, len(g.Keys)))
		}
		for _, kh := range g.KeyHashes {
			if kh.Hash != "" {
				owners[kh.Hash] = append(owners[kh.Hash], name)
			}
		}
		for _, k := range g.Keys {
			if k != "" {
				owners[hashKey(k)] = append(owners[hashKey(k)], name)
			}
		}
	}
	for h, groups := range owners {
		if len(groups) > 1 {
			sort.Strings(groups)
			findings = append(findings, fmt.Sprintf("key %s appears in groups %s; any of them can widen its access",
				shortHash(h), strings.Join(groups, ", ")))
		}
	}
	sort.Strings(findings)
	return findings, nil
}

func shortHash(h string) string {
	if len(h) > len("sha256:")+8 {
		return h[:len("sha256:")+8]
	}
	return h
}

var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "Manage the HTTP MCP server config (indexes, keys, access)",
	Long:  "Edit ~/.config/qmd/server.yml: add indexes as mounts, issue and revoke bearer keys, and check for dead access rules.",
}

var keyCmd = &cobra.Command{
	Use:   "key",
	Short: "Issue, list and revoke bearer keys",
}

var keyAddCmd = &cobra.Command{
	Use:   "add <group>",
	Short: "Generate a key for a group and print it once",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		path, err := serverConfigPath(cmd)
		if err != nil {
			return err
		}
		label, _ := cmd.Flags().GetString("label")
		allow, _ := cmd.Flags().GetStringArray("allow")
		if !cmd.Flags().Changed("allow") {
			allow = nil
		}
		key, fp, err := addServerKey(path, args[0], label, allow)
		if err != nil {
			return err
		}
		fmt.Printf("group:       %s\n", args[0])
		fmt.Printf("fingerprint: %s\n", fp)
		fmt.Printf("key:         %s\n", key)
		fmt.Println()
		fmt.Println("This key is not stored. Put it in the client config now.")
		fmt.Println("The server picks it up without a restart.")
		return nil
	},
}

var keyListCmd = &cobra.Command{
	Use:   "list",
	Short: "List key fingerprints, groups and allowed paths",
	RunE: func(cmd *cobra.Command, args []string) error {
		path, err := serverConfigPath(cmd)
		if err != nil {
			return err
		}
		entries, err := listServerKeys(path)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			fmt.Println("no keys configured")
			return nil
		}
		for _, e := range entries {
			label := e.Label
			if label == "" {
				label = "-"
			}
			fmt.Printf("%-20s %-20s %-12s %s\n", shortHash(e.Key), e.Group, label, strings.Join(e.Allow, " "))
		}
		return nil
	},
}

var keyRemoveCmd = &cobra.Command{
	Use:   "remove <fingerprint>",
	Short: "Revoke a key by fingerprint prefix",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		path, err := serverConfigPath(cmd)
		if err != nil {
			return err
		}
		group, _ := cmd.Flags().GetString("group")
		n, err := removeServerKey(path, group, args[0])
		if err != nil {
			return err
		}
		fmt.Printf("removed %d key(s)\n", n)
		return nil
	},
}

var serverCheckCmd = &cobra.Command{
	Use:   "check",
	Short: "Report mounts, access rules and keys that need attention",
	RunE: func(cmd *cobra.Command, args []string) error {
		path, err := serverConfigPath(cmd)
		if err != nil {
			return err
		}
		findings, err := checkServerConfig(path)
		if err != nil {
			return err
		}
		if len(findings) == 0 {
			fmt.Println("ok")
			return nil
		}
		for _, f := range findings {
			fmt.Println("- " + f)
		}
		return nil
	},
}

func serverConfigPath(cmd *cobra.Command) (string, error) {
	p, _ := cmd.Flags().GetString("server-config")
	return resolveServerConfigPath(p)
}

func init() {
	serverCmd.PersistentFlags().String("server-config", "", "Server config YAML (default ~/.config/qmd/server.yml)")
	keyAddCmd.Flags().String("label", "", "Label for the key, e.g. a device name")
	keyAddCmd.Flags().StringArray("allow", nil, "Paths the group may use (repeatable); omit to keep existing")
	keyRemoveCmd.Flags().String("group", "", "Only remove the key from this group")
	keyCmd.AddCommand(keyAddCmd, keyListCmd, keyRemoveCmd)
	serverCmd.AddCommand(keyCmd, serverCheckCmd)
	rootCmd.AddCommand(serverCmd)
}
