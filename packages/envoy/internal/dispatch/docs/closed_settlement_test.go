package docs

import (
	"context"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/logtest"
	"github.com/sjawhar/envoy/testwait"
)

// A closed issue's document settles as an open one's does (settleRoomWithin), whatever armed its
// settlement: a browser's append the store admitted inside the close margin, a room's load, the
// resumption, or the resumption after the close stopped a settlement that was armed. A settlement
// whose room's stored history was replaced under it reloads the room and goes on, and one whose
// room took an update the store refused after the close margin saves nothing of that room and
// leaves the document to the resumption once the room has closed (endOnRefusedUpdate). Each case
// ends on the same three readings (requireSettledFromTheStore): a new version holds the text the
// store holds, the document owes no settlement, and its block index reads that version.

const (
	// closedSettlementParagraph is the owed document's one paragraph, which a browser types over
	// whole (typeOver).
	closedSettlementParagraph = "Stored before the close."
	// closedSettlementText is the owed document's live text, which its store holds.
	closedSettlementText = closedSettlementParagraph + "\n"
	// closedSettlementVersionOne is the owed document's version 1. It is not the live text, so the
	// document's settlement writes a version.
	closedSettlementVersionOne = "Versioned before the document was edited.\n"
	// closedSettlementDelay is the settle delay a test sets once the settlement it waits for may
	// run: the fixture's own delay is an hour, so a settlement armed before then does not run.
	closedSettlementDelay = 20 * time.Millisecond
	// refusedKeystroke is what a browser types after the close margin, which the store refuses.
	refusedKeystroke = "Typed after the close margin."
	// settlementEndedOnRefusal is the line a settlement logs when it ends on an update its closed
	// issue refused (endOnRefusedUpdate).
	settlementEndedOnRefusal = "dispatch: document settlement ended on a room update its closed issue refused"
)

func TestAClosedIssuesOwedSettlementCompletes(t *testing.T) {
	t.Run("AppendInsideTheCloseMargin", settleAnAppendInsideTheCloseMargin)
	t.Run("RoomLoad", settleOnARoomLoad)
	t.Run("Resumption", settleOnTheResumption)
	t.Run("ArmedBeforeTheClose", settleWhatTheCloseStopped)
	t.Run("ReplacedHistory", settleAReplacedHistory)
	t.Run("RefusedUpdateAfterTheMargin", func(t *testing.T) {
		t.Run("TheSettlementsStoreWriteIsRefused", settleAfterItsStoreWriteIsRefused)
		t.Run("TheRoomsPersistenceIsRefusedFirst", settleAfterTheRoomsPersistenceIsRefused)
	})
}

// closedSettlementFixture is one document of issue CORE-1 that owes a settlement, with CORE-1 still
// open: its live text, which its store holds, is closedSettlementText, and its version 1 is
// closedSettlementVersionOne (createOwedDocument). Its seed arms no settlement and loads no room.
// The service serves browsers on server, its settle delay is an hour, and held holds the room's
// next append of a browser's update once a test arms it (holdOneRoomUpdate), until the test lets
// it go.
type closedSettlementFixture struct {
	database   *store.Store
	service    *Service
	server     *httptest.Server
	held       *heldStore
	artifactID string
}

func newClosedSettlementFixture(t *testing.T) closedSettlementFixture {
	t.Helper()
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(context.Background(), `
		insert into projects (key, name, default_assignee) values ('CORE', 'Core', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank) values
			('CORE-1', 'CORE', 1, 'One', '{"kind":"user","id":"alice"}', 'U');
	`); err != nil {
		t.Fatalf("seed project CORE: %v", err)
	}
	artifactID := uuid.NewString()
	held := &heldStore{
		VersionedStore: NewPgVersioned(database),
		room:           artifactID,
		entered:        make(chan struct{}),
		release:        make(chan struct{}),
	}
	service := New(Deps{
		Store:       database,
		Persistence: held,
		Events:      events.NewBroker(),
		Identity:    headerIdentity(database),
		ServerURL:   "https://dispatch.example",
		Settle:      time.Hour,
		TestHooks:   true,
	})
	t.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	// Cleanups run in reverse, and Shutdown waits for the room's persistence worker this hold parks.
	t.Cleanup(held.let)
	server := newTestServer(t, service)
	createOwedDocument(t, database, service, "CORE-1", artifactID, "spec", closedSettlementText, closedSettlementVersionOne)
	return closedSettlementFixture{database: database, service: service, server: server, held: held, artifactID: artifactID}
}

// settleSoon sets the service's settle delay to closedSettlementDelay, so the next settlement armed
// runs at once. The delay is written once, before what it arms: the service's goroutines read it.
func (f closedSettlementFixture) settleSoon() {
	if f.service.settle != closedSettlementDelay {
		f.service.settle = closedSettlementDelay
	}
}

// resume runs one settlement-resumption pass that arms the settlement of every document owing one,
// however young its pending row (resumeOwedSettlements), and that settlement runs at once.
func (f closedSettlementFixture) resume(t *testing.T) {
	t.Helper()
	f.settleSoon()
	if err := f.service.resumeOwedSettlements(context.Background(), 0); err != nil {
		t.Fatalf("resume the owed settlements: %v", err)
	}
}

// settlementArmed is whether the document's settlement timer is armed on the service.
func (f closedSettlementFixture) settlementArmed() bool {
	state := f.service.lockExistingState(f.artifactID)
	if state == nil {
		return false
	}
	defer f.service.unlockState(f.artifactID, state)
	return f.service.isSettleTimerArmed(state.settle)
}

// settlementState is what the service and the store hold of the document's settlement, for the
// failure text of a wait that gave up on it, so a room that failed, an append that never landed
// and a settlement nobody armed read apart. The room's record is read under its lock, as the
// service reads it (lockExistingState): whether its settle timer is armed, the settlements running
// on it, its generation, whether a close is retiring its room for a reload (reloading) or a
// settlement is loading the room (settleWarming), whether a live write took a settlement over
// (settleDeferred), whether it reads the issue closed, its failure, and the settlements it counts
// failed. The room is read after the record's lock is let go: whether a room of the document is
// resident and the durable version that room's instance holds every update through (cursorOf),
// whether the service holds a load's record of the room (roomLoads), and the times a room of the
// document failed (RoomCounters, failRoomLocked). The store's reading is the database's clock, the
// document's pending-settlement row, its latest version, its durable head (doc_updates) and the
// text its stored updates fold to.
func (f closedSettlementFixture) settlementState() string {
	record := "the service holds no record of the document"
	if state := f.service.lockExistingState(f.artifactID); state != nil {
		failure := "none"
		if state.failed != nil {
			failure = state.failed.Error()
		}
		record = fmt.Sprintf("settle timer armed %t, settlements running %d, generation %d, reloading %t, settle warming %t, settle deferred %t, reads its issue closed %t, failed %s, settlements failed %d",
			f.service.isSettleTimerArmed(state.settle), state.settling, state.roomGeneration, state.reloading,
			state.settleWarming, state.settleDeferred, state.closed, failure, state.settleFailures)
		f.service.unlockState(f.artifactID, state)
	}
	room := "resident false"
	if live := f.service.srv.GetDoc(f.artifactID); live != nil {
		room = "resident true, instance listed false"
		if cursor := f.service.cursorOf(live); cursor != nil {
			room = fmt.Sprintf("resident true, instance holds every update through %d", cursor.heldThrough())
		}
	}
	_, loadHeld := f.service.roomLoads.Load(f.artifactID)
	failures := "unread"
	if counters, err := f.service.RoomCounters(f.artifactID); err == nil {
		failures = fmt.Sprint(counters.RoomFailures)
	}
	room = fmt.Sprintf("%s, load record held %t, room failures %s", room, loadHeld, failures)

	var clock time.Time
	var latest int
	var head int64
	var markedAt, triedAt *time.Time
	var lastActor []byte
	var stored string
	if err := f.database.Pool.QueryRow(context.Background(), `
		select clock_timestamp(),
			(select coalesce(max(number), 0) from artifact_versions where artifact_id = $1),
			(select coalesce(max(version), 0) from doc_updates where artifact_id = $1),
			p.marked_at, p.tried_at, p.last_actor
		from (select 1) one left join doc_settlements_pending p on p.artifact_id = $1
	`, f.artifactID).Scan(&clock, &latest, &head, &markedAt, &triedAt, &lastActor); err != nil {
		stored = fmt.Sprintf("unread: %v", err)
	} else {
		pending := "no pending-settlement row"
		if markedAt != nil {
			tried, actor := "null", "null"
			if triedAt != nil {
				tried = triedAt.Format(time.RFC3339Nano)
			}
			if lastActor != nil {
				actor = string(lastActor)
			}
			pending = fmt.Sprintf("pending-settlement row marked_at %s, tried_at %s, last_actor %s",
				markedAt.Format(time.RFC3339Nano), tried, actor)
		}
		stored = fmt.Sprintf("database clock %s, %s, latest version %d, durable head %d, stored text %s",
			clock.Format(time.RFC3339Nano), pending, latest, head, f.storedText())
	}
	return fmt.Sprintf("the document's settlement state:\n  record: %s\n  room: %s\n  store: %s", record, room, stored)
}

// storedText is the quoted rendering the document's stored updates fold to, or why it could not
// be read: a diagnostic's reading, which fails no test.
func (f closedSettlementFixture) storedText() string {
	loaded, err := NewPgVersioned(f.database).Load(context.Background(), f.artifactID)
	if err != nil {
		return fmt.Sprintf("unread: %v", err)
	}
	doc := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(doc, loaded.Update, nil); err != nil {
		return fmt.Sprintf("undecodable: %v", err)
	}
	tree, err := treeOf(doc)
	if err != nil {
		return fmt.Sprintf("unreadable: %v", err)
	}
	markdown, err := renderTree(tree)
	if err != nil {
		return fmt.Sprintf("unrendered: %v", err)
	}
	return fmt.Sprintf("%q", markdown)
}

// waitFor is waitFor within roomWait, failing with the document's settlement state
// (settlementState) when it gives up.
func (f closedSettlementFixture) waitFor(t *testing.T, description string, condition func() bool) {
	t.Helper()
	if !testwait.Holds(roomWait(t), 5*time.Millisecond, condition) {
		t.Fatalf("timed out waiting for %s\n%s", description, f.settlementState())
	}
}

// requireEnds is requireEnds, failing with the document's settlement state (settlementState). It
// reads roomWait once: read again for the failure text, it fails the test itself once the test
// binary's deadline is what bounds the wait, and the state is not reported.
func (f closedSettlementFixture) requireEnds(t *testing.T, peer *docstest.Peer, what string) {
	t.Helper()
	wait := roomWait(t)
	select {
	case <-peer.Ended:
	case <-time.After(wait):
		t.Fatalf("%s: the browser's socket was still open after %v\n%s", what, wait, f.settlementState())
	}
}

// waitForNoSettlement waits until no settlement of the document is armed or running: the timer's
// callback has returned, with what follows its settlement (followUpSettlement), and nothing armed
// the settlement again. A settlement that stopped short is run again a settle delay later, so the
// wait goes on through each retry until the retries end or fail the room.
func (f closedSettlementFixture) waitForNoSettlement(t *testing.T) {
	t.Helper()
	waitFor(t, roomWait(t), "no settlement of the document to be armed or running", func() bool {
		state := f.service.lockExistingState(f.artifactID)
		if state == nil {
			return true
		}
		defer f.service.unlockState(f.artifactID, state)
		return state.settling == 0 && !f.service.isSettleTimerArmed(state.settle)
	})
}

// requireNoVersionSince fails unless the document's latest version is still before; where says
// what it was read after.
func (f closedSettlementFixture) requireNoVersionSince(t *testing.T, before int, where string) {
	t.Helper()
	number, markdown, _ := latestDocumentVersion(t, f.database, f.artifactID)
	if number == before {
		return
	}
	holding := ""
	if strings.Contains(markdown, refusedKeystroke) {
		holding = ", which holds the keystroke the closed issue refused"
	}
	t.Fatalf("%s: the document has version %d reading %q%s, want no version past %d while the room that took the refused keystroke stands",
		where, number, markdown, holding, before)
}

// requireSettledFromTheStore waits for the document's settlement after its issue closed and fails
// unless it settled: a version past before holds the text the store holds, the document owes no
// settlement, and its block index reads that version. It answers that version's text.
func (f closedSettlementFixture) requireSettledFromTheStore(t *testing.T, before int) string {
	t.Helper()
	ctx := context.Background()
	f.waitFor(t, "the closed issue's document to settle and owe no settlement", func() bool {
		owed, err := settlementPending(ctx, f.database.Pool, f.artifactID)
		return err == nil && !owed
	})
	number, markdown, _ := latestDocumentVersion(t, f.database, f.artifactID)
	if number <= before {
		t.Fatalf("the document's latest version is %d, want one past %d: its settlement wrote no version", number, before)
	}
	stored := storedMarkdown(t, f.database, f.artifactID)
	if markdown == "" || stored == "" {
		t.Fatalf("version %d reads %q and the store holds %q, want the document in both", number, markdown, stored)
	}
	if markdown != stored {
		t.Fatalf("version %d reads %q, want the text the store holds, %q", number, markdown, stored)
	}
	indexed := blockIndexAt(t, f.database, f.artifactID)
	if indexed == nil {
		t.Fatalf("the document's block index reads no version, want the settled version %d", number)
	}
	if *indexed != number {
		t.Fatalf("the document's block index reads version %d, want the settled version %d", *indexed, number)
	}
	return markdown
}

// AppendInsideTheCloseMargin: alice's browser types over the document's paragraph while CORE-1 is
// open, and the room's append of that keystroke is held at the store. CORE-1 then closes, and the
// close is recorded on the room (issueOpenChanged): it stops the settlement the keystroke armed, and
// its close of the room waits for that append. Let go, the append reaches the gate after the close,
// which stores it since the room took it before the close (observedBeforeClose), and the append's
// own store arms the document's settlement (scheduleSettleAfterAppend), which settles the keystroke
// on the closed issue.
func settleAnAppendInsideTheCloseMargin(t *testing.T) {
	f := newClosedSettlementFixture(t)
	alice := connectWithScope(t, f.server.URL, f.artifactID, "alice", "read-write", closedSettlementText)
	before := latestVersionNumber(t, f.service, f.artifactID)
	const typed = "Typed while the issue was open."
	holdOneRoomUpdate(t, f.held, func() { typeOver(t, alice, closedSettlementParagraph, typed) })
	f.settleSoon()

	writeIssueClosed(t, f.database, "CORE-1", true)
	recorded := make(chan struct{})
	go func() {
		defer close(recorded)
		f.service.issueOpenChanged(store.WithTransactionTracking(context.Background()), "CORE-1")
	}()
	f.waitFor(t, "the close to be recorded on the room's record", func() bool {
		return recordReadsClosed(f.service, f.artifactID)
	})
	if f.settlementArmed() {
		t.Fatal("the close left the keystroke's settlement armed, so the append's own settlement is not what this case settles with")
	}
	f.held.let()
	wait := roomWait(t)
	select {
	case <-recorded:
	case <-time.After(wait):
		t.Fatalf("the close of CORE-1 had not returned %v after the room's append went on\n%s", wait, f.settlementState())
	}
	f.requireEnds(t, alice, "the close of CORE-1 closed alice's room")

	if markdown := f.requireSettledFromTheStore(t, before); !strings.Contains(markdown, typed) {
		t.Fatalf("the settled version reads %q, want the keystroke the store took inside the close margin", markdown)
	}
}

// loadWhileOpen loads the document's room while CORE-1 is open (warmLiveDocument), and the load
// arms the settlement the document owes, an hour out (onLoadDocument).
func (f closedSettlementFixture) loadWhileOpen(t *testing.T) {
	t.Helper()
	if err := f.service.warmLiveDocument(context.Background(), f.artifactID); err != nil {
		t.Fatalf("load the document's room: %v", err)
	}
	if !f.settlementArmed() {
		t.Fatal("the room's load armed no settlement of the document it owes")
	}
}

// RoomLoad: the document's room is evicted, and CORE-1 closes while the document owes its
// settlement; the close stops the settlement the room's earlier load armed. A reader opening the
// document then loads its room through the socket's admission, and the room's load arms the
// settlement the document owes (onLoadDocument), which settles it on the closed issue.
func settleOnARoomLoad(t *testing.T) {
	f := newClosedSettlementFixture(t)
	f.loadWhileOpen(t)
	closeDocumentRoom(t, f.service, f.artifactID)
	before := latestVersionNumber(t, f.service, f.artifactID)
	setIssueClosedHere(t, f.service, "CORE-1", true)
	if f.settlementArmed() {
		t.Fatal("the close left the document's settlement armed, so the room's load is not what this case settles with")
	}
	if roomResident(f.service, f.artifactID) {
		t.Fatal("a room holds the document before the reader opens it")
	}
	f.settleSoon()

	connectWithScope(t, f.server.URL, f.artifactID, "alice", "readonly", closedSettlementText)
	f.requireSettledFromTheStore(t, before)
}

// Resumption: the document's room is evicted, CORE-1 closes while the document owes its
// settlement, and nobody opens the document. The resumption pass arms the settlement
// (resumeOwedSettlements), which settles the document on the closed issue.
func settleOnTheResumption(t *testing.T) {
	f := newClosedSettlementFixture(t)
	f.loadWhileOpen(t)
	closeDocumentRoom(t, f.service, f.artifactID)
	before := latestVersionNumber(t, f.service, f.artifactID)
	setIssueClosedHere(t, f.service, "CORE-1", true)
	if f.settlementArmed() {
		t.Fatal("the close left the document's settlement armed, so the resumption is not what this case settles with")
	}

	f.resume(t)
	counters := requireRoomCounters(t, f.service, f.artifactID)
	if armed := len(counters.SettlementResumptions); armed != 1 {
		t.Fatalf("the resumption pass armed the closed issue's document %d times, want once", armed)
	}
	f.requireSettledFromTheStore(t, before)
}

// ArmedBeforeTheClose: the document's room loads while CORE-1 is open and arms the settlement the
// document owes an hour out, and CORE-1 closes before it runs. The close stops that settlement
// (closeRecordLocked) and closes the room, and no version is written; the document's
// pending-settlement row keeps the work, and the resumption pass settles the document on the
// closed issue.
func settleWhatTheCloseStopped(t *testing.T) {
	f := newClosedSettlementFixture(t)
	before := latestVersionNumber(t, f.service, f.artifactID)
	f.loadWhileOpen(t)

	setIssueClosedHere(t, f.service, "CORE-1", true)
	if f.settlementArmed() {
		t.Fatal("the close of CORE-1 left the document's settlement armed")
	}
	if number := latestVersionNumber(t, f.service, f.artifactID); number != before {
		t.Fatalf("the document has version %d after the close, want none past %d", number, before)
	}
	if owed, err := settlementPending(context.Background(), f.database.Pool, f.artifactID); err != nil || !owed {
		t.Fatalf("the document owes a settlement after the close = %t (%v), want its pending row kept", owed, err)
	}

	f.resume(t)
	f.requireSettledFromTheStore(t, before)
}

// ReplacedHistory: the document's room loads while CORE-1 is open. CORE-1 then closes in the
// database alone, and a reader's admission records the close on the room's record (authorize)
// without closing the room, so a closed issue's room stays resident. The document's stored history
// is replaced under that room, as a rebuild on another process replaces it (replaceStoredHistory).
// The settlement the resumption arms finds the room on a history no catch-up brings to the head,
// so it closes the room for a reload and arms itself again (reloadReplacedRoom), and the settlement
// of the reloaded room writes the replaced history's version on the closed issue.
func settleAReplacedHistory(t *testing.T) {
	f := newClosedSettlementFixture(t)
	f.loadWhileOpen(t)
	waitForLandedAppends(t, f.service, f.artifactID)
	before := latestVersionNumber(t, f.service, f.artifactID)
	writeIssueClosed(t, f.database, "CORE-1", true)
	reader := connectWithScope(t, f.server.URL, f.artifactID, "alice", "readonly", closedSettlementText)
	requireRecordClosed(t, f.service, f.artifactID)

	const replacement = "Replaced under the closed room."
	replaceStoredHistory(t, f.database, f.artifactID, closedSettlementText+"\n"+replacement+"\n")
	f.resume(t)
	requireEnds(t, reader, "the settlement on the replaced history closed the room for its reload")

	if markdown := f.requireSettledFromTheStore(t, before); !strings.Contains(markdown, replacement) {
		t.Fatalf("the settled version reads %q, want the replaced history's paragraph", markdown)
	}
}

// refusalWatch is what a RefusedUpdateAfterTheMargin order watches of the document's room: failed
// is set, and failedNow closed, once the room fails (beforeFailedEviction); closes counts the closes
// the append gate's refusals start (beforeRefusedAppendClose), and the first of them closes parked
// and waits until the test lets it go (let).
type refusalWatch struct {
	failed    atomic.Bool
	failedNow chan struct{}
	closes    atomic.Int64
	parked    chan struct{}
	release   chan struct{}
	once      sync.Once
}

func (w *refusalWatch) let() { w.once.Do(func() { close(w.release) }) }

// watchRefusals installs f's refusalWatch. The parked close is let go before the service shuts
// down: cleanups run in reverse, and Shutdown joins it (evictWG).
func (f closedSettlementFixture) watchRefusals(t *testing.T) *refusalWatch {
	t.Helper()
	watch := &refusalWatch{failedNow: make(chan struct{}), parked: make(chan struct{}), release: make(chan struct{})}
	f.service.beforeFailedEviction = func(room string) {
		if room == f.artifactID && watch.failed.CompareAndSwap(false, true) {
			close(watch.failedNow)
		}
	}
	f.service.beforeRefusedAppendClose = func(string) {
		if watch.closes.Add(1) == 1 {
			close(watch.parked)
			<-watch.release
		}
	}
	t.Cleanup(watch.let)
	return watch
}

// requireNoSettlementFailure fails unless the document's room has not failed and counts no failed
// settlement; where says what it was read after. It is read while the room stands: the room's
// close starts the count over (closeRecordLocked).
func (f closedSettlementFixture) requireNoSettlementFailure(t *testing.T, watch *refusalWatch, where string) {
	t.Helper()
	counters := requireRoomCounters(t, f.service, f.artifactID)
	failures, roomErr := 0, f.service.roomFailure(f.artifactID)
	if state := f.service.lockExistingState(f.artifactID); state != nil {
		failures = state.settleFailures
		f.service.unlockState(f.artifactID, state)
	}
	if watch.failed.Load() || roomErr != nil || counters.RoomFailures != 0 {
		t.Fatalf("%s: the room failed (%d failures counted, %d settlement failures, error %v), want the refused update to fail no room",
			where, counters.RoomFailures, failures, roomErr)
	}
	if failures != 0 {
		t.Fatalf("%s: the document's room counts %d settlement failures, want none for the refused update", where, failures)
	}
}

// admitBeforeTheClose connects alice's browser writable while CORE-1 is open, and then closes
// CORE-1 two minutes back in the database alone: nothing calls issueOpenChanged, as on a process
// whose listener has not heard the close, so the room keeps taking alice's keystrokes, and the
// append gate refuses each one the room takes from here on, well past closeMargin
// (observedBeforeClose).
func (f closedSettlementFixture) admitBeforeTheClose(t *testing.T) *docstest.Peer {
	t.Helper()
	alice := connectWithScope(t, f.server.URL, f.artifactID, "alice", "read-write", closedSettlementText)
	closeIssueAgo(t, f.database, "CORE-1", 2*time.Minute)
	return alice
}

// requireOneRefusalEnding fails unless exactly one settlement logged that it ended on the update
// the closed issue refused (endOnRefusedUpdate).
func requireOneRefusalEnding(t *testing.T, logs *storetest.LockedLog) {
	t.Helper()
	if lines := logs.Lines(settlementEndedOnRefusal); strings.Count(lines, "\n") != 1 {
		t.Fatalf("the settlements logged %q:\n%s\nwant exactly one settlement ended on the refused update", settlementEndedOnRefusal, lines)
	}
}

// requireOneRefusalClose waits for the room's close the refusal started and fails unless it was
// the one close any refusal started: the settlement and the room's persistence worker take the one
// mark of the room instance (roomInstance.issueCloseStarted), whichever meets the refusal first.
func (f closedSettlementFixture) requireOneRefusalClose(t *testing.T, watch *refusalWatch, alice *docstest.Peer) {
	t.Helper()
	watch.let()
	requireEnds(t, alice, "the close the refusal started closed alice's room")
	waitForRefusedAppendClose(t, f.service)
	if closes := watch.closes.Load(); closes != 1 {
		t.Fatalf("the refusals started %d closes of the room, want one", closes)
	}
}

// RefusedUpdateAfterTheMargin, its first order: the settlement's own store write meets the refusal.
// alice's keystroke reaches the room's persistence, whose append is held before it reaches the gate
// (holdOneRoomUpdate), so the keystroke is still queued and unstored when the settlement the
// resumption arms stores the room's updates through its read (Ledger.flushThrough), and the gate
// refuses that write (ErrIssueClosed). The refusal starts the room's close while the room's own
// append is still held, so it is the settlement's. The settlement ends there: no failure is counted,
// the room does not fail, and no version is written while the room stands. Let go, the room's own
// append is refused too and starts no second close. Once the room has closed, the resumption
// settles the document from the store, without the refused keystroke.
func settleAfterItsStoreWriteIsRefused(t *testing.T) {
	logs := &storetest.LockedLog{}
	logtest.SetDefault(t, slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	f := newClosedSettlementFixture(t)
	watch := f.watchRefusals(t)
	alice := f.admitBeforeTheClose(t)
	before := latestVersionNumber(t, f.service, f.artifactID)
	holdOneRoomUpdate(t, f.held, func() { typeOver(t, alice, closedSettlementParagraph, refusedKeystroke) })

	f.resume(t)
	select {
	case <-watch.parked:
	case <-watch.failedNow:
		f.requireNoSettlementFailure(t, watch, "after the settlement stored the room's updates")
		t.Fatal("the room failed after the settlement stored the room's updates, want the refused update to fail no room")
	case <-time.After(roomWait(t)):
		t.Fatalf("no refusal started the room's close %v after the settlement was armed: its store write of the room's updates met no refusal", roomWait(t))
	}
	f.waitForNoSettlement(t)
	f.requireNoSettlementFailure(t, watch, "after the settlement met the refusal")
	f.requireNoVersionSince(t, before, "after the settlement met the refusal")
	requireOneRefusalEnding(t, logs)

	f.held.let()
	f.requireOneRefusalClose(t, watch, alice)
	f.requireNoVersionSince(t, before, "once the room closed")

	f.resume(t)
	if markdown := f.requireSettledFromTheStore(t, before); strings.Contains(markdown, refusedKeystroke) {
		t.Fatalf("the settled version reads %q, which holds the keystroke the closed issue refused", markdown)
	}
}

// RefusedUpdateAfterTheMargin, its second order: the room's persistence worker meets the refusal
// first. The gate refuses alice's keystroke, the worker takes the room instance's mark and drops the
// keystroke from the room's queue, and the close the refusal starts is held. The room's tree still
// holds the keystroke the store refused, and no store write of a settlement meets it now. The
// settlement the resumption arms reaches the document's lock and ends on the instance's mark
// (roomRefusedUpdate): no failure is counted, the room does not fail, and no version is written
// while the room stands. Once the room has closed, the resumption settles the document from the
// store, without the refused keystroke.
func settleAfterTheRoomsPersistenceIsRefused(t *testing.T) {
	logs := &storetest.LockedLog{}
	logtest.SetDefault(t, slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	f := newClosedSettlementFixture(t)
	watch := f.watchRefusals(t)
	var locked atomic.Int64
	f.service.afterSettleLock = func(room string) {
		if room == f.artifactID {
			locked.Add(1)
		}
	}
	alice := f.admitBeforeTheClose(t)
	before := latestVersionNumber(t, f.service, f.artifactID)
	typeOver(t, alice, closedSettlementParagraph, refusedKeystroke)
	awaitRefusedAppendCloseParked(t, watch.parked)
	if markdown := residentRoomMarkdown(t, f.service, f.artifactID); !strings.Contains(markdown, refusedKeystroke) {
		t.Fatalf("the room reads %q, want the keystroke the store refused still in it, which is what a settlement must not save", markdown)
	}

	f.resume(t)
	waitFor(t, roomWait(t), "the settlement the resumption armed to take the document's lock", func() bool {
		return locked.Load() > 0
	})
	f.waitForNoSettlement(t)
	f.requireNoVersionSince(t, before, "after the settlement ran on the room holding the refused keystroke")
	f.requireNoSettlementFailure(t, watch, "after the settlement ran on the room holding the refused keystroke")
	requireOneRefusalEnding(t, logs)

	f.requireOneRefusalClose(t, watch, alice)
	f.requireNoVersionSince(t, before, "once the room closed")

	f.resume(t)
	if markdown := f.requireSettledFromTheStore(t, before); strings.Contains(markdown, refusedKeystroke) {
		t.Fatalf("the settled version reads %q, which holds the keystroke the closed issue refused", markdown)
	}
}
