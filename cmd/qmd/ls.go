package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"

	"github.com/ba0f3/qmd-go/internal/config"
	"github.com/ba0f3/qmd-go/internal/store"
	"github.com/spf13/cobra"
)

// printIndexes lists every index in the config directory with its database and
// collections, which is what `qmd ls` means when there is no default index.
func printIndexes(names []string) bool {
	ok := true
	dir, _ := config.GetConfigDir()
	fmt.Printf("Indexes in %s:\n", dir)
	for _, name := range names {
		dbPath, err := config.DBPathFor(name)
		if err != nil {
			fmt.Printf("\n  %s  (no database path: %v)\n", name, err)
			ok = false
			continue
		}
		fmt.Printf("\n  %s  %s\n", name, dbPath)
		cfg, err := config.LoadConfigFor(name)
		if err != nil {
			fmt.Printf("    cannot read config: %v\n", err)
			ok = false
			continue
		}
		if len(cfg.Collections) == 0 {
			fmt.Println("    no collections")
			continue
		}
		if _, err := os.Stat(dbPath); errors.Is(err, fs.ErrNotExist) {
			fmt.Println("    (not built; run qmd update)")
			continue
		} else if err != nil {
			fmt.Printf("    cannot read database: %v\n", err)
			ok = false
			continue
		}
		s, err := store.NewStore(dbPath)
		if err != nil {
			fmt.Printf("    cannot open database: %v\n", err)
			ok = false
			continue
		}
		collections := make([]string, 0, len(cfg.Collections))
		for c := range cfg.Collections {
			collections = append(collections, c)
		}
		sort.Strings(collections)
		for _, c := range collections {
			var n int
			_ = s.DB.QueryRow(`SELECT COUNT(*) FROM documents WHERE collection = ? AND active = 1`, c).Scan(&n)
			fmt.Printf("    qmd://%s/  (%d files)\n", c, n)
		}
		_ = s.Close()
	}
	return ok
}

var lsCmd = &cobra.Command{
	Use:   "ls [collection[/path]]",
	Short: "List collections or files in a collection",
	Run: func(cmd *cobra.Command, args []string) {
		initRoot()
		// With several indexes and no --index, the useful answer is the list of
		// them. Refusing to run and asking for a flag is what `ls` should not
		// do, since listing is its whole job.
		if len(args) == 0 && !rootCmd.PersistentFlags().Changed("index") && !config.HasIndexConfig("index") {
			if names, err := config.IndexNames(); err == nil && len(names) > 0 {
				if !printIndexes(names) {
					os.Exit(1)
				}
				return
			}
		}
		requireIndex()
		cfg, err := config.LoadConfig()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
			os.Exit(1)
		}
		if dbPath, err := config.DBPathFor(getIndexName()); err == nil {
			if _, err := os.Stat(dbPath); errors.Is(err, fs.ErrNotExist) {
				fmt.Printf("Index %q (%s): not built; run qmd update\n", getIndexName(), dbPath)
				return
			} else if err != nil {
				fmt.Fprintf(os.Stderr, "Index %q (%s): %v\n", getIndexName(), dbPath, err)
				os.Exit(1)
			}
		}
		s, err := openStore()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error opening store: %v\n", err)
			os.Exit(1)
		}
		defer s.Close()

		if len(args) == 0 || args[0] == "" {
			if len(cfg.Collections) == 0 {
				fmt.Println("No collections. Run 'qmd collection add .' to index files.")
				return
			}
			fmt.Println("Collections:")
			fmt.Println()
			for name := range cfg.Collections {
				var cnt int
				_ = s.DB.QueryRow(`SELECT COUNT(*) FROM documents WHERE collection = ? AND active = 1`, name).Scan(&cnt)
				fmt.Printf("  qmd://%s/  (%d files)\n", name, cnt)
			}
			return
		}

		arg := args[0]
		var collectionName, pathPrefix string
		if strings.HasPrefix(arg, "qmd://") {
			rest := strings.TrimPrefix(arg, "qmd://")
			idx := strings.Index(rest, "/")
			if idx < 0 {
				collectionName = rest
			} else {
				collectionName = rest[:idx]
				pathPrefix = rest[idx+1:]
			}
		} else {
			parts := strings.SplitN(arg, "/", 2)
			collectionName = parts[0]
			if len(parts) > 1 {
				pathPrefix = parts[1]
			}
		}

		if _, ok := cfg.Collections[collectionName]; !ok {
			fmt.Fprintf(os.Stderr, "Collection not found: %s\n", collectionName)
			os.Exit(1)
		}

		sql := `SELECT d.path, d.title, d.modified_at, LENGTH(content.doc) as size
			FROM documents d
			JOIN content ON content.hash = d.hash
			WHERE d.collection = ? AND d.active = 1`
		argsQ := []interface{}{collectionName}
		if pathPrefix != "" {
			sql += ` AND d.path LIKE ?`
			argsQ = append(argsQ, pathPrefix+"%")
		}
		sql += ` ORDER BY d.path`

		rows, err := s.DB.Query(sql, argsQ...)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return
		}
		defer rows.Close()

		var path, title, modified string
		var size int64
		count := 0
		for rows.Next() {
			if err := rows.Scan(&path, &title, &modified, &size); err != nil {
				continue
			}
			count++
			sizeStr := formatBytes(size)
			fmt.Printf("%10s  qmd://%s/%s\n", sizeStr, collectionName, path)
		}
		if count == 0 {
			if pathPrefix != "" {
				fmt.Printf("No files under qmd://%s/%s\n", collectionName, pathPrefix)
			} else {
				fmt.Printf("No files in collection %s\n", collectionName)
			}
		}
	},
}

func init() {
	rootCmd.AddCommand(lsCmd)
}
