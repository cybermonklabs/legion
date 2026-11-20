package docs

import (
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/contracts"
)

// rv9cFillCapUntilGone opens and leaves alice's pages until the row of gone is given back, and
// returns how many opens that took.
func rv9cFillCapUntilGone(t *testing.T, service *Service, artifactID, serverURL string, gone crdt.ClientID) int {
	t.Helper()
	url := socketURL(serverURL, artifactID) + "&" + contracts.DocumentClientIdParameter + "="
	opens := 0
	deadline := time.Now().Add(2 * time.Minute)
	for storedClientIDs(t, service, artifactID, gone) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("after %d opens the row of %d is still stored", opens, gone)
		}
		if _, busy := openAndLeave(t, url, "alice"); busy {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		opens++
	}
	return opens
}

// An honest offline edit: alice's tab is given id A, its socket goes down, she types offline and
// the tab closes before it reconnects - the dashboard keeps the row under A (pending-sync.ts).
// She goes on reading the document elsewhere without typing (each writable page load mints a row
// that no edit lands under). Once she holds maxUnlandedMintsADocument such rows, the next mint
// gives back the least recently presented one: A's, the closed tab's. Her next page of the
// document restores the saved row as the dashboard does - applied to its document and sent whole,
// under A - and must land it: nothing of the row is anyone else's.
func TestRV9CAClosedTabsSavedRowLandsAfterTheCapGaveBackItsID(t *testing.T) {
	presenceIntervalForTest.Store(int64(300 * time.Millisecond))
	t.Cleanup(func() { presenceIntervalForTest.Store(0) })
	service, artifactID, serverURL := clientIDRoom(t)
	room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }

	first := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, first, "alice's first tab")
	a := first.Given
	first.Close()
	<-first.Ended
	saved := typeAt(t, first.Doc, 0, "offline ")
	time.Sleep(time.Second) // A's row lapses (3 x 300 ms)

	opens := rv9cFillCapUntilGone(t, service, artifactID, serverURL, a)
	t.Logf("alice's row for id %d was given back after %d more page loads of hers; her unlanded rows: %d", a, opens, unlandedMintsOf(t, service, artifactID, "alice"))
	time.Sleep(1100 * time.Millisecond) // the left pages' rows lapse, so the next mint can give one back

	second := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, second, "alice's next tab")
	if err := crdt.ApplyUpdateV1(second.Doc, saved, nil); err != nil {
		t.Fatal(err)
	}
	if err := second.Write(ygsync.EncodeUpdate(saved)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-second.Ended:
		t.Fatalf("alice's next tab replaying her own closed tab's saved edit under id %d was closed %v: the dashboard drops every saved row under that id (dropClientRows) and her offline edit is lost", a, second.Closed())
	case <-time.After(3 * time.Second):
	}
	waitForRoomRendering(t, room, func(rendering string) bool { return strings.Contains(rendering, "offline ") }, "the room holding alice's offline edit")
}

// A row whose id landed an edit through another socket's replay - the closed tab's saved rows, which
// the next tab sends under the closed tab's id - is never marked landed (settleClients marks only the
// socket's own id), so the comment's claim that every id the history names in
// [mintedClientFloor, mintedClientCeiling) keeps its row does not hold: the row is given back past
// the cap, and anyone presenting the id is then minted it.
func TestRV9CAnIDLandedByAReplayKeepsItsRow(t *testing.T) {
	presenceIntervalForTest.Store(int64(300 * time.Millisecond))
	t.Cleanup(func() { presenceIntervalForTest.Store(0) })
	service, artifactID, serverURL := clientIDRoom(t)
	room := func() *crdt.Doc { return service.srv.GetDoc(artifactID) }

	first := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, first, "alice's first tab")
	a := first.Given
	first.Close()
	<-first.Ended
	saved := typeAt(t, first.Doc, 0, "offline ")

	second := openPage(t, serverURL, artifactID, "alice")
	awaitAnswer(t, second, "alice's next tab")
	if err := crdt.ApplyUpdateV1(second.Doc, saved, nil); err != nil {
		t.Fatal(err)
	}
	if err := second.Write(ygsync.EncodeUpdate(saved)); err != nil {
		t.Fatal(err)
	}
	waitForRoomRendering(t, room, func(rendering string) bool { return strings.Contains(rendering, "offline ") }, "the room holding the replayed edit")
	requireConnected(t, second, "alice's next tab")
	time.Sleep(time.Second)
	landed, stored := clientIDLanded(t, service, artifactID, a)
	t.Logf("after the replay landed under %d: its row stored=%t landed=%t", a, stored, landed)
	second.Close()
	<-second.Ended

	// A landed row is neither given back nor swept, so where the loop waited for this row's
	// deletion it now fills the cap and requires the row kept (the architect's ruling, Envoy
	// 9f1f2cfb, 2026-11-20).
	fill := socketURL(serverURL, artifactID) + "&" + contracts.DocumentClientIdParameter + "="
	deadline := time.Now().Add(2 * time.Minute)
	for opens := 0; opens < maxUnlandedMintsADocument+1; {
		if time.Now().After(deadline) {
			t.Fatalf("after %d opens the cap was not filled", opens)
		}
		if _, busy := openAndLeave(t, fill, "alice"); busy {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		opens++
	}
	if storedClientIDs(t, service, artifactID, a) == 0 {
		t.Fatalf("alice's row for %d, an id the stored history names, was given back after %d page loads", a, maxUnlandedMintsADocument+1)
	}
	t.Logf("alice's row for %d, an id the stored history names, is still stored after %d page loads", a, maxUnlandedMintsADocument+1)

	url := socketURL(serverURL, artifactID) + "&" + contracts.DocumentClientIdParameter + "=" + strconvU(uint64(a))
	mallory, _ := openAndLeave(t, url, "mallory")
	if mallory == a {
		t.Fatalf("mallory, presenting id %d which the history names for alice's edit, was minted it", a)
	}
	if !landed {
		t.Fatalf("the row of id %d, under which a replay landed alice's edit, never said it landed, so a mint could give it back", a)
	}
}

func strconvU(v uint64) string {
	var b [20]byte
	i := len(b)
	for {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
		if v == 0 {
			break
		}
	}
	return string(b[i:])
}
