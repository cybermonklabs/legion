#!/usr/bin/env bash
# Drives check c-derived (spec requirements r-derived and r-no-regression of dispatch://INFRA-1098):
# an upgrade of a database the previous release wrote leaves the store and its projections holding
# every record that release wrote, each census reads zero both ways over a population it counts,
# the store holds the closed list of supertags R21 names, each moved read answers over the same rows
# what the read it replaces answered, and each child whose new read replaces an old one passed its
# own comparison at the head its pull request merged at.
#
# The run gives the driver an empty database of its own as DATABASE_URL and the baseline commit as
# BASELINE_SHA (the pull request body's BASELINE_SHA= line), and the driver boots its own servers
# over that one database through lib-baseline.sh, reading no server started before it:
#
#   H   Whether this head holds dispatch://INFRA-1336's merge, which creates the handoff supertag:
#       the merge commit of the newest merged pull request of legion/INFRA-1336, read through gh
#       with the role's own credential, and `git merge-base --is-ancestor <commit> HEAD`. With no
#       merged pull request the head does not hold it, and the driver prints `handoff: not created at
#       this head (dispatch://INFRA-1336)`; with one this head does not hold, it fails, printing
#       `merge main forward: dispatch://INFRA-1336 merged at <sha>, which this head does not hold`.
#   B   The baseline: BASELINE_SHA built from its own tree and booted, its /healthz naming that
#       commit. Through its own API alone (the agent bearer DISPATCH_TOKEN, and DEV_SIGNIN_LOGIN
#       signed in at its dev sign-in route) it writes the fixture: PROJECT; an issue the main issue
#       waits on; the main issue, routed to a role, whose spec holds a memo callout, two
#       requirements, an ask block and prose, whose Verification document holds two checks (one
#       serving both requirements, one requirement served by both) and prose, and whose plan holds
#       two plan tasks (one serving a requirement, one needing the other) and prose; a child of it;
#       a result recorded on a check; an open ask of the issue and the ask block answered by the
#       person; the person's comment mentioning a session, the session's reply to it; the person's
#       message to that session on the issue and a direct one to it; and one run of the command
#       bar. Where this head holds INFRA-1336's merge, also one handoff
#       (POST /api/v1/issues/{key}/handoffs/{phase}). No decisions document: the Decisions read and
#       the conversion it needs are c-decisions-converted's, so the kinds whose rows come from one
#       are outside this inventory. It prints the count it wrote of each migrated kind, and fails on
#       a kind it wrote none of, naming the gap in the fixture.
#   I   The inventory, straight from that database before the upgrade: every row of every kind
#       this tree migrates, each as the node or tuple it is to become. The priorities, runs and
#       plans that the boot's move of R32 takes into the store are c-stores-moved's, whose driver
#       reads them across the move: the fixture writes none, and `census --store` reads their three
#       kinds with no refusal of a zero count. A component is written only by the architecture importer from
#       a repository on GitHub, which no server this harness boots reaches, so the fixture writes
#       none and the inventory holds none, which its `inventory: component` line says with the
#       count the baseline's rows hold; `census --store` still reads the component censuses.
#   O   The old answers, on the baseline before the upgrade: the coverage and results reads; the
#       Spec, Verification and Plan documents read with their blocks (GET .../text?blocks=true);
#       and, as the person, the three reads the one inbox replaces (GET /api/v1/inbox,
#       GET /api/v1/me/replies and GET /api/v1/agents/{session_id}/messages).
#   U   The upgrade: the baseline stops, keeping the database, and this head's envoy-dispatch (on
#       PATH, or HEAD_BINARY) boots over it, migrates it and runs its boot's passes, which the
#       driver waits for by the line `dispatch: boot backfill done`; then `envoy-dispatch
#       backfill-graph-store` writes the store's nodes of the rows the release before wrote; then
#       the driver waits, polling every 2 s for at most 300 s, until no document_block_versions row
#       holds store_owed_at, the mark of a document the block index owes this build, printing the
#       documents it saw owed at its start and failing naming those still owed. A census before
#       that reads every block of an owed document as a difference, since the reconciliation has
#       not yet stated its part_of, and a wait on owed documents alone can pass before the backfill
#       marks anything, so it comes after both. Each step of the run prints a `time:` line.
#   S   The store's supertags against R21's closed list of thirty-one, handoff apart where this head
#       does not hold INFRA-1336's merge: each the store lacks fails, and so does each the store
#       holds that the list does not name and each node whose supertag sits outside the list.
#   C   Each census of `envoy-dispatch census --store`, the store against the projection that
#       serves the read for its kind, both ways, every one at zero differences.
#   V   The inventory against the store and against the projections, kind by kind: every record the
#       upgrade lost from either is printed, and the run fails on any. A mention, a delivery and a
#       read mark are tuples and never nodes; a delivery attempt is a delivery_attempt node, whose
#       state and envelope the node holds; a value's history is its appended tuples; a retracted
#       relation is ended rather than deleted; and an edge whose target block was deleted is marked
#       removed, its count printed.
#   R   The moved reads, each answer at this head once the tree read of each in-flight issue the
#       baseline wrote answers `converted: true`, polled every 2 s and failing after 300 s naming
#       each issue that stayed unconverted: the coverage and results reads as the release before
#       answered them, the Spec, Verification and Plan queries over the blocks the documents held,
#       the tree read answering each typed block once and no prose block, and the one inbox
#       (GET /api/v1/me/inbox) answering every row the three reads it replaces answered.
#   K   For each child whose new read replaces an old one, its comparison check passed at the head
#       its pull request merged at: dispatch://INFRA-1149's c-census, dispatch://INFRA-1150's
#       c-census, dispatch://INFRA-1152's c-routes-gone and dispatch://INFRA-1253's c-queries. The
#       head is that of the newest merged pull request of legion/<KEY>, read through gh with the
#       role's own credential; the result is read with `dispatch results --issue <KEY> --at <head>`
#       against the Dispatch the pod's own DISPATCH_URL names, with the pod's own
#       DISPATCH_TOKEN_FILE, which the command reads before DISPATCH_TOKEN
#       (packages/envoy-client/src/dispatch-config.ts). It writes nothing there.
#
# Inputs: BASELINE_SHA, DATABASE_URL (the run's own empty database, on this machine), DISPATCH_TOKEN
# (the agent bearer every server here is booted with), PROJECT, DEV_SIGNIN_LOGIN and TMPDIR; and the
# pod's own DISPATCH_URL and DISPATCH_TOKEN_FILE, which name the deployed Dispatch K reads. HEAD_BINARY
# names this head's envoy-dispatch where the one on PATH is not it. Each unmet input is exit 2. The
# servers and the work directory go when the run ends, also when a check fails; the database is
# the run's own, which it drops.
#
# Each passing check prints `PASS: <section> <what>`; the first failing one prints
# `FAIL: <section> <what>: <observed>` on stderr and the run exits 1, after printing every record of
# the inventory that the section lost. It prints no SKIP line, no bearer and no email address.
#
# Every check is `cmd || fail …`, never part of an `&&` list: under `set -e` a failing command
# anywhere but last in such a list does not stop the script.

# shellcheck disable=SC2016 # the single-quoted jq filters and SQL name jq and psql variables, not shell ones
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: BASELINE_SHA=<the baseline commit> DATABASE_URL=${LEGION_TEST_PG_DSN%/*}/infra_1098_census?sslmode=disable \
         DISPATCH_TOKEN=e2e-token PROJECT=CORE DEV_SIGNIN_LOGIN=alice [HEAD_BINARY=<this head's envoy-dispatch>] \
         bash e2e/acceptance/graph-census.sh
       (run from packages/dispatch, in a pod whose own DISPATCH_URL and DISPATCH_TOKEN_FILE name the
       deployed Dispatch, which is read and never written. DATABASE_URL names an empty database on
       this machine, which the run created and drops. HEAD_BINARY defaults to the envoy-dispatch on
       PATH, the Verification preamble's build of this head.)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
# shellcheck source-path=SCRIPTDIR source=lib-baseline.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib-baseline.sh"

readonly driver=graph-census.sh
require_env BASELINE_SHA DATABASE_URL DISPATCH_TOKEN PROJECT DEV_SIGNIN_LOGIN TMPDIR
# The deployed Dispatch K reads, by the pod's own address and token file, kept before anything
# here unsets them.
# shellcheck disable=SC2031 # the pod's own DISPATCH_URL, before lib.sh's `cli` ever runs
[[ -n "${DISPATCH_URL:-}" && -n "${DISPATCH_TOKEN_FILE:-}" ]] || {
  printf 'the pod'"'"'s own DISPATCH_URL and DISPATCH_TOKEN_FILE name the deployed Dispatch section K reads, and one is unset\n' >&2
  usage
  exit 2
}
# shellcheck disable=SC2031 # the pod's own DISPATCH_URL, before lib.sh's `cli` ever runs
readonly deployed_url="$DISPATCH_URL" deployed_token_file="$DISPATCH_TOKEN_FILE"
[[ -r "$deployed_token_file" ]] || {
  printf 'DISPATCH_TOKEN_FILE names %s, which cannot be read\n' "$deployed_token_file" >&2
  exit 2
}
require_commands awk bun curl gh git go jq psql python3 sed sort tar
# Not readonly: lib-baseline.sh's baseline_boot takes a local of this name.
# shellcheck disable=SC2031 # the run's own DATABASE_URL: only a server's subshell sets another
database_url="$DATABASE_URL"
loopback_database_url "$database_url" || {
  printf 'DATABASE_URL names no database on this machine: every server here migrates it and the driver reads it\n' >&2
  usage
  exit 2
}
# The servers get no pair the caller exported for another driver.
unset BASELINE_EXTRA_ENV
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
checkout="$(git -C "$script_dir" rev-parse --show-toplevel)" || {
  printf 'cannot find the checkout %s is part of\n' "$script_dir" >&2
  exit 2
}
readonly checkout
readonly cli_entry="$checkout/packages/envoy-client/bin/dispatch.ts"
[[ -r "$cli_entry" ]] || {
  printf '%s cannot be read: section K runs that dispatch CLI\n' "$cli_entry" >&2
  exit 2
}
head_binary="${HEAD_BINARY:-$(command -v envoy-dispatch || true)}"
[[ -n "$head_binary" && -f "$head_binary" && -x "$head_binary" ]] || {
  printf 'no envoy-dispatch on PATH, and HEAD_BINARY names none: build this head as the Verification preamble does\n' >&2
  exit 2
}
[[ "$head_binary" == /* ]] || head_binary="$PWD/$head_binary"
readonly head_binary
# shellcheck disable=SC2153 # the run's PROJECT and DEV_SIGNIN_LOGIN, which require_env checked above
readonly project="$PROJECT" login="$DEV_SIGNIN_LOGIN"
stamp="$(date +%s)-$$"
readonly stamp
readonly session="graph-census-$stamp"
# The agent session the fixture mentions, messages and replies as: a uuid, since the agent routes
# refuse any other session id.
agent_session="019a1c0c-0000-7000-8000-$(printf '%012d' "$(($(date +%s) % 1000000 * 100000 + $$ % 100000))")"
readonly agent_session
readonly fixture_role="graph-census-$stamp"
# How long the conversion of the in-flight issues may take, polled every 2 s (R), and how long the
# block index may take to drain the documents backfill-graph-store marked owed (U).
readonly convert_limit=300 owed_limit=300

# R21's closed list of supertags: the thirty-one of spec version 117, handoff among them, the query
# and query version nodes R28's approval row points at, and priority, step and plan, which R32 adds
# (20261126000300_stores_moved).
readonly -a r21_supertags=(issue document "document version" block callout ask requirement plan_task check result
  decision reading criterion lens lens_kind constraint risk comment message person session role page interaction
  delivery_attempt component supertag attribute handoff query "query version" priority step plan)
# The four children whose new read replaces an old one, each with the check that compares the two.
readonly -a compared_children=("INFRA-1149 c-census" "INFRA-1150 c-census" "INFRA-1152 c-routes-gone" "INFRA-1253 c-queries")

work_dir
mkdir "$work/old" "$work/home" "$work/state"
head_pid=""
cleanup() {
  local code=$?
  baseline_stop_pid "$head_pid"
  baseline_stop
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# --- helpers -------------------------------------------------------------------------------------

# step_begin and step_end WHAT: the time a step of the run took, printed as one `time:` line with
# the wall-clock moments it began and ended, so a slow step is named in the run's own output.
step_started=$SECONDS
step_started_at=""
step_begin() {
  step_started=$SECONDS
  step_started_at="$(date -u +%H:%M:%S)"
}
step_end() {
  printf 'time: %s took %d s, from %s to %s\n' "$1" "$((SECONDS - step_started))" "$step_started_at" "$(date -u +%H:%M:%S)"
}

# field SECTION WHAT JQ: one value of the last answer's body, which must be there, in $value.
field() {
  value="$(jq -er "$3" "$work/body" 2>/dev/null)" || fail "$1" "$2" "no $3 in: $(body)"
}

# wait_zero SECTION WHAT SQL LIMIT: waits until SQL reads 0, polling every 0.2 s, for at most LIMIT
# seconds.
wait_zero() {
  local started=$SECONDS
  while true; do
    query "$1" "$2" "$3"
    [[ "$answer" != 0 ]] || return 0
    ((SECONDS - started < $4)) || fail "$1" "$2 within $4 s" "it still reads $answer"
    sleep 0.2
  done
}

# settled SECTION: waits until no document owes a settlement, so that what a write left is read.
settled() {
  wait_zero "$1" "no document owes a settlement" 'select count(*) from doc_settlements_pending' 180
}

# as_person METHOD PATH [JSON]: request as the person, whose cookie sign_in keeps.
as_person() { request "cookie:$login" "$@"; }

# login_of TEXT: TEXT with every email address reduced to its login, so no line prints one.
login_of() { sed -E 's/([A-Za-z0-9._%+-]+)@[A-Za-z0-9.-]+\.[A-Za-z]{2,}/\1/g' <<<"$1"; }

# create_issue SECTION NAME BODY: the agent creates an issue of PROJECT from BODY, a jq expression
# of an object (title, spec and what else it names), and keeps its key in key_of[NAME].
declare -A key_of=()
create_issue() {
  send POST /api/v1/issues "$(jq -cn --arg p "$project" --argjson a "$(actor)" "($3) + {project: \$p, force: true, actor: \$a}")"
  want_status 201 "$1" "create the issue $2"
  field "$1" "the key of $2" .key
  key_of["$2"]="$value"
}

# upload SECTION KEY NAME CONTENT: the agent uploads CONTENT as the document NAME of the issue KEY.
upload() {
  send POST "/api/v1/issues/$2/artifacts" "$(jq -cn --arg n "$3" --arg c "$4" --argjson a "$(actor)" '{name: $n, content: $c, actor: $a}')"
  [[ "$status" == 200 || "$status" == 201 ]] || fail "$1" "upload $3 to $2" "HTTP $status: $(body)"
}

# inventory_sql: every row of every kind this tree migrates, one `kind<TAB>node` line each, the
# node the store is to hold for it (a node as `kind:id`, a tuple as `from relation to`). It reads
# the projection, so it is run once against the release before's rows and once after the upgrade.
inventory_sql() {
  cat <<SQL
select 'issue', 'issue:' || key from issues
union all select 'document', 'artifact:' || ref_key from artifacts
union all select 'document version', 'version:' || a.ref_key || '@v' || v.number from artifact_versions v join artifacts a on a.id = v.artifact_id
union all select 'block', 'block:' || a.ref_key || '#' || b.block_id from document_blocks b join artifacts a on a.id = b.artifact_id
union all select 'ask', 'ask:' || id::text from asks
union all select 'comment', 'comment:' || id::text from comments
union all select 'message', 'message:' || id::text from messages
union all select 'result', 'result:' || id::text from check_results
union all select 'interaction', 'interaction:' || id::text from going_requests
union all select distinct 'person', 'person:' || lower(w.author->>'id') from (
    select author from comments union all select author from messages union all select author from going_requests
  ) w where w.author->>'kind' = 'user' and coalesce(w.author->>'id', '') <> ''
union all select distinct 'session', 'session:' || (w.author->>'id') from (
    select author from comments union all select author from messages union all select author from asks
  ) w where w.author->>'kind' = 'session' and coalesce(w.author->>'id', '') <> ''
union all select distinct 'role', 'role:' || substr(route, 6) from issues where route like 'role:%'
union all select distinct 'page', 'page:role/' || substr(route, 6) from issues where route like 'role:%'
union all select 'delivery_attempt', 'delivery_attempt:comment/' || comment_id::text || '/' || target || '/' || attempt from comment_deliveries
union all select 'delivery_attempt', 'delivery_attempt:message/' || message_id::text || '/' || attempt from message_deliveries
union all select 'mention', 'comment:' || comment_id::text || ' mentions ' || replace(target, 'user:', 'person:') from comment_mentions
union all select 'child_of', 'issue:' || key || ' child_of issue:' || parent_key from issues where parent_key is not null
union all select 'needs', 'issue:' || issue_key || ' needs issue:' || target_key from issue_links where kind = 'blocked_by'
$([[ "$handoff_held" == 1 ]] && printf "union all select 'handoff', 'handoff:' || id::text from handoffs")
SQL
}

# The kinds the inventory reads, in the order it prints them.
inventory_kinds() {
  printf '%s\n' issue document "document version" block ask comment message result interaction person session role page \
    delivery_attempt mention child_of needs
  [[ "$handoff_held" != 1 ]] || printf 'handoff\n'
}

# take_inventory FILE: inventory_sql's lines, sorted, in FILE.
take_inventory() {
  env -u PGHOSTADDR -u PGSERVICE -u PGSERVICEFILE psql "$database_url" -X -q -At -F $'\t' -v ON_ERROR_STOP=1 \
    -c "$(inventory_sql)" 2>"$work/psql" | LC_ALL=C sort >"$1" || fail "$2" "take the inventory" "psql: $(<"$work/psql")"
}

# old_read NAME AS PATH: AS reads PATH on the baseline, kept as $work/old/NAME.json.
old_read() {
  request "$2" GET "$3"
  want_status 200 O "read $3 on the baseline"
  cp "$work/body" "$work/old/$1.json"
}

# --- H: does this head hold dispatch://INFRA-1336's merge? --------------------------------------

gh pr list --head legion/INFRA-1336 --state merged --limit 100 --json number,mergeCommit,mergedAt >"$work/infra-1336.json" 2>"$work/gh.err" ||
  fail H "read the merged pull requests of legion/INFRA-1336 through gh" "gh exited non-zero: $(<"$work/gh.err")"
merge_1336="$(jq -r 'sort_by(.mergedAt) | last | .mergeCommit.oid // ""' "$work/infra-1336.json")"
handoff_held=0
if [[ -z "$merge_1336" ]]; then
  printf 'handoff: not created at this head (dispatch://INFRA-1336)\n'
  pass H "legion/INFRA-1336 has no merged pull request, so this head does not hold its merge, and the handoff supertag is not R21's at this head"
else
  git -C "$checkout" merge-base --is-ancestor "$merge_1336" HEAD 2>"$work/git.err" ||
    fail H "this head holds dispatch://INFRA-1336's merge" \
      "merge main forward: dispatch://INFRA-1336 merged at $merge_1336, which this head does not hold"
  handoff_held=1
  pass H "this head holds dispatch://INFRA-1336's merge, $merge_1336"
fi
readonly handoff_held

# --- B: the baseline writes the fixture ----------------------------------------------------------

step_begin
baseline_start
step_end "the baseline's build and boot"
# The run's bearer, which baseline_start boots every server with; read before it is unset, and
# printed nowhere.
token="$baseline_token"
unset DISPATCH_TOKEN DATABASE_URL DISPATCH_TOKEN_FILE
printf 'Authorization: Bearer %s\n' "$token" >"$work/auth"
base="$baseline_url"
send GET /healthz
want_status 200 B "read the baseline's /healthz"
served="$(jq -r '.commit // ""' "$work/body")"
if [[ -n "$baseline_commit" ]]; then
  [[ "$served" == "$baseline_commit" ]] ||
    fail B "the baseline serves the commit BASELINE_SHA names" "it serves '$served', and BASELINE_SHA is $baseline_commit"
  pass B "the baseline serves $served, the commit BASELINE_SHA names, built from its tree"
else
  pass B "the baseline is BASELINE_BINARY $baseline_binary, which serves '$served'"
fi
sign_in B "$login" " at the baseline"

step_begin
send POST /api/v1/projects "$(jq -cn --arg k "$project" --argjson a "$(actor)" '{key: $k, name: ("The census of " + $k), actor: $a}')"
want_status 201 B "create the project $project"
as_person PUT "/api/v1/projects/$project/default-assignee" "$(jq -cn --arg p "$login" '{assignee: $p}')"
want_status 200 B "make $login the default assignee of $project"

create_issue B upstream '{title: "The issue the census fixture waits on", spec: "An issue another waits on.\n"}'
readonly spec_text=':::callout{#memo kind="note" title="Memo"}
The memo the Spec query answers.
:::

:::requirement{#r-a}
Every kind the release moves is held in the store.
:::

:::requirement{#r-b}
Every census of the store reads zero differences.
:::

A paragraph of the spec that no query answers.

:::ask{#q-shape urgency="med"}
Which shape should the census print?

- Lines: one line per kind
- Table: one table of every kind
:::
'
create_issue B main "$(jq -cn --arg s "$spec_text" --arg w "${key_of[upstream]}" \
  '{title: "The issue the census fixture writes", spec: $s, blocked_by: [$w]}')"
readonly main="${key_of[main]}"
create_issue B child "$(jq -cn --arg p "$main" '{title: "A child of the census fixture issue", spec: "A child.\n", parent: $p}')"
send PATCH "/api/v1/issues/$main" "$(jq -cn --arg r "role:$fixture_role" --argjson a "$(actor)" '{route: $r, actor: $a}')"
want_status 200 B "route $main to the role $fixture_role"
upload B "$main" verification.md ":::check{#k-a serves=\"dispatch://$main/spec#b-r-a dispatch://$main/spec#b-r-b\" surface=\"api\" run=\"bash census-a.sh\" when=\"before_merge\" negative_control=\"Break the census and read it fail.\"}
The census reads zero over both requirements.
:::

:::check{#k-b serves=\"dispatch://$main/spec#b-r-a\" surface=\"cli\" run=\"bash census-b.sh\"}
The store holds every kind.
:::

A paragraph of the Verification document that no query answers.
"
upload B "$main" plan.md ":::plan_task{#t-a serves=\"dispatch://$main/spec#b-r-a\"}
Write the store's nodes.
:::

:::plan_task{#t-b needs=\"dispatch://$main/artifact/plan-md#b-t-a\"}
Read the census.
:::

A paragraph of the plan that no query answers.
"
settled B
send POST "/api/v1/issues/$main/results" "$(jq -cn --argjson a "$(actor)" \
  '{check: "k-b", role: "tester", verdict: "fail", commit: "0000000000000000000000000000000000000001", surface: "cli", run: "bash census-b.sh", observed: "The store lacked a kind.", actor: $a}')"
want_status 201 B "record a result of k-b"
send POST "/api/v1/issues/$main/asks" "$(jq -cn --argjson a "$(actor)" \
  '{question: "Should the census print its digests?", options: [{label: "Yes", description: "Print them"}, {label: "No", description: "Leave them out"}], actor: $a}')"
want_status 201 B "open an ask of $main"
# The ask block's row, which the settlement writes, answered by the person.
started=$SECONDS
while true; do
  send GET "/api/v1/issues/$main/asks?state=all"
  want_status 200 B "read the asks of $main"
  ask_block_id="$(jq -r '[.[] | select(.block_id == "q-shape") | .id] | if length == 1 then .[0] else "" end' "$work/body")"
  [[ -z "$ask_block_id" ]] || break
  ((SECONDS - started < 60)) || fail B "the ask block q-shape of $main settles into an ask" "no ask names it after 60 s"
  sleep 0.2
done
send GET "/api/v1/asks/$ask_block_id"
want_status 200 B "read the ask $ask_block_id"
as_person POST "/api/v1/asks/$ask_block_id/answer" "$(jq -c '{selected: ["Lines"], text: "One line per kind.", expected_edited_at: .ask.edited_at}' "$work/body")"
want_status 200 B "$login answers the ask block q-shape"
settled B
as_person POST "/api/v1/issues/$main/comments" "$(jq -cn --arg s "$agent_session" \
  '{body: "The census should name each kind it moves.", mentions: [{target: ("session:" + $s)}], delivery: "btw"}')"
want_status 201 B "$login comments on $main, mentioning the session $agent_session"
field B "the id of the comment" .id
readonly comment_id="$value"
# The session answers in the thread through the comment route's reply_to: the reply route answers a
# delivery attempt, and the mention's attempt failed, since no Envoy listener answers this server.
send POST "/api/v1/issues/$main/comments" "$(jq -cn --arg c "$comment_id" --arg s "$agent_session" \
  '{body: "It names each kind.", reply_to: $c, actor: {kind: "session", id: $s}}')"
want_status 201 B "the session $agent_session replies in the thread of the comment"
as_person POST "/api/v1/issues/$main/messages" "$(jq -cn --arg s "$agent_session" \
  '{body: "Print the census once the upgrade is done.", target: ("session:" + $s), delivery: "steer"}')"
want_status 201 B "$login messages the session $agent_session on $main"
as_person POST "/api/v1/agents/$agent_session/messages" "$(jq -cn '{body: "A direct word to the agent.", delivery: "steer"}')"
want_status 201 B "$login messages the session $agent_session directly"
as_person POST /api/v1/going/requests "$(jq -cn --arg p "$project" '{words: "What does the census count?", project: $p}')"
want_status 201 B "$login asks the command bar one question"
if [[ "$handoff_held" == 1 ]]; then
  send POST "/api/v1/issues/$main/handoffs/plan" "$(jq -cn --argjson a "$(actor)" '{body: "{\"next\": \"read the census\"}", actor: $a}')"
  want_status 201 B "the agent records a handoff of $main"
fi
settled B
step_end "the baseline's write of the fixture through its API"
pass B "the baseline's API wrote the fixture: $(printf '%s=%s ' upstream "${key_of[upstream]}" main "$main" child "${key_of[child]}")"

# --- I: the inventory, before the upgrade -------------------------------------------------------

take_inventory "$work/inventory.tsv" I
while IFS= read -r kind; do
  count="$(awk -F '\t' -v k="$kind" '$1 == k' "$work/inventory.tsv" | grep -c . || true)"
  [[ "$count" -gt 0 ]] ||
    fail I "the fixture wrote at least one $kind" "a gap in the fixture: the baseline's rows hold no $kind, and a census over a kind nobody wrote proves nothing"
  printf 'inventory: %s %s\n' "$kind" "$count"
done < <(inventory_kinds)
# A component is no record of this inventory: only the architecture importer writes one, from an
# architecture repository on GitHub, which no server lib-baseline.sh boots reaches, so the fixture
# seeds none. The line says so beside the count the baseline's rows hold, rather than leaving the
# kind out unnamed; census --store still reads the component censuses (C).
query I "count the baseline's components" "select count(*) from components"
printf 'inventory: component %s (not seeded: only the architecture importer writes a component, from an architecture repository on GitHub, which no server lib-baseline.sh boots reaches)\n' "$answer"
pass I "the inventory holds $(grep -c . "$work/inventory.tsv") records of $(inventory_kinds | grep -c .) migrated kinds, every kind at least one; components are not seeded (the line above says why)"

# --- O: the old answers, on the baseline --------------------------------------------------------

old_read coverage agent "/api/v1/issues/$main/coverage"
old_read results agent "/api/v1/issues/$main/results"
for slug in spec verification-md plan-md; do
  old_read "text-$slug" agent "/api/v1/issues/$main/artifacts/$slug/text?blocks=true"
done
old_read inbox "cookie:$login" /api/v1/inbox
old_read replies "cookie:$login" "/api/v1/me/replies?kind=reply,agent&state=inbox"
old_read agent-messages "cookie:$login" "/api/v1/agents/$agent_session/messages"
for name in coverage results text-spec text-verification-md text-plan-md inbox replies agent-messages; do
  printf 'old answer %s: %s\n' "$name" "$(login_of "$(jq -c '.' "$work/old/$name.json" | head -c 400)")"
done
jq -e '(.requirements | length) > 0' "$work/old/coverage.json" >/dev/null || fail O "the old coverage read answers the requirements" "$(head -c 400 "$work/old/coverage.json")"
pass O "the baseline answered the coverage and results reads, the three documents with their blocks, and the three reads the one inbox replaces"

# --- U: the upgrade ------------------------------------------------------------------------------

baseline_stop_server
wait_zero U "no server process of the database is live" \
  "select count(*) from server_processes where heartbeat_at > now() - interval '15 seconds'" 60
step_begin
baseline_boot head "$head_binary" "$baseline_database_url"
head_pid="$baseline_boot_pid"
base="$baseline_boot_url"
readonly head_log="$baseline_log"
step_end "the upgrade's boot of this head, until it listens"
step_begin
until grep -q 'dispatch: boot backfill done' "$head_log"; do
  ((SECONDS - step_started < 300)) || fail U "this head's boot runs its backfill passes" "no 'dispatch: boot backfill done' within 300 s"
  sleep 1
done
step_end "the wait for the boot's line '$(grep -m 1 -o 'msg="dispatch: boot backfill done"' "$head_log")'"
step_begin
(env -u PGHOSTADDR -u PGSERVICE -u PGSERVICEFILE DATABASE_URL="$database_url" HOME="$work/home" "$head_binary" backfill-graph-store) \
  >"$work/backfill.out" 2>&1 || fail U "envoy-dispatch backfill-graph-store" "it exited $?: $(tail -c 1500 "$work/backfill.out")"
grep -E 'complete=|kind=' "$work/backfill.out" | sed 's/^/backfill: /' || true
step_end "envoy-dispatch backfill-graph-store"
# The census holds the store to the projections only once the block index has drained every
# document the backfill marked owed (document_block_versions.store_owed_at): before that the
# reconciliation has not yet stated the blocks' part_of, and the census reads each such block as a
# difference. A wait on owed documents alone can pass before the backfill marks anything, so it
# runs after both the boot's line and backfill-graph-store.
step_begin
owed_documents() {
  query U "read the documents the block index owes this build's mark" \
    "select coalesce(string_agg(coalesce(a.ref_key, d.artifact_id::text), ' ' order by 1), '') from document_block_versions d
     left join artifacts a on a.id = d.artifact_id where d.store_owed_at is not null"
}
owed_documents
owed_first="$answer"
printf 'owed documents at %s: %d (%s)\n' "$(date -u +%H:%M:%S)" "$(wc -w <<<"$owed_first")" "${owed_first:-none}"
while [[ -n "$answer" ]]; do
  ((SECONDS - step_started < owed_limit)) ||
    fail U "the block index drains every document backfill-graph-store marked owed within $owed_limit s" \
      "after $owed_limit s these still owe it (store_owed_at set): $answer"
  sleep 2
  owed_documents
done
printf 'owed documents at %s: 0\n' "$(date -u +%H:%M:%S)"
step_end "the wait until no document_block_versions row holds store_owed_at (it saw $(wc -w <<<"$owed_first") at its start, 0 at its end)"
pass U "this head's server booted over the baseline's database at $base, ran its boot's passes, envoy-dispatch backfill-graph-store wrote the store's nodes of the release before's rows, and the block index drained the $(wc -w <<<"$owed_first") documents it owed"

# --- S: R21's supertags ------------------------------------------------------------------------

want_supertags=()
for supertag in "${r21_supertags[@]}"; do
  [[ "$supertag" == handoff && "$handoff_held" != 1 ]] || want_supertags+=("$supertag")
done
printf 'R21 supertags (%d): %s\n' "${#r21_supertags[@]}" "$(printf '%s, ' "${r21_supertags[@]}" | sed 's/, $//')"
query S "read the store's supertags" "select coalesce(json_agg(id order by id), '[]') from graph_nodes where kind = 'supertag' and state <> 'gone'"
held_supertags="$answer"
want_json="$(printf '%s\n' "${want_supertags[@]}" | jq -R . | jq -cs 'sort')"
lacked="$(jq -cn --argjson w "$want_json" --argjson h "$held_supertags" '$w - $h')"
[[ "$lacked" == "[]" ]] || fail S "the store holds every supertag of R21 at this head" "it lacks $lacked"
pass S "the store holds each of the $(jq 'length' <<<"$want_json") supertags of R21 this head creates"
extra="$(jq -cn --argjson w "$want_json" --argjson h "$held_supertags" '$h - $w')"
if [[ "$handoff_held" != 1 && "$extra" == *'"handoff"'* ]]; then
  fail S "a head without dispatch://INFRA-1336's merge holds no handoff supertag" "the store holds handoff, present at a head without that merge"
fi
[[ "$extra" == "[]" ]] || fail S "the store holds no supertag R21's list does not name" "it holds $extra"
pass S "the store holds no supertag R21's list does not name: it holds $(jq 'length' <<<"$held_supertags")"
query S "count the nodes of the store" "select count(*) from graph_nodes"
examined="$answer"
query S "list the supertags outside R21's list" \
  "select coalesce(json_object_agg(supertag, n), '{}') from (select supertag, count(*) as n from graph_nodes
     where supertag <> all(string_to_array('$(printf '%s,' "${r21_supertags[@]}" | sed 's/,$//')', ',')) group by supertag) o"
[[ "$examined" -gt 0 && "$answer" == "{}" ]] ||
  fail S "no node of the store carries a supertag outside R21's list" "of $examined nodes, these carry one: $answer"
pass S "no node of the store carries a supertag outside R21's list: examined $examined nodes"
if [[ "$handoff_held" != 1 ]]; then
  want_value S "a head without dispatch://INFRA-1336's merge holds no handoff node" 0 \
    "select count(*) from graph_nodes where kind = 'handoff' or supertag = 'handoff'"
fi

# --- C: each census of the store, both ways -----------------------------------------------------

step_begin
census_code=0
(env -u PGHOSTADDR -u PGSERVICE -u PGSERVICEFILE DATABASE_URL="$database_url" HOME="$work/home" "$head_binary" census --store) \
  >"$work/census.out" 2>&1 || census_code=$?
step_end "envoy-dispatch census --store"
grep -E '^census: (store|project) ' "$work/census.out" | grep -v ' checks ' || true
[[ "$census_code" == 0 ]] || fail C "envoy-dispatch census --store reads zero differences" "it exited $census_code: $(grep -E 'differ|refus|incomplete|error' "$work/census.out" | head -n 20)"
summary="$(grep -E '^census: store rows_checked=' "$work/census.out" | tail -n 1)"
[[ "$summary" =~ rows_checked=([0-9,]+)\ differences=0$ && "${BASH_REMATCH[1]//,/}" -gt 0 ]] ||
  fail C "envoy-dispatch census --store checks rows and reads zero differences" "it printed '$summary'"
pass C "every census of the store against the projection serving its kind reads zero differences both ways: $summary"

# --- V: the inventory against the store and the projections -------------------------------------

# The store's nodes and the tuples it asserts now, each as the inventory names a node or a tuple.
env -u PGHOSTADDR -u PGSERVICE -u PGSERVICEFILE psql "$database_url" -X -q -At -v ON_ERROR_STOP=1 -c "
  select kind || ':' || id from graph_nodes
  union all
  select from_kind || ':' || from_id || ' ' || kind || ' ' || to_kind || ':' || to_id from (
    select distinct on (from_kind, from_id, kind, to_kind, to_id) * from graph_tuples where to_kind <> 'value'
    order by from_kind, from_id, kind, to_kind, to_id, seq desc) t where asserted" 2>"$work/psql" | LC_ALL=C sort -u >"$work/store.txt" ||
  fail V "read the store's nodes and current tuples" "psql: $(<"$work/psql")"
take_inventory "$work/after.tsv" V
lost_any=0
while IFS= read -r kind; do
  awk -F '\t' -v k="$kind" '$1 == k { print $2 }' "$work/inventory.tsv" | LC_ALL=C sort -u >"$work/kind.txt"
  count="$(grep -c . "$work/kind.txt" || true)"
  store_lost="$(LC_ALL=C comm -23 "$work/kind.txt" "$work/store.txt")"
  projection_lost="$(awk -F '\t' -v k="$kind" '$1 == k { print $2 }' "$work/after.tsv" | LC_ALL=C sort -u | LC_ALL=C comm -23 "$work/kind.txt" -)"
  if [[ -z "$store_lost" && -z "$projection_lost" ]]; then
    printf 'census: inventory kind=%s records=%s lost_from_store=0 lost_from_projection=0\n' "$kind" "$count"
    continue
  fi
  lost_any=1
  printf 'census: inventory kind=%s records=%s lost_from_store=%s lost_from_projection=%s\n' "$kind" "$count" \
    "$(grep -c . <<<"$store_lost" || true)" "$(grep -c . <<<"$projection_lost" || true)"
  while IFS= read -r record; do
    [[ -z "$record" ]] || printf 'lost from the store: %s %s\n' "$kind" "$(login_of "$record")"
  done <<<"$store_lost"
  while IFS= read -r record; do
    [[ -z "$record" ]] || printf 'lost from the projection: %s %s\n' "$kind" "$(login_of "$record")"
  done <<<"$projection_lost"
done < <(inventory_kinds)
[[ "$lost_any" == 0 ]] ||
  fail V "the store and the projections hold every record of the inventory the release before wrote" "the upgrade lost the records printed above"
pass V "the store and the projections hold every one of the $(grep -c . "$work/inventory.tsv") records of the inventory"

# A mention, a delivery and a read mark are tuples, never nodes: the same read counts the tuples of
# each and the nodes of none.
query V "count the mentions, deliveries and read marks held as nodes and as tuples" \
  "select json_build_object(
     'nodes', (select count(*) from graph_nodes where kind in ('mention', 'mentions', 'delivery', 'delivered_to', 'read', 'read_mark', 'mark')
                 or supertag in ('mention', 'delivery', 'read', 'read_mark')),
     'mention_tuples', (select count(*) from graph_tuples where kind = 'mentions' and asserted),
     'delivery_tuples', (select count(*) from graph_tuples where kind = 'delivered_to' and asserted))"
jq -e '.nodes == 0 and .mention_tuples > 0' <<<"$answer" >/dev/null ||
  fail V "a mention, a delivery and a read mark are held as tuples and none as a node" "the store counts $answer"
pass V "a mention, a delivery and a read mark are held as tuples and none as a node: $answer"
# One attempt to reach a target is a delivery_attempt node, whose state and envelope it holds.
query V "read the delivery_attempt nodes of the fixture's message to $agent_session" \
  "select coalesce(json_agg(json_build_object('id', n.id, 'values', (select coalesce(json_agg(distinct t.kind), '[]')
     from graph_tuples t where t.from_kind = n.kind and t.from_id = n.id and t.to_kind = 'value' and t.asserted))), '[]')
   from graph_nodes n join message_deliveries d on n.id = 'message/' || d.message_id::text || '/' || d.attempt where n.kind = 'delivery_attempt'"
jq -e 'length > 0 and all(.[]; (.values | index("state")) != null and (.values | index("envelope")) != null)' <<<"$answer" >/dev/null ||
  fail V "each delivery_attempt node holds its attempt's state and envelope" "the store holds $answer"
pass V "each delivery_attempt node holds its attempt's state and envelope: $answer"
# A value's history is its appended tuples, and an edge whose target block was deleted is marked
# removed: the spec of the main issue is written twice at this head, r-a's words changed each time
# and r-b, which k-a serves, deleted by the second write.
send GET "/api/v1/issues/$main/artifacts/spec"
want_status 200 V "read the spec document of $main"
field V "the name of the spec of $main" .name
readonly spec_name="$value"
send GET "/api/v1/issues/$main/artifacts/spec/text"
want_status 200 V "read the spec of $main"
jq -r '.markdown' "$work/body" |
  sed 's/^Every kind the release moves is held in the store\.$/Every kind the release moves is held in the store, first edit./' >"$work/spec.first.md"
sed 's/, first edit\.$/, second edit./' "$work/spec.first.md" |
  awk 'BEGIN { skip = 0 } /^:::requirement\{#r-b\}/ { skip = 1; next } skip && /^:::$/ { skip = 0; next } !skip' >"$work/spec.second.md"
for edit in first second; do
  upload V "$main" "$spec_name" "$(<"$work/spec.$edit.md")"
  settled V
done
with_vars V "read the text values of r-a of $main, oldest first" \
  "select coalesce(json_agg(to_id order by seq), '[]') from graph_tuples
   where from_kind = 'block' and from_id like :'key' || '/%#r-a' and kind = 'text' and to_kind = 'value'" "key=$main"
jq -e 'length >= 2 and (.[-2] | contains("first edit")) and (.[-1] | contains("second edit"))' <<<"$answer" >/dev/null ||
  fail V "the history of the words of r-a is its appended text values, in order" "the store holds $answer"
pass V "the history of the words of r-a is its appended text values, in order: $(jq 'length' <<<"$answer") values, the last two the two edits"
# A retracted relation is ended, not deleted: the child's child_of, retracted by a move to no parent.
child="${key_of[child]}"
send PATCH "/api/v1/issues/$child" "$(jq -cn --argjson a "$(actor)" '{parent: null, actor: $a}')"
want_status 200 V "take $child out from under $main"
with_vars V "read the child_of tuples of $child, oldest first" \
  "select coalesce(json_agg(json_build_object('to', to_id, 'asserted', asserted) order by seq), '[]') from graph_tuples
   where from_kind = 'issue' and from_id = :'key' and kind = 'child_of'" "key=$child"
jq -e --arg m "$main" '.[-1] == {to: $m, asserted: false} and any(.[]; . == {to: $m, asserted: true})' <<<"$answer" >/dev/null ||
  fail V "the retracted child_of of $child is ended, its assertion kept" "the store holds $answer"
pass V "the retracted child_of of $child is ended, its assertion kept: $answer"
query V "count the edges whose target block was deleted, marked removed" \
  "select count(*) from graph_tuples where kind = 'removed_by' and asserted"
[[ "$answer" -gt 0 ]] || fail V "an edge whose target block was deleted is marked removed" "the store marks $answer edges removed"
pass V "an edge whose target block was deleted is marked removed: $answer such edges"

# --- R: the moved reads, old answer against new -------------------------------------------------

started=$SECONDS
in_flight=("${key_of[upstream]}" "$main" "$child")
while true; do
  unconverted=()
  for issue in "${in_flight[@]}"; do
    send GET "/api/v1/issues/$issue/tree"
    if [[ "$status" != 200 ]] || ! jq -e '.converted == true' "$work/body" >/dev/null; then
      unconverted+=("$issue (HTTP $status, converted $(jq -c '.converted // null' "$work/body" 2>/dev/null || printf '?'))")
    fi
  done
  [[ "${#unconverted[@]}" -gt 0 ]] || break
  printf 'conversion: %d s, unconverted: %s\n' "$((SECONDS - started))" "${unconverted[*]}"
  ((SECONDS - started < convert_limit)) ||
    fail R "this head reports each in-flight issue the baseline wrote converted (GET /api/v1/issues/{key}/tree answers converted: true)" \
      "after $convert_limit s these stayed unconverted: ${unconverted[*]}"
  sleep 2
done
pass R "this head reports each in-flight issue the baseline wrote converted: ${in_flight[*]}"
# The coverage read: each requirement the release before answered, field for field, and the gaps.
send GET "/api/v1/issues/$main/coverage"
want_status 200 R "read the coverage of $main at this head"
got="$(jq -cS --slurpfile o "$work/old/coverage.json" '[$o[0].requirements[] as $r | (.requirements[] | select(.block_id == $r.block_id)) as $n
  | $r | keys | map(select(. as $k | $r[$k] != $n[$k])) | select(length > 0) | {block_id: $r.block_id, fields: .}]' "$work/body")"
[[ "$got" == "[]" && "$(jq -c '.gaps | sort' "$work/body")" == "$(jq -c '.gaps | sort' "$work/old/coverage.json")" ]] ||
  fail R "the coverage read answers what the release before answered" "old $(jq -c . "$work/old/coverage.json"); new $(jq -c . "$work/body"); differing $got"
pass R "the coverage read of $main answers every requirement and gap the release before answered, field for field"
send GET "/api/v1/issues/$main/results"
want_status 200 R "read the results of $main at this head"
readonly check_shape='[.checks[] | {id: (.block_id // .id), when, surface, run, negative_control, results: [.results[] | {verdict, commit}]}] | sort_by(.id)'
[[ "$(jq -cS "$check_shape" "$work/body")" == "$(jq -cS "$check_shape" "$work/old/results.json")" ]] ||
  fail R "the results read answers what the release before answered" "old $(jq -cS "$check_shape" "$work/old/results.json"); new $(jq -cS "$check_shape" "$work/body")"
pass R "the results read of $main answers every check and result the release before answered"
# The Spec, Verification and Plan queries, block by block over the typed blocks each document held.
for pair in "spec:spec:callout,requirement" "verification:verification-md:check" "plan:plan-md:plan_task"; do
  view="${pair%%:*}" rest="${pair#*:}"
  slug="${rest%%:*}" types="${rest#*:}"
  send GET "/api/v1/issues/$main/tree?view=$view"
  want_status 200 R "read the $view query of $main"
  old_ids="$(jq -c --arg t "$types" '[.blocks[] | select(.type as $x | $t | split(",") | index($x)) | .id] | sort' "$work/old/text-$slug.json")"
  new_ids="$(jq -c --arg t "$types" '[.nodes[] | select(.supertag as $x | $t | split(",") | index($x)) | .block_id] | sort' "$work/body")"
  [[ "$old_ids" != "[]" && "$old_ids" == "$new_ids" ]] ||
    fail R "the $view query answers the typed blocks $slug held before the move" "before: $old_ids; now: $new_ids"
  pass R "the $view query answers the typed blocks $slug held before the move: $new_ids"
done
send GET "/api/v1/issues/$main/tree"
want_status 200 R "read the tree of $main"
typed="$(jq -c '[.nodes[] | select(.supertag == "requirement" or .supertag == "check" or .supertag == "plan_task" or .supertag == "callout") | .block_id] | group_by(.) | map({(.[0]): length}) | add // {}' "$work/body")"
prose="$(jq -c --slurpfile s "$work/old/text-spec.json" --slurpfile v "$work/old/text-verification-md.json" --slurpfile p "$work/old/text-plan-md.json" \
  '[($s[0].blocks + $v[0].blocks + $p[0].blocks)[] | select(.type == "paragraph") | .id] as $prose | [.nodes[] | select(.block_id as $b | $prose | index($b)) | .block_id]' "$work/body")"
if ! jq -e 'length > 0 and all(.[]; . == 1)' <<<"$typed" >/dev/null || [[ "$prose" != "[]" ]]; then
  fail R "the tree read answers each typed block once and no prose block" "typed blocks $typed; prose blocks it answers $prose"
fi
pass R "the tree read of $main answers each typed block once, $typed, and no prose block"
# The one inbox answers every row the three reads it replaces answered, and not the reverse.
sign_in R "$login" " at this head"
as_person GET /api/v1/me/inbox
want_status 200 R "read $login's one inbox, GET /api/v1/me/inbox"
missing_rows="$(jq -c --slurpfile i "$work/old/inbox.json" --slurpfile r "$work/old/replies.json" --slurpfile m "$work/old/agent-messages.json" \
  '[.. | strings] as $now | [($i[0] | if type == "array" then .[] else empty end | .id), ($r[0].rows[]? | .thread_id // .id),
     ($m[0] | if type == "array" then .[] else (.messages // [])[] end | .message.id // .id)]
   | map(select(. != null)) | map(select(. as $id | $now | index($id) | not))' "$work/body")"
[[ "$missing_rows" == "[]" ]] ||
  fail R "the one inbox answers every row the three reads it replaces answered" "it lacks $missing_rows"
pass R "the one inbox answers every row GET /api/v1/inbox, GET /api/v1/me/replies and GET /api/v1/agents/{session_id}/messages answered"

# --- K: each child's comparison check passed at its merged head --------------------------------

for entry in "${compared_children[@]}"; do
  child_key="${entry%% *}" check="${entry#* }"
  gh pr list --head "legion/$child_key" --state merged --limit 100 --json number,headRefOid,mergedAt >"$work/pr.json" 2>"$work/gh.err" ||
    fail K "read the merged pull requests of legion/$child_key through gh" "gh exited non-zero: $(<"$work/gh.err")"
  head="$(jq -r 'sort_by(.mergedAt) | last | .headRefOid // ""' "$work/pr.json")"
  [[ -n "$head" ]] ||
    fail K "dispatch://$child_key's $check passed at the head its pull request merged at" \
      "legion/$child_key has no merged pull request, so no head of it merged: $(jq -c '[.[] | {number, mergedAt}]' "$work/pr.json")"
  (
    set +x
    cd -- "$work"
    # shellcheck disable=SC2031 # this shell's own PATH and TMPDIR, which no subshell here changed
    env -i PATH="$PATH" HOME="$work/home" DISPATCH_STATE_DIR="$work/state" TMPDIR="$TMPDIR" DISPATCH_HOST=omp \
      DISPATCH_URL="$deployed_url" DISPATCH_TOKEN_FILE="$deployed_token_file" \
      bun "$cli_entry" results --issue "$child_key" --at "$head" --brief
  ) >"$work/results.txt" 2>&1 || fail K "read the results of dispatch://$child_key at $head" "dispatch results exited non-zero: $(head -c 600 "$work/results.txt")"
  line="$(grep -m 1 -E "^- $check \(" "$work/results.txt" || true)"
  [[ -n "$line" ]] ||
    fail K "dispatch://$child_key holds the check $check, passed at $head" \
      "dispatch results --issue $child_key --at $head names no check $check; it read: $(grep -E '^- ' "$work/results.txt" | cut -d. -f1 | tr '\n' ' ')"
  [[ "$line" == *"A pass stands: yes"* ]] ||
    fail K "dispatch://$child_key's $check passed at $head, the head its pull request merged at, and stands current" "it read: $line"
  pass K "dispatch://$child_key's $check passed at $head, the head its pull request merged at: $line"
done

pass "$driver" complete
