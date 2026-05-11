# pi

A Zed-native AI coding agent powered by [OpenRouter](https://openrouter.ai). Pi runs as a native language model provider inside Zed via the Agent Client Protocol (ACP) — giving Zed access to every model OpenRouter offers (500+), with a full agentic loop and built-in tools.

---

## Features

- **Every OpenRouter model** — model list fetched live from the API; no hardcoded registry
- **8 built-in tools** — read, write, edit, bash, grep, find, ls, hashline_edit
- **Full agentic loop** — LLM → tools → LLM cycles inside Zed's chat panel
- **Session memory** — multi-turn conversation history maintained per Zed session
- **Runtime intelligence** — 13 math-driven subsystems covering observability, safety, planning, and agent protocol (see [Runtime Intelligence](#runtime-intelligence))
- **JavaScript + native extensions** — load custom tools from `~/.pi/extensions/` (goja JS VM or subprocess JSON protocol)
- **One API key** — `OPENROUTER_API_KEY` is all you need
- **Tiny binary** — 4 direct dependencies (cobra, uuid, sqlite, goja)

---

## Installation

### One-liner

```bash
curl -fsSL https://raw.githubusercontent.com/tiru-r/pi-agent-go/main/install.sh | bash
```

Requires Go 1.24+. Installs to `~/.local/bin/pi`.

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
- **Thinking level** — Off / Minimal / Low / Medium / High / Max selector for extended reasoning (works with Claude 3.7+, Claude 4, DeepSeek R1, QwQ, and any `:thinking`-suffixed model)
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

Pi's `internal/runtime` package implements 13 math-driven subsystems that run continuously alongside every agent session. They are grouped into four concern areas: observability, safety, planning, and agent protocol.

### Observability

#### Regime-shift detection — CUSUM + BOCPD

Two complementary detectors run on every LLM and tool latency sample:

- **CUSUM** (two-sided, k=0.5, h=5.0) catches *persistent drift* — a slow mean shift that accumulates over time.
- **BOCPD** (Adams & MacKay 2007, λ=50) catches *sudden regime changes* — a spike in P(r=0|x₁:t) without brittle fixed thresholds. Uses a Normal-Gamma conjugate prior with log-space run-length posteriors pruned to 500 hypotheses.

Output drift runs a parallel pair of CUSUM detectors on response token length and tool call rate, independent of latency, to catch model behavior changes.

#### HDR histogram — tail latency

Per-stage and global HDR histograms with 200 log-spaced bins covering 100 µs–30 s track p50, p95, p99, and p999 latency without allocating per-observation memory. Means cannot hide tail behavior that affects the interactive experience of a coding agent.

#### Reservoir sampling — unbiased attribution

`AttributionTracker` uses Algorithm R reservoir sampling (n=1000) instead of a FIFO ring buffer. Under high tool-call load, every stage gets a statistically uniform random sample regardless of arrival order — not a recency-biased window skewed toward the most recent stage.

#### Shapley-value attribution

`ShapleyReport()` computes each stage's Shapley value — its average marginal contribution to total latency across all insertion orderings. For uncorrelated stages the Shapley value equals the weighted latency share (fast path). For correlated stages (same weight bucket) it uses 200 random coalition samples. Shapley values sum to 1.0 and give a fair blame decomposition in the presence of correlated tool chains.

#### Expected calibration error

`ECECalibrator` bins (probability, outcome) pairs into 10 equal-width buckets and computes:

```
ECE = Σ_b |acc(b) − conf(b)| · |b| / n
```

ECE close to 0 means the agent's confidence scores predict outcomes accurately. Rising ECE indicates miscalibrated probability estimates that cannot be trusted for downstream safety decisions.

### Safety & Reliability

#### Conformal prediction envelope

A sliding window (n=200) of nonconformity scores |xₜ − μₜ| provides an *adaptive* anomaly threshold:

```
q = score[⌈(n+1)·0.95⌉−1]     anomaly if |xₜ − μₜ| > q
```

The threshold tightens automatically when behavior stabilises and widens when variance is high — no static latency cutoff.

#### Conformal quantile UQ

`ConformalPredictor` maintains a 500-point sliding window of calibration residuals and returns distribution-free 95% prediction intervals for the next latency observation:

```
q̂ = sorted_residuals[⌈(n+1)·0.95⌉−1]
interval = [center − q̂,  center + q̂]
```

This is model-free: it makes no parametric assumption about the latency distribution, unlike the PAC-Bayes bound which requires conjugate priors.

#### PAC-Bayes safety bound

Before allowing aggressive policy moves, the safety envelope computes a PAC-Bayes-kl upper bound on the true error rate:

```
kl(q̂, q_bound) ≤ (KL(Q‖P) + ln(2√n/δ)) / n
```

Solved via bisection. `PACBayesSafety.Veto(maxErr)` returns true — failing closed — when the upper bound exceeds `maxErr`.

#### Circuit breaker

A `CircuitBreaker` per stage (keyed by "llm", "tool:bash", etc.) prevents cascade failure when a backend degrades:

- **CLOSED** — normal; failures accumulate toward threshold (default 5)
- **OPEN** — fast-fail for `timeout` (default 30 s); no requests forwarded
- **HALF-OPEN** — one probe allowed; consecutive successes (default 2) close the circuit

The breaker is updated on every `Monitor.Observe()` call alongside the latency subsystems. `Monitor.CircuitBreakerFor(stage).Allow()` lets callers gate dispatch before sending work.

#### Off-policy evaluation — IPS / WIS / DR + ESS + regret gate

Candidate policy changes are evaluated from trace data before being applied:

```
wᵢ = π(aᵢ|xᵢ) / μ(aᵢ|xᵢ)          (clipped to [0, 20])
V̂_IPS = (1/n) Σ wᵢrᵢ
V̂_WIS = Σ wᵢrᵢ / Σ wᵢ
V̂_DR  = (1/n) Σ (r̂ᵢ + wᵢ(rᵢ − r̂ᵢ))
N_eff  = (Σ wᵢ)² / Σ wᵢ²
median_regret = median(r̄ − V̂_IPS, r̄ − V̂_WIS, r̄ − V̂_DR)
```

Median regret across all three estimators is used for the veto decision; a single mis-specified estimator cannot trigger or suppress it alone.

#### Token-budget guardrail

`Agent.TokenBudget` enforces a hard ceiling on cumulative `InputTokens` per session run. `Options.TurnTokenBudget` adds an independent per-turn ceiling. When either limit is exceeded the agent emits `EventKindError` and returns immediately — preventing runaway cost in long Zed sessions.

### Planning & Optimization

#### Thompson sampling / Bayesian bandit

Each registered probe carries a Beta(α, β) posterior over its utility. `UpdateUtility` shifts the posterior: high observed learning increments α, low increments β. Probe selection in `Plan` and `Next` draws a sample from each Beta distribution via the regularised incomplete beta inverse CDF (Newton–Raphson with Lentz continued fraction), multiplied by 1/overhead. This gives uncertainty-aware exploration: stale or under-tried probes get a chance even when their mean utility is lower than an established probe's.

DAgger-style policy transfer runs in `MarkRun`: when a probe's utility significantly exceeds its Beta prior mean plus one standard deviation, all other probe utilities are nudged upward by δ — a lightweight imitation of the discovered superior policy.

#### MCTS probe planning

`Monitor.PlanProbes(budget)` uses UCB1 Monte Carlo Tree Search when two or more stale probes are available:

```
UCB1(node) = Q(s,a)/N(s,a) + C·√(ln N(s) / N(s,a))     C = 1.414
```

State = (remaining budget, set of run probes). Action = run next probe. Reward = cumulative utility. The MCTS runs 500 iterations by default and returns probes in priority order. It degrades gracefully to the greedy VOI plan when only one stale probe exists.

#### Online convex control + regret rollback

A continuous parameter tuner adapts routing weights, batch budgets, and backoff factors per stage via projected gradient descent with oscillation damping:

```
τₜ₊₁ = clip(τₜ − η_eff·∇L, τ_min, τ_max)
```

The effective step size η_eff is halved for each gradient sign flip above threshold, preventing the controller from hunting. Starvation recovery boosts weights when a stage has low queue depth and low error rate simultaneously.

### Agent Protocol

#### Semantic cache

`SemanticCache` caches (prompt → response) pairs using Jaccard similarity over character 3-grams as a semantic proxy:

```
J(A, B) = |A ∩ B| / |A ∪ B|     A, B = sets of char 3-grams
```

A lookup returns a cached response when the best-match similarity exceeds the threshold (default 0.85). LRU eviction at 512 entries. The cache is wired into the OpenRouter provider; tool-use requests are never cached (their results depend on live filesystem/shell state).

#### Information-theoretic compaction

Context compaction uses TF-IDF term overlap with the system prompt as an MI proxy to rank messages by informativeness. `findCutPointMI` keeps the highest-MI messages up to the token budget rather than simply dropping the oldest ones, then aligns the cut to a clean assistant→user turn boundary. This preserves high-information error traces and failed tool calls that a recency-based cut would discard precisely when they are most needed.

#### Trace distillation

`session.Distill` reads all sessions from SQLite, scores each by a quality heuristic — penalising tool errors, rewarding natural stops and session conciseness — and exports high-quality traces as JSONL for offline fine-tuning.

The OpenRouter provider accepts a `DistillSink` for online knowledge distillation data collection: every completed non-tool response is passed as a (prompt, model, response, timestamp) record to the sink, which appends it as JSONL.

### Summary

| Subsystem | File | Concern |
|---|---|---|
| CUSUM + BOCPD + output drift | `runtime/detector.go` | Observability |
| HDR histogram (p50/p95/p99/p999) | `runtime/histogram.go` | Observability |
| Reservoir sampling attribution | `runtime/attribution.go` | Observability |
| Shapley-value attribution | `runtime/attribution.go` | Observability |
| ECE calibration error | `runtime/ece.go` | Observability |
| Conformal anomaly envelope | `runtime/conformal.go` | Safety |
| Conformal quantile UQ | `runtime/quantile.go` | Safety |
| PAC-Bayes safety bound | `runtime/safety.go` | Safety |
| Circuit breaker (per stage) | `runtime/circuitbreaker.go` | Safety |
| IPS/WIS/DR OPE + regret gate | `runtime/ope.go` | Safety |
| Token-budget guardrail | `agent/agent.go` | Safety |
| Thompson sampling / Bayesian bandit | `runtime/voi.go` | Planning |
| MCTS probe planning | `runtime/mcts.go` | Planning |
| OCO controller + rollback | `runtime/controller.go` | Planning |
| Semantic cache | `runtime/semantic_cache.go` | Agent protocol |
| MI-based context compaction | `agent/compaction.go` | Agent protocol |
| Trace + knowledge distillation | `session/distill.go` | Agent protocol |

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
pi config set thinking_level off       # off | minimal | low | medium | high | xhigh
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

## Extensions

Pi loads custom tools from `~/.pi/extensions/` on startup. Two types are supported.

### JavaScript extensions (`.js`)

Written in plain JS and executed in a pure-Go goja VM (no Node.js required):

```js
// ~/.pi/extensions/my_tool.js
pi.tool("fetch_url", "Fetch a URL and return the body", {
  type: "object",
  properties: { url: { type: "string" } },
  required: ["url"]
}, async (params) => {
  const res = await pi.http({ url: params.url, method: "GET" });
  return res.body;
});
```

Available host APIs: `pi.tool()`, `pi.http()`, `pi.exec()`, `pi.env()`, `pi.session()`, `pi.log()`

Hooks run before and after each tool call (`before_tool`, `after_tool`). Pi applies safe auto-repairs for common JS mistakes (forbidden patterns, unavailable imports) before loading.

### Native extensions (`.json`)

Any subprocess that speaks a simple JSON protocol:

```json
{
  "name": "my_tool",
  "description": "Run my CLI",
  "command": ["my-cli", "--json"],
  "schema": { "type": "object", "properties": { "arg": { "type": "string" } } }
}
```

Pi sends tool params as JSON on stdin; the process replies with `{"content": "...", "is_error": false}` on stdout.

### Extension directory

```bash
PI_EXTENSIONS_DIR=~/.pi/extensions    # default location (also configurable in settings.json)
```

Extensions are discovered automatically at `pi run` and `pi acp` startup. A trust registry allows/quarantines extensions by name.

---

## Configuration

Settings file: `~/.pi/agent/settings.json` (or `$PI_CONFIG`).

```json
{
  "openrouter_api_key": "sk-or-...",
  "openrouter_site_url": "https://mysite.com",
  "openrouter_app_name": "My App",
  "model": "openai/gpt-oss-120b:free",
  "max_tokens": 8096,
  "temperature": 0.7,
  "thinking_level": "off",
  "system_prompt": "",
  "session_dir": "~/.pi/agent/sessions",
  "sqlite": true
}
```

`openrouter_site_url` and `openrouter_app_name` are optional and appear on your OpenRouter dashboard.

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

When conversation history grows large, pi automatically compacts older messages. The compaction algorithm scores each message by TF-IDF term overlap with the system prompt (as an MI proxy) and keeps the highest-information messages up to the token budget — preserving error traces and failed tool calls that a recency-only cut would drop.

### Trace distillation

```bash
# Export high-quality sessions as JSONL for fine-tuning (score ≥ 0.7)
session.Distill(store, "distill-out.jsonl", 0.7)
```

Quality score: 1.0 for clean sessions ending with a natural stop; penalised for tool errors and verbose turn counts. The OpenRouter provider can optionally write every completed response to a `JSONLDistillSink` for knowledge distillation data collection.

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
│       ├── openrouter.go    OpenRouter streaming + semantic cache + distill sink
│       └── models.go        Live model list from /api/v1/models
├── agent/
│   ├── agent.go             Core agentic loop + token-budget guardrail
│   ├── session_agent.go     Session-aware wrapper (feeds runtime.Monitor)
│   └── compaction.go        MI-based context compaction
├── acp/acp.go               Zed ACP server (JSON-RPC 2.0 over stdio)
├── runtime/
│   ├── metrics.go           Shared types (Observation, PolicyTrace, RuntimeReport)
│   ├── detector.go          CUSUM + BOCPD latency + output drift detection
│   ├── conformal.go         Conformal prediction anomaly envelope
│   ├── histogram.go         HDR histogram — p50/p95/p99/p999 tail latency
│   ├── ece.go               Expected Calibration Error tracker
│   ├── safety.go            PAC-Bayes-kl safety bound + veto
│   ├── circuitbreaker.go    Per-stage circuit breaker (CLOSED/OPEN/HALF-OPEN)
│   ├── quantile.go          Conformal quantile UQ — distribution-free intervals
│   ├── ope.go               Off-policy evaluator (IPS/WIS/DR + ESS + regret gate)
│   ├── voi.go               Thompson sampling VOI planner + DAgger policy transfer
│   ├── mcts.go              UCB1 MCTS for multi-probe planning
│   ├── attribution.go       Reservoir-sampled attribution + Shapley values
│   ├── semantic_cache.go    Semantic cache (Jaccard 3-gram, LRU)
│   ├── controller.go        OCO controller — routing weights, batch budget, backoff
│   └── monitor.go           Top-level Monitor wiring all subsystems
├── session/
│   ├── session.go           Session types and JSONL persistence
│   ├── sqlite.go            SQLite session index
│   ├── index.go             Session listing helpers
│   ├── metrics.go           Session-level usage metrics
│   └── distill.go           Trace distillation + JSONLDistillSink
├── tools/tools.go           8 built-in tools
├── extensions/
│   ├── manager.go           Extension discovery, trust registry, repair
│   ├── js.go                JavaScript extensions (goja VM, pi.* host APIs)
│   └── native.go            Native subprocess extensions (JSON protocol)
├── httpclient/client.go     HTTP client (streaming + non-streaming)
├── sse/sse.go               SSE parser
└── doctor/doctor.go         Health checks
```

### Design notes

**Single provider.** Everything routes through OpenRouter. One interface, one streaming implementation, one API key.

**Live model list.** `FetchModels()` calls `GET /api/v1/models` on startup. No hardcoded model IDs anywhere. New models on OpenRouter appear in Zed's model picker automatically.

**New ACP protocol.** Pi implements Zed's `agent_servers` ACP (not the older `language_models` protocol). Session state tracks the active model and thinking level per conversation; Zed's UI controls drive both via `session/set_config_option` and `session/set_model`.

**OpenRouter-native.** Every request goes through OpenRouter's OpenAI-compatible `/v1/chat/completions` endpoint. Pi passes OpenRouter's full provider preferences API through: `provider.order` and `allow_fallbacks` for routing, `data_collection: "deny"` for privacy, `quantization` for quantization level selection, and fallback model lists (route="fallback"). The `HTTP-Referer` and `X-Title` headers are set from `openrouter_site_url` / `openrouter_app_name` in config.

**Extended thinking maps to Anthropic budget_tokens.** Each level maps to a fixed token budget passed upstream: off=0, minimal=1024, low=2048, medium=8192, high=16384, xhigh=32768. Thinking detection (which models support it) is inferred from the model ID — no hardcoded allowlist.

**Minimal dependencies.** 4 direct deps: `cobra` (CLI), `uuid` (session IDs), `sqlite` (session index), `goja` (pure-Go JS VM for extensions). The entire runtime intelligence package uses only stdlib (`math`, `sort`, `sync`, `container/list`).

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
| Go source files | 37 |
| Lines of code | ~8,500 |
| Providers | 1 (OpenRouter) |
| Models | 500+ (live from API) |
| Built-in tools | 8 |
| Thinking levels | 6 (off / minimal / low / medium / high / xhigh) |
| Runtime subsystems | 17 |
| Direct dependencies | 4 |
| Go version | 1.24+ |
