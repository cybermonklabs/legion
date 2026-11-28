# shellcheck shell=bash
# What the issue-tree acceptance drivers share, so that no driver copies it: the request to a
# server as the agent or as the person, the person's dev sign-in and the project create that names
# that person, the tree read (one page or every page), the two CLIs, the census's `census: versions`
# line R16 reads, and for a driver over rows an earlier release wrote the database it empties, the
# boot of this head over those rows, the wait for the backfill, the wait for this head's build
# announcement, the conversion and what it left; the checks of an issue key and a uuid a driver's
# SQL names; a node's tuples and its move; the person's edit; and the document socket peer.
# Sourced, never run, after lib.sh, and for an upgrade driver after lib-baseline.sh too, which this
# file reads but does not source: a driver against `bun run dispatch:local` boots no server and has
# no use for it.
#
# The checks it serves are every driver check of the issue-tree Verification document
# (verification-md), and seed-corpus.sh, which writes the corpus several of them read:
#   against `bun run dispatch:local` on infra_1253: c-reparent-and-state, c-coverage-states,
#     c-issue-relations-read, c-conversation-holds-the-discussion, c-tree-read-views,
#     c-child-placement and c-child-view;
#   over rows the earlier release wrote, on infra_1253_upgrade: c-tree-census, c-one-room,
#     c-room-cutover, c-order, c-conversion-scope, c-shared-id-census, c-redirects, c-tree-page,
#     c-queries, c-artifacts-in-tree, c-old-versions-render, c-one-dependency, c-view-arms and
#     c-reparent-and-state's second driver, issue-tree-text.sh.
#
# The inputs are the caller's own variables, which each function reads when it runs, not when this
# file is read. They are named on the lines after this comment, so a reader of this file sees
# them, as does shellcheck, which reads one file at a time; lib.sh names the first four too:
#   work          the run's directory (lib.sh's work_dir); every answer, jar and header file is here
#   status        the last answer's HTTP status, which each request here writes and tree_want reads
#   base          the address of the server a request goes to, http://127.0.0.1:<port>. An upgrade
#                 driver changes it from baseline_url to tree_head_url between its two halves
#   database_url  the database that server serves, which every psql read here reads. An upgrade
#                 driver leaves it writable: lib-baseline.sh's baseline_boot takes a local of that
#                 name, which a readonly variable refuses
#   token         the agent bearer (lib.sh's read_bearer), written to a header file by tree_init
#   session       the agent session every agent write names in its actor, and lib.sh's `cli` too
#   project       the project key (the check's PROJECT), which the conversion converts
#   answer        what lib.sh's query read, which the two waits read after each query
#   driver        the script's name, which the database refusals name (lib.sh names it too)
#   sections      the caller's count of sections, which tree_section adds one to
# From lib-baseline.sh, after its baseline_start: baseline_database_url, baseline_boot (with the
# baseline_boot_pid, baseline_boot_url and baseline_log it leaves), baseline_read_extra_env and
# baseline_stop_pid.
#
# What it leaves its caller: `status` and $work/body after each request, which lib.sh's
# want_status, expect and body read; tree_last_request, the request the last answer came from;
# tree_value, the value tree_field read; tree_person, the login tree_sign_in signed in; $work/out,
# the output of the last CLI run, and tree_envoy_status, the exit status of the last
# `envoy-dispatch`; tree_census_line, the `census: versions` line tree_census_versions read;
# tree_head_pid, tree_head_url, tree_head_log and tree_head_commit after tree_boot_head; and what
# each function below names as its answer where it says so (database_server, outcome, first_seq,
# tree_pages, tree_node_kind, tree_block_node_id, reply, fragments and the like).
#
# A refused input or call exits 2 on stderr, as lib.sh's require_env does; a failed check prints
# lib.sh's `FAIL: <section> <what>: <observed>` and exits 1. Every check is `cmd || fail …`, never
# part of an `&&` list: under `set -e` a failing command anywhere but last in such a list does not
# stop the script.
: "${work:=}" "${status:=}" "${base:=}" "${database_url:=}" "${token:=}" "${session:=}" "${project:=}"
: "${answer:=}" "${driver:=}" "${sections:=}"
: "${baseline_database_url:=}" "${baseline_boot_pid:=}" "${baseline_boot_url:=}" "${baseline_log:=}"

tree_last_request=""
tree_value=""
tree_person=""
tree_head_pid=""
tree_head_url=""
tree_head_log=""
tree_head_commit=""
tree_envoy_status=""
tree_census_line=""

# The settings every upgrade boot of this head takes, as the Verification preamble states: with
# them this head holds the conversion step of R6 at boot and on its tick, so it converts an issue
# only when a driver runs `envoy-dispatch convert-issues`. The one boot without them is the
# automatic case of c-conversion-scope (tree_boot_head's `unheld`). Production sets neither.
readonly tree_hold_env=$'DISPATCH_TEST_HOOKS=1\nDISPATCH_TEST_HOLD_ISSUE_ROOM_STEP=1'

# The line the boot step logs once its run of graphcensus.Backfill's passes, which it runs at every
# boot, has ended (startBootBackfill in cmd/dispatch/main.go). tree_wait_backfill reads the
# server's log for it as written here, so that step and this constant name one line and are
# changed together.
readonly tree_backfill_end='msg="dispatch: boot backfill done"'

# How long each wait below polls, and how often: the Verification preamble's 300 s and 2 s.
readonly tree_wait_seconds=300
readonly tree_poll_seconds=2

# How long the socket peer waits for a socket's answer; tree_peer waits 30 s more for one line of
# it.
readonly tree_peer_answer_seconds=60

# tree_refuse WORDS…: a call this file will not take. It exits 2, as lib.sh's refusals do.
tree_refuse() {
  printf 'lib-tree.sh: %s\n' "$*" >&2
  exit 2
}

# tree_init: writes the agent's header file, $work/auth, from the caller's `token`, so that no
# command line holds the bearer. Inputs: `work` (lib.sh's work_dir, whose umask keeps the file the
# run's own) and `token` (lib.sh's read_bearer). Call it once, before the first request.
tree_init() {
  [[ -n "$work" && -d "$work" ]] || tree_refuse "tree_init writes under \$work: call lib.sh's work_dir first"
  [[ -n "$token" ]] || tree_refuse "tree_init needs the agent bearer in \$token: call lib.sh's read_bearer first"
  require_commands curl jq psql
  printf 'Authorization: Bearer %s\n' "$token" >"$work/auth"
}

# tree_request AS METHOD PATH [BODY]: sends METHOD PATH to $base as AS: `agent`, with the bearer of
# $work/auth, or `person`, with the cookie tree_sign_in left in $work/jar and the server's origin.
# BODY is JSON, or @FILE for JSON held in a file. The answer's body goes to $work/body, its HTTP
# status to `status`, and "METHOD PATH" to tree_last_request, which tree_want names in a failure.
tree_request() {
  local as="$1" method="$2" path="$3"
  local -a auth data=()
  case "$as" in
    agent) auth=(-H "@$work/auth") ;;
    person)
      [[ -n "$tree_person" ]] || tree_refuse "a person's request needs a signed-in person: call tree_sign_in first"
      auth=(-b "$work/jar" -H "Origin: $(base_origin)")
      ;;
    *) tree_refuse "tree_request sends as agent or person, not '$as'" ;;
  esac
  [[ $# -lt 4 ]] || data=(-H 'Content-Type: application/json' --data-binary "$4")
  tree_last_request="$method $path"
  status="$(curl -sS -X "$method" -o "$work/body" -w '%{http_code}' "${auth[@]}" "${data[@]}" \
    "$base$path" 2>"$work/curl")" || status="curl failed: $(<"$work/curl")"
}

# tree_send METHOD PATH [BODY]: tree_request as the agent. tree_person_send: as the person.
tree_send() { tree_request agent "$@"; }
tree_person_send() { tree_request person "$@"; }

# tree_want CODE SECTION WHAT: the last answer's status is CODE; otherwise FAIL names the request,
# the status it answered and the head of its body.
tree_want() {
  [[ "$status" == "$1" ]] ||
    fail "$2" "$3" "$tree_last_request answered HTTP $status, want $1: $(body)"
}

# tree_field SECTION WHAT JQ: the one value JQ reads out of the last answer's body, which must be
# there, in tree_value. It sets a variable rather than printing the value: a fail inside `$(…)`
# would end only that subshell, and the driver would go on with an empty value.
# shellcheck disable=SC2034 # tree_value is this function's answer, read by its caller
tree_field() {
  tree_value="$(jq -er "$3" "$work/body" 2>/dev/null)" ||
    fail "$1" "$2" "no $3 in the answer to $tree_last_request: $(body)"
}

# tree_actor: the actor member of every agent write, the caller's `session`.
tree_actor() {
  [[ -n "$session" ]] || tree_refuse "an agent write names \$session in its actor: set it first"
  jq -cn --arg s "$session" '{kind: "session", id: $s}'
}

# tree_sign_in SECTION LOGIN: signs LOGIN in through the server's dev sign-in,
# GET /auth/_dev/signin?login=LOGIN, which answers a redirect that sets the session cookie, and
# keeps that cookie in $work/jar for every person's request after it. Every server lib-baseline.sh
# boots signs the cookie with the one key it holds for the run, and curl's jar keys a cookie by host
# and not by port, so the one sign-in holds for each server a driver boots over that database.
# Inputs: `base`, `work`, and LOGIN (the check's DEV_SIGNIN_LOGIN, `alice`). It sets tree_person.
tree_sign_in() {
  local section="$1" login="$2"
  [[ -n "$login" ]] || tree_refuse "tree_sign_in needs a login"
  tree_last_request="GET /auth/_dev/signin?login=$login"
  status="$(curl -sS -o "$work/body" -w '%{http_code}' -c "$work/jar" \
    "$base/auth/_dev/signin?login=$(q "$login")" 2>"$work/curl")" ||
    status="curl failed: $(<"$work/curl")"
  tree_want 302 "$section" "dev sign-in as $login"
  grep -q $'\tdsession\t' "$work/jar" ||
    fail "$section" "dev sign-in as $login" "the answer set no session cookie"
  tree_person="$login"
  pass "$section" "signed in as $login"
}

# tree_create_project SECTION KEY NAME: creates the project KEY, named NAME, as the agent, with
# default_assignee set to the person tree_sign_in signed in: a create by the agent bearer, which
# acts for no person, must name a person who has signed in (api/projects.go), so the sign-in comes
# first. Inputs: `session`, and tree_person.
tree_create_project() {
  local section="$1" key="$2" name="$3"
  [[ -n "$tree_person" ]] ||
    tree_refuse "the project's create names a signed-in person as its default_assignee: call tree_sign_in first"
  tree_send POST /api/v1/projects "$(jq -cn --arg k "$key" --arg n "$name" --arg p "$tree_person" \
    --argjson a "$(tree_actor)" '{key: $k, name: $n, default_assignee: $p, actor: $a}')"
  tree_want 201 "$section" "create the project $key"
  expect "$section" "the project $key names $tree_person as its default assignee" \
    "$(jq -cn --arg k "$key" --arg p "$tree_person" '[$k, $p]')" '[.key, .default_assignee]'
}

# tree_read AS ISSUE [VIEW [V [CURSOR]]]: reads ISSUE's tree, GET /api/v1/issues/{key}/tree, as AS
# (agent or person), with each of `view` (spec, verification or plan), `v` (a version of the
# query's node) and `cursor` (the next node of the walk a page answered) that is given and not
# empty, and no other parameter: those three are the route's whole contract (the plan's T16a). The
# answer is in $work/body and `status`, as every request's is.
tree_read() {
  local as="$1" issue="$2" view="${3:-}" version="${4:-}" cursor="${5:-}" parameters=""
  [[ -z "$view" ]] || parameters+="&view=$(q "$view")"
  [[ -z "$version" ]] || parameters+="&v=$(q "$version")"
  [[ -z "$cursor" ]] || parameters+="&cursor=$(q "$cursor")"
  tree_request "$as" GET "/api/v1/issues/$(q "$issue")/tree${parameters:+?${parameters#&}}"
}

# tree_cli_init: readies lib.sh's `cli` and `run`, the `dispatch` client an agent session runs:
# it sets `cli_entry` to this checkout's packages/envoy-client/bin/dispatch.ts where the caller
# set none, and saves what `cli` reads - `bun_bin`, `saved_path` and `saved_tmpdir` - and makes
# the `home` and `state` directories under $work, as lib.sh's cli_env does. It does not call
# cli_env, which makes `saved_path` readonly: lib-baseline.sh's baseline_boot takes a local of that
# name, which a readonly variable refuses, so after cli_env no upgrade driver could boot a server.
# Inputs: `work`, and `bun` on PATH. Call it once, after work_dir; `run` then reads `base`,
# `token` and `session` at each call.
# shellcheck disable=SC2034 # bun_bin, saved_path and saved_tmpdir are what lib.sh's cli reads
tree_cli_init() {
  require_commands bun
  if [[ -z "${cli_entry:-}" ]]; then
    cli_entry="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../../../envoy-client/bin" && pwd)/dispatch.ts" ||
      tree_refuse "could not find packages/envoy-client/bin from $(dirname -- "${BASH_SOURCE[0]}")"
  fi
  [[ -r "$cli_entry" ]] || tree_refuse "the dispatch client $cli_entry cannot be read"
  bun_bin="$(command -v bun)"
  saved_path="$PATH"
  saved_tmpdir="${TMPDIR:-}"
  mkdir "$work/home" "$work/state"
}

# tree_envoy_dispatch_status ARGS…: runs `envoy-dispatch ARGS` - the server's own command line, the
# build the Verification preamble stamps with this head's commit and puts on PATH - over
# $database_url, its output in $work/out and its exit status in tree_envoy_status, which it does
# not judge. It runs in an environment holding only PATH, TMPDIR, HOME and DATABASE_URL, so nothing
# a calling shell exported (a deployment's Dispatch address, a token file, a PGHOSTADDR) reaches
# it. Inputs: `database_url`, `work`.
tree_envoy_dispatch_status() {
  command -v envoy-dispatch >/dev/null ||
    tree_refuse "envoy-dispatch is not on PATH: build ./cmd/dispatch stamped with this head's commit, as the Verification preamble does"
  tree_envoy_status=0
  (
    set +x
    local name path_kept="$PATH" tmpdir_kept="${TMPDIR:-}"
    local -a exported
    mapfile -t exported < <(compgen -e)
    for name in "${exported[@]}"; do
      unset "$name" 2>/dev/null || true
    done
    export PATH="$path_kept" HOME=/nonexistent DATABASE_URL="$database_url"
    if [[ -n "$tmpdir_kept" ]]; then
      export TMPDIR="$tmpdir_kept"
    fi
    exec envoy-dispatch "$@"
  ) >"$work/out" 2>&1 || tree_envoy_status=$?
}

# tree_envoy_dispatch SECTION WHAT ARGS…: tree_envoy_dispatch_status ARGS, where a non-zero exit
# fails, naming the command, its status and the end of its output.
tree_envoy_dispatch() {
  local section="$1" what="$2"
  shift 2
  tree_envoy_dispatch_status "$@"
  [[ "$tree_envoy_status" == 0 ]] ||
    fail "$section" "$what" "envoy-dispatch $* exited $tree_envoy_status: $(tail -n 20 "$work/out")"
}

# tree_census_versions SECTION WHAT [OVER [OUTPUT]]: R16's reading of the census's line
# `census: versions rendered_from_tree=… deferred=… tree_mismatch=… tree_mismatch_repaired=…
# differences=…`, which c-tree-census, c-reparent-and-state, c-redirects and c-artifacts-in-tree
# each make: no writer of the product writes a version whose tree does not render as its stored
# markdown, and the server's reconciliation repairs a mark on its next quiet tick, after which only
# tree_mismatch_repaired shows it, so both read 0. OUTPUT is the output of an `envoy-dispatch census
# --store` the caller has run already; where it names none, this runs the census over $database_url
# (tree_envoy_dispatch_status), its output in $work/out, and fails on an exit above 1 (1 is the
# census's refusal, whose reasons are classes other than this line's). It prints the line and leaves
# it in tree_census_line, whose members tree_census_member reads in any order. A line that does not
# read tree_mismatch=0 and tree_mismatch_repaired=0, or no line at all, fails with WHAT, the
# caller's message, and `the census reads tree_mismatch=<n> tree_mismatch_repaired=<m>, want 0 and
# 0: <line>`, with ` over OVER` before the comma where the caller names the corpus the census read.
# Inputs: `database_url`, `work`.
tree_census_versions() {
  local section="$1" what="$2" over="${3:-}" output="${4:-}" mismatch repaired
  if [[ -z "$output" ]]; then
    tree_envoy_dispatch_status census --store
    [[ "$tree_envoy_status" -le 1 ]] ||
      fail "$section" "$what" "envoy-dispatch census --store exited $tree_envoy_status: $(tail -n 20 "$work/out")"
    output="$work/out"
  fi
  tree_census_line="$(grep -m 1 '^census: versions ' "$output")" ||
    fail "$section" "$what" "the census printed no line 'census: versions …'${over:+ over $over}: $(tail -n 20 "$output")"
  printf '%s\n' "$tree_census_line"
  mismatch="$(tree_census_member tree_mismatch)"
  repaired="$(tree_census_member tree_mismatch_repaired)"
  [[ "$mismatch" == 0 && "$repaired" == 0 ]] ||
    fail "$section" "$what" \
      "the census reads tree_mismatch=${mismatch:-(none)} tree_mismatch_repaired=${repaired:-(none)}${over:+ over $over}, want 0 and 0: $tree_census_line"
  pass "$section" "the census reads tree_mismatch=0 tree_mismatch_repaired=0${over:+ over $over}"
}

# tree_census_member NAME: the value of the member NAME=<n> of tree_census_line, its thousands'
# commas dropped; nothing where the line holds no such member. A member is read between spaces, so
# tree_mismatch is never read off tree_mismatch_repaired.
tree_census_member() {
  if [[ " $tree_census_line " =~ [[:space:]]$1=([0-9,]+)[[:space:]] ]]; then
    printf '%s' "${BASH_REMATCH[1]//,/}"
  fi
}

# tree_convert SECTION [--dry-run]: the conversion R6 names, `envoy-dispatch convert-issues
# --project <project>`, with --dry-run where it is given, its output in $work/out. Inputs:
# `project`, `database_url`. A driver calls it after tree_wait_backfill and tree_wait_announcement,
# since the command refuses unless the newest announcement names its own commit.
tree_convert() {
  local section="$1" dry=""
  [[ -n "$project" ]] || tree_refuse "the conversion converts \$project: set it to the check's PROJECT"
  case "${2:-}" in
    "") ;;
    --dry-run) dry=--dry-run ;;
    *) tree_refuse "tree_convert takes --dry-run or nothing, not '$2'" ;;
  esac
  tree_envoy_dispatch "$section" "convert-issues --project $project${dry:+ $dry}" \
    convert-issues --project "$project" ${dry:+"$dry"}
  pass "$section" "convert-issues --project $project${dry:+ $dry} ran"
}

# tree_boot_head NAME [MODE [BINARY]]: boots this head's server over the database the baseline
# wrote, as lib-baseline.sh's own --selftest boots its branch server: through baseline_boot, on a
# port held-port.py holds, logging to $baseline_work/logs/NAME.log. BINARY is the server, by
# default the stamped `envoy-dispatch` on PATH. MODE `held`, the default, adds tree_hold_env's two
# pairs to BASELINE_EXTRA_ENV for this boot alone, through lib-baseline.sh's own reading of that
# variable, so the baseline server never gets them and every pair the caller gave still reaches
# this one; `unheld` boots without them, for c-conversion-scope's automatic case. It reads the
# commit the server reports on /healthz and refuses a build that reports none, since an unstamped
# build holds back every announcement (servedByEvery). Inputs: baseline_start's
# baseline_database_url. It leaves tree_head_pid, tree_head_url, tree_head_log and
# tree_head_commit; the caller's cleanup stops the server with tree_stop_head before baseline_stop.
tree_boot_head() {
  local name="$1" mode="${2:-held}" binary="${3:-}" given="${BASELINE_EXTRA_ENV-}"
  declare -F baseline_boot >/dev/null ||
    tree_refuse "tree_boot_head boots through lib-baseline.sh: source it before this file"
  [[ -n "$baseline_database_url" ]] ||
    tree_refuse "tree_boot_head boots over the baseline's database: call baseline_start first"
  if [[ -z "$binary" ]]; then
    binary="$(command -v envoy-dispatch)" ||
      tree_refuse "envoy-dispatch is not on PATH: build ./cmd/dispatch stamped with this head's commit, as the Verification preamble does"
  fi
  case "$mode" in
    held) BASELINE_EXTRA_ENV="${given:+$given$'\n'}$tree_hold_env" ;;
    unheld) ;;
    *) tree_refuse "tree_boot_head boots held or unheld, not '$mode'" ;;
  esac
  baseline_read_extra_env
  baseline_boot "$name" "$binary" "$baseline_database_url"
  BASELINE_EXTRA_ENV="$given"
  baseline_read_extra_env
  tree_head_pid="$baseline_boot_pid"
  tree_head_url="$baseline_boot_url"
  tree_head_log="$baseline_log"
  tree_head_commit="$(curl -sS "$tree_head_url/healthz" 2>"$work/curl" | jq -r '.commit // ""')" ||
    fail boot "read the commit $name serves" "GET $tree_head_url/healthz: $(<"$work/curl")"
  [[ "$tree_head_commit" =~ ^[0-9a-f]{40}$ ]] ||
    fail boot "$name serves a stamped commit" \
      "its /healthz reports commit '$tree_head_commit': build it with -ldflags \"-X main.buildCommit=<commit>\""
  printf 'head server %s %s %s commit %s\n' "$name" "$mode" "$tree_head_url" "$tree_head_commit"
}

# tree_stop_head: stops the server tree_boot_head booted, as lib-baseline.sh stops its own, and is
# safe to call twice and when no server was booted. A process that stops deletes its own
# server_processes row.
tree_stop_head() {
  [[ -z "$tree_head_pid" ]] || baseline_stop_pid "$tree_head_pid"
  tree_head_pid=""
  tree_head_url=""
}

# tree_wait_backfill SECTION [LOG]: waits for the backfill, as the Verification preamble states it.
# Every 2 s it reads, with the database's clock_timestamp(), the count of document_block_versions
# owed a block index by either mark (version null, or this build's mark store_owed_at set), the
# owed counts of comment_block_state, message_block_state and ask_block_state (rows whose owed_at is
# not null) and the count of issues that hold no head mark (issue_head_marks, which the boot's
# backfill and the block index's tick fill), prints them on one line, and reads LOG (by default the
# log of the server tree_boot_head booted) for tree_backfill_end. It passes once all five read 0 and
# that line is in the log, and fails after 300 s naming each count that stayed above zero and the
# line, where the log holds none. It never waits on a fixed sleep. Inputs: `database_url`, and
# tree_head_log or LOG.
tree_wait_backfill() {
  local section="$1" log="${2:-$tree_head_log}" deadline=$((SECONDS + tree_wait_seconds))
  local at unindexed comments messages asks unmarked ended stayed
  [[ -n "$log" ]] || tree_refuse "tree_wait_backfill reads a server's log: boot it with tree_boot_head or name the log"
  while true; do
    query "$section" "read what the backfill still owes" "
      select clock_timestamp(),
             (select count(*) from document_block_versions where version is null or store_owed_at is not null),
             (select count(*) from comment_block_state where owed_at is not null),
             (select count(*) from message_block_state where owed_at is not null),
             (select count(*) from ask_block_state where owed_at is not null),
             (select count(*) from issues i where not exists (select 1 from issue_head_marks m where m.issue_key = i.key))"
    IFS='|' read -r at unindexed comments messages asks unmarked <<<"$answer"
    ended=no
    if grep -qF -- "$tree_backfill_end" "$log"; then
      ended=yes
    fi
    printf 'backfill at %s: document_block_versions_owed=%s comment_block_state_owed=%s message_block_state_owed=%s ask_block_state_owed=%s issues_unmarked=%s boot_backfill_ended=%s\n' \
      "$at" "$unindexed" "$comments" "$messages" "$asks" "$unmarked" "$ended"
    if [[ "$unindexed" == 0 && "$comments" == 0 && "$messages" == 0 && "$asks" == 0 && "$unmarked" == 0 && "$ended" == yes ]]; then
      pass "$section" "the backfill owes nothing, every issue holds its head mark, and the backfill has logged its end"
      return 0
    fi
    if ((SECONDS >= deadline)); then
      stayed=""
      [[ "$unindexed" == 0 ]] || stayed+=" document_block_versions_owed=$unindexed"
      [[ "$comments" == 0 ]] || stayed+=" comment_block_state_owed=$comments"
      [[ "$messages" == 0 ]] || stayed+=" message_block_state_owed=$messages"
      [[ "$asks" == 0 ]] || stayed+=" ask_block_state_owed=$asks"
      [[ "$unmarked" == 0 ]] || stayed+=" issues_unmarked=$unmarked"
      [[ "$ended" == yes ]] || stayed+=" and $log holds no line '$tree_backfill_end'"
      fail "$section" "the backfill ends within ${tree_wait_seconds} s" "after ${tree_wait_seconds} s:${stayed}"
    fi
    sleep "$tree_poll_seconds"
  done
}

# tree_wait_announcement SECTION [COMMIT]: waits until the newest server_build_announcements row
# names COMMIT, by default the commit tree_boot_head read off this head's server: this head writes
# that row once every live server process serves its commit, and the conversion refuses before it.
# It polls every 2 s, prints each reading with the database's clock_timestamp(), and fails after
# 300 s naming the commit the newest row named. Inputs: `database_url`, and tree_head_commit or
# COMMIT.
tree_wait_announcement() {
  local section="$1" commit="${2:-$tree_head_commit}" deadline=$((SECONDS + tree_wait_seconds))
  local at newest
  [[ "$commit" =~ ^[0-9a-f]{40}$ ]] ||
    tree_refuse "tree_wait_announcement waits for a full commit, not '$commit': boot this head with tree_boot_head or name it"
  while true; do
    query "$section" "read the newest build announcement" "
      select clock_timestamp(),
             coalesce((select commit from server_build_announcements order by seq desc limit 1), '(none)')"
    IFS='|' read -r at newest <<<"$answer"
    printf 'announcement at %s: newest names %s, want %s\n' "$at" "$newest" "$commit"
    if [[ "$newest" == "$commit" ]]; then
      pass "$section" "the newest build announcement names $commit"
      return 0
    fi
    ((SECONDS < deadline)) ||
      fail "$section" "the newest build announcement names $commit within ${tree_wait_seconds} s" \
        "after ${tree_wait_seconds} s the newest names $newest"
    sleep "$tree_poll_seconds"
  done
}

# --- the database an upgrade driver empties ---------------------------------------------------------
#
# Each upgrade driver drops the database database_url names and creates it empty, so the earlier
# release's own Store.Migrate writes its schema and the run's rows are the database's only content,
# connecting to the `postgres` database of the same server with the same role. The drivers differ
# only in the database names each takes, as the head comment of each states them.

# tree_database_refuse WORDS…: a database this file will not drop. It exits 2, as tree_refuse
# does, but names the caller's driver, the script its user ran and whose DATABASE_URL it refuses.
tree_database_refuse() {
  printf '%s: %s\n' "${driver:-lib-tree.sh}" "$*" >&2
  exit 2
}

# tree_database_parts URL NAMES: URL split into the server it names (database_server, its URL with
# the `postgres` database in place of its own), its database's name (database_name), and what a
# database beside it is named from: the URL before its path (database_prefix) and its query
# (database_query). It refuses (tree_database_refuse), with status 2 and before any psql call, a
# URL that names no single loopback postgres database with no connection override that libpq would
# connect by instead (lib.sh's loopback_database_url), and a database whose name is not lowercase
# letters, digits and _ matching NAMES, a glob pattern: the database the caller's head comment
# names, never infra_1253, which `bun run dispatch:local` serves. A driver that refuses before it
# builds anything calls it first; tree_reset_database calls it again.
# shellcheck disable=SC2034 # database_query and database_prefix are read by the caller
tree_database_parts() {
  local url="$1" names="$2"
  loopback_database_url "$url" ||
    tree_database_refuse "DATABASE_URL names no single loopback postgres database: this driver drops and creates it"
  # Read after loopback_database_url, whose own match replaces BASH_REMATCH.
  [[ "$url" =~ ^(postgres(ql)?://)([^/?]*)/([^/?]+)(\?.*)?$ ]] ||
    tree_database_refuse "DATABASE_URL is no postgres:// URL naming a database"
  database_server="${BASH_REMATCH[1]}${BASH_REMATCH[3]}/postgres${BASH_REMATCH[5]}"
  database_name="${BASH_REMATCH[4]}"
  database_query="${BASH_REMATCH[5]}"
  database_prefix="${BASH_REMATCH[1]}${BASH_REMATCH[3]}"
  # shellcheck disable=SC2053 # NAMES is a glob pattern, matched as one
  [[ "$database_name" =~ ^[a-z0-9_]+$ && "$database_name" == $names ]] ||
    tree_database_refuse "DATABASE_URL names the database $database_name: this driver drops and creates only a database whose name, of lowercase letters, digits and _, matches $names"
}

# tree_reset_database URL NAMES SECTION: drops the database URL names and creates it empty, once
# tree_database_parts URL NAMES has taken it, whose parts it leaves; SECTION's PASS line says so,
# and its FAIL line where psql fails.
tree_reset_database() {
  tree_database_parts "$1" "$2"
  baseline_psql "$database_server" -q \
    -c "drop database if exists \"$database_name\" with (force)" -c "create database \"$database_name\"" >"$work/reset" 2>&1 ||
    fail "$3" "empty the database $database_name" "psql: $(<"$work/reset")"
  pass "$3" "dropped the database $database_name and created it empty"
}

# --- the values a driver's SQL names, and what the conversion left -----------------------------------

# tree_key SECTION WHAT VALUE and tree_uuid SECTION WHAT VALUE: VALUE is an issue key, or a uuid
# as Dispatch writes one, which the caller's SQL names.
tree_key() {
  [[ "$3" =~ ^[A-Z][A-Z0-9]*-[0-9]+$ ]] || fail "$1" "$2" "'$3' is no issue key"
}
tree_uuid() {
  [[ "$3" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$ ]] || fail "$1" "$2" "'$3' is no uuid"
}

# tree_outcome KEY: what the last conversion's output in $work/out printed for the issue KEY, in
# outcome: `converted`, `passed over`, `deferred` or `none`, read off the line that names the key as
# a word.
tree_outcome() {
  local pattern="(^|[^A-Za-z0-9-])${1}([^0-9]|\$)" line
  outcome=none
  while IFS= read -r line; do
    [[ "$line" =~ $pattern ]] || continue
    case "$line" in
      *deferred*) outcome=deferred ;;
      *"passed over"*) outcome="passed over" ;;
      *converted*) outcome=converted ;;
    esac
  done <"$work/out"
}

# tree_converted SECTION ISSUE: whether ISSUE converted, the converted member of GET
# /api/v1/issues/{key}/tree, in converted, and its tree artifact in tree_value.
# shellcheck disable=SC2034 # converted is this function's answer, read by its caller
tree_converted() {
  tree_read agent "$2"
  tree_want 200 "$1" "read $2's tree"
  converted="$(jq -r '.converted' "$work/body")"
  tree_value="$(jq -r '.tree // ""' "$work/body")"
}

# tree_first_part_of SECTION TREE REF AFTER DOCUMENT NAME: the seq of the first part_of tuple the
# conversion wrote, after the seq AFTER, for the nodes the room TREE holds of DOCUMENT (its
# issue_room_blocks rows, each node keyed by the tree's address REF), in first_seq; `none` where it
# wrote none.
# shellcheck disable=SC2034 # first_seq is this function's answer, read by its caller
tree_first_part_of() {
  query "$1" "read the first part_of the conversion wrote for the nodes of $6" "
    select coalesce(min(t.seq)::text, 'none') from graph_tuples t
    join issue_room_blocks r on r.tree_artifact_id = '$2' and r.document_id = '$5'
      and t.from_kind = 'block' and t.from_id = '$3' || '#' || r.block_id
    where t.kind = 'part_of' and t.asserted and t.seq > $4"
  first_seq="$answer"
}

# tree_store_census SECTION: `envoy-dispatch census --store` over $database_url, its output in
# $work/out and kept in $work/census.out. It exits 1 where a kind of another child holds no row, so
# an exit above 1 alone fails, and the caller reads its lines, not that verdict.
tree_store_census() {
  tree_envoy_dispatch_status census --store
  cp "$work/out" "$work/census.out"
  [[ "$tree_envoy_status" -le 1 ]] ||
    fail "$1" "run the store census" "envoy-dispatch census --store exited $tree_envoy_status: $(tail -n 20 "$work/census.out")"
}

# --- whole reads and kept answers ---------------------------------------------------------------------

# tree_read_all SECTION FILE ISSUE [VIEW]: the agent's tree read of ISSUE (tree_read), every page
# of it followed through its cursor, as one answer in FILE: the first page's members and every
# page's nodes and tuples. It leaves the count of pages it read in tree_pages.
tree_read_all() {
  local section="$1" file="$2" issue="$3" view="${4:-}" cursor=""
  tree_pages=0
  : >"$work/pages"
  while true; do
    tree_read agent "$issue" "$view" "" "$cursor"
    tree_want 200 "$section" "read the tree of $issue${view:+ (?view=$view)}"
    cat "$work/body" >>"$work/pages"
    tree_pages=$((tree_pages + 1))
    cursor="$(jq -r '.cursor // ""' "$work/body")"
    [[ -n "$cursor" ]] || break
    ((tree_pages < 500)) || fail "$section" "read the tree of $issue" "its cursor did not end after $tree_pages pages"
  done
  jq -s '.[0] + {nodes: [.[].nodes[]?], tuples: [.[].tuples[]?]}' "$work/pages" >"$file" ||
    fail "$section" "read the tree of $issue" "the pages do not join: $(head -c 600 "$work/pages")"
}

# tree_section NAME: starts the section NAME, which the caller's closing line counts in `sections`.
tree_section() {
  sections=$((sections + 1))
  printf -- '--- section %s\n' "$1"
}

# tree_keep SECTION PATH FILE: GET PATH as the agent, answered 200, kept in FILE.
tree_keep() {
  tree_send GET "$2"
  tree_want 200 "$1" "read $2"
  cp "$work/body" "$3"
}

# tree_kept_expect FILE SECTION WHAT WANT JQ-ARGS…: lib.sh's expect over the answer kept in FILE
# rather than over the last one, which it puts back as $work/body first.
tree_kept_expect() {
  cp "$1" "$work/body"
  shift
  expect "$@"
}

# tree_person_edit SECTION WHAT DOCUMENT OPS: the person applies the edit ops OPS (a JSON list) to
# DOCUMENT, as seed-corpus.sh's corpus_edit applies them as the agent; an edit that changed nothing
# fails.
tree_person_edit() {
  tree_person_send POST "/api/v1/artifacts/$3/edits" "$(jq -cn --argjson ops "$4" '{ops: $ops}')"
  tree_want 200 "$1" "$2"
  jq -e '.changed == true' "$work/body" >/dev/null || fail "$1" "$2" "the edit changed nothing: $(body)"
}

# --- one node of the store and its move -----------------------------------------------------------

# tree_node_store ADDRESS: the store's kind and id of the node at ADDRESS, in tree_node_kind and
# tree_node_id: a block's address (a document's block or a lens's finding) holds a `#` and is the
# block node's id; every other address is `<kind>/<id>`.
tree_node_store() {
  if [[ "$1" == *"#"* ]]; then
    tree_node_kind=block tree_node_id="$1"
  else
    tree_node_kind="${1%%/*}" tree_node_id="${1#*/}"
  fi
}

# tree_node_far SECTION ADDRESS TUPLE: the far ends of the current TUPLE tuples out of the node at
# ADDRESS - a block's id, every other node `<kind>/<id>` - joined by commas, in answer. A tuple
# names a lens's finding, a block of the store, as lens_block (graphstore.AddressKind), so that far
# end is read as a block's id too.
tree_node_far() {
  tree_node_store "$2"
  with_vars "$1" "read the $3 of $2" \
    "select coalesce(string_agg(case when h.to_kind in ('block', 'lens_block') then h.to_id else h.to_kind || '/' || h.to_id end, ',' order by h.to_kind, h.to_id), '')
     from (select distinct on (to_kind, to_id) to_kind, to_id, asserted from graph_tuples
           where from_kind = :'kind' and from_id = :'id' and kind = :'tuple' and to_kind <> 'value'
           order by to_kind, to_id, seq desc) h where h.asserted" \
    kind="$tree_node_kind" id="$tree_node_id" tuple="$3"
}

# tree_node_value SECTION ADDRESS ATTRIBUTE: the current value of ATTRIBUTE of the node at ADDRESS,
# empty where it holds none, in answer.
tree_node_value() {
  tree_node_store "$2"
  with_vars "$1" "read the $3 of $2" \
    "select coalesce((select case when asserted then to_id end from graph_tuples
       where from_kind = :'kind' and from_id = :'id' and kind = :'attribute' and to_kind = 'value'
       order by seq desc limit 1), '')" \
    kind="$tree_node_kind" id="$tree_node_id" attribute="$3"
}

# tree_node_tag SECTION ADDRESS [any]: the placement_seq of the node at ADDRESS, the seq of its
# current part_of (as the tree read answers it), in answer; with `any`, 0 for a node that holds none
# (a comment of the issue), which a refusal that comes before the If-Match test is sent with.
tree_node_tag() {
  tree_node_store "$2"
  with_vars "$1" "read the placement_seq of $2" \
    "select coalesce((select h.seq from (select distinct on (to_kind, to_id) seq, asserted from graph_tuples
       where from_kind = :'kind' and from_id = :'id' and kind = 'part_of' and to_kind <> 'value'
       order by to_kind, to_id, seq desc) h where h.asserted order by h.seq desc limit 1)::text, '')" \
    kind="$tree_node_kind" id="$tree_node_id"
  if [[ -z "$answer" ]]; then
    [[ "${3:-}" == any ]] || fail "$1" "read the placement_seq of $2" "it holds no part_of"
    answer=0
  fi
}

# tree_node_send METHOD AS KEY NODE TAG BODY QUERY: METHOD /api/v1/issues/KEY/nodes/NODE as AS
# (agent or person), NODE percent-encoded as one segment, with the JSON BODY and the query string
# QUERY (dry_run=true, expected_count=<n>) where each is not empty, and `If-Match: "TAG"` where TAG
# is not empty. The answer is in $work/body and `status`, as tree_request leaves them.
tree_node_send() {
  local method="$1" as="$2" key="$3" node="$4" tag="$5" body="$6" query="$7"
  local -a auth match=() data=()
  case "$as" in
    agent) auth=(-H "@$work/auth") ;;
    person) auth=(-b "$work/jar" -H "Origin: $(base_origin)") ;;
    *) tree_refuse "tree_node_send sends as agent or person, not '$as'" ;;
  esac
  [[ -z "$tag" ]] || match=(-H "If-Match: \"$tag\"")
  [[ -z "$body" ]] || data=(-H 'Content-Type: application/json' --data-binary "$body")
  tree_last_request="$method /api/v1/issues/$key/nodes/$node${query:+?$query}${tag:+ If-Match \"$tag\"}${body:+ $body}"
  status="$(curl -sS -X "$method" -o "$work/body" -w '%{http_code}' "${auth[@]}" "${match[@]}" "${data[@]}" \
    "$base/api/v1/issues/$(q "$key")/nodes/$(q "$node")${query:+?$query}" 2>"$work/curl")" ||
    status="curl failed: $(<"$work/curl")"
}

# tree_block_node SECTION DOCUMENT BLOCK: the store address of block BLOCK of the document whose
# artifact id is DOCUMENT, as its document_blocks row names it, read in the room the document is
# edited in (its document_rooms row's room, else the document itself), in tree_block_node_id; empty
# where no row holds the block.
# shellcheck disable=SC2034 # tree_block_node_id is this function's answer, read by its caller
tree_block_node() {
  with_vars "$1" "read the node of $3 of $2" \
    "select coalesce((select b.node_id from document_blocks b
       where b.artifact_id = coalesce((select r.room from document_rooms r where r.artifact_id = :'doc'::uuid), :'doc'::uuid)
         and b.block_id = :'block'), '')" \
    doc="$2" block="$3"
  tree_block_node_id="$answer"
}

# --- the document socket peer ---------------------------------------------------------------------
#
# What c-one-room and c-room-cutover hold document sockets through: the Bun program
# e2e/acceptance/issue-room-peer.ts, whose head comment names the commands it answers, run as a
# coprocess. The functions below leave the caller peer_pid, peer_in and peer_out, the coprocess and
# the two descriptors the driver holds, each of which the caller sets empty before its cleanup can
# call tree_peer_stop; reply, the peer's last line; and fragments, what tree_peer_open read.

# This package's directory, packages/dispatch, read once as this file is sourced: the socket peer
# is its file e2e/acceptance/issue-room-peer.ts, which imports yjs and lib0 from its node_modules.
tree_dispatch_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly tree_dispatch_dir

# tree_peer_require: refuses, exit 2, a checkout without this package's node_modules, which the
# socket peer imports yjs and lib0 from. A driver calls it with its other input checks.
tree_peer_require() {
  [[ -d "$tree_dispatch_dir/node_modules/yjs" && -d "$tree_dispatch_dir/node_modules/lib0" ]] || {
    printf 'the socket peer imports yjs and lib0 from %s/node_modules: run bun install first\n' "$tree_dispatch_dir" >&2
    exit 2
  }
}

# tree_peer_cookie: the person's session cookie, out of the jar tree_sign_in wrote, as the Cookie
# header's value, in $work/cookie, which the socket peer reads.
tree_peer_cookie() {
  local domain flag path secure expiry name value
  : >"$work/cookie"
  while IFS=$'\t' read -r domain flag path secure expiry name value; do
    [[ "$name" == dsession ]] || continue
    printf 'dsession=%s' "$value" >"$work/cookie"
  done <"$work/jar"
  [[ -s "$work/cookie" ]] || fail setup "read the person's session cookie" "the jar holds no dsession cookie"
}

# tree_peer_start SCHEMA: starts the socket peer against $base as the person, on the block schema
# version SCHEMA, as a coprocess whose stdin and stdout the driver holds. The peer waits
# tree_peer_answer_seconds for a socket's answer.
# shellcheck disable=SC2034 # peer_pid is this function's answer, read by its caller
tree_peer_start() {
  tree_peer_cookie
  coproc TREE_PEER {
    PEER_ANSWER_MS=$((tree_peer_answer_seconds * 1000)) exec bun "$tree_dispatch_dir/e2e/acceptance/issue-room-peer.ts" \
      "$base" "$work/cookie" "$1" 2>>"$work/peer.err"
  }
  peer_pid="$TREE_PEER_PID"
  exec {peer_in}>&"${TREE_PEER[1]}" {peer_out}<&"${TREE_PEER[0]}"
}

# tree_peer_stop: stops the socket peer through its `quit`, which closes every socket it holds, and
# closes the two descriptors; safe when none runs.
tree_peer_stop() {
  [[ -n "$peer_pid" ]] || return 0
  printf 'quit\n' >&"$peer_in" 2>/dev/null || true
  exec {peer_in}>&- {peer_out}<&-
  wait "$peer_pid" 2>/dev/null || true
  peer_pid=""
  peer_in=""
  peer_out=""
}

# tree_peer SECTION WHAT COMMAND…: sends COMMAND to the peer and reads its one line into reply; a
# peer that answers nothing within its bound, or answers `error`, fails naming WHAT.
tree_peer() {
  local section="$1" what="$2"
  shift 2
  printf '%s\n' "$*" >&"$peer_in" ||
    fail "$section" "$what" "the socket peer stopped: $(tail -n 20 "$work/peer.err" 2>/dev/null)"
  IFS= read -r -t $((tree_peer_answer_seconds + 30)) reply <&"$peer_out" ||
    fail "$section" "$what" "the socket peer answered nothing to '$1': $(tail -n 20 "$work/peer.err" 2>/dev/null)"
  [[ "$reply" != error* ]] || fail "$section" "$what" "the socket peer answered: $reply"
  printf 'peer: %s -> %s\n' "$1 ${2:-}" "$reply"
}

# tree_peer_open SECTION NAME DOCUMENT: opens the socket NAME on DOCUMENT, which must be admitted
# writable; its fragments in fragments.
# shellcheck disable=SC2034 # fragments is this function's answer, read by its caller
tree_peer_open() {
  tree_peer "$1" "open a document socket on $3" open "$2" "$3"
  [[ "$reply" =~ ^opened\ $2\ read-write\ (.*)$ ]] ||
    fail "$1" "a document socket on $3 is admitted writable" "the peer answered: $reply"
  fragments="${BASH_REMATCH[1]}"
}

# tree_peer_append SECTION NAME FRAGMENT JSON: NAME appends the block JSON describes to FRAGMENT,
# and the room applies it.
tree_peer_append() {
  tree_peer "$1" "write a block through the socket $2" append "$2" "$3" "$4"
  [[ "$reply" == "appended $2 1" ]] ||
    fail "$1" "the room applies the block the socket $2 writes into the fragment $3" "the peer answered: $reply"
}

# tree_peer_holds SECTION NAME FRAGMENT ID: whether the socket NAME's document holds the block ID
# in FRAGMENT.
tree_peer_holds() {
  tree_peer "$1" "read the blocks the socket $2 draws" blocks "$2" "$3"
  jq -e --arg id "$4" 'any(.[]; .id == $id)' <<<"${reply#blocks "$2" }" >/dev/null
}
