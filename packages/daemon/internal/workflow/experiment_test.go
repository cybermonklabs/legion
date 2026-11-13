package workflow

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
)

// planLength is an issue-unit experiment: one the daemon draws for each issue it records, so two
// issues of one tree may run under different arms. Its two arms carry equal weight, so a source
// answering 0 draws the first and one answering 1 the second.
var planLength = config.Experiment{
	Name: "plan-length", Mode: config.ExperimentRandom, Unit: config.ExperimentIssue, OnVariant: "short",
	Variants: []config.Variant{{Name: "short", Weight: 1}, {Name: "long", Weight: 1}},
}

// treeMemory is the tree-unit experiment the project's `memory` block declares, declared beside
// the issue-unit one so that each draw below is shown to take only the experiments of its own
// unit: a tree's answers are drawn when its tree is opened (admit's promoteHolds), never here.
var treeMemory = config.Project{Memory: config.Memory{Mode: config.ExperimentOn}}.ExperimentsDeclared("projects.LEGION")

// scriptedSource answers each of values in turn and fails the test once they run out, so a test
// says which arm each draw takes and a draw nobody expected is loud.
func scriptedSource(t *testing.T, values ...uint64) func(uint64) (uint64, error) {
	t.Helper()
	read := 0
	return func(bound uint64) (uint64, error) {
		if read >= len(values) {
			t.Errorf("the engine read the random source %d times, want %d (bound %d)", read+1, len(values), bound)
			return 0, nil
		}
		value := values[read]
		read++
		return value, nil
	}
}

// refusingSource fails the test when it is read: the source of a pass that must draw nothing.
func refusingSource(t *testing.T) func(uint64) (uint64, error) {
	return func(bound uint64) (uint64, error) {
		t.Errorf("the engine read the random source on a pass that draws nothing (bound %d)", bound)
		return 0, nil
	}
}

// answersOf is every answer the record holds for key, one line each in experiment order: the
// experiment, the unit its draw covered, the arm, the mode it was drawn under and the instant.
func answersOf(t *testing.T, pool *pgxpool.Pool, key string) []string {
	t.Helper()
	var held []record.Assignment
	seedRecord(t, pool, func(tx pgx.Tx) error {
		var err error
		held, err = record.NewStore().AssignmentsOf(t.Context(), tx, key)
		return err
	})
	read := make([]string, 0, len(held))
	for _, assignment := range held {
		at := "no draw"
		if assignment.DrawnAt != nil {
			at = assignment.DrawnAt.UTC().Format(time.RFC3339Nano)
		}
		read = append(read, fmt.Sprintf("%s/%s: variant %q drawn as %q at %s",
			assignment.Experiment, assignment.Unit, assignment.Variant, assignment.Mode, at))
	}
	return read
}

// experimentEngine is testEngine declaring experiments and drawing them from source.
func experimentEngine(t *testing.T, declared []config.Experiment, source func(uint64) (uint64, error)) *Engine {
	t.Helper()
	engine := testEngine(config.DesignGateRootIssues, nil)
	engine.cfg.Experiments = declared
	engine.cfg.ExperimentSource = source
	return engine
}

// An issue-unit experiment is drawn for each issue the daemon records, against that issue's own
// key: a tree of three children records three answers, one for each, and each is that issue's
// alone — here the first and the third child draw `short` and the second `long`, from one source
// read once per issue in the order the children were recorded. The tree-unit experiment declared
// beside it is no part of any of it: a tree's answer is drawn once for the whole tree when the
// tree is opened, so nothing here writes one.
func TestAnIssueExperimentIsDrawnForEachIssueOfATree(t *testing.T) {
	pool := migratedPool(t)
	seedHeldTree(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 3, ApprovedVersion: new(3)})
	engine := experimentEngine(t, append([]config.Experiment{planLength}, treeMemory...), scriptedSource(t, 0, 1, 0))
	drawnAt := engine.now()

	for i, child := range []string{"LEGION-209", "LEGION-210", "LEGION-211"} {
		applyChild(t, pool, engine, child, fmt.Sprintf("child-%d", i))
	}

	for _, tc := range []struct{ child, variant string }{
		{child: "LEGION-209", variant: "short"},
		{child: "LEGION-210", variant: "long"},
		{child: "LEGION-211", variant: "short"},
	} {
		want := []string{fmt.Sprintf("plan-length/issue: variant %q drawn as %q at %s",
			tc.variant, config.ExperimentRandom, drawnAt.Format(time.RFC3339Nano))}
		if got := answersOf(t, pool, tc.child); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s holds %v, want %v: each issue reads the arm drawn for it alone", tc.child, got, want)
		}
	}
	if got := answersOf(t, pool, "LEGION-208"); len(got) != 0 {
		t.Fatalf("the tree's root holds %v, want none: the engine draws no tree experiment and recorded no root", got)
	}
}

// An issue the daemon has already recorded is never drawn again, so an experiment declared after
// it was recorded never reaches it: the draw is made where the daemon first records an issue and
// nowhere else. The child here is first recorded by an engine handed no experiment at all, and
// that record goes through recordChild, which enters the child (enterChild) and then draws its
// issue-unit experiments (drawExperiments), none for that engine. The experiment is declared
// afterwards, and the person moves the recorded child back to todo: that fact finds the record,
// so it goes through Engine.ReenterChild instead, which enters the child through the same
// enterChild and draws nothing. The test depends on that difference, and holds the re-entry to it
// under a source that fails the test when it is read; the fresh child at the end, recorded
// through recordChild under the declaring engine, is drawn.
func TestAnIssueAlreadyRecordedIsNotDrawnForAnExperimentDeclaredAfterIt(t *testing.T) {
	pool := migratedPool(t)
	seedHeldTree(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 3, ApprovedVersion: new(3)})
	declaring := experimentEngine(t, nil, refusingSource(t))

	applyChild(t, pool, declaring, "LEGION-209", "child")

	if got := answersOf(t, pool, "LEGION-209"); len(got) != 0 {
		t.Fatalf("a child recorded by an engine handed no experiment holds %v, want none", got)
	}
	// The operator declares the experiment, and the person moves the recorded child back to todo.
	declared := experimentEngine(t, []config.Experiment{planLength}, refusingSource(t))
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: "LEGION-208", Project: "LEGION", Title: "a child", Parent: new("LEGION-208"), Phase: phase.Planning,
		Generation: 4, Status: "in_progress", Rank: "V", LastDispatchSeq: 2})
	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "child-reopened", intake.DispatchIssue{
		Key: "LEGION-209", ResolvedRepo: "sjawhar/legion", Seq: 3, Type: "issue.updated", Status: "todo",
		Title: "a child", Parent: "LEGION-208", Rank: "V"}, declared, admissionStub{}); err != nil {
		t.Fatalf("reopen the recorded child: %v", err)
	}

	if got := phaseOf(t, pool, "LEGION-209"); got != phase.Admitted {
		t.Fatalf("the reopened child = %s, want it re-entered at admitted", got)
	}
	if got := answersOf(t, pool, "LEGION-209"); len(got) != 0 {
		t.Fatalf("the reopened child holds %v, want none: an experiment declared after it was recorded never reaches it", got)
	}
	// A child the daemon records for the first time under the same engine is drawn, so what the
	// re-entry above leaves alone is the record's age and not a declaration the engine missed.
	drawing := experimentEngine(t, []config.Experiment{planLength}, scriptedSource(t, 1))

	applyChild(t, pool, drawing, "LEGION-210", "fresh-child")

	want := []string{fmt.Sprintf("plan-length/issue: variant %q drawn as %q at %s",
		"long", config.ExperimentRandom, drawing.now().Format(time.RFC3339Nano))}
	if got := answersOf(t, pool, "LEGION-210"); !reflect.DeepEqual(got, want) {
		t.Fatalf("a child recorded for the first time holds %v, want %v", got, want)
	}
}

// applyChild records key as an unrecorded todo child of the held tree, the one door a child
// enters its tree through (recordChildUnderLiveTree).
func applyChild(t *testing.T, pool *pgxpool.Pool, engine *Engine, key, eventID string) {
	t.Helper()
	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", eventID, intake.DispatchIssue{
		Key: key, ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.updated", Status: "todo",
		Title: "a child", Parent: "LEGION-208", Rank: "V"}, engine, admissionStub{}); err != nil {
		t.Fatalf("record the child %s: %v", key, err)
	}
}
