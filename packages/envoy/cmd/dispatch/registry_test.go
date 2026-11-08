package main

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// bootProbeDialDelay is how late slowPostgres lets each of the process's connections reach
// Postgres in the row below. It is half again store.healthProbeTimeout's two seconds, so the
// boot's own cold dial cannot fit inside that warm bound however the machine is loaded, and the
// boot pays it three times over: measured at this head through a counting proxy, over three runs,
// the connections arrive in three rounds, each round waiting on the one before it. The shared
// pool's one connection comes first, at 0.03-0.04 s, and carries every sequential boot step; ten
// more open together once it is up, at 3.50-3.59 s; seven or eight more once those are up, at
// 6.55-6.68 s. How many connections that comes to is no invariant, and nothing here rests on it:
// it was 18, 18 and 19, all of them before the first /healthz 200, which came 9.64, 9.70 and
// 9.76 s after the process started. publishDashboard, the one step bounded tightly enough to care
// (dashboardPublishTimeout, 60 s, and it exits 1), opens no connection of its own - none reaches
// the proxy while it runs - and took 11, 11 and 16 ms in those runs, so a delay charged from the
// first connection never reaches it.
const bootProbeDialDelay = 3 * time.Second

// bootProbeDeadline is the failure CI showed when the boot's probe ran out of the warm probe's
// bound - "register this server process: probe the database: context deadline exceeded"
// (dispatch://INFRA-985, 1 run in 13 at a Postgres with max_connections 400 and 4 in 16 at 100)
// - written as the binary writes it, which is slog's message and the error it wraps as two
// fields rather than one joined string.
const bootProbeDeadline = `msg="dispatch: register this server process" ` +
	`error="probe the database: context deadline exceeded"`

// The boot's one probe of the database outlasts the bound /healthz's probe reads within, and it
// leaves that pool warm for the first /healthz a prober makes. Why that read is the one that is
// always cold, and why the warm bound is no bound for it, is on store.Pool.HealthyWithin; the
// boot's own bound is bootProbeTimeout, thirty seconds. Here every connection reaches Postgres
// bootProbeDialDelay late - three seconds, half again store.healthProbeTimeout's two - which is
// past the warm bound, and the process must still register, serve, and answer that first
// /healthz 200.
//
// Each step below reddens its own way. Step two reads the listening line, which comes after
// joinRegistry, so a boot that died at the probe - or at any step before it, every one of which
// dials through the same slow proxy - never reaches it: narrow the probe's bound to Healthy's
// two seconds and the process exits 1 before it serves anything, and this step fails with
// bootProbeDeadline. Step three reads /healthz once, never polling, since a poll hides the
// defect this row is here for: delete the probe and the health pool's first read is that
// handler's own, which pays the three-second dial inside the two-second warm bound, so it
// answers 503 with db false. Step four requires that no probe of that pool failed, so a 200 that
// followed a failed probe cannot pass as a warm read. Step five reads the registration line for
// the log it names, so a reader of a later failure sees which step the boot got past.
func TestTheBootProbeOutlastsTheWarmProbesBoundAndLeavesTheFirstHealthzWarm(t *testing.T) {
	migrated := storetest.Open(t)
	dsn, err := url.Parse(migrated.Pool.Config().ConnString())
	if err != nil {
		t.Fatalf("parse the test database URL: %v", err)
	}
	dsn.Host = slowPostgres(t, dsn.Host, bootProbeDialDelay)

	process := startDispatchProcessWith(t, dsn.String(), dispatchOptions{})
	// waitHealthy's own 30 s is a budget this boot has no reason to meet: it pays
	// bootProbeDialDelay once for each of the three rounds of connections it opens, each round
	// waiting on the one before it, and every other step it takes is as slow as the machine is.
	// So the row waits on a hang guard instead, far from the 9.6-9.8 s the boot took here, which
	// fails only a process that never serves at all.
	listening := `msg="dispatch: listening" addr=127.0.0.1:` + strconv.Itoa(process.port)
	hangGuard := 2 * time.Minute
	deadline := time.Now().Add(hangGuard)
serving:
	for !process.logged(listening) {
		select {
		case <-process.Exited:
			// Exited closes only once the output is whole, so these reads are final.
			if process.logged(listening) {
				break serving
			}
			// A probe that ran out of its bound exits the process with 1 before it serves
			// anything, so this is where that failure arrives: name it, rather than leaving the
			// reader to find the line in the output.
			if strings.Contains(process.Output.String(), bootProbeDeadline) {
				t.Fatalf("the boot probe ran out of its bound at a dial of %s:\n%s",
					bootProbeDialDelay, process.Output.String())
			}
			t.Fatalf("Dispatch exited before it served anything:\n%s", process.Output.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("Dispatch did not log that it serves within %v:\n%s", hangGuard, process.Output.String())
		}
		time.Sleep(50 * time.Millisecond)
	}

	status, health := process.health(t)
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
	process.WaitForOutput(t, "dispatch: registered this server process")
}

// A process refused by a live peer waits, before it serves anything, for the peer's heartbeat to
// lapse (processes.Register), up to a live window and a heartbeat. A deploy that stops it in that
// wait stops it cleanly: SIGTERM ends the wait, the process logs the stop at INFO and exits 0,
// and no supervisor reads the signal it sent as a failed start. The peer here is a running
// process with a key set of its own, so its row keeps heartbeating and the wait does not end on
// its own before the signal.
func TestAProcessStoppedWhileWaitingForAdmissionExitsCleanly(t *testing.T) {
	database := storetest.Open(t)
	peer := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{SigningKey: "the-live-peers-key"})
	peer.waitHealthy(t)

	waiting := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{SigningKey: "a-key-the-peer-does-not-share"})
	waiting.WaitForOutput(t, "a live server process refuses this one's signing keys; waiting for its heartbeat to lapse")
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
