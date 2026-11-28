package docs

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A call that runs a room's batch stores the room's unstored updates recorded before its own
// (flushRun), and ygo's persistence observer can hand a later update over before an earlier one:
// ygo runs a transaction's observers once it has released the document's lock, so the hand-overs
// need not come in the order the room recorded the updates in (room_appends.go, "The batch").
// Here alice's room records keystroke A and then keystroke B, its issue closing between the two,
// and ygo hands B's update over first. B's call runs the batch, whose run is A and B, and the
// closed issue's gate admits A, which the room took in time, and cuts B, which it took longer than
// closeMargin after the close (observedBeforeClose, gateAppend). B's call commits A, and it is the
// last call that meets the gate for B: B's class leaves the room's queue as the call ends
// (finishUpdateClass), and A's own call, made next, finds A stored. So B's call must refuse B as a
// call whose whole run the gate refused does (refuseClosedIssueAppend): the room is closed for its
// issue, and alice's browser reconnects read-only. A call that returned nil once it had stored A
// left B neither stored nor refused, and alice's room went on taking keystrokes on the closed
// issue that the store would never take.
//
// The two keystrokes are recorded and handed to the room's persistence here as the room's update
// observer and ygo's worker do (classifyDrained, StoreUpdateContext), so the test decides the
// order of the calls. Each carries when the room took it - A a minute ago, B now - and the issue
// closed half a minute past the margin ago, so the gate's verdict on each is fixed by those times
// alone.
func TestABrowserUpdateTheClosedIssuesGateCutsFromItsOwnBatchIsRefusedToItsSender(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service, server := newTestServiceInstance(t, database)
	seedServiceText(t, service, artifactID, "before")
	waitForLandedAppends(t, service, artifactID)
	var failed atomic.Bool
	service.beforeFailedEviction = func(string) { failed.Store(true) }
	alice := connectWithScope(t, server.URL, artifactID, "alice", "read-write", "before\n")
	waitForLandedAppends(t, service, artifactID)
	live := service.srv.GetDoc(artifactID)
	instance := service.instanceOf(live)
	if instance == nil {
		t.Fatalf("alice's browser loaded no room of %s that lists an instance, so the keystrokes below are recorded on none", artifactID)
	}
	head := storedHead(t, database, artifactID)
	// No room sync runs here, so nothing tells the room its issue closed: the gate is what
	// notices.
	writeIssueClosedAgo(t, database, "DOC-1", closeMargin+30*time.Second)

	// What the room's update observer records for a browser's keystroke (classifyDrained): its
	// position among the instance's commits, a content change credited to alice, the one browser
	// connected, and when the room took it, listed on the instance until its append returns.
	browser := &browserOrigin{}
	record := func(seq uint64, update []byte, observed time.Time) *queuedClass {
		class := service.queueUpdateClass(artifactID, update, documentUpdateClass{
			seq:            seq,
			contentChanged: true,
			durable:        true,
			credit:         service.creditContentChange(artifactID, browser, &instance.hold, nil),
			cursor:         &instance.cursor,
			observed:       observed,
			instance:       instance,
		}, false)
		instance.listUnstored(class)
		return class
	}
	_, inTime := peerEdit(t, live, appendBlocks(t, "typed in time\n"))
	_, late := peerEdit(t, live, appendBlocks(t, "typed after the close\n"))
	now := time.Now()
	a := record(1, inTime, now.Add(-time.Minute))
	b := record(2, late, now)
	adapter := &servicePersistenceAdapter{store: service.persistence, service: service}

	// ygo hands B over first. Its call's batch runs through B and stores the updates the room
	// recorded before it, so A is in its run.
	if err := adapter.StoreUpdateContext(ctx, artifactID, late); err != nil {
		t.Fatalf("B's call = %v, want nil: a closed issue's refusal fails no room", err)
	}
	if !a.stored.Load() {
		t.Fatal("B's call did not store A, so its batch's run did not hold the keystroke recorded before B and the gate cut nothing from it: nothing here is tested")
	}
	if b.stored.Load() {
		t.Fatal("B's call marked B stored: the gate admitted the keystroke the room took after the close margin was out")
	}
	refused := instance.issueCloseStarted.Load()
	// A's own call comes next, as ygo's worker hands it over, and finds A stored.
	if err := adapter.StoreUpdateContext(ctx, artifactID, inTime); err != nil {
		t.Fatalf("A's call = %v, want nil: B's call stored A", err)
	}

	if moved := storedHead(t, database, artifactID); moved != head+1 {
		t.Fatalf("the document moved from update %d to %d, want one stored update: A, the keystroke the room took in time", head, moved)
	}
	stored := storedMarkdown(t, database, artifactID)
	if !strings.Contains(stored, "typed in time") {
		t.Fatalf("the store reads %q, without A, the keystroke the room took while the issue was open", stored)
	}
	if strings.Contains(stored, "typed after the close") {
		t.Fatalf("the store reads %q, holding B, the keystroke the room took after the close margin was out", stored)
	}
	if !refused {
		t.Fatal("B's call returned with B neither stored nor refused: refuseClosedIssueAppend never ran for the room, which goes on taking alice's keystrokes on the closed issue, and no later call meets the gate for B, whose class left the queue as its call ended")
	}
	requireEnds(t, alice, "B's call refused B on the closed issue and left alice's browser connected")
	if failed.Load() {
		t.Fatal("the refusal failed the room rather than closing it for its issue")
	}
	connectWithScope(t, server.URL, artifactID, "alice", "readonly", stored)
}
