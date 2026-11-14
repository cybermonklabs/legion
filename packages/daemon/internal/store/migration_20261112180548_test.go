package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// leakCheckCauseVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const leakCheckCauseVersion = 20261112180548

// A claim whose launch tries the runtime refused because the shared public leak check could not
// be installed carries the refusal's sentence beside its failure record (supervise.Claim's
// LeakCheckCause). It is persisted, so an agent reading the claim after a daemon restart reads
// which check refused it, and so a claim restored from the store between two refused tries carries
// the cause of the one before.
//
// The round trip is the column through putClaim and scanClaim: a claim failed on the refusal,
// then the retry that clears the sentence with the failure record. The migration is read back
// under it: every row the table already holds takes the empty default.
func TestALeakCheckRefusedClaimCarriesItsCause(t *testing.T) {
	const sentence = "the shared public leak check could not be installed: the pod's launcher did not answer the install of leak-check-v3 within 2m0s"

	t.Run("the cause round-trips beside the failure record", func(t *testing.T) {
		ctx := context.Background()
		store := migratedStore(t)
		want := tmuxClaim("legion-LEGION-209-refused")
		want.State = supervise.StateFailed
		want.Failure = "launch failures ran out: launch legion-LEGION-209-refused: start through its authenticated launcher: " + sentence
		want.FailureCause = supervise.CauseLaunchRefused
		want.FailedAt = at(11)
		want.LeakCheckCause = sentence
		if err := store.PutClaim(ctx, want); err != nil {
			t.Fatalf("put the refused claim: %v", err)
		}
		sameClaim(t, onlyClaim(t, store), want)

		// The retry that revives the claim clears the cause with the failure record
		// (supervise's retry).
		want.State = supervise.StateLaunching
		want.Failure, want.FailureCause, want.FailedAt, want.LeakCheckCause = "", "", time.Time{}, ""
		if err := store.PutClaim(ctx, want); err != nil {
			t.Fatalf("put the relaunching claim: %v", err)
		}
		sameClaim(t, onlyClaim(t, store), want)
	})

	t.Run("the migration gives every stored claim no cause", func(t *testing.T) {
		ctx := context.Background()
		store := emptyStore(t)
		migrateThrough(t, store, leakCheckCauseVersion-1)

		const token = "legion-LEGION-210-implementer"
		if _, err := store.Pool().Exec(ctx, `insert into claims (token, project, tree, issue, role, generation,
			session, session_file, state, failure, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
			values ($1, 'legion', 'LEGION-208', 'LEGION-208', 'implementer', 1, '', '', 'failed',
			'launch failures ran out: tmux refused the window', 3, 0, 0, 0)`, token); err != nil {
			t.Fatalf("record %s before the migration: %v", token, err)
		}
		_, err := store.Pool().Exec(ctx, "update claims set leak_check_cause = 'x'")
		if err == nil || !strings.Contains(err.Error(), "leak_check_cause") {
			t.Fatalf("writing leak_check_cause before the migration = %v, want the column to be unknown", err)
		}

		if _, err := store.Migrate(ctx); err != nil {
			t.Fatalf("migrate through the leak-check cause: %v", err)
		}

		var cause string
		if err := store.Pool().QueryRow(ctx,
			"select leak_check_cause from claims where token = $1", token,
		).Scan(&cause); err != nil {
			t.Fatalf("read %s after the migration: %v", token, err)
		}
		if cause != "" {
			t.Errorf("the stored claim reads the leak-check cause %q after the migration, want none", cause)
		}
	})
}
