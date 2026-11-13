package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"strings"
	"time"
)

func HashContent(content string) string {
	hash := sha256.Sum256([]byte(content))
	return hex.EncodeToString(hash[:])
}

func (s *Store) InsertContent(hash, content string, createdAt time.Time) error {
	_, err := s.DB.Exec(`INSERT OR IGNORE INTO content (hash, doc, created_at) VALUES (?, ?, ?)`,
		hash, content, createdAt.Format(time.RFC3339))
	return err
}

func (s *Store) InsertDocument(collection, path, title, hash string, createdAt, modifiedAt time.Time) error {
	_, err := s.DB.Exec(`
		INSERT INTO documents (collection, path, title, hash, created_at, modified_at, active)
		VALUES (?, ?, ?, ?, ?, ?, 1)
	`, collection, path, title, hash, createdAt.Format(time.RFC3339), modifiedAt.Format(time.RFC3339))
	return err
}

type Document struct {
	ID         int64
	Collection string
	Path       string
	Title      string
	Hash       string
	CreatedAt  time.Time
	ModifiedAt time.Time
	Active     bool
}

func (s *Store) FindActiveDocument(collection, path string) (*Document, error) {
	row := s.DB.QueryRow(`
		SELECT id, collection, path, title, hash, created_at, modified_at, active
		FROM documents
		WHERE collection = ? AND path = ? AND active = 1
	`, collection, path)

	var doc Document
	var active int
	var createdAt, modifiedAt string

	if err := row.Scan(&doc.ID, &doc.Collection, &doc.Path, &doc.Title, &doc.Hash, &createdAt, &modifiedAt, &active); err != nil {
		return nil, err
	}
	doc.Active = active == 1
	doc.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	doc.ModifiedAt, _ = time.Parse(time.RFC3339, modifiedAt)

	return &doc, nil
}

func (s *Store) UpdateDocument(id int64, title, hash string, modifiedAt time.Time) error {
	_, err := s.DB.Exec(`UPDATE documents SET title = ?, hash = ?, modified_at = ? WHERE id = ?`,
		title, hash, modifiedAt.Format(time.RFC3339), id)
	return err
}

func (s *Store) UpdateDocumentTitle(id int64, title string, modifiedAt time.Time) error {
	_, err := s.DB.Exec(`UPDATE documents SET title = ?, modified_at = ? WHERE id = ?`,
		title, modifiedAt.Format(time.RFC3339), id)
	return err
}

func (s *Store) DeactivateDocument(collection, path string) error {
	_, err := s.DB.Exec(`UPDATE documents SET active = 0 WHERE collection = ? AND path = ? AND active = 1`,
		collection, path)
	return err
}

func (s *Store) GetActiveDocumentPaths(collection string) ([]string, error) {
	rows, err := s.DB.Query(`SELECT path FROM documents WHERE collection = ? AND active = 1`, collection)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, nil
}

func (s *Store) CleanupOrphanedContent() (int64, error) {
	res, err := s.DB.Exec(`
		DELETE FROM content
		WHERE hash NOT IN (SELECT DISTINCT hash FROM documents WHERE active = 1)
	`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DocPath is a document path entry for listing or glob matching.
type DocPath struct {
	Filepath    string
	DisplayPath string
	BodyLength  int64
	Collection  string
	Path        string
}

// GetDocumentBody returns the content body for a document by collection and path.
// fromLine is 1-based; maxLines limits output lines (0 = all).
// DocumentExists answers whether this index holds the document, which is what a
// federated lookup needs: a path can be parsed without being present here.
func (s *Store) DocumentExists(collection, path string) bool {
	var one int
	err := s.DB.QueryRow(`
		SELECT 1 FROM documents d WHERE d.collection = ? AND d.path = ? AND d.active = 1 LIMIT 1
	`, collection, path).Scan(&one)
	return err == nil
}

func (s *Store) GetDocumentBody(collection, path string, fromLine, maxLines int) (string, error) {
	var body string
	err := s.DB.QueryRow(`
		SELECT content.doc
		FROM documents d
		JOIN content ON content.hash = d.hash
		WHERE d.collection = ? AND d.path = ? AND d.active = 1
	`, collection, path).Scan(&body)
	if err != nil {
		return "", err
	}
	if fromLine > 0 || maxLines > 0 {
		body = sliceLines(body, fromLine, maxLines)
	}
	return body, nil
}

func sliceLines(text string, fromLine, maxLines int) string {
	lines := strings.Split(text, "\n")
	if fromLine > 0 {
		if fromLine > len(lines) {
			return ""
		}
		lines = lines[fromLine-1:]
	}
	if maxLines > 0 && len(lines) > maxLines {
		lines = lines[:maxLines]
	}
	return strings.Join(lines, "\n")
}

// FindByDocid finds a document by short docid (first 6 chars of hash).
// Returns collection, path, and full hash. Empty strings if not found.
func (s *Store) FindByDocid(docid string) (collection, path, hash string, err error) {
	docid = strings.TrimSpace(docid)
	docid = strings.TrimPrefix(docid, "#")
	if len(docid) == 0 {
		return "", "", "", nil
	}
	err = s.DB.QueryRow(`
		SELECT d.collection, d.path, d.hash
		FROM documents d
		WHERE d.hash LIKE ? AND d.active = 1
		LIMIT 1
	`, docid+"%").Scan(&collection, &path, &hash)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", "", "", nil
		}
		return "", "", "", err
	}
	return collection, path, hash, nil
}

// ListDocumentPaths returns all active documents with filepath, display path, and body length.
func (s *Store) ListDocumentPaths() ([]DocPath, error) {
	rows, err := s.DB.Query(`
		SELECT
			'qmd://' || d.collection || '/' || d.path AS filepath,
			d.collection || '/' || d.path AS display_path,
			LENGTH(content.doc) AS body_length,
			d.collection,
			d.path
		FROM documents d
		JOIN content ON content.hash = d.hash
		WHERE d.active = 1
		ORDER BY d.collection, d.path
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DocPath
	for rows.Next() {
		var d DocPath
		if err := rows.Scan(&d.Filepath, &d.DisplayPath, &d.BodyLength, &d.Collection, &d.Path); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetDocumentByVirtualPath returns body for a document identified by virtual path (qmd://collection/path).
func (s *Store) GetDocumentByVirtualPath(virtualPath string) (collection, path, body string, err error) {
	if !strings.HasPrefix(virtualPath, "qmd://") {
		return "", "", "", nil
	}
	rest := strings.TrimPrefix(virtualPath, "qmd://")
	idx := strings.Index(rest, "/")
	if idx < 0 {
		return "", "", "", nil
	}
	collection = rest[:idx]
	path = rest[idx+1:]
	var b string
	err = s.DB.QueryRow(`
		SELECT content.doc
		FROM documents d
		JOIN content ON content.hash = d.hash
		WHERE d.collection = ? AND d.path = ? AND d.active = 1
	`, collection, path).Scan(&b)
	if err != nil {
		return "", "", "", err
	}
	return collection, path, b, nil
}
