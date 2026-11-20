#!/usr/bin/env bash
# Biome reads nothing of a file at or above its files.maxSize. It prints an info diagnostic ("The
# size of the file is ..., which exceeds the configured maximum of ..."), and the run still exits
# 0. Biome 2.4.11 has no setting that raises that diagnostic's severity, and --error-on-warnings
# covers warnings only. So a file that grows past the limit drops out of the lint job's
# `bunx biome check .`, and the step stays green. This check fails and names each file that
# Biome skips for its size.
#
# The limit is not one number for the whole tree. biome.json's files.maxSize applies under the
# root configuration and under each nested one that extends it ("extends": "//"). An override can
# set its own limit for the files it matches. A nested configuration that extends nothing (today
# packages/claude-envoy/biome.json) keeps Biome's default of 1 MiB, and --files-max-size does not
# change that one. So Biome decides, not this script:
#   1. List every file git tracks, and every untracked file that no .gitignore leaves out. Only
#      the .gitignore files, which Biome reads too: a file that only .git/info/exclude or
#      core.excludesFile leaves out is still listed.
#   2. Keep each file whose size is at or above the smallest limit: the smallest files.maxSize
#      that any Biome configuration sets (overrides included), or Biome's default if that is
#      smaller. Biome skips a file whose size equals the limit, so equal counts.
#   3. Run Biome over those files only, and take each size diagnostic it prints as a finding. A
#      file that Biome never reads prints no such diagnostic: files.includes leaves it out, an
#      override turns all of Biome's tools off for it, or Biome has no handler for its type (a
#      video, a Markdown page).
#
# Step 3 is two runs of Biome. `biome check` with its linter on first scans the whole project for
# noImportCycles, which costs minutes of CPU even for one file, and `check --only` scans too. So
# `check --linter-enabled=false` reads each file that the formatter or assist covers, and
# `lint --only=suspicious/noDebugger` reads each file that the linter covers. Neither run scans,
# and between them they read each file that the whole `bunx biome check .` reads.
#
# Exits 0 when Biome skips no file for its size, and 1 when it skips one or more (each is named).
# Exits 2 when the check cannot tell: git lists no file, jq cannot read a Biome configuration,
# node_modules holds no Biome, or a run of Biome printed no summary or printed a size diagnostic
# that this check cannot match to a file it gave Biome.
#
# Run from anywhere, after `bun install`: .github/scripts/check-biome-max-size.sh
# CI runs it in ci.yaml's lint job, and its tests (check-biome-max-size.test.sh) in ci.yaml's
# test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

# Biome's default files.maxSize, for a configuration that sets none (`biome check --help`).
default_limit=1048576
# The Biome that bun.lock pins. `bunx biome` without an install would fetch whatever npm serves.
biome=node_modules/.bin/biome

paths=$(mktemp)
configs=$(mktemp)
sized=$(mktemp)
trap 'rm -f "$paths" "$configs" "$sized"' EXIT

list() {
  git -c core.quotePath=false ls-files -z --cached --others --exclude-per-directory=.gitignore "$@"
}

list > "$paths"
listed=$(tr -cd '\0' < "$paths" | wc -c)
if [ "$listed" -eq 0 ]; then
  echo "check-biome-max-size: git listed no file in $(pwd), so this check cannot tell which files Biome skips" >&2
  exit 2
fi

list -- ':(glob)**/biome.json' ':(glob)**/biome.jsonc' > "$configs"
mapfile -d '' -t config_files < "$configs"
threshold=$default_limit
if [ "${#config_files[@]}" -gt 0 ]; then
  if ! threshold=$(jq -sr --argjson default "$default_limit" \
    '[$default, (.[] | .. | objects | .files? | objects | .maxSize? | numbers)] | min' \
    "${config_files[@]}" 2>&1); then
    echo "check-biome-max-size: jq could not read the Biome configurations (${config_files[*]}), so this check cannot tell which limit applies: $threshold" >&2
    exit 2
  fi
fi

# The size of each listed file. A tracked file deleted from the work tree has no size and Biome
# cannot read it, so it is left out. A symbolic link counts at the size of its target.
while IFS= read -r -d '' path; do
  if [ -f "$path" ]; then
    printf '%s\0' "$path"
  fi
done < "$paths" | xargs -0 -r stat -L --printf='%s\t%n\0' -- > "$sized"

declare -A size_of=()
candidates=()
while IFS=$'\t' read -r -d '' size path; do
  if [ "$size" -ge "$threshold" ]; then
    candidates+=("$path")
    size_of["$path"]=$size
  fi
done < "$sized"

scope="$listed files listed, ${#candidates[@]} at or above $threshold bytes, the smallest limit a Biome configuration here sets"
if [ "${#candidates[@]}" -eq 0 ]; then
  echo "check-biome-max-size: Biome skips no file for its size ($scope)"
  exit 0
fi

if [ ! -x "$biome" ]; then
  echo "check-biome-max-size: $biome is missing, so this check cannot ask Biome which of ${#candidates[@]} files it skips; run bun install first" >&2
  exit 2
fi

declare -A message_of=()
skipped=()

# ask_biome <command and flags...>: runs Biome over the candidates and records each file it skips
# for its size in `skipped`, with Biome's own sentence in `message_of`.
#
# Biome's exit status says nothing here: a file it reads can fail its lint or format checks,
# which the lint job's `bunx biome check .` reports. The summary line shows that the run happened.
# Each size diagnostic is a header line `<path> <command>`, which a rule of `━` follows, and then,
# as its first non-blank line, `  i The size of the file is ..., which exceeds the configured
# maximum of ...`. The rule is matched as any text, since bash reads `━` byte by byte outside a
# UTF-8 locale.
ask_biome() {
  local out line pending=""
  out=$("$biome" "$@" --colors=off --max-diagnostics=none --no-errors-on-unmatched -- "${candidates[@]}" 2>&1) || true
  if ! grep -qE '^Checked [0-9]+ files? in ' <<< "$out"; then
    echo "check-biome-max-size: \`biome $*\` printed no summary for the ${#candidates[@]} files at or above $threshold bytes, so this check cannot tell which it skips. Its output:" >&2
    echo "$out" >&2
    exit 2
  fi
  while IFS= read -r line; do
    if [[ "$line" =~ ^([^ ].*)\ $1(\ .*)?$ ]]; then
      pending=${BASH_REMATCH[1]}
      continue
    fi
    [ -n "${line// /}" ] || continue
    if [[ "$line" =~ ^\ \ i\ (The\ size\ of\ the\ file\ is\ .*exceeds\ the\ configured\ maximum.*)$ ]]; then
      if [ -z "$pending" ] || [ -z "${size_of["$pending"]+set}" ]; then
        echo "check-biome-max-size: \`biome $*\` printed a size diagnostic that this check cannot match to a file it gave Biome (header: '$pending'), so it cannot tell which file Biome skips. Its output:" >&2
        echo "$out" >&2
        exit 2
      fi
      if [ -z "${message_of["$pending"]+set}" ]; then
        skipped+=("$pending")
        message_of["$pending"]=${BASH_REMATCH[1]}
      fi
    fi
    pending=""
  done <<< "$out"
}

ask_biome check --linter-enabled=false
ask_biome lint --only=suspicious/noDebugger

if [ "${#skipped[@]}" -gt 0 ]; then
  for path in "${skipped[@]}"; do
    echo "::error file=$path::$path is ${size_of["$path"]} bytes, and Biome reads none of it: ${message_of["$path"]} Biome still exits 0, so \`bunx biome check .\` passes without reading it. Split the file, or raise files.maxSize in the Biome configuration that applies to it."
  done
  echo "check-biome-max-size: Biome skips ${#skipped[@]} file(s) for their size ($scope)"
  exit 1
fi

echo "check-biome-max-size: Biome skips no file for its size ($scope)"
