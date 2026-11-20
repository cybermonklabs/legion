#!/usr/bin/env bash
# Tests for `check-biome-tree.sh`. A clean tree passes and keeps Biome's own output. A TypeScript
# file above biome.json's files.maxSize fails and is named in a GitHub annotation with Biome's own
# sentence. A file Biome fails on (bad formatting) fails with Biome's own output and no
# annotation, and a tree with both is named and fails. Each of 25 skipped files is named, though
# Biome prints only 20 diagnostics by default. A file above Biome's default of 1 MiB under a nested
# configuration that extends nothing is named, though the root limit is larger. Through a stand-in
# Biome: the script exits with Biome's own status, and exits 2 on a size diagnostic it cannot
# match to a file.
#
# Each case builds a miniature git repository under a temporary directory and symlinks the real
# script into it, so the script under test is the file CI runs: it resolves its root from its own
# path, which the symlink puts at the fixture root. Every case but the stand-in ones also links in
# this repository's node_modules, so it runs the Biome that bun.lock pins. No case runs the check
# over this repository's own tree: ci.yaml's lint job does (.github/workflows/ci-job-lint.yaml,
# "Check the whole tree with Biome"), on every change.
#
# Run from anywhere, after `bun install`: .github/scripts/check-biome-tree.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check-biome-tree.sh"
node_modules=$(cd "$script_dir/../.." && pwd)/node_modules
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

command -v git > /dev/null || { echo "  FAIL: git is required" >&2; exit 1; }
[ -x "$node_modules/.bin/biome" ] || { echo "  FAIL: $node_modules/.bin/biome is missing; run bun install first" >&2; exit 1; }

# fixture <name> [max size]: a git repository whose biome.json sets files.maxSize (2000 bytes
# unless given), with one small TypeScript file and this repository's node_modules. Echoes its root.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/src"
  ln -s "$check_script" "$root/.github/scripts/check-biome-tree.sh"
  ln -s "$node_modules" "$root/node_modules"
  printf 'node_modules\n' > "$root/.gitignore"
  cat > "$root/biome.json" <<JSON
{
  "vcs": { "enabled": true, "clientKind": "git", "useIgnoreFile": true },
  "files": { "maxSize": ${2:-2000} },
  "formatter": { "indentStyle": "space" }
}
JSON
  ts "$root/src/small.ts" 100
  git -C "$root" init -q
  git -C "$root" add .
  echo "$root"
}

# stand_in <root> <script body>: replaces the fixture's node_modules with one whose only Biome is
# a script of the case's own.
stand_in() {
  rm "$1/node_modules"
  mkdir -p "$1/node_modules/.bin"
  printf '#!/usr/bin/env bash\n%s\n' "$2" > "$1/node_modules/.bin/biome"
  chmod +x "$1/node_modules/.bin/biome"
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
  out=$(cd "$1" && .github/scripts/check-biome-tree.sh 2>&1)
  status=$?
  set -e
}

echo "case: a clean tree passes, with Biome's own output"
root=$(fixture clean)
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "keeps Biome's summary" "$(contains "$out" '^Checked [0-9]* files in ')"
check "says Biome skipped nothing" "$(contains "$out" '^check-biome-tree: Biome skipped no file for its size$')"

echo "case: a TypeScript file above the limit fails, naming the file (the control)"
root=$(fixture over)
ts "$root/src/big.ts" 3000
git -C "$root" add src/big.ts
run_check "$root"
check "fails" "$(is "$status" 1)"
check "annotates the file for GitHub, with Biome's own sentence" \
  "$(contains "$out" '^::error file=src/big.ts::src/big.ts: Biome reads none of this file and still exits 0. The size of the file is 2.9 KiB, which exceeds the configured maximum of 2.0 KiB')"
check "counts the findings" "$(contains "$out" '^check-biome-tree: Biome skipped 1 file(s) for their size$')"
check "names no other file" "$(is "$(grep -c '^::error' <<< "$out")" 1)"

echo "case: a file Biome fails on fails with Biome's own output, and names no file"
root=$(fixture biome-fails)
printf 'export const a   =1\n' > "$root/src/bad.ts"
git -C "$root" add src/bad.ts
run_check "$root"
check "fails" "$(is "$status" 1)"
check "keeps Biome's diagnostic" "$(contains "$out" 'Formatter would have printed the following content')"
check "names no file" "$(is "$(grep -c '^::error' <<< "$out")" 0)"

echo "case: a file Biome fails on beside a file it skips: both are reported"
root=$(fixture both)
printf 'export const a   =1\n' > "$root/src/bad.ts"
ts "$root/src/big.ts" 3000
git -C "$root" add src
run_check "$root"
check "fails" "$(is "$status" 1)"
check "keeps Biome's diagnostic" "$(contains "$out" 'Formatter would have printed the following content')"
check "names the skipped file" "$(contains "$out" '^::error file=src/big.ts::')"

echo "case: each of 25 skipped files is named, though Biome prints 20 diagnostics by default"
root=$(fixture many)
for i in $(seq -w 1 25); do
  ts "$root/src/big$i.ts" 3000
done
git -C "$root" add src
run_check "$root"
check "fails for the sizes alone" "$(contains "$out" '^check-biome-tree: Biome skipped 25 file(s) for their size$')"
check "names all 25" "$(is "$(grep -c '^::error file=src/big[0-9]*\.ts::' <<< "$out")" 25)"

echo "case: a nested configuration that extends nothing keeps Biome's default of 1 MiB"
root=$(fixture nested-default 4194304)
mkdir -p "$root/nested"
printf '{ "root": false }\n' > "$root/nested/biome.json"
ts "$root/nested/big.ts" 1100000
ts "$root/src/big.ts" 1100000
git -C "$root" add .
run_check "$root"
check "fails for the size alone" "$(contains "$out" '^check-biome-tree: Biome skipped 1 file(s) for their size$')"
check "names the file under the nested configuration" "$(contains "$out" '^::error file=nested/big.ts::')"
check "names no file under the root limit" "$(is "$(grep -c '^::error' <<< "$out")" 1)"

echo "case: the script exits with Biome's own status (a stand-in Biome)"
root=$(fixture status)
stand_in "$root" 'echo "Checked 1 file in 1ms. No fixes applied."; exit 3'
run_check "$root"
check "exits 3" "$(is "$status" 3)"
check "keeps the stand-in's output" "$(contains "$out" '^Checked 1 file in 1ms')"

echo "case: a size diagnostic with no file header exits 2 (a stand-in Biome)"
root=$(fixture no-header)
stand_in "$root" 'printf "  i The size of the file is 2.9 KiB, which exceeds the configured maximum of 2.0 KiB for this project.\nChecked 0 files in 1ms. No fixes applied.\n"'
run_check "$root"
check "exits 2" "$(is "$status" 2)"
check "says it cannot name the file" "$(contains "$out" 'no file header before it, so this check cannot name the file Biome skipped')"

summary "check-biome-tree.sh"
