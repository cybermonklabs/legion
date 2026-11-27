package store

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// crossings reads the two times upsertClaim stamps on a claim: when it last began to hold its role,
// the rule the daemon publishes its session as its role's holder by, and when it last stopped.
func crossings(t *testing.T, store *Store, token claim.Token) (published, unpublished *time.Time) {
	t.Helper()
	if err := store.Pool().QueryRow(context.Background(),
		`select published_since, unpublished_since from claims where token = $1`, string(token)).Scan(&published, &unpublished); err != nil {
		t.Fatalf("read the crossings of %s: %v", token, err)
	}
	return published, unpublished
}

// Each crossing of the states and suspension sources that hold a claim's role, the rule the daemon
// publishes its session as its role's holder by (supervise.HoldsRole), writes its own column, and
// a move between two that hold it or between two that do not writes neither, so the repair can
// judge a session's write by the holders Dispatch was shown when it recorded the write
// (record.RoleHolderAt.PublishedAt). An insert that holds the role is a crossing in, a queued
// claim's included, and an insert that does not writes neither. The idle release's suspension and
// the launch that resumes the session it kept hold the role, so neither writes anything. A move
// out writes unpublished_since and keeps the entry; a move in after it writes published_since
// anew, after that leave, and keeps the leave: a suspension by another source is a move out, the
// launch after it a move in, and a failure a move out again. idle to working writes neither.
func TestAClaimCrossingWritesItsPublicationTime(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	c := tmuxClaim("legion-LEGION-209-implementer")
	putBy := func(state supervise.ClaimState, by supervise.SuspendSource) (published, unpublished *time.Time) {
		t.Helper()
		c.State, c.SuspendedBy = state, by
		if err := store.PutClaim(ctx, c); err != nil {
			t.Fatalf("put the claim at %s, suspended by %q: %v", state, by, err)
		}
		return crossings(t, store, c.Token)
	}
	put := func(state supervise.ClaimState) (published, unpublished *time.Time) {
		t.Helper()
		return putBy(state, "")
	}

	entered, unpublished := put(supervise.StateIdle)
	if entered == nil || unpublished != nil {
		t.Fatalf("an insert at idle left published %v, unpublished %v; want the insert's entry and no leave", entered, unpublished)
	}
	if published, unpublished := put(supervise.StateWorking); !sameInstant(published, entered) || unpublished != nil {
		t.Fatalf("idle to working left published %v, unpublished %v; want %v kept and no leave", published, unpublished, entered)
	}
	for _, state := range []supervise.ClaimState{supervise.StateSuspended, supervise.StateLaunching} {
		if published, unpublished := putBy(state, supervise.SuspendedByReleased); !sameInstant(published, entered) || unpublished != nil {
			t.Fatalf("the idle release's move to %s left published %v, unpublished %v; want %v kept and no leave: the released claim keeps its role", state, published, unpublished, entered)
		}
	}
	if published, unpublished := put(supervise.StateWorking); !sameInstant(published, entered) || unpublished != nil {
		t.Fatalf("the resumed launch to working left published %v, unpublished %v; want %v kept and no leave", published, unpublished, entered)
	}
	published, left := put(supervise.StateSuspended)
	if !sameInstant(published, entered) || left == nil || !left.After(*entered) {
		t.Fatalf("the move out to suspended left published %v, unpublished %v; want %v kept and a leave stamped after it", published, left, entered)
	}
	relaunched, unpublished := put(supervise.StateLaunching)
	if relaunched == nil || !relaunched.After(*left) || !sameInstant(unpublished, left) {
		t.Fatalf("the move in to launching left published %v, unpublished %v; want an entry stamped after the leave %v, and the leave kept", relaunched, unpublished, left)
	}
	published, failed := put(supervise.StateFailed)
	if !sameInstant(published, relaunched) || failed == nil || !failed.After(*relaunched) {
		t.Fatalf("the move out to failed left published %v, unpublished %v; want %v kept and a leave stamped after it", published, failed, relaunched)
	}
	reentered, unpublished := put(supervise.StateRegistered)
	if reentered == nil || !reentered.After(*failed) || !sameInstant(unpublished, failed) {
		t.Fatalf("the move in to registered left published %v, unpublished %v; want an entry stamped after the leave %v, and the leave kept", reentered, unpublished, failed)
	}

	queued := tmuxClaim("legion-LEGION-211-implementer")
	queued.Issue, queued.Session, queued.SessionFile, queued.State = "LEGION-211", "ses_queued", "/state/sessions/queued.jsonl", supervise.StateQueued
	if err := store.PutClaim(ctx, queued); err != nil {
		t.Fatalf("put a claim first written queued: %v", err)
	}
	if published, unpublished := crossings(t, store, queued.Token); published == nil || unpublished != nil {
		t.Fatalf("a claim first written queued holds published %v, unpublished %v; want the insert's entry and no leave: a queued claim holds its role", published, unpublished)
	}

	held := tmuxClaim("legion-LEGION-212-implementer")
	held.Issue, held.Session, held.SessionFile = "LEGION-212", "ses_held", "/state/sessions/held.jsonl"
	held.State, held.SuspendedBy = supervise.StateSuspended, supervise.SuspendedByOperator
	if err := store.PutClaim(ctx, held); err != nil {
		t.Fatalf("put a claim first written suspended by the operator: %v", err)
	}
	if published, unpublished := crossings(t, store, held.Token); published != nil || unpublished != nil {
		t.Fatalf("a claim first written suspended by the operator holds published %v, unpublished %v; want neither: it has crossed nothing", published, unpublished)
	}
}
