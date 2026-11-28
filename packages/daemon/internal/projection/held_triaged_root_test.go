package projection

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// An issue whose Dispatch parent stands out of the workflow at triage, at phase done and under a
// gate a person approved, has nothing above it in Legion's work, so it is a root (R15 and R21 of
// dispatch://INFRA-3320): labelled and todo, it waits in the line for its first start, and no
// held row names it. Today's daemon held such an issue as the member of a stopped tree; the chain
// makes it the root it is on Dispatch.
func TestAnIssueUnderAParentOutOfTheWorkflowIsARoot(t *testing.T) {
	root := treeRoot("LEGION-200", "triage", phase.Done)
	member := treeMember("LEGION-201", "LEGION-200", "LEGION-200", "todo", phase.Admitted)
	store := projectionStore{
		issues: []record.Issue{root, member},
		gates:  map[string]map[record.DesignStep]record.DesignGate{"LEGION-200": openGate("LEGION-200", projectedAt.Add(-time.Hour))},
	}

	state, err := Project(context.Background(), Inputs{Store: store, Project: "LEGION", Now: projectedAt})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if !slices.Contains(state.Admission.Waiting, "LEGION-201") {
		t.Errorf("admission.waiting = %v, want LEGION-201 waiting for its first start as a root", state.Admission.Waiting)
	}
	for _, row := range state.Admission.Held {
		if row.Issue == "LEGION-201" {
			t.Errorf("admission.held names LEGION-201 as %#v, want no row: it stands in the line", row)
		}
	}
	if !slices.Contains(state.Admission.Trees, "LEGION-201") {
		t.Errorf("admission.trees = %v, want LEGION-201 among the roots in Legion's work", state.Admission.Trees)
	}
}
