#!/usr/bin/env bash
#
# Computes the next release version as vX.Y.Z.
#
# Usage: build/version.sh [git-sha]   (defaults to HEAD)
#
# Logic:
#   1. Find the latest vX.Y.Z tag that is an ancestor of the given commit
#      (git tag --merged, not a plain `git describe`, so a tag from an
#      unrelated branch is never picked up).
#   2. If none exists yet, this is the first-ever release: print v1.0.0
#      directly, with no bump applied.
#   3. Otherwise, scan commit messages since that tag for Conventional Commits:
#        "BREAKING CHANGE" or "!:"  -> major bump
#        "feat:"                   -> minor bump
#        anything else (fix:, chore:, plain messages, ...) -> patch bump
#   4. Print the resulting vX.Y.Z.
set -euo pipefail

SHA="${1:-$(git rev-parse HEAD)}"

git fetch --tags --force >/dev/null 2>&1 || true

LAST_TAG="$(git tag --list --merged "$SHA" --sort=-v:refname 2>/dev/null \
  | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | head -n1 || true)"

if [[ -z "$LAST_TAG" ]]; then
  echo "v1.0.0"
  exit 0
fi

RANGE="${LAST_TAG}..${SHA}"
COMMITS="$(git log "$RANGE" --pretty=%B 2>/dev/null || true)"

BUMP="patch"
if echo "$COMMITS" | grep -qE 'BREAKING CHANGE|^[a-zA-Z]+(\([^)]*\))?!:'; then
  BUMP="major"
elif echo "$COMMITS" | grep -qE '^feat(\([^)]*\))?:'; then
  BUMP="minor"
fi

IFS='.' read -r MAJOR MINOR PATCH <<< "${LAST_TAG#v}"

case "$BUMP" in
  major) MAJOR=$((MAJOR + 1)); MINOR=0; PATCH=0 ;;
  minor) MINOR=$((MINOR + 1)); PATCH=0 ;;
  patch) PATCH=$((PATCH + 1)) ;;
esac

echo "v${MAJOR}.${MINOR}.${PATCH}"
