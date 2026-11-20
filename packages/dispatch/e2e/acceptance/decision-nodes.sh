#!/usr/bin/env bash
# The check c-census: the child's census over a corpus half of which the release
# before wrote, the conversion of every section of the old decisions document, and the node read
# set beside the document read it replaces.
#
# It drops and creates the database DATABASE_URL names (seed_reset_database: a database on this
# machine whose name begins infra_1150_), boots the base release on it through lib-baseline.sh
# (BASELINE_BINARY, default $TMPDIR/base-dispatch, the stamped build of BASELINE_SHA) with the dev
# sign-in, the pinned agent bearer e2e-token and the hold setting (DISPATCH_TEST_HOOKS=1 and
# DISPATCH_TEST_HOLD_DECISIONS_STEP=1, through BASELINE_EXTRA_ENV), and has seed-decisions.sh write
# the corpus through that server's API (seed_decisions, then seed_decisions_further_kinds, which
# adds to issue A one section of each of R9's four further kinds), with one more ask the base
# writes on a project document, so the asks rows it wrote name an anchor, an owner and a follower.
# Before this head boots it
# captures the document answer, the base's GET /api/v1/issues/{key}/decisions of each issue that
# holds a decisions.md, with the ask node each section names or holds, the author of the version
# each section last changed in, and stops the server.
#
# BASELINE_SHA, the commit of the release this branch lands after, is required and has no
# default: only the caller knows which release that is. Once the base server has booted, the
# commit it is stamped with (the `commit` of its /healthz, the one it registers in
# server_processes) must be the commit BASELINE_SHA names in this checkout, or the run fails
# naming both, so that a binary built from another commit never stands in for that release.
#
# It boots this head, HEAD_BINARY (default $TMPDIR/bin/envoy-dispatch), with the hold setting, and
# reads graph_edges over two asks rows the base wrote: the old anchored_to, owned_by and followed_by
# arms answer them and the tuple arm does not (the rows are unmarked and their nodes state no such
# tuple); it runs `envoy-dispatch backfill-graph-store`, the step's first phase, and reads the
# reverse - the rows marked with asks.node_kind and asks.node_address, the tuples stated, each edge
# still answered once - with the three old arms still in the view, and the census's unmarked rows,
# a class of their own, read before and after. It then writes this head's half through this head's
# API (project NODES): a spec holding four requirements, an ask a person answers in their own words
# that shapes the first and the fourth, one left open, one declined with a note, a session that
# follows the answered ask, and an agent's decision with its uncertainty and its reason that shapes
# the first three, settles the ask and holds a reading; and a second document holding an ask, a
# copy of the answered ask and an agent's decision whose block id repeats the spec's.
#
# It runs the restore and the conversion through their entry points
# (`envoy-dispatch restore-answered-asks` and `convert-decisions`, --project SEED) and the store
# census (`envoy-dispatch census --store`). It first reads the section of issue E, which cites the
# answered ask its person removed from the spec through the base release: R9 deletes it, its
# heading takes one moved_to to that ask's node, and the census's moved_to store-only line counts
# every moved_to the store states now, that one among them, with unresolved=0, which a moved_to
# reads only where its far end is a block that stands or a live ask node. For each of shaped,
# from_ask, decided_by, followed_by, reading_parent, ask_parent, ask_anchor and ask_answer it then
# prints the store's count, the projection's count and the differences in each direction, and
# requires rows on both sides and zero differences; it does the same for graph_edges against the
# store for each of those kinds graph_edges answers (shaped, from_ask, decided_by, followed_by,
# ask_parent's owned_by and ask_anchor; no arm of it answers a reading's or an ask's part_of),
# each a `census pair <kind>: … (graph_edges)` line; it prints the store-only lines of copied_from
# and moved_to and requires every far end to resolve; it requires the census's count of
# unconverted sections, and of decisions documents, to read zero; and it reports the two halves of
# every projection, the base release's (project SEED) and this head's (project NODES), each above
# zero.
#
# It reads each rule of R9 on the section that carries it, and fails where the capture holds no
# section with an `Unsure:` paragraph, none under `## Replaced` or none with a standing Keep, each of
# which the fixture seeds; a reading's part_of is read from the store against the ask the capture
# names. It reads the approved specs still approved at their newest version, whose only change over
# the restore's version is the decision blocks, the readings inside answered asks and the shaped
# pointers; it pairs each captured section with the ask node the capture names for it or with the
# decision node that keeps its heading block id, and prints missing, extra and unpaired with every
# reference, and the issues holding a section no rule dropped and no spec holds; and it sets the
# old `decisions` member beside `entries` for each decision and finds the same decisions, stale,
# changed_version and changed_at in both. It reads each of R9's four further kinds on its section:
# the ask block a section held, which no row names, moved into the spec and the rest a decision;
# and the sections citing two answered asks, an issue-level answered ask, and one answered ask with
# a body no reading holds, each a decision that keeps its heading id, takes its links as shaped and
# no from_ask, cites its asks and waits for a person's review with uncertainty high; and the
# decisions.md retired.
#
# Last it stops this head, boots the base alone and has it write a decisions.md on issue C, boots
# this head without the hold setting and the base beside it, both stamped, has the base answer an
# ask this head created, and reads the answer, the selection, the answerer and the time on the ask
# node, each under that ask.answered event; then it stops both, boots this head alone without the
# hold setting, and reads that the step at boot leaves no decisions document within a bound, and
# that /healthz then answers decisions_step_owed zero.
#
# Each passing check prints `PASS: <section> <what>`; the first failing one prints
# `FAIL: <section> <what>: <observed>` on stderr and the run exits 1; a missing input exits 2. It
# prints no SKIP line. Every check is `cmd || fail …`, never part of an `&&` list.
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DATABASE_URL=${LEGION_TEST_PG_DSN%/*}/infra_1150_decision_nodes?sslmode=disable \
         BASELINE_SHA=<the commit of the release this branch lands after> \
         [BASELINE_BINARY=$TMPDIR/base-dispatch] [HEAD_BINARY=$TMPDIR/bin/envoy-dispatch] \
         bash e2e/acceptance/decision-nodes.sh
       (run from packages/dispatch; DATABASE_URL names a database on this machine whose name
       begins infra_1150_, which this run drops and creates again. BASELINE_SHA is required and
       has no default; BASELINE_BINARY must be the server built from it and stamped with it)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
# shellcheck source-path=SCRIPTDIR source=lib-baseline.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib-baseline.sh"
# shellcheck source-path=SCRIPTDIR source=seed-decisions.sh
. "$(dirname "${BASH_SOURCE[0]}")/seed-decisions.sh"

require_env DATABASE_URL TMPDIR BASELINE_SHA
require_commands cmp curl date diff git jq psql sed sort tr
# shellcheck disable=SC2031 # the run's own DATABASE_URL: only a server's subshell sets another
database_url="$DATABASE_URL"
if [[ -z "${BASELINE_BINARY:-}" ]]; then
  # shellcheck disable=SC2031 # this shell's own TMPDIR: only a server's subshell sets another
  BASELINE_BINARY="$TMPDIR/base-dispatch"
fi
# shellcheck disable=SC2031 # this shell's own TMPDIR
head_binary="${HEAD_BINARY:-$TMPDIR/bin/envoy-dispatch}"
[[ -f "$head_binary" && -x "$head_binary" ]] || {
  printf 'HEAD_BINARY %s is not an executable file: build it as the Verification preamble does\n' "$head_binary" >&2
  exit 2
}
readonly head_binary
# The commit BASELINE_SHA names, in full: the one the base server booted below must be stamped with.
base_sha="$(git -C "$(dirname -- "${BASH_SOURCE[0]}")" rev-parse --verify --quiet "${BASELINE_SHA}^{commit}")" || {
  printf 'BASELINE_SHA %s names no commit of this checkout\n' "$BASELINE_SHA" >&2
  exit 2
}
readonly base_sha
readonly project=SEED nodes_project=NODES
# The kinds of this child the census holds to their projections.
readonly -a kinds=(shaped from_ask decided_by followed_by reading_parent ask_parent ask_anchor ask_answer)
# Those of them graph_edges answers, which the census also holds graph_edges to, each as the kind
# <kind>_edge: every one but reading_parent, whose part_of no arm answers, and ask_answer, which is
# values; ask_parent's is its owned_by, since no arm answers an ask's part_of either.
readonly -a edge_kinds=(shaped from_ask decided_by followed_by ask_parent ask_anchor)
# How long the step at boot gets to leave no decisions document.
readonly step_bound_seconds=180

work_dir
head_pid=""
beside_pid=""
cleanup() {
  local code=$?
  baseline_stop_pid "$head_pid"
  baseline_stop_pid "$beside_pid"
  baseline_stop
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
mkdir "$work/capture" "$work/home"

# ---------------------------------------------------------------------------------------------
# The helpers.
# ---------------------------------------------------------------------------------------------
# send AS METHOD PATH [JSON]: METHOD PATH to $server as AS, `agent` (the bearer, in $work/auth) or
# `person` (the dev sign-in's cookie in $work/jar and the server's origin). The answer's body goes
# to $work/body and its status to $status.
send() {
  local -a auth data=()
  case "$1" in
    agent) auth=(-H "@$work/auth") ;;
    person) auth=(-b "$work/jar" -H "Origin: $server") ;;
  esac
  [[ $# -lt 4 ]] || data=(-H 'Content-Type: application/json' --data-binary "$4")
  status="$(curl -sS -X "$2" -o "$work/body" -w '%{http_code}' "${auth[@]}" "${data[@]}" \
    "$server$3" 2>"$work/curl")" || status="curl failed: $(<"$work/curl")"
}

# field SECTION WHAT JQ: one value of the last answer's body, which must be there, in $value.
field() {
  value="$(jq -er "$3" "$work/body" 2>/dev/null)" || fail "$1" "$2" "no $3 in: $(body)"
}

# sign_in SECTION: signs the person in at $server, the cookie in $work/jar.
sign_in() {
  status="$(curl -sS -o "$work/body" -w '%{http_code}' -c "$work/jar" \
    "$server/auth/_dev/signin?login=$seed_person" 2>"$work/curl")" || status="curl failed: $(<"$work/curl")"
  [[ "$status" =~ ^[23][0-9][0-9]$ ]] || fail "$1" "sign $seed_person in at $server" "HTTP $status: $(body)"
  grep -q $'\t' "$work/jar" || fail "$1" "sign $seed_person in at $server" "the dev sign-in set no cookie"
}

# commit_of SECTION: the commit $server registers, from its /healthz, in $value; it must be one.
commit_of() {
  send agent GET /healthz
  want_status 200 "$1" "read /healthz of $server"
  field "$1" "the commit $server is stamped with" '.commit'
}

# dispatch_command NAME ARGS…: this head's `envoy-dispatch ARGS` over the database, in an
# environment holding only PATH, a home and a TMPDIR of this run's and DATABASE_URL, its output in
# $work/NAME.out and its exit in $command_code.
dispatch_command() {
  local name="$1"
  shift
  command_code=0
  # shellcheck disable=SC2031 # this shell's own PATH: only a server's subshell sets another
  env -i PATH="$PATH" HOME="$work/home" TMPDIR="$work" DATABASE_URL="$database_url" \
    "$head_binary" "$@" >"$work/$name.out" 2>&1 || command_code=$?
}

# census SECTION NAME: `envoy-dispatch census --store` into $work/NAME.out, which exits 1 where a
# kind of another child holds no row; this check reads its lines, not that verdict.
census() {
  dispatch_command "$2" census --store
  [[ "$command_code" -le 1 ]] || fail "$1" "run the store census" "exited $command_code: $(<"$work/$2.out")"
}

# kind_line NAME KIND: the census line of KIND in $work/NAME.out, in $line, its fields in $fields.
kind_line() {
  line="$(grep -E "^census: store kind=$2 (tuples|nodes)=" "$work/$1.out" || true)"
  [[ -n "$line" ]] || fail census "the census prints a line for $2" "no such line in $1"
  declare -gA fields=()
  local pair
  # shellcheck disable=SC2086 # the line's fields, split on the spaces between them
  for pair in ${line#census: store }; do
    [[ "$pair" != *=* ]] || fields["${pair%%=*}"]="${pair#*=}"
  done
}

# wait_settled SECTION: waits, within two minutes, until no document owes a settlement or its block
# index, so a read that follows reads every write before it.
wait_settled() {
  local waited=0
  while true; do
    query "$1" "read what the documents owe" \
      "select (select count(*) from doc_settlements_pending) + (select count(*) from document_block_versions where version is null)"
    [[ "$answer" != 0 ]] || return 0
    [[ "$waited" -lt 1200 ]] || fail "$1" "every document settles" "$answer settlements and block indexes still owed after 120 s"
    sleep 0.1
    waited=$((waited + 1))
  done
}

# wait_ask SECTION ISSUE BLOCK: waits for the ask of block BLOCK of ISSUE's documents, its id in
# $value.
wait_ask() {
  local waited=0
  while true; do
    send agent GET "/api/v1/issues/$2/asks?state=all"
    want_status 200 "$1" "read the asks of $2"
    value="$(jq -r --arg b "$3" '[.[] | select(.block_id == $b)] | if length == 1 then .[0].id else "" end' "$work/body")"
    [[ -z "$value" ]] || return 0
    [[ "$waited" -lt 600 ]] || fail "$1" "the ask of block $3 opens" "no single ask of $2 names block $3 after 60 s"
    sleep 0.1
    waited=$((waited + 1))
  done
}

# rows SQL: SQL's rows, `|` between columns, into $work/rows.
rows() {
  env -u PGHOSTADDR psql "$database_url" -XAtq -F '|' -c "$1" >"$work/rows" 2>"$work/psql" ||
    fail sql "read $1" "psql: $(<"$work/psql")"
}

# current_of KIND FROM_KIND FROM_ID TUPLE: the far ends of the current TUPLE tuples out of the node
# KIND/ID, `to_kind:to_id` one a line, sorted, in $work/rows.
current_of() {
  rows "select c.to_kind || ':' || c.to_id from (
          select distinct on (t.to_kind, t.to_id) t.to_kind, t.to_id, t.asserted from graph_tuples t
          where t.from_kind = '$1' and t.from_id = '$2' and t.kind = '$3' and t.to_kind <> 'value'
          order by t.to_kind, t.to_id, t.seq desc) c where c.asserted order by 1"
}

# value_of FROM_KIND FROM_ID ATTRIBUTE: the newest value of ATTRIBUTE on the node, in $answer.
value_of() {
  query value "read $3 of $1 $2" \
    "select coalesce((select t.to_id from graph_tuples t where t.from_kind = '$1' and t.from_id = '$2' and t.kind = '$3' and t.to_kind = 'value' and t.asserted order by t.seq desc limit 1), '')"
}

# ---------------------------------------------------------------------------------------------
# The base release writes its half.
# ---------------------------------------------------------------------------------------------
seed_reset_database "$database_url"
DISPATCH_TOKEN=e2e-token
BASELINE_EXTRA_ENV=$'DISPATCH_TEST_HOOKS=1\nDISPATCH_TEST_HOLD_DECISIONS_STEP=1'
baseline_start
unset DISPATCH_TOKEN DATABASE_URL BASELINE_EXTRA_ENV
printf 'Authorization: Bearer %s\n' "$baseline_token" >"$work/auth"
server="$baseline_url"
# The base server is the release BASELINE_SHA names, as the commit it is stamped with says, before
# it writes a row.
commit_of capture
readonly base_commit="$value"
[[ "$base_commit" == "$base_sha" ]] ||
  fail capture "the base release is stamped with the commit BASELINE_SHA names" \
    "it serves '$base_commit', and BASELINE_SHA $BASELINE_SHA is $base_sha"
pass capture "the base release serves $base_commit, the commit BASELINE_SHA names"
# database_url stays writable: lib-baseline.sh's baseline_boot, which boots this head, holds a
# local of that name.
seed_decisions
seed_decisions_further_kinds

readonly issue_a="${seed_ids[issue_a]}" issue_b="${seed_ids[issue_b]}" issue_c="${seed_ids[issue_c]}"
readonly spec_a="${seed_ids[spec_a]}" spec_b="${seed_ids[spec_b]}"
readonly decisions_a="${seed_ids[decisions_a]}" decisions_b="${seed_ids[decisions_b]}"

# An ask the base writes on a project document, so an asks row it wrote names an owner.
send agent POST "/api/v1/projects/$project/artifacts" \
  "$(jq -cn --argjson a "$(seed_actor)" '{name: "project-notes.md", content: "# Project notes\n\nThe corpus has one owner.\n", actor: $a}')"
want_status 201 capture "upload a project document through the base release"
field capture "the project document's id" '.artifact.id // .id'
readonly project_document="$value"
send agent POST "/api/v1/artifacts/$project_document/asks" \
  "$(jq -cn --argjson a "$(seed_actor)" '{question: "Who owns the corpus?", actor: $a}')"
want_status 201 capture "open an ask on the project document through the base release"
field capture "the project document's ask" '.id // .ask.id'
readonly owned_ask="$value"
readonly kept_ask="${seed_ids[ask_kept]}"
seed_wait_settled "the project document's ask settles"

# The document answer of each issue that holds a decisions.md, and what the capture records of each
# section: the ask node it names or holds, and the author of the version it last changed in. A
# section of R9's four further kinds is restated by no ask: it pairs with the decision that keeps
# its heading block id, and the checks of `kinds` below read each.
declare -A decisions_of=(["$issue_a"]="$decisions_a" ["$issue_b"]="$decisions_b")
declare -A further_kinds=(
  ["${seed_ids[section_held]}"]=held
  ["${seed_ids[section_two_asks]}"]=two_asks
  ["${seed_ids[section_issue_level]}"]=issue_level
  ["${seed_ids[section_body]}"]=body
)
: >"$work/capture/sections"
for issue in "$issue_a" "$issue_b"; do
  send agent GET "/api/v1/issues/$issue/decisions"
  want_status 200 capture "read the Decisions of $issue through the base release"
  cp "$work/body" "$work/capture/decisions.$issue.json"
  send agent GET "/api/v1/issues/$issue/asks?state=all"
  want_status 200 capture "read the asks of $issue through the base release"
  cp "$work/body" "$work/capture/asks.$issue.json"
  while IFS='|' read -r heading_id changed named held; do
    ask="$named"
    if [[ -z "$ask" && -n "$held" ]]; then
      ask="$(jq -r --arg b "$held" --arg d "${decisions_of[$issue]}" \
        '[.[] | select(.block_id == $b and .block_artifact.id == $d) | .id] | if length == 1 then .[0] else "" end' \
        "$work/capture/asks.$issue.json")"
    fi
    [[ -z "${further_kinds[$heading_id]:-}" ]] || ask=""
    send agent GET "/api/v1/artifacts/${decisions_of[$issue]}/versions/$changed"
    want_status 200 capture "read version $changed of the decisions document of $issue"
    # A version names a person as a user; the node the store states for them is a person's.
    author="$(jq -r '.authors[0] | "\(if .kind == "user" then "person" else .kind end):\(.id)"' "$work/body")"
    printf '%s\t%s\t%s\t%s\n' "$issue" "$heading_id" "${ask:--}" "$author" >>"$work/capture/sections"
  done < <(jq -r '.decisions[] | [.block_id, (.changed_version | tostring),
      (if (.asks | length) == 1 then .asks[0] else "" end),
      ((.text | capture(":::ask\\{#(?<id>[^ }]+)") | .id) // "")] | join("|")' "$work/capture/decisions.$issue.json")
done
printf 'captured sections (issue, heading block, ask, author of its newest version):\n%s\n' "$(<"$work/capture/sections")"
baseline_stop_server

# ---------------------------------------------------------------------------------------------
# This head, held: graph_edges over the rows the base wrote, before and after the first phase.
# ---------------------------------------------------------------------------------------------
baseline_boot head "$head_binary" "$baseline_database_url"
head_pid="$baseline_boot_pid"
server="$baseline_boot_url"
commit_of head
readonly head_commit="$value"
[[ "$head_commit" != "$base_commit" ]] || fail head "this head and the base are stamped apart" "both serve $head_commit"
pass head "this head serves $head_commit over the database the base release ($base_commit) wrote, the step held"
sign_in head

# edges_of ASK: the graph_edges rows out of ask ASK of the three kinds the old arms answer, one a
# line, sorted, in $work/rows.
edges_of() {
  rows "select kind || ' ' || to_kind || ':' || to_id from graph_edges
        where from_kind = 'ask' and from_id = '$1' and kind in ('anchored_to', 'owned_by', 'followed_by') order by 1"
}
# arm_state ASK: whether the asks row of ASK and its ask_followers rows are marked, and how many
# anchored_to, owned_by and followed_by tuples its node states, in $answer.
arm_state() {
  query edges "read the marks and the tuples of ask $1" "
    select (select coalesce(node_kind, 'none') || ':' || coalesce(node_address, 'none') from asks where id = '$1')
      || ' followers_unmarked=' || (select count(*) from ask_followers where ask_id = '$1' and node_kind is null)
      || ' tuples=' || (select count(*) from graph_tuples where from_kind = 'ask' and from_id = '$1'
                        and kind in ('anchored_to', 'owned_by', 'followed_by'))"
}

declare -A edges_before=()
for ask in "$kept_ask" "$owned_ask"; do
  edges_of "$ask"
  edges_before["$ask"]="$(<"$work/rows")"
  printf 'graph_edges over the asks row %s before the backfill:\n%s\n' "$ask" "$(<"$work/rows")"
  [[ -n "$(<"$work/rows")" ]] || fail edges "graph_edges answers the asks row $ask the base wrote" "no edge"
  arm_state "$ask"
  [[ "$answer" =~ ^none:none\ followers_unmarked=[1-9][0-9]*\ tuples=0$ ]] ||
    fail edges "before the backfill the asks row $ask is unmarked and its node states no such tuple" "$answer"
  printf 'asks row %s before the backfill: %s\n' "$ask" "$answer"
done
grep -q '^anchored_to ' <<<"${edges_before[$kept_ask]}" || fail edges "an old arm answers anchored_to" "${edges_before[$kept_ask]}"
grep -q '^owned_by ' <<<"${edges_before[$owned_ask]}" || fail edges "an old arm answers owned_by" "${edges_before[$owned_ask]}"
grep -q '^followed_by ' <<<"${edges_before[$kept_ask]}" || fail edges "an old arm answers followed_by" "${edges_before[$kept_ask]}"
pass edges "before the backfill the old anchored_to, owned_by and followed_by arms answer the rows the base wrote, and the tuple arm, whose rows are unmarked and whose tuples are none, does not"

census edges census-unmarked
for kind in ask_parent ask_anchor ask_answer followed_by; do
  unmarked="$(grep -E "^census: store kind=$kind unmarked_rows=" "$work/census-unmarked.out" || true)"
  printf '  %s\n' "${unmarked:-no unmarked_rows line for $kind}"
  [[ "$unmarked" =~ unmarked_rows=[1-9] ]] || fail edges "the census prints the unmarked rows of $kind as a class of its own" "${unmarked:-no line}"
done
pass edges "the census prints the unmarked rows the release before wrote as a class of their own"

dispatch_command backfill backfill-graph-store
[[ "$command_code" == 0 ]] || fail edges "envoy-dispatch backfill-graph-store" "exited $command_code: $(<"$work/backfill.out")"
grep -E '^backfill-graph-store: kind=(asks|ask_followers) ' "$work/backfill.out" | sed 's/^/  /' || true
pass edges "envoy-dispatch backfill-graph-store ran the step's first phase"
for ask in "$kept_ask" "$owned_ask"; do
  edges_of "$ask"
  [[ "$(<"$work/rows")" == "${edges_before[$ask]}" ]] ||
    fail edges "after the backfill graph_edges answers each edge of $ask once, as before" "before: ${edges_before[$ask]}; after: $(<"$work/rows")"
  arm_state "$ask"
  [[ "$answer" =~ ^ask:$ask\ followers_unmarked=0\ tuples=[1-9][0-9]*$ ]] ||
    fail edges "after the backfill the asks row $ask is marked and its node states the tuples" "$answer"
  printf 'asks row %s after the backfill: %s\n' "$ask" "$answer"
done
pass edges "after the backfill the tuple arm answers the marked rows and the old arms, which answer only unmarked rows, do not"
query edges "read the view graph_edges" "select pg_get_viewdef('graph_edges'::regclass)"
viewdef="${answer//$'\n'/ }"
for arm in "anchored_to asks" "owned_by asks" "followed_by ask_followers"; do
  kind="${arm% *}"
  table="${arm#* }"
  found=""
  while IFS= read -r branch; do
    if [[ "$branch" == *"'$kind'::text"* && "$branch" =~ FROM\ \(?$table\ [a-z]* && "$branch" == *"node_kind IS NULL"* ]]; then
      found="$branch"
    fi
  done <<<"${viewdef//UNION ALL/$'\n'}"
  [[ -n "$found" ]] || fail edges "the old $kind arm over $table still stands in graph_edges" "no such arm in the view"
  printf 'old arm still in graph_edges: %s\n' "$(tr -s ' ' <<<"$found")"
done
pass edges "the three old arms still stand in the view"
census edges census-marked
for kind in ask_parent ask_anchor ask_answer followed_by; do
  grep -q "^census: store kind=$kind unmarked_rows=0 " "$work/census-marked.out" ||
    fail edges "the census's class of unmarked $kind rows reads zero after the backfill" "$(grep "kind=$kind unmarked" "$work/census-marked.out" || echo 'no line')"
done
pass edges "the census's class of unmarked rows reads zero after the backfill"

# ---------------------------------------------------------------------------------------------
# This head writes its half.
# ---------------------------------------------------------------------------------------------
send agent POST /api/v1/projects "$(jq -cn --arg k "$nodes_project" --arg p "$seed_person" --argjson a "$(seed_actor)" \
  '{key: $k, name: "This head'"'"'s half", default_assignee: $p, actor: $a}')"
want_status 201 half "create the project $nodes_project at this head"
send agent POST /api/v1/issues "$(jq -cn --arg p "$nodes_project" --argjson a "$(seed_actor)" '{project: $p,
  title: "The decision nodes this head writes",
  spec: ":::requirement{#r1}\nOne.\n:::\n\n:::requirement{#r2}\nTwo.\n:::\n\n:::requirement{#r3}\nThree.\n:::\n\n:::requirement{#r4}\nFour.\n:::\n",
  actor: $a}')"
want_status 201 half "create the issue of $nodes_project"
field half "the issue's key" '.key'
readonly issue_n="$value"
field half "the issue's spec" '.primary_artifact_id'
readonly spec_n="$value"
pointer() { printf 'dispatch://%s/spec#b-%s' "$issue_n" "$1"; }
send agent POST "/api/v1/artifacts/$spec_n/edits" "$(jq -cn --arg s "$(pointer r1) $(pointer r4)" --argjson a "$(seed_actor)" '{ops: [{op: "insert", after: "end",
  markdown: (":::ask{#a-answered urgency=\"med\" shaped=\"" + $s + "\"}\nWhich transport?\n\n- REST: Plain HTTP\n- gRPC: Streaming\n:::\n\n:::ask{#a-open urgency=\"low\"}\nWhich region?\n:::\n\n:::ask{#a-declined urgency=\"low\"}\nWhich colour?\n:::")}], actor: $a}')"
want_status 200 half "write the three asks into the spec of $issue_n"
wait_ask half "$issue_n" a-answered
readonly answered_n="$value"
wait_ask half "$issue_n" a-declined
readonly declined_n="$value"
send person POST "/api/v1/asks/$answered_n/answer" \
  '{"text":"Plain HTTP for now, as every service already speaks it.","selected":["REST"],"expected_edited_at":null}'
want_status 200 half "the person answers a-answered in their own words"
send person POST "/api/v1/asks/$declined_n/decline" '{"note":"Not mine to choose."}'
want_status 200 half "the person declines a-declined with a note"
send agent PUT "/api/v1/asks/$answered_n/followers/ses_decision_nodes_follower" \
  "$(jq -cn '{actor: {kind: "session", id: "ses_decision_nodes_follower"}}')"
[[ "$status" =~ ^20[04]$ ]] || fail half "a session follows a-answered" "HTTP $status: $(body)"
wait_settled half
send agent POST "/api/v1/artifacts/$spec_n/edits" "$(jq -cn --arg s "$(pointer r1) $(pointer r2) $(pointer r3)" \
  --arg f "dispatch://$issue_n/ask/$answered_n" --argjson a "$(seed_actor)" '{ops: [{op: "insert", after: "end",
  markdown: ("::::decision{#d1 uncertainty=\"medium\" uncertainty_reason=\"a second region would change it\" shaped=\"" + $s + "\" from_ask=\"" + $f + "\"}\nREST for every service this quarter.\n\n:::reading{#g1}\nThe agent reads it so.\n:::\n::::")}], actor: $a}')"
want_status 200 half "an agent writes the decision d1, which shapes three requirements, into the spec"
send agent POST "/api/v1/issues/$issue_n/artifacts" "$(jq -cn --argjson a "$(seed_actor)" '{name: "choices.md",
  content: ":::ask{#a-second urgency=\"low\"}\nWhich queue?\n:::\n\n:::ask{#a-answered urgency=\"med\" multiple=\"false\"}\nWhich transport?\n\n- REST: Plain HTTP\n- gRPC: Streaming\n:::\n\n:::decision{#d1}\nA second decision under the spec decision'"'"'s block id.\n:::\n",
  actor: $a}')"
want_status 201 half "an agent writes a second document holding an ask, a copy of a-answered and a decision d1"
wait_settled half
pass half "this head wrote its half of the corpus in $issue_n"

# ---------------------------------------------------------------------------------------------
# The restore and the conversion, through their entry points.
# ---------------------------------------------------------------------------------------------
dispatch_command restore restore-answered-asks --project "$project"
grep '^restore-answered-asks: ' "$work/restore.out" | sed 's/^/  /' || true
[[ "$command_code" == 0 ]] || fail convert "envoy-dispatch restore-answered-asks --project $project" "exited $command_code: $(<"$work/restore.out")"
wait_settled convert
declare -A restored_version=()
for spec in "$spec_a" "$spec_b"; do
  send agent GET "/api/v1/artifacts/$spec"
  want_status 200 convert "read the spec $spec"
  field convert "the newest version of $spec" '[.versions[].number] | max'
  restored_version["$spec"]="$value"
  send agent GET "/api/v1/artifacts/$spec/versions/$value"
  want_status 200 convert "read version $value of $spec"
  jq -j '.markdown' "$work/body" >"$work/restored.$spec.md"
done
dispatch_command convert convert-decisions --project "$project"
grep '^convert-decisions: ' "$work/convert.out" | sed 's/^/  /' || true
[[ "$command_code" == 0 ]] || fail convert "envoy-dispatch convert-decisions --project $project" "exited $command_code: $(<"$work/convert.out")"
wait_settled convert
pass convert "envoy-dispatch restore-answered-asks and convert-decisions ran over $project"

# ---------------------------------------------------------------------------------------------
# The census.
# ---------------------------------------------------------------------------------------------
census census census
# The section of issue E, which cites the answered ask its person removed from the spec through the
# base release: R9 deletes it, and its heading takes one moved_to to the ask's node. The census's
# moved_to store-only line counts every moved_to the store states now - the newest tuple of each
# near and far end out of a block or an ask's node, while it asserts, as the census reads them -
# that one among them, and reads it resolved only while the ask's node is live.
readonly issue_e="${seed_ids[issue_e]}" removed_ask="${seed_ids[ask_e_removed]}"
readonly removed_heading="$issue_e/decisions-md#${seed_ids[section_removed]}"
rows "select to_kind || ':' || to_id from graph_tuples
      where kind = 'moved_to' and from_kind = 'block' and from_id = '$removed_heading' and asserted"
[[ "$(<"$work/rows")" == "ask:$removed_ask" ]] ||
  fail removed "the heading $removed_heading of the section citing ask $removed_ask, which its person removed, takes one moved_to to that ask's node" \
    "it takes $(tr '\n' ' ' <"$work/rows")"
query removed "count the moved_to tuples the store states now" "
  select count(*) from (
    select distinct on (t.from_kind, t.from_id, t.to_kind, t.to_id) t.asserted from graph_tuples t
    where t.kind = 'moved_to' and t.from_kind in ('block', 'ask') and t.to_kind <> 'value'
    order by t.from_kind, t.from_id, t.to_kind, t.to_id, t.seq desc) current
  where current.asserted"
readonly stated_moves="$answer"
line="$(grep -E '^census: store-only kind=moved_to ' "$work/census.out" || true)"
printf '%s\n' "${line:-no store-only line for moved_to}"
[[ "$line" =~ tuples=([0-9,]+)\ unresolved=0\ left_out=0$ && "${BASH_REMATCH[1]//,/}" == "$stated_moves" ]] ||
  fail removed "the census's moved_to line counts the $stated_moves moved_to tuples the store states, the one to ask $removed_ask among them, each resolving" \
    "${line:-no line}"
pass removed "the section citing ask $removed_ask, which its person removed, is deleted: its heading moves to the ask's node, and the census reads that moved_to among the $stated_moves it counts, unresolved=0"
for kind in "${kinds[@]}"; do
  kind_line census "$kind"
  printf '%s\n' "$line"
  if [[ -n "${fields[tuples]:-}" ]]; then
    store="${fields[tuples]}" missing="${fields[rows_without_a_tuple]}" extra="${fields[tuples_without_a_row]}"
  else
    store="${fields[nodes]}" missing="${fields[rows_without_a_node]}" extra="${fields[nodes_without_a_row]}"
  fi
  printf 'census pair %s: store=%s projection=%s (%s) differences=%s\n' "$kind" "$store" "${fields[rows]}" \
    "${fields[table]}" "$((${missing//,/} + ${extra//,/}))"
  grep -E "^census: store kind=$kind difference at " "$work/census.out" || true
  [[ "${store//,/}" -gt 0 && "${fields[rows]//,/}" -gt 0 ]] ||
    fail census "the census compares rows of $kind on both sides" "$line"
  [[ "${missing//,/}" == 0 && "${extra//,/}" == 0 ]] ||
    fail census "the store and the projection of $kind agree triple by triple" "$line"
done
# graph_edges against the store, for each kind it answers.
for kind in "${edge_kinds[@]}"; do
  kind_line census "${kind}_edge"
  printf '%s\n' "$line"
  store="${fields[tuples]}" missing="${fields[rows_without_a_tuple]}" extra="${fields[tuples_without_a_row]}"
  printf 'census pair %s: store=%s projection=%s (%s) differences=%s\n' "$kind" "$store" "${fields[rows]}" \
    "${fields[table]}" "$((${missing//,/} + ${extra//,/}))"
  grep -E "^census: store kind=${kind}_edge difference at " "$work/census.out" || true
  [[ "${store//,/}" -gt 0 && "${fields[rows]//,/}" -gt 0 ]] ||
    fail census "the census compares graph_edges' edges of $kind with the store on both sides" "$line"
  [[ "${missing//,/}" == 0 && "${extra//,/}" == 0 ]] ||
    fail census "the store and graph_edges agree on $kind triple by triple" "$line"
done
grep -E '^census: store rows_checked=' "$work/census.out" || true
grep -qE '^census: store rows_checked=[0-9,]+ differences=0$' "$work/census.out" ||
  fail census "the census reads zero differences" "$(grep -E '^census: store (rows_checked|kind=.* difference)' "$work/census.out" || echo 'no line')"
pass census "every kind of this child agrees with every projection it feeds, with zero differences"
for kind in copied_from moved_to; do
  line="$(grep -E "^census: store-only kind=$kind " "$work/census.out" || true)"
  printf '%s\n' "${line:-no store-only line for $kind}"
  [[ "$line" =~ tuples=([0-9,]+)\ unresolved=0\ left_out=0$ && "${BASH_REMATCH[1]//,/}" -gt 0 ]] ||
    fail census "the store-only kind $kind holds tuples whose every far end resolves" "${line:-no line}"
done
pass census "copied_from and moved_to print their store counts, and every far end resolves"
line="$(grep '^census: decisions ' "$work/census.out" || true)"
printf '%s\n' "${line:-no decisions line}"
[[ "$line" == "census: decisions documents=0 unconverted_sections=0 specs_with_moved_answers=0" ]] ||
  fail census "no issue of the fixture holds an unconverted section after the conversion" "${line:-no line}"
pass census "the census counts zero unconverted sections and zero decisions documents after the conversion"

# The two halves of every projection: the base release's (project SEED) and this head's (NODES).
rows "select p.key,
        (select count(*) from asks k join issues i on i.key = k.issue_key where i.project_key = p.key),
        (select count(*) from ask_followers f join asks k on k.id = f.ask_id join issues i on i.key = k.issue_key where i.project_key = p.key),
        (select count(*) from block_refs r join artifacts a on a.id = r.artifact_id where a.project_key = p.key and r.kind in ('shaped', 'from_ask', 'decided_by')),
        (select count(*) from document_blocks d join artifacts a on a.id = d.artifact_id where a.project_key = p.key and d.type = 'reading' and d.parent_block_id is not null)
      from projects p where p.key in ('$project', '$nodes_project') order by p.key"
while IFS='|' read -r key asks followers typed readings; do
  printf 'half %s: asks=%s ask_followers=%s block_refs(shaped, from_ask, decided_by)=%s readings_with_a_parent=%s\n' \
    "$key" "$asks" "$followers" "$typed" "$readings"
  [[ "$asks" -gt 0 && "$followers" -gt 0 && "$typed" -gt 0 ]] ||
    fail halves "the half of $key holds rows of every projection" "asks=$asks ask_followers=$followers block_refs=$typed"
done <"$work/rows"
[[ "$(wc -l <"$work/rows")" == 2 ]] || fail halves "both halves are read" "$(<"$work/rows")"
pass halves "both halves, the base release's and this head's, hold rows the census compared"

# ---------------------------------------------------------------------------------------------
# Each rule of R9, on the section that carries it.
# ---------------------------------------------------------------------------------------------
declare -A entries_file=()
for issue in "$issue_a" "$issue_b"; do
  send agent GET "/api/v1/issues/$issue/decisions"
  want_status 200 rules "read the Decisions of $issue at this head"
  cp "$work/body" "$work/entries.$issue.json"
  entries_file["$issue"]="$work/entries.$issue.json"
done
# entry ISSUE KIND ID: the entry of node KIND/ID in ISSUE's node read, into $work/entry and, for
# lib.sh's expect, $work/body; empty when there is none.
entry() {
  jq --arg k "$2" --arg i "$3" '[.entries[] | select(.node.kind == $k and .node.id == $i)] | .[0] // empty' \
    "${entries_file[$1]}" >"$work/entry"
  cp "$work/entry" "$work/body"
}
# section_text ISSUE HEADING_ID: the captured text of the section.
section_text() {
  jq -r --arg h "$2" '.decisions[] | select(.block_id == $h) | .text' "$work/capture/decisions.$1.json"
}
# normal TEXT: TEXT with its whitespace collapsed and its case folded.
normal() { tr -s '[:space:]' ' ' <<<"${1,,}" | sed -E 's/^ //; s/ $//'; }

# How many sections each conditional rule below read. The fixture seeds a section of each, so a
# rule that read none fails after the loop rather than passing over nothing.
unsure_read=0
replaced_read=0
kept_read=0
while IFS=$'\t' read -r issue heading ask author; do
  # A section of R9's four further kinds is read by the checks of `kinds` below.
  [[ -z "${further_kinds[$heading]:-}" ]] || continue
  heading_node="$issue/decisions-md#$heading"
  rows "select to_id from graph_tuples where kind = 'moved_to' and from_kind = 'block' and from_id = '$heading_node' and asserted"
  moved="$(<"$work/rows")"
  [[ "$(wc -l <<<"$moved")" == 1 && -n "$moved" ]] || fail rules "one moved_to tuple from the heading of section $heading" "${moved:-none}"
  text="$(section_text "$issue" "$heading")"
  if [[ "$ask" != - ]]; then
    query rules "read the block of ask $ask" "select block_id from asks where id = '$ask'"
    ask_block="$issue/spec#$answer"
    [[ "$moved" == "$ask_block" ]] ||
      fail rules "the section $heading, which cites or holds ask $ask, moves to that ask's block" "it moves to $moved, the block is $ask_block"
    current_of ask "$ask" shaped
    sed 's/^block://' "$work/rows" >"$work/shaped"
    jq -r --arg h "$heading" '.decisions[] | select(.block_id == $h) | .links[].to.id' "$work/capture/decisions.$issue.json" |
      LC_ALL=C sort -u >"$work/links"
    LC_ALL=C comm -13 "$work/shaped" "$work/links" >"$work/unshaped"
    [[ ! -s "$work/unshaped" ]] || fail rules "the section $heading gives ask $ask its links as shaped" "not shaped: $(tr '\n' ' ' <"$work/unshaped")"
    entry "$issue" ask "$ask"
    [[ -s "$work/entry" ]] || fail rules "the node read holds ask $ask" "no entry"
    answer_text="$(jq -r '.answer // ""' "$work/entry")"
    body="$(sed '1d' <<<"$text" | grep -v '^:::' | grep -v '^- ' || true)"
    body_words="$(normal "$(sed -E 's#dispatch://[^ ]*##g' <<<"$body")")"
    held_block="$(grep -c '^:::ask{' <<<"$text" || true)"
    if [[ "$held_block" != 0 ]]; then
      pass rules "the section $heading held the block of ask $ask, which the restore moved to $ask_block, and its heading moves there"
    elif [[ "$(normal "$body")" == "$(normal "$answer_text")" ]]; then
      [[ "$(jq '.readings | length' "$work/entry")" == 0 ]] ||
        fail rules "the section $heading, which only restates the answer, is dropped and writes no reading" "$(jq -c '.readings' "$work/entry")"
      pass rules "the section $heading, whose body is the answer and nothing more, is dropped: no reading, one moved_to to $ask_block"
    else
      [[ "$(jq '.readings | length' "$work/entry")" == 1 ]] ||
        fail rules "the body of section $heading becomes a reading under ask $ask" "the ask holds $(jq -c '.readings' "$work/entry")"
      reading_text="$(normal "$(jq -r '.readings[0].text // ""' "$work/entry" | sed -E 's#dispatch://[^ ]*##g')")"
      [[ -n "$body_words" && "$reading_text" == *"${body_words:0:80}"* ]] ||
        fail rules "the reading under ask $ask holds the section's words" "reading: $reading_text; section: $body_words"
      expect rules "the reading of section $heading is authored by the author of its newest version, $author" \
        "\"$author\"" '.readings[0].authored_by | "\(.kind):\(.id)"'
      # The entry is picked by the ask's node, so its own node says nothing: the store's part_of
      # tuple out of the reading is read against the ask the capture names for this section.
      reading_node="$(jq -r '.readings[0].node | "\(.kind)|\(.id)"' "$work/entry")"
      current_of "${reading_node%%|*}" "${reading_node#*|}" part_of
      [[ "$(<"$work/rows")" == "ask:$ask" ]] ||
        fail rules "the reading of section $heading hangs under ask:$ask, the ask the capture names, by its part_of tuple" \
          "the reading ${reading_node#*|} states the part_of $(tr '\n' ' ' <"$work/rows")"
      pass rules "the section $heading gives ask $ask its links as shaped, and its body becomes a reading under it, by $author"
    fi
    continue
  fi
  # A section that cites no ask: a decision block that keeps its heading block id.
  decision="$issue/spec#$heading"
  [[ "$moved" == "$decision" ]] || fail rules "the section $heading moves to the decision that keeps its id" "it moves to $moved"
  entry "$issue" block "$decision"
  [[ -s "$work/entry" ]] || fail rules "the section $heading becomes a decision node of the spec under its heading id" "no entry for $decision"
  expect rules "the section $heading is an agent's decision" '"agent"' '.kind'
  if grep -q '^Unsure:' <<<"$text"; then
    unsure_read=$((unsure_read + 1))
    reason="$(sed -nE 's/^Unsure: *//p' <<<"$text" | head -n 1)"
    expect rules "the section $heading with an Unsure: paragraph reads uncertainty high" '"high"' '.uncertainty'
    expect rules "the section $heading reads the rest of the paragraph as uncertainty_reason" "$(jq -cn --arg r "$reason" '$r')" '.uncertainty_reason'
  fi
  if [[ "$(jq -r --arg h "$heading" '.decisions[] | select(.block_id == $h) | .replaced' "$work/capture/decisions.$issue.json")" == true ]]; then
    replaced_read=$((replaced_read + 1))
    expect rules "the section $heading under ## Replaced becomes a decision with replaced=\"true\"" true '.replaced'
    value_of block "$decision" replaced
    [[ "$answer" == true ]] || fail rules "the decision of section $heading holds the value replaced true" "it holds '$answer'"
  fi
  if [[ "$(jq -r --arg h "$heading" '.decisions[] | select(.block_id == $h) | .kept != null' "$work/capture/decisions.$issue.json")" == true ]]; then
    kept_read=$((kept_read + 1))
    current_of block "$decision" kept_by
    [[ "$(<"$work/rows")" == "person:$seed_person" ]] ||
      fail rules "the section $heading with a standing Keep reads a kept_by tuple to $seed_person" "$(<"$work/rows")"
    value_of block "$decision" kept_text_sum
    [[ -n "$answer" && "$answer" == "$(jq -r '.text_sha256' "$work/entry")" ]] ||
      fail rules "the decision of section $heading holds kept_text_sum where the sum still matches" "kept_text_sum '$answer', text_sha256 $(jq -r '.text_sha256' "$work/entry")"
  fi
  pass rules "the section $heading, which cites no ask, is the decision $decision, as R9 names"
done <"$work/capture/sections"
printf 'sections read by the conditional rules: Unsure=%s Replaced=%s kept=%s\n' "$unsure_read" "$replaced_read" "$kept_read"
[[ "$unsure_read" -gt 0 ]] ||
  fail rules "the capture holds a section with an Unsure: paragraph, which the fixture seeds" "the uncertainty rule read none"
[[ "$replaced_read" -gt 0 ]] ||
  fail rules "the capture holds a section under ## Replaced, which the fixture seeds" "the replaced rule read none"
[[ "$kept_read" -gt 0 ]] ||
  fail rules "the capture holds a section with a standing Keep, which the fixture seeds" "the Keep rule read none"
pass rules "the conditional rules each read a section: $unsure_read with Unsure:, $replaced_read under ## Replaced, $kept_read kept"

# ---------------------------------------------------------------------------------------------
# R9's four further kinds, each on the section that carries it.
# ---------------------------------------------------------------------------------------------
# moved_from ADDRESS: the far end of the one asserted moved_to tuple out of the block ADDRESS, in
# $moved; there must be one.
moved_from() {
  rows "select to_id from graph_tuples where kind = 'moved_to' and from_kind = 'block' and from_id = '$1' and asserted"
  moved="$(<"$work/rows")"
  [[ "$(wc -l <<<"$moved")" == 1 && -n "$moved" ]] || fail kinds "one moved_to tuple from $1" "${moved:-none}"
}
send agent GET "/api/v1/artifacts/$spec_a/text"
want_status 200 kinds "read the newest version of $spec_a"
jq -j '.markdown' "$work/body" >"$work/converted-a.md"

# The section whose body held an ask block no row names: the block moved into the spec by R6's
# second shape, under its own id, and the rest of the section converted by R9's rules.
held_heading="${seed_ids[section_held]}"
moved_from "$issue_a/decisions-md#q-shelf"
[[ "$moved" == "$issue_a/spec#q-shelf" ]] ||
  fail kinds "the ask block q-shelf the section $held_heading held moves into the spec under its id" "it moves to $moved"
grep -qF ':::ask{#q-shelf ' "$work/converted-a.md" ||
  fail kinds "the spec's newest version holds the moved ask block q-shelf" "$(grep -F 'q-shelf' "$work/converted-a.md" || echo 'no line names q-shelf')"
query kinds "read the rows naming a block of the decisions document of $issue_a" \
  "select count(*) from asks where block_artifact_id = '$decisions_a'"
[[ "$answer" == 0 ]] || fail kinds "no row names a block of the retired decisions document" "$answer rows do"
held_decision="$issue_a/spec#$held_heading"
moved_from "$issue_a/decisions-md#$held_heading"
[[ "$moved" == "$held_decision" ]] ||
  fail kinds "the rest of the section $held_heading becomes the decision that keeps its heading id" "it moves to $moved"
entry "$issue_a" block "$held_decision"
[[ -s "$work/entry" ]] || fail kinds "the rest of the section $held_heading is a decision node of the spec" "no entry for $held_decision"
expect kinds "the rest of the section $held_heading is an agent's decision" '"agent"' '.kind'
pass kinds "the section $held_heading held the ask block q-shelf, which no row names: it moved by R6's second shape to $issue_a/spec#q-shelf, and the rest of the section is the decision $held_decision"

# The three sections the conversion cannot place under one ask: each a decision that keeps its
# heading id, takes the section's links as shaped and no from_ask, cites each of its asks, and
# waits for a person's review.
declare -A further_cause=(
  [two_asks]="it cites more than one answered ask"
  [issue_level]="the answered ask it cites has its block in no document of the issue"
  [body]="its body is no reading one answered ask can hold"
)
declare -A further_asks=(
  [two_asks]="${seed_ids[ask_two_store]} ${seed_ids[ask_two_format]}"
  [issue_level]="${seed_ids[ask_issue_level]}"
  [body]="${seed_ids[ask_body]}"
)
grep -qF -- '- The census reads every row once.' "$work/converted-a.md" ||
  fail kinds "the decision of the section whose body no reading holds keeps its list" "no such line in the spec's newest version"
for kind in two_asks issue_level body; do
  heading="${seed_ids[section_$kind]}"
  decision="$issue_a/spec#$heading"
  moved_from "$issue_a/decisions-md#$heading"
  [[ "$moved" == "$decision" ]] || fail kinds "the section $heading ($kind) moves to the decision that keeps its heading id" "it moves to $moved"
  entry "$issue_a" block "$decision"
  [[ -s "$work/entry" ]] || fail kinds "the section $heading ($kind) is a decision node of the spec" "no entry for $decision"
  expect kinds "the section $heading ($kind) is an agent's decision" '"agent"' '.kind'
  expect kinds "the decision of section $heading ($kind) reads uncertainty high" '"high"' '.uncertainty'
  reason="The conversion of the decisions document could not place this section under one ask: ${further_cause[$kind]}. It waits for a person's review."
  expect kinds "the decision of section $heading ($kind) gives the reason it waits for a person's review" \
    "$(jq -cn --arg r "$reason" '$r')" '.uncertainty_reason'
  current_of block "$decision" shaped
  sed 's/^block://' "$work/rows" | LC_ALL=C sort -u >"$work/shaped"
  jq -r --arg h "$heading" '.decisions[] | select(.block_id == $h) | .links[].to.id' "$work/capture/decisions.$issue_a.json" |
    LC_ALL=C sort -u >"$work/links"
  [[ -s "$work/links" ]] || fail kinds "the capture holds the links of section $heading ($kind)" "none"
  cmp -s "$work/shaped" "$work/links" ||
    fail kinds "the decision of section $heading ($kind) takes the section's links as shaped" \
      "shaped: $(tr '\n' ' ' <"$work/shaped"); links: $(tr '\n' ' ' <"$work/links")"
  current_of block "$decision" from_ask
  [[ ! -s "$work/rows" ]] || fail kinds "the decision of section $heading ($kind) takes no from_ask" "$(tr '\n' ' ' <"$work/rows")"
  read -ra cited_asks <<<"${further_asks[$kind]}"
  for cited in "${cited_asks[@]}"; do
    query kinds "read the spec's mentions of ask $cited" \
      "select count(*) from block_refs where artifact_id = '$spec_a' and to_kind = 'ask' and kind = 'mentions' and to_id = '$cited'"
    [[ "$answer" -gt 0 ]] || fail kinds "the decision of section $heading ($kind) cites ask $cited, which the block index records" "no mention of it in $spec_a"
    current_of ask "$cited" shaped
    [[ ! -s "$work/rows" ]] || fail kinds "ask $cited takes none of the links of section $heading ($kind)" "$(tr '\n' ' ' <"$work/rows")"
  done
  pass kinds "the section $heading ($kind) is the decision $decision: its heading id, its links as shaped, no from_ask, a citation of each of its asks (${further_asks[$kind]}), uncertainty high and the reason: ${further_cause[$kind]}"
done
query kinds "read whether the decisions document of $issue_a is retired" \
  "select (retired_at is not null)::text from artifacts where id = '$decisions_a'"
[[ "$answer" == true ]] || fail kinds "the decisions.md of $issue_a, holding a section of each further kind, retires" "retired: $answer"
pass kinds "the decisions.md of $issue_a, which held a section of each of R9's four further kinds, is retired"

# The approved specs still read approved, at a version whose only change is the conversion's.
# conversion_only FILE: FILE without decision blocks, reading blocks, the shaped pointers of ask
# blocks, a nested ask's wider fence and repeated blank lines. The blank lines before a removed
# block go with it: they are the separator its insertion wrote, so a reading added after an
# ask's last option leaves no blank line above the ask's closing fence.
conversion_only() {
  awk '
    skip { if ($0 == fence) { skip = 0 } ; next }
    /^:+(decision|reading)\{/ { held = 0; skip = 1; match($0, /^:+/); fence = substr($0, 1, RLENGTH); next }
    $0 == "" { held++; next }
    { for (; held > 0; held--) print "" }
    /^::::+ask\{/ { sub(/^:+/, ":::"); asking = 1 }
    asking && /^::::+$/ { $0 = ":::"; asking = 0 }
    /^:+ask\{/ { gsub(/ shaped="[^"]*"/, "") }
    { print }
  ' "$1" | cat -s | sed -e :a -e '/^\n*$/{$d;N;ba' -e '}'
}
for spec in "$spec_a" "$spec_b"; do
  send agent GET "/api/v1/artifacts/$spec"
  want_status 200 approval "read the spec $spec"
  field approval "the newest version of $spec" '[.versions[].number] | max'
  latest="$value"
  expect approval "the spec $spec a person approved still reads approved after the conversion" '"approved"' '.approval.state'
  expect approval "the approval of $spec stands at its newest version, $latest" "$latest" '.approval.version'
  [[ "$latest" -gt "${restored_version[$spec]}" ]] ||
    fail approval "the conversion wrote a version of $spec" "it is still at ${restored_version[$spec]}"
  send agent GET "/api/v1/artifacts/$spec/versions/$latest"
  want_status 200 approval "read version $latest of $spec"
  jq -j '.markdown' "$work/body" >"$work/converted.md"
  conversion_only "$work/converted.md" >"$work/converted.left"
  conversion_only "$work/restored.$spec.md" >"$work/restored.left"
  diff "$work/restored.left" "$work/converted.left" >"$work/diff" ||
    fail approval "version $latest of $spec changes only the decision blocks, the readings and the shaped pointers over version ${restored_version[$spec]}" "$(<"$work/diff")"
  pass approval "$spec reads approved at version $latest, whose only change over version ${restored_version[$spec]} is the decision blocks, the readings inside answered asks and the shaped pointers"
done

# ---------------------------------------------------------------------------------------------
# The node answer beside the document answer, and the old member beside entries.
# ---------------------------------------------------------------------------------------------
missing=0
extra=0
unpaired=0
outside=0
declare -A outside_issues=()
declare -A paired_nodes=()
while IFS=$'\t' read -r issue heading ask _; do
  file="${entries_file[$issue]}"
  if [[ "$ask" != - ]]; then
    node="ask:$ask"
  elif jq -e --arg n "$issue/spec#$heading" '[.entries[] | select(.node.kind == "block" and .node.id == $n)] | length == 1' "$file" >/dev/null; then
    node="block:$issue/spec#$heading"
  else
    printf 'unpaired: section %s of %s matches neither the ask the capture names nor a decision keeping its heading id\n' "$heading" "$issue"
    unpaired=$((unpaired + 1))
    continue
  fi
  paired_nodes["$issue $node"]=1
  in_entries="$(jq --arg n "$node" '[.entries[] | select("\(.node.kind):\(.node.id)" == $n)] | length' "$file")"
  block_id="$(jq -r --arg n "$node" '[.entries[] | select("\(.node.kind):\(.node.id)" == $n)][0].document.block_id // ""' "$file")"
  document="$(jq -r --arg n "$node" '[.entries[] | select("\(.node.kind):\(.node.id)" == $n)][0].document.ref // ""' "$file")"
  in_member="$(jq --arg b "$block_id" '[.decisions[] | select(.block_id == $b)] | length' "$file")"
  if [[ "$in_entries" != 1 || "$in_member" != 1 ]]; then
    printf 'missing: section %s of %s, paired with %s: %s in entries, %s in the old decisions member\n' \
      "$heading" "$issue" "$node" "$in_entries" "$in_member"
    missing=$((missing + 1))
    continue
  fi
  if [[ "$document" != "dispatch://$issue/spec" ]]; then
    printf 'in no spec: section %s of %s, paired with %s, stands in %s\n' "$heading" "$issue" "$node" "${document:-nothing}"
    outside=$((outside + 1))
    outside_issues["$issue"]=1
    continue
  fi
  printf 'paired: section %s of %s with %s, at %s#b-%s\n' "$heading" "$issue" "$node" "$document" "$block_id"
done <"$work/capture/sections"
for issue in "$issue_a" "$issue_b"; do
  while IFS= read -r node; do
    [[ -n "${paired_nodes[$issue $node]:-}" ]] && continue
    printf 'extra: %s of %s is a decision node no section became\n' "$node" "$issue"
    extra=$((extra + 1))
  done < <(jq -r '.entries[] | select(.kind == "agent") | "\(.node.kind):\(.node.id)"' "${entries_file[$issue]}")
done
printf 'pairing: missing=%s extra=%s unpaired=%s\n' "$missing" "$extra" "$unpaired"
printf 'issues holding a section no rule dropped and no spec holds: %s%s\n' "${#outside_issues[@]}" \
  "$([[ "${#outside_issues[@]}" == 0 ]] || printf ' (%s)' "${!outside_issues[*]}")"
[[ "$missing" == 0 && "$extra" == 0 && "$unpaired" == 0 ]] ||
  fail pairing "every section is paired with the node it became" "missing=$missing extra=$extra unpaired=$unpaired, each printed above"
[[ "$outside" == 0 ]] || fail pairing "every section no rule dropped stands in a spec" "$outside sections in ${#outside_issues[@]} issues do not"
pass pairing "every section of both decisions documents is paired with its node, in a spec: missing=0 extra=0 unpaired=0"

# An answered ask no block holds (an issue-level ask) stands in entries with no document, and in the
# old member under the block id "": both read it as "".
for issue in "$issue_a" "$issue_b"; do
  file="${entries_file[$issue]}"
  jq -r '[.entries[] | select(.kind == "agent" or (.kind == "person" and .answer != null)) | .document.block_id // ""] | sort | .[]' "$file" >"$work/entry-ids"
  jq -r '[.decisions[].block_id] | sort | .[]' "$file" >"$work/member-ids"
  cmp -s "$work/entry-ids" "$work/member-ids" ||
    fail member "entries and the old decisions member of $issue answer the same decisions" \
      "entries: $(tr '\n' ' ' <"$work/entry-ids"); decisions: $(tr '\n' ' ' <"$work/member-ids")"
  while IFS= read -r block_id; do
    jq -cS --arg b "$block_id" '[.entries[] | select(.kind == "agent" or (.kind == "person" and .answer != null)) |
      select((.document.block_id // "") == $b)][0] |
      {changed_version, changed_at, stale: ([.shaped[] | {to: .to.id, stale}] | sort_by(.to))}' "$file" >"$work/from-entries"
    jq -cS --arg b "$block_id" '[.decisions[] | select(.block_id == $b)][0] |
      {changed_version, changed_at, stale: ([.links[] | {to: .to.id, stale}] | sort_by(.to))}' "$file" >"$work/from-member"
    cmp -s "$work/from-entries" "$work/from-member" ||
      fail member "the decision $block_id of $issue reads the same stale, changed_version and changed_at in both members" \
        "entries: $(<"$work/from-entries"); decisions: $(<"$work/from-member")"
  done <"$work/entry-ids"
  pass member "entries and the old decisions member of $issue answer the same $(wc -l <"$work/entry-ids") decisions, with the same stale, changed_version and changed_at"
done

# ---------------------------------------------------------------------------------------------
# The catch-up: the base answers an ask this head created, beside it.
# ---------------------------------------------------------------------------------------------
baseline_stop_pid "$head_pid"
head_pid=""
baseline_boot base "$BASELINE_BINARY" "$baseline_database_url"
beside_pid="$baseline_boot_pid"
server="$baseline_boot_url"
readonly base_url="$server"
sign_in catchup
send agent POST "/api/v1/issues/$issue_c/artifacts" "$(jq -cn --argjson a "$(seed_actor)" '{name: "decisions.md",
  content: "# Decisions\n\n## The corpus stays on one server\n\nOne server writes the corpus.\n", actor: $a}')"
want_status 201 catchup "the base release writes a decisions.md on $issue_c"
seed_wait_settled "the decisions.md of $issue_c settles"
# This head, without the hold setting, beside the base.
baseline_extra_env=(DISPATCH_TEST_HOOKS=1)
baseline_boot head-unheld "$head_binary" "$baseline_database_url"
head_pid="$baseline_boot_pid"
server="$baseline_boot_url"
readonly head_url="$server"
commit_of catchup
[[ "$value" == "$head_commit" ]] || fail catchup "this head serves its own commit" "$value"
server="$base_url"
commit_of catchup
[[ "$value" == "$base_commit" && "$value" != "$head_commit" ]] ||
  fail catchup "the base beside it serves a commit of its own" "base $value, head $head_commit"
pass catchup "this head ($head_commit, unheld) and the base ($base_commit) serve one database, both stamped"
server="$head_url"
send agent POST "/api/v1/issues/$issue_n/asks" \
  "$(jq -cn --argjson a "$(seed_actor)" '{question: "Which day does the corpus refresh?", options: [{label: "Monday"}, {label: "Friday"}], actor: $a}')"
want_status 201 catchup "this head opens an ask"
field catchup "the ask's id" '.id // .ask.id'
readonly catchup_ask="$value"
server="$base_url"
send person POST "/api/v1/asks/$catchup_ask/answer" \
  '{"text":"Friday, after the week'"'"'s writes.","selected":["Friday"],"expected_edited_at":null}'
want_status 200 catchup "the base answers the ask this head opened"
send agent GET "/api/v1/asks/$catchup_ask"
want_status 200 catchup "read the answer through the base"
field catchup "the answer's time" '.ask.answer.at'
answered_at="$value"
query catchup "read the ask.answered event" \
  "select id from events where type = 'ask.answered' and payload->>'id' = '$catchup_ask' order by id desc limit 1"
readonly answered_event="$answer"
[[ "$answered_event" =~ ^[0-9]+$ ]] || fail catchup "the base appended ask.answered" "no event"
waited=0
while true; do
  query catchup "read the catch-up's answer tuple" \
    "select count(*) from graph_tuples where from_kind = 'ask' and from_id = '$catchup_ask' and kind = 'answer' and source_event = $answered_event"
  [[ "$answer" == 0 ]] || break
  [[ "$waited" -lt 1800 ]] || fail catchup "the catch-up states the base's answer on the ask node" "no answer tuple under event $answered_event after 180 s"
  sleep 0.1
  waited=$((waited + 1))
done
rows "select kind, to_kind, to_id, source_event from graph_tuples
      where from_kind = 'ask' and from_id = '$catchup_ask' and kind in ('answer', 'selected', 'answered_by', 'answered_at')
        and source_event = $answered_event and asserted order by kind"
printf 'the catch-up stated, under event %s:\n%s\n' "$answered_event" "$(<"$work/rows")"
declare -A stated=()
while IFS='|' read -r kind to_kind to_id _; do
  stated["$kind"]="$to_kind:$to_id"
done <"$work/rows"
[[ "${stated[answer]:-}" == "value:Friday, after the week's writes." ]] || fail catchup "the answer on the ask node, under that event" "${stated[answer]:-none}"
[[ "${stated[selected]:-}" == value:*Friday* ]] || fail catchup "the selection on the ask node, under that event" "${stated[selected]:-none}"
[[ "${stated[answered_by]:-}" == "person:$seed_person" ]] || fail catchup "the answerer on the ask node, under that event" "${stated[answered_by]:-none}"
[[ -n "${stated[answered_at]:-}" ]] || fail catchup "the answer's time on the ask node, under that event" "none"
query catchup "compare the stated time with the answer's" \
  "select ('${stated[answered_at]#value:}'::timestamptz = '$answered_at'::timestamptz)::text"
[[ "$answer" == true ]] || fail catchup "the time on the ask node is the answer's" "${stated[answered_at]} against $answered_at"
pass catchup "the answer, the selection, the answerer and the time of the base's answer stand on the ask node, each under ask.answered event $answered_event"

# ---------------------------------------------------------------------------------------------
# The step at boot leaves no decisions document.
# ---------------------------------------------------------------------------------------------
# This head goes first: with the base still live it converts nothing, so the decisions document of
# issue C still stands when the next boot starts.
baseline_stop_pid "$head_pid"
head_pid=""
baseline_stop_pid "$beside_pid"
beside_pid=""
query boot "read the decisions documents left" "select count(*) from decisions_documents()"
[[ "$answer" -gt 0 ]] || fail boot "a decisions document stands before this head boots" "decisions_documents() answers none"
printf 'decisions documents standing before the boot: %s\n' "$answer"
started="$(date +%s)"
baseline_boot head-boot "$head_binary" "$baseline_database_url"
head_pid="$baseline_boot_pid"
server="$baseline_boot_url"
while true; do
  query boot "read the decisions documents left" "select count(*) from decisions_documents()"
  [[ "$answer" != 0 ]] || break
  [[ $(($(date +%s) - started)) -lt "$step_bound_seconds" ]] ||
    fail boot "the step at boot leaves no decisions document within ${step_bound_seconds} s" "$answer still stand"
  sleep 1
done
elapsed=$(($(date +%s) - started))
pass boot "the step at boot left no decisions document, ${elapsed} s after this head started, within ${step_bound_seconds} s"

# /healthz answers decisions_step_owed zero once a tick of the step has counted nothing owed: every
# decisions.md retired, no answered ask left out of its spec, and no list of gated steps holding
# `decisions`. The member is absent before the process's first tick has counted.
started="$(date +%s)"
while true; do
  send agent GET /healthz
  want_status 200 owed "read /healthz of this head"
  owed="$(jq -r '.decisions_step_owed // "absent"' "$work/body")"
  [[ "$owed" != 0 ]] || break
  [[ $(($(date +%s) - started)) -lt "$step_bound_seconds" ]] ||
    fail owed "/healthz answers decisions_step_owed zero within ${step_bound_seconds} s" "it answers $owed: $(body)"
  sleep 1
done
pass owed "/healthz answers decisions_step_owed 0, $(($(date +%s) - started)) s after the last decisions document left"
