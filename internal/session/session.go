// Package session provides JSONL-based session persistence (format version 3).
package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tiru-r/pi-agent-go/internal/model"
)

// SessionVersion is the current JSONL file format version.
const SessionVersion = 3

// EntryType tags each JSONL entry.
type EntryType string

const (
	EntryMessage       EntryType = "message"
	EntryModelChange   EntryType = "model_change"
	EntryThinkingLevel EntryType = "thinking_level"
	EntryCompaction    EntryType = "compaction"
	EntryMetadata      EntryType = "metadata"
)

// Entry is a single line in the JSONL session file.
type Entry struct {
	Type      EntryType      `json:"type"`
	ID        string         `json:"id"`
	ParentID  string         `json:"parent_id,omitempty"`
	Timestamp time.Time      `json:"timestamp"`
	Message   *model.Message `json:"message,omitempty"`
	Model     string         `json:"model,omitempty"`
	Level     string         `json:"level,omitempty"`   // thinking level
	Summary   string         `json:"summary,omitempty"` // compaction summary
	Usage     *model.Usage   `json:"usage,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// versionHeader is the first line written to every new session file.
type versionHeader struct {
	Version int `json:"version"`
}

// Session is a single conversation stored as a JSONL file.
// Entries form a tree via ParentID; headID tracks the active branch tip.
//
// Session is safe for concurrent use. All exported methods acquire the
// internal mutex; callers must not hold it externally.
type Session struct {
	ID   string
	Path string

	entries []Entry // guarded by mu
	headID  string  // ID of the most recent entry on the active branch; guarded by mu

	mu   sync.Mutex
	file *os.File
}

// New creates a new session in dir with a freshly-generated UUID.
func New(dir string) (*Session, error) {
	return NewWithID(dir, uuid.New().String())
}

// NewWithID creates a new session in dir using the given ID.
func NewWithID(dir, id string) (*Session, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("session: create dir: %w", err)
	}
	path := filepath.Join(dir, id+".jsonl")

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, fmt.Errorf("session: create file: %w", err)
	}

	s := &Session{ID: id, Path: path, file: f}

	hdr, _ := json.Marshal(versionHeader{Version: SessionVersion})
	if _, err := f.Write(append(hdr, '\n')); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("session: write version header: %w", err)
	}

	// Write initial metadata entry; its ID becomes the chain root.
	meta := Entry{
		Type:      EntryMetadata,
		ID:        uuid.New().String(),
		Timestamp: time.Now().UTC(),
		Metadata:  map[string]any{"session_id": id},
	}
	if err := s.appendEntry(meta); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("session: write metadata entry: %w", err)
	}

	return s, nil
}

// Open loads an existing session from a JSONL file and opens it for appending.
func Open(path string) (*Session, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("session: read file: %w", err)
	}

	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) == 0 {
		return nil, fmt.Errorf("session: file is empty")
	}

	// First line may be a version header — skip it.
	start := 0
	var hdr versionHeader
	if err := json.Unmarshal([]byte(lines[0]), &hdr); err == nil && hdr.Version > 0 {
		start = 1
	}

	var entries []Entry
	for _, line := range lines[start:] {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue // skip malformed lines
		}
		entries = append(entries, e)
	}

	// Extract session ID from metadata entry or filename.
	id := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	for _, e := range entries {
		if e.Type == EntryMetadata {
			if sid, ok := e.Metadata["session_id"].(string); ok && sid != "" {
				id = sid
			}
			break
		}
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("session: open for append: %w", err)
	}

	return &Session{
		ID:      id,
		Path:    path,
		entries: entries,
		headID:  leafID(entries),
		file:    f,
	}, nil
}

// computeLeafSet returns a set of entry IDs that are referenced as a
// parent by at least one other entry. Entries not in this set are leaves.
func computeLeafSet(entries []Entry) map[string]bool {
	parents := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.ParentID != "" {
			parents[e.ParentID] = true
		}
	}
	return parents
}

// leafID returns the ID of the most-recently-timestamped leaf entry.
// A leaf is any entry that no other entry lists as its ParentID.
// For legacy sessions where all ParentIDs are empty, every entry is a leaf,
// so the most recent one (last appended) is returned.
func leafID(entries []Entry) string {
	if len(entries) == 0 {
		return ""
	}
	parents := computeLeafSet(entries)
	var leaves []Entry
	for _, e := range entries {
		if !parents[e.ID] {
			leaves = append(leaves, e)
		}
	}
	if len(leaves) == 0 {
		return entries[len(entries)-1].ID
	}
	slices.SortFunc(leaves, func(a, b Entry) int {
		return b.Timestamp.Compare(a.Timestamp) // descending — most recent first
	})
	return leaves[0].ID
}

// Snapshot returns a copy of all entries in the session at the moment of the call.
func (s *Session) Snapshot() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, len(s.entries))
	copy(out, s.entries)
	return out
}

// Append atomically appends an entry to the file and in-memory slice.
func (s *Session) Append(entry Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendEntry(entry)
}

// appendEntry must be called with s.mu held.
func (s *Session) appendEntry(entry Entry) error {
	if s.file == nil {
		return fmt.Errorf("session: file is closed")
	}
	if entry.ID == "" {
		entry.ID = uuid.New().String()
	}
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now().UTC()
	}
	// Chain to the current head so the tree is always explicit.
	if entry.ParentID == "" && s.headID != "" {
		entry.ParentID = s.headID
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("session: marshal entry: %w", err)
	}
	data = append(data, '\n')
	if _, err := s.file.Write(data); err != nil {
		return fmt.Errorf("session: write entry: %w", err)
	}
	s.entries = append(s.entries, entry)
	s.headID = entry.ID
	return nil
}

// AppendMessage appends a message entry to the session.
func (s *Session) AppendMessage(msg model.Message, usage *model.Usage) error {
	return s.Append(Entry{
		Type:    EntryMessage,
		Message: &msg,
		Usage:   usage,
	})
}

// Branch returns a new Session view branched from the entry with fromEntryID
// as its head. Both the original and the branch share the same JSONL file;
// diverging entries are appended with different ParentIDs, forming the tree
// implicitly in the append-only log.
//
// The caller must Close the returned Session when done — it holds an
// independent file descriptor.
func (s *Session) Branch(fromEntryID string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	found := false
	for _, e := range s.entries {
		if e.ID == fromEntryID {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("session: branch point %s not found", fromEntryID)
	}

	f, err := os.OpenFile(s.Path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("session: open branch file: %w", err)
	}

	return &Session{
		ID:      s.ID,
		Path:    s.Path,
		entries: append([]Entry(nil), s.entries...),
		headID:  fromEntryID,
		file:    f,
	}, nil
}

// HeadID returns the ID of the current branch tip.
func (s *Session) HeadID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.headID
}

// SetHead moves the active branch tip to the given entry ID without creating
// a new branch (analogous to `git checkout <sha>`).
func (s *Session) SetHead(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if e.ID == id {
			s.headID = id
			return nil
		}
	}
	return fmt.Errorf("session: entry %s not found", id)
}

// Heads returns all leaf entries — the tips of every branch — sorted newest first.
func (s *Session) Heads() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()

	parents := computeLeafSet(s.entries)
	var heads []Entry
	for _, e := range s.entries {
		if !parents[e.ID] {
			heads = append(heads, e)
		}
	}
	slices.SortFunc(heads, func(a, b Entry) int {
		return b.Timestamp.Compare(a.Timestamp)
	})
	return heads
}

// Messages reconstructs the message list for the active branch.
//
// When parent links are present it walks head → root via ParentID, reverses
// the path, and materialises only the messages on that branch.
// For legacy sessions (all ParentIDs empty) it falls back to linear order.
func (s *Session) Messages() []model.Message {
	s.mu.Lock()
	defer s.mu.Unlock()

	hasLinks := false
	for _, e := range s.entries {
		if e.ParentID != "" {
			hasLinks = true
			break
		}
	}
	if hasLinks && s.headID != "" {
		return s.messagesFromTree()
	}
	return s.messagesLinear()
}

// messagesFromTree walks head → root via ParentID, reverses, then builds the
// message slice. Must be called with s.mu held.
func (s *Session) messagesFromTree() []model.Message {
	byID := make(map[string]*Entry, len(s.entries))
	for i := range s.entries {
		byID[s.entries[i].ID] = &s.entries[i]
	}

	var path []string
	cur := s.headID
	for cur != "" {
		path = append(path, cur)
		e, ok := byID[cur]
		if !ok {
			break
		}
		cur = e.ParentID
	}
	slices.Reverse(path)

	return s.buildMessages(path, byID)
}

// messagesLinear reconstructs in entry order for legacy sessions without
// parent links. Must be called with s.mu held.
func (s *Session) messagesLinear() []model.Message {
	byID := make(map[string]*Entry, len(s.entries))
	path := make([]string, 0, len(s.entries))
	for i := range s.entries {
		byID[s.entries[i].ID] = &s.entries[i]
		path = append(path, s.entries[i].ID)
	}
	return s.buildMessages(path, byID)
}

// buildMessages materialises messages from an ordered ID path applying
// compaction. Must be called with s.mu held.
func (s *Session) buildMessages(path []string, byID map[string]*Entry) []model.Message {
	var msgs []model.Message
	compactionIdx := -1
	var compactionSummary string

	for _, id := range path {
		e, ok := byID[id]
		if !ok {
			continue
		}
		switch e.Type {
		case EntryMessage:
			if e.Message != nil {
				msgs = append(msgs, *e.Message)
			}
		case EntryCompaction:
			if e.Summary != "" {
				compactionIdx = len(msgs)
				compactionSummary = e.Summary
			}
		}
	}

	if compactionIdx >= 0 && compactionSummary != "" {
		summaryMsg := model.NewTextMessage(model.RoleUser,
			"[Previous conversation summary]\n\n"+compactionSummary)
		kept := msgs[compactionIdx:]
		result := make([]model.Message, 0, 1+len(kept))
		result = append(result, summaryMsg)
		result = append(result, kept...)
		return result
	}

	return msgs
}

// LastN returns the last n messages from the active branch.
func (s *Session) LastN(n int) []model.Message {
	msgs := s.Messages()
	if n >= len(msgs) {
		return msgs
	}
	return msgs[len(msgs)-n:]
}

// Title returns the first user message truncated to 60 runes.
func (s *Session) Title() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if e.Type == EntryMessage && e.Message != nil && e.Message.Role == model.RoleUser {
			runes := []rune(e.Message.Text())
			if len(runes) > 60 {
				return string(runes[:60]) + "..."
			}
			return string(runes)
		}
	}
	return s.ID
}

// Close closes the underlying file handle.
func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file != nil {
		err := s.file.Close()
		s.file = nil
		return err
	}
	return nil
}
