#!/usr/bin/env bash
# Tests for rule 8 of check-workflow-files.sh (bun_test_steps.py): every bun test of ci.yaml's run
# runs under bun_test_run.py, and every job that runs one keeps its reports with the upload step.
#
# Each case builds a miniature repository under a temporary directory and symlinks the real
# check-workflow-files.sh into it; the script imports the rule from beside its own file, so the
# rule under test is the module CI runs.
#
# Run from anywhere: .github/scripts/bun_test_steps.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
check_script="$script_dir/check-workflow-files.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# colon is the colon of a path:line the check prints, built at run time so that this file's text
# holds no path followed by a colon and a line number, which ci_citations.py refuses under .github.
colon=':'

# fixture <name>: a tree with the script linked in, a copy of the repository's upload action, and a
# ci.yaml that calls ci-job-a.yaml, the one workflow of ci.yaml's run each case writes. Echoes its
# root.
action="$script_dir/../actions/bun-test-reports-upload/action.yml"
fixture() {
  local root="$work/$1"
  mkdir -p "$root/.github/scripts" "$root/.github/workflows" "$root/.github/actions/bun-test-reports-upload"
  ln -s "$check_script" "$root/.github/scripts/check-workflow-files.sh"
  cp "$action" "$root/.github/actions/bun-test-reports-upload/action.yml"
  cat > "$root/.github/workflows/ci.yaml" <<'YAML'
name: CI
on: pull_request
jobs:
  a:
    uses: ./.github/workflows/ci-job-a.yaml
YAML
  echo "$root"
}

run_check() {
  local root=$1
  set +e
  out=$(cd "$root" && "./.github/scripts/check-workflow-files.sh" 2>&1)
  status=$?
  set -e
}

errors() { grep -c '^::error file=' <<<"$1" || true; }
# line_of <file> <text>: the first line of the file that holds the text.
line_of() { grep -n -F -- "$2" "$1" | head -n 1 | cut -d: -f1; }

echo "case: every bun test form the repository's steps use, under the wrapper, with each job's upload, passes"
root=$(fixture clean)
cat > "$root/.github/workflows/ci-job-a.yaml" <<'YAML'
name: A
on: workflow_call
jobs:
  package:
    runs-on: ubuntu-latest
    steps:
      - name: Test the package
        run: ../../.github/scripts/bun_test_run.py test bun run test
        working-directory: packages/p
      - name: Test a file with -t, its temp directory set
        run: |
          # bun test runs here under the wrapper, which this comment names: bun test
          mkdir -p "$RUNNER_TEMP/tmp"
          TMPDIR="$RUNNER_TEMP/tmp" ../../.github/scripts/bun_test_run.py stall bun test src/stall.test.ts -t "NATS path"
        working-directory: packages/p
      - name: Test at the root, its command continued
        run: |
          .github/scripts/bun_test_run.py scripts-e2e-lib \
            bun run test:e2e-lib --path-ignore-patterns=x
      - name: Words that run no bun test the rule reads
        run: |
          echo my-bun test
          "$bun" test
          bun testing
          bun test:web
      - name: Test two lanes from arrays
        run: |
          web=(test:web web/src)
          ../../.github/scripts/bun_test_run.py web bun run "${web[@]}"
        working-directory: packages/d
      - name: Keep the bun test reports
        if: always()
        uses: ./.github/actions/bun-test-reports-upload
        with:
          check-run-id: ${{ job.check_run_id }}
  pooled:
    runs-on: ubuntu-latest
    steps:
      - name: Scan in the pool
        run: ../../.github/scripts/harness-pool.sh start scan ../../.github/scripts/bun_test_run.py approve bun test scripts/approve/
        working-directory: packages/e
      - id: harness-wait
        run: exec .github/scripts/harness-pool.sh wait
      - name: Keep the bun test reports
        if: always()
        uses: ./.github/actions/bun-test-reports-upload
        with:
          check-run-id: ${{ job.check_run_id }}
  none:
    runs-on: ubuntu-latest
    steps:
      - run: bunx tsc --noEmit
      - run: bun run typecheck
YAML
# A workflow ci.yaml does not call: its bare bun test is outside the rule.
cat > "$root/.github/workflows/repeat.yaml" <<'YAML'
name: Repeat
on: workflow_dispatch
jobs:
  leg:
    runs-on: ubuntu-latest
    steps:
      - run: bun run test:repeat
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "counts the runs, the steps and the jobs it held" \
  "$(contains "$out" "and run each of 5 bun test command(s) in 5 step(s) of 2 job(s) of ci.yaml.s run under bun_test_run.py, each job keeping its reports$")"

echo "case: a bun test run other than under the wrapper is refused, naming file, job, step and line"
root=$(fixture bare)
file="$root/.github/workflows/ci-job-a.yaml"
cat > "$file" <<'YAML'
name: A
on: workflow_call
jobs:
  package:
    runs-on: ubuntu-latest
    steps:
      - name: Test the package
        run: bun run test
        working-directory: packages/p
      - name: Test two lanes
        run: |
          bun run build:contracts
          bun run test:web web/src
      - name: Test with a temp directory
        run: TMPDIR=/tmp bun test src
      - name: Scan in the pool
        run: ../../.github/scripts/harness-pool.sh start scan bun test scripts/approve/
      - name: Test under a quoted wrapper
        run: '"$GITHUB_WORKSPACE/.github/scripts/bun_test_run.py" q bun test'
      - name: Test under a name the wrapper refuses
        run: .github/scripts/bun_test_run.py aé bun test
      - name: A bare one chained after a wrapped one
        run: .github/scripts/bun_test_run.py one bun test a && bun test b
      - id: harness-wait
        run: exec .github/scripts/harness-pool.sh wait
      - name: Keep the bun test reports
        if: always()
        uses: ./.github/actions/bun-test-reports-upload
        with:
          check-run-id: ${{ job.check_run_id }}
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "refuses a bare bun run test" \
  "$(contains "$out" "^::error file=.github/workflows/ci-job-a.yaml,line=8::.github/workflows/ci-job-a.yaml${colon}8: step 1 ('Test the package') of job package runs \`bun run test\` other than under .github/scripts/bun_test_run.py")"
check "refuses bun run test:web on its line of a block, and not the build beside it" \
  "$(contains "$out" "line=$(line_of "$file" 'bun run test:web')::.github/workflows/ci-job-a.yaml${colon}$(line_of "$file" 'bun run test:web'): step 2 ('Test two lanes') of job package runs \`bun run test:web\`")"
check "refuses one behind an assignment" \
  "$(contains "$out" "line=$(line_of "$file" 'TMPDIR=/tmp')::.*step 3 ('Test with a temp directory') of job package runs \`bun test\`")"
check "refuses one the pool starts bare" \
  "$(contains "$out" "line=$(line_of "$file" 'start scan bun test')::.*step 4 ('Scan in the pool') of job package runs \`bun test\`")"
check "refuses one under a quoted wrapper path, which it does not read as the wrapper" \
  "$(contains "$out" "step 5 ('Test under a quoted wrapper') of job package runs \`bun test\`")"
check "says how to write it" "$(contains "$out" 'Run it as `.github/scripts/bun_test_run.py <report name> bun test …`')"
check "refuses one under a report name the wrapper refuses, as the wrapper's NAME reads it" \
  "$(contains "$out" "step 6 ('Test under a name the wrapper refuses') of job package runs \`bun test\`")"
check "refuses a bare one chained on the line after a wrapped one, and not the wrapped one" \
  "$(is "$(grep -c "step 7 ('A bare one chained after a wrapped one') of job package runs" <<<"$out")" 1)"
check "refuses those seven and nothing else" "$(is "$(errors "$out")" 7)"

echo "case: a job that runs bun test and keeps no reports, or keeps them wrongly, is refused"
root=$(fixture upload)
file="$root/.github/workflows/ci-job-a.yaml"
cat > "$file" <<'YAML'
name: A
on: workflow_call
jobs:
  no-upload:
    runs-on: ubuntu-latest
    steps:
      - name: Test
        run: .github/scripts/bun_test_run.py t bun test
  upload-before:
    runs-on: ubuntu-latest
    steps:
      - name: Test one
        run: .github/scripts/bun_test_run.py one bun test a
      - name: Keep the bun test reports
        if: always()
        uses: ./.github/actions/bun-test-reports-upload
        with:
          check-run-id: ${{ job.check_run_id }}
      - name: Test two
        run: .github/scripts/bun_test_run.py two bun test b
  renamed:
    runs-on: ubuntu-latest
    steps:
      - name: Test
        run: .github/scripts/bun_test_run.py t bun test
      - name: Upload the bun test reports
        if: always()
        uses: ./.github/actions/bun-test-reports-upload
        with:
          check-run-id: ${{ job.check_run_id }}
  not-always:
    runs-on: ubuntu-latest
    steps:
      - name: Test
        run: .github/scripts/bun_test_run.py t bun test
      - name: Keep the bun test reports
        uses: ./.github/actions/bun-test-reports-upload
        with:
          check-run-id: ${{ job.check_run_id }}
  other-id:
    runs-on: ubuntu-latest
    steps:
      - name: Test
        run: .github/scripts/bun_test_run.py t bun test
      - name: Keep the bun test reports
        if: always()
        uses: ./.github/actions/bun-test-reports-upload
        with:
          check-run-id: ${{ github.run_id }}
  pool-before-wait:
    runs-on: ubuntu-latest
    steps:
      - name: Scan in the pool
        run: .github/scripts/harness-pool.sh start scan .github/scripts/bun_test_run.py approve bun test x
      - name: Keep the bun test reports
        if: always()
        uses: ./.github/actions/bun-test-reports-upload
        with:
          check-run-id: ${{ job.check_run_id }}
      - id: harness-wait
        run: exec .github/scripts/harness-pool.sh wait
  pool-no-wait:
    runs-on: ubuntu-latest
    steps:
      - name: Scan in the pool
        run: .github/scripts/harness-pool.sh start scan .github/scripts/bun_test_run.py approve bun test x
      - name: Keep the bun test reports
        if: always()
        uses: ./.github/actions/bun-test-reports-upload
        with:
          check-run-id: ${{ job.check_run_id }}
YAML
run_check "$root"
check "exits 1" "$(is "$status" 1)"
check "refuses a job with no upload step, naming its last bun test run" \
  "$(contains "$out" "line=4::.github/workflows/ci-job-a.yaml${colon}4: job no-upload runs bun test (its last such run is step 1 ('Test'), line 8) and has no step after it that uses ./.github/actions/bun-test-reports-upload, so the reports its runs write are not kept")"
check "refuses a job whose upload comes before its last bun test run" \
  "$(contains "$out" "line=9::.*job upload-before runs bun test (its last such run is step 3 ('Test two'), line $(line_of "$file" 'bun_test_run.py two')) and has no step after it that uses ./.github/actions/bun-test-reports-upload (its step 2 does, before that run ends)")"
check "refuses an upload step of another name, the name the reader finds a job by" \
  "$(contains "$out" "line=$(line_of "$file" 'name: Upload the bun test reports')::.*step 2 of job renamed, its upload of the bun test reports, is named 'Upload the bun test reports', not 'Keep the bun test reports'")"
check "refuses an upload step that does not run always" \
  "$(contains "$out" "step 2 of job not-always, its upload of the bun test reports, has no \`if:\`, not \`if: always()\`")"
check "refuses an upload step named for another id" \
  "$(contains "$out" "step 2 of job other-id, its upload of the bun test reports, gives check-run-id '\${{ github.run_id }}', not \`\${{ job.check_run_id }}\`")"
check "refuses an upload before the wait of a run the pool starts" \
  "$(contains "$out" "job pool-before-wait runs bun test (its last such run is step 1 ('Scan in the pool'), line $(line_of "$file" 'start scan .github')) and has no step after it")"
check "refuses a run the pool starts with no wait after it" \
  "$(contains "$out" "step 1 ('Scan in the pool') of job pool-no-wait starts its bun test in harness-pool.sh, and no later step of the job runs \`harness-pool.sh wait\`")"
check "refuses those seven and nothing else" "$(is "$(errors "$out")" 7)"

echo "case: the upload action's own artifact name, path and hidden files are the record's"
# upload_action <case> <python>: a fixture whose one job is clean, its action.yml edited by the
# python, which reads and writes `text`.
upload_action() {
  root=$(fixture "action-$1")
  cat > "$root/.github/workflows/ci-job-a.yaml" <<'YAML'
name: A
on: workflow_call
jobs:
  package:
    runs-on: ubuntu-latest
    steps:
      - name: Test
        run: .github/scripts/bun_test_run.py t bun test
      - name: Keep the bun test reports
        if: always()
        uses: ./.github/actions/bun-test-reports-upload
        with:
          check-run-id: ${{ job.check_run_id }}
YAML
  file="$root/.github/actions/bun-test-reports-upload/action.yml"
  python3 -c '
import sys
text = open(sys.argv[1]).read()
exec(sys.argv[2])
open(sys.argv[1], "w").write(text)' "$file" "$2"
  run_check "$root"
}
upload_action same 'pass'
check "the repository's own action passes" "$(is "$status" 0)"
upload_action name 'text = text.replace("name: bun-test-reports-$", "name: bun-reports-$")'
check "an artifact name of another prefix is refused, naming the file and line" \
  "$(is "$status:$(contains "$out" "^::error file=.github/actions/bun-test-reports-upload/action.yml,line=$(line_of "$file" 'name: bun-reports-')::.*its upload-artifact step gives name 'bun-reports-[$]{{ inputs.check-run-id }}', not 'bun-test-reports-[$]{{ inputs.check-run-id }}'")" 1:true)"
upload_action path 'text = text.replace("/bun-test-reports/", "/bun-reports/")'
check "a path of another directory is refused" \
  "$(is "$status:$(contains "$out" "its upload-artifact step gives path '[$]{{ runner.temp }}/bun-reports/', not '[$]{{ runner.temp }}/bun-test-reports/'")" 1:true)"
upload_action hidden 'text = text.replace("        include-hidden-files: true\n", "")'
check "an upload without its hidden files is refused" \
  "$(is "$status:$(contains "$out" "its upload-artifact step gives include-hidden-files None, not 'true'")" 1:true)"
upload_action none 'text = text.replace("actions/upload-artifact@", "actions/cache@")'
check "an action with no upload-artifact step is refused" \
  "$(is "$status:$(contains "$out" 'action.yml has no step that uses actions/upload-artifact')" 1:true)"

echo "case: a tree with no ci.yaml says the rule read nothing"
root=$(fixture no-router)
rm "$root/.github/workflows/ci.yaml"
cat > "$root/.github/workflows/build.yaml" <<'YAML'
name: Build
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: bun test
YAML
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "says the record is UNKNOWN, not kept" \
  "$(contains "$out" "and keep bun test reports UNKNOWN: there is no .github/workflows/ci.yaml, so rule 8 read no step$")"

summary "bun_test_steps.py (check-workflow-files.sh rule 8)"
