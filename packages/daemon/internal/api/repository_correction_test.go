package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// A child out of the workflow whose repository changed in Dispatch meanwhile runs again on the
// repository Dispatch resolves it to now. rerun_child queues the child's todo as a status row,
// which its answer names, and opens the child's next run on its record before Dispatch echoes the
// write (decision 5 of dispatch://INFRA-4730, the workflow's statusCall), from the record alone:
// until the echo, generation 2 still holds the repository the child left, with no correction. The
// outbox writes the todo, and its echo, the Dispatch event of the daemon's own write, carries the
// issue as Dispatch holds it, resolved repository and all, readied as the consumer readies an
// issue's event (Options.PrepareRepository): the fact it makes has the record follow Dispatch's
// resolution (the engine's dispatchIssue, workflow.Engine.ApplyRepositoryChange) on the run the
// rerun opened. No other event of the repository change is delivered here: the write's own echo
// is the whole of what the daemon reads. dispatch://INFRA-2865 measured the absence of this, where
// a move to the backlog and a run again left the new generation on the repository the issue had
// left. The child leaves the workflow first by its owning architect's own backlog in Dispatch
// (ownersBacklog), which the daemon takes as it takes a person's move (dispatch://INFRA-3178).
// What the first launch of that run is told, both repositories, is the daemon's
// TestRepositoryCorrectionFollowsRerunChildIntoTheFirstLaunch.
func TestRepositoryCorrectionFollowsRerunChild(t *testing.T) {
	h, handlers, statuses := statusHarness(t)
	architect := newLiveClaim(t, h, statusRoot, claim.RoleArchitect)
	ownersBacklog(t, h, handlers, statuses, architect, statusChild)
	if out := recordedChild(t, h, statusChild); out.Repo != "acme/widgets" {
		t.Fatalf("the child out of the workflow = %#v, want it on acme/widgets, the repository Dispatch resolved it to then", out)
	}

	// Dispatch moves the child to another repository the project serves while it is out of the
	// workflow.
	moved := statuses.issues[statusChild]
	moved.ResolvedRepo = "sjawhar/legion"
	statuses.issues[statusChild] = moved

	recorder := h.request(http.MethodPost, "/legion/v1/children/rerun", RerunChildRequest{GrantID: architect.grant(t), Issue: statusChild}, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("rerun = %d %s, want 200", recorder.Code, recorder.Body)
	}
	queuedWrite(t, h, recorder, statusChild, "todo")
	// Before the echo, the run the rerun opened stands on the repository the child left: the
	// record has read nothing of Dispatch's new resolution yet.
	opened := recordedChild(t, h, statusChild)
	if opened == nil || opened.Generation != 2 {
		t.Fatalf("the child once rerun_child answered = %#v, want its next run, generation 2", opened)
	}
	if opened.Repo != "acme/widgets" || opened.GitHubRepo != "acme/widgets" || opened.RepoCorrection != nil || opened.RepoChange != nil {
		t.Fatalf("the child's next run before the todo's echo is on %s (spelled %s), corrected %+v, change %+v; "+
			"want acme/widgets, the repository it left, corrected in nothing and with no change waiting",
			opened.Repo, opened.GitHubRepo, opened.RepoCorrection, opened.RepoChange)
	}

	// The outbox writes the queued todo, and Dispatch's event of that write arrives.
	echo := statusEcho(moved.LastSeq+1, "todo")
	echo.ResolvedRepo = moved.ResolvedRepo
	prepared, err := testRepositories.prepare(context.Background(), echo)
	if err != nil {
		t.Fatalf("prepare the todo's echo: %v", err)
	}
	if _, err := intake.ApplyFact(context.Background(), h.store.Pool(), "dispatch", "rerun-todo-echo", prepared, handlers...); err != nil {
		t.Fatalf("apply the todo's echo: %v", err)
	}

	rerun := recordedChild(t, h, statusChild)
	if rerun == nil {
		t.Fatalf("%s is not recorded", statusChild)
	}
	if rerun.Repo != "sjawhar/legion" || rerun.DefaultBranch != "main" || rerun.GitHubRepo != "sjawhar/legion" {
		t.Fatalf("the child's next run is on %s at %s, spelled %s; want sjawhar/legion at main: the record follows the repository Dispatch resolves it to",
			rerun.Repo, rerun.DefaultBranch, rerun.GitHubRepo)
	}
	if c := rerun.RepoCorrection; c == nil || c.Before != "acme/widgets" {
		t.Fatalf("the correction = %+v, want acme/widgets left at the moment the correction was applied", c)
	}
	if rerun.RepoChange != nil {
		t.Fatalf("the waiting change after the rerun = %+v, want none: Legion serves the repository, so the correction applied it", rerun.RepoChange)
	}
	if rerun.Generation != 2 || rerun.Phase != phase.Planning {
		t.Fatalf("the child's record once the todo's echo arrived = %#v, want the run the rerun opened (generation 2) planning on the corrected repository", rerun)
	}
}
