package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/ba0f3/qmd-go/internal/config"
	"github.com/ba0f3/qmd-go/internal/store"
	"github.com/spf13/cobra"
)

var getCmd = &cobra.Command{
	Use:   "get <file>[:line]",
	Short: "Get document by path or docid",
	Long:  "Get document by path (qmd://collection/path, collection/path, or index:qmd://collection/path as printed by search) or by docid (#abc123).",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		input := args[0]
		fromLine, _ := cmd.Flags().GetInt("from")
		maxLines, _ := cmd.Flags().GetInt("l")
		lineNumbers, _ := cmd.Flags().GetBool("line-numbers")
		full, _ := cmd.Flags().GetBool("full")
		if full {
			maxLines = 0 // output full document (no line limit)
		}

		// Parse :line suffix (e.g. file.md:100)
		if fromLine == 0 && strings.Contains(input, ":") {
			if idx := strings.LastIndex(input, ":"); idx >= 0 {
				if n, err := strconv.Atoi(input[idx+1:]); err == nil {
					fromLine = n
					input = input[:idx]
				}
			}
		}

		initRoot()
		sources, err := resolveSources()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer closeSources(sources)
		sources, input = indexPrefix(sources, input)

		src, collection, path, ok := findDoc(sources, input)
		if !ok {
			fmt.Fprintf(os.Stderr, "Document not found: %s\n", input)
			os.Exit(1)
		}

		body, err := src.Store.GetDocumentBody(collection, path, fromLine, maxLines)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Document not found: %s\n", input)
			os.Exit(1)
		}

		if ctx := config.FindContextForPath(src.Cfg, collection, path); ctx != "" {
			fmt.Println("Folder Context: " + ctx)
			fmt.Println("---")
		}
		if lineNumbers {
			body = addLineNumbers(body, fromLine)
		}
		fmt.Print(body)
	},
}

func resolveInputToDoc(s *store.Store, input string) (collection, path string) {
	input = strings.TrimSpace(input)

	// Docid: #abc123 or abc123 (hex, 6+ chars)
	if trimmed := strings.TrimPrefix(input, "#"); trimmed != input || (len(input) >= 6 && isHex(input)) {
		c, p, _, err := s.FindByDocid(input)
		if err == nil && c != "" {
			return c, p
		}
	}

	// qmd://collection/path
	if strings.HasPrefix(input, "qmd://") {
		rest := strings.TrimPrefix(input, "qmd://")
		idx := strings.Index(rest, "/")
		if idx < 0 {
			return rest, ""
		}
		return rest[:idx], rest[idx+1:]
	}

	// collection/path (first segment is collection name)
	parts := strings.SplitN(input, "/", 2)
	if len(parts) == 2 && parts[0] != "" {
		return parts[0], parts[1]
	}
	return "", ""
}

func isHex(s string) bool {
	for _, c := range s {
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') {
			continue
		}
		return false
	}
	return true
}

func addLineNumbers(text string, start int) string {
	if start <= 0 {
		start = 1
	}
	lines := strings.Split(text, "\n")
	var b strings.Builder
	for i, line := range lines {
		fmt.Fprintf(&b, "%d: %s\n", start+i, line)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func init() {
	getCmd.Flags().Int("from", 0, "Start line (1-based)")
	getCmd.Flags().IntP("l", "l", 0, "Maximum lines to output")
	getCmd.Flags().Bool("full", false, "Output full document (same as default for get)")
	getCmd.Flags().Bool("line-numbers", false, "Add line numbers")
	rootCmd.AddCommand(getCmd)
}
