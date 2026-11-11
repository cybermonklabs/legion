package docs

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/pglisten"
)

// newListenerHub is service's process listener as cmd/dispatch builds it: a pglisten.Hub on a
// dedicated connection named for service's membership. A test may wrap its Dial before it runs it
// (startRoomSync).
func newListenerHub(service *Service) *pglisten.Hub {
	return &pglisten.Hub{
		Owner: "dispatch test",
		Dial: func(ctx context.Context) (*pgx.Conn, error) {
			return service.store.Pool.Dedicated(ctx, ListenerApplicationPrefix+service.ProcessID())
		},
	}
}

// startRoomSync registers rs on hub and runs both, as cmd/dispatch does, until the test ends or
// stop is called. listening receives a token after each LISTEN's connected work has run
// (roomSync.connected); it waits for none, because a test whose dial is down at the start has to
// write before any LISTEN stands. stop returns once the hub and every lane have returned.
func startRoomSync(t *testing.T, hub *pglisten.Hub, rs *roomSync) (listening <-chan struct{}, stop func()) {
	t.Helper()
	connected := make(chan struct{}, 4)
	rs.listening = func() {
		select {
		case connected <- struct{}{}:
		default:
		}
	}
	run := rs.register(hub)
	ctx, cancel := context.WithCancel(context.Background())
	var running sync.WaitGroup
	running.Add(2)
	go func() {
		defer running.Done()
		hub.Run(ctx)
	}()
	go func() {
		defer running.Done()
		run(ctx)
	}()
	done := make(chan struct{})
	go func() {
		running.Wait()
		close(done)
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("the room sync and its listener hub did not return within 10s of their context ending")
			}
		})
	}
	t.Cleanup(stop)
	return connected, stop
}

// runRoomSync runs rs on a listener hub of its service's (newListenerHub, startRoomSync) and
// returns once its first LISTEN stands: a notification sent before that reaches nobody. stop
// returns once the hub and rs have returned.
func runRoomSync(t *testing.T, rs *roomSync) (stop func()) {
	t.Helper()
	listening, stop := startRoomSync(t, newListenerHub(rs.service), rs)
	select {
	case <-listening:
	case <-time.After(roomWait(t)):
		t.Fatal("the room sync's listener hub never listened")
	}
	return stop
}

// Membership is the set of processes holding their shared listener connection (the
// pglisten.Hub room sync registers on), read from pg_stat_activity by the name that connection
// carries. Two processes on one database, each on a pool of its own, each see both ids, their
// own included; the NOTIFY sender, a connection of its own as well, is never counted; and a
// process whose listener stops leaves at once, with nothing to collect, because its connection is
// gone.
func TestEveryProcessesRoomSyncListenerIsAMemberUntilItStops(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	second := storetest.SecondProcess(t, database)
	taskA, _ := newTestServiceInstance(t, database)
	taskB, _ := newTestServiceInstance(t, second)
	if taskA.processID == "" || taskA.processID == taskB.processID {
		t.Fatalf("the two services' process ids are %q and %q, want two distinct ids", taskA.processID, taskB.processID)
	}

	runRoomSync(t, newRoomSync(taskA))
	stopB := runRoomSync(t, newRoomSync(taskB))
	if err := taskA.roomSyncSender.Notify(ctx, documentAwarenessChannel, "{}"); err != nil {
		t.Fatalf("send through task A's sender: %v", err)
	}

	want := []string{taskA.processID, taskB.processID}
	slices.Sort(want)
	for _, process := range []struct {
		name     string
		database *store.Store
	}{{"task A", database}, {"task B", second}} {
		members, err := roomSyncMembers(ctx, process.database.Pool)
		if err != nil {
			t.Fatalf("%s reads the members: %v", process.name, err)
		}
		if !slices.Equal(members, want) {
			t.Fatalf("%s reads the members %v, want %v: both listeners, and not the sender", process.name, members, want)
		}
	}

	stopB()
	waitFor(t, roomWait(t), "task B to leave the members once its listener stopped", func() bool {
		members, err := roomSyncMembers(ctx, database.Pool)
		if err != nil {
			t.Fatalf("read the members: %v", err)
		}
		return slices.Equal(members, []string{taskA.processID})
	})
}

// The sender is what a request uses to reach the other processes while it holds a transaction of
// the shared pool, so it must take no connection of that pool (store.ErrNestedAcquire) and its
// notification must go out at once, not at the transaction's commit. Here the caller's context
// refuses a second shared connection, each send reaches the other process's handler before the
// transaction ends, and the transaction then rolls back, which would have dropped a notification
// sent inside it. The sender's connection is then killed under it, as one idle for long can die,
// and the next send redials and still arrives.
func TestTheRoomSyncSenderReachesTheOtherProcessFromInsideATransaction(t *testing.T) {
	database := storetest.Open(t)
	taskA, _ := newTestServiceInstance(t, database)
	taskB, _ := newTestServiceInstance(t, storetest.SecondProcess(t, database))
	heard := make(chan string, 4)
	listenerB := newRoomSync(taskB)
	listenerB.awareness = func(_ context.Context, payload string) { heard <- payload }
	runRoomSync(t, listenerB)

	ctx := store.WithTransactionTracking(context.Background())
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the request's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := database.Pool.Exec(ctx, `select 1`); !errors.Is(err, store.ErrNestedAcquire) {
		t.Fatalf("a second shared connection inside the transaction = %v, want ErrNestedAcquire: the guard this test leans on is not armed", err)
	}
	send := func(when, payload string) {
		t.Helper()
		if err := taskA.roomSyncSender.Notify(ctx, documentAwarenessChannel, payload); err != nil {
			t.Fatalf("%s: %v", when, err)
		}
		select {
		case got := <-heard:
			if got != payload {
				t.Fatalf("%s: task B's handler heard %q, want %q", when, got, payload)
			}
		case <-time.After(roomWait(t)):
			t.Fatalf("%s: task B's handler heard nothing while the sender's caller held its transaction", when)
		}
	}
	send("the first send", "first")

	senderName := roomSyncSenderApplicationPrefix + taskA.processID
	var terminated int
	if err := tx.QueryRow(ctx, `
		select count(*) from (
			select pg_terminate_backend(pid) from pg_stat_activity
			where datname = current_database() and application_name = $1
		) killed
	`, senderName).Scan(&terminated); err != nil {
		t.Fatalf("terminate the sender's connection: %v", err)
	}
	if terminated != 1 {
		t.Fatalf("terminated %d connections named %q, want the sender's one", terminated, senderName)
	}
	waitFor(t, roomWait(t), "the sender's terminated connection to leave pg_stat_activity", func() bool {
		var alive bool
		if err := database.Pool.QueryRow(context.Background(), `
			select exists (select 1 from pg_stat_activity where datname = current_database() and application_name = $1)
		`, senderName).Scan(&alive); err != nil {
			t.Fatalf("read the sender's connection: %v", err)
		}
		return !alive
	})
	send("the send after the sender's connection died", "second")

	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("roll back the request's transaction: %v", err)
	}
}

// removeArtifactRow deletes artifactID's artifacts row, as a removal's transaction does
// (api.removeArtifact). The rows that reference it go first - neither doc_updates nor
// artifact_versions cascades, so the foreign key refuses the delete while one of them stands -
// and nothing here announces the removal: the pass under test is the one that reads the rows
// itself, for the removals whose announcement reached nobody.
func removeArtifactRow(t *testing.T, database *store.Store, artifactID string) {
	t.Helper()
	ctx := context.Background()
	for _, statement := range []string{
		`delete from doc_updates where artifact_id = $1`,
		`delete from artifact_versions where artifact_id = $1`,
		`delete from artifacts where id = $1`,
	} {
		if _, err := database.Pool.Exec(ctx, statement, artifactID); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

// A room is resident under the name the browser's URL spelled, which the admission parses and
// keeps as it stands (ServeHTTP), and a uuid has more spellings than one: a person who opens
// /ws/doc/<the id in upper case> holds a room named in upper case over a row whose id::text
// Postgres always writes in lower case. The removal lane's pass after a LISTEN has to read that
// room as the document it is. Matching the rooms' own names against id::text read it as a row
// that had gone and closed a live room under the person in it, and only when some other document
// was really removed: with every row still there the pass returns on the count of the answer,
// comparing nothing, which is why the harm needs the removed document here beside the kept one.
func TestTheRemovalPassKeepsARoomHeldUnderAnotherSpellingOfItsId(t *testing.T) {
	database := storetest.Open(t)
	removed := createDocument(t, database, "Removed.")
	kept := createDocument(t, database, "Kept.")
	service, server := newTestServiceInstance(t, database)
	seedServiceText(t, service, removed, "Removed.")
	seedServiceText(t, service, kept, "Kept.")
	waitForLandedAppends(t, service, removed)
	waitForLandedAppends(t, service, kept)
	upper := strings.ToUpper(kept)
	// Each room stands under the name its browser's URL spelled: bob's under the removed
	// document's canonical id, alice's under the kept document's id in upper case.
	bob := connectWithScope(t, server.URL, removed, "bob", "read-write", storedMarkdown(t, database, removed))
	alice := connectWithScope(t, server.URL, upper, "alice", "read-write", storedMarkdown(t, database, kept))
	if !roomResident(service, removed) || !roomResident(service, upper) {
		t.Fatalf("the process holds the rooms %v, want one named %s and one named %s", service.ResidentRooms(), removed, upper)
	}

	removeArtifactRow(t, database, removed)
	service.recheckRemovedRooms(context.Background())

	if roomResident(service, removed) {
		t.Fatalf("the pass left the room %s standing, whose artifacts row has gone: bob types into it what no append can store", removed)
	}
	requireEnds(t, bob, "the removal of "+removed+" left bob's browser on its room connected")
	if !roomResident(service, upper) {
		t.Fatalf("the pass closed the room %s, alice's live room of a document that is still there: a room resident under another spelling of its own id read as removed", upper)
	}
	select {
	case <-alice.Ended:
		t.Fatalf("the pass ended alice's browser on the room %s, whose document is still there", upper)
	default:
	}
}

// Two rooms of one document, held under two spellings of its id, both close when that document
// is removed: the pass closes every key it holds under a canonical spelling, not the first of
// them. A key left behind would be exactly the room this pass exists to take - one standing over
// a deleted row, whose person types what the doc_updates foreign key refuses.
func TestTheRemovalPassClosesEverySpellingOfARemovedDocumentsRoom(t *testing.T) {
	database := storetest.Open(t)
	removed := createDocument(t, database, "Removed.")
	service, server := newTestServiceInstance(t, database)
	seedServiceText(t, service, removed, "Removed.")
	waitForLandedAppends(t, service, removed)
	upper := strings.ToUpper(removed)
	stored := storedMarkdown(t, database, removed)
	alice := connectWithScope(t, server.URL, removed, "alice", "read-write", stored)
	bob := connectWithScope(t, server.URL, upper, "bob", "read-write", stored)
	if !roomResident(service, removed) || !roomResident(service, upper) {
		t.Fatalf("the process holds the rooms %v, want one named %s and one named %s", service.ResidentRooms(), removed, upper)
	}

	removeArtifactRow(t, database, removed)
	service.recheckRemovedRooms(context.Background())

	for _, room := range []string{removed, upper} {
		if roomResident(service, room) {
			t.Fatalf("the pass left the room %s standing over the removed document %s: every room of it has to go, whatever it is named", room, removed)
		}
	}
	requireEnds(t, alice, "the document's removal left alice's browser on the room "+removed+" connected")
	requireEnds(t, bob, "the document's removal left bob's browser on the room "+upper+" connected")
}
