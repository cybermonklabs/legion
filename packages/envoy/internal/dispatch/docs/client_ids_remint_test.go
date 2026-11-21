package docs

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// These tests hold the judgement's mint of an id from 2^32 up that no row names and the document
// holds nothing under (judgeClients, mintForSender): the id a closed tab of the person's was given,
// whose row a give-back or the sweep took, and whose saved edits another page of the browser
// replays.

// closedTabsSavedEdit opens a page of user's, closes it, types text on its document as the closed
// tab's editor saves an offline edit, and takes the tab's row away as a give-back or the sweep
// does (forgetStoredClientID). It returns the tab's id and the saved edit, which no row names and
// the document holds nothing of.
func closedTabsSavedEdit(t *testing.T, service *Service, artifactID, serverURL, user, text string) (crdt.ClientID, []byte) {
	t.Helper()
	tab := openPage(t, serverURL, artifactID, user)
	awaitAnswer(t, tab, user+"'s tab")
	tab.Close()
	<-tab.Ended
	saved := typeAt(t, tab.Doc, 0, text)
	forgetStoredClientID(t, service, artifactID, tab.Given)
	return tab.Given, saved
}

// The mint is a page of this build's. alice's closed tab's saved edit, under its id whose row is
// gone, is sent first through a legacy socket of hers, which presents no client id: it is refused
// 4413 DOC_CLIENT_ID_REFUSED, nothing of it lands and no row is stored, since no tab of the
// earlier build held an id from 2^32 up. The same frame from a page of hers that presented a
// client id is minted the id: it lands, the row names alice, and it is marked landed, since the
// history names the id from then on.
func TestAnUnboundIDIsMintedForAPageThatPresentedOneAndRefusedFromALegacySocket(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }
	freed, saved := closedTabsSavedEdit(t, service, artifactID, serverURL, "alice", "offline ")

	legacy := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("alice"), artifactID, crdt.New(crdt.WithClientID(1_234_569_001)))
	awaitAnswer(t, legacy, "alice's legacy socket")
	if err := legacy.Write(ygsync.EncodeUpdate(saved)); err != nil {
		t.Fatal(err)
	}
	awaitRefused(t, legacy, "alice's legacy socket sending the closed tab's saved edit")
	if clock := room().StateVector().Clock(freed); clock != 0 {
		t.Fatalf("the room holds id %d to clock %d after the legacy socket's refused edit, want 0", freed, clock)
	}
	if rows := storedClientIDs(t, service, artifactID, freed); rows != 0 {
		t.Fatalf("%d rows name id %d after the legacy socket's refused edit, want none", rows, freed)
	}

	page := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, page, "alice's next page")
	if err := crdt.ApplyUpdateV1(page.Doc, saved, nil); err != nil {
		t.Fatal(err)
	}
	if err := page.Write(ygsync.EncodeUpdate(saved)); err != nil {
		t.Fatal(err)
	}
	waitForRoomRendering(t, room, func(rendering string) bool { return strings.Contains(rendering, "offline ") }, "the room holding the closed tab's saved edit")
	requireConnected(t, page, "alice's next page")
	if owner := clientIDOwner(t, service, artifactID, freed); owner != "alice" {
		t.Fatalf("the row of id %d names %q, want alice, whose page's edit landed under it", freed, owner)
	}
	awaitLanded(t, service, artifactID, freed, "the row of the id alice's page replayed the closed tab's edit under")
}

// Two people's pages send an edit under the same id from 2^32 up that no row names and the document
// holds nothing under - alice's page replaying her closed tab's saved edit under its id, whose row a
// give-back took, and mallory's under that id, read off the tab's presence before it closed - and
// both reach the id's mint before either stores it. The row's key, the binding's own lock, lets the
// first in: its edit lands and the row names its sender, and the second is refused 4413
// DOC_CLIENT_ID_REFUSED with nothing of its edit landing. The test holds a row of the id in an open
// transaction until both mints wait behind its key, and then takes it back.
func TestTwoPeopleSendingEditsUnderOneFreedIDTheFirstMintLandsAndTheSecondIsRefused(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }
	freed, saved := closedTabsSavedEdit(t, service, artifactID, serverURL, "alice", "SAVED ")

	alice := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, alice, "alice's next page")
	mallory := openPage(t, serverURL, artifactID, "mallory")
	awaitAnswer(t, mallory, "mallory's page")
	forged := forgedUnder(t, mallory.Doc, "EVIL ", freed, 0)
	if err := crdt.ApplyUpdateV1(alice.Doc, saved, nil); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	holder, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Rollback(ctx) })
	if _, err := holder.Exec(ctx, `
		insert into document_client_ids (artifact_id, client_id, actor_kind, actor_id, minted) values ($1, $2, 'user', 'holder', true)
	`, artifactID, int64(freed)); err != nil {
		t.Fatal(err)
	}
	if err := alice.Write(ygsync.EncodeUpdate(saved)); err != nil {
		t.Fatal(err)
	}
	if err := mallory.Write(ygsync.EncodeUpdate(forged)); err != nil {
		t.Fatal(err)
	}
	if waiting := storetest.WaitForLockWaiters(t, holder, 2, 30*time.Second); waiting < 2 {
		t.Fatalf("%d of the two edits' mints of id %d waited behind its held row, want both: alice's page ended %v, mallory's %v",
			waiting, freed, connectionEnd(alice), connectionEnd(mallory))
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	pages := map[string]*docstest.Peer{"alice": alice, "mallory": mallory}
	texts := map[string]string{"alice": "SAVED ", "mallory": "EVIL "}
	var loser string
	select {
	case <-alice.Ended:
		loser = "alice"
	case <-mallory.Ended:
		loser = "mallory"
	case <-time.After(30 * time.Second):
		t.Fatal("neither page was refused once the held row was taken back: both edits under one id stood")
	}
	winner := map[string]string{"alice": "mallory", "mallory": "alice"}[loser]
	if closed := pages[loser].Closed(); closed == nil || closed.Code != contracts.DocumentTooLargeCloseCode || closed.Text != clientIDRefusedReason {
		t.Fatalf("the room closed %s's page with %v, want %d %s", loser, closed, contracts.DocumentTooLargeCloseCode, clientIDRefusedReason)
	}
	requireConnected(t, pages[winner], winner+"'s page, whose mint came first")
	waitForRoomRendering(t, room, func(rendering string) bool { return strings.Contains(rendering, texts[winner]) }, "the room holding "+winner+"'s edit")
	if rendering := renderedBy(t, room()); strings.Contains(rendering, texts[loser]) {
		t.Fatalf("the room renders %q, which holds %s's refused edit", rendering, loser)
	}
	if clock := room().StateVector().Clock(freed); clock != uint64(len(texts[winner])) {
		t.Fatalf("the room holds id %d to clock %d, want the %d of %s's edit alone", freed, clock, len(texts[winner]), winner)
	}
	if owner := clientIDOwner(t, service, artifactID, freed); owner != winner {
		t.Fatalf("the row of id %d names %q, want %s, whose mint came first", freed, owner, winner)
	}
}

// connectionEnd is whether page's connection has ended, and with what close.
func connectionEnd(page *docstest.Peer) string {
	select {
	case <-page.Ended:
		return fmt.Sprintf("closed %v", page.Closed())
	default:
		return "open"
	}
}

// clientIDOwner is the login of the person the row of client in artifactID names, or "" where no
// row names it.
func clientIDOwner(t *testing.T, service *Service, artifactID string, client crdt.ClientID) string {
	t.Helper()
	var owner string
	err := service.store.Pool.QueryRow(context.Background(),
		`select coalesce(max(actor_id), '') from document_client_ids where artifact_id = $1 and client_id = $2`, artifactID, int64(client)).Scan(&owner)
	if err != nil {
		t.Fatal(err)
	}
	return owner
}
