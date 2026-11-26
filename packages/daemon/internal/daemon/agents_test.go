// The whole-daemon tests of one key across roles: the owner of a real issue gives the
// steps `implementer:a tester:a reviewer`, and steps 1 and 2 run in one agent and one session. Each
// runs the whole daemon over real PostgreSQL, NATS and Dispatch, a fake runtime and a fake GitHub
// (ownerStepsRig), and each pane follows its claim as Oh My Pi's plugin does: a relaunch of the
// claim on another role resumes the session the claim recorded (relaunchedAs).
package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// newAgentsRig is the owner's rig over one issue, its plan not given yet.
func newAgentsRig(t *testing.T) *ownerStepsRig {
	t.Helper()
	return newOwnerStepsRig(t, newLiveProofOn(t, workflowNATS(t)), startScratchDispatchOn(t, scratchDispatchOptions{}), 1, config.Project{})
}

// launchSpecsOf is the spec of every launch the runtime made of token's claim, in order.
func launchSpecsOf(rt *fake.Runtime, token claim.Token) []runtime.SpawnSpec {
	var specs []runtime.SpawnSpec
	for _, call := range rt.Calls() {
		if (call.Method == "Spawn" || call.Method == "Resume") && call.Spec.Claim == token {
			specs = append(specs, call.Spec)
		}
	}
	return specs
}

// sessionFile is the file c's session is recorded in, as its pane registered it (workerOf).
func sessionFile(c *stepClaimant) string { return "/sessions/" + c.session + ".jsonl" }

// relaunchedAs is c's pane coming up in the launch the daemon makes of its claim after before
// launches of it: the launch must run role and resume c's session. The earlier process's pane is
// gone, as a relaunch ends it, so its stream closes; the new pane says hello with the launch's boot
// token once the daemon has let the old stream go (helloAcked), registers in that same session and
// says ready, as the plugin does. The claimant then works role. It answers the launch's spec.
func (r *ownerStepsRig) relaunchedAs(c *stepClaimant, before int, role claim.Role) runtime.SpawnSpec {
	t := r.t
	t.Helper()
	testwait.WithinPolling(t, stepRecordsWait, 50*time.Millisecond, "the daemon's relaunch of "+string(c.token)+" as "+string(role), func() bool {
		return len(launchSpecsOf(r.rt, c.token)) > before
	})
	launch := lastLaunch(t, r.rt, c.token)
	if launch.Role != role || launch.ResumeSessionFile != sessionFile(c) {
		t.Fatalf("the daemon relaunched %s as %s resuming %q, want it as %s resuming %s, its own session", c.token, launch.Role,
			launch.ResumeSessionFile, role, sessionFile(c))
	}
	c.mu.Lock()
	old := c.sh
	c.mu.Unlock()
	_ = old.conn.Close()
	r.built.mu.Lock()
	address := r.built.address
	r.built.mu.Unlock()
	sh := helloAcked(t, address, launch.BootToken)
	status, body := r.d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: launch.BootToken, SessionID: c.session, OmpSessionFile: sessionFile(c), AgentID: c.session, PluginContract: api.DaemonAPIVersion(),
	}, false)
	if status != http.StatusOK {
		t.Fatalf("register %s in its session %s = %d %s, want 200", c.token, c.session, status, body)
	}
	var registered claim.RegisterResponse
	if err := json.Unmarshal(body, &registered); err != nil {
		t.Fatalf("decode the registration of %s: %v", c.token, err)
	}
	c.mu.Lock()
	c.sh, c.bootToken, c.secret, c.role, c.open, c.inTurn = sh, launch.BootToken, registered.Secret, role, nil, false
	c.mu.Unlock()
	d := r.d
	sh.serve(func() { c.serve(sh, d) })
	if status, body := r.d.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: c.token, SessionID: c.session, Secret: c.secret, Generation: launch.Generation,
	}, false); status != http.StatusNoContent {
		t.Fatalf("ready %s = %d %s, want 204", c.token, status, body)
	}
	return launch
}

// helloAcked is a pane's shim on the worker stream at address with bootToken, once the daemon
// acknowledges its hello: a hello the daemon refuses because the claim's earlier stream is still
// bound (its close not read yet) is said again on a new connection, until stepRecordsWait.
func helloAcked(t *testing.T, address, bootToken string) *shim {
	t.Helper()
	var refused string
	for deadline := time.Now().Add(stepRecordsWait); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		conn, err := net.Dial("unix", strings.TrimPrefix(address, "unix://"))
		if err != nil {
			t.Fatalf("dial the worker stream %s: %v", address, err)
		}
		s := &shim{t: t, conn: conn, lines: bufio.NewReader(conn), writer: shimwire.NewWriter(conn), stopping: make(chan struct{})}
		if err := s.writer.WriteFrame(shimwire.Hello2{BootToken: bootToken}); err != nil {
			t.Fatalf("send the hello: %v", err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		line, err := s.lines.ReadBytes('\n')
		if err == nil {
			if frame, err := shimwire.Decode(line); err == nil && frame.FrameType() == shimwire.TypeHelloAck {
				t.Cleanup(s.end)
				return s
			}
		}
		refused = fmt.Sprintf("%s %v", line, err)
		_ = conn.Close()
	}
	t.Fatalf("the daemon acknowledged no hello of the boot token within %s; last answer %s", stepRecordsWait, refused)
	return nil
}

// takeTask is c taking the daemon's next task, as take does, and answering the task's text.
func (c *stepClaimant) takeTask() string {
	c.r.t.Helper()
	prompt := c.receive()
	c.answer()
	c.send(shimwire.AgentStart{DeliveryID: prompt.DeliveryID})
	c.waitState(supervise.StateWorking)
	return prompt.Message
}

// implementCompletion is the implementer's completion of a round on the pull request's head.
var implementCompletion = api.HandoffCompleteRequest{Summary: "implemented", Commit: stepRecordsHead}

// The owner gives `implementer:a tester:a reviewer`: the agent of key a, `…-implementer`, works
// steps 1 and 2 in one session, and step 3 gets an agent and a session of its own. The tester's step
// is a relaunch of `…-implementer` with the role tester that resumes step 1's session file, and its
// task names the report of step 1 by the step's word, `implementer:a`. The step records say the
// same: steps 1 and 2 name one agent and one session, and step 3 names another of each.
//
// The control takes the key off step 2 (`implementer:a tester reviewer`): step 2 then gets an agent
// of its own, `…-tester`, in a session of its own, which the same reads must show.
func TestOneKeyIsOneSessionAcrossTwoRoles(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		steps []string
		keyed bool
	}{
		{name: "one key across the implementer's and the tester's steps", steps: oneKeyPlan, keyed: true},
		{name: "control: the key taken off step 2", steps: oneKeyControlPlan},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newAgentsRig(t)
			if err := oneKeyHolds(r, r.walkOneKey(tc.steps, tc.keyed), tc.keyed); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// oneKeyPlan is the plan of one key across two roles, and oneKeyControlPlan the same with the key taken off
// step 2.
var (
	oneKeyPlan        = []string{"implementer:a", "tester:a", "reviewer"}
	oneKeyControlPlan = []string{"implementer:a", "tester", "reviewer"}
)

// walkOneKey gives the walk's issue steps, three steps of the roles implementer, tester and
// reviewer, and walks it to done, keyed saying whether step 2 shares step 1's key: a keyed step 2
// and the retro are relaunches of `…-implementer` that resume its session (relaunchedAs), and step 2
// of a plan without the key is an agent of its own, `…-tester`, with `…-implementer` launched once
// for it. The tester's task must name step 1's report by its word. It answers the three steps'
// records once each is read ended.
func (r *ownerStepsRig) walkOneKey(steps []string, keyed bool) [3]stepRecordRead {
	t := r.t
	t.Helper()
	r.givePlan(steps...)
	r.openGate(phase.Implementing)
	r.github.open("legion/"+r.key, r.number)
	implementer := r.worker(claim.RoleImplementer)
	launched := len(launchSpecsOf(r.rt, implementer.token))
	r.work(claim.RoleImplementer, "implement", implementCompletion, phase.Testing)

	tester := implementer
	if keyed {
		r.relaunchedAs(implementer, launched, claim.RoleTester)
		launched++
	} else {
		tester = r.worker(claim.RoleTester)
		if launches := launchSpecsOf(r.rt, implementer.token); len(launches) != launched {
			t.Fatalf("%s was launched %d times, want once: step 2 is another agent's", implementer.token, len(launches))
		}
	}
	if task := tester.takeTask(); !strings.Contains(task, "Step 1 ("+steps[0]+") reported:") {
		t.Fatalf("the tester's task is %q, want it to name the report of step 1 (%s)", task, steps[0])
	}
	r.finish(tester, "test", testCompletion, phase.Reviewing)
	r.review("APPROVED", phase.Retro)
	if keyed {
		// The retro is the implementer's, whose agent is key a's: the claim takes its first role
		// again, in the same session.
		r.relaunchedAs(implementer, launched, claim.RoleImplementer)
	}
	r.retro()
	r.merge()

	var records [3]stepRecordRead
	r.waitSteps("the three steps' records", func(read stepRecordsRead) error {
		for place := range records {
			if records[place] = read.find("step", place+1, 1); records[place].EndedAt == nil {
				return fmt.Errorf("step %d has no ended record", place+1)
			}
		}
		return nil
	})
	return records
}

// oneKeyHolds says whether the records of the walk's three steps are one key's: step 1 is
// `…-implementer`'s, step 3 `…-reviewer`'s in a session of its own, and steps 1 and 2 name one
// agent and one session when keyed, and two agents and two sessions when not, step 2 then
// `…-tester`'s. The error names the first record that is not.
func oneKeyHolds(r *ownerStepsRig, steps [3]stepRecordRead, keyed bool) error {
	implementer, tester, reviewer := r.token(claim.RoleImplementer), r.token(claim.RoleTester), r.token(claim.RoleReviewer)
	if steps[0].Agent != string(implementer) {
		return fmt.Errorf("step 1 names agent %q, want %s", steps[0].Agent, implementer)
	}
	if steps[2].Agent != string(reviewer) || steps[2].Session == steps[0].Session {
		return fmt.Errorf("step 3 names agent %q and session %q, want %s in a session of its own (step 1's is %q)", steps[2].Agent,
			steps[2].Session, reviewer, steps[0].Session)
	}
	sameAgent, sameSession := steps[1].Agent == steps[0].Agent, steps[1].Session == steps[0].Session
	switch {
	case keyed && (!sameAgent || !sameSession):
		return fmt.Errorf("steps 1 and 2 name agents %q and %q, sessions %q and %q; want one agent and one session", steps[0].Agent,
			steps[1].Agent, steps[0].Session, steps[1].Session)
	case !keyed && (steps[1].Agent != string(tester) || sameSession):
		return fmt.Errorf("step 2 names agent %q and session %q; want %s in a session of its own (step 1's is %q)", steps[1].Agent,
			steps[1].Session, tester, steps[0].Session)
	}
	return nil
}

// A step keeps its agent when its task returns to it (R6). The plan `implementer:a tester:a
// reviewer` reaches step 3, whose reviewer requests changes, and the task returns to step 1 in the
// session of steps 1 and 2: `…-implementer` is relaunched with its first role, resuming that one
// session, and it takes the return's task. Each entry of the claim rises across its two rows
// (enter): the tester's entry stands above the implementer's, and the return's above the tester's,
// so no task the claim is handed falls at or below one it was handed before. The reviewer's row
// stays its own. The task then walks to done again in the same two agents.
func TestAKeyedAgentReturnsToItsFirstStepInItsSession(t *testing.T) {
	t.Parallel()
	r := newAgentsRig(t)
	r.givePlan("implementer:a", "tester:a", "reviewer")
	r.openGate(phase.Implementing)
	r.github.open("legion/"+r.key, r.number)
	implementer := r.worker(claim.RoleImplementer)
	launched := len(launchSpecsOf(r.rt, implementer.token))
	r.work(claim.RoleImplementer, "implement", implementCompletion, phase.Testing)
	r.relaunchedAs(implementer, launched, claim.RoleTester)
	launched++
	implementer.takeTask()
	r.entryRose(implementer, claim.RoleTester, claim.RoleImplementer)
	r.finish(implementer, "test", testCompletion, phase.Reviewing)
	reviewer := r.worker(claim.RoleReviewer)
	r.review("CHANGES_REQUESTED", phase.Implementing)

	r.relaunchedAs(implementer, launched, claim.RoleImplementer)
	launched++
	implementer.takeTask()
	if rows := r.phaseRowsNow(); rows[claim.RoleReviewer].claim != string(reviewer.token) {
		t.Fatalf("the reviewer's row names %q, want its own %s", rows[claim.RoleReviewer].claim, reviewer.token)
	}
	r.entryRose(implementer, claim.RoleImplementer, claim.RoleTester)

	r.finish(implementer, "implement", implementCompletion, phase.Testing)
	r.relaunchedAs(implementer, launched, claim.RoleTester)
	launched++
	implementer.takeTask()
	r.finish(implementer, "test", testCompletion, phase.Reviewing)
	r.review("APPROVED", phase.Retro)
	r.relaunchedAs(implementer, launched, claim.RoleImplementer)
	r.retro()
	r.merge()
	if spawns := slices.IndexFunc(launchSpecsOf(r.rt, implementer.token)[1:], func(s runtime.SpawnSpec) bool {
		return s.ResumeSessionFile != sessionFile(implementer)
	}); spawns >= 0 {
		t.Fatalf("a relaunch of %s started a fresh session, want each to resume %s", implementer.token, sessionFile(implementer))
	}
}

// phaseRowNow is what entryRose reads of one phases row: the claim it names and its entry.
type phaseRowNow struct {
	claim string
	entry int64
}

// phaseRowsNow is each phases row of the walk's issue, by role, as the store holds it now.
func (r *ownerStepsRig) phaseRowsNow() map[claim.Role]phaseRowNow {
	t := r.t
	t.Helper()
	rows := map[claim.Role]phaseRowNow{}
	read, err := r.st.Pool().Query(context.Background(), `select role, claim, entry from phases where issue = $1`, r.key)
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	for read.Next() {
		var role, token string
		var entry int64
		if err := read.Scan(&role, &token, &entry); err != nil {
			t.Fatal(err)
		}
		rows[claim.Role(role)] = phaseRowNow{token, entry}
	}
	return rows
}

// entryRose holds that the rows of entered and before both name c's claim, and that the entry of
// entered, the step c works now, stands above the entry of before, the claim's other row.
func (r *ownerStepsRig) entryRose(c *stepClaimant, entered, before claim.Role) {
	t := r.t
	t.Helper()
	rows := r.phaseRowsNow()
	if rows[entered].claim != string(c.token) || rows[before].claim != string(c.token) {
		t.Fatalf("the %s's and the %s's rows name %q and %q, want both %s, key a's agent", entered, before, rows[entered].claim,
			rows[before].claim, c.token)
	}
	if rows[entered].entry <= rows[before].entry {
		t.Fatalf("the %s's entry is %d and the %s's %d; want it above, risen across the claim's two rows", entered, rows[entered].entry,
			before, rows[before].entry)
	}
}

// An issue that ran under the release before keeps its claims (R20). The plan `implementer
// reviewer` reaches reviewing on this release; the daemon stops, and the store is taken back to the
// schema the release before leaves by the way backs its migrations' headers give (the name of each
// step's agent and the claim of each resume), so its plan rows name no agent, as that release wrote
// them. This release then boots and migrates the column again, which names each step for its role
// (20261124213820), and the issue reaches done with the claims `…-implementer` and `…-reviewer`
// of its steps and no other.
func TestAnIssueThatRanUnderTheReleaseBeforeKeepsItsClaims(t *testing.T) {
	t.Parallel()
	r := newAgentsRig(t)
	r.givePlan("implementer", "reviewer")
	r.openGate(phase.Implementing)
	r.github.open("legion/"+r.key, r.number)
	r.work(claim.RoleImplementer, "implement", implementCompletion, phase.Reviewing)
	r.worker(claim.RoleReviewer)
	r.restartWith(func() {
		for _, migration := range []string{"_pod_resume_claim.up.sql", planStepAgentMigration} {
			r.exec(strings.ReplaceAll(r.headerTransaction(migration), "<PROJECT>", r.cfg.Project))
		}
		var agent bool
		if err := r.st.Pool().QueryRow(context.Background(), `select exists (select 1 from information_schema.columns
			where table_name = 'plan_steps' and column_name = 'agent')`).Scan(&agent); err != nil || agent {
			t.Fatalf("plan_steps holds an agent column after the way back = %t, %v; want the schema the release before leaves", agent, err)
		}
	})
	r.review("APPROVED", phase.Retro)
	r.retro()
	r.merge()

	rows, err := r.st.Pool().Query(context.Background(), `select token from claims where issue = $1 order by token`, r.key)
	if err != nil {
		t.Fatal(err)
	}
	var tokens []string
	for rows.Next() {
		var token string
		if err := rows.Scan(&token); err != nil {
			t.Fatal(err)
		}
		tokens = append(tokens, token)
	}
	rows.Close()
	want := []string{string(r.token(claim.RoleArchitect)), string(r.token(claim.RoleImplementer)), string(r.token(claim.RoleMerger)),
		string(r.token(claim.RoleReviewer))}
	slices.Sort(want)
	if !slices.Equal(tokens, want) {
		t.Fatalf("%s holds the claims %v, want %v: each step's claim named by its role and no other", r.key, tokens, want)
	}
}
