package session

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// SQLiteStore is a SQLite-backed session index and entry store.
type SQLiteStore struct {
	db *sql.DB
}

// SessionMeta holds lightweight summary information about a session.
type SessionMeta struct {
	ID           string
	Title        string
	Model        string
	Provider     string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	MessageCount int
	TokenCount   int
	Path         string
}

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
    id           TEXT PRIMARY KEY,
    title        TEXT,
    model        TEXT,
    provider     TEXT,
    created_at   DATETIME,
    updated_at   DATETIME,
    message_count INTEGER DEFAULT 0,
    token_count   INTEGER DEFAULT 0,
    path         TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS session_entries (
    id         TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id),
    parent_id  TEXT,
    type       TEXT NOT NULL,
    timestamp  DATETIME,
    data       JSON NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_entries_session ON session_entries(session_id, timestamp);
`

// NewSQLiteStore opens (or creates) a SQLite database at path and applies the schema.
func NewSQLiteStore(path string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("sqlite: open: %w", err)
	}
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite: apply schema: %w", err)
	}
	return &SQLiteStore{db: db}, nil
}

// Close closes the underlying database.
func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

// SaveSession upserts the session header and inserts any new entries.
// Existing entries are skipped (they are immutable once written).
func (s *SQLiteStore) SaveSession(sess *Session) error {
	sess.mu.Lock()
	entries := make([]Entry, len(sess.entries))
	copy(entries, sess.entries)
	id := sess.ID
	path := sess.Path
	sess.mu.Unlock()

	title := sess.Title()

	var msgCount, tokenCount int
	for _, e := range entries {
		if e.Type == EntryMessage {
			msgCount++
			if e.Usage != nil {
				tokenCount += e.Usage.InputTokens + e.Usage.OutputTokens
			}
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("sqlite: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()

	_, err = tx.Exec(`
		INSERT INTO sessions (id, title, model, provider, created_at, updated_at, message_count, token_count, path)
		VALUES (?, ?, '', '', ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		    title=excluded.title,
		    updated_at=excluded.updated_at,
		    message_count=excluded.message_count,
		    token_count=excluded.token_count,
		    path=excluded.path`,
		id, title, now, now, msgCount, tokenCount, path)
	if err != nil {
		return fmt.Errorf("sqlite: upsert session: %w", err)
	}

	// INSERT OR IGNORE: entries are immutable once written, so we skip duplicates
	// rather than re-writing unchanged data on every save.
	for _, e := range entries {
		data, err := json.Marshal(e)
		if err != nil {
			continue
		}
		_, err = tx.Exec(`
			INSERT OR IGNORE INTO session_entries (id, session_id, parent_id, type, timestamp, data)
			VALUES (?, ?, ?, ?, ?, ?)`,
			e.ID, id, nullStr(e.ParentID), string(e.Type), e.Timestamp.UTC(), string(data))
		if err != nil {
			return fmt.Errorf("sqlite: insert entry %s: %w", e.ID, err)
		}
	}

	return tx.Commit()
}

// LoadSession loads a session from SQLite by ID and opens its JSONL file for appending.
func (s *SQLiteStore) LoadSession(id string) (*Session, error) {
	var path string
	err := s.db.QueryRow(`SELECT path FROM sessions WHERE id=?`, id).Scan(&path)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("sqlite: session %s not found", id)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: query session: %w", err)
	}

	entries, err := s.QueryEntries(id)
	if err != nil {
		return nil, err
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open session file for append: %w", err)
	}

	return &Session{
		ID:      id,
		Path:    path,
		entries: entries,
		headID:  leafID(entries),
		file:    f,
	}, nil
}

// QueryEntries returns all entries for the given session ID ordered by timestamp.
func (s *SQLiteStore) QueryEntries(sessionID string) ([]Entry, error) {
	rows, err := s.db.Query(`
		SELECT data FROM session_entries
		WHERE session_id=?
		ORDER BY timestamp ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: query entries: %w", err)
	}
	defer rows.Close()

	var entries []Entry
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			continue
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate entries: %w", err)
	}
	return entries, nil
}

// ListSessions returns metadata for all sessions, newest first.
func (s *SQLiteStore) ListSessions() ([]SessionMeta, error) {
	rows, err := s.db.Query(`
		SELECT id, title, model, provider, created_at, updated_at, message_count, token_count, path
		FROM sessions
		ORDER BY updated_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list sessions: %w", err)
	}
	defer rows.Close()
	return scanSessionMetas(rows)
}

// DeleteSession removes a session and all its entries.
func (s *SQLiteStore) DeleteSession(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("sqlite: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`DELETE FROM session_entries WHERE session_id=?`, id); err != nil {
		return fmt.Errorf("sqlite: delete entries: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM sessions WHERE id=?`, id); err != nil {
		return fmt.Errorf("sqlite: delete session: %w", err)
	}
	return tx.Commit()
}

// Search performs a case-insensitive title search.
// The query string is escaped so that '%' and '_' are treated as literals.
func (s *SQLiteStore) Search(query string) ([]SessionMeta, error) {
	pattern := "%" + escapeLike(query) + "%"
	rows, err := s.db.Query(`
		SELECT id, title, model, provider, created_at, updated_at, message_count, token_count, path
		FROM sessions
		WHERE title LIKE ? ESCAPE '\' COLLATE NOCASE
		ORDER BY updated_at DESC`, pattern)
	if err != nil {
		return nil, fmt.Errorf("sqlite: search: %w", err)
	}
	defer rows.Close()
	return scanSessionMetas(rows)
}

// escapeLike escapes special LIKE characters (\, %, _) for use with ESCAPE '\'.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// UpdateSessionMeta updates the model and provider for a session.
func (s *SQLiteStore) UpdateSessionMeta(id, model, provider string) error {
	_, err := s.db.Exec(
		`UPDATE sessions SET model=?, provider=?, updated_at=? WHERE id=?`,
		model, provider, time.Now().UTC(), id)
	return err
}

// sqliteTimeFormats lists candidate datetime formats SQLite may return.
var sqliteTimeFormats = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05",
}

// parseSQLiteTime tries multiple formats to parse a datetime string from SQLite.
func parseSQLiteTime(s string) time.Time {
	for _, layout := range sqliteTimeFormats {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// scanSessionMetas scans rows into []SessionMeta.
func scanSessionMetas(rows *sql.Rows) ([]SessionMeta, error) {
	var metas []SessionMeta
	for rows.Next() {
		var m SessionMeta
		var createdAt, updatedAt string
		if err := rows.Scan(&m.ID, &m.Title, &m.Model, &m.Provider,
			&createdAt, &updatedAt, &m.MessageCount, &m.TokenCount, &m.Path); err != nil {
			continue
		}
		m.CreatedAt = parseSQLiteTime(createdAt)
		m.UpdatedAt = parseSQLiteTime(updatedAt)
		metas = append(metas, m)
	}
	return metas, rows.Err()
}

// nullStr returns nil for empty strings (for nullable SQL columns).
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
