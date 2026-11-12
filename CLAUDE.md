# QMD - Query Markup Documents

QMD is implemented in **Go**. Use the Go CLI and avoid modifying the SQLite index directly.

## Working in this checkout

This repository is shared with an agent account in group `claude`. The tree and
`.git` must stay group-writable, with a default ACL so new files stay editable
by the owner:

```sh
chmod -R g+rwX . && setfacl -R -d -m g::rwx .
```

If a write fails, stop and ask for that command. Do not clone the repository
elsewhere, work under `/tmp`, or report a commit that is not in this
repository's history. Keep agent work in a registered worktree:

```sh
git worktree add .worktrees/<name> -b <branch>
git worktree remove .worktrees/<name>
```

## Commands

```sh
qmd collection add . --name <n>   # Create/index collection
qmd collection list               # List all collections with details
qmd collection remove <name>      # Remove a collection by name
qmd collection rename <old> <new> # Rename a collection
qmd ls [collection[/path]]        # List collections or files in a collection
qmd context add [path] "text"     # Add context for path (defaults to current dir)
qmd context list                  # List all contexts
qmd context check                 # Check for collections/paths missing context
qmd context rm <path>             # Remove context
qmd get <file>                    # Get document by path or docid (#abc123)
qmd multi-get <pattern>           # Get multiple docs by glob or comma-separated list
qmd status                        # Show index status and collections
qmd update [--pull] [--full]     # Re-index every index; --index <name> for one (--pull: git pull first)
qmd embed                         # Generate vector embeddings (Ollama or OpenAI-compatible API)
qmd search <query>                # BM25 full-text search
qmd vsearch <query>               # Vector similarity search
qmd query <query>                 # Hybrid search (BM25 + vector, RRF)
qmd mcp                           # Run MCP server (stdio)
qmd cleanup                       # Remove cache, orphaned data, vacuum DB
```

## Collection Management

```sh
# List all collections
qmd collection list

# Create a collection with explicit name
qmd collection add ~/Documents/notes --name mynotes --mask '**/*.md'

# Remove a collection
qmd collection remove mynotes

# Rename a collection
qmd collection rename mynotes my-notes

# List all files in a collection
qmd ls mynotes

# List files with a path prefix
qmd ls journals/2025
qmd ls qmd://journals/2025
```

## Context Management

```sh
# Add context to current directory (auto-detects collection)
qmd context add "Description of these files"

# Add context to a specific path
qmd context add /subfolder "Description for subfolder"

# Add global context to all collections (system message)
qmd context add / "Always include this context"

# Add context using virtual paths
qmd context add qmd://journals/ "Context for entire journals collection"
qmd context add qmd://journals/2024 "Journal entries from 2024"

# List all contexts
qmd context list

# Check for collections or paths without context
qmd context check

# Remove context
qmd context rm qmd://journals/2024
qmd context rm /  # Remove global context
```

## Document IDs (docid)

Each document has a unique short ID (docid) - the first 6 characters of its content hash.
Docids are shown in search results as `#abc123` and can be used with `get` and `multi-get`:

```sh
# Search returns docid in results
qmd search "query" --json
# Output: [{"docid": "#abc123", "score": 0.85, "file": "docs/readme.md", ...}]

# Get document by docid
qmd get "#abc123"
qmd get abc123  # Leading # is optional

# Docids also work in multi-get comma-separated lists
qmd multi-get "#abc123, #def456"
```

## Options

```sh
# Search & retrieval
-c, --collection <name>  # Restrict search to a collection (matches pwd suffix)
-n <num>                 # Number of results
--all                    # Return all matches
--min-score <num>        # Minimum score threshold
--full                   # Show full document content
--line-numbers           # Add line numbers to output

# Multi-get specific
-l <num>                 # Maximum lines per file
--max-bytes <num>        # Skip files larger than this (default 10KB)

# Output formats (search and multi-get)
--json, --csv, --md, --xml, --files
```

## Development

```sh
go build -o qmd ./cmd/qmd   # Build binary
./qmd --help                   # From repo: script runs go run or qmd
go test ./...
```

## Architecture

- SQLite FTS5 for full-text search (BM25)
- Embeddings stored in SQLite (embedding_blobs); generated via Ollama/OpenAI-compatible API or GGUF (build with -tags gguf; Hugging Face auto-download in internal/huggingface)
- Reciprocal Rank Fusion (RRF) for combining BM25 and vector results
- Token-based chunking: 800 tokens/chunk with 15% overlap (character-based in Go)

## Important: Do NOT run automatically

- Never run `qmd collection add`, `qmd embed`, or `qmd update` automatically
- Never modify the SQLite database directly
- Write out example commands for the user to run manually
- Index is stored at `~/.cache/qmd/index.sqlite`
