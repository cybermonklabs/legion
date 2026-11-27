package docs

// Review 10 (R9Regressions): finding 6's approved rule driven as a race of two people, and the
// legacy socket's refusal. Each asserts the rule, so it fails where the rule does not hold.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
)

// r9regRequireAnswered requires page to have heard a SyncStatus 1 since it began to keep what it
// heard (docstest.Peer.Hear) and no SyncStatus 0.
func r9regRequireAnswered(t *testing.T, page *docstest.Peer, who string) {
	t.Helper()
	heard := awaitHeardStatus(t, page, 1, who)
	for _, h := range heard {
		if strings.HasPrefix(h, "SyncStatus 0") {
			t.Fatalf("%s was answered not applied: %q", who, heard)
		}
	}
	if !strings.HasPrefix(heard[0], "SyncStatus 1") {
		t.Fatalf("%s heard %q, want SyncStatus 1", who, heard)
	}
}

// r9regStoredHolds waits for the stored document to render want.
func r9regStoredHolds(t *testing.T, service *Service, artifactID, want string) bool {
	t.Helper()
	return lcWait(t, "the store to hold "+want, 15*time.Second, func() bool {
		loaded, err := NewPgVersioned(service.store).Load(context.Background(), artifactID)
		if err != nil {
			return false
		}
		md, err := renderDocument(copyOf(t, loaded.Update))
		return err == nil && strings.Contains(md, want)
	})
}

// Two people send an edit under one freed id at once, with no lock held: alice's page replays her
// closed tab's saved edit under the tab's id, whose row is gone, and mallory's sends an edit forged
// under that id at clock 0. Exactly one gets the id: that page's edit is answered applied, lands,
// is stored, and the row names its sender and says it landed; the other page is answered not
// applied and closed 4413 DOC_CLIENT_ID_REFUSED, and nothing of its edit lands.
func TestR9RegTwoPeopleRaceForAFreedID(t *testing.T) {
	for i, order := range []string{"together", "alice first", "mallory first", "together", "together"} {
		t.Run(fmt.Sprintf("%d %s", i, order), func(t *testing.T) {
			service, artifactID, serverURL := clientIDRoom(t)
			room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }
			freed, saved := closedTabsSavedEdit(t, service, artifactID, serverURL, "alice", "SAVED ")
			alice := openPage(t, serverURL, artifactID, "alice")
			awaitAnswer(t, alice, "alice's next page")
			mallory := openPage(t, serverURL, artifactID, "mallory")
			awaitAnswer(t, mallory, "mallory's page")
			hearFromNow(t, alice, "alice's next page")
			hearFromNow(t, mallory, "mallory's page")
			forged := forgedUnder(t, mallory.Doc, "EVIL ", freed, 0)
			if err := crdt.ApplyUpdateV1(alice.Doc, saved, nil); err != nil {
				t.Fatal(err)
			}
			var aliceDelay, malloryDelay time.Duration
			switch order {
			case "alice first":
				malloryDelay = 300 * time.Millisecond
			case "mallory first":
				aliceDelay = 300 * time.Millisecond
			}
			var start, done sync.WaitGroup
			start.Add(1)
			send := func(page *docstest.Peer, frame []byte, delay time.Duration) {
				done.Add(1)
				go func() {
					defer done.Done()
					start.Wait()
					time.Sleep(delay)
					if err := page.Write(ygsync.EncodeUpdate(frame)); err != nil {
						t.Error(err)
					}
				}()
			}
			send(alice, saved, aliceDelay)
			send(mallory, forged, malloryDelay)
			start.Done()
			done.Wait()

			pages := map[string]*docstest.Peer{"alice": alice, "mallory": mallory}
			texts := map[string]string{"alice": "SAVED ", "mallory": "EVIL "}
			var loser string
			select {
			case <-alice.Ended:
				loser = "alice"
			case <-mallory.Ended:
				loser = "mallory"
			case <-time.After(30 * time.Second):
				t.Fatalf("neither page was refused: both edits under id %d stood; room renders %q", freed, renderedBy(t, room()))
			}
			winner := map[string]string{"alice": "mallory", "mallory": "alice"}[loser]
			t.Logf("order %s: %s's page got id %d, %s's was closed %v", order, winner, freed, loser, pages[loser].Closed())
			if closed := pages[loser].Closed(); closed == nil || closed.Code != contracts.DocumentTooLargeCloseCode || closed.Text != clientIDRefusedReason {
				t.Fatalf("the room closed %s's page with %v, want %d %s", loser, closed, contracts.DocumentTooLargeCloseCode, clientIDRefusedReason)
			}
			requireNothingApplied(t, pages[loser], loser+"'s refused page")
			requireConnected(t, pages[winner], winner+"'s page")
			r9regRequireAnswered(t, pages[winner], winner+"'s page")
			waitForRoomRendering(t, room, func(rendering string) bool { return strings.Contains(rendering, texts[winner]) }, "the room holding "+winner+"'s edit")
			if rendering := renderedBy(t, room()); strings.Contains(rendering, texts[loser]) {
				t.Fatalf("the room renders %q, which holds %s's refused edit", rendering, loser)
			}
			if clock := room().StateVector().Clock(freed); clock != uint64(len(texts[winner])) {
				t.Fatalf("the room holds id %d to clock %d, want the %d of %s's edit alone", freed, clock, len(texts[winner]), winner)
			}
			if owner := clientIDOwner(t, service, artifactID, freed); owner != winner {
				t.Fatalf("the row of id %d names %q, want %s", freed, owner, winner)
			}
			awaitLanded(t, service, artifactID, freed, "the row of the id "+winner+" got")
			if !r9regStoredHolds(t, service, artifactID, texts[winner]) {
				t.Fatalf("%s's edit, answered applied, is not stored", winner)
			}
			if order == "mallory first" && winner != "mallory" || order == "alice first" && winner != "alice" {
				t.Errorf("the page whose edit went %s was not the one given the id: %s was", order, winner)
			}
		})
	}
}

// Two people present one freed id at once as their page's admission parameter: alice's page
// presents her closed tab's id, whose row is gone, and mallory's presents the same id. Exactly one
// is given it, and the other is given another id; one row names the id.
func TestR9RegTwoPeoplePresentAFreedIDAtAdmissionAtOnce(t *testing.T) {
	for i := range 4 {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			service, artifactID, serverURL := clientIDRoom(t)
			freed, _ := closedTabsSavedEdit(t, service, artifactID, serverURL, "alice", "SAVED ")
			url := socketURL(serverURL, artifactID) + "&" + contracts.DocumentClientIdParameter + "=" + strconvU(uint64(freed))
			var given sync.Map
			var wg sync.WaitGroup
			for _, who := range []string{"alice", "mallory"} {
				wg.Add(1)
				go func() {
					defer wg.Done()
					client, busy := openAndLeave(t, url, who)
					given.Store(who, [2]any{client, busy})
				}()
			}
			wg.Wait()
			a, _ := given.Load("alice")
			m, _ := given.Load("mallory")
			ac, mc := a.([2]any)[0].(crdt.ClientID), m.([2]any)[0].(crdt.ClientID)
			t.Logf("alice was given %d (busy %v), mallory %d (busy %v); freed %d; the row names %q",
				ac, a.([2]any)[1], mc, m.([2]any)[1], freed, clientIDOwner(t, service, artifactID, freed))
			if (ac == freed) == (mc == freed) {
				t.Fatalf("alice was given %d and mallory %d for the freed id %d: want exactly one given it", ac, mc, freed)
			}
			want := map[bool]string{true: "alice", false: "mallory"}[ac == freed]
			if owner := clientIDOwner(t, service, artifactID, freed); owner != want {
				t.Fatalf("the row of id %d names %q, want %s", freed, owner, want)
			}
		})
	}
}

// Two of alice's pages replay her closed tab's saved edit under its freed id at once: neither is
// refused, the room holds the edit once, and the row names alice and says it landed.
func TestR9RegTwoOfOnePersonsPagesReplayAFreedIDAtOnce(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }
	freed, saved := closedTabsSavedEdit(t, service, artifactID, serverURL, "alice", "SAVED ")
	one := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, one, "alice's page one")
	two := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, two, "alice's page two")
	hearFromNow(t, one, "alice's page one")
	hearFromNow(t, two, "alice's page two")
	for _, page := range []*docstest.Peer{one, two} {
		if err := crdt.ApplyUpdateV1(page.Doc, saved, nil); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for _, page := range []*docstest.Peer{one, two} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := page.Write(ygsync.EncodeUpdate(saved)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	r9regRequireAnswered(t, one, "alice's page one")
	r9regRequireAnswered(t, two, "alice's page two")
	requireConnected(t, one, "alice's page one")
	requireConnected(t, two, "alice's page two")
	waitForRoomRendering(t, room, func(rendering string) bool { return strings.Count(rendering, "SAVED ") == 1 }, "the room holding alice's edit once")
	if owner := clientIDOwner(t, service, artifactID, freed); owner != "alice" {
		t.Fatalf("the row of id %d names %q, want alice", freed, owner)
	}
	awaitLanded(t, service, artifactID, freed, "the row of the id alice's pages replayed")
}

// A legacy socket of alice's (no client_id parameter) sending her closed tab's saved edit under
// its freed id is refused as before: answered not applied, closed 4413 DOC_CLIENT_ID_REFUSED, the
// room holds nothing under the id and no row is stored. A legacy socket of mallory's sending under
// the id once alice's page has been minted it is refused too.
func TestR9RegALegacySocketKeepsItsRefusalOfAFreedID(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }
	freed, saved := closedTabsSavedEdit(t, service, artifactID, serverURL, "alice", "offline ")

	legacy := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("alice"), artifactID, crdt.New(crdt.WithClientID(1_234_569_011)))
	awaitAnswer(t, legacy, "alice's legacy socket")
	hearFromNow(t, legacy, "alice's legacy socket")
	if err := legacy.Write(ygsync.EncodeUpdate(saved)); err != nil {
		t.Fatal(err)
	}
	awaitRefused(t, legacy, "alice's legacy socket")
	requireNothingApplied(t, legacy, "alice's legacy socket")
	if clock := room().StateVector().Clock(freed); clock != 0 {
		t.Fatalf("the room holds id %d to clock %d after the legacy refusal", freed, clock)
	}
	if rows := storedClientIDs(t, service, artifactID, freed); rows != 0 {
		t.Fatalf("%d rows name id %d after the legacy refusal", rows, freed)
	}

	page := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, page, "alice's page")
	if err := crdt.ApplyUpdateV1(page.Doc, saved, nil); err != nil {
		t.Fatal(err)
	}
	if err := page.Write(ygsync.EncodeUpdate(saved)); err != nil {
		t.Fatal(err)
	}
	waitForRoomRendering(t, room, func(rendering string) bool { return strings.Contains(rendering, "offline ") }, "the room holding alice's replay")
	awaitLanded(t, service, artifactID, freed, "alice's replayed id")

	mal := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("mallory"), artifactID, crdt.New(crdt.WithClientID(1_234_569_012)))
	awaitAnswer(t, mal, "mallory's legacy socket")
	hearFromNow(t, mal, "mallory's legacy socket")
	forged := forgedUnder(t, mal.Doc, "EVIL ", freed, room().StateVector().Clock(freed))
	if err := mal.Write(ygsync.EncodeUpdate(forged)); err != nil {
		t.Fatal(err)
	}
	awaitRefused(t, mal, "mallory's legacy socket")
	requireNothingApplied(t, mal, "mallory's legacy socket")
	if rendering := renderedBy(t, room()); strings.Contains(rendering, "EVIL") {
		t.Fatalf("the room renders %q with mallory's refused edit", rendering)
	}
}

// Finding 6's rule, the second person of two: mallory, who read alice's closed tab's id off its
// presence, sends first, once the tab's row is gone, an edit forged under that id at clock 0 that
// runs 40 clocks. The judgement mints her the id and the edit lands. Alice's next page then
// replays the closed tab's saved edit, 6 clocks under the same id, as the dashboard does. The rule
// says the second is refused with a true notice: 4413 DOC_CLIENT_ID_REFUSED, answered not
// applied, so the dashboard tells her the edit was not saved. Her structs fall wholly at clocks
// the room holds, mallory's, so the judgement asks nothing about the id (beyondHeld), and compares
// them with the room's from clock 0, since the row names another person (heldDuplicates,
// refusedDuplicates). Before, heldDuplicates took ids below 2^32 only, so nothing compared them,
// and the apply dropped them as duplicates.
func TestR9RegASecondPersonsReplayAtClocksTheFirstWroteIsRefusedNotSwallowed(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }
	freed, saved := closedTabsSavedEdit(t, service, artifactID, serverURL, "alice", "SAVED ")

	mallory := openPage(t, serverURL, artifactID, "mallory")
	awaitAnswer(t, mallory, "mallory's page")
	hearFromNow(t, mallory, "mallory's page")
	forged := forgedUnder(t, mallory.Doc, strings.Repeat("EVIL ", 8), freed, 0)
	if err := mallory.Write(ygsync.EncodeUpdate(forged)); err != nil {
		t.Fatal(err)
	}
	r9regRequireAnswered(t, mallory, "mallory's forged edit")
	waitForRoomRendering(t, room, func(rendering string) bool { return strings.Contains(rendering, "EVIL ") }, "the room holding mallory's edit")
	if owner := clientIDOwner(t, service, artifactID, freed); owner != "mallory" {
		t.Fatalf("the row of id %d names %q after mallory's edit, want mallory", freed, owner)
	}

	alice := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, alice, "alice's next page")
	hearFromNow(t, alice, "alice's next page")
	if err := crdt.ApplyUpdateV1(alice.Doc, saved, nil); err != nil {
		t.Fatal(err)
	}
	if err := alice.Write(ygsync.EncodeUpdate(saved)); err != nil {
		t.Fatal(err)
	}
	heard := awaitHeardStatus(t, alice, 1, "alice's replay")
	select {
	case <-alice.Ended:
	case <-time.After(3 * time.Second):
	}
	rendering := renderedBy(t, room())
	t.Logf("alice's replay of her closed tab's saved edit under id %d (room clock %d, row names %q) was answered %q; her page ended: %s; the room renders %q",
		freed, room().StateVector().Clock(freed), clientIDOwner(t, service, artifactID, freed), heard, connectionEnd(alice), rendering)
	if strings.Contains(rendering, "SAVED") {
		return // her edit landed: nothing lost
	}
	if end := connectionEnd(alice); !strings.Contains(end, clientIDRefusedReason) {
		t.Fatalf("alice's replayed edit is nowhere in the room, and her page was answered %q and is %s, not closed %d %s: the dashboard clears her saved row on that answer (pending-sync.ts clearUpdate), so her offline edit is lost in silence",
			heard, end, contracts.DocumentTooLargeCloseCode, clientIDRefusedReason)
	}
	requireNothingApplied(t, alice, "alice's refused replay")
}
