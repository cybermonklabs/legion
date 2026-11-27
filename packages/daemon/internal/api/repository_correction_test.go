package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// A child out of the workflow whose repository changed in Dispatch meanwhile runs again on the
// repository Dispatch resolves it to now: rerun_child writes todo in Dispatch and applies that
// write in its own request (applyOwnStatusWrite), the write's answer is the issue as Dispatch
// holds it, resolved repository and all (intake.OwnStatusWrite), and the fact that answer makes
// has the record follow Dispatch's resolution (the engine's dispatchIssue,
// workflow.Engine.ApplyRepositoryChange) in the same transaction that opens the child's next run.
// No event of the repository change is ever delivered here: the route's own write is the whole of
// what the daemon reads. dispatch://INFRA-2865 measured the absence of this, where a move to the backlog
// and a run again gave a new generation on the repository the issue had left. The child leaves the
// workflow first by its owning architect's own backlog in Dispatch (ownersBacklog), which the
// daemon takes as it takes a person's move (dispatch://INFRA-3178).
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

	recorder := h.request(http.MethodPost, "/legion/v1/children/rerun", map[string]any{"grantId": architect.grant(t), "issue": statusChild}, nil)
	if recorder.Code != http.StatusOK || strings.TrimSpace(recorder.Body.String()) != "{}" {
		t.Fatalf("rerun = %d %s, want 200 {}", recorder.Code, recorder.Body)
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
		t.Fatalf("the child's record once rerun_child answered = %#v, want its next run (generation 2) planning on the corrected repository", rerun)
	}
}
