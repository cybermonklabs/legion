#!/usr/bin/env bash
# The check c-order of the issue tree's Verification document: the order the work lands in, and
# the order the conversion writes one issue's three documents in.
#
# It empties the database DATABASE_URL names (a database on this machine whose name ends
# `_upgrade`, which it drops and creates again), boots the release BASELINE_SHA names on it through
# lib-baseline.sh, signs DEV_SIGNIN_LOGIN in (default alice), creates PROJECT, and writes through
# that release's API an issue W and an issue O in flight that W blocks: O holds a spec of two
# requirements, a Verification document of two checks that serve them and a plan of one task that
# serves the first, each holding the shapes seed-corpus.sh writes. Before the upgrade it captures
# O's three documents as their text reads answer them, the markdown of the spec's newest version as
# its version read answers it, and O's issue read. It stops that server and boots this head's
# (the stamped `envoy-dispatch` on PATH) over the same rows, with the hold settings lib-tree.sh
# gives every upgrade boot, and waits for the backfill as the Verification preamble states it.
#
# Before the first node moves it reads what the fifth step of R21 lands: an issue H this head
# creates, with its three documents, holds the four query nodes (spec, verification, plan and
# decisions) and O holds none yet; a person's approval of H's spec is taken, and H's newest spec
# version answers a digest; version_renders holds a row with its time and its digest for that
# version; and the render boundary answers, the spec version the earlier release wrote reading
# `Dispatch-Render-Source: stored` with the bytes captured before the upgrade, and H's reading
# `tree`.
#
# It then waits for this head's build announcement and runs `envoy-dispatch convert-issues
# --project PROJECT`. It reads O converted (`converted: true` on GET /api/v1/issues/{key}/tree),
# its three document_rooms rows naming O's tree and written by one transaction (one xmin), and in
# that transaction the seq of the first part_of tuple written for the nodes of each document,
# which must rise in the order verification-md, plan-md, spec; otherwise it exits non-zero printing
# the three seqs in the order it read them. It reads the store census (`envoy-dispatch census
# --store`) at zero differences after that transaction, and finds no issue of the database with
# some of its three documents converted and some not.
#
# It reads the block schema this head serves (GET /api/v1/schema/blocks) one version past the one
# BASELINE_SHA's pmdoc/schema/blocks.json gives, carrying the criterion type, the requirement's
# serves attribute, plan_task's size attribute, the widened needs scope and the items field, no
# typed block attribute taking a node state value, and it finds one commit alone of
# BASELINE_SHA..HEAD moving that file's version. It reads `git log --reverse --format=%H
# origin/main..HEAD` and, with `git log -S` for each of requirementBlocksIn and checkBlocksIn, finds
# the commit that deletes the name later in that list than the commit that adds the route GET
# /api/v1/issues/{key}/tree, to routes_table.go or to its route file. It reads the routes declared
# at BASELINE_SHA and in this checkout, each a row of routes_table.go or, since main's #5409 gave
# each route a file of its own, a route file route_<path>+<method>.go, and finds every route of the
# first in the second, GET /api/v1/issues/{key}/decisions among them; a route missing at this head
# fails, naming it.
# It searches Go source only: the Verification fixture quotes the names but implements neither lookup.
#
# Last it reads O's one dependency through GET /api/v1/issues/{key}, GET /api/v1/issues (the
# summary read: its rows are model.IssueSummary) and GET /api/v1/issues/{key}/events, blocked_by
# beside needs in
# each, both naming W; a PATCH naming needs and blocked_by with the same targets is served from
# them and one whose two lists differ is refused 400 naming both; and every field O's issue read
# answered before the upgrade still answers, not null, once O has converted.
#
# Inputs: DATABASE_URL, PROJECT, BASELINE_SHA (or BASELINE_BINARY, with BASELINE_SHA still naming
# its commit, which the schema and route reads take), DISPATCH_TOKEN (the agent bearer every server
# here accepts), DEV_SIGNIN_LOGIN (optional, default alice), and envoy-dispatch on PATH. Each
# passing check prints `PASS: <section> <what>`; the first failing one prints
# `FAIL: <section> <what>: <observed>` and the script exits 1; a missing input exits 2. It prints
# no SKIP line, and its last line is `PASS: issue-tree-order.sh complete <N> sections`.
#
# Every check is `cmd || fail …`, never part of an `&&` list: under `set -e` a failing command
# anywhere but last in such a list does not stop the script.

# shellcheck disable=SC2016 # the single-quoted jq filters name jq variables, not shell ones
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DATABASE_URL=postgres://<user>@127.0.0.1:<port>/<name>_upgrade PROJECT=CORE \
         DISPATCH_TOKEN=e2e-token BASELINE_SHA=<commit> [DEV_SIGNIN_LOGIN=alice] \
         bash e2e/acceptance/issue-tree-order.sh
       (run from packages/dispatch, with the stamped envoy-dispatch on PATH; DATABASE_URL names a
       database on this machine whose name ends _upgrade, which this run drops and creates again)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
# shellcheck source-path=SCRIPTDIR source=lib-baseline.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib-baseline.sh"
# shellcheck source-path=SCRIPTDIR source=lib-tree.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib-tree.sh"
# shellcheck source-path=SCRIPTDIR source=seed-corpus.sh
. "$(dirname "${BASH_SOURCE[0]}")/seed-corpus.sh"

require_env DATABASE_URL PROJECT BASELINE_SHA
require_commands cmp comm curl diff git head jq paste psql sort tail tr wc
readonly driver=issue-tree-order.sh
# shellcheck disable=SC2153 # PROJECT is the caller's input, checked by name above
project="$PROJECT"
readonly project
# Not readonly: lib-baseline.sh's baseline_boot takes a local of that name.
# shellcheck disable=SC2031 # the run's own DATABASE_URL, which no server's subshell changes
database_url="$DATABASE_URL"
readonly person_login="${DEV_SIGNIN_LOGIN:-alice}"
checkout="${LEGION_WORKSPACE:-$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../../.." && pwd)}"
readonly checkout
readonly schema_path=packages/envoy/internal/dispatch/pmdoc/schema/blocks.json
readonly api_path=packages/envoy/internal/dispatch/api
readonly routes_path="$api_path/routes_table.go"
# The route file of GET /api/v1/issues/{key}/tree (internal/dispatch/routefile's name for it).
readonly tree_route_path="$api_path/route_api_v1_issues_{key}_tree+get.go"
# The document lookups only this build runs, which a commit of this branch deletes once the four
# queries' census reads zero (R21's sixth step).
readonly -a deleted_lookups=(requirementBlocksIn checkBlocksIn)
readonly tree_route='GET /api/v1/issues/{key}/tree' decisions_route='GET /api/v1/issues/{key}/decisions'
base_commit="$(git -C "$checkout" rev-parse --verify --quiet "${BASELINE_SHA}^{commit}")" || {
  printf 'BASELINE_SHA %s names no commit of %s\n' "$BASELINE_SHA" "$checkout" >&2
  exit 2
}
readonly base_commit

work_dir
mkdir "$work/capture"
sections=0
cleanup() {
  local code=$?
  tree_stop_head
  baseline_stop
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# issue_tree_order_version SECTION ARTIFACT NUMBER: GET /api/v1/artifacts/{id}/versions/{number}
# as the agent, its body in $work/body, and the Dispatch-Render-Source it answered in
# render_source.
issue_tree_order_version() {
  tree_last_request="GET /api/v1/artifacts/$2/versions/$3"
  status="$(curl -sS -o "$work/body" -D "$work/headers" -w '%{http_code}' -H "@$work/auth" \
    "$base/api/v1/artifacts/$2/versions/$3" 2>"$work/curl")" || status="curl failed: $(<"$work/curl")"
  tree_want 200 "$1" "read version $3 of $2"
  render_source="$(tr -d '\r' <"$work/headers" | grep -i '^dispatch-render-source:' | tail -n 1)" || render_source=""
  render_source="${render_source#*: }"
}

# issue_tree_order_route_lines [REVISION]: the lines that declare routes at REVISION, or in this
# checkout's working tree, untracked files too, where none is named: routes_table.go's and every
# route file's (route_<path>+<method>.go), each file's lines in its own order. A search that finds
# no line prints nothing; one that fails returns non-zero.
issue_tree_order_route_lines() {
  local status=0 where=(--untracked)
  (($# == 0)) || where=("$1")
  git -C "$checkout" grep -h -F -e '{http.Method' -e 'Method:' -e 'Pattern:' "${where[@]}" -- \
    "$routes_path" "$api_path/route_*+*.go" || status=$?
  ((status <= 1))
}

# issue_tree_order_routes: the routes the lines on stdin declare, one `METHOD path` a line, sorted
# as text: a row of routes_table.go, `{http.MethodX, "path", …}`, as a release before main's #5409
# writes each route, and a route file's `Method: http.MethodX,` line with the `Pattern: "path",`
# line after it, as each route is written since.
issue_tree_order_routes() {
  local line method="" path
  while IFS= read -r line; do
    if [[ "$line" =~ \{http\.Method([A-Za-z]+),\ \"([^\"]+)\" ]]; then
      method="${BASH_REMATCH[1]}" path="${BASH_REMATCH[2]}"
    elif [[ "$line" =~ ^[[:space:]]*Method:[[:space:]]*http\.Method([A-Za-z]+), ]]; then
      method="${BASH_REMATCH[1]}"
      continue
    elif [[ -n "$method" && "$line" =~ ^[[:space:]]*Pattern:[[:space:]]*\"([^\"]+)\" ]]; then
      path="${BASH_REMATCH[1]}"
    else
      continue
    fi
    printf '%s %s\n' "$(printf '%s' "$method" | tr '[:lower:]' '[:upper:]')" "$path"
    method=""
  done | LC_ALL=C sort -u
}

# issue_tree_order_schema_version REVISION: the version pmdoc/schema/blocks.json gives at
# REVISION, in schema_version; empty where the file is absent there.
issue_tree_order_schema_version() {
  schema_version="$(git -C "$checkout" show "$1:$schema_path" 2>/dev/null | jq -r '.version // empty' 2>/dev/null)" ||
    schema_version=""
}

# --- setup: the earlier release over an emptied database --------------------------------------

section=setup
tree_reset_database "$database_url" '?*_upgrade' "$section"
baseline_start
token="$baseline_token"
unset DISPATCH_TOKEN DISPATCH_TOKEN_FILE
session="issue-tree-order-$(date +%s)-$$"
readonly session
base="$baseline_url"
tree_init
tree_sign_in "$section" "$person_login"
tree_create_project "$section" "$project" "The order of the issue tree's conversion"
sections=$((sections + 1))

# --- base: the issue the conversion orders, written by the earlier release --------------------

section=base
printf 'The issue the order check waits on.\n\n:::requirement{#r-w}\nThe blocker holds one requirement.\n:::\n' >"$work/w-spec.md"
corpus_issue "$section" "The blocker of the order check" "$work/w-spec.md"
blocker="$corpus_key"
readonly blocker
tree_key "$section" "the blocker's key" "$blocker"
{
  printf 'The issue whose three documents the conversion orders.\n\n'
  printf ':::requirement{#r1}\nThe documents convert in one transaction.\n:::\n\n'
  printf ':::requirement{#r2}\nThe spec converts last.\n:::\n\n'
  corpus_shapes spec order-spec
} >"$work/o-spec.md"
corpus_issue "$section" "The issue the conversion orders" "$work/o-spec.md" "" "$blocker"
ordered="$corpus_key"
ordered_spec="$corpus_spec"
readonly ordered ordered_spec
tree_key "$section" "the ordered issue's key" "$ordered"
tree_uuid "$section" "the ordered issue's spec" "$ordered_spec"
{
  printf 'How the order is proved.\n\n'
  printf ':::check{#c1 serves="dispatch://%s/spec#b-r1" surface="cli" run="bash e2e/acceptance/issue-tree-order.sh" negative_control="Convert the spec first and the seqs read out of order."}\nThe three documents convert in one transaction.\n:::\n\n' "$ordered"
  printf ':::check{#c2 serves="dispatch://%s/spec#b-r2" surface="cli" run="bash e2e/acceptance/issue-tree-order.sh" negative_control="Convert the spec first and its seq is the lowest."}\nThe spec converts last.\n:::\n\n' "$ordered"
  corpus_shapes verification.md order-verification
} >"$work/o-verification.md"
corpus_upload "$section" "$ordered" verification.md "$work/o-verification.md"
ordered_verification="$corpus_doc"
{
  printf 'How the order is built.\n\n'
  printf ':::plan_task{#t1 serves="dispatch://%s/spec#b-r1"}\nConvert the three documents in one transaction.\n:::\n\n' "$ordered"
  corpus_shapes plan.md order-plan
} >"$work/o-plan.md"
corpus_upload "$section" "$ordered" plan.md "$work/o-plan.md"
ordered_plan="$corpus_doc"
readonly ordered_verification ordered_plan
tree_uuid "$section" "the ordered issue's verification-md" "$ordered_verification"
tree_uuid "$section" "the ordered issue's plan-md" "$ordered_plan"
corpus_wait_settled "$section"
# The capture: the three documents as their text reads answer them, the spec's newest version as
# its version read answers it, and the issue read.
for slug in spec verification-md plan-md; do
  tree_send GET "/api/v1/issues/$ordered/artifacts/$slug/text"
  tree_want 200 "$section" "read $ordered's $slug before the upgrade"
  jq -j '.markdown' "$work/body" >"$work/capture/$slug.md" ||
    fail "$section" "capture $ordered's $slug" "no markdown in: $(body)"
done
corpus_version "$section" "$ordered_spec"
ordered_spec_version="$tree_value"
readonly ordered_spec_version
issue_tree_order_version "$section" "$ordered_spec" "$ordered_spec_version"
jq -j '.markdown' "$work/body" >"$work/capture/spec-version.md" ||
  fail "$section" "capture version $ordered_spec_version of $ordered's spec" "no markdown in: $(body)"
tree_send GET "/api/v1/issues/$ordered"
tree_want 200 "$section" "read $ordered before the upgrade"
cp "$work/body" "$work/capture/issue.json"
pass "$section" "the earlier release wrote $ordered, blocked by $blocker, with its spec, verification-md and plan-md, and the run captured them"
baseline_stop_server
sections=$((sections + 1))

# --- upgrade: this head over the earlier release's rows ----------------------------------------

section=upgrade
tree_boot_head head
base="$tree_head_url"
tree_wait_backfill "$section"
sections=$((sections + 1))

# --- boundary: the fifth step of R21 answers before the first node moves ----------------------

section=boundary
printf 'The issue this head creates before any node moves.\n\n:::requirement{#r-h}\nThe query nodes stand before the move.\n:::\n' >"$work/h-spec.md"
corpus_issue "$section" "An issue this head creates before the conversion" "$work/h-spec.md"
fresh="$corpus_key"
fresh_spec="$corpus_spec"
readonly fresh fresh_spec
tree_key "$section" "the fresh issue's key" "$fresh"
printf ':::check{#c-h serves="dispatch://%s/spec#b-r-h" surface="cli" run="bash e2e/acceptance/issue-tree-order.sh" negative_control="Leave the query nodes out and they are not there."}\nThe query nodes stand.\n:::\n' "$fresh" >"$work/h-verification.md"
corpus_upload "$section" "$fresh" verification.md "$work/h-verification.md"
printf ':::plan_task{#t-h serves="dispatch://%s/spec#b-r-h"}\nWrite the query nodes.\n:::\n' "$fresh" >"$work/h-plan.md"
corpus_upload "$section" "$fresh" plan.md "$work/h-plan.md"
tree_wait_backfill "$section"
want_value "$section" "$fresh, which this head created, holds the spec, verification, plan and decisions query nodes before the first node moves" \
  decisions,plan,spec,verification "
    select coalesce(string_agg(n.attributes->>'query', ',' order by n.attributes->>'query'), '')
    from graph_nodes n
    where n.kind = 'artifact' and n.supertag = 'query' and split_part(n.id, '/', 1) = '$fresh'"
want_value "$section" "$ordered, which has not converted, holds no query node before the first node moves" 0 "
    select count(*) from graph_nodes n
    where n.kind = 'artifact' and n.supertag = 'query' and split_part(n.id, '/', 1) = '$ordered'"
corpus_review "$section" "$fresh_spec" approved
approved_version="${corpus_approved[$fresh_spec]}"
deadline=$((SECONDS + tree_wait_seconds))
while true; do
  tree_send GET "/api/v1/issues/$fresh/artifacts/spec"
  tree_want 200 "$section" "read $fresh's spec and its versions"
  digest="$(jq -r --argjson n "$approved_version" '[.versions[] | select(.number == $n) | .digest // ""][0] // ""' "$work/body")"
  [[ ! "$digest" =~ ^[0-9a-f]{64}$ ]] || break
  ((SECONDS < deadline)) ||
    fail "$section" "the approved version of $fresh's spec answers the digest of its query's record" \
      "after ${tree_wait_seconds} s version $approved_version answers digest '$digest'"
  sleep "$tree_poll_seconds"
done
pass "$section" "a person's approval of $fresh's spec at version $approved_version is taken, and that version answers the digest $digest"
want_value "$section" "version_renders holds the time and the digest of version $approved_version of $fresh's spec" "true|$digest" "
    select (r.at is not null)::text || '|' || coalesce(r.record_sha256, '')
    from version_renders r where r.artifact_id = '$fresh_spec' and r.number = $approved_version"
issue_tree_order_version "$section" "$ordered_spec" "$ordered_spec_version"
[[ "$render_source" == stored ]] ||
  fail "$section" "the render boundary serves the earlier release's version of $ordered's spec from its stored bytes" \
    "version $ordered_spec_version answers Dispatch-Render-Source '$render_source'"
jq -j '.markdown' "$work/body" >"$work/spec-version.after.md"
cmp -s "$work/capture/spec-version.md" "$work/spec-version.after.md" ||
  fail "$section" "the render boundary answers the bytes the earlier release stored for version $ordered_spec_version of $ordered's spec" \
    "they differ from the capture: $(diff "$work/capture/spec-version.md" "$work/spec-version.after.md" | head -n 20)"
pass "$section" "version $ordered_spec_version of $ordered's spec, written by the earlier release, answers its stored bytes (Dispatch-Render-Source: stored)"
issue_tree_order_version "$section" "$fresh_spec" "$approved_version"
[[ "$render_source" == tree ]] ||
  fail "$section" "the render boundary renders a version this head wrote from the tree" \
    "version $approved_version of $fresh's spec answers Dispatch-Render-Source '$render_source'"
pass "$section" "version $approved_version of $fresh's spec, written by this head, renders from the tree (Dispatch-Render-Source: tree)"
sections=$((sections + 1))

# --- order: the three documents convert in one transaction, spec last -------------------------

section=order
tree_read agent "$ordered"
tree_want 200 "$section" "read $ordered's tree before the conversion"
expect "$section" "$ordered reads converted: false before the conversion" false '.converted'
tree_wait_announcement "$section"
query "$section" "read the store's last seq before the conversion" "select coalesce(max(seq), 0) from graph_tuples"
seq_before="$answer"
readonly seq_before
tree_convert "$section"
printf '%s\n' "$(<"$work/out")"
tree_outcome "$ordered"
[[ "$outcome" == converted ]] ||
  fail "$section" "convert-issues prints $ordered converted" "it prints $outcome for it: $(tail -n 20 "$work/out")"
tree_read agent "$ordered"
tree_want 200 "$section" "read $ordered's tree after the conversion"
expect "$section" "$ordered reads converted: true after the conversion" true '.converted'
tree_field "$section" "$ordered's tree artifact" '.tree'
ordered_tree="$tree_value"
readonly ordered_tree
tree_uuid "$section" "$ordered's tree artifact" "$ordered_tree"
want_value "$section" "each of $ordered's spec, verification-md and plan-md holds a document_rooms row naming its tree $ordered_tree" 3 "
    select count(*) from document_rooms
    where artifact_id in ('$ordered_spec', '$ordered_verification', '$ordered_plan') and room = '$ordered_tree'"
query "$section" "read the transaction that wrote $ordered's document_rooms rows" "
    select count(distinct xmin::text) || '|' || min(xmin::text) from document_rooms
    where artifact_id in ('$ordered_spec', '$ordered_verification', '$ordered_plan')"
[[ "${answer%%|*}" == 1 ]] ||
  fail "$section" "one transaction converted $ordered's three documents" "their document_rooms rows were written by ${answer%%|*} transactions"
pass "$section" "one transaction (${answer#*|}) wrote $ordered's three document_rooms rows"
query "$section" "read $ordered's tree's address" "select ref_key from artifacts where id = '$ordered_tree'"
tree_ref="$answer"
readonly tree_ref
tree_first_part_of "$section" "$ordered_tree" "$tree_ref" "$seq_before" "$ordered_verification" verification-md
verification_seq="$first_seq"
tree_first_part_of "$section" "$ordered_tree" "$tree_ref" "$seq_before" "$ordered_plan" plan-md
plan_seq="$first_seq"
tree_first_part_of "$section" "$ordered_tree" "$tree_ref" "$seq_before" "$ordered_spec" spec
spec_seq="$first_seq"
readonly verification_seq plan_seq spec_seq
read_seqs="verification-md=$verification_seq plan-md=$plan_seq spec=$spec_seq"
[[ "$verification_seq" =~ ^[0-9]+$ && "$plan_seq" =~ ^[0-9]+$ && "$spec_seq" =~ ^[0-9]+$ ]] ||
  fail "$section" "the conversion wrote a part_of for the nodes of each of $ordered's three documents" "the seqs read: $read_seqs"
((verification_seq < plan_seq && plan_seq < spec_seq)) ||
  fail "$section" "the first part_of of each document's nodes rises in the order verification-md, plan-md, spec" \
    "the seqs read, in that order: $read_seqs"
pass "$section" "the first part_of of each document's nodes rises in the order verification-md, plan-md, spec: $read_seqs"
tree_store_census "$section"
summary="$(grep -E '^census: store rows_checked=' "$work/census.out" || true)"
[[ -n "$summary" ]] || fail "$section" "the store census prints its summary line" "$(tail -n 20 "$work/census.out")"
printf '%s\n' "$summary"
[[ "$summary" =~ differences=0([^0-9,]|$) ]] ||
  fail "$section" "the store census reads zero differences after the conversion's transaction" \
    "$summary; $(grep -E 'difference at' "$work/census.out" | head -n 20)"
pass "$section" "the store census reads zero differences after $ordered's conversion"
query "$section" "read the issues left with some of their three documents converted and some not" "
    select coalesce(string_agg(issue_key || ' (' || joined || ' of ' || held || ')', ', ' order by issue_key), '')
    from (
      select a.issue_key,
             count(*) filter (where exists (select 1 from document_rooms d where d.artifact_id = a.id)) as joined,
             count(*) as held
      from artifacts a
      where a.kind = 'doc' and a.retired_at is null and (a.is_primary or a.slug in ('verification-md', 'plan-md'))
      group by a.issue_key
    ) split
    where joined > 0 and joined < held"
[[ -z "$answer" ]] || fail "$section" "no issue is left with some documents converted and some not" "these are: $answer"
pass "$section" "no issue of the database holds some of its three documents converted and some not"
sections=$((sections + 1))

# --- schema: one block schema bump, the one after the earlier release's ------------------------

section=schema
issue_tree_order_schema_version "$base_commit"
base_schema="$schema_version"
[[ "$base_schema" =~ ^[0-9]+$ ]] ||
  fail "$section" "read the block schema version at BASELINE_SHA" "$schema_path at $base_commit gives '$base_schema'"
readonly next_schema=$((base_schema + 1))
tree_send GET /api/v1/schema/blocks
tree_want 200 "$section" "read the block schema this head serves"
expect "$section" "this head serves block schema version $next_schema, the one after BASELINE_SHA's $base_schema" "$next_schema" '.version'
expect "$section" "version $next_schema carries the criterion type, the requirement's serves, plan_task's size, the widened needs scope and the items field" \
  '{"criterion":true,"items":true,"needs":true,"serves":true,"size":true}' --argjson v "$next_schema" '
    (.types | map({key: .name, value: .}) | from_entries) as $t
    | {criterion: ($t.criterion.since == $v),
       serves: ($t.requirement.attributes.serves.since == $v),
       size: ($t.plan_task.attributes.size.since == $v),
       needs: (($t.plan_task.attributes.needs.scope // null) == null
               and (($t.plan_task.attributes.needs.items // []) | index("issue")) != null
               and $t.plan_task.attributes.needs.item_versions.issue.since == $v),
       items: ([$t[] | .attributes // {} | .[] | select(.items != null and .since == $v)] | length > 0)}'
expect "$section" "no typed block declares an attribute that takes a node state value, which rides no block schema bump" 0 '
    [.types[] | .attributes // {} | .[] | (.choices // []) | .[] | select(. == "proposed" or . == "settled" or . == "overtaken")] | length'
bumps=()
while IFS= read -r commit; do
  [[ -n "$commit" ]] || continue
  issue_tree_order_schema_version "$commit"
  after_version="$schema_version"
  issue_tree_order_schema_version "$commit^"
  [[ "$after_version" == "$schema_version" ]] || bumps+=("$commit ($schema_version to $after_version)")
done < <(git -C "$checkout" log --no-merges --format=%H "$base_commit..HEAD" -- "$schema_path")
((${#bumps[@]} == 1)) ||
  fail "$section" "one commit of BASELINE_SHA..HEAD bumps the block schema" \
    "${#bumps[@]} commits move its version: ${bumps[*]:-none}"
pass "$section" "one commit of BASELINE_SHA..HEAD bumps the block schema: ${bumps[0]}"
sections=$((sections + 1))

# --- history: the deletion lands after the tree read, and no route goes -----------------------

section=history
mapfile -t landed < <(git -C "$checkout" log --reverse --format=%H origin/main..HEAD)
((${#landed[@]} > 0)) || fail "$section" "read git log --reverse --format=%H origin/main..HEAD" "it lists no commit"
declare -A position=()
for index in "${!landed[@]}"; do
  position[${landed[$index]}]="$index"
done
route_commit="$(git -C "$checkout" log --reverse --format=%H -S'"/api/v1/issues/{key}/tree"' origin/main..HEAD -- "$routes_path" "$tree_route_path" | head -n 1)"
[[ -n "$route_commit" && -n "${position[$route_commit]:-}" ]] ||
  fail "$section" "a commit of origin/main..HEAD adds the route $tree_route to $routes_path or $tree_route_path" "git log -S finds none"
pass "$section" "commit $route_commit, number $((position[$route_commit] + 1)) of ${#landed[@]}, adds $tree_route"
for name in "${deleted_lookups[@]}"; do
  deletion=""
  while IFS= read -r commit; do
    [[ -n "$commit" ]] || continue
    if git -C "$checkout" grep -qw "$name" "$commit^" -- 'packages/envoy/*.go' &&
      ! git -C "$checkout" grep -qw "$name" "$commit" -- 'packages/envoy/*.go'; then
      deletion="$commit"
    fi
  done < <(git -C "$checkout" log --reverse --format=%H -S"$name" origin/main..HEAD -- 'packages/envoy/*.go')
  [[ -n "$deletion" && -n "${position[$deletion]:-}" ]] ||
    fail "$section" "a commit of origin/main..HEAD deletes $name" "git log -S$name finds no commit after which Go source names it no more"
  ! git -C "$checkout" grep -qw "$name" HEAD -- 'packages/envoy/*.go' ||
    fail "$section" "$name stays deleted at HEAD" "Go source names it again at HEAD"
  ((position[$deletion] > position[$route_commit])) ||
    fail "$section" "the commit deleting $name lands after the one adding $tree_route" \
      "$deletion is number $((position[$deletion] + 1)) and $route_commit number $((position[$route_commit] + 1)) of ${#landed[@]}"
  pass "$section" "commit $deletion, number $((position[$deletion] + 1)), deletes $name after $tree_route landed"
done
issue_tree_order_route_lines "$base_commit" | issue_tree_order_routes >"$work/routes.base" ||
  fail "$section" "read the routes declared at BASELINE_SHA" "git grep at $base_commit over $api_path failed"
issue_tree_order_route_lines | issue_tree_order_routes >"$work/routes.head" ||
  fail "$section" "read the routes declared in this checkout" "git grep over $checkout/$api_path failed"
[[ -s "$work/routes.base" && -s "$work/routes.head" ]] ||
  fail "$section" "read the routes declared in $api_path at BASELINE_SHA and at this head" \
    "read $(wc -l <"$work/routes.base") and $(wc -l <"$work/routes.head") routes"
grep -qxF "$decisions_route" "$work/routes.base" ||
  fail "$section" "BASELINE_SHA serves $decisions_route" "no row of its $routes_path and no route file of its $api_path declares that route"
missing="$(LC_ALL=C comm -23 "$work/routes.base" "$work/routes.head" | paste -sd ';' -)"
[[ -z "$missing" ]] || fail "$section" "every route of BASELINE_SHA stands at this head" "missing at this head: $missing"
pass "$section" "all $(wc -l <"$work/routes.base") routes of BASELINE_SHA stand at this head, $decisions_route among them"
sections=$((sections + 1))

# --- dependency: one relation, two spellings -----------------------------------------------------

section=dependency
tree_send GET "/api/v1/issues/$ordered"
tree_want 200 "$section" "read $ordered"
cp "$work/body" "$work/issue.after.json"
expect "$section" "GET /api/v1/issues/$ordered answers blocked_by beside needs, each naming $blocker" \
  "$(jq -cn --arg b "$blocker" '[[$b], [$b]]')" '[.blocked_by, .needs]'
# The summary read is the list read's row (model.IssueSummary, which carries blocked_by beside
# needs): GET /api/v1/issues/{key}/summary is the reference summary (key, project, title, status,
# documents) and answers neither.
tree_send GET "/api/v1/issues?project=${ordered%%-*}"
tree_want 200 "$section" "read the issue list of ${ordered%%-*}"
expect "$section" "GET /api/v1/issues lists $ordered with blocked_by beside needs, each naming $blocker" \
  "$(jq -cn --arg b "$blocker" '[[$b], [$b]]')" \
  --arg k "$ordered" '[(if type == "array" then . else .issues end)[] | select(.key == $k) | [.blocked_by, .needs]]
    | if length == 1 then .[0] else "the list holds \(length) rows of " + $k end'
tree_send GET "/api/v1/issues/$ordered/events"
tree_want 200 "$section" "read $ordered's events"
# Every object of the events that holds either member holds both, with the same targets; a failure
# prints the objects whose two members differ, which is what a reader needs to find the writer.
expect "$section" "GET /api/v1/issues/$ordered/events answers no object whose blocked_by and needs differ" '[]' \
  '[.. | objects | select(has("blocked_by") or has("needs")) | select(.blocked_by != .needs)] | map({blocked_by, needs, keys: keys})'
expect "$section" "GET /api/v1/issues/$ordered/events answers an object holding both members, naming $blocker" true \
  --arg b "$blocker" '[.. | objects | select(has("blocked_by") or has("needs"))] as $held
    | ($held | length > 0) and any($held[]; (.needs // []) | index($b) != null)'
tree_send PATCH "/api/v1/issues/$ordered" "$(jq -cn --arg b "$blocker" --argjson a "$(tree_actor)" \
  '{needs: [$b], blocked_by: [$b], actor: $a}')"
tree_want 200 "$section" "PATCH $ordered with needs and blocked_by naming the same target"
expect "$section" "a PATCH naming needs and blocked_by with the same target is served from them" \
  "$(jq -cn --arg b "$blocker" '[[$b], [$b]]')" '[.blocked_by, .needs]'
tree_send PATCH "/api/v1/issues/$ordered" "$(jq -cn --arg b "$blocker" --arg f "$fresh" --argjson a "$(tree_actor)" \
  '{needs: [$b], blocked_by: [$f], actor: $a}')"
tree_want 400 "$section" "PATCH $ordered with needs and blocked_by naming different targets"
expect "$section" "the refusal of two different lists names needs and blocked_by" true \
  '(tostring | contains("needs")) and (tostring | contains("blocked_by"))'
# Each field the issue read answered before the upgrade still answers once the issue converted.
lost="$(jq -r --slurpfile after "$work/issue.after.json" \
  '[to_entries[] | select(.value != null) | .key as $k | select(($after[0] | has($k) | not) or $after[0][$k] == null) | $k] | join(", ")' \
  "$work/capture/issue.json")" || fail "$section" "compare $ordered's issue read before and after" "jq could not read them"
[[ -z "$lost" ]] ||
  fail "$section" "no field of the issue read answers null for ever after the conversion" "these answered before the upgrade and answer null or nothing now: $lost"
pass "$section" "every field $ordered's issue read answered before the upgrade still answers after its conversion"
sections=$((sections + 1))

printf 'PASS: %s complete %d sections\n' "$driver" "$sections"
