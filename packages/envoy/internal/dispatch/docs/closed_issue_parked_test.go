package docs

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testcleanup"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A browser's update can park bytes, structs that wait for an edit the room has not received, and
// the room stores them on their own once the update is applied (onPeerUpdate, storeParked,
// AppendParkedUpdate). They carry no observed time, so once the document's issue is closed the
// append gate refuses them (ErrIssueClosed), however soon after the edit the close came
// (observedBeforeClose). Here alice's update types a paragraph and carries another client's edit
// that waits for its predecessor, so the room takes the paragraph and parks the rest. The issue
// then closes in the database before the room stores the parked bytes, with nothing notified, as
// a listener's redial leaves it (this test runs no room sync), and the gate refuses them. That
// refusal must close the room for its issue, as a browser update's refusal does
// (refuseClosedIssueAppend): alice's browser reconnects read-only, and the close waits for the
// room's persistence, which stores the paragraph, an edit the room took while the issue was open.
// A refusal that fails the room instead drops that paragraph unstored (dropUpdate) and reloads
// the room as though the store had broken.
//
// The issue closes in the room's update observer for alice's update (afterCreditUpdate), which
// runs once the drain has queued the parked bytes and before the peer's read loop stores them.
// The room's persistence worker is held meanwhile on alice's keystroke before that update
// (heldStore), so the paragraph's own append begins only once the room has failed or begun its
// close for the issue and the test lets that keystroke go.
func TestParkedBytesTheClosedIssuesGateRefusesCloseTheirRoomForItsIssue(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service, held := closedIssueRoomService(t, database, artifactID)
	server := newTestServer(t, service)

	// decided closes once the room has failed (beforeFailedEviction) or begun its close for its
	// issue (beforeRefusedAppendClose), whichever comes first.
	var failed atomic.Bool
	decided := make(chan struct{})
	var decide sync.Once
	service.beforeFailedEviction = func(room string) {
		if room == artifactID {
			failed.Store(true)
			decide.Do(func() { close(decided) })
		}
	}
	service.beforeRefusedAppendClose = func(room string) {
		if room == artifactID {
			decide.Do(func() { close(decided) })
		}
	}
	// Armed, the next content change the room's observer credits closes the issue in the
	// database, as PATCH /api/v1/issues/{key} does (writeIssueClosed), before ygo hands the
	// update on and before the peer's read loop stores what it parked.
	var closeOnCredit atomic.Bool
	closed := make(chan error, 1)
	service.afterCreditUpdate = func(room string) {
		if room != artifactID || !closeOnCredit.CompareAndSwap(true, false) {
			return
		}
		_, err := database.Pool.Exec(context.Background(), `update issues set closed_at = now() where key = $1`, "DOC-1")
		closed <- err
	}

	seedServiceText(t, service, artifactID, "before")
	waitForLandedAppends(t, service, artifactID)
	alice := connectWithScope(t, server.URL, artifactID, "alice", "read-write", "before\n")
	waitForLandedAppends(t, service, artifactID)
	live := service.srv.GetDoc(artifactID)
	instance := service.instanceOf(live)
	if instance == nil {
		t.Fatalf("alice's browser loaded no room of %s that lists an instance, so nothing below is typed into one", artifactID)
	}

	// Alice's first keystroke reaches the store and is held there, so the room's persistence
	// worker, which makes one call at a time, takes the paragraph's append only once it is let go.
	holdOneRoomUpdate(t, held, func() { typeOver(t, alice, "before", "typed first") })

	// Alice's update: a paragraph the room takes, and another client's edit that waits for a
	// predecessor the room never received, which the room parks.
	_, paragraph := peerEdit(t, live, appendBlocks(t, "typed while open\n"))
	other := crdt.New(crdt.WithClientID(8))
	otherText := other.GetText("t")
	other.Transact(func(txn *crdt.Transaction) { otherText.Insert(txn, 0, "base", nil) })
	atBase := other.StateVector()
	other.Transact(func(txn *crdt.Transaction) { otherText.Insert(txn, 4, " later", nil) })
	waits := crdt.EncodeStateAsUpdateV1(other, atBase)
	mixed, err := crdt.MergeUpdatesV1(paragraph, waits)
	if err != nil {
		t.Fatalf("merge the paragraph with the edit that waits for its predecessor: %v", err)
	}
	closeOnCredit.Store(true)
	if err := alice.Write(ygsync.EncodeUpdate(mixed)); err != nil {
		t.Fatalf("send alice's update that types a paragraph and parks: %v", err)
	}
	if err := testwait.Receive(t, "the room's observer of alice's update to close the issue", closed); err != nil {
		t.Fatalf("close issue DOC-1 in the database: %v", err)
	}
	if pending := live.PendingStats(); pending.Items == 0 {
		t.Fatalf("the room parked nothing of alice's update (%+v), so no parked bytes meet the gate and nothing here is tested", pending)
	}
	testwait.Receive(t, "the room to fail or begin its close for its issue once the gate refused the parked bytes", decided)
	held.let()
	requireEnds(t, alice, "the gate refused the parked bytes on the closed issue and left alice's browser connected")
	waitForRoomGone(t, service, artifactID, "DOC-1", live)
	waitForRefusedAppendClose(t, service)

	stored := storedMarkdown(t, database, artifactID)
	if failed.Load() {
		t.Error("the gate's refusal of the parked bytes on the closed issue failed the room rather than closing it for its issue")
	}
	if !instance.issueCloseStarted.Load() {
		t.Error("the gate's refusal of the parked bytes did not close the room for its issue: refuseClosedIssueAppend never ran for the instance that parked them")
	}
	if !strings.Contains(stored, "typed while open") {
		t.Errorf("the store reads %q, without the paragraph the room took while the issue was open, whose append was queued when the gate refused the parked bytes", stored)
	}
	if t.Failed() {
		t.FailNow()
	}
	connectWithScope(t, server.URL, artifactID, "alice", "readonly", stored)
}

// errParkedAppendBroke is what failingParkedStore answers an append of a commit's parked bytes
// with: a failure of the store's own, not the closed issue's refusal (ErrIssueClosed).
var errParkedAppendBroke = errors.New("the store broke while appending the parked bytes")

// failingParkedStore stores a document's updates as the store does, but fails every append of a
// commit's parked bytes (AppendParkedUpdate) with errParkedAppendBroke, and counts those appends.
type failingParkedStore struct {
	VersionedStore
	appends atomic.Int64
}

func (s *failingParkedStore) AppendParkedUpdate(context.Context, string, []byte) (persistence.Version, error) {
	s.appends.Add(1)
	return 0, errParkedAppendBroke
}

// The gate's refusal of a commit's parked bytes (ErrIssueClosed) closes their room for its issue,
// as the test above holds; every other error of that append still fails the room (storeParked,
// appendFailed). Here the store fails the append of the parked bytes with an error of its own on
// an open issue (failingParkedStore). Alice's update types a paragraph and carries another
// client's edit that waits for its predecessor, so the room takes the paragraph and parks the
// rest, as above, and the peer's read loop then stores the parked bytes on their own. The room
// must fail on that error (beforeFailedEviction, for the store's error) and its eviction must end
// alice's socket; refuseClosedIssueAppend must not run for it, so the room begins no close for
// its issue (beforeRefusedAppendClose) and the instance that parked the bytes keeps its mark
// untaken (issueCloseStarted). A storeParked that answered every error as the closed issue's
// refusal would close a room whose store had broken as though its issue had closed.
func TestParkedBytesAStoreErrorOtherThanTheCloseFailsTheRoom(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	failing := &failingParkedStore{VersionedStore: NewPgVersioned(database)}
	service := New(Deps{
		Store: database, Persistence: failing, Events: events.NewBroker(),
		Identity: headerIdentity(database), Settle: time.Hour,
	})
	testcleanup.Add(t, func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	server := newTestServer(t, service)

	// failure takes the room's failure as its eviction begins (beforeFailedEviction), and decided
	// closes once the room has failed or begun its close for its issue (beforeRefusedAppendClose),
	// whichever comes first.
	failure := make(chan error, 1)
	var refused atomic.Bool
	decided := make(chan struct{})
	var decide sync.Once
	service.beforeFailedEviction = func(room string) {
		if room == artifactID {
			select {
			case failure <- service.roomFailure(room):
			default:
			}
			decide.Do(func() { close(decided) })
		}
	}
	service.beforeRefusedAppendClose = func(room string) {
		if room == artifactID {
			refused.Store(true)
			decide.Do(func() { close(decided) })
		}
	}
	// Armed, the room's observer of the next content change closes credited, once the drain has
	// queued what that update parked and before the peer's read loop stores it.
	var creditArmed atomic.Bool
	credited := make(chan struct{})
	service.afterCreditUpdate = func(room string) {
		if room == artifactID && creditArmed.CompareAndSwap(true, false) {
			close(credited)
		}
	}

	seedServiceText(t, service, artifactID, "before")
	waitForLandedAppends(t, service, artifactID)
	alice := connectWithScope(t, server.URL, artifactID, "alice", "read-write", "before\n")
	waitForLandedAppends(t, service, artifactID)
	live := service.srv.GetDoc(artifactID)
	instance := service.instanceOf(live)
	if instance == nil {
		t.Fatalf("alice's browser loaded no room of %s that lists an instance, so nothing below is typed into one", artifactID)
	}

	// Alice's update: a paragraph the room takes, and another client's edit that waits for a
	// predecessor the room never received, which the room parks.
	_, paragraph := peerEdit(t, live, appendBlocks(t, "typed while open\n"))
	other := crdt.New(crdt.WithClientID(8))
	otherText := other.GetText("t")
	other.Transact(func(txn *crdt.Transaction) { otherText.Insert(txn, 0, "base", nil) })
	atBase := other.StateVector()
	other.Transact(func(txn *crdt.Transaction) { otherText.Insert(txn, 4, " later", nil) })
	waits := crdt.EncodeStateAsUpdateV1(other, atBase)
	mixed, err := crdt.MergeUpdatesV1(paragraph, waits)
	if err != nil {
		t.Fatalf("merge the paragraph with the edit that waits for its predecessor: %v", err)
	}
	creditArmed.Store(true)
	if err := alice.Write(ygsync.EncodeUpdate(mixed)); err != nil {
		t.Fatalf("send alice's update that types a paragraph and parks: %v", err)
	}
	testwait.Receive(t, "the room's observer of alice's update", credited)
	if pending := live.PendingStats(); pending.Items == 0 {
		t.Fatalf("the room parked nothing of alice's update (%+v), so no parked bytes meet the store and nothing here is tested", pending)
	}
	testwait.Receive(t, "the room to fail or begin its close for its issue once the store failed the parked bytes' append", decided)
	requireEnds(t, alice, "the store failed the parked bytes' append and left alice's browser connected")

	if failing.appends.Load() == 0 {
		t.Error("the store was never asked to append the parked bytes, so the room's end came from elsewhere")
	}
	select {
	case cause := <-failure:
		if !errors.Is(cause, errParkedAppendBroke) {
			t.Errorf("the room failed for %v, not for the store's error on the parked bytes' append", cause)
		}
	default:
		t.Error("the store's error on the parked bytes' append, which is not the closed issue's refusal, did not fail the room")
	}
	if refused.Load() {
		t.Error("the store's error on the parked bytes' append began a close of the room for its issue (beforeRefusedAppendClose), although the issue is open and the error is not ErrIssueClosed")
	}
	if instance.issueCloseStarted.Load() {
		t.Error("refuseClosedIssueAppend ran for the instance that parked the bytes: its issueCloseStarted mark is taken on a store error that is not ErrIssueClosed")
	}
}
