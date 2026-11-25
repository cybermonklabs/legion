# shellcheck shell=bash
# What the acceptance drivers of dispatch://INFRA-1253 share, so that no driver copies it: the
# request to a server as the agent or as the person, the person's dev sign-in and the project create
# that names that person, the tree read, the two CLIs, and for a driver over rows an earlier release
# wrote the boot of this head over them, the store backfill's run and the wait for the backfill, the
# wait for this head's build announcement and the conversion. Sourced, never run, after lib.sh, and
# for an upgrade driver after lib-baseline.sh too, which this file reads but does not source: a
# driver against `bun run dispatch:local` boots no server and has no use for it.
#
# The checks it serves are every driver check of the Verification document of dispatch://INFRA-1253
# (verification-md), and seed-corpus.sh, which writes the corpus several of them read:
#   against `bun run dispatch:local` on infra_1253: c-reparent-and-state, c-coverage-states,
#     c-issue-relations-read, c-conversation-holds-the-discussion, c-tree-read-views,
#     c-child-placement and c-child-view;
#   over rows the earlier release wrote, on infra_1253_upgrade: c-tree-census, c-one-room,
#     c-room-cutover, c-order, c-conversion-scope, c-shared-id-census, c-redirects, c-tree-page,
#     c-queries, c-artifacts-in-tree, c-old-versions-render, c-one-dependency and c-view-arms.
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
# From lib-baseline.sh, after its baseline_start: baseline_database_url, baseline_boot (with the
# baseline_boot_pid, baseline_boot_url and baseline_log it leaves), baseline_read_extra_env and
# baseline_stop_pid.
#
# What it leaves its caller: `status` and $work/body after each request, which lib.sh's
# want_status, expect and body read; tree_last_request, the request the last answer came from;
# tree_value, the value tree_field read; tree_person, the login tree_sign_in signed in; $work/out,
# the output of the last CLI run, and tree_envoy_status, the exit status of the last
# `envoy-dispatch`; and tree_head_pid, tree_head_url, tree_head_log and tree_head_commit after
# tree_boot_head.
#
# A refused input or call exits 2 on stderr, as lib.sh's require_env does; a failed check prints
# lib.sh's `FAIL: <section> <what>: <observed>` and exits 1. Every check is `cmd || fail …`, never
# part of an `&&` list: under `set -e` a failing command anywhere but last in such a list does not
# stop the script.
: "${work:=}" "${status:=}" "${base:=}" "${database_url:=}" "${token:=}" "${session:=}" "${project:=}"
: "${answer:=}"
: "${baseline_database_url:=}" "${baseline_boot_pid:=}" "${baseline_boot_url:=}" "${baseline_log:=}"

tree_last_request=""
tree_value=""
tree_person=""
tree_head_pid=""
tree_head_url=""
tree_head_log=""
tree_head_commit=""
tree_envoy_status=""

# The settings every upgrade boot of this head takes, as the Verification preamble states: with
# them this head holds the conversion step of R6 at boot and on its tick, so it converts an issue
# only when a driver runs `envoy-dispatch convert-issues`. The one boot without them is the
# automatic case of c-conversion-scope (tree_boot_head's `unheld`). Production sets neither.
readonly tree_hold_env=$'DISPATCH_TEST_HOOKS=1\nDISPATCH_TEST_HOLD_ISSUE_ROOM_STEP=1'

# The line the boot step of plan task T9 logs once its run of graphcensus.Backfill's passes, which
# it runs at every boot, has ended. tree_wait_backfill reads the server's log for it as written
# here, so that step and this constant name one line and are changed together. Until T9 lands the
# store backfill is no boot step but the `envoy-dispatch backfill-graph-store` subcommand
# (cmd/dispatch/subcommands.go), which prints one `backfill-graph-store: kind=… complete=…` line
# for each kind: a driver runs it with tree_run_backfill, which waits for every kind's
# `complete=true`, and no server's log holds this line yet.
readonly tree_backfill_end='msg="dispatch: boot backfill done"'

# How long each wait below polls, and how often: the Verification preamble's 300 s and 2 s.
readonly tree_wait_seconds=300
readonly tree_poll_seconds=2

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

# tree_origin: the scheme and host of $base, which a write the person's cookie authenticates must
# name in Origin (enforceCookieOrigin). It is read at each call, since an upgrade driver moves
# $base from one server to the next.
tree_origin() {
  local host="${base#*://}"
  printf '%s://%s' "${base%%://*}" "${host%%/*}"
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
      auth=(-b "$work/jar" -H "Origin: $(tree_origin)")
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

# tree_run_backfill SECTION: the store backfill, as node-projects.sh runs it, until T9's boot step
# runs it at every boot (tree_backfill_end): `envoy-dispatch backfill-graph-store` over
# $database_url, its output in $work/out. It prints each kind's `backfill-graph-store: kind=…`
# line, and passes when the command exits 0 and every kind it reports reads `complete=true`. It
# fails naming each kind whose line reads otherwise, with that line; and where every kind reads
# complete or none was reported, a non-zero exit fails naming its status and the end of its
# output, which then names the pass that stopped. Inputs: `database_url`, `work`.
tree_run_backfill() {
  local section="$1" line kind kinds=0 incomplete=""
  tree_envoy_dispatch_status backfill-graph-store
  while IFS= read -r line; do
    [[ "$line" == "backfill-graph-store: kind="* ]] || continue
    kinds=$((kinds + 1))
    printf '%s\n' "$line"
    kind="${line#backfill-graph-store: kind=}"
    kind="${kind%% *}"
    [[ "$line" == *" complete=true" ]] || incomplete+="; kind $kind stayed incomplete: $line"
  done <"$work/out"
  [[ -z "$incomplete" ]] ||
    fail "$section" "backfill-graph-store completes every kind" "${incomplete#; }"
  [[ "$tree_envoy_status" == 0 && "$kinds" -gt 0 ]] ||
    fail "$section" "backfill-graph-store completes every kind" \
      "it exited $tree_envoy_status after $kinds kind lines: $(tail -n 20 "$work/out")"
  pass "$section" "backfill-graph-store completed all $kinds kinds"
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
