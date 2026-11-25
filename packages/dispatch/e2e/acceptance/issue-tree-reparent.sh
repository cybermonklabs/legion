#!/usr/bin/env bash
# The issue-tree check c-reparent-and-state, the cases of the move route: a person or
# an agent moves a node of an issue's tree through PATCH /api/v1/issues/{key}/nodes/{id} (R7), with
# `{id}` the node's store address as one percent-encoded path segment, the body {parent, after} and
# an If-Match naming the node's placement_seq as a quoted entity tag. It runs against a server of
# this head serving alone, so every issue it creates is born converted, on the database that server
# serves, and seeds every issue it reads.
#
#   subtree  Alice writes, under the spec's memo callout, two criteria K1 and K2, a requirement R1
#            under K1 and two checks under R1, and moves R1 under K2: one part_of appended to K2, one
#            retracted from K1, no tuple of either check, both checks under R1 in their order, and
#            their serves tuples as they were. Every move taken answers its parent, the ranked
#            sibling the store holds it right after (or null) and its placement_seq.
#   refusals A parent inside the moving subtree and a parent in another tree are refused 400
#            INVALID_POINTER, a stale If-Match 412, each storing nothing; the move kept R1's words.
#            Lenses L1 (pre-mortem on K1), L2 (pre-mortem on K2, triggered by L1) and a risk under
#            L1 prevented by a check: L2 moves under R1 keeping has_kind and triggered_by, the risk
#            moves under L2 keeping prevented_by, and under L3, a trade-off, it is refused 400
#            INVALID_POINTER by the store's rule.
#   constraint A constraint moves only under an issue node: under R1 400 INVALID_POINTER, under the
#            child issue's node 200, and the child's constraints list it then.
#   ask      The route moves no ask: 400 NODE_NOT_MOVABLE, its part_of still the spec's, and the store
#            census, run whatever the move answered, reads no ask_parent difference and no tree
#            mismatch.
#   child    A block under the child issue's node is 400 INVALID_POINTER, a comment is 400
#            NODE_NOT_MOVABLE and stays listed under the issue, both trees read whatever the move
#            answered, and L3 moves under the child.
#   removable No move puts a node where a removal of a document would leave it live behind no row:
#            U (one empty version) and W (text), the constraint under U 400 INVALID_POINTER, L4 onto
#            U 200 with U's row as its artifact_id, a finding onto U 409 PARENT_DOCUMENT_REMOVABLE,
#            the finding under L4 200, a requirement under L4 409, under W 200, and under L4 again
#            409 after a comment cites U.
#   depth    The rule holds two levels down, and reads the moved node's whole subtree (L5).
#   rowless  A document node whose row has gone counts as removable: V's row is deleted by SQL as
#            main's removal deletes it, under the block-index lease, and the move under V's node is
#            409 PARENT_DOCUMENT_REMOVABLE, not 404.
#   state    A requirement with a plan node set overtaken leaves the coverage gaps and the plan view.
#   outline  The outline's moves through PATCH alone, chaining each answer's placement_seq; the text
#            passes keep every stored place; the input refusals; the route rows.
#   bands    after: null puts a node first in its own rank band, the memo first throughout.
#   request  An open approval request on the spec query node names the version the drain writes.
#
# Inputs: DATABASE_URL (the database the server serves, on this machine: the driver reads the store
# through psql, deletes V's rows there and holds the block-index lease there), DISPATCH_URL (the
# server), DISPATCH_TOKEN or DISPATCH_TOKEN_FILE (the agent bearer, read here and never printed),
# PROJECT (an existing project key) and DEV_SIGNIN_LOGIN (the login the server's dev sign-in route
# signs in: `alice`). `envoy-dispatch` of this head is on PATH, for the census. Each unmet input is
# exit 2. The agent bearer closes every issue the driver made when the run ends, also when a check
# fails.
#
# Each passing check prints `PASS: <section> <what>`; the first failing one prints
# `FAIL: <section> <what>: <observed>` on stderr and the script exits 1. It prints no SKIP line. The
# last line is `PASS: issue-tree-reparent.sh complete <n> moves`. Every check is `cmd || fail …`,
# never part of an `&&` list.

# shellcheck disable=SC2016 # the single-quoted jq filters and SQL name jq and psql variables
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DATABASE_URL=$INFRA_1253_DATABASE_URL DISPATCH_URL=http://127.0.0.1:8777 DISPATCH_TOKEN=e2e-token \
         PROJECT=CORE DEV_SIGNIN_LOGIN=alice bash e2e/acceptance/issue-tree-reparent.sh
       (run from packages/dispatch against a server of this head serving alone; DATABASE_URL names
       the database that server serves, on this machine; envoy-dispatch of this head on PATH;
       DISPATCH_TOKEN_FILE may stand in for DISPATCH_TOKEN)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
# shellcheck source-path=SCRIPTDIR source=lib-tree.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib-tree.sh"

require_env DATABASE_URL DISPATCH_URL PROJECT DEV_SIGNIN_LOGIN
require_commands curl jq psql
# shellcheck disable=SC2031 # the run's own DATABASE_URL
loopback_database_url "$DATABASE_URL" || {
  printf 'issue-tree-reparent.sh: DATABASE_URL names no single loopback host; the driver deletes rows in the store it names\n' >&2
  usage
  exit 2
}
command -v envoy-dispatch >/dev/null || {
  printf 'issue-tree-reparent.sh: envoy-dispatch is not on PATH: build ./cmd/dispatch of this head\n' >&2
  usage
  exit 2
}
read_bearer

readonly driver=issue-tree-reparent.sh
# shellcheck disable=SC2031 # the caller's DISPATCH_URL; only lib.sh's `cli` exports another
readonly base="${DISPATCH_URL%/}"
# shellcheck disable=SC2153 # PROJECT is the caller's input, checked by name above
readonly project="$PROJECT"
readonly person_login="$DEV_SIGNIN_LOGIN"
# shellcheck disable=SC2031 # the run's own DATABASE_URL
readonly database_url="$DATABASE_URL"
stamp="$(date +%s)-$$"
readonly stamp
readonly session="issue-tree-reparent-$stamp"
# The routes table this head serves, which the route rows are read from.
readonly routes_table="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../../envoy/internal/dispatch/api" && pwd)/routes_table.go"

work_dir
tree_init

keys=()
lease_pid=""
moves=0
cleanup() {
  local code=$? issue
  trap - EXIT INT TERM
  if [[ -n "$lease_pid" ]]; then
    kill "$lease_pid" 2>/dev/null || true
    wait "$lease_pid" 2>/dev/null || true
  fi
  for issue in "${keys[@]}"; do
    close_issue "$issue"
  done
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

tree_sign_in setup "$person_login"

# --- the vocabulary of this driver ------------------------------------------------------------------

# rp_patch AS KEY NODE TAG BODY: PATCH /api/v1/issues/KEY/nodes/NODE as AS (agent or person), NODE
# percent-encoded as one segment, with BODY, and with `If-Match: "TAG"` where TAG is not empty. The
# answer is in $work/body and `status`, as tree_request leaves them.
rp_patch() {
  local as="$1" key="$2" node="$3" tag="$4" body="$5"
  local -a auth match=()
  case "$as" in
    agent) auth=(-H "@$work/auth") ;;
    person) auth=(-b "$work/jar" -H "Origin: $(tree_origin)") ;;
    *) tree_refuse "rp_patch sends as agent or person, not '$as'" ;;
  esac
  [[ -z "$tag" ]] || match=(-H "If-Match: \"$tag\"")
  tree_last_request="PATCH /api/v1/issues/$key/nodes/$node${tag:+ If-Match \"$tag\"} $body"
  status="$(curl -sS -X PATCH -o "$work/body" -w '%{http_code}' "${auth[@]}" "${match[@]}" \
    -H 'Content-Type: application/json' --data-binary "$body" \
    "$base/api/v1/issues/$(q "$key")/nodes/$(q "$node")" 2>"$work/curl")" || status="curl failed: $(<"$work/curl")"
}

# rp_store ADDRESS: the store's kind and id of the node at ADDRESS, in rp_kind and rp_id: a block's
# address (a document's block or a lens's finding) holds a `#` and is the block node's id; every
# other address is `<kind>/<id>`.
rp_store() {
  if [[ "$1" == *"#"* ]]; then
    rp_kind=block rp_id="$1"
  else
    rp_kind="${1%%/*}" rp_id="${1#*/}"
  fi
}

# rp_seq: the newest seq of the tuple log, in rp_mark.
rp_seq() {
  query "$1" "read the newest seq of the tuple log" "select coalesce(max(seq), 0) from graph_tuples"
  rp_mark="$answer"
}

# rp_parents SECTION ADDRESS: the far ends of the current part_of of the node at ADDRESS -
# `<kind>/<id>`, or a block's id - joined by commas, in rp_parent.
rp_parents() {
  rp_store "$2"
  with_vars "$1" "read the part_of of $2" \
    "select coalesce(string_agg(case when h.to_kind = 'block' then h.to_id else h.to_kind || '/' || h.to_id end, ',' order by h.to_kind, h.to_id), '')
     from (select distinct on (to_kind, to_id) to_kind, to_id, asserted from graph_tuples
           where from_kind = :'kind' and from_id = :'id' and kind = 'part_of' and to_kind <> 'value'
           order by to_kind, to_id, seq desc) h where h.asserted" \
    kind="$rp_kind" id="$rp_id"
  rp_parent="$answer"
}

# rp_tag SECTION ADDRESS [any]: the placement_seq of the node at ADDRESS, the seq of its current
# part_of (as the tree read answers it), in rp_placed; with `any`, 0 for a node that holds none (a
# comment of the issue), which a refusal that comes before the If-Match test is sent with.
rp_tag() {
  rp_store "$2"
  with_vars "$1" "read the placement_seq of $2" \
    "select coalesce((select h.seq from (select distinct on (to_kind, to_id) seq, asserted from graph_tuples
       where from_kind = :'kind' and from_id = :'id' and kind = 'part_of' and to_kind <> 'value'
       order by to_kind, to_id, seq desc) h where h.asserted order by h.seq desc limit 1)::text, '')" \
    kind="$rp_kind" id="$rp_id"
  if [[ -z "$answer" ]]; then
    [[ "${3:-}" == any ]] || fail "$1" "read the placement_seq of $2" "it holds no part_of"
    answer=0
  fi
  rp_placed="$answer"
}

# rp_value SECTION ADDRESS ATTRIBUTE: the current value of ATTRIBUTE of the node at ADDRESS, empty
# where it holds none, in answer.
rp_value() {
  rp_store "$2"
  with_vars "$1" "read the $3 of $2" \
    "select coalesce((select case when asserted then to_id end from graph_tuples
       where from_kind = :'kind' and from_id = :'id' and kind = :'attribute' and to_kind = 'value'
       order by seq desc limit 1), '')" \
    kind="$rp_kind" id="$rp_id" attribute="$3"
}

# rp_since SECTION ADDRESS MARK: the tuples out of the node at ADDRESS appended after MARK, each
# `<kind>:<far end>:<asserted>` in seq order, joined by commas, in answer.
rp_since() {
  rp_store "$2"
  with_vars "$1" "read the tuples of $2 since $3" \
    "select coalesce(string_agg(kind || ':' || case when to_kind = 'block' then to_id else to_kind || '/' || to_id end || ':' || asserted::text, ',' order by seq), '')
     from graph_tuples where from_kind = :'kind' and from_id = :'id' and seq > :'mark'::bigint" \
    kind="$rp_kind" id="$rp_id" mark="$3"
}

# rp_untouched SECTION WHAT ADDRESS MARK: no tuple out of the node at ADDRESS was appended after
# MARK: a refused move stores nothing.
rp_untouched() {
  rp_since "$1" "$3" "$4"
  [[ -z "$answer" ]] || fail "$1" "$2" "the tuples $answer were appended out of $3"
}

# rp_moved_tuples SECTION ADDRESS MARK FROM TO: the part_of tuples out of the node at ADDRESS since
# MARK are one retraction of FROM and one assertion of TO, and no other.
rp_moved_tuples() {
  local got
  rp_since "$1" "$2" "$3"
  got="$(tr ',' '\n' <<<"$answer" | grep '^part_of:' | sort | paste -sd, -)" || got=""
  [[ "$got" == "$(printf 'part_of:%s:false\npart_of:%s:true\n' "$4" "$5" | sort | paste -sd, -)" ]] ||
    fail "$1" "the move of $2 appended one part_of to $5 and retracted the one to $4" "its part_of tuples since the move: '$got'"
  pass "$1" "the move of $2 appended one part_of to $5 and retracted the one to $4"
}

# rp_preceding SECTION ADDRESS PARENT: the ranked child of PARENT that the node at ADDRESS now
# follows - the child just before it by rank, compared byte for byte, of any band - as a store
# address (a block's node id, every other node `<kind>/<id>`), or `null` where it follows none or
# holds no rank, in rp_before. It reads the store, as the move's answer must agree with it.
rp_preceding() {
  rp_store "$3"
  with_vars "$1" "read the ranked child of $3 that $2 follows" \
    "select coalesce((select o.before from (
       select s.child, lag(s.child) over (order by s.rank collate \"C\") as before from (
         select distinct on (t.from_kind, t.from_id) t.asserted,
                case when t.from_kind = 'block' then t.from_id else t.from_kind || '/' || t.from_id end as child,
                (select case when x.asserted then x.to_id end from graph_tuples x
                  where x.from_kind = t.from_kind and x.from_id = t.from_id and x.kind = 'rank' and x.to_kind = 'value'
                  order by x.seq desc limit 1) as rank
         from graph_tuples t
         where t.kind = 'part_of' and t.to_kind = :'kind' and t.to_id = :'id'
         order by t.from_kind, t.from_id, t.seq desc) s
       where s.asserted and s.rank is not null) o
     where o.child = :'node'), 'null')" \
    kind="$rp_kind" id="$rp_id" node="$2"
  rp_before="$answer"
}

# rp_move SECTION AS KEY ADDRESS PARENT [AFTER]: moves ADDRESS under PARENT (after AFTER, where it is
# given: `null` for first) with the If-Match of its placement_seq, wants 200 with parent, the
# sibling it now follows and placement_seq, and checks the tuples (rp_moved_tuples) where the parent
# changed. The sibling the answer names is the one the store holds right before the node
# (rp_preceding), and where AFTER names a node, that node. It leaves the answer's placement_seq in
# rp_placed.
rp_move() {
  local section="$1" as="$2" key="$3" address="$4" parent="$5" after="${6:-}" from mark body
  rp_parents "$section" "$address"
  from="$rp_parent"
  rp_tag "$section" "$address"
  rp_seq "$section"
  mark="$rp_mark"
  body="$(jq -cn --arg p "$parent" '{parent: $p}')"
  case "$after" in
    "") ;;
    null) body="$(jq -c '. + {after: null}' <<<"$body")" ;;
    *) body="$(jq -c --arg a "$after" '. + {after: $a}' <<<"$body")" ;;
  esac
  rp_patch "$as" "$key" "$address" "$rp_placed" "$body"
  [[ "$status" == 200 ]] ||
    fail "$section" "the move of $address under $parent" "the move of $address under $parent answered $status, want 200: $(body)"
  expect "$section" "the move of $address answers its parent" "\"$parent\"" .parent
  rp_placed="$(jq -er '.placement_seq | numbers' "$work/body")" ||
    fail "$section" "the move of $address answers its placement_seq" "PATCH of $address answered without placement_seq for the next If-Match: $(body)"
  rp_preceding "$section" "$address" "$parent"
  case "$after" in
    "" | null) ;;
    *)
      [[ "$rp_before" == "$after" ]] ||
        fail "$section" "the move of $address after $after stands right after it" "it stands after $rp_before among the ranked children of $parent"
      ;;
  esac
  expect "$section" "the move of $address answers the sibling it now follows, $rp_before" \
    "$(jq -cn --arg a "$rp_before" 'if $a == "null" then null else $a end')" .after
  moves=$((moves + 1))
  [[ "$from" == "$parent" ]] || rp_moved_tuples "$section" "$address" "$mark" "$from" "$parent"
}

# rp_named WORD: the words a refusal names the node at the store address WORD by, the end of its
# dispatch:// address: `#b-<block>` for a block of a document, `lens/<id>#b-<block>` for a finding,
# `<key>/artifact/<slug>` for a document, `dispatch://<key>` for an issue; any other word as it is.
rp_named() {
  local ref
  case "$1" in
    lens/*"#"*) printf '%s#b-%s' "${1%%#*}" "${1#*#}" ;;
    *"#"*) printf '#b-%s' "${1#*#}" ;;
    artifact/*) ref="${1#artifact/}" && printf '%s/artifact/%s' "${ref%%/*}" "${ref#*/}" ;;
    issue/*) printf 'dispatch://%s' "${1#issue/}" ;;
    *) printf '%s' "$1" ;;
  esac
}

# rp_refusal SECTION AS KEY ADDRESS PARENT STATUS CODE [WORDS…]: sends the move of ADDRESS under
# PARENT with the If-Match of its placement_seq (0 for a node that holds none, a refusal that comes
# before that test), and reads whether it answers STATUS CODE naming each of WORDS, appends no tuple
# out of ADDRESS and leaves its part_of where it stood. It fails nothing: rp_problem holds what it
# found wrong, empty where nothing was, so a caller reads what the check reads whatever the move
# answered before it fails.
rp_refusal() {
  local section="$1" as="$2" key="$3" address="$4" parent="$5" want="$6" code="$7" mark before word got
  shift 7
  rp_problem=""
  rp_parents "$section" "$address"
  before="$rp_parent"
  rp_tag "$section" "$address" any
  rp_seq "$section"
  mark="$rp_mark"
  rp_patch "$as" "$key" "$address" "$rp_placed" "$(jq -cn --arg p "$parent" '{parent: $p}')"
  got="$(jq -r '.code // ""' "$work/body" 2>/dev/null)" || got=""
  if [[ "$status" != "$want" || "$got" != "$code" ]]; then
    rp_problem="the move of $address under $parent answered $status${got:+ $got}, want $want $code: $(body)"
  else
    for word in "$@"; do
      jq -e --arg w "$(rp_named "$word")" '.error | contains($w)' "$work/body" >/dev/null ||
        rp_problem+="${rp_problem:+; }the refusal of the move of $address under $parent names no $word: $(body)"
    done
  fi
  rp_since "$section" "$address" "$mark"
  [[ -z "$answer" ]] || rp_problem+="${rp_problem:+; }the move of $address under $parent appended the tuples $answer out of it"
  rp_parents "$section" "$address"
  [[ "$rp_parent" == "$before" ]] ||
    rp_problem+="${rp_problem:+; }the refused move of $address under $parent changed its part_of to $rp_parent"
}

# rp_refused SECTION AS KEY ADDRESS PARENT STATUS CODE [WORDS…]: rp_refusal, failing on what it
# found wrong: the move of ADDRESS under PARENT answers STATUS CODE naming each of WORDS, and
# appends no tuple out of ADDRESS; its part_of stays.
rp_refused() {
  local section="$1" address="$4" parent="$5" want="$6" code="$7"
  rp_refusal "$@"
  [[ -z "$rp_problem" ]] ||
    fail "$section" "the move of $address under $parent is refused $want $code, storing nothing" "$rp_problem"
  shift 7
  pass "$section" "the move of $address under $parent answers $want $code, naming $*, and stores nothing"
}

# rp_edit SECTION WHAT ARTIFACT OPS: the person applies the edit ops OPS to ARTIFACT; it must change it.
rp_edit() {
  tree_person_send POST "/api/v1/artifacts/$3/edits" "$(jq -cn --argjson ops "$4" '{ops: $ops}')"
  tree_want 200 "$1" "$2"
  jq -e '.changed == true' "$work/body" >/dev/null || fail "$1" "$2" "the edit changed nothing: $(body)"
  pass "$1" "$2"
}

# rp_issue SECTION TITLE SPEC [PARENT]: the person creates an issue of the project with SPEC, under
# PARENT where given; sets rp_key and rp_spec, the spec's artifact id.
rp_issue() {
  jq -cn --arg p "$project" --arg t "$2 $stamp" --arg s "$3" --arg parent "${4:-}" \
    '{project: $p, title: $t, spec: $s, force: true} + (if $parent == "" then {} else {parent: $parent} end)' >"$work/issue.json"
  tree_person_send POST /api/v1/issues "@$work/issue.json"
  tree_want 201 "$1" "create the issue \"$2\""
  tree_field "$1" "create the issue \"$2\"" .key
  rp_key="$tree_value"
  keys+=("$rp_key")
  tree_field "$1" "create the issue \"$2\"" .primary_artifact_id
  rp_spec="$tree_value"
  pass "$1" "created $rp_key${4:+ under $4}"
}

# rp_upload SECTION ISSUE NAME CONTENT: the person uploads CONTENT as the document NAME of ISSUE
# (empty CONTENT makes a document by its name alone); sets rp_doc and rp_ref, its id and ref_key.
rp_upload() {
  local body
  if [[ -z "$4" ]]; then
    body="$(jq -cn --arg n "$3" '{name: $n}')"
  else
    body="$(jq -cn --arg n "$3" --arg c "$4" '{name: $n, content: $c}')"
  fi
  tree_person_send POST "/api/v1/issues/$2/artifacts" "$body"
  tree_want 201 "$1" "upload $3 to $2"
  tree_field "$1" "upload $3 to $2" .artifact.id
  rp_doc="$tree_value"
  tree_field "$1" "upload $3 to $2" .artifact.ref_key
  rp_ref="$tree_value"
}

# rp_tree SECTION ISSUE [VIEW]: the person's tree read of ISSUE, kept in $work/tree.json.
rp_tree() {
  tree_read person "$2" "${3:-}"
  tree_want 200 "$1" "read the tree of $2${3:+ ?view=$3}"
  jq -e '.cursor == null' "$work/body" >/dev/null ||
    fail "$1" "the tree of $2 answers on one page" "it names a next cursor: $(jq -c .cursor "$work/body")"
  cp "$work/body" "$work/tree.json"
}

# rp_children ISSUE PARENT: the ids of the nodes the tree read last kept lists under PARENT, in its
# order, joined by commas, in answer.
rp_children() {
  answer="$(jq -r --arg p "$2" '[.nodes[] | select(.parent.id == $p) | .id] | join(",")' "$work/tree.json")"
}

# rp_block KEY BLOCK: the node id of block BLOCK of an issue room of KEY, `<key>/tree#<block>`.
rp_block() { printf '%s/tree#%s' "$1" "$2"; }

# rp_lens SECTION ISSUE ON KIND [TRIGGERED_BY]: the person invokes a lens of KIND on ON; sets rp_lens,
# its store address `lens/<id>`.
rp_lens() {
  tree_person_send POST "/api/v1/issues/$2/lenses" \
    "$(jq -cn --arg on "$3" --arg k "$4" --arg t "${5:-}" '{on: $on, kind: $k} + (if $t == "" then {} else {triggered_by: $t} end)')"
  tree_want 201 "$1" "invoke a $4 lens on $3"
  tree_field "$1" "invoke a $4 lens on $3" .id
  rp_lens="lens/$tree_value"
  pass "$1" "invoked the $4 lens $rp_lens on $3"
}

# rp_finding SECTION ISSUE LENS TEXT: the person adds a finding to LENS; sets rp_finding, its id.
rp_finding() {
  tree_person_send POST "/api/v1/issues/$2/nodes/$(q "$3")/findings" "$(jq -cn --arg t "$4" '{text: $t}')"
  tree_want 201 "$1" "add a finding to $3"
  tree_field "$1" "add a finding to $3" .id
  rp_finding="$tree_value"
}

# rp_pointers SECTION ADDRESS KIND: the current far ends of the KIND tuples out of ADDRESS with the
# seq of the tuple that holds each, joined by commas, in answer.
rp_pointers() {
  rp_store "$2"
  with_vars "$1" "read the $3 tuples of $2" \
    "select coalesce(string_agg(h.to_kind || '/' || h.to_id || '@' || h.seq, ',' order by h.to_kind, h.to_id), '')
     from (select distinct on (to_kind, to_id) to_kind, to_id, seq, asserted from graph_tuples
           where from_kind = :'kind' and from_id = :'id' and kind = :'tuple' order by to_kind, to_id, seq desc) h
     where h.asserted" \
    kind="$rp_kind" id="$rp_id" tuple="$3"
}

# rp_same_pointers SECTION ADDRESS KIND BEFORE: the KIND tuples of ADDRESS are BEFORE still, each the
# very tuple read before: a move neither retracted nor appended them again.
rp_same_pointers() {
  rp_pointers "$1" "$2" "$3"
  [[ "$answer" == "$4" ]] ||
    fail "$1" "the $3 tuples of $2 stand as they were" "they read $answer, $4 before: the move retracted and appended them again"
  pass "$1" "the $3 tuples of $2 stand as they were: $answer"
}

# --- subtree: a subtree moves with one appended tuple ------------------------------------------------

section=subtree
# The spec opens with a callout, its memo (R14), which stays the issue's first child through every
# move (bands).
spec=":::callout{#memo kind=\"note\" title=\"\"}
The memo of the reparent check, $stamp.
:::

:::criterion{#k1}
The first criterion holds a requirement.
:::

:::criterion{#k2}
The second criterion takes it.
:::
"
rp_issue "$section" "Reparent check" "$spec"
# Not readonly: the move helpers take a local `key`, which a readonly name refuses.
key="$rp_key"
readonly spec_doc="$rp_spec"
readonly k1="$(rp_block "$key" k1)" k2="$(rp_block "$key" k2)" r1="$(rp_block "$key" r1)" memo="$(rp_block "$key" memo)"
rp_edit "$section" "alice writes R1 under K1" "$spec_doc" "$(jq -cn --arg m ":::requirement{#r1 parent=\"dispatch://$key/spec#b-k1\"}
The moved requirement keeps its words.
:::" '[{op: "insert", after: "block:k1", markdown: $m}]')"
verification="How $key is checked.

:::check{#c1 serves=\"dispatch://$key/spec#b-r1\" surface=\"api\" run=\"true\" negative_control=\"Undo it.\"}
The first check of R1.
:::

:::check{#c2 serves=\"dispatch://$key/spec#b-r1\" surface=\"api\" run=\"true\" negative_control=\"Undo it.\"}
The second check of R1.
:::
"
rp_upload "$section" "$key" verification.md "$verification"
readonly verification_doc="$rp_doc"
readonly c1="$(rp_block "$key" c1)" c2="$(rp_block "$key" c2)"
rp_tree "$section" "$key"
rp_children "$key" "$r1"
[[ "$answer" == "$c1,$c2" ]] || fail "$section" "the checks stand under R1 before the move" "the tree lists under $r1: '$answer'"
rp_pointers "$section" "$c1" serves
readonly c1_serves="$answer"
rp_pointers "$section" "$c2" serves
readonly c2_serves="$answer"
# The first move takes the If-Match from the tree read's placement_seq.
r1_tag="$(jq -r --arg n "$r1" '.nodes[] | select(.id == $n) | .placement_seq' "$work/tree.json")"
rp_tag "$section" "$r1"
[[ "$r1_tag" == "$rp_placed" ]] || fail "$section" "the tree read answers R1's placement_seq" "it answers '$r1_tag', the store $rp_placed"
rp_seq "$section"
subtree_mark="$rp_mark"
rp_move "$section" person "$key" "$r1" "$k2"
rp_untouched "$section" "the move appended no tuple of $c1" "$c1" "$subtree_mark"
rp_untouched "$section" "the move appended no tuple of $c2" "$c2" "$subtree_mark"
rp_tree "$section" "$key"
rp_children "$key" "$r1"
[[ "$answer" == "$c1,$c2" ]] || fail "$section" "both checks stand under the moved R1 in their order" "the tree lists under $r1: '$answer'"
pass "$section" "both checks stand under the moved R1 in their order"
rp_children "$key" "$k2"
[[ ",$answer," == *",$r1,"* ]] || fail "$section" "the tree reads R1 under K2" "it lists under $k2: '$answer'"
rp_children "$key" "$k1"
[[ ",$answer," != *",$r1,"* ]] ||
  fail "$section" "the subtree hangs under one parent" "$r1 hangs under both $k1 and $k2"
pass "$section" "the tree reads R1 under K2 alone"
rp_same_pointers "$section" "$c1" serves "$c1_serves"
rp_same_pointers "$section" "$c2" serves "$c2_serves"

# --- refusals ---------------------------------------------------------------------------------------

section=refusals
rp_refused "$section" person "$key" "$k2" "$c1" 400 INVALID_POINTER "cycle" "$c1"
rp_issue "$section" "Another tree" "The memo of a tree of its own.

:::criterion{#x1}
A criterion of another tree.
:::
"
readonly other_key="$rp_key"
rp_refused "$section" person "$key" "$r1" "$(rp_block "$other_key" x1)" 400 INVALID_POINTER "$other_key" "outside the tree of $key"
rp_tag "$section" "$r1"
fresh="$rp_placed"
rp_seq "$section"
mark="$rp_mark"
rp_patch person "$key" "$r1" "$((fresh - 1))" "$(jq -cn --arg p "$k1" '{parent: $p}')"
[[ "$status" == 412 && "$(jq -r .code "$work/body")" == PRECONDITION_FAILED ]] ||
  fail "$section" "a move with a stale placement revision is refused" "it answered $status: $(body)"
rp_untouched "$section" "the stale move stores nothing" "$r1" "$mark"
rp_parents "$section" "$r1"
[[ "$rp_parent" == "$k2" ]] || fail "$section" "the newer parent still stands" "R1 stands under $rp_parent"
pass "$section" "a stale If-Match is 412 PRECONDITION_FAILED, and R1 still stands under K2"
rp_tree "$section" "$key"
jq -e --arg n "$r1" '.nodes[] | select(.id == $n) | .text | contains("The moved requirement keeps its words.")' "$work/tree.json" >/dev/null ||
  fail "$section" "the move keeps R1's words" "the tree reads $(jq -c --arg n "$r1" '.nodes[] | select(.id == $n) | .text' "$work/tree.json")"
pass "$section" "the move keeps R1's words"
rp_lens "$section" "$key" "dispatch://$key/spec#b-k1" pre-mortem
readonly l1="$rp_lens"
rp_lens "$section" "$key" "dispatch://$key/spec#b-k2" pre-mortem "dispatch://$l1"
readonly l2="$rp_lens"
tree_person_send POST "/api/v1/issues/$key/risks" "$(jq -cn --arg l "$l1" --arg p "$c1" \
  '{lens: $l, heading: "The store outgrows its disk", early_warning: "The disk fills", prevention: "Prune it", prevented_by: $p}')"
tree_want 201 "$section" "write a risk under L1"
tree_field "$section" "write a risk under L1" .id
readonly risk="risk/$tree_value"
rp_pointers "$section" "$l2" has_kind
l2_kind="$answer"
rp_pointers "$section" "$l2" triggered_by
l2_trigger="$answer"
[[ "$l2_kind" == lens_kind/pre-mortem@* && "$l2_trigger" == "$l1@"* ]] ||
  fail "$section" "L2 holds its has_kind and its triggered_by" "it holds has_kind '$l2_kind' and triggered_by '$l2_trigger'"
rp_move "$section" person "$key" "$l2" "$r1"
rp_same_pointers "$section" "$l2" has_kind "$l2_kind"
rp_same_pointers "$section" "$l2" triggered_by "$l2_trigger"
rp_pointers "$section" "$risk" prevented_by
risk_prevented="$answer"
[[ "$risk_prevented" == "block/$c1@"* ]] || fail "$section" "the risk is prevented by C1" "it reads '$risk_prevented'"
rp_move "$section" person "$key" "$risk" "$l2"
rp_same_pointers "$section" "$risk" prevented_by "$risk_prevented"
rp_lens "$section" "$key" "dispatch://$key/spec#b-k1" trade-off
readonly l3="$rp_lens"
rp_refused "$section" person "$key" "$risk" "$l3" 400 INVALID_POINTER
rp_parents "$section" "$risk"
[[ "$rp_parent" == "$l2" ]] || fail "$section" "the risk reads back under L2" "it stands under $rp_parent"
pass "$section" "the store's rule keeps the risk under the pre-mortem L2"

# --- constraint: a constraint moves only under an issue node ----------------------------------------

section=constraint
rp_issue "$section" "Child of the reparent check" "The memo of the child issue.

:::criterion{#y1}
A criterion of the child.
:::
" "$key"
readonly child="$rp_key"
tree_person_send POST "/api/v1/issues/$key/constraints" '{"standard": "Every write answers within a second", "reference": "The latency budget"}'
tree_want 201 "$section" "write a constraint under $key"
tree_field "$section" "write a constraint under $key" .id
readonly constraint="constraint/$tree_value"
rp_refused "$section" person "$key" "$constraint" "$r1" 400 INVALID_POINTER "$tree_value" "$r1" requirement
tree_person_send GET "/api/v1/issues/$key/constraints"
tree_want 200 "$section" "list the constraints of $key"
jq -e --arg c "${constraint#constraint/}" '[.. | objects | select(.id? == $c)] | length > 0' "$work/body" >/dev/null ||
  fail "$section" "the refused constraint is still listed under $key" "the constraint $constraint is listed under neither: $(body)"
rp_move "$section" person "$key" "$constraint" "issue/$child"
tree_person_send GET "/api/v1/issues/$child/constraints"
tree_want 200 "$section" "list the constraints of $child"
jq -e --arg c "${constraint#constraint/}" '[.. | objects | select(.id? == $c)] | length > 0' "$work/body" >/dev/null ||
  fail "$section" "the constraint is listed under $child" "the constraint $constraint is listed under $key, want $child: $(body)"
tree_person_send GET "/api/v1/issues/$key/constraints"
jq -e --arg c "${constraint#constraint/}" '[.. | objects | select(.id? == $c)] | length == 0' "$work/body" >/dev/null ||
  fail "$section" "the constraint is no longer listed under $key" "the constraint $constraint is listed under $key, want $child"
pass "$section" "the constraint is listed under $child and not under $key"

# --- ask: the route moves no ask ---------------------------------------------------------------------

section=ask
rp_edit "$section" "alice writes an ask block into the spec" "$spec_doc" \
  '[{"op": "insert", "after": "end", "markdown": ":::ask{#q1}\nWhich store do the writers take?\n:::"}]'
# The ask's row is written when the room's edit settles, so the read waits for it, at most 60 s.
deadline=$((SECONDS + 60))
while true; do
  with_vars "$section" "read the ask of the block q1" \
    "select coalesce((select id::text from asks where issue_key = :'key' and block_id = 'q1' limit 1), '')" key="$key"
  [[ -z "$answer" ]] || break
  ((SECONDS < deadline)) || fail "$section" "read the ask of the block q1" "no ask holds q1 after 60 s"
  sleep 1
done
readonly ask="ask/$answer"
rp_parents "$section" "$ask"
ask_parent="$rp_parent"
[[ -n "$ask_parent" ]] || fail "$section" "the ask stands under the spec" "it holds no part_of"
rp_refusal "$section" person "$key" "$ask" "$k2" 400 NODE_NOT_MOVABLE "${ask#ask/}"
problems="$rp_problem"
rp_parents "$section" "$ask"
[[ "$rp_parent" == "$ask_parent" ]] ||
  problems+="${problems:+; }the ask's part_of names $rp_parent after the move of $ask, want $ask_parent, the spec's node"
# The census runs whatever the move answered, so a move it took shows as the difference it leaves.
tree_envoy_dispatch_status census --store
if line="$(grep -m1 '^census: store kind=ask_parent difference' "$work/out")"; then
  problems+="${problems:+; }the census reads an ask_parent difference after the move of $ask: $line"
fi
if line="$(grep -m1 '^census: versions' "$work/out")"; then
  [[ "$line" == *" tree_mismatch=0"* && "$line" == *" tree_mismatch_repaired=0"* ]] ||
    problems+="${problems:+; }the census reads $(grep -o 'tree_mismatch=[0-9]*' <<<"$line" || true) $(grep -o 'tree_mismatch_repaired=[0-9]*' <<<"$line" || true), want 0 and 0: $line"
else
  problems+="${problems:+; }envoy-dispatch census --store printed no versions line and exited $tree_envoy_status: $(tail -n 20 "$work/out")"
fi
[[ -z "$problems" ]] ||
  fail "$section" "the route moves no ask, and the census reads no ask_parent difference and no tree mismatch" "$problems"
pass "$section" "the move of $ask answers 400 NODE_NOT_MOVABLE storing nothing, its part_of names $ask_parent, and the census reads no ask_parent difference, and tree_mismatch=0 tree_mismatch_repaired=0"

# --- child: a block, a comment and a lens under the child issue's node --------------------------------

section=child
rp_refused "$section" person "$key" "$k1" "issue/$child" 400 INVALID_POINTER "$k1" "issue/$child" "$child"
tree_person_send POST "/api/v1/issues/$key/comments" '{"body": "A remark on the issue that stays where it was."}'
tree_want 201 "$section" "alice comments on $key"
tree_field "$section" "alice comments on $key" .id
readonly comment="comment/$tree_value"
rp_refusal "$section" person "$key" "$comment" "issue/$child" 400 NODE_NOT_MOVABLE "${comment#comment/}" comment
problems="$rp_problem"
# Both trees are read whatever the move answered.
rp_tree "$section" "$key"
in_key="$(jq -r --arg c "${comment#comment/}" '[.nodes[] | select(.kind == "comment" and .id == $c)] | length' "$work/tree.json")"
rp_tree "$section" "$child"
in_child="$(jq -r --arg c "${comment#comment/}" '[.nodes[] | select(.kind == "comment" and .id == $c)] | length' "$work/tree.json")"
if [[ "$in_key" != 1 && "$in_child" == 0 ]]; then
  problems+="${problems:+; }the comment $comment is listed by the tree of neither $key nor $child"
elif [[ "$in_key" != 1 ]]; then
  problems+="${problems:+; }the comment $comment is listed by the tree of $child, want $key, where it stood"
fi
[[ -z "$problems" ]] || fail "$section" "the route moves no comment, which stays listed under $key" "$problems"
pass "$section" "the move of $comment answers 400 NODE_NOT_MOVABLE storing nothing, and the tree of $key lists it, where it stood"
rp_move "$section" person "$key" "$l3" "issue/$child"
rp_tree "$section" "$child"
jq -e --arg l "${l3#lens/}" '[.nodes[] | select(.kind == "lens" and .id == $l)] | length == 1' "$work/tree.json" >/dev/null ||
  fail "$section" "L3 is listed by the tree of $child" "the lens $l3 is not listed by the tree of $child"
pass "$section" "L3 is listed by the tree of $child"

# --- removable: no move puts a node where a removal would leave it live behind no row ----------------

section=removable
rp_upload "$section" "$key" "U $stamp" ""
readonly u_doc="$rp_doc" u_ref="$rp_ref" u_node="artifact/$rp_ref"
rp_upload "$section" "$key" "W $stamp" "W holds writing."
readonly w_node="artifact/$rp_ref"
rp_refused "$section" person "$child" "$constraint" "$u_node" 400 INVALID_POINTER
rp_lens "$section" "$key" "dispatch://$key/spec#b-k1" trade-off
readonly l4="$rp_lens"
rp_finding "$section" "$key" "$l4" "The fourth lens finds the store slow."
readonly f4="$rp_finding"
rp_finding "$section" "$key" "$l1" "The first lens finds the disk full."
readonly f1="$rp_finding"
rp_move "$section" person "$key" "$l4" "$u_node"
rp_value "$section" "$l4" artifact_id
[[ "$answer" == "$u_doc" ]] || fail "$section" "L4 records U's row as its artifact_id" "it records '$answer', want $u_doc"
pass "$section" "L4 records U's row $u_doc as its artifact_id"
rp_refused "$section" person "$key" "$f1" "$u_node" 409 PARENT_DOCUMENT_REMOVABLE "$f1" finding "$u_node" "$u_ref"
rp_move "$section" person "$key" "$f1" "$l4"
rp_edit "$section" "alice writes R9 under K1" "$spec_doc" "$(jq -cn --arg m ":::requirement{#r9 parent=\"dispatch://$key/spec#b-k1\"}
A requirement with nothing under it.
:::" '[{op: "insert", after: "block:r1", markdown: $m}]')"
readonly r9="$(rp_block "$key" r9)"
rp_refused "$section" person "$key" "$r9" "$l4" 409 PARENT_DOCUMENT_REMOVABLE "$r9" requirement
rp_move "$section" person "$key" "$r9" "$w_node"
tree_person_send POST "/api/v1/issues/$key/comments" "$(jq -cn --arg u "dispatch://$(rp_named "$u_node")" '{body: ("This cites " + $u + " and leaves it empty.")}')"
tree_want 201 "$section" "alice writes a comment citing U"
rp_refused "$section" person "$key" "$r9" "$l4" 409 PARENT_DOCUMENT_REMOVABLE "$r9" requirement "$u_ref"

# --- depth: the rule holds two levels down, and reads the whole subtree ------------------------------

section=depth
rp_move "$section" person "$key" "$f1" "$f4"
rp_refused "$section" person "$key" "$r9" "$f1" 409 PARENT_DOCUMENT_REMOVABLE "$r9"
rp_lens "$section" "$key" "dispatch://$key/spec#b-k1" trade-off
readonly l5="$rp_lens"
rp_move "$section" person "$key" "$r9" "$l5"
for parent in "$u_node" "$l4"; do
  rp_refused "$section" person "$key" "$l5" "$parent" 409 PARENT_DOCUMENT_REMOVABLE "$l5" lens "$parent" "$u_ref" "its subtree holds a node of a kind that may not stand there"
  jq -e --arg a "$r9" --arg b "$(rp_named "$r9")" '.error | (contains($a) or contains($b)) | not' "$work/body" >/dev/null ||
    fail "$section" "the refusal of the move of $l5 under $parent leaves the subtree's node unnamed" \
      "the refusal of the move of $l5 under $parent reads $(jq -r .error "$work/body"), want the subtree's node of a kind that may not stand there, unnamed"
done
pass "$section" "the refusals of L5 name L5, U's node and U, and not the requirement under it"

# --- rowless: a document node whose row has gone counts as removable -----------------------------------

section=rowless
rp_upload "$section" "$key" "V $stamp" ""
readonly v_doc="$rp_doc" v_ref="$rp_ref" v_node="artifact/$rp_ref"
# The lease of the block index's tick, held on a psql session of the driver's own, so no tick and no
# sweep of one states V's node gone while the move reads it.
coproc lease { env -u PGHOSTADDR psql "$database_url" -X -q -At 2>&1; }
lease_pid="$lease_PID"
printf "select pg_try_advisory_lock(1280266064, hashtext('block-index'));\n" >&"${lease[1]}"
read -r -t 60 held <&"${lease[0]}" || fail "$section" "take the block-index lease" "the psql session answered nothing"
[[ "$held" == t ]] || fail "$section" "take the block-index lease" "pg_try_advisory_lock answered '$held': a tick holds it"
pass "$section" "the driver holds the block-index lease"
psql_run "$section" "delete V's rows as main's removal deletes them" -v doc="$v_doc" <<'SQL'
begin;
delete from artifact_versions where artifact_id = :'doc'::uuid;
delete from doc_updates where artifact_id = :'doc'::uuid;
delete from doc_snapshots where artifact_id = :'doc'::uuid;
delete from doc_checkpoints where artifact_id = :'doc'::uuid;
delete from document_search where artifact_id = :'doc'::uuid;
delete from doc_compaction_owed where artifact_id = :'doc'::uuid;
delete from search_term_queue where artifact_id = :'doc'::uuid;
delete from user_threads where artifact_id = :'doc'::uuid;
delete from refs where from_kind = 'artifact' and from_id = :'doc';
delete from refs_grammar_owed where id = :'doc'::uuid;
delete from reference_excerpt_backoff where artifact_id = :'doc'::uuid;
delete from artifacts where id = :'doc'::uuid;
commit;
SQL
want_value "$section" "V's node is live and no artifacts row stands at its address" "live|0" \
  "select (select state from graph_nodes where kind = 'artifact' and id = '$v_ref') || '|' || (select count(*) from artifacts where ref_key = '$v_ref')"
# With no row, the loader names V's node by the address the move gave, and V by its ref_key.
rp_refused "$section" person "$key" "$r9" "$v_node" 409 PARENT_DOCUMENT_REMOVABLE "$r9" requirement "$v_ref"
exec {lease[1]}>&-
wait "$lease_pid" 2>/dev/null || true
lease_pid=""
pass "$section" "the driver released the block-index lease"

# --- state: a dead branch stops counting as open ------------------------------------------------------

section=state
rp_edit "$section" "alice writes R3 and its sibling R4 under K1" "$spec_doc" "$(jq -cn --arg a ":::requirement{#r3 parent=\"dispatch://$key/spec#b-k1\"}
The third requirement has no check.
:::" --arg b ":::requirement{#r4 parent=\"dispatch://$key/spec#b-k1\"}
Its sibling has no check either.
:::" '[{op: "insert", after: "end", markdown: $a}, {op: "insert", after: "end", markdown: $b}]')"
readonly r3="$(rp_block "$key" r3)" r4="$(rp_block "$key" r4)"
rp_upload "$section" "$key" plan.md "The plan of $key.

:::plan_task{#t3 serves=\"dispatch://$key/spec#b-r3\"}
Build what R3 asks.
:::

:::plan_task{#t4}
A second step of the plan.
:::

:::plan_task{#t5}
A third step of the plan.
:::
"
# rp_coverage SECTION WANT: the coverage read of the issue answers R3 and R4 as WANT, a JSON object of
# block id to `gap` (a requirement listed with gaps) or `none` (not listed, or listed with none).
rp_coverage() {
  local got
  tree_person_send GET "/api/v1/issues/$key/coverage"
  tree_want 200 "$1" "read the coverage of $key"
  got="$(jq -cS '[.requirements] | (["r3", "r4"] | map({key: ., value: "none"}) | from_entries) as $none
    | $none + ([.[0][] | select(.block_id == "r3" or .block_id == "r4") | {key: .block_id, value: (if ((.gaps // []) | length) > 0 then "gap" else "none" end)}] | from_entries)' "$work/body")"
  [[ "$got" == "$(jq -cS . <<<"$2")" ]] || fail "$1" "the coverage reads $2" "it reads $got: $(jq -c '[.requirements[] | {block_id, gaps}]' "$work/body")"
  pass "$1" "the coverage reads $got"
}
# rp_plan_position SECTION WANT: the plan view answers T3 a position (WANT `held`) or none (`none`).
rp_plan_position() {
  local got
  rp_tree "$1" "$key" plan
  got="$(jq -r '[.nodes[] | select(.block_id == "t3")][0].plan.position // "none"' "$work/tree.json")"
  case "$2" in
    held) [[ "$got" =~ ^[0-9]+$ ]] || fail "$1" "the plan view gives T3 a position" "it gives '$got'" ;;
    none) [[ "$got" == none ]] || fail "$1" "the plan view gives T3 no position" "it gives $got" ;;
  esac
  pass "$1" "the plan view gives T3 position $got"
}
rp_coverage "$section" '{"r3": "gap", "r4": "gap"}'
rp_plan_position "$section" held
tree_person_send POST "/api/v1/issues/$key/nodes/$(q "$r3")/state" '{"state": "overtaken"}'
tree_want 200 "$section" "set R3 overtaken"
rp_coverage "$section" '{"r3": "none", "r4": "gap"}'
rp_plan_position "$section" none
with_vars "$section" "read the state tuples of R3" \
  "select string_agg(to_id || '@' || at::text, ',' order by seq) from graph_tuples where from_kind = 'block' and from_id = :'node' and kind = 'state'" node="$r3"
[[ "$answer" == live@*,overtaken@* ]] || fail "$section" "R3's state appends, keeping the earlier value with its time" "its state tuples read $answer"
pass "$section" "R3's state tuples read $answer"
tree_person_send POST "/api/v1/issues/$key/nodes/$(q "$r3")/state" '{"state": "propsed"}'
tree_want 400 "$section" "a state outside the five is refused"
expect "$section" "the refusal names the attribute and the value" '["INVALID_ATTRIBUTE_VALUE",true,true]' \
  '[.code, (.error | contains("state")), (.error | contains("propsed"))]'

# --- outline: the outline's moves through PATCH alone ---------------------------------------------------

section=outline
with_vars "$section" "read the tree artifact of $key" "select id::text from artifacts where ref_key = :'ref'" ref="$key/tree"
readonly tree_doc="$answer"
rp_edit "$section" "alice writes P1, P2 and P3 into the room's own fragment" "$tree_doc" \
  '[{"op": "insert", "after": "end", "markdown": "The first prose block of the room, P1."}, {"op": "insert", "after": "end", "markdown": "The second prose block of the room, P2."}, {"op": "insert", "after": "end", "markdown": "The third prose block of the room, P3."}, {"op": "insert", "after": "end", "markdown": "The fourth prose block of the room, P4."}]'
# rp_paragraph SECTION WORDS: the node id of the block of the room whose text begins with WORDS, in
# answer.
rp_paragraph() {
  with_vars "$1" "read the block that reads '$2'" \
    "select coalesce((select d.node_id from document_blocks d where d.artifact_id = coalesce((select room from document_rooms where artifact_id = :'doc'::uuid), :'doc'::uuid)
       and d.excerpt like :'words' || '%' limit 1), '')" doc="$tree_doc" words="$2"
  [[ -n "$answer" ]] || fail "$1" "read the block that reads '$2'" "no block of the room reads it"
}
rp_paragraph "$section" "The second prose block"
readonly p2="$answer"
rp_paragraph "$section" "The third prose block"
readonly p3="$answer"
rp_paragraph "$section" "The first prose block"
readonly p1="$answer"
rp_paragraph "$section" "The fourth prose block"
readonly p4="$answer"
tree_person_send POST "/api/v1/issues/$key/comments" "$(jq -cn '{body: "A remark on P2.", anchor: {artifact: "tree", quote: "The second prose block of the room, P2."}}')"
tree_want 201 "$section" "alice comments on P2"
tree_field "$section" "alice comments on P2" .id
readonly p2_comment="comment/$tree_value"
rp_parents "$section" "$p2_comment"
[[ "$rp_parent" == "$p2" ]] || fail "$section" "the comment hangs under P2" "it hangs under $rp_parent"
with_vars "$section" "read the newest version of the tree artifact" "select max(number) from artifact_versions where artifact_id = :'doc'::uuid" doc="$tree_doc"
tree_version="$answer"
# rp_row SECTION NODE: what a move of NODE keeps of its text, in answer: the md5 of its row's text
# digest and changed_version and of its Keep, the kept_by and kept_text_sum tuples of its node.
rp_row() {
  with_vars "$1" "read the row and the Keep of $2" \
    "select md5(coalesce((select text_sha256 || '|' || coalesce(changed_version::text, '') from document_blocks where node_id = :'node' limit 1), '')
       || '|' || coalesce((select string_agg(kind || ':' || to_id || ':' || asserted::text, ',' order by seq) from graph_tuples
                           where from_kind = 'block' and from_id = :'node' and kind in ('kept_by', 'kept_text_sum')), ''))" node="$2"
}
rp_row "$section" "$p3"
p3_row="$answer"
rp_seq "$section"
p3_mark="$rp_mark"
rp_tree "$section" "$key" spec
p3_tag="$(jq -r --arg n "$p3" '.nodes[] | select(.id == $n) | .placement_seq // empty' "$work/tree.json")"
[[ -n "$p3_tag" ]] || fail "$section" "the tree read answers P3's placement_seq" "the spec view lists no $p3"
# rp_chain SECTION NODE PARENT AFTER WANT_AFTER: moves NODE under PARENT with AFTER (a JSON value) and
# the If-Match of the last answer's placement_seq, $chain_tag, and wants 200 naming PARENT and
# WANT_AFTER (`null` or a node); the answer's placement_seq becomes $chain_tag.
rp_chain() {
  rp_patch person "$key" "$2" "$chain_tag" "$(jq -cn --arg p "$3" --argjson a "$4" '{parent: $p, after: $a}')"
  [[ "$status" == 200 ]] || fail "$1" "the move of $2 under $3" "the move of $2 under $3 answered $status, want 200: $(body)"
  expect "$1" "the move of $2 answers its parent and the sibling it follows" "$(jq -cn --arg p "$3" --arg a "$5" '[$p, (if $a == "null" then null else $a end)]')" '[.parent, .after]'
  chain_tag="$(jq -er '.placement_seq | numbers' "$work/body")" ||
    fail "$1" "the move of $2 answers its placement_seq" "PATCH of $2 answered without placement_seq for the next If-Match: $(body)"
  moves=$((moves + 1))
}
# rp_stored SECTION NODE PARENT: NODE's part_of names PARENT, and its placement is stored.
rp_stored() {
  rp_parents "$1" "$2"
  [[ "$rp_parent" == "$3" ]] || fail "$1" "$2 stands under $3" "it stands under '$rp_parent'"
  rp_value "$1" "$2" placement
  [[ "$answer" == stored ]] || fail "$1" "$2 holds the placement stored" "it holds '$answer', part_of $rp_parent"
  pass "$1" "$2 stands under $3 with the placement stored"
}
# rp_rank SECTION NODE: NODE's rank, in answer.
rp_rank() { rp_value "$1" "$2" rank; }
chain_tag="$p3_tag"
rp_chain "$section" "$p3" "$p2" null null
rp_stored "$section" "$p3" "$p2"
rp_chain "$section" "$p3" "issue/$key" "$(jq -cn --arg a "$p2" '$a')" "$p2"
rp_stored "$section" "$p3" "issue/$key"
rp_chain "$section" "$p3" "$p2" null null
rp_stored "$section" "$p3" "$p2"
pass "$section" "the indent, outdent and second indent of P3 chained each answer's placement_seq"
rp_tree "$section" "$key" spec
first_under_p2="$(jq -r --arg p "$p2" '[.nodes[] | select(.parent.id == $p and .rank != null)][0].id // ""' "$work/tree.json")"
[[ "$first_under_p2" == "$p3" ]] || fail "$section" "P3 is first among P2's ranked children" "the first is '$first_under_p2'"
pass "$section" "P3 is first among P2's ranked children"
with_vars "$section" "read the newest version of the tree artifact" "select max(number) from artifact_versions where artifact_id = :'doc'::uuid" doc="$tree_doc"
[[ "$answer" == "$tree_version" ]] || fail "$section" "Markdown keeps P3 flat under P2" "the moves wrote version $answer of the tree artifact, $tree_version before"
rp_since "$section" "$p3" "$p3_mark"
if tr ',' '\n' <<<"$answer" | grep -qE '^(text|text_parent|text_rank):'; then
  fail "$section" "the moves of P3 write no text value, text_parent or text_rank" "they appended $answer"
fi
rp_row "$section" "$p3"
[[ "$answer" == "$p3_row" ]] || fail "$section" "P3's text digest, changed_version and Keep stay" "its row or its Keep moved"
pass "$section" "Markdown keeps P3 flat: no version, no text tuple, its digest, changed_version and Keep unchanged"
# A check up and down among the checks under R1, then a criterion, a requirement and a plan task in
# and out.
rp_move "$section" person "$key" "$c1" "$r1" "$c2"
rp_stored "$section" "$c1" "$r1"
rp_move "$section" person "$key" "$c1" "$r1" null
rp_stored "$section" "$c1" "$r1"
rp_edit "$section" "alice writes a third criterion K3" "$spec_doc" '[{"op": "insert", "after": "block:k2", "markdown": ":::criterion{#k3}\nA third criterion, indented and outdented.\n:::"}]'
readonly k3="$(rp_block "$key" k3)" t4="$(rp_block "$key" t4)" t5="$(rp_block "$key" t5)"
for pair in "$k3 $k2" "$r4 $r3" "$t5 $t4"; do
  read -r node above <<<"$pair"
  rp_parents "$section" "$node"
  home="$rp_parent"
  rp_move "$section" person "$key" "$node" "$above" null
  rp_stored "$section" "$node" "$above"
  rp_move "$section" person "$key" "$node" "$home" "$above"
  rp_stored "$section" "$node" "$home"
done
# A text pass after the moves keeps every stored place.
declare -A held_parent=() held_rank=()
for node in "$p3" "$c1" "$k3" "$r4" "$t5"; do
  rp_parents "$section" "$node"
  held_parent[$node]="$rp_parent"
  rp_rank "$section" "$node"
  held_rank[$node]="$answer"
done
rp_edit "$section" "alice edits P1's words alone" "$tree_doc" '[{"op": "replace", "find": "The first prose block of the room, P1.", "with": "The first prose block of the room, P1, reworded."}]'
corpus_wait_indexed() {
  local deadline=$((SECONDS + 120))
  while true; do
    query "$1" "read whether a document of $key owes its index" \
      "select count(*) from document_block_versions v join artifacts a on a.id = v.artifact_id where a.issue_key = '$key' and (v.version is null or v.store_owed_at is not null)"
    [[ "$answer" != 0 ]] || return 0
    ((SECONDS < deadline)) || fail "$1" "the index passes of $key run" "after 120 s $answer documents still owe theirs"
    sleep 1
  done
}
corpus_wait_indexed "$section"
for node in "$p3" "$c1" "$k3" "$r4" "$t5"; do
  rp_parents "$section" "$node"
  rp_rank "$section" "$node"
  [[ "$rp_parent" == "${held_parent[$node]}" && "$answer" == "${held_rank[$node]}" ]] ||
    fail "$section" "the next index pass keeps the place of $node" "it stands under $rp_parent at rank $answer, ${held_parent[$node]} at ${held_rank[$node]} before"
  rp_value "$section" "$node" placement
  [[ "$answer" == stored ]] || fail "$section" "$node keeps the placement stored" "it holds '$answer'"
done
pass "$section" "the next index pass keeps every stored place and rank"
# A paragraph inserted between the two paragraphs a stored paragraph's rank stands between takes a
# rank of its own, and the stored rank stays.
rp_move "$section" person "$key" "$p4" "issue/$key" "$p1"
rp_rank "$section" "$p4"
p4_rank="$answer"
rp_edit "$section" "alice inserts P5 between P1 and P2 in the text" "$tree_doc" '[{"op": "insert", "after": "The first prose block of the room, P1, reworded.", "markdown": "The fifth prose block of the room, P5."}]'
corpus_wait_indexed "$section"
rp_paragraph "$section" "The fifth prose block"
readonly p5="$answer"
rp_rank "$section" "$p5"
p5_rank="$answer"
with_vars "$section" "read the ranks under $key" \
  "select count(*) from (select distinct on (t.from_kind, t.from_id) t.from_id, r.to_id as rank from graph_tuples t
     join lateral (select to_id from graph_tuples x where x.from_kind = t.from_kind and x.from_id = t.from_id and x.kind = 'rank' order by seq desc limit 1) r on true
     where t.kind = 'part_of' and t.to_kind = 'issue' and t.to_id = :'key' and t.asserted order by t.from_kind, t.from_id, t.seq desc) s
   where s.rank = :'rank' and s.from_id <> :'node'" key="$key" rank="$p5_rank" node="$p5"
[[ -n "$p5_rank" && "$answer" == 0 ]] || fail "$section" "P5's rank is no rank under the issue" "the inserted paragraph $p5 and the stored paragraph holding one rank $p5_rank"
rp_rank "$section" "$p4"
[[ "$answer" == "$p4_rank" ]] || fail "$section" "the stored P4 keeps its rank" "it reads $answer, $p4_rank before"
pass "$section" "P5 takes the rank $p5_rank of its own, and the stored P4 keeps $p4_rank"
# An edit naming another requirement in the moved check's parent attribute places it there.
rp_edit "$section" "alice names R4 in C1's parent attribute" "$verification_doc" "$(jq -cn --arg s "dispatch://$key/spec#b-r1" --arg p "dispatch://$key/spec#b-r4" \
  '[{op: "retype", block: "c1", type: "check", attributes: {serves: $s, parent: $p, surface: "api", run: "true", negative_control: "Undo it."}}]')"
corpus_wait_indexed "$section"
rp_parents "$section" "$c1"
rp_value "$section" "$c1" placement
[[ "$rp_parent" == "$r4" && "$answer" == authored ]] ||
  fail "$section" "the edit places C1 under R4, authored" "the check $c1 still stands under $rp_parent with the placement '$answer' after the edit that names $r4"
pass "$section" "the edit places C1 under R4 with the placement authored"
# The input refusals, each storing nothing: no If-Match, a stale one, a body that names no move,
# and three afters that name no ranked sibling of the check's band - a node that is no child of the
# parent, the comment under P2, which holds no rank, and a requirement under K1, which stands in the
# spec's band where the check stands in the Verification document's.
rp_tag "$section" "$c2"
c2_tag="$rp_placed"
rp_seq "$section"
mark="$rp_mark"
rp_patch person "$key" "$c2" "" "$(jq -cn --arg p "$r1" '{parent: $p, after: null}')"
[[ "$status" == 428 ]] || fail "$section" "a PATCH with no If-Match is refused" "PATCH with no If-Match answered $status, not 428"
expect "$section" "a PATCH with no If-Match is 428 PRECONDITION_REQUIRED" '"PRECONDITION_REQUIRED"' .code
rp_patch person "$key" "$c2" "$((c2_tag - 1))" "$(jq -cn --arg p "$r1" '{parent: $p, after: null}')"
tree_want 412 "$section" "a stale tag is refused"
expect "$section" "a stale tag is 412 PRECONDITION_FAILED" '"PRECONDITION_FAILED"' .code
rp_patch person "$key" "$c2" "$c2_tag" '{}'
tree_want 400 "$section" "a body with neither parent nor after is refused"
expect "$section" "a body with neither parent nor after is 400 MOVE_INPUT" '"MOVE_INPUT"' .code
for pair in "$r1 $k1" "$p2 $p2_comment" "$k1 $r3"; do
  read -r under after <<<"$pair"
  rp_patch person "$key" "$c2" "$c2_tag" "$(jq -cn --arg p "$under" --arg a "$after" '{parent: $p, after: $a}')"
  [[ "$status" == 400 && "$(jq -r .code "$work/body")" == INVALID_POINTER ]] ||
    fail "$section" "an after of $after under $under is refused" "it answered $status: $(body)"
  jq -e '.error | contains("after")' "$work/body" >/dev/null || fail "$section" "the refusal of an after of $after names after" "it reads $(body)"
done
rp_untouched "$section" "the refused inputs store nothing" "$c2" "$mark"
pass "$section" "no If-Match 428, a stale one 412, an empty body 400 MOVE_INPUT, three bad afters 400 INVALID_POINTER naming after, each storing nothing"
# The removal of P2 from the text places the stored P3 by its text again.
rp_edit "$section" "alice deletes P2 from the room's text" "$tree_doc" "$(jq -cn --arg b "${p2#*#}" '[{op: "delete", block: $b}]')"
corpus_wait_indexed "$section"
rp_tree "$section" "$key" spec
p3_parent="$(jq -r --arg n "$p3" '.nodes[] | select(.id == $n) | .parent.id // ""' "$work/tree.json")"
[[ -n "$p3_parent" ]] || fail "$section" "P3 stands in the tree read after P2's removal" "P3 is missing from the tree read after P2's deletion"
rp_value "$section" "$p3" placement
[[ "$answer" == text ]] || fail "$section" "P3 takes the placement text" "it holds '$answer' under $p3_parent"
pass "$section" "P3 stands under $p3_parent with the placement text"

# --- bands: after: null puts a node first in its own band -----------------------------------------------

section=bands
# rp_band_order SECTION NODE BAND: under the issue, NODE holds a rank of BAND, the first byte its
# document gives every rank of it (RankBandSpec 1, RankBandTree 2, RankBandVerification 3,
# RankBandPlan 4, RankBandOther 5 in refs/block_nodes.go); it stands after every ranked child of an
# earlier band and first in its own band, but in the spec's band right after the memo; and the memo
# stands first of all. The children are read from the store, each a block's node id and its rank.
rp_band_order() {
  local section="$1" node="$2" want="$3" order mine band first
  with_vars "$section" "read the ranked children of $key" \
    "select string_agg(s.from_id || '=' || s.rank, ',' order by s.rank collate \"C\") from (
       select distinct on (t.from_kind, t.from_id) t.from_id, t.asserted,
              (select to_id from graph_tuples x where x.from_kind = t.from_kind and x.from_id = t.from_id and x.kind = 'rank' order by seq desc limit 1) as rank
       from graph_tuples t where t.kind = 'part_of' and t.to_kind = 'issue' and t.to_id = :'key' and t.from_kind = 'block'
       order by t.from_kind, t.from_id, t.seq desc) s
     where s.asserted and s.rank is not null" key="$key"
  order="$answer"
  mine="$(tr ',' '\n' <<<"$order" | awk -F= -v n="$node" '$1 == n { print; exit }')"
  [[ -n "$mine" ]] || fail "$section" "$node holds a rank under $key" "the children read $order"
  band="${mine#*=}"
  band="${band:0:1}"
  [[ "$band" == "$want" ]] || fail "$section" "$node holds a rank of band $want" "its rank ${mine#*=} stands in band $band"
  first="$(tr ',' '\n' <<<"$order" | awk -F= -v b="$band" -v m="$memo" 'substr($2, 1, 1) == b && $1 != m { print; exit }')"
  [[ "$first" == "$mine" ]] ||
    fail "$section" "$node goes first in its band $band" "the band reads $(tr ',' '\n' <<<"$order" | awk -F= -v b="$band" 'substr($2, 1, 1) == b' | paste -sd, -)"
  [[ "$(tr ',' '\n' <<<"$order" | head -n1)" == "$memo="* ]] || fail "$section" "the memo stays first" "the children read $order"
  pass "$section" "$node stands first in band $band, after every earlier band, the memo first: $order"
}
for pair in "$r3 1" "$p1 2" "$c2 3" "$t5 4"; do
  read -r node band <<<"$pair"
  rp_move "$section" person "$key" "$node" "issue/$key" null
  rp_band_order "$section" "$node" "$band"
done
# The last band: an uploaded document's blocks stand under the document's node, which the skeleton
# does not open, so both are moved under the issue and read from the store. The second paragraph
# goes first, last in its band, so the first, moved with after null, has a sibling of its band to
# precede.
rp_upload "$section" "$key" "Notes $stamp" "The notes hold one paragraph that moves.

A second paragraph of the notes."
readonly notes_doc="$rp_doc"
corpus_wait_indexed "$section"
# rp_notes_block SECTION WORDS: the node id of the block of the notes whose text begins with WORDS,
# in answer, read in the room the notes are edited in.
rp_notes_block() {
  with_vars "$1" "read the block of the notes that reads '$2'" \
    "select coalesce((select d.node_id from document_blocks d where d.artifact_id = coalesce((select room from document_rooms where artifact_id = :'doc'::uuid), :'doc'::uuid)
       and d.excerpt like :'words' || '%' limit 1), '')" doc="$notes_doc" words="$2"
  [[ -n "$answer" ]] || fail "$1" "read the block of the notes that reads '$2'" "no block of the notes reads it"
}
rp_notes_block "$section" "A second paragraph of the notes"
readonly notes_second="$answer"
rp_notes_block "$section" "The notes hold"
readonly notes_block="$answer"
rp_move "$section" person "$key" "$notes_second" "issue/$key"
rp_move "$section" person "$key" "$notes_block" "issue/$key" null
rp_band_order "$section" "$notes_block" 5
rp_preceding "$section" "$notes_second" "issue/$key"
[[ "$rp_before" == "$notes_block" ]] ||
  fail "$section" "the first block of the notes precedes the other block of its band" "the second block of the notes follows $rp_before"
pass "$section" "the first block of the notes stands first in band 5, before $notes_second"

# --- request: the open request on the spec query names the drained version -------------------------------

section=request
with_vars "$section" "read the spec's newest version" "select max(number) from artifact_versions where artifact_id = :'doc'::uuid" doc="$spec_doc"
readonly before_move="$answer"
tree_person_send POST "/api/v1/artifacts/$spec_doc/approval-requests" '{}'
[[ "$status" == 201 || "$status" == 200 ]] || fail "$section" "open an approval request on $key's spec query node" "it answered $status: $(body)"
tree_field "$section" "open an approval request on $key's spec query node" .ask.id
readonly request="$tree_value"
rp_move "$section" person "$key" "$r4" "$k2"
deadline=$((SECONDS + 60))
while true; do
  query "$section" "read whether $key's queries are owed" "select count(*) from issue_query_marks where issue_key = '$key'"
  [[ "$answer" != 0 ]] || break
  ((SECONDS < deadline)) || fail "$section" "the drain of $key runs" "the mark of $key stands after 60 s"
  sleep 1
done
with_vars "$section" "read the request and the spec's newest version" \
  "select coalesce((select artifact_version::text from asks where id = :'ask'::uuid), '') || '|' || (select max(number) from artifact_versions where artifact_id = :'doc'::uuid)" \
  ask="$request" doc="$spec_doc"
IFS='|' read -r named newest <<<"$answer"
((newest > before_move)) || fail "$section" "the drain writes a version of the spec query" "the spec query of $key stands at version $newest, $before_move before the move of $r4"
[[ "$named" == "$newest" ]] ||
  fail "$section" "the request on the spec query names the drained version" "the request on $spec_doc names version $named after the drain, want $newest"
pass "$section" "the request $request names version $named, the newest, which the drain wrote after the move of $r4"

# --- routes: the node routes this head serves -------------------------------------------------------------

section=routes
tree_send GET /api/v1
tree_want 200 "$section" "read GET /api/v1"
got="$(jq -cS '[.. | objects | select((.path? // .pattern? // "") == "/api/v1/issues/{key}/nodes/{id}") | (.method // .methods // empty)] | flatten | sort' "$work/body")"
[[ "$got" == '["DELETE","PATCH"]' ]] || fail "$section" "GET /api/v1 lists PATCH and DELETE under /api/v1/issues/{key}/nodes/{id}" "it lists $got"
pass "$section" "GET /api/v1 lists PATCH and DELETE under /api/v1/issues/{key}/nodes/{id}"
# rp_node_rows FILE: the route rows of FILE under /api/v1/issues/{key}/nodes/{id}, each its method or
# its last segment, sorted and joined by commas, in answer.
rp_node_rows() {
  answer="$(grep -oE '\{http\.Method[A-Za-z]+, "/api/v1/issues/\{key\}/nodes/\{id\}(/[a-z]+)?"' "$1" |
    sed -E 's#\{http\.Method([A-Za-z]+), "/api/v1/issues/\{key\}/nodes/\{id\}(/([a-z]+))?"#\1 \3#' |
    awk '{ print ($2 == "" ? toupper($1) : $2) }' | sort | paste -sd, -)" || answer=""
}
cp "$routes_table" "$work/routes_table.go"
printf '\t\t{http.MethodPost, "/api/v1/issues/{key}/nodes/{id}/move", authAny, "a row the grep must see"},\n' >>"$work/routes_table.go"
rp_node_rows "$work/routes_table.go"
[[ ",$answer," == *",move,"* ]] || fail "$section" "the grep of the route rows finds a /move row written into a scratch copy" "it reads $answer"
rp_node_rows "$routes_table"
[[ "$answer" == "DELETE,PATCH,findings,pointers,settle,state" ]] ||
  fail "$section" "the routes table's rows under /api/v1/issues/{key}/nodes/{id} are exactly PATCH, DELETE, findings, pointers, state and settle" "they read $answer"
pass "$section" "the routes table's rows under /api/v1/issues/{key}/nodes/{id} are exactly $answer"

printf 'PASS: %s complete %d moves\n' "$driver" "$moves"
