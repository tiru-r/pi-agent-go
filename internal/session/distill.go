package session

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/tiru-r/pi-agent-go/internal/model"
)

// DistillSink receives completed (prompt, response, model) triples for
// knowledge distillation data collection.
type DistillSink interface {
	Record(prompt string, response string, model string)
}

// distillRecord is the JSONL line format for distillation output.
type distillRecord struct {
	Prompt    string    `json:"prompt"`
	Model     string    `json:"model"`
	Response  string    `json:"response"`
	Timestamp time.Time `json:"timestamp"`
}

// JSONLDistillSink appends (prompt, model, response, timestamp) JSONL lines to a
// file. The file is kept open for the lifetime of the sink; call Close when done.
type JSONLDistillSink struct {
	mu   sync.Mutex
	file *os.File
}

// NewJSONLDistillSink opens path for appending (creating it if absent) and
// returns a sink ready to receive records.
func NewJSONLDistillSink(path string) (*JSONLDistillSink, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("distill: open sink: %w", err)
	}
	return &JSONLDistillSink{file: f}, nil
}

// Record appends one distillation record to the sink file.
func (s *JSONLDistillSink) Record(prompt, response, modelName string) {
	rec := distillRecord{
		Prompt:    prompt,
		Model:     modelName,
		Response:  response,
		Timestamp: time.Now().UTC(),
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.file.Write(append(data, '\n'))
}

// Close flushes and closes the underlying file.
func (s *JSONLDistillSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.file.Close()
}

// traceScore returns a quality score ∈ [0, 1] for a session.
//
// Heuristic:
//   - A tool error drops the base score to 0.
//   - Ending with a natural assistant stop gives base score 1.
//   - An efficiency bonus rewards shorter sessions (≤10 turns).
func traceScore(entries []Entry) float64 {
	var msgCount int
	hasToolError := false
	naturalStop := false

	for _, e := range entries {
		if e.Type != EntryMessage {
			continue
		}
		msgCount++
		if e.Message == nil {
			continue
		}
		for _, block := range e.Message.Content {
			if block.IsError {
				hasToolError = true
			}
		}
	}

	if msgCount == 0 {
		return 0
	}

	// Natural stop: last message entry is an assistant message.
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.Type != EntryMessage || e.Message == nil {
			continue
		}
		naturalStop = e.Message.Role == model.RoleAssistant
		break
	}

	base := 0.0
	if !hasToolError && naturalStop {
		base = 1.0
	} else if !hasToolError {
		base = 0.6
	} else if naturalStop {
		base = 0.3
	}

	// Efficiency interpolation: [1..10] turns → max bonus 0.2; >10 → 0 bonus.
	efficiency := 0.0
	if msgCount <= 10 {
		efficiency = 0.2 * (1 - float64(msgCount-1)/10)
	}

	score := base + efficiency
	if score > 1 {
		score = 1
	}
	return score
}

// Distill reads all sessions from store, scores each by the quality heuristic,
// and exports entries from sessions scoring ≥ threshold as JSONL to outPath.
// Returns the number of sessions exported and any write error.
func Distill(store *SQLiteStore, outPath string, threshold float64) (int, error) {
	metas, err := store.ListSessions()
	if err != nil {
		return 0, fmt.Errorf("distill: list sessions: %w", err)
	}

	f, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, fmt.Errorf("distill: open output: %w", err)
	}
	defer f.Close()

	exported := 0
	for _, meta := range metas {
		entries, queryErr := store.QueryEntries(meta.ID)
		if queryErr != nil {
			continue
		}
		score := traceScore(entries)
		if score < threshold {
			continue
		}
		rec := map[string]any{
			"session_id": meta.ID,
			"score":      score,
			"entries":    entries,
			"timestamp":  time.Now().UTC(),
		}
		data, marshalErr := json.Marshal(rec)
		if marshalErr != nil {
			continue
		}
		if _, writeErr := f.Write(append(data, '\n')); writeErr != nil {
			return exported, fmt.Errorf("distill: write record: %w", writeErr)
		}
		exported++
	}
	return exported, nil
}
