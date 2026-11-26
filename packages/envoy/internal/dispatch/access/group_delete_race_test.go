package access

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// awaitGroupLockWaiter waits up to ten seconds for a backend of database to wait on a group's
// name lock (LockGroupName's statement), and fails the test if the call under test answers on
// answered first: that call never waited for the lock the test holds.
func awaitGroupLockWaiter(t *testing.T, database *store.Store, what string, answered <-chan error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case err := <-answered:
			t.Fatalf("%s finished (%v) while a write naming its group held the group's name lock; want it to wait", what, err)
		default:
		}
		waiting, err := storetest.CountLockWaits(context.Background(), database, "%'access_group:'%")
		if err != nil {
			t.Fatalf("count the waiters on a group's name lock: %v", err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never waited on its group's name lock within 10s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// listWrite is one write of a project's list as the routes run it: LockForAccessWrite, then the
// write, in the one transaction tx.
type listWrite func(ctx context.Context, tx pgx.Tx, ev *events.Broker, key, kind, principal string, by model.Actor) (model.ProjectAccess, model.Event, error)

// raceAgainstDelete opens a write of project's list naming group crew, takes its locks, starts a
// delete of crew in another transaction, and only then writes and commits, so the delete reaches
// every lock it can before the write does. Both must commit (no deadlock), and no row may name
// crew once they have.
func raceAgainstDelete(t *testing.T, project string, write listWrite) {
	t.Helper()
	ctx := context.Background()
	database := seedAccess(t)
	broker := events.NewBroker()
	by := model.Actor{Kind: "user", ID: olga}

	open, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the list write: %v", err)
	}
	defer open.Rollback(ctx)
	if _, err := LockForAccessWrite(ctx, open, project, "crew", ""); err != nil {
		t.Fatalf("lock %s for its list write: %v", project, err)
	}

	deleted := make(chan error, 1)
	go func() {
		deleted <- func() error {
			tx, err := database.Pool.Begin(context.Background())
			if err != nil {
				return err
			}
			defer tx.Rollback(context.Background())
			if _, err := DeleteGroup(context.Background(), tx, broker, "crew", by); err != nil {
				return err
			}
			return tx.Commit(context.Background())
		}()
	}()
	awaitGroupLockWaiter(t, database, "the delete of crew", deleted)
	if _, _, err := write(ctx, open, broker, project, PrincipalGroup, "crew", by); err != nil {
		t.Fatalf("write crew on %s's list while its delete waits: %v", project, err)
	}
	if err := open.Commit(ctx); err != nil {
		t.Fatalf("commit the list write: %v", err)
	}
	select {
	case err := <-deleted:
		if err != nil {
			t.Fatalf("delete crew after the list write committed: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the delete of crew never finished after the list write committed")
	}

	var rows int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from project_access where principal_kind = 'group' and principal = 'crew'
	`).Scan(&rows); err != nil {
		t.Fatalf("count crew's list rows: %v", err)
	}
	if rows != 0 {
		t.Fatalf("%d access-list rows still name crew after it was deleted, want none", rows)
	}
}

// TestAGroupDeletedWhileAListNamesItLeavesNoRow: a group named on a list by a write still open
// when the group is deleted is off that list once both commit; otherwise the list keeps a row
// naming a group that no longer exists, and a later group of the same name inherits the project.
func TestAGroupDeletedWhileAListNamesItLeavesNoRow(t *testing.T) {
	raceAgainstDelete(t, "PUB", AddPrincipal)
}

// TestAGroupDeletedWhileItIsTakenOffAListDeadlocksNothing: a delete of a group racing a write
// that takes the group off a list already naming it both commit. The write holds the project and
// the list row; the delete, unordered against it, would hold the group and sweep that row, then
// wait for the project the write holds, while the write waits for the row.
func TestAGroupDeletedWhileItIsTakenOffAListDeadlocksNothing(t *testing.T) {
	raceAgainstDelete(t, "OTHER", RemovePrincipal)
}

// TestAGroupsDeleteReadsEachProjectAfterItsLock: a delete of a group reads each project it takes
// the group off only once it holds that project's owner lock, so the record of the change it
// writes there (recordProjectChanges: the project's owner and visibility, which a caller's lost
// and gained sight is read from) carries every write the project's lock ordered before it. The
// project.access_changed event itself names only the project and the change, so the record is
// where a read before the lock would show: an owner written on OTHER by a write that holds OTHER's
// lock while the delete of crew waits is the owner the delete records; read before the lock, it
// would record OTHER's owner as it was, and a caller's sight would be judged against it.
func TestAGroupsDeleteReadsEachProjectAfterItsLock(t *testing.T) {
	ctx := context.Background()
	database := seedAccess(t)
	broker := events.NewBroker()
	by := model.Actor{Kind: "user", ID: olga}

	open, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the list write: %v", err)
	}
	defer open.Rollback(ctx)
	if _, err := LockForAccessWrite(ctx, open, "OTHER", "", ""); err != nil {
		t.Fatalf("lock OTHER for its list write: %v", err)
	}

	type result struct {
		events []model.Event
		err    error
	}
	deleted := make(chan result, 1)
	go func() {
		tx, err := database.Pool.Begin(context.Background())
		if err != nil {
			deleted <- result{err: err}
			return
		}
		defer tx.Rollback(context.Background())
		appended, err := DeleteGroup(context.Background(), tx, broker, "crew", by)
		if err == nil {
			err = tx.Commit(context.Background())
		}
		deleted <- result{events: appended, err: err}
	}()
	if got, answered, _ := storetest.AwaitOwnerLockWaiter(t, database, "the delete of crew", deleted); answered {
		t.Fatalf("the delete of crew finished (%v) while a write of OTHER's list held OTHER's lock; want it to wait", got.err)
	}
	if _, _, err := SetOwner(ctx, open, broker, "OTHER", carol, by); err != nil {
		t.Fatalf("make %s OTHER's owner while the delete of crew waits: %v", carol, err)
	}
	if err := open.Commit(ctx); err != nil {
		t.Fatalf("commit the list write: %v", err)
	}
	var got result
	select {
	case got = <-deleted:
	case <-time.After(30 * time.Second):
		t.Fatal("the delete of crew never finished after the list write committed")
	}
	if got.err != nil {
		t.Fatalf("delete crew after the list write committed: %v", got.err)
	}
	for _, event := range got.events {
		payload, ok := event.Payload.(model.ProjectAccessEventPayload)
		if !ok || payload.Project != "OTHER" {
			continue
		}
		var owner string
		if err := database.Pool.QueryRow(ctx, `select owner from project_access_changes where event_id = $1`, event.ID).Scan(&owner); err != nil {
			t.Fatalf("read the record of the delete's change on OTHER (event %d): %v", event.ID, err)
		}
		if owner != carol {
			t.Fatalf("the delete recorded OTHER's owner as %q, want %s, whom a write OTHER's lock ordered first made its owner", owner, carol)
		}
		return
	}
	t.Fatalf("the delete of crew appended no project.access_changed on OTHER: %v", got.events)
}
