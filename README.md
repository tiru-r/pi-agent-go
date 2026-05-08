# pi

A high-performance AI coding agent CLI written in Go. Pi provides an interactive terminal interface for AI-assisted coding with streaming responses, tool execution, and session persistence — supporting 11 LLM providers and 39 models out of the box.

```
$ pi
```

## Features

- **11 LLM providers** — Anthropic, OpenAI, Gemini, Azure OpenAI, Cohere, AWS Bedrock, Google Vertex AI, GitHub Copilot, GitLab Duo, OpenRouter
- **8 built-in tools** — read, write, edit, bash, grep, find, ls, hashline_edit
- **Interactive TUI** — Bubbletea-powered chat interface with streaming, syntax highlighting, and markdown rendering
- **Session persistence** — JSONL format with optional SQLite index; branch/replay support
- **Zed ACP** — Native language model provider for the Zed editor
- **RPC server** — JSON-RPC 2.0 over stdio for SDK embedding
- **Context compaction** — Automatic summarisation when context grows too large
- **Extended thinking** — First-class support for Anthropic and DeepSeek thinking models

---

## Installation

### One-liner (recommended)

```bash
curl -fsSL https://raw.githubusercontent.com/tiru-r/pi-agent-go/main/install.sh | bash
```

Requires Go 1.23+. Builds from source, installs to `~/.local/bin/pi`, and updates your shell PATH.

Options:

```bash
./install.sh --system          # install to /usr/local/bin (needs sudo)
./install.sh --dest ~/bin      # custom install directory
./install.sh --yes --quiet     # non-interactive
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

# Also delete config and session data:
./uninstall.sh --purge
```

---

## Quick start

```bash
# Set your API key (pick one provider)
pi auth set anthropic  sk-ant-...
pi auth set openrouter sk-or-...    # 200+ models via openrouter.ai

# Set default provider (optional — defaults to anthropic)
pi config set provider openrouter

# Start the interactive TUI
pi

# One-shot prompt
pi run "Explain this codebase"

# Use a specific provider and model
pi --provider openrouter --model deepseek/deepseek-r1 run "Solve this bug"
```

---

## Providers

Configure a provider by setting the appropriate environment variable or running `pi auth set`.

### Anthropic
```bash
export ANTHROPIC_API_KEY=sk-ant-...
pi --provider anthropic --model claude-sonnet-4-6
```

| Model ID | Display Name | Max Tokens | Capabilities |
|---|---|---|---|
| `claude-opus-4-7` | Claude Opus 4.7 | 32,000 | tools, vision, thinking |
| `claude-sonnet-4-6` | Claude Sonnet 4.6 | 16,000 | tools, vision, thinking |
| `claude-haiku-4-5-20251001` | Claude Haiku 4.5 | 8,096 | tools, vision |

### OpenAI
```bash
export OPENAI_API_KEY=sk-...
pi --provider openai --model gpt-4o
```

| Model ID | Max Tokens | Capabilities |
|---|---|---|
| `gpt-4o` | 16,384 | tools, vision |
| `gpt-4o-mini` | 16,384 | tools, vision |
| `o3` | 100,000 | tools |
| `o4-mini` | 100,000 | tools |

### Google Gemini
```bash
export GEMINI_API_KEY=...    # or GOOGLE_API_KEY
pi --provider gemini --model gemini-2.0-flash
```

| Model ID | Max Tokens | Capabilities |
|---|---|---|
| `gemini-2.0-flash` | 8,192 | tools, vision |
| `gemini-1.5-pro` | 8,192 | tools, vision |
| `gemini-1.5-flash` | 8,192 | tools, vision |

### Azure OpenAI
```bash
export AZURE_OPENAI_API_KEY=...
export AZURE_OPENAI_ENDPOINT=https://my-resource.openai.azure.com
pi --provider azure --model azure/gpt-4o
```

Requires `AZURE_OPENAI_DEPLOYMENT` or set `azure_deployment` in settings.json.
Default API version: `2024-08-01-preview`.

### Cohere
```bash
export COHERE_API_KEY=...
pi --provider cohere --model command-r-plus
```

| Model ID | Max Tokens | Capabilities |
|---|---|---|
| `command-r-plus` | 4,096 | tools |
| `command-r` | 4,096 | tools |

### AWS Bedrock
```bash
# Uses standard AWS credential chain (~/.aws/, environment, EC2 instance profile)
export AWS_REGION=us-east-1
pi --provider bedrock --model bedrock/claude-sonnet-4-6
```

| Model ID | Notes |
|---|---|
| `bedrock/claude-opus-4-7` | Maps to `us.anthropic.claude-opus-4-7-20250514-v1:0` |
| `bedrock/claude-sonnet-4-6` | Maps to `us.anthropic.claude-sonnet-4-6-20241022-v2:0` |

### Google Vertex AI
```bash
# Uses Application Default Credentials (gcloud auth application-default login)
export GOOGLE_CLOUD_PROJECT=my-project
pi --provider vertex --model vertex/gemini-1.5-pro
```

### GitHub Copilot
```bash
export GITHUB_TOKEN=ghp_...    # or GITHUB_OAUTH_TOKEN
pi --provider copilot --model copilot/gpt-4o
```

Token is also auto-discovered from `~/.config/gh/hosts.yml` (gh CLI config).

### GitLab Duo
```bash
export GITLAB_TOKEN=glpat-...
export GITLAB_URL=https://gitlab.com    # optional, defaults to gitlab.com
pi --provider gitlab --model gitlab/claude-3-5-sonnet
```

Token is also auto-discovered from `~/.config/glab-cli/config.yml`.

### OpenRouter
```bash
export OPENROUTER_API_KEY=sk-or-...
pi --provider openrouter --model anthropic/claude-opus-4-7
```

OpenRouter gives access to 200+ models from all major providers through a single API key. Provider routing and fallbacks can be configured via `Request.Extra`:

```go
req.Extra = map[string]any{
    "provider": map[string]any{
        "order":           []string{"Anthropic", "Together"},
        "data_collection": "deny",
        "allow_fallbacks": true,
    },
    "models": []string{"anthropic/claude-opus-4-7", "openai/gpt-4o"},
}
```

Popular OpenRouter models:

| Model ID | Provider | Capabilities |
|---|---|---|
| `anthropic/claude-opus-4-7` | Anthropic | tools, vision, thinking |
| `anthropic/claude-sonnet-4-6` | Anthropic | tools, vision, thinking |
| `openai/gpt-4o` | OpenAI | tools, vision |
| `openai/o3` | OpenAI | tools |
| `google/gemini-2.0-flash-001` | Google | tools, vision |
| `meta-llama/llama-3.3-70b-instruct` | Meta | tools |
| `deepseek/deepseek-r1` | DeepSeek | thinking |
| `deepseek/deepseek-chat-v3-0324` | DeepSeek | tools |
| `mistralai/codestral-2501` | Mistral | tools |
| `qwen/qwen-2.5-coder-32b-instruct` | Qwen | tools |
| `x-ai/grok-3-beta` | xAI | tools, vision |

---

## CLI reference

### Global flags

| Flag | Short | Description |
|---|---|---|
| `--model` | `-m` | Override LLM model |
| `--provider` | `-p` | Override provider |
| `--system` | | Override system prompt |
| `--session` | `-s` | Session ID to continue |
| `--no-session` | | Disable session persistence |
| `--think` | | Enable extended thinking |
| `--debug` | | Enable debug logging |
| `--acp` | | Run as Zed ACP server (stdio) |

### Commands

#### `pi` — Interactive TUI

```bash
pi                              # Start with default provider/model
pi -p openrouter -m deepseek/deepseek-r1
pi --think                      # Enable extended thinking
pi -s abc123                    # Resume session abc123
```

#### `pi run` — One-shot agent

```bash
pi run "Refactor this module to use interfaces"
pi run --think "Solve this algorithmic problem"
```

Streams the agent's response to stdout and exits.

#### `pi session` — Session management

```bash
pi session list                 # List all sessions (most recent first)
pi session open <id>            # Open session in interactive TUI
pi session show <id>            # Print session messages to stdout
pi session delete <id>          # Delete session (prompts for confirmation)
pi session delete <id> --force  # Delete without confirmation
```

#### `pi auth` — API key management

```bash
pi auth set anthropic  sk-ant-...    # Store Anthropic key
pi auth set openrouter sk-or-...     # Store OpenRouter key
pi auth set openai     sk-...
pi auth set gemini     AIza...
pi auth set cohere     ...
pi auth set azure      ...
pi auth status                       # Show which providers have keys (masked)
pi auth check                        # Validate all configured keys
```

Keys are stored in `~/.pi/agent/settings.json` (mode 0600).

#### `pi models` — List available models

```bash
pi models                       # All 39 models with provider, max tokens, capabilities
pi models | grep openrouter     # Filter by provider
```

#### `pi config` — Configuration

```bash
pi config show                  # Print current effective config
pi config set provider openrouter
pi config set model deepseek/deepseek-r1
pi config set theme light
pi config set thinking_level auto
pi config set system_prompt "You are an expert Go developer"
```

#### `pi doctor` — Health checks

```bash
pi doctor
```

Checks: config file, session directory, configured provider API key presence, and model registry lookup.

#### `pi acp` — Zed language model server

```bash
pi acp                          # Explicitly run ACP server
pi --acp                        # Same via root flag
```

#### `pi version`

```bash
pi version                      # pi version 0.1.0
```

---

## Built-in tools

The agent has access to 8 built-in tools for interacting with the filesystem and shell.

### `read` — Read file with line numbers

```json
{ "path": "src/main.go", "offset": 50, "limit": 100 }
```

- `path` (required) — File path to read
- `offset` — Start line (1-indexed, default 1)
- `limit` — Max lines to return (default 2000)
- Detects images by extension (jpg, png, gif, webp, svg) and returns them as image content blocks

### `write` — Write or create a file

```json
{ "path": "src/utils.go", "content": "package main\n..." }
```

Creates parent directories as needed.

### `edit` — String replacement

```json
{ "path": "main.go", "old_string": "fmt.Println", "new_string": "log.Println" }
```

- `replace_all` (bool, default `false`) — Replace all occurrences; without it, fails if `old_string` appears more than once

### `bash` — Execute shell command

```json
{ "command": "go test ./...", "timeout": 60000 }
```

- `timeout` — Milliseconds (default 120,000 ms / 2 min)
- Output capped at 100 KB
- Captures combined stdout + stderr

### `grep` — Search file contents

```json
{ "pattern": "func.*Handler", "path": ".", "context": 3, "recursive": true }
```

- `context` — Lines of context around each match
- `case_sensitive` (bool, default `true`)
- `recursive` (bool, default `true`) — Walk subdirectories
- Skips `.git/`, `node_modules/`, `target/`; max 100 matches

### `find` — Find files by pattern

```json
{ "path": ".", "pattern": "*.go", "type": "f", "max_depth": 3 }
```

- `type` — `f` (file), `d` (directory), `l` (symlink)
- Skips `.git/`, `node_modules/`, `target/`; max 1000 results

### `ls` — List directory

```json
{ "path": "internal/provider" }
```

Returns entries with type, size, and permissions. Max 500 entries.

### `hashline_edit` — Precise line editing

```json
{
  "path": "main.go",
  "edits": [{ "line_hash": "42#a3f91c", "new_content": "    return nil" }]
}
```

Line hash format is `LINE#HASH` where `HASH` is the first 6 characters of SHA-256 of the line content. Read a file with `read` to obtain hashes, then use them for surgical edits that survive minor file changes.

---

## Sessions

Sessions are stored in `~/.pi/agent/sessions/` by default.

### JSONL format (version 3)

Each session is a `.jsonl` file — one JSON object per line. The first line is always `{"version":3}`.

Entry types:

| Type | Purpose |
|---|---|
| `metadata` | Session creation info (ID, model, provider, timestamps) |
| `message` | A conversation turn (role, content blocks, usage) |
| `model_change` | Model switch mid-session |
| `thinking_level` | Thinking level change |
| `compaction` | Context compaction summary |

### SQLite index

When `sqlite: true` (default), an `index.db` is maintained alongside sessions for fast listing and search. Schema:

```sql
sessions (id, title, model, provider, created_at, updated_at, message_count, token_count, path)
session_entries (id, session_id, parent_id, type, timestamp, data)
```

### Context compaction

When the estimated token count exceeds the configured threshold, pi automatically:
1. Keeps the last 10 messages intact
2. Sends the older messages to the LLM for summarisation
3. Prepends a `[Previous conversation summary]` stub
4. Continues the session from the trimmed context

---

## Configuration

Settings are loaded from `~/.pi/agent/settings.json` (or `$PI_CONFIG`), with environment variables taking precedence.

### Settings file

```json
{
  "provider": "anthropic",
  "model": "claude-sonnet-4-6",
  "max_tokens": 8096,
  "temperature": 0,
  "thinking_level": "off",
  "system_prompt": "",
  "theme": "dark",
  "syntax_theme": "monokai",
  "session_dir": "~/.pi/agent/sessions",
  "sqlite": true
}
```

### Environment variables

| Variable | Config key | Notes |
|---|---|---|
| `ANTHROPIC_API_KEY` | `anthropic_api_key` | |
| `OPENAI_API_KEY` | `openai_api_key` | |
| `GEMINI_API_KEY` / `GOOGLE_API_KEY` | `gemini_api_key` | Either works |
| `COHERE_API_KEY` | `cohere_api_key` | |
| `OPENROUTER_API_KEY` | `openrouter_api_key` | |
| `AZURE_OPENAI_API_KEY` | `azure_api_key` | |
| `AZURE_OPENAI_ENDPOINT` | `azure_endpoint` | |
| `GOOGLE_CLOUD_PROJECT` | `vertex_project` | |
| `PI_MODEL` | `model` | Override model |
| `PI_PROVIDER` | `provider` | Override provider |
| `PI_CONFIG` | — | Custom config file path |

---

## Zed integration (ACP)

Pi can serve as a native language model provider for the [Zed](https://zed.dev) editor via the Agent Client Protocol (ACP).

### Protocol

JSON-RPC 2.0 over line-delimited JSON on stdio. Protocol version: `0.1.0`.

**Zed → pi:**

```json
{"jsonrpc":"2.0","id":0,"method":"initialize","params":{}}
{"jsonrpc":"2.0","id":1,"method":"complete","params":{"id":1,"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"Hello"}]}}
{"jsonrpc":"2.0","method":"cancel","params":{"id":1}}
```

**pi → Zed:**

```json
{"jsonrpc":"2.0","id":0,"result":{"protocol_version":"0.1.0","name":"pi","models":[...]}}
{"jsonrpc":"2.0","method":"chunk","params":{"id":1,"text":"Hello! "}}
{"jsonrpc":"2.0","id":1,"result":{"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":10}}}
```

### Zed config

In your Zed `settings.json`:

```json
{
  "language_models": {
    "pi": {
      "provider": "custom",
      "command": "pi",
      "args": ["acp"],
      "env": {
        "ANTHROPIC_API_KEY": "sk-ant-..."
      }
    }
  }
}
```

All 39 registered models are advertised to Zed on `initialize`, with full capability flags for tools, vision, and thinking.

---

## RPC server

Pi exposes a JSON-RPC 2.0 server over stdin/stdout for programmatic embedding:

```bash
echo '{"jsonrpc":"2.0","id":1,"method":"chat","params":{"message":"Hello"}}' | pi --provider anthropic
```

### Methods

| Method | Params | Returns |
|---|---|---|
| `chat` | `{message, model?, system?}` | Streams `{"event":"text","text":"..."}` notifications; final `{"event":"done","usage":{...}}` |
| `session/new` | `{}` | `{session_id}` |
| `session/list` | `{}` | `{sessions:[...]}` |
| `session/open` | `{session_id}` | Session metadata |
| `model/list` | `{}` | `{models:[...]}` |

---

## TUI keyboard shortcuts

| Key | Action |
|---|---|
| `Enter` | Submit message |
| `Shift+Enter` | Insert newline |
| `Esc` | Abort current generation |
| `Ctrl+C` / `Ctrl+Q` | Quit |
| `PgUp` / `PgDn` | Scroll conversation |
| `Ctrl+U` / `Ctrl+F` | Scroll up / down |
| `Ctrl+Home` / `Ctrl+End` | Jump to top / bottom |
| `↑` / `↓` | Navigate input history |
| `Ctrl+Y` | Copy last response |
| `Ctrl+N` | New session |
| `Ctrl+O` | Open session picker |
| `Ctrl+T` | Toggle extended thinking |
| `Ctrl+L` | Clear screen |
| `?` | Show help |

### Slash commands

| Command | Description |
|---|---|
| `/help` (or `/h`) | Show available commands |
| `/clear` | Clear conversation history |
| `/model <id>` (or `/m`) | Switch model |
| `/session new\|list\|open <id>` | Session management |
| `/think on\|off\|auto` | Toggle thinking level |
| `/system <prompt>` | Set system prompt |
| `/copy` | Copy last response to clipboard |
| `/exit` (or `/quit`, `/q`) | Exit pi |

---

## Architecture

```
cmd/pi/main.go                  Binary entry point
internal/
├── cli/root.go                 Cobra command tree (all subcommands + flags)
├── config/config.go            Settings loading (file + env precedence)
├── model/
│   ├── message.go              Message/ContentBlock/Usage types
│   └── registry.go             45-model registry with capability flags
├── provider/
│   ├── provider.go             Provider interface + Request/Event types
│   ├── factory/factory.go      Provider constructor dispatcher
│   ├── anthropic/              Anthropic Messages API (SSE streaming)
│   ├── openai/                 OpenAI Chat Completions (SSE streaming)
│   ├── gemini/                 Google Gemini (SSE streaming)
│   ├── azure/                  Azure OpenAI (SSE streaming)
│   ├── cohere/                 Cohere Chat v2 (SSE streaming)
│   ├── bedrock/                AWS Bedrock Converse Stream (SDK)
│   ├── vertex/                 Google Vertex AI (OAuth2 + SSE)
│   ├── copilot/                GitHub Copilot (token exchange + SSE)
│   ├── gitlab/                 GitLab Duo (OpenAI-compatible SSE)
│   └── openrouter/             OpenRouter (OpenAI-compatible + routing)
├── tools/tools.go              8 built-in tools + registry
├── agent/
│   ├── agent.go                Core agent loop (stream → tool → loop)
│   ├── session_agent.go        Session-aware wrapper
│   └── compaction.go           Context compaction
├── session/
│   ├── session.go              JSONL persistence (version 3)
│   ├── sqlite.go               SQLite index + search
│   ├── index.go                In-memory session index
│   ├── metrics.go              Session statistics
│   └── picker.go               Bubbletea session picker
├── tui/
│   ├── app.go                  Main Bubbletea TUI model
│   ├── view.go                 Conversation rendering
│   ├── commands.go             Slash command handlers
│   ├── theme.go                Color themes (dark/light)
│   ├── keybindings.go          Key binding definitions
│   └── model_selector.go       Searchable model picker overlay
├── acp/acp.go                  Zed ACP server (JSON-RPC 2.0 over stdio)
├── rpc/rpc.go                  SDK RPC server (JSON-RPC 2.0 over stdio)
├── auth/auth.go                Credential storage + validation
├── httpclient/client.go        HTTP client (streaming + non-streaming)
├── sse/sse.go                  SSE stream parser
├── doctor/doctor.go            Environment health checks
├── migrations/migrations.go    Startup migration from legacy layouts
├── platform/platform.go        OS/arch detection
└── version/version.go          Background update checker
```

### Key design decisions

**Provider interface is minimal:** `Name()` and `Stream()` only. Every provider returns a `<-chan Event` channel — the agent loop is identical for all 11 providers.

**SSE parsing is per-provider:** Each provider speaks a slightly different wire format. Sharing SSE parsing across providers would require abstraction that costs clarity. Each provider file is self-contained.

**`Request.Extra map[string]any`:** Provider-specific parameters (OpenRouter routing, Azure deployment config) flow through this escape hatch without polluting the shared interface.

**Tool execution is parallel:** The agent executes all tool calls from a single assistant turn concurrently (capped at 4 goroutines), then feeds all results back in one user turn.

**Session compaction is provider-agnostic:** The compactor calls the same `provider.Stream()` interface to generate summaries, so any configured provider handles compaction.

---

## Building for release

```bash
VERSION=$(git describe --tags --always --dirty)
LDFLAG="-s -w -X github.com/tiru-r/pi-agent-go/internal/cli.Version=${VERSION}"

go build -ldflags "$LDFLAG" -o pi ./cmd/pi/

# Cross-compile
GOOS=linux  GOARCH=amd64 go build -ldflags "$LDFLAG" -o pi-linux-amd64   ./cmd/pi/
GOOS=linux  GOARCH=arm64 go build -ldflags "$LDFLAG" -o pi-linux-arm64   ./cmd/pi/
GOOS=darwin GOARCH=arm64 go build -ldflags "$LDFLAG" -o pi-darwin-arm64  ./cmd/pi/
GOOS=windows GOARCH=amd64 go build -ldflags "$LDFLAG" -o pi-windows-amd64.exe ./cmd/pi/
```

## Running tests

```bash
go test ./...
go test ./internal/provider/... -v
go test ./internal/session/... -v
```

---

## Stats

| Metric | Value |
|---|---|
| Go files | 41 |
| Lines of code | ~12,000 |
| Providers | 11 |
| Models | 39 |
| Built-in tools | 8 |
| Go version | 1.23.0 |
| Binary size (release) | ~18 MB |
