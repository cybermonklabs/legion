package docs

import (
	"bytes"
	"errors"
	"io"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	"github.com/reearth/ygo/provider/websocket"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
)

// Review 8 repro (ClientIdsAudit), made by the decision to delete the step 2's cut (Q1, round 9)
// into an assertion of what now holds. The repro's step 2 of about 14 MB - one GC struct under
// alice's minted id at a clock the room lacks, and a delete set of 3,000,000 one-clock ranges -
// was decoded whole by the socket filter's cut (crdt.DiffUpdateV1) on the read path, outside every
// load slot, on any socket: 192,090,872 bytes allocated for it there, review 8 measured. Sent now
// on a socket of mallory's, it reaches the peer update hook byte for byte as it was sent, so the
// filter decoded and rewrote none of it; and what the process allocates from the message's first
// byte to the hook's entry, before the load slot the hook takes there for an update past
// unslottedApplyBytes, is ygo's own read of the message (io.ReadAll) and no more than the frames'
// headers besides. Inside the slot the hook judges it as it judges any update, and refuses it
// whole: mallory is answered not applied and closed 4413 DOC_CLIENT_ID_REFUSED, and nothing of it
// lands, the delete set included.
func TestRV8AStepTwoReachesTheHookUnreadAndCostsOnlyItsOwnReadBeforeTheSlot(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }
	// hookEntry is what the hook was handed the step 2 as, and what the process had allocated in
	// all as it was.
	type hookEntry struct {
		allocated uint64
		unread    bool
	}
	// watched is the step 2 the hook is watched for, set once it is built.
	var watched atomic.Pointer[[]byte]
	reached := make(chan hookEntry, 1)
	weigh := service.srv.OnPeerUpdate
	service.srv.OnPeerUpdate = func(info websocket.PeerUpdateInfo, apply func() error) error {
		if update := watched.Load(); update != nil && len(info.Update) == len(*update) {
			var stats runtime.MemStats
			runtime.ReadMemStats(&stats)
			reached <- hookEntry{allocated: stats.TotalAlloc, unread: bytes.Equal(info.Update, *update)}
		}
		return weigh(info, apply)
	}
	alice := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, alice, "the document")

	// Mallory's socket is a page of this build written to raw, so that nothing on the sending side
	// copies the message while the server reads it: the client's writer masks each frame in a
	// buffer of its own as it goes.
	dialer := *gws.DefaultDialer
	dialer.WriteBufferSize = 1 << 20
	connection, _, err := dialer.Dial(socketURL(serverURL, artifactID)+"&"+contracts.DocumentClientIdParameter+"=", pageHeader("mallory"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	given, named, _, err := docstest.ReadGivenClient(connection)
	if err != nil || !named {
		t.Fatalf("mallory's socket was given no client id in its first frame: %v", err)
	}
	const ranges = 3_000_000
	f := newV1Frame(1).group(1, uint64(alice.Given), 1000).gc(1)
	f.uint(1).uint(uint64(given)).uint(ranges)
	for i := range ranges {
		f.uint(uint64(2 * i)).uint(1)
	}
	stepTwo := f.done()
	if len(stepTwo) <= unslottedApplyBytes {
		t.Fatalf("the step 2 is %d bytes, at or under the %d an apply takes no load slot for", len(stepTwo), unslottedApplyBytes)
	}
	// The Hocuspocus message: the document's name, the sync message type (0), then the sync step 2.
	message := encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarString(artifactID)
		encoder.WriteVarUint(0)
		encoder.WriteVarUint(ygsync.MsgSyncStep2)
		encoder.WriteVarBytes(stepTwo)
	})
	// What ygo's reader allocates for a message of that length, the one read of it the server
	// needs (provider/websocket/server.go, io.ReadAll): its growth follows the length alone.
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	readStart := stats.TotalAlloc
	if read, err := io.ReadAll(bytes.NewReader(message)); err != nil || len(read) != len(message) {
		t.Fatalf("read the message back: %d bytes, %v", len(read), err)
	}
	runtime.ReadMemStats(&stats)
	ownRead := stats.TotalAlloc - readStart

	watched.Store(&stepTwo)
	runtime.GC()
	runtime.ReadMemStats(&stats)
	sent := stats.TotalAlloc
	started := time.Now()
	if err := connection.WriteMessage(gws.BinaryMessage, message); err != nil {
		t.Fatal(err)
	}
	var entry hookEntry
	select {
	case entry = <-reached:
	case <-time.After(time.Minute):
		t.Fatal("the peer update hook was never handed mallory's step 2")
	}
	allocated := entry.allocated - sent
	t.Logf("a %d-byte step 2 in a %d-byte message reached the hook in %s, handed on unread: %t; the process allocated %d bytes from its first byte to the hook, against %d for one read of the message (%.2f x)",
		len(stepTwo), len(message), time.Since(started), entry.unread, allocated, ownRead, float64(allocated)/float64(ownRead))
	if !entry.unread {
		t.Fatal("the hook was handed another update than the step 2 mallory sent: something before it rewrote the frame")
	}
	// Past the read, the frames' headers and ygo's own few objects for the message, and what the
	// idle room and its pool allocate meanwhile: well under a mebibyte, where the cut allocated
	// 192 MB for this frame.
	if slack := uint64(4 << 20); allocated > ownRead+slack {
		t.Fatalf("the process allocated %d bytes for the step 2 before the hook's slot, past the %d its one read costs by more than %d: something before the hook decoded or copied it",
			allocated, ownRead, slack)
	}

	// Inside the slot the hook refuses it whole.
	if err := connection.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var statuses []uint64
	var closed *gws.CloseError
	for {
		_, read, err := connection.ReadMessage()
		if err != nil {
			if !errors.As(err, &closed) {
				t.Fatalf("mallory's socket ended without a close frame: %v", err)
			}
			break
		}
		decoder := encoding.NewDecoder(read)
		if _, err := decoder.ReadVarString(); err != nil {
			continue
		}
		if kind, err := decoder.ReadVarUint(); err != nil || kind != 8 { // Hocuspocus's SyncStatus
			continue
		}
		if applied, err := decoder.ReadVarUint(); err == nil {
			statuses = append(statuses, applied)
		}
	}
	if closed.Code != contracts.DocumentTooLargeCloseCode || closed.Text != clientIDRefusedReason {
		t.Fatalf("mallory's socket was closed %d %s, want %d %s", closed.Code, closed.Text, contracts.DocumentTooLargeCloseCode, clientIDRefusedReason)
	}
	if len(statuses) != 1 || statuses[0] != 0 {
		t.Fatalf("mallory's step 2 was answered %v, want one answer of not applied", statuses)
	}
	if clock := room().StateVector().Clock(alice.Given); clock != 0 {
		t.Fatalf("the room holds alice's client to clock %d, want 0: the refused step 2's struct landed", clock)
	}
	if parked := room().PendingStats(); parked.Items != 0 || parked.DeleteRanges != 0 {
		t.Fatalf("the room parks %+v after the refused step 2, want nothing of it", parked)
	}
}

// Review 8 repro (ClientIdsAudit): mallory, an editor, reads alice's client id and next clock off
// what the room relays, and sends a frame of no structs whose delete set names alice's id at the
// clocks she has not typed yet. judgeClients judges no deletion (client_ids.go:55-56, the soleClient
// fast path at :864 passes groups == 0), so the room parks the ranges. Alice's next keystroke at
// those clocks then lands and is deleted by the parked ranges in the same apply: she is answered
// applied, stays connected and is told nothing, and the room, bob and the store never hold what she
// typed. Asserts the correct behaviour, so it fails where the defect stands.
func TestRV8AForgedDeleteOfAnotherPersonsFutureClocksDropsHerNextKeystrokeInSilence(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	alice := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, alice, "the document")
	bob := openPage(t, serverURL, artifactID, "bob")
	awaitAnswer(t, bob, "the document")
	mallory := openPage(t, serverURL, artifactID, "mallory")
	awaitAnswer(t, mallory, "the document")

	victim := alice.Doc.ClientID()
	sendTyped(t, alice, 0, "a")
	if err := mallory.WaitForText(func(p *docstest.Peer) string {
		if p.Doc.StateVector().Clock(victim) >= 1 {
			return "seen"
		}
		return ""
	}, "seen", time.Minute); err != nil {
		t.Fatalf("mallory was never relayed alice's keystroke: %v", err)
	}
	next := mallory.Doc.StateVector().Clock(victim)
	// A delete set over alice's next 1,000 clocks, and no struct.
	forged := newV1Frame(0).uint(1).uint(uint64(victim)).uint(1).uint(next).uint(1000).done()
	if err := mallory.Write(ygsync.EncodeUpdate(forged)); err != nil {
		t.Fatal(err)
	}
	room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }
	deadline := time.Now().Add(10 * time.Second)
	for stats := room().PendingStats(); stats.Items == 0 && stats.DeleteRanges == 0 && time.Now().Before(deadline); stats = room().PendingStats() {
		time.Sleep(10 * time.Millisecond)
	}
	t.Logf("after mallory's frame the room's pending queue is %+v", room().PendingStats())
	mallorysClosed := false
	select {
	case <-mallory.Ended:
		mallorysClosed = true
	case <-time.After(500 * time.Millisecond):
	}
	t.Logf("mallory closed: %t", mallorysClosed)

	alice.Hear()
	sendTyped(t, alice, 1, "b")
	sendTyped(t, alice, 2, "c")
	want := "# Title\n\nabcAlice's paragraph.\n"
	deadline = time.Now().Add(5 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		got, _ = renderDocument(room())
		if got == want {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	bobs, _ := renderDocument(bob.Doc)
	alices, _ := renderDocument(alice.Doc)
	requireConnected(t, alice, "alice")
	t.Logf("alice heard: %v", alice.Heard())
	if got != want {
		t.Fatalf("after mallory's delete of alice's future clocks, alice's two keystrokes were taken without a refusal and the room renders %q (bob's page %q, alice's own page %q), want %q",
			got, bobs, alices, want)
	}
}

// Review 8 repro (ClientIdsAudit): inside the claim window a page of this build - a socket that
// presents the client id parameter, which any signed-in editor's script can do - claims every id
// below mintedClientFloor no row names that its updates write under, with no count: judgeClients
// caps only a legacy socket's claims (client_ids.go:911, capped = unnamed && legacy), so
// claimClients runs uncapped (:921, len(capped) == 0) and neither maxConfirmedClients nor
// maxClaimsADay is asked. Each claim is stored landed, so no sweep takes it (:752-753). Asserts
// the bound the comment at client_ids.go:160-163 says holds, so it fails where the defect stands.
//
// Review 8's repro is kept as it was up to its tail, its 192 updates included. Round 9 holds every
// claim, on every kind of socket, to maxClaimsASocket and maxClaimsADay, 64 each, and that changed
// the assertions after the updates: the repro required the room to hold a struct under all 192
// ids and the page to stay connected, which only an uncapped claim allows. Now the first 64
// claims land, update 65 is refused 4413 DOC_CLIENT_ID_REFUSED with nothing claimed for it, and the
// repro's own last assertion stands: at most 64 landed rows remain. Its message names the socket's
// bound by the constant round 9 gave it, maxClaimsASocket, in place of maxConfirmedClients, which
// is now the size of a cache alone.
func TestRV8APageOfThisBuildClaimsPastTheDaysBoundInsideTheWindow(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	setClientIDsSince(t, service, time.Now().Add(-24*time.Hour))
	room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }
	page := openPage(t, serverURL, artifactID, "mallory")
	awaitAnswer(t, page, "the document")
	const n = 3 * maxClaimsADay
	claimed := make([]crdt.ClientID, n)
	for i := range claimed {
		claimed[i] = crdt.ClientID(1_800_000_001 + i)
		writeUnder(t, page, claimed[i])
	}
	awaitRefused(t, page, "mallory's page at its 65th claim")
	awaitHeld(t, room, claimed[:maxClaimsADay], "the page's first 64 claims")
	requireNothingClaimed(t, service, artifactID, claimed[maxClaimsADay])
	var landed int
	if err := service.store.Pool.QueryRow(t.Context(), `
		select count(*) from document_client_ids where artifact_id = $1 and actor_id = 'mallory' and not minted and landed
	`, artifactID).Scan(&landed); err != nil {
		t.Fatal(err)
	}
	t.Logf("one socket of mallory's made %d claims in one document today, each stored landed", landed)
	if landed > maxClaimsADay {
		t.Fatalf("one page of this build made %d claims in one document in one day, past maxClaimsADay %d and maxClaimsASocket %d, each stored landed so no sweep takes it",
			landed, maxClaimsADay, maxClaimsASocket)
	}
}

// Review 8's measurement (ClientIdsAudit), kept, and made into an assertion of the bounds that now
// hold. It opened and closed 300 sockets of one person in a row against a resident room, each
// presenting the client id parameter with no id, and every open stored a row for 30 days, read-only
// ones included: 300 rows, 230 a second from one sequential client. Now eve's 300 read-only opens -
// her page presenting a schema_creatable no manifest has, which no fleet admits (schemaPageOf) -
// store no row at all, each sent an id drawn at mintedClientFloor or above that is stored nowhere;
// and her 300 writable opens stop at maxUnlandedMintsADocument unlanded rows, every open past it
// refused 4503 DOC_BUSY while each of those rows is live, and the row of the page she holds open
// from the start, the oldest of them, is never given back.
func TestRV8AReadOnlyOpensStoreNoRowAndWritableOpensStopAtTheMintCap(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	keeper := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, keeper, "the document")
	held := openPage(t, serverURL, artifactID, "eve")
	awaitAnswer(t, held, "eve's page held open")
	url := socketURL(serverURL, artifactID) + "&" + contracts.DocumentClientIdParameter + "="
	const opens = 300
	started := time.Now()
	for range opens {
		given, busy := openAndLeave(t, url+"&schema_creatable=0", "eve")
		if busy {
			t.Fatal("a read-only open of eve's was refused busy")
		}
		requireGiven(t, given)
		if stored := storedClientIDs(t, service, artifactID, given); stored != 0 {
			t.Fatalf("a read-only open of eve's was given id %d, stored in %d rows", given, stored)
		}
	}
	readOnly := time.Since(started)
	if rows := unlandedMintsOf(t, service, artifactID, "eve"); rows != 1 {
		t.Fatalf("%d read-only opens of eve's left her %d rows, want the one of her open page", opens, rows)
	}
	started = time.Now()
	given, busy := 0, 0
	for range opens {
		if _, refused := openAndLeave(t, url, "eve"); refused {
			busy++
		} else {
			given++
		}
	}
	writable := time.Since(started)
	rows := unlandedMintsOf(t, service, artifactID, "eve")
	t.Logf("%d read-only opens of one person took %s and left no row; %d writable opens took %s, %d given an id and %d refused busy, leaving %d rows",
		opens, readOnly, opens, writable, given, busy, rows)
	if rows != maxUnlandedMintsADocument {
		t.Fatalf("%d writable opens of eve's left her %d unlanded rows, want the %d a person holds in a document", opens, rows, maxUnlandedMintsADocument)
	}
	if given != maxUnlandedMintsADocument-1 || busy != opens-given {
		t.Fatalf("%d writable opens of eve's were given %d ids and refused %d busy, want %d given beside her open page's", opens, given, busy, maxUnlandedMintsADocument-1)
	}
	if stored := storedClientIDs(t, service, artifactID, held.Given); stored != 1 {
		t.Fatal("the row of eve's page held open, the oldest, was given back")
	}
	requireConnected(t, held, "eve's page held open")
}
