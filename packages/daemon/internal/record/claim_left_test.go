package record

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// sameClaimLeft is whether two entries of issues.claim_left say the same: the same holder and
// reason, at the same moment, read back from jsonb in whatever location it hands back.
func sameClaimLeft(a, b *ClaimLeft) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Holder == b.Holder && a.Reason == b.Reason && a.At.Equal(b.At)
}

// issues.claim_left is the outcome of the newest claim release that judged the issue's holder and
// left its claim (ClaimLeft), and RecordClaimLeft, the release's own statement, is its one writer:
// PutIssue names the column in neither its insert nor its update, as it does members_owed. Every
// site that records the issue writes a copy it read before its own fact, and the release runs on a
// lane of its own, so a copy read before the release wrote its entry would write that entry away,
// and a copy carrying an older entry would write the older one back. A nil entry clears it, and a
// key with no record writes nothing and is no error: the release runs for an issue the daemon may
// not record, which has no record to write into.
func TestPutIssueKeepsTheClaimLeftItsOwnStatementWrote(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	key := "LEGION-212"
	left := ClaimLeft{Holder: "user:alice", Reason: ClaimLeftPerson, At: time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)}
	read := func() *Issue {
		t.Helper()
		var got *Issue
		inTx(t, st, func(tx pgx.Tx) {
			var err error
			got, err = records.Issue(ctx, tx, key)
			must(t, err)
		})
		if got == nil {
			t.Fatalf("issue %s has no record", key)
		}
		return got
	}

	first := issueFixture(key)
	first.ClaimLeft = &ClaimLeft{Reason: ClaimLeftUnread, At: left.At.Add(-time.Hour)}
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, first))
	})
	stale := read()
	if stale.ClaimLeft != nil {
		t.Fatalf("the record PutIssue first wrote carries claimLeft %+v, want none: its insert names no claim_left", *stale.ClaimLeft)
	}

	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.RecordClaimLeft(ctx, tx, key, &left))
	})
	if got := read().ClaimLeft; !sameClaimLeft(got, &left) {
		t.Fatalf("after RecordClaimLeft the record carries claimLeft %+v, want %+v", got, left)
	}

	stale.Title = "Persist every workflow fact, renamed"
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, *stale))
	})
	got := read()
	if got.Title != stale.Title {
		t.Fatalf("PutIssue of the copy read before the entry wrote title %q, want %q", got.Title, stale.Title)
	}
	if !sameClaimLeft(got.ClaimLeft, &left) {
		t.Fatalf("PutIssue of a copy read before RecordClaimLeft left claimLeft %+v, want the entry RecordClaimLeft wrote, %+v", got.ClaimLeft, left)
	}

	carrying := *got
	carrying.ClaimLeft = &ClaimLeft{Reason: ClaimLeftRefused, At: left.At.Add(time.Hour)}
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, carrying))
	})
	if got := read().ClaimLeft; !sameClaimLeft(got, &left) {
		t.Fatalf("PutIssue of a copy carrying another entry left claimLeft %+v, want the entry RecordClaimLeft wrote, %+v", got, left)
	}

	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.RecordClaimLeft(ctx, tx, key, nil))
	})
	if got := read().ClaimLeft; got != nil {
		t.Fatalf("RecordClaimLeft of no entry left claimLeft %+v, want it cleared", *got)
	}

	inTx(t, st, func(tx pgx.Tx) {
		if err := records.RecordClaimLeft(ctx, tx, "LEGION-999", &left); err != nil {
			t.Fatalf("RecordClaimLeft of an issue with no record = %v, want no error", err)
		}
		unrecorded, err := records.Issue(ctx, tx, "LEGION-999")
		must(t, err)
		if unrecorded != nil {
			t.Fatalf("RecordClaimLeft of an issue with no record wrote %+v, want no record", unrecorded)
		}
	})
}
