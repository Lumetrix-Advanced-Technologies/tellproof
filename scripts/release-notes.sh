#!/usr/bin/env bash
# Prints the CHANGELOG.md section for a tag (its release notes); fails if there is none.
set -euo pipefail
tag=${1:?usage: release-notes.sh vX.Y.Z}
notes=$(awk -v tag="$tag" '
  /^## / { if (found) exit; if ($2 == tag) { found = 1; next } }
  found { print }
' CHANGELOG.md | sed -e '/./,$!d')
if [ -z "$notes" ]; then
  echo "release-notes.sh: CHANGELOG.md has no section \"## $tag\"" >&2
  exit 1
fi
printf '%s\n' "$notes"
