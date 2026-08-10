#!/usr/bin/env bash
set -euo pipefail

ROOT=$(git rev-parse --show-toplevel)
SOURCE="$ROOT/upstream/opencode"
PATCH_DIR="$ROOT/upstream/patches"
WORKTREE=$(mktemp -d "${TMPDIR:-/tmp}/opencode-workspaces.XXXXXX")
cleanup() {
  rm -rf "$WORKTREE"
}
trap cleanup EXIT

# The pinned submodule remains a pristine upstream commit. Build and test in a
# throwaway copy so the patch queue is the only source of downstream changes.
git -C "$SOURCE" archive HEAD | tar -x -C "$WORKTREE"
git -C "$WORKTREE" init --quiet
git -C "$WORKTREE" add -A
git -C "$WORKTREE" -c user.name='Patch Check' -c user.email='patch-check@invalid' commit --quiet -m 'pinned upstream snapshot'
for patch in "$PATCH_DIR"/*.patch; do
  [[ -e "$patch" ]] || continue
  git -C "$WORKTREE" apply --3way "$patch"
done

cd "$WORKTREE"
exec "$@"
