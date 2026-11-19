package docs

import (
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
)

// heardStatus waits for page to hear a SyncStatus answer and returns everything it heard.
func heardStatus(t *testing.T, page *docstest.Peer) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var heard []string
		for _, h := range page.Heard() {
			heard = append(heard, h.What)
		}
		for _, what := range heard {
			if strings.HasPrefix(what, "SyncStatus") {
				return heard
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("page heard no SyncStatus: %v", page.Heard())
	return nil
}

// RV8 (ApiAndWebAudit): a legacy tab of the earlier build whose id mallory claimed first with one
// character, and which then types past mallory's clock, is refused 4413 DOC_CLIENT_ID_REFUSED on
// the keystroke. The earlier build knows no 4413 and its provider reconnects; the reconnect's
// sync step 2 carries the same edit. The PR body says the edit is "never dropped in silence" and
// that "the refusal repeats until the tab reloads". It does not: the step 2 has the edit cut out
// (stepTwoCut), is answered SyncStatus 1, and the tab stays connected, so the earlier build's
// pending sync clears its saved rows (clearThrough) with the edit nowhere in the room.
//
// Review 8's repro is kept, with what the decision to delete the cut (Q1, round 9) changed. The
// room now refuses this step 2 whole, as it refuses the keystroke, and closes the tab, so:
//   - Where the repro required the reconnected tab to stay connected after its step 2, it requires
//     the tab closed 4413 DOC_CLIENT_ID_REFUSED and answered nothing applied. The room still lacks
//     the edit.
//   - The reconnect dials a document holding nothing the room lacks under the tab's id, and the
//     tab's own document is what the test's step 2 hands back. docstest's reader answers the
//     room's sync step 1 at once, before the test can keep what the peer hears; with the tab's
//     document that answer was the step 2 carrying the edit, which the room now refuses, so its
//     not-applied answer could come before the peer kept it, and its close before the test's own
//     write. The step 2 carrying the edit is now the test's alone, sent once the peer hears
//     (hearFromNow), as the repro meant.
//   - That write tolerates a socket the room has already closed, so a refusal that overtakes it
//     fails on the assertions after it rather than on the write.
func TestRV8ALegacyTabsEditPastAForgersClaimIsAnsweredAppliedOnItsReconnect(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }
	const tabClient = crdt.ClientID(1_234_569_001)
	tab := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("alice"), artifactID, crdt.New(crdt.WithClientID(tabClient)))
	awaitAnswer(t, tab, "the document")
	mallory := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("mallory"), artifactID, crdt.New(crdt.WithClientID(1_234_569_002)))
	awaitAnswer(t, mallory, "the document")
	// The tab types a run of text, whose frame is in flight as mallory claims the tab's id with one
	// character at clock 0 (the race the PR body names), so the tab's edit runs past her claim.
	typed := typeAt(t, tab.Doc, 0, "alice typed this")
	if err := mallory.Write(ygsync.EncodeUpdate(forgedUnder(t, mallory.Doc, "E", tabClient, 0))); err != nil {
		t.Fatal(err)
	}
	waitForRoomRendering(t, room, func(rendering string) bool { return strings.Contains(rendering, "EAlice") }, "the room holding mallory's claim")
	requireConnected(t, mallory, "mallory")
	if err := tab.Write(ygsync.EncodeUpdate(typed)); err != nil {
		t.Fatal(err)
	}
	awaitRefused(t, tab, "the tab whose edit runs past mallory's claim")

	// The earlier build's provider reconnects after the 4413; it answers the room's sync step 1
	// with what the room lacks. The test sends that answer itself, once it hears.
	back := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("alice"), artifactID, crdt.New(crdt.WithClientID(tabClient)))
	awaitAnswer(t, back, "the document after the tab's reconnect")
	hearFromNow(t, back, "the reconnected legacy tab")
	if err := back.Write(encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarUint(ygsync.MsgSyncStep2)
		encoder.WriteVarBytes(crdt.EncodeStateAsUpdateV1(tab.Doc, room().StateVector()))
	})); err != nil {
		select {
		case <-back.Ended:
		default:
			t.Fatal(err)
		}
	}
	heard := heardStatus(t, back)
	awaitRefused(t, back, "the reconnected legacy tab")
	requireNothingApplied(t, back, "the reconnected legacy tab's sync step 2")
	rendering := renderedBy(t, room())
	t.Logf("the reconnected tab heard %v; the room renders %q", heard, rendering)
	if strings.Contains(rendering, "alice typed") {
		t.Fatalf("the room holds the tab's edit: %q", rendering)
	}
	for _, what := range heard {
		if strings.HasPrefix(what, "SyncStatus 1") {
			t.Errorf("the tab's edit is not in the room (%q), yet its sync step 2 carrying it was answered %q and the tab is still connected: an earlier-build tab clears its saved rows on that answer, so the edit is lost with no refusal", rendering, what)
		}
	}
}

// RV8 (ApiAndWebAudit): the same cut reaches a page of this build. Alice's reloaded page restores
// a row the legacy tab saved under its id, which mallory claimed first; before the replay's
// Update is judged, the socket drops (network, a deploy, a 4503 close). On the reconnect the
// page's sync step 2 carries the row's structs, has them cut, and is answered SyncStatus 1; the
// dashboard's pending sync acknowledges a step 2 as covering every row recorded so far
// (pending-sync.ts frameWritten/clearThrough), so the row is deleted with no notice.
//
// Review 8's repro is kept, with what the decision to delete the cut (Q1, round 9) changed. The
// room now refuses this step 2 whole and closes the page, which the dashboard reads as a refusal:
// it keeps its saved rows, rebuilds and replays them. So:
//   - Where the repro required the reconnected page to stay connected after its step 2, it
//     requires the page closed 4413 DOC_CLIENT_ID_REFUSED and answered nothing applied. The room
//     still lacks the restored row.
//   - The reconnect presents the page's client id with a document holding nothing the room lacks,
//     and the page's own document is what the test's step 2 hands back. docstest's reader answers
//     the room's sync step 1 at once, before the test can keep what the peer hears; with the
//     page's document that answer was the step 2 carrying the row, which the room now refuses, so
//     its not-applied answer could come before the peer kept it, and its close before the test's
//     own write. The step 2 carrying the row is now the test's alone, sent once the peer hears
//     (hearFromNow), as the repro meant.
//   - That write tolerates a socket the room has already closed, so a refusal that overtakes it
//     fails on the assertions after it rather than on the write.
func TestRV8APageOfThisBuildsRestoredRowUnderAClaimedIDIsAnsweredAppliedInItsStepTwo(t *testing.T) {
	service, artifactID, serverURL := clientIDRoom(t)
	room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }
	const tabClient = crdt.ClientID(1_234_569_101)
	oldTab := crdt.New(crdt.WithClientID(tabClient))
	first := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("alice"), artifactID, oldTab)
	awaitAnswer(t, first, "the document")
	first.Close()
	saved := typeAt(t, oldTab, 0, "saved offline by the old tab")

	mallory := docstest.Dial(t, socketURL(serverURL, artifactID), pageHeader("mallory"), artifactID, crdt.New(crdt.WithClientID(1_234_569_102)))
	awaitAnswer(t, mallory, "the document")
	if err := mallory.Write(ygsync.EncodeUpdate(forgedUnder(t, mallory.Doc, "E", tabClient, 0))); err != nil {
		t.Fatal(err)
	}
	waitForRoomRendering(t, room, func(rendering string) bool { return strings.Contains(rendering, "EAlice") }, "the room holding mallory's claim")

	page := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, page, "the reloaded page")
	// The restore applies the saved row to the page's document; its socket drops before the
	// replay's frame is judged.
	if err := crdt.ApplyUpdateV1(page.Doc, saved, nil); err != nil {
		t.Fatal(err)
	}
	page.Close()
	back := reopenPage(t, serverURL, artifactID, "alice", crdt.New(crdt.WithClientID(page.Doc.ClientID())))
	awaitAnswer(t, back, "the page after its reconnect")
	hearFromNow(t, back, "alice's reconnected page")
	if err := back.Write(encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarUint(ygsync.MsgSyncStep2)
		encoder.WriteVarBytes(crdt.EncodeStateAsUpdateV1(page.Doc, room().StateVector()))
	})); err != nil {
		select {
		case <-back.Ended:
		default:
			t.Fatal(err)
		}
	}
	heard := heardStatus(t, back)
	awaitRefused(t, back, "alice's reconnected page")
	requireNothingApplied(t, back, "alice's reconnected page's sync step 2")
	rendering := renderedBy(t, room())
	t.Logf("the reconnected page heard %v; the room renders %q", heard, rendering)
	if strings.Contains(rendering, "saved offline") {
		t.Fatalf("the room holds the restored row: %q", rendering)
	}
	for _, what := range heard {
		if strings.HasPrefix(what, "SyncStatus 1") {
			t.Errorf("the restored row is not in the room (%q), yet the step 2 carrying it was answered %q with no refusal: the dashboard clears every row recorded so far on that answer", rendering, what)
		}
	}
}
