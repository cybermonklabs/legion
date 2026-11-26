#!/usr/bin/env bash
# Drives the invocation of a lens end to end against a running server (check c-lens-invocation,
# spec requirement r-lens). As the person DEV_SIGNIN_LOGIN signs
# in, it invokes three lenses on one spec - a pre-mortem, a trade-off analysis and a BOTEC - each
# one lens node under the spec through its part_of tuple, with one has_kind tuple to a lens_kind
# node of the catalogue, and reads them back through GET /api/v1/lenses in the order it invoked
# them, each with its kind, its author and its time. It writes the findings of the trade-off
# analysis and of the BOTEC as block nodes in the request that writes each lens, adds one more to
# the BOTEC later, and reads each finding under its lens, with its rank and its text, at its own
# address `dispatch://lens/<uuid>#b-<id>`, carrying the visibility of its lens. It reads each
# lens's own tuples through GET /api/v1/references?from=, invokes a lens on a comment, which is
# no spec, retargets the kind of a lens that is no pre-mortem, and is refused the retarget of a
# pre-mortem's kind twice, once for a pre-mortem that holds a risk and once for one that holds
# none, with one body. Last it writes the refusals - a second has_kind tuple on one lens, a
# has_kind tuple at a word no lens_kind node holds, and a lens with no kind - and after each reads
# the lens's tuples as they were, or finds no lens stored.
#
# Inputs: DISPATCH_URL (the server), DISPATCH_TOKEN or DISPATCH_TOKEN_FILE (an agent bearer, read
# here and never printed), PROJECT (an existing project key) and DEV_SIGNIN_LOGIN (the login the
# server's dev sign-in route, /auth/_dev/signin, signs in: `alice` on `bun run dispatch:local`).
# Every lens is that person's write, made with the cookie the dev sign-in sets and the server's
# origin, so that person is each lens's author; the agent bearer writes the issue and the comment
# and closes the issue when the run ends, also when a check fails. Each unmet input is exit 2.
#
# The first line is `PASS: lens-invocation.sh the catalogue holds its ten kinds`, read before any
# lens is written. Each passing check prints `PASS: <section> <what>`; the first failing one
# prints `FAIL: <section> <what>: <observed>` and the script exits 1. The last line is
# `PASS: lens-invocation.sh complete`.
#
# Every check is `cmd || fail …`, never part of an `&&` list: under `set -e` a failing command
# anywhere but last in such a list does not stop the script.

# shellcheck disable=SC2016 # the single-quoted jq filters name jq variables, not shell ones
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DISPATCH_URL=http://127.0.0.1:8777 DISPATCH_TOKEN=e2e-token PROJECT=CORE \
         DEV_SIGNIN_LOGIN=alice lens-invocation.sh
       (DISPATCH_TOKEN_FILE=<file holding the bearer> in place of DISPATCH_TOKEN; the server must
       mount its dev sign-in route, as `bun run dispatch:local` does)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
require_env DISPATCH_URL PROJECT DEV_SIGNIN_LOGIN
require_commands cmp curl jq
read_bearer

readonly driver=lens-invocation.sh
# shellcheck disable=SC2031 # the DISPATCH_URL lib.sh's `cli` exports is its own; this one is the caller's
readonly base="${DISPATCH_URL%/}"
# shellcheck disable=SC2153 # PROJECT is the caller's input, checked by name above
readonly project="$PROJECT"
readonly login="$DEV_SIGNIN_LOGIN"
stamp="$(date +%s)-$$"
readonly stamp
readonly session="lens-invocation-$stamp"
# A word no lens_kind node holds: a slug the catalogue's ids take the form of, named for this run.
readonly unheld_kind="unheld-kind-$stamp"

work_dir
printf 'Authorization: Bearer %s\n' "$token" >"$work/auth"

key=""
cleanup() {
  local code=$?
  [[ -z "$key" ]] || close_issue "$key"
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# lenses_on SECTION WHAT ADDRESS: the person's read of GET /api/v1/lenses?on=ADDRESS.
lenses_on() {
  person_send GET "/api/v1/lenses?on=$(q "$3")"
  want_status 200 "$1" "$2"
}

# references SECTION WHAT DIRECTION ADDRESS [KIND]: the person's read of GET /api/v1/references
# with DIRECTION (from or to) at ADDRESS, kept to the edge kind KIND where it is given.
references() {
  person_send GET "/api/v1/references?$3=$(q "$4")${5:+&kind=$5}"
  want_status 200 "$1" "$2"
}

# tuples_out SECTION LENS FILE: writes the tuples out of the lens LENS - each edge's kind, far end
# and time, sorted - to FILE, the record a refusal is held to.
tuples_out() {
  references "$1" "read the tuples out of $2" from "$2"
  jq -cS '[.edges[] | {kind, to: (.node.ref // .node.id), created_at}] | sort' "$work/body" >"$3" ||
    fail "$1" "read the tuples out of $2" "unreadable: $(body)"
}

# held_kinds SECTION LENS FILE: writes the tuples out of the lens LENS to FILE (tuples_out), and
# sets $kinds to the far ends of the has_kind tuples among them, by id, comma-separated, and
# $kind_count to how many there are.
held_kinds() {
  tuples_out "$1" "$2" "$3"
  kinds="$(jq -r '[.[] | select(.kind == "has_kind") | .to | ltrimstr("dispatch://lens_kind/")] | sort | join(", ")' "$3")"
  kind_count="$(jq -r '[.[] | select(.kind == "has_kind")] | length' "$3")"
}

# pointer SECTION WHAT NODE BODY: the person writes one pointer tuple out of the node whose store
# address is NODE (POST /api/v1/issues/{key}/nodes/{id}/pointers), the answer left as it came.
pointer() {
  person_send POST "/api/v1/issues/$key/nodes/$(q "$3")/pointers" "$4"
}

# --- the catalogue ------------------------------------------------------------------------------

seeded_catalogue

# words KIND: the words the catalogue's lens_kind node KIND carries.
words() { jq -r --arg k "$1" '.lens_kinds[] | select(.id == $k) | .words' "$work/catalogue.json"; }

# --- setup --------------------------------------------------------------------------------------

sign_in setup "$login"

send POST /api/v1/issues "$(jq -cn --arg p "$project" --arg s "$session" \
  '{project: $p, title: "Lens invocation check", force: true, actor: {kind: "session", id: $s},
    spec: "The spec three lenses are invoked on.\n\nIt holds one more paragraph."}')"
want_status 201 setup "create the issue"
key="$(jq -er '.key' "$work/body")" || fail setup "create the issue" "no key: $(body)"
readonly spec="dispatch://$key/spec"
pass setup "created $key in $project, whose spec is $spec"

send POST "/api/v1/issues/$key/comments" "$(jq -cn --arg s "$session" \
  '{body: "A comment a lens is invoked on.", actor: {kind: "session", id: $s}}')"
want_status 201 setup "write a comment on $key"
comment_id="$(jq -er '.id' "$work/body")" || fail setup "write a comment on $key" "no id: $(body)"
readonly comment="dispatch://$key/comment/$comment_id"
pass setup "wrote the comment $comment"

# --- L1: three lenses on one spec, in the order they were invoked -------------------------------

invoke L1 "invoke a pre-mortem on the spec" "cookie:$login" "$(jq -cn --arg on "$spec" '{on: $on, kind: "pre-mortem"}')"
readonly premortem="$lens" premortem_id="$lens_id"
cp "$work/invoked" "$work/premortem.json"

invoke L1 "invoke a trade-off analysis on the spec, with two findings" "cookie:$login" "$(jq -cn --arg on "$spec" \
  '{on: $on, kind: "trade-off", findings: ["Postgres holds the graph; a second store doubles the writes.", "One store is the boring choice."]}')"
readonly tradeoff="$lens" tradeoff_id="$lens_id"
cp "$work/invoked" "$work/tradeoff.json"

# The BOTEC is reserved for its author, so a finding that took any visibility but its lens's own
# reads apart from the trade-off analysis's, which every viewer of the project reads.
invoke L1 "invoke a BOTEC on the spec, reserved for its author, with one finding" "cookie:$login" "$(jq -cn --arg on "$spec" \
  '{on: $on, kind: "botec", visibility: "author", findings: ["Three lenses an issue, a hundred issues a week: three hundred lens rows."]}')"
readonly botec="$lens" botec_id="$lens_id"
cp "$work/invoked" "$work/botec.json"
# Each of the three by its name: its address, its uuid, its kind and the visibility it was written at.
declare -rA address_of=([premortem]="$premortem" [tradeoff]="$tradeoff" [botec]="$botec")
declare -rA id_of=([premortem]="$premortem_id" [tradeoff]="$tradeoff_id" [botec]="$botec_id")
declare -rA kind_of=([premortem]=pre-mortem [tradeoff]=trade-off [botec]=botec)
declare -rA visibility_of=([premortem]=project [tradeoff]=project [botec]=author)

for written in premortem tradeoff botec; do
  cp "$work/$written.json" "$work/body"
  address="${address_of[$written]}" kind="${kind_of[$written]}" visibility="${visibility_of[$written]}"
  expect L1 "the write answers $address as a $kind lens on the spec, by $login, at $visibility" \
    "$(jq -cnS --arg k "$kind" --arg on "$spec" --arg l "$login" --arg v "$visibility" \
      '{author: {id: $l, kind: "person"}, kind: $k, on: $on, state: "live", visibility: $v}')" \
    '{author, kind, on, state, visibility}'
done

# The pre-mortem's finding is its risks: one risk under it, its heading the finding.
person_send POST "/api/v1/issues/$key/risks" "$(jq -cn --arg l "$premortem" \
  '{lens: $l, heading: "The graph store loses a write under load.", early_warning: "A tuple count that falls behind the rows.", prevention: "One transaction per write."}')"
want_status 201 L1 "write a risk under the pre-mortem"
risk="$(jq -er '.address' "$work/body")" || fail L1 "write a risk under the pre-mortem" "no address: $(body)"
readonly risk
pass L1 "wrote $risk under the pre-mortem"

# One more finding of the BOTEC, added after the lens: it takes its lens's visibility.
person_send POST "/api/v1/issues/$key/nodes/$(q "lens/$botec_id")/findings" \
  '{"text": "At one kilobyte a row, three hundred kilobytes a week."}'
want_status 201 L1 "add one more finding to the BOTEC"
cp "$work/body" "$work/botec-added.json"
pass L1 "added $(jq -r '.address' "$work/botec-added.json") to the BOTEC"

lenses_on L1 "read the lenses on the spec" "$spec"
cp "$work/body" "$work/spec-lenses.json"
expect L1 "the spec answers the three lenses in the order they were invoked" \
  "$(jq -cn --arg a "$premortem" --arg b "$tradeoff" --arg c "$botec" '[$a, $b, $c]')" \
  '[.lenses[].address]'
for written in premortem tradeoff botec; do
  address="${address_of[$written]}" kind="${kind_of[$written]}"
  expect L1 "$address answers its kind $kind with the words its lens_kind node carries" \
    "$(jq -cnS --arg k "$kind" --arg w "$(words "$kind")" '{id: $k, words: $w}')" \
    --arg a "$address" '.lenses[] | select(.address == $a) | .kind'
  expect L1 "$address answers its author, $login" \
    "$(jq -cnS --arg l "$login" '{id: $l, kind: "person"}')" \
    --arg a "$address" '.lenses[] | select(.address == $a) | .author'
  expect L1 "$address answers the time it was written" \
    "$(jq -c '.created_at' "$work/$written.json")" \
    --arg a "$address" '.lenses[] | select(.address == $a) | .created_at'
  expect L1 "$address answers the spec it was invoked on" "\"$spec\"" \
    --arg a "$address" '.lenses[] | select(.address == $a) | .on'
done

# --- L2: the findings ---------------------------------------------------------------------------

expect L2 "the pre-mortem's finding is its risk, and it holds no block" \
  "$(jq -cn --arg r "$risk" '{findings: [], risks: [$r]}')" \
  --arg a "$premortem" '.lenses[] | select(.address == $a) | {findings, risks: [.risks[].address]}'

# The findings each write answered, then the one added to the BOTEC later, as {address, text} in
# the order they were written.
jq -c '[.findings[] | {address, text}]' "$work/tradeoff.json" >"$work/tradeoff-findings.json"
jq -c --slurpfile added "$work/botec-added.json" \
  '[.findings[] | {address, text}] + [$added[0] | {address, text}]' "$work/botec.json" \
  >"$work/botec-findings.json"

for written in tradeoff botec; do
  address="${address_of[$written]}" id="${id_of[$written]}" visibility="${visibility_of[$written]}"
  cp "$work/spec-lenses.json" "$work/body"
  expect L2 "$address answers its findings under it, text for text, in the order they were written" \
    "$(jq -cS '.' "$work/$written-findings.json")" \
    --arg a "$address" '.lenses[] | select(.address == $a) | [.findings[] | {address, text}]'
  expect L2 "each finding of $address holds a rank, the ranks rising in that order" true \
    --arg a "$address" '.lenses[] | select(.address == $a) | [.findings[].rank] as $r
      | ($r | length) > 0 and all($r[]; type == "number") and $r == ($r | sort) and ($r | unique | length) == ($r | length)'
  expect L2 "each finding of $address carries the visibility of its lens, $visibility" \
    "$(jq -cn --arg v "$visibility" '[$v]')" \
    --arg a "$address" '.lenses[] | select(.address == $a) | [.findings[].visibility] | unique'
  expect L2 "each finding of $address is at an address of its own, dispatch://lens/<uuid>#b-<random block id>" true \
    --arg a "$address" --arg id "$id" '.lenses[] | select(.address == $a) | [.findings[].address] as $f
      | ($f | unique | length) == ($f | length)
        and all($f[]; test("^dispatch://lens/" + $id + "#b-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$"))'
  while IFS= read -r finding; do
    lenses_on L2 "read the finding $finding at its own address" "$finding"
    expect L2 "$finding answers at its own address as a finding of $address, at $visibility" \
      "$(jq -cnS --arg f "$finding" --arg v "$visibility" '{address: $f, kind: "lens_block", visibility: $v}')" \
      '.on | {address, kind, visibility}'
  done < <(jq -r '.[].address' "$work/$written-findings.json")
done

# --- L3: the parent tuple is the log of the invocation ------------------------------------------

for written in premortem tradeoff botec; do
  address="${address_of[$written]}" kind="${kind_of[$written]}"
  references L3 "read the tuples out of $address" from "$address"
  expect L3 "$address holds one part_of tuple, to the spec, and one has_kind tuple, to lens_kind $kind" \
    "$(jq -cn --arg s "$spec" --arg k "dispatch://lens_kind/$kind" '[["has_kind", "lens_kind", $k], ["part_of", "artifact", $s]]')" \
    '[.edges[] | select(.kind == "part_of" or .kind == "has_kind") | [.kind, .node.kind, .node.ref]] | sort'
done

# --- L4: a lens applies to any node -------------------------------------------------------------

invoke L4 "invoke an inversion on the comment, which is no spec" "cookie:$login" \
  "$(jq -cn --arg on "$comment" '{on: $on, kind: "inversion"}')"
readonly on_comment="$lens"
lenses_on L4 "read the lenses on the comment" "$comment"
expect L4 "the comment answers as a comment" '"comment"' '.on.kind'
expect L4 "the comment answers the inversion invoked on it, and no other lens" \
  "$(jq -cnS --arg l "$on_comment" --arg on "$comment" '[{address: $l, kind: "inversion", on: $on}]')" \
  '[.lenses[] | {address, kind: .kind.id, on}]'

# --- L5: a retarget of the kind of a lens that is no pre-mortem, and of a pre-mortem's ----------

references L5 "read the trade-off analysis's part_of tuple before the retarget" from "$tradeoff" part_of
jq -cS '[.edges[] | {kind, to: .node.ref, created_at}]' "$work/body" >"$work/tradeoff-part-of.json" ||
  fail L5 "read the trade-off analysis's part_of tuple before the retarget" "unreadable: $(body)"
[[ "$(jq 'length' "$work/tradeoff-part-of.json")" == 1 ]] ||
  fail L5 "the trade-off analysis holds one part_of tuple before the retarget" "$(<"$work/tradeoff-part-of.json")"
pass L5 "the trade-off analysis holds one part_of tuple before the retarget"

pointer L5 "retarget the trade-off analysis's kind" "lens/$tradeoff_id" \
  '{"kind": "has_kind", "action": "retarget", "from": "trade-off", "to": "reversibility"}'
want_status 200 L5 "retarget the trade-off analysis's has_kind from trade-off to reversibility"
expect L5 "the retarget answers the move from trade-off to reversibility" \
  '{"action":"retarget","from":"dispatch://lens_kind/trade-off","to":"dispatch://lens_kind/reversibility"}' \
  '{action, from: .from.address, to: .to.address}'

# The second pre-mortem holds no risk; the first holds the one L1 wrote under it.
invoke L5 "invoke a second pre-mortem on the spec, which holds no risk" "cookie:$login" \
  "$(jq -cn --arg on "$spec" '{on: $on, kind: "pre-mortem"}')"
readonly bare_premortem="$lens" bare_premortem_id="$lens_id"
lenses_on L5 "read the lenses on the spec" "$spec"
expect L5 "the first pre-mortem holds a risk, and the second holds none" '[1,0]' \
  --arg a "$premortem" --arg b "$bare_premortem" \
  '[(.lenses[] | select(.address == $a) | .risks | length), (.lenses[] | select(.address == $b) | .risks | length)]'

pointer L5 "retarget the pre-mortem that holds a risk" "lens/$premortem_id" \
  '{"kind": "has_kind", "action": "retarget", "from": "pre-mortem", "to": "inversion"}'
want_status 409 L5 "the retarget of the pre-mortem that holds a risk away from pre-mortem"
expect L5 "the retarget of the pre-mortem that holds a risk is POINTER_ALREADY_ASSERTED" \
  '"POINTER_ALREADY_ASSERTED"' '.code'
cp "$work/body" "$work/kept-with-risk.json"
pointer L5 "retarget the pre-mortem that holds none" "lens/$bare_premortem_id" \
  '{"kind": "has_kind", "action": "retarget", "from": "pre-mortem", "to": "inversion"}'
want_status 409 L5 "the retarget of the pre-mortem that holds none away from pre-mortem"
expect L5 "the retarget of the pre-mortem that holds none is POINTER_ALREADY_ASSERTED" \
  '"POINTER_ALREADY_ASSERTED"' '.code'
cmp -s "$work/kept-with-risk.json" "$work/body" ||
  fail L5 "the two refusals answer the same body" \
    "with a risk: $(<"$work/kept-with-risk.json"); with none: $(body)"
pass L5 "the two refusals answer the same body, byte for byte: $(body)"
for address in "$premortem" "$bare_premortem"; do
  held_kinds L5 "$address" "$work/held.json"
  [[ "$kinds" == pre-mortem && "$kind_count" == 1 ]] ||
    fail L5 "$address keeps its one kind, pre-mortem" "it holds $kind_count has_kind tuples: $kinds"
  pass L5 "$address keeps its one kind, pre-mortem"
done

lenses_on L5 "read the lenses on the spec after the retarget" "$spec"
expect L5 "the trade-off analysis answers its new kind, reversibility" \
  "$(jq -cnS --arg w "$(words reversibility)" '{id: "reversibility", words: $w}')" \
  --arg a "$tradeoff" '.lenses[] | select(.address == $a) | .kind'
references L5 "read the trade-off analysis's tuples after the retarget" from "$tradeoff"
expect L5 "its references hold one has_kind tuple, to the new kind" \
  '[["lens_kind","dispatch://lens_kind/reversibility"]]' \
  '[.edges[] | select(.kind == "has_kind") | [.node.kind, .node.ref]]'
expect L5 "its part_of tuple to the spec is the one it had" \
  "$(<"$work/tradeoff-part-of.json")" '[.edges[] | select(.kind == "part_of") | {kind, to: .node.ref, created_at}]'

# --- R1: a second has_kind tuple on one lens is refused -----------------------------------------

tuples_out R1 "$botec" "$work/botec-before.json"
pointer R1 "assert a second kind of the BOTEC" "lens/$botec_id" \
  '{"kind": "has_kind", "action": "append", "to": "inversion"}'
cp "$work/body" "$work/refusal.json"
refused_status="$status"
held_kinds R1 "$botec" "$work/botec-after.json"
[[ "$kind_count" == 1 ]] ||
  fail R1 "the BOTEC holds one kind after a second has_kind is asked for" \
    "$botec holds $kind_count has_kind tuples: $kinds (the write answered HTTP $refused_status: $(<"$work/refusal.json"))"
pass R1 "the BOTEC holds one kind after a second has_kind is asked for: $kinds"
cp "$work/refusal.json" "$work/body"
status="$refused_status"
want_status 409 R1 "the second has_kind tuple is refused"
expect R1 "the refusal is POINTER_ALREADY_ASSERTED" '"POINTER_ALREADY_ASSERTED"' '.code'
cmp -s "$work/botec-before.json" "$work/botec-after.json" ||
  fail R1 "the BOTEC's tuples are as they were" "before: $(<"$work/botec-before.json"); after: $(<"$work/botec-after.json")"
pass R1 "the BOTEC's tuples are as they were"

# --- R2: a has_kind tuple at a word no lens_kind node holds is refused --------------------------

# The absence beside its control: the same read finds the ten kinds and no node of this word.
cp "$work/catalogue.json" "$work/body"
expect R2 "the catalogue that holds the ten kinds holds no lens_kind $unheld_kind" \
  '{"seeded":10,"unheld":0}' --argjson ten "$seeded_kinds" --arg w "$unheld_kind" \
  '{seeded: ([.lens_kinds[].id] as $ids | [$ten[] | select(. as $id | $ids | index($id))] | length),
    unheld: ([.lens_kinds[] | select(.id == $w)] | length)}'

lenses_on R2 "read the lenses on the spec before the write" "$spec"
jq -c '[.lenses[].address]' "$work/body" >"$work/spec-before.json"
person_send POST "/api/v1/issues/$key/lenses" "$(jq -cn --arg on "$spec" --arg k "$unheld_kind" '{on: $on, kind: $k}')"
cp "$work/body" "$work/refusal.json"
refused_status="$status"
lenses_on R2 "read the lenses on the spec after the write" "$spec"
jq -c '[.lenses[].address]' "$work/body" >"$work/spec-after.json"
cmp -s "$work/spec-before.json" "$work/spec-after.json" ||
  fail R2 "a lens of the word $unheld_kind, which no lens_kind node holds, is refused" \
    "the word was accepted: HTTP $refused_status $(<"$work/refusal.json"); the spec holds $(<"$work/spec-after.json"), it held $(<"$work/spec-before.json")"
pass R2 "no lens of the word $unheld_kind is stored on the spec"
cp "$work/refusal.json" "$work/body"
status="$refused_status"
want_status 400 R2 "a lens of the word $unheld_kind is refused"
expect R2 "the refusal is INVALID_POINTER" '"INVALID_POINTER"' '.code'

tuples_out R2 "$botec" "$work/botec-before.json"
pointer R2 "retarget the BOTEC's kind at the word $unheld_kind" "lens/$botec_id" \
  "$(jq -cn --arg k "$unheld_kind" '{kind: "has_kind", action: "retarget", from: "botec", to: $k}')"
cp "$work/body" "$work/refusal.json"
refused_status="$status"
held_kinds R2 "$botec" "$work/botec-after.json"
[[ "$kinds" == botec && "$kind_count" == 1 ]] ||
  fail R2 "the BOTEC keeps its kind when its has_kind is retargeted at $unheld_kind" \
    "the word was accepted: $botec holds $kind_count has_kind tuples: $kinds (HTTP $refused_status: $(<"$work/refusal.json"))"
pass R2 "the BOTEC keeps its one kind, botec, when its has_kind is retargeted at $unheld_kind"
cp "$work/refusal.json" "$work/body"
status="$refused_status"
want_status 400 R2 "the has_kind tuple at the word $unheld_kind is refused"
expect R2 "the refusal is INVALID_POINTER" '"INVALID_POINTER"' '.code'
cmp -s "$work/botec-before.json" "$work/botec-after.json" ||
  fail R2 "the BOTEC's tuples are as they were" "before: $(<"$work/botec-before.json"); after: $(<"$work/botec-after.json")"
pass R2 "the BOTEC's tuples are as they were"

# --- R3: a lens with no kind is refused ---------------------------------------------------------

lenses_on R3 "read the lenses on the spec before the write" "$spec"
jq -c '[.lenses[].address]' "$work/body" >"$work/spec-before.json"
person_send POST "/api/v1/issues/$key/lenses" "$(jq -cn --arg on "$spec" '{on: $on, findings: ["A finding of no kind."]}')"
cp "$work/body" "$work/refusal.json"
refused_status="$status"
lenses_on R3 "read the lenses on the spec after the write" "$spec"
jq -c '[.lenses[].address]' "$work/body" >"$work/spec-after.json"
cmp -s "$work/spec-before.json" "$work/spec-after.json" ||
  fail R3 "no lens of no kind is stored on the spec" \
    "HTTP $refused_status $(<"$work/refusal.json"); the spec holds $(<"$work/spec-after.json"), it held $(<"$work/spec-before.json")"
pass R3 "no lens of no kind is stored on the spec"
cp "$work/refusal.json" "$work/body"
status="$refused_status"
want_status 400 R3 "a lens with no kind is refused with HTTP 400"
pass R3 "a lens with no kind is refused with HTTP $status"
expect R3 "the refusal carries a code, LENS_KIND_REQUIRED" '"LENS_KIND_REQUIRED"' '.code'

tuples_out R3 "$on_comment" "$work/comment-lens-before.json"
pointer R3 "retract the one kind of the inversion on the comment" "lens/${on_comment#dispatch://lens/}" \
  '{"kind": "has_kind", "action": "retract", "to": "inversion"}'
cp "$work/body" "$work/refusal.json"
refused_status="$status"
held_kinds R3 "$on_comment" "$work/comment-lens-after.json"
[[ "$kinds" == inversion && "$kind_count" == 1 ]] ||
  fail R3 "the inversion keeps its one kind when it is retracted alone" \
    "$on_comment holds $kind_count has_kind tuples: $kinds (HTTP $refused_status: $(<"$work/refusal.json"))"
pass R3 "the inversion keeps its one kind, inversion, when it is retracted alone"
cp "$work/refusal.json" "$work/body"
status="$refused_status"
want_status 400 R3 "a lens left with no kind is refused with HTTP 400"
pass R3 "a lens left with no kind is refused with HTTP $status"
expect R3 "the refusal carries a code, INVALID_POINTER" '"INVALID_POINTER"' '.code'
cmp -s "$work/comment-lens-before.json" "$work/comment-lens-after.json" ||
  fail R3 "the inversion's tuples are as they were" \
    "before: $(<"$work/comment-lens-before.json"); after: $(<"$work/comment-lens-after.json")"
pass R3 "the inversion's tuples are as they were"

# --- A: the lenses' author ----------------------------------------------------------------------

lenses_on A "read the lenses on the spec" "$spec"
expect A "every lens on the spec is $login's, whom DEV_SIGNIN_LOGIN signs in" \
  "$(jq -cnS --arg l "$login" '{authors: [{id: $l, kind: "person"}], lenses: 4}')" \
  '{authors: ([.lenses[].author] | unique), lenses: (.lenses | length)}'
lenses_on A "read the lenses on the comment" "$comment"
expect A "the lens on the comment is $login's" \
  "$(jq -cnS --arg l "$login" '[{id: $l, kind: "person"}]')" '[.lenses[].author] | unique'

pass "$driver" complete
