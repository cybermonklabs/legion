#!/usr/bin/env bash
# Tests for the label terms of ci.yaml's `changes` outputs, through ci_router_outputs.py, which
# evaluates this repository's own ci.yaml as GitHub would: on a pull request the slow suites'
# outputs are false whatever their paths say, `full-ci` turns every one of them on, a
# `lane:<output>` label turns on that one output and no other, still only where its paths
# matched, and a label that names no such output turns on nothing
# (.github/ci-notes/ci.md#a-lane-label-turns-on-one-slow-suite-on-a-pull-request).
#
# The held-back set is read, not listed here, and read apart from the label terms it tests: the
# outputs a merge group turns on and an unlabelled pull request holds false, every paths filter
# saying yes in both. A read that finds none fails, since every case below would then pass over
# nothing. Two copies of ci.yaml, each with one label literal broken, show that a broken term
# fails its case rather than leaving the set.
#
# It holds a dispatched run's selection and the workflow's cancel key the same way, through the
# changes job's own steps and the workflow's own `concurrency:`
# (.github/ci-notes/ci.md#what-a-dispatched-run-of-a-branch-selects-and-when-to-ask-for-every-job):
# a dispatch of main selects every job whatever the filters say, a dispatch of a branch selects
# what a merge group with the same verdicts selects and diffs its own commit with main,
# `every-job` selects every job on a branch, and a newer dispatch of a branch cancels that
# branch's older one while a dispatch of main is never cancelled. Copies of ci.yaml with one of
# those terms broken show that each case fails without it.
#
# Run from anywhere: .github/scripts/ci-router-outputs.test.sh
# CI runs it in ci.yaml's test job.
set -euo pipefail
shopt -s inherit_errexit

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(cd "$script_dir/../.." && pwd)

# shellcheck source=.github/scripts/test-lib.sh
source "$script_dir/test-lib.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# outputs_of <workflow> <argument>...: every output as `name=value`, sorted, for one event and set
# of labels; outputs <argument>... reads this repository's own ci.yaml.
outputs_of() {
  local workflow=$1
  shift
  "$script_dir/ci_router_outputs.py" "$workflow" "$@" | sort
}
outputs() { outputs_of "$repo/.github/workflows/ci.yaml" "$@"; }
# value <outputs text> <name>: one output's value.
value() { sed -n "s/^$2=//p" <<<"$1"; }
# others_equal <outputs> <baseline> <name>...: true when every output but those named reads as
# in the baseline.
others_equal() {
  local got=$1 base=$2
  shift 2
  local pattern
  pattern=$(printf '^%s=|' "$@")
  pattern=${pattern%|}
  [ "$(grep -Ev "$pattern" <<<"$got")" = "$(grep -Ev "$pattern" <<<"$base")" ] && echo true || echo false
}
# held_of <workflow>: the outputs whose expression reads the pull request's labels, one per line,
# found in the text (ci_router_outputs.py --label-readers), so no broken term of an output can
# take it out of the set the cases below test.
held_of() { "$script_dir/ci_router_outputs.py" "$1" --label-readers; }
# not_value <workflow> <want> <argument>...: each output of held_of that does not read <want>
# for that event and those labels.
not_value() {
  local workflow=$1 want=$2 name got
  shift 2
  got=$(outputs_of "$workflow" "$@")
  while read -r name; do
    [ "$(value "$got" "$name")" = "$want" ] || echo "$name"
  done < <(held_of "$workflow")
}
# off_under <workflow> <label>...: each output of held_of that those labels leave off on a pull
# request.
off_under() {
  local workflow=$1 label
  local args=()
  shift
  for label in "$@"; do args+=(--label "$label"); done
  not_value "$workflow" true --event pull_request "${args[@]}"
}

baseline=$(outputs --event pull_request)
mapfile -t held < <(held_of "$repo/.github/workflows/ci.yaml")

echo "case: exactly the nine gated outputs read the pull request's labels"
# A count and the names, so an output whose label term is deleted outright fails here by name
# rather than leaving the set. A tenth gated suite adds its name to this list.
gated='dispatch_memory dispatch_multiprocess dispatch_two_builds envoy_go envoy_go_claim_model envoy_go_race envoy_go_race_api envoy_go_race_docs envoy_smoke'
check "the reader finds ${#held[@]}: ${held[*]}" "$(is "${#held[@]}" 9)"
check "and they are the nine" "$(is "$(printf '%s\n' "${held[@]}" | sort | tr '\n' ' ')" "$gated ")"

echo "case: each of them is on for a merge group and for a push, where no label is read"
check "a merge group leaves none off" \
  "$(is "$(not_value "$repo/.github/workflows/ci.yaml" true --event merge_group | tr '\n' ' ')" "")"
check "a push leaves none off" \
  "$(is "$(not_value "$repo/.github/workflows/ci.yaml" true --event push | tr '\n' ' ')" "")"

echo "case: each of them is held off on an unlabelled pull request"
check "none is on" "$(is "$(not_value "$repo/.github/workflows/ci.yaml" false --event pull_request | tr '\n' ' ')" "")"

echo "case: full-ci turns each of them on"
check "no one stays off under full-ci" \
  "$(is "$(off_under "$repo/.github/workflows/ci.yaml" full-ci | tr '\n' ' ')" "")"

echo "case: a lane label turns on its own output and no other"
for name in "${held[@]}"; do
  got=$(outputs --event pull_request --label "lane:$name")
  check "lane:$name turns $name on" "$(is "$(value "$got" "$name")" true)"
  check "lane:$name leaves every other output as an unlabelled pull request has it" \
    "$(others_equal "$got" "$baseline" "$name")"
done

echo "case: two lane labels turn on their two outputs"
got=$(outputs --event pull_request --label "lane:${held[0]}" --label "lane:${held[-1]}")
check "${held[0]} is true" "$(is "$(value "$got" "${held[0]}")" true)"
check "${held[-1]} is true" "$(is "$(value "$got" "${held[-1]}")" true)"
check "and nothing else moves" "$(others_equal "$got" "$baseline" "${held[0]}" "${held[-1]}")"

echo "case: a lane label still needs its paths, as full-ci does"
got=$(outputs --event pull_request --label "lane:${held[0]}" --verdict "${held[0]}=false")
check "${held[0]} stays false when its filter says no" "$(is "$(value "$got" "${held[0]}")" false)"
got=$(outputs --event pull_request --label full-ci --verdict "${held[0]}=false")
check "and so it does under full-ci" "$(is "$(value "$got" "${held[0]}")" false)"

echo "case: a label that names no held-back output turns on nothing"
for label in lane:no_such_output lane:typecheck lane:envoy-go "lane:${held[0]}x" "${held[0]}" lane:; do
  got=$(outputs --event pull_request --label "$label")
  check "$label leaves every output as an unlabelled pull request has it" "$(is "$got" "$baseline")"
done

# broken_copy <dir> <output> <text> <replacement>: a copy of this repository's ci.yaml and filters
# under <dir> with <text> changed to <replacement> on that output's line alone (neither holds a
# `|`, which the sed below delimits with).
broken_copy() {
  mkdir -p "$1/.github/workflows"
  cp "$repo/.github/ci-filters.yaml" "$1/.github/ci-filters.yaml"
  sed -E "/^ +$2: /s|$3|$4|" "$repo/.github/workflows/ci.yaml" > "$1/.github/workflows/ci.yaml"
  cmp -s "$repo/.github/workflows/ci.yaml" "$1/.github/workflows/ci.yaml" && echo "broken_copy: $2 has no $3" >&2
  echo "$1/.github/workflows/ci.yaml"
}

echo "case: an output whose full-ci literal is broken stays in the set and fails the full-ci case"
broken=$(broken_copy "$work/no-full-ci" envoy_go_race_api "'full-ci'" "'full-cj'")
check "the copy differs from ci.yaml" "$(is "$(cmp -s "$broken" "$repo/.github/workflows/ci.yaml" && echo same)" "")"
check "the set keeps all ${#held[@]}" "$(is "$(held_of "$broken" | wc -l)" "${#held[@]}")"
check "full-ci leaves exactly envoy_go_race_api off" "$(is "$(off_under "$broken" full-ci | tr '\n' ' ')" "envoy_go_race_api ")"

echo "case: an output whose lane literal is broken fails its lane row"
broken=$(broken_copy "$work/no-lane" envoy_smoke "'lane:envoy_smoke'" "'lane:envoy_smokx'")
check "the set keeps all ${#held[@]}" "$(is "$(held_of "$broken" | wc -l)" "${#held[@]}")"
check "lane:envoy_smoke leaves it off" "$(is "$(off_under "$broken" lane:envoy_smoke | grep -c '^envoy_smoke$')" 1)"
check "full-ci still turns it on" "$(is "$(off_under "$broken" full-ci | tr '\n' ' ')" "")"

echo "case: an output whose event term is broken stays in the set and fails the merge-group case"
broken=$(broken_copy "$work/no-event" envoy_go_race "github.event_name != 'pull_request'" "github.event_name == 'workflow_dispatch'")
check "the copy differs from ci.yaml" "$(is "$(cmp -s "$broken" "$repo/.github/workflows/ci.yaml" && echo same)" "")"
check "the set keeps all ${#held[@]}" "$(is "$(held_of "$broken" | wc -l)" "${#held[@]}")"
check "a merge group leaves exactly envoy_go_race off" \
  "$(is "$(not_value "$broken" true --event merge_group | tr '\n' ' ')" "envoy_go_race ")"

echo "case: an output that reads the labels through the whole pull request is refused, never missed"
broken=$(broken_copy "$work/whole-pr" envoy_smoke \
  "contains\(github\.event\.pull_request\.labels\.\*\.name, 'lane:envoy_smoke'\)" \
  "contains(toJSON(github.event.pull_request), 'lane:envoy_smoke')")
check "the copy differs from ci.yaml" "$(is "$(cmp -s "$broken" "$repo/.github/workflows/ci.yaml" && echo same)" "")"
set +e
err=$("$script_dir/ci_router_outputs.py" "$broken" --event pull_request 2>&1 >/dev/null)
status=$?
set -e
check "exits 2" "$(is "$status" 2)"
check "names envoy_smoke and the read" \
  "$(contains "$err" 'output envoy_smoke: context github.event.pull_request is outside the modelled contexts')"

ci="$repo/.github/workflows/ci.yaml"
# selecting <outputs>: the outputs that select a job, as `name=value`, one per line: every output
# but filter_verdicts, the filter's own verdicts as JSON over several lines, and ref_is_main.
selecting() { grep -E '^[a-z0-9_]+=' <<<"$1" | grep -Ev '^(filter_verdicts|ref_is_main)=' || true; }
# on_of / off_of <outputs>: the selecting outputs that are 'true' / not 'true', space-separated.
on_of() { selecting "$1" | sed -n 's/=true$//p' | tr '\n' ' '; }
off_of() { selecting "$1" | grep -v '=true$' | cut -d= -f1 | tr '\n' ' ' || true; }
# How many outputs select a job, read from the file itself, so a case that reads fewer fails.
n_selecting=$(python3 -c 'import sys, yaml; print(len(yaml.safe_load(open(sys.argv[1]))["jobs"]["changes"]["outputs"]) - 2)' "$ci")
mapfile -t filters < <(python3 -c 'import sys, yaml; print("\n".join(yaml.safe_load(open(sys.argv[1]))))' "$repo/.github/ci-filters.yaml")
old=1111111111111111111111111111111111111111
new=2222222222222222222222222222222222222222

echo "case: a dispatch of main selects every job, whatever each filter says"
got=$(outputs --event workflow_dispatch --ref refs/heads/main --verdicts false)
check "it reads all $n_selecting outputs that select a job" "$(is "$(selecting "$got" | wc -l)" "$n_selecting")"
check "every one of them is true" "$(is "$(off_of "$got")" "")"
check "ref_is_main is true" "$(is "$(value "$got" ref_is_main)" true)"
check "filter_verdicts is {}, the filter step skipped" "$(is "$(value "$got" filter_verdicts)" "{}")"

echo "case: a dispatch of a branch selects nothing when no filter matches"
got=$(outputs --event workflow_dispatch --ref refs/heads/feature --verdicts false)
check "it reads all $n_selecting outputs that select a job" "$(is "$(selecting "$got" | wc -l)" "$n_selecting")"
check "none of them is true" "$(is "$(on_of "$got")" "")"

echo "case: a dispatch of a branch selects what a merge group with the same verdicts selects"
check "the filters file holds filters to try" "$(is "$([ "${#filters[@]}" -gt 1 ] && echo yes)" yes)"
for parity in 0 1 all none; do
  args=(--verdicts false)
  case $parity in
    all) args=(--verdicts true) ;;
    none) ;;
    *) for i in "${!filters[@]}"; do [ $((i % 2)) -ne "$parity" ] || args+=(--verdict "${filters[$i]}=true"); done ;;
  esac
  dispatched=$(outputs --event workflow_dispatch --ref refs/heads/feature "${args[@]}")
  queued=$(outputs --event merge_group "${args[@]}")
  check "filters $parity: the same outputs, value for value" "$(is "$(selecting "$dispatched")" "$(selecting "$queued")")"
  check "filters $parity: all $n_selecting read" "$(is "$(selecting "$dispatched" | wc -l)" "$n_selecting")"
  case $parity in
    0 | 1) check "filters $parity: some jobs selected and some left off" \
      "$(is "$([ -n "$(on_of "$dispatched")" ] && [ -n "$(off_of "$dispatched")" ] && echo both)" both)" ;;
  esac
done

echo "case: every-job selects every job on a branch"
got=$(outputs --event workflow_dispatch --ref refs/heads/feature --input every-job=true --verdicts false)
check "every one of them is true" "$(is "$(off_of "$got")" "")"
check "ref_is_main stays false" "$(is "$(value "$got" ref_is_main)" false)"
got=$(outputs --event workflow_dispatch --ref refs/heads/feature --input every-job=false --verdicts false)
check "every-job=false selects by the diff" "$(is "$(on_of "$got")" "")"

# filter_step <argument>...: whether the filter step runs, and the base and ref it is handed.
filter_step() { "$script_dir/ci_router_outputs.py" "$ci" --filter-step "$@" | tr '\n' ' '; }
echo "case: the filter step diffs a dispatched branch's own commit with main, and reads a merge group's and a pull request's own event"
check "a dispatch of a branch: base main, ref its commit" \
  "$(is "$(filter_step --event workflow_dispatch --ref refs/heads/feature --sha "$old")" "runs=true base=main ref=$old ")"
check "a dispatch of main skips the step" "$(contains "$(filter_step --event workflow_dispatch --ref refs/heads/main)" '^runs=false ')"
check "every-job skips the step" \
  "$(contains "$(filter_step --event workflow_dispatch --ref refs/heads/feature --input every-job=true)" '^runs=false ')"
check "a merge group: neither input, so its own base_sha..head_sha" "$(is "$(filter_step --event merge_group)" "runs=true base= ref= ")"
check "a pull request: neither input, so the API's file list" "$(is "$(filter_step --event pull_request)" "runs=true base= ref= ")"

# concurrency_of <workflow> <argument>...: the run's concurrency group and cancel-in-progress.
concurrency_of() {
  local workflow=$1
  shift
  "$script_dir/ci_router_outputs.py" "$workflow" --concurrency "$@" | tr '\n' ' '
}
echo "case: a newer dispatch of a branch cancels the older one; a dispatch of main never is"
check "a pull request keys on its number and cancels" \
  "$(is "$(concurrency_of "$ci" --event pull_request)" "group=ci-1 cancel-in-progress=true ")"
check "a merge group keys on its commit and is never cancelled" \
  "$(is "$(concurrency_of "$ci" --event merge_group --sha "$old")" "group=ci-$old cancel-in-progress=false ")"
check "a dispatch of main keys on its commit and is never cancelled" \
  "$(is "$(concurrency_of "$ci" --event workflow_dispatch --ref refs/heads/main --sha "$old")" "group=ci-$old cancel-in-progress=false ")"
check "  so a second dispatch of main at another commit is a group of its own" \
  "$(is "$(concurrency_of "$ci" --event workflow_dispatch --ref refs/heads/main --sha "$new")" "group=ci-$new cancel-in-progress=false ")"
check "a dispatch of a branch keys on the branch and cancels" \
  "$(is "$(concurrency_of "$ci" --event workflow_dispatch --ref refs/heads/feature --sha "$old")" "group=ci-refs/heads/feature cancel-in-progress=true ")"
check "  and a newer dispatch of it at another commit joins that group" \
  "$(is "$(concurrency_of "$ci" --event workflow_dispatch --ref refs/heads/feature --sha "$new")" "group=ci-refs/heads/feature cancel-in-progress=true ")"
check "  while another branch is another group" \
  "$(is "$(concurrency_of "$ci" --event workflow_dispatch --ref refs/heads/other)" "group=ci-refs/heads/other cancel-in-progress=true ")"
check "a branch spelled Main keys on its commit, since the expression ignores case, and is never cancelled" \
  "$(is "$(concurrency_of "$ci" --event workflow_dispatch --ref refs/heads/Main --sha "$old")" "group=ci-$old cancel-in-progress=false ")"

# copy_with <dir> <text> <replacement>: a copy of this repository's ci.yaml and filters under <dir>
# with the one occurrence of <text> replaced; it fails when <text> is not there exactly once.
copy_with() {
  mkdir -p "$1/.github/workflows"
  cp "$repo/.github/ci-filters.yaml" "$1/.github/ci-filters.yaml"
  python3 - "$ci" "$1/.github/workflows/ci.yaml" "$2" "$3" <<'PY'
import sys
source, target, text, replacement = sys.argv[1:]
body = open(source).read()
if body.count(text) != 1:
    sys.exit(f"copy_with: {text!r} occurs {body.count(text)} times in {source}, not once")
open(target, "w").write(body.replace(text, replacement))
PY
  echo "$1/.github/workflows/ci.yaml"
}

echo "case: a scope step that reads no main fails the dispatch-of-main case"
broken=$(copy_with "$work/scope-no-main" '{ [ "$IS_MAIN" = true ] || [ "$EVERY_JOB" = true ]; }' '[ "$EVERY_JOB" = true ]')
got=$(outputs_of "$broken" --event workflow_dispatch --ref refs/heads/main --verdicts false)
check "a dispatch of main then selects nothing" "$(is "$(on_of "$got")" "")"

echo "case: an output still forced on every dispatch fails the branch case"
broken=$(copy_with "$work/output-every-dispatch" "typecheck: \${{ steps.scope.outputs.every_job == 'true'" "typecheck: \${{ github.event_name == 'workflow_dispatch'")
got=$(outputs_of "$broken" --event workflow_dispatch --ref refs/heads/feature --verdicts false)
check "a dispatch of a branch then selects exactly typecheck" "$(is "$(on_of "$got")" "typecheck ")"
check "  which a merge group with the same verdicts does not" \
  "$(is "$(on_of "$(outputs_of "$broken" --event merge_group --verdicts false)")" "")"

echo "case: a cancel key without the dispatch arm fails the branch case"
broken=$(copy_with "$work/no-dispatch-key" "github.event_name == 'workflow_dispatch' && github.ref != 'refs/heads/main' && github.ref || " "")
check "a dispatch of a branch then keys on its commit" \
  "$(contains "$(concurrency_of "$broken" --event workflow_dispatch --ref refs/heads/feature --sha "$old")" "^group=ci-$old ")"

echo "case: an expression this reader does not model is refused, never read as false"
mkdir -p "$work/.github/workflows"
printf 'f: []\n' > "$work/.github/ci-filters.yaml"
cat > "$work/.github/workflows/ci.yaml" <<'YAML'
jobs:
  changes:
    outputs:
      x: ${{ github.actor == 'someone' && 'true' || 'false' }}
YAML
set +e
err=$("$script_dir/ci_router_outputs.py" "$work/.github/workflows/ci.yaml" --event pull_request 2>&1 >/dev/null)
status=$?
set -e
check "exits 2" "$(is "$status" 2)"
check "names the output and the context" "$(contains "$err" 'output x: context github.actor is outside the modelled contexts')"

summary "ci.yaml's changes outputs turn on what full-ci, the lane labels and a dispatch's ref and diff name, and its cancel key cancels only a branch's older dispatch"
