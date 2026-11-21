//go:build e2e

// Check w3 of this change's verification (R3: the current process wins), run on the rig (RIG.md) as
// the check states it. scripts/e2e/lockout-generations.sh builds this driver and runs it; with
// --base-commit it runs step 1 against the base's code, the check's negative control.
//
// Three claims of three issues, each an implementer whose real pane has answered its start's
// task and stands idle, so each step reads a claim no other step has touched:
//
//   - the first claim: its shim A reported gone while it runs (rigRuntime.detach), its next
//     generation's shim B, then a second shim C of B's generation (steps 1 and 2);
//   - the second: its shim A2 reads the stop frame and does not stop (the relay drops the frame,
//     rigRelay.makeStubborn), for ten minutes past the rig's 6-minute registration deadline, then
//     its connection is cut from the daemon's side while it runs (steps 3 and 4);
//   - the third: the same lock-out, with a daemon restart between the cut and its shim's redial
//     (step 5).
//
// Step 6 starts a shim with a boot token the daemon never minted.
//
// What a step reads is the daemon's own log (the hellos it refused and why, with their claims and
// generations), the relay's record of every connection (each hello, the frames the daemon sent,
// when it closed), the runtime's (each process's end, and every read of a process the daemon
// made), the operator's claim listing, and the controller's topic as the stream stored it. Every
// daemon text a step matches is written here as a literal, so step 1 builds against the base,
// whose code names none of the head's symbols: lockout-generations.sh's negative control lays this
// file and the rig's over the base's tree and runs step 1 there.
package daemon

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The daemon's lines a step reads, as the worker stream, the machine and the R6 writer write them
// (internal/stream listener.go and lockout.go, internal/supervise machine.go and table.go,
// internal/fleetalert).
const (
	rejectedHelloPrefix   = "worker-stream: rejected hello ("
	refusedAlreadyBound   = "worker-stream: rejected hello (already bound to a live stream)"
	refusedOlderBound     = "worker-stream: rejected hello (already bound to a live stream of an older generation, which has been told to stop)"
	refusedStale          = "worker-stream: rejected hello (stale worker generation)"
	refusedUnknown        = "worker-stream: rejected hello (unknown boot token)"
	droppedStaleEvent     = "supervise: dropped a stale event"
	registrationHeld      = "supervise: the worker stream refuses this launch while an older generation's connection is bound; its registration deadline is held, charging nothing"
	fleetAlertPrefix      = "LEGION FLEET ALERT: "
	newerProcessRefused   = "newer_process_refused"
	workerLockedOutNotice = "worker-locked-out"
)

// The bounds the check reads against: the shim's grace before it kills the process it wraps
// (shim.DefaultGrace), the 2 minutes R3 gives the newer process, the 5 minutes after which a
// lock-out is reported (stream.LockOutReportAfter), the 10 minutes the check watches it past the
// rig's 6-minute registration deadline (testConfig: 120 s × 3), and the allowance a loaded host
// is given over each moment it measures.
const (
	shimGrace          = 5 * time.Second
	newerBindsWithin   = 2 * time.Minute
	lockOutReported    = 5 * time.Minute
	lockOutWatched     = 10 * time.Minute
	rigAllowance       = 5 * time.Second
	reportAllowance    = 90 * time.Second
	stopFrameWait      = 90 * time.Second
	launchWait         = 3 * time.Minute
	idleWait           = 6 * time.Minute
	unknownTokenWindow = 20 * time.Second
)

// lockoutLive is one run of w3.
type lockoutLive struct {
	*rig
	first, stubborn, restarted string
	// b is step 1's newer process, which step 2's second shim names the generation of; a2 and b2
	// are step 3's older process that will not stop and its newer one, which step 4 ends.
	b, a2, b2 *rigLaunch
}

// TestTheCurrentProcessWinsItsWorkerStreamLive runs w3's steps in the order each one's state
// follows from the last.
func TestTheCurrentProcessWinsItsWorkerStreamLive(t *testing.T) {
	r := newLockoutLive(t)
	r.step("step1-the-older-process-is-told-to-stop-and-the-newer-binds", r.olderIsToldToStop)
	r.step("step2-a-second-shim-of-the-bound-generation-is-refused", r.secondShimIsRefused)
	r.step("step3-an-older-process-that-will-not-stop-holds-the-claim-for-ten-minutes", r.olderHoldsTenMinutes)
	r.step("step4-the-cut-older-process-is-refused-and-told-and-the-newer-stays-bound", r.cutOlderIsRefused)
	r.step("step5-after-a-daemon-restart-the-older-process-is-still-refused-and-told", r.restartKeepsTheOlderRefused)
	r.step("step6-a-boot-token-the-daemon-never-minted-keeps-its-shim-running", r.unmintedTokenRuns)
}

func newLockoutLive(t *testing.T) *lockoutLive {
	t.Helper()
	r := &lockoutLive{rig: startRig(t, rigOptions{script: "scripts/e2e/lockout-generations.sh"})}
	r.first, r.stubborn, r.restarted = r.cfg.Project+"-1", r.cfg.Project+"-2", r.cfg.Project+"-3"
	var seeds []rigSeed
	for _, key := range []string{r.first, r.stubborn, r.restarted} {
		seeds = append(seeds, rigSeed{issue: key, phase: phase.Implementing, role: claim.RoleImplementer,
			task: "Reply with the one word READY and nothing else. Call no tool."})
	}
	r.seed(seeds...)
	r.boot()
	r.registerController()
	for _, key := range []string{r.first, r.stubborn, r.restarted} {
		token := r.token(key, claim.RoleImplementer)
		r.awaitReady(token)
		r.awaitIdle(token)
	}
	for _, c := range r.nats.connections() {
		r.note("NATS connection %d %q as %s", c.CID, c.Name, r.nats.userOf(c))
	}
	return r
}

// awaitIdle waits for token's agent to answer its start's task and stand idle, holding nothing.
func (r *lockoutLive) awaitIdle(token claim.Token) {
	r.t.Helper()
	r.await(string(token)+" to answer its task and stand idle", idleWait, func() bool {
		c := r.claim(token)
		if c.State == string(supervise.StateFailed) {
			r.t.Fatalf("%s failed; its workers' output is under %s", token, r.rt.paneLogs)
		}
		return c.State == string(supervise.StateIdle) && c.Pending == nil
	})
	launch := r.runtime.current(token)
	r.note("%s is idle at generation %d (pid %d)", token, launch.spec.Generation, launch.pane.pid)
}

// ---- the steps ----------------------------------------------------------------------------------

// olderIsToldToStop is the check's first step: shim A bound, A's process reported gone while it
// keeps running and connected, the daemon's next generation's shim B. A gets the stop frame and
// its process ends within its grace; B binds once A's connection closes, within 2 minutes of its
// first hello, and answers a request over the worker stream; A's close leaves the claim bound to
// B, with no probe of the claim. Its negative control is this step at the base: A gets no stop
// frame, and the daemon refuses B, `already bound to a live stream`, until A's connection ends.
func (r *lockoutLive) olderIsToldToStop() {
	token := r.token(r.first, claim.RoleImplementer)
	from := r.daemon.count()
	before := r.claim(token)
	a := r.runtime.detach(token)
	reported := time.Now()
	r.note("%s: the runtime reports generation %d's process (pid %d) gone at %s, and it keeps running and connected; the claim was %s with %d launch failures",
		token, a.spec.Generation, a.pane.pid, clockAt(reported), before.State, before.Budgets.LaunchFailures)

	b := r.awaitNextLaunch(token, a)
	bHello := r.awaitHello(b.spec.BootToken, b.at, launchWait)
	r.note("the daemon launched generation %d (pid %d) at %s; shim B's first hello at %s", b.spec.Generation, b.pane.pid, clockAt(b.at), clockAt(bHello))
	bound := r.boundConn(a.spec.BootToken)

	stop, told := r.awaitFrameOn(a.spec.BootToken, bound.id, shimwire.TypeShutdown, bHello, stopFrameWait)
	if !told {
		r.baseLockOut(token, a, b, bHello, bound)
	}
	aEnded := r.awaitEnd(a, stop.at.Add(shimGrace+rigAllowance))
	aConn := r.connByID(a.spec.BootToken, bound.id)
	r.note("A (generation %d) got the stop frame at %s, %s after B's first hello; its process ended at %s, %s after the frame (grace %s); its connection closed at %s; its frames: %s",
		a.spec.Generation, clockAt(stop.at), stop.at.Sub(bHello).Round(time.Millisecond), clockAt(aEnded),
		aEnded.Sub(stop.at).Round(time.Millisecond), shimGrace, clockAt(aConn.closedAt), aConn.typesToShim())
	if aEnded.Sub(stop.at) > shimGrace+rigAllowance {
		r.fails("older-ends-in-grace", "A's process ended %s after its stop frame, past its %s grace", aEnded.Sub(stop.at), shimGrace)
	}

	bConn := r.awaitBound(b.spec.BootToken, bHello, newerBindsWithin+rigAllowance)
	r.note("B (generation %d) bound at %s: %s after its first hello, %s after A's connection closed",
		b.spec.Generation, clockAt(bConn.ackAt), bConn.ackAt.Sub(bHello).Round(time.Millisecond), bConn.ackAt.Sub(aConn.closedAt).Round(time.Millisecond))
	if bConn.ackAt.Before(aConn.closedAt) {
		r.fails("newer-binds-after-older-closes", "B bound at %s, before A's connection closed at %s", clockAt(bConn.ackAt), clockAt(aConn.closedAt))
	}
	if bConn.ackAt.Sub(bHello) > newerBindsWithin {
		r.fails("newer-binds-within-2m", "B bound %s after its first hello, past %s", bConn.ackAt.Sub(bHello), newerBindsWithin)
	}
	refusals := r.logsFrom(from, func(rec loggedRecord) bool {
		return strings.HasPrefix(rec.Message, rejectedHelloPrefix) && rec.Attrs["claim"] == string(token)
	})
	if len(refusals) == 0 {
		r.fails("refusal-names-generations", "the daemon logged no refused hello of %s between B's first hello and its bind", token)
	}
	for _, rec := range refusals {
		if rec.Message != refusedOlderBound || !generationIs(rec.Attrs["generation"], b.spec.Generation) || !generationIs(rec.Attrs["boundGeneration"], a.spec.Generation) {
			r.fails("refusal-names-generations", "a refused hello of %s reads %q %v, want %q naming generation %d and bound generation %d",
				token, rec.Message, rec.Attrs, refusedOlderBound, b.spec.Generation, a.spec.Generation)
		}
	}
	r.note("the daemon refused B %d times, %s to %s; the first: %s", len(refusals), clockAt(refusals[0].at), clockAt(refusals[len(refusals)-1].at), describe(refusals[0]))

	r.await(string(token)+" to be live at generation "+fmt.Sprint(b.spec.Generation), launchWait, func() bool {
		c := r.claim(token)
		return c.Generation == b.spec.Generation && (c.State == string(supervise.StateReady) || c.State == string(supervise.StateIdle) || c.State == string(supervise.StateWorking))
	})
	time.Sleep(liveSettle)
	if probes := r.runtime.probesOf(token, aConn.closedAt); len(probes) != 0 {
		r.fails("no-probe", "A's close was read as the claim's: the daemon read %s's process %d times from %s, first %s at %s",
			token, len(probes), clockAt(aConn.closedAt), probes[0].loc.Incarnation, clockAt(probes[0].at))
	}
	dropped := r.logsFrom(from, func(rec loggedRecord) bool {
		return rec.Message == droppedStaleEvent && rec.Attrs["claim"] == string(token) && rec.Attrs["event"] == "StreamClosed"
	})
	if len(dropped) != 1 || dropped[0].Attrs["got"] != fmt.Sprint(a.spec.Generation) {
		r.fails("close-is-not-the-claims", "the daemon logged %d dropped StreamClosed events of %s, want one naming generation %d: %v",
			len(dropped), token, a.spec.Generation, dropped)
	}
	after := r.claim(token)
	r.note("A's close was dropped as stale (%s) and the daemon read no process of the claim from it; %s is %s at generation %d, %d launch failures",
		describe(dropped[0]), token, after.State, after.Generation, after.Budgets.LaunchFailures)

	asked := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	conn, ok := r.conn(token)
	if !ok {
		r.fails("newer-answers", "the daemon holds no connection of %s", token)
	}
	state, err := conn.GetState(ctx)
	if err != nil {
		r.fails("newer-answers", "get_state over %s's worker stream: %v", token, err)
	}
	answered, by := r.answeredBy(token, shimwire.TypeGetState, asked)
	if !answered || by.bootToken != b.spec.BootToken {
		r.fails("newer-answers", "the get_state of %s was answered by %s, want shim B (generation %d)", token, by.label(), b.spec.Generation)
	}
	r.note("a get_state over the worker stream at %s was answered by %s (streaming %v)", clockAt(asked), by.label(), state.IsStreaming)
	r.b = b
}

// baseLockOut is what step 1 reads where A gets no stop frame, which is the base's code: the
// daemon refuses B, `already bound to a live stream`, for as long as A's connection lives. The step
// judges the stop frame as the window it waited for one ends, before any wait for B's bind: it
// fails there with the refusals the daemon logged in that window, up to A's connection's end where
// that connection ended in it and through the window where it did not, which the script's
// negative control reads back. So the base fails at its stop frame whether or not A's connection
// ends, and a base whose daemon refused nothing fails elsewhere.
func (r *lockoutLive) baseLockOut(token claim.Token, a, b *rigLaunch, bHello time.Time, bound relayConn) {
	windowEnd := time.Now()
	aConn := r.connByID(a.spec.BootToken, bound.id)
	aState := "still open"
	if !aConn.closedAt.IsZero() {
		aState = "closed at " + clockAt(aConn.closedAt)
	}
	// The base's line names no claim, so each one counts there; a line that names one counts only
	// for B's claim.
	refused := r.logs(func(rec loggedRecord) bool {
		claimed, named := rec.Attrs["claim"]
		return rec.Message == refusedAlreadyBound && (!named || claimed == string(token)) &&
			!rec.at.Before(bHello.Add(-time.Second)) && !rec.at.After(windowEnd)
	})
	r.note("A (generation %d) got no stop frame in %s of B's first hello; A's frames: %s; A's connection is %s; the daemon logged %d refused hellos `already bound to a live stream` from B's first hello to %s",
		a.spec.Generation, stopFrameWait, aConn.typesToShim(), aState, len(refused), clockAt(windowEnd))
	for _, rec := range refused {
		r.note("  %s", describe(rec))
	}
	if len(refused) == 0 {
		r.fails("base-refuses-newer", "A (generation %d) got no stop frame within %s of B's (generation %d) first hello at %s, and the daemon logged no refused hello `already bound to a live stream` in that window",
			a.spec.Generation, stopFrameWait, b.spec.Generation, clockAt(bHello))
	}
	first, last := clockAt(refused[0].at), clockAt(refused[len(refused)-1].at)
	if !aConn.closedAt.IsZero() && !aConn.closedAt.After(windowEnd) {
		after := 0
		for _, rec := range refused {
			if rec.at.After(aConn.closedAt) {
				after++
			}
		}
		r.fails("stop-frame", "A (generation %d) got no stop frame within %s of B's (generation %d) first hello at %s; the daemon refused B `already bound to a live stream` %d times, from %s to %s, until A's connection ended at %s, %d of them after it",
			a.spec.Generation, stopFrameWait, b.spec.Generation, clockAt(bHello), len(refused), first, last, clockAt(aConn.closedAt), after)
	}
	r.fails("stop-frame", "A (generation %d) got no stop frame within %s of B's (generation %d) first hello at %s; the daemon refused B `already bound to a live stream` %d times, from %s to %s, through the window, and A's connection had not ended by its end at %s",
		a.spec.Generation, stopFrameWait, b.spec.Generation, clockAt(bHello), len(refused), first, last, clockAt(windowEnd))
}

// secondShimIsRefused is the check's second step: a second shim C with B's boot token while B is
// bound is refused, and its log line names the claim and both connections' generations. It is
// refused as today, told nothing, and keeps dialling; B stays bound.
func (r *lockoutLive) secondShimIsRefused() {
	if r.b == nil {
		r.fails("after-step1", "step 1 left no bound newer process")
	}
	token := r.token(r.first, claim.RoleImplementer)
	conn, _ := r.conn(token)
	sequence := sequenceOf(conn)
	from := r.daemon.count()
	started := time.Now()
	c := r.runtime.startShim("C", r.b.spec.BootToken, "sleep", "3600")
	defer c.kill()
	var refusals []loggedRecord
	r.await("two refused hellos of shim C", time.Minute, func() bool {
		refusals = r.logsFrom(from, func(rec loggedRecord) bool {
			return strings.HasPrefix(rec.Message, rejectedHelloPrefix) && rec.Attrs["claim"] == string(token)
		})
		return len(refusals) >= 2
	})
	for _, rec := range refusals {
		if rec.Message != refusedAlreadyBound || !generationIs(rec.Attrs["generation"], r.b.spec.Generation) || !generationIs(rec.Attrs["boundGeneration"], r.b.spec.Generation) {
			r.fails("second-shim-refused", "a refused hello of C reads %q %v, want %q naming generation %d and bound generation %d",
				rec.Message, rec.Attrs, refusedAlreadyBound, r.b.spec.Generation, r.b.spec.Generation)
		}
	}
	var cConns []relayConn
	for _, rc := range r.relay.connsOf(r.b.spec.BootToken) {
		if rc.opened.After(started) {
			cConns = append(cConns, rc)
			if !rc.ackAt.IsZero() || len(rc.toShim) != 0 {
				r.fails("second-shim-refused", "C's %s was sent %s, want nothing: a second process of the bound generation is refused as today", rc.label(), rc.typesToShim())
			}
		}
	}
	if !c.running() {
		r.fails("second-shim-refused", "C's process ended; a shim refused as a second process of the bound generation keeps running")
	}
	now, _ := r.conn(token)
	if sequenceOf(now) != sequence {
		r.fails("newer-stays-bound", "%s's connection moved from registration %d to %d while C dialled", token, sequence, sequenceOf(now))
	}
	r.note("C (pid %d), dialling with B's boot token, was refused %d times in %d connections, each sent nothing, and still runs; the first refusal: %s; B stays bound (registration %d)",
		c.pid, len(refusals), len(cConns), describe(refusals[0]), sequence)
}

// olderHoldsTenMinutes is the check's third step: the first step again with a shim A2 that reads
// the stop frame and does not stop. For 10 minutes, past the rig's 6-minute registration deadline,
// the newer shim stays refused and A2 stays bound, the claim is charged no launch failure and no
// newer generation is launched; 5 minutes after the newer shim's first hello the controller's
// notice names the claim and both generations, once, and the daemon writes R6's line, once.
func (r *lockoutLive) olderHoldsTenMinutes() {
	token := r.token(r.stubborn, claim.RoleImplementer)
	from := r.daemon.count()
	a2 := r.runtime.current(token)
	r.relay.makeStubborn(a2.spec.BootToken)
	r.runtime.detach(token)
	r.note("%s: A2 (generation %d, pid %d) reads every stop frame and does not stop; its process is reported gone at %s",
		token, a2.spec.Generation, a2.pane.pid, clockAt(time.Now()))
	b2 := r.awaitNextLaunch(token, a2)
	b2Hello := r.awaitHello(b2.spec.BootToken, b2.at, launchWait)
	bound := r.boundConn(a2.spec.BootToken)
	stop, told := r.awaitFrameOn(a2.spec.BootToken, bound.id, shimwire.TypeShutdown, b2Hello, stopFrameWait)
	if !told {
		r.fails("stop-frame", "A2 got no stop frame within %s of the newer shim's first hello", stopFrameWait)
	}
	launched := r.claim(token)
	launches := len(r.runtime.launchesOf(token))
	r.note("generation %d (pid %d) launched at %s, first hello at %s; A2 was sent the stop frame at %s and the relay dropped it; the claim: %s, generation %d, %d launch failures",
		b2.spec.Generation, b2.pane.pid, clockAt(b2.at), clockAt(b2Hello), clockAt(stop.at), launched.State, launched.Generation, launched.Budgets.LaunchFailures)

	until := b2Hello.Add(lockOutWatched)
	samples := 0
	for time.Now().Before(until) {
		samples++
		c := r.claim(token)
		aConn := r.connByID(a2.spec.BootToken, bound.id)
		switch {
		case !a2.running() || !aConn.closedAt.IsZero():
			r.fails("older-stays-bound", "A2's process or connection ended at %s, %s into the lock-out", clockAt(aConn.closedAt), time.Since(b2Hello))
		case r.everBound(b2.spec.BootToken):
			r.fails("newer-stays-refused", "the newer shim bound %s into the lock-out", time.Since(b2Hello))
		case c.Generation != b2.spec.Generation || len(r.runtime.launchesOf(token)) != launches:
			r.fails("no-newer-generation", "the claim moved to generation %d with %d launches, %s into the lock-out", c.Generation, len(r.runtime.launchesOf(token)), time.Since(b2Hello))
		case c.Budgets.LaunchFailures != launched.Budgets.LaunchFailures:
			r.fails("no-launch-failure", "the claim was charged %d launch failures, %s into the lock-out", c.Budgets.LaunchFailures-launched.Budgets.LaunchFailures, time.Since(b2Hello))
		}
		if samples%8 == 1 {
			r.note("%s into the lock-out: A2 bound, the newer shim refused, %s %s at generation %d with %d launch failures",
				time.Since(b2Hello).Round(time.Second), token, c.State, c.Generation, c.Budgets.LaunchFailures)
		}
		time.Sleep(15 * time.Second)
	}
	end := r.claim(token)
	refusals := r.logsFrom(from, func(rec loggedRecord) bool {
		return rec.Message == refusedOlderBound && rec.Attrs["claim"] == string(token)
	})
	held := r.logsFrom(from, func(rec loggedRecord) bool {
		return rec.Message == registrationHeld && rec.Attrs["claim"] == string(token)
	})
	r.note("after %s: %s %s at generation %d with %d launch failures, %d launches; the newer shim was refused %d times; the registration deadline was held %d times, first at %s",
		lockOutWatched, token, end.State, end.Generation, end.Budgets.LaunchFailures, len(r.runtime.launchesOf(token)), len(refusals), len(held), firstAtOf(held))
	if len(held) == 0 {
		r.fails("deadline-held", "the registration deadline of %s never came round in %s, so the lock-out was not watched past it", token, lockOutWatched)
	}

	var notices []rigNotice
	for _, n := range r.controllerNotices(workerLockedOutNotice) {
		if n.claim == string(token) {
			notices = append(notices, n)
		}
	}
	if len(notices) != 1 {
		r.fails("controller-told-once", "the controller was told of %s's lock-out %d times, want once", token, len(notices))
	}
	notice := notices[0]
	r.note("the controller's notice, stored at %s (%s after the newer shim's first hello): kind %s, claim %s, reason %q",
		clockAt(notice.at), notice.at.Sub(b2Hello).Round(time.Second), notice.kind, notice.claim, notice.reason)
	if wait := notice.at.Sub(b2Hello); wait < lockOutReported || wait > lockOutReported+reportAllowance {
		r.fails("controller-told-at-5m", "the controller was told %s after the newer shim's first hello, want %s", wait, lockOutReported)
	}
	if !strings.Contains(notice.reason, fmt.Sprintf("generation %d", b2.spec.Generation)) || !strings.Contains(notice.reason, fmt.Sprintf("generation %d", a2.spec.Generation)) {
		r.fails("controller-told-generations", "the notice's reason %q does not name generations %d and %d", notice.reason, b2.spec.Generation, a2.spec.Generation)
	}
	alerts := r.logsFrom(from, func(rec loggedRecord) bool {
		return strings.HasPrefix(rec.Message, fleetAlertPrefix) && rec.Attrs["case"] == newerProcessRefused && rec.Attrs["claim"] == string(token)
	})
	if len(alerts) != 1 {
		r.fails("r6-line-once", "the daemon wrote R6's line for %s %d times, want once", token, len(alerts))
	}
	if wait := alerts[0].at.Sub(b2Hello); wait < lockOutReported || wait > lockOutReported+reportAllowance {
		r.fails("r6-line-at-5m", "R6's line came %s after the newer shim's first hello, want %s", wait, lockOutReported)
	}
	r.note("R6's line at %s (%s after the first hello): %s", clockAt(alerts[0].at), alerts[0].at.Sub(b2Hello).Round(time.Second), describe(alerts[0]))
	r.a2, r.b2 = a2, b2
}

// cutOlderIsRefused is the check's fourth step: A2's connection closed from the daemon's side
// while its process runs, as a network cut would; the newer shim binds; A2's shim dials again, its
// hello is refused as an older generation, it is told so and ends its process, and the claim
// stays bound to the newer shim.
func (r *lockoutLive) cutOlderIsRefused() {
	if r.a2 == nil || r.b2 == nil {
		r.fails("after-step3", "step 3 left no lock-out to end")
	}
	token := r.token(r.stubborn, claim.RoleImplementer)
	from := r.daemon.count()
	// A2's shim redials at once after a cut, and the check lets the newer shim bind first.
	r.relay.hold(r.a2.spec.BootToken)
	cut, closed := r.relay.cut(r.a2.spec.BootToken)
	b2Conn := r.awaitBound(r.b2.spec.BootToken, cut, newerBindsWithin)
	r.note("A2's connection cut at both ends at %s (%d closed) while its process runs; the newer shim (generation %d) bound at %s",
		clockAt(cut), closed, r.b2.spec.Generation, clockAt(b2Conn.ackAt))
	released := time.Now()
	r.relay.release(r.a2.spec.BootToken)
	redial := r.awaitRedialToldToStop(r.a2, released)
	ended := r.awaitEnd(r.a2, redial.stop.Add(shimGrace+rigAllowance))
	refused := r.awaitRefusal(from, token, refusedStale, r.a2.spec.Generation, time.Minute)
	if !generationIs(refused.Attrs["boundGeneration"], r.b2.spec.Generation) {
		r.fails("stale-refusal-names-generations", "the refusal of A2's redial reads %v, want bound generation %d", refused.Attrs, r.b2.spec.Generation)
	}
	r.note("A2 redialled at %s; the daemon refused it as an older generation (%s) and sent it the stop frame before closing (%s); A2's process ended at %s",
		clockAt(redial.conn.helloAt), describe(*refused), redial.conn.typesToShim(), clockAt(ended))
	r.expectBoundTo(token, r.b2)
}

// restartKeepsTheOlderRefused is the check's fifth step: the lock-out again, with a daemon restart
// between the cut and A3's redial. After the restart A3's hello is still refused as an older
// generation and its shim is told so and ends its process, since the daemon keeps every previous
// launch's boot-token hash; the base, which keeps only the current one, logs `rejected hello
// (unknown boot token)` every 5 s with A3 still running.
//
// The check asks nothing of the newer process after that, and this step reads none: the newer
// shim binds while A3 still runs, as the spec says a restart lets it (R3's design, "a daemon
// restart ... is no remedy"), so for those seconds two Oh My Pi processes run one session, and
// what becomes of the newer one is recorded here rather than held.
func (r *lockoutLive) restartKeepsTheOlderRefused() {
	token := r.token(r.restarted, claim.RoleImplementer)
	a3 := r.runtime.current(token)
	r.relay.makeStubborn(a3.spec.BootToken)
	r.runtime.detach(token)
	b3 := r.awaitNextLaunch(token, a3)
	b3Hello := r.awaitHello(b3.spec.BootToken, b3.at, launchWait)
	bound := r.boundConn(a3.spec.BootToken)
	if _, told := r.awaitFrameOn(a3.spec.BootToken, bound.id, shimwire.TypeShutdown, b3Hello, stopFrameWait); !told {
		r.fails("stop-frame", "A3 got no stop frame within %s of the newer shim's first hello", stopFrameWait)
	}
	r.note("%s locked out: A3 (generation %d, pid %d) bound and not stopping, generation %d refused from %s",
		token, a3.spec.Generation, a3.pane.pid, b3.spec.Generation, clockAt(b3Hello))
	r.relay.hold(a3.spec.BootToken)
	cut, _ := r.relay.cut(a3.spec.BootToken)
	stopped, serving := r.restartDaemon()
	from := r.daemon.count()
	r.note("A3's connection cut at %s; the daemon stopped at %s and served again at %s, A3's redials held meanwhile", clockAt(cut), clockAt(stopped), clockAt(serving))
	b3Conn := r.awaitBound(b3.spec.BootToken, serving, newerBindsWithin)
	released := time.Now()
	r.relay.release(a3.spec.BootToken)
	redial := r.awaitRedialToldToStop(a3, released)
	ended := r.awaitEnd(a3, redial.stop.Add(shimGrace+rigAllowance))
	refused := r.awaitRefusal(from, token, refusedStale, a3.spec.Generation, time.Minute)
	r.note("after the restart the newer shim bound at %s; A3 redialled at %s, the daemon refused it as an older generation (%s), sent it %s, and A3's process ended at %s",
		clockAt(b3Conn.ackAt), clockAt(redial.conn.helloAt), describe(*refused), redial.conn.typesToShim(), clockAt(ended))
	unknown := r.logsFrom(from, func(rec loggedRecord) bool { return rec.Message == refusedUnknown })
	if len(unknown) != 0 {
		r.fails("stale-after-restart", "the restarted daemon refused %d hellos as unknown boot tokens: %s", len(unknown), describe(unknown[0]))
	}
	time.Sleep(liveSettle)
	after := r.claim(token)
	r.note("%s after A3 ended: %s at generation %d, %d launch failures, %d deaths; the newer shim had bound while A3 still ran on the same session",
		token, after.State, after.Generation, after.Budgets.LaunchFailures, after.Budgets.Deaths)
}

// unmintedTokenRuns is the check's sixth step: a shim dialling with a boot token the daemon never
// minted is refused as an unknown token, told nothing, and keeps running, because an unknown token
// can also mean a wrong or restored database or another project's daemon.
func (r *lockoutLive) unmintedTokenRuns() {
	token := "never-minted-" + randomSuffix(r.t)
	from := r.daemon.count()
	started := time.Now()
	s := r.runtime.startShim("unminted", token, "sleep", "3600")
	defer s.kill()
	time.Sleep(unknownTokenWindow)
	refusals := r.logsFrom(from, func(rec loggedRecord) bool { return rec.Message == refusedUnknown })
	conns := r.relay.connsOf(token)
	for _, c := range conns {
		if len(c.toShim) != 0 {
			r.fails("unknown-told-nothing", "the shim's %s was sent %s, want nothing", c.label(), c.typesToShim())
		}
	}
	if len(refusals) < 3 || len(conns) < 3 {
		r.fails("unknown-refused", "the daemon refused %d hellos as unknown in %d connections over %s, want the shim dialling again and again", len(refusals), len(conns), unknownTokenWindow)
	}
	if !s.running() {
		r.fails("unknown-keeps-running", "the shim's process ended; a shim refused for a boot token the daemon never minted keeps running")
	}
	r.note("a shim (pid %d) with a token the daemon never minted dialled %d times in %s from %s; the daemon refused each, %d lines `%s` (first %s, last %s), sent it nothing, and it still runs",
		s.pid, len(conns), unknownTokenWindow, clockAt(started), len(refusals), refusedUnknown, clockAt(refusals[0].at), clockAt(refusals[len(refusals)-1].at))
}

// ---- the reads the steps make -------------------------------------------------------------------

// awaitNextLaunch waits for the daemon to launch token again after prev.
func (r *lockoutLive) awaitNextLaunch(token claim.Token, prev *rigLaunch) *rigLaunch {
	r.t.Helper()
	var next *rigLaunch
	r.await("the daemon's launch of "+string(token)+"'s next generation", launchWait, func() bool {
		if c := r.runtime.current(token); c != prev {
			next = c
			return true
		}
		return false
	})
	if next.spec.Generation <= prev.spec.Generation {
		r.fails("next-generation", "%s's next launch is generation %d, after %d", token, next.spec.Generation, prev.spec.Generation)
	}
	return next
}

// awaitHello is the moment the first hello of bootToken reached the relay at or after from.
func (r *lockoutLive) awaitHello(bootToken string, from time.Time, within time.Duration) time.Time {
	r.t.Helper()
	var at time.Time
	r.await("a hello", within, func() bool {
		for _, c := range r.relay.connsOf(bootToken) {
			if !c.helloAt.IsZero() && !c.helloAt.Before(from) {
				at = c.helloAt
				return true
			}
		}
		return false
	})
	return at
}

// boundConn is bootToken's connection the daemon acked and has not closed.
func (r *lockoutLive) boundConn(bootToken string) relayConn {
	r.t.Helper()
	for _, c := range r.relay.connsOf(bootToken) {
		if !c.ackAt.IsZero() && c.closedAt.IsZero() {
			return c
		}
	}
	r.t.Fatalf("the relay carries no bound connection of that launch")
	return relayConn{}
}

func (r *lockoutLive) connByID(bootToken string, id int) relayConn {
	for _, c := range r.relay.connsOf(bootToken) {
		if c.id == id {
			return c
		}
	}
	r.t.Fatalf("the relay has no connection %d of that launch", id)
	return relayConn{}
}

// awaitFrameOn waits for the daemon to send connection id a frame of typ at or after from.
func (r *lockoutLive) awaitFrameOn(bootToken string, id int, typ string, from time.Time, within time.Duration) (relayFrame, bool) {
	r.t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		for _, f := range r.connByID(bootToken, id).toShim {
			if f.typ == typ && !f.at.Before(from) {
				return f, true
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return relayFrame{}, false
}

// awaitBound waits for a connection of bootToken opened at or after from to be acked.
func (r *lockoutLive) awaitBound(bootToken string, from time.Time, within time.Duration) relayConn {
	r.t.Helper()
	var bound relayConn
	r.await("the newer shim to bind", within, func() bool {
		for _, c := range r.relay.connsOf(bootToken) {
			if !c.ackAt.IsZero() && !c.opened.Before(from.Add(-time.Second)) {
				bound = c
				return true
			}
		}
		return false
	})
	return bound
}

// everBound is whether any connection of bootToken was acked.
func (r *lockoutLive) everBound(bootToken string) bool {
	for _, c := range r.relay.connsOf(bootToken) {
		if !c.ackAt.IsZero() {
			return true
		}
	}
	return false
}

// awaitEnd waits for the launch's process to end, until by and a minute past it, and answers when
// the runtime saw it end.
func (r *lockoutLive) awaitEnd(launch *rigLaunch, by time.Time) time.Time {
	r.t.Helper()
	select {
	case <-launch.pane.done:
		return time.Now()
	case <-time.After(time.Until(by.Add(time.Minute))):
		r.fails("older-ends", "the process of generation %d (pid %d) still runs a minute past %s; its log is %s", launch.spec.Generation, launch.pane.pid, clockAt(by), launch.pane.log)
		return time.Time{}
	}
}

// redialTold is a shim's redial the daemon refused with the stop frame before its ack.
type redialTold struct {
	conn relayConn
	stop time.Time
}

// awaitRedialToldToStop waits for a connection of the launch's shim whose hello reached the relay
// at or after from, not held, that the daemon sent the stop frame with no ack before closing.
func (r *lockoutLive) awaitRedialToldToStop(launch *rigLaunch, from time.Time) redialTold {
	r.t.Helper()
	var told redialTold
	r.await("the older shim's redial to be refused with the stop frame", time.Minute, func() bool {
		for _, c := range r.relay.connsOf(launch.spec.BootToken) {
			// The relay holds a redial by its hello, not by its accept: a connection accepted
			// while the hold stood whose hello came after from reached the daemon, so it counts.
			if c.held || c.helloAt.IsZero() || c.helloAt.Before(from) || c.closedAt.IsZero() {
				continue
			}
			if !c.ackAt.IsZero() {
				r.fails("older-redial-refused", "the older shim's redial (%s) was acked", c.label())
			}
			if stop, ok := c.firstFrame(shimwire.TypeShutdown); ok {
				told = redialTold{conn: c, stop: stop.at}
				return true
			}
			r.fails("older-told-to-stop", "the older shim's redial (%s) closed with %s, no stop frame", c.label(), c.typesToShim())
		}
		return false
	})
	return told
}

// awaitRefusal waits for the daemon to log a refused hello of token reading message and naming
// generation.
func (r *lockoutLive) awaitRefusal(from int, token claim.Token, message string, generation uint64, within time.Duration) *loggedRecord {
	r.t.Helper()
	return r.awaitLogFrom("the refusal "+message, from, within, func(rec loggedRecord) bool {
		return rec.Message == message && rec.Attrs["claim"] == string(token) && generationIs(rec.Attrs["generation"], generation)
	})
}

// expectBoundTo holds that token's agent is live at launch's generation and that a request over
// its worker stream is answered by that launch's shim.
func (r *lockoutLive) expectBoundTo(token claim.Token, launch *rigLaunch) {
	r.t.Helper()
	r.await(string(token)+" live at generation "+fmt.Sprint(launch.spec.Generation), launchWait, func() bool {
		c := r.claim(token)
		return c.Generation == launch.spec.Generation &&
			(c.State == string(supervise.StateReady) || c.State == string(supervise.StateIdle) || c.State == string(supervise.StateWorking))
	})
	asked := time.Now()
	conn, ok := r.conn(token)
	if !ok {
		r.fails("newer-stays-bound", "the daemon holds no connection of %s", token)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := conn.GetState(ctx); err != nil {
		r.fails("newer-stays-bound", "get_state over %s's worker stream: %v", token, err)
	}
	answered, by := r.answeredBy(token, shimwire.TypeGetState, asked)
	if !answered || by.bootToken != launch.spec.BootToken {
		r.fails("newer-stays-bound", "the get_state of %s was answered by %s, want generation %d", token, by.label(), launch.spec.Generation)
	}
	c := r.claim(token)
	r.note("%s stays bound to generation %d: %s, and a get_state at %s was answered by %s", token, launch.spec.Generation, c.State, clockAt(asked), by.label())
}

// answeredBy is the connection of token's launches that carried the daemon's typ request sent at
// or after from and the shim's response to it.
func (r *lockoutLive) answeredBy(token claim.Token, typ string, from time.Time) (bool, relayConn) {
	r.t.Helper()
	var found relayConn
	ok := false
	r.await("the answer to "+typ, time.Minute, func() bool {
		for _, launch := range r.runtime.launchesOf(token) {
			for _, c := range r.relay.connsOf(launch.spec.BootToken) {
				for _, sent := range c.toShim {
					if sent.typ != typ || sent.at.Before(from) {
						continue
					}
					for _, back := range c.toDaemon {
						if back.typ == shimwire.TypeResponse && back.id == sent.id {
							found, ok = c, true
							return true
						}
					}
				}
			}
		}
		return false
	})
	return ok, found
}

// logsFrom is every record the daemon wrote after its first from, matching want.
func (r *lockoutLive) logsFrom(from int, want func(loggedRecord) bool) []loggedRecord {
	var out []loggedRecord
	for i, rec := range r.daemon.matching(func(loggedRecord) bool { return true }) {
		if i >= from && want(rec) {
			out = append(out, rec)
		}
	}
	return out
}

// generationIs reads a logged generation, which the log keeps as a number.
func generationIs(logged any, generation uint64) bool {
	return fmt.Sprint(logged) == fmt.Sprint(generation) || logged == float64(generation)
}

// describe is one logged record as a note prints it.
func describe(rec loggedRecord) string {
	keys := make([]string, 0, len(rec.Attrs))
	for key := range rec.Attrs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "%s %q", clockAt(rec.at), rec.Message)
	for _, key := range keys {
		fmt.Fprintf(&b, " %s=%v", key, rec.Attrs[key])
	}
	return b.String()
}

// firstAtOf is when the first of records was written, as a note prints it.
func firstAtOf(records []loggedRecord) string {
	if len(records) == 0 {
		return "never"
	}
	return clockAt(records[0].at)
}

// sequenceOf is the order the daemon registered conn in, and 0 for none.
func sequenceOf(conn any) uint64 {
	if sequenced, ok := conn.(interface{ Sequence() uint64 }); ok {
		return sequenced.Sequence()
	}
	return 0
}
