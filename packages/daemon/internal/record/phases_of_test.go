package record

import (
	"context"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// PhasesOf reads the phases rows of many issues in one statement and answers each issue's rows as
// Phases answers them, in role order, keyed by issue: an issue with no row has no entry, an issue
// it was not asked about is not read, and no issues read nothing.
func TestPhasesOfReadsEachIssuesPhasesRowsAsPhasesReadsThem(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	inTx(t, st, func(tx pgx.Tx) {
		for _, key := range []string{"LEGION-301", "LEGION-302", "LEGION-303", "LEGION-304"} {
			must(t, records.PutIssue(ctx, tx, issueFixture(key)))
		}
		for _, row := range []PhaseRow{
			{Issue: "LEGION-301", Role: claim.RoleTester, Claim: "tester-301", Entry: 2},
			{Issue: "LEGION-301", Role: claim.RoleImplementer, Claim: "implementer-301", Entry: 4},
			{Issue: "LEGION-302", Role: claim.RoleMerger, Claim: "merger-302", Entry: 1},
			{Issue: "LEGION-304", Role: claim.RoleImplementer, Claim: "implementer-304", Entry: 7},
		} {
			must(t, records.PutPhase(ctx, tx, row))
		}
	})
	inTx(t, st, func(tx pgx.Tx) {
		got, err := records.PhasesOf(ctx, tx, []string{"LEGION-301", "LEGION-302", "LEGION-303"})
		must(t, err)
		want := map[string][]PhaseRow{}
		for _, key := range []string{"LEGION-301", "LEGION-302"} {
			rows, err := records.Phases(ctx, tx, key)
			must(t, err)
			want[key] = rows
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("PhasesOf = %#v, want %#v: each asked issue's rows as Phases reads them, and no entry for an issue with none or one not asked about", got, want)
		}
		if roles := []claim.Role{got["LEGION-301"][0].Role, got["LEGION-301"][1].Role}; roles[0] != claim.RoleImplementer || roles[1] != claim.RoleTester {
			t.Fatalf("LEGION-301's rows come in the roles %v, want role order", roles)
		}
		none, err := records.PhasesOf(ctx, tx, nil)
		must(t, err)
		if len(none) != 0 {
			t.Fatalf("PhasesOf of no issues = %#v, want nothing", none)
		}
	})
}
