#!/usr/bin/env bash
# Drives the risks and the constraints of an issue end to end against a running server
# (dispatch://INFRA-1950's c-risk-prevention, dispatch://INFRA-1950/spec#b-r-constraint-risk). It
# writes an issue whose spec holds a requirement and a paragraph and whose verification document
# holds three checks, the second serving the requirement; then, through this child's routes, a lens
# of kind pre-mortem holding two risks, the first prevented by the first check, a constraint
# carrying a standard with its reference, a second constraint carrying a measurement with its value
# and where it was taken, and the requirement's constrained_by tuple to the first constraint. It
# reads each end: the risk's prevented_by tuple and its parent, the lens's and each check's
# backlinks, the requirement's constraint and its reference, the constraint nobody points at, and
# GET /api/v1/schema/blocks for the pointers no block type declares. It is refused a risk under an
# inversion and a constrained_by tuple at a paragraph, reading no tuple stored after either; adds a
# second prevented_by tuple, retargets the first, and removes the third check's block in a new
# version of its document, after which the index pass has marked the block gone, retracted the
# prevented_by tuple that pointed at it, and the risk answers the second check alone. Last, through
# the pointer route, it points the second risk at the check of a second issue of the project,
# retargets it at the check of a third and retracts it, reading the risk's tuples and each check's
# backlinks after each step; and a prevented_by tuple at the check of an issue of another project,
# which the run creates, is refused 400 INVALID_POINTER naming that check - a retarget and an
# append through the pointer route, and a risk the risk route writes - with nothing stored at
# either end.
#
# Inputs: DISPATCH_URL (the server), DISPATCH_TOKEN or DISPATCH_TOKEN_FILE (an agent bearer, read
# here and never printed) and PROJECT (an existing project key). Every write is the agent bearer's,
# acting for a session of this run's own; it closes the issues it wrote when the run ends, also
# when a check fails. The other project it creates takes PROJECT's default assignee as its own, and
# stays on the server, since no route removes a project. Each unmet input is exit 2.
#
# The first line is `PASS: premortem-risks.sh the catalogue holds its ten kinds`, read before any
# lens is written. Each passing check prints `PASS: <section> <what>`; the first failing one prints
# `FAIL: <section> <what>: <observed>` and the script exits 1. The last line is
# `PASS: premortem-risks.sh complete`.
#
# Every check is `cmd || fail …`, never part of an `&&` list: under `set -e` a failing command
# anywhere but last in such a list does not stop the script.

# shellcheck disable=SC2016 # the single-quoted jq filters name jq variables, not shell ones
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DISPATCH_URL=http://127.0.0.1:8777 DISPATCH_TOKEN=e2e-token PROJECT=CORE premortem-risks.sh
       (DISPATCH_TOKEN_FILE=<file holding the bearer> in place of DISPATCH_TOKEN)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
require_env DISPATCH_URL PROJECT
require_commands curl jq
read_bearer

readonly driver=premortem-risks.sh
# shellcheck disable=SC2031 # the DISPATCH_URL lib.sh's `cli` exports is its own; this one is the caller's
readonly base="${DISPATCH_URL%/}"
# shellcheck disable=SC2153 # PROJECT is the caller's input, checked by name above
readonly project="$PROJECT"
stamp="$(date +%s)-$$"
readonly stamp
readonly session="premortem-risks-$stamp"
# The standard the first constraint carries and its reference, which the requirement reads back
# character for character: a section sign, an en dash and curly quotes, each of which a reader that
# rewrote the text would change.
readonly standard='ISO 8601-1:2019, Date and time — Representations for information interchange'
readonly reference='ISO 8601-1:2019 §5.4.2 “Extended format” – pp. 21–22, https://www.iso.org/standard/70907.html'

work_dir
printf 'Authorization: Bearer %s\n' "$token" >"$work/auth"

key=""
keys=()
cleanup() {
  local code=$?
  local other
  for other in "${keys[@]}"; do
    close_issue "$other"
  done
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# references SECTION WHAT DIRECTION ADDRESS [KIND]: GET /api/v1/references with DIRECTION (from or
# to) at ADDRESS, kept to the edge kind KIND where it is given.
references() {
  send GET "/api/v1/references?$3=$(q "$4")${5:+&kind=$5}"
  want_status 200 "$1" "$2"
}

# lenses_on SECTION WHAT ADDRESS: GET /api/v1/lenses?on=ADDRESS.
lenses_on() {
  send GET "/api/v1/lenses?on=$(q "$3")"
  want_status 200 "$1" "$2"
}

# pointer NODE BODY: one pointer tuple out of the node whose store address is NODE
# (POST /api/v1/issues/{key}/nodes/{id}/pointers), the answer left as it came.
pointer() {
  send POST "/api/v1/issues/$key/nodes/$(q "$1")/pointers" "$(jq -c --argjson a "$(actor)" '. + {actor: $a}' <<<"$2")"
}

# upload SECTION WHAT NAME FILE: writes FILE as the next version of the issue's document NAME.
upload() {
  send POST "/api/v1/issues/$key/artifacts" "$(jq -cn --arg n "$3" --rawfile c "$4" --argjson a "$(actor)" \
    '{name: $n, content: $c, actor: $a}')"
  want_status 201 "$1" "$2"
}

# write_risk SECTION WHAT LENS [CHECK]: a risk under LENS, prevented by CHECK where one is given;
# the answer left as it came.
write_risk() {
  send POST "/api/v1/issues/$key/risks" "$(jq -cn --arg l "$3" --arg c "${4-}" --arg h "$2" --argjson a "$(actor)" \
    '{lens: $l, heading: $h, early_warning: "The first sign of \($h | ascii_downcase)",
      prevention: "What keeps \($h | ascii_downcase) from happening", actor: $a}
     + (if $c == "" then {} else {prevented_by: $c} end)')"
}

# ends SECTION WHEN HELD CHECK…: both ends of the second risk's prevented_by tuples, read after
# WHEN. Out of the risk, the tuples reach HELD alone, or nothing where HELD is empty; into each
# CHECK, its backlinks hold the risk where that CHECK is HELD, and no risk where it is not.
ends() {
  local section="$1" when="$2" held="$3" check want
  shift 3
  references "$section" "read the second risk's prevented_by tuples $when" from "$risk2" prevented_by
  want="$(jq -cn --arg h "$held" 'if $h == "" then [] else [$h] end')"
  expect "$section" "$when the second risk is prevented by ${held:-nothing}" "$want" '[.edges[] | .node.ref]'
  for check in "$@"; do
    references "$section" "read the backlinks of $check $when" to "$check"
    if [[ "$check" == "$held" ]]; then
      want="[[\"prevented_by\",\"$risk2\"]]"
    else
      want='[]'
    fi
    expect "$section" "$when the backlinks of $check hold $([[ "$want" == '[]' ]] && printf 'no risk' || printf 'the second risk')" \
      "$want" '[.edges[] | select(.node.kind == "risk") | [.kind, .node.ref]]'
  done
}

# refused_far SECTION WHAT WHEN HELD: the pointer write just sent, WHAT, which asked for a
# prevented_by tuple from the second risk at the check of another project, stored nothing - a
# tuple the store holds there is printed first - and left both ends as they were, read after WHEN:
# the risk prevented by HELD alone, or by nothing where HELD is empty, and no risk in the
# backlinks of any other check. It was refused 400 INVALID_POINTER, naming that check.
refused_far() {
  local section="$1" what="$2" when="$3" held="$4" refused_status="$status" stored
  cp "$work/body" "$work/refusal.json"
  references "$section" "read the second risk's prevented_by tuples after $what" from "$risk2" prevented_by
  stored="$(jq -c --arg c "$far_check" --arg r "$risk2" \
    '[.edges[] | select(.node.ref == $c) | {kind, from: $r, to: .node.ref, block_id: .node.block_id, created_at}]' \
    "$work/body")" || fail "$section" "read the second risk's prevented_by tuples after $what" "unreadable: $(body)"
  [[ "$stored" == "[]" ]] ||
    fail "$section" "$what is refused" \
      "the tuple at $far_check, a check of $other_project, was stored: $stored (HTTP $refused_status: $(<"$work/refusal.json"))"
  ends "$section" "$when" "$held" "$second_check" "$third_check" "$far_check"
  cp "$work/refusal.json" "$work/body"
  status="$refused_status"
  want_status 400 "$section" "$what is refused"
  expect "$section" "the refusal is INVALID_POINTER, naming the check $far_check" \
    '{"code":"INVALID_POINTER","names_the_check":true}' --arg c "$far_check" '{code, names_the_check: (.error | contains($c))}'
}

# --- the catalogue ------------------------------------------------------------------------------

seeded_catalogue

# --- setup: the requirement, a paragraph and three checks, blocks of the issue's documents -------

cat >"$work/spec.md" <<'SPEC'
The spec whose requirement a constraint holds.

:::requirement{#r1}
Every date the graph stores is written in one representation.
:::

A paragraph, which is no constraint.
SPEC
send POST /api/v1/issues "$(jq -cn --arg p "$project" --rawfile s "$work/spec.md" --argjson a "$(actor)" \
  '{project: $p, title: "Pre-mortem risks check", spec: $s, force: true, actor: $a}')"
want_status 201 setup "create the issue"
key="$(jq -er '.key' "$work/body")" || fail setup "create the issue" "no key: $(body)"
keys+=("$key")
readonly spec="dispatch://$key/spec"
readonly requirement="$spec#b-r1"
pass setup "created $key in $project, whose spec holds the requirement $requirement"

send GET "/api/v1/issues/$key/artifacts/spec/text"
want_status 200 setup "read the spec's text"
cp "$work/body" "$work/spec-text.json"
send GET "/api/v1/issues/$key/artifacts/spec/blocks"
want_status 200 setup "read the spec's blocks"
expect setup "the spec's block r1 is a requirement" '["requirement"]' '[.[] | select(.id == "r1") | .type]'
paragraph_id="$(jq -r --slurpfile text "$work/spec-text.json" \
  '$text[0].markdown as $md | [.[] | select(.type == "paragraph" and $md[.from:.to] == "A paragraph, which is no constraint.") | .id] | first // empty' \
  "$work/body")" || fail setup "find the paragraph's block" "unreadable: $(body)"
[[ -n "$paragraph_id" ]] || fail setup "find the paragraph's block" "blocks: $(body)"
readonly paragraph="$spec#b-$paragraph_id"
pass setup "the spec's paragraph is the block $paragraph_id ($paragraph)"

cat >"$work/verification.md" <<VERIFICATION
How $key is proved.

:::check{#c1}
The first check, which prevents the first risk.
:::

:::check{#c2 serves="$requirement"}
The second check, which serves the requirement and prevents no risk.
:::

:::check{#c3}
The third check, which a later version removes.
:::
VERIFICATION
upload setup "write the verification document with three checks" verification.md "$work/verification.md"
readonly verification="dispatch://$key/artifact/verification-md"
readonly check1="$verification#b-c1" check2="$verification#b-c2" check3="$verification#b-c3"
pass setup "wrote $verification, holding $check1, $check2 and $check3"
send GET "/api/v1/issues/$key/artifacts/verification-md/blocks"
want_status 200 setup "read the verification document's blocks"
expect setup "the verification document's blocks c1, c2 and c3 are checks" \
  '[["c1","check"],["c2","check"],["c3","check"]]' \
  '[.[] | select(.id == "c1" or .id == "c2" or .id == "c3") | [.id, .type]]'
for block in "$requirement" "$check1" "$check2" "$check3"; do
  case "$block" in
    "$requirement") document="$spec" ;;
    *) document="$verification" ;;
  esac
  references setup "read $block" to "$block"
  expect setup "$block is a block of the issue's document $document" \
    "$(jq -cnS --arg d "$document" '{document: $d, kind: "block", parent: $d, state: "live"}')" \
    '.node | {document, kind, parent: .parent.ref, state}'
done

# --- P1: the lens, its risks, the constraints and the requirement's pointer ---------------------

send POST "/api/v1/issues/$key/lenses" "$(jq -cn --arg on "$spec" --argjson a "$(actor)" '{on: $on, kind: "pre-mortem", actor: $a}')"
want_status 201 P1 "invoke a pre-mortem on the spec"
lens="$(jq -er '.address' "$work/body")" || fail P1 "invoke a pre-mortem on the spec" "no address: $(body)"
readonly lens
pass P1 "invoked the pre-mortem $lens"

write_risk P1 "A date written in two representations" "$lens" "$check1"
want_status 201 P1 "write the first risk, prevented by the first check"
risk1="$(jq -er '.address' "$work/body")" || fail P1 "write the first risk" "no address: $(body)"
readonly risk1 risk1_id="${risk1#dispatch://risk/}"
expect P1 "the first risk's write answers its lens and its check" \
  "$(jq -cn --arg l "$lens" --arg c "$check1" '{lens: $l, prevented_by: [$c]}')" '{lens, prevented_by}'

write_risk P1 "A zone dropped on the way to the store" "$lens"
want_status 201 P1 "write the second risk, prevented by no check"
risk2="$(jq -er '.address' "$work/body")" || fail P1 "write the second risk" "no address: $(body)"
readonly risk2 risk2_id="${risk2#dispatch://risk/}"
expect P1 "the second risk's write answers its lens and no check" \
  "$(jq -cn --arg l "$lens" '{lens: $l, prevented_by: []}')" '{lens, prevented_by}'

send POST "/api/v1/issues/$key/constraints" "$(jq -cn --arg s "$standard" --arg r "$reference" --argjson a "$(actor)" \
  '{standard: $s, reference: $r, actor: $a}')"
want_status 201 P1 "write the constraint carrying a standard with its reference"
standard_constraint="$(jq -er '.address' "$work/body")" || fail P1 "write the first constraint" "no address: $(body)"
readonly standard_constraint
pass P1 "wrote the constraint $standard_constraint"

send POST "/api/v1/issues/$key/constraints" "$(jq -cn --argjson a "$(actor)" \
  '{measurement: "p99 latency of one graph write", value: "41 ms", taken_where: "the staging cluster, 2026-11-12, 10 000 writes", actor: $a}')"
want_status 201 P1 "write the constraint carrying a measurement with its value and where it was taken"
measured_constraint="$(jq -er '.address' "$work/body")" || fail P1 "write the second constraint" "no address: $(body)"
readonly measured_constraint
pass P1 "wrote the constraint $measured_constraint"

send GET "/api/v1/issues/$key/constraints"
want_status 200 P1 "read the issue's constraints"
expect P1 "the issue answers both constraints, each with the fields of its shape" \
  "$(jq -cnS --arg a "$standard_constraint" --arg b "$measured_constraint" --arg s "$standard" --arg r "$reference" \
    '[{address: $a, standard: $s, reference: $r, measurement: null, value: null, taken_where: null},
      {address: $b, standard: null, reference: null, measurement: "p99 latency of one graph write", value: "41 ms",
       taken_where: "the staging cluster, 2026-11-12, 10 000 writes"}]')" \
  '[.constraints[] | {address, standard, reference, measurement, value, taken_where}]'

pointer "$key/spec#r1" "$(jq -cn --arg t "$standard_constraint" '{kind: "constrained_by", action: "append", to: $t}')"
want_status 200 P1 "point the requirement at the first constraint"
expect P1 "the pointer route answers the requirement's constrained_by tuple to the first constraint" \
  "$(jq -cn --arg n "$requirement" --arg t "$standard_constraint" '{kind: "constrained_by", node: $n, to: $t}')" \
  '{kind, node: .node.address, to: .to.address}'

# --- P2: each end of a risk ---------------------------------------------------------------------

references P2 "read the tuples out of the first risk" from "$risk1"
expect P2 "the first risk comes back with the first check as its prevented_by tuple" "[\"$check1\"]" \
  '[.edges[] | select(.kind == "prevented_by") | .node.ref]'
expect P2 "the first risk's parent is the lens" "\"$lens\"" '.node.parent.ref'
expect P2 "the first risk reaches the lens by its part_of tuple alone, no second pointer" \
  "$(jq -cn --arg l "$lens" '[["part_of", $l]]')" \
  --arg l "$lens" '[.edges[] | select(.node.ref == $l) | [.kind, .node.ref]]'

references P2 "read the lens's backlinks" to "$lens"
expect P2 "the lens's backlinks hold both of its risks" \
  "$(jq -cn --arg a "$risk1" --arg b "$risk2" '[$a, $b] | sort')" \
  '[.edges[] | select(.kind == "part_of" and .node.kind == "risk") | .node.ref] | sort'

references P2 "read the first check's backlinks" to "$check1"
expect P2 "the first check's backlinks hold the one risk" "[[\"prevented_by\",\"$risk1\"]]" \
  '[.edges[] | select(.node.kind == "risk") | [.kind, .node.ref]]'

# The absence beside its control: the read of the first risk above found its prevented_by tuple.
references P2 "read the tuples out of the second risk" from "$risk2"
expect P2 "the second risk, which points at no check, comes back prevented by nothing" '[]' \
  '[.edges[] | select(.kind == "prevented_by") | .node.ref]'
lenses_on P2 "read the pre-mortem" "$lens"
expect P2 "the pre-mortem answers its two risks, the first prevented by the first check and the second by none" \
  "$(jq -cn --arg a "$risk1" --arg b "$risk2" --arg c "$check1" '[[$a, [$c]], [$b, []]]')" \
  '[.lens.risks[] | [.address, .prevented_by]]'

# The absence beside its control: the read of the first check above found its risk.
references P2 "read the second check's backlinks" to "$check2"
expect P2 "the second check, which serves a requirement and prevents no risk, shows no risk in its backlinks" '[]' \
  '[.edges[] | select(.node.kind == "risk") | .node.ref]'
references P2 "read the tuples out of the second check" from "$check2"
expect P2 "the second check serves the requirement" "[\"$requirement\"]" \
  '[.edges[] | select(.kind == "serves") | .node.ref]'

# --- P3: the requirement and its constraint -----------------------------------------------------

references P3 "read the requirement's constrained_by tuples" from "$requirement" constrained_by
expect P3 "the requirement holds one constrained_by tuple, to the first constraint" "[\"$standard_constraint\"]" \
  '[.edges[] | .node.ref]'
far="$(jq -r '.edges[0].node.ref' "$work/body")"
send GET "/api/v1/issues/$key/constraints"
want_status 200 P3 "read the issue's constraints, the far end of the requirement's constrained_by tuple among them"
got="$(jq -r --arg a "$far" '.constraints[] | select(.address == $a) | .reference' "$work/body")"
[[ "$got" == "$reference" ]] ||
  fail P3 "the requirement reads the standard's reference back through its constrained_by tuple" \
    "the tuple's far end $far holds the reference '$got', want '$reference'"
pass P3 "the requirement reads the standard's reference back character for character through its constrained_by tuple: $got"

references P3 "read the first constraint's backlinks" to "$standard_constraint"
expect P3 "the first constraint's backlinks hold the requirement" "[[\"constrained_by\",\"$requirement\"]]" \
  '[.edges[] | [.kind, .node.ref]]'
# The absence beside its control: the same read of the first constraint found the requirement.
references P3 "read the unpointed constraint's backlinks" to "$measured_constraint"
expect P3 "the unpointed constraint shows no requirement" '[]' \
  '[.edges[] | select(.kind == "constrained_by") | .node.ref]'

# --- P4: no block type declares the store's pointers --------------------------------------------

cp "$work/catalogue.json" "$work/body"
# block_attributes: every attribute of kind block[] the schema answers, on a block type or a store
# type, as "<type>.<attribute>".
readonly block_attributes='[(.types[] | .name as $t | (.attributes // {}) | to_entries[] | select(.value.kind == "block[]") | "\($t).\(.key)"),
  (.store_types[] | .name as $t | .attributes[] | select(.kind == "block[]") | "\($t).\(.name)")]'
expect P4 "the schema read finds the block[] attribute serves on check, so it can see one" true \
  "$block_attributes | index(\"check.serves\") != null"
expect P4 "no type declares a block[] attribute named prevented_by or constrained_by" '[]' \
  "$block_attributes | map(select(endswith(\".prevented_by\") or endswith(\".constrained_by\")))"

# --- P5: the refusals ---------------------------------------------------------------------------

send POST "/api/v1/issues/$key/lenses" "$(jq -cn --arg on "$spec" --argjson a "$(actor)" '{on: $on, kind: "inversion", actor: $a}')"
want_status 201 P5 "invoke an inversion on the spec"
inversion="$(jq -er '.address' "$work/body")" || fail P5 "invoke an inversion on the spec" "no address: $(body)"
readonly inversion
pass P5 "invoked the inversion $inversion"

write_risk P5 "A risk under an inversion" "$inversion"
cp "$work/body" "$work/refusal.json"
refused_status="$status"
# What the refused write could have stored: a risk under the inversion, read the way the
# pre-mortem's risks were read above.
references P5 "read the inversion's backlinks" to "$inversion"
expect P5 "after the refusal the inversion holds no risk" '[]' \
  '[.edges[] | select(.node.kind == "risk") | .node.ref]'
lenses_on P5 "read the inversion" "$inversion"
expect P5 "the inversion answers no risk" '[]' '.lens.risks'
cp "$work/refusal.json" "$work/body"
status="$refused_status"
want_status 400 P5 "a risk under a lens of kind inversion is refused"
expect P5 "the refusal is INVALID_POINTER" '"INVALID_POINTER"' '.code'
expect P5 "its message says a risk belongs under a pre-mortem" true \
  '.error | contains("a risk belongs under a lens of kind pre-mortem")'

pointer "$key/spec#r1" "$(jq -cn --arg t "$paragraph" '{kind: "constrained_by", action: "append", to: $t}')"
cp "$work/body" "$work/refusal.json"
refused_status="$status"
references P5 "read the requirement's constrained_by tuples after the write to the paragraph" from "$requirement" constrained_by
stored="$(jq -c --arg p "$paragraph" --arg r "$requirement" \
  '[.edges[] | select(.node.ref == $p) | {kind, from: $r, to: .node.ref, block_id: .node.block_id, created_at}]' \
  "$work/body")" || fail P5 "read the requirement's constrained_by tuples" "unreadable: $(body)"
[[ "$stored" == "[]" ]] ||
  fail P5 "a constrained_by tuple from the requirement to the paragraph is refused" \
    "the tuple to the paragraph's block $paragraph_id was stored: $stored (HTTP $refused_status: $(<"$work/refusal.json"))"
expect P5 "after the refusal the requirement holds its one constrained_by tuple, to the first constraint" \
  "[\"$standard_constraint\"]" '[.edges[] | .node.ref]'
cp "$work/refusal.json" "$work/body"
status="$refused_status"
want_status 400 P5 "a constrained_by tuple from the requirement to the paragraph $paragraph_id is refused"
expect P5 "the refusal is INVALID_POINTER" '"INVALID_POINTER"' '.code'

# --- P6: a second prevented_by tuple, and a retarget --------------------------------------------

pointer "risk/$risk1_id" "$(jq -cn --arg t "$check3" '{kind: "prevented_by", action: "append", to: $t}')"
want_status 200 P6 "append a second prevented_by tuple from the first risk to the third check"
pass P6 "appended a prevented_by tuple from $risk1 to $check3"
references P6 "read the first risk's prevented_by tuples" from "$risk1" prevented_by
expect P6 "the first risk answers both checks" "$(jq -cn --arg a "$check1" --arg b "$check3" '[$a, $b] | sort')" \
  '[.edges[] | .node.ref] | sort'
for check in "$check1" "$check3"; do
  references P6 "read the backlinks of $check" to "$check"
  expect P6 "the first risk is in the backlinks of $check" "[[\"prevented_by\",\"$risk1\"]]" \
    '[.edges[] | select(.node.kind == "risk") | [.kind, .node.ref]]'
done

pointer "risk/$risk1_id" "$(jq -cn --arg f "$check1" --arg t "$check2" '{kind: "prevented_by", action: "retarget", from: $f, to: $t}')"
want_status 200 P6 "retarget the first risk's prevented_by tuple from the first check to the second through the tuple route"
expect P6 "the retarget answers the move from the first check to the second" \
  "$(jq -cn --arg f "$check1" --arg t "$check2" '{action: "retarget", from: $f, to: $t}')" \
  '{action, from: .from.address, to: .to.address}'
references P6 "read the first risk's prevented_by tuples after the retarget" from "$risk1" prevented_by
expect P6 "the first risk answers the second and the third check" \
  "$(jq -cn --arg a "$check2" --arg b "$check3" '[$a, $b] | sort')" '[.edges[] | .node.ref] | sort'
references P6 "read the first risk after the retarget" from "$risk1"
expect P6 "the first risk still sits under the lens it sat under" "\"$lens\"" '.node.parent.ref'
lenses_on P6 "read the pre-mortem after the retarget" "$lens"
expect P6 "the pre-mortem still answers the first risk among its risks" true \
  --arg r "$risk1" '[.lens.risks[].address] | index($r) != null'
references P6 "read the first check's backlinks after the retarget" to "$check1"
expect P6 "the first check no longer holds the risk in its backlinks" '[]' \
  '[.edges[] | select(.node.kind == "risk") | .node.ref]'

# --- P7: the index pass retracts the pointer at a removed block ---------------------------------

cat >"$work/verification-v2.md" <<VERIFICATION
How $key is proved.

:::check{#c1}
The first check, which prevents the first risk.
:::

:::check{#c2 serves="$requirement"}
The second check, which serves the requirement and prevents no risk.
:::
VERIFICATION
upload P7 "write version 2 of the verification document without the third check" verification.md "$work/verification-v2.md"
removed_in="$(jq -er '.version.number' "$work/body")" || fail P7 "write version 2" "no version: $(body)"
pass P7 "wrote version $removed_in of $verification, which holds no third check"

# The index pass runs in the write of the version; a server that runs it later is given 30 s.
waited=0
while true; do
  references P7 "read the first risk's prevented_by tuples after the removal" from "$risk1" prevented_by
  [[ "$(jq -c '[.edges[] | .node.ref]' "$work/body")" != "[\"$check2\"]" ]] || break
  [[ "$waited" -lt 60 ]] ||
    fail P7 "the first risk answers the second check alone" "after 30 s it answers $(jq -c '[.edges[] | .node.ref]' "$work/body")"
  sleep 0.5
  waited=$((waited + 1))
done
pass P7 "the first risk answers the second check alone"
lenses_on P7 "read the pre-mortem after the removal" "$lens"
expect P7 "the pre-mortem answers the first risk prevented by the second check alone" "[\"$check2\"]" \
  --arg r "$risk1" '.lens.risks[] | select(.address == $r) | .prevented_by'

references P7 "read the third check at the version that held it" to "$check3@v1"
expect P7 "the index pass marked the third check's block gone, removed in version $removed_in" \
  "$(jq -cn --argjson v "$removed_in" '{pin: {removed_version: $v, state: "removed"}, state: "gone"}')" \
  '.node | {state, pin: {state: .pin.state, removed_version: .pin.removed_version}}'
expect P7 "no prevented_by tuple points at the removed block" '[]' \
  '[.edges[] | select(.kind == "prevented_by") | .node.ref]'

# A tuple the index pass retracted stays retracted when the block comes back, where one a read had
# only hidden would be answered again: the block restored in a later version holds none.
upload P7 "write version 3 of the verification document, the third check restored" verification.md "$work/verification.md"
references P7 "read the restored third check's backlinks" to "$check3"
expect P7 "the restored third check is live" '"live"' '.node.state'
expect P7 "the restored third check holds no prevented_by tuple: the index pass retracted it" '[]' \
  '[.edges[] | select(.kind == "prevented_by") | .node.ref]'
references P7 "read the first risk's prevented_by tuples after the restore" from "$risk1" prevented_by
expect P7 "the first risk still answers the second check alone" "[\"$check2\"]" '[.edges[] | .node.ref]'

# --- P8: the checks of other issues of the project, and the check of another project -----------

checked_issue P8 "$project" "Pre-mortem risks check: a second issue of $project"
readonly second_check="$checked_check"
checked_issue P8 "$project" "Pre-mortem risks check: a third issue of $project"
readonly third_check="$checked_check"
[[ -n "$checked_assignee" ]] ||
  fail P8 "read the default assignee of $project" "the issue $checked_key went to nobody: $(body)"
another_project P8 "$checked_assignee"
checked_issue P8 "$other_project" "Pre-mortem risks check: an issue of another project than $project"
readonly far_check="$checked_check"
ends P8 "before any write" "" "$second_check" "$third_check" "$far_check"

pointer "risk/$risk2_id" "$(jq -cn --arg t "$second_check" '{kind: "prevented_by", action: "append", to: $t}')"
want_status 200 P8 "append a prevented_by tuple from the second risk to $second_check, a check of another issue of $project"
expect P8 "the pointer route answers the second risk's prevented_by tuple to $second_check" \
  "$(jq -cn --arg n "$risk2" --arg t "$second_check" '{action: "append", node: $n, to: $t}')" \
  '{action, node: .node.address, to: .to.address}'
ends P8 "after the append" "$second_check" "$second_check" "$third_check" "$far_check"

pointer "risk/$risk2_id" "$(jq -cn --arg f "$second_check" --arg t "$third_check" \
  '{kind: "prevented_by", action: "retarget", from: $f, to: $t}')"
want_status 200 P8 "retarget the second risk's prevented_by tuple from $second_check to $third_check, a check of a third issue"
expect P8 "the retarget answers the move from the second issue's check to the third's" \
  "$(jq -cn --arg f "$second_check" --arg t "$third_check" '{action: "retarget", from: $f, to: $t}')" \
  '{action, from: .from.address, to: .to.address}'
ends P8 "after the retarget" "$third_check" "$second_check" "$third_check" "$far_check"

pointer "risk/$risk2_id" "$(jq -cn --arg f "$third_check" --arg t "$far_check" \
  '{kind: "prevented_by", action: "retarget", from: $f, to: $t}')"
refused_far P8 "a retarget of the second risk's prevented_by tuple to $far_check, a check of $other_project," \
  "after the refused retarget" "$third_check"

pointer "risk/$risk2_id" "$(jq -cn --arg t "$third_check" '{kind: "prevented_by", action: "retract", to: $t}')"
want_status 200 P8 "retract the second risk's prevented_by tuple to $third_check"
ends P8 "after the retract" "" "$second_check" "$third_check" "$far_check"

pointer "risk/$risk2_id" "$(jq -cn --arg t "$far_check" '{kind: "prevented_by", action: "append", to: $t}')"
refused_far P8 "an append of a prevented_by tuple from the second risk to $far_check, a check of $other_project," \
  "after the refused append" ""

write_risk P8 "A risk prevented by a check of another project" "$lens" "$far_check"
cp "$work/body" "$work/refusal.json"
refused_status="$status"
references P8 "read the lens's backlinks after the write of a risk prevented by $far_check" to "$lens"
stored="$(jq -c --arg a "$risk1" --arg b "$risk2" \
  '[.edges[] | select(.kind == "part_of" and .node.kind == "risk") | .node.ref] - [$a, $b]' "$work/body")" ||
  fail P8 "read the lens's backlinks" "unreadable: $(body)"
[[ "$stored" == "[]" ]] ||
  fail P8 "a risk prevented by $far_check, a check of $other_project, is refused" \
    "the risk $stored was stored under $lens (HTTP $refused_status: $(<"$work/refusal.json"))"
expect P8 "after the refusal the lens's backlinks hold its two risks alone" \
  "$(jq -cn --arg a "$risk1" --arg b "$risk2" '[$a, $b] | sort')" \
  '[.edges[] | select(.kind == "part_of" and .node.kind == "risk") | .node.ref] | sort'
references P8 "read the backlinks of $far_check after the write of a risk prevented by it" to "$far_check"
expect P8 "after the refusal the backlinks of $far_check hold no risk" '[]' \
  '[.edges[] | select(.node.kind == "risk") | [.kind, .node.ref]]'
cp "$work/refusal.json" "$work/body"
status="$refused_status"
want_status 400 P8 "a risk prevented by $far_check, a check of $other_project, is refused"
expect P8 "the refusal is INVALID_POINTER, naming the check $far_check" \
  '{"code":"INVALID_POINTER","names_the_check":true}' --arg c "$far_check" '{code, names_the_check: (.error | contains($c))}'

pass "$driver" complete
