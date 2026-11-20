package docs

// Round 9 review repros (ParkedRound9). Each asserts the correct behaviour, so it fails where the
// defect stands.

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// One writer's page parks GC structs, each behind a one-clock skip, under its own given id, frame
// after frame, each frame the whole structs' share the room's allowance leaves it. Nothing parked
// is stored and nothing lands, and the room keeps a handful of structs; another person's keystroke
// must still apply.
func TestRV9ParkedStructsUnderTheSendersOwnIdLockASmallDocument(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	mallory := openPage(t, serverURL, artifactID, "mallory")
	awaitAnswer(t, mallory, "the document")
	bob := openPage(t, serverURL, artifactID, "bob")
	awaitAnswer(t, bob, "the document")
	room := roomDocument(t, service, artifactID)
	own := uint64(mallory.Given)
	hearFromNow(t, mallory, "mallory's page")
	applies := service.roomWeighingOf(room).applies()

	clock := uint64(0)
	sent := 0
	for sent < 40 {
		ceiling := ceilingWithQueue(applies.structsHeld(room), room.PendingStats().Items)
		carried, _ := capShares(ceiling)
		if carried < 1 {
			break
		}
		f := newV1Frame(1).group(2*carried, own, clock)
		for range carried {
			f = f.skip(1).gc(1)
		}
		frame := f.noDeletes()
		clock += uint64(2 * carried)
		if err := mallory.Write(ygsync.EncodeUpdate(frame)); err != nil {
			t.Fatalf("send frame %d: %v", sent+1, err)
		}
		sent++
		heard := awaitHeardStatus(t, mallory, sent, "mallory after frame")
		t.Logf("frame %d: %d GC structs behind skips under mallory's own id, %d bytes, answered %q; pending %+v; ceiling now %d",
			sent, carried, len(frame), heard[sent-1], room.PendingStats(), ceilingWithQueue(applies.structsHeld(room), room.PendingStats().Items))
	}
	requireConnected(t, mallory, "mallory")
	t.Logf("after %d frames the room holds %d structs in its store, parks %d, is weighed at %d against the %d cap; its stored state is %d bytes",
		sent, room.StructCount(), room.PendingStats().Items, ceilingWithQueue(applies.structsHeld(room), room.PendingStats().Items), maxUpdateItems,
		len(crdt.EncodeStateAsUpdateV1(room, nil)))
	if charge := service.roomWeighingOf(room).roomCharge(); charge != nil {
		t.Logf("the room's budget charge is %d bytes", chargedWeight(charge))
	}

	hearFromNow(t, bob, "bob's page")
	sendTyped(t, bob, 0, "x")
	select {
	case <-bob.Ended:
		t.Fatalf("bob's honest keystroke into a %d-struct document was refused and his page closed %v, after mallory's page parked %d structs under her own id",
			room.StructCount(), bob.Closed(), room.PendingStats().Items)
	case <-time.After(2 * time.Second):
	}
	heard := awaitHeardStatus(t, bob, 1, "bob after his keystroke")
	t.Logf("bob's keystroke was answered %q", heard[0])
}

// A read-only page's sync step 2 at or under unslottedApplyBytes is decoded outside every slot
// under the apply's value limit alone. A 2 MB step 2 of a million GC structs is within that limit
// and builds a million items; it must cost no more than the browser's apply of the same frame.
func TestRV9AReadOnlyStepTwoOfAMillionStructsCostsNoMoreThanAWritersApply(t *testing.T) {
	const n = 1_000_000
	f := newV1Frame(1).group(n, 7_300_001, 0)
	for range n {
		f = f.gc(1)
	}
	stepTwo := f.noDeletes()
	if len(stepTwo) > unslottedApplyBytes {
		t.Fatalf("the step 2 is %d bytes, past the %d a read-only step 2 is decoded at", len(stepTwo), unslottedApplyBytes)
	}
	// The read-only decode ygo runs (provider/websocket/peer.go roomHolds).
	room := newDocumentCopy()
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	before := stats.TotalAlloc
	held, err := crdt.DocContainsUpdateV1Limit(room, stepTwo, maxDecodedValuesPerApply)
	runtime.ReadMemStats(&stats)
	readOnly := stats.TotalAlloc - before
	t.Logf("read-only decode of a %d-byte step 2 of %d GC structs: held=%t err=%v, allocated %d bytes (%.1f x the frame)",
		len(stepTwo), n, held, err, readOnly, float64(readOnly)/float64(len(stepTwo)))

	// The writer's apply of the same frame through the hook.
	service, live, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph.\n", 0)
	defer runtime.KeepAlive(replica)
	runtime.GC()
	runtime.ReadMemStats(&stats)
	before = stats.TotalAlloc
	werr, called := weighInto(service, live, stepTwo)
	runtime.ReadMemStats(&stats)
	writer := stats.TotalAlloc - before
	t.Logf("writer's hook apply of the same frame: hook=%v applied=%t, allocated %d bytes (%.1f x the frame)",
		werr, called, writer, float64(writer)/float64(len(stepTwo)))

	// End to end on a read-only socket.
	svc, artifactID, serverURL := clientIDRoom(t)
	writeIssueClosed(t, svc.store, "DOC-1", true)
	connection, _, err := gws.DefaultDialer.Dial(socketURL(serverURL, artifactID)+"&"+contracts.DocumentClientIdParameter+"=", pageHeader("bob"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	if _, named, _, err := docstest.ReadGivenClient(connection); err != nil || !named {
		t.Fatalf("bob's read-only socket was given no client id in its first frame: %v", err)
	}
	message := encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarString(artifactID)
		encoder.WriteVarUint(0)
		encoder.WriteVarUint(ygsync.MsgSyncStep2)
		encoder.WriteVarBytes(stepTwo)
	})
	runtime.GC()
	runtime.ReadMemStats(&stats)
	sent := stats.TotalAlloc
	started := time.Now()
	if err := connection.WriteMessage(gws.BinaryMessage, message); err != nil {
		t.Fatal(err)
	}
	if err := connection.SetReadDeadline(time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	answer := -1
	for answer < 0 {
		_, read, err := connection.ReadMessage()
		if err != nil {
			var closed *gws.CloseError
			if errors.As(err, &closed) {
				t.Fatalf("closed %d %s", closed.Code, closed.Text)
			}
			t.Fatalf("socket ended: %v", err)
		}
		decoder := encoding.NewDecoder(read)
		if _, err := decoder.ReadVarString(); err != nil {
			continue
		}
		if kind, err := decoder.ReadVarUint(); err != nil || kind != 8 {
			continue
		}
		if applied, err := decoder.ReadVarUint(); err == nil {
			answer = int(applied)
		}
	}
	runtime.ReadMemStats(&stats)
	endToEnd := stats.TotalAlloc - sent
	t.Logf("read-only socket: a %d-byte step 2 answered %d in %s; the process allocated %d bytes from its first byte to the answer (%.1f x the frame)",
		len(stepTwo), answer, time.Since(started), endToEnd, float64(endToEnd)/float64(len(stepTwo)))
	// The in-tree test of a step 2 dense in values holds the same path to 16 MiB
	// (TestAReadOnlySocketsStepTwoDenseInValuesDecodesNoFurtherThanTheApplysValueLimit).
	if bound := uint64(16 << 20); endToEnd > bound {
		t.Fatalf("a read-only page's %d-byte step 2 made the process allocate %d bytes outside every slot, past %d (the writer's unslotted apply of the same frame: %d)",
			len(stepTwo), endToEnd, bound, writer)
	}
}

// A writer's page on task A parks two frames of pasted text behind clocks task A has not caught up
// to; task A's catch-up then supplies them and its drain lands both. The drain of a server write is
// passed by the render check (renderCheck.check, writtenApplyOf), and since round 9 its commit is
// stored by the room (landedBeyond). What lands must be held to the document bound as a browser's
// drain is, and the room's queue figure must read the queue the drain emptied.
func TestRV9ACatchUpsDrainLandsParkedContentPastTheDocumentBoundAndStoresIt(t *testing.T) {
	bob := model.Actor{Kind: "user", ID: "bob"}
	pair := newProcessPair(t)
	ctx := context.Background()
	var lines []string
	var linesMu sync.Mutex
	t.Cleanup(func() {
		if t.Failed() {
			linesMu.Lock()
			defer linesMu.Unlock()
			t.Logf("the drive:\n%s", strings.Join(lines, "\n"))
		}
	})
	start := time.Now()
	alice := lcDial(t, pair.serverA.URL, pair.artifactID, "alice", nil, start, &lines, &linesMu)
	<-alice.synced
	if !lcWait(t, "alice's first sync to be answered", 5*time.Second, func() bool { return len(alice.heard()) > 0 }) {
		t.Fatal("alice's first SyncStep2 was never answered")
	}
	pair.leaveTaskABehind(t, 2, bob, "Bob's paragraph.\n")
	loaded, err := NewPgVersioned(pair.database).Load(ctx, pair.artifactID)
	if err != nil {
		t.Fatalf("load the stored document: %v", err)
	}
	if err := crdt.ApplyUpdateV1(alice.doc, loaded.Update, "remote"); err != nil {
		t.Fatalf("give alice's page bob's paragraph: %v", err)
	}
	const paste = 900_000
	for i := range 2 {
		chunk := strings.Repeat(string(rune('a'+i)), paste)
		typed := docstest.Transact(alice.doc, func(txn *crdt.Transaction) {
			texts := paragraphTextsOf(txn.GetXmlFragment(fragmentName))
			last := texts[len(texts)-1]
			last.Insert(txn, last.Len(), chunk, nil)
		})
		alice.write(ygsync.EncodeUpdate(typed), &lcSent{update: typed, what: "paste"})
		if !lcWait(t, "alice's paste to be answered", 10*time.Second, func() bool { return len(alice.heard()) > i+1 }) {
			t.Fatal("task A never answered alice's paste")
		}
		t.Logf("paste %d (%d bytes) answered %q", i+1, len(typed), alice.heard()[len(alice.heard())-1])
	}
	room := pair.taskA.srv.GetDoc(pair.artifactID)
	weighing := pair.taskA.roomWeighingOf(room)
	applies := weighing.applies()
	rendered, _ := weighing.size()
	t.Logf("task A parks %+v; its queue figure %d, resident %d; rendering %d bytes", room.PendingStats(), applies.queuedBytes(), applies.residentBytes(), rendered)
	if room.PendingStats().Items == 0 {
		t.Fatal("alice's pastes did not park")
	}

	if outcome, err := pair.taskA.catchUpRoom(ctx, pair.database.Pool, pair.artifactID, room); err != nil || outcome != catchUpApplied {
		t.Fatalf("task A's catch-up answered %v, %v", outcome, err)
	}
	// The wait assumed the drain lands the pastes, which design F's discard of a refused drain
	// forbids, so it now holds until the drain has run (the architect's ruling, Envoy bc5f351a,
	// 2026-11-20); every assertion is unchanged.
	waitForRoomRendering(t, func() *crdt.Doc { return room }, func(string) bool { return room.PendingStats().Items == 0 }, "the drain to run")
	// An empty queue is not yet a discard: the drain that empties it either lands the pastes or is
	// discarded, and the discard closes the page that parked them.
	select {
	case <-alice.ended:
		t.Logf("alice's page closed %v", alice.closed)
		if alice.closed == nil {
			t.Errorf("alice's page ended with no close from task A: want the discard of the drain past the bound to close it")
		}
	case <-time.After(10 * time.Second):
		t.Errorf("alice's page was not closed after task A's catch-up drained her pastes: want the drain past the bound discarded and her page, which parked them, closed")
	}
	rendered, _ = weighing.size()
	md, _ := renderDocument(room)
	t.Logf("after the catch-up task A parks %+v; its queue figure %d against the queue's %d content bytes; resident %d, tracked %d; rendering %d bytes (published %d), bound %d",
		room.PendingStats(), applies.queuedBytes(), room.PendingStats().ContentBytes, applies.residentBytes(), applies.stateBytes(), len(md), rendered, pmdoc.MaxDocumentBytes)

	staleQueue := applies.queuedBytes() != int64(room.PendingStats().ContentBytes)
	// Is it stored? Wait for the room's appends to settle, then read the store.
	var storedLen int
	lcWait(t, "the store to hold alice's pastes", 15*time.Second, func() bool {
		loaded, err := NewPgVersioned(pair.database).Load(ctx, pair.artifactID)
		if err != nil {
			return false
		}
		md, err := renderDocument(copyOf(t, loaded.Update))
		if err != nil {
			return false
		}
		storedLen = len(md)
		return storedLen > pmdoc.MaxDocumentBytes
	})
	t.Logf("the stored document renders %d bytes", storedLen)

	carol := lcDial(t, pair.serverA.URL, pair.artifactID, "carol", nil, start, &lines, &linesMu)
	<-carol.synced
	carol.typeAt(0, "C")
	lcWait(t, "carol's keystroke answered or closed", 5*time.Second, func() bool {
		select {
		case <-carol.ended:
			return true
		default:
		}
		return len(carol.heard()) > 1
	})
	select {
	case <-carol.ended:
		t.Logf("carol's keystroke: page closed %v", carol.closed)
	default:
		t.Logf("carol heard %v", carol.heard())
	}

	if len(md) > int(maxPeerRenderingBytes) {
		t.Errorf("task A's catch-up landed parked browser content unchecked: the room renders %d bytes, past the %d bound a browser's drain is held to; the store renders %d",
			len(md), maxPeerRenderingBytes, storedLen)
	}
	if staleQueue {
		t.Errorf("after the catch-up's drain emptied the queue the room's queue figure is %d against the queue's %d: the room is weighed at %d resident, its landed content counted twice",
			applies.queuedBytes(), room.PendingStats().ContentBytes, applies.residentBytes())
	}
}

// 4,096 frames of one delete range each under one client, each range at clocks of its own so no two
// merge: the room parks 4,096 ranges, the cap, and is charged 32 bytes each. The live heap they
// hold, read with the frames freed, must stay within four times that charge plus 1 MiB.
func TestRV9ParkedRangesOneAFrameHoldNoMoreHeapThanTheirCharge(t *testing.T) {
	service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph.\n", 0)
	defer runtime.KeepAlive(replica)
	room.SetMaxPendingDeleteRanges(maxParkedDeleteRanges)
	const client = 7_400_001
	var stats runtime.MemStats
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&stats)
	before := stats.HeapAlloc
	for i := range maxParkedDeleteRanges {
		frame := newV1Frame(0).uint(1).uint(client).uint(1).uint(uint64(4 * i)).uint(1).done()
		if err, called := weighInto(service, room, frame); err != nil || !called {
			t.Fatalf("frame %d refused: %v", i+1, err)
		}
	}
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&stats)
	held := int64(stats.HeapAlloc) - int64(before)
	applies := service.roomWeighingOf(room).applies()
	charge := applies.parkedRangeCount() * bytesOfHeapAParkedRange
	t.Logf("%d one-range frames: the room parks %d ranges, charged %d bytes for them; the live heap grew %d bytes (%.1f bytes a range)",
		maxParkedDeleteRanges, room.PendingStats().DeleteRanges, charge, held, float64(held)/float64(maxParkedDeleteRanges))
	if held > 4*charge+(1<<20) {
		t.Errorf("the room's parked ranges hold %d bytes of heap, past four times the %d it is charged plus 1 MiB", held, charge)
	}
}

// One process. Mallory's page adds a paragraph with no block id and types "x" in it, then sends two
// frames of 900,000 characters after the "x": the first names as its right origin the room
// document's own client at its next clock, which only the server's own transaction writes, and
// the second follows the first. Both park. The block-id backfill (the stamp settlement also writes)
// writes that clock in the room's own transaction, which runs no drain; an agent's API edit then
// publishes, and its apply's drain lands both frames past the document bound with no render check.
func TestRV9OneProcessAPublishLandsParkedContentPastTheDocumentBound(t *testing.T) {
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
	heard := awaitHeardStatus(t, mallory, 1, "mallory after her paragraph")
	server := uint64(room.ClientID())
	next := room.StateVector().Clock(room.ClientID())
	m := room.StateVector().Clock(mallory.Given)
	t.Logf("mallory's paragraph answered %q; the room's own client %d stands at clock %d; mallory's at %d", heard[0], server, next, m)
	const paste = 900_000
	first := newV1Frame(1).group(1, own, m).raw(0xC4).uint(own).uint(m - 1).uint(server).uint(next).
		str(strings.Repeat("a", paste)).noDeletes()
	second := newV1Frame(1).group(1, own, m+paste).raw(0x84).uint(own).uint(m + paste - 1).
		str(strings.Repeat("b", paste)).noDeletes()
	for i, frame := range [][]byte{first, second} {
		if err := mallory.Write(ygsync.EncodeUpdate(frame)); err != nil {
			t.Fatal(err)
		}
		heard := awaitHeardStatus(t, mallory, i+2, "mallory after a paste")
		t.Logf("paste %d (%d bytes) answered %q; the room parks %+v", i+1, len(frame), heard[i+1], room.PendingStats())
	}
	if room.PendingStats().Items != 2 {
		t.Fatalf("the pastes did not park: %+v", room.PendingStats())
	}
	report := service.backfillBlockIDs(context.Background(), artifactID)
	t.Logf("the backfill stamped %d block ids (err %v); the room's own client now stands at %d; the room parks %+v",
		report.Stamped, report.Err, room.StateVector().Clock(room.ClientID()), room.PendingStats())
	if _, err := joinedApplyOps(service, artifactID, []model.EditOp{{Op: "replace", Find: "Alice's paragraph.", With: "Alice's paragraph!"}},
		model.Actor{Kind: "session", ID: "session-0123456789abcdef"}, nil); err != nil {
		t.Fatalf("the agent's edit: %v", err)
	}
	md, _ := renderDocument(room)
	applies := service.roomWeighingOf(room).applies()
	t.Logf("after the agent's edit the room parks %+v, renders %d bytes (bound %d); queue figure %d, resident %d, tracked %d",
		room.PendingStats(), len(md), maxPeerRenderingBytes, applies.queuedBytes(), applies.residentBytes(), applies.stateBytes())
	var stored int
	lcWait(t, "the store to hold the pastes", 15*time.Second, func() bool {
		loaded, err := NewPgVersioned(service.store).Load(context.Background(), artifactID)
		if err != nil {
			return false
		}
		got, err := renderDocument(copyOf(t, loaded.Update))
		stored = len(got)
		return err == nil && stored > pmdoc.MaxDocumentBytes
	})
	t.Logf("the stored document renders %d bytes", stored)
	if len(md) > int(maxPeerRenderingBytes) {
		t.Errorf("on one process an agent's edit landed mallory's parked pastes unchecked: the room renders %d bytes past the %d bound; the store renders %d",
			len(md), maxPeerRenderingBytes, stored)
	}
}
