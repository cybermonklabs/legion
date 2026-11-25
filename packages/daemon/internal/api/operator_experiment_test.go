package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// The operator's spawn of a root draws the tree-unit experiments the project declares only for a
// root the daemon holds an issue record of — a root the workflow recorded, which an operator
// spawned a session on — in the transaction that creates the tree's lifecycle row, under each
// experiment's own mode, from the source the deployment hands in and at the instant its clock
// reads (Options.Now). It reads the source once and records the tree's experiments and no other:
// over a working source the root holds its one answer, and over a failing one the spawn is
// refused whole, 500 with no answer written. A root the daemon holds no record of — the ordinary
// operator case, a tree the operator opens for work Dispatch never filed — is drawn for nothing:
// its spawn reads no random source and writes no answer, so a source that fails refuses nothing;
// the tree's lifecycle is opened as any other tree's is, and no issue record is invented.
func TestTheOperatorsSpawnDrawsATreeOnlyWhenTheDaemonHoldsItsRootsRecord(t *testing.T) {
	drawnAt := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	refused := errors.New("the random source is closed")
	// The two experiments the project declares: the `memory` block's own, at random, and an
	// issue-unit experiment the spawn must leave alone, since a tree's open draws the tree's
	// experiments and nothing else.
	declared := append(config.Project{Memory: config.Memory{Mode: config.ExperimentRandom}}.ExperimentsDeclared("projects."+testProject),
		config.Experiment{Name: "plan-length", Mode: config.ExperimentOn, Unit: config.ExperimentIssue, OnVariant: "short",
			Variants: []config.Variant{{Name: "short", Weight: 1}, {Name: "long", Weight: 1}}})
	for _, tc := range []struct {
		name     string
		recorded bool
		fails    bool
		code     int
		reads    int
		drawn    []string
	}{
		{name: "a root with no issue record, over a working source", code: http.StatusCreated},
		{name: "a root with no issue record, over a failing source", fails: true, code: http.StatusCreated},
		{name: "a recorded root, over a working source", recorded: true, code: http.StatusCreated, reads: 1,
			drawn: []string{`memory/tree: variant "memory" drawn as "random" at ` + drawnAt.Format(time.RFC3339Nano)}},
		{name: "a recorded root, over a failing source", recorded: true, fails: true, code: http.StatusInternalServerError, reads: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			reads := 0
			h.handler = NewServer("127.0.0.1", 8437, Options{
				Steps:      config.DesignGateRootIssues.Steps(),
				Supervisor: h.supervisor, BootTokens: h.tokens, Project: testProject, OperatorToken: testOperatorToken,
				Controller: h.store, Pool: h.store.Pool(), Record: record.NewStore(), Releaser: h.runtime, Trees: h.store,
				Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
				Experiments: declared,
				// The first arm of the two the `memory` block declares, each of weight 1, or the
				// source's refusal.
				ExperimentSource: func(uint64) (uint64, error) {
					reads++
					if tc.fails {
						return 0, refused
					}
					return 0, nil
				},
				Now: func() time.Time { return drawnAt },
			}).Handler
			spawned := spawnBody()
			if tc.recorded {
				h.recordIssue(record.Issue{
					Key: spawned.Tree, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
					Tree: spawned.Tree, Project: testDispatchProject, Title: spawned.Tree, Phase: phase.Admitted,
					Generation: 1, Status: "todo", Rank: "A", HandedOver: true,
				})
			}

			if recorder := h.operator(http.MethodPost, "/legion/v1/operator/claims", spawned); recorder.Code != tc.code {
				t.Fatalf("spawn = %d %s, want %d", recorder.Code, recorder.Body, tc.code)
			}
			if reads != tc.reads {
				t.Fatalf("the spawn read the random source %d times, want %d", reads, tc.reads)
			}
			if held := readAssignments(t, h, spawned.Tree); !slices.Equal(held, tc.drawn) {
				t.Fatalf("the spawned root holds %v, want %v: the tree's experiments and no other", held, tc.drawn)
			}
			if tc.recorded {
				return
			}
			if stored := readIssue(t, h, spawned.Tree); stored != nil {
				t.Fatalf("the spawn of an unrecorded tree wrote the record %+v, want none invented", *stored)
			}
			if started, _ := h.treeCleanup(spawned.Tree); started {
				t.Fatalf("the lifecycle of %s reads a reserved cleanup, want the tree opened", spawned.Tree)
			}
		})
	}
}

// readAssignments is every experiment answer the store holds for key, in experiment order, one
// line each: the experiment and the unit it covers, then the arm, the mode it was drawn under and
// the instant.
func readAssignments(t *testing.T, h *harness, key string) []string {
	t.Helper()
	var held []record.Assignment
	if err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
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
	if err := store.Transact(context.Background(), h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		stored, err = record.NewStore().Issue(context.Background(), tx, key)
		return err
	}); err != nil {
		t.Fatalf("read the record of %s: %v", key, err)
	}
	return stored
}
