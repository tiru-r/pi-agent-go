package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tiru-r/pi-agent-go/internal/model"
	"github.com/tiru-r/pi-agent-go/internal/provider"
	"github.com/tiru-r/pi-agent-go/internal/tools"
)

const (
	charsPerToken              = 4
	imageTokenEstimate         = 1200
	defaultMaxCompactionTokens = 100_000
	defaultReserveRatio        = 8  // percent of ContextWindow reserved for output
	defaultKeepRecentRatio     = 10 // percent of ContextWindow kept as recent context
	defaultKeepRecentTokens    = 10_000
)

// Compactor decides when and how to compact (summarise) a conversation's
// message history so it stays within the provider's context window.
type Compactor struct {
	Provider         provider.Provider
	Model            string // model to use for summarisation (defaults to provider default)
	ContextWindow    int    // model's actual context window in tokens (0 = unknown)
	ReserveTokens    int    // tokens to keep free for new output (0 = 8% of ContextWindow)
	MaxTokens        int    // fallback trigger threshold when ContextWindow is 0 (default 100_000)
	KeepRecentTokens int    // token budget for recent messages (0 = 10% of ContextWindow)
}

// threshold returns the token count above which compaction is triggered.
// When ContextWindow is known the reserve defaults to 8% of the window;
// otherwise it falls back to MaxTokens (or 100 000).
func (c *Compactor) threshold() int {
	if c.ContextWindow > 0 {
		reserve := c.ReserveTokens
		if reserve <= 0 {
			reserve = c.ContextWindow * defaultReserveRatio / 100
		}
		return c.ContextWindow - reserve
	}
	if c.MaxTokens > 0 {
		return c.MaxTokens
	}
	return defaultMaxCompactionTokens
}

// keepBudget returns the token budget for the "recent" portion of the
// conversation that is always kept intact. Defaults to 10% of ContextWindow.
func (c *Compactor) keepBudget() int {
	if c.KeepRecentTokens > 0 {
		return c.KeepRecentTokens
	}
	if c.ContextWindow > 0 {
		return c.ContextWindow * defaultKeepRecentRatio / 100
	}
	if c.MaxTokens > 0 {
		return c.MaxTokens * defaultKeepRecentRatio / 100
	}
	return defaultKeepRecentTokens
}

// ShouldCompact returns true when the estimated token count exceeds the
// threshold. measuredTokens should be the InputTokens value from the last API
// response; pass 0 to fall back to the chars÷4 heuristic. toolDefsTokens is
// added to the heuristic estimate so callers can account for tool-definition
// overhead not present in the message content (see EstimateToolDefsTokens).
func (c *Compactor) ShouldCompact(msgs []model.Message, measuredTokens, toolDefsTokens int) bool {
	tokens := measuredTokens
	if tokens <= 0 {
		tokens = estimateTokens(msgs) + toolDefsTokens
	}
	return tokens > c.threshold()
}

// Compact summarises the less-relevant portion of msgs and returns a shortened
// list. When system is non-empty, messages are scored by TF-IDF similarity to
// the system prompt; the lowest-scoring messages are summarised and the
// highest-scoring ones are preserved verbatim in their original order (non-
// contiguous selection). Adjacent tool-use/tool-result pairs are kept or
// discarded together to avoid broken conversation structure. When system is
// empty, a contiguous recency-based cut is used (see findCutPoint).
//
// The returned list is [{role:user, "[Previous conversation summary]\n\n<summary>"}, ...keptMsgs].
// The second return value is the summary text for session persistence.
func (c *Compactor) Compact(ctx context.Context, msgs []model.Message, system string) ([]model.Message, string, error) {
	var toSummarise, kept []model.Message

	if system != "" {
		toSummarise, kept = splitByMI(msgs, c.keepBudget(), system)
	} else {
		cut := findCutPoint(msgs, c.keepBudget())
		if cut == 0 {
			return msgs, "", nil
		}
		toSummarise, kept = msgs[:cut], msgs[cut:]
	}

	if len(toSummarise) == 0 {
		return msgs, "", nil
	}

	summary, err := c.summarise(ctx, toSummarise, system)
	if err != nil {
		return nil, "", fmt.Errorf("compactor: summarise: %w", err)
	}

	summaryMsg := model.NewTextMessage(model.RoleUser, "[Previous conversation summary]\n\n"+summary)
	compacted := make([]model.Message, 0, 1+len(kept))
	compacted = append(compacted, summaryMsg)
	compacted = append(compacted, kept...)
	return compacted, summary, nil
}

// findCutPoint returns the index of the first message to keep (msgs[:cut] is
// summarised). It walks backward from the end of msgs, accumulating token
// estimates until keepBudget tokens are covered. The tentative cut is then
// aligned to a clean turn boundary (after an assistant message and before a
// genuine user prompt) by walking backward first, then forward, then forcing
// a cut at position 1 as a last resort.
// Returns 0 if compaction cannot meaningfully reduce the history.
func findCutPoint(msgs []model.Message, keepBudget int) int {
	accumulated := 0
	target := len(msgs)

	for i := len(msgs) - 1; i >= 0; i-- {
		accumulated += estimateMsgTokens(msgs[i])
		if accumulated >= keepBudget {
			target = i
			break
		}
	}

	if target == len(msgs) || target == 0 {
		return 0
	}

	// Prefer a backward boundary: cut lands after an assistant turn.
	for i := target; i > 0; i-- {
		if msgs[i-1].Role == model.RoleAssistant && isRealUserMessage(msgs[i]) {
			return i
		}
	}

	// Fallback: walk forward so compaction fires despite an unclean boundary.
	for i := target + 1; i < len(msgs); i++ {
		if msgs[i-1].Role == model.RoleAssistant && isRealUserMessage(msgs[i]) {
			return i
		}
	}

	// Last resort: force cut at 1 to avoid a silent no-op when context is full.
	if len(msgs) > 1 {
		return 1
	}
	return 0
}

// splitByMI scores each message by TF-IDF similarity to the system prompt and
// partitions msgs into (toSummarise, kept), preserving original order. The kept
// set contains the highest-scoring messages up to keepBudget tokens; the rest
// go to toSummarise. Adjacent tool-use/tool-result pairs are kept or discarded
// together so the resulting conversation has valid structure.
//
// Returns (nil, msgs) when nothing needs summarising.
func splitByMI(msgs []model.Message, keepBudget int, system string) (toSummarise, kept []model.Message) {
	if len(msgs) == 0 || system == "" {
		return nil, msgs
	}

	refTerms := tokeniseText(system)
	if len(refTerms) == 0 {
		cut := findCutPoint(msgs, keepBudget)
		if cut == 0 {
			return nil, msgs
		}
		return msgs[:cut], msgs[cut:]
	}

	refTF := termFreq(refTerms)
	n := len(msgs)

	// Compute document frequency across all messages.
	df := make(map[string]int, len(refTF))
	for _, msg := range msgs {
		seen := make(map[string]bool)
		for _, t := range tokeniseText(msgText(msg)) {
			if !seen[t] {
				df[t]++
				seen[t] = true
			}
		}
	}

	// Score each message: sum of TF-IDF weights for terms shared with system prompt.
	scores := make([]float64, n)
	for i, msg := range msgs {
		tf := termFreq(tokeniseText(msgText(msg)))
		var score float64
		for term, msgTF := range tf {
			if _, inRef := refTF[term]; inRef {
				idf := math.Log(float64(n+1)/float64(df[term]+1)) + 1
				score += msgTF * idf
			}
		}
		scores[i] = score
	}

	// Rank by score descending, greedily select top-scoring messages up to budget.
	type scored struct {
		idx   int
		score float64
	}
	ranked := make([]scored, n)
	for i, s := range scores {
		ranked[i] = scored{i, s}
	}
	sort.Slice(ranked, func(a, b int) bool {
		return ranked[a].score > ranked[b].score
	})

	keptSet := make([]bool, n)
	budget := keepBudget
	for _, r := range ranked {
		tok := estimateMsgTokens(msgs[r.idx])
		if tok > budget {
			continue
		}
		keptSet[r.idx] = true
		budget -= tok
		if budget <= 0 {
			break
		}
	}

	// Enforce tool-use/tool-result pair constraint: keep both or neither in each pair.
	for i := 0; i+1 < n; i++ {
		if isToolPair(msgs[i], msgs[i+1]) && (keptSet[i] != keptSet[i+1]) {
			keptSet[i] = true
			keptSet[i+1] = true
		}
	}

	// Partition in original order.
	for i, msg := range msgs {
		if keptSet[i] {
			kept = append(kept, msg)
		} else {
			toSummarise = append(toSummarise, msg)
		}
	}
	if len(toSummarise) == 0 {
		return nil, msgs
	}

	// Non-contiguous selection can produce invalid role sequences (e.g. two
	// consecutive user messages). Fall back to contiguous recency cut when that
	// happens — correctness beats relevance scoring.
	if !isRoleAlternationValid(kept) {
		cut := findCutPoint(msgs, keepBudget)
		if cut == 0 {
			return nil, msgs
		}
		return msgs[:cut], msgs[cut:]
	}

	return toSummarise, kept
}

// isRoleAlternationValid reports whether msgs contains no two consecutive
// messages with the same role. This checks only within the kept slice itself;
// the inherent summary(user)→kept[0](user) boundary that findCutPoint also
// produces is a separate pre-existing design characteristic, not checked here.
func isRoleAlternationValid(msgs []model.Message) bool {
	for i := 1; i < len(msgs); i++ {
		if msgs[i].Role == msgs[i-1].Role {
			return false
		}
	}
	return true
}

// isToolPair reports whether prev is an assistant message containing tool_use
// blocks and next is a user message containing tool_result blocks — a pair that
// must be kept together to produce valid conversation structure.
func isToolPair(prev, next model.Message) bool {
	if prev.Role != model.RoleAssistant || next.Role != model.RoleUser {
		return false
	}
	hasTU := false
	for _, b := range prev.Content {
		if b.Type == model.ContentTypeToolUse {
			hasTU = true
			break
		}
	}
	if !hasTU {
		return false
	}
	for _, b := range next.Content {
		if b.Type == model.ContentTypeToolResult {
			return true
		}
	}
	return false
}

// EstimateToolDefsTokens returns a rough token estimate for a set of tool
// definitions using the chars÷4 heuristic. Pass the result as toolDefsTokens
// to ShouldCompact so the heuristic path accounts for tool-definition overhead
// that is not present in the message content.
func EstimateToolDefsTokens(defs []model.ToolDefinition) int {
	total := 0
	for _, d := range defs {
		total += len(d.Name) / charsPerToken
		total += len(d.Description) / charsPerToken
		total += len(d.InputSchema) / charsPerToken
	}
	return total
}

// BackgroundCompactor wraps a Compactor, running Compact asynchronously so
// compaction does not block the foreground agent turn. Only one compaction runs
// at a time; concurrent Trigger calls while one is in-flight are silently
// ignored. The completed result is stored and retrieved with Take on the next
// agent turn.
//
// Use NewBackgroundCompactor to construct — it binds a parent context so
// background goroutines cannot outlive the server or session that owns them.
type BackgroundCompactor struct {
	C   *Compactor     // must not be nil
	ctx context.Context // parent context; set by NewBackgroundCompactor

	mu      sync.Mutex
	running bool
	pending *BGResult
}

// NewBackgroundCompactor creates a BackgroundCompactor whose goroutines derive
// their context from parent. Pass the server or session lifetime context so
// compaction goroutines are bounded by the owner's lifetime rather than
// context.Background().
func NewBackgroundCompactor(parent context.Context, c *Compactor) *BackgroundCompactor {
	return &BackgroundCompactor{C: c, ctx: parent}
}

// BGResult holds the output of a completed background Compact call plus the
// snapshot metadata needed to splice in messages added since Trigger.
type BGResult struct {
	Compacted []model.Message // [summaryMsg, ...keptMsgs]
	Summary   string
	// SnapLen is len(msgs) at Trigger time. Messages at indices ≥ SnapLen in the
	// current history are "new" and must be appended to Compacted when applying.
	SnapLen int
	// HeadID is session.Session.HeadID() at Trigger time for SessionAgent; empty for Agent.
	HeadID string
}

// Trigger starts asynchronous compaction of msgs if no compaction is already
// running. headID must be session.Session.HeadID() for SessionAgent; pass ""
// for the stateless Agent. msgs is deep-copied before the goroutine starts to
// prevent data races.
func (b *BackgroundCompactor) Trigger(msgs []model.Message, system, headID string) {
	b.mu.Lock()
	if b.running {
		b.mu.Unlock()
		return
	}
	b.running = true
	b.mu.Unlock()

	snapLen := len(msgs)
	cp := make([]model.Message, len(msgs))
	copy(cp, msgs)

	go func() {
		parent := b.ctx
		if parent == nil {
			parent = context.Background() // safe fallback for zero-value construction
		}
		ctx, cancel := context.WithTimeout(parent, 3*time.Minute)
		defer cancel()

		compacted, summary, err := b.C.Compact(ctx, cp, system)

		b.mu.Lock()
		defer b.mu.Unlock()
		b.running = false
		if err == nil {
			b.pending = &BGResult{
				Compacted: compacted,
				Summary:   summary,
				SnapLen:   snapLen,
				HeadID:    headID,
			}
		}
	}()
}

// Take returns the latest compaction result and clears it. Returns nil when
// no result is ready yet.
func (b *BackgroundCompactor) Take() *BGResult {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.pending
	b.pending = nil
	return r
}

// Running reports whether a background compaction goroutine is currently active.
func (b *BackgroundCompactor) Running() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.running
}

// tokeniseText splits text into lowercase word tokens (non-alpha chars as delimiters).
func tokeniseText(s string) []string {
	var tokens []string
	var cur strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' {
			cur.WriteRune(r)
		} else {
			if cur.Len() > 1 {
				tokens = append(tokens, cur.String())
			}
			cur.Reset()
		}
	}
	if cur.Len() > 1 {
		tokens = append(tokens, cur.String())
	}
	return tokens
}

// termFreq returns normalised term frequency (count/total) for a token slice.
func termFreq(tokens []string) map[string]float64 {
	counts := make(map[string]int, len(tokens))
	for _, t := range tokens {
		counts[t]++
	}
	n := float64(len(tokens))
	if n == 0 {
		n = 1
	}
	tf := make(map[string]float64, len(counts))
	for t, c := range counts {
		tf[t] = float64(c) / n
	}
	return tf
}

// msgText returns the plain-text content of a message for TF-IDF scoring.
func msgText(msg model.Message) string {
	var sb strings.Builder
	for _, b := range msg.Content {
		switch b.Type {
		case model.ContentTypeText:
			sb.WriteString(b.Text)
			sb.WriteByte(' ')
		case model.ContentTypeThinking:
			sb.WriteString(b.Thinking)
			sb.WriteByte(' ')
		}
	}
	return sb.String()
}

// isRealUserMessage returns true when msg is a genuine user prompt, not a
// tool_result reply injected by the agent loop.
func isRealUserMessage(msg model.Message) bool {
	if msg.Role != model.RoleUser {
		return false
	}
	for _, b := range msg.Content {
		if b.Type == model.ContentTypeToolResult {
			return false
		}
	}
	return true
}

// summarise calls the provider with a summarisation request for toSummarise.
// File paths touched by Read / Write / Edit tool calls are extracted and
// appended to the prompt so the summary preserves file-operation context.
// The summary token budget scales with the input: ~20% of estimated input
// tokens, clamped to [2048, 8192].
func (c *Compactor) summarise(ctx context.Context, msgs []model.Message, system string) (string, error) {
	readFiles, modFiles := extractFilePaths(msgs)

	var promptSB strings.Builder
	promptSB.WriteString("Summarize this conversation so far. Be thorough. Include all important context, " +
		"decisions made, files modified, and any ongoing tasks.")
	if len(readFiles) > 0 {
		promptSB.WriteString("\n\n<read-files>\n")
		for _, p := range readFiles {
			promptSB.WriteString(p)
			promptSB.WriteByte('\n')
		}
		promptSB.WriteString("</read-files>")
	}
	if len(modFiles) > 0 {
		promptSB.WriteString("\n\n<modified-files>\n")
		for _, p := range modFiles {
			promptSB.WriteString(p)
			promptSB.WriteByte('\n')
		}
		promptSB.WriteString("</modified-files>")
	}

	reqMsgs := make([]model.Message, 0, len(msgs)+1)
	reqMsgs = append(reqMsgs, msgs...)
	reqMsgs = append(reqMsgs, model.NewTextMessage(model.RoleUser, promptSB.String()))

	summaryMaxTokens := min(max(estimateTokens(msgs)/5, 2048), 8192)

	req := &provider.Request{
		Model:     c.Model,
		Messages:  reqMsgs,
		System:    system,
		MaxTokens: summaryMaxTokens,
	}

	events, err := c.Provider.Stream(ctx, req)
	if err != nil {
		return "", err
	}
	resp, err := provider.Collect(events)
	if err != nil {
		return "", err
	}

	var sb strings.Builder
	for _, block := range resp.Message.Content {
		if block.Type == model.ContentTypeText {
			sb.WriteString(block.Text)
		}
	}
	return sb.String(), nil
}

// extractFilePaths scans tool_use blocks in msgs and returns sorted slices of
// read and modified file paths, recognised via the standard "path" parameter
// of the Read, Write, Edit, and hashline_edit tools.
func extractFilePaths(msgs []model.Message) (read, modified []string) {
	readSet := make(map[string]bool)
	modSet := make(map[string]bool)

	for _, msg := range msgs {
		for _, block := range msg.Content {
			if block.Type != model.ContentTypeToolUse || len(block.Input) == 0 {
				continue
			}
			var params map[string]any
			if json.Unmarshal(block.Input, &params) != nil {
				continue
			}
			path, _ := params["path"].(string)
			if path == "" {
				continue
			}
			switch block.Name {
			case tools.ToolNameRead:
				readSet[path] = true
			case tools.ToolNameWrite, tools.ToolNameEdit, tools.ToolNameHashlineEdit:
				modSet[path] = true
			}
		}
	}

	for p := range readSet {
		read = append(read, p)
	}
	for p := range modSet {
		modified = append(modified, p)
	}
	sort.Strings(read)
	sort.Strings(modified)
	return
}

// estimateTokens returns a rough token count for a slice of messages using
// the chars÷4 heuristic for text and a flat 1 200 tokens per image.
func estimateTokens(msgs []model.Message) int {
	total := 0
	for _, msg := range msgs {
		total += estimateMsgTokens(msg)
	}
	return total
}

// estimateMsgTokens returns a rough token count for a single message.
func estimateMsgTokens(msg model.Message) int {
	total := 0
	for _, block := range msg.Content {
		switch block.Type {
		case model.ContentTypeText:
			total += len(block.Text) / charsPerToken
		case model.ContentTypeImage:
			total += imageTokenEstimate
		case model.ContentTypeToolUse:
			total += len(block.Input) / charsPerToken
		case model.ContentTypeToolResult:
			for _, sub := range block.Content {
				total += len(sub.Text) / charsPerToken
			}
		case model.ContentTypeThinking:
			total += len(block.Thinking) / charsPerToken
		}
	}
	return total
}
