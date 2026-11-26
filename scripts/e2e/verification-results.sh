#!/usr/bin/env bash
# The live proof that a recorded check result says where its commit stands against the commits and
# branches its check compares with (comparisonReader,
# packages/envoy/internal/dispatch/api/results_comparison.go), and that the head it is compared with
# moves with the pull-request events Dispatch takes off the bus
# (recordPullRequestHead, packages/envoy/internal/dispatch/delivery/pull_request_heads.go). It runs
# the real envoy-dispatch binary against a PostgreSQL the caller names, a NATS server of the run's
# own (internal/testnats/cmd/natsd), the e2e harness's fake Envoy listener
# (packages/dispatch/e2e/fake-envoy.ts) and its fake GitHub (packages/dispatch/e2e/fake-github.ts,
# which the server reads as DISPATCH_GITHUB_API_BASE), drives it over HTTP as one session and one
# person seeded through dev sign-in, and publishes each pull-request event as the normalized
# `pull_request` envelope on `notifications.github.<owner>.<repo>.pr.<number>`, which is the one
# path Dispatch's delivery intake reads (verification-results.ts beside this script).
#
# Its cases: the run's first result write, which reaches the record route and is refused 400
# UNKNOWN_CHECK for a check nobody has; a result recorded and read back under its check (A2); a
# push that moves the pull request's head, after which the earlier result stands at none of the
# compared commits with no write of Dispatch's, and no pass of the check stands until a result at
# the new head is recorded (A4); a fork's pull request taken for no basis, where the same pull
# request with its head in this repository is one; a result for an `after_merge` check recorded on
# a closed issue, where a document write is still refused 409 ISSUE_CLOSED (A5); a `default_branch`
# check whose result at a commit the compare calls `behind` stands at the branch, while its result
# at a commit the compare calls `diverged` stands at another commit (A10); a result at a 40-hex
# commit the fake was seeded with nowhere, refused 400 RESULT_COMMIT_UNKNOWN naming the commit and
# the `owner/repo`, which leaves the check no row; an issue whose repository the fake's
# installation lookup does not find, whose results are answered unknown; the bases one issue's
# three checks compare with, read through the `dispatch results` command and through the API,
# with no commit named, with a commit every check is compared with instead, and again after an
# event moves one head; and what decides a pull request's issue, where a branch, a body and a
# person's link each decide one row as the intake stores it. Three more cases run the `dispatch`
# command as a process against the run's server and judge each step by its exit code and what it
# printed: `dash-refused`, where the reported call
# `dispatch result ... --run - --observed - --negative-control -`, `--run=-`,
# `--observed -` with `--dry-run`, `dispatch comment --body -`, `dispatch message --body -` and
# `dispatch issue --title -` each exit 2 with the spec's line for each flag and leave no result,
# comment, message or issue, while the call the refusal leads to records what two files and stdin
# hold and `dispatch artifact --path -` uploads a local file named `-`;
# `dash-refused-every-flag`, where each text flag the command's help lists, given `-` with
# `--dry-run`, exits 2 with its line, over at least 28 commands and 112 flags; and `values-kept`,
# where a value that starts with `-`, a value written `--observed=<value>` and a `-` a file or
# stdin gives are stored byte for byte, while `--observed '--- PASS ...'` still reads as a flag.
# Each case prints `== <case>`, what it read, and `ok <case>` or `FAIL <case>`; the first case
# failing stops the run, since every later case records a result too, and any FAIL exits 1.
#
# Then the one-pass phases run (one_pass_phase). Each one has the driver make the writes that need
# a server, stops the server, has the driver write its fixture while no server runs, starts the
# server with a pass at its start and the next pass an hour away, and has the driver assert after
# that one pass. A phase that reads before its pass first starts the server with no pass at its
# start, has the driver read, and stops it again. The phase `boundary` checks that boundary
# itself: the two records of the pass in duty_passes, the lease free, and one attribution line in
# the log. The phase `k-attribution` stores two rows as the release before writes them, with no
# texts, and after one pass reads the issue that each row of the attribution case records. The
# seven checks of the attribution follow, each one or more phases in the project ATT: older-rows,
# rule-reaches-each-row, key-reading-changes, facts-only, read-compares, stores-agree and
# limit-of-one (the driver's header says what each one proves).
#
# older-rows runs first, on a database that the build of the release before wrote, before this
# checkout's build has started on it at all (older_rows_upgrade). The release before's build
# migrates the emptied database and stores older-rows' 300 merged pull requests itself, from their
# events and from one pass of its own reconcile, under the delivery settings it needs to take any
# event; then this checkout's build starts on that database and older-rows' phases run on it. The
# run then empties the database again and starts a NATS of its own, so the cases and every phase
# after them start from nothing, with no delivery settings, as they did before older-rows came
# first. key-reading-changes' 5,000 pull requests are stored by the server under test too: its
# intake takes their events and its passes, every two seconds, read each of them once more
# (one_pass_phase --store-every-2s), so a negative control's build that stores another way stores
# them that way.
#
# first-start-tip runs last (first_start_tip), on a NATS of its own and an emptied database: R3 of
# the results lookup, the first start after a deploy. A server with no GitHub App, so no delivery
# intake, makes the stream; the driver publishes the events of two pull requests while no server
# runs; the server starts with the App, and the driver holds that its intake read none of those
# events, that a push after the start writes its head within 5 seconds and the results route
# compares with it, and, after the server starts a second time, that the intake reads no event
# again and the start calls GitHub for nothing but the facts its reconcile pass owes (the driver's
# section "first-start-tip"). A last start, with the durable made again at the stream's first
# message, shows that the driver's reading of where a start binds sees a start that reads again.
#
#   DATABASE_URL=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     [DISPATCH_BIN=<envoy-dispatch>] \
#     [VERIFICATION_RESULTS_E2E_DISPATCH_CLI=<bin/dispatch.ts>] \
#     [VERIFICATION_RESULTS_E2E_ONLY=<case>] [VERIFICATION_RESULTS_E2E_OLD_BUILD=1] \
#     [VERIFICATION_RESULTS_E2E_TIP_CONTROL=1] \
#     [VERIFICATION_RESULTS_E2E_RELEASE_BEFORE=<commit>] \
#     [VERIFICATION_RESULTS_E2E_RELEASE_BEFORE_BIN=<envoy-dispatch of that commit>] \
#     bash scripts/e2e/verification-results.sh
#
# DATABASE_URL is required, and the run refuses to start without it: it names a loopback PostgreSQL
# (127.0.0.1, localhost or ::1, as scripts/e2e/lib/loopback-dsn.py reads the DSN with libpq's own
# parser) whose `public` schema this run empties before the server migrates it, keeping the
# extensions that schema holds, and it leaves what the run wrote there for inspection. The driver
# reads it too, for the fixtures and the answers no route of Dispatch carries (its header lists
# them). DISPATCH_BIN is optional and names a prebuilt envoy-dispatch, which the run copies to
# $work/bin/envoy-dispatch, the binary of every start: built from the base commit it is this
# change's negative control, and it fails at the first result write, the route it does not serve;
# unset, the script builds ./cmd/dispatch from this checkout to that path.
# VERIFICATION_RESULTS_E2E_DISPATCH_CLI is optional and names the bin/dispatch.ts the driver runs
# in place of this checkout's packages/envoy-client/bin/dispatch.ts; the run refuses a value that
# is not a readable file and makes it absolute, since `dash-refused` runs the command in another
# directory. It is the negative control of the three cases above: naming the command of a git
# worktree of the commit this change starts from (`bun install --frozen-lockfile` run in it),
# `dash-refused` fails at step 1, which exits 0 and stores `-` in run, observed and
# negative_control, and `dash-refused-every-flag` fails at its first call, whose output holds no
# line for its flag; naming the command of a worktree of the head whose rule refuses every value
# that starts with `-`, `values-kept` fails at step 1, which exits 2 with the line for
# --negative-control.
# VERIFICATION_RESULTS_E2E_ONLY and VERIFICATION_RESULTS_E2E_OLD_BUILD reach the driver as they
# are, and only a negative control of the seven checks sets them: the first names the one case to
# run, and the driver skips every other case and phase but the boundary's self-check and the
# writes the named case stands on; the second, set to 1, names a server built before the
# attribution line of a pass, as DISPATCH_BIN built from a commit before that line is, so the
# boundary of one pass does not count that line. Under the first, a run exits 1 (`FAIL <case>`)
# only when the named case failed; a run that stopped before that case's check exits 2, and its
# lines read `UNKNOWN <case>`: it shows nothing of the check, as a control or as a pass.
# VERIFICATION_RESULTS_E2E_ONLY=first-start-tip runs first-start-tip alone: no case, no phase and
# no older-rows. VERIFICATION_RESULTS_E2E_TIP_CONTROL=1, which the run refuses without it, is
# first-start-tip's negative control: before the first start the driver creates the intake's
# durable to deliver from the stream's first message, so that start reads each older event and
# writes its head, and first-start-tip fails.
# VERIFICATION_RESULTS_E2E_RELEASE_BEFORE names the commit of the release before this change, whose
# build writes older-rows' database. Unset, it is the newest commit of main without this change,
# read from git at each run (release_before_of_head): on a branch that has not merged, main at the
# commit the branch last merged, so no merge of main leaves it stale; on main and on any branch of
# main after this change merged, the first parent of the commit of main that added this change's
# first migration, so the merge of this change does not make the change its own release before.
# Its packages/envoy is taken with
# `git archive` and built: from this checkout's own repository where that holds the commit, else
# from a bare repository of the run's own that fetches that one commit from this checkout's
# `origin`, since a CI checkout is shallow and holds its own head alone; either way this working
# copy is never touched, and git and tar join the prerequisites.
# VERIFICATION_RESULTS_E2E_RELEASE_BEFORE_BIN names a prebuilt envoy-dispatch of that commit
# instead, which the run copies as it copies DISPATCH_BIN. Neither is read when
# VERIFICATION_RESULTS_E2E_ONLY names another case, since older-rows does not run then.
#
# The ports are VERIFICATION_RESULTS_E2E_DISPATCH_PORT (19061), VERIFICATION_RESULTS_E2E_ENVOY_PORT
# (19063) and VERIFICATION_RESULTS_E2E_GITHUB_PORT (19065), each refused when something already
# answers on it. The server's port is refused inside the kernel's ephemeral range too, since each
# one-pass phase starts the server on it again. NATS listens on a port the kernel picks. The work
# directory, under TMPDIR, holds the binaries, the driver's state file and every process's log, the
# server's of every start in one: removed when the run passes, kept and printed when it fails.
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
  if [ -n "${only_case:-}" ] && [ -z "${case_failed:-}" ]; then
    echo "UNKNOWN $only_case: the run stopped at $check, before the check of $only_case: $*" >&2
    exit 2
  fi
  echo "FAIL $check: $*" >&2
  exit 1
}
# What lib/rig.sh reads of its caller: start_process and await_start log under $evidence/logs, the
# work directory's once the build makes it.
evidence=''
timeout_hook=''
# shellcheck source-path=SCRIPTDIR source=lib/rig.sh
. "$root/scripts/e2e/lib/rig.sh"
# shellcheck source-path=SCRIPTDIR source=lib/run-verdict.sh
. "$root/scripts/e2e/lib/run-verdict.sh"

# DISPATCH_BIN is this script's input, read before the sweep below drops every DISPATCH_* variable.
# VERIFICATION_RESULTS_E2E_DISPATCH_CLI, its other input, is read beside it; the sweep leaves it.
dispatch_bin=${DISPATCH_BIN:-}
dispatch_cli=${VERIFICATION_RESULTS_E2E_DISPATCH_CLI:-}
database_url=${DATABASE_URL:-}
dispatch_port=${VERIFICATION_RESULTS_E2E_DISPATCH_PORT:-19061}
envoy_port=${VERIFICATION_RESULTS_E2E_ENVOY_PORT:-19063}
github_port=${VERIFICATION_RESULTS_E2E_GITHUB_PORT:-19065}
release_before=${VERIFICATION_RESULTS_E2E_RELEASE_BEFORE:-}
release_before_bin=${VERIFICATION_RESULTS_E2E_RELEASE_BEFORE_BIN:-}
# older-rows runs in a whole run and in its own negative control (the driver skips it under any
# other VERIFICATION_RESULTS_E2E_ONLY), and only then is the release before built.
older_rows=''
case "${VERIFICATION_RESULTS_E2E_ONLY:-}" in '' | older-rows) older_rows=1 ;; esac
# first-start-tip runs in a whole run, last, and alone in its own negative control; under any
# other VERIFICATION_RESULTS_E2E_ONLY it does not run.
first_start_tip=''
case "${VERIFICATION_RESULTS_E2E_ONLY:-}" in '' | first-start-tip) first_start_tip=1 ;; esac
tip_control=${VERIFICATION_RESULTS_E2E_TIP_CONTROL:-}
# A negative control's run (VERIFICATION_RESULTS_E2E_ONLY) shows something of its check only when
# that check failed: the driver's status 1, which under that setting is a failed case of the named
# check alone (case_failed). Any other stop (a prerequisite, a build, a start, a server that exited,
# a step of the driver outside the cases, or a failed case of another name, which the driver exits
# 2 for) came before that check, so fail prints UNKNOWN for it and exits 2, never the 1 of a failed
# check, and the run's last line says UNKNOWN too (cleanup).
only_case=${VERIFICATION_RESULTS_E2E_ONLY:-}
case_failed=''

# A Legion pane exports the real listener (ENVOY_URL), the real Dispatch (DISPATCH_URL,
# DISPATCH_TOKEN_FILE) and an nkey for the real NATS; nothing this run starts inherits any of it.
# PGHOSTADDR, PGSERVICE and PGSERVICEFILE go with them: require_loopback_dsn in
# scripts/e2e/lib/loopback-dsn.sh says why a caller of the loopback guard clears the three.
mapfile -t inherited < <(compgen -e)
for name in "${inherited[@]}"; do
  case "$name" in
    DISPATCH_* | ENVOY_* | NATS_* | FAKE_* | PGHOSTADDR | PGSERVICE | PGSERVICEFILE) unset "$name" ;;
  esac
done

# The work directory is made once the prerequisites hold, so a refused run leaves nothing behind.
work=''
nats_pid=''
fake_envoy_pid=''
fake_github_pid=''
dispatch_pid=''

# cleanup stops what the run started and prints the run's last line through lib/run-verdict.sh,
# which exits with rc: the script's own status for EXIT, 128 plus the signal's number for INT and
# TERM. PASS needs rc 0 and run_end, the script's last statement, so a run that a HUP ended, whose
# EXIT trap reads 0, reads FAIL as an interrupted run does. The work directory goes with a pass.
# A negative control's run that did not pass and whose check did not fail reads UNKNOWN, and exits
# 2 in place of 0 or 1.
cleanup() {
  local rc=$1
  local failed="verification results e2e: FAIL at $check after $((SECONDS - started))s${work:+ (logs: $work)}"
  trap - EXIT INT TERM
  set +e
  stop_pid "$dispatch_pid"
  stop_pid "$fake_github_pid"
  stop_pid "$fake_envoy_pid"
  stop_pid "$nats_pid"
  if [ -n "$work" ]; then
    if run_passed "$rc"; then
      rm -rf "$work"
    else
      rm -rf "${work:?}/bin" "${work:?}/nats-store" "${work:?}/older-rows-nats-store" \
        "${work:?}/first-start-tip-nats-store" "${work:?}/release-before" \
        "${work:?}/release-before.git" "${work:?}/main-history.git"
    fi
  fi
  if [ -n "$only_case" ] && [ -z "$case_failed" ] && ! run_passed "$rc"; then
    failed="verification results e2e: UNKNOWN for $only_case: the run stopped at $check after $((SECONDS - started))s, before that check${work:+ (logs: $work)}"
    case "$rc" in 0 | 1) rc=2 ;; esac
  fi
  run_verdict "$rc" "verification results e2e: PASS in $((SECONDS - started))s" "$failed"
}
trap 'cleanup "$?"' EXIT
trap 'cleanup 130' INT
trap 'cleanup 143' TERM

begin prerequisites
[ -n "$database_url" ] ||
  fail "DATABASE_URL is required: it names a loopback PostgreSQL database this run resets (postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable)"
for tool in go bun curl psql openssl python3; do
  command -v "$tool" >/dev/null || fail "$tool is not on PATH"
done
case "$database_url" in
  postgres://* | postgresql://*) ;;
  *) fail "DATABASE_URL must be a postgres:// or postgresql:// URL" ;;
esac
require_loopback_dsn db_host DATABASE_URL "$database_url" "this run resets the database" || fail "$db_host"
if [ -n "$dispatch_bin" ]; then
  [ -x "$dispatch_bin" ] || fail "DISPATCH_BIN=$dispatch_bin is not an executable file"
fi
if [ -n "$dispatch_cli" ]; then
  if [ ! -f "$dispatch_cli" ] || [ ! -r "$dispatch_cli" ]; then
    fail "VERIFICATION_RESULTS_E2E_DISPATCH_CLI=$dispatch_cli is not a readable file"
  fi
  dispatch_cli=$(cd "$(dirname "$dispatch_cli")" && pwd)/$(basename "$dispatch_cli")
fi
if [ -n "$older_rows" ]; then
  if [ -n "$release_before_bin" ]; then
    [ -x "$release_before_bin" ] ||
      fail "VERIFICATION_RESULTS_E2E_RELEASE_BEFORE_BIN=$release_before_bin is not an executable file"
  else
    for tool in git tar; do
      command -v "$tool" >/dev/null || fail "$tool is not on PATH: older-rows takes the release before's tree with git archive"
    done
  fi
fi
if [ -n "$tip_control" ]; then
  [ "$tip_control" = 1 ] || fail "VERIFICATION_RESULTS_E2E_TIP_CONTROL=$tip_control is not 1"
  [ "$only_case" = first-start-tip ] ||
    fail "VERIFICATION_RESULTS_E2E_TIP_CONTROL=1 is first-start-tip's negative control, and needs VERIFICATION_RESULTS_E2E_ONLY=first-start-tip"
fi
for port in "$dispatch_port" "$envoy_port" "$github_port"; do
  if ! [[ "$port" =~ ^[1-9][0-9]{0,4}$ ]] || [ "$port" -gt 65535 ]; then
    fail "'$port' is not a port"
  fi
  port_answers "$port" && fail "something already answers on 127.0.0.1:$port"
done
[ "$(printf '%s\n' "$dispatch_port" "$envoy_port" "$github_port" | sort -u | wc -l)" -eq 3 ] ||
  fail "the Dispatch, fake Envoy and fake GitHub ports are not three different ports"
# Each one-pass phase starts the server again on its port. The kernel gives any port of its
# ephemeral range to an outgoing connection, so such a connection can take the port between two
# starts, and the next start fails with "address already in use".
if read -r ephemeral_low ephemeral_high </proc/sys/net/ipv4/ip_local_port_range 2>/dev/null &&
  [ "$dispatch_port" -ge "$ephemeral_low" ] && [ "$dispatch_port" -le "$ephemeral_high" ]; then
  fail "VERIFICATION_RESULTS_E2E_DISPATCH_PORT=$dispatch_port is in the kernel's ephemeral range ($ephemeral_low to $ephemeral_high); the server starts on it again for each one-pass phase, so give a port outside that range"
fi
note "database host $db_host (URL not printed)"
pass

# release_before_of_head <checkout> <repository>: prints the release before this change when the
# run is given none: the newest commit of main that does not hold this change. It takes the merge
# base of HEAD with origin's main, the newest commit of main the branch holds. Where that commit
# does not hold this change's first migration (20261117111040_pull_request_texts), the branch has
# not merged and the merge base is the answer. Where it does, this change is on main: the answer
# is the first parent of the commit on main's first-parent line that added the migration, the
# commit of main just before the merge, whichever commit a run starts from, main's or a later
# branch's. The commit that added the migration in HEAD's own history is no answer on a branch:
# the branch's commits before it hold part of this change.
# It clones main's commits and trees, no file contents (--filter=blob:none), from the checkout's
# `origin` into <repository>, a bare repository of the run's own, so the checkout is never written.
# A checkout with its whole history lends HEAD's commits to that repository
# (objects/info/alternates), so a HEAD that was never pushed is read as it stands; a shallow
# checkout, as CI's is, has HEAD's history fetched from `origin` in one fetch, where the partial
# clone would otherwise fetch it a commit at a time as merge-base walks it. It prints nothing and
# fails when the checkout names no origin or git cannot answer.
release_before_of_head() {
  local checkout=$1 repository=$2 url head shallow base added
  local migration=packages/envoy/internal/dispatch/store/migrations/20261117111040_pull_request_texts.up.sql
  url=$(git -C "$checkout" remote get-url origin) || return 1
  head=$(git -C "$checkout" rev-parse --verify 'HEAD^{commit}') || return 1
  shallow=$(git -C "$checkout" rev-parse --is-shallow-repository) || return 1
  git clone -q --bare --filter=blob:none --single-branch --branch main --no-tags "$url" "$repository" ||
    return 1
  if [ "$shallow" = false ]; then
    git -C "$checkout" rev-parse --path-format=absolute --git-path objects \
      >>"$repository/objects/info/alternates" || return 1
  else
    git -C "$repository" fetch -q --no-tags origin "$head" || return 1
  fi
  base=$(git -C "$repository" merge-base "$head" refs/heads/main) || return 1
  # --no-renames: a rename's detection reads file contents, which the clone does not hold.
  added=$(git -C "$repository" log --first-parent --no-renames --diff-filter=A --format=%H "$base" -- "$migration") ||
    return 1
  if [ -z "$added" ]; then
    echo "$base"
  else
    git -C "$repository" rev-parse --verify "${added##*$'\n'}^1"
  fi
}

# release_before_tree takes the release before's packages/envoy into $work/release-before and sets
# release_before_sha. The tree is the commit's own, taken out of a repository and never checked
# out, so this working copy is never touched: this checkout's own repository where it holds the
# commit, else a bare repository of the run's own that fetches that one commit, at depth 1, from
# this checkout's `origin`, since a CI checkout is shallow and holds its own head alone.
release_before_tree() {
  local source=$root url
  if ! release_before_sha=$(git -C "$root" rev-parse --verify --quiet "$release_before^{commit}" 2>/dev/null); then
    url=$(git -C "$root" remote get-url origin 2>/dev/null) ||
      fail "this checkout holds no commit $release_before and names no origin to fetch it from; give VERIFICATION_RESULTS_E2E_RELEASE_BEFORE_BIN, an envoy-dispatch built from it"
    source=$work/release-before.git
    git init -q --bare "$source" || fail "could not make the repository $source"
    git -C "$source" fetch -q --no-tags --depth=1 "$url" "$release_before" ||
      fail "could not fetch the release before, $release_before, from $url"
    release_before_sha=$(git -C "$source" rev-parse --verify --quiet "FETCH_HEAD^{commit}") ||
      fail "the fetch of $release_before from $url brought no commit"
  fi
  mkdir -p "$work/release-before"
  git -C "$source" archive "$release_before_sha" packages/envoy | tar -x -C "$work/release-before" ||
    fail "could not take the tree of $release_before_sha"
}

begin build
work=$(mktemp -d "${TMPDIR:-/tmp}/verification-results.XXXXXXXX")
evidence=$work
note "work directory $work"
mkdir -p "$work/bin" "$work/web-dist" "$work/logs"
if [ -z "$dispatch_bin" ]; then
  go -C "$root/packages/envoy" build -o "$work/bin/envoy-dispatch" ./cmd/dispatch ||
    fail "./cmd/dispatch did not build"
  note "built envoy-dispatch from $root/packages/envoy"
else
  cp "$dispatch_bin" "$work/bin/envoy-dispatch" || fail "could not copy DISPATCH_BIN=$dispatch_bin"
  note "envoy-dispatch is a copy of DISPATCH_BIN=$dispatch_bin"
fi
dispatch_bin=$work/bin/envoy-dispatch
go -C "$root/packages/envoy" build -o "$work/bin/natsd" ./internal/testnats/cmd/natsd ||
  fail "./internal/testnats/cmd/natsd did not build"
if [ -n "$older_rows" ] && [ -n "$release_before_bin" ]; then
  cp "$release_before_bin" "$work/bin/envoy-dispatch-release-before" ||
    fail "could not copy VERIFICATION_RESULTS_E2E_RELEASE_BEFORE_BIN=$release_before_bin"
  note "the release before's envoy-dispatch is a copy of VERIFICATION_RESULTS_E2E_RELEASE_BEFORE_BIN=$release_before_bin"
elif [ -n "$older_rows" ]; then
  if [ -z "$release_before" ]; then
    release_before=$(release_before_of_head "$root" "$work/main-history.git") ||
      fail "could not read the newest commit of origin's main without this change; give VERIFICATION_RESULTS_E2E_RELEASE_BEFORE=<commit> or VERIFICATION_RESULTS_E2E_RELEASE_BEFORE_BIN"
    note "the release before is $release_before, the newest commit of origin's main without this change"
  fi
  release_before_tree
  go -C "$work/release-before/packages/envoy" build -o "$work/bin/envoy-dispatch-release-before" ./cmd/dispatch ||
    fail "the release before's ./cmd/dispatch did not build"
  note "built the release before's envoy-dispatch from $release_before_sha"
fi
pass

begin reset-database
# Empty, migrations included: the server migrates the new schema at boot, so a base binary and this
# checkout's each start from a database of their own migrations. empty_public_schema
# (lib/loopback-dsn.sh) says what goes and why the schema's extensions stay.
empty_public_schema "$database_url" || fail "could not empty the database's public schema"
pass

begin nats
# start_nats STORE starts natsd with its store in STORE, on the port the kernel picks (`-p -1`),
# read back from the line the server logs once it listens, and sets nats_url. older-rows runs on a
# NATS of its own, which older_rows_upgrade stops before it starts the run's own.
start_nats() {
  start_process nats "$work/bin/natsd" -p -1 -store_dir "$1"
  await_start nats "$nats_pid" 60 "natsd to be ready" grep -q 'Server is ready' "$work/logs/nats.log"
  nats_port=$(report_port "$work/logs/nats.log" 'Listening for client connections on [^ ]*:') ||
    fail "natsd logged no client port; see $work/logs/nats.log"
  nats_url=nats://127.0.0.1:$nats_port
}
if [ -n "$older_rows" ]; then
  start_nats "$work/older-rows-nats-store"
else
  start_nats "$work/nats-store"
fi
note "JetStream on $nats_url"
pass

begin fake-envoy
FAKE_ENVOY_PORT=$envoy_port start_process fake_envoy bun "$root/packages/dispatch/e2e/fake-envoy.ts"
await_start fake_envoy "$fake_envoy_pid" 60 "the fake Envoy listener to answer /healthz" \
  curl -fsS --max-time 2 "http://127.0.0.1:$envoy_port/healthz"
note "fake Envoy listener on http://127.0.0.1:$envoy_port"
pass

begin fake-github
FAKE_GITHUB_PORT=$github_port start_process fake_github bun "$root/packages/dispatch/e2e/fake-github.ts"
await_start fake_github "$fake_github_pid" 60 "the fake GitHub to answer /healthz" \
  curl -fsS --max-time 2 "http://127.0.0.1:$github_port/healthz"
github_url=http://127.0.0.1:$github_port
note "fake GitHub on $github_url"
pass

begin identity
# The server gets this environment and nothing else. Identity is the production cookie, minted at
# the dev sign-in route (DISPATCH_DEV_SIGNIN=1) with a signing key the process generates for itself;
# HOME and the XDG directories name nothing, so no envoy.json, data directory or AWS profile of the
# caller's is read. The GitHub App is a throwaway RSA key beside a fake client id, as the browser
# harness's own server runs with (packages/dispatch/e2e/run-server.sh): it is what makes the
# delivery intake run at all, and every call it makes goes to the fake GitHub above.
#
# The delivery reconcile runs every two seconds here, under the test hooks, in place of the five
# minutes a deployment runs it at (DISPATCH_TEST_DELIVERY_RECONCILE_INTERVAL,
# packages/dispatch/e2e/run-server.sh runs it the same way), and its first pass runs as the server
# starts: the cases' answers hold while passes run beside them, as passes run beside every read of
# a deployment. A case that reads the answers of exactly one pass is a one-pass phase below, which
# starts the server again with the next pass an hour away.
agent_token=$(openssl rand -hex 24)
app_pem=$(openssl genrsa 2048 2>/dev/null) || fail "could not generate the throwaway App key"
dispatch_url=http://127.0.0.1:$dispatch_port
# start_dispatch INTERVAL [AT_START [BINARY [APP]]] starts envoy-dispatch with the delivery
# reconcile ticking every INTERVAL and, unless AT_START is given empty, its first pass as it
# starts, and waits for it to answer /healthz. BINARY is this checkout's build unless it names
# another, which only older_rows_upgrade does, for the release before's. APP given empty starts it
# with no GitHub App, so it runs no delivery intake: first_start_tip's start that makes the stream
# alone does. dispatch_settings adds settings of the test hooks to the environment; only
# first_start_tip sets it. Each start appends to $work/logs/dispatch.log, which the driver reads
# (VERIFICATION_RESULTS_E2E_DISPATCH_LOG).
dispatch_settings=()
start_dispatch() {
  local app=(DISPATCH_APP_CLIENT_ID=Iv1.e2efake DISPATCH_APP_PEM_B64="$app_pem")
  [ -n "${4-1}" ] || app=()
  start_process dispatch env -i \
    PATH="$PATH" \
    TMPDIR="$work" \
    HOME=/nonexistent \
    XDG_CACHE_HOME=/nonexistent \
    XDG_CONFIG_HOME=/nonexistent \
    XDG_DATA_HOME=/nonexistent \
    DATABASE_URL="$database_url" \
    DISPATCH_AGENT_TOKEN="$agent_token" \
    "${app[@]}" \
    DISPATCH_DEV_SIGNIN=1 \
    DISPATCH_GITHUB_API_BASE="$github_url" \
    DISPATCH_IDENTITY=cookie \
    DISPATCH_INSECURE_COOKIE=1 \
    DISPATCH_LISTEN_HOST=127.0.0.1 \
    DISPATCH_PORT="$dispatch_port" \
    DISPATCH_SERVER_URL="$dispatch_url" \
    DISPATCH_TEST_DELIVERY_RECONCILE_INTERVAL="$1" \
    DISPATCH_TEST_HOOKS=1 \
    DISPATCH_TEST_RECONCILE_AT_START="${2-1}" \
    DISPATCH_WEB_DIST="$work/web-dist" \
    ENVOY_URL="http://127.0.0.1:$envoy_port" \
    NATS_URLS="$nats_url" \
    "${dispatch_settings[@]}" \
    "${3:-$dispatch_bin}"
  await_start dispatch "$dispatch_pid" 60 "envoy-dispatch to answer /healthz" \
    curl -fsS --max-time 2 "$dispatch_url/healthz"
}
export VERIFICATION_RESULTS_E2E_DISPATCH_LOG=$work/logs/dispatch.log
pass

export VERIFICATION_RESULTS_E2E_DISPATCH_LOG=$work/logs/dispatch.log

# driver ARG... runs the driver with ARG... before its three URLs. The state file carries what one
# run of the driver leaves for a later one. A server that exited during the run fails the check,
# and so does the driver's status: 1 is a case that failed (it sets case_failed), 3 a half of a
# one-pass phase that VERIFICATION_RESULTS_E2E_ONLY skips (it sets phase_skipped), any other a step
# outside the cases.
phase_skipped=''
driver() {
  local status=0
  VERIFICATION_RESULTS_E2E_AGENT_TOKEN=$agent_token \
    VERIFICATION_RESULTS_E2E_DATABASE_URL=$database_url \
    VERIFICATION_RESULTS_E2E_DISPATCH_CLI=$dispatch_cli \
    VERIFICATION_RESULTS_E2E_STATE=$work/driver-state.json \
    bun "$root/scripts/e2e/verification-results.ts" "$@" "$dispatch_url" "$nats_url" "$github_url" ||
    status=$?
  if [ -n "$dispatch_pid" ] && ! kill -0 "$dispatch_pid" 2>/dev/null; then
    fail "envoy-dispatch exited during the run; see $work/logs/dispatch.log"
  fi
  case "$status" in
    0) ;;
    1)
      case_failed=1
      fail "a case failed (above)"
      ;;
    3) phase_skipped=1 ;;
    *) fail "the driver stopped before its cases (status $status)" ;;
  esac
}

# dispatch_port_closed succeeds once nothing answers on the server's port.
dispatch_port_closed() { ! port_answers "$dispatch_port"; }

# stop_dispatch stops the server by its recorded pid, and waits until nothing answers on its port.
stop_dispatch() {
  stop_pid "$dispatch_pid"
  dispatch_pid=''
  until_true 30 "envoy-dispatch to stop listening on $dispatch_port" dispatch_port_closed
}

# one_pass_phase PHASE [--read-before-the-pass | --store-every-2s] runs the driver's phase PHASE
# around one complete pass of the delivery reconcile (the driver's section "the tooling of one
# complete pass"). First the driver makes the phase's writes that need a server (--phase
# PHASE-prepare), while the server of the step before still runs; with --store-every-2s, through
# the server started again with its reconcile every 2 seconds, so its own passes read what its
# intake stored in those writes. Then the shell stops the server by its recorded pid and has the
# driver write the phase's fixture while no server runs. With --read-before-the-pass, it starts the
# server with no pass at its start and the next pass an hour away, has the driver read the fixture
# through it (--phase PHASE-before), and stops it again. Then it starts the server with a pass at
# its start and the next pass an hour away, and has the driver assert after that one pass. A phase
# whose assert the driver skipped (VERIFICATION_RESULTS_E2E_ONLY) prints `skip PHASE`, never `ok`.
one_pass_phase() {
  begin "$1"
  if [ "${2:-}" = --store-every-2s ]; then
    stop_dispatch
    start_dispatch 2s
    note "envoy-dispatch started again, its delivery reconcile every 2s, for the writes of $1"
  fi
  driver --phase "$1-prepare"
  stop_dispatch
  driver --phase "$1-fixture"
  if [ "${2:-}" = --read-before-the-pass ]; then
    start_dispatch 1h ''
    note "envoy-dispatch started with no pass at its start, its delivery reconcile every 1h"
    driver --phase "$1-before"
    stop_dispatch
  fi
  start_dispatch 1h
  note "envoy-dispatch started again, its delivery reconcile every 1h after its start pass"
  phase_skipped=''
  driver --phase "$1-assert"
  if [ -n "$phase_skipped" ]; then
    echo "skip $check: the driver asserted nothing (VERIFICATION_RESULTS_E2E_ONLY=${VERIFICATION_RESULTS_E2E_ONLY:-})"
  else
    pass
  fi
}

# older_rows_upgrade runs older-rows on a database the release before wrote (the driver's section
# "older-rows" says what each build writes there). The release before's build starts on the
# emptied database and migrates it, with no pass at its start and its next an hour away, and the
# driver writes through it: the project, the issues L(j), the delivery settings, the events of 260
# merged pull requests, under the repository's old name and then its new one, and the issues D(j).
# It starts again with a pass at its start, which the driver waits for before it makes the issues
# N(j), and stops. Then older-rows' two phases run on this checkout's build, the first with a read
# before its pass. Last, the database is emptied again and the run's own NATS starts in place of
# older-rows' own, so the cases and the phases after them start from nothing: the delivery
# settings that only the release before needed go with it.
older_rows_upgrade() {
  local release=$work/bin/envoy-dispatch-release-before
  begin older-rows-release-before
  start_dispatch 1h '' "$release"
  note "the release before's envoy-dispatch started, with no pass at its start and its delivery reconcile every 1h"
  driver --phase older-rows-release-writes
  stop_dispatch
  start_dispatch 1h 1 "$release"
  note "the release before's envoy-dispatch started again, with one pass at its start"
  driver --phase older-rows-release-pass
  stop_dispatch
  pass
  one_pass_phase older-rows --read-before-the-pass
  one_pass_phase older-rows-set-back
  begin after-older-rows
  stop_dispatch
  empty_public_schema "$database_url" || fail "could not empty the database's public schema"
  rm -f "$work/driver-state.json"
  stop_pid "$nats_pid"
  nats_pid=''
  rm -rf "$work/older-rows-nats-store"
  mv "$work/logs/nats.log" "$work/logs/older-rows-nats.log"
  start_nats "$work/nats-store"
  note "the database is empty again, and the run's own JetStream is on $nats_url"
  pass
}

# first_start_tip runs first-start-tip (the driver's section "first-start-tip"), each start of it
# with the next pass of its delivery reconcile an hour away. It stops the server of the step
# before, if any, and the NATS, empties the database and starts a NATS of its own, with no stream.
# A server with no GitHub App, and so no delivery intake, starts and makes the stream, as a
# deployment's stream stands before the first start that runs an intake, and stops. The driver
# publishes the events of two pull requests while no server runs (backlog). The server starts
# with the App and a pass at its start, and the driver holds that start and a push after it
# (first-start). It stops; the driver reads where the durable stands and empties the fake's log of
# calls (stopped); it starts again, and the driver holds that second start (second-start). Last,
# with the server stopped, the driver creates the durable again to deliver from the stream's first
# message (control-durable), and one more start must bind at or below where the second start
# found it (control-start). Each start with the App runs the CI reconcile and the webhook
# redelivery sweep an hour apart, so the first pass of each, at a random moment within its hour,
# falls in the window of calls the driver reads only once in more than a thousand runs.
first_start_tip() {
  begin first-start-tip
  stop_dispatch
  stop_pid "$nats_pid"
  nats_pid=''
  rm -rf "$work/nats-store"
  mv "$work/logs/nats.log" "$work/logs/nats-before-first-start-tip.log"
  empty_public_schema "$database_url" || fail "could not empty the database's public schema"
  rm -f "$work/driver-state.json"
  start_nats "$work/first-start-tip-nats-store"
  dispatch_settings=(DISPATCH_TEST_CI_RECONCILE_INTERVAL=1h DISPATCH_TEST_REDELIVERY_INTERVAL=1h)
  start_dispatch 1h '' '' ''
  note "envoy-dispatch started with no GitHub App, so no delivery intake, and made the stream on $nats_url"
  stop_dispatch
  driver --phase first-start-tip-backlog
  start_dispatch 1h
  note "envoy-dispatch started with the App, its first start after the events"
  driver --phase first-start-tip-first-start
  stop_dispatch
  driver --phase first-start-tip-stopped
  start_dispatch 1h
  note "envoy-dispatch started again"
  driver --phase first-start-tip-second-start
  stop_dispatch
  driver --phase first-start-tip-control-durable
  start_dispatch 1h
  note "envoy-dispatch started once more, its durable made again at the stream's first message"
  driver --phase first-start-tip-control-start
  stop_dispatch
  dispatch_settings=()
  pass
}

# Under VERIFICATION_RESULTS_E2E_ONLY=first-start-tip, first-start-tip runs alone.
if [ "$only_case" != first-start-tip ]; then
  [ -z "$older_rows" ] || older_rows_upgrade

  begin dispatch
  start_dispatch 2s
  note "envoy-dispatch on $dispatch_url, NATS on, delivery intake reading $nats_url, delivery reconcile every 2s"
  pass

  check=cases
  driver
  one_pass_phase boundary
  one_pass_phase k-attribution
  # The seven checks of the attribution (the driver's section "the seven checks"), in the order the
  # driver's cases need: each phase that wants no read of GitHub runs before stores-agree writes
  # the delivery settings, since a pass with settings reads GitHub for its other steps. older-rows
  # ran before the cases (older_rows_upgrade).
  one_pass_phase rule-reaches-each-row
  one_pass_phase rule-reaches-each-row-link
  one_pass_phase rule-reaches-each-row-move
  one_pass_phase rule-reaches-each-row-new-issue
  one_pass_phase key-reading-changes --store-every-2s
  one_pass_phase facts-only
  one_pass_phase read-compares
  one_pass_phase stores-agree --read-before-the-pass
  one_pass_phase stores-agree-link
  one_pass_phase limit-of-one
fi
[ -z "$first_start_tip" ] || first_start_tip
run_end
