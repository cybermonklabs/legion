package docs

import (
	"runtime"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// The room's tracked state is the library's count of what each apply added to its encoded state
// (crdt.ApplyReport.StateBytes), and TestTheAnchorFindsNoDifferenceFromWhatTheRoomsAppliesReported
// holds it against an encode of the room for the browser's shapes. These hold it there on the
// paths no browser's report covers: the re-base a compaction gives a room holding something
// pending or something its store has not taken, the format cleanup ygo commits after a remote
// apply, the server's own writes, a replica that is a coarser copy of the room than the room
// itself, and a commit the replica cannot measure at all. Each compares the tally with an encode
// of the room's own document, never of the replica.

// A re-base is a reading of the room's whole state, and a reading of a room whose pending queue
// holds something is a lower bound: the encode merges the queue into the store's encoding, which
// can come out shorter than the direct form the reports count (roomApplies.noteApply). So a
// re-base taken while frames are parked keeps the larger of the tally and the reading, and only
// one taken with nothing pending replaces the tally. The two frames here are the anchor's
// parked shape, which leaves the encode a byte short of the tally: a re-base taking that
// encode as the truth lowered the tally below what the room holds once the gaps fill.
func TestAReBaseTakenWhileAFrameIsParkedDoesNotLowerTheTally(t *testing.T) {
	clients := []uint64{8_200_001, 8_200_002}
	room := loadAndType(t, seededState(t), [][]byte{parkedGCFrame(clients[0], 2_000), parkedGCFrame(clients[1], 3_000)})
	defer runtime.KeepAlive(room)
	store := keepStore(room)
	if !holdsPending(room.live) {
		t.Fatal("the frames parked nothing, so no re-base here is taken while the room holds something pending")
	}
	tracked := room.replica.roomStateBytes()
	read := int64(len(crdt.EncodeStateAsUpdateV1(room.live, nil)))
	t.Logf("with the frames parked the tally is %d bytes and an encode of the room holds %d", tracked, read)
	for _, reading := range []int64{read, read / 2} {
		room.service.rebaseResident("room", room.live, reading, room.service.markResident(room.live))
		if after := room.replica.roomStateBytes(); after < tracked {
			t.Fatalf("a re-base reading %d bytes while the room held parked frames took the tally from %d to %d", reading, tracked, after)
		}
	}

	// The struct each frame waits for fills its gap, the queue empties, and once the store has
	// taken what the room committed, the encode is exact.
	for _, client := range clients {
		weighAsBrowser(t, room, newV1Frame(1).group(1, client, 0).gc(1).noDeletes(), new(int))
	}
	if holdsPending(room.live) {
		t.Fatal("the fillers left the room holding something pending")
	}
	store.stored()
	requireTallyCovers(t, room, "the gaps filled")
	read = int64(len(crdt.EncodeStateAsUpdateV1(room.live, nil)))
	room.service.rebaseResident("room", room.live, read, room.service.markResident(room.live))
	if after := room.replica.roomStateBytes(); after != read {
		t.Fatalf("a re-base reading %d bytes of a room holding nothing pending left the tally at %d, want the reading", read, after)
	}
}

// A compaction's reading is of the store, which can lag the room: a browser's commit is in the
// room, and in its tally, from the moment it applies, and in the store only once its append
// returns. A re-base that took such a reading as the truth dropped that commit from the tally
// until the room's next load. So the compaction marks the room before it reads the rows
// (markRebase), and the re-base takes the reading as the truth only where the mark says the
// store held everything the room did and the room has committed nothing since
// (rebaseMark.covers). Here the reading is the room's stored state as its load read it, which
// leaves out a keystroke the room's persistence has not yet stored, and then one it stored after
// the mark, and the tally must cover an encode of the room after either re-base.
func TestAReBaseOfARoomWhoseStoreHasNotTakenItsLastCommitKeepsTheTally(t *testing.T) {
	state := seededState(t)
	stored := int64(len(state))
	room := loadAndType(t, state, nil)
	defer runtime.KeepAlive(room)
	store := keepStore(room)

	weighAsBrowser(t, room, peerTyping(t, room.live, "typed"), new(int))
	if !store.instance.holdsUnstored() {
		t.Fatal("the room's keystroke is listed as stored already, so this re-base reads a store holding it")
	}
	room.service.rebaseResident("room", room.live, stored, room.service.markResident(room.live))
	requireTallyCovers(t, room, "a re-base on a store that has not taken the room's last commit")

	// Once its append has returned, a fold of the store holds the keystroke, and a reading of it
	// is the truth.
	store.stored()
	read := int64(len(crdt.EncodeStateAsUpdateV1(room.live, nil)))
	room.service.rebaseResident("room", room.live, read, room.service.markResident(room.live))
	if after := room.replica.roomStateBytes(); after != read {
		t.Fatalf("a re-base reading %d bytes of a room its store had taken whole left the tally at %d, want the reading", read, after)
	}

	// A keystroke the room takes after the compaction's mark, and stores before its re-base, is in
	// no reading of the rows the fold read.
	mark := room.service.markResident(room.live)
	weighAsBrowser(t, room, peerTyping(t, room.live, "more"), new(int))
	store.stored()
	room.service.rebaseResident("room", room.live, read, mark)
	requireTallyCovers(t, room, "a re-base on a fold read before the room's last commit")
}

// roomStore stands for a room's persistence in a room no service loaded (loadAndType): an
// instance listed for the room, on which each update its replica drains is listed as unstored,
// as the room's classifier lists a browser's (Service.classifyDrained), until stored says the
// appends have returned.
type roomStore struct {
	instance *roomInstance
	listed   []*queuedClass
}

func keepStore(room *typedResidentRoom) *roomStore {
	store := &roomStore{instance: &roomInstance{}}
	listResident(&room.service.instances, room.live, store.instance)
	room.replica.mu.Lock()
	defer room.replica.mu.Unlock()
	room.replica.classify = func(crdt.LoggedUpdate, uint64, bool, func() map[string]struct{}) (bool, bool, bool) {
		class := &queuedClass{}
		store.listed = append(store.listed, class)
		store.instance.listUnstored(class)
		return true, true, true
	}
	return store
}

// stored takes every update the room listed off the instance, as each append's return does.
func (s *roomStore) stored() {
	for _, class := range s.listed {
		s.instance.unlistUnstored(class)
	}
	s.listed = nil
}

// ygo follows a remote apply that leaves a text's format markers redundant with a cleanup of its
// own, a local transaction with a nil origin, which the apply's report does not cover: the
// cleanup's delete-set entry is only in the cleanup's commit. Here one browser deletes "abc"
// while another marks its last character, the room taking the mark first, so the delete leaves
// the mark's two markers around nothing and the cleanup deletes them. The mark is a one-letter
// key with a one-byte value, so what the cleanup collects of the markers is less than the entry
// it adds and the cleanup grows the state: measured here, the delete's apply grew the room 13
// bytes and reported 13, and the cleanup grew it 2 more. The tally must cover an encode of the
// room once both have landed, which it does only if the cleanup's commit is counted by the drain
// that hands it over.
func TestTheTallyCoversTheFormatCleanupARemoteDeleteLeaves(t *testing.T) {
	state := stateOf(t, "One paragraph xabcy.\n")
	live := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(live, state, nil); err != nil {
		t.Fatal(err)
	}
	// As a room's load leaves its document (bootstrapLoadedState).
	live.SetFormatCleanup(true)
	room := residentRoomOf(t, live, true)
	defer runtime.KeepAlive(room)

	at := strings.Index(textOf(t, live), "abc")
	mark := crdt.Attributes{"b": true}
	marking := paragraphEdit(t, state, func(txn *crdt.Transaction, text *crdt.YXmlText) { text.Format(txn, at+2, 1, mark) })
	deletion := paragraphEdit(t, state, func(txn *crdt.Transaction, text *crdt.YXmlText) { text.Delete(txn, at, 3) })

	weighAsBrowser(t, room, marking, new(int))
	before := room.replica.roomStateBytes()
	encodedBefore := int64(len(crdt.EncodeStateAsUpdateV1(live, nil)))
	// The deleting apply's own commit, read as its observers run and before ygo runs the cleanup
	// that follows it (crdt.Doc.afterRemoteObservers).
	deleting := new(int)
	var reported, applied int64
	cleaned := false
	stop := live.OnUpdate(func(_ []byte, origin any) {
		switch origin {
		case deleting:
			reported = room.replica.roomStateBytes() - before
			applied = int64(len(crdt.EncodeStateAsUpdateV1(live, nil))) - encodedBefore
		case nil:
			cleaned = true
		}
	})
	weighAsBrowser(t, room, deletion, deleting)
	stop()
	if !cleaned {
		t.Fatal("no format cleanup followed the delete, so this shape tests nothing")
	}
	grew := int64(len(crdt.EncodeStateAsUpdateV1(live, nil))) - encodedBefore
	t.Logf("the delete's report raised the tally by %d as its apply grew the room by %d; with its cleanup the room grew by %d and the tally by %d",
		reported, applied, grew, room.replica.roomStateBytes()-before)
	if grew <= reported {
		t.Fatalf("the delete's apply grew the room by %d and its cleanup left it grown by %d, within the delete's own report of %d, so this shape no longer tells a tally that counts the cleanup from one that does not",
			applied, grew, reported)
	}
	requireTallyCovers(t, room, "a remote delete and the cleanup it left")
}

// A server write is a local transaction, which ygo reports nothing for, and the bytes its commit
// logs leave out each split's header: a delete inside a run cuts the item it falls in, and the
// halves are structs no update carries. So each is counted at what the replica's own report of
// the same update said it added (renderedReplica.countCommit), and the tally must cover an
// encode of the room after each shape whose frame falls short of it.
func TestAServerWriteIsCountedAtWhatItAddedToTheRoomsState(t *testing.T) {
	state := stateOf(t, "First paragraph of ordinary prose. "+strings.Repeat("abcdefghij", 40)+"\n")
	strong := crdt.Attributes{"strong": crdt.Attributes{"marker": "**"}}
	for _, shape := range []struct {
		name  string
		write func(txn *crdt.Transaction, text *crdt.YXmlText)
	}{
		{"a delete of one character inside a run", func(txn *crdt.Transaction, text *crdt.YXmlText) { text.Delete(txn, 8, 1) }},
		{"a delete of every other character of 200", func(txn *crdt.Transaction, text *crdt.YXmlText) {
			for i := range 100 {
				text.Delete(txn, 40+i, 1)
			}
		}},
		{"an insert inside a run", func(txn *crdt.Transaction, text *crdt.YXmlText) { text.Insert(txn, 10, "xyz", nil) }},
		{"a bold over ten characters", func(txn *crdt.Transaction, text *crdt.YXmlText) { text.Format(txn, 5, 10, strong) }},
	} {
		t.Run(shape.name, func(t *testing.T) {
			room := loadAndType(t, state, nil)
			defer runtime.KeepAlive(room)
			before, encodedBefore := room.replica.roomStateBytes(), len(crdt.EncodeStateAsUpdateV1(room.live, nil))
			frame := writeAsServer(t, room, shape.write)
			t.Logf("the write grew the room's encoded state by %d bytes on a frame of %d, and its tally by %d",
				len(crdt.EncodeStateAsUpdateV1(room.live, nil))-encodedBefore, frame, room.replica.roomStateBytes()-before)
			requireTallyCovers(t, room, "the server's write")
		})
	}
}

// The replica is the room, or a copy of it no finer than the room: one started from an encode of
// the room rather than from the room's own load (start, startFromRead) decodes each run of a
// client's text into one struct, where the room holds one for each transaction that typed it
// (crdt.Doc.Clone's notes). Here the room's run was typed a character a transaction before its
// replica started from the log's first base, so the two differ, and a server's delete inside
// the run splits the replica's one struct where the room needed no split at all: the replica's
// report is above the room's growth, which keeps the tally above the room's state.
func TestAReplicaCoarserThanItsRoomCountsAServerWriteNoLowerThanTheRoomGrew(t *testing.T) {
	live := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(live, seededState(t), nil); err != nil {
		t.Fatal(err)
	}
	text := paragraphTexts(live)[0]
	end := text.Len()
	for i := range 60 {
		live.Transact(func(txn *crdt.Transaction) { text.Insert(txn, end+i, string(rune('a'+i%26)), nil) })
	}
	room := residentRoomOf(t, live, false)
	defer runtime.KeepAlive(room)
	before := room.replica.roomStateBytes()
	encodedBefore := len(crdt.EncodeStateAsUpdateV1(live, nil))
	writeAsServer(t, room, func(txn *crdt.Transaction, text *crdt.YXmlText) { text.Delete(txn, end+10, 1) })
	room.replica.lockForUpdate()
	replicaBytes := len(crdt.EncodeStateAsUpdateV1(room.replica.doc, nil))
	room.replica.mu.Unlock()
	liveBytes := len(crdt.EncodeStateAsUpdateV1(live, nil))
	if replicaBytes == liveBytes {
		t.Fatalf("the replica and the room both encode to %d bytes, so this shape no longer holds a replica coarser than its room", liveBytes)
	}
	t.Logf("the room encodes to %d bytes and its replica to %d; the write grew the room by %d and its tally by %d",
		liveBytes, replicaBytes, liveBytes-encodedBefore, room.replica.roomStateBytes()-before)
	requireTallyCovers(t, room, "a server's delete inside a run the replica holds as one struct")
}

// A commit the replica cannot measure leaves the room's tally owed, never charged by the bytes
// its update logs, which are no bound on what it added; the drain that hands it over re-reads
// the room itself. The replica's document reports, so it applies as a bounded document does and
// refuses a subdocument, which the room takes from the server's own transaction: that refusal
// sends the replica to recover, the one path where a commit goes unmeasured.
func TestACommitTheReplicaCannotMeasureIsReReadFromTheRoom(t *testing.T) {
	room := loadAndType(t, seededState(t), nil)
	defer runtime.KeepAlive(room)
	margin := room.live.GetMap(marksMapName)
	room.live.Transact(func(txn *crdt.Transaction) { margin.Set(txn, "subdocument", crdt.New()) }, &struct{ write string }{"server"})
	room.replica.lockForUpdate()
	recovering := room.replica.doc == nil
	room.replica.mu.Unlock()
	if !recovering {
		t.Fatal("the replica took the subdocument, so no commit here went unmeasured")
	}
	if room.replica.applies().owed.Load() {
		t.Fatal("the drain left the tally owed, want it re-read from the room, which holds nothing pending")
	}
	encoded := int64(len(crdt.EncodeStateAsUpdateV1(room.live, nil)))
	if tracked := room.replica.roomStateBytes(); tracked != encoded {
		t.Fatalf("the room's tally is %d bytes after a commit its replica could not measure, want the %d its re-read of the room found", tracked, encoded)
	}
}

// stateOf is the stored state of a document holding markdown.
func stateOf(t *testing.T, markdown string) []byte {
	t.Helper()
	tree, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatal(err)
	}
	state, err := encodeDocumentTree(tree)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

// textOf is the text of live's first paragraph.
func textOf(t *testing.T, live *crdt.Doc) string {
	t.Helper()
	texts := paragraphTexts(live)
	if len(texts) == 0 {
		t.Fatal("the document holds no paragraph")
	}
	return texts[0].ToString()
}

// residentRoomOf makes live a room as a load does (loadAndType), from live as it stands: its
// update log opened, its replica kept, charged at an encode of it, and an observer draining the
// log into the replica. copied says whether the replica starts from a decode of that encode, as
// a load's own copy does, or waits for the log's first base.
func residentRoomOf(t *testing.T, live *crdt.Doc, copied bool) *typedResidentRoom {
	t.Helper()
	state := crdt.EncodeStateAsUpdateV1(live, nil)
	service := calibratedService()
	service.budget.limit = roomBudgetBytes
	var decoded decodedState
	if copied {
		var err error
		if decoded, err = decodeStoredCopy(state, maxUpdateItems); err != nil {
			t.Fatalf("decode the stored state's copy: %v", err)
		}
	}
	replica := service.keepReplica(live, live.OpenUpdateLog(), liveRendering(t, live), decoded.doc)
	if copied {
		replica.scans.rescanned(decoded.scans)
	}
	charge := service.chargeLoadedRoom("room", replica, int64(len(state)))
	live.OnUpdate(func([]byte, any) { replica.observe("room") })
	return &typedResidentRoom{residentRoom: residentRoom{service: service, live: live, replica: replica}, charge: charge}
}

// paragraphEdit is the update a browser holding state sends for edit, made on its first
// paragraph.
func paragraphEdit(t *testing.T, state []byte, edit func(txn *crdt.Transaction, text *crdt.YXmlText)) []byte {
	t.Helper()
	browser := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(browser, state, nil); err != nil {
		t.Fatal(err)
	}
	text := paragraphTexts(browser)[0]
	before := browser.StateVector()
	browser.Transact(func(txn *crdt.Transaction) { edit(txn, text) })
	return crdt.EncodeStateAsUpdateV1(browser, before)
}

// weighAsBrowser hands update to the room's hook as the browser origin's, applied as the room
// applies a peer's update, and requires it admitted.
func weighAsBrowser(t *testing.T, room *typedResidentRoom, update []byte, origin any) {
	t.Helper()
	info := websocket.PeerUpdateInfo{Room: "room", Doc: room.live, Update: update, Origin: origin}
	if err := room.service.weighPeerUpdate(info, func() error { return crdt.ApplyUpdateV1(room.live, update, origin) }); err != nil {
		t.Fatalf("the hook answered %v, want the update admitted", err)
	}
}

// writeAsServer makes write on the room's first paragraph in a transaction of the room's own, as
// a server write is made, and returns the bytes its commit logged.
func writeAsServer(t *testing.T, room *typedResidentRoom, write func(txn *crdt.Transaction, text *crdt.YXmlText)) int {
	t.Helper()
	text := paragraphTexts(room.live)[0]
	log := room.live.OpenUpdateLog()
	defer log.Close()
	room.live.Transact(func(txn *crdt.Transaction) { write(txn, text) }, &struct{ write string }{"server"})
	_, logged := log.Drain()
	frame := 0
	for _, update := range logged {
		frame += len(update.Update) + len(update.Parked)
	}
	return frame
}

// requireTallyCovers requires the room's tracked state to be at or above an encode of the room's
// own document, after what.
func requireTallyCovers(t *testing.T, room *typedResidentRoom, what string) {
	t.Helper()
	tracked := room.replica.roomStateBytes()
	encoded := int64(len(crdt.EncodeStateAsUpdateV1(room.live, nil)))
	t.Logf("after %s the room's tally is %d bytes and an encode of it holds %d, %d over", what, tracked, encoded, tracked-encoded)
	if tracked < encoded {
		t.Fatalf("after %s the room's tally is %d bytes, %d under the %d an encode of the room holds", what, tracked, encoded-tracked, encoded)
	}
}
