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
// owner needs. A row that names no claim is still the run of the role's token.
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

func TestRoleRunsKeepsEachNamedClaimWhenSeveralAgentsHaveTheSameRole(t *testing.T) {
	ctx := t.Context()
	st := migratedStore(t)
	records := NewStore()
	tokens := []claim.Token{"legion-legion-legion-209-implementer", "legion-legion-legion-209-implementer-2"}
	var keys []RoleRunKey
	inTx(t, st, func(tx pgx.Tx) {
		for _, token := range tokens {
			row, err := NewOutboxRow("LEGION-209", SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleImplementer,
				Claim: token, Generation: 1, Phase: phase.Implementing}, time.Now())
			must(t, err)
			must(t, records.Enqueue(ctx, tx, row))
			keys = append(keys, RoleRunKey{Token: token, Issue: "LEGION-209", Role: claim.RoleImplementer, Generation: 1})
		}
	})
	inTx(t, st, func(tx pgx.Tx) {
		runs, err := records.RoleRuns(ctx, tx, keys)
		must(t, err)
		for _, token := range tokens {
			if queued := runs[token].Queued; len(queued) != 1 || queued[0].Request.Claim != token {
				t.Fatalf("the run of %s counts %+v, want exactly the start naming that agent", token, queued)
			}
		}
	})
}
