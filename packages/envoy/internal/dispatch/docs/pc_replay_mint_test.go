package docs

import (
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	ygsync "github.com/reearth/ygo/sync"
)

// An id a replaying socket's update minted (mintForSender) is held by that socket (confirmed)
// and presented every interval while the socket is open (keepPresenting,
// socketClients.presenting). Before, keepPresenting presented clients.own alone, so while the
// socket was open its row lapsed, a later mint of the same person gave it back, another person's
// update under the id was then minted for that person, and the first socket - which no longer
// asks the table about an id it holds - wrote under the same id. Its frame at clocks the second
// person took was answered applied while the room kept the second person's text.
func TestPCAReplayMintedIDOfAnOpenSocketIsGivenBackAndMintedToAnotherPerson(t *testing.T) {
	presenceIntervalForTest.Store(int64(300 * time.Millisecond))
	t.Cleanup(func() { presenceIntervalForTest.Store(0) })
	service, artifactID, serverURL := clientIDRoom(t)
	room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }

	first := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, first, "alice's first tab")
	a := first.Given
	first.Close()
	<-first.Ended
	saved1 := typeAt(t, first.Doc, 0, "OFF")
	saved2 := typeAt(t, first.Doc, 3, "LINE")
	time.Sleep(time.Second)
	opens := fillCapUntilGone(t, service, artifactID, serverURL, a)
	t.Logf("step 1: alice's closed tab's row for %d given back after %d page loads", a, opens)
	time.Sleep(1100 * time.Millisecond)

	// mallory fills the room's parked cap with structs under her own id behind a gap.
	mallory := openPage(t, serverURL, artifactID, "mallory")
	awaitAnswer(t, mallory, "mallory's page")
	hearFromNow(t, mallory, "mallory's page")
	if err := mallory.Write(ygsync.EncodeUpdate(parkedGCFrame(uint64(mallory.Given), maxParkedStructs))); err != nil {
		t.Fatal(err)
	}
	heard := awaitHeardStatus(t, mallory, 1, "mallory")
	t.Logf("step 2: mallory's %d parked structs answered %q; the room parks %+v", maxParkedStructs, heard[0], room().PendingStats())

	// alice's second tab replays the closed tab's second row under A: minted for her, refused at
	// the cap (it would park behind the first row), and left connected, holding A.
	second := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, second, "alice's second tab")
	for _, row := range [][]byte{saved1, saved2} {
		if err := crdt.ApplyUpdateV1(second.Doc, row, nil); err != nil {
			t.Fatal(err)
		}
	}
	hearFromNow(t, second, "alice's second tab")
	if err := second.Write(ygsync.EncodeUpdate(saved2)); err != nil {
		t.Fatal(err)
	}
	heard = awaitHeardStatus(t, second, 1, "alice's second tab")
	requireConnected(t, second, "alice's second tab")
	t.Logf("step 3: alice's replay under %d answered %q; row owner %q; the room holds %d of it",
		a, heard[0], clientIDOwner(t, service, artifactID, a), room().StateVector()[a])

	// Alice's next page loads give the row back only if the open second tab stops presenting it.
	time.Sleep(time.Second)
	opens, gone := fillCapUntilGoneWithin(t, service, artifactID, serverURL, a, 2*maxUnlandedMintsADocument)
	requireConnected(t, second, "alice's second tab")
	if !gone {
		t.Logf("step 4: the row for %d, held by alice's open second tab, still names %q after %d more page loads", a, clientIDOwner(t, service, artifactID, a), opens)
		return
	}
	t.Logf("step 4: the row for %d, held by alice's open second tab, was given back after %d more page loads", a, opens)

	// bob writes under A at clock 0: no row names it, the room and the history hold nothing
	// under it, so it is minted for bob.
	bobDoc := crdt.New(crdt.WithClientID(a))
	if err := crdt.ApplyUpdateV1(bobDoc, crdt.EncodeStateAsUpdateV1(room(), nil), nil); err != nil {
		t.Fatal(err)
	}
	bobUpdate := typeAt(t, bobDoc, 0, "BOB")
	bob := openPage(t, serverURL, artifactID, "bob")
	awaitAnswer(t, bob, "bob's page")
	hearFromNow(t, bob, "bob's page")
	if err := bob.Write(ygsync.EncodeUpdate(bobUpdate)); err != nil {
		t.Fatal(err)
	}
	heard = awaitHeardStatus(t, bob, 1, "bob")
	requireConnected(t, bob, "bob")
	waitForRoomRendering(t, room, func(r string) bool { return strings.Contains(r, "BOB") }, "bob's text under alice's old id")
	t.Logf("step 5: bob's update under %d answered %q; row owner now %q", a, heard[0], clientIDOwner(t, service, artifactID, a))

	// alice's open second tab sends the closed tab's first row under A, at the clocks bob took.
	hearFromNow(t, second, "alice's second tab")
	if err := second.Write(ygsync.EncodeUpdate(saved1)); err != nil {
		t.Fatal(err)
	}
	heard = awaitHeardStatus(t, second, 1, "alice's second tab")
	time.Sleep(500 * time.Millisecond)
	rendering, _ := renderDocument(room())
	t.Logf("step 6: alice's row under %d answered %q, socket %s; the room renders %q", a, heard[0], connectionEnd(second), rendering)
	if strings.HasPrefix(heard[0], "SyncStatus 1") && !strings.Contains(rendering, "OFF") {
		t.Fatalf("alice's edit under %d, an id whose row now names bob, was answered applied while the room keeps bob's text at those clocks and none of hers: two people's sockets write under one id, and alice's edit is dropped in silence", a)
	}
}
