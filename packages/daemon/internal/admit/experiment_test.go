package admit

import (
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// planLength is an issue-unit experiment: one the daemon draws for each issue it records, against
// that issue's own key, rather than once for the issue's whole tree. Its mode is `on`, so every
// issue it reaches is given the arm on_variant names and no source is read.
var planLength = config.Experiment{
	Name: "plan-length", Mode: config.ExperimentOn, Unit: config.ExperimentIssue, OnVariant: "short",
	Variants: []config.Variant{{Name: "short", Weight: 1}, {Name: "long", Weight: 1}},
}

// A root is an issue like any other, so the issue-unit experiments the project declares are drawn
// for it where the daemon first records it (putNewRoot), against its own key — not at the opening
// of its tree, which is where the tree-unit experiments are drawn. Both answers stand on the root
// here, each naming the unit its draw covered, because a root is both an issue and the key its
// tree is read by. A root the daemon already holds a record of is never drawn again: the
// observations that follow write what Dispatch says and no answer at all.
func TestARootIsDrawnForTheIssueExperimentsWhenItIsFirstRecorded(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)
	admission.UseExperiments(append(declaring(config.MemoryOn), planLength))
	admission.source = refusingSource(t)
	engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
		Steps: config.DesignGateRootIssues.Steps(), Linger: time.Hour, Clock: func() time.Time { return fixedNow }}, nil)
	const root = "LEGION-1"

	// A root the daemon holds no record of, handed to Legion in todo: admission records it, and
	// that record is where its issue-unit experiments are drawn.
	apply(t, pool, admission, "root-recorded", intake.DispatchIssue{Key: root, ResolvedRepo: "acme/widgets", Seq: 1,
		Type: "issue.updated", Status: "todo", Title: root, Rank: "A", HandedOver: handed}, engine)

	wantIssue := drawnAs("short", string(config.ExperimentOn), &fixedNow)
	if got := answerFor(t, pool, root, planLength.Name); got != wantIssue {
		t.Fatalf("the recorded root was drawn %s for the %s experiment, want %s", got, planLength.Name, wantIssue)
	}
	// Its tree's own experiments were drawn by the same pass, which promoted it: the one answer
	// is keyed by the root as the tree's key, the other by the root as an issue.
	wantTree := drawnAs("memory", string(config.MemoryOn), &fixedNow)
	if got := memoryOf(t, pool, root); got != wantTree {
		t.Fatalf("the promoted root was drawn %s for the memory experiment, want %s", got, wantTree)
	}

	// A later observation of the recorded root — a rename — records the observation and draws
	// nothing, not even for an experiment declared since.
	before := answers(t, pool, root)
	admission.UseExperiments(append(declaring(config.MemoryOn), planLength, config.Experiment{
		Name: "review-order", Mode: config.ExperimentOn, Unit: config.ExperimentIssue, OnVariant: "oldest",
		Variants: []config.Variant{{Name: "oldest", Weight: 1}, {Name: "newest", Weight: 1}}}))

	apply(t, pool, admission, "root-renamed", intake.DispatchIssue{Key: root, ResolvedRepo: "acme/widgets", Seq: 2,
		Type: "issue.updated", Status: "in_progress", Title: "a renamed root", Rank: "A", HandedOver: handed}, engine)

	if renamed := issue(t, pool, root); renamed.Title != "a renamed root" {
		t.Fatalf("the observed root is titled %q, want the observation recorded", renamed.Title)
	}
	if after := answers(t, pool, root); !reflect.DeepEqual(after, before) {
		t.Fatalf("after an observation the root holds %v, want %v: an experiment declared after an issue was recorded never reaches it", after, before)
	}
}
