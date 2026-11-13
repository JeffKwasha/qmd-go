package store

import (
	"encoding/binary"
	"math"
	"time"
)

// GetHashesForEmbedding returns all content hashes from active documents that do not yet have embeddings.
func (s *Store) GetHashesForEmbedding() ([]struct{ Hash, Body, Path string }, error) {
	rows, err := s.DB.Query(`
		SELECT d.hash, c.doc AS body, MIN(d.path) AS path
		FROM documents d
		JOIN content c ON d.hash = c.hash
		LEFT JOIN content_vectors v ON d.hash = v.hash AND v.seq = 0
		WHERE d.active = 1 AND v.hash IS NULL
		GROUP BY d.hash
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []struct{ Hash, Body, Path string }
	for rows.Next() {
		var h, body, path string
		if err := rows.Scan(&h, &body, &path); err != nil {
			return nil, err
		}
		out = append(out, struct{ Hash, Body, Path string }{h, body, path})
	}
	return out, rows.Err()
}

// GetHashesNeedingEmbeddingCount returns the number of unique content hashes that need embedding.
func (s *Store) GetHashesNeedingEmbeddingCount() (int, error) {
	var n int
	err := s.DB.QueryRow(`
		SELECT COUNT(DISTINCT d.hash)
		FROM documents d
		LEFT JOIN content_vectors v ON d.hash = v.hash AND v.seq = 0
		WHERE d.active = 1 AND v.hash IS NULL
	`).Scan(&n)
	return n, err
}

// EnsureEmbeddingBlobTable creates the embedding_blobs table if it does not exist.
// We use a BLOB table so Go can store/retrieve vectors without sqlite-vec.
func (s *Store) EnsureEmbeddingBlobTable() error {
	_, err := s.DB.Exec(`
		CREATE TABLE IF NOT EXISTS embedding_blobs (
			hash_seq TEXT PRIMARY KEY,
			embedding BLOB NOT NULL
		)
	`)
	return err
}

// InsertEmbedding inserts one embedding into content_vectors and embedding_blobs.
func (s *Store) InsertEmbedding(hash string, seq, pos int, embedding []float32, model string, embeddedAt time.Time) error {
	hashSeq := hash + "_" + itoa(seq)
	blob := float32SliceToBlob(embedding)
	_, err := s.DB.Exec(`
		INSERT OR REPLACE INTO content_vectors (hash, seq, pos, model, embedded_at)
		VALUES (?, ?, ?, ?, ?)
	`, hash, seq, pos, model, embeddedAt.Format(time.RFC3339))
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(`
		INSERT OR REPLACE INTO embedding_blobs (hash_seq, embedding) VALUES (?, ?)
	`, hashSeq, blob)
	return err
}

func itoa(i int) string {
	if i <= 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}

func float32SliceToBlob(f []float32) []byte {
	b := make([]byte, 4*len(f))
	for i, v := range f {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(v))
	}
	return b
}

// BlobToFloat32Slice decodes a BLOB back to float32 slice (for vsearch).
func BlobToFloat32Slice(b []byte) []float32 {
	n := len(b) / 4
	out := make([]float32, n)
	for i := 0; i < n; i++ {
		out[i] = float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out
}

func float32frombits(x uint32) float32 {
	return math.Float32frombits(x)
}

// ClearAllEmbeddings removes all rows from content_vectors and embedding_blobs (force re-embed).
func (s *Store) ClearAllEmbeddings() error {
	if _, err := s.DB.Exec(`DELETE FROM content_vectors`); err != nil {
		return err
	}
	_, err := s.DB.Exec(`DELETE FROM embedding_blobs`)
	return err
}

// VecSearchResult is one vector search hit.
type VecSearchResult struct {
	Filepath    string
	DisplayPath string
	Title       string
	Body        string
	Score       float64
	Hash        string
}

// SearchVectorsBrute does brute-force cosine similarity search over embedding_blobs.
// queryEmbedding must be the same dimension as stored embeddings. Returns results sorted by score descending.
func (s *Store) SearchVectorsBrute(queryEmbedding []float32, limit int, collection string) ([]VecSearchResult, error) {
	// The collection filter belongs in the query: the caller's limit must count
	// rows of that collection, not rows that survive afterwards.
	query := `
		SELECT eb.hash_seq, eb.embedding,
			'qmd://' || d.collection || '/' || d.path AS filepath,
			d.collection || '/' || d.path AS display_path,
			d.title, content.doc AS body, d.hash
		FROM embedding_blobs eb
		JOIN content_vectors cv ON cv.hash || '_' || cv.seq = eb.hash_seq
		JOIN documents d ON d.hash = cv.hash AND d.active = 1
		JOIN content ON content.hash = d.hash`
	var args []any
	if collection != "" {
		query += ` WHERE d.collection = ?`
		args = append(args, collection)
	}
	rows, err := s.DB.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type row struct {
		HashSeq     string
		Embedding   []byte
		Filepath    string
		DisplayPath string
		Title       string
		Body        string
		Hash        string
	}
	var rowsList []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.HashSeq, &r.Embedding, &r.Filepath, &r.DisplayPath, &r.Title, &r.Body, &r.Hash); err != nil {
			return nil, err
		}
		rowsList = append(rowsList, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Cosine similarity with each, then sort and limit
	type scored struct {
		r     row
		score float64
	}
	scores := make([]scored, 0, len(rowsList))
	for _, r := range rowsList {
		vec := BlobToFloat32Slice(r.Embedding)
		sim := cosineSimilarity(queryEmbedding, vec)
		scores = append(scores, scored{r: r, score: sim})
	}
	// Sort descending by score
	for i := 0; i < len(scores); i++ {
		for j := i + 1; j < len(scores); j++ {
			if scores[j].score > scores[i].score {
				scores[i], scores[j] = scores[j], scores[i]
			}
		}
	}
	if limit <= 0 || limit > len(scores) {
		limit = len(scores)
	}
	out := make([]VecSearchResult, 0, limit)
	for i := 0; i < limit && i < len(scores); i++ {
		s := scores[i]
		out = append(out, VecSearchResult{
			Filepath:    s.r.Filepath,
			DisplayPath: s.r.DisplayPath,
			Title:       s.r.Title,
			Body:        s.r.Body,
			Score:       s.score,
			Hash:        s.r.Hash,
		})
	}
	return out, nil
}

func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

// EmbeddingModels lists the distinct models recorded for stored vectors, sorted.
// A non-empty result that does not match the configured model means the index
// holds vectors from another model, which cannot be compared by dimension.
func (s *Store) EmbeddingModels() ([]string, error) {
	rows, err := s.DB.Query(`SELECT DISTINCT model FROM content_vectors WHERE model <> '' ORDER BY model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var models []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		models = append(models, m)
	}
	return models, rows.Err()
}
