package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/ba0f3/qmd-go/internal/config"
	"github.com/ba0f3/qmd-go/internal/llm"
	"github.com/spf13/cobra"
)

func formatQueryForEmbedding(query string) string {
	return "task: search result | query: " + query
}

var vsearchCmd = &cobra.Command{
	Use:   "vsearch [query]",
	Short: "Vector similarity search",
	Long:  "Search using vector embeddings. Run 'qmd embed' first. Uses Ollama or OpenAI-compatible API.",
	Args:  cobra.MinimumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		initRoot()
		requireIndex()
		query := strings.Join(args, " ")
		limit, _ := cmd.Flags().GetInt("n")
		minScore, _ := cmd.Flags().GetFloat64("min-score")
		full, _ := cmd.Flags().GetBool("full")
		lineNumbers, _ := cmd.Flags().GetBool("line-numbers")
		format := getFormatFlag(cmd)

		s, err := openStore()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error opening store: %v\n", err)
			os.Exit(1)
		}
		defer s.Close()

		var hasTable int
		if err := s.DB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='embedding_blobs'`).Scan(&hasTable); err != nil || hasTable == 0 {
			fmt.Fprintln(os.Stderr, "Vector index not found. Run 'qmd embed' first.")
			os.Exit(1)
		}

		cfg, _ := config.LoadConfig()
		client, err := llm.NewEmbedClient(config.EmbedModel(cfg))
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error creating embed client: %v\n", err)
			os.Exit(1)
		}

		formatted := formatQueryForEmbedding(query)
		result, err := client.Embed(formatted)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error embedding query: %v\n", err)
			os.Exit(1)
		}

		results, err := s.SearchVectorsBrute(result.Embedding, limit)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error searching: %v\n", err)
			os.Exit(1)
		}

		var rows []SearchOutputRow
		for _, r := range results {
			if r.Score < minScore {
				continue
			}
			ctx := ""
			if cfg != nil {
				col, path := parseVirtualPath(r.Filepath)
				if col != "" {
					ctx = config.FindContextForPath(cfg, col, path)
				}
			}
			body := r.Body
			if !full && len(body) > 500 {
				body = body[:500] + "..."
			}
			rows = append(rows, SearchOutputRow{
				Docid: docid(r.Hash), Filepath: r.Filepath, Title: r.Title, Body: body, Score: r.Score, Context: ctx, Full: full,
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
	vsearchCmd.Flags().IntP("n", "n", 5, "Number of results")
	vsearchCmd.Flags().StringP("collection", "c", "", "Restrict to collection (not yet implemented)")
	vsearchCmd.Flags().Float64("min-score", 0.3, "Minimum score threshold")
	vsearchCmd.Flags().Bool("full", false, "Show full document content")
	vsearchCmd.Flags().Bool("line-numbers", false, "Add line numbers")
	vsearchCmd.Flags().String("format", "cli", "Output: cli, json, csv, md, xml, files")
	vsearchCmd.Flags().Bool("json", false, "JSON output")
	vsearchCmd.Flags().Bool("csv", false, "CSV output")
	vsearchCmd.Flags().Bool("md", false, "Markdown output")
	vsearchCmd.Flags().Bool("xml", false, "XML output")
	vsearchCmd.Flags().Bool("files", false, "Output docid,score,filepath,context")
	rootCmd.AddCommand(vsearchCmd)
}
