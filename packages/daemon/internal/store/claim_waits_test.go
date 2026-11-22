package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A claim's wait is no column of claims, so no write of the claim's machine touches it
// (dispatch://INFRA-2040 D1). A launch is the write that moves the claim's generation, and it
// clears the activity columns, since the process that reported them is gone (putClaim); the wait
// row stays as it was declared, with no end, whatever else the launch wrote, a new session
// included. Whether a wait under a new session still stands is the wait's rule's to say
// (workflow.WaitStands), not the store's, so the claim read gives the new session beside the wait.
func TestALaunchOfTheClaimLeavesItsWaitAsItWas(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	records := record.NewStore()
	held := tmuxClaim("legion-legion-legion-209-implementer")
	if err := store.PutClaim(ctx, held); err != nil {
		t.Fatalf("record the implementer's claim: %v", err)
	}
	reported := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	if known, err := store.PutClaimActivity(ctx, held.Token, held.Generation, claim.SessionActivity{At: reported,
		BackgroundWork: claim.BackgroundWork{Running: true, Kinds: []string{"async-job"}}}); err != nil || !known {
		t.Fatalf("record the implementer's activity = %t, %v", known, err)
	}
	declared := record.ClaimWait{ID: "wait-1", Project: held.Project, Token: held.Token, Issue: held.Issue, Role: held.Role,
		Generation: 3, Phase: phase.Implementing, PhaseEntry: 4, TakenEntry: 4, Session: held.Session,
		WaitingOn: "the review of the root's pull request", WakeAt: at(40), DeclaredAt: at(10)}
	if err := store.Tx(ctx, func(tx pgx.Tx) error {
		_, err := records.DeclareWait(ctx, tx, declared)
		return err
	}); err != nil {
		t.Fatalf("declare the implementer's wait: %v", err)
	}

	relaunched := held
	relaunched.Generation++
	relaunched.Session, relaunched.SessionFile = "ses_relaunched", "/state/sessions/relaunched.jsonl"
	relaunched.State = supervise.StateLaunching
	if err := store.PutClaim(ctx, relaunched); err != nil {
		t.Fatalf("record the claim's launch: %v", err)
	}

	var read *record.RoleClaim
	var rows int
	if err := store.Tx(ctx, func(tx pgx.Tx) error {
		claims, err := records.RoleClaims(ctx, tx, []claim.Token{held.Token})
		if err != nil {
			return err
		}
		read = claims[held.Token]
		return tx.QueryRow(ctx, "select count(*) from claim_waits where claim_token = $1", string(held.Token)).Scan(&rows)
	}); err != nil {
		t.Fatalf("read the claim after its launch: %v", err)
	}
	if read == nil {
		t.Fatalf("the record holds no claim of %s after its launch", held.Token)
	}
	if !read.ActivityAt.IsZero() || read.ActivityBackground {
		t.Fatalf("the claim's activity after its launch = %s, background %t, want the report cleared: the fixture's own premise",
			read.ActivityAt, read.ActivityBackground)
	}
	if read.Session != relaunched.Session {
		t.Errorf("the claim's session after its launch = %q, want the launch's %q", read.Session, relaunched.Session)
	}
	if rows != 1 {
		t.Fatalf("claim_waits holds %d rows of the claim after its launch, want the one declared", rows)
	}
	got := read.Wait
	if got == nil {
		t.Fatal("the claim's wait after its launch is gone, want it as it was declared")
	}
	if !got.WakeAt.Equal(declared.WakeAt) || !got.DeclaredAt.Equal(declared.DeclaredAt) || !got.EndedAt.IsZero() {
		t.Fatalf("the claim's wait after its launch is due %s, declared %s, ended %s, want due %s, declared %s, with no end",
			got.WakeAt, got.DeclaredAt, got.EndedAt, declared.WakeAt, declared.DeclaredAt)
	}
	untimed := *got
	untimed.WakeAt, untimed.DeclaredAt = declared.WakeAt, declared.DeclaredAt
	if untimed != declared {
		t.Fatalf("the claim's wait after its launch = %+v, want it as it was declared: %+v", *got, declared)
	}
}
