package docs

import (
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testwait"
)

// alice's tab typed after an edit of bob's that the room relayed and then lost (here: one the
// room has not received yet), so her frame parks and is answered not applied, and her tab keeps
// the row. Her socket ends, she types once more offline, and the tab closes. Bob's page sends his
// edit again; it lands, and its drain lands alice's parked frame, which the room stores. Alice's
// socket ended before the landing, so the room kept her person and her id (parkedWaits.end), and
// the store of the landing marks her row landed (markEndedLanded): her later page loads do not
// give the row back, and her next page's restore of the two saved rows under that id lands.
// Before, markParkedLanded marked nothing and her row stayed unlanded while the stored history
// named her id, her later page loads gave the row back, and the restore was refused
// DOC_CLIENT_ID_REFUSED - on which the dashboard deletes every saved row under the id
// (dropClientRows), her offline edit with them.
func TestPCAClosedTabsParkedFrameLandedAfterItClosedLeavesItsRowUnlandedAndItsReplayRefused(t *testing.T) {
	presenceIntervalForTest.Store(int64(300 * time.Millisecond))
	t.Cleanup(func() { presenceIntervalForTest.Store(0) })
	service, artifactID, serverURL := clientIDRoom(t)
	room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }

	bob := openPage(t, serverURL, artifactID, "bob")
	awaitAnswer(t, bob, "bob's page")
	alice := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, alice, "alice's page")
	a := alice.Given

	// bob's edit reaches alice's page but not the room.
	bobEdit := typeAt(t, bob.Doc, 0, "BOB ")
	if err := crdt.ApplyUpdateV1(alice.Doc, bobEdit, nil); err != nil {
		t.Fatal(err)
	}
	hearFromNow(t, alice, "alice's page")
	parkedRow := typeAt(t, alice.Doc, 4, "ALICE ")
	if err := alice.Write(ygsync.EncodeUpdate(parkedRow)); err != nil {
		t.Fatal(err)
	}
	heard := awaitHeardStatus(t, alice, 1, "alice's page")
	t.Logf("alice's frame typed after bob's unsent edit answered %q; the room parks %+v", heard[0], room().PendingStats())
	alice.Close()
	<-alice.Ended
	offlineRow := typeAt(t, alice.Doc, 10, "OFFLINE ")

	// bob's page sends his edit; its drain lands alice's parked frame.
	hearFromNow(t, bob, "bob's page")
	if err := bob.Write(ygsync.EncodeUpdate(bobEdit)); err != nil {
		t.Fatal(err)
	}
	awaitHeardStatus(t, bob, 1, "bob's page")
	waitForRoomRendering(t, room, func(r string) bool { return strings.Contains(r, "ALICE") }, "alice's parked frame landed by bob's edit")
	time.Sleep(time.Second) // the room stores the landing commit; the row lapses (3 x 300 ms)
	landed, stored := clientIDLanded(t, service, artifactID, a)
	t.Logf("after the landing was stored: alice's row for %d stored=%t landed=%t", a, stored, landed)

	opens, gone := fillCapUntilGoneWithin(t, service, artifactID, serverURL, a, 2*maxUnlandedMintsADocument)
	t.Logf("alice's row for %d, an id the stored history names, was given back: %t, after %d page loads", a, gone, opens)
	time.Sleep(1100 * time.Millisecond)

	next := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, next, "alice's next page")
	for _, row := range [][]byte{parkedRow, offlineRow} {
		if err := crdt.ApplyUpdateV1(next.Doc, row, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range [][]byte{parkedRow, offlineRow} {
		if err := next.Write(ygsync.EncodeUpdate(row)); err != nil {
			t.Fatal(err)
		}
	}
	// The page's read loop takes the two frames in order, so the room renders OFFLINE only once
	// neither was refused, and a refusal closes the page before that.
	testwait.Holds(30*time.Second, testwait.Poll, func() bool {
		select {
		case <-next.Ended:
			return true
		default:
		}
		rendering, _ := renderDocument(room())
		return strings.Contains(rendering, "OFFLINE")
	})
	select {
	case <-next.Ended:
		closed := next.Closed()
		rendering, _ := renderDocument(room())
		if closed != nil && closed.Code == contracts.DocumentTooLargeCloseCode && closed.Text == clientIDRefusedReason {
			t.Fatalf("alice's next page replaying her closed tab's two saved rows under %d was refused %d %s; the dashboard deletes both, and the offline one was never stored (the room renders %q)",
				a, closed.Code, closed.Text, rendering)
		}
		t.Fatalf("alice's next page was closed %v", closed)
	default:
	}
	waitForRoomRendering(t, room, func(r string) bool { return strings.Contains(r, "OFFLINE") }, "alice's offline row")
}
