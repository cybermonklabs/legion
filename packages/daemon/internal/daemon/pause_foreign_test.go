package daemon

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A claim of another project's issue is a session this daemon runs, so a pause of the project
// stops it and holds it as it holds every other claim but the controller's (the Summary and R2 of
// dispatch://INFRA-3759: while the project is paused, the daemon launches no session and hands
// none a task except the controller's, and each other session stops; the architect's reading at
// dispatch://INFRA-3759/comment/80967314-0b83-4d8a-8a0c-a329afb72e1e). The operator's spawn makes
// such a claim under the daemon's own project token (api/operator.go builds the token from
// s.project and checks only the key's shape). Review round 2's defect stays fixed too: the pause
// succeeds with such a claim live, rather than failing on the stop table's foreign key.
//
// While the project is paused, a spawn of another project's issue answers 409 PROJECT_PAUSED and
// launches nothing, as a spawn of the project's own issue does, and so does a delivery to the
// claim the pause stopped.
func TestAPauseStopsAndHoldsAClaimOfAnotherProjectsIssue(t *testing.T) {
	t.Parallel()
	r := newPauseDaemonRig(t, nil)
	X := r.issue(1)
	r.seed(r.root(X, phase.Planning), r.running(X, claim.RoleArchitect, supervise.StateIdle))
	r.boot()
	r.controllerUp()

	// A spawn of another project's issue, up and idle in its own process.
	const foreignIssue = "OTHERPROJECT-5"
	// The launch reads the issue's repository from Dispatch, so Dispatch holds the issue.
	r.board.place(foreignIssue, "todo", "", false)
	foreign := r.d.spawn(api.SpawnRequest{Tree: foreignIssue, Issue: foreignIssue, Role: claim.RoleArchitect, Prompt: "Reply ready and wait."})
	testwait.Eventually(t, "the spawn of another project's issue to launch", func() bool { return len(r.rt.launchesOf(foreign)) == 1 })
	launched := r.claim(foreign)
	session := "ses-other-project"
	r.handle(foreign, supervise.StreamHello{Claim: foreign, Generation: launched.Generation})
	r.handle(foreign, supervise.RequestRegister{Claim: foreign, Generation: launched.Generation, Session: session,
		SessionFile: "/sessions/" + session + ".jsonl", PluginContract: api.DaemonAPIVersion()})
	r.handle(foreign, supervise.RequestReady{Claim: foreign, Generation: launched.Generation, Session: session})
	testwait.Eventually(t, "the spawn's agent to be ready", func() bool {
		state := r.claim(foreign).State
		return state == supervise.StateReady || state == supervise.StateIdle
	})

	// The pause succeeds, and stops that claim as it stops the project's own architect.
	r.pauseProject()
	r.awaitState(r.token(X, claim.RoleArchitect), supervise.StateSuspended, "the pause stops the project's own idle architect")
	if !testwait.Holds(testwait.Bounded(t, 10*time.Second, time.Second), testwait.Poll, func() bool {
		return r.claim(foreign).State == supervise.StateSuspended
	}) {
		c := r.claim(foreign)
		t.Fatalf("the spawn of %s is %s ten seconds into the pause, want it stopped: the daemon runs it, so the pause stops it (R2)", foreignIssue, c.State)
	}

	// While paused, a spawn of another project's issue is refused and launches nothing.
	launches := len(r.rt.launches())
	status, body := r.d.request(http.MethodPost, "/legion/v1/operator/claims",
		api.SpawnRequest{Tree: "OTHERPROJECT-6", Issue: "OTHERPROJECT-6", Role: claim.RoleArchitect, Prompt: "Reply ready and wait."}, true)
	var refusal struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(body, &refusal)
	if status != http.StatusConflict || refusal.Code != "PROJECT_PAUSED" {
		t.Fatalf("a spawn of another project's issue while the project is paused = %d %s, want 409 PROJECT_PAUSED", status, strings.TrimSpace(string(body)))
	}
	time.Sleep(time.Second)
	if got := r.rt.launches()[launches:]; len(got) != 0 {
		t.Fatalf("%d launches after the refused spawn (%+v), want none while the project is paused", len(got), got)
	}

	// A delivery to the stopped claim is refused for the pause too.
	status, body = r.d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(foreign)+"/deliver", api.DeliverRequest{Task: "while paused"}, true)
	refusal.Code = ""
	_ = json.Unmarshal(body, &refusal)
	if status != http.StatusConflict || refusal.Code != "PROJECT_PAUSED" {
		t.Fatalf("a delivery to the stopped claim of another project's issue while paused = %d %s, want 409 PROJECT_PAUSED", status, strings.TrimSpace(string(body)))
	}
}
