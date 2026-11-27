package record

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// AllPhases is the phases rows of one project's issues and of no other project's, each with the
// columns the state route reads and no other. The database is shared by every project's daemon and
// a row is keyed by an issue, so the project a row belongs to is the project of the issue it is
// keyed by: the rows of two projects' issues are read apart, and the read of one project answers
// the rows of the other for nobody. Every row here holds a value in every column, so a column the
// read leaves out shows as the zero value, and a column it reads shows as the value written.
func TestThePhasesOfAProjectLeaveEveryOtherProjectsOut(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	at := time.Date(2026, 11, 22, 1, 0, 0, 0, time.UTC)

	inTx(t, st, func(tx pgx.Tx) {
		for _, issue := range []Issue{
			{Key: "AAA-1", Project: "AAA"}, {Key: "AAA-2", Project: "AAA"}, {Key: "BBB-1", Project: "BBB"},
		} {
			issue.Repo, issue.GitHubRepo, issue.DefaultBranch = "sjawhar/legion", "sjawhar/legion", "main"
			issue.Tree, issue.Title, issue.Phase = issue.Key, issue.Key, phase.Reviewing
			issue.Generation, issue.Status, issue.Rank = 1, "in_progress", "U"
			must(t, records.PutIssue(ctx, tx, issue))
		}
		// written is a row of issue and role with a value in every column.
		written := func(issue string, role claim.Role) PhaseRow {
			return PhaseRow{
				Issue: issue, Role: role, Claim: claim.Token("legion-" + strings.ToLower(issue) + "-" + string(role)),
				HandoffCommit: "commit-" + issue, Pass: 2, HandoffRecord: "record-" + issue, HandoffVersion: 3, Rounds: 4,
				ConflictRounds: 1, BackwardMoves: map[string]int{"reviewing-implementing": 2}, CapTold: 5, Verdict: "pass",
				Summary: "the summary of " + issue, LastHandoff: "last-" + issue,
				Decision:    &ReviewDecision{State: "approved", Body: "ship it", Head: "head-" + issue},
				CompletedAt: at, Entry: 6, EntryTask: "the task of " + issue, EntryVerbatim: true, EntryRaisedAt: at,
				EntryMovedBack: true, ReviewedHead: "reviewed-" + issue,
			}
		}
		// listed is row as AllPhases reads it: the columns the state route reads, and the zero
		// value in every other field.
		listed := func(row PhaseRow) PhaseRow {
			return PhaseRow{
				Issue: row.Issue, Role: row.Role, Claim: row.Claim, HandoffCommit: row.HandoffCommit, Pass: row.Pass,
				HandoffRecord: row.HandoffRecord, HandoffVersion: row.HandoffVersion, Rounds: row.Rounds,
				BackwardMoves: row.BackwardMoves, Decision: row.Decision, Entry: row.Entry,
			}
		}
		rows := []PhaseRow{
			written("AAA-1", claim.RoleImplementer), written("AAA-1", claim.RoleReviewer),
			written("AAA-2", claim.RolePlanner), written("BBB-1", claim.RoleImplementer),
		}
		for _, row := range rows {
			must(t, records.PutPhase(ctx, tx, row))
		}

		for _, tc := range []struct {
			project string
			want    map[string][]PhaseRow
		}{
			{"AAA", map[string][]PhaseRow{
				"AAA-1": {listed(rows[0]), listed(rows[1])},
				"AAA-2": {listed(rows[2])},
			}},
			{"BBB", map[string][]PhaseRow{"BBB-1": {listed(rows[3])}}},
			{"CCC", map[string][]PhaseRow{}},
		} {
			got, err := records.AllPhases(ctx, tx, tc.project)
			must(t, err)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("the phases of %s are %#v, want %#v: another project's rows are no entry of this read, and only the columns the state route reads are read",
					tc.project, got, tc.want)
			}
		}

		// Each issue's rows are the rows Phases answers for it, in the same order, in every column
		// AllPhases reads.
		all, err := records.AllPhases(ctx, tx, "AAA")
		must(t, err)
		for _, key := range []string{"AAA-1", "AAA-2"} {
			own, err := records.Phases(ctx, tx, key)
			must(t, err)
			want := make([]PhaseRow, len(own))
			for i, row := range own {
				want[i] = listed(row)
			}
			if !reflect.DeepEqual(all[key], want) {
				t.Fatalf("AllPhases answers %s %#v, and Phases answers %#v", key, all[key], own)
			}
		}
	})
}
