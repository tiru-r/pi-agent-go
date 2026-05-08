# pi

A Zed-native AI coding agent powered by [OpenRouter](https://openrouter.ai). Pi runs as a native language model provider inside Zed via the Agent Client Protocol (ACP) — giving Zed access to every model OpenRouter offers (500+), with a full agentic loop and built-in tools.

---

## Features

- **Every OpenRouter model** — model list fetched live from the API; no hardcoded registry
- **8 built-in tools** — read, write, edit, bash, grep, find, ls, hashline_edit
- **Full agentic loop** — LLM → tools → LLM cycles inside Zed's chat panel
- **Session memory** — multi-turn conversation history maintained per Zed session
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

Pi advertises itself as a native language model provider. Zed invokes `pi acp` as a subprocess and communicates over stdin/stdout via JSON-RPC 2.0.

### Zed `settings.json`

```json
{
  "language_models": {
    "pi": {
      "provider": "custom",
      "command": "pi",
      "args": ["acp"],
      "env": {
        "OPENROUTER_API_KEY": "sk-or-..."
      }
    }
  }
}
```

On `initialize`, pi fetches the live model list from OpenRouter and returns it to Zed. All models appear in Zed's model picker immediately.

### Session flow

When Zed uses `session/prompt`, pi:
1. Loads conversation history for the session ID
2. Runs the full agentic loop — LLM calls tools (read, write, bash, …), feeds results back, repeats
3. Streams text and tool notifications back to Zed as `chunk` events
4. Saves updated history for the next turn

### Debug logging

```bash
PI_DEBUG=1 pi acp    # writes verbose logs to ~/.pi/agent/acp.log
```

### Protocol summary

```
Zed → pi:  initialize, complete, session/new, session/prompt, cancel
pi → Zed:  initialize result (model list), chunk notifications, complete result
```

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
│   ├── session_agent.go     Session-aware wrapper
│   └── compaction.go        Context compaction
├── acp/acp.go               Zed ACP server (JSON-RPC 2.0 over stdio)
├── session/                 JSONL + SQLite persistence
├── tools/tools.go           8 built-in tools
├── httpclient/client.go     HTTP client (streaming + non-streaming)
├── sse/sse.go               SSE parser
└── doctor/doctor.go         Health checks
```

### Design notes

**Single provider.** Everything routes through OpenRouter. One interface, one streaming implementation, one API key.

**Live model list.** `FetchModels()` calls `GET /api/v1/models` on startup. No hardcoded model IDs anywhere. New models on OpenRouter appear in Zed automatically.

**Minimal dependencies.** 3 direct deps: `cobra` (CLI), `uuid` (session IDs), `sqlite` (session index). The rest is stdlib.

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
| Go files | 21 |
| Lines of code | ~5,100 |
| Providers | 1 (OpenRouter) |
| Models | 500+ (live from API) |
| Built-in tools | 8 |
| Direct dependencies | 3 |
| Go version | 1.23+ |
