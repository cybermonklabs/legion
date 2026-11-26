package legion

import (
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// TestTheWholeReadLineNamesTheSequenceUnderDispatchsOwnHandler: each whole read is logged with its
// reason and the daemon.stateSeq it read (wholeReadLine), and cmd/dispatch logs through a text
// handler (main.go's slog.NewTextHandler), not the JSON handler the other tests here install. The
// line a person reads in Dispatch's log names the sequence as a number.
func TestTheWholeReadLineNamesTheSequenceUnderDispatchsOwnHandler(t *testing.T) {
	logs := &logtest.Buffer{}
	logtest.SetDefault(t, slog.NewTextHandler(logs, nil))
	daemon := newFakeDaemon(t, daemonState("INFRA", 15, planning("INFRA")))
	database, setting := configured(t, daemon.URL)
	if err := ReadWhole(t.Context(), database.Pool, setting, ReadReasonStart); err != nil {
		t.Fatalf("read INFRA's daemon whole: %v", err)
	}
	line := logs.Lines(wholeReadLine)
	if !strings.Contains(line, "stateSeq=15 ") {
		t.Errorf("the whole read's line under Dispatch's text handler is %q, want it to name stateSeq=15", strings.TrimSpace(line))
	}
}

// TestAMessageOfADaemonTheSettingNoLongerNamesLeavesTheNamedDaemonsEventsApplied: R3 reads a
// project whole when its Legion setting names another daemon, so that the observations follow
// the daemon the setting names from that read on. A message the daemon named before published
// while the setting still named it reaches Dispatch after the move, as one does that was in
// flight, held back by a take-over, or redelivered. Both URLs serve the rig's store and run, the
// one named before at 2000000 and the named one at 5; the message, at 2000001, carries a run
// neither serves, so it is of another store (publishOf). It is no event of the daemon the setting
// names and writes nothing that daemon did, and the events the named daemon publishes after it are
// applied, each one in turn, with no event skipped.
func TestAMessageOfADaemonTheSettingNoLongerNamesLeavesTheNamedDaemonsEventsApplied(t *testing.T) {
	rig := newIntakeRig(t)
	previous := newFakeDaemon(t, daemonState("INFRA", 2000000, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", previous.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")
	requireLastSeq(t, rig.database, "INFRA", 2000000)

	// The setting moves to a daemon that serves the rig's store and run at 5.
	named := newFakeDaemon(t, daemonState("INFRA", 5, strings.Replace(planning("INFRA"), `"planning"`, `"implementing"`, 1)))
	if _, err := PutSettings(t.Context(), rig.database.Pool, "INFRA", named.URL, model.Actor{Kind: "user", ID: "alice@example.com"}); err != nil {
		t.Fatalf("point INFRA at the named daemon: %v", err)
	}
	rig.wake <- struct{}{}
	rig.awaitWholeReads(2)
	requireLastSeq(t, rig.database, "INFRA", 5)
	// The write's read starts a minute that the old daemon's message would otherwise wait out
	// (read-rule-cases v11, rule 1).
	rig.clock.advance(ReadRetryAfter + time.Second)

	// A message of the daemon the setting no longer names, under a run neither URL serves.
	rig.publishOf("INFRA", "0b6f2d1e-7c4a-4e93-a8d5-3f1b9e6c2a70", time.Now().UTC().Truncate(time.Microsecond),
		change(2000001, "INFRA-1", moved("phase", "merging")))
	rig.settled()
	// The named daemon's next events.
	rig.publish("INFRA", change(6, "INFRA-1", moved("phase", "testing")))
	rig.publish("INFRA", change(7, "INFRA-1", moved("phase", "reviewing")))
	rig.settled()

	if phase := currentValues(t, rig.database, "INFRA")["INFRA-1/phase"]; phase != `"reviewing"` {
		t.Errorf("INFRA-1's phase is %s, want the named daemon's newest event's reviewing: its events after the other daemon's message were skipped", phase)
	}
	for _, value := range history(t, rig.database, "INFRA", "INFRA-1", FieldPhase) {
		if value == `"merging"` {
			t.Errorf("INFRA-1's phase history %v holds merging, which only the daemon the setting no longer names wrote", history(t, rig.database, "INFRA", "INFRA-1", FieldPhase))
			break
		}
	}
	requireLastSeq(t, rig.database, "INFRA", 7)
}

// TestAWholeReadOfADaemonThatRunsNoWorkflowNamesThatReason: R2's last sentence. A daemon with no
// dispatch_url runs no outbox, so it commits no state row, and serves the daemon.stateSeq of the
// newest state row its store committed before: 0 for a store that never published, as here (the
// daemon's DaemonInfo.StateSeq). Dispatch records each whole read of such a daemon as a failed read
// that names that reason: its settings carry no dispatch_url, so it runs no workflow.
func TestAWholeReadOfADaemonThatRunsNoWorkflowNamesThatReason(t *testing.T) {
	rig := newIntakeRig(t)
	noWorkflow := strings.Replace(daemonState("INFRA", 0, planning("INFRA")), `"dispatch_url":"http://dispatch.internal.example",`, ``, 1)
	daemon := newFakeDaemon(t, noWorkflow)
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitWholeReads(1)

	reads := readRecords(t, rig.database, "INFRA")
	requireWholeRead(t, reads[0], ReadReasonStart, false)
	if reads[0].error == nil || !strings.Contains(*reads[0].error, ErrNoDispatchURL.Error()) {
		t.Errorf("the whole read of a daemon that runs no workflow recorded the error %v, want it to name that reason: %q", orNone(reads[0].error), ErrNoDispatchURL.Error())
	}
}

// TestAFailedWholeReadSetsNoRun: only a good whole read takes the run that served it as the
// cursor's run (recordWhole). A read that fails, here one answered with another project's state
// served by another run, keeps the run of the cursor's last good read: a cursor that took the
// failed read's run would take a later boot of that run, at or below the cursor, for one it already
// holds, and read nothing where a daemon whose counter went back needs a whole read.
func TestAFailedWholeReadSetsNoRun(t *testing.T) {
	const otherRun = "9e3c5a71-2f4d-4b86-a0c9-6d1e8b7f3a52"
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")

	// A gap whose read the daemon refuses: the read is owed under the rig's run, and the message
	// that called for it holds the read. The refused read lasts a minute on the intake's clock
	// (lagsNext), so the read is due again once it has failed, and the message goes back to the
	// stream for holdAtLeast alone. The read its redelivery makes waits for retry, so the cursor is
	// checked as the refused read left it.
	other := strings.Replace(daemonState("INFRA", 25, planning("INFRA")), rigRun, otherRun, 1)
	other = strings.Replace(other, `"daemon":{"project":"INFRA"`, `"daemon":{"project":"LINUX"`, 1)
	daemon.serves(other)
	daemon.refusesFor(1, http.StatusServiceUnavailable)
	retry := make(chan struct{})
	release := sync.OnceFunc(func() { close(retry) })
	t.Cleanup(release)
	daemon.lagsNext(func() {
		rig.clock.advance(ReadRetryAfter)
		daemon.lagsNext(func() { <-retry })
	})
	rig.publish("INFRA", change(20, "INFRA-1", moved("phase", "implementing")))
	rig.settledHolding(1)
	if c := cursorOf(t, rig.database, "INFRA"); !c.readOwed || c.run == nil || *c.run != rigRun {
		t.Fatalf("after the refused gap read the cursor is %+v under the run %s, want a read owed under %s", c, orNone(c.run), rigRun)
	}

	// The owed read, which the message's redelivery makes a minute after the refused read began,
	// is answered with another project's state, which another run serves: a failed read.
	release()
	rig.awaitWholeReads(3)

	reads := readRecords(t, rig.database, "INFRA")
	requireWholeRead(t, reads[len(reads)-1], ReadReasonRetry, false)
	if c := cursorOf(t, rig.database, "INFRA"); c.run == nil || *c.run != rigRun || !c.readOwed {
		t.Errorf("after a failed read served by %s the cursor follows the run %s with a read owed %v, want %s, its last good read's, and the read still owed",
			otherRun, orNone(c.run), c.readOwed, rigRun)
	}
}

// TestATakeOverReadsAProjectWhoseOwedReadNoMessageHolds: each take-over queues every configured
// project's starting point (takenOver, SettingsChanged, startingPoint). Where a message holds the
// project's owed read, that call joins the read and makes none of its own
// (TestATakeOverDuringAHeldReadThatTimesOutKeepsTheFloorAboutElevenMinutes); where no message holds
// it, the take-over reads. A project whose read is owed and due and that no message holds is read
// at the take-over for retry: after a failed starting read, which no message makes, and after the
// message that held the read went back stateMaxHolds times and was let go. A project with no
// cursor, here one whose setting was written while no settings wake reached the intake, is read
// whole at the take-over (ReadReasonStart). Each is read once at the take-over, and the good read
// leaves its cursor at the read's stateSeq with nothing owed and no holder. A project whose cursor
// owes nothing, after a good starting read, is not read at the take-over: R3 reads a project whole
// only for the reasons it names.
func TestATakeOverReadsAProjectWhoseOwedReadNoMessageHolds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		project string
		// reason is the reason of the take-over's read, "" for a project the take-over does not
		// read.
		reason string
		// before brings project to what the take-over finds, from the first intake's start on,
		// and answers project's daemon and how many whole reads of every project are recorded by
		// then.
		before func(t *testing.T, rig *intakeRig) (daemon *fakeDaemon, reads int)
	}{
		{
			name: "a read owed since a failed starting read", project: "INFRA", reason: ReadReasonRetry,
			before: func(t *testing.T, rig *intakeRig) (*fakeDaemon, int) {
				daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
				daemon.refuses(http.StatusServiceUnavailable)
				seedLegionProject(t, rig.database, "INFRA", daemon.URL)
				rig.start()
				rig.awaitWholeReads(1)
				requireWholeRead(t, wholeReadsOf(t, rig.database, "INFRA")[0], ReadReasonStart, false)
				requireOwedWithNoHolder(t, rig, "INFRA")
				return daemon, 1
			},
		},
		{
			name: "a read owed since its holder was let go", project: "INFRA", reason: ReadReasonRetry,
			before: func(t *testing.T, rig *intakeRig) (*fakeDaemon, int) {
				// As in TestAHeldReadThatKeepsFailingLetsItsMessageGoAfterTenHandBacks: a gap's
				// message holds the read, the daemon refuses each read the message's deliveries
				// make, and the message is acknowledged after stateMaxHolds hand-backs.
				daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
				seedLegionProject(t, rig.database, "INFRA", daemon.URL)
				rig.start()
				rig.awaitStartingRead("INFRA")
				daemon.refuses(http.StatusServiceUnavailable)
				rig.clock.jumps(ReadRetryAfter - holdAtLeast)
				rig.publish("INFRA", change(12, "INFRA-1", moved("phase", "merging")))
				holder := rig.lastStreamSeq()
				rig.waitFor(time.Minute, func() bool {
					info := rig.consumerInfo()
					return info.AckFloor.Stream >= holder && info.NumAckPending == 0 && info.NumPending == 0
				}, func() string {
					return fmt.Sprintf("the message that holds INFRA's read is not acknowledged after %d hand-backs", len(rig.logRecords(heldLine)))
				})
				rig.clock.jumps(0)
				if released := rig.awaitLogRecords(releasedLine, 1); len(released) != 1 || released[0]["streamSeq"] != float64(holder) {
					t.Fatalf("the release lines = %v, want one naming the message at %d", released, holder)
				}
				requireOwedWithNoHolder(t, rig, "INFRA")
				return daemon, stateMaxHolds + 2
			},
		},
		{
			name: "a project with no cursor", project: "LINUX", reason: ReadReasonStart,
			before: func(t *testing.T, rig *intakeRig) (*fakeDaemon, int) {
				infra := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
				seedLegionProject(t, rig.database, "INFRA", infra.URL)
				rig.start()
				rig.awaitStartingRead("INFRA")
				linux := newFakeDaemon(t, daemonState("LINUX", 20, planning("LINUX")))
				seedLegionProject(t, rig.database, "LINUX", linux.URL)
				return linux, 1
			},
		},
		{
			name: "a project that owes nothing", project: "INFRA", reason: "",
			before: func(t *testing.T, rig *intakeRig) (*fakeDaemon, int) {
				daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
				seedLegionProject(t, rig.database, "INFRA", daemon.URL)
				rig.start()
				rig.awaitStartingRead("INFRA")
				if c := cursorOf(t, rig.database, "INFRA"); c.readOwed || c.lastSeq == nil || *c.lastSeq != 10 {
					t.Fatalf("INFRA's cursor after its good starting read is %s, want it at 10 with nothing owed", c)
				}
				return daemon, 1
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newIntakeRig(t)
			daemon, reads := tc.before(t, rig)

			// A minute on, so that an owed read is due, a second intake takes the durable over.
			served := daemon.served()
			daemon.serves(daemonState(tc.project, 30, planning(tc.project)))
			rig.clock.advance(ReadRetryAfter + time.Second)
			rig.stop()
			rig.start()
			if tc.reason == "" {
				if testwait.Holds(3*time.Second, 25*time.Millisecond, func() bool { return daemon.served() > served }) {
					t.Errorf("%s's daemon served %d reads at the take-over, want none: its cursor owes no read", tc.project, daemon.served()-served)
				}
				if c := cursorOf(t, rig.database, tc.project); c.readOwed || c.lastSeq == nil || *c.lastSeq != 10 {
					t.Errorf("after the take-over %s's cursor is %s, want it at its starting read's 10 with nothing owed", tc.project, c)
				}
				return
			}
			rig.awaitWholeReads(reads + 1)

			got := wholeReadsOf(t, rig.database, tc.project)
			requireWholeRead(t, got[len(got)-1], tc.reason, true)
			if n := daemon.served(); n != served+1 {
				t.Errorf("%s's daemon served %d reads at the take-over, want 1", tc.project, n-served)
			}
			if c := cursorOf(t, rig.database, tc.project); c.readOwed || c.readHolder != nil || c.readHolds != 0 || c.lastSeq == nil || *c.lastSeq != 30 {
				t.Errorf("after the take-over's read %s's cursor is %s, want it at the read's 30 with nothing owed and no holder", tc.project, c)
			}
		})
	}
}

// requireOwedWithNoHolder fails unless project's cursor owes a read for retry alone and names no
// message as its holder, with no hand-back counted.
func requireOwedWithNoHolder(t *testing.T, rig *intakeRig, project string) {
	t.Helper()
	if c := cursorOf(t, rig.database, project); !slices.Equal(c.readReasons, []string{ReadReasonRetry}) || c.readHolder != nil || c.readHolds != 0 {
		t.Fatalf("%s's cursor is %s, want the read owed for retry, no holder and no hand-back counted", project, c)
	}
}

// lastReadStartedAt is when project's newest whole read began, as its cursor keeps it
// (last_read_started_at), on the intake's clock.
func lastReadStartedAt(t *testing.T, rig *intakeRig, project string) time.Time {
	t.Helper()
	var started *time.Time
	if err := rig.database.Pool.QueryRow(t.Context(), `select last_read_started_at from legion_event_cursors where project = $1`, project).Scan(&started); err != nil || started == nil {
		t.Fatalf("read the start of %s's newest whole read: %v, %v", project, orNone(started), err)
	}
	return *started
}

// requireBReadAMinuteAfterA moves the intake's clock on by advance, what is left of the minute
// that began at first, the start of INFRA's last read of daemon a. It then fails unless INFRA has
// wholeReads whole reads, a served aServed reads and b one, and b's is INFRA's last whole read: a
// good starting read that starts at least ReadRetryAfter after first, made by the one call the
// intake deferred (why), which deferred none. It fails too unless INFRA's cursor then follows b's
// URL in b's store at 20.
func requireBReadAMinuteAfterA(t *testing.T, rig *intakeRig, a, b *fakeDaemon, first time.Time, advance time.Duration, wholeReads, aServed int, why string) {
	t.Helper()
	rig.clock.advance(advance)
	rig.waitFor(10*time.Second, func() bool { return b.served() >= 1 && len(wholeReadsOf(t, rig.database, "INFRA")) >= wholeReads }, func() string {
		return fmt.Sprintf("B was not read once a minute had passed since the read of A began: it served %d reads, and INFRA's whole reads are %v",
			b.served(), wholeReadsOf(t, rig.database, "INFRA"))
	})
	reads := wholeReadsOf(t, rig.database, "INFRA")
	if len(reads) != wholeReads || a.served() != aServed || b.served() != 1 {
		t.Fatalf("INFRA's whole reads are %v, and A served %d and B %d, want %d whole reads, %d of A and the last of B by %s", reads, a.served(), b.served(), wholeReads, aServed, why)
	}
	requireWholeRead(t, reads[wholeReads-1], ReadReasonStart, true)
	if deferred, made := rig.clock.deferrals(); deferred != 1 || made != 1 {
		t.Errorf("the intake deferred %d calls and made %d of them, want %s, made: the call that read B deferred none", deferred, made, why)
	}
	if apart := lastReadStartedAt(t, rig, "INFRA").Sub(first); apart < ReadRetryAfter {
		t.Errorf("the read of B started %s after the read of A on the intake's clock, want at least %s", apart, ReadRetryAfter)
	}
	if c := cursorOf(t, rig.database, "INFRA"); c.stateURL != b.URL || c.stateID == nil || *c.stateID != bStore || c.lastSeq == nil || *c.lastSeq != 20 {
		t.Errorf("INFRA's cursor is %s, want it following B's URL %s in B's store %s at 20", c, b.URL, bStore)
	}
}

// TestTwoWritesOfTheSettingTenSecondsApartStartTheirReadsAMinuteApart: R3 starts the whole reads
// of one project at least 60 seconds apart, whatever their reasons (read-rule-cases v11, rules 1
// and 3). A minute after INFRA's take-over read, a write of
// INFRA's setting naming its daemon A again is read at once. Ten seconds later on the intake's
// clock, a write names a second daemon B. Its starting read is not due, so the write's wake queues
// the starting point once more for the end of the minute (startLater). B is not read before the
// intake's clock is a minute past the start of A's second read; it is read then, with no message,
// and its read starts at least a minute after A's. At d69f92b6, a starting read was always due, so
// B was read at its write's wake, ten seconds after A. No timer recurs: the intake defers the one
// call of the second write's wake (intakeClock.deferrals), and that call, which reads B, defers
// none.
func TestTwoWritesOfTheSettingTenSecondsApartStartTheirReadsAMinuteApart(t *testing.T) {
	rig := newIntakeRig(t)
	a := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", a.URL)
	rig.start()
	rig.awaitWholeReads(1)
	rig.clock.advance(ReadRetryAfter + time.Second)
	write := func(daemon *fakeDaemon) {
		t.Helper()
		if _, err := PutSettings(t.Context(), rig.database.Pool, "INFRA", daemon.URL, model.Actor{Kind: "user", ID: "alice@example.com"}); err != nil {
			t.Fatalf("write INFRA's setting: %v", err)
		}
		rig.wake <- struct{}{}
	}

	write(a)
	rig.awaitWholeReads(2)
	first := lastReadStartedAt(t, rig, "INFRA")
	b := newFakeDaemon(t, daemonStateIn("INFRA", 20, inPhase("implementing"), bStore, bRun))
	rig.clock.advance(10 * time.Second)
	write(b)
	if testwait.Holds(2*time.Second, 25*time.Millisecond, func() bool { return b.served() > 0 }) {
		t.Fatalf("the write naming B read B about 10 s after the read of A began, on the intake's clock: want no read before a minute has passed")
	}
	rig.awaitDeferredCalls(1, "the write naming B")
	if deferred := rig.clock.deferredCalls(); deferred != 1 {
		t.Fatalf("the intake deferred %d calls by the write naming B, want the one call of that write's wake: the take-over and the first write found the starting read due", deferred)
	}
	rig.clock.advance(45 * time.Second)
	if testwait.Holds(time.Second, 25*time.Millisecond, func() bool { return b.served() > 0 }) {
		t.Fatalf("B was read before a minute had passed since the read of A began, on the intake's clock")
	}
	requireBReadAMinuteAfterA(t, rig, a, b, first, 5*time.Second, 3, 2, "the second write's one call")
}

// TestADeleteAndAWriteOfTheSettingWithinAMinuteStartTheirReadsAMinuteApart: R3-g starts the
// whole reads of one project at least 60 seconds apart, with no exemption. A delete of the
// setting resets the cursor as a write of another URL does and keeps its row, with the start of
// the project's newest whole read (read-rule-cases row 6.5, the architect's ruling of
// 2026-11-25T06:25Z). INFRA's take-over reads its daemon A. Ten seconds later on the intake's
// clock, INFRA's setting is deleted, and a write then names a second daemon B. B's starting read
// is not due, so the write's wake queues the starting point once more for the end of the minute
// (startLater). B is read once, a starting read of B's URL, when the intake's clock is a minute
// past the start of A's read, and its read starts at least a minute after A's. At cdc5fd84 the
// delete deleted the cursor, and the start of A's read with it, so B was read at its write's
// wake: the second read began 10 s after the first. No timer recurs: the intake defers the one
// call of the write's wake (intakeClock.deferrals), and that call, which reads B, defers none.
func TestADeleteAndAWriteOfTheSettingWithinAMinuteStartTheirReadsAMinuteApart(t *testing.T) {
	rig := newIntakeRig(t)
	a := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", a.URL)
	rig.start()
	rig.awaitWholeReads(1)
	requireWholeRead(t, wholeReadsOf(t, rig.database, "INFRA")[0], ReadReasonStart, true)
	first := lastReadStartedAt(t, rig, "INFRA")
	b := newFakeDaemon(t, daemonStateIn("INFRA", 20, inPhase("implementing"), bStore, bRun))

	rig.clock.advance(10 * time.Second)
	if deleted, err := DeleteSettings(t.Context(), rig.database.Pool, "INFRA"); err != nil || !deleted {
		t.Fatalf("delete INFRA's setting: deleted %v, %v", deleted, err)
	}
	if _, err := PutSettings(t.Context(), rig.database.Pool, "INFRA", b.URL, model.Actor{Kind: "user", ID: "alice@example.com"}); err != nil {
		t.Fatalf("write INFRA's setting naming B: %v", err)
	}
	rig.wake <- struct{}{}
	if testwait.Holds(2*time.Second, 25*time.Millisecond, func() bool { return b.served() > 0 }) {
		t.Fatalf("the write naming B after the delete read B about 10 s after the read of A began, on the intake's clock: want no read before a minute has passed")
	}
	rig.awaitDeferredCalls(1, "the write naming B after the delete")
	if deferred := rig.clock.deferredCalls(); deferred != 1 {
		t.Fatalf("the intake deferred %d calls by the write naming B after the delete, want the one call of its wake: the take-over found the starting read due", deferred)
	}
	requireBReadAMinuteAfterA(t, rig, a, b, first, 50*time.Second, 2, 1, "the write's one call")
}

// TestATakeOverThatFindsTheStartingReadNotDueDefersOneCallThatMakesIt: R3 starts the whole reads
// of one project at least 60 seconds apart, and no timer recurs. INFRA's take-over read of its
// daemon A is good. Ten seconds later on the intake's clock, the intake stops, and a write names a
// daemon B while no intake holds the durable, so no wake reaches one. A second intake then takes
// the durable over. Its starting point finds the starting read not due and held by no message: it
// reads nothing and defers one call for the end of the minute (Intake.startLater,
// intakeClock.deferrals). That call makes the starting read of B a minute after A's read began, and
// defers none.
func TestATakeOverThatFindsTheStartingReadNotDueDefersOneCallThatMakesIt(t *testing.T) {
	rig := newIntakeRig(t)
	a := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", a.URL)
	rig.start()
	rig.awaitWholeReads(1)
	requireWholeRead(t, wholeReadsOf(t, rig.database, "INFRA")[0], ReadReasonStart, true)
	first := lastReadStartedAt(t, rig, "INFRA")
	if deferred := rig.clock.deferredCalls(); deferred != 0 {
		t.Fatalf("the intake deferred %d calls by the first take-over, want none: it found the starting read due", deferred)
	}

	b := newFakeDaemon(t, daemonStateIn("INFRA", 20, inPhase("implementing"), bStore, bRun))
	rig.clock.advance(10 * time.Second)
	rig.stop()
	if _, err := PutSettings(t.Context(), rig.database.Pool, "INFRA", b.URL, model.Actor{Kind: "user", ID: "alice@example.com"}); err != nil {
		t.Fatalf("write INFRA's setting naming B: %v", err)
	}
	rig.start()
	if testwait.Holds(2*time.Second, 25*time.Millisecond, func() bool { return b.served() > 0 }) {
		t.Fatalf("the second take-over read B about 10 s after the read of A began, on the intake's clock: want no read before a minute has passed")
	}
	rig.awaitDeferredCalls(1, "the second take-over")
	if deferred, made := rig.clock.deferrals(); deferred != 1 || made != 0 {
		t.Fatalf("the intake deferred %d calls and made %d of them by the second take-over, want the one call that take-over defers, not made yet", deferred, made)
	}
	requireBReadAMinuteAfterA(t, rig, a, b, first, 50*time.Second, 2, 1, "the second take-over's one call")
}

// TestADeferredCallThatFindsTheStartingReadNotDueDefersNoOtherCall: no timer recurs (R3). The
// call that a write's wake or a take-over defers to the end of the minute (Intake.startLater)
// defers no call of its own, even where it finds the starting read not due and held by no message.
// INFRA's take-over read of its daemon A is good. Ten seconds later on the intake's clock, a write
// names a daemon B: its wake finds the starting read not due and defers one call. At the end of
// the minute, a moment before that call runs, a second write of B's URL is read at its wake, and a
// third write of B's URL resets the cursor (read-rule-cases row 6.2), whose wake comes only after
// the deferred call has run. The deferred call finds the starting read not due, since the second
// write's read began a moment before, and held by no message: it reads nothing and defers none, so
// the intake has deferred one call. The third write's wake then finds the starting read so too,
// and defers its own call.
//
// The intake's clock runs ahead of the rig's clock, which runs the deferred call
// (intakeClock.after), while the second write's wake makes its read and the third write resets the
// cursor, so both come before the deferred call runs. The test catches a deferred call that runs
// the starting point as a wake does (startLater passing true to startingPoint): that call then
// defers a second call. A whole read, good or failed, leaves the cursor following the read's URL
// (cursor.afterRead), so without the third write the deferred call would find a read owed for
// another reason than ReadReasonStart, or none, and defer nothing either way (readAnswer.later).
func TestADeferredCallThatFindsTheStartingReadNotDueDefersNoOtherCall(t *testing.T) {
	rig := newIntakeRig(t)
	a := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", a.URL)
	// ahead is how far the intake's clock runs ahead of the rig's clock.
	var ahead atomic.Int64
	rig.startOn(func() time.Time { return rig.clock.now().Add(time.Duration(ahead.Load())) })
	rig.awaitWholeReads(1)
	requireWholeRead(t, wholeReadsOf(t, rig.database, "INFRA")[0], ReadReasonStart, true)
	due := lastReadStartedAt(t, rig, "INFRA").Add(ReadRetryAfter)
	b := newFakeDaemon(t, daemonStateIn("INFRA", 20, inPhase("implementing"), bStore, bRun))
	write := func() {
		t.Helper()
		if _, err := PutSettings(t.Context(), rig.database.Pool, "INFRA", b.URL, model.Actor{Kind: "user", ID: "alice@example.com"}); err != nil {
			t.Fatalf("write INFRA's setting naming B: %v", err)
		}
	}

	rig.clock.advance(10 * time.Second)
	write()
	rig.wake <- struct{}{}
	rig.awaitDeferredCalls(1, "the write naming B 10 s after the read of A began, on the intake's clock,")
	if served := b.served(); served != 0 {
		t.Fatalf("B served %d reads at the first write's wake, want none: the starting read was not due", served)
	}

	// The end of the minute on the intake's clock, and not yet on the rig's clock.
	ahead.Store(int64(due.Add(time.Second).Sub(rig.clock.peek())))
	write()
	rig.wake <- struct{}{}
	rig.waitFor(10*time.Second, func() bool { return len(wholeReadsOf(t, rig.database, "INFRA")) >= 2 }, func() string {
		return fmt.Sprintf("the second write's wake did not read B once the minute had ended: B served %d reads, and INFRA's cursor is %s",
			b.served(), cursorOf(t, rig.database, "INFRA"))
	})
	requireWholeRead(t, wholeReadsOf(t, rig.database, "INFRA")[1], ReadReasonStart, true)
	write()
	if c := cursorOf(t, rig.database, "INFRA"); c.stateURL != "" || c.lastSeq != nil || c.readOwed || c.readHolder != nil {
		t.Fatalf("INFRA's cursor after the third write is %s, want it reset: following no daemon, nothing owed and no holder", c)
	}
	if deferred, made := rig.clock.deferrals(); deferred != 1 || made != 0 {
		t.Fatalf("the intake deferred %d calls and made %d of them by the third write, want the first write's one call, not made yet", deferred, made)
	}

	// The rig's clock catches up with the intake's, and the deferred call runs.
	rig.clock.advance(time.Duration(ahead.Swap(0)))
	rig.waitFor(5*time.Second, func() bool { _, made := rig.clock.deferrals(); return made >= 1 }, func() string {
		return "the first write's deferred call was not made once the minute had ended on the rig's clock"
	})
	if testwait.Holds(2*time.Second, 25*time.Millisecond, func() bool { return rig.clock.deferredCalls() > 1 }) {
		t.Errorf("the intake deferred %d calls, want the first write's 1: the deferred call found the starting read not due and deferred another", rig.clock.deferredCalls())
	}
	if served := b.served(); served != 1 {
		t.Errorf("B served %d reads, want the second write's alone: the deferred call read B", served)
	}

	// The third write's wake.
	rig.wake <- struct{}{}
	rig.awaitDeferredCalls(2, "the third write's wake")
	if testwait.Holds(time.Second, 25*time.Millisecond, func() bool { return b.served() > 1 || rig.clock.deferredCalls() > 2 }) {
		t.Errorf("after the third write's wake B served %d reads and the intake deferred %d calls, want 1 and 2: that wake read B or deferred more than its one call", b.served(), rig.clock.deferredCalls())
	}
}

// TestAGapFoundRightAfterAGoodStartingReadIsReadWhenItsMinuteEnds: R3 starts the whole reads of
// one project at least 60 seconds apart, the read right after a good starting read included
// (read-rule-cases v11, rule 2). INFRA's take-over read is good at 10. Ten seconds later on the
// intake's clock, its message at 12 shows a gap: it holds the gap read and goes back to the stream
// for the 50 s left of the starting read's minute, and the daemon is read no sooner. The message's
// redelivery makes the gap read, which starts a minute after the starting read began, within a few
// seconds, and INFRA-1's phase reaches 12's testing. At d69f92b6, a good starting read held back
// no read, so the message made the gap read at once.
func TestAGapFoundRightAfterAGoodStartingReadIsReadWhenItsMinuteEnds(t *testing.T) {
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitWholeReads(1)
	requireWholeRead(t, wholeReadsOf(t, rig.database, "INFRA")[0], ReadReasonStart, true)
	started := lastReadStartedAt(t, rig, "INFRA")

	daemon.serves(daemonState("INFRA", 12, inPhase("testing")))
	rig.clock.advance(10 * time.Second)
	rig.publish("INFRA", change(12, "INFRA-1", moved("phase", "testing")))
	holder := rig.lastStreamSeq()
	held := rig.awaitLogRecords(heldLine, 1)
	if delay, _ := held[0]["delay"].(float64); held[0]["streamSeq"] != float64(holder) || held[0]["reason"] != ReadReasonGap ||
		time.Duration(delay) < 45*time.Second || time.Duration(delay) > 50*time.Second {
		t.Fatalf("the message sent back = %v, want the message at 12 (stream sequence %d) holding the gap read for what is left of the starting read's minute, about 50 s", held[0], holder)
	}
	if testwait.Holds(2*time.Second, 25*time.Millisecond, func() bool { return daemon.served() > 1 }) {
		t.Fatalf("INFRA's daemon served %d reads within the starting read's minute, want the starting read alone", daemon.served())
	}
	rig.waitFor(70*time.Second, func() bool { return len(wholeReadsOf(t, rig.database, "INFRA")) >= 2 }, func() string {
		return fmt.Sprintf("the gap read was not made once the starting read's minute ended: INFRA's cursor is %s", cursorOf(t, rig.database, "INFRA"))
	})
	reads := wholeReadsOf(t, rig.database, "INFRA")
	requireWholeRead(t, reads[1], ReadReasonGap, true)
	if apart := lastReadStartedAt(t, rig, "INFRA").Sub(started); apart < ReadRetryAfter || apart > ReadRetryAfter+10*time.Second {
		t.Errorf("the gap read started %s after the starting read on the intake's clock, want a minute, within 10 s", apart)
	}
	rig.waitFor(5*time.Second, func() bool { return currentValues(t, rig.database, "INFRA")["INFRA-1/phase"] == `"testing"` }, func() string {
		return fmt.Sprintf("INFRA-1's phase is %s after the gap read, want 12's testing", currentValues(t, rig.database, "INFRA")["INFRA-1/phase"])
	})
}
