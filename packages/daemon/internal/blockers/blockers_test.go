package blockers

import (
	"context"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Unfinished is the whole judgement the mirror exists for, and it is read inside a transaction
// that can ask Dispatch nothing: what it does not know, it treats as still blocking.
func TestUnfinishedHoldsOnEveryBlockerThatIsNotDone(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows map[string]Row
		key  string
		want []string
	}{
		{
			name: "done is the Dispatch status done, exactly",
			rows: map[string]Row{
				"LEGION-900": {Status: "todo", Blockers: []string{"LEGION-901", "LEGION-902", "LEGION-903", "LEGION-904"}},
				"LEGION-901": {Status: "done"},
				"LEGION-902": {Status: "backlog"},
				"LEGION-903": {Status: "icebox"},
				"LEGION-904": {Status: "in_progress"},
			},
			key:  "LEGION-900",
			want: []string{"LEGION-902", "LEGION-903", "LEGION-904"},
		},
		{
			name: "a blocker with no row of its own is not done",
			rows: map[string]Row{
				"LEGION-900": {Status: "todo", Blockers: []string{"LEGION-901", "LEGION-902"}},
				"LEGION-902": {Status: "done"},
			},
			key:  "LEGION-900",
			want: []string{"LEGION-901"},
		},
		{
			name: "a key with no row of its own waits on nothing",
			rows: map[string]Row{"LEGION-901": {Status: "todo"}},
			key:  "LEGION-900",
			want: []string{},
		},
		{
			name: "nothing unfinished is an empty answer",
			rows: map[string]Row{
				"LEGION-900": {Status: "todo", Blockers: []string{"LEGION-901", "LEGION-902"}},
				"LEGION-901": {Status: "done"},
				"LEGION-902": {Status: "done"},
			},
			key:  "LEGION-900",
			want: []string{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Unfinished(tc.rows, tc.key)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Unfinished(%s) = %#v, want %#v in the row's own order", tc.key, got, tc.want)
			}
		})
	}
}

// UnfinishedOf judges a list its caller holds, so an issue the mirror has no row of yet is judged
// all the same, and by the same rule as a mirrored one: a blocker with no row is not done.
func TestUnfinishedOfJudgesAListWhoseIssueHasNoRow(t *testing.T) {
	rows := map[string]Row{"LEGION-901": {Status: "done"}, "LEGION-902": {Status: "in_progress"}}
	if got, want := UnfinishedOf(rows, []string{"LEGION-901", "LEGION-902", "LEGION-903"}), []string{"LEGION-902", "LEGION-903"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("UnfinishedOf = %#v, want %#v in the list's own order", got, want)
	}
	if got := UnfinishedOf(rows, nil); got == nil || len(got) != 0 {
		t.Fatalf("UnfinishedOf of no blockers = %#v, want an empty list", got)
	}
}

// Every caller reads its project's mirror in one query and judges in memory, so the rows arrive
// as a slice and are indexed by key once.
func TestRowsIndexesTheProjectByKey(t *testing.T) {
	rows := Rows([]record.DispatchIssueRow{
		{Key: "LEGION-900", Project: "LEGION", Status: "todo", Blockers: []string{"LEGION-901"}, Seq: 2},
		{Key: "LEGION-901", Project: "LEGION", Status: "done", Blockers: []string{}, Seq: 1},
	})
	want := map[string]Row{
		"LEGION-900": {Status: "todo", Blockers: []string{"LEGION-901"}},
		"LEGION-901": {Status: "done", Blockers: []string{}},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("Rows = %#v, want %#v", rows, want)
	}
	if got := Unfinished(rows, "LEGION-900"); len(got) != 0 {
		t.Fatalf("Unfinished of an issue whose only blocker is done = %#v, want none", got)
	}
}

// Blocked is the one list the state shows and the tick notice carries: a waiting root promotion
// would skip and an issue held at `admitted` under an issue in Legion's work, together and sorted
// by issue key, each with what it still waits on. A root carrying a run is never judged, an issue
// that is past `admitted` has already started, and an issue whose blockers are all done is in
// neither half.
func TestBlockedListsTheWaitingRootsAndTheHeldChildrenByKey(t *testing.T) {
	rows := map[string]Row{
		"LEGION-211": {Status: "todo", Blockers: []string{"LEGION-300"}},
		"LEGION-212": {Status: "todo", Blockers: []string{"LEGION-301"}},
		"LEGION-213": {Status: "todo", Blockers: []string{"LEGION-302"}},
		"LEGION-209": {Status: "todo", Blockers: []string{"LEGION-300", "LEGION-303"}},
		"LEGION-214": {Status: "todo", Blockers: []string{"LEGION-303"}},
		"LEGION-300": {Status: "in_progress"},
		"LEGION-301": {Status: "in_progress"},
		"LEGION-303": {Status: "done"},
	}
	issues := []record.Issue{
		// A root at work, with a child held behind an unfinished issue and one whose blockers
		// are all done.
		chainIssue("LEGION-208", "", phase.Implementing),
		chainIssue("LEGION-209", "LEGION-208", phase.Admitted),
		chainIssue("LEGION-214", "LEGION-208", phase.Admitted),
		// A child already past `admitted`: its own start read the links once and is done with them.
		chainIssue("LEGION-215", "LEGION-208", phase.Planning),
		// Three roots in the waiting line: one blocked, one carrying a run, and one whose blocker
		// Dispatch has not recorded at all.
		chainIssue("LEGION-211", "", phase.Admitted),
		chainIssue("LEGION-212", "", phase.Testing),
		chainIssue("LEGION-213", "", phase.Admitted),
	}
	chains := chainsOf(t, issues)
	// The line as admission reads it: three roots whose first starts have still to come.
	line := []record.LineEntry{
		{Issue: issues[4], Kind: record.LineQueuedRoot},
		{Issue: issues[5], Kind: record.LineQueuedRoot},
		{Issue: issues[6], Kind: record.LineQueuedRoot},
	}

	want := []record.BlockedIssue{
		{Issue: "LEGION-209", BlockedBy: []string{"LEGION-300"}},
		{Issue: "LEGION-211", BlockedBy: []string{"LEGION-300"}},
		{Issue: "LEGION-213", BlockedBy: []string{"LEGION-302"}},
	}
	if got := Blocked(rows, issues, line, record.LineFacts{Chains: chains}); !reflect.DeepEqual(got, want) {
		t.Fatalf("Blocked = %#v, want %#v by issue key", got, want)
	}
}

// Nothing blocked is an empty list, never nil: the state serves it as an array and the notice
// leaves it out.
func TestBlockedIsEmptyWhenNothingWaitsOnALink(t *testing.T) {
	issues := []record.Issue{chainIssue("LEGION-208", "", phase.Implementing), chainIssue("LEGION-209", "LEGION-208", phase.Admitted)}
	got := Blocked(map[string]Row{}, issues, nil, record.LineFacts{Chains: chainsOf(t, issues)})
	if len(got) != 0 || got == nil {
		t.Fatalf("Blocked = %#v, want an empty list", got)
	}
}

// chainIssue is a record of the test project in Legion's work, standing at at, under the
// Dispatch parent named ("" for a root).
func chainIssue(key, parent string, at phase.Phase) record.Issue {
	issue := record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: key,
		Project: "LEGION", Title: key, Phase: at, Generation: 1, Status: "in_progress", Rank: key}
	if parent != "" {
		issue.Parent = &parent
	}
	return issue
}

// chainsOf records issues and writes each one's row of the copy of Dispatch's issues under the
// parent its record names, then reads the project's chains as a decision pass does
// (record.ReadChains): the chain an issue stands on is read from the copy and nowhere else.
func chainsOf(t *testing.T, issues []record.Issue) *record.Chains {
	t.Helper()
	var chains *record.Chains
	inTx(t, migratedPool(t), func(tx pgx.Tx) {
		ctx, records := context.Background(), record.NewStore()
		keys := make([]string, len(issues))
		for i, issue := range issues {
			keys[i] = issue.Key
			parent := ""
			if issue.Parent != nil {
				parent = *issue.Parent
			}
			if err := records.PutIssue(ctx, tx, issue); err != nil {
				t.Fatalf("put %s: %v", issue.Key, err)
			}
			if err := records.PutDispatchIssue(ctx, tx, record.DispatchReading{Key: issue.Key, Project: issue.Project, Seq: 1,
				Status: record.Carry(issue.Status), Parent: record.Carry(parent)}); err != nil {
				t.Fatalf("put the copy's row of %s: %v", issue.Key, err)
			}
		}
		var err error
		if chains, err = record.ReadChains(ctx, tx, records, "LEGION", keys); err != nil {
			t.Fatalf("read the chains: %v", err)
		}
	})
	return chains
}

// What a started issue waits on, for the release of its row, is asked only of an issue whose
// phase keeps a row (record.KeepsSlot). The same unfinished link holds an issue in implementing
// and holds nothing of one past its review, held from its review, or in its merge, so the rule
// that names the wait names the phase as well and no order of the admission pass's releases
// decides it.
func TestAStartedIssuePastItsReviewWaitsOnNothing(t *testing.T) {
	rows := Rows([]record.DispatchIssueRow{{Key: "LEGION-9", Status: "in_progress"}})
	for _, tc := range []struct {
		at   phase.Phase
		from phase.Phase
		want []string
	}{
		{at: phase.Implementing, want: []string{"LEGION-9"}},
		{at: phase.Held, from: phase.Reviewing, want: []string{"LEGION-9"}},
		{at: phase.Accepting},
		{at: phase.Held, from: phase.Accepting},
		{at: phase.Retro},
		{at: phase.Merging},
	} {
		issue := record.Issue{Key: "LEGION-1", Tree: "LEGION-1", Phase: tc.at, Status: "in_progress", DispatchStatus: "in_progress"}
		if tc.from != "" {
			issue.Hold = &record.Hold{From: tc.from, Reason: record.HoldEscalated}
		}
		rows["LEGION-1"] = Row{Status: "in_progress", Blockers: []string{"LEGION-9"}}
		if got := waitsOf(rows, issue, record.LineFacts{}); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("an issue in %s (held from %q) waits on %v, want %v", tc.at, tc.from, got, tc.want)
		}
	}
}
