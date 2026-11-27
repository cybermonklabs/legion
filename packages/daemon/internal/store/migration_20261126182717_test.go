package store

import (
	"context"
	"reflect"
	"slices"
	"testing"
)

// claimCyberPolicySessionsVersion is 20261126182717_claim_cyber_policy_sessions, the migration
// that adds claims.cyber_policy_sessions.
const claimCyberPolicySessionsVersion = 20261126182717

// A claim keeps every session whose content OpenAI refused for cyber policy across every write
// (supervise.Claim.CyberPolicySessions): the store writes the stored list with each session of
// the written one it lacks added after it, so a write of a copy of the claim read before a session
// was recorded, which names none, drops none, and the list reads back in the order it was
// recorded. A claim that records none reads back with none, as its machine holds it.
func TestAClaimsCyberPolicySessionsOutliveEveryWrite(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-209-implementer")
	if err := store.PutClaim(ctx, c); err != nil {
		t.Fatalf("put the claim: %v", err)
	}
	if got := onlyClaim(t, store).CyberPolicySessions; got != nil {
		t.Fatalf("a claim that recorded no refused session reads back %q, want none", got)
	}
	for _, step := range []struct {
		name          string
		written, want []string
	}{
		{"the failure records the refused session", []string{"ses_implementer"}, []string{"ses_implementer"}},
		{"a write of a copy read before the failure", nil, []string{"ses_implementer"}},
		{"a later refusal of the new session", []string{"ses_implementer", "ses_new"}, []string{"ses_implementer", "ses_new"}},
		{"a write of a copy that names the new session alone", []string{"ses_new"}, []string{"ses_implementer", "ses_new"}},
	} {
		c.CyberPolicySessions = step.written
		if err := store.PutClaim(ctx, c); err != nil {
			t.Fatalf("%s: put the claim: %v", step.name, err)
		}
		if got := onlyClaim(t, store).CyberPolicySessions; !slices.Equal(got, step.want) {
			t.Fatalf("%s: the claim reads back the refused sessions %q, want %q", step.name, got, step.want)
		}
	}
}

// The migration records the session a retry of the release before forgot after OpenAI refused its
// content for cyber policy: the claim's abandoned session, whose sentence names the refusal in
// classifyProviderError's words. It records too the abandoned session of a claim held now, failed
// or retired, that records no session, which that release's forget-session left behind under the
// asker's sentence after the failure: the retry that ends the hold forgets no session then. It
// reads that hold as supervise.DecodeCause does, so a held_for a rollback left beside another
// cause holds nothing, and it takes only a session abandoned after the failure, as supervise's
// holdCyberPolicySession does, so a session abandoned before it, or beside a failure whose moment
// no row recorded, records none. A session abandoned for another cause by a claim held for none, a
// claim that abandoned none, and a claim held now that still records its session record none: the
// hold covers every conversation of the held claim until the retry that ends it records the
// session it forgets (supervise's forgetSession).
func TestTheMigrationRecordsTheSessionAnEarlierRetryForgotAfterACyberPolicyRefusal(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, claimCyberPolicySessionsVersion-1)
	const forgotten = `{"session":"ses_forgotten","why":"an operator cleared it with ` + "`legion claims forget-session`" +
		` or ` + "`legion claims retry --fresh-session`" + `, so this agent starts in a new session","at":"2026-11-25T13:00:00Z"}`
	const failed = "2026-11-25T12:00:00Z"
	seeds := []struct{ token, state, session, failureCause, heldFor, abandoned, failedAt, want string }{
		{"legion-LEGION-208-implementer", "launching", "", "", "",
			`{"session":"ses_refused","why":"a provider error that cannot clear ended this claim's turn: OpenAI's cyber_policy refusal of this session's content (HTTP 400)","at":"2026-11-25T12:00:00Z"}`,
			"", `(legion-LEGION-208-implementer,{ses_refused})`},
		{"legion-LEGION-209-implementer", "launching", "", "", "",
			`{"session":"ses_died","why":"deaths with work outstanding ran out","at":"2026-11-25T12:00:00Z"}`,
			"", `(legion-LEGION-209-implementer,{})`},
		{"legion-LEGION-210-implementer", "idle", "ses_running", "", "", "", "", `(legion-LEGION-210-implementer,{})`},
		{"legion-LEGION-211-implementer", "failed", "ses_held", "provider_error", "cyber_policy", "", failed, `(legion-LEGION-211-implementer,{})`},
		{"legion-LEGION-212-implementer", "failed", "", "provider_error", "cyber_policy", forgotten, failed, `(legion-LEGION-212-implementer,{ses_forgotten})`},
		{"legion-LEGION-213-implementer", "retired", "", "provider_error", "cyber_policy", forgotten, failed, `(legion-LEGION-213-implementer,{ses_forgotten})`},
		{"legion-LEGION-214-implementer", "failed", "", "provider_error", "", forgotten, failed, `(legion-LEGION-214-implementer,{})`},
		// A held_for the release before 20261125223008 left standing after a rollback, beside the
		// cause it wrote for a later failure: no hold (supervise.DecodeCause).
		{"legion-LEGION-215-implementer", "failed", "", "died", "cyber_policy", forgotten, failed, `(legion-LEGION-215-implementer,{})`},
		// A session abandoned before the failure, and one beside a failure whose moment no row
		// recorded: neither is the session refused (supervise's holdCyberPolicySession).
		{"legion-LEGION-216-implementer", "failed", "", "provider_error", "cyber_policy", forgotten, "2026-11-25T14:00:00Z", `(legion-LEGION-216-implementer,{})`},
		{"legion-LEGION-217-implementer", "failed", "", "provider_error", "cyber_policy", forgotten, "", `(legion-LEGION-217-implementer,{})`},
	}
	for _, seed := range seeds {
		if _, err := store.Pool().Exec(ctx, `insert into claims (token, project, tree, issue, role, generation,
			session, session_file, state, failure, launch_failures, prompt_failures, prompt_retires, uncertain_streak,
			failure_cause, held_for, abandoned_session, failed_at)
			values ($1, 'legion', 'LEGION-208', 'LEGION-208', 'implementer', 1, $2, '', $3, '', 0, 0, 0, 0, $4, $5, nullif($6, '')::jsonb,
			nullif($7, '')::timestamptz)`,
			seed.token, seed.session, seed.state, seed.failureCause, seed.heldFor, seed.abandoned, seed.failedAt); err != nil {
			t.Fatalf("record %s before the migration: %v", seed.token, err)
		}
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through cyber_policy_sessions: %v", err)
	}

	got := rowsAsText(t, store, `select row(token, cyber_policy_sessions)::text from claims order by token`)
	var want []string
	for _, seed := range seeds {
		want = append(want, seed.want)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the claims after the migration (token, cyber_policy_sessions) = %v, want %v", got, want)
	}
}
