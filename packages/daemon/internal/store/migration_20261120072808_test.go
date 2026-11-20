package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// lastStartFailureAtVersion is the version of the migration this file tests, so the test can
// migrate through everything before it whatever lands beside it.
const lastStartFailureAtVersion = 20261120072808

// A claim's failed start is stored with the moment it failed (supervise.Claim's
// LastStartFailureAt), so a reader after a daemon restart reads how old the failure is rather
// than taking a sentence of days ago for a current one.
//
// The round trip is the column through putClaim and scanClaim: a failed start with its moment,
// then a later write of the same claim, which keeps both. The migration is read under it: every
// row the table already holds takes null, the moment unknown, and nothing makes one up, since no
// release before this one kept it.
func TestAFailedStartIsStoredWithTheMomentItFailed(t *testing.T) {
	const sentence = "launch legion-LEGION-209-implementer: start through its authenticated launcher: the shared public leak check could not be installed: the served check could not be run: waitid: no child processes"

	t.Run("the moment round-trips beside the sentence", func(t *testing.T) {
		ctx := context.Background()
		store := migratedStore(t)
		want := tmuxClaim("legion-LEGION-209-implementer")
		want.State = supervise.StateLaunching
		want.LastStartFailure, want.LastStartFailureAt = sentence, at(11)
		if err := store.PutClaim(ctx, want); err != nil {
			t.Fatalf("put the claim whose start failed: %v", err)
		}
		sameClaim(t, onlyClaim(t, store), want)

		// The next start succeeds and the claim is written again: the failure and its moment
		// stand as they were, since only another failed start replaces them.
		want.State = supervise.StateReady
		if err := store.PutClaim(ctx, want); err != nil {
			t.Fatalf("put the claim that came up: %v", err)
		}
		got := onlyClaim(t, store)
		sameClaim(t, got, want)
		if !got.LastStartFailureAt.Equal(at(11)) {
			t.Errorf("after the ready the claim records the failed start at %s, want %s", got.LastStartFailureAt, at(11))
		}
	})

	t.Run("the migration gives every stored failure no moment", func(t *testing.T) {
		ctx := context.Background()
		store := emptyStore(t)
		migrateThrough(t, store, lastStartFailureAtVersion-1)

		const token = "legion-LEGION-210-implementer"
		if _, err := store.Pool().Exec(ctx, `insert into claims (token, project, tree, issue, role, generation,
			session, session_file, state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, last_start_failure)
			values ($1, 'legion', 'LEGION-208', 'LEGION-210', 'implementer', 1, '', '', 'idle', 0, 0, 0, 0, $2)`,
			token, sentence); err != nil {
			t.Fatalf("record %s before the migration: %v", token, err)
		}
		_, err := store.Pool().Exec(ctx, "update claims set last_start_failure_at = now()")
		if err == nil || !strings.Contains(err.Error(), "last_start_failure_at") {
			t.Fatalf("writing last_start_failure_at before the migration = %v, want the column to be unknown", err)
		}

		if _, err := store.Migrate(ctx); err != nil {
			t.Fatalf("migrate through the failed start's moment: %v", err)
		}

		var moment *time.Time
		if err := store.Pool().QueryRow(ctx,
			"select last_start_failure_at from claims where token = $1", token,
		).Scan(&moment); err != nil {
			t.Fatalf("read %s after the migration: %v", token, err)
		}
		if moment != nil {
			t.Errorf("the stored failure reads the moment %s after the migration, want none: no release before kept it", moment)
		}
		got := onlyClaim(t, store)
		if got.LastStartFailure != sentence || !got.LastStartFailureAt.IsZero() {
			t.Errorf("the stored claim reads the failed start %q at %s, want %q with no moment", got.LastStartFailure, got.LastStartFailureAt, sentence)
		}
	})
}
