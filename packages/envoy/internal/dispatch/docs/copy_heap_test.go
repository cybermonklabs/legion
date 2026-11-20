package docs

import (
	"context"
	"errors"
	"math"
	"os"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	"github.com/reearth/ygo/persistence"
	"github.com/reearth/ygo/provider/websocket"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// copyHeapMeasurement is the environment variable that runs the load-slot measurement's heavy
// rows (TestEveryFullStateDecodesTransientHeapIsWithinItsSlot).
const copyHeapMeasurement = "DISPATCH_MEASURE_COPY_HEAP"

// keystrokeRunStates are the states of a room that took a run of a million keystrokes, each its
// own item, after the first paragraph of a small document, and of the same room once three
// one-character frames each named a run item as its left origin and the run item a third of a
// million later as its right origin: a decode of the second scans 1,000,002 distinct items, each
// with its own scan mark, the whole of roomScanTotal, the heaviest shape a decode re-scans.
//
// The run is six keystrokes past a million so that those three frames carry the whole total
// themselves. Under sjawhar/ygo v1.51.3-sami.7 the seed document's own decode counted four scans,
// and a run of 1,000,000 reached the total with them; under v1.55.1-sami.1 that decode counts
// none, measured as the unscanned state's re-scan in
// TestACopyOfARoomAtTheScanTotalCostsACopyOfItsStateAndAboutASecondOfScans.
func keystrokeRunStates(t *testing.T) (keystrokes, scanned []byte) {
	t.Helper()
	const runner, scanner crdt.ClientID = 1_000, 4_000_000
	const run = 1_000_006
	tree, err := pmdoc.Parse("# Title\n\nOne paragraph of ordinary prose.\n")
	if err != nil {
		t.Fatal(err)
	}
	seed, err := encodeDocumentTree(tree)
	if err != nil {
		t.Fatal(err)
	}
	room := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(room, seed, nil); err != nil {
		t.Fatal(err)
	}
	chainedRun(t, room, lastCharacter(t, room), runner, run)
	keystrokes = crdt.EncodeStateAsUpdateV1(room, nil)
	span := uint64(run / 3)
	for k := range uint64(3) {
		frame := newV1Frame(1).group(1, uint64(scanner)+k, 0).
			raw(structStringRef | infoHasOrigin | infoHasRightOrigin).
			uint(uint64(runner)).uint(k * span).uint(uint64(runner)).uint((k + 1) * span).str("z").noDeletes()
		if err := crdt.ApplyUpdateV1(room, frame, nil); err != nil {
			t.Fatal(err)
		}
	}
	return keystrokes, crdt.EncodeStateAsUpdateV1(room, nil)
}

// emptyUpdate is a V1 update of no struct and no deletion: a stored history's second update that
// makes its fold decode the state whole, and adds nothing a document at the decode cap has no room
// for.
var emptyUpdate = []byte{0, 0}

// errCopyMeasured ends the copy measurement's joined transactions without committing them.
var errCopyMeasured = errors.New("the copy was measured")

// copyPath is one full-state decode the copy-heap bound grants, as measureCopyPaths measures it:
// bytes is the encoded state its grant is weighed by, read once prepare has run.
type copyPath struct {
	name    string
	prepare func()
	bytes   func() int
	copy    func() any
}

// copyMeasurementGCPercent is the collector's GOGC while a copy is measured: the heap may grow a
// tenth past what the last collection left live before the next one runs, so what the sampler sees
// at its peak is what the copy holds live, with at most about a tenth more of garbage. At the
// default of 100 the garbage of a load's first decode, left uncollected while its second runs,
// showed the load of a million keystrokes holding 601,928,008 bytes above what it left, against
// 253,699,168 at 10, on 2026-10-28; under the production's memory limit the collector frees
// garbage as the heap nears it, and what it cannot free is what the bound is for.
const copyMeasurementGCPercent = 10

// copyTransient is what one run of a full-state decode holds live above what it leaves behind:
// the most heap its objects held above the heap before it (loadCost.peak), measured at
// copyMeasurementGCPercent, less what it still holds once collected, which is its product - a
// room's two copies, a fork, a replica's copy - and is charged where its owner keeps it, if
// anywhere.
func copyTransient(cost loadCost) int64 {
	return int64(cost.peak) - max(cost.held, 0)
}

// Every full-state decode holds no more heap above what it leaves behind than one slot of the
// pod's load concurrency allows it (loadSlotBytes, 600 MB), which is the most any taker of the
// pool takes however heavy its document (copyWeight). For each shape, at
// GOMAXPROCS=1 (measureLoad), it measures the room's load as the server's own work makes it
// (Server.Apply on a room no one holds, and the room's first transaction) and as a socket's
// admission and load make it; a write's fork from the store (coldFork) and from a resident room
// it has never forked (warmFork's rebuild); the replica's build from a base
// (renderedReplica.start); the copy a server write's decode-cap count takes (structsAfter); a
// rebuild's decode and re-scan of the history (loadDecodedHistory, nearACap); and, with an empty
// update stored after the state, the room's load that folds the history first, a cold read's
// fold (LoadDocument) and compaction's (Compact). Each path starts from the state as the whole
// stored history, since a closed room compacts what its own transactions stored into a state
// re-encoded from its decode.
//
// A taker is weighed per byte of the state its copy is built from and capped at one slot
// (copyWeight): what this holds is that the heaviest decode of the heaviest stored document
// still fits that cap, so the cap never admits a copy the pool has not the room for.
//
// The fast row is a mebibyte of prose. Set DISPATCH_MEASURE_COPY_HEAP=1 for the heavy rows - the
// document an agent rewrote whole until the decode cap refused a write (agentRewrittenState, kept
// between runs in the file DISPATCH_AGENT_REWRITTEN_CACHE names), the long-edited document typed
// on until the cap (longEditedAtCapState), a run of a million keystrokes and the same run scanned
// once whole (keystrokeRunStates), each near or at the decode cap, every one of whose paths must
// also fit one slot, since a stored document must always load. They took 597 to 623 s together
// here on 2026-10-28.
func TestEveryFullStateDecodesTransientHeapIsWithinItsSlot(t *testing.T) {
	skipUnderRace(t)
	heavy := os.Getenv(copyHeapMeasurement) != ""
	var keystrokes, scanned []byte
	runStates := func(t *testing.T) {
		if keystrokes == nil {
			keystrokes, scanned = keystrokeRunStates(t)
		}
	}
	for _, shape := range []struct {
		name  string
		heavy bool
		state func(t *testing.T) []byte
	}{
		{"a mebibyte of prose", false, func(t *testing.T) []byte {
			state, _ := residentRoomState(t, 1<<20)
			return state
		}},
		{"an agent's whole rewrites until the cap refused one", true, cachedAgentRewrittenState},
		{"a long-edited document typed on until the cap", true, longEditedAtCapState},
		{"a run of a million keystrokes", true, func(t *testing.T) []byte { runStates(t); return keystrokes }},
		{"a run of a million keystrokes scanned whole", true, func(t *testing.T) []byte { runStates(t); return scanned }},
	} {
		t.Run(shape.name, func(t *testing.T) {
			if shape.heavy && !heavy {
				t.Skip("set " + copyHeapMeasurement + "=1 to run the heavy rows")
			}
			measureCopyPaths(t, shape.state(t))
		})
	}
}

// measureCopyPaths measures every path TestEveryFullStateDecodesTransientHeapIsWithinItsSlot
// names on a document storing state, and fails a path whose transient passes one load slot.
func measureCopyPaths(t *testing.T, state []byte) {
	ctx := context.Background()
	service, artifactID := newTestService(t)
	service.settle = time.Hour // no settlement runs in the room while it is measured
	closed := func() { closeResidentRoom(t, service, artifactID) }
	// fresh leaves the room closed and state the whole stored history again: a closed room's
	// persistence compacts what its transactions stored into a state of its own.
	fresh := func() {
		closed()
		storeHistory(t, service, artifactID, state)
	}
	resident := func() *crdt.Doc {
		if err := service.srv.Apply(ctx, artifactID, func(*crdt.Doc, func(func(*crdt.Transaction))) {}); err != nil && !errors.Is(err, websocket.ErrNoChanges) {
			t.Fatalf("load the room: %v", err)
		}
		return roomDocument(t, service, artifactID)
	}
	load := func() any {
		live := resident()
		live.Transact(func(*crdt.Transaction) {})
		return live
	}
	fork := func() any {
		var fork *crdt.Doc
		err := joinedWrite(service, func(ctx context.Context) error {
			write, err := service.joinLiveWrite(ctx, ledgerFrom(ctx), artifactID)
			if err != nil {
				return err
			}
			if fork, err = service.forkLive(ctx, write); err != nil {
				return err
			}
			return errCopyMeasured
		})
		if !errors.Is(err, errCopyMeasured) {
			t.Fatalf("fork the document: %v", err)
		}
		return fork
	}
	stored := func() int { return len(state) }
	var live *crdt.Doc
	liveState := func() int { return len(crdt.EncodeStateAsUpdateV1(live, nil)) }
	var markdown string
	// appended is fresh with an empty update stored after the state, so the next read of the
	// history folds two updates.
	appended := func() {
		fresh()
		if _, err := service.persistence.AppendUpdate(ctx, artifactID, emptyUpdate); err != nil {
			t.Fatal(err)
		}
		if stored := storedUpdateCount(t, service.store, artifactID); stored != 2 {
			t.Fatalf("the history holds %d updates, want the state and the update after it", stored)
		}
	}
	// history is the stored updates' bytes, or the state their fold makes where that is larger.
	history := func() int {
		loaded, err := service.persistence.Load(ctx, artifactID)
		if err != nil {
			t.Fatal(err)
		}
		return max(storedUpdateBytes(t, service.store, artifactID), len(loaded.Update))
	}
	paths := []copyPath{
		{"the server's load and first transaction", fresh, stored, load},
		{"a socket's admission, load and first transaction", fresh, stored, func() any {
			tree, loaded, _, err := service.loadTree(ctx, artifactID)
			if err != nil {
				t.Fatalf("the admission's schema check: %v", err)
			}
			rendering, err := replicaRenderingOf(tree)
			if err != nil {
				t.Fatalf("the admission's rendering: %v", err)
			}
			defer service.offerPreload(artifactID, loaded, admissionRendering{tree: tree, rendering: rendering})()
			return load()
		}},
		{"a write's fork of the stored history", fresh, stored, fork},
		{"a write's first fork of the resident room", func() { fresh(); live = resident() }, liveState, fork},
		{"the replica's build from a base", func() {
			rendered, err := renderDocument(copyOf(t, state))
			if err != nil {
				t.Fatal(err)
			}
			markdown = rendered
		}, stored, func() any {
			replica := &renderedReplica{rendering: &replicaRendering{markdown: markdown}, roomWeighing: &roomWeighing{}, paths: &service.renderPaths}
			replica.start(artifactID, state)
			if replica.doc == nil {
				t.Fatal("the replica took no copy of the base")
			}
			return replica
		}},
		{"the copy a server write's decode-cap count takes", func() { fresh(); live = resident() }, liveState, func() any {
			return structsAfter(ctx, live, serverWrite{apply: func(*crdt.Doc) {}}, service.copyOfRoom(live))
		}},
		{"a rebuild's decode and re-scan of the history", func() {}, stored, func() any {
			history := decodedHistory(t, state)
			if _, _, err := nearACap(ctx, history, copyWeight(int64(len(state)), 0), int64(len(state))); err != nil {
				t.Fatal(err)
			}
			return history
		}},
		{"the server's load of a history it folds first", appended, history, load},
		{"a cold read's fold of the history", appended, history, func() any {
			loaded, err := service.persistence.LoadDocument(ctx, artifactID)
			if err != nil {
				t.Fatal(err)
			}
			return loaded.Doc
		}},
		// The cold read loaded no room, so the history it folded is still the two updates. The
		// keep-1 rule folds them now: the tail rule would pass over rows this young (compactTail).
		{"compaction's fold of the history", func() {}, history, func() any {
			folded, err := service.persistence.compactRoom(ctx, artifactID, compactRule{keep: 1}, true)
			if err != nil || folded.deleted != 1 {
				t.Fatalf("compaction folded %d updates (%v), want 1", folded.deleted, err)
			}
			return nil
		}},
	}
	for _, path := range paths {
		path.prepare()
		bytes := path.bytes()
		cost := func() loadCost {
			defer debug.SetGCPercent(debug.SetGCPercent(copyMeasurementGCPercent))
			return measureLoad(t, path.copy)
		}()
		live = nil
		transient := copyTransient(cost)
		t.Logf("%d-byte state, %s: %s, %d bytes allocated, %d peak, %d held; %d transient, %.1f a byte of %d, against a %d-byte slot",
			len(state), path.name, cost.took.Round(time.Millisecond), cost.allocated, cost.peak, cost.held,
			transient, float64(transient)/float64(bytes), bytes, loadSlotBytes)
		if transient > loadSlotBytes {
			t.Errorf("%s holds %d bytes above what it leaves, %.1f a byte of its %d, past the %d bytes one load slot allows",
				path.name, transient, float64(transient)/float64(bytes), bytes, loadSlotBytes)
		}
	}
}

// stateAnswerMessageBytes is how long ygo's server lets one sync message of a state answer be on
// a y-websocket connection: the 64 KiB one frame of the answer may be, less the one byte of the
// frame's own message type (theorem-labs/ygo v1.55.1-theorem.2, provider/websocket/peer.go,
// stateFrameBytes and stateMessageBytes).
const stateAnswerMessageBytes = 64<<10 - 1

// stateAnswerParker is the client whose struct the state-answer measurement parks in a document
// and lands again, one clock further each time (measureStateAnswers).
const stateAnswerParker = 7_000_001

// A state answer holds no more heap than its grant, whether its room parks anything or not
// (weighStateAnswer, answerWeight): every room's answer is cut from its own structs
// (crdt.SplitStateAsUpdateV1), and what a room parks is no part of its state (leaveParkedOut), so
// an answer from a room that parks carries nothing of it and is granted what its cut is. For each
// shape this answers a browser that holds nothing six times on one document, three times parking
// nothing and three times parking a struct, alternating: it parks the struct before each such
// answer and lands it after, so both answer the same structs. Each answer is what ygo's server
// builds between the hook's admission and its release (answerFrames), measured as a copy is
// (measureLoad, copyMeasurementGCPercent): the most heap it held above the document, which is
// what the grant covers, since the frames are that heap's until the release and the browser's
// write queue's after it; what it allocated; and in how many objects. These are the readings
// answerHeapAStateByte and answerHeapAClient are set from.
//
// Every row is a measurement, run with DISPATCH_MEASURE_COPY_HEAP=1 alone: an answer's peak moves
// with where the collector runs, by half again on a busy machine (a mebibyte of prose answered
// from its encoded state, as a room that parked was answered before theorem.6, held 47,280,064
// to 73,004,928 bytes in one run of three), so a later run can pass a grant set past the most an
// earlier one measured. The shapes are a
// mebibyte of prose; one paragraph of a mebibyte typed in one transaction, a few structs; and,
// each of about a million structs, a run of a million keystrokes, each its own item as a room
// takes them one transaction at a time (chainedRun), a run of 1,024,000 characters with every
// other one deleted, a struct for each character (splitDenseRoom), and a million clients of one
// struct each, the shape a sender makes by naming a client id of its own in each group it sends.
// None is at the decode cap itself.
func TestAStateAnswerHoldsNoMoreHeapThanItsGrantParkedOrNot(t *testing.T) {
	skipUnderRace(t)
	if os.Getenv(copyHeapMeasurement) == "" {
		t.Skip("set " + copyHeapMeasurement + "=1 to measure the state answers")
	}
	for _, shape := range []struct {
		name string
		doc  func(t *testing.T) *crdt.Doc
	}{
		{"a mebibyte of prose", func(t *testing.T) *crdt.Doc { return residentRoomDocument(t, 1<<20) }},
		{"one paragraph of a mebibyte typed in one transaction", func(t *testing.T) *crdt.Doc {
			doc := newDocumentCopy()
			if err := crdt.ApplyUpdateV1(doc, longRunState(t, 1), nil); err != nil {
				t.Fatal(err)
			}
			return doc
		}},
		{"a run of a million keystrokes", func(t *testing.T) *crdt.Doc {
			doc := seededDocumentCopy(t)
			chainedRun(t, doc, lastCharacter(t, doc), 1_000, 1_000_000)
			return doc
		}},
		{"a run of 1,024,000 characters with every other one deleted", func(t *testing.T) *crdt.Doc {
			before, cut := splitDenseRoom(t, 1_024_000)
			doc := newDocumentCopy()
			for _, update := range [][]byte{before, cut} {
				if err := crdt.ApplyUpdateV1(doc, update, nil); err != nil {
					t.Fatal(err)
				}
			}
			return doc
		}},
		{"a million clients of one struct each", func(t *testing.T) *crdt.Doc {
			doc := seededDocumentCopy(t)
			const clients, perFrame = 1_000_000, 1 << 16
			for first := 0; first < clients; first += perFrame {
				n := min(perFrame, clients-first)
				frame := newV1Frame(n)
				for c := range n {
					frame = frame.group(1, uint64(gcFillClient+first+c), 0).gc(1)
				}
				if err := crdt.ApplyUpdateV1(doc, frame.noDeletes(), nil); err != nil {
					t.Fatal(err)
				}
			}
			return doc
		}},
	} {
		t.Run(shape.name, func(t *testing.T) {
			measureStateAnswers(t, shape.doc(t))
		})
	}
}

// measureStateAnswers answers doc's state to a browser that holds nothing three times parking
// nothing and three times parking a struct, alternating, with doc leaving what it parks out of
// its state as a room's document does (leaveParkedOut), and fails an answer whose peak passes its
// grant (TestAStateAnswerHoldsNoMoreHeapThanItsGrantParkedOrNot).
func measureStateAnswers(t *testing.T, doc *crdt.Doc) {
	leaveParkedOut(doc)
	if parks(doc) {
		t.Fatal("the document parks something before the measurement parked anything")
	}
	step1 := ygsync.EncodeSyncStep1(crdt.New())
	apply := func(what string, update []byte) {
		if err := crdt.ApplyUpdateV1(doc, update, nil); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	for run := range 3 {
		for _, parked := range []bool{false, true} {
			parker := uint64(stateAnswerParker + run)
			if parked {
				apply("park a struct", parkedGCFrame(parker, 1))
			}
			if parks(doc) != parked {
				t.Fatalf("run %d: the document parks something: %t, want %t", run, parks(doc), parked)
			}
			state := int64(len(crdt.EncodeStateAsUpdateV1(doc, nil)))
			structs := int64(doc.StructCount())
			weight := answerWeight(doc, state, structs)
			cost := func() loadCost {
				defer debug.SetGCPercent(debug.SetGCPercent(copyMeasurementGCPercent))
				return measureLoad(t, func() any { return answerFrames(t, doc, step1) })
			}()
			path := "parking nothing"
			if parked {
				path = "parking a struct"
			}
			t.Logf("run %d, %s: %d structs, %d-byte state: %s, %d peak, %d bytes allocated in %d objects, %d held; %.2f a state byte, %.3f of the %d-byte copy, against a %d-byte grant",
				run, path, structs, state, cost.took.Round(time.Millisecond), cost.peak, cost.allocated, cost.allocations, cost.held,
				float64(cost.peak)/float64(state), float64(cost.peak)/float64(max(state*bytesOfHeapAStateByte, structs*bytesOfHeapAStruct)),
				max(state*bytesOfHeapAStateByte, structs*bytesOfHeapAStruct), weight)
			if int64(cost.peak) > weight {
				t.Errorf("run %d: an answer %s held %d bytes above the document, past its %d-byte grant", run, path, cost.peak, weight)
			}
			if parked {
				apply("land the parked struct", gcFrameFrom(parker, 1))
			}
		}
	}
}

// answerFrames is what ygo's server builds to answer step1 from doc's room between the hook's
// admission and its release: the sync messages sync.EncodeSyncStep2Split cuts doc's state into,
// each wrapped in the frame the browser's writer is handed (theorem-labs/ygo
// v1.55.1-theorem.2, provider/websocket's answerSyncStep1, sendSyncSeries and syncFrame).
func answerFrames(t *testing.T, doc *crdt.Doc, step1 []byte) [][]byte {
	t.Helper()
	messages, err := ygsync.EncodeSyncStep2Split(doc, step1, stateAnswerMessageBytes)
	if err != nil {
		t.Fatalf("answer the browser: %v", err)
	}
	frames := make([][]byte, len(messages))
	for i, message := range messages {
		frames[i] = encoding.EncodeBytes(func(e *encoding.Encoder) {
			e.WriteVarUint(0)
			e.WriteRaw(message)
		})
	}
	return frames
}

// A state answer to a room of small structs holds no more live heap than its grant at the mid
// sizes, where the update its cut is packing is most of what it holds (answerFrameWeight): a run
// of 12,288 GC structs of one client, the smallest struct a browser can send, whose update's
// record of its structs grows from 11,946 to 15,360 as the cut packs it; 1,024 clients of one GC
// struct each beside that run, whose queues the cut holds beside that record; 135 clients of 127
// GC structs each, whose update holds the most structs one can (answerFrameStructs), so that its
// record grows past 15,360; and a run of 4,096 keystrokes, a room an editor makes. Before the
// update was priced the four were granted 198,840, 348,840, 290,488 and 230,520 bytes, and read
// 925,232 to 930,680, 999,160, 1,164,112 and 315,720 to 318,496 of live heap here; they are
// granted 1,087,000, 1,525,272, 1,458,336 and 366,104 now.
//
// It reads the live heap (liveHeapPeak), in a process of its own (measuredAlone) whose collections
// stop the world (GODEBUG gcstoptheworld=1), rather than the peak measureLoad samples, which
// counts garbage a collection at copyMeasurementGCPercent has not reached yet: that peak put the
// keystrokes' answer at 561,560 bytes, where 312,968 to 323,104 of them were live.
func TestAStateAnswerAtMidSizesHoldsNoMoreLiveHeapThanItsGrant(t *testing.T) {
	if !measuredAloneStoppingTheWorld(t) {
		return
	}
	step1 := ygsync.EncodeSyncStep1(crdt.New())
	for _, shape := range []struct {
		name string
		doc  func(t *testing.T) *crdt.Doc
	}{
		{"a run of 12,288 GC structs of one client", func(t *testing.T) *crdt.Doc { return smallStructsDocument(t, 0, 0, 12_288) }},
		{"1,024 clients of one GC struct each beside that run", func(t *testing.T) *crdt.Doc { return smallStructsDocument(t, 1_024, 1, 12_288) }},
		{"135 clients of 127 GC structs each", func(t *testing.T) *crdt.Doc { return smallStructsDocument(t, 135, 127, 0) }},
		{"a run of 4,096 keystrokes", func(t *testing.T) *crdt.Doc {
			doc := seededDocumentCopy(t)
			chainedRun(t, doc, lastCharacter(t, doc), 1_000, 4_096)
			return doc
		}},
	} {
		t.Run(shape.name, func(t *testing.T) {
			doc := shape.doc(t)
			if parks(doc) {
				t.Fatal("the document parks something, so its answer is not cut from its own structs")
			}
			state, structs := int64(len(crdt.EncodeStateAsUpdateV1(doc, nil))), int64(doc.StructCount())
			grant := answerWeight(doc, state, structs)
			reading := liveHeapPeak(t, func() any { return answerFrames(t, doc, step1) })
			t.Logf("%d structs of %d clients, %d-byte state: at most %d bytes of live heap above the document over %d collections in %d answers, against a %d-byte grant",
				structs, doc.ClientCount(), state, reading.peak, reading.collections, reading.runs, grant)
			if reading.peak > grant {
				t.Errorf("the answer held %d bytes of live heap above the document, %d past its %d-byte grant",
					reading.peak, reading.peak-grant, grant)
			}
		})
	}
}

// An answer cut from a room's own structs can allocate several times its grant, and holds no more
// live heap than the grant all the same, because what it allocates past what it holds is scratch
// it drops update by update: each update it packs begins a delete set and a state vector of its
// own, grows an encoder to the update's size and copies the update out of it, and the next update
// begins those afresh (crdt/split.go, newPiece and encodePiece), so what is held at once is one
// update's scratch and the updates written so far, while what is allocated grows with every
// update. Garbage is not the pool's to bound but the Go memory limit's, which the collector
// steers the whole heap to (cmd/dispatch's memlimit.Set, memory_bounds.go). The pool bounds what
// copies hold at once, and this holds the cut's grant to that for the rooms whose answers allocate
// the most past it - each row is checked to allocate more than its grant, and to hold less: a run
// of 50,000 and one of 200,000 GC structs of one client, the smallest struct a browser can send,
// which allocated 5,914,928 and 17,982,416 bytes against grants of 1,412,776 and 3,202,248 and
// held 999,328 to 1,004,664 and 2,112,224 to 2,178,240; 20,000 clients of one GC struct each,
// 11,025,984 to 11,237,656 against 8,212,436, holding 4,476,384 to 4,476,864; and a run of
// 102,400 characters with every other one deleted, 11,575,360 against 8,405,672, holding
// 2,411,016. A grant without the update term (answerFrameWeight) held the first of them short,
// 802,248 against 999,216, and one without the client term (answerHeapAClient) the third,
// 2,402,144 against 4,476,384.
//
// It reads the live heap as TestAStateAnswerAtMidSizesHoldsNoMoreLiveHeapThanItsGrant does
// (liveHeapPeak, a process of its own whose collections stop the world), and the allocation as
// the fewest bytes of five answers (answerAllocation).
func TestAnAnswerAllocatingPastItsGrantHoldsNoMoreLiveHeapThanIt(t *testing.T) {
	if !measuredAloneStoppingTheWorld(t) {
		return
	}
	step1 := ygsync.EncodeSyncStep1(crdt.New())
	for _, shape := range []struct {
		name string
		doc  func(t *testing.T) *crdt.Doc
	}{
		{"a run of 50,000 GC structs of one client", func(t *testing.T) *crdt.Doc { return smallStructsDocument(t, 0, 0, 50_000) }},
		{"a run of 200,000 GC structs of one client", func(t *testing.T) *crdt.Doc { return smallStructsDocument(t, 0, 0, 200_000) }},
		{"20,000 clients of one GC struct each", func(t *testing.T) *crdt.Doc { return smallStructsDocument(t, 20_000, 1, 0) }},
		{"a run of 102,400 characters with every other one deleted", func(t *testing.T) *crdt.Doc {
			// A whole number of the 1,024-character chunks the run is typed in, so the delete
			// names only clocks the room holds and parks nothing (splitDenseRoom).
			before, cut := splitDenseRoom(t, 102_400)
			doc := newDocumentCopy()
			for _, update := range [][]byte{before, cut} {
				if err := crdt.ApplyUpdateV1(doc, update, nil); err != nil {
					t.Fatal(err)
				}
			}
			return doc
		}},
	} {
		t.Run(shape.name, func(t *testing.T) {
			doc := shape.doc(t)
			if parks(doc) {
				t.Fatal("the document parks something, and this measures the answer of one that parks nothing")
			}
			state, structs := int64(len(crdt.EncodeStateAsUpdateV1(doc, nil))), int64(doc.StructCount())
			grant := answerWeight(doc, state, structs)
			allocated := answerAllocation(t, doc, step1)
			reading := liveHeapPeak(t, func() any { return answerFrames(t, doc, step1) })
			t.Logf("%d structs of %d clients, %d-byte state: the answer allocates %d bytes in all, %.2f times its %d-byte grant, and holds at most %d of live heap above the document over %d collections in %d answers, %.2f of the grant",
				structs, doc.ClientCount(), state, allocated, float64(allocated)/float64(grant), grant,
				reading.peak, reading.collections, reading.runs, float64(reading.peak)/float64(grant))
			if allocated <= grant {
				t.Fatalf("the answer allocates %d bytes, within its %d-byte grant: this row no longer shows an answer allocating past its grant", allocated, grant)
			}
			if reading.peak > grant {
				t.Errorf("the answer held %d bytes of live heap above the document, %d past its %d-byte grant",
					reading.peak, reading.peak-grant, grant)
			}
		})
	}
}

// A room of under a thousand structs holds no more live heap than its grant either, and what it
// holds beyond the grant's terms that grow with the room - answerHeapAClient a client and
// answerHeapAStruct a struct of the update being packed - is covered by the term that does not,
// answerHeapAnAnswer's 4,096 bytes. In these rooms that variable part alone is what sets the
// grant beside the fixed term, so what the answer holds past it is the figure the fixed term has
// to cover, and each row asserts that figure is no more than 4,096.
//
// The rooms are the small ones TestAnAnswerAllocatingPastItsGrantHoldsNoMoreLiveHeapThanIt leaves
// out: the seeded paragraph, runs of 64 and 256 keystrokes, runs of 64, 256 and 512 GC structs of
// one client, 100 clients of one GC struct each, and 7 clients of 127 GC structs each, 899
// structs. They are measured as that test measures, in a process of its own whose collections
// stop the world (liveHeapPeak), and at these sizes the measurement's own floor matters: a cut
// that holds nothing and drops 64 KiB of garbage read 4,432 to 18,832 bytes over five readings,
// the most in the first two. So each row is read five times, after one reading the process
// discards, and the fewest of the five is the figure, as answerAllocation takes the fewest
// allocation: what else the process holds live can only add to a reading, and each reading's
// hundreds of collections over thousands of answers find the answer's own peak. Measured
// 2026-11-16 on theorem-labs/ygo v1.55.1-theorem.6, the answers held, past their variable
// terms, from below nothing to 3,360 and 3,504 bytes in two runs (the run of 256 GC structs,
// 27,448 and 27,592 against the 28,184 a struct priced at 88 granted), where single readings of
// that room read up to 31,480 and one of the run of 512 GC structs 328 bytes past its grant: the
// measurement's floor, which the fewest of five sets aside. On arm64 the same room read 30,608
// (CI job 134902505095), past that grant, and the struct's price was raised to cover it
// (answerHeapAStruct), which grants the room 31,642. Each run logs the architecture and the
// processors it measured on, so a reading names the machine it came from.
func TestAnAnswerFromARoomOfUnderAThousandStructsHoldsNoMoreThanItsGrant(t *testing.T) {
	if !measuredAloneStoppingTheWorld(t) {
		return
	}
	t.Logf("measured on %s/%s, %d processors of the machine's %d (liveHeapProcessors)",
		runtime.GOOS, runtime.GOARCH, liveHeapProcessors, runtime.NumCPU())
	step1 := ygsync.EncodeSyncStep1(crdt.New())
	keystrokes := func(count int) func(t *testing.T) *crdt.Doc {
		return func(t *testing.T) *crdt.Doc {
			doc := seededDocumentCopy(t)
			chainedRun(t, doc, lastCharacter(t, doc), 1_000, count)
			return doc
		}
	}
	warmed := false
	for _, shape := range []struct {
		name string
		doc  func(t *testing.T) *crdt.Doc
	}{
		{"the seeded paragraph", seededDocumentCopy},
		{"a run of 64 keystrokes", keystrokes(64)},
		{"a run of 256 keystrokes", keystrokes(256)},
		{"a run of 64 GC structs of one client", func(t *testing.T) *crdt.Doc { return smallStructsDocument(t, 0, 0, 64) }},
		{"a run of 256 GC structs of one client", func(t *testing.T) *crdt.Doc { return smallStructsDocument(t, 0, 0, 256) }},
		{"a run of 512 GC structs of one client", func(t *testing.T) *crdt.Doc { return smallStructsDocument(t, 0, 0, 512) }},
		{"100 clients of one GC struct each", func(t *testing.T) *crdt.Doc { return smallStructsDocument(t, 100, 1, 0) }},
		{"7 clients of 127 GC structs each", func(t *testing.T) *crdt.Doc { return smallStructsDocument(t, 7, 127, 0) }},
	} {
		t.Run(shape.name, func(t *testing.T) {
			doc := shape.doc(t)
			if parks(doc) {
				t.Fatal("the document parks something, and this measures the answer of one that parks nothing")
			}
			state, structs, clients := int64(len(crdt.EncodeStateAsUpdateV1(doc, nil))), int64(doc.StructCount()), int64(doc.ClientCount())
			if structs >= 1_000 {
				t.Fatalf("the room holds %d structs, not under a thousand", structs)
			}
			grant := answerWeight(doc, state, structs)
			variable := clients*answerHeapAClient + min(structs, answerFrameStructs)*answerHeapAStruct
			if grant != variable+answerHeapAnAnswer {
				t.Fatalf("the room is granted %d, not its variable terms' %d and the fixed %d: its state term sets the grant, so the fixed term is not what this row weighs",
					grant, variable, answerHeapAnAnswer)
			}
			cut := func() any { return answerFrames(t, doc, step1) }
			if !warmed {
				liveHeapPeak(t, cut)
				warmed = true
			}
			fewest, most := int64(math.MaxInt64), int64(0)
			for range 5 {
				peak := liveHeapPeak(t, cut).peak
				fewest, most = min(fewest, peak), max(most, peak)
			}
			beyond := fewest - variable
			t.Logf("%d structs of %d clients, %d-byte state: held %d to %d bytes of live heap above the document over five readings, against a %d-byte grant; %d past its %d-byte variable terms, against the fixed %d",
				structs, clients, state, fewest, most, grant, beyond, variable, answerHeapAnAnswer)
			if beyond > answerHeapAnAnswer {
				t.Errorf("the answer held %d bytes of live heap, %d past its variable terms, more than the fixed %d-byte term covers: %d past its %d-byte grant",
					fewest, beyond, answerHeapAnAnswer, fewest-grant, grant)
			}
		})
	}
}

// An answer to a small room is granted near what it holds, and not the update a cut can pack at
// its fullest: what that update holds is bounded by the room's own structs (answerFrameWeight),
// so a room of ten structs is charged for ten. The guard is what the answer allocates in all,
// which bounds from above whatever it holds at once: the grant is held to twice that, which
// leaves the price its rounding past the most measured and fails any price that charges a small
// room the fullest update's 1,620,096 bytes, or a flat 950,000 for the update whatever the room,
// where the seeded paragraph's answer allocates 3,840 in all. The rooms are the seeded
// paragraph, ten structs; a run of 64 keystrokes; a run of 256 GC structs, the heaviest struct
// for its bytes; and a run of 1,000 keystrokes. It runs in a process of its own (measuredAlone),
// where nothing else allocates while the answer is counted.
func TestAnAnswerFromASmallRoomIsGrantedNoMoreThanTwiceWhatItAllocates(t *testing.T) {
	skipUnderRace(t)
	if !measuredAlone(t) {
		return
	}
	step1 := ygsync.EncodeSyncStep1(crdt.New())
	keystrokes := func(count int) func(t *testing.T) *crdt.Doc {
		return func(t *testing.T) *crdt.Doc {
			doc := seededDocumentCopy(t)
			chainedRun(t, doc, lastCharacter(t, doc), 1_000, count)
			return doc
		}
	}
	for _, shape := range []struct {
		name string
		doc  func(t *testing.T) *crdt.Doc
	}{
		{"the seeded paragraph", seededDocumentCopy},
		{"a run of 64 keystrokes", keystrokes(64)},
		{"a run of 256 GC structs of one client", func(t *testing.T) *crdt.Doc { return smallStructsDocument(t, 0, 0, 256) }},
		{"a run of 1,000 keystrokes", keystrokes(1_000)},
	} {
		t.Run(shape.name, func(t *testing.T) {
			doc := shape.doc(t)
			if parks(doc) {
				t.Fatal("the document parks something, so its answer is not cut from its own structs")
			}
			state, structs := int64(len(crdt.EncodeStateAsUpdateV1(doc, nil))), int64(doc.StructCount())
			grant := answerWeight(doc, state, structs)
			allocated := answerAllocation(t, doc, step1)
			t.Logf("%d structs of %d clients, %d-byte state: the answer allocates %d bytes in all, against a %d-byte grant (%.2f of it)",
				structs, doc.ClientCount(), state, allocated, grant, float64(grant)/float64(allocated))
			if grant > 2*allocated {
				t.Errorf("the answer to a room of %d structs is granted %d bytes, more than twice the %d it allocates in all",
					structs, grant, allocated)
			}
		})
	}
}

// answerAllocation is the fewest bytes an answer to step1 from doc allocated in all over five
// answers (runtime.MemStats.TotalAlloc), after one that warms what it reuses: what else the
// process allocates meanwhile can only add to a reading, so the fewest is the nearest.
func answerAllocation(t *testing.T, doc *crdt.Doc, step1 []byte) int64 {
	t.Helper()
	runtime.KeepAlive(answerFrames(t, doc, step1))
	fewest := int64(math.MaxInt64)
	for range 5 {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		frames := answerFrames(t, doc, step1)
		runtime.ReadMemStats(&after)
		runtime.KeepAlive(frames)
		fewest = min(fewest, int64(after.TotalAlloc-before.TotalAlloc))
	}
	return fewest
}

// smallStructsDocument is the seeded document holding clients clients of each GC structs each
// and, beside them, a run of run GC structs of one client: the smallest structs a browser can
// send, two bytes each of the room's state, and client ids a browser names of its own, as any
// browser can.
func smallStructsDocument(t *testing.T, clients, each, run int) *crdt.Doc {
	t.Helper()
	doc := seededDocumentCopy(t)
	groups := clients
	if run > 0 {
		groups++
	}
	frame := newV1Frame(groups)
	if run > 0 {
		frame = frame.group(run, gcFillClient, 0)
		for range run {
			frame = frame.gc(1)
		}
	}
	for client := range clients {
		frame = frame.group(each, uint64(gcFillClient+1+client), 0)
		for range each {
			frame = frame.gc(1)
		}
	}
	if err := crdt.ApplyUpdateV1(doc, frame.noDeletes(), nil); err != nil {
		t.Fatalf("apply the small structs: %v", err)
	}
	return doc
}

// liveHeapReading is what liveHeapPeak read: the most live heap a collection marked above what
// was live before the cut first ran, how many collections ended while it ran, and how many times
// it ran.
type liveHeapReading struct {
	peak        int64
	collections uint64
	runs        int
}

// measuredAloneStoppingTheWorld is measuredAlone for a reading of the live heap (liveHeapPeak):
// the process it measures in has every collection stop the world (GODEBUG gcstoptheworld=1,
// beside what the test inherits), and a race build, whose instrumentation is no heap a room
// holds, measures nothing.
func measuredAloneStoppingTheWorld(t *testing.T) bool {
	t.Helper()
	skipUnderRace(t)
	godebug := "gcstoptheworld=1"
	if inherited := os.Getenv("GODEBUG"); inherited != "" {
		godebug = inherited + "," + godebug
	}
	t.Setenv("GODEBUG", godebug)
	return measuredAlone(t)
}

// liveHeapPeak runs cut again and again, for a second and at least five times, with the collector
// at a GOGC of 1, and reads the live heap each collection marks as it ends (/gc/heap/live:bytes).
// At that rate a few collections end within each run of the cut, at points that move from run to
// run, so over a second they fall across the whole cut and the most any of them marks is its peak
// to within what it allocates between two of them: runs of 12,288 to 49,152 GC structs of one
// client read 915,128 to 949,160 bytes this way, where a collection at every growth of the cut's
// arrays and encoders, on a copy of the fork instrumented for it, read 914,840 for each. It is
// live heap only where each collection stops the world, as in a process started with GODEBUG
// gcstoptheworld=1: a concurrent collection marks as live whatever is allocated while it marks,
// which at this rate is most of what the cut allocates. The goroutine reading each mark as it
// ends needs a processor of its own beside the cut's, and the measurement runs on exactly
// liveHeapProcessors of them, whatever the machine has, set here and given back on return.
func liveHeapPeak(t *testing.T, cut func() any) liveHeapReading {
	t.Helper()
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(liveHeapProcessors))
	defer debug.SetGCPercent(debug.SetGCPercent(1))
	marks := func() []metrics.Sample {
		return []metrics.Sample{{Name: "/gc/heap/live:bytes"}, {Name: "/gc/cycles/total:gc-cycles"}}
	}
	before := marks()
	runtime.GC()
	runtime.GC()
	metrics.Read(before)
	base, last := int64(before[0].Value.Uint64()), before[1].Value.Uint64()
	stop, read := make(chan struct{}), make(chan liveHeapReading, 1)
	stopReading := sync.OnceFunc(func() { close(stop) })
	defer stopReading()
	go func() {
		var reading liveHeapReading
		marked := marks()
		for {
			select {
			case <-stop:
				read <- reading
				return
			default:
			}
			metrics.Read(marked)
			if cycles := marked[1].Value.Uint64(); cycles != last {
				reading.collections += cycles - last
				last = cycles
				reading.peak = max(reading.peak, int64(marked[0].Value.Uint64())-base)
			}
		}
	}()
	var kept any
	runs := 0
	for deadline := time.Now().Add(time.Second); runs < 5 || time.Now().Before(deadline); runs++ {
		kept = nil
		kept = cut()
	}
	stopReading()
	reading := <-read
	runtime.KeepAlive(kept)
	reading.runs = runs
	if reading.collections == 0 {
		t.Fatal("no collection ended while the cut ran, so nothing read its live heap")
	}
	return reading
}

// liveHeapProcessors is how many processors liveHeapPeak measures on: the cut's and the reader's,
// the count the prices it checks were measured at. It is fixed because every processor keeps
// caches of its own that the cut fills and that outlive a collection - ygo's encoder pool, which
// the cut takes each encoder from (crdt.SplitStateAsUpdateV1, sync.EncodeSyncStep2Split), is a
// sync.Pool with a slot a processor - so a reading grew with the machine: on this repository's
// amd64 pod on 2026-11-18 the run of 256 GC structs of one client read 27,448 bytes at one and two
// processors, 27,816 at four and 28,456 at eight, where the test failed
// (TestAnAnswerFromARoomOfUnderAThousandStructsHoldsNoMoreThanItsGrant). What those caches hold
// is the process's, bounded by its processors and shared by every answer, not one answer's.
const liveHeapProcessors = 2

// useLoadSlots makes slots the process's load concurrency for the rest of t (podLoadSlots).
func useLoadSlots(t *testing.T, slots *loadSlots) *loadSlots {
	t.Helper()
	previous := podLoadSlots.Swap(slots)
	t.Cleanup(func() { podLoadSlots.Store(previous) })
	return slots
}

// loadSlotsWithRoomFor makes the process a load concurrency of slots slots and holds all of its
// pool but free bytes of it for the rest of t, so that what a test's own takers queue behind is
// the bytes it leaves them rather than the slots. The pool is sized in slots of loadSlotBytes
// (newLoadSlots) and a taker takes only what its own copy costs (copyWeight), so a test whose
// documents are far lighter than a slot says how much room it leaves their copies instead of
// how many slots there are. It answers the pool and the bytes it holds, which every reading of
// loadSlots.held stands above.
func loadSlotsWithRoomFor(t *testing.T, slots int, free int64) (*loadSlots, int64) {
	t.Helper()
	pool := useLoadSlots(t, newLoadSlots(slots))
	held := pool.poolBytes() - free
	if held <= 0 {
		t.Fatalf("a pool of %d slots has no %d bytes to hold back", slots, free)
	}
	holder := newLoadSlot()
	if !holder.tryTake(held) {
		t.Fatalf("the empty pool refused %d of its %d bytes", held, pool.poolBytes())
	}
	t.Cleanup(holder.release)
	return pool, held
}

// proseState is the state of a fresh document of at least bytes of prose (proseAtLeast).
func proseState(t *testing.T, bytes int) []byte {
	t.Helper()
	tree, err := pmdoc.Parse(proseAtLeast(bytes))
	if err != nil {
		t.Fatal(err)
	}
	state, err := encodeDocumentTree(tree)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

// loadRoomOf loads artifactID's room as the server's own work does, a Server.Apply that changes
// nothing.
func loadRoomOf(service *Service, artifactID string) error {
	err := service.srv.Apply(context.Background(), artifactID, func(*crdt.Doc, func(func(*crdt.Transaction))) {})
	if errors.Is(err, websocket.ErrNoChanges) {
		return nil
	}
	return err
}

// slotsHeld records how many bytes of the pool were held as each grant was taken (loadSlots.taken).
type slotsHeld struct {
	mu   sync.Mutex
	held []int64
}

func (g *slotsHeld) record(held int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.held = append(g.held, held)
}

func (g *slotsHeld) all() []int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.held)
}

// A room's load folds a history of several updates on the grant it took for the fold, and decodes
// and hands ygo the state on that same grant: with the rest of the pool taken the moment the load
// holds its own, before its fold decodes anything, the load finishes - no part of it takes a
// second grant (withLoadSlot) - and gives its grant back.
func TestALoadWhoseFoldRunsWhileTheSlotsAreFullFoldsOnTheLoadsOwnSlot(t *testing.T) {
	ctx := context.Background()
	service, artifactID := newTestService(t)
	storeHistory(t, service, artifactID, proseState(t, 64<<10))
	if _, err := service.persistence.AppendUpdate(ctx, artifactID, emptyUpdate); err != nil {
		t.Fatal(err)
	}
	if stored := storedUpdateCount(t, service.store, artifactID); stored != 2 {
		t.Fatalf("the history holds %d updates, want the state and the update after it", stored)
	}
	slots := useLoadSlots(t, newLoadSlots(2))
	var taken slotsHeld
	var rest atomic.Int64
	var filled bool
	slots.taken = func(held int64) {
		taken.record(held)
		if !filled {
			filled = true
			rest.Store(slots.poolBytes() - held)
			if !slots.semaphore().TryAcquire(rest.Load()) {
				t.Errorf("the pool had no %d bytes left beside the load's %d held", rest.Load(), held)
			}
		}
	}
	loaded := make(chan error, 1)
	go func() { loaded <- loadRoomOf(service, artifactID) }()
	select {
	case err := <-loaded:
		if err != nil {
			t.Fatalf("the load failed: %v", err)
		}
	case <-time.After(30 * time.Second):
		slots.semaphore().Release(rest.Load())
		t.Fatal("the load did not finish in 30 s while the rest of the pool was held: it waits for a second grant inside its own")
	}
	defer slots.semaphore().Release(rest.Load())
	if got := taken.all(); len(got) != 1 {
		t.Fatalf("the load took grants leaving %v held, want one grant of its own for its fold and its decode together", got)
	}
	if roomDocument(t, service, artifactID) == nil {
		t.Fatal("the room did not load")
	}
	if held := slots.held.Load(); held != 0 {
		t.Fatalf("the load still holds %d bytes once loaded", held)
	}
}

// A load that finds the pool full waits for room rather than decoding beside its holders, and is
// never refused, since a stored document must always load: it waits without a deadline
// (holdServerLoad), takes the bytes the holder gives back, and gives them back itself once the
// room is loaded.
func TestALoadWaitsForASlotWhileEveryOneIsHeldAndThenTakesIt(t *testing.T) {
	service, artifactID := newTestService(t)
	storeHistory(t, service, artifactID, proseState(t, 16<<10))
	slots := useLoadSlots(t, newLoadSlots(1))
	holder := newLoadSlot()
	if !holder.tryTake(loadSlotBytes) {
		t.Fatal("the empty pool refused a whole slot")
	}
	waiting := make(chan struct{})
	var waitOnce sync.Once
	slots.waiting = func() { waitOnce.Do(func() { close(waiting) }) }
	var taken slotsHeld
	slots.taken = taken.record
	loaded := make(chan error, 1)
	go func() { loaded <- loadRoomOf(service, artifactID) }()
	select {
	case <-waiting:
	case err := <-loaded:
		t.Fatalf("the load finished (%v) while the only slot was held", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the load neither waited for a slot nor finished in 30 s")
	}
	select {
	case err := <-loaded:
		t.Fatalf("the load finished (%v) while the only slot was held", err)
	case <-time.After(200 * time.Millisecond):
	}
	holder.release()
	select {
	case err := <-loaded:
		if err != nil {
			t.Fatalf("the load failed: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the load did not finish in 30 s once the slot was free")
	}
	if got := taken.all(); len(got) == 0 {
		t.Fatal("the load took no slot")
	}
	if held := slots.held.Load(); held != 0 {
		t.Fatalf("the load still holds %d bytes once loaded", held)
	}
}

// A room ygo loaded without reading the store through this service's persistence - ygo decoded
// what another persistence handed it into the room's document itself (theorem-labs/ygo
// v1.55.1-theorem.4, provider/websocket/server.go:2052-2071), so the load hook finds no record
// of a load of its own (takeLoadedState) - is decoded again by the hook, from an encode of its
// own document, into the copy its replica takes, under a grant of the pod's load concurrency of
// its own (onLoadDocument, holdServerLoad). That encode carries what the room parks, so the copy
// parks it too, and the grant is weighed on the structs the room holds as an apply reports them,
// parked structs included (heldStructs, copyWeight). A reading of the structs in the room's store
// alone (crdt.Doc.StructCount) granted a room whose pending queue is heavy a copy short by
// everything in that queue: the class of review 5856254176's second High (dispatch://INFRA-179).
//
// The room holds a small document of prose and one browser's frame of 2,000 GC structs parked
// behind a gap in its client's clocks (parkedGCFrame), whose structs outweigh their bytes in the
// pool, so the two readings grant weights apart. The hook is called as ygo's loadRoom calls it,
// on the document as that load leaves it, and the grant is read off the pool as it is taken
// (loadSlots.taken), as every other load's grant is read here.
func TestARoomLoadedWithoutTheServicesPersistenceIsGrantedACopyThatCountsWhatItParks(t *testing.T) {
	const parker, parkedStructs = 8_100_001, 2_000
	service, artifactID := newTestService(t)
	service.settle = time.Hour // no settlement takes a grant beside the load's
	// The room's document as ygo's loadRoom hands it to the hook: made with the pending cap
	// lifted, the state its persistence read decoded into it - here with a frame parked behind
	// a gap among it - and the server's cap set once that state is in (theorem-labs/ygo
	// v1.55.1-theorem.4, provider/websocket/server.go:1885, 1951-1954, 2065-2066).
	doc := crdt.New(crdt.WithMaxPendingItems(math.MaxInt))
	for _, update := range [][]byte{seededState(t), parkedGCFrame(parker, parkedStructs)} {
		if err := crdt.ApplyUpdateV1(doc, update, nil); err != nil {
			t.Fatalf("decode the room's state: %v", err)
		}
	}
	doc.SetMaxPendingItems(service.srv.MaxPendingItems)
	if _, recorded := service.roomLoads.Load(artifactID); recorded {
		t.Fatal("the room has a load of this service's persistence on record, so the hook would decode the state that load read rather than its own document's")
	}
	stored, parked := int64(doc.StructCount()), int64(doc.PendingStats().Items)
	if parked != parkedStructs {
		t.Fatalf("the room parks %d structs, want the frame's %d", parked, parkedStructs)
	}
	state := int64(len(crdt.EncodeStateAsUpdateV1(doc, nil)))
	want, short := copyWeight(state, stored+parked), copyWeight(state, stored)
	if short >= want {
		t.Fatalf("a copy of %d bytes of state weighs %d at the %d structs of the room's store and %d with the %d it parks as well, so the fixture does not tell the two readings apart",
			state, short, stored, want, parked)
	}

	slots := useLoadSlots(t, newLoadSlots(1))
	var taken slotsHeld
	slots.taken = taken.record
	if err := service.srv.OnLoadDocument(context.Background(), artifactID, doc); err != nil {
		t.Fatalf("the load hook failed: %v", err)
	}
	switch grants := taken.all(); {
	case len(grants) != 1:
		t.Fatalf("the load took grants leaving %v held, want the one its copy's decode takes", grants)
	case grants[0] == short:
		t.Fatalf("the load's copy was granted %d bytes, what its %d bytes of state and the %d structs of the room's store alone come to, want the %d they come to with the %d structs it parks: the copy parks them too",
			grants[0], state, stored, want, parked)
	case grants[0] != want:
		t.Fatalf("the load's copy was granted %d bytes, want the %d its %d bytes of state and its %d structs with the %d it parks come to",
			grants[0], want, state, stored+parked, parked)
	}
	if held := slots.held.Load(); held != 0 {
		t.Fatalf("the load still holds %d bytes once loaded", held)
	}
	// The copy the grant covered is the room's replica, and it parks what the room parks: the
	// structs the grant is weighed on are the ones that copy holds.
	replica := service.replicaOf(doc)
	if replica == nil || replica.doc == nil {
		t.Fatal("the load kept no replica holding the copy it decoded")
	}
	if copyParks := int64(replica.doc.PendingStats().Items); copyParks != parked {
		t.Fatalf("the copy the load decoded parks %d structs, want the room's %d", copyParks, parked)
	}
	t.Logf("a room of %d structs in its store and %d parked, %d bytes of state, was granted %d bytes for its copy, where its state and its store's structs alone come to %d",
		stored, parked, state, want, short)
}

// Loads of six documents at once, against a pool left with room for two of their copies, never
// hold more of it than it has, and every one of them loads: the third waits for one of the two
// to give its bytes back. Each grant is held a tenth of a second past its decode, so the loads
// overlap. What bounds them here is the bytes the test leaves free and not the slots: each of
// these documents is 16 KiB, whose copy costs far less than a slot (copyWeight).
func TestConcurrentLoadsNeverHoldMoreOfThePoolThanItHas(t *testing.T) {
	const together = 2
	service, first := newTestService(t)
	state := proseState(t, 16<<10)
	rooms := append([]string{first}, createDocuments(t, service.store, 5)...)
	for _, room := range rooms {
		storeHistory(t, service, room, state)
	}
	// Each room's whole stored history is the one row storeHistory wrote, so each load takes
	// exactly what that state's copy costs (loadDecodedHistory), which nothing has decoded when
	// the grant is taken, so its state term alone.
	each := copyWeight(int64(len(state)), 0)
	slots, test := loadSlotsWithRoomFor(t, 2, together*each)
	var most atomic.Int64
	slots.taken = func(held int64) {
		for seen := most.Load(); held > seen && !most.CompareAndSwap(seen, held); seen = most.Load() {
		}
		time.Sleep(100 * time.Millisecond)
	}
	errs := make(chan error, len(rooms))
	for _, room := range rooms {
		go func() { errs <- loadRoomOf(service, room) }()
	}
	for range rooms {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatalf("a load failed: %v", err)
			}
		case <-time.After(60 * time.Second):
			t.Fatal("the loads did not all finish in 60 s")
		}
	}
	if got := most.Load(); got > slots.poolBytes() {
		t.Fatalf("the loads held %d bytes at most, past the %d the pool has", got, slots.poolBytes())
	}
	if got := most.Load() - test; got < together*each {
		t.Fatalf("the loads held %d bytes of the pool at most beside the test's %d, want at least the %d two copies take: they loaded one at a time", got, test, together*each)
	}
	for _, room := range rooms {
		if roomDocument(t, service, room) == nil {
			t.Fatalf("room %s did not load", room)
		}
	}
	if held := slots.held.Load(); held != test {
		t.Fatalf("the loads hold %d bytes once loaded, want the %d the test holds and none of their own", held, test)
	}
}

// A socket's admission waits for its slot within its request: one whose request ends while its
// fold waits for a slot answers the request's error, fails no room, and holds neither a slot nor
// a connection of the store, so the next admission reads and decodes the history once a slot is
// free. The concurrency here holds one slot, held by the test.
func TestAnAdmissionWhoseRequestEndsWhileItWaitsForASlotAnswersItsErrorAndHoldsNothing(t *testing.T) {
	ctx := context.Background()
	service, artifactID := newTestService(t)
	storeHistory(t, service, artifactID, proseState(t, 16<<10))
	if _, err := service.persistence.AppendUpdate(ctx, artifactID, emptyUpdate); err != nil {
		t.Fatal(err)
	}
	slots := useLoadSlots(t, newLoadSlots(1))
	holder := newLoadSlot()
	if !holder.tryTake(loadSlotBytes) {
		t.Fatal("the empty pool refused a whole slot")
	}
	waiting := make(chan struct{})
	var waitOnce sync.Once
	slots.waiting = func() { waitOnce.Do(func() { close(waiting) }) }
	request, cancel := context.WithCancel(ctx)
	type admission struct {
		tree   *pmdoc.Node
		loaded *persistence.LoadResult
		err    error
	}
	admitted := make(chan admission, 1)
	go func() {
		tree, loaded, _, err := service.loadTree(request, artifactID)
		admitted <- admission{tree, loaded, err}
	}()
	select {
	case <-waiting:
	case <-time.After(30 * time.Second):
		t.Fatal("the admission did not wait for a slot in 30 s")
	}
	cancel()
	var ended admission
	select {
	case ended = <-admitted:
	case <-time.After(30 * time.Second):
		t.Fatal("the admission did not answer in 30 s once its request ended")
	}
	if !errors.Is(ended.err, context.Canceled) || ended.loaded != nil || ended.tree != nil {
		t.Fatalf("the admission answered %v with state %v and tree %v, want the request's context.Canceled and neither", ended.err, ended.loaded, ended.tree)
	}
	if err := service.roomFailure(artifactID); err != nil {
		t.Fatalf("the ended request failed the room: %v", err)
	}
	if held := slots.held.Load(); held != loadSlotBytes {
		t.Fatalf("%d bytes are held, want the slot the test holds and nothing of the ended admission's", held)
	}
	holder.release()
	tree, loaded, _, err := service.loadTree(ctx, artifactID)
	if err != nil || loaded == nil || tree == nil {
		t.Fatalf("the next admission answered %v with state %v and tree %v, want the history read and decoded", err, loaded, tree)
	}
	service.offerPreload(artifactID, loaded, admissionRendering{tree: tree})()
	if held := slots.held.Load(); held != 0 {
		t.Fatalf("%d bytes are still held once both admissions have answered", held)
	}
}

// A rebuild's check of a resident room decodes the room's whole state before the rebuild decides
// anything (nearACap), so that decode takes bytes of the pod's load concurrency like every other
// full-state decode: with the pool full, the rebuild waits rather than decoding beside the
// holder; once those bytes are given back it runs and answers that the document is live and far
// from both caps (ErrDocumentLive), holding nothing of the pool after it.
func TestARebuildOfAResidentDocumentTakesASlotBeforeItWeighsIt(t *testing.T) {
	ctx := context.Background()
	service, artifactID := newTestService(t)
	// No settlement of the room takes a grant of its own while the rebuild waits.
	service.settle = time.Hour
	storeHistory(t, service, artifactID, proseState(t, 16<<10))
	if err := loadRoomOf(service, artifactID); err != nil {
		t.Fatalf("load the room: %v", err)
	}
	slots := useLoadSlots(t, newLoadSlots(1))
	holder := newLoadSlot()
	if !holder.tryTake(loadSlotBytes) {
		t.Fatal("the empty pool refused a whole slot")
	}
	waiting := make(chan struct{})
	var waitOnce sync.Once
	slots.waiting = func() { waitOnce.Do(func() { close(waiting) }) }
	var taken slotsHeld
	slots.taken = taken.record
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin rebuild: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	answered := make(chan error, 1)
	go func() {
		_, _, err := service.RebuildDocument(joined, artifactID, nil, model.Actor{Kind: "user", ID: "alice"})
		answered <- err
	}()
	select {
	case <-waiting:
	case err := <-answered:
		t.Fatalf("the rebuild answered %v while the only slot was held: its check decoded the resident room's state outside the concurrency", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the rebuild's check neither waited for a slot nor answered in 30 s")
	}
	select {
	case err := <-answered:
		t.Fatalf("the rebuild answered %v while the only slot was held", err)
	case <-time.After(200 * time.Millisecond):
	}
	holder.release()
	select {
	case err := <-answered:
		if !errors.Is(err, ErrDocumentLive) {
			t.Fatalf("the rebuild of a resident document far from both caps answered %v, want ErrDocumentLive", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the rebuild did not answer in 30 s once the slot was free")
	}
	if got := taken.all(); len(got) == 0 {
		t.Fatal("the rebuild's check took no slot")
	}
	if held := slots.held.Load(); held != 0 {
		t.Fatalf("the refused rebuild still holds %d bytes", held)
	}
}

// agentRewrittenCacheEnv names a file the heavy measurements keep the agent-rewritten state in
// (agentRewrittenState), which takes about 200 s to build: read when the file exists, written
// once built when it does not.
const agentRewrittenCacheEnv = "DISPATCH_AGENT_REWRITTEN_CACHE"

// cachedAgentRewrittenState is agentRewrittenState, kept in the file agentRewrittenCacheEnv names.
func cachedAgentRewrittenState(t *testing.T) []byte {
	t.Helper()
	path := os.Getenv(agentRewrittenCacheEnv)
	if path != "" {
		if state, err := os.ReadFile(path); err == nil {
			return state
		}
	}
	state, refusal := agentRewrittenState(t)
	if !errors.Is(refusal, ErrDocumentTooLarge) {
		t.Fatalf("the agent's last write was refused with %v, want ErrDocumentTooLarge", refusal)
	}
	if path != "" {
		if err := os.WriteFile(path, state, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return state
}

// loadCost is what one run of a load cost at GOMAXPROCS=1: how long it took, what it allocated
// (runtime.MemStats.TotalAlloc across it) in how many objects (runtime.MemStats.Mallocs), the
// most heap its objects held above what the heap held before it, sampled every millisecond the
// scheduler lets the sampler run (/memory/classes/heap/objects:bytes), and what it still holds
// once collected.
type loadCost struct {
	took        time.Duration
	allocated   uint64
	allocations uint64
	peak        uint64
	held        int64
}

// measureLoad runs load once at GOMAXPROCS=1 and returns its cost (loadCost).
func measureLoad(t *testing.T, load func() any) loadCost {
	t.Helper()
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	heapObjects := func() uint64 {
		metrics.Read(sample)
		return sample[0].Value.Uint64()
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&before)
	base := heapObjects()
	// The sampler is stopped whichever way this returns, since load fails the test (t.Fatal) from
	// this goroutine: one left running would read the heap every millisecond for the rest of the
	// run, under every measurement after it, and its reading, which no one would take, is left
	// in peaked rather than blocking it there forever.
	stop, peaked := make(chan struct{}), make(chan uint64, 1)
	stopSampling := sync.OnceFunc(func() { close(stop) })
	defer stopSampling()
	go func() {
		peak := base
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				peaked <- max(peak, heapObjects())
				return
			case <-ticker.C:
				peak = max(peak, heapObjects())
			}
		}
	}()
	started := time.Now()
	kept := load()
	took := time.Since(started)
	stopSampling()
	peak := <-peaked
	runtime.ReadMemStats(&after)
	allocated, allocations := after.TotalAlloc-before.TotalAlloc, after.Mallocs-before.Mallocs
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(kept)
	return loadCost{took: took, allocated: allocated, allocations: allocations, peak: peak - base,
		held: int64(after.HeapAlloc) - int64(before.HeapAlloc)}
}

// A holder that already holds a grant keeps it and takes only the difference where its next copy
// weighs more, so the pool holds the heaviest copy the grant covers and never the sum of the
// copies made under it, and a copy lighter than the grant takes nothing at all
// (dispatch://INFRA-179, review 5820480859's finding 10). One transaction's forks share the
// first fork's grant (forkSlot) and every decode made under a carried one covers itself on it
// (withLoadSlot), so a take that found the grant held and took nothing more charged a
// transaction its first copy however heavy the copies after it: one transaction that edited a
// resident 256-byte document and then a resident 2 MiB one was measured at "small fork weighs
// 32676, big fork weighs 32148200" with the pool holding 32,676 bytes, which
// Docs.WriteIssueAskStates does on every issue close, reopen and sweep.
//
// The two copies are weighed as every taker of the pool is (copyWeight), and what the pool holds
// is read off its own accounting (loadSlots.held).
func TestAHoldersHeavierCopyTakesTheDifferenceAndALighterOneTakesNothing(t *testing.T) {
	// The measurement's two documents: the small one the transaction forked first, and the
	// 2 MiB one it forked after it. Neither has been decoded where its grant is taken, so the
	// state term alone weighs each (copyWeight).
	small, large := copyWeight(256, 0), copyWeight(2<<20, 0)
	if small >= large {
		t.Fatalf("the two copies weigh %d and %d bytes of the pool, want the second the heavier", small, large)
	}
	slots := useLoadSlots(t, newLoadSlots(1))
	grant := newLoadSlot()
	t.Cleanup(grant.release)
	for _, take := range []struct {
		name string
		// bytes is what the copy this take covers costs, and held what the pool holds once it
		// has been taken.
		bytes, held int64
	}{
		{name: "the transaction's first fork", bytes: small, held: small},
		{name: "its second fork, which weighs more", bytes: large, held: large},
		{name: "a third fork lighter than the grant covers", bytes: small, held: large},
	} {
		if err := grant.take(context.Background(), take.bytes); err != nil {
			t.Fatalf("%s took %d bytes of the pool: %v", take.name, take.bytes, err)
		}
		if held := slots.held.Load(); held != take.held {
			t.Fatalf("the pool holds %d bytes once the grant covers %s, a copy of %d bytes, want %d; the two copies together are %d",
				held, take.name, take.bytes, take.held, small+large)
		}
	}
	t.Logf("a grant taken for a %d-byte copy and then for a %d-byte one holds %d bytes of the pool, not %d",
		small, large, slots.held.Load(), small+large)
	// The difference is given back with the rest: the grant gives back exactly what it took.
	grant.release()
	if held := slots.held.Load(); held != 0 {
		t.Fatalf("the released grant leaves %d bytes of the pool held, want none", held)
	}
	if !slots.semaphore().TryAcquire(slots.poolBytes()) {
		t.Fatalf("the pool has less than its whole %d bytes free once the grant was released", slots.poolBytes())
	}
	slots.semaphore().Release(slots.poolBytes())
}
