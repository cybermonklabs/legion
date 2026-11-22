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
# held_of <workflow>: the outputs a merge group turns on and an unlabelled pull request holds
# false, every filter saying yes, one per line. No label term decides it.
held_of() {
  comm -12 <(grep '=false$' <<<"$(outputs_of "$1" --event pull_request)" | cut -d= -f1) \
    <(grep '=true$' <<<"$(outputs_of "$1" --event merge_group)" | cut -d= -f1)
}
# off_under <workflow> <label>...: each output of held_of that those labels leave off.
off_under() {
  local workflow=$1 name got label
  local args=()
  shift
  for label in "$@"; do args+=(--label "$label"); done
  got=$(outputs_of "$workflow" --event pull_request "${args[@]}")
  while read -r name; do
    [ "$(value "$got" "$name")" = true ] || echo "$name"
  done < <(held_of "$workflow")
}

baseline=$(outputs --event pull_request)
mapfile -t held < <(held_of "$repo/.github/workflows/ci.yaml")

echo "case: some outputs a merge group turns on are held false on an unlabelled pull request"
check "it holds at least one back (${#held[@]}: ${held[*]})" "$(is "$([ "${#held[@]}" -gt 0 ] && echo yes)" yes)"

echo "case: full-ci turns every held-back output on"
check "no held-back output stays off under full-ci" \
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

# broken_copy <dir> <output> <literal> <replacement>: a copy of this repository's ci.yaml and
# filters under <dir> with that literal changed on that output's line alone.
broken_copy() {
  mkdir -p "$1/.github/workflows"
  cp "$repo/.github/ci-filters.yaml" "$1/.github/ci-filters.yaml"
  sed -E "/^ +$2: /s/'$3'/'$4'/" "$repo/.github/workflows/ci.yaml" > "$1/.github/workflows/ci.yaml"
  cmp -s "$repo/.github/workflows/ci.yaml" "$1/.github/workflows/ci.yaml" && echo "broken_copy: $2 has no '$3'" >&2
  echo "$1/.github/workflows/ci.yaml"
}

echo "case: an output whose full-ci literal is broken stays in the set and fails the full-ci case"
broken=$(broken_copy "$work/no-full-ci" envoy_go_race_api full-ci full-cj)
check "the copy differs from ci.yaml" "$(is "$(cmp -s "$broken" "$repo/.github/workflows/ci.yaml" && echo same)" "")"
check "envoy_go_race_api is still held back" "$(contains "$(held_of "$broken")" '^envoy_go_race_api$')"
check "the set keeps all ${#held[@]}" "$(is "$(held_of "$broken" | wc -l)" "${#held[@]}")"
check "full-ci leaves exactly envoy_go_race_api off" "$(is "$(off_under "$broken" full-ci | tr '\n' ' ')" "envoy_go_race_api ")"

echo "case: an output whose lane literal is broken fails its lane row"
broken=$(broken_copy "$work/no-lane" envoy_smoke lane:envoy_smoke lane:envoy_smokx)
check "envoy_smoke is still held back" "$(contains "$(held_of "$broken")" '^envoy_smoke$')"
check "lane:envoy_smoke leaves it off" "$(is "$(off_under "$broken" lane:envoy_smoke | grep -c '^envoy_smoke$')" 1)"
check "full-ci still turns it on" "$(is "$(off_under "$broken" full-ci | tr '\n' ' ')" "")"

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
check "names the output and the context" "$(contains "$err" 'output x: context github.actor is not modelled')"

summary "ci.yaml's changes outputs turn on what full-ci and the lane labels name, and nothing else"
