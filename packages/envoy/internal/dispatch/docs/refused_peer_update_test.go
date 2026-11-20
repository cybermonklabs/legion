package docs

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A peer's update the room refuses leaves the room's document as every other peer of the room holds
// it. The room refuses an update whose delete set would park more delete ranges than its pending
// queue takes (maxParkedDeleteRanges, crdt.ErrPendingCap); a refusal that comes after the update has
// already deleted text the room holds leaves the room with a deletion it told the sender failed and
// sent no other peer, so the document the room stores and settles is no longer the one every open
// editor shows. Here one peer's update deletes text the room holds and names one delete range more
// than the room's pending queue takes, each past the clocks the room holds of the peer's own client;
// an ordinary keystroke the same connection sends next, which needs nothing of the refused update,
// then reaches the other peer, and the room and that peer must read the same document.
//
// A delete range parks only under a client the sender's socket holds (parkJudge), and one under any
// other is refused first, with the sender closed 4413 DOC_CLIENT_ID_REFUSED (refusedByApply), so the
// ranges are under the id the peer has typed under. The peer presents no client id, as a page of the
// earlier build does, and its socket holds its document's id once a keystroke of its own has written
// a struct under it (judgeClients' claim). Round 9 gave the parked ranges a cap of their own, apart
// from the structs' cap that shrinkPendingItems lowers, so the update carries one range more than
// that cap.
func TestARefusedPeerUpdateLeavesTheRoomAsItsOtherPeersHoldIt(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "hello world")
	server := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(server.Close)
	alpha := connectPeer(t, server.URL, artifactID, "alice")
	beta := connectPeer(t, server.URL, artifactID, "alice")
	waitFor(t, 10*time.Second, "both peers to hold the room's document", func() bool {
		return strings.TrimSpace(peerMarkdown(alpha)) == "hello world" && strings.TrimSpace(peerMarkdown(beta)) == "hello world"
	})
	typeOver(t, alpha, "hello world", "hello world!")
	waitFor(t, 10*time.Second, "the other peer to receive the first peer's keystroke", func() bool {
		return strings.TrimSpace(peerMarkdown(beta)) == "hello world!"
	})

	// The keystroke sent after the refused update is made on a copy of the room's document as the
	// peers hold it now, so the room can integrate it whatever became of the refused one.
	unrelated := crdt.New()
	if err := crdt.ApplyUpdateV1(unrelated, crdt.EncodeStateAsUpdateV1(alpha.Doc, nil), nil); err != nil {
		t.Fatalf("copy the peer's document: %v", err)
	}
	after := docstest.Transact(unrelated, typeParagraph(unrelated, "after"))

	fragment := alpha.Doc.GetXmlFragment(fragmentName)
	var editErr error
	deletion := docstest.Transact(alpha.Doc, func(txn *crdt.Transaction) {
		tree, err := pmdoc.ReadInTransaction(txn, fragment)
		if err != nil {
			editErr = err
			return
		}
		if len(tree.Children) != 1 || len(tree.Children[0].Children) != 1 {
			editErr = errors.New("the document is not one paragraph of one text run")
			return
		}
		tree.Children[0].Children[0].Text = "hello"
		editErr = pmdoc.Update(txn, fragment, tree)
	})
	if editErr != nil || deletion == nil {
		t.Fatalf("delete the text: update=%d bytes err=%v", len(deletion), editErr)
	}
	// The peer's document holds every clock of its own client the room can hold, so ranges from its
	// clock on delete nothing the room holds, and each would park.
	own := alpha.Doc.ClientID()
	refused, err := crdt.MergeUpdatesV1(deletion, deleteRangesPast(own, alpha.Doc.StateVector().Clock(own), maxParkedDeleteRanges+1))
	if err != nil {
		t.Fatalf("merge the deletion with the delete ranges: %v", err)
	}

	if err := alpha.Write(ygsync.EncodeUpdate(refused)); err != nil {
		t.Fatalf("send the update the room refuses: %v", err)
	}
	if err := alpha.Write(ygsync.EncodeUpdate(after)); err != nil {
		t.Fatalf("send the keystroke after it: %v", err)
	}
	waitFor(t, 10*time.Second, "the other peer to receive the keystroke sent after the refused update", func() bool {
		return strings.HasPrefix(peerMarkdown(beta), "after")
	})
	// Nothing of the refused update parked. A frame within the cap parks its ranges and lands its
	// deletion beside them, and the room and the other peer agree with nothing refused: the 65
	// ranges this test sent while the structs' cap held the ranges too did that under the ranges'
	// own cap.
	if stats := roomDocument(t, service, artifactID).PendingStats(); stats.DeleteRanges != 0 {
		t.Fatalf("the room parks %d delete ranges after the update past its cap of %d, want none: nothing refused the update",
			stats.DeleteRanges, maxParkedDeleteRanges)
	}

	room := currentLiveMarkdown(t, service, artifactID)
	if other := peerMarkdown(beta); room != other {
		t.Fatalf("the room reads %q but its other peer reads %q: the room kept part of an update it sent no other peer", room, other)
	}
}

// deleteRangesPast is an update holding only a delete set: ranges non-adjacent delete ranges of one
// clock each of client's, the first at clock from.
func deleteRangesPast(client crdt.ClientID, from uint64, ranges int) []byte {
	f := newV1Frame(0).uint(1).uint(uint64(client)).uint(uint64(ranges))
	for index := range ranges {
		f.uint(from + uint64(2*index)).uint(1)
	}
	return f.done()
}
