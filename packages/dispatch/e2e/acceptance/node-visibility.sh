#!/usr/bin/env bash
# Drives a node reserved for its author through every route that can disclose it, against a running
# server (dispatch://INFRA-1950's c-visibility-every-route, dispatch://INFRA-1950/spec#b-r-node-visibility).
# A direct read refused proves nothing of the graph's other routes, so the driver hangs the hidden
# nodes among public neighbours. On one issue the first person (DEV_SIGNIN_LOGIN, `alice`) writes,
# under a spec every person reads, a public trade-off analysis that triggered a pre-mortem reserved
# for her, which triggered a public inversion, so a chain of three runs through it; a risk reserved
# for her under that pre-mortem, prevented by a public check of the issue's verification document;
# and a public comment written under the run of a question she asked the command bar (the
# Dispatch-Going-Run header), so a changed_by_run tuple runs from the comment to her interaction. A
# second issue, the twin, is seeded the same way without the hidden nodes: no pre-mortem, no risk,
# and its comment written under no run.
#
# The second person, bob, owns none of it and reads every surface that can disclose one of these
# kinds, on both issues: the chain read of the spec (GET /api/v1/lenses?on=) first, with each chain
# it walks; the list of each kind, the lens read's lenses and each lens's risks and the person page
# of the first person (GET /api/v1/people/{login}/nodes), which lists what a person wrote of every
# kind, a run of the command bar to the credential that asked it alone; the read of each hidden
# node's own address, with his cookie and with a personal token of his own; GET /api/v1/references
# in both directions on each public neighbour, with the backlink count beside it, and the issue's
# own counts; the issue's event list and the whole event log (GET /api/v1/events/log); and
# GET /api/v1/search for each hidden node's words, which answers no hit and a total of zero to both
# people. Each answer on the issue names no hidden node and carries none of its words, each count
# he reads on it equals the count he reads on the twin, and each answer equals the twin's whole once
# every id, time, seq and rank is replaced by its place. One public neighbour has no twin: the
# catalogue's dispatch://lens_kind/pre-mortem, the far end of the pre-mortem's has_kind tuple, which
# both issues share, so his reads of it are held to the same reads he made before the pre-mortem
# was written. The first person repeats each surface that holds these kinds and finds the node in
# each, beside his reads, so each absence stands beside the same read finding the node; her
# personal token finds her pre-mortem and her risk at their addresses and reads her run as an
# address nobody wrote, since a run is read by the whole credential that asked it, her cookie. One
# read finds her run for nobody, her cookie included: the lens read of it (GET /api/v1/lenses?on=).
# A run stands under no issue and no lens stands on it (plan P9), and that read answers a node of a
# kind no lens stands on the 404 an address nobody wrote answers before it reads the store
# (lensStandsOnKinds), so it answers every reader of her run that address's answer, byte for byte
# once the id is substituted, as it answers bob. The event list, the log and search hold none of
# them for anybody, so each absence there stands beside the same read finding a public comment. The
# issue tree, the inbox, the coverage read and a version read can hold none of these kinds, and no
# route walks up from a risk, so the driver reads none of them.
#
# His read of a hidden node's own address answers the status, the Content-Type and the body bytes
# (never Date) an address of the same form nobody ever wrote answers, once the id is substituted. The
# tuple whose near end is hidden and whose far end is not, the pre-mortem's triggered_by tuple to the
# trade-off analysis, is absent from both ends, and the trade-off analysis counts the lenses it
# triggered as it did before the pre-mortem was written. A third person who has written nothing
# writes a lens reserved for them, and bob reads their page before and after that write as a login
# nobody holds, while they find the lens on their own page. Last the first person writes `gone` on the
# hidden pre-mortem, and bob's read of its address is byte for byte the one he read before.
#
# Inputs: DISPATCH_URL (the server), DISPATCH_TOKEN or DISPATCH_TOKEN_FILE (an agent bearer, read
# here and never printed), PROJECT (an existing project key) and DEV_SIGNIN_LOGIN (the first person,
# whom the server's dev sign-in route, /auth/_dev/signin, signs in: `alice` on `bun run
# dispatch:local`); the second person is bob and the third a login of this run's own, each signed in
# through the same route. Every lens, risk, run and comment is a person's write with the cookie the
# dev sign-in sets and the server's origin; the first and the second person each mint a personal
# token with that cookie (POST /api/v1/me/agent-tokens), which the run revokes when it ends. The
# agent bearer writes both issues and the verification documents, and closes both issues when the
# run ends, also when a check fails. Each unmet input is exit 2.
#
# The first line is `PASS: node-visibility.sh the catalogue holds its ten kinds`, read before any
# lens is written. Each passing check prints `PASS: <section> <what>`; the first failing one prints
# `FAIL: <section> <what>: <observed>` and the script exits 1. The last line is
# `PASS: node-visibility.sh complete`.
#
# Every check is `cmd || fail …`, never part of an `&&` list: under `set -e` a failing command
# anywhere but last in such a list does not stop the script.

# shellcheck disable=SC2016 # the single-quoted jq filters name jq variables, not shell ones
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DISPATCH_URL=http://127.0.0.1:8777 DISPATCH_TOKEN=e2e-token PROJECT=CORE \
         DEV_SIGNIN_LOGIN=alice node-visibility.sh
       (DISPATCH_TOKEN_FILE=<file holding the bearer> in place of DISPATCH_TOKEN; the server must
       mount its dev sign-in route, as `bun run dispatch:local` does, for DEV_SIGNIN_LOGIN, for bob
       and for a third login of the run's own)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
require_env DISPATCH_URL PROJECT DEV_SIGNIN_LOGIN
require_commands cmp curl jq sed tr
read_bearer

readonly driver=node-visibility.sh
# shellcheck disable=SC2031 # the DISPATCH_URL lib.sh's `cli` exports is its own; this one is the caller's
readonly base="${DISPATCH_URL%/}"
# shellcheck disable=SC2153 # PROJECT is the caller's input, checked by name above
readonly project="$PROJECT"
stamp="$(date +%s)-$$"
readonly stamp
# The three people: the one DEV_SIGNIN_LOGIN names, who writes every hidden node; bob, who reads;
# and a person of this run's own, who has written nothing until this run signs them in.
readonly first="$DEV_SIGNIN_LOGIN" second=bob third="node-visibility-$stamp"
[[ "$first" != "$second" ]] || {
  printf 'DEV_SIGNIN_LOGIN is %s, the second person this driver signs in; name another login\n' "$second" >&2
  usage
  exit 2
}
# A login nobody holds, never signed in: the page a person's page is held to.
readonly nobody_login="nobody-$stamp"
readonly session="node-visibility-$stamp"
# The words only one node of this run carries, letters alone so search reads each as one word.
letters="$(tr '0-9' 'a-j' <<<"${stamp//-/}")"
readonly letters
readonly hidden_word="veiled$letters" risk_word="shrouded$letters" run_word="unseen$letters" public_word="open$letters"
# An id nobody ever wrote: the address of each form a hidden node's address is held to.
readonly missing="00000000-0000-4000-8000-000000000000"
# The catalogue's kind of the hidden pre-mortem, the far end of its has_kind tuple: a public
# neighbour every issue shares, so it has no twin.
readonly lens_kind="dispatch://lens_kind/pre-mortem"
# canon: an answer with each id replaced by its place among the ids it names, in the order it names
# them, the issue key $key by <key>, and each time, seq and rank by its name, so two answers the same
# writes made on two issues read equal where they hold the same nodes and tuples.
readonly canon='([.. | strings | match("[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}"; "g") | .string]
    | reduce .[] as $u ([]; if any(.[]; . == $u) then . else . + [$u] end)) as $ids
  | walk(if type == "object" then with_entries(
      if (.key | test("_at$|^last_seen$|^source_seq$|^rank$")) and .value != null then .value = "<\(.key)>" else . end)
    else . end)
  | walk(if type == "string" then reduce range($ids | length) as $i (.; gsub($ids[$i]; "<id \($i)>")) | gsub($key + "\\b"; "<key>")
    else . end)'
# The counts each read answers.
readonly lens_counts='{lenses: (.lenses | length), chains: [.chains[] | .lenses | length],
  each: [(if .lens then [.lens] else [] end), .lenses | .[]
    | {triggered: (.triggered | length), triggered_by: (.triggered_by | length), findings: (.findings | length), risks: (.risks | length)}]}'
readonly edge_counts='{edges: (.edges | length), kinds: ([.edges[].kind] | sort)}'
readonly issue_counts='{referenced_by_count, last_seq, artifacts: (.artifacts | length), open_asks: (.open_asks | length),
  children: (.children | length)}'

work_dir
printf 'Authorization: Bearer %s\n' "$token" >"$work/auth"

issues=()
cleanup() {
  local code=$? issue
  revoke_tokens
  for issue in "${issues[@]}"; do
    close_issue "$issue"
  done
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# keep NAME: the last answer kept as $work/NAME.json, and its status and Content-Type as
# $work/NAME.head.
keep() {
  cp "$work/body" "$work/$1.json"
  printf '%s %s' "$status" "$content_type" >"$work/$1.head"
}

# lens_body KIND VERDICT FINDING [TRIGGER [VISIBILITY]]: the body of a lens of KIND on $spec with
# VERDICT and the one finding FINDING, triggered by TRIGGER and at VISIBILITY where each is given.
lens_body() {
  jq -cn --arg on "$spec" --arg k "$1" --arg v "$2" --arg f "$3" --arg t "${4:-}" --arg vis "${5:-}" \
    '{on: $on, kind: $k, verdict: $v, findings: [$f]}
     + (if $t == "" then {} else {triggered_by: $t} end) + (if $vis == "" then {} else {visibility: $vis} end)'
}

# seed WITH: writes one issue of PROJECT as the agent bearer, with a spec and a verification document
# holding one check, and as the first person a trade-off analysis and an inversion on the spec, each
# with one finding, and a comment on the issue, all three public. WITH=hidden also writes, as
# the first person, the pre-mortem reserved for her between the two lenses, triggered by the
# trade-off analysis and triggering the inversion, with one finding; the risk reserved for her under
# it, prevented by the check; and one question of the command bar, under whose run the comment is
# written. Before the pre-mortem is written, bob reads what the trade-off analysis answers of the
# lenses it triggered, and the tuples in both directions at $lens_kind, the pre-mortem's has_kind
# far end, which no twin stands for. It sets $key, $spec, $check, $tradeoff, $inversion and
# $comment_id, and with WITH=hidden $premortem, $finding, $risk and $run_id.
seed() {
  local section="setup:$1" document trigger="" read
  request agent POST /api/v1/issues "$(jq -cn --arg p "$project" --arg s "$spec_text" --argjson a "$(actor)" \
    '{project: $p, title: "Node visibility check", force: true, actor: $a, spec: $s}')"
  want_status 201 "$section" "create the issue"
  key="$(jq -er '.key' "$work/body")" || fail "$section" "create the issue" "no key: $(body)"
  issues+=("$key")
  spec="dispatch://$key/spec"
  document="$(printf ':::check{#c1}\nThe check of %s that prevents the risk.\n:::\n' "$project")"
  request agent POST "/api/v1/issues/$key/artifacts" "$(jq -cn --arg c "$document" --argjson a "$(actor)" \
    '{name: "verification.md", content: $c, actor: $a}')"
  want_status 201 "$section" "write the verification document of $key"
  check="dispatch://$key/artifact/verification-md#b-c1"
  pass "$section" "created $key in $project, whose spec is $spec and whose verification document holds the check $check"

  invoke "$section" "$first invokes a public trade-off analysis on the spec" "cookie:$first" \
    "$(lens_body trade-off 'The verdict of the public trade-off analysis.' 'A finding of the public trade-off analysis.')"
  tradeoff="$lens"

  if [[ "$1" == hidden ]]; then
    request "cookie:$second" GET "/api/v1/lenses?on=$(q "$tradeoff")"
    want_status 200 "$section" "$second reads the trade-off analysis before the pre-mortem is written"
    keep before.lenses
    request "cookie:$second" GET "/api/v1/references?to=$(q "$tradeoff")&kind=triggered_by"
    want_status 200 "$section" "$second reads the triggered_by tuples into the trade-off analysis before the pre-mortem is written"
    keep before.triggered
    for read in to from; do
      request "cookie:$second" GET "/api/v1/references?$read=$(q "$lens_kind")"
      want_status 200 "$section" "$second reads the tuples $read $lens_kind before the pre-mortem is written"
      keep "before.kind.$read"
    done
    pass "$section" "before the pre-mortem is written, the trade-off analysis counts $(jq -r '.lens.triggered | length' "$work/before.lenses.json") lenses it triggered and $(jq -r '.edges | length' "$work/before.triggered.json") triggered_by tuples into it, read as $second"

    invoke "$section" "$first invokes the pre-mortem reserved for her, triggered by the trade-off analysis" "cookie:$first" \
      "$(lens_body pre-mortem "The verdict only the hidden lens carries: $hidden_word." \
        "A finding of the hidden lens: $hidden_word." "$tradeoff" author)"
    premortem="$lens"
    cp "$work/invoked" "$work/body"
    expect "$section" "the pre-mortem is reserved for $first, triggered by the trade-off analysis" \
      "$(jq -cnS --arg t "$tradeoff" --arg l "$first" \
        '{author: {id: $l, kind: "person"}, finding: "author", triggered_by: $t, visibility: "author"}')" \
      '{author, visibility, triggered_by, finding: .findings[0].visibility}'
    finding="$(jq -er '.findings[0].address' "$work/invoked")" || fail "$section" "read the pre-mortem's finding" "$(body)"
  fi

  [[ "$1" != hidden ]] || trigger="$premortem"
  invoke "$section" "$first invokes a public inversion on the spec${trigger:+, triggered by the pre-mortem}" "cookie:$first" \
    "$(lens_body inversion 'The verdict of the public inversion.' 'A finding of the public inversion.' "$trigger")"
  inversion="$lens"

  if [[ "$1" == hidden ]]; then
    request "cookie:$first" POST "/api/v1/issues/$key/risks" "$(jq -cn --arg l "$premortem" --arg c "$check" --arg w "$risk_word" \
      '{lens: $l, heading: "A risk only its author reads: \($w)", early_warning: "The first sign of \($w)",
        prevention: "What keeps \($w) from happening", prevented_by: $c, visibility: "author"}')"
    want_status 201 "$section" "$first writes the risk reserved for her under the pre-mortem, prevented by the check"
    risk="$(jq -er '.address' "$work/body")" || fail "$section" "write the risk" "no address: $(body)"
    expect "$section" "the risk is reserved for $first, under the pre-mortem and prevented by the public check $check" \
      "$(jq -cnS --arg p "$premortem" --arg c "$check" '{lens: $p, prevented_by: [$c], visibility: "author"}')" \
      '{lens, prevented_by, visibility}'

    request "cookie:$first" POST /api/v1/going/requests "$(jq -cn --arg p "$project" --arg w "$run_word" \
      '{words: "Which run changed the comment, \($w)?", project: $p}')"
    want_status 201 "$section" "$first asks the command bar one question"
    run_id="$(jq -er '.id' "$work/body")" || fail "$section" "ask the command bar" "no id: $(body)"
    pass "$section" "$first's run $run_id is live until $(jq -r '.ends_at' "$work/body")"
    # The write that must land inside the run's 30 s window: the comment under the run's header.
    # The server admits a header only for a run still live (withinGoingRun), and this driver sets
    # no ends_at far ahead (its run string gives no STORE_DSN), so it writes the comment straight
    # after the run is asked; a window that closed first is answered as a run nobody holds, and
    # the status check below fails naming the write.
    request "cookie:$first" POST "/api/v1/issues/$key/comments" "$comment_body" "$run_id"
    want_status 201 "$section" "$first writes a public comment on $key under her run, named in the Dispatch-Going-Run header"
  else
    request "cookie:$first" POST "/api/v1/issues/$key/comments" "$comment_body"
    want_status 201 "$section" "$first writes the same public comment on $key under no run"
  fi
  comment_id="$(jq -er '.id' "$work/body")" || fail "$section" "write the comment" "no id: $(body)"
  pass "$section" "the comment is dispatch://$key/comment/$comment_id"
}

# leaks FILE: the strings of the answer FILE that name a hidden node or carry one of its words, as a
# JSON array.
leaks() {
  jq -c --slurpfile hidden "$work/hidden.json" \
    '[.. | strings | select(. as $s | any($hidden[0][]; . as $h | $s | contains($h)))] | unique' "$1"
}

# no_leak SECTION WHAT FILE: the answer FILE names no hidden node and carries none of its words, no
# excerpt included.
no_leak() {
  local got
  got="$(leaks "$3")" || fail "$1" "$2" "unreadable: $(head -c 600 "$3")"
  [[ "$got" == "[]" ]] || fail "$1" "$2" "the answer holds $got: $(head -c 1200 "$3")"
  pass "$1" "$2"
}

# differ_at A B NAME-A NAME-B: the first five paths at which the JSON answers A and B hold
# different values, each with the value of each under NAME-A and NAME-B.
differ_at() {
  jq -cn --argjson a "$1" --argjson b "$2" --arg na "$3" --arg nb "$4" \
    '[($a | paths(scalars)), ($b | paths(scalars))] | unique
     | map(select(($a | getpath(.)) != ($b | getpath(.)))) | .[:5]
     | map({at: ., ($na): ($a | getpath(.)), ($nb): ($b | getpath(.))})'
}

# twin SECTION WHAT COUNTS PATH TWIN-PATH [KEEP]: bob reads PATH on the issue and TWIN-PATH on the
# twin, each answered 200, kept as $work/bob.json and $work/bob.twin.json. The answer on the issue
# names no hidden node and carries none of their words; each count COUNTS reads of it equals that
# count on the twin; and the two answers, each as the filter KEEP (default `.`) keeps it, are equal
# whole once each id, time, seq and rank is replaced by its place (canon).
twin() {
  local section="$1" what="$2" counts="$3" keep_filter="${6:-.}" got want
  request "cookie:$second" GET "$4"
  want_status 200 "$section" "$second reads $what on $key"
  cp "$work/body" "$work/bob.json"
  request "cookie:$second" GET "$5"
  want_status 200 "$section" "$second reads $what on the twin $twin_key"
  cp "$work/body" "$work/bob.twin.json"
  no_leak "$section" "$second's read of $what on $key names no hidden node and carries none of their words, in no excerpt either" \
    "$work/bob.json"
  got="$(jq -cS "$counts" "$work/bob.json")" || fail "$section" "count $what on $key" "unreadable: $(head -c 600 "$work/bob.json")"
  want="$(jq -cS "$counts" "$work/bob.twin.json")" || fail "$section" "count $what on $twin_key" "unreadable: $(head -c 600 "$work/bob.twin.json")"
  [[ "$got" == "$want" ]] ||
    fail "$section" "each count $second reads in $what on $key is the count on $twin_key, which holds no hidden node" \
      "on $key he reads $got, on $twin_key $want"
  pass "$section" "each count $second reads in $what on $key is the count on $twin_key, which holds no hidden node: $got"
  got="$(jq -cS --arg key "$key" "$keep_filter | $canon" "$work/bob.json")" ||
    fail "$section" "put $second's read of $what on $key in canonical form" "unreadable: $(head -c 600 "$work/bob.json")"
  want="$(jq -cS --arg key "$twin_key" "$keep_filter | $canon" "$work/bob.twin.json")" ||
    fail "$section" "put $second's read of $what on $twin_key in canonical form" "unreadable: $(head -c 600 "$work/bob.twin.json")"
  [[ "$got" == "$want" ]] ||
    fail "$section" "$second's read of $what on $key is the read on $twin_key whole, once each id, time, seq and rank is replaced by its place" \
      "the two differ at $(differ_at "$got" "$want" on_issue on_twin); on $key he reads $got"
  pass "$section" "$second's read of $what on $key is the read on $twin_key whole, once each id, time, seq and rank is replaced by its place"
}

# as_first SECTION WHAT PATH: the first person reads PATH, answered 200.
as_first() {
  request "cookie:$first" GET "$3"
  want_status 200 "$1" "$2"
}

# address_read ADDRESS READ: the path of READ at ADDRESS, READ being `to` or `from` (GET
# /api/v1/references) or `on` (GET /api/v1/lenses).
address_read() {
  case "$2" in
    on) printf '/api/v1/lenses?on=%s' "$(q "$1")" ;;
    *) printf '/api/v1/references?%s=%s' "$2" "$(q "$1")" ;;
  esac
}

# same_answer KEPT ID [FOR]: the last answer is the one kept as KEPT (keep) once ID is substituted
# by FOR, $missing unless given: its status, its Content-Type and its body, byte for byte.
same_answer() {
  sed "s/$2/${3:-$missing}/g" "$work/body" >"$work/substituted.json"
  [[ "$status $content_type" == "$(<"$work/$1.head")" ]] || return 1
  cmp -s "$work/substituted.json" "$work/$1.json"
}

# --- the catalogue ------------------------------------------------------------------------------

seeded_catalogue

# --- setup --------------------------------------------------------------------------------------

sign_in setup "$first"
sign_in setup "$second"
mint_token "$first"
mint_token "$second"

spec_text="$(printf 'The spec every person of %s reads.\n\nIt holds one more paragraph.\n' "$project")"
comment_body="$(jq -cn --arg w "$public_word" '{body: "A public comment of the issue: \($w)."}')"
readonly spec_text comment_body
premortem=""

seed twin
# Each twin_<name> is read as ${!twin_name} in C4, by the name of the neighbour it stands for.
# shellcheck disable=SC2034
readonly twin_key="$key" twin_spec="$spec" twin_check="$check" twin_tradeoff="$tradeoff" twin_inversion="$inversion"
# shellcheck disable=SC2034
readonly twin_comment="dispatch://$key/comment/$comment_id" twin_comment_id="$comment_id"
seed hidden
readonly key spec check tradeoff inversion premortem finding risk run_id
readonly comment="dispatch://$key/comment/$comment_id" comment_id interaction="dispatch://interaction/$run_id"
readonly premortem_id="${premortem#dispatch://lens/}" risk_id="${risk#dispatch://risk/}"
readonly finding_id="${finding##*#b-}"
pass setup "$key holds the hidden pre-mortem $premortem, its finding $finding, the risk $risk and the interaction $interaction; $twin_key holds none of them"

# What no answer to bob may name: each hidden node's id, and the words only those nodes carry.
jq -cn --args '$ARGS.positional' "$premortem_id" "$finding_id" "$risk_id" "$run_id" \
  "$hidden_word" "$risk_word" "$run_word" >"$work/hidden.json"

# --- C1: the chain read of the spec -------------------------------------------------------------

# The first read bob makes of a public node since the pre-mortem was written: a chain walk that read
# the visibility of the lens it started from and of nothing it reached would list the pre-mortem
# here, between the two public lenses. twin keeps his read of the spec in $work/bob.json.
twin C1 "the lenses on the spec" "$lens_counts" "/api/v1/lenses?on=$(q "$spec")" "/api/v1/lenses?on=$(q "$twin_spec")"
cp "$work/bob.json" "$work/body"
chains="$(jq -c '[.chains[] | {lenses: [.lenses[].address], stopped_at}]' "$work/body")" ||
  fail C1 "read the chains of the spec" "unreadable: $(body)"
got="$(jq -c --arg p "$premortem" '[.chains[].lenses[] | select(.address == $p) | .address]' "$work/body")" ||
  fail C1 "read the lenses of the chains of the spec" "unreadable: $(body)"
[[ "$got" == "[]" ]] ||
  fail C1 "no chain $second reads under the public spec $spec passes through $premortem, the lens reserved for $first" \
    "the chain read of $spec, read as $second, holds $premortem: chains $chains"
pass C1 "no chain $second reads under the public spec $spec passes through $premortem, the lens reserved for $first: chains $chains"
expect C1 "$second's read of the spec lists the two public lenses, each joined to no lens he may see, and no chain" \
  "$(jq -cnS --arg a "$tradeoff" --arg b "$inversion" \
    '{chains: [], lenses: [{address: $a, triggered: [], triggered_by: []}, {address: $b, triggered: [], triggered_by: []}]}')" \
  '{chains, lenses: [.lenses[] | {address, triggered, triggered_by}]}'

as_first C1 "$first reads the lenses on the spec $spec" "/api/v1/lenses?on=$(q "$spec")"
expect C1 "$first's read of the spec walks the chain of three through her pre-mortem" \
  "$(jq -cnS --arg a "$tradeoff" --arg b "$premortem" --arg c "$inversion" '[{lenses: [$a, $b, $c], stopped_at: null}]')" \
  '[.chains[] | {lenses: [.lenses[].address], stopped_at}]'
expect C1 "$first's read of the spec lists the pre-mortem between the public lenses, joined to each" \
  "$(jq -cnS --arg a "$tradeoff" --arg b "$premortem" --arg c "$inversion" \
    '[{address: $a, triggered: [$b], triggered_by: [], visibility: "project"},
      {address: $b, triggered: [$c], triggered_by: [$a], visibility: "author"},
      {address: $c, triggered: [], triggered_by: [$b], visibility: "project"}]')" \
  '[.lenses[] | {address, triggered, triggered_by, visibility}]'

# --- C2: the list of each kind ------------------------------------------------------------------

# A lens is listed by the lens read of the node it stands on, a risk by the lens read of its
# pre-mortem, and the person page lists what a person wrote of every kind. Bob's lens read of the
# spec above lists no risk: the risk stands under the pre-mortem, which he reads as nobody's (C3).
expect C2 "$first's lens read of the spec lists her risk under the pre-mortem, prevented by the public check" \
  "$(jq -cnS --arg r "$risk" --arg c "$check" '[{address: $r, prevented_by: [$c], visibility: "author"}]')" \
  --arg p "$premortem" '[.lenses[] | select(.address == $p) | .risks[] | {address, prevented_by, visibility}]'
expect C2 "$first's lens read of the spec lists the pre-mortem's finding, reserved for her" \
  "$(jq -cnS --arg f "$finding" '[{address: $f, visibility: "author"}]')" \
  --arg p "$premortem" '[.lenses[] | select(.address == $p) | .findings[] | {address, visibility}]'

# The person page is newest first: this run's rows are the newest of the first person's, within a
# page of 200. A row of a lens, a finding, a risk or a comment names the issue it opens at in its
# href. A run of the command bar stands under no issue and opens nothing, so its row's href is
# empty, and the row counts as the issue's when its run is one the issue's comment was written
# under ($runs): her run on $key, none on $twin_key.
readonly page_rows='[.rows[] | select(.href == "/issues/\($k)" or (.href | startswith("/issues/\($k)/"))
  or (.href == "" and .kind == "interaction" and (.id as $id | any($runs[]; . == $id))))]'
request "cookie:$second" GET "/api/v1/people/$(q "$first")/nodes?limit=200"
want_status 200 C2 "$second reads $first's page"
cp "$work/body" "$work/bob.page.json"
no_leak C2 "$second's read of $first's page lists no hidden node and carries none of their words" "$work/bob.page.json"
got="$(jq -cS --arg k "$key" --argjson runs "[\"$run_id\"]" "$page_rows | [.[].kind] | sort" "$work/bob.page.json")" ||
  fail C2 "count the rows of $first's page on $key" "unreadable: $(head -c 600 "$work/bob.page.json")"
want="$(jq -cS --arg k "$twin_key" --argjson runs '[]' "$page_rows | [.[].kind] | sort" "$work/bob.page.json")" ||
  fail C2 "count the rows of $first's page on $twin_key" "unreadable: $(head -c 600 "$work/bob.page.json")"
[[ "$got" == "$want" ]] ||
  fail C2 "$second's read of $first's page counts of each kind on $key what it counts on $twin_key" \
    "on $key it lists $got, on $twin_key $want"
pass C2 "$second's read of $first's page counts of each kind on $key what it counts on $twin_key, which holds no hidden node: $got"
expect C2 "$second's read of $first's page lists no run of the command bar, her run $run_id among them: a run is listed to the credential that asked it alone" \
  '[]' '[.rows[] | select(.kind == "interaction") | .id]'
as_first C2 "$first reads her own page" "/api/v1/people/$(q "$first")/nodes?limit=200"
expect C2 "$first's own page lists her pre-mortem, its finding, her risk and her run on $key, each reserved for her" \
  "$(jq -cnS --arg p "$premortem_id" --arg f "lens/$premortem_id#$finding_id" --arg r "$risk_id" --arg i "$run_id" \
    '[["interaction", $i, "author"], ["lens", $p, "author"], ["lens_block", $f, "author"], ["risk", $r, "author"]] | sort')" \
  --arg k "$key" --argjson runs "[\"$run_id\"]" "$page_rows | [.[] | select(.visibility == \"author\") | [.kind, .id, .visibility]] | sort"
expect C2 "$first's own page lists her run $run_id, which she wrote, titled by its words and opening nothing" \
  "$(jq -cnS --arg w "$run_word" '[{href: "", relations: ["authored_by"], title: "Which run changed the comment, \($w)?"}]')" \
  --arg i "$run_id" '[.rows[] | select(.kind == "interaction" and .id == $i) | {href, relations, title}]'

# --- C3: the read of each hidden node's own address ---------------------------------------------

# finds SECTION WHO PATH READ ADDRESS: the last answer, WHO's read GET PATH (READ being to, from or
# on), answers ADDRESS, reserved for its author.
finds() {
  case "$4" in
    on) expect "$1" "$2's read GET $3 answers $5, reserved for her" \
      "$(jq -cnS --arg a "$5" '{address: $a, visibility: "author"}')" '.on | {address, visibility}' ;;
    *) expect "$1" "$2's read GET $3 answers $5, reserved for her" \
      "$(jq -cnS --arg a "$5" '{ref: $a, visibility: "author"}')" '.node | {ref, visibility}' ;;
  esac
}

# Bob reads each address with his cookie and with his personal token, and each answers him what an
# address of the same form nobody wrote answers through the same credential. The first person's
# cookie, which wrote all three, finds each in every read but one. Her personal token proves her
# too, so it finds her pre-mortem and her risk, whose predicate compares the author a credential
# proved, and reads her run as an address nobody wrote: a run's predicate compares the whole
# credential that asked it, and her cookie asked it. The read her cookie does not find is the lens
# read of her run: a run stands under no issue and no lens stands on it (plan P9), and GET
# /api/v1/lenses?on= refuses its kind before it reads the store (lensStandsOnKinds), so its answer
# is the same whatever the store holds: every reader, her cookie among them, reads what an address
# nobody wrote answers, byte for byte once the id is substituted.
for name in premortem risk interaction; do
  address="${!name}"
  form="${address%/*}"
  id="${address##*/}"
  for read in to from on; do
    nobody_path="$(address_read "$form/$missing" "$read")"
    path="$(address_read "$address" "$read")"
    # The lens read of a run, which the read refuses by its kind whoever asks.
    refused=false
    [[ "$name" != interaction || "$read" != on ]] || refused=true
    readers=("cookie:$second" "token:$second")
    [[ "$name" != interaction ]] || readers+=("token:$first")
    [[ "$refused" == false ]] || readers+=("cookie:$first")
    for reader in "${readers[@]}"; do
      case "$reader" in
        "cookie:$second") who="$second" label=bob ;;
        "token:$second") who="$second's personal token" label=bob-token ;;
        "cookie:$first") who="$first" label=first ;;
        *) who="$first's personal token" label=first-token ;;
      esac
      request "$reader" GET "$nobody_path"
      keep "nobody.$label.$name.$read"
      [[ "$status" != 200 ]] ||
        fail C3 "an address nobody wrote, $form/$missing, answers $who otherwise than a node it reads" "HTTP 200: $(body)"
      request "$reader" GET "$path"
      keep "$label.$name.$read"
      same_answer "nobody.$label.$name.$read" "$id" ||
        fail C3 "$who's read GET $path answers what the address nobody wrote, $form/$missing, answers" \
          "HTTP $status, Content-Type $content_type: $(body); the address nobody wrote answers $(<"$work/nobody.$label.$name.$read.head"): $(<"$work/nobody.$label.$name.$read.json")"
      pass C3 "$who's read GET $path answers HTTP $status, Content-Type $content_type and the body bytes of $form/$missing, once the id is substituted: $(body)"
    done
    # Her cookie read the refused read above, as an address nobody wrote; it finds every other.
    [[ "$refused" == false ]] || continue
    as_first C3 "$first reads GET $path" "$path"
    finds C3 "$first" "$path" "$read" "$address"
    if [[ "$name" != interaction ]]; then
      request "token:$first" GET "$path"
      want_status 200 C3 "$first's personal token reads GET $path"
      finds C3 "$first's personal token" "$path" "$read" "$address"
    fi
  done
done

# --- C4: the references of each public neighbour, with its backlink count -----------------------

for name in issue spec tradeoff inversion check comment; do
  case "$name" in
    issue) address="dispatch://$key" twin_address="dispatch://$twin_key" ;;
    *)
      address="${!name}"
      twin_name="twin_$name"
      twin_address="${!twin_name}"
      ;;
  esac
  for read in to from; do
    twin C4 "the tuples $read $address" "$edge_counts" \
      "/api/v1/references?$read=$(q "$address")" "/api/v1/references?$read=$(q "$twin_address")"
  done
done
# A document's read lists the part_of tuples of what stands under it only when asked for by kind
# (parseReferencesQuery), and a lens stands under the spec by one. The spec's blocks share one
# write's time and are ordered by their random ids, so each answer is kept in an order of its words.
twin C4 "the part_of tuples to $spec" "$edge_counts" \
  "/api/v1/references?to=$(q "$spec")&kind=part_of" "/api/v1/references?to=$(q "$twin_spec")&kind=part_of" \
  '.edges |= sort_by([.kind, .node.kind, (.excerpt.text // "")])'
# The issue's number is its key's own, which canon replaces as <key>.
twin C4 "the issue, with its backlink count" "$issue_counts" "/api/v1/issues/$key" "/api/v1/issues/$twin_key" 'del(.number)'
# The lens read of the trade-off analysis is C5's, beside the count it held before.
twin C4 "the lens read of the public inversion $inversion" "$lens_counts" \
  "/api/v1/lenses?on=$(q "$inversion")" "/api/v1/lenses?on=$(q "$twin_inversion")"
# The far end of the pre-mortem's has_kind tuple, $lens_kind, is a node of the catalogue that both
# issues share, so no read on the twin stands for it: bob's reads of it in both directions are held
# to the same reads he made before the pre-mortem was written (seed), count for count and whole. A
# lens of that kind another writer adds meanwhile, on a shared server, fails here naming itself.
for read in to from; do
  request "cookie:$second" GET "/api/v1/references?$read=$(q "$lens_kind")"
  want_status 200 C4 "$second reads the tuples $read $lens_kind"
  cp "$work/body" "$work/bob.json"
  no_leak C4 "$second's read of the tuples $read $lens_kind names no hidden node and carries none of their words, in no excerpt either" \
    "$work/bob.json"
  got="$(jq -cS "$edge_counts" "$work/bob.json")" ||
    fail C4 "count the tuples $read $lens_kind" "unreadable: $(head -c 600 "$work/bob.json")"
  want="$(jq -cS "$edge_counts" "$work/before.kind.$read.json")" ||
    fail C4 "count the tuples $read $lens_kind before the pre-mortem was written" "unreadable: $(head -c 600 "$work/before.kind.$read.json")"
  [[ "$got" == "$want" ]] ||
    fail C4 "each count $second reads in the tuples $read $lens_kind is the count he read before the pre-mortem was written" \
      "now he reads $got, before $want"
  pass C4 "each count $second reads in the tuples $read $lens_kind is the count he read before the pre-mortem was written: $got"
  got="$(jq -cS . "$work/bob.json")" || fail C4 "read the tuples $read $lens_kind" "unreadable: $(head -c 600 "$work/bob.json")"
  want="$(jq -cS . "$work/before.kind.$read.json")" ||
    fail C4 "read the tuples $read $lens_kind before the pre-mortem was written" "unreadable: $(head -c 600 "$work/before.kind.$read.json")"
  [[ "$got" == "$want" ]] ||
    fail C4 "$second's read of the tuples $read $lens_kind is, whole, the read he made before the pre-mortem was written" \
      "the two differ at $(differ_at "$got" "$want" now before); now he reads $got"
  pass C4 "$second's read of the tuples $read $lens_kind is, whole, the read he made before the pre-mortem was written"
done

# The same reads find each hidden node as its author, with the words only it carries in the excerpt.
as_first C4 "$first reads the part_of tuples to the spec" "/api/v1/references?to=$(q "$spec")&kind=part_of"
expect C4 "the part_of tuples to the spec hold the pre-mortem's for $first, its excerpt carrying the pre-mortem's words" \
  true --arg p "$premortem" --arg w "$hidden_word" \
  '[.edges[] | select(.kind == "part_of" and .node.ref == $p and (.excerpt.text | contains($w)))] | length == 1'
as_first C4 "$first reads the tuples to the trade-off analysis" "/api/v1/references?to=$(q "$tradeoff")"
expect C4 "the tuples to the trade-off analysis hold the pre-mortem's triggered_by tuple for $first, its excerpt carrying the pre-mortem's words" \
  true --arg p "$premortem" --arg w "$hidden_word" \
  '[.edges[] | select(.kind == "triggered_by" and .node.ref == $p and (.excerpt.text | contains($w)))] | length == 1'
as_first C4 "$first reads the tuples from the inversion" "/api/v1/references?from=$(q "$inversion")"
expect C4 "the tuples from the inversion hold its triggered_by tuple to the pre-mortem for $first, its excerpt carrying the pre-mortem's words" \
  true --arg p "$premortem" --arg w "$hidden_word" \
  '[.edges[] | select(.kind == "triggered_by" and .node.ref == $p and (.excerpt.text | contains($w)))] | length == 1'
as_first C4 "$first reads the tuples to the check" "/api/v1/references?to=$(q "$check")"
expect C4 "the tuples to the check hold the risk's prevented_by tuple for $first, its excerpt carrying the risk's words" \
  true --arg r "$risk" --arg w "$risk_word" \
  '[.edges[] | select(.kind == "prevented_by" and .node.ref == $r and (.excerpt.text | contains($w)))] | length == 1'
as_first C4 "$first reads the tuples from the comment" "/api/v1/references?from=$(q "$comment")"
expect C4 "the tuples from the comment hold one changed_by_run tuple, to her interaction, its excerpt carrying the run's words" \
  "$(jq -cn --arg i "$interaction" --arg w "$run_word" '[["changed_by_run", $i, "interaction", true]]')" \
  --arg w "$run_word" '[.edges[] | [.kind, .node.ref, .node.kind, (.excerpt.text | contains($w))]]'
as_first C4 "$first reads the lens read of the trade-off analysis" "/api/v1/lenses?on=$(q "$tradeoff")"
expect C4 "the trade-off analysis answers $first the pre-mortem among the lenses it triggered, in a chain of three" \
  "$(jq -cnS --arg a "$tradeoff" --arg b "$premortem" --arg c "$inversion" '{chains: [[$a, $b, $c]], triggered: [$b]}')" \
  '{triggered: .lens.triggered, chains: [.chains[] | [.lenses[].address]]}'
as_first C4 "$first reads the tuples to $lens_kind" "/api/v1/references?to=$(q "$lens_kind")"
expect C4 "the tuples to $lens_kind hold the pre-mortem's has_kind tuple for $first, its excerpt carrying the pre-mortem's words" \
  true --arg p "$premortem" --arg w "$hidden_word" \
  '[.edges[] | select(.kind == "has_kind" and .node.ref == $p and (.excerpt.text | contains($w)))] | length == 1'

# --- C5: a tuple whose near end is hidden and whose far end is not ------------------------------

# The pre-mortem's triggered_by tuple to the trade-off analysis: bob may read the far end and not
# the near one. readEdgeRows filters both ends, so the tuple is in neither end's answer.
path="$(address_read "$premortem" from)"
request "cookie:$second" GET "$path"
same_answer nobody.bob.premortem.from "$premortem_id" ||
  fail C5 "the near end: $second's read of the tuples out of $premortem answers what an address nobody wrote answers" \
    "HTTP $status, Content-Type $content_type: $(body)"
pass C5 "the near end: $second's read of the tuples out of $premortem answers what an address nobody wrote answers, so the tuple is in none"
# The whole answer of each end, beside the answer the same read gives on the twin, where the tuple
# was never written; then the counts of the answer on the issue, which twin keeps in
# $work/bob.json, beside the counts bob read before the pre-mortem was written.
twin C5 "the triggered_by tuples into the trade-off analysis" "$edge_counts" \
  "/api/v1/references?to=$(q "$tradeoff")&kind=triggered_by" "/api/v1/references?to=$(q "$twin_tradeoff")&kind=triggered_by"
cp "$work/bob.json" "$work/body"
got="$(jq -c '[.edges[] | {kind, from: .node.ref}]' "$work/body")" || fail C5 "read the triggered_by tuples into the trade-off analysis" "unreadable: $(body)"
[[ "$got" == "[]" ]] ||
  fail C5 "the far end: the triggered_by tuples $second reads into the trade-off analysis hold no tuple from $premortem" \
    "they hold $got"
pass C5 "the far end: the triggered_by tuples $second reads into the trade-off analysis hold no tuple from $premortem"
got="$(jq -r '.edges | length' "$work/body")" || fail C5 "count the triggered_by tuples into the trade-off analysis" "unreadable: $(body)"
want="$(jq -r '.edges | length' "$work/before.triggered.json")" ||
  fail C5 "count the triggered_by tuples into the trade-off analysis before the pre-mortem was written" "unreadable: $(<"$work/before.triggered.json")"
[[ "$got" == "$want" ]] ||
  fail C5 "$second counts the triggered_by tuples into the trade-off analysis he counted before the pre-mortem was written" \
    "he counts $got, and counted $want before"
pass C5 "$second counts $got triggered_by tuples into the trade-off analysis, the $want he counted before the pre-mortem was written"
twin C5 "the lens read of the trade-off analysis" "$lens_counts" \
  "/api/v1/lenses?on=$(q "$tradeoff")" "/api/v1/lenses?on=$(q "$twin_tradeoff")"
cp "$work/bob.json" "$work/body"
got="$(jq -r '.lens.triggered | length' "$work/body")" || fail C5 "count the lenses the trade-off analysis triggered" "unreadable: $(body)"
want="$(jq -r '.lens.triggered | length' "$work/before.lenses.json")" ||
  fail C5 "count the lenses the trade-off analysis triggered before the pre-mortem was written" "unreadable: $(<"$work/before.lenses.json")"
[[ "$got" == "$want" ]] ||
  fail C5 "the count of triggered lenses $second reads on the trade-off analysis is the count before the pre-mortem was written" \
    "he reads $got ($(jq -c '.lens.triggered' "$work/body")), and read $want before"
pass C5 "the count of triggered lenses $second reads on the trade-off analysis is $got, the count before the pre-mortem was written"
as_first C5 "$first reads the triggered_by tuples into the trade-off analysis" \
  "/api/v1/references?to=$(q "$tradeoff")&kind=triggered_by"
expect C5 "the same read finds the pre-mortem's triggered_by tuple for $first" "[\"$premortem\"]" '[.edges[] | .node.ref]'
as_first C5 "$first reads the tuples out of the pre-mortem" "$path"
expect C5 "the tuples out of the pre-mortem hold its triggered_by tuple to the trade-off analysis for $first" "[\"$tradeoff\"]" \
  '[.edges[] | select(.kind == "triggered_by") | .node.ref]'

# --- C6: the event list and the whole event log -------------------------------------------------

request "cookie:$second" GET "/api/v1/issues/$key/events"
want_status 200 C6 "$second reads the event list of $key"
cp "$work/body" "$work/bob.events.json"
no_leak C6 "$second's event list of $key holds neither the address nor the text of any hidden node" "$work/bob.events.json"
expect C6 "the same read holds the public comment, by its id and its words" true \
  --arg c "$comment_id" --arg w "$public_word" '[.. | strings] | (any(contains($c)) and any(contains($w)))'
# Every event written since the twin was created, paged by id.
request "cookie:$second" GET "/api/v1/issues/$twin_key/events"
want_status 200 C6 "$second reads the event list of $twin_key"
after="$(jq -er '[.[].id] | min - 1' "$work/body")" || fail C6 "read the first event of $twin_key" "$(body)"
printf '[]' >"$work/bob.log.json"
while true; do
  request "cookie:$second" GET "/api/v1/events/log?after=$after&limit=1000"
  want_status 200 C6 "$second reads the event log after the event $after"
  jq -c --slurpfile page "$work/body" '. + $page[0]' "$work/bob.log.json" >"$work/log.next" ||
    fail C6 "gather the event log after the event $after" "unreadable: $(body)"
  mv "$work/log.next" "$work/bob.log.json"
  [[ "$(jq -r 'length' "$work/body")" == 1000 ]] || break
  after="$(jq -er '.[-1].id' "$work/body")" || fail C6 "read the last event of the page after $after" "$(body)"
done
no_leak C6 "$second's read of the event log, $(jq -r 'length' "$work/bob.log.json") events since $twin_key was created, holds neither the address nor the text of any hidden node" \
  "$work/bob.log.json"
cp "$work/bob.log.json" "$work/body"
expect C6 "the same read holds the public comments of both issues, by their ids and their words" true \
  --arg c "$comment_id" --arg t "$twin_comment_id" --arg w "$public_word" \
  '[.. | strings] | (any(contains($c)) and any(contains($t)) and any(contains($w)))'

# --- C7: search ---------------------------------------------------------------------------------

for login in "$second" "$first"; do
  for word in "$hidden_word" "$risk_word" "$run_word"; do
    request "cookie:$login" GET "/api/v1/search?q=$(q "$word")"
    want_status 200 C7 "$login searches for $word"
    expect C7 "$login's search for $word, which only a hidden node carries, answers no hit and a total of zero" \
      '{"hits":[],"total":0}' '{hits: .results, total}'
  done
  # The search finding a word a public node carries, at the same scope.
  request "cookie:$login" GET "/api/v1/search?q=$(q "$public_word")"
  want_status 200 C7 "$login searches for $public_word"
  expect C7 "$login's search for $public_word, which the public comments carry, finds both comments" \
    "$(jq -cnS --arg a "$comment_id" --arg b "$twin_comment_id" '{hits: ([$a, $b] | sort), total: 2}')" \
    '{hits: ([.results[].id] | sort), total}'
done

# --- C8: a person who has written nothing -------------------------------------------------------

sign_in setup "$third"
request "cookie:$second" GET "/api/v1/people/$(q "$nobody_login")/nodes"
keep nobody.page
[[ "$status" != 200 ]] ||
  fail C8 "the page of $nobody_login, a login nobody holds, answers $second otherwise than a page he reads" "HTTP 200: $(body)"
pass C8 "the page of $nobody_login, a login nobody holds, answers $second HTTP $status, Content-Type $content_type: $(body)"
request "cookie:$second" GET "/api/v1/people/$(q "$third")/nodes"
same_answer nobody.page "$third" "$nobody_login" ||
  fail C8 "before $third writes, $second's read of $third's page answers what a login nobody holds answers" \
    "HTTP $status, Content-Type $content_type: $(body)"
pass C8 "before $third writes, $second's read of $third's page answers what a login nobody holds answers, byte for byte once the login is substituted"
invoke C8 "$third, who has written nothing, invokes a lens reserved for them on the spec" "cookie:$third" \
  "$(lens_body first-principles 'The verdict of the third person.' 'A finding of the third person.' '' author)"
readonly third_lens="$lens" third_lens_id="$lens_id"
third_finding="$(jq -er '.findings[0].id' "$work/invoked")" || fail C8 "read the finding of $third's lens" "$(<"$work/invoked")"
readonly third_finding
cp "$work/invoked" "$work/body"
expect C8 "$third's lens and its finding are reserved for them" \
  "$(jq -cnS --arg l "$third" '{author: {id: $l, kind: "person"}, finding: "author", visibility: "author"}')" \
  '{author, visibility, finding: .findings[0].visibility}'
request "cookie:$second" GET "/api/v1/people/$(q "$third")/nodes"
same_answer nobody.page "$third" "$nobody_login" ||
  fail C8 "after $third writes $third_lens, $second's read of $third's page answers what a login nobody holds answers" \
    "HTTP $status, Content-Type $content_type: $(body)"
pass C8 "after $third writes $third_lens, $second's read of $third's page answers what a login nobody holds answers, byte for byte once the login is substituted"
request "cookie:$third" GET "/api/v1/people/$(q "$third")/nodes"
want_status 200 C8 "$third reads their own page"
expect C8 "$third finds the lens and its finding on their own page, each reserved for them, and nothing else" \
  "$(jq -cnS --arg i "$third_lens_id" --arg f "$third_finding" '{rows: [["lens", $i, "author"], ["lens_block", $f, "author"]], total: 2}')" \
  '{rows: ([.rows[] | [.kind, .id, .visibility]] | sort), total}'

# --- C9: the hidden lens gone -------------------------------------------------------------------

request "cookie:$first" POST "/api/v1/issues/$key/nodes/$(q "lens/$premortem_id")/state" '{"state":"gone"}'
want_status 200 C9 "$first writes gone on $premortem"
expect C9 "the write answers $premortem moved from live to gone" \
  "$(jq -cnS --arg a "$premortem" '{address: $a, previous: "live", state: "gone"}')" \
  '{address: .node.address, previous, state: .node.state}'
for read in to from on; do
  path="$(address_read "$premortem" "$read")"
  request "cookie:$second" GET "$path"
  same_answer "bob.premortem.$read" "$premortem_id" "$premortem_id" ||
    fail C9 "$second's read GET $path after the gone is the one he read before, byte for byte" \
      "HTTP $status, Content-Type $content_type: $(body); before: $(<"$work/bob.premortem.$read.head"): $(<"$work/bob.premortem.$read.json")"
  pass C9 "$second's read GET $path after the gone is the one he read before, byte for byte: HTTP $status, Content-Type $content_type"
  # The write took: the same read of the author moves from the node to no node.
  request "cookie:$first" GET "$path"
  [[ "$status" == 404 ]] ||
    fail C9 "$first's read GET $path no longer answers the gone lens" "HTTP $status: $(body)"
  pass C9 "$first's read GET $path, which answered the lens before, answers HTTP 404 now it is gone"
done

pass "$driver" complete
