package docs

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
// not one that only parked, and a landing the store did not mark leaves the row presented; and
// every claim is held to the day's bound.

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

// A read-only admission stores no row and claims nothing, and the page it admits keeps its id and
// writes with no loss once it is admitted writable. Bob's page opens while the issue is closed: it
// is sent an id drawn at mintedClientFloor or above that no row names, and so is his page
// presenting an id below it that he could claim if he could write. Bob's page writes under the id
// it was sent, as the dashboard's editor does while it reads, and the read-only room takes none of
// it. Alice's page, whose id has a row of hers, is given that id back read-only, and adds no row.
// Once the issue reopens, bob's page presents the id it was sent, finds no row, and is minted it:
// its row is stored then, the page keeps the id its document wrote under, rather than a fresh one
// the dashboard's takeClientId would rebuild the document for, and what it wrote while read-only
// lands under it with its answer to the room's sync step 1, and its row says so.
func TestAReadOnlyAdmissionStoresNoRowAndItsPageKeepsItsIDOnceWritable(t *testing.T) {
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
	sendTyped(t, reader, 0, "b")
	requireConnected(t, reader, "bob's read-only page after it wrote")
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
	again := reopenPage(t, serverURL, artifactID, "bob", reader.Doc)
	awaitAnswer(t, again, "bob's page reconnected writable")
	if stored := storedClientIDs(t, service, artifactID, drawn); stored != 1 {
		t.Fatalf("bob's writable page kept id %d, stored in %d rows", drawn, stored)
	}
	awaitRendering(t, room, "# Title\n\nbwAlice's paragraph.\n", "the room after bob's page reconnected writable")
	requireConnected(t, again, "bob's writable page")
	awaitLanded(t, service, artifactID, drawn, "bob's writable page")
}

// A read-only page keeps the id its admission sent it across its read-only reconnects. The
// dashboard's editor writes under the id its document holds as it reads - it gives the document's
// headings ids - and the read-only room takes none of that, so a reconnect that sent the page
// another id would find its document had written under the first, and the page would rebuild it
// and tell a reader who typed nothing that it held edits under an identity Dispatch did not give
// them (the dashboard's takeClientId; e2e/doc-reload-line.e2e.ts). So bob's read-only page, which
// has written under the id it was sent, presents that id while the issue is still closed and is
// given it back, and no row is stored for it. A read-only page presenting an id another person's
// row names is not given that id.
func TestAReadOnlyPageKeepsTheIDItWasSentAcrossReadOnlyReconnects(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	writer := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, writer, "alice's page")
	writer.Close()
	writeIssueClosed(t, service.store, "DOC-1", true)

	reader := openPage(t, serverURL, artifactID, "bob")
	awaitAnswer(t, reader, "bob's read-only page")
	requireGiven(t, reader.Given)
	sendTyped(t, reader, 0, "r")
	requireConnected(t, reader, "bob's read-only page after it wrote")
	reader.Close()
	again := reopenPage(t, serverURL, artifactID, "bob", reader.Doc)
	awaitAnswer(t, again, "bob's page reconnected read-only")
	if stored := storedClientIDs(t, service, artifactID, again.Given); stored != 0 {
		t.Fatalf("bob's read-only reconnect was given back id %d, stored in %d rows", again.Given, stored)
	}
	again.Close()

	presenting := docstest.DialGiven(t, socketURL(serverURL, artifactID)+"&"+contracts.DocumentClientIdParameter+"="+strconv.FormatUint(uint64(writer.Given), 10),
		pageHeader("bob"), artifactID, func(given crdt.ClientID) *crdt.Doc { return crdt.New(crdt.WithClientID(given)) })
	awaitAnswer(t, presenting, "bob's read-only page presenting alice's id")
	if presenting.Given == writer.Given {
		t.Fatalf("a read-only admission gave bob's page id %d, which alice's row names", writer.Given)
	}
	requireGiven(t, presenting.Given)
	if stored := storedClientIDs(t, service, artifactID, presenting.Given); stored != 0 {
		t.Fatalf("bob's read-only page presenting alice's id was given id %d, stored in %d rows", presenting.Given, stored)
	}
}

// A request that never becomes a socket stores nothing: each request ygo's upgrade refuses is
// answered exactly as gorilla's own Upgrader.Upgrade answers it, with the same origin rule
// (sameOrigin), before the admission gives out a client id (refusedAtUpgrade). Each row is a real
// HTTP request carrying a websocket upgrade's headers with the one change its name says, and
// what it wants is what a plain gorilla Upgrader writes for that same request, in whichever
// gorilla the build resolves: the status for a request naming no websocket in its Upgrade header
// is 400 in gorilla/websocket v1.5.3 and 426 in later commits. A same-origin upgrade is then given
// one id.
func TestARequestThatNeverBecomesASocketStoresNoClientID(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	address := socketURL(serverURL, artifactID) + "&" + contracts.DocumentClientIdParameter + "="
	stored := 0
	for _, row := range []struct {
		name   string
		method string
		header func(http.Header)
	}{
		{"a plain GET, no websocket upgrade", http.MethodGet, func(h http.Header) { h.Del("Connection"); h.Del("Upgrade") }},
		{"an upgrade naming no websocket", http.MethodGet, func(h http.Header) { h.Del("Upgrade") }},
		{"a HEAD, which the socket's GET route takes too", http.MethodHead, func(http.Header) {}},
		{"a websocket version other than 13", http.MethodGet, func(h http.Header) { h.Set("Sec-WebSocket-Version", "8") }},
		{"a key that is not 16 bytes in base64", http.MethodGet, func(h http.Header) { h.Set("Sec-WebSocket-Key", "c2hvcnQ=") }},
		{"no key", http.MethodGet, func(h http.Header) { h.Del("Sec-WebSocket-Key") }},
		{"another origin", http.MethodGet, func(h http.Header) { h.Set("Origin", "https://elsewhere.example") }},
	} {
		request, err := http.NewRequest(row.method, "http"+strings.TrimPrefix(address, "ws"), nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header = pageHeader("eve")
		request.Header.Set("Connection", "Upgrade")
		request.Header.Set("Upgrade", "websocket")
		request.Header.Set("Sec-WebSocket-Version", "13")
		request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		row.header(request.Header)
		// gorilla's own answer to the same request. The recorder is no http.Hijacker, so a request
		// gorilla would take is answered 500 here: every row must be one it refuses.
		gorilla := httptest.NewRecorder()
		_, _ = (&gws.Upgrader{CheckOrigin: sameOrigin}).Upgrade(gorilla, request.Clone(context.Background()), nil)
		want, wantVersion := gorilla.Code, gorilla.Header().Get("Sec-WebSocket-Version")
		if want < 400 || want >= 500 {
			t.Fatalf("%s: gorilla's own upgrade answered %d, not a refusal of the request", row.name, want)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("%s: %v", row.name, err)
		}
		_ = response.Body.Close()
		if response.StatusCode != want || response.Header.Get("Sec-WebSocket-Version") != wantVersion {
			t.Errorf("%s was answered %d with Sec-WebSocket-Version %q, want %d with %q as gorilla's own upgrade answers it",
				row.name, response.StatusCode, response.Header.Get("Sec-WebSocket-Version"), want, wantVersion)
		}
		t.Logf("%s: answered %d, as gorilla's own upgrade answers it", row.name, response.StatusCode)
		if rows := unlandedMintsOf(t, service, artifactID, "eve"); rows != stored {
			t.Errorf("%s never became a socket and stored %d rows for eve", row.name, rows-stored)
			stored = rows
		}
	}
	if _, busy := openAndLeave(t, address, "eve"); busy {
		t.Fatal("eve's same-origin socket was refused busy")
	}
	if rows := unlandedMintsOf(t, service, artifactID, "eve"); rows != stored+1 {
		t.Fatalf("eve's same-origin socket stored %d rows, want its one", rows-stored)
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

// A landing whose mark the store did not write leaves its socket presenting the row, so no mint
// gives back the id an open page writes under, and the socket's next landing writes the mark. The
// store refuses the first mark of a landing once (a trigger on document_client_ids), and the
// presence interval is cut to 300 ms, so a row no socket presents is a mint's to give back 900 ms
// after its last presentation (clientPresenceLapses). Alice's first keystroke lands and its mark
// is refused; past that lapse her row is still live, as the mint reads it; her second keystroke
// lands and its mark is written.
func TestALandingTheStoreDidNotMarkLeavesItsSocketPresentingTheRow(t *testing.T) {
	presenceIntervalForTest.Store(int64(300 * time.Millisecond))
	t.Cleanup(func() { presenceIntervalForTest.Store(0) })
	service, artifactID, serverURL := clientIDRoom(t)
	room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }
	pool := service.store.Pool
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `drop trigger if exists dispatch_test_fail_first_landing on document_client_ids`)
		_, _ = pool.Exec(context.Background(), `drop function if exists dispatch_test_fail_first_landing()`)
		_, _ = pool.Exec(context.Background(), `drop sequence if exists dispatch_test_landing_failure`)
	})
	for _, statement := range []string{
		`create sequence dispatch_test_landing_failure`,
		`create function dispatch_test_fail_first_landing() returns trigger language plpgsql as $$
		begin
			if nextval('dispatch_test_landing_failure') = 1 then
				raise exception 'the test fails this landing''s mark';
			end if;
			return new;
		end $$`,
		`create trigger dispatch_test_fail_first_landing before update on document_client_ids for each row
		when (new.landed and not old.landed) execute function dispatch_test_fail_first_landing()`,
	} {
		if _, err := pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("make the store refuse the first landing's mark: %v", err)
		}
	}
	page := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, page, "alice's page")
	sendTyped(t, page, 0, "a")
	awaitRendering(t, room, "# Title\n\naAlice's paragraph.\n", "the room after alice's first keystroke")
	if !testwait.Holds(30*time.Second, 20*time.Millisecond, func() bool {
		var asked bool
		if err := pool.QueryRow(context.Background(), `select is_called from dispatch_test_landing_failure`).Scan(&asked); err != nil {
			t.Fatal(err)
		}
		return asked
	}) {
		t.Fatal("the store was never asked to mark alice's row after her first keystroke landed")
	}
	if landed, stored := clientIDLanded(t, service, artifactID, page.Given); !stored || landed {
		t.Fatalf("after the store refused its mark, alice's row is stored %t and says an edit landed %t", stored, landed)
	}
	lapse := clientPresenceLapses * presenceInterval()
	time.Sleep(lapse + presenceInterval())
	if !testwait.Holds(10*time.Second, 20*time.Millisecond, func() bool {
		var live bool
		if err := pool.QueryRow(context.Background(), `
			select presented_at >= now() - $3::bigint * interval '1 microsecond'
			from document_client_ids where artifact_id = $1 and client_id = $2
		`, artifactID, int64(page.Given), lapse.Microseconds()).Scan(&live); err != nil {
			t.Fatal(err)
		}
		return live
	}) {
		t.Fatalf("alice's open page stopped presenting its row once the store refused its landing's mark: unpresented for %s, a mint may give back the id it writes under", lapse)
	}
	sendTyped(t, page, 1, "b")
	awaitRendering(t, room, "# Title\n\nabAlice's paragraph.\n", "the room after alice's second keystroke")
	awaitLanded(t, service, artifactID, page.Given, "alice's page once its second keystroke landed")
	requireConnected(t, page, "alice's page")
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
