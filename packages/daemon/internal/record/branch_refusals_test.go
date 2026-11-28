package record

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// A branch row's refusal for good is recorded once for its issue's generation: the first refusal
// stands, and a second of the same generation records nothing, which is what tells the architect
// once. The state's read names the refusal of the generation each issue of its project stands in
// now, and no refusal of an earlier generation or of another project. A clear deletes the issue's
// refusals.
func TestABranchRefusalIsRecordedOnceForItsGeneration(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	at := time.Date(2026, 11, 27, 21, 0, 0, 0, time.UTC)
	issue, earlier, other := issueFixture("LEGION-301"), issueFixture("LEGION-302"), issueFixture("OTHER-7")
	other.Project, other.Parent = "OTHER", nil
	first := BranchRefusal{Issue: issue.Key, Generation: issue.Generation, Row: 121, Write: BranchWriteMove, Kind: "no-node-id",
		Reason: "read the node id of acme/widgets: GitHub named none", RefusedAt: at}
	inTx(t, st, func(tx pgx.Tx) {
		for _, put := range []Issue{issue, earlier, other} {
			must(t, records.PutIssue(ctx, tx, put))
		}
		if recorded, err := records.RefuseBranch(ctx, tx, first); err != nil || !recorded {
			t.Fatalf("the first refusal of %s's generation %d = %t, %v; want it recorded", issue.Key, issue.Generation, recorded, err)
		}
		second := first
		second.Row, second.Kind, second.RefusedAt = 122, "graphql-forbidden", at.Add(time.Minute)
		if recorded, err := records.RefuseBranch(ctx, tx, second); err != nil || recorded {
			t.Fatalf("a second refusal of %s's generation %d = %t, %v; want nothing recorded beside the first", issue.Key, issue.Generation, recorded, err)
		}
		for _, refusal := range []BranchRefusal{
			{Issue: earlier.Key, Generation: earlier.Generation - 1, Row: 130, Write: BranchWriteCreate, Kind: "forbidden", Reason: "an earlier change's", RefusedAt: at},
			{Issue: other.Key, Generation: other.Generation, Row: 140, Write: BranchWriteCreate, Kind: "forbidden", Reason: "another project's", RefusedAt: at},
		} {
			if recorded, err := records.RefuseBranch(ctx, tx, refusal); err != nil || !recorded {
				t.Fatalf("the refusal of %s's generation %d = %t, %v; want it recorded", refusal.Issue, refusal.Generation, recorded, err)
			}
		}
	})
	named := func() []BranchRefusal {
		t.Helper()
		var got []BranchRefusal
		inTx(t, st, func(tx pgx.Tx) {
			var err error
			got, err = records.BranchRefusals(ctx, tx, "LEGION")
			must(t, err)
		})
		return got
	}
	if got := named(); !reflect.DeepEqual(got, []BranchRefusal{first}) {
		t.Fatalf("LEGION's refusals = %+v, want the first refusal of %s alone: %+v", got, issue.Key, first)
	}
	inTx(t, st, func(tx pgx.Tx) { must(t, records.ClearBranchRefusals(ctx, tx, issue.Key)) })
	if got := named(); len(got) != 0 {
		t.Fatalf("LEGION's refusals after the clear of %s = %+v, want none", issue.Key, got)
	}
}
