package record

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

// closedIssueFixture is key recorded as a closed root of project ACME on acme/widgets, at
// generation, whose Dispatch status is done, with GitHub's spelling of the repository its own.
func closedIssueFixture(key string, generation uint64) Issue {
	issue := issueFixture(key)
	issue.Project, issue.Tree, issue.Parent = "ACME", key, nil
	issue.Phase, issue.Status, issue.DispatchStatus, issue.Generation = phase.Done, "done", "done", generation
	issue.Repo, issue.GitHubRepo = "acme/widgets", "acme/widgets"
	return issue
}

// The class of an issue's merge history is what its rows show, and nothing else. Seven closed
// issues: one with a merge snapshot, one whose retained pull request row is merged, and one whose
// closed production check names a pull request derive ANSWERED. One whose pull request row is open,
// one whose only row is the clock a clear kept of it, and one whose production check names pull
// request 0 derive PARTIAL. The seventh holds no row that bears on a merge: it stands at
// generation 1, its repository and GitHub's spelling of it match, and nothing of a rename is
// configured. It derives NONE, since neither its generation nor its names prove that its work
// never merged. ANSWERED's known merge is the row's own fields. The reads change no row.
func TestMergeHistoryOfUsesOnlyEvidence(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	at := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	closed := at.Add(2 * time.Hour)
	head, sha := strings.Repeat("a", 40), strings.Repeat("b", 40)
	keys := []string{"ACME-1", "ACME-2", "ACME-3", "ACME-4", "ACME-5", "ACME-6", "ACME-7"}
	inTx(t, st, func(tx pgx.Tx) {
		for _, key := range keys {
			generation := uint64(2)
			if key == "ACME-7" {
				generation = 1
			}
			must(t, records.PutIssue(ctx, tx, closedIssueFixture(key, generation)))
		}
		_, err := records.PutMergeSnapshot(ctx, tx, "ACME-1", Merge{Number: 11, Head: head, SHA: sha, Base: "main", Repo: "acme/widgets", MergedAt: at})
		must(t, err)
		must(t, records.PutPullRequest(ctx, tx, PullRequest{Issue: "ACME-2", Repo: "acme/widgets", Number: 12, Branch: "legion/ACME-2", Base: "main",
			HeadSHA: head, MergeSHA: sha, State: PullRequestMerged}))
		must(t, records.PutProductionCheck(ctx, tx, ProductionCheck{Issue: "ACME-3", Project: "ACME", Tree: "ACME-3", Repo: "acme/widgets", Number: 13,
			MergeSHA: sha, OpenedAt: at, BoundAt: at.Add(time.Hour), ClosedAt: &closed, Outcome: CheckRecorded}))
		must(t, records.PutPullRequest(ctx, tx, PullRequest{Issue: "ACME-4", Repo: "acme/widgets", Number: 14, Branch: "legion/ACME-4", Base: "main",
			HeadSHA: head, State: PullRequestOpen}))
		must(t, records.PutPullRequest(ctx, tx, PullRequest{Issue: "ACME-5", Repo: "acme/widgets", Number: 15, Branch: "legion/ACME-5", Base: "main",
			HeadSHA: head, HeadUpdatedAt: at, State: PullRequestClosed}))
		must(t, records.ClearGeneration(ctx, tx, "ACME-5", at))
		must(t, records.PutProductionCheck(ctx, tx, ProductionCheck{Issue: "ACME-6", Project: "ACME", Tree: "ACME-6", Repo: "acme/widgets",
			OpenedAt: at, BoundAt: at.Add(time.Hour)}))
	})

	before := testdb.TableRows(t, st.Pool())
	histories := map[string]MergeHistoryRow{}
	inTx(t, st, func(tx pgx.Tx) {
		for _, key := range keys {
			history, err := records.MergeHistoryOf(ctx, tx, key)
			must(t, err)
			histories[key] = history
		}
	})
	if changed := testdb.ChangedTables(before, testdb.TableRows(t, st.Pool())); len(changed) != 0 {
		t.Fatalf("the reads of the merge history changed %v, want no row written", changed)
	}

	classes := map[string]MergeHistoryClass{}
	for key, history := range histories {
		classes[key] = history.Class
	}
	want := map[string]MergeHistoryClass{
		"ACME-1": MergeHistoryAnswered, "ACME-2": MergeHistoryAnswered, "ACME-3": MergeHistoryAnswered,
		"ACME-4": MergeHistoryPartial, "ACME-5": MergeHistoryPartial, "ACME-6": MergeHistoryPartial,
		"ACME-7": MergeHistoryNone,
	}
	if !reflect.DeepEqual(classes, want) {
		t.Fatalf("the classes = %v, want %v: three of positive evidence, three of other rows and one silent", classes, want)
	}
	for key, merge := range map[string]Merge{
		"ACME-1": {Number: 11, Head: head, SHA: sha, Base: "main", Repo: "acme/widgets", MergedAt: at},
		"ACME-2": {Number: 12, Head: head, SHA: sha, Base: "main", Repo: "acme/widgets"},
		"ACME-3": {Number: 13, SHA: sha, Repo: "acme/widgets"},
		"ACME-4": {}, "ACME-5": {}, "ACME-6": {}, "ACME-7": {},
	} {
		if got := histories[key].Evidence.Merge; got != merge {
			t.Errorf("the known merge of %s = %+v, want %+v", key, got, merge)
		}
	}
	if got, want := histories["ACME-5"].Evidence.Clocks, []MergeEvidenceClock{{Repo: "acme/widgets", Number: 15}}; !reflect.DeepEqual(got, want) {
		t.Errorf("the clocks of ACME-5 = %+v, want %+v", got, want)
	}
	if got := histories["ACME-4"].Evidence.PullRequest; got == nil || got.Number != 14 || got.State != PullRequestOpen {
		t.Errorf("the pull request of ACME-4 = %+v, want its open #14", got)
	}
	if got := histories["ACME-6"].Evidence.Check; got == nil || got.Number != 0 {
		t.Errorf("the production check of ACME-6 = %+v, want its assessment of pull request 0", got)
	}
	if silent := histories["ACME-7"].Evidence; silent.PullRequest != nil || silent.Check != nil || len(silent.Clocks) != 0 || silent.OtherRows {
		t.Errorf("the evidence of ACME-7 = %+v, want no row", silent)
	}
	for key, history := range histories {
		if missing := key == "ACME-3"; history.MissingMetadata != missing {
			t.Errorf("the missing metadata of %s = %t, want %t: only the merge-only check knows no base and no final head", key, history.MissingMetadata, missing)
		}
	}
}

// A clock a clear kept counts for the issue its issue column names at the read, never for the
// issue whose repository and number match it. LEGION-1 and LEGION-2 each hold a clock of pull
// request 42, under the old name and the new one, so each derives PARTIAL. The rename of LEGION-1
// merges its clock into the row the new name holds, which then names LEGION-1: LEGION-1 still
// derives PARTIAL, and LEGION-2, which holds no other row, derives NONE.
func TestARenameMovesAClearedClockToAnotherIssue(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	const from, to = "sjawhar/legion", "cybermonklabs/legion"
	applied := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	records := NewStore()
	for _, seed := range []struct {
		key, repo        string
		clock, clearedAt time.Time
	}{
		{"LEGION-1", from, applied.Add(time.Hour), applied.Add(2 * time.Hour)},
		{"LEGION-2", to, applied, applied.Add(3 * time.Hour)},
	} {
		inTx(t, st, func(tx pgx.Tx) {
			issue := issueFixture(seed.key)
			issue.Tree, issue.Parent, issue.Repo, issue.GitHubRepo = seed.key, nil, seed.repo, seed.repo
			must(t, records.PutIssue(ctx, tx, issue))
			must(t, records.PutPullRequest(ctx, tx, PullRequest{Issue: seed.key, Repo: seed.repo, Number: 42, Branch: "legion/" + seed.key,
				HeadSHA: "head", HeadUpdatedAt: seed.clock, State: PullRequestClosed}))
			must(t, records.ClearGeneration(ctx, tx, seed.key, seed.clearedAt))
		})
	}
	classes := func() map[string]MergeHistoryClass {
		t.Helper()
		got := map[string]MergeHistoryClass{}
		inTx(t, st, func(tx pgx.Tx) {
			for _, key := range []string{"LEGION-1", "LEGION-2"} {
				history, err := records.MergeHistoryOf(ctx, tx, key)
				must(t, err)
				got[key] = history.Class
			}
		})
		return got
	}
	if got, want := classes(), map[string]MergeHistoryClass{"LEGION-1": MergeHistoryPartial, "LEGION-2": MergeHistoryPartial}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the classes before the rename = %v, want %v: each holds its own clock", got, want)
	}
	inTx(t, st, func(tx pgx.Tx) {
		_, err := RenameIssueRepository(ctx, tx, "LEGION-1", from, to)
		must(t, err)
	})
	if got, want := classes(), map[string]MergeHistoryClass{"LEGION-1": MergeHistoryPartial, "LEGION-2": MergeHistoryNone}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the classes after the rename = %v, want %v: the one clock left names LEGION-1", got, want)
	}
}
