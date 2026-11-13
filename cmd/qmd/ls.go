package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/ba0f3/qmd-go/internal/config"
	"github.com/spf13/cobra"
)

var lsCmd = &cobra.Command{
	Use:   "ls [collection[/path]]",
	Short: "List collections or files in a collection",
	Run: func(cmd *cobra.Command, args []string) {
		initRoot()
		requireIndex()
		s, err := openStore()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error opening store: %v\n", err)
			return
		}
		defer s.Close()

		cfg, err := config.LoadConfig()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
			return
		}

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
			return
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
