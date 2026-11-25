// The production check a merge opens follows the plan the issue ran: the role the project checks
// production with takes the check's look in the session that worked the task where the plan ran
// that role, and in a fresh session where it did not. Each test boots the whole daemon over the
// real Dispatch of this package's tests and walks each issue from its owner's plan to its merge
// (ownerStepsRig, owner_steps_rig_test.go).
package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A production check after one merge: under `production_check: tester`, an issue whose plan is
// `implementer tester` is done at its merge, and the check the merge opens gives its first look to
// the tester's own session, the one that tested the task: the claim takes it where its pane still
// runs, and a resume of that session where the close's suspension has stopped it. An issue whose
// plan is `implementer` runs no tester, and the check's first look opens on a fresh tester session,
// a spawn that resumes none. Each issue is done before its check ends, and each check ends with the
// tester's report. The control is `production_check: off`, under which the same walk opens no check.
func TestAProductionCheckFollowsAPlan(t *testing.T) {
	t.Parallel()
	t.Run("a plan with a tester and a plan with none", func(t *testing.T) {
		r := newOwnerStepsRig(t, newLiveProofOn(t, workflowNATS(t)), startScratchDispatchOn(t, scratchDispatchOptions{}), 2,
			config.Project{ProductionCheck: claim.RoleTester})
		r.givePlan("implementer", "tester")
		r.openGate(phase.Implementing)
		r.walkSteps([]string{"implementer", "tester"})
		tester := r.worker(claim.RoleTester)
		session := tester.session
		r.lookOpens(tester)
		if got := r.d.claim(tester.token).Session; got != session {
			t.Fatalf("%s's tester holds the session %q at the check's look, want %q, the one that tested the task", r.key, got, session)
		}
		r.reportLook(tester)

		r.on(r.issues[1].key)
		r.givePlan("implementer")
		r.openGate(phase.Implementing)
		r.walkSteps([]string{"implementer"})
		fresh := r.worker(claim.RoleTester)
		if launches := r.launchesOfRole(claim.RoleTester); len(launches) != 1 || launches[0] != "Spawn" || lastLaunch(t, r.rt, fresh.token).ResumeSessionFile != "" {
			t.Fatalf("%s's tester was launched %v, want one Spawn that resumes no session: no tester ran on the plan implementer, so the check opens on a fresh session", r.key, launches)
		}
		r.lookOpens(fresh)
		r.reportLook(fresh)
	})

	t.Run("control: production_check off opens none", func(t *testing.T) {
		r := newOwnerStepsRig(t, newLiveProofOn(t, workflowNATS(t)), startScratchDispatchOn(t, scratchDispatchOptions{}), 1, config.Project{})
		r.givePlan("implementer", "tester")
		r.openGate(phase.Implementing)
		r.walkSteps([]string{"implementer", "tester"})
		for deadline := time.Now().Add(liveSettle); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			if r.checkOpened() {
				t.Fatalf("%s's merge opened a production check under production_check: off", r.key)
			}
		}
	})
}

// checkOpened is whether the walk's issue has a production check in the daemon's store.
func (r *ownerStepsRig) checkOpened() bool {
	r.t.Helper()
	var open bool
	if err := r.st.Pool().QueryRow(context.Background(), `select exists (select 1 from production_checks where issue = $1)`, r.key).Scan(&open); err != nil {
		r.t.Fatalf("read whether %s has a production check: %v", r.key, err)
	}
	return open
}

// lookOpens waits for the merge of the walk's issue to open its production check, the first look
// open for the tester while the issue is done, and for c, the tester's claimant, to take that
// look, which its turn then takes up. A launch of c's claim after the one its pane came up in is the
// look's start resuming the claim, which the close had suspended: that launch must resume c's own
// session, and its pane comes up in it (reattach) before its plugin takes the look.
func (r *ownerStepsRig) lookOpens(c *stepClaimant) {
	t := r.t
	t.Helper()
	testwait.WithinPolling(t, stepRecordsWait, 100*time.Millisecond, "the merge to open "+r.key+"'s production check", r.checkOpened)
	if check := readProductionCheck(t, r.st.Pool(), r.records, r.key); check.Role != claim.RoleTester || check.Looks != 1 || !check.LookOpen || !check.Open() {
		t.Fatalf("the check the merge of %s opened is %+v, want look 1 open for the tester", r.key, check)
	}
	if got := string(r.d.state().Issues[r.key].Phase); got != string(phase.Done) {
		t.Fatalf("%s stands in %s while its production check is open, want done", r.key, got)
	}
	var taken takenTask
	deadline := time.Now().Add(stepRecordsWait)
	for {
		got, err := c.driver.takeWithin(100 * time.Millisecond)
		if err == nil {
			taken = got
			break
		}
		if lastLaunch(t, r.rt, c.token).BootToken != c.bootToken {
			r.reattach(c)
			continue
		}
		if time.Now().After(deadline) {
			held := r.d.claim(c.token)
			t.Fatalf("%s's tester took nothing within %s: its claim is %s, the daemon launched it %v, and its last take: %v",
				r.key, stepRecordsWait, held.State, r.launchesOfRole(claim.RoleTester), err)
		}
	}
	// A resumed session is first told why its process ended, and the check's task follows.
	if lead := "Production check of " + r.key + ":"; !strings.HasPrefix(taken.Text, lead) && !strings.Contains(taken.Text, "\n\n"+lead) {
		t.Fatalf("%s's tester took %q, want the first look of the production check", r.key, taken.Text)
	}
	c.answer()
	c.driver.mustStartTurn(t)
	c.waitState(supervise.StateWorking)
}

// reattach is c's pane coming up in its claim's latest launch, which must resume c's own session:
// it says hello with that launch's boot token, registers in that session and says ready, as the
// plugin does.
func (r *ownerStepsRig) reattach(c *stepClaimant) {
	t := r.t
	t.Helper()
	launch := lastLaunch(t, r.rt, c.token)
	file := "/sessions/" + c.session + ".jsonl"
	if launch.ResumeSessionFile != file {
		t.Fatalf("the daemon launched %s again with the session file %q, want a resume of %s, its own session's", c.token, launch.ResumeSessionFile, file)
	}
	r.built.mu.Lock()
	address := r.built.address
	r.built.mu.Unlock()
	// The suspension stopped the pane its claimant played, so that pane's connection closes, and
	// the claim's stream takes the new pane's hello once the listener has read the close.
	c.mu.Lock()
	_ = c.sh.conn.Close()
	c.mu.Unlock()
	sh := dialShimUntilAcked(t, address, launch.BootToken)
	c.mu.Lock()
	c.sh, c.bootToken = sh, launch.BootToken
	c.mu.Unlock()
	status, body := r.d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: launch.BootToken, SessionID: c.session, OmpSessionFile: file, AgentID: c.session, PluginContract: api.DaemonAPIVersion(),
	}, false)
	if status != http.StatusOK {
		t.Fatalf("register %s again in its session %s = %d %s, want 200", c.token, c.session, status, body)
	}
	var registered claim.RegisterResponse
	if err := json.Unmarshal(body, &registered); err != nil {
		t.Fatalf("decode the registration of %s: %v", c.token, err)
	}
	c.secret = registered.Secret
	// The new pane's plugin takes the claim's tasks under the registration it just made.
	c.driver = newTakeDriver(func(path string, body, answer any) error { return c.r.d.post(path, body, answer) }, c.write,
		claim.AgentCaller{ClaimToken: c.token, SessionID: c.session, Secret: c.secret}, launch.Generation)
	c.driver.started = func() (bool, error) { return c.r.d.turnRead(c.token)() }
	d := r.d
	sh.serve(func() { c.serve(sh, d) })
	if status, body := r.d.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: c.token, SessionID: c.session, Secret: c.secret, Generation: launch.Generation,
	}, false); status != http.StatusNoContent {
		t.Fatalf("ready %s = %d %s, want 204", c.token, status, body)
	}
}

// reportLook is c's report of pass on the look its turn took up, which ends the check, and the wait
// for the check to end with the issue still done.
func (r *ownerStepsRig) reportLook(c *stepClaimant) {
	t := r.t
	t.Helper()
	if status, body := r.complete(c, api.HandoffCompleteRequest{Summary: "production holds every requirement", Verdict: "pass"}); status != http.StatusOK {
		t.Fatalf("the tester's report on %s's look = %d %s, want 200", r.key, status, body)
	}
	c.endTurn()
	testwait.WithinPolling(t, stepRecordsWait, 100*time.Millisecond, "the report to end "+r.key+"'s production check", func() bool {
		return !readProductionCheck(t, r.st.Pool(), r.records, r.key).Open()
	})
	if got := string(r.d.state().Issues[r.key].Phase); got != string(phase.Done) {
		t.Fatalf("%s stands in %s after its production check ended, want done", r.key, got)
	}
}
