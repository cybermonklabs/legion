# shellcheck shell=bash
# The run a whole-daemon live proof makes around its driver: every entry script that sources
# this file differs from the others only in what its driver proves, so each is this run;
# merged-root-waits.sh and no-required-checks.sh have gone with the daemon's merge and its read of
# a base branch's required checks. Sourced by the entry script, never run. The entry script sets
# `set -euo pipefail` and
# root, the checkout it sources this file from, sets the run's guard once, at its head, with
# lib/proof-wait.sh's `proof_guard <seconds>` (which lib/rig.sh sources) and the reading of it
# beside the number, and calls
# live_daemon_proof as its last command.
# busy-relaunch-delivery.sh's negative control calls it instead in a subshell of its own whose root
# is a `git archive` of the base commit, so this run's EXIT trap is that subshell's and the script
# goes on to read the run's proof.log.
#
# live_daemon_proof LABEL SLUG TEST EVIDENCE runs four checks, each printing `== <check>`, what it
# observed and `ok <check> (<s>s)`; the first that fails prints `FAIL <check>: <why>` and ends the
# run non-zero. Exit 2 is an input of the caller's the run refuses. Exit 1 is every other failure.
# prerequisites wants LEGION_TEST_PG_DSN, the PostgreSQL the daemon migrates and runs on, which
# nothing here starts or prints, go, awk and python3, the libpq lib/loopback-dsn.py loads, and a
# nats-server: LEGION_E2E_NATS_BIN's, or the pinned one installed with `go install`. It refuses
# two inputs with exit 2, before the driver is built and before the broker starts: a
# LEGION_TEST_PG_DSN that is unset, and one whose host is not loopback, which
# lib/loopback-dsn.sh's require_loopback_dsn reads. Exit 2 covers one fault of the host as well:
# lib/loopback-dsn.py refuses every DSN where this machine has no libpq, the parser it asks which
# host a DSN names, and this file answers that refusal as it answers the other two. The refusal
# line tells that case apart, and names libpq and the package to install. The status does not:
# the helper answers every refusal with one status, so a status of its own here would need this
# file to read the helper's wording, or to ask for libpq a second time. broker starts that
# nats-server with JetStream on a port the kernel picks, and waits for it through
# lib/proof-wait.sh's wait_for, which gives up 30 s before the guard's deadline. driver builds
# the daemon package's `-tags e2e` test binary. proof runs the one test TEST against them, with
# the time left before the guard's deadline as the driver's -test.timeout, so its testwait waits
# give up when the shell's would, and holds that it passed.
#
# SLUG names the run's own files: the work directory legion-e2e-SLUG.<pid>.* under TMPDIR (or /tmp
# when it is unset), which holds the broker's store and the driver binary SLUG.test and is removed
# on any exit, and the evidence directory legion-e2e-SLUG-evidence.* beside it when EVIDENCE, the
# directory the caller named, is empty. The evidence directory is kept and printed; it holds the
# transcript (lib/transcript.sh), the nats-server's log, the daemon's own, which the driver writes
# through LEGION_E2E_DAEMON_LOG, and the driver's output. On any exit the nats-server is stopped and
# the run prints `LABEL e2e: PASS in <n>s (evidence: <dir>)`, or `LABEL e2e: FAIL at <check> after
# <n>s (evidence: <dir>)` on stderr.

# shellcheck source-path=SCRIPTDIR source=transcript.sh
. "$root/scripts/e2e/lib/transcript.sh"
# lib/rig.sh sources lib/proof-wait.sh, which holds proof_guard and wait_for, the run's guard and
# its one wait, and lib/loopback-dsn.sh, which holds require_loopback_dsn, the guard
# prerequisites reads LEGION_TEST_PG_DSN with. It reads this file's work, evidence, note and fail.
# shellcheck source-path=SCRIPTDIR source=rig.sh
. "$root/scripts/e2e/lib/rig.sh"

# require_dispatch_cli refuses a host where the proof's driver could not run the `dispatch` CLI,
# with exit 1 and a FAIL line of its own, before the run builds or starts anything: that CLI is
# this checkout's (packages/envoy-client/bin/dispatch.ts), which runs under bun out of the
# checkout's node_modules. It then exports LEGION_E2E_REPO_ROOT, the checkout the driver builds
# Dispatch from and runs the CLI out of, which the driver binary needs because it runs from the
# directory the caller ran the entry script in, not the package's. An entry script whose driver
# drives the CLI calls it after sourcing this file and before live_daemon_proof, so the refusal
# comes before the run's own checks.
require_dispatch_cli() {
  command -v bun >/dev/null || {
    echo "FAIL prerequisites: bun is not on PATH; the dispatch CLI the proof drives runs under it" >&2
    exit 1
  }
  [ -d "$root/node_modules" ] || {
    echo "FAIL prerequisites: $root/node_modules is missing; run \`bun install\` at the checkout's root first" >&2
    exit 1
  }
  export LEGION_E2E_REPO_ROOT="$root"
}

# The nats-server the run installs when the caller names none: the hub's v2.15.1, the version
# packages/envoy/go.mod pins and the daemon's own NATS tests run in their test process
# (packages/daemon/go.mod). The daemon refuses a nats-server below v2.11.12, v2.12.4 or v2.14.0
# (packages/envoy/natsversion).
nats_version=v2.15.1

# The run's state, global rather than live_daemon_proof's own because cleanup reads it from the
# EXIT trap, after live_daemon_proof has returned.
proof_label=''
evidence=''
work=''
driver=''
nats_pid=''
port_nats=''
check=setup
started=''
check_started=''

begin() { check=$1; check_started=$SECONDS; echo "== $check"; }
note() { echo "   $*"; }
pass() { echo "ok $check ($((SECONDS - check_started))s)"; }
# refuse is for an input of the caller's (exit 2); fail is for the box the run is on, or a step
# that did not hold (exit 1).
refuse() { echo "FAIL $check: $*" >&2; exit 2; }
fail() { echo "FAIL $check: $*" >&2; exit 1; }

cleanup() {
  local status=$?
  if [ -n "$nats_pid" ] && kill -0 "$nats_pid" 2>/dev/null; then
    kill -TERM "$nats_pid" 2>/dev/null || true
    wait "$nats_pid" 2>/dev/null || true
  fi
  rm -rf "$work"
  if [ "$status" -eq 0 ]; then
    echo "$proof_label e2e: PASS in $((SECONDS - started))s (evidence: $evidence)"
  else
    echo "$proof_label e2e: FAIL at $check after $((SECONDS - started))s (evidence: $evidence)" >&2
  fi
}

# nats_ready is the broker's readiness for wait_for: the server logs that it is ready once it
# listens, and a server that exited never will.
nats_ready() {
  grep -q 'Server is ready' "$evidence/nats.log" && return 0
  kill -0 "$nats_pid" 2>/dev/null || wait_impossible="the nats-server exited at startup; see $evidence/nats.log"
  return 1
}

live_daemon_proof() {
  local slug=$2 test_name=$3 tool nats_bin db_host
  proof_label=$1
  evidence=${4:-$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-$slug-evidence.XXXXXXXX")}
  mkdir -p "$evidence"
  transcript_to "$evidence/transcript.log"

  work=$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-$slug.$$.XXXXXXXX")
  driver=$work/$slug.test
  started=$SECONDS
  check_started=$SECONDS
  trap cleanup EXIT

  begin prerequisites
  [ -n "$proof_deadline_ms" ] ||
    fail "the entry script set no guard: it calls lib/proof-wait.sh's proof_guard <seconds> (through lib/rig.sh) before live_daemon_proof"
  [ -n "${LEGION_TEST_PG_DSN:-}" ] ||
    refuse "LEGION_TEST_PG_DSN is required: it names the PostgreSQL the daemon migrates and runs on (postgres://<user>@<host>:<port>/<database>?sslmode=disable). This script starts no database."
  for tool in go awk python3; do
    command -v "$tool" >/dev/null || fail "$tool is not on PATH"
  done
  # require_loopback_dsn in lib/loopback-dsn.sh says why a caller of the loopback guard clears
  # these three, and why the call comes after the tool check above.
  unset PGHOSTADDR PGSERVICE PGSERVICEFILE
  require_loopback_dsn db_host LEGION_TEST_PG_DSN "$LEGION_TEST_PG_DSN" "this run creates and drops a database there" ||
    refuse "$db_host"
  note "go $(go version | awk '{print $3}'), store LEGION_TEST_PG_DSN (set; not printed)"
  nats_bin=${LEGION_E2E_NATS_BIN:-}
  if [ -z "$nats_bin" ]; then
    note "LEGION_E2E_NATS_BIN names none; installing nats-server $nats_version into this run's own GOBIN"
    # The toolchain's own go, under its own GOROOT. mise's go shim replaces a caller's GOBIN when
    # go.set_gobin is true, and, on mise before 2026.9.0, in any shell `mise activate` set up.
    # Separately, an activated shell exports mise's Go's GOROOT, whose compiler a newer toolchain's
    # go refuses.
    local goroot
    goroot=$(go env GOROOT) &&
      GOBIN="$work/bin" GOROOT=$goroot "$goroot/bin/go" install "github.com/nats-io/nats-server/v2@$nats_version" ||
      fail "could not install nats-server $nats_version; name a binary in LEGION_E2E_NATS_BIN instead"
    nats_bin=$work/bin/nats-server
  fi
  [ -x "$nats_bin" ] || fail "no executable nats-server at $nats_bin"
  note "nats-server $("$nats_bin" --version)"
  pass

  begin broker
  mkdir -p "$work/nats-store"
  # Both logs are this run's, whatever an evidence directory a caller named already holds: the port
  # below is read back out of the broker's.
  : >"$evidence/nats.log"
  : >"$evidence/daemon.log"
  # The port is the kernel's own pick (`-p -1`), read back from the line the server logs once it is
  # listening: the server binds it before anything else can take it, which no free-port pick can
  # promise, and a pod with no `ss` runs the proof all the same.
  "$nats_bin" -js -a 127.0.0.1 -p -1 -sd "$work/nats-store" >>"$evidence/nats.log" 2>&1 &
  nats_pid=$!
  wait_for "the nats-server to be ready" nats_ready
  port_nats=$(awk -F: '/Listening for client connections on/ {print $NF; exit}' "$evidence/nats.log")
  [ -n "$port_nats" ] || fail "the nats-server logged no client port; see $evidence/nats.log"
  note "JetStream on nats://127.0.0.1:$port_nats, store $work/nats-store, log $evidence/nats.log"
  pass

  begin driver
  go -C "$root/packages/daemon" test -c -tags e2e -o "$driver" ./internal/daemon ||
    fail "the driver did not build"
  note "built $(basename "$driver") from $root/packages/daemon with -tags e2e"
  pass

  begin proof
  # The driver's steps print their own `== <name>` / `ok <name>` pair between this check's, so the
  # run's checks and the proof's read alike. The daemon's own log goes to the evidence directory
  # rather than into them.
  LEGION_E2E_NATS_URL="nats://127.0.0.1:$port_nats" \
    LEGION_E2E_DAEMON_LOG="$evidence/daemon.log" \
    TMPDIR="$work" \
    "$driver" -test.count=1 -test.timeout="$(proof_seconds_left)s" -test.v \
    -test.run "^$test_name\$" 2>&1 | tee "$evidence/proof.log" ||
    fail "the proof's driver failed; see $evidence/proof.log and $evidence/daemon.log"
  # A driver that skipped, or whose -test.run matched nothing, exits 0 having proved nothing.
  grep -q "^--- PASS: $test_name" "$evidence/proof.log" ||
    fail "the proof's driver ran no step: it skipped, or -test.run matched no test; see $evidence/proof.log"
  pass
}
