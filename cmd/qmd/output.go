package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// SearchOutputRow is one row for search output (all formats).
type SearchOutputRow struct {
	Index    string
	Docid    string
	Filepath string
	Title    string
	Body     string
	Score    float64
	Context  string
	Full     bool
}

func docid(hash string) string {
	if len(hash) >= 6 {
		return hash[:6]
	}
	return hash
}

func escapeXML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return strings.ReplaceAll(s, "'", "&apos;")
}

func escapeCSV(s string) string {
	if strings.ContainsAny(s, ",\"\n") {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}

// WriteSearchOutput writes results in the requested format.
func WriteSearchOutput(rows []SearchOutputRow, format string, full bool, lineNumbers bool) {
	for i := range rows {
		if full {
			rows[i].Full = true
		}
		if lineNumbers {
			rows[i].Body = addLineNumbers(rows[i].Body, 1)
		}
	}
	switch format {
	case "json":
		out := make([]map[string]interface{}, 0, len(rows))
		for _, r := range rows {
			m := map[string]interface{}{
				"docid": "#" + r.Docid,
				"score": roundScore(r.Score),
				"file":  r.Filepath,
				"title": r.Title,
			}
			if r.Index != "" {
				m["index"] = r.Index
			}
			if r.Context != "" {
				m["context"] = r.Context
			}
			if r.Full {
				m["body"] = r.Body
			} else if r.Body != "" {
				m["snippet"] = truncateSnippet(r.Body, 300)
			}
			out = append(out, m)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
	case "files":
		for _, r := range rows {
			ctx := ""
			if r.Context != "" {
				ctx = `,"` + strings.ReplaceAll(r.Context, `"`, `""`) + `"`
			}
			file := r.Filepath
			if r.Index != "" {
				file = r.Index + ":" + file
			}
			fmt.Printf("#%s,%.2f,%s%s\n", r.Docid, r.Score, file, ctx)
		}
	case "csv":
		w := csv.NewWriter(os.Stdout)
		_ = w.Write([]string{"docid", "index", "score", "file", "title", "context", "snippet"})
		for _, r := range rows {
			snippet := r.Body
			if !r.Full && len(snippet) > 500 {
				snippet = truncateSnippet(snippet, 500)
			}
			_ = w.Write([]string{
				"#" + r.Docid,
				r.Index,
				strconv.FormatFloat(r.Score, 'f', 4, 64),
				r.Filepath,
				r.Title,
				r.Context,
				snippet,
			})
		}
		w.Flush()
	case "md":
		for _, r := range rows {
			fmt.Println("---")
			fmt.Printf("# %s\n\n", r.Title)
			if r.Index != "" {
				fmt.Printf("**index:** `%s`\n", r.Index)
			}
			fmt.Printf("**docid:** `#%s`\n", r.Docid)
			if r.Context != "" {
				fmt.Printf("**context:** %s\n", r.Context)
			}
			fmt.Println()
			fmt.Println(r.Body)
			fmt.Println()
		}
	case "xml":
		fmt.Println(`<?xml version="1.0" encoding="UTF-8"?>`)
		fmt.Println("<results>")
		for _, r := range rows {
			fmt.Println("  <result>")
			if r.Index != "" {
				fmt.Printf("    <index>%s</index>\n", escapeXML(r.Index))
			}
			fmt.Printf("    <docid>#%s</docid>\n", r.Docid)
			fmt.Printf("    <score>%.4f</score>\n", r.Score)
			fmt.Printf("    <file>%s</file>\n", escapeXML(r.Filepath))
			fmt.Printf("    <title>%s</title>\n", escapeXML(r.Title))
			if r.Context != "" {
				fmt.Printf("    <context>%s</context>\n", escapeXML(r.Context))
			}
			fmt.Printf("    <body>%s</body>\n", escapeXML(r.Body))
			fmt.Println("  </result>")
		}
		fmt.Println("</results>")
	default:
		for _, r := range rows {
			if r.Index != "" {
				fmt.Println(r.Index+":"+r.Filepath, "#"+r.Docid)
			} else {
				fmt.Println(r.Filepath, "#"+r.Docid)
			}
			if r.Title != "" {
				fmt.Println("Title:", r.Title)
			}
			if r.Context != "" {
				fmt.Println("Context:", r.Context)
			}
			fmt.Printf("Score: %.0f%%\n\n", r.Score*100)
			fmt.Println(r.Body)
			fmt.Println()
		}
	}
}

func roundScore(s float64) float64 {
	return float64(int(s*100+0.5)) / 100
}

func truncateSnippet(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
