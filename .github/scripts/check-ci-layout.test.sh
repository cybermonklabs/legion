#!/usr/bin/env bash
# Tests for `check-ci-layout.sh`: a copy of this repository's own layout passes, and each copy that
# breaks one rule the way the Verification document's c-ci-shape check names is refused, naming
# the rule's subject: a step that tests an output of changes (main's step gate on the two-build
# Go step), a -skip list that loses one test a -run list of another job names, kind kept to pull
# requests in CI_OK_PULL_REQUEST_ONLY or by its own condition or with merge_group left out of it,
# a go test outside go-test.sh, a go-test.sh call with no --jsonfile, a retry turned off, the
# mutants script taking the retry, the search job tested on an output, a step inside an action
# gated on merge_group, a job without the run_attempt term, a liveGroups entry no kind leg runs,
# a job gated on an output changes does not declare, a carrier with a fourth step, a carrier for a
# job that is not split, a carrier with no report step, a split job's legs that leave an index out,
# that name one twice, that name two counts or an index outside them, a step that hands a leg
# another count than its name's as its `with: legs` (written out or read off the matrix), ci-ok's
# reconcile step renamed and its hold step's if: drifted from the term the reconcile negates,
# an AUDITED_SCRIPTS pin a script's text has moved past, pr-title needing changes, a literal name
# on a job a run can skip (worker-image, an agent-harness part, dispatch-browser.yaml's visual), a
# name of string literals alone on one (kind-image, dispatch-browser.yaml's recordings), a caller
# of a reusable workflow whose evaluated name is a check-run name a check reads (worker-image as
# `worker-image / build`) or the layout writes (docs as `kind / image`),
# a changes output reading another step, the airlock job on x64, the race step dropping a package
# the fill names, a push-only if: in an action only ci.yaml runs, a cache key drifting inside an
# action and a cache path drifting inside one ci-caches.yaml runs too, an install that loses its
# 11-minute bound and two timed installs folded into one, a matrix without fail-fast: false, a
# name of main's ci.yaml the layout no longer carries, a second merge_group workflow, a notes
# section no link names, a link to no section, a `## <name>:` heading naming no output or job, and
# a darwin vet that stops reading go_build_tags.py or whose tag sets miss a file.
#
# Each case copies .github/ of this checkout and links every other top-level entry of it, so the
# scripts under test, the workflows and the actions are this checkout's own and the Go sources the
# step reader reads are its tree; the case edits only files it copied. Rule 20's go list runs over
# a module of the case's own, and every other case pins no darwin vet, so no case lists this
# checkout's modules. It needs python3 with PyYAML, jq and go.
#
# Run from anywhere: .github/scripts/check-ci-layout.test.sh
# CI runs it in ci.yaml's scripts-held job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(cd "$script_dir/../.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

# replace <file> <old> <new>: one literal replacement, failing the case loudly if <old> is absent.
replace() {
  local file=$1 old=$2 new=$3 text
  text=$(<"$file")
  if [[ $text != *"$old"* ]]; then
    echo "  FAIL: $file has no '$old' to replace" >&2
    return 1
  fi
  printf '%s\n' "${text/"$old"/"$new"}" >"$file"
}

base=$work/base
mkdir -p "$base"
cp -a "$repo/.github" "$base/.github"
rm -rf "$base/.github/scripts/__pycache__"
for entry in "$repo"/* "$repo"/.[!.]*; do
  name=$(basename "$entry")
  case $name in .github | .git | .jj) continue ;; esac
  ln -s "$entry" "$base/$name"
done
printf '\n# The cases but rule 20s own vet no module of this checkout.\nDARWIN_VETS = []\n' \
  >>"$base/.github/scripts/ci_pins.py"

# fresh <name>: a copy of the base for one case. Echoes its root.
fresh() {
  cp -a "$base" "$work/$1"
  echo "$work/$1"
}

run_check() {
  set +e
  out=$("$1/.github/scripts/check-ci-layout.sh" 2>&1)
  status=$?
  set -e
}

has() { grep -Fq -- "$2" <<<"$1" && echo true || echo false; }

pins() { printf '%s\n' "$2" >>"$1/.github/scripts/ci_pins.py"; }

echo "case: this checkout's own layout passes"
root=$(fresh head)
run_check "$root"
check "exits 0" "$(is "$status" 0)"
check "says every rule holds" "$(has "$out" 'the CI layout holds every rule of check-ci-layout.sh')"
check "prints no error" "$(is "$(grep -c '^::error' <<<"$out" || true)" 0)"

echo "case: the gates of ci.yaml's jobs and steps"
root=$(fresh gates)
ci=$root/.github/workflows/ci.yaml
# main's step gate on the two-build Go step, here on the two-build leg's step
replace "$ci" "if: matrix.leg == 'dispatch-two-builds'" \
  "if: matrix.leg == 'dispatch-two-builds' && needs.changes.outputs.go-envoy-held == 'true'"
# kind kept to pull requests in ci-ok's list
replace "$ci" "CI_OK_PULL_REQUEST_ONLY: '[\"worker-image\", \"dispatch-browser-pr\"]'" "CI_OK_PULL_REQUEST_ONLY: '[\"worker-image\", \"dispatch-browser-pr\", \"kind\"]'"
# the search job's condition tests an output
replace "$ci" $'github.run_attempt == \'1\') }}\n    name: ${{ matrix.name }}\n    runs-on: ${{ matrix.runner }}' \
  $'github.run_attempt == \'1\') && needs.changes.outputs.scripts == \'true\' }}\n    name: ${{ matrix.name }}\n    runs-on: ${{ matrix.runner }}'
# a job gated on an output changes does not declare
replace "$ci" "needs.changes.outputs.plugin-pack == 'true'" "needs.changes.outputs.plugin-pack-x == 'true'"
# a job without the run_attempt term
replace "$ci" "if: needs.changes.outputs.daemon-live == 'true' && (github.event_name != 'merge_group' || github.run_attempt == '1')" \
  "if: needs.changes.outputs.daemon-live == 'true'"
# pr-title needs changes
replace "$ci" $'  pr-title:\n' $'  pr-title:\n    needs: changes\n'
# a changes output reading a step that is none of the three
replace "$ci" "      unnamed_files: \${{ steps.unnamed.outputs.found }}" \
  $'      unnamed_files: ${{ steps.unnamed.outputs.found }}\n      extra: ${{ steps.ref.outputs.other }}'
# the airlock job on x64
replace "$ci" $'    runs-on: ubuntu-24.04-arm\n    timeout-minutes: 45' $'    runs-on: ubuntu-24.04\n    timeout-minutes: 45'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "refuses a step that tests an output of changes" \
  "$(has "$out" "job go-envoy-held step \"dispatch-two-builds\" tests \`needs.changes.outputs.go-envoy-held\`")"
check "refuses kind in CI_OK_PULL_REQUEST_ONLY (rule 7)" \
  "$(has "$out" "CI_OK_PULL_REQUEST_ONLY lists kind, whose if: has no github.event_name == 'pull_request' term")"
check "refuses the search job gated on an output (rule 7)" \
  "$(has "$out" "CI_OK_ALWAYS_RUN names lint, whose if: tests needs.changes.outputs.scripts")"
check "refuses a gate on an undeclared output (rule 2)" \
  "$(has "$out" "job plugin-pack's if: tests needs.changes.outputs.plugin-pack-x, which the changes job does not declare")"
check "refuses a job without the run_attempt term (rule 12)" \
  "$(has "$out" "job daemon-live's if: has no \`(github.event_name != 'merge_group' || github.run_attempt == '1')\`")"
check "refuses pr-title needing a job" "$(has "$out" "job pr-title needs a job")"
check "refuses an output reading another step (rule 19)" \
  "$(has "$out" "the changes job's output extra reads steps.ref.outputs.other, and step ref is neither")"
check "refuses the airlock job off arm (rule 26)" "$(has "$out" "job airlock-arm runs on \`ubuntu-24.04\`")"

echo "case: kind kept to pull requests by its own condition"
root=$(fresh kind-pr)
replace "$root/.github/workflows/ci.yaml" "if: \${{ !cancelled() && needs.kind-image.result == 'success'" \
  "if: \${{ !cancelled() && github.event_name == 'pull_request' && needs.kind-image.result == 'success'"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names kind's gate on the event (rule 28)" "$(has "$out" "job kind has \`\${{ !cancelled() && github.event_name == 'pull_request'")"
check "names the merge group it skips" "$(has "$out" "which skips it on merge_group for the event or a label")"

echo "case: kind's condition leaves merge_group out"
root=$(fresh kind-no-merge-group)
replace "$root/.github/workflows/ci.yaml" "if: \${{ !cancelled() && needs.kind-image.result == 'success'" \
  "if: \${{ !cancelled() && (github.event_name == 'pull_request' || github.event_name == 'workflow_dispatch') && needs.kind-image.result == 'success'"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the merge group kind's condition skips (rule 28)" \
  "$(has "$out" "github.event_name == 'workflow_dispatch') && needs.kind-image.result == 'success'")"

echo "case: the Go steps' retry, jsonfile and split lists"
root=$(fresh go-steps)
# a -skip list that keeps plugin-pack's tests out of go-daemon loses one of them
replace "$root/.github/actions/go-daemon/action.yml" '|TestTheRealPluginsOnTheRealOhMyPi)' ')'
# a go-test.sh call with no --jsonfile
replace "$root/.github/actions/plugin-pack/action.yml" '--jsonfile plugin-pack-plugin-pack-pack.json ' ''
# a retry turned off
replace "$root/.github/actions/daemon-live/action.yml" 'run: ../../.github/scripts/go-test.sh' \
  'run: GO_TEST_RETRY=off ../../.github/scripts/go-test.sh'
# a step inside broker-multiprocess gated on merge_group
replace "$root/.github/actions/go-envoy/broker-multiprocess/action.yml" \
  $'    - name: Two-process tests of the secrets broker\n' \
  $'    - name: Two-process tests of the secrets broker\n      if: github.event_name == \'merge_group\'\n'
# the race step drops a package the fill still names
replace "$root/.github/actions/go-daemon/action.yml" './internal/intake/ ./internal/shimwire/"' './internal/intake/"'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the test the -skip list lost (rule 22)" \
  "$(has "$out" "runs TestTheRealPluginsOnTheRealOhMyPi of")"
check "names the step with no --jsonfile (rule 24)" "$(has "$out" "plugin-pack runs go-test.sh with no --jsonfile")"
check "names the retry turned off (rule 24)" "$(has "$out" "daemon-live sets GO_TEST_RETRY=off")"
check "names the step gated on merge_group (rule 28)" \
  "$(has "$out" "step \"Two-process tests of the secrets broker\" has \`github.event_name == 'merge_group'\`, which skips it on pull_request")"
check "names the package the fill compiles and the race step drops (rule 16)" \
  "$(has "$out" "job daemon's race build names ./internal/shimwire/, which the race step")"

echo "case: a go test outside go-test.sh, and the mutants script taking the retry"
root=$(fresh no-retry)
replace "$root/.github/actions/plugin-pack/action.yml" \
  '../../.github/scripts/go-test.sh --jsonfile plugin-pack-plugin-pack-pack.json --packages ./internal/daemon/ --' \
  'go test ./internal/daemon/'
grep -v '^export GO_TEST_RETRY=off' "$repo/packages/envoy/scripts/airlock-mutants.sh" >"$root/mutants.sh"
pins "$root" 'NO_RETRY_SCRIPT = "mutants.sh"'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the go test outside go-test.sh (rule 24)" "$(has "$out" "plugin-pack runs \`go test")"
check "names the mutants script taking the retry (rule 24)" \
  "$(has "$out" "mutants.sh does not \`export GO_TEST_RETRY=off\`")"

echo "case: a liveGroups entry no kind leg runs"
root=$(fresh live-groups)
cp "$repo/packages/daemon/internal/runtime/sandbox/live_test.go" "$root/live_test.go"
replace "$root/live_test.go" $'var liveGroups = []liveGroup{\n' $'var liveGroups = []liveGroup{\n\t{name: "unrun", checks: nil},\n'
pins "$root" 'LIVE_GROUPS = {**LIVE_GROUPS, "file": "live_test.go"}'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the group in no leg (rule 27)" \
  "$(has "$out" "liveGroups entry unrun of live_test.go is in 0 kind legs' -run (none), and must be in exactly one")"

echo "case: the carriers, the names and the matrices"
root=$(fresh carriers)
ci=$root/.github/workflows/ci.yaml
replace "$ci" $'leg-verdict.sh \'envoy-go (1/3)\' \'envoy-go (2/3)\' \'envoy-go (3/3)\'\n' \
  $'leg-verdict.sh \'envoy-go (1/3)\' \'envoy-go (2/3)\' \'envoy-go (3/3)\'\n      - run: echo a fourth step\n'
replace "$ci" $'leg-verdict.sh \'envoy-go-race-api (1/2)\' \'envoy-go-race-api (2/2)\'\n      - *report\n' \
  $'leg-verdict.sh \'envoy-go-race-api (1/2)\' \'envoy-go-race-api (2/2)\'\n'
replace "$ci" $'  airlock-arm:\n' \
  "  plugin-pack-carrier:
    name: \${{ github.event_name && 'plugin-pack / plugin-pack' }}
    needs: [changes, plugin-pack]
    if: \${{ !cancelled() && needs.changes.outputs.plugin-pack == 'true' && (github.event_name != 'merge_group' || github.run_attempt == '1') }}
    runs-on: ubuntu-24.04-arm
    timeout-minutes: 10
    steps:
      - *checkout-github
      - env: {GH_TOKEN: '\${{ github.token }}'}
        run: .github/scripts/leg-verdict.sh 'plugin-pack / plugin-pack'
      - *report

  airlock-arm:
"
replace "$ci" "name: \${{ github.event_name && 'worker-image' }}" 'name: worker-image'
replace "$ci" "name: \${{ format('agent-harness / {0}', matrix.leg) }}" 'name: agent-harness'
replace "$root/.github/workflows/dispatch-browser.yaml" "    name: \${{ github.event_name && 'visual' }}"$'\n' ''
# a name GitHub evaluates even for a skipped job: string literals alone, in ci.yaml and in a
# workflow it calls
replace "$ci" "name: \${{ github.event_name && 'kind / image' }}" "name: \${{ 'kind / image' }}"
replace "$root/.github/workflows/dispatch-browser.yaml" "name: \${{ github.event_name && 'recordings' }}" \
  "name: \${{ 'recordings' || 'recordings (full)' }}"
replace "$ci" $'    strategy:\n      fail-fast: false\n      matrix:\n        include:\n          - {leg: lint,' \
  $'    strategy:\n      matrix:\n        include:\n          - {leg: lint,'
replace "$ci" "name: \${{ github.event_name && 'text-checks / text-checks' }}" \
  "name: \${{ github.event_name && 'text-check / text-check' }}"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the carrier with a fourth step (rule 32)" "$(has "$out" "carrier envoy-go holds 4 step(s)")"
check "names the carrier with no report step (rule 7)" "$(has "$out" "job envoy-go-race-api's last step is not")"
check "names the carrier of a job that is not split (rule 32)" \
  "$(has "$out" "job plugin-pack-carrier runs .github/scripts/leg-verdict.sh, a carrier, and carries \`plugin-pack / plugin-pack\`")"
check "names worker-image's literal name (rule 25)" "$(has "$out" "job worker-image of .github/workflows/ci.yaml has an if: that can skip it and the literal name")"
check "names agent-harness's literal name (rule 25)" "$(has "$out" "job agent-harness of .github/workflows/ci.yaml has an if: that can skip it and the literal name")"
check "names the visual job with no name (rule 25)" "$(has "$out" "job visual of .github/workflows/dispatch-browser.yaml has an if: that can skip it and no name:")"
check "names kind-image's name of string literals alone (rule 25)" \
  "$(has "$out" "job kind-image of .github/workflows/ci.yaml has an if: that can skip it and the name \`\${{ 'kind / image' }}\`, an expression that reads no context, which GitHub evaluates even for a skipped job")"
check "and gives it a name that reads a context" \
  "$(has "$out" "give it a name that is an expression reading a context, such as \`\${{ github.event_name && 'kind-image' }}\`")"
check "names recordings' expression of literals and || in the called workflow (rule 25)" \
  "$(has "$out" "job recordings of .github/workflows/dispatch-browser.yaml has an if: that can skip it and the name \`\${{ 'recordings' || 'recordings (full)' }}\`, an expression that reads no context")"
check "and passes over every name that reads a context" \
  "$(is "$(grep -c 'an expression that reads no context' <<<"$out" || true)" 2)"
check "names the matrix without fail-fast: false (rule 29)" "$(has "$out" "job lint's matrix does not set fail-fast: false")"
check "names main's check-run name the layout lost (rule 30)" "$(has "$out" "carries the check-run name \`text-checks / text-checks\`")"

echo "case: a caller of a reusable workflow whose evaluated name is a check's"
# GitHub writes a skipped caller's record under the name it evaluates (run 45523492636 wrote
# `worker-image` for `${{ github.event_name && 'worker-image' }}`), so a caller named as a check
# run leaves a skipped record that a reader takes for that check: worker-image named as the build
# a check reads, and docs named as kind-image's check run.
root=$(fresh caller-names)
ci=$root/.github/workflows/ci.yaml
replace "$ci" "name: \${{ github.event_name && 'worker-image' }}" "name: \${{ github.event_name && 'worker-image / build' }}"
replace "$ci" "name: \${{ github.event_name && 'docs' }}" "name: \${{ github.event_name && 'kind / image' }}"
run_check "$root"
check "fails (rule 25)" "$(is "$status" 1)"
check "names worker-image, its evaluated name and the check-run name a check reads" \
  "$(has "$out" "job worker-image of .github/workflows/ci.yaml calls .github/workflows/worker-image.yaml, so GitHub writes its skipped record under its evaluated name \`worker-image / build\`, which is the check-run name \`worker-image / build\` that a check reads (ci_pins.MAIN_CHECK_RUN_NAMES)")"
check "names docs, its evaluated name and the check-run name the layout writes" \
  "$(has "$out" "job docs of .github/workflows/ci.yaml calls .github/workflows/docs-site.yaml, so GitHub writes its skipped record under its evaluated name \`kind / image\`, which is the check-run name \`kind / image\` that .github/workflows/ci.yaml job kind-image writes;")"
check "and no other caller" \
  "$(is "$(grep -c 'so GitHub writes its skipped record under its evaluated name' <<<"$out" || true)" 2)"

echo "case: the indexes the legs of a split job name"
root=$(fresh shards)
ci=$root/.github/workflows/ci.yaml
# envoy-go-race-api's two legs named as two of three: index 3 names no leg
replace "$ci" 'name: envoy-go-race-api (1/2)' 'name: envoy-go-race-api (1/3)'
replace "$ci" 'name: envoy-go-race-api (2/2)' 'name: envoy-go-race-api (2/3)'
# envoy-go-docs-finalizers' third leg named as its second: index 2 names two legs
replace "$ci" 'name: envoy-go-docs-finalizers (3/3)' 'name: envoy-go-docs-finalizers (2/3)'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the index no leg names (rule 34)" \
  "$(has "$out" "job go-envoy-held splits envoy-go-race-api into 3 legs, and no leg is \`envoy-go-race-api (3/3)\`")"
check "names the index two legs name (rule 34)" \
  "$(has "$out" "job go-envoy-held splits envoy-go-docs-finalizers into 3 legs, and 2 legs are \`envoy-go-docs-finalizers (2/3)\`")"
check "names the leg whose part is not its index (rule 34)" \
  "$(has "$out" "leg \`envoy-go-docs-finalizers (2/3)\` of job go-envoy-held hands its steps part 3, not its own index 2")"

echo "case: the counts the legs of a split job name, and the counts their steps divide by"
root=$(fresh shard-counts)
ci=$root/.github/workflows/ci.yaml
# dispatch-memory's second leg named as one of three: its legs name two counts
replace "$ci" 'name: dispatch-memory (2/2)' 'name: dispatch-memory (2/3)'
# envoy-go-race-docs' second leg named as the third of two: an index outside 1 to 2
replace "$ci" 'name: envoy-go-race-docs (2/2)' 'name: envoy-go-race-docs (3/2)'
# the finalizers' step hands its three legs a fourth leg's count (CORPUS_SHARD=<i>/4)
replace "$ci" "uses: ./.github/actions/go-envoy-held/envoy-go-docs-finalizers, with: {part: '\${{ matrix.part }}', legs: 3," \
  "uses: ./.github/actions/go-envoy-held/envoy-go-docs-finalizers, with: {part: '\${{ matrix.part }}', legs: 4,"
# go-daemon's third leg's matrix count, which its step reads as \${{ matrix.legs }}
replace "$ci" '{name: daemon-go (3/3), leg: 3, legs: 3,' '{name: daemon-go (3/3), leg: 3, legs: 4,'
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the legs that name two counts (rule 34)" \
  "$(has "$out" "job go-envoy-held's legs of dispatch-memory name 2 counts of legs (2, 3), where every leg names the same count")"
check "names the index outside 1 to n (rule 34)" \
  "$(has "$out" "job go-envoy-held splits envoy-go-race-docs into 2 legs, and a leg is \`envoy-go-race-docs (3/2)\`, an index outside 1 to 2")"
check "names a step's with: legs that is not the legs' count (rule 34)" \
  "$(has "$out" "leg \`envoy-go-docs-finalizers (1/3)\` of job go-envoy-held runs step \"envoy-go-docs-finalizers\", which hands it legs 4, where its name counts 3")"
check "names a matrix count a step reads that is not the leg's (rule 34)" \
  "$(has "$out" "leg \`daemon-go (3/3)\` of job go-daemon runs step \"daemon-go\", which hands it legs 4, where its name counts 3")"
check "and not the legs whose matrix count is right" \
  "$(is "$(has "$out" "leg \`daemon-go (1/3)\` of job go-daemon runs step \"daemon-go\"")" false)"
check "and no step of a leg its if: keeps it out of" \
  "$(is "$(has "$out" "leg \`envoy-go-race-api (1/2)\` of job go-envoy-held runs step")" false)"

echo "case: ci-ok's reconcile step renamed, and its hold step's if: off the term the reconcile reads"
# The reconcile step's if: is held by name, so a renamed step must be refused rather than passed
# over; and the hold step's if: must be the one whose term the reconcile step's negates, or the two
# could both run, or neither, on a failed verdict of a merge group's first attempt.
root=$(fresh ci-ok-steps)
ci=$root/.github/workflows/ci.yaml
replace "$ci" "      - name: Set every other ci-ok on this head to this job's conclusion" \
  "      - name: Set every other ci-ok on this head"
replace "$ci" "!cancelled() && steps.verdict.outcome == 'failure'" "!cancelled() && steps.verdict.outcome != 'success'"
run_check "$root"
check "fails (rule 7)" "$(is "$status" 1)"
check "names the reconcile step it no longer holds" \
  "$(has "$out" "job ci-ok holds 0 step(s) named \"Set every other ci-ok on this head to this job's conclusion\", where it holds one, with \`if: \${{ always() &&")"
check "names the hold step and the if: it must carry" \
  "$(has "$out" "job ci-ok's step \"Hold a failing verdict until this group is the front of the queue\" must carry \`if: \${{ !cancelled() && steps.verdict.outcome == 'failure' && github.event_name == 'merge_group' && github.run_attempt == '1' }}\`")"

echo "case: an AUDITED_SCRIPTS pin a script's text has moved past"
# The case's go_test_steps.py is its own copy and the script is this checkout's, so the pin is
# the one that moves: as stale as an edit of the script past an unchanged pin.
root=$(fresh audited-pin)
actual=$(sha256sum "$repo/packages/envoy/infra/scripts/race-unit.sh" | cut -d' ' -f1)
stale=$(printf '0%.0s' {1..64})
replace "$root/.github/scripts/go_test_steps.py" "sha256=\"$actual\"" "sha256=\"$stale\""
run_check "$root"
check "fails (rule 35)" "$(is "$status" 1)"
check "names the script, its SHA-256 and the pin its entry was read from" \
  "$(has "$out" "packages/envoy/infra/scripts/race-unit.sh has SHA-256 $actual, and its AUDITED_SCRIPTS entry in .github/scripts/go_test_steps.py was read from $stale")"
check "names the steps the reader then refuses, rather than ending the check" \
  "$(has "$out" "the step reader refuses the workflows: ")"
check "and prints no traceback" "$(is "$(has "$out" "Traceback")" false)"

echo "case: the actions' conditions, caches and timed steps, and the merge-group workflows"
root=$(fresh actions)
ci=$root/.github/workflows/ci.yaml
replace "$root/.github/actions/plugin-pack/action.yml" '      run: ../../.github/scripts/go-test.sh' \
  $'      if: github.event_name == \'push\'\n      run: ../../.github/scripts/go-test.sh'
replace "$root/.github/actions/go-daemon/action.yml" \
  'key: go-daemon-${{ runner.os }}-${{ runner.arch }}-go${{ steps.setup-go.outputs.go-version }}' \
  'key: go-daemon-${{ runner.os }}-${{ runner.arch }}-go${{ steps.setup-go.outputs.go-version }}-drift'
replace "$root/.github/actions/architecture-check/action.yml" '${{ runner.temp }}/architecture-check/mod' \
  '${{ runner.temp }}/architecture-check/modules'
# tool-fallback-arm's first crane install loses its bound; two of tool-fallback's three elan
# installs are left
replace "$ci" $'      - {timeout-minutes: 11, uses: ./.github/actions/setup-bun}\n      - {uses: ./.github/actions/tool-fallback/arm, with: {after: healthy-bun}}\n      - {timeout-minutes: 11, uses: ./.github/actions/install-crane}' \
  $'      - {timeout-minutes: 11, uses: ./.github/actions/setup-bun}\n      - {uses: ./.github/actions/tool-fallback/arm, with: {after: healthy-bun}}\n      - {uses: ./.github/actions/install-crane}'
replace "$ci" $'      - {timeout-minutes: 11, uses: ./.github/actions/install-elan}\n      - {timeout-minutes: 11, uses: ./.github/actions/install-uv}\n' \
  $'      - {timeout-minutes: 11, uses: ./.github/actions/install-uv}\n'
printf 'name: Extra\non:\n  merge_group:\njobs:\n  extra:\n    runs-on: ubuntu-24.04\n    steps:\n      - run: "true"\n' \
  >"$root/.github/workflows/extra.yaml"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the push-only if: in an action only ci.yaml runs (rule 10)" \
  "$(has "$out" "of .github/actions/plugin-pack/action.yml has \`if: github.event_name == 'push'\`, which tests")"
check "names the cache key that drifted inside an action (rule 11)" \
  "$(has "$out" "but job go-daemon of .github/workflows/ci.yaml (in .github/actions/go-daemon/action.yml) restores it under")"
check "names the cache path that drifted inside an action ci-caches.yaml runs (rule 11)" \
  "$(has "$out" "the save of .github/actions/architecture-check/action.yml, which job architecture-check runs")"
check "names the install that lost its bound (rule 31)" \
  "$(has "$out" "job tool-fallback-arm of .github/workflows/ci.yaml holds 1 step(s) with uses ./.github/actions/install-crane bound to 11 minutes, where 2 kept")"
check "names the timed installs folded into one (rule 31)" \
  "$(has "$out" "job tool-fallback of .github/workflows/ci.yaml holds 2 step(s) with uses ./.github/actions/install-elan bound to 11 minutes, where 3 kept")"
check "names the second merge-group workflow (rule 14)" "$(has "$out" ".github/workflows/extra.yaml runs on merge_group")"

# Rule 11 over ci-caches.yaml's refills (main's #5141): a family the fill saves under the readers'
# key followed by the commit is filled again on every push from its newest entry, so its restore
# there downloads (no lookup-only) and takes exactly the readers' key and a `-` as its
# restore-keys, and the family is saved under that suffix alone; a save under a computed key names
# a restore of its own job, and no save key holds hashFiles(), which the save evaluates after the
# build (go-envoy-infra's did, and every push missed its lookup: run 45088325195). The real file's
# go-daemon, go-notices, go-envoy, go-envoy-test and go-envoy-darwin-vet are refills, and
# go-envoy-infra and elan saves under their lookups' computed keys: the base passes with each.
echo "case: ci-caches.yaml's refills and computed save keys, each broken (rule 11)"
root=$(fresh cache-refills)
caches=$root/.github/workflows/ci-caches.yaml
replace "$caches" \
  '          restore-keys: go-daemon-${{ runner.os }}-${{ runner.arch }}-go${{ steps.setup-go.outputs.go-version }}-' \
  '          lookup-only: true'
replace "$caches" \
  'restore-keys: go-notices-${{ runner.arch }}-${{ runner.os }}-go${{ steps.setup-go.outputs.go-version }}-' \
  'restore-keys: go-notices-'
replace "$caches" \
  $'\n          restore-keys: go-envoy-${{ runner.os }}-${{ runner.arch }}-go${{ steps.setup-go.outputs.go-version }}-\n' \
  $'\n'
replace "$caches" \
  'key: go-envoy-test-${{ runner.os }}-${{ runner.arch }}-go${{ steps.setup-go.outputs.go-version }}-${{ github.sha }}' \
  'key: go-envoy-test-${{ runner.os }}-${{ runner.arch }}-go${{ steps.setup-go.outputs.go-version }}-${{ github.run_id }}'
replace "$caches" \
  $'          key: go-envoy-darwin-vet-${{ runner.os }}-${{ runner.arch }}-go${{ steps.setup-go.outputs.go-version }}-${{ github.sha }}\n' \
  $'          key: go-envoy-darwin-vet-${{ runner.os }}-${{ runner.arch }}-go${{ steps.setup-go.outputs.go-version }}-${{ github.sha }}\n      - uses: actions/cache/save@55cc8345863c7cc4c66a329aec7e433d2d1c52a9 # v6.1.0\n        with:\n          path: ${{ runner.temp }}/go-build-darwin\n          key: go-envoy-darwin-vet-${{ runner.os }}-${{ runner.arch }}-go${{ steps.setup-go.outputs.go-version }}\n'
replace "$caches" '          key: ${{ steps.go-envoy-infra.outputs.cache-primary-key }}' \
  '          key: ${{ steps.go-other.outputs.cache-primary-key }}'
replace "$caches" '          key: ${{ steps.elan-cache.outputs.cache-primary-key }}' \
  "          key: elan-\${{ runner.os }}-\${{ hashFiles('lean-toolchain') }}"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names a refill's restore that is a lookup" \
  "$(has "$out" "job daemon's actions/cache/restore of family go-daemon is \`lookup-only: true\`, but this workflow saves the family under")"
check "names a refill's restore-keys that take a shorter prefix" \
  "$(has "$out" "job notices's actions/cache/restore of family go-notices has restore-keys go-notices-, but a family this workflow saves under")"
check "names a refill's restore with no restore-keys" \
  "$(has "$out" "job envoy's actions/cache/restore of family go-envoy has restore-keys (none), but a family this workflow saves under")"
check "names a save under another suffix" \
  "$(has "$out" "job envoy saves family go-envoy-test under key")"
check "names a family saved both under the commit and without it" \
  "$(has "$out" "this workflow saves family go-envoy-darwin-vet both under")"
check "names a save under the computed key of no restore of its job" \
  "$(has "$out" "job envoy-infra saves under")"
check "names a save key that holds hashFiles()" \
  "$(has "$out" "job elan saves under")"

echo "case: the verdict job's reconcile step runs on a dispatch of another ref, or after the hold"
# On a dispatch of another ref every other ci-ok of the head is a pull request's or the queue's,
# and the step would set each posted one to that run's verdict; `if: always()` alone was the shape
# before, and the expression's comparison of github.ref read refs/heads/Main as main. The third
# shape, rule 7's text before main's #5186, runs the step on a merge group's first attempt whose
# verdict failed too, after the hold step, which must end the job so its check run concludes the
# moment the group is the front.
reconcile_if="\${{ always() && (github.event_name != 'workflow_dispatch' || needs.changes.outputs.ref_is_main == 'true') && !(steps.verdict.outcome == 'failure' && github.event_name == 'merge_group' && github.run_attempt == '1') }}"
n=0
for given in "always()" "\${{ always() && !(github.event_name == 'workflow_dispatch' && github.ref != 'refs/heads/main') }}" \
  "\${{ always() && (github.event_name != 'workflow_dispatch' || needs.changes.outputs.ref_is_main == 'true') }}"; do
  n=$((n + 1))
  root=$(fresh "reconcile-if-$n")
  replace "$root/.github/workflows/ci.yaml" "        if: $reconcile_if" "        if: $given"
  run_check "$root"
  check "if: $given: fails (rule 7)" "$(is "$status" 1)"
  check "if: $given: names the step and the if: it must carry" \
    "$(has "$out" "job ci-ok's step \"Set every other ci-ok on this head to this job's conclusion\" must carry \`if: $reconcile_if\`")"
done

echo "case: a carrier of main's race job name, and one whose name its pin no longer gives"
# daemon-go-race's legs are `daemon-go-race (i/2)`, and their carrier keeps main's name
# `daemon-go / daemon-go-race` by ci_pins.CARRIER_NAMES; without the pin it names no split job.
root=$(fresh carrier-names)
pins "$root" 'CARRIER_NAMES = {}'
run_check "$root"
check "fails without the pin (rule 32)" "$(is "$status" 1)"
check "names the carrier whose name is no split job's" \
  "$(has "$out" "job daemon-go-race runs .github/scripts/leg-verdict.sh, a carrier, and carries \`daemon-go / daemon-go-race\`")"
check "and the split job left with no carrier" \
  "$(has "$out" "job go-daemon splits daemon-go-race into 2 legs, and 0 carrier(s) carry \`daemon-go-race / daemon-go-race\`")"

echo "case: the hold step of ci-ok without its event gate"
root=$(fresh hold-gate)
pins "$root" 'EVENT_GATES = [g for g in EVENT_GATES if g[2] != "Hold a failing verdict until this group is the front of the queue"]'
run_check "$root"
check "fails (rule 28)" "$(is "$status" 1)"
check "names the hold step's merge_group gate" \
  "$(has "$out" "job ci-ok step \"Hold a failing verdict until this group is the front of the queue\" has")"

echo "case: the notes"
root=$(fresh notes)
printf '\n## daemon_go_tests: The daemon-go job: what its two Go steps read together\n\nText.\n\n## Old filters\n\nText.\n' \
  >>"$root/.github/ci-notes/ci.md"
printf '# why: .github/ci-notes/ci.md#daemon_go_tests-the-daemon-go-job-what-its-two-go-steps-read-together\n# why: .github/ci-notes/ci.md#no-such-section\n' \
  >>"$root/.github/actions/plugin-pack/action.yml"
run_check "$root"
check "fails" "$(is "$status" 1)"
check "names the heading that names no output or job (rule 33)" \
  "$(has "$out" "\`## daemon_go_tests: The daemon-go job: what its two Go steps read together\` names daemon_go_tests, which is neither")"
check "names the section no link names (rule 33)" "$(has "$out" "\`## Old filters\` is the target of no why: link")"
check "names the link to no section (rule 33)" "$(has "$out" "links .github/ci-notes/ci.md#no-such-section, a section that file does not hold")"

echo "case: a darwin vet over a module whose tag sets cover it, and over two that do not"
root=$(fresh vet)
mkdir -p "$root/vetmod" "$root/.github/actions/vetmod"
printf 'module example.com/vetmod\n\ngo 1.22\n' >"$root/vetmod/go.mod"
printf 'go 1.22\n\nuse .\n' >"$root/vetmod/go.work"
printf 'package vetmod\n\nfunc A() int { return 1 }\n' >"$root/vetmod/a.go"
printf '//go:build special\n\npackage vetmod\n\nfunc B() int { return 2 }\n' >"$root/vetmod/b_special.go"
printf '//go:build !special\n\npackage vetmod\n\nfunc C() int { return 3 }\n' >"$root/vetmod/c_plain.go"
vet_action() { # vet_action <run text>
  {
    printf 'name: vet\ndescription: A darwin vet.\nruns:\n  using: composite\n  steps:\n'
    printf '    - name: Vet vetmod for darwin\n      shell: bash\n      working-directory: vetmod\n      run: |\n'
    sed 's/^/        /' <<<"$1"
  } >"$root/.github/actions/vetmod/action.yml"
}
vet_action "$(PYTHONPATH="$root/.github/scripts" python3 -c 'import ci_pins; print(ci_pins.darwin_vet_script("vetmod"))')"
pins "$root" 'DARWIN_VETS = [(".github/actions/vetmod/action.yml", "Vet vetmod for darwin", "vetmod")]'
run_check "$root"
check "a vet over the sets go_build_tags.py prints passes" "$(is "$status" 0)"
check "reports the two sets it vets under" "$(has "$out" '"Vet vetmod for darwin": 2 tag set(s), 3 of 3 files compiled, 0 missed')"
vet_action $'GOOS=darwin GOARCH=arm64 go vet -tags=special ./...'
run_check "$root"
check "a vet that stops reading go_build_tags.py fails (rule 20)" "$(is "$status" 1)"
check "names the step and the vet it must run" \
  "$(has "$out" 'holds no single step "Vet vetmod for darwin" in vetmod whose run: is the darwin vet that takes its tag sets from .github/scripts/go_build_tags.py')"
vet_action "$(PYTHONPATH="$root/.github/scripts" python3 -c 'import ci_pins; print(ci_pins.darwin_vet_script("vetmod"))')"
replace "$root/.github/scripts/go_build_tags.py" '    sets = [every]' '    return [every]'
run_check "$root"
check "a set list that misses a file fails (rule 20)" "$(is "$status" 1)"
check "names the file no set compiles" "$(has "$out" 'none of them compiles vetmod/c_plain.go, which darwin builds with -tags (none)')"

summary "check-ci-layout.sh holds the CI layout"
