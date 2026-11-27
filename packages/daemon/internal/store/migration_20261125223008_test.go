package store

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// heldForVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const heldForVersion = 20261125223008

// releaseBeforeClaimSelect is the claim store's claimSelect at 04887f0de34a, origin/main when
// migration 20261125223008_claim_held_for was written: the claim read of the release before this
// one, the one a rollback runs, word for word. It names every column of claims that release
// reads, and held_for is none of them. It is run here on the head of this branch's schema, where
// migration 20261113175649_task_numbers has dropped three columns of pending_task_deliveries
// (delivered_at, marked_by and prompted) that its way back, which a rollback runs first, puts
// back; the select names those three no more, and every column of claims it names is as that
// release reads it, which is all the cases below read.
const releaseBeforeClaimSelect = `select c.token, c.project, c.tree, c.issue, c.role, c.generation, c.tree_epoch, c.session,
	c.session_file, c.locator, c.state, c.failure, c.pod_end, c.failure_cause, c.failed_at, c.pending_cause, c.leak_check_cause,
	c.launch_failures, c.deaths, c.prompt_failures, c.prompt_retires,
	c.boot_token_hash, c.capability_hash, c.uncertain_streak, c.last_start_row, c.serving_generation,
	c.given_entry, c.served_entry, c.handshake_entry, c.handshake_generation, c.handshake_queued_at,
	c.handshake_confirmed_at, c.serving_check_look, c.serving_check_opened_at,
	c.agent_secrets_enrollment, c.agent_secrets_enrollment_incarnation, c.suspended_by,
	c.plugin_contract, c.plugin_relaunch_for, c.agent_resume_generation, c.delivered_rows,
	c.last_turn_start, c.last_turn_end,
	c.last_death_cause, c.last_death_at, c.last_death_digest,
	c.last_start_failure, c.last_start_failure_at, c.last_start_failure_digest, c.last_ready,
	c.turn_outcomes, c.abandoned_session, c.ended_work,
	c.update_round, c.update_reasons, c.update_opened_at, c.update_tasked_at, c.update_signalled_at,
	c.update_head, c.update_told_at,
	c.update_left_round, c.provider_error_run, c.death_charged,
	c.step_at, c.turn_started_at, c.step_tool, c.step_call_id, c.step_args_bytes,
	c.step_args_hash, c.step_started_at, c.step_ended_at,
	c.activity_at, c.activity_background, c.activity_kinds, c.activity_next_reminder_at,
	d.delivery_id, d.task, d.phase, d.generation, d.entry, d.check_look, d.check_opened_at,
	d.queued_at, d.confirmed_at, d.interrupted, d.out_of_memory,
	d.send_failures, d.last_send_failure_at, d.last_send_error,
	d.kept_decisions, d.provider_error_retries, d.provider_error_note, d.design_news, d.carried_news,
	coalesce(d.carried_ran, false), coalesce(d.news_only, false), d.round_named
	from claims c left join pending_task_deliveries d on d.claim_token = c.token`

// releaseBeforeCauses runs releaseBeforeClaimSelect for token and answers the two cause columns
// that release decodes (supervise.FailureCause of each), read by name off the row it answers, and
// the names of every column it answered.
func releaseBeforeCauses(t *testing.T, store *Store, token string) (failureCause, pendingCause string, columns []string) {
	t.Helper()
	rows, err := store.Pool().Query(context.Background(), releaseBeforeClaimSelect+` where c.token = $1`, token)
	if err != nil {
		t.Fatalf("the release before's claim read of %s: %v", token, err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("the release before's claim read of %s answered no row: %v", token, rows.Err())
	}
	values, err := rows.Values()
	if err != nil {
		t.Fatalf("decode the release before's claim read of %s: %v", token, err)
	}
	found := 0
	for i, field := range rows.FieldDescriptions() {
		columns = append(columns, field.Name)
		switch field.Name {
		case "failure_cause":
			failureCause, found = values[i].(string), found+1
		case "pending_cause":
			pendingCause, found = values[i].(string), found+1
		}
	}
	if found != 2 {
		t.Fatalf("the release before's claim read of %s named %d of failure_cause and pending_cause, want both: %v", token, found, columns)
	}
	return failureCause, pendingCause, columns
}

// storedCauses is the row of token as SQL holds it: failure_cause, pending_cause and held_for.
func storedCauses(t *testing.T, store *Store, token string) []string {
	t.Helper()
	return rowsAsText(t, store, `select row(failure_cause, pending_cause, held_for)::text from claims where token = '`+token+`'`)
}

// A claim held for OpenAI's cyber-policy refusal reads back as held (supervise.CauseCyberPolicy
// in FailureCause or PendingCause), while its two cause columns hold `provider_error`, a word the
// release before knows, and held_for holds `cyber_policy` (the claim store's storedCause and
// heldFor, and supervise.DecodeCause). The release before reads the same row with the statement
// it ships and decodes `provider_error`; the claim failed under `provider_error` alone, with
// held_for empty, is the control that reads `provider_error` back on this release too.
func TestAClaimHeldForCyberPolicyStoresOnlyWordsTheReleaseBeforeKnows(t *testing.T) {
	t.Parallel()
	const token = "legion-LEGION-209-implementer"
	held := func() supervise.Claim {
		c := tmuxClaim(token)
		c.State = supervise.StateFailed
		c.Failure = "a provider error that cannot clear ended this claim's turn: OpenAI's cyber_policy refusal of this session's content"
		c.FailureCause = supervise.CauseCyberPolicy
		c.FailedAt = at(11)
		return c
	}

	t.Run("a held claim round-trips and its cause columns hold provider_error", func(t *testing.T) {
		ctx := context.Background()
		store := migratedStore(t)
		want := held()
		if err := store.PutClaim(ctx, want); err != nil {
			t.Fatalf("put the held claim: %v", err)
		}
		sameClaim(t, onlyClaim(t, store), want)
		if got, wantRow := storedCauses(t, store, token), []string{`(provider_error,"",cyber_policy)`}; !reflect.DeepEqual(got, wantRow) {
			t.Fatalf("the held claim's row (failure_cause, pending_cause, held_for) = %v, want %v", got, wantRow)
		}

		// A pending cause of cyber_policy maps the same way, through the same column: the store
		// does not depend on which cause field holds it.
		want.State = supervise.StateLaunching
		want.Failure, want.FailureCause, want.FailedAt = "", "", time.Time{}
		want.PendingCause = supervise.CauseCyberPolicy
		if err := store.PutClaim(ctx, want); err != nil {
			t.Fatalf("put the claim with a pending cyber_policy: %v", err)
		}
		sameClaim(t, onlyClaim(t, store), want)
		if got, wantRow := storedCauses(t, store, token), []string{`("",provider_error,cyber_policy)`}; !reflect.DeepEqual(got, wantRow) {
			t.Fatalf("the pending claim's row = %v, want %v", got, wantRow)
		}

		// The control: a claim failed under provider_error itself writes held_for empty, and reads
		// back provider_error.
		want.State = supervise.StateFailed
		want.PendingCause = ""
		want.Failure, want.FailureCause, want.FailedAt = "a provider error that cannot clear ended this claim's turn: 400 bad request", supervise.CauseProviderError, at(12)
		if err := store.PutClaim(ctx, want); err != nil {
			t.Fatalf("put the claim failed under provider_error: %v", err)
		}
		sameClaim(t, onlyClaim(t, store), want)
		if got, wantRow := storedCauses(t, store, token), []string{`(provider_error,"","")`}; !reflect.DeepEqual(got, wantRow) {
			t.Fatalf("the provider_error claim's row = %v, want %v", got, wantRow)
		}
	})

	t.Run("the release before reads provider_error and no held_for", func(t *testing.T) {
		ctx := context.Background()
		store := migratedStore(t)
		if err := store.PutClaim(ctx, held()); err != nil {
			t.Fatalf("put the held claim: %v", err)
		}
		failureCause, pendingCause, columns := releaseBeforeCauses(t, store, token)
		if failureCause != string(supervise.CauseProviderError) || pendingCause != "" {
			t.Fatalf("the release before reads failure_cause %q and pending_cause %q, want %q and none", failureCause, pendingCause, supervise.CauseProviderError)
		}
		for _, column := range columns {
			if column == "held_for" {
				t.Fatalf("the release before's claim read names held_for: %v", columns)
			}
		}
	})

	t.Run("after a rollback the cause column decides and held_for only confirms it", func(t *testing.T) {
		ctx := context.Background()
		store := migratedStore(t)
		if err := store.PutClaim(ctx, held()); err != nil {
			t.Fatalf("put the held claim: %v", err)
		}
		// The release before's writes name no held_for (its upsertClaim), so each leaves it as it
		// stands. A claim it left alone is held again after the roll-forward.
		if got := onlyClaim(t, store); got.FailureCause != supervise.CauseCyberPolicy {
			t.Fatalf("the claim the release before left alone reads cause %q, want %q", got.FailureCause, supervise.CauseCyberPolicy)
		}
		for _, step := range []struct {
			name, statement string
			want            supervise.FailureCause
		}{
			{"relaunched", `update claims set state = 'launching', failure = '', failure_cause = '', failed_at = null where token = $1`, ""},
			{"failed under died", `update claims set state = 'failed', failure = 'deaths with work outstanding ran out', failure_cause = 'died', failed_at = now() where token = $1`, supervise.CauseDied},
		} {
			if _, err := store.Pool().Exec(ctx, step.statement, token); err != nil {
				t.Fatalf("the release before's write of the %s claim: %v", step.name, err)
			}
			if got := onlyClaim(t, store); got.FailureCause != step.want || got.PendingCause != "" {
				t.Errorf("the claim the release before %s reads cause %q and pending cause %q, want %q and none: held_for %v stands from before",
					step.name, got.FailureCause, got.PendingCause, step.want, storedCauses(t, store, token))
			}
		}
	})

	t.Run("the migration moves cyber_policy out of the cause columns", func(t *testing.T) {
		ctx := context.Background()
		store := emptyStore(t)
		migrateThrough(t, store, heldForVersion-1)
		// The rows a build of this change could have written before held_for existed, and two
		// any release writes: provider_error itself, and no cause at all.
		seeds := []struct{ token, failureCause, pendingCause, want string }{
			{"legion-LEGION-208-architect", "cyber_policy", "", `(legion-LEGION-208-architect,provider_error,"",cyber_policy)`},
			{"legion-LEGION-209-planner", "", "cyber_policy", `(legion-LEGION-209-planner,"",provider_error,cyber_policy)`},
			{"legion-LEGION-210-implementer", "provider_error", "", `(legion-LEGION-210-implementer,provider_error,"","")`},
			{"legion-LEGION-211-tester", "", "died", `(legion-LEGION-211-tester,"",died,"")`},
		}
		for _, seed := range seeds {
			if _, err := store.Pool().Exec(ctx, `insert into claims (token, project, tree, issue, role, generation,
				session, session_file, state, failure, launch_failures, prompt_failures, prompt_retires, uncertain_streak,
				failure_cause, pending_cause)
				values ($1, 'legion', 'LEGION-208', 'LEGION-208', 'implementer', 1, '', '', 'failed', 'failed', 0, 0, 0, 0, $2, $3)`,
				seed.token, seed.failureCause, seed.pendingCause); err != nil {
				t.Fatalf("record %s before the migration: %v", seed.token, err)
			}
		}
		const column = `select count(*)::text from information_schema.columns where table_name = 'claims' and column_name = 'held_for'`
		if got := rowsAsText(t, store, column); !reflect.DeepEqual(got, []string{"0"}) {
			t.Fatalf("the column before the migration = %v, want none", got)
		}

		if _, err := store.Migrate(ctx); err != nil {
			t.Fatalf("migrate through held_for: %v", err)
		}

		got := rowsAsText(t, store, `select row(token, failure_cause, pending_cause, held_for)::text from claims order by token`)
		var want []string
		for _, seed := range seeds {
			want = append(want, seed.want)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("the claims after the migration (token, failure_cause, pending_cause, held_for) = %v, want %v", got, want)
		}
		claims, err := store.Claims(ctx)
		if err != nil {
			t.Fatalf("read the migrated claims: %v", err)
		}
		wantCauses := []string{"cyber_policy/", "/cyber_policy", "provider_error/", "/died"}
		var causes []string
		for _, c := range claims {
			causes = append(causes, string(c.FailureCause)+"/"+string(c.PendingCause))
		}
		if !reflect.DeepEqual(causes, wantCauses) {
			t.Fatalf("the migrated claims read (failure cause/pending cause) %v, want %v", causes, wantCauses)
		}
	})
}
