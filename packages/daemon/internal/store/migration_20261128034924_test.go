package store

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// leftEditsVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const leftEditsVersion = 20261128034924

// The commits in which a stale working copy's update left the edits that no jj command had
// recorded are recorded on the claim's own row (supervise.Claim.LeftEdits), because the task that
// tells the role of them can be a dropped task, a relaunch and a daemon restart away. The round
// trip is the column through putClaim and scanClaim, and the migration is read back under it:
// every row the table already holds takes null.
func TestAStaleAdoptionRecordsTheCommitsOfItsEditsOnTheClaimRow(t *testing.T) {
	t.Parallel()
	t.Run("the record round-trips with the claim, and its absence is null", func(t *testing.T) {
		ctx := context.Background()
		store := migratedStore(t)
		want := tmuxClaim("legion-LEGION-212-implementer")
		want.LeftEdits = supervise.LeftEdits{
			Commits:  []string{"c45ff7e33d4899a2b99db849241fe128115021b0", "9b1e0d6f2a7c4e3b8d5f1a0c6e2b7d4f9a3c8e1b"},
			At:       at(5),
			Delivery: "outbox:41",
		}
		if err := store.PutClaim(ctx, want); err != nil {
			t.Fatalf("put the claim with its record: %v", err)
		}
		sameClaim(t, onlyClaim(t, store), want)

		// A record a turn has told says so, and names the delivery whose confirmation told it.
		want.LeftEdits.Told, want.LeftEdits.ToldBy = true, "outbox:41~2"
		if err := store.PutClaim(ctx, want); err != nil {
			t.Fatalf("put the claim with its told record: %v", err)
		}
		sameClaim(t, onlyClaim(t, store), want)

		// A claim that records none writes null, not an empty record.
		want.LeftEdits = supervise.LeftEdits{}
		if err := store.PutClaim(ctx, want); err != nil {
			t.Fatalf("put the claim with no record: %v", err)
		}
		sameClaim(t, onlyClaim(t, store), want)
		var null bool
		if err := store.Pool().QueryRow(ctx, "select left_edits is null from claims where token = $1",
			string(want.Token)).Scan(&null); err != nil {
			t.Fatalf("read the column: %v", err)
		}
		if !null {
			t.Error("a claim that records no commits wrote a record, want null")
		}
	})

	t.Run("the migration gives every stored claim no record", func(t *testing.T) {
		ctx := context.Background()
		store := emptyStore(t)
		migrateThrough(t, store, leftEditsVersion-1)

		const token = "legion-LEGION-213-implementer"
		if _, err := store.Pool().Exec(ctx, `insert into claims (token, project, tree, issue, role, generation,
			session, session_file, state, failure, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
			values ($1, 'legion', 'LEGION-213', 'LEGION-213', 'implementer', 1, '', '', 'idle', '', 0, 0, 0, 0)`, token); err != nil {
			t.Fatalf("record %s before the migration: %v", token, err)
		}
		_, err := store.Pool().Exec(ctx, `update claims set left_edits = '{}'::jsonb`)
		if err == nil || !strings.Contains(err.Error(), "left_edits") {
			t.Fatalf("writing left_edits before the migration = %v, want the column to be unknown", err)
		}

		if _, err := store.Migrate(ctx); err != nil {
			t.Fatalf("migrate through the record of a stale adoption's commits: %v", err)
		}

		var null bool
		if err := store.Pool().QueryRow(ctx, "select left_edits is null from claims where token = $1", token).Scan(&null); err != nil {
			t.Fatalf("read the column after the migration: %v", err)
		}
		if !null {
			t.Error("the stored claim holds a record after the migration, want null")
		}
		if read := onlyClaim(t, store); len(read.LeftEdits.Commits) != 0 || read.LeftEdits.Told {
			t.Errorf("the stored claim reads the record %+v after the migration, want none", read.LeftEdits)
		}
	})
}
