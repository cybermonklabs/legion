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
	state, err := projection.Project(ctx, tx, record.NewStore(), treeRunsProject, config.DesignGateRootIssues.Steps(),
		nil, nil, nil, treeRunsNow)
	return state, nil, err
}

// The served `treeRuns` is the daemon's own answer to whether a root's tree runs (record.TreeRuns,
// dispatch://INFRA-3320, R4), on every root it records, read through the route a session reads
// its record from, over the state the projection builds from a real store. Four roots, each a
// case no other field of the record decides alone: a root whose tree runs, true; a root whose
// tree finished and lingers, false, though its lifecycle still stands open; a root a re-admission
// left at todo waiting in the line for its tree to be opened again, false, though the lifecycle
// of the run that ended stands open too; and a root a person answered, which waits in the line
// for its architect's resume, true, since its tree runs while it waits there. A child carries no
// such key.
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
			Rank: rank, HandedOver: true, LastDispatchSeq: 1, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
	}
	const running, lingering, readmitted, answered, child = "LEGION-300", "LEGION-301", "LEGION-302", "LEGION-303", "LEGION-304"
	lingerUntil := treeRunsNow.Add(time.Hour)
	answeredAt := treeRunsNow.Add(-10 * time.Minute)
	parent := running

	issues := []record.Issue{
		root(running, phase.Planning, "in_progress", 1, "A"),
		root(lingering, phase.Done, "done", 1, "B"),
		root(readmitted, phase.Admitted, "todo", 2, "C"),
		root(answered, phase.AwaitingPerson, "in_progress", 1, "D"),
		{Key: child, Tree: running, Parent: &parent, Project: treeRunsProject, Title: child, Phase: phase.Admitted, Generation: 1,
			Status: "todo", Rank: "E", LastDispatchSeq: 1, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"},
	}
	issues[1].LingerUntil = &lingerUntil
	issues[3].Awaiting = &record.Awaiting{Ask: "ask-303", Since: record.EventMark{Seq: 5, At: treeRunsNow.Add(-time.Hour)},
		AnsweredAt: &answeredAt, AnsweredBy: record.AnsweredByAsk}
	if err := st.Tx(ctx, func(tx pgx.Tx) error {
		for _, issue := range issues {
			if err := records.PutIssue(ctx, tx, issue); err != nil {
				return err
			}
		}
		// Every one of the four trees once opened, and none has been cleaned up: the lingering
		// tree and the re-admitted one stand open from the run that ended, which is why the
		// lifecycle alone would call both running.
		for _, tree := range []string{running, lingering, readmitted, answered} {
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
		return nil
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
	for _, key := range []string{readmitted, answered} {
		if body.Issues[key].WaitingLine == nil {
			t.Fatalf("%s stands in no place of the waiting line in %s; this test proves nothing of a root that waits there", key, recorder.Body)
		}
	}
	for _, tc := range []struct {
		key  string
		runs bool
	}{
		{running, true},
		{lingering, false},
		{readmitted, false},
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
