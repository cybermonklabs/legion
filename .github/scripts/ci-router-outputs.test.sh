#!/usr/bin/env bash
# Tests for the label terms of ci.yaml's `changes` outputs, through ci_router_outputs.py, which
# evaluates this repository's own ci.yaml as GitHub would: on a pull request the slow suites'
# outputs are false whatever their paths say, `full-ci` turns every one of them on, a
# `lane:<output>` label turns on that one output and no other, still only where its paths
# matched, and a label that names no such output turns on nothing
# (.github/ci-notes/ci.md#a-lane-label-turns-on-one-slow-suite-on-a-pull-request).
#
# The held-back set is read, not listed here: the outputs an unlabelled pull request holds false
# while every paths filter says yes. A read that finds none fails, since every case below would
# then pass over nothing.
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

# outputs <argument>...: every output as `name=value`, sorted, for one event and set of labels.
outputs() {
  "$script_dir/ci_router_outputs.py" "$repo/.github/workflows/ci.yaml" "$@" | sort
}
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

baseline=$(outputs --event pull_request)
everything=$(outputs --event pull_request --verdicts true --label full-ci)
mapfile -t held < <(comm -13 <(grep '=true$' <<<"$baseline" | cut -d= -f1) <(grep '=true$' <<<"$everything" | cut -d= -f1))

echo "case: an unlabelled pull request holds some outputs false although every paths filter says yes"
check "it holds at least one back (${#held[@]}: ${held[*]})" "$(is "$([ "${#held[@]}" -gt 0 ] && echo yes)" yes)"

echo "case: full-ci turns every held-back output on"
for name in "${held[@]}"; do
  check "$name is true" "$(is "$(value "$everything" "$name")" true)"
done

echo "case: a merge group holds nothing back"
group=$(outputs --event merge_group)
for name in "${held[@]}"; do
  check "$name is true" "$(is "$(value "$group" "$name")" true)"
done

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
