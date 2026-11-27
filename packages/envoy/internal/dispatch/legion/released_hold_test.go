package legion

import (
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The runs and stores of the daemons the tests name B and C, besides INFRA's first.
const (
	bRun, bStore = "3c9e5a71-8b2d-4f06-a4e1-7d0b6c2f9a38", "9a4f2c6e-1d7b-4e38-b5a0-2e8c7f1d6b49"
	cRun, cStore = "6f1b8d2a-4e7c-4a59-9d3b-0c5e2a7f8b16", "1e7c3b9d-5a2f-4c86-8e0b-4d6a1f9c2e73"
)

// windowed has every Client wait readWindow at the most for a daemon's answer, as the rig's client
// window, until the test ends: every Client shares the default transport.
func windowed(t *testing.T, readWindow time.Duration) {
	t.Helper()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = readWindow
	shared := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() {
		http.DefaultTransport = shared
		transport.CloseIdleConnections()
	})
}

// finishedThrough reports whether the intake has acknowledged every message through stream sequence
// seq, and holds none.
func (r *intakeRig) finishedThrough(seq uint64) func() bool {
	return func() bool {
		info := r.consumerInfo()
		return info.AckFloor.Stream >= seq && info.NumAckPending == 0 && info.NumPending == 0
	}
}

// handBacksOf is how many times each message went back to the stream holding its project's owed
// read, by its stream sequence (heldLine).
func (r *intakeRig) handBacksOf() map[uint64]int {
	handBacks := map[uint64]int{}
	for _, line := range r.logRecords(heldLine) {
		if seq, named := line["streamSeq"].(float64); named {
			handBacks[uint64(seq)]++
		}
	}
	return handBacks
}

// TestEachWriteOfAnotherDaemonEndsTheHoldOfTheMessageBeforeIt: R3 applies no message of a daemon the
// setting named before, and a message holds the durable's ack floor for about 11 minutes at the
// most. A write of the setting that names another daemon ends the hold of the message that holds
// the project's owed read, whatever store that message is of (released_holders): the message is
// finished unapplied at its next delivery, holds no read and makes none, and endHold says whether
// it marks the message's store as no daemon of the project. Here it marks each held message's
// store, which is neither the cursor's (no read of B or C is good) nor INFRA's first, the store
// the write recorded. INFRA's setting names B, which never answers: each read of it waits out
// the client's window, readWindow, and moves the intake's clock RequestTimeout on, so a read that
// fails is due again at once. B's own message holds the owed read and goes back to the stream after
// each read it makes. Then the setting names C, which never answers either, and C's own message
// holds the read in its turn; then the setting names D. Each held message is finished within
// readWindow and a second of the write after it: its next delivery waits on the project's runner
// behind the write's starting read at the most. No message goes back to the stream more than
// stateMaxHolds times.
func TestEachWriteOfAnotherDaemonEndsTheHoldOfTheMessageBeforeIt(t *testing.T) {
	const readWindow = 3 * time.Second
	windowed(t, readWindow)
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")
	stall := func() { rig.clock.advance(RequestTimeout) }

	// hold waits until named has served a read and INFRA's cursor owes one, as named's failed
	// starting read leaves it, then publishes a message of named's in store under run, waits for
	// that message to go back to the stream holding the read, and answers its stream sequence.
	hold := func(name string, named *fakeDaemon, run, store string) uint64 {
		t.Helper()
		rig.waitFor(time.Minute, func() bool { return named.served() >= 1 && cursorOf(t, rig.database, "INFRA").readOwed }, func() string {
			return fmt.Sprintf("the starting read of %s has not failed: INFRA's cursor is %s", name, cursorOf(t, rig.database, "INFRA"))
		})
		rig.publishIn("INFRA", run, store, time.Now().UTC().Truncate(time.Microsecond), false, change(1, "INFRA-1", moved("phase", "merging")))
		held := rig.lastStreamSeq()
		rig.waitFor(time.Minute, func() bool { return rig.handBacksOf()[held] >= 1 }, func() string {
			return fmt.Sprintf("%s's message never went back to the stream holding INFRA's read: %v", name, rig.logRecords(heldLine))
		})
		return held
	}
	// write names named in INFRA's setting, waits for held to be finished, and fails unless that
	// took less than readWindow and a second, held went back to the stream at most once after the
	// write (the hand-back in flight at the write), and the cursor marks exactly the stores marks
	// as no daemon of INFRA and records no released holder.
	write := func(name string, named *fakeDaemon, held uint64, marks []string) {
		t.Helper()
		atWrite := rig.handBacksOf()[held]
		wroteAt := time.Now()
		if _, err := PutSettings(t.Context(), rig.database.Pool, "INFRA", named.URL, model.Actor{Kind: "user", ID: "alice@example.com"}); err != nil {
			t.Fatalf("point INFRA at %s: %v", name, err)
		}
		rig.wake <- struct{}{}
		rig.waitFor(time.Minute, rig.finishedThrough(held), func() string {
			return fmt.Sprintf("the held message at %d is not finished a minute after the write naming %s: %+v", held, name, rig.consumerInfo())
		})
		if took := time.Since(wroteAt); took >= readWindow+time.Second {
			t.Errorf("the held message at %d was finished %s after the write naming %s, want less than %s, the write's starting read of %s and a second",
				held, took.Round(time.Millisecond), name, readWindow+time.Second, readWindow)
		}
		if after := rig.handBacksOf()[held] - atWrite; after > 1 {
			t.Errorf("the held message at %d went back %d more times after the write naming %s, want at most the one in flight at the write", held, after, name)
		}
		if c := cursorOf(t, rig.database, "INFRA"); !slices.Equal(c.foreignStateIDs, marks) || len(c.releasedHolders) != 0 {
			t.Errorf("INFRA's cursor after the write naming %s is %s, want the stores %v marked as no daemon of INFRA and no message to release", name, c, marks)
		}
	}

	b := newFakeDaemon(t, "")
	b.stalls(stall)
	if _, err := PutSettings(t.Context(), rig.database.Pool, "INFRA", b.URL, model.Actor{Kind: "user", ID: "alice@example.com"}); err != nil {
		t.Fatalf("point INFRA at B: %v", err)
	}
	rig.wake <- struct{}{}
	heldB := hold("B", b, bRun, bStore)

	c := newFakeDaemon(t, "")
	c.stalls(stall)
	write("C", c, heldB, []string{bStore})
	heldC := hold("C", c, cRun, cStore)

	d := newFakeDaemon(t, "")
	d.stalls(stall)
	write("D", d, heldC, []string{cStore})

	for seq, handBacks := range rig.handBacksOf() {
		if handBacks > stateMaxHolds {
			t.Errorf("the message at %d went back %d times, want %d at the most", seq, handBacks, stateMaxHolds)
		}
	}
	released := rig.awaitLogRecords(writeReleasedLine, 2)
	if len(released) != 2 || released[0]["streamSeq"] != float64(heldB) || released[0]["stateId"] != bStore ||
		released[1]["streamSeq"] != float64(heldC) || released[1]["stateId"] != cStore || released[0]["level"] != "INFO" {
		t.Errorf("the releases logged = %v, want B's message at %d, then C's at %d, each at INFO with its store", released, heldB, heldC)
	}
	if got := history(t, rig.database, "INFRA", "INFRA-1", FieldPhase); slices.Contains(got, `"merging"`) {
		t.Errorf("the phase's history is %v: a held message was applied", got)
	}
}

// TestAHeldMessageOfTheStoreASettingNamedBeforeWritesNothingWhenTheNewURLServesThatStore: R3
// applies every event of the daemon its setting names, past the read's snapshot. INFRA's daemon
// stops answering after its good starting read at 10: each read of it waits out the client's
// window and moves the intake's clock RequestTimeout on. A gap's message at 12 holds the owed read.
// The setting then names a second URL that serves INFRA's own store at 13: the daemon moved. The
// write ends the message's hold (released_holders), and the write's starting read of the second URL
// is good, gives the cursor the store and clears its record (previous_state_id). The held message
// is judged at its next delivery: it is finished unapplied, writes nothing at or below the read's
// 13, and its store, the cursor's own, is not marked as no daemon of INFRA.
func TestAHeldMessageOfTheStoreASettingNamedBeforeWritesNothingWhenTheNewURLServesThatStore(t *testing.T) {
	windowed(t, 3*time.Second)
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")

	daemon.stalls(func() { rig.clock.advance(RequestTimeout) })
	rig.publish("INFRA", change(12, "INFRA-1", moved("phase", "merging")))
	holder := rig.lastStreamSeq()
	rig.waitFor(time.Minute, func() bool { return rig.handBacksOf()[holder] >= 1 }, func() string {
		return fmt.Sprintf("the gap's message never went back to the stream holding INFRA's read: %v", rig.logRecords(heldLine))
	})

	relocated := newFakeDaemon(t, daemonState("INFRA", 13, inPhase("implementing")))
	if _, err := PutSettings(t.Context(), rig.database.Pool, "INFRA", relocated.URL, model.Actor{Kind: "user", ID: "alice@example.com"}); err != nil {
		t.Fatalf("point INFRA at the URL its daemon moved to: %v", err)
	}
	rig.wake <- struct{}{}
	rig.waitFor(time.Minute, rig.finishedThrough(holder), func() string {
		return fmt.Sprintf("the held message is not finished a minute after the write: %+v", rig.consumerInfo())
	})
	rig.waitFor(20*time.Second, func() bool {
		c := cursorOf(t, rig.database, "INFRA")
		return c.lastSeq != nil && *c.lastSeq == 13
	}, func() string {
		return fmt.Sprintf("the write's starting read of the URL the daemon moved to is not recorded: INFRA's cursor is %s", cursorOf(t, rig.database, "INFRA"))
	})
	if c := cursorOf(t, rig.database, "INFRA"); c.stateID == nil || *c.stateID != rigStateID || c.previousStateID != nil || len(c.foreignStateIDs) != 0 ||
		len(c.releasedHolders) != 0 || c.readOwed || c.readHolder != nil {
		t.Errorf("INFRA's cursor is %s, want it at the read's 13 in the store %s, no store recorded or marked, no message to release, nothing owed and no holder", c, rigStateID)
	}
	if got := history(t, rig.database, "INFRA", "INFRA-1", FieldPhase); slices.Contains(got, `"merging"`) || !slices.Contains(got, `"implementing"`) {
		t.Errorf("the phase's history is %v, want the read's implementing and not the held message's merging", got)
	}
	released := rig.awaitLogRecords(writeReleasedLine, 1)
	if len(released) != 1 || released[0]["streamSeq"] != float64(holder) || released[0]["stateId"] != rigStateID {
		t.Errorf("the releases logged = %v, want one naming the held message at %d and the store %s", released, holder, rigStateID)
	}
	if served := relocated.served(); served != 1 {
		t.Errorf("the URL the daemon moved to served %d reads, want the write's starting read alone", served)
	}
}

// TestASecondWriteOfAnotherDaemonKeepsTheReleaseOfAMessageStillOut: R3-i bounds how long one
// message holds the durable's ack floor at stateMaxHolds hand-backs, about 11 minutes, and a write
// of the setting that names another daemon ends the hold of the message that holds the project's
// owed read: the cursor records that message as released until it comes back
// (released_holders), and at its next delivery it is finished unapplied, holds no read and makes
// none. INFRA's setting names A, which refuses every read, so no read gives the cursor a store and
// no write records one. A's message H1 holds the read that A's failed starting read left owed,
// which is not due yet. A write naming B ends H1's hold while H1 waits out its hand-back. B's
// message H2 then holds B's starting read, which is not due either, and a write naming C ends
// H2's hold before H1 has come back. The cursor records both, H1 first. At their next deliveries
// both are finished unapplied, each logged once with its own store (writeReleasedLine), neither
// goes back to the stream again or makes a read, and the record is empty after. C's starting read
// is made once, by the call that a write's wake deferred to the end of the minute, and no message
// of A or B is applied.
//
// H1 is still out when the write naming C lands by construction, not by a race. The test sets the
// intake's clock left before A's next read is due, so H1 goes back to the stream for about left,
// the time until that read is due, and the two writes and H2's hold come well inside it. B's
// starting read is due at that same moment, since whole reads of one project start ReadRetryAfter
// apart whatever the setting names. So H1, H2 and the deferred call all come back at about the end
// of left, in no fixed order, and the test asserts no order among them.
//
// The test catches a cursor that keeps one released holder: the write naming C records H2 over H1,
// and H1's next delivery is judged as any message's. At 8a18f054 the cursor kept one
// (released_holder): the write naming C recorded H2 over H1, so H1 was not released and was judged
// as any message of another store, making C's starting read or holding a store read of C from a
// count of 1.
func TestASecondWriteOfAnotherDaemonKeepsTheReleaseOfAMessageStillOut(t *testing.T) {
	const aRun, aStore = "d5a8c3e1-9f2b-4d76-8a0e-3b7c1f6d2e94", "2f9d6b3a-8e1c-4a57-b6d0-9c4e7a1f3b82"
	// left is how long before A's next read is due H1 goes back to the stream.
	const left = 15 * time.Second
	rig := newIntakeRig(t)
	a := newFakeDaemon(t, "")
	a.refuses(http.StatusServiceUnavailable)
	seedLegionProject(t, rig.database, "INFRA", a.URL)
	rig.start()
	rig.awaitWholeReads(1)
	requireWholeRead(t, wholeReadsOf(t, rig.database, "INFRA")[0], ReadReasonStart, false)
	due := lastReadStartedAt(t, rig, "INFRA").Add(ReadRetryAfter)
	rig.clock.advance(due.Add(-left).Sub(rig.clock.now()))

	// hold has the daemon named name publish a message of store under run, waits for that message
	// to go back to the stream holding INFRA's owed read, and answers its stream sequence.
	hold := func(name, run, store string) uint64 {
		t.Helper()
		rig.publishIn("INFRA", run, store, time.Now().UTC().Truncate(time.Microsecond), false, change(1, "INFRA-1", moved("phase", "merging")))
		held := rig.lastStreamSeq()
		rig.waitFor(time.Minute, func() bool { return rig.handBacksOf()[held] >= 1 }, func() string {
			return fmt.Sprintf("%s's message never went back to the stream holding INFRA's read: %v", name, rig.logRecords(heldLine))
		})
		return held
	}
	// write names named in INFRA's setting, with the write's wake, and answers the messages that
	// INFRA's cursor records as released after the write.
	write := func(name string, named *fakeDaemon) []int64 {
		t.Helper()
		if _, err := PutSettings(t.Context(), rig.database.Pool, "INFRA", named.URL, model.Actor{Kind: "user", ID: "alice@example.com"}); err != nil {
			t.Fatalf("point INFRA at %s: %v", name, err)
		}
		released := cursorOf(t, rig.database, "INFRA").releasedHolders
		rig.wake <- struct{}{}
		return released
	}

	h1 := hold("A", aRun, aStore)
	if delay, _ := rig.logRecords(heldLine)[0]["delay"].(float64); time.Duration(delay) > left || time.Duration(delay) < left-5*time.Second {
		t.Fatalf("A's message went back to the stream for %s, want the %s or so left of the minute, so it is still out at the second write", time.Duration(delay), left)
	}
	b := newFakeDaemon(t, "")
	b.refuses(http.StatusServiceUnavailable)
	if released := write("B", b); !slices.Equal(released, []int64{int64(h1)}) {
		t.Fatalf("INFRA's released holders after the write naming B are %v, want A's message at %d alone", released, h1)
	}
	h2 := hold("B", bRun, bStore)
	c := newFakeDaemon(t, daemonStateIn("INFRA", 20, inPhase("implementing"), cStore, cRun))
	if released, want := write("C", c), []int64{int64(h1), int64(h2)}; !slices.Equal(released, want) {
		t.Errorf("INFRA's released holders after the write naming C are %v, want %v: A's message, still out, then B's", released, want)
	}

	rig.waitFor(time.Minute, func() bool { return len(cursorOf(t, rig.database, "INFRA").releasedHolders) == 0 }, func() string {
		return fmt.Sprintf("INFRA's cursor still records released messages a minute after the write naming C: %s", cursorOf(t, rig.database, "INFRA"))
	})
	testwait.Holds(5*time.Second, 25*time.Millisecond, func() bool { return len(rig.logRecords(writeReleasedLine)) >= 2 })
	var released []string
	for _, line := range rig.logRecords(writeReleasedLine) {
		released = append(released, fmt.Sprintf("%v %v %v", line["streamSeq"], line["stateId"], line["level"]))
	}
	slices.Sort(released)
	want := []string{fmt.Sprintf("%d %s INFO", h1, aStore), fmt.Sprintf("%d %s INFO", h2, bStore)}
	slices.Sort(want)
	if !slices.Equal(released, want) {
		t.Errorf("the releases logged = %v, want %v: A's message and B's, each once at INFO with its store", released, want)
	}
	for name, held := range map[string]uint64{"A": h1, "B": h2} {
		if handBacks := rig.handBacksOf()[held]; handBacks != 1 {
			t.Errorf("%s's message at %d went back to the stream %d times, want once, before the write that ended its hold", name, held, handBacks)
		}
	}
	rig.waitFor(time.Minute, rig.finishedThrough(h2), func() string {
		return fmt.Sprintf("the released messages are not finished a minute after the write naming C: %+v", rig.consumerInfo())
	})
	rig.waitFor(20*time.Second, func() bool {
		cur := cursorOf(t, rig.database, "INFRA")
		return cur.stateID != nil && *cur.stateID == cStore
	}, func() string {
		return fmt.Sprintf("C's starting read is not recorded: INFRA's cursor is %s", cursorOf(t, rig.database, "INFRA"))
	})

	if cur := cursorOf(t, rig.database, "INFRA"); !slices.Equal(slices.Sorted(slices.Values(cur.foreignStateIDs)), slices.Sorted(slices.Values([]string{aStore, bStore}))) ||
		len(cur.releasedHolders) != 0 || cur.readOwed || cur.readHolder != nil {
		t.Errorf("INFRA's cursor is %s, want A's store and B's marked as no daemon of INFRA, no message recorded as released, nothing owed and no holder", cur)
	}
	if served := []int{a.served(), b.served(), c.served()}; !slices.Equal(served, []int{1, 0, 1}) {
		t.Errorf("A, B and C served %v reads, want [1 0 1]: A's failed starting read and C's starting read alone, and no read of a released message", served)
	}
	if reads := wholeReadsOf(t, rig.database, "INFRA"); len(reads) != 2 {
		t.Errorf("INFRA's whole reads are %v, want A's failed starting read and C's starting read alone", reads)
	} else {
		requireWholeRead(t, reads[1], ReadReasonStart, true)
	}
	if got := history(t, rig.database, "INFRA", "INFRA-1", FieldPhase); slices.Contains(got, `"merging"`) {
		t.Errorf("the phase's history is %v: a released message was applied", got)
	}
}

// markedRig is the rig of the tests of a store marked as no daemon of the project. INFRA's
// setting names A, read well in the rig's store, then B, whose starting read fails, so no read
// gives B's store and no write records it, then C, whose starting read is good, in a store of its
// own. B still publishes, three messages. The first one is of another store than C's: it makes one
// store read of C, which finds C's store unchanged, so the read marks B's store as no daemon of
// INFRA (foreign_state_ids), and the message is finished unapplied. B's later messages are
// finished unapplied at once: none of them makes a read, goes back to the stream or is applied.
// markedRig fails unless all of that holds, and answers the rig and B.
func markedRig(t *testing.T) (*intakeRig, *fakeDaemon) {
	t.Helper()
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
	reads := wholeReadsOf(t, rig.database, "INFRA")
	requireWholeRead(t, reads[len(reads)-1], ReadReasonStart, true)
	if cur := cursorOf(t, rig.database, "INFRA"); cur.stateID == nil || *cur.stateID != cStore || cur.previousStateID == nil ||
		*cur.previousStateID != rigStateID || len(cur.foreignStateIDs) != 0 {
		t.Fatalf("INFRA's cursor after C's starting read is %s, want C's store %s, A's store %s recorded and no store marked", cur, cStore, rigStateID)
	}

	for i := range 3 {
		rig.publishIn("INFRA", bRun, bStore, time.Now().UTC().Truncate(time.Microsecond), false, change(20+i, "INFRA-1", moved("phase", "merging")))
	}
	rig.settled()
	if testwait.Holds(2*time.Second, 25*time.Millisecond, func() bool { return c.served() > 2 }) || c.served() != 2 {
		t.Fatalf("C served %d reads, want its starting read and one store read for B's messages", c.served())
	}
	reads = wholeReadsOf(t, rig.database, "INFRA")
	requireWholeRead(t, reads[len(reads)-1], ReadReasonStore, true)
	if cur := cursorOf(t, rig.database, "INFRA"); !slices.Equal(cur.foreignStateIDs, []string{bStore}) || cur.stateID == nil || *cur.stateID != cStore ||
		cur.readOwed || cur.readHolder != nil {
		t.Fatalf("INFRA's cursor after B's messages is %s, want B's store %s marked, C's store %s, nothing owed and no holder", cur, bStore, cStore)
	}
	if held := rig.logRecords(heldLine); len(held) != 0 {
		t.Fatalf("the messages sent back = %v, want none: no message of B holds a read", held)
	}
	if got := history(t, rig.database, "INFRA", "INFRA-1", FieldPhase); slices.Contains(got, `"merging"`) {
		t.Fatalf("the phase's history is %v: a message of B was applied", got)
	}
	if served := b.served(); served != 1 {
		t.Fatalf("B served %d reads, want its failed starting read alone", served)
	}
	return rig, b
}

// TestADaemonWhoseReadsAllFailedMakesOneReadOnceTheSettingNamesAVerifiedDaemon: R3 reads a project
// whole only for the reasons it names, and applies no message of a daemon the setting named before.
// A, then B, whose reads all fail, then C, read well: B's messages make one read at the most, then
// none, and none of them goes back to the stream or is applied (markedRig).
func TestADaemonWhoseReadsAllFailedMakesOneReadOnceTheSettingNamesAVerifiedDaemon(t *testing.T) {
	rig, _ := markedRig(t)
	if drops := rig.awaitLogRecords(otherStoreLine, 1); len(drops) != 1 || drops[0]["stateId"] != bStore || drops[0]["cursorStateId"] != cStore {
		t.Errorf("the drops logged = %v, want one naming B's store %s and C's store %s", drops, bStore, cStore)
	}
	if drops := rig.logRecords(forgottenLine); len(drops) != 0 {
		t.Errorf("the drops logged as the daemon the setting named before = %v, want none: B's store is no store a write recorded", drops)
	}
}

// TestAWriteBackToADaemonMarkedAsNoDaemonOfTheProjectAppliesItsMessages: every write of the setting
// that resets the cursor clears the marks of stores that are no daemon of the project, since a
// mark says only that the store is no daemon the setting named before the write. After markedRig,
// B's store is marked; a minute after the store read that marked it, a write back to B's URL
// clears the mark, the write's starting read of B is due at once (whole reads of one project start
// ReadRetryAfter apart), is good and gives the cursor B's store, and B's next messages are
// applied, each in turn.
func TestAWriteBackToADaemonMarkedAsNoDaemonOfTheProjectAppliesItsMessages(t *testing.T) {
	rig, b := markedRig(t)
	rig.clock.advance(ReadRetryAfter + time.Second)
	b.serves(daemonStateIn("INFRA", 30, inPhase("testing"), bStore, bRun))
	if _, err := PutSettings(t.Context(), rig.database.Pool, "INFRA", b.URL, model.Actor{Kind: "user", ID: "alice@example.com"}); err != nil {
		t.Fatalf("point INFRA back at B: %v", err)
	}
	if c := cursorOf(t, rig.database, "INFRA"); len(c.foreignStateIDs) != 0 || c.previousStateID == nil || *c.previousStateID != cStore {
		t.Fatalf("INFRA's cursor after the write back to B is %s, want no store marked and C's store %s recorded", c, cStore)
	}
	rig.wake <- struct{}{}
	rig.awaitWholeReads(5)
	reads := wholeReadsOf(t, rig.database, "INFRA")
	requireWholeRead(t, reads[len(reads)-1], ReadReasonStart, true)
	if c := cursorOf(t, rig.database, "INFRA"); c.stateID == nil || *c.stateID != bStore || c.lastSeq == nil || *c.lastSeq != 30 || len(c.foreignStateIDs) != 0 {
		t.Fatalf("INFRA's cursor after the read of B is %s, want it at the read's 30 in B's store %s, with no store marked", c, bStore)
	}

	rig.publishIn("INFRA", bRun, bStore, time.Now().UTC().Truncate(time.Microsecond), false, change(31, "INFRA-1", moved("phase", "reviewing")))
	rig.publishIn("INFRA", bRun, bStore, time.Now().UTC().Truncate(time.Microsecond), false, change(32, "INFRA-1", claimMoved("tester", "working")))
	rig.settled()
	if got := history(t, rig.database, "INFRA", "INFRA-1", FieldPhase); !slices.Equal(got, []string{`"planning"`, `"implementing"`, `"testing"`, `"reviewing"`}) {
		t.Errorf("the phase's history is %v, want A's planning, C's implementing, B's read's testing and B's event's reviewing, and no merging", got)
	}
	if claim := currentValues(t, rig.database, "INFRA")["INFRA-1/claim.tester"]; claim != `"working"` {
		t.Errorf("the tester's claim is %q, want B's event's working", claim)
	}
	requireLastSeq(t, rig.database, "INFRA", 32)
	if held := rig.logRecords(heldLine); len(held) != 0 {
		t.Errorf("the messages sent back = %v, want none", held)
	}
}

// TestTheMarksOfStoresThatAreNoDaemonOfTheProjectKeepTheNewestEight: a cursor marks at most 8
// stores as no daemon of its project, the newest kept (maxForeignStores, marked; item 4 of
// read-rule-cases' "Where the table differs from the 17:41Z words"). INFRA's daemon is read well,
// and then nine senders, each in a store of its own, publish in turn, each a minute after the last
// read on the intake's clock: each message makes one store read, which finds INFRA's store
// unchanged and marks the message's store. The cursor then marks the newest eight stores. A
// message of a store still marked makes no read, and the next message of the first sender, whose
// mark went, makes one store read again, which marks its store as the newest.
//
// The test catches a cursor that marks fewer than eight stores, which after the nine senders has
// no mark of the second sender's store, and one that marks more than eight, which keeps the first
// sender's mark.
func TestTheMarksOfStoresThatAreNoDaemonOfTheProjectKeepTheNewestEight(t *testing.T) {
	const senders = 9
	stores := make([]string, senders)
	for i := range stores {
		stores[i] = fmt.Sprintf("%08d-6b1e-4c2a-9d3f-5e7a0b4c8d21", i+1)
	}
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")
	// send has the sender of store publish a message a minute after the last read, so a read it
	// calls for is due, and waits until the intake has finished every message.
	send := func(store string) {
		t.Helper()
		rig.clock.advance(ReadRetryAfter + time.Second)
		rig.publishIn("INFRA", rig.run, store, time.Now().UTC().Truncate(time.Microsecond), false, change(11, "INFRA-1", moved("phase", "merging")))
		rig.settled()
	}

	for i, store := range stores {
		send(store)
		if served := daemon.served(); served != i+2 {
			t.Fatalf("INFRA's daemon served %d reads after the message of sender %d, want its starting read and one store read for each sender", served, i+1)
		}
	}
	if c := cursorOf(t, rig.database, "INFRA"); !slices.Equal(c.foreignStateIDs, stores[1:]) {
		t.Fatalf("INFRA's cursor marks %v after %d senders, want the newest 8 stores %v", c.foreignStateIDs, senders, stores[1:])
	}

	send(stores[4])
	if testwait.Holds(2*time.Second, 25*time.Millisecond, func() bool { return daemon.served() > 10 }) {
		t.Errorf("INFRA's daemon served %d reads after a message of a store still marked, want 10: that message made a read", daemon.served())
	}
	send(stores[0])
	if served := daemon.served(); served != 11 {
		t.Errorf("INFRA's daemon served %d reads after the first sender's next message, want 11: one store read again, once its mark went", served)
	}
	reads := wholeReadsOf(t, rig.database, "INFRA")
	requireWholeRead(t, reads[len(reads)-1], ReadReasonStore, true)
	if c, want := cursorOf(t, rig.database, "INFRA"), append(slices.Clone(stores[2:]), stores[0]); !slices.Equal(c.foreignStateIDs, want) || c.readOwed {
		t.Errorf("INFRA's cursor is %s, want the stores %v marked, the first sender's newest, and nothing owed", c, want)
	}
	if held := rig.logRecords(heldLine); len(held) != 0 {
		t.Errorf("the messages sent back = %v, want none", held)
	}
	if got := history(t, rig.database, "INFRA", "INFRA-1", FieldPhase); slices.Contains(got, `"merging"`) {
		t.Errorf("the phase's history is %v: a sender's message was applied", got)
	}
}
