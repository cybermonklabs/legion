package docs

import (
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// These tests hold what one document socket holds (socketClients): the id its page was given and
// every id its updates showed is its person's, each kept while the socket is open, so a delete
// range parks under any of them (parkJudge), up to maxConfirmedClients besides its own.

// landedRowsOf stores n client ids from mintedClientFloor up as login's in artifactID, each a row
// an edit landed under, as the ids of the person's other pages are once each has typed, and
// returns them.
func landedRowsOf(t *testing.T, service *Service, artifactID, login string, n int) []crdt.ClientID {
	t.Helper()
	ids := make([]crdt.ClientID, n)
	rows := make([]int64, n)
	for i := range ids {
		ids[i] = crdt.ClientID(mintedClientFloor + 900_000_000 + i)
		rows[i] = int64(ids[i])
	}
	if _, err := service.store.Pool.Exec(t.Context(), `
		insert into document_client_ids (artifact_id, client_id, actor_kind, actor_id, minted, landed)
		select $1, client, 'user', $2, true, true from unnest($3::bigint[]) as client
	`, artifactID, login, rows); err != nil {
		t.Fatal(err)
	}
	return ids
}

// writeUnderEach sends one edit under each of clients on page's socket (writeUnder) and waits for
// the room to hold every one, with page still connected.
func writeUnderEach(t *testing.T, service *Service, artifactID string, page *docstest.Peer, clients []crdt.ClientID, what string) {
	t.Helper()
	for _, client := range clients {
		writeUnder(t, page, client)
	}
	awaitHeld(t, func() *crdt.Doc { return service.srv.GetDoc(artifactID) }, clients, what)
	requireConnected(t, page, what)
}

// A socket keeps every id its updates showed is its person's while it is open, so a delete range
// parks under any of them. The table names alice for 65 ids in the document besides her page's
// own, each a row an edit landed under, as the ids of her other pages are; her page lands an edit
// under each, and then sends one frame deleting a clock past what the room holds under every one
// of the 65. The room parks a range under each, and her page stays connected.
//
// Before, the socket remembered 64 such ids (maxConfirmedClients, a cache of 64) and the park
// judge asks the socket alone, so the range under the 65th was refused and the page closed 4413
// DOC_CLIENT_ID_REFUSED, though the table names alice for that id and her edit under it had just
// landed (dispatch://INFRA-179, the round 9 class search's D2).
func TestADeleteRangeParksUnderEveryIDItsSocketHolds(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	alice := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, alice, "the document")
	held := landedRowsOf(t, service, artifactID, "alice", 65)
	writeUnderEach(t, service, artifactID, alice, held, "alice's edits under 65 ids of hers")

	// One clock past the one each edit left the room holding.
	deletes := newV1Frame(0).uint(uint64(len(held)))
	for _, client := range held {
		deletes.uint(uint64(client)).uint(1).uint(5).uint(1)
	}
	if err := alice.Write(ygsync.EncodeUpdate(deletes.done())); err != nil {
		t.Fatalf("send the deletes: %v", err)
	}
	room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }
	ended := func() bool {
		select {
		case <-alice.Ended:
			return true
		default:
			return false
		}
	}
	testwait.Holds(30*time.Second, 10*time.Millisecond, func() bool {
		return ended() || room().PendingStats().DeleteRanges == len(held)
	})
	if ended() {
		t.Fatalf("alice's page was closed with %v for deleting past the room's clocks under ids of her own its socket had written under, the room parking %d ranges",
			alice.Closed(), room().PendingStats().DeleteRanges)
	}
	if parked := room().PendingStats().DeleteRanges; parked != len(held) {
		t.Fatalf("the room parks %d delete ranges, want one under each of alice's %d ids", parked, len(held))
	}
	requireConnected(t, alice, "alice after her deletes")
}

// A socket holds at most 128 ids besides its own (maxConfirmedClients), which bounds what it
// remembers: the table names alice for one more than that in the document, each a row an edit
// landed under; her page lands an edit under 128 of them, and its edit under the next is refused
// 4413 DOC_CLIENT_ID_REFUSED with nothing of it landing, though the table names her for that id.
// Before, a socket held no such bound: past what it remembered it read an id from the table on
// every update carrying it, and its edits under any number of its person's ids landed.
func TestASocketHoldsAtMost128IDsBesidesItsOwn(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	alice := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, alice, "the document")
	ids := landedRowsOf(t, service, artifactID, "alice", maxConfirmedClients+1)
	writeUnderEach(t, service, artifactID, alice, ids[:maxConfirmedClients], "alice's edits under the ids her socket may hold")

	over := ids[maxConfirmedClients]
	writeUnder(t, alice, over)
	awaitRefused(t, alice, "alice")
	if live := service.srv.GetDoc(artifactID); live != nil && live.StateVector().Clock(over) != 0 {
		t.Fatalf("the room holds client %d to clock %d after the edit under it was refused", over, live.StateVector().Clock(over))
	}
}
