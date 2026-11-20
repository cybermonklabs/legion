# shellcheck shell=bash
# The run a whole-daemon live proof makes around its driver: every entry script that sources
# this file differs from the others only in what its driver proves, so each is this run;
# merged-root-waits.sh and no-required-checks.sh have gone with the daemon's merge and its read of
# a base branch's required checks. Sourced by the entry script, never run. The entry script sets
# `set -euo pipefail` and
# root, the checkout it sources this file from, and calls live_daemon_proof as its last command.
# busy-relaunch-delivery.sh's negative control calls it instead in a subshell of its own whose root
# is a `git archive` of the base commit, so this run's EXIT trap is that subshell's and the script
# goes on to read the run's proof.log.
#
# live_daemon_proof LABEL SLUG TEST EVIDENCE runs three checks, each printing `== <check>`, what it
# observed and `ok <check> (<s>s)`; the first that fails prints `FAIL <check>: <why>` and ends the
# run non-zero. Exit 2 is an input of the caller's the run refuses. Exit 1 is every other failure.
# prerequisites wants LEGION_TEST_PG_DSN, the PostgreSQL the daemon migrates and runs on, which
# nothing here starts or prints, go, awk and python3, the libpq lib/loopback-dsn.py loads, a way
# for .github/scripts/private_proc.py to make the driver's namespace (an unprivileged user
# namespace, as a Legion pod gives one, or `sudo -n` where the kernel refuses that: the wrapper's
# --check), and a nats-server: LEGION_E2E_NATS_BIN's, or the pinned
# one installed with `go install`. It refuses
# two inputs with exit 2, before the driver is built and before the broker starts: a
# LEGION_TEST_PG_DSN that is unset, and one whose host is not loopback, which
# lib/loopback-dsn.sh's require_loopback_dsn reads. Exit 2 covers one fault of the host as well:
# lib/loopback-dsn.py refuses every DSN where this machine has no libpq, the parser it asks which
# host a DSN names, and this file answers that refusal as it answers the other two. The refusal
# line tells that case apart, and names libpq and the package to install. The status does not:
# the helper answers every refusal with one status, so a status of its own here would need this
# file to read the helper's wording, or to ask for libpq a second time. broker starts that
# nats-server with JetStream on a port the kernel picks. proof builds the daemon package's
# `-tags e2e` test binary, the driver, and runs its one test TEST against them, in one go test
# through .github/scripts/private_proc.py as CI runs every daemon test binary: go runs the driver
# beneath a supervisor at PID 1 of a PID and mount namespace of its own, whose /proc lists the
# driver's processes and none of the machine's, and a driver that does not build fails proof. The
# wrapper is this checkout's, the one beside this file, whatever root the caller set. proof holds
# that the test passed.
#
# SLUG names the run's own files: the work directory legion-e2e-SLUG.<pid>.* under TMPDIR (or /tmp
# when it is unset), which holds the broker's store and go's build of the driver, and is removed
# on any exit, and the evidence directory legion-e2e-SLUG-evidence.* beside it when EVIDENCE, the
# directory the caller named, is empty. The evidence directory is kept and printed; it holds the
# transcript (lib/transcript.sh), the nats-server's log, the daemon's own, which the driver writes
# through LEGION_E2E_DAEMON_LOG, and the driver's output with the wrapper's. On any exit the
# nats-server is stopped and
# the run prints `LABEL e2e: PASS in <n>s (evidence: <dir>)`, or `LABEL e2e: FAIL at <check> after
# <n>s (evidence: <dir>)` on stderr.

# shellcheck source-path=SCRIPTDIR source=transcript.sh
. "$root/scripts/e2e/lib/transcript.sh"
# lib/loopback-dsn.sh holds require_loopback_dsn, the guard prerequisites reads LEGION_TEST_PG_DSN
# with. It needs nothing of this file's.
# shellcheck source-path=SCRIPTDIR source=loopback-dsn.sh
. "$root/scripts/e2e/lib/loopback-dsn.sh"

# require_dispatch_cli refuses a host where the proof's driver could not run the `dispatch` CLI,
# with exit 1 and a FAIL line of its own, before the run builds or starts anything: that CLI is
# this checkout's (packages/envoy-client/bin/dispatch.ts), which runs under bun out of the
# checkout's node_modules. It then exports LEGION_E2E_REPO_ROOT, the checkout the driver builds
# Dispatch from and runs the CLI out of. The driver runs in its package's directory, where go test
# runs a test binary, and reads the checkout from that variable (internal/testbuild's RootEnv)
# rather than from the directory it runs in. An entry script whose driver drives the CLI calls it
# after sourcing this file and before live_daemon_proof, so the refusal comes before the run's own
# checks.
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
# packages/envoy/go.mod pins. The daemon refuses a nats-server below v2.11.12, v2.12.4 or v2.14.0
# (packages/envoy/natsversion).
nats_version=v2.15.1

# The wrapper proof runs the driver through, read beside this file when it is sourced rather than
# under root: busy-relaunch-delivery.sh's negative control sets root to a `git archive` of a commit
# that may predate it.
proof_wrapper="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)/.github/scripts/private_proc.py"

# The run's state, global rather than live_daemon_proof's own because cleanup reads it from the
# EXIT trap, after live_daemon_proof has returned.
proof_label=''
evidence=''
work=''
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

live_daemon_proof() {
  local slug=$2 test_name=$3 tool nats_bin db_host
  proof_label=$1
  evidence=${4:-$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-$slug-evidence.XXXXXXXX")}
  mkdir -p "$evidence"
  transcript_to "$evidence/transcript.log"

  work=$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-$slug.$$.XXXXXXXX")
  started=$SECONDS
  check_started=$SECONDS
  trap cleanup EXIT

  begin prerequisites
  [ -n "${LEGION_TEST_PG_DSN:-}" ] ||
    refuse "LEGION_TEST_PG_DSN is required: it names the PostgreSQL the daemon migrates and runs on (postgres://<user>@<host>:<port>/<database>?sslmode=disable). This script starts no database."
  for tool in go awk python3; do
    command -v "$tool" >/dev/null || fail "$tool is not on PATH"
  done
  [ -x "$proof_wrapper" ] || fail "no executable $proof_wrapper, which the proof runs its driver through"
  # The wrapper makes the driver's PID and mount namespace as an unprivileged user namespace where
  # the kernel allows one, as a Legion pod's does, and with `sudo -n` where it refuses one; --check
  # says which, or why neither can, before anything is built or started.
  local namespace
  namespace=$("$proof_wrapper" --check 2>&1) ||
    fail "the driver cannot have a /proc of its own, which the proof runs it in: ${namespace#private-proc: }"
  note "${namespace#private-proc: }"
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
  for _ in $(seq 1 120); do
    grep -q 'Server is ready' "$evidence/nats.log" && break
    kill -0 "$nats_pid" 2>/dev/null || fail "the nats-server exited at startup; see $evidence/nats.log"
    sleep 0.5
  done
  grep -q 'Server is ready' "$evidence/nats.log" || fail "the nats-server was not ready within 60s; see $evidence/nats.log"
  port_nats=$(awk -F: '/Listening for client connections on/ {print $NF; exit}' "$evidence/nats.log")
  [ -n "$port_nats" ] || fail "the nats-server logged no client port; see $evidence/nats.log"
  note "JetStream on nats://127.0.0.1:$port_nats, store $work/nats-store, log $evidence/nats.log"
  pass

  begin proof
  # go test builds the driver and runs it in its package's directory, beneath the wrapper's
  # supervisor, so a driver that does not build fails this check. The driver's steps print their
  # own `== <name>` / `ok <name>` pair between this check's, so the run's checks and the proof's
  # read alike. The daemon's own log goes to the evidence directory rather than into them. The
  # wrapper's own lines (`private-proc: ...`: the way it made the namespace, the tests that read a
  # process table, and how many binaries ran in a namespace of their own) end the proof's log.
  (cd "$root/packages/daemon" &&
    LEGION_E2E_NATS_URL="nats://127.0.0.1:$port_nats" \
      LEGION_E2E_DAEMON_LOG="$evidence/daemon.log" \
      TMPDIR="$work" \
      "$proof_wrapper" go test -tags e2e -count=1 -timeout 20m -v \
      -run "^$test_name\$" ./internal/daemon) 2>&1 | tee "$evidence/proof.log" ||
    fail "the proof's driver did not build, or failed; see $evidence/proof.log and $evidence/daemon.log"
  # A driver that skipped, or whose -test.run matched nothing, exits 0 having proved nothing.
  grep -q "^--- PASS: $test_name" "$evidence/proof.log" ||
    fail "the proof's driver ran no step: it skipped, or -test.run matched no test; see $evidence/proof.log"
  pass
}
