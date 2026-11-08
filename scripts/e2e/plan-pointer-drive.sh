#!/usr/bin/env bash
# The live proof that a Legion phase worker whose task line names a stale plan-handoff pointer
# builds from the issue's `plan.md` as it stands (dispatch://INFRA-2038). One staged case is run
# twice, as two arms that differ in one thing: the implementer's role prompt. The head arm
# composes it from this checkout's own `packages/daemon`, with the change's new text; the base arm
# composes it from the daemon module of the commit `--base-commit` names, taken with `git archive`
# into the run's own work directory, which is the control that carries no such text. Both arms
# compose through the daemon's own composer (`workerPane`,
# packages/pi-legion/scripts/skill-scenarios/daemon-pane.ts), so what each session is told is what
# the daemon would send it, and the whole `--append-system-prompt` argument is turned into the
# prompt file by the shell rather than by a regular expression
# (docs/solutions/testing/prove-a-prompt-change-with-the-production-composer-a-real-session-and-the-old-prompt-as-the-control-INFRA-336.md).
#
# Each run stages one issue on a Dispatch server and a database of its own, runs one real headless
# Oh My Pi session against it, and moves the plan under that session while it works. One attempt
# is these steps:
#
#   1. a database of the run's own on the PostgreSQL LEGION_TEST_PG_DSN names, and the e2e
#      harness's Dispatch server on it (packages/dispatch/e2e/run-server.sh), whose agent bearer
#      is the pinned `e2e-token`;
#   2. the staged issue, seeded through packages/dispatch/e2e/api.ts: a spec, a `plan.md` at eight
#      versions whose design changes at version 4, six `plan` handoff records, the first citing
#      plan.md version 1 and the sixth version 8, and the plan review's approval at version 1, so
#      "approved" (v1), "the version the task's pointer cites" (v1) and "newest" (v8) are
#      different answers. Each design names one call - `pulumi.Import`, `aws.s3.BucketV2`, and
#      `pulumi.Alias` at the version written mid-session - and each of the plan's two tasks is to
#      write that call to a file, so the score reads an artifact rather than prose about one. The
#      first task's file carries `note: <call>` and the second `adopt: <call>`. The second is the
#      file the row scores, because a worker that reads the plan again between the two tasks
#      reads it before that file is written;
#   3. the implementer's pane prompt, composed from the arm's daemon module;
#   4. the session: `omp -p --session-dir`, with every `LEGION_*`, `DISPATCH_*`, `ENVOY_*`,
#      `NATS_*`, `GH_*`, `GITHUB_*`, `OMP_SESSION_*` and `PI_SESSION_*` variable of this shell
#      unset, `OMP_SESSION_STORAGE=file`, and the staged server as its own Dispatch. Its task is
#      an ordinary implementing assignment whose `Handoff:` line is the one the daemon renders
#      (packages/daemon/internal/workflow/effects.go), naming plan version 1 and nothing else: it
#      asks for the plan's two tasks, each one's line written to a file of the run's own, and for
#      the round's `implement` handoff body, written to a third, which a worker with no workspace
#      can all do and which the role prompt reads the plan again before;
#   5. the watcher, which writes plan.md version 9 - the second design change, with the plan
#      review's approval of it - the moment the session's first read of the plan's text appears in
#      the transcript, and at its bound when no such read appears;
#   6. the run's row: every plan version the session read and when, the design each of the two
#      files names, read off the files, whether the two differ, the task line the session was
#      given, the version the accepted handoff record cites, the trigger, and the times the
#      trigger fired and the version landed.
#
# An attempt whose mid-session version landed after the session stopped writing is unscorable: no
# turn of the session saw the new version, so the attempt is neither a pass nor a failure. The
# shell prints one line for each such attempt, with the two times, and makes the run again, up to
# PLAN_POINTER_DRIVE_ATTEMPTS attempts. Only a scorable attempt's row reaches the summary. A run
# whose every attempt is unscorable fails.
#
#   LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     LEGION_OMP_PATH=<omp> bash scripts/e2e/plan-pointer-drive.sh [--runs <n>] [--base-commit <rev>]
#
# LEGION_TEST_PG_DSN is required and names a loopback PostgreSQL server this run creates databases
# on; its own database is never written. LEGION_OMP_PATH is required and names the Oh My Pi binary
# the sessions run, which must reach a model. --runs is how many runs each arm makes (default 1;
# three or more is what a reading needs). --base-commit is the commit the control's daemon module
# is taken from (default `jj log -r 'trunk()'`); `git archive` takes `packages/daemon` and
# `packages/envoy` of it, the second because `packages/daemon/go.mod` replaces
# `github.com/sjawhar/envoy` with `../envoy` and the composer runs `go run` in that module.
# PLAN_POINTER_DRIVE_TRIGGER_SECONDS (120) is the watcher's bound,
# PLAN_POINTER_DRIVE_SESSION_SECONDS (900) each session's, and PLAN_POINTER_DRIVE_ATTEMPTS (3)
# how many attempts one run makes before an unscorable run fails. The run refuses to start - no
# database, no server, no session - when LEGION_TEST_PG_DSN is unset or names a host that is not
# loopback, when LEGION_OMP_PATH is not executable, or when either arm's daemon module is
# missing.
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
# it. A run whose two files name the same design does not say whether the session read the plan
# again: it says only that the second file carries no newer design. Two arms over a few runs are
# a reproduction and a control, not a rate.
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

runs=1
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
    --base-commit)
      [ $# -ge 2 ] || fail "--base-commit takes the commit the control's daemon module is taken from"
      base_commit=$2
      shift 2
      ;;
    --base-commit=*)
      base_commit=${1#--base-commit=}
      shift
      ;;
    *) fail "unknown argument '$1': this script takes --runs <n> and --base-commit <rev>" ;;
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
  stop_tree "$watcher_pid"
  stop_pid "$dispatch_pid"
  for database in ${databases[@]+"${databases[@]}"}; do
    dropdb -h "$db_host" -p "$db_port" -U "$db_user" --if-exists --force "$database" >/dev/null 2>&1
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

# database_url NAME prints the caller's DSN with NAME as its database, keeping its user and its
# parameters: one server, a database per run.
database_url() {
  printf '%s://%s%s/%s%s\n' "$db_scheme" "$db_userinfo" "$db_hostport" "$1" "$db_params"
}

# json_field FIELD FILE prints one string, number or boolean member of the JSON object FILE
# holds. bun is already required: it runs this script's own driver.
json_field() {
  # shellcheck disable=SC2016 # the JavaScript below is bun's, not this shell's
  bun -e 'const file = process.argv[2]; const value = JSON.parse(require("node:fs").readFileSync(file, "utf8"))[process.argv[1]]; if (!["string", "number", "boolean"].includes(typeof value) || value === "") { console.error(`no ${process.argv[1]} in ${file}`); process.exit(1); } console.log(value);' "$1" "$2"
}

begin prerequisites
[ -n "$pg_dsn" ] ||
  fail "LEGION_TEST_PG_DSN is required: it names the loopback PostgreSQL server this run creates a database of its own on (postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable)"
case "$pg_dsn" in
  postgres://* | postgresql://*) ;;
  *) fail "LEGION_TEST_PG_DSN must be a postgres:// or postgresql:// URL" ;;
esac
db_scheme=${pg_dsn%%://*}
db_rest=${pg_dsn#*://}
db_authority=${db_rest%%/*}
db_after=${db_rest#*/}
db_params=''
case "$db_after" in
  *\?*) db_params="?${db_after#*\?}" ;;
esac
db_userinfo=''
case "$db_authority" in
  *@*) db_userinfo="${db_authority%@*}@" ;;
esac
db_hostport=${db_authority##*@}
if [[ "$db_hostport" == \[* ]]; then
  db_host=${db_hostport#\[}
  db_host=${db_host%%\]*}
else
  db_host=${db_hostport%%:*}
fi
db_port=${db_hostport##*:}
[ "$db_port" != "$db_hostport" ] || db_port=5432
db_user=${db_userinfo%@}
db_user=${db_user%%:*}
[ -n "$db_user" ] || db_user=$(id -un)
case "$db_host" in
  127.0.0.1 | localhost | ::1) ;;
  *) fail "LEGION_TEST_PG_DSN must name 127.0.0.1, localhost or ::1, not '$db_host': this run creates and drops databases there" ;;
esac
[ -n "$omp" ] || fail "LEGION_OMP_PATH is required: it names the Oh My Pi binary each session runs"
[ -x "$omp" ] || fail "LEGION_OMP_PATH=$omp is not an executable file"
for tool in bun go git jj psql createdb dropdb curl dispatch; do
  command -v "$tool" >/dev/null || fail "$tool is not on PATH"
done
[ -n "$gh_path" ] || gh_path=$(command -v gh) ||
  fail "no gh on PATH and no LEGION_GH_PATH: the pane composer is told the real gh"
[ -x "$gh_path" ] || fail "the gh the composer is told, $gh_path, is not an executable file"
[ -d "$head_module" ] || fail "the head arm's daemon module $head_module is missing"
[ -f "$head_module/go.mod" ] || fail "$head_module holds no go.mod"
[ -n "$base_commit" ] || base_commit=$(jj -R "$root" log -r 'trunk()' --no-graph -T commit_id) ||
  fail "jj could not read trunk() of $root for the base commit"
base_sha=$(git -C "$root" rev-parse --verify "$base_commit^{commit}" 2>/dev/null) ||
  fail "--base-commit '$base_commit' is no commit of $root"
[ -n "$(git -C "$root" ls-tree --name-only "$base_sha" packages/daemon)" ] ||
  fail "the base arm's daemon module is missing: $base_sha holds no packages/daemon"
[ -n "$(git -C "$root" ls-tree --name-only "$base_sha" packages/envoy)" ] ||
  fail "the base arm's daemon module cannot build: $base_sha holds no packages/envoy, which packages/daemon/go.mod replaces github.com/sjawhar/envoy with"
case "$runs" in
  '' | *[!0-9]* | 0*) fail "--runs takes a count from 1 up, not '$runs'" ;;
esac
case "$attempts" in
  '' | *[!0-9]* | 0*) fail "PLAN_POINTER_DRIVE_ATTEMPTS takes a count from 1 up, not '$attempts'" ;;
esac
note "PostgreSQL $db_host:$db_port as $db_user, $runs run(s) per arm, up to $attempts attempt(s) each"
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

# The environment each session runs in: nothing of this pod's Legion, Dispatch, Envoy, NATS or
# GitHub reaches it, no session file or session id of the caller's, and no SQL session store -
# OMP_SESSION_STORAGE=file with the SQL pointer unset is what writes the transcript the watcher
# and the score read. The run's own values are added per run, after this sweep.
session_unsets=()
mapfile -t inherited < <(compgen -e)
for name in "${inherited[@]}"; do
  case "$name" in
    LEGION_* | DISPATCH_* | ENVOY_* | NATS_* | GH_* | GITHUB_* | OMP_SESSION_* | PI_SESSION_* | PI_TOOL_CALL_ID)
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
  local versions status=0
  local session_id=plan-pointer-drive-$arm-$number-try$attempt
  local -a session_env args
  attempt_dir=$run_dir
  evidence=$run_dir
  mkdir -p "$run_dir/logs" "$run_dir/cwd" "$run_dir/tmp"

  begin "$run database"
  createdb -h "$db_host" -p "$db_port" -U "$db_user" "$database" >>"$run_dir/logs/postgres.log" 2>&1 ||
    fail "createdb $database failed; see $run_dir/logs/postgres.log"
  databases+=("$database")
  note "database $database"
  pass

  begin "$run dispatch"
  : >"$run_dir/logs/dispatch.log"
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
    bun "$root/scripts/e2e/plan-pointer-drive.ts" seed "$run_dir" ||
    fail "the staged issue was not seeded on $dispatch_url"
  issue=$(json_field issue "$run_dir/seed.json") || fail "the seed names no issue"
  title=$(json_field title "$run_dir/seed.json") || fail "the seed names no title"
  versions=$(json_field versionsAtStart "$run_dir/seed.json") ||
    fail "the seed does not say how many versions plan.md holds"
  pass

  begin "$run prompt"
  LEGION_GH_PATH=$gh_path PLAYWRIGHT_BASE_URL=$dispatch_url E2E_AGENT_TOKEN=e2e-token \
    bun "$root/scripts/e2e/plan-pointer-drive.ts" compose "$run_dir" "$module" ||
    fail "the implementer's pane did not compose from $module"
  # The composer's answer is the whole `--append-system-prompt` argument as shell text, so the
  # shell itself turns it into the prompt file: no regular expression reads it.
  eval "args=( $(cat "$run_dir/arg.txt") )" ||
    fail "the composed argument in $run_dir/arg.txt is no shell word list"
  [ "${args[0]}" = "--append-system-prompt" ] ||
    fail "the composed argument starts with '${args[0]}', not --append-system-prompt"
  printf '%s' "${args[1]}" >"$run_dir/prompt.txt"
  [ -s "$run_dir/prompt.txt" ] || fail "the composed prompt is empty"
  note "prompt $(wc -c <"$run_dir/prompt.txt") bytes from $module"
  pass

  # The session's own environment, which the Dispatch CLI check below runs in first.
  session_env=("${session_unsets[@]}"
    OMP_SESSION_STORAGE=file
    "TMPDIR=$run_dir/tmp"
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
  pass

  begin "$run task"
  # The `Handoff:` line is the one the daemon renders (task, effects.go): the phase's word, the
  # version the daemon accepted, and the command that reads that version. Nothing else in the task
  # names a plan version, and nothing in it says the plan can have moved.
  printf 'Continue %s. Issue: %s. Phase: implementing. Handoff: plan version 1 (dispatch handoff-read --issue %s --phase plan --version 1). This box carries no workspace, no jj and no GitHub for this issue, so your round is the plan'"'"'s two tasks and your own record of them: write task 1'"'"'s line to %s with the write tool, then write task 2'"'"'s line to %s with the write tool, then write your `implement` handoff body to %s with the write tool. Record nothing with dispatch and open no pull request: the round ends when the three files are written.\n' \
    "$title" "$issue" "$issue" "$run_dir/note.txt" "$run_dir/adoption.txt" "$run_dir/implement.json" \
    >"$run_dir/task.txt"
  note "task: $(cat "$run_dir/task.txt")"
  pass

  begin "$run session"
  PLAYWRIGHT_BASE_URL=$dispatch_url E2E_AGENT_TOKEN=e2e-token \
    start_process watcher bun "$root/scripts/e2e/plan-pointer-drive.ts" advance "$run_dir" "$trigger_seconds"
  (
    cd "$run_dir/cwd" &&
      exec env "${session_env[@]}" timeout "$session_seconds" "$omp" -p --session-dir "$run_dir/sess" \
        "$(cat "$run_dir/task.txt")" --append-system-prompt "$(cat "$run_dir/prompt.txt")"
  ) >>"$run_dir/logs/session.log" 2>&1 || status=$?
  [ "$status" -eq 0 ] ||
    fail "the session exited $status (124 is its ${session_seconds}s bound); see $run_dir/logs/session.log"
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
  dropdb -h "$db_host" -p "$db_port" -U "$db_user" --if-exists --force "$database" ||
    fail "dropdb $database failed"
  pass
}

# run_one ARM NUMBER MODULE makes one run of that arm. It makes attempts until one is scorable,
# up to `attempts`, and sets kept_dir to that attempt's directory. An unscorable attempt is named
# with the two times that make it one, and its row reaches no summary: the mid-session version
# landed after the session stopped writing, so no turn of that session saw it.
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
    note "$arm-$number try $attempt discarded as unscorable: the mid-session version landed at $(json_field landedAt "$row"), after the session stopped writing at $(json_field sessionLastWroteAt "$row")"
  done
  begin "$arm-$number"
  fail "all $attempts attempt(s) were unscorable: the mid-session version landed after the session stopped writing each time, so the run is neither a pass nor a failure; see $work"
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
  PLAYWRIGHT_BASE_URL=http://127.0.0.1:1 \
    bun "$root/scripts/e2e/plan-pointer-drive.ts" summarize "$arm" "${kept[@]}" ||
    fail "the $arm arm's rows did not summarize"
  unset -n kept
done
pass
