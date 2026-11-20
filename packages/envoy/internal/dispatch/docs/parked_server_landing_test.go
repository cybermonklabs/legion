package docs

import (
	"context"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A browser's frame parked behind clocks a server write then supplies lands in that write's
// commit, and is stored before its sender is told it is saved. It is driven through both server
// writes whose commit the room holds against the update its writer stored (written): a catch-up,
// the rows another process stored, and a live write's publish, the update this process's own
// write committed. Either is durable already, so the room stores its commit only where it carries
// more than its writer stored - here the parked frame the commit's drain landed (classifyDrained,
// heldAgainst) - and resyncs the sender once that store has returned, so the SyncStep2 its
// answer clears the row with comes after the store. Before that rule the server write's commit was
// taken as stored, the landed content was in no row, and the next browser commit's store
// resynced the sender all the same: the row cleared, and the edit was the room's alone.
//
// Bob writes a paragraph that task A's room does not hold yet: from his browser on task B, which
// task A's room has not caught up to, or by a write on task A held after its commit and before its
// publish. Alice's page on task A takes bob's paragraph from the store, as a page that held it
// from another process's room would, and types after it; task A parks her frame behind bob's
// clocks and answers it not applied. Task A then catches up, or bob's write publishes, which lands
// her frame, and carol types on task A, whose store is the one that resynced alice before the
// rule. Once alice's row has cleared, the store holds her edit.
func TestAFrameParkedBehindAServerWritesClocksIsStoredBeforeItsSenderIsAnsweredSaved(t *testing.T) {
	for _, server := range serverLandings() {
		t.Run(server.name, func(t *testing.T) {
			drive := parkBehindAServerWrite(t, server, " Alice's ending.")
			drive.land(t, drive.room)
			if parked := drive.room.PendingStats(); parked.Items != 0 {
				t.Fatalf("task A's room still parks %+v after %s supplied bob's clocks", parked, server.name)
			}
			carol := lcDial(t, drive.pair.serverA.URL, drive.pair.artifactID, "carol", nil, drive.start, drive.lines, drive.linesMu)
			<-carol.synced
			carol.typeAt(0, "C")

			alice := drive.alice
			if !lcWait(t, "alice's row to clear", 10*time.Second, func() bool { return alice.rowsHeld() == 0 }) {
				t.Fatalf("alice's row never cleared: she heard %v", alice.heard())
			}
			own := alice.doc.ClientID()
			if stored, wrote := storedClock(t, drive.pair, own), alice.doc.StateVector().Clock(own); stored < wrote {
				t.Fatalf("alice's row cleared, answered saved, while the store holds her client %d to clock %d of %d: her edit is the room's alone",
					own, stored, wrote)
			}
			t.Logf("alice heard %v, and the store holds her edit", alice.heard())
		})
	}
}

// A server write's drain is held to the document bound as a browser's drain is
// (renderCheck.check): alice's page parks two pastes of 900,000 characters behind bob's clocks,
// which together would leave the room rendering past maxPeerRenderingBytes, and the catch-up or
// the publish that supplies the clocks lands neither. The write stands - the catch-up answers
// applied, the publish returns - the room parks nothing and renders within the bound, the store
// holds nothing past it, and alice is closed with the transient DOC_BUSY, since the check does not
// render what the write's own update left and cannot say whether the drain grew the rendering.
// DOC_BUSY keeps the edit, and alice's page sends it again as it reconnects: its sync step 2
// carries both pastes, whose gap the landing filled, so nothing of it parks, and the room weighs
// it as her own apply, which refuses it for good, DOC_TOO_LARGE, and lands nothing of it. So the
// busy close does not come round again. Before, the check passed every run of a server write's
// apply, and the catch-up landed both pastes and stored them: 1,800,034 bytes rendered and stored
// (TestRV9ACatchUpsDrainLandsParkedContentPastTheDocumentBoundAndStoresIt).
func TestAServerWritesDrainOfParkedContentPastTheBoundIsDiscardedAndItsSenderClosed(t *testing.T) {
	const paste = 900_000
	for _, server := range serverLandings() {
		t.Run(server.name, func(t *testing.T) {
			drive := parkBehindAServerWrite(t, server, strings.Repeat("a", paste), strings.Repeat("b", paste))
			drive.land(t, drive.room)
			rendered, err := renderDocument(drive.room)
			if err != nil {
				t.Fatalf("render task A's room: %v", err)
			}
			parked := drive.room.PendingStats()
			t.Logf("after %s task A's room parks %+v and renders %d bytes (bound %d)", server.name, parked, len(rendered), maxPeerRenderingBytes)
			if len(rendered) > int(maxPeerRenderingBytes) {
				t.Fatalf("%s landed alice's parked pastes past the bound: the room renders %d bytes, past the %d a browser's drain is held to",
					server.name, len(rendered), maxPeerRenderingBytes)
			}
			if parked.Items != 0 {
				t.Fatalf("task A's room still parks %+v: the drain the check refused was not discarded", parked)
			}
			select {
			case <-drive.alice.ended:
			case <-time.After(roomWait(t)):
				t.Fatal("alice's page was never closed for the pastes the room discarded")
			}
			if closed := drive.alice.closed; closed == nil || closed.Code != contracts.DocumentBusyCloseCode {
				t.Fatalf("alice's page ended with %v, want %d %s", closed, contracts.DocumentBusyCloseCode, contracts.DocumentBusyCloseReason)
			}
			waitForLandedAppends(t, drive.pair.taskA, drive.pair.artifactID)
			loaded, err := NewPgVersioned(drive.pair.database).Load(context.Background(), drive.pair.artifactID)
			if err != nil {
				t.Fatalf("load the stored document: %v", err)
			}
			stored, err := renderDocument(copyOf(t, loaded.Update))
			if err != nil {
				t.Fatalf("render the stored document: %v", err)
			}
			if len(stored) > int(maxPeerRenderingBytes) {
				t.Fatalf("the store renders %d bytes, past the %d bound", len(stored), maxPeerRenderingBytes)
			}
			again := lcDial(t, drive.pair.serverA.URL, drive.pair.artifactID, "alice", drive.alice, drive.start, drive.lines, drive.linesMu)
			select {
			case <-again.ended:
			case <-time.After(roomWait(t)):
				t.Fatalf("alice's page sent her pastes again and was never closed: it heard %v", again.heard())
			}
			if closed := again.closed; closed == nil || closed.Code != contracts.DocumentTooLargeCloseCode || closed.Text != contracts.DocumentTooLargeCloseReason {
				t.Fatalf("alice's page sent her pastes again and ended with %v, want %d %s", closed, contracts.DocumentTooLargeCloseCode, contracts.DocumentTooLargeCloseReason)
			}
			if room := drive.pair.taskA.srv.GetDoc(drive.pair.artifactID); room != nil {
				resent, err := renderDocument(room)
				if err != nil {
					t.Fatalf("render task A's room after alice's resend: %v", err)
				}
				if parked := room.PendingStats(); parked.Items != 0 || len(resent) > int(maxPeerRenderingBytes) {
					t.Fatalf("after alice's resend task A's room parks %+v and renders %d bytes, want nothing parked and the room within the %d bound",
						parked, len(resent), maxPeerRenderingBytes)
				}
			}
		})
	}
}

// A server write whose own update the room already holds integrates nothing, so ygo runs the
// room's apply check on it once, for its drain, and that run is judged as a drain's
// (crdt.ApplyView.Drained, renderCheck.check). Mallory parks two pastes the room's next remote
// apply drains (parkPastesBehindTheRoomsOwnClock), and a catch-up then applies a batch the room
// holds whole, the room's own state. The catch-up stands, the drain is discarded and the room
// renders within the bound. Before, that one run was passed as an update's, and the drain landed
// both pastes past the bound.
func TestAServerWriteTheRoomAlreadyHoldsHasItsDrainJudged(t *testing.T) {
	service, artifactID, room := parkPastesBehindTheRoomsOwnClock(t)
	state := crdt.EncodeStateAsUpdateV1(room, nil)
	if held, err := crdt.DocContainsUpdateV1(room, state); !held || err != nil {
		t.Fatalf("the room's own state reads as held %t (%v), want held", held, err)
	}
	catchUpPastTheBound(t, service, artifactID, room, state, "a catch-up the room held whole", 0)
}

// A server write whose structs only park, because they name a clock the room has not reached,
// changes nothing either, so ygo runs the room's apply check on it once, for its drain, and that
// run is judged as a drain's (crdt.ApplyView.Drained, renderCheck.check). Mallory parks two pastes
// the room's next remote apply drains (parkPastesBehindTheRoomsOwnClock), and a catch-up then
// applies a batch of one struct at the second clock of a client the room has never seen. The
// struct parks, the catch-up stands, the drain is discarded and the room renders within the bound.
// Before the check read the run off the view, it judged a server write's run as the drain's only
// where the room held the write whole or a run had come before, so this one run was passed as the
// update's, and the drain landed both pastes past the bound.
func TestAServerWriteWhoseStructsOnlyParkHasItsDrainJudged(t *testing.T) {
	service, artifactID, room := parkPastesBehindTheRoomsOwnClock(t)
	const client = 7_600_001
	batch := newV1Frame(1).group(1, client, 1).raw(structStringRef | infoHasOrigin).uint(client).uint(0).str("x").noDeletes()
	if held, err := crdt.DocContainsUpdateV1(room, batch); held || err != nil {
		t.Fatalf("a struct of a client the room has never seen reads as held %t (%v), want not held", held, err)
	}
	catchUpPastTheBound(t, service, artifactID, room, batch, "a catch-up whose struct only parks", 1)
}

// parkPastesBehindTheRoomsOwnClock opens a room of clientIDsSeed, whose next remote apply drains
// two pastes of 900,000 characters: mallory's page adds a paragraph and parks the pastes after it,
// the first naming the room document's own client at its next clock and the second following the
// first, and the block-id backfill then writes that clock in a transaction of the room's own,
// which drains nothing. It returns the service, the document and the room's document.
func parkPastesBehindTheRoomsOwnClock(t *testing.T) (*Service, string, *crdt.Doc) {
	t.Helper()
	service, artifactID, serverURL := clientIDRoom(t)
	service.settle = time.Hour
	mallory := openPage(t, serverURL, artifactID, "mallory")
	awaitAnswer(t, mallory, "the document")
	room := roomDocument(t, service, artifactID)
	own := uint64(mallory.Given)
	hearFromNow(t, mallory, "mallory's page")
	added := docstest.Transact(mallory.Doc, func(txn *crdt.Transaction) {
		fragment := txn.GetXmlFragment(fragmentName)
		paragraph := crdt.NewYXmlElement("paragraph")
		fragment.InsertElement(txn, fragment.Len(), paragraph)
		run := crdt.NewYXmlText()
		paragraph.InsertText(txn, 0, run)
		run.Insert(txn, 0, "x", nil)
	})
	if err := mallory.Write(ygsync.EncodeUpdate(added)); err != nil {
		t.Fatal(err)
	}
	awaitHeardStatus(t, mallory, 1, "mallory after her paragraph")
	server := uint64(room.ClientID())
	next := room.StateVector().Clock(room.ClientID())
	m := room.StateVector().Clock(mallory.Given)
	const paste = 900_000
	first := newV1Frame(1).group(1, own, m).raw(structStringRef | infoHasOrigin | infoHasRightOrigin).uint(own).uint(m - 1).uint(server).uint(next).
		str(strings.Repeat("a", paste)).noDeletes()
	second := newV1Frame(1).group(1, own, m+paste).raw(structStringRef | infoHasOrigin).uint(own).uint(m + paste - 1).
		str(strings.Repeat("b", paste)).noDeletes()
	for i, frame := range [][]byte{first, second} {
		if err := mallory.Write(ygsync.EncodeUpdate(frame)); err != nil {
			t.Fatal(err)
		}
		awaitHeardStatus(t, mallory, i+2, "mallory after a paste")
	}
	if parked := room.PendingStats(); parked.Items != 2 {
		t.Fatalf("the pastes did not park: %+v", parked)
	}
	if report := service.backfillBlockIDs(context.Background(), artifactID); report.Err != nil || report.Stamped == 0 {
		t.Fatalf("the backfill stamped %d block ids (err %v), want mallory's paragraph stamped", report.Stamped, report.Err)
	}
	if parked := room.PendingStats(); parked.Items != 2 {
		t.Fatalf("the backfill's own transaction drained the queue: it parks %+v", parked)
	}
	return service, artifactID, room
}

// catchUpPastTheBound applies batch to room as a catch-up, whose drain would land mallory's two
// pastes (parkPastesBehindTheRoomsOwnClock), and requires the catch-up to stand and the drain to
// be discarded: the room renders within the bound and parks no struct but the parks structs batch
// parks itself. what names the catch-up in every line.
func catchUpPastTheBound(t *testing.T, service *Service, artifactID string, room *crdt.Doc, batch []byte, what string, parks int) {
	t.Helper()
	if _, open, err := service.applyRemoteBatch(artifactID, room, batch); !open || err != nil {
		t.Fatalf("%s answered open %t, %v", what, open, err)
	}
	rendered, err := renderDocument(room)
	if err != nil {
		t.Fatalf("render the room: %v", err)
	}
	parked := room.PendingStats()
	t.Logf("after %s the room parks %+v and renders %d bytes (bound %d)", what, parked, len(rendered), maxPeerRenderingBytes)
	if len(rendered) > int(maxPeerRenderingBytes) {
		t.Fatalf("%s landed mallory's parked pastes past the bound: the room renders %d bytes, past %d",
			what, len(rendered), maxPeerRenderingBytes)
	}
	if parked.Items != parks {
		t.Fatalf("after %s the room parks %+v, want %d structs: the drain was neither landed nor discarded", what, parked, parks)
	}
}

// After a server write's drain the room's figure of its pending queue reads the queue the drain
// left: the drain that lands the frame alice parked empties the queue, and the room is weighed
// resident at its tracked state alone (renderedReplica.chargeAdded). Before, the figure was read
// only after a browser's apply (noteQueued), so it kept alice's parked bytes after they landed,
// and the room was weighed with them twice, in its tracked state and in its queue: after a
// catch-up that landed 1,800,000 bytes, 3,600,342 resident against 1,800,342 tracked
// (TestRV9ACatchUpsDrainLandsParkedContentPastTheDocumentBoundAndStoresIt).
func TestAServerWritesDrainLeavesTheQueueFigureReadingTheQueue(t *testing.T) {
	for _, server := range serverLandings() {
		t.Run(server.name, func(t *testing.T) {
			drive := parkBehindAServerWrite(t, server, " Alice's ending.")
			applies := drive.pair.taskA.roomWeighingOf(drive.room).applies()
			if queued, held := applies.queuedBytes(), drive.room.PendingStats().ContentBytes; queued == 0 || queued != int64(held) {
				t.Fatalf("before %s the room's queue figure reads %d against the queue's %d content bytes, want alice's parked frame in both",
					server.name, queued, held)
			}
			drive.land(t, drive.room)
			parked := drive.room.PendingStats()
			if parked.Items != 0 {
				t.Fatalf("task A's room still parks %+v after %s supplied bob's clocks", parked, server.name)
			}
			if queued := applies.queuedBytes(); queued != int64(parked.ContentBytes) {
				t.Fatalf("after %s's drain emptied the queue the room's queue figure reads %d against the queue's %d content bytes",
					server.name, queued, parked.ContentBytes)
			}
			if ranges := applies.parkedRangeCount(); ranges != int64(parked.DeleteRanges) {
				t.Fatalf("after %s's drain the room's figure of its parked delete ranges reads %d against the queue's %d",
					server.name, ranges, parked.DeleteRanges)
			}
			if resident, tracked := applies.residentBytes(), applies.stateBytes(); resident != tracked {
				t.Fatalf("after %s's drain the room is weighed at %d resident against %d tracked, with nothing queued",
					server.name, resident, tracked)
			}
		})
	}
}

// Content a server write's drain lands is credited to the browsers connected as the room took it,
// as a browser's commit is (classifyDrained): alice's frame, parked behind bob's clocks, lands in
// the catch-up or the publish that supplies them, and alice, the one browser connected, is among
// the document's pending authors. Before, the commit was credited as its origin says, a server
// write's, to no one (originCreditsNoOne), so no version named an author for alice's edit.
func TestContentAServerWritesDrainLandsIsCreditedToTheBrowsersConnected(t *testing.T) {
	for _, server := range serverLandings() {
		t.Run(server.name, func(t *testing.T) {
			drive := parkBehindAServerWrite(t, server, " Alice's ending.")
			state := drive.pair.taskA.room(drive.pair.artifactID)
			state.mu.Lock()
			connected := slices.Collect(maps.Values(state.connected))
			state.mu.Unlock()
			if len(connected) != 1 {
				t.Fatalf("task A's room has %v connected, want alice's page alone", connected)
			}
			alice := connected[0]
			drive.land(t, drive.room)
			owed := pendingAuthors(t, drive.pair.taskA, drive.pair.artifactID)
			if !slices.ContainsFunc(owed, func(author model.Actor) bool { return actorKey(author) == actorKey(alice) }) {
				t.Fatalf("after %s landed alice's parked frame the document owes %v, want alice (%v) among them", server.name, owed, alice)
			}
		})
	}
}

// serverLanding is one of the two server writes whose commit the room holds against the update its
// writer stored (writtenApply). write has bob write his paragraph, which the store holds and task
// A's room does not once it returns, and answers land, which lands the paragraph in that room.
type serverLanding struct {
	name  string
	write func(t *testing.T, pair processPair) (land func(t *testing.T, room *crdt.Doc))
}

// serverLandings is both of them: a catch-up of bob's browser edit on task B, and the publish of
// bob's write on task A, held between its commit and its publish.
func serverLandings() []serverLanding {
	bob := model.Actor{Kind: "user", ID: "bob"}
	return []serverLanding{
		{name: "a catch-up", write: func(t *testing.T, pair processPair) func(*testing.T, *crdt.Doc) {
			pair.leaveTaskABehind(t, 2, bob, "Bob's paragraph.\n")
			return func(t *testing.T, room *crdt.Doc) {
				if outcome, err := pair.taskA.catchUpRoom(context.Background(), pair.database.Pool, pair.artifactID, room); err != nil || outcome != catchUpApplied {
					t.Fatalf("task A's catch-up answered %v, %v, want bob's rows applied", outcome, err)
				}
			}
		}},
		{name: "a live write's publish", write: func(t *testing.T, pair processPair) func(*testing.T, *crdt.Doc) {
			committed, publish := make(chan struct{}), make(chan struct{})
			var held, released sync.Once
			release := func() { released.Do(func() { close(publish) }) }
			t.Cleanup(release)
			pair.taskA.checkRendering = func(at string, write *liveWrite) {
				if at == "publish" && write.artifactID == pair.artifactID {
					held.Do(func() {
						close(committed)
						<-publish
					})
				}
			}
			wrote := make(chan error, 1)
			go func() {
				_, err := joinedReplaceText(pair.taskA, pair.artifactID, "First.\n\nSecond.\n\nBob's paragraph.\n", bob)
				wrote <- err
			}()
			select {
			case <-committed:
			case err := <-wrote:
				t.Fatalf("bob's write on task A returned %v without reaching its publish", err)
			case <-time.After(roomWait(t)):
				t.Fatal("bob's write on task A never reached its publish")
			}
			return func(t *testing.T, _ *crdt.Doc) {
				release()
				select {
				case err := <-wrote:
					if err != nil {
						t.Fatalf("bob's write on task A: %v", err)
					}
				case <-time.After(roomWait(t)):
					t.Fatal("bob's write on task A never returned from its publish")
				}
			}
		}},
	}
}

// serverLandingDrive is one drive of a server landing up to the landing itself: the pair, alice's
// page on task A, task A's room, which parks what alice typed behind bob's clocks, land, which
// supplies those clocks, and the drive's log, which the test prints if it fails.
type serverLandingDrive struct {
	pair    processPair
	alice   *lcPage
	room    *crdt.Doc
	land    func(t *testing.T, room *crdt.Doc)
	start   time.Time
	lines   *[]string
	linesMu *sync.Mutex
}

// parkBehindAServerWrite opens alice's page on task A, has bob write his paragraph through server,
// gives alice's page that paragraph from the store, as a page that held it from another process's
// room would, and has her type each of edits at its end, recorded as a row and sent as an Update.
// Task A's room parks each behind bob's clocks and answers it not applied.
func parkBehindAServerWrite(t *testing.T, server serverLanding, edits ...string) serverLandingDrive {
	t.Helper()
	drive := serverLandingDrive{pair: newProcessPair(t), start: time.Now(), lines: new([]string), linesMu: new(sync.Mutex)}
	t.Cleanup(func() {
		if t.Failed() {
			drive.linesMu.Lock()
			defer drive.linesMu.Unlock()
			t.Logf("the drive:\n%s", strings.Join(*drive.lines, "\n"))
		}
	})
	alice := lcDial(t, drive.pair.serverA.URL, drive.pair.artifactID, "alice", nil, drive.start, drive.lines, drive.linesMu)
	<-alice.synced
	if !lcWait(t, "alice's first sync to be answered", 5*time.Second, func() bool { return len(alice.heard()) > 0 }) {
		t.Fatal("alice's first SyncStep2 was never answered")
	}
	drive.alice, drive.land = alice, server.write(t, drive.pair)

	loaded, err := NewPgVersioned(drive.pair.database).Load(context.Background(), drive.pair.artifactID)
	if err != nil {
		t.Fatalf("load the stored document: %v", err)
	}
	if err := crdt.ApplyUpdateV1(alice.doc, loaded.Update, "remote"); err != nil {
		t.Fatalf("give alice's page bob's paragraph: %v", err)
	}
	for _, edit := range edits {
		typed := docstest.Transact(alice.doc, func(txn *crdt.Transaction) {
			texts := paragraphTextsOf(txn.GetXmlFragment(fragmentName))
			last := texts[len(texts)-1]
			last.Insert(txn, last.Len(), edit, nil)
		})
		alice.mu.Lock()
		alice.nextOrdinal++
		alice.rows = append(alice.rows, lcRow{ordinal: alice.nextOrdinal, update: typed})
		alice.mu.Unlock()
		alice.logf("types after bob's paragraph (%d bytes)", len(typed))
		answered := len(alice.heard())
		alice.write(ygsync.EncodeUpdate(typed), &lcSent{update: typed, what: "Update after bob's paragraph"})
		if !lcWait(t, "alice's frame to be answered", 10*time.Second, func() bool { return len(alice.heard()) > answered }) {
			t.Fatal("task A never answered alice's frame")
		}
		if heard := alice.heard(); heard[len(heard)-1] != "Update after bob's paragraph applied=false" {
			t.Fatalf("alice's frame was answered %q, want it parked and answered not applied", heard[len(heard)-1])
		}
	}
	drive.room = drive.pair.taskA.srv.GetDoc(drive.pair.artifactID)
	if parked := drive.room.PendingStats(); parked.Items == 0 {
		t.Fatalf("task A's room parks %+v, want alice's frames behind bob's clocks", parked)
	}
	return drive
}

// storedClock is the clock the stored document holds client to.
func storedClock(t *testing.T, pair processPair, client crdt.ClientID) uint64 {
	t.Helper()
	loaded, err := NewPgVersioned(pair.database).Load(context.Background(), pair.artifactID)
	if err != nil {
		t.Fatalf("load the stored document: %v", err)
	}
	return copyOf(t, loaded.Update).StateVector().Clock(client)
}
