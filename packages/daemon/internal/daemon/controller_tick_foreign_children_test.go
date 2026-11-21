package daemon

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testlog"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// The parent these tests start from, an issue of this daemon's project in accepting, and the
// child of another project a person files under it. The child sends this daemon no event: an
// event whose subject is another project's issue is dropped undecoded (intake.decodeDispatchFact),
// and the project's own listing names no issue of another project.
const (
	acceptingParent    = "LEGION-1"
	otherProjectChild  = "OTHR-5"
	otherProject       = "OTHR"
	otherProjectParent = "OTHR-6"
)

// childrenListing is tickDispatch with Dispatch's list of an issue's children in every project
// (dispatch.Client.ListChildren): it answers each parent's children from its own table, and fails
// every such list while err is set.
type childrenListing struct {
	*tickDispatch
	mu       sync.Mutex
	children map[string][]dispatch.IssueSummary
	err      error
}

func (d *childrenListing) ListChildren(_ context.Context, parent string) ([]dispatch.IssueSummary, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		return nil, d.err
	}
	return d.children[parent], nil
}

// serveChild is what Dispatch shows of the child of the other project from now on: its single read
// and the list of its parent's children.
func (d *childrenListing) serveChild(status, parent string, seq int64) {
	d.tickDispatch.serve(dispatch.Issue{Key: otherProjectChild, Project: otherProject, Status: status, Parent: &parent, LastSeq: seq})
	d.mu.Lock()
	defer d.mu.Unlock()
	d.children = map[string][]dispatch.IssueSummary{
		parent: {{Key: otherProjectChild, Status: status, Parent: &parent, LastSeq: seq}},
	}
}

// failLists makes every list of an issue's children fail from now on.
func (d *childrenListing) failLists() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.err = errors.New("Dispatch is restarting")
}

// The controller tick reads the children of each issue whose phases have started in every
// project, so a child of another project, which sends this daemon no event, holds its parent as a
// child of its own project does (record.ChildOpen): the tick writes its row of the copy with the
// project and the parent Dispatch shows, and the same tick's admission pass moves the parent into
// awaiting_children. The child's close, its move under another parent and its move to icebox each
// reach the daemon at the next tick, which takes the parent back to testing (the start a held
// accepting returns to). A failed list of the children ends nothing: the row and the wait stand,
// and the tick logs one warning.
func TestControllerTickReadsChildrenOfAnotherProject(t *testing.T) {
	ctx := context.Background()
	// opened is a runtime of project LEGION whose one tick has read the child, open under the
	// parent, and moved the parent into awaiting_children.
	opened := func(t *testing.T) (*store.Pool, *childrenListing, *workflowRuntime) {
		t.Helper()
		pool := isolatedOutboxPool(t)
		client := &childrenListing{tickDispatch: &tickDispatch{}}
		client.serveChild("in_progress", acceptingParent, 1)
		w := tickRuntimeIn(t, pool, client, "LEGION")
		seedAcceptingParent(t, pool)

		w.applyControllerTick(ctx, time.Now())

		if row, held := foreignChildRow(t, pool); !held || row.Project != otherProject || row.Parent != acceptingParent || row.Status != "in_progress" || row.Seq != 1 {
			t.Fatalf("the copy holds %s as %+v (held %v), want the tick's reading: project %s, in_progress under %s at 1", otherProjectChild, row, held, otherProject, acceptingParent)
		}
		if got := recordOf(t, pool, record.NewStore(), acceptingParent); got.Phase != phase.AwaitingChildren {
			t.Fatalf("%s after the tick = %s, want awaiting_children: %s, its child of another project, is open", acceptingParent, got.Phase, otherProjectChild)
		}
		return pool, client, w
	}

	for _, tc := range []struct {
		name          string
		status, under string
	}{
		{name: "the child is done", status: "done", under: acceptingParent},
		{name: "the child moves under another parent", status: "in_progress", under: otherProjectParent},
		{name: "the child moves to icebox", status: "icebox", under: acceptingParent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, client, w := opened(t)
			client.serveChild(tc.status, tc.under, 2)

			w.applyControllerTick(ctx, time.Now().Add(time.Second))

			if row, _ := foreignChildRow(t, pool); row.Status != tc.status || row.Parent != tc.under || row.Seq != 2 {
				t.Fatalf("the copy holds %s as %+v, want %s under %s at 2", otherProjectChild, row, tc.status, tc.under)
			}
			if got := recordOf(t, pool, record.NewStore(), acceptingParent); got.Phase != phase.Testing || got.ChildrenWait != nil {
				t.Fatalf("%s after the next tick = phase %s, held start %+v; want testing, where a held accepting returns, with nothing held", acceptingParent, got.Phase, got.ChildrenWait)
			}
		})
	}

	t.Run("a failed list of the children", func(t *testing.T) {
		pool, client, w := opened(t)
		var logs testlog.Buffer
		w.log = slog.New(slog.NewTextHandler(&logs, nil))
		client.failLists()

		w.applyControllerTick(ctx, time.Now().Add(time.Second))

		if row, held := foreignChildRow(t, pool); !held || row.Status != "in_progress" || row.Parent != acceptingParent || row.Seq != 1 {
			t.Fatalf("the copy holds %s as %+v (held %v) after a failed list, want the row as it stood: in_progress under %s at 1", otherProjectChild, row, held, acceptingParent)
		}
		if got := recordOf(t, pool, record.NewStore(), acceptingParent); got.Phase != phase.AwaitingChildren {
			t.Fatalf("%s after a failed list = %s, want awaiting_children still", acceptingParent, got.Phase)
		}
		if warnings := strings.Count(logs.String(), "level=WARN"); warnings != 1 {
			t.Fatalf("the tick logged %d warnings, want one for the failed list:\n%s", warnings, logs.String())
		}
	})
}

// seedAcceptingParent records acceptingParent as a root of project LEGION in accepting, its
// reviewed head waiting for its architect's sign-off: its record, its mirror row and its tree
// lifecycle open under the workflow. It holds no slot, as an issue past its review holds none
// (record.KeepsSlot), and no child of it is in the copy.
func seedAcceptingParent(t *testing.T, pool *store.Pool) {
	t.Helper()
	seedTickIssue(t, pool, record.Issue{
		Key: acceptingParent, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Title: acceptingParent, Tree: acceptingParent, Phase: phase.Accepting, Generation: 1, Status: "needs_review",
		DispatchStatus: "needs_review", Rank: "A", HandedOver: true, LastDispatchSeq: 1,
	})
	ctx := context.Background()
	token, err := claim.ProjectToken("LEGION")
	if err != nil {
		t.Fatalf("project token: %v", err)
	}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		records := record.NewStore()
		if err := records.PutDispatchIssue(ctx, tx, record.DispatchReading{Key: acceptingParent, Project: "LEGION", Seq: 1,
			Status: record.Carry("needs_review"), Parent: record.Carry(""), Blockers: record.Carry[[]string](nil)}); err != nil {
			return err
		}
		_, err := records.OpenTreeLifecycle(ctx, tx, token, acceptingParent, treelifecycle.AuthorityWorkflow)
		return err
	}); err != nil {
		t.Fatalf("seed the mirror row and the tree of %s: %v", acceptingParent, err)
	}
}

// foreignChildRow is the copy's row of the child of the other project, and whether it holds one.
func foreignChildRow(t *testing.T, pool *store.Pool) (record.DispatchIssueRow, bool) {
	t.Helper()
	var rows []record.DispatchIssueRow
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		var err error
		rows, err = record.NewStore().DispatchIssuesOf(context.Background(), tx, []string{otherProjectChild})
		return err
	}); err != nil {
		t.Fatalf("read the copy's row of %s: %v", otherProjectChild, err)
	}
	for _, row := range rows {
		if row.Key == otherProjectChild {
			return row, true
		}
	}
	return record.DispatchIssueRow{}, false
}
