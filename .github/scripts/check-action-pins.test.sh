#!/usr/bin/env bash
# Tests for `check-action-pins.sh`: every remote `uses:` in a workflow or composite action is
# pinned to a 40-character commit SHA, and every actions/setup-go step carries
# `go-download-base-url: https://go.dev/dl` and `token: ''`.
#
# Each case builds a miniature repository under a temporary directory and symlinks the real
# script into it, so the script under test is the file CI runs.
#
# Run from anywhere: .github/scripts/check-action-pins.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check-action-pins.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

SHA=fbc6f3992d24b796d5a048ff273f7fcc4a7b6c09

# fixture <name>: an empty tree with the script linked in. Echoes its root.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/.github/workflows"
  ln -s "$check_script" "$root/.github/scripts/check-action-pins.sh"
  echo "$root"
}

run_check() {
  local root=$1
  set +e
  out=$(cd "$root" && "./.github/scripts/check-action-pins.sh" 2>&1)
  status=$?
  set -e
}

echo "case: every remote uses, in a workflow and a composite action, is pinned to a commit SHA"
root=$(fixture green)
cat > "$root/.github/workflows/ci.yaml" <<YAML
name: CI
on: push
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@${SHA} # v5.1.0
      - uses: ./.github/actions/local
YAML
mkdir -p "$root/.github/actions/local"
cat > "$root/.github/actions/local/action.yml" <<YAML
name: Local
runs:
  using: composite
  steps:
    - uses: oven-sh/setup-bun@${SHA} # v2.2.0
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "says what it covered" "$(contains "$out" 'every remote action across 2 files is pinned')"
check "and that no step uses setup-go" "$(contains "$out" 'and no step uses actions/setup-go$')"

echo "case: a floating tag fails"
root=$(fixture tag)
cat > "$root/.github/workflows/ci.yaml" <<'YAML'
name: CI
on: push
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@v5
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file, line and ref" \
  "$(contains "$out" ".github/workflows/ci.yaml:7: 'actions/checkout@v5' is pinned to 'v5', not a 40-character commit SHA")"

echo "case: a short SHA fails"
root=$(fixture short-sha)
cat > "$root/.github/workflows/ci.yaml" <<'YAML'
name: CI
on: push
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@fbc6f39
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names it" "$(contains "$out" "'actions/checkout@fbc6f39' is pinned to 'fbc6f39'")"

echo "case: an upper-case SHA fails"
root=$(fixture upper-sha)
cat > "$root/.github/workflows/ci.yaml" <<YAML
name: CI
on: push
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@${SHA^^}
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names it" "$(contains "$out" ", not a 40-character commit SHA")"

echo "case: a ref-less uses fails"
root=$(fixture refless)
cat > "$root/.github/workflows/ci.yaml" <<'YAML'
name: CI
on: push
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "says there is no ref to pin" "$(contains "$out" 'names no ref')"

echo "case: a local path and a Docker reference are out of scope"
root=$(fixture local-and-docker)
cat > "$root/.github/workflows/ci.yaml" <<'YAML'
name: CI
on: push
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - uses: ./.github/actions/local
      - uses: docker://alpine:3.19
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"

echo "case: a reusable-workflow call at job level is checked too"
root=$(fixture job-uses)
cat > "$root/.github/workflows/ci.yaml" <<'YAML'
name: CI
on: push
jobs:
  build:
    uses: octo-org/octo-repo/.github/workflows/build.yaml@v1
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the job-level call" "$(contains "$out" "octo-org/octo-repo/.github/workflows/build.yaml@v1' is pinned to 'v1'")"

echo "case: a file that cannot be parsed as YAML"
root=$(fixture bad-yaml)
printf 'name: CI\non: [push\n' > "$root/.github/workflows/ci.yaml"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file, without a traceback" "$(contains "$out" 'is not valid YAML')"
check "no traceback" "$(is "$(contains "$out" 'Traceback')" false)"

# setup_go_workflow <root> <with: lines>: a workflow whose one step uses actions/setup-go on line
# 7, with the given lines (each already indented under `with:`) as its inputs; no lines leaves the
# step with no `with:` at all.
setup_go_workflow() {
  local root=$1 inputs=$2
  {
    printf 'name: CI\non: push\njobs:\n  build:\n    runs-on: ubuntu-24.04\n    steps:\n'
    printf '      - uses: actions/setup-go@%s # v7.0.0\n' "$SHA"
    if [ -n "$inputs" ]; then printf '        with:\n%s\n' "$inputs"; fi
  } > "$root/.github/workflows/ci.yaml"
}

echo "case: a setup-go step with both inputs, in a workflow and in a composite action, passes"
root=$(fixture setup-go-green)
setup_go_workflow "$root" "          go-version-file: go.mod
          go-download-base-url: https://go.dev/dl
          token: ''"
mkdir -p "$root/.github/actions/local"
cat > "$root/.github/actions/local/action.yml" <<YAML
name: Local
runs:
  using: composite
  steps:
    - uses: actions/setup-go@${SHA} # v7.0.0
      with:
        go-version-file: go.mod
        go-download-base-url: https://go.dev/dl
        token: ""
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts both setup-go steps" "$(contains "$out" 'and all 2 actions/setup-go steps read Go from go.dev$')"

echo "case: a planted setup-go step with neither input fails by name"
root=$(fixture setup-go-neither)
setup_go_workflow "$root" "          go-version-file: go.mod
          cache: false"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the file, the line and both inputs" \
  "$(contains "$out" "^::error::.github/workflows/ci.yaml:7: 'actions/setup-go@$SHA' lacks go-download-base-url: https://go.dev/dl and token: '' under with:")"
check "says why" "$(contains "$out" "reads Go's version list through the GitHub API with the job's GITHUB_TOKEN")"
check "counts it" "$(contains "$out" '1 of 1 actions/setup-go step(s) not reading Go from go.dev')"

echo "case: a setup-go step with no with: at all fails"
root=$(fixture setup-go-no-with)
setup_go_workflow "$root" ""
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names both inputs" "$(contains "$out" "ci.yaml:7: 'actions/setup-go@$SHA' lacks go-download-base-url: https://go.dev/dl and token: ''")"

echo "case: the base URL alone fails, since setup-go would send the job's token to go.dev"
root=$(fixture setup-go-no-token)
setup_go_workflow "$root" "          go-version-file: go.mod
          go-download-base-url: https://go.dev/dl"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the token alone" "$(contains "$out" "ci.yaml:7: 'actions/setup-go@$SHA' lacks token: '' under with:")"

echo "case: the job's token named in place of the empty one fails"
root=$(fixture setup-go-github-token)
setup_go_workflow "$root" "          go-version-file: go.mod
          go-download-base-url: https://go.dev/dl
          token: \${{ github.token }}"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the token" "$(contains "$out" "lacks token: '' under with:")"
check "prints no input value" "$(is "$(contains "$out" 'github.token')" false)"

echo "case: another base URL fails"
root=$(fixture setup-go-mirror)
setup_go_workflow "$root" "          go-version-file: go.mod
          go-download-base-url: https://mirror.example/go
          token: ''"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the base URL alone" "$(contains "$out" "ci.yaml:7: 'actions/setup-go@$SHA' lacks go-download-base-url: https://go.dev/dl under with:")"
check "prints no input value" "$(is "$(contains "$out" 'mirror.example')" false)"

echo "case: a composite action's setup-go step is held to the rule too"
root=$(fixture setup-go-composite)
mkdir -p "$root/.github/actions/local"
cat > "$root/.github/actions/local/action.yml" <<YAML
name: Local
runs:
  using: composite
  steps:
    - uses: actions/setup-go@${SHA} # v7.0.0
      with:
        go-version-file: go.mod
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the action file and line" "$(contains "$out" ".github/actions/local/action.yml:5: 'actions/setup-go@$SHA' lacks go-download-base-url")"

summary "check-action-pins.sh"
