package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// baseGitHub is GitHub's REST API for acme/widgets as an implementer's completion reads it: the
// list of the open pull requests on the branch the read asks for, which lists the pull request
// open holds for that branch and none for any other branch. A test changes open between calls.
type baseGitHub struct {
	mu   sync.Mutex
	open map[string]openPull
}

// openPull is one open pull request of baseGitHub: its number and the branch it is against.
type openPull struct {
	number int
	base   string
}

// newBaseGitHub serves a baseGitHub that lists nothing, and answers the harness's view of it.
func newBaseGitHub(t *testing.T) (*baseGitHub, *handoffGitHub) {
	t.Helper()
	g := &baseGitHub{open: map[string]openPull{}}
	url := githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/widgets/pulls" {
			githubtest.Unexpected(t, w, r)
			return
		}
		_, branch, _ := strings.Cut(r.URL.Query().Get("head"), ":")
		g.mu.Lock()
		pull, ok := g.open[branch]
		g.mu.Unlock()
		if !ok {
			_, _ = io.WriteString(w, "[]")
			return
		}
		_, _ = fmt.Fprintf(w, `[{"number":%d,"state":"open","html_url":"https://github.com/acme/widgets/pull/%d","head":{"ref":%q,"sha":%q,"repo":{"full_name":"acme/widgets"}},"base":{"ref":%q},"updated_at":"2026-11-22T06:00:00Z"}]`,
			pull.number, pull.number, branch, headSHA, pull.base)
	}})
	return g, &handoffGitHub{url: url}
}

// opens makes pull the open pull request on branch.
func (g *baseGitHub) opens(branch string, pull openPull) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.open[branch] = pull
}

// seedTasksOfBases gives key, at generation 1, an owner's plan of four tasks, each of one
// implementer's step: a and b need nothing, c needs b, and d, the task that merges, needs a and c.
// Their starts gave a the issue's own branch, b legion/<KEY>.b and c legion/<KEY>.c
// (record.Postgres.AssignTaskBranch). b is done, a and c stand at their implementer's step, a's of
// the agent implementer-2 and c's of the agent implementer, and d waits.
func seedTasksOfBases(t *testing.T, h *harness, key string) {
	t.Helper()
	ctx, records := context.Background(), record.NewStore()
	v := record.PlanVersion{Issue: key, Generation: 1, Version: 1, ArtifactID: "plan-" + key, Source: record.PlanSourceOwner,
		GivenBy: "owner", GivenAt: time.Date(2026, 11, 22, 6, 0, 0, 0, time.UTC)}
	var tasks []record.PlanTask
	var steps []record.PlanStep
	for _, task := range []struct {
		id    string
		needs []string
		agent claim.Agent
	}{{"a", nil, "implementer-2"}, {"b", nil, "implementer-3"}, {"c", []string{"b"}, "implementer"}, {"d", []string{"a", "c"}, "implementer-4"}} {
		tasks = append(tasks, record.PlanTask{Issue: key, Generation: 1, Task: task.id, PlanVersion: 1, BlockText: "Task " + task.id + ".", Needs: task.needs, State: record.TaskWaiting})
		steps = append(steps, record.PlanStep{Issue: key, Generation: 1, Task: task.id, Position: 1, Role: "implementer", Type: "implementation", Agent: task.agent})
	}
	if err := store.Transact(ctx, h.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		if err := records.PutPlan(ctx, tx, v, tasks, steps); err != nil {
			return err
		}
		for _, at := range []struct {
			task  string
			state record.TaskState
		}{{"a", record.TaskRunning}, {"b", record.TaskDone}, {"c", record.TaskRunning}} {
			if _, err := records.AssignTaskBranch(ctx, tx, key, 1, at.task); err != nil {
				return err
			}
			if err := records.MoveTask(ctx, tx, key, 1, at.task, record.TaskPlace{Kind: record.PlaceStep, Position: 1}, at.state); err != nil {
				return err
			}
			if at.state != record.TaskRunning {
				continue
			}
			// The move writes the wait of a start for its join; this seed stands for a task whose join ended.
			if err := records.SetStepWait(ctx, tx, key, 1, at.task, record.StepWaitNone, nil); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the plan of %s: %v", key, err)
	}
}

// An implementer's completion of a task of a plan is held to the base the plan gives that task's
// pull request (record.StoredPlan.BaseOf), for a plan of any count of tasks: c needs b, and with
// GitHub's open pull request on c's branch against the default branch, c's implementer's completion
// is refused 409 HANDOFF_PULL_REQUEST_BASE, naming b's branch and the command that sets it, and
// nothing is applied. The same completion with that pull request against b's branch is applied, as
// a completion of task c. The one task of a plan of one is the task that merges, whose base is the
// default branch: its implementer's completion on a pull request against another base is refused
// the same way, and taken, as a completion of that task, against the default branch.
//
// The fault it catches is a task's work in a pull request against a base the plan does not give,
// which a later merge would land there (its control: the read without the base check, under which
// the first completion is applied).
func TestACompletionOnAPullRequestAgainstAnotherBaseIsRefused(t *testing.T) {
	t.Run("a plan of several tasks", func(t *testing.T) {
		github, served := newBaseGitHub(t)
		h, facts := newHandoffHarness(t, served, nil, nil)
		const key = "LEGION-212"
		seedIssueAt(t, h, key, phase.Implementing)
		seedTasksOfBases(t, h, key)
		github.opens("legion/LEGION-212.c", openPull{number: 7, base: "main"})

		implementer := newLiveClaim(t, h, key, claim.RoleImplementer)
		written := implementer.writeHandoff("implement", 1)
		complete := func() *httptest.ResponseRecorder {
			t.Helper()
			return h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "implemented", Commit: headSHA, HandoffID: written, HandoffVersion: 1})
		}
		refusedSaying(t, complete(), http.StatusConflict, "HANDOFF_PULL_REQUEST_BASE",
			"pull request #7 on legion/LEGION-212.c is against main, and the base of task c is legion/LEGION-212.b: set it with gh pr edit 7 --base legion/LEGION-212.b, then complete again.")
		if got := facts.recorded(); len(got) != 0 {
			t.Fatalf("the refused completion applied %#v, want nothing", got)
		}

		github.opens("legion/LEGION-212.c", openPull{number: 7, base: "legion/LEGION-212.b"})
		completion(t, complete())
		if fact := lastCompletion(t, facts); fact.Task != "c" || fact.Role != claim.RoleImplementer {
			t.Fatalf("the completion applied %+v, want the implementer's completion of task c", fact)
		}
		if opened, ok := facts.recorded()[0].(intake.PullRequestOpened); !ok || opened.Number != 7 || opened.Branch != "legion/LEGION-212.c" {
			t.Fatalf("the first fact applied = %#v, want #7 opened on legion/LEGION-212.c", facts.recorded()[0])
		}
	})

	t.Run("a plan of one task", func(t *testing.T) {
		github, served := newBaseGitHub(t)
		h, facts := newHandoffHarness(t, served, nil, nil)
		const key = "LEGION-213"
		seedIssueAt(t, h, key, phase.Implementing)
		seedTaskPlan(t, h, key, taskStanding{task: "a", state: record.TaskRunning, place: 1})
		github.opens("legion/LEGION-213", openPull{number: 8, base: "release"})

		implementer := newLiveClaim(t, h, key, claim.RoleImplementer)
		written := implementer.writeHandoff("implement", 1)
		complete := func() *httptest.ResponseRecorder {
			t.Helper()
			return h.complete(HandoffCompleteRequest{GrantID: implementer.grant(t), Summary: "implemented", Commit: headSHA, HandoffID: written, HandoffVersion: 1})
		}
		refusedSaying(t, complete(), http.StatusConflict, "HANDOFF_PULL_REQUEST_BASE",
			"pull request #8 on legion/LEGION-213 is against release, and the base of task a is main: set it with gh pr edit 8 --base main, then complete again.")
		if got := facts.recorded(); len(got) != 0 {
			t.Fatalf("the refused completion applied %#v, want nothing", got)
		}

		github.opens("legion/LEGION-213", openPull{number: 8, base: "main"})
		completion(t, complete())
		if fact := lastCompletion(t, facts); fact.Role != claim.RoleImplementer || fact.Task != "a" {
			t.Fatalf("the completion applied %+v, want the implementer's completion of the issue's one task, a", fact)
		}
	})
}
