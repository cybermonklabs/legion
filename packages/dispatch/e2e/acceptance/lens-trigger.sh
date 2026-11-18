#!/usr/bin/env bash
# Drives the chain of triggered lenses end to end against a running server (dispatch://INFRA-1950's
# c-lens-trigger, dispatch://INFRA-1950/spec#b-r-lens-trigger). As the person DEV_SIGNIN_LOGIN
# signs in, it invokes a trade-off analysis on a spec and then a pre-mortem the trade-off analysis
# asked for, with a triggered_by tuple to it, and reads both ends in one GET /api/v1/lenses?on= the
# spec: the pre-mortem answers the trade-off analysis as the lens that triggered it, the trade-off
# analysis answers the pre-mortem among the lenses it triggered, and the same read answers the
# chain whole in invoked order. It adds a third lens the pre-mortem triggered and reads the chain of
# three, retracts the pre-mortem's triggered_by tuple through the tuple route and reads that
# neither end answers the other, and is refused a lens triggered by itself. Then it holds the walk
# to its two rules apart: a cycle of two lenses is read within 30 s with each lens listed once, an
# acyclic chain of 60 lenses is listed to 50 with the lens where the walk stopped named, and a
# chain of exactly 50 is listed whole with no stop named. Each of those three stands on a spec of
# its own, so no other lens joins its chain.
#
# Inputs: DISPATCH_URL (the server), DISPATCH_TOKEN or DISPATCH_TOKEN_FILE (an agent bearer, read
# here and never printed), PROJECT (an existing project key) and DEV_SIGNIN_LOGIN (the login the
# server's dev sign-in route, /auth/_dev/signin, signs in: `alice` on `bun run dispatch:local`).
# Every lens and tuple is that person's write, made with the cookie the dev sign-in sets and the
# server's origin, so that person is each lens's author; the agent bearer writes the issues and
# closes them when the run ends, also when a check fails. Each unmet input is exit 2.
#
# The first line is `PASS: lens-trigger.sh the catalogue holds its ten kinds`, read before any lens
# is written. Each passing check prints `PASS: <section> <what>`; the first failing one prints
# `FAIL: <section> <what>: <observed>` and the script exits 1. The last line is
# `PASS: lens-trigger.sh complete`.
#
# Every check is `cmd || fail …`, never part of an `&&` list: under `set -e` a failing command
# anywhere but last in such a list does not stop the script.

# shellcheck disable=SC2016 # the single-quoted jq filters name jq variables, not shell ones
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DISPATCH_URL=http://127.0.0.1:8777 DISPATCH_TOKEN=e2e-token PROJECT=CORE \
         DEV_SIGNIN_LOGIN=alice lens-trigger.sh
       (DISPATCH_TOKEN_FILE=<file holding the bearer> in place of DISPATCH_TOKEN; the server must
       mount its dev sign-in route, as `bun run dispatch:local` does)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
require_env DISPATCH_URL PROJECT DEV_SIGNIN_LOGIN
require_commands curl jq
read_bearer

readonly driver=lens-trigger.sh
# shellcheck disable=SC2031 # the DISPATCH_URL lib.sh's `cli` exports is its own; this one is the caller's
readonly base="${DISPATCH_URL%/}"
# shellcheck disable=SC2153 # PROJECT is the caller's input, checked by name above
readonly project="$PROJECT"
readonly login="$DEV_SIGNIN_LOGIN"
stamp="$(date +%s)-$$"
readonly stamp
readonly session="lens-trigger-$stamp"
# The bound of one chain that R8 states (dispatch://INFRA-1950/spec#b-r-lens-trigger), and the
# length of the chain read past it, ten more.
readonly bound=50
readonly past_bound=60

work_dir
printf 'Authorization: Bearer %s\n' "$token" >"$work/auth"

issues=()
cleanup() {
  local code=$?
  local issue
  for issue in "${issues[@]}"; do
    close_issue "$issue"
  done
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# lenses_on SECTION WHAT ADDRESS: the person's read of GET /api/v1/lenses?on=ADDRESS, given up
# after 30 s, which `curl --max-time 30` holds.
lenses_on() {
  max_time=30 person_send GET "/api/v1/lenses?on=$(q "$3")"
  want_status 200 "$1" "$2"
}

# new_issue SECTION TITLE: the agent writes an issue in PROJECT, with a spec of its own, and sets
# $key and $spec, the spec's address.
new_issue() {
  send POST /api/v1/issues "$(jq -cn --arg p "$project" --arg s "$session" --arg t "$2" \
    '{project: $p, title: $t, force: true, actor: {kind: "session", id: $s},
      spec: "The spec of \($t), which lenses are invoked on."}')"
  want_status 201 "$1" "create the issue for $2"
  key="$(jq -er '.key' "$work/body")" || fail "$1" "create the issue for $2" "no key: $(body)"
  issues+=("$key")
  spec="dispatch://$key/spec"
  pass "$1" "created $key in $project, whose spec is $spec"
}

# invoke_kind SECTION WHAT KIND [TRIGGER]: the person invokes a lens of KIND on $spec (lib.sh's
# invoke), with a triggered_by tuple to the lens TRIGGER names where one is given, which the answer
# names. It sets $lens to the lens's address and $lens_id to its uuid.
invoke_kind() {
  local trigger="${4-}"
  invoke "$1" "$2" "cookie:$login" "$(jq -cn --arg on "$spec" --arg k "$3" --arg t "$trigger" \
    '{on: $on, kind: $k} + (if $t == "" then {} else {triggered_by: $t} end)')"
  [[ -z "$trigger" ]] || [[ "$(jq -r '.triggered_by' "$work/invoked")" == "$trigger" ]] ||
    fail "$1" "$2" "the write answers triggered_by $(jq -c '.triggered_by' "$work/invoked"), want $trigger"
}

# pointer NODE BODY: the person writes one pointer tuple out of the node whose store address is
# NODE (POST /api/v1/issues/{key}/nodes/{id}/pointers), the answer left as it came.
pointer() {
  person_send POST "/api/v1/issues/$key/nodes/$(q "$1")/pointers" "$2"
}

# lens_field ADDRESS FILTER: FILTER over the lens ADDRESS of the last answer's lenses, compact.
lens_field() {
  jq -c --arg a "$1" ".lenses[] | select(.address == \$a) | $2" "$work/body"
}

# chain_of SECTION LENGTH: seeds an acyclic chain of LENGTH lenses on $spec, each but the first
# triggered by the one before, and writes their addresses in invoked order to $work/chain.json. The
# passing line of each invocation is kept to itself; a failing one is printed.
chain_of() {
  local section="$1" length="$2" at previous=""
  : >"$work/chain.txt"
  for ((at = 1; at <= length; at++)); do
    invoke_kind "$section" "invoke lens $at of the chain of $length" trade-off "$previous" >/dev/null
    printf '%s\n' "$lens" >>"$work/chain.txt"
    previous="$lens"
  done
  jq -Rsc 'split("\n") | map(select(. != ""))' "$work/chain.txt" >"$work/chain.json"
  pass "$section" "seeded an acyclic chain of $length lenses on $spec, each triggered by the one before"
}

# --- the catalogue ------------------------------------------------------------------------------

seeded_catalogue

# --- setup --------------------------------------------------------------------------------------

sign_in setup "$login"

# --- T1: a trigger read from both ends ----------------------------------------------------------

new_issue T1 "Lens trigger check"
invoke_kind T1 "invoke a trade-off analysis on the spec" trade-off
readonly tradeoff="$lens" tradeoff_id="$lens_id"
invoke_kind T1 "invoke the pre-mortem the trade-off analysis asked for, triggered by it" pre-mortem "$tradeoff"
readonly premortem="$lens" premortem_id="$lens_id"

lenses_on T1 "read the lenses on the spec" "$spec"
got="$(lens_field "$premortem" '.triggered_by')"
[[ "$got" == "[\"$tradeoff\"]" ]] ||
  fail T1 "the pre-mortem answers the trade-off analysis as the lens that triggered it" \
    "the pre-mortem $premortem answers triggered_by $got, want the trade-off analysis $tradeoff"
pass T1 "the pre-mortem $premortem answers the trade-off analysis $tradeoff as the lens that triggered it"
got="$(lens_field "$tradeoff" '.triggered')"
[[ "$got" == "[\"$premortem\"]" ]] ||
  fail T1 "the trade-off analysis answers the pre-mortem among the lenses it triggered" \
    "the trade-off analysis $tradeoff answers triggered $got, want the pre-mortem $premortem"
pass T1 "the trade-off analysis $tradeoff answers the pre-mortem $premortem among the lenses it triggered"
expect T1 "the same read answers the chain whole, both lenses in their invoked order, with no stop" \
  "$(jq -cn --arg a "$tradeoff" --arg b "$premortem" '[{lenses: [$a, $b], stopped_at: null}]')" \
  '[.chains[] | {lenses: [.lenses[].address], stopped_at}]'
expect T1 "in the chain the pre-mortem is marked as triggered by the trade-off analysis" \
  "$(jq -cn --arg a "$tradeoff" --arg b "$premortem" '[{address: $a, triggered_by: []}, {address: $b, triggered_by: [$a]}]')" \
  '[.chains[0].lenses[] | {address, triggered_by}]'

invoke_kind T1 "invoke a third lens, an inversion the pre-mortem triggered" inversion "$premortem"
readonly third="$lens"
lenses_on T1 "read the lenses on the spec" "$spec"
expect T1 "the spec answers the chain of three, in invoked order, with no stop" \
  "$(jq -cn --arg a "$tradeoff" --arg b "$premortem" --arg c "$third" '[{lenses: [$a, $b, $c], stopped_at: null}]')" \
  '[.chains[] | {lenses: [.lenses[].address], stopped_at}]'
expect T1 "in the chain of three each lens is marked as triggered by the one before it" \
  "$(jq -cn --arg a "$tradeoff" --arg b "$premortem" --arg c "$third" \
    '[{address: $a, triggered: [$b], triggered_by: []}, {address: $b, triggered: [$c], triggered_by: [$a]}, {address: $c, triggered: [], triggered_by: [$b]}]')" \
  '[.chains[0].lenses[] | {address, triggered, triggered_by}]'

# --- T2: a retracted trigger is answered by neither end -----------------------------------------

pointer "lens/$premortem_id" "$(jq -cn --arg t "$tradeoff" '{kind: "triggered_by", action: "retract", to: $t}')"
want_status 200 T2 "retract the pre-mortem's triggered_by tuple through the tuple route"
expect T2 "the retraction answers the tuple it retracted" \
  "$(jq -cn --arg n "$premortem" --arg t "$tradeoff" '{action: "retract", kind: "triggered_by", node: $n, to: $t}')" \
  '{action, kind, node: .node.address, to: .to.address}'

lenses_on T2 "read the lenses on the spec after the retraction" "$spec"
got="$(lens_field "$premortem" '.triggered_by')"
[[ "$got" == "[]" ]] ||
  fail T2 "the pre-mortem no longer answers the trade-off analysis" \
    "the pre-mortem $premortem answers triggered_by $got after the retraction of its tuple to $tradeoff"
pass T2 "the pre-mortem $premortem no longer answers the trade-off analysis $tradeoff"
got="$(lens_field "$tradeoff" '.triggered')"
[[ "$got" == "[]" ]] ||
  fail T2 "the trade-off analysis no longer answers the pre-mortem" \
    "the trade-off analysis $tradeoff answers triggered $got after the retraction of the tuple from $premortem"
pass T2 "the trade-off analysis $tradeoff no longer answers the pre-mortem $premortem"
# The same read still answers the trigger nobody retracted, so the two empty lists above are the
# relation's absence and not a read that answers none.
expect T2 "the same read still answers the pre-mortem's trigger of the inversion, which nobody retracted" \
  "$(jq -cn --arg b "$premortem" --arg c "$third" '{inversion: [$b], premortem: [$c]}')" \
  --arg b "$premortem" --arg c "$third" \
  '{premortem: (.lenses[] | select(.address == $b) | .triggered), inversion: (.lenses[] | select(.address == $c) | .triggered_by)}'
expect T2 "the chain now runs from the pre-mortem to the inversion, and the trade-off analysis is in none" \
  "$(jq -cn --arg b "$premortem" --arg c "$third" '[[$b, $c]]')" '[.chains[] | [.lenses[].address]]'

lenses_on T2 "read the pre-mortem at its own address" "$premortem"
expect T2 "the pre-mortem, read at its own address, answers no lens that triggered it" '[]' '.lens.triggered_by'
lenses_on T2 "read the trade-off analysis at its own address" "$tradeoff"
expect T2 "the trade-off analysis, read at its own address, answers no lens it triggered" '[]' '.lens.triggered'

# --- T3: a lens triggered by itself is refused --------------------------------------------------

pointer "lens/$tradeoff_id" "$(jq -cn --arg t "$tradeoff" '{kind: "triggered_by", action: "append", to: $t}')"
want_status 400 T3 "a triggered_by tuple from the trade-off analysis at itself is refused with HTTP 400"
pass T3 "a triggered_by tuple from the trade-off analysis at itself is refused with HTTP $status"
expect T3 "the refusal carries a code, INVALID_POINTER" '"INVALID_POINTER"' '.code'

person_send GET "/api/v1/references?from=$(q "$tradeoff")&kind=triggered_by"
want_status 200 T3 "read the trade-off analysis's triggered_by tuples"
expect T3 "the trade-off analysis holds no triggered_by tuple" '[]' '[.edges[] | .node.ref]'
# The same read of the inversion, which holds one, finds it.
person_send GET "/api/v1/references?from=$(q "$third")&kind=triggered_by"
want_status 200 T3 "read the inversion's triggered_by tuples"
expect T3 "the same read finds the inversion's triggered_by tuple, to the pre-mortem" "[\"$premortem\"]" \
  '[.edges[] | .node.ref]'
lenses_on T3 "read the trade-off analysis at its own address" "$tradeoff"
expect T3 "the trade-off analysis reads back triggered by no lens" '[]' '.lens.triggered_by'

# --- T4: the lenses' author ---------------------------------------------------------------------

lenses_on T4 "read the lenses on the spec" "$spec"
expect T4 "every lens on the spec is $login's, whom DEV_SIGNIN_LOGIN signs in" \
  "$(jq -cnS --arg l "$login" '{authors: [{id: $l, kind: "person"}], lenses: 3}')" \
  '{authors: ([.lenses[].author] | unique), lenses: (.lenses | length)}'

# --- T5: a cycle is read once -------------------------------------------------------------------

new_issue T5 "Lens trigger cycle check"
invoke_kind T5 "invoke lens A" trade-off
readonly cycle_a="$lens" cycle_a_id="$lens_id"
invoke_kind T5 "invoke lens B, triggered by A" pre-mortem "$cycle_a"
readonly cycle_b="$lens"
pointer "lens/$cycle_a_id" "$(jq -cn --arg t "$cycle_b" '{kind: "triggered_by", action: "append", to: $t}')"
want_status 200 T5 "append a triggered_by tuple from A to B, which closes the cycle"
pass T5 "appended a triggered_by tuple from A to B, which closes the cycle"

lenses_on T5 "read the lenses on the spec of the cycle within 30 s" "$spec"
pass T5 "the read of the cycle returned within 30 s"
expect T5 "the spec answers A and B once each" \
  "$(jq -cn --arg a "$cycle_a" --arg b "$cycle_b" '[$a, $b]')" '[.lenses[].address]'
expect T5 "the cycle is one chain that lists A and B once each, which is the visited set at work" \
  "$(jq -cn --arg a "$cycle_a" --arg b "$cycle_b" '[{lenses: [$a, $b], stopped_at: null}]')" \
  '[.chains[] | {lenses: [.lenses[].address], stopped_at}]'
expect T5 "each end of the cycle answers the other both ways" \
  "$(jq -cn --arg a "$cycle_a" --arg b "$cycle_b" \
    '[{address: $a, triggered: [$b], triggered_by: [$b]}, {address: $b, triggered: [$a], triggered_by: [$a]}]')" \
  '[.chains[0].lenses[] | {address, triggered, triggered_by}]'
expect T5 "both lenses of the cycle are $login's" "$(jq -cnS --arg l "$login" '[{id: $l, kind: "person"}]')" \
  '[.lenses[].author] | unique'

# --- T6: a chain past the bound is cut at fifty -------------------------------------------------

new_issue T6 "Lens trigger bound check"
chain_of T6 "$past_bound"
lenses_on T6 "read the lenses on the spec of the chain of $past_bound within 30 s" "$spec"
pass T6 "the read of the chain of $past_bound returned within 30 s"
served="$(jq -r '.chains[0].lenses | length' "$work/body")" ||
  fail T6 "read the first chain of the spec" "unreadable: $(body)"
[[ "$served" == "$bound" ]] ||
  fail T6 "the read lists $bound lenses of the chain of $past_bound" "the read served a chain of $served lenses"
pass T6 "the read lists $bound lenses of the chain of $past_bound, and no more"
expect T6 "the chain it lists is the first $bound lenses, in invoked order" \
  "$(jq -c --argjson n "$bound" '.[:$n]' "$work/chain.json")" '[.chains[0].lenses[].address]'
expect T6 "the read names the lens where it stopped, lens $((bound + 1))" \
  "$(jq -c --argjson n "$bound" '.[$n]' "$work/chain.json")" '.chains[0].stopped_at'
expect T6 "the spec answers all $past_bound lenses of the chain, each $login's" \
  "$(jq -cnS --arg l "$login" --argjson n "$past_bound" '{authors: [{id: $l, kind: "person"}], lenses: $n}')" \
  '{authors: ([.lenses[].author] | unique), lenses: (.lenses | length)}'

# --- T7: a chain of exactly fifty is listed whole -----------------------------------------------

new_issue T7 "Lens trigger whole chain check"
chain_of T7 "$bound"
lenses_on T7 "read the lenses on the spec of the chain of $bound within 30 s" "$spec"
pass T7 "the read of the chain of $bound returned within 30 s"
served="$(jq -c '[.chains[].lenses | length]' "$work/body")" ||
  fail T7 "read the chains of the spec" "unreadable: $(body)"
[[ "$served" == "[$bound]" ]] ||
  fail T7 "the read lists the chain of $bound whole, as one chain" "the read served chains of $served lenses"
pass T7 "the read lists the chain of $bound whole, as one chain"
expect T7 "the chain is all $bound lenses, in invoked order" "$(jq -c '.' "$work/chain.json")" \
  '[.chains[0].lenses[].address]'
expect T7 "the read reports no bound" 'null' '.chains[0].stopped_at'
expect T7 "the spec answers all $bound lenses of the chain, each $login's" \
  "$(jq -cnS --arg l "$login" --argjson n "$bound" '{authors: [{id: $l, kind: "person"}], lenses: $n}')" \
  '{authors: ([.lenses[].author] | unique), lenses: (.lenses | length)}'

pass "$driver" complete
