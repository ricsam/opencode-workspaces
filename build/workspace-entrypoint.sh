#!/usr/bin/env bash
set -euo pipefail

mkdir -p "$HOME" "$XDG_DATA_HOME" "$XDG_CONFIG_HOME" "$WORKSPACE_DIR"
chmod 700 "$HOME" "$XDG_CONFIG_HOME" 2>/dev/null || true
cd "$WORKSPACE_DIR"

exec opencode web --hostname 0.0.0.0 --port "${WORKSPACE_PORT:-4096}" >/dev/null 2>&1
