#!/usr/bin/env bash
# Drives the node state's five values end to end against a running server (check c-state-closed,
# spec requirement r-state-closed). As the person DEV_SIGNIN_LOGIN signs
# in, it writes the state of two nodes through POST /api/v1/issues/{key}/nodes/{id}/state, `{id}`
# the node's store address as one percent-encoded path segment: one pre-mortem lens, which holds a
# finding and a risk, through proposed, overtaken, live and gone; and one requirement of the spec
# through proposed, overtaken and back to live, after which gone is refused, since a document's own
# write removes its block. Before the lens's gone, the shared agent bearer's gone on it, a move a
# person alone makes on a lens, a finding, a risk or a constraint, since it takes every node under
# that node to gone, at every depth, is refused 403 STATE_NOT_PERMITTED naming the lens, which
# keeps its value, and the finding and the risk each read live by their own address and answer
# their words as a reference target. After it, the finding and the risk each read gone by their
# own address, through GET /api/v1/references?to= and GET /api/v1/reference-targets, neither
# answering their words; and live, proposed and overtaken on the lens are each refused 409
# NODE_GONE naming the lens, which still reads gone,
# since gone is final. It writes proposed on the requirement again, is refused settled through the
# state route, and settles it through POST .../nodes/{id}/settle. Last it writes `propsed`, one
# letter off proposed, and is refused 400 INVALID_ATTRIBUTE_VALUE naming the attribute and the
# word, the requirement still settled; and through psql it finds no tuple of the store whose far
# end is `propsed`, where the same read finds the tuples whose far end is `overtaken` this run
# wrote. After every write, accepted or refused, it reads the node back through GET
# /api/v1/references and its row of graph_nodes, so each refusal reads as the store refusing one
# value beside the values it took.
#
# Inputs: DISPATCH_URL (the server), DISPATCH_TOKEN or DISPATCH_TOKEN_FILE (an agent bearer, read
# here and never printed), PROJECT (an existing project key), DEV_SIGNIN_LOGIN (the login the
# server's dev sign-in route, /auth/_dev/signin, signs in: `alice` on `bun run dispatch:local`) and
# STORE_DSN (the server's own database, which psql reads; never DATABASE_URL, which a local
# instance empties). Every state write but the bearer's refused gone, the lens, its finding and its
# risk are that person's, made with the cookie the dev sign-in sets and the server's origin; the
# agent bearer writes the issue, makes that one refused write and closes the issue when the run
# ends, also when a check fails. Each unmet input is exit 2.
#
# The first line is `PASS: node-state-values.sh the catalogue holds its ten kinds`, read before any
# lens is written. Each passing check prints `PASS: <section> <what>`; the first failing one prints
# `FAIL: <section> <what>: <observed>` and the script exits 1. The last line is
# `PASS: node-state-values.sh complete`.
#
# Every check is `cmd || fail …`, never part of an `&&` list: under `set -e` a failing command
# anywhere but last in such a list does not stop the script.

# shellcheck disable=SC2016 # the single-quoted jq filters name jq variables, not shell ones
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DISPATCH_URL=http://127.0.0.1:8777 DISPATCH_TOKEN=e2e-token PROJECT=CORE \
         DEV_SIGNIN_LOGIN=alice STORE_DSN=postgres://…/node_states?sslmode=disable node-state-values.sh
       (DISPATCH_TOKEN_FILE=<file holding the bearer> in place of DISPATCH_TOKEN; the server must
       mount its dev sign-in route, as `bun run dispatch:local` does, and STORE_DSN must name the
       database that server serves)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
require_env DISPATCH_URL PROJECT DEV_SIGNIN_LOGIN STORE_DSN
require_commands curl jq psql
read_bearer

readonly driver=node-state-values.sh
# shellcheck disable=SC2031 # the DISPATCH_URL lib.sh's `cli` exports is its own; this one is the caller's
readonly base="${DISPATCH_URL%/}"
# shellcheck disable=SC2153 # PROJECT is the caller's input, checked by name above
readonly project="$PROJECT"
readonly login="$DEV_SIGNIN_LOGIN"
# The store lib.sh's `query` reads.
readonly database_url="$STORE_DSN"
stamp="$(date +%s)-$$"
readonly stamp
readonly session="node-state-values-$stamp"
# The word one letter off proposed, which the node state does not take.
readonly misspelled=propsed
# The words of the lens's finding and of its risk's heading, which each answers by its own address
# while the lens is live and neither answers once it is gone.
readonly finding_words="The cache answers a read past its minute." risk_words="A stale read reaches a writer"

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

# write_state NODE STATE [AS]: the person writes STATE on the node whose store address is NODE (POST
# /api/v1/issues/{key}/nodes/{id}/state), or the shared agent bearer, naming this run's session,
# where AS is `agent`; the answer left as it came.
write_state() {
  local path
  path="/api/v1/issues/$key/nodes/$(q "$1")/state"
  if [[ "${3:-}" == agent ]]; then
    send POST "$path" "$(jq -cn --arg s "$2" --argjson a "$(actor)" '{state: $s, actor: $a}')"
  else
    person_send POST "$path" "$(jq -cn --arg s "$2" '{state: $s}')"
  fi
}

# held SECTION WHAT ADDRESS KIND ID WANT: reads the node at ADDRESS back, whose row of graph_nodes
# is keyed KIND and ID: the state the person's node read answers (GET
# /api/v1/references?from=ADDRESS), or the status and body where it answers none, and the state
# the row holds, which $stored_state keeps. A gone node is in no read, so for WANT gone the row
# alone is read. $seen says what each read found. It answers whether each is WANT.
held() {
  local section="$1" what="$2" address="$3" kind="$4" id="$5" want="$6" read_state=""
  if [[ "$want" != gone ]]; then
    person_send GET "/api/v1/references?from=$(q "$address")"
    if [[ "$status" == 200 ]]; then
      read_state="$(jq -r '.node.state' "$work/body")"
    else
      read_state="HTTP $status $(body)"
    fi
  fi
  query "$section" "$what" \
    "select coalesce((select state from graph_nodes where kind = '$kind' and id = '$id'), '(no row)')"
  stored_state="$answer"
  if [[ "$want" == gone ]]; then
    seen="its row holds $stored_state, and no read is asked of it, since a gone node is in none"
  else
    seen="the node read answers $read_state and its row holds $stored_state"
  fi
  [[ "$stored_state" == "$want" ]] || return 1
  [[ "$want" == gone || "$read_state" == "$want" ]]
}

# reads SECTION ADDRESS KIND ID WANT: the node at ADDRESS reads back WANT, through the read and its
# row.
reads() {
  held "$1" "read $2 back" "$2" "$3" "$4" "$5" || fail "$1" "$2 reads back $5" "$seen"
  pass "$1" "$2 reads back $5: $seen"
}

# moves SECTION NODE ADDRESS KIND ID FROM TO: the person writes TO on the node whose store address
# is NODE, at ADDRESS; the write answers it moved from FROM, and the node reads back TO.
moves() {
  write_state "$2" "$7"
  want_status 200 "$1" "write $7 on $3"
  expect "$1" "the write of $7 on $3 answers it moved from $6 to $7" \
    "$(jq -cnS --arg a "$3" --arg f "$6" --arg t "$7" '{address: $a, previous: $f, state: $t}')" \
    '{address: .node.address, previous, state: .node.state}'
  reads "$1" "$3" "$4" "$5" "$7"
}

# refused SECTION NODE ADDRESS KIND ID ASKED HELD STATUS CODE [AS]: the person, or the shared agent
# bearer where AS is `agent`, writes ASKED on the node whose store address is NODE, at ADDRESS,
# which holds HELD; the node still reads HELD, and the write was refused with HTTP STATUS and the
# code CODE. The node is read back before the answer, so a value the route stored is named with the
# node it was stored on. The refusal is left in $work/body.
refused() {
  write_state "$2" "$6" "${10:-}"
  cp "$work/body" "$work/refusal.json"
  refused_status="$status"
  held "$1" "read $3 back after a write of $6" "$3" "$4" "$5" "$7" ||
    fail "$1" "a write of $6 on $3 is refused, and the node stays $7" \
      "the value $6 was stored on $3: the write answered HTTP $refused_status $(<"$work/refusal.json"), and $seen; want HTTP $8 $9 and the node at $7"
  pass "$1" "$3 still reads $7 after a write of $6: $seen"
  cp "$work/refusal.json" "$work/body"
  status="$refused_status"
  want_status "$8" "$1" "a write of $6 on $3 is refused with HTTP $8"
  pass "$1" "a write of $6 on $3 is refused with HTTP $8"
  expect "$1" "the refusal of $6 on $3 is $9" "\"$9\"" '.code'
}

# names SECTION WHAT ID: the refusal in $work/body names the node whose id is ID.
names() {
  expect "$1" "$2" true --arg id "$3" '.error | contains($id)'
}

# answers SECTION ADDRESS WORDS: the node at ADDRESS reads live by its own address, through GET
# /api/v1/references?to=, which answers a finding or a risk with no text, and is a reference target
# GET /api/v1/reference-targets answers not gone and named by WORDS.
answers() {
  person_send GET "/api/v1/references?to=$(q "$2")"
  want_status 200 "$1" "read $2 by its own address"
  expect "$1" "$2 reads live by its own address" '"live"' '.node.state'
  person_send GET "/api/v1/reference-targets?ref=$(q "$2")"
  want_status 200 "$1" "read $2 as a reference target"
  expect "$1" "$2 is a reference target that is not gone, named by its words" true \
    --arg w "$3" '.targets | length == 1 and (.[0].gone | not) and (.[0].name | contains($w))'
}

# gone_by_address SECTION ADDRESS KIND ID WORDS: the node at ADDRESS, whose row of graph_nodes is
# keyed KIND and ID, reads gone by its own address: GET /api/v1/references?to= answers it 404, as
# an address nobody holds, GET /api/v1/reference-targets answers it gone and naming nothing,
# neither answers WORDS, and its row holds gone.
gone_by_address() {
  local section="$1" address="$2" kind="$3" id="$4" words="$5" read
  person_send GET "/api/v1/references?to=$(q "$address")"
  if [[ "$status" == 200 ]]; then
    read="HTTP 200 with the node at state $(jq -r '.node.state' "$work/body")"
  else
    read="HTTP $status $(body)"
  fi
  [[ "$status" == 404 ]] ||
    fail "$section" "$address reads gone by its own address once its lens is gone" \
      "GET /api/v1/references?to= answers $read; want HTTP 404, as an address nobody holds"
  ! grep -qF "$words" "$work/body" ||
    fail "$section" "GET /api/v1/references?to=$address answers none of its words" "it answers: $(body)"
  pass "$section" "$address reads gone by its own address: GET /api/v1/references?to= answers $read, none of its words"
  person_send GET "/api/v1/reference-targets?ref=$(q "$address")"
  want_status 200 "$section" "read $address as a reference target"
  ! grep -qF "$words" "$work/body" ||
    fail "$section" "GET /api/v1/reference-targets answers none of the words of $address" "it answers: $(body)"
  expect "$section" "$address is a reference target that is gone and names nothing" '{"gone":true,"name":""}' \
    '.targets[0] | {gone, name: (.name // "")}'
  reads "$section" "$address" "$kind" "$id" gone
}

# --- the catalogue ------------------------------------------------------------------------------

seeded_catalogue

# --- setup --------------------------------------------------------------------------------------

sign_in setup "$login"

cat >"$work/spec.md" <<'SPEC'
The spec whose requirement and pre-mortem this run writes the state of.

:::requirement{#r1}
A node's state is one of the five values it takes.
:::
SPEC
send POST /api/v1/issues "$(jq -cn --arg p "$project" --rawfile s "$work/spec.md" --arg sn "$session" \
  '{project: $p, title: "Node state values check", spec: $s, force: true, actor: {kind: "session", id: $sn}}')"
want_status 201 setup "create the issue"
key="$(jq -er '.key' "$work/body")" || fail setup "create the issue" "no key: $(body)"
# The key and the ids below go into the SQL this driver reads the store with, so each is held to
# its shape first.
[[ "$key" =~ ^[A-Z][A-Z0-9]*-[0-9]+$ ]] || fail setup "create the issue" "the key $key is no issue key"
readonly spec="dispatch://$key/spec"
readonly requirement="$spec#b-r1" requirement_node="$key/spec#r1"
pass setup "created $key in $project, whose spec holds the requirement $requirement"
send GET "/api/v1/issues/$key/artifacts/spec/blocks"
want_status 200 setup "read the spec's blocks"
expect setup "the spec's block r1 is a requirement" '["requirement"]' '[.[] | select(.id == "r1") | .type]'

# A risk stands only under a pre-mortem lens, so the lens is one.
person_send POST "/api/v1/issues/$key/lenses" \
  "$(jq -cn --arg on "$spec" --arg f "$finding_words" '{on: $on, kind: "pre-mortem", findings: [$f]}')"
want_status 201 setup "invoke a pre-mortem on the spec, holding a finding"
lens_id="$(jq -er '.id' "$work/body")" || fail setup "invoke a pre-mortem on the spec" "no id: $(body)"
[[ "$lens_id" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$ ]] ||
  fail setup "invoke a pre-mortem on the spec" "the lens id $lens_id is no uuid"
readonly lens_id lens="dispatch://lens/$lens_id" lens_node="lens/$lens_id"
finding="$(jq -er '.findings[0].address' "$work/body")" || fail setup "invoke a pre-mortem on the spec" "no finding: $(body)"
finding_node="$(jq -er '.findings[0].id' "$work/body")" || fail setup "invoke a pre-mortem on the spec" "no finding id: $(body)"
[[ "$finding_node" =~ ^lens/$lens_id#[A-Za-z0-9_-]+$ ]] ||
  fail setup "invoke a pre-mortem on the spec" "the finding id $finding_node is no block id of the lens"
readonly finding finding_node
pass setup "invoked the pre-mortem $lens on the spec, holding the finding $finding"
person_send POST "/api/v1/issues/$key/risks" "$(jq -cn --arg l "$lens" --arg h "$risk_words" \
  '{lens: $l, heading: $h, early_warning: "A writer reads a value older than a minute", prevention: "Each write evicts what it changed"}')"
want_status 201 setup "write a risk under the pre-mortem"
risk_id="$(jq -er '.id' "$work/body")" || fail setup "write a risk under the pre-mortem" "no id: $(body)"
[[ "$risk_id" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$ ]] ||
  fail setup "write a risk under the pre-mortem" "the risk id $risk_id is no uuid"
readonly risk_id risk="dispatch://risk/$risk_id"
pass setup "wrote the risk $risk under $lens"

# Each node's row read through STORE_DSN, live: the database psql reads is the server's.
reads setup "$requirement" block "$requirement_node" live
reads setup "$lens" lens "$lens_id" live
reads setup "$finding" block "$finding_node" live
reads setup "$risk" risk "$risk_id" live
answers setup "$finding" "$finding_words"
answers setup "$risk" "$risk_words"

# --- L1: a lens through proposed, overtaken and live --------------------------------------------

moves L1 "$lens_node" "$lens" lens "$lens_id" live proposed
moves L1 "$lens_node" "$lens" lens "$lens_id" proposed overtaken
moves L1 "$lens_node" "$lens" lens "$lens_id" overtaken live

# --- B1: a lens's gone is a person's alone, so the shared agent bearer is refused it ------------

refused B1 "$lens_node" "$lens" lens "$lens_id" gone live 403 STATE_NOT_PERMITTED agent
names B1 "the refusal of the bearer's gone names the lens $lens_id" "$lens_id"
reads B1 "$finding" block "$finding_node" live
reads B1 "$risk" risk "$risk_id" live

# --- G1: the person takes the lens to gone, and its finding and its risk with it ----------------

moves G1 "$lens_node" "$lens" lens "$lens_id" live gone
gone_by_address G1 "$finding" block "$finding_node" "$finding_words"
gone_by_address G1 "$risk" risk "$risk_id" "$risk_words"

# --- L2: gone is final, so a gone lens takes no value -------------------------------------------

for asked in live proposed overtaken; do
  refused L2 "$lens_node" "$lens" lens "$lens_id" "$asked" gone 409 NODE_GONE
  names L2 "the refusal of $asked on the gone lens names the lens $lens_id" "$lens_id"
done

# --- R1: a requirement through proposed, overtaken and back to live -----------------------------

moves R1 "$requirement_node" "$requirement" block "$requirement_node" live proposed
moves R1 "$requirement_node" "$requirement" block "$requirement_node" proposed overtaken
moves R1 "$requirement_node" "$requirement" block "$requirement_node" overtaken live

# --- R2: a document's own write removes its block, so gone is refused on the requirement --------

refused R2 "$requirement_node" "$requirement" block "$requirement_node" gone live 409 NODE_STATE_NOT_WRITABLE

# --- S1: settled is the settle's alone ----------------------------------------------------------

moves S1 "$requirement_node" "$requirement" block "$requirement_node" live proposed
refused S1 "$requirement_node" "$requirement" block "$requirement_node" settled proposed 409 NODE_STATE_NOT_WRITABLE
person_send POST "/api/v1/issues/$key/nodes/$(q "$requirement_node")/settle"
want_status 200 S1 "$login settles $requirement through /settle"
expect S1 "the settle answers $requirement moved from proposed to settled" \
  "$(jq -cnS --arg a "$requirement" '{address: $a, previous: "proposed", state: "settled"}')" \
  '{address: .node.address, previous, state: .node.state}'
reads S1 "$requirement" block "$requirement_node" settled

# --- V1: a word one letter off proposed is no value of the node state ---------------------------

refused V1 "$requirement_node" "$requirement" block "$requirement_node" "$misspelled" settled 400 INVALID_ATTRIBUTE_VALUE
expect V1 "the refusal names the attribute state and the value $misspelled" true \
  --arg w "$misspelled" '.error | contains("the node state \"" + $w + "\" is none of")'
pass V1 "the refusal reads: $(jq -r '.error' "$work/body")"

# --- T1: the store holds no tuple whose far end is the misspelling ------------------------------

# One read of graph_tuples for the two far ends, over the whole store: the tuples it finds at
# overtaken, this run's among them, show it can see a state tuple at all.
query T1 "read the tuples whose far end is $misspelled or overtaken" \
  "select coalesce(string_agg(to_id || ' ' || kind || ' ' || from_kind || ' ' || from_id, E'\n' order by seq), '')
     from graph_tuples where to_id in ('$misspelled', 'overtaken')"
tuples="$answer"
stored=""
while IFS= read -r tuple; do
  [[ "$tuple" != "$misspelled "* ]] || stored+="${stored:+; }$tuple"
done <<<"$tuples"
for written in "state block $requirement_node" "state lens $lens_id"; do
  grep -qxF "overtaken $written" <<<"$tuples" ||
    fail T1 "the read finds the state tuple this run wrote at overtaken, $written" \
      "the read of the far ends $misspelled and overtaken found: ${tuples:-no tuple}"
  pass T1 "the read finds the tuple whose far end is overtaken, $written"
done
[[ -z "$stored" ]] || fail T1 "no tuple of the store has the far end $misspelled" "the same read found: $stored"
pass T1 "the same read finds no tuple whose far end is $misspelled"

pass "$driver" complete
