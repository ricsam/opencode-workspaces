#!/usr/bin/env bash
set -euo pipefail

ROOT=$(git rev-parse --show-toplevel)
SUBMODULE="$ROOT/upstream/opencode"
PATCH_DIR="$ROOT/upstream/patches"
BASE=${1:-}

if [[ -z "$BASE" ]]; then
  echo "Usage: $0 <upstream-base-commit>" >&2
  echo "Commit custom changes on a temporary branch in upstream/opencode, then provide its upstream base." >&2
  exit 2
fi
if [[ -n $(git -C "$SUBMODULE" status --porcelain) ]]; then
  echo "Commit all OpenCode customizations before exporting patches." >&2
  exit 1
fi
if ! git -C "$SUBMODULE" merge-base --is-ancestor "$BASE" HEAD; then
  echo "$BASE is not an ancestor of the submodule HEAD." >&2
  exit 1
fi

rm -f "$PATCH_DIR"/*.patch
git -C "$SUBMODULE" format-patch --output-directory "$PATCH_DIR" "$BASE"..HEAD
printf 'Exported patches for %s..%s\n' "$BASE" "$(git -C "$SUBMODULE" rev-parse HEAD)"
printf 'Now return the submodule to the upstream base: git -C upstream/opencode checkout --detach %s\n' "$BASE"
