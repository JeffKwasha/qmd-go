package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/ba0f3/qmd-go/internal/config"
	"github.com/ba0f3/qmd-go/internal/store"
	"github.com/bmatcuk/doublestar/v4"
	"github.com/spf13/cobra"
)

const defaultMultiGetMaxBytes = 10240

var multiGetCmd = &cobra.Command{
	Use:   "multi-get <pattern>",
	Short: "Get multiple documents by glob or comma-separated list",
	Long:  "Pattern can be a glob (e.g. journals/2025*.md) or comma-separated paths/docids.",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		pattern := args[0]
		maxLines, _ := cmd.Flags().GetInt("l")
		maxBytes, _ := cmd.Flags().GetInt("max-bytes")
		if maxBytes <= 0 {
			maxBytes = defaultMultiGetMaxBytes
		}
		format := "cli"
		if ok, _ := cmd.Flags().GetBool("json"); ok {
			format = "json"
		} else if ok, _ := cmd.Flags().GetBool("csv"); ok {
			format = "csv"
		} else if ok, _ := cmd.Flags().GetBool("md"); ok {
			format = "md"
		} else if ok, _ := cmd.Flags().GetBool("xml"); ok {
			format = "xml"
		} else if ok, _ := cmd.Flags().GetBool("files"); ok {
			format = "files"
		}

		initRoot()
		requireIndex()
		s, err := openStore()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error opening store: %v\n", err)
			os.Exit(1)
		}
		defer s.Close()

		var docs []docForMultiGet
		if strings.Contains(pattern, ",") && !strings.Contains(pattern, "*") && !strings.Contains(pattern, "?") {
			docs = multiGetByList(s, strings.Split(pattern, ","), maxLines, maxBytes)
		} else {
			docs = multiGetByGlob(s, pattern, maxLines, maxBytes)
		}

		if len(docs) == 0 {
			fmt.Fprintf(os.Stderr, "No files matched pattern: %s\n", pattern)
			os.Exit(1)
		}

		switch format {
		case "json":
			out := make([]map[string]interface{}, 0, len(docs))
			for _, d := range docs {
				m := map[string]interface{}{
					"file":  d.DisplayPath,
					"title": d.Title,
				}
				if d.Context != "" {
					m["context"] = d.Context
				}
				if d.Skipped {
					m["skipped"] = true
					m["reason"] = d.SkipReason
				} else {
					m["body"] = d.Body
				}
				out = append(out, m)
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(out)
			return
		case "csv":
			fmt.Println("file,title,context,skipped,body")
			for _, d := range docs {
				skipped := "false"
				body := d.Body
				if d.Skipped {
					skipped = "true"
					body = d.SkipReason
				}
				fmt.Printf("%s,%s,%s,%s,%s\n", escapeCSV(d.DisplayPath), escapeCSV(d.Title), escapeCSV(d.Context), skipped, escapeCSV(body))
			}
			return
		case "md":
			for _, d := range docs {
				fmt.Println("##", d.DisplayPath)
				fmt.Println()
				if d.Title != "" {
					fmt.Println("**Title:**", d.Title)
					fmt.Println()
				}
				if d.Context != "" {
					fmt.Println("**Context:**", d.Context)
					fmt.Println()
				}
				if d.Skipped {
					fmt.Println(">", d.SkipReason)
					fmt.Println()
				} else {
					fmt.Println("```")
					fmt.Println(d.Body)
					fmt.Println("```")
					fmt.Println()
				}
			}
			return
		case "xml":
			fmt.Println(`<?xml version="1.0" encoding="UTF-8"?>`)
			fmt.Println("<documents>")
			for _, d := range docs {
				fmt.Println("  <document>")
				fmt.Printf("    <file>%s</file>\n", escapeXML(d.DisplayPath))
				fmt.Printf("    <title>%s</title>\n", escapeXML(d.Title))
				if d.Context != "" {
					fmt.Printf("    <context>%s</context>\n", escapeXML(d.Context))
				}
				if d.Skipped {
					fmt.Printf("    <skipped>true</skipped>\n    <reason>%s</reason>\n", escapeXML(d.SkipReason))
				} else {
					fmt.Printf("    <body>%s</body>\n", escapeXML(d.Body))
				}
				fmt.Println("  </document>")
			}
			fmt.Println("</documents>")
			return
		case "files":
			for _, d := range docs {
				ctx := ""
				if d.Context != "" {
					ctx = `,"` + strings.ReplaceAll(d.Context, `"`, `""`) + `"`
				}
				skip := ""
				if d.Skipped {
					skip = ",[SKIPPED]"
				}
				fmt.Printf("%s%s%s\n", d.DisplayPath, ctx, skip)
			}
			return
		}

		for _, d := range docs {
			fmt.Println(strings.Repeat("=", 60))
			fmt.Println("File:", d.DisplayPath)
			fmt.Println(strings.Repeat("=", 60))
			if d.Skipped {
				fmt.Println("[SKIPPED:", d.SkipReason+"]")
				continue
			}
			if d.Context != "" {
				fmt.Println("Folder Context:", d.Context)
				fmt.Println("---")
			}
			fmt.Println(d.Body)
		}
	},
}

type docForMultiGet struct {
	DisplayPath string
	Title       string
	Body        string
	Context     string
	Skipped     bool
	SkipReason  string
}

func multiGetByList(s *store.Store, names []string, maxLines, maxBytes int) []docForMultiGet {
	var out []docForMultiGet
	cfg, _ := config.LoadConfig()
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		collection, path := resolveInputToDoc(s, name)
		if collection == "" && path == "" {
			fmt.Fprintf(os.Stderr, "File not found: %s\n", name)
			continue
		}
		body, err := s.GetDocumentBody(collection, path, 0, maxLines)
		if err != nil {
			fmt.Fprintf(os.Stderr, "File not found: %s\n", name)
			continue
		}
		displayPath := "qmd://" + collection + "/" + path
		title := path
		if idx := strings.LastIndex(title, "/"); idx >= 0 {
			title = title[idx+1:]
		}
		ctx := ""
		if cfg != nil {
			ctx = config.FindContextForPath(cfg, collection, path)
		}
		skipped := false
		reason := ""
		if len(body) > maxBytes {
			skipped = true
			reason = fmt.Sprintf("File too large (%d bytes > %d bytes)", len(body), maxBytes)
			body = ""
		}
		out = append(out, docForMultiGet{
			DisplayPath: displayPath,
			Title:       title,
			Body:        body,
			Context:     ctx,
			Skipped:     skipped,
			SkipReason:  reason,
		})
	}
	return out
}

func multiGetByGlob(s *store.Store, pattern string, maxLines, maxBytes int) []docForMultiGet {
	paths, err := s.ListDocumentPaths()
	if err != nil {
		return nil
	}
	var matched []store.DocPath
	for _, p := range paths {
		ok, _ := doublestar.Match(pattern, p.Filepath)
		if !ok {
			ok, _ = doublestar.Match(pattern, p.DisplayPath)
		}
		if ok {
			matched = append(matched, p)
		}
	}
	if len(matched) == 0 {
		return nil
	}
	cfg, _ := config.LoadConfig()
	out := make([]docForMultiGet, 0, len(matched))
	for _, p := range matched {
		body, err := s.GetDocumentBody(p.Collection, p.Path, 0, maxLines)
		if err != nil {
			continue
		}
		title := p.Path
		if idx := strings.LastIndex(title, "/"); idx >= 0 {
			title = title[idx+1:]
		}
		ctx := ""
		if cfg != nil {
			ctx = config.FindContextForPath(cfg, p.Collection, p.Path)
		}
		skipped := false
		reason := ""
		if p.BodyLength > int64(maxBytes) {
			skipped = true
			reason = fmt.Sprintf("File too large (%d bytes > %d bytes)", p.BodyLength, maxBytes)
			body = ""
		}
		out = append(out, docForMultiGet{
			DisplayPath: p.Filepath,
			Title:       title,
			Body:        body,
			Context:     ctx,
			Skipped:     skipped,
			SkipReason:  reason,
		})
	}
	return out
}

func init() {
	multiGetCmd.Flags().IntP("l", "l", 0, "Maximum lines per file")
	multiGetCmd.Flags().Int("max-bytes", defaultMultiGetMaxBytes, "Skip files larger than N bytes")
	multiGetCmd.Flags().Bool("json", false, "JSON output")
	multiGetCmd.Flags().Bool("csv", false, "CSV output")
	multiGetCmd.Flags().Bool("md", false, "Markdown output")
	multiGetCmd.Flags().Bool("xml", false, "XML output")
	multiGetCmd.Flags().Bool("files", false, "Output file paths only")
	rootCmd.AddCommand(multiGetCmd)
}
