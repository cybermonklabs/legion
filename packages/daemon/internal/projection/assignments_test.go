package projection

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// An issue's memory group is its tree's, read off the assignment of the experiment named
// `memory` recorded against the tree's root, and only where that row records a draw: the root
// and every issue of its tree are shown the group, the mode it was drawn under and the instant
// of the draw, a done issue included, and an issue of a tree no draw has reached is shown none
// of the three. There is no ancestor walk: a record's Tree is its root's key.
//
// The two classes the old issues.memory_* columns told apart are told apart by the instant. A
// row that records a draw fills all three whatever arm it drew: the control arm `none` and the
// empty arm an experiment at mode off draws are both the group `none`, which is the word the
// memory draw wrote for a tree under mode off and what a deployment at that mode has always
// read. A row with no instant — a hand-over settling a tree at no arm — fills none of the
// three, as the empty memory_group of such a tree filled none of them: a tree that answered
// nothing goes on answering nothing, and its assignment carries the settled row all the same.
func TestProjectShowsEveryIssueItsTreeRootsMemoryDraw(t *testing.T) {
	drawnAt := projectedAt.Add(-48 * time.Hour)
	issue := func(key, tree string, at phase.Phase, status, rank string) record.Issue {
		return record.Issue{
			Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: tree,
			Phase: at, Status: status, Rank: rank, HandedOver: true,
		}
	}
	store := projectionStore{
		issues: []record.Issue{
			issue("LEGION-208", "LEGION-208", phase.Implementing, "in_progress", "A"),
			// A child of that tree, and a done child of it: both run with what their root was
			// drawn into, and neither holds a row of its own.
			issue("LEGION-209", "LEGION-208", phase.Admitted, "in_progress", "B"),
			issue("LEGION-210", "LEGION-208", phase.Done, "done", "C"),
			// A tree first opened before the setting existed: no row, so no group.
			issue("LEGION-211", "LEGION-211", phase.Implementing, "in_progress", "D"),
			// A tree the mode drew the other way, one an experiment at mode off drew no arm
			// for, and one a hand-over settled at no arm at all.
			issue("LEGION-212", "LEGION-212", phase.Implementing, "in_progress", "E"),
			issue("LEGION-213", "LEGION-213", phase.Implementing, "in_progress", "F"),
			issue("LEGION-214", "LEGION-214", phase.Implementing, "in_progress", "G"),
		},
		assignments: map[string][]record.Assignment{
			"LEGION-208": {{Experiment: "memory", Unit: "tree", Variant: "memory", Mode: "random", DrawnAt: &drawnAt}},
			"LEGION-212": {{Experiment: "memory", Unit: "tree", Variant: "none", Mode: "random", DrawnAt: &drawnAt}},
			"LEGION-213": {{Experiment: "memory", Unit: "tree", Mode: "off", DrawnAt: &drawnAt}},
			"LEGION-214": {{Experiment: "memory", Unit: "tree", Carried: true}},
		},
	}

	got, err := Project(context.Background(), nil, store, "LEGION", nil, nil, nil, nil, projectedAt)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	for _, key := range []string{"LEGION-208", "LEGION-209", "LEGION-210"} {
		view := got.Issues[key]
		if view.MemoryGroup != "memory" || view.MemoryDrawMode != "random" ||
			view.MemoryDrawnAt == nil || !view.MemoryDrawnAt.Equal(drawnAt) {
			t.Errorf("%s = group %q drawn as %q at %v, want its root's memory, random, %s",
				key, view.MemoryGroup, view.MemoryDrawMode, view.MemoryDrawnAt, drawnAt)
		}
	}
	if view := got.Issues["LEGION-211"]; view.MemoryGroup != "" || view.MemoryDrawMode != "" || view.MemoryDrawnAt != nil {
		t.Errorf("the undrawn tree's root = group %q drawn as %q at %v, want none of the three",
			view.MemoryGroup, view.MemoryDrawMode, view.MemoryDrawnAt)
	}
	if view := got.Issues["LEGION-212"]; view.MemoryGroup != "none" || view.MemoryDrawMode != "random" ||
		view.MemoryDrawnAt == nil || !view.MemoryDrawnAt.Equal(drawnAt) {
		t.Errorf("the tree drawn into the control arm = group %q drawn as %q at %v, want none, random, %s",
			view.MemoryGroup, view.MemoryDrawMode, view.MemoryDrawnAt, drawnAt)
	}
	// A deployment at mode off draws every tree the empty arm, which the memory draw wrote as
	// the word `none`: such a tree answers the group `none` with its own mode and instant.
	if view := got.Issues["LEGION-213"]; view.MemoryGroup != "none" || view.MemoryDrawMode != "off" ||
		view.MemoryDrawnAt == nil || !view.MemoryDrawnAt.Equal(drawnAt) {
		t.Errorf("the tree drawn under mode off = group %q drawn as %q at %v, want none, off, %s",
			view.MemoryGroup, view.MemoryDrawMode, view.MemoryDrawnAt, drawnAt)
	}
	// The tree a hand-over settled at no arm answered none of the three before the assignments
	// and answers none of them now, while its assignment still carries the settled row.
	settled := got.Issues["LEGION-214"]
	if settled.MemoryGroup != "" || settled.MemoryDrawMode != "" || settled.MemoryDrawnAt != nil {
		t.Errorf("the tree a hand-over settled at no arm = group %q drawn as %q at %v, want none of the three",
			settled.MemoryGroup, settled.MemoryDrawMode, settled.MemoryDrawnAt)
	}
	if want := []api.AssignmentView{{Experiment: "memory"}}; !reflect.DeepEqual(settled.Assignments, want) {
		t.Errorf("the settled tree runs under %+v, want %+v: the row rides with its empty arm", settled.Assignments, want)
	}
}

// The three memory fields of a tree of a project that writes no `memory` block and declares no
// experiment of its own — the deployment this fleet runs. Such a project declares the memory
// experiment at mode off (config.Project.ExperimentsDeclared), so admission records each new
// tree at no arm under the word `off` with the instant of the draw, and this is the row's own
// shape. The state answers the group `none` under the mode `off` with that instant, for the
// root and for every issue of its tree, which is what it answered for such a tree before any
// experiment was declared: a reader counting the trees a group reached counts the same trees.
func TestProjectAnswersTheMemoryFieldsOfAProjectThatDeclaresNothing(t *testing.T) {
	drawnAt := projectedAt.Add(-48 * time.Hour)
	issue := func(key, tree, rank string) record.Issue {
		return record.Issue{
			Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: tree,
			Phase: phase.Implementing, Status: "in_progress", Rank: rank, HandedOver: true,
		}
	}
	store := projectionStore{
		issues: []record.Issue{
			issue("LEGION-208", "LEGION-208", "A"),
			issue("LEGION-209", "LEGION-208", "B"),
		},
		// The row admission writes for such a tree: the experiment named memory, for the unit
		// tree, at no variant, under the word off, at the instant of the draw.
		assignments: map[string][]record.Assignment{
			"LEGION-208": {{Experiment: config.MemoryExperimentName, Unit: "tree", Mode: string(config.ExperimentOff), DrawnAt: &drawnAt}},
		},
	}

	got, err := Project(context.Background(), nil, store, "LEGION", nil, config.Project{}.ExperimentsDeclared("projects.LEGION"), nil, nil, projectedAt)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	for _, key := range []string{"LEGION-208", "LEGION-209"} {
		view := got.Issues[key]
		if view.MemoryGroup != "none" || view.MemoryDrawMode != "off" ||
			view.MemoryDrawnAt == nil || !view.MemoryDrawnAt.Equal(drawnAt) {
			t.Errorf("%s = group %q drawn as %q at %v, want none, off, %s",
				key, view.MemoryGroup, view.MemoryDrawMode, view.MemoryDrawnAt, drawnAt)
		}
	}
}

// Every assignment that applies to an issue is on its view: its tree's, for an experiment whose
// unit is the tree, and its own, for one whose unit is the issue. A root's own key holds both
// kinds and every row of it applies to the root itself; a child takes its root's tree rows and
// its own issue rows, and no issue row of its root's and no tree row of a sibling's. An issue no
// row applies to carries none, which leaves the key off the wire.
func TestProjectShowsEveryIssueTheExperimentsItRunsUnder(t *testing.T) {
	drawnAt := projectedAt.Add(-48 * time.Hour)
	issue := func(key, tree string, rank string) record.Issue {
		return record.Issue{
			Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: tree,
			Phase: phase.Implementing, Status: "in_progress", Rank: rank, HandedOver: true,
		}
	}
	store := projectionStore{
		issues: []record.Issue{
			issue("LEGION-208", "LEGION-208", "A"),
			issue("LEGION-209", "LEGION-208", "B"),
			issue("LEGION-210", "LEGION-208", "C"),
			issue("LEGION-211", "LEGION-211", "D"),
		},
		// The root's key holds the tree experiment its whole tree runs under and the issue
		// experiment drawn for the root itself; each child holds its own issue row, and
		// LEGION-210 holds none, so it runs under the tree row alone.
		assignments: map[string][]record.Assignment{
			"LEGION-208": {
				{Experiment: "memory", Unit: "tree", Variant: "memory", Mode: "random", DrawnAt: &drawnAt},
				{Experiment: "plan-length", Unit: "issue", Variant: "short", Mode: "random", DrawnAt: &drawnAt},
			},
			"LEGION-209": {{Experiment: "plan-length", Unit: "issue", Variant: "long", Mode: "random", DrawnAt: &drawnAt}},
		},
	}

	got, err := Project(context.Background(), nil, store, "LEGION", nil, nil, nil, nil, projectedAt)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	memory := api.AssignmentView{Experiment: "memory", Variant: "memory", Mode: "random", DrawnAt: &drawnAt}
	for key, want := range map[string][]api.AssignmentView{
		"LEGION-208": {memory, {Experiment: "plan-length", Variant: "short", Mode: "random", DrawnAt: &drawnAt}},
		"LEGION-209": {memory, {Experiment: "plan-length", Variant: "long", Mode: "random", DrawnAt: &drawnAt}},
		"LEGION-210": {memory},
		"LEGION-211": nil,
	} {
		if got := got.Issues[key].Assignments; !reflect.DeepEqual(got, want) {
			t.Errorf("%s runs under %+v, want %+v", key, got, want)
		}
	}
}

// The assignments ride in the order the project's file declares the experiments — the `memory`
// block's experiment first, then the `experiments` key's own — and not the order the record keys
// its rows by, which is the experiments' names. An experiment the file no longer names keeps its
// row and rides after every declared one.
//
// The order is put on for every count, one declared experiment included: the number of
// experiments a project declares is not the number of rows an issue holds, because a row stays
// in the record after its experiment leaves the file. The block-only deployment below declares
// one experiment and its root holds two rows, and the undeclared one rides second although its
// name sorts first.
func TestProjectOrdersEveryIssuesAssignmentsAsTheProjectDeclaresThem(t *testing.T) {
	drawnAt := projectedAt.Add(-48 * time.Hour)
	root := record.Issue{
		Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208",
		Phase: phase.Implementing, Status: "in_progress", Rank: "A", HandedOver: true,
	}
	child := root
	child.Key, child.Rank = "LEGION-209", "B"
	row := func(experiment, unit, variant string) record.Assignment {
		return record.Assignment{Experiment: experiment, Unit: unit, Variant: variant, Mode: "random", DrawnAt: &drawnAt}
	}
	// As record.Store.Assignments answers them: by the experiments' names.
	store := projectionStore{
		issues: []record.Issue{root, child},
		assignments: map[string][]record.Assignment{
			"LEGION-208": {row("dropped", "tree", "on"), row("memory", "tree", "memory"), row("review-depth", "tree", "deep")},
			"LEGION-209": {row("plan-length", "issue", "long")},
		},
	}
	memory := config.Experiment{
		Name: config.MemoryExperimentName, Mode: config.ExperimentRandom, Unit: config.ExperimentTree,
		Variants: []config.Variant{{Name: "memory", Weight: 1}, {Name: "none", Weight: 1}},
	}
	named := func(t *testing.T, views []api.AssignmentView) []string {
		t.Helper()
		names := make([]string, len(views))
		for i, view := range views {
			names[i] = view.Experiment
		}
		return names
	}

	// A deployment that writes the `memory` block and no `experiments` key: one declared
	// experiment, and a row of an experiment the file no longer names beside it.
	block, err := Project(context.Background(), nil, store, "LEGION", nil, []config.Experiment{memory}, nil, nil, projectedAt)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if got, want := named(t, block.Issues["LEGION-208"].Assignments), []string{"memory", "dropped", "review-depth"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the block-only deployment serves %v, want %v: the one declared experiment first, then the undeclared rows in their names", got, want)
	}

	// The same rows under a file that declares three experiments, the block's among them.
	declared := []config.Experiment{
		memory,
		{Name: "review-depth", Mode: config.ExperimentOn, Unit: config.ExperimentTree, OnVariant: "deep",
			Variants: []config.Variant{{Name: "deep", Weight: 1}, {Name: "shallow", Weight: 1}}},
		{Name: "plan-length", Mode: config.ExperimentRandom, Unit: config.ExperimentIssue,
			Variants: []config.Variant{{Name: "long", Weight: 1}, {Name: "short", Weight: 1}}},
	}
	got, err := Project(context.Background(), nil, store, "LEGION", nil, declared, nil, nil, projectedAt)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	if got, want := named(t, got.Issues["LEGION-208"].Assignments), []string{"memory", "review-depth", "dropped"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the root serves %v, want %v: the file's order, with the experiment it no longer names last", got, want)
	}
	if got, want := named(t, got.Issues["LEGION-209"].Assignments), []string{"memory", "review-depth", "plan-length", "dropped"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the child serves %v, want %v: its tree's rows and its own, in the file's order", got, want)
	}
}
