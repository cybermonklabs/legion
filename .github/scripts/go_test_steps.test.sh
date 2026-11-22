#!/usr/bin/env bash
# Tests for `go_test_steps.py`, rule 8 of `check-workflow-files.sh`: in ci.yaml's run every go test
# step keeps its events through .github/scripts/go_test_events.py, under an id of its own, its job
# uploads them after it with .github/actions/go-test-events-upload, and every script the rule reads
# is one the ci_filters filter selects.
#
# Each case builds a miniature repository under a temporary directory and symlinks the real check
# into it, which imports the module from beside its own file, so the code under test is the code CI
# runs, read through the check's own output and exit status.
#
# Run from anywhere: .github/scripts/go_test_steps.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check-workflow-files.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# colon is the colon of a path:line that the check under test prints. It is built at run
# time so that this file's text holds no path followed by a colon and a line number, which
# ci_citations.py refuses wherever it stands under .github.
colon=':'

# fixture <name>: a tree with the check linked in and a ci.yaml whose one job calls ci-job-unit.yaml,
# the file each case writes its job into. Echoes its root.
fixture() {
  local root="$work/$1"
  if [ -e "$root" ]; then
    echo "  FAIL: the fixture name '$1' is already taken by an earlier case" >&2
    return 1
  fi
  mkdir -p "$root/.github/scripts" "$root/.github/workflows"
  ln -s "$check_script" "$root/.github/scripts/check-workflow-files.sh"
  cat > "$root/.github/workflows/ci.yaml" <<'YAML'
name: CI
on: pull_request
jobs:
  unit:
    uses: ./.github/workflows/ci-job-unit.yaml
YAML
  echo "$root"
}

# unit_job <root>: the fixture's ci-job-unit.yaml, whose first job `unit` runs on ubuntu-latest, the
# rest of that job and any job after it read from stdin.
unit_job() {
  { printf 'name: CI job unit\non: workflow_call\njobs:\n  unit:\n    runs-on: ubuntu-latest\n'; cat; } \
    > "$1/.github/workflows/ci-job-unit.yaml"
}

# run_check <root> [<filters file>]: the check over the tree, its output in $out and its status in
# $status. CI_FILTERS_FILE is the file named, or empty, so a case never reads filters from the
# environment the harness runs in.
run_check() {
  local root=$1 filters=${2:-}
  set +e
  out=$(cd "$root" && CI_FILTERS_FILE="$filters" "./.github/scripts/check-workflow-files.sh" 2>&1)
  status=$?
  set -e
}

# the upload step rule 8 asks of a job that runs go test: the repository's action, given the job's id.
upload_step='      - name: Upload the go test events
        if: always()
        uses: ./.github/actions/go-test-events-upload
        with:
          check-run-id: ${{ job.check_run_id }}'

# events_scripts <root>: the scripts the rule 8 cases run, in the fixture's tree: a counter that
# appends its own -json report to TEST_EVENTS_FILE, a script that runs go test bare and one that
# runs that one, a script that runs go test under the writer and one that runs that one, a script
# that runs go test only in a quoted string and a heredoc, and a fixture harness of FIXTURES' list,
# whose go test runs are its own.
events_scripts() {
  mkdir -p "$1/packages/envoy/scripts" "$1/packages/x/scripts" "$1/.github/scripts"
  cat > "$1/packages/envoy/scripts/database-skips.sh" <<'SH'
#!/bin/sh
go test -json ./... | tee -a "$TEST_EVENTS_FILE"
SH
  cat > "$1/packages/x/scripts/unit.sh" <<'SH'
#!/bin/sh
cd "$(dirname "$0")/.." && go test ./pkg
SH
  cat > "$1/packages/x/scripts/outer.sh" <<'SH'
#!/bin/sh
dir=$(dirname "$0")
bash "$dir/unit.sh"
SH
  cat > "$1/packages/x/scripts/kept.sh" <<'SH'
#!/bin/sh
exec ../../.github/scripts/go_test_events.py go test ./pkg
SH
  cat > "$1/packages/x/scripts/kept-outer.sh" <<'SH'
#!/bin/sh
dir=$(dirname "$0")
sh "$dir/kept.sh"
SH
  cat > "$1/packages/x/scripts/words.sh" <<'SH'
#!/bin/sh
echo "run go test ./pkg to see"
cat <<EOF
go test ./pkg
EOF
SH
  cat > "$1/.github/scripts/go_test_events.test.sh" <<'SH'
#!/bin/sh
go test ./fixture-module/...
SH
}

echo "case: go test steps of ci.yaml's run that keep their events, under ids, uploaded after them, pass and are counted"
root=$(fixture events-kept)
events_scripts "$root"
unit_job "$root" <<YAML
    steps:
      - name: Unit tests
        id: unit
        run: |
          set -o pipefail
          go list ./... | grep -v /live | xargs ../../.github/scripts/go_test_events.py go test -count=1
          ../../.github/scripts/go_test_events.py go test -count=1 -v -run "\$pattern" ./cmd/x
          "../../.github/scripts/go_test_events.py" go -C mod test ./pkg
          ../../.github/scripts/go_test_events.py \\
            go test ./continued/
          echo "go test ./pkg is how to run it by hand"
          scripts/words.sh
        working-directory: packages/x
      - name: Race tests under the sampler
        id: race
        run: RACE_MEM_LIMIT_KB=1 ../../.github/scripts/go_test_events.py ../../.github/scripts/race_memory.py go test -json -race ./y/
      - name: The skip counter
        id: skips
        run: .github/scripts/go_test_events.py packages/envoy/scripts/database-skips.sh --timeout 20m
      - name: A script two levels down, under the writer as a whole
        id: outer
        if: inputs.unit == 'true'
        run: ../../.github/scripts/go_test_events.py scripts/kept-outer.sh
        working-directory: packages/x
      - name: Test the writer, a fixture harness
        run: .github/scripts/go_test_events.test.sh
      - name: Fill the build cache
        run: go test -c -o /dev/null ./...
$upload_step
YAML
# A workflow ci.yaml does not reach is not ci.yaml's run, whatever its go test steps do.
cat > "$root/.github/workflows/release.yaml" <<'YAML'
name: Release
on: push
jobs:
  smoke:
    runs-on: ubuntu-latest
    steps:
      - run: go test ./smoke/
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the seven commands, four steps and one job it holds to the rule" "$(contains "$out" 'keep the events of 7 go test command(s) in 4 step(s) of 1 job(s) of ci.yaml.s run, ')"
check "and with no CI_FILTERS_FILE says whether a change to its scripts starts the job is UNKNOWN" \
  "$(contains "$out" 'run, and whether a change to the [0-9]* script(s) rule 8 read starts the ci-filters job UNKNOWN: no CI_FILTERS_FILE names the filters ci.yaml.s changes job computed$')"

echo "case: control: a go test step that keeps no events is refused by name, however it spells or reaches go test"
root=$(fixture events-bare)
events_scripts "$root"
unit_job "$root" <<YAML
    steps:
      - name: Unit tests
        id: unit
        run: go test -count=1 ./...
      - name: Integration tests
        id: integration
        run: |
          ../../.github/scripts/go_test_events.py go test ./a/
          go list ./... | xargs go test -count=1
      - name: The skip counter
        id: skips
        run: packages/envoy/scripts/database-skips.sh --timeout 20m
      - name: Race tests under the sampler alone
        id: race
        run: ../../.github/scripts/race_memory.py go test -json -race ./y/
      - name: A script that runs go test
        id: script
        run: scripts/unit.sh
        working-directory: packages/x
      - name: A script that runs that script
        id: outer
        run: bash scripts/outer.sh
        working-directory: packages/x
      - name: Go with a directory
        id: go-c
        run: go -C mod test ./pkg
      - name: A continued line
        id: continued
        run: |
          go \\
            test ./pkg
      - name: A test binary's own -c
        id: args-c
        run: go test ./argsflag -args -c
$upload_step
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the bare go test step, its file, job, number and line" \
  "$(contains "$out" '^::error file=.github/workflows/ci-job-unit.yaml,line=7::.github/workflows/ci-job-unit.yaml:7: step 1 of job unit (Unit tests) runs `go test -count=1 ./...` other than through .github/scripts/go_test_events.py')"
check "names the bare line of a step whose other line keeps its events" "$(contains "$out" 'step 2 of job unit (Integration tests) runs `go test -count=1` other than through')"
check "names the skip counter run bare" \
  "$(contains "$out" "step 3 of job unit (The skip counter) runs packages/envoy/scripts/database-skips.sh, which runs \`go test -json ./...\` (packages/envoy/scripts/database-skips.sh${colon}2) other than through")"
check "names the sampler run outside the writer" \
  "$(contains "$out" 'step 4 of job unit (Race tests under the sampler alone) runs `../../.github/scripts/race_memory.py go test -json -race ./y/` other than through')"
check "names a script of no list, by the go test line it runs" \
  "$(contains "$out" 'step 5 of job unit (A script that runs go test) runs packages/x/scripts/unit.sh, which runs `go test ./pkg` (packages/x/scripts/unit.sh:2) other than through')"
check "follows a script into the script it runs" \
  "$(contains "$out" 'step 6 of job unit (A script that runs that script) runs packages/x/scripts/outer.sh, which runs `go test ./pkg` (packages/x/scripts/unit.sh:2) other than through')"
check "reads go -C <dir> test" "$(contains "$out" 'step 7 of job unit (Go with a directory) runs `go -C mod test ./pkg` other than through')"
check "reads a go test continued over two lines" "$(contains "$out" 'step 8 of job unit (A continued line) runs `go test ./pkg` other than through')"
check "reads a -c after -args as the binary's, not go test's compile-only flag" \
  "$(contains "$out" 'step 9 of job unit (A test binary.s own -c) runs `go test ./argsflag -args -c` other than through')"
check "says what the log keeps without it" "$(contains "$out" 'go test prints `ok <package> <time>` alone')"
check "refuses those nine and nothing else" "$(contains "$out" '^9 problem(s) across the 2 workflow file(s) under .github/workflows$')"

echo "case: a kept go test step with no id, a job whose upload could not run or is not the action's, a script whose events go nowhere and a step's own TEST_EVENTS_FILE are refused"
root=$(fixture events-unnamed)
events_scripts "$root"
unit_job "$root" <<YAML
    steps:
$upload_step
      - name: Unit tests
        run: ../../.github/scripts/go_test_events.py go test -count=1 ./...
  other:
    runs-on: ubuntu-latest
    steps:
      - name: Other tests
        id: other
        run: ../../.github/scripts/go_test_events.py go test -count=1 ./other/
      - name: Upload on success alone
        uses: ./.github/actions/go-test-events-upload
        with:
          check-run-id: \${{ job.check_run_id }}
  never:
    runs-on: ubuntu-latest
    steps:
      - name: Never-uploaded tests
        id: never
        run: ../../.github/scripts/go_test_events.py go test -count=1 ./never/
      - name: An upload whose condition is false
        if: always() && false
        uses: ./.github/actions/go-test-events-upload
        with:
          check-run-id: \${{ job.check_run_id }}
  leaky:
    runs-on: ubuntu-latest
    steps:
      - name: A bare script under the writer
        id: leaky
        run: ../../.github/scripts/go_test_events.py scripts/unit.sh
        working-directory: packages/x
      - name: Elsewhere
        id: elsewhere
        env:
          TEST_EVENTS_FILE: elsewhere/events.jsonl
        run: ../../.github/scripts/go_test_events.py go test ./pkg
$upload_step
  direct:
    runs-on: ubuntu-latest
    steps:
      - name: Directly uploaded tests
        id: direct
        run: ../../.github/scripts/go_test_events.py go test -count=1 ./direct/
      - name: The upload written out instead of the action
        if: always()
        uses: actions/upload-artifact@043fb46d1a93c77aae656e7c1c64a875d1fc6a0a # v7.0.1
        with:
          name: go-test-events-\${{ job.check_run_id }}
          path: \${{ runner.temp }}/go-test-events/
          if-no-files-found: error
  run-id:
    runs-on: ubuntu-latest
    steps:
      - name: Tests uploaded under the run's id
        id: run-id
        run: ../../.github/scripts/go_test_events.py go test -count=1 ./run-id/
      - name: The action handed another id
        if: always()
        uses: ./.github/actions/go-test-events-upload
        with:
          check-run-id: \${{ github.run_id }}
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the step with no id" \
  "$(contains "$out" 'line=12::.github/workflows/ci-job-unit.yaml:12: step 2 of job unit (Unit tests) runs go test through .github/scripts/go_test_events.py and has no id: of its own')"
check "names the job whose upload comes before its go test step" "$(contains "$out" 'job unit runs go test (its last such step is step 2, Unit tests) and no step after it uploads the events')"
check "names the job whose upload is not if: always()" "$(contains "$out" 'job other runs go test (its last such step is step 1, Other tests) and no step after it uploads the events')"
check "names the job whose upload's condition holds more than always()" \
  "$(contains "$out" 'job never runs go test (its last such step is step 1, Never-uploaded tests) and no step after it uploads the events')"
check "names the script under the writer whose go test neither runs under it nor tees into TEST_EVENTS_FILE" \
  "$(contains "$out" 'step 1 of job leaky (A bare script under the writer) runs a script under .github/scripts/go_test_events.py, and the script runs `go test ./pkg` (packages/x/scripts/unit.sh:2), which neither runs under that writer nor tees its output into TEST_EVENTS_FILE on its own line')"
check "names the step that sets TEST_EVENTS_FILE itself" "$(contains "$out" 'step 2 of job leaky (Elsewhere) sets TEST_EVENTS_FILE, which only .github/scripts/go_test_events.py sets')"
check "names the job that writes the upload out rather than run the action" \
  "$(contains "$out" 'job direct runs go test (its last such step is step 1, Directly uploaded tests) and no step after it uploads the events')"
check "names the job that hands the action an id other than its own" \
  "$(contains "$out" 'job run-id runs go test (its last such step is step 1, Tests uploaded under the run.s id) and no step after it uploads the events')"
check "says which step to add: the action's own uses line, its condition and the job's id" \
  "$(contains "$out" 'add a step `uses: ./.github/actions/go-test-events-upload` with `if: always()` (or `always() && ` the one condition its go test steps share) and `with:` check-run-id: \${{ job.check_run_id }}, so a reader fetches them by the job.s id')"
check "and prints no placeholder for a pin" "$(is "$(contains "$out" '<pin>')" false)"
check "refuses those eight and nothing else" "$(contains "$out" '^8 problem(s) across the 2 workflow file(s) under .github/workflows$')"

# events_more_scripts <root>: the scripts of the second pair of rule 8 cases: one a script runs by
# `source`, one with no suffix and a shell's #! line, one that runs go test under the writer
# itself, one whose comment names TEST_EVENTS_FILE beside a go test that keeps nothing.
events_more_scripts() {
  mkdir -p "$1/packages/x/scripts"
  printf '#!/bin/sh\ngo test ./sourced\n' > "$1/packages/x/scripts/sourced.sh"
  printf '#!/usr/bin/env bash\ngo test ./nosuffix\n' > "$1/packages/x/scripts/unit"
  printf '#!/bin/sh\nexec ../../.github/scripts/go_test_events.py go test ./pkg\n' > "$1/packages/x/scripts/kept.sh"
  printf '#!/bin/sh\n# TEST_EVENTS_FILE is inherited.\n../../.github/scripts/go_test_events.py go test ./one\ngo test ./two\n' \
    > "$1/packages/x/scripts/mixed.sh"
  # Three go tests whose tees keep no event: one that writes no -json, one whose tee is a
  # comment, one that tees into another file.
  printf '%s\n' '#!/bin/sh' \
    'go test -count=1 ./plain | tee -a "$TEST_EVENTS_FILE" >/dev/null' \
    'go test -json -count=1 ./comment # | tee -a "$TEST_EVENTS_FILE"' \
    'go test -json -count=1 ./suffix | tee -a "${TEST_EVENTS_FILE}.other" >/dev/null' \
    > "$1/packages/x/scripts/tees.sh"
  # A go command held in an array, as packages/envoy/scripts/forced-order.sh builds its: a -list
  # of it runs no test, and a run of it does.
  printf '%s\n' '#!/usr/bin/env bash' 'go_cmd=(env GOWORK=off go)' \
    "\"\${go_cmd[@]}\" test -list '^Test' ./v" '"${go_cmd[@]}" test -count=1 ./v' \
    > "$1/packages/x/scripts/varcmd.sh"
}

echo "case: a script that keeps its own events, an \${{ }} condition, an echoed TEST_EVENTS_FILE= and -c=true pass"
root=$(fixture events-kept-more)
events_more_scripts "$root"
unit_job "$root" <<YAML
    steps:
      - name: A script that runs go test under the writer itself
        id: inside
        if: \${{ inputs.unit == 'true' }}
        run: |
          echo "TEST_EVENTS_FILE=example.jsonl is what the writer names"
          scripts/kept.sh
          go test -c=true -o /dev/null ./pkg
        working-directory: packages/x
      - name: Upload the go test events
        if: \${{ always() && inputs.unit == 'true' }}
        uses: ./.github/actions/go-test-events-upload
        with:
          check-run-id: \${{ job.check_run_id }}
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the one script, its step and its job" "$(contains "$out" 'keep the events of 1 go test command(s) in 1 step(s) of 1 job(s) of ci.yaml.s run, ')"

echo "case: control: go test run by a shell's heredoc, quoted, by source, with no suffix, from the workflow's directory, with -c=false, or with no upload, is refused"
root=$(fixture events-bare-more)
events_more_scripts "$root"
cat > "$root/.github/workflows/ci-job-unit.yaml" <<YAML
name: CI job unit
on: workflow_call
defaults:
  run:
    working-directory: packages/x
jobs:
  unit:
    runs-on: ubuntu-latest
    steps:
      - name: A heredoc bash runs
        id: heredoc-bash
        run: |
          bash <<'SCRIPT'
          go test ./a
          SCRIPT
      - name: A heredoc's substitution
        id: heredoc-substitution
        run: |
          cat <<SCRIPT
          \$(go test ./b)
          SCRIPT
      - name: Quoted go and a quoted assignment
        id: quoted
        run: |
          "go" test ./c
          GODEBUG="checkfinalizers=1" go test ./d
      - name: A sourced script
        id: sourced
        run: source scripts/sourced.sh
      - name: A script with no suffix, from the workflow's default directory
        id: nosuffix
        run: bash scripts/unit
      - name: -c=false runs the tests
        id: compile-false
        run: go test -c=false ./e
      - name: A text heredoc runs nothing
        id: text
        run: |
          cat <<'TEXT'
          go test ./f
          TEXT
      - name: A string bash -c runs
        id: bash-c
        run: bash -c 'go test -count=1 ./h'
      - name: A heredoc env bash runs
        id: env-heredoc
        run: |
          env bash <<'SCRIPT'
          go test -count=1 ./i
          SCRIPT
      - name: A backquoted go test
        id: backquoted
        run: result=\`go test -count=1 ./j\`
      - name: A go command held in an array
        id: varcmd
        run: scripts/varcmd.sh
$upload_step
  tees:
    runs-on: ubuntu-latest
    steps:
      - name: Tees that keep nothing
        id: tees
        run: ../../.github/scripts/go_test_events.py scripts/tees.sh
$upload_step
  mixed:
    runs-on: ubuntu-latest
    steps:
      - name: A script whose comment names the file and whose go test keeps nothing
        id: mixed
        run: ../../.github/scripts/go_test_events.py scripts/mixed.sh
$upload_step
  python-writer:
    runs-on: ubuntu-latest
    steps:
      - name: The writer run by python3, no upload
        id: python-writer
        run: python3 ../../.github/scripts/go_test_events.py go test ./g
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "reads a heredoc bash runs" "$(contains "$out" 'step 1 of job unit (A heredoc bash runs) runs `go test ./a` other than through')"
check "reads a substitution in an unquoted heredoc" "$(contains "$out" 'step 2 of job unit (A heredoc.s substitution) runs `go test ./b` other than through')"
check "reads a quoted go and a quoted assignment's go test" "$(contains "$out" 'step 3 of job unit (Quoted go and a quoted assignment) runs `"go" test ./c`, `go test ./d` other than through')"
check "follows source" "$(contains "$out" 'step 4 of job unit (A sourced script) runs packages/x/scripts/sourced.sh, which runs `go test ./sourced`')"
check "follows a script with no suffix from the workflow's directory" \
  "$(contains "$out" 'step 5 of job unit (A script with no suffix, from the workflow.s default directory) runs packages/x/scripts/unit, which runs `go test ./nosuffix`')"
check "reads -c=false as a run" "$(contains "$out" 'step 6 of job unit (-c=false runs the tests) runs `go test -c=false ./e` other than through')"
check "leaves a text heredoc alone" "$(is "$(contains "$out" 'A text heredoc runs nothing')" false)"
check "reads the string bash -c runs" "$(contains "$out" 'step 8 of job unit (A string bash -c runs) runs `go test -count=1 ./h` other than through')"
check "reads a heredoc env bash runs" "$(contains "$out" 'step 9 of job unit (A heredoc env bash runs) runs `go test -count=1 ./i` other than through')"
check "reads a backquoted go test" "$(contains "$out" 'step 10 of job unit (A backquoted go test) runs `go test -count=1 ./j` other than through')"
check "reads a go command held in an array, past the -list of it, which runs no test" \
  "$(contains "$out" 'step 11 of job unit (A go command held in an array) runs packages/x/scripts/varcmd.sh, which runs `"${go_cmd\[@\]}" test -count=1 ./v` (packages/x/scripts/varcmd.sh:4) other than through')"
check "holds a tee to go test -json, to the file itself and to the line outside its comment" \
  "$(contains "$out" 'step 1 of job tees (Tees that keep nothing) runs a script under .github/scripts/go_test_events.py, and the script runs `go test -count=1 ./plain` (packages/x/scripts/tees.sh:2), `go test -json -count=1 ./comment` (packages/x/scripts/tees.sh:3), `go test -json -count=1 ./suffix` (packages/x/scripts/tees.sh:4), which neither')"
check "holds a script's go test to its own line, not a comment naming the file" \
  "$(contains "$out" 'step 1 of job mixed (A script whose comment names the file and whose go test keeps nothing) runs a script under .github/scripts/go_test_events.py, and the script runs `go test ./two` (packages/x/scripts/mixed.sh:4)')"
check "counts the writer python3 runs, and asks its job for the upload" \
  "$(contains "$out" 'job python-writer runs go test (its last such step is step 1, The writer run by python3, no upload) and no step after it uploads the events')"
check "refuses those thirteen and nothing else" "$(contains "$out" '^13 problem(s) across the 2 workflow file(s) under .github/workflows$')"

# pool_upload_job <root> <order>: a ci-job-unit.yaml whose job queues a kept go test with
# harness-pool.sh start and then, in <order>'s order, uploads and waits: "wait" is a wait under
# `if: ${{ !cancelled() }}`, as the test job's own is, "falsewait" one the job never runs, and
# "plainwait" one with no `if:`, which a failed step before it skips.
pool_upload_job() {
  local root=$1 order=$2 steps="" part
  for part in $order; do
    case $part in
      upload) steps+="$upload_step"$'\n' ;;
      wait) steps+='      - name: Wait for the script harnesses
        if: ${{ !cancelled() }}
        run: exec .github/scripts/harness-pool.sh wait
' ;;
      falsewait) steps+='      - name: A wait the job never runs
        if: ${{ false }}
        run: exec .github/scripts/harness-pool.sh wait
' ;;
      plainwait) steps+='      - name: A wait a failed step skips
        run: exec .github/scripts/harness-pool.sh wait
' ;;
    esac
  done
  unit_job "$root" <<YAML
    steps:
      - name: Queue the unit tests
        id: pooled
        run: .github/scripts/harness-pool.sh start unit .github/scripts/go_test_events.py go test -count=1 ./pkg
$steps
YAML
}

echo "case: a go test harness-pool.sh runs keeps its events only once a wait after its start has returned, so the upload comes after the wait"
root=$(fixture events-pool-wait-first)
pool_upload_job "$root" "wait upload"
run_check "$root"
check "control: start, wait, then upload passes" "$(is "$status" 0)"
check "and counts the pooled command" "$(contains "$out" 'keep the events of 1 go test command(s) in 1 step(s) of 1 job(s)')"
root=$(fixture events-pool-upload-first)
pool_upload_job "$root" "upload wait"
run_check "$root"
check "an upload before the pool's wait is refused" "$(is "$status" 1)"
check "naming the wait it must follow" \
  "$(contains "$out" 'job unit runs go test (its last such step is step 1, Queue the unit tests, and the pool it queues go test in is waited for at step 3) and no step after it uploads the events')"
root=$(fixture events-pool-no-wait)
pool_upload_job "$root" "upload"
run_check "$root"
check "a pooled go test no step waits for is refused" "$(is "$status" 1)"
check "by name" "$(contains "$out" 'job unit queues a go test with harness-pool.sh start (step 1, Queue the unit tests) and no step after it waits for the pool')"

echo "case: a go test's flags are read as the shell splits them: a quoted -run word, an empty -list, a nested substitution and a Python argv"
root=$(fixture events-flag-words)
mkdir -p "$root/packages/x/scripts"
printf '%s\n' '#!/bin/sh' \
  'go test -json -count=1 -run "$(printf TestTwo)" ./pkg | tee -a "$TEST_EVENTS_FILE" >/dev/null' \
  > "$root/packages/x/scripts/nested.sh"
printf '%s\n' '"""Prints a leg'"'"'s packages, which a step pipes into' 'go test ./... of its own."""' \
  'print("./pkg")' > "$root/packages/x/scripts/legs.py"
printf '%s\n' 'import subprocess' 'subprocess.run(["go", "test", "-count=1", "./pkg"], check=True)' \
  > "$root/packages/x/scripts/argv.py"
unit_job "$root" <<YAML
    steps:
      - name: A -c inside the quoted -run pattern
        id: pattern-c
        run: go test -v -count=1 -run '^(TestTwo| -c )\$' ./pkg
      - name: An empty -list runs the tests
        id: empty-list
        run: go test -v -count=1 -list= ./pkg
      - name: A -list with a pattern runs none
        id: list
        run: go test -list 'Test' ./pkg
      - name: A nested substitution, teed, under the writer
        id: nested
        run: ../../.github/scripts/go_test_events.py scripts/nested.sh
        working-directory: packages/x
      - name: A Python script whose docstring names go test
        id: legs
        run: python3 scripts/legs.py | xargs ../../.github/scripts/go_test_events.py go test -count=1
        working-directory: packages/x
      - name: A Python script that runs go test as an argv
        id: argv
        run: python3 scripts/argv.py
        working-directory: packages/x
$upload_step
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "a -c inside the quoted pattern is the pattern's: the step runs a test" \
  "$(contains "$out" "step 1 of job unit (A -c inside the quoted -run pattern) runs \`go test -v -count=1 -run '^(TestTwo| -c )\\$' ./pkg\` other than through")"
check "an empty -list runs the tests" "$(contains "$out" 'step 2 of job unit (An empty -list runs the tests) runs `go test -v -count=1 -list= ./pkg` other than through')"
check "a -list with a pattern runs none" "$(is "$(contains "$out" 'A -list with a pattern runs none')" false)"
check "a substitution's ) is the argument's, and the tee after it keeps the events" "$(is "$(contains "$out" 'A nested substitution')" false)"
check "a Python docstring's go test is prose" "$(is "$(contains "$out" 'A Python script whose docstring names go test')" false)"
check "a Python argv's go test is a run" \
  "$(contains "$out" "step 6 of job unit (A Python script that runs go test as an argv) runs packages/x/scripts/argv.py, which runs \`\\['go', 'test', '-count=1', './pkg'\\] (a Python argv)\`")"
check "refuses those three and nothing else" "$(contains "$out" '^3 problem(s) across the 2 workflow file(s) under .github/workflows$')"

echo "case: a pool wait the job can skip is no wait: one under if: false, and one with no if, which a failed step skips"
root=$(fixture events-pool-false-wait)
pool_upload_job "$root" "falsewait upload wait"
run_check "$root"
check "an upload after a wait the job never runs is refused" "$(is "$status" 1)"
check "naming the skippable wait, and the wait that counts" \
  "$(contains "$out" 'waited for at step 4; its wait at step 2 runs only when `false` holds, so the job can skip it, and only the wait at step 4 counts) and no step after it uploads the events')"
root=$(fixture events-pool-plain-wait)
pool_upload_job "$root" "plainwait upload"
run_check "$root"
check "and so is one after a wait with no if:" "$(is "$status" 1)"
check "which runs only on success()" "$(contains "$out" 'its wait for the pool, step 2, runs only when `success()` holds')"
root=$(fixture events-pool-false-then-guaranteed)
pool_upload_job "$root" "falsewait wait upload"
run_check "$root"
check "control: a skippable wait before a wait the job always runs, then the upload, passes" "$(is "$status" 0)"

echo "case: a flag's value is the flag's: -skip '-c' runs tests, and -skip '-json' writes no events"
root=$(fixture events-flag-values)
mkdir -p "$root/packages/x/scripts"
printf '%s\n' '#!/bin/sh' "go test -count=1 -skip '-json' ./two | tee -a \"\$TEST_EVENTS_FILE\" >/dev/null" \
  > "$root/packages/x/scripts/skipjson.sh"
unit_job "$root" <<YAML
    steps:
      - name: A -skip whose pattern is -c
        id: skip-c
        run: go test -v -count=1 -skip '-c' ./two
      - name: The same pattern after =
        id: skip-c-equals
        run: go test -v -count=1 -skip=-c ./two
      - name: A tee of a go test whose -skip pattern is -json
        id: skip-json
        run: ../../.github/scripts/go_test_events.py scripts/skipjson.sh
        working-directory: packages/x
      - name: A -run value that is no package, then a -c that compiles only
        id: run-then-c
        run: go test -run TestTwo -c -o /dev/null ./two
$upload_step
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "-skip '-c' runs tests, so its step is refused" "$(contains "$out" "step 1 of job unit (A -skip whose pattern is -c) runs \`go test -v -count=1 -skip '-c' ./two\` other than through")"
check "-skip=-c too" "$(contains "$out" 'step 2 of job unit (The same pattern after =) runs `go test -v -count=1 -skip=-c ./two` other than through')"
check "-skip '-json' writes plain output, so its tee keeps no event" \
  "$(contains "$out" "step 3 of job unit (A tee of a go test whose -skip pattern is -json) runs a script under .github/scripts/go_test_events.py, and the script runs \`go test -count=1 -skip '-json' ./two\`")"
check "a -c after a -run value still compiles only" "$(is "$(contains "$out" 'A -run value that is no package')" false)"
check "refuses those three and nothing else" "$(contains "$out" '^3 problem(s) across the 2 workflow file(s) under .github/workflows$')"

echo "case: a Python script is read with Python's parser: a commented argv runs, prose and other argvs do not, and -c compiles only"
root=$(fixture events-python-ast)
mkdir -p "$root/packages/x/scripts"
printf '%s\n' 'import subprocess' 'subprocess.run([' '    "go",  # the Go executable' '    "test", "-v", "-count=1", "./two",' '], check=True)' \
  > "$root/packages/x/scripts/commented.py"
printf '%s\n' '"""An argv example: ["go", "test", "./two"]."""' 'print("documentation only")' \
  > "$root/packages/x/scripts/docstring.py"
printf '%s\n' '# subprocess.run(["go", "test", "./two"])' 'print("comment only")' > "$root/packages/x/scripts/comment.py"
printf '%s\n' 'import subprocess' 'subprocess.run(["echo", "go", "test"], check=True)' > "$root/packages/x/scripts/echo.py"
printf '%s\n' 'import subprocess' 'subprocess.run(["go", "test", "-c", "-o", "compiled.test", "./two"], check=True)' \
  > "$root/packages/x/scripts/compile.py"
printf '%s\n' 'import subprocess, sys' 'subprocess.run([' '    "../../.github/scripts/go_test_events.py",' '    "go",  # kept' \
  '    "test", "-count=1", "./two",' '], check=True)' > "$root/packages/x/scripts/kept.py"
unit_job "$root" <<YAML
    defaults:
      run:
        working-directory: packages/x
    steps:
      - name: A commented argv
        id: commented
        run: python3 scripts/commented.py
      - name: A docstring that holds an argv
        id: docstring
        run: python3 scripts/docstring.py
      - name: A comment that holds an argv
        id: comment
        run: python3 scripts/comment.py
      - name: An echo of go test
        id: echo
        run: python3 scripts/echo.py
      - name: A go test -c argv
        id: compile
        run: python3 scripts/compile.py
      - name: An argv under the writer, a comment inside it
        id: kept
        run: python3 scripts/kept.py
$upload_step
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "a comment inside the argv hides no run, and the argv is named without it" \
  "$(contains "$out" "step 1 of job unit (A commented argv) runs packages/x/scripts/commented.py, which runs \`\\['go', 'test', '-v', '-count=1', './two'\\] (a Python argv)\`")"
check "a docstring's argv is prose" "$(is "$(contains "$out" 'A docstring that holds an argv')" false)"
check "a comment's argv is prose" "$(is "$(contains "$out" 'A comment that holds an argv')" false)"
check "go test as another command's arguments is no go run" "$(is "$(contains "$out" 'An echo of go test')" false)"
check "an argv whose -c compiles only runs no test" "$(is "$(contains "$out" 'A go test -c argv')" false)"
check "an argv the writer heads keeps its events, a comment inside it or not" "$(is "$(contains "$out" 'An argv under the writer')" false)"
check "refuses that one and nothing else" "$(contains "$out" '^1 problem(s) across the 2 workflow file(s) under .github/workflows$')"

echo "case: go test's words end where go's do: at --, at a bare -args, and not at a -skip value of -args"
root=$(fixture events-word-ends)
mkdir -p "$root/packages/x/scripts"
printf '%s\n' '#!/bin/sh' "go test -count=1 -skip '-args' -json ./two | tee -a \"\$TEST_EVENTS_FILE\" >/dev/null" \
  > "$root/packages/x/scripts/argsvalue.sh"
unit_job "$root" <<YAML
    steps:
      - name: A -c after --, which go hands the test binary
        id: dashdash-c
        run: go test -v -count=1 ./two -- -c
      - name: A -test.list after --
        id: dashdash-list
        run: go test -count=1 ./two -- -test.list=TestTwo
      - name: A -c after -args, the binary's
        id: args-c
        run: go test -count=1 ./two -args -c
      - name: A tee of a go test whose -skip pattern is -args, then -json
        id: args-value
        run: ../../.github/scripts/go_test_events.py scripts/argsvalue.sh
        working-directory: packages/x
$upload_step
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "a -c after -- is the binary's: the step runs tests" \
  "$(contains "$out" 'step 1 of job unit (A -c after --, which go hands the test binary) runs `go test -v -count=1 ./two -- -c` other than through')"
check "so is a -test.list after --" "$(contains "$out" 'step 2 of job unit (A -test.list after --) runs `go test -count=1 ./two -- -test.list=TestTwo` other than through')"
check "and a -c after -args" "$(contains "$out" 'step 3 of job unit (A -c after -args, the binary.s) runs `go test -count=1 ./two -args -c` other than through')"
check "a -skip value of -args ends nothing: the -json after it writes events, which the tee keeps" "$(is "$(contains "$out" 'A tee of a go test whose -skip pattern is -args')" false)"
check "refuses those three and nothing else" "$(contains "$out" '^3 problem(s) across the 2 workflow file(s) under .github/workflows$')"

echo "case: a value no check can read is no evidence that no test runs, and a list a script prints is data"
root=$(fixture events-unknown-values)
mkdir -p "$root/packages/x/scripts"
printf '%s\n' 'import subprocess' 'subprocess.run([' '    "go", "test", "-v", "-count=1",' '    "-list", str(), "./two",' '], check=True)' \
  > "$root/packages/x/scripts/computed.py"
printf '%s\n' 'print(["go", "test", "./two"])' 'print(f"would run {["go", "test", "./two"]}")' > "$root/packages/x/scripts/printed.py"
unit_job "$root" <<YAML
    defaults:
      run:
        working-directory: packages/x
    steps:
      - name: A Python -list whose pattern is computed
        id: computed
        run: python3 scripts/computed.py
      - name: A shell -list whose pattern is a variable
        id: shell-list
        run: go test -v -count=1 -list "\$pattern" ./two
      - name: A shell -c whose value is a variable
        id: shell-c
        run: go test -v -count=1 -c=\$compile ./two
      - name: A script that prints a go test argv
        id: printed
        run: python3 scripts/printed.py
$upload_step
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "a computed -list pattern may be empty: the argv runs tests" \
  "$(contains "$out" "step 1 of job unit (A Python -list whose pattern is computed) runs packages/x/scripts/computed.py, which runs \`\\['go', 'test', '-v', '-count=1', '-list', str(), './two'\\] (a Python argv)\`")"
check "so may a shell variable's" "$(contains "$out" 'step 2 of job unit (A shell -list whose pattern is a variable) runs `go test -v -count=1 -list "$pattern" ./two` other than through')"
check "and a -c whose value is a variable may be false" "$(contains "$out" 'step 3 of job unit (A shell -c whose value is a variable) runs `go test -v -count=1 -c=$compile ./two` other than through')"
check "a printed or formatted argv is data" "$(is "$(contains "$out" 'A script that prints a go test argv')" false)"
check "refuses those three and nothing else" "$(contains "$out" '^3 problem(s) across the 2 workflow file(s) under .github/workflows$')"

echo "case: a \$ the shell leaves alone is literal, a list nested in printed data is data, and a joined argv the shell runs is a run"
root=$(fixture events-literal-dollar)
mkdir -p "$root/packages/x/scripts"
printf '%s\n' 'import subprocess' 'subprocess.run(["go", "test", "-count=1", "-list", "^Test.*$", "./two"], check=True)' \
  > "$root/packages/x/scripts/anchored.py"
printf '%s\n' 'print({"argv": ["go", "test", "./two"]})' 'print([("go", "test", "./two")])' 'print(" ".join(["go", "test", "./two"]))' \
  > "$root/packages/x/scripts/nested.py"
printf '%s\n' 'import subprocess' 'print(subprocess.run(["go", "test", "-v", "-count=1", "./two"], check=True))' \
  'result = f"run finished: {subprocess.run(["go", "test", "-count=1", "./three"], check=True)}"' \
  > "$root/packages/x/scripts/printedrun.py"
printf '%s\n' 'import subprocess' 'result = f"run finished: {subprocess.run(["go", "test", "-count=1", "./three"], check=True)}"' \
  > "$root/packages/x/scripts/formattedrun.py"
printf '%s\n' 'import subprocess' 'subprocess.run(" ".join(["go", "test", "-count=1", "./two"]), shell=True, check=True)' \
  > "$root/packages/x/scripts/joined.py"
unit_job "$root" <<YAML
    defaults:
      run:
        working-directory: packages/x
    steps:
      - name: A single-quoted list pattern with an end anchor
        id: shell-anchored
        run: go test -count=1 -list '^Test.*\$' ./two
      - name: An escaped dollar in a list pattern
        id: shell-escaped
        run: go test -count=1 -list ^Test.*\\\$ ./two
      - name: A Python list pattern with an end anchor
        id: python-anchored
        run: python3 scripts/anchored.py
      - name: A list nested in printed data
        id: nested
        run: python3 scripts/nested.py
      - name: An argv joined into a string the shell runs
        id: joined
        run: python3 scripts/joined.py
      - name: A double-quoted list pattern the shell expands
        id: shell-expanded
        run: go test -count=1 -list "^\$prefix" ./two
      - name: A go test run whose result is printed and formatted
        id: printed-run
        run: python3 scripts/printedrun.py
      - name: A go test run whose result is formatted alone
        id: formatted-run
        run: python3 scripts/formattedrun.py
$upload_step
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "a single-quoted end anchor is literal: the list runs no test" "$(is "$(contains "$out" 'A single-quoted list pattern')" false)"
check "so is an escaped one" "$(is "$(contains "$out" 'An escaped dollar')" false)"
check "and a Python string's" "$(is "$(contains "$out" 'A Python list pattern')" false)"
check "a list nested in printed data is data, a printed join of one too" "$(is "$(contains "$out" 'A list nested in printed data')" false)"
check "an argv joined into a string the shell runs is a run" "$(contains "$out" 'step 5 of job unit (An argv joined into a string the shell runs) runs packages/x/scripts/joined.py')"
check "a double-quoted \$ the shell expands may be empty: that list runs tests" \
  "$(contains "$out" 'step 6 of job unit (A double-quoted list pattern the shell expands) runs `go test -count=1 -list "^$prefix" ./two` other than through')"
check "a run whose result is printed still runs: the argv inside the call is a run" \
  "$(contains "$out" "step 7 of job unit (A go test run whose result is printed and formatted) runs packages/x/scripts/printedrun.py, which runs \`\\['go', 'test', '-v', '-count=1', './two'\\] (a Python argv)\`")"
check "and so does one whose result an f-string formats" \
  "$(contains "$out" "step 8 of job unit (A go test run whose result is formatted alone) runs packages/x/scripts/formattedrun.py, which runs \`\\['go', 'test', '-count=1', './three'\\] (a Python argv)\`")"
check "refuses those four and nothing else" "$(contains "$out" '^4 problem(s) across the 2 workflow file(s) under .github/workflows$')"

echo "case: one fault hides no other: a bare go test step with no id: in a job with no upload is refused for all three in one run"
root=$(fixture events-every-fault)
unit_job "$root" <<'YAML'
    steps:
      - name: Unit tests
        run: go test -count=1 ./...
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the go test the step runs other than through the writer" \
  "$(contains "$out" "^::error file=.github/workflows/ci-job-unit.yaml,line=7::.github/workflows/ci-job-unit.yaml${colon}7: step 1 of job unit (Unit tests) runs \`go test -count=1 ./...\` other than through .github/scripts/go_test_events.py")"
check "and its missing id:, in the same run" "$(contains "$out" 'step 1 of job unit (Unit tests) runs go test and has no id: of its own')"
check "and its job's missing upload, by the action's own uses line" \
  "$(contains "$out" 'job unit runs go test (its last such step is step 1, Unit tests) and no step after it uploads the events: add a step `uses: ./.github/actions/go-test-events-upload`')"
check "refuses those three and nothing else" "$(contains "$out" '^3 problem(s) across the 2 workflow file(s) under .github/workflows$')"

# wrapped_job <root> <writer> <line>...: a ci-job-unit.yaml whose job runs each line as a step with an
# id, then the upload; a <writer> that is not empty stands before the line's first `go test`.
wrapped_job() {
  local root=$1 writer=$2 steps="" n=0 line
  shift 2
  for line in "$@"; do
    n=$((n + 1))
    [ -z "$writer" ] || line=${line/go test/$writer go test}
    steps+="      - name: Step $n
        id: step-$n
        run: $line
"
  done
  unit_job "$root" <<YAML
    steps:
$steps$upload_step
YAML
}

# wrapper_case <name> <what> <line>...: each line, a go test behind <what> in one of the spellings its
# options take, is refused by its step's number; the control, the same lines with the events writer
# before each go test, passes and is counted. <name> names the two fixtures.
wrapper_case() {
  local name=$1 what=$2 root n
  shift 2
  echo "case: a go test behind $what, read past its options, is refused bare and passes behind the writer"
  root=$(fixture "events-behind-$name")
  wrapped_job "$root" "" "$@"
  run_check "$root"
  check "exits 1" "$(is "$status" 1)"
  for ((n = 1; n <= $#; n++)); do
    check "refuses \`${!n}\`" "$(contains "$out" "step $n of job unit (Step $n) runs \`go test [^\`]*\` other than through")"
  done
  check "refuses those $# and nothing else" "$(contains "$out" "^$# problem(s) across the 2 workflow file(s) under .github/workflows$")"
  root=$(fixture "events-behind-$name-kept")
  wrapped_job "$root" ".github/scripts/go_test_events.py" "$@"
  run_check "$root"
  check "control: the same steps behind the writer pass" "$(is "$status" 0)"
  check "and count each command" "$(contains "$out" "keep the events of $# go test command(s) in $# step(s) of 1 job(s) of ci.yaml.s run")"
}

wrapper_case timeout timeout \
  'timeout 600 go test -count=1 ./a' \
  'timeout -s KILL -k 10s 600 go test -count=1 ./a' \
  'timeout --signal=TERM --kill-after 30s --preserve-status 10m go test -count=1 ./a' \
  'timeout -sKILL -v -- 5m go test -count=1 ./a'
wrapper_case nice nice \
  'nice go test -count=1 ./b' \
  'nice -n 5 go test -count=1 ./b' \
  'nice -n5 go test -count=1 ./b' \
  'nice --adjustment=5 go test -count=1 ./b' \
  'nice -10 go test -count=1 ./b'
wrapper_case sudo sudo \
  'sudo go test -count=1 ./c' \
  'sudo -E -u runner go test -count=1 ./c' \
  'sudo --preserve-env=PATH,GOFLAGS --user=runner -H go test -count=1 ./c' \
  'sudo -n -g docker GOFLAGS=-mod=mod go test -count=1 ./c' \
  'sudo --chdir /tmp -- go test -count=1 ./c'
wrapper_case xargs xargs \
  'go list ./... | xargs -n1 go test -count=1' \
  'go list ./... | xargs -n 1 -P 4 go test -count=1' \
  'go list ./... | xargs -0 -r -I {} go test -count=1 {}' \
  'go list ./... | xargs --max-args=1 --max-procs 4 -t go test -count=1' \
  'go list ./... | xargs -i go test -count=1 {}'
wrapper_case kept-places "env, find's -exec, a stack of wrappers, and nohup and setsid" \
  'env -u GOFLAGS GOWORK=off go test -count=1 ./d' \
  'find . -maxdepth 1 -exec go test -count=1 ./e \;' \
  'timeout 600 nice -n 5 sudo -E go test -count=1 ./f' \
  'nohup setsid -w go test -count=1 ./g'
wrapper_case env-split "env -S, whose string and the words after it are the command" \
  'env -S "go test -count=1 ./h"' \
  "env -i -S'go test -count=1' ./h" \
  'env --split-string="go test -v" -count=1 ./h' \
  'env -S go test -count=1 ./h'

echo "case: a go test behind setsid -f with no -w is refused as detached, at the step, through a script, env -S and bash -c; setsid -fw is no fault"
root=$(fixture events-setsid-fork)
mkdir -p "$root/scripts" && printf '%s\n' '#!/usr/bin/env bash' '.github/scripts/go_test_events.py go test -count=1 ./j' > "$root/scripts/unit.sh" && printf '%s\n' '#!/usr/bin/env bash' 'go test -count=1 ./k' > "$root/scripts/bare.sh"
wrapped_job "$root" ".github/scripts/go_test_events.py" 'setsid -f go test -count=1 ./i' 'setsid --fork go test -count=1 ./i' \
  'setsid -fw go test -count=1 ./i' 'setsid -f -w go test -count=1 ./i' 'setsid -f scripts/unit.sh' 'setsid -f env -S scripts/unit.sh' \
  'setsid -fw env -S scripts/unit.sh' "setsid -f bash -c 'cd . && scripts/unit.sh'" 'setsid -f bash -c "$(scripts/unit.sh >&2)"' 'setsid -f bash -c "`scripts/bare.sh >&2`"'
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "refuses the go test behind setsid -f" "$(contains "$out" "step 1 of job unit (Step 1) runs \`.github/scripts/go_test_events.py go test -count=1 ./i\` detached: behind \`setsid -f\` with no \`-w\`")"
check "and the one behind setsid --fork" "$(contains "$out" "step 2 of job unit (Step 2) runs \`[^\`]*\` detached: behind \`setsid -f\`")"
check "and the script setsid -f runs, naming its go test" "$(contains "$out" "step 5 of job unit (Step 5) runs scripts/unit.sh, which runs \`[^\`]* ./j\` (scripts/unit.sh:2) detached")"
check "and the script setsid -f env -S runs" "$(contains "$out" "step 6 of job unit (Step 6) runs scripts/unit.sh, which runs \`[^\`]* ./j\` (scripts/unit.sh:2) detached")"
check "and the script that is the second command of a bash -c setsid -f runs" "$(contains "$out" "step 8 of job unit (Step 8) runs scripts/unit.sh, which runs \`[^\`]* ./j\` (scripts/unit.sh:2) detached")"
check "control: -fw and -f -w wait, and the caller runs a double-quoted \$(...), so steps 3, 4, 7 and 9 are no fault" "$(is "$(contains "$out" 'step [3479] of job unit')" false)"
check "and reads a double-quoted backquote's script, which the caller runs, as foreground: bare, it is refused" "$(contains "$out" "step 10 of job unit (Step 10) runs scripts/bare.sh, which runs \`go test -count=1 ./k\` (scripts/bare.sh:2) other than through")"
check "refuses those six and nothing else" "$(contains "$out" '^6 problem(s) across the 2 workflow file(s) under .github/workflows$')"

echo "case: control: a wrapper's name as another command's argument, a wrapper's --help or --version, and sudo -l run nothing"
root=$(fixture events-wrapper-words)
wrapped_job "$root" "" 'echo timeout 600 go test -count=1 ./x' 'echo sudo nice go test -count=1 ./x' 'nice --help go test -count=1 ./k' \
  'xargs --version go test -count=1 ./k' 'sudo -l go test -count=1 ./k' 'timeout --help 600 go test -count=1 ./k' 'env --version go test -count=1 ./k'
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "and counts no go test" "$(contains "$out" 'keep the events of 0 go test command(s) in 0 step(s) of 0 job(s) of ci.yaml.s run')"

# filters_file <root> <pattern>...: a computed filters file, as ci.yaml's changes job writes one,
# whose ci_filters filter holds the patterns; its path, in the fixture's root.
filters_file() {
  local root=$1
  shift
  {
    echo "lint:"
    echo "  - '.github/workflows/**'"
    echo "ci_filters:"
    echo "  - '.github/workflows/**'"
    for pattern in "$@"; do echo "  - '$pattern'"; done
  } > "$root/ci-filters.computed.yaml"
  echo "$root/ci-filters.computed.yaml"
}

echo "case: control: a script rule 8 reads that the ci_filters filter does not select is refused, naming the step that reaches it"
root=$(fixture events-filter-misses)
events_scripts "$root"
filters=$(filters_file "$root" 'packages/envoy/**')
unit_job "$root" <<YAML
    steps:
      - name: A script two levels down, under the writer as a whole
        id: outer
        run: ../../.github/scripts/go_test_events.py scripts/kept-outer.sh
        working-directory: packages/x
      - name: The skip counter
        id: skips
        run: .github/scripts/go_test_events.py packages/envoy/scripts/database-skips.sh --timeout 20m
$upload_step
YAML
run_check "$root" "$filters"
check "exits 1" "$(is "$status" 1)"
check "names the script the step runs" \
  "$(contains "$out" '^::error file=.github/workflows/ci-job-unit.yaml,line=7::.github/workflows/ci-job-unit.yaml:7: step 1 of job unit (A script two levels down, under the writer as a whole) runs packages/x/scripts/kept-outer.sh, itself or through a script it runs, and rule 8 reads it, but the ci_filters filter ci.yaml.s changes job computed does not select it')"
check "and the script that script runs, by the same step" \
  "$(contains "$out" 'step 1 of job unit (A script two levels down, under the writer as a whole) runs packages/x/scripts/kept.sh, itself or through a script it runs')"
check "says what that lets merge" "$(contains "$out" 'a go test added to it merges unread and fails the next pull request that starts the job')"
check "passes the script the filter selects" "$(is "$(contains "$out" 'database-skips.sh, itself')" false)"
check "refuses those two and nothing else" "$(contains "$out" '^2 problem(s) across the 2 workflow file(s) under .github/workflows$')"
filters=$(filters_file "$root" 'packages/envoy/**' 'packages/*/scripts/**')
run_check "$root" "$filters"
check "with a pattern that selects them, exits 0" "$(is "$status" 0)"
check "and says each script it read is one the filter selects" "$(contains "$out" 'run, and each of the 3 script(s) it read is one the ci_filters filter selects$')"
printf '# FAIL-OPEN: the filters could not be computed\nci_filters:\n  - "**"\n' > "$root/fail-open.yaml"
run_check "$root" "$root/fail-open.yaml"
check "a filters file written because the filters could not be computed holds nothing, and says so" \
  "$(contains "$out" "UNKNOWN: $root/fail-open.yaml was written because the filters could not be computed$")"
run_check "$root" "$root/no-such-file.yaml"
check "and so does a CI_FILTERS_FILE that is not there" "$(contains "$out" "UNKNOWN: cannot read CI_FILTERS_FILE's $root/no-such-file.yaml: FileNotFoundError$")"

echo "case: with no ci.yaml, rule 8 says it read nothing rather than that nothing failed it, a bare go test elsewhere among them"
root=$(fixture events-no-router)
rm "$root/.github/workflows/ci.yaml"
printf 'name: Build\non: push\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - run: go test ./...\n' \
  > "$root/.github/workflows/build.yaml"
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "says the count is UNKNOWN and why" "$(contains "$out" 'keep go test events UNKNOWN: there is no .github/workflows/ci.yaml, so rule 8 read no step$')"

echo "case: the writer's first form takes the word go alone: a go test behind it under another go word is refused, and go is kept"
root=$(fixture events-writer-go-word) w=.github/scripts/go_test_events.py
wrapped_job "$root" "" "$w \"\$GO\" test ./a" "$w \${GO} test ./a" "$w /usr/local/go/bin/go test ./a" "$w \"\$GOROOT/bin/go\" test ./a" "$w \"\$(go env GOROOT)/bin/go\" test ./a" "$w go test ./a"
run_check "$root"
for n in 1 2 3 4 5; do check "refuses step $n, saying to write go" "$(contains "$out" "step $n of job unit (Step $n) runs \`[^\`]*\` through $w with a go word other than \`go\`.* Write \`go\` right after the writer")"; done
check "and those five alone: the literal go is kept" "$(contains "$out" '^5 problem(s) across the 2 workflow file(s) under .github/workflows$')"

echo "case: a Python script that does not parse is refused once, as such a script, and is no go test step"
root=$(fixture events-unparsable)
mkdir -p "$root/scripts" && printf 'def broken(:\n' > "$root/scripts/lint.py"
printf '    steps:\n      - name: Lint\n        run: python3 scripts/lint.py\n' | unit_job "$root"
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "names the script and the line its parser names" \
  "$(contains "$out" 'step 1 of job unit (Lint) runs scripts/lint.py, a Python script this check cannot parse (scripts/lint.py:1: ')"
check "refuses that one and nothing else: no id or upload is asked of it" "$(contains "$out" '^1 problem(s) across the 2 workflow file(s) under .github/workflows$')"

# The generated forms (go_test_step_forms.py): every context, go spelling and indirection, run under
# bash with a stand-in go and read by this tree's rule 8. Each control is a copy of the rule with one
# fix taken out, over the forms of that fix alone: the generator must fail, naming the form.
echo "case: rule 8 reads every form bash runs a go test in, but those .github/go-test-step-misses.tsv lists"
set +e; forms=$(python3 "$script_dir/go_test_step_forms.py" 2>&1); status=$?; set -e
check "exits 0" "$(is "$status" 0)"
check "with no unlisted miss and no false refusal" "$(contains "$forms" ' 0 unlisted, refuses [0-9]* negative form(s) the list accepts, and refuses 0 form variant(s) falsely$')"
# control <name> <file> <text> <replacement> <forms>: the generator over <forms>, read by a copy of
# the rule whose <file> holds <text> once, replaced; its output in $out, its status in $status.
control() {
  local copy="$work/control-$1"
  mkdir -p "$copy" && cp "$script_dir/go_test_steps.py" "$script_dir/go_test_shell.py" "$script_dir/go_test_events.py" \
    "$script_dir/workflow_paths.py" "$copy/"
  python3 -c 'import sys, pathlib; p = pathlib.Path(sys.argv[1]); s = p.read_text(); assert s.count(sys.argv[2]) == 1, sys.argv[2]; p.write_text(s.replace(sys.argv[2], sys.argv[3]))' "$copy/$2" "$3" "$4"
  set +e; out=$(python3 "$script_dir/go_test_step_forms.py" --module "$copy" --only "$5" 2>&1); status=$?; set -e
}
control negation go_test_shell.py 'RESERVED = r"(?:!|' 'RESERVED = r"(?:' '*/negation/go'
check "control: with no ! among the reserved words, it fails" "$(is "$status" 1)"
check "naming the form" "$(contains "$out" '^UNLISTED run/negation/go (bare): ')"
control cycle go_test_steps.py '    if not read.cut:' '    if True:' 'cycle-a-first/plain/go'
check "control: keeping a read the cycle guard cut short, it fails" "$(is "$status" 1)"
check "naming the step behind the cycle" "$(contains "$out" '^UNLISTED cycle-a-first/plain/go (bare): bare, step(s) 2 not refused')"

summary "go_test_steps.py"
