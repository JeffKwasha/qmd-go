package main

import (
	"fmt"
	"os"
	"strings"

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
		query := strings.Join(args, " ")
		limit, _ := cmd.Flags().GetInt("n")
		minScore, _ := cmd.Flags().GetFloat64("min-score")
		collection, _ := cmd.Flags().GetString("collection")
		full, _ := cmd.Flags().GetBool("full")
		lineNumbers, _ := cmd.Flags().GetBool("line-numbers")
		format := getFormatFlag(cmd)

		sources, err := resolveSources()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer closeSources(sources)

		hasVectors := false
		for _, src := range sources {
			if src.hasVectors() {
				hasVectors = true
				break
			}
		}
		if !hasVectors {
			fmt.Fprintln(os.Stderr, "Vector index not found. Run 'qmd embed' first.")
			os.Exit(1)
		}

		hits, err := runVectors(sources, query, limit, minScore, collection)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if len(hits) > limit {
			hits = hits[:limit]
		}
		if len(hits) == 0 {
			fmt.Println("No results found.")
			return
		}
		rows := make([]SearchOutputRow, 0, len(hits))
		for _, h := range hits {
			rows = append(rows, rowFromHit(h, full, len(sources) > 1))
		}
		WriteSearchOutput(rows, format, full, lineNumbers)
	},
}

func init() {
	vsearchCmd.Flags().IntP("n", "n", 5, "Number of results")
	vsearchCmd.Flags().StringP("collection", "c", "", "Restrict to collection")
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
