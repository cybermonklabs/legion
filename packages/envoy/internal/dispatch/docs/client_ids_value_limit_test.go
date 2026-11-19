package docs

import (
	"errors"
	"testing"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/contracts"
)

// The comparison of a claimed id's structs with what the room holds at the same clocks decodes the
// whole update into a document of its own (refusedDuplicate), which no apply's budget reaches, so it
// decodes under the apply's own value limit and refuses an update past it as the apply refuses one,
// DOC_TOO_LARGE, stopping before it builds the value past it (crdt.HeldStructsDifferV1Limit,
// crdt.ErrValueLimit). One struct the room holds, under the claimed id, beside
// maxDecodedValuesPerApply+1 one-clock delete ranges, is such an update: unbounded, the comparison
// decoded every range, found nothing that differs and passed it.
func TestAClaimedIDsComparisonDecodesNoMoreThanTheApplysValueLimit(t *testing.T) {
	const claimed = 1_234_570_001
	room := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(room, newV1Frame(1).group(1, claimed, 0).gc(1).noDeletes(), nil); err != nil {
		t.Fatalf("seed the room with the claimed id's struct: %v", err)
	}
	f := newV1Frame(1).group(1, claimed, 0).gc(1)
	f.uint(1).uint(7_200_001).uint(maxDecodedValuesPerApply + 1)
	for i := range maxDecodedValuesPerApply + 1 {
		f.uint(uint64(2 * i)).uint(1)
	}
	update := f.done()
	info := websocket.PeerUpdateInfo{Room: "room", Doc: room, Update: update}
	err := calibratedService().refusedDuplicate(info, nil, clientOwner{claimed: true}, claimed, crdt.StateVector{claimed: 0})
	var closed *websocket.PeerClose
	if !errors.As(err, &closed) || closed.Code != contracts.DocumentTooLargeCloseCode || closed.Reason != contracts.DocumentTooLargeCloseReason {
		t.Fatalf("the comparison of a %d-byte update of %d values answered %v, want the apply's own refusal past its %d values, %d %s",
			len(update), maxDecodedValuesPerApply+2, err, maxDecodedValuesPerApply, contracts.DocumentTooLargeCloseCode, contracts.DocumentTooLargeCloseReason)
	}
	if !errors.Is(err, crdt.ErrValueLimit) {
		t.Fatalf("the comparison refused the update with %v, want it stopped at its value limit", err)
	}
}
