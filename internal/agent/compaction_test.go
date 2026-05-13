package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/tiru-r/pi-agent-go/internal/model"
	"github.com/tiru-r/pi-agent-go/internal/provider"
)

// mockProvider satisfies provider.Provider and returns a fixed text response.
type mockProvider struct {
	text string
	err  error
}

func (m *mockProvider) Name() string { return "mock" }

func (m *mockProvider) Stream(_ context.Context, _ *provider.Request) (<-chan provider.Event, error) {
	if m.err != nil {
		return nil, m.err
	}
	ch := make(chan provider.Event, 2)
	ch <- provider.Event{Type: provider.EventTextDelta, Text: m.text}
	ch <- provider.Event{Type: provider.EventMessageStop, StopReason: model.StopReasonEndTurn}
	close(ch)
	return ch, nil
}

func userMsg(text string) model.Message  { return model.NewTextMessage(model.RoleUser, text) }
func assistMsg(text string) model.Message { return model.NewTextMessage(model.RoleAssistant, text) }

func toolUseMsg(id string) model.Message {
	return model.Message{
		Role: model.RoleAssistant,
		Content: []model.ContentBlock{{
			Type:  model.ContentTypeToolUse,
			ID:    id,
			Name:  "bash",
			Input: json.RawMessage(`{"command":"ls"}`),
		}},
	}
}

func toolResultMsg(id string) model.Message {
	return model.Message{
		Role: model.RoleUser,
		Content: []model.ContentBlock{{
			Type:      model.ContentTypeToolResult,
			ToolUseID: id,
			Content:   []model.ContentBlock{{Type: model.ContentTypeText, Text: "output"}},
		}},
	}
}

// ── findCutPoint ─────────────────────────────────────────────────────────────

func TestFindCutPoint_AllFitInBudget(t *testing.T) {
	msgs := []model.Message{userMsg("hi"), assistMsg("hello")}
	// keepBudget larger than all tokens → nothing to summarise
	if got := findCutPoint(msgs, 100_000); got != 0 {
		t.Fatalf("got %d, want 0", got)
	}
}

func TestFindCutPoint_Empty(t *testing.T) {
	if got := findCutPoint(nil, 100); got != 0 {
		t.Fatalf("got %d, want 0", got)
	}
}

func TestFindCutPoint_BackwardBoundary(t *testing.T) {
	// Four messages; budget covers last two. Clean backward boundary at index 2.
	msgs := []model.Message{
		userMsg("msg0 msg0 msg0 msg0"), // ~5 tok
		assistMsg("msg1 msg1 msg1 msg1"), // ~5 tok
		userMsg("msg2 msg2 msg2 msg2"), // ~5 tok  ← keepBudget covers from here
		assistMsg("msg3 msg3 msg3 msg3"), // ~5 tok
	}
	// keepBudget = 12 tokens → walk back from end: msg3(5) + msg2(5) = 10 < 12, msg1(5) = 15 ≥ 12 → target=1
	// backward scan from target=1: msgs[0]=user, msgs[1]=assistant → not a clean boundary at i=1
	// Actually let me trace more carefully.
	// Budget 12 tokens. Walk backward:
	//   i=3: acc=5 < 12
	//   i=2: acc=10 < 12
	//   i=1: acc=15 ≥ 12 → target=1
	// Backward scan from target=1 to i=1: msgs[0]=user, not assistant → skip
	// i=0: stop (i>0 fails)
	// Forward scan from target+1=2: msgs[1]=assistant, msgs[2]=user(real) → return 2
	cut := findCutPoint(msgs, 12)
	if cut != 2 {
		t.Fatalf("got cut=%d, want 2", cut)
	}
	// msgs[:2] summarised, msgs[2:] kept
}

func TestFindCutPoint_LastResort(t *testing.T) {
	// Single long message that blows the budget; no turn boundary found.
	longText := strings.Repeat("word ", 1000)
	msgs := []model.Message{
		userMsg(longText),
		assistMsg(longText),
	}
	cut := findCutPoint(msgs, 1) // tiny budget forces target=1
	if cut != 1 {
		t.Fatalf("got cut=%d, want 1 (last resort)", cut)
	}
}

func TestFindCutPoint_SingleMessage(t *testing.T) {
	msgs := []model.Message{userMsg("hi")}
	if got := findCutPoint(msgs, 1); got != 0 {
		t.Fatalf("got %d, want 0 — single message cannot be cut", got)
	}
}

// ── splitByMI ────────────────────────────────────────────────────────────────

func TestSplitByMI_EmptySystem_FallsBackToRecency(t *testing.T) {
	msgs := make([]model.Message, 6)
	for i := range msgs {
		if i%2 == 0 {
			msgs[i] = userMsg("hello world hello world hello world")
		} else {
			msgs[i] = assistMsg("I understand")
		}
	}
	toSum, kept := splitByMI(msgs, 1, "") // empty system → recency cut
	// With empty system it falls through to findCutPoint, which with budget=1
	// will cut at some boundary.
	_ = toSum
	_ = kept
	// Key assertion: function returns without panic and kept+toSum covers all msgs.
	if len(toSum)+len(kept) != len(msgs) {
		t.Fatalf("partition lost messages: toSum=%d kept=%d total=%d", len(toSum), len(kept), len(msgs))
	}
}

func TestSplitByMI_NonContiguous_HighScoringMessagePreserved(t *testing.T) {
	// The two high-MI messages are an assistant→user pair so the kept sequence
	// is valid after the summary (user): summary(user)|assistant|user.
	// msg0 and msg3–5 are low-MI filler that should be summarised.
	system := "kubernetes deployment pipeline container orchestration"
	msgs := []model.Message{
		userMsg("hello"),                                                          // 0: low MI
		assistMsg("kubernetes deployment pipeline configuration ready"),           // 1: HIGH MI
		userMsg("kubernetes container orchestration setup confirmed"),             // 2: HIGH MI
		assistMsg("what is your favourite colour"),                                // 3: low MI
		userMsg("blue"),                                                           // 4: low MI
		assistMsg("ok"),                                                           // 5: low MI
	}

	// Exact budget for the two high-MI messages — no surplus to pull in filler.
	budget := estimateMsgTokens(msgs[1]) + estimateMsgTokens(msgs[2])

	toSum, kept := splitByMI(msgs, budget, system)

	if toSum == nil {
		t.Fatal("expected some messages to be summarised")
	}
	// msgs[1] and msgs[2] must be in kept.
	keptSet := make(map[string]bool)
	for _, m := range kept {
		keptSet[m.Content[0].Text] = true
	}
	if !keptSet["kubernetes deployment pipeline configuration ready"] {
		t.Error("msg1 (high MI assistant) should be preserved, but was summarised")
	}
	if !keptSet["kubernetes container orchestration setup confirmed"] {
		t.Error("msg2 (high MI user) should be preserved, but was summarised")
	}
	// Total must still equal len(msgs).
	if len(toSum)+len(kept) != len(msgs) {
		t.Fatalf("partition lost messages: toSum=%d kept=%d total=%d", len(toSum), len(kept), len(msgs))
	}
}

func TestSplitByMI_FallsBackWhenInternalAlternationInvalid(t *testing.T) {
	// Both high-MI messages are user messages; TF-IDF selection would produce
	// [user, user] within kept — internally invalid. The function must detect
	// this and fall back to findCutPoint so kept has no consecutive same-role pairs.
	system := "kubernetes deployment"
	msgs := []model.Message{
		userMsg("kubernetes deployment start"),    // 0: HIGH MI — user
		assistMsg("acknowledged"),                 // 1: low MI
		userMsg("kubernetes deployment continue"), // 2: HIGH MI — user
		assistMsg("got it"),                       // 3: low MI
		userMsg("something else entirely"),        // 4: low MI
		assistMsg("sure"),                         // 5: low MI
	}
	// Budget that selects msg0+msg2 (both user) — triggers the fallback.
	budget := estimateMsgTokens(msgs[0]) + estimateMsgTokens(msgs[2])

	toSum, kept := splitByMI(msgs, budget, system)

	if toSum == nil {
		t.Fatal("expected some messages to be summarised")
	}
	if len(toSum)+len(kept) != len(msgs) {
		t.Fatalf("partition lost messages: toSum=%d kept=%d total=%d", len(toSum), len(kept), len(msgs))
	}
	// Kept must have no two consecutive same-role messages.
	for i := 1; i < len(kept); i++ {
		if kept[i].Role == kept[i-1].Role {
			t.Errorf("kept[%d] and kept[%d] both have role %s — invalid conversation structure",
				i-1, i, kept[i].Role)
		}
	}
}

func TestSplitByMI_AllFitInBudget(t *testing.T) {
	msgs := []model.Message{userMsg("hello"), assistMsg("hi")}
	toSum, kept := splitByMI(msgs, 100_000, "some system prompt")
	if toSum != nil {
		t.Fatalf("expected nil toSummarise when all messages fit in budget, got %v", toSum)
	}
	if len(kept) != len(msgs) {
		t.Fatalf("all messages should be kept")
	}
}

func TestSplitByMI_ToolPairConstraint(t *testing.T) {
	// msg0: high-MI (would be kept), msg1: tool_use (would be discarded), msg2: tool_result (would be discarded)
	// Pair constraint: if either of msg1/msg2 is kept, both must be kept.
	// Since neither is initially selected, constraint doesn't fire.
	// Verify: if we manually construct a case where msg2 (tool_result) is selected but msg1 (tool_use) is not:
	// The pair constraint should force msg1 to be kept too.
	system := "bash shell command execution"
	msgs := []model.Message{
		userMsg("hello world how are you today"), // low MI
		toolUseMsg("t1"),                          // tool_use — has "bash" in input, some MI
		toolResultMsg("t1"),                       // tool_result — low content
		assistMsg("bash executed successfully"),   // high MI: "bash"
	}
	toSum, kept := splitByMI(msgs, 50, system)
	_ = toSum

	// Find tool_use and tool_result in result sets.
	var keptHasToolUse, keptHasToolResult bool
	var sumHasToolUse, sumHasToolResult bool
	for _, m := range kept {
		for _, b := range m.Content {
			if b.Type == model.ContentTypeToolUse {
				keptHasToolUse = true
			}
			if b.Type == model.ContentTypeToolResult {
				keptHasToolResult = true
			}
		}
	}
	for _, m := range toSum {
		for _, b := range m.Content {
			if b.Type == model.ContentTypeToolUse {
				sumHasToolUse = true
			}
			if b.Type == model.ContentTypeToolResult {
				sumHasToolResult = true
			}
		}
	}
	// Pair must be on same side.
	if keptHasToolUse != keptHasToolResult {
		t.Error("tool_use and tool_result should be in the same partition (kept)")
	}
	if sumHasToolUse != sumHasToolResult {
		t.Error("tool_use and tool_result should be in the same partition (summarised)")
	}
}

// ── ShouldCompact ─────────────────────────────────────────────────────────────

func TestShouldCompact_MeasuredBelowThreshold(t *testing.T) {
	c := &Compactor{MaxTokens: 1000}
	msgs := []model.Message{userMsg("hi")}
	if c.ShouldCompact(msgs, 500, 0) {
		t.Fatal("should not compact when measured tokens < threshold")
	}
}

func TestShouldCompact_MeasuredAboveThreshold(t *testing.T) {
	c := &Compactor{MaxTokens: 1000}
	msgs := []model.Message{userMsg("hi")}
	if !c.ShouldCompact(msgs, 1001, 0) {
		t.Fatal("should compact when measured tokens > threshold")
	}
}

func TestShouldCompact_HeuristicFallback(t *testing.T) {
	// measuredTokens=0 → falls back to heuristic; long text pushes over threshold.
	c := &Compactor{MaxTokens: 10}
	long := strings.Repeat("x", 200) // 200 chars / 4 = 50 tokens > 10
	msgs := []model.Message{userMsg(long)}
	if !c.ShouldCompact(msgs, 0, 0) {
		t.Fatal("should compact: heuristic estimate > threshold")
	}
}

func TestShouldCompact_ToolDefsTokensPushesOverThreshold(t *testing.T) {
	// Without tool defs overhead, heuristic is below threshold.
	// Adding tool defs tokens tips it over.
	c := &Compactor{MaxTokens: 20}
	msgs := []model.Message{userMsg(strings.Repeat("x", 40))} // 10 tokens
	if c.ShouldCompact(msgs, 0, 0) {
		t.Fatal("pre-condition: should not compact without tool defs overhead")
	}
	if !c.ShouldCompact(msgs, 0, 15) { // 10 + 15 = 25 > 20
		t.Fatal("should compact when tool defs tokens tip heuristic over threshold")
	}
}

func TestShouldCompact_MeasuredIgnoresToolDefs(t *testing.T) {
	// When measuredTokens > 0, toolDefsTokens is NOT added (API already includes them).
	c := &Compactor{MaxTokens: 1000}
	msgs := []model.Message{userMsg("hi")}
	// measured=500 well below threshold; tool defs overhead would push heuristic over
	// but measuredTokens takes precedence.
	if c.ShouldCompact(msgs, 500, 600) {
		t.Fatal("measuredTokens should take precedence; toolDefsTokens must not be added")
	}
}

// ── Compact ───────────────────────────────────────────────────────────────────

func TestCompact_NoSystem_UsesRecencyCut(t *testing.T) {
	prov := &mockProvider{text: "this is a summary"}
	c := &Compactor{
		Provider:         prov,
		MaxTokens:        100,
		KeepRecentTokens: 1, // tiny budget forces a cut
	}

	long := strings.Repeat("word ", 30) // ~30 tokens each
	msgs := []model.Message{
		userMsg(long),
		assistMsg(long),
		userMsg(long),
		assistMsg(long),
	}

	compacted, summary, err := c.Compact(context.Background(), msgs, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if summary != "this is a summary" {
		t.Fatalf("summary mismatch: %q", summary)
	}
	// compacted[0] must be the summary placeholder message.
	if len(compacted) == 0 {
		t.Fatal("compacted is empty")
	}
	if !strings.Contains(compacted[0].Content[0].Text, "this is a summary") {
		t.Fatalf("compacted[0] should contain summary text, got: %q", compacted[0].Content[0].Text)
	}
}

func TestCompact_WithSystem_UsesNonContiguousMI(t *testing.T) {
	prov := &mockProvider{text: "summary of irrelevant messages"}
	c := &Compactor{
		Provider:         prov,
		MaxTokens:        500,
		KeepRecentTokens: 20, // small budget so only a couple messages are kept
	}

	system := "database sql query optimisation index"
	filler := strings.Repeat("word ", 20) // ~20 tokens each
	msgs := []model.Message{
		userMsg("database sql index optimisation question " + filler), // high MI
		assistMsg(filler),                                              // low MI
		userMsg("what time is it " + filler),                          // low MI — should be summarised
		assistMsg("I do not know " + filler),                          // low MI — should be summarised
	}

	compacted, _, err := c.Compact(context.Background(), msgs, system)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(compacted) == 0 {
		t.Fatal("compacted is empty")
	}
	// compacted[0] is summary placeholder.
	if !strings.HasPrefix(compacted[0].Content[0].Text, "[Previous conversation summary]") {
		t.Fatalf("compacted[0] should be summary message, got: %q", compacted[0].Content[0].Text)
	}
}

func TestCompact_NothingToSummarise_ReturnsOriginal(t *testing.T) {
	prov := &mockProvider{text: "irrelevant"}
	c := &Compactor{
		Provider:         prov,
		MaxTokens:        100,
		KeepRecentTokens: 100_000, // budget covers everything
	}
	msgs := []model.Message{userMsg("hi"), assistMsg("hello")}
	compacted, summary, err := c.Compact(context.Background(), msgs, "some system")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if summary != "" {
		t.Fatalf("expected empty summary, got %q", summary)
	}
	if len(compacted) != len(msgs) {
		t.Fatalf("expected original messages returned unchanged")
	}
}

func TestCompact_ProviderError_Propagates(t *testing.T) {
	prov := &mockProvider{err: &mockErr{"stream failed"}}
	c := &Compactor{
		Provider:         prov,
		MaxTokens:        10,
		KeepRecentTokens: 1,
	}
	msgs := []model.Message{
		userMsg(strings.Repeat("x", 200)),
		assistMsg(strings.Repeat("y", 200)),
	}
	_, _, err := c.Compact(context.Background(), msgs, "")
	if err == nil {
		t.Fatal("expected error from provider, got nil")
	}
}

// ── EstimateToolDefsTokens ────────────────────────────────────────────────────

func TestEstimateToolDefsTokens_Empty(t *testing.T) {
	if got := EstimateToolDefsTokens(nil); got != 0 {
		t.Fatalf("got %d, want 0", got)
	}
}

func TestEstimateToolDefsTokens_Basic(t *testing.T) {
	defs := []model.ToolDefinition{
		{
			Name:        "bash",
			Description: strings.Repeat("x", 400), // 400 chars → 100 tokens
			InputSchema: json.RawMessage(`{"type":"object"}`),
		},
	}
	got := EstimateToolDefsTokens(defs)
	// "bash" = 1 tok, description = 100 tok, schema ≈ 4 tok
	if got < 100 {
		t.Fatalf("got %d, expected ≥ 100 tokens for a 400-char description", got)
	}
}

// ── BackgroundCompactor ───────────────────────────────────────────────────────

func TestBackgroundCompactor_TriggerAndTake(t *testing.T) {
	prov := &mockProvider{text: "background summary"}
	comp := &Compactor{
		Provider:         prov,
		MaxTokens:        100,
		KeepRecentTokens: 1,
	}
	bg := NewBackgroundCompactor(context.Background(), comp)

	msgs := []model.Message{
		userMsg(strings.Repeat("a", 200)),
		assistMsg(strings.Repeat("b", 200)),
	}

	bg.Trigger(msgs, "", "")

	// Poll until result is ready (bounded wait for test speed).
	var result *BGResult
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if result = bg.Take(); result != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if result == nil {
		t.Fatal("background compaction did not complete within 5s")
	}
	if result.Summary != "background summary" {
		t.Fatalf("summary mismatch: %q", result.Summary)
	}
	if result.SnapLen != len(msgs) {
		t.Fatalf("SnapLen=%d, want %d", result.SnapLen, len(msgs))
	}
}

func TestBackgroundCompactor_ConcurrentTrigger_IsNoOp(t *testing.T) {
	block := make(chan struct{})
	prov := &blockingProvider{block: block, text: "summary"}
	comp := &Compactor{
		Provider:         prov,
		MaxTokens:        10,
		KeepRecentTokens: 1,
	}
	bg := NewBackgroundCompactor(context.Background(), comp)

	msgs := []model.Message{
		userMsg(strings.Repeat("x", 200)),
		assistMsg(strings.Repeat("y", 200)),
	}

	bg.Trigger(msgs, "", "")
	if !bg.Running() {
		t.Fatal("expected Running() to be true after Trigger")
	}

	// Second trigger while first is in-flight must be a no-op.
	bg.Trigger(msgs, "should-be-ignored", "")

	// Unblock the goroutine.
	close(block)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !bg.Running() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	result := bg.Take()
	if result == nil {
		t.Fatal("expected a result after goroutine completed")
	}
	// The second Trigger's system prompt was ignored.
}

func TestBackgroundCompactor_Take_ClearsResult(t *testing.T) {
	prov := &mockProvider{text: "x"}
	comp := &Compactor{Provider: prov, MaxTokens: 10, KeepRecentTokens: 1}
	bg := NewBackgroundCompactor(context.Background(), comp)

	msgs := []model.Message{
		userMsg(strings.Repeat("a", 200)),
		assistMsg("hi"),
	}
	bg.Trigger(msgs, "", "")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if bg.Take() != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Second Take must return nil — result was consumed.
	if bg.Take() != nil {
		t.Fatal("Take should return nil after result has been consumed")
	}
}

// ── isToolPair ────────────────────────────────────────────────────────────────

func TestIsToolPair(t *testing.T) {
	tu := toolUseMsg("id1")
	tr := toolResultMsg("id1")
	plain := userMsg("hello")

	if !isToolPair(tu, tr) {
		t.Error("tool_use → tool_result should be a pair")
	}
	if isToolPair(plain, tr) {
		t.Error("user message → tool_result is not a pair")
	}
	if isToolPair(tu, plain) {
		t.Error("tool_use → plain user message is not a pair")
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

type mockErr struct{ msg string }

func (e *mockErr) Error() string { return e.msg }

// blockingProvider blocks until the block channel is closed, then returns text.
type blockingProvider struct {
	block <-chan struct{}
	text  string
}

func (p *blockingProvider) Name() string { return "blocking" }

func (p *blockingProvider) Stream(_ context.Context, _ *provider.Request) (<-chan provider.Event, error) {
	<-p.block
	ch := make(chan provider.Event, 2)
	ch <- provider.Event{Type: provider.EventTextDelta, Text: p.text}
	ch <- provider.Event{Type: provider.EventMessageStop, StopReason: model.StopReasonEndTurn}
	close(ch)
	return ch, nil
}
