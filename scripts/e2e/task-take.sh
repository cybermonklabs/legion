#!/usr/bin/env bash
# The live proof that a task is a value the worker takes, not a prompt the daemon pushes
# (dispatch://INFRA-2811's R3 and R4, its check c8), on a real NATS through a real Envoy listener.
# The script builds this checkout's listener (packages/envoy/cmd/listener) and runs it on the
# run's nats-server, with consumer delivery, on a port the kernel picks (lib/held-port.py, as
# envoy-session-delivery.sh starts one); the driver
# (packages/daemon/internal/daemon/task_take_live_test.go, which this script builds with
# `-tags e2e`) boots the whole daemon with its own task bucket on that nats-server and the
# PostgreSQL LEGION_TEST_PG_DSN names, and drives the claim's worker as its pod would: each process
# says hello on the daemon's worker stream, registers its session with the listener on its own
# consumer, subscribes the relay subject the listener publishes that session's frames on, registers
# with the daemon, claims its role on the listener and takes its task through the daemon's routes.
# Each check prints `== <name>`, what it observed and `ok <name>`, the driver's steps quoting the
# daemon's own lines; the first that fails ends the run non-zero, naming it:
#   1. a claim is given a task and the driver sees its value relayed — the negative control,
#      asserted first: against the base, which writes no value and relays none, the run fails here;
#   2. the worker takes the number, starts and ends its turn, and the daemon's lines name the number
#      taken and finished;
#   3. its process is killed mid-turn, and its next registration takes the same number, once;
#   4. a newer task written meanwhile (the relaunched worker moves its phase back, retro to
#      implementing, while the killed turn's task is owed) is taken after it, under the next number;
#   5. the daemon's idle rule releases the issue's pod, a role message nobody holds is the
#      listener's wake, and the wake's task is taken once across the relaunch it causes;
#   6. after a daemon restart the finished number is not taken again;
#   7. a claim whose task is finished and whose phase waits registers again as a new process, and
#      no value is written: driven through an idle release cancelled after its role stop.
#
# Run it from a checkout on a Linux host with go, awk and python3, as
#   LEGION_TEST_PG_DSN=postgres://<user>@127.0.0.1:<port>/<database>?sslmode=disable \
#     bash scripts/e2e/task-take.sh
# LEGION_TEST_PG_DSN is required and names the PostgreSQL the daemon migrates and runs on; the run
# creates a database of its own there. That server must be this machine's: prerequisites reads the
# DSN with lib/loopback-dsn.sh's require_loopback_dsn and refuses a host that is not 127.0.0.1,
# localhost or ::1 with exit 2, before it builds or starts anything. This script starts no
# database and no container. The broker is the run's own: a nats-server from LEGION_E2E_NATS_BIN,
# or the pinned one installed with `go install`; the listener is the run's own too, built from this
# checkout and stopped on any exit. TASK_TAKE_EVIDENCE_DIR (default a fresh directory under TMPDIR,
# or /tmp when it is unset, kept and printed) keeps the transcript, the nats-server's log, the
# listener's (listener.log), the daemon's and the driver's output. Nothing reaches github.com or a
# deployed Dispatch: the run is self-contained. A measured run in a Legion pod took 76 s with a warm
# build cache, 29 s of it the proof itself and 27 s installing the pinned nats-server; a cold
# runner adds the listener's and the driver's compile. The run around the driver is
# lib/live-daemon-proof.sh's.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
# shellcheck source-path=SCRIPTDIR source=lib/live-daemon-proof.sh
. "$root/scripts/e2e/lib/live-daemon-proof.sh"
# shellcheck source-path=SCRIPTDIR source=lib/port-file.sh
. "$root/scripts/e2e/lib/port-file.sh"

# listener_healthy URL: the listener at URL answers /healthz healthy within two seconds. It asks
# with python3, which prerequisites already holds the run to, rather than a curl the run would
# otherwise need.
listener_healthy() {
  python3 - "$1" <<'PY'
import json, sys, urllib.request
try:
    with urllib.request.urlopen(sys.argv[1] + "/healthz", timeout=2) as answer:
        sys.exit(0 if json.load(answer).get("status") == "healthy" else 1)
except Exception:
    sys.exit(1)
PY
}

# start_listener is the run's services (lib/live-daemon-proof.sh's SERVICES): it builds the
# listener and runs it on the broker, with machine id task-take and a HOME of its own, serving the
# socket lib/held-port.py holds for it on a port the kernel picked (ENVOY_LISTEN_FD), waits for its
# /healthz to answer healthy, and names it to the driver in LEGION_E2E_ENVOY_URL. The listener gives
# its sessions consumer delivery whatever it is told, and the run's NATS user is the server's
# anonymous one, which holds the session consumers' calls and the task bucket's reads.
start_listener() {
  local listener_pid listener_port listener_url waited=0
  begin listener
  go -C "$root/packages/envoy" build -o "$work/envoy-listener" ./cmd/listener ||
    fail "the Envoy listener did not build"
  note "built envoy-listener from $root/packages/envoy/cmd/listener"
  : >"$evidence/listener.log"
  mkdir -p "$work/listener-home"
  HOME="$work/listener-home" ENVOY_LISTEN_HOST=127.0.0.1 ENVOY_MACHINE_ID=task-take \
    NATS_URLS="nats://127.0.0.1:$port_nats" \
    python3 "$root/scripts/e2e/lib/held-port.py" exec --port-file "$work/listener.port" \
    --fd-env ENVOY_LISTEN_FD --env PORT={port} -- "$work/envoy-listener" >>"$evidence/listener.log" 2>&1 &
  listener_pid=$!
  service_pids="$service_pids $listener_pid"
  read_port_file "$work/listener.port" "$listener_pid" listener_port || waited=$?
  case $waited in
    0) ;;
    1) fail "the Envoy listener exited before it held a port; see $evidence/listener.log" ;;
    *) fail "the Envoy listener held no port within ${port_file_seconds}s; see $evidence/listener.log" ;;
  esac
  listener_url="http://127.0.0.1:$listener_port"
  for _ in $(seq 1 120); do
    listener_healthy "$listener_url" && break
    kill -0 "$listener_pid" 2>/dev/null || fail "the Envoy listener exited at startup; see $evidence/listener.log"
    sleep 0.5
  done
  listener_healthy "$listener_url" ||
    fail "the Envoy listener did not answer /healthz healthy within 60s; see $evidence/listener.log"
  export LEGION_E2E_ENVOY_URL="$listener_url"
  note "Envoy listener on $listener_url, machine id task-take, NATS nats://127.0.0.1:$port_nats, log $evidence/listener.log"
  pass
}

live_daemon_proof 'task take' task-take TestATaskIsTakenOnceOnARealNATSLive \
  "${TASK_TAKE_EVIDENCE_DIR:-}" start_listener
