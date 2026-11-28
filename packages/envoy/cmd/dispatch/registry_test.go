package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// bootProbeDialDelay applies only to connections of the health probe's pool. Boot pays it once,
// on that pool's cold dial. Three seconds exceeds the warm probe's two-second bound
// (store.healthProbeTimeout), so the cold dial needs the boot probe's own bound.
const bootProbeDialDelay = 3 * time.Second

// bootProbeDeadline is the failure CI showed when the boot's probe ran out of the warm probe's
// bound - "register this server process: probe the database: context deadline exceeded"
// (dispatch://INFRA-985, 1 run in 13 at a Postgres with max_connections 400 and 4 in 16 at 100)
// - written as the binary writes it, which is slog's message and the error it wraps as two
// fields rather than one joined string.
const bootProbeDeadline = `msg="dispatch: register this server process" ` +
	`error="probe the database: context deadline exceeded"`

// registryRefusalLine is what a process logs when a live server process refuses its signing keys
// and it waits for that process's heartbeat to lapse (processes.Register).
const registryRefusalLine = "a live server process refuses this one's signing keys; waiting for its heartbeat to lapse"

// The boot's one probe of the database outlasts the bound /healthz's probe reads within, and it
// leaves that pool warm for the first /healthz a prober makes. Why that read is the one that is
// always cold, and why the warm bound is no bound for it, is on store.Pool.HealthyWithin; the
// boot's own bound is bootProbeTimeout, thirty seconds. The proxy delays only the health pool's
// dial. The process must still register, serve, and answer that first /healthz 200.
//
// The listening line comes after joinRegistry. Narrow the boot probe's bound to two seconds and
// the process exits 1 before it serves anything. The wait fails with the whole output and the
// hint below names the boot probe's deadline failure.
//
// The test reads /healthz once. Delete the boot probe and this read pays the three-second dial
// inside the two-second warm bound, so it answers 503 with db false. A poll could see a later
// 200 and hide that failure, so the test also checks for failed health probes in the log.
// The delayed-dial count at the listening line proves boot paid the delay, not this request.
// The registration line names the step the process reached for a reader of a later failure.
func TestTheBootProbeOutlastsTheWarmProbesBoundAndLeavesTheFirstHealthzWarm(t *testing.T) {
	t.Parallel()
	migrated := storetest.Open(t)
	dsn, err := url.Parse(migrated.Pool.Config().ConnString())
	if err != nil {
		t.Fatalf("parse the test database URL: %v", err)
	}
	host, dials := slowPostgres(t, dsn.Host, bootProbeDialDelay)
	dsn.Host = host
	defer func() {
		if count := dials.sslRequests.Load(); count != 0 {
			t.Errorf("the slow Postgres proxy received %d SSLRequest(s), which hide the startup message; use sslmode=disable", count)
		}
	}()

	process := startDispatchProcessWith(t, dsn.String(), dispatchOptions{})
	// A probe that ran out of its bound exits the process with 1 before it serves anything, so
	// the wait below fails with an output holding bootProbeDeadline: this names that failure,
	// rather than leaving the reader to find the line.
	defer func() {
		if t.Failed() && strings.Contains(process.Output.String(), bootProbeDeadline) {
			t.Logf("the boot probe ran out of its bound at a dial of %s", bootProbeDialDelay)
		}
	}()
	process.awaitLog(t, process.listeningLine())
	delayedAtBoot := dials.delayed.Load()

	status, health := process.healthUnder(t, testwait.Context(t))
	if status != http.StatusOK || !health.DB {
		t.Fatalf("the first /healthz answered %d with db %t, want 200 with db true: the boot's probe opened the "+
			"health pool, so this read paid no dial; a 503 says this read was that pool's first and paid the "+
			"%s dial inside store.healthProbeTimeout's two seconds:\n%s",
			status, health.DB, bootProbeDialDelay, process.Output.String())
	}
	if failed := process.logLines("dispatch: health probe failed"); len(failed) > 0 {
		t.Fatalf("a probe of the health pool failed, so the 200 above came after a cold read rather than from a "+
			"warm pool:\n%s", strings.Join(failed, "\n"))
	}
	if delayedAtBoot == 0 {
		t.Fatal("the slow Postgres proxy delayed no dials before Dispatch started listening; want at least one dial named dispatch-health")
	}
	process.WaitForOutput(t, "dispatch: registered this server process")
}

// A process refused by a live peer waits, before it serves anything, for the peer's heartbeat to
// lapse (processes.Register), up to a live window and a heartbeat. A deploy that stops it in that
// wait stops it cleanly: SIGTERM ends the wait, the process logs the stop at INFO and exits 0,
// and no supervisor reads the signal it sent as a failed start. The peer here is a running
// process with a key set of its own, so its row keeps heartbeating and the wait does not end on
// its own before the signal.
func TestAProcessStoppedWhileWaitingForAdmissionExitsCleanly(t *testing.T) {
	t.Parallel()
	// Two Dispatch processes at once: the live peer, and the one waiting for admission beside it.
	dispatchSlots.reserve(t, 2)
	database := storetest.Open(t)
	peer := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{SigningKey: "the-live-peers-key"})
	peer.waitHealthy(t)

	waiting := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{SigningKey: "a-key-the-peer-does-not-share"})
	waiting.WaitForOutput(t, registryRefusalLine)
	waiting.Terminate(t)
	waiting.WaitExit(t, "SIGTERM")

	output := waiting.Output.String()
	if code := waiting.Cmd.ProcessState.ExitCode(); code != 0 {
		t.Fatalf("a process stopped while waiting for admission exited %d, want 0:\n%s", code, output)
	}
	if !strings.Contains(output, `level=INFO msg="dispatch: stopped while waiting for admission to the registry of live server processes"`) {
		t.Fatalf("a process stopped while waiting for admission did not log the stop at INFO:\n%s", output)
	}
}
