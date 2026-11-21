package projection

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// H36, decision 17: the state shows why each issue the work hold holds waits — `why: held`, with
// the version that closed its tree's gate — for each member of a running tree whose hold the record
// keeps (record.WorkHold) and whose work is a phase worker's. The root, whose architect works on, a
// member that has not started, a closed member, a tree the record holds no hold of, a hold of an
// older generation of its tree and a tree that lingers carry no such answer.
func TestWorkHoldShowsWhyInTheState(t *testing.T) {
	approved := 3
	closedRow := func(root string) map[record.DesignStep]record.DesignGate {
		return map[record.DesignStep]record.DesignGate{record.DesignStepSpec: {Issue: root, Step: record.DesignStepSpec,
			ArtifactID: "artifact-" + root[len("LEGION-"):], LatestVersion: 4, ApprovedVersion: &approved,
			ApprovedMark: record.EventMark{Seq: 3, At: projectedAt.Add(-time.Hour)}, EverOpened: true}}
	}
	readmitted := lineIssue("LEGION-900", "LEGION-900", "in_progress", phase.Admitted, "N")
	readmitted.Generation = 2
	lingering := lineIssue("LEGION-950", "LEGION-950", "in_progress", phase.Admitted, "P")
	lingerUntil := projectedAt.Add(time.Hour)
	lingering.LingerUntil = &lingerUntil
	hold := func(tree string, generation uint64) record.WorkHold {
		return record.WorkHold{Project: "LEGION", Tree: tree, RootGeneration: generation, Step: record.DesignStepSpec,
			ArtifactID: "artifact-" + tree[len("LEGION-"):], Version: 4, ClosedAt: projectedAt.Add(-30 * time.Minute)}
	}
	store := projectionStore{
		issues: []record.Issue{
			lineIssue("LEGION-800", "LEGION-800", "in_progress", phase.Admitted, "A"),
			lineIssue("LEGION-801", "LEGION-800", "in_progress", phase.Implementing, "B"),
			lineIssue("LEGION-802", "LEGION-800", "in_progress", phase.Merging, "C"),
			lineIssue("LEGION-803", "LEGION-800", "todo", phase.Admitted, "D"),
			lineIssue("LEGION-804", "LEGION-800", "done", phase.Done, "E"),
			lineIssue("LEGION-850", "LEGION-850", "in_progress", phase.Admitted, "F"),
			lineIssue("LEGION-851", "LEGION-850", "in_progress", phase.Testing, "G"),
			readmitted,
			lineIssue("LEGION-901", "LEGION-900", "in_progress", phase.Reviewing, "O"),
			lingering,
			lineIssue("LEGION-951", "LEGION-950", "in_progress", phase.Retro, "Q"),
		},
		slots: []record.Slot{
			{Issue: "LEGION-801", Index: 0, AdmittedAt: projectedAt},
			{Issue: "LEGION-851", Index: 1, AdmittedAt: projectedAt},
			{Issue: "LEGION-901", Index: 2, AdmittedAt: projectedAt},
		},
		lifecycles: []string{"LEGION-800", "LEGION-850", "LEGION-900", "LEGION-950"},
		gates: map[string]map[record.DesignStep]record.DesignGate{
			"LEGION-800": closedRow("LEGION-800"), "LEGION-850": closedRow("LEGION-850"),
			"LEGION-900": closedRow("LEGION-900"), "LEGION-950": closedRow("LEGION-950"),
		},
		// LEGION-850's tree is closed under `merge`, which keeps no record of a hold; LEGION-900's
		// record is of the generation its re-admission ended.
		workHolds: []record.WorkHold{hold("LEGION-800", 1), hold("LEGION-900", 1), hold("LEGION-950", 1)},
	}

	got, err := Project(context.Background(), Inputs{Store: store, Project: "LEGION", Now: projectedAt})
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	held := func(stands phase.Phase) *api.WhyView {
		return &api.WhyView{State: api.WhyHeld, Reason: "Its phase is " + string(stands) + ", and the work hold holds the work of its tree " +
			"LEGION-800 until every gated step stands open again: version 4 of its spec document (artifact-800) closed the design gate."}
	}
	for key, want := range map[string]*api.WhyView{"LEGION-801": held(phase.Implementing), "LEGION-802": held(phase.Merging)} {
		if got := got.Issues[key].Why; !reflect.DeepEqual(got, want) {
			t.Errorf("%s's why = %+v, want %+v", key, got, want)
		}
	}
	for key, view := range got.Issues {
		if key == "LEGION-801" || key == "LEGION-802" {
			continue
		}
		if view.Why != nil && view.Why.State == api.WhyHeld {
			t.Errorf("%s's why = %+v, want no held answer", key, view.Why)
		}
	}
}
