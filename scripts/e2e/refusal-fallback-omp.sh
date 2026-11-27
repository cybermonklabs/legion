#!/usr/bin/env bash
# The live proof of a refusal's cooldown and a rate limit's suppression, using four real Legion
# panes of the pinned Oh My Pi. Each loads this checkout's two plugins and talks to its own
# loopback OpenAI-shaped stand-in. The profile walks from strong-model to weak-model with
# retry.fallbackRevertPolicy: never and retry.rateLimitWalksChain: true.
#
# A content-filter refusal must move the same session to the next model until the plugin's
# cooldown ends, so this proof measures both switches in a long turn.
#
# The two content-filter panes differ in whether text streamed before the refusal. One lets
# the host walk its chain; the other requires the plugin to walk it. Both stay off the strong
# model between minutes five and ten, and return after the ten-minute plugin cooldown.
#
# The rate-limited pane answers 429 with retry-after: 60. At the pinned Oh My Pi the transport
# retries that answer until its ten-minute first-event budget ends. The host then sees a timeout
# with no retry-after, walks the chain and suppresses the strong model for its five-minute floor.
# The plugin must return within one request's cadence after that suppression, before its own
# ten-minute cooldown would end. The rate-limited-final pane adds x-should-retry: false, so the
# host sees the 429 and suppresses for sixty seconds. Its requests are ten seconds apart: a
# plugin that ignores suppression then produces a strong-model request before sixty seconds.
# The driver also reads the model_change timestamps to bound the switch itself, because the
# host chooses each request's model before the plugin's context hook can switch it.
#
# The four panes run together for twenty-one minutes, long enough for the retried 429's timeout,
# suppression and return. A failed expectation does not stop the others' measurements. Each
# prints its name, measurement and ok or FAIL; any failure makes the script exit non-zero.
#
# Run from a Linux checkout after bun install:
#   LEGION_OMP_PATH=/opt/omp/bin/omp bash scripts/e2e/refusal-fallback-omp.sh
# LEGION_OMP_PATH names the absolute executable every pane runs; the script prints its version.
# The run needs bash, bun and go, which builds its own JetStream server, and no PostgreSQL,
# Docker, kube context or tmux. All model, daemon, listener and NATS endpoints are loopback.
# REFUSAL_FALLBACK_EVIDENCE_DIR defaults to a fresh directory under TMPDIR (or /tmp). It keeps
# the transcript, NATS log and each pane's Oh My Pi transcript. The run removes only its own
# scratch directory and stops its own children; the evidence directory remains.
set -euo pipefail

# The cooldown is ten minutes. Drive beyond the retried 429's ten-minute transport timeout and
# five-minute host suppression, with room to read the request after the return.
cooldown_ms=600000
drive_ms=1260000

root=$(cd "$(dirname "$0")/../.." && pwd)
if [ -z "${LEGION_OMP_PATH:-}" ]; then
  echo "FAIL setup: LEGION_OMP_PATH is required: it names the Oh My Pi every pane of this proof runs (an absolute executable path). This proof launches real agents and guesses no build." >&2
  exit 1
fi
if [ ! -x "$LEGION_OMP_PATH" ]; then
  echo "FAIL setup: LEGION_OMP_PATH ($LEGION_OMP_PATH) is not an executable file" >&2
  exit 1
fi
export LEGION_OMP_PATH

started=$SECONDS
check=setup
check_started=$SECONDS

begin() {
  check=$1
  check_started=$SECONDS
  echo "== $check"
}
note() { echo "   $*"; }
pass() { echo "ok $check ($((SECONDS - check_started))s)"; }
fail() {
  echo "FAIL $check: $*" >&2
  exit 1
}

evidence=${REFUSAL_FALLBACK_EVIDENCE_DIR:-}
if [ -z "$evidence" ]; then
  evidence=$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-refusal-fallback-evidence.XXXXXXXX")
fi
# logs/ is the rig's own: start_process redirects each service it starts to $evidence/logs/<name>.log
# and creates no directory for it, so a run whose evidence directory holds none dies at the first
# service with "No such file or directory" rather than starting it (lib/rig.sh, start_process).
mkdir -p "$evidence/logs"

# shellcheck source-path=SCRIPTDIR source=lib/transcript.sh
. "$root/scripts/e2e/lib/transcript.sh"
transcript_to "$evidence/transcript.log"
work=$(mktemp -d "${TMPDIR:-/tmp}/legion-e2e-refusal-fallback.XXXXXXXX")
timeout_hook=
# shellcheck source-path=SCRIPTDIR source=lib/rig.sh
. "$root/scripts/e2e/lib/rig.sh"
# shellcheck source-path=SCRIPTDIR source=lib/run-verdict.sh
. "$root/scripts/e2e/lib/run-verdict.sh"

# cleanup prints how the run ended through lib/run-verdict.sh, which exits with rc: the script's own
# status for EXIT, 128 plus the signal's number for INT and TERM. PASS needs rc 0 and run_end, the
# script's last statement, so a run that a HUP ended, whose EXIT trap reads 0, reads FAIL as an
# interrupted run does. The panes are the driver's own children and it stops them itself. natsd is
# this script's own, started under start_process's `set -m` in a process group of its own, so
# nothing else ends it: it is stopped by the pid start_process recorded, and `$work`, which holds
# only the natsd binary this run built and its JetStream store, goes with it. The evidence
# directory is not touched: every log, the transcript and each pane's Oh My Pi transcript live
# there and are the run's record.
cleanup() {
  local rc=$1
  trap - EXIT INT TERM
  stop_pid "${nats_pid:-}"
  [ -z "${work:-}" ] || rm -rf "${work:?}"
  run_verdict "$rc" \
    "refusal fallback e2e: PASS in $((SECONDS - started))s (evidence: $evidence)" \
    "refusal fallback e2e: FAIL at $check after $((SECONDS - started))s (evidence: $evidence)"
}
trap 'cleanup "$?"' EXIT
trap 'cleanup 130' INT
trap 'cleanup 143' TERM

begin prerequisites
command -v bun >/dev/null || fail "bun is not on PATH: the driver is a bun program"
driver=$root/packages/pi-legion/scripts/refusal-fallback-pane.ts
[ -f "$driver" ] || fail "$driver is missing"
[ -d "$root/packages/pi-legion/node_modules/@legion/pi-shared" ] ||
  fail "packages/pi-legion/node_modules holds no @legion/pi-shared: run bun install in this checkout"
[ -d "$root/packages/pi-shared/node_modules" ] ||
  fail "packages/pi-shared/node_modules is missing: run bun install in this checkout"
omp_build=$("$LEGION_OMP_PATH" --version 2>&1) || fail "$LEGION_OMP_PATH --version failed: $omp_build"
note "Oh My Pi ${omp_build%%$'\n'*} at $LEGION_OMP_PATH"
note "cooldown ${cooldown_ms}ms, each pane driven for ${drive_ms}ms"
note "evidence directory $evidence"
pass

begin nats
# A pane's Envoy extension is given no consumer by a listener it cannot reach a NATS server
# through, and a session with no consumer ends at its claim boot (INFRA-771), so the run starts a
# JetStream server of its own, as the delivery proofs do. The port is the kernel's pick (`-p -1`),
# read back from the line the server logs once it listens.
command -v go >/dev/null || fail "go is not on PATH: the run builds its own natsd"
go -C "$root/packages/envoy" build -o "$work/bin/natsd" ./internal/testnats/cmd/natsd ||
  fail "./internal/testnats/cmd/natsd did not build"
start_process nats "$work/bin/natsd" -p -1 -store_dir "$work/nats-store"
await_start nats "$nats_pid" 60 "natsd to be ready" grep -q 'Server is ready' "$evidence/logs/nats.log"
nats_port=$(report_port "$evidence/logs/nats.log" 'Listening for client connections on [^ ]*:') ||
  fail "natsd logged no client port; see $evidence/logs/nats.log"
REFUSAL_FALLBACK_NATS_URL=nats://127.0.0.1:$nats_port
export REFUSAL_FALLBACK_NATS_URL
note "JetStream on $REFUSAL_FALLBACK_NATS_URL"
pass

# The driver prints a section per expectation of its own, as verification-results.sh's does, so this
# check names the run rather than opening one.
check=runs
status=0
bun "$driver" "$cooldown_ms" "$drive_ms" "$evidence" || status=$?
case "$status" in
  0) ;;
  1) fail "an expectation failed (above)" ;;
  *) fail "the driver stopped before its expectations (status $status)" ;;
esac
run_end
