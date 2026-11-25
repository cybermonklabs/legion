package docs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testcleanup"
	"github.com/sjawhar/envoy/internal/testwait"
)

// Room queries (room_query.go) are how one process asks every other one about a document room: a
// flush waits for the other process's room to store what it applied, and a share hands over what
// it holds and has not stored, with who is connected there. The tests here run two
// services on one database, each on a pool of its own and each with its room sync running, as two
// envoy-dispatch processes do. Task B's room appends pass a gate the test holds (gatedAppends), so
// an update its room applied stays unstored for as long as a test needs.

// newQueryProcesses is the two-process pair (processPair, process_pair_test.go) the room
// queries are asked over: the lost-edit document, both processes' room sync running, and task B's
// room resident, while task A holds none. Both processes run on the returned fake clock, set before
// their room sync starts, so an asker's wait and a flush's timeout run out only where the test
// moves it. Their settlement timers run on that clock too (scheduleSettleAfterLocked), each armed
// querySettleDelay off, past any wait a query arms: queryArms reads the clock without them.
func newQueryProcesses(t *testing.T) (processPair, *clocktest.Fake) {
	t.Helper()
	pair := newSeededProcessPair(t, lostEditDocument)
	clock := clocktest.New(time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC))
	pair.taskA.clock, pair.taskB.clock = clock, clock
	pair.taskA.settle, pair.taskB.settle = querySettleDelay, querySettleDelay
	pair.runRoomSync(t)
	if err := pair.taskB.warmLiveDocument(context.Background(), pair.artifactID); err != nil {
		t.Fatalf("load task B's room: %v", err)
	}
	return pair, clock
}

// querySettleDelay is the settle delay of newQueryProcesses's services: the longest duration there
// is, so a settlement timer stands centuries off on the clock, and no row moves the clock to it.
// A query's own waits end by the test binary's deadline at the latest, and under no deadline a
// quarter of this delay off (answerWait), short of the half that queryArms reads them by.
const querySettleDelay = time.Duration(1<<63 - 1)

// queryArms is what the room queries hold armed on clock, newQueryProcesses's: every deadline on
// it but the settlement timers, which stand past half of querySettleDelay from its present.
func queryArms(clock *clocktest.Fake) []time.Time {
	horizon := clock.Now().Add(querySettleDelay / 2)
	return slices.DeleteFunc(clock.Armed(), func(at time.Time) bool { return at.After(horizon) })
}

// awaitQueryArms is clocktest.Fake.AwaitArmed for the room queries' arms alone (queryArms).
func awaitQueryArms(t *testing.T, clock *clocktest.Fake, what string, n int) []time.Time {
	t.Helper()
	return testwait.Until(t, fmt.Sprintf("%s (at least %d of the room queries' timers armed on the fake clock)", what, n), func() (queryArmsReading, bool) {
		read := queryArmsReading{now: clock.Now(), all: clock.Armed(), arms: queryArms(clock)}
		return read, len(read.arms) >= n
	}).arms
}

// queryArmsReading is what awaitQueryArms last read: the fake's present, every timer armed on it,
// and the ones queryArms keeps, so a give-up names where the fake stood and what was armed.
type queryArmsReading struct {
	now       time.Time
	all, arms []time.Time
}

func (r queryArmsReading) String() string {
	return fmt.Sprintf("%d of the room queries' timers armed (%v) of %d on the fake (%v), the fake at %s",
		len(r.arms), r.arms, len(r.all), r.all, r.now.Format(time.RFC3339Nano))
}

// roomQueryResult is what one roomQueries.ask returned.
type roomQueryResult struct {
	answers []roomAnswer
	asked   bool
	err     error
}

// askOnTheSide runs service's kind query about the pair's document on a goroutine of its own, so
// the test can move the clock or open the gate while it waits, and hands back its result.
func (pair processPair) askOnTheSide(service *Service, kind string) <-chan roomQueryResult {
	result := make(chan roomQueryResult, 1)
	go func() {
		answers, asked, err := service.roomQueries.ask(context.Background(), pair.database.Pool, pair.artifactID, kind)
		result <- roomQueryResult{answers, asked, err}
	}()
	return result
}

// awaitingOnly waits until service's one query under way waits on the members want names and on
// no other: every other member's answer has arrived whole.
func awaitingOnly(t *testing.T, service *Service, want ...string) {
	t.Helper()
	slices.Sort(want)
	testwait.Until(t, fmt.Sprintf("the query to wait on %v alone", want), func() ([]string, bool) {
		service.roomQueries.mu.Lock()
		defer service.roomQueries.mu.Unlock()
		var waiting []string
		for _, pending := range service.roomQueries.pending {
			for member := range pending.waiting {
				waiting = append(waiting, member)
			}
		}
		slices.Sort(waiting)
		return waiting, len(service.roomQueries.pending) == 1 && slices.Equal(waiting, want)
	})
}

// captureWarnings sends every log line at WARN and above to the returned log until the test ends,
// and gives the log package its output back then (logtest.SetDefault).
func captureWarnings(t *testing.T) *storetest.LockedLog {
	t.Helper()
	logs := &storetest.LockedLog{}
	logtest.SetDefault(t, slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	return logs
}

// storedDocumentWith is the stored document's rendering with updates applied over it.
func storedDocumentWith(t *testing.T, database *store.Store, artifactID string, updates ...[]byte) string {
	t.Helper()
	loaded, err := NewPgVersioned(database).Load(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("load the stored document: %v", err)
	}
	doc := newDocumentCopy()
	for _, update := range append([][]byte{loaded.Update}, updates...) {
		if len(update) == 0 {
			continue
		}
		if err := crdt.ApplyUpdateV1(doc, update, nil); err != nil {
			t.Fatalf("apply an update to the stored document: %v", err)
		}
	}
	tree, err := treeOf(doc)
	if err != nil {
		t.Fatalf("read the document: %v", err)
	}
	markdown, err := renderTree(tree)
	if err != nil {
		t.Fatalf("render the document: %v", err)
	}
	return markdown
}

// A flush is answered only once the other process's room has stored what it applied: task B's
// room applied bob's edit and holds its append at the gate, and task A's flush returns once the
// gate opens, with the edit stored and task B's answer saying it holds the room. It names no
// actors: only a share's asker reads them. The clock never moves, so task A's wait cannot run out:
// the answer it returns is task B's.
func TestAFlushWaitsForTheOtherProcessesHeldAppend(t *testing.T) {
	t.Parallel()
	pair, clock := newQueryProcesses(t)
	bob := model.Actor{Kind: "user", ID: "bob"}
	pair.heldEditOnB(t, 1, bob, replaceRun("Bravo paragraph.", "Bravo paragraph, by bob."))
	if stored := storedMarkdown(t, pair.database, pair.artifactID); strings.Contains(stored, "by bob") {
		t.Fatalf("the store holds bob's edit %q while its append is held, so nothing here is tested", stored)
	}

	result := pair.askOnTheSide(pair.taskA, roomQueryFlush)
	// Task A waits for the answer, and task B's flush waits for its append, each with its deadline
	// armed on the clock. Task B answers only once its flush's wait for its room's updates to be
	// stored returns (awaitStored), so the test waits until task B's flush is inside that wait, a
	// state it reads from the goroutines rather than one instant after the arms: a flush that
	// answers without waiting for the append is never seen there, and its answer ends the wait.
	awaitQueryArms(t, clock, "task A's wait for the answers and task B's flush timeout", 2)
	testwait.Eventually(t, "task B's flush to wait for its held append to be stored (awaitStored)", func() bool {
		select {
		case early := <-result:
			t.Fatalf("task A's flush returned %+v while task B's append was held: it did not wait for the append", early)
		default:
		}
		return goroutinesIn("docs.awaitStored(", "docs.(*roomQueries).query") > 0
	})
	pair.gateB.release()
	flushed := testwait.Receive(t, "task A's flush to return once task B's append is stored", result)
	answers, asked, err := flushed.answers, flushed.asked, flushed.err
	if err != nil || !asked {
		t.Fatalf("task A's flush = asked %t, %v; want task B asked", asked, err)
	}
	if len(answers) != 1 || answers[0].process != pair.taskB.ProcessID() || !answers[0].holds {
		t.Fatalf("task A's flush answers = %+v, want task B's one, holding the room", answers)
	}
	if len(answers[0].actors) != 0 {
		t.Fatalf("task B's flush answer names actors %+v, which no asker of a flush reads", answers[0].actors)
	}
	if stored := storedMarkdown(t, pair.database, pair.artifactID); !strings.Contains(stored, "by bob") {
		t.Fatalf("the store reads %q once task B answered the flush, without bob's edit", stored)
	}
}

// A share is answered at once, appending nothing, with the updates the other process's room holds
// and the store does not - here bob's paragraph, whose append is under way and held at the gate -
// and an answer that large crosses in several notifications and is put back together whole. The
// clock never moves, so task A's wait cannot run out: the answer it returns came whole.
func TestAShareReturnsTheOtherProcessesInFlightAppend(t *testing.T) {
	t.Parallel()
	pair, _ := newQueryProcesses(t)
	bob := model.Actor{Kind: "user", ID: "bob"}
	paragraph := "Bob's long paragraph " + strings.Repeat("with many words ", 1200) + "ends."
	pair.heldEditOnB(t, 1, bob, appendBlocks(t, paragraph+"\n"))

	answers, asked, err := pair.taskA.roomQueries.ask(testwait.Context(t), pair.database.Pool, pair.artifactID, roomQueryShare)
	if err != nil || !asked {
		t.Fatalf("task A's share = asked %t, %v; want task B asked", asked, err)
	}
	if len(answers) != 1 || !answers[0].holds || answers[0].tooLarge {
		t.Fatalf("task A's share answers = %+v, want task B's one, holding the room", answers)
	}
	if len(answers[0].updates) <= roomSyncPayloadLimit {
		t.Fatalf("task B shared %d bytes, which fit one notification; the test wants an answer in parts", len(answers[0].updates))
	}
	if !reflect.DeepEqual(answers[0].actors, []model.Actor{bob}) {
		t.Fatalf("task B answered actors %+v, want bob", answers[0].actors)
	}
	if stored := storedMarkdown(t, pair.database, pair.artifactID); strings.Contains(stored, "Bob's long paragraph") {
		t.Fatalf("the store holds bob's paragraph while its append is held: the share appended it")
	}
	if shared := storedDocumentWith(t, pair.database, pair.artifactID, answers[0].updates); !strings.Contains(shared, paragraph) {
		t.Fatalf("the stored document with task B's shared updates reads %q, without bob's paragraph", shared)
	}
	pair.gateB.release()
	waitForLandedAppends(t, pair.taskB, pair.artifactID)
	if stored := storedMarkdown(t, pair.database, pair.artifactID); strings.Count(stored, "Bob's long paragraph") != 1 {
		t.Fatalf("once the gate opened the store reads %q; want bob's paragraph stored once, by task B's own append", stored)
	}
}

// deliverRoomQuery hands service's query lane one kind query about room from sender, as room
// sync does when the notification of it arrives (roomQueries.query), so a test of what answering
// it does waits on no notification.
func deliverRoomQuery(t *testing.T, service *Service, sender, room, kind string) {
	t.Helper()
	payload, err := json.Marshal(roomQuery{ID: uuid.NewString(), Sender: sender, Room: room, Kind: kind})
	if err != nil {
		t.Fatalf("encode the %s query: %v", kind, err)
	}
	service.roomQueries.query(context.Background(), string(payload))
}

// A room's browser appends linger for the room's next keystrokes before they go (roomAppendLinger),
// and a flush has roomQueryWait to answer in, spent by an asker holding whatever it asked the
// flush for. So a query of another process asks the room's appends to go before it waits for
// them, as a wait for the room's own appends does (waitForRoomIdle): every linger already
// waiting is woken, no update the room recorded before the query lingers any more, and none
// lingers while the answer waits (askRoomAppends).
func TestARoomQueryOfAnotherProcessEndsAPendingBatchesLinger(t *testing.T) {
	t.Parallel()
	pair, _ := newQueryProcesses(t)
	pair.heldEditOnB(t, 1, model.Actor{Kind: "user", ID: "bob"}, replaceRun("Bravo paragraph.", "Bravo paragraph, by bob."))
	defer pair.gateB.release()
	state := pair.taskB.lockExistingState(pair.artifactID)
	if len(state.updateClasses) == 0 {
		pair.taskB.unlockState(pair.artifactID, state)
		t.Fatal("task B's room queued no class for bob's edit, so no batch of it can linger")
	}
	class := state.updateClasses[len(state.updateClasses)-1]
	// The moment task B's room recorded bob's update, on the product's own clock: the batch lingers
	// while it is after the room's last ask of its appends (roomBatchThrough).
	recorded := class.observed
	lingering := state.appendLinger.wait()
	pair.taskB.unlockState(pair.artifactID, state)

	deliverRoomQuery(t, pair.taskB, pair.taskA.ProcessID(), pair.artifactID, roomQueryFlush)

	testwait.Receive(t, "the query to wake task B's lingering appends (a batch of its keystrokes would hold the answer up for a linger)", lingering)
	state = pair.taskB.lockExistingState(pair.artifactID)
	flushed, waiters := state.appendsFlushed, state.appendWaiters
	pair.taskB.unlockState(pair.artifactID, state)
	if recorded.After(flushed) {
		t.Fatalf("task B recorded its appends asked for at %v, before its room recorded bob's update at %v: the query asked for none, and an update recorded before it lingers all the same", flushed, recorded)
	}
	if waiters == 0 {
		t.Fatal("task B counts no waiter on its appends while it answers the flush, so a keystroke its room takes meanwhile lingers while the answer waits")
	}
	if _, wake, wait := pair.taskB.roomBatchThrough(pair.artifactID, class, true); wake != nil {
		t.Fatalf("the batch of an update task B's room recorded before the query lingers another %v", wait)
	}
}

// Every update of a pending batch stays listed as its room instance's unstored work until its own
// append has returned, so a share answers a query that arrives while the batch is still to go with
// all of them: here the update whose append waits at the gate and the keystroke queued behind it,
// whose call has not taken it yet. Once the gate opens, both are stored once and the instance
// lists neither: a stored update is unlisted exactly once, by its own call.
func TestAShareHoldsEveryQueuedUpdateOfAPendingBatch(t *testing.T) {
	t.Parallel()
	pair, _ := newQueryProcesses(t)
	bob := model.Actor{Kind: "user", ID: "bob"}
	pair.heldEditOnB(t, 1, bob, appendBlocks(t, "Bob's first paragraph.\n"))
	editAsPeer(t, pair.taskB, pair.artifactID, appendBlocks(t, "Bob's second paragraph.\n"))

	answers, asked, err := pair.taskA.roomQueries.ask(testwait.Context(t), pair.database.Pool, pair.artifactID, roomQueryShare)
	if err != nil || !asked {
		t.Fatalf("task A's share = asked %t, %v; want task B asked", asked, err)
	}
	if len(answers) != 1 || !answers[0].holds || answers[0].tooLarge {
		t.Fatalf("task A's share answers = %+v, want task B's one, holding the room", answers)
	}
	shared := storedDocumentWith(t, pair.database, pair.artifactID, answers[0].updates)
	for _, paragraph := range []string{"Bob's first paragraph.", "Bob's second paragraph."} {
		if !strings.Contains(shared, paragraph) {
			t.Fatalf("the stored document with task B's shared updates reads %q, without %q: an update of its pending batch is listed nowhere", shared, paragraph)
		}
	}

	pair.gateB.release()
	waitForLandedAppends(t, pair.taskB, pair.artifactID)
	instance := pair.taskB.instanceOf(pair.taskB.srv.GetDoc(pair.artifactID))
	if instance == nil {
		t.Fatal("task B holds no room instance of the document once its batch was stored")
	}
	if listed := instance.unstoredUpdates(); len(listed) != 0 {
		t.Fatalf("task B's room lists %d of its stored updates as unstored, want none: each leaves the list with its own call", len(listed))
	}
	stored := storedMarkdown(t, pair.database, pair.artifactID)
	for _, paragraph := range []string{"Bob's first paragraph.", "Bob's second paragraph."} {
		if count := strings.Count(stored, paragraph); count != 1 {
			t.Fatalf("the store reads %q, holding %q %d times, want once", stored, paragraph, count)
		}
	}
}

// What a share holds past roomShareLimit is not sent: the answer says too_large, carries no
// updates, and the asker logs a WARN and judges without them.
func TestAShareTooLargeForOneQueryAnswersTooLarge(t *testing.T) {
	pair, _ := newQueryProcesses(t)
	pair.heldEditOnB(t, 1, model.Actor{Kind: "user", ID: "bob"}, appendBlocks(t, strings.Repeat("Bob's huge paragraph. ", 4000)+"\n"))
	logs := captureWarnings(t)

	answers, _, err := pair.taskA.roomQueries.ask(testwait.Context(t), pair.database.Pool, pair.artifactID, roomQueryShare)
	if err != nil {
		t.Fatalf("task A's share: %v", err)
	}
	if len(answers) != 1 || !answers[0].holds || !answers[0].tooLarge || len(answers[0].updates) != 0 {
		t.Fatalf("task A's share answers = %+v, want task B's one, holding the room, too large and with no updates", answers)
	}
	if warned := logs.Lines("more not-yet-stored updates than one query shares"); !strings.Contains(warned, pair.taskB.ProcessID()) {
		t.Fatalf("task A logged no WARN naming task B for its too-large share; logged:\n%s", logs.String())
	}
}

// A member that never answers - an older build, or a process that has just died - costs a query
// the wait and no more, and a WARN naming it. The silent member here holds the listener
// connection's name, which is all membership reads, and listens to nothing. Each query stays open
// with task B's answer in, waiting on the silent member alone, until the clock passes the wait
// task A armed when it sent the query; then it returns task B's answer.
func TestAMemberThatNeverAnswersCostsTheWaitAndAWarn(t *testing.T) {
	pair, clock := newQueryProcesses(t)
	// Production's wait, which this test holds (nil is roomQueryWait): the pair's own is
	// answerWait, the shared wait's (newSeededProcessPair).
	pair.taskA.roomQueries.wait = nil
	silent := uuid.NewString()
	conn, err := pair.database.Pool.Dedicated(context.Background(), ListenerApplicationPrefix+silent)
	if err != nil {
		t.Fatalf("connect the silent member: %v", err)
	}
	testcleanup.Add(t, func() { _ = conn.Close(context.Background()) })
	logs := captureWarnings(t)

	for _, kind := range []string{roomQueryHolds, roomQueryFlush, roomQueryShare} {
		asked := clock.Now()
		result := pair.askOnTheSide(pair.taskA, kind)
		awaitingOnly(t, pair.taskA, silent)
		// Task B's flush answer cancels its timeout and only then counts itself off its room's
		// append waiters (askRoomAppends' done), deferred in that order, so once task B counts no
		// waiter its timeout is gone, whatever task B's goroutine was doing when its answer reached
		// task A. A holds or a share arms no such timeout.
		testwait.Until(t, kind+": task B to let go of its flush answer's timeout", func() (int, bool) {
			state := pair.taskB.lockExistingState(pair.artifactID)
			if state == nil {
				return 0, true
			}
			defer pair.taskB.unlockState(pair.artifactID, state)
			return state.appendWaiters, state.appendWaiters == 0
		})
		// Every answer's send arms roomSyncSendTimeout on task B's clock, this fake, and lets it go
		// as the send returns (roomQueries.send), which can be after its last notification reached
		// task A. Task A answers none of its own queries and the silent member answers nothing, so
		// every send is task B's, its bound armed for asked+roomSyncSendTimeout until it lets go:
		// once the room queries' timers hold task A's wait, armed for roomQueryWait from asked, and
		// no other, task B holds none of its own. A give-up names what the fake held, and where.
		want := asked.Add(roomQueryWait)
		testwait.Until(t, fmt.Sprintf("%s: with only the silent member left to answer, task B to let go of its send's bound and the room queries' timers on the fake clock to hold task A's wait alone, armed for %v", kind, want), func() (queryArmsReading, bool) {
			read := queryArmsReading{now: clock.Now(), all: clock.Armed(), arms: queryArms(clock)}
			return read, slices.EqualFunc(read.arms, []time.Time{want}, time.Time.Equal)
		})
		select {
		case early := <-result:
			t.Fatalf("%s returned %+v before its wait ran out, with a member yet to answer", kind, early)
		default:
		}
		clock.Set(want)
		waited := testwait.Receive(t, kind+": task A's query to return once its wait ran out", result)
		if waited.err != nil || !waited.asked {
			t.Fatalf("%s: asked %t, %v; want the members asked", kind, waited.asked, waited.err)
		}
		if len(waited.answers) != 1 || waited.answers[0].process != pair.taskB.ProcessID() {
			t.Fatalf("%s answers = %+v, want task B's alone", kind, waited.answers)
		}
	}
	if warned := logs.Lines("did not answer a document room query"); strings.Count(warned, silent) != 3 {
		t.Fatalf("task A logged %q; want a WARN naming the silent member for each of its three queries", warned)
	}
}

// A flush is never sent while the asker holds the document's advisory lock, which every other
// process's append waits on: it would wait out every member. The query reads the lock from the
// database, skips the flush at once and logs it at ERROR.
func TestAFlushIsNeverSentUnderTheDocumentsAdvisoryLock(t *testing.T) {
	pair, clock := newQueryProcesses(t)
	logs := captureWarnings(t)
	ctx := context.Background()
	tx, err := pair.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, locked, err := readRoomQueryMembers(ctx, tx, pair.artifactID, true); err != nil || locked {
		t.Fatalf("before the lock, the members' read says locked %t, %v; want unlocked", locked, err)
	}
	if err := lockDocumentRoom(ctx, tx, pair.artifactID); err != nil {
		t.Fatalf("take the document's advisory lock: %v", err)
	}
	if _, locked, err := readRoomQueryMembers(ctx, tx, pair.artifactID, true); err != nil || !locked {
		t.Fatalf("under the lock, the members' read says locked %t, %v; want locked", locked, err)
	}
	if _, locked, err := readRoomQueryMembers(ctx, tx, uuid.NewString(), true); err != nil || locked {
		t.Fatalf("another document's lock read says locked %t, %v; want unlocked", locked, err)
	}

	// A flush sent under the lock would wait for its answers, its wait armed on a clock that never
	// moves: the query either returns or arms that wait.
	done := make(chan roomQueryResult, 1)
	go func() {
		answers, asked, err := pair.taskA.roomQueries.ask(ctx, tx, pair.artifactID, roomQueryFlush)
		done <- roomQueryResult{answers, asked, err}
	}()
	type outcome struct {
		result   *roomQueryResult
		deadline []time.Time
	}
	// A try that finds neither reads where the fake stands and every timer it holds armed.
	var ended outcome
	testwait.Until(t, "the flush under the lock to return, or to arm a wait for its answers", func() (string, bool) {
		select {
		case result := <-done:
			ended = outcome{result: &result}
			return fmt.Sprintf("the flush returned %+v", result), true
		default:
		}
		if armed := queryArms(clock); len(armed) > 0 {
			ended = outcome{deadline: armed}
			return fmt.Sprintf("the room queries' timers armed at %v", armed), true
		}
		return fmt.Sprintf("the flush has not returned, and no room query's timer is armed; the fake at %s holds %v armed",
			clock.Now().Format(time.RFC3339Nano), clock.Armed()), false
	})
	if ended.result == nil {
		t.Fatalf("a flush under the lock armed a wait for its answers, until %v; want it skipped at once", ended.deadline)
	}
	if ended.result.err != nil || len(ended.result.answers) != 0 {
		t.Fatalf("a flush under the lock = %+v, %v; want it skipped", ended.result.answers, ended.result.err)
	}
	if logged := logs.Lines("level=ERROR"); !strings.Contains(logged, "flush was asked for while holding") {
		t.Fatalf("a flush under the lock logged no ERROR; logged:\n%s", logs.String())
	}
}

// An agent's write on task A inserts into a paragraph that bob, on task B, deletes while the
// write is open: task A's room never sees the delete, and task B's append of it waits at the gate.
// The write's version capture shares task B's unstored updates into a scratch copy of its fork and
// refuses the write as lost, naming everyone connected on either process - alice on task A, bob
// (in two tabs) on task B - once each, and not the writer, connected on task B too. The refused
// write leaves bob's delete to be stored, once the gate opens.
func TestALostWriteNamesTheParticipantsOfEveryProcess(t *testing.T) {
	t.Parallel()
	pair, _ := newQueryProcesses(t)
	alice := model.Actor{Kind: "user", ID: "alice"}
	bob := model.Actor{Kind: "user", ID: "bob"}
	if err := pair.taskA.warmLiveDocument(context.Background(), pair.artifactID); err != nil {
		t.Fatalf("load task A's room: %v", err)
	}
	pair.taskA.addConnection(pair.artifactID, 1, alice)
	pair.taskB.addConnection(pair.artifactID, 2, bob)
	pair.taskB.addConnection(pair.artifactID, 3, lostEditAgent)

	edit := startEditInFlight(t, pair.taskA, pair.artifactID, model.EditOp{
		Op: "replace", Find: "Alpha paragraph.", With: "Alpha paragraph edited.",
	})
	pair.heldEditOnB(t, 4, bob, withoutTopLevelBlock("Alpha paragraph."))

	_, err := pair.taskA.NamedVersion(edit.ctx, pair.artifactID, "the agent's version", lostEditAgent)
	var lost *ErrEditLost
	if !errors.As(err, &lost) {
		t.Fatalf("the agent's version after bob deleted its paragraph on task B = %v, want the write refused as lost", err)
	}
	if !reflect.DeepEqual(lost.Ops, []int{0}) {
		t.Fatalf("lost operations = %v, want [0]", lost.Ops)
	}
	if want := []model.Actor{alice, bob}; !reflect.DeepEqual(lost.Participants, want) {
		t.Fatalf("participants = %+v, want %+v: the connected actors of both processes, once each, without the writer", lost.Participants, want)
	}

	edit.ledger.Discard()
	if err := edit.tx.Rollback(context.Background()); err != nil {
		t.Fatalf("roll back the refused write: %v", err)
	}
	pair.gateB.release()
	waitForLandedAppends(t, pair.taskB, pair.artifactID)
	if stored := storedMarkdown(t, pair.database, pair.artifactID); strings.Contains(stored, "Alpha") {
		t.Fatalf("the store reads %q once the refused write rolled back and task B's gate opened; want bob's delete stored", stored)
	}
}

// Before a write forks, every other process holding the document's room stores what it applied:
// bob deleted the paragraph on task B, whose append is held, and an agent's edit of that paragraph
// through task A, which holds no room, waits for the append rather than forking a store without
// the delete, and is refused as an edit whose target is gone.
func TestALiveWriteForksOnlyOnceTheOtherProcessHasStoredItsRoom(t *testing.T) {
	t.Parallel()
	pair, clock := newQueryProcesses(t)
	pair.heldEditOnB(t, 1, model.Actor{Kind: "user", ID: "bob"}, withoutTopLevelBlock("Alpha paragraph."))
	written := make(chan error, 1)
	go func() {
		_, err := joinedApplyOps(pair.taskA, pair.artifactID, []model.EditOp{{Op: "replace", Find: "Alpha paragraph.", With: "Alpha edited."}}, lostEditAgent, nil)
		written <- err
	}()
	// The write's flush is under way before the append goes: task A waits for the answer and task
	// B's flush for its append.
	awaitQueryArms(t, clock, "the write's wait for task B's flush and task B's flush timeout", 2)
	pair.gateB.release()
	err := testwait.Receive(t, "the agent's write through task A to end", written)
	var gone *ErrQuoteNotFound
	if !errors.As(err, &gone) {
		t.Fatalf("the agent's edit through task A of the paragraph bob deleted on task B = %v, want ErrQuoteNotFound: the write forked a store without the delete", err)
	}
	if pair.taskA.roomHeld(pair.artifactID) {
		t.Fatal("the refused write left a room of the document on task A")
	}
}

// A write's verdict after its commit is judged on the document at the stored head: here the write
// went through task A, which holds no room, and bob deleted its paragraph on task B between the
// write's commit and its publish, with task B's append of the delete held. The second flush waits
// for that append, and the write's lost operations name the one bob removed. The write loads no
// room on task A, publishing into none.
//
// The write runs on a goroutine of its own, and every wait of the row is the test goroutine's: the
// write's hook parks it once it has committed, before its publish, until the test has made bob's
// delete and lets it go (letGo, which a test that ended first calls in its cleanup). The publish's
// loss check is bounded on task A's clock, this fake, which never moves here, so the check ends
// only once task B answers; a check that never does fails the row's wait for the write by name.
func TestAPublishedWritesLossIsJudgedAtTheStoredHeadAfterTheOtherProcessFlushes(t *testing.T) {
	t.Parallel()
	pair, clock := newQueryProcesses(t)
	bob := model.Actor{Kind: "user", ID: "bob"}
	committed, proceed := make(chan struct{}), make(chan struct{})
	var once, proceeded sync.Once
	letGo := func() { proceeded.Do(func() { close(proceed) }) }
	testcleanup.Add(t, letGo)
	testcleanup.Add(t, pair.gateB.release)
	pair.taskA.afterLiveCommit = func(string) {
		once.Do(func() {
			close(committed)
			<-proceed
		})
	}
	edited := make(chan publishedEdit, 1)
	go func() {
		lost, verdict, err := committedEdit(pair.taskA, pair.artifactID, model.EditOp{Op: "replace", Find: "Alpha paragraph.", With: "Alpha paragraph, extended."})
		edited <- publishedEdit{lost: lost, verdict: verdict, err: err}
	}()
	testwait.Eventually(t, "the agent's edit through task A to commit", func() bool {
		select {
		case got := <-edited:
			t.Fatalf("the agent's edit through task A ended (%+v) before its commit's hook ran", got)
		default:
		}
		select {
		case <-committed:
			return true
		default:
			return false
		}
	})
	pair.heldEditOnB(t, 1, bob, withoutFirstTopLevelBlock)
	// Task B's append opens once the publish's flush waits for it: an append that landed before
	// that wait would be in the fork the write reads, not the loss this row reads.
	//
	// What the fake holds armed here can still hold task B's answer to the write's join flush: its
	// flush timeout and its send's bound (roomQueries.query, send), each armed before that answer
	// left task B and let go only as its goroutine returns, which can be after task A read the
	// answer and committed. Neither is armed again. The publish arms three, in this order: its loss
	// check's bound (recordPublishedLoss, publishedLossTimeout), before it asks task B anything; then
	// task A's wait for the publish's answers; then task B's flush timeout. The first is armed at
	// the fake's moment plus publishedLossTimeout (Fake.WithTimeout), and the fake never moves here,
	// so the starting reading holds that deadline before it is armed: armed as it stands, it could
	// let the wait go while the publish has yet to ask for task B's flush. So the wait is for a
	// deadline this reading does not hold (armedBeyond), which no arm let go of ever makes, in
	// whatever order the join's two go and the publish's three come: task A's wait or task B's
	// flush timeout. Each of those is answerWait from the fake's moment, and answerWait is what is
	// left of the shared wait: read after this reading, it is shorter than every one read before
	// it, and it equals a send's roomSyncSendTimeout only when read 2 s before the shared wait gives
	// up, or publishedLossTimeout only when read 10 s before, when the loss check's bound is armed
	// beside it and the reading's one such deadline matches only one of the two. Under no deadline
	// answerWait is one span, so the join's flush timeout, if this reading holds it, stands at the
	// publish's two deadlines, and matches only one of them. A write that returns before either is
	// armed published without waiting for task B's flush.
	before := append(queryArms(clock), clock.Now().Add(publishedLossTimeout))
	letGo()
	testwait.Until(t, fmt.Sprintf("the publish's wait for task B's flush (a room query's timer armed on the fake clock beyond the %v read at the commit)", before), func() (queryArmsReading, bool) {
		select {
		case got := <-edited:
			t.Fatalf("the agent's edit published (%+v) without waiting for task B's flush of bob's held delete", got)
		default:
		}
		read := queryArmsReading{now: clock.Now(), all: clock.Armed(), arms: queryArms(clock)}
		return read, len(armedBeyond(before, read.arms)) > 0
	})
	pair.gateB.release()
	got := testwait.Receive(t, "the agent's edit through task A to return once its publish judged its loss (recordPublishedLoss)", edited)
	if got.err != nil {
		t.Fatal(got.err)
	}
	if !got.verdict || !reflect.DeepEqual(got.lost, []int{0}) {
		t.Fatalf("the published write's lost operations = %v (verdict %t), want [0]: bob's delete on task B removed its text", got.lost, got.verdict)
	}
	if pair.taskA.roomHeld(pair.artifactID) {
		t.Fatal("the cold write's publish loaded a room on task A")
	}
}

// publishedEdit is what committedEdit returned.
type publishedEdit struct {
	lost    []int
	verdict bool
	err     error
}

// committedEdit commits op as the agent's edit of artifactID through service and returns the
// published write's lost operations, whether it reached a verdict (Ledger.LostOps), and why the
// edit failed. It touches no t, so it runs off the test goroutine.
func committedEdit(service *Service, artifactID string, op model.EditOp) (lost []int, verdict bool, err error) {
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("begin the edit: %w", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	if _, err := service.ApplyOps(joined, artifactID, []model.EditOp{op}, lostEditAgent, nil); err != nil {
		return nil, false, fmt.Errorf("the agent's edit: %w", err)
	}
	if err := ledger.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("commit the agent's edit: %w", err)
	}
	lost, verdict = ledger.LostOps(artifactID)
	return lost, verdict, nil
}

// withoutFirstTopLevelBlock deletes the document's first top-level block, whatever its text: a
// browser's paragraph delete, made in a room that may or may not have taken in a write to it yet.
func withoutFirstTopLevelBlock(tree *pmdoc.Node) *pmdoc.Node {
	return &pmdoc.Node{Type: tree.Type, Attrs: tree.Attrs, Children: slices.Clone(tree.Children[1:])}
}

// A cold write's fork is read under the document's advisory lock, which its transaction takes with
// the head (readFork) and holds until it ends, so the other process cannot store an edit between
// that read and the write's appends: task B's browser edit, made while the write is open, waits on
// that lock, and the version the write renders holds the agent's edit and nothing of bob's. Once
// the write commits, bob's append lands on top of it and the stored document holds both. Read
// without that lock, the write's fork could miss an edit the other process stored under it, and
// the version recorded a cursor over text it did not hold.
func TestAColdWritesVersionIsNotRacedByTheOtherProcessesAppend(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	taskA, _ := newTestServiceInstance(t, database)
	taskB, _ := newTestServiceInstance(t, storetest.SecondProcess(t, database))
	seedServiceText(t, taskA, artifactID, lostEditDocument)
	if err := taskB.warmLiveDocument(context.Background(), artifactID); err != nil {
		t.Fatalf("load task B's room: %v", err)
	}
	waitForLandedAppends(t, taskB, artifactID)
	if taskA.roomHeld(artifactID) {
		t.Fatal("task A holds a room, so the write would not be cold")
	}

	edit := startEditInFlight(t, taskA, artifactID, model.EditOp{Op: "replace", Find: "Alpha paragraph.", With: "Alpha paragraph, by the agent."})
	// Bob types on task B while the write is open: his room takes the edit at once, and its
	// append queues on the document's lock the write holds.
	taskB.addConnection(artifactID, 1, model.Actor{Kind: "user", ID: "bob"})
	if err := browserStyleWrite(taskB, artifactID, replaceRun("Bravo paragraph.", "Bravo paragraph, by bob."), make(chan struct{})); err != nil {
		t.Fatalf("bob's edit on task B: %v", err)
	}
	waitFor(t, "bob's append to queue on the document's advisory lock", func() bool {
		return waitingAdvisoryLocks(t, database) > 0
	})
	if stored := storedMarkdown(t, database, artifactID); strings.Contains(stored, "by bob") {
		t.Fatalf("bob's edit was stored while the write held the document's lock: %q", stored)
	}

	if _, err := taskA.NamedVersion(edit.ctx, artifactID, "both", lostEditAgent); err != nil {
		t.Fatalf("the agent's version: %v", err)
	}
	if err := edit.ledger.Commit(context.Background()); err != nil {
		t.Fatalf("commit the agent's write: %v", err)
	}
	markdown, _ := latestVersionOf(t, database, artifactID)
	if !strings.Contains(markdown, "by the agent") || strings.Contains(markdown, "by bob") {
		t.Fatalf("the cold write's version reads %q; want the agent's edit alone, bob's append being queued behind its lock", markdown)
	}
	waitForLandedAppends(t, taskB, artifactID)
	waitFor(t, "bob's edit to be stored once the write let go of the lock", func() bool {
		return strings.Contains(storedMarkdown(t, database, artifactID), "by bob")
	})
	if stored := storedMarkdown(t, database, artifactID); !strings.Contains(stored, "by the agent") {
		t.Fatalf("the stored document reads %q; want the agent's edit and bob's", stored)
	}
}

// An answer whose notification would pass the payload limit is split into numbered parts, the
// actors and then the updates spread across them, the last flagged; put back together, it is the
// answer that was split.
func TestARoomAnswerTooLargeForOneNotificationIsSplitIntoParts(t *testing.T) {
	t.Parallel()
	actors := make([]model.Actor, 0, 300)
	for index := range 300 {
		actors = append(actors, model.Actor{Kind: "user", ID: fmt.Sprintf("reader-with-a-long-login-%03d", index)})
	}
	updates := []byte(strings.Repeat("an update's bytes ", 2000))
	payloads, err := roomAnswerPayloads(roomAnswerPart{ID: "query", Process: "member", Holds: true}, actors, base64.StdEncoding.EncodeToString(updates))
	if err != nil {
		t.Fatalf("split the answer: %v", err)
	}
	if len(payloads) < 3 {
		t.Fatalf("the answer went in %d notifications, want several", len(payloads))
	}
	parts := &roomAnswerParts{parts: make(map[int]roomAnswerPart), last: -1}
	// Parts may arrive in any order.
	for _, payload := range slices.Backward(payloads) {
		if len(payload) > roomSyncPayloadLimit {
			t.Fatalf("a part of %d bytes passes the %d-byte limit", len(payload), roomSyncPayloadLimit)
		}
		var part roomAnswerPart
		if err := json.Unmarshal([]byte(payload), &part); err != nil {
			t.Fatalf("decode a part: %v", err)
		}
		parts.parts[part.Part] = part
		if part.Last {
			parts.last = part.Part
		}
	}
	if parts.last != len(payloads)-1 {
		t.Fatalf("the last part is numbered %d, want %d", parts.last, len(payloads)-1)
	}
	answer, err := parts.assemble()
	if err != nil {
		t.Fatalf("assemble the answer: %v", err)
	}
	if !answer.holds || answer.process != "member" || !reflect.DeepEqual(answer.actors, actors) || string(answer.updates) != string(updates) {
		t.Fatalf("the assembled answer differs from the one split: holds %t, process %q, %d actors, %d bytes of updates",
			answer.holds, answer.process, len(answer.actors), len(answer.updates))
	}
}
