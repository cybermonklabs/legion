package delivery

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

var groupTestActor = model.Actor{Kind: "user", ID: "alice"}

// createCommittedGroup creates and commits a group named name holding sessions.
func createCommittedGroup(t *testing.T, ctx context.Context, pool *store.Pool, name string, sessions ...string) SessionGroup {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the create: %v", err)
	}
	group, err := CreateSessionGroup(ctx, tx, name, sessions, groupTestActor)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("CreateSessionGroup: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the create: %v", err)
	}
	return group
}

// renameAndCommit renames the group id to name in tx, which it commits, and answers the rename's
// answer.
func renameAndCommit(t *testing.T, ctx context.Context, tx pgx.Tx, id, name string) SessionGroup {
	t.Helper()
	renamed, err := UpdateSessionGroup(ctx, tx, id, SessionGroupUpdate{Name: &name}, groupTestActor)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("rename the group to %q: %v", name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the rename to %q: %v", name, err)
	}
	return renamed
}

// Two renames of one group whose transactions begin in one order and commit in the other. A
// group write begins its transaction before it takes the group writers' lock (lockSessionGroups),
// so the rename that began first can take the lock second and commit last, and the row keeps its
// name. That rename holds the later updated_at, in its answer and in the row: a reader that keeps
// whichever version of a group has the later updated_at, as a client holding a rename's answer
// beside a read of the timeline does, would otherwise keep the version the row no longer holds.
// Stamped with the transaction's start, now(), it held the earlier time of the two.
func TestARenameThatBeganFirstAndCommittedLastHoldsTheLaterUpdatedAt(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	group := createCommittedGroup(t, ctx, pool, "Crew", "s1")

	begin := func(what string) (pgx.Tx, time.Time) {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin %s: %v", what, err)
		}
		t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
		var began time.Time
		if err := tx.QueryRow(ctx, `select now()`).Scan(&began); err != nil {
			t.Fatalf("read when %s began: %v", what, err)
		}
		return tx, began
	}
	beganFirst, beganFirstAt := begin("the rename that begins first")
	beganSecond, beganSecondAt := begin("the rename that begins second")
	if !beganFirstAt.Before(beganSecondAt) {
		t.Fatalf("the renames' transactions began at %s and %s, not one after the other, so this test proves nothing", beganFirstAt, beganSecondAt)
	}

	committedFirst := renameAndCommit(t, ctx, beganSecond, group.ID, "Night crew")
	committedLast := renameAndCommit(t, ctx, beganFirst, group.ID, "Ship crew")

	if !committedLast.UpdatedAt.After(committedFirst.UpdatedAt) {
		t.Fatalf("the rename that committed last answered updated_at %s, not after %s, which the rename it overwrote answered (the transactions began at %s and %s)",
			committedLast.UpdatedAt, committedFirst.UpdatedAt, beganFirstAt, beganSecondAt)
	}
	stored, err := getSessionGroup(ctx, pool, group.ID)
	if err != nil {
		t.Fatalf("read the group: %v", err)
	}
	if stored.Name != "Ship crew" || !stored.UpdatedAt.Equal(committedLast.UpdatedAt) {
		t.Fatalf("the row holds %q at %s, want the last commit's %q at %s", stored.Name, stored.UpdatedAt, "Ship crew", committedLast.UpdatedAt)
	}
}

// The statement's clock is a floor, not the value: a group whose updated_at is already past the
// clock (a clock that stepped back) never moves backwards, and each rename still holds a later
// updated_at than the version before it, one microsecond past it, so no two versions tie. Two
// renames in a row while the stored value is ahead stamp it plus one microsecond, then plus two.
func TestEachRenameOfAGroupAheadOfTheClockHoldsALaterUpdatedAt(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	group := createCommittedGroup(t, ctx, pool, "Crew", "s1")
	var ahead time.Time
	if err := pool.QueryRow(ctx, `
		update delivery_session_groups set updated_at = clock_timestamp() + interval '1 hour' where id = $1::uuid
		returning updated_at
	`, group.ID).Scan(&ahead); err != nil {
		t.Fatalf("move updated_at an hour ahead: %v", err)
	}

	for step, name := range []string{"Ship crew", "Night crew"} {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the rename to %q: %v", name, err)
		}
		renamed := renameAndCommit(t, ctx, tx, group.ID, name)
		want := ahead.Add(time.Duration(step+1) * time.Microsecond)
		if renamed.Name != name || !renamed.UpdatedAt.Equal(want) {
			t.Fatalf("rename %d answered %q at %s, want %q at %s, one microsecond past the version before it", step+1, renamed.Name, renamed.UpdatedAt, name, want)
		}
	}
}
