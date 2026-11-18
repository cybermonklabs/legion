#!/usr/bin/env bash
# The live proof that a rollback leaves no daemon with notices it cannot decode (the review's
# finding 4). Four runs of the daemon package's `-tags e2e` driver, over one PostgreSQL database of
# the run's own and one nats-server of the run's own; the third and the fourth boot the whole daemon
# by the function `legion start` calls:
#
#   fixture      the build of --base-commit, the release before, migrates the empty database and
#                records a tree with one claim in each state (rollback_live_test.go, copied into
#                that commit's tree, since the commit has no such file);
#   seed         this change's build migrates that database forward over those rows, reads each
#                claim back, launches each twice more, which keeps the boot tokens of the launches
#                it left, and queues one of each notice this change adds beside a canary tick
#                (rollback_forward_live_test.go);
#   rollback     the release before's daemon starts on that database, runs its outbox — the canary
#                tick is delivered — and leaves each of those notices as it stands, with no
#                attempt, no error and no decode failure in its log, and its state answers;
#   roll forward before this change's daemon starts, the database holds the boot token of each
#                launch the release before replaced during the rollback, which only the database's
#                own trigger keeps; then the daemon starts on the database again and delivers each
#                notice once, and prints the claims and the boot tokens of earlier launches.
#
# --new-commit builds the seed and the roll-forward from that commit's tree instead of this
# checkout, with this checkout's two driver files copied into it. Run with the commit of this
# change before its notices had kinds of their own, it is the negative control of the notices: the
# rollback run fails, naming the decode failures the older daemon logged and the rows that carry
# them. Run with a commit of this change that has the notices' kinds and not the trigger, it is the
# negative control of the kept tokens: the roll-forward run fails, naming each claim the release
# before launched whose replaced boot token is not kept.
#
# Run it from a checkout on a Linux host with go, git, tar, psql, awk and python3, as
#   LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/rollback-notices.sh --base-commit <the release before> [--new-commit <rev>]
# LEGION_TEST_PG_DSN names a loopback PostgreSQL server the run may create a database on, which it
# drops on any exit; lib/loopback-dsn.sh refuses any other host with exit 2, before anything is
# built or started. The broker is LEGION_E2E_NATS_BIN's nats-server, or the pinned one the run
# installs into a GOBIN of its own. ROLLBACK_NOTICES_EVIDENCE_DIR (default a fresh directory under
# TMPDIR, or /tmp) keeps the transcript, the nats-server's log, each run's driver output and each
# daemon's own log, and is printed. It needs no kube context, no tmux and no agent.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib/transcript.sh
. "$root/scripts/e2e/lib/transcript.sh"
# shellcheck source-path=SCRIPTDIR source=lib/loopback-dsn.sh
. "$root/scripts/e2e/lib/loopback-dsn.sh"

# The nats-server the run installs when the caller names none: lib/live-daemon-proof.sh's pin, the
# hub's v2.15.1, the version packages/envoy/go.mod pins. The daemon refuses a nats-server below
# v2.11.12, v2.12.4 or v2.14.0 (packages/envoy/natsversion).
nats_version=v2.15.1

# The two driver files, relative to packages/daemon: the rollback run's, which the release before
# is built with, and the seed's and the roll-forward's, which only this change's tree compiles.
rollback_driver=internal/daemon/rollback_live_test.go
forward_driver=internal/daemon/rollback_forward_live_test.go

base_commit=''
new_commit=''
evidence=''
work=''
nats_pid=''
database=''
# require_loopback_dsn and run_database_url (lib/loopback-dsn.sh) assign these two.
db_host=''
database_url=''
check=setup
check_started=$SECONDS
started=$SECONDS

begin() { check=$1; check_started=$SECONDS; echo "== $check"; }
note() { echo "   $*"; }
pass() { echo "ok $check ($((SECONDS - check_started))s)"; }
# refuse is for an input of the caller's (exit 2); fail is for the box the run is on, or a step that
# did not hold (exit 1).
refuse() { echo "FAIL $check: $*" >&2; exit 2; }
fail() { echo "FAIL $check: $*" >&2; exit 1; }

cleanup() {
  local status=$?
  set +e
  if [ -n "$nats_pid" ] && kill -0 "$nats_pid" 2>/dev/null; then
    kill -TERM "$nats_pid" 2>/dev/null
    wait "$nats_pid" 2>/dev/null
  fi
  if [ -n "$database" ]; then
    psql "$LEGION_TEST_PG_DSN" -X -q -v ON_ERROR_STOP=1 -c "drop database if exists $database with (force)" >/dev/null ||
      echo "could not drop the run's database $database" >&2
  fi
  [ -z "$work" ] || rm -rf "$work"
  if [ "$status" -eq 0 ]; then
    echo "rollback notices e2e: PASS in $((SECONDS - started))s (evidence: $evidence)"
  else
    echo "rollback notices e2e: FAIL at $check after $((SECONDS - started))s (evidence: ${evidence:-none})" >&2
  fi
}
trap cleanup EXIT

while [ $# -gt 0 ]; do
  case "$1" in
    --base-commit | --new-commit)
      [ $# -ge 2 ] || refuse "$1 takes a commit"
      if [ "$1" = --base-commit ]; then base_commit=$2; else new_commit=$2; fi
      shift 2
      ;;
    --base-commit=*) base_commit=${1#--base-commit=}; shift ;;
    --new-commit=*) new_commit=${1#--new-commit=}; shift ;;
    *) refuse "unknown argument '$1': this script takes --base-commit <rev> and --new-commit <rev>" ;;
  esac
done

evidence=${ROLLBACK_NOTICES_EVIDENCE_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-rollback-notices-evidence.XXXXXXXX")}
mkdir -p "$evidence"
transcript_to "$evidence/transcript.log"

begin prerequisites
[ -n "$base_commit" ] || refuse "--base-commit is required: it names the release before this change, whose daemon the rollback runs"
[ -n "${LEGION_TEST_PG_DSN:-}" ] ||
  refuse "LEGION_TEST_PG_DSN is required: it names the PostgreSQL server this run creates its database on (postgres://<user>@<host>:<port>/<database>?sslmode=disable). This script starts no database."
for tool in go git tar psql awk python3; do
  command -v "$tool" >/dev/null || fail "$tool is not on PATH"
done
# require_loopback_dsn in lib/loopback-dsn.sh says why a caller of the loopback guard clears these
# three, and why the call comes after the tool check above.
unset PGHOSTADDR PGSERVICE PGSERVICEFILE
require_loopback_dsn db_host LEGION_TEST_PG_DSN "$LEGION_TEST_PG_DSN" "this run creates and drops a database there" ||
  refuse "$db_host"
base_sha=$(git -C "$root" rev-parse --verify "$base_commit^{commit}" 2>/dev/null) ||
  refuse "--base-commit '$base_commit' names no commit of this repository"
new_sha=''
if [ -n "$new_commit" ]; then
  new_sha=$(git -C "$root" rev-parse --verify "$new_commit^{commit}" 2>/dev/null) ||
    refuse "--new-commit '$new_commit' names no commit of this repository"
fi
work=$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-rollback-notices.$$.XXXXXXXX")
nats_bin=${LEGION_E2E_NATS_BIN:-}
if [ -z "$nats_bin" ]; then
  note "LEGION_E2E_NATS_BIN names none; installing nats-server $nats_version into this run's own GOBIN"
  # The toolchain's own go, under its own GOROOT, for the reason lib/live-daemon-proof.sh gives.
  goroot=$(go env GOROOT) || fail "go names no GOROOT"
  GOBIN="$work/bin" GOROOT=$goroot "$goroot/bin/go" install "github.com/nats-io/nats-server/v2@$nats_version" ||
    fail "could not install nats-server $nats_version; name a binary in LEGION_E2E_NATS_BIN instead"
  nats_bin=$work/bin/nats-server
fi
[ -x "$nats_bin" ] || fail "no executable nats-server at $nats_bin"
note "go $(go version | awk '{print $3}'), nats-server $("$nats_bin" --version), database host $db_host (DSN not printed)"
note "the release before: $base_sha; this change: ${new_sha:-this checkout, $(git -C "$root" rev-parse HEAD)}"
pass

begin database
database=legion_rollback_$$_$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
run_database_url database_url LEGION_TEST_PG_DSN "$LEGION_TEST_PG_DSN" "$database" "this run creates and drops a database there" ||
  refuse "$database_url"
psql "$LEGION_TEST_PG_DSN" -X -q -v ON_ERROR_STOP=1 -c "create database $database" >/dev/null ||
  fail "could not create the run's database $database"
project=RB$(od -An -N5 -tx1 /dev/urandom | tr -d ' \n' | tr '[:lower:]' '[:upper:]')
note "database $database, project $project"
pass

begin broker
mkdir -p "$work/nats-store"
: >"$evidence/nats.log"
# The port is the kernel's own pick, read back from the line the server logs once it listens, as
# lib/live-daemon-proof.sh reads it.
"$nats_bin" -js -a 127.0.0.1 -p -1 -sd "$work/nats-store" >>"$evidence/nats.log" 2>&1 &
nats_pid=$!
for _ in $(seq 1 120); do
  grep -q 'Server is ready' "$evidence/nats.log" && break
  kill -0 "$nats_pid" 2>/dev/null || fail "the nats-server exited at startup; see $evidence/nats.log"
  sleep 0.5
done
grep -q 'Server is ready' "$evidence/nats.log" || fail "the nats-server was not ready within 60s; see $evidence/nats.log"
port_nats=$(awk -F: '/Listening for client connections on/ {print $NF; exit}' "$evidence/nats.log")
[ -n "$port_nats" ] || fail "the nats-server logged no client port; see $evidence/nats.log"
note "JetStream on nats://127.0.0.1:$port_nats"
pass

begin drivers
# Each tree other than this checkout is the commit's own, taken out of the repository rather than
# checked out, so this working copy is never touched; the driver files are this checkout's.
new_tree=$root
if [ -n "$new_sha" ]; then
  new_tree=$work/new
  mkdir -p "$new_tree"
  git -C "$root" archive "$new_sha" | tar -x -C "$new_tree" || fail "could not take the tree of $new_sha"
  cp "$root/packages/daemon/$rollback_driver" "$new_tree/packages/daemon/$rollback_driver"
  cp "$root/packages/daemon/$forward_driver" "$new_tree/packages/daemon/$forward_driver"
fi
mkdir -p "$work/base"
git -C "$root" archive "$base_sha" | tar -x -C "$work/base" || fail "could not take the tree of $base_sha"
[ ! -e "$work/base/packages/daemon/$forward_driver" ] ||
  fail "$base_sha already holds $forward_driver; it is no release before this change"
cp "$root/packages/daemon/$rollback_driver" "$work/base/packages/daemon/$rollback_driver"
go -C "$new_tree/packages/daemon" test -c -tags e2e -o "$work/new.test" ./internal/daemon ||
  fail "this change's driver did not build from $new_tree"
go -C "$work/base/packages/daemon" test -c -tags e2e -o "$work/base.test" ./internal/daemon ||
  fail "the release before's driver did not build from $base_sha"
note "built new.test from ${new_sha:-this checkout} and base.test from $base_sha, each with -tags e2e"
pass

# run_driver NAME DRIVER TEST runs the one test TEST of DRIVER over the run's database, project and
# broker, with the daemon's log in the evidence directory under NAME, and holds that it passed.
run_driver() {
  local name=$1 driver=$2 test_name=$3
  : >"$evidence/$name-daemon.log"
  LEGION_E2E_NATS_URL="nats://127.0.0.1:$port_nats" \
    LEGION_E2E_DAEMON_LOG="$evidence/$name-daemon.log" \
    LEGION_E2E_ROLLBACK_DSN="$database_url" \
    LEGION_E2E_ROLLBACK_PROJECT="$project" \
    TMPDIR="$work" \
    "$driver" -test.count=1 -test.timeout=20m -test.v -test.run "^$test_name\$" 2>&1 | tee "$evidence/$name.log" ||
    fail "the $name run failed; see $evidence/$name.log and $evidence/$name-daemon.log"
  # A driver that skipped, or whose -test.run matched nothing, exits 0 having proved nothing.
  grep -q "^--- PASS: $test_name" "$evidence/$name.log" ||
    fail "the $name run ran no step: it skipped, or -test.run matched no test; see $evidence/$name.log"
}

begin fixture
run_driver fixture "$work/base.test" TestARollbackFixtureIsRecordedByTheReleaseBeforeLive
pass

begin seed
run_driver seed "$work/new.test" TestARollbackSeedIsWrittenByTheNewerReleaseLive
pass

begin rollback
run_driver rollback "$work/base.test" TestARollbackDaemonLeavesTheNoticesItCannotDecodeLive
pass

begin "roll forward"
run_driver forward "$work/new.test" TestARolledForwardDaemonDeliversTheNoticesTheRollbackLeftLive
pass
