package legion

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testwait"
)

// lastSeqOf is project's cursor's last_seq, 0 for none.
func lastSeqOf(t *testing.T, rig *intakeRig, project string) int64 {
	t.Helper()
	var last int64
	if err := rig.database.Pool.QueryRow(context.Background(),
		`select coalesce(max(last_seq), 0) from legion_event_cursors where project = $1`, project).Scan(&last); err != nil {
		t.Fatalf("read the cursor of %s: %v", project, err)
	}
	return last
}

// TestFifteenProjectsInSlowReadsLeaveTheWindowAMessageOfAnotherProject: R3 holds back no project
// behind another project's read, and each project that owes a read holds at most one message of
// the consumer's window. A project keeps at most stateMaxWaiting messages waiting on its runner,
// the one in a read included; past that, one message holds the read it owes and goes back to the
// stream, and the others are acknowledged (judgeCrowded). So a project in a slow read holds
// stateMaxWaiting+1 messages of the durable's window of stateMaxAckPending, 17 of 256, and fifteen
// such projects hold 255. Fifteen projects each make a gap read that their daemon holds, and each
// publishes stateMaxWaiting+2 more messages while that read is held. The test waits until every
// one of them has been delivered and judged: each slow project's first message past the cap has
// gone back to the stream holding the read after the one in flight, and the slow projects hold
// exactly that share of the window. A message of a sixteenth project is then applied in its usual
// time, and no slow project's daemon is asked for another read.
func TestFifteenProjectsInSlowReadsLeaveTheWindowAMessageOfAnotherProject(t *testing.T) {
	t.Parallel()
	const slow = 15
	const usual = 3 * time.Second
	window := slow * (stateMaxWaiting + 1)
	rig := newIntakeRig(t)
	projects := make([]string, slow)
	daemons := make([]*fakeDaemon, slow)
	for i := range slow {
		projects[i] = fmt.Sprintf("SLOW%02d", i+1)
		daemons[i] = newFakeDaemon(t, daemonState(projects[i], 10, planning(projects[i])))
		seedLegionProject(t, rig.database, projects[i], daemons[i].URL)
	}
	quick := newFakeDaemon(t, daemonState("QUICK", 10, planning("QUICK")))
	seedLegionProject(t, rig.database, "QUICK", quick.URL)
	rig.start()
	rig.awaitWholeReads(slow + 1)
	// Each starting read's minute passes, so each project's gap read is due at once.
	rig.clock.advance(ReadRetryAfter + time.Second)

	releases := make([]func(), slow)
	for i, daemon := range daemons {
		releases[i] = daemon.holds(t)
	}
	for _, project := range projects {
		rig.publish(project, change(12, project+"-1", moved("phase", "testing")))
	}
	rig.waitFor(30*time.Second, func() bool {
		return !slices.ContainsFunc(daemons, func(daemon *fakeDaemon) bool { return daemon.served() < 2 })
	}, func() string { return "a slow project's gap read never began" })
	for _, project := range projects {
		for seq := 13; seq < 13+stateMaxWaiting+2; seq++ {
			rig.publish(project, change(seq, project+"-1", moved("phase", "testing")))
		}
	}
	last := rig.lastStreamSeq()
	rig.waitFor(30*time.Second, func() bool {
		info := rig.consumerInfo()
		return info.Delivered.Stream >= last && len(rig.logRecords(heldLine)) == slow && info.NumAckPending == window
	}, func() string {
		return fmt.Sprintf("the slow projects' messages did not reach the intake: want every message through stream sequence %d delivered, one of each slow project's gone back to the stream (%d in all, %d logged) and %d held of the window: %+v",
			last, slow, len(rig.logRecords(heldLine)), window, rig.consumerInfo())
	})

	published := time.Now()
	rig.publish("QUICK", change(11, "QUICK-1", moved("phase", "implementing")))
	if !testwait.Holds(usual, 25*time.Millisecond, func() bool { return lastSeqOf(t, rig, "QUICK") >= 11 }) {
		t.Errorf("QUICK's message was not applied within %s of its publish while fifteen projects were in slow reads: the window is full: %+v",
			usual, rig.consumerInfo())
	} else {
		t.Logf("QUICK's message was applied %s after its publish", time.Since(published).Round(time.Millisecond))
	}
	if held := rig.consumerInfo().NumAckPending; held != window {
		t.Errorf("the slow projects hold %d messages of the window, want %d: %d waiting on each runner and one held each", held, window, stateMaxWaiting)
	}
	if holds := rig.logRecords(heldLine); len(holds) != slow {
		t.Errorf("%d messages went back to the stream, want one for each slow project: %v", len(holds), holds)
	}
	for i, daemon := range daemons {
		if served := daemon.served(); served != 2 {
			t.Errorf("%s's daemon served %d reads, want its starting read and its gap read alone", projects[i], served)
		}
	}
	for _, release := range releases {
		release()
	}
}

// TestAProjectPastTheCapDuringAGoodReadThatGoesQuietIsReadWithinTheMinute: R3 keeps a project that
// goes quiet at most one minute behind while its reads succeed. INFRA's gap read at 12 is in flight,
// held by its daemon, which serves 30. Near the end of that read's minute on the intake's clock,
// INFRA publishes 15 messages at or below 30, 13 to 27, which wait on INFRA's runner behind the
// read, and then 31 to 33, past the read's snapshot and past the cap of 16 messages waiting
// (stateMaxWaiting, read-rule-cases row 8.1): each owes a gap read, which the read in flight does
// not clear (recordWhole), the first of them holds it and goes back to the stream until the minute
// ends, and the other two join it and are acknowledged. So while 31 holds the read, gone back
// once, INFRA holds 17 messages of the window: 16 waiting on its runner, 12 to 27, and 31. The
// read succeeds at 30, and INFRA goes quiet. Within the time left of the minute and a few seconds,
// a gap read is made, and INFRA-1's phase reaches 33's reviewing.
//
// 31 goes back for no more than the time left of the minute, on the intake's clock. Its hand-back
// is armed on the rig's clock, which moves to its end once INFRA has gone quiet (runUntil), by no
// more than that time left: the read 31's redelivery makes begins no sooner than a minute after
// the gap read at 30 began. Once every message is acknowledged and INFRA's runner has run what was
// queued on it, INFRA's daemon has served three reads.
//
// The test catches a cap other than 16. It reads the window while the first message past the cap
// has gone back once: that message goes back again each holdAtLeast once the minute is over, and
// is let go after stateMaxHolds hand-backs, after which INFRA holds one message fewer. With a cap
// of 15 INFRA holds 16 messages while that message has gone back once, and with a cap of 17, 18.
func TestAProjectPastTheCapDuringAGoodReadThatGoesQuietIsReadWithinTheMinute(t *testing.T) {
	t.Parallel()
	// left is what is left of the read's minute when the messages past the cap arrive.
	const left = 5 * time.Second
	phase := func(phase string) string {
		return strings.Replace(planning("INFRA"), `"planning"`, fmt.Sprintf("%q", phase), 1)
	}
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")

	release := daemon.holds(t)
	daemon.serves(daemonState("INFRA", 30, phase("implementing")))
	rig.publish("INFRA", change(12, "INFRA-1", moved("phase", "implementing")))
	rig.waitFor(20*time.Second, func() bool { return daemon.served() >= 2 }, func() string {
		return "INFRA's gap read never began"
	})
	// The gap read at 30 began as the daemon was called: the read after it is due a minute on, on
	// the intake's clock.
	minuteEnds := lastReadStartedAt(t, rig, "INFRA").Add(ReadRetryAfter)
	// The read in flight serves the state at 30, which it took as it began; any later read serves 33.
	daemon.serves(daemonState("INFRA", 33, phase("reviewing")))
	rig.clock.advance(ReadRetryAfter - left)
	for seq := 13; seq <= 27; seq++ {
		rig.publish("INFRA", change(seq, "INFRA-1", moved("phase", "implementing")))
	}
	for seq, past := range []string{"testing", "merging", "reviewing"} {
		rig.publish("INFRA", change(31+seq, "INFRA-1", moved("phase", past)))
	}
	// held is INFRA's share of the window while the first message past the cap has gone back once.
	var held int
	rig.waitFor(20*time.Second, func() bool {
		info := rig.consumerInfo()
		if info.NumPending != 0 || len(rig.logRecords(heldLine)) != 1 {
			return false
		}
		held = info.NumAckPending
		return held == 17
	}, func() string {
		return fmt.Sprintf("INFRA holds %d messages of the window while the first message past the cap has gone back once, want 17: 16 waiting on its runner and that message (%+v)",
			held, rig.consumerInfo())
	})
	if holds := rig.logRecords(heldLine); holds[0]["seq"] != float64(31) || holds[0]["reason"] != ReadReasonGap {
		t.Errorf("the message sent back = %v, want 31, the first message past the cap, holding the gap read", holds)
	} else if delay, _ := holds[0]["delay"].(float64); time.Duration(delay) < holdAtLeast || time.Duration(delay) > left {
		t.Errorf("31 went back for %s, want the %s or less left of the minute of the gap read at 30 on the intake's clock", time.Duration(delay), left)
	}

	release()
	read := time.Now()
	// 31's hand-back is armed on the rig's clock for what is left of the minute: runUntil moves the
	// clock to it once INFRA has gone quiet, by no more than left.
	if !rig.runUntil(func() bool {
		return currentValues(t, rig.database, "INFRA")["INFRA-1/phase"] == `"reviewing"`
	}, left, left+15*time.Second) {
		t.Fatalf("%s after the read at 30, with INFRA quiet, INFRA-1's phase is %s and INFRA's cursor is %s: the events past the cap were never read",
			time.Since(read).Round(time.Second), currentValues(t, rig.database, "INFRA")["INFRA-1/phase"], cursorOf(t, rig.database, "INFRA"))
	}
	rig.awaitWholeReads(3)
	reads := wholeReadsOf(t, rig.database, "INFRA")
	if len(reads) != 3 {
		t.Fatalf("INFRA's whole reads are %v, want the starting read, the gap read at 30 and one more", reads)
	}
	requireWholeRead(t, reads[1], ReadReasonGap, true)
	requireWholeRead(t, reads[2], ReadReasonGap, true)
	if started := lastReadStartedAt(t, rig, "INFRA"); started.Before(minuteEnds) {
		t.Errorf("the read after the gap read at 30 began at %s, want it once that read's minute ended, at %s",
			started.Format(time.RFC3339Nano), minuteEnds.Format(time.RFC3339Nano))
	}
	// Each message is acknowledged once its read has landed (settled), and a barrier on INFRA's
	// runner runs after whatever was queued there: the count of reads is final.
	rig.settled()
	rig.drained("INFRA")
	if served := daemon.served(); served != 3 {
		t.Errorf("INFRA's daemon served %d reads, want the starting read and two gap reads", served)
	}
}

// TestMessagesPastTheCapOfAProjectWithNoCursorHoldOneMessageAndAreReadOnTheRunner: row 8.1 for a
// project whose setting was written while no settings wake reached the intake, so it has no cursor
// yet. Its first message makes the starting read on the project's runner, and INFRA's daemon holds
// the answer, a state at 30. While that read is held, near the end of its minute on the intake's
// clock, INFRA publishes 15 more messages at or below 30, which wait on the runner behind it, and
// then 31 to 33, past the read's snapshot and past the cap of stateMaxWaiting. Those make no read
// beside the runner: the project is given a cursor that follows no daemon, which records the gap
// read they owe, the first of them holds that read and the other two are acknowledged, so INFRA
// holds stateMaxWaiting+1 messages of the window. Once the starting read lands, the gap read is
// made on the runner too, and INFRA-1's phase reaches 33's reviewing; once every message is
// acknowledged and INFRA's runner has run what was queued on it, INFRA's daemon has served those
// two reads alone.
func TestMessagesPastTheCapOfAProjectWithNoCursorHoldOneMessageAndAreReadOnTheRunner(t *testing.T) {
	t.Parallel()
	// left is what is left of the starting read's minute when the messages past the cap arrive.
	const left = 5 * time.Second
	phase := func(phase string) string {
		return strings.Replace(planning("INFRA"), `"planning"`, fmt.Sprintf("%q", phase), 1)
	}
	rig := newIntakeRig(t)
	rig.start()
	daemon := newFakeDaemon(t, daemonState("INFRA", 30, phase("implementing")))
	release := daemon.holds(t)
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	var cursors int
	if err := rig.database.Pool.QueryRow(t.Context(), `select count(*) from legion_event_cursors where project = 'INFRA'`).Scan(&cursors); err != nil || cursors != 0 {
		t.Fatalf("INFRA has %d cursors before its first message (%v), want none", cursors, err)
	}

	rig.publish("INFRA", change(11, "INFRA-1", moved("phase", "implementing")))
	rig.waitFor(20*time.Second, func() bool { return daemon.served() >= 1 }, func() string {
		return "INFRA's first message never made the starting read"
	})
	// The read in flight serves the state at 30, which it took as it began; any later read serves 33.
	daemon.serves(daemonState("INFRA", 33, phase("reviewing")))
	rig.clock.advance(ReadRetryAfter - left)
	for seq := 12; seq < 12+stateMaxWaiting-1; seq++ {
		rig.publish("INFRA", change(seq, "INFRA-1", moved("phase", "implementing")))
	}
	for seq, past := range []string{"testing", "merging", "reviewing"} {
		rig.publish("INFRA", change(31+seq, "INFRA-1", moved("phase", past)))
	}
	last := rig.lastStreamSeq()
	rig.waitFor(20*time.Second, func() bool {
		info := rig.consumerInfo()
		return info.Delivered.Stream >= last && info.NumAckPending == stateMaxWaiting+1 &&
			len(rig.logRecords(heldLine)) >= 1 && len(rig.logRecords(crowdedLine)) >= 1
	}, func() string {
		return fmt.Sprintf("INFRA holds %+v, %d messages went back and %d drops past the cap were logged, want every message through stream sequence %d delivered, %d messages waiting on its runner and one held",
			rig.consumerInfo(), len(rig.logRecords(heldLine)), len(rig.logRecords(crowdedLine)), last, stateMaxWaiting)
	})
	if served := daemon.served(); served != 1 {
		t.Errorf("INFRA's daemon served %d reads while its starting read was held, want that read alone: a message past the cap made a read beside the runner", served)
	}
	if holds := rig.logRecords(heldLine); len(holds) != 1 || holds[0]["seq"] != float64(31) || holds[0]["reason"] != ReadReasonGap {
		t.Errorf("the messages sent back = %v, want the first message past the cap, at 31, once, holding the gap read", holds)
	}
	if c := cursorOf(t, rig.database, "INFRA"); !slices.Equal(c.readReasons, []string{ReadReasonGap}) || c.readHolder == nil || c.readHolds != 1 ||
		c.lastSeq != nil || c.stateID != nil {
		t.Errorf("INFRA's cursor while its starting read is held is %s, want one that follows no daemon, owes the gap read and names its holder once", c)
	}
	if drops := rig.awaitLogRecords(crowdedLine, 1); drops[0]["project"] != "INFRA" || drops[0]["reason"] != ReadReasonGap {
		t.Errorf("the drops logged = %v, want one naming INFRA and the gap", drops)
	}

	release()
	// 31's hand-back is armed on the rig's clock for left: runUntil moves the clock to it once the
	// rig is quiet.
	if !rig.runUntil(func() bool {
		return currentValues(t, rig.database, "INFRA")["INFRA-1/phase"] == `"reviewing"`
	}, left, left+15*time.Second) {
		t.Fatalf("after the starting read at 30 INFRA-1's phase is %s and INFRA's cursor is %s: the events past the cap were never read",
			currentValues(t, rig.database, "INFRA")["INFRA-1/phase"], cursorOf(t, rig.database, "INFRA"))
	}
	rig.settled()
	reads := wholeReadsOf(t, rig.database, "INFRA")
	if len(reads) != 2 {
		t.Fatalf("INFRA's whole reads are %v, want the starting read and the gap read", reads)
	}
	requireWholeRead(t, reads[0], ReadReasonStart, true)
	requireWholeRead(t, reads[1], ReadReasonGap, true)
	// Every message is acknowledged once its read has landed (settled), and a barrier on INFRA's
	// runner runs after whatever was queued there: the count of reads is final.
	rig.drained("INFRA")
	if served := daemon.served(); served != 2 {
		t.Errorf("INFRA's daemon served %d reads, want the starting read and the gap read", served)
	}
	if c := cursorOf(t, rig.database, "INFRA"); c.readOwed || c.readHolder != nil || c.lastSeq == nil || *c.lastSeq != 33 {
		t.Errorf("INFRA's cursor is %s, want it at the gap read's 33 with nothing owed and no holder", c)
	}
}

// TestABootOfADroppedStorePastTheCapFindsTheSettingsDaemonAgain: read-rule-cases rows 3.5 and
// 8.1. A boot of a store the cursor drops - a store
// marked as no daemon of the project, or the recorded old store - while the cursor follows the
// setting's URL in another store a read verified, is judged as a message of another store, on the
// project's runner and past the cap of stateMaxWaiting alike: it owes the store read and holds
// it, and its redelivery makes that read. So a daemon that a redeploy started behind the setting's
// URL in that store is found again within about a minute, even where its boot is the project's
// 17th waiting message and no read is in flight.
//
// Each case sets INFRA's setting as TestABootOfAMarkedOrOldStoreWhoseReadFindsTheCursorsStoreUnchangedAppliesNothing
// does: A read well in the rig's store, B whose starting read fails, then C read well in a store of
// its own, so A's store is the recorded old store; for the marked store, B's message marks B's
// store. C's URL comes to serve the dropped store at 41, with no write of the setting, and the
// intake's clock moves ReadRetryAfter and a second on. Work queued on INFRA's runner keeps it busy
// with no read in flight, 16 messages of C's store wait behind that work, and the dropped store's
// boot at 41 arrives past the cap. The test holds the boot to owing the store read and holding it:
// the boot goes back to the stream holding that read, at its hold's first hand-back (heldLine), and
// INFRA's cursor owes the store read alone and names the boot as its holder, while no read is in
// flight. The runner is then let go, and within 75 s of wall-clock time INFRA's cursor follows the
// dropped store at 41.
//
// C serves the store at the boot's sequence, as a daemon that published the boot does: a daemon
// commits each event before it publishes it. So the cursor reaches 41 whichever delivery makes
// the store read the boot owes: the boot's redelivery, or the first message that waited on the
// runner, which makes a read the cursor owes once it is due, whatever message holds it, none
// included (cursor.readFor). The cursor at 41 so does not show the hold, and the test checks the
// hold before it lets the runner go.
//
// At d69f92b6, judgeCrowded finished the boot as a message of a dropped store (cursor.forgets)
// while no read was in flight: it owed nothing, and INFRA stayed on C's store.
func TestABootOfADroppedStorePastTheCapFindsTheSettingsDaemonAgain(t *testing.T) {
	t.Parallel()
	const bootRun = "4b8e1d6a-2c7f-4a93-b5e0-9d3c6f1a8e24"
	for _, tc := range []struct {
		name  string
		store string // the store that boots
		marks bool
	}{
		{name: "a store marked as no daemon of the project", store: bStore, marks: true},
		{name: "the recorded old store", store: rigStateID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newIntakeRig(t)
			a := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
			seedLegionProject(t, rig.database, "INFRA", a.URL)
			rig.start()
			rig.awaitStartingRead("INFRA")
			b := newFakeDaemon(t, "")
			b.refuses(http.StatusServiceUnavailable)
			rig.pointAt(b, 2)
			c := newFakeDaemon(t, daemonStateIn("INFRA", 5, inPhase("implementing"), cStore, cRun))
			rig.pointAt(c, 3)
			if tc.marks {
				rig.publishIn("INFRA", bRun, bStore, time.Now().UTC().Truncate(time.Microsecond), false, change(20, "INFRA-1", moved("phase", "merging")))
				rig.settled()
			}
			if cur := cursorOf(t, rig.database, "INFRA"); cur.stateID == nil || *cur.stateID != cStore || cur.previousStateID == nil ||
				*cur.previousStateID != rigStateID || slices.Contains(cur.foreignStateIDs, bStore) != tc.marks || cur.readOwed {
				t.Fatalf("control: INFRA's cursor before the redeploy is %s, want C's store %s, A's store %s recorded, B's store marked %v and nothing owed",
					cur, cStore, rigStateID, tc.marks)
			}

			// The redeploy: C's URL serves the dropped store, with no write of the setting.
			c.serves(daemonStateIn("INFRA", 41, inPhase("reviewing"), tc.store, bootRun))
			rig.clock.advance(ReadRetryAfter + time.Second)
			release := make(chan struct{})
			let := sync.OnceFunc(func() { close(release) })
			t.Cleanup(let)
			if !rig.intake.durable.Queue("INFRA", func(ctx context.Context) {
				select {
				case <-release:
				case <-ctx.Done():
				}
			}) {
				t.Fatal("control: the intake took no work on INFRA's runner")
			}
			at := time.Now().UTC().Truncate(time.Microsecond)
			for seq := 6; seq < 6+stateMaxWaiting; seq++ {
				rig.publishIn("INFRA", cRun, cStore, at, false, change(seq, "INFRA-1", moved("phase", "implementing")))
			}
			rig.waitFor(20*time.Second, func() bool {
				info := rig.consumerInfo()
				return info.NumPending == 0 && info.NumAckPending >= stateMaxWaiting
			}, func() string {
				return fmt.Sprintf("control: INFRA holds %+v, want %d messages waiting on its runner", rig.consumerInfo(), stateMaxWaiting)
			})
			rig.publishIn("INFRA", bootRun, tc.store, at, false, booted(41, stateSchema, 2))
			boot := rig.lastStreamSeq()
			rig.waitFor(20*time.Second, func() bool { return rig.consumerInfo().NumPending == 0 }, func() string {
				return fmt.Sprintf("control: the boot past the cap was not delivered: %+v", rig.consumerInfo())
			})
			var held map[string]any
			rig.waitFor(20*time.Second, func() bool {
				for _, record := range rig.logRecords(heldLine) {
					if record["streamSeq"] == float64(boot) && record["reason"] == ReadReasonStore {
						held = record
						return true
					}
				}
				return false
			}, func() string {
				return fmt.Sprintf("the boot of %s past the cap (stream sequence %d) never went back to the stream holding the store read: the messages sent back = %v, and INFRA's cursor is %s",
					tc.store, boot, rig.logRecords(heldLine), cursorOf(t, rig.database, "INFRA"))
			})
			if cur := cursorOf(t, rig.database, "INFRA"); held["handBack"] != float64(1) || cur.readHolder == nil || *cur.readHolder != int64(boot) ||
				!slices.Equal(cur.readReasons, []string{ReadReasonStore}) {
				t.Fatalf("the boot went back as %v with INFRA's cursor %s, want its hold's first hand-back, and the cursor owing the store read alone with the boot (stream sequence %d) as its holder",
					held, cur, boot)
			}
			if rig.intake.readsNow("INFRA") {
				t.Fatal("control: a whole read of INFRA is in flight while the boot past the cap is judged")
			}
			let()
			if !testwait.Holds(75*time.Second, 200*time.Millisecond, func() bool {
				cur := cursorOf(t, rig.database, "INFRA")
				return cur.stateID != nil && *cur.stateID == tc.store && cur.lastSeq != nil && *cur.lastSeq == 41
			}) {
				t.Errorf("75 s after the boot of %s past the cap, with no read in flight, INFRA's cursor is %s and C's URL served %d reads: the store read the boot held never found the daemon behind the setting's URL in that store",
					tc.store, cursorOf(t, rig.database, "INFRA"), c.served())
			}
		})
	}
}

// readsLockWaiters counts the statements of the rig's database that wait for a lock on
// legion_reads.
func readsLockWaiters(t *testing.T, rig *intakeRig) int {
	t.Helper()
	var waiters int
	if err := rig.database.Pool.QueryRow(t.Context(), `
		select count(*) from pg_locks
		where not granted and relation = 'legion_reads'::regclass
			and database = (select oid from pg_database where datname = current_database())
	`).Scan(&waiters); err != nil {
		t.Fatalf("count the waits for a lock on legion_reads: %v", err)
	}
	return waiters
}

// cursorWaitersBehindARecord counts the statements of the rig's database that wait for a lock
// (wait_event_type 'Lock') as lockCursor's select of a project's cursor row for update, each
// blocked (pg_blocking_pids) by a statement that waits for a lock on legion_reads: a message past
// the cap whose judgeCrowded waits on its project's cursor row behind a whole read's record, which
// holds that row (recordWhole, lockCursor) while its insert of the read's row waits
// (readsLockWaiters).
func cursorWaitersBehindARecord(t *testing.T, rig *intakeRig) int {
	t.Helper()
	var waiters int
	if err := rig.database.Pool.QueryRow(t.Context(), `
		select count(*) from pg_stat_activity waiter
		where waiter.datname = current_database() and waiter.wait_event_type = 'Lock' and waiter.query = $1
			and exists (
				select from pg_locks record
				where not record.granted and record.relation = 'legion_reads'::regclass
					and record.database = (select oid from pg_database where datname = current_database())
					and record.pid = any(pg_blocking_pids(waiter.pid))
			)
	`, `select `+cursorColumns+` from legion_event_cursors where project = $1 for update`).Scan(&waiters); err != nil {
		t.Fatalf("count the waits for a cursor row behind a whole read's record: %v", err)
	}
	return waiters
}

// TestEventsPastTheCapWhileTheFirstReadOfAProjectWithNoCursorIsRecordedAreApplied: R3-h and the
// second invariant of read-rule-cases. A project with no cursor row is given one that follows no
// daemon by its starting read, before the read calls the daemon (Intake.readWhole, read-rule-cases
// row 7.6). Messages past the cap of stateMaxWaiting that judgeCrowded judges while that read's
// record is written record on that row the gap read they owe, its holder and the sequences they
// skipped. The record, which locks the row (lockCursor), must neither write over them nor miss
// them.
//
// INFRA has a setting and no cursor. Its first message makes the starting read, which serves 30,
// and the test holds the read's record in the database: a lock on legion_reads keeps the insert of
// the read's row waiting, after the record has read INFRA's position. Meanwhile 15 more messages
// at or below 30 wait on INFRA's runner, and 31 to 33 arrive past the cap, past the read's
// snapshot, near the end of the read's minute. The first of them is judged while the record is
// held: its lockCursor waits on INFRA's cursor row, blocked by the record, which holds the row. The
// lock is let go. INFRA then goes quiet: within the time left of the minute and 15 s, INFRA-1's
// phase reaches 33's reviewing.
//
// 31 goes back for no more than the time left of the minute, on the intake's clock. Its hand-back
// is armed on the rig's clock, which moves to its end once INFRA has gone quiet (runUntil), by no
// more than that time left: the gap read 31's redelivery makes begins no sooner than a minute
// after the starting read began.
//
// At d69f92b6, the record read no cursor and upserted one computed from none: it cleared the gap
// read, its holder and the skipped sequences that judgeCrowded recorded meanwhile. The holder at 31
// was then applied at its redelivery, and 32 and 33 were never read.
func TestEventsPastTheCapWhileTheFirstReadOfAProjectWithNoCursorIsRecordedAreApplied(t *testing.T) {
	t.Parallel()
	// left is what is left of the starting read's minute when the messages past the cap arrive.
	const left = 5 * time.Second
	rig := newIntakeRig(t)
	rig.start()
	daemon := newFakeDaemon(t, daemonState("INFRA", 30, inPhase("implementing")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	var cursors int
	if err := rig.database.Pool.QueryRow(t.Context(), `select count(*) from legion_event_cursors where project = 'INFRA'`).Scan(&cursors); err != nil || cursors != 0 {
		t.Fatalf("control: INFRA has %d cursors before its first message (%v), want none", cursors, err)
	}
	lock, err := rig.database.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("open the transaction that holds the read's record: %v", err)
	}
	t.Cleanup(func() { _ = lock.Rollback(context.Background()) })
	if _, err := lock.Exec(t.Context(), `lock table legion_reads in share mode`); err != nil {
		t.Fatalf("lock legion_reads: %v", err)
	}

	rig.publish("INFRA", change(11, "INFRA-1", moved("phase", "implementing")))
	rig.waitFor(20*time.Second, func() bool { return daemon.served() >= 1 && readsLockWaiters(t, rig) > 0 }, func() string {
		return fmt.Sprintf("control: the starting read's record does not wait on legion_reads: INFRA's daemon served %d reads", daemon.served())
	})
	// The starting read began as the daemon was called: the read after it is due a minute on, on the
	// intake's clock.
	minuteEnds := lastReadStartedAt(t, rig, "INFRA").Add(ReadRetryAfter)
	// Any later read serves 33.
	daemon.serves(daemonState("INFRA", 33, inPhase("reviewing")))
	rig.clock.advance(ReadRetryAfter - left)
	for seq := 12; seq < 12+stateMaxWaiting-1; seq++ {
		rig.publish("INFRA", change(seq, "INFRA-1", moved("phase", "implementing")))
	}
	for seq, past := range []string{"testing", "merging", "reviewing"} {
		rig.publish("INFRA", change(31+seq, "INFRA-1", moved("phase", past)))
	}
	rig.waitFor(20*time.Second, func() bool {
		info := rig.consumerInfo()
		return info.NumPending == 0 && info.NumAckPending > stateMaxWaiting
	}, func() string {
		return fmt.Sprintf("control: INFRA holds %+v, want %d messages waiting on its runner and the messages past the cap", rig.consumerInfo(), stateMaxWaiting)
	})
	// The first message past the cap is judged beside the runner while the record is held: it waits
	// on INFRA's cursor row, which the record holds while its insert waits on legion_reads.
	testwait.Until(t, "the first message past the cap to wait on INFRA's cursor row behind the starting read's record", func() (int, bool) {
		waiters := cursorWaitersBehindARecord(t, rig)
		return waiters, waiters >= 1
	})
	if err := lock.Rollback(t.Context()); err != nil {
		t.Fatalf("let the read's record go: %v", err)
	}
	// 31's hand-back is armed on the rig's clock for what is left of the minute: runUntil moves the
	// clock to it once INFRA has gone quiet, by no more than left.
	if !rig.runUntil(func() bool {
		return currentValues(t, rig.database, "INFRA")["INFRA-1/phase"] == `"reviewing"`
	}, left, left+15*time.Second) {
		t.Fatalf("after the starting read at 30, INFRA-1's phase is %s, INFRA's cursor is %s and its daemon served %d reads: the events past the cap were never read",
			currentValues(t, rig.database, "INFRA")["INFRA-1/phase"], cursorOf(t, rig.database, "INFRA"), daemon.served())
	}
	if holds := rig.logRecords(heldLine); len(holds) != 1 || holds[0]["seq"] != float64(31) || holds[0]["reason"] != ReadReasonGap {
		t.Errorf("the messages sent back = %v, want 31, the first message past the cap, once, holding the gap read", holds)
	} else if delay, _ := holds[0]["delay"].(float64); time.Duration(delay) < holdAtLeast || time.Duration(delay) > left {
		t.Errorf("31 went back for %s, want the %s or less left of the starting read's minute on the intake's clock", time.Duration(delay), left)
	}
	if started := lastReadStartedAt(t, rig, "INFRA"); started.Before(minuteEnds) {
		t.Errorf("the gap read 31 held began at %s, want it once the starting read's minute ended, at %s",
			started.Format(time.RFC3339Nano), minuteEnds.Format(time.RFC3339Nano))
	}
}

// heldTakeOver sets in's take-over hook so that the body of its first take-over is not run but
// handed to the channel it answers, for the test to run where its schedule says, and the body of
// any later take-over runs at once. A test calls it in startHeld's set, before that first take-over.
func heldTakeOver(in *Intake) <-chan func() {
	bodies := make(chan func(), 1)
	var held atomic.Bool
	in.takeOverHook = func(body func()) {
		if held.CompareAndSwap(false, true) {
			bodies <- body
			return
		}
		body()
	}
	return bodies
}

// queuedCalls sets in's call-queued hook to send the project of each starting point callRead
// queues to the channel it answers, which holds more than any test here queues: the count of the
// calls queued is the channel's length.
func queuedCalls(in *Intake) <-chan string {
	calls := make(chan string, 16)
	in.callQueued = func(project string) { calls <- project }
	return calls
}

// waitingOn is how many of project's messages the running intake counts as waiting on the
// project's runner (Intake.waiting): messageKey counts a message up as it keys it there, and
// deliver counts it down as it returns.
func waitingOn(rig *intakeRig, project string) int {
	rig.intake.mu.Lock()
	defer rig.intake.mu.Unlock()
	return rig.intake.waiting.projects[project]
}

// awaitWaiting waits for the running intake to count want of project's messages waiting on the
// project's runner (waitingOn).
func awaitWaiting(t *testing.T, rig *intakeRig, project string, want int) {
	t.Helper()
	testwait.Until(t, fmt.Sprintf("%d of %s's messages to wait on its runner", want, project), func() (int, bool) {
		waiting := waitingOn(rig, project)
		return waiting, waiting == want
	})
}

// keyedThrough publishes a message of project, which has no Legion setting, and waits for the
// intake to drop it (droppedLine). The durable delivers the stream's messages in order, and the
// subscription's callback keys each of them before it takes the next (nats.go runs one
// subscription's callbacks one at a time), so once that message has been handled, every message
// published before it has been keyed. droppedLine is logged once per project per dropLogEvery, so
// each call names a project of its own.
func keyedThrough(t *testing.T, rig *intakeRig, project string) {
	t.Helper()
	rig.publish(project, change(1, project+"-1", moved("phase", "planning")))
	testwait.Until(t, fmt.Sprintf("the intake to drop the message of %s, which has no setting", project), func() (int, bool) {
		dropped := 0
		for _, record := range rig.logRecords(droppedLine) {
			if record["project"] == project {
				dropped++
			}
		}
		return dropped, dropped >= 1
	})
}

// TestAMessageKeyedBeforeTheTakeOversCallbackRanCountsTowardTheCap: a project keeps at most
// stateMaxWaiting messages waiting on its runner, the messages a take-over's subscription keys
// before the take-over's callback runs included. A take-over subscribes and creates the durable,
// so its subscription is delivered messages and keys them, before it records its deliver subject
// and calls takenOver (delivery's finishTakeOver). The test holds takenOver's body back
// (Intake.takeOverHook) until INFRA's first message, INFRA having no cursor yet, has been keyed to
// its runner and makes the starting read, which INFRA's daemon holds; then the body runs. INFRA
// publishes 15 more messages at or below the read's 30, which wait on the runner behind it, and 31
// to 33 past it: 31, the 17th message the subscription keyed, is the first past the cap, holds the
// gap read and goes back to the stream once, the cursor names it as the read's holder, no read is
// made beside the runner, and INFRA holds stateMaxWaiting+1 messages of the window.
//
// At a869784c takenOver started the counts again at 0, so the first message was counted in none:
// 31 waited on the runner as its 17th message and 32 was the first past the cap, as one CI run of
// TestMessagesPastTheCapOfAProjectWithNoCursorHoldOneMessageAndAreReadOnTheRunner found
// (dispatch://INFRA-3904/comment/47d7a6b4-9f56-485d-98fc-d2434980b987).
func TestAMessageKeyedBeforeTheTakeOversCallbackRanCountsTowardTheCap(t *testing.T) {
	t.Parallel()
	rig := newIntakeRig(t)
	var bodies <-chan func()
	rig.startHeld(func(in *Intake) { bodies = heldTakeOver(in) })
	daemon := newFakeDaemon(t, daemonState("INFRA", 30, inPhase("implementing")))
	daemon.holds(t)
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)

	rig.publish("INFRA", change(11, "INFRA-1", moved("phase", "implementing")))
	testwait.Until(t, "INFRA's first message to make the starting read", func() (int, bool) {
		served := daemon.served()
		return served, served >= 1
	})
	body := testwait.Receive(t, "the take-over's body", bodies)
	body()
	for seq := 12; seq < 12+stateMaxWaiting-1; seq++ {
		rig.publish("INFRA", change(seq, "INFRA-1", moved("phase", "implementing")))
	}
	rig.publish("INFRA", change(31, "INFRA-1", moved("phase", "testing")))
	past := rig.lastStreamSeq()
	rig.publish("INFRA", change(32, "INFRA-1", moved("phase", "merging")))
	rig.publish("INFRA", change(33, "INFRA-1", moved("phase", "reviewing")))

	holds := testwait.Until(t, "a message past INFRA's cap to go back to the stream holding the gap read", func() ([]map[string]any, bool) {
		holds := rig.logRecords(heldLine)
		return holds, len(holds) >= 1
	})
	if len(holds) != 1 || holds[0]["streamSeq"] != float64(past) || holds[0]["seq"] != float64(31) || holds[0]["reason"] != ReadReasonGap ||
		holds[0]["handBack"] != float64(1) {
		t.Fatalf("the messages sent back = %v, want one: the first message past the cap, 31 at stream sequence %d, the 17th INFRA's new subscription keyed, holding the gap read once; a message keyed before the take-over's callback ran was counted in none",
			holds, past)
	}
	if c := cursorOf(t, rig.database, "INFRA"); !slices.Equal(c.readReasons, []string{ReadReasonGap}) || c.readHolder == nil || *c.readHolder != int64(past) ||
		c.readHolds != 1 || c.lastSeq != nil || c.stateID != nil {
		t.Errorf("INFRA's cursor while its starting read is held is %s, want one that follows no daemon, owes the gap read and names 31 (stream sequence %d) its holder once", c, past)
	}
	if served := daemon.served(); served != 1 {
		t.Errorf("INFRA's daemon served %d reads while its starting read was held, want that read alone: a message past the cap made a read beside the runner", served)
	}
	testwait.Until(t, fmt.Sprintf("INFRA to hold %d messages of the window: %d waiting on its runner and the one holding the gap read", stateMaxWaiting+1, stateMaxWaiting), func() (int, bool) {
		pending := rig.consumerInfo().NumAckPending
		return pending, pending == stateMaxWaiting+1
	})
}

// TestACallForTheOwedReadQueuedBeforeTheTakeOversCallbackRanIsQueuedOnce: at most one call for a
// project's owed read waits on its runner (callRead, calling), the call a take-over's subscription
// queues before the take-over's callback runs included. INFRA's setting names a second daemon,
// whose starting read fails, so the read is owed for retry with the store INFRA followed before
// recorded; its minute passes, so the read is due and no message holds it. A second intake takes
// the durable over, and the test holds its takenOver's body back (Intake.takeOverHook). Two
// messages of the store recorded reach INFRA's runner, with work the test holds in front of each,
// so the second waits on the runner before the call the first queues behind itself. The first is
// let go, and calls for the owed read: callRead queues INFRA's starting point (Intake.callQueued
// counts it). The take-over's body then runs, and the second is let go: it calls for the same read
// while that starting point still waits behind it, and queues none. Once the starting point has
// made its read, callRead has queued one call.
//
// At a869784c takenOver emptied calling, so the second message found no call waiting and queued
// another.
func TestACallForTheOwedReadQueuedBeforeTheTakeOversCallbackRanIsQueuedOnce(t *testing.T) {
	t.Parallel()
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")
	named := newFakeDaemon(t, "")
	named.refuses(http.StatusServiceUnavailable)
	rig.pointAt(named, 2)
	if c := cursorOf(t, rig.database, "INFRA"); !slices.Equal(c.readReasons, []string{ReadReasonRetry}) || c.stateID != nil || c.readHolder != nil ||
		c.previousStateID == nil || *c.previousStateID != rigStateID {
		t.Fatalf("control: INFRA's cursor after the failed starting read is %s, want the read owed for retry, no store, no holder and the store %s recorded", c, rigStateID)
	}
	rig.stop()

	var bodies <-chan func()
	var calls <-chan string
	rig.startHeld(func(in *Intake) { bodies, calls = heldTakeOver(in), queuedCalls(in) })
	letFirst := rig.holdRunner("INFRA")
	rig.publish("INFRA", change(11, "INFRA-1", moved("phase", "merging")))
	awaitWaiting(t, rig, "INFRA", 1)
	letSecond := rig.holdRunner("INFRA")
	rig.publish("INFRA", change(12, "INFRA-1", moved("phase", "merging")))
	awaitWaiting(t, rig, "INFRA", 2)

	letFirst()
	testwait.Until(t, "the first message to queue the call for INFRA's owed read", func() (int, bool) {
		return len(calls), len(calls) >= 1
	})
	body := testwait.Receive(t, "the take-over's body", bodies)
	body()
	letSecond()
	// The starting point waits behind the second message, so once it has called the daemon the
	// second message has made its call.
	testwait.Until(t, "the starting point the first message queued to read the daemon the setting names", func() (int, bool) {
		served := named.served()
		return served, served >= 2
	})
	if queued := len(calls); queued != 1 {
		t.Errorf("callRead queued %d calls for INFRA's owed read, want 1: the second message called for the read the first had queued while that call still waited behind it, and the take-over's callback, which ran between them, emptied the record of it",
			queued)
	}
}

// TestAReplacedConnectionKeepsTheCountsAndTheCallOfTheSubscriptionItBindsAgain: a connection that
// the bus client replaces binds the intake's subscription again on a handle of its own at the same
// deliver subject, and calls no takenOver (bus.Client's restoreSubscriptions): the messages keyed
// before still wait on their runners, and the call queued before still waits on its runner, so the
// project's cap and its one waiting call hold across it. LINUX, with no cursor, makes its starting
// read at its first message, which its daemon holds, and 15 more messages wait behind it. INFRA's
// read is owed for retry and due, as in
// TestACallForTheOwedReadQueuedBeforeTheTakeOversCallbackRanIsQueuedOnce, and two messages of the
// store it followed before wait on its runner behind work the test holds in front of each; the
// first is let go and queues the call for the owed read. The intake's connection is then closed,
// and its client dials another and binds the subscription again, with no take-over. LINUX's 31 to
// 33 arrive past the read's snapshot: 31, its 17th message, goes past the cap and holds the gap
// read. INFRA's second message is let go, and queues no call: one is waiting.
//
// Counts or calls that began again at each subscription handle would hold LINUX's 31 to 33 on its
// runner, and have INFRA's second message queue a second call.
func TestAReplacedConnectionKeepsTheCountsAndTheCallOfTheSubscriptionItBindsAgain(t *testing.T) {
	t.Parallel()
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	var takeOvers atomic.Int32
	var calls <-chan string
	rig.startHeld(func(in *Intake) {
		calls = queuedCalls(in)
		in.takeOverHook = func(body func()) {
			takeOvers.Add(1)
			body()
		}
	})
	rig.awaitStartingRead("INFRA")
	named := newFakeDaemon(t, "")
	named.refuses(http.StatusServiceUnavailable)
	rig.pointAt(named, 2)
	if c := cursorOf(t, rig.database, "INFRA"); !slices.Equal(c.readReasons, []string{ReadReasonRetry}) || c.stateID != nil || c.readHolder != nil ||
		c.previousStateID == nil || *c.previousStateID != rigStateID {
		t.Fatalf("control: INFRA's cursor after the failed starting read is %s, want the read owed for retry, no store, no holder and the store %s recorded", c, rigStateID)
	}

	linux := newFakeDaemon(t, daemonState("LINUX", 30, planning("LINUX")))
	linux.holds(t)
	seedLegionProject(t, rig.database, "LINUX", linux.URL)
	rig.publish("LINUX", change(11, "LINUX-1", moved("phase", "implementing")))
	testwait.Until(t, "LINUX's first message to make the starting read", func() (int, bool) {
		served := linux.served()
		return served, served >= 1
	})
	for seq := 12; seq < 12+stateMaxWaiting-1; seq++ {
		rig.publish("LINUX", change(seq, "LINUX-1", moved("phase", "implementing")))
	}
	letFirst := rig.holdRunner("INFRA")
	rig.publish("INFRA", change(11, "INFRA-1", moved("phase", "merging")))
	awaitWaiting(t, rig, "INFRA", 1)
	letSecond := rig.holdRunner("INFRA")
	rig.publish("INFRA", change(12, "INFRA-1", moved("phase", "merging")))
	awaitWaiting(t, rig, "INFRA", 2)
	awaitWaiting(t, rig, "LINUX", stateMaxWaiting)
	letFirst()
	testwait.Until(t, "INFRA's first message to queue the call for INFRA's owed read", func() (int, bool) {
		return len(calls), len(calls) >= 1
	})

	replaced := rig.client.Connection()
	replaced.Close()
	testwait.Until(t, "the intake's client to bind its subscription again on a new connection, at the same deliver subject", func() (string, bool) {
		info := rig.consumerInfo()
		bound := rig.client.Connection() != replaced && rig.client.SubOK() && rig.client.SubSubject() == rig.subject &&
			info.PushBound && info.Config.DeliverSubject == rig.subject
		return fmt.Sprintf("the client's subscription delivers on %q (ok %t), the durable is bound %t to %q, want %q",
			rig.client.SubSubject(), rig.client.SubOK(), info.PushBound, info.Config.DeliverSubject, rig.subject), bound
	})
	if taken := takeOvers.Load(); taken != 1 {
		t.Fatalf("control: the intake was taken over %d times, want its first take-over alone: the connection replaced is no take-over", taken)
	}

	rig.publish("LINUX", change(31, "LINUX-1", moved("phase", "testing")))
	past := rig.lastStreamSeq()
	rig.publish("LINUX", change(32, "LINUX-1", moved("phase", "merging")))
	rig.publish("LINUX", change(33, "LINUX-1", moved("phase", "reviewing")))
	keyedThrough(t, rig, "QUIET")
	if waiting := waitingOn(rig, "LINUX"); waiting != stateMaxWaiting {
		t.Fatalf("%d of LINUX's messages wait on its runner once 31 to 33 are keyed, want %d: the subscription bound again on the new connection counted the messages keyed before it in none, so its 17th went on the runner",
			waiting, stateMaxWaiting)
	}
	holds := testwait.Until(t, "LINUX's message past the cap to go back to the stream holding the gap read", func() ([]map[string]any, bool) {
		holds := rig.logRecords(heldLine)
		return holds, len(holds) >= 1
	})
	if len(holds) != 1 || holds[0]["project"] != "LINUX" || holds[0]["streamSeq"] != float64(past) || holds[0]["seq"] != float64(31) ||
		holds[0]["reason"] != ReadReasonGap || holds[0]["handBack"] != float64(1) {
		t.Errorf("the messages sent back = %v, want one: LINUX's 17th message, 31 at stream sequence %d, past the cap, holding the gap read once", holds, past)
	}
	if c := cursorOf(t, rig.database, "LINUX"); !slices.Equal(c.readReasons, []string{ReadReasonGap}) || c.readHolder == nil || *c.readHolder != int64(past) ||
		c.readHolds != 1 {
		t.Errorf("LINUX's cursor while its starting read is held is %s, want it owing the gap read and naming 31 (stream sequence %d) its holder once", c, past)
	}

	letSecond()
	// The starting point waits behind INFRA's second message, so once it has called the daemon the
	// second message has made its call.
	testwait.Until(t, "the starting point INFRA's first message queued to read the daemon the setting names", func() (int, bool) {
		served := named.served()
		return served, served >= 2
	})
	if queued := len(calls); queued != 1 {
		t.Errorf("callRead queued %d calls for INFRA's owed read, want 1: INFRA's second message called for the read the first had queued before the connection was replaced, while that call still waited behind it",
			queued)
	}
	if taken := takeOvers.Load(); taken != 1 {
		t.Errorf("the intake was taken over %d times, want its first take-over alone", taken)
	}
}
