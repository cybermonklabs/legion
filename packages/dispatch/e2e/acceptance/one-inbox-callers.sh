#!/usr/bin/env bash
# Drives the check c-callers-moved at this branch's head: every caller of the three
# reads the one read replaced moved, the comparison and the opt-in are gone, and a page of the
# release before still works, because the three reads answer this head as they answer that
# release.
#
# The three reads are GET /api/v1/inbox (listInbox), GET /api/v1/me/replies (listMyReplies) and
# GET /api/v1/agents/{session_id}/messages (listAgentMessages). They stay served for one release,
# with their handlers, their rows in api/routes_table.go and their contract response types, and
# the root deletes them a release later. POST /api/v1/agents/{session_id}/messages, the send on
# the same path, stays.
#
#   setup       the dispatch:local this run reads (DISPATCH_URL) answers; the commit BASELINE_SHA
#               names is built in $TMPDIR/base-callers (or BASELINE_BINARY is taken) and booted on
#               port 8780 over ISSUE_DATABASE_URL, the database that dispatch:local serves, with its
#               Envoy at the same fake Envoy listener (FAKE_ENVOY_PORT), so both builds read one set
#               of rows and one listener; the two people sign in; and the rows the later sections
#               read are written, through this head alone.
#   callers     packages/dispatch/web/src, packages/envoy-client/src, packages/pi-envoy/src,
#               packages/claude-envoy/src and packages/dispatch/e2e hold no read of the three
#               paths, a send to /api/v1/agents/{session_id}/messages aside, written in any way a
#               caller writes one: a literal, a template, a route pattern or glob, a regex with
#               escaped slashes, string pieces joined by `+`, or envoy-client's segment list. The
#               e2e suite is read beyond the four trees the check names: its specs wait for, stub
#               and hold the page's requests by their paths, and a wait on a path the page no
#               longer reads fails only in a browser run. A few of its files read a kept route on
#               purpose and are spared, each by its exact path (callers_spared says why), and its
#               recorded HARs by their extension. A scratch tree holding a planted read in each of
#               those forms, and two planted sends, is searched first.
#   gone        no tracked file but frozen history names the comparison (its command
#               `inbox-compare`, its package, its table, its route and page, its types) or the
#               opt-in (its query key `one=1` and its browser preference `inbox.one_read`); a
#               planted instance of each is found first.
#   old_routes  the three kept reads answer every person who has signed in the same body, field for
#               field, from this head as from the base build, the top-level `as_of` aside.
#   isolation   two people each read their own comment, and not the other's, in the one read.
#   default     a comment its person wrote and nobody answered is in that person's one read, read
#               with no query at all, so with no opt-in, and in none of their Reply rows.
#   open_asks   GET /api/v1/asks/open with waiting_on answers the asks that wait on that person.
#   conversation
#               GET /api/v1/agents/{session_id}/conversation answers a person's message to that
#               session, with the session's reply under it, and another session's does not.
#   post_stays  POST /api/v1/agents/{session_id}/messages still takes a message.
#   names       the paths, handlers, response shapes, operations, contract types, client methods,
#               queries and query keys of the open-asks read and the session-conversation read
#               carry no word `inbox`; a scratch copy of the files they are read from, with one such
#               name planted for each kind but the path, which is this script's own, is read first.
#
# Every grep is shown able to fail before it is trusted: each runs first over a scratch tree
# holding a planted instance of each form and alternative it searches for, and must find each,
# before it runs over the tree. The base build is sent reads
# only. While it serves it still runs its own background sweeps over ISSUE_DATABASE_URL, such as
# the maintenance sweep (api.RunMaintenanceSweep), so "reads only" names what this script sends
# it, and not all that build writes. The script stops it before it exits.
#
# Inputs: DISPATCH_URL (the dispatch:local this branch's head serves), DISPATCH_TOKEN or
# DISPATCH_TOKEN_FILE (its agent bearer, read here and never printed), PROJECT (an existing
# project key), ISSUE_DATABASE_URL (the database that server serves, on this machine), FAKE_ENVOY_PORT
# (the port of its fake Envoy listener, which dispatch:local prints), and BASELINE_SHA (the
# release this branch lands after; no default) or BASELINE_BINARY (a server already built from
# it). Optional: FAKE_GITHUB_PORT and FAKE_BROKER_PORT, the fake GitHub and the fake secrets broker
# dispatch:local runs, which the base build is pointed at too (unset, the base build's GitHub is a
# port nothing listens on and it has no broker: no read this script sends it reaches either);
# DISPATCH_E2E_DEV_SIGNIN_SHARED_KEY, as dispatch:local was given it (default e2e-dev-signin), the
# cookie key both builds must share; ONE_INBOX_CALLERS_SECTIONS, a space-separated subset of the
# sections above, of which callers, gone and names need no server and read none of the inputs
# above. Without BASELINE_BINARY the build needs git, go, tar and flock. A missing or bad input is
# exit 2.
#
# Each passing assertion prints `PASS: <section> <what>`; a failing one prints
# `FAIL: <section> <what>: <observed>` and ends its section. Every section runs whatever an earlier
# one found, and the script exits 1 at its end if any failed.
#
# Every check is `cmd || fail …`, never part of an `&&` list: under `set -e` a failing command
# anywhere but last in such a list does not stop the script.

# shellcheck disable=SC2016 # the single-quoted jq filters name jq variables, not shell ones
set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: ISSUE_DATABASE_URL=postgres://user@127.0.0.1:5433/db \
         DISPATCH_URL=http://127.0.0.1:8777 DISPATCH_TOKEN=e2e-token PROJECT=CORE \
         FAKE_ENVOY_PORT=<port> {BASELINE_SHA=<commit> | BASELINE_BINARY=<path>} \
         [FAKE_GITHUB_PORT=<port>] [FAKE_BROKER_PORT=<port>] \
         [ONE_INBOX_CALLERS_SECTIONS='callers gone names'] one-inbox-callers.sh
       (DISPATCH_TOKEN_FILE=<file holding the bearer> in place of DISPATCH_TOKEN; BASELINE_SHA is
       the release this branch lands after, and has no default; callers, gone and names alone
       need none of the server's inputs)
USAGE
}

# What lib.sh's functions set for this script: the bearer (read_bearer) and the work directory
# (work_dir). They are named here so that shellcheck, which reads one file at a time unless it is
# run with -x, sees them assigned.
token="" work=""
# shellcheck source-path=SCRIPTDIR source=lib.sh disable=SC1091 # followed under shellcheck -x
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
repo="$(cd -- "$script_dir/../../../.." && pwd)"
readonly repo
readonly dispatch_dir="$repo/packages/dispatch"
# This file as the tree names it, which the gone section's search leaves out: it names everything
# that search looks for.
readonly self="packages/dispatch/e2e/acceptance/one-inbox-callers.sh"

readonly all_sections="setup callers gone old_routes isolation default open_asks conversation post_stays names"
readonly offline_sections="callers gone names"
sections="${ONE_INBOX_CALLERS_SECTIONS:-$all_sections}"
online=0
for chosen in $sections; do
  [[ " $all_sections " == *" $chosen "* ]] || {
    printf 'one-inbox-callers.sh: ONE_INBOX_CALLERS_SECTIONS names %s, which is none of: %s\n' "$chosen" "$all_sections" >&2
    usage
    exit 2
  }
  [[ " $offline_sections " == *" $chosen "* ]] || online=1
done
# A section that reads the server reads what setup wrote, so setup runs whenever one does.
if [[ "$online" == 1 && " $sections " != *" setup "* ]]; then
  sections="setup $sections"
fi
readonly sections online

require_commands awk cmp cp git grep jq sed sort tr wc
if [[ "$online" == 1 ]]; then
  require_env DISPATCH_URL PROJECT ISSUE_DATABASE_URL FAKE_ENVOY_PORT
  [[ -n "${BASELINE_SHA:-}" || -n "${BASELINE_BINARY:-}" ]] || {
    printf 'one-inbox-callers.sh: BASELINE_SHA is unset: set it to the commit of the release this branch lands after, or BASELINE_BINARY to a server built from it. It has no default\n' >&2
    usage
    exit 2
  }
  require_commands bash curl env kill openssl sleep
  [[ -n "${BASELINE_BINARY:-}" ]] || require_commands flock go tar
  for variable in FAKE_ENVOY_PORT FAKE_GITHUB_PORT FAKE_BROKER_PORT; do
    [[ -z "${!variable:-}" || "${!variable}" =~ ^[0-9]+$ ]] || {
      printf 'one-inbox-callers.sh: %s is %s, not a port number\n' "$variable" "${!variable}" >&2
      usage
      exit 2
    }
  done
  loopback_database_url "$ISSUE_DATABASE_URL" || {
    printf 'one-inbox-callers.sh: ISSUE_DATABASE_URL names no single loopback host; the base build this script boots serves it, and only a test database on this machine is one\n' >&2
    exit 2
  }
  read_bearer
fi

# shellcheck disable=SC2031 # the DISPATCH_URL lib.sh's `cli` exports is its own; this one is the caller's
base="${DISPATCH_URL:-}"
readonly base="${base%/}"
base_host="${base#*://}"
# The dashboard's origin, which a write the person's cookie authenticates must name
# (enforceCookieOrigin).
readonly origin="${base%%://*}://${base_host%%/*}"
readonly old_port=8780
readonly old="http://127.0.0.1:$old_port"
# shellcheck disable=SC2153 # these are the caller's inputs, checked by name above
readonly project="${PROJECT:-}"
readonly database_url="${ISSUE_DATABASE_URL:-}"
stamp="$(date +%s)-$$"
readonly stamp
readonly session="one-inbox-callers-$stamp"
readonly actor="{\"kind\": \"session\", \"id\": \"$session\"}"
# The two people the isolation, default, open_asks and conversation sections read as: the two
# dispatch:local signs in.
readonly first=alice second=bob
# shellcheck disable=SC2031 # lib.sh's `cli` exports its own TMPDIR in a subshell; this is the run's
readonly base_dir="${TMPDIR:-/tmp}/base-callers"

work_dir
printf 'Authorization: Bearer %s\n' "$token" >"$work/auth"

# The base server's process, which setup records here once it starts it, and the issues setup
# writes, which it records one per line: the cleanup stops the one and closes the others.
cleanup() {
  local code=$? pid key
  if [[ -s "$work/base.pid" ]]; then
    pid="$(<"$work/base.pid")"
    if kill -TERM "$pid" 2>/dev/null; then
      for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 22 23 24 25; do
        kill -0 "$pid" 2>/dev/null || break
        sleep 0.2
      done
      kill -KILL "$pid" 2>/dev/null || true
    fi
    printf 'one-inbox-callers.sh: stopped the base build (pid %s)\n' "$pid"
  fi
  if [[ -s "$work/issues" ]]; then
    while IFS= read -r key; do
      request agent head PATCH "/api/v1/issues/$key" "$(jq -cn --argjson a "$actor" '{status: "done", actor: $a}')"
      [[ "$status" == 200 ]] ||
        printf 'one-inbox-callers.sh: could not close %s: HTTP %s: %s\n' "$key" "$status" "$(body)" >&2
    done <"$work/issues"
  fi
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# request AS SERVER METHOD PATH [JSON]: sends METHOD PATH to SERVER (`head`, the dispatch:local
# this run reads, or `old`, the base build) as AS: `agent`, with the bearer, or a person's login,
# with the cookie dev sign-in gave that person (sign_in) and, for a write, the dashboard's origin.
# Both builds sign cookies with one key and read one database, and a cookie names no port, so the
# one cookie reaches both. The answer's body goes to $work/body and its HTTP status to $status.
request() {
  local who="$1" server="$2" method="$3" path="$4" url
  local -a auth data=()
  if [[ "$who" == agent ]]; then
    auth=(-H "@$work/auth")
  else
    auth=(-b "$work/jar.$who" -H "Origin: $origin")
  fi
  url="$base"
  [[ "$server" == head ]] || url="$old"
  [[ $# -lt 5 ]] || data=(-H 'Content-Type: application/json' --data-binary "$5")
  status="$(curl -sS -X "$method" -o "$work/body" -w '%{http_code}' "${auth[@]}" "${data[@]}" \
    "$url$path" 2>"$work/curl")" || status="curl failed: $(<"$work/curl")"
}

# sign_in SECTION LOGIN: has the head's dev sign-in route sign LOGIN in, keeping the cookie in
# $work/jar.LOGIN. The route answers its sign-in with a redirect, and curl writes a jar's header
# lines whatever the answer, so the jar holds a cookie only where it holds a cookie's line, whose
# fields a tab parts.
sign_in() {
  local code
  code="$(curl -sS -o /dev/null -w '%{http_code}' -c "$work/jar.$2" "$base/auth/_dev/signin?login=$(q "$2")" 2>"$work/curl")" ||
    fail "$1" "sign $2 in" "curl: $(<"$work/curl")"
  [[ "$code" == 302 ]] || fail "$1" "sign $2 in" "the dev sign-in route answered HTTP $code, want 302"
  grep -q $'\t' "$work/jar.$2" || fail "$1" "sign $2 in" "the dev sign-in route left no cookie"
  pass "$1" "signed $2 in"
}

# post SECTION WHAT AS PATH JSON: writes JSON at PATH on the head as AS and sets $id to the id the
# answer names.
post() {
  request "$3" head POST "$4" "$5"
  want_status 201 "$1" "$2"
  id="$(jq -er '.id' "$work/body")" || fail "$1" "$2" "no id: $(body)"
  pass "$1" "$2 ($id)"
}
# as_session JSON: JSON with this run's session as its actor, as a bearer caller names it.
as_session() { jq -cn --argjson f "$1" --argjson a "$actor" '$f + {actor: $a}'; }

# seeded SECTION NAME: what setup recorded under NAME, or a failure of SECTION naming it.
seeded() {
  [[ -s "$work/seed.$2" ]] || fail "$1" "read what setup wrote" "setup recorded no $2: it failed before it wrote it"
  printf '%s' "$(<"$work/seed.$2")"
}

# isolated COMMAND...: runs COMMAND in a subshell of its own, with errexit, and sets
# $isolated_status to its exit status, so a `fail` inside ends COMMAND alone. errexit is turned off
# around it, not left to the caller's `||`: under an `||` bash ignores errexit in the whole
# subshell, and an unguarded failing command inside would run on.
isolated() {
  set +e
  (
    set -e
    "$@"
  )
  isolated_status=$?
  set -e
}

# grep_tree ROOT GIT-GREP-ARGS...: `git grep -nIE` over ROOT: its tracked files and the untracked
# ones git does not ignore where ROOT is a checkout, and every file under it where it is a scratch
# directory (`--no-index`). Its hits go to stdout, one `path:line:text` each; no hit is no failure.
grep_tree() {
  local root="$1" code=0
  shift
  if git -C "$root" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    git -C "$root" grep --untracked -nIE "$@" || code=$?
  else
    (cd -- "$root" && git grep --no-index -nIE "$@") || code=$?
  fi
  [[ "$code" -le 1 ]]
}

# --- setup: the base build, the people and the rows the sections read ---------------------------

# setup_base: builds the base (once per commit, under a lock, in $base_dir) unless BASELINE_BINARY
# names it, and boots it on $old_port over the head's database through e2e/run-server.sh, the
# script dispatch:local boots the head through, so it gets the head's settings and listeners.
# $base_dir is made first whichever build is booted: it is the server's TMPDIR and holds its log.
setup_base() {
  local section=setup binary commit="" waited=0 pid
  mkdir -p "$base_dir" || fail "$section" "make $base_dir" "mkdir failed"
  if [[ -n "${BASELINE_BINARY:-}" ]]; then
    binary="$BASELINE_BINARY"
    [[ "$binary" == /* ]] || binary="$PWD/$binary"
    [[ -f "$binary" && -x "$binary" ]] || fail "$section" "take the base build BASELINE_BINARY names" "$binary is no executable file"
    pass "$section" "the base build is BASELINE_BINARY, $binary"
  else
    commit="$(git -C "$repo" rev-parse --verify --quiet "${BASELINE_SHA}^{commit}")" ||
      fail "$section" "resolve BASELINE_SHA" "'$BASELINE_SHA' names no commit of $repo"
    binary="$base_dir/dispatch-$commit"
    # The tree is taken with git archive, a read of the repository (no worktree, no checkout),
    # and built stamped with its commit as the image build stamps a server.
    flock "$base_dir/build.lock" bash -c '
      set -euo pipefail
      binary="$1" commit="$2" repo="$3" tree="$4"
      [[ -x "$binary" ]] && exit 0
      rm -rf "$tree"
      mkdir -p "$tree"
      git -C "$repo" archive "$commit" | tar -x -C "$tree"
      go -C "$tree/packages/envoy" build -ldflags "-X main.buildCommit=$commit" -o "$binary.partial" ./cmd/dispatch
      mv "$binary.partial" "$binary"
      rm -rf "$tree"
    ' build "$binary" "$commit" "$repo" "$base_dir/tree-$commit" >"$work/build.log" 2>&1 ||
      fail "$section" "build $commit in $base_dir" "$(tail -n 20 "$work/build.log")"
    pass "$section" "the base build of $commit is $binary"
  fi

  if curl -s -o /dev/null "$old/healthz" 2>/dev/null; then
    fail "$section" "boot the base build on port $old_port" "something already answers $old"
  fi
  local -a fakes=(FAKE_ENVOY_PORT="$FAKE_ENVOY_PORT" FAKE_GITHUB_PORT="${FAKE_GITHUB_PORT:-9}")
  if [[ -n "${FAKE_BROKER_PORT:-}" ]]; then
    fakes+=(FAKE_BROKER_PORT="$FAKE_BROKER_PORT")
  else
    fakes+=(DISPATCH_E2E_AGENT_SECRETS_URL=)
  fi
  (
    cd -- "$dispatch_dir"
    # shellcheck disable=SC2031 # lib.sh's `cli` exports its own PATH and HOME; these are this run's
    exec env -i PATH="$PATH" HOME="${HOME:-/nonexistent}" TMPDIR="$base_dir" \
      DATABASE_URL="$database_url" DISPATCH_E2E_PORT="$old_port" DISPATCH_E2E_SERVER_BIN="$binary" \
      DISPATCH_E2E_DEV_SIGNIN_SHARED_KEY="${DISPATCH_E2E_DEV_SIGNIN_SHARED_KEY:-e2e-dev-signin}" \
      "${fakes[@]}" bash e2e/run-server.sh
  ) </dev/null >"$base_dir/server-$stamp.log" 2>&1 &
  pid=$!
  printf '%s' "$pid" >"$work/base.pid"
  while ! curl -fsS -o /dev/null "$old/healthz" 2>/dev/null; do
    kill -0 "$pid" 2>/dev/null ||
      fail "$section" "boot the base build on port $old_port" "it exited: $(tail -n 20 "$base_dir/server-$stamp.log")"
    [[ "$waited" -lt 360 ]] ||
      fail "$section" "boot the base build on port $old_port" "no /healthz within 180 s: $(tail -n 20 "$base_dir/server-$stamp.log")"
    sleep 0.5
    waited=$((waited + 1))
  done
  : >"$work/base.up"
  pass "$section" "the base build serves $old over ISSUE_DATABASE_URL, its Envoy the fake listener on port $FAKE_ENVOY_PORT (log $base_dir/server-$stamp.log)"
}

# create_issue NAME TITLE ASSIGNEE: creates an issue in PROJECT assigned to ASSIGNEE as this run's
# session, recorded for the cleanup, and records its key under NAME. `force` takes it past the
# duplicate check a second run's title would meet.
create_issue() {
  local key
  request agent head POST /api/v1/issues "$(jq -cn --arg p "$project" --arg t "$2" --arg a "$3" --argjson s "$actor" \
    '{project: $p, title: $t, assignee: $a, force: true, actor: $s}')"
  want_status 201 setup "create \"$2\" assigned to $3"
  key="$(jq -er '.key' "$work/body")" || fail setup "create \"$2\"" "no key: $(body)"
  printf '%s\n' "$key" >>"$work/issues"
  printf '%s' "$key" >"$work/seed.$1"
  pass setup "created $key in $project, assigned to $3"
}

setup_seed() {
  local section=setup
  request agent head GET /healthz
  want_status 200 "$section" "reach the head at $base"
  pass "$section" "the head answers at $base"
  sign_in "$section" "$first"
  sign_in "$section" "$second"

  # One issue each person holds, a comment each person writes on their own issue and nobody
  # answers, and an ask on each issue, which waits on that issue's assignee.
  create_issue first_issue "What $first holds, $stamp" "$first"
  create_issue second_issue "What $second holds, $stamp" "$second"
  local who issue
  for who in first second; do
    issue="$(<"$work/seed.${who}_issue")"
    post "$section" "${!who} comments on $issue" "${!who}" "/api/v1/issues/$issue/comments" \
      "$(jq -cn --arg b "A note ${!who} wrote, $stamp." '{body: $b}')"
    printf '%s' "$id" >"$work/seed.${who}_comment"
    post "$section" "an ask on $issue" agent "/api/v1/issues/$issue/asks" \
      "$(as_session "$(jq -cn --arg q "Which way, ${!who}? ($stamp)" '{question: $q}')")"
    printf '%s' "$id" >"$work/seed.${who}_ask"
  done

  # Two sessions the head lists: the one the conversation and post_stays sections write to, and
  # another whose conversation must not hold what was written to the first.
  request agent head GET /api/v1/agents
  want_status 200 "$section" "list the agents"
  jq -r '(if type == "array" then . else .agents end)[].session_id' "$work/body" >"$work/seed.agents" ||
    fail "$section" "list the agents" "$(body)"
  [[ "$(wc -l <"$work/seed.agents")" -ge 2 ]] ||
    fail "$section" "find two sessions the head lists" "it lists $(jq -c '[(if type == "array" then . else .agents end)[].session_id]' "$work/body")"
  pass "$section" "the head lists $(wc -l <"$work/seed.agents") sessions"
}

# setup_section: setup's two halves, each isolated, the section recorded as failed when either
# fails. It is run as a command of its own, never under an `||`, which would turn errexit off in
# both isolated halves (isolated).
setup_section() {
  local failed=0
  isolated setup_base
  [[ "$isolated_status" == 0 ]] || failed=1
  isolated setup_seed
  [[ "$isolated_status" == 0 ]] || failed=1
  [[ "$failed" == 0 ]] || failed_sections+=(setup)
}

# --- callers: no read of the three paths in the four trees and the e2e suite ----------------------

# The four trees the check names, and the e2e suite beside them. The suite's specs wait for, stub
# or hold the page's own requests by their paths, and a wait on a path the page no longer reads
# compiles and lints clean and fails only in a browser run, where it times out; such a wait is as
# often a regex with escaped slashes as a literal. So the search reads the suite too, beyond the
# four trees the check names, and spares by path the few of its files that read a route.
readonly caller_trees=(packages/dispatch/web/src packages/envoy-client/src packages/pi-envoy/src packages/claude-envoy/src packages/dispatch/e2e)
# The files of the suite that name a read of the three on purpose, each by its exact path:
# - inbox-unchanged.e2e.ts reads the base build's GET /api/v1/inbox?assignee= to compare the
#   head's read of the open asks with it;
# - this file and one-inbox-kinds.sh, acceptance drivers whose checks have the kept reads as their
#   subject: old_routes here reads all three from both builds, and kinds reads
#   GET /api/v1/agents/{session_id}/messages beside the session's conversation;
# - __tests__/stub-recording.test.ts, whose GET /api/v1/inbox is a sample URL the recorder's own
#   test writes and normalises, and no page asks.
# And the suite's recordings: each HAR under __recordings__ holds the requests a page made when it
# was recorded, data that calls nothing, and is recorded again against this branch's server before
# the hand-off. They are spared by their extension, so a script or spec under that directory is
# still read.
readonly callers_spared=(
  ':(exclude)packages/dispatch/e2e/inbox-unchanged.e2e.ts'
  ":(exclude)$self"
  ':(exclude)packages/dispatch/e2e/acceptance/one-inbox-kinds.sh'
  ':(exclude)packages/dispatch/e2e/__tests__/stub-recording.test.ts'
  ':(exclude)packages/dispatch/e2e/__recordings__/*.har'
)
# Each path the three reads answer at, in every way a caller writes it: a literal, a template, a
# route pattern or a glob; a regex, its slashes escaped (`\/`, or `\\/` in the string a RegExp is
# built from); string pieces joined by `+` ("/api/v1/" + "inbox"); or the segment list
# envoy-client's dispatch-http.ts builds a URL from. Neither a regex's alternation nor a prefix
# held in a variable of its own is followed.
readonly quote='["'"'"'`]'
readonly slash='\\*/'
readonly joint="(${quote}[[:space:]]*\\+[[:space:]]*${quote})?"
readonly sep="${joint}${slash}${joint}"
readonly lead='(^|[^-A-Za-z0-9_])api'
# Where a path ends: at anything but a further segment. A slash is a further segment unless it
# closes a regex literal (flags and all), and a backslash unless it escapes something else, as
# `\?` does.
readonly path_end='([^-/A-Za-z0-9_\\]|/[dgimsuvy]*([^-/A-Za-z0-9_{$:*.[\\]|$)|\\+[^/\\]|\\*$)'
# A session id in the sessions' path: a template's `${...}`, a shell's `$(...)`, a route
# pattern's `{session_id}`, a glob's `*`, a regex's class (`[^/]+`, which holds a slash) or escape
# (`\w+`), or an expression joined in by `+` ("/api/v1/agents/" + id + "/messages").
readonly session_segment='([^/[:space:]"'"'"'`\\]|\\[^/]|\[[^]]*\]|\$\{[^}]*\}|\$\([^)]*\)|'"$quote"'[[:space:]]*\+[^"'"'"'`]*\+[[:space:]]*'"$quote"')+'
readonly path_inbox="${lead}${sep}v1${sep}inbox${path_end}"'|"api", *"v1", *"inbox" *\]'
readonly path_replies="${lead}${sep}v1${sep}me${sep}replies${path_end}"'|"api", *"v1", *"me", *"replies" *\]'
readonly path_messages="${lead}${sep}v1${sep}agents${sep}${session_segment}${sep}messages${path_end}"'|"api", *"v1", *"agents", *[^],]+, *"messages" *\]'
# A send is what names the POST: its method, the web client's send (createAgentMessage) or a send
# helper, on the line of the path or within six lines of it. In the e2e suite that is also one of
# its send helpers (sends.ts's refusePosts and holdPosts, a `posted(` wait), the `delivery` field
# only a message's body carries, or `sent`, the name the suite gives a send it waits on. A line
# that names a read itself is a read whatever is near it.
readonly names_a_send='\bPOST\b|["'"'"']post["'"'"']|\.post\(|createAgentMessage|\bsend[A-Z][A-Za-z0-9_]*\(|\bsend\(|[a-z]Posts\b|\bposted\(|\bdelivery"?:|\b(const|let) sent\b|\bawait sent\b'
readonly names_a_read='\bGET\b|["'"'"']get["'"'"']|\.get\(|\.read\(|#json\("GET"'
readonly a_comment='^[[:space:]]*(//|\*|/\*|#)'

# callers_in ROOT: every read of the three paths under ROOT's five trees, the spared files aside,
# one `path:line: <read>` each. A comment line names a path and calls nothing; a hit of the
# sessions' path is a send, and no read, where the send's words stand near it.
callers_in() {
  local root="$1" hits file line text read from
  hits="$(grep_tree "$root" -e "$path_inbox" -e "$path_replies" -e "$path_messages" -- "${caller_trees[@]}" "${callers_spared[@]}")" ||
    return 1
  [[ -n "$hits" ]] || return 0
  while IFS= read -r hit; do
    file="${hit%%:*}"
    line="${hit#*:}"
    text="${line#*:}"
    line="${line%%:*}"
    [[ ! "$text" =~ $a_comment ]] || continue
    if grep -qE "$path_inbox" <<<"$text"; then
      read="GET /api/v1/inbox"
    elif grep -qE "$path_replies" <<<"$text"; then
      read="GET /api/v1/me/replies"
    else
      from=$((line > 6 ? line - 6 : 1))
      if ! grep -qE "$names_a_read" <<<"$text" &&
        sed -n "${from},$((line + 6))p" "$root/$file" | grep -qE "$names_a_send"; then
        continue
      fi
      read="GET /api/v1/agents/{session_id}/messages"
    fi
    printf '%s:%s: %s: %s\n' "$file" "$line" "$read" "$(sed -E 's/^[[:space:]]+//' <<<"$text" | head -c 160)"
  done <<<"$hits"
}

callers_section() {
  local section=callers plant="$work/plant-callers" found
  # The scratch tree holds the five trees and what is planted in them alone: the run over the
  # repository below reads the real trees, where a file no longer spared fails that run.
  local tree
  for tree in "${caller_trees[@]}" packages/dispatch/e2e/__recordings__; do
    mkdir -p "$plant/$tree"
  done
  # A read of each path, in a template and in a segment list, and a send far enough below them
  # that no read is within its six lines.
  cat >"$plant/packages/pi-envoy/src/planted-caller.ts" <<'TS'
export async function planted(base: string, session: string, client: { json: Function }) {
  const replies = await fetch(`${base}/api/v1/me/replies?kind=reply`);
  const asks = await client.json("GET", ["api", "v1", "inbox"], undefined, { assignee: "me" });
  const thread = await fetch(`${base}/api/v1/agents/${session}/messages`);
  const mine = await client.json("GET", ["api", "v1", "me", "replies"]);
  const theirs = await client.json("GET", ["api", "v1", "agents", session, "messages"]);






  await fetch(`${base}/api/v1/agents/${session}/messages`, { method: "POST", body: "{}" });
  return [replies, asks, thread, mine, theirs];
}
TS
  # A spec's waits on each path as regexes with escaped slashes - in a literal, and in the string
  # a RegExp is built from - two reads of string pieces joined by `+`, a wait on a glob and a read
  # of a route pattern; then a send helper given a glob, as far below them.
  cat >"$plant/packages/dispatch/e2e/planted-caller.e2e.ts" <<'TS'
export async function planted(page: Page, base: string, session: string) {
  await page.waitForRequest(/\/api\/v1\/inbox(\?|$)/);
  await page.route(new RegExp("\\/api\\/v1\\/me\\/replies(\\?.*)?$"), (route) => route.continue());
  await page.waitForResponse(/\/api\/v1\/agents\/[^/]+\/messages$/);
  const split = await fetch(base + "/api/v1/" + "inbox");
  const joined = await fetch(base + "/api/v1/agents/" + session + "/messages");
  await page.waitForResponse("**/api/v1/me/replies*");
  const route = { method: "GET", path: "/api/v1/agents/{session_id}/messages" };
  return [split, joined, route];
}






export async function plantedSend(page: Page) {
  await refusePosts(page, "**/api/v1/agents/*/messages");
}
TS
  # A script beside the recordings, which their sparing by extension still reads.
  printf '%s\n' 'await fetch("/api/v1/inbox");' >"$plant/packages/dispatch/e2e/__recordings__/planted-caller.ts"
  found="$(callers_in "$plant")" || fail "$section" "search the scratch tree" "git grep failed"
  local want
  local -a wants=(
    "packages/pi-envoy/src/planted-caller.ts:2: GET /api/v1/me/replies"
    "packages/pi-envoy/src/planted-caller.ts:3: GET /api/v1/inbox"
    "packages/pi-envoy/src/planted-caller.ts:4: GET /api/v1/agents/{session_id}/messages"
    "packages/pi-envoy/src/planted-caller.ts:5: GET /api/v1/me/replies"
    "packages/pi-envoy/src/planted-caller.ts:6: GET /api/v1/agents/{session_id}/messages"
    "packages/dispatch/e2e/planted-caller.e2e.ts:2: GET /api/v1/inbox"
    "packages/dispatch/e2e/planted-caller.e2e.ts:3: GET /api/v1/me/replies"
    "packages/dispatch/e2e/planted-caller.e2e.ts:4: GET /api/v1/agents/{session_id}/messages"
    "packages/dispatch/e2e/planted-caller.e2e.ts:5: GET /api/v1/inbox"
    "packages/dispatch/e2e/planted-caller.e2e.ts:6: GET /api/v1/agents/{session_id}/messages"
    "packages/dispatch/e2e/planted-caller.e2e.ts:7: GET /api/v1/me/replies"
    "packages/dispatch/e2e/planted-caller.e2e.ts:8: GET /api/v1/agents/{session_id}/messages"
    "packages/dispatch/e2e/__recordings__/planted-caller.ts:1: GET /api/v1/inbox"
  )
  for want in "${wants[@]}"; do
    [[ "$found" == *"$want:"* ]] ||
      fail "$section" "the search finds a planted read ($want) in a scratch tree of the five trees" "it found: ${found:-nothing}"
  done
  [[ "$(grep -c . <<<"$found")" == "${#wants[@]}" ]] ||
    fail "$section" "the search finds the ${#wants[@]} planted reads in the scratch tree, and nothing else" "it found: $found"
  pass "$section" "the search finds the ${#wants[@]} planted reads in a scratch tree of the five trees - plain, in a template, in a segment list, regex-escaped, joined by +, as a glob and as a route pattern - and not the two planted sends or anything else"

  found="$(callers_in "$repo")" || fail "$section" "search the five trees" "git grep failed"
  [[ -z "$found" ]] || fail "$section" "no read of the three paths in ${caller_trees[*]}" "$found"
  pass "$section" "no read of GET /api/v1/inbox, GET /api/v1/me/replies or GET /api/v1/agents/{session_id}/messages in ${caller_trees[*]}, the spared files aside"
}

# --- gone: the comparison and the opt-in ----------------------------------------------------------

# What the search looks for, each with its name: the comparison's command, package, table, route
# and page address, types and page, and the opt-in's query key - written as an address, or as the
# route builder and parser of web/src/features/refs/routes.ts wrote it - and its browser preference.
readonly gone_names=(
  "the comparison's command"
  "the comparison's package"
  "the comparison's table"
  "the comparison's route and page address"
  "the comparison's types, query and path constant"
  "the comparison's page"
  "the opt-in's query key"
  "the opt-in's browser preference"
)
readonly gone_patterns=(
  'inbox-compare'
  'inboxcompare'
  'inbox_comparison'
  '/inbox/comparison'
  '[Ii]nboxComparison|INBOX_COMPARISON'
  'ComparisonPage'
  '[?&]one=1([^0-9A-Za-z]|$)|\.set\("one", "1"\)|\.get\("one"\) === "1"|\b(filter|route)\.one\b|\bone\?: true\b'
  'inbox\.one_read'
)
# The lines planted in the scratch copy, each `<index of the pattern it is planted for>|<line>`:
# one for each alternative of each pattern.
readonly gone_planted=(
  '0|envoy-dispatch inbox-compare --old-url http://127.0.0.1:8780'
  '1|import "github.com/sjawhar/envoy/internal/dispatch/inboxcompare"'
  '2|select count(*) from inbox_comparison_runs'
  '3|<Route element={<Page />} path="/inbox/comparison" />'
  '4|api.getInboxComparison(): Promise<InboxComparisonResponse>'
  '4|export const INBOX_COMPARISON = true;'
  '5|const ComparisonPage = lazy(() => import("./Page"));'
  '6|return buildInboxPath({ one: true }); // "/?one=1"'
  '6|params.set("one", "1");'
  '6|const opted = params.get("one") === "1";'
  '6|if (filter.one) return true;'
  '6|type InboxFilter = { one?: true };'
  '7|readPreference("inbox.one_read")'
)
# Frozen history, and the two migrations whose text cannot change once applied: the one that made
# the comparison's table and this branch's that drops it. And this file, which names them all.
readonly gone_spared=(
  ':(exclude)*CHANGELOG.md'
  ':(exclude).changes/*'
  ':(exclude)*/.changes/*'
  ':(exclude)docs/solutions/*'
  ':(exclude)docs/plans/*'
  ':(exclude)packages/envoy/internal/dispatch/store/migrations/20261117132340_inbox_comparison_runs.*'
  ':(exclude)packages/envoy/internal/dispatch/store/migrations/*_drop_inbox_comparison_runs.*'
  ":(exclude)$self"
)

# gone_in ROOT: every line under ROOT, frozen history and the two migrations aside, that names the
# comparison or the opt-in, as `<name>: path:line: text`.
gone_in() {
  local root="$1" hits index
  local -a patterns=()
  for index in "${!gone_patterns[@]}"; do
    patterns+=(-e "${gone_patterns[$index]}")
  done
  hits="$(grep_tree "$root" "${patterns[@]}" -- . "${gone_spared[@]}")" || return 1
  [[ -n "$hits" ]] || return 0
  for index in "${!gone_patterns[@]}"; do
    while IFS= read -r hit; do
      printf '%s: %s\n' "${gone_names[$index]}" "$(head -c 200 <<<"$hit")"
    done < <(grep -E "^[^:]+:[0-9]+:.*(${gone_patterns[$index]})" <<<"$hits" || true)
  done
}

gone_section() {
  local section=gone plant="$work/plant-gone" found entry line=0 file
  # The planted lines stand in a file at a path the search reads (web/src/app.tsx), and one each
  # under the frozen history the search spares, so the planted run shows both that it finds a
  # line where it should and that it spares where it should.
  mkdir -p "$plant/packages/dispatch/web/src" "$plant/docs/plans" "$plant/packages/contracts"
  for file in packages/dispatch/web/src/app.tsx docs/plans/frozen.md packages/contracts/CHANGELOG.md; do
    for entry in "${gone_planted[@]}"; do
      printf '%s\n' "${entry#*|}"
    done >"$plant/$file"
  done
  found="$(gone_in "$plant")" || fail "$section" "search the scratch copy" "git grep failed"
  for entry in "${gone_planted[@]}"; do
    line=$((line + 1))
    [[ "$found" == *"${gone_names[${entry%%|*}]}: packages/dispatch/web/src/app.tsx:$line:"* ]] ||
      fail "$section" "the search finds ${gone_names[${entry%%|*}]} planted in a scratch copy as ${entry#*|}" "it found: ${found:-nothing}"
  done
  [[ "$found" != *docs/plans/* && "$found" != *CHANGELOG.md* ]] ||
    fail "$section" "the search spares frozen history" "it found: $found"
  pass "$section" "the search finds each of the ${#gone_planted[@]} lines planted in a scratch copy, one for each alternative of the ${#gone_patterns[@]} names, and spares frozen history"

  found="$(gone_in "$repo")" || fail "$section" "search the tree" "git grep failed"
  [[ -z "$found" ]] || fail "$section" "no tracked file but frozen history names the comparison or the opt-in" "$found"
  pass "$section" "no tracked file but frozen history and the two migrations names the comparison, its page, inbox-compare, the query key one=1 or the preference inbox.one_read"
}

# --- old_routes: the three kept reads answer as the base build answers ----------------------------

# The body of the last answer, with the top-level as_of the read stamps when it answers taken out.
normalized() { jq -S 'if type == "object" then del(.as_of) else . end' "$work/body"; }

# differences: what this head's answer ($work/head.json) holds that the base build's
# ($work/old.json) does not, and the other way round, by field (an array's elements as `[]`), and
# the first field whose value differs where both hold the same fields.
differences() {
  jq -rn --slurpfile h "$work/head.json" --slurpfile o "$work/old.json" '
    def fields: [paths((type != "object" and type != "array") or length == 0)
                 | map(if type == "number" then "[]" else . end) | join(".")] | unique;
    ($h[0] | fields) as $hf | ($o[0] | fields) as $of
    | (($hf - $of) | map("this head answers the field " + . + ", which the base build does not"))
      + (($of - $hf) | map("the base build answers the field " + . + ", which this head does not"))
    | if length > 0 then .[:5] | join("; ")
      else "both answer the same fields, and the value at "
           + (first(($h[0] | paths((type != "object" and type != "array") or length == 0)) as $p
                    | select(($h[0] | getpath($p)) != ($o[0] | getpath($p)))
                    | $p | map(tostring) | join(".")) // "(none)")
           + " differs"
      end'
}

# same_answer PERSON PATH: PATH answers PERSON the same body from both builds. A write some other
# caller lands between the two reads moves one of them, so the pair is read again, up to three
# times, before a difference fails.
same_answer() {
  local person="$1" path="$2" pair
  for pair in 1 2 3; do
    request "$person" head GET "$path"
    want_status 200 old_routes "GET $path as $person on this head"
    normalized >"$work/head.json" || fail old_routes "GET $path as $person on this head" "$(body)"
    request "$person" old GET "$path"
    want_status 200 old_routes "GET $path as $person on the base build"
    normalized >"$work/old.json" || fail old_routes "GET $path as $person on the base build" "$(body)"
    cmp -s "$work/head.json" "$work/old.json" && break
    [[ "$pair" -lt 3 ]] || fail old_routes "GET $path answers $person the same body from this head and the base build" "$(differences)"
    sleep 1
  done
  pass old_routes "GET $path answers $person the same body from this head and the base build"
}

old_routes_section() {
  local section=old_routes person agent count=0
  [[ -e "$work/base.up" ]] || fail "$section" "read the base build" "setup did not boot it"
  # Everyone who has signed in, as the head lists them: each is a person the three reads answer.
  request "$first" head GET /api/v1/users
  want_status 200 "$section" "list the people"
  mapfile -t people < <(jq -r '.users[].login' "$work/body")
  [[ "${#people[@]}" -gt 0 ]] || fail "$section" "list the people" "the head lists none: $(body)"
  [[ -s "$work/seed.agents" ]] || fail "$section" "read the sessions setup listed" "setup listed none"
  mapfile -t agents <"$work/seed.agents"
  for person in "${people[@]}"; do
    [[ -s "$work/jar.$person" ]] || sign_in "$section" "$person"
    same_answer "$person" /api/v1/inbox
    same_answer "$person" "/api/v1/me/replies?kind=reply,agent"
    same_answer "$person" "/api/v1/me/replies?kind=reply,agent&state=inbox"
    for agent in "${agents[@]}"; do
      same_answer "$person" "/api/v1/agents/$(q "$agent")/messages"
    done
    count=$((count + 3 + ${#agents[@]}))
  done
  pass "$section" "the three kept reads answered all ${#people[@]} people the same $count bodies from both builds"
}

# --- isolation and default: the one read, a person's own ----------------------------------------

# one_read SECTION PERSON: reads PERSON's one read with no query into $work/one-read.json, and
# sets $read_person to the person the read names.
one_read() {
  request "$2" head GET /api/v1/me/inbox
  want_status 200 "$1" "read $2's one read"
  cp "$work/body" "$work/one-read.json"
  read_person="$(jq -r '.person' "$work/one-read.json")" || fail "$1" "read $2's one read" "$(body)"
}

# entry_of SECTION KIND ID: sets $entry to the relations that tie KIND/ID to the person in the one
# read one_read read last, as `kind direction` sorted and space-separated, or empty where the read
# holds no such entry.
entry_of() {
  entry="$(jq -r --arg k "$2" --arg i "$3" '
    [.entries[]? | select(.node.kind == $k and .node.id == $i) | .relations[]? | .kind + " " + .direction]
    | sort | join(" ")' "$work/one-read.json")" ||
    fail "$1" "read the one read's entry of $2 $3" "$(head -c 600 "$work/one-read.json")"
}

isolation_section() {
  local section=isolation who other mine theirs own foreign
  for who in first second; do
    other=second
    [[ "$who" == first ]] || other=first
    mine="$(seeded "$section" "${who}_comment")"
    theirs="$(seeded "$section" "${other}_comment")"
    one_read "$section" "${!who}"
    entry_of "$section" comment "$mine"
    own="$entry"
    [[ "$read_person" == "${!who}" ]] || fail "$section" "${!who}'s one read is ${!who}'s" "it names $read_person"
    [[ "$own" == *"authored_by "* ]] ||
      fail "$section" "${!who}'s one read holds the comment ${!who} wrote" "its entry's relations: '${own:-no entry}'"
    entry_of "$section" comment "$theirs"
    foreign="$entry"
    [[ -z "$foreign" ]] ||
      fail "$section" "${!who}'s one read holds nothing of ${!other}'s comment" "it holds it, by: $foreign"
    pass "$section" "${!who}'s one read holds the comment ${!who} wrote ($own) and not ${!other}'s"
  done
}

default_section() {
  local section=default comment entry
  comment="$(seeded "$section" first_comment)"
  # GET /api/v1/me/inbox with no query at all: no state, no limit and no opt-in.
  one_read "$section" "$first"
  entry_of "$section" comment "$comment"
  [[ "$entry" == *"authored_by "* ]] ||
    fail "$section" "the one read answers $first's unanswered comment by default" "its entry's relations: '${entry:-no entry}'"
  pass "$section" "the one read, read with no query and so with no opt-in, answers $first the comment nobody answered ($entry)"
  # No Reply row is that comment's: a Reply row is a thread somebody else wrote in.
  request "$first" head GET "/api/v1/me/replies?kind=reply,agent&state=inbox"
  want_status 200 "$section" "read $first's Reply rows"
  expect "$section" "no Reply row of $first's is the comment nobody answered, which only the one read answers" 0 \
    --arg c "$comment" '[.rows[]? | select(.thread_id == $c)] | length'
}

# --- open_asks: the asks that wait on one person ---------------------------------------------------

open_asks_section() {
  local section=open_asks who other mine theirs
  for who in first second; do
    other=second
    [[ "$who" == first ]] || other=first
    mine="$(seeded "$section" "${who}_ask")"
    theirs="$(seeded "$section" "${other}_ask")"
    request agent head GET "/api/v1/asks/open?project=$(q "$project")&waiting_on=$(q "${!who}")"
    want_status 200 "$section" "GET /api/v1/asks/open?waiting_on=${!who}"
    expect "$section" "it answers the ask on ${!who}'s issue, which waits on ${!who}" 1 \
      --arg a "$mine" '[.asks[]? | select(.id == $a)] | length'
    expect "$section" "it answers no ask on ${!other}'s issue" 0 --arg a "$theirs" '[.asks[]? | select(.id == $a)] | length'
    expect "$section" "every ask it answers waits on ${!who}" '[]' --arg p "${!who}" \
      '[.asks[]? | select(.waiting_on_person != $p) | .id]'
  done
}

# --- conversation and post_stays: a session's conversation, and the send that stays -------------

conversation_section() {
  local section=conversation agent elsewhere message reply
  [[ -s "$work/seed.agents" ]] || fail "$section" "read the two sessions setup listed" "setup listed none"
  agent="$(sed -n 1p "$work/seed.agents")"
  elsewhere="$(sed -n 2p "$work/seed.agents")"
  [[ -n "$agent" && -n "$elsewhere" ]] || fail "$section" "read the two sessions setup listed" "setup listed none"
  post "$section" "$first sends $agent a message" "$first" "/api/v1/agents/$(q "$agent")/messages" \
    "$(jq -cn --arg b "A message to $agent, $stamp." '{body: $b, delivery: "btw"}')"
  message="$id"
  post "$section" "$agent replies to it" agent "/api/v1/messages/$message/reply" \
    "$(jq -cn --arg s "$agent" '{body: "Seen.", attempt: 1, actor: {kind: "session", id: $s}}')"
  reply="$id"
  request "$first" head GET "/api/v1/agents/$(q "$agent")/conversation"
  want_status 200 "$section" "GET /api/v1/agents/$agent/conversation as $first"
  expect "$section" "$agent's conversation holds $first's message once, with $agent's reply under it" '[1]' \
    --arg m "$message" --arg r "$reply" '[.[] | select(.message.id == $m) | [.replies[]? | select(.id == $r)] | length]'
  request "$first" head GET "/api/v1/agents/$(q "$elsewhere")/conversation"
  want_status 200 "$section" "GET /api/v1/agents/$elsewhere/conversation as $first"
  expect "$section" "$elsewhere's conversation does not hold the message to $agent" 0 \
    --arg m "$message" '[.[] | select(.message.id == $m)] | length'
}

post_stays_section() {
  local section=post_stays agent message body="A send through the route that stays, $stamp."
  [[ -s "$work/seed.agents" ]] || fail "$section" "read the session setup listed" "setup listed none"
  agent="$(sed -n 1p "$work/seed.agents")"
  [[ -n "$agent" ]] || fail "$section" "read the session setup listed" "setup listed none"
  post "$section" "POST /api/v1/agents/$agent/messages takes $first's message" "$first" \
    "/api/v1/agents/$(q "$agent")/messages" "$(jq -cn --arg b "$body" '{body: $b, delivery: "btw"}')"
  message="$id"
  request "$first" head GET "/api/v1/messages/$message"
  want_status 200 "$section" "read $message back"
  expect "$section" "the message is stored as sent, to $agent" "$(jq -cn --arg b "$body" --arg t "session:$agent" '[$b, $t]')" \
    '[.message.body, .message.target]'
}

# --- names: no read of the two is named inbox ----------------------------------------------------

readonly name_files=(
  packages/envoy/internal/dispatch/api/routes_table.go
  packages/envoy/internal/dispatch/api/routes_shapes.go
  packages/contracts/src/dispatch-api.generated.ts
  packages/dispatch/web/src/api/client.ts
  packages/dispatch/web/src/api/queries.ts
)
readonly name_kinds="handler shape operation contract-type client-method query query-key"

# names_of ROOT PATH: every name the read at PATH goes by under ROOT, one `<kind> <name>` each: its
# path; its handler, from its row in api/routes_table.go; its response and parameter shapes, from
# api/routes_shapes.go; its operation and the contract types its 200 answer is, with those types'
# own, in the generated contract; the web client's method that reads it; and the query that calls
# that method, with the words of its key.
names_of() {
  local path="$2" api="$1/packages/envoy/internal/dispatch/api" contract operation type method
  local generated="$1/packages/contracts/src/dispatch-api.generated.ts" web="$1/packages/dispatch/web/src/api"
  # The generated contract is rendered at install (packages/contracts/scripts/gen-api.ts), so its
  # indentation is the renderer's: a block is found by its opening line with the indentation
  # trimmed (trimmed), and it ends at the first closing brace at the indentation it opened at
  # (opened, closes).
  local block='function trimmed(   line) {line = $0; sub(/^ */, "", line); return line}
    function opened() {f = 1; match($0, /^ */); indent = RLENGTH}
    function closes() {match($0, /^ */); return RLENGTH == indent && $0 ~ /^ *};?$/}'
  printf 'path %s\n' "$path"
  grep -F "{http.MethodGet, \"$path\"," "$api/routes_table.go" | sed -nE 's/.*s\.([A-Za-z0-9_]+)\},?[[:space:]]*$/handler \1/p'
  grep -F "\"GET $path\":" "$api/routes_shapes.go" | grep -oE '(Response|Params): *(\[\])?[A-Za-z0-9_]+' |
    sed -E 's/^[A-Za-z]+: *(\[\])?/shape /'
  operation="$(awk -v p="\"$path\": {" "$block"' !f && trimmed() == p {opened(); next}
      f && /get: operations\["/ {sub(/.*get: operations\["/, ""); sub(/"\].*/, ""); print; exit} f && closes() {exit}' "$generated")"
  [[ -z "$operation" ]] || printf 'operation %s\n' "$operation"
  contract="$(awk -v o="$operation: {" "$block"' !f && trimmed() == o {opened(); next} f && closes() {exit}
      f && /^ *200: \{/ {ok = 1; next} f && ok && /^ *(default|[0-9][0-9][0-9]): \{/ {exit}
      f && ok {while (match($0, /components\["schemas"\]\["[A-Za-z0-9_]+"\]/)) {
        s = substr($0, RSTART + 23, RLENGTH - 25); print s; $0 = substr($0, RSTART + RLENGTH)}}' "$generated" | sort -u)"
  for type in $contract; do
    printf 'contract-type %s\n' "$type"
    awk -v t="$type: {" "$block"' !f && trimmed() == t {opened(); next} f && closes() {exit}
        f {while (match($0, /components\["schemas"\]\["[A-Za-z0-9_]+"\]/)) {
          s = substr($0, RSTART + 23, RLENGTH - 25); print "contract-type " s; $0 = substr($0, RSTART + RLENGTH)}}' "$generated"
  done | sort -u
  method="$(awk -v p="this.read(\"$path\"" '/^  [A-Za-z0-9_]+\(/ {m = $1; sub(/\(.*/, "", m)} index($0, p) {print m; exit}' "$web/client.ts")"
  [[ -z "$method" ]] || printf 'client-method %s\n' "$method"
  [[ -n "$method" ]] || return 0
  # The query whose queryFn calls the method, and the words of its key, a spread of a constant key
  # (`...openAsksKey`) read as that constant's words.
  awk -v call="api.$method(" '
      /^export const [A-Za-z0-9_]+ =/ {q = $3; key = ""}
      /queryKey:/ {key = $0}
      index($0, call) && q != "" {print "query " q; print "key " key; exit}' "$web/queries.ts" >"$work/query"
  sed -n 's/^query /query /p' "$work/query"
  local key spread
  key="$(sed -n 's/^key //p' "$work/query")"
  grep -oE '"[^"]*"' <<<"$key" | tr -d '"' | sed 's/^/query-key /' || true
  while IFS= read -r spread; do
    grep -E "^export const $spread = " "$web/queries.ts" | grep -oE '"[^"]*"' | tr -d '"' | sed 's/^/query-key /' || true
  done < <(grep -oE '\.\.\.[A-Za-z0-9_]+' <<<"$key" | sed 's/^\.\.\.//' || true)
}

# named_inbox ROOT: the names of the open-asks read and the session-conversation read under ROOT
# that hold the word inbox, any case, as `<path>: <kind> <name>`; a read for which a kind yields
# no name fails, since a reading that found nothing would hold no inbox either. Every name it reads
# is added to $work/names, one per line.
named_inbox() {
  local root="$1" path kind names
  for path in /api/v1/asks/open "/api/v1/agents/{session_id}/conversation"; do
    names="$(names_of "$root" "$path")"
    printf '%s\n' "$names" >>"$work/names"
    for kind in $name_kinds; do
      grep -q "^$kind " <<<"$names" || printf 'MISSING %s: no %s read\n' "$path" "$kind"
    done
    grep -i 'inbox' <<<"$names" | sed "s#^#$path: #" || true
  done
}

names_section() {
  local section=names plant="$work/plant-names" found file want
  for file in "${name_files[@]}"; do
    mkdir -p "$plant/$(dirname "$file")"
    cp "$repo/$file" "$plant/$file" || fail "$section" "copy $file" "cp failed"
  done
  # One name of each kind but the path planted: a handler, a response shape, an operation, a
  # contract type and a type of its own (read from the first's block), a client method (and the
  # query's call of it), a query and the words of two keys.
  sed -i 's/s\.listOpenAsks},/s.listInboxOpenAsks},/' "$plant/${name_files[0]}"
  sed -i 's/Response: openAsksResponse{}/Response: openAsksInboxResponse{}/' "$plant/${name_files[1]}"
  sed -i -e 's/\["OpenAsksResponse"\]/["OpenAsksInboxResponse"]/' -e 's/^\( *\)OpenAsksResponse: {/\1OpenAsksInboxResponse: {/' \
    -e 's/\["OpenAsk"\]/["OpenAskInbox"]/g' \
    -e 's/getApiV1AsksOpen/getApiV1InboxAsksOpen/g' \
    "$plant/${name_files[2]}"
  sed -i 's/^  getAgentConversation(/  getAgentInboxConversation(/' "$plant/${name_files[3]}"
  sed -i -e 's/api\.getAgentConversation(/api.getAgentInboxConversation(/' \
    -e 's/^export const openAsksKey = \["open-asks"\]/export const openAsksKey = ["open-asks-inbox"]/' \
    -e 's/queryKey: \["agents", sessionId, "conversation"\]/queryKey: ["agents", sessionId, "inbox-conversation"]/' \
    -e 's/^export const agentConversationQuery = /export const agentInboxConversationQuery = /' \
    "$plant/${name_files[4]}"
  found="$(named_inbox "$plant")"
  for want in "handler listInboxOpenAsks" "shape openAsksInboxResponse" "operation getApiV1InboxAsksOpen" \
    "contract-type OpenAsksInboxResponse" "contract-type OpenAskInbox" "client-method getAgentInboxConversation" \
    "query agentInboxConversationQuery" "query-key open-asks-inbox" "query-key inbox-conversation"; do
    [[ "$found" == *": $want"* ]] ||
      fail "$section" "the reading of the two reads' names finds the planted $want in a scratch copy" "it found: ${found:-nothing}"
  done
  pass "$section" "the reading of the two reads' names finds the nine names planted in a scratch copy"

  : >"$work/names"
  found="$(named_inbox "$repo")"
  [[ -z "$found" ]] ||
    fail "$section" "no path, handler, shape, operation, contract type, client method, query or query key of the two reads is named inbox" "$found"
  pass "$section" "none of the $(wc -l <"$work/names") names of GET /api/v1/asks/open and GET /api/v1/agents/{session_id}/conversation holds the word inbox"
}

# --- the run -----------------------------------------------------------------------------------

# run_section NAME: runs NAME_section on its own (isolated), recording it in $failed_sections when
# it fails, so every section runs whatever an earlier one found.
failed_sections=()
run_section() {
  if [[ "$1" == setup ]]; then
    setup_section
    return 0
  fi
  isolated "$1_section"
  [[ "$isolated_status" == 0 ]] || failed_sections+=("$1")
}

for chosen in $all_sections; do
  [[ " $sections " != *" $chosen "* ]] || run_section "$chosen"
done

if [[ "${#failed_sections[@]}" -gt 0 ]]; then
  printf 'one-inbox-callers.sh: %d section(s) failed: %s\n' "${#failed_sections[@]}" "${failed_sections[*]}" >&2
  exit 1
fi
printf 'one-inbox-callers.sh: every check passed (%s)\n' "$sections"
