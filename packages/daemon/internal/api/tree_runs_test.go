package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/projection"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testpg"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// treeRunsProject is the Dispatch project the roots below are recorded under, and treeRunsNow the
// instant the state is projected at.
const treeRunsProject = "LEGION"

var treeRunsNow = time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)

// projectedState is the daemon's own state source over a real store: the state the projection
// builds in the route's own snapshot, with no claim, as the daemon's source answers it.
type projectedState struct{}

func (projectedState) State(ctx context.Context, tx pgx.Tx) (api.State, []supervise.Claim, error) {
	state, err := projection.Project(ctx, projection.Inputs{Tx: tx, Store: record.NewStore(), Project: treeRunsProject,
		Steps: config.DesignGateRootIssues.Steps(), Now: treeRunsNow})
	return state, nil, err
}

func (projectedState) ObservedState(ctx context.Context, tx pgx.Tx) (api.State, []supervise.Claim, error) {
	observed, err := projection.Observe(ctx, projection.ObservedInputs{Tx: tx, Store: record.NewStore(), Project: treeRunsProject,
		Steps: config.DesignGateRootIssues.Steps(), Now: treeRunsNow})
	return observed, nil, err
}

// The served `treeRuns` is the daemon's own answer to whether a root's tree runs (record.TreeRuns,
// dispatch://INFRA-3320, R4), on every root it records, read through the route a session reads
// its record from, over the state the projection builds from a real store and the copy of
// Dispatch's issues, from which every chain is read (R15). Five roots, each a case no other field
// of the record decides alone: a root whose tree runs, true; a root whose run finished, false,
// though its lifecycle still stands open; a root a person moved to the backlog, which keeps its
// phase out of Legion's work (R20) with Dispatch showing backlog, false, though its lifecycle
// stands open too; a root a person moved to the backlog and back to todo, which stands in its phase
// again and waits in the line among the work that came back for the slot its stop freed, with the
// start it owes its implementer (R20), false, though its phase carries its run and its lifecycle
// stands open as well; and a root a person answered, which waits in the line for its architect's
// resume holding its slot, true, since its tree runs while it waits there. A child carries no such
// key.
func TestTreeRunsIsServedForEveryRootState(t *testing.T) {
	ctx := context.Background()
	st := testpg.CloneStore(t, "legion_api_test")
	records := record.NewStore()
	token, err := claim.ProjectToken(treeRunsProject)
	if err != nil {
		t.Fatalf("project token of %s: %v", treeRunsProject, err)
	}
	root := func(key string, at phase.Phase, status string, generation uint64, rank string) record.Issue {
		return record.Issue{Key: key, Tree: key, Project: treeRunsProject, Title: key, Phase: at, Generation: generation, Status: status,
			DispatchStatus: status, Rank: rank, HandedOver: true, LastDispatchSeq: 1, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
	}
	const running, finished, returned, answered, child, parked = "LEGION-300", "LEGION-301", "LEGION-302", "LEGION-303", "LEGION-304", "LEGION-305"
	answeredAt := treeRunsNow.Add(-10 * time.Minute)
	parent := running

	issues := []record.Issue{
		root(running, phase.Planning, "in_progress", 1, "A"),
		root(finished, phase.Done, "done", 1, "B"),
		root(returned, phase.Implementing, "in_progress", 1, "C"),
		root(answered, phase.AwaitingPerson, "in_progress", 1, "D"),
		{Key: child, Tree: running, Parent: &parent, Project: treeRunsProject, Title: child, Phase: phase.Admitted, Generation: 1,
			Status: "todo", DispatchStatus: "todo", Rank: "E", LastDispatchSeq: 1, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"},
		root(parked, phase.Planning, "backlog", 1, "F"),
	}
	// The returned root's return wrote in_progress, its implementing's status, which Dispatch, still
	// showing the person's todo, has not echoed yet.
	issues[2].DispatchStatus = "todo"
	issues[3].Awaiting = &record.Awaiting{Ask: "ask-303", Since: record.EventMark{Seq: 5, At: treeRunsNow.Add(-time.Hour)},
		AnsweredAt: &answeredAt, AnsweredBy: record.AnsweredByAsk}
	if err := store.Transact(ctx, st.Pool(), func(_ context.Context, tx pgx.Tx) error {
		for _, issue := range issues {
			if err := records.PutIssue(ctx, tx, issue); err != nil {
				return err
			}
			parent := ""
			if issue.Parent != nil {
				parent = *issue.Parent
			}
			if err := records.PutDispatchIssue(ctx, tx, record.DispatchReading{Key: issue.Key, Project: treeRunsProject, Seq: 1,
				Status: record.Carry(issue.DispatchStatus), Parent: record.Carry(parent), Labels: record.Carry([]string{})}); err != nil {
				return err
			}
		}
		// Every one of the five trees once opened, and none has been cleaned up: the finished
		// tree, the parked one and the returned one stand open from their runs, which is why the
		// lifecycle alone would call each of them running.
		for _, tree := range []string{running, finished, returned, answered, parked} {
			if _, err := records.OpenTreeLifecycle(ctx, tx, token, tree, treelifecycle.AuthorityWorkflow); err != nil {
				return err
			}
		}
		// The running root and the answered one hold the slots they took when the daemon started
		// them; the answered root keeps its own through the wait (record.KeepsSlot).
		for i, key := range []string{running, answered} {
			if err := records.PutSlot(ctx, tx, record.Slot{Issue: key, Index: i, AdmittedAt: treeRunsNow.Add(-2 * time.Hour)}); err != nil {
				return err
			}
		}
		// The start the returned root's return owes its implementer, held until a slot comes: what
		// puts it in the line among the work that came back (record.Waiting, LineReturning).
		start, err := record.NewOutboxRow(returned, record.SuperviseRequest{Op: "start", Tree: returned, Role: claim.RoleImplementer,
			Generation: 1, Phase: phase.Implementing, NeedsSlot: true, Task: "Continue LEGION-302. Issue: LEGION-302. Phase: implementing."}, treeRunsNow)
		if err != nil {
			return err
		}
		return records.EnqueueStart(ctx, tx, start)
	}); err != nil {
		t.Fatalf("record the roots: %v", err)
	}

	handler := api.NewServer("127.0.0.1", 8437, api.Options{
		Steps: config.DesignGateRootIssues.Steps(), State: projectedState{}, StateTransactions: st.Pool(),
	}).Handler
	request := httptest.NewRequest(http.MethodGet, "/legion/v1/state", nil)
	request.RemoteAddr = "192.0.2.1:41000"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("state = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	var body struct {
		Issues map[string]struct {
			TreeRuns    *bool           `json:"treeRuns"`
			WaitingLine json.RawMessage `json:"waitingLine"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode the state %s: %v", recorder.Body, err)
	}

	// The two roots the line holds are the line's: a test of a root waiting in the line that
	// found it in no line would prove nothing of that case.
	for _, key := range []string{returned, answered} {
		if body.Issues[key].WaitingLine == nil {
			t.Fatalf("%s stands in no place of the waiting line in %s; this test proves nothing of a root that waits there", key, recorder.Body)
		}
	}
	for _, tc := range []struct {
		key  string
		runs bool
	}{
		{running, true},
		{finished, false},
		{parked, false},
		{returned, false},
		{answered, true},
	} {
		served := body.Issues[tc.key].TreeRuns
		if served == nil || *served != tc.runs {
			t.Errorf("%s is served treeRuns %v, want %t", tc.key, served, tc.runs)
		}
	}
	if served := body.Issues[child].TreeRuns; served != nil {
		t.Errorf("%s, a child of %s, is served treeRuns %t, want no such key on an issue that roots no tree", child, running, *served)
	}
}
