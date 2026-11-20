#!/usr/bin/env bash
# The lint job's Biome step: `bunx biome check .` over the whole tree, with Biome's own output and
# exit status, and a failure for each file that Biome skips for its size.
#
# Biome reads nothing of a file at or above files.maxSize. It prints an info diagnostic ("The size
# of the file is ..., which exceeds the configured maximum of ...") and still exits 0. Biome 2.4.11
# has no setting that raises that diagnostic's severity, and --error-on-warnings covers warnings
# only. So a file that grew past the limit dropped out of the check, and the step stayed green.
#
# Biome decides which limit applies to a file: biome.json's, an override's, or Biome's default of
# 1 MiB under a nested configuration that extends nothing, which --files-max-size does not change.
# So this script reads the size diagnostics of the run itself. It shows every diagnostic
# (--max-diagnostics=none), since by default Biome prints only the first 20, and turns colours off
# so that the lines it reads are plain text.
#
# Exits with Biome's status when Biome fails, after naming any file it skipped. Otherwise exits 1
# when Biome skipped one or more files for their size, each named in a GitHub annotation, and 2
# when Biome printed a size diagnostic that this script cannot match to a file, since it then
# cannot name what Biome skipped.
#
# Run from anywhere, after `bun install`: .github/scripts/check-biome-tree.sh
# CI runs it in ci.yaml's lint job, and its tests (check-biome-tree.test.sh) in ci.yaml's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

log=$(mktemp)
trap 'rm -f "$log"' EXIT

set +e
bunx biome check . --colors=off --max-diagnostics=none 2>&1 | tee "$log"
biome_status=${PIPESTATUS[0]}
set -e

# Each size diagnostic is a header line `<path> check`, which a rule of `━` follows, and then, as
# its first non-blank line, `  i The size of the file is ..., which exceeds the configured maximum
# of ...`. The rule is matched as any text, since bash reads `━` byte by byte outside a UTF-8 locale.
skipped=()
messages=()
pending=""
while IFS= read -r line; do
  if [[ "$line" =~ ^([^ ].*)\ check(\ .*)?$ ]]; then
    pending=${BASH_REMATCH[1]}
    continue
  fi
  [ -n "${line// /}" ] || continue
  if [[ "$line" =~ ^\ \ i\ (The\ size\ of\ the\ file\ is\ .*exceeds\ the\ configured\ maximum.*)$ ]]; then
    if [ -z "$pending" ]; then
      echo "check-biome-tree: Biome printed a size diagnostic with no file header before it, so this check cannot name the file Biome skipped: ${BASH_REMATCH[1]}" >&2
      exit 2
    fi
    skipped+=("$pending")
    messages+=("${BASH_REMATCH[1]}")
  fi
  pending=""
done < "$log"

for i in "${!skipped[@]}"; do
  path=${skipped[$i]}
  echo "::error file=$path::$path: Biome reads none of this file and still exits 0. ${messages[$i]} Split the file, or raise files.maxSize in the Biome configuration that applies to it."
done

if [ "$biome_status" -ne 0 ]; then
  exit "$biome_status"
fi
if [ "${#skipped[@]}" -gt 0 ]; then
  echo "check-biome-tree: Biome skipped ${#skipped[@]} file(s) for their size"
  exit 1
fi
echo "check-biome-tree: Biome skipped no file for its size"
