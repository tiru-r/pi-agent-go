# pi

A Zed-native AI coding agent powered by [OpenRouter](https://openrouter.ai). Pi runs as a native language model provider inside Zed via the Agent Client Protocol (ACP) — giving Zed access to every model OpenRouter offers (500+), with a full agentic loop and built-in tools.

---

## Features

- **Every OpenRouter model** — model list fetched live from the API; no hardcoded registry
- **8 built-in tools** — read, write, edit, bash, grep, find, ls, hashline_edit
- **Full agentic loop** — LLM → tools → LLM cycles inside Zed's chat panel
- **Session memory** — multi-turn conversation history maintained per Zed session
- **Runtime intelligence** — CUSUM+BOCPD regime detection, conformal anomaly gating, PAC-Bayes safety bounds, off-policy evaluation, VOI experiment scheduling, weighted attribution, and OCO control (see [Runtime Intelligence](#runtime-intelligence))
- **One API key** — `OPENROUTER_API_KEY` is all you need
- **Tiny binary** — 3 direct dependencies (cobra, uuid, sqlite)

---

## Installation

### One-liner

```bash
curl -fsSL https://raw.githubusercontent.com/tiru-r/pi-agent-go/main/install.sh | bash
```

Requires Go 1.23+. Installs to `~/.local/bin/pi`.

```bash
./install.sh --system        # /usr/local/bin (needs sudo)
./install.sh --dest ~/bin    # custom directory
```

### Manual build

```bash
git clone https://github.com/tiru-r/pi-agent-go
cd pi-agent-go
go build -ldflags "-s -w -X github.com/tiru-r/pi-agent-go/internal/cli.Version=$(git describe --tags --always)" -o pi ./cmd/pi/
mv pi ~/.local/bin/
```

### Uninstall

```bash
curl -fsSL https://raw.githubusercontent.com/tiru-r/pi-agent-go/main/uninstall.sh | bash
./uninstall.sh --purge   # also removes config and session data
```

---

## Quick start

```bash
# Set your OpenRouter key
export OPENROUTER_API_KEY=sk-or-...
# or persist it:
pi auth set sk-or-...

# One-shot agent run
pi run "Explain this codebase"
pi run --model deepseek/deepseek-r1 "Solve this bug"

# List every model OpenRouter offers
pi models

# Run the Zed ACP server directly (Zed calls this automatically)
pi acp
```

---

## Zed integration

Pi registers as a native agent server. Zed invokes `pi acp` as a subprocess and communicates over stdin/stdout via JSON-RPC 2.0 using the Agent Client Protocol (ACP).

### Zed `settings.json`

```json
{
  "agent_servers": {
    "pi": {
      "type": "custom",
      "command": "pi",
      "args": ["acp"],
      "env": {
        "OPENROUTER_API_KEY": "sk-or-..."
      }
    }
  }
}
```

### What you get in Zed's panel

- **Model picker** — all 500+ OpenRouter models, populated from the live API on startup
- **Thinking level** — Off / Auto / Full selector for extended reasoning (works with Claude 3.7+, DeepSeek R1, QwQ, etc.)
- **Full agentic loop** — pi runs tools (read, write, bash, …) across multiple turns before returning

### Session flow

When Zed sends `session/prompt`, pi:
1. Looks up the session's current model and thinking level
2. Loads conversation history for the session
3. Runs the full agentic loop — LLM calls tools, feeds results back, repeats
4. Streams text chunks (`agent_message_chunk`) and thinking chunks (`agent_thought_chunk`) to Zed via `session/update` notifications
5. Saves updated history for the next turn

Changing the model or thinking level in Zed's panel triggers `session/set_config_option` or `session/set_model`, which pi applies to all subsequent prompts in that session.

### Debug logging

```bash
PI_DEBUG=1 pi acp    # writes verbose logs to ~/.pi/agent/acp.log
```

### Protocol summary

```
Zed → pi:  initialize, session/new, session/prompt, session/cancel,
           session/set_config_option, session/set_model, session/close
pi → Zed:  initialize result (agentInfo), session/new result (configOptions + models),
           session/update notifications (agent_message_chunk, agent_thought_chunk),
           session/prompt result (stopReason, usage)

Internal:  runtime/report  →  RuntimeReport JSON (regime, anomaly, OPE, attribution, …)
```

---

## Runtime Intelligence

Pi's `internal/runtime` package implements seven math-driven decision systems that run continuously alongside every agent session. The goal is safer policy decisions, faster recovery from workload shifts, and more trustworthy performance attribution — not formulas in docs.

### Regime-Shift Detection — CUSUM + BOCPD

Two complementary detectors run on every LLM and tool latency sample:

- **CUSUM** (two-sided, k=0.5, h=5.0) catches *persistent drift* — a slow mean shift that accumulates over time.
- **BOCPD** (Adams & MacKay 2007, λ=50) catches *sudden regime changes* — a spike in P(r=0|x₁:t) without brittle fixed thresholds. Uses a Normal-Gamma conjugate prior with log-space run-length posteriors pruned to 500 hypotheses.

`RegimeDetector` fires when either detector alarms and exposes the BOCPD posterior mode as the estimated change point.

### Conformal Prediction Envelope

A sliding window (n=200) of nonconformity scores |xₜ − μₜ| provides an *adaptive* anomaly threshold:

```
q = score[⌈(n+1)·0.95⌉−1]     anomaly if |xₜ − μₜ| > q
```

The threshold tightens automatically when behavior stabilises and widens when variance is high — no static latency cutoff.

### PAC-Bayes Safety Bound

Before allowing aggressive policy moves, the safety envelope computes a PAC-Bayes-kl upper bound on the true error rate:

```
kl(q̂, q_bound) ≤ (KL(Q‖P) + ln(2√n/δ)) / n
```

Solved via bisection. `PACBayesSafety.Veto(maxErr)` returns true — failing closed — when the upper bound exceeds `maxErr`.

### Off-Policy Evaluation — IPS / WIS / DR + ESS + Regret Gate

Candidate policy changes are evaluated from trace data before being applied:

```
wᵢ = π(aᵢ|xᵢ) / μ(aᵢ|xᵢ)          (clipped to [0, 20])
V̂_IPS = (1/n) Σ wᵢrᵢ
V̂_WIS = Σ wᵢrᵢ / Σ wᵢ
V̂_DR  = (1/n) Σ (r̂ᵢ + wᵢ(rᵢ − r̂ᵢ))
N_eff  = (Σ wᵢ)² / Σ wᵢ²
Δ_regret = r̄_baseline − V̂_DR
```

`ShouldVeto` returns true if N_eff < 5 (insufficient support) or regret exceeds threshold.

### VOI-Driven Experiment Selection

The VOI planner schedules diagnostic probes by highest expected value per unit cost:

```
priorityᵢ ∝ utilityᵢ / overheadᵢ
```

Only stale probes (TTL expired) within the overhead budget are considered. `Next(budget)` returns the best candidate or nil.

### Weighted Bottleneck Attribution

Per-stage latency is weighted by session message count to reflect realistic workload distribution:

```
weighted_contribution_s = (Σ wᵢ·mᵢ,s) / (Σ wᵢ·tᵢ) · 100     wᵢ = session_messages
n_eff = (Σ wᵢ)² / Σ wᵢ²
CI₉₅  = μ ± 1.96 · √(σ²_w / n_eff)
```

`AttributionTracker.Report()` returns per-stage shares with 95% confidence intervals — ranking optimisation work by actual end-to-end impact.

### Online Convex Control + Regret Rollback

A continuous parameter tuner (controlling e.g. concurrency or timeout scaling) adapts via projected gradient descent:

```
τₜ₊₁ = clip(τₜ − η·∇L, τ_min, τ_max)
```

If instantaneous loss exceeds the rollback threshold, `τ` reverts immediately to the last known safe value. Default: τ ∈ [0.1, 10], η=0.05, rollback at loss > 2.0.

### Summary

| Subsystem | File | Trigger |
|---|---|---|
| CUSUM + BOCPD | `runtime/detector.go` | Every LLM/tool latency sample |
| Conformal envelope | `runtime/conformal.go` | Every latency sample |
| PAC-Bayes safety | `runtime/safety.go` | Every success/error event |
| IPS/WIS/DR + ESS | `runtime/ope.go` | Every logged policy trace |
| VOI planner | `runtime/voi.go` | On-demand probe selection |
| Weighted attribution | `runtime/attribution.go` | Rolled up on `Report()` |
| OCO controller | `runtime/controller.go` | Every latency sample |

All subsystems are wired through `runtime.Monitor`, attached to the ACP server, and fed from `session_agent.go`. The full report is available via the `runtime/report` internal ACP method.

---

## CLI reference

### Global flags

| Flag | Short | Description |
|---|---|---|
| `--model` | `-m` | OpenRouter model ID override |
| `--system` | | Override system prompt |
| `--session` | `-s` | Session ID to continue |
| `--no-session` | | Disable session persistence |
| `--think` | | Enable extended thinking |
| `--debug` | | Enable debug logging |

### Commands

#### `pi run` — One-shot agent

```bash
pi run "Refactor this module to use interfaces"
pi run --model qwen/qwq-32b "Solve this algorithmic problem"
```

Streams the full agentic response (including tool output) to stdout.

#### `pi session` — Session management

```bash
pi session list                  # List all sessions
pi session show <id>             # Print session messages to stdout
pi session delete <id>           # Delete (prompts for confirmation)
pi session delete <id> --force   # Delete without confirmation
```

#### `pi auth` — API key

```bash
pi auth set sk-or-...    # Store OpenRouter key in settings.json
pi auth status           # Show key (masked)
```

#### `pi models` — Live model list

```bash
pi models                # Fetches from OpenRouter and prints all available models
```

#### `pi config` — Configuration

```bash
pi config show
pi config set model      anthropic/claude-sonnet-4-6
pi config set model      deepseek/deepseek-r1
pi config set thinking_level auto
pi config set system_prompt "You are an expert Go developer"
```

#### `pi doctor` — Health check

```bash
pi doctor    # Checks config, session dir, and OPENROUTER_API_KEY
```

#### `pi acp` — Zed ACP server

```bash
pi acp    # Start the ACP server (Zed calls this automatically)
```

#### `pi version`

```bash
pi version
```

---

## Built-in tools

The agent has 8 tools for interacting with the filesystem and shell.

### `read`
```json
{ "path": "src/main.go", "offset": 50, "limit": 100 }
```
Returns file contents with line numbers. Detects images by extension and returns base64.

### `write`
```json
{ "path": "src/utils.go", "content": "package main\n..." }
```
Creates parent directories as needed.

### `edit`
```json
{ "path": "main.go", "old_string": "fmt.Println", "new_string": "log.Println", "replace_all": false }
```
Fails if `old_string` is not found or appears more than once (when `replace_all` is false).

### `bash`
```json
{ "command": "go test ./...", "timeout": 60000 }
```
Timeout in ms (default 120,000). Output capped at 100 KB. Combined stdout + stderr.

### `grep`
```json
{ "pattern": "func.*Handler", "path": ".", "context": 3 }
```
Skips `.git/`, `node_modules/`, `target/`. Max 100 matches.

### `find`
```json
{ "path": ".", "pattern": "*.go", "type": "f", "max_depth": 3 }
```
`type`: `f` (file), `d` (dir), `l` (symlink). Max 1000 results.

### `ls`
```json
{ "path": "internal/provider" }
```
Returns entries with type and size. Max 500 entries.

### `hashline_edit`
```json
{
  "path": "main.go",
  "edits": [{ "line_hash": "42#a3f91c", "new_content": "    return nil" }]
}
```
`line_hash` format: `LINE#HASH` where HASH is the first 6 hex chars of SHA-256 of the line. More precise than string matching — survives surrounding edits.

---

## Configuration

Settings file: `~/.pi/agent/settings.json` (or `$PI_CONFIG`).

```json
{
  "openrouter_api_key": "sk-or-...",
  "model": "tencent/hy3-preview:free",
  "max_tokens": 8096,
  "thinking_level": "off",
  "system_prompt": "",
  "session_dir": "~/.pi/agent/sessions",
  "sqlite": true
}
```

### Environment variables

| Variable | Description |
|---|---|
| `OPENROUTER_API_KEY` | OpenRouter API key |
| `PI_MODEL` | Override model |
| `PI_CONFIG` | Custom config file path |
| `PI_DEBUG` | Set to any value to enable debug logging to `~/.pi/agent/acp.log` |

---

## Sessions

Sessions live in `~/.pi/agent/sessions/` as `.jsonl` files (one JSON object per line, version 3 format). A SQLite index (`index.db`) is maintained alongside for fast listing.

### Context compaction

When conversation history grows large, pi automatically summarises older messages and continues from the trimmed context.

---

## Architecture

```
cmd/pi/main.go
internal/
├── cli/root.go              Cobra command tree
├── config/config.go         Settings (file + env)
├── model/
│   ├── message.go           Message / ContentBlock / Usage types
│   └── registry.go          ModelInfo struct (populated from OpenRouter API)
├── provider/
│   ├── provider.go          Provider interface + Request / Event types
│   ├── factory/factory.go   Builds the OpenRouter provider from config
│   └── openrouter/
│       ├── openrouter.go    OpenRouter streaming (OpenAI-compatible SSE)
│       └── models.go        Live model list from /api/v1/models
├── agent/
│   ├── agent.go             Core agentic loop (stream → tools → stream)
│   ├── session_agent.go     Session-aware wrapper (feeds runtime.Monitor)
│   └── compaction.go        Context compaction
├── acp/acp.go               Zed ACP server (JSON-RPC 2.0 over stdio)
├── runtime/
│   ├── metrics.go           Shared types (Observation, PolicyTrace, RuntimeReport)
│   ├── detector.go          CUSUM + BOCPD regime-shift detection
│   ├── conformal.go         Conformal prediction anomaly envelope
│   ├── safety.go            PAC-Bayes-kl safety bound + veto
│   ├── ope.go               Off-policy evaluator (IPS/WIS/DR + ESS + regret gate)
│   ├── voi.go               VOI-driven experiment scheduler
│   ├── attribution.go       Weighted bottleneck attribution + CI₉₅
│   ├── controller.go        OCO online controller with rollback
│   └── monitor.go           Top-level Monitor wiring all subsystems
├── session/                 JSONL + SQLite persistence
├── tools/tools.go           8 built-in tools
├── httpclient/client.go     HTTP client (streaming + non-streaming)
├── sse/sse.go               SSE parser
└── doctor/doctor.go         Health checks
```

### Design notes

**Single provider.** Everything routes through OpenRouter. One interface, one streaming implementation, one API key.

**Live model list.** `FetchModels()` calls `GET /api/v1/models` on startup. No hardcoded model IDs anywhere. New models on OpenRouter appear in Zed's model picker automatically.

**New ACP protocol.** Pi implements Zed's `agent_servers` ACP (not the older `language_models` protocol). Session state tracks the active model and thinking level per conversation; Zed's UI controls drive both via `session/set_config_option` and `session/set_model`.

**Minimal dependencies.** 3 direct deps: `cobra` (CLI), `uuid` (session IDs), `sqlite` (session index). The runtime intelligence package uses only stdlib (`math`, `sort`, `sync`).

**Tool execution is parallel.** All tool calls from a single assistant turn run concurrently (capped at 4 goroutines), results fed back in one user turn.

---

## Building for release

```bash
VERSION=$(git describe --tags --always --dirty)
LDFLAG="-s -w -X github.com/tiru-r/pi-agent-go/internal/cli.Version=${VERSION}"

go build -ldflags "$LDFLAG" -o pi ./cmd/pi/

# Cross-compile
GOOS=linux   GOARCH=amd64 go build -ldflags "$LDFLAG" -o pi-linux-amd64    ./cmd/pi/
GOOS=linux   GOARCH=arm64 go build -ldflags "$LDFLAG" -o pi-linux-arm64    ./cmd/pi/
GOOS=darwin  GOARCH=arm64 go build -ldflags "$LDFLAG" -o pi-darwin-arm64   ./cmd/pi/
GOOS=windows GOARCH=amd64 go build -ldflags "$LDFLAG" -o pi-windows-amd64.exe ./cmd/pi/
```

---

| | |
|---|---|
| Go files | 30 |
| Lines of code | ~6,400 |
| Providers | 1 (OpenRouter) |
| Models | 500+ (live from API) |
| Built-in tools | 8 |
| Runtime subsystems | 7 |
| Direct dependencies | 3 |
| Go version | 1.23+ |
