package legion

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/testwait"
)

// TestAMessageOfTheStoreASettingNamedBeforeMakesNoStartingRead: R3 applies no message of the
// daemon a setting named before, once the setting names another. A write of INFRA's setting names
// another daemon, and the daemon named before publishes its next event after the write and before
// the write's wake reaches the intake. The write left the cursor following no daemon, so INFRA has
// no starting point, and recorded the store the cursor held (previous_state_id): the message is of
// that store, so it is finished at its first delivery, unapplied, makes no starting read and holds
// none, and its drop is logged. The wake then reads the named daemon whole, and the named daemon's
// own events are applied, each in turn.
func TestAMessageOfTheStoreASettingNamedBeforeMakesNoStartingRead(t *testing.T) {
	const namedRun, namedStore = "8d2b6f41-3e7a-4c95-b0d8-1a5f9c3e7b26", "4b9e1c7a-6f2d-4a83-9e5b-0c7d3f8a1e64"
	rig := newIntakeRig(t)
	previous := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", previous.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")

	implementing := strings.Replace(planning("INFRA"), `"planning"`, `"implementing"`, 1)
	named := newFakeDaemon(t, strings.Replace(strings.Replace(daemonState("INFRA", 5, implementing), rigStateID, namedStore, 1), rigRun, namedRun, 1))
	if _, err := PutSettings(t.Context(), rig.database.Pool, "INFRA", named.URL, model.Actor{Kind: "user", ID: "alice@example.com"}); err != nil {
		t.Fatalf("point INFRA at the named daemon: %v", err)
	}
	if c := cursorOf(t, rig.database, "INFRA"); c.lastSeq != nil || c.stateID != nil || c.readOwed || c.readHolder != nil ||
		c.previousStateID == nil || *c.previousStateID != rigStateID {
		t.Fatalf("INFRA's cursor after the write is %s, want no sequence, no store, nothing owed, no holder and the store %s it held recorded", c, rigStateID)
	}

	// The daemon named before publishes its next event, under its own run and store.
	rig.publish("INFRA", change(11, "INFRA-1", moved("phase", "merging")))
	rig.settled()
	if served := named.served(); served != 0 {
		t.Errorf("the named daemon served %d reads before the wake, want none: a message of the daemon the setting named before makes no starting read", served)
	}
	if served := previous.served(); served != 1 {
		t.Errorf("the daemon named before served %d reads, want its starting one alone", served)
	}
	if reads := wholeReadsOf(t, rig.database, "INFRA"); len(reads) != 1 {
		t.Errorf("whole reads = %v, want the first daemon's starting read alone", reads)
	}
	if drops := rig.awaitLogRecords(forgottenLine, 1); len(drops) != 1 || drops[0]["project"] != "INFRA" || drops[0]["stateId"] != rigStateID || drops[0]["seq"] != float64(11) {
		t.Errorf("the drops logged = %v, want one naming INFRA, the message's store and its sequence", drops)
	}
	if held := rig.logRecords(heldLine); len(held) != 0 {
		t.Errorf("the messages sent back = %v, want none", held)
	}

	// The wake reads the named daemon whole, and its events follow that read.
	rig.wake <- struct{}{}
	rig.awaitWholeReads(2)
	reads := wholeReadsOf(t, rig.database, "INFRA")
	requireWholeRead(t, reads[len(reads)-1], ReadReasonStart, true)
	rig.publishIn("INFRA", namedRun, namedStore, time.Now().UTC().Truncate(time.Microsecond), false, change(6, "INFRA-1", moved("phase", "testing")))
	rig.publishIn("INFRA", namedRun, namedStore, time.Now().UTC().Truncate(time.Microsecond), false, change(7, "INFRA-1", moved("phase", "reviewing")))
	rig.settled()
	if got := history(t, rig.database, "INFRA", "INFRA-1", FieldPhase); !slices.Equal(got, []string{`"planning"`, `"implementing"`, `"testing"`, `"reviewing"`}) {
		t.Errorf("the phase's history is %v, want the first daemon's planning, the named daemon's read's implementing and its events' testing and reviewing, and no merging", got)
	}
	requireLastSeq(t, rig.database, "INFRA", 7)
	if served := named.served(); served != 1 {
		t.Errorf("the named daemon served %d reads, want the wake's alone", served)
	}
}

// TestAWriteBackToTheDaemonOfTheStoreASettingNamedBeforeAppliesItsMessages: R3 applies no message
// of the daemon a setting named before while the setting names another, and applies the messages of
// a daemon the setting names once a good read has read it. INFRA's setting names another daemon,
// whose starting read fails, so the cursor holds no store and keeps the store of the daemon named
// before recorded, with the URL it followed (previous_state_id, previous_state_url). A write back
// to that URL names that daemon again, so it clears the record. The write back comes a minute
// after the other daemon's read, so its wake's starting read is due at once (whole reads of one
// project start ReadRetryAfter apart). That read is good and gives the cursor that same store, so
// the daemon's next events are applied, each in turn, as every message of the cursor's store is,
// and none is dropped.
func TestAWriteBackToTheDaemonOfTheStoreASettingNamedBeforeAppliesItsMessages(t *testing.T) {
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")
	actor := model.Actor{Kind: "user", ID: "alice@example.com"}

	other := newFakeDaemon(t, "")
	other.refuses(http.StatusServiceUnavailable)
	if _, err := PutSettings(t.Context(), rig.database.Pool, "INFRA", other.URL, actor); err != nil {
		t.Fatalf("point INFRA at the other daemon: %v", err)
	}
	rig.wake <- struct{}{}
	rig.awaitWholeReads(2)
	reads := wholeReadsOf(t, rig.database, "INFRA")
	requireWholeRead(t, reads[len(reads)-1], ReadReasonStart, false)

	// A minute on, the setting names the first daemon again, which has moved on meanwhile.
	rig.clock.advance(ReadRetryAfter + time.Second)
	daemon.serves(daemonState("INFRA", 12, strings.Replace(planning("INFRA"), `"planning"`, `"implementing"`, 1)))
	if _, err := PutSettings(t.Context(), rig.database.Pool, "INFRA", daemon.URL, actor); err != nil {
		t.Fatalf("point INFRA back at its first daemon: %v", err)
	}
	if c := cursorOf(t, rig.database, "INFRA"); c.stateID != nil || c.previousStateID != nil || c.previousStateURL != nil {
		t.Fatalf("INFRA's cursor after the write back is %s, want no store and no store recorded", c)
	}
	rig.wake <- struct{}{}
	rig.awaitWholeReads(3)
	reads = wholeReadsOf(t, rig.database, "INFRA")
	requireWholeRead(t, reads[len(reads)-1], ReadReasonStart, true)
	if c := cursorOf(t, rig.database, "INFRA"); c.readOwed || c.lastSeq == nil || *c.lastSeq != 12 || c.stateID == nil || *c.stateID != rigStateID ||
		c.previousStateID != nil {
		t.Fatalf("INFRA's cursor after the good read is %s, want it at the read's 12 with the store %s, nothing owed and no store recorded", c, rigStateID)
	}

	rig.publish("INFRA", change(13, "INFRA-1", moved("phase", "testing")))
	rig.publish("INFRA", change(14, "INFRA-1", moved("phase", "reviewing")))
	rig.settled()
	if got := history(t, rig.database, "INFRA", "INFRA-1", FieldPhase); !slices.Equal(got, []string{`"planning"`, `"implementing"`, `"testing"`, `"reviewing"`}) {
		t.Errorf("the phase's history is %v, want the reads' planning and implementing, then the daemon's events' testing and reviewing", got)
	}
	requireLastSeq(t, rig.database, "INFRA", 14)
	if drops := rig.logRecords(forgottenLine); len(drops) != 0 {
		t.Errorf("the drops logged = %v, want none: the daemon the setting names again is read and followed", drops)
	}
	if served := daemon.served(); served != 2 {
		t.Errorf("the daemon served %d reads, want its first starting read and the write back's", served)
	}
}

// TestAWriteBackWhoseFirstReadFailsIsReadAgainAtItsDaemonsNextMessage: R3 tries a failed read
// again at the project's next message, and a write back to the daemon a setting named before names
// that daemon again. INFRA's setting names another daemon, whose starting read fails, so the cursor
// keeps the first daemon's store recorded with the URL it followed (previous_state_id,
// previous_state_url). A minute on, the setting names the first daemon again, which does not
// answer either, so the write's starting read is due at once and fails. The write back clears the
// record, and the failed read leaves the read owed for retry: the cursor holds no store, records no
// store and owes the read for retry. The first daemon's own messages are then the setting's
// daemon's, as every message is: its next message arrives left before the read is due, holds the
// owed read and goes back to the stream until the read is due, and its redelivery makes the read.
// The read is good and gives the cursor the daemon's store, and the daemon's next event, at 13, is
// applied. No message of the daemon is dropped as one of the daemon the setting named before.
func TestAWriteBackWhoseFirstReadFailsIsReadAgainAtItsDaemonsNextMessage(t *testing.T) {
	const left = 3 * time.Second
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")
	actor := model.Actor{Kind: "user", ID: "alice@example.com"}

	other := newFakeDaemon(t, "")
	other.refuses(http.StatusServiceUnavailable)
	if _, err := PutSettings(t.Context(), rig.database.Pool, "INFRA", other.URL, actor); err != nil {
		t.Fatalf("point INFRA at the other daemon: %v", err)
	}
	if c := cursorOf(t, rig.database, "INFRA"); c.previousStateID == nil || *c.previousStateID != rigStateID ||
		c.previousStateURL == nil || *c.previousStateURL != daemon.URL {
		t.Fatalf("INFRA's cursor after the write naming another daemon is %s, want the store %s recorded with the URL %s it followed", c, rigStateID, daemon.URL)
	}
	rig.wake <- struct{}{}
	rig.awaitWholeReads(2)
	reads := wholeReadsOf(t, rig.database, "INFRA")
	requireWholeRead(t, reads[len(reads)-1], ReadReasonStart, false)

	// A minute on, the setting names the first daemon again, which does not answer its starting
	// read.
	rig.clock.advance(ReadRetryAfter + time.Second)
	daemon.refuses(http.StatusServiceUnavailable)
	if _, err := PutSettings(t.Context(), rig.database.Pool, "INFRA", daemon.URL, actor); err != nil {
		t.Fatalf("point INFRA back at its first daemon: %v", err)
	}
	rig.wake <- struct{}{}
	rig.awaitWholeReads(3)
	reads = wholeReadsOf(t, rig.database, "INFRA")
	requireWholeRead(t, reads[len(reads)-1], ReadReasonStart, false)
	if c := cursorOf(t, rig.database, "INFRA"); !slices.Equal(c.readReasons, []string{ReadReasonRetry}) || c.stateID != nil || c.readHolder != nil ||
		c.previousStateID != nil || c.previousStateURL != nil {
		t.Errorf("INFRA's cursor after the failed read is %s, want the read owed for retry, no store, no holder and no store recorded", c)
	}
	before := currentValues(t, rig.database, "INFRA")

	// left before the read is due, the daemon's next message holds the owed read: it goes back to
	// the stream, unapplied, and makes no read yet.
	daemon.serves(daemonState("INFRA", 12, strings.Replace(planning("INFRA"), `"planning"`, `"testing"`, 1)))
	rig.clock.advance(ReadRetryAfter - left)
	rig.publish("INFRA", change(11, "INFRA-1", moved("phase", "implementing")))
	holder := rig.lastStreamSeq()
	rig.settledHolding(1)
	if served := daemon.served(); served != 2 {
		t.Errorf("the daemon served %d reads before the read was due, want its first starting read and the failed one", served)
	}
	if after := currentValues(t, rig.database, "INFRA"); !maps.Equal(after, before) {
		t.Errorf("legion_current = %v, want %v: no message is applied while the cursor holds no store", after, before)
	}

	// Its redelivery, once the read is due, makes the owed read.
	rig.settled()
	if served := daemon.served(); served != 3 {
		t.Errorf("the daemon served %d reads, want one more once the read was due", served)
	}
	reads = wholeReadsOf(t, rig.database, "INFRA")
	requireWholeRead(t, reads[len(reads)-1], ReadReasonRetry, true)
	if got := rig.heldValues(holder, "handBack"); !slices.Equal(got, oneTo(1)) {
		t.Errorf("the cursor counted the hand-backs %v of the daemon's message, want 1: it held the read once", got)
	}
	if c := cursorOf(t, rig.database, "INFRA"); c.readOwed || c.lastSeq == nil || *c.lastSeq != 12 || c.stateID == nil || *c.stateID != rigStateID ||
		c.readHolder != nil || c.readHolds != 0 {
		t.Fatalf("INFRA's cursor after the good read is %s, want it at the read's 12 with the store %s, nothing owed and no holder", c, rigStateID)
	}

	rig.publish("INFRA", change(13, "INFRA-1", moved("phase", "reviewing")))
	rig.settled()
	if got := history(t, rig.database, "INFRA", "INFRA-1", FieldPhase); !slices.Equal(got, []string{`"planning"`, `"testing"`, `"reviewing"`}) {
		t.Errorf("the phase's history is %v, want the first read's planning, the retry's testing and the daemon's event's reviewing", got)
	}
	requireLastSeq(t, rig.database, "INFRA", 13)
	if drops := rig.logRecords(forgottenLine); len(drops) != 0 {
		t.Errorf("the drops logged = %v, want none: the daemon the setting names again is not the daemon it named before", drops)
	}
}

// TestATakeOverFinishesAMessageOfTheStoreASettingNamedBeforeAfterTheTakeOversRead: an intake that
// takes the durable over queues each project's starting point on the project's runner
// (SettingsChanged), and a message of the store the project's cursor drops waits on that runner
// behind the take-over's read, as every message does, and is judged against the cursor that read
// leaves. INFRA's setting names another daemon, whose starting read fails. A minute on, that read
// is due again; the intake stops and a second intake takes the durable over, and its take-over
// reads the named daemon again, an answer the test holds. The daemon named before publishes
// meanwhile: its message is not finished while the take-over's read is in flight. Once that read
// has failed, the message is finished unapplied, makes no read and holds none: the read it would
// call for began a moment before, so it is not due.
func TestATakeOverFinishesAMessageOfTheStoreASettingNamedBeforeAfterTheTakeOversRead(t *testing.T) {
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")

	named := newFakeDaemon(t, "")
	named.refuses(http.StatusServiceUnavailable)
	if _, err := PutSettings(t.Context(), rig.database.Pool, "INFRA", named.URL, model.Actor{Kind: "user", ID: "alice@example.com"}); err != nil {
		t.Fatalf("point INFRA at another daemon: %v", err)
	}
	rig.wake <- struct{}{}
	rig.awaitWholeReads(2)
	reads := wholeReadsOf(t, rig.database, "INFRA")
	requireWholeRead(t, reads[len(reads)-1], ReadReasonStart, false)

	// A minute on, the failed read is due, and a second intake takes the durable over. The named
	// daemon holds its answer to the take-over's read until the test releases it.
	release := named.holds(t)
	rig.clock.advance(ReadRetryAfter + time.Second)
	rig.stop()
	rig.start()
	rig.waitFor(time.Minute, func() bool { return named.served() >= 2 }, func() string {
		return fmt.Sprintf("the take-over did not read the named daemon again: it served %d reads", named.served())
	})
	rig.publish("INFRA", change(11, "INFRA-1", moved("phase", "merging")))
	message := rig.lastStreamSeq()
	if testwait.Holds(time.Second, 25*time.Millisecond, func() bool { return rig.consumerInfo().AckFloor.Stream >= message }) {
		t.Errorf("the message of the daemon the setting named before was finished while the take-over's read was in flight: it did not wait on the project's runner")
	}

	release()
	rig.settled()
	reads = wholeReadsOf(t, rig.database, "INFRA")
	requireWholeRead(t, reads[len(reads)-1], ReadReasonRetry, false)
	if testwait.Holds(2*time.Second, 25*time.Millisecond, func() bool { return named.served() > 2 }) {
		t.Errorf("the named daemon served %d reads, want the write's and the take-over's: the message of the daemon named before made a read or called for one", named.served())
	}
	if held := rig.logRecords(heldLine); len(held) != 0 {
		t.Errorf("the messages sent back = %v, want none: the message of the daemon named before holds no read", held)
	}
	if c := cursorOf(t, rig.database, "INFRA"); !slices.Equal(c.readReasons, []string{ReadReasonRetry}) || c.readHolder != nil || c.stateID != nil {
		t.Errorf("INFRA's cursor is %s, want the read owed for retry, no holder and no store", c)
	}
	if got := history(t, rig.database, "INFRA", "INFRA-1", FieldPhase); slices.Contains(got, `"merging"`) {
		t.Errorf("the phase's history is %v: the message of the daemon the setting named before was applied", got)
	}
	if drops := rig.awaitLogRecords(forgottenLine, 1); len(drops) != 1 || drops[0]["stateId"] != rigStateID || drops[0]["seq"] != float64(11) {
		t.Errorf("the drops logged = %v, want one, of the message at 11 in the store %s", drops, rigStateID)
	}
}

// TestAMessageOfTheStoreASettingNamedBeforeCallsForTheOwedRead: until a read of the URL a write of
// the setting names has succeeded, Dispatch cannot know whether that URL names another store or the
// same store behind a new URL, and a daemon the cursor follows in another store can be redeployed
// into the store the setting named before. So, whatever store the cursor holds, a message of that
// store calls for the owed read where it is due and no message holds it (read-rule-cases row 4.3).
// The message waits on the project's runner behind a read in flight, as every message does, is
// finished unapplied, and the project's runner makes the read after it (callRead). At most one
// such call waits for each project. In each subtest INFRA's daemon has served a good starting read
// in the rig's store, and then a write of INFRA's setting names a second URL, which the write's
// wake reads (repoint).
func TestAMessageOfTheStoreASettingNamedBeforeCallsForTheOwedRead(t *testing.T) {
	const namedRun, namedStore = "7a3d9e2b-4c1f-4b86-9e5a-2f8c6d1b3e74", "5d8b2e6f-1a4c-4f97-b3e0-6c9a2d7f1b58"
	implementing := strings.Replace(planning("INFRA"), `"planning"`, `"implementing"`, 1)
	// repoint starts the rig with INFRA's daemon read whole, writes INFRA's setting naming named,
	// and waits for the wake's starting read of named. It answers the rig and INFRA's first daemon.
	repoint := func(t *testing.T, named *fakeDaemon) (*intakeRig, *fakeDaemon) {
		t.Helper()
		rig := newIntakeRig(t)
		daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
		seedLegionProject(t, rig.database, "INFRA", daemon.URL)
		rig.start()
		rig.awaitStartingRead("INFRA")
		if _, err := PutSettings(t.Context(), rig.database.Pool, "INFRA", named.URL, model.Actor{Kind: "user", ID: "alice@example.com"}); err != nil {
			t.Fatalf("point INFRA at a second URL: %v", err)
		}
		rig.wake <- struct{}{}
		rig.awaitWholeReads(2)
		return rig, daemon
	}

	// The daemon behind the second URL serves INFRA's own store, and its first read fails. A
	// minute on, the read is due, and a later message of that store is acknowledged while the read
	// it calls for waits for the daemon's answer. That read is good, so it clears the record of the
	// store, and the project's next message of that store is applied.
	t.Run("the same store behind a new URL whose first read fails", func(t *testing.T) {
		alias := newFakeDaemon(t, "")
		alias.refuses(http.StatusServiceUnavailable)
		rig, daemon := repoint(t, alias)
		reads := wholeReadsOf(t, rig.database, "INFRA")
		requireWholeRead(t, reads[len(reads)-1], ReadReasonStart, false)
		if c := cursorOf(t, rig.database, "INFRA"); !slices.Equal(c.readReasons, []string{ReadReasonRetry}) || c.stateID != nil || c.readHolder != nil ||
			c.previousStateID == nil || *c.previousStateID != rigStateID || c.previousStateURL == nil || *c.previousStateURL != daemon.URL {
			t.Fatalf("INFRA's cursor after the failed starting read is %s, want the read owed for retry, no store, no holder and the store %s recorded with the URL %s",
				c, rigStateID, daemon.URL)
		}

		// Each answer of the daemon behind the second URL waits until the test releases it, so the
		// message is acknowledged while the read it calls for is not answered yet.
		alias.serves(daemonState("INFRA", 12, implementing))
		release := alias.holds(t)
		rig.clock.advance(ReadRetryAfter + time.Second)
		rig.publish("INFRA", change(11, "INFRA-1", moved("phase", "merging")))
		rig.settled()
		rig.waitFor(20*time.Second, func() bool { return alias.served() >= 2 }, func() string {
			return fmt.Sprintf("the daemon behind the second URL served %d reads after the message of its store, want the failed starting read and the read the message called for", alias.served())
		})
		release()
		rig.awaitWholeReads(3)
		reads = wholeReadsOf(t, rig.database, "INFRA")
		requireWholeRead(t, reads[len(reads)-1], ReadReasonRetry, true)
		if c := cursorOf(t, rig.database, "INFRA"); c.readOwed || c.lastSeq == nil || *c.lastSeq != 12 || c.stateID == nil || *c.stateID != rigStateID ||
			c.readHolder != nil || c.readHolds != 0 || c.previousStateID != nil || c.previousStateURL != nil {
			t.Fatalf("INFRA's cursor after the good read is %s, want it at the read's 12 in the store %s, nothing owed, no holder and no store recorded", c, rigStateID)
		}
		if held := rig.logRecords(heldLine); len(held) != 0 {
			t.Errorf("the messages sent back = %v, want none: no message holds the read", held)
		}

		rig.publish("INFRA", change(13, "INFRA-1", moved("phase", "testing")))
		rig.settled()
		if got := history(t, rig.database, "INFRA", "INFRA-1", FieldPhase); !slices.Equal(got, []string{`"planning"`, `"implementing"`, `"testing"`}) {
			t.Errorf("the phase's history is %v, want the first read's planning, the good read's implementing and the next message's testing, and no merging", got)
		}
		requireLastSeq(t, rig.database, "INFRA", 13)
		if drops := rig.awaitLogRecords(forgottenLine, 1); len(drops) != 1 || drops[0]["stateId"] != rigStateID || drops[0]["seq"] != float64(11) {
			t.Errorf("the drops logged = %v, want one, of the message at 11 before the good read", drops)
		}
		if served := alias.served(); served != 2 {
			t.Errorf("the daemon behind the second URL served %d reads, want the failed starting read and the good one", served)
		}
	})

	// The daemon behind the second URL serves a store of its own, and its starting read is good.
	// No cheap path of the intake then leaves the cursor owing a read that is due and that no
	// message holds, so the test writes INFRA's cursor so by hand: a call no message makes would
	// make a read now. A message of the store the setting named before calls for no read of its
	// own, and it makes that owed read as such a call (callRead): the intake cannot tell the old
	// daemon from the setting's own daemon moved into that store, whose messages are then all of
	// it (read-rule-cases row 4.3). The read is good, so nothing stays owed.
	t.Run("another store, verified", func(t *testing.T) {
		named := newFakeDaemon(t, strings.Replace(strings.Replace(daemonState("INFRA", 5, implementing), rigStateID, namedStore, 1), rigRun, namedRun, 1))
		rig, _ := repoint(t, named)
		reads := wholeReadsOf(t, rig.database, "INFRA")
		requireWholeRead(t, reads[len(reads)-1], ReadReasonStart, true)
		if _, err := rig.database.Pool.Exec(t.Context(), `
			update legion_event_cursors set read_reasons = '{retry}', last_read_started_at = now() - interval '2 minutes', read_holder = null, read_holds = 0
			where project = 'INFRA'
		`); err != nil {
			t.Fatalf("write INFRA's cursor: %v", err)
		}
		_, current, found, err := readPosition(t.Context(), rig.database.Pool, "INFRA", false)
		if err != nil || !found {
			t.Fatalf("read INFRA's cursor: found %v, %v", found, err)
		}
		if current.stateID == nil || *current.stateID != namedStore || !current.forgets(rigStateID) || !current.readFor("", rig.clock.now(), 0, false, nil).read {
			t.Fatalf("INFRA's cursor is %s, want the store %s, the store %s dropped, and a read that a call no message makes would make now",
				cursorOf(t, rig.database, "INFRA"), namedStore, rigStateID)
		}

		rig.publish("INFRA", change(11, "INFRA-1", moved("phase", "merging")))
		rig.settled()
		rig.awaitWholeReads(3)
		reads = wholeReadsOf(t, rig.database, "INFRA")
		requireWholeRead(t, reads[len(reads)-1], ReadReasonRetry, true)
		if testwait.Holds(2*time.Second, 25*time.Millisecond, func() bool { return named.served() > 2 }) {
			t.Errorf("the daemon the setting names served %d reads, want its starting read and the owed read the message made: the message called for more than the owed read", named.served())
		}
		if c := cursorOf(t, rig.database, "INFRA"); c.readOwed || c.stateID == nil || *c.stateID != namedStore ||
			c.previousStateID == nil || *c.previousStateID != rigStateID {
			t.Errorf("INFRA's cursor is %s, want nothing owed after the good read, the store %s and the store %s still recorded", c, namedStore, rigStateID)
		}
		if got := history(t, rig.database, "INFRA", "INFRA-1", FieldPhase); slices.Contains(got, `"merging"`) {
			t.Errorf("the phase's history is %v: the message of the store the setting named before was applied", got)
		}
		if drops := rig.awaitLogRecords(forgottenLine, 1); len(drops) != 1 || drops[0]["stateId"] != rigStateID {
			t.Errorf("the drops logged = %v, want one naming the store %s", drops, rigStateID)
		}
	})

	// The first read of the daemon behind the second URL fails, and each read of it from then on
	// lasts a minute on the intake's clock, as a read that the daemon never answers within
	// RequestTimeout does: each read of it that fails is due again at once. A minute on, the read
	// is due, and several messages of the store the setting named before reach the project's
	// runner while the first one waits on INFRA's cursor, which the test holds, so each of them is
	// on the runner before the call the first one queues behind itself. They queue one read, not
	// one each, which the daemon answers once all of them are finished.
	t.Run("a burst queues one read", func(t *testing.T) {
		const burst = 4
		named := newFakeDaemon(t, "")
		named.refuses(http.StatusServiceUnavailable)
		rig, _ := repoint(t, named)
		reads := wholeReadsOf(t, rig.database, "INFRA")
		requireWholeRead(t, reads[len(reads)-1], ReadReasonStart, false)

		var lasts func()
		lasts = func() {
			rig.clock.advance(ReadRetryAfter)
			named.lagsNext(lasts)
		}
		named.lagsNext(lasts)
		release := named.holds(t)
		rig.clock.advance(ReadRetryAfter + time.Second)
		lock, err := rig.database.Pool.Begin(t.Context())
		if err != nil {
			t.Fatalf("open the transaction that holds INFRA's cursor: %v", err)
		}
		t.Cleanup(func() { _ = lock.Rollback(context.Background()) })
		if _, err := lock.Exec(t.Context(), `select 1 from legion_event_cursors where project = 'INFRA' for update`); err != nil {
			t.Fatalf("hold INFRA's cursor: %v", err)
		}
		for i := range burst {
			rig.publish("INFRA", change(11+i, "INFRA-1", moved("phase", "merging")))
		}
		rig.waitFor(20*time.Second, func() bool {
			info := rig.consumerInfo()
			return info.NumPending == 0 && info.NumAckPending == burst
		}, func() string {
			return fmt.Sprintf("the burst did not reach the intake while INFRA's cursor was held: %+v", rig.consumerInfo())
		})
		if err := lock.Rollback(t.Context()); err != nil {
			t.Fatalf("let INFRA's cursor go: %v", err)
		}
		rig.settled()
		rig.waitFor(20*time.Second, func() bool { return named.served() >= 2 }, func() string {
			return fmt.Sprintf("the daemon the setting names served %d reads after the burst, want the failed starting read and the read the burst called for", named.served())
		})
		release()
		rig.awaitWholeReads(3)
		if testwait.Holds(2*time.Second, 25*time.Millisecond, func() bool { return named.served() > 2 }) {
			t.Errorf("the daemon the setting names served %d reads after a burst of %d messages of the store the setting named before, want the failed starting read and one read for the burst",
				named.served(), burst)
		}
		reads = wholeReadsOf(t, rig.database, "INFRA")
		requireWholeRead(t, reads[len(reads)-1], ReadReasonRetry, false)
		if got := history(t, rig.database, "INFRA", "INFRA-1", FieldPhase); slices.Contains(got, `"merging"`) {
			t.Errorf("the phase's history is %v: a message of the store the setting named before was applied", got)
		}
		if held := rig.logRecords(heldLine); len(held) != 0 {
			t.Errorf("the messages sent back = %v, want none: no message holds the read", held)
		}
	})
}

// TestAMessageOfTheStoreASettingNamedBeforeCallsForNoReadBeforeTheOwedReadIsDue: a message of the
// store that a write of the setting named before calls for the owed read only where that read is
// due (callRead). INFRA's setting names another daemon, whose starting read fails, so the read is
// owed for retry and is due a minute after that read began. Inside that minute, with no read in
// flight, the daemon named before publishes: its message is finished unapplied, holds no read and
// calls for none, so the named daemon serves no other read.
func TestAMessageOfTheStoreASettingNamedBeforeCallsForNoReadBeforeTheOwedReadIsDue(t *testing.T) {
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")
	named := newFakeDaemon(t, "")
	named.refuses(http.StatusServiceUnavailable)
	// The write is made here rather than through pointAt, which moves the intake's clock past the
	// minute of the read it waits for: this test stays inside that minute.
	if _, err := PutSettings(t.Context(), rig.database.Pool, "INFRA", named.URL, model.Actor{Kind: "user", ID: "alice@example.com"}); err != nil {
		t.Fatalf("point INFRA at the named daemon: %v", err)
	}
	rig.wake <- struct{}{}
	rig.awaitWholeReads(2)
	if c := cursorOf(t, rig.database, "INFRA"); !slices.Equal(c.readReasons, []string{ReadReasonRetry}) || c.stateID != nil || c.readHolder != nil ||
		c.previousStateID == nil || *c.previousStateID != rigStateID {
		t.Fatalf("INFRA's cursor after the failed starting read is %s, want the read owed for retry, no store, no holder and the store %s recorded", c, rigStateID)
	}

	rig.clock.advance(ReadRetryAfter - 10*time.Second)
	rig.publish("INFRA", change(11, "INFRA-1", moved("phase", "merging")))
	rig.settled()
	if testwait.Holds(2*time.Second, 25*time.Millisecond, func() bool { return named.served() > 1 }) {
		t.Errorf("the named daemon served %d reads, want its failed starting read alone: the message of the store the setting named before called for a read that was not due", named.served())
	}
	if c := cursorOf(t, rig.database, "INFRA"); !slices.Equal(c.readReasons, []string{ReadReasonRetry}) || c.readHolder != nil || c.readHolds != 0 {
		t.Errorf("INFRA's cursor is %s, want the read still owed for retry and no holder", c)
	}
	if held := rig.logRecords(heldLine); len(held) != 0 {
		t.Errorf("the messages sent back = %v, want none: the message of the store the setting named before holds no read", held)
	}
	if got := history(t, rig.database, "INFRA", "INFRA-1", FieldPhase); slices.Contains(got, `"merging"`) {
		t.Errorf("the phase's history is %v: the message of the store the setting named before was applied", got)
	}
	if drops := rig.awaitLogRecords(forgottenLine, 1); len(drops) != 1 || drops[0]["stateId"] != rigStateID || drops[0]["seq"] != float64(11) {
		t.Errorf("the drops logged = %v, want one, of the message at 11 in the store %s", drops, rigStateID)
	}
}

// TestAWriteOfTheSameURLDeletesTheHeldPartsOfASplitTransaction: R3 applies a transaction the daemon
// split over several messages whole, once every part has arrived, and a write of the project's
// setting that no message's hold keeps leaves the project no starting point (resetCursor). INFRA's
// daemon is read at 10, and the first part of a transaction of 11 and 12 is held
// (legion_event_parts). A write of the same URL, while no message holds a read, resets the cursor
// and deletes the held part, and its wake reads the daemon whole. A minute later on the intake's
// clock, so that a read the last part calls for is due at once, the transaction's last part
// arrives and writes nothing of the transaction:
//   - where the write's read holds the transaction (it serves 12), the last part is at or below the
//     read's snapshot, and no read follows;
//   - where the write's read is older than the transaction (it serves 10), the last part is past
//     the read's next sequence, so it calls for a gap read, which serves 12, and is at or below it.
func TestAWriteOfTheSameURLDeletesTheHeldPartsOfASplitTransaction(t *testing.T) {
	for _, tc := range []struct {
		name string
		// writeRead is the stateSeq the write's read serves, and reasons the whole reads INFRA's
		// daemon serves in all.
		writeRead int
		reasons   []string
	}{
		{"the write's read holds the transaction", 12, []string{ReadReasonStart, ReadReasonStart}},
		{"the write's read is older than the transaction", 10, []string{ReadReasonStart, ReadReasonStart, ReadReasonGap}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newIntakeRig(t)
			daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
			seedLegionProject(t, rig.database, "INFRA", daemon.URL)
			rig.start()
			rig.awaitStartingRead("INFRA")

			at := time.Now().UTC().Truncate(time.Microsecond)
			rig.publishIn("INFRA", rig.run, rigStateID, at, true, change(11, "INFRA-1", moved("phase", "merging")))
			rig.settled()
			if held := heldPartCount(t, rig.database, "INFRA"); held != 1 {
				t.Fatalf("%d parts held after the first part, want 1", held)
			}
			daemon.serves(daemonState("INFRA", tc.writeRead, inPhase("implementing")))
			if _, err := PutSettings(t.Context(), rig.database.Pool, "INFRA", daemon.URL, model.Actor{Kind: "user", ID: "alice@example.com"}); err != nil {
				t.Fatalf("write INFRA's setting again: %v", err)
			}
			if held := heldPartCount(t, rig.database, "INFRA"); held != 0 {
				t.Fatalf("%d parts held after the write of the same URL, want none", held)
			}
			rig.wake <- struct{}{}
			rig.awaitWholeReads(2)

			rig.clock.advance(ReadRetryAfter + time.Second)
			daemon.serves(daemonState("INFRA", 12, inPhase("implementing")))
			rig.publishIn("INFRA", rig.run, rigStateID, at, false, change(12, "INFRA-1", claimMoved("tester", "working")))
			rig.settled()
			if reasons := rig.wholeReadReasons("INFRA"); !slices.Equal(reasons, tc.reasons) {
				t.Errorf("INFRA's whole reads %v, want %v", reasons, tc.reasons)
			}
			if got := history(t, rig.database, "INFRA", "INFRA-1", FieldPhase); slices.Contains(got, `"merging"`) {
				t.Errorf("the phase's history is %v: the first part was applied", got)
			}
			if claim, written := currentValues(t, rig.database, "INFRA")["INFRA-1/claim.tester"]; written {
				t.Errorf("the tester's claim is %s: the last part was applied", claim)
			}
			requireLastSeq(t, rig.database, "INFRA", 12)
			if held := heldPartCount(t, rig.database, "INFRA"); held != 0 {
				t.Errorf("%d parts held after the last part, want none", held)
			}
		})
	}
}

// TestAWriteOfTheSameURLWhileAReadOfItIsInFlightTakesThatReadAsItsStartingPoint: read-rule-cases
// row 6.2b, the architect's ruling of 2026-11-25T22:52Z. INFRA's starting read is good at 10, and a
// minute later on the intake's clock its message at 12 shows a gap and makes the gap read, whose
// answer the daemon holds. While that read is in flight on INFRA's runner, a write of INFRA's own
// URL finds no message holding a read, so it resets the cursor (resetCursor, row 6.2). The read
// then lands and is recorded as any good read of the URL the setting names (recordWhole): it gives
// the cursor its store and its 12, owes nothing and names no holder (cursor.afterRead). The
// write's wake, which waited on the runner behind the read, finds the cursor following the
// setting's URL again, so it makes no starting read and defers no call (Intake.startingPoint): the
// daemon serves no read after the write, a minute on included, and INFRA's next event, at 13, past
// the read's snapshot, is applied. So the read that answers the write began before the write. An
// operator who needs a read made after the write writes the setting once more (docs/kubernetes.md,
// on an edit of the daemon's database by SQL).
func TestAWriteOfTheSameURLWhileAReadOfItIsInFlightTakesThatReadAsItsStartingPoint(t *testing.T) {
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")

	release := daemon.holds(t)
	daemon.serves(daemonState("INFRA", 12, inPhase("testing")))
	rig.publish("INFRA", change(12, "INFRA-1", moved("phase", "testing")))
	rig.waitFor(10*time.Second, func() bool { return daemon.served() >= 2 }, func() string {
		return "INFRA's gap read never began"
	})
	if _, err := PutSettings(t.Context(), rig.database.Pool, "INFRA", daemon.URL, model.Actor{Kind: "user", ID: "alice@example.com"}); err != nil {
		t.Fatalf("write INFRA's setting again with its own URL: %v", err)
	}
	wrote := rig.clock.now()
	if c := cursorOf(t, rig.database, "INFRA"); c.stateURL != "" || c.lastSeq != nil || c.stateID != nil || c.readOwed || c.readHolder != nil {
		t.Fatalf("INFRA's cursor after the write of its own URL during the gap read is %s, want it reset: following no daemon, no sequence, no store, nothing owed and no holder", c)
	}
	rig.wake <- struct{}{}
	// A second for the wake to queue INFRA's starting point on its runner, behind the read.
	if testwait.Holds(time.Second, 25*time.Millisecond, func() bool { return daemon.served() > 2 }) {
		t.Fatalf("INFRA's daemon served %d reads while the gap read was in flight, want that read alone: the write's wake read beside it", daemon.served())
	}
	release()
	rig.settled()

	reads := wholeReadsOf(t, rig.database, "INFRA")
	if len(reads) != 2 {
		t.Fatalf("INFRA's whole reads are %v, want the starting read and the gap read", reads)
	}
	requireWholeRead(t, reads[1], ReadReasonGap, true)
	if started := lastReadStartedAt(t, rig, "INFRA"); !started.Before(wrote) {
		t.Errorf("INFRA's newest whole read began at %s on the intake's clock, want before the write at %s: the gap read answers the write", started, wrote)
	}
	if c := cursorOf(t, rig.database, "INFRA"); c.stateURL != daemon.URL || c.stateID == nil || *c.stateID != rigStateID || c.lastSeq == nil || *c.lastSeq != 12 ||
		c.readOwed || c.readHolder != nil || c.readHolds != 0 {
		t.Errorf("INFRA's cursor after the gap read is %s, want it following %s in the store %s at the read's 12, with nothing owed and no holder", c, daemon.URL, rigStateID)
	}
	if testwait.Holds(2*time.Second, 25*time.Millisecond, func() bool { return daemon.served() > 2 || rig.clock.deferredCalls() > 0 }) {
		t.Errorf("after the gap read INFRA's daemon served %d reads and the intake deferred %d calls, want 2 and none: the write's wake read the daemon or deferred a call", daemon.served(), rig.clock.deferredCalls())
	}
	rig.clock.advance(ReadRetryAfter + time.Second)
	if deferred, made := rig.clock.deferrals(); deferred != 0 || made != 0 {
		t.Errorf("the intake deferred %d calls and made %d of them, want none", deferred, made)
	}

	rig.publish("INFRA", change(13, "INFRA-1", moved("phase", "reviewing")))
	rig.settled()
	if got := history(t, rig.database, "INFRA", "INFRA-1", FieldPhase); !slices.Equal(got, []string{`"planning"`, `"testing"`, `"reviewing"`}) {
		t.Errorf("the phase's history is %v, want the starting read's planning, the gap read's testing and the event's reviewing", got)
	}
	requireLastSeq(t, rig.database, "INFRA", 13)
	if served := daemon.served(); served != 2 {
		t.Errorf("INFRA's daemon served %d reads, want the starting read and the gap read: the event past the read's snapshot made a read", served)
	}
}

// TestASettingDeletedDuringAHoldDropsTheHeldMessage: R3 applies no message of a project with no
// setting, and a delete of the setting resets the project's cursor as a write of another URL does
// and keeps its row (settingWritten; read-rule-cases row 6.5, the architect's ruling of
// 2026-11-25T06:25Z). INFRA's daemon refuses every read after its good starting read, and a gap's
// message holds the owed read, going back to the stream after each read it makes: the intake's
// clock jumps ReadRetryAfter less holdAtLeast at each reading, so each hand-back is holdAtLeast
// long. Once it has gone back, the setting is deleted: the message's next delivery is dropped and
// acknowledged, which releases the durable's ack floor, and the message goes back no more. The
// cursor stays. It follows no daemon, owes nothing, names no holder, counts no hand-back and holds
// no part. It records the message's stream sequence as released (released_holders): the delete
// ended the hold, and the message's delivery after it is dropped before the cursor is read, so the
// record stays. It keeps the start of the project's newest whole read (last_read_started_at): the
// one it had before the delete, or a later one where the holder's redelivery read in the second
// before the delete. At cdc5fd84 the delete deleted the cursor row, and that start with it.
func TestASettingDeletedDuringAHoldDropsTheHeldMessage(t *testing.T) {
	rig := newIntakeRig(t)
	daemon := newFakeDaemon(t, daemonState("INFRA", 10, planning("INFRA")))
	seedLegionProject(t, rig.database, "INFRA", daemon.URL)
	rig.start()
	rig.awaitStartingRead("INFRA")

	daemon.refuses(http.StatusServiceUnavailable)
	rig.clock.jumps(ReadRetryAfter - holdAtLeast)
	rig.publish("INFRA", change(12, "INFRA-1", moved("phase", "merging")))
	holder := rig.lastStreamSeq()
	rig.waitFor(time.Minute, func() bool { return len(rig.logRecords(heldLine)) >= 1 }, func() string {
		return "the gap's message never went back to the stream holding INFRA's read"
	})
	started := lastReadStartedAt(t, rig, "INFRA")
	if deleted, err := DeleteSettings(t.Context(), rig.database.Pool, "INFRA"); err != nil || !deleted {
		t.Fatalf("delete INFRA's setting: deleted %v, %v", deleted, err)
	}
	deletedAt, atDelete := time.Now(), len(rig.logRecords(heldLine))
	rig.waitFor(30*time.Second, func() bool {
		info := rig.consumerInfo()
		return info.AckFloor.Stream >= holder && info.NumAckPending == 0 && info.NumPending == 0
	}, func() string {
		return fmt.Sprintf("the held message is not finished 30 s after the delete of INFRA's setting: %+v", rig.consumerInfo())
	})
	rig.clock.jumps(0)
	if took := time.Since(deletedAt); took >= 5*time.Second {
		t.Errorf("the held message was finished %s after the delete, want less than 5 s: its next delivery after the delete", took.Round(time.Millisecond))
	}
	if after := len(rig.heldValues(holder, "handBack")) - atDelete; after > 1 {
		t.Errorf("the held message went back %d more times after the delete, want at most the one in flight at the delete", after)
	}
	if drops := rig.awaitLogRecords(droppedLine, 1); drops[0]["project"] != "INFRA" {
		t.Errorf("the drops logged = %v, want one naming INFRA", drops)
	}
	if got := history(t, rig.database, "INFRA", "INFRA-1", FieldPhase); slices.Contains(got, `"merging"`) {
		t.Errorf("the phase's history is %v: the held message was applied", got)
	}
	if c := cursorOf(t, rig.database, "INFRA"); c.stateURL != "" || c.readOwed || c.readHolder != nil || c.readHolds != 0 ||
		!slices.Equal(c.releasedHolders, []int64{int64(holder)}) {
		t.Errorf("INFRA's cursor after the delete of its setting is %s, want it kept and reset: following no daemon, nothing owed, no holder and no hand-back counted, with the held message's stream sequence %d recorded as released", c, holder)
	}
	if after := lastReadStartedAt(t, rig, "INFRA"); after.Before(started) {
		t.Errorf("the start of INFRA's newest whole read is %s after the delete of its setting, want the %s it had before the delete, or a later one", after, started)
	}
	if parts := heldPartCount(t, rig.database, "INFRA"); parts != 0 {
		t.Errorf("%d parts held after the delete of INFRA's setting, want none", parts)
	}
}

// TestAWriteOfTheSameURLKeepsTheHoldAndAWriteOfAnotherURLRecordsItsRelease: R3 says a reason that
// arrives while a read is owed joins it, the message that holds the owed read makes it at its
// redelivery, and that message goes back to the stream stateMaxHolds times at the most, so it holds
// the durable's ack floor for about 11 minutes at the most (read-rule-cases rows 6.1, 6.1b and
// 6.3b, the architect's rulings of 2026-11-24T23:29Z and 2026-11-25T00:15Z: a write of the same URL
// keeps the hold's count; a write of another URL records the release). The test writes INFRA's
// cursor by hand: it owes a read, names stream sequence 42 as the holder, gone back 3 times, keeps
// a sequence skipped past the cap and one held part, and follows either the setting's URL (S1h,
// after a failed read) or no daemon yet (S0h, a message that holds the starting read, which was not
// due when it came). A write of the setting's own URL leaves that cursor exactly as it was, every
// column, and keeps the held part. A write of another URL, made on that same cursor written again,
// resets it, deletes the held part and records 42 as released (released_holders), in both cases, so
// 42 is finished unapplied at its next delivery.
//
// At 0a0ea48a the write of the same URL reset the cursor that followed no daemon yet, dropping 42
// and its count, so 42's next delivery took the hold again from a count of 1. And a write of
// another URL recorded no release for such a cursor, so 42's next delivery was judged as any
// message's.
func TestAWriteOfTheSameURLKeepsTheHoldAndAWriteOfAnotherURLRecordsItsRelease(t *testing.T) {
	const settingURL, anotherURL = "http://127.0.0.1:1", "http://another-daemon.example"
	for _, tc := range []struct {
		name    string
		follows string
		reason  string
	}{
		{name: "a cursor that follows the setting's URL", follows: settingURL, reason: ReadReasonRetry},
		{name: "a cursor that follows no daemon yet", follows: "", reason: ReadReasonStart},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, _ := configured(t, settingURL)
			// columns is INFRA's cursor row whole, every column, as JSON text.
			columns := func() string {
				t.Helper()
				var row string
				if err := database.Pool.QueryRow(t.Context(), `select to_jsonb(c)::text from legion_event_cursors c where project = 'INFRA'`).Scan(&row); err != nil {
					t.Fatalf("read INFRA's cursor: %v", err)
				}
				return row
			}
			// held writes INFRA's cursor holding the read, as the test's doc says, and answers its
			// columns.
			held := func() string {
				t.Helper()
				if _, err := database.Pool.Exec(t.Context(), `delete from legion_event_cursors where project = 'INFRA'`); err != nil {
					t.Fatalf("delete INFRA's cursor: %v", err)
				}
				if _, err := database.Pool.Exec(t.Context(), `
					insert into legion_event_cursors (project, state_url, read_reasons, last_read_started_at, read_holder, read_holds,
						skipped_state_ids, skipped_seqs)
					values ('INFRA', $1, array[$2], now() - interval '10 seconds', 42, 3, array[$3], '{12}')
				`, tc.follows, tc.reason, rigStateID); err != nil {
					t.Fatalf("write INFRA's cursor: %v", err)
				}
				if _, err := database.Pool.Exec(t.Context(), `
					insert into legion_event_parts (project, first_seq, last_seq, run, state_id, at, bytes, events)
					values ('INFRA', 11, 11, $1, $2, now(), 2, '[]')
				`, rigRun, rigStateID); err != nil {
					t.Fatalf("hold a part of INFRA's: %v", err)
				}
				return columns()
			}
			write := func(stateURL string) {
				t.Helper()
				if _, err := PutSettings(t.Context(), database.Pool, "INFRA", stateURL, model.Actor{Kind: "user", ID: "alice@example.com"}); err != nil {
					t.Fatalf("write INFRA's setting naming %s: %v", stateURL, err)
				}
			}

			before := held()
			write(settingURL)
			if after := columns(); after != before {
				t.Errorf("INFRA's cursor after a write of the setting's own URL is %s, want it as it was, %s: the holder 42, its 3 hand-backs, the reason %s and the skipped sequence", after, before, tc.reason)
			}
			if parts := heldPartCount(t, database, "INFRA"); parts != 1 {
				t.Errorf("%d parts held after a write of the setting's own URL, want the one held before it", parts)
			}

			held()
			write(anotherURL)
			c := cursorOf(t, database, "INFRA")
			if c.readOwed || c.readHolder != nil || c.readHolds != 0 || !slices.Equal(c.releasedHolders, []int64{42}) {
				t.Errorf("INFRA's cursor after a write of another URL is %s, want it reset, nothing owed, no holder and no hand-back counted, with 42 recorded as released", c)
			}
			if parts := heldPartCount(t, database, "INFRA"); parts != 0 {
				t.Errorf("%d parts held after a write of another URL, want none", parts)
			}
		})
	}
}

// TestTheReleasedHoldersKeepTheNewestEight: R3-i bounds how long one message holds the durable's
// ack floor at stateMaxHolds hand-backs, about 11 minutes. A write of the setting that names
// another URL ends the hold of the message that holds the owed read, and the cursor records that
// message as released until it comes back (released_holders), the newest last, 8 at the most
// (maxReleasedHolders, read-rule-cases row 6.3b). The test writes INFRA's cursor by hand: it
// follows the setting's URL, owes the read for retry and names stream sequence 109 as its holder,
// gone back twice, and it records the eight messages 101 to 108 as released, none of which has
// come back. A write of another URL, the ninth release, records 109 as the newest and drops the
// oldest record, 101: the cursor then records 102 to 109. A write of a third URL after it finds no
// holder and keeps those eight as they are.
//
// The test catches a cursor that keeps fewer than eight, which after the write of another URL
// records fewer than 102 to 109; one that drops the earlier records when it adds one, which then
// records 109 alone, as the cursor at 8a18f054 did with its one released_holder; and one that
// records more than eight, which then keeps 101 too.
func TestTheReleasedHoldersKeepTheNewestEight(t *testing.T) {
	const settingURL, anotherURL, thirdURL = "http://127.0.0.1:1", "http://another-daemon.example", "http://third-daemon.example"
	const holder = 109
	database, _ := configured(t, settingURL)
	released := []int64{101, 102, 103, 104, 105, 106, 107, 108}
	if _, err := database.Pool.Exec(t.Context(), `
		insert into legion_event_cursors (project, state_url, read_reasons, last_read_started_at, read_holder, read_holds, released_holders)
		values ('INFRA', $1, '{retry}', now() - interval '10 seconds', $2, 2, $3)
	`, settingURL, holder, released); err != nil {
		t.Fatalf("write INFRA's cursor: %v", err)
	}
	write := func(stateURL string) {
		t.Helper()
		if _, err := PutSettings(t.Context(), database.Pool, "INFRA", stateURL, model.Actor{Kind: "user", ID: "alice@example.com"}); err != nil {
			t.Fatalf("write INFRA's setting naming %s: %v", stateURL, err)
		}
	}

	write(anotherURL)
	want := []int64{102, 103, 104, 105, 106, 107, 108, 109}
	if c := cursorOf(t, database, "INFRA"); !slices.Equal(c.releasedHolders, want) || c.readHolder != nil || c.readHolds != 0 || c.readOwed {
		t.Errorf("INFRA's cursor after a ninth write of another URL that found a holder is %s, want %v recorded as released: the newest 8, the oldest, 101, dropped and the holder 109 last; and no holder, no hand-back counted and nothing owed",
			c, want)
	}
	write(thirdURL)
	if c := cursorOf(t, database, "INFRA"); !slices.Equal(c.releasedHolders, want) {
		t.Errorf("INFRA's cursor after a write of a third URL that found no holder is %s, want the %v recorded before it kept", c, want)
	}
}

// TestTwoFirstWritesOfOneURLTakeTurnsAndTheSecondKeepsTheHold: a write of the setting's own URL,
// while a message holds the owed read, leaves the cursor as it is (read-rule-cases row 6.1b), so a
// write must read the URL the setting named before it under a lock that the write before it held,
// a project's first write included. The test holds a first write of LINUX's setting open: one
// transaction inserts the setting, naming the daemon, and a cursor that follows no daemon yet,
// owes the starting read and names stream sequence 42 as its holder, gone back 3 times, as a first
// write whose starting read a message held would leave it. While that transaction is open,
// PutSettings writes the same URL. It must wait for the first write, which then commits, and then
// find the URL the first wrote: the cursor stays as it was, holder 42 and its 3 hand-backs.
//
// At 3ff616dd PutSettings read the URL before its write with `for update`, which locks no row of a
// project that has no setting, so the second write read none, wrote behind the first, and reset
// the cursor as a write of another URL would: it recorded 42 as released and dropped its count.
func TestTwoFirstWritesOfOneURLTakeTurnsAndTheSecondKeepsTheHold(t *testing.T) {
	const settingURL = "http://127.0.0.1:1"
	database, _ := configured(t, settingURL)
	seedProject(t, database, "LINUX")
	first, err := database.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("open the first write's transaction: %v", err)
	}
	defer func() { _ = first.Rollback(context.Background()) }()
	if _, err := first.Exec(t.Context(), `
		insert into legion_settings (project, state_url, created_by) values ('LINUX', $1, '{"kind":"user","id":"alice@example.com"}')
	`, settingURL); err != nil {
		t.Fatalf("write LINUX's first setting: %v", err)
	}
	if _, err := first.Exec(t.Context(), `
		insert into legion_event_cursors (project, state_url, read_reasons, read_holder, read_holds) values ('LINUX', '', '{start}', 42, 3)
	`); err != nil {
		t.Fatalf("write LINUX's cursor: %v", err)
	}

	second := make(chan error, 1)
	go func() {
		_, err := PutSettings(context.Background(), database.Pool, "LINUX", settingURL, model.Actor{Kind: "user", ID: "bob@example.com"})
		second <- err
	}()
	// A wait on another transaction's insert is a wait on its transaction id, a lock pg_locks names
	// no database for, so the wait is found by the waiting backend's database.
	testwait.Eventually(t, "the second write waits for the first", func() bool {
		var waits int
		if err := database.Pool.QueryRow(t.Context(), `
			select count(*) from pg_locks l join pg_stat_activity a on a.pid = l.pid
			where not l.granted and a.datname = current_database()
		`).Scan(&waits); err != nil {
			t.Fatalf("read the waits on the database: %v", err)
		}
		return waits > 0
	})
	if err := first.Commit(t.Context()); err != nil {
		t.Fatalf("commit the first write: %v", err)
	}
	if err := <-second; err != nil {
		t.Fatalf("write LINUX's setting a second time with the same URL: %v", err)
	}
	if c := cursorOf(t, database, "LINUX"); c.stateURL != "" || c.readHolder == nil || *c.readHolder != 42 || c.readHolds != 3 ||
		!slices.Equal(c.readReasons, []string{ReadReasonStart}) || len(c.releasedHolders) != 0 {
		t.Errorf("LINUX's cursor after the second write of the URL the first wrote is %s, want it as the first left it: following no daemon yet, the starting read owed and held by 42, gone back 3 times, and no release recorded", c)
	}
}

// daemonStateIn is daemonState as a daemon of its own serves it: in the store store, under the run
// run.
func daemonStateIn(project string, stateSeq int, issues, store, run string) string {
	return strings.Replace(strings.Replace(daemonState(project, stateSeq, issues), rigStateID, store, 1), rigRun, run, 1)
}

// inPhase is planning("INFRA") with INFRA-1 in phase.
func inPhase(phase string) string {
	return strings.Replace(planning("INFRA"), `"planning"`, fmt.Sprintf("%q", phase), 1)
}

// pointAt writes INFRA's setting naming daemon's URL, signals the write's wake, and waits until
// reads whole reads of every project are recorded: the wake's starting read of daemon among them.
// Whole reads of a project start ReadRetryAfter apart whatever their reasons (read-rule-cases v11),
// so it then moves the intake's clock past that read's minute, and the next read a message calls
// for is due at once, as the tests that point INFRA so expect.
func (r *intakeRig) pointAt(daemon *fakeDaemon, reads int) {
	r.t.Helper()
	if _, err := PutSettings(r.t.Context(), r.database.Pool, "INFRA", daemon.URL, model.Actor{Kind: "user", ID: "alice@example.com"}); err != nil {
		r.t.Fatalf("point INFRA at %s: %v", daemon.URL, err)
	}
	r.wake <- struct{}{}
	r.awaitWholeReads(reads)
	r.clock.advance(ReadRetryAfter + time.Second)
}
