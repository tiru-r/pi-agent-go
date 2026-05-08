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
type Index struct {
	Dir      string
	Sessions []SessionMeta
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
		for _, e := range sess.Entries {
			if e.Type == EntryMessage {
				msgCount++
				if e.Usage != nil {
					tokenCount += e.Usage.InputTokens + e.Usage.OutputTokens
				}
			}
		}

		var createdAt, updatedAt time.Time
		if len(sess.Entries) > 0 {
			createdAt = sess.Entries[0].Timestamp
			updatedAt = sess.Entries[len(sess.Entries)-1].Timestamp
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

	// Sort newest first
	sort.Slice(metas, func(i, j int) bool {
		return metas[i].UpdatedAt.After(metas[j].UpdatedAt)
	})

	idx.mu.Lock()
	idx.Sessions = metas
	idx.mu.Unlock()

	return idx.persist()
}

// Add inserts or updates a SessionMeta in the index.
func (idx *Index) Add(meta SessionMeta) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	for i, m := range idx.Sessions {
		if m.ID == meta.ID {
			idx.Sessions[i] = meta
			return
		}
	}
	idx.Sessions = append([]SessionMeta{meta}, idx.Sessions...)
}

// Remove deletes a session from the index by ID.
func (idx *Index) Remove(id string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	for i, m := range idx.Sessions {
		if m.ID == id {
			idx.Sessions = append(idx.Sessions[:i], idx.Sessions[i+1:]...)
			return
		}
	}
}

// Find looks up a session by ID.
func (idx *Index) Find(id string) (SessionMeta, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	for _, m := range idx.Sessions {
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

	if n >= len(idx.Sessions) {
		out := make([]SessionMeta, len(idx.Sessions))
		copy(out, idx.Sessions)
		return out
	}
	out := make([]SessionMeta, n)
	copy(out, idx.Sessions[:n])
	return out
}

// Search returns sessions whose title contains q (case-insensitive).
func (idx *Index) Search(q string) []SessionMeta {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	q = strings.ToLower(q)
	var results []SessionMeta
	for _, m := range idx.Sessions {
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
	idx.Sessions = disk.Sessions
	idx.mu.Unlock()
	return nil
}

// persist writes the current index to .index.json.
func (idx *Index) persist() error {
	if err := os.MkdirAll(idx.Dir, 0o755); err != nil {
		return err
	}
	idx.mu.RLock()
	disk := indexDisk{
		UpdatedAt: time.Now().UTC(),
		Sessions:  idx.Sessions,
	}
	idx.mu.RUnlock()

	data, err := json.MarshalIndent(disk, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(idx.Dir, indexFileName), data, 0o644)
}
