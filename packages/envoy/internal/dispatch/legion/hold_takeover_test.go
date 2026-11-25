package legion

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/testwait"
)

// TestATakeOverDuringAHeldReadThatTimesOutKeepsTheFloorAboutElevenMinutes: R3 bounds the time a
// held message holds the durable's ack floor at about 11 minutes, 11 reads and 10 hand-backs, and
// a lease take-over in the middle of the count does not lengthen it, however many take-overs there
// are. After INFRA's good starting read its daemon stops answering, as in
// TestAHeldReadThatTimesOutHoldsTheFloorAboutElevenMinutes: each read waits out the client's
// window, and the daemon moves the intake's clock RequestTimeout on at each read. A gap's message
// holds the read. After each step's hand-backs the intake stops and a second intake takes the
// durable over, as in TestAHoldsHandBacksAreCountedAcrossATakeOver. The message is acknowledged
// after stateMaxHolds hand-backs in all; on the intake's clock it held the floor for less than 11
// reads of RequestTimeout and a minute.
//
// At 88db4653 the take-over's own read of the owed project (takenOver, SettingsChanged,
// startingPoint, due since the last read began RequestTimeout before) runs on the project's runner
// ahead of the held message's redelivery. With a daemon that times out, that read adds one
// RequestTimeout to the hold, so the floor is held 12 reads long, and each further take-over adds
// one more. The tester measured the same on the real programs: two Dispatch processes, a daemon
// that never answers and a SIGTERM after four hand-backs held the floor 12m12s from the publish.
// R3 says a reason that arrives while a read is in flight or owed joins it, so the take-over's
// reason joins the held message's read and makes no read of its own.
func TestATakeOverDuringAHeldReadThatTimesOutKeepsTheFloorAboutElevenMinutes(t *testing.T) {
	for _, test := range []struct {
		name string
		// takeOvers lists, for each take-over, how many hand-backs the intakes have made in all
		// at the least before it.
		takeOvers []int
	}{
		{"one take-over", []int{4}},
		{"two take-overs", []int{3, 6}},
	} {
		t.Run(test.name, func(t *testing.T) {
			const failures = stateMaxHolds + 1
			// window is the client's window in the rig, as in
			// TestAHeldReadThatTimesOutHoldsTheFloorAboutElevenMinutes.
			const window = 200 * time.Millisecond
			transport := http.DefaultTransport.(*http.Transport).Clone()
			transport.ResponseHeaderTimeout = window
			shared := http.DefaultTransport
			http.DefaultTransport = transport
			t.Cleanup(func() {
				http.DefaultTransport = shared
				transport.CloseIdleConnections()
			})
			rig := newIntakeRig(t)
			daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
			seedLegionProject(t, rig.database, "INFRA", daemon.URL)
			rig.start()
			rig.awaitStartingRead("INFRA")

			daemon.stalls(func() { rig.clock.advance(RequestTimeout) })
			from := rig.clock.now()
			rig.publish("INFRA", change(12, "INFRA-1", moved("phase", "merging")))
			holder := rig.lastStreamSeq()
			var before []int
			for _, at := range test.takeOvers {
				rig.waitFor(time.Minute, func() bool { return len(rig.logRecords(heldLine)) >= at }, func() string {
					return fmt.Sprintf("the message went back %d times, want %d before take-over %d", len(rig.logRecords(heldLine)), at, len(before)+1)
				})
				rig.stop()
				before = append(before, len(rig.logRecords(heldLine)))
				rig.start()
			}

			var info *natsgo.ConsumerInfo
			rig.waitFor(2*time.Minute, func() bool {
				info = rig.consumerInfo()
				return info.AckFloor.Stream >= holder && info.NumAckPending == 0 && info.NumPending == 0
			}, func() string {
				return fmt.Sprintf("the message that holds INFRA's read is not acknowledged after %d hand-backs, %v of them before each take-over: %+v",
					len(rig.logRecords(heldLine)), before, info)
			})
			held := rig.clock.now().Sub(from)
			if got := rig.heldValues(holder, "handBack"); len(got) != stateMaxHolds {
				t.Errorf("the message went back %d times in all, %v of them before each take-over, want %d", len(got), before, stateMaxHolds)
			}
			reads := time.Duration(failures) * RequestTimeout
			if held >= reads+time.Minute {
				t.Errorf("on the intake's clock the message held the ack floor for %s across %d take-overs, want less than the %s of its %d reads and a minute: INFRA's daemon served %d reads that timed out, %d more than the message's own; each take-over's read of the owed project ran ahead of the held message's redelivery",
					held.Round(time.Second), len(test.takeOvers), reads, failures, daemon.served()-1, daemon.served()-1-failures)
			}
		})
	}
}

// TestASettingWrittenDuringAHeldReadThatTimesOutKeepsTheFloorAboutElevenMinutes: R3 bounds the
// time a held message holds the durable's ack floor at about 11 minutes, 11 reads and 10
// hand-backs, and no sequence of writes of the project's own setting lengthens it. After INFRA's
// good starting read its daemon stops answering, as in
// TestAHeldReadThatTimesOutHoldsTheFloorAboutElevenMinutes, and a gap's message holds the read.
// During the count a person writes INFRA's setting:
//   - naming the same daemon, once or three times. R3 says a project has at most one whole read in
//     flight and at most one owed, and a reason that arrives while one is in flight or owed joins
//     it. So each write's read joins the held read, as a take-over's does, and the count goes on:
//     the message is acknowledged after stateMaxHolds hand-backs in all, and on the intake's clock
//     it held the floor for less than its 11 reads and a minute.
//   - naming another daemon, which does not answer either. R3 applies no message of the daemon the
//     setting named before once the setting names another, so the held message, the old daemon's,
//     is finished at its next delivery after the write, unapplied, and goes back no more: its
//     delay is holdAtLeast in the rig, so the floor is released within seconds of the write. The
//     new daemon's starting read is made at once, and no message holds it.
//
// At f77fd4c7 each write of the setting deletes the project's cursor (settingWritten), and the
// count of the hold's hand-backs with it (read_holds), so the message's next redelivery holds the
// read again from 1: the tester measured hand-backs 1 to 4 and then 1 to 10, 14 in all, and the
// floor held 16m17s. Each further write of the setting starts the count again.
func TestASettingWrittenDuringAHeldReadThatTimesOutKeepsTheFloorAboutElevenMinutes(t *testing.T) {
	for _, test := range []struct {
		name string
		// writes lists, for each write of the setting, how many hand-backs the message has made
		// at the least before it.
		writes []int
		// another names another daemon, which stalls as INFRA's does, in the setting written.
		another bool
	}{
		{"the same daemon", []int{4}, false},
		{"the same daemon three times", []int{2, 4, 6}, false},
		{"another daemon that does not answer", []int{4}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			const failures = stateMaxHolds + 1
			const window = 200 * time.Millisecond
			transport := http.DefaultTransport.(*http.Transport).Clone()
			transport.ResponseHeaderTimeout = window
			shared := http.DefaultTransport
			http.DefaultTransport = transport
			t.Cleanup(func() {
				http.DefaultTransport = shared
				transport.CloseIdleConnections()
			})
			rig := newIntakeRig(t)
			daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
			seedLegionProject(t, rig.database, "INFRA", daemon.URL)
			rig.start()
			rig.awaitStartingRead("INFRA")

			stall := func() { rig.clock.advance(RequestTimeout) }
			daemon.stalls(stall)
			from := rig.clock.now()
			rig.publish("INFRA", change(12, "INFRA-1", moved("phase", "merging")))
			holder := rig.lastStreamSeq()
			named := daemon
			if test.another {
				named = newFakeDaemon(t, "")
				named.stalls(stall)
			}
			var wroteAt time.Time
			var atWrite int
			for i, at := range test.writes {
				rig.waitFor(time.Minute, func() bool { return len(rig.logRecords(heldLine)) >= at }, func() string {
					return fmt.Sprintf("the message went back %d times, want %d before write %d of the setting", len(rig.logRecords(heldLine)), at, i+1)
				})
				atWrite, wroteAt = len(rig.logRecords(heldLine)), time.Now()
				if _, err := PutSettings(context.Background(), rig.database.Pool, "INFRA", named.URL, model.Actor{Kind: "user", ID: "alice@example.com"}); err != nil {
					t.Fatalf("write INFRA's setting again: %v", err)
				}
				rig.wake <- struct{}{}
			}

			var info *natsgo.ConsumerInfo
			acknowledged := func() bool {
				info = rig.consumerInfo()
				return info.AckFloor.Stream >= holder && info.NumAckPending == 0 && info.NumPending == 0
			}
			if test.another {
				const within = 10 * time.Second
				if !testwait.Holds(within, 25*time.Millisecond, acknowledged) {
					t.Fatalf("the message of the daemon the setting named before was not finished within %s of the write that named another daemon, %d hand-backs in all, %d at the write: %+v",
						within, len(rig.logRecords(heldLine)), atWrite, info)
				}
				if after := len(rig.heldValues(holder, "handBack")) - atWrite; after > 1 {
					t.Errorf("the message of the daemon the setting named before went back %d more times after the write that named another daemon, want at most the one in flight at the write", after)
				}
				if got := history(t, rig.database, "INFRA", "INFRA-1", FieldPhase); slices.Contains(got, `"merging"`) {
					t.Errorf("the phase's history is %v: the message of the daemon the setting named before was applied", got)
				}
				if named.served() < 1 {
					t.Errorf("the daemon the setting names was never read")
				}
				t.Logf("finished %s after the write, %d hand-backs in all", time.Since(wroteAt).Round(time.Millisecond), len(rig.logRecords(heldLine)))
				return
			}
			rig.waitFor(2*time.Minute, acknowledged, func() string {
				return fmt.Sprintf("the message that holds INFRA's read is not acknowledged after %d hand-backs: %+v", len(rig.logRecords(heldLine)), info)
			})
			held := rig.clock.now().Sub(from)
			handBacks := rig.heldValues(holder, "handBack")
			reads := time.Duration(failures) * RequestTimeout
			if len(handBacks) > stateMaxHolds || held >= reads+time.Minute {
				t.Errorf("the message went back %d times, counted %v, and held the ack floor for %s on the intake's clock across %d write(s) of the setting, want at most %d hand-backs and less than %s, its %d reads and a minute: each write's read joins the held read",
					len(handBacks), handBacks, held.Round(time.Second), len(test.writes), stateMaxHolds, reads+time.Minute, failures)
			}
		})
	}
}

// startingReadHeld brings INFRA to a cursor that follows no daemon yet and whose starting read a
// message holds (read-rule-cases state S0h), on a rig whose client window is 200 ms, and answers the
// rig, INFRA's daemon, the holder's stream sequence, the intake's clock as the holder was published,
// and the moment the starting read is due on that clock. INFRA's take-over read is good at 10. A
// write of INFRA's own URL a moment later finds no message holding a read, so it resets the cursor
// (row 6.2), and its wake finds the starting read not due: the good read began less than a minute
// before. From then on the daemon stalls every read, moving the intake's clock RequestTimeout on at
// each, as in TestAHeldReadThatTimesOutHoldsTheFloorAboutElevenMinutes. With left of that minute to
// go, INFRA's message at 12 calls for the starting read, which is not due (row 3.2): it holds the
// read and goes back to the stream for what is left of the minute. startingReadHeld fails unless
// the message went back so, once, and the cursor then follows no daemon, owes the starting read
// alone and names that message as the holder, gone back once.
func startingReadHeld(t *testing.T, left time.Duration) (rig *intakeRig, daemon *fakeDaemon, holder uint64, from, due time.Time) {
	t.Helper()
	windowed(t, 200*time.Millisecond)
	rig = newIntakeRig(t)
	daemon = newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitWholeReads(1)
	requireWholeRead(t, wholeReadsOf(t, rig.database, "INFRA")[0], ReadReasonStart, true)
	due = lastReadStartedAt(t, rig, "INFRA").Add(ReadRetryAfter)

	if _, err := PutSettings(t.Context(), rig.database.Pool, "INFRA", daemon.URL, model.Actor{Kind: "user", ID: "alice@example.com"}); err != nil {
		t.Fatalf("write INFRA's setting again with its own URL: %v", err)
	}
	rig.wake <- struct{}{}
	if c := cursorOf(t, rig.database, "INFRA"); c.lastSeq != nil || c.stateID != nil || c.readOwed || c.readHolder != nil {
		t.Fatalf("INFRA's cursor after a write of its own URL that found no holder is %s, want it reset: no sequence, no store, nothing owed and no holder", c)
	}

	daemon.stalls(func() { rig.clock.advance(RequestTimeout) })
	rig.clock.advance(due.Add(-left).Sub(rig.clock.now()))
	from = rig.clock.now()
	rig.publish("INFRA", change(12, "INFRA-1", moved("phase", "merging")))
	holder = rig.lastStreamSeq()
	held := rig.awaitLogRecords(heldLine, 1)[0]
	if delay, _ := held["delay"].(float64); held["streamSeq"] != float64(holder) || held["reason"] != ReadReasonStart || held["handBack"] != float64(1) ||
		time.Duration(delay) > left || time.Duration(delay) < left-5*time.Second {
		t.Fatalf("the message sent back = %v, want the message at 12 (stream sequence %d) holding the starting read once, for the %s or so left of the minute", held, holder, left)
	}
	if !requireStartingReadHeld(t, rig, holder, 1, "its message held the starting read") {
		t.FailNow()
	}
	return rig, daemon, holder, from, due
}

// requireStartingReadHeld fails the test unless INFRA's cursor follows no daemon yet, owes the
// starting read alone and names the message at stream sequence holder as its holder, gone back
// holds times, as a message that holds the starting read leaves it (read-rule-cases state S0h), and
// reports whether it did. after names the step the cursor is read after.
func requireStartingReadHeld(t *testing.T, rig *intakeRig, holder uint64, holds int, after string) bool {
	t.Helper()
	c := cursorOf(t, rig.database, "INFRA")
	if c.stateURL != "" || c.readHolder == nil || *c.readHolder != int64(holder) || c.readHolds != holds ||
		!slices.Equal(c.readReasons, []string{ReadReasonStart}) || c.lastSeq != nil || c.stateID != nil {
		t.Errorf("INFRA's cursor after %s is %s, want it to follow no daemon yet, owe the starting read alone and name the message at stream sequence %d as its holder, gone back %d times",
			after, c, holder, holds)
		return false
	}
	return true
}

// TestASettingWrittenWhileAMessageHoldsTheStartingReadKeepsTheFloorAboutElevenMinutes: R3 bounds
// the time a held message holds the durable's ack floor at about 11 minutes, 10 hand-backs, and
// says that the redelivery of the message that holds an owed read makes it and that a reason that
// arrives while a read is owed joins it, a starting read included (read-rule-cases rows 3.2 and
// 6.1b, state S0h: the architect's rulings of 2026-11-24T23:29Z and 2026-11-25T00:15Z).
// INFRA's message holds its starting read while the cursor follows no daemon yet
// (startingReadHeld), and the daemon stalls every read. A person writes INFRA's setting with its
// own URL again, each write with its wake:
//   - once, a second after the starting read became due on the intake's clock, while the message
//     waits out its hand-back and before its redelivery first makes the read;
//   - in the second case also once before the starting read is due, and twice once the message's
//     redeliveries have made reads that failed, as in
//     TestASettingWrittenDuringAHeldReadThatTimesOutKeepsTheFloorAboutElevenMinutes.
//
// Each write leaves the cursor as it was, so the holder and its count go on, and no wake makes a
// read of its own: each joins the held read. The message's next delivery makes the starting read,
// each delivery after it makes one more, and the message is acknowledged after stateMaxHolds
// hand-backs, counted 1 to 10 with no restart. On the intake's clock it held the floor for less
// than its stateMaxHolds+1 reads of RequestTimeout and a minute, and INFRA's daemon served the good
// starting read and the message's stateMaxHolds reads, and no other.
//
// The test catches a revert of the rule that a write of the same URL keeps the hold
// (settingWritten, resetCursor), which fails at the first write of the test, and a revert of the
// rule that a call no message makes joins a held starting read (cursor.readFor): the wake of the
// write made once the read is due runs on the project's runner while the message waits out the
// rest of its hand-back, left in the rig, so that wake, or the call that the wake of
// startingReadHeld's write deferred to the same moment (a call of the same kind), reads the daemon
// first, and the daemon serves one read too many. A revert of the rule that such a call defers no
// call (Intake.startingPoint) alone only adds a deferred call for the write made before the read is
// due, which then joins the held read too: this test does not catch it.
//
// At 0a0ea48a a write of the same URL reset the cursor that followed no daemon yet, dropping the
// holder and its count (resetCursor kept the cursor only where it followed the write's URL), so the
// message's next delivery held the read again from a count of 1, 11 hand-backs and more in all. The
// seeded model test found it on 8 seeds at 0dc86e28: "the message at stream sequence N went back to
// the stream as hand-back 1 after hand-back M, and nothing ended its hold between". And a wake's
// call read the daemon while the message held the starting read, since readFor made a due starting
// read for any call no message makes, so the hold took one more read.
func TestASettingWrittenWhileAMessageHoldsTheStartingReadKeepsTheFloorAboutElevenMinutes(t *testing.T) {
	for _, test := range []struct {
		name string
		// early writes the setting once more before the starting read is due.
		early bool
		// later lists, for each write after the message first made the read, how many hand-backs
		// it has made at the least before it.
		later []int
	}{
		{"once while the starting read is held", false, nil},
		{"twice while the starting read is held and twice after it was made", true, []int{4, 6}},
	} {
		t.Run(test.name, func(t *testing.T) {
			const left = 10 * time.Second
			rig, daemon, holder, from, due := startingReadHeld(t, left)
			write := func() {
				t.Helper()
				if _, err := PutSettings(context.Background(), rig.database.Pool, "INFRA", daemon.URL, model.Actor{Kind: "user", ID: "alice@example.com"}); err != nil {
					t.Fatalf("write INFRA's setting again with its own URL: %v", err)
				}
			}
			if test.early {
				write()
				requireStartingReadHeld(t, rig, holder, 1, "a write of its own URL before the starting read was due")
				rig.wake <- struct{}{}
			}
			rig.clock.advance(due.Add(time.Second).Sub(rig.clock.now()))
			write()
			requireStartingReadHeld(t, rig, holder, 1, "a write of its own URL once the starting read was due")
			rig.wake <- struct{}{}
			for i, at := range test.later {
				rig.waitFor(time.Minute, func() bool { return len(rig.logRecords(heldLine)) >= at }, func() string {
					return fmt.Sprintf("the message went back %d times, want %d before write %d after the starting read was made", len(rig.logRecords(heldLine)), at, i+1)
				})
				write()
				if c := cursorOf(t, rig.database, "INFRA"); c.readHolder == nil || *c.readHolder != int64(holder) || c.readHolds < at {
					t.Errorf("INFRA's cursor after write %d of its own URL after the starting read was made is %s, want the message at stream sequence %d still its holder, gone back %d times at the least",
						i+1, c, holder, at)
				}
				rig.wake <- struct{}{}
			}

			requireHoldEndedWithinElevenMinutes(t, rig, holder, from, "no write of the same URL starts the count again")
			if served := daemon.served(); served != 1+stateMaxHolds {
				t.Errorf("INFRA's daemon served %d reads, want the good starting read and the message's %d: a wake read while the message held the starting read",
					served, stateMaxHolds)
			}
		})
	}
}

// TestATakeOverWhileAMessageHoldsTheStartingReadKeepsTheFloorAboutElevenMinutes: R3 bounds the
// time a held message holds the durable's ack floor at about 11 minutes, 10 hand-backs, and says
// that the redelivery of the message that holds an owed read makes it and that a reason that
// arrives while a read is owed joins it (read-rule-cases row 7.5, state S0h: the architect's ruling
// of 2026-11-24T23:29Z). INFRA's message holds its starting read while the cursor follows no daemon
// yet (startingReadHeld), and the daemon stalls every read. The intake stops, the intake's clock
// moves a second past the moment the starting read is due, and a second intake takes the durable
// over. The take-over redelivers the message at once, and the message goes back once more (see
// below). The take-over's starting point makes no read: it joins the held read. The message's next
// delivery makes the starting read, each delivery after it makes one more, and the message is
// acknowledged after stateMaxHolds hand-backs, counted 1 to 10 across the take-over. On the
// intake's clock it held the floor for less than its stateMaxHolds+1 reads of RequestTimeout and a
// minute, and INFRA's daemon served the good starting read and the message's stateMaxHolds-1 reads
// (its first two deliveries found the starting read not due), and no other.
//
// The order on the project's runner is forced, not left to the race the fix would win by chance.
// The take-over's starting point (takenOver, SettingsChanged) and the message's redelivery reach the
// runner in no fixed order, and where the redelivery came first and made the read, the starting
// point would find a cursor that follows the daemon and join the read whatever the rule. So the
// test holds legion_settings locked (access exclusive) while the second intake takes the durable
// over. The redelivery waits for that lock on the project's runner (lockPosition), and the
// take-over's settings read (ListSettings) waits for it before it queues the starting point. Once
// both wait, the test lets the lock go, and the starting point queues behind the redelivery. And
// the second intake's clock stands still clamp before the read is due until the redelivery has
// gone back (heldLine), as it would where the redelivery came a moment before the minute ended: the
// redelivery finds the read not due and goes back for clamp, and the starting point, which runs
// right after it, finds the read due while the holder waits out its hand-back.
//
// The test catches a revert of the rule that a call no message makes joins a held starting read
// (cursor.readFor): the take-over's starting point then reads the daemon, at once, and the daemon
// serves one read too many. A revert of the rule that such a call defers no call
// (Intake.startingPoint) alone changes nothing here: the take-over's call finds the starting read
// due, and that rule only keeps a call that finds it held and not due from deferring one more call,
// which would join the held read too.
//
// At 0a0ea48a the take-over's starting point read the daemon while the message held the starting
// read, since readFor made a due starting read for any call no message makes: the hold took one
// more read of RequestTimeout, and the message's next delivery made a read of its own after it.
func TestATakeOverWhileAMessageHoldsTheStartingReadKeepsTheFloorAboutElevenMinutes(t *testing.T) {
	const left, clamp = 10 * time.Second, 5 * time.Second
	rig, daemon, holder, from, due := startingReadHeld(t, left)

	rig.stop()
	rig.clock.advance(due.Add(time.Second).Sub(rig.clock.now()))
	lock, err := rig.database.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("open the transaction that locks legion_settings: %v", err)
	}
	defer func() { _ = lock.Rollback(context.Background()) }()
	if _, err := lock.Exec(t.Context(), `lock table legion_settings in access exclusive mode`); err != nil {
		t.Fatalf("lock legion_settings: %v", err)
	}
	away := len(rig.logRecords(heldLine))
	var back atomic.Bool
	rig.startOn(func() time.Time {
		now := rig.clock.now()
		if !back.Load() && len(rig.logRecords(heldLine)) == away {
			if still := due.Add(-clamp); now.After(still) {
				return still
			}
			return now
		}
		back.Store(true)
		return now
	})
	var readers, lister int
	rig.waitFor(10*time.Second, func() bool {
		if err := rig.database.Pool.QueryRow(t.Context(), `
			select count(*) filter (where mode = 'RowShareLock'), count(*) filter (where mode = 'AccessShareLock')
			from pg_locks
			where relation = 'legion_settings'::regclass and not granted
				and database = (select oid from pg_database where datname = current_database())
		`).Scan(&readers, &lister); err != nil {
			t.Fatalf("read the waits on legion_settings: %v", err)
		}
		return readers >= 1 && lister >= 1
	}, func() string {
		return fmt.Sprintf("%d statements wait for legion_settings for share and %d to read it, want the redelivery's and the take-over's settings read", readers, lister)
	})
	before := daemon.served()
	if err := lock.Commit(t.Context()); err != nil {
		t.Fatalf("let legion_settings go: %v", err)
	}

	held := rig.awaitLogRecords(heldLine, away+1)[away]
	if delay, _ := held["delay"].(float64); held["streamSeq"] != float64(holder) || held["reason"] != ReadReasonStart || held["handBack"] != float64(2) ||
		time.Duration(delay) != clamp {
		t.Fatalf("the take-over's redelivery went back as %v, want the message at stream sequence %d holding the starting read for its second hand-back, for %s", held, holder, clamp)
	}
	if testwait.Holds(2*time.Second, 25*time.Millisecond, func() bool { return daemon.served() > before }) {
		t.Errorf("INFRA's daemon served %d reads from the take-over on, before the message came back from its hand-back of %s: the take-over's starting point read while the message held the starting read",
			daemon.served()-before, clamp)
	}

	requireHoldEndedWithinElevenMinutes(t, rig, holder, from, "no take-over starts the count again")
	if served := daemon.served(); served != stateMaxHolds {
		t.Errorf("INFRA's daemon served %d reads, want the good starting read and the message's %d: the take-over's starting point read while the message held the starting read",
			served, stateMaxHolds-1)
	}
}

// requireHoldEndedWithinElevenMinutes waits up to two minutes for the intake to acknowledge the
// message at stream sequence holder and to hold no message, then fails the test unless the cursor
// counted the message's hand-backs 1 to stateMaxHolds with no restart and, on the intake's clock
// from from, the message held the durable's ack floor for less than its stateMaxHolds+1 reads of
// RequestTimeout and a minute. why names what may not start the count again.
func requireHoldEndedWithinElevenMinutes(t *testing.T, rig *intakeRig, holder uint64, from time.Time, why string) {
	t.Helper()
	var info *natsgo.ConsumerInfo
	rig.waitFor(2*time.Minute, func() bool {
		info = rig.consumerInfo()
		return info.AckFloor.Stream >= holder && info.NumAckPending == 0 && info.NumPending == 0
	}, func() string {
		return fmt.Sprintf("the message that holds INFRA's read is not acknowledged after %d hand-backs: %+v", len(rig.logRecords(heldLine)), info)
	})
	held := rig.clock.now().Sub(from)
	if got := rig.heldValues(holder, "handBack"); !slices.Equal(got, oneTo(stateMaxHolds)) {
		t.Errorf("the cursor counted the hand-backs %v, want %v: %s", got, oneTo(stateMaxHolds), why)
	}
	reads := time.Duration(stateMaxHolds+1) * RequestTimeout
	if held >= reads+time.Minute {
		t.Errorf("on the intake's clock the message held the ack floor for %s, want less than %s, its %d reads and a minute",
			held.Round(time.Second), reads+time.Minute, stateMaxHolds+1)
	}
}

// TestAReadCutShortByAStopKeepsItsStart: R3-g starts the whole reads of one project at least
// ReadRetryAfter apart, and a read that a stop of the intake cuts short has called the daemon all
// the same (read-rule-cases row 7.6, the reviewer's round 6, finding 2 of review 6052791164).
// After INFRA's good starting read, its message at 12 shows a gap and makes the gap read at once,
// which the daemon holds for longer than the intake's stop budget (15 s). The intake stops while
// that read is in flight, and the stop gives the read up: the read records no legion_reads row,
// and the cursor keeps its start. A second intake takes the durable over when the intake's clock
// is left short of ReadRetryAfter after that start. Neither the take-over nor the message's
// redelivery reads INFRA before then: the take-over owes no read, and the redelivery holds the gap
// read until it is due. Then the redelivery makes one read, the gap read, which starts at least
// ReadRetryAfter after the read the stop cut short.
//
// At cdc5fd84 the start of a read the stop cut short was lost: readWhole kept it in memory only
// and returned before any write. The cursor kept the starting read's start, so the message's
// redelivery after the take-over read INFRA at once, inside the minute after the read before.
func TestAReadCutShortByAStopKeepsItsStart(t *testing.T) {
	// left is what is left of the minute after the read the stop cut short when the second intake
	// takes the durable over.
	const left = 5 * time.Second
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")
	first := lastReadStartedAt(t, rig, "INFRA")

	release := daemon.holds(t)
	from := rig.clock.now()
	rig.publish("INFRA", change(12, "INFRA-1", moved("phase", "testing")))
	rig.waitFor(20*time.Second, func() bool { return daemon.served() >= 2 }, func() string {
		return "INFRA's message at 12 never made the gap read"
	})
	to := rig.clock.now()
	rig.stop()

	if reads := wholeReadsOf(t, rig.database, "INFRA"); len(reads) != 1 {
		t.Fatalf("INFRA's whole reads after the stop are %v, want the starting read alone: the stop cut the gap read short", reads)
	}
	cut := lastReadStartedAt(t, rig, "INFRA")
	if cut.Before(from) || cut.After(to) {
		t.Errorf("after the stop INFRA's cursor keeps %s as the start of its newest whole read, want the start of the gap read the stop cut short, between %s and %s; the starting read began at %s",
			cut.Format(time.RFC3339Nano), from.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano), first.Format(time.RFC3339Nano))
		cut = from
	}

	daemon.serves(daemonState("INFRA", 12, inPhase("testing")))
	release()
	due := cut.Add(ReadRetryAfter)
	rig.clock.advance(due.Add(-left).Sub(rig.clock.now()))
	rig.start()
	rig.waitFor(left+15*time.Second, func() bool { return len(wholeReadsOf(t, rig.database, "INFRA")) >= 2 }, func() string {
		return fmt.Sprintf("INFRA was not read again once the minute after the read the stop cut short ended: its cursor is %s", cursorOf(t, rig.database, "INFRA"))
	})
	requireWholeRead(t, wholeReadsOf(t, rig.database, "INFRA")[1], ReadReasonGap, true)
	if started := lastReadStartedAt(t, rig, "INFRA"); started.Before(due) {
		t.Errorf("the read after the take-over began %s after the read the stop cut short, on the intake's clock, want at least %s",
			started.Sub(cut), ReadRetryAfter)
	}
	if testwait.Holds(2*time.Second, 25*time.Millisecond, func() bool { return daemon.served() > 3 }) || daemon.served() != 3 {
		t.Errorf("INFRA's daemon served %d reads, want the starting read, the gap read the stop cut short and one read after the take-over", daemon.served())
	}
}

// TestASettingNamingAnotherDaemonFinishesTheOldDaemonsMessagesWithNoRead: R3 applies no message
// of the daemon a setting named before once it names another, and a failed starting read is one
// "which no message holds". So, after a write that names another daemon, the old daemon's held
// message is finished unapplied at its next redelivery, once the write's starting read of the new
// daemon, which it waits behind on the project's runner, has ended: within one RequestTimeout and a
// second of the write. It makes no read of its own or of the new daemon, and holds none. Once the
// new daemon's failed starting read is due again, a later message of the old daemon's store is
// acknowledged unapplied the same way, within finishedWithin of its publish: it waits on no read
// and holds none. Until a read of the new URL has succeeded, Dispatch cannot know whether the URL
// names another store or the same store behind a new URL, so that message calls for the owed
// read, which the project's runner makes after the message is acknowledged, and it calls for no
// more than that one read.
//
// The rig's client window is readWindow, long enough to tell a message that waits behind a read,
// or makes one, from one that does neither. INFRA's daemon stops answering after its good starting
// read, and a gap's message holds the owed read. After two hand-backs a person writes INFRA's
// setting naming another daemon, which does not answer either. The old message's next delivery
// comes holdAtLeast after its last hand-back and waits behind the write's starting read, which
// waits out readWindow, so it is finished within readWindow and a second of the write, and the new
// daemon has served the write's own starting read alone.
//
// At 3e6a6784 the old message's redelivery waits on the project's runner behind the write's
// starting read of the new daemon, then makes the new daemon's read owed for retry
// (outcomeForgotten calls readFor as a call no message makes), and is finished only after it: on
// the real programs 2m0s after the write. A later message of the old store makes that read too
// once it is due, before it is acknowledged.
func TestASettingNamingAnotherDaemonFinishesTheOldDaemonsMessagesWithNoRead(t *testing.T) {
	const readWindow = 5 * time.Second
	const finishedWithin = 3 * time.Second
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = readWindow
	shared := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() {
		http.DefaultTransport = shared
		transport.CloseIdleConnections()
	})
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")

	stall := func() { rig.clock.advance(RequestTimeout) }
	daemon.stalls(stall)
	rig.publish("INFRA", change(12, "INFRA-1", moved("phase", "merging")))
	holder := rig.lastStreamSeq()
	rig.waitFor(2*time.Minute, func() bool { return len(rig.logRecords(heldLine)) >= 2 }, func() string {
		return fmt.Sprintf("the message went back %d times, want 2 before the write", len(rig.logRecords(heldLine)))
	})
	named := newFakeDaemon(t, "")
	named.stalls(stall)
	wroteAt := time.Now()
	if _, err := PutSettings(context.Background(), rig.database.Pool, "INFRA", named.URL, model.Actor{Kind: "user", ID: "alice@example.com"}); err != nil {
		t.Fatalf("point INFRA at another daemon: %v", err)
	}
	rig.wake <- struct{}{}
	finishedThrough := func(seq uint64) func() bool {
		return func() bool {
			info := rig.consumerInfo()
			return info.AckFloor.Stream >= seq && info.NumAckPending == 0 && info.NumPending == 0
		}
	}
	rig.waitFor(time.Minute, finishedThrough(holder), func() string {
		return fmt.Sprintf("the old daemon's held message is not finished a minute after the write: %+v", rig.consumerInfo())
	})
	if took := time.Since(wroteAt); took >= readWindow+time.Second {
		t.Errorf("the old daemon's held message was finished %s after the write naming another daemon, want less than %s, the write's starting read of %s and a second: it waited behind more than that read, or made one",
			took.Round(time.Millisecond), readWindow+time.Second, readWindow)
	}
	if served := named.served(); served != 1 {
		t.Errorf("the daemon the setting names served %d reads by the time the old daemon's message was finished, want 1, the write's own starting read: the old daemon's message made a read", served)
	}

	// The new daemon's failed starting read is due again a minute on. A later message of the old
	// daemon's store is acknowledged unapplied within finishedWithin of its publish, and at most
	// one read of the new daemon follows it while the new URL is unverified.
	rig.waitFor(time.Minute, func() bool { return cursorOf(t, rig.database, "INFRA").readOwed }, func() string {
		return fmt.Sprintf("the new daemon's starting read has not failed a minute after the write: INFRA's cursor is %s", cursorOf(t, rig.database, "INFRA"))
	})
	if c := cursorOf(t, rig.database, "INFRA"); !slices.Equal(c.readReasons, []string{ReadReasonRetry}) || c.stateID != nil || c.readHolder != nil {
		t.Fatalf("INFRA's cursor after the new daemon's failed starting read is %s, want the read owed for retry, no store and no holder", c)
	}
	rig.clock.advance(ReadRetryAfter + time.Second)
	before := named.served()
	published := time.Now()
	rig.publish("INFRA", change(13, "INFRA-1", moved("phase", "testing")))
	later := rig.lastStreamSeq()
	rig.waitFor(time.Minute, finishedThrough(later), func() string {
		return fmt.Sprintf("the later message of the old daemon's store is not finished: %+v", rig.consumerInfo())
	})
	if took := time.Since(published); took >= finishedWithin {
		t.Errorf("the later message of the old daemon's store was finished %s after its publish, want less than %s: it waited behind a read, or made one, of %s",
			took.Round(time.Millisecond), finishedWithin, readWindow)
	}
	if testwait.Holds(readWindow+2*time.Second, 25*time.Millisecond, func() bool { return named.served() > before+1 }) {
		t.Errorf("the daemon the setting names served %d reads after the later message of the old daemon's store, want %d at the most: that message called for more than the one owed read", named.served(), before+1)
	}
	if got := history(t, rig.database, "INFRA", "INFRA-1", FieldPhase); slices.Contains(got, `"merging"`) || slices.Contains(got, `"testing"`) {
		t.Errorf("the phase's history is %v: a message of the daemon the setting named before was applied", got)
	}
}

// TestAnEventOfTheSameStoreDuringTheReadThatVerifiesANewURLIsApplied: R3 applies every event of
// the daemon its setting names, and a project that goes quiet is at most one minute behind while
// its reads succeed. A setting written to a new URL that serves the same store (the daemon moved)
// is unverified until a read of that URL succeeds. While that read is in flight, the daemon
// commits and publishes an event past the read's snapshot. The read then lands, verifies the
// store, and the project goes quiet. The event is applied within about a minute of the read.
//
// INFRA's daemon is read whole at 10. The setting names a second URL that serves INFRA's own
// store at 11, and the test holds that answer. While it is held, the daemon publishes 12. The test
// lets the answer go, and the read verifies the store at 11. Then nothing is published: on the
// intake's clock a minute passes, and INFRA-1's phase reaches 12's testing within 75 s.
//
// At 86ded796 the intake takes 12 as a message of the store the setting named before, since no
// read of the new URL has succeeded, and acknowledges it unapplied beside the project's runner
// while the read is in flight. The read verifies the store at 11 and clears the record, and
// nothing applies 12 or owes the read that would: INFRA stays at 11 until its next message.
func TestAnEventOfTheSameStoreDuringTheReadThatVerifiesANewURLIsApplied(t *testing.T) {
	implementing := strings.Replace(planning("INFRA"), `"planning"`, `"implementing"`, 1)
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")

	relocated := newFakeDaemon(t, daemonState("INFRA", 11, implementing))
	release := relocated.holds(t)
	if _, err := PutSettings(context.Background(), rig.database.Pool, "INFRA", relocated.URL, model.Actor{Kind: "user", ID: "alice@example.com"}); err != nil {
		t.Fatalf("point INFRA at the URL its daemon moved to: %v", err)
	}
	rig.wake <- struct{}{}
	rig.waitFor(20*time.Second, func() bool { return relocated.served() >= 1 }, func() string {
		return "the write's starting read of the URL the daemon moved to never began"
	})

	// The event past the read's snapshot reaches the intake while the read is held.
	rig.publish("INFRA", change(12, "INFRA-1", moved("phase", "testing")))
	rig.waitFor(20*time.Second, func() bool { return rig.consumerInfo().NumPending == 0 }, func() string {
		return fmt.Sprintf("the message at 12 was not delivered while the read was held: %+v", rig.consumerInfo())
	})
	time.Sleep(time.Second)
	release()
	rig.awaitWholeReads(2)
	if c := cursorOf(t, rig.database, "INFRA"); c.stateID == nil || *c.stateID != rigStateID {
		t.Fatalf("control: INFRA's cursor after the read of the URL the daemon moved to is %s, want the store %s it serves", c, rigStateID)
	}

	rig.clock.advance(ReadRetryAfter + time.Second)
	if !testwait.Holds(75*time.Second, 200*time.Millisecond, func() bool {
		return slices.Contains(history(t, rig.database, "INFRA", "INFRA-1", FieldPhase), `"testing"`)
	}) {
		t.Errorf("75 s after the read that verified the store, with the project quiet, INFRA-1's phase history is %v and INFRA's cursor is %s: the event at 12, published past the read's snapshot while it was in flight, was never applied and nothing owes the read that would",
			history(t, rig.database, "INFRA", "INFRA-1", FieldPhase), cursorOf(t, rig.database, "INFRA"))
	}
}

// TestAStoreMarkedAsNoDaemonOfTheProjectIsFoundAgainWhenTheSettingsURLServesIt: R3 reads a project
// whole when a boot event names a run whose first sequence does not follow the last one applied,
// and a message of another store calls for a read, so that a daemon whose store was replaced is
// found again. A mark that a store is no daemon of the project (foreign_state_ids, read-rule-cases
// rows 3.1 and 3.4) does not stop that once the daemon behind the setting's URL serves the marked
// store.
//
// INFRA's setting names A, read well, then B, whose starting read fails, then C, read well in a
// store of its own. B still publishes: its first message makes one store read of C, which finds
// C's store unchanged and marks B's store. Then B takes C's place behind C's URL with no write of
// the setting, as a redeploy that starts B where C ran does: the URL now serves B's store at 40.
// B boots under a new run at 41 and publishes 42. Within about a minute, INFRA follows B's store
// and 42's testing is applied.
//
// At 107f6a10 every message of a marked store is finished unapplied as one of a store that is no
// daemon of the project (cursor.forgets), the boot included, and none calls for a read while the
// cursor holds a store. Only a write of the setting clears the mark, so INFRA stays on C's store
// at 5 and drops each of the daemon's messages.
func TestAStoreMarkedAsNoDaemonOfTheProjectIsFoundAgainWhenTheSettingsURLServesIt(t *testing.T) {
	const bootRun = "5d2e8a1c-7b3f-4e9a-a6c0-1f8b4d2e7c93"
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
	rig.publishIn("INFRA", bRun, bStore, time.Now().UTC().Truncate(time.Microsecond), false, change(20, "INFRA-1", moved("phase", "merging")))
	rig.settled()
	rig.waitFor(20*time.Second, func() bool { return slices.Equal(cursorOf(t, rig.database, "INFRA").foreignStateIDs, []string{bStore}) }, func() string {
		return fmt.Sprintf("control: B's store is not marked as no daemon of INFRA after B's message: INFRA's cursor is %s", cursorOf(t, rig.database, "INFRA"))
	})

	// B takes C's place behind C's URL, with no write of the setting, and boots there.
	c.serves(daemonStateIn("INFRA", 40, inPhase("reviewing"), bStore, bootRun))
	rig.clock.advance(ReadRetryAfter + time.Second)
	rig.publishIn("INFRA", bootRun, bStore, time.Now().UTC().Truncate(time.Microsecond), false, booted(41, stateSchema, 2))
	rig.publishIn("INFRA", bootRun, bStore, time.Now().UTC().Truncate(time.Microsecond), false, change(42, "INFRA-1", moved("phase", "testing")))
	if !testwait.Holds(75*time.Second, 200*time.Millisecond, func() bool {
		c := cursorOf(t, rig.database, "INFRA")
		return c.stateID != nil && *c.stateID == bStore && slices.Contains(history(t, rig.database, "INFRA", "INFRA-1", FieldPhase), `"testing"`)
	}) {
		t.Errorf("75 s after the daemon behind the setting's URL came to serve B's store and booted there, INFRA's cursor is %s and INFRA-1's phase history is %v; C's URL served %d reads: the boot and the event of the store marked as no daemon of INFRA were dropped, and nothing read the setting's daemon to find its store again",
			cursorOf(t, rig.database, "INFRA"), history(t, rig.database, "INFRA", "INFRA-1", FieldPhase), c.served())
	}
}

// TestTheRecordedOldStoreIsFoundAgainWhenTheSettingsURLServesIt: the variant of
// TestAStoreMarkedAsNoDaemonOfTheProjectIsFoundAgainWhenTheSettingsURLServesIt for the store a
// write recorded as the daemon the setting named before (previous_state_id). R3 reads a project
// whole when a boot names a run that does not follow, so a daemon whose store was replaced is
// found again; the architect ruled at 21:11Z that a boot of the recorded old store, while the
// cursor holds another verified store, calls for a store read, and a good read that finds that
// store clears the record, as read-rule-cases row 5.2 does.
//
// INFRA's setting names A, read well in the rig's store, then B, whose starting read fails, then
// C, read well in a store of its own, so A's store is the recorded old store. Then a redeploy
// starts A where C ran, with no write of the setting: C's URL serves A's store at 40. A boots
// under a new run at 41 and publishes 42. Within about a minute, INFRA follows A's store, the
// record is cleared, and 42's testing is applied.
//
// At b58c9b76 every message of the recorded old store is finished unapplied (cursor.forgets), the
// boot included, and none calls for a read while the cursor holds C's store.
func TestTheRecordedOldStoreIsFoundAgainWhenTheSettingsURLServesIt(t *testing.T) {
	const bootRun = "8b3d1f6e-2a4c-4d79-b0e5-3c7a9f1d6e28"
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
	if cur := cursorOf(t, rig.database, "INFRA"); cur.stateID == nil || *cur.stateID != cStore || cur.previousStateID == nil || *cur.previousStateID != rigStateID {
		t.Fatalf("control: INFRA's cursor after C's starting read is %s, want C's store %s and A's store %s recorded", cur, cStore, rigStateID)
	}

	// A takes C's place behind C's URL, with no write of the setting, and boots there.
	c.serves(daemonStateIn("INFRA", 40, inPhase("reviewing"), rigStateID, bootRun))
	rig.clock.advance(ReadRetryAfter + time.Second)
	rig.publishIn("INFRA", bootRun, rigStateID, time.Now().UTC().Truncate(time.Microsecond), false, booted(41, stateSchema, 2))
	rig.publishIn("INFRA", bootRun, rigStateID, time.Now().UTC().Truncate(time.Microsecond), false, change(42, "INFRA-1", moved("phase", "testing")))
	if !testwait.Holds(75*time.Second, 200*time.Millisecond, func() bool {
		c := cursorOf(t, rig.database, "INFRA")
		return c.stateID != nil && *c.stateID == rigStateID && c.previousStateID == nil &&
			slices.Contains(history(t, rig.database, "INFRA", "INFRA-1", FieldPhase), `"testing"`)
	}) {
		t.Errorf("75 s after the daemon behind the setting's URL came to serve A's store, the recorded old store, and booted there, INFRA's cursor is %s and INFRA-1's phase history is %v; C's URL served %d reads: the boot and the event of the recorded old store were dropped, and nothing read the setting's daemon to find its store again",
			cursorOf(t, rig.database, "INFRA"), history(t, rig.database, "INFRA", "INFRA-1", FieldPhase), c.served())
	}
}

// TestABootOfAMarkedOrOldStoreWhoseReadFindsTheCursorsStoreUnchangedAppliesNothing: R3 applies
// none of the messages of a daemon the setting does not name, and reads the state only for the
// reasons it names (read-rule-cases rows 3.5 and 5.7). A boot of a store marked as no daemon of
// the project, or of the recorded old store, calls for one store read while the cursor follows
// the setting's URL in another verified store. When that read finds the cursor's store unchanged,
// the boot is finished unapplied, the mark stays, the recorded old store is not marked, and the
// store's later messages call for no read.
//
// Each case sets INFRA's setting as the tests above do (A read well, B whose starting read fails,
// C read well in a store of its own), and C's URL keeps serving C's store: no redeploy took place.
func TestABootOfAMarkedOrOldStoreWhoseReadFindsTheCursorsStoreUnchangedAppliesNothing(t *testing.T) {
	const bootRun = "2c7a9e41-6d3b-4f85-a0e2-8b1d5c4f7a63"
	for _, tc := range []struct {
		name  string
		store string // the store that boots
		marks []string
	}{
		{name: "a store marked as no daemon of the project", store: bStore, marks: []string{bStore}},
		{name: "the recorded old store", store: rigStateID},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			if tc.marks != nil {
				rig.publishIn("INFRA", bRun, bStore, time.Now().UTC().Truncate(time.Microsecond), false, change(20, "INFRA-1", moved("phase", "merging")))
				rig.settled()
			}
			if cur := cursorOf(t, rig.database, "INFRA"); cur.stateID == nil || *cur.stateID != cStore || !slices.Equal(cur.foreignStateIDs, tc.marks) {
				t.Fatalf("control: INFRA's cursor before the boot is %s, want C's store %s and the marks %v", cur, cStore, tc.marks)
			}
			before := c.served()

			rig.clock.advance(ReadRetryAfter + time.Second)
			rig.publishIn("INFRA", bootRun, tc.store, time.Now().UTC().Truncate(time.Microsecond), false, booted(41, stateSchema, 2))
			rig.publishIn("INFRA", bootRun, tc.store, time.Now().UTC().Truncate(time.Microsecond), false, change(42, "INFRA-1", moved("phase", "testing")))
			rig.settled()
			cur := cursorOf(t, rig.database, "INFRA")
			if got := c.served() - before; got != 1 {
				t.Errorf("the boot of %s made %d reads of C's URL, want the one store read", tc.store, got)
			}
			if cur.stateID == nil || *cur.stateID != cStore || cur.lastSeq == nil || *cur.lastSeq != 5 ||
				cur.previousStateID == nil || *cur.previousStateID != rigStateID || !slices.Equal(cur.foreignStateIDs, tc.marks) {
				t.Errorf("after a read that found C's store unchanged, INFRA's cursor is %s; want C's store %s at 5, the recorded old store %s, and the marks %v",
					cur, cStore, rigStateID, tc.marks)
			}
			if phases := history(t, rig.database, "INFRA", "INFRA-1", FieldPhase); slices.Contains(phases, `"testing"`) {
				t.Errorf("INFRA-1's phase history is %v: an event of %s, which is no daemon of INFRA, was applied", phases, tc.store)
			}

			rig.clock.advance(ReadRetryAfter + time.Second)
			rig.publishIn("INFRA", bootRun, tc.store, time.Now().UTC().Truncate(time.Microsecond), false, change(43, "INFRA-1", moved("phase", "reviewing")))
			rig.settled()
			if got := c.served() - before; got != 1 {
				t.Errorf("after a later message of %s, C's URL served %d reads since the boot, want the boot's one: that message called for a read", tc.store, got)
			}
			if phases := history(t, rig.database, "INFRA", "INFRA-1", FieldPhase); slices.Contains(phases, `"reviewing"`) {
				t.Errorf("INFRA-1's phase history is %v: a later event of %s was applied", phases, tc.store)
			}
		})
	}
}

// TestEventsPastTheCapDuringTheReadThatFindsAForgottenStoreAreApplied: R3 keeps a project that goes
// quiet at most one minute behind while its reads succeed, and a read that verifies a store the
// cursor dropped (read-rule-cases rows 4.4, 5.2 and 3.5) takes that store's later events. Every
// message of a project waits on its runner behind a read in flight, so it is judged on the cursor
// the read leaves (TestAnEventOfTheSameStoreDuringTheReadThatVerifiesANewURLIsApplied). A message
// past the cap of stateMaxWaiting (row 8.1) does not wait: it is judged beside the runner while the
// read is in flight. Where its store is one the read is about to verify, it is no message of a
// store that is no daemon of the project, and its events, past the read's snapshot, are applied.
//
// Each case holds the read that will verify the store, publishes stateMaxWaiting messages of that
// store, which wait on the runner, and then three more past the cap, the last moving INFRA-1 to
// reviewing. The read is let go and verifies the store at its snapshot. INFRA then goes quiet; a
// minute on, on the intake's clock, INFRA-1's phase reaches reviewing within 75 s.
//   - the same store behind a new URL: a write names a URL that serves INFRA's own store (the
//     daemon moved), and the write's starting read of it is the read held.
//   - a marked store whose boot finds it: A, then B, whose reads all fail and whose store is
//     marked, then C; C's URL comes to serve B's store, B boots there, and the boot's store read
//     is the read held.
//
// At eb6e317a the messages past the cap are judged beside the runner (judgeCrowded) on the cursor
// as it stands before the read lands, which drops their store (cursor.forgets), so they are
// acknowledged unapplied and owe nothing. The read verifies the store at its snapshot, the
// messages that waited are applied, and INFRA stays at the last of them, behind the three past the
// cap, until its next message.
func TestEventsPastTheCapDuringTheReadThatFindsAForgottenStoreAreApplied(t *testing.T) {
	const bootRun = "7e1c4a92-3b5d-4f68-9c0a-5d2e8b1f6a47"
	// heldRead is where a case leaves INFRA: the daemon whose answer to the verifying read is held,
	// the release of that answer, the store and run of the daemon's messages, the first sequence
	// past the read's snapshot, and how many of INFRA's messages already wait on its runner.
	type heldRead struct {
		daemon     *fakeDaemon
		release    func()
		store, run string
		first      int
		waiting    int
	}
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, rig *intakeRig) heldRead
	}{
		{"the same store behind a new URL", func(t *testing.T, rig *intakeRig) heldRead {
			relocated := newFakeDaemon(t, daemonState("INFRA", 11, inPhase("implementing")))
			release := relocated.holds(t)
			if _, err := PutSettings(context.Background(), rig.database.Pool, "INFRA", relocated.URL, model.Actor{Kind: "user", ID: "alice@example.com"}); err != nil {
				t.Fatalf("point INFRA at the URL its daemon moved to: %v", err)
			}
			rig.wake <- struct{}{}
			rig.waitFor(20*time.Second, func() bool { return relocated.served() >= 1 }, func() string {
				return "the write's starting read of the URL the daemon moved to never began"
			})
			return heldRead{relocated, release, rigStateID, rigRun, 12, 0}
		}},
		{"a marked store whose boot finds it", func(t *testing.T, rig *intakeRig) heldRead {
			b := newFakeDaemon(t, "")
			b.refuses(http.StatusServiceUnavailable)
			rig.pointAt(b, 2)
			c := newFakeDaemon(t, daemonStateIn("INFRA", 5, inPhase("implementing"), cStore, cRun))
			rig.pointAt(c, 3)
			rig.publishIn("INFRA", bRun, bStore, time.Now().UTC().Truncate(time.Microsecond), false, change(20, "INFRA-1", moved("phase", "merging")))
			rig.settled()
			rig.waitFor(20*time.Second, func() bool { return slices.Equal(cursorOf(t, rig.database, "INFRA").foreignStateIDs, []string{bStore}) }, func() string {
				return fmt.Sprintf("control: B's store is not marked: INFRA's cursor is %s", cursorOf(t, rig.database, "INFRA"))
			})
			// B takes C's place behind C's URL and boots there; the boot's store read is held, and
			// the boot, which makes it, holds a place on the runner.
			c.serves(daemonStateIn("INFRA", 40, inPhase("implementing"), bStore, bootRun))
			release := c.holds(t)
			before := c.served()
			rig.clock.advance(ReadRetryAfter + time.Second)
			rig.publishIn("INFRA", bootRun, bStore, time.Now().UTC().Truncate(time.Microsecond), false, booted(41, stateSchema, 2))
			rig.waitFor(20*time.Second, func() bool { return c.served() > before }, func() string {
				return "control: the boot of B's store made no store read of C's URL"
			})
			return heldRead{c, release, bStore, bootRun, 42, 1}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newIntakeRig(t)
			daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
			seedLegionProject(t, rig.database, "INFRA", daemon.URL)
			rig.start()
			rig.awaitStartingRead("INFRA")
			h := tc.setup(t, rig)

			// The messages that wait on the runner fill it, and three more arrive past the cap;
			// every one is past the read's snapshot.
			at := time.Now().UTC().Truncate(time.Microsecond)
			seq := h.first
			for range stateMaxWaiting - h.waiting {
				rig.publishIn("INFRA", h.run, h.store, at, false, change(seq, "INFRA-1", moved("phase", "testing")))
				seq++
			}
			rig.waitFor(20*time.Second, func() bool {
				info := rig.consumerInfo()
				return info.NumPending == 0 && info.NumAckPending >= stateMaxWaiting
			}, func() string {
				return fmt.Sprintf("control: INFRA holds %+v, want %d messages waiting on its runner", rig.consumerInfo(), stateMaxWaiting)
			})
			// Near the end of the read's minute on the intake's clock, as in
			// TestAProjectPastTheCapDuringAGoodReadThatGoesQuietIsReadWithinTheMinute, so that a
			// message past the cap that holds a read goes back for the few seconds left.
			rig.clock.advance(ReadRetryAfter - 5*time.Second)
			last := seq + 2
			for i, phase := range []string{"merging", "accepting", "reviewing"} {
				rig.publishIn("INFRA", h.run, h.store, at, false, change(seq+i, "INFRA-1", moved("phase", phase)))
			}
			rig.waitFor(20*time.Second, func() bool { return rig.consumerInfo().NumPending == 0 }, func() string {
				return fmt.Sprintf("control: the messages past the cap were not delivered: %+v", rig.consumerInfo())
			})
			time.Sleep(time.Second)
			// Any read after the one held serves the daemon's newest state.
			h.daemon.serves(daemonStateIn("INFRA", last, inPhase("reviewing"), h.store, h.run))
			h.release()
			rig.waitFor(20*time.Second, func() bool {
				c := cursorOf(t, rig.database, "INFRA")
				return c.stateID != nil && *c.stateID == h.store
			}, func() string {
				return fmt.Sprintf("control: the held read did not verify the store %s: INFRA's cursor is %s", h.store, cursorOf(t, rig.database, "INFRA"))
			})
			rig.clock.advance(6 * time.Second)
			if !testwait.Holds(75*time.Second, 100*time.Millisecond, func() bool {
				return currentValues(t, rig.database, "INFRA")["INFRA-1/phase"] == `"reviewing"`
			}) {
				t.Errorf("75 s after the read that verified the store %s, with INFRA quiet, INFRA-1's phase is %s and INFRA's cursor is %s: the events past the cap, published while that read was in flight, were dropped as a store's that is no daemon of INFRA, and nothing owes the read that would take them",
					h.store, currentValues(t, rig.database, "INFRA")["INFRA-1/phase"], cursorOf(t, rig.database, "INFRA"))
			}
		})
	}
}

// TestABootOfTheOldStoreWhileTheNewURLIsUnverifiedHoldsNoRead: judge's boot rule (rows 3.5 and
// 5.7) calls for a store read of a boot of a store the cursor drops only while the cursor follows
// the setting's URL in a store a read verified. While the URL a write named is unverified, the
// cursor holds no store, and a boot of the store the setting named before is a message of that
// store like any other (rows 4.3 and 4.4): it is finished unapplied, logged as dropped, holds no
// read, and calls for the owed read only where it is due. Each subtest writes INFRA's setting
// naming a URL whose reads all fail, and then INFRA's daemon named before restarts and publishes a
// boot of its store, once before the owed read is due and once after.
func TestABootOfTheOldStoreWhileTheNewURLIsUnverifiedHoldsNoRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		due  bool
		// served is how many reads the URL the write named serves: the write's failed starting
		// read, and the owed read where it is due.
		served int
	}{
		{"before the owed read is due", false, 1},
		{"once the owed read is due", true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unverified := newFakeDaemon(t, "")
			unverified.refuses(http.StatusServiceUnavailable)
			rig := newIntakeRig(t)
			daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
			seedLegionProject(t, rig.database, "INFRA", daemon.URL)
			rig.start()
			rig.awaitStartingRead("INFRA")
			if _, err := PutSettings(t.Context(), rig.database.Pool, "INFRA", unverified.URL, model.Actor{Kind: "user", ID: "alice@example.com"}); err != nil {
				t.Fatalf("point INFRA at a URL whose reads fail: %v", err)
			}
			rig.wake <- struct{}{}
			rig.awaitWholeReads(2)
			if tc.due {
				rig.clock.advance(ReadRetryAfter + time.Second)
			}
			rig.publish("INFRA", booted(11, stateSchema, 2))
			if drops := rig.awaitLogRecords(forgottenLine, 1); len(drops) != 1 || drops[0]["stateId"] != rigStateID {
				t.Errorf("the drops logged = %v, want one, of the boot of the store %s the setting named before", drops, rigStateID)
			}
			if testwait.Holds(2*time.Second, 25*time.Millisecond, func() bool { return unverified.served() > tc.served }) {
				t.Errorf("the URL the write named served %d reads, want %d: the boot of the store the setting named before made a read of its own", unverified.served(), tc.served)
			}
			if c := cursorOf(t, rig.database, "INFRA"); c.readHolder != nil || c.readHolds != 0 || c.stateID != nil || !slices.Equal(c.readReasons, []string{ReadReasonRetry}) {
				t.Errorf("INFRA's cursor is %s, want the read owed for retry alone, no holder and no store: the boot of the store the setting named before holds the read", c)
			}
			if held := rig.logRecords(heldLine); len(held) != 0 {
				t.Errorf("the messages sent back = %v, want none: no message of the store the setting named before holds the read", held)
			}
		})
	}
}

// TestEventsPastTheCapBehindAHeldMessageOfADroppedStoreAreAppliedWhenTheReadFindsAnotherStore: R3
// keeps a project that goes quiet at most one minute behind while its reads succeed. Row 8.1 has a
// message of a store the cursor drops, past the cap while a whole read of the project is in
// flight, owe the gap read as any message past the cap does, and the first message past the cap
// holds that read. Where the read in flight does not find that store, the holder's redelivery is
// judged as the holder (row 8.2): it makes the read it holds once that read is due, and the
// setting's daemon's message that arrived past the cap after it and joined that read is taken
// from the read's snapshot. Were the holder acknowledged with no read made, nothing would take
// that message's events while the project stays quiet.
//
// INFRA's daemon is read whole in the rig's store, and a write of INFRA's setting names another
// daemon, in a store of its own, whose starting read is held. The daemon named before publishes
// stateMaxWaiting messages, which wait on the runner, and one more past the cap; then the daemon
// the setting names publishes one message past the cap, moving INFRA-1 to reviewing, past the
// held read's snapshot. The read is let go and verifies the new daemon's store. INFRA then goes
// quiet; a minute on, on the intake's clock, INFRA-1's phase reaches reviewing within 75 s.
//
// At b4600876 the old daemon's message past the cap holds the gap read and the new daemon's
// message joins it and is acknowledged unapplied; the holder's redelivery is acknowledged as a
// message of a dropped store and makes no read, so INFRA-1 stays at the read's implementing. At
// d1e22ece the old daemon's message past the cap owes nothing, the new daemon's message holds the
// read, and its redelivery makes it.
func TestEventsPastTheCapBehindAHeldMessageOfADroppedStoreAreAppliedWhenTheReadFindsAnotherStore(t *testing.T) {
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")

	named := newFakeDaemon(t, daemonStateIn("INFRA", 5, inPhase("implementing"), bStore, bRun))
	release := named.holds(t)
	if _, err := PutSettings(t.Context(), rig.database.Pool, "INFRA", named.URL, model.Actor{Kind: "user", ID: "alice@example.com"}); err != nil {
		t.Fatalf("point INFRA at another daemon: %v", err)
	}
	rig.wake <- struct{}{}
	rig.waitFor(20*time.Second, func() bool { return named.served() >= 1 }, func() string {
		return "the write's starting read of the daemon it names never began"
	})

	// The daemon named before fills the runner behind the held read, and one more of its messages
	// arrives past the cap, near the end of the read's minute on the intake's clock.
	for seq := 11; seq < 11+stateMaxWaiting; seq++ {
		rig.publish("INFRA", change(seq, "INFRA-1", moved("phase", "testing")))
	}
	rig.waitFor(20*time.Second, func() bool {
		info := rig.consumerInfo()
		return info.NumPending == 0 && info.NumAckPending >= stateMaxWaiting
	}, func() string {
		return fmt.Sprintf("control: INFRA holds %+v, want %d messages waiting on its runner", rig.consumerInfo(), stateMaxWaiting)
	})
	rig.clock.advance(ReadRetryAfter - 5*time.Second)
	rig.publish("INFRA", change(11+stateMaxWaiting, "INFRA-1", moved("phase", "merging")))
	rig.waitFor(20*time.Second, func() bool { return rig.consumerInfo().NumPending == 0 }, func() string {
		return fmt.Sprintf("control: the old daemon's message past the cap was not delivered: %+v", rig.consumerInfo())
	})

	// The daemon the setting names commits INFRA-1's move to reviewing after the held read's
	// snapshot, and publishes it past the cap.
	rig.publishIn("INFRA", bRun, bStore, time.Now().UTC().Truncate(time.Microsecond), false, change(6, "INFRA-1", moved("phase", "reviewing")))
	rig.waitFor(20*time.Second, func() bool { return rig.consumerInfo().NumPending == 0 }, func() string {
		return fmt.Sprintf("control: the new daemon's message past the cap was not delivered: %+v", rig.consumerInfo())
	})
	time.Sleep(time.Second)
	named.serves(daemonStateIn("INFRA", 6, inPhase("reviewing"), bStore, bRun))
	release()
	rig.waitFor(20*time.Second, func() bool {
		c := cursorOf(t, rig.database, "INFRA")
		return c.stateID != nil && *c.stateID == bStore
	}, func() string {
		return fmt.Sprintf("control: the held read did not verify the store %s: INFRA's cursor is %s", bStore, cursorOf(t, rig.database, "INFRA"))
	})
	rig.clock.advance(6 * time.Second)
	if !testwait.Holds(75*time.Second, 100*time.Millisecond, func() bool {
		return currentValues(t, rig.database, "INFRA")["INFRA-1/phase"] == `"reviewing"`
	}) {
		t.Errorf("75 s after the read that verified the store %s, with INFRA quiet, INFRA-1's phase is %s, INFRA's cursor is %s and the daemon it names served %d reads: its event past the cap joined the gap read that a message of the store the setting named before held, and that message's redelivery made no read",
			bStore, currentValues(t, rig.database, "INFRA")["INFRA-1/phase"], cursorOf(t, rig.database, "INFRA"), named.served())
	}
}

// TestEventsPastTheCapDuringAnOwedGapReadAreAppliedOnceTheProjectGoesQuiet: R3 keeps a project
// that goes quiet at most one minute behind while its reads succeed, and row 8.1 has a message past
// the cap owe the read its position calls for, "which the read in flight does not clear, since its
// events may be past that read's snapshot". The read in flight here is a gap read the cursor owed
// before it began: its holder's redelivery makes it, so the cursor still owes gap while it is in
// flight. A message past the cap that calls for gap then adds no reason (cursor.readFor adds a
// reason only where the cursor does not owe it), and the read's record clears the reasons owed
// when it was decided (recordWhole, owedSince), gap among them, and with them the holder. So
// nothing owes the read that would take the events past its snapshot.
//
// INFRA's daemon serves 15 and INFRA's message at 12 makes a gap read of it. Near the end of that
// read's minute, the message at 17 calls for a gap read that is not due: the cursor owes it and the
// message holds it. Its redelivery, once the minute is out, makes that read, which the daemon
// holds; the read serves 40. Meanwhile INFRA publishes messages at or below 40, which wait on the
// runner behind the read, and 41 to 43, past the read's snapshot and past the cap, the last moving
// INFRA-1 to reviewing. The read is let go, INFRA goes quiet, and a minute on, on the intake's
// clock, INFRA-1 reaches reviewing within 75 s. At b4600876, and at d1e22ece, INFRA stays at 40's
// testing with nothing owed.
func TestEventsPastTheCapDuringAnOwedGapReadAreAppliedOnceTheProjectGoesQuiet(t *testing.T) {
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")

	// A gap read made at once, which starts the minute the next one waits after.
	daemon.serves(daemonState("INFRA", 15, inPhase("implementing")))
	rig.publish("INFRA", change(12, "INFRA-1", moved("phase", "implementing")))
	rig.waitFor(20*time.Second, func() bool {
		c := cursorOf(t, rig.database, "INFRA")
		return c.lastSeq != nil && *c.lastSeq == 15
	}, func() string {
		return fmt.Sprintf("control: the gap read at 12 did not land at 15: INFRA's cursor is %s", cursorOf(t, rig.database, "INFRA"))
	})

	// Near the end of that minute, a gap the cursor owes and the message at 17 holds.
	rig.clock.advance(ReadRetryAfter - 3*time.Second)
	release := daemon.holds(t)
	daemon.serves(daemonState("INFRA", 40, inPhase("testing")))
	rig.publish("INFRA", change(17, "INFRA-1", moved("phase", "testing")))
	rig.waitFor(20*time.Second, func() bool {
		return slices.Equal(cursorOf(t, rig.database, "INFRA").readReasons, []string{ReadReasonGap})
	}, func() string {
		return fmt.Sprintf("control: INFRA's cursor does not owe the gap read: %s", cursorOf(t, rig.database, "INFRA"))
	})
	rig.clock.advance(5 * time.Second)
	rig.waitFor(30*time.Second, func() bool { return daemon.served() >= 3 }, func() string {
		return fmt.Sprintf("control: the held message at 17 never made the gap read it owed: INFRA's cursor is %s", cursorOf(t, rig.database, "INFRA"))
	})
	if c := cursorOf(t, rig.database, "INFRA"); !slices.Equal(c.readReasons, []string{ReadReasonGap}) {
		t.Fatalf("control: INFRA's cursor is %s while the owed gap read is in flight, want gap owed", c)
	}

	// Behind the read in flight: messages at or below its snapshot fill the runner, and three more
	// arrive past the cap and past the snapshot, near the end of the read's minute.
	daemon.serves(daemonState("INFRA", 43, inPhase("reviewing")))
	at := time.Now().UTC().Truncate(time.Microsecond)
	for seq := 18; seq < 18+stateMaxWaiting-1; seq++ {
		rig.publishIn("INFRA", rigRun, rigStateID, at, false, change(seq, "INFRA-1", moved("phase", "testing")))
	}
	rig.waitFor(20*time.Second, func() bool {
		info := rig.consumerInfo()
		return info.NumPending == 0 && info.NumAckPending >= stateMaxWaiting
	}, func() string {
		return fmt.Sprintf("control: INFRA holds %+v, want %d messages waiting on its runner", rig.consumerInfo(), stateMaxWaiting)
	})
	rig.clock.advance(ReadRetryAfter - 5*time.Second)
	for i, phase := range []string{"merging", "accepting", "reviewing"} {
		rig.publishIn("INFRA", rigRun, rigStateID, at, false, change(41+i, "INFRA-1", moved("phase", phase)))
	}
	rig.waitFor(20*time.Second, func() bool { return rig.consumerInfo().NumPending == 0 }, func() string {
		return fmt.Sprintf("control: the messages past the cap were not delivered: %+v", rig.consumerInfo())
	})
	time.Sleep(time.Second)
	release()
	rig.waitFor(20*time.Second, func() bool {
		c := cursorOf(t, rig.database, "INFRA")
		return c.lastSeq != nil && *c.lastSeq >= 40
	}, func() string {
		return fmt.Sprintf("control: the owed gap read did not land at 40: INFRA's cursor is %s", cursorOf(t, rig.database, "INFRA"))
	})
	rig.clock.advance(6 * time.Second)
	if !testwait.Holds(75*time.Second, 100*time.Millisecond, func() bool {
		return currentValues(t, rig.database, "INFRA")["INFRA-1/phase"] == `"reviewing"`
	}) {
		t.Errorf("75 s after the owed gap read at 40, with INFRA quiet, INFRA-1's phase is %s, INFRA's cursor is %s and the daemon served %d reads: the gap the messages past the cap owed was folded into the read in flight, whose record cleared it",
			currentValues(t, rig.database, "INFRA")["INFRA-1/phase"], cursorOf(t, rig.database, "INFRA"), daemon.served())
	}
}

// TestMessagesPastTheCapDuringAnOwedGapReadHoldOneMessageOfTheWindow: R3 has each owing project
// hold one message of the consumer's window, and row 8.1 has the first message past the cap hold
// the read it owes while each later one joins it and is acknowledged. Where the read in flight is
// a gap read the cursor owed before it began, the message whose redelivery makes it holds it. The
// first message past the cap since that read began takes the hold of the read after it, and every
// later one joins that hold (judgeCrowded, inFlight). So while the read is in flight, INFRA holds
// the messages that wait on its runner and that first message; once the read lands short of the
// messages past the cap, the gap read stays owed, that first message still holds it, and the
// message that made the read is acknowledged, so INFRA holds that one message.
//
// The setup is TestEventsPastTheCapDuringAnOwedGapReadAreAppliedOnceTheProjectGoesQuiet's, with
// 20 s of the read's minute left when 41 to 43 arrive past the cap, so the first of them is still
// held when the test looks. At 23a92974 41 to 43 all join the hold of the message at 17 and are
// acknowledged, and the read's record clears the gap and the holder. A rule that has every message
// past the cap during a read in flight take the hold sends 41, 42 and 43 back, three messages of
// the window.
func TestMessagesPastTheCapDuringAnOwedGapReadHoldOneMessageOfTheWindow(t *testing.T) {
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")

	// A gap read at 12 starts the minute; near its end the message at 17 owes and holds a gap read,
	// and its redelivery makes that read, which the daemon holds at 40.
	daemon.serves(daemonState("INFRA", 15, inPhase("implementing")))
	rig.publish("INFRA", change(12, "INFRA-1", moved("phase", "implementing")))
	rig.waitFor(20*time.Second, func() bool {
		c := cursorOf(t, rig.database, "INFRA")
		return c.lastSeq != nil && *c.lastSeq == 15
	}, func() string {
		return fmt.Sprintf("control: the gap read at 12 did not land at 15: INFRA's cursor is %s", cursorOf(t, rig.database, "INFRA"))
	})
	rig.clock.advance(ReadRetryAfter - 3*time.Second)
	release := daemon.holds(t)
	daemon.serves(daemonState("INFRA", 40, inPhase("testing")))
	rig.publish("INFRA", change(17, "INFRA-1", moved("phase", "testing")))
	maker := rig.lastStreamSeq()
	rig.waitFor(20*time.Second, func() bool {
		c := cursorOf(t, rig.database, "INFRA")
		return c.readHolder != nil && *c.readHolder == int64(maker) && slices.Equal(c.readReasons, []string{ReadReasonGap})
	}, func() string {
		return fmt.Sprintf("control: the message at 17 does not hold the owed gap read: INFRA's cursor is %s", cursorOf(t, rig.database, "INFRA"))
	})
	rig.clock.advance(5 * time.Second)
	rig.waitFor(30*time.Second, func() bool { return daemon.served() >= 3 }, func() string {
		return fmt.Sprintf("control: the held message at 17 never made the gap read it owed: INFRA's cursor is %s", cursorOf(t, rig.database, "INFRA"))
	})

	// Messages at or below 40 fill the runner, and 41 to 43 arrive past the cap.
	daemon.serves(daemonState("INFRA", 43, inPhase("reviewing")))
	at := time.Now().UTC().Truncate(time.Microsecond)
	for seq := 18; seq < 18+stateMaxWaiting-1; seq++ {
		rig.publishIn("INFRA", rigRun, rigStateID, at, false, change(seq, "INFRA-1", moved("phase", "testing")))
	}
	rig.waitFor(20*time.Second, func() bool {
		info := rig.consumerInfo()
		return info.NumPending == 0 && info.NumAckPending >= stateMaxWaiting
	}, func() string {
		return fmt.Sprintf("control: INFRA holds %+v, want %d messages waiting on its runner", rig.consumerInfo(), stateMaxWaiting)
	})
	rig.clock.advance(ReadRetryAfter - 20*time.Second)
	var first uint64
	for i, phase := range []string{"merging", "accepting", "reviewing"} {
		rig.publishIn("INFRA", rigRun, rigStateID, at, false, change(41+i, "INFRA-1", moved("phase", phase)))
		if i == 0 {
			first = rig.lastStreamSeq()
		}
	}
	rig.waitFor(20*time.Second, func() bool { return rig.consumerInfo().NumPending == 0 }, func() string {
		return fmt.Sprintf("control: the messages past the cap were not delivered: %+v", rig.consumerInfo())
	})
	time.Sleep(time.Second)
	if held := rig.consumerInfo().NumAckPending; held != stateMaxWaiting+1 {
		t.Errorf("INFRA holds %d messages of the window while its owed gap read is in flight, want %d: %d waiting on its runner and the first message past the cap",
			held, stateMaxWaiting+1, stateMaxWaiting)
	}
	if handBacks := rig.handBacksOf(); len(handBacks) != 2 || handBacks[maker] != 1 || handBacks[first] != 1 {
		t.Errorf("the hand-backs by stream sequence are %v, want the message at 17 (%d) once, before it made the read, and the first message past the cap (%d) once", handBacks, maker, first)
	}

	release()
	rig.waitFor(20*time.Second, func() bool {
		c := cursorOf(t, rig.database, "INFRA")
		return c.lastSeq != nil && *c.lastSeq >= 40
	}, func() string {
		return fmt.Sprintf("control: the owed gap read did not land at 40: INFRA's cursor is %s", cursorOf(t, rig.database, "INFRA"))
	})
	if !testwait.Holds(20*time.Second, 25*time.Millisecond, func() bool { return rig.consumerInfo().NumAckPending == 1 }) {
		t.Errorf("INFRA holds %d messages of the window after the read at 40, want the first message past the cap alone", rig.consumerInfo().NumAckPending)
	}
	if c := cursorOf(t, rig.database, "INFRA"); !slices.Equal(c.readReasons, []string{ReadReasonGap}) || c.readHolder == nil ||
		*c.readHolder != int64(first) || c.readHolds != 1 {
		t.Errorf("INFRA's cursor after the read at 40 is %s, want the gap read owed and held by the first message past the cap (%d), gone back once", c, first)
	}
	if handBacks := rig.handBacksOf(); handBacks[maker] != 1 {
		t.Errorf("the message at 17 went back %d times, want once: once its read lands it holds nothing", handBacks[maker])
	}
}

// TestAMessageOfANewStorePastTheCapDuringAReadOfTheOldStoreIsFound: R3 keeps a project that goes
// quiet at most one minute behind while its reads succeed, and a read covers only the events of its
// own store at or below the stateSeq it serves. INFRA's gap read of the rig's store is in flight,
// held by its daemon, and serves 30. Then the daemon behind INFRA's URL is redeployed in a store of
// its own (no write of the setting). Behind the read, 15 messages of the rig's store at or below 30
// fill the runner with the gap message; one more of the rig's store at 29 arrives past the cap and
// holds the next read; then the new store's boot arrives past the cap, and is acknowledged
// unapplied. The read lands with the rig's store at 30. INFRA then goes quiet: within 75 s, a minute
// on, on the intake's clock, INFRA's cursor follows the new store (R3-a: "a message of another store
// calls for a read, so a daemon whose store was replaced is found again").
//
// At bc057cc2 the read's record checks the skipped record for its own store only
// (cursor.skippedAbove), so the new store's boot owes nothing, the holder goes, and INFRA stays on
// the rig's store at 30 with nothing owed. At 23a92974 the boot's reason stayed owed and the next
// read found the new store.
func TestAMessageOfANewStorePastTheCapDuringAReadOfTheOldStoreIsFound(t *testing.T) {
	const newRun, newStore = "e4b81c2d-7a9f-4e36-b0d5-3c8a1f6e9b27", "f8d2a6c1-3e7b-4c95-a0f4-6b9e2d8c1a53"
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")

	release := daemon.holds(t)
	daemon.serves(daemonState("INFRA", 30, inPhase("implementing")))
	rig.publish("INFRA", change(12, "INFRA-1", moved("phase", "implementing")))
	rig.waitFor(20*time.Second, func() bool { return daemon.served() >= 2 }, func() string {
		return "INFRA's gap read never began"
	})
	// The redeploy: every later read serves the new store.
	daemon.serves(daemonStateIn("INFRA", 41, inPhase("reviewing"), newStore, newRun))
	rig.clock.advance(ReadRetryAfter - 5*time.Second)
	for seq := 13; seq < 13+stateMaxWaiting-1; seq++ {
		rig.publish("INFRA", change(seq, "INFRA-1", moved("phase", "implementing")))
	}
	rig.waitFor(20*time.Second, func() bool {
		info := rig.consumerInfo()
		return info.NumPending == 0 && info.NumAckPending >= stateMaxWaiting
	}, func() string {
		return fmt.Sprintf("control: INFRA holds %+v, want %d messages waiting on its runner", rig.consumerInfo(), stateMaxWaiting)
	})
	rig.publish("INFRA", change(29, "INFRA-1", moved("phase", "implementing")))
	rig.waitFor(20*time.Second, func() bool { return rig.consumerInfo().NumPending == 0 }, func() string {
		return fmt.Sprintf("control: the message past the cap was not delivered: %+v", rig.consumerInfo())
	})
	rig.publishIn("INFRA", newRun, newStore, time.Now().UTC().Truncate(time.Microsecond), false, booted(41, stateSchema, 2))
	rig.waitFor(20*time.Second, func() bool { return rig.consumerInfo().NumPending == 0 }, func() string {
		return fmt.Sprintf("control: the new store's boot was not delivered: %+v", rig.consumerInfo())
	})
	time.Sleep(time.Second)
	release()
	rig.waitFor(20*time.Second, func() bool {
		c := cursorOf(t, rig.database, "INFRA")
		return c.lastSeq != nil && *c.lastSeq >= 30
	}, func() string {
		return fmt.Sprintf("control: the read did not land at 30: INFRA's cursor is %s", cursorOf(t, rig.database, "INFRA"))
	})
	t.Logf("after the read: INFRA's cursor is %s", cursorOf(t, rig.database, "INFRA"))
	rig.clock.advance(6 * time.Second)
	if !testwait.Holds(75*time.Second, 100*time.Millisecond, func() bool {
		c := cursorOf(t, rig.database, "INFRA")
		return c.stateID != nil && *c.stateID == newStore
	}) {
		t.Errorf("75 s after the read of the rig's store at 30, with INFRA quiet, INFRA's cursor is %s and the daemon served %d reads, want the store %s the daemon behind INFRA's URL now serves: the new store's boot past the cap was acknowledged unapplied and the read's record owed nothing for it",
			cursorOf(t, rig.database, "INFRA"), daemon.served(), newStore)
	}
}
