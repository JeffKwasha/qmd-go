package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/ba0f3/qmd-go/internal/config"
	"github.com/ba0f3/qmd-go/internal/llm"
	"github.com/ba0f3/qmd-go/internal/store"
	"github.com/spf13/cobra"
)

const rrfK = 60

// hybridResult is a merged result with RRF score.
type hybridResult struct {
	Filepath    string
	DisplayPath string
	Title       string
	Body        string
	Hash        string
	Score       float64
}

// reciprocalRankFusion merges FTS and vector results by filepath using RRF.
func reciprocalRankFusion(fts []store.SearchResult, vec []store.VecSearchResult, limit int) []hybridResult {
	scores := make(map[string]*hybridResult)
	for rank, r := range fts {
		rrf := 1.0 / (float64(rrfK) + float64(rank) + 1)
		if scores[r.Filepath] == nil {
			scores[r.Filepath] = &hybridResult{
				Filepath: r.Filepath, DisplayPath: r.DisplayPath, Title: r.Title, Body: r.Body, Hash: r.Hash, Score: rrf,
			}
		} else {
			scores[r.Filepath].Score += rrf
		}
	}
	for rank, r := range vec {
		rrf := 1.0 / (float64(rrfK) + float64(rank) + 1)
		if scores[r.Filepath] == nil {
			scores[r.Filepath] = &hybridResult{
				Filepath: r.Filepath, DisplayPath: r.DisplayPath, Title: r.Title, Body: r.Body, Hash: r.Hash, Score: rrf,
			}
		} else {
			scores[r.Filepath].Score += rrf
		}
	}
	// Sort by score descending and take top limit
	var list []*hybridResult
	for _, v := range scores {
		list = append(list, v)
	}
	for i := 0; i < len(list); i++ {
		for j := i + 1; j < len(list); j++ {
			if list[j].Score > list[i].Score {
				list[i], list[j] = list[j], list[i]
			}
		}
	}
	if limit <= 0 || limit > len(list) {
		limit = len(list)
	}
	out := make([]hybridResult, 0, limit)
	for i := 0; i < limit && i < len(list); i++ {
		out = append(out, *list[i])
	}
	return out
}

var queryCmd = &cobra.Command{
	Use:   "query [query]",
	Short: "Hybrid search (BM25 + vector)",
	Long:  "Combines BM25 and vector search with RRF. Run 'qmd embed' for vector results. No reranker in Go build.",
	Args:  cobra.MinimumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		initRoot()
		requireIndex()
		query := strings.Join(args, " ")
		limit, _ := cmd.Flags().GetInt("n")
		collection, _ := cmd.Flags().GetString("collection")
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

		fetchLimit := limit * 4
		if fetchLimit < 20 {
			fetchLimit = 20
		}

		// 1) BM25
		ftsResults, err := s.SearchFTS(query, fetchLimit, collection)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Search failed: %v\n", err)
			os.Exit(1)
		}

		var vecResults []store.VecSearchResult
		var hasVec int
		_ = s.DB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='embedding_blobs'`).Scan(&hasVec)
		if hasVec > 0 {
			indexCfg, _ := config.LoadConfig()
			client, err := llm.NewEmbedClient(config.EmbedModel(indexCfg))
			if err == nil {
				formatted := formatQueryForEmbedding(query)
				result, err := client.Embed(formatted)
				if err == nil {
					vecResults, err = s.SearchVectorsBrute(result.Embedding, fetchLimit)
					if err != nil {
						vecResults = nil
					}
				}
			}
		}

		// 2) Merge with RRF
		merged := reciprocalRankFusion(ftsResults, vecResults, limit)

		if len(merged) == 0 {
			fmt.Println("No results found.")
			fmt.Fprintln(os.Stderr, "Tip: Run 'qmd collection add' and 'qmd update' to index documents; run 'qmd embed' for vector search.")
			return
		}

		cfg, _ := config.LoadConfig()
		var rows []SearchOutputRow
		for _, r := range merged {
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
