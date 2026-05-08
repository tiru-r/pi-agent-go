#!/usr/bin/env bash
#
# pi uninstaller — removes the pi binary and optional config/session data.
#
# One-liner uninstall:
#   curl -fsSL https://raw.githubusercontent.com/tiru-r/pi-agent-go/main/uninstall.sh | bash
#
# Usage:
#   ./uninstall.sh [options]
#
# Options:
#   --purge           Also remove config and session data (~/.pi/agent/)
#   --keep-path       Do not remove PATH lines added by the installer
#   --yes, -y         Skip confirmation prompts
#   --quiet, -q       Suppress non-error output
#   -h, --help        Show this help

set -euo pipefail

# ── Defaults ──────────────────────────────────────────────────────────────────

YES=0
QUIET=0
PURGE=0
KEEP_PATH=0

BINARY_NAME="pi"
STATE_DIR="${XDG_STATE_HOME:-$HOME/.local/state}/pi-agent"
STATE_FILE="$STATE_DIR/install-state.env"
PATH_MARKER="# pi-agent installer PATH"

CONFIG_DIR="$HOME/.pi/agent"

# Recorded by installer; overwritten from state file when available.
PIAG_INSTALL_BIN=""
PIAG_INSTALL_DEST=""
PIAG_PATH_MARKER="$PATH_MARKER"

# ── Argument parsing ──────────────────────────────────────────────────────────

while [ $# -gt 0 ]; do
  case "$1" in
    --purge)
      PURGE=1
      shift
      ;;
    --keep-path)
      KEEP_PATH=1
      shift
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

log()  { [ "$QUIET" -eq 1 ] && return; echo -e "$*"; }
ok()   { [ "$QUIET" -eq 1 ] && return; echo -e "\033[0;32m✓\033[0m $*"; }
warn() { [ "$QUIET" -eq 1 ] && return; echo -e "\033[1;33m⚠\033[0m $*"; }
err()  { echo -e "\033[0;31m✗\033[0m $*" >&2; }
info() { [ "$QUIET" -eq 1 ] && return; echo -e "\033[0;34m→\033[0m $*"; }

prompt_confirm() {
  local msg="$1"
  local default_yes="${2:-0}"
  [ "$YES" -eq 1 ] && return 0
  [ -t 0 ] || { [ "$default_yes" -eq 1 ] && return 0 || return 1; }
  local suffix="[y/N]"
  [ "$default_yes" -eq 1 ] && suffix="[Y/n]"
  printf "%s %s " "$msg" "$suffix"
  local ans
  read -r ans || true
  if [ -z "$ans" ]; then
    [ "$default_yes" -eq 1 ] && return 0 || return 1
  fi
  case "$ans" in
    y|Y|yes|YES|Yes) return 0 ;;
    *) return 1 ;;
  esac
}

# ── State ─────────────────────────────────────────────────────────────────────

load_state() {
  [ -f "$STATE_FILE" ] || return 0
  # shellcheck disable=SC1090
  source "$STATE_FILE"
  # Use state-recorded PATH marker if present.
  [ -n "${PIAG_PATH_MARKER:-}" ] && PATH_MARKER="$PIAG_PATH_MARKER"
}

# ── Binary removal ────────────────────────────────────────────────────────────

is_pi_binary() {
  local path="$1"
  [ -x "$path" ] || return 1
  "$path" version 2>/dev/null | grep -q "^pi version" || return 1
}

find_binary_candidates() {
  # State-recorded path first, then common fallbacks.
  local candidates=()
  [ -n "$PIAG_INSTALL_BIN" ] && candidates+=("$PIAG_INSTALL_BIN")
  candidates+=(
    "$HOME/.local/bin/${BINARY_NAME}"
    "/usr/local/bin/${BINARY_NAME}"
    "/usr/bin/${BINARY_NAME}"
  )
  printf '%s\n' "${candidates[@]}"
}

remove_binary() {
  local removed=0

  while IFS= read -r cand; do
    [ -n "$cand" ] || continue
    [ -e "$cand" ] || continue
    if is_pi_binary "$cand"; then
      rm -f "$cand"
      ok "Removed binary: ${cand}"
      removed=1
    fi
  done < <(find_binary_candidates | sort -u)

  if [ "$removed" -eq 0 ]; then
    warn "pi binary not found — nothing to remove."
  fi
}

# ── PATH cleanup ──────────────────────────────────────────────────────────────

remove_path_entries() {
  [ "$KEEP_PATH" -eq 1 ] && return 0

  local touched=0
  for rc in "$HOME/.zshrc" "$HOME/.bashrc"; do
    [ -f "$rc" ] || continue
    grep -qF "$PATH_MARKER" "$rc" 2>/dev/null || continue
    local tmp="${rc}.pi-uninstall.tmp"
    grep -vF "$PATH_MARKER" "$rc" > "$tmp" || true
    mv "$tmp" "$rc"
    ok "Removed PATH entry from ${rc}"
    touched=1
  done

  [ "$touched" -eq 0 ] && info "No PATH entries to remove."
}

# ── Config / session purge ────────────────────────────────────────────────────

remove_config_data() {
  [ "$PURGE" -eq 0 ] && return 0

  if [ ! -d "$CONFIG_DIR" ]; then
    info "No config directory found at ${CONFIG_DIR}."
    return 0
  fi

  warn "This will permanently delete ${CONFIG_DIR} (all sessions and settings)."
  if ! prompt_confirm "Delete config and session data?" 0; then
    info "Keeping config data."
    return 0
  fi

  rm -rf "$CONFIG_DIR"
  ok "Removed config directory: ${CONFIG_DIR}"
}

# ── State cleanup ─────────────────────────────────────────────────────────────

remove_state() {
  [ -f "$STATE_FILE" ] && rm -f "$STATE_FILE" && ok "Removed installer state"
  rmdir "$STATE_DIR" 2>/dev/null || true
}

# ── Plan summary ──────────────────────────────────────────────────────────────

plan_summary() {
  [ "$QUIET" -eq 1 ] && return

  echo -e "\033[1;31mPlanned uninstall actions\033[0m"

  local bin_path=""
  while IFS= read -r cand; do
    [ -n "$cand" ] || continue
    if [ -e "$cand" ] && is_pi_binary "$cand" 2>/dev/null; then
      bin_path="$cand"
      break
    fi
  done < <(find_binary_candidates | sort -u)

  if [ -n "$bin_path" ]; then
    echo -e "  \033[0;90mRemove binary:  ${bin_path}\033[0m"
  else
    echo -e "  \033[0;90mBinary:         not found\033[0m"
  fi

  if [ "$KEEP_PATH" -eq 0 ]; then
    echo -e "  \033[0;90mPATH cleanup:   remove installer lines from .bashrc / .zshrc\033[0m"
  fi

  if [ "$PURGE" -eq 1 ]; then
    echo -e "  \033[0;90mPurge data:     ${CONFIG_DIR}\033[0m"
  fi
}

# ── Header ────────────────────────────────────────────────────────────────────

show_header() {
  [ "$QUIET" -eq 1 ] && return
  echo ""
  echo -e "\033[1;31mpi uninstaller\033[0m"
  echo -e "\033[0;90mRemoves pi-agent artifacts\033[0m"
  echo ""
}

# ── Main ──────────────────────────────────────────────────────────────────────

main() {
  show_header
  load_state
  plan_summary

  log ""
  if ! prompt_confirm "Proceed with uninstall?" 1; then
    warn "Uninstall cancelled."
    exit 0
  fi

  remove_binary
  remove_path_entries
  remove_config_data
  remove_state

  log ""
  if [ "$QUIET" -eq 0 ]; then
    echo -e "\033[1;32mUninstall complete.\033[0m"
    if [ "$PURGE" -eq 0 ] && [ -d "$CONFIG_DIR" ]; then
      echo ""
      echo "  Sessions and config remain at: ${CONFIG_DIR}"
      echo "  Run with --purge to delete them."
    fi
  fi
}

main "$@"
