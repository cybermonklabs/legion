#!/usr/bin/env bash
# Check c-view-arms (the issue-tree spec's r-issue-relations, and r-no-regression of its parent
# spec): graph_edges keeps every relation of an issue, a component and a document across an upgrade
# from a release that holds no graph store at all, and across a rollback to it. This driver is the
# half of the check that needs an earlier build. The other half is
# TestGraphEdgesKeepsEveryRelationAcrossARollback (packages/envoy/internal/dispatch/api), which
# proves the same arms over a database storetest migrated to this head, and the plan at its scale.
#
# What it proves, over servers that really ran. Each relation the release before writes - an issue
# link, a parent, a component's parent, a component dependency, an issue's component, a document
# on an issue and that document's citation of another issue - is answered once by graph_edges: by
# the old arm over its own table while its row names no node, and by the store once this head's
# backfill has stamped the row, dated as the release before dated it. A row a rolled-back release
# writes with no node, or whose edge column it moves in place, goes back to its old arm until the
# next backfill; a row it deletes with no retraction is answered nowhere. The issue link reads as
# blocked_by in graph_edges and as needs in the outbound references read. The four arms the
# graph-store spec moved (document_blocks' part_of, block_refs, and comments' anchored_to and
# owned_by) stay while the newest announcement of another build lists none of their kinds, go at
# the first boot of a later build once this head's own announcement, which lists them, is recorded,
# and go at once over a database this head created.
#
# It runs over infra_1253_upgrade, as the Verification preamble states it for a driver that reads
# rows an earlier release wrote, and empties that database twice. Its sections:
#   setup     empties the database DATABASE_URL names, starts the fake GitHub (e2e/fake-github.ts),
#             and calls lib-baseline.sh's baseline_start with BASELINE_SHA set to PRE_1149_SHA, the
#             release before the graph store, which lib-baseline.sh builds stamped with that commit
#             (-X main.buildCommit) and boots with a throwaway GitHub App pointed at the fake. It
#             signs DEV_SIGNIN_LOGIN in through GET /auth/_dev/signin and creates PROJECT with that
#             person as its default assignee.
#   before    through that server's API writes nine issues: P; T; M; S, a child of P waiting on T; D,
#             waiting on T; E and X, children of P; and N and C, which hold no relation yet. It
#             imports an architecture source whose model is six components (legion; daemon and envoy
#             under it, daemon depending on envoy; workspace under daemon; listener and server under
#             envoy, server depending on listener), gives S the component daemon and D envoy, and
#             uploads to S a document citing T. That release's graph_edges answers each of those
#             relations once, which the driver captures with each one's time, and its
#             referenced_by_count of T, P and M. That server registers PRE_1149_SHA in
#             server_processes as the only live process, and the driver waits until the newest
#             server_build_announcements row names it, which that server's own startup.Announcer
#             writes, polling every 2 s and failing after 300 s, and then stops it.
#   held      boots this head over that database with its boot backfill, its block index and its
#             decisions and issue-room steps held, so every row reads as the release before wrote
#             it: each relation is answered once, by its old arm, as captured; the outbound
#             references read answers each once, the link as needs; each count reads as captured.
#   upgrade   boots this head with R6's step alone held and waits for the backfill as the preamble
#             states, then for the line of the boot's step of the four arms, which it logs whichever
#             way it ends (`dispatch: the graph_edges arms step ...`) and which here says it left
#             them. The newest announcement of another build is PRE_1149_SHA's, which lists no kind,
#             so the four arms stay. Each relation is answered once, by the store, dated as captured,
#             the issue link under blocked_by; GET /api/v1/references?from=<ref> answers each once,
#             the link as needs; each count reads as captured.
#   beside    a second process of this head, started beside the first: the same holds through it.
#   rollback  through this head uploads to D a document citing C, which the store states; then by
#             SQL, as a build of the release before writes and deletes, naming no node and
#             retracting nothing: N waits on T, C's row names P and no node, D's link and D's
#             component go, E's parent is cleared, X's parent moves in place to M, relay is added
#             under legion, daemon depends on listener, workspace and server's dependency go,
#             listener's parent moves in place to daemon, N is given envoy, and the refs row of D's
#             document goes. Each standing relation is answered once - by its old arm where its row
#             names no node or was moved in place, by the store otherwise - and each deleted one,
#             and each old value, nowhere, through graph_edges, the references read of both
#             processes and the counts.
#   restart   the second process restarted: its boot backfill appends the tuple that names each new
#             value, so each standing relation is answered once, by the store, dated as its old arm
#             dated it, and the old arms answer none; X and listener hang under their new parents
#             alone; the four arms stay.
#   later     a later build, over the same database: this head's own announcement, which it wrote
#             once it was the only live build and which lists the four arms' kinds, is the newest.
#             With every process of this head stopped, it builds this head again stamped with
#             another commit (the checkout's HEAD^), which stands for a later build, boots it, and
#             waits for the backfill and the line of its step: the step deleted the four arms, and
#             the tuple arm probes none of their tables; each standing relation is still answered
#             once, by the store, dated as before, each deleted one nowhere, through graph_edges, the
#             references read and the counts.
#   scale     with no process up, seeds by SQL the scale of the Go half - 500 issues, 50,000 issue
#             links, 5,000 components with 50,000 dependencies and 50,000 issue components, half of
#             each naming its node - runs analyze, and takes explain (analyze, buffers) of a
#             graph_edges read of one issue in each direction, over the view the later build left:
#             no Seq Scan of issues, issue_links, components, component_depends or
#             issue_component_members.
#   empty     empties the database and boots this head over it: the four arms are gone at once, and
#             the child_of, blocked_by, three component and attached_to arms and the arm over refs
#             stand, each over the rows that name no node.
#
# Inputs: DATABASE_URL (infra_1253_upgrade on this machine, which this driver empties),
# DISPATCH_TOKEN (the agent bearer every server accepts, e2e-token), PROJECT (CORE) and PRE_1149_SHA
# (the release before the graph store, which is `git merge-base origin/main
# origin/legion/INFRA-1149` where unset), the inputs c-view-arms' run line passes; DEV_SIGNIN_LOGIN
# (optional, the person, `alice` by default), BASELINE_EXTRA_ENV and LEGION_WORKSPACE as
# lib-baseline.sh reads them; and on PATH the stamped envoy-dispatch of this head, bun (the fake
# GitHub), base64, curl, diff, git, go, jq, openssl, psql and python3. Each unmet input is exit 2.
# Run it with bash, never bun.
#
# Each passing check prints `PASS: <section> <what>`; the first failing one prints
# `FAIL: <section> <what>: <observed>` and exits 1. The last line is
# `PASS: view-arms-rollback.sh complete 10 sections`, printed only after the last section.
#
# Every check is `cmd || fail …`, never part of an `&&` list: under `set -e` a failing command
# anywhere but last in such a list does not stop the script.

# shellcheck disable=SC2016 # the single-quoted jq filters name jq variables
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: cd packages/dispatch && DATABASE_URL=$INFRA_1253_UPGRADE_DATABASE_URL DISPATCH_TOKEN=e2e-token \
         PROJECT=CORE [PRE_1149_SHA=<commit>] [DEV_SIGNIN_LOGIN=alice] \
         bash e2e/acceptance/view-arms-rollback.sh
       (DATABASE_URL names infra_1253_upgrade on this machine, which this driver empties;
       PRE_1149_SHA is the release before the graph store and defaults to
       `git merge-base origin/main origin/legion/INFRA-1149`; the stamped envoy-dispatch of this
       head is on PATH)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
# lib-processes.sh sources lib-baseline.sh, which builds and boots every server here, and gives
# start_fake, which starts the fake GitHub the earlier release imports its components from.
# shellcheck source-path=SCRIPTDIR source=lib-processes.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib-processes.sh"
# shellcheck source-path=SCRIPTDIR source=lib-tree.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib-tree.sh"
# shellcheck source-path=SCRIPTDIR source=seed-corpus.sh
. "$(dirname "${BASH_SOURCE[0]}")/seed-corpus.sh"

require_env DATABASE_URL DISPATCH_TOKEN PROJECT
require_commands base64 bun curl diff envoy-dispatch git go jq openssl psql python3
readonly driver=view-arms-rollback.sh
# shellcheck disable=SC2153 # PROJECT is the caller's input, checked by name above
project="$PROJECT"
readonly project
# Not readonly: lib-baseline.sh's baseline_boot takes a local of that name.
# shellcheck disable=SC2031 # the run's own DATABASE_URL, which no server's subshell changes
database_url="$DATABASE_URL"
readonly person_login="${DEV_SIGNIN_LOGIN:-alice}"
# The caller's own BASELINE_EXTRA_ENV pairs, which every server here gets beside the driver's.
readonly given_extra_env="${BASELINE_EXTRA_ENV-}"
# The earlier release is built from its commit: a BASELINE_BINARY the shell exported would make
# baseline_start boot that binary in place of PRE_1149_SHA's build.
unset BASELINE_BINARY

# The database this driver empties: only a database on this machine whose name begins
# infra_1253_upgrade, which the upgrade drivers of this issue alone use, and never infra_1253, which
# `bun run dispatch:local` serves, named with no connection override that libpq would connect by
# instead (lib-tree.sh's tree_database_parts). It is refused before anything is built.
readonly database_names='infra_1253_upgrade*'
tree_database_parts "$database_url" "$database_names"

# The earlier release, read in full in the checkout lib-baseline.sh builds it from, so the commit
# read here is the commit built. PRE_1149_SHA must predate the graph store: over a build that holds
# it, the announcement it writes could list the four arms' kinds, and the upgrade below would read
# nothing about a release that writes no tuple. PRE_1149_SHA is `git merge-base origin/main
# origin/legion/INFRA-1149` where the run names none, the release before the branch that added the
# graph store. The later build the `later` section boots is this head stamped again with the
# checkout's HEAD^ (later_commit), read there.
readonly repo="${LEGION_WORKSPACE:-$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../../.." && pwd)}"
if [[ -z "${PRE_1149_SHA:-}" ]]; then
  PRE_1149_SHA="$(git -C "$repo" merge-base origin/main origin/legion/INFRA-1149)" || {
    printf '%s: PRE_1149_SHA is unset and git merge-base origin/main origin/legion/INFRA-1149 names no commit of %s\n' \
      "$driver" "$repo" >&2
    exit 2
  }
fi
readonly graph_store_migration=packages/envoy/internal/dispatch/store/migrations/20261109013400_graph_store.up.sql
pre_commit="$(git -C "$repo" rev-parse --verify --quiet "${PRE_1149_SHA}^{commit}")" || {
  printf '%s: PRE_1149_SHA %s names no commit of %s\n' "$driver" "$PRE_1149_SHA" "$repo" >&2
  exit 2
}
readonly pre_commit
if git -C "$repo" cat-file -e "$pre_commit:$graph_store_migration" 2>/dev/null; then
  printf '%s: PRE_1149_SHA names %s, whose tree holds %s: it must name the release before the graph store\n' \
    "$driver" "$pre_commit" "$graph_store_migration" >&2
  exit 2
fi
printf 'PRE_1149_SHA names %s, the release before the graph store\n' "$pre_commit"

# The architecture source the earlier release imports its components from, on the fake GitHub.
readonly arch_repo=acme/view-arms
# The four arms the graph-store spec moved, by the names view_arms_marks finds them by, and the
# three projection tables the tuple arm probes for their kinds while they stand.
readonly -a four_arms=(block_refs document_blocks comments_anchored_to comments_owned_by)
readonly -a four_probes=(document_blocks block_refs comments)
# The kinds those four arms answer, which an announcement lists once its build writes them as
# tuples: a block's in_document (rendered part_of), a link's mentions, a comment's anchored_to and
# owned_by.
readonly -a four_kinds=(in_document mentions anchored_to owned_by)
# What every line the boot's step over the four arms logs opens with (graphcensus.ViewArmsStepLine):
# it logs one whichever way it ends, and a read after that line reads the view the step left. The
# boot runs it on the backfill's own goroutine once the backfill has logged its end, before the
# typed block supertags step, which states two functions the view calls and none of its text. The
# two ends this driver waits for: the step left the four arms, and the step deleted them.
readonly steps_line='dispatch: the graph_edges arms step'
readonly step_kept='leaves the four old arms'
readonly step_deleted='deleted the four old arms'
# The arms an edge is read through, which view_arms_load reads out of the catalogue.
readonly -a loaded_arms=(tuple refs issues issue_links artifacts artifacts_marked components component_depends
  issue_component_members)
# The holds of the boot that reads the rows as the release before left them: its run of the
# backfill, the block index's reconciliation and the decisions step, beside tree_boot_head's own
# hold of R6's step.
readonly before_backfill_holds=$'DISPATCH_TEST_HOLD_BOOT_BACKFILL=1\nDISPATCH_TEST_HOLD_BLOCK_INDEX=1\nDISPATCH_TEST_HOLD_DECISIONS_STEP=1'

# Each process of this head, by name: its pid, address and log, which view_arms_boot records.
declare -A head_pid=() head_url=() head_log=()
head_commit=""
# Each document's address by its uuid, which its outbound references read names.
declare -A artifact_address=()

work_dir
cleanup() {
  local code=$? name
  trap - EXIT INT TERM
  for name in "${!head_pid[@]}"; do
    baseline_stop_pid "${head_pid[$name]}"
  done
  tree_stop_head
  stop_processes
  baseline_stop
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# --- helpers ------------------------------------------------------------------------------------

# lit VALUE: VALUE as one SQL string literal.
lit() { printf "'%s'" "${1//\'/\'\'}"; }

# view_arms_github SECTION WHAT REPOS: puts REPOS, a JSON object of repositories, as everything the
# fake GitHub serves.
view_arms_github() {
  status="$(curl -sS -X PUT -o "$work/body" -w '%{http_code}' -H 'Content-Type: application/json' \
    --data-binary "$3" "http://127.0.0.1:$github_port/__fixture/repos" 2>"$work/curl")" ||
    status="curl failed: $(<"$work/curl")"
  want_status 200 "$1" "$2"
  pass "$1" "$2"
}

# view_arms_boot_earlier SECTION COMMIT: empties the database and boots the earlier release COMMIT
# over it through lib-baseline.sh's baseline_start, with BASELINE_SHA set to COMMIT and the fake
# GitHub's throwaway App beside the caller's pairs; signs the person in and creates the project.
view_arms_boot_earlier() {
  local section="$1" commit="$2"
  tree_reset_database "$database_url" "$database_names" "$section"
  BASELINE_SHA="$commit"
  BASELINE_EXTRA_ENV="${given_extra_env:+$given_extra_env$'\n'}$github_env"
  # The bearer every server accepts, which baseline_start reads from DISPATCH_TOKEN.
  DISPATCH_TOKEN="$token"
  baseline_start
  unset DISPATCH_TOKEN DISPATCH_TOKEN_FILE
  BASELINE_EXTRA_ENV="$given_extra_env"
  [[ "$baseline_commit" == "$commit" ]] ||
    fail "$section" "the earlier release booted is the build of $commit" "it was built from '$baseline_commit'"
  base="$baseline_url"
  tree_sign_in "$section" "$person_login"
  tree_create_project "$section" "$project" "The relations graph_edges keeps across a rollback"
}

# view_arms_issue SECTION TITLE [PARENT [BLOCKED_BY]]: the agent creates an issue of the project,
# under PARENT and waiting on BLOCKED_BY where given, through the members every release reads
# (seed-corpus.sh's corpus_issue); its key is in corpus_key.
view_arms_issue() {
  printf 'The issue "%s".\n' "$2" >"$work/spec.md"
  corpus_issue "$1" "$2" "$work/spec.md" "${3:-}" "${4:-}"
  [[ "$corpus_key" =~ ^[A-Z][A-Z0-9]*-[0-9]+$ ]] ||
    fail "$1" "create the issue \"$2\"" "the key $corpus_key is no issue key"
}

# view_arms_upload SECTION ISSUE NAME TEXT: the agent uploads TEXT as the document NAME of ISSUE;
# its uuid is in uploaded_id, and its address, which its outbound references read names, is kept
# in artifact_address.
view_arms_upload() {
  local section="$1" issue="$2" name="$3" slug
  jq -cn --arg n "$name" --arg c "$4" --argjson a "$(tree_actor)" '{name: $n, content: $c, actor: $a}' \
    >"$work/upload.json"
  tree_send POST "/api/v1/issues/$(q "$issue")/artifacts" "@$work/upload.json"
  tree_want 201 "$section" "upload $name to $issue"
  tree_field "$section" "upload $name to $issue" .artifact.id
  uploaded_id="$tree_value"
  tree_field "$section" "upload $name to $issue" .artifact.slug
  slug="$tree_value"
  artifact_address["$uploaded_id"]="$issue/artifact/$slug"
  pass "$section" "uploaded $name ($uploaded_id) to $issue"
}

# view_arms_fixture SECTION: writes the relations through the server at $base, as the earlier
# release writes them, and names each in upgrade_edges.
view_arms_fixture() {
  local section="$1" attach issue component
  view_arms_issue "$section" "The parent the children hang under"
  parent="$corpus_key"
  view_arms_issue "$section" "The target the links wait on"
  target="$corpus_key"
  view_arms_issue "$section" "The parent a child is moved to in place"
  moved_to="$corpus_key"
  view_arms_issue "$section" "Waits on the target under the parent" "$parent" "$target"
  stamped="$corpus_key"
  view_arms_issue "$section" "Its link is deleted with no retraction" "" "$target"
  deleted_link="$corpus_key"
  view_arms_issue "$section" "Its parent is cleared with no retraction" "$parent"
  deleted_parent="$corpus_key"
  view_arms_issue "$section" "Its parent is moved in place" "$parent"
  moved="$corpus_key"
  view_arms_issue "$section" "The waiter a rolled-back release writes with no node"
  waiter="$corpus_key"
  view_arms_issue "$section" "The child a rolled-back release writes with no node"
  child="$corpus_key"
  pass "$section" "wrote P $parent, T $target, M $moved_to, S $stamped, D $deleted_link, E $deleted_parent, X $moved, N $waiter and C $child"

  # The components come through the earlier release's own importer, from a model on the fake
  # GitHub; the fake is emptied once the import has run, so no later import of either release
  # rewrites a component the driver reads.
  view_arms_github "$section" "seed the fake GitHub with $arch_repo's model of six components" \
    "$(jq -cn --arg repo "$arch_repo" '{($repo): {contents: "read", installation_id: 367, files: {
      "legion.md": "---\ntitle: Legion\n---\nThe swarm.\n",
      "daemon.md": "---\ntitle: Daemon\nparent: legion\ndepends_on: [envoy]\n---\nThe daemon.\n",
      "envoy.md": "---\ntitle: Envoy\nparent: legion\n---\nRouting.\n",
      "workspace.md": "---\ntitle: Workspace\nparent: daemon\n---\nIssue workspaces.\n",
      "listener.md": "---\ntitle: Listener\nparent: envoy\n---\nThe listener.\n",
      "server.md": "---\ntitle: Server\nparent: envoy\ndepends_on: [listener]\n---\nThe server.\n"}}}')"
  tree_send PUT "/api/v1/projects/$(q "$project")/architecture-sources/$arch_repo" \
    "$(jq -cn --argjson a "$(tree_actor)" '{branch: "main", actor: $a}')"
  tree_want 200 "$section" "add $arch_repo as an architecture source of $project"
  tree_send POST "/api/v1/projects/$(q "$project")/architecture-sources/$arch_repo/sync"
  tree_want 200 "$section" "sync $arch_repo"
  expect "$section" "the sync of $arch_repo imports its six components" '[null,6]' '[.last_error, .components]'
  view_arms_github "$section" "empty the fake GitHub, so no later import rewrites a component" '{}'
  for attach in "$stamped daemon" "$deleted_link envoy"; do
    read -r issue component <<<"$attach"
    tree_send PATCH "/api/v1/issues/$(q "$issue")" "$(jq -cn --arg c "$component" --argjson a "$(tree_actor)" \
      '{components: {mode: "explicit", ids: [$c]}, actor: $a}')"
    tree_want 200 "$section" "give $issue the component $component"
  done
  pass "$section" "gave $stamped the component daemon and $deleted_link envoy"

  view_arms_upload "$section" "$stamped" notes.md \
    "The notes of the waiting issue cite dispatch://$target, the issue it waits on."$'\n'
  doc_id="$uploaded_id"

  upgrade_edges=(
    "issue $stamped blocked_by issue $target"
    "issue $deleted_link blocked_by issue $target"
    "issue $stamped child_of issue $parent"
    "issue $deleted_parent child_of issue $parent"
    "issue $moved child_of issue $parent"
    "component $project/daemon part_of component $project/legion"
    "component $project/envoy part_of component $project/legion"
    "component $project/workspace part_of component $project/daemon"
    "component $project/listener part_of component $project/envoy"
    "component $project/server part_of component $project/envoy"
    "component $project/daemon depends_on component $project/envoy"
    "component $project/server depends_on component $project/listener"
    "issue $stamped affects component $project/daemon"
    "issue $deleted_link affects component $project/envoy"
    "artifact $doc_id attached_to issue $stamped"
    "artifact $doc_id mentions issue $target"
  )
  # The document's citation is indexed by the earlier release's own writer, which may settle after
  # the upload answers.
  view_arms_wait_edge "$section" "artifact $doc_id mentions issue $target"
}

# view_arms_where EDGE: the SQL test that a row is EDGE, `from_kind from_id kind to_kind to_id`.
view_arms_where() {
  local from_kind from_id kind to_kind to_id
  read -r from_kind from_id kind to_kind to_id <<<"$1"
  printf 'from_kind = %s and from_id = %s and kind = %s and to_kind = %s and to_id = %s' \
    "$(lit "$from_kind")" "$(lit "$from_id")" "$(lit "$kind")" "$(lit "$to_kind")" "$(lit "$to_id")"
}

# view_arms_answers SECTION EDGE: how many rows graph_edges answers EDGE with, in view_rows, and the
# created_at of the last, in view_at.
view_arms_answers() {
  query "$1" "read $2 out of graph_edges" \
    "select count(*) || '|' || coalesce(max(created_at)::text, '') from graph_edges where $(view_arms_where "$2")"
  view_rows="${answer%%|*}" view_at="${answer#*|}"
}

# view_arms_wait_edge SECTION EDGE: waits until graph_edges answers EDGE once, polling every 2 s
# and failing after 300 s with how many times it answered it last.
view_arms_wait_edge() {
  local section="$1" edge="$2" deadline=$((SECONDS + tree_wait_seconds))
  while true; do
    view_arms_answers "$section" "$edge"
    if [[ "$view_rows" == 1 ]]; then
      pass "$section" "graph_edges answers $edge"
      return 0
    fi
    ((SECONDS < deadline)) ||
      fail "$section" "graph_edges answers $edge within ${tree_wait_seconds} s" \
        "after ${tree_wait_seconds} s it answers it $view_rows times"
    sleep "$tree_poll_seconds"
  done
}

# view_arms_marks NAME: the fragments of graph_edges' text, as pg_get_viewdef's pretty form prints
# the view, that every one of the arm NAME holds and no other arm holds all of, in marks. The pretty
# form opens each arm's own FROM at three spaces after a newline and prints each constant column as
# `'<value>'::text AS <column>`; a probe inside an arm is indented further.
view_arms_marks() {
  case "$1" in
    tuple) marks=('edge.from_kind' 'graph_tuples tuple') ;;
    refs) marks=($'\n   FROM refs r') ;;
    issues) marks=("'child_of'::text AS kind" $'\n   FROM issues') ;;
    issue_links) marks=("'blocked_by'::text AS kind" $'\n   FROM issue_links') ;;
    artifacts) marks=("'attached_to'::text AS kind" 'node_kind IS NULL') ;;
    artifacts_marked) marks=("'attached_to'::text AS kind" 'JOIN graph_nodes node') ;;
    components) marks=("'part_of'::text AS kind" $'\n   FROM components c') ;;
    component_depends) marks=("'depends_on'::text AS kind" $'\n   FROM component_depends d') ;;
    issue_component_members) marks=("'affects'::text AS kind" $'\n   FROM issue_component_members m') ;;
    block_refs) marks=($'\n   FROM block_refs\n') ;;
    document_blocks) marks=($'\n   FROM document_blocks d') ;;
    comments_anchored_to) marks=("'anchored_to'::text AS kind" $'\n   FROM comments c') ;;
    comments_owned_by) marks=("'owned_by'::text AS kind" $'\n   FROM comments c') ;;
    *) tree_refuse "view_arms_marks knows no arm '$1'" ;;
  esac
}

# The arms of graph_edges as the catalogue prints the view back, split on its top-level UNION ALL,
# which the pretty form puts on a line of its own: no arm holds a union of its own.
readonly view_arms_split="regexp_split_to_table(rtrim(rtrim(pg_get_viewdef('graph_edges'::regclass, true), E'\\n'), ';'), E'\\nUNION ALL\\n')"

# view_arms_tests NAME: the SQL test that an arm, `arm`, holds every mark of the arm NAME.
view_arms_tests() {
  local mark tests=""
  view_arms_marks "$1"
  for mark in "${marks[@]}"; do
    tests+=" and strpos(arm, $(lit "$mark")) > 0"
  done
  printf '%s' "$tests"
}

# view_arms_count SECTION NAME: how many arms of graph_edges read as the arm NAME, in arm_count.
view_arms_count() {
  query "$1" "count the $2 arms of graph_edges" \
    "select count(*) from $view_arms_split arm where true$(view_arms_tests "$2")"
  arm_count="$answer"
}

# view_arms_arm SECTION NAME: the one arm of graph_edges that reads as the arm NAME, its text kept
# in $work/arm.NAME, which an edge is read through on its own; any other count fails.
view_arms_arm() {
  local section="$1" name="$2" count
  query "$section" "read the $name arm of graph_edges" \
    "select count(*) || E'\\n' || coalesce(string_agg(arm, E'\\n'), '') from $view_arms_split arm where true$(view_arms_tests "$name")"
  count="${answer%%$'\n'*}"
  [[ "$count" == 1 ]] || fail "$section" "graph_edges holds one $name arm" "it holds $count arms that read as $name"
  printf '%s\n' "${answer#*$'\n'}" >"$work/arm.$name"
}

# view_arms_arm_answers SECTION NAME EDGE: how many rows the arm NAME, read on its own, answers EDGE
# with, in arm_rows.
view_arms_arm_answers() {
  query "$1" "read $3 through the $2 arm of graph_edges" \
    "select count(*) from ($(<"$work/arm.$2")) arm (from_kind, from_id, kind, to_kind, to_id, created_at, source_seq)
     where $(view_arms_where "$3")"
  arm_rows="$answer"
}

# view_arms_load SECTION STAGE: reads graph_edges back out of the catalogue, prints each arm by the
# relation its own FROM reads, and keeps the text of each arm an edge is read through. The six arms
# over an issue's, a component's and a document's relations answer only a row that names no node,
# the child_of and part_of arms also a marked row moved in place while the newest tuple of its kind
# does not name the column's value, and the arm over refs answers a document's row only while the
# document names no node.
view_arms_load() {
  local section="$1" stage="$2" name
  query "$section" "read graph_edges out of the catalogue" "
    select count(*) || ' arms: ' || string_agg(coalesce(substring(arm from E'\\n   FROM ([a-z_]+)'), 'a sub-select'), ', ' order by place)
    from $view_arms_split with ordinality as split (arm, place)"
  printf '%s pg_get_viewdef(graph_edges, true), split on its top-level UNION ALL, holds %s\n' "$stage" "$answer"
  for name in "${loaded_arms[@]}"; do
    view_arms_arm "$section" "$name"
  done
  for name in issues issue_links artifacts components component_depends issue_component_members; do
    grep -qF 'node_kind IS NULL' "$work/arm.$name" ||
      fail "$section" "$stage the $name arm answers only the rows that name no node" \
        "its text holds no node_kind IS NULL test: $(head -c 600 "$work/arm.$name")"
  done
  for name in issues components; do
    grep -qF 'graph_tuples tuple' "$work/arm.$name" ||
      fail "$section" "$stage the $name arm answers a marked row moved in place while the newest tuple of its kind does not name the column's value" \
        "its text reads no graph_tuples: $(head -c 600 "$work/arm.$name")"
  done
  grep -qF 'artifacts mentioning' "$work/arm.refs" ||
    fail "$section" "$stage the arm over refs answers a document's row only while the document names no node" \
      "its text reads no artifacts row of the citing document: $(head -c 600 "$work/arm.refs")"
  pass "$section" "$stage the child_of, blocked_by, three component and attached_to arms stand over the rows that name no node, the child_of and part_of arms also over a row moved in place, beside the arm over refs and the tuple arm"
}

# view_arms_old_arm EDGE / view_arms_store_arm EDGE: the arm that answers EDGE while its row names no
# node, and the arm that answers it once the row names its node, in arm_name.
view_arms_old_arm() {
  local from_kind from_id kind
  read -r from_kind from_id kind _ <<<"$1"
  case "$kind" in
    blocked_by) arm_name=issue_links ;;
    child_of) arm_name=issues ;;
    part_of) arm_name=components ;;
    depends_on) arm_name=component_depends ;;
    affects) arm_name=issue_component_members ;;
    attached_to) arm_name=artifacts ;;
    mentions) arm_name=refs ;;
    *) tree_refuse "view_arms_old_arm knows no old arm of $kind" ;;
  esac
}
view_arms_store_arm() {
  local from_kind from_id kind
  read -r from_kind from_id kind _ <<<"$1"
  arm_name=tuple
  [[ "$kind" != attached_to ]] || arm_name=artifacts_marked
}

# view_arms_once SECTION STAGE VIA EDGE [WHAT]: graph_edges answers EDGE once, through its old arm
# (VIA `old`) or the store's (VIA `store`) alone, and the other arm answers it nowhere. WHAT says
# which row EDGE is, for a failure.
view_arms_once() {
  local section="$1" stage="$2" via="$3" edge="$4" what="${5:+ ($5)}" answering other
  view_arms_old_arm "$edge"
  answering="$arm_name"
  view_arms_store_arm "$edge"
  other="$arm_name"
  if [[ "$via" == store ]]; then
    other="$answering"
    answering="$arm_name"
  fi
  view_arms_answers "$section" "$edge"
  [[ "$view_rows" == 1 ]] ||
    fail "$section" "$stage graph_edges answers $edge once$what" "it answers it $view_rows times"
  view_arms_arm_answers "$section" "$answering" "$edge"
  [[ "$arm_rows" == 1 ]] ||
    fail "$section" "$stage the $answering arm answers $edge once$what" "it answers it $arm_rows times"
  view_arms_arm_answers "$section" "$other" "$edge"
  [[ "$arm_rows" == 0 ]] ||
    fail "$section" "$stage the $other arm answers $edge nowhere$what: the $answering arm answers it" \
      "it answers it $arm_rows times"
  pass "$section" "$stage graph_edges answers $edge once, through the $answering arm alone$what"
}

# view_arms_nowhere SECTION STAGE EDGE WHY: graph_edges answers EDGE nowhere, nor does its old arm or
# the store's on its own.
view_arms_nowhere() {
  local section="$1" stage="$2" edge="$3" why="$4" name
  view_arms_answers "$section" "$edge"
  [[ "$view_rows" == 0 ]] ||
    fail "$section" "$stage graph_edges answers $edge nowhere ($why)" "it answers it $view_rows times"
  for name in old store; do
    if [[ "$name" == old ]]; then view_arms_old_arm "$edge"; else view_arms_store_arm "$edge"; fi
    view_arms_arm_answers "$section" "$arm_name" "$edge"
    [[ "$arm_rows" == 0 ]] ||
      fail "$section" "$stage the $arm_name arm answers $edge nowhere ($why)" "it answers it $arm_rows times"
  done
  pass "$section" "$stage graph_edges answers $edge nowhere ($why)"
}

# view_arms_dates SECTION FILE EDGE…: how many rows graph_edges answers each EDGE with and the time
# the last is dated, one line each, into FILE.
view_arms_dates() {
  local section="$1" file="$2" edge
  shift 2
  : >"$file"
  for edge in "$@"; do
    view_arms_answers "$section" "$edge"
    printf '%s|%s|%s\n' "$edge" "$view_rows" "$view_at" >>"$file"
  done
}

# view_arms_captured SECTION WHAT FILE EDGE…: captures the reading of each EDGE into FILE, each of
# which must be answered once.
view_arms_captured() {
  local section="$1" what="$2" file="$3"
  shift 3
  view_arms_dates "$section" "$file" "$@"
  if grep -v -E '\|1\|[^|]+$' "$file" >"$work/dates.off"; then
    fail "$section" "$what answers each relation it wrote once" "these it does not: $(<"$work/dates.off")"
  fi
  pass "$section" "captured what $what answers for the $# relations, each once: $(cut -d'|' -f3 "$file" | paste -sd, -)"
}

# view_arms_as_dated SECTION STAGE FILE EDGE…: graph_edges answers each EDGE as FILE captured it,
# once and dated at the same time.
view_arms_as_dated() {
  local section="$1" stage="$2" want="$3"
  shift 3
  view_arms_dates "$section" "$work/dates.now" "$@"
  diff "$want" "$work/dates.now" >"$work/dates.diff" ||
    fail "$section" "$stage graph_edges answers each relation as it was captured, once and dated as then" \
      "the readings differ (< captured, > now): $(<"$work/dates.diff")"
  pass "$section" "$stage graph_edges answers each of the $# relations once, dated as captured"
}

# view_arms_link_kinds SECTION STAGE WAITER: graph_edges names the wait of WAITER on the target
# blocked_by and nothing else, as a process of the release before reads it.
view_arms_link_kinds() {
  query "$1" "read the kinds graph_edges answers from $3 to $target" "
    select coalesce(string_agg(kind, ',' order by kind), '(none)') from graph_edges
    where from_kind = 'issue' and from_id = $(lit "$3") and to_kind = 'issue' and to_id = $(lit "$target")"
  [[ "$answer" == blocked_by ]] ||
    fail "$1" "$2 graph_edges names the issue link of $3 on $target blocked_by, as a process of the release before reads it" \
      "it answers $3 -> $target as: $answer"
  pass "$1" "$2 graph_edges names the issue link of $3 on $target blocked_by alone"
}

# view_arms_refs SECTION STAGE WANT EDGE…: GET /api/v1/references?from=<ref> of each EDGE's source,
# through the server at $base, answers EDGE WANT times; an issue link is answered as needs, and
# never as blocked_by. A source the release before deleted, whose node no read can find, answers
# 404, which reads as no edge at all where WANT is 0. A document's attached_to is graph_edges' own,
# and is not read here.
view_arms_refs() {
  local section="$1" stage="$2" want="$3" edge from_kind from_id kind to_kind to_id from answered via
  shift 3
  for edge in "$@"; do
    read -r from_kind from_id kind to_kind to_id <<<"$edge"
    [[ "$kind" != attached_to ]] || continue
    case "$from_kind" in
      issue) from="dispatch://$from_id" ;;
      component) from="dispatch://${from_id%%/*}/component/${from_id#*/}" ;;
      *) from="dispatch://${artifact_address[$from_id]}" ;;
    esac
    via="$kind"
    [[ "$kind" != blocked_by ]] || via=needs
    tree_send GET "/api/v1/references?from=$(q "$from")"
    if [[ "$status" == 404 && "$want" == 0 ]]; then
      pass "$section" "$stage GET /api/v1/references?from=$from answers $via to $to_kind $to_id nowhere: its source is gone (HTTP 404)"
      continue
    fi
    tree_want 200 "$section" "$stage read the references out of $from"
    answered="$(jq -r --arg k "$via" --arg tk "$to_kind" --arg t "$to_id" \
      '[.edges[] | select(.kind == $k and .node.kind == $tk and .node.id == $t)] | length' "$work/body" 2>/dev/null)" ||
      fail "$section" "$stage read the references out of $from" "the answer holds no edges: $(body)"
    [[ "$answered" == "$want" ]] ||
      fail "$section" "$stage GET /api/v1/references?from=$from answers $via to $to_kind $to_id $want times" \
        "it answers it $answered times: $(body)"
    if [[ "$kind" == blocked_by ]]; then
      answered="$(jq -r --arg t "$to_id" '[.edges[] | select(.kind == "blocked_by" and .node.id == $t)] | length' "$work/body")"
      [[ "$answered" == 0 ]] ||
        fail "$section" "$stage GET /api/v1/references?from=$from answers the link on $to_id as needs alone" \
          "it answers it as blocked_by $answered times: $(body)"
    fi
    pass "$section" "$stage GET /api/v1/references?from=$from through $base answers $via to $to_kind $to_id $want times"
  done
}

# view_arms_counts SECTION STAGE: refs.BacklinkCounts through the issue read
# (GET /api/v1/issues/{key}, referenced_by_count) of T, P and M, in count_target, count_parent and
# count_moved_to.
view_arms_counts() {
  local section="$1" stage="$2" key
  for key in "$target" "$parent" "$moved_to"; do
    tree_send GET "/api/v1/issues/$(q "$key")"
    tree_want 200 "$section" "$stage read $key"
    tree_field "$section" "$stage read $key's referenced_by_count" .referenced_by_count
    case "$key" in
      "$target") count_target="$tree_value" ;;
      "$parent") count_parent="$tree_value" ;;
      *) count_moved_to="$tree_value" ;;
    esac
  done
  printf '%s referenced_by_count: %s=%s %s=%s %s=%s\n' "$stage" "$target" "$count_target" \
    "$parent" "$count_parent" "$moved_to" "$count_moved_to"
}

# view_arms_want_counts SECTION STAGE TARGET PARENT MOVED_TO WHY: the counts read now are those.
view_arms_want_counts() {
  view_arms_counts "$1" "$2"
  [[ "$count_target" == "$3" && "$count_parent" == "$4" && "$count_moved_to" == "$5" ]] ||
    fail "$1" "$2 refs.BacklinkCounts reads $3 at $target, $4 at $parent and $5 at $moved_to ($6)" \
      "it reads $count_target, $count_parent and $count_moved_to"
  pass "$1" "$2 refs.BacklinkCounts reads $3 at $target, $4 at $parent and $5 at $moved_to ($6)"
}

# view_arms_boot NAME [HOLDS [BINARY COMMIT]]: boots this head as NAME over the database through
# lib-tree.sh's tree_boot_head, with R6's step held and HOLDS' NAME=value lines added for this boot
# alone, and keeps its pid, address and log by NAME. Every process of this head serves one stamped
# commit, head_commit; a process booted from BINARY, this head built again under another stamp,
# must serve COMMIT instead.
view_arms_boot() {
  local name="$1" holds="${2:-}" binary="${3:-}" want="${4:-}"
  BASELINE_EXTRA_ENV="$given_extra_env"
  if [[ -n "$holds" ]]; then
    BASELINE_EXTRA_ENV="${BASELINE_EXTRA_ENV:+$BASELINE_EXTRA_ENV$'\n'}$holds"
  fi
  tree_boot_head "$name" held "$binary"
  BASELINE_EXTRA_ENV="$given_extra_env"
  baseline_read_extra_env
  head_pid["$name"]="$tree_head_pid"
  head_url["$name"]="$tree_head_url"
  head_log["$name"]="$tree_head_log"
  # The process is stopped by name, not through tree_stop_head, which tracks one process alone.
  tree_head_pid=""
  if [[ -n "$binary" ]]; then
    [[ "$tree_head_commit" == "$want" ]] ||
      fail boot "$name serves the commit it was stamped with, $want" "it serves $tree_head_commit"
    return 0
  fi
  [[ -n "$head_commit" ]] || head_commit="$tree_head_commit"
  [[ "$tree_head_commit" == "$head_commit" ]] ||
    fail boot "every process of this head serves one commit" "$name serves $tree_head_commit and the first $head_commit"
}

# view_arms_stop NAME: stops the process of this head NAME; it deletes its own server_processes row.
view_arms_stop() {
  baseline_stop_pid "${head_pid[$1]:-}"
  unset 'head_pid[$1]'
}

# view_arms_wait_steps SECTION NAME WANT: waits until NAME's log holds a line of steps_line, which
# its boot logs once its step of the four arms has run, and finds WANT in it, the step_kept or the
# step_deleted fragment, polling every 2 s and failing after 300 s, or at once when the process
# stops. A boot whose run of the backfill logged no done line never runs the step.
view_arms_wait_steps() {
  local section="$1" name="$2" want="$3" log="${head_log[$2]}" deadline=$((SECONDS + tree_wait_seconds)) logged
  while true; do
    logged="$(grep -m 1 -F -- "$steps_line" "$log" 2>/dev/null)" || logged=""
    if [[ -n "$logged" ]]; then
      printf '%s logged, after its step of the four arms: %s\n' "$name" "$logged"
      [[ "$logged" == *"$want"* ]] ||
        fail "$section" "$name's step of the four arms says it $want" "its line reads: $logged"
      pass "$section" "$name's boot ran its step of the four arms, which says it $want"
      return 0
    fi
    kill -0 "${head_pid[$name]}" 2>/dev/null ||
      fail "$section" "$name runs its step of the four arms" "it stopped; the last lines of its log: $(tail -n 20 "$log")"
    ((SECONDS < deadline)) ||
      fail "$section" "$name runs its step of the four arms within ${tree_wait_seconds} s" \
        "after ${tree_wait_seconds} s its log holds no line '$steps_line': $(tail -n 20 "$log")"
    sleep "$tree_poll_seconds"
  done
}

# view_arms_four SECTION: which of the four arms the graph-store spec moved graph_edges holds, in
# four_standing.
view_arms_four() {
  local section="$1" name
  four_standing=""
  for name in "${four_arms[@]}"; do
    view_arms_count "$section" "$name"
    case "$arm_count" in
      0) ;;
      1) four_standing+=" $name" ;;
      *) fail "$section" "graph_edges holds the $name arm at most once" "it holds it $arm_count times" ;;
    esac
  done
  four_standing="${four_standing# }"
}

# view_arms_probes SECTION: which of the four arms' projection tables the tuple arm probes for their
# kinds, in probes_standing.
view_arms_probes() {
  local section="$1" name
  view_arms_arm "$section" tuple
  probes_standing=""
  for name in "${four_probes[@]}"; do
    if grep -qF -- "FROM $name projected" "$work/arm.tuple"; then
      probes_standing+=" $name"
    fi
  done
  probes_standing="${probes_standing# }"
}

# view_arms_four_stay SECTION STAGE WHY: all four arms stand, and the tuple arm still probes their
# three tables; a failure names each arm and each probe that went.
view_arms_four_stay() {
  local section="$1" stage="$2" why="$3" name gone=""
  view_arms_four "$section"
  view_arms_probes "$section"
  for name in "${four_arms[@]}"; do
    [[ " $four_standing " == *" $name "* ]] || gone+=" $name"
  done
  [[ -z "$gone" ]] ||
    fail "$section" "$stage the four arms the graph-store spec moved stay ($why)" "gone:${gone}; standing: ${four_standing:-none}"
  [[ "$probes_standing" == "${four_probes[*]}" ]] ||
    fail "$section" "$stage the tuple arm still probes ${four_probes[*]} for the four arms' kinds ($why)" \
      "it probes only: ${probes_standing:-none}"
  pass "$section" "$stage the four arms the graph-store spec moved stay ($four_standing), and the tuple arm still probes $probes_standing ($why)"
}

# view_arms_four_gone SECTION STAGE WHY: none of the four arms stands, and the tuple arm probes none
# of their tables; a failure names each that stands.
view_arms_four_gone() {
  local section="$1" stage="$2" why="$3"
  view_arms_four "$section"
  view_arms_probes "$section"
  [[ -z "$four_standing" ]] ||
    fail "$section" "$stage the four arms the graph-store spec moved are gone ($why)" "standing: $four_standing"
  [[ -z "$probes_standing" ]] ||
    fail "$section" "$stage the tuple arm's join for the four arms' kinds is gone ($why)" "it still probes: $probes_standing"
  pass "$section" "$stage the four arms the graph-store spec moved are gone, and the tuple arm probes none of ${four_probes[*]} ($why)"
}

# view_arms_other_announcement SECTION STAGE [COMMIT]: the newest announcement naming a build other
# than COMMIT, by default this head's, which the step of the four arms of a process serving COMMIT
# reads: its commit in other_commit, its kinds, comma-joined, in other_kinds.
view_arms_other_announcement() {
  query "$1" "read the newest announcement of another build" "
    select coalesce((select a.commit || '|' || coalesce(array_to_string(a.tuple_kinds, ','), '')
                     from server_build_announcements a where a.commit <> $(lit "${3:-$head_commit}")
                     order by a.seq desc limit 1), '|')"
  other_commit="${answer%%|*}" other_kinds="${answer#*|}"
  printf '%s the newest announcement of another build names %s and lists: %s\n' "$2" \
    "${other_commit:-no build}" "${other_kinds:-no kind}"
}

# view_arms_lists_four SECTION WHAT KINDS: KINDS, comma-joined, lists every kind of the four arms.
view_arms_lists_four() {
  local kind missing=""
  for kind in "${four_kinds[@]}"; do
    [[ ",$3," == *",$kind,"* ]] || missing+=" $kind"
  done
  [[ -z "$missing" ]] || fail "$1" "$2 lists the four arms' kinds, ${four_kinds[*]}" "it lacks:${missing}; it lists: ${3:-none}"
  pass "$1" "$2 lists the four arms' kinds, ${four_kinds[*]}"
}

# view_arms_current SECTION FROM_KIND FROM_ID KIND: the far ends of the current KIND tuples out of
# that node - the newest tuple of each relation, kept while it asserts - comma-joined, in answer.
view_arms_current() {
  query "$1" "read the current $4 tuples out of $2 $3" "
    select coalesce(string_agg(to_id, ',' order by to_id), '(none)') from (
      select distinct on (to_kind, to_id) to_id, asserted from graph_tuples
      where from_kind = $(lit "$2") and from_id = $(lit "$3") and kind = $(lit "$4")
      order by to_kind, to_id, seq desc) newest
    where asserted"
}

# --- setup: the release before the graph store over an emptied database ----------------------------

section=setup
start_fake "fake GitHub" fake-github.ts
readonly github_port="$fake_port"
# A throwaway App key, made for this run alone and printed nowhere: every server that imports a
# component signs its calls to the fake with it. Its base64 is one line, as BASELINE_EXTRA_ENV
# takes a value, and both releases read DISPATCH_APP_PEM_B64 as the base64 of a PEM.
app_pem="$(openssl genrsa 2048 2>/dev/null | base64 -w0)" ||
  fail "$section" "make a throwaway GitHub App key" "openssl genrsa or base64 failed"
[[ -n "$app_pem" ]] || fail "$section" "make a throwaway GitHub App key" "openssl genrsa printed nothing"
readonly github_env="DISPATCH_APP_CLIENT_ID=Iv1.e2efake"$'\n'"DISPATCH_APP_PEM_B64=$app_pem"$'\n'"DISPATCH_GITHUB_API_BASE=http://127.0.0.1:$github_port"
unset app_pem
# The bearer every server accepts, the run's DISPATCH_TOKEN; printed nowhere.
# shellcheck disable=SC2153 # DISPATCH_TOKEN is the caller's input, checked by name above
token="$DISPATCH_TOKEN"
session="view-arms-rollback-$(date +%s)-$$"
readonly session
tree_init
# The release before the graph store is the baseline lib-baseline.sh builds and boots:
# baseline_start is called with BASELINE_SHA set to PRE_1149_SHA.
view_arms_boot_earlier "$section" "$pre_commit"

# --- before: the relations the release before writes ---------------------------------------------

section=before
view_arms_fixture "$section"
view_arms_captured "$section" "the release before's graph_edges" "$work/dates.before" "${upgrade_edges[@]}"
view_arms_counts "$section" "the release before:"
readonly before_target="$count_target" before_parent="$count_parent" before_moved_to="$count_moved_to"
# lib-baseline.sh stamped the build with its commit, so the server registers it; it is the only
# live process, so its own startup.Announcer records the announcement that names that commit. No
# announcement is written here by SQL.
query "$section" "read the live processes" \
  "select count(*) || '|' || coalesce(string_agg(commit, ','), '') from server_processes"
[[ "$answer" == "1|$pre_commit" ]] ||
  fail "$section" "the release before registers PRE_1149_SHA $pre_commit in server_processes as the only live process" \
    "server_processes holds: $answer"
pass "$section" "the release before registers PRE_1149_SHA $pre_commit in server_processes as the only live process"
tree_wait_announcement "$section" "$pre_commit"
baseline_stop_server
pass "$section" "stopped the release before"

# --- held: this head over those rows, before its backfill -------------------------------------------

section=held
view_arms_boot head-held "$before_backfill_holds"
base="${head_url[head-held]}"
view_arms_load "$section" "before the backfill:"
for edge in "${upgrade_edges[@]}"; do
  view_arms_once "$section" "before the backfill:" old "$edge" "written by the release before with no node"
done
view_arms_as_dated "$section" "before the backfill:" "$work/dates.before" "${upgrade_edges[@]}"
view_arms_link_kinds "$section" "before the backfill:" "$stamped"
view_arms_refs "$section" "before the backfill:" 1 "${upgrade_edges[@]}"
view_arms_want_counts "$section" "before the backfill:" "$before_target" "$before_parent" "$before_moved_to" \
  "as the release before read them"
view_arms_stop head-held

# --- upgrade: this head's first boot that runs the backfill -----------------------------------------

section=upgrade
view_arms_boot head-a
base="${head_url[head-a]}"
tree_wait_backfill "$section" "${head_log[head-a]}"
view_arms_wait_steps "$section" head-a "$step_kept"
view_arms_other_announcement "$section" "after the upgrade:"
[[ "$other_commit" == "$pre_commit" && -z "$other_kinds" ]] ||
  fail "$section" "after the upgrade the newest announcement of another build is PRE_1149_SHA's, which lists no kind" \
    "it names ${other_commit:-no build} and lists: ${other_kinds:-no kind}"
view_arms_four_stay "$section" "after the upgrade:" "the newest announcement of another build, PRE_1149_SHA's, lists none of their kinds"
view_arms_load "$section" "after the upgrade:"
for edge in "${upgrade_edges[@]}"; do
  view_arms_once "$section" "after the upgrade:" store "$edge" "stamped by this head's backfill"
done
view_arms_as_dated "$section" "after the upgrade:" "$work/dates.before" "${upgrade_edges[@]}"
view_arms_link_kinds "$section" "after the upgrade:" "$stamped"
view_arms_refs "$section" "after the upgrade:" 1 "${upgrade_edges[@]}"
view_arms_want_counts "$section" "after the upgrade:" "$before_target" "$before_parent" "$before_moved_to" \
  "as the release before read them"

# --- beside: a second process of this head ------------------------------------------------------------

section=beside
view_arms_boot head-b
base="${head_url[head-b]}"
tree_wait_backfill "$section" "${head_log[head-b]}"
view_arms_wait_steps "$section" head-b "$step_kept"
view_arms_four_stay "$section" "beside the first process:" "the newest announcement of another build still lists none of their kinds"
view_arms_load "$section" "beside the first process:"
for edge in "${upgrade_edges[@]}"; do
  view_arms_once "$section" "beside the first process:" store "$edge"
done
view_arms_as_dated "$section" "beside the first process:" "$work/dates.before" "${upgrade_edges[@]}"
view_arms_refs "$section" "through the second process:" 1 "${upgrade_edges[@]}"
view_arms_want_counts "$section" "through the second process:" "$before_target" "$before_parent" "$before_moved_to" \
  "as the release before read them"

# --- rollback: what a build of the release before writes and deletes after a rollback --------------

section=rollback
base="${head_url[head-a]}"
# A document this head uploads cites C, a citation its writer states in the store.
view_arms_upload "$section" "$deleted_link" cited.md "Cites dispatch://$child as the issue this document names."$'\n'
cited_id="$uploaded_id"
readonly cited_edge="artifact $cited_id mentions issue $child"
view_arms_wait_edge "$section" "$cited_edge"
view_arms_load "$section" "before the rollback:"
view_arms_once "$section" "before the rollback:" store "$cited_edge" "a citation this head stated in the store"
# By SQL, as a build of the release before writes and deletes: every row it writes names no node,
# every row it deletes leaves its tuple standing, and an edge column it updates leaves the row's
# node columns as they were. The release before's issue PATCH moves a parent in place so; no writer
# of it moves a component's parent in place, since its importer deletes a source's components and
# inserts them again, so the component's move stands for a later writer that updates the column.
psql_run "$section" "write and delete rows as a build of the release before does" \
  -v p="$project" -v target="$target" -v waiter="$waiter" -v child="$child" -v parent="$parent" \
  -v deleted_link="$deleted_link" -v deleted_parent="$deleted_parent" -v moved="$moved" -v moved_to="$moved_to" <<'SQL'
insert into issue_links (issue_key, kind, target_key, created_at) values (:'waiter', 'blocked_by', :'target', now() - interval '1 day');
update issues set parent_key = :'parent', node_kind = null, node_address = null where key = :'child';
delete from issue_links where issue_key = :'deleted_link' and target_key = :'target';
update issues set parent_key = null where key = :'deleted_parent';
update issues set parent_key = :'moved_to' where key = :'moved';
insert into components (project_key, id, title, prose, parent, snapshot_id, source_id)
  select project_key, 'relay', 'Relay', 'The relay.', 'legion', snapshot_id, source_id
  from components where project_key = :'p' and id = 'legion';
insert into component_depends (project_key, from_id, to_id) values (:'p', 'daemon', 'listener');
delete from components where project_key = :'p' and id = 'workspace';
delete from component_depends where project_key = :'p' and from_id = 'server' and to_id = 'listener';
update components set parent = 'daemon' where project_key = :'p' and id = 'listener';
insert into issue_components (issue_key, mode) values (:'waiter', 'explicit');
insert into issue_component_members (issue_key, project_key, component_id) values (:'waiter', :'p', 'envoy');
delete from issue_component_members where issue_key = :'deleted_link';
SQL
with_vars "$section" "delete the refs row of $cited_id's citation of $child as a build of the release before does" "
  with gone as (
    delete from refs where from_kind = 'artifact' and from_id = :'cited' and to_kind = 'issue' and to_id = :'child'
    returning 1)
  select count(*) from gone" "cited=$cited_id" "child=$child"
[[ "$answer" == 1 ]] ||
  fail "$section" "delete the refs row of $cited_id's citation of $child" "it deleted $answer rows"
pass "$section" "wrote and deleted the rows a build of the release before writes and deletes, by SQL"

rollback_store=(
  "issue $stamped blocked_by issue $target"
  "issue $stamped child_of issue $parent"
  "component $project/daemon part_of component $project/legion"
  "component $project/envoy part_of component $project/legion"
  "component $project/server part_of component $project/envoy"
  "component $project/daemon depends_on component $project/envoy"
  "issue $stamped affects component $project/daemon"
  "artifact $doc_id attached_to issue $stamped"
  "artifact $doc_id mentions issue $target"
)
rollback_old=(
  "issue $waiter blocked_by issue $target|a link the release before writes with no node"
  "issue $child child_of issue $parent|a parent the release before writes on a row naming no node"
  "issue $moved child_of issue $moved_to|$moved's parent, which the release before moved in place to $moved_to"
  "component $project/relay part_of component $project/legion|a component's parent the release before writes with no node"
  "component $project/listener part_of component $project/daemon|$project/listener's parent, moved in place to $project/daemon"
  "component $project/daemon depends_on component $project/listener|a component dependency the release before writes with no node"
  "issue $waiter affects component $project/envoy|an issue's component the release before writes with no node"
)
rollback_deleted=(
  "issue $deleted_link blocked_by issue $target|a stamped link the release before deleted with no retraction"
  "issue $deleted_parent child_of issue $parent|a stamped parent the release before cleared with no retraction"
  "issue $moved child_of issue $parent|the parent $moved left when the release before moved it in place"
  "component $project/workspace part_of component $project/daemon|a stamped component's parent the release before deleted with no retraction"
  "component $project/listener part_of component $project/envoy|the parent $project/listener left when it was moved in place"
  "component $project/server depends_on component $project/listener|a stamped component dependency the release before deleted with no retraction"
  "issue $deleted_link affects component $project/envoy|a stamped issue's component the release before deleted with no retraction"
  "$cited_edge|a citation the store states whose refs row the release before deleted with no retraction"
)
rollback_standing=("${rollback_store[@]}")
for entry in "${rollback_old[@]}"; do
  rollback_standing+=("${entry%%|*}")
done
rollback_gone=()
for entry in "${rollback_deleted[@]}"; do
  rollback_gone+=("${entry%%|*}")
done

for edge in "${rollback_store[@]}"; do
  view_arms_once "$section" "after the rollback:" store "$edge" "a stamped row the release before left alone"
done
for entry in "${rollback_old[@]}"; do
  view_arms_once "$section" "after the rollback:" old "${entry%%|*}" "${entry#*|}"
done
for entry in "${rollback_deleted[@]}"; do
  view_arms_nowhere "$section" "after the rollback:" "${entry%%|*}" "${entry#*|}"
done
view_arms_link_kinds "$section" "after the rollback:" "$stamped"
view_arms_link_kinds "$section" "after the rollback:" "$waiter"
view_arms_captured "$section" "graph_edges after the rollback" "$work/dates.rollback" "${rollback_standing[@]}"
for name in head-a head-b; do
  base="${head_url[$name]}"
  view_arms_refs "$section" "after the rollback, through $name:" 1 "${rollback_standing[@]}"
  view_arms_refs "$section" "after the rollback, through $name:" 0 "${rollback_gone[@]}"
  # T keeps as many waits (N's for D's), P loses E and X and gains C, and M gains X.
  view_arms_want_counts "$section" "after the rollback, through $name:" "$before_target" "$((before_parent - 1))" \
    "$((before_moved_to + 1))" "each standing relation once and each deleted one nowhere"
done

# --- restart: the second process again, whose boot backfill states the new values ----------------

section=restart
view_arms_stop head-b
view_arms_boot head-b-restarted
base="${head_url[head-b-restarted]}"
tree_wait_backfill "$section" "${head_log[head-b-restarted]}"
view_arms_wait_steps "$section" head-b-restarted "$step_kept"
view_arms_four_stay "$section" "after the restart:" "the newest announcement of another build still lists none of their kinds"
view_arms_load "$section" "after the restart:"
for edge in "${rollback_standing[@]}"; do
  view_arms_once "$section" "after the restart:" store "$edge" "the backfill states the row its marker did not mark"
done
for entry in "${rollback_deleted[@]}"; do
  view_arms_nowhere "$section" "after the restart:" "${entry%%|*}" "${entry#*|}"
done
# Each relation the old arms answered is dated as its old arm dated it: a row's own created_at, a
# component's snapshot's imported_at, an issue's components' updated_at.
view_arms_as_dated "$section" "after the restart:" "$work/dates.rollback" "${rollback_standing[@]}"
view_arms_current "$section" issue "$moved" child_of
[[ "$answer" == "$moved_to" ]] ||
  fail "$section" "after the restart $moved hangs under $moved_to alone, the parent the release before moved it to in place" \
    "its current child_of tuples name: $answer"
pass "$section" "after the restart $moved hangs under $moved_to alone: the backfill appended the tuple that names the new value"
view_arms_current "$section" component "$project/listener" part_of
[[ "$answer" == "$project/daemon" ]] ||
  fail "$section" "after the restart $project/listener hangs under $project/daemon alone, the parent it was moved to in place" \
    "its current part_of tuples name: $answer"
pass "$section" "after the restart $project/listener hangs under $project/daemon alone"
view_arms_refs "$section" "after the restart:" 1 "${rollback_standing[@]}"
view_arms_refs "$section" "after the restart:" 0 "${rollback_gone[@]}"
view_arms_want_counts "$section" "after the restart:" "$before_target" "$((before_parent - 1))" \
  "$((before_moved_to + 1))" "each standing relation once and each deleted one nowhere"

# --- later: the first boot of a later build, after this head's own announcement ---------------------

section=later
# This head wrote its own announcement once head-a was the only live build (the upgrade section), and
# every process since serves the same commit, so that announcement, which lists the four arms' kinds,
# is the newest; a build of another commit reads it as the newest announcement of another build.
tree_wait_announcement "$section" "$head_commit"
query "$section" "read the kinds the newest announcement lists" \
  "select coalesce(array_to_string(tuple_kinds, ','), '') from server_build_announcements order by seq desc limit 1"
view_arms_lists_four "$section" "the announcement of this head, $head_commit" "$answer"
view_arms_stop head-a
view_arms_stop head-b-restarted
pass "$section" "stopped every process of this head"
# This head again, stamped with another commit: the checkout's HEAD^, as issue-room-cutover.sh stamps
# its second build. It stands for a later build, which runs this step as this head does.
later_commit="$(git -C "$repo" rev-parse --verify --quiet HEAD^)" ||
  fail "$section" "read another commit to stamp the later build with" "git rev-parse HEAD^ failed in $repo"
[[ "$later_commit" != "$head_commit" && "$later_commit" != "$pre_commit" ]] ||
  fail "$section" "the later build carries a commit of its own" "HEAD^ is $later_commit, which this head or PRE_1149_SHA already serves"
readonly later_commit
mkdir -p "$work/bin"
go -C "$repo/packages/envoy" build -ldflags "-X main.buildCommit=$later_commit" -o "$work/bin/envoy-dispatch-later" ./cmd/dispatch \
  >"$work/build-later" 2>&1 || fail "$section" "build this head again stamped with $later_commit" "$(tail -n 20 "$work/build-later")"
pass "$section" "built this head again stamped with $later_commit, the later build"
view_arms_boot head-later "" "$work/bin/envoy-dispatch-later" "$later_commit"
base="${head_url[head-later]}"
tree_wait_backfill "$section" "${head_log[head-later]}"
view_arms_wait_steps "$section" head-later "$step_deleted"
view_arms_other_announcement "$section" "at the later build's first boot:" "$later_commit"
[[ "$other_commit" == "$head_commit" ]] ||
  fail "$section" "at the later build's first boot the newest announcement of another build is this head's, $head_commit" \
    "it names ${other_commit:-no build}"
view_arms_lists_four "$section" "the newest announcement of another build" "$other_kinds"
view_arms_four_gone "$section" "at the later build's first boot:" "the newest announcement of another build, this head's, lists their kinds"
view_arms_load "$section" "at the later build's first boot:"
for edge in "${rollback_standing[@]}"; do
  view_arms_once "$section" "at the later build's first boot:" store "$edge" "answered by the store once the four arms are gone"
done
for entry in "${rollback_deleted[@]}"; do
  view_arms_nowhere "$section" "at the later build's first boot:" "${entry%%|*}" "${entry#*|}"
done
view_arms_as_dated "$section" "at the later build's first boot:" "$work/dates.rollback" "${rollback_standing[@]}"
view_arms_link_kinds "$section" "at the later build's first boot:" "$stamped"
view_arms_refs "$section" "at the later build's first boot:" 1 "${rollback_standing[@]}"
view_arms_refs "$section" "at the later build's first boot:" 0 "${rollback_gone[@]}"
view_arms_want_counts "$section" "at the later build's first boot:" "$before_target" "$((before_parent - 1))" \
  "$((before_moved_to + 1))" "each standing relation once and each deleted one nowhere"

# --- scale: the plan of a graph_edges read of one issue ---------------------------------------------

section=scale
view_arms_stop head-later
pass "$section" "stopped the later build"
query "$section" "read the node $stamped names" \
  "select coalesce(node_kind || ' ' || node_address, '') from issues where key = $(lit "$stamped")"
[[ "$answer" == "issue $stamped" ]] || fail "$section" "$stamped names its node" "its row names '$answer'"
query "$section" "read the node component legion names" \
  "select coalesce(node_address, '') from components where project_key = $(lit "$project") and id = 'legion'"
[[ "$answer" == "$project/legion" ]] || fail "$section" "component legion names its node" "its row names '$answer'"
# The Go half's scale, by SQL: 500 issues, each but the first a child of the issue at half its
# number and each waiting on the 100 after it; 5,000 components, each under the one at half its
# number and each depending on the 10 after it; and 100 components each issue holds. The rows of an
# even issue and of an even component name their node, made as an existing one's, beside the tuple
# each projects; an odd one's name none, as the release before writes them.
psql_run "$section" "seed the scale of the plan" -v p="$project" -v issue_node="$stamped" \
  -v component_node="$project/legion" <<'SQL'
insert into issues (key, project_key, number, title, created_by, rank, parent_key, node_kind, node_address)
select :'p' || '-' || (1000 + n), :'p', 1000 + n, 'Bulk issue ' || n, jsonb_build_object('kind', 'user', 'id', 'alice'),
       'z' || lpad(n::text, 4, '0'),
       case when n > 1 then :'p' || '-' || (1000 + n / 2) end,
       case when n % 2 = 0 then 'issue' end, case when n % 2 = 0 then :'p' || '-' || (1000 + n) end
from generate_series(1, 500) n;
insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event, project_key, parties)
select node.kind, i.node_address, node.supertag, node.visibility, node.state, node.attributes, node.made_at, node.made_by, 0,
       node.project_key, node.parties
from issues i join graph_nodes node on node.kind = 'issue' and node.id = :'issue_node'
where i.project_key = :'p' and i.number > 1000 and i.node_kind is not null;
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
select 'issue', i.key, 'child_of', 'issue', i.parent_key, i.created_at, jsonb_build_object('kind', 'user', 'id', 'alice'), 0, true
from issues i
where i.project_key = :'p' and i.number > 1000 and i.node_kind is not null and i.parent_key is not null;
insert into issue_links (issue_key, kind, target_key, created_at, node_kind, node_address)
select i.key, 'blocked_by', :'p' || '-' || (1001 + (i.number - 1001 + k) % 500), now() - interval '1 day', i.node_kind, i.node_address
from issues i cross join generate_series(1, 100) k
where i.project_key = :'p' and i.number > 1000;
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
select 'issue', l.issue_key, 'needs', 'issue', l.target_key, l.created_at, jsonb_build_object('kind', 'user', 'id', 'alice'), 0, true
from issue_links l join issues i on i.key = l.issue_key
where i.project_key = :'p' and i.number > 1000 and l.node_kind is not null;
insert into components (project_key, id, title, prose, parent, snapshot_id, source_id, node_kind, node_address)
select :'p', 'bulk-' || n, 'Bulk ' || n, 'A bulk component.', case when n > 1 then 'bulk-' || (n / 2) else 'legion' end,
       legion.snapshot_id, legion.source_id,
       case when n % 2 = 0 then 'component' end, case when n % 2 = 0 then :'p' || '/bulk-' || n end
from components legion cross join generate_series(1, 5000) n
where legion.project_key = :'p' and legion.id = 'legion';
insert into graph_nodes (kind, id, supertag, visibility, state, attributes, made_at, made_by, source_event, project_key, parties)
select node.kind, c.node_address, node.supertag, node.visibility, node.state, node.attributes, node.made_at, node.made_by, 0,
       node.project_key, node.parties
from components c join graph_nodes node on node.kind = 'component' and node.id = :'component_node'
where c.project_key = :'p' and c.id like 'bulk-%' and c.node_kind is not null;
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
select 'component', c.node_address, 'part_of', 'component', :'p' || '/' || c.parent, s.imported_at,
       jsonb_build_object('kind', 'user', 'id', 'alice'), 0, true
from components c join architecture_snapshots s on s.id = c.snapshot_id
where c.project_key = :'p' and c.id like 'bulk-%' and c.node_kind is not null;
insert into component_depends (project_key, from_id, to_id, node_kind, node_address)
select :'p', 'bulk-' || n, 'bulk-' || (1 + (n - 1 + k) % 5000),
       case when n % 2 = 0 then 'component' end, case when n % 2 = 0 then :'p' || '/bulk-' || n end
from generate_series(1, 5000) n cross join generate_series(1, 10) k;
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
select 'component', d.node_address, 'depends_on', 'component', :'p' || '/' || d.to_id, s.imported_at,
       jsonb_build_object('kind', 'user', 'id', 'alice'), 0, true
from component_depends d
join components c on c.project_key = d.project_key and c.id = d.from_id
join architecture_snapshots s on s.id = c.snapshot_id
where d.project_key = :'p' and d.from_id like 'bulk-%' and d.node_kind is not null;
insert into issue_components (issue_key, mode)
select i.key, 'explicit' from issues i where i.project_key = :'p' and i.number > 1000;
insert into issue_component_members (issue_key, project_key, component_id, node_kind, node_address)
select i.key, :'p', 'bulk-' || (1 + ((i.number - 1001) * 100 + k - 1) % 5000), i.node_kind, i.node_address
from issues i cross join generate_series(1, 100) k
where i.project_key = :'p' and i.number > 1000;
insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event, asserted)
select 'issue', m.issue_key, 'affects', 'component', m.project_key || '/' || m.component_id, c.updated_at,
       jsonb_build_object('kind', 'user', 'id', 'alice'), 0, true
from issue_component_members m join issue_components c using (issue_key)
where m.project_key = :'p' and m.component_id like 'bulk-%' and m.node_kind is not null;
analyze;
SQL
query "$section" "count the rows the scale wrote" "
  select (select count(*) from issue_links l join issues i on i.key = l.issue_key where i.project_key = $(lit "$project") and i.number > 1000)
         || ' issue links, ' || (select count(*) from component_depends where project_key = $(lit "$project") and from_id like 'bulk-%')
         || ' component dependencies, ' || (select count(*) from issue_component_members where project_key = $(lit "$project") and component_id like 'bulk-%')
         || ' issue components at ' || clock_timestamp()"
pass "$section" "seeded and analyzed: $answer"
readonly scale_issue="$project-1250"
# The read of one issue, a row this head's writers leave: graph_edges answers its child_of, its 100
# waits and its 100 components out of it, and its one child and the 100 waits on it, 50 by the old
# blocked_by arm and 50 by the tuple arm, into it.
for reading in 'from {"affects":100,"blocked_by":100,"child_of":1}' 'to {"blocked_by":100,"child_of":1}'; do
  side="${reading%% *}" want="${reading#* }"
  query "$section" "count the edges $side $scale_issue" "
    select coalesce(json_object_agg(kind, n), '{}') from (
      select kind, count(*) n from graph_edges where ${side}_kind = 'issue' and ${side}_id = $(lit "$scale_issue") group by kind) k"
  got="$(jq -cS . <<<"$answer")" || fail "$section" "count the edges $side $scale_issue" "no JSON: $answer"
  [[ "$got" == "$want" ]] || fail "$section" "graph_edges answers the edges $side $scale_issue by kind as $want" "it answers $got"
  pass "$section" "graph_edges answers the edges $side $scale_issue by kind as $want"
done
readonly scanned_filter='[.. | objects | select((."Node Type" // "") | test("Seq Scan$")) | (."Relation Name" // empty)] | unique | join(",")'
# The search the plans are read with, run first over a plan that does read issue_links whole, so a
# search that could find nothing reads as broken, not as a clean plan.
query "$section" "take the plan of a read of issue_links whole" "explain (format json) select * from issue_links"
scanned="$(jq -r "$scanned_filter" <<<"$answer")" || fail "$section" "read the plan of a read of issue_links whole" "no JSON: $(head -c 600 <<<"$answer")"
[[ ",$scanned," == *,issue_links,* ]] ||
  fail "$section" "the Seq Scan search finds issue_links in the plan of a read of it whole" "it finds: ${scanned:-nothing}"
pass "$section" "the Seq Scan search finds issue_links in the plan of a read of it whole"
for side in from to; do
  query "$section" "take the plan of the read of the edges $side $scale_issue" "
    explain (analyze, buffers, format json)
    select from_kind, from_id, kind, to_kind, to_id, created_at, source_seq from graph_edges
    where ${side}_kind = 'issue' and ${side}_id = $(lit "$scale_issue")"
  printf '%s\n' "$answer" >"$work/plan.$side.json"
  scanned="$(jq -r "$scanned_filter" "$work/plan.$side.json")" ||
    fail "$section" "read the plan of the read of the edges $side $scale_issue" "no JSON: $(head -c 600 "$work/plan.$side.json")"
  printf 'the plan of the read of the edges %s %s reads by a Seq Scan: %s\n' "$side" "$scale_issue" "${scanned:-nothing}"
  whole=""
  for table in issues issue_links components component_depends issue_component_members; do
    [[ ",$scanned," != *",$table,"* ]] || whole+=" $table"
  done
  [[ -z "$whole" ]] ||
    fail "$section" "the plan of the read of the edges $side $scale_issue reads no relation table whole" \
      "it reads${whole} by a Seq Scan: $(head -c 600 "$work/plan.$side.json")"
  pass "$section" "the plan of the read of the edges $side $scale_issue reads none of issues, issue_links, components, component_depends and issue_component_members by a Seq Scan"
done

# --- empty: a database this head creates ------------------------------------------------------------

section=empty
tree_reset_database "$database_url" "$database_names" "$section"
view_arms_boot head-empty
base="${head_url[head-empty]}"
tree_wait_backfill "$section" "${head_log[head-empty]}"
view_arms_wait_steps "$section" head-empty "$step_deleted"
view_arms_four_gone "$section" "over a database this boot created:" "this boot created the database from empty"
view_arms_load "$section" "over a database this boot created:"

printf 'PASS: %s complete 10 sections\n' "$driver"
