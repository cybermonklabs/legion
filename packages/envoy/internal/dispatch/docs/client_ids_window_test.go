package docs

import (
	"context"
	"os"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// These tests hold the client id rule over time (client_ids.go): the claim window counted from the
// first boot of a build that gives ids out, which bounds the claims of ids the stored history
// names and no others; the bounds on the claims no window ends; the set of the ids the stored
// history names that a room reads once; and the sweep of ids no edit used. The measurements at the
// end run only with claimCostMeasurement set.

// claimCostMeasurement names the switch the claim rule's measurements run behind: each writes a
// history of thousands of rows, or a table of ten thousand, and reports figures it asserts
// nothing of.
const claimCostMeasurement = "DISPATCH_MEASURE_CLAIM_COST"

// setClientIDsSince records since as when service's database first served a build that gives
// client ids out, as that boot would have.
func setClientIDsSince(t *testing.T, service *Service, since time.Time) {
	t.Helper()
	if _, err := service.store.Pool.Exec(context.Background(), `
		insert into document_client_ids_since (since) values ($1)
		on conflict (singleton) do update set since = excluded.since
	`, since); err != nil {
		t.Fatal(err)
	}
}

// clientIDLanded is whether the row of client in artifactID says an edit under it landed, and
// whether there is a row at all.
func clientIDLanded(t *testing.T, service *Service, artifactID string, client crdt.ClientID) (landed, stored bool) {
	t.Helper()
	err := service.store.Pool.QueryRow(context.Background(),
		`select landed from document_client_ids where artifact_id = $1 and client_id = $2`, artifactID, int64(client)).Scan(&landed)
	if err != nil {
		return false, false
	}
	return landed, true
}

// A tab of the earlier build, or any client that presents no client id, lands its edit under an
// id the stored history does not name whenever it writes: a day after the deploy, and 31 days
// after it, once the window has closed. An id the history names - a tab that typed before the
// deploy, its binding not stored, coming back under the same id - is claimable by the person who
// typed under it within the window, and once the window has closed its edit is refused, as the
// replay of a row saved under such an id would be.
func TestALegacyTabClaimsAnIDNoHistoryNamesAtAnyTimeAndANamedOneOnlyWithinTheWindow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		since  time.Duration
		named  bool
		claims bool
	}{
		{name: "a day after the deploy, an id no history names", since: 24 * time.Hour, claims: true},
		{name: "a day after the deploy, an id the history names", since: 24 * time.Hour, named: true, claims: true},
		{name: "31 days after the deploy, an id no history names", since: 31 * 24 * time.Hour, claims: true},
		{name: "31 days after the deploy, an id the history names", since: 31 * 24 * time.Hour, named: true, claims: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, artifactID, serverURL := clientIDRoom(t)
			room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }
			const tab = crdt.ClientID(1_234_567_101)
			doc := crdt.New(crdt.WithClientID(tab))
			want := "# Title\n\noAlice's paragraph.\n"
			if tc.named {
				// Before the deploy the tab typed, and the earlier build stored no binding: the
				// history names the tab's id, and no row does.
				before := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("alice"), artifactID, doc)
				awaitAnswer(t, before, "the document")
				sendTyped(t, before, 0, "b")
				awaitRendering(t, room, "# Title\n\nbAlice's paragraph.\n", "the room before the deploy")
				before.Close()
				<-before.Ended
				forgetStoredClientIDs(t, service, artifactID)
				want = "# Title\n\nobAlice's paragraph.\n"
			}
			setClientIDsSince(t, service, time.Now().Add(-tc.since))
			tabPage := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("alice"), artifactID, doc)
			awaitAnswer(t, tabPage, "the document")
			sendTyped(t, tabPage, 0, "o")
			if !tc.claims {
				awaitRefused(t, tabPage, "the earlier build's tab")
				if stored := storedClientIDs(t, service, artifactID, tab); stored != 0 {
					t.Fatalf("the refused tab's id is stored as someone's (%d rows)", stored)
				}
				return
			}
			awaitRendering(t, room, want, "the room")
			requireConnected(t, tabPage, "the earlier build's tab")
		})
	}
}

// The date the window counts from is the first boot's: a process that boots later records nothing
// over it.
func TestALaterBootDoesNotMoveWhenClientIDsWereFirstGivenOut(t *testing.T) {
	database := storetest.Open(t)
	first, _ := newTestServiceInstance(t, database)
	if err := first.RecordClientIDsSince(context.Background()); err != nil {
		t.Fatal(err)
	}
	var since time.Time
	if err := database.Pool.QueryRow(context.Background(), `select since from document_client_ids_since`).Scan(&since); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	later, _ := newTestServiceInstance(t, database)
	if err := later.RecordClientIDsSince(context.Background()); err != nil {
		t.Fatal(err)
	}
	var after time.Time
	if err := database.Pool.QueryRow(context.Background(), `select since from document_client_ids_since`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !after.Equal(since) {
		t.Fatalf("a later boot moved the date from %s to %s", since, after)
	}
}

// An id no edit used and no admission presented for 30 days is swept, and a page presenting it
// after that is given a fresh one; an id an edit landed under is never swept, however long ago its
// page was last admitted, and is given back.
func TestAnIDNoEditUsedIsSweptAndGivenAfreshWhileOneThatWroteIsKept(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	reader := openPage(t, serverURL, artifactID, "bob")
	awaitAnswer(t, reader, "the document")
	writer := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, writer, "the document")
	sendTyped(t, writer, 0, "w")
	deadline := time.Now().Add(30 * time.Second)
	for landed, _ := clientIDLanded(t, service, artifactID, writer.Doc.ClientID()); !landed; landed, _ = clientIDLanded(t, service, artifactID, writer.Doc.ClientID()) {
		if time.Now().After(deadline) {
			t.Fatal("the writer's client id was never marked as having landed an edit")
		}
		time.Sleep(20 * time.Millisecond)
	}
	reader.Close()
	writer.Close()
	<-reader.Ended
	<-writer.Ended
	if _, err := service.store.Pool.Exec(context.Background(),
		`update document_client_ids set presented_at = now() - interval '31 days' where artifact_id = $1`, artifactID); err != nil {
		t.Fatal(err)
	}
	pool, err := service.store.Pool.Compaction()
	if err != nil {
		t.Fatal(err)
	}
	swept, err := sweepUnusedClientIDs(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, stored := clientIDLanded(t, service, artifactID, reader.Doc.ClientID()); stored || swept < 1 {
		t.Fatalf("the reader's unused id is still stored (the sweep deleted %d rows)", swept)
	}
	if landed, stored := clientIDLanded(t, service, artifactID, writer.Doc.ClientID()); !stored || !landed {
		t.Fatal("the sweep deleted the id an edit landed under")
	}
	url := socketURL(serverURL, artifactID) + "&" + contracts.DocumentClientIdParameter + "="
	back := docstest.DialGiven(t, url+strconv.FormatUint(uint64(reader.Doc.ClientID()), 10), pageHeader("bob"), artifactID, func(given crdt.ClientID) *crdt.Doc {
		return crdt.New(crdt.WithClientID(given))
	})
	awaitAnswer(t, back, "the document")
	if back.Given == reader.Doc.ClientID() {
		t.Fatalf("the reader's swept id %d was given back", back.Given)
	}
	requireGiven(t, back.Given)
	again := reopenPage(t, serverURL, artifactID, "alice", writer.Doc)
	awaitAnswer(t, again, "the document")
	sendTyped(t, again, 1, "x")
	awaitRendering(t, func() *crdt.Doc { return service.srv.GetDoc(artifactID) }, "# Title\n\nwxAlice's paragraph.\n", "the room")
}

// writeUnder sends the room, on page's socket, an edit typed into a copy of page's document under
// client at clock 0: for an id no history names, what a legacy socket claims as it writes.
func writeUnder(t *testing.T, page *docstest.Peer, client crdt.ClientID) {
	t.Helper()
	if err := page.Write(ygsync.EncodeUpdate(forgedUnder(t, page.Doc, "x", client, 0))); err != nil {
		t.Fatalf("send an edit under client %d: %v", client, err)
	}
}

// awaitHeld waits for room to hold a struct under every one of clients.
func awaitHeld(t *testing.T, room func() *crdt.Doc, clients []crdt.ClientID, what string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		missing := len(clients)
		if live := room(); live != nil {
			held := live.StateVector()
			missing = 0
			for _, client := range clients {
				if held.Clock(client) == 0 {
					missing++
				}
			}
		}
		if missing == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: the room holds nothing under %d of the %d clients", what, missing, len(clients))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// claimsOf is how many claimed rows artifactID stores for the user login.
func claimsOf(t *testing.T, service *Service, artifactID, login string) int {
	t.Helper()
	var claims int
	if err := service.store.Pool.QueryRow(context.Background(), `
		select count(*) from document_client_ids where artifact_id = $1 and actor_id = $2 and not minted
	`, artifactID, login).Scan(&claims); err != nil {
		t.Fatal(err)
	}
	return claims
}

// requireNothingClaimed requires the room to hold nothing under client and no row to store it.
func requireNothingClaimed(t *testing.T, service *Service, artifactID string, client crdt.ClientID) {
	t.Helper()
	if stored := storedClientIDs(t, service, artifactID, client); stored != 0 {
		t.Fatalf("the refused client %d is stored as someone's (%d rows)", client, stored)
	}
	if live := service.srv.GetDoc(artifactID); live != nil && live.StateVector().Clock(client) != 0 {
		t.Fatalf("the room holds client %d to clock %d after its edit was refused", client, live.StateVector().Clock(client))
	}
}

// A legacy socket holds at most 64 claims of ids no stored history names, in all: its 64 claims
// land, and once they are a day old, so that the person's daily bound counts none of them, the
// same socket's 65th is refused and nothing of it is claimed.
func TestALegacySocketHoldsAtMost64ClaimsOfIDsNoHistoryNames(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	setClientIDsSince(t, service, time.Now().Add(-31*24*time.Hour))
	room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }
	legacy := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("alice"), artifactID, crdt.New(crdt.WithClientID(1_600_000_000)))
	awaitAnswer(t, legacy, "the document")
	claimed := make([]crdt.ClientID, maxConfirmedClients)
	for i := range claimed {
		claimed[i] = crdt.ClientID(1_600_000_001 + i)
		writeUnder(t, legacy, claimed[i])
	}
	awaitHeld(t, room, claimed, "the legacy socket's 64 claims")
	requireConnected(t, legacy, "the legacy socket after its 64 claims")
	if got := claimsOf(t, service, artifactID, "alice"); got != maxConfirmedClients {
		t.Fatalf("alice holds %d claims, want the socket's %d", got, maxConfirmedClients)
	}
	if _, err := service.store.Pool.Exec(context.Background(),
		`update document_client_ids set created_at = created_at - interval '1 day' where artifact_id = $1`, artifactID); err != nil {
		t.Fatal(err)
	}
	over := crdt.ClientID(1_600_000_001 + maxConfirmedClients)
	writeUnder(t, legacy, over)
	awaitRefused(t, legacy, "the legacy socket's 65th claim")
	requireNothingClaimed(t, service, artifactID, over)
}

// A person makes at most 64 claims in one document in one UTC day, whichever of their legacy
// sockets makes them, counted from the stored rows, and the ids their pages of this build were
// given count for nothing: two legacy sockets of alice's land 32 claims each, a third socket's
// claim, the 65th, is refused and nothing of it is claimed, and bob's claim in the same document
// lands.
func TestAPersonMakesAtMost64ClaimsADayInADocument(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	setClientIDsSince(t, service, time.Now().Add(-31*24*time.Hour))
	room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }
	for range 2 {
		awaitAnswer(t, openPage(t, serverURL, artifactID, "alice"), "the document in a page of this build")
	}
	for socket := range 2 {
		legacy := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("alice"), artifactID, crdt.New(crdt.WithClientID(crdt.ClientID(1_700_000_000+1000*socket))))
		awaitAnswer(t, legacy, "the document")
		claimed := make([]crdt.ClientID, maxClaimsADay/2)
		for i := range claimed {
			claimed[i] = crdt.ClientID(1_700_000_001 + 1000*socket + i)
			writeUnder(t, legacy, claimed[i])
		}
		awaitHeld(t, room, claimed, "a legacy socket's 32 claims")
		requireConnected(t, legacy, "the legacy socket after its 32 claims")
	}
	if got := claimsOf(t, service, artifactID, "alice"); got != maxClaimsADay {
		t.Fatalf("alice holds %d claims, want %d", got, maxClaimsADay)
	}
	third := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("alice"), artifactID, crdt.New(crdt.WithClientID(1_700_009_000)))
	awaitAnswer(t, third, "the document")
	over := crdt.ClientID(1_700_009_001)
	writeUnder(t, third, over)
	awaitRefused(t, third, "alice's 65th claim of the day")
	requireNothingClaimed(t, service, artifactID, over)
	bob := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("bob"), artifactID, crdt.New(crdt.WithClientID(1_700_010_000)))
	awaitAnswer(t, bob, "the document")
	writeUnder(t, bob, 1_700_010_001)
	awaitHeld(t, room, []crdt.ClientID{1_700_010_001}, "bob's claim")
	requireConnected(t, bob, "bob")
}

// The room reads its stored history for the claim rule once, at the first claim that needs it:
// alice's earlier-build tab's first edit, a claim of an id no history names, reads it, and neither
// the same tab's next edit after its binding is forgotten - a claim of an id the history names -
// nor bob's tab's first edit, a claim of an id no history names, reads it again.
func TestARoomReadsItsHistoryForTheClaimRuleOnce(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	var reads atomic.Int64
	service.onHistoryRead = func(room string, _ int) {
		if room == artifactID {
			reads.Add(1)
		}
	}
	room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }
	// A reader stays on the document throughout, so the room is the one instance throughout.
	awaitAnswer(t, openPage(t, serverURL, artifactID, "carol"), "the document in carol's page")
	tabDoc := crdt.New(crdt.WithClientID(1_234_567_301))
	tab := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("alice"), artifactID, tabDoc)
	awaitAnswer(t, tab, "the document")
	sendTyped(t, tab, 0, "a")
	awaitRendering(t, room, "# Title\n\naAlice's paragraph.\n", "the room after the first claim")
	if got := reads.Load(); got != 1 {
		t.Fatalf("the first claim read the room's history %d times, want once", got)
	}
	tab.Close()
	<-tab.Ended
	forgetStoredClientIDs(t, service, artifactID)
	back := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("alice"), artifactID, tabDoc)
	awaitAnswer(t, back, "the document after the tab's reconnect")
	sendTyped(t, back, 1, "b")
	awaitRendering(t, room, "# Title\n\nabAlice's paragraph.\n", "the room after the claim of an id the history names")
	bob := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("bob"), artifactID, crdt.New(crdt.WithClientID(1_234_567_302)))
	awaitAnswer(t, bob, "the document")
	sendTyped(t, bob, 0, "c")
	awaitRendering(t, room, "# Title\n\ncabAlice's paragraph.\n", "the room after bob's claim")
	requireConnected(t, back, "alice's tab")
	requireConnected(t, bob, "bob's tab")
	if got := reads.Load(); got != 1 {
		t.Fatalf("three claims read the room's history %d times, want once", got)
	}
}

// The set of the ids the stored history names goes with the room it was read for. A rebuild
// closes the room and replaces its history, and the rebuilt history names none of the clients the
// rebuild retired: the rebuilt room reads it afresh, and its set does not name the earlier-build
// tab's id that the set read before the rebuild named.
func TestARoomsSetOfTheIDsItsHistoryNamesDoesNotSurviveARebuild(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	ctx := context.Background()
	var reads atomic.Int64
	service.onHistoryRead = func(room string, _ int) {
		if room == artifactID {
			reads.Add(1)
		}
	}
	room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }
	const tabClient = crdt.ClientID(1_234_567_401)
	tab := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("alice"), artifactID, crdt.New(crdt.WithClientID(tabClient)))
	awaitAnswer(t, tab, "the document")
	sendTyped(t, tab, 0, "a")
	awaitRendering(t, room, "# Title\n\naAlice's paragraph.\n", "the room")
	if _, _, named, err := service.historyNames(ctx, artifactID, room(), tabClient); err != nil || !named {
		t.Fatalf("the room's set names the tab's client %d: %t (%v), want it named", tabClient, named, err)
	}
	if got := reads.Load(); got != 1 {
		t.Fatalf("the room read its history %d times before the rebuild, want once", got)
	}
	tab.Close()
	<-tab.Ended
	closeResidentRoom(t, service, artifactID)
	// The route rebuilds only a history that cannot load or stands near a cap, so the history is
	// carried past what ygo decodes, as TestARebuildWhileABrowserIsOpenRefusesNothingOfTheBindings's is.
	for _, update := range oversizedHistory(t) {
		if _, err := NewPgVersioned(service.store).AppendUpdate(ctx, artifactID, update); err != nil {
			t.Fatalf("append the history that cannot load: %v", err)
		}
	}
	if _, _, err := rebuildDocument(t, service, artifactID, nil); err != nil {
		t.Fatal(err)
	}
	awaitAnswer(t, openPage(t, serverURL, artifactID, "bob"), "the rebuilt document")
	if _, _, named, err := service.historyNames(ctx, artifactID, room(), tabClient); err != nil || named {
		t.Fatalf("the rebuilt room's set names the retired client %d: %t (%v), want it unnamed", tabClient, named, err)
	}
	if got := reads.Load(); got != 2 {
		t.Fatalf("the room read its history %d times, want twice: once before the rebuild and once after", got)
	}
}

// The claim's cost on a room whose stored history is 5,001 rows long: one typist's 5,000
// keystrokes after the seed. Before the room kept the set of the ids its history names, each
// claim waited for the room's appends and read every row: measured 2026-11-18 on this pod, on the
// build before this one, with this test's history, claimable took 7.9 to 15.3 ms over five claims
// of ids no history names, each reading all 5,001 rows, and a legacy socket's first write, its
// claim included, landed in 8.2 to 16.2 ms. Measured the same day with the set: the room's first
// claim 17.7 ms, reading all 5,001 rows, each later one 0.2 to 1.5 ms, reading none, and a legacy
// socket's first write landing in 3.1 to 38.6 ms, reading none (the pod shared with other runs).
func TestTheCostOfAClaimOnALongHistory(t *testing.T) {
	if os.Getenv(claimCostMeasurement) == "" {
		t.Skip("a measurement: set " + claimCostMeasurement + "=1 to run it")
	}
	const keystrokes = 5000
	service, artifactID, serverURL := clientIDRoom(t)
	ctx := context.Background()
	var rowsRead atomic.Int64
	service.onHistoryRead = func(room string, rows int) {
		if room == artifactID {
			rowsRead.Add(int64(rows))
		}
	}
	page := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, page, "the document")
	state := crdt.EncodeStateAsUpdateV1(page.Doc, nil)
	page.Close()
	<-page.Ended
	closeResidentRoom(t, service, artifactID)
	typist := crdt.New(crdt.WithClientID(1_300_000_001))
	if err := crdt.ApplyUpdateV1(typist, state, nil); err != nil {
		t.Fatal(err)
	}
	batch := make([]BrowserUpdate, 0, 250)
	for i := range keystrokes {
		batch = append(batch, BrowserUpdate{Update: typeAt(t, typist, i, "k"), ContentChanged: true})
		if len(batch) == cap(batch) {
			if _, err := NewPgVersioned(service.store).AppendUpdatesWithCredit(ctx, artifactID, batch); err != nil {
				t.Fatal(err)
			}
			batch = batch[:0]
		}
	}
	awaitAnswer(t, openPage(t, serverURL, artifactID, "carol"), "the long document")
	live := service.srv.GetDoc(artifactID)
	actor := model.Actor{Kind: "user", ID: "bob"}
	for i := range 5 {
		var stored int
		if err := service.store.Pool.QueryRow(ctx, `select count(*) from doc_updates where artifact_id = $1`, artifactID).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		before := rowsRead.Load()
		started := time.Now()
		if _, unnamed, err := service.claimable(ctx, artifactID, live, live.StateVector(), actor, true, uint64(1_400_000_001+i)); err != nil || !unnamed {
			t.Fatalf("claimable: unnamed %t, %v", unnamed, err)
		}
		t.Logf("claim %d of an id no history names: %v, reading %d of the history's %d rows", i+1, time.Since(started), rowsRead.Load()-before, stored)
	}
	room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }
	for i := range 3 {
		client := crdt.ClientID(1_500_000_001 + i)
		legacy := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("bob"), artifactID, crdt.New(crdt.WithClientID(client)))
		awaitAnswer(t, legacy, "the long document")
		before := rowsRead.Load()
		started := time.Now()
		sendTyped(t, legacy, 0, "x")
		for room().StateVector().Clock(client) == 0 {
			if time.Since(started) > 30*time.Second {
				t.Fatal("the claim never landed")
			}
			time.Sleep(time.Millisecond)
		}
		t.Logf("a legacy socket's first write, its claim included, landed in %v, reading %d rows", time.Since(started), rowsRead.Load()-before)
		requireConnected(t, legacy, "the legacy socket")
	}
}

// What a reconnect's whole-state handoff costs the judgement whatever the room's claims: a document
// of 5,001 ids below mintedClientFloor, one struct each, which a page hands back whole, so the
// structs under every one of them fall at clocks the room holds (heldDuplicates), and bob claims 1,
// 4, 100, 1,000 and then all 5,001 of them. It reports the best of five judgements (judgeClients) of
// the handoff from a page of this build and from a legacy socket, and the lookup of the ids' rows
// (clientOwners) each makes.
//
// Measured 2026-11-18 on this pod, before the comparison was bounded (every claimed id compared, a
// tenth of the ids claimed): 1,001 ids in a 16,251-byte handoff, 100 claimed, the lookup 1.5 ms and
// the judgement 47.8 ms; 5,001 ids in 80,251 bytes, 500 claimed, 3.2 ms and 1.25 s, since each
// comparison reads the whole update (crdt.HeldStructsDifferV1). Now an update past
// maxComparedClaims claims is not compared (refusedDuplicates): measured the same day, a 75,256-byte
// handoff of 5,002 ids with 1, 4, 100, 1,000 and 5,001 claimed, the judgement 4.6 to 10.0 ms from a
// page of this build and 5.0 to 11.5 ms from a legacy socket, refused past 4 claims, and the lookup
// 1.8 to 8.1 ms, growing with the rows it finds.
func TestTheCostOfAHandoffsJudgementWhateverTheRoomsClaims(t *testing.T) {
	if os.Getenv(claimCostMeasurement) == "" {
		t.Skip("a measurement: set " + claimCostMeasurement + "=1 to run it")
	}
	const ids = 5001
	service, artifactID, serverURL := clientIDRoom(t)
	ctx := context.Background()
	seeded := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, seeded, "the document")
	base := seeded.Doc
	batch := make([]BrowserUpdate, 0, 250)
	for i := range ids {
		batch = append(batch, BrowserUpdate{Update: forgedUnder(t, base, "x", crdt.ClientID(1_800_000_001+i), 0), ContentChanged: true})
		if len(batch) == cap(batch) || i == ids-1 {
			if _, err := NewPgVersioned(service.store).AppendUpdatesWithCredit(ctx, artifactID, batch); err != nil {
				t.Fatal(err)
			}
			batch = batch[:0]
		}
	}
	seeded.Close()
	<-seeded.Ended
	closeResidentRoom(t, service, artifactID)
	page := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, page, "the document of many clients")
	live := service.srv.GetDoc(artifactID)
	handoff := crdt.EncodeStateAsUpdateV1(page.Doc, nil)
	from, to, err := crdt.ParseUpdateMeta(handoff)
	if err != nil {
		t.Fatal(err)
	}
	alice := model.Actor{Kind: "user", ID: "alice"}
	sockets := []struct {
		name    string
		clients *socketClients
	}{
		{name: "a page of this build", clients: &socketClients{own: page.Given}},
		{name: "a legacy socket", clients: &socketClients{legacy: true}},
	}
	claimedSoFar := 0
	for _, claims := range []int{1, maxComparedClaims, 100, 1000, ids} {
		if _, err := service.store.Pool.Exec(ctx, `
			insert into document_client_ids (artifact_id, client_id, actor_kind, actor_id, minted, landed, claimed_from)
			select $1, 1800000001 + g, 'user', 'bob', false, true, 0 from generate_series($2, $3 - 1) g
		`, artifactID, claimedSoFar, claims); err != nil {
			t.Fatal(err)
		}
		claimedSoFar = claims
		for _, socket := range sockets {
			duplicated := heldDuplicates(live.StateVector(), from, to, socket.clients)
			info := websocket.PeerUpdateInfo{
				Room:    artifactID,
				Doc:     live,
				Update:  handoff,
				Context: context.WithValue(ctx, connectionContextKey{}, &connectionState{room: artifactID, actor: alice, clients: socket.clients}),
			}
			var lookups, judgements []time.Duration
			var refusal error
			for range 5 {
				started := time.Now()
				if _, err := service.clientOwners(ctx, artifactID, duplicated); err != nil {
					t.Fatal(err)
				}
				lookups = append(lookups, time.Since(started))
				started = time.Now()
				_, refusal = service.judgeClients(info)
				judgements = append(judgements, time.Since(started))
			}
			if refused := refusal != nil; refused != (socket.clients.legacy && claims > maxComparedClaims) {
				t.Fatalf("%s's handoff with %d claims: refused %t (%v)", socket.name, claims, refused, refusal)
			}
			t.Logf("%s's handoff of %d bytes, %d ids at held clocks, %d claimed by bob: the judgement %v (refused %t), its lookup %v",
				socket.name, len(handoff), len(duplicated), claims, slices.Min(judgements), refusal != nil, slices.Min(lookups))
		}
	}
}

// The bytes one claimed row of document_client_ids takes, for the bound on what the claims no
// window ends leave behind: 64 a person a document a UTC day (maxClaimsADay). It reports the
// stored size of a real claim's row (pg_column_size) and the table's whole growth, its indexes and
// TOAST included, over 10,000 claimed rows (pg_total_relation_size). Measured 2026-11-18: 88
// bytes a tuple, and 1,441,792 bytes over the 10,000 rows, 144.2 a row, so maxClaimsADay's rows
// come to 9,229 bytes a person a document a day.
func TestTheBytesOfAClaimedRow(t *testing.T) {
	if os.Getenv(claimCostMeasurement) == "" {
		t.Skip("a measurement: set " + claimCostMeasurement + "=1 to run it")
	}
	const rows = 10_000
	service, artifactID, serverURL := clientIDRoom(t)
	ctx := context.Background()
	setClientIDsSince(t, service, time.Now().Add(-31*24*time.Hour))
	legacy := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("alice"), artifactID, crdt.New(crdt.WithClientID(1_900_000_000)))
	awaitAnswer(t, legacy, "the document")
	writeUnder(t, legacy, 1_900_000_001)
	awaitHeld(t, func() *crdt.Doc { return service.srv.GetDoc(artifactID) }, []crdt.ClientID{1_900_000_001}, "the claim")
	var rowBytes int64
	if err := service.store.Pool.QueryRow(ctx, `
		select pg_column_size(c.*) from document_client_ids c where artifact_id = $1 and client_id = 1900000001
	`, artifactID).Scan(&rowBytes); err != nil {
		t.Fatal(err)
	}
	var before, after int64
	if err := service.store.Pool.QueryRow(ctx, `select pg_total_relation_size('document_client_ids')`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := service.store.Pool.Exec(ctx, `
		insert into document_client_ids (artifact_id, client_id, actor_kind, actor_id, minted, landed, claimed_from)
		select $1, 2000000000 + g, 'user', 'alice', false, true, g from generate_series(1, $2) g
	`, artifactID, rows); err != nil {
		t.Fatal(err)
	}
	if _, err := service.store.Pool.Exec(ctx, `vacuum analyze document_client_ids`); err != nil {
		t.Fatal(err)
	}
	if err := service.store.Pool.QueryRow(ctx, `select pg_total_relation_size('document_client_ids')`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	t.Logf("a claimed row's stored tuple is %d bytes (pg_column_size); %d claimed rows grew the table, its indexes included, by %d bytes, %.1f a row",
		rowBytes, rows, after-before, float64(after-before)/rows)
}
