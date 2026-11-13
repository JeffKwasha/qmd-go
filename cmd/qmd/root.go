package main

import (
	"fmt"
	"os"
	"runtime/debug"
	"time"

	"github.com/ba0f3/qmd-go/internal/config"
	"github.com/ba0f3/qmd-go/internal/llm"
	"github.com/ba0f3/qmd-go/internal/store"
	"github.com/spf13/cobra"
)

const version = "1.0.0"

// versionString answers "which build is this?" in one line per fact. `qmd
// --version` and `qmd version` print it, because the CLI's file formats change
// between builds and an installed binary is not visibly one version or another.
func versionString() string {
	backend := "API backend only (Ollama/OpenAI)"
	if llm.GGUFEnabled() {
		backend = "GGUF support enabled"
	}
	// Commit time first: a person comparing two builds wants to know which is
	// newer, and a hash does not say that. The hash follows for cross-reference.
	stamp := backend
	if rev, when := buildIdentity(); rev != "" {
		stamp = when.Format("2006-01-02 15:04Z") + " (" + rev + "), " + backend
	}
	return fmt.Sprintf("qmd version %s\nBuild: %s\nDefault embed model: %s\n",
		version, stamp, llm.DefaultEmbedModel())
}

// buildIdentity reports the commit the binary was built from and when it was
// committed, as Go recorded them at build time. A version number alone cannot
// tell two builds of this CLI apart, and the file formats move between builds.
func buildIdentity() (string, time.Time) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", time.Time{}
	}
	rev, when, dirty := "", time.Time{}, false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.time":
			if t, err := time.Parse(time.RFC3339, s.Value); err == nil {
				when = t.UTC()
			}
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "", time.Time{}
	}
	if len(rev) > 7 {
		rev = rev[:7]
	}
	if dirty {
		rev += "+dirty"
	}
	return rev, when
}

var rootCmd = &cobra.Command{
	Use:     "qmd",
	Short:   "Quick Markdown Search",
	Version: versionString(),
	Long: `An on-device search engine for everything you need to remember.
Index your markdown notes, meeting transcripts, documentation, and knowledge bases.`,
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Show version and build info",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Fprint(cmd.OutOrStdout(), versionString())
	},
}

func getIndexName() string {
	name, _ := rootCmd.PersistentFlags().GetString("index")
	if name == "" {
		return "index"
	}
	return name
}

func getStorePath() (string, error) {
	return config.DBPathFor(getIndexName())
}

func openStore() (*store.Store, error) {
	path, err := getStorePath()
	if err != nil {
		return nil, err
	}
	return store.NewStore(path)
}

func initRoot() {
	config.CurrentIndexName = getIndexName()
}

// requireIndex stops a read that names an index with no config. It runs before
// the store opens, because DBPathFor would otherwise invent a cache path and
// NewStore would create an empty database for an index that does not exist.
func requireIndex() {
	if _, err := config.LoadIndexConfig(getIndexName()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().String("index", "", "Use one named index (default: index.yml)")
	rootCmd.SetVersionTemplate("{{.Version}}")
	rootCmd.AddCommand(versionCmd)
}
