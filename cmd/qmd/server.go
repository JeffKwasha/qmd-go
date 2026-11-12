package main

// Management commands for the HTTP MCP server config. They edit server.yml in
// place, preserving comments and key order by operating on the YAML node tree
// rather than re-marshalling a struct.
//
// Keys live in the single auth.keys list, one record per key. The commands
// never write the legacy groups.*.keys / key_hashes layout, and the loader
// refuses to read it.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ba0f3/qmd-go/internal/config"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// keyRow is a key plus the paths of its group, for display.
type keyRow struct {
	keyEntry
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

func isNullNode(n *yaml.Node) bool {
	return n != nil && n.Kind == yaml.ScalarNode && n.ShortTag() == "!!null"
}

// hasMergeKey reports a "<<" entry, which yaml expands at decode time, so the
// node tree does not show the keys it contributes.
func hasMergeKey(m *yaml.Node) bool {
	if m == nil || m.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].ShortTag() == "!!merge" || m.Content[i].Value == "<<" {
			return true
		}
	}
	return false
}

func replaceMappingChild(m *yaml.Node, key string, n *yaml.Node) bool {
	if m == nil || m.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			n.HeadComment = m.Content[i+1].HeadComment
			n.LineComment = m.Content[i+1].LineComment
			n.FootComment = m.Content[i+1].FootComment
			m.Content[i+1] = n
			return true
		}
	}
	return false
}

func ensureMapping(m *yaml.Node, key string) (*yaml.Node, error) {
	if n, ok := mappingChild(m, key); ok {
		if n.Kind == yaml.MappingNode {
			return n, nil
		}
		if !isNullNode(n) {
			return nil, fmt.Errorf("%s is not a mapping", key)
		}
		replacement := &yaml.Node{Kind: yaml.MappingNode}
		replaceMappingChild(m, key, replacement)
		return replacement, nil
	}
	n := &yaml.Node{Kind: yaml.MappingNode}
	m.Content = append(m.Content, scalarValue(key), n)
	return n, nil
}

func ensureSequence(m *yaml.Node, key string, style yaml.Style) (*yaml.Node, error) {
	if n, ok := mappingChild(m, key); ok {
		if n.Kind == yaml.SequenceNode {
			return n, nil
		}
		if !isNullNode(n) {
			return nil, fmt.Errorf("%s is not a list", key)
		}
		replacement := &yaml.Node{Kind: yaml.SequenceNode, Style: style}
		replaceMappingChild(m, key, replacement)
		return replacement, nil
	}
	n := &yaml.Node{Kind: yaml.SequenceNode, Style: style}
	m.Content = append(m.Content, scalarValue(key), n)
	return n, nil
}

func scalarValue(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
}

func quotedValue(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Value: s, Style: yaml.DoubleQuotedStyle}
}

func encodeYAML(doc *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeYAML(path string, doc *yaml.Node) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	data, err := encodeYAML(doc)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, info.Mode().Perm())
}

func readYAMLDoc(path string) (*yaml.Node, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &doc, nil
}

// editServerYAML rewrites path through fn, keeping the original file mode.
func editServerYAML(path string, fn func(root *yaml.Node) error) error {
	doc, err := readYAMLDoc(path)
	if err != nil {
		return err
	}
	if err := fn(documentRoot(doc)); err != nil {
		return err
	}
	return writeYAML(path, doc)
}

func keyNode(hash, group, label string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
		scalarValue("hash"), quotedValue(hash),
		scalarValue("group"), scalarValue(group),
	}}
	if label != "" {
		n.Content = append(n.Content, scalarValue("label"), scalarValue(label))
	}
	return n
}

type keyAddOptions struct {
	Group   string
	Label   string
	Allow   []string
	Expires time.Time
}

// addServerKey appends a freshly generated key to auth.keys and returns the
// plaintext key exactly once. Only its hash is written.
func addServerKey(path string, opt keyAddOptions) (string, string, error) {
	if _, err := loadServerConfig(path); err != nil {
		return "", "", err
	}
	if opt.Group == "" {
		return "", "", fmt.Errorf("group name required")
	}
	if strings.TrimSpace(opt.Group) != opt.Group {
		return "", "", fmt.Errorf("group name %q has leading or trailing whitespace", opt.Group)
	}
	if opt.Allow != nil && len(opt.Allow) == 0 {
		return "", "", fmt.Errorf("--allow path is empty")
	}
	for _, p := range opt.Allow {
		if strings.TrimSpace(p) == "" {
			return "", "", fmt.Errorf("--allow path is empty")
		}
	}
	if !opt.Expires.IsZero() && !opt.Expires.After(time.Now()) {
		return "", "", fmt.Errorf("expiry %s is in the past", opt.Expires.UTC().Format(time.RFC3339))
	}
	key, err := newKey()
	if err != nil {
		return "", "", err
	}
	issued := time.Now().UTC().Truncate(time.Second)
	err = editServerYAML(path, func(root *yaml.Node) error {
		auth, err := ensureMapping(root, "auth")
		if err != nil {
			return err
		}
		if hasMergeKey(auth) {
			// A merge key supplies the sections auth omits. Adding one would
			// shadow the merged section, so only edit a section written here.
			for _, key := range []string{"groups", "keys"} {
				if _, ok := mappingChild(auth, key); !ok {
					return fmt.Errorf("auth in %s gets %s from a merge key (<<); write auth.%s explicitly so key add does not drop it",
						path, key, key)
				}
			}
		}
		groups, err := ensureMapping(auth, "groups")
		if err != nil {
			return err
		}
		if hasMergeKey(groups) {
			if _, ok := mappingChild(groups, opt.Group); !ok {
				return fmt.Errorf("auth.groups in %s gets group %q from a merge key (<<); write it explicitly so key add does not drop the merged group",
					path, opt.Group)
			}
		}
		g, err := ensureMapping(groups, opt.Group)
		if err != nil {
			return err
		}
		if opt.Allow != nil && hasMergeKey(g) {
			return fmt.Errorf("group %q in %s gets allow from a merge key (<<); write allow explicitly so --allow does not drop the merged paths",
				opt.Group, path)
		}
		keys, err := ensureSequence(auth, "keys", 0)
		if err != nil {
			return err
		}
		entry := keyNode(hashKey(key), opt.Group, opt.Label)
		entry.Content = append(entry.Content,
			scalarValue("issued"), quotedValue(issued.Format(time.RFC3339)))
		if !opt.Expires.IsZero() {
			entry.Content = append(entry.Content,
				scalarValue("expires"), quotedValue(opt.Expires.UTC().Format(time.RFC3339)))
		}
		keys.Content = append(keys.Content, entry)
		if opt.Allow != nil {
			allowSeq, err := ensureSequence(g, "allow", yaml.FlowStyle)
			if err != nil {
				return err
			}
			allowSeq.Content = nil
			for _, p := range opt.Allow {
				allowSeq.Content = append(allowSeq.Content, quotedValue(normalizePath(p)))
			}
		}
		return nil
	})
	if err != nil {
		return "", "", err
	}
	return key, keyFingerprint(key), nil
}

// recordHash resolves one auth.keys entry the way the loader does: Decode
// applies aliases and merge keys, which the node tree still shows unresolved.
// An entry that does not resolve to a canonical hash is reported as not ok.
func recordHash(entry *yaml.Node) (hash, group string, ok bool) {
	var rec keyRecord
	if err := entry.Decode(&rec); err != nil {
		return "", "", false
	}
	canonical, err := canonicalKeyHash(rec.Hash)
	if err != nil {
		return "", "", false
	}
	return canonical, rec.Group, true
}

// removeServerKey deletes every record whose stored hash starts with
// fingerprint. The prefix must match one distinct hash; duplicate records of
// that hash are all removed, which is the recovery path for a config the loader
// refuses.
func removeServerKey(path, fingerprint string) (int, error) {
	fingerprint = strings.ToLower(strings.TrimSpace(fingerprint))
	if fingerprint == "" {
		return 0, fmt.Errorf("fingerprint required")
	}
	doc, err := readYAMLDoc(path)
	if err != nil {
		return 0, err
	}
	auth, ok := mappingChild(documentRoot(doc), "auth")
	if !ok {
		return 0, fmt.Errorf("no auth section in %s", path)
	}
	seq, ok := mappingChild(auth, "keys")
	if !ok {
		return 0, fmt.Errorf("no auth.keys in %s", path)
	}
	if isNullNode(seq) {
		return 0, fmt.Errorf("no key matches %q", fingerprint)
	}
	if seq.Kind != yaml.SequenceNode {
		return 0, fmt.Errorf("auth.keys is not a list in %s", path)
	}

	owners := map[string]string{}
	for _, entry := range seq.Content {
		canonical, group, ok := recordHash(entry)
		if !ok || !strings.HasPrefix(canonical, fingerprint) {
			continue
		}
		owners[canonical] = group
	}
	switch len(owners) {
	case 0:
		return 0, fmt.Errorf("no key matches %q", fingerprint)
	case 1:
	default:
		hashes := make([]string, 0, len(owners))
		for h := range owners {
			hashes = append(hashes, h)
		}
		sort.Strings(hashes)
		descr := make([]string, len(hashes))
		for i, h := range hashes {
			descr[i] = fmt.Sprintf("%s (group %q)", shortHash(h), owners[h])
		}
		return 0, fmt.Errorf("prefix %q matches %d keys: %s; give more characters",
			fingerprint, len(hashes), strings.Join(descr, ", "))
	}

	var target string
	for h := range owners {
		target = h
	}
	removed := 0
	kept := make([]*yaml.Node, 0, len(seq.Content))
	for _, entry := range seq.Content {
		if canonical, _, ok := recordHash(entry); ok && canonical == target {
			removed++
			continue
		}
		kept = append(kept, entry)
	}
	seq.Content = kept
	if err := writeYAML(path, doc); err != nil {
		return 0, err
	}
	return removed, nil
}

func listServerKeys(path string) ([]keyRow, error) {
	sc, err := loadServerConfig(path)
	if err != nil {
		return nil, err
	}
	out := make([]keyRow, 0, len(sc.byHash))
	for _, e := range sc.byHash {
		out = append(out, keyRow{keyEntry: *e, Allow: sc.Auth.Groups[e.Group].Allow})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Group != out[j].Group {
			return out[i].Group < out[j].Group
		}
		return out[i].Hash < out[j].Hash
	})
	return out, nil
}

// checkServerConfig reports configuration problems that do not stop the server
// from starting: dead mounts, allow paths that reach nothing, groups no key can
// use, and keys that are expired or about to expire.
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
	keysPerGroup := make(map[string]int, len(sc.Auth.Groups))
	for _, e := range sc.byHash {
		keysPerGroup[e.Group]++
	}
	if len(sc.byHash) == 0 && len(sc.Auth.Public) == 0 {
		findings = append(findings, "no keys and no public paths: every mount answers 401")
	}
	for _, p := range sc.Auth.Public {
		if _, ok := sc.Servers[p]; !ok {
			findings = append(findings, fmt.Sprintf("public path %s is not mounted", p))
		}
	}
	for name, g := range sc.Auth.Groups {
		if keysPerGroup[name] == 0 {
			findings = append(findings, fmt.Sprintf("group %q has no keys, so nothing can use it", name))
		}
		if len(g.Allow) == 0 {
			findings = append(findings, fmt.Sprintf("group %q allows no paths", name))
		}
		for _, a := range g.Allow {
			if _, ok := sc.Servers[normalizePath(a)]; !ok {
				findings = append(findings, fmt.Sprintf("group %q allows %s, which is not mounted", name, a))
			}
		}
	}
	now := time.Now()
	for _, e := range sc.byHash {
		if !e.Issued.IsZero() && !e.Expires.IsZero() && e.Expires.Before(e.Issued) {
			findings = append(findings, fmt.Sprintf("key %s (group %q) expires on %s, before it was issued on %s",
				shortHash(e.Hash), e.Group, e.Expires.Format("2006-01-02"), e.Issued.Format("2006-01-02")))
			continue
		}
		switch {
		case e.Expires.IsZero():
		case !now.Before(e.Expires):
			findings = append(findings, fmt.Sprintf("key %s (group %q) expired on %s; remove it with: qmd server key remove %s",
				shortHash(e.Hash), e.Group, e.Expires.Format("2006-01-02"), shortHash(e.Hash)))
		case e.Expires.Sub(now) <= 14*24*time.Hour:
			findings = append(findings, fmt.Sprintf("key %s (group %q) expires on %s",
				shortHash(e.Hash), e.Group, e.Expires.Format("2006-01-02")))
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

func expiryLabel(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.Format("2006-01-02")
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
		expires, _ := cmd.Flags().GetString("expires")
		expiry, err := parseExpires(expires, time.Now())
		if err != nil {
			return err
		}
		key, fp, err := addServerKey(path, keyAddOptions{
			Group:   args[0],
			Label:   label,
			Allow:   allow,
			Expires: expiry,
		})
		if err != nil {
			return err
		}
		fmt.Printf("group:       %s\n", args[0])
		fmt.Printf("fingerprint: %s\n", fp)
		fmt.Printf("expires:     %s\n", expiryLabel(expiry))
		fmt.Printf("key:         %s\n", key)
		fmt.Println()
		fmt.Println("This key is not stored. Put it in the client config now.")
		fmt.Println("The server picks it up without a restart.")
		return nil
	},
}

var keyListCmd = &cobra.Command{
	Use:   "list",
	Short: "List key fingerprints, groups, expiry and allowed paths",
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
		fmt.Printf("%-20s %-14s %-14s %-12s %s\n", "FINGERPRINT", "GROUP", "LABEL", "EXPIRES", "PATHS")
		for _, e := range entries {
			label := e.Label
			if label == "" {
				label = "-"
			}
			fmt.Printf("%-20s %-14s %-14s %-12s %s\n",
				shortHash(e.Hash), e.Group, label, expiryLabel(e.Expires), strings.Join(e.Allow, " "))
		}
		return nil
	},
}

var keyRemoveCmd = &cobra.Command{
	Use:   "remove <fingerprint>",
	Short: "Revoke one key by fingerprint prefix",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		path, err := serverConfigPath(cmd)
		if err != nil {
			return err
		}
		n, err := removeServerKey(path, args[0])
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
	keyAddCmd.Flags().String("expires", "", "Expiry: 30d, 720h, 2027-03-01 or RFC3339; omit for never")
	keyCmd.AddCommand(keyAddCmd, keyListCmd, keyRemoveCmd)
	serverCmd.AddCommand(keyCmd, serverCheckCmd)
	rootCmd.AddCommand(serverCmd)
	for _, c := range []*cobra.Command{keyAddCmd, keyListCmd, keyRemoveCmd, serverCheckCmd} {
		c.SilenceUsage = true
		c.SilenceErrors = true
	}
}
