package main

import (
	"context"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/ba0f3/qmd-go/internal/config"
	"github.com/ba0f3/qmd-go/internal/llm"
	"github.com/ba0f3/qmd-go/internal/store"
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
	RunE:  runMCPServer,
}

func init() {
	mcpCmd.Flags().Bool("http", false, "Serve over MCP streamable HTTP instead of stdio")
	mcpCmd.Flags().String("addr", "127.0.0.1:8422", "HTTP listen address (with --http)")
	mcpCmd.Flags().String("server-config", "", "Server config YAML (with --http; default ~/.config/qmd/server.yml)")
	rootCmd.AddCommand(mcpCmd)
}

func runMCPServer(cmd *cobra.Command, args []string) error {
	if httpMode, _ := cmd.Flags().GetBool("http"); httpMode {
		addr, _ := cmd.Flags().GetString("addr")
		serverCfg, _ := cmd.Flags().GetString("server-config")
		return runMCPHTTP(addr, serverCfg)
	}

	initRoot()
	s, err := openStore()
	if err != nil {
		return err
	}
	defer s.Close()
	cfg, _ := config.LoadConfig()
	return newServer(s, cfg).Run(context.Background(), &mcp.StdioTransport{})
}

// newServer builds the MCP server (tools, resources, prompts) bound to one
// store. cfg is the collection config of that store's index and is only used
// for path-context annotations; nil is safe.
func newServer(s *store.Store, cfg *config.Config) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "qmd", Version: "1.0.0"}, nil)

	server.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: "qmd://{+path}"}, resourceHandler(s, cfg))
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
	}, searchTool(s, cfg))
	mcp.AddTool(server, &mcp.Tool{
		Name:        "vsearch",
		Description: "Semantic similarity search using vector embeddings. Requires embeddings (run 'qmd embed' first).",
	}, vsearchTool(s, cfg))
	mcp.AddTool(server, &mcp.Tool{
		Name:        "query",
		Description: "Hybrid search combining BM25 and vector search with RRF. Best quality when embeddings exist.",
	}, queryTool(s, cfg))
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get",
		Description: "Retrieve the full content of a document by its file path or docid (#abc123).",
	}, getTool(s, cfg))
	mcp.AddTool(server, &mcp.Tool{
		Name:        "multi_get",
		Description: "Retrieve multiple documents by glob pattern or comma-separated list. Skips files larger than maxBytes.",
	}, multiGetTool(s))
	mcp.AddTool(server, &mcp.Tool{
		Name:        "status",
		Description: "Show the status of the QMD index: collections and document counts.",
	}, statusTool(s))

	return server
}

func resourceHandler(s *store.Store, cfg *config.Config) mcp.ResourceHandler {
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
		body, err := s.GetDocumentBody(collection, path, 0, 0)
		if err != nil {
			all, _ := s.ListDocumentPaths()
			for _, p := range all {
				if strings.HasSuffix(p.Path, path) || p.Path == path {
					body, err = s.GetDocumentBody(p.Collection, p.Path, 0, 0)
					if err == nil {
						collection, path = p.Collection, p.Path
						break
					}
				}
			}
		}
		if err != nil || body == "" {
			return nil, mcp.ResourceNotFoundError(uri)
		}
		if cfg != nil {
			if ctxText := config.FindContextForPath(cfg, collection, path); ctxText != "" {
				body = "<!-- Context: " + ctxText + " -->\n\n" + body
			}
		}
		body = addLineNumbers(body, 1)
		return &mcp.ReadResourceResult{
			Contents: []*mcp.ResourceContents{{URI: uri, MIMEType: "text/markdown", Text: body}},
		}, nil
	}
}

type searchArgs struct {
	Query      string  `json:"query" jsonschema:"Search query - keywords or phrases to find"`
	Limit      int     `json:"limit,omitempty" jsonschema:"Maximum number of results (default 10)"`
	MinScore   float64 `json:"minScore,omitempty" jsonschema:"Minimum relevance score 0-1 (default 0)"`
	Collection string  `json:"collection,omitempty" jsonschema:"Filter to a specific collection by name"`
}

func searchTool(s *store.Store, cfg *config.Config) func(context.Context, *mcp.CallToolRequest, searchArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, args searchArgs) (*mcp.CallToolResult, any, error) {
		limit := args.Limit
		if limit <= 0 {
			limit = 10
		}
		results, err := s.SearchFTS(args.Query, limit*2, args.Collection)
		if err != nil {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Search failed: " + err.Error()}}, IsError: true}, nil, nil
		}
		var filtered []store.SearchResult
		for _, r := range results {
			if r.Score >= args.MinScore && (args.Collection == "" || r.CollectionName == args.Collection) {
				filtered = append(filtered, r)
				if len(filtered) >= limit {
					break
				}
			}
		}
		summary := formatSearchSummary(filtered, args.Query, s)
		structured := make([]map[string]any, len(filtered))
		for i, r := range filtered {
			structured[i] = map[string]any{
				"docid": "#" + docid(r.Hash), "file": r.DisplayPath, "title": r.Title,
				"score": roundScore(r.Score), "context": getContextForFile(cfg, r.Filepath), "snippet": snippet(r.Body, args.Query, 300),
			}
		}
		return &mcp.CallToolResult{
			Content:           []mcp.Content{&mcp.TextContent{Text: summary}},
			StructuredContent: map[string]any{"results": structured},
		}, nil, nil
	}
}

type vsearchArgs struct {
	Query      string  `json:"query" jsonschema:"Natural language query"`
	Limit      int     `json:"limit,omitempty" jsonschema:"Maximum number of results (default 10)"`
	MinScore   float64 `json:"minScore,omitempty" jsonschema:"Minimum relevance score 0-1 (default 0.3)"`
	Collection string  `json:"collection,omitempty" jsonschema:"Filter to a specific collection"`
}

func vsearchTool(s *store.Store, cfg *config.Config) func(context.Context, *mcp.CallToolRequest, vsearchArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, args vsearchArgs) (*mcp.CallToolResult, any, error) {
		var hasVec int
		_ = s.DB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='embedding_blobs'`).Scan(&hasVec)
		if hasVec == 0 {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "Vector index not found. Run 'qmd embed' first to create embeddings."}},
				IsError: true,
			}, nil, nil
		}
		model := os.Getenv("QMD_EMBED_MODEL")
		if model == "" {
			model = llm.DefaultEmbedModel()
		}
		client, err := llm.NewEmbedClient(model)
		if err != nil {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Embed client: " + err.Error()}}, IsError: true}, nil, nil
		}
		formatted := formatQueryForEmbedding(args.Query)
		emb, err := client.Embed(formatted)
		if err != nil {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Embedding failed: " + err.Error()}}, IsError: true}, nil, nil
		}
		limit := args.Limit
		if limit <= 0 {
			limit = 10
		}
		vecResults, err := s.SearchVectorsBrute(emb.Embedding, limit*2)
		if err != nil {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Vector search failed: " + err.Error()}}, IsError: true}, nil, nil
		}
		var filtered []store.VecSearchResult
		for _, r := range vecResults {
			if r.Score >= args.MinScore && (args.Collection == "" || strings.HasPrefix(r.Filepath, "qmd://"+args.Collection+"/")) {
				filtered = append(filtered, r)
				if len(filtered) >= limit {
					break
				}
			}
		}
		summary := formatVecSearchSummary(filtered, args.Query, s)
		structured := make([]map[string]any, len(filtered))
		for i, r := range filtered {
			structured[i] = map[string]any{
				"docid": "#" + docid(r.Hash), "file": r.DisplayPath, "title": r.Title,
				"score": roundScore(r.Score), "context": getContextForFile(cfg, r.Filepath), "snippet": snippet(r.Body, args.Query, 300),
			}
		}
		return &mcp.CallToolResult{
			Content:           []mcp.Content{&mcp.TextContent{Text: summary}},
			StructuredContent: map[string]any{"results": structured},
		}, nil, nil
	}
}

type queryArgs struct {
	Query      string  `json:"query" jsonschema:"Natural language query"`
	Limit      int     `json:"limit,omitempty" jsonschema:"Maximum number of results (default 10)"`
	MinScore   float64 `json:"minScore,omitempty" jsonschema:"Minimum relevance score 0-1"`
	Collection string  `json:"collection,omitempty" jsonschema:"Filter to a specific collection"`
}

func queryTool(s *store.Store, cfg *config.Config) func(context.Context, *mcp.CallToolRequest, queryArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, args queryArgs) (*mcp.CallToolResult, any, error) {
		limit := args.Limit
		if limit <= 0 {
			limit = 10
		}
		fetchLimit := limit * 4
		if fetchLimit < 20 {
			fetchLimit = 20
		}
		ftsResults, err := s.SearchFTS(args.Query, fetchLimit, args.Collection)
		if err != nil {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Search failed: " + err.Error()}}, IsError: true}, nil, nil
		}
		var vecResults []store.VecSearchResult
		var hasVec int
		_ = s.DB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='embedding_blobs'`).Scan(&hasVec)
		if hasVec > 0 {
			model := os.Getenv("QMD_EMBED_MODEL")
			if model == "" {
				model = llm.DefaultEmbedModel()
			}
			client, err := llm.NewEmbedClient(model)
			if err == nil {
				formatted := formatQueryForEmbedding(args.Query)
				emb, err := client.Embed(formatted)
				if err == nil {
					vecResults, _ = s.SearchVectorsBrute(emb.Embedding, fetchLimit)
				}
			}
		}
		merged := reciprocalRankFusion(ftsResults, vecResults, limit)
		var filtered []hybridResult
		for _, r := range merged {
			if r.Score >= args.MinScore {
				filtered = append(filtered, r)
			}
		}
		summary := formatHybridSummary(filtered, args.Query, s)
		structured := make([]map[string]any, len(filtered))
		for i, r := range filtered {
			structured[i] = map[string]any{
				"docid": "#" + docid(r.Hash), "file": r.DisplayPath, "title": r.Title,
				"score": roundScore(r.Score), "context": getContextForFile(cfg, r.Filepath), "snippet": snippet(r.Body, args.Query, 300),
			}
		}
		return &mcp.CallToolResult{
			Content:           []mcp.Content{&mcp.TextContent{Text: summary}},
			StructuredContent: map[string]any{"results": structured},
		}, nil, nil
	}
}

type getArgs struct {
	File        string `json:"file" jsonschema:"File path or docid (e.g. pages/meeting.md or #abc123)"`
	FromLine    int    `json:"fromLine,omitempty" jsonschema:"Start from this line number (1-indexed)"`
	MaxLines    int    `json:"maxLines,omitempty" jsonschema:"Maximum number of lines to return"`
	LineNumbers bool   `json:"lineNumbers,omitempty" jsonschema:"Add line numbers to output"`
}

func getTool(s *store.Store, cfg *config.Config) func(context.Context, *mcp.CallToolRequest, getArgs) (*mcp.CallToolResult, any, error) {
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
		collection, path := resolveInputToDoc(s, input)
		if collection == "" && path == "" {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "Document not found: " + args.File}},
				IsError: true,
			}, nil, nil
		}
		body, err := s.GetDocumentBody(collection, path, fromLine, maxLines)
		if err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "Document not found: " + args.File}},
				IsError: true,
			}, nil, nil
		}
		if cfg != nil {
			if ctxText := config.FindContextForPath(cfg, collection, path); ctxText != "" {
				body = "<!-- Context: " + ctxText + " -->\n\n" + body
			}
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

func multiGetTool(s *store.Store) func(context.Context, *mcp.CallToolRequest, multiGetArgs) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, args multiGetArgs) (*mcp.CallToolResult, any, error) {
		maxBytes := args.MaxBytes
		if maxBytes <= 0 {
			maxBytes = mcpDefaultMultiGetMax
		}
		var docs []docForMultiGet
		if strings.Contains(args.Pattern, ",") && !strings.Contains(args.Pattern, "*") && !strings.Contains(args.Pattern, "?") {
			docs = multiGetByList(s, strings.Split(args.Pattern, ","), args.MaxLines, maxBytes)
		} else {
			docs = multiGetByGlob(s, args.Pattern, args.MaxLines, maxBytes)
		}
		if len(docs) == 0 {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "No files matched pattern: " + args.Pattern}},
				IsError: true,
			}, nil, nil
		}
		var content []mcp.Content
		for _, d := range docs {
			text := d.Body
			if d.Skipped {
				text = "[SKIPPED: " + d.DisplayPath + " - " + d.SkipReason + ". Use get with file=\"" + d.DisplayPath + "\" to retrieve.]"
			} else {
				if args.LineNumbers {
					text = addLineNumbers(text, 1)
				}
				if d.Context != "" {
					text = "<!-- Context: " + d.Context + " -->\n\n" + text
				}
			}
			content = append(content, &mcp.TextContent{Text: "--- " + d.DisplayPath + " ---\n" + text})
		}
		return &mcp.CallToolResult{Content: content}, nil, nil
	}
}

func statusTool(s *store.Store) func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		st, err := s.GetStatus()
		if err != nil {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Failed to get status: " + err.Error()}}, IsError: true}, nil, nil
		}
		var needsEmbed int
		_ = s.DB.QueryRow(`SELECT COUNT(DISTINCT d.hash) FROM documents d LEFT JOIN content_vectors v ON d.hash = v.hash AND v.seq = 0 WHERE d.active = 1 AND v.hash IS NULL`).Scan(&needsEmbed)
		var n int
		_ = s.DB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='embedding_blobs'`).Scan(&n)
		hasVec := n > 0
		lines := []string{
			"QMD Index Status:",
			"  Total documents: " + strconv.Itoa(st.DocCount),
			"  Needs embedding: " + strconv.Itoa(needsEmbed),
			"  Vector index: " + strconv.FormatBool(hasVec),
			"  Collections: " + strconv.Itoa(len(st.Collections)),
		}
		for _, c := range st.Collections {
			lines = append(lines, "    - "+c.Name+" ("+strconv.Itoa(c.ActiveCount)+" docs)")
		}
		structured := map[string]any{
			"totalDocuments": st.DocCount,
			"needsEmbedding": needsEmbed,
			"hasVectorIndex": hasVec,
			"collections":    st.Collections,
		}
		return &mcp.CallToolResult{
			Content:           []mcp.Content{&mcp.TextContent{Text: strings.Join(lines, "\n")}},
			StructuredContent: structured,
		}, nil, nil
	}
}

func getContextForFile(cfg *config.Config, filepath string) string {
	col, path := parseVirtualPath(filepath)
	if col == "" {
		return ""
	}
	if cfg == nil {
		return ""
	}
	return config.FindContextForPath(cfg, col, path)
}

func formatSearchSummary(results []store.SearchResult, query string, s *store.Store) string {
	if len(results) == 0 {
		return "No results found for \"" + query + "\""
	}
	var b strings.Builder
	b.WriteString("Found ")
	b.WriteString(strconv.Itoa(len(results)))
	b.WriteString(" result(s) for \"")
	b.WriteString(query)
	b.WriteString("\":\n\n")
	for _, r := range results {
		b.WriteString("#")
		b.WriteString(docid(r.Hash))
		b.WriteString(" ")
		b.WriteString(strconv.FormatFloat(r.Score*100, 'f', 0, 64))
		b.WriteString("% ")
		b.WriteString(r.DisplayPath)
		b.WriteString(" - ")
		b.WriteString(r.Title)
		b.WriteString("\n")
	}
	return b.String()
}

func formatVecSearchSummary(results []store.VecSearchResult, query string, s *store.Store) string {
	if len(results) == 0 {
		return "No results found for \"" + query + "\""
	}
	var b strings.Builder
	b.WriteString("Found ")
	b.WriteString(strconv.Itoa(len(results)))
	b.WriteString(" result(s) for \"")
	b.WriteString(query)
	b.WriteString("\":\n\n")
	for _, r := range results {
		b.WriteString("#")
		b.WriteString(docid(r.Hash))
		b.WriteString(" ")
		b.WriteString(strconv.FormatFloat(r.Score*100, 'f', 0, 64))
		b.WriteString("% ")
		b.WriteString(r.DisplayPath)
		b.WriteString(" - ")
		b.WriteString(r.Title)
		b.WriteString("\n")
	}
	return b.String()
}

func formatHybridSummary(results []hybridResult, query string, s *store.Store) string {
	if len(results) == 0 {
		return "No results found for \"" + query + "\""
	}
	var b strings.Builder
	b.WriteString("Found ")
	b.WriteString(strconv.Itoa(len(results)))
	b.WriteString(" result(s) for \"")
	b.WriteString(query)
	b.WriteString("\":\n\n")
	for _, r := range results {
		b.WriteString("#")
		b.WriteString(docid(r.Hash))
		b.WriteString(" ")
		b.WriteString(strconv.FormatFloat(r.Score*100, 'f', 0, 64))
		b.WriteString("% ")
		b.WriteString(r.DisplayPath)
		b.WriteString(" - ")
		b.WriteString(r.Title)
		b.WriteString("\n")
	}
	return b.String()
}

func snippet(body, query string, maxLen int) string {
	if body == "" {
		return ""
	}
	if len(body) <= maxLen {
		return addLineNumbers(body, 1)
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
