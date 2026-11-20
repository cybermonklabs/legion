#!/usr/bin/env bash
# Tests for `check-biome-max-size.sh`. A TypeScript file above biome.json's files.maxSize fails
# and is named in a GitHub annotation, and so does a file exactly at the limit, since Biome skips a
# file whose size equals it; a file one byte under passes. An untracked file counts, and an
# ignored one does not. A file that Biome never reads passes: files.includes leaves it out, an
# override turns all of Biome's tools off for it, or Biome has no handler for its type. A file that
# only the linter reads, and one that only the formatter reads, each count, since the check runs
# Biome twice to read both without the project scanner. The smaller limit of an override counts,
# and so does Biome's default of 1 MiB under a nested configuration that extends nothing. A
# configuration jq cannot read, a Biome run that prints no summary, and a tree with no Biome each
# exit 2 rather than pass.
#
# Each case builds a miniature git repository under a temporary directory, symlinks the real
# script into it, and symlinks in this repository's node_modules, so every case runs the Biome
# that bun.lock pins: the script resolves its root from its own path, which the symlink puts at
# the fixture root. No case runs the check over this repository's own tree: ci.yaml's lint job
# does (.github/workflows/ci-job-lint.yaml, "Check no file is too large for Biome to read"), on
# every change.
#
# Run from anywhere, after `bun install`: .github/scripts/check-biome-max-size.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check-biome-max-size.sh"
node_modules=$(cd "$script_dir/../.." && pwd)/node_modules
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

command -v git > /dev/null || { echo "  FAIL: git is required" >&2; exit 1; }
command -v jq > /dev/null || { echo "  FAIL: jq is required" >&2; exit 1; }
[ -x "$node_modules/.bin/biome" ] || { echo "  FAIL: $node_modules/.bin/biome is missing; run bun install first" >&2; exit 1; }

# fixture <name> [max size]: a git repository whose biome.json sets files.maxSize (2000 bytes
# unless given), with one small TypeScript file. Echoes its root.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/src"
  ln -s "$check_script" "$root/.github/scripts/check-biome-max-size.sh"
  ln -s "$node_modules" "$root/node_modules"
  printf 'node_modules\n' > "$root/.gitignore"
  config "$root" "{ \"maxSize\": ${2:-2000} }"
  ts "$root/src/small.ts" 100
  git -C "$root" init -q
  git -C "$root" add .
  echo "$root"
}

# config <root> <files object> [rest]: writes the fixture's biome.json.
config() {
  cat > "$1/biome.json" <<JSON
{
  "vcs": { "enabled": true, "clientKind": "git", "useIgnoreFile": true },
  "files": $2${3:+,
  $3}
}
JSON
}

# ts <file> <bytes>: a formatted TypeScript file of exactly that many bytes (at least 40): export
# lines, and a comment line that makes up the rest.
ts() {
  mkdir -p "$(dirname "$1")"
  awk -v n="$2" 'BEGIN {
    s = 0
    for (i = 0; ; i++) {
      line = sprintf("export const v%d = %d;\n", i, i)
      if (s + length(line) + 40 > n) break
      printf "%s", line
      s += length(line)
    }
    rest = n - s - 3
    pad = ""
    for (j = 0; j < rest; j++) pad = pad "x"
    printf "//%s\n", pad
  }' > "$1"
}

# run_check <root>: sets `out` to the combined output and `status` to the exit code.
run_check() {
  set +e
  out=$(cd "$1" && .github/scripts/check-biome-max-size.sh 2>&1)
  status=$?
  set -e
}

echo "case: a tree with no file at or above the limit passes, saying what it read"
root=$(fixture clean)
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "says Biome skips nothing, and the limit it held the files to" \
  "$(contains "$out" '^check-biome-max-size: Biome skips no file for its size ([0-9]* files listed, [0-9]* at or above 2000 bytes')"

echo "case: a TypeScript file above the limit fails, naming the file (the control)"
root=$(fixture over)
ts "$root/src/big.ts" 3000
git -C "$root" add src/big.ts
run_check "$root"
check "fails" "$(is "$status" 1)"
check "annotates the file for GitHub, with its size and Biome's own sentence" \
  "$(contains "$out" '^::error file=src/big.ts::src/big.ts is 3000 bytes, and Biome reads none of it: The size of the file is 2.9 KiB, which exceeds the configured maximum of 2.0 KiB')"
check "counts the findings" "$(contains "$out" '^check-biome-max-size: Biome skips 1 file(s) for their size')"
check "names no other file" "$(is "$(grep -c '^::error' <<< "$out")" 1)"

echo "case: a file exactly at the limit fails, since Biome skips a file whose size equals it"
root=$(fixture equal)
ts "$root/src/equal.ts" 2000
git -C "$root" add src/equal.ts
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names it" "$(contains "$out" '^::error file=src/equal.ts::src/equal.ts is 2000 bytes')"

echo "case: a file one byte under the limit passes"
root=$(fixture under)
ts "$root/src/under.ts" 1999
git -C "$root" add src/under.ts
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: an untracked file counts, and an ignored one does not"
root=$(fixture untracked)
ts "$root/src/new.ts" 3000
run_check "$root"
check "an untracked file fails" "$(is "$status" 1)"
check "names it" "$(contains "$out" '^::error file=src/new.ts::')"

root=$(fixture ignored)
printf 'node_modules\nbuild/\n' > "$root/.gitignore"
ts "$root/build/out.ts" 3000
run_check "$root"
check "an ignored file passes" "$(is "$status" 0)"

echo "case: a file that Biome never reads passes"
root=$(fixture never-read)
config "$root" '{ "maxSize": 2000, "includes": ["**", "!!fixtures"] }' \
  '"overrides": [{ "includes": ["off/**"], "linter": { "enabled": false }, "formatter": { "enabled": false }, "assist": { "enabled": false } }]'
ts "$root/fixtures/big.ts" 3000
ts "$root/off/big.ts" 3000
head -c 3000 /dev/zero | tr '\0' 'x' > "$root/src/notes.md"
git -C "$root" add .
run_check "$root"
check "exits 0: files.includes, an override with every tool off, and a type with no handler" "$(is "$status" 0)"
check "still held those files to the limit" "$(contains "$out" 'files listed, [4-9] at or above 2000 bytes')"

echo "case: a file that only the linter reads, and one that only the formatter reads, each fail"
root=$(fixture one-tool)
config "$root" '{ "maxSize": 2000 }' \
  '"linter": { "rules": { "suspicious": { "noImportCycles": "error" } } },
  "overrides": [
    { "includes": ["lint-only/**"], "formatter": { "enabled": false }, "assist": { "enabled": false } },
    { "includes": ["format-only/**"], "linter": { "enabled": false }, "assist": { "enabled": false } }
  ]'
ts "$root/lint-only/big.ts" 3000
ts "$root/format-only/big.ts" 3000
git -C "$root" add .
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the file only the linter reads" "$(contains "$out" '^::error file=lint-only/big.ts::')"
check "names the file only the formatter reads" "$(contains "$out" '^::error file=format-only/big.ts::')"
check "counts each once" "$(contains "$out" '^check-biome-max-size: Biome skips 2 file(s) for their size')"

echo "case: an override's smaller limit counts"
root=$(fixture override-limit 100000)
config "$root" '{ "maxSize": 100000 }' '"overrides": [{ "includes": ["tight/**"], "files": { "maxSize": 2000 } }]'
ts "$root/tight/big.ts" 3000
ts "$root/src/big.ts" 3000
git -C "$root" add .
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the file under the override" "$(contains "$out" '^::error file=tight/big.ts::')"
check "names no file under the root limit" "$(is "$(grep -c '^::error' <<< "$out")" 1)"

echo "case: a nested configuration that extends nothing keeps Biome's default of 1 MiB"
root=$(fixture nested-default 4194304)
mkdir -p "$root/nested"
printf '{ "root": false }\n' > "$root/nested/biome.json"
ts "$root/nested/big.ts" 1100000
ts "$root/src/big.ts" 1100000
git -C "$root" add .
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the file under the nested configuration" "$(contains "$out" '^::error file=nested/big.ts::nested/big.ts is 1100000 bytes')"
check "names no file under the root limit" "$(is "$(grep -c '^::error' <<< "$out")" 1)"

echo "case: a configuration jq cannot read exits 2"
root=$(fixture unreadable)
printf '// a comment jq refuses\n' >> "$root/biome.json"
run_check "$root"
check "exits 2" "$(is "$status" 2)"
check "says it cannot tell the limit" "$(contains "$out" 'jq could not read the Biome configurations (biome.json)')"

echo "case: a Biome run that prints no summary exits 2"
root=$(fixture refused)
config "$root" '{ "maxSize": 2000 }' '"noSuchSetting": true'
ts "$root/src/big.ts" 3000
git -C "$root" add .
run_check "$root"
check "exits 2" "$(is "$status" 2)"
check "says Biome printed no summary" "$(contains "$out" 'printed no summary for the [0-9]* files at or above 2000 bytes')"

echo "case: a tree with no Biome under node_modules exits 2"
root=$(fixture no-biome)
rm "$root/node_modules"
ts "$root/src/big.ts" 3000
git -C "$root" add .
run_check "$root"
check "exits 2" "$(is "$status" 2)"
check "says to install" "$(contains "$out" 'node_modules/.bin/biome is missing.*run bun install first')"

summary "check-biome-max-size.sh"
