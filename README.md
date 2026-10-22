# QMD - Query Markup Documents

An on-device search engine for everything you need to remember. Index your markdown notes, meeting transcripts, documentation, and knowledge bases. Search with keywords or natural language. Ideal for your agentic flows.

QMD combines BM25 full-text search, vector semantic search, and LLM re-ranking—all running locally via node-llama-cpp with GGUF models.

**This project is a fork of [tobi/qmd](https://github.com/tobi/qmd)** (Go port, optional GGUF embedding backend).

## Quick Start

```sh
# Install (requires Go 1.23+)
go install -tags fts5 github.com/ba0f3/qmd-go
# Or from repo:
# git clone https://github.com/ba0f3/qmd-go && cd qmd && go build -tags fts5 -o qmd-go ./cmd/qmd

# Create collections for your notes, docs, and meeting transcripts
qmd collection add ~/notes --name notes
qmd collection add ~/Documents/meetings --name meetings
qmd collection add ~/work/docs --name docs

# Add context to help with search results
qmd context add qmd://notes "Personal notes and ideas"
qmd context add qmd://meetings "Meeting transcripts and notes"
qmd context add qmd://docs "Work documentation"

# Generate embeddings for semantic search
qmd embed

# Search across everything
qmd search "project timeline"           # Fast keyword search
qmd vsearch "how to deploy"             # Semantic search
qmd query "quarterly planning process"  # Hybrid + reranking (best quality)

# Get a specific document
qmd get "meetings/2024-01-15.md"

# Get a document by docid (shown in search results)
qmd get "#abc123"

# Get multiple documents by glob pattern
qmd multi-get "journals/2025-05*.md"

# Search within a specific collection
qmd search "API" -c notes

# Export all matches for an agent
qmd search "API" --all --files --min-score 0.3
```

### Using with AI Agents

QMD's `--json` and `--files` output formats are designed for agentic workflows:

```sh
# Get structured results for an LLM
qmd search "authentication" --json -n 10

# List all relevant files above a threshold
qmd query "error handling" --all --files --min-score 0.4

# Retrieve full document content
qmd get "docs/api-reference.md" --full
```

### MCP Server

QMD exposes an MCP (Model Context Protocol) server for integration with Claude, Cursor, and other AI tools.

**Tools exposed:**
- `search` – Fast BM25 keyword search (supports collection filter)
- `vsearch` – Semantic vector search (supports collection filter)
- `query` – Hybrid search (BM25 + vector, RRF)
- `get` – Retrieve document by path or docid
- `multi_get` – Retrieve multiple documents by glob or list
- `status` – Index health and collection info

**Resources:** Documents are readable via `qmd://` URIs (e.g. `qmd://collection/path/to/file.md`).

**Shared HTTP server (one process, many clients)**

`qmd mcp` defaults to stdio — one child process per client. With `--http` it serves
several named indexes over MCP streamable HTTP, so a single process can back every
agent on the machine:

```bash
qmd mcp --http --addr 127.0.0.1:8422     # config: ~/.config/qmd/server.yml
```

`server.yml` maps URL paths to named indexes and gates access with bearer keys:

```yaml
servers:
  /mcp: index              # conventional endpoint (same index as /mcp/shared)
  /mcp/shared: index       # project notes, docs
  /mcp/personal: personal  # private index

auth:
  public: [/mcp, /mcp/shared]   # reachable without a key
  groups:
    personal:
      keys: ["<token>"]         # generate: openssl rand -hex 24
      allow: [/mcp/personal, /mcp, /mcp/shared]
```

No key reaches public paths only; a valid key reaches the paths its group allows.
Unknown paths return 404, denied requests 401 (`WWW-Authenticate: Bearer`). Keys are
compared in constant time and logged only as `sha256:<8>`. `GET /healthz` reports
liveness. Point any streamable-HTTP MCP client at
`http://127.0.0.1:8422/mcp`; add `Authorization: Bearer <token>` for private indexes.

**Claude Desktop** (`~/Library/Application Support/Claude/claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "qmd": {
      "command": "qmd",
      "args": ["mcp"]
    }
  }
}
```

**Claude Code** — Install the plugin (recommended):

```bash
claude marketplace add tobi/qmd
claude plugin add qmd@qmd
```

Or configure MCP manually in `~/.claude/settings.json`:

```json
{
  "mcpServers": {
    "qmd": {
      "command": "qmd",
      "args": ["mcp"]
    }
  }
}
```

## Architecture

- **SQLite FTS5** – Full-text search (BM25)
- **Embeddings** – Stored in SQLite; generated via Ollama, any OpenAI-compatible API, or local GGUF (purego, no CGO)
- **Hybrid search** – `query` combines BM25 and vector results using Reciprocal Rank Fusion (RRF)
- **Chunking** – 800 tokens per chunk, 15% overlap (character-based in Go)
- **Index** – `db_path` in `~/.config/qmd/<index>.yml`, else `QMD_DB_DIR/<index>.sqlite`, else `INDEX_PATH`, else `~/.cache/qmd/index.sqlite`

```
Query ──┬──► BM25 (FTS5) ──► Ranked list
        │
        └──► Embed query ──► Vector similarity ──► Ranked list
                                    │
                                    ▼
                            RRF fusion ──► Final results
```

## Requirements

- **Go** 1.23 or later (for building)
- **SQLite** with FTS5 (e.g. `github.com/mattn/go-sqlite3` – enable via build tag `fts5`)
- **Embeddings**: [Ollama](https://ollama.ai) (default), any OpenAI-compatible API, or local GGUF via **purego** (no CGO; build `libllama_go` with `make deps-purego` and `make build-purego`)

### Build tags

| Tag   | Purpose |
|-------|---------|
| `fts5` | **Use for normal builds.** Enables SQLite FTS5 full-text search. Without it, `qmd search` and related features will not work. |
| `gguf` | Optional. Enables local GGUF embedding models (no Ollama required). **Preferred: purego (no CGO)** — build the shared library with `make deps-purego` and `make build-purego`, then `make build-gguf`. Fallback: CGO with [go-llama.cpp](https://github.com/go-skynet/go-llama.cpp) via `make deps-gguf` then `make build-gguf`. |

**Examples:** `make build` (outputs `qmd-go`) or `go build -tags fts5 -o qmd-go ./cmd/qmd`. For GGUF embeddings: `make build-gguf`.

## Installation

**From source (recommended)**

```sh
git clone https://github.com/tobi/qmd
cd qmd
make build
make install
# Installs to /usr/local/bin/qmd; ensure /usr/local/bin is in your PATH
```

To install to a different prefix (e.g. your user directory):

```sh
make install PREFIX=$HOME/.local
# Then add $HOME/.local/bin to your PATH
```

**Using Go install**

```sh
go install -tags fts5 github.com/ba0f3/qmd-go@latest
# Binary is installed as qmd-go in $GOPATH/bin or $HOME/go/bin; add that to your PATH
```

### Development

```sh
git clone https://github.com/tobi/qmd
cd qmd
./qmd --help          # Runs via go run
make build && ./qmd-go status
go test ./...
```

### Testing the GGUF build

1. **Build with GGUF** (purego, no CGO — recommended):

   ```sh
   make deps-purego  # once: clone llama.cpp into llama-go/
   make build-purego # build libllama_go (shared library)
   make build-gguf   # produces qmd-go (loads lib at runtime)
   ```

   Or with CGO (go-llama.cpp) as fallback:

   ```sh
   make deps-gguf    # once: clone go-llama.cpp, build libbinding.a
   make build-gguf   # produces qmd-go
   ```

2. **Run unit tests** (including GGUF default/backend logic):

   ```sh
   go test -tags fts5 ./internal/llm/...
   go test -tags gguf,fts5 ./internal/llm/...   # with GGUF tag
   ```

3. **Manual embed test** (first run downloads Nomic Embed v1.5 Q8_0, ~146MB, to `~/.cache/qmd/models`):

   ```sh
   ./qmd-go collection add . --name test
   ./qmd-go update
   ./qmd-go embed    # uses local GGUF by default; no Ollama needed
   ./qmd-go vsearch "some query"
   ```

4. **Force API backend** with the GGUF binary (e.g. to use Ollama instead):

   ```sh
   QMD_EMBED_BACKEND=api ./qmd-go embed
   ```

## Usage

### Collection Management

```sh
# Create a collection from current directory
qmd collection add . --name myproject

# Create a collection with explicit path and custom glob mask
qmd collection add ~/Documents/notes --name notes --mask "**/*.md"

# List all collections
qmd collection list

# Remove a collection
qmd collection remove myproject

# Rename a collection
qmd collection rename myproject my-project

# List files in a collection
qmd ls notes
qmd ls notes/subfolder
```

### Generate Vector Embeddings

```sh
# Embed all indexed documents (Ollama or OpenAI-compatible API)
qmd embed

# Force re-embed everything
qmd embed -f
```

Set `QMD_EMBED_MODEL` (default: `nomic-embed-text` for Ollama) or use an OpenAI-compatible endpoint.

### Context Management

Context adds descriptive metadata to collections and paths, helping search understand your content.

```sh
# Add context to a collection (using qmd:// virtual paths)
qmd context add qmd://notes "Personal notes and ideas"
qmd context add qmd://docs/api "API documentation"

# Add context from within a collection directory
cd ~/notes && qmd context add "Personal notes and ideas"

# Add global context (applies to all collections)
qmd context add / "Knowledge base for my projects"

# List all contexts
qmd context list

# Remove context
qmd context rm qmd://notes/old
```

### Search Commands

| Command   | Description                                      |
|----------|--------------------------------------------------|
| `search` | BM25 full-text search only                       |
| `vsearch` | Vector semantic search only                    |
| `query`  | Hybrid: BM25 + vector, RRF (no LLM reranker)     |

```sh
# Full-text search (fast, keyword-based)
qmd search "authentication flow"

# Vector search (semantic similarity)
qmd vsearch "how to login"

# Hybrid search (best quality without external reranker)
qmd query "user authentication"
```

### Options

```sh
# Search options
-n <num>           # Number of results (default: 5, or 20 for --files/--json)
-c, --collection   # Restrict search to a specific collection
--all              # Return all matches (use with --min-score to filter)
--min-score <num>  # Minimum score threshold (default: 0)
--full             # Show full document content
--line-numbers     # Add line numbers to output
--index <name>     # Use named index (default: index)

# Output formats (for search and multi-get)
--files            # Output: docid,score,filepath,context
--json             # JSON output with snippets
--csv              # CSV output
--md               # Markdown output
--xml              # XML output

# Get options
qmd get <file>[:line]  # Get document, optionally starting at line
-l <num>               # Maximum lines to return
--from <num>           # Start from line number

# Multi-get options
-l <num>           # Maximum lines per file
--max-bytes <num>  # Skip files larger than N bytes (default: 10KB)
```

### Index Maintenance

```sh
# Show index status and collections
qmd status

# Re-index all collections
qmd update

# Re-index with git pull first (for remote repos)
qmd update --pull

# Get document by filepath (with docid fallback)
qmd get notes/meeting.md

# Get document by docid (from search results)
qmd get "#abc123"

# Get document starting at line 50, max 100 lines
qmd get notes/meeting.md:50 -l 100

# Get multiple documents by glob pattern
qmd multi-get "journals/2025-05*.md"

# Get multiple documents by comma-separated list (supports docids)
qmd multi-get "doc1.md, doc2.md, #abc123"

# Limit multi-get to files under 20KB
qmd multi-get "docs/*.md" --max-bytes 20480

# Output multi-get as JSON for agent processing
qmd multi-get "docs/*.md" --json

# Clean up cache and orphaned data
qmd cleanup
```

## Data Storage

Index stored in: `~/.cache/qmd/index.sqlite` (or `INDEX_PATH`; use `--index <name>` for `~/.cache/qmd/<name>.sqlite`).

### Schema (overview)

- **documents** – Paths, titles, content hash, collection, active flag
- **content** – Full document text (keyed by hash)
- **documents_fts** – FTS5 full-text index
- **content_vectors** / **embedding_blobs** – Chunk embeddings for vector search
- Config (collections, context) – YAML in `~/.config/qmd/index.yml` (or per `--index`)

## Embedding backends

**Default (no build tag):** Embeddings use Ollama or any OpenAI-compatible API. Default model: `nomic-embed-text`. Set `OLLAMA_HOST` and optionally `QMD_EMBED_MODEL`.

**GGUF build (`-tags gguf`):** When built with GGUF support, the default is a local GGUF embedding model (Nomic Embed Text v1.5). go-llama.cpp uses the **ggerganov/llama.cpp** submodule by default, which may lack support for embedding architectures like `nomic-bert`. To use local GGUF embeddings **without Ollama**, switch the submodule to **ggml-org/llama.cpp** (has nomic_bert and other embed architectures):

```sh
make deps-gguf              # once
make update-llama-cpp-ggml  # point llama.cpp submodule at ggml-org, rebuild libbinding.a
make build-gguf
```

If you prefer to keep using the API (Ollama/OpenAI) for embeddings even with a GGUF build, set:

```sh
QMD_EMBED_BACKEND=api ./qmd embed
```

To build with GGUF support, qmd supports two methods. **Purego (no CGO) is recommended**: no C compiler or CGO required for the Go binary; only the shared library is built with CMake. At runtime the binary tries purego first, then falls back to CGO if the library is not found.

   **Method 1: Purego (no CGO)** — recommended; no CGO for the Go build; cross-compile the Go binary and build the shared library on the target (or ship the lib with the binary):
   ```sh
   make deps-purego  # clone llama.cpp into llama-go/ (once)
   make build-purego # build shared library (libllama_go.so / .dll / .dylib in llama-go/build/)
   make build-gguf   # build qmd with -tags gguf,fts5 (loads lib at runtime via purego)
   ```
   Set `LLAMA_GO_LIB` to the path of the shared library if it is not in the default location (`llama-go/build/` next to the repo).

   **Method 2: CGO (go-llama.cpp)** — fallback if the purego library is not available:
   ```sh
   make deps-gguf   # clone go-llama.cpp + submodules, build libbinding.a (once)
   make build-gguf  # add replace in go.mod and build qmd with -tags gguf,fts5
   ```
   The CGO method compiles go-llama.cpp's binding into the binary; CGO and a C/C++ toolchain are required.

2. **GGUF models (Tobi's setup)** — the reference TypeScript/Bun qmd uses three local GGUF models (auto-downloaded from Hugging Face into `~/.cache/qmd/models/`):

   | Model | Purpose | Size | Hugging Face spec |
   |-------|---------|------|-------------------|
   | embeddinggemma-300M-Q8_0 | Vector embeddings | ~300 MB | `ggml-org/embeddinggemma-300M-GGUF:embeddinggemma-300M-Q8_0.gguf` |
   | qwen3-reranker-0.6b-q8_0 | Re-ranking | ~640 MB | `ggml-org/Qwen3-Reranker-0.6B-Q8_0-GGUF` (reranker not yet in Go build) |
   | qmd-query-expansion-1.7B-q4_k_m | Query expansion (fine-tuned) | ~1.1 GB | `tobil/qmd-query-expansion-1.7B-gguf:qmd-query-expansion-1.7B-q4_k_m.gguf` (query expansion not yet in Go build) |

   The **Go port** currently uses only the **embedding** model (default: EmbeddingGemma). Reranker and query expansion are not yet implemented in the Go CLI.

3. **Model spec** (optional; GGUF build default: EmbeddingGemma 300M) for `QMD_EMBED_MODEL` when using GGUF:
   - **Default (Tobi-aligned):** `ggml-org/embeddinggemma-300M-GGUF:embeddinggemma-300M-Q8_0.gguf`
   - **Local path:** `/path/to/model-Q8_0.gguf`
   - **Nomic (BERT-based):** `nomic-ai/nomic-embed-text-v1.5-GGUF:nomic-embed-text-v1.5.Q8_0.gguf`

   The first time you use a Hugging Face spec, the file is downloaded to `~/.cache/qmd/models` (or `QMD_MODEL_CACHE`). EmbeddingGemma uses the `gemma-embedding` architecture; use `make update-llama-cpp-ggml` so the llama.cpp submodule supports it (and nomic_bert if you switch to Nomic).

**Troubleshooting:** If you get "failed to load model" or "unknown model architecture: 'nomic-bert'" (or similar):

- **Try the default build first:** The go-llama.cpp submodule may already support your model. Run `make deps-gguf && make build-gguf` and test.
- **Update llama.cpp (may fail):** Run `make update-llama-cpp` to use a newer ggerganov/llama.cpp. If that fails with compilation errors, go-llama.cpp's binding.cpp is incompatible with that version.
- **Use ggml-org (experimental, may fail):** Run `make update-llama-cpp-ggml` to switch to ggml-org/llama.cpp. **Warning:** This often fails due to API incompatibilities (`load_binding_model` or `llama_binding_state` errors). If it fails, use the API backend instead.
- **Use the API backend:** Run Ollama, `ollama pull nomic-embed-text`, then `QMD_EMBED_BACKEND=api ./qmd embed`.

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `XDG_CACHE_HOME` | `~/.cache` | Cache directory (index SQLite) |
| `INDEX_PATH` | (derived) | Override index DB path |
| `OLLAMA_HOST` | `http://localhost:11434/v1` | Ollama API base for embed (API backend only) |
| `QMD_EMBED_MODEL` | API: `nomic-embed-text`; GGUF build: EmbeddingGemma 300M (HF) | Embedding model name or GGUF path/spec |
| `QMD_EMBED_BACKEND` | GGUF build: `gguf`; API build: (none) | `gguf` = use local GGUF; `api` = use Ollama/OpenAI (e.g. when using gguf binary but want API) |
| `QMD_MODEL_CACHE` | `~/.cache/qmd/models` | Directory for downloaded GGUF models |
| `LLAMA_GO_LIB` | (auto-detected) | Path to `libllama_go.so` / `llama_go.dll` / `libllama_go.dylib` (purego method) |

For OpenAI-compatible APIs, set your provider’s base URL and API key (e.g. `OPENAI_API_BASE`, `OPENAI_API_KEY`); the Go CLI uses the same env names as typical OpenAI clients where applicable.

## License

MIT
