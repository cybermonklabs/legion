package docs

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// loadDocumentHeld is the service's store, noting how many bytes of the pod's load concurrency
// were held as each of its LoadDocument reads of room returned.
type loadDocumentHeld struct {
	VersionedStore
	slotsHeld
	room  string
	slots *loadSlots
}

func (s *loadDocumentHeld) LoadDocument(ctx context.Context, room string) (LoadedDocument, error) {
	loaded, err := s.VersionedStore.LoadDocument(ctx, room)
	if room == s.room {
		s.record(s.slots.held.Load())
	}
	return loaded, err
}

// A read of the stored document (storedWithUpdates) keeps the grant its load took: the load hands
// the document back still holding it, the merge of what a room behind the stored head has not
// stored yet and the build of the tree run on it, and the read gives it back once the tree is
// built. A merge of a heavy share - a run of keystrokes a room took one transaction each, past
// unslottedApplyStructs structs - raises that grant by the share's own weight before it decodes
// any of it (heavyUpdateWeight), and the grant then holds both. Before, the load gave its grant
// back as it returned, so the document, the merge and the tree were built beside the pool's
// holders, whatever the share weighed.
func TestAStoredReadMergesAndBuildsItsTreeOnItsLoadsGrant(t *testing.T) {
	ctx := context.Background()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	state := proseState(t, 16<<10)
	storeHistory(t, service, artifactID, state)

	room := copyOf(t, state)
	stored := docMarkdown(room)
	storedClocks := room.StateVector()
	const typist crdt.ClientID = 0x7e57
	chainedRun(t, room, lastCharacter(t, room), typist, unslottedApplyStructs+100)
	share := crdt.EncodeStateAsUpdateV1(room, storedClocks)
	merge, heavy := heavyUpdateWeight(share)
	if !heavy {
		t.Fatalf("a share of %d bytes declaring %d structs is light, so nothing here is tested", len(share), declaredStructs(share))
	}

	slots := useLoadSlots(t, newLoadSlots(1))
	var taken slotsHeld
	slots.taken = taken.record
	loads := &loadDocumentHeld{VersionedStore: service.persistence, room: artifactID, slots: slots}
	service.persistence = loads

	for _, read := range []struct {
		name     string
		unstored [][]byte
		want     string
		// raised says whether the read raises its load's grant before the merge.
		raised bool
	}{
		{name: "the stored document alone (storedTree)", want: stored},
		{name: "the stored document with a heavy share merged in", unstored: [][]byte{share}, want: docMarkdown(room), raised: true},
	} {
		before, loadsBefore := len(taken.all()), len(loads.all())
		tree, err := service.storedWithUpdates(ctx, artifactID, read.unstored)
		if err != nil {
			t.Fatalf("%s: %v", read.name, err)
		}
		if markdown, err := renderTree(tree); err != nil || markdown != read.want {
			t.Fatalf("%s rendered %d bytes (%v), want the %d the document holds", read.name, len(markdown), err, len(read.want))
		}
		grants := taken.all()[before:]
		returned := loads.all()[loadsBefore:]
		if len(grants) == 0 || grants[0] <= 0 {
			t.Fatalf("%s took grants leaving %v held, want its load's", read.name, grants)
		}
		load := grants[0]
		if !slices.Equal(returned, []int64{load}) {
			t.Fatalf("%s: the pool held %v as its load returned, want the %d its load took still held, for the merge and the tree to run on",
				read.name, returned, load)
		}
		want := []int64{load}
		if read.raised {
			want = append(want, min(load+merge, loadSlotBytes))
		}
		if !slices.Equal(grants, want) {
			t.Fatalf("%s took grants leaving %v held, want %v: the load's, and the share's %d bytes added to it before the merge", read.name, grants, want, merge)
		}
		if held := slots.held.Load(); held != 0 {
			t.Fatalf("%s still holds %d bytes once its tree is built", read.name, held)
		}
		t.Logf("%s: its load took %d bytes and still held them as it returned; grants left %v held", read.name, load, grants)
	}
}

// storedJudgement is a service whose document's history is one stored row of prose, which a load
// reads taking no grant of its own (foldRead), and a published write of that document whose loss
// check claims the stored text: its writer's runs, every block of them claimed by its one
// operation, which the stored document still holds. It answers the write and what a decode of the
// stored state costs the pool (storedCopyWeight).
func storedJudgement(t *testing.T) (*Service, *liveWrite, int64) {
	t.Helper()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	storeHistory(t, service, artifactID, proseState(t, 16<<10))
	loaded, err := service.persistence.Load(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("read the stored state: %v", err)
	}
	written := copyOf(t, loaded.Update)
	clocks := written.StateVector()
	if len(clocks) != 1 {
		t.Fatalf("the stored state names %d clients, want the one that wrote it", len(clocks))
	}
	var writer crdt.ClientID
	for client := range clocks {
		writer = client
	}
	inserted, err := pmdoc.AuthoredTextRuns(written.GetXmlFragment(fragmentName), writer, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	blocks := map[string]struct{}{}
	for block := range inserted {
		blocks[block] = struct{}{}
	}
	loss := newLossCheck(writer, 0, []map[string]struct{}{blocks}, inserted)
	if loss == nil {
		t.Fatal("the stored text claims no block, so the judgement would read nothing")
	}
	return service, &liveWrite{artifactID: artifactID, loss: loss}, storedCopyWeight(loaded.Update)
}

// A published write's judgement on the stored document (lostAtStoredHead), where no room of this
// process holds it, decodes and walks that document under a grant of what its copy costs
// (storedCopyWeight): with the pool full, the judgement waits rather than decoding beside the
// holder, and once those bytes are given back within its bound it takes that weight, reaches its
// verdict and gives it back. Before, the decode took nothing, after the publish's transaction had
// given its own grant back.
func TestAPublishedWritesJudgementOnTheStoredDocumentWaitsForAGrantOfItsCopy(t *testing.T) {
	service, write, weight := storedJudgement(t)
	waiting := make(chan struct{})
	slots, _ := poolOfOneSlot(t, sync.OnceFunc(func() { close(waiting) }))
	holder := fillPoolBut(t, slots, 0)
	var taken slotsHeld
	slots.taken = taken.record

	judged := make(chan struct{})
	go func() {
		defer close(judged)
		service.recordPublishedLoss(write)
	}()
	select {
	case <-waiting:
	case <-judged:
		t.Fatal("the judgement reached its verdict while the only slot was held: it decoded the stored document beside the pool's holder")
	case <-time.After(30 * time.Second):
		t.Fatal("the judgement neither waited for a slot nor reached its verdict in 30 s")
	}
	select {
	case <-judged:
		t.Fatal("the judgement reached its verdict while the only slot was held")
	case <-time.After(200 * time.Millisecond):
	}
	holder.release()
	select {
	case <-judged:
	case <-time.After(30 * time.Second):
		t.Fatal("the judgement did not reach its verdict in 30 s once the slot was free")
	}
	if !write.lostVerdict || len(write.lost) != 0 {
		t.Fatalf("the judgement's verdict = lost %v (reached %t), want nothing lost: the stored document holds the write's text", write.lost, write.lostVerdict)
	}
	if got := taken.all(); !slices.Equal(got, []int64{weight}) {
		t.Fatalf("the judgement took grants leaving %v held, want one of %d, what the stored document's copy costs", got, weight)
	}
	if held := slots.held.Load(); held != 0 {
		t.Fatalf("the judgement still holds %d bytes once it reached its verdict", held)
	}
	t.Logf("the judgement waited for the pool, then took %d bytes for the stored document's copy", weight)
}

// A published write's judgement whose grant the pool does not give within publishedLossTimeout
// ends there: the wait is the read's error, no decode of the stored document runs, the write is
// left with no verdict, and its transaction's LostOps answers false, which the caller reports as
// undetermined (LEGION-269), as after a flush or a load that fails. A wait with no deadline would
// hold the write's writer slot, which a joined transaction of the server's own holding its first
// document's grant can wait for with no bound of its own.
func TestAPublishedWritesJudgementWhoseGrantOutwaitsItsBoundReachesNoVerdict(t *testing.T) {
	bound := publishedLossTimeout
	publishedLossTimeout = time.Second
	t.Cleanup(func() { publishedLossTimeout = bound })
	logs := captureLogs(t)
	service, write, _ := storedJudgement(t)
	ledger := &Ledger{live: map[string]*liveWrite{write.artifactID: write}}
	slots, waited := poolOfOneSlot(t, nil)
	holder := fillPoolBut(t, slots, 0)
	var taken slotsHeld
	slots.taken = taken.record

	judged := make(chan struct{})
	began := time.Now()
	go func() {
		defer close(judged)
		service.recordPublishedLoss(write)
	}()
	select {
	case <-judged:
	case <-time.After(10 * time.Second):
		holder.release()
		<-judged
		t.Fatalf("the judgement still waited for the pool 10 s into its %s bound, and ended only once the slot was given back (verdict reached %t)",
			publishedLossTimeout, write.lostVerdict)
	}
	ended := time.Since(began)
	if !waited.Load() {
		t.Fatal("the judgement ended without waiting for the pool, so nothing here is tested")
	}
	if got := taken.all(); len(got) != 0 {
		t.Fatalf("the judgement took grants leaving %v held while the pool was full: a decode ran", got)
	}
	if lost, known := ledger.LostOps(write.artifactID); known {
		t.Fatalf("LostOps = %v with a verdict, want none from a judgement that could not read the document within its bound", lost)
	}
	line := logs.Lines("wait for the decode's grant")
	if !strings.Contains(line, context.DeadlineExceeded.Error()) {
		t.Fatalf("the judgement logged %q, want its wait for the decode's grant ended by its bound", line)
	}
	if held := slots.held.Load(); held != loadSlotBytes {
		t.Fatalf("the pool holds %d bytes once the judgement ended, want the holder's %d alone", held, loadSlotBytes)
	}
	t.Logf("the judgement ended %s after it began, against its %s bound, with no verdict: %s", ended.Round(time.Millisecond), publishedLossTimeout, strings.TrimSpace(line))
}

// A read of a room behind the stored head hands storedWithUpdates the updates that room holds and
// the store may not, each as the room committed it (unstoredParts), and they are merged there on
// the load's grant: the merge decodes every struct of every one of them (crdt.MergeUpdatesV1), so
// where any of them is heavy the read raises the grant by what their decodes cost, summed
// (mergeWeight), before it merges anything, and the apply of what the merge made then raises it by
// what that decode costs past the merge's raise (heavyUpdateWeight). A share of light keystrokes
// takes nothing at either. Before, the caller merged the updates on no grant at all
// (unstoredShare, inside the room's Apply for docTree), and the read weighed the merged update's
// apply alone.
func TestAStoredReadRaisesItsGrantForTheMergeOfARoomsUpdatesBeforeMergingThem(t *testing.T) {
	ctx := context.Background()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	state := proseState(t, 16<<10)
	storeHistory(t, service, artifactID, state)

	// room stands for the resident room: each update below is one commit of its own, as the room
	// lists them unstored.
	room := copyOf(t, state)
	commit := func(edit func()) []byte {
		t.Helper()
		before := room.StateVector()
		edit()
		return crdt.EncodeStateAsUpdateV1(room, before)
	}
	keystrokes := func(first crdt.ClientID, count int) [][]byte {
		t.Helper()
		var updates [][]byte
		for i := range count {
			updates = append(updates, commit(func() { chainedRun(t, room, lastCharacter(t, room), first+crdt.ClientID(i), 1) }))
		}
		return updates
	}
	light := keystrokes(0x5000, 10)
	lightText := docMarkdown(room)
	heavyA := commit(func() { chainedRun(t, room, lastCharacter(t, room), 0x7e57, unslottedApplyStructs+100) })
	between := keystrokes(0x6000, 10)
	heavyB := commit(func() { chainedRun(t, room, lastCharacter(t, room), 0x7e58, unslottedApplyStructs+100) })
	mixed := slices.Concat(light, [][]byte{heavyA}, between, [][]byte{heavyB})
	for _, update := range [][]byte{heavyA, heavyB} {
		if _, heavy := heavyUpdateWeight(update); !heavy {
			t.Fatalf("a run of %d bytes declaring %d structs is light, so nothing here is tested", len(update), declaredStructs(update))
		}
	}

	slots := useLoadSlots(t, newLoadSlots(1))
	var taken slotsHeld
	slots.taken = taken.record
	loads := &loadDocumentHeld{VersionedStore: service.persistence, room: artifactID, slots: slots}
	service.persistence = loads

	for _, read := range []struct {
		name     string
		unstored [][]byte
		want     string
	}{
		{name: "a share of light keystrokes", unstored: light, want: lightText},
		{name: "a share of two heavy runs among light keystrokes", unstored: mixed, want: docMarkdown(room)},
	} {
		merged, err := mergeUpdates(read.unstored)
		if err != nil {
			t.Fatalf("%s: merge the updates: %v", read.name, err)
		}
		merge := mergeWeight(read.unstored)
		apply, applyHeavy := heavyUpdateWeight(merged)
		before, loadsBefore := len(taken.all()), len(loads.all())
		tree, err := service.storedWithUpdates(ctx, artifactID, read.unstored)
		if err != nil {
			t.Fatalf("%s: %v", read.name, err)
		}
		if markdown, err := renderTree(tree); err != nil || markdown != read.want {
			t.Fatalf("%s rendered %d bytes (%v), want the %d the room holds", read.name, len(markdown), err, len(read.want))
		}
		grants := taken.all()[before:]
		if len(grants) == 0 || grants[0] <= 0 {
			t.Fatalf("%s took grants leaving %v held, want its load's first", read.name, grants)
		}
		load := grants[0]
		if returned := loads.all()[loadsBefore:]; !slices.Equal(returned, []int64{load}) {
			t.Fatalf("%s: the pool held %v as its load returned, want the %d its load took still held", read.name, returned, load)
		}
		want := []int64{load}
		if merge > 0 {
			want = append(want, min(load+merge, loadSlotBytes))
		}
		if applyHeavy && apply > merge {
			want = append(want, min(load+apply, loadSlotBytes))
		}
		if !slices.Equal(grants, want) {
			t.Fatalf("%s took grants leaving %v held, want %v: the load's, then the %d bytes the merge decodes before it merges, then what the apply of its %d-byte update decodes past them (%d, heavy %t)",
				read.name, grants, want, merge, len(merged), apply, applyHeavy)
		}
		if held := slots.held.Load(); held != 0 {
			t.Fatalf("%s still holds %d bytes once its tree is built", read.name, held)
		}
		t.Logf("%s: %d updates, the merge weighed %d bytes and its apply %d (heavy %t); grants left %v held", read.name, len(read.unstored), merge, apply, applyHeavy, grants)
	}
	if mergeWeight(mixed) == 0 {
		t.Fatal("the mixed share's merge weighs nothing, so no raise before the merge was tested")
	}
}

// A share another process asks for (unstoredShare) merges the updates the room holds and the store
// may not on a grant of the pod's load concurrency of its own, since the goroutine answering it
// carries none: what the heavy ones' decodes cost, summed (mergeWeight), taken before the merge
// and given back once it is made. Where the pool does not give it before the share's context
// ends, the share answers that context's error and merges nothing. Before, the merge took nothing,
// whatever it decoded.
func TestAShareAnsweredToAnotherProcessMergesOnAGrantOfItsOwn(t *testing.T) {
	ctx := context.Background()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	storeHistory(t, service, artifactID, proseState(t, 16<<10))
	if err := loadRoomOf(service, artifactID); err != nil {
		t.Fatalf("load the room: %v", err)
	}
	live := service.srv.GetDoc(artifactID)
	if live == nil {
		t.Fatal("the room did not load")
	}
	// The room's persistence is held at its store of the first commit, so it and every commit
	// after it stay listed among the room's not-yet-stored updates.
	appending, _ := holdNextAppend(t, service, artifactID)
	writer := copyOf(t, crdt.EncodeStateAsUpdateV1(live, nil))
	commit := func(edit func()) {
		t.Helper()
		before := writer.StateVector()
		edit()
		if err := crdt.ApplyUpdateV1(live, crdt.EncodeStateAsUpdateV1(writer, before), "peer"); err != nil {
			t.Fatalf("commit to the room: %v", err)
		}
	}
	commit(func() { chainedRun(t, writer, lastCharacter(t, writer), 0x5000, 1) })
	select {
	case <-appending:
	case <-time.After(roomWait(t)):
		t.Fatal("the room's persistence never reached its store of the first commit")
	}
	// Each heavy commit types its characters one after the first character of the paragraph,
	// every one before the one typed just ahead of it, so none merges with its neighbour as the
	// room integrates the commit and the update the room lists holds every one as a struct.
	run := func() {
		text := paragraphTexts(writer)[0]
		writer.Transact(func(txn *crdt.Transaction) {
			for range unslottedApplyStructs + 100 {
				text.Insert(txn, 1, "x", nil)
			}
		})
	}
	commit(run)
	commit(run)
	parts := service.unstoredParts(artifactID, live)
	weight := mergeWeight(parts)
	if len(parts) != 3 || weight == 0 {
		t.Fatalf("the room lists %d updates not yet stored whose merge weighs %d bytes, want the 3 commits, two of them heavy", len(parts), weight)
	}
	want, err := mergeUpdates(parts)
	if err != nil {
		t.Fatalf("merge the room's updates: %v", err)
	}

	slots := useLoadSlots(t, newLoadSlots(1))
	var taken slotsHeld
	slots.taken = taken.record
	share, err := unstoredShare(ctx, parts, weight)
	if err != nil {
		t.Fatalf("share the room's updates: %v", err)
	}
	if !bytes.Equal(share, want) {
		t.Fatalf("the share is %d bytes, want the %d-byte merge of the room's updates", len(share), len(want))
	}
	if got := taken.all(); !slices.Equal(got, []int64{weight}) {
		t.Fatalf("the share took grants leaving %v held, want one of the %d bytes its merge decodes", got, weight)
	}
	if held := slots.held.Load(); held != 0 {
		t.Fatalf("the share still holds %d bytes once it has merged", held)
	}

	holder := fillPoolBut(t, slots, 0)
	bounded, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if _, err := unstoredShare(bounded, parts, weight); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the share with the pool full answered %v, want its context's end", err)
	}
	if got := taken.all(); len(got) != 2 {
		t.Fatalf("the pool's grants left %v held, want the first share's and the holder's alone: the refused share took one", got)
	}
	holder.release()
	if held := slots.held.Load(); held != 0 {
		t.Fatalf("the pool holds %d bytes once the holder gave its own back", held)
	}
	t.Logf("the share of %d updates took %d bytes before its merge, and waited past its context with the pool full", len(parts), weight)
}

// A cold read's fold keeps the grant its load took (foldDocument): the load hands the document
// back still holding it, foldLoaded reads its tree, renders it and walks it on that grant, and the
// fold gives it back once it returns. Before, the load gave its grant back as it returned, and the
// document was read, rendered and walked beside the pool's holders.
func TestAColdReadReadsAndRendersWhatItFoldedOnItsLoadsGrant(t *testing.T) {
	ctx := context.Background()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	state := proseState(t, 16<<10)
	storeHistory(t, service, artifactID, state)
	if service.srv.GetDoc(artifactID) != nil {
		t.Fatal("the document's room is resident, want a cold read of the stored history")
	}
	slots := useLoadSlots(t, newLoadSlots(1))
	var taken slotsHeld
	slots.taken = taken.record
	loads := &loadDocumentHeld{VersionedStore: service.persistence, room: artifactID, slots: slots}
	service.persistence = loads

	read, _, err := service.coldRead(ctx, artifactID)
	if err != nil || read == nil {
		t.Fatalf("the cold read answered %v (rendered %t), want the stored document rendered", err, read != nil)
	}
	if want := docMarkdown(copyOf(t, state)); read.markdown != want {
		t.Fatalf("the cold read rendered %d bytes, want the %d the stored document holds", len(read.markdown), len(want))
	}
	grants := taken.all()
	if len(grants) == 0 || grants[0] <= 0 {
		t.Fatalf("the cold read took grants leaving %v held, want its load's", grants)
	}
	if returned := loads.all(); !slices.Equal(returned, []int64{grants[0]}) {
		t.Fatalf("the pool held %v as the fold's load returned, want the %d its load took still held, for the fold to read and render the document on",
			returned, grants[0])
	}
	if held := slots.held.Load(); held != 0 {
		t.Fatalf("the cold read still holds %d bytes once it has answered", held)
	}
	t.Logf("the fold's load took %d bytes and still held them as it returned; grants left %v held", grants[0], grants)
}

// A rebuild reads what it builds from out of the stored history it decoded - its margin records
// and its tree (readRebuildHistory) - on the grant that decode took, the history's own weight
// (storedCopyWeight), and gives that grant back with the decoded document before it waits for the
// document owner's lock. The rebuild here is of a resident room near the state cap, whose history
// the rebuild decodes once it has closed the room. Before, rebuildSource gave its grant back as it
// returned the decoded history, and RebuildDocument read the margin records and the tree, and
// held the document, on no grant.
func TestARebuildReadsItsDecodedHistorysMarginAndTreeOnTheDecodesGrant(t *testing.T) {
	ctx := context.Background()
	service, artifactID := newTestService(t)
	const markdown = "# Resident\n\nA room the hook refuses an ordinary edit in.\n"
	seedServiceText(t, service, artifactID, markdown)
	if err := loadRoomOf(service, artifactID); err != nil {
		t.Fatal(err)
	}
	live := service.srv.GetDoc(artifactID)
	if live == nil {
		t.Fatal("the room did not load")
	}
	service.roomWeighingOf(live).applies().loaded(maxRoomStateBytes - 1<<10)
	loaded, err := service.persistence.Load(ctx, artifactID)
	if err != nil {
		t.Fatal(err)
	}
	weight := storedCopyWeight(loaded.Update)

	slots := useLoadSlots(t, newLoadSlots(1))
	var mu sync.Mutex
	var atReads []int64
	service.afterRebuildRead = func(room string) {
		if room == artifactID {
			mu.Lock()
			atReads = append(atReads, slots.held.Load())
			mu.Unlock()
		}
	}
	if _, _, err := rebuildDocument(t, service, artifactID, nil); err != nil {
		t.Fatalf("rebuild the resident room near the state cap: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(atReads, []int64{weight}) {
		t.Fatalf("the pool held %v as the rebuild read its decoded history's margin records and tree, want the %d its decode took, still held",
			atReads, weight)
	}
	if held := slots.held.Load(); held != 0 {
		t.Fatalf("the rebuild still holds %d bytes once it has answered", held)
	}
	if text, err := service.Text(ctx, artifactID); err != nil || text != markdown {
		t.Fatalf("the rebuilt document reads %q (%v), want %q", text, err, markdown)
	}
	t.Logf("the rebuild read its decoded history's margin records and tree holding %d bytes of the pool", weight)
}
