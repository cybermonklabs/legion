package store

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// slotsReadingVersion is the version of the migration this file tests.
const slotsReadingVersion = 20261121103000

// slotsReadingRelease names the release whose way back the migration's header carries, as a
// failure to read that block names it.
const slotsReadingRelease = "the release that reads the slots"

// slotsReadingIssue is the slot holder every check of this file records: a root of its own tree,
// held from implementing.
func slotsReadingIssue() record.Issue {
	return record.Issue{Key: "ACME-1", Tree: "ACME-1", Project: "ACME", Title: "ACME-1", Phase: phase.Held,
		Hold: &record.Hold{From: phase.Implementing}, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress",
		Rank: "a", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
}

// 20261121103000 adds the three times the slots reading dates a holder by, and the way back, the
// last paragraph of its header (migrations.WayBack), takes them away again with the tick notices
// that carry a slots reading, which the release before decodes strictly and refuses at every
// attempt. Run over a store this release wrote: a held holder with its hold's start, the same
// holder's wait on another issue, a suspended claim with its stop, and three controller notices of
// the project, a tick carrying the reading, a tick of the release before's shape and a todo wake.
// The way back leaves the two notices the release before decodes, takes away the two tables, the
// column and the migration's version row, and leaves every column and row the release before
// reads; Migrate then runs the migration again, and the times read as none, as at the first
// upgrade.
func TestTheSlotsReadingWayBackLeavesWhatTheReleaseBeforeReads(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	at := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	st.UseClock(func() time.Time { return at })
	records := record.NewStore()
	issue := slotsReadingIssue()
	if err := Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		if err := records.PutIssue(ctx, tx, issue); err != nil {
			return err
		}
		if err := records.PutSlot(ctx, tx, record.Slot{Issue: issue.Key, AdmittedAt: at.Add(-time.Hour)}); err != nil {
			return err
		}
		if err := records.MarkHoldBegan(ctx, tx, issue.Key, at.Add(-30*time.Minute)); err != nil {
			return err
		}
		if err := records.MarkSlotWait(ctx, tx, issue.Key, true, at.Add(-20*time.Minute)); err != nil {
			return err
		}
		reading := record.SlotsReading{InUse: 1, Cap: 1, Stopped: record.StateCount{Count: 1}}
		reading.Line = record.SlotsLine(reading)
		for _, notice := range []record.OutboxPayload{
			record.ControllerNotice{Kind: record.TickNotice, Slots: &reading},
			record.ControllerNotice{Kind: record.TickNotice},
			record.ControllerNotice{Kind: record.TodoNotice},
		} {
			row, err := record.NewOutboxRow("ACME", notice, at)
			if err != nil {
				return err
			}
			if err := records.Enqueue(ctx, tx, row); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the store this release writes: %v", err)
	}
	token, err := claim.NewToken("acme", issue.Key, claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutClaim(ctx, supervise.Claim{Token: token, Project: "acme", Tree: issue.Key, Issue: issue.Key,
		Role: claim.RoleImplementer, Generation: 1, TreeEpoch: 1, State: supervise.StateSuspended,
		SuspendedBy: supervise.SuspendedByOperator}); err != nil {
		t.Fatalf("record the suspended claim: %v", err)
	}

	const notices = `select payload->>'kind' || case when payload ? 'slots' then ' with slots' else '' end from outbox
		where kind = 'controller_notice' order by id`
	if got, want := rowsAsText(t, st, notices), []string{"tick with slots", "tick", "todo"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the controller notices this release queued = %v, want %v", got, want)
	}
	if got := rowsAsText(t, st, `select stopped_at::text from claims`); len(got) != 1 {
		t.Fatalf("the suspended claim's stop reads %v, want one time", got)
	}

	if _, err := st.Pool().Exec(ctx, wayBackOf(t, slotsReadingVersion, slotsReadingRelease)); err != nil {
		t.Fatalf("run the way back of %s: %v", slotsReadingRelease, err)
	}
	if got, want := rowsAsText(t, st, notices), []string{"tick", "todo"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the controller notices after the way back = %v, want %v: only the tick carrying a slots reading goes", got, want)
	}
	const gone = `select count(*)::text from information_schema.tables where table_schema = current_schema()
		and table_name in ('issue_holds', 'slot_waits')
		union all select count(*)::text from information_schema.columns where table_schema = current_schema()
		and table_name = 'claims' and column_name = 'stopped_at'
		union all select count(*)::text from schema_version where version = 20261121103000`
	if got, want := rowsAsText(t, st, gone), []string{"0", "0", "0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the tables, the column and the version row after the way back count %v, want %v", got, want)
	}
	// What the release before reads stands: the issue, its slot and the claim, by the columns it
	// names.
	if got, want := rowsAsText(t, st, `select key || ' ' || phase || ' ' || held_from from issues
		union all select issue from slots union all select token || ' ' || state from claims`),
		[]string{"ACME-1 held implementing", "ACME-1", string(token) + " suspended"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the rows the release before reads after the way back = %v, want %v", got, want)
	}

	applied, err := st.Migrate(ctx)
	if err != nil {
		t.Fatalf("migrate forward again: %v", err)
	}
	if applied != 1 {
		t.Fatalf("the roll-forward applied %d migrations, want this one alone, again", applied)
	}
	if err := Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		read, err := records.Issue(ctx, tx, issue.Key)
		if err != nil {
			return err
		}
		if read.Hold == nil || read.Hold.Since != nil {
			t.Errorf("the held issue after the roll-forward reads the hold %+v, want it with no time", read.Hold)
		}
		slots, err := records.Slots(ctx, tx)
		if err != nil {
			return err
		}
		if len(slots) != 1 || slots[0].WaitsSince != nil {
			t.Errorf("the slots after the roll-forward read %+v, want the one slot with no wait time", slots)
		}
		claims, err := records.HolderClaims(ctx, tx, "acme")
		if err != nil {
			return err
		}
		if len(claims) != 1 || !claims[0].StoppedAt.IsZero() {
			t.Errorf("the claims after the roll-forward read %+v, want the suspended claim with no stop", claims)
		}
		return nil
	}); err != nil {
		t.Fatalf("read the store after the roll-forward: %v", err)
	}
}

// The claim store keeps when a claim stopped (record.HolderClaim.StoppedAt): the write that moves
// it into suspended or retired from a state that is neither stamps the store's clock, a move
// between the two keeps the first stop, and a write in any other state clears it. A failed claim
// is dated by its failure instead. Each step writes the claim as a machine writes it, at a clock
// of its own, and reads it back through the slots reading's own read (record.Store.HolderClaims),
// which reads the claims of slot holders alone, so the claim's issue holds a slot.
func TestAClaimsStopIsStampedWhenItStops(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	token, err := claim.NewToken("acme", "ACME-1", claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	if err := Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		records := record.NewStore()
		if err := records.PutIssue(ctx, tx, slotsReadingIssue()); err != nil {
			return err
		}
		return records.PutSlot(ctx, tx, record.Slot{Issue: "ACME-1", AdmittedAt: base})
	}); err != nil {
		t.Fatalf("record ACME-1 holding a slot: %v", err)
	}
	failedAt := base.Add(9 * time.Minute)
	stopOf := func(minute int, state supervise.ClaimState) time.Time {
		t.Helper()
		st.UseClock(func() time.Time { return base.Add(time.Duration(minute) * time.Minute) })
		c := supervise.Claim{Token: token, Project: "acme", Tree: "ACME-1", Issue: "ACME-1", Role: claim.RoleImplementer,
			Generation: 1, TreeEpoch: 1, State: state}
		if state == supervise.StateFailed {
			c.FailedAt = failedAt
		}
		if err := st.PutClaim(ctx, c); err != nil {
			t.Fatalf("record the claim %s: %v", state, err)
		}
		var claims []record.HolderClaim
		if err := Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
			var err error
			claims, err = record.NewStore().HolderClaims(ctx, tx, "acme")
			return err
		}); err != nil || len(claims) != 1 {
			t.Fatalf("read the claim back = %+v, %v", claims, err)
		}
		return claims[0].StoppedAt
	}
	for _, step := range []struct {
		minute int
		state  supervise.ClaimState
		want   time.Time
	}{
		{1, supervise.StateIdle, time.Time{}},
		{2, supervise.StateSuspended, base.Add(2 * time.Minute)},
		{3, supervise.StateSuspended, base.Add(2 * time.Minute)},
		{4, supervise.StateRetired, base.Add(2 * time.Minute)},
		{5, supervise.StateQueued, time.Time{}},
		{6, supervise.StateRetired, base.Add(6 * time.Minute)},
		{7, supervise.StateFailed, failedAt},
	} {
		if got := stopOf(step.minute, step.state); !got.Equal(step.want) {
			t.Errorf("at minute %d a claim written %s stopped at %v, want %v", step.minute, step.state, got, step.want)
		}
	}
}
