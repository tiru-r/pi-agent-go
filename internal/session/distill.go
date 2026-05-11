package session

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"time"
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

// JSONLDistillSink appends (prompt, model, response, timestamp) JSONL lines to a file.
type JSONLDistillSink struct {
	path string
}

// NewJSONLDistillSink creates a sink that appends to path (creating if absent).
func NewJSONLDistillSink(path string) *JSONLDistillSink {
	return &JSONLDistillSink{path: path}
}

// Record appends one distillation record to the sink file.
func (s *JSONLDistillSink) Record(prompt, response, model string) {
	rec := distillRecord{
		Prompt:    prompt,
		Model:     model,
		Response:  response,
		Timestamp: time.Now().UTC(),
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(data, '\n'))
}

// traceScore returns a quality score ∈ [0, 1] for a session loaded from the
// store.  The scoring heuristic:
//   - If any message entry carries a tool error, base score → 0.
//   - Sessions that ended with a natural stop get base score 1.
//   - Efficiency bonus: shorter sessions (fewer turns) get a small bonus so
//     we prefer concise, correct sessions over verbose ones.
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
	// Natural stop: last message entry is an assistant message (not user/tool).
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.Type != EntryMessage || e.Message == nil {
			continue
		}
		if string(e.Message.Role) == "assistant" {
			naturalStop = true
		}
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
	if msgCount <= 10 && msgCount > 0 {
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
		entries, queryErr := loadSessionEntries(store.db, meta.ID)
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

// loadSessionEntries returns the entries for the given session ID directly
// from the database without constructing a full Session object.
func loadSessionEntries(db *sql.DB, sessionID string) ([]Entry, error) {
	rows, err := db.Query(`
		SELECT data FROM session_entries
		WHERE session_id=?
		ORDER BY timestamp ASC`, sessionID)
	if err != nil {
		return nil, err
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
	return entries, rows.Err()
}
