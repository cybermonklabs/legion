package projection

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// routeRecords is a store as the state route reads it (internal/daemon's projectRecords): its
// Issues are the project's own records alone, since the issues table is shared by every project's
// daemon, and every other read is the store's own.
type routeRecords struct {
	projectionStore
	project string
}

func (s routeRecords) Issues(ctx context.Context, tx pgx.Tx) ([]record.Issue, error) {
	all, err := s.projectionStore.Issues(ctx, tx)
	if err != nil {
		return nil, err
	}
	var own []record.Issue
	for _, issue := range all {
		if issue.Project == s.project {
			own = append(own, issue)
		}
	}
	return own, nil
}

// The admission block has one assembly (blockers.AdmissionOf), which the state route serves over
// its judgement (blockers.JudgeAdmission) and the admission pass's state event carries, both
// judged from the pass's own read: the project's live records and its own slots. It holds the
// block the state serves (Project) to the judgement's block as the event carries it
// (record.AdmissionBlock), over the store the controller-claim golden projects and over one
// holding what the two reads part on: done issues — a child, a child holding a slot still, a
// parent on a member's chain, a root whose linger ran out — another project's slot, and a
// lingering tree with a member still in the workflow.
func TestTheJudgedAdmissionBlockIsTheBlockTheStateServes(t *testing.T) {
	architect, err := claim.NewToken("legion", "LEGION-208", claim.RoleArchitect)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		store  projectionStore
		cap    int
		filled bool
	}{
		{name: "the controller-claim golden's store", store: controllerClaimStore(architect, time.Date(2026, 10, 6, 18, 2, 11, 0, time.UTC)), cap: 2},
		{name: "done issues, another project's slot and a lingering tree", store: partingStore(), cap: 3, filled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			records := routeRecords{projectionStore: tc.store, project: "LEGION"}
			state, err := Project(ctx, Inputs{Store: records, Project: "LEGION", Cap: tc.cap, Now: projectedAt})
			if err != nil {
				t.Fatalf("Project: %v", err)
			}
			served, err := json.Marshal(state.Admission)
			if err != nil {
				t.Fatalf("encode the served block: %v", err)
			}
			slots, err := records.Slots(ctx, nil)
			if err != nil {
				t.Fatalf("read the slots: %v", err)
			}
			judged, err := blockers.JudgeAdmission(ctx, nil, records, "LEGION", nil, slots, tc.cap, projectedAt)
			if err != nil {
				t.Fatalf("JudgeAdmission: %v", err)
			}
			carried, err := json.Marshal(judged.Block)
			if err != nil {
				t.Fatalf("encode the judged block as its event carries it: %v", err)
			}
			if !bytes.Equal(carried, served) {
				t.Fatalf("the judged block, as its event carries it, is\n%s\nwant the block the state serves\n%s", carried, served)
			}
			block := judged.Block
			if tc.filled && (len(block.Active) == 0 || len(block.Waiting) == 0 || len(block.Trees) == 0 || len(block.Blocked) == 0 || len(block.Held) == 0) {
				t.Fatalf("the block %s leaves a list empty; this case proves nothing of that list", served)
			}
		})
	}
}

// partingStore is a project whose live records and whose whole record part. LEGION-400's tree
// runs: the root holds slot 0, its child LEGION-403 slot 1, and LEGION-410, a child already done,
// still holds slot 2, which the next release frees. LEGION-401 is a done child, and the parent of
// LEGION-402, which the workflow holds at admitted behind an issue in progress, so the live records
// reach 402's chain only through the done parent's own record. LEGION-420's tree lingers with a
// member still in the workflow, and a done member beside it. LEGION-450 is a root whose linger ran
// out. LEGION-430 and LEGION-431 wait in the line, 431 behind an issue nobody has finished.
// LEGION-440 is a root a person parked in backlog. OTHER-1 is another project's issue holding the
// shared table's slot 3.
func partingStore() projectionStore {
	lingers, lingered := projectedAt.Add(time.Hour), projectedAt.Add(-time.Hour)
	parent := func(key string) *string { return &key }
	issue := func(key, tree, status string, at phase.Phase, shape func(*record.Issue)) record.Issue {
		issue := record.Issue{Key: key, Project: "LEGION", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Tree: tree, Status: status, Phase: at, Generation: 1}
		if shape != nil {
			shape(&issue)
		}
		return issue
	}
	return projectionStore{
		issues: []record.Issue{
			issue("LEGION-400", "LEGION-400", "in_progress", phase.Implementing, func(i *record.Issue) { i.HandedOver, i.Rank = true, "a0" }),
			issue("LEGION-401", "LEGION-400", "done", phase.Done, func(i *record.Issue) { i.Parent = parent("LEGION-400") }),
			issue("LEGION-402", "LEGION-400", "todo", phase.Admitted, func(i *record.Issue) { i.Parent = parent("LEGION-401") }),
			issue("LEGION-403", "LEGION-400", "in_progress", phase.Planning, func(i *record.Issue) { i.Parent = parent("LEGION-400") }),
			issue("LEGION-410", "LEGION-400", "done", phase.Done, func(i *record.Issue) { i.Parent = parent("LEGION-400") }),
			issue("LEGION-420", "LEGION-420", "done", phase.Done, func(i *record.Issue) { i.HandedOver, i.Rank, i.LingerUntil = true, "a3", &lingers }),
			issue("LEGION-421", "LEGION-420", "in_progress", phase.Implementing, func(i *record.Issue) { i.Parent = parent("LEGION-420") }),
			issue("LEGION-422", "LEGION-420", "done", phase.Done, func(i *record.Issue) { i.Parent = parent("LEGION-420") }),
			issue("LEGION-430", "LEGION-430", "todo", phase.Admitted, func(i *record.Issue) { i.HandedOver, i.Rank = true, "a1" }),
			issue("LEGION-431", "LEGION-431", "todo", phase.Admitted, func(i *record.Issue) { i.HandedOver, i.Rank = true, "a2" }),
			issue("LEGION-440", "LEGION-440", "backlog", phase.Admitted, func(i *record.Issue) { i.HandedOver, i.Rank = true, "a4" }),
			issue("LEGION-450", "LEGION-450", "done", phase.Done, func(i *record.Issue) { i.HandedOver, i.Rank, i.LingerUntil = true, "a5", &lingered }),
			{Key: "OTHER-1", Project: "OTHER", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Tree: "OTHER-1", Status: "in_progress", Phase: phase.Implementing, Generation: 1, HandedOver: true},
		},
		slots: []record.Slot{
			{Issue: "LEGION-400", Index: 0, AdmittedAt: projectedAt.Add(-3 * time.Hour)},
			{Issue: "LEGION-403", Index: 1, AdmittedAt: projectedAt.Add(-2 * time.Hour)},
			{Issue: "LEGION-410", Index: 2, AdmittedAt: projectedAt.Add(-2 * time.Hour)},
			{Issue: "OTHER-1", Index: 3, AdmittedAt: projectedAt.Add(-time.Hour)},
		},
		lifecycles: []string{"LEGION-400", "LEGION-420"},
		mirror: []record.DispatchIssueRow{
			{Key: "LEGION-402", Project: "LEGION", Status: "todo", Blockers: []string{"LEGION-499"}, Seq: 1},
			{Key: "LEGION-430", Project: "LEGION", Status: "todo", Blockers: []string{}, Seq: 2},
			{Key: "LEGION-431", Project: "LEGION", Status: "todo", Blockers: []string{"LEGION-498"}, Seq: 3},
			{Key: "LEGION-498", Project: "LEGION", Status: "todo", Blockers: []string{}, Seq: 4},
			{Key: "LEGION-499", Project: "LEGION", Status: "in_progress", Blockers: []string{}, Seq: 5},
		},
	}
}
