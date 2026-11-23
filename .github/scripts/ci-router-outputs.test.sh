#!/usr/bin/env bash
# Tests for the label terms of ci.yaml's `changes` outputs, through ci_router_outputs.py, which
# evaluates this repository's own ci.yaml as GitHub would: on a pull request the slow suites'
# outputs are false whatever their paths say, `full-ci` turns every one of them on, a
# `lane:<output>` label turns on that one output and no other, still only where its paths
# matched, and a label that names no such output turns on nothing
# (.github/ci-notes/ci.md#a-lane-label-turns-on-one-slow-suite-on-a-pull-request). The jobs that
# run in part read the same labels through their `lane` input, and the ci-ok job's PARTIAL must
# name exactly the jobs whose lane reads `pull-request` under every label, or ci-ok calls a job
# that ran in full a partial one, or the reverse
# (.github/ci-notes/ci.md#what-the-lane-input-reads).
#
# The held-back set is read, not listed here, and read apart from the label terms it tests: the
# outputs a merge group turns on and an unlabelled pull request holds false, every paths filter
# saying yes in both. A read that finds none fails, since every case below would then pass over
# nothing. Two copies of ci.yaml, each with one label literal broken, show that a broken term
# fails its case rather than leaving the set.
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

echo "case: exactly the eleven gated outputs read the pull request's labels"
# A count and the names, so an output whose label term is deleted outright fails here by name
# rather than leaving the set. A twelfth gated suite adds its name to this list.
gated='daemon_go_tests dispatch_memory dispatch_multiprocess dispatch_two_builds envoy_go envoy_go_checks envoy_go_claim_model envoy_go_race envoy_go_race_api envoy_go_race_docs envoy_smoke'
check "the reader finds ${#held[@]}: ${held[*]}" "$(is "${#held[@]}" 11)"
check "and they are the eleven" "$(is "$(printf '%s\n' "${held[@]}" | sort | tr '\n' ' ')" "$gated ")"

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

# lanes_of <workflow> <argument>...: the `lane` input of each job that passes one, as `job=lane`,
# then `PARTIAL=<the ci-ok job's list>`, for one event and set of labels.
lanes_of() {
  local workflow=$1
  shift
  "$script_dir/ci_router_outputs.py" "$workflow" --lanes "$@"
}
# short_of <lanes text>: the jobs on the pull-request lane, sorted, space-separated.
short_of() { sed -n 's/=pull-request$//p' <<<"$1" | sort | tr '\n' ' '; }
# partial_of <lanes text>: the jobs PARTIAL names, sorted, space-separated.
partial_of() { sed -n 's/^PARTIAL=//p' <<<"$1" | jq -r '.[]' | sort | tr '\n' ' '; }
ci="$repo/.github/workflows/ci.yaml"
part_run='dispatch-browser envoy-infra-go pi-legion test '

echo "case: an unlabelled pull request runs the four in part, and PARTIAL names the four"
got=$(lanes_of "$ci" --event pull_request)
check "the four are on the pull-request lane" "$(is "$(short_of "$got")" "$part_run")"
check "PARTIAL names the same four" "$(is "$(partial_of "$got")" "$part_run")"

echo "case: a merge group, a dispatched run and full-ci run every job in full, and PARTIAL is empty"
for args in "--event merge_group" "--event workflow_dispatch" "--event pull_request --label full-ci"; do
  # shellcheck disable=SC2086 # each word of args is one argument
  got=$(lanes_of "$ci" $args)
  check "$args: no job on the pull-request lane" "$(is "$(short_of "$got")" "")"
  check "$args: PARTIAL is empty" "$(is "$(partial_of "$got")" "")"
done

echo "case: a job's own lane label runs that job in full, and PARTIAL drops it alone"
for pair in test:test pi_legion:pi-legion envoy_infra_go:envoy-infra-go; do
  output=${pair%%:*} job=${pair#*:}
  got=$(lanes_of "$ci" --event pull_request --label "lane:$output")
  want=$(tr ' ' '\n' <<<"$part_run" | grep -vx -- "$job" | grep . | tr '\n' ' ')
  check "lane:$output runs $job in full" "$(is "$(short_of "$got")" "$want")"
  check "lane:$output: PARTIAL names the other three" "$(is "$(partial_of "$got")" "$want")"
  check "lane:$output leaves every changes output as an unlabelled pull request has it" \
    "$(is "$(outputs --event pull_request --label "lane:$output")" "$baseline")"
done

echo "case: a lane label of a held-back output leaves every lane as an unlabelled pull request has it"
got=$(lanes_of "$ci" --event pull_request --label lane:daemon_go_tests --label lane:envoy-infra-go)
check "the four stay on the pull-request lane" "$(is "$(short_of "$got")" "$part_run")"
check "PARTIAL still names the four" "$(is "$(partial_of "$got")" "$part_run")"

echo "case: a lane whose label literal is broken disagrees with PARTIAL, which this harness reads"
broken=$(broken_copy "$work/no-lane-input" lane "'lane:pi_legion'" "'lane:pi_legiox'")
check "the copy differs from ci.yaml" "$(is "$(cmp -s "$broken" "$ci" && echo same)" "")"
got=$(lanes_of "$broken" --event pull_request --label lane:pi_legion)
check "pi-legion stays on the pull-request lane" "$(is "$(short_of "$got" | grep -c pi-legion)" 1)"
check "while PARTIAL drops it, so the two disagree" \
  "$(is "$([ "$(short_of "$got")" = "$(partial_of "$got")" ] && echo agree || echo disagree)" disagree)"

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

summary "ci.yaml's changes outputs turn on what full-ci and the lane labels name, and nothing else"
