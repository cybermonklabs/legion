package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The operator's spawn of a root draws the tree-unit experiments the project declares, in the
// transaction that creates the tree's lifecycle row, under each experiment's own mode, from the
// source the deployment hands in and at the instant its clock reads (Options.Now). The answers
// reach the tree's root record where the daemon has one — a root the workflow recorded, which an
// operator spawned a session on — and nothing at all is written where it has none, the ordinary
// operator case: a tree the operator opens for work Dispatch never filed is not refused, no row
// is written for it and no issue record is invented.
func TestTheOperatorsSpawnDrawsTheMemoryGroupOfTheTreeItOpens(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	records := record.NewStore()
	drawnAt := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	// The two experiments the project declares: the `memory` block's own, at random, and an
	// issue-unit experiment the spawn must leave alone, since a tree's open draws the tree's
	// experiments and nothing else.
	declared := append(config.Project{Memory: config.Memory{Mode: config.MemoryRandom}}.ExperimentsDeclared(),
		config.Experiment{Name: "plan-length", Mode: config.ExperimentOn, Unit: config.ExperimentIssue, OnVariant: "short",
			Variants: []config.Variant{{Name: "short", Weight: 1}, {Name: "long", Weight: 1}}})
	h.handler = NewServer("127.0.0.1", 8437, Options{
		Steps:      config.DesignGateRootIssues.Steps(),
		Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken,
		Controller: h.store, Pool: h.store.Pool(), Record: records, Releaser: h.runtime, Trees: h.store,
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		Experiments: declared,
		// The first arm of the two the `memory` block declares, each of weight 1.
		ExperimentSource: func(uint64) (uint64, error) { return 0, nil },
		Now:              func() time.Time { return drawnAt },
	}).Handler
	recorded := spawnBody()
	if err := h.store.Tx(ctx, func(tx pgx.Tx) error {
		return records.PutIssue(ctx, tx, record.Issue{
			Key: recorded.Tree, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Tree: recorded.Tree, Project: testDispatchProject, Title: recorded.Tree, Phase: phase.Admitted,
			Generation: 1, Status: "todo", Rank: "A", HandedOver: true,
		})
	}); err != nil {
		t.Fatalf("seed the root's record: %v", err)
	}

	if code := h.operator(http.MethodPost, "/legion/v1/operator/claims", recorded).Code; code != http.StatusCreated {
		t.Fatalf("spawn of a recorded root = %d, want 201", code)
	}

	drawn := readAssignments(t, h, recorded.Tree)
	want := []string{`memory/tree: variant "memory" drawn as "random" at ` + drawnAt.Format(time.RFC3339Nano)}
	if len(drawn) != len(want) || drawn[0] != want[0] {
		t.Fatalf("the spawned root holds %v, want %v: the tree's experiments and no other", drawn, want)
	}

	// A tree the daemon has no record of: the spawn opens it, draws nothing, and answers as it
	// does for any other tree.
	unrecorded := SpawnRequest{Tree: "LEGION-404", Issue: "LEGION-404", Role: claim.RoleArchitect, Prompt: "Reply ready and wait."}
	if code := h.operator(http.MethodPost, "/legion/v1/operator/claims", unrecorded).Code; code != http.StatusCreated {
		t.Fatalf("spawn of a tree with no record = %d, want 201", code)
	}
	if held := readAssignments(t, h, unrecorded.Tree); len(held) != 0 {
		t.Fatalf("the spawn of an unrecorded tree wrote %v, want nothing written", held)
	}
	if stored := readIssue(t, h, unrecorded.Tree); stored != nil {
		t.Fatalf("the spawn of an unrecorded tree wrote the record %+v, want none invented", *stored)
	}
	if started, _ := h.treeCleanup(unrecorded.Tree); started {
		t.Fatalf("the lifecycle of %s reads a reserved cleanup, want the tree opened", unrecorded.Tree)
	}
}

// readAssignments is every experiment answer the store holds for key, in experiment order, one
// line each: the experiment and the unit it covers, then the arm, the mode it was drawn under and
// the instant.
func readAssignments(t *testing.T, h *harness, key string) []string {
	t.Helper()
	var held []record.Assignment
	if err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		var err error
		held, err = record.NewStore().AssignmentsOf(context.Background(), tx, key)
		return err
	}); err != nil {
		t.Fatalf("read the experiment assignments of %s: %v", key, err)
	}
	read := make([]string, 0, len(held))
	for _, assignment := range held {
		at := "no draw"
		if assignment.DrawnAt != nil {
			at = assignment.DrawnAt.Format(time.RFC3339Nano)
		}
		read = append(read, assignment.Experiment+"/"+assignment.Unit+": "+
			`variant "`+assignment.Variant+`" drawn as "`+assignment.Mode+`" at `+at)
	}
	return read
}

// readIssue is the record of key as the store holds it now, nil when the daemon has none.
func readIssue(t *testing.T, h *harness, key string) *record.Issue {
	t.Helper()
	var stored *record.Issue
	if err := h.store.Tx(context.Background(), func(tx pgx.Tx) error {
		var err error
		stored, err = record.NewStore().Issue(context.Background(), tx, key)
		return err
	}); err != nil {
		t.Fatalf("read the record of %s: %v", key, err)
	}
	return stored
}
