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
# It also holds the test job's step gates, which read one output, the paths filter's own list of
# the filters that matched: a step whose filter matched runs, one whose filter did not is skipped,
# even where another filter's name holds its own, and every one runs on a dispatched run and when
# changes did not succeed. Copies with one read misspelt, unquoted or undispatched show each case
# fails on what it guards.
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

echo "case: exactly the fifteen gated outputs read the pull request's labels"
# A count and the names, so an output whose label term is deleted outright fails here by name
# rather than leaving the set. A sixteenth gated suite adds its name to this list.
gated='daemon_go_tests dispatch_browser dispatch_memory dispatch_multiprocess dispatch_two_builds envoy_go envoy_go_checks envoy_go_claim_model envoy_go_race envoy_go_race_api envoy_go_race_docs envoy_infra_go envoy_smoke pi_legion test'
check "the reader finds ${#held[@]}: ${held[*]}" "$(is "${#held[@]}" 15)"
check "and they are the fifteen" "$(is "$(printf '%s\n' "${held[@]}" | sort | tr '\n' ' ')" "$gated ")"

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

# steps_of <workflow> <argument>...: each step of the test job gated on what the changes job hands
# it, as `step=true|false`, sorted, for one event and set of verdicts (ci_router_outputs.py
# --steps); steps <argument>... reads this repository's own ci.yaml and ci-job-test.yaml.
steps_of() {
  local workflow=$1
  shift
  "$script_dir/ci_router_outputs.py" "$workflow" --steps test "$@" | sort
}
steps() { steps_of "$repo/.github/workflows/ci.yaml" "$@"; }
# running <steps text>: the steps that run, one per line.
running() { sed -n 's/=true$//p' <<<"$1"; }
# list_copy <dir> <workflow file> <text> <replacement>: a copy of this repository's ci.yaml,
# ci-job-test.yaml and filters under <dir> with <text> changed to <replacement> once in that one
# workflow file (ci.yaml or ci-job-test.yaml), failing loudly if <text> is not in it.
list_copy() {
  mkdir -p "$1/.github/workflows"
  cp "$repo/.github/ci-filters.yaml" "$1/.github/ci-filters.yaml"
  cp "$repo/.github/workflows/ci.yaml" "$repo/.github/workflows/ci-job-test.yaml" "$1/.github/workflows/"
  local file="$1/.github/workflows/$2" text
  text=$(<"$file")
  if [[ $text != *"$3"* ]]; then
    echo "list_copy: $2 has no $3" >&2
  fi
  printf '%s\n' "${text/"$3"/"$4"}" > "$file"
  echo "$1/.github/workflows/ci.yaml"
}

# The test job's harness steps read their gates out of one output, the paths filter's own list
# of the filters that matched (.github/ci-notes/ci.md#one-list-for-the-test-jobs-step-gates). The
# cases below read each step's `if:` through ci.yaml's `with:` as GitHub would, so a list that
# skipped a matched harness, or ran an unmatched one, fails here by step name.
echo "case: the test job's step gates are read out of the one list"
none=$(steps --event pull_request --verdicts false --verdict test=true)
gated=$(grep -c . <<<"$none" || true)
# Each step of the test job whose `if:` names the list in any spelling, with the filter its quoted
# `contains` names, read from ci-job-test.yaml's text and not by the evaluator under test, so a
# step the evaluator passed over fails the count below by name.
gates_of_text=$(python3 -c '
import re, sys, yaml
jobs = yaml.safe_load(open(sys.argv[1]))["jobs"]
for step in jobs["test"].get("steps") or []:
    condition = str(step.get("if", ""))
    if re.search(r"matched[_-]filters", condition, re.I):
        names = re.findall(r"\x27\"([A-Za-z0-9_-]+)\"\x27", condition)
        print(step["name"] + "\t" + (names[0] if len(names) == 1 else "?"))
' "$repo/.github/workflows/ci-job-test.yaml")
check "it finds every step whose if: names the list ($(grep -c . <<<"$gates_of_text"))" \
  "$(is "$gated" "$(grep -c . <<<"$gates_of_text")")"
check "each of them names one filter" "$(is "$(grep -c $'\t?$' <<<"$gates_of_text" || true)" 0)"
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

echo "case: a harness whose filter matched runs, and one whose filter did not is skipped"
got=$(steps --event pull_request --verdicts false --verdict test=true --verdict release_push_test=true)
check "release_push_test matched: its step alone runs" "$(is "$(running "$got")" "Test release scripts")"
got=$(steps --event pull_request --verdicts false --verdict test=true --verdict ci_tools_test=true)
check "ci_tools_test matched: its step alone runs" "$(is "$(running "$got")" "Test the tool fetch helper")"
got=$(steps --event pull_request --verdicts false --verdict test=true --verdict mirror_ci_tools_test=true)
check "mirror_ci_tools_test matched: its step alone runs, not ci_tools_test's, whose name its own holds" \
  "$(is "$(running "$got")" "Test the tool copy filler")"

echo "case: every gated step runs on a dispatched run and when changes did not succeed"
got=$(steps --event workflow_dispatch --verdicts false)
check "a dispatched run runs all $gated" "$(is "$(running "$got" | grep -c . || true)" "$gated")"
got=$(steps --event pull_request --verdicts false --changes-result failure)
check "a run whose changes failed runs all $gated" "$(is "$(running "$got" | grep -c . || true)" "$gated")"

echo "case: a step whose list read drops its quotes runs on a filter whose name holds its own"
broken=$(list_copy "$work/no-quotes" ci-job-test.yaml "contains(inputs.matched-filters, '\"ci_tools_test\"')" \
  "contains(inputs.matched-filters, 'ci_tools_test')")
got=$(steps_of "$broken" --event pull_request --verdicts false --verdict test=true --verdict mirror_ci_tools_test=true)
check "ci_tools_test's step runs on mirror_ci_tools_test alone" \
  "$(is "$(running "$got" | grep -cx 'Test the tool fetch helper' || true)" 1)"

echo "case: a step whose list read names another filter is skipped when its own matched"
broken=$(list_copy "$work/misnamed" ci-job-test.yaml "'\"release_push_test\"'" "'\"release_push_tset\"'")
got=$(steps_of "$broken" --event pull_request --verdicts false --verdict test=true --verdict release_push_test=true)
check "Test release scripts stays skipped" "$(is "$(running "$got")" "")"

echo "case: tooling-checks, the file's other caller, runs its guides step on guide_layout alone"
# The file holds one job per caller, each `if: inputs.job == '<id>'`: each caller is read for its
# own job's steps, and the test job's harness gates are none of tooling-checks'.
tooling=$("$script_dir/ci_router_outputs.py" "$repo/.github/workflows/ci.yaml" --steps tooling-checks \
  --event pull_request --verdicts false --verdict tooling_checks=true | sort)
check "it reads one gated step" "$(is "$(grep -c . <<<"$tooling")" 1)"
check "skipped when guide_layout did not match" "$(is "$(running "$tooling")" "")"
tooling=$("$script_dir/ci_router_outputs.py" "$repo/.github/workflows/ci.yaml" --steps tooling-checks \
  --event pull_request --verdicts false --verdict tooling_checks=true --verdict guide_layout=true | sort)
check "and run when it did" "$(is "$(running "$tooling")" "Check the guides' semantic line breaks")"
got=$(steps --event pull_request --verdicts false --verdict test=true --verdict guide_layout=true)
check "while the test job runs none of its harnesses on guide_layout" "$(is "$(running "$got")" "")"

echo "case: a list that is not all on a dispatched run skips every step it gates there"
broken=$(list_copy "$work/no-all" ci.yaml "'workflow_dispatch' && 'all'" "'workflow_dispatch' && '[]'")
got=$(steps_of "$broken" --event workflow_dispatch --verdicts false)
check "none of them runs" "$(is "$(running "$got")" "")"

summary "ci.yaml's changes outputs turn on what full-ci and the lane labels name, and the test job's steps what their filters name"
