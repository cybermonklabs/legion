package record

import (
	"cmp"
	"context"
	"reflect"
	"slices"
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

// A claim's queued rows (ClaimQueued) are its role runs of every role (RoleRun), read alike: one
// read of the queued rows serves both (readQueued), with only the selection apart, so a row reads
// the same age, lease and attempts whichever asks. Key a's claim, `…-implementer`, works the
// implementer's step and the tester's: it has a start of each that names it, the second leased and
// tried twice, and a start of the implementer that names no claim, which reaches the role's own
// token, key a's. A start of another generation, one that names the tester's own claim of an
// earlier version, and a row of the reviewer that does not decode are no row of key a's at
// generation 1, and neither read decodes the last.
func TestAClaimsQueuedRowsAreItsRoleRunsOfEveryRole(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	keyed := claim.Token("legion-legion-legion-209-implementer")
	stale := claim.Token("legion-legion-legion-209-tester")
	inTx(t, st, func(tx pgx.Tx) {
		for _, request := range []SuperviseRequest{
			{Op: "start", Tree: "LEGION-208", Role: claim.RoleImplementer, Claim: keyed, Generation: 1, Phase: phase.Implementing, Entry: 1},
			{Op: "start", Tree: "LEGION-208", Role: claim.RoleTester, Claim: keyed, Generation: 1, Phase: phase.Testing, Entry: 2},
			{Op: "start", Tree: "LEGION-208", Role: claim.RoleImplementer, Generation: 1, Phase: phase.Implementing, Entry: 3},
			{Op: "start", Tree: "LEGION-208", Role: claim.RoleTester, Claim: keyed, Generation: 2, Phase: phase.Testing, Entry: 1},
			{Op: "start", Tree: "LEGION-208", Role: claim.RoleTester, Claim: stale, Generation: 1, Phase: phase.Testing, Entry: 2},
		} {
			row, err := NewOutboxRow("LEGION-209", request, time.Now())
			must(t, err)
			must(t, records.Enqueue(ctx, tx, row))
		}
		_, err := tx.Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ('supervise', 'LEGION-209', '{"op": "bogus", "tree": "LEGION-208", "role": "reviewer", "generation": 1}', 0, now(), '')`)
		must(t, err)
		_, err = tx.Exec(ctx, `update outbox set attempts = 2, lease_token = 'lease-1', lease_until = now() + interval '1 hour'
			where issue = 'LEGION-209' and payload->>'role' = 'tester' and payload->>'claim' = $1 and payload->>'generation' = '1'`, string(keyed))
		must(t, err)
	})
	inTx(t, st, func(tx pgx.Tx) {
		queued, err := records.ClaimQueued(ctx, tx, keyed, "LEGION-209", 1)
		must(t, err)
		var runs []QueuedSupervise
		for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
			run, err := records.RoleRun(ctx, tx, keyed, "LEGION-209", role, 1)
			must(t, err)
			runs = append(runs, run.Queued...)
		}
		slices.SortFunc(runs, func(a, b QueuedSupervise) int { return cmp.Compare(a.ID, b.ID) })
		if len(queued) != 3 || queued[1].Request.Role != claim.RoleTester || !queued[1].Running || queued[1].Attempts != 2 {
			t.Fatalf("key a's queued rows = %+v, want its three starts at generation 1, the tester's leased and tried twice", queued)
		}
		if !reflect.DeepEqual(queued, runs) {
			t.Fatalf("key a's queued rows = %+v, but its role runs read %+v: the two reads differ", queued, runs)
		}
	})
}
