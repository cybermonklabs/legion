package docs

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// These tests hold the client id rule (client_ids.go) to what a person sees: an edit or a cursor
// sent under another person's client id reaches no one, and every honest editor - a tab coming
// back, two tabs of one person, a rebuild, a restart, an agent's write, and a tab of the build
// before this one - lands what it typed. Each page is opened through client_ids_pages_test.go.

const clientIDsSeed = "# Title\n\nAlice's paragraph.\n"

// clientIDTexts is every text of node in document order.
func clientIDTexts(node any) []*crdt.YXmlText {
	var texts []*crdt.YXmlText
	var walk func(node any)
	walk = func(node any) {
		switch node := node.(type) {
		case *crdt.YXmlText:
			texts = append(texts, node)
		case *crdt.YXmlFragment:
			for _, child := range node.Children() {
				walk(child)
			}
		case *crdt.YXmlElement:
			for _, child := range node.Children() {
				walk(child)
			}
		}
	}
	walk(node)
	return texts
}

// typeAt types text at offset into the seed's paragraph, in one transaction of doc, and returns
// the update it made.
func typeAt(t *testing.T, doc *crdt.Doc, offset int, text string) []byte {
	t.Helper()
	update := docstest.Transact(doc, func(txn *crdt.Transaction) {
		clientIDTexts(txn.GetXmlFragment(fragmentName))[1].Insert(txn, offset, text, nil)
	})
	if update == nil {
		t.Fatalf("typing %q made no update", text)
	}
	return update
}

// sendTyped types text at offset on page's document and sends the room the update, as a
// keystroke does.
func sendTyped(t *testing.T, page *docstest.Peer, offset int, text string) {
	t.Helper()
	if err := page.Write(ygsync.EncodeUpdate(typeAt(t, page.Doc, offset, text))); err != nil {
		t.Fatalf("send %q: %v", text, err)
	}
}

// forgedUnder is an update typing text at the start of the paragraph of a copy of source, with its
// one struct re-encoded under client at clock: what a signed-in person who has read client's next
// clock off the room sends to write ahead of that client's own keystroke.
func forgedUnder(t *testing.T, source *crdt.Doc, text string, client crdt.ClientID, clock uint64) []byte {
	t.Helper()
	maker := crdt.New(crdt.WithClientID(crdt.ClientID(910_000_000 + clock)))
	if err := crdt.ApplyUpdateV1(maker, crdt.EncodeStateAsUpdateV1(source, nil), nil); err != nil {
		t.Fatal(err)
	}
	made := typeAt(t, maker, 0, text)
	decoder := encoding.NewDecoder(made)
	if groups, _ := decoder.ReadVarUint(); groups != 1 {
		t.Fatalf("the made update has %d client groups, want 1", groups)
	}
	structs, _ := decoder.ReadVarUint()
	_, _ = decoder.ReadVarUint() // the maker's client
	_, _ = decoder.ReadVarUint() // its clock
	rest := append([]byte(nil), decoder.RemainingBytes()...)
	return encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarUint(1)
		encoder.WriteVarUint(structs)
		encoder.WriteVarUint(uint64(client))
		encoder.WriteVarUint(clock)
		encoder.WriteRaw(rest)
	})
}

// awaitRendering waits for doc to render want, the room's own document or a page's.
func awaitRendering(t *testing.T, doc func() *crdt.Doc, want, what string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		if current := doc(); current != nil {
			got, _ = renderDocument(current)
			if got == want {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s renders %q, want %q", what, got, want)
}

// clientIDRoom is a document seeded with clientIDsSeed and served over websockets.
func clientIDRoom(t *testing.T) (*Service, string, string) {
	t.Helper()
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, clientIDsSeed)
	return service, artifactID, newTestServer(t, service).URL
}

// The defect this rule closes, and acceptance 1 of INFRA-179's binding: mallory, reading alice's
// next clock off what the room relays, sends a struct under alice's client id at that clock while
// alice's own keystroke at that clock is in flight. The room refuses mallory's frame whole and
// closes her page, and alice's keystroke lands where she typed it, for her, the room and bob.
func TestAnEditForgedUnderAnotherPersonsClientIsRefusedAndHersLandsWhereSheTypedIt(t *testing.T) {
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
	// Alice types "b" at her next clock; its frame is in flight while mallory's arrives.
	inFlight := typeAt(t, alice.Doc, 1, "b")
	if err := mallory.Write(ygsync.EncodeUpdate(forgedUnder(t, mallory.Doc, "EVIL", victim, next))); err != nil {
		t.Fatal(err)
	}
	awaitRefused(t, mallory, "mallory")
	if live := service.srv.GetDoc(artifactID); live == nil || live.StateVector().Clock(victim) != next {
		t.Fatalf("the room holds alice's client to clock %d after mallory's frame, want %d: the forged struct landed", live.StateVector().Clock(victim), next)
	}
	if err := alice.Write(ygsync.EncodeUpdate(inFlight)); err != nil {
		t.Fatal(err)
	}
	want := "# Title\n\nabAlice's paragraph.\n"
	awaitRendering(t, func() *crdt.Doc { return service.srv.GetDoc(artifactID) }, want, "the room")
	awaitRendering(t, func() *crdt.Doc { return bob.Doc }, want, "bob's page")
	sendTyped(t, alice, 2, "c")
	awaitRendering(t, func() *crdt.Doc { return bob.Doc }, "# Title\n\nabcAlice's paragraph.\n", "bob's page after alice's next keystroke")
	requireConnected(t, alice, "alice")
	if rendering, _ := renderDocument(alice.Doc); rendering != "# Title\n\nabcAlice's paragraph.\n" {
		t.Fatalf("alice's own page renders %q", rendering)
	}
}

// The race a binding by first writer would have opened, and the presence it would have left
// forgeable: a page announces its presence under its client id as it opens, so mallory reads
// alice's id off the room's awareness before alice has typed anything. A cursor mallory then
// announces under that id reaches no one, a struct she sends under it at clock 0 is refused, and
// alice's first keystroke, at that clock, lands.
func TestACursorOrAnEditUnderAnotherPersonsClientBeforeSheTypesReachesNoOne(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	alice := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, alice, "the document")
	bob := openPage(t, serverURL, artifactID, "bob")
	awaitAnswer(t, bob, "the document")
	mallory := openPage(t, serverURL, artifactID, "mallory")
	awaitAnswer(t, mallory, "the document")

	victim := uint64(alice.Doc.ClientID())
	if err := alice.SetAwareness(victim, map[string]any{"user": map[string]string{"name": "alice"}}); err != nil {
		t.Fatal(err)
	}
	if err := mallory.WaitForAwareness(30*time.Second, func(states map[uint64]json.RawMessage) bool {
		return len(states[victim]) > 0
	}); err != nil {
		t.Fatalf("mallory never read alice's client id off the room's awareness: %v", err)
	}
	if err := bob.WaitForAwareness(30*time.Second, func(states map[uint64]json.RawMessage) bool {
		return len(states[victim]) > 0
	}); err != nil {
		t.Fatalf("bob never saw alice's presence: %v", err)
	}
	if err := mallory.SetAwareness(victim, map[string]any{"user": map[string]string{"name": "alice"}, "cursor": "forged by mallory"}); err != nil {
		t.Fatal(err)
	}
	if err := bob.WaitForAwareness(2*time.Second, func(states map[uint64]json.RawMessage) bool {
		return strings.Contains(string(states[victim]), "forged by mallory")
	}); err == nil {
		t.Fatalf("bob was shown mallory's cursor under alice's client id: %s", bob.AwarenessStates()[victim])
	}

	if err := mallory.Write(ygsync.EncodeUpdate(forgedUnder(t, mallory.Doc, "EVIL", crdt.ClientID(victim), 0))); err != nil {
		t.Fatal(err)
	}
	awaitRefused(t, mallory, "mallory")
	sendTyped(t, alice, 0, "a")
	want := "# Title\n\naAlice's paragraph.\n"
	awaitRendering(t, func() *crdt.Doc { return service.srv.GetDoc(artifactID) }, want, "the room")
	awaitRendering(t, func() *crdt.Doc { return bob.Doc }, want, "bob's page")
	requireConnected(t, alice, "alice")
}

// The same tab coming back: its socket drops, the person types on while it is away, and the page
// reconnects with the document it kept. The room gives it its own client id back, its offline
// edits land with its reconnect, and it types on under the same id.
func TestTheSameTabReconnectingKeepsItsClientIDAndItsOfflineEditsLand(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	alice := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, alice, "the document")
	sendTyped(t, alice, 0, "a")
	room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }
	awaitRendering(t, room, "# Title\n\naAlice's paragraph.\n", "the room")
	alice.Close()
	<-alice.Ended
	// Typed while the socket was down: two edits the page keeps.
	typeAt(t, alice.Doc, 1, "b")
	typeAt(t, alice.Doc, 2, "c")

	back := reopenPage(t, serverURL, artifactID, "alice", alice.Doc)
	awaitAnswer(t, back, "the document after the reconnect")
	awaitRendering(t, room, "# Title\n\nabcAlice's paragraph.\n", "the room after the reconnect")
	sendTyped(t, back, 3, "d")
	awaitRendering(t, room, "# Title\n\nabcdAlice's paragraph.\n", "the room after the next keystroke")
	requireConnected(t, back, "alice's reconnected page")
}

// Two tabs of one person: each is given its own client id and types under it, and one tab replays
// the other's saved edit - an edit typed in a tab that closed before it was sent, which the
// browser kept and the next tab of the document restores under the client id it was typed under.
func TestTwoTabsOfOnePersonEachWriteAndOneReplaysTheOthersSavedEdit(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	first := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, first, "the document")
	second := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, second, "the document")
	if first.Doc.ClientID() == second.Doc.ClientID() {
		t.Fatalf("both of alice's tabs were given client id %d", first.Doc.ClientID())
	}
	sendTyped(t, first, 0, "1")
	sendTyped(t, second, 0, "2")
	room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }
	waitForRoomRendering(t, room, func(rendering string) bool {
		return strings.Contains(rendering, "1") && strings.Contains(rendering, "2")
	}, "the room holding both tabs' edits")
	// The first tab types once more and closes before the edit is sent: the browser keeps the row.
	first.Close()
	<-first.Ended
	saved := typeAt(t, first.Doc, 0, "3")
	// The second tab restores the row as the restore does: applied to its document, then sent.
	if err := crdt.ApplyUpdateV1(second.Doc, saved, nil); err != nil {
		t.Fatal(err)
	}
	if err := second.Write(ygsync.EncodeUpdate(saved)); err != nil {
		t.Fatal(err)
	}
	waitForRoomRendering(t, room, func(rendering string) bool {
		return strings.Contains(rendering, "1") && strings.Contains(rendering, "2") && strings.Contains(rendering, "3")
	}, "the room holding both tabs' edits and the replayed one")
	requireConnected(t, second, "alice's second tab")
}

// A rebuild while a browser is open replaces the history alice's page typed into. The rebuild
// takes a room no browser is connected to (RebuildDocument refuses a live room far from both
// caps), so alice's page is the one the rebuild found her socket gone from, still holding the
// document she typed into. Her reconnect presents the id she was given, which the binding still
// names her for: whatever the rebuild's own guard makes of the history she hands back, this rule
// refuses none of it. The editor then reads the rebuilt document into a fresh page, which is given
// a fresh client id and writes.
func TestARebuildWhileABrowserIsOpenRefusesNothingOfTheBindings(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	alice := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, alice, "the document")
	sendTyped(t, alice, 0, "a")
	room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }
	awaitRendering(t, room, "# Title\n\naAlice's paragraph.\n", "the room")
	alice.Close()
	<-alice.Ended
	closeResidentRoom(t, service, artifactID)
	// The route rebuilds only a history that cannot load or stands near a cap, so the history is
	// carried past what ygo decodes, as TestRebuildDocumentRestoresDocumentThatCannotLoad's is.
	for _, update := range oversizedHistory(t) {
		if _, err := NewPgVersioned(service.store).AppendUpdate(context.Background(), artifactID, update); err != nil {
			t.Fatalf("append the history that cannot load: %v", err)
		}
	}
	if _, _, err := rebuildDocument(t, service, artifactID, nil); err != nil {
		t.Fatal(err)
	}
	held := reopenPage(t, serverURL, artifactID, "alice", alice.Doc)
	select {
	case <-held.Ended:
	case <-time.After(3 * time.Second):
	}
	requireNoClientRefusal(t, held, "alice's page holding the replaced history")
	fresh := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, fresh, "the rebuilt document")
	if fresh.Doc.ClientID() == alice.Doc.ClientID() {
		t.Fatal("the fresh page was given the retired client id")
	}
	sendTyped(t, fresh, 0, "z")
	waitForRoomRendering(t, room, func(rendering string) bool {
		return strings.HasPrefix(rendering, "# Title\n\nz")
	}, "the rebuilt room holding the fresh page's keystroke")
	requireConnected(t, fresh, "alice's fresh page")
}

// A restart: the binding is stored, so a process that has never held the room - the process that
// replaced the one alice's page was on - gives her page its client id back, takes her offline edit
// under it, and refuses mallory's edit under it.
func TestAProcessThatNeverHeldTheRoomKeepsTheBinding(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	first, firstServer := newTestServiceInstance(t, database)
	seedServiceText(t, first, artifactID, clientIDsSeed)
	alice := openPage(t, firstServer.URL, artifactID, "alice")
	awaitAnswer(t, alice, "the document")
	sendTyped(t, alice, 0, "a")
	awaitRendering(t, func() *crdt.Doc { return first.srv.GetDoc(artifactID) }, "# Title\n\naAlice's paragraph.\n", "the first process's room")
	alice.Close()
	<-alice.Ended
	firstServer.Close()

	second, secondServer := newTestServiceInstance(t, database)
	typeAt(t, alice.Doc, 1, "b")
	back := reopenPage(t, secondServer.URL, artifactID, "alice", alice.Doc)
	awaitAnswer(t, back, "the document on the second process")
	room := func() *crdt.Doc { return second.srv.GetDoc(artifactID) }
	awaitRendering(t, room, "# Title\n\nabAlice's paragraph.\n", "the second process's room")
	mallory := openPage(t, secondServer.URL, artifactID, "mallory")
	awaitAnswer(t, mallory, "the document on the second process")
	next := mallory.Doc.StateVector().Clock(alice.Doc.ClientID())
	if err := mallory.Write(ygsync.EncodeUpdate(forgedUnder(t, mallory.Doc, "EVIL", alice.Doc.ClientID(), next))); err != nil {
		t.Fatal(err)
	}
	awaitRefused(t, mallory, "mallory")
	sendTyped(t, back, 2, "c")
	awaitRendering(t, room, "# Title\n\nabcAlice's paragraph.\n", "the second process's room")
	requireConnected(t, back, "alice")
}

// An agent writing through the API while a browser types: the write is the server's own, authored
// on a fork of the room under an id ygo draws below mintedClientFloor, so it is neither judged here
// nor stored, and no page is ever given an id it could be. The typist is never refused.
func TestAnAgentsWriteWhileABrowserTypesIsUnderAClientNoPageIsGiven(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	alice := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, alice, "the document")
	room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }
	before := room().StateVector()
	typed := make(chan struct{})
	go func() {
		defer close(typed)
		for i := range 20 {
			update := docstest.Transact(alice.Doc, func(txn *crdt.Transaction) {
				clientIDTexts(txn.GetXmlFragment(fragmentName))[1].Insert(txn, i, "k", nil)
			})
			if err := alice.Write(ygsync.EncodeUpdate(update)); err != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	for i := range 3 {
		if _, err := joinedApplyOps(service, artifactID, []model.EditOp{{
			Op: "insert", After: "end", Markdown: "Agent paragraph " + string(rune('A'+i)) + ".",
		}}, model.Actor{Kind: "session", ID: "session-0123456789abcdef"}, nil); err != nil {
			t.Fatalf("the agent's write %d: %v", i, err)
		}
	}
	<-typed
	waitForRoomRendering(t, room, func(rendering string) bool {
		return strings.Count(rendering, "k") >= 20 && strings.Contains(rendering, "Agent paragraph C.")
	}, "the room holding the typing and the agent's writes")
	requireConnected(t, alice, "alice")
	given := alice.Doc.ClientID()
	requireGiven(t, given)
	var server []crdt.ClientID
	for client := range room().StateVector() {
		if _, held := before[client]; !held && client != given {
			server = append(server, client)
		}
	}
	if len(server) == 0 {
		t.Fatal("the room holds no client of the agent's writes")
	}
	for _, client := range server {
		if client >= mintedClientFloor {
			t.Fatalf("the agent's write is under client %d, in the range pages are given", client)
		}
		if stored := storedClientIDs(t, service, artifactID, client); stored != 0 {
			t.Fatalf("the agent's write's client %d is stored as someone's (%d rows)", client, stored)
		}
	}
	t.Logf("alice's page writes under %d; the agent's writes under %v", given, server)
}

// The deploy: a tab of the build before this one presents no client id and writes under the id it
// drew for itself, below mintedClientFloor, and the history it typed before the deploy names that
// id with no row beside it. Each of its edits lands: the ones it sends when its socket comes back
// across the deploy, and the saved rows the next page of this build replays once the reader
// reloads, under the id they were typed under - one whose id the history names, and one typed
// offline from the start, whose id it does not. An id the history names is claimed only by a
// person a version or a pending author row lists since the history first named it: mallory, who
// never edited the document, is refused it before alice's tab comes back, and alice, who typed
// under it, claims it. The id the history never named is claimed by its first writer, and
// mallory's edit under a claimed id afterwards is refused.
func TestAnEarlierBuildsTabLandsItsEditsAndSavedRowsAcrossTheDeploy(t *testing.T) {
	// The service's lines are kept for the claim trace (awaitClaimRefused) and printed only with it.
	logs, start := captureLogs(t), time.Now()
	service, artifactID, serverURL := clientIDRoom(t)
	room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }
	url := socketURL(serverURL, artifactID)
	const open, reloaded, offline = crdt.ClientID(1_234_567_001), crdt.ClientID(1_234_567_002), crdt.ClientID(1_234_567_003)

	// Before the deploy: two tabs of the earlier build type, and the room stores what they typed.
	openTab := docstest.Dial(t, url, pageHeader("alice"), artifactID, crdt.New(crdt.WithClientID(open)))
	awaitAnswer(t, openTab, "the document")
	reloadedTab := docstest.Dial(t, url, pageHeader("alice"), artifactID, crdt.New(crdt.WithClientID(reloaded)))
	awaitAnswer(t, reloadedTab, "the document")
	sendTyped(t, openTab, 0, "o")
	sendTyped(t, reloadedTab, 0, "r")
	waitForRoomRendering(t, room, func(rendering string) bool {
		return strings.Contains(rendering, "o") && strings.Contains(rendering, "r")
	}, "the room holding both earlier-build tabs' edits")
	openTab.Close()
	reloadedTab.Close()
	<-openTab.Ended
	<-reloadedTab.Ended
	// The earlier build stored no binding: the history names both ids, and no row does.
	forgetStoredClientIDs(t, service, artifactID)
	// While the deploy runs, each tab types on, unsent; a third tab, offline since it opened,
	// types into its copy of the document under an id the history has never seen.
	typeAt(t, openTab.Doc, 0, "O")
	savedReloaded := typeAt(t, reloadedTab.Doc, 0, "R")
	offlineTab := crdt.New(crdt.WithClientID(offline))
	if err := crdt.ApplyUpdateV1(offlineTab, crdt.EncodeStateAsUpdateV1(reloadedTab.Doc, nil), nil); err != nil {
		t.Fatal(err)
	}
	savedOffline := typeAt(t, offlineTab, 0, "F")

	// Mallory, a reader who never edited the document, reads the open tab's id off its history and
	// claims it before the tab comes back.
	early := openPage(t, serverURL, artifactID, "mallory")
	awaitAnswer(t, early, "the document")
	if err := early.Write(ygsync.EncodeUpdate(forgedUnder(t, early.Doc, "EVIL", open, early.Doc.StateVector().Clock(open)))); err != nil {
		t.Fatal(err)
	}
	awaitClaimRefused(t, service, artifactID, open, early, logs, start, "mallory claiming the open tab's id")
	// The tab left open reconnects across the deploy, still of the earlier build.
	back := docstest.Dial(t, url, pageHeader("alice"), artifactID, openTab.Doc)
	awaitAnswer(t, back, "the document after the deploy")
	// The reader reloads the other tab: a page of this build, given an id of its own, replays the
	// rows the browser saved under the ids they were typed under.
	page := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, page, "the document in the reloaded page")
	for _, saved := range [][]byte{savedReloaded, savedOffline} {
		if err := crdt.ApplyUpdateV1(page.Doc, saved, nil); err != nil {
			t.Fatal(err)
		}
		if err := page.Write(ygsync.EncodeUpdate(saved)); err != nil {
			t.Fatal(err)
		}
	}
	waitForRoomRendering(t, room, func(rendering string) bool {
		for _, typed := range []string{"o", "r", "O", "R", "F"} {
			if !strings.Contains(rendering, typed) {
				return false
			}
		}
		return true
	}, "the room holding every edit of the earlier build's tabs")
	requireConnected(t, back, "the earlier build's tab")
	requireConnected(t, page, "the reloaded page")

	mallory := openPage(t, serverURL, artifactID, "mallory")
	awaitAnswer(t, mallory, "the document")
	next := mallory.Doc.StateVector().Clock(reloaded)
	if err := mallory.Write(ygsync.EncodeUpdate(forgedUnder(t, mallory.Doc, "EVIL", reloaded, next))); err != nil {
		t.Fatal(err)
	}
	awaitClaimRefused(t, service, artifactID, reloaded, mallory, logs, start, "mallory")
}

// awaitClaimRefused is awaitRefused for page, whose update claims client in artifactID, that says,
// when the room leaves page connected, what the claim rule read and what page and the room were
// told: the stored rows and whether each names client (historyNames), the versions and pending
// authors the rule reads for a co-author (claimable), the room's state vector, the row the table
// holds for client, what page heard and when, and the service's lines for the room since start.
// It prints nothing where the room closes page.
func awaitClaimRefused(t *testing.T, service *Service, artifactID string, client crdt.ClientID, page *docstest.Peer, logs *storetest.LockedLog, start time.Time, who string) {
	t.Helper()
	select {
	case <-page.Ended:
	case <-time.After(30 * time.Second):
		logClaimTrace(t, service, artifactID, client, page, logs, start)
		t.Fatalf("%s's page stayed connected: the room did not refuse its update", who)
	}
	awaitRefused(t, page, who)
}

// logClaimTrace logs what awaitClaimRefused says of a claim the room did not refuse. Times are
// from start, the test's own; a query that fails is logged and the rest goes on.
func logClaimTrace(t *testing.T, service *Service, artifactID string, client crdt.ClientID, page *docstest.Peer, logs *storetest.LockedLog, start time.Time) {
	t.Helper()
	ctx := context.Background()
	since := func(at time.Time) time.Duration { return at.Sub(start).Round(time.Millisecond) }
	t.Logf("claim trace at %v, test began %s", since(time.Now()), start.UTC().Format(time.RFC3339Nano))
	names, through, err := service.readHistoryClients(ctx, artifactID)
	entry, named := names[client]
	t.Logf("claim trace: the stored history, read through version %d, names client %d: %t, from version %d, to clock %d (error %v)",
		through, client, named, entry.from, entry.heldTo, err)
	if rows, err := service.store.Pool.Query(ctx, `select version, update, created_at from doc_updates where artifact_id = $1 order by version`, artifactID); err != nil {
		t.Logf("claim trace: read doc_updates: %v", err)
	} else {
		for rows.Next() {
			var version int64
			var update []byte
			var created time.Time
			if err := rows.Scan(&version, &update, &created); err != nil {
				t.Logf("claim trace: read doc_updates: %v", err)
				break
			}
			structs, ends, metaErr := crdt.ParseUpdateMeta(update)
			_, names := structs[client]
			t.Logf("claim trace: doc_updates version %d, stored %s, %d bytes: names client %d %t (clocks %d to %d; parse error %v)",
				version, created.UTC().Format(time.RFC3339Nano), len(update), client, names, structs[client], ends[client], metaErr)
		}
		rows.Close()
	}
	for _, query := range []string{
		`select 'version ' || number || ' at doc_updates version ' || coalesce(doc_update_version::text, 'none') || ', authors ' || coalesce(authors::text, 'none') from artifact_versions where artifact_id = $1 order by number`,
		`select 'pending author ' || actor::text || ' written through ' || written_through from doc_pending_authors where artifact_id = $1 order by written_through`,
	} {
		rows, err := service.store.Pool.Query(ctx, query, artifactID)
		if err != nil {
			t.Logf("claim trace: %v", err)
			continue
		}
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Logf("claim trace: %v", err)
				break
			}
			t.Logf("claim trace: %s", line)
		}
		rows.Close()
	}
	var binding string
	if err := service.store.Pool.QueryRow(ctx, `
		select actor_kind || ':' || actor_id || ' minted ' || minted || ' landed ' || landed ||
			' claimed_from ' || coalesce(claimed_from::text, 'none') || ' presented_at ' || coalesce(presented_at::text, 'never')
		from document_client_ids where artifact_id = $1 and client_id = $2
	`, artifactID, int64(client)).Scan(&binding); err != nil {
		binding = "no row (" + err.Error() + ")"
	}
	t.Logf("claim trace: document_client_ids for client %d: %s", client, binding)
	if live := service.srv.GetDoc(artifactID); live != nil {
		t.Logf("claim trace: the room's state vector: %v", live.StateVector())
	} else {
		t.Logf("claim trace: the room is not loaded")
	}
	for _, heard := range page.Heard() {
		t.Logf("claim trace: at %v the page heard %s", since(heard.At), heard.What)
	}
	lines := logs.Lines(artifactID)
	if lines == "" {
		lines = "(none)"
	}
	t.Logf("claim trace: the service's lines for the room since the test began:\n%s", lines)
}

// The race a claim leaves among co-authors loses nothing in silence. Bob, who edited the document
// after the earlier build's tab first typed in it, reads the tab's id and next clock off the
// history and writes under it first, so the claim is his, from that clock (claimed_from). The tab
// had typed at that clock already, offline, and its saved row, replayed by alice's reloaded page
// under the id it was typed under, falls wholly at clocks the room holds: the apply would land
// nothing of it and the page would be answered as if it had. It is refused with the client id
// refusal instead, and the room holds bob's text at that clock, not hers. A browser handing back
// what the room relayed under that id - carol's whole state, bob's struct in it - passes, and
// carol stays connected.
func TestAnEditAtAClockACoAuthorClaimedFirstIsRefusedNotDroppedInSilence(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }
	const tab = crdt.ClientID(1_234_567_011)

	// Before the deploy: the earlier build's tab types, and bob edits the document after it.
	oldTab := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("alice"), artifactID, crdt.New(crdt.WithClientID(tab)))
	awaitAnswer(t, oldTab, "the document")
	sendTyped(t, oldTab, 0, "o")
	waitForRoomRendering(t, room, func(rendering string) bool { return strings.Contains(rendering, "oAlice") }, "the room holding the tab's edit")
	bob := openPage(t, serverURL, artifactID, "bob")
	awaitAnswer(t, bob, "the document")
	sendTyped(t, bob, 0, "b")
	waitForRoomRendering(t, room, func(rendering string) bool { return strings.Contains(rendering, "boAlice") }, "the room holding bob's edit")
	oldTab.Close()
	<-oldTab.Ended
	// The earlier build stored no binding for the tab's id: the history names it, and no row does.
	if _, err := service.store.Pool.Exec(context.Background(),
		`delete from document_client_ids where artifact_id = $1 and client_id = $2`, artifactID, int64(tab)); err != nil {
		t.Fatal(err)
	}

	// While the deploy runs, the tab types on offline at its next clock and saves the row.
	next := oldTab.Doc.StateVector().Clock(tab)
	saved := typeAt(t, oldTab.Doc, 0, "T")
	// Bob writes under the tab's id first, at that clock.
	if err := bob.Write(ygsync.EncodeUpdate(forgedUnder(t, bob.Doc, "BOB", tab, next))); err != nil {
		t.Fatal(err)
	}
	waitForRoomRendering(t, room, func(rendering string) bool { return strings.Contains(rendering, "BOB") }, "the room holding bob's claim")
	requireConnected(t, bob, "bob")
	var claimedFrom int64
	if err := service.store.Pool.QueryRow(context.Background(),
		`select claimed_from from document_client_ids where artifact_id = $1 and client_id = $2 and actor_id = 'bob' and not minted`,
		artifactID, int64(tab)).Scan(&claimedFrom); err != nil {
		t.Fatalf("read bob's claim of the tab's id: %v", err)
	}
	if uint64(claimedFrom) != next {
		t.Fatalf("bob's claim of the tab's id starts at clock %d, want %d, the clock the room held it to", claimedFrom, next)
	}

	// Carol hands back the room's whole state, bob's struct under the tab's id in it: equal, so
	// it passes.
	carol := openPage(t, serverURL, artifactID, "carol")
	awaitAnswer(t, carol, "the document")
	sendStepTwo(t, carol, crdt.EncodeStateAsUpdateV1(carol.Doc, nil))
	requireConnected(t, carol, "carol")

	// Alice reloads: a page of this build replays the tab's saved row under the id it was typed
	// under, at the clock bob's claim took.
	before := mustRendering(t, room())
	page := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, page, "the document in the reloaded page")
	if err := page.Write(ygsync.EncodeUpdate(saved)); err != nil {
		t.Fatal(err)
	}
	awaitRefused(t, page, "alice's reloaded page replaying the tab's saved row")
	if after := mustRendering(t, room()); after != before {
		t.Fatalf("the room renders %q after the refused row, want %q as before it", after, before)
	}
	if clock := room().StateVector().Clock(tab); clock != next+uint64(len("BOB")) {
		t.Fatalf("the room holds the tab's id to clock %d, want bob's claim's end %d", clock, next+uint64(len("BOB")))
	}
}

// Once the claim window has closed a legacy socket claims an id no history names whenever it
// writes, and every forgery the rule refused before is refused still. Mallory, through a legacy
// socket of her own each time, writes at the next clock of: the id alice's page of this build was
// given; an id at 2^32 or above that no admission gave out; the id alice's legacy tab claimed; and
// the id of an earlier-build tab of alice's that the history names and no row does.
func TestALegacySocketsForgeriesAreRefusedAfterTheWindowAsBefore(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	setClientIDsSince(t, service, time.Now().Add(-31*24*time.Hour))
	room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }
	given := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, given, "the document")
	sendTyped(t, given, 0, "g")
	const claimed, unbound = crdt.ClientID(1_234_567_701), crdt.ClientID(1_234_567_702)
	tab := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("alice"), artifactID, crdt.New(crdt.WithClientID(claimed)))
	awaitAnswer(t, tab, "the document")
	sendTyped(t, tab, 0, "c")
	earlier := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("alice"), artifactID, crdt.New(crdt.WithClientID(unbound)))
	awaitAnswer(t, earlier, "the document")
	sendTyped(t, earlier, 0, "e")
	awaitHeld(t, room, []crdt.ClientID{given.Doc.ClientID(), claimed, unbound}, "alice's three edits")
	earlier.Close()
	<-earlier.Ended
	// The earlier build stored no binding for that tab's id: the history names it, and no row does.
	if _, err := service.store.Pool.Exec(context.Background(),
		`delete from document_client_ids where artifact_id = $1 and client_id = $2`, artifactID, int64(unbound)); err != nil {
		t.Fatal(err)
	}
	for _, forged := range []struct {
		name   string
		client crdt.ClientID
	}{
		{name: "the id alice's page was given", client: given.Doc.ClientID()},
		{name: "an id at 2^32 or above no admission gave out", client: mintedClientFloor + 1_234_567},
		{name: "the id alice's legacy tab claimed", client: claimed},
		{name: "the id the history names and no row does", client: unbound},
	} {
		mallory := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("mallory"), artifactID, crdt.New(crdt.WithClientID(1_234_567_799)))
		awaitAnswer(t, mallory, "the document")
		next := mallory.Doc.StateVector().Clock(forged.client)
		if err := mallory.Write(ygsync.EncodeUpdate(forgedUnder(t, mallory.Doc, "EVIL", forged.client, next))); err != nil {
			t.Fatal(err)
		}
		awaitRefused(t, mallory, "mallory writing under "+forged.name)
		if clock := room().StateVector().Clock(forged.client); clock != next {
			t.Fatalf("the room holds %s to clock %d after mallory's edit, want %d: the forged struct landed", forged.name, clock, next)
		}
	}
	requireConnected(t, given, "alice's page")
	requireConnected(t, tab, "alice's legacy tab")
}

// Once the claim window has closed, a forger who reads a legacy tab's id off the room's awareness
// - a page of the earlier build announces its presence under the id it drew before it types - and
// claims it first, through a legacy socket of her own, at the clocks the tab writes next, does not
// take the tab's keystroke in silence: the keystroke, in flight as the forger's edit landed, falls
// wholly at clocks the room holds and differs from what it holds there, and the tab is refused
// with the client id refusal instead of answered as if the room had taken it.
func TestAForgerWhoClaimsALegacyTabsIDFirstAfterTheWindowHasTheTabRefusedNotDropped(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	setClientIDsSince(t, service, time.Now().Add(-31*24*time.Hour))
	room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }
	const tabClient = crdt.ClientID(1_234_567_801)
	tab := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("alice"), artifactID, crdt.New(crdt.WithClientID(tabClient)))
	awaitAnswer(t, tab, "the document")
	mallory := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("mallory"), artifactID, crdt.New(crdt.WithClientID(1_234_567_802)))
	awaitAnswer(t, mallory, "the document")
	if err := tab.SetAwareness(uint64(tabClient), map[string]any{"user": map[string]string{"name": "alice"}}); err != nil {
		t.Fatal(err)
	}
	if err := mallory.WaitForAwareness(30*time.Second, func(states map[uint64]json.RawMessage) bool {
		return len(states[uint64(tabClient)]) > 0
	}); err != nil {
		t.Fatalf("mallory never read the tab's client id off the room's awareness: %v", err)
	}
	// The tab types its first keystroke, whose frame is in flight as mallory's arrives.
	inFlight := typeAt(t, tab.Doc, 0, "a")
	if err := mallory.Write(ygsync.EncodeUpdate(forgedUnder(t, mallory.Doc, "EVIL", tabClient, 0))); err != nil {
		t.Fatal(err)
	}
	waitForRoomRendering(t, room, func(rendering string) bool { return strings.Contains(rendering, "EVIL") }, "the room holding mallory's claim")
	requireConnected(t, mallory, "mallory, whose claim of an id no history names stands")
	if err := tab.Write(ygsync.EncodeUpdate(inFlight)); err != nil {
		t.Fatal(err)
	}
	awaitRefused(t, tab, "the legacy tab whose keystroke fell at the clocks mallory claimed")
	if clock := room().StateVector().Clock(tabClient); clock != uint64(len("EVIL")) {
		t.Fatalf("the room holds the tab's id to clock %d, want mallory's claim's end %d", clock, len("EVIL"))
	}
}

// The same forger, and a tab whose keystroke never left on the socket mallory read its id off: the
// tab's socket drops with the keystroke queued, mallory claims the id meanwhile, and the tab's
// first frame on its reconnect, before it announces anything, is the queued keystroke, at the
// clocks mallory claimed. It is refused with the client id refusal, not dropped as a duplicate.
func TestAForgerWhoClaimsALegacyTabsIDFirstHasTheTabsQueuedKeystrokeRefusedOnItsReconnect(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	setClientIDsSince(t, service, time.Now().Add(-31*24*time.Hour))
	room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }
	const tabClient = crdt.ClientID(1_234_568_001)
	tab := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("alice"), artifactID, crdt.New(crdt.WithClientID(tabClient)))
	awaitAnswer(t, tab, "the document")
	mallory := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("mallory"), artifactID, crdt.New(crdt.WithClientID(1_234_568_002)))
	awaitAnswer(t, mallory, "the document")
	if err := tab.SetAwareness(uint64(tabClient), map[string]any{"user": map[string]string{"name": "alice"}}); err != nil {
		t.Fatal(err)
	}
	if err := mallory.WaitForAwareness(30*time.Second, func(states map[uint64]json.RawMessage) bool {
		return len(states[uint64(tabClient)]) > 0
	}); err != nil {
		t.Fatalf("mallory never read the tab's client id off the room's awareness: %v", err)
	}
	// The tab types, and its socket drops before the keystroke is sent: the provider queues it.
	queued := typeAt(t, tab.Doc, 0, "a")
	tab.Close()
	<-tab.Ended
	if err := mallory.Write(ygsync.EncodeUpdate(forgedUnder(t, mallory.Doc, "EVIL", tabClient, 0))); err != nil {
		t.Fatal(err)
	}
	waitForRoomRendering(t, room, func(rendering string) bool { return strings.Contains(rendering, "EVIL") }, "the room holding mallory's claim")
	back := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("alice"), artifactID, tab.Doc)
	awaitAnswer(t, back, "the document after the tab's reconnect")
	if err := back.Write(ygsync.EncodeUpdate(queued)); err != nil {
		t.Fatal(err)
	}
	awaitRefused(t, back, "the reconnected tab whose queued keystroke fell at the clocks mallory claimed")
	if clock := room().StateVector().Clock(tabClient); clock != uint64(len("EVIL")) {
		t.Fatalf("the room holds the tab's id to clock %d, want mallory's claim's end %d", clock, len("EVIL"))
	}
}

// The deploy's race after day 30: bob, a co-author, claims an earlier-build tab's id at its next
// clock while the window is open, and the tab's saved row at that clock is replayed by alice's
// reloaded page of this build only once the window has closed. The replay falls wholly at clocks
// the room holds and differs from bob's text there: it is refused with the client id refusal, as
// inside the window, rather than dropped as a duplicate.
func TestAPageReplayingAnOldTabsSavedRowAfterTheWindowAtClocksClaimedInsideItIsRefused(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }
	const tab = crdt.ClientID(1_234_568_101)
	oldTab := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("alice"), artifactID, crdt.New(crdt.WithClientID(tab)))
	awaitAnswer(t, oldTab, "the document")
	sendTyped(t, oldTab, 0, "o")
	waitForRoomRendering(t, room, func(rendering string) bool { return strings.Contains(rendering, "oAlice") }, "the room holding the tab's edit")
	bob := openPage(t, serverURL, artifactID, "bob")
	awaitAnswer(t, bob, "the document")
	sendTyped(t, bob, 0, "b")
	waitForRoomRendering(t, room, func(rendering string) bool { return strings.Contains(rendering, "boAlice") }, "the room holding bob's edit")
	oldTab.Close()
	<-oldTab.Ended
	if _, err := service.store.Pool.Exec(context.Background(),
		`delete from document_client_ids where artifact_id = $1 and client_id = $2`, artifactID, int64(tab)); err != nil {
		t.Fatal(err)
	}
	next := oldTab.Doc.StateVector().Clock(tab)
	saved := typeAt(t, oldTab.Doc, 0, "T")
	// Inside the window, bob writes under the tab's id first, at that clock.
	if err := bob.Write(ygsync.EncodeUpdate(forgedUnder(t, bob.Doc, "BOB", tab, next))); err != nil {
		t.Fatal(err)
	}
	waitForRoomRendering(t, room, func(rendering string) bool { return strings.Contains(rendering, "BOB") }, "the room holding bob's claim")
	requireConnected(t, bob, "bob")
	// Day 31: alice reloads, and her page replays the tab's saved row.
	setClientIDsSince(t, service, time.Now().Add(-31*24*time.Hour))
	before := mustRendering(t, room())
	page := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, page, "the document in the reloaded page")
	if err := page.Write(ygsync.EncodeUpdate(saved)); err != nil {
		t.Fatal(err)
	}
	awaitRefused(t, page, "alice's reloaded page replaying the tab's saved row after the window")
	if after := mustRendering(t, room()); after != before {
		t.Fatalf("the room renders %q after the refused row, want %q as before it", after, before)
	}
}

// mergedUpdate is updates merged into one, as a whole-state handoff carries them.
func mergedUpdate(t *testing.T, updates [][]byte) []byte {
	t.Helper()
	merged, err := crdt.MergeUpdatesV1(updates...)
	if err != nil {
		t.Fatal(err)
	}
	return merged
}

// An update is compared only where it carries structs at clocks the room holds under at most
// maxComparedClaims ids other people claimed. Bob's legacy socket claims five ids. A page of this
// build sending structs at their clocks that differ from bob's is refused for four of the ids, as
// a resend under them is compared, and not for all five, which is a whole-state handoff of the
// history and is neither compared nor refused for it. A legacy socket sending bob's own structs
// back is let through for four ids, equal as they are, and refused whole for five: an honest
// legacy tab writes under one id.
func TestAnUpdateUnderMoreClaimedIDsThanTheComparedFewIsNotComparedOrIsRefusedFromALegacySocket(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }
	bob := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("bob"), artifactID, crdt.New(crdt.WithClientID(1_234_568_200)))
	awaitAnswer(t, bob, "the document")
	claimed := make([]crdt.ClientID, maxComparedClaims+1)
	bobs := make([][]byte, len(claimed))
	for i := range claimed {
		claimed[i] = crdt.ClientID(1_234_568_201 + i)
		bobs[i] = forgedUnder(t, bob.Doc, "x", claimed[i], 0)
		if err := bob.Write(ygsync.EncodeUpdate(bobs[i])); err != nil {
			t.Fatal(err)
		}
	}
	awaitHeld(t, room, claimed, "bob's claims")
	before := mustRendering(t, room())
	differing := func(page *docstest.Peer, ids []crdt.ClientID) []byte {
		updates := make([][]byte, len(ids))
		for i, id := range ids {
			updates[i] = forgedUnder(t, page.Doc, "y", id, 0)
		}
		return mergedUpdate(t, updates)
	}
	handoff := openPage(t, serverURL, artifactID, "carol")
	awaitAnswer(t, handoff, "the document")
	if err := handoff.Write(ygsync.EncodeUpdate(differing(handoff, claimed))); err != nil {
		t.Fatal(err)
	}
	requireConnected(t, handoff, "carol's page sending structs under five claimed ids")
	resend := openPage(t, serverURL, artifactID, "carol")
	awaitAnswer(t, resend, "the document")
	if err := resend.Write(ygsync.EncodeUpdate(differing(resend, claimed[:maxComparedClaims]))); err != nil {
		t.Fatal(err)
	}
	awaitRefused(t, resend, "carol's page sending differing structs under four claimed ids")
	few := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("dave"), artifactID, crdt.New(crdt.WithClientID(1_234_568_300)))
	awaitAnswer(t, few, "the document")
	if err := few.Write(ygsync.EncodeUpdate(mergedUpdate(t, bobs[:maxComparedClaims]))); err != nil {
		t.Fatal(err)
	}
	requireConnected(t, few, "dave's legacy socket handing back bob's structs under four ids")
	many := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("dave"), artifactID, crdt.New(crdt.WithClientID(1_234_568_301)))
	awaitAnswer(t, many, "the document")
	if err := many.Write(ygsync.EncodeUpdate(mergedUpdate(t, bobs))); err != nil {
		t.Fatal(err)
	}
	awaitRefused(t, many, "dave's legacy socket handing back bob's structs under five ids")
	if after := mustRendering(t, room()); after != before {
		t.Fatalf("the room renders %q after the duplicates, want %q as before them", after, before)
	}
}

// Once the claim window has closed, a legacy tab whose socket dropped before the room answered its
// keystroke sends the keystroke again on its reconnect: its structs under the id it claimed all
// fall at clocks the room holds, and the new socket has shown nothing of that id yet, so they are
// looked up and compared, and pass, the claim being the tab's own person's. The tab types on, and a
// page of this build handing the room back its whole state, the tab's structs in it, passes too.
func TestALegacyTabResendingItsOwnClaimedKeystrokeAfterTheWindowIsNotRefused(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	setClientIDsSince(t, service, time.Now().Add(-31*24*time.Hour))
	room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }
	doc := crdt.New(crdt.WithClientID(1_234_567_901))
	tab := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("alice"), artifactID, doc)
	awaitAnswer(t, tab, "the document")
	keystroke := typeAt(t, doc, 0, "a")
	if err := tab.Write(ygsync.EncodeUpdate(keystroke)); err != nil {
		t.Fatal(err)
	}
	awaitRendering(t, room, "# Title\n\naAlice's paragraph.\n", "the room")
	tab.Close()
	<-tab.Ended
	back := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("alice"), artifactID, doc)
	awaitAnswer(t, back, "the document after the tab's reconnect")
	if err := back.Write(ygsync.EncodeUpdate(keystroke)); err != nil {
		t.Fatal(err)
	}
	sendTyped(t, back, 1, "b")
	awaitRendering(t, room, "# Title\n\nabAlice's paragraph.\n", "the room after the tab's next keystroke")
	carol := openPage(t, serverURL, artifactID, "carol")
	awaitAnswer(t, carol, "the document")
	sendStepTwo(t, carol, crdt.EncodeStateAsUpdateV1(carol.Doc, nil))
	requireConnected(t, back, "the reconnected tab")
	requireConnected(t, carol, "carol")
}

// sendStepTwo sends the room update as a sync step 2, the whole-state answer a provider gives the
// room's sync step 1.
func sendStepTwo(t *testing.T, page *docstest.Peer, update []byte) {
	t.Helper()
	if err := page.Write(encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarUint(ygsync.MsgSyncStep2)
		encoder.WriteVarBytes(update)
	})); err != nil {
		t.Fatal(err)
	}
}

// A forger's sync step 2 carrying structs under alice's client id beside the forger's own: the
// structs under alice's id are cut out of it before the apply and the room holds none of them,
// the forger's own land, the forger is not closed for it, and alice's next keystroke lands where
// she typed it.
func TestAForgersSyncStep2HasAnotherPersonsStructsCutAndItsOwnLand(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	alice := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, alice, "the document")
	mallory := openPage(t, serverURL, artifactID, "mallory")
	awaitAnswer(t, mallory, "the document")
	victim := alice.Doc.ClientID()
	sendTyped(t, alice, 0, "a")
	room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }
	awaitRendering(t, func() *crdt.Doc { return mallory.Doc }, "# Title\n\naAlice's paragraph.\n", "mallory's page")
	next := mallory.Doc.StateVector().Clock(victim)
	forged := forgedUnder(t, mallory.Doc, "EVIL", victim, next)
	own := typeAt(t, mallory.Doc, 0, "M")
	answer, err := crdt.MergeUpdatesV1(forged, own)
	if err != nil {
		t.Fatal(err)
	}
	sendStepTwo(t, mallory, answer)
	waitForRoomRendering(t, room, func(rendering string) bool {
		return strings.Contains(rendering, "M")
	}, "the room holding the forger's own struct")
	if rendering := mustRendering(t, room()); strings.Contains(rendering, "EVIL") {
		t.Fatalf("the room holds the struct forged under alice's id: %q", rendering)
	}
	if clock := room().StateVector().Clock(victim); clock != next {
		t.Fatalf("the room holds alice's client to clock %d, want %d", clock, next)
	}
	requireConnected(t, mallory, "mallory")
	awaitRendering(t, func() *crdt.Doc { return alice.Doc }, "# Title\n\nMaAlice's paragraph.\n", "alice's page")
	sendTyped(t, alice, 2, "b")
	awaitRendering(t, room, "# Title\n\nMabAlice's paragraph.\n", "the room after alice's next keystroke")
	requireConnected(t, alice, "alice")
}

// Bob holds an edit of alice's the room relayed and then lost, and reconnects: his sync step 2
// carries alice's lost structs beside his own. Alice's are cut, so the room holds none of them,
// bob is not closed, and his own edit that builds on nothing of hers lands; his edit typed after
// her lost text names an item the room does not hold and parks.
func TestASyncStep2CarryingAnotherPersonsLostEditLandsTheSendersOwnAndCutsHers(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	alice := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, alice, "the document")
	bob := openPage(t, serverURL, artifactID, "bob")
	awaitAnswer(t, bob, "the document")
	sendTyped(t, alice, 0, "a")
	room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }
	awaitRendering(t, func() *crdt.Doc { return bob.Doc }, "# Title\n\naAlice's paragraph.\n", "bob's page")
	bob.Close()
	<-bob.Ended
	// The room relays alice's "L" to bob and then loses it: bob holds it, the room does not.
	lost := typeAt(t, alice.Doc, 1, "L")
	if err := crdt.ApplyUpdateV1(bob.Doc, lost, nil); err != nil {
		t.Fatal(err)
	}
	typeAt(t, bob.Doc, 0, "B") // builds on nothing of alice's lost edit
	typeAt(t, bob.Doc, 3, "D") // typed right after alice's lost "L"
	back := reopenPage(t, serverURL, artifactID, "bob", bob.Doc)
	awaitAnswer(t, back, "the document after bob's reconnect")
	waitForRoomRendering(t, room, func(rendering string) bool {
		return strings.Contains(rendering, "B")
	}, "the room holding bob's own edit")
	requireConnected(t, back, "bob")
	rendering := mustRendering(t, room())
	if strings.Contains(rendering, "L") || strings.Contains(rendering, "D") {
		t.Fatalf("the room renders %q: alice's lost edit, or bob's edit built on it, landed", rendering)
	}
	t.Logf("the room renders %q and parks %+v: bob's edit built on alice's lost one waits for it", rendering, room().PendingStats())
}

// mustRendering is doc's rendering, failing the test where it does not render.
func mustRendering(t *testing.T, doc *crdt.Doc) string {
	t.Helper()
	rendering, err := renderDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	return rendering
}

// waitForRoomRendering waits for room's rendering to satisfy holds.
func waitForRoomRendering(t *testing.T, room func() *crdt.Doc, holds func(string) bool, what string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		if doc := room(); doc != nil {
			got, _ = renderDocument(doc)
			if holds(got) {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("waited for %s; it renders %q", what, got)
}
