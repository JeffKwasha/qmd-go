package main

import (
	"context"
	"net/url"
	"strconv"
	"strings"

	"github.com/ba0f3/qmd-go/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

const (
	mcpDefaultMultiGetMax = 10240
	qmdQueryGuideTitle    = "QMD Query Guide"
	qmdQueryGuideBody     = `# QMD - Quick Markdown Search

QMD is your on-device search engine for markdown knowledge bases. Use it to find information across your notes, documents, and meeting transcripts.

## Available Tools

### 1. search (Fast keyword search)
Best for: Finding documents with specific keywords or phrases.
- Uses BM25 full-text search
- Fast, no LLM required
- Good for exact matches
- Use ` + "`collection`" + ` parameter to filter to a specific collection

### 2. vsearch (Semantic search)
Best for: Finding conceptually related content even without exact keyword matches.
- Uses vector embeddings
- Understands meaning and context
- Good for "how do I..." or conceptual queries
- Use ` + "`collection`" + ` parameter to filter to a specific collection

### 3. query (Hybrid search - highest quality)
Best for: Important searches where you want the best results.
- Combines keyword + semantic search with RRF
- Run 'qmd embed' for vector part
- Use ` + "`collection`" + ` parameter to filter to a specific collection

### 4. get (Retrieve document)
Best for: Getting the full content of a single document you found.
- Use the file path from search results
- Supports line ranges: ` + "`file.md:100`" + ` or fromLine/maxLines parameters

### 5. multi_get (Retrieve multiple documents)
Best for: Getting content from multiple files at once.
- Use glob patterns: ` + "`journals/2025-05*.md`" + `
- Or comma-separated: ` + "`file1.md, file2.md`" + `
- Skips files over maxBytes (default 10KB) - use get for large files

### 6. status (Index info)
Shows collection info and document counts.

## Resources

You can also access documents directly via the ` + "`qmd://`" + ` URI scheme:
- Read a document: ` + "`resources/read`" + ` with uri ` + "`qmd://collection/path/to/file.md`" + `

## Search Strategy

1. **Start with search** for quick keyword lookups
2. **Use vsearch** when keywords aren't working or for conceptual queries
3. **Use query** for important searches or when you need high confidence
4. **Use get** to retrieve a single full document
5. **Use multi_get** to batch retrieve multiple related files

## Tips

- Use ` + "`minScore: 0.5`" + ` to filter low-relevance results
- Use ` + "`collection: \"notes\"`" + ` to search only in a specific collection
- File paths are relative to their collection (e.g., ` + "`pages/meeting.md`" + `)
- For glob patterns, match on display_path (e.g., ` + "`journals/2025-*.md`" + `)`
)

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Run MCP server (stdio or streamable HTTP)",
	Long:  "Start the Model Context Protocol server for QMD. Exposes search, get, and document resources. Defaults to stdio; with --http it serves multiple named indexes over MCP streamable HTTP, gated by bearer keys defined in the auth config.",
	// A config the server refuses is read in a service journal; usage
	// suggestions and a repeated error line only bury it.
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runMCPServer,
}

func init() {
	mcpCmd.Flags().Bool("http", false, "Serve over MCP streamable HTTP instead of stdio")
	mcpCmd.Flags().String("addr", "127.0.0.1:8422", "HTTP listen address (with --http)")
	mcpCmd.Flags().String("server-config", "", "Server config YAML (with --http; default <configdir>/CONFIG.yml)")
	rootCmd.AddCommand(mcpCmd)
}

func runMCPServer(cmd *cobra.Command, args []string) error {
	if httpMode, _ := cmd.Flags().GetBool("http"); httpMode {
		addr, _ := cmd.Flags().GetString("addr")
		serverCfg, _ := cmd.Flags().GetString("server-config")
		return runMCPHTTP(addr, serverCfg)
	}

	initRoot()
	sources, err := resolveSources()
	if err != nil {
		return err
	}
	defer closeSources(sources)
	return newServer(sources).Run(context.Background(), &mcp.StdioTransport{})
}

// newServer builds the MCP server (tools, resources, prompts) over one or more
// indexes. Tools merge across them by rank; see federate.go.
func newServer(sources []indexSource) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "qmd", Version: version}, nil)

	server.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: "qmd://{+path}"}, resourceHandler(sources))
	server.AddPrompt(&mcp.Prompt{
		Name:        "query",
		Description: "How to effectively search your knowledge base with QMD",
	}, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{
			Description: qmdQueryGuideTitle,
			Messages:    []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: qmdQueryGuideBody}}},
		}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "search",
		Description: "Fast keyword-based full-text search using BM25. Best for finding documents with specific words or phrases.",
	}, searchTool(sources))
	mcp.AddTool(server, &mcp.Tool{
		Name:        "vsearch",
		Description: "Semantic similarity search using vector embeddings. Requires embeddings (run 'qmd embed' first).",
	}, vsearchTool(sources))
	mcp.AddTool(server, &mcp.Tool{
		Name:        "query",
		Description: "Hybrid search combining BM25 and vector search with RRF. Best quality when embeddings exist.",
	}, queryTool(sources))
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get",
		Description: "Retrieve the full content of a document by its file path or docid (#abc123).",
	}, getTool(sources))
	mcp.AddTool(server, &mcp.Tool{
		Name:        "multi_get",
		Description: "Retrieve multiple documents by glob pattern or comma-separated list. Skips files larger than maxBytes.",
	}, multiGetTool(sources))
	mcp.AddTool(server, &mcp.Tool{
		Name:        "status",
		Description: "Show the status of the QMD index: collections and document counts.",
	}, statusTool(sources))

	return server
}

// fused reports whether more than one index answers here, which is what decides
// whether results carry an index label.
func fused(sources []indexSource) bool { return len(sources) > 1 }

func toolError(msg string) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: msg}}, IsError: true}, nil, nil
}

func resultLimit(n, def int) int {
	if n <= 0 {
		return def
	}
	return n
}

func truncateHits(hits []fusedHit, limit int) []fusedHit {
	if limit > 0 && len(hits) > limit {
		return hits[:limit]
	}
	return hits
}

// hitsResult turns merged hits into the tool answer. The index name appears only
// when several indexes contributed, so single-index answers look as they always
// did.
func hitsResult(hits []fusedHit, query string, label bool) (*mcp.CallToolResult, any, error) {
	if len(hits) == 0 {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "No results found for \"" + query + "\""}},
		}, nil, nil
	}
	var b strings.Builder
	b.WriteString("Found ")
	b.WriteString(strconv.Itoa(len(hits)))
	b.WriteString(" result(s) for \"")
	b.WriteString(query)
	b.WriteString("\":\n\n")
	structured := make([]map[string]any, len(hits))
	for i, h := range hits {
		b.WriteString("#")
		b.WriteString(docid(h.Hash))
		b.WriteString(" ")
		b.WriteString(strconv.FormatFloat(h.Score*100, 'f', 0, 64))
		b.WriteString("% ")
		if label {
			b.WriteString(h.Index)
			b.WriteString(":")
		}
		b.WriteString(h.DisplayPath)
		b.WriteString(" - ")
		b.WriteString(h.Title)
		b.WriteString("\n")
		m := map[string]any{
			"docid": "#" + docid(h.Hash), "file": h.DisplayPath, "title": h.Title,
			"score": roundScore(h.Score), "context": h.Context, "snippet": snippet(h.Body, query, 300, h.Anchor, h.Centered),
		}
		if label {
			m["index"] = h.Index
		}
		structured[i] = m
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: b.String()}},
		StructuredContent: map[string]any{"results": structured},
	}, nil, nil
}

func resourceHandler(sources []indexSource) mcp.ResourceHandler {
	return func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		uri := req.Params.URI
		if !strings.HasPrefix(uri, "qmd://") {
			return nil, mcp.ResourceNotFoundError(uri)
		}
		pathPart := strings.TrimPrefix(uri, "qmd://")
		decoded, err := url.PathUnescape(pathPart)
		if err != nil {
			decoded = pathPart
		}
		parts := strings.SplitN(decoded, "/", 2)
		if len(parts) < 2 || parts[0] == "" {
			return nil, mcp.ResourceNotFoundError(uri)
		}
		collection, path := parts[0], parts[1]
		for _, src := range sources {
			body, err := src.Store.GetDocumentBody(collection, path, 0, 0)
			if err != nil {
				all, _ := src.Store.ListDocumentPaths()
				for _, p := range all {
					if strings.HasSuffix(p.Path, path) || p.Path == path {
						body, err = src.Store.GetDocumentBody(p.Collection, p.Path, 0, 0)
						if err == nil {
							collection, path = p.Collection, p.Path
							break
						}
					}
				}
			}
			if err != nil || body == "" {
				continue
			}
			if ctxText := config.FindContextForPath(src.Cfg, collection, path); ctxText != "" {
				body = "<!-- Context: " + ctxText + " -->\n\n" + body
			}
			body = addLineNumbers(body, 1)
			return &mcp.ReadResourceResult{
				Contents: []*mcp.ResourceContents{{URI: uri, MIMEType: "text/markdown", Text: body}},
			}, nil
		}
		return nil, mcp.ResourceNotFoundError(uri)
	}
}

type searchArgs struct {
	Query      string  `json:"query" jsonschema:"Search query - keywords or phrases to find"`
	Limit      int     `json:"limit,omitempty" jsonschema:"Maximum number of results (default 10)"`
	MinScore   float64 `json:"minScore,omitempty" jsonschema:"Minimum relevance score 0-1 (default 0)"`
	Collection string  `json:"collection,omitempty" jsonschema:"Filter to a specific collection by name"`
}

func searchTool(sources []indexSource) func(context.Context, *mcp.CallToolRequest, searchArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, args searchArgs) (*mcp.CallToolResult, any, error) {
		limit := resultLimit(args.Limit, 10)
		hits, err := runFTS(sources, args.Query, limit*2, args.Collection, args.MinScore)
		if err != nil {
			return toolError("Search failed: " + err.Error())
		}
		return hitsResult(truncateHits(hits, limit), args.Query, fused(sources))
	}
}

type vsearchArgs struct {
	Query      string  `json:"query" jsonschema:"Natural language query"`
	Limit      int     `json:"limit,omitempty" jsonschema:"Maximum number of results (default 10)"`
	MinScore   float64 `json:"minScore,omitempty" jsonschema:"Minimum relevance score 0-1 (default 0.3)"`
	Collection string  `json:"collection,omitempty" jsonschema:"Filter to a specific collection"`
}

func vsearchTool(sources []indexSource) func(context.Context, *mcp.CallToolRequest, vsearchArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, args vsearchArgs) (*mcp.CallToolResult, any, error) {
		hasAny := false
		for _, src := range sources {
			if src.hasVectors() {
				hasAny = true
				break
			}
		}
		if !hasAny {
			return toolError("Vector index not found. Run 'qmd embed' first to create embeddings.")
		}
		limit := resultLimit(args.Limit, 10)
		hits, err := runVectors(sources, args.Query, limit, args.MinScore, args.Collection)
		if err != nil {
			return toolError("Vector search failed: " + err.Error())
		}
		return hitsResult(truncateHits(hits, limit), args.Query, fused(sources))
	}
}

type queryArgs struct {
	Query      string  `json:"query" jsonschema:"Natural language query"`
	Limit      int     `json:"limit,omitempty" jsonschema:"Maximum number of results (default 10)"`
	MinScore   float64 `json:"minScore,omitempty" jsonschema:"Minimum relevance score 0-1"`
	Collection string  `json:"collection,omitempty" jsonschema:"Filter to a specific collection"`
}

func queryTool(sources []indexSource) func(context.Context, *mcp.CallToolRequest, queryArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, args queryArgs) (*mcp.CallToolResult, any, error) {
		limit := resultLimit(args.Limit, 10)
		fetchLimit := limit * 4
		if fetchLimit < 20 {
			fetchLimit = 20
		}
		hits, err := runHybrid(sources, args.Query, fetchLimit, args.Collection, args.MinScore)
		if err != nil {
			return toolError("Search failed: " + err.Error())
		}
		return hitsResult(truncateHits(hits, limit), args.Query, fused(sources))
	}
}

type getArgs struct {
	File        string `json:"file" jsonschema:"File path or docid (e.g. pages/meeting.md or #abc123)"`
	FromLine    int    `json:"fromLine,omitempty" jsonschema:"Start from this line number (1-indexed)"`
	MaxLines    int    `json:"maxLines,omitempty" jsonschema:"Maximum number of lines to return"`
	LineNumbers bool   `json:"lineNumbers,omitempty" jsonschema:"Add line numbers to output"`
}

func getTool(sources []indexSource) func(context.Context, *mcp.CallToolRequest, getArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, args getArgs) (*mcp.CallToolResult, any, error) {
		input := args.File
		fromLine := args.FromLine
		maxLines := args.MaxLines
		if idx := strings.LastIndex(input, ":"); idx >= 0 && fromLine == 0 {
			if n, err := strconv.Atoi(input[idx+1:]); err == nil {
				fromLine = n
				input = input[:idx]
			}
		}
		narrowed, input := indexPrefix(sources, input)
		src, collection, path, ok := findDoc(narrowed, input)
		if !ok {
			return toolError("Document not found: " + args.File)
		}
		body, err := src.Store.GetDocumentBody(collection, path, fromLine, maxLines)
		if err != nil {
			return toolError("Document not found: " + args.File)
		}
		if ctxText := config.FindContextForPath(src.Cfg, collection, path); ctxText != "" {
			body = "<!-- Context: " + ctxText + " -->\n\n" + body
		}
		if args.LineNumbers {
			start := fromLine
			if start <= 0 {
				start = 1
			}
			body = addLineNumbers(body, start)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: body}},
		}, nil, nil
	}
}

type multiGetArgs struct {
	Pattern     string `json:"pattern" jsonschema:"Glob pattern or comma-separated list of paths"`
	MaxLines    int    `json:"maxLines,omitempty" jsonschema:"Maximum lines per file"`
	MaxBytes    int    `json:"maxBytes,omitempty" jsonschema:"Skip files larger than this (default 10240)"`
	LineNumbers bool   `json:"lineNumbers,omitempty" jsonschema:"Add line numbers"`
}

func multiGetTool(sources []indexSource) func(context.Context, *mcp.CallToolRequest, multiGetArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, args multiGetArgs) (*mcp.CallToolResult, any, error) {
		maxBytes := args.MaxBytes
		if maxBytes <= 0 {
			maxBytes = mcpDefaultMultiGetMax
		}
		var docs []docForMultiGet
		if strings.Contains(args.Pattern, ",") && !strings.Contains(args.Pattern, "*") && !strings.Contains(args.Pattern, "?") {
			docs = multiGetByList(sources, strings.Split(args.Pattern, ","), args.MaxLines, maxBytes)
		} else {
			for _, src := range sources {
				docs = append(docs, multiGetByGlob(src, args.Pattern, args.MaxLines, maxBytes)...)
			}
		}
		if !fused(sources) {
			for i := range docs {
				docs[i].Index = ""
			}
		}
		if len(docs) == 0 {
			return toolError("No files matched pattern: " + args.Pattern)
		}
		var content []mcp.Content
		for _, d := range docs {
			text := d.Body
			if d.Skipped {
				text = "[SKIPPED: " + d.label() + " - " + d.SkipReason + ". Use get with file=\"" + d.label() + "\" to retrieve.]"
			} else {
				if args.LineNumbers {
					text = addLineNumbers(text, 1)
				}
				if d.Context != "" {
					text = "<!-- Context: " + d.Context + " -->\n\n" + text
				}
			}
			content = append(content, &mcp.TextContent{Text: "--- " + d.label() + " ---\n" + text})
		}
		return &mcp.CallToolResult{Content: content}, nil, nil
	}
}

func statusTool(sources []indexSource) func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		var lines []string
		structured := make([]map[string]any, 0, len(sources))
		for _, src := range sources {
			st, err := src.Store.GetStatus()
			if err != nil {
				return toolError("Failed to get status: " + err.Error())
			}
			var needsEmbed int
			_ = src.Store.DB.QueryRow(`SELECT COUNT(DISTINCT d.hash) FROM documents d LEFT JOIN content_vectors v ON d.hash = v.hash AND v.seq = 0 WHERE d.active = 1 AND v.hash IS NULL`).Scan(&needsEmbed)
			hasVec := src.hasVectors()
			label := "QMD Index Status:"
			if fused(sources) {
				label = "QMD Index Status: " + src.Name
			}
			lines = append(lines,
				label,
				"  Total documents: "+strconv.Itoa(st.DocCount),
				"  Needs embedding: "+strconv.Itoa(needsEmbed),
				"  Vector index: "+strconv.FormatBool(hasVec),
				"  Collections: "+strconv.Itoa(len(st.Collections)),
			)
			for _, c := range st.Collections {
				lines = append(lines, "    - "+c.Name+" ("+strconv.Itoa(c.ActiveCount)+" docs)")
			}
			structured = append(structured, map[string]any{
				"index": src.Name, "totalDocuments": st.DocCount, "needsEmbedding": needsEmbed,
				"hasVectorIndex": hasVec, "collections": st.Collections,
			})
		}
		return &mcp.CallToolResult{
			Content:           []mcp.Content{&mcp.TextContent{Text: strings.Join(lines, "\n")}},
			StructuredContent: map[string]any{"indexes": structured},
		}, nil, nil
	}
}

func snippet(body, query string, maxLen, anchor int, centered bool) string {
	if body == "" {
		return ""
	}
	if len(body) <= maxLen {
		return addLineNumbers(body, 1)
	}
	if centered && anchor > 0 {
		text, startLine := centeredSnippet(body, anchor, maxLen)
		if text != "" {
			return addLineNumbers(text, startLine)
		}
	}
	lines := strings.Split(body, "\n")
	var out []string
	n := 0
	for i, line := range lines {
		if n+len(line)+1 > maxLen {
			break
		}
		out = append(out, strconv.Itoa(i+1)+": "+line)
		n += len(line) + 1
	}
	return strings.Join(out, "\n")
}
