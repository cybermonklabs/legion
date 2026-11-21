package docs

import (
	"testing"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
)

// typedRuns types each of runs at the end of doc's first paragraph, one transaction each, and
// answers each transaction's update: structs of doc's client alone, each run's clocks following
// the run's before it, and each run's first character typed after the last of the run before.
func typedRuns(t *testing.T, doc *crdt.Doc, runs ...string) [][]byte {
	t.Helper()
	updates := make([][]byte, 0, len(runs))
	for _, run := range runs {
		update := docstest.Transact(doc, func(txn *crdt.Transaction) {
			text := paragraphTextsOf(txn.GetXmlFragment(fragmentName))[0]
			text.Insert(txn, text.Len(), run, nil)
		})
		if update == nil {
			t.Fatalf("typing %q made no update", run)
		}
		updates = append(updates, update)
	}
	return updates
}

// A server write's commit carries more than its writer stored wherever it holds a clock the
// writer's update does not, and a struct the commit's drain landed inside a gap of that update's
// own clocks for a client is such a clock (heldAgainst). The rows stored here are the first and
// the last of three runs one browser typed, merged: their client's clocks hold a Skip where the
// middle run's are. The room parked the middle run, which waits behind the first; the rows' apply
// supplies the first, and its drain lands the middle run beside them, so the commit holds the
// client's clocks unbroken from the first run's start to the last run's end. Its lowest and
// highest clock are the rows' own, which is all a comparison of the two updates' headers
// (crdt.ParseUpdateMeta) saw, so that comparison took the commit as stored and the middle run
// was in no row. The same rows committed into a room that parked nothing land only what they
// hold, the first run, and carry nothing beyond them.
func TestACommitThatLandsAStructInsideAGapOfItsWritersClocksCarriesBeyondThem(t *testing.T) {
	browser := seededDocumentCopy(t)
	seed := crdt.EncodeStateAsUpdateV1(browser, nil)
	runs := typedRuns(t, browser, "first ", "middle ", "last ")
	written, err := crdt.MergeUpdatesV1(runs[0], runs[2])
	if err != nil {
		t.Fatalf("merge the first run and the last: %v", err)
	}
	// commit is the update the room commits as it applies written, after parked, which may be
	// nil, has parked in it.
	commit := func(t *testing.T, parked []byte) []byte {
		t.Helper()
		room := newDocumentCopy()
		if err := crdt.ApplyUpdateV1(room, seed, nil); err != nil {
			t.Fatalf("seed the room: %v", err)
		}
		if parked != nil {
			if err := crdt.ApplyUpdateV1(room, parked, "browser"); err != nil {
				t.Fatalf("park the middle run: %v", err)
			}
			if stats := room.PendingStats(); stats.Items == 0 {
				t.Fatalf("the room parks %+v, want the middle run behind the first", stats)
			}
		}
		origin := "the stored rows"
		var committed []byte
		unsubscribe := room.OnUpdate(func(update []byte, updateOrigin any) {
			if updateOrigin == origin {
				committed = append([]byte(nil), update...)
			}
		})
		if err := crdt.ApplyUpdateV1(room, written, origin); err != nil {
			t.Fatalf("apply the stored rows: %v", err)
		}
		unsubscribe()
		if committed == nil {
			t.Fatal("the stored rows' apply committed nothing")
		}
		return committed
	}

	landed := commit(t, runs[1])
	client := browser.ClientID()
	from, to, err := crdt.ParseUpdateMeta(landed)
	if err != nil {
		t.Fatalf("read the commit's header: %v", err)
	}
	writtenFrom, writtenTo, err := crdt.ParseUpdateMeta(written)
	if err != nil {
		t.Fatalf("read the rows' header: %v", err)
	}
	if from[client] != writtenFrom[client] || to[client] != writtenTo[client] {
		t.Fatalf("the commit holds the browser's clocks %d to %d and the rows %d to %d; nothing here is tested",
			from[client], to[client], writtenFrom[client], writtenTo[client])
	}
	if beyond, _ := heldAgainst(landed, written); !beyond {
		t.Fatalf("a commit whose drain landed the middle run, clocks %d to %d of a client the rows hold %d to %d of with a gap there, was taken as carrying nothing beyond them",
			writtenFrom[client]+uint64(len("first ")), writtenFrom[client]+uint64(len("first middle ")), writtenFrom[client], writtenTo[client])
	}
	if beyond, _ := heldAgainst(commit(t, nil), written); beyond {
		t.Fatal("a commit that landed nothing was taken as carrying more than the rows it applied")
	}
}
