// The step record of a key's agent counts only the launches of its step's role. One key's agent
// runs steps of two roles in one claim (dispatch://INFRA-4112), and its step of the second role
// starts while the claim still runs the first role's launch. The whole daemon runs over real
// PostgreSQL, NATS and Dispatch, a fake runtime and a fake GitHub (ownerStepsRig), and each pane's
// shim counts its agent's calls (stepClaimant).
package daemon

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The owner gives `implementer:a tester:a reviewer`: key a's agent, `…-implementer`, runs step 1
// as the implementer, its launch set to stepRecordsModel, and step 2 as the tester. Step 2 starts
// at the implementer's completion, while the claim still runs the implementer's launch. The claim
// takes the tester's role once that launch has ended its turn, and its next launch, set to
// stepRecordsModelB as the tester's settings give it, runs step 2. The implementer's model
// answers once more after the completion, in the turn the completion ends, and that launch's final
// totals count the call. Step 2's record names the tester's set model and the calls of the
// tester's launch alone, and step 1's names the implementer's set model and its one call before
// the completion. Counted from the implementer's launch, as a start counts a live launch of its
// own role, step 2 would name no set model, since its two launches name two, and the implementer's
// call after the completion would be among its answers.
func TestAKeyedAgentsStepOfAnotherRoleCountsOnlyThatRolesLaunch(t *testing.T) {
	t.Parallel()
	r := newAgentsRig(t)
	r.givePlan(oneKeyPlan...)
	r.openGate(phase.Implementing)
	r.github.open("legion/"+r.key, r.number)
	agent := r.worker(claim.RoleImplementer)
	launched := len(launchSpecsOf(r.rt, agent.token))
	implementing := r.d.claim(agent.token).Generation
	agent.take()
	req := implementCompletion
	req.HandoffID, req.HandoffVersion = r.writeHandoff(agent.session, "implement")
	if status, body := r.complete(agent, req); status != http.StatusOK {
		t.Fatalf("the implementer's completion = %d %s, want 200", status, body)
	}
	agent.answer()
	agent.sendFinal()
	r.waitFinalKept(agent.token, implementing)
	agent.endTurn()
	r.waitPhase(phase.Testing)

	r.relaunchedCountingAs(agent, launched, claim.RoleTester, stepRecordsModelB)
	agent.take()
	agent.answer()
	r.finish(agent, "test", testCompletion, phase.Reviewing)

	r.waitModels(r.key, 1, 1, stepModels{result: "passed", setModel: stepRecordsModel,
		answers: answerList(answerOf(stepRecordsModel, 1)), model: stepRecordsModel})
	r.waitModels(r.key, 2, 1, stepModels{result: "passed", setModel: stepRecordsModelB,
		answers: answerList(answerOf(stepRecordsModelB, 2)), model: stepRecordsModelB})
}

// relaunchedCountingAs is relaunchedAs for a pane whose shim counts its agent's calls: the launch
// of c's claim after before launches of it must run role and resume c's session, and its pane, set
// to model, connects a counting shim with the launch's boot token, registers in that session and
// says ready. The new launch counts from zero, its read number too, and its agent answers with
// model. It answers the launch's spec.
func (r *ownerStepsRig) relaunchedCountingAs(c *stepClaimant, before int, role claim.Role, model string) runtime.SpawnSpec {
	t := r.t
	t.Helper()
	testwait.WithinPolling(t, stepRecordsWait, 50*time.Millisecond, "the daemon's relaunch of "+string(c.token)+" as "+string(role), func() bool {
		return len(launchSpecsOf(r.rt, c.token)) > before
	})
	launch := lastLaunch(t, r.rt, c.token)
	if launch.Role != role || launch.ResumeSessionFile != sessionFile(c) || launch.ResumeSessionID != c.session {
		t.Fatalf("the daemon relaunched %s as %s resuming %q by the session id %q, want it as %s resuming %s by the session id %s, its own session",
			c.token, launch.Role, launch.ResumeSessionFile, launch.ResumeSessionID, role, sessionFile(c), c.session)
	}
	c.closeConn()
	r.built.mu.Lock()
	address := r.built.address
	r.built.mu.Unlock()
	sh := dialCountingShim(t, address, launch.BootToken)
	c.bootToken, c.secret = launch.BootToken, c.register(launch.BootToken)
	c.mu.Lock()
	c.sh, c.role, c.open, c.inTurn = sh, role, nil, false
	c.model, c.setModel = model, model
	c.counts, c.outsideForm, c.overflowed, c.read = nil, 0, false, 0
	c.mu.Unlock()
	d := r.d
	sh.serve(func() { c.serve(sh, d) })
	c.ready(launch.Generation)
	return launch
}

// sendFinal is the shim sending its launch's final totals unasked, the read number raised by 1, as
// it does when its harness exits.
func (c *stepClaimant) sendFinal() {
	c.mu.Lock()
	c.read++
	totals := c.totalsLocked()
	c.mu.Unlock()
	c.send(shimwire.ModelTotalsFinal{Totals: totals})
}

// waitFinalKept waits for the store to hold final totals of token's launch at generation, which the
// daemon keeps on a goroutine of its own (supervision.keepFinal).
func (r *ownerStepsRig) waitFinalKept(token claim.Token, generation uint64) {
	t := r.t
	t.Helper()
	testwait.WithinPolling(t, stepRecordsWait, 20*time.Millisecond, "the final totals of "+string(token)+"'s launch", func() bool {
		var kept bool
		err := r.st.Pool().QueryRow(context.Background(), `select final is not null from claim_launches where claim = $1 and generation = $2`,
			string(token), int64(generation)).Scan(&kept)
		return err == nil && kept
	})
}
