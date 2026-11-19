package docs

import (
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/cputest"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// BenchmarkATangledRoomTakesAKeystroke measures what one keystroke costs the room of the tangled
// browser row (packages/dispatch/e2e/document-refusals.e2e.ts, "a merge the server cannot scan
// through is dropped with its reason, and the next edit saves"), whose history the tangle hook
// gave a chain of tangledChain items, each of a client of its own (TangleBeyondOneApplyForTest,
// writeChain): the browser's keystroke into a paragraph after the chain, as the row types it, handed
// to the peer update hook and applied by the room's own apply, with the room's update observer
// draining the commit into the replica inside that apply, as observeRoomUpdates does.
//
// ygo walks every client of a document several times in each apply whatever the apply holds: a
// keystroke here makes eight StateVector calls over the room's 40,002 clients, four in the room's
// apply and four in the replica's (its state before and after, the apply's own, the drain's),
// counted from an allocation profile. Before theorem-labs/ygo v1.55.1-theorem.9 every apply check
// a document carried added two walks more, which is why the replica's document carries none while
// it holds nothing parked (holdParkedIn). Measured on theorem.8 on a shared two-CPU pod, five runs
// a side of 30 keystrokes each, alternating between 2a57f092, where the replica always carried
// holdParked, and the change that dropped it there: a median of 166 ms a keystroke (151 ms of
// CPU) before and 150 ms (145 ms of CPU) after.
func BenchmarkATangledRoomTakesAKeystroke(b *testing.B) {
	tree, err := pmdoc.Parse("# Tangled\n\nThe tangle follows the next paragraph.\n")
	if err != nil {
		b.Fatal(err)
	}
	state, err := encodeDocumentTree(tree)
	if err != nil {
		b.Fatal(err)
	}
	live := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(live, state, nil); err != nil {
		b.Fatal(err)
	}
	// The chain onto the end of the document's last text, as the hook appends it.
	text := lastWrittenText(live.GetXmlFragment(fragmentName))
	chain := encoding.NewEncoder()
	chain.WriteVarUint(tangledChain)
	writeChain(chain, *crdt.CreateRelativePositionFromIndex(text, text.Len()-1, 0).Item, tangledChain, tangledChainClient)
	chain.WriteVarUint(0)
	if err := crdt.ApplyUpdateV1(live, chain.Bytes(), nil); err != nil {
		b.Fatal(err)
	}
	// Enter at the document's end and the first character after it, from a browser's copy, as
	// typeAtEnd types: each later keystroke names the character before it as its left origin.
	browser := newDocumentCopy(crdt.WithClientID(tangledTypist))
	if err := crdt.ApplyUpdateV1(browser, crdt.EncodeStateAsUpdateV1(live, nil), nil); err != nil {
		b.Fatal(err)
	}
	fragment := browser.GetXmlFragment(fragmentName)
	var typed *crdt.YXmlText
	enter := docstest.Transact(browser, func(txn *crdt.Transaction) {
		paragraph := crdt.NewYXmlElement("paragraph")
		fragment.InsertElement(txn, len(fragment.Children()), paragraph)
		typed = crdt.NewYXmlText()
		paragraph.InsertText(txn, 0, typed)
		typed.Insert(txn, 0, "s", nil)
	})
	last := *crdt.CreateRelativePositionFromIndex(typed, 0, 0).Item
	if next := browser.StateVector().Clock(tangledTypist); last.Client != tangledTypist || last.Clock+1 != next {
		b.Fatalf("the typed character is %+v and the browser's next clock %d, want the character last", last, next)
	}
	if err := crdt.ApplyUpdateV1(live, enter, nil); err != nil {
		b.Fatal(err)
	}
	service := calibratedService()
	service.budget.limit = roomBudgetBytes
	replica := observedReplica(b, service, live, liveRendering(b, live))
	service.chargeLoadedRoom("bench", replica, int64(len(crdt.EncodeStateAsUpdateV1(live, nil))))
	live.OnUpdate(func([]byte, any) { replica.observe("bench") })
	origin := new(int)
	// The CPU the process spends on each keystroke, beside the wall time b.Loop reports: on a
	// shared machine the wall time of a run this heavy moves with its neighbours' load.
	var keystrokes int64
	var cpuFrom time.Duration
	for b.Loop() {
		if keystrokes == 0 {
			cpuFrom = cputest.Process()
		}
		keystrokes++
		keystroke := encoding.NewEncoder()
		keystroke.WriteVarUint(1) // one client group
		keystroke.WriteVarUint(1) // one struct in it
		keystroke.WriteVarUint(uint64(tangledTypist))
		keystroke.WriteVarUint(last.Clock + 1)
		writeOriginString(keystroke, last, "k")
		keystroke.WriteVarUint(0) // no delete set
		update := keystroke.Bytes()
		info := websocket.PeerUpdateInfo{Room: "bench", Doc: live, Update: update, Origin: origin}
		if err := service.weighPeerUpdate(info, func() error {
			_, err := crdt.ApplyUpdateV1Outcome(live, update, origin)
			return err
		}); err != nil {
			b.Fatalf("the room refused a keystroke: %v", err)
		}
		last = crdt.ID{Client: tangledTypist, Clock: last.Clock + 1}
	}
	if held := live.StateVector().Clock(tangledTypist); held != last.Clock+1 {
		b.Fatalf("the room holds the typist to clock %d, want %d", held, last.Clock+1)
	}
	b.ReportMetric(float64(cputest.Process()-cpuFrom)/float64(keystrokes)/float64(time.Millisecond), "cpu-ms/op")
	b.ReportMetric(float64(len(live.StateVector())), "clients")
}
