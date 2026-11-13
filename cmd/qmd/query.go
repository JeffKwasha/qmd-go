package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

var queryCmd = &cobra.Command{
	Use:   "query [query]",
	Short: "Hybrid search (BM25 + vector)",
	Long:  "Combines BM25 and vector search with RRF. Run 'qmd embed' for vector results. No reranker in Go build.",
	Args:  cobra.MinimumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		initRoot()
		query := strings.Join(args, " ")
		limit, _ := cmd.Flags().GetInt("n")
		collection, _ := cmd.Flags().GetString("collection")
		minScore, _ := cmd.Flags().GetFloat64("min-score")
		full, _ := cmd.Flags().GetBool("full")
		lineNumbers, _ := cmd.Flags().GetBool("line-numbers")
		format := getFormatFlag(cmd)

		sources, err := resolveSources()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer closeSources(sources)

		fetchLimit := limit * 4
		if fetchLimit < 20 {
			fetchLimit = 20
		}
		hits, err := runHybrid(sources, query, fetchLimit, collection, minScore)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Search failed: %v\n", err)
			os.Exit(1)
		}
		if len(hits) > limit {
			hits = hits[:limit]
		}
		if len(hits) == 0 {
			fmt.Println("No results found.")
			fmt.Fprintln(os.Stderr, "Tip: Run 'qmd collection add' and 'qmd update' to index documents; run 'qmd embed' for vector search.")
			return
		}
		rows := make([]SearchOutputRow, 0, len(hits))
		for _, h := range hits {
			rows = append(rows, rowFromHit(h, full, len(sources) > 1))
		}
		WriteSearchOutput(rows, format, full, lineNumbers)
	},
}

func parseVirtualPath(v string) (collection, path string) {
	if !strings.HasPrefix(v, "qmd://") {
		return "", ""
	}
	rest := strings.TrimPrefix(v, "qmd://")
	idx := strings.Index(rest, "/")
	if idx < 0 {
		return rest, ""
	}
	return rest[:idx], rest[idx+1:]
}

func getFormatFlag(cmd *cobra.Command) string {
	if ok, _ := cmd.Flags().GetBool("json"); ok {
		return "json"
	}
	if ok, _ := cmd.Flags().GetBool("csv"); ok {
		return "csv"
	}
	if ok, _ := cmd.Flags().GetBool("md"); ok {
		return "md"
	}
	if ok, _ := cmd.Flags().GetBool("xml"); ok {
		return "xml"
	}
	if ok, _ := cmd.Flags().GetBool("files"); ok {
		return "files"
	}
	s, _ := cmd.Flags().GetString("format")
	if s == "" {
		return "cli"
	}
	return s
}

func init() {
	queryCmd.Flags().IntP("n", "n", 5, "Number of results")
	queryCmd.Flags().StringP("collection", "c", "", "Restrict to collection")
	queryCmd.Flags().Float64("min-score", 0, "Minimum score threshold")
	queryCmd.Flags().Bool("full", false, "Show full document content")
	queryCmd.Flags().Bool("line-numbers", false, "Add line numbers")
	queryCmd.Flags().String("format", "cli", "Output: cli, json, csv, md, xml, files")
	queryCmd.Flags().Bool("json", false, "JSON output")
	queryCmd.Flags().Bool("csv", false, "CSV output")
	queryCmd.Flags().Bool("md", false, "Markdown output")
	queryCmd.Flags().Bool("xml", false, "XML output")
	queryCmd.Flags().Bool("files", false, "Output docid,score,filepath,context")
	rootCmd.AddCommand(queryCmd)
}
