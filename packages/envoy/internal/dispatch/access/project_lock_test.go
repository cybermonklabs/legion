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

// TestAProjectWriteTakesTheOwnerLockBeforeTheRow: every write of a project's access takes the
// project's owner lock itself, before it writes anything, the order store.LockOwner's contract
// names ("the lock and then the row, never the other way round"), so a caller that took no lock
// first, as `envoy-dispatch project-owner` does, is safe. While another transaction holds the lock,
// each write must wait on it holding no transaction id: a write that wrote its row (or its list
// row) first holds one, and its append then waits for the lock with the row held. The lock's
// holder then writes what the write would have taken first, as an issue create writes the
// project's row after its lock: row first, that is a deadlock Postgres breaks by aborting one of
// the two. Once the holder commits, the write must commit.
func TestAProjectWriteTakesTheOwnerLockBeforeTheRow(t *testing.T) {
	by := model.Actor{Kind: "user", ID: olga}
	writeProjectRow := func(ctx context.Context, tx pgx.Tx, ev *events.Broker, key string) error {
		if _, err := tx.Exec(ctx, `set local lock_timeout = '3s'`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `update projects set name = name where key = $1`, key)
		return err
	}
	for _, write := range []struct {
		name    string
		project string
		run     func(ctx context.Context, tx pgx.Tx, ev *events.Broker) error
		// held is what the lock's holder writes once the write waits on the lock.
		held func(ctx context.Context, tx pgx.Tx, ev *events.Broker, key string) error
	}{
		{"SetOwner", "PUB", func(ctx context.Context, tx pgx.Tx, ev *events.Broker) error {
			_, _, err := SetOwner(ctx, tx, ev, "PUB", carol, by)
			return err
		}, writeProjectRow},
		{"SetVisibility", "PUB", func(ctx context.Context, tx pgx.Tx, ev *events.Broker) error {
			_, _, err := SetVisibility(ctx, tx, ev, "PUB", model.ProjectPrivate, by)
			return err
		}, writeProjectRow},
		{"SetSlackChannels", "PUB", func(ctx context.Context, tx pgx.Tx, ev *events.Broker) error {
			_, _, err := SetSlackChannels(ctx, tx, ev, "PUB", []string{"C012345678"}, by, by)
			return err
		}, writeProjectRow},
		// The holder lists carol itself, so the write, reading the list after the lock, finds
		// her listed and writes no second row.
		{"AddPrincipal", "PUB", func(ctx context.Context, tx pgx.Tx, ev *events.Broker) error {
			_, _, err := AddPrincipal(ctx, tx, ev, "PUB", PrincipalPerson, carol, by)
			return err
		}, func(ctx context.Context, tx pgx.Tx, ev *events.Broker, key string) error {
			_, _, err := AddPrincipal(ctx, tx, ev, key, PrincipalPerson, carol, by)
			return err
		}},
		// The holder takes carol off PRIV itself, so the write, reading the list after the lock,
		// finds nothing to take off.
		{"RemovePrincipal", "PRIV", func(ctx context.Context, tx pgx.Tx, ev *events.Broker) error {
			_, _, err := RemovePrincipal(ctx, tx, ev, "PRIV", PrincipalPerson, carol, by)
			return err
		}, func(ctx context.Context, tx pgx.Tx, ev *events.Broker, key string) error {
			_, _, err := RemovePrincipal(ctx, tx, ev, key, PrincipalPerson, carol, by)
			return err
		}},
	} {
		t.Run(write.name, func(t *testing.T) {
			ctx := context.Background()
			database := seedAccess(t)
			broker := events.NewBroker()

			holder, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin the lock's holder: %v", err)
			}
			defer holder.Rollback(ctx)
			if err := store.LockOwner(ctx, holder, store.ProjectOwner(write.project)); err != nil {
				t.Fatalf("take %s's owner lock: %v", write.project, err)
			}

			done := make(chan error, 1)
			go func() {
				done <- func() error {
					tx, err := database.Pool.Begin(context.Background())
					if err != nil {
						return err
					}
					defer tx.Rollback(context.Background())
					if err := write.run(context.Background(), tx, broker); err != nil {
						return err
					}
					return tx.Commit(context.Background())
				}()
			}()
			early, answered, holdingXid := storetest.AwaitOwnerLockWaiter(t, database, write.name, done)
			if answered {
				t.Fatalf("%s finished (%v) while another transaction held %s's owner lock; want it to wait for the lock", write.name, early, write.project)
			}
			if holdingXid != 0 {
				t.Fatalf("%s waits on %s's owner lock holding a transaction id: it wrote before it took the lock", write.name, write.project)
			}

			if err := write.held(ctx, holder, broker, write.project); err != nil {
				t.Fatalf("the owner lock's holder could not write %s while %s waited: %v", write.project, write.name, err)
			}
			if err := holder.Commit(ctx); err != nil {
				t.Fatalf("commit the lock's holder: %v", err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("%s after the lock's holder committed: %v", write.name, err)
				}
			case <-time.After(30 * time.Second):
				t.Fatalf("%s never finished after the lock's holder committed", write.name)
			}
		})
	}
}
