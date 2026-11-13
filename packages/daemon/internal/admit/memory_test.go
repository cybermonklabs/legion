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
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// drawnAs is one experiment's answer for a unit, as one line, so two answers are compared whole and
// a failure names what each holds: the arm drawn, the mode it was drawn under and the instant. An
// answer with no instant reads the same whatever the other two say, which is what a hand-over
// settles a unit at for an experiment the tree it left never answered.
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

// settled is what a hand-over leaves for an experiment the tree the child left held no answer for:
// a row at no variant, under no mode and at no instant, which the child's own first open then
// writes nothing over.
var settled = drawnAs("", "", nil)

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

// A tree is drawn once, and everything that happens to it afterwards keeps every answer byte for
// byte: a re-admission of its root (a new generation), the clearing of that generation's rows
// (record.Store.ClearTreeGeneration), and the next epoch its lifecycle opens after a cleanup
// confirmed. Each of those passes runs under modes the operator has since changed and a source
// that fails the test when it is read, so a second draw would be loud.
func TestADrawnTreeKeepsItsMemoryGroupThroughEveryLaterPass(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	admission := quiet(t, 1)
	admission.UseExperiments(declaring(config.ExperimentOn))
	admission.source = refusingSource(t)
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(),
		Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
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
	// From here the project is a random one whose source nothing may read: every pass below opens
	// a tree that was drawn already, so none of them draws.
	admission.UseExperiments(declaring(config.ExperimentRandom))

	// The root signs off, lingers, and a person hands it back to Legion: a new generation of the
	// same tree, recorded from the copy read before the draw.
	linger(t, pool, root)
	apply(t, pool, admission, "root-readmitted", intake.DispatchIssue{Key: root, ResolvedRepo: "acme/widgets", Seq: 2,
		Type: "issue.updated", Status: "todo", Title: root, Rank: "A", HandedOver: handed}, engine)
	if readmitted := issue(t, pool, root); readmitted.Generation != 2 {
		t.Fatalf("the re-admitted root is generation %d, want the tree's second", readmitted.Generation)
	}
	if got := memoryOf(t, pool, root); got != wantMemory {
		t.Fatalf("after a re-admission the root was drawn %s, want its first draw's %s", got, wantMemory)
	}

	// Clearing the generation's rows is not the tree's experiments' business either.
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().ClearTreeGeneration(ctx, tx, root, fixedNow); err != nil {
			t.Fatalf("clear the tree's generation: %v", err)
		}
	})
	if got := memoryOf(t, pool, root); got != wantMemory {
		t.Fatalf("after ClearTreeGeneration the root was drawn %s, want its first draw's %s", got, wantMemory)
	}

	// The tree's cleanup is reserved and confirmed and the root goes back into the waiting line:
	// its promotion opens the tree's second epoch, which creates no row and so draws nothing. The
	// seed writes the record again from a copy that carries no answer at all — which is what a
	// re-admission does too, since an answer is a row of its own and record.Store.PutIssue writes
	// none — so what the tree reads afterwards is what the draw wrote and nothing else.
	inTx(t, pool, func(tx pgx.Tx) {
		if _, err := treelifecycle.ReserveCleanup(ctx, tx, "legion", root, treelifecycle.AuthorityWorkflow); err != nil {
			t.Fatalf("reserve the tree's cleanup: %v", err)
		}
		if err := treelifecycle.ConfirmCleanup(ctx, tx, "legion", root, 1); err != nil {
			t.Fatalf("confirm the tree's cleanup: %v", err)
		}
	})
	linger(t, pool, root)
	seedWaiting(t, pool, root, "A")

	tick(t, pool, admission, "next-epoch")

	var epoch int64
	if err := pool.QueryRow(ctx, `select epoch from tree_lifecycles where project = 'legion' and tree = $1`, root).Scan(&epoch); err != nil {
		t.Fatalf("read the tree's lifecycle: %v", err)
	}
	if epoch != 2 {
		t.Fatalf("the tree's lifecycle is at epoch %d, want the second epoch its promotion opened", epoch)
	}
	if got := memoryOf(t, pool, root); got != wantMemory {
		t.Fatalf("after the tree's next epoch the root was drawn %s, want its first draw's %s", got, wantMemory)
	}
	if got := depthOf(t, pool, root); got != wantDepth {
		t.Fatalf("after the tree's next epoch the root was drawn %s for the %s experiment, want its first draw's %s",
			got, depthExperiment, wantDepth)
	}
}

// A child handed over as a tree of its own keeps the arms it has been running under: the carry
// copies every answer of its old tree verbatim, the time of that tree's draw included, and the
// draw its own promotion would make writes nothing. The hand-over here runs an hour later under a
// project the operator has moved to `off`, so a child that drew for itself would carry no arm at
// all, drawn as `off`, at the later hour — three differences from what it must carry.
func TestAHandedOverChildCarriesItsOldTreesMemoryGroupVerbatim(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 2)
	admission.UseExperiments(declaring(config.ExperimentOn))
	admission.source = refusingSource(t)
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
		Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	const root, child = "LEGION-1", "LEGION-2"
	seedWaiting(t, pool, root, "A")

	tick(t, pool, admission, "admit")

	wantMemory := drawnAs("memory", string(config.ExperimentOn), &fixedNow)
	wantDepth := drawnAs("deep", string(config.ExperimentOn), &fixedNow)
	if got := memoryOf(t, pool, root); got != wantMemory {
		t.Fatalf("the admitted root was drawn %s, want %s", got, wantMemory)
	}
	// The root signs off and lingers; an hour later the project is off, and the child is handed to
	// Legion as a root of its own.
	later := fixedNow.Add(time.Hour)
	admission.UseExperiments(declaring(config.ExperimentOff))

	handOverChild(t, pool, admission, engine, root, child, "B", later)

	if got := memoryOf(t, pool, child); got != wantMemory {
		t.Fatalf("the handed-over child was drawn %s, want its old tree's %s verbatim", got, wantMemory)
	}
	if got := depthOf(t, pool, child); got != wantDepth {
		t.Fatalf("the handed-over child was drawn %s for the %s experiment, want its old tree's %s verbatim",
			got, depthExperiment, wantDepth)
	}
	if got := memoryOf(t, pool, root); got != wantMemory {
		t.Fatalf("after the hand-over the old root was drawn %s, want its own %s", got, wantMemory)
	}
}

// A root with no answer before this release — the shape of every tree open when an experiment is
// declared: no assignment row of its own and its tree_lifecycles row already there — is never
// drawn for. Its lifecycle row exists, so no later open creates one, and the project's modes say
// nothing about a tree that is already running. It goes on being admitted, it releases a child
// into its tree, and that child holds no answer either, since a tree's answers are its root's.
// Nothing here is refused and nothing errors: such a tree simply runs without memory, as it has
// all along.
func TestARootWithNoMemoryBeforeThisReleaseIsNeverDrawnAndItsChildCarriesNone(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	admission := quiet(t, 2)
	// A project that would draw every new tree `memory` and `deep`: a draw of this tree would be
	// loud.
	admission.UseExperiments(declaring(config.ExperimentOn))
	admission.source = refusingSource(t)
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(),
		Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	const root, child = "LEGION-1", "LEGION-2"
	// The tree as the release found it: a lifecycle row opened before the experiments existed, and
	// a root recorded with no answer of its own.
	inTx(t, pool, func(tx pgx.Tx) {
		if _, err := treelifecycle.Open(ctx, tx, "legion", root, treelifecycle.AuthorityWorkflow); err != nil {
			t.Fatalf("open the tree's lifecycle as the release found it: %v", err)
		}
	})
	seedWaiting(t, pool, root, "A")

	tick(t, pool, admission, "admit")

	if got := memoryOf(t, pool, root); got != noDraw {
		t.Fatalf("a root whose tree was open before the experiments existed was drawn %s, want %s", got, noDraw)
	}
	if got := depthOf(t, pool, root); got != noDraw {
		t.Fatalf("a root whose tree was open before the experiments existed was drawn %s for the %s experiment, want %s",
			got, depthExperiment, noDraw)
	}
	// A person makes a queued root a child of this tree: the release admission applies on its next
	// pass (adoptQueuedRoots). The child joins the tree and holds no answer of its own.
	parent := root
	putIssue(t, pool, record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Tree: child, Project: testProject, Title: "child", Parent: &parent, Phase: phase.Admitted, Generation: 1,
		Status: "todo", Rank: "B", HandedOver: handed, LastDispatchSeq: 1})

	tick(t, pool, admission, "release-the-child")

	released := issue(t, pool, child)
	if released.Tree != root {
		t.Fatalf("the released child is in tree %s, want %s: it never joined the tree", released.Tree, root)
	}
	if got := answers(t, pool, child); len(got) != 0 {
		t.Fatalf("the released child holds %v, want no answer of its own", got)
	}
	// And no later pass draws for the root either: a re-admission opens the same row again.
	linger(t, pool, root)
	apply(t, pool, admission, "root-readmitted", intake.DispatchIssue{Key: root, ResolvedRepo: "acme/widgets", Seq: 2,
		Type: "issue.updated", Status: "todo", Title: root, Rank: "A", HandedOver: handed}, engine)
	if readmitted := issue(t, pool, root); readmitted.Generation != 2 {
		t.Fatalf("the re-admitted root is generation %d, want the tree's second", readmitted.Generation)
	}
	if got := memoryOf(t, pool, root); got != noDraw {
		t.Fatalf("after a re-admission the undrawn root was drawn %s, want %s", got, noDraw)
	}
}

// A child handed over from a tree nothing ever drew for is never drawn either: a root that leaves
// another tree takes that tree's answers at the hand-over, and the answer of a tree opened before
// an experiment was declared is no arm at all. The carry has nothing to copy and settles the child
// at no arm for every experiment declared at that moment, which is the answer its own first
// promotion then writes nothing over (record.Store.CarryAssignments, which the lifecycle row this
// test seeds for the old tree is what it reads). The project here is at `on` with a source nothing
// may read, so a draw would be loud: the child would carry `memory` and `deep`, drawn as `on`, at
// the hour of its hand-over, where the tree it left holds nothing at all.
func TestAChildHandedOverFromAnUndrawnTreeIsNeverDrawn(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	admission := quiet(t, 2)
	admission.UseExperiments(declaring(config.ExperimentOn))
	admission.source = refusingSource(t)
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
		Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	const root, child = "LEGION-1", "LEGION-2"
	// The tree as the release found it: a lifecycle row opened before the experiments existed, and
	// a root recorded with no answer of its own.
	inTx(t, pool, func(tx pgx.Tx) {
		if _, err := treelifecycle.Open(ctx, tx, "legion", root, treelifecycle.AuthorityWorkflow); err != nil {
			t.Fatalf("open the tree's lifecycle as the release found it: %v", err)
		}
	})
	seedWaiting(t, pool, root, "A")

	tick(t, pool, admission, "admit")

	if got := memoryOf(t, pool, root); got != noDraw {
		t.Fatalf("a root whose tree was open before the experiments existed was drawn %s, want %s", got, noDraw)
	}
	// The root signs off and lingers; an hour later the child is handed to Legion as a root of its
	// own, and the hand-over records which tree it left (record.Issue.HandedOverFrom).
	later := fixedNow.Add(time.Hour)

	handOverChild(t, pool, admission, engine, root, child, "B", later)

	if got := memoryOf(t, pool, child); got != settled {
		t.Fatalf("the handed-over child was drawn %s, want %s: its old tree had no answer to give it", got, settled)
	}
	if got := depthOf(t, pool, child); got != settled {
		t.Fatalf("the handed-over child was drawn %s for the %s experiment, want %s", got, depthExperiment, settled)
	}

	// And the pass that opens the child's own tree for the first time draws nothing for it: the
	// row is created there, which is the one moment a draw is made, and this root is one that left
	// another tree.
	tick(t, pool, admission, "promote-the-handed-over-child")

	var epoch int64
	if err := pool.QueryRow(ctx, `select epoch from tree_lifecycles where project = 'legion' and tree = $1`, child).Scan(&epoch); err != nil {
		t.Fatalf("read the lifecycle of the child's own tree: %v", err)
	}
	if got := memoryOf(t, pool, child); got != settled {
		t.Fatalf("after its own tree opened at epoch %d the child was drawn %s, want %s", epoch, got, settled)
	}
	if got := memoryOf(t, pool, root); got != noDraw {
		t.Fatalf("after the hand-over the old root was drawn %s, want %s", got, noDraw)
	}
}

// A root that left a tree nothing ever opened is drawn at its own first open, like any other new
// tree: the answers it takes with it are its old tree's, and a tree with no tree_lifecycles row
// was never opened, so its work never ran under an arm at all. Here the old root waits for a slot
// this admission never gives it, a child is recorded under it, and the person takes the root out
// of todo, which brings the child back as a root of its own (record.Issue.HandedOverFrom, whose
// tree the carry finds no lifecycle row for, so it settles nothing). The project is at `random`
// with the source pinned to the first arm, so the pass that re-admits that child opens its own
// tree — the one moment a lifecycle row is created — and draws it `memory` and `deep`, recorded as
// `random` at the hour of that pass.
func TestAChildHandedOverFromATreeNeverOpenedIsDrawnAtItsOwnFirstOpen(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 2)
	admission.UseExperiments(declaring(config.ExperimentRandom))
	admission.source = pinnedSource(0)
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
		Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	const root, child = "LEGION-1", "LEGION-2"
	// The old tree as the record holds it: a root recorded and waiting for a slot, and a child
	// recorded under it. No pass admits the root here, so nothing ever opens its tree.
	seedWaiting(t, pool, root, "A")
	if rows := lifecycleRows(t, pool, root); rows != 0 {
		t.Fatalf("the old tree has %d lifecycle rows, want none: nothing ever opened it", rows)
	}
	// The person takes the root out of todo; an hour later the child comes back as a root of its
	// own, and the hand-over records which tree it left.
	later := fixedNow.Add(time.Hour)

	handOverChild(t, pool, admission, engine, root, child, "B", later)

	if rows := lifecycleRows(t, pool, child); rows != 1 {
		t.Fatalf("the child's own tree has %d lifecycle rows, want the one its promotion created", rows)
	}
	// And the draw is made once: a later pass over the same tree creates no row and draws
	// nothing, under a source that fails the test when it is read.
	admission.source = refusingSource(t)

	tick(t, pool, admission, "another-pass-over-the-childs-own-tree")

	wantMemory := drawnAs("memory", string(config.ExperimentRandom), &later)
	wantDepth := drawnAs("deep", string(config.ExperimentRandom), &later)
	if got := memoryOf(t, pool, child); got != wantMemory {
		t.Fatalf("after its own tree opened the child was drawn %s, want %s", got, wantMemory)
	}
	if got := depthOf(t, pool, child); got != wantDepth {
		t.Fatalf("after its own tree opened the child was drawn %s for the %s experiment, want %s",
			got, depthExperiment, wantDepth)
	}
	if got := memoryOf(t, pool, root); got != noDraw {
		t.Fatalf("the root it left was drawn %s, want %s: its own tree was never opened", got, noDraw)
	}
}

// The hand-over decides a handed-over child's answers, so the order the two roots are promoted in
// changes nothing. The tree this child leaves is one nothing ever opened at the moment of the
// hand-over, which settles the child at no answer, so its own first open draws it like any other
// new tree. The old root is handed back to Legion too, and one pass with slots for both takes
// them in rank order, in one transaction: this test runs that pass both ways round — the slots
// they take say which went first — and reads the same answers off the child in each. A draw that
// read the old tree's lifecycle rows at the child's own first open would refuse the child its
// draw in the run where the old root goes first, and draw it in the other.
func TestAHandedOverChildsMemoryDoesNotTurnOnWhichRootIsPromotedFirst(t *testing.T) {
	const root, child = "LEGION-1", "LEGION-2"
	later := fixedNow.Add(time.Hour)
	for _, tc := range []struct {
		name, childRank string
		order           []string
	}{
		{name: "the old root promoted first", childRank: "C", order: []string{root, child}},
		{name: "the child promoted first", childRank: "A", order: []string{child, root}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			// No slot stands free while the hand-over and the hand-back run, so neither root is
			// promoted before the one pass below, and the source fails the test if anything draws.
			handing := quiet(t, 0)
			handing.UseExperiments(declaring(config.ExperimentRandom))
			handing.source = refusingSource(t)
			engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
				Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
			// The old tree as the record holds it: a root waiting for a slot it never gets, so
			// nothing ever opens its tree.
			seedWaiting(t, pool, root, "B")
			if rows := lifecycleRows(t, pool, root); rows != 0 {
				t.Fatalf("the old tree has %d lifecycle rows, want none: nothing ever opened it", rows)
			}

			handOverChild(t, pool, handing, engine, root, child, tc.childRank, later)
			if got := answers(t, pool, child); len(got) != 0 {
				t.Fatalf("the handed-over child holds %v, want none: the tree it left was never opened", got)
			}
			// The person hands the old root back to Legion, so both roots wait for a slot.
			apply(t, pool, handing, "root-handed-back", intake.DispatchIssue{Key: root, ResolvedRepo: "acme/widgets", Seq: 4,
				Type: "issue.updated", Status: "todo", Title: root, Rank: "B", HandedOver: handed}, engine)

			promoting := quiet(t, 2)
			promoting.UseExperiments(declaring(config.ExperimentRandom))
			promoting.source = pinnedSource(0)
			promoting.now = func() time.Time { return later }

			tick(t, pool, promoting, "promote-both-roots")

			if got := slotOrder(t, pool); !reflect.DeepEqual(got, tc.order) {
				t.Fatalf("the pass promoted %v, want %v: this run is meant to be the other order", got, tc.order)
			}
			wantMemory := drawnAs("memory", string(config.ExperimentRandom), &later)
			wantDepth := drawnAs("deep", string(config.ExperimentRandom), &later)
			if got := memoryOf(t, pool, child); got != wantMemory {
				t.Fatalf("after its own first open the child was drawn %s, want %s", got, wantMemory)
			}
			if got := depthOf(t, pool, child); got != wantDepth {
				t.Fatalf("after its own first open the child was drawn %s for the %s experiment, want %s",
					got, depthExperiment, wantDepth)
			}
			if got := memoryOf(t, pool, root); got != wantMemory {
				t.Fatalf("the old root was drawn %s, want %s: its own first open drew it", got, wantMemory)
			}
		})
	}
}

// handOverChild records child in root's tree, signs root off, and hands child to Legion as a tree
// of its own at at, the hour of the hand-over, which is where every test of a hand-over above
// starts. The child is recorded holding no answer of its own, as every child of a tree is, since
// a tree's answers are its root's; what the hand-over settles it at is what each caller reads.
func handOverChild(t *testing.T, pool *pgxpool.Pool, admission *Admission, engine intake.Handler,
	root, child, rank string, at time.Time) {
	t.Helper()
	parent := root
	putIssue(t, pool, record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Tree: root, Project: testProject, Title: "child", Parent: &parent, Phase: phase.Done, Generation: 1,
		Status: "done", Rank: rank, LastDispatchSeq: 2})
	if got := answers(t, pool, child); len(got) != 0 {
		t.Fatalf("a child of the tree holds %v, want none: a tree's answers are its root's", got)
	}
	linger(t, pool, root)
	admission.now = func() time.Time { return at }
	apply(t, pool, admission, "child-handed-over", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: 3,
		Type: "issue.updated", Status: "todo", Title: "child", Parent: root, Rank: rank, HandedOver: handed}, engine)
	handedOver := issue(t, pool, child)
	if handedOver.Tree != child {
		t.Fatalf("the handed-over child = %+v, want a tree of its own left by %s", handedOver, root)
	}
}

// slotOrder is the issues holding admission slots, in the order a pass took them: each takes the
// lowest free index (nextSlotIndex), so index order is the order they were promoted in.
func slotOrder(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select issue from slots order by index`)
	if err != nil {
		t.Fatalf("list the admission slots: %v", err)
	}
	defer rows.Close()
	var issues []string
	for rows.Next() {
		var issue string
		if err := rows.Scan(&issue); err != nil {
			t.Fatalf("read an admission slot: %v", err)
		}
		issues = append(issues, issue)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list the admission slots: %v", err)
	}
	return issues
}

// lifecycleRows is how many tree_lifecycles rows the store holds for this project's tree, the fact
// that says whether that tree was ever opened and the fact the hand-over reads
// (record.Store.CarryAssignments, by the project and the tree).
func lifecycleRows(t *testing.T, pool *pgxpool.Pool, tree string) int {
	t.Helper()
	var rows int
	if err := pool.QueryRow(context.Background(), `select count(*) from tree_lifecycles where project = 'legion' and tree = $1`, tree).Scan(&rows); err != nil {
		t.Fatalf("count the lifecycle rows of %s: %v", tree, err)
	}
	return rows
}

// linger signs key off and leaves it lingering with its slot given back, the state a root is in
// when a person hands it to Legion again or its children are handed over as trees of their own.
func linger(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	ctx := context.Background()
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		stored, err := records.Issue(ctx, tx, key)
		if err != nil || stored == nil {
			t.Fatalf("read %s: %v", key, err)
		}
		until := fixedNow.Add(time.Hour)
		stored.Phase, stored.Status, stored.LingerUntil = phase.Done, "done", &until
		if err := records.PutIssue(ctx, tx, *stored); err != nil {
			t.Fatalf("sign %s off: %v", key, err)
		}
		if err := records.ReleaseSlot(ctx, tx, key); err != nil {
			t.Fatalf("release the slot of %s: %v", key, err)
		}
	})
}
