package projection

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A parent waiting in awaiting_children stands in `admission.blocked` with its open children and
// in neither `admission.active` nor `admission.waiting` (dispatch://INFRA-3320 R11), and
// `admission.held` names it awaiting_children with those children beside. It does so also when a
// blocked_by link holds it as well, which overrides dispatch://INFRA-3036's rule that an issue a
// link holds stands in the line for the whole wait: the phase keeps no slot, so the line places no
// entry of the parent and judges no link of it, and its row in `blocked` names its open children
// alone. A child it waits on that is done is no part of the row.
//
// The parent is a root of its own running tree under an open gate, holding no slot, as the
// admission pass leaves a parent it has moved into the phase; LEGION-401 is the open child the
// architect filed and has not released, and LEGION-402 a child already done.
func TestAWaitingParentStandsInBlockedWithItsOpenChildrenAlone(t *testing.T) {
	const (
		parent   = "LEGION-400"
		open     = "LEGION-401"
		finished = "LEGION-402"
		blocker  = "LEGION-900"
	)
	waited := projectedAt.Add(-2 * time.Hour)
	waiting := treeRoot(parent, "in_progress", phase.AwaitingChildren)
	waiting.ChildrenWait = &record.ChildrenWait{Start: phase.Implementing, Task: "Continue LEGION-400. Phase: implementing.",
		Reason: "a review asked for changes, while these Dispatch children of LEGION-400 were open: LEGION-401 (backlog)", Since: waited}
	waiting.ChildrenWaitedAt = &waited
	children := []record.DispatchIssueRow{
		{Key: open, Project: "LEGION", Status: "backlog", Parent: parent},
		{Key: finished, Project: "LEGION", Status: "done", Parent: parent},
	}
	for _, tc := range []struct {
		name   string
		mirror []record.DispatchIssueRow
	}{
		{name: "its open children alone hold it", mirror: children},
		{name: "a blocked_by link holds it as well", mirror: append(slices.Clone(children),
			record.DispatchIssueRow{Key: parent, Project: "LEGION", Status: "in_progress", Blockers: []string{blocker}},
			record.DispatchIssueRow{Key: blocker, Project: "LEGION", Status: "todo"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := projectionStore{
				issues:     []record.Issue{waiting},
				lifecycles: []string{parent},
				gates:      map[string]map[record.DesignStep]record.DesignGate{parent: openGate(parent, projectedAt.Add(-3*time.Hour))},
				mirror:     tc.mirror,
			}
			projected, err := Project(context.Background(), Inputs{Store: store, Project: "LEGION", Now: projectedAt})
			if err != nil {
				t.Fatalf("Project: %v", err)
			}
			want := []api.BlockedIssue{{Issue: parent, BlockedBy: []string{open}}}
			if got := projected.Admission.Blocked; !slices.EqualFunc(got, want, func(a, b api.BlockedIssue) bool {
				return a.Issue == b.Issue && slices.Equal(a.BlockedBy, b.BlockedBy)
			}) {
				t.Fatalf("admission.blocked = %#v, want %#v: the waiting parent with its open child, and nothing of its link", got, want)
			}
			if slices.Contains(projected.Admission.Active, parent) {
				t.Fatalf("admission.active = %v, want %s out of it: it holds no slot", projected.Admission.Active, parent)
			}
			if slices.Contains(projected.Admission.Waiting, parent) {
				t.Fatalf("admission.waiting = %v, want %s out of it: it has no start to offer a slot to until its children are finished", projected.Admission.Waiting, parent)
			}
			if row := heldRow(t, projected, parent); row.Reason != record.HeldAwaitingChildren || !slices.Equal(row.HeldBy, []string{open}) {
				t.Fatalf("admission.held names %s as %#v, want awaiting_children held by %s", parent, row, open)
			}
			if why := projected.Issues[parent].Why; why == nil || why.State != api.WhyBlocked {
				t.Fatalf("issues.%s.why = %#v, want the blocked state", parent, why)
			}
		})
	}
}
