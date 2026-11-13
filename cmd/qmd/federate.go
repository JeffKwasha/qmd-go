package main

// Several indexes may answer one request. Their scores are not comparable:
// BM25 depends on the corpus each index holds, and cosine similarity depends on
// the model each index was embedded with. Merging therefore uses ranks, not
// scores — reciprocal rank fusion — and every result carries the index it came
// from, so the caller never has to ask each database and compare numbers that
// mean different things.
//
// A single index keeps its own scores: fusion only happens when there are two
// or more lists to fuse.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"

	"github.com/ba0f3/qmd-go/internal/config"
	"github.com/ba0f3/qmd-go/internal/llm"
	"github.com/ba0f3/qmd-go/internal/store"
)

const rrfK = 60

// modelMismatch is a configuration error, not a search miss: the index's vectors
// came from a model other than the one its file now names, so comparing them
// with a query embedded by the current model is meaningless (0 on a dimension
// change, plausible garbage when dimensions agree).
type modelMismatch struct {
	Index      string
	Stored     string
	Source     string
	Configured string
}

func (e *modelMismatch) Error() string {
	return fmt.Sprintf("index %q holds embeddings from model %q while %s gives %q; run qmd embed --force to rebuild",
		e.Index, e.Stored, e.Source, e.Configured)
}

// checkModel refuses a read from an index whose vectors do not match its
// configured model. Embedding refuses the same way; a query must not answer
// "nothing found" when the truth is "this index cannot be compared".
func checkModel(src indexSource) error {
	model := config.EmbedModel(src.Cfg)
	stored, err := src.Store.EmbeddingModels()
	if err != nil {
		return err
	}
	for _, m := range stored {
		if m != model {
			return &modelMismatch{Index: src.Name, Stored: m, Source: config.EmbedModelSource(src.Cfg), Configured: model}
		}
	}
	return nil
}

type indexSource struct {
	Name  string
	Store *store.Store
	Cfg   *config.Config
}

// fusedHit is one document as it appears in a merged answer.
type fusedHit struct {
	Index       string
	Collection  string
	Filepath    string
	DisplayPath string
	Title       string
	Body        string
	Hash        string
	Context     string
	Score       float64
}

// resolveSources answers what a read covers. An explicit --index means that
// index and no other. Otherwise the conventional single index applies when it
// exists, and when it does not, every index in the config directory is read:
// asking which database to search is the caller's problem only when the answer
// depends on access, and here it never does.
func resolveSources() ([]indexSource, error) {
	name := getIndexName()
	var names []string
	if rootCmd.PersistentFlags().Changed("index") || config.HasIndexConfig(name) {
		names = []string{name}
	} else {
		all, err := config.IndexNames()
		if err != nil {
			return nil, err
		}
		names = all
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no indexes in %s: add one with qmd collection add", mustConfigDir())
	}
	return openSources(names)
}

// namedSources opens exactly the indexes a mount names, in the order given.
func namedSources(names []string) ([]indexSource, error) {
	if len(names) == 0 {
		names = []string{"index"}
	}
	return openSources(names)
}

func openSources(names []string) ([]indexSource, error) {
	out := make([]indexSource, 0, len(names))
	for _, name := range names {
		cfg, err := config.LoadIndexConfig(name)
		if err != nil {
			closeSources(out)
			return nil, err
		}
		dbPath, err := config.DBPathFor(name)
		if err != nil {
			closeSources(out)
			return nil, err
		}
		if _, err := os.Stat(dbPath); errors.Is(err, fs.ErrNotExist) {
			if len(names) == 1 {
				closeSources(out)
				return nil, fmt.Errorf("index %q (%s): not built; run qmd update", name, dbPath)
			}
			fmt.Fprintf(os.Stderr, "qmd: index %q is not built; run qmd update\n", name)
			continue
		} else if err != nil {
			closeSources(out)
			return nil, fmt.Errorf("index %q (%s): %w", name, dbPath, err)
		}
		s, err := store.NewStore(dbPath)
		if err != nil {
			closeSources(out)
			return nil, fmt.Errorf("index %q (%s): %w", name, dbPath, err)
		}
		out = append(out, indexSource{Name: name, Store: s, Cfg: cfg})
	}
	return out, nil
}

func closeSources(sources []indexSource) {
	for _, src := range sources {
		if src.Store != nil {
			_ = src.Store.Close()
		}
	}
}

func mustConfigDir() string {
	dir, err := config.GetConfigDir()
	if err != nil {
		return "the config directory"
	}
	return dir
}

// fuseLists merges ranked lists with reciprocal rank fusion. The top result
// scores 1.0 and the rest are relative to it; that is a ranking, not a
// confidence, because no cross-index score exists to report. One list is
// returned untouched, so single-index results keep their BM25 or cosine scores.
func fuseLists(lists [][]fusedHit) []fusedHit {
	if len(lists) == 0 {
		return nil
	}
	if len(lists) == 1 {
		return lists[0]
	}
	order := make([]string, 0)
	byKey := make(map[string]*fusedHit)
	sums := make(map[string]float64)
	for _, list := range lists {
		for rank, h := range list {
			key := h.Index + "\x00" + h.Filepath
			if _, ok := byKey[key]; !ok {
				copied := h
				byKey[key] = &copied
				order = append(order, key)
			}
			sums[key] += 1.0 / float64(rrfK+rank+1)
		}
	}
	var max float64
	for _, key := range order {
		if sums[key] > max {
			max = sums[key]
		}
	}
	out := make([]fusedHit, 0, len(order))
	for _, key := range order {
		h := *byKey[key]
		h.Score = 0
		if max > 0 {
			h.Score = sums[key] / max
		}
		out = append(out, h)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

func contextFor(cfg *config.Config, collection, displayPath string) string {
	if cfg == nil {
		return ""
	}
	path := displayPath
	if i := strings.Index(path, "/"); i >= 0 {
		path = path[i+1:]
	}
	return config.FindContextForPath(cfg, collection, path)
}

func ftsHit(src indexSource, r store.SearchResult) fusedHit {
	return fusedHit{
		Index:       src.Name,
		Collection:  r.CollectionName,
		Filepath:    r.Filepath,
		DisplayPath: r.DisplayPath,
		Title:       r.Title,
		Body:        r.Body,
		Hash:        r.Hash,
		Context:     contextFor(src.Cfg, r.CollectionName, r.DisplayPath),
		Score:       r.Score,
	}
}

func vecHit(src indexSource, r store.VecSearchResult) fusedHit {
	collection := ""
	if i := strings.Index(r.DisplayPath, "/"); i >= 0 {
		collection = r.DisplayPath[:i]
	}
	return fusedHit{
		Index:       src.Name,
		Collection:  collection,
		Filepath:    r.Filepath,
		DisplayPath: r.DisplayPath,
		Title:       r.Title,
		Body:        r.Body,
		Hash:        r.Hash,
		Context:     contextFor(src.Cfg, collection, r.DisplayPath),
		Score:       r.Score,
	}
}

// runFTS searches every source's full text and fuses the lists.
func runFTS(sources []indexSource, query string, limit int, collection string, minScore float64) ([]fusedHit, error) {
	var lists [][]fusedHit
	var errs []error
	for _, src := range sources {
		results, err := src.Store.SearchFTS(query, limit, collection)
		if err != nil {
			errs = append(errs, fmt.Errorf("index %q: %w", src.Name, err))
			continue
		}
		list := make([]fusedHit, 0, len(results))
		for _, r := range results {
			if r.Score < minScore {
				continue
			}
			list = append(list, ftsHit(src, r))
		}
		lists = append(lists, list)
	}
	if len(lists) == 0 && len(errs) > 0 {
		return nil, errs[0]
	}
	for _, err := range errs {
		fmt.Fprintf(os.Stderr, "qmd: %v\n", err)
	}
	return fuseLists(lists), nil
}

// runVectors embeds the query once per index, with that index's own model, and
// fuses the lists. Two indexes may well use different models; the ranks still
// compare, the scores would not.
func runVectors(sources []indexSource, query string, limit int, minScore float64, collection string) ([]fusedHit, error) {
	var lists [][]fusedHit
	var errs []error
	for _, src := range sources {
		if !src.hasVectors() {
			continue
		}
		if err := checkModel(src); err != nil {
			return nil, err
		}
		client, err := llm.NewEmbedClient(config.EmbedModel(src.Cfg))
		if err != nil {
			errs = append(errs, fmt.Errorf("index %q: %w", src.Name, err))
			continue
		}
		emb, err := client.Embed(formatQueryForEmbedding(query))
		if err != nil {
			errs = append(errs, fmt.Errorf("index %q: %w", src.Name, err))
			continue
		}
		results, err := src.Store.SearchVectorsBrute(emb.Embedding, limit, collection)
		if err != nil {
			errs = append(errs, fmt.Errorf("index %q: %w", src.Name, err))
			continue
		}
		// One document may contribute several chunks; the best-placed chunk is
		// the document's rank, so the same file cannot fill the answer or add
		// its rank twice in the fusion.
		seen := make(map[string]bool, len(results))
		list := make([]fusedHit, 0, len(results))
		for _, r := range results {
			if r.Score < minScore || seen[r.Filepath] {
				continue
			}
			if collection != "" && !strings.HasPrefix(r.DisplayPath, collection+"/") {
				continue
			}
			seen[r.Filepath] = true
			list = append(list, vecHit(src, r))
		}
		lists = append(lists, list)
	}
	if len(lists) == 0 && len(errs) > 0 {
		return nil, errs[0]
	}
	for _, err := range errs {
		fmt.Fprintf(os.Stderr, "qmd: %v\n", err)
	}
	return fuseLists(lists), nil
}

// runHybrid fuses full text and vectors inside each index, then fuses the
// indexes. Nesting the fusions is what keeps a keyword-only or vector-only
// index useful in a federated answer.
func runHybrid(sources []indexSource, query string, limit int, collection string, minScore float64) ([]fusedHit, error) {
	var lists [][]fusedHit
	var errs []error
	answered := false
	for _, src := range sources {
		if src.hasVectors() {
			if err := checkModel(src); err != nil {
				return nil, err
			}
		}
		one := []indexSource{src}
		fts, ftsErr := runFTS(one, query, limit, collection, minScore)
		vec, vecErr := runVectors(one, query, limit, minScore, collection)
		// An index that produced a list answered, even if that list is empty:
		// "no match here" is an answer, and must not be reported as a failure.
		if ftsErr == nil || vecErr == nil {
			answered = true
		}
		if ftsErr != nil {
			errs = append(errs, ftsErr)
		}
		if vecErr != nil {
			errs = append(errs, vecErr)
		}
		lists = append(lists, fuseLists([][]fusedHit{fts, vec}))
	}
	out := fuseLists(lists)
	if len(out) == 0 && len(errs) > 0 && !answered {
		return nil, errs[0]
	}
	for _, err := range errs {
		fmt.Fprintf(os.Stderr, "qmd: %v\n", err)
	}
	return out, nil
}

// rowFromHit is the shape every output format consumes. label adds the index
// name, which only makes sense when more than one index answered.
func rowFromHit(h fusedHit, full, label bool) SearchOutputRow {
	body := h.Body
	if !full && len(body) > 500 {
		body = body[:500] + "..."
	}
	index := ""
	if label {
		index = h.Index
	}
	return SearchOutputRow{
		Docid:    docid(h.Hash),
		Filepath: h.Filepath,
		Title:    h.Title,
		Body:     body,
		Score:    h.Score,
		Context:  h.Context,
		Full:     full,
		Index:    index,
	}
}

// hasVectors reports whether this index has ever been embedded.
func (src indexSource) hasVectors() bool {
	var n int
	_ = src.Store.DB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='embedding_blobs'`).Scan(&n)
	return n > 0
}

// indexPrefix accepts the label the search output prints (`public:qmd://...`)
// and narrows the search to that index. Without it, a path printed by a
// federated search could not be fetched at all.
func indexPrefix(sources []indexSource, input string) ([]indexSource, string) {
	i := strings.Index(input, ":")
	if i <= 0 {
		return sources, input
	}
	name := input[:i]
	for _, src := range sources {
		if src.Name == name {
			return []indexSource{src}, input[i+1:]
		}
	}
	return sources, input
}

// findDoc resolves a path or docid across sources, first match wins.
func findDoc(sources []indexSource, input string) (indexSource, string, string, bool) {
	for _, src := range sources {
		collection, path := resolveInputToDoc(src.Store, input)
		if collection == "" || path == "" {
			continue
		}
		// resolveInputToDoc parses a qmd:// or collection/path spelling without
		// asking this index whether it holds the file, so a federated lookup
		// must check before accepting the first parse as a match.
		if !src.Store.DocumentExists(collection, path) {
			continue
		}
		return src, collection, path, true
	}
	return indexSource{}, "", "", false
}
