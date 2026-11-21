package admit

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// drawnAs is one experiment's answer for a unit, as one line, so two answers are compared whole and
// a failure names what each holds: the arm drawn, the mode it was drawn under and the instant.
func drawnAs(variant, mode string, at *time.Time) string {
	when := "no draw"
	if at != nil {
		when = at.UTC().Format(time.RFC3339Nano)
	}
	return fmt.Sprintf("variant %q drawn as %q at %s", variant, mode, when)
}

// noDraw is what a unit holding no row for an experiment reads as: the state of every tree opened
// before that experiment was declared, and of every unit of a project that does not declare it.
const noDraw = "no row"

// answerFor is the answer key holds for the experiment named name, read back from the assignment
// row the draw writes (record.Store.AssignmentsOf), and noDraw where key holds no row for it.
func answerFor(t *testing.T, pool *pgxpool.Pool, key, name string) string {
	t.Helper()
	for _, assignment := range assignments(t, pool, key) {
		if assignment.Experiment == name {
			return drawnAs(assignment.Variant, assignment.Mode, assignment.DrawnAt)
		}
	}
	return noDraw
}

// memoryOf is answerFor the experiment the project's `memory` block declares, which is named
// memory (config.Project.ExperimentsDeclared).
func memoryOf(t *testing.T, pool *pgxpool.Pool, key string) string {
	t.Helper()
	return answerFor(t, pool, key, memoryExperiment)
}

// depthOf is answerFor the twin experiment every test here declares beside the `memory` block's
// (reviewDepth): the same facts, for an experiment that is not memory.
func depthOf(t *testing.T, pool *pgxpool.Pool, key string) string {
	t.Helper()
	return answerFor(t, pool, key, depthExperiment)
}

// assignments is every answer key holds, in experiment order.
func assignments(t *testing.T, pool *pgxpool.Pool, key string) []record.Assignment {
	t.Helper()
	var held []record.Assignment
	inTx(t, pool, func(tx pgx.Tx) {
		var err error
		held, err = record.NewStore().AssignmentsOf(context.Background(), tx, key)
		if err != nil {
			t.Fatalf("read the experiment assignments of %s: %v", key, err)
		}
	})
	return held
}

// answers is every answer key holds as one line each, in experiment order: what a pass that draws
// nothing must leave exactly as it stands.
func answers(t *testing.T, pool *pgxpool.Pool, key string) []string {
	t.Helper()
	var read []string
	for _, assignment := range assignments(t, pool, key) {
		read = append(read, assignment.Experiment+": "+drawnAs(assignment.Variant, assignment.Mode, assignment.DrawnAt))
	}
	return read
}

const (
	// memoryExperiment is the name the `memory` block's experiment is declared under, and
	// depthExperiment the name of the twin declared beside it in every test here.
	memoryExperiment = "memory"
	depthExperiment  = "review-depth"
)

// declaring is the experiments a project with this `memory` block declares: the memory
// experiment, as config.Project.ExperimentsDeclared maps it, and a tree experiment of the
// project's own beside it at the same mode, so every fact below is read twice — once for the
// block and once for an experiment that is not it. A project that names no mode writes no block
// and declares the memory experiment alone, at mode off: that is a deployment that declares
// nothing of its own, and its trees are drawn for memory all the same.
func declaring(mode config.ExperimentMode) []config.Experiment {
	project := config.Project{Memory: config.Memory{Mode: mode}}
	if mode == "" {
		return project.ExperimentsDeclared("projects.LEGION")
	}
	return append(project.ExperimentsDeclared("projects.LEGION"), config.Experiment{
		Name: depthExperiment, Mode: mode, Unit: config.ExperimentTree, OnVariant: "deep",
		Variants: []config.Variant{{Name: "deep", Weight: 1}, {Name: "shallow", Weight: 1}},
	})
}

// refusingSource fails the test when it is read: the source of a pass that must draw nothing, and
// of a mode that decides without one.
func refusingSource(t *testing.T) func(uint64) (uint64, error) {
	return func(bound uint64) (uint64, error) {
		t.Errorf("admission read the random source on a pass that draws nothing (bound %d)", bound)
		return 0, nil
	}
}

// pinnedSource answers value below every bound: the draw of a `random` experiment is then the arm
// whose cumulative weight value falls below, which for two arms of weight 1 is the first arm at 0
// and the second at 1.
func pinnedSource(value uint64) func(uint64) (uint64, error) {
	return func(uint64) (uint64, error) { return value, nil }
}

// Admission draws a tree's experiments when it opens that tree for the first time, under the mode
// each is declared at: `off` draws no arm at all, `on` draws the arm on_variant names, and
// `random` draws what the source says, both ways. A project whose file declares nothing — no
// `memory` block and no experiment of its own — declares the memory experiment at mode off, so
// its tree is drawn at no arm for memory and holds no row for anything else. Each answer is the
// tree root's own row, written with the mode it was drawn under and the time of the draw. A
// later pass under a changed mode leaves every row exactly as it stands: the tree runs under the
// arms it was drawn, for as long as it lives.
func TestAdmissionDrawsEachTreesMemoryGroupUnderTheProjectsMode(t *testing.T) {
	const root = "LEGION-1"
	for _, tc := range []struct {
		name string
		mode config.ExperimentMode
		// value is what the pinned source answers, read only under `random`; reads says whether
		// the pass reads the source at all, and a pass that must not reads a refusing one.
		value uint64
		reads bool
		// word is the mode each answer records, which is never the empty string: a block that
		// names no mode is recorded `off`, the word an operator would have written for it
		// (config.Memory.Word).
		word                config.ExperimentMode
		memory, reviewDepth string
	}{
		{name: "a project that writes no memory block", mode: "", word: config.ExperimentOff, memory: "", reviewDepth: noDraw},
		{name: "off", mode: config.ExperimentOff, word: config.ExperimentOff, memory: "", reviewDepth: ""},
		{name: "on", mode: config.ExperimentOn, word: config.ExperimentOn, memory: "memory", reviewDepth: "deep"},
		{name: "random, the source drawing the first arm", mode: config.ExperimentRandom, word: config.ExperimentRandom,
			value: 0, reads: true, memory: "memory", reviewDepth: "deep"},
		{name: "random, the source drawing the second arm", mode: config.ExperimentRandom, word: config.ExperimentRandom,
			value: 1, reads: true, memory: "none", reviewDepth: "shallow"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := quiet(t, 1)
			admission.UseExperiments(declaring(tc.mode))
			admission.source = refusingSource(t)
			if tc.reads {
				admission.source = pinnedSource(tc.value)
			}
			seedWaiting(t, pool, root, "A")

			tick(t, pool, admission, "admit")

			wantMemory := drawnAs(tc.memory, string(tc.word), &fixedNow)
			wantDepth := noDraw
			if tc.mode != "" {
				wantDepth = drawnAs(tc.reviewDepth, string(tc.word), &fixedNow)
			}
			if got := memoryOf(t, pool, root); got != wantMemory {
				t.Fatalf("the admitted root was drawn %s for the memory experiment, want %s", got, wantMemory)
			}
			if got := depthOf(t, pool, root); got != wantDepth {
				t.Fatalf("the admitted root was drawn %s for the %s experiment, want %s", got, depthExperiment, wantDepth)
			}
			// A second pass, under modes the operator changed and a source that must not be read,
			// writes nothing at all: not an arm, and no other column of any row either.
			before := answers(t, pool, root)
			admission.UseExperiments(declaring(config.ExperimentOn))
			admission.source = refusingSource(t)
			tick(t, pool, admission, "second-pass")
			if after := answers(t, pool, root); !reflect.DeepEqual(after, before) {
				t.Fatalf("after a pass under changed modes the root holds %v, want %v unchanged", after, before)
			}
		})
	}
}

// The deployment that writes no `memory` block and declares no experiment of its own — the
// deployment this fleet runs — records every new tree at the memory experiment all the same:
// one row, naming the experiment `memory` for the unit `tree`, at no variant, under the word
// `off`, at the instant of the draw. That is the row the daemon has written for such a tree
// since the memory draw existed, and the row the three memory fields of every issue of the tree
// are read off (internal/projection's memoryOf answers `none` under `off` for it). A tree opened
// after this release must hold what a tree opened before it holds, so this reads the whole row
// and not the arm alone.
func TestATreeOfAProjectThatDeclaresNothingIsRecordedAtTheMemoryExperiment(t *testing.T) {
	const root = "LEGION-1"
	pool := migratedPool(t)
	admission := quiet(t, 1)
	// Exactly what the loader gives such a project: a project with no `memory` block and no
	// `experiments` key at all.
	admission.UseExperiments(config.Project{}.ExperimentsDeclared("projects.LEGION"))
	// Mode off decides without a draw, so the source is one that fails the test when it is read.
	admission.source = refusingSource(t)
	seedWaiting(t, pool, root, "A")

	tick(t, pool, admission, "admit")

	held := assignments(t, pool, root)
	if len(held) != 1 {
		t.Fatalf("the admitted root holds %v, want the one memory row", answers(t, pool, root))
	}
	row := held[0]
	if row.Experiment != memoryExperiment || row.Unit != string(config.ExperimentTree) {
		t.Fatalf("the row names the experiment %q for the unit %q, want %q for %q",
			row.Experiment, row.Unit, memoryExperiment, config.ExperimentTree)
	}
	if got, want := drawnAs(row.Variant, row.Mode, row.DrawnAt), drawnAs("", string(config.ExperimentOff), &fixedNow); got != want {
		t.Fatalf("the admitted root was drawn %s for the memory experiment, want %s", got, want)
	}
}

// An issue is drawn once, at its first start, and everything that happens to it afterwards keeps
// every answer byte for byte: a close and a new run (a new generation), the clearing of that
// generation's facts (record.Store.ClearGeneration), and the start that run makes, which opens no
// new lifecycle. Each of those passes runs under modes the operator has since changed and a source
// that fails the test when it is read, so a second draw would be loud.
func TestADrawnTreeKeepsItsMemoryGroupThroughEveryLaterPass(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	admission := quiet(t, 1)
	admission.UseExperiments(declaring(config.ExperimentOn))
	admission.source = refusingSource(t)
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(),
		Clock: func() time.Time { return fixedNow }}, nil)
	const root = "LEGION-1"
	seedWaiting(t, pool, root, "A")

	tick(t, pool, admission, "admit")

	wantMemory := drawnAs("memory", string(config.ExperimentOn), &fixedNow)
	wantDepth := drawnAs("deep", string(config.ExperimentOn), &fixedNow)
	if got := memoryOf(t, pool, root); got != wantMemory {
		t.Fatalf("the admitted root was drawn %s for the memory experiment, want %s", got, wantMemory)
	}
	if got := depthOf(t, pool, root); got != wantDepth {
		t.Fatalf("the admitted root was drawn %s for the %s experiment, want %s", got, depthExperiment, wantDepth)
	}
	// From here the project is a random one whose source nothing may read: every pass below starts
	// an issue that was drawn already, so none of them draws.
	admission.UseExperiments(declaring(config.ExperimentRandom))

	// The daemon closes the root, and a person hands it back to Legion: a new run of the same
	// issue (decision 8 of dispatch://INFRA-4730).
	closed := issue(t, pool, root)
	closed.Phase, closed.Status, closed.DispatchStatus = phase.Done, "done", "done"
	putIssue(t, pool, closed)
	// The pass after the close gives the closed root's row back (record.NeedsSlot), as the
	// close's own pass does, so its new run waits in the line for a row like any first start.
	tick(t, pool, admission, "closed")
	apply(t, pool, admission, "root-again", intake.DispatchIssue{Key: root, ResolvedRepo: "acme/widgets", Seq: 5,
		Type: "issue.updated", Status: "todo", Title: root, Rank: "A", HandedOver: handed}, engine)
	if again := issue(t, pool, root); again.Generation != 2 || again.Status != "in_progress" {
		t.Fatalf("the root after its new run's promotion = %#v, want generation 2, in progress", again)
	}
	if got := memoryOf(t, pool, root); got != wantMemory {
		t.Fatalf("after a new run the root was drawn %s, want its first draw's %s", got, wantMemory)
	}

	// Clearing the generation's facts is not the issue's experiments' business either.
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().ClearGeneration(ctx, tx, root, fixedNow); err != nil {
			t.Fatalf("clear the issue's generation: %v", err)
		}
	})
	if got := memoryOf(t, pool, root); got != wantMemory {
		t.Fatalf("after ClearGeneration the root was drawn %s, want its first draw's %s", got, wantMemory)
	}
	if got := depthOf(t, pool, root); got != wantDepth {
		t.Fatalf("after ClearGeneration the root was drawn %s for the %s experiment, want its first draw's %s",
			got, depthExperiment, wantDepth)
	}
}

// assign records answers as an earlier draw of key wrote them: one tree-unit row for each
// experiment declaring(config.ExperimentOn) declares, memory at variant memory and the twin at
// variant, drawn under on at drawnAt.
func assign(t *testing.T, pool *pgxpool.Pool, key, memory, variant string, drawnAt time.Time) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		if _, err := record.NewStore().PutAssignments(context.Background(), tx, key, []record.Assignment{
			{Experiment: memoryExperiment, Unit: string(config.ExperimentTree), Variant: memory, Mode: string(config.ExperimentOn), DrawnAt: &drawnAt},
			{Experiment: depthExperiment, Unit: string(config.ExperimentTree), Variant: variant, Mode: string(config.ExperimentOn), DrawnAt: &drawnAt},
		}); err != nil {
			t.Fatalf("record the answers of %s: %v", key, err)
		}
	})
}

// startUnder records child todo at admitted under parent on Dispatch's chain, with nothing of it
// started, and runs a pass: the line starts its first phase, every gate above it being open.
func startUnder(t *testing.T, pool *pgxpool.Pool, admission *Admission, child, parent string) {
	t.Helper()
	putIssue(t, pool, record.Issue{Key: child, Tree: parent, Parent: record.ParentOf(parent), Repo: "acme/widgets", DefaultBranch: "main",
		GitHubRepo: "acme/widgets", Project: testProject, Title: child, Phase: phase.Admitted, Generation: 1, Status: "todo",
		DispatchStatus: "todo", Rank: "C", LastDispatchSeq: 1})
	tick(t, pool, admission, "start-"+child)
	if !issueOpen(t, pool, child) {
		t.Fatalf("%s was not started by the pass: its lifecycle is not open", child)
	}
}

// An issue that starts under an issue in Legion's work takes that issue's tree-unit answers, and
// they stay what they were drawn as (R17 of dispatch://INFRA-3320): a change of parent onto an issue
// holding other answers, and the passes after it, change none of them, and nothing is drawn again.
func TestAnExperimentDrawStaysAcrossAReparent(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 3)
	admission.UseExperiments(declaring(config.ExperimentRandom))
	admission.source = refusingSource(t)
	const first, second, child = "LEGION-10", "LEGION-20", "LEGION-11"
	drawnFirst, drawnSecond := fixedNow.Add(-2*time.Hour), fixedNow.Add(-time.Hour)
	seedOpenTree(t, pool, first)
	assign(t, pool, first, "memory", "deep", drawnFirst)
	seedSlotted(t, pool, second, "B")
	assign(t, pool, second, "none", "shallow", drawnSecond)

	startUnder(t, pool, admission, child, first)

	want := []string{
		memoryExperiment + ": " + drawnAs("memory", string(config.ExperimentOn), &drawnFirst),
		depthExperiment + ": " + drawnAs("deep", string(config.ExperimentOn), &drawnFirst),
	}
	if got := answers(t, pool, child); !reflect.DeepEqual(got, want) {
		t.Fatalf("%s started under %s holds %v, want %s's answers %v", child, first, got, first, want)
	}
	for _, row := range assignments(t, pool, child) {
		if !row.Carried {
			t.Fatalf("%s's answer %+v is not marked carried, want each one taken from %s", child, row, first)
		}
	}

	moved := issue(t, pool, child)
	moved.Parent = record.ParentOf(second)
	putIssue(t, pool, moved)
	tick(t, pool, admission, "after-the-reparent")
	tick(t, pool, admission, "a-later-pass")

	if got := answers(t, pool, child); !reflect.DeepEqual(got, want) {
		t.Fatalf("%s moved under %s holds %v, want the answers it started with %v", child, second, got, want)
	}
}

// An issue that starts takes the answers of the nearest issue above it in Legion's work that holds
// one, else draws its own (R17 of dispatch://INFRA-3320): under a parent that holds answers it takes
// the parent's over its root's; under a parent that holds none it takes its root's; and a root,
// with nothing above it in work, draws its own.
func TestTheNearestAssignedIssueGivesTheDraw(t *testing.T) {
	drawnRoot, drawnParent := fixedNow.Add(-2*time.Hour), fixedNow.Add(-time.Hour)
	const root, parent, child = "LEGION-10", "LEGION-12", "LEGION-13"
	seed := func(t *testing.T, parentAssigned bool) (*pgxpool.Pool, *Admission) {
		t.Helper()
		pool := migratedPool(t)
		admission := quiet(t, 3)
		admission.UseExperiments(declaring(config.ExperimentRandom))
		admission.source = refusingSource(t)
		seedOpenTree(t, pool, root)
		assign(t, pool, root, "memory", "deep", drawnRoot)
		putIssue(t, pool, record.Issue{Key: parent, Tree: root, Parent: record.ParentOf(root), Repo: "acme/widgets", DefaultBranch: "main",
			GitHubRepo: "acme/widgets", Project: testProject, Title: parent, Phase: phase.Planning, Generation: 1, Status: "in_progress",
			DispatchStatus: "in_progress", Rank: "B", LastDispatchSeq: 1})
		if parentAssigned {
			assign(t, pool, parent, "none", "shallow", drawnParent)
		}
		return pool, admission
	}
	t.Run("the parent holds answers", func(t *testing.T) {
		pool, admission := seed(t, true)
		startUnder(t, pool, admission, child, parent)
		if got, want := depthOf(t, pool, child), drawnAs("shallow", string(config.ExperimentOn), &drawnParent); got != want {
			t.Fatalf("%s under %s took %s, want its parent's %s", child, parent, got, want)
		}
		if got, want := memoryOf(t, pool, child), drawnAs("none", string(config.ExperimentOn), &drawnParent); got != want {
			t.Fatalf("%s under %s took %s for memory, want its parent's %s", child, parent, got, want)
		}
	})
	t.Run("the parent holds none", func(t *testing.T) {
		pool, admission := seed(t, false)
		startUnder(t, pool, admission, child, parent)
		if got, want := depthOf(t, pool, child), drawnAs("deep", string(config.ExperimentOn), &drawnRoot); got != want {
			t.Fatalf("%s under %s took %s, want its root's %s", child, parent, got, want)
		}
	})
	t.Run("nothing above it in work", func(t *testing.T) {
		pool := migratedPool(t)
		admission := quiet(t, 1)
		admission.UseExperiments(declaring(config.ExperimentRandom))
		admission.source = pinnedSource(1)
		seedWaiting(t, pool, root, "A")
		tick(t, pool, admission, "admit")
		if got, want := depthOf(t, pool, root), drawnAs("shallow", string(config.ExperimentRandom), &fixedNow); got != want {
			t.Fatalf("the root drew %s, want its own draw %s", got, want)
		}
		for _, row := range assignments(t, pool, root) {
			if row.Carried {
				t.Fatalf("the root's answer %+v is marked carried, want its own draw", row)
			}
		}
	})
}
