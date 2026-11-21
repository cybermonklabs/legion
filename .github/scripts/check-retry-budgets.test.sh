#!/usr/bin/env bash
# Tests for `check-retry-budgets.sh`: every call of apt-install.sh and changed-files.sh holds a
# budget of at most half the timeout it runs under, no workflow step runs apt itself, and a check
# that read no call says so instead of passing.
#
# Each case builds a fixture tree: the script under test, copied into .github/scripts, and small
# workflows shaped like this repository's callers - the changes job's file read, pi-envoy's tmux
# install under a literal timeout, the repeat workflow's zsh install under a matrix timeout narrowed
# by the step's `if:`, and e2e.yaml's proof job, whose timeout comes from .github/e2e-scripts.json's
# rows. The first case shows that tree passes; every other case plants one fault in it and shows
# the check fails, naming the fault.
#
# Run from anywhere: .github/scripts/check-retry-budgets.test.sh
# CI runs it in ci.yaml's test job.
# shellcheck disable=SC2016 # every $ and ${{ }} in a quoted fixture edit is workflow text, not ours to expand
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# fixture <name>: a fresh fixture tree whose every call fits; prints its root.
fixture() {
  local root="$work/$1"
  mkdir -p "$root/.github/scripts" "$root/.github/workflows" "$root/.github/actions/setup"
  cp "$script_dir/check-retry-budgets.sh" "$root/.github/scripts/"
  cat > "$root/.github/workflows/ci.yaml" <<'YAML'
on: pull_request
jobs:
  changes:
    runs-on: ubuntu-24.04-arm
    timeout-minutes: 10
    steps:
      - name: List the change's files
        run: .github/scripts/changed-files.sh --budget 300 "$RUNNER_TEMP/changed-files"
YAML
  cat > "$root/.github/workflows/ci-job-pi-envoy.yaml" <<'YAML'
on: workflow_call
jobs:
  pi-envoy:
    runs-on: ubuntu-24.04
    timeout-minutes: 10
    steps:
      - name: Install tmux
        run: .github/scripts/apt-install.sh --budget 300 tmux
      - name: Test
        run: bun run test
YAML
  cat > "$root/.github/workflows/repeat.yaml" <<'YAML'
on: workflow_dispatch
jobs:
  repeat:
    runs-on: ubuntu-24.04
    timeout-minutes: ${{ matrix.job_minutes }}
    strategy:
      matrix:
        lane: [web, web-cold, harness]
        include:
          - lane: web
            job_minutes: 17
          - lane: web-cold
            job_minutes: 3
          - lane: harness
            job_minutes: 33
    steps:
      - name: Install zsh
        if: matrix.lane == 'harness'
        run: .github/scripts/apt-install.sh --budget 900 zsh
YAML
  cat > "$root/.github/workflows/e2e.yaml" <<'YAML'
on: workflow_dispatch
jobs:
  proof:
    runs-on: ubuntu-24.04
    timeout-minutes: ${{ matrix.timeout_minutes }}
    strategy:
      matrix:
        include: ${{ fromJSON(needs.select.outputs.proofs) }}
    steps:
      - name: Install this proof's packages
        if: matrix.apt != ''
        env:
          PACKAGES: ${{ matrix.apt }}
        run: |
          .github/scripts/apt-install.sh --budget 300 $PACKAGES
YAML
  cat > "$root/.github/e2e-scripts.json" <<'JSON'
{"hosted": [
  {"script": "stage1-skeleton.sh", "timeout_minutes": 20, "apt": ["tmux"]},
  {"script": "child-adoption.sh", "timeout_minutes": 5, "apt": []}
]}
JSON
  cat > "$root/.github/actions/setup/action.yml" <<'YAML'
name: setup
runs:
  using: composite
  steps:
    - shell: bash
      run: echo set up
YAML
  echo "$root"
}

# replace <file> <old> <new>: replaces the one occurrence of <old>, failing the harness if it is
# not there, so a fixture edit that silently missed cannot read as a pass.
replace() {
  python3 - "$@" <<'PY'
import sys
path, old, new = sys.argv[1:4]
text = open(path).read()
if text.count(old) != 1:
    sys.exit(f"replace: {old!r} occurs {text.count(old)} times in {path}")
open(path, "w").write(text.replace(old, new))
PY
}

# run_check <root>: runs the fixture's copy of the check; sets out and status.
run_check() {
  set +e
  out=$(bash "$1/.github/scripts/check-retry-budgets.sh" 2>&1)
  status=$?
  set -e
}

echo "case: every call fits half the timeout it runs under"
root=$(fixture clean)
run_check "$root"
check "it passes" "$(is "$status" 0)"
check "and counts the calls it read" \
  "$(contains "$out" '^every bounded retry fits half the timeout it runs under: 3 call(s) of apt-install.sh and 1 of changed-files.sh, across 4 workflow(s)$')"
check "the repeat leg is held to its harness row's 33 minutes" \
  "$(contains "$out" 'repeat.yaml job repeat, step Install zsh: apt-install.sh --budget 900, at most half of its job.s timeout-minutes of 33 (1980 s)')"
check "the proof is held to the 20 minutes of the one row that installs a package" \
  "$(contains "$out" 'e2e.yaml job proof, step Install this proof.s packages: apt-install.sh --budget 300, at most half of its job.s timeout-minutes of 20 (1200 s)')"

echo "control: one install's budget planted over half its job's timeout"
root=$(fixture planted)
replace "$root/.github/workflows/ci-job-pi-envoy.yaml" '--budget 300 tmux' '--budget 301 tmux'
run_check "$root"
check "it fails" "$(is "$status" 1)"
check "naming the workflow, job, step, budget and timeout, and the most it may be" \
  "$(contains "$out" "^::error file=.github/workflows/ci-job-pi-envoy.yaml::job pi-envoy, step Install tmux: calls apt-install.sh with a 301 s budget, more than half of its job's timeout-minutes of 10 (600 s), so a call that spends its budget can be cancelled with the job before its error prints; give it at most 300 s\$")"

echo "control: the changes job's reads planted over half its timeout"
root=$(fixture changes-over)
replace "$root/.github/workflows/ci.yaml" 'timeout-minutes: 10' 'timeout-minutes: 9'
run_check "$root"
check "it fails" "$(is "$status" 1)"
check "naming the read's budget and the 9 minutes" \
  "$(contains "$out" "job changes, step List the change's files: calls changed-files.sh with a 300 s budget, more than half of its job's timeout-minutes of 9 (540 s)")"

echo "case: a step's own timeout-minutes below its job's is the one that binds"
root=$(fixture step-timeout)
replace "$root/.github/workflows/ci-job-pi-envoy.yaml" "      - name: Install tmux
" "      - name: Install tmux
        timeout-minutes: 4
"
run_check "$root"
check "it fails" "$(is "$status" 1)"
check "naming the step's 4 minutes" "$(contains "$out" "more than half of its step's timeout-minutes of 4 (240 s)")"

echo "case: a matrix timeout with no if: is held to its smallest row"
root=$(fixture matrix-no-if)
replace "$root/.github/workflows/repeat.yaml" "        if: matrix.lane == 'harness'
" ""
run_check "$root"
check "it fails" "$(is "$status" 1)"
check "naming the web-cold row's 3 minutes" "$(contains "$out" "more than half of its job's timeout-minutes of 3 (180 s)")"

echo "case: an e2e row with packages and a short timeout"
root=$(fixture e2e-short)
replace "$root/.github/e2e-scripts.json" '"timeout_minutes": 5, "apt": []' '"timeout_minutes": 5, "apt": ["postgresql-client"]'
run_check "$root"
check "it fails" "$(is "$status" 1)"
check "naming that row's 5 minutes" "$(contains "$out" "job proof, step Install this proof's packages: calls apt-install.sh with a 300 s budget, more than half of its job's timeout-minutes of 5 (300 s)")"

echo "case: a call with no budget"
root=$(fixture no-budget)
replace "$root/.github/workflows/ci-job-pi-envoy.yaml" '--budget 300 tmux' 'tmux'
run_check "$root"
check "it fails" "$(is "$status" 1)"
check "naming the call" "$(contains "$out" 'job pi-envoy, step Install tmux: calls apt-install.sh with no --budget <seconds> right after it$')"

echo "case: a step that runs apt-get itself, and one that runs apt"
root=$(fixture raw-apt)
replace "$root/.github/workflows/ci-job-pi-envoy.yaml" 'run: bun run test' 'run: sudo apt-get update && sudo apt-get install -y zsh'
replace "$root/.github/workflows/ci.yaml" '"$RUNNER_TEMP/changed-files"' '"$RUNNER_TEMP/changed-files"; sudo apt install -y jq'
run_check "$root"
check "it fails" "$(is "$status" 1)"
check "naming the apt-get step" "$(contains "$out" '^::error file=.github/workflows/ci-job-pi-envoy.yaml::job pi-envoy, step Test: runs apt itself, which no budget bounds')"
check "naming the apt step" "$(contains "$out" "^::error file=.github/workflows/ci.yaml::job changes, step List the change's files: runs apt itself")"
check "counting two problems" "$(contains "$out" '^2 problem(s) with the budgets of bounded retries$')"

echo "case: a job with no timeout-minutes"
root=$(fixture no-timeout)
replace "$root/.github/workflows/ci-job-pi-envoy.yaml" '    timeout-minutes: 10
' ''
run_check "$root"
check "it fails" "$(is "$status" 1)"
check "naming the job" "$(contains "$out" 'job pi-envoy, step Install tmux: calls apt-install.sh in a job with no timeout-minutes')"

echo "case: a timeout this check cannot read is refused, not guessed"
root=$(fixture unread-timeout)
replace "$root/.github/workflows/ci-job-pi-envoy.yaml" 'timeout-minutes: 10' 'timeout-minutes: ${{ inputs.minutes }}'
run_check "$root"
check "it fails" "$(is "$status" 1)"
check "naming the expression" "$(contains "$out" "its job's timeout-minutes is '\${{ inputs.minutes }}', which this check cannot read")"

echo "case: an if: naming the matrix in a form this check does not read"
root=$(fixture unread-if)
replace "$root/.github/workflows/repeat.yaml" "if: matrix.lane == 'harness'" "if: matrix.lane != 'web'"
run_check "$root"
check "it fails" "$(is "$status" 1)"
check "naming the condition" "$(contains "$out" "its if: \"matrix.lane != 'web'\" names the matrix in a form this check cannot read")"

echo "case: a composite action that runs a budgeted script"
root=$(fixture action)
replace "$root/.github/actions/setup/action.yml" 'run: echo set up' 'run: .github/scripts/apt-install.sh --budget 60 tmux'
run_check "$root"
check "it fails" "$(is "$status" 1)"
check "naming the action" "$(contains "$out" '^::error file=.github/actions/setup/action.yml::step 1: a composite action runs apt or a budgeted script')"

echo "case: no call of apt-install.sh at all is UNKNOWN, not a pass"
root=$(fixture none)
replace "$root/.github/workflows/ci-job-pi-envoy.yaml" '.github/scripts/apt-install.sh --budget 300 tmux' 'true'
replace "$root/.github/workflows/repeat.yaml" '.github/scripts/apt-install.sh --budget 900 zsh' 'true'
replace "$root/.github/workflows/e2e.yaml" '.github/scripts/apt-install.sh --budget 300 $PACKAGES' 'true'
run_check "$root"
check "it fails" "$(is "$status" 1)"
check "saying it read nothing" \
  "$(contains "$out" '^::error::found no call of .github/scripts/apt-install.sh in .github/workflows: UNKNOWN, there was nothing to hold to a timeout, so this is no pass$')"

summary "the budgets of the bounded retries CI steps run"
