#!/usr/bin/env bash
# Tests for the label terms of `.github/workflows/ci.yaml#changes` outputs, through ci_router_outputs.py, which
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
# what a merge group with the same verdicts selects and diffs its own commit with main, the filter
# generation measures a branch's dispatch against its merge base with main as it measures a merge
# group against its base, and is handed that base only because the base step runs before it (the
# runner evaluates a step's env when the step starts), `every-job` selects every job on a branch,
# and a newer dispatch of a branch cancels that branch's older one while a dispatch of main is
# never cancelled. Copies of ci.yaml with one of those terms broken, or the two steps swapped, show
# that each case fails without it.
#
# The filter names the outputs read verdicts of are the changes job's filters, which no file of the
# repository holds: the job computes them when it runs. The script reads them from the file
# CI_FILTERS_FILE names - the test job hands it the file this run's changes job computed - and,
# with none given, from this tree's, computed here with `check-ci-filters.sh generate`.
#
# It also holds the test job's step gates, which read one output, the paths filter's own list of
# the filters that matched: a step whose filter matched runs, one whose filter did not is skipped,
# even where another filter's name holds its own, a dispatch of a branch runs the steps its own
# diff's filters matched, and every one runs on a run that selects every job (a dispatch of main,
# a tag or every-job), when the filters fell open and when changes did not succeed. Copies with
# one read misspelt, unquoted, never `all` or `all` on every dispatch show each case fails on what
# it guards. Every copy is made by copy_with, which stops the harness, naming the text, when the
# text it is to change is not in the file exactly once.
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

filters=${CI_FILTERS_FILE:-}
if [ -z "$filters" ]; then
  filters=$work/computed-filters.yaml
  if ! "$script_dir/check-ci-filters.sh" generate "$filters" > "$work/generate.log" 2>&1; then
    tail -20 "$work/generate.log" >&2
    echo "ci-router-outputs.test.sh: no CI_FILTERS_FILE is given, and \`check-ci-filters.sh generate\` could not compute the changes job's filters" >&2
    exit 1
  fi
fi

# outputs_of <workflow> <argument>...: every output as `name=value`, sorted, for one event and set
# of labels, the filter names read from the computed filters; outputs <argument>... reads this
# repository's own ci.yaml.
outputs_of() {
  local workflow=$1
  shift
  "$script_dir/ci_router_outputs.py" "$workflow" --filters "$filters" "$@" | sort
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

echo "case: exactly the sixteen gated outputs read the pull request's labels"
# A count and the names, so an output whose label term is deleted outright fails here by name
# rather than leaving the set. A seventeenth gated suite adds its name to this list.
gated='daemon_go_tests dispatch_browser dispatch_memory dispatch_multiprocess dispatch_references dispatch_two_builds envoy_go envoy_go_checks envoy_go_claim_model envoy_go_race envoy_go_race_api envoy_go_race_docs envoy_infra_go envoy_smoke pi_legion test'
check "the reader finds ${#held[@]}: ${held[*]}" "$(is "${#held[@]}" 16)"
check "and they are the sixteen" "$(is "$(printf '%s\n' "${held[@]}" | sort | tr '\n' ' ')" "$gated ")"

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

echo "case: a verdict for a filter the computed filters do not hold is refused, naming their file"
set +e
err=$("$script_dir/ci_router_outputs.py" "$repo/.github/workflows/ci.yaml" --filters "$filters" \
  --event pull_request --verdict no_such_filter=false 2>&1 >/dev/null)
status=$?
set -e
check "exits 2" "$(is "$status" 2)"
check "names the verdict and the file" "$(contains "$err" "--verdict no_such_filter=false names no filter of $filters\$")"

# copy_with <dir> <file> <text> <replacement> [<text> <replacement>]...: a copy of this repository's
# ci.yaml and ci-job-test.yaml under <dir>, the one named <file> with each <text> replaced by its
# <replacement>, in order; echoes the copy's ci.yaml. It is the one way a case here changes a
# workflow. A <text> that the file does not hold exactly once, after the pairs before it, a
# replacement that is its own text, and a <file> that is neither workflow stop the harness, naming
# the file and the text, since a case run over an unchanged copy would test ci.yaml as it is.
copy_with() {
  local dir=$1 file=$2
  shift 2
  mkdir -p "$dir/.github/workflows"
  cp "$repo/.github/workflows/ci.yaml" "$repo/.github/workflows/ci-job-test.yaml" "$dir/.github/workflows/"
  if [ ! -f "$dir/.github/workflows/$file" ]; then
    echo "copy_with: $file is neither ci.yaml nor ci-job-test.yaml, so there is nothing to change" >&2
    return 1
  fi
  python3 - "$dir/.github/workflows/$file" "$@" <<'PY'
import sys
path, pairs = sys.argv[1], sys.argv[2:]
if not pairs or len(pairs) % 2:
    sys.exit(f"copy_with: {path}: give <text> <replacement> pairs, not {len(pairs)} argument(s)")
body = open(path).read()
for text, replacement in zip(pairs[::2], pairs[1::2]):
    if body.count(text) != 1:
        sys.exit(f"copy_with: {path} holds {text!r} {body.count(text)} times, not once")
    if replacement == text:
        sys.exit(f"copy_with: {path}: {text!r} is replaced by itself, which changes nothing")
    body = body.replace(text, replacement)
open(path, "w").write(body)
PY
  echo "$dir/.github/workflows/ci.yaml"
}

echo "case: an output whose full-ci literal is broken stays in the set and fails the full-ci case"
broken=$(copy_with "$work/no-full-ci" ci.yaml \
  "contains(github.event.pull_request.labels.*.name, 'full-ci') || contains(github.event.pull_request.labels.*.name, 'lane:envoy_go_race_api')" \
  "contains(github.event.pull_request.labels.*.name, 'full-cj') || contains(github.event.pull_request.labels.*.name, 'lane:envoy_go_race_api')")
check "the copy differs from ci.yaml" "$(is "$(cmp -s "$broken" "$repo/.github/workflows/ci.yaml" && echo same)" "")"
check "the set keeps all ${#held[@]}" "$(is "$(held_of "$broken" | wc -l)" "${#held[@]}")"
check "full-ci leaves exactly envoy_go_race_api off" "$(is "$(off_under "$broken" full-ci | tr '\n' ' ')" "envoy_go_race_api ")"

echo "case: an output whose lane literal is broken fails its lane row"
broken=$(copy_with "$work/no-lane" ci.yaml "'lane:envoy_smoke'" "'lane:envoy_smokx'")
check "the set keeps all ${#held[@]}" "$(is "$(held_of "$broken" | wc -l)" "${#held[@]}")"
check "lane:envoy_smoke leaves it off" "$(is "$(off_under "$broken" lane:envoy_smoke | grep -c '^envoy_smoke$')" 1)"
check "full-ci still turns it on" "$(is "$(off_under "$broken" full-ci | tr '\n' ' ')" "")"

echo "case: an output whose event term is broken stays in the set and fails the merge-group case"
broken=$(copy_with "$work/no-event" ci.yaml \
  "(github.event_name != 'pull_request' || contains(github.event.pull_request.labels.*.name, 'full-ci') || contains(github.event.pull_request.labels.*.name, 'lane:envoy_go_race'))" \
  "(github.event_name == 'workflow_dispatch' || contains(github.event.pull_request.labels.*.name, 'full-ci') || contains(github.event.pull_request.labels.*.name, 'lane:envoy_go_race'))")
check "the copy differs from ci.yaml" "$(is "$(cmp -s "$broken" "$repo/.github/workflows/ci.yaml" && echo same)" "")"
check "the set keeps all ${#held[@]}" "$(is "$(held_of "$broken" | wc -l)" "${#held[@]}")"
check "a merge group leaves exactly envoy_go_race off" \
  "$(is "$(not_value "$broken" true --event merge_group | tr '\n' ' ')" "envoy_go_race ")"

echo "case: an output that reads the labels through the whole pull request is refused, never missed"
broken=$(copy_with "$work/whole-pr" ci.yaml \
  "contains(github.event.pull_request.labels.*.name, 'lane:envoy_smoke')" \
  "contains(toJSON(github.event.pull_request), 'lane:envoy_smoke')")
check "the copy differs from ci.yaml" "$(is "$(cmp -s "$broken" "$repo/.github/workflows/ci.yaml" && echo same)" "")"
set +e
err=$("$script_dir/ci_router_outputs.py" "$broken" --filters "$filters" --event pull_request 2>&1 >/dev/null)
status=$?
set -e
check "exits 2" "$(is "$status" 2)"
check "names envoy_smoke and the read" \
  "$(contains "$err" 'output envoy_smoke: context github.event.pull_request is outside the modelled contexts')"

ci="$repo/.github/workflows/ci.yaml"
# selecting <outputs>: the outputs that select a job, as `name=value`, one per line: every output
# but filter_verdicts, the filter's own verdicts as JSON over several lines, matched_filters, the
# list the test job's steps read their gates out of (the cases further down hold it), and
# ref_is_main.
selecting() { grep -E '^[a-z0-9_]+=' <<<"$1" | grep -Ev '^(filter_verdicts|matched_filters|ref_is_main)=' || true; }
# on_of / off_of <outputs>: the selecting outputs that are 'true' / not 'true', space-separated.
on_of() { selecting "$1" | sed -n 's/=true$//p' | tr '\n' ' '; }
off_of() { selecting "$1" | grep -v '=true$' | cut -d= -f1 | tr '\n' ' ' || true; }
# How many outputs select a job, read from the file itself, so a case that reads fewer fails.
n_selecting=$(python3 -c 'import sys, yaml; print(len(yaml.safe_load(open(sys.argv[1]))["jobs"]["changes"]["outputs"]) - 3)' "$ci")
mapfile -t names < <(python3 -c 'import sys, yaml; print("\n".join(yaml.safe_load(open(sys.argv[1]))))' "$filters")
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
check "the computed filters hold filters to try" "$(is "$([ "${#names[@]}" -gt 1 ] && echo yes)" yes)"
for parity in 0 1 all none; do
  args=(--verdicts false)
  case $parity in
    all) args=(--verdicts true) ;;
    none) ;;
    *) for i in "${!names[@]}"; do [ $((i % 2)) -ne "$parity" ] || args+=(--verdict "${names[$i]}=true"); done ;;
  esac
  dispatched=$(outputs --event workflow_dispatch --ref refs/heads/feature "${args[@]}")
  queued=$(outputs --event merge_group "${args[@]}")
  check "filters $parity: the same outputs, value for value" "$(is "$(selecting "$dispatched")" "$(selecting "$queued")")"
  check "filters $parity: all $n_selecting read" "$(is "$(selecting "$dispatched" | wc -l)" "$n_selecting")"
  case $parity in
    0 | 1) check "filters $parity: some jobs selected and some left off" \
      "$(is "$([ -n "$(on_of "$dispatched")" ] && [ -n "$(off_of "$dispatched")" ] && echo both)" both)" ;;
    # Equality alone would pass an output that lost its filter on both events at once.
    all) check "filters all: every output that selects a job is true on the branch" "$(is "$(off_of "$dispatched")" "")" ;;
  esac
done

echo "case: every-job selects every job on a branch"
got=$(outputs --event workflow_dispatch --ref refs/heads/feature --input every-job=true --verdicts false)
check "every one of them is true" "$(is "$(off_of "$got")" "")"
check "ref_is_main stays false" "$(is "$(value "$got" ref_is_main)" false)"
got=$(outputs --event workflow_dispatch --ref refs/heads/feature --input every-job=false --verdicts false)
check "every-job=false selects by the diff" "$(is "$(on_of "$got")" "")"

echo "case: a dispatch of a ref that is no branch selects every job"
# Its checkout can hold a tag named main, which paths-filter would read as its base.
got=$(outputs --event workflow_dispatch --ref refs/tags/main --verdicts false)
check "a tag named main: every one of them is true" "$(is "$(off_of "$got")" "")"
check "  and ref_is_main is false" "$(is "$(value "$got" ref_is_main)" false)"
got=$(outputs --event workflow_dispatch --ref refs/tags/v1 --verdicts false)
check "another tag: every one of them is true" "$(is "$(off_of "$got")" "")"

# filter_step <argument>...: whether the filter step runs, and the base and ref it is handed.
filter_step() { "$script_dir/ci_router_outputs.py" "$ci" --filters "$filters" --filter-step "$@" | tr '\n' ' '; }
echo "case: the filter step diffs a dispatched branch's own commit with main, and reads a merge group's and a pull request's own event"
check "a dispatch of a branch: base main, ref its commit" \
  "$(is "$(filter_step --event workflow_dispatch --ref refs/heads/feature --sha "$old")" "runs=true base=main ref=$old ")"
check "a dispatch of main skips the step" "$(contains "$(filter_step --event workflow_dispatch --ref refs/heads/main)" '^runs=false ')"
check "every-job skips the step" \
  "$(contains "$(filter_step --event workflow_dispatch --ref refs/heads/feature --input every-job=true)" '^runs=false ')"
check "a dispatch of a tag skips the step" "$(contains "$(filter_step --event workflow_dispatch --ref refs/tags/main)" '^runs=false ')"
check "a merge group: neither input, so its own base_sha..head_sha" "$(is "$(filter_step --event merge_group)" "runs=true base= ref= ")"
check "a pull request: neither input, so the API's file list" "$(is "$(filter_step --event pull_request)" "runs=true base= ref= ")"

# generate_step_of <workflow> <argument>...: the CI_FILTERS_BASE the filter generation is handed,
# its env as the runner evaluates it when that step starts.
generate_step_of() {
  local workflow=$1
  shift
  "$script_dir/ci_router_outputs.py" "$workflow" --filters "$filters" --generate-step "$@"
}
merge_base=3333333333333333333333333333333333333333
# A pull request event's own base.sha, older than the merge commit's first parent once main has moved.
event_base=4444444444444444444444444444444444444444
echo "case: the filter generation measures a change against the commit it lands onto"
# The generation holds a change to how the filters are computed against the base tree's filters
# only when it is handed a base (.github/ci-notes/ci.md#the-changes-job-computes-its-filters-when-it-runs):
# a branch's dispatch handed none would select by its own engine alone, where its merge group does not.
# A pull request's base is the first parent of its merge commit, which the base step finds, and never
# the event's base.sha (.github/ci-notes/ci.md#the-commit-a-change-is-measured-against).
check "a dispatch of a branch: the merge base the base step finds" \
  "$(is "$(generate_step_of "$ci" --event workflow_dispatch --ref refs/heads/feature --base "$merge_base")" "filters-base=$merge_base")"
check "a merge group: its base" "$(is "$(generate_step_of "$ci" --event merge_group --base "$merge_base")" "filters-base=$merge_base")"
check "a pull request: the first parent the base step finds, not the event's own base" \
  "$(is "$(generate_step_of "$ci" --event pull_request --base "$merge_base" --pull-base "$event_base")" "filters-base=$merge_base")"
check "a dispatch of main: none, since it selects every job" \
  "$(is "$(generate_step_of "$ci" --event workflow_dispatch --ref refs/heads/main --base "$merge_base")" "filters-base=")"
check "every-job: none" \
  "$(is "$(generate_step_of "$ci" --event workflow_dispatch --ref refs/heads/feature --input every-job=true --base "$merge_base")" "filters-base=")"
check "a dispatch of a tag: none" \
  "$(is "$(generate_step_of "$ci" --event workflow_dispatch --ref refs/tags/main --base "$merge_base")" "filters-base=")"

# concurrency_of <workflow> <argument>...: the run's concurrency group and cancel-in-progress.
concurrency_of() {
  local workflow=$1
  shift
  "$script_dir/ci_router_outputs.py" "$workflow" --filters "$filters" --concurrency "$@" | tr '\n' ' '
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

echo "case: a scope step that reads no main fails the dispatch-of-main case"
broken=$(copy_with "$work/scope-no-main" ci.yaml '[ "$IS_MAIN" = true ] || ' '')
got=$(outputs_of "$broken" --event workflow_dispatch --ref refs/heads/main --verdicts false)
check "a dispatch of main then selects nothing" "$(is "$(on_of "$got")" "")"

echo "case: a scope step that reads no branch test fails the tag case"
broken=$(copy_with "$work/scope-no-branch" ci.yaml ' || [[ $GITHUB_REF != refs/heads/* ]]' '')
got=$(outputs_of "$broken" --event workflow_dispatch --ref refs/tags/main --verdicts false)
check "a dispatch of a tag named main then selects nothing" "$(is "$(on_of "$got")" "")"

echo "case: a scope step that writes every_job=false fails the branch case"
# 'false' is a non-empty string, which an expression reads as true, so every output would turn on.
broken=$(copy_with "$work/scope-writes-false" ci.yaml '&& v=true || v=' '&& v=true || v=false')
got=$(outputs_of "$broken" --event workflow_dispatch --ref refs/heads/feature --verdicts false)
check "a dispatch of a branch then selects every job" "$(is "$(off_of "$got")" "")"
got=$(outputs_of "$broken" --event pull_request --verdicts false)
check "  and so does a pull request with every filter saying no" "$(is "$(on_of "$got" | grep -c typecheck)" 1)"

echo "case: an output that lost its filter fails the all-filters case, which equality alone misses"
broken=$(copy_with "$work/output-no-filter" ci.yaml "typecheck: \${{ steps.scope.outputs.every_job && 'true' || steps.filter.outputs.typecheck }}" "typecheck: \${{ steps.scope.outputs.every_job }}")
dispatched=$(outputs_of "$broken" --event workflow_dispatch --ref refs/heads/feature --verdicts true)
check "every filter true: the branch then leaves exactly typecheck off" "$(is "$(off_of "$dispatched")" "typecheck ")"
check "  while a merge group leaves it off too, so the two still agree" \
  "$(is "$(selecting "$dispatched")" "$(selecting "$(outputs_of "$broken" --event merge_group --verdicts true)")")"

echo "case: an output still forced on every dispatch fails the branch case"
broken=$(copy_with "$work/output-every-dispatch" ci.yaml "typecheck: \${{ steps.scope.outputs.every_job" "typecheck: \${{ github.event_name == 'workflow_dispatch'")
got=$(outputs_of "$broken" --event workflow_dispatch --ref refs/heads/feature --verdicts false)
check "a dispatch of a branch then selects exactly typecheck" "$(is "$(on_of "$got")" "typecheck ")"
check "  which a merge group with the same verdicts does not" \
  "$(is "$(on_of "$(outputs_of "$broken" --event merge_group --verdicts false)")" "")"

echo "case: a cancel key without the dispatch arm fails the branch case"
broken=$(copy_with "$work/no-dispatch-key" ci.yaml "github.event_name == 'workflow_dispatch' && github.ref != 'refs/heads/main' && github.ref || " "")
check "a dispatch of a branch then keys on its commit" \
  "$(contains "$(concurrency_of "$broken" --event workflow_dispatch --ref refs/heads/feature --sha "$old")" "^group=ci-$old ")"

echo "case: a generation step handed no merge base fails the branch and pull-request cases"
# The generation step's own line: the restore of the base's cached filters names the same
# expression in its `if:` and its key.
broken=$(copy_with "$work/no-merge-base" ci.yaml \
  'CI_FILTERS_BASE: ${{ github.event.merge_group.base_sha || steps.base.outputs.sha }}' \
  'CI_FILTERS_BASE: ${{ github.event.merge_group.base_sha }}')
check "a dispatch of a branch then hands the generation no base" \
  "$(is "$(generate_step_of "$broken" --event workflow_dispatch --ref refs/heads/feature --base "$merge_base")" "filters-base=")"
check "  nor does a pull request" \
  "$(is "$(generate_step_of "$broken" --event pull_request --base "$merge_base" --pull-base "$event_base")" "filters-base=")"
check "  while a merge group still hands its own" \
  "$(is "$(generate_step_of "$broken" --event merge_group --base "$merge_base")" "filters-base=$merge_base")"

echo "case: a generation step that reads the pull request event's base fails the pull-request case"
broken=$(copy_with "$work/event-base" ci.yaml \
  'CI_FILTERS_BASE: ${{ github.event.merge_group.base_sha || steps.base.outputs.sha }}' \
  'CI_FILTERS_BASE: ${{ github.event.merge_group.base_sha || github.event.pull_request.base.sha || steps.base.outputs.sha }}')
check "a pull request then hands the generation the event's older base" \
  "$(is "$(generate_step_of "$broken" --event pull_request --base "$merge_base" --pull-base "$event_base")" "filters-base=$event_base")"

echo "case: a base step that runs on no pull request fails the pull-request case"
broken=$(copy_with "$work/base-no-pull-request" ci.yaml \
  "if: \${{ github.event_name == 'pull_request' || (github.event_name == 'workflow_dispatch' && !steps.scope.outputs.every_job) }}" \
  "if: \${{ github.event_name == 'workflow_dispatch' && !steps.scope.outputs.every_job }}")
check "a pull request then hands the generation no base" \
  "$(is "$(generate_step_of "$broken" --event pull_request --base "$merge_base" --pull-base "$event_base")" "filters-base=")"

echo "case: a base step that runs on every dispatch fails the dispatch-of-main case"
# edited-migrations-base.sh exits 2 on a dispatch of main that names no migration-base, which would
# fail the changes job of every bypass check.
broken=$(copy_with "$work/base-every-dispatch" ci.yaml "github.event_name == 'workflow_dispatch' && !steps.scope.outputs.every_job" \
  "github.event_name == 'workflow_dispatch'")
check "a dispatch of main then runs the base step" \
  "$(is "$(generate_step_of "$broken" --event workflow_dispatch --ref refs/heads/main --base "$merge_base")" "filters-base=$merge_base")"

echo "case: a generation step that starts before the base step is handed no merge base"
# The runner evaluates a step's env: when that step starts, over the outputs of the steps before it
# alone. In this copy the base step comes after the generation step, so on a dispatch of a branch
# the generation starts while steps.base.outputs.sha is still unwritten, and is handed no base,
# though the base step runs later. A reader that evaluated the env after every step had run would
# hand it the merge base here as in ci.yaml's own order; this case fails that reader.
base_step=$(cat <<'YAML'
      # why: .github/ci-notes/ci.md#the-commit-a-change-is-measured-against
      - name: Find the commit this run's change is measured against
        id: base
        if: ${{ github.event_name == 'pull_request' || (github.event_name == 'workflow_dispatch' && !steps.scope.outputs.every_job) }}
        timeout-minutes: 3
        run: |
          sha=$(.github/scripts/edited-migrations-base.sh)
          echo "sha=$sha" >> "$GITHUB_OUTPUT"
YAML
)
generate_run='        run: .github/scripts/ci-filters-generate.sh "$RUNNER_TEMP/ci-filters.yaml"'
broken=$(copy_with "$work/base-after-generation" ci.yaml "$base_step"$'\n' "" \
  "$generate_run"$'\n' "$generate_run"$'\n'"$base_step"$'\n')
check "ci.yaml's order: a dispatch of a branch hands the generation the merge base" \
  "$(is "$(generate_step_of "$ci" --event workflow_dispatch --ref refs/heads/feature --base "$merge_base")" "filters-base=$merge_base")"
check "the base step after the generation: a dispatch of a branch hands it no base" \
  "$(is "$(generate_step_of "$broken" --event workflow_dispatch --ref refs/heads/feature --base "$merge_base")" "filters-base=")"
check "  while a merge group, whose base no step writes, still hands its own" \
  "$(is "$(generate_step_of "$broken" --event merge_group --base "$merge_base")" "filters-base=$merge_base")"

echo "case: an expression this reader does not model is refused, never read as false"
mkdir -p "$work/.github/workflows"
printf 'f: []\n' > "$work/filters.yaml"
cat > "$work/.github/workflows/ci.yaml" <<'YAML'
jobs:
  changes:
    outputs:
      x: ${{ github.actor == 'someone' && 'true' || 'false' }}
YAML
set +e
err=$("$script_dir/ci_router_outputs.py" "$work/.github/workflows/ci.yaml" --filters "$work/filters.yaml" --event pull_request 2>&1 >/dev/null)
status=$?
set -e
check "exits 2" "$(is "$status" 2)"
check "names the output and the context" "$(contains "$err" 'output x: context github.actor is outside the modelled contexts')"

# steps_of <workflow> <argument>...: each step of the test job gated on what the changes job hands
# it, as `step=true|false`, sorted, for one event and set of verdicts (ci_router_outputs.py
# --steps), the filter names read from the computed filters; steps <argument>... reads this
# repository's own ci.yaml and ci-job-test.yaml.
steps_of() {
  local workflow=$1
  shift
  "$script_dir/ci_router_outputs.py" "$workflow" --filters "$filters" --steps test "$@" | sort
}
steps() { steps_of "$repo/.github/workflows/ci.yaml" "$@"; }
# running <steps text>: the steps that run, one per line.
running() { sed -n 's/=true$//p' <<<"$1"; }

# The test job's harness steps read their gates out of one output, the paths filter's own list
# of the filters that matched (.github/ci-notes/ci.md#one-list-for-the-test-jobs-step-gates). The
# cases below read each step's `if:` through the `with:` that ci.yaml's calling job passes, as
# GitHub would, so a list that skipped a matched harness, or ran an unmatched one, fails here by
# step name.
echo "case: the test job's step gates are read out of the one list"
none=$(steps --event pull_request --verdicts false --verdict test=true)
gated=$(grep -c . <<<"$none" || true)
# Each step of the test job whose `if:` names the list in any spelling, once per filter its
# quoted `contains` terms name (the step that fetches the computed filters names two), read from
# ci-job-test.yaml's text and not by the evaluator under test, so a step the evaluator passed over
# fails the count below by name.
gates_of_text=$(python3 -c '
import re, sys, yaml
jobs = yaml.safe_load(open(sys.argv[1]))["jobs"]
for step in jobs["test"].get("steps") or []:
    condition = str(step.get("if", ""))
    if re.search(r"matched[_-]filters", condition, re.I):
        for name in re.findall(r"\x27\"([A-Za-z0-9_-]+)\"\x27", condition) or ["?"]:
            print(step["name"] + "\t" + name)
' "$repo/.github/workflows/ci-job-test.yaml")
gated_of_text=$(cut -f1 <<<"$gates_of_text" | sort -u | grep -c . || true)
check "it finds every step whose if: names the list ($gated_of_text)" "$(is "$gated" "$gated_of_text")"
check "each of them names a filter" "$(is "$(grep -c $'\t?$' <<<"$gates_of_text" || true)" 0)"
check "with no step's filter matched, none of them runs" "$(is "$(running "$none")" "")"

echo "case: each filter, matched alone, runs exactly the steps it gates"
# Every filter of the list, one at a time: the steps that run must be the steps whose `if:` names
# that filter, so a step that runs on no match (a `!` before its `contains`) or never (an `&&
# false` after it) fails here by name.
wrong=""
while IFS= read -r filter; do
  want=$(awk -F'\t' -v f="$filter" '$2 == f { print $1 }' <<<"$gates_of_text" | sort)
  got=$(running "$(steps --event pull_request --verdicts false --verdict test=true --verdict "$filter=true")")
  [ "$got" = "$want" ] || wrong+="$filter: ran [${got//$'\n'/, }] where [${want//$'\n'/, }]; "
done < <(cut -f2 <<<"$gates_of_text" | sort -u)
check "all $(cut -f2 <<<"$gates_of_text" | sort -u | grep -c .) filters run their own steps and no other" "$(is "$wrong" "")"

echo "case: the step that fetches the computed filters runs for either harness that reads them"
got=$(steps --event pull_request --verdicts false --verdict test=true --verdict check_ci_filters_test=true)
check "check_ci_filters_test matched: the fetch runs" \
  "$(is "$(running "$got" | grep -cx "Fetch the filters the changes job selected this run's jobs by" || true)" 1)"
got=$(steps --event pull_request --verdicts false --verdict test=true --verdict ci_router_outputs_test=true)
check "ci_router_outputs_test matched: the fetch runs" \
  "$(is "$(running "$got" | grep -cx "Fetch the filters the changes job selected this run's jobs by" || true)" 1)"

echo "case: a harness whose filter matched runs, and one whose filter did not is skipped"
got=$(steps --event pull_request --verdicts false --verdict test=true --verdict release_push_test=true)
check "release_push_test matched: its step alone runs" "$(is "$(running "$got")" "Test release scripts")"
got=$(steps --event pull_request --verdicts false --verdict test=true --verdict ci_tools_test=true)
check "ci_tools_test matched: its step alone runs" "$(is "$(running "$got")" "Test the tool fetch helper")"
got=$(steps --event pull_request --verdicts false --verdict test=true --verdict mirror_ci_tools_test=true)
check "mirror_ci_tools_test matched: its step alone runs, not ci_tools_test's, whose name its own holds" \
  "$(is "$(running "$got")" "Test the tool copy filler")"

echo "case: every gated step runs on a run that selects every job and when changes did not succeed"
got=$(steps --event workflow_dispatch --ref refs/heads/main --verdicts false)
check "a dispatch of main runs all $gated" "$(is "$(running "$got" | grep -c . || true)" "$gated")"
got=$(steps --event workflow_dispatch --ref refs/heads/feature --input every-job=true --verdicts false)
check "a dispatch of a branch given every-job runs all $gated" "$(is "$(running "$got" | grep -c . || true)" "$gated")"
got=$(steps --event pull_request --verdicts false --changes-result failure)
check "a run whose changes failed runs all $gated" "$(is "$(running "$got" | grep -c . || true)" "$gated")"

# A dispatch of a branch runs its filter step over its diff with its merge base with main
# (.github/ci-notes/ci.md#what-a-dispatched-run-of-a-branch-selects-and-when-to-ask-for-every-job),
# so the list it hands on is that step's, as a merge group's is; every run that selects every job
# skips the step and hands on `all`.
echo "case: a dispatch of a branch hands on the filter step's list; a dispatch of main, a tag and every-job hand on all"
two=(--verdicts false --verdict test=true --verdict release_push_test=true --verdict ci_tools_test=true)
got=$(outputs --event workflow_dispatch --ref refs/heads/feature "${two[@]}")
queued=$(outputs --event merge_group "${two[@]}")
check "a dispatch of a branch: the list a merge group with the same verdicts hands on" \
  "$(is "$(value "$got" matched_filters)" "$(value "$queued" matched_filters)")"
check "  which names the filters that matched, test, release_push_test and ci_tools_test" \
  "$(is "$(python3 -c 'import json, sys; print(" ".join(sorted(json.loads(sys.argv[1]))))' "$(value "$got" matched_filters)")" \
    "ci_tools_test release_push_test test")"
got=$(steps --event workflow_dispatch --ref refs/heads/feature --verdicts false --verdict test=true --verdict release_push_test=true)
check "  and the test job runs that filter's step alone" "$(is "$(running "$got")" "Test release scripts")"
check "a dispatch of main: all" \
  "$(is "$(value "$(outputs --event workflow_dispatch --ref refs/heads/main "${two[@]}")" matched_filters)" all)"
check "a dispatch of a branch given every-job: all" \
  "$(is "$(value "$(outputs --event workflow_dispatch --ref refs/heads/feature --input every-job=true "${two[@]}")" matched_filters)" all)"
check "a dispatch of a tag: all" \
  "$(is "$(value "$(outputs --event workflow_dispatch --ref refs/tags/v1 "${two[@]}")" matched_filters)" all)"

echo "case: a step whose list read drops its quotes runs on a filter whose name holds its own"
broken=$(copy_with "$work/no-quotes" ci-job-test.yaml "contains(inputs.matched-filters, '\"ci_tools_test\"')" \
  "contains(inputs.matched-filters, 'ci_tools_test')")
got=$(steps_of "$broken" --event pull_request --verdicts false --verdict test=true --verdict mirror_ci_tools_test=true)
check "ci_tools_test's step runs on mirror_ci_tools_test alone" \
  "$(is "$(running "$got" | grep -cx 'Test the tool fetch helper' || true)" 1)"

echo "case: a step whose list read names another filter is skipped when its own matched"
broken=$(copy_with "$work/misnamed" ci-job-test.yaml "'\"release_push_test\"'" "'\"release_push_tset\"'")
got=$(steps_of "$broken" --event pull_request --verdicts false --verdict test=true --verdict release_push_test=true)
check "Test release scripts stays skipped" "$(is "$(running "$got")" "")"

echo "case: tooling-checks, the file's other caller, runs its guides step on guide_layout alone"
# The file holds one job per caller, each `if: inputs.job == '<id>'`: each caller is read for its
# own job's steps, and the test job's harness gates are none of tooling-checks'.
tooling=$("$script_dir/ci_router_outputs.py" "$repo/.github/workflows/ci.yaml" --filters "$filters" --steps tooling-checks \
  --event pull_request --verdicts false --verdict tooling_checks=true | sort)
check "it reads one gated step" "$(is "$(grep -c . <<<"$tooling")" 1)"
check "skipped when guide_layout did not match" "$(is "$(running "$tooling")" "")"
tooling=$("$script_dir/ci_router_outputs.py" "$repo/.github/workflows/ci.yaml" --filters "$filters" --steps tooling-checks \
  --event pull_request --verdicts false --verdict tooling_checks=true --verdict guide_layout=true | sort)
check "and run when it did" "$(is "$(running "$tooling")" "Check the guides' semantic line breaks")"
got=$(steps --event pull_request --verdicts false --verdict test=true --verdict guide_layout=true)
check "while the test job runs none of its harnesses on guide_layout" "$(is "$(running "$got")" "")"

echo "case: a list that is not all on a run that selects every job skips every step it gates there"
broken=$(copy_with "$work/no-all" ci.yaml "steps.scope.outputs.every_job && 'all'" "steps.scope.outputs.every_job && '[]'")
got=$(steps_of "$broken" --event workflow_dispatch --ref refs/heads/main --verdicts false)
check "a dispatch of main then runs none of them" "$(is "$(running "$got")" "")"

echo "case: a list still all on every dispatch fails the branch case"
# The term before the every-job one: a dispatch of a branch would run every harness its diff
# does not reach, where the merge group runs only those it reaches.
broken=$(copy_with "$work/list-every-dispatch" ci.yaml "matched_filters: \${{ steps.scope.outputs.every_job && 'all'" \
  "matched_filters: \${{ github.event_name == 'workflow_dispatch' && 'all'")
got=$(outputs_of "$broken" --event workflow_dispatch --ref refs/heads/feature "${two[@]}")
check "a dispatch of a branch then hands on all" "$(is "$(value "$got" matched_filters)" all)"
got=$(steps_of "$broken" --event workflow_dispatch --ref refs/heads/feature --verdicts false --verdict test=true --verdict release_push_test=true)
check "  and runs all $gated gated steps, not its filter's alone" "$(is "$(running "$got" | grep -c . || true)" "$gated")"

# When the filters cannot be computed, ci-filters-generate.sh hands the paths filter a fallback
# file whose every filter is '**', named from the changes job's outputs. No step's filter read out
# of the list is among them, so the list must be `all` there, which the script's every_filter output
# makes it. The fallback here is the real script's, over this repository's ci.yaml, in a tree whose
# check-ci-filters.sh fails on purpose, as one whose go cannot run does.
fallback_tree="$work/fallback-tree"
mkdir -p "$fallback_tree/.github/scripts" "$fallback_tree/.github/workflows"
cp "$script_dir/ci-filters-generate.sh" "$fallback_tree/.github/scripts/"
printf '#!/bin/sh\necho "check-ci-filters.sh: failing on purpose" >&2\nexit 1\n' > "$fallback_tree/.github/scripts/check-ci-filters.sh"
chmod +x "$fallback_tree/.github/scripts/check-ci-filters.sh"
cp "$repo/.github/workflows/ci.yaml" "$fallback_tree/.github/workflows/ci.yaml"
: > "$work/fallback.out"
GITHUB_OUTPUT="$work/fallback.out" CI_FILTERS_BASE='' \
  "$fallback_tree/.github/scripts/ci-filters-generate.sh" "$work/fallback.yaml" >/dev/null 2>&1 || true
# fallen <workflow> <argument>...: steps_of over the fallback file and the outputs the script wrote.
fallen() {
  local workflow=$1
  shift
  "$script_dir/ci_router_outputs.py" "$workflow" --filters "$work/fallback.yaml" \
    --step-outputs generate="$work/fallback.out" "$@" | sort
}

echo "case: on a merge group whose filters fell open, every step gated on the list runs"
check "the script wrote a fail-open file" "$(contains "$(head -1 "$work/fallback.yaml")" '^# FAIL-OPEN: ')"
check "it holds no filter named changes, under which the paths filter writes no list" \
  "$(is "$(grep -c '^changes:$' "$work/fallback.yaml" || true)" 0)"
check "and its step output says so" "$(is "$(cat "$work/fallback.out")" "every_filter=true")"
got=$(fallen "$repo/.github/workflows/ci.yaml" --event merge_group)
check "the list is all" "$(is "$(value "$got" matched_filters)" all)"
check "the test job is selected" "$(is "$(value "$got" test)" true)"
got=$(fallen "$repo/.github/workflows/ci.yaml" --event merge_group --steps test)
check "the test job runs all $gated of its gated steps" "$(is "$(running "$got" | grep -c . || true)" "$gated")"
got=$(fallen "$repo/.github/workflows/ci.yaml" --event merge_group --steps tooling-checks)
check "and tooling-checks its guides step" "$(is "$(running "$got")" "Check the guides' semantic line breaks")"
got=$(fallen "$repo/.github/workflows/ci.yaml" --event workflow_dispatch --ref refs/heads/feature --steps test)
check "and so does a dispatch of a branch whose filters fell open" "$(is "$(running "$got" | grep -c . || true)" "$gated")"

echo "case: a list that reads no fall-open term skips every step it gates on a fallback file"
broken=$(copy_with "$work/no-fall-open" ci.yaml " || steps.generate.outputs.every_filter == 'true' && 'all'" "")
got=$(fallen "$broken" --event merge_group --steps test)
check "none of them runs" "$(is "$(running "$got")" "")"

summary "ci.yaml's changes outputs turn on what full-ci, the lane labels and a dispatch's ref and diff name, its filter generation is measured against each diff's base, its cancel key cancels only a branch's older dispatch, and the test job's steps run on what their filters name"
