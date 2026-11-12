package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ba0f3/qmd-go/internal/config"
	"github.com/ba0f3/qmd-go/internal/llm"
	"github.com/ba0f3/qmd-go/internal/store"
	"github.com/spf13/cobra"
)

func formatDocForEmbedding(text, title string) string {
	if title == "" {
		title = "none"
	}
	return "title: " + title + " | text: " + text
}

func extractTitle(body string) string {
	lines := strings.Split(strings.TrimSpace(body), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			return strings.TrimSpace(strings.TrimLeft(line, "#"))
		}
		return line
	}
	return ""
}

var embedCmd = &cobra.Command{
	Use:           "embed",
	Short:         "Generate vector embeddings, in every index unless --index names one",
	Long:          "Generate vector embeddings for indexed documents. One index holds one model's vectors; a different configured model is refused until --force rebuilds them.",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		names, err := indexTargets(cmd.Flag("index").Changed, getIndexName())
		if err != nil {
			return err
		}
		force, _ := cmd.Flags().GetBool("force")
		var errs []error
		for _, name := range names {
			if err := embedIndex(name, force); err != nil {
				errs = append(errs, fmt.Errorf("index %q: %w", name, err))
			}
		}
		return errors.Join(errs...)
	},
}

func embedIndex(name string, force bool) error {
	model := os.Getenv("QMD_EMBED_MODEL")
	if model == "" {
		model = llm.DefaultEmbedModel()
	}

	config.CurrentIndexName = name
	dbPath, err := config.DBPathFor(name)
	if err != nil {
		return err
	}
	s, err := store.NewStore(dbPath)
	if err != nil {
		return err
	}
	defer s.Close()

	if err := s.EnsureEmbeddingBlobTable(); err != nil {
		return err
	}
	if force {
		fmt.Printf("Index %q: clearing all vectors\n", name)
		if err := s.ClearAllEmbeddings(); err != nil {
			return err
		}
	} else {
		models, err := s.EmbeddingModels()
		if err != nil {
			return err
		}
		for _, stored := range models {
			if stored != model {
				return fmt.Errorf("holds embeddings from model %q while the configured model is %q; run qmd embed --force to rebuild", stored, model)
			}
		}
	}

	hashes, err := s.GetHashesForEmbedding()
	if err != nil {
		return err
	}
	if len(hashes) == 0 {
		fmt.Printf("Index %q: nothing to embed\n", name)
		return nil
	}

	backend := os.Getenv("QMD_EMBED_BACKEND")
	if backend == "" {
		if llm.GGUFEnabled() {
			backend = "gguf (default)"
		} else {
			backend = "api (default)"
		}
	}
	client, err := llm.NewEmbedClient(model)
	if err != nil {
		return err
	}

	var totalChunks int
	for _, h := range hashes {
		totalChunks += len(store.ChunkDocument(h.Body, store.ChunkSizeChars, store.ChunkOverlapChars))
	}
	fmt.Printf("Index %q: embedding %d documents (%d chunks), model: %s, backend: %s\n",
		name, len(hashes), totalChunks, model, backend)

	embedded := 0
	failures := 0
	now := time.Now()

	for i, h := range hashes {
		docTitle := extractTitle(h.Body)
		if docTitle == "" {
			parts := strings.Split(h.Path, "/")
			if len(parts) > 0 {
				docTitle = parts[len(parts)-1]
			}
		}
		for seq, ch := range store.ChunkDocument(h.Body, store.ChunkSizeChars, store.ChunkOverlapChars) {
			formatted := formatDocForEmbedding(ch.Text, docTitle)
			result, err := client.Embed(formatted)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error embedding %s chunk %d: %v\n", h.Path, seq, err)
				failures++
				continue
			}
			if err := s.InsertEmbedding(h.Hash, seq, ch.Pos, result.Embedding, model, now); err != nil {
				fmt.Fprintf(os.Stderr, "Error inserting embedding: %v\n", err)
				failures++
				continue
			}
			embedded++
		}
		fmt.Fprintf(os.Stderr, "\rEmbedded %d/%d documents (%d chunks)...", i+1, len(hashes), embedded)
	}
	fmt.Fprintln(os.Stderr)
	fmt.Printf("Done. Embedded %d chunks from %d documents in %.1fs\n", embedded, len(hashes), time.Since(now).Seconds())
	if failures > 0 {
		return fmt.Errorf("%d chunk(s) failed to embed", failures)
	}
	return nil
}

func init() {
	embedCmd.Flags().BoolP("force", "f", false, "Clear this index's vectors and rebuild them with the configured model")
	rootCmd.AddCommand(embedCmd)
}
