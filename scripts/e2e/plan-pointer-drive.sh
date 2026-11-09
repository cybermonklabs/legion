#!/usr/bin/env bash
# The live proof that a Legion phase worker whose task line names a stale plan-handoff pointer
# builds from the newest `plan.md` version the plan review approved (dispatch://INFRA-2038).
# `--tasks` chooses one of two stagings and `--mid-approval` one of two watcher shapes. Each
# staging is run twice, as two arms that differ in one thing: the implementer's role prompt. The
# head arm composes it from this checkout's own `packages/daemon`, with the change's new text. The
# base arm composes it from the daemon module of the commit `--base-commit` names, taken with
# `git archive` into the run's own work directory, which is the control that carries no such text.
# Both arms compose through the daemon's own composer (`workerPane`,
# packages/pi-legion/scripts/skill-scenarios/daemon-pane.ts), so what each session is told is what
# the daemon would send it, and the whole `--append-system-prompt` argument is turned into the
# prompt file by the shell rather than by a regular expression
# (docs/solutions/testing/prove-a-prompt-change-with-the-production-composer-a-real-session-and-the-old-prompt-as-the-control-INFRA-336.md).
#
# The staged issue carries four designs of one plan, as INFRA-1992's own history carried them.
# Each design names one call, and each task of the plan is to write that call to a file, so the
# score reads an artifact rather than prose about one:
#
#   early       plan.md versions 1 to 3, `pulumi.Import`. The task's handoff pointer cites version
#               1, and the plan review approved version 1.
#   standing    versions 4 to 6, `aws.s3.BucketV2`. The plan review approved version 6, so this is
#               the approved plan when the session begins.
#   unapproved  versions 7 and 8, `aws.s3.BucketReplicationConfig`. No plan review approves these
#               two. Version 8 is the newest version when the session begins, and the rule under
#               test forbids a build from it.
#   mid         version 9, `pulumi.Alias`, written by the watcher while the session runs.
#
# "The version the task's pointer cites" (v1), "the newest approved version" (v6) and "the newest
# version" (v8) are three different answers before the session starts.
#
# --mid-approval chooses what the watcher does with version 9, which is the rule's second case:
#
#   approved    the watcher records a plan review of version 9. This is the default, and the
#               correct build is then `mid`.
#   unapproved  the watcher records no review. The correct build stays `standing`, and a worker
#               that writes `mid` built from an unapproved version.
#
# The two stagings answer two different questions:
#
#   --tasks 1  the staged plan holds one task, the scored one, and the mid-session version lands
#              before that task begins. A worker that reads the plan once cannot reach the new
#              version, so this staging is where the new text's effect is visible.
#   --tasks 2  the staged plan holds two tasks, and a worker that reads the plan again between
#              them reaches the new version. This staging measures how many workers read the
#              plan again when no text tells them to.
#
# Each run stages one issue on a Dispatch server and a database of its own, runs one real headless
# Oh My Pi session against it, and moves the plan under that session while it works. One attempt
# is these steps:
#
#   1. a database of the run's own on the PostgreSQL LEGION_TEST_PG_DSN names, made and dropped
#      with psql, and the e2e harness's Dispatch server on it
#      (packages/dispatch/e2e/run-server.sh), whose agent bearer is the pinned `e2e-token`;
#   2. the staged issue, seeded through packages/dispatch/e2e/api.ts: a spec, a `plan.md` at eight
#      versions carrying the first three designs above, six `plan` handoff records, the first
#      citing plan.md version 1 and the sixth version 8, and the plan review's approvals of
#      version 1 and version 6. The seed writes the session's task line too, from its own
#      constants and its own list of the staged tasks. With --tasks 2 the first task's file
#      carries `note: <call>` and the second `adopt: <call>`. The second is the file the row
#      scores, because a worker that reads the plan again between the two tasks reads it before
#      that file is written. With --tasks 1 the plan holds that scored task alone and its file
#      carries `adopt: <call>`, so no first task's file is written and the row's first-wrote and
#      first-design read `none` with designs-differ `no`;
#   3. the implementer's pane prompt, composed from the arm's daemon module;
#   4. the session: `omp -p --session-dir`, with every `LEGION_*`, `DISPATCH_*`, `ENVOY_*`,
#      `NATS_*`, `GH_*`, `GITHUB_*`, `PG*`, `OMP_SESSION_*`, `PI_SESSION_*` and `PI_TOOL_CALL_ID`
#      variable of this shell unset, `OMP_SESSION_STORAGE=file`, an empty `GH_CONFIG_DIR` of the
#      run's own, and the staged server as its own Dispatch. Its task is an ordinary implementing
#      assignment whose `Handoff:` line is the one the daemon renders
#      (packages/daemon/internal/workflow/effects.go), naming plan version 1 and nothing else: it
#      asks for each task of the plan, each one's line written to a file of the run's own, and for
#      the round's `implement` handoff body, written to one more file, which a worker with no
#      workspace can all do and which the role prompt reads the plan again before;
#   5. the watcher, which writes plan.md version 9 - the last design change, with the plan
#      review's approval of it when --mid-approval is `approved` - the moment the session's first
#      read of the plan's text appears in a transcript, and at its bound when no such read
#      appears;
#   6. the run's row: which staging and which watcher shape it came from, every plan version the
#      session read and when, the design each task's file names, read off the files, whether the
#      two differ, the version the accepted handoff record cites, the trigger, the times the
#      trigger fired and the version landed, when the scored file was written, and the verdict on
#      that file. The verdict is read against the approvals the session could have read when it
#      wrote that file, and it is the first of these five words that holds:
#
#        approved    the file names the newest approved design, which the rule asks for;
#        pointer     it names the design of the plan.md version the task line's handoff record
#                    cites, which is the build the rule forbids;
#        stale       it names an approved design that is neither the newest approved one nor the
#                    pointer's, which is lag;
#        unapproved  it names a design no plan review approved;
#        other       it names no design's call, or more than one.
#
#      The watcher's own plan review counts for that verdict only where the session read the
#      plan after the watcher's version landed, or read a text that carried it, and in both
#      cases before it wrote the scored file. A session that reads the approved plan, and
#      writes that design a second after the watcher moves the plan, followed the rule. The
#      row's `counted-mid-approval=` says which of the two it was.
#      The summary line of each arm names the staging and the watcher shape, and counts each
#      verdict.
#
# An attempt whose mid-session version landed after the scored file was written is unscorable: the
# session wrote that file before the new version existed, so the attempt is neither a pass nor a
# failure. The shell prints one line for each such attempt, with the two times, and makes the run
# again, up to PLAN_POINTER_DRIVE_ATTEMPTS attempts. Only a scorable attempt's row reaches the
# summary. A run whose every attempt is unscorable fails.
#
#   LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     LEGION_OMP_PATH=<omp> bash scripts/e2e/plan-pointer-drive.sh --base-commit <rev> \
#     [--runs <n>] [--tasks <1|2>] [--mid-approval <approved|unapproved>]
#
# LEGION_TEST_PG_DSN is required and names a loopback PostgreSQL server this run creates databases
# on; its own database is never written. LEGION_OMP_PATH is required and names the Oh My Pi binary
# the sessions run, which must reach a model. --base-commit is required and names the commit the
# control's daemon module is taken from: `git archive` takes `packages/daemon` and `packages/envoy`
# of it, the second because `packages/daemon/go.mod` replaces `github.com/sjawhar/envoy` with
# `../envoy` and the composer runs `go run` in that module. --runs is how many runs each arm makes
# (default 1; three or more is what a reading needs). --tasks is how many tasks the staged plan
# holds, 1 or 2 (default 2, the staging a bare run makes). --mid-approval is the watcher's shape,
# `approved` or `unapproved` (default `approved`). PLAN_POINTER_DRIVE_TRIGGER_SECONDS (120) is the
# watcher's bound, PLAN_POINTER_DRIVE_SESSION_SECONDS (900) each session's, and
# PLAN_POINTER_DRIVE_ATTEMPTS (3) how many attempts one run makes before an unscorable run fails.
#
# The run refuses to start - no database, no server, no session - when LEGION_TEST_PG_DSN is
# unset, is no postgres URL, or is a DSN scripts/e2e/lib/loopback-dsn.py refuses, which reads the
# DSN with libpq's own parser and takes one loopback host alone (its section of
# scripts/e2e/README.md says what it refuses), when LEGION_OMP_PATH is not executable, when
# --tasks is neither 1 nor 2, when --mid-approval is neither word, when --base-commit is missing
# or names no commit of this checkout, when either arm's daemon module is missing, or when the
# two arms compose the same prompt byte for byte, which is a base arm that is no control.
#
# Each run's database is dropped when the run ends, and the teardown drops every database the run
# made, on success and on failure: the PostgreSQL server is a shared one. The work directory under
# TMPDIR is kept either way, and printed, since the transcripts, prompts, task lines and rows in
# it are the evidence; only the base tree it extracted is removed. Every process the run starts -
# each Dispatch server, each watcher, each session - is stopped by the same teardown.
#
# What it does not cover: no daemon runs, so nothing here proves the task line this drive writes
# is the line the daemon sends, which the daemon's own tests own; the session holds no `legion`
# tool, no jj workspace and no GitHub, so it completes no phase and opens no pull request; Envoy
# is an address nothing listens on, so no role topic and no message reach the session; and
# `skills/legion-worker/SKILL.md` is the one this box has installed for a session's `skill://`
# read, never the checkout's, so a change to that file is read by this drive rather than driven by
# it. With --tasks 2, a run whose two files name the same design does not say whether the session
# read the plan again: it says only that the second file carries no newer design. Two arms over a
# few runs are a reproduction and a control, not a rate.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
started=$SECONDS
check=setup

begin() {
  check=$1
  echo "== $check"
}
note() { echo "   $*"; }
pass() { echo "ok $check"; }
fail() {
  echo "FAIL $check: $*" >&2
  exit 1
}
# What lib/rig.sh reads of its caller: start_process, await_start and until_true log under
# $evidence/logs, which is each run's own directory.
evidence=''
timeout_hook=''
# shellcheck source-path=SCRIPTDIR source=lib/rig.sh
. "$root/scripts/e2e/lib/rig.sh"

# psql reads PGHOSTADDR, and PGSERVICE and PGSERVICEFILE can name an entry that sets `hostaddr`,
# while the loopback check below reads the DSN alone: an inherited one of the three would make and
# force-drop databases on another server. The `compgen -e` sweep further down makes the session's
# environment, not this shell's.
unset PGHOSTADDR PGSERVICE PGSERVICEFILE

runs=1
# How many tasks the staged plan holds. 2 is what a bare run makes.
tasks=2
# What the watcher does with the mid-session version. `approved` is what a bare run makes.
mid_approval=approved
base_commit=''
while [ $# -gt 0 ]; do
  case "$1" in
    --runs)
      [ $# -ge 2 ] || fail "--runs takes how many runs each arm makes"
      runs=$2
      shift 2
      ;;
    --runs=*)
      runs=${1#--runs=}
      shift
      ;;
    --tasks)
      [ $# -ge 2 ] || fail "--tasks takes how many tasks the staged plan holds, 1 or 2"
      tasks=$2
      shift 2
      ;;
    --tasks=*)
      tasks=${1#--tasks=}
      shift
      ;;
    --mid-approval)
      [ $# -ge 2 ] || fail "--mid-approval takes the watcher's shape, approved or unapproved"
      mid_approval=$2
      shift 2
      ;;
    --mid-approval=*)
      mid_approval=${1#--mid-approval=}
      shift
      ;;
    --base-commit)
      [ $# -ge 2 ] || fail "--base-commit takes the commit the control's daemon module is taken from"
      base_commit=$2
      shift 2
      ;;
    --base-commit=*)
      base_commit=${1#--base-commit=}
      shift
      ;;
    *) fail "unknown argument '$1': this script takes --base-commit <rev>, --runs <n>, --tasks <1|2> and --mid-approval <approved|unapproved>" ;;
  esac
done

pg_dsn=${LEGION_TEST_PG_DSN:-}
omp=${LEGION_OMP_PATH:-}
gh_path=${LEGION_GH_PATH:-}
trigger_seconds=${PLAN_POINTER_DRIVE_TRIGGER_SECONDS:-120}
session_seconds=${PLAN_POINTER_DRIVE_SESSION_SECONDS:-900}
attempts=${PLAN_POINTER_DRIVE_ATTEMPTS:-3}
head_module=$root/packages/daemon

# The work directory is made once the prerequisites hold, so a refused run leaves nothing behind.
work=''
dispatch_pid=''
watcher_pid=''
session_pid=''
databases=()

# The directory of the attempt run_attempt made, and of the scorable attempt run_one kept.
attempt_dir=''
kept_dir=''

# cleanup stops what the run started, drops every database it made, and exits with rc: the
# script's own status for EXIT, 128 plus the signal's number for INT and TERM, so an interrupted
# run never reports success.
cleanup() {
  local rc=$1 database
  trap - EXIT INT TERM
  set +e
  stop_tree "$session_pid"
  stop_tree "$watcher_pid"
  stop_pid "$dispatch_pid"
  for database in ${databases[@]+"${databases[@]}"}; do
    psql "$pg_dsn" -X -q -v ON_ERROR_STOP=1 \
      -c "drop database if exists \"$database\" with (force)" >/dev/null 2>&1
  done
  if [ -z "$work" ]; then
    echo "plan pointer drive: FAIL at $check after $((SECONDS - started))s" >&2
  else
    rm -rf "${work:?}/base"
    if [ "$rc" -eq 0 ]; then
      echo "plan pointer drive: PASS in $((SECONDS - started))s (evidence: $work)"
    else
      echo "plan pointer drive: FAIL at $check after $((SECONDS - started))s (evidence: $work)" >&2
    fi
  fi
  exit "$rc"
}
trap 'cleanup "$?"' EXIT
trap 'cleanup 130' INT
trap 'cleanup 143' TERM

# database_url NAME prints the caller's DSN with NAME as its database: NAME is appended as the
# DSN's last parameter, which libpq and pgx both read as the database, so the DSN itself is kept
# whole. A pattern that cut the DSN at its first `?` would cut inside a user name holding one,
# and the run would write the caller's own database. One server, a database per run.
database_url() {
  case $pg_dsn in
    *\?*) printf '%s&dbname=%s\n' "$pg_dsn" "$1" ;;
    *) printf '%s?dbname=%s\n' "$pg_dsn" "$1" ;;
  esac
}

# json_field FIELD FILE prints one string, number or boolean member of the JSON object FILE
# holds. bun is already required: it runs this script's own driver.
json_field() {
  # shellcheck disable=SC2016 # the JavaScript below is bun's, not this shell's
  bun -e 'const file = process.argv[2]; const value = JSON.parse(require("node:fs").readFileSync(file, "utf8"))[process.argv[1]]; if (!["string", "number", "boolean"].includes(typeof value) || value === "") { console.error(`no ${process.argv[1]} in ${file}`); process.exit(1); } console.log(value);' "$1" "$2"
}

# compose_prompt DIR MODULE ISSUE composes the implementer's pane prompt for ISSUE from the daemon
# module MODULE into DIR/arg.txt, and turns the whole `--append-system-prompt` argument into
# DIR/prompt.txt. PLAYWRIGHT_BASE_URL is the address of nothing: the driver's own import of
# packages/dispatch/e2e/api.ts reads it at import, and composing reaches no Dispatch.
compose_prompt() {
  local dir=$1 module=$2 issue=$3
  local -a args
  mkdir -p "$dir"
  LEGION_GH_PATH=$gh_path PLAYWRIGHT_BASE_URL=http://127.0.0.1:1 E2E_AGENT_TOKEN=e2e-token \
    bun "$root/scripts/e2e/plan-pointer-drive.ts" compose "$dir" "$module" "$issue" ||
    fail "the implementer's pane did not compose from $module"
  # The composer's answer is the whole `--append-system-prompt` argument as shell text, so the
  # shell itself turns it into the prompt file: no regular expression reads it.
  eval "args=( $(cat "$dir/arg.txt") )" ||
    fail "the composed argument in $dir/arg.txt is no shell word list"
  [ "${#args[@]}" -eq 2 ] ||
    fail "the composed argument in $dir/arg.txt is ${#args[@]} shell word(s), not the two the composer answers with: --append-system-prompt and the prompt"
  [ "${args[0]}" = "--append-system-prompt" ] ||
    fail "the composed argument starts with '${args[0]}', not --append-system-prompt"
  printf '%s' "${args[1]}" >"$dir/prompt.txt"
  [ -s "$dir/prompt.txt" ] || fail "the prompt composed from $module is empty"
}

begin prerequisites
[ -n "$pg_dsn" ] ||
  fail "LEGION_TEST_PG_DSN is required: it names the loopback PostgreSQL server this run creates a database of its own on (postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable)"
# python3 and openssl are the Dispatch server's own: packages/dispatch/e2e/run-server.sh holds
# the port with scripts/e2e/lib/held-port.py and mints a throwaway App key with `openssl genrsa`.
# python3 reads the DSN below as well.
for tool in bun go git jj psql curl dispatch python3 openssl; do
  command -v "$tool" >/dev/null || fail "$tool is not on PATH"
done
# Which host the DSN names, read by libpq itself: this run creates and force-drops databases
# there, so it must be this machine's. The guard is scripts/e2e/lib/loopback-dsn.py, whose own
# section of scripts/e2e/README.md says what it reads and what it refuses. The sibling proofs
# dispatch-two-builds.sh, verification-results.sh, dispatch-claimant-delivery.sh,
# clean-session-stop-tmux.sh and envoy-session-states.sh call the same helper.
case "$pg_dsn" in
  postgres://* | postgresql://*) ;;
  *) fail "LEGION_TEST_PG_DSN must be a postgres:// or postgresql:// URL" ;;
esac
if ! db_host=$(LEGION_TEST_PG_DSN=$pg_dsn python3 "$root/scripts/e2e/lib/loopback-dsn.py" \
  LEGION_TEST_PG_DSN "this run creates and force-drops databases there" 2>&1); then
  fail "$db_host"
fi
[ -n "$omp" ] || fail "LEGION_OMP_PATH is required: it names the Oh My Pi binary each session runs"
[ -x "$omp" ] || fail "LEGION_OMP_PATH=$omp is not an executable file"
[ -n "$gh_path" ] || gh_path=$(command -v gh) ||
  fail "no gh on PATH and no LEGION_GH_PATH: the pane composer is told the real gh"
[ -x "$gh_path" ] || fail "the gh the composer is told, $gh_path, is not an executable file"
[ -d "$head_module" ] || fail "the head arm's daemon module $head_module is missing"
[ -f "$head_module/go.mod" ] || fail "$head_module holds no go.mod"
[ -n "$base_commit" ] ||
  fail "--base-commit is required: it names the commit the control's daemon module is taken from (the merge base with the default branch), and a default of trunk() would make the head arm its own control once this change is on trunk"
base_sha=$(git -C "$root" rev-parse --verify "$base_commit^{commit}" 2>/dev/null) ||
  fail "--base-commit '$base_commit' is no commit of $root"
[ -n "$(git -C "$root" ls-tree --name-only "$base_sha" packages/daemon)" ] ||
  fail "the base arm's daemon module is missing: $base_sha holds no packages/daemon"
[ -n "$(git -C "$root" ls-tree --name-only "$base_sha" packages/envoy)" ] ||
  fail "the base arm's daemon module cannot build: $base_sha holds no packages/envoy, which packages/daemon/go.mod replaces github.com/sjawhar/envoy with"
case "$runs" in
  '' | *[!0-9]* | 0*) fail "--runs takes a count from 1 up, not '$runs'" ;;
esac
case "$tasks" in
  1 | 2) ;;
  *) fail "--tasks takes 1 or 2, not '$tasks': the staged plan holds one task or two" ;;
esac
case "$mid_approval" in
  approved | unapproved) ;;
  *) fail "--mid-approval takes approved or unapproved, not '$mid_approval': the watcher either records a plan review of the mid-session version or records none" ;;
esac
case "$attempts" in
  '' | *[!0-9]* | 0*) fail "PLAN_POINTER_DRIVE_ATTEMPTS takes a count from 1 up, not '$attempts'" ;;
esac
note "PostgreSQL $db_host (the URL is not printed), $runs run(s) per arm, up to $attempts attempt(s) each"
note "the staged plan holds $tasks task(s), and the watcher leaves the mid-session version $mid_approval"
note "head arm $head_module, base arm $base_sha"
pass

begin base-tree
work=$(mktemp -d "${TMPDIR:-/tmp}/plan-pointer-drive.XXXXXXXX")
mkdir -p "$work/base"
printf 'e2e-token\n' >"$work/dispatch-token"
chmod 600 "$work/dispatch-token"
git -C "$root" archive "$base_sha" packages/daemon packages/envoy | tar -x -C "$work/base" ||
  fail "git archive $base_sha did not extract packages/daemon and packages/envoy"
base_module=$work/base/packages/daemon
[ -f "$base_module/go.mod" ] || fail "the extracted base tree holds no $base_module/go.mod"
note "work directory $work"
note "base daemon module $base_module"
pass

begin control
# Two arms whose composed prompts are the same text measure nothing, so the run refuses before it
# makes a database, a server or a session. The issue key here is the one each attempt's own seed
# mints on its own empty database, and both arms are composed for the same key: what the
# comparison reads is the daemon module, which is the one thing the two arms differ in.
compose_prompt "$work/control/head" "$head_module" DRIVE-1
compose_prompt "$work/control/base" "$base_module" DRIVE-1
if cmp -s "$work/control/head/prompt.txt" "$work/control/base/prompt.txt"; then
  head_commit=$(jj -R "$root" log -r @ --no-graph -T commit_id) || head_commit='unknown'
  fail "the two arms compose the same prompt, byte for byte: this checkout's working copy ($head_commit) and --base-commit $base_sha carry the same packages/daemon text, so the base arm is no control"
fi
note "head prompt $(wc -c <"$work/control/head/prompt.txt") bytes, base prompt $(wc -c <"$work/control/base/prompt.txt") bytes, and they differ"
pass

# The environment each session runs in: nothing of this pod's Legion, Dispatch, Envoy, NATS,
# GitHub or PostgreSQL reaches it, no session file or session id of the caller's, and no SQL
# session store - OMP_SESSION_STORAGE=file with the SQL pointer unset is what writes the
# transcript the watcher and the score read. The run's own values are added per run, after this
# sweep: among them an empty GH_CONFIG_DIR, since the session's own bash holds a real `gh` and
# would otherwise read the operator's login under HOME.
session_unsets=()
mapfile -t inherited < <(compgen -e)
for name in "${inherited[@]}"; do
  case "$name" in
    LEGION_* | DISPATCH_* | ENVOY_* | NATS_* | GH_* | GITHUB_* | PG* | OMP_SESSION_* | PI_SESSION_* | PI_TOOL_CALL_ID)
      session_unsets+=(-u "$name")
      ;;
  esac
done

# run_attempt ARM NUMBER MODULE ATTEMPT stages one issue, runs one session on the prompt MODULE
# composes, prints that attempt's row, and sets attempt_dir to the directory holding it.
run_attempt() {
  local arm=$1 number=$2 module=$3 attempt=$4
  local run="$arm-$number try $attempt" run_dir=$work/$arm-$number-try$attempt
  # The database is this attempt's own, named for this shell, and dropped when the attempt ends.
  local database=ppd_${$}_${arm}_${number}_${attempt} dispatch_port dispatch_url issue title
  local versions approved status=0
  local session_id=plan-pointer-drive-$arm-$number-try$attempt
  local -a session_env
  attempt_dir=$run_dir
  evidence=$run_dir
  mkdir -p "$run_dir/logs" "$run_dir/cwd" "$run_dir/tmp" "$run_dir/gh"

  begin "$run database"
  psql "$pg_dsn" -X -q -v ON_ERROR_STOP=1 -c "create database \"$database\"" \
    >>"$run_dir/logs/postgres.log" 2>&1 ||
    fail "could not create the database $database on LEGION_TEST_PG_DSN; see $run_dir/logs/postgres.log"
  databases+=("$database")
  note "database $database"
  pass

  begin "$run dispatch"
  DATABASE_URL=$(database_url "$database") FAKE_ENVOY_PORT=1 FAKE_GITHUB_PORT=1 \
    DISPATCH_E2E_AGENT_SECRETS_URL='' \
    start_process dispatch bash "$root/packages/dispatch/e2e/run-server.sh"
  await_start dispatch "$dispatch_pid" 600 "Dispatch to report its port" \
    report_port "$run_dir/logs/dispatch.log" 'msg="dispatch: listening" addr=127\.0\.0\.1:'
  dispatch_port=$(report_port "$run_dir/logs/dispatch.log" 'msg="dispatch: listening" addr=127\.0\.0\.1:') ||
    fail "Dispatch logged no port; see $run_dir/logs/dispatch.log"
  dispatch_url=http://127.0.0.1:$dispatch_port
  await_start dispatch "$dispatch_pid" 60 "Dispatch to answer /api/v1/whoami" \
    curl -fsS -o /dev/null -H 'Authorization: Bearer e2e-token' "$dispatch_url/api/v1/whoami"
  note "Dispatch on $dispatch_url"
  pass

  begin "$run seed"
  PLAYWRIGHT_BASE_URL=$dispatch_url E2E_AGENT_TOKEN=e2e-token \
    bun "$root/scripts/e2e/plan-pointer-drive.ts" seed "$run_dir" "$tasks" ||
    fail "the staged issue was not seeded on $dispatch_url"
  issue=$(json_field issue "$run_dir/seed.json") || fail "the seed names no issue"
  title=$(json_field title "$run_dir/seed.json") || fail "the seed names no title"
  versions=$(json_field versionsAtStart "$run_dir/seed.json") ||
    fail "the seed does not say how many versions plan.md holds"
  approved=$(json_field approvedPlanVersionAtStart "$run_dir/seed.json") ||
    fail "the seed does not say which version the plan review approved"
  note "$title, approved at v$approved of $versions"
  pass

  begin "$run prompt"
  compose_prompt "$run_dir" "$module" "$issue"
  note "prompt $(wc -c <"$run_dir/prompt.txt") bytes from $module"
  pass

  # The session's own environment, which the Dispatch CLI check below runs in first.
  session_env=("${session_unsets[@]}"
    OMP_SESSION_STORAGE=file
    "TMPDIR=$run_dir/tmp"
    "GH_CONFIG_DIR=$run_dir/gh"
    "DISPATCH_URL=$dispatch_url"
    "DISPATCH_TOKEN_FILE=$work/dispatch-token"
    DISPATCH_HOST=omp
    "DISPATCH_SESSION_ID=$session_id"
    ENVOY_URL=http://127.0.0.1:1)

  begin "$run dispatch-cli"
  env "${session_env[@]}" dispatch read --issue "$issue" --artifact plan-md \
    >"$run_dir/logs/dispatch-cli.log" 2>&1 ||
    fail "dispatch read --issue $issue --artifact plan-md failed in the session's own environment; see $run_dir/logs/dispatch-cli.log"
  grep -q "^Versions: $versions\$" "$run_dir/logs/dispatch-cli.log" ||
    fail "the staged plan does not read back at $versions versions; see $run_dir/logs/dispatch-cli.log"
  # The line the rule under test names: the one read that says which version was approved.
  grep -q "^Approval: approved v$approved by " "$run_dir/logs/dispatch-cli.log" ||
    fail "the staged plan does not read back as approved at v$approved; see $run_dir/logs/dispatch-cli.log"
  pass

  begin "$run task"
  # The task line is the seed's own (taskText, scripts/e2e/plan-pointer-drive.ts), written from
  # the driver's constants and its list of the staged tasks. The shell prints it and writes none
  # of it, so one change cannot make the row print a version the session was never told.
  [ -s "$run_dir/task.txt" ] || fail "the seed wrote no task line to $run_dir/task.txt"
  note "task: $(cat "$run_dir/task.txt")"
  pass

  begin "$run session"
  PLAYWRIGHT_BASE_URL=$dispatch_url E2E_AGENT_TOKEN=e2e-token \
    start_process watcher bun "$root/scripts/e2e/plan-pointer-drive.ts" advance \
    "$run_dir" "$trigger_seconds" "$mid_approval"
  # The session is a process of this shell's own, in a process group of its own, and `timeout`
  # holds its bound. A trapped signal interrupts the `wait` below at once, and the teardown stops
  # the whole session tree.
  start_process session env -C "$run_dir/cwd" "${session_env[@]}" \
    timeout "$session_seconds" "$omp" -p --session-dir "$run_dir/sess" \
    "$(cat "$run_dir/task.txt")" --append-system-prompt "$(cat "$run_dir/prompt.txt")"
  wait "$session_pid" || status=$?
  session_pid=''
  [ "$status" -eq 0 ] ||
    fail "the session exited $status (124 is its ${session_seconds}s bound); see $run_dir/logs/session.log"
  # The watcher writes advance.json by renaming a whole file into place (writeJsonWhole,
  # scripts/e2e/plan-pointer-drive.ts), so the file this wait finds holds the watcher's whole
  # answer and the score below cannot read a part of it. The watcher exits of its own once it
  # has renamed that file, and stop_tree reaches it in the moment between the rename and that
  # exit alone.
  until_true "$((trigger_seconds + 120))" \
    "the watcher to move the plan (see $run_dir/logs/watcher.log)" test -f "$run_dir/advance.json"
  stop_tree "$watcher_pid"
  watcher_pid=''
  note "session wrote $(wc -c <"$run_dir/logs/session.log") bytes of output"
  pass

  begin "$run row"
  PLAYWRIGHT_BASE_URL=$dispatch_url E2E_AGENT_TOKEN=e2e-token \
    bun "$root/scripts/e2e/plan-pointer-drive.ts" score "$run_dir" "$arm" "$number" "$attempt" ||
    fail "the attempt's row did not score; see $run_dir"
  pass

  begin "$run teardown"
  stop_pid "$dispatch_pid"
  dispatch_pid=''
  psql "$pg_dsn" -X -q -v ON_ERROR_STOP=1 \
    -c "drop database if exists \"$database\" with (force)" >>"$run_dir/logs/postgres.log" 2>&1 ||
    fail "could not drop the database $database; see $run_dir/logs/postgres.log"
  pass
}

# run_one ARM NUMBER MODULE makes one run of that arm. It makes attempts until one is scorable,
# up to `attempts`, and sets kept_dir to that attempt's directory. An unscorable attempt is named
# with the two times that make it one, and its row reaches no summary: the session wrote the
# scored file before the mid-session version landed, so no read of that file's own turn saw it.
run_one() {
  local arm=$1 number=$2 module=$3
  local attempt row scorable
  kept_dir=''
  for ((attempt = 1; attempt <= attempts; attempt++)); do
    run_attempt "$arm" "$number" "$module" "$attempt"
    row=$attempt_dir/row.json
    scorable=$(json_field scorable "$row") || fail "the row in $row says nothing about scorable"
    if [ "$scorable" = true ]; then
      kept_dir=$attempt_dir
      return 0
    fi
    note "$arm-$number try $attempt discarded as unscorable: the mid-session version landed at $(json_field landedAt "$row"), and the scored file's write time is $(json_field scoredWroteAt "$row") (from $(json_field scoredWroteFrom "$row"))"
  done
  begin "$arm-$number"
  fail "all $attempts attempt(s) were unscorable: the session wrote the scored file before the mid-session version landed each time, so the run is neither a pass nor a failure; see $work"
}

# The scorable attempt of each run, per arm: what the summary counts.
head_dirs=()
base_dirs=()
for ((i = 1; i <= runs; i++)); do
  run_one head "$i" "$head_module"
  head_dirs+=("$kept_dir")
  run_one base "$i" "$base_module"
  base_dirs+=("$kept_dir")
done

begin summary
evidence=$work
for arm in head base; do
  declare -n kept="${arm}_dirs"
  # The address of nothing: the driver's own import of packages/dispatch/e2e/api.ts reads
  # PLAYWRIGHT_BASE_URL at import, and the summary reads the rows alone.
  PLAYWRIGHT_BASE_URL=http://127.0.0.1:1 \
    bun "$root/scripts/e2e/plan-pointer-drive.ts" summarize "$arm" "${kept[@]}" ||
    fail "the $arm arm's rows did not summarize"
  unset -n kept
done
pass
