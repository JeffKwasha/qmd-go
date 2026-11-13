package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/ba0f3/qmd-go/internal/config"
	"github.com/spf13/cobra"
)

var searchCmd = &cobra.Command{
	Use:   "search [query]",
	Short: "Full-text search (BM25)",
	Args:  cobra.MinimumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		initRoot()
		requireIndex()
		query := strings.Join(args, " ")
		limit, _ := cmd.Flags().GetInt("n")
		collection, _ := cmd.Flags().GetString("collection")
		all, _ := cmd.Flags().GetBool("all")
		minScore, _ := cmd.Flags().GetFloat64("min-score")
		full, _ := cmd.Flags().GetBool("full")
		lineNumbers, _ := cmd.Flags().GetBool("line-numbers")
		format, _ := cmd.Flags().GetString("format")
		useJSON, _ := cmd.Flags().GetBool("json")
		useCSV, _ := cmd.Flags().GetBool("csv")
		useMD, _ := cmd.Flags().GetBool("md")
		useXML, _ := cmd.Flags().GetBool("xml")
		useFiles, _ := cmd.Flags().GetBool("files")
		if useJSON {
			format = "json"
		} else if useCSV {
			format = "csv"
		} else if useMD {
			format = "md"
		} else if useXML {
			format = "xml"
		} else if useFiles {
			format = "files"
		}

		if all && limit == 5 {
			limit = 100000
		}
		if (format == "json" || format == "files") && limit == 5 {
			limit = 20
		}

		s, err := openStore()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error opening store: %v\n", err)
			os.Exit(1)
		}
		defer s.Close()

		results, err := s.SearchFTS(query, limit, collection)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Search failed: %v\n", err)
			os.Exit(1)
		}

		cfg, _ := config.LoadConfig()
		var rows []SearchOutputRow
		for _, r := range results {
			if r.Score < minScore {
				continue
			}
			ctx := ""
			if cfg != nil {
				path := r.DisplayPath
				if idx := strings.Index(path, "/"); idx >= 0 {
					path = path[idx+1:]
				}
				ctx = config.FindContextForPath(cfg, r.CollectionName, path)
			}
			body := r.Body
			if !full && len(body) > 500 {
				body = body[:500] + "..."
			}
			rows = append(rows, SearchOutputRow{
				Docid:    docid(r.Hash),
				Filepath: r.Filepath,
				Title:    r.Title,
				Body:     body,
				Score:    r.Score,
				Context:  ctx,
				Full:     full,
			})
		}

		if len(rows) == 0 {
			fmt.Println("No results found.")
			return
		}
		WriteSearchOutput(rows, format, full, lineNumbers)
	},
}

func init() {
	searchCmd.Flags().IntP("n", "n", 5, "Number of results")
	searchCmd.Flags().StringP("collection", "c", "", "Restrict to collection")
	searchCmd.Flags().Bool("all", false, "Return all matches (use with --min-score)")
	searchCmd.Flags().Float64("min-score", 0, "Minimum score threshold")
	searchCmd.Flags().Bool("full", false, "Show full document content")
	searchCmd.Flags().Bool("line-numbers", false, "Add line numbers")
	searchCmd.Flags().String("format", "cli", "Output: cli, json, csv, md, xml, files")
	searchCmd.Flags().Bool("json", false, "JSON output (short for --format=json)")
	searchCmd.Flags().Bool("csv", false, "CSV output")
	searchCmd.Flags().Bool("md", false, "Markdown output")
	searchCmd.Flags().Bool("xml", false, "XML output")
	searchCmd.Flags().Bool("files", false, "Output docid,score,filepath,context")
	rootCmd.AddCommand(searchCmd)
}
