package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testport"
	"github.com/sjawhar/envoy/internal/testwait"
)

// On 2026-11-22 the webhook listener's start began the role claim scan through a Service that
// still pointed at the VM hub. The Service's switch to the clustered hub reset the connection one
// second in, nats.go reconnected to the clustered hub a second later, and no further entry came:
// the scan's idle bound (kvwatch.ScanIdleTimeout) ended it after 4,577 of the bucket's 17,454
// entries, and the listener exited. A run that spans a reconnect is not shown to have read the
// whole bucket, complete or not: after the reconnect either the idle bound fires first, or nats.go
// makes the ordered consumer again at the next stream sequence on whatever server the connection
// now reaches, where that sequence need not name the same entry. So such a run is made again
// (scanRoleClaims). The first case is that morning: the forwarder in front of the client points at
// server A and then at server B, whose buckets differ in three claims, and the claims Open keeps
// must be B's alone, never one only A holds that the first run read before the switch, though the
// run that spanned the switch ends complete on what it read. The second is a reconnect to the same
// server, the third a reconnect that has to wait for the server's port. The fourth is a reconnect
// whose handshake the server does not answer for a while: nats.go holds the connection's lock
// through that handshake, so every read of the connection's status or count waits for it, and the
// scan must notice the reconnect all the same, by the connection's own notice.
//
// The scan's timers run on a clocktest fake that each case moves (roleScanSeams.clock, scanCase):
// its deadline, its polls of the connection and each run's idle bound. A case moves the fake past
// one only once the scan has armed it, at the point of the case where that timer comes due. What
// the bucket's watch hands each run is a gate the case keeps (scanGate): the first run is held at an
// entry, the case acts on the connection, and then ends the run as a run that spanned a reconnect
// ended that morning, on its idle bound, or complete. nats.go makes each reconnect at once
// (forceReconnect) rather than after its reconnect wait, which no verdict reads.
func TestARoleClaimScanThatSpansAReconnectRunsAgain(t *testing.T) {
	t.Parallel()
	// heldAt is the entry each case's first run is held at, of a bucket of claims and markers by
	// turns (roleBucket): past A's a-only, so the first run has read it when the case acts.
	const heldAt = roleScanEntries / 2

	t.Run("the forwarder switches from server A to server B mid-scan", func(t *testing.T) {
		t.Parallel()
		a, b := twoRoleScanServers(t)
		forwarder := startScanForwarder(t, a.address)
		conn := connectForScan(t, forwarder.url())
		gate := &scanGate{held: heldAt, heldRuns: 1}
		scan := newScanCase()
		scan.open(t, conn, gate.watch)
		gate.awaitHeld(t, scan, 1)
		before := conn.Stats().Reconnects
		forwarder.switchTo(b.address)
		forceReconnect(t, conn)
		scan.awaitNoticed(t, 1)
		scan.awaitReconnected(t, conn, before)
		// The run that spanned the switch ends complete, on A's claims before it, a-only among them.
		gate.end(1)
		scan.awaitAfter(t, 1)
		scan.pollUntilRun(t, 2)
		result := scan.pollUntilReturned(t)
		if result.err != nil {
			t.Fatalf("Open over a switch from A to B mid-scan failed after %d runs: %v", scan.probe.count(), result.err)
		}
		if runs := scan.probe.count(); runs < 2 {
			t.Fatalf("Open returned after %d run of the scan, which spanned the switch; want two or more", runs)
		}
		if revision, held := result.registry.restoredRoleRevisions["a-only"]; held {
			t.Fatalf("Open restored the claim only A holds at revision %d: a run that spanned the switch, or its result, reached the start", revision)
		}
		requireRevisions(t, result.registry.restoredRoleRevisions, b.revisions, "B's")
	})

	t.Run("a forced reconnect to the same server mid-scan", func(t *testing.T) {
		t.Parallel()
		server := startRoleScanServer(t, roleBucket(roleScanEntries))
		conn := connectForScan(t, server.uri)
		gate := &scanGate{held: heldAt, heldRuns: 1}
		scan := newScanCase()
		scan.open(t, conn, gate.watch)
		gate.awaitHeld(t, scan, 1)
		before := conn.Stats().Reconnects
		forceReconnect(t, conn)
		scan.awaitNoticed(t, 1)
		scan.awaitReconnected(t, conn, before)
		// The run that spanned the reconnect gets no further entry, and its idle bound ends it.
		gate.stall(1)
		scan.endRunOnIdle(t)
		scan.awaitAfter(t, 1)
		scan.pollUntilRun(t, 2)
		result := scan.pollUntilReturned(t)
		if result.err != nil {
			t.Fatalf("Open over a forced reconnect mid-scan failed after %d runs: %v", scan.probe.count(), result.err)
		}
		if runs := scan.probe.count(); runs != 2 {
			t.Fatalf("Open returned after %d runs of the scan; want 2: the one that spanned the reconnect and the one after it", runs)
		}
		requireRevisions(t, result.registry.restoredRoleRevisions, server.revisions, "the bucket's")
	})

	t.Run("the forwarder's port is closed for 15 s mid-scan", func(t *testing.T) {
		t.Parallel()
		server := startRoleScanServer(t, roleBucket(roleScanEntries))
		forwarder := startScanForwarder(t, server.address)
		conn := connectForScan(t, forwarder.url())
		gate := &scanGate{held: heldAt, heldRuns: 1}
		scan := newScanCase()
		scan.open(t, conn, gate.watch)
		gate.awaitHeld(t, scan, 1)
		const closedFor = 15 * time.Second
		before := conn.Stats().Reconnects
		forwarder.closePort()
		droppedAt := scan.clock.Now()
		scan.awaitNoticed(t, 1)
		gate.stall(1)
		scan.endRunOnIdle(t)
		scan.awaitAfter(t, 1)
		// The run ended while the connection could not reach the server: the scan takes the reading
		// begun after the run's end, which shows that, then waits for the connection, taking the
		// readings that follow. It makes no run meanwhile, and the second reading taken shows it: a
		// run begun after the first reading would have taken it.
		scan.takeReading(t, "readConn's poll after the reading begun after the run's end, armed once the scan took that reading")
		scan.takeReading(t, "readConn's poll after a reading of the wait for the connection, armed once the scan took that reading")
		if runs := scan.probe.count(); runs != 1 {
			t.Fatalf("the scan made %d runs while the forwarder's port was closed; want 1: it runs again only once the connection is back", runs)
		}
		// The port opens closedFor after the drop, on the scan's clock.
		scan.clock.Set(droppedAt.Add(closedFor))
		if err := forwarder.openPort(); err != nil {
			t.Fatalf("the forwarder could not listen again: %v", err)
		}
		forceReconnect(t, conn)
		scan.awaitReconnected(t, conn, before)
		scan.pollUntilRun(t, 2)
		result := scan.pollUntilReturned(t)
		if result.err != nil {
			t.Fatalf("Open over a connection that was down for %s mid-scan failed after %d runs: %v", closedFor, scan.probe.count(), result.err)
		}
		if waited := result.at.Sub(droppedAt); waited < closedFor {
			t.Fatalf("Open returned %s after the drop on the scan's clock, before the forwarder listened again %s after it", waited, closedFor)
		}
		if runs := scan.probe.count(); runs != 2 {
			t.Fatalf("Open returned after %d runs of the scan; want 2: the one that ended while disconnected and the one once connected", runs)
		}
		requireRevisions(t, result.registry.restoredRoleRevisions, server.revisions, "the bucket's")
	})

	t.Run("a reconnect whose handshake stalls mid-scan", func(t *testing.T) {
		t.Parallel()
		server := startRoleScanServer(t, roleBucket(roleScanEntries))
		hold := testnats.StartHandshakeHold(t, server.uri)
		conn := connectForScan(t, hold.URL())
		// Registered after the connection's close, so it runs first, and the close does not wait
		// for a handshake the hold still keeps.
		t.Cleanup(hold.Release)
		gate := &scanGate{held: heldAt, heldRuns: 1}
		scan := newScanCase()
		scan.open(t, conn, gate.watch)
		gate.awaitHeld(t, scan, 1)
		before, attempts := conn.Stats().Reconnects, hold.Attempts()
		hold.Hold()
		forceReconnect(t, conn)
		// The reconnect's CONNECT has reached the server, which does not answer it while the hold
		// lasts, and nats.go holds the connection's lock through the wait.
		hold.AwaitAttempt(t, conn, attempts)
		heldAt := scan.clock.Now()
		scan.awaitNoticed(t, 1)
		// The scan noticed the reconnect while the handshake held the lock, which every reading of
		// the connection waits on, and with the scan's clock where it stood as the hold began, so no
		// poll brought a reading either: the connection's own notice is what it noticed it by.
		testnats.RequireLockHeld(t, conn)
		noticed, _ := scan.probe.noticedAt(1)
		if !noticed.Equal(heldAt) {
			t.Fatalf("the scan noticed the reconnect at %v on its clock, %s after its handshake was held there; want with no move of the clock, by the connection's notice", noticed, noticed.Sub(heldAt))
		}
		hold.Release()
		scan.awaitReconnected(t, conn, before)
		gate.stall(1)
		scan.endRunOnIdle(t)
		scan.awaitAfter(t, 1)
		scan.pollUntilRun(t, 2)
		result := scan.pollUntilReturned(t)
		if result.err != nil {
			t.Fatalf("Open over a reconnect whose handshake stalled mid-scan failed after %d runs: %v", scan.probe.count(), result.err)
		}
		if runs := scan.probe.count(); runs != 2 {
			t.Fatalf("Open returned after %d runs of the scan; want 2: the one the reconnect spanned and the one after it", runs)
		}
		requireRevisions(t, result.registry.restoredRoleRevisions, server.revisions, "the bucket's")
	})
}

// A run that meets a reconnect is made again, and the scan that never ends cleanly must still end
// the start: at most four runs (roleClaimScanRuns), and a deadline 70 s after the first run started
// (roleClaimScanDeadline) for the reconnect path alone. Each case holds one bound and the line it
// must not cross. Runs that each span a reconnect stop at four; a reconnect in a slow run, or a wait
// for a connection that does not come back, ends at the deadline; a reconnect after the deadline
// ends the run in progress at once. A run that meets no reconnect keeps main's bounds and no other:
// one that stalls fails on the idle bound after one run, and one that is merely slow, past the
// deadline, completes. A closed connection fails the start, and is not taken for a reconnect. The
// eighth case holds the deadline while a reconnect's handshake holds nats.go's connection lock,
// which the teardown of the run's watch needs: the start must fail at the deadline, not when the
// handshake lets the lock go. The ninth holds the reading that decides what follows a run to one
// begun after the run ended.
//
// The scan's timers run on a clocktest fake that each case moves, as in
// TestARoleClaimScanThatSpansAReconnectRunsAgain, so each bound is read on the scan's clock: the
// case moves the fake to the deadline, or past a run's idle bound, only once the scan has armed it,
// and waits at each step before it, failing at once should Open return early (scanCase.until).
func TestARoleClaimScanThatNeverCompletesFailsTheStart(t *testing.T) {
	t.Parallel()
	// paced is how far a case that paces its run moves the scan's clock before it lets the run's
	// watch hand its next entry (scanGate.step): inside the idle bound (kvwatch.ScanIdleTimeout,
	// 10 s), so the run ends only by completing or by the scan's own ending of it, however far past
	// the deadline it goes.
	const paced = 8 * time.Second

	t.Run("every run spans a switch between two servers", func(t *testing.T) {
		t.Parallel()
		a, b := twoRoleScanServers(t)
		forwarder := startScanForwarder(t, a.address)
		conn := connectForScan(t, forwarder.url())
		gate := &scanGate{held: 1, heldRuns: roleClaimScanRuns}
		scan := newScanCase()
		scan.open(t, conn, gate.watch)
		servers := []string{a.address, b.address}
		for run := 1; run <= roleClaimScanRuns; run++ {
			// At each run's first entry the forwarder switches, and the run gets no further entry.
			gate.awaitHeld(t, scan, run)
			before := conn.Stats().Reconnects
			forwarder.switchTo(servers[run%2])
			forceReconnect(t, conn)
			scan.awaitNoticed(t, run)
			scan.awaitReconnected(t, conn, before)
			gate.stall(run)
			scan.endRunOnIdle(t)
			scan.awaitAfter(t, run)
			if run < roleClaimScanRuns {
				scan.pollUntilRun(t, run+1)
			}
		}
		result := scan.pollUntilReturned(t)
		requireScanFailure(t, result.err)
		if runs := scan.probe.count(); runs != roleClaimScanRuns {
			t.Fatalf("Open failed after %d runs of the scan, each spanning a switch; want exactly %d: %v", runs, roleClaimScanRuns, result.err)
		}
	})

	t.Run("a reconnect in a run that no idle bound ends", func(t *testing.T) {
		t.Parallel()
		server := startRoleScanServer(t, claimsOnly(20))
		conn := connectForScan(t, server.uri)
		gate := &scanGate{stepped: true}
		scan := newScanCase()
		scan.open(t, conn, gate.watch)
		deadline := scan.deadline(t)
		gate.awaitRead(t, scan, 1)
		// Two seconds into the run, the connection reconnects.
		scan.awaitIdleArmed(t)
		scan.clock.Advance(2 * time.Second)
		before := conn.Stats().Reconnects
		forceReconnect(t, conn)
		scan.awaitNoticed(t, 1)
		scan.awaitReconnected(t, conn, before)
		scan.clock.Advance(paced - 2*time.Second)
		gate.step(1)
		for scan.clock.Now().Add(paced).Before(deadline) {
			scan.awaitIdleArmed(t)
			scan.clock.Advance(paced)
			gate.step(1)
		}
		scan.awaitIdleArmed(t)
		scan.clock.Set(deadline)
		result := scan.result(t)
		requireDeadline(t, result, scan.probe)
		if runs := scan.probe.count(); runs >= roleClaimScanRuns {
			t.Fatalf("Open failed after %d runs; the deadline should have ended the scan in fewer than %d", runs, roleClaimScanRuns)
		}
	})

	t.Run("a scan that stalls with no reconnect", func(t *testing.T) {
		t.Parallel()
		server := startRoleScanServer(t, claimsOnly(3000))
		conn := connectForScan(t, server.uri)
		stalled := func(kv bus.KeyValue) bus.KeyValue {
			return bus.NewKeyValue(kv.Legacy(), earlyEndKV{KeyValue: kv.WatchHandle(), after: 2000}, kv.Streams())
		}
		scan := newScanCase()
		scan.open(t, conn, stalled)
		scan.until(t, "the first run to be handed the 2,000 claims its watch delivers before it stalls", func() (string, bool) {
			claims := scan.probe.claimsHanded()
			return fmt.Sprintf("%d claims handed", claims), claims >= 2000
		})
		// The run's idle bound lies ScanIdleTimeout after its last entry, on the scan's clock.
		idle := scan.probe.lastClaimAt().Add(kvwatch.ScanIdleTimeout)
		scan.awaitArmedAt(t, "the stalled run's idle bound", idle)
		scan.clock.Set(idle)
		result := scan.pollUntilReturned(t)
		if !errors.Is(result.err, kvwatch.ErrScanTimedOut) {
			t.Fatalf("Open over a scan that stalled after 2,000 entries returned %v; want the scan's %v, as on main", result.err, kvwatch.ErrScanTimedOut)
		}
		requireScanFailure(t, result.err)
		if runs := scan.probe.count(); runs != 1 {
			t.Fatalf("Open made %d runs of a scan that met no reconnect; want 1", runs)
		}
		idled := result.at.Sub(scan.probe.lastClaimAt())
		if idled < kvwatch.ScanIdleTimeout-500*time.Millisecond || idled > kvwatch.ScanIdleTimeout+5*time.Second {
			t.Fatalf("Open failed %s after the scan's last entry on the scan's clock; want about the idle bound, %s", idled, kvwatch.ScanIdleTimeout)
		}
	})

	t.Run("a long healthy scan past the deadline", func(t *testing.T) {
		t.Parallel()
		server := startRoleScanServer(t, claimsOnly(11))
		conn := connectForScan(t, server.uri)
		gate := &scanGate{stepped: true}
		scan := newScanCase()
		scan.open(t, conn, gate.watch)
		scan.deadline(t)
		gate.awaitRead(t, scan, 1)
		// One entry every paced, the ninth past the deadline: the run goes on through it.
		for range len(server.revisions) {
			scan.awaitIdleArmed(t)
			scan.clock.Advance(paced)
			gate.step(1)
		}
		result := scan.pollUntilReturned(t)
		if result.err != nil {
			t.Fatalf("Open over a slow scan that met no reconnect failed after %d runs: %v", scan.probe.count(), result.err)
		}
		if runs := scan.probe.count(); runs != 1 {
			t.Fatalf("Open made %d runs of a scan that met no reconnect; want 1", runs)
		}
		if lasted := result.at.Sub(scan.probe.firstRunAt()); lasted < roleClaimScanDeadline+5*time.Second {
			t.Fatalf("the scan lasted %s on its clock, not past the deadline (%s): this case tests nothing", lasted, roleClaimScanDeadline)
		}
		requireRevisions(t, result.registry.restoredRoleRevisions, server.revisions, "the bucket's")
	})

	t.Run("a closed connection", func(t *testing.T) {
		t.Parallel()
		server := startRoleScanServer(t, claimsOnly(3000))
		conn := connectForScan(t, server.uri)
		scan := newScanCase()
		closed := make(chan time.Time, 1)
		scan.probe.handed = func(run, claims int) {
			if run == 1 && claims == 2000 {
				closed <- scan.clock.Now()
				conn.Close()
			}
		}
		scan.open(t, conn, nil)
		// The case never moves the scan's clock: no timer of the scan's has to pass for a closed
		// connection to fail the start.
		result := scan.result(t)
		var closedAt time.Time
		select {
		case closedAt = <-closed:
		default:
			t.Fatalf("the test never closed the connection: the first run did not deliver 2,000 claims (Open: %v)", result.err)
		}
		if runs := scan.probe.count(); runs != 1 {
			t.Fatalf("Open made %d runs on a connection closed during the first; a closed connection is no reconnect, want 1: %v", runs, result.err)
		}
		requireScanFailure(t, result.err)
		if !errors.Is(result.err, natsgo.ErrConnectionClosed) || !strings.Contains(result.err.Error(), "closed NATS connection") {
			t.Fatalf("Open's error does not name the closed connection: %v", result.err)
		}
		if took := result.at.Sub(closedAt); took != 0 {
			t.Fatalf("Open failed %s after the connection closed on the scan's clock; want with no move of it", took)
		}
	})

	t.Run("a wait for the connection past the deadline", func(t *testing.T) {
		t.Parallel()
		server := startRoleScanServer(t, claimsOnly(3000))
		forwarder := startScanForwarder(t, server.address)
		conn := connectForScan(t, forwarder.url())
		gate := &scanGate{held: 2000, heldRuns: 1}
		scan := newScanCase()
		scan.open(t, conn, gate.watch)
		deadline := scan.deadline(t)
		gate.awaitHeld(t, scan, 1)
		forwarder.closePort()
		scan.awaitNoticed(t, 1)
		gate.stall(1)
		scan.endRunOnIdle(t)
		scan.awaitAfter(t, 1)
		// The run ended while the connection could not reach the server: the scan takes the reading
		// begun after the run's end, then waits for the connection, taking the readings that follow.
		// Once it has taken one of those, it is in that wait, held to the deadline.
		scan.takeReading(t, "readConn's poll after the reading begun after the run's end, armed once the scan took that reading")
		scan.takeReading(t, "readConn's poll after a reading of the wait for the connection, armed once the scan took that reading")
		scan.clock.Set(deadline)
		result := scan.result(t)
		requireDeadline(t, result, scan.probe)
		if runs := scan.probe.count(); runs != 1 {
			t.Fatalf("Open made %d runs while the connection never came back; want 1", runs)
		}
	})

	t.Run("a reconnect after the deadline", func(t *testing.T) {
		t.Parallel()
		server := startRoleScanServer(t, claimsOnly(11))
		conn := connectForScan(t, server.uri)
		gate := &scanGate{stepped: true}
		scan := newScanCase()
		scan.open(t, conn, gate.watch)
		scan.deadline(t)
		gate.awaitRead(t, scan, 1)
		const forceAt = 80 * time.Second
		force := scan.probe.firstRunAt().Add(forceAt)
		for scan.clock.Now().Add(paced).Before(force) {
			scan.awaitIdleArmed(t)
			scan.clock.Advance(paced)
			gate.step(1)
		}
		// The run is still in progress, past the deadline, when the connection reconnects.
		scan.awaitIdleArmed(t)
		scan.clock.Set(force)
		forceReconnect(t, conn)
		result := scan.result(t)
		requireScanFailure(t, result.err)
		if !errors.Is(result.err, errRoleClaimScanLateReconnect) || !strings.Contains(result.err.Error(), "reconnected") {
			t.Fatalf("Open's error does not name the reconnect after the deadline: %v", result.err)
		}
		if took := result.at.Sub(force); took != 0 {
			t.Fatalf("Open failed %s after the reconnect past the deadline on the scan's clock; want with no move of it", took)
		}
		if runs := scan.probe.count(); runs != 1 {
			t.Fatalf("Open made %d runs; want 1", runs)
		}
	})

	t.Run("a deadline while a reconnect's handshake holds the connection", func(t *testing.T) {
		t.Parallel()
		server := startRoleScanServer(t, claimsOnly(20))
		hold := testnats.StartHandshakeHold(t, server.uri)
		// The connection's connect timeout is the test command's guard (connectForScan), so the held
		// attempt holds nats.go's lock from the hold to its release; and the release is registered
		// after the connection's close, so it runs first and the close does not wait for the hold.
		conn := connectForScan(t, hold.URL())
		t.Cleanup(hold.Release)
		gate := &scanGate{stepped: true}
		scan := newScanCase()
		scan.open(t, conn, gate.watch)
		deadline := scan.deadline(t)
		gate.awaitRead(t, scan, 1)
		// The hold begins 8 s before the deadline, so the reconnect's handshake holds the lock across
		// it.
		const holdAt = roleClaimScanDeadline - 8*time.Second
		holdFrom := scan.probe.firstRunAt().Add(holdAt)
		for scan.clock.Now().Add(paced).Before(holdFrom) {
			scan.awaitIdleArmed(t)
			scan.clock.Advance(paced)
			gate.step(1)
		}
		scan.awaitIdleArmed(t)
		scan.clock.Set(holdFrom)
		attempts := hold.Attempts()
		hold.Hold()
		forceReconnect(t, conn)
		hold.AwaitAttempt(t, conn, attempts)
		heldAt := scan.clock.Now()
		scan.awaitNoticed(t, 1)
		// One more entry, so the run's idle bound lies past the deadline and the run is in progress
		// when the deadline comes.
		gate.step(1)
		scan.awaitIdleArmed(t)
		scan.clock.Set(deadline)
		result := scan.result(t)
		if since := heldAt.Sub(scan.probe.firstRunAt()); since >= roleClaimScanDeadline {
			t.Fatalf("the reconnect's handshake was held only %s after the first run started, past the deadline, so this run cannot show what the deadline does while a handshake holds the connection", since)
		}
		requireDeadline(t, result, scan.probe)
		if late := result.at.Sub(scan.probe.firstRunAt()) - roleClaimScanDeadline; late > time.Second {
			t.Fatalf("Open failed %s after the deadline on the scan's clock while a reconnect's handshake held the connection; want within 1s", late)
		}
		// Open answered while the handshake still held the lock its watch's teardown needs.
		testnats.RequireLockHeld(t, conn)
		hold.Release()
		if runs := scan.probe.count(); runs != 1 {
			t.Fatalf("Open made %d runs; want 1", runs)
		}
	})

	t.Run("a reading begun before the run ended decides nothing after it", func(t *testing.T) {
		t.Parallel()
		server := startRoleScanServer(t, claimsOnly(20))
		hold := testnats.StartHandshakeHold(t, server.uri)
		conn := connectForScan(t, hold.URL())
		t.Cleanup(hold.Release)
		gate := &scanGate{held: 10, heldRuns: 1}
		scan := newScanCase()
		reads := &nowCounted{Fake: scan.clock}
		scan.seamClock = reads
		scan.open(t, conn, gate.watch)
		gate.awaitHeld(t, scan, 1)
		// A reconnect whose handshake the hold keeps: nats.go holds the connection's lock until the
		// hold lets the server's answer through.
		attempts := hold.Attempts()
		hold.Hold()
		forceReconnect(t, conn)
		hold.AwaitAttempt(t, conn, attempts)
		scan.awaitNoticed(t, 1)
		// readConn's next poll: it reads the time its reading begins at, then waits in
		// conn.Status() for the lock the handshake holds. While the run waits on its held watch, with
		// the reconnect noticed, readConn is the one reader of the scan's clock, so its read is the
		// next one counted.
		poll := scan.clock.Now().Add(roleClaimScanPoll)
		scan.awaitArmedAt(t, "readConn's next poll of the connection", poll)
		read := reads.count()
		scan.clock.Set(poll)
		scan.until(t, "readConn to read the time its reading begins at", func() (string, bool) {
			n := reads.count()
			return fmt.Sprintf("%d reads of the scan's clock, %d before the poll", n, read), n > read
		})
		// The run ends later on the scan's clock: its watch's subscription ends, which takes no
		// lock of the connection's.
		scan.clock.Advance(time.Second)
		gate.close(1)
		scan.awaitAfter(t, 1)
		// The hold lets the reconnect through, and the reading begun at the poll reaches the scan
		// only now, after the run's end.
		hold.Release()
		scan.awaitArmedAt(t, "readConn's poll after the reading begun before the run's end, armed once the scan took that reading", scan.clock.Now().Add(roleClaimScanPoll))
		// The scan still waits for a reading begun after the run ended, and the clock does not move,
		// so none comes. The connection's close ends that wait, and the start with it, on the run
		// that ended.
		conn.Close()
		result := scan.result(t)
		if runs := scan.probe.count(); runs != 1 {
			t.Fatalf("Open made %d runs: the scan took the reading begun before run 1 ended, which the held handshake delivered after it, for the one that decides what follows the run (%v)", runs, result.err)
		}
		requireScanFailure(t, result.err)
		if !errors.Is(result.err, natsgo.ErrConnectionClosed) || !strings.Contains(result.err.Error(), "run 1 ended on a closed NATS connection") {
			t.Fatalf("Open's error does not name run 1's end on the closed connection: %v", result.err)
		}
	})
}

// A run whose watch cannot be made fails the start as a run that ended incomplete does, and its
// error names the role claim scan as every other failure of the scan does: on its own, the error
// of the watch's construction (a consumer create the server refused, say), which
// kvwatch.ScanExistingKeysOn returns as nats.go gave it, names nothing a reader of the listener's
// exit can trace to the scan.
func TestARoleClaimScanWhoseWatchCannotBeMadeNamesTheScan(t *testing.T) {
	t.Parallel()
	server := startRoleScanServer(t, claimsOnly(1))
	conn := connectForScan(t, server.uri)
	refused := errors.New("the consumer create was refused")
	unmade := func(kv bus.KeyValue) bus.KeyValue {
		return bus.NewKeyValue(kv.Legacy(), unmadeWatchKV{KeyValue: kv.WatchHandle(), err: refused}, kv.Streams())
	}
	scan := newScanCase()
	scan.open(t, conn, unmade)
	result := scan.pollUntilReturned(t)
	if !errors.Is(result.err, refused) {
		t.Fatalf("Open over a watch that could not be made returned %v; want the watch's own error, %v", result.err, refused)
	}
	requireScanFailure(t, result.err)
	if runs := scan.probe.count(); runs != 1 {
		t.Fatalf("Open made %d runs of a scan whose watch could not be made, with no reconnect; want 1", runs)
	}
}

// unmadeWatchKV refuses every watch with err, as a server that refuses the watch's consumer create
// does.
type unmadeWatchKV struct {
	jetstream.KeyValue

	err error
}

func (k unmadeWatchKV) Watch(context.Context, string, ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	return nil, k.err
}

// withRoleScanSeams sets the role claim scan's test seams (roleScanSeams) for one Open.
func withRoleScanSeams(seams roleScanSeams) OpenOption {
	return func(o *openOpts) { o.roleScan = seams }
}

// roleScanEntries is the size of the role claim bucket of the cases that read a bucket of claims
// and markers by turns, a run of which each case holds at an entry halfway through
// (TestARoleClaimScanThatSpansAReconnectRunsAgain) and then reads again whole. A gate holds the run
// where the case acts (scanGate), so the bucket need not be long for a run to last until then; it
// is still past the 256 entries nats.go's watcher buffers, so a scan's watch delivers it in turns.
const roleScanEntries = 1000

// roleEntry is one write a test makes to the role claim bucket: a claim of key, or a delete
// marker of it.
type roleEntry struct {
	key    string
	marker bool
}

// roleBucket is n writes to the role claim bucket, of keys role-00001 on, a claim and a delete
// marker by turns, so a scan streams past markers among the claims as it does in the deployment.
func roleBucket(n int) []roleEntry {
	entries := make([]roleEntry, n)
	for i := range entries {
		entries[i] = roleEntry{key: fmt.Sprintf("role-%05d", i+1), marker: i%2 == 1}
	}
	return entries
}

// claimsOnly is n claims, of keys role-00001 on, and no marker, so a count of the claims a run is
// handed is a count of its entries too.
func claimsOnly(n int) []roleEntry {
	entries := make([]roleEntry, n)
	for i := range entries {
		entries[i] = roleEntry{key: fmt.Sprintf("role-%05d", i+1)}
	}
	return entries
}

// roleScanServer is a NATS server whose role claim bucket a case scans: its client URL, its address
// for a forwarder, and the revision of each claim its bucket holds, which is what Open must restore
// from a scan of it.
type roleScanServer struct {
	uri       string
	address   string
	revisions map[string]uint64
}

// startRoleScanServer starts a NATS server with the interest and role claim buckets and writes
// entries to the role claim bucket in order. The writes are asynchronous publishes to the keys'
// subjects, a delete marker one with the KV-Operation header nats.go's Delete sends, so a bucket of
// thousands is written at once; each write's acknowledgement gives its revision.
func startRoleScanServer(t *testing.T, entries []roleEntry) roleScanServer {
	t.Helper()
	_, uri := testnats.Start(t)
	conn := testnats.Connect(t, uri)
	defer conn.Close()
	js, err := conn.JetStream(natsgo.PublishAsyncMaxPending(1024), testnats.MaxWait(t))
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	for _, bucket := range []string{bus.InterestBucket, bus.RoleClaimBucket} {
		if _, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bucket, Storage: natsgo.FileStorage}); err != nil {
			t.Fatalf("create the %s bucket: %v", bucket, err)
		}
	}
	// A write past the 1,024 pending waits for room until the test command's guard, not a span of
	// its own.
	stallWait := natsgo.StallWait(untilGiveUp(t))
	futures := make([]natsgo.PubAckFuture, len(entries))
	for i, entry := range entries {
		msg := natsgo.NewMsg("$KV." + bus.RoleClaimBucket + "." + entry.key)
		if entry.marker {
			msg.Header.Set("KV-Operation", "DEL")
		} else {
			msg.Data = []byte(`{"holder_session_id":"ses_` + entry.key + `"}`)
		}
		if futures[i], err = js.PublishMsgAsync(msg, stallWait); err != nil {
			t.Fatalf("write entry %d of the role claim bucket: %v", i+1, err)
		}
	}
	testwait.Receive(t, "the server to answer every write to the role claim bucket", js.PublishAsyncComplete())
	revisions := map[string]uint64{}
	for i, future := range futures {
		select {
		case ack := <-future.Ok():
			if entries[i].marker {
				delete(revisions, entries[i].key)
			} else {
				revisions[entries[i].key] = ack.Sequence
			}
		case err := <-future.Err():
			t.Fatalf("write entry %d of the role claim bucket: %v", i+1, err)
		default:
			t.Fatalf("write entry %d of the role claim bucket has no answer, though nats.go reported every write answered", i+1)
		}
	}
	server, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("read the server's URL: %v", err)
	}
	return roleScanServer{uri: uri, address: server.Host, revisions: revisions}
}

// twoRoleScanServers starts servers A and B with the same role claim bucket but for three claims:
// one only A holds, written early in the bucket so a first run reads it before the case acts; one
// only B holds, at the same place in B's bucket; and B's first claim, which B writes once more, so
// its revision there is not A's.
func twoRoleScanServers(t *testing.T) (a, b roleScanServer) {
	t.Helper()
	entries := roleBucket(roleScanEntries)
	onA, onB := slices.Clone(entries), slices.Clone(entries)
	onA[roleScanEntries/4] = roleEntry{key: "a-only"}
	onB[roleScanEntries/4] = roleEntry{key: "b-only"}
	onB = append(onB, entries[0])
	a, b = startRoleScanServer(t, onA), startRoleScanServer(t, onB)
	if _, held := a.revisions["a-only"]; !held {
		t.Fatalf("A's bucket holds no a-only claim")
	}
	if a.revisions[entries[0].key] == b.revisions[entries[0].key] {
		t.Fatalf("%s is at revision %d on both servers; B's write of it again did not land", entries[0].key, a.revisions[entries[0].key])
	}
	return a, b
}

// connectForScan connects to uri as the listener's bus client does in what this test reads: it
// reconnects for ever, a second after a drop (bus's ReconnectWait), so a scan can meet a reconnect.
// Its connect timeout, which bounds each attempt's dial and handshake, is what is left until the
// shared wait gives up (untilGiveUp), so no attempt ends before the test command's guard does: a
// handshake a case holds ends when the case lets it. It closes when the test ends, before any
// server or forwarder it reaches.
func connectForScan(t *testing.T, uri string) *natsgo.Conn {
	t.Helper()
	conn, err := natsgo.Connect(uri, natsgo.Name(t.Name()), natsgo.Timeout(untilGiveUp(t)),
		natsgo.MaxReconnects(-1), natsgo.ReconnectWait(time.Second))
	if err != nil {
		t.Fatalf("connect to %s: %v", uri, err)
	}
	t.Cleanup(conn.Close)
	return conn
}

// untilGiveUp is what is left until the shared wait gives up (testwait.GiveUpAt), for a client
// timeout of the cases' own that must end no wait of the test before the test command's guard does;
// under -timeout 0, where no wait gives up, it is the longest span a time.Duration holds.
func untilGiveUp(t testing.TB) time.Duration {
	if end, bounded := testwait.GiveUpAt(t); bounded {
		return max(time.Until(end), time.Millisecond)
	}
	return time.Duration(math.MaxInt64)
}

// forceReconnect has nats.go reconnect conn at once (natsgo.Conn.ForceReconnect): after a drop the
// case made, nats.go waits its reconnect wait (ReconnectWait, connectForScan) before its first
// attempt, which no verdict of these cases reads, and this makes the attempt now.
func forceReconnect(t *testing.T, conn *natsgo.Conn) {
	t.Helper()
	if err := conn.ForceReconnect(); err != nil {
		t.Fatalf("force a reconnect: %v", err)
	}
}

// scanCase is one Open of a role claim scan case: the fake clock the case hands the scan and moves
// (roleScanSeams.clock), what the scan's seams record (scanProbe), and what Open returned.
type scanCase struct {
	clock *clocktest.Fake
	// seamClock is the clock the scan is handed: clock, or a case's wrapper of it.
	seamClock clock.Clock
	probe     *scanProbe
	// done closes once Open has returned, and outcome is what it returned, read once done is closed.
	done    chan struct{}
	outcome openResult
}

// newScanCase is a case whose scan's clock stands at scanClockStart, so far unopened (open).
func newScanCase() *scanCase {
	fake := clocktest.New(scanClockStart)
	return &scanCase{clock: fake, seamClock: fake, probe: &scanProbe{clock: fake}, done: make(chan struct{})}
}

// open opens the registry on conn on a goroutine of its own through the scan's seams
// (scanProbe.seams), with watch wrapping the bucket each run reads, nil for the bucket as it is,
// and returns at once: the case moves the scan's clock while Open runs. A registry Open returns is
// stopped when the test ends, one it returns after that too.
func (c *scanCase) open(t *testing.T, conn *natsgo.Conn, watch func(bus.KeyValue) bus.KeyValue) {
	t.Helper()
	seams := c.probe.seams(c.seamClock, watch)
	ctx := testwait.Context(t)
	go func() {
		registry, err := Open(ctx, conn, WithReplicas(1), withRoleScanSeams(seams))
		c.outcome = openResult{registry: registry, err: err, at: c.clock.Now()}
		close(c.done)
	}()
	t.Cleanup(func() {
		select {
		case <-c.done:
			if c.outcome.registry != nil {
				c.outcome.registry.StopWatch()
			}
		default:
			go func() {
				<-c.done
				if c.outcome.registry != nil {
					c.outcome.registry.StopWatch()
				}
			}()
		}
	})
}

// returned reports whether Open has returned, and what it returned.
func (c *scanCase) returned() (openResult, bool) {
	select {
	case <-c.done:
		return c.outcome, true
	default:
		return openResult{}, false
	}
}

// result waits for Open to return and returns what it returned.
func (c *scanCase) result(t *testing.T) openResult {
	t.Helper()
	testwait.Receive(t, "Open to return", c.done)
	return c.outcome
}

// until waits through the shared wait until read holds, and fails the test at once when Open
// returns first: what the case waits for, on the way to a verdict on Open, cannot come then.
func (c *scanCase) until(t *testing.T, what string, read func() (string, bool)) {
	t.Helper()
	testwait.Until(t, what, func() (string, bool) {
		if result, returned := c.returned(); returned {
			t.Fatalf("Open returned after %d runs of the scan, before %s: %v", c.probe.count(), what, result.err)
		}
		return read()
	})
}

// awaitArmedAt waits until a timer due at at is pending on the scan's clock (until).
func (c *scanCase) awaitArmedAt(t *testing.T, what string, at time.Time) {
	t.Helper()
	c.until(t, what+" armed for "+at.Format(time.RFC3339Nano), func() (string, bool) {
		armed := c.clock.Armed()
		return fmt.Sprintf("armed %v, the clock at %v", armed, c.clock.Now()), slices.ContainsFunc(armed, at.Equal)
	})
}

// awaitRun waits until the scan has begun run n.
func (c *scanCase) awaitRun(t *testing.T, n int) {
	t.Helper()
	c.until(t, fmt.Sprintf("the scan to begin run %d", n), func() (string, bool) {
		runs := c.probe.count()
		return fmt.Sprintf("%d runs begun", runs), runs >= n
	})
}

// deadline waits until the scan has begun its first run and armed that run's deadline, and returns
// it: roleClaimScanDeadline after the start the scan hands the run seam.
func (c *scanCase) deadline(t *testing.T) time.Time {
	t.Helper()
	c.awaitRun(t, 1)
	deadline := c.probe.firstRunAt().Add(roleClaimScanDeadline)
	c.awaitArmedAt(t, "the first run's deadline", deadline)
	return deadline
}

// awaitIdleArmed waits until the run in progress has its idle bound armed ScanIdleTimeout after
// the clock's present, and returns that deadline: drainScan arms it as its scan begins, and again
// at each entry, at the present, so this holds once the run's scan has begun, or has taken the
// entry the case let through last, while the case has not moved the clock since.
func (c *scanCase) awaitIdleArmed(t *testing.T) time.Time {
	t.Helper()
	idle := c.clock.Now().Add(kvwatch.ScanIdleTimeout)
	c.awaitArmedAt(t, "the idle bound of the run in progress", idle)
	return idle
}

// endRunOnIdle ends the run in progress on its idle bound, once its scan has armed it
// (awaitIdleArmed). The clock moves first to a millisecond short of the bound, and the case waits
// until readConn has armed its next poll, past the bound, which it does once the run has taken the
// reading that move brought; so the move to the bound fires the idle timer alone, and the run ends
// with no reading of the connection on its way, one the wait after the run (roleClaimScan.after)
// would take before any the case brings.
func (c *scanCase) endRunOnIdle(t *testing.T) {
	t.Helper()
	idle := c.awaitIdleArmed(t)
	short := idle.Add(-time.Millisecond)
	c.clock.Set(short)
	c.awaitArmedAt(t, "readConn's poll after the reading the move short of the idle bound brought, armed once the run took that reading", short.Add(roleClaimScanPoll))
	c.clock.Set(idle)
}

// awaitNoticed waits until the scan has noticed, while run was in progress, that the connection
// left its server or reconnected.
func (c *scanCase) awaitNoticed(t *testing.T, run int) {
	t.Helper()
	c.until(t, fmt.Sprintf("the scan to notice, while run %d was in progress, that the connection left its server", run), func() (string, bool) {
		_, seen := c.probe.noticedAt(run)
		return fmt.Sprintf("noticed %t", seen), seen
	})
}

// awaitReconnected waits until nats.go has reconnected conn since its count of reconnects was
// before.
func (c *scanCase) awaitReconnected(t *testing.T, conn *natsgo.Conn, before uint64) {
	t.Helper()
	c.until(t, fmt.Sprintf("nats.go to reconnect the connection, after %d reconnects", before), func() (string, bool) {
		status, reconnects := conn.Status(), conn.Stats().Reconnects
		return fmt.Sprintf("%s after %d reconnects", status, reconnects), status == natsgo.CONNECTED && reconnects > before
	})
}

// awaitAfter waits until run has ended and the scan waits, held to its deadline, for a reading of
// the connection begun after the run's end (roleClaimScan.after): for a run the connection left
// its server during, it arms a second timer at the deadline beside the run's own.
func (c *scanCase) awaitAfter(t *testing.T, run int) {
	t.Helper()
	deadline := c.probe.firstRunAt().Add(roleClaimScanDeadline)
	c.until(t, fmt.Sprintf("run %d to end, and the scan to wait, held to its deadline, for a reading of the connection begun after it", run), func() (string, bool) {
		armed := c.clock.Armed()
		at := len(slices.DeleteFunc(slices.Clone(armed), func(due time.Time) bool { return !due.Equal(deadline) }))
		return fmt.Sprintf("%d timers armed at the deadline, of %v", at, armed), at >= 2
	})
}

// awaitPoll waits until readConn has armed its next poll of the connection, and returns its
// deadline (scanPollDue). It reads what is armed and the clock's present under one hold of the
// clock's lock (SetWhen), and moves nothing.
func (c *scanCase) awaitPoll(t *testing.T) time.Time {
	t.Helper()
	deadline := c.probe.firstRunAt().Add(roleClaimScanDeadline)
	var poll time.Time
	c.until(t, "readConn's next poll of the connection", func() (string, bool) {
		armed, _ := c.clock.SetWhen(func(now time.Time, armed []time.Time) (time.Time, bool) {
			if due, found := scanPollDue(now, armed, deadline, time.Time{}); found {
				poll = due
			}
			return now, false
		})
		return fmt.Sprintf("armed %v", armed), !poll.IsZero()
	})
	return poll
}

// takeReading moves the clock to the poll readConn has armed, and waits until the scan has taken
// the reading that poll began: readConn arms its next poll only once the scan has received it, and
// what names that.
func (c *scanCase) takeReading(t *testing.T, what string) {
	t.Helper()
	poll := c.awaitPoll(t)
	c.clock.Set(poll)
	c.awaitArmedAt(t, what, poll.Add(roleClaimScanPoll))
}

// pollUntilRun moves the clock past each poll of the connection the scan waits for between its
// runs (scanPace.anyPoll) until the scan begins run n.
func (c *scanCase) pollUntilRun(t *testing.T, n int) {
	t.Helper()
	c.until(t, fmt.Sprintf("the scan to begin run %d, the clock moved past each poll of the connection it waits for", n), func() (string, bool) {
		if runs := c.probe.count(); runs >= n {
			return fmt.Sprintf("%d runs begun", runs), true
		}
		armed, moved := c.clock.SetWhen(c.probe.pace.anyPoll)
		return c.probe.pace.reading(armed, moved), false
	})
}

// pollUntilReturned moves the clock past each poll of the connection the scan waits for between
// its runs (scanPace.anyPoll) until Open returns, and returns what it returned.
func (c *scanCase) pollUntilReturned(t *testing.T) openResult {
	t.Helper()
	testwait.Until(t, "Open to return, the clock moved past each poll of the connection the scan waits for", func() (string, bool) {
		if _, returned := c.returned(); returned {
			return "Open returned", true
		}
		armed, moved := c.clock.SetWhen(c.probe.pace.anyPoll)
		return c.probe.pace.reading(armed, moved), false
	})
	return c.outcome
}

// nowCounted is the scan's clock with each read of its present counted, for a case that must know
// a goroutine of the scan's has read the time while nothing else of the scan reads it.
type nowCounted struct {
	*clocktest.Fake

	reads atomic.Int64
}

func (c *nowCounted) Now() time.Time {
	c.reads.Add(1)
	return c.Fake.Now()
}

func (c *nowCounted) count() int64 { return c.reads.Load() }

// scanProbe counts the role claim scan's runs and records, on the scan's clock, when the first
// started, when a run was last handed a claim and how many it had been handed, and when the scan
// first noticed a reconnect in each run, through the scan's test seams; it runs handed at each claim
// a run is handed, after it records it, and keeps the pace of the scan's runs and their watches
// (scanPace) for the moves of the clock between runs.
//
// When the first run started is the scan's own clock read, the start it hands the run seam, which
// its deadline is roleClaimScanDeadline after; every bound below reads the scan's clock, which
// only the case moves, so it measures how far the case moved the clock from that start.
type scanProbe struct {
	clock  *clocktest.Fake
	handed func(run, claims int)
	pace   scanPace

	mu        sync.Mutex
	firstRun  time.Time
	lastClaim time.Time
	claims    int
	noticed   map[int]time.Time
}

// seams are the scan's test seams for one Open: c, the clock the scan runs on, and watch, which
// wraps the bucket each run reads (nil reads it as it is), under the count of the runs' watches.
func (p *scanProbe) seams(c clock.Clock, watch func(bus.KeyValue) bus.KeyValue) roleScanSeams {
	return roleScanSeams{
		clock: c,
		watch: func(kv bus.KeyValue) bus.KeyValue {
			if watch != nil {
				kv = watch(kv)
			}
			return p.pace.counted(kv)
		},
		run: func(n int, started time.Time) {
			p.mu.Lock()
			if n == 1 {
				p.firstRun = started
			}
			p.mu.Unlock()
			p.pace.began(n, started)
		},
		handed: func(run, claims int) {
			at := p.clock.Now()
			p.mu.Lock()
			p.lastClaim, p.claims = at, claims
			p.mu.Unlock()
			if p.handed != nil {
				p.handed(run, claims)
			}
		},
		noticed: func(run int) {
			at := p.clock.Now()
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.noticed == nil {
				p.noticed = map[int]time.Time{}
			}
			if _, seen := p.noticed[run]; !seen {
				p.noticed[run] = at
			}
		},
	}
}

// noticedAt is when the scan first noticed a reconnect while run was in progress, and whether it
// did.
func (p *scanProbe) noticedAt(run int) (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	at, seen := p.noticed[run]
	return at, seen
}

func (p *scanProbe) count() int { return p.pace.count() }

func (p *scanProbe) firstRunAt() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.firstRun
}

func (p *scanProbe) lastClaimAt() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastClaim
}

// claimsHanded is how many claims the run handed one last had been handed then.
func (p *scanProbe) claimsHanded() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.claims
}

// openResult is what one Open returned, and where the scan's clock stood when it did.
type openResult struct {
	registry *Registry
	err      error
	at       time.Time
}

// requireScanFailure fails the test unless err is a failed start that names the role claim scan.
func requireScanFailure(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("Open returned no error")
	}
	if !strings.Contains(err.Error(), "role claim scan") {
		t.Fatalf("Open's error does not name the role claim scan: %v", err)
	}
}

// requireDeadline fails the test unless Open failed naming the scan's deadline, between the
// deadline and five seconds after it on the scan's clock, counted from the first run's start.
func requireDeadline(t *testing.T, result openResult, probe *scanProbe) {
	t.Helper()
	requireScanFailure(t, result.err)
	if !errors.Is(result.err, errRoleClaimScanDeadline) || !strings.Contains(result.err.Error(), "deadline") {
		t.Fatalf("Open's error does not name the scan's deadline: %v", result.err)
	}
	if took := result.at.Sub(probe.firstRunAt()); took < roleClaimScanDeadline || took > roleClaimScanDeadline+5*time.Second {
		t.Fatalf("Open failed %s after the first run started on the scan's clock; want between %s and %s", took, roleClaimScanDeadline, roleClaimScanDeadline+5*time.Second)
	}
}

// requireRevisions fails the test unless got is want, claim for claim and revision for revision;
// whose names want in the failure.
func requireRevisions(t *testing.T, got, want map[string]uint64, whose string) {
	t.Helper()
	if maps.Equal(got, want) {
		return
	}
	var wrong []string
	for key, revision := range got {
		if want[key] != revision {
			wrong = append(wrong, fmt.Sprintf("%s at %d, want %d", key, revision, want[key]))
		}
	}
	for key, revision := range want {
		if _, held := got[key]; !held {
			wrong = append(wrong, fmt.Sprintf("%s missing, want %d", key, revision))
		}
	}
	slices.Sort(wrong)
	if len(wrong) > 10 {
		wrong = append(wrong[:10], fmt.Sprintf("and %d more", len(wrong)-10))
	}
	t.Fatalf("Open restored %d claims, not %s %d:\n%s", len(got), whose, len(want), strings.Join(wrong, "\n"))
}

// scanForwarder is the TCP forwarder the role claim scan's cases put between their client and its
// NATS server, as the Service in front of the hub was on 2026-11-22. It relays each connection it
// accepts to the server it points at. switchTo points it at another server and drops every
// connection it relays, as the Service's switch reset them; closePort stops it listening and drops
// them, so a reconnect fails at TCP, and openPort listens again. Its address is a port the kernel
// chose at an address of its own (testport.LoopbackIP), held for the test's life by a reservation
// that never listens (testport.Reserve): a dial while the port is closed is refused, and a program
// that binds the port without SO_REUSEADDR meanwhile is refused too. Its dials to the server end
// with the test command's guard (testwait.Context).
type scanForwarder struct {
	address string
	dials   context.Context

	mu       sync.Mutex
	listener net.Listener
	target   string
	// generation moves on at each switch and close, so a connection accepted before one is closed
	// rather than joined to the server it was dialing.
	generation int
	relayed    map[net.Conn]struct{}
	ended      bool
}

// startScanForwarder starts a forwarder to target, a server's host and port, until the test ends.
func startScanForwarder(t *testing.T, target string) *scanForwarder {
	t.Helper()
	ip := testport.LoopbackIP().To4()
	address := &syscall.SockaddrInet4{}
	copy(address.Addr[:], ip)
	reservation, port, err := testport.Reserve(address)
	if err != nil {
		t.Fatalf("reserve the forwarder's port: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Close(reservation) })
	f := &scanForwarder{address: net.JoinHostPort(ip.String(), strconv.Itoa(port)), dials: testwait.Context(t), target: target, relayed: map[net.Conn]struct{}{}}
	if err := f.openPort(); err != nil {
		t.Fatalf("start the forwarder: %v", err)
	}
	t.Cleanup(f.end)
	return f
}

// url is the NATS URL of the forwarder's address.
func (f *scanForwarder) url() string { return "nats://" + f.address }

// openPort listens on the forwarder's address, beside its reservation. It listens no more once the
// test has ended.
func (f *scanForwarder) openPort() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ended || f.listener != nil {
		return nil
	}
	listener, err := net.Listen("tcp", f.address)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", f.address, err)
	}
	f.listener = listener
	go f.serve(listener)
	return nil
}

// closePort stops listening, so a dial of the address is refused, and drops every connection.
func (f *scanForwarder) closePort() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listener != nil {
		_ = f.listener.Close()
		f.listener = nil
	}
	f.dropLocked()
}

// switchTo points the forwarder at target and drops every connection, so the client's reconnect
// reaches target.
func (f *scanForwarder) switchTo(target string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.target = target
	f.dropLocked()
}

// end closes the forwarder for good, as the test ends.
func (f *scanForwarder) end() {
	f.closePort()
	f.mu.Lock()
	f.ended = true
	f.mu.Unlock()
}

// dropLocked closes every relayed connection and moves the generation on. f.mu is held.
func (f *scanForwarder) dropLocked() {
	f.generation++
	for conn := range f.relayed {
		_ = conn.Close()
		delete(f.relayed, conn)
	}
}

// serve joins each connection listener accepts to the server the forwarder points at.
func (f *scanForwarder) serve(listener net.Listener) {
	for {
		client, err := listener.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		target, generation := f.target, f.generation
		f.mu.Unlock()
		go f.join(client, target, generation)
	}
}

// join dials target for client and relays the two into each other, unless the forwarder has been
// switched or closed since client was accepted.
func (f *scanForwarder) join(client net.Conn, target string, generation int) {
	server, err := (&net.Dialer{}).DialContext(f.dials, "tcp", target)
	if err != nil {
		_ = client.Close()
		return
	}
	f.mu.Lock()
	if f.generation != generation {
		f.mu.Unlock()
		_ = client.Close()
		_ = server.Close()
		return
	}
	f.relayed[client] = struct{}{}
	f.relayed[server] = struct{}{}
	f.mu.Unlock()
	go f.relay(server, client)
	go f.relay(client, server)
}

// relay copies src to dst until either ends, then closes both, which ends the other direction too.
func (f *scanForwarder) relay(dst, src net.Conn) {
	_, _ = io.Copy(dst, src)
	_ = dst.Close()
	_ = src.Close()
	f.mu.Lock()
	delete(f.relayed, dst)
	delete(f.relayed, src)
	f.mu.Unlock()
}

// gatedKV hands each watch's entries on as the case's gate lets them through (scanGate).
type gatedKV struct {
	jetstream.KeyValue

	gate *scanGate
}

func (k gatedKV) Watch(ctx context.Context, keys string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	watcher, err := k.KeyValue.Watch(ctx, keys, opts...)
	if err != nil {
		return nil, err
	}
	return k.gate.add(watcher), nil
}

// scanGate is what the watch of each run of a case's scan hands on: a stand-in for a bucket's
// server whose delivery the case holds at an entry of a run, then stalls or ends, or paces, so what
// a run reads, and when it ends, are events the case chooses rather than spans of the real clock.
// Its watches take every entry the real watch delivers as it comes, so nats.go is never held up
// behind them, and hand them on in order as the gate lets them; watch n is run n's.
//
// Each of the first heldRuns watches hands on held entries and holds the rest, until the case
// stalls it (stall: it hands nothing more, so only the run's idle bound ends the run), ends it
// (end: the end of the bucket's existing keys next, so the run ends complete on what it read) or
// closes it (close: its updates close, as nats.go closes them when a watch's subscription ends,
// and the run ends short of the bucket). A stepped gate's watches hand one entry for each step the
// case takes, and the end of the existing keys at once after the last, as a bucket whose server
// delivers it slowly and steadily; the case moves the scan's clock between the steps. Every other
// watch hands each entry on as it comes.
type scanGate struct {
	held     int
	heldRuns int
	stepped  bool

	mu       sync.Mutex
	watchers []*gatedWatcher
}

// watch is the gate as a case's watch seam: kv with each watch made through the gate.
func (g *scanGate) watch(kv bus.KeyValue) bus.KeyValue {
	return bus.NewKeyValue(kv.Legacy(), gatedKV{KeyValue: kv.WatchHandle(), gate: g}, kv.Streams())
}

// add makes the next run's watch over watcher, the real one.
func (g *scanGate) add(watcher jetstream.KeyWatcher) *gatedWatcher {
	g.mu.Lock()
	defer g.mu.Unlock()
	w := &gatedWatcher{KeyWatcher: watcher, updates: make(chan jetstream.KeyValueEntry), stop: make(chan struct{}), changed: make(chan struct{}, 1), budget: -1}
	switch {
	case g.stepped:
		w.budget = 0
	case len(g.watchers) < g.heldRuns:
		w.budget = g.held
	}
	g.watchers = append(g.watchers, w)
	go w.hand(watcher.Updates())
	return w
}

// watcher is run's watch, or nil while it has none.
func (g *scanGate) watcher(run int) *gatedWatcher {
	g.mu.Lock()
	defer g.mu.Unlock()
	if run > len(g.watchers) {
		return nil
	}
	return g.watchers[run-1]
}

// awaitHeld waits until run's watch has handed on its held entries and holds the rest.
func (g *scanGate) awaitHeld(t *testing.T, scan *scanCase, run int) {
	t.Helper()
	scan.until(t, fmt.Sprintf("run %d's watch to hand on %d entries and hold the rest", run, g.held), func() (string, bool) {
		w := g.watcher(run)
		if w == nil {
			return "no watch made", false
		}
		handed, _ := w.state()
		return fmt.Sprintf("%d entries handed on", handed), handed >= g.held
	})
}

// awaitRead waits until run's watch has taken every existing key of the bucket from the real
// watch, their end included, so what it hands on no longer depends on the connection.
func (g *scanGate) awaitRead(t *testing.T, scan *scanCase, run int) {
	t.Helper()
	scan.until(t, fmt.Sprintf("run %d's watch to take the bucket's every existing key from the real watch", run), func() (string, bool) {
		w := g.watcher(run)
		if w == nil {
			return "no watch made", false
		}
		_, read := w.state()
		return fmt.Sprintf("read %t", read), read
	})
}

// step lets run's watch hand on one more entry.
func (g *scanGate) step(run int) { g.watcher(run).change(func(w *gatedWatcher) { w.budget++ }) }

// stall has run's watch hand on nothing more.
func (g *scanGate) stall(run int) { g.watcher(run).change(func(w *gatedWatcher) { w.stalled = true }) }

// end has run's watch hand on the end of the bucket's existing keys next, and nothing after it.
func (g *scanGate) end(run int) { g.watcher(run).change(func(w *gatedWatcher) { w.ending = true }) }

// close has run's watch close its updates, with nothing more handed on.
func (g *scanGate) close(run int) { g.watcher(run).change(func(w *gatedWatcher) { w.closing = true }) }

// gatedWatcher is the watch a scanGate hands a run. Its Stop stops the real watcher and closes its
// own updates, so the drain a scan leaves behind (bus.DrainWatcher) ends.
type gatedWatcher struct {
	jetstream.KeyWatcher

	updates  chan jetstream.KeyValueEntry
	stop     chan struct{}
	stopping sync.Once
	// changed carries a signal after each change the case makes, so hand reads the state again.
	changed chan struct{}

	mu sync.Mutex
	// queue is what the real watch has delivered and this has not handed on, read whether the real
	// watch has delivered the end of the existing keys, and closed whether it closed its updates.
	queue  []jetstream.KeyValueEntry
	read   bool
	closed bool
	// budget is how many more entries it may hand on, -1 for any, and handed how many it has.
	budget int
	handed int
	// stalled, ending and closing are the case's stall, end and close (scanGate); finished is
	// whether it has handed the end of the existing keys.
	stalled, ending, closing, finished bool
}

func (w *gatedWatcher) Updates() <-chan jetstream.KeyValueEntry { return w.updates }

// Stop stops the watch. A watch the case stalled stops its real watcher on a goroutine of its own:
// nats.go's stop deletes the watch's consumer by a request (natsgo.Subscription.Unsubscribe), which
// waits out nats.go's request timeout when the connection cannot reach the server, and a stalled
// run is one whose end on its idle bound the case reads, not that teardown. Every other watch's
// stop waits for it, as a scan's does.
func (w *gatedWatcher) Stop() error {
	w.stopping.Do(func() { close(w.stop) })
	w.mu.Lock()
	aside := w.stalled
	w.mu.Unlock()
	if aside {
		go func() { _ = w.KeyWatcher.Stop() }()
		return nil
	}
	return w.KeyWatcher.Stop()
}

// change makes the case's change to the watch and has hand read it.
func (w *gatedWatcher) change(apply func(*gatedWatcher)) {
	w.mu.Lock()
	apply(w)
	w.mu.Unlock()
	select {
	case w.changed <- struct{}{}:
	default:
	}
}

// state is how many entries the watch has handed on, and whether it has read every existing key.
func (w *gatedWatcher) state() (handed int, read bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.handed, w.read
}

// hand takes what the real watch delivers and hands it on as the gate lets it, until the watch is
// stopped or closed, or the real watch closes with nothing left to hand on. It keeps reading the
// real watch after that, until it closes.
func (w *gatedWatcher) hand(real <-chan jetstream.KeyValueEntry) {
	defer close(w.updates)
	defer func() {
		if real != nil {
			go func() {
				for range real {
				}
			}()
		}
	}()
	for {
		next, queued, send, done := w.next()
		if done {
			return
		}
		var out chan<- jetstream.KeyValueEntry
		if send {
			out = w.updates
		}
		select {
		case entry, open := <-real:
			w.mu.Lock()
			if open {
				w.queue = append(w.queue, entry)
				w.read = w.read || entry == nil
			} else {
				w.closed = true
			}
			w.mu.Unlock()
			if !open {
				real = nil
			}
		case out <- next:
			w.took(next, queued)
		case <-w.changed:
		case <-w.stop:
			return
		}
	}
}

// next is what hand may hand on now and whether it may: nothing once the case stalled the watch or
// the end of the existing keys has gone; that end next once the case ended it; otherwise the first
// entry it holds, an entry while the budget lasts and the end of the existing keys whenever it
// comes next. queued is whether that is the first of the queue, and done whether the watch's
// updates close now: the case closed it, or the real watch closed with nothing left to hand on.
func (w *gatedWatcher) next() (entry jetstream.KeyValueEntry, queued, send, done bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case w.closing:
		return nil, false, false, true
	case w.finished || w.stalled:
		return nil, false, false, false
	case w.ending:
		return nil, false, true, false
	case len(w.queue) == 0:
		return nil, false, false, w.closed
	case w.queue[0] == nil, w.budget != 0:
		return w.queue[0], true, true, false
	}
	return nil, false, false, false
}

// took records that hand handed entry on, the first of the queue when queued.
func (w *gatedWatcher) took(entry jetstream.KeyValueEntry, queued bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if queued {
		w.queue = w.queue[1:]
	}
	if entry == nil {
		w.finished = true
		return
	}
	w.handed++
	if w.budget > 0 {
		w.budget--
	}
}
