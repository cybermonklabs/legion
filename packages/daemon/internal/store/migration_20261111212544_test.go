package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// failureCauseVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const failureCauseVersion = 20261111212544

// A failed claim carries the one word for what failed it beside the sentence the daemon already
// keeps, and the moment it failed; a claim relaunching after a failure carries the cause of the
// episode under way, which a launch the runtime refuses fails it under (supervise.Claim's
// FailureCause, FailedAt and PendingCause). All three are persisted, so an agent reading the claim
// after a daemon restart reads the cause the daemon that failed it chose rather than a sentence
// four different events share.
//
// The round trip is the three columns through putClaim and scanClaim: a failed claim with its
// cause and its moment, then the relaunch that clears them and carries a pending cause instead.
// The migration is read back under it: the five sentences this release's predecessors wrote become
// the four words that can be proved of them.
func TestAFailedClaimCarriesItsCause(t *testing.T) {
	t.Run("the cause, the moment and the pending cause round-trip", func(t *testing.T) {
		ctx := context.Background()
		store := migratedStore(t)
		want := tmuxClaim("legion-LEGION-209-failed")
		want.State = supervise.StateFailed
		want.Failure = "launch failures ran out"
		want.FailureCause = supervise.CauseDied
		want.FailedAt = at(11)
		if err := store.PutClaim(ctx, want); err != nil {
			t.Fatalf("put the failed claim: %v", err)
		}
		sameClaim(t, onlyClaim(t, store), want)

		// The launch that runs the claim again clears the sentence, the cause and the moment
		// together (supervise's launchNow), and the relaunch of a claim whose process died carries
		// that death as the cause a refused launch would fail it under.
		want.State = supervise.StateLaunching
		want.Failure, want.FailureCause, want.FailedAt = "", "", time.Time{}
		want.PendingCause = supervise.CauseDied
		if err := store.PutClaim(ctx, want); err != nil {
			t.Fatalf("put the relaunching claim: %v", err)
		}
		sameClaim(t, onlyClaim(t, store), want)

		// The agent comes up, which is what ends the episode the pending cause names.
		want.State = supervise.StateReady
		want.PendingCause = ""
		if err := store.PutClaim(ctx, want); err != nil {
			t.Fatalf("put the claim that came up: %v", err)
		}
		sameClaim(t, onlyClaim(t, store), want)
	})

	t.Run("the migration reads what the stored sentences prove", func(t *testing.T) {
		ctx := context.Background()
		store := emptyStore(t)
		migrateThrough(t, store, failureCauseVersion-1)

		// The five sentences the daemon writes today, and what each proves of the build that
		// wrote it. The two that begin alike are the launch pair: the colon-and-space form is
		// launchNow's alone, over a launch the runtime refused, while the bare sentence is four
		// different events under one wording (supervise's relaunchAfterFailure) and names none of
		// them, so it takes unrecorded as the deaths sentence does.
		seeds := []struct{ token, state, failure, cause string }{{
			token:   "legion-LEGION-208-architect",
			state:   "failed",
			failure: "prompt retirements ran out",
			cause:   "prompt_retires_exhausted",
		}, {
			token:   "legion-LEGION-209-planner",
			state:   "failed",
			failure: "a provider error that cannot clear ended this claim's turn: the model is gone",
			cause:   "provider_error",
		}, {
			token:   "legion-LEGION-210-implementer",
			state:   "failed",
			failure: "launch failures ran out: tmux refused the window",
			cause:   "launch_refused",
		}, {
			token:   "legion-LEGION-211-tester",
			state:   "failed",
			failure: "launch failures ran out",
			cause:   "unrecorded",
		}, {
			token:   "legion-LEGION-212-reviewer",
			state:   "failed",
			failure: "deaths with work outstanding ran out",
			cause:   "unrecorded",
		}, {
			// The reachable row of another state: an operator stopped a claim that had already
			// failed, which retires it with its sentence standing, and R2 backfills a claim that
			// holds a sentence whatever state it stands in.
			token:   "legion-LEGION-213-merger",
			state:   "retired",
			failure: "deaths with work outstanding ran out",
			cause:   "unrecorded",
		}, {
			// A state no live path leaves a sentence in, seeded to prove the migration reads the
			// sentence and not the state.
			token:   "legion-LEGION-214-tester",
			state:   "suspended",
			failure: "launch failures ran out: tmux refused the window",
			cause:   "launch_refused",
		}, {
			// A claim holding no sentence is left at the empty default, whatever its state: the
			// backfill reads the sentence, and this one has none to read.
			token:   "legion-LEGION-215-reviewer",
			state:   "ready",
			failure: "",
			cause:   "",
		}}
		for _, seed := range seeds {
			if _, err := store.Pool().Exec(ctx, `insert into claims (token, project, tree, issue, role, generation,
				session, session_file, state, failure, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
				values ($1, 'legion', 'LEGION-208', 'LEGION-208', 'implementer', 1, '', '', $2, $3, 0, 0, 0, 0)`,
				seed.token, seed.state, seed.failure); err != nil {
				t.Fatalf("record %s before the migration: %v", seed.token, err)
			}
		}
		for _, column := range []string{"failure_cause = 'died'", "failed_at = now()", "pending_cause = 'died'"} {
			_, err := store.Pool().Exec(ctx, "update claims set "+column)
			if err == nil || !strings.Contains(err.Error(), strings.SplitN(column, " ", 2)[0]) {
				t.Fatalf("writing %s before the migration = %v, want the column to be unknown", column, err)
			}
			t.Logf("before the migration the column is unknown: %v", err)
		}

		if _, err := store.Migrate(ctx); err != nil {
			t.Fatalf("migrate through the failure cause: %v", err)
		}

		for _, seed := range seeds {
			var cause, pending string
			var failedAt *time.Time
			if err := store.Pool().QueryRow(ctx,
				"select failure_cause, failed_at, pending_cause from claims where token = $1", seed.token,
			).Scan(&cause, &failedAt, &pending); err != nil {
				t.Fatalf("read %s after the migration: %v", seed.token, err)
			}
			if cause != seed.cause {
				t.Errorf("the claim that failed with %q reads cause %q, want %q", seed.failure, cause, seed.cause)
			}
			// The release this upgrades from recorded the moment nowhere, and no cause was
			// pending on a claim it had already failed: a null reads as absent where an invented
			// time would read as evidence.
			if failedAt != nil {
				t.Errorf("the claim that failed with %q reads failed_at %s, want null: the daemon never recorded it", seed.failure, *failedAt)
			}
			if pending != "" {
				t.Errorf("the claim that failed with %q reads pending cause %q, want none", seed.failure, pending)
			}
		}
	})
}

// INFRA-1413 selection probe, never merged.
