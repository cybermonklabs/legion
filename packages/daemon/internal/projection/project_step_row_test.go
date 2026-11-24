package projection

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A role that stands at two steps of a task has a row for each, and the state serves its worker
// from its row now, the one with the highest entry (record.PhaseRowOf), as the admission pass
// weighs it: in `implementer:a reviewer implementer:a` at step 3, the row of step 3, at its entry,
// with no report yet; after a return to step 1, the row of step 1 again, which the return raised
// above step 3's, though step 3's row holds a report. Before, the served worker was the role's last
// row in the store's order, step 3's at both places, and the served entry its first row's.
func TestTheStateServesARolesRowNow(t *testing.T) {
	implementer := claim.Token("legion-legion-legion-208-implementer")
	at := func(minute int) time.Time { return time.Date(2026, 11, 22, 7, minute, 0, 0, time.UTC) }
	for _, tc := range []struct {
		name  string
		rows  []record.PhaseRow
		entry int64
	}{
		{"step 3 runs", []record.PhaseRow{
			{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: implementer, StepTask: "a", StepPlace: 1, Entry: 1, EntryRaisedAt: at(1),
				HandoffRecord: "implement-1", HandoffVersion: 1},
			{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: implementer, StepTask: "a", StepPlace: 3, Entry: 3, EntryRaisedAt: at(3)},
		}, 3},
		{"step 1 runs again after a return", []record.PhaseRow{
			{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: implementer, StepTask: "a", StepPlace: 1, Entry: 4, EntryRaisedAt: at(4)},
			{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: implementer, StepTask: "a", StepPlace: 3, Entry: 3, EntryRaisedAt: at(3),
				HandoffRecord: "implement-2", HandoffVersion: 2},
		}, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := projectionStore{
				issues: []record.Issue{{Key: "LEGION-208", Project: "LEGION", Repo: "acme/widgets", DefaultBranch: "main",
					GitHubRepo: "acme/widgets", Tree: "LEGION-208", Status: "in_progress", Phase: phase.Implementing, Generation: 1}},
				phases: map[string][]record.PhaseRow{"LEGION-208": tc.rows},
			}
			got, err := Project(context.Background(), Inputs{Store: store, Project: "LEGION", Claims: []supervise.Claim{
				{Token: implementer, Issue: "LEGION-208", Role: claim.RoleImplementer, State: supervise.StateIdle, Session: "ses_implementer"},
			}, Now: projectedAt})
			if err != nil {
				t.Fatalf("Project: %v", err)
			}
			issue := got.Issues["LEGION-208"]
			if issue.PhaseEntry != tc.entry {
				t.Errorf("the served phase entry = %d, want %d, the entry of the implementer's row now", issue.PhaseEntry, tc.entry)
			}
			worker, served := issue.Workers[claim.RoleImplementer]
			if !served {
				t.Fatal("the implementer is served no worker")
			}
			if worker.HandoffRecord != nil {
				t.Errorf("the served implementer's handoff record = %+v, want none, as its row now holds none", worker.HandoffRecord)
			}
		})
	}
}
