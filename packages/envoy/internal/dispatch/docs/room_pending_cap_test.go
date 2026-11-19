package docs

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A room's document decodes its stored state with the pending caps lifted, and ygo sets the room's
// caps - maxUpdateItems for its parked structs, and maxParkedDeleteRanges for its parked delete
// ranges apart (Server.MaxPendingDeleteRanges) - once the state is in and before OnLoadDocument
// (sjawhar/ygo v1.55.1-sami.1, provider/websocket/server.go:1830, :1897-1899; loadRoom in
// theorem-labs/ygo v1.55.1-theorem.10), so every peer's apply after the load is held to them. A
// room loads a stored state that parks structs and delete ranges; a browser then sends, over the
// document socket, an update parking delete ranges up to the range cap exactly, which the room
// applies, and one parking a range more, which the room refuses, leaving its parked queue as it
// was and the browser connected. The two reach the room's own apply of a peer update with the
// hook's weighing passed over for them alone: they park under clients no socket holds, which the
// hook's judge refuses before the cap is reached (parkJudge), so only ygo's apply can show the cap
// itself. TestABrowsersDeleteRangesParkUpToTheRoomsCapAndOnePastItIsAnsweredNotApplied drives the
// same cap through the hook, under the browser's own client id.
//
// The frames were sized to maxUpdateItems while that cap held parked delete ranges too, shared
// with the structs; round 9 gave the ranges a cap of their own, so they are sized to that.
//
// A cold read decodes the stored state, an earlier build's, that parks structs and delete ranges.
// The copy a write's fork is made from (snapshotDocument) holds the room's document and nothing
// of its pending queue, however full: what the room parks is no part of the document until it
// lands (leaveParkedOut), so the room's encode leaves it out. The fold of a stored log that parks
// both is TestALoadAndACompactionKeepWhatTheDocumentParked.
func TestAPeerApplyIsHeldToTheRoomsPendingCapAfterTheLoad(t *testing.T) {
	const (
		parkedStructs = 1_000
		parkedRanges  = 1_000
		// The clients of the parked structs and of the three sets of ranges: none of them is a
		// client of the document's seed, so the structs wait behind client 8's clock 0 and every
		// range waits for structs the room never receives.
		structClient, storedRangeClient, capRangeClient, pastRangeClient = 8, 11, 12, 13
	)
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	ctx := context.Background()

	tree, err := pmdoc.Parse("# First\n\nOne paragraph of ordinary prose.\n")
	if err != nil {
		t.Fatal(err)
	}
	seed, err := encodeDocumentTree(tree)
	if err != nil {
		t.Fatal(err)
	}
	history := newDocumentCopy()
	parkedFrame := newV1Frame(1).group(parkedStructs, structClient, 1)
	for range parkedStructs {
		parkedFrame.gc(1)
	}
	for _, update := range [][]byte{seed, parkedFrame.noDeletes(), deleteRangesFrom(storedRangeClient, parkedRanges)} {
		if err := crdt.ApplyUpdateV1(history, update, nil); err != nil {
			t.Fatal(err)
		}
	}
	if stats := history.PendingStats(); stats.Items != parkedStructs || stats.DeleteRanges != parkedRanges {
		t.Fatalf("the stored state parks %d structs and %d delete ranges, want %d and %d", stats.Items, stats.DeleteRanges, parkedStructs, parkedRanges)
	}
	storeHistory(t, service, artifactID, crdt.EncodeStateAsUpdateV1(history, nil))
	if text, err := service.Text(ctx, artifactID); err != nil || !strings.Contains(text, "First") {
		t.Fatalf("a cold read of the stored state that parks structs and delete ranges reads %q (%v), want the document", text, err)
	}

	atTheCap := deleteRangesFrom(capRangeClient, maxParkedDeleteRanges-parkedRanges)
	pastTheCap := deleteRangesFrom(pastRangeClient, 1)
	applies := make(chan error, 2)
	weigh := service.srv.OnPeerUpdate
	service.srv.OnPeerUpdate = func(info websocket.PeerUpdateInfo, apply func() error) error {
		if !bytes.Equal(info.Update, atTheCap) && !bytes.Equal(info.Update, pastTheCap) {
			return weigh(info, apply)
		}
		err := apply()
		applies <- err
		return err
	}
	server := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(server.Close)
	browser := connectPeer(t, server.URL, artifactID, "alice")
	awaitAnswer(t, browser, "the document loaded from the stored state")
	room := roomDocument(t, service, artifactID)
	if stats := room.PendingStats(); stats.Items != parkedStructs || stats.DeleteRanges != parkedRanges {
		t.Fatalf("the loaded room parks %d structs and %d delete ranges, want the stored state's %d and %d", stats.Items, stats.DeleteRanges, parkedStructs, parkedRanges)
	}

	send := func(t *testing.T, what string, update []byte) error {
		t.Helper()
		if err := browser.Write(ygsync.EncodeUpdate(update)); err != nil {
			t.Fatalf("send %s: %v", what, err)
		}
		select {
		case err := <-applies:
			return err
		case <-browser.Ended:
			t.Fatalf("the room ended the browser's connection before applying %s: %v", what, browser.Closed())
		case <-time.After(time.Minute):
			t.Fatalf("the room never applied %s", what)
		}
		return nil
	}
	if err := send(t, "the update parking delete ranges up to the cap", atTheCap); err != nil {
		t.Fatalf("the room refused an update parking %d delete ranges, up to its %d cap: %v", maxParkedDeleteRanges-parkedRanges, maxParkedDeleteRanges, err)
	}
	if stats := room.PendingStats(); stats.DeleteRanges != maxParkedDeleteRanges || stats.Items != parkedStructs {
		t.Fatalf("the room parks %d delete ranges and %d structs, want the %d cap and the %d it loaded", stats.DeleteRanges, stats.Items, maxParkedDeleteRanges, parkedStructs)
	}
	if err := send(t, "the update parking one delete range past the cap", pastTheCap); !errors.Is(err, crdt.ErrPendingCap) {
		t.Fatalf("the room answered %v to an update parking one delete range past its %d cap, want ygo's pending cap refusal", err, maxParkedDeleteRanges)
	}
	if stats := room.PendingStats(); stats.DeleteRanges != maxParkedDeleteRanges || stats.Items != parkedStructs {
		t.Fatalf("the refused update left the room parking %d delete ranges and %d structs, want the %d and %d before it", stats.DeleteRanges, stats.Items, maxParkedDeleteRanges, parkedStructs)
	}
	select {
	case <-browser.Ended:
		t.Fatalf("the room closed the browser whose update its pending cap refused: %v", browser.Closed())
	case <-time.After(200 * time.Millisecond):
	}

	snapshot, err := snapshotDocument(room)
	if err != nil {
		t.Fatalf("copy the room holding %d parked delete ranges: %v", maxParkedDeleteRanges, err)
	}
	if stats := snapshot.PendingStats(); stats.DeleteRanges != 0 || stats.Items != 0 {
		t.Fatalf("the copy parks %d delete ranges and %d structs, want none of what the room parks", stats.DeleteRanges, stats.Items)
	}
	if got, want := snapshot.StateVector(), room.StateVector(); !maps.Equal(got, want) {
		t.Fatalf("the copy holds the state vector %v, want the room's %v", got, want)
	}
}

// A browser's delete range past what the room holds parks only under a client id its socket holds
// (parkJudge), and a room parks at most maxParkedDeleteRanges of them
// (Server.MaxPendingDeleteRanges). Through the document socket and the peer update hook, alice's
// page sends a frame of delete ranges under its own given id, of which the room holds no clock, up
// to the cap: the room parks every one and answers the frame not applied, as it answers every frame
// that leaves something of its own parked (AnswerParkedNotApplied). The page then sends one range
// more under the same id, apart from the others: ygo refuses that apply before it changes anything
// (crdt.ErrPendingCap), and the hook answers it as it answers the structs' pending cap, not
// applied with the socket left open (refusedByApply), so the room still parks the cap and alice's
// page is still connected. Under the structs' cap, maxUpdateItems, which held the ranges too
// before round 9, the range past this cap parked as well.
func TestABrowsersDeleteRangesParkUpToTheRoomsCapAndOnePastItIsAnsweredNotApplied(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	alice := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, alice, "the document")
	room := roomDocument(t, service, artifactID)
	own := uint64(alice.Given)
	if clock := room.StateVector().Clock(alice.Given); clock != 0 {
		t.Fatalf("the room holds alice's given id to clock %d before she sends anything, want 0", clock)
	}
	alice.Hear()
	send := func(what string, frame []byte, answers int) string {
		t.Helper()
		if err := alice.Write(ygsync.EncodeUpdate(frame)); err != nil {
			t.Fatalf("send %s: %v", what, err)
		}
		heard := awaitHeardStatus(t, alice, answers, "alice after "+what)
		return heard[answers-1]
	}

	atTheCap := deleteRangesFrom(own, maxParkedDeleteRanges)
	if heard := send("a frame of delete ranges up to the cap", atTheCap, 1); heard != "SyncStatus 0 (applied: false)" {
		t.Fatalf("alice's frame of %d delete ranges under her own id was answered %q, want not applied: it parks", maxParkedDeleteRanges, heard)
	}
	if stats := room.PendingStats(); stats.DeleteRanges != maxParkedDeleteRanges {
		t.Fatalf("the room parks %d delete ranges after alice's frame, want the %d it carried under her own id", stats.DeleteRanges, maxParkedDeleteRanges)
	}
	// One range two clocks past the frame's last, so it merges with none of them.
	pastTheCap := newV1Frame(0).uint(1).uint(own).uint(1).uint(2 * maxParkedDeleteRanges).uint(1).done()
	if heard := send("one delete range past the cap", pastTheCap, 2); heard != "SyncStatus 0 (applied: false)" {
		t.Fatalf("alice's delete range past the room's %d was answered %q, want not applied", maxParkedDeleteRanges, heard)
	}
	if stats := room.PendingStats(); stats.DeleteRanges != maxParkedDeleteRanges {
		t.Fatalf("the room parks %d delete ranges after alice's range past its cap, want the %d cap: the range parked", stats.DeleteRanges, maxParkedDeleteRanges)
	}
	requireConnected(t, alice, "alice")
}
