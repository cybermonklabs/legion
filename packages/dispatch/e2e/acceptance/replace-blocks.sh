#!/usr/bin/env bash
# Drives what `dispatch doc-edit`'s `replace` does with a `with` that names blocks
# (dispatch://INFRA-1520), end to end through the real `dispatch` command against a running
# Dispatch, as an agent runs it: `with` is read by the block parser an `insert`'s `markdown` is
# read by, and the blocks it names are written over the match, fitted where the match sits; a
# `with` of one paragraph keeps the inline behaviour it always had; and every `doc-edit` answers
# with the markdown of the blocks it wrote, which the command prints under its `Applied` line.
#
# It runs in four sections, each reading the document back with `dispatch doc-read --blocks`:
#
#   one    a list written over a paragraph's whole text: the read-back holds the paragraph with
#          the first line of what was written, and the `bullet_list` beside it with its items;
#   two    a list written over the text of a nested list item: the list stands inside that item,
#          beside the item's own text, and not beside the list the item is in;
#   three  the three refusals, each leaving the document at the version it had: a match in a
#          heading, a match in a table cell, and a `with` whose first line opens with the matched
#          item's own marker;
#   four   the answer of each `doc-edit` the run applied: the markdown of the blocks it wrote,
#          printed under the `Applied` line, and the two versions those edits left.
#
# Inputs, and how it reaches a server:
#
# DATABASE_URL, a loopback PostgreSQL database of this run's own, starts a Dispatch of its own
# with `bun run dispatch:local` (packages/dispatch/e2e/local-instance.ts) and stops it again from
# this script's trap, however the run ends. That command truncates and reseeds the database it is
# given, so the database must be one nothing else holds: a DATABASE_URL naming the database of
# LEGION_TEST_PG_DSN is refused. It builds the dashboard, builds the server and seeds before it
# prints anything, so the wait for it is minutes long and bounded (instance_seconds); it refuses
# to start at all when PLAYWRIGHT_BASE_URL or DISPATCH_E2E_PROCESSES is set, which is refused here
# first, by name. The instance prints no bare base URL, so the base URL is cut out of its
# `Open (signed in as alice): <base>/auth/_dev/signin?login=alice&next=/` line and used exactly as
# printed: another Host earns 421. Its agent bearer is `e2e-token`. Nothing this run starts
# inherits the Dispatch, Envoy, NATS or libpq variables a Legion pane exports: they are unset
# before the instance starts.
#
# DISPATCH_URL (the server), with DISPATCH_TOKEN or DISPATCH_TOKEN_FILE (an agent bearer, read
# here and never printed), is the other way: the caller's own server, and this script starts and
# stops nothing. PROJECT, optional, is an existing project to write the run's one issue in; with
# no PROJECT the run makes a project of its own with `dispatch project`. DATABASE_URL names the
# server to use when both are given, since it is the one a caller writes on the command line and
# DISPATCH_URL is what an agent's shell already holds; the run says so and leaves that server
# alone.
#
# The run writes one issue, with a spec of its own, and the project it made for it where it made
# one; it closes that issue at the end, also when a check fails. Nothing it writes is read by a
# second run, so it may be run again at once.
#
# Each passing check prints `PASS: <section> <what>`; the first failing one prints
# `FAIL: <section> <what>: <observed>` and the script exits 1. A refusal the run does not expect
# fails it, and so does a run that wrote nothing: section four reads the versions the two edits
# left.
#
# Every check is `cmd || fail …`, never part of an `&&` list: under `set -e` a failing command
# anywhere but last in such a list does not stop the script. A negative check — `grep -q … ||
# fail` and the like — is written as the AND list it reads as, and is safe for that same reason:
# its failing command is not the last one, so `set -e` leaves the `|| fail` to decide.

set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: DATABASE_URL=postgres://user@127.0.0.1:5433/<a database of this run's own> \
         replace-blocks.sh
       (starts a Dispatch of its own with `bun run dispatch:local`, which truncates and reseeds
       that database, and stops it again; PLAYWRIGHT_BASE_URL and DISPATCH_E2E_PROCESSES are
       refused, since that command refuses them)

       DISPATCH_URL=http://127.0.0.1:8777 DISPATCH_TOKEN=e2e-token [PROJECT=CORE] \
         replace-blocks.sh
       (the caller's own running server; DISPATCH_TOKEN_FILE=<file holding the bearer> in place of
       DISPATCH_TOKEN; with no PROJECT the run makes a project of its own)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

require_commands bun curl jq grep sed head mktemp date

script_dir="$(cd -- "$(dirname -- "$0")" && pwd)"
readonly script_dir
readonly repo_root="$script_dir/../../../.."
readonly cli_entry="$script_dir/../../../envoy-client/bin/dispatch.ts"

# How long the instance this run starts may take to print its signed-in line. It builds the
# dashboard, builds the Go server and seeds the database first: measured on 2026-11-11 on a
# two-core pod, four minutes with the Go build cache cold and two minutes with it warm.
readonly instance_seconds=900

# database_name URL: the database a postgres:// URL names — its path, with the leading slash and
# any query taken off.
database_name() {
  local path="${1%%\?*}"
  printf '%s\n' "${path##*/}"
}

readonly database_url="${DATABASE_URL:-}"
if [[ -n "$database_url" ]]; then
  own_instance=1
  if [[ -n "${PLAYWRIGHT_BASE_URL:-}" || -n "${DISPATCH_E2E_PROCESSES:-}" ]]; then
    printf 'unset PLAYWRIGHT_BASE_URL and DISPATCH_E2E_PROCESSES: the dispatch:local command this run starts refuses to start with either of them set\n' >&2
    usage
    exit 2
  fi
  # The pod's shared test database, which every other check of this repository reads and writes.
  # The value a Legion pane exports carries its own quotes, so they come off before it is read.
  shared_dsn="${LEGION_TEST_PG_DSN-}"
  shared_dsn="${shared_dsn#[\"\']}"
  shared_dsn="${shared_dsn%[\"\']}"
  if [[ -n "$shared_dsn" && "$(database_name "$database_url")" == "$(database_name "$shared_dsn")" ]]; then
    printf 'DATABASE_URL names the database %s, which LEGION_TEST_PG_DSN names too: the dispatch:local command truncates and reseeds the database it is given, so give this run a database of its own (createdb)\n' \
      "$(database_name "$database_url")" >&2
    usage
    exit 2
  fi
  require_commands setsid ps tail
  if [[ -n "${DISPATCH_URL:-}" ]]; then
    printf 'replace-blocks.sh: DATABASE_URL names the Dispatch to start, so the DISPATCH_URL in the environment is left alone.\n'
  fi
  # Nothing this run starts reads the Dispatch, Envoy, NATS or libpq settings a Legion pane
  # exports: the instance serves a Dispatch of its own, on a database of its own, and its psql
  # must reach the host its URL names.
  mapfile -t inherited < <(compgen -e)
  for name in "${inherited[@]}"; do
    case "$name" in
      DISPATCH_* | ENVOY_* | NATS_* | PGHOSTADDR | PGSERVICE | PGSERVICEFILE) unset "$name" ;;
    esac
  done
  token=e2e-token
else
  own_instance=0
  require_env DISPATCH_URL
  read_bearer
  base="${DISPATCH_URL%/}"
fi
readonly own_instance
# No child process inherits the bearer from here: read_bearer has unset it from the environment,
# curl reads it from a header file, and the CLI gets it in an environment this script builds.

bun_bin="$(command -v bun)"
readonly bun_bin
readonly saved_path="$PATH"
readonly saved_tmpdir="${TMPDIR:-}"
session="replace-blocks-$(date +%s)-$$"
readonly session

umask 077
work="$(mktemp -d)"
readonly work
mkdir "$work/home" "$work/state"

# What the run started and wrote, for the trap below. The issue is closed from there, so a run
# that fails a check closes it too.
instance_pid=""
instance_group=""
issue_key=""

# stop_instance stops the Dispatch this run started, and is best effort: a cleanup that fails must
# never hide the check that failed. The instance leads a session of its own, so the whole group
# gets the signal: `bun run dispatch:local` is reached through two `bun run` parents, and the
# harness stops its own processes when its signal fires.
stop_instance() {
  local waited=0
  [[ -n "$instance_pid" ]] || return 0
  if [[ -n "$instance_group" ]]; then
    kill -TERM -"$instance_group" 2>/dev/null || return 0
  else
    kill -TERM "$instance_pid" 2>/dev/null || return 0
  fi
  while ((waited < 60)); do
    instance_running || return 0
    sleep 0.5
    waited=$((waited + 1))
  done
  if [[ -n "$instance_group" ]]; then
    kill -KILL -"$instance_group" 2>/dev/null || true
  else
    kill -KILL "$instance_pid" 2>/dev/null || true
  fi
}

# instance_running reports whether any process of the instance is still alive.
instance_running() {
  if [[ -n "$instance_group" ]]; then
    kill -0 -"$instance_group" 2>/dev/null
  else
    kill -0 "$instance_pid" 2>/dev/null
  fi
}

cleanup() {
  local code=$?
  if [[ -n "$issue_key" ]]; then
    cli issue-update --issue "$issue_key" --status "done" \
      --reason "The replace check closes the issue it wrote." \
      >"$work/cleanup" 2>&1 ||
      printf 'replace-blocks.sh: could not close %s: %s\n' "$issue_key" "$(<"$work/cleanup")" >&2
  fi
  stop_instance
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

pass() { printf 'PASS: %s %s\n' "$1" "$2"; }
fail() {
  printf 'FAIL: %s %s: %s\n' "$1" "$2" "$3" >&2
  exit 1
}

# The real `dispatch` CLI as an agent session runs it, in an environment holding only what this
# driver names: nothing a calling shell exported (its Dispatch URL, token file or Envoy config)
# reaches it.
cli() {
  (
    set +x
    local name
    local -a exported
    mapfile -t exported < <(compgen -e)
    for name in "${exported[@]}"; do
      unset "$name" 2>/dev/null || true
    done
    export PATH="$saved_path" HOME="$work/home" DISPATCH_STATE_DIR="$work/state"
    if [[ -n "$saved_tmpdir" ]]; then
      export TMPDIR="$saved_tmpdir"
    fi
    export DISPATCH_HOST=omp DISPATCH_SESSION_ID="$session" DISPATCH_URL="$base"
    export DISPATCH_TOKEN="$token"
    exec "$bun_bin" "$cli_entry" "$@"
  )
}

# run SECTION WHAT ARGS...: runs the CLI with ARGS, its output in $work/out; a non-zero exit fails.
run() {
  local section="$1" what="$2"
  shift 2
  cli "$@" >"$work/out" 2>&1 || fail "$section" "$what" "dispatch $1 exited $?: $(<"$work/out")"
}

# start_instance starts `bun run dispatch:local` on DATABASE_URL, in the background and in a
# session of its own, and sets $base to the base URL of the one line it prints with it. The wait
# is bounded and says what it waits for, and an instance that stops while it starts fails the run
# with the last lines of its log.
start_instance() {
  local deadline=$((SECONDS + instance_seconds)) own_group line
  printf 'replace-blocks.sh: starting a Dispatch of its own with bun run dispatch:local. It builds the dashboard, builds the server and seeds %s first, so this takes minutes.\n' \
    "$(database_name "$database_url")"
  (cd "$repo_root" && DATABASE_URL="$database_url" exec setsid bun run dispatch:local) \
    >"$work/instance.log" 2>&1 </dev/null &
  instance_pid=$!
  own_group="$(ps -o pgid= -p $$ 2>/dev/null | tr -d ' ' || true)"
  # The child is in this shell's own group until it reaches setsid, and a group of this shell's
  # own is never signalled: that would stop this run with the instance. So the group is read
  # until it is one of the instance's own, which is what stop_instance signals to reach the two
  # `bun run` parents and the server under them.
  instance_group=""
  while ((SECONDS < deadline)); do
    instance_group="$(ps -o pgid= -p "$instance_pid" 2>/dev/null | tr -d ' ' || true)"
    if [[ -z "$instance_group" || "$instance_group" != "$own_group" ]]; then
      break
    fi
    instance_group=""
    sleep 0.2
  done
  while :; do
    line="$(grep -m 1 -F '  Open (signed in as alice): ' "$work/instance.log" 2>/dev/null || true)"
    if [[ -n "$line" ]]; then
      base="${line#"  Open (signed in as alice): "}"
      base="${base%%/auth/_dev/signin*}"
      [[ "$base" == http*://* ]] ||
        fail setup "read the instance's base URL" "its signed-in line reads $line"
      printf 'replace-blocks.sh: the instance is up at %s.\n' "$base"
      return 0
    fi
    instance_running ||
      fail setup "the Dispatch this run started" \
        "it stopped while it started; the last lines of its log: $(tail -n 10 "$work/instance.log")"
    ((SECONDS < deadline)) ||
      fail setup "the Dispatch this run started" \
        "it printed no signed-in line in ${instance_seconds}s; the last lines of its log: $(tail -n 10 "$work/instance.log")"
    sleep 2
  done
}

if ((own_instance == 1)); then
  start_instance
fi
readonly base

# version_of SECTION WHAT: sets $version to the live version of the run's spec document.
version_of() {
  local status
  status="$(curl -sS -H "@$work/auth" -o "$work/body" -w '%{http_code}' \
    "$base/api/v1/issues/$issue_key/artifacts/spec/text" 2>"$work/curl")" ||
    fail "$1" "$2" "curl failed: $(<"$work/curl")"
  [[ "$status" == 200 ]] || fail "$1" "$2" "HTTP $status: $(head -c 400 "$work/body")"
  version="$(jq -er '.version' "$work/body")" ||
    fail "$1" "$2" "no version in: $(head -c 400 "$work/body")"
}

# edit SECTION WHAT OPS FILE: applies OPS through `dispatch doc-edit`, its answer kept in FILE for
# section four, which reads the markdown of the blocks each edit wrote.
edit() {
  run "$1" "$2" doc-edit --issue "$issue_key" --artifact spec --ops-json "$3"
  cp "$work/out" "$4"
  pass "$1" "$2"
}

# refused_edit SECTION WHAT OPS SUBSTRING: the edit is refused, and names SUBSTRING.
refused_edit() {
  cli doc-edit --issue "$issue_key" --artifact spec --ops-json "$3" >"$work/out" 2>&1 &&
    fail "$1" "$2" "the edit was accepted: $(<"$work/out")"
  grep -qF -- "$4" "$work/out" || fail "$1" "$2" "$(<"$work/out")"
  pass "$1" "$2"
}

# unchanged SECTION WHAT VERSION: the live document still stands at VERSION.
unchanged() {
  version_of "$1" "$2"
  [[ "$version" == "$3" ]] || fail "$1" "$2" "the document stands at version $version, want $3"
  pass "$1" "$2"
}

# blocks SECTION WHAT: reads the document's blocks into $work/blocks with `dispatch doc-read
# --blocks`, the read every section judges its write by.
blocks() {
  run "$1" "$2" doc-read --issue "$issue_key" --artifact spec --blocks
  cp "$work/out" "$work/blocks"
}

# block SECTION WHAT PATTERN: the one line of the read-back whose text after its indent matches
# the extended regular expression PATTERN, left in $line, with its depth — one level for each two
# spaces of indent, which is how the read-back shows a block inside another — in $depth. No such
# line, or more than one, fails the check.
block() {
  local count body indent
  # grep -c exits 1 when it counts none, which is a count and not a failure of the read.
  count="$(grep -cE -- "^ *($3)\$" "$work/blocks" || true)"
  [[ "$count" == 1 ]] ||
    fail "$1" "$2" "$count blocks read $3; the read-back is: $(<"$work/blocks")"
  line="$(grep -E -- "^ *($3)\$" "$work/blocks")"
  body="${line#"${line%%[![:space:]]*}"}"
  indent=$((${#line} - ${#body}))
  ((indent % 2 == 0)) || fail "$1" "$2" "the block is indented $indent spaces: $line"
  depth=$((indent / 2))
}

# at_depth SECTION WHAT DEPTH PATTERN: the one block reading PATTERN stands at DEPTH.
at_depth() {
  block "$1" "$2" "$4"
  [[ "$depth" == "$3" ]] || fail "$1" "$2" "it stands at depth $depth, want $3: $line"
  pass "$1" "$2"
}

# written SECTION WHAT FILE MARKDOWN: the lines an edit's answer printed between its `Applied`
# line and its `Document token:` line are MARKDOWN — the markdown of the blocks it wrote.
written() {
  local got
  got="$(sed -n '/^Applied /,/^Document token: /p' "$3" | sed '1d;$d')"
  [[ "$got" == "$4" ]] ||
    fail "$1" "$2" "the answer reads $(printf '%q' "$got"), want $(printf '%q' "$4")"
  pass "$1" "$2"
}

# --- setup ---------------------------------------------------------------------------------------

printf 'Authorization: Bearer %s\n' "$token" >"$work/auth"

project="${PROJECT:-}"
if [[ -z "$project" ]]; then
  project="RB$(date -u +%H%M%S)$(printf '%02d' $(($$ % 100)))"
  run setup "create the project $project" project --key "$project" --name "Replace with blocks"
  pass setup "created the project $project"
fi
readonly project

# The document each section writes on: a paragraph to write a list over, a heading and a table
# cell that hold one line and one paragraph of text and no block of their own, and a list holding
# a list, whose inner item is the one section two writes inside.
cat >"$work/spec.md" <<'SPEC'
The replace check's spec.

Replace the whole text of this paragraph.

## A heading that holds one line of text

| Step | Note |
| --- | --- |
| Parse | A cell that holds one paragraph. |

- An outer item.

  - An inner item.
SPEC

run setup "create the issue" issue --project "$project" --title "Replace writes the blocks its text names" \
  --spec-file "$work/spec.md" --force
[[ "$(<"$work/out")" =~ Created\ ([A-Z][A-Z0-9]*-[0-9]+): ]] ||
  fail setup "create the issue" "no issue key in: $(<"$work/out")"
issue_key="${BASH_REMATCH[1]}"
readonly issue_key
pass setup "created $issue_key in $project, with the spec the sections write on"

# --- one: a list written over a paragraph's whole text -------------------------------------------

one_find="Replace the whole text of this paragraph."
one_with="$(printf 'Here is the plan:\n\n- parse the with\n- fit the blocks')"
readonly one_find one_with
edit one "a list written over a paragraph's whole text" \
  "$(jq -cn --arg f "$one_find" --arg w "$one_with" '[{op: "replace", find: $f, with: $w}]')" \
  "$work/edit-one"
blocks one "read the blocks back"
at_depth one "the paragraph holds the first line of what the edit wrote" 0 \
  '[0-9a-f-]+ paragraph \(.+\): Here is the plan:'
at_depth one "a bullet_list stands where the paragraph's other text was written" 0 \
  '[0-9a-f-]+ bullet_list \(.+\): - parse the with - fit the blocks'
at_depth one "the list holds its first item" 1 '[0-9a-f-]+ paragraph \(.+\): parse the with'
at_depth one "the list holds its second item" 1 '[0-9a-f-]+ paragraph \(.+\): fit the blocks'
grep -qF -- "$one_find" "$work/blocks" &&
  fail one "the matched text is gone" "$(<"$work/blocks")"
pass one "the text the replace matched is gone from the document"

# --- two: a list written over the text of a nested list item -------------------------------------

two_find="An inner item."
two_with="$(printf 'An inner item.\n- a point inside the item')"
readonly two_find two_with
edit two "a list written over the text of a nested list item" \
  "$(jq -cn --arg f "$two_find" --arg w "$two_with" '[{op: "replace", find: $f, with: $w}]')" \
  "$work/edit-two"
blocks two "read the blocks back"
at_depth two "the list the item stands in holds the item and what was written in it" 1 \
  '[0-9a-f-]+ bullet_list \(.+\): - An inner item. - a point inside the item'
block two "the inner item's own text" '[0-9a-f-]+ paragraph \(.+\): An inner item.'
item_depth="$depth"
readonly item_depth
pass two "the inner item's own text stands at depth $item_depth"
block two "the list the edit wrote" '[0-9a-f-]+ bullet_list \(.+\): - a point inside the item'
[[ "$depth" == "$item_depth" ]] ||
  fail two "the list stands inside the inner item" \
    "the list stands at depth $depth and the item's own text at $item_depth"
pass two "the list stands inside the inner item, beside the item's own text at depth $depth"
at_depth two "the list the edit wrote holds its item" $((item_depth + 1)) \
  '[0-9a-f-]+ paragraph \(.+\): a point inside the item'

# --- three: the three refusals, each leaving the document as it was ------------------------------

version_of three "read the version the refusals must leave"
held="$version"
readonly held

refused_edit three "a list written over a heading's text is refused" \
  "$(jq -cn --arg f "A heading that holds one line of text" \
    --arg w "$(printf 'A heading the check rewrites\n\n- a point under the heading')" \
    '[{op: "replace", find: $f, with: $w}]')" \
  "the match is in a heading, which holds one line of text and no block of its own"
unchanged three "the heading refusal left the document at version $held" "$held"

refused_edit three "a list written over a table cell's text is refused" \
  "$(jq -cn --arg f "A cell that holds one paragraph." \
    --arg w "$(printf 'A cell the check rewrites\n\n- a point in the cell')" \
    '[{op: "replace", find: $f, with: $w}]')" \
  "the match is in a table cell, which holds one paragraph of text"
unchanged three "the table cell refusal left the document at version $held" "$held"

refused_edit three "a with opening with the matched item's own marker is refused" \
  "$(jq -cn --arg f "An outer item." \
    --arg w "$(printf -- '- the outer item rewritten\n- a point beside it')" \
    '[{op: "replace", find: $f, with: $w}]')" \
  "with begins with a marker of the same kind as the matched block's own"
unchanged three "the repeated marker refusal left the document at version $held" "$held"

# --- four: what each doc-edit answered -----------------------------------------------------------

written four "the first edit answered with the paragraph and the list it wrote" \
  "$work/edit-one" "$one_with"
written four "the second edit answered with the list it wrote the item into" \
  "$work/edit-two" "$(printf -- '- An outer item.\n\n  - An inner item.\n    - a point inside the item')"

# A run that wrote nothing cannot read this: the upload of the spec is version 1, and each of the
# two edits above wrote one version more.
version_of four "read the version the run's edits left"
[[ "$version" == 3 ]] ||
  fail four "the run wrote two versions of the document" \
    "the document stands at version $version; the spec's upload and the run's two edits make 3"
pass four "the document stands at version 3: its upload and the two edits the run applied"
