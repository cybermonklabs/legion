package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// The operator's spawn records no experiment arm. An issue the workflow records is the workflow's
// to start, and the promotion that starts it draws its arms or takes the arms of the issue above
// it (R17 of dispatch://INFRA-3320), so a spawn on it opens no lifecycle and draws nothing: a root
// the workflow recorded and has not started, and a recorded issue of that root's tree, are each
// refused, since a claim of either binds the lifecycle the workflow opens. A tree the operator
// opens for work Dispatch never filed is opened under the operator's authority and not refused; no
// record backs it to hold an arm, so no row is written for it and no issue record is invented.
func TestTheOperatorsSpawnRecordsNoArm(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	records := record.NewStore()
	recorded := spawnBody()
	if err := store.Transact(ctx, h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		return records.PutIssue(ctx, tx, record.Issue{
			Key: recorded.Tree, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Tree: recorded.Tree, Project: testDispatchProject, Title: recorded.Tree, Phase: phase.Admitted,
			Generation: 1, Status: "todo", Rank: "A", HandedOver: true,
		})
	}); err != nil {
		t.Fatalf("seed the root's record: %v", err)
	}

	if code := h.operator(http.MethodPost, "/legion/v1/operator/claims", recorded).Code; code != http.StatusConflict {
		t.Fatalf("spawn of a recorded root the workflow has not started = %d, want 409", code)
	}
	if drawn := readAssignments(t, h, recorded.Tree); len(drawn) != 0 {
		t.Fatalf("the spawn of a recorded root drew %v, want nothing: its promotion draws it", drawn)
	}
	if authority := issueAuthority(t, h, recorded.Tree); authority != "" {
		t.Fatalf("the spawn of a recorded root opened its lifecycle under %s, want none", authority)
	}

	// A worker on a recorded issue of that tree opens nothing either.
	worker := spawnBody()
	worker.Issue, worker.Role = "LEGION-209", claim.RoleImplementer
	if err := store.Transact(ctx, h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		parent := recorded.Tree
		return records.PutIssue(ctx, tx, record.Issue{
			Key: worker.Issue, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Tree: recorded.Tree, Parent: &parent, Project: testDispatchProject, Title: worker.Issue, Phase: phase.Admitted,
			Generation: 1, Status: "todo", Rank: "B", HandedOver: true,
		})
	}); err != nil {
		t.Fatalf("seed the worker's issue: %v", err)
	}
	if code := h.operator(http.MethodPost, "/legion/v1/operator/claims", worker).Code; code != http.StatusConflict {
		t.Fatalf("spawn of a worker on a recorded issue the workflow has not started = %d, want 409", code)
	}
	if drawn := readAssignments(t, h, worker.Issue); len(drawn) != 0 || issueAuthority(t, h, worker.Issue) != "" {
		t.Fatalf("the spawn of a worker on a recorded issue drew %v and opened its lifecycle under %q, want neither", drawn, issueAuthority(t, h, worker.Issue))
	}

	// A tree the daemon has no record of: the spawn opens it under the operator's authority, draws
	// onto no record, and answers as it does for any other tree.
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
	if authority := issueAuthority(t, h, unrecorded.Issue); authority != string(treelifecycle.AuthorityOperator) {
		t.Fatalf("the lifecycle of %s stands under %q, want the operator's", unrecorded.Issue, authority)
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
