package docs

import (
	"context"
	"encoding/json"
	"maps"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/testwait"
)

// fillCapUntilGoneWithin opens and leaves alice's pages until the row of gone is given back or
// most pages have opened, and returns how many opened and whether the row went. An open refused
// DOC_BUSY, every row of hers being live, is made again 100 ms later, for two minutes at most. A
// row an open socket presents, or one that says an edit landed under it, is never given back, so
// where that holds the opens run out; most past maxUnlandedMintsADocument gives each of her rows
// presented less recently than gone's the time to go first.
func fillCapUntilGoneWithin(t *testing.T, service *Service, artifactID, serverURL string, gone crdt.ClientID, most int) (opens int, given bool) {
	t.Helper()
	url := socketURL(serverURL, artifactID) + "&" + contracts.DocumentClientIdParameter + "="
	deadline := time.Now().Add(2 * time.Minute)
	for opens < most {
		if storedClientIDs(t, service, artifactID, gone) == 0 {
			return opens, true
		}
		if time.Now().After(deadline) {
			t.Fatalf("after %d opens the row of %d is still stored and the time is out", opens, gone)
		}
		if _, busy := openAndLeave(t, url, "alice"); busy {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		opens++
	}
	return opens, storedClientIDs(t, service, artifactID, gone) == 0
}

// An id a socket holds besides its own is presented every interval while the socket is open
// (keepPresenting, socketClients.presenting), so no mint gives its row back and it is never
// minted to another person. Alice's first tab is given id A and closes; she typed two rows
// offline under A. Her second tab replays the second row alone, which parks behind the first:
// the row of A names her and says no edit landed, so the socket holds A and its row awaits the
// socket's mark (socketClients.unmarked). The interval is cut to 300 ms, so a row no socket
// presents lapses in 900 ms, and her page loads past maxUnlandedMintsADocument give back the
// least recently presented. Before, the socket presented its own id alone, so A's row lapsed
// while the tab stayed open and wrote under A, a page load gave it back, and bob's update under A
// was then minted A for bob.
func TestR12AnIDASocketHoldsBesidesItsOwnIsPresentedWhileItIsOpen(t *testing.T) {
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
	heard := awaitHeardStatus(t, second, 1, "alice's second tab")
	requireConnected(t, second, "alice's second tab")
	if landed, stored := clientIDLanded(t, service, artifactID, a); !stored || landed {
		t.Fatalf("the row of %d after alice's second tab's parked replay: stored %t, landed %t; want stored and not landed", a, stored, landed)
	}
	t.Logf("alice's second tab's replay of her second row under %d answered %q; the room parks %+v", a, heard[0], room().PendingStats())

	time.Sleep(time.Second) // a row no socket presents lapses (3 x 300 ms)
	opens, gone := fillCapUntilGoneWithin(t, service, artifactID, serverURL, a, 2*maxUnlandedMintsADocument)
	requireConnected(t, second, "alice's second tab")
	if gone {
		t.Fatalf("the row of %d, which alice's open second tab holds and writes under, was given back after %d page loads of hers", a, opens)
	}
	if owner := clientIDOwner(t, service, artifactID, a); owner != "alice" {
		t.Fatalf("the row of %d names %q after %d page loads of alice's, want alice", a, owner, opens)
	}
	t.Logf("after %d page loads of alice's the row of %d still names her", opens, a)

	bobUpdate := forgedUnder(t, room(), "BOB", a, 0)
	bob := openPage(t, serverURL, artifactID, "bob")
	awaitAnswer(t, bob, "bob's page")
	if err := bob.Write(ygsync.EncodeUpdate(bobUpdate)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-bob.Ended:
	case <-time.After(10 * time.Second):
		t.Fatalf("bob's update under %d, alice's id, was not refused; the row names %q", a, clientIDOwner(t, service, artifactID, a))
	}
	if closed := bob.Closed(); closed == nil || closed.Code != contracts.DocumentTooLargeCloseCode || closed.Text != clientIDRefusedReason {
		t.Fatalf("bob's page under alice's id %d was closed %v, want %d %s", a, closed, contracts.DocumentTooLargeCloseCode, clientIDRefusedReason)
	}
	if owner := clientIDOwner(t, service, artifactID, a); owner != "alice" {
		t.Fatalf("the row of %d names %q after bob's refused update, want alice", a, owner)
	}
	if rendering, _ := renderDocument(room()); strings.Contains(rendering, "BOB") {
		t.Fatalf("the room renders %q with bob's refused update under alice's id", rendering)
	}
}

// r12Socket is a document socket's context as its admission makes it, for login, given own and a
// row that names it and says no edit landed under it, and what ends it.
func r12Socket(login string, own crdt.ClientID) (context.Context, context.CancelFunc) {
	connection := &connectionState{actor: model.Actor{Kind: "user", ID: login}, clients: &socketClients{own: own, stored: true}}
	return context.WithCancel(context.WithValue(context.Background(), connectionContextKey{}, connection))
}

// r12Ended is the records the room keeps of ended connections still waiting, by the person each
// was signed in as.
func r12Ended(applies *roomApplies) map[string][]crdt.ClientID {
	applies.parked.mu.Lock()
	defer applies.parked.mu.Unlock()
	ended := map[string][]crdt.ClientID{}
	for _, wait := range applies.parked.waits {
		if wait.ended != nil {
			ended[wait.ended.actor.ID] = slices.Collect(maps.Keys(wait.needs))
		}
	}
	return ended
}

// A sender whose parked frames a refused drain discards is dropped as the discard closes it
// (closeDiscardedSenders, dropDiscarded), so its connection's end keeps nothing of it: what it
// parked will not land. Another sender whose connection ends with its frame still parked is kept,
// its person and the id its frame reached (parkedWaits.end), for the store of the landing to mark.
func TestR12ADiscardedSendersEndKeepsNothingAndAnotherClosedTabsParkedFrameIsKept(t *testing.T) {
	service := calibratedService()
	var applies roomApplies
	alice, aliceEnds := r12Socket("alice", 5_000_000_001)
	bob, bobEnds := r12Socket("bob", 5_000_000_002)
	applies.parked.note(alice, "alice's page", crdt.StateVector{5_000_000_001: 3})
	applies.parked.note(bob, "bob's page", crdt.StateVector{5_000_000_002: 2})
	service.closeDiscardedSenders("room", &applies, nil, &crdt.DrainDiscardedError{Senders: []any{"alice's page"}})
	aliceEnds()
	bobEnds()
	if !testwait.Holds(time.Second, 5*time.Millisecond, func() bool { return applies.parked.held.Load() == 1 && applies.parked.endedHeld.Load() == 1 }) {
		t.Fatalf("once both connections ended the room lists %d senders, %d of them ended (%v), want bob's kept alone",
			applies.parked.held.Load(), applies.parked.endedHeld.Load(), r12Ended(&applies))
	}
	if ended := r12Ended(&applies); len(ended) != 1 || len(ended["bob"]) != 1 || ended["bob"][0] != 5_000_000_002 {
		t.Fatalf("the room keeps %v of the ended connections, want bob's id 5000000002 alone", ended)
	}
}

// The room keeps no more records of ended connections still waiting than its queue holds
// (boundEnded), the oldest dropped first: each waited on a struct or a range of its own sender's
// the queue held as it ended, so the rest wait on what a discard took after their senders had
// gone, which names none of them.
func TestR12TheRecordsOfEndedSendersAreHeldToWhatTheQueueHolds(t *testing.T) {
	var applies roomApplies
	room := crdt.New(crdt.WithClientID(1))
	for i, login := range []string{"alice", "bob", "carol"} {
		own := crdt.ClientID(5_000_000_001 + i)
		connection, end := r12Socket(login, own)
		applies.parked.note(connection, login+"'s page", crdt.StateVector{own: 1})
		end()
		if !testwait.Holds(time.Second, 5*time.Millisecond, func() bool { return applies.parked.endedHeld.Load() == int32(i+1) }) {
			t.Fatalf("the room keeps %d records once %s's connection ended, want %d", applies.parked.endedHeld.Load(), login, i+1)
		}
	}
	applies.noteParked(ygws.PeerUpdateInfo{Doc: room}, crdt.PendingStats{}, crdt.PendingStats{Items: 1})
	if ended := r12Ended(&applies); len(ended) != 1 || ended["carol"] == nil {
		t.Fatalf("with one struct parked the room keeps %v of the ended connections, want carol's, the latest, alone", ended)
	}
	if got := applies.parked.held.Load(); got != 1 {
		t.Fatalf("the room lists %d senders, want 1", got)
	}
}

// The list of parked senders is a Go map, which keeps the slots of the most entries it ever held,
// and since a connection's record stays once it ends with its frames still parked
// (parkedWaits.end), a room's list can reach the thousands of records its queue bounds. A list
// that held 8,192 senders and fell to one keeps no more than 64 KiB above that one in a map made
// at its size (parkedWaits.shrinkLocked), whichever way the rest went: dropped past what the queue
// holds (boundEnded), taken out by a landing (landedParked), dropped by a discard
// (dropDiscarded), or ended one connection at a time with nothing to mark (end). Before, the map
// kept the slots of all 8,192, about 436 KB no room charge counts, for as long as the room ran.
func TestR12TheListOfParkedSendersKeepsNoSlotsForItsPeak(t *testing.T) {
	const peak = 8192
	const own = crdt.ClientID(5_000_000_001)
	// noted is a room's list of peak senders, each a page of alice's noted on connection.
	noted := func(connection context.Context) (*roomApplies, []any) {
		applies := &roomApplies{}
		origins := make([]any, peak)
		for i := range origins {
			origins[i] = i
			applies.parked.note(connection, origins[i], crdt.StateVector{own: uint64(i + 1)})
		}
		return applies, origins
	}
	for _, fall := range []struct {
		name string
		// to is a list of peak senders fallen to the first of them.
		to func(t *testing.T) *roomApplies
	}{
		{"the records of ended connections past what the queue holds (boundEnded)", func(t *testing.T) *roomApplies {
			connection, end := r12Socket("alice", own)
			applies, _ := noted(connection)
			end()
			if !testwait.Holds(30*time.Second, 5*time.Millisecond, func() bool { return applies.parked.endedHeld.Load() == peak }) {
				t.Fatalf("the room keeps %d records once the connection ended, want %d", applies.parked.endedHeld.Load(), peak)
			}
			applies.parked.boundEnded(1)
			return applies
		}},
		{"a landing (landedParked)", func(t *testing.T) *roomApplies {
			applies, origins := noted(nil)
			applies.parked.mu.Lock()
			for _, wait := range applies.parked.waits {
				if wait.origin != origins[0] {
					wait.coveredAt = 1
				}
			}
			applies.parked.mu.Unlock()
			if landed := applies.landedParked(1); len(landed) != peak-1 {
				t.Fatalf("the landing took out %d senders, want %d", len(landed), peak-1)
			}
			return applies
		}},
		{"a discard (dropDiscarded)", func(t *testing.T) *roomApplies {
			applies, origins := noted(nil)
			applies.dropDiscarded(origins[1:])
			return applies
		}},
		{"each connection's end (end)", func(t *testing.T) *roomApplies {
			applies, origins := noted(nil)
			for _, origin := range origins[1:] {
				applies.parked.end(origin)
			}
			return applies
		}},
	} {
		t.Run(fall.name, func(t *testing.T) {
			applies := fall.to(t)
			w := &applies.parked
			w.mu.Lock()
			defer w.mu.Unlock()
			if got := len(w.waits); got != 1 || w.held.Load() != 1 {
				t.Fatalf("the list holds %d senders (held %d) after the fall, want 1", got, w.held.Load())
			}
			withPeak := liveHeap()
			rebuilt := make(map[any]*parkedWait, len(w.waits))
			maps.Copy(rebuilt, w.waits)
			w.waits = rebuilt
			kept := int64(withPeak) - int64(liveHeap())
			runtime.KeepAlive(applies)
			t.Logf("a list that held %d senders and fell to one by %s keeps %d bytes more than one made at its size", peak, fall.name, kept)
			if kept > 64<<10 {
				t.Fatalf("a list of parked senders that held %d and fell to one by %s keeps %d bytes more than one made at its size", peak, fall.name, kept)
			}
		})
	}
}

// An id a socket takes up besides its own is presented as the socket takes it up (judgeClients,
// presentRows), not first at the socket's next interval: the row of a closed tab's id is most
// likely older than the give-back's clientPresenceLapses intervals by the time a page replays it.
// Alice's first tab is given id A and closes, its row last presented four hours ago; she typed
// two rows offline under A. Her second tab replays the second row alone, which parks behind the
// first: the row of A names her and says no edit landed, so the socket takes A up. The interval
// is ten minutes, so no socket presents anything within the test and no row her page loads add
// lapses. Those page loads fill her maxUnlandedMintsADocument unlanded rows, and the next one
// finds every row live and is refused DOC_BUSY. Before, A's row stayed four hours old until the
// socket's first interval, and the page load past the cap gave it back while the tab held A and
// wrote under it.
func TestR12AnIDASocketTakesUpIsPresentedAsItTakesItUp(t *testing.T) {
	presenceIntervalForTest.Store(int64(10 * time.Minute))
	t.Cleanup(func() { presenceIntervalForTest.Store(0) })
	service, artifactID, serverURL := clientIDRoom(t)

	first := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, first, "alice's first tab")
	a := first.Given
	first.Close()
	<-first.Ended
	saved1 := typeAt(t, first.Doc, 0, "OFF")
	saved2 := typeAt(t, first.Doc, 3, "LINE")
	if _, err := service.store.Pool.Exec(context.Background(),
		`update document_client_ids set presented_at = now() - interval '4 hours' where artifact_id = $1 and client_id = $2`,
		artifactID, int64(a)); err != nil {
		t.Fatal(err)
	}

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
	heard := awaitHeardStatus(t, second, 1, "alice's second tab")
	requireConnected(t, second, "alice's second tab")
	if landed, stored := clientIDLanded(t, service, artifactID, a); !stored || landed {
		t.Fatalf("the row of %d after alice's second tab's parked replay: stored %t, landed %t; want stored and not landed", a, stored, landed)
	}
	t.Logf("alice's second tab's replay of her second row under %d answered %q", a, heard[0])

	url := socketURL(serverURL, artifactID) + "&" + contracts.DocumentClientIdParameter + "="
	opens, refused := 0, false
	for !refused && opens <= maxUnlandedMintsADocument {
		if _, busy := openAndLeave(t, url, "alice"); busy {
			refused = true
		} else {
			opens++
		}
	}
	requireConnected(t, second, "alice's second tab")
	if owner := clientIDOwner(t, service, artifactID, a); owner != "alice" {
		t.Fatalf("the row of %d, which alice's open second tab took up and writes under, names %q after %d page loads of hers: a page load gave it back", a, owner, opens)
	}
	if !refused {
		t.Fatalf("none of alice's %d page loads was refused DOC_BUSY, so none passed her %d unlanded rows", opens, maxUnlandedMintsADocument)
	}
	t.Logf("after %d page loads of alice's the next was refused DOC_BUSY, and the row of %d still names her", opens, a)
}

// A page of an earlier build, whose socket presented no client id, carries awareness under any id
// below mintedClientFloor (socketClients.announces), an id another person claimed among them: a
// claim is known to the table alone, which nothing reads for an awareness frame. So bob's legacy
// tab claims its id by typing under it, and mallory's legacy tab shows alice a cursor under that
// id, while mallory's edit under it is refused (judgeClients). A cursor and a presence only, and
// from a legacy socket only: a page of this build carries awareness under its own id alone
// (TestACursorOrAnEditUnderAnotherPersonsClientBeforeSheTypesReachesNoOne).
func TestR12ALegacySocketsCursorUnderAnotherPersonsClaimedIDReachesTheRoom(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }
	const tab = crdt.ClientID(1_234_574_100)
	bob := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("bob"), artifactID, crdt.New(crdt.WithClientID(tab)))
	awaitAnswer(t, bob, "bob's legacy tab")
	sendTyped(t, bob, 0, "b")
	awaitRendering(t, room, "# Title\n\nbAlice's paragraph.\n", "the room")
	if owner := clientIDOwner(t, service, artifactID, tab); owner != "bob" {
		t.Fatalf("the row of bob's legacy tab's id %d names %q after its edit, want bob", tab, owner)
	}
	alice := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, alice, "alice's page")
	mallory := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("mallory"), artifactID, crdt.New(crdt.WithClientID(1_234_574_101)))
	awaitAnswer(t, mallory, "mallory's legacy tab")
	if err := mallory.SetAwareness(uint64(tab), map[string]any{"user": map[string]string{"name": "bob"}, "cursor": "set by mallory"}); err != nil {
		t.Fatal(err)
	}
	if err := alice.WaitForAwareness(30*time.Second, func(states map[uint64]json.RawMessage) bool {
		return strings.Contains(string(states[uint64(tab)]), "set by mallory")
	}); err != nil {
		t.Fatalf("alice was not shown mallory's legacy cursor under bob's claimed id %d: %v", tab, err)
	}
	if err := mallory.Write(ygsync.EncodeUpdate(forgedUnder(t, mallory.Doc, "EVIL", tab, 1))); err != nil {
		t.Fatal(err)
	}
	awaitRefused(t, mallory, "mallory, writing under bob's claimed id")
}
