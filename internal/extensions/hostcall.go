package extensions

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tiru-r/pi-agent-go/internal/tools"
)

// HostcallKind identifies the type of operation a hostcall performs.
type HostcallKind string

const (
	HCKindTool    HostcallKind = "tool"
	HCKindHTTP    HostcallKind = "http"
	HCKindExec    HostcallKind = "exec"
	HCKindEnv     HostcallKind = "env"
	HCKindSession HostcallKind = "session"
	HCKindUI      HostcallKind = "ui"
	HCKindLog     HostcallKind = "log"
)

// kindCapability maps a HostcallKind to the capability it requires.
// Empty string means the call is always allowed.
var kindCapability = map[HostcallKind]Capability{
	HCKindHTTP:    CapNetwork,
	HCKindExec:    CapExec,
	HCKindEnv:     CapEnv,
	HCKindSession: CapSession,
	HCKindUI:      CapUI,
	HCKindLog:     "", // always allowed
	HCKindTool:    "", // capability determined per tool name
}

// HostcallRequest is the canonical record for a single extension → Pi call.
type HostcallRequest struct {
	CallID    string       `json:"call_id"`
	ExtName   string       `json:"ext_name"`
	Kind      HostcallKind `json:"kind"`
	ToolName  string       `json:"tool_name,omitempty"` // HCKindTool only
	Payload   any          `json:"payload,omitempty"`
	Timestamp time.Time    `json:"timestamp"`
	DedupKey  string       `json:"-"` // SHA-256 of canonical payload
}

// DispatchMode controls how the dispatcher batches concurrent hostcalls.
type DispatchMode string

const (
	ModeSequentialFastPath DispatchMode = "sequential_fast_path"
	ModeInterleavedBatch   DispatchMode = "interleaved_batching"
)

// HostcallTelemetry is the structured telemetry record emitted per hostcall.
// Schema name: pi.ext.hostcall_telemetry.v1
type HostcallTelemetry struct {
	CallID          string        `json:"call_id"`
	ExtName         string        `json:"ext_name"`
	Kind            HostcallKind  `json:"kind"`
	LaneKey         string        `json:"lane_key"`
	Lane            string        `json:"lane"`
	FallbackReason  string        `json:"fallback_reason,omitempty"`
	DispatchLatency time.Duration `json:"dispatch_latency_us"`
	MarshalPath     string        `json:"marshal_path"`
	OptHit          bool          `json:"opt_hit"` // dedup cache hit
	Success         bool          `json:"success"`
	Timestamp       time.Time     `json:"timestamp"`
}

// HostcallDispatcher manages capability enforcement, deduplication, lane
// selection, shadow dual execution, adaptive dispatch, and telemetry.
type HostcallDispatcher struct {
	extName  string
	manifest Manifest

	// Deduplication: SHA-256(canonical payload) → cached result, TTL 500ms.
	dedupMu    sync.Mutex
	dedupCache map[string]dedupEntry

	// Shadow dual execution: sample read-only calls and compare lane outputs.
	shadowRate   float64 // fraction of read-only calls to shadow (default 0.05)
	shadowBudget int     // max divergences before disabling fast lane
	shadowCount  int     // running divergence count
	fastLaneOK   atomic.Bool

	// Adaptive dispatch: switch mode based on queue pressure.
	modeMu   sync.Mutex
	mode     DispatchMode
	queueLen atomic.Int64

	// Telemetry ring buffer (capped at 1000 entries).
	telMu  sync.Mutex
	telBuf []HostcallTelemetry

	// Monotonic call counter for generating call IDs.
	counter atomic.Uint64
}

type dedupEntry struct {
	result  string
	isError bool
	expires time.Time
}

const (
	dedupWindow   = 500 * time.Millisecond
	telBufCap     = 1000
	defaultShadow = 0.05
	shadowBudget  = 3
)

// NewHostcallDispatcher constructs a dispatcher for the named extension.
func NewHostcallDispatcher(extName string, manifest Manifest) *HostcallDispatcher {
	d := &HostcallDispatcher{
		extName:      extName,
		manifest:     manifest,
		dedupCache:   make(map[string]dedupEntry),
		shadowRate:   defaultShadow,
		shadowBudget: shadowBudget,
		mode:         ModeSequentialFastPath,
	}
	d.fastLaneOK.Store(true)
	return d
}

// NewRequest builds a HostcallRequest with a unique call_id and dedup key.
func (d *HostcallDispatcher) NewRequest(kind HostcallKind, toolName string, payload any) HostcallRequest {
	id := d.counter.Add(1)
	req := HostcallRequest{
		CallID:    fmt.Sprintf("hc-%04x-%s", id, d.extName),
		ExtName:   d.extName,
		Kind:      kind,
		ToolName:  toolName,
		Payload:   payload,
		Timestamp: time.Now(),
	}
	req.DedupKey = canonicalHash(kind, toolName, payload)
	return req
}

// CheckPolicy returns the PolicyDecision for req.
// Tool calls consult toolCapability; other kinds use kindCapability.
func (d *HostcallDispatcher) CheckPolicy(req HostcallRequest) PolicyDecision {
	var required Capability
	if req.Kind == HCKindTool {
		required = toolCapability(req.ToolName)
	} else {
		required = kindCapability[req.Kind]
	}
	if required == "" {
		return PolicyAllow
	}
	if d.manifest.has(required) {
		return PolicyAllow
	}
	return PolicyDeny
}

// Dispatch executes req through the full pipeline:
// dedup check → capability policy → lane selection → execution → telemetry.
func (d *HostcallDispatcher) Dispatch(ctx context.Context, req HostcallRequest) (string, bool, error) {
	start := time.Now()
	tel := HostcallTelemetry{
		CallID:    req.CallID,
		ExtName:   req.ExtName,
		Kind:      req.Kind,
		Timestamp: start,
	}

	// ── 1. Capability policy ─────────────────────────────────────────────────
	if d.CheckPolicy(req) == PolicyDeny {
		var required Capability
		if req.Kind == HCKindTool {
			required = toolCapability(req.ToolName)
		} else {
			required = kindCapability[req.Kind]
		}
		tel.Success = false
		tel.Lane = "denied"
		tel.LaneKey = fmt.Sprintf("%s|denied|policy", req.Kind)
		d.emitTelemetry(tel, start)
		return "", true, fmt.Errorf("capability %q not granted to extension %q", required, d.extName)
	}

	// ── 2. Deduplication ────────────────────────────────────────────────────
	if result, isError, ok := d.dedupLookup(req.DedupKey); ok {
		tel.OptHit = true
		tel.Lane = "dedup"
		tel.LaneKey = fmt.Sprintf("%s|dedup|%s", req.Kind, laneKeyDomain(req))
		tel.Success = !isError
		d.emitTelemetry(tel, start)
		return result, isError, nil
	}

	// ── 3. Adaptive mode selection ──────────────────────────────────────────
	d.updateMode()

	// ── 4. Lane selection and dispatch ────────────────────────────────────
	useFast := d.fastLaneOK.Load() && d.mode == ModeSequentialFastPath
	var result string
	var isError bool
	var execErr error

	if useFast {
		tel.Lane = "fast"
		tel.LaneKey = fmt.Sprintf("%s|%s.%s|%s", req.Kind, req.Kind, req.ToolName, laneKeyDomain(req))
		tel.MarshalPath = "direct"
		result, isError, execErr = d.fastDispatch(ctx, req)

		// Shadow check: run compat lane too and compare fingerprints.
		if execErr == nil && isReadOnlyCall(req) && rand.Float64() < d.shadowRate {
			d.runShadow(ctx, req, result)
		}
	} else {
		tel.Lane = "compat"
		tel.LaneKey = fmt.Sprintf("%s|fallback|%s", req.Kind, laneKeyDomain(req))
		tel.MarshalPath = "marshal"
		if !useFast && d.fastLaneOK.Load() {
			tel.FallbackReason = "interleaved_batching_mode"
		} else {
			tel.FallbackReason = "fast_lane_backed_off"
		}
		result, isError, execErr = d.compatDispatch(ctx, req)
	}

	tel.Success = execErr == nil && !isError

	// ── 5. Store in dedup cache for read-only calls ───────────────────────
	if execErr == nil && isReadOnlyCall(req) {
		d.dedupStore(req.DedupKey, result, isError)
	}

	d.emitTelemetry(tel, start)
	return result, isError, execErr
}

// fastDispatch is the low-allocation path: skips JSON re-serialization when
// the payload is already a json.RawMessage, avoiding an extra marshal round-trip.
func (d *HostcallDispatcher) fastDispatch(ctx context.Context, req HostcallRequest) (string, bool, error) {
	switch req.Kind {
	case HCKindTool:
		if raw, ok := req.Payload.(json.RawMessage); ok {
			return dispatchToolRaw(ctx, req.ToolName, raw)
		}
		return dispatchTool(ctx, req.ToolName, req.Payload)
	case HCKindLog:
		return logHostcall(req.Payload), false, nil
	case HCKindEnv:
		return envHostcall(req.Payload), false, nil
	default:
		return d.compatDispatch(ctx, req)
	}
}

// compatDispatch is the full marshal/unmarshal path: always serializes payload
// through JSON for canonical validation before dispatch.  Used as the fallback
// lane and for shadow comparison.
func (d *HostcallDispatcher) compatDispatch(ctx context.Context, req HostcallRequest) (string, bool, error) {
	switch req.Kind {
	case HCKindTool:
		raw, err := json.Marshal(req.Payload)
		if err != nil {
			return "", true, fmt.Errorf("marshal payload for %q: %w", req.ToolName, err)
		}
		return dispatchToolRaw(ctx, req.ToolName, raw)
	case HCKindLog:
		return logHostcall(req.Payload), false, nil
	case HCKindEnv:
		return envHostcall(req.Payload), false, nil
	default:
		return "", true, fmt.Errorf("hostcall kind %q not implemented", req.Kind)
	}
}

// runShadow executes req on the compat lane and compares the fingerprint to
// fastResult. Divergence increments the counter; exceeding shadowBudget backs
// off the fast lane.  Divergences are emitted via slog for structured capture.
func (d *HostcallDispatcher) runShadow(ctx context.Context, req HostcallRequest, fastResult string) {
	compatResult, _, err := d.compatDispatch(ctx, req)
	if err != nil {
		return
	}
	if sha256Hex(fastResult) != sha256Hex(compatResult) {
		d.shadowCount++
		slog.Info("ext:shadow divergence",
			"count", d.shadowCount,
			"budget", d.shadowBudget,
			"call_id", req.CallID,
			"ext", d.extName,
		)
		if d.shadowCount >= d.shadowBudget {
			d.fastLaneOK.Store(false)
			slog.Warn("ext:shadow fast lane disabled",
				"divergences", d.shadowCount,
				"ext", d.extName,
			)
		}
	}
}

// updateMode switches between sequential_fast_path and interleaved_batching
// based on queue pressure. Called before lane selection.
func (d *HostcallDispatcher) updateMode() {
	qlen := d.queueLen.Load()
	d.modeMu.Lock()
	defer d.modeMu.Unlock()
	if qlen > 4 && d.mode == ModeSequentialFastPath {
		d.mode = ModeInterleavedBatch
	} else if qlen <= 1 && d.mode == ModeInterleavedBatch {
		d.mode = ModeSequentialFastPath
	}
}

// Telemetry returns a snapshot of the telemetry ring buffer.
func (d *HostcallDispatcher) Telemetry() []HostcallTelemetry {
	d.telMu.Lock()
	defer d.telMu.Unlock()
	out := make([]HostcallTelemetry, len(d.telBuf))
	copy(out, d.telBuf)
	return out
}

func (d *HostcallDispatcher) emitTelemetry(tel HostcallTelemetry, start time.Time) {
	tel.DispatchLatency = time.Since(start)
	d.telMu.Lock()
	defer d.telMu.Unlock()
	if len(d.telBuf) >= telBufCap {
		copy(d.telBuf, d.telBuf[1:])
		d.telBuf = d.telBuf[:telBufCap-1]
	}
	d.telBuf = append(d.telBuf, tel)
}

func (d *HostcallDispatcher) dedupLookup(key string) (string, bool, bool) {
	d.dedupMu.Lock()
	defer d.dedupMu.Unlock()
	e, ok := d.dedupCache[key]
	if !ok || time.Now().After(e.expires) {
		delete(d.dedupCache, key)
		return "", false, false
	}
	return e.result, e.isError, true
}

func (d *HostcallDispatcher) dedupStore(key, result string, isError bool) {
	d.dedupMu.Lock()
	defer d.dedupMu.Unlock()
	d.dedupCache[key] = dedupEntry{
		result:  result,
		isError: isError,
		expires: time.Now().Add(dedupWindow),
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// dispatchToolRaw calls a registered tool with a pre-serialized JSON payload,
// skipping any marshal step.  Used by the fast lane when payload is already
// a json.RawMessage, and by the compat lane after it has marshaled the payload.
func dispatchToolRaw(ctx context.Context, name string, raw json.RawMessage) (string, bool, error) {
	t, ok := tools.Get(name)
	if !ok {
		return "", true, fmt.Errorf("tool %q not found", name)
	}
	res, err := t.Execute(ctx, raw)
	if err != nil {
		return "", true, err
	}
	var sb strings.Builder
	for _, block := range res.Content {
		sb.WriteString(block.Text)
	}
	return sb.String(), res.IsError, nil
}

// dispatchTool marshals payload to JSON and then calls dispatchToolRaw.
// Used by the fast lane when payload is not already a json.RawMessage.
func dispatchTool(ctx context.Context, name string, payload any) (string, bool, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", true, fmt.Errorf("marshal params for %q: %w", name, err)
	}
	return dispatchToolRaw(ctx, name, raw)
}

func logHostcall(payload any) string {
	switch v := payload.(type) {
	case string:
		fmt.Printf("[ext:log] %s\n", v)
		return ""
	default:
		b, _ := json.Marshal(v)
		fmt.Printf("[ext:log] %s\n", b)
		return ""
	}
}

func envHostcall(payload any) string {
	key, _ := payload.(string)
	if key == "" {
		return ""
	}
	if IsEnvBlocked(key) {
		return "" // blocked; return empty rather than error
	}
	return os.Getenv(key)
}

func isReadOnlyCall(req HostcallRequest) bool {
	if req.Kind == HCKindTool {
		return isReadOnlyTool(req.ToolName)
	}
	return req.Kind == HCKindEnv || req.Kind == HCKindLog
}

func laneKeyDomain(req HostcallRequest) string {
	switch req.Kind {
	case HCKindTool:
		return toolDomain(req.ToolName)
	case HCKindHTTP:
		return "network"
	case HCKindExec:
		return "exec"
	case HCKindEnv:
		return "env"
	default:
		return "meta"
	}
}

// canonicalHash returns a stable SHA-256 hex fingerprint of (kind, toolName, payload).
// Object keys in payload are sorted for determinism.
func canonicalHash(kind HostcallKind, toolName string, payload any) string {
	type canonical struct {
		Kind     HostcallKind    `json:"k"`
		ToolName string          `json:"t,omitempty"`
		Payload  json.RawMessage `json:"p,omitempty"`
	}
	raw, _ := json.Marshal(sortedPayload(payload))
	c := canonical{Kind: kind, ToolName: toolName, Payload: raw}
	b, _ := json.Marshal(c)
	return sha256Hex(string(b))
}

// sortedPayload recursively sorts map keys for canonical serialization.
func sortedPayload(v any) any {
	switch m := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		ordered := make(map[string]any, len(m))
		for _, k := range keys {
			ordered[k] = sortedPayload(m[k])
		}
		return ordered
	case []any:
		for i, item := range m {
			m[i] = sortedPayload(item)
		}
		return m
	default:
		return v
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", sum)
}
