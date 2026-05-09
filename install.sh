#!/usr/bin/env bash
#
# pi installer — builds from source and installs the pi binary.
#
# One-liner install:
#   curl -fsSL https://raw.githubusercontent.com/tiru-r/pi-agent-go/main/install.sh | bash
#
# Usage:
#   ./install.sh [options]
#
# Options:
#   --system          Install to /usr/local/bin (requires sudo)
#   --dest <dir>      Install binary to <dir> (default: ~/.local/bin)
#   --yes, -y         Skip confirmation prompts
#   --quiet, -q       Suppress non-error output
#   -h, --help        Show this help

set -euo pipefail

# ── Defaults ──────────────────────────────────────────────────────────────────

DEST_DEFAULT="$HOME/.local/bin"
DEST="$DEST_DEFAULT"
SYSTEM=0
YES=0
QUIET=0

BINARY_NAME="pi"
REPO_URL="https://github.com/tiru-r/pi-agent-go"
MIN_GO_MAJOR=1
MIN_GO_MINOR=24

STATE_DIR="${XDG_STATE_HOME:-$HOME/.local/state}/pi-agent"
STATE_FILE="$STATE_DIR/install-state.env"
PATH_MARKER="# pi-agent installer PATH"

# ── Argument parsing ──────────────────────────────────────────────────────────

while [ $# -gt 0 ]; do
  case "$1" in
    --system)
      SYSTEM=1
      DEST="/usr/local/bin"
      shift
      ;;
    --dest)
      DEST="${2:?--dest requires an argument}"
      shift 2
      ;;
    --yes|-y)
      YES=1
      shift
      ;;
    --quiet|-q)
      QUIET=1
      shift
      ;;
    -h|--help)
      sed -n '2,16p' "$0" | sed 's/^# \?//'
      exit 0
      ;;
    *)
      echo "Unknown option: $1" >&2
      exit 1
      ;;
  esac
done

# ── Logging helpers ───────────────────────────────────────────────────────────

log()  { [ "$QUIET" -eq 1 ] && return; echo -e "$*" >&2; }
info() { [ "$QUIET" -eq 1 ] && return; echo -e "\033[0;34m→\033[0m $*" >&2; }
ok()   { [ "$QUIET" -eq 1 ] && return; echo -e "\033[0;32m✓\033[0m $*" >&2; }
warn() { [ "$QUIET" -eq 1 ] && return; echo -e "\033[1;33m⚠\033[0m $*" >&2; }
err()  { echo -e "\033[0;31m✗\033[0m $*" >&2; }

prompt_confirm() {
  local msg="$1"
  [ "$YES" -eq 1 ] && return 0
  [ -t 0 ] || return 0
  printf "%s [Y/n] " "$msg"
  local ans
  read -r ans || true
  case "${ans:-y}" in
    y|Y|yes|YES|Yes) return 0 ;;
    *) return 1 ;;
  esac
}

# ── Checks ────────────────────────────────────────────────────────────────────

check_go() {
  if ! command -v go >/dev/null 2>&1; then
    err "Go is not installed. Install Go ${MIN_GO_MAJOR}.${MIN_GO_MINOR}+ from https://go.dev/dl/ and re-run."
    exit 1
  fi

  local ver
  ver=$(go version | grep -oP 'go\K[0-9]+\.[0-9]+' | head -1)
  local major minor
  major=$(echo "$ver" | cut -d. -f1)
  minor=$(echo "$ver" | cut -d. -f2)

  if [ "$major" -lt "$MIN_GO_MAJOR" ] || { [ "$major" -eq "$MIN_GO_MAJOR" ] && [ "$minor" -lt "$MIN_GO_MINOR" ]; }; then
    err "Go ${MIN_GO_MAJOR}.${MIN_GO_MINOR}+ required; found go${ver}."
    exit 1
  fi
  ok "Go ${ver} found"
}

check_dest_writable() {
  if [ "$SYSTEM" -eq 1 ] && [ ! -w "$DEST" ]; then
    err "$DEST is not writable. Re-run with sudo or use --dest ~/.local/bin."
    exit 1
  fi
  mkdir -p "$DEST" 2>/dev/null || true
  if [ ! -w "$DEST" ]; then
    err "Cannot write to $DEST."
    exit 1
  fi
}

# ── Build ─────────────────────────────────────────────────────────────────────

# Temporary clone directory; cleaned up on exit.
_TMP_CLONE=""

cleanup() {
  [ -n "$_TMP_CLONE" ] && rm -rf "$_TMP_CLONE"
}
trap cleanup EXIT

# Returns the source directory to build from.
# If running via curl pipe (no local source), clones the repo to a temp dir.
resolve_src_dir() {
  local script_dir
  script_dir="$(cd "$(dirname "$0")" 2>/dev/null && pwd)" || script_dir=""

  if [ -f "${script_dir}/cmd/pi/main.go" ]; then
    echo "$script_dir"
    return
  fi

  # Running via curl pipe — no local source available.
  if ! command -v git >/dev/null 2>&1; then
    err "git is required to install via one-liner. Install git and re-run."
    exit 1
  fi

  _TMP_CLONE=$(mktemp -d)
  info "Cloning ${REPO_URL}…"
  git clone --depth=1 "$REPO_URL" "$_TMP_CLONE" >/dev/null 2>&1
  ok "Clone complete"
  echo "$_TMP_CLONE"
}

build_binary() {
  local src_dir
  src_dir=$(resolve_src_dir)

  local version
  if command -v git >/dev/null 2>&1 && git -C "$src_dir" rev-parse --git-dir >/dev/null 2>&1; then
    version=$(git -C "$src_dir" describe --tags --always --dirty 2>/dev/null || echo "dev")
  else
    version="dev"
  fi

  info "Building pi ${version}…"

  cd "$src_dir"
  go build \
    -ldflags "-s -w -X github.com/tiru-r/pi-agent-go/internal/cli.Version=${version}" \
    -o "$src_dir/pi" \
    ./cmd/pi/

  ok "Build complete"
  echo "$src_dir/pi"
}

# ── Install ───────────────────────────────────────────────────────────────────

install_binary() {
  local built_path="$1"
  local dest_bin="${DEST}/${BINARY_NAME}"

  if [ -e "$dest_bin" ]; then
    local existing_ver
    existing_ver=$("$dest_bin" version 2>/dev/null || echo "unknown")
    warn "Existing binary at ${dest_bin} (${existing_ver})"
    if ! prompt_confirm "Overwrite?"; then
      info "Skipping binary install."
      rm -f "$built_path"
      return
    fi
  fi

  mv "$built_path" "$dest_bin"
  chmod 755 "$dest_bin"
  ok "Installed ${dest_bin}"
}

# ── PATH ──────────────────────────────────────────────────────────────────────

add_to_path() {
  # Skip if DEST is already in PATH.
  case ":${PATH}:" in
    *":${DEST}:"*) return 0 ;;
  esac

  local snippet
  snippet="$(printf '\nexport PATH="%s:$PATH" %s' "$DEST" "$PATH_MARKER")"

  local updated=0
  for rc in "$HOME/.zshrc" "$HOME/.bashrc"; do
    [ -f "$rc" ] || continue
    grep -qF "$PATH_MARKER" "$rc" && continue
    printf '%s\n' "$snippet" >> "$rc"
    ok "Added ${DEST} to PATH in ${rc}"
    updated=1
  done

  if [ "$updated" -eq 1 ]; then
    warn "Restart your shell or run: export PATH=\"${DEST}:\$PATH\""
  fi
}

# ── State ─────────────────────────────────────────────────────────────────────

save_state() {
  local dest_bin="$1"
  mkdir -p "$STATE_DIR"
  cat > "$STATE_FILE" <<EOF
PIAG_INSTALL_BIN="${dest_bin}"
PIAG_INSTALL_DEST="${DEST}"
PIAG_PATH_MARKER="${PATH_MARKER}"
PIAG_INSTALL_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
EOF
  chmod 600 "$STATE_FILE"
}

# ── Header ────────────────────────────────────────────────────────────────────

show_header() {
  [ "$QUIET" -eq 1 ] && return
  echo ""
  echo -e "\033[1;34mpi installer\033[0m"
  echo -e "\033[0;90mZed-native AI agent (OpenRouter) — ${REPO_URL}\033[0m"
  echo ""
}

# ── Main ──────────────────────────────────────────────────────────────────────

main() {
  show_header

  check_go
  check_dest_writable

  info "Install location: ${DEST}/${BINARY_NAME}"

  if ! prompt_confirm "Proceed with install?"; then
    warn "Aborted."
    exit 0
  fi

  local built_path
  built_path=$(build_binary)

  install_binary "$built_path"

  if [ "$SYSTEM" -eq 0 ]; then
    add_to_path
  fi

  save_state "${DEST}/${BINARY_NAME}"

  log ""
  if [ "$QUIET" -eq 0 ]; then
    echo -e "\033[1;32mInstall complete.\033[0m"
    echo ""
    echo "  Set your OpenRouter API key:"
    echo "    export OPENROUTER_API_KEY=sk-or-..."
    echo "    # or persist it:"
    echo "    pi auth set sk-or-..."
    echo ""
    echo "  Add to Zed settings.json:"
    echo "    \"agent_servers\": {"
    echo "      \"pi\": {"
    echo "        \"type\": \"custom\","
    echo "        \"command\": \"pi\","
    echo "        \"args\": [\"acp\"],"
    echo "        \"env\": { \"OPENROUTER_API_KEY\": \"sk-or-...\" }"
    echo "      }"
    echo "    }"
    echo ""
    echo "  One-shot prompt:"
    echo "    pi run \"Explain this codebase\""
    echo ""
    echo "  Extensions (optional):"
    echo "    Drop .js or .json tool definitions into ~/.pi/extensions/"
    echo "    They are loaded automatically at startup."
    echo ""
    echo "  Run 'pi --help' for all options."
  fi
}

main "$@"
