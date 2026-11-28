package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testpg"
)

// An issue's `tree` in the state is its gate root (R22 of dispatch://INFRA-3320,
// record.Chains.StateTree), read from the Dispatch chain the copy holds and from no stored tree:
// the nearest issue at or above it in Legion's work that holds its own gate. So the children and
// grandchildren of a root read the root, and an issue that ran as a root and now stands under a
// parent in Legion's work, which holds its own gate, reads itself, as its own children read it. A
// finished issue's is the nearest issue above it that the daemon records and that holds its own
// gate, else the issue itself. Every record stores another tree (the wrong one), which nothing
// serves.
func TestTheTreeOfAnIssueIsItsGateRoot(t *testing.T) {
	ctx := context.Background()
	st := testpg.CloneStore(t, "legion_api_test")
	records := record.NewStore()
	const (
		root, child, grandchild    = "LEGION-400", "LEGION-401", "LEGION-402"
		member, underMember        = "LEGION-403", "LEGION-404"
		finishedChild, finishedTop = "LEGION-405", "LEGION-406"
		unrecordedParent, finished = "LEGION-407", "LEGION-408"
	)
	rows := []struct {
		key, parent, status string
		at                  phase.Phase
		holdsGate           bool
		want                string
	}{
		{root, "", "in_progress", phase.Implementing, true, root},
		{child, root, "in_progress", phase.Planning, false, root},
		{grandchild, child, "todo", phase.Admitted, false, root},
		{member, root, "in_progress", phase.Implementing, true, member},
		{underMember, member, "in_progress", phase.Planning, false, member},
		{finishedChild, child, "done", phase.Done, false, root},
		{finishedTop, unrecordedParent, "done", phase.Done, false, finishedTop},
		{finished, member, "done", phase.Done, false, member},
	}
	if err := store.Transact(ctx, st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		for _, row := range rows {
			issue := record.Issue{Key: row.key, Tree: "LEGION-999", Project: treeRunsProject, Title: row.key, Phase: row.at, Generation: 1,
				Status: row.status, DispatchStatus: row.status, Rank: row.key, LastDispatchSeq: 1, HoldsGate: row.holdsGate,
				Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
			if row.parent != "" {
				parent := row.parent
				issue.Parent = &parent
			}
			if err := records.PutIssue(ctx, tx, issue); err != nil {
				return err
			}
			if err := records.PutDispatchIssue(ctx, tx, record.DispatchReading{Key: row.key, Project: treeRunsProject, Seq: 1,
				Status: record.Carry(row.status), Parent: record.Carry(row.parent), Labels: record.Carry([]string{})}); err != nil {
				return err
			}
		}
		return records.PutDispatchIssue(ctx, tx, record.DispatchReading{Key: unrecordedParent, Project: treeRunsProject, Seq: 1,
			Status: record.Carry("backlog"), Parent: record.Carry(""), Labels: record.Carry([]string{})})
	}); err != nil {
		t.Fatalf("record the issues: %v", err)
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
			Tree string `json:"tree"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode the state %s: %v", recorder.Body, err)
	}
	for _, row := range rows {
		if got := body.Issues[row.key].Tree; got != row.want {
			t.Errorf("%s is served tree %q, want %s", row.key, got, row.want)
		}
	}
}
