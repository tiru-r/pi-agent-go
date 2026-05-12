package session

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const indexFileName = ".index.json"

// Index is an in-memory session index that can be persisted to disk.
// All exported methods are safe for concurrent use.
type Index struct {
	Dir string

	sessions []SessionMeta // guarded by mu
	mu       sync.RWMutex
}

// indexDisk is the on-disk format for the index file.
type indexDisk struct {
	UpdatedAt time.Time     `json:"updated_at"`
	Sessions  []SessionMeta `json:"sessions"`
}

// NewIndex creates an Index for dir, loading from the persisted index file
// (fast path) or scanning the directory (slow path if stale/missing).
func NewIndex(dir string) (*Index, error) {
	idx := &Index{Dir: dir}
	if err := idx.load(); err != nil {
		// Fall back to a full scan on any load error.
		if err2 := idx.Refresh(); err2 != nil {
			return nil, err2
		}
	}
	return idx, nil
}

// Refresh re-scans the session directory and updates the index.
func (idx *Index) Refresh() error {
	var metas []SessionMeta

	err := filepath.WalkDir(idx.Dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && path != idx.Dir {
			return filepath.SkipDir
		}
		if !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		sess, err := Open(path)
		if err != nil {
			return nil
		}
		_ = sess.Close()

		info, _ := d.Info()
		var msgCount, tokenCount int
		for _, e := range sess.entries {
			if e.Type == EntryMessage {
				msgCount++
				if e.Usage != nil {
					tokenCount += e.Usage.InputTokens + e.Usage.OutputTokens
				}
			}
		}

		var createdAt, updatedAt time.Time
		if len(sess.entries) > 0 {
			createdAt = sess.entries[0].Timestamp
			updatedAt = sess.entries[len(sess.entries)-1].Timestamp
		} else if info != nil {
			createdAt = info.ModTime()
			updatedAt = info.ModTime()
		}

		metas = append(metas, SessionMeta{
			ID:           sess.ID,
			Title:        sess.Title(),
			Path:         path,
			MessageCount: msgCount,
			TokenCount:   tokenCount,
			CreatedAt:    createdAt,
			UpdatedAt:    updatedAt,
		})
		return nil
	})
	if err != nil {
		return err
	}

	sort.Slice(metas, func(i, j int) bool {
		return metas[i].UpdatedAt.After(metas[j].UpdatedAt)
	})

	idx.mu.Lock()
	idx.sessions = metas
	idx.mu.Unlock()

	return idx.persist()
}

// Add inserts or updates a SessionMeta in the index and persists to disk.
func (idx *Index) Add(meta SessionMeta) error {
	idx.mu.Lock()
	found := false
	for i, m := range idx.sessions {
		if m.ID == meta.ID {
			idx.sessions[i] = meta
			found = true
			break
		}
	}
	if !found {
		idx.sessions = append([]SessionMeta{meta}, idx.sessions...)
	}
	idx.mu.Unlock()
	return idx.persist()
}

// Remove deletes a session from the index by ID and persists to disk.
func (idx *Index) Remove(id string) error {
	idx.mu.Lock()
	for i, m := range idx.sessions {
		if m.ID == id {
			idx.sessions = append(idx.sessions[:i], idx.sessions[i+1:]...)
			break
		}
	}
	idx.mu.Unlock()
	return idx.persist()
}

// Find looks up a session by ID.
func (idx *Index) Find(id string) (SessionMeta, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	for _, m := range idx.sessions {
		if m.ID == id {
			return m, true
		}
	}
	return SessionMeta{}, false
}

// Recent returns the n most recently updated sessions.
func (idx *Index) Recent(n int) []SessionMeta {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	src := idx.sessions
	if n < len(src) {
		src = src[:n]
	}
	out := make([]SessionMeta, len(src))
	copy(out, src)
	return out
}

// Search returns sessions whose title contains q (case-insensitive).
func (idx *Index) Search(q string) []SessionMeta {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	q = strings.ToLower(q)
	var results []SessionMeta
	for _, m := range idx.sessions {
		if strings.Contains(strings.ToLower(m.Title), q) {
			results = append(results, m)
		}
	}
	return results
}

// load reads the index from the persisted .index.json file.
func (idx *Index) load() error {
	path := filepath.Join(idx.Dir, indexFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var disk indexDisk
	if err := json.Unmarshal(data, &disk); err != nil {
		return err
	}
	idx.mu.Lock()
	idx.sessions = disk.Sessions
	idx.mu.Unlock()
	return nil
}

// persist writes the current index to .index.json atomically (snapshot under
// RLock, then write — concurrent callers each write a valid full state).
func (idx *Index) persist() error {
	if err := os.MkdirAll(idx.Dir, 0o755); err != nil {
		return err
	}
	idx.mu.RLock()
	sessions := make([]SessionMeta, len(idx.sessions))
	copy(sessions, idx.sessions)
	idx.mu.RUnlock()

	data, err := json.Marshal(indexDisk{
		UpdatedAt: time.Now().UTC(),
		Sessions:  sessions,
	})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(idx.Dir, indexFileName), data, 0o644)
}
