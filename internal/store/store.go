package store

import (
	"database/sql"
	"fmt"

	"github.com/ba0f3/qmd-go/internal/config"

	_ "github.com/mattn/go-sqlite3"
)

type Store struct {
	DB     *sql.DB
	DBPath string
}

// GetDefaultDbPath resolves the SQLite path for a named index.
// Deprecated: use config.DBPathFor, which additionally honours db_path in the
// index config file (so CLI and MCP server agree on one location).
func GetDefaultDbPath(indexName string) (string, error) {
	return config.DBPathFor(indexName)
}

func NewStore(dbPath string) (*Store, error) {
	if dbPath == "" {
		var err error
		dbPath, err = GetDefaultDbPath("index")
		if err != nil {
			return nil, err
		}
	}

	// Enable WAL mode via DSN
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_foreign_keys=on", dbPath)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}

	if err := db.Ping(); err != nil {
		return nil, err
	}

	s := &Store{DB: db, DBPath: dbPath}
	if err := s.initSchema(); err != nil {
		db.Close()
		return nil, err
	}

	return s, nil
}

func (s *Store) Close() error {
	return s.DB.Close()
}

// Status holds index status for the status command.
type Status struct {
	DBPath      string
	DocCount    int
	VectorCount int
	Collections []CollectionStatus
}

// CollectionStatus is per-collection stats.
type CollectionStatus struct {
	Name         string
	ActiveCount  int
	LastModified string
}

// GetStatus returns index path, document count, vector count, and per-collection stats.
func (s *Store) GetStatus() (*Status, error) {
	st := &Status{DBPath: s.DBPath}
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM documents WHERE active = 1`).Scan(&st.DocCount); err != nil {
		return nil, err
	}
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM content_vectors`).Scan(&st.VectorCount)

	rows, err := s.DB.Query(`
		SELECT collection, COUNT(*) as cnt, MAX(modified_at) as last_modified
		FROM documents WHERE active = 1
		GROUP BY collection
		ORDER BY collection
	`)
	if err != nil {
		return st, nil
	}
	defer rows.Close()
	for rows.Next() {
		var c CollectionStatus
		var lastMod sql.NullString
		if err := rows.Scan(&c.Name, &c.ActiveCount, &lastMod); err != nil {
			continue
		}
		if lastMod.Valid {
			c.LastModified = lastMod.String
		}
		st.Collections = append(st.Collections, c)
	}
	return st, rows.Err()
}

func (s *Store) initSchema() error {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS content (
			hash TEXT PRIMARY KEY,
			doc TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS documents (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			collection TEXT NOT NULL,
			path TEXT NOT NULL,
			title TEXT NOT NULL,
			hash TEXT NOT NULL,
			created_at TEXT NOT NULL,
			modified_at TEXT NOT NULL,
			active INTEGER NOT NULL DEFAULT 1,
			FOREIGN KEY (hash) REFERENCES content(hash) ON DELETE CASCADE,
			UNIQUE(collection, path)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_documents_collection ON documents(collection, active)`,
		`CREATE INDEX IF NOT EXISTS idx_documents_hash ON documents(hash)`,
		`CREATE INDEX IF NOT EXISTS idx_documents_path ON documents(path, active)`,
		`CREATE TABLE IF NOT EXISTS llm_cache (
			hash TEXT PRIMARY KEY,
			result TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS content_vectors (
			hash TEXT NOT NULL,
			seq INTEGER NOT NULL DEFAULT 0,
			pos INTEGER NOT NULL DEFAULT 0,
			model TEXT NOT NULL,
			embedded_at TEXT NOT NULL,
			PRIMARY KEY (hash, seq)
		)`,
		// FTS5 table
		`CREATE VIRTUAL TABLE IF NOT EXISTS documents_fts USING fts5(
			filepath, title, body,
			tokenize='porter unicode61'
		)`,
		// Triggers
		`CREATE TRIGGER IF NOT EXISTS documents_ai AFTER INSERT ON documents
		WHEN new.active = 1
		BEGIN
			INSERT INTO documents_fts(rowid, filepath, title, body)
			SELECT
				new.id,
				new.collection || '/' || new.path,
				new.title,
				(SELECT doc FROM content WHERE hash = new.hash)
			WHERE new.active = 1;
		END`,
		`CREATE TRIGGER IF NOT EXISTS documents_ad AFTER DELETE ON documents BEGIN
			DELETE FROM documents_fts WHERE rowid = old.id;
		END`,
		`CREATE TRIGGER IF NOT EXISTS documents_au AFTER UPDATE ON documents
		BEGIN
			DELETE FROM documents_fts WHERE rowid = old.id AND new.active = 0;
			INSERT OR REPLACE INTO documents_fts(rowid, filepath, title, body)
			SELECT
				new.id,
				new.collection || '/' || new.path,
				new.title,
				(SELECT doc FROM content WHERE hash = new.hash)
			WHERE new.active = 1;
		END`,
	}

	for _, query := range queries {
		if _, err := s.DB.Exec(query); err != nil {
			return fmt.Errorf("schema init failed: %w (query: %s)", err, query)
		}
	}

	return nil
}
