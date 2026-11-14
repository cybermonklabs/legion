#!/usr/bin/env bash
# Tests for `check-workflow-files.sh`: every file under .github/workflows is within 64,000 bytes,
# parses as YAML, repeats no mapping key after upper-casing, and calls at most 45 unique reusable
# workflows.
#
# Each case builds a miniature repository under a temporary directory and symlinks the real
# script into it, so the script under test is the file CI runs.
#
# Run from anywhere: .github/scripts/check-workflow-files.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check-workflow-files.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# fixture <name>: an empty tree with the script linked in. Echoes its root.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/.github/workflows"
  ln -s "$check_script" "$root/.github/scripts/check-workflow-files.sh"
  echo "$root"
}

run_check() {
  local root=$1
  set +e
  out=$(cd "$root" && "./.github/scripts/check-workflow-files.sh" 2>&1)
  status=$?
  set -e
}

# errors <text>: how many annotations the run printed.
errors() { grep -c '^::error file=' <<<"$1" || true; }

# workflow <file>: the smallest workflow every case starts from.
workflow() {
  cat > "$1" <<'YAML'
name: Build
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo build
YAML
}

# reusable <file>: a workflow another one may call.
reusable() {
  cat > "$1" <<'YAML'
name: Reusable
on: workflow_call
jobs:
  work:
    runs-on: ubuntu-latest
    steps:
      - run: echo work
YAML
}

# caller <file> <count> <first>: a workflow whose <count> jobs each call one local reusable
# workflow, numbered from <first>.
caller() {
  local file=$1 count=$2 first=$3 index name
  printf 'name: Caller\non: push\njobs:\n' > "$file"
  for ((index = 0; index < count; index++)); do
    name=$(printf 'w%02d' "$((first + index))")
    printf '  call-%s:\n    uses: ./.github/workflows/%s.yaml\n' "$name" "$name" >> "$file"
  done
}

# callees <dir> <count> <first>: the reusable workflows a caller of those numbers calls.
callees() {
  local dir=$1 count=$2 first=$3 index name
  for ((index = 0; index < count; index++)); do
    name=$(printf 'w%02d' "$((first + index))")
    reusable "$dir/$name.yaml"
  done
}

# pad_to <file> <bytes>: append one comment line so the file is exactly <bytes> long.
pad_to() {
  local file=$1 want=$2 have gap
  have=$(wc -c < "$file")
  gap=$((want - have))
  if [ "$gap" -lt 2 ]; then
    echo "  FAIL: $file is already $have bytes, which is too long to pad to $want" >&2
    return 1
  fi
  printf '#%*s\n' "$((gap - 2))" '' | tr ' ' 'x' >> "$file"
}

echo "case: a workflow file one byte above 64,000"
root=$(fixture over-limit)
workflow "$root/.github/workflows/ci.yaml"
pad_to "$root/.github/workflows/ci.yaml" 64001
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file and its bytes" \
  "$(contains "$out" '^::error file=.github/workflows/ci.yaml::.github/workflows/ci.yaml is 64,001 bytes, above this check.s limit of 64,000 bytes')"
check "names GitHub's cap too" "$(contains "$out" 'its own cap of 500,000 bytes (500 KB)')"
check "counts the problem" \
  "$(contains "$out" '^1 problem(s) across the 1 workflow file(s) under .github/workflows$')"

echo "case: a workflow file one byte above GitHub's own cap of 500 KB"
root=$(fixture over-cap)
workflow "$root/.github/workflows/ci.yaml"
pad_to "$root/.github/workflows/ci.yaml" 500001
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names GitHub's cap and what it does to the run" \
  "$(contains "$out" 'is 500,001 bytes, above GitHub.s own cap of 500,000 bytes (500 KB): GitHub starts no run at all')"
check "says it is above this check's limit as well" \
  "$(contains "$out" "above this check's limit of 64,000 bytes as well")"
check "reports it once, not twice" "$(is "$(errors "$out")" 1)"

echo "case: a workflow file holding one key twice"
root=$(fixture repeated-key)
cat > "$root/.github/workflows/ci.yaml" <<'YAML'
name: Build
on: push
name: Build again
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo build
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names both keys and the line of the second" \
  "$(contains "$out" "^::error file=.github/workflows/ci.yaml,line=3::.github/workflows/ci.yaml:3: the mapping key 'name' is the key 'name' of line 1 again after upper-casing")"
check "says why GitHub refuses it" \
  "$(contains "$out" 'compares keys without regard to case')"

echo "case: an env mapping holding http_proxy and HTTP_PROXY"
root=$(fixture folded-env)
cat > "$root/.github/workflows/ci.yaml" <<'YAML'
name: Build
on: push
env:
  http_proxy: http://proxy.invalid:3128
  HTTP_PROXY: http://proxy.invalid:3128
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo build
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names both keys and the line of the second" \
  "$(contains "$out" "^::error file=.github/workflows/ci.yaml,line=5::.github/workflows/ci.yaml:5: the mapping key 'HTTP_PROXY' is the key 'http_proxy' of line 4 again")"

echo "case: a workflow file that is not valid YAML"
root=$(fixture bad-yaml)
printf 'name: "CI\non: push\n' > "$root/.github/workflows/ci.yaml"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file and the parser's message" \
  "$(contains "$out" '^::error file=.github/workflows/ci.yaml::.github/workflows/ci.yaml is not valid YAML: ')"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

echo "case: two files at fault at once, one of each class"
root=$(fixture two-faults)
workflow "$root/.github/workflows/big.yaml"
pad_to "$root/.github/workflows/big.yaml" 64001
cat > "$root/.github/workflows/folded.yaml" <<'YAML'
name: Build
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    env:
      path: /usr/bin
      PATH: /usr/bin
    steps:
      - run: echo build
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file above the limit" \
  "$(contains "$out" '^::error file=.github/workflows/big.yaml::.github/workflows/big.yaml is 64,001 bytes')"
check "and the file with the folded keys, in the same run" \
  "$(contains "$out" "^::error file=.github/workflows/folded.yaml,line=8::.github/workflows/folded.yaml:8: the mapping key 'PATH' is the key 'path' of line 7 again")"
check "counts both" \
  "$(contains "$out" '^2 problem(s) across the 2 workflow file(s) under .github/workflows$')"

echo "case: exactly 64,000 bytes and exactly 45 unique reusable workflows passes"
root=$(fixture at-both-limits)
caller "$root/.github/workflows/ci.yaml" 45 1
callees "$root/.github/workflows" 45 1
pad_to "$root/.github/workflows/ci.yaml" 64000
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "says what it read" \
  "$(contains "$out" '^all 46 workflow files under .github/workflows are within 64,000 bytes')"
check "and what it held them to" \
  "$(contains "$out" 'call at most 45 unique reusable workflows$')"

echo "case: job ids and with: keys that differ only in case"
root=$(fixture folded-elsewhere)
cat > "$root/.github/workflows/jobs.yaml" <<'YAML'
name: Build
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo one
  Build:
    runs-on: ubuntu-latest
    steps:
      - run: echo two
YAML
reusable "$root/.github/workflows/called.yaml"
cat > "$root/.github/workflows/with.yaml" <<'YAML'
name: Caller
on: push
jobs:
  call:
    uses: ./.github/workflows/called.yaml
    with:
      token: one
      Token: two
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the two job ids" \
  "$(contains "$out" "^::error file=.github/workflows/jobs.yaml,line=8::.github/workflows/jobs.yaml:8: the mapping key 'Build' is the key 'build' of line 4 again")"
check "and the two with: keys" \
  "$(contains "$out" "^::error file=.github/workflows/with.yaml,line=8::.github/workflows/with.yaml:8: the mapping key 'Token' is the key 'token' of line 7 again")"

echo "case: a file calling 46 unique reusable workflows"
root=$(fixture over-calls)
caller "$root/.github/workflows/ci.yaml" 46 1
callees "$root/.github/workflows" 46 1
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the count and the limit" \
  "$(contains "$out" '^::error file=.github/workflows/ci.yaml::.github/workflows/ci.yaml calls 46 unique reusable workflows, above this check.s limit of 45:')"
check "lists the callees" "$(contains "$out" '.github/workflows/w46.yaml')"
check "names GitHub's own cap of 50" \
  "$(contains "$out" 'GitHub allows 50 from a single workflow file, counting every tree of nested calls')"

echo "case: 44 calls, one of which calls two more, is a tree of 46"
root=$(fixture nested-calls)
caller "$root/.github/workflows/ci.yaml" 44 1
callees "$root/.github/workflows" 44 1
caller "$root/.github/workflows/w01.yaml" 2 45
callees "$root/.github/workflows" 2 45
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "counts the whole tree" \
  "$(contains "$out" '^::error file=.github/workflows/ci.yaml::.github/workflows/ci.yaml calls 46 unique reusable workflows')"
check "names a workflow only the nested call reaches" \
  "$(contains "$out" '.github/workflows/w46.yaml')"
check "the nested caller itself, which calls two, passes" \
  "$(is "$(contains "$out" 'file=.github/workflows/w01.yaml')" false)"

echo "case: a .yml file is covered, and a file of that directory that is neither is passed over"
root=$(fixture suffixes)
workflow "$root/.github/workflows/ci.yml"
pad_to "$root/.github/workflows/ci.yml" 64001
printf 'The workflows of this repository live here.\n' > "$root/.github/workflows/README.md"
pad_to "$root/.github/workflows/README.md" 70000
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the .yml file" \
  "$(contains "$out" '^::error file=.github/workflows/ci.yml::.github/workflows/ci.yml is 64,001 bytes')"
check "says nothing of the README" "$(is "$(contains "$out" 'README')" false)"
check "read one file" \
  "$(contains "$out" '^1 problem(s) across the 1 workflow file(s) under .github/workflows$')"

summary "check-workflow-files.sh"
