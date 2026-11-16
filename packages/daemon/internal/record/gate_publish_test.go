package record

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/store"
)

// gatePublication is the one design_gates_publish row of issue and the set it carries, with the
// number of such rows the outbox holds for it: the enqueue keeps one row per issue (the partial
// index of 20261110235320), and every assertion below reads the row and that count together, so a
// second row queued beside the first would fail wherever it was written.
func gatePublication(t *testing.T, st *store.Store, issue string) (OutboxRow, DesignGatesPublish, int) {
	t.Helper()
	ctx := context.Background()
	var rows int
	if err := st.Pool().QueryRow(ctx, "select count(*) from outbox where kind = $1 and issue = $2",
		string(OutboxKindDesignGatesPublish), issue).Scan(&rows); err != nil {
		t.Fatalf("count the design-gates publications of %s: %v", issue, err)
	}
	if rows == 0 {
		return OutboxRow{}, DesignGatesPublish{}, 0
	}
	row, err := scanOutbox(st.Pool().QueryRow(ctx, "select "+outboxColumns+" from outbox where kind = $1 and issue = $2 order by id",
		string(OutboxKindDesignGatesPublish), issue))
	if err != nil {
		t.Fatalf("read the design-gates publication of %s: %v", issue, err)
	}
	payload, err := DecodeOutboxPayload(row)
	if err != nil {
		t.Fatalf("decode the design-gates publication of %s: %v", issue, err)
	}
	publish, ok := payload.(DesignGatesPublish)
	if !ok {
		t.Fatalf("the payload of the design-gates publication of %s is %T, want a %T", issue, payload, DesignGatesPublish{})
	}
	return row, publish, rows
}

// An issue's design-gate publication is one row for as long as it waits: every gate change inside
// the window where Dispatch has not taken the row writes the whole set again, and the enqueue
// replaces what that row carries rather than queueing beside it
// (Postgres.EnqueueDesignGates). Queueing beside it would send Dispatch the older set after the
// newer one, which is the one outcome the row exists to prevent.
//
// The replacement keeps the row's attempts and its next time, because it is the same publish
// waiting on the same lane and only the set it carries is newer: a set that changed while the row
// backed off must not restart that backoff against a Dispatch that is down. It rotates the lease
// of a row a lane already holds, because that worker read the older set and its finish would
// delete the row carrying the newer one, leaving Dispatch holding what the record no longer says.
// And a publication of the set the row already carries writes nothing at all, so a boot pass or a
// re-registration over an unchanged set costs the waiting row neither its lease nor its place.
func TestGatePublicationKeepsOnePendingRowPerIssue(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	const issue = "LEGION-208"
	now := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	spec := []DesignGateDocument{{Step: DesignStepSpec, ArtifactID: "artifact-spec"}}
	both := []DesignGateDocument{
		{Step: DesignStepCriteria, ArtifactID: "artifact-criteria"},
		{Step: DesignStepSpec, ArtifactID: "artifact-spec"},
	}
	enqueue := func(gates []DesignGateDocument, at time.Time) {
		t.Helper()
		inTx(t, st, func(tx pgx.Tx) {
			must(t, records.EnqueueDesignGates(ctx, tx, DesignGatesPublish{Issue: issue, Gates: gates}, at))
		})
	}

	enqueue(spec, now)
	queued, carried, rows := gatePublication(t, st, issue)
	if rows != 1 || !reflect.DeepEqual(carried.Gates, spec) || carried.Issue != issue {
		t.Fatalf("the first publication of %s = %d rows carrying %+v, want the one row carrying %+v", issue, rows, carried, spec)
	}
	id := queued.ID

	// The lane runs the row and Dispatch is down, so it waits on its backoff: attempts 1, due in
	// five minutes. This is the window a gate change lands in.
	claim := func(at time.Time, lease time.Duration) OutboxRow {
		t.Helper()
		var claimed OutboxRow
		inTx(t, st, func(tx pgx.Tx) {
			row, ok, err := records.ClaimRow(ctx, tx, id, at, lease)
			must(t, err)
			if !ok {
				t.Fatalf("the design-gates publication of %s was not claimable at %s", issue, at)
			}
			claimed = row
		})
		return claimed
	}
	attempt := claim(now, time.Minute)
	retried := now.Add(5 * time.Minute)
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.RetryOutbox(ctx, tx, id, attempt.LeaseToken, retried, "Dispatch is down"))
	})

	// A gate change while the row waits replaces what it carries, and leaves the row itself alone:
	// the same row, due at the same moment, with the attempts it has already spent.
	enqueue(both, now.Add(time.Minute))
	waiting, carried, rows := gatePublication(t, st, issue)
	if rows != 1 || waiting.ID != id {
		t.Fatalf("the publication of %s after a second gate change = %d rows (id %d), want the one row %d replaced in place", issue, rows, waiting.ID, id)
	}
	if !reflect.DeepEqual(carried.Gates, both) {
		t.Fatalf("the waiting row of %s carries %+v, want the newer set %+v: the older set must not reach Dispatch after it", issue, carried.Gates, both)
	}
	if waiting.Attempts != 1 || !waiting.NextAt.Equal(retried) {
		t.Fatalf("the replaced row of %s = %d attempts due at %s, want the 1 attempt and the %s of the row that was already waiting",
			issue, waiting.Attempts, waiting.NextAt, retried)
	}

	// The lane claims the row and is inside the PUT, carrying the set it read, when a gate change
	// writes a newer one. The replacement rotates the lease, so nothing that worker does lands:
	// its finish would otherwise delete the row holding the set Dispatch has not been told.
	leased := claim(retried, time.Minute)
	enqueue(spec, retried)
	rotated, carried, rows := gatePublication(t, st, issue)
	if rows != 1 || rotated.ID != id || !reflect.DeepEqual(carried.Gates, spec) {
		t.Fatalf("the leased row of %s after a gate change = %d rows (id %d) carrying %+v, want the one row %d carrying %+v",
			issue, rows, rotated.ID, carried.Gates, id, spec)
	}
	if rotated.LeaseToken == leased.LeaseToken || rotated.LeaseToken == "" {
		t.Fatalf("the lease of %s's publication = %q, want a lease other than the running attempt's %q", issue, rotated.LeaseToken, leased.LeaseToken)
	}
	var lost error
	inTx(t, st, func(tx pgx.Tx) {
		lost = records.FinishLeasedOutbox(ctx, tx, id, leased.LeaseToken)
	})
	if !errors.Is(lost, ErrOutboxLeaseLost) {
		t.Fatalf("the running attempt's finish of %s = %v, want %v: the set it sent is no longer the set the row carries", issue, lost, ErrOutboxLeaseLost)
	}
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.RetryOutbox(ctx, tx, id, leased.LeaseToken, retried.Add(time.Hour), "Dispatch is down"))
	})
	survived, carried, rows := gatePublication(t, st, issue)
	if rows != 1 || survived.ID != id || !reflect.DeepEqual(carried.Gates, spec) {
		t.Fatalf("the publication of %s after the running attempt ended = %d rows (id %d) carrying %+v, want the one row %d still carrying %+v",
			issue, rows, survived.ID, carried.Gates, id, spec)
	}
	if survived.Attempts != rotated.Attempts || !survived.NextAt.Equal(rotated.NextAt) {
		t.Fatalf("the running attempt's retry moved %s's row to %d attempts due at %s, want the %d attempts and the %s it held: its lease is no longer the row's",
			issue, survived.Attempts, survived.NextAt, rotated.Attempts, rotated.NextAt)
	}

	// A publication of the set the row already carries writes nothing at all: the lane that holds
	// the row keeps its lease, so the attempt sending exactly that set finishes it.
	running := claim(retried.Add(time.Hour), time.Minute)
	enqueue(spec, retried.Add(time.Hour))
	unchanged, carried, rows := gatePublication(t, st, issue)
	if rows != 1 || unchanged.LeaseToken != running.LeaseToken || unchanged.Attempts != running.Attempts ||
		!unchanged.NextAt.Equal(running.NextAt) || !reflect.DeepEqual(carried.Gates, spec) {
		t.Fatalf("publishing the set %s's row already carries left %d rows leased %q with %d attempts due at %s carrying %+v, want the running attempt's row untouched: leased %q, %d attempts, due %s",
			issue, rows, unchanged.LeaseToken, unchanged.Attempts, unchanged.NextAt, carried.Gates, running.LeaseToken, running.Attempts, running.NextAt)
	}
	var finished error
	inTx(t, st, func(tx pgx.Tx) {
		finished = records.FinishLeasedOutbox(ctx, tx, id, running.LeaseToken)
	})
	must(t, finished)
	if _, _, rows := gatePublication(t, st, issue); rows != 0 {
		t.Fatalf("the outbox holds %d publications of %s after the attempt that sent its set finished, want none", rows, issue)
	}
}
