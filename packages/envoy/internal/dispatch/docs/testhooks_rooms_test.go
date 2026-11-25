package docs

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	ygws "github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The room-counters hook counts each room of a document the service closes, and no close that
// closed nothing (a refusal for the room's peers, a document no room holds), and records the time
// of each settlement-resumption pass that armed a document's owed settlement, on the service's
// clock: here a fake the test never moves, so the time a pass records is that clock's one reading.
func TestRoomCountersCountServiceClosesAndSettlementResumptions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	serviceClock := clocktest.New(time.Now())
	service, server := newTestServiceInstanceOn(t, database, serviceClock)
	service.hooks = newTestHooks()
	seedServiceText(t, service, artifactID, "before")
	waitForLandedAppends(t, service, artifactID)
	alice := connectWithScope(t, server.URL, artifactID, "alice", "read-write", "before\n")

	if err := closeRoomJoined(t, service, artifactID, false); !errors.Is(err, ygws.ErrRoomHasPeers) {
		t.Fatalf("an unforced close of the room alice is in = %v, want ErrRoomHasPeers", err)
	}
	if closes := requireRoomCounters(t, service, artifactID).RoomCloses; closes != 0 {
		t.Fatalf("room closes after a close refused for the room's peers = %d, want 0", closes)
	}
	if err := closeRoomJoined(t, service, artifactID, true); err != nil {
		t.Fatalf("close the room: %v", err)
	}
	if closes := requireRoomCounters(t, service, artifactID).RoomCloses; closes != 1 {
		t.Fatalf("room closes after one close of the room = %d, want 1", closes)
	}
	requireEnds(t, alice, "closing the room left alice's browser connected")
	const untouched = "00000000-0000-0000-0000-000000000000"
	if err := closeRoomJoined(t, service, untouched, true); !errors.Is(err, ygws.ErrRoomNotFound) {
		t.Fatalf("a close of a document no room holds = %v, want ErrRoomNotFound", err)
	}

	// A document of its own, which no room holds and nothing settles, so its pending row stands
	// until the pass reads it.
	owed := createIssueDocument(t, database, 2, "# Owed")
	if resumed := requireRoomCounters(t, service, owed).SettlementResumptions; len(resumed) != 0 {
		t.Fatalf("settlement resumptions before any pass = %v, want none", resumed)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into doc_settlements_pending (artifact_id, marked_at) values ($1, now() - interval '2 minutes')
	`, owed); err != nil {
		t.Fatalf("owe the document's settlement: %v", err)
	}
	if err := service.resumeOwedSettlements(ctx, settlementResumeAge); err != nil {
		t.Fatalf("resume the owed settlements: %v", err)
	}
	resumed := requireRoomCounters(t, service, owed).SettlementResumptions
	if at := serviceClock.Now(); len(resumed) != 1 || !resumed[0].Equal(at) {
		t.Fatalf("settlement resumptions after one pass that armed the document = %v, want one, at the service clock's %s",
			resumed, at.Format(time.RFC3339Nano))
	}
	if other := requireRoomCounters(t, service, untouched); other.RoomCloses != 0 || len(other.SettlementResumptions) != 0 {
		t.Fatalf("the counters of a document whose only close found no room = %+v, want zero", other)
	}
}

func requireRoomCounters(t *testing.T, service *Service, artifactID string) RoomCounters {
	t.Helper()
	counters, err := service.RoomCounters(artifactID)
	if err != nil {
		t.Fatalf("read the room counters: %v", err)
	}
	return counters
}

// The room-counters hook counts, per awareness client, what a process's room takes of that client
// from another process's notification, and nothing the room did itself: alice's state reaching
// task B counts one state of her there and none on task A, where it is a change of the room's
// own, bob's the other way round, and alice leaving task A counts her one removal on task B. Each
// count is read once the reader on the other task has seen the change, which that task's room
// hands its readers only after it has applied it. A room's own removal of a client counts only as
// the expiry of one it took from another process: task A expiring bob counts his expiry there,
// and alice's departure from task A, a removal of task A's own client, took no entry on task A.
// ygo fires a room's observers before RemoveExpired returns, so the count is read after it.
func TestRoomCountersCountTheAwarenessAnotherProcessesNotificationsMakeTheRoomTake(t *testing.T) {
	pair := newProcessPair(t)
	pair.taskA.hooks = newTestHooks()
	pair.taskB.hooks = newTestHooks()
	runAwareness(t, pair.taskA)
	runAwareness(t, pair.taskB)
	alice := pair.openReader(t, pair.serverA, "alice")
	bob := pair.openReader(t, pair.serverB, "bob")

	setAwareness(t, alice, aliceClient, cursorOf("alice", 1))
	requireAwareness(t, bob, holdsAwareness(aliceClient, cursorOf("alice", 1)), "alice's cursor on task A reaching bob on task B")
	setAwareness(t, bob, bobClient, cursorOf("bob", 2))
	requireAwareness(t, alice, holdsAwareness(bobClient, cursorOf("bob", 2)), "bob's cursor on task B reaching alice on task A")
	requireRelayedAwareness(t, pair.taskB, pair.artifactID, aliceClient, RelayedAwarenessChanges{States: 1}, "task B, which took alice's state from task A")
	requireRelayedAwareness(t, pair.taskA, pair.artifactID, aliceClient, RelayedAwarenessChanges{}, "task A, where alice's state is its own room's")
	requireRelayedAwareness(t, pair.taskA, pair.artifactID, bobClient, RelayedAwarenessChanges{States: 1}, "task A, which took bob's state from task B")
	requireRelayedAwareness(t, pair.taskB, pair.artifactID, bobClient, RelayedAwarenessChanges{}, "task B, where bob's state is its own room's")

	alice.Close()
	requireAwareness(t, bob, lacksAwareness(aliceClient), "alice leaving task A removing her from bob's reader on task B")
	requireRelayedAwareness(t, pair.taskB, pair.artifactID, aliceClient, RelayedAwarenessChanges{States: 1, Removals: 1}, "task B, which took alice's removal from task A")

	awarenessA, resident := pair.taskA.srv.GetAwareness(pair.artifactID)
	if !resident {
		t.Fatal("task A holds no room of the document to expire bob from")
	}
	awarenessA.RemoveExpired(0)
	requireRelayedAwareness(t, pair.taskA, pair.artifactID, bobClient, RelayedAwarenessChanges{States: 1, Expired: 1}, "task A, which expired bob after taking his state from task B")
	requireRelayedAwareness(t, pair.taskA, pair.artifactID, aliceClient, RelayedAwarenessChanges{}, "task A, from whose room alice, its own client, departed")
}

// requireRelayedAwareness fails unless service's rooms of artifactID have taken exactly want of
// client from other processes' notifications (RoomCounters.RelayedAwareness).
func requireRelayedAwareness(t *testing.T, service *Service, artifactID string, client uint64, want RelayedAwarenessChanges, where string) {
	t.Helper()
	if got := requireRoomCounters(t, service, artifactID).RelayedAwareness[client]; got != want {
		t.Fatalf("on %s the room counters hold %+v relayed of client %d, want %+v", where, got, client, want)
	}
}

// The room-counters hook counts each notification that a document's issue closed or reopened
// which the issue-open lane applies to the document (issueOpenChanged), and apart from it each
// close the recheck after a LISTEN (recheckResidentIssues) makes. Here the issue closes with
// nothing notified, as a listener's redial leaves it, and the recheck closes alice's room: a
// close of the service's, one recheck's close and no notification applied. The reopen is then
// notified, and the lane's close of alice's read-only room counts one notification and no
// recheck. A recheck that finds the room's record matching the issue, as the one after alice
// reconnects does, closes nothing and counts nothing.
func TestRoomCountersCountTheIssueNotificationsTheIssueOpenLaneApplies(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	service, server := newTestServiceInstance(t, database)
	service.hooks = newTestHooks()
	seedServiceText(t, service, artifactID, "before")
	waitForLandedAppends(t, service, artifactID)
	alice := connectWithScope(t, server.URL, artifactID, "alice", "read-write", "before\n")

	writeIssueClosed(t, database, "DOC-1", true)
	service.recheckResidentIssues(store.WithTransactionTracking(context.Background()))
	requireEnds(t, alice, "the recheck of issue DOC-1's close left alice's browser connected")
	if counters := requireRoomCounters(t, service, artifactID); counters.RoomCloses == 0 || counters.NotifiedIssueCloses != 0 {
		t.Fatalf("after the recheck closed alice's room the counters hold %d room closes and %d notified issue closes, want a close and no notification applied",
			counters.RoomCloses, counters.NotifiedIssueCloses)
	}
	if rechecked := requireRoomCounters(t, service, artifactID).RecheckedIssueCloses; rechecked != 1 {
		t.Fatalf("after the recheck closed alice's room the counters hold %d rechecked issue closes, want 1", rechecked)
	}

	alice = connectWithScope(t, server.URL, artifactID, "alice", "readonly", "before\n")
	setIssueClosedHere(t, service, "DOC-1", false)
	requireEnds(t, alice, "the notified reopen of issue DOC-1 left alice's read-only browser connected")
	if closes := requireRoomCounters(t, service, artifactID).NotifiedIssueCloses; closes != 1 {
		t.Fatalf("after one notified reopen the counters hold %d notified issue closes, want 1", closes)
	}
	if rechecked := requireRoomCounters(t, service, artifactID).RecheckedIssueCloses; rechecked != 1 {
		t.Fatalf("after a notified reopen the counters hold %d rechecked issue closes, want the recheck's 1 alone", rechecked)
	}

	connectWithScope(t, server.URL, artifactID, "alice", "read-write", "before\n")
	service.recheckResidentIssues(store.WithTransactionTracking(context.Background()))
	if rechecked := requireRoomCounters(t, service, artifactID).RecheckedIssueCloses; rechecked != 1 {
		t.Fatalf("after a recheck of a room whose record matches its issue the counters hold %d rechecked issue closes, want the first recheck's 1 alone", rechecked)
	}
}

// The room-counters hook records the head a process's room held after each catch-up another
// process's notification asked for and, apart from it, after the sweep each LISTEN that stands
// runs, and it counts the LISTENs. Task A's room is resident when its first LISTEN stands, so that
// sweep records the head the room loaded at. Task B's write then reaches task A's room on its
// notification, which records the head that write left as notified and leaves the swept head
// where it was. Then task A's listener loses its connection and task B writes while it redials, so
// that write's notification reaches nobody on task A: the sweep after the LISTEN that stands again
// carries it, which records the new head as swept, leaves the notified head where it was, and
// counts a second LISTEN.
func TestRoomCountersRecordTheHeadsTheNotifiedCatchUpsAndTheSweepsLeave(t *testing.T) {
	ctx := context.Background()
	pair := newProcessPair(t)
	pair.taskA.hooks = newTestHooks()
	hub := newListenerHub(pair.taskA)
	var dials atomic.Int32
	up := make(chan struct{})
	dial := hub.Dial
	hub.Dial = func(ctx context.Context) (*pgx.Conn, error) {
		if dials.Add(1) == 1 {
			return dial(ctx)
		}
		select {
		case <-up:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return dial(ctx)
	}
	if err := pair.taskA.warmLiveDocument(ctx, pair.artifactID); err != nil {
		t.Fatalf("load task A's room: %v", err)
	}
	listening, _ := startRoomSync(t, hub, newRoomSync(pair.taskA))
	testwait.Receive(t, "task A's first LISTEN to stand and its sweep to run", listening)
	loaded := storedVersionHead(t, pair.database, pair.artifactID)
	requireCaughtUpHeads(t, pair.taskA, pair.artifactID, 0, loaded, 1, "after the first LISTEN's sweep")

	bob := model.Actor{Kind: "user", ID: "bob"}
	editAsBrowserPeer(t, pair.taskB, pair.artifactID, 2, bob, appendBlocks(t, "Bob writes while task A listens.\n"))
	waitForLandedAppends(t, pair.taskB, pair.artifactID)
	notified := storedVersionHead(t, pair.database, pair.artifactID)
	testwait.Until(t, "task A's room to record the head task B's notified write left", func() (int64, bool) {
		head := requireRoomCounters(t, pair.taskA, pair.artifactID).NotifiedHead
		return head, head >= notified
	})
	requireCaughtUpHeads(t, pair.taskA, pair.artifactID, notified, loaded, 1, "after task B's notified write")

	if _, err := pair.database.Pool.Exec(ctx, `
		select pg_terminate_backend(pid) from pg_stat_activity where application_name = $1
	`, ListenerApplicationPrefix+pair.taskA.ProcessID()); err != nil {
		t.Fatalf("end task A's listener connection: %v", err)
	}
	testwait.Until(t, "task A's listener to dial again after losing its connection", func() (int32, bool) {
		attempts := dials.Load()
		return attempts, attempts >= 2
	})
	editAsBrowserPeer(t, pair.taskB, pair.artifactID, 3, bob, appendBlocks(t, "Bob writes while task A redials.\n"))
	waitForLandedAppends(t, pair.taskB, pair.artifactID)
	swept := storedVersionHead(t, pair.database, pair.artifactID)
	close(up)
	testwait.Receive(t, "task A's LISTEN to stand again and its sweep to run", listening)
	requireCaughtUpHeads(t, pair.taskA, pair.artifactID, notified, swept, 2, "after the sweep that followed the redial")
}

// storedVersionHead is the highest version database stores of artifactID.
func storedVersionHead(t *testing.T, database *store.Store, artifactID string) int64 {
	t.Helper()
	var head int64
	if err := database.Pool.QueryRow(context.Background(),
		`select coalesce(max(version), 0) from doc_updates where artifact_id = $1`, artifactID).Scan(&head); err != nil {
		t.Fatalf("read the stored head of %s: %v", artifactID, err)
	}
	return head
}

// requireCaughtUpHeads fails unless service's room counters of artifactID hold notified as
// NotifiedHead, swept as SweptHead and stands as ListenStands.
func requireCaughtUpHeads(t *testing.T, service *Service, artifactID string, notified, swept, stands int64, when string) {
	t.Helper()
	counters := requireRoomCounters(t, service, artifactID)
	if counters.NotifiedHead != notified || counters.SweptHead != swept || counters.ListenStands != stands {
		t.Fatalf("%s the room counters hold notified head %d, swept head %d and %d LISTENs, want %d, %d and %d",
			when, counters.NotifiedHead, counters.SweptHead, counters.ListenStands, notified, swept, stands)
	}
}
