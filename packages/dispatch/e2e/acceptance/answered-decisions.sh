#!/usr/bin/env bash
# The check c-answers-restored: the restore returns every answered ask the release
# before let leave its spec, in both shapes, over rows that release wrote, and an agent can no
# longer take an answered ask's block out of a document.
#
# It drops and creates the database DATABASE_URL names (seed_reset_database: a database on this
# machine whose name begins infra_1150_), boots the base release on it through lib-baseline.sh
# (BASELINE_BINARY, default $TMPDIR/base-dispatch, the stamped build of BASELINE_SHA), with the dev
# sign-in, the pinned agent bearer e2e-token and the hold settings (DISPATCH_TEST_HOOKS=1,
# DISPATCH_TEST_HOLD_DECISIONS_STEP=1 and DISPATCH_TEST_HOLD_BOOT_BACKFILL=1, through
# BASELINE_EXTRA_ENV), and has seed-decisions.sh
# write the corpus through that server's own API. Through the same server it gives issue D a
# questions.md holding an ask the person answers, and has an agent put a copy of that ask's block
# into spec D, which the base settles as a copy no row names; then it closes issue D, so one issue
# the restore owes is closed.
#
# BASELINE_SHA, the commit of the release this branch lands after, is required and has no
# default: only the caller knows which release that is. Once the base server has booted, the
# commit it is stamped with (the `commit` of its /healthz, the one it registers in
# server_processes) must be the commit BASELINE_SHA names in this checkout, or the run fails
# naming both, so that a binary built from another commit never stands in for that release. In
# the same way this head's server must be stamped with the commit this checkout stands on (`git
# rev-parse HEAD`, which the Verification preamble stamps HEAD_BINARY with), so that a binary left
# from an earlier head never stands in for the head the result is recorded at.
#
# Before this head boots it captures, through the base server: each restored ask's id, question,
# options, answer and answerer (GET /api/v1/asks/{id}); the spec blocks the base's Decisions read
# links each ask's section to (`links[].to`); every version of the spec and the decisions document
# of each issue the restore owes (GET /api/v1/artifacts/{id}/versions/{n}); the text the citation
# of the second shape's old addresses answers (GET /api/v1/references?to=), and that of the main
# shape's colliding old spec address; and the block_refs rows the decisions document holds for the
# second shape's ask blocks and the blocks each holds (document_blocks.parent_block_id), among
# them the citation of an issue the seed writes in each one's question, and it fails when an ask
# holds none. It then stops that server and boots this head, HEAD_BINARY (default
# $TMPDIR/bin/envoy-dispatch, built stamped as the Verification preamble builds it), over the same
# database with the hold settings, so the commands below and not the boot make the first move:
# neither the decisions step nor the boot's run of the backfill states a row before them.
#
# It runs `envoy-dispatch backfill-graph-store` (the step's first phase, which the census and the
# restore read an answer from) and the store census (`envoy-dispatch census --store`), which must
# list each spec whose answered asks left it with exactly those asks; then the restore,
# `envoy-dispatch restore-answered-asks --project SEED`, and the census again, which must list
# list none. For each restored ask it reads, at this head: the same ask id, its answer and answerer
# character for character as captured; its block in the spec's `## Decisions` part, in answer-time
# order - which is not the order of their blocks, since the capture reads q-left-format answered
# before q-left-store, whose block stood above it - the pointer line gone; that block's question
# and options character for character as the captured row, and no answer in its markdown (the
# stored version's); for the main shape its old block id and no moved_to tuple naming it, and for
# the second shape one moved_to tuple from its old address to its new one; one current shaped tuple
# from the ask's node to each spec node captured from its section's links, and no other; and no
# copied_from on its block. It reads every ask block of each version the restore wrote - the
# versions its line of each issue names (spec_version, decisions_version) and counts
# (other_versions), as the artifact.version events the step appends in its own name name them,
# every such event after the restore started read with no limit and held to as many as the lines
# count - in both shapes, and each holds the form R10 keeps, each value equal to its ask's node as
# the store states it: `state` where the ask is answered or resolved and none where it is open,
# `answered_by` and `answered_at` where it is answered, and
# `copied_from` and `copied_from_document` on a copy of an ask of another document; a block that
# has no ask row and is no copy of an ask holds none of the eight server-owned attributes; and no
# block holds `selected`, `answer` or `invalid`. A block that differs fails the run, which prints
# the block, its attributes and the ones its ask's node gives. Spec D's version holds the copy of
# the answered ask, and the run fails unless a version the restore wrote holds a copy of an
# answered ask whose kept form it read. It requires no block that has no ask row and is no copy:
# no release's API makes one, and the Go test
# TestEveryVersionTheRestoreWritesHoldsNoneOfTheEightOnABlockWithNoAskRowThatIsNoCopy holds that
# half. It reads every captured version of
# both documents answering the captured bytes. For the second shape it reads the two documents
# move together: the decisions document's block index and block_refs hold nothing of the moved
# asks or of a block they held, and the spec's hold each ask and each captured edge, an ask block's
# own under its new id and a held block's under a block the restored ask block holds. For both
# collisions it reads the restored ask's new id, held by no other document of the issue, in the
# asks row asks_block_id_unique keys; for the second shape the old address still resolving through
# its one moved_to tuple to the captured text, and for the main shape no moved_to tuple and the old
# spec address answering the live block that holds that id, as captured. A text is held to the
# captured one character for character with two differences and no other, each printed: the
# attributes R10 takes out of markdown, `selected` and `answer`, are absent, and a block that took
# a new id carries it; its `state`, `answered_by` and `answered_at`, which R10 keeps, written from
# the same ask node, equal the captured ones, each printed. It runs the restore again and reads
# the same result for the closed issue, and for every other.
#
# Then, on the spec, which still holds the answered ask q-kept, it makes three writes as the agent
# - a delete of that block, a whole-document replacement without it, and a retype of it - and reads
# each refused 409 ANSWERED_ASK_KEPT, quoting the ask's question and naming its node and its block,
# the document's version unchanged and the answer still in place; then three such writes as the
# person, each dropping or retyping an answered ask's block, which are taken.
#
# Then the step on its own tick. It stops this head and boots the base release again, which writes
# a project of its own, PERSON, whose one issue's spec holds an ask the person answers, beside a
# decisions.md holding one section, which cites that ask; and an issue of SEED whose answered ask
# an agent's session, with no act of a person, drops from its spec. It stops the base and boots
# this head held again, runs the step's first phase, and, as the person, puts a paragraph in place
# of the answered ask's block of the PERSON spec, a write that leaves one removed_by tuple to the
# person. It restarts this head without the hold settings, so the step runs on its own tick, and
# waits three tick intervals (docs.BlockIndexReconcileInterval, read from its source) after the
# first tick's report line. Out of the store and the report lines of every tick it reads the PERSON
# spec holding no block of that ask, its newest version the person's, and no restore of it in any
# report - a failure names the ask and the version that wrote its block back; the section deleted
# by R9's rule for it, its heading address answering the ask through one moved_to tuple, the
# decisions.md retired, and /healthz's decisions_step_owed zero; the SEED ask the agent dropped
# restored in the same run, the version that dropped it read as a session with no act of a person;
# and every ask block of each version the step wrote on its ticks, each an artifact.version event
# it appended in its own name, in the form R10 keeps, as for the versions the restore wrote.
#
# Last it stops this head and runs the Go test that carries a drop by a broker token the secrets
# broker scoped to a person, which no shell driver can present, since that token signs a DPoP proof
# for each request: `go test -tags basebuild -count=1 -v -timeout 30m -run
# '^TestADropByAPersonScopedBrokerTokenUnderTheBaseStays$' ./cmd/dispatch` in packages/envoy, with
# BASELINE_SHA and the test database server, DISPATCH_TEST_DATABASE_URL or else LEGION_TEST_PG_DSN,
# passed through. It requires the test's `--- PASS:` line and fails on any `--- SKIP` line, printing
# the test's output.
#
# Each passing check prints `PASS: <section> <what>`; the first failing one prints
# `FAIL: <section> <what>: <observed>` on stderr and the run exits 1; a missing input exits 2. It
# prints no SKIP line. Every check is `cmd || fail …`, never part of an `&&` list.
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DATABASE_URL=${LEGION_TEST_PG_DSN%/*}/infra_1150_answered_decisions?sslmode=disable \
         BASELINE_SHA=<the commit of the release this branch lands after> \
         [DISPATCH_TEST_DATABASE_URL=$LEGION_TEST_PG_DSN] \
         [BASELINE_BINARY=$TMPDIR/base-dispatch] [HEAD_BINARY=$TMPDIR/bin/envoy-dispatch] \
         bash e2e/acceptance/answered-decisions.sh
       (run from packages/dispatch; DATABASE_URL names a database on this machine whose name
       begins infra_1150_, which this run drops and creates again. BASELINE_SHA is required and
       has no default; BASELINE_BINARY must be the server built from it and stamped with it.
       The Go test it runs creates a database of its own on the server DISPATCH_TEST_DATABASE_URL
       names, or else LEGION_TEST_PG_DSN, one of which is required)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
# shellcheck source-path=SCRIPTDIR source=lib-baseline.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib-baseline.sh"
# shellcheck source-path=SCRIPTDIR source=seed-decisions.sh
. "$(dirname "${BASH_SOURCE[0]}")/seed-decisions.sh"

require_env DATABASE_URL TMPDIR BASELINE_SHA
require_commands cmp curl git go jq paste psql sed sort
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
# The Go module of this checkout, which the broker-token test is run from.
envoy_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../../envoy" && pwd)"
readonly envoy_dir
# The server the broker-token test creates its own database on (storetest's
# DISPATCH_TEST_DATABASE_URL, which the Verification preamble gives LEGION_TEST_PG_DSN's value).
test_database_url="${DISPATCH_TEST_DATABASE_URL:-${LEGION_TEST_PG_DSN:-}}"
[[ -n "$test_database_url" ]] || {
  printf 'neither DISPATCH_TEST_DATABASE_URL nor LEGION_TEST_PG_DSN is set: the broker-token test needs a database server\n' >&2
  usage
  exit 2
}
readonly test_database_url
# The step's tick interval, docs.BlockIndexReconcileInterval, as its source states it.
tick_seconds="$(sed -nE 's/^[[:space:]]*BlockIndexReconcileInterval[[:space:]]*=[[:space:]]*([0-9]+) \* time\.Second$/\1/p' \
  "$envoy_dir/internal/dispatch/docs/block_index.go")"
[[ "$tick_seconds" =~ ^[1-9][0-9]*$ ]] || {
  printf 'no BlockIndexReconcileInterval of whole seconds in %s\n' "$envoy_dir/internal/dispatch/docs/block_index.go" >&2
  exit 2
}
readonly tick_seconds
# The commit BASELINE_SHA names, in full: the one the base server booted below must be stamped with.
base_sha="$(git -C "$(dirname -- "${BASH_SOURCE[0]}")" rev-parse --verify --quiet "${BASELINE_SHA}^{commit}")" || {
  printf 'BASELINE_SHA %s names no commit of this checkout\n' "$BASELINE_SHA" >&2
  exit 2
}
readonly base_sha
# The commit this checkout stands on, in full: the one this head's server must be stamped with.
head_sha="$(git -C "$(dirname -- "${BASH_SOURCE[0]}")" rev-parse --verify --quiet HEAD)" || {
  printf 'this checkout stands on no commit (git rev-parse HEAD)\n' >&2
  exit 2
}
readonly head_sha
readonly project=SEED
# The eight server-owned ask attributes. R10 takes `selected` and `answer` out of markdown, keeps
# the others a version can store in the form keptAskState writes, and no rendering writes `invalid`.
readonly -a server_attributes=(state answered_by answered_at selected answer invalid copied_from copied_from_document)

work_dir
head_pid=""
base_pid=""
cleanup() {
  local code=$?
  baseline_stop_pid "$head_pid"
  baseline_stop_pid "$base_pid"
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
# `person` (the dev sign-in's cookie in $work/jar and the server's origin, which a cookie's write
# must name). The answer's body goes to $work/body and its status to $status.
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

# census SECTION NAME: `envoy-dispatch census --store --verbose`, which prints its lines whatever it
# refuses, into $work/NAME.out; printed with the run's own output.
census() {
  dispatch_command "$2" census --store --verbose
  [[ "$command_code" -le 1 ]] || fail "$1" "run the store census" "exited $command_code: $(<"$work/$2.out")"
  grep -E '^census: (decisions|spec_with_moved_answers|dangling_citations|collisions|store-only|store rows_checked)' "$work/$2.out" |
    sed 's/^/  /' || true
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

# latest_markdown SECTION DOCUMENT: the newest stored version of DOCUMENT, its number in $version
# and its markdown in $work/latest.md, read through the versions route.
latest_markdown() {
  send agent GET "/api/v1/artifacts/$2"
  want_status 200 "$1" "read document $2"
  field "$1" "the newest version of $2" '[.versions[].number] | max'
  version="$value"
  send agent GET "/api/v1/artifacts/$2/versions/$version"
  want_status 200 "$1" "read version $version of $2"
  jq -j '.markdown' "$work/body" >"$work/latest.md"
}

# document_token SECTION DOCUMENT: the token DOCUMENT carries now, the one `dispatch doc-read`
# prints, in $doc_token. An upload replacing DOCUMENT names it, as every replacement does.
document_token() {
  send agent GET "/api/v1/artifacts/$2/text"
  want_status 200 "$1" "read the text of $2"
  field "$1" "the token of $2" '.token'
  doc_token="$value"
}

# ask_block ID FILE: the markdown of the ask block ID in FILE, its fence and attributes included,
# on stdout; nothing where FILE holds no such block.
ask_block() {
  awk -v id="$1" '
    !open && match($0, /^:+ask\{#/) {
      rest = substr($0, RLENGTH + 1)
      if (index(rest, id " ") == 1 || index(rest, id "}") == 1) {
        open = 1; fence = substr($0, 1, index($0, "ask{") - 1); print; next
      }
    }
    open { print; if ($0 == fence) exit }
  ' "$2"
}

# block_body FILE: the lines between the first line and the closing fence of the block in FILE,
# nested blocks left out.
block_body() {
  awk 'NR == 1 { next } { lines[NR] = $0; last = NR } END {
    depth = 0
    for (n = 2; n < last; n++) {
      line = lines[n]
      if (line ~ /^:::+[a-z]+\{/) { depth++; continue }
      if (depth > 0) { if (line ~ /^:::+$/) depth--; continue }
      print line
    }
  }' "$1"
}

# expected_body ASK_FILE: the question and options of the captured ask row ASK_FILE as a block
# body: the question, and where it has options, a blank line and one `- label: description` line
# each.
expected_body() {
  jq -r '.question, (if (.options // []) | length > 0 then "", (.options[] | "- " + .label + (if (.description // "") == "" then "" else ": " + .description end)) else empty end)' "$1"
}

# attribute_of OPENING NAME: the attribute NAME as the opening line OPENING of an ask block holds
# it, `NAME="<value>"`, or `NAME=` and what follows up to a space or a brace where its value is not
# quoted; nothing where OPENING holds no such attribute.
attribute_of() {
  if [[ "$1" =~ \ $2=\"([^\"]*)\" ]]; then
    printf '%s="%s"' "$2" "${BASH_REMATCH[1]}"
  elif [[ "$1" =~ \ $2=([^ }]*) ]]; then
    printf '%s=%s' "$2" "${BASH_REMATCH[1]}"
  fi
}

# same_text SECTION WHAT CAPTURED GOT OLD_ID NEW_ID: GOT, the text a citation resolves to now,
# equals CAPTURED, the text it resolved to before the upgrade, character for character but for two
# differences, each printed: the attributes R10 takes out of markdown, `selected` and `answer`, are
# absent from GOT, and where NEW_ID is not OLD_ID the block carries NEW_ID in place of OLD_ID. GOT
# holds every other server-owned ask attribute as CAPTURED holds it - `state`, `answered_by` and
# `answered_at`, which R10 keeps, written from the same ask node - each printed. A `selected` or an
# `answer` still in GOT fails, as does a kept attribute that differs, and any other difference.
same_text() {
  local section="$1" what="$2" captured="$3" got="$4" old="$5" new="$6" name opening captured_opening want held
  printf '%s: the captured text:\n%s\n%s: the text now:\n%s\n' "$what" "$(<"$captured")" "$what" "$(<"$got")"
  opening="$(head -n 1 "$got")"
  captured_opening="$(head -n 1 "$captured")"
  cp "$captured" "$work/expected.txt"
  for name in "${server_attributes[@]}"; do
    want="$(attribute_of "$captured_opening" "$name")"
    held="$(attribute_of "$opening" "$name")"
    case "$name" in
      selected | answer)
        [[ -z "$held" ]] ||
          fail "$section" "$what" "the attribute $name, which R10 takes out of markdown, is still in the text: $opening"
        if [[ -n "$want" ]]; then
          printf '%s: allowed difference: the attribute %s is absent\n' "$what" "$want"
          sed -i -E "1s/ $name=\"[^\"]*\"//" "$work/expected.txt"
        fi
        ;;
      *)
        [[ "$held" == "$want" ]] ||
          fail "$section" "$what" "the text holds ${held:-no $name} and the captured text ${want:-no $name}: R10 keeps it, written from the same ask node"
        [[ -z "$want" ]] || printf '%s: kept as captured: %s\n' "$what" "$want"
        ;;
    esac
  done
  if [[ "$old" != "$new" ]]; then
    printf '%s: allowed difference: the block took the id %s in place of %s\n' "$what" "$new" "$old"
    sed -i "1s/{#$old /{#$new /; 1s/{#$old}/{#$new}/" "$work/expected.txt"
  fi
  cmp -s "$work/expected.txt" "$got" ||
    fail "$section" "$what" "the text differs beyond those two: want $(<"$work/expected.txt"), got $(<"$got")"
  pass "$section" "$what"
}

# kept_form SECTION DOCUMENT NUMBER WHAT: every ask block of version NUMBER of DOCUMENT (WHAT says
# whose version it is), read through the versions route, holds the form R10 keeps
# (docs.keptAskState), each value equal to its ask's node as the store states it now: the ask whose
# row names the block of DOCUMENT under its id, or, for a block no row names whose block node
# holds a standing copied_from (settlement's claimCopies), the ask it copies. Such a block holds
# `state` where that node's answer_state is answered or resolved and none where it is open or
# absent, `answered_by` and `answered_at` as the node holds them where it is answered, and, on a
# copy, `copied_from`, the ask, and `copied_from_document`, the address of the document that ask's
# row names (refs.ArtifactRef). A block that has no ask row and is no copy of an ask holds none of
# the eight server-owned attributes, and no block holds `selected`, `answer` or `invalid`. It prints
# each block's attributes; a block that differs fails the run, which prints the block, the
# attributes it holds and the ones its ask's node gives. It counts the blocks of each branch it read
# that hold the form, in kept_copies, kept_answered_copies (a copy whose source is answered) and
# kept_rowless (a block with no ask row and no copy), for a caller that requires a branch read or
# prints what it read.
kept_form() {
  local section="$1" document="$2" number="$3" what="$4" id own source copied state by at name whose
  local opening got held wants differs blocks=0
  local -a ids
  local -A wanted
  send agent GET "/api/v1/artifacts/$document/versions/$number"
  want_status 200 "$section" "read version $number of $document, $what"
  jq -j '.markdown' "$work/body" >"$work/kept-form.md"
  mapfile -t ids < <(sed -nE 's/^:+ask\{#([^ }]+).*/\1/p' "$work/kept-form.md")
  if [[ "${#ids[@]}" == 0 ]]; then
    pass "$section" "version $number of $document, $what, holds no ask block"
    return 0
  fi
  env -u PGHOSTADDR psql "$database_url" -XAtq -F '|' -c "
    with block (block_id, position) as (
      select b, p from unnest(string_to_array('${ids[*]}', ' ')) with ordinality as u (b, p)
    ), owned as (
      select block.block_id, block.position,
        (select a.id::text from asks a where a.block_artifact_id = '$document' and a.block_id = block.block_id) as own,
        (select c.to_id from document_blocks d
           cross join lateral (
             select distinct on (t.to_kind, t.to_id) t.to_id, t.asserted from graph_tuples t
             where t.from_kind = d.node_kind and t.from_id = d.node_address and t.kind = 'copied_from' and t.to_kind = 'ask'
             order by t.to_kind, t.to_id, t.seq desc) c
           where d.artifact_id = '$document' and d.block_id = block.block_id and d.node_address is not null and c.asserted
           limit 1) as copied
      from block
    ), asked as (
      select o.block_id, o.position, o.own, case when o.own is null then o.copied end as source,
        coalesce(o.own, o.copied) as ask
      from owned o
    )
    select k.block_id, coalesce(k.own, ''), coalesce(k.source, ''),
      coalesce((select case when sa.session_id is not null then 'dispatch://agent/' || sa.session_id || '/artifact/' || sa.slug
                            when sa.issue_key is null then 'dispatch://' || sa.project_key || '/artifact/' || sa.slug
                            when sa.is_primary and sa.kind = 'doc' then 'dispatch://' || sa.issue_key || '/spec'
                            else 'dispatch://' || sa.issue_key || '/artifact/' || sa.slug end
                from asks s join artifacts sa on sa.id = s.block_artifact_id where s.id::text = k.source), ''),
      coalesce(n.state, ''), coalesce(n.answered_by, ''), coalesce(n.answered_at, '')
    from asked k
    left join lateral (
      select max(f.to_id) filter (where f.kind = 'answer_state') as state,
        string_agg(f.to_id, ',' order by f.to_id) filter (where f.kind = 'answered_by') as answered_by,
        max(f.to_id) filter (where f.kind = 'answered_at') as answered_at
      from (
        select distinct on (g.kind, g.to_kind, case when g.to_kind = 'value' then '' else g.to_id end) g.kind, g.to_id, g.asserted
        from graph_tuples g
        where g.from_kind = 'ask' and g.from_id = k.ask and g.kind in ('answer_state', 'answered_by', 'answered_at')
        order by g.kind, g.to_kind, case when g.to_kind = 'value' then '' else g.to_id end, g.seq desc) f
      where f.asserted) n on true
    order by k.position" >"$work/kept-form-rows" 2>"$work/psql" ||
    fail "$section" "read the ask of each ask block of version $number of $document" "psql: $(<"$work/psql")"
  [[ "$(wc -l <"$work/kept-form-rows")" == "${#ids[@]}" ]] ||
    fail "$section" "read the ask of each of the ${#ids[@]} ask blocks of version $number of $document" "$(<"$work/kept-form-rows")"
  while IFS='|' read -r id own source copied state by at; do
    blocks=$((blocks + 1))
    wanted=()
    whose="no ask row and no copy of an ask"
    [[ -z "$own" ]] || whose="ask $own, its node's answer_state ${state:-absent}"
    [[ -z "$source" ]] || whose="a copy of ask $source of $copied, its node's answer_state ${state:-absent}"
    if [[ -n "$own$source" && -n "$state" && "$state" != open ]]; then
      wanted[state]="$state"
    fi
    if [[ -n "$own$source" && "$state" == answered ]]; then
      [[ -z "$by" ]] || wanted[answered_by]="$by"
      [[ -z "$at" ]] || wanted[answered_at]="$at"
    fi
    if [[ -n "$source" ]]; then
      wanted[copied_from]="$source"
      wanted[copied_from_document]="$copied"
    fi
    ask_block "$id" "$work/kept-form.md" >"$work/kept-form-block.md"
    opening="$(head -n 1 "$work/kept-form-block.md")"
    held="" wants="" differs=""
    for name in "${server_attributes[@]}"; do
      got="$(attribute_of "$opening" "$name")"
      held+="${got:+ $got}"
      if [[ -n "${wanted[$name]+set}" ]]; then
        wants+=" $name=\"${wanted[$name]}\""
        [[ "$got" == "$name=\"${wanted[$name]}\"" ]] || differs+=" $name"
      elif [[ -n "$got" ]]; then
        differs+=" $name"
      fi
    done
    if [[ -n "$differs" ]]; then
      printf '%s: version %s of %s, %s: the ask block %s (%s):\n%s\n' "$section" "$number" "$document" "$what" "$id" "$whose" \
        "$(<"$work/kept-form-block.md")"
      printf '%s: the server-owned attributes it holds:%s\n' "$section" "${held:- none}"
      printf '%s: the form R10 keeps, from its ask'"'"'s node:%s\n' "$section" "${wants:- none}"
      fail "$section" "every ask block of version $number of $document, $what, holds the form R10 keeps, each value its ask's node's" \
        "the block $id ($whose) differs in$differs: it holds${held:- none}, want${wants:- none}"
    fi
    printf '%s: version %s of %s: the ask block %s (%s) holds%s\n' "$section" "$number" "$document" "$id" "$whose" \
      "${held:- none of the eight server-owned attributes}"
    # Which branch of the kept form the block read, for a caller that requires a branch read.
    if [[ -n "$source" ]]; then
      kept_copies=$((kept_copies + 1))
      [[ "$state" != answered ]] || kept_answered_copies=$((kept_answered_copies + 1))
    elif [[ -z "$own" ]]; then
      kept_rowless=$((kept_rowless + 1))
    fi
  done <"$work/kept-form-rows"
  pass "$section" "the $blocks ask blocks of version $number of $document, $what, each hold the form R10 keeps, each value its ask's node's"
}

# step_events SECTION MARKER ISSUE: every artifact.version event the decisions step appended in its
# own name (docs.DecisionStepActor) after event MARKER, of ISSUE ("" for every issue), `document
# number` one a line, oldest first, in $work/step-events. It reads them all, with no limit, so a
# version the step wrote and no line of it reports is read too.
step_events() {
  env -u PGHOSTADDR psql "$database_url" -XAtq -F ' ' -c "
    select e.payload->>'artifact_id', e.payload->'version'->>'number' from events e
    where e.id > $2 and ('$3' = '' or e.issue_key = '$3') and e.type = 'artifact.version'
      and e.actor->>'kind' = 'system' and e.actor->>'id' = 'dispatch-decisions-step'
    order by e.id" >"$work/step-events" 2>"$work/psql" ||
    fail "$1" "read the versions the decisions step wrote after event $2" "psql: $(<"$work/psql")"
}

# step_versions SECTION NAME MARKER: each version the decisions step run by hand, whose output is
# $work/NAME.out, wrote, `document number` one a line, into $work/NAME.versions. Its line of each
# issue names the version it wrote of the spec (spec_version) and of the decisions document
# (decisions_version), 0 for none, and counts the versions it wrote of the issue's other documents
# (other_versions). It writes each with its artifact.version event in its own name, and nothing
# else writes in that name while the step is held, so every such event of the issue after event
# MARKER, read once the command has ended and with no limit, is one of its versions: there must be
# as many as the line counts, none for an issue whose line counts none, and they must hold the
# spec's and the decisions document's under the numbers the line names, and name the other
# documents. Every such event of any issue after MARKER must then be one the lines count, so a
# version of an issue no line names fails too. The caller reads each version's ask blocks after.
step_versions() {
  local section="$1" name="$2" marker="$3" line issue spec_version decisions_version others count spec reported=0
  : >"$work/$name.versions"
  while IFS= read -r line <&3; do
    [[ "$line" =~ \ issue=([^ ]+)\ .*\ spec_version=([0-9]+)\ decisions_version=([0-9]+)\ other_versions=([0-9]+)\  ]] ||
      fail "$section" "the line of $name names the versions it wrote" "$line"
    issue="${BASH_REMATCH[1]}" spec_version="${BASH_REMATCH[2]}" decisions_version="${BASH_REMATCH[3]}" others="${BASH_REMATCH[4]}"
    count=$(((spec_version > 0) + (decisions_version > 0) + others))
    step_events "$section" "$marker" "$issue"
    [[ "$(wc -l <"$work/step-events")" == "$count" ]] ||
      fail "$section" "the step appended an artifact.version event for each of the $count versions $name reports of $issue, and no other" \
        "it appended $(wc -l <"$work/step-events") after event $marker: $(tr '\n' ';' <"$work/step-events")"
    [[ "$count" -gt 0 ]] || continue
    reported=$((reported + count))
    query "$section" "read the spec of $issue" "select id from artifacts where issue_key = '$issue' and is_primary and kind = 'doc'"
    spec="$answer"
    [[ "$spec_version" == 0 ]] || grep -qx -- "$spec $spec_version" "$work/step-events" ||
      fail "$section" "the versions $name wrote of $issue hold version $spec_version of its spec $spec" "$(tr '\n' ';' <"$work/step-events")"
    [[ "$decisions_version" == 0 ]] ||
      awk -v s="$spec" -v n="$decisions_version" '$1 != s && $2 == n { found = 1 } END { exit !found }' "$work/step-events" ||
      fail "$section" "the versions $name wrote of $issue hold version $decisions_version of its decisions document" \
        "$(tr '\n' ';' <"$work/step-events")"
    printf '%s: the versions %s wrote of %s (document number): %s\n' "$section" "$name" "$issue" "$(tr '\n' ';' <"$work/step-events")"
    cat "$work/step-events" >>"$work/$name.versions"
  done 3< <(grep -E '^[a-z-]+: issue=[^ ]+ restored=' "$work/$name.out")
  step_events "$section" "$marker" ""
  [[ "$(wc -l <"$work/step-events")" == "$reported" ]] ||
    fail "$section" "every artifact.version event the step appended after event $marker is one of the $reported versions the lines of $name report" \
      "it appended $(wc -l <"$work/step-events"): $(tr '\n' ';' <"$work/step-events")"
}

# moved_to_rows ADDRESS: every moved_to tuple from the block node ADDRESS, one `to_id|asserted`
# line each, oldest first, into $work/moved.
moved_to_rows() {
  env -u PGHOSTADDR psql "$database_url" -XAtq -F '|' -c \
    "select to_id, asserted from graph_tuples where kind = 'moved_to' and from_kind = 'block' and from_id = '$1' order by seq" \
    >"$work/moved" 2>"$work/psql" || fail moved_to "read the moved_to tuples of $1" "psql: $(<"$work/psql")"
}

# moved_to_naming ADDRESS: how many moved_to tuples name the block node ADDRESS at either end, in
# $answer.
moved_to_naming() {
  query moved_to "read the moved_to tuples naming $1" \
    "select count(*) from graph_tuples where kind = 'moved_to' and ((from_kind = 'block' and from_id = '$1') or (to_kind = 'block' and to_id = '$1'))"
}

# shaped_of ASK: the spec nodes the current shaped tuples of ASK's node reach, one a line, sorted.
shaped_of() {
  env -u PGHOSTADDR psql "$database_url" -XAtq -c "
    select s.to_id from (
      select distinct on (t.to_kind, t.to_id) t.to_kind, t.to_id, t.asserted from graph_tuples t
      where t.from_kind = 'ask' and t.from_id = '$1' and t.kind = 'shaped' and t.to_kind <> 'value'
      order by t.to_kind, t.to_id, t.seq desc
    ) s where s.asserted order by s.to_id" 2>"$work/psql" ||
    fail shaped "read the shaped tuples of ask $1" "psql: $(<"$work/psql")"
}

# held_cte DOCUMENT BLOCK: on stdout, the `with recursive held (block_id)` clause of a query that
# follows it: BLOCK of DOCUMENT and every block BLOCK holds there, at any depth, through the block
# index's document_blocks.parent_block_id.
held_cte() {
  printf "with recursive held (block_id) as (select '%s'::text union select b.block_id from document_blocks b join held h on b.parent_block_id = h.block_id where b.artifact_id = '%s')" \
    "$2" "$1"
}

# ---------------------------------------------------------------------------------------------
# The base release writes the corpus.
# ---------------------------------------------------------------------------------------------
seed_reset_database "$database_url"
# The pinned agent bearer the e2e harness gives its own server, and the hold settings: the base
# reads neither hook, nor DISPATCH_TEST_HOLD_BOOT_BACKFILL, which its build lacks, and this head
# holds its step and its boot's run of the backfill.
DISPATCH_TOKEN=e2e-token
BASELINE_EXTRA_ENV=$'DISPATCH_TEST_HOOKS=1\nDISPATCH_TEST_HOLD_DECISIONS_STEP=1\nDISPATCH_TEST_HOLD_BOOT_BACKFILL=1'
baseline_start
unset DISPATCH_TOKEN DATABASE_URL BASELINE_EXTRA_ENV
printf 'Authorization: Bearer %s\n' "$baseline_token" >"$work/auth"
server="$baseline_url"
# The base server is the release BASELINE_SHA names, as the commit it is stamped with says, before
# it writes a row.
send agent GET /healthz
want_status 200 capture "read the base release's /healthz"
base_commit="$(jq -r '.commit // ""' "$work/body")"
readonly base_commit
[[ "$base_commit" == "$base_sha" ]] ||
  fail capture "the base release is stamped with the commit BASELINE_SHA names" \
    "it serves '$base_commit', and BASELINE_SHA $BASELINE_SHA is $base_sha"
pass capture "the base release serves $base_commit, the commit BASELINE_SHA names"
# database_url stays writable: lib-baseline.sh's baseline_boot, which boots this head, holds a
# local of that name.
seed_decisions

readonly issue_a="${seed_ids[issue_a]}" issue_b="${seed_ids[issue_b]}" issue_d="${seed_ids[issue_d]}"
readonly spec_a="${seed_ids[spec_a]}" spec_b="${seed_ids[spec_b]}" spec_d="${seed_ids[spec_d]}"
readonly decisions_a="${seed_ids[decisions_a]}" decisions_b="${seed_ids[decisions_b]}"
# The asks the restore owes: their shape, issue, spec and old block id.
readonly -a restored=(ask_left_store ask_left_format ask_doc_plain ask_doc_collide ask_b_left ask_d_approved)
declare -A shape_of=([ask_left_store]=main [ask_left_format]=main [ask_doc_plain]=second
  [ask_doc_collide]=second [ask_b_left]=main [ask_d_approved]=main)
declare -A issue_of=([ask_left_store]="$issue_a" [ask_left_format]="$issue_a" [ask_doc_plain]="$issue_a"
  [ask_doc_collide]="$issue_a" [ask_b_left]="$issue_b" [ask_d_approved]="$issue_d")
declare -A spec_of=([ask_left_store]="$spec_a" [ask_left_format]="$spec_a" [ask_doc_plain]="$spec_a"
  [ask_doc_collide]="$spec_a" [ask_b_left]="$spec_b" [ask_d_approved]="$spec_d")
declare -A old_id=([ask_left_store]=q-left-store [ask_left_format]=q-left-format [ask_doc_plain]=q-doc-plain
  [ask_doc_collide]=q-doc-collide [ask_b_left]=q-b-left [ask_d_approved]=q-d-approved)
# The headings of the sections that hold the second shape's ask blocks.
declare -A section_heading=([ask_doc_plain]="Which reader reads the corpus first"
  [ask_doc_collide]="Which line names a moved block")
# The two collisions: a live block of the spec holds the old block id.
declare -A collides=([ask_left_store]=1 [ask_doc_collide]=1)

# A copy of an answered ask in a version the restore writes, written through the base release as the
# seed writes: questions.md of issue D holds an ask the person answers, and an agent puts a copy of
# its block into spec D - the same id, question and urgency - which the base release settles as a
# copy of that ask, so no row names it and the block holds copied_from. The restore writes spec D,
# so the version it writes holds that copy, and kept_form reads a copy's kept form on it.
cat >"$work/questions-d.md" <<'MD'
# Questions

:::ask{#q-d-copied urgency="low"}
Which shelf keeps the approved version?
:::
MD
seed_upload questions_d "$issue_d" questions.md "$work/questions-d.md"
seed_ask ask_d_copied "$issue_d" "${seed_ids[questions_d]}" q-d-copied
seed_answer ask_d_copied "${seed_ids[questions_d]}" q-d-copied "The top shelf, beside the approval."
seed_edit "$spec_d" "an agent puts a copy of the answered ask q-d-copied into spec D" \
  "$(jq -cn '[{op: "insert", after: "end", markdown: ":::ask{#q-d-copied urgency=\"low\"}\nWhich shelf keeps the approved version?\n:::"}]')"
seed_wait_block "$spec_d" q-d-copied answered "the copy of q-d-copied in spec D shows its source answered"
seed_wait_settled "the copy of q-d-copied in spec D settles"
send agent GET "/api/v1/issues/$issue_d/asks?state=all"
want_status 200 capture "read the asks of $issue_d"
jq -e --arg d "$spec_d" '[.[] | select(.block_id == "q-d-copied" and .block_artifact.id == $d)] | length == 0' "$work/body" >/dev/null ||
  fail capture "the block q-d-copied of spec D is a copy no row names" \
    "$(jq -c '[.[] | select(.block_id == "q-d-copied") | {id, block_artifact}]' "$work/body")"
send agent GET "/api/v1/artifacts/$spec_d/text"
want_status 200 capture "read spec D"
jq -e --arg c "copied_from=\"${seed_ids[ask_d_copied]}\"" \
  '[.markdown | split("\n")[] | select(startswith(":::ask{#q-d-copied "))] | length == 1 and (.[0] | contains($c))' \
  "$work/body" >/dev/null ||
  fail capture "spec D holds the block q-d-copied, a copy of ask_d_copied" "$(jq -r '.markdown' "$work/body" | head -c 600)"
pass capture "spec D holds q-d-copied, a copy of the answered ask ${seed_ids[ask_d_copied]} of questions.md, which no row names"

# The closed issue: D, whose answered ask left its spec.
send agent PATCH "/api/v1/issues/$issue_d" "$(jq -cn --argjson a "$(seed_actor)" '{status: "done", actor: $a}')"
want_status 200 capture "close issue $issue_d through the base release"
expect capture "issue $issue_d is closed" '"done"' '.status'
# The close writes the copy in spec D from its source's row, and the settlement it leaves owed
# versions spec D; the capture below reads every version once nothing is owed.
seed_wait_settled "the close of issue $issue_d settles"

# Each restored ask, and q-kept, as the base release's ask read answers them.
for key in "${restored[@]}" ask_kept; do
  send agent GET "/api/v1/asks/${seed_ids[$key]}"
  want_status 200 capture "read $key through the base release"
  jq '.ask' "$work/body" >"$work/capture/ask.$key.json"
  jq -e '.state == "answered" and (.answer.text | type == "string") and (.answer.user | type == "string")' \
    "$work/capture/ask.$key.json" >/dev/null || fail capture "$key is answered" "$(body)"
done
pass capture "captured the id, question, options, answer and answerer of ${#restored[@]} restored asks"

# The person answered q-left-format before q-left-store, whose block stood above it in the spec, so
# the answer-time order the main section reads below is not the order of the asks' blocks, and a
# restore that wrote them in their blocks' order fails it.
left_format_at="$(jq -r '.answer.at' "$work/capture/ask.ask_left_format.json")"
left_store_at="$(jq -r '.answer.at' "$work/capture/ask.ask_left_store.json")"
readonly left_format_at left_store_at
query capture "compare the answer times of ask_left_format and ask_left_store" \
  "select ('$left_format_at'::timestamptz < '$left_store_at'::timestamptz)::text"
[[ "$answer" == true ]] ||
  fail capture "the person answered q-left-format before q-left-store, the reverse of their blocks' order" \
    "q-left-format at $left_format_at, q-left-store at $left_store_at"
pass capture "the person answered q-left-format ($left_format_at) before q-left-store ($left_store_at), the reverse of their blocks' order in the spec"

# The spec blocks the base's Decisions read links each ask's section to.
for issue in "$issue_a" "$issue_b"; do
  send agent GET "/api/v1/issues/$issue/decisions"
  want_status 200 capture "read the Decisions of $issue through the base release"
  cp "$work/body" "$work/capture/decisions.$issue.json"
done
for key in "${restored[@]}"; do
  file="$work/capture/decisions.${issue_of[$key]}.json"
  [[ -f "$file" ]] || {
    : >"$work/capture/links.$key"
    continue
  }
  if [[ -n "${section_heading[$key]:-}" ]]; then
    jq -r --arg h "${section_heading[$key]}" '.decisions[] | select(.heading == $h) | .links[].to.id' "$file"
  else
    jq -r --arg a "${seed_ids[$key]}" '.decisions[] | select(.asks | index($a)) | .links[].to.id' "$file"
  fi | LC_ALL=C sort -u >"$work/capture/links.$key"
  printf 'captured links of %s: %s\n' "$key" "$(tr '\n' ' ' <"$work/capture/links.$key")"
done
[[ -s "$work/capture/links.ask_left_store" ]] ||
  fail capture "the section restating ask_left_store links spec blocks" "the base read links none"

# Every version of the spec and the decisions document of each issue the restore owes.
captured_versions=0
for document in "$spec_a" "$decisions_a" "$spec_b" "$decisions_b" "$spec_d"; do
  send agent GET "/api/v1/artifacts/$document"
  want_status 200 capture "read document $document"
  for number in $(jq -r '.versions[].number' "$work/body"); do
    send agent GET "/api/v1/artifacts/$document/versions/$number"
    want_status 200 capture "read version $number of $document"
    jq -j '.markdown' "$work/body" >"$work/capture/version.$document.$number.md"
    captured_versions=$((captured_versions + 1))
  done
done
pass capture "captured $captured_versions versions of the five documents"

# What the old addresses of the colliding blocks answer: the second shape's block in the
# decisions document, and the callout of the spec that holds the main shape's old id.
send agent GET "/api/v1/references?to=$(q "dispatch://$issue_a/artifact/decisions-md#b-q-doc-collide")"
want_status 200 capture "resolve decisions-md#b-q-doc-collide through the base release"
jq -j '.node.text' "$work/body" >"$work/capture/text.q-doc-collide"
send agent GET "/api/v1/references?to=$(q "dispatch://$issue_a/spec#b-q-left-store")"
want_status 200 capture "resolve spec#b-q-left-store through the base release"
expect capture "spec#b-q-left-store is the callout that took that id" '"callout"' '.node.text | capture("^:::(?<t>[a-z]+)").t'
jq -j '.node.text' "$work/body" >"$work/capture/text.q-left-store"

# The block_refs rows the decisions document holds for the second shape's ask blocks and for every
# block each holds: the seed's question of each cites an issue, which the base release indexes
# under the question's paragraph, a block the ask block holds. The blocks' ids go to
# capture/held.$key and the edges, one `where|kind|to_kind|to_id|count` line each (where: `own`
# for the ask block's own, `held` for a block it holds), to capture/edges.$key, which must not be
# empty: an ask that holds no edge would leave the check of the two documents moving together
# below nothing to check.
for key in ask_doc_plain ask_doc_collide; do
  env -u PGHOSTADDR psql "$database_url" -XAtq -c \
    "$(held_cte "$decisions_a" "${old_id[$key]}") select block_id from held order by 1" \
    >"$work/capture/held.$key" 2>"$work/psql" || fail capture "read the blocks $key holds" "psql: $(<"$work/psql")"
  env -u PGHOSTADDR psql "$database_url" -XAtq -F '|' -c "$(held_cte "$decisions_a" "${old_id[$key]}")
    select case when block_id = '${old_id[$key]}' then 'own' else 'held' end, kind, to_kind, to_id, count(*)
    from block_refs where artifact_id = '$decisions_a' and block_id in (select block_id from held)
    group by 1, 2, 3, 4 order by 1, 2, 3, 4" \
    >"$work/capture/edges.$key" 2>"$work/psql" || fail capture "read the edges of $key" "psql: $(<"$work/psql")"
  [[ -s "$work/capture/edges.$key" ]] ||
    fail capture "the decisions document holds an edge of $key or of a block it holds" \
      "block_refs holds none for its blocks $(tr '\n' ' ' <"$work/capture/held.$key")"
  printf 'captured edges of %s and the blocks it holds in the decisions document: %s\n' "$key" \
    "$(tr '\n' ' ' <"$work/capture/edges.$key")"
done
baseline_stop_server

# ---------------------------------------------------------------------------------------------
# This head, held, over the same database.
# ---------------------------------------------------------------------------------------------
baseline_boot head "$head_binary" "$baseline_database_url"
head_pid="$baseline_boot_pid"
server="$baseline_boot_url"
send agent GET /healthz
want_status 200 head "read this head's /healthz"
head_commit="$(jq -r '.commit // ""' "$work/body")"
[[ "$head_commit" == "$head_sha" && "$head_commit" != "$base_commit" ]] ||
  fail head "this head is stamped with the commit this checkout stands on, not the base's" \
    "it serves '$head_commit', the checkout stands on '$head_sha', the base serves '$base_commit'"
pass head "this head serves $head_commit, the commit this checkout stands on, over the base release's database, the decisions step held"
sign_in head

# The step's first phase, which states each answer on its ask's node.
dispatch_command backfill backfill-graph-store
[[ "$command_code" == 0 ]] || fail census "envoy-dispatch backfill-graph-store" "exited $command_code: $(<"$work/backfill.out")"
pass census "envoy-dispatch backfill-graph-store stated the rows the release before wrote"

# The census of specs whose answered asks were moved out lists each with exactly its asks.
census census census-before
declare -A owed=()
for key in "${restored[@]}"; do
  owed["${issue_of[$key]}"]+="${seed_ids[$key]} "
done
grep -q "^census: decisions documents=[0-9]* unconverted_sections=[0-9]* specs_with_moved_answers=${#owed[@]}\$" "$work/census-before.out" ||
  fail census "the census lists ${#owed[@]} specs whose answered asks left them" "$(grep '^census: decisions' "$work/census-before.out")"
for issue in "${!owed[@]}"; do
  want="$(tr ' ' '\n' <<<"${owed[$issue]}" | sed '/^$/d' | sort | tr '\n' ',')"
  line="$(grep "^census: spec_with_moved_answers issue=$issue asks=" "$work/census-before.out" || true)"
  got="$(tr ',' '\n' <<<"${line##*asks=}" | sed '/^$/d' | sort | tr '\n' ',')"
  [[ -n "$line" && "$got" == "$want" ]] ||
    fail census "the census lists $issue with the asks that left its spec" "got '${line:-no line}', want asks ${want%,}"
  pass census "the census lists $issue with asks ${want%,}"
done

# The restore, through its entry point. The newest event before it bounds the artifact.version
# events of the versions it writes (step_versions).
query restore "read the newest event before the restore" "select coalesce(max(id), 0) from events"
readonly before_restore="$answer"
dispatch_command restore restore-answered-asks --project "$project"
grep '^restore-answered-asks: ' "$work/restore.out" | sed 's/^/  /' || true
[[ "$command_code" == 0 ]] || fail restore "envoy-dispatch restore-answered-asks --project $project" "exited $command_code: $(<"$work/restore.out")"
for issue in "${!owed[@]}"; do
  count="$(tr ' ' '\n' <<<"${owed[$issue]}" | sed '/^$/d' | wc -l)"
  grep -q "^restore-answered-asks: issue=$issue restored=$count " "$work/restore.out" ||
    fail restore "the restore returns $count asks to the spec of $issue" "$(grep "issue=$issue " "$work/restore.out" || echo 'no line')"
done
pass restore "envoy-dispatch restore-answered-asks --project $project restored every ask the census listed"
# The version of each spec the restore itself wrote, as its line names it: the block the restore
# built is read there, before any later write of the spec (a settlement's repair of a decided
# wording among them) can stand in for it.
declare -A restore_version=()
for issue in "${!owed[@]}"; do
  restore_version["$issue"]="$(grep "^restore-answered-asks: issue=$issue " "$work/restore.out" |
    sed -nE 's/.* spec_version=([0-9]+)( .*)?$/\1/p')"
  [[ "${restore_version[$issue]}" =~ ^[1-9][0-9]*$ ]] ||
    fail restore "the restore names the version of the spec of $issue it wrote" "$(grep "issue=$issue " "$work/restore.out")"
done
wait_settled restore

census census census-after
grep -q '^census: decisions documents=[0-9]* unconverted_sections=[0-9]* specs_with_moved_answers=0$' "$work/census-after.out" ||
  fail census "the census prints zero once the restore has run" "$(grep '^census: decisions' "$work/census-after.out")"
! grep -q '^census: spec_with_moved_answers ' "$work/census-after.out" ||
  fail census "the census lists no spec once the restore has run" "$(grep '^census: spec_with_moved_answers ' "$work/census-after.out")"
pass census "the census prints specs_with_moved_answers=0 once the restore has run"

# ---------------------------------------------------------------------------------------------
# Each restored ask.
# ---------------------------------------------------------------------------------------------
declare -A new_id=()
for key in "${restored[@]}"; do
  captured="$work/capture/ask.$key.json"
  send agent GET "/api/v1/asks/${seed_ids[$key]}"
  want_status 200 answer "read $key at this head"
  expect answer "$key keeps its id" "\"${seed_ids[$key]}\"" '.ask.id'
  expect answer "$key keeps its answer character for character" "$(jq -cS '.answer.text' "$captured")" '.ask.answer.text'
  expect answer "$key keeps its answerer" "$(jq -cS '.answer.user' "$captured")" '.ask.answer.user'
  expect answer "$key names the spec as its block's document" "\"${spec_of[$key]}\"" '.ask.block_artifact.id'
  field answer "the block id of $key" '.ask.block_id'
  new_id["$key"]="$value"
  if [[ -n "${collides[$key]:-}" ]]; then
    [[ "${new_id[$key]}" != "${old_id[$key]}" ]] ||
      fail collision "$key takes a new block id, since the spec holds ${old_id[$key]}" "it kept ${old_id[$key]}"
  else
    [[ "${new_id[$key]}" == "${old_id[$key]}" ]] ||
      fail restore "$key takes its old block id ${old_id[$key]}" "it took ${new_id[$key]}"
  fi
done

for spec in "$spec_a" "$spec_b" "$spec_d"; do
  latest_markdown main "$spec"
  cp "$work/latest.md" "$work/spec.$spec.md"
  ! grep -Fq "The decisions this spec rests on are in dispatch://" "$work/latest.md" ||
    fail main "the restored asks of $spec stand in place of the pointer line" "the pointer line is still there"
  [[ "$(grep -c '^## Decisions$' "$work/latest.md")" == 1 ]] ||
    fail main "$spec holds one Decisions part" "$(grep -c '^## Decisions$' "$work/latest.md") such headings"
  # The asks of this spec, in answer-time order, and the order the Decisions part holds them in.
  want_order=""
  while read -r _ key; do
    want_order+="${new_id[$key]} "
  done < <(for key in "${restored[@]}"; do
    if [[ "${spec_of[$key]}" == "$spec" ]]; then
      printf '%s %s\n' "$(jq -r '.answer.at' "$work/capture/ask.$key.json")" "$key"
    fi
  done | sort)
  got_order="$(sed -n '/^## Decisions$/,/^## /p' "$work/latest.md" | sed -nE 's/^:+ask\{#([^ }]+).*/\1/p' | tr '\n' ' ')"
  [[ "$got_order" == "$want_order" ]] ||
    fail main "the Decisions part of $spec holds the restored asks in answer-time order" "got $got_order, want $want_order"
  pass main "the Decisions part of $spec holds ${want_order% } in answer-time order, in place of the pointer line"
done

for key in "${restored[@]}"; do
  spec="${spec_of[$key]}"
  ask_block "${new_id[$key]}" "$work/spec.$spec.md" >"$work/block.md"
  [[ -s "$work/block.md" ]] || fail main "the spec holds the block of $key" "no ask block ${new_id[$key]} in $spec"
  opening="$(head -n 1 "$work/block.md")"
  answer_text="$(jq -r '.answer.text' "$work/capture/ask.$key.json")"
  ! grep -Fq -- "$answer_text" "$work/block.md" || fail main "the block of $key holds no answer" "$(<"$work/block.md")"
  block_body "$work/block.md" >"$work/body.got"
  expected_body "$work/capture/ask.$key.json" >"$work/body.want"
  cmp -s "$work/body.got" "$work/body.want" ||
    fail main "the block of $key holds the question and options of its row character for character" \
      "want $(<"$work/body.want"), got $(<"$work/body.got")"
  pass main "the block of $key holds its row's question and options and no answer: $opening"
  restored_at="${restore_version[${issue_of[$key]}]}"
  send agent GET "/api/v1/artifacts/$spec/versions/$restored_at"
  want_status 200 main "read version $restored_at of $spec, the restore's own"
  jq -j '.markdown' "$work/body" >"$work/restored.$spec.md"
  ask_block "${new_id[$key]}" "$work/restored.$spec.md" >"$work/restored-block.md"
  [[ -s "$work/restored-block.md" ]] ||
    fail main "version $restored_at of $spec, the restore's own, holds the block of $key" "no ask block ${new_id[$key]} in it"
  block_body "$work/restored-block.md" >"$work/restored-body.got"
  cmp -s "$work/restored-body.got" "$work/body.want" ||
    fail main "the block the restore wrote for $key, at version $restored_at, holds the question and options of its row character for character" \
      "want $(<"$work/body.want"), got $(<"$work/restored-body.got")"
  pass main "the block the restore wrote for $key, at version $restored_at of $spec, holds its row's question and options"

  spec_ref="${issue_of[$key]}/spec"
  if [[ "${shape_of[$key]}" == main ]]; then
    moved_to_naming "$spec_ref#${new_id[$key]}"
    [[ "$answer" == 0 ]] || fail moved_to "no moved_to tuple names the block of $key" "$answer do"
    moved_to_naming "$spec_ref#${old_id[$key]}"
    [[ "$answer" == 0 ]] || fail moved_to "no moved_to tuple names the old address of $key" "$answer do"
    pass moved_to "no moved_to tuple names $key, which returned under the spec's own address"
  else
    moved_to_rows "${issue_of[$key]}/decisions-md#${old_id[$key]}"
    [[ "$(<"$work/moved")" == "$spec_ref#${new_id[$key]}|t" ]] ||
      fail moved_to "one moved_to tuple from the old address of $key to its new one" "$(tr '\n' ' ' <"$work/moved")"
    pass moved_to "one moved_to tuple from ${issue_of[$key]}/decisions-md#${old_id[$key]} to $spec_ref#${new_id[$key]}"
  fi

  shaped_of "${seed_ids[$key]}" | LC_ALL=C sort >"$work/shaped"
  cmp -s "$work/shaped" "$work/capture/links.$key" ||
    fail shaped "one shaped tuple from $key to each spec node its section linked" \
      "got $(tr '\n' ' ' <"$work/shaped"), want $(tr '\n' ' ' <"$work/capture/links.$key")"
  pass shaped "$key shapes exactly the spec nodes its section linked: $(tr '\n' ' ' <"$work/shaped")"

  query copy "read the copied_from tuples of the block of $key" \
    "select count(*) from graph_tuples where kind = 'copied_from' and from_kind = 'block' and from_id = '$spec_ref#${new_id[$key]}'"
  [[ "$answer" == 0 ]] || fail copy "no mark of a copy on the restored block of $key" "$answer copied_from tuples"
  pass copy "the restored block of $key carries no mark of a copy"
done

# ---------------------------------------------------------------------------------------------
# The form R10 keeps, in every version the restore wrote.
# ---------------------------------------------------------------------------------------------
# Every ask block of each version the restore wrote - each spec's, which holds the restored blocks
# of both shapes beside the blocks it held already, the decisions document's it took the second
# shape's blocks out of, and any other its lines count - holds the form R10 keeps, each value equal
# to its ask's node (kept_form). Spec D's holds the copy of the answered ask q-d-copied, so the
# kept form of a copy is read there: its source's state, answerer and time, and copied_from and
# copied_from_document. A block that has no ask row and is no copy is read wherever a version holds
# one, but none is required and the PASS line claims none: no release's API makes such a block (a
# browser writes it into a room), and the Go test
# TestEveryVersionTheRestoreWritesHoldsNoneOfTheEightOnABlockWithNoAskRowThatIsNoCopy holds that
# half. kept_rowless is printed in the FAIL line alone, beside the copies, to say what was read.
kept_copies=0 kept_answered_copies=0 kept_rowless=0
step_versions kept restore "$before_restore"
[[ -s "$work/restore.versions" ]] || fail kept "the restore wrote a version" "its lines name none: $(<"$work/restore.out")"
while read -r document number <&3; do
  kept_form kept "$document" "$number" "a version the restore wrote"
done 3<"$work/restore.versions"
pass kept "every ask block of the $(wc -l <"$work/restore.versions") versions the restore wrote, in both shapes, holds the form R10 keeps, each value its ask's node's"
[[ "$kept_answered_copies" -gt 0 ]] ||
  fail kept "a version the restore wrote holds a copy of an answered ask, so the kept form of a copy is read" \
    "the versions it wrote hold $kept_copies copies, none of an answered ask, and $kept_rowless blocks with no ask row and no copy"
pass kept "the versions the restore wrote hold $kept_answered_copies copies of an answered ask, each holding its source's state, answered_by and answered_at, and copied_from and copied_from_document, as its source's node gives them"

# Every version captured before the upgrade answers the same bytes.
for captured in "$work"/capture/version.*.md; do
  name="${captured##*/version.}"
  document="${name%%.*}"
  number="${name#*.}"
  number="${number%.md}"
  send agent GET "/api/v1/artifacts/$document/versions/$number"
  want_status 200 history "read version $number of $document"
  jq -j '.markdown' "$work/body" >"$work/version.md"
  cmp -s "$captured" "$work/version.md" ||
    fail history "version $number of $document answers the bytes captured before the upgrade" "it differs"
done
pass history "every one of the $captured_versions captured versions answers the bytes captured before the upgrade"

# The two documents of the second shape move together: the ask block and every block it held leave
# the decisions document's index and block_refs, and each captured edge stands in the spec, the
# ask block's own under its new id and a held block's under a block the restored ask block holds.
for key in ask_doc_plain ask_doc_collide; do
  held_list="$(sed "s/.*/'&'/" "$work/capture/held.$key" | paste -sd, -)"
  query together "read the decisions document's index of $key and the blocks it held" \
    "select count(*) from document_blocks where artifact_id = '$decisions_a' and block_id in ($held_list)"
  [[ "$answer" == 0 ]] ||
    fail together "the decisions document no longer holds the block of $key or a block it held" "its index holds $answer"
  query together "read the spec's index of $key" \
    "select count(*) from document_blocks where artifact_id = '$spec_a' and block_id = '${new_id[$key]}' and type = 'ask'"
  [[ "$answer" == 1 ]] || fail together "the spec holds the block of $key" "its index holds $answer"
  query together "read the decisions document's edges of $key and the blocks it held" \
    "select count(*) from block_refs where artifact_id = '$decisions_a' and block_id in ($held_list)"
  [[ "$answer" == 0 ]] || fail together "no edge of $key or of a block it held is left under the decisions document" "$answer are"
  while IFS='|' read -r where kind to_kind to_id count; do
    if [[ "$where" == own ]]; then
      query together "read the spec's edge $kind of $key" \
        "select count(*) from block_refs where artifact_id = '$spec_a' and block_id = '${new_id[$key]}' and kind = '$kind' and to_kind = '$to_kind' and to_id = '$to_id'"
    else
      query together "read the spec's edge $kind of a block $key holds" \
        "$(held_cte "$spec_a" "${new_id[$key]}") select count(*) from block_refs where artifact_id = '$spec_a' and block_id in (select block_id from held where block_id <> '${new_id[$key]}') and kind = '$kind' and to_kind = '$to_kind' and to_id = '$to_id'"
    fi
    [[ "$answer" == "$count" ]] ||
      fail together "the spec holds the edge $kind -> $to_id of $key ($where block)" "it holds $answer, want $count"
  done <"$work/capture/edges.$key"
  pass together "$key moved with its captured edges ($(tr '\n' ' ' <"$work/capture/edges.$key")): the spec's index and block_refs hold it and them, the decisions document's hold nothing of it or of a block it held"
done

# ---------------------------------------------------------------------------------------------
# The two collisions.
# ---------------------------------------------------------------------------------------------
query collision "read that asks_block_id_unique stands" \
  "select count(*) from pg_indexes where indexname = 'asks_block_id_unique'"
[[ "$answer" == 1 ]] || fail collision "asks_block_id_unique stands" "pg_indexes holds $answer such index"
for key in ask_left_store ask_doc_collide; do
  query collision "read the asks row of $key" "select block_id from asks where id = '${seed_ids[$key]}'"
  [[ "$answer" == "${new_id[$key]}" ]] ||
    fail collision "asks_block_id_unique took the row of $key with its new id" "the row holds $answer"
  query collision "read which documents of $issue_a hold ${new_id[$key]}" \
    "select string_agg(a.ref_key, ' ') from document_blocks b join artifacts a on a.id = b.artifact_id where a.issue_key = '$issue_a' and b.block_id = '${new_id[$key]}'"
  [[ "$answer" == "$issue_a/spec" ]] ||
    fail collision "no other document of $issue_a holds the new id of $key" "held by: $answer"
  pass collision "$key took ${new_id[$key]}, which no other document of $issue_a holds, and its asks row reads it"
done
# The second shape: the old address still resolves through its one moved_to tuple.
send agent GET "/api/v1/references?to=$(q "dispatch://$issue_a/artifact/decisions-md#b-q-doc-collide")"
want_status 200 collision "resolve decisions-md#b-q-doc-collide"
expect collision "decisions-md#b-q-doc-collide resolves to the restored block" "\"$issue_a/spec#${new_id[ask_doc_collide]}\"" '.node.id'
jq -j '.node.text' "$work/body" >"$work/text.got"
same_text collision "decisions-md#b-q-doc-collide answers the captured text" "$work/capture/text.q-doc-collide" \
  "$work/text.got" q-doc-collide "${new_id[ask_doc_collide]}"
# The main shape: no moved_to, and the old address answers the live block that holds that id.
moved_to_naming "$issue_a/spec#q-left-store"
[[ "$answer" == 0 ]] || fail collision "no moved_to tuple names $issue_a/spec#q-left-store" "$answer do"
send agent GET "/api/v1/references?to=$(q "dispatch://$issue_a/spec#b-q-left-store")"
want_status 200 collision "resolve spec#b-q-left-store"
expect collision "spec#b-q-left-store answers the live block that holds that id" "\"$issue_a/spec#q-left-store\"" '.node.id'
jq -j '.node.text' "$work/body" >"$work/text.got"
cmp -s "$work/capture/text.q-left-store" "$work/text.got" ||
  fail collision "spec#b-q-left-store answers the live callout as captured" "got $(<"$work/text.got")"
pass collision "spec#b-q-left-store answers the live callout that holds that id, character for character: $(<"$work/text.got")"

# ---------------------------------------------------------------------------------------------
# The closed issue, and the restore run again.
# ---------------------------------------------------------------------------------------------
send agent GET "/api/v1/issues/$issue_d"
want_status 200 closed "read issue $issue_d"
expect closed "issue $issue_d is still closed" '"done"' '.status'
pass closed "the restore returned ask_d_approved to the spec of the closed issue $issue_d, as for the open ones"
declare -A version_after=()
for spec in "$spec_a" "$spec_b" "$spec_d"; do
  latest_markdown closed "$spec"
  version_after["$spec"]="$version"
done
dispatch_command restore-again restore-answered-asks --project "$project"
[[ "$command_code" == 0 ]] || fail closed "run the restore again" "exited $command_code: $(<"$work/restore-again.out")"
! grep -E '^restore-answered-asks: issue=.* restored=[1-9]' "$work/restore-again.out" ||
  fail closed "the restore run again restores nothing more" "$(grep '^restore-answered-asks: ' "$work/restore-again.out")"
wait_settled closed
for spec in "$spec_a" "$spec_b" "$spec_d"; do
  latest_markdown closed "$spec"
  [[ "$version" == "${version_after[$spec]}" ]] ||
    fail closed "the restore run again writes no version of $spec" "it is at $version, was ${version_after[$spec]}"
  cmp -s "$work/latest.md" "$work/spec.$spec.md" || fail closed "the restore run again leaves $spec as it was" "it differs"
done
send agent GET "/api/v1/asks/${seed_ids[ask_d_approved]}"
want_status 200 closed "read ask_d_approved again"
expect closed "ask_d_approved of the closed issue reads the same answer" \
  "$(jq -cS '.answer.text' "$work/capture/ask.ask_d_approved.json")" '.ask.answer.text'
expect closed "ask_d_approved of the closed issue reads the same block" "\"q-d-approved\"" '.ask.block_id'
pass closed "the restore run again reads the same result, for the closed issue $issue_d and every other"

# ---------------------------------------------------------------------------------------------
# An agent cannot drop an answered ask's block; the person can.
# ---------------------------------------------------------------------------------------------
readonly kept_ask="${seed_ids[ask_kept]}"
kept_question="$(jq -r '.question' "$work/capture/ask.ask_kept.json")"
readonly kept_question
send agent GET "/api/v1/artifacts/$spec_a"
want_status 200 refusal "read the spec of $issue_a"
field refusal "the spec's name" '.name'
readonly spec_name="$value"

# refused WHAT: the last answer is the refusal of a write dropping or retyping q-kept, quoting the
# ask's question and naming its node and its block; the spec's version is unchanged and its
# answer still in place.
refused() {
  want_status 409 refusal "$1"
  expect refusal "$1 is ANSWERED_ASK_KEPT" '"ANSWERED_ASK_KEPT"' '.code'
  field refusal "$1 says why" '.error'
  message="$value"
  [[ "$message" == *"dispatch://$issue_a/ask/$kept_ask"* && "$message" == *"\"$kept_question\""* &&
    "$message" == *"dispatch://$issue_a/spec#b-q-kept"* ]] ||
    fail refusal "$1 quotes the ask and names its node and block" "$message"
  printf 'refusal: %s\n' "$message"
  latest_markdown refusal "$spec_a"
  [[ "$version" == "$kept_version" ]] || fail refusal "$1 writes no version" "the spec is at $version, was $kept_version"
  ask_block q-kept "$work/latest.md" >"$work/kept.md"
  [[ -s "$work/kept.md" ]] || fail refusal "$1 leaves q-kept in the spec" "the newest version holds no q-kept"
  send agent GET "/api/v1/asks/$kept_ask"
  want_status 200 refusal "read q-kept's ask"
  expect refusal "$1 leaves the answer in place" "$(jq -cS '.answer.text' "$work/capture/ask.ask_kept.json")" '.ask.answer.text'
  pass refusal "$1 is refused, quoting the ask and naming its node and block; the version and the answer stand"
}

latest_markdown refusal "$spec_a"
kept_version="$version"
cp "$work/latest.md" "$work/spec-before-refusals.md"
send agent POST "/api/v1/artifacts/$spec_a/edits" \
  "$(jq -cn --argjson a "$(seed_actor)" '{ops: [{op: "delete", block: "q-kept"}], actor: $a}')"
refused "an agent's delete of q-kept"
# without_block ID FILE: FILE without the ask block ID and the blank line after it.
without_block() {
  awk -v id="$1" '
    skip == 2 && $0 == "" { skip = 0; next }
    skip == 2 { skip = 0 }
    !skip && match($0, /^:+ask\{#/) {
      rest = substr($0, RLENGTH + 1)
      if (index(rest, id " ") == 1 || index(rest, id "}") == 1) { skip = 1; fence = substr($0, 1, index($0, "ask{") - 1); next }
    }
    skip == 1 { if ($0 == fence) skip = 2; next }
    { print }
  ' "$2"
}
without_block q-kept "$work/spec-before-refusals.md" >"$work/replacement.md"
document_token refusal "$spec_a"
send agent POST "/api/v1/issues/$issue_a/artifacts" \
  "$(jq -cn --arg n "$spec_name" --rawfile c "$work/replacement.md" --arg t "$doc_token" --argjson a "$(seed_actor)" \
    '{name: $n, content: $c, precondition: {document: $t}, actor: $a}')"
refused "an agent's whole-document replacement without q-kept"
send agent POST "/api/v1/artifacts/$spec_a/edits" \
  "$(jq -cn --argjson a "$(seed_actor)" '{ops: [{op: "retype", block: "q-kept", type: "callout", attributes: {kind: "note", title: ""}}], actor: $a}')"
refused "an agent's retype of q-kept"
[[ "$message" == *retype* ]] || fail refusal "the retype's refusal says it retypes the block" "$message"

# The same three writes as the person, each dropping or retyping the block of an answered ask.
# taken WHAT ID STILL: the person's write WHAT was taken, and the newest version holds the ask
# block ID no longer (STILL empty) or holds a block under ID that is no ask (STILL a type).
taken() {
  [[ "$status" =~ ^20[01]$ ]] || fail person "$1" "HTTP $status: $(body)"
  wait_settled person
  latest_markdown person "$spec_a"
  [[ "$version" -gt "$kept_version" ]] || fail person "$1 writes a version" "the spec is still at $version"
  kept_version="$version"
  [[ -z "$(ask_block "$2" "$work/latest.md")" ]] || fail person "$1 is taken" "the ask block $2 is still there"
  if [[ -n "$3" ]]; then
    grep -Eq "^:+$3\\{#$2[ }]" "$work/latest.md" || fail person "$1 leaves a $3 under $2" "none in version $version"
  fi
  pass person "$1 is taken: version $version"
}
send person POST "/api/v1/artifacts/$spec_a/edits" '{"ops":[{"op":"delete","block":"q-kept"}]}'
taken "the person's delete of q-kept" q-kept ""
latest_markdown person "$spec_a"
without_block q-left-format "$work/latest.md" >"$work/replacement.md"
document_token person "$spec_a"
send person POST "/api/v1/issues/$issue_a/artifacts" \
  "$(jq -cn --arg n "$spec_name" --rawfile c "$work/replacement.md" --arg t "$doc_token" '{name: $n, content: $c, precondition: {document: $t}}')"
taken "the person's whole-document replacement without the answered q-left-format" q-left-format ""
send person POST "/api/v1/artifacts/$spec_a/edits" \
  '{"ops":[{"op":"retype","block":"q-doc-plain","type":"callout","attributes":{"kind":"note","title":""}}]}'
taken "the person's retype of the answered q-doc-plain" q-doc-plain callout

# ---------------------------------------------------------------------------------------------
# A person's removal stays on the step's own tick, and an agent's drop is restored.
# ---------------------------------------------------------------------------------------------
# ask_row SECTION ISSUE DOCUMENT BLOCK: waits, within a minute, for the ask row $server settled the
# ask block BLOCK of DOCUMENT into, and leaves its id in $value.
ask_row() {
  local waited=0
  while true; do
    send agent GET "/api/v1/issues/$2/asks"
    want_status 200 "$1" "read the asks of $2"
    value="$(jq -r --arg b "$4" --arg d "$3" \
      '[.[] | select(.block_id == $b and .block_artifact.id == $d) | .id] | if length == 1 then .[0] else "" end' \
      "$work/body")" || value=""
    [[ -z "$value" ]] || return 0
    [[ "$waited" -lt 600 ]] || fail "$1" "the ask of block $4 of $3" "no ask of $2 names it after 60 s"
    sleep 0.1
    waited=$((waited + 1))
  done
}

# answer_block SECTION ASK DOCUMENT BLOCK TEXT: the person answers ASK at $server in the words TEXT,
# and it waits, within a minute, until DOCUMENT's text writes the ask block BLOCK answered.
answer_block() {
  local waited=0
  send person POST "/api/v1/asks/$2/answer" "$(jq -cn --arg t "$5" '{text: $t, selected: [], expected_edited_at: null}')"
  want_status 200 "$1" "the person answers the ask of block $4"
  while true; do
    send agent GET "/api/v1/artifacts/$3/text"
    want_status 200 "$1" "read the text of $3"
    jq -e --arg open ":::ask{#$4 " \
      '[.markdown | split("\n")[] | select(startswith($open)) | contains("state=\"answered\"")] == [true]' \
      "$work/body" >/dev/null && return 0
    [[ "$waited" -lt 600 ]] || fail "$1" "the answer to block $4 lands in $3" "it does not read answered after 60 s: $(body)"
    sleep 0.1
    waited=$((waited + 1))
  done
}

# restorer_of SECTION SPEC FROM BLOCK…: the first version of SPEC after version FROM that holds an
# ask block under one of the ids BLOCK…, and who wrote it, in $restorer, so that a failure names
# the version that wrote the block back.
restorer_of() {
  local section="$1" spec="$2" from="$3" number id
  shift 3
  restorer="no version after $from holds its block"
  send agent GET "/api/v1/artifacts/$spec"
  want_status 200 "$section" "read document $spec"
  for number in $(jq -r --argjson from "$from" '[.versions[].number | select(. > $from)] | sort | .[]' "$work/body"); do
    send agent GET "/api/v1/artifacts/$spec/versions/$number"
    want_status 200 "$section" "read version $number of $spec"
    jq -j '.markdown' "$work/body" >"$work/restorer.md"
    for id in "$@"; do
      [[ -n "$(ask_block "$id" "$work/restorer.md")" ]] || continue
      query "$section" "read the authors of version $number of $spec" \
        "select authors::text from artifact_versions where artifact_id = '$spec' and number = $number"
      restorer="version $number of $spec, written by $answer, holds its block $id again"
      return 0
    done
  done
}

# removals_of ASK: the removed_by tuples of ASK's node, `kind:id:asserted` each, oldest first, in
# $answer.
removals_of() {
  query removal "read the removed_by tuples of ask $1" \
    "select coalesce(string_agg(to_kind || ':' || to_id || ':' || asserted::text, ' ' order by seq), '') from graph_tuples
     where from_kind = 'ask' and from_id = '$1' and kind = 'removed_by'"
}

# The base release again, over the same database, writes what the step's tick meets: a project of
# its own holding the one issue whose answered ask the person removes on this head below, and an
# issue of SEED whose answered ask an agent's session drops.
baseline_stop_pid "$head_pid"
head_pid=""
baseline_boot base-again "$BASELINE_BINARY" "$baseline_database_url"
base_pid="$baseline_boot_pid"
server="$baseline_boot_url"
send agent GET /healthz
want_status 200 removal "read /healthz of the base release, booted again"
[[ "$(jq -r '.commit // ""' "$work/body")" == "$base_commit" ]] ||
  fail removal "the base release booted again serves $base_commit" "it serves '$(jq -r '.commit // ""' "$work/body")'"
sign_in removal
readonly removal_project=PERSON
send agent POST /api/v1/projects "$(jq -cn --arg k "$removal_project" --arg n "A person's removal" --arg p "$seed_person" \
  --argjson a "$(seed_actor)" '{key: $k, name: $n, default_assignee: $p, actor: $a}')"
want_status 201 removal "create the project $removal_project through the base release"
cat >"$work/spec-person.md" <<'MD'
# A person's removal

:::requirement{#r-shelf}
The printed corpus stands on one shelf.
:::

:::ask{#q-person urgency="med"}
Which shelf holds the printed corpus?
:::
MD
send agent POST /api/v1/issues "$(jq -cn --arg p "$removal_project" --rawfile s "$work/spec-person.md" \
  --argjson a "$(seed_actor)" '{project: $p, title: "The ask a person removes", spec: $s, actor: $a}')"
want_status 201 removal "create the issue of $removal_project through the base release"
field removal "the key of the issue of $removal_project" .key
readonly issue_p="$value"
field removal "the spec of $issue_p" .primary_artifact_id
readonly spec_p="$value"
ask_row removal "$issue_p" "$spec_p" q-person
readonly person_ask="$value"
cat >"$work/spec-printed.md" <<'MD'
# An agent's drop

:::requirement{#r-printed}
The corpus is printed once a week.
:::

:::ask{#q-printed urgency="med"}
Which day is the corpus printed?
:::
MD
send agent POST /api/v1/issues "$(jq -cn --arg p "$project" --rawfile s "$work/spec-printed.md" \
  --argjson a "$(seed_actor)" '{project: $p, title: "The ask an agent drops", spec: $s, actor: $a}')"
want_status 201 removal "create an issue of $project through the base release"
field removal "the key of the issue of $project" .key
readonly issue_e="$value"
field removal "the spec of $issue_e" .primary_artifact_id
readonly spec_e="$value"
ask_row removal "$issue_e" "$spec_e" q-printed
readonly dropped_ask="$value"
readonly dropped_answer="Monday, before the first reading."
answer_block removal "$person_ask" "$spec_p" q-person "The top shelf, beside the census."
answer_block removal "$dropped_ask" "$spec_e" q-printed "$dropped_answer"
seed_wait_settled "the answers of $issue_p and $issue_e settle"

# The decisions.md of the person's issue, of the old shape: one section, which cites the ask.
readonly removal_heading="The printed corpus stands on the top shelf"
printf '# Decisions\n\n## %s\n\nThe person chose the top shelf, beside the census. Decided in dispatch://%s/ask/%s.\n' \
  "$removal_heading" "$issue_p" "$person_ask" >"$work/decisions-person.md"
send agent POST "/api/v1/issues/$issue_p/artifacts" "$(jq -cn --rawfile c "$work/decisions-person.md" \
  --argjson a "$(seed_actor)" '{name: "decisions.md", content: $c, actor: $a}')"
want_status 201 removal "upload the decisions.md of $issue_p through the base release"
field removal "the decisions.md of $issue_p" .artifact.id
readonly decisions_p="$value"

# An agent's session drops the answered q-printed.
send agent POST "/api/v1/artifacts/$spec_e/edits" \
  "$(jq -cn --argjson a "$(seed_actor)" '{ops: [{op: "delete", block: "q-printed"}], actor: $a}')"
want_status 200 removal "an agent drops the answered q-printed from the spec of $issue_e through the base release"
seed_wait_settled "the decisions.md of $issue_p and the drop of q-printed settle"

# The heading the base's Decisions read gives that section, which it reads citing the ask.
send agent GET "/api/v1/issues/$issue_p/decisions"
want_status 200 removal "read the Decisions of $issue_p through the base release"
heading_id="$(jq -er --arg h "$removal_heading" --arg a "$person_ask" \
  '[.decisions[] | select(.heading == $h and (.asks | index($a)))] | if length == 1 then .[0].block_id else error end' \
  "$work/body" 2>/dev/null)" ||
  fail removal "the base's Decisions read holds one section of $issue_p citing $person_ask" "$(body)"
readonly heading_id
pass removal "the decisions.md of $issue_p holds one section, heading block $heading_id, citing the answered $person_ask"

# The version that dropped q-printed, the first that holds no block of it, names a session with no
# act of a person.
query removal "read the version of $spec_e that dropped q-printed" \
  "select coalesce(min(number)::text, '') from artifact_versions where artifact_id = '$spec_e' and position(':::ask{#q-printed ' in markdown) = 0"
readonly drop_version="$answer"
[[ "$drop_version" =~ ^[1-9][0-9]*$ ]] || fail removal "a version of $spec_e drops q-printed" "none does"
query removal "read the authors of version $drop_version of $spec_e" \
  "select authors::text from artifact_versions where artifact_id = '$spec_e' and number = $drop_version"
jq -e --arg s "$seed_session" 'length == 1 and .[0].kind == "session" and .[0].id == $s and .[0].act == null' \
  <<<"$answer" >/dev/null ||
  fail removal "the base records the drop of q-printed as a session with no act of a person" "version $drop_version names $answer"
pass removal "the base recorded version $drop_version of $spec_e, which drops the answered q-printed, as written by $answer: a session with no act of a person"
baseline_stop_pid "$base_pid"
base_pid=""

# This head, held, takes the person's removal: the step's first phase states the rows the base
# wrote, as a boot's would, and the person puts a paragraph in place of the answered q-person.
baseline_boot head-held "$head_binary" "$baseline_database_url"
head_pid="$baseline_boot_pid"
server="$baseline_boot_url"
send agent GET /healthz
want_status 200 removal "read /healthz of this head, held"
[[ "$(jq -r '.commit // ""' "$work/body")" == "$head_commit" ]] ||
  fail removal "this head booted again serves $head_commit" "it serves '$(jq -r '.commit // ""' "$work/body")'"
sign_in removal
dispatch_command backfill-again backfill-graph-store
[[ "$command_code" == 0 ]] || fail removal "envoy-dispatch backfill-graph-store" "exited $command_code: $(<"$work/backfill-again.out")"
wait_settled removal
latest_markdown removal "$spec_p"
readonly before_removal="$version"
[[ -n "$(ask_block q-person "$work/latest.md")" ]] ||
  fail removal "the spec of $issue_p holds q-person before the person's removal" "version $version holds no such block"
readonly removal_paragraph="The person took this question back: no shelf is decided here."
send person POST "/api/v1/artifacts/$spec_p/edits" "$(jq -cn --arg m "$removal_paragraph" \
  '{ops: [{op: "insert", after: "block:q-person", markdown: $m}, {op: "delete", block: "q-person"}]}')"
want_status 200 removal "the person puts a paragraph in place of the answered q-person"
wait_settled removal
latest_markdown removal "$spec_p"
[[ "$version" -gt "$before_removal" ]] || fail removal "the person's removal writes a version of $spec_p" "it is still at $version"
readonly person_version="$version"
[[ -z "$(ask_block q-person "$work/latest.md")" ]] ||
  fail removal "the person's removal takes q-person out of $spec_p" "version $person_version still holds it"
grep -Fqx -- "$removal_paragraph" "$work/latest.md" ||
  fail removal "the person's paragraph stands in place of q-person" "version $person_version holds no such paragraph"
query removal "read the authors of version $person_version of $spec_p" \
  "select authors::text from artifact_versions where artifact_id = '$spec_p' and number = $person_version"
readonly person_authors="$answer"
jq -e --arg p "$seed_person" 'any(.[]; .kind == "user" and .id == $p)' <<<"$person_authors" >/dev/null ||
  fail removal "version $person_version of $spec_p is the person's" "it names $person_authors"
removals_of "$person_ask"
[[ "$answer" == "person:$seed_person:true" ]] ||
  fail removal "the person's removal leaves one asserted removed_by from $person_ask to $seed_person" "${answer:-none}"
readonly person_removals="$answer"
pass removal "the person's version $person_version of $spec_p puts a paragraph in place of q-person, written by $person_authors, and leaves the removed_by tuple $person_removals"
baseline_stop_pid "$head_pid"
head_pid=""

# This head without the hold settings, alone, so its step's first tick runs the restore and the
# conversion: the window of 15 s is processes.LiveWindow.
waited=0
while true; do
  query step "read the live server processes" \
    "select count(*) from server_processes where heartbeat_at > now() - interval '15 seconds'"
  [[ "$answer" != 0 ]] || break
  [[ "$waited" -lt 300 ]] || fail step "no server process is live before this head boots without the hold" "$answer still are after 30 s"
  sleep 0.1
  waited=$((waited + 1))
done
# The newest event before this head boots bounds the artifact.version events of the versions its
# step writes on its ticks.
query step "read the newest event before the step's ticks" "select coalesce(max(id), 0) from events"
readonly before_ticks="$answer"
baseline_extra_env=(DISPATCH_TEST_HOOKS=1)
baseline_boot head-unheld "$head_binary" "$baseline_database_url"
head_pid="$baseline_boot_pid"
server="$baseline_boot_url"
readonly step_log="$baseline_log"
waited=0
while true; do
  first_tick="$(grep -m 1 -F 'msg="dispatch: decisions step counted what it owes"' "$step_log" || true)"
  [[ -z "$first_tick" ]] || break
  [[ "$waited" -lt 1200 ]] || fail step "the step's first tick reports what it owes" "no such line 120 s after this head listened"
  sleep 0.1
  waited=$((waited + 1))
done
printf "the step's first tick: %s\n" "$first_tick"
printf 'waiting three tick intervals of %s s after it, and 10 s more for the third tick to end\n' "$tick_seconds"
sleep $((3 * tick_seconds + 10))
grep -F 'msg="dispatch: decisions step ' "$step_log" >"$work/report" || true
printf 'the report lines of every tick:\n'
sed 's/^/  /' "$work/report"

# The person's removal stays: the spec holds no block of the ask, its newest version is the
# person's, and no tick reports a restore of it. Each failure names the version that wrote the
# block back, where one did.
send agent GET "/api/v1/asks/$person_ask"
want_status 200 step "read $person_ask after the ticks"
field step "the block id the row of $person_ask names" '.ask.block_id // "q-person"'
readonly person_block="$value"
latest_markdown step "$spec_p"
[[ -z "$(ask_block "$person_block" "$work/latest.md")$(ask_block q-person "$work/latest.md")" ]] || {
  restorer_of step "$spec_p" "$person_version" "$person_block" q-person
  fail step "the spec of $issue_p holds no block of $person_ask, the answered ask the person removed" "$restorer"
}
[[ "$version" == "$person_version" ]] || {
  restorer_of step "$spec_p" "$person_version" "$person_block" q-person
  query step "read the authors of version $version of $spec_p" \
    "select authors::text from artifact_versions where artifact_id = '$spec_p' and number = $version"
  fail step "the newest version of $spec_p is $person_version, the person's" \
    "it is $version, written by $answer; of $person_ask: $restorer"
}
pass step "after three ticks $spec_p holds no block of $person_ask, and its newest version is $person_version, the person's"
restores="$(grep -F 'msg="dispatch: decisions step restored an answered ask"' "$work/report" | grep -F " ask=$person_ask " || true)"
[[ -z "$restores" ]] || {
  restorer_of step "$spec_p" "$person_version" "$person_block" q-person
  fail step "no tick reports a restore of $person_ask" "$restores; $restorer"
}
issue_lines="$(grep -F 'msg="dispatch: decisions step restored and converted an issue"' "$work/report" | grep -F " issue=$issue_p " || true)"
[[ -n "$issue_lines" ]] || fail step "a tick reports what it did of $issue_p" "no report line names $issue_p"
! grep -Eq ' (restored|declined)=[1-9]' <<<"$issue_lines" || {
  restorer_of step "$spec_p" "$person_version" "$person_block" q-person
  fail step "no tick restores or declines an ask of $issue_p" "$issue_lines; of $person_ask: $restorer"
}
removals_of "$person_ask"
[[ "$answer" == "$person_removals" ]] ||
  fail step "the ticks append no removed_by to $person_ask" "it holds $answer, was $person_removals"
pass step "no tick's report restores $person_ask, and its node holds the one removed_by the person's write left: $person_removals"

# The section that cites it is deleted by R9's rule for an answered ask a person removed: its
# heading address answers the ask through one moved_to tuple, and the decisions.md retires.
grep -Fq ' restored=0 declined=0 decisions=0 readings=0 dropped=0 removed=1 left=0 retired=true ' <<<"$issue_lines" ||
  fail section "a tick deletes the one section of $issue_p by the rule for an ask a person removed, and retires its decisions.md" "$issue_lines"
pass section "a tick reports the one section of $issue_p deleted by the rule for an ask a person removed (removed=1), and its decisions.md retired"
query section "read the moved_to tuples of $issue_p/decisions-md#$heading_id" \
  "select coalesce(string_agg(to_kind || ':' || to_id || ':' || asserted::text, ' ' order by seq), '') from graph_tuples
   where kind = 'moved_to' and from_kind = 'block' and from_id = '$issue_p/decisions-md#$heading_id'"
[[ "$answer" == "ask:$person_ask:true" ]] ||
  fail section "one moved_to tuple from $issue_p/decisions-md#$heading_id to the ask $person_ask" "${answer:-none}"
send agent GET "/api/v1/references?to=$(q "dispatch://$issue_p/ask/$person_ask")"
want_status 200 section "resolve dispatch://$issue_p/ask/$person_ask"
jq -cS '.node' "$work/body" >"$work/ask-node.json"
send agent GET "/api/v1/references?to=$(q "dispatch://$issue_p/artifact/decisions-md#b-$heading_id")"
want_status 200 section "resolve the heading address dispatch://$issue_p/artifact/decisions-md#b-$heading_id"
jq -cS '.node' "$work/body" >"$work/heading-node.json"
jq -e --arg a "$person_ask" '.kind == "ask" and .id == $a' "$work/heading-node.json" >/dev/null ||
  fail section "the heading address answers the ask $person_ask" "it answers $(<"$work/heading-node.json")"
cmp -s "$work/ask-node.json" "$work/heading-node.json" ||
  fail section "the heading address answers the ask as the ask's own address does" \
    "got $(<"$work/heading-node.json"), want $(<"$work/ask-node.json")"
pass section "dispatch://$issue_p/artifact/decisions-md#b-$heading_id answers the ask $person_ask through one moved_to tuple, the node its own address answers: $(jq -c '{kind, id, ref, parent}' "$work/heading-node.json")"
query section "read the decisions.md of $issue_p" \
  "select (role is null)::text || ' ' || (retired_at is not null)::text from artifacts where id = '$decisions_p'"
[[ "$answer" == "true true" ]] || fail section "the decisions.md of $issue_p is retired, with no role" "role null and retired read: $answer"
pass section "the decisions.md of $issue_p is retired, with no role"

# Nothing is owed: not for that issue, and not on this head's count.
query owed "read the decisions documents of $issue_p" "select count(*) from decisions_documents() where issue_key = '$issue_p'"
[[ "$answer" == 0 ]] || fail owed "no decisions document of $issue_p stands" "decisions_documents() lists $answer"
send agent GET /healthz
want_status 200 owed "read /healthz of this head"
step_owed="$(jq -r '.decisions_step_owed // "absent"' "$work/body")"
[[ "$step_owed" == 0 ]] ||
  fail owed "/healthz answers decisions_step_owed zero after the ticks" \
    "it answers $step_owed; the step's last count: $(grep -F 'msg="dispatch: decisions step counted what it owes"' "$step_log" | tail -n 1)"
pass owed "/healthz answers decisions_step_owed 0 after the ticks, and decisions_documents() lists nothing of $issue_p"

# In the same run the step restores the answered ask an agent's session dropped.
agent_restores="$(grep -F 'msg="dispatch: decisions step restored an answered ask"' "$work/report" | grep -F " ask=$dropped_ask " || true)"
[[ -n "$agent_restores" && "$(wc -l <<<"$agent_restores")" == 1 ]] ||
  fail agent "one tick reports the restore of $dropped_ask" "${agent_restores:-no report line names it}"
[[ "$agent_restores" == *" issue=$issue_e "* && "$agent_restores" == *" read=no_person "* ]] ||
  fail agent "the restore of $dropped_ask reads the version that dropped it as no person's" "$agent_restores"
send agent GET "/api/v1/asks/$dropped_ask"
want_status 200 agent "read $dropped_ask after the ticks"
expect agent "$dropped_ask names $spec_e as its block's document" "\"$spec_e\"" '.ask.block_artifact.id'
expect agent "$dropped_ask keeps its block id q-printed" '"q-printed"' '.ask.block_id'
expect agent "$dropped_ask keeps its answer" "$(jq -cn --arg t "$dropped_answer" '$t')" '.ask.answer.text'
latest_markdown agent "$spec_e"
[[ -n "$(ask_block q-printed "$work/latest.md")" ]] ||
  fail agent "the spec of $issue_e holds q-printed again" "version $version holds no such block"
pass agent "the answered q-printed, which version $drop_version dropped as a session with no act of a person, is restored in the same run, at version $version of $spec_e: $agent_restores"

# Every ask block of each version the step wrote on its ticks - the restore of q-printed and the
# conversion of the issue of PERSON among them - holds the form R10 keeps, as those of the versions
# the restore by hand wrote do.
step_events tick "$before_ticks" ""
cp "$work/step-events" "$work/tick.versions"
[[ -s "$work/tick.versions" ]] ||
  fail tick "the step wrote a version on its ticks" "no artifact.version event in its name after event $before_ticks"
printf 'the versions the step wrote on its ticks (document number): %s\n' "$(tr '\n' ';' <"$work/tick.versions")"
while read -r document number <&3; do
  kept_form tick "$document" "$number" "a version the step wrote on its tick"
done 3<"$work/tick.versions"
pass tick "every ask block of the $(wc -l <"$work/tick.versions") versions the step wrote on its ticks, the restore of q-printed among them, holds the form R10 keeps, each value its ask's node's"

# ---------------------------------------------------------------------------------------------
# A drop by a broker token the secrets broker scoped to a person stays.
# ---------------------------------------------------------------------------------------------
# No shell driver can present that token, which signs a DPoP proof for each request, so a Go test
# carries the row; this head stops first, so the test's build and servers run alone.
baseline_stop_pid "$head_pid"
head_pid=""
readonly broker_test=TestADropByAPersonScopedBrokerTokenUnderTheBaseStays
broker_code=0
(
  cd -- "$envoy_dir"
  env DISPATCH_TEST_DATABASE_URL="$test_database_url" BASELINE_SHA="$BASELINE_SHA" \
    go test -tags basebuild -count=1 -v -timeout 30m -run "^${broker_test}\$" ./cmd/dispatch
) >"$work/broker.out" 2>&1 || broker_code=$?
broker_failure=""
if [[ "$broker_code" != 0 ]]; then
  broker_failure="it exited $broker_code"
elif ! grep -q "^--- PASS: $broker_test " "$work/broker.out"; then
  broker_failure="it printed no --- PASS: line of $broker_test"
elif grep -q -- '--- SKIP' "$work/broker.out"; then
  broker_failure="it skipped: $(grep -- '--- SKIP' "$work/broker.out")"
fi
[[ -z "$broker_failure" ]] || {
  printf 'the output of go test -run ^%s$ ./cmd/dispatch:\n' "$broker_test"
  sed 's/^/  /' "$work/broker.out"
  fail broker "go test -tags basebuild -run ^$broker_test\$ ./cmd/dispatch passes and skips nothing" "$broker_failure"
}
sed -n "/^--- PASS: $broker_test /p" "$work/broker.out"
pass broker "$broker_test passes and skips nothing: under the base release a broker token scoped to a person drops an answered ask, and this head's step leaves it out, with one removed_by to that person"
