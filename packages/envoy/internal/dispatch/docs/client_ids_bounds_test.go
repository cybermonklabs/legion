package docs

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// These tests hold the bounds on what a person stores in document_client_ids, on every kind of
// socket (client_ids.go): a read-only admission and a request that never becomes a socket store
// nothing; a person's unlanded minted rows in a document stop at maxUnlandedMintsADocument, past
// which a mint gives back a row no open socket holds; "landed" is an edit integrated under the id,
// not one that only parked; and every claim is held to the day's bound.

// unlandedMintsOf is how many rows artifactID stores for login of ids an admission gave out that
// no edit landed under.
func unlandedMintsOf(t *testing.T, service *Service, artifactID, login string) int {
	t.Helper()
	var rows int
	if err := service.store.Pool.QueryRow(context.Background(),
		`select count(*) from document_client_ids where artifact_id = $1 and actor_id = $2 and minted and not landed`,
		artifactID, login).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

// openAndLeave opens a document socket of login's at url, reads its first frame and closes it, as
// a page does that the person leaves at once. given is the id the frame gave the page, and busy
// whether the room closed the socket 4503 DOC_BUSY in its place.
func openAndLeave(t *testing.T, url, login string) (given crdt.ClientID, busy bool) {
	t.Helper()
	connection, _, err := gws.DefaultDialer.Dial(url, pageHeader(login))
	if err != nil {
		t.Fatalf("open a socket of %s's: %v", login, err)
	}
	defer connection.Close()
	client, named, _, err := docstest.ReadGivenClient(connection)
	var closed *gws.CloseError
	switch {
	case errors.As(err, &closed) && closed.Code == contracts.DocumentBusyCloseCode && closed.Text == contracts.DocumentBusyCloseReason:
		return 0, true
	case err != nil || !named:
		t.Fatalf("a socket of %s's was given no client id in its first frame: %v", login, err)
	}
	return client, false
}

// awaitHeard waits for page, which hears what the room answers (docstest.Peer.Hear), to have heard
// want.
func awaitHeard(t *testing.T, page *docstest.Peer, want, what string) {
	t.Helper()
	if !testwait.Holds(30*time.Second, 10*time.Millisecond, func() bool {
		for _, heard := range page.Heard() {
			if strings.HasPrefix(heard.What, want) {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("%s: the page never heard %q, only %v", what, want, page.Heard())
	}
}

// awaitLanded waits for client's row in artifactID to say an edit landed under it.
func awaitLanded(t *testing.T, service *Service, artifactID string, client crdt.ClientID, what string) {
	t.Helper()
	if !testwait.Holds(30*time.Second, 20*time.Millisecond, func() bool {
		landed, _ := clientIDLanded(t, service, artifactID, client)
		return landed
	}) {
		t.Fatalf("%s: the row of client id %d never said an edit landed under it", what, client)
	}
}

// The architect's case for the give-back: a tab whose row is the oldest of its person's keeps
// writing while it is connected, through mints by the same person past the cap. Alice's tab
// opens first and types nothing while she opens and leaves maxUnlandedMintsADocument + 16 more
// pages of the document. Past the cap each mint gives back her least recently presented row that
// no open socket holds - a page she has left - and never the tab's, which the tab presents again
// every interval while it is open (keepPresenting). The interval is cut to 300 ms here, so a left
// page's row lapses within the test (clientPresenceLapses of it, 900 ms); an open past the cap
// that finds none lapsed yet is refused 4503 DOC_BUSY, and is opened again, as the editor retries.
// Her unlanded rows never pass the cap, and the tab's row is stored after every mint. Then the tab
// types: its edit lands under its id, it is still connected, and its row says the edit landed.
func TestATabWhoseRowIsTheOldestKeepsWritingThroughMintsPastTheCap(t *testing.T) {
	presenceIntervalForTest.Store(int64(300 * time.Millisecond))
	t.Cleanup(func() { presenceIntervalForTest.Store(0) })
	service, artifactID, serverURL := clientIDRoom(t)
	room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }
	tab := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, tab, "alice's tab")
	url := socketURL(serverURL, artifactID) + "&" + contracts.DocumentClientIdParameter + "="
	const past = 16
	minted, refused := 0, 0
	deadline := time.Now().Add(2 * time.Minute)
	for minted < maxUnlandedMintsADocument-1+past {
		if time.Now().After(deadline) {
			t.Fatalf("alice's opens gave out %d ids in two minutes, %d refused busy", minted, refused)
		}
		if _, busy := openAndLeave(t, url, "alice"); busy {
			refused++
			time.Sleep(100 * time.Millisecond)
			continue
		}
		minted++
		if rows := unlandedMintsOf(t, service, artifactID, "alice"); rows > maxUnlandedMintsADocument {
			t.Fatalf("after %d opens alice holds %d unlanded rows in the document, past the %d a person holds", minted, rows, maxUnlandedMintsADocument)
		}
		if stored := storedClientIDs(t, service, artifactID, tab.Given); stored != 1 {
			t.Fatalf("after %d opens the row of alice's open tab is gone: a mint gave back the id an open page writes under", minted)
		}
	}
	t.Logf("alice's opens gave out %d ids past her tab's, %d of them past the cap of %d, and %d opens were refused busy until a left page's row lapsed",
		minted, minted-(maxUnlandedMintsADocument-1), maxUnlandedMintsADocument, refused)
	requireConnected(t, tab, "alice's tab")
	sendTyped(t, tab, 0, "t")
	awaitRendering(t, room, "# Title\n\ntAlice's paragraph.\n", "the room after the tab typed")
	requireConnected(t, tab, "alice's tab after it typed")
	awaitLanded(t, service, artifactID, tab.Given, "alice's tab")
}

// A read-only admission stores no row and claims nothing, and the page it admits writes with no
// loss once it is admitted writable. Bob's page opens while the issue is closed: it is sent an id
// drawn at mintedClientFloor or above that no row names, and so is his page presenting an id below
// it that he could claim if he could write. Alice's page, whose id has a row of hers, is given that
// id back read-only, and adds no row. Once the issue reopens, bob's page presents the id it was
// sent, finds no row, and is given one afresh, which the page takes since it wrote nothing under
// the first (the dashboard's takeClientId); what it types lands under the fresh id, and its row
// says so.
func TestAReadOnlyAdmissionStoresNoRowAndItsPageWritesUnderAFreshIDOnceWritable(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	setClientIDsSince(t, service, time.Now().Add(-24*time.Hour))
	room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }
	writer := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, writer, "alice's page")
	sendTyped(t, writer, 0, "w")
	awaitRendering(t, room, "# Title\n\nwAlice's paragraph.\n", "the room")
	writer.Close()
	writeIssueClosed(t, service.store, "DOC-1", true)
	countRows := func() int {
		t.Helper()
		var n int
		if err := service.store.Pool.QueryRow(context.Background(),
			`select count(*) from document_client_ids where artifact_id = $1`, artifactID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	rows := countRows()

	reader := openPage(t, serverURL, artifactID, "bob")
	awaitAnswer(t, reader, "bob's read-only page")
	requireGiven(t, reader.Given)
	if stored := storedClientIDs(t, service, artifactID, reader.Given); stored != 0 {
		t.Fatalf("bob's read-only page was given id %d, stored in %d rows", reader.Given, stored)
	}
	const unclaimed = crdt.ClientID(1_234_572_001)
	presenting := docstest.DialGiven(t, socketURL(serverURL, artifactID)+"&"+contracts.DocumentClientIdParameter+"="+strconv.FormatUint(uint64(unclaimed), 10),
		pageHeader("bob"), artifactID, func(given crdt.ClientID) *crdt.Doc { return crdt.New(crdt.WithClientID(given)) })
	awaitAnswer(t, presenting, "bob's read-only page presenting an id no row names")
	if presenting.Given == unclaimed {
		t.Fatalf("a read-only admission kept id %d, which no row names, for bob's page", unclaimed)
	}
	requireGiven(t, presenting.Given)
	kept := reopenPage(t, serverURL, artifactID, "alice", writer.Doc)
	awaitAnswer(t, kept, "alice's page reopened read-only")
	if after := countRows(); after != rows {
		t.Fatalf("three read-only admissions took the document's rows from %d to %d", rows, after)
	}
	for _, page := range []*docstest.Peer{reader, presenting, kept} {
		page.Close()
	}

	writeIssueClosed(t, service.store, "DOC-1", false)
	drawn := reader.Given
	again := docstest.DialGiven(t, socketURL(serverURL, artifactID)+"&"+contracts.DocumentClientIdParameter+"="+strconv.FormatUint(uint64(drawn), 10),
		pageHeader("bob"), artifactID, func(given crdt.ClientID) *crdt.Doc { return crdt.New(crdt.WithClientID(given)) })
	awaitAnswer(t, again, "bob's page reconnected writable")
	if again.Given == drawn {
		t.Fatalf("bob's writable reconnect was given back %d, the id a read-only admission drew and stored nowhere", drawn)
	}
	requireGiven(t, again.Given)
	if stored := storedClientIDs(t, service, artifactID, again.Given); stored != 1 {
		t.Fatalf("bob's writable page was given id %d, stored in %d rows", again.Given, stored)
	}
	sendTyped(t, again, 0, "b")
	awaitRendering(t, room, "# Title\n\nbwAlice's paragraph.\n", "the room after bob's page typed")
	requireConnected(t, again, "bob's writable page")
	awaitLanded(t, service, artifactID, again.Given, "bob's writable page")
}

// A request that never becomes a socket stores nothing: one that is no websocket upgrade is
// answered 400, and an upgrade from another origin 403, as ygo's upgrade answers them, before the
// admission gives out a client id; a same-origin upgrade is given one.
func TestARequestThatNeverBecomesASocketStoresNoClientID(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	address := socketURL(serverURL, artifactID) + "&" + contracts.DocumentClientIdParameter + "="
	request, err := http.NewRequest(http.MethodGet, "http"+strings.TrimPrefix(address, "ws"), nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header = pageHeader("eve")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("a plain GET of the document's socket was answered %d, want 400", response.StatusCode)
	}
	header := pageHeader("eve")
	header.Set("Origin", "https://elsewhere.example")
	connection, response, err := gws.DefaultDialer.Dial(address, header)
	if connection != nil {
		_ = connection.Close()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("an upgrade from another origin: %v, %v; want it refused 403", response, err)
	}
	if rows := unlandedMintsOf(t, service, artifactID, "eve"); rows != 0 {
		t.Fatalf("two requests that never became sockets stored %d rows for eve", rows)
	}
	if _, busy := openAndLeave(t, address, "eve"); busy {
		t.Fatal("eve's same-origin socket was refused busy")
	}
	if rows := unlandedMintsOf(t, service, artifactID, "eve"); rows != 1 {
		t.Fatalf("eve's same-origin socket left %d rows, want its one", rows)
	}
}

// "Landed" is an edit integrated under the socket's own id, not one that only parked: alice's page
// sends its second keystroke before its first, so the second waits behind a clock of its own the
// room lacks and is answered not applied, and the row still says nothing landed, which leaves it a
// mint's to give back once the page has gone. Her first keystroke then lands both, and marks it.
func TestAFrameThatOnlyParksMarksNoRowLanded(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }
	page := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, page, "the document")
	page.Hear()
	first := typeAt(t, page.Doc, 0, "a")
	second := typeAt(t, page.Doc, 1, "b")
	if err := page.Write(ygsync.EncodeUpdate(second)); err != nil {
		t.Fatal(err)
	}
	awaitHeard(t, page, "SyncStatus 0", "alice's second keystroke, sent first")
	// The row is marked off the read loop (recordLanded): a short wait lets a mark in flight land.
	time.Sleep(300 * time.Millisecond)
	if landed, stored := clientIDLanded(t, service, artifactID, page.Given); !stored || landed {
		t.Fatalf("after a frame of alice's that only parked her row is stored %t and says an edit landed %t", stored, landed)
	}
	if err := page.Write(ygsync.EncodeUpdate(first)); err != nil {
		t.Fatal(err)
	}
	awaitRendering(t, room, "# Title\n\nabAlice's paragraph.\n", "the room")
	requireConnected(t, page, "alice's page")
	awaitLanded(t, service, artifactID, page.Given, "alice's page once its first keystroke landed")
}

// A claim of an update that stood is kept even where its structs only parked, and binds the id to
// its claimant (settleClients): a legacy tab's second keystroke, sent before its first, claims the
// tab's id, parks behind the clock the room lacks and is answered not applied, and the row names
// alice from then on. So mallory's struct under the id at the clock the tab types next is refused
// as one under another person's id, rather than parked there to drop the tab's own keystroke as a
// duplicate once the gap fills. The tab's first keystroke then lands, and its parked second with
// it, and the id is alice's throughout.
func TestAClaimWhoseFrameOnlyParksIsKeptAndBindsTheID(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }
	const tab = crdt.ClientID(1_234_573_001)
	ownerOf := func() string {
		t.Helper()
		var owner string
		if err := service.store.Pool.QueryRow(context.Background(),
			`select actor_id from document_client_ids where artifact_id = $1 and client_id = $2`, artifactID, int64(tab)).Scan(&owner); err != nil {
			t.Fatalf("read the row of id %d: %v", tab, err)
		}
		return owner
	}
	legacy := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("alice"), artifactID, crdt.New(crdt.WithClientID(tab)))
	awaitAnswer(t, legacy, "the document")
	legacy.Hear()
	first := typeAt(t, legacy.Doc, 0, "a")
	second := typeAt(t, legacy.Doc, 1, "b")
	if err := legacy.Write(ygsync.EncodeUpdate(second)); err != nil {
		t.Fatal(err)
	}
	awaitHeard(t, legacy, "SyncStatus 0", "the tab's second keystroke, sent first")
	if owner := ownerOf(); owner != "alice" {
		t.Fatalf("the claim of the tab's parked keystroke names %q, want alice", owner)
	}
	mallory := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("mallory"), artifactID, crdt.New(crdt.WithClientID(1_234_573_900)))
	awaitAnswer(t, mallory, "mallory's document")
	if err := mallory.Write(ygsync.EncodeUpdate(forgedUnder(t, mallory.Doc, "EVIL", tab, 0))); err != nil {
		t.Fatal(err)
	}
	awaitRefused(t, mallory, "mallory, writing under the tab's id at the clock it types next")
	if err := legacy.Write(ygsync.EncodeUpdate(first)); err != nil {
		t.Fatal(err)
	}
	awaitRendering(t, room, "# Title\n\nabAlice's paragraph.\n", "the room")
	requireConnected(t, legacy, "the legacy tab")
	if owner := ownerOf(); owner != "alice" {
		t.Fatalf("after the tab's keystrokes landed the id's row names %q, want alice", owner)
	}
	if landed, _ := clientIDLanded(t, service, artifactID, tab); !landed {
		t.Fatalf("the claim of id %d is stored unlanded", tab)
	}
}

// Every claim is held to the claims a person makes in a document in a UTC day, a legacy socket's
// claim of an id the stored history names among them, which no count held before: alice's tab of
// the earlier build typed before the deploy, so the history names its id and no row does, and
// once she has made maxClaimsADay claims in the document today, the tab's claim of it inside the
// window is refused, and nothing of it is claimed.
func TestALegacyClaimOfAnIDTheHistoryNamesIsHeldToTheDaysClaims(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }
	const tab = crdt.ClientID(1_234_574_001)
	doc := crdt.New(crdt.WithClientID(tab))
	before := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("alice"), artifactID, doc)
	awaitAnswer(t, before, "the document")
	sendTyped(t, before, 0, "b")
	awaitRendering(t, room, "# Title\n\nbAlice's paragraph.\n", "the room before the deploy")
	before.Close()
	forgetStoredClientIDs(t, service, artifactID)
	setClientIDsSince(t, service, time.Now().Add(-24*time.Hour))
	if _, err := service.store.Pool.Exec(context.Background(), `
		insert into document_client_ids (artifact_id, client_id, actor_kind, actor_id, minted, landed, claimed_from)
		select $1, 2200000000 + g, 'user', 'alice', false, true, 0 from generate_series(1, $2::int) g
	`, artifactID, maxClaimsADay); err != nil {
		t.Fatal(err)
	}
	tabPage := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("alice"), artifactID, doc)
	awaitAnswer(t, tabPage, "the document")
	sendTyped(t, tabPage, 0, "o")
	awaitRefused(t, tabPage, "the earlier build's tab past alice's claims of the day")
	if stored := storedClientIDs(t, service, artifactID, tab); stored != 0 {
		t.Fatalf("the tab's id %d is stored past alice's claims of the day (%d rows)", tab, stored)
	}
	if claims := claimsOf(t, service, artifactID, "alice"); claims != maxClaimsADay {
		t.Fatalf("alice holds %d claims after the refused one, want the %d she made", claims, maxClaimsADay)
	}
}
