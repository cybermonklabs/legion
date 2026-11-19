package access

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// TestAProjectWriteTakesTheOwnerLockBeforeTheRow: SetOwner, SetVisibility and SetSlackChannels
// each take the project's owner lock before they write its row, the order store.LockOwner's
// contract names ("the lock and then the row, never the other way round"). A caller that took no
// lock first, as `envoy-dispatch project-owner` does, then waits for a transaction that holds the
// lock without holding the row. Row first, the write would hold PUB's row while it waited for the
// lock in its append, and the lock's holder, writing PUB's row next as an issue create does, would
// wait for it in turn: a deadlock Postgres breaks by aborting one of the two.
func TestAProjectWriteTakesTheOwnerLockBeforeTheRow(t *testing.T) {
	by := model.Actor{Kind: "user", ID: olga}
	for _, write := range []struct {
		name string
		run  func(ctx context.Context, tx pgx.Tx, ev *events.Broker) error
	}{
		{"SetOwner", func(ctx context.Context, tx pgx.Tx, ev *events.Broker) error {
			_, _, err := SetOwner(ctx, tx, ev, "PUB", carol, by)
			return err
		}},
		{"SetVisibility", func(ctx context.Context, tx pgx.Tx, ev *events.Broker) error {
			_, _, err := SetVisibility(ctx, tx, ev, "PUB", model.ProjectPrivate, by)
			return err
		}},
		{"SetSlackChannels", func(ctx context.Context, tx pgx.Tx, ev *events.Broker) error {
			_, _, err := SetSlackChannels(ctx, tx, ev, "PUB", []string{"C012345678"}, by, nil)
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
			if err := store.LockOwner(ctx, holder, store.ProjectOwner("PUB")); err != nil {
				t.Fatalf("take PUB's owner lock: %v", err)
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
			select {
			case err := <-done:
				t.Fatalf("%s finished (%v) while another transaction held PUB's owner lock; want it to wait for the lock", write.name, err)
			case <-time.After(500 * time.Millisecond):
			}

			if _, err := holder.Exec(ctx, `set local lock_timeout = '3s'`); err != nil {
				t.Fatalf("bound the holder's row wait: %v", err)
			}
			if _, err := holder.Exec(ctx, `update projects set name = name where key = 'PUB'`); err != nil {
				t.Fatalf("the owner lock's holder could not write PUB's row while %s waited (%v): %s holds the row before the lock", write.name, err, write.name)
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
