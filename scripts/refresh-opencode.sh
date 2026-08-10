#!/usr/bin/env bash
set -euo pipefail

ROOT=$(git rev-parse --show-toplevel)
SUBMODULE="$ROOT/upstream/opencode"
PATCH_DIR="$ROOT/upstream/patches"
UPSTREAM_REF=${1:-origin/dev}
CURRENT=$(git -C "$SUBMODULE" rev-parse HEAD)

if [[ -n $(git -C "$SUBMODULE" status --porcelain) ]]; then
  echo "The OpenCode submodule has uncommitted changes." >&2
  exit 1
fi

git -C "$SUBMODULE" fetch origin dev
git -C "$SUBMODULE" checkout --detach "$UPSTREAM_REF"

# Validate the queue without modifying the pristine submodule checkout.
"$ROOT/scripts/with-patched-opencode.sh" git diff --check

printf 'OpenCode moved from pinned upstream %s to %s\n' "$CURRENT" "$(git -C "$SUBMODULE" rev-parse HEAD)"
printf 'The patch queue remains in %s. Run make opencode-verify before committing the parent repository.\n' "$PATCH_DIR"
