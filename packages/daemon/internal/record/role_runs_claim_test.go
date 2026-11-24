package record

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// A member testing on `implementer:a tester:a reviewer` also holds version 1's tester, and its
// re-admission queued a start naming that claim, `…-tester`, with the role tester. That row is the
// run of `…-tester` and not the run of key a's `…-implementer`, which works the tester's step: a
// read of the owner's run that counted it would take it for the owner's own start and skip one the
// owner needs. A row that names no claim is the run of the claim it reaches when it runs
// (WorkerClaim): the claim the role's phases row names while that row runs a step of the plan, and
// the role's own token otherwise, as for the reviewer here, which has no phases row.
func TestARoleRunCountsNoQueuedRowThatNamesAnotherClaim(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	keyed := claim.Token("legion-legion-legion-209-implementer")
	stale := claim.Token("legion-legion-legion-209-tester")
	named := SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleTester, Claim: stale, Generation: 1, Phase: phase.Testing}
	inTx(t, st, func(tx pgx.Tx) {
		row, err := NewOutboxRow("LEGION-209", named, time.Now())
		must(t, err)
		must(t, records.Enqueue(ctx, tx, row))
	})
	owner := RoleRunKey{Token: keyed, Issue: "LEGION-209", Role: claim.RoleTester, Generation: 1}
	other := RoleRunKey{Token: stale, Issue: "LEGION-209", Role: claim.RoleTester, Generation: 1}
	inTx(t, st, func(tx pgx.Tx) {
		runs, err := records.RoleRuns(ctx, tx, []RoleRunKey{owner})
		must(t, err)
		if queued := runs[keyed].Queued; len(queued) != 0 {
			t.Fatalf("key a's run counts %+v, want no row: the row names %s", queued, stale)
		}
		runs, err = records.RoleRuns(ctx, tx, []RoleRunKey{other})
		must(t, err)
		if queued := runs[stale].Queued; len(queued) != 1 || queued[0].Request.Claim != stale {
			t.Fatalf("the run of %s counts %+v, want the row that names it", stale, queued)
		}
	})
	unnamed := SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleReviewer, Generation: 1, Phase: phase.Reviewing}
	inTx(t, st, func(tx pgx.Tx) {
		row, err := NewOutboxRow("LEGION-209", unnamed, time.Now())
		must(t, err)
		must(t, records.Enqueue(ctx, tx, row))
	})
	reviewer := RoleRunKey{Token: "legion-legion-legion-209-reviewer", Issue: "LEGION-209", Role: claim.RoleReviewer, Generation: 1}
	inTx(t, st, func(tx pgx.Tx) {
		runs, err := records.RoleRuns(ctx, tx, []RoleRunKey{reviewer})
		must(t, err)
		if queued := runs[reviewer.Token].Queued; len(queued) != 1 {
			t.Fatalf("the reviewer's run counts %+v, want the row that names no claim", queued)
		}
	})
}

// Two keys of one role, issue and generation read together, as the stranded report reads its
// batch: key a's claim works the tester's step and the tester's own claim of an earlier version
// stands beside it. A queued start naming key a's claim is key a's run and not the other's, and a
// start that names no claim, on an issue with no plan rows, is the run of the role's own claim,
// which is the claim such a row reaches.
func TestTwoKeysOfOneRoleReadTogetherEachKeepTheirOwnRun(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	keyed := claim.Token("legion-legion-legion-209-implementer")
	own := claim.Token("legion-legion-legion-209-tester")
	inTx(t, st, func(tx pgx.Tx) {
		row, err := NewOutboxRow("LEGION-209", SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleTester,
			Claim: keyed, Generation: 1, Phase: phase.Testing}, time.Now())
		must(t, err)
		must(t, records.Enqueue(ctx, tx, row))
	})
	keys := []RoleRunKey{
		{Token: keyed, Issue: "LEGION-209", Role: claim.RoleTester, Generation: 1},
		{Token: own, Issue: "LEGION-209", Role: claim.RoleTester, Generation: 1},
	}
	inTx(t, st, func(tx pgx.Tx) {
		runs, err := records.RoleRuns(ctx, tx, keys)
		must(t, err)
		if queued := runs[keyed].Queued; len(queued) != 1 || queued[0].Request.Claim != keyed {
			t.Fatalf("key a's run counts %+v, want the start that names it", queued)
		}
		if queued := runs[own].Queued; len(queued) != 0 {
			t.Fatalf("the tester's own run counts %+v, want none: the start names key a's claim", queued)
		}
	})
	inTx(t, st, func(tx pgx.Tx) {
		row, err := NewOutboxRow("LEGION-209", SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleTester,
			Generation: 1, Phase: phase.Testing}, time.Now())
		must(t, err)
		must(t, records.Enqueue(ctx, tx, row))
	})
	inTx(t, st, func(tx pgx.Tx) {
		runs, err := records.RoleRuns(ctx, tx, keys)
		must(t, err)
		if queued := runs[own].Queued; len(queued) != 1 || queued[0].Request.Claim != "" {
			t.Fatalf("the tester's own run counts %+v, want the start that names no claim: no plan row names another worker", queued)
		}
		if queued := runs[keyed].Queued; len(queued) != 1 {
			t.Fatalf("key a's run counts %+v, want its own start alone", queued)
		}
	})
}
