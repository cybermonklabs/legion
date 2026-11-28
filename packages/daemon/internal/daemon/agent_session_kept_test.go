package daemon

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A keyed agent keeps its session across a version that moves its second step, through the
// engine, the outbox's runner and the claim's machine. The owner gives `implementer:a tester:a
// reviewer`: key a's agent, `…-implementer`, works step 1 and then step 2 in one session. The owner
// stops task a at step 2 and gives version 2, `implementer:a reviewer tester:a`, which starts the
// task again at its first step. Each start of key a's steps in version 2 — step 1, step 3 and the
// retro — names key a, the agent the claim records as its session's, so each relaunch of
// `…-implementer` resumes the one session its first launch began (relaunchedAs), and the claim is
// spawned once. The fault: the start of version 2's step 1 was judged by the plan in force at the
// claim's last row, place 2, where version 2 has the reviewer, and key a's own session was left
// behind as another agent's.
func TestAKeyedAgentKeepsItsSessionAcrossAVersionThatMovesItsStep(t *testing.T) {
	r := newAgentsRig(t)
	artifact := r.givePlan("implementer:a", "tester:a", "reviewer")
	r.openGate(phase.Implementing)
	r.github.open("legion/"+r.key, r.number)
	implementer := r.worker(claim.RoleImplementer)
	launched := len(launchSpecsOf(r.rt, implementer.token))
	r.work(claim.RoleImplementer, "implement", implementCompletion, phase.Testing)
	r.relaunchedAs(implementer, launched, claim.RoleTester)
	launched++
	implementer.takeTask()

	status, body := r.d.request(http.MethodPost, "/legion/v1/tasks/stop", map[string]any{
		"grantId": r.worker(claim.RoleArchitect).grant(), "issue": r.key, "task": "a"}, false)
	if status != http.StatusOK {
		t.Fatalf("stop_task of %s's task a = %d %s, want 200", r.key, status, body)
	}
	implementer.endTurn()
	testwait.WithinPolling(t, stepRecordsWait, 100*time.Millisecond, "the step_stop of "+string(implementer.token), func() bool {
		stopped := r.d.claim(implementer.token)
		return stopped.State == string(supervise.StateSuspended) && stopped.SuspendedBy == string(supervise.SuspendedByDaemon)
	})
	r.holds(r.key, record.HoldTaskStopped)

	// A later upload of the same name is that document's next version, naming the token of the
	// version it replaces.
	var read struct {
		Token string `json:"token"`
	}
	r.person(http.MethodGet, "/api/v1/artifacts/"+artifact+"/text", nil, http.StatusOK, &read)
	var uploaded struct {
		Artifact struct {
			ID string `json:"id"`
		} `json:"artifact"`
	}
	r.person(http.MethodPost, "/api/v1/issues/"+r.key+"/artifacts", map[string]any{"name": "plan.md",
		"content": planOf("implementer:a", "reviewer", "tester:a"), "precondition": map[string]string{"document": read.Token}},
		http.StatusCreated, &uploaded)
	if uploaded.Artifact.ID != artifact {
		t.Fatalf("the second upload of plan.md made the document %s, want version 2 of %s", uploaded.Artifact.ID, artifact)
	}
	if status, body := r.registerPlan(artifact, 2); status != http.StatusOK {
		t.Fatalf("register_plan of %s's plan version 2 = %d %s, want 200", r.key, status, body)
	}
	r.waitPhase(phase.Implementing)

	r.relaunchedAs(implementer, launched, claim.RoleImplementer)
	launched++
	implementer.takeTask()
	r.finish(implementer, "implement", implementCompletion, phase.Reviewing)
	r.review("APPROVED", phase.Testing)
	r.relaunchedAs(implementer, launched, claim.RoleTester)
	launched++
	if task := implementer.takeTask(); !strings.Contains(task, "Step 3 of 3: tester:a.") {
		t.Fatalf("key a's task at version 2's step 3 is %q, want step 3, tester:a", task)
	}
	r.finish(implementer, "test", testCompletion, phase.Retro)
	r.relaunchedAs(implementer, launched, claim.RoleImplementer)
	r.retro()
	r.merge()

	spawns := 0
	for _, call := range r.rt.Calls() {
		if call.Method == "Spawn" && call.Spec.Claim == implementer.token {
			spawns++
		}
	}
	if spawns != 1 {
		t.Fatalf("%s was spawned %d times, want once: every later launch resumes key a's one session", implementer.token, spawns)
	}
}
