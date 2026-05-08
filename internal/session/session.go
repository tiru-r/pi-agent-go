// Package session provides JSONL-based session persistence (format version 3).
package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tiru-r/pi-agent-go/internal/model"
)

// SESSION_VERSION is the current JSONL file format version.
const SESSION_VERSION = 3

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
type Session struct {
	ID      string
	Path    string
	Entries []Entry

	mu   sync.Mutex
	file *os.File
}

// New creates a new session in dir with a freshly-generated UUID.
func New(dir string) (*Session, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("session: create dir: %w", err)
	}
	id := uuid.New().String()
	path := filepath.Join(dir, id+".jsonl")

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, fmt.Errorf("session: create file: %w", err)
	}

	s := &Session{ID: id, Path: path, file: f}

	// Write version header
	hdr, _ := json.Marshal(versionHeader{Version: SESSION_VERSION})
	if _, err := f.Write(append(hdr, '\n')); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("session: write version header: %w", err)
	}

	// Write initial metadata entry
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

// Open loads an existing session from a JSONL file.
func Open(path string) (*Session, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("session: read file: %w", err)
	}

	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) == 0 {
		return nil, fmt.Errorf("session: file is empty")
	}

	// First line may be version header — skip it.
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

	// Extract session ID from metadata entry or filename
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
		Entries: entries,
		file:    f,
	}, nil
}

// Append atomically appends an entry to the file and in-memory slice.
func (s *Session) Append(entry Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendEntry(entry)
}

// appendEntry must be called with s.mu held.
func (s *Session) appendEntry(entry Entry) error {
	if entry.ID == "" {
		entry.ID = uuid.New().String()
	}
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now().UTC()
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("session: marshal entry: %w", err)
	}
	data = append(data, '\n')
	if _, err := s.file.Write(data); err != nil {
		return fmt.Errorf("session: write entry: %w", err)
	}
	s.Entries = append(s.Entries, entry)
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

// Messages reconstructs the linear message list from entries.
// Compaction entries cause older messages to be replaced by a summary stub.
func (s *Session) Messages() []model.Message {
	s.mu.Lock()
	defer s.mu.Unlock()

	var msgs []model.Message
	compactionIdx := -1
	var compactionSummary string

	for _, e := range s.Entries {
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

// LastN returns the last n messages from the session.
func (s *Session) LastN(n int) []model.Message {
	msgs := s.Messages()
	if n >= len(msgs) {
		return msgs
	}
	return msgs[len(msgs)-n:]
}

// Title returns the first user message truncated to 60 characters.
func (s *Session) Title() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.Entries {
		if e.Type == EntryMessage && e.Message != nil && e.Message.Role == model.RoleUser {
			text := e.Message.Text()
			if len(text) > 60 {
				text = text[:60] + "..."
			}
			return text
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
