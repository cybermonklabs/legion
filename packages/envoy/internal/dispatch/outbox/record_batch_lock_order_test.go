package outbox

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// recordLockWait bounds each wait of the lock-order test: for the record to queue behind a hold,
// and for the scan to return once the record has queued behind none.
const recordLockWait = 30 * time.Second

// TestRecordBatchTakesAPrefixOfTheStoreLockOrder is the outbox half of dispatch://INFRA-1149's
// c-lock-order: recordBatch is a store write path that takes fewer steps than the store's lock
// order lists, and the steps it takes are a prefix of that order with nothing out of it. The
// order's steps are the owner row, the node rows in ascending (kind, id), the tuple append, the
// projection rows and the project of the event (the lock order on api/inbox_read.go's
// markInboxReadThreads). The record runs on a pooled connection with no transaction of its own,
// so it takes no owner row, and it writes no projection row and appends no event, so it takes
// no project row either: what it takes is the node rows and then the tuple append.
// graphstore's TestEveryStoreWriteTakesOneLockOrder cannot run this path, since outbox imports
// graphstore.
//
// The run drives one batch through the publisher's own scan: a message routed to a role whose
// holder the claim store names, so the record writes the role's node and its page's and appends
// the role's claims (deliveredGraph). Before the scan it holds, each in a transaction of its own:
// the issue's row and its project's row `for update`, which conflicts with every lock any writer
// takes on them; the page's node and the role's node, each written by the store's own writer and
// not committed, which is what the record's node upsert waits for; and the held_by tuple the
// record appends, written under the same event and not committed, which is what its repeat key
// waits for. Then it lets go of each hold the record queues behind, one at a time, and reads the
// order the record queued in. It has to be the page, the role and then the tuple: the page sorts
// below the role while deliveredGraph lists the role first, so input order and ascending order
// disagree, and the tuple append comes after the node rows. The record runs as one statement, so
// the order of its steps is the order Postgres runs the arms graphstore.WriteArms splices in.
// The owner and the project are never among them: they are still held when the scan returns with
// the batch recorded.
func TestRecordBatchTakesAPrefixOfTheStoreLockOrder(t *testing.T) {
	const (
		key  = "T-1"
		role = "lock-order-role"
		// The session the claim store names is the one recordingPublisher takes a role
		// forward to, so the forward is acknowledged and the event is published.
		holder = roleHolder
	)
	database := storetest.Open(t)
	ctx := context.Background()
	broker := events.NewBroker()
	route := "role:" + role
	seedIssue(t, database, key, &route)
	claim := roleClaimStub{holder: holder, lastSeen: time.Now().UnixMilli()}
	deps := Deps{Store: database, Publisher: &recordingPublisher{}, Broker: broker,
		RoleClaims: claim, Sessions: claim}
	event := appendEvent(t, database, broker, model.Event{
		IssueKey: new(key), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.Message{ID: "0f9c2d2b-6a16-4b2f-9b2a-7c5a1a6d4f20", IssueKey: new(key), Body: "for the role"},
	})
	outbox := holding(t, deps)

	at := time.Now()
	page := graphstore.PageNode(graphstore.RolePageID(role), graphWriter, at, event.ID)
	roleNode := graphstore.RoleNode(role, graphWriter, at, event.ID)
	if page.Kind >= roleNode.Kind {
		t.Fatalf("the page node %s/%s does not sort below the role node %s/%s; the test needs the batch's input order and ascending order to disagree",
			page.Kind, page.ID, roleNode.Kind, roleNode.ID)
	}
	heldBy := graphstore.Tuple{
		FromKind: graphstore.KindRole, FromID: role, Kind: graphstore.TupleHeldBy,
		ToKind: graphstore.KindSession, ToID: holder,
		At: at, Who: graphWriter, SourceEvent: event.ID, Asserted: true,
	}

	owner := beginRecordHold(t, ctx, database, "the issue's and its project's rows")
	for _, statement := range []string{
		`select 1 from issues where key = $1 for update`,
		`select 1 from projects where key = (select project_key from issues where key = $1) for update`,
	} {
		if _, err := owner.tx.Exec(ctx, statement, key); err != nil {
			t.Fatalf("hold the owner rows of %s: %v", key, err)
		}
	}
	lower := beginRecordHold(t, ctx, database, "the page's node row")
	if err := graphstore.Write(ctx, lower.tx, []graphstore.Node{page}, nil); err != nil {
		t.Fatalf("write the page's node in a transaction left open: %v", err)
	}
	upper := beginRecordHold(t, ctx, database, "the role's node row")
	if err := graphstore.Write(ctx, upper.tx, []graphstore.Node{roleNode}, nil); err != nil {
		t.Fatalf("write the role's node in a transaction left open: %v", err)
	}
	tuple := beginRecordHold(t, ctx, database, "the held_by tuple")
	if err := graphstore.Write(ctx, tuple.tx, nil, []graphstore.Tuple{heldBy}); err != nil {
		t.Fatalf("append the held_by tuple in a transaction left open: %v", err)
	}

	scanned := make(chan scanEnd, 1)
	go func() { scanned <- outbox.scan(ctx, unfenced{}) }()
	queued, end := followTheRecord(t, ctx, database, []*recordHold{owner, lower, upper, tuple}, scanned)
	names := make([]string, len(queued))
	for index, hold := range queued {
		names[index] = hold.name
	}
	t.Logf("the record queued behind, in order: %v", names)

	if slices.Contains(queued, owner) {
		t.Errorf("the record queued behind %s (in order: %v): recordBatch takes no owner row and no project row, since it runs on a pooled connection with no transaction of its own and appends no event",
			owner.name, names)
	}
	pageAt, roleAt, tupleAt := slices.Index(queued, lower), slices.Index(queued, upper), slices.Index(queued, tuple)
	if pageAt < 0 || roleAt < 0 || pageAt > roleAt {
		t.Errorf("the record queued behind %v, want %s before %s: a write over several nodes takes their rows in ascending (kind, id), whatever order its caller lists them in",
			names, lower.name, upper.name)
	}
	if tupleAt < 0 {
		t.Errorf("the record queued behind %v and never behind %s: the tuple append is a step the record takes", names, tuple.name)
	} else if tupleAt < pageAt || tupleAt < roleAt {
		t.Errorf("the record queued behind %v, want %s after both node rows: the tuple append comes after the node rows in the store's lock order, and a record that appends first holds a tuple's repeat key while it waits for a node another writer holds",
			names, tuple.name)
	}
	if end.failed || end.fenced || end.natsLost {
		t.Fatalf("the scan ended %+v, want the batch recorded", end)
	}
	var published bool
	if err := database.Pool.QueryRow(ctx,
		`select published_at is not null from events where id = $1`, event.ID).Scan(&published); err != nil {
		t.Fatalf("read event %d: %v", event.ID, err)
	}
	if !published {
		t.Fatalf("event %d is not published after the scan, so the record the test followed is not one that committed", event.ID)
	}
	requireTuple(t, database, graphstore.KindRole, role, graphstore.TupleHeldBy, graphstore.KindSession, holder, true)
	owner.release()
}

// recordHold is one transaction the lock-order test holds rows in, named as a failure names it.
type recordHold struct {
	name     string
	tx       pgx.Tx
	released bool
}

// beginRecordHold opens a transaction to hold rows in. Its release is registered as a cleanup
// too, so a failed assertion never leaves the scan queued on a lock nothing will drop.
func beginRecordHold(t *testing.T, ctx context.Context, database *store.Store, name string) *recordHold {
	t.Helper()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction holding %s: %v", name, err)
	}
	hold := &recordHold{name: name, tx: tx}
	t.Cleanup(hold.release)
	return hold
}

func (h *recordHold) release() {
	if h.released {
		return
	}
	h.released = true
	_ = h.tx.Rollback(context.Background())
}

// recordStatement is a phrase of recordBatch's own statement, which the record's backend is found
// by, since the record runs on whatever connection the pool hands it. It comes from the head of
// the statement: pg_stat_activity keeps only the first track_activity_query_size bytes of it, and
// the arms graphstore.WriteArms splices in come after that. The fallback record
// (recordFallbackCopies) opens with the same hold, and updates another table.
const recordStatement = "%with hold as%recorded as (%update events e set%"

// followTheRecord lets go of each hold the record's backend queues behind, one at a time, until
// the scan returns, and answers the holds in the order the record queued behind them and how the
// scan ended. A statement waits on one lock at a time, so a record queued behind two holds at
// once is a failure of the test's own reading, and a record that queues behind none and does not
// return is waiting on something the test does not hold.
func followTheRecord(t *testing.T, ctx context.Context, database *store.Store, holds []*recordHold, scanned <-chan scanEnd) ([]*recordHold, scanEnd) {
	t.Helper()
	var queued []*recordHold
	deadline := time.Now().Add(recordLockWait)
	for {
		select {
		case end := <-scanned:
			return queued, end
		default:
		}
		behind := holdsTheRecordWaitsOn(t, ctx, database, holds)
		switch len(behind) {
		case 0:
			if time.Now().After(deadline) {
				t.Fatalf("the record queued behind none of the holds still held and the scan did not return within %s (queued so far behind %d)",
					recordLockWait, len(queued))
			}
			time.Sleep(2 * time.Millisecond)
		case 1:
			queued = append(queued, behind[0])
			behind[0].release()
			deadline = time.Now().Add(recordLockWait)
		default:
			t.Fatalf("the record queued behind %d holds at once, which one statement cannot do", len(behind))
		}
	}
}

// holdsTheRecordWaitsOn is every hold still held that the record's backend is queued behind. It
// reads pg_stat_activity through the pool, a fresh view on each read.
func holdsTheRecordWaitsOn(t *testing.T, ctx context.Context, database *store.Store, holds []*recordHold) []*recordHold {
	t.Helper()
	rows, err := database.Pool.Query(ctx, `
		select pid from pg_stat_activity
		where datname = current_database() and pid <> pg_backend_pid() and query like $1
	`, recordStatement)
	if err != nil {
		t.Fatalf("find the record's backend: %v", err)
	}
	pids, err := pgx.CollectRows(rows, pgx.RowTo[uint32])
	if err != nil {
		t.Fatalf("find the record's backend: %v", err)
	}
	var behind []*recordHold
	for _, pid := range pids {
		for _, hold := range holds {
			if hold.released {
				continue
			}
			queued, err := storetest.QueuedBehind(ctx, hold.tx, pid)
			if err != nil {
				t.Fatalf("read what the record waits on: %v", err)
			}
			if queued {
				behind = append(behind, hold)
			}
		}
	}
	return behind
}
