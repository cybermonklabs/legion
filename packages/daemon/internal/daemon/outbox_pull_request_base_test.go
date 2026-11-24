package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// pullBaseGitHub is GitHub's pull request #4 of acme/widgets as the row of a merge's start reads
// and writes it: its base, which a PATCH changes, and each request other than the read of #4.
type pullBaseGitHub struct {
	mu     sync.Mutex
	base   string
	writes []string
}

// serve answers the read and the PATCH of #4, records each other request, and answers false for a
// request it does not serve.
func (g *pullBaseGitHub) serve(w http.ResponseWriter, r *http.Request) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if r.URL.Path != "/repos/acme/widgets/pulls/4" {
		g.writes = append(g.writes, r.Method+" "+r.URL.Path)
		return false
	}
	switch r.Method {
	case http.MethodGet:
	case http.MethodPatch:
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return true
		}
		g.writes = append(g.writes, "PATCH base "+body["base"])
		g.base = body["base"]
	default:
		return false
	}
	_, _ = fmt.Fprintf(w, `{"number":4,"state":"open","base":{"ref":%q},"head":{"ref":"legion/LEGION-208","sha":"h"}}`, g.base)
	return true
}

// The row of a task at its merge place sets the base of the task's open pull request to the default
// branch only where GitHub's differs, and applies the end of the join with that base: it creates no
// branch and sends no POST /merges. (1) GitHub holds #4 against main already: no PATCH, and the fact
// names Base main. (2) GitHub holds #4 against legion/LEGION-208-stack: one PATCH to main. In both
// the row run again sends no second PATCH and applies no second fact.
//
// The fault it catches is a merge's row that joins as a start's does, which merges the needs'
// branches into the task's branch at its merge (its control: taskBranch with no case of the merge
// place, under which the row reads and writes branches the fake does not serve).
func TestTheBranchRowSetsTheBaseOnlyWhereItDiffers(t *testing.T) {
	for _, tc := range []struct {
		name, base string
		writes     []string
	}{
		{"GitHub holds the default branch", "main", nil},
		{"GitHub holds another base", "legion/LEGION-208-stack", []string{"PATCH base main"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTaskBranchRig(t, plannedTask{id: "a"})
			github := &pullBaseGitHub{base: tc.base}
			rig.runner.githubAPI = githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
				if !github.serve(w, r) {
					githubtest.Unexpected(t, w, r)
				}
			}})
			handler := &outboxFactHandler{}
			rig.runner.handlers = []intake.Handler{handler}
			ctx := context.Background()
			if err := pgx.BeginFunc(ctx, rig.pool, func(tx pgx.Tx) error {
				if err := rig.records.MoveTask(ctx, tx, taskBranchIssue.Key, taskBranchIssue.Generation, "a", record.TaskPlace{Kind: record.PlaceMerge}, record.TaskRunning); err != nil {
					return err
				}
				return rig.records.PutPullRequest(ctx, tx, record.PullRequest{Issue: taskBranchIssue.Key, Repo: "acme/widgets", Number: 4, Branch: rig.branch(t, "a"),
					Base: tc.base, HeadSHA: "h", State: record.PullRequestOpen, Failing: []string{}})
			}); err != nil {
				t.Fatalf("stand task a at its merge with #4 open: %v", err)
			}
			row := mustOutboxRow(t, taskBranchIssue.Key, record.TaskBranch{Generation: taskBranchIssue.Generation, Task: "a"}, time.Now())
			row.ID = 21

			for range 2 {
				if err := rig.runner.execute(ctx, row); err != nil {
					t.Fatalf("execute the branch row of task a at its merge: %v", err)
				}
			}

			if !reflect.DeepEqual(github.writes, tc.writes) {
				t.Fatalf("the requests to GitHub beside the read of #4 = %q, want %q: no branch, no merge, and a PATCH only where the base differs", github.writes, tc.writes)
			}
			want := intake.TaskJoined{Issue: taskBranchIssue.Key, Generation: taskBranchIssue.Generation, Task: "a", Branch: rig.branch(t, "a"), Base: "main"}
			if got := handler.facts(); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
				t.Fatalf("the facts the row applied = %#v, want the one %#v", got, want)
			}
		})
	}
}
