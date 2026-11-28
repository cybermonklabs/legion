// The whole-daemon tests of the agents of an owner's plan: one key across roles, where the owner of
// a real issue gives the steps `implementer:a tester:a reviewer` and steps 1 and 2 run in one agent
// and one session; one role at two steps, `implementer:a reviewer implementer:a`, whose steps 1
// and 3 run in one agent and one session; and a second agent of one role, `implementer reviewer
// implementer`, whose step 3 runs in an agent and a session of its own. Each runs the whole daemon
// over real PostgreSQL, NATS and Dispatch, a fake runtime and a fake GitHub (ownerStepsRig), and
// each pane follows its claim as Oh My Pi's plugin does: a relaunch of the claim on another role
// resumes the session the claim recorded (relaunchedAs).
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
	"github.com/sjawhar/legion/daemon/internal/record"
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

// phaseRowsNow is the phases rows of the walk's issue, by role, as the store holds them now. It
// keeps one row of each role, so it reads only walks whose roles each stand at one step, as
// `implementer:a tester:a reviewer`'s do.
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
// schema the release before leaves by the way backs its migrations' headers give, newest first: the
// agent of a claim's session, the key of a phases row by its step, whose way back writes the key
// (issue, role) that release writes its phases rows by (docs/kubernetes.md, "Rolling back the
// release that keys a phases row by its step"), the claim of each backstop, the claim of each
// resume, whose way back also puts back one open resume per role, and the name of each step's
// agent, so its plan rows name no agent, as that release wrote them. This release then boots,
// migrates the key, the columns and the indexes again, the column naming each step for its role
// (20261124213820), and the issue reaches done with the claims `…-implementer` and `…-reviewer` of
// its steps and no other.
func TestAnIssueThatRanUnderTheReleaseBeforeKeepsItsClaims(t *testing.T) {
	t.Parallel()
	r := newAgentsRig(t)
	r.givePlan("implementer", "reviewer")
	r.openGate(phase.Implementing)
	r.github.open("legion/"+r.key, r.number)
	r.work(claim.RoleImplementer, "implement", implementCompletion, phase.Reviewing)
	r.worker(claim.RoleReviewer)
	r.restartWith(func() {
		for _, migration := range []string{"_claim_session_agent.up.sql", phaseStepKeyMigration, "_pod_wake_backstop_claim.up.sql",
			"_pod_resume_claim.up.sql", planStepAgentMigration} {
			r.exec(strings.ReplaceAll(r.headerTransaction(migration), "<PROJECT>", r.cfg.Project))
		}
		if key := r.phasesKey(); key != releaseBeforesPhasesKey {
			t.Fatalf("the key of phases after the way back is (%s), want (%s), the key the release before writes by", key, releaseBeforesPhasesKey)
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
	r.holdsClaimsOf(claim.RoleArchitect, claim.RoleImplementer, claim.RoleMerger, claim.RoleReviewer)
}

// releaseBeforesPhasesKey is the key of phases a release before the key of a phases row by its
// step writes its rows by, and thisReleasesPhasesKey the key this release writes them by.
const (
	releaseBeforesPhasesKey = "issue, role"
	thisReleasesPhasesKey   = "issue, role, step_task, step_place"
)

// phasesKey is the columns of the primary key of phases as the store holds it now, in the key's
// order, joined by ", ".
func (r *stepRecordsRig) phasesKey() string {
	r.t.Helper()
	var key string
	if err := r.st.Pool().QueryRow(context.Background(), `select coalesce(string_agg(a.attname, ', ' order by k.n), '') from pg_index i
		cross join lateral unnest(i.indkey) with ordinality k(attnum, n)
		join pg_attribute a on a.attrelid = i.indrelid and a.attnum = k.attnum
		where i.indrelid = 'phases'::regclass and i.indisprimary`).Scan(&key); err != nil {
		r.t.Fatalf("read the key of phases: %v", err)
	}
	return key
}

// holdsClaimsOf fails the test unless the walk's issue holds the claims of roles and no other,
// each claim named by its role.
func (r *stepRecordsRig) holdsClaimsOf(roles ...claim.Role) {
	t := r.t
	t.Helper()
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
	var want []string
	for _, role := range roles {
		want = append(want, string(r.token(role)))
	}
	slices.Sort(want)
	if !slices.Equal(tokens, want) {
		t.Fatalf("%s holds the claims %v, want %v: each step's claim named by its role and no other", r.key, tokens, want)
	}
}

// A role stands at two steps of a task, one agent doing both, and the task of the later step names
// the report of each step before it. The owner gives `implementer:a reviewer
// implementer:a`: the agent of key a, `…-implementer`, works steps 1 and 3, launched once and so in
// one session, and step 2 is `…-reviewer`'s, in a session of its own. The reviewer requests changes
// first, which returns the task to step 1 in that same session; its approval then moves the task to
// step 3, whose task names the latest report of step 1 and of step 2, each under its own step. The
// step records say the same: each run of steps 1 and 3 names one agent and one session, and each
// run of step 2 another of each.
//
// Two controls. The key taken off step 3 (`implementer:a reviewer implementer`) gives step 3 a
// second agent that starts with the role implementer, `…-implementer-2`, in a session of its own
// and not step 1's (walkSecondAgent). And key b across the reviewer's and the implementer's steps
// (`implementer:a reviewer:b implementer:b`, whose agent is named reviewer, its first role) gives
// step 3 the session of step 2, not of step 1: `…-reviewer` is relaunched with the role
// implementer, resuming its own session, which the same reads must show.
func TestARoleTwoTimesInATask(t *testing.T) {
	t.Parallel()
	t.Run("one key at steps 1 and 3, through a failed review", func(t *testing.T) {
		r := newAgentsRig(t)
		if err := roleTwiceHolds(r, r.walkRoleTwice()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("control: the key taken off step 3", func(t *testing.T) {
		r := newAgentsRig(t)
		if err := secondAgentHolds(r, r.walkSecondAgent(roleTwiceUnkeyedPlan)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("control: key b across the reviewer's and the implementer's steps", func(t *testing.T) {
		r := newAgentsRig(t)
		if err := keyBHolds(r, r.walkKeyB()); err != nil {
			t.Fatal(err)
		}
	})
}

// roleTwicePlan is the plan of one key at two steps of one role; roleTwiceUnkeyedPlan the same with
// the key taken off step 3, which gives step 3 a second agent that starts with the role
// implementer; and roleTwiceKeyBPlan the plan whose steps 2 and 3 are key b's, an agent that starts
// with the role reviewer and so is named reviewer.
var (
	roleTwicePlan        = []string{"implementer:a", "reviewer", "implementer:a"}
	roleTwiceUnkeyedPlan = []string{"implementer:a", "reviewer", "implementer"}
	roleTwiceKeyBPlan    = []string{"implementer:a", "reviewer:b", "implementer:b"}
)

// stepRun is one run of a step: the step's place and the run's attempt, as its record names them.
type stepRun struct{ step, attempt int }

// roleTwiceRuns is each run of the walk of roleTwicePlan (walkRoleTwice), in the order they ran.
var roleTwiceRuns = []stepRun{{1, 1}, {2, 1}, {1, 2}, {2, 2}, {3, 1}}

// walkRoleTwice gives the walk's issue roleTwicePlan and walks it to done: the implementer's
// completion; the reviewer's report and a person's review that requests changes, which returns the
// task to step 1, where `…-implementer` takes it; the implementer's completion again; the
// reviewer's report and an approving review, which moves the task to step 3, whose task
// `…-implementer` takes, naming the latest reports of steps 1 and 2; then the retro and the merge.
// `…-implementer` must be launched once, so each of its steps runs in its one session. It answers
// the record of each run of roleTwiceRuns, in that order, once each is read ended.
func (r *ownerStepsRig) walkRoleTwice() []stepRecordRead {
	t := r.t
	t.Helper()
	r.givePlan(roleTwicePlan...)
	r.openGate(phase.Implementing)
	r.github.open("legion/"+r.key, r.number)
	implementer := r.worker(claim.RoleImplementer)
	r.work(claim.RoleImplementer, "implement", implementCompletion, phase.Reviewing)
	r.reviewReported("CHANGES_REQUESTED", phase.Implementing)
	if task := implementer.takeTask(); !strings.Contains(task, " Step 1 of 3: implementer:a.") {
		t.Fatalf("the implementer's task after the failed review is %q, want the task of step 1", task)
	}
	r.finish(implementer, "implement", implementCompletion, phase.Reviewing)
	r.reviewReported("APPROVED", phase.Implementing)
	third := implementer.takeTask()
	for _, says := range []string{" Step 3 of 3: implementer:a.", " Step 1 (implementer:a) reported: implement version 2.",
		" Step 2 (reviewer) reported: review version 2."} {
		if !strings.Contains(third, says) {
			t.Fatalf("step 3's task is %q, want it to say %q", third, says)
		}
	}
	r.finish(implementer, "implement", implementCompletion, phase.Retro)
	r.retro()
	r.merge()
	if launches := launchSpecsOf(r.rt, implementer.token); len(launches) != 1 {
		t.Fatalf("%s was launched %d times, want once: steps 1 and 3 and the return to step 1 run in its one session", implementer.token, len(launches))
	}
	return r.runsRecorded(roleTwiceRuns)
}

// roleTwiceHolds says whether the records of roleTwiceRuns are one key's at steps 1 and 3: each run
// of steps 1 and 3 names `…-implementer` and the session of step 1's first run, and each run of
// step 2 names `…-reviewer` in another session. The error names the first record that does not.
func roleTwiceHolds(r *ownerStepsRig, records []stepRecordRead) error {
	implementer, reviewer := r.token(claim.RoleImplementer), r.token(claim.RoleReviewer)
	session := records[0].Session
	for i, run := range roleTwiceRuns {
		got := records[i]
		switch {
		case run.step != 2 && (got.Agent != string(implementer) || got.Session != session):
			return fmt.Errorf("run %d of step %d names agent %q and session %q, want %s in step 1's session %q", run.attempt, run.step, got.Agent,
				got.Session, implementer, session)
		case run.step == 2 && (got.Agent != string(reviewer) || got.Session == session):
			return fmt.Errorf("run %d of step 2 names agent %q and session %q, want %s in a session of its own (step 1's is %q)", run.attempt,
				got.Agent, got.Session, reviewer, session)
		}
	}
	return nil
}

// A step with no key gets a fresh agent, a second agent of a role among them. The owner gives
// `implementer reviewer implementer`: step 1 is `…-implementer`'s and step 2 `…-reviewer`'s, and
// step 3, whose role the agent of step 1 already holds as its name, gets an agent of its own named
// `…-implementer-2` (record.NameAgents), launched fresh in a session of its own while
// `…-implementer` is launched once. Step 3's task names the reports of steps 1 and 2, and the
// retro, which the agent of the task's last implementer's step runs, is `…-implementer-2`'s. The
// step records say the same: three agents, each in a session of its own.
func TestASecondAgentOfOneRoleRunsInASessionOfItsOwn(t *testing.T) {
	r := newAgentsRig(t)
	if err := secondAgentHolds(r, r.walkSecondAgent(secondAgentPlan)); err != nil {
		t.Fatal(err)
	}
}

// secondAgentPlan is the plan of two agents of the role implementer, no step keyed.
var secondAgentPlan = []string{"implementer", "reviewer", "implementer"}

// secondAgent is the name record.NameAgents gives the agent of step 3 of secondAgentPlan and of
// roleTwiceUnkeyedPlan: the role implementer is the name of the agent of step 1.
const secondAgent claim.Agent = "implementer-2"

// walkSecondAgent gives the walk's issue steps, `implementer reviewer implementer` with or without
// a key on step 1, and walks it to done: the implementer's completion; the reviewer's report and an
// approving review; step 3, the launch of `…-implementer-2`, the second agent, fresh, whose task
// names the reports of steps 1 and 2; the retro, which `…-implementer-2` runs as the agent of the
// task's last implementer's step; and the merge. `…-implementer` and `…-implementer-2` must each be
// launched once, and the second's launch must resume no session. It answers the records of steps 1
// to 3 once each is read ended.
func (r *ownerStepsRig) walkSecondAgent(steps []string) []stepRecordRead {
	t := r.t
	t.Helper()
	r.givePlan(steps...)
	r.openGate(phase.Implementing)
	r.github.open("legion/"+r.key, r.number)
	implementer := r.worker(claim.RoleImplementer)
	r.work(claim.RoleImplementer, "implement", implementCompletion, phase.Reviewing)
	r.reviewReported("APPROVED", phase.Implementing)
	second := r.agentWorker(secondAgent, claim.RoleImplementer)
	launches := launchSpecsOf(r.rt, second.token)
	if len(launches) != 1 || launches[0].Role != claim.RoleImplementer || launches[0].ResumeSessionFile != "" {
		t.Fatalf("the daemon launched %s as %+v, want one launch as implementer resuming no session", second.token, launches)
	}
	third := second.takeTask()
	for _, says := range []string{" Step 3 of 3: " + steps[2] + ".", " Step 1 (" + steps[0] + ") reported: implement version 1.",
		" Step 2 (" + steps[1] + ") reported: review version 1."} {
		if !strings.Contains(third, says) {
			t.Fatalf("step 3's task is %q, want it to say %q", third, says)
		}
	}
	r.finish(second, "implement", implementCompletion, phase.Retro)
	second.take()
	r.finish(second, "", r.retroCompletion(&record.PlanRating{Rating: 4, Reason: "The plan fit the work."}), phase.Merging)
	r.merge()
	for _, c := range []*stepClaimant{implementer, second} {
		if launches := launchSpecsOf(r.rt, c.token); len(launches) != 1 {
			t.Fatalf("%s was launched %d times, want once: steps 1 and 3 are two agents' and no step relaunches either", c.token, len(launches))
		}
	}
	return r.runsRecorded([]stepRun{{1, 1}, {2, 1}, {3, 1}})
}

// secondAgentHolds says whether the records of steps 1 to 3 of walkSecondAgent name three agents,
// each in a session of its own: step 1 `…-implementer`, step 2 `…-reviewer` and step 3
// `…-implementer-2`. The error names the first record that does not.
func secondAgentHolds(r *ownerStepsRig, records []stepRecordRead) error {
	second, err := claim.AgentToken(r.project, r.key, secondAgent)
	if err != nil {
		return err
	}
	want := []claim.Token{r.token(claim.RoleImplementer), r.token(claim.RoleReviewer), second}
	for place, got := range records {
		if got.Agent != string(want[place]) {
			return fmt.Errorf("step %d names agent %q, want %s", place+1, got.Agent, want[place])
		}
		for earlier := range place {
			if got.Session == records[earlier].Session {
				return fmt.Errorf("steps %d and %d name one session %q, want each step's agent in a session of its own", earlier+1, place+1, got.Session)
			}
		}
	}
	return nil
}

// walkKeyB gives the walk's issue roleTwiceKeyBPlan and walks it to done: the implementer's
// completion; the reviewer's report and an approving review; step 3, a relaunch of `…-reviewer`,
// the agent of key b, with the role implementer that resumes its own session, whose task names the
// reports of steps 1 and 2; the retro, which the agent of the task's last implementer's step runs,
// so `…-reviewer` too; and the merge. `…-implementer` must be launched once. It answers the records
// of steps 1 to 3 once each is read ended.
func (r *ownerStepsRig) walkKeyB() []stepRecordRead {
	t := r.t
	t.Helper()
	r.givePlan(roleTwiceKeyBPlan...)
	r.openGate(phase.Implementing)
	r.github.open("legion/"+r.key, r.number)
	implementer := r.worker(claim.RoleImplementer)
	r.work(claim.RoleImplementer, "implement", implementCompletion, phase.Reviewing)
	reviewer := r.worker(claim.RoleReviewer)
	launched := len(launchSpecsOf(r.rt, reviewer.token))
	r.reviewReported("APPROVED", phase.Implementing)
	r.relaunchedAs(reviewer, launched, claim.RoleImplementer)
	third := reviewer.takeTask()
	for _, says := range []string{" Step 3 of 3: implementer:b.", " Step 1 (implementer:a) reported: implement version 1.",
		" Step 2 (reviewer:b) reported: review version 1."} {
		if !strings.Contains(third, says) {
			t.Fatalf("step 3's task is %q, want it to say %q", third, says)
		}
	}
	r.finish(reviewer, "implement", implementCompletion, phase.Retro)
	reviewer.take()
	r.finish(reviewer, "", r.retroCompletion(&record.PlanRating{Rating: 4, Reason: "The plan fit the work."}), phase.Merging)
	r.merge()
	if launches := launchSpecsOf(r.rt, implementer.token); len(launches) != 1 {
		t.Fatalf("%s was launched %d times, want once: step 3 is key b's", implementer.token, len(launches))
	}
	return r.runsRecorded([]stepRun{{1, 1}, {2, 1}, {3, 1}})
}

// keyBHolds says whether the records of steps 1 to 3 of roleTwiceKeyBPlan give step 3 the agent and
// the session of step 2: step 1 names `…-implementer`, and steps 2 and 3 name `…-reviewer` in one
// session, which is not step 1's. The error names the first record that does not.
func keyBHolds(r *ownerStepsRig, records []stepRecordRead) error {
	implementer, reviewer := r.token(claim.RoleImplementer), r.token(claim.RoleReviewer)
	switch {
	case records[0].Agent != string(implementer):
		return fmt.Errorf("step 1 names agent %q, want %s", records[0].Agent, implementer)
	case records[1].Agent != string(reviewer) || records[1].Session == records[0].Session:
		return fmt.Errorf("step 2 names agent %q and session %q, want %s in a session of its own (step 1's is %q)", records[1].Agent,
			records[1].Session, reviewer, records[0].Session)
	case records[2].Agent != records[1].Agent || records[2].Session != records[1].Session:
		return fmt.Errorf("step 3 names agent %q and session %q, want step 2's, %q in %q, not step 1's session %q", records[2].Agent,
			records[2].Session, records[1].Agent, records[1].Session, records[0].Session)
	}
	return nil
}

// reviewReported is the reviewer taking its task, writing its handoff record of the word review and
// completing, as a reviewer reports its round, and then a person submitting state on the walk's pull
// request (publishReview); the issue then reaches next. The completion stands before the review, so
// the reviewer's row holds its report when the review moves the task on.
func (r *stepRecordsRig) reviewReported(state string, next phase.Phase) {
	t := r.t
	t.Helper()
	c := r.worker(claim.RoleReviewer)
	c.take()
	req := api.HandoffCompleteRequest{Summary: "reviewed"}
	req.HandoffID, req.HandoffVersion = r.writeHandoff(c.session, "review")
	if status, body := r.complete(c, req); status != http.StatusOK {
		t.Fatalf("the reviewer's completion = %d %s, want 200", status, body)
	}
	c.endTurn()
	r.publishReview(r.number, state)
	r.waitPhase(next)
}

// runsRecorded waits for the record of each run of runs on the walk's issue to read ended, and
// answers them in the order of runs.
func (r *stepRecordsRig) runsRecorded(runs []stepRun) []stepRecordRead {
	r.t.Helper()
	records := make([]stepRecordRead, len(runs))
	r.waitSteps("the records of the walk's runs", func(read stepRecordsRead) error {
		for i, run := range runs {
			if records[i] = read.find("step", run.step, run.attempt); records[i].EndedAt == nil {
				return fmt.Errorf("run %d of step %d has no ended record", run.attempt, run.step)
			}
		}
		return nil
	})
	return records
}

// An issue that ran before the key of a phases row by its step keeps its rows. The plan
// `implementer:a tester:a reviewer`, which the release before takes, reaches reviewing on this
// release; the daemon stops, and the store is taken back to the key that release writes its phases
// rows by, (issue, role), by the way back the migration's header gives, which keeps each role's one
// row with the step it ran, every column as it stood. This release then boots and migrates the key
// again, which keeps each of those rows whole, and the reviewer requests changes: the task returns
// to step 1 in the session of steps 1 and 2, on the implementer's row the release before left at
// step 1, which still names the record step 1's first run wrote, and walks to done again with the
// claims of its steps and no other.
func TestAnIssueThatRanBeforeTheStepKeyKeepsItsRows(t *testing.T) {
	t.Parallel()
	r := newAgentsRig(t)
	r.givePlan(oneKeyPlan...)
	r.openGate(phase.Implementing)
	r.github.open("legion/"+r.key, r.number)
	implementer := r.worker(claim.RoleImplementer)
	launched := len(launchSpecsOf(r.rt, implementer.token))
	r.work(claim.RoleImplementer, "implement", implementCompletion, phase.Testing)
	r.relaunchedAs(implementer, launched, claim.RoleTester)
	implementer.takeTask()
	r.finish(implementer, "test", testCompletion, phase.Reviewing)
	r.worker(claim.RoleReviewer)
	left := r.stepRowsNow()
	kept := r.carriedOf(claim.RoleImplementer, "a", 1)
	if kept.lastHandoff == "" {
		t.Fatalf("the implementer's row of step 1 names no handoff record before the way back, so no read after the return can tell it from a row written new")
	}
	r.restartWith(func() {
		r.rollBackStepKey()
		if key := r.phasesKey(); key != releaseBeforesPhasesKey {
			t.Fatalf("the key of phases after the way back is (%s), want (%s), the key the release before writes by", key, releaseBeforesPhasesKey)
		}
		if rows := r.stepRowsNow(); !slices.Equal(rows, left) {
			t.Fatalf("the way back left the rows %q, want each role's one row as it stood: %q", rows, left)
		}
	})
	if key := r.phasesKey(); key != thisReleasesPhasesKey {
		t.Fatalf("the key of phases after this release booted is (%s), want (%s): its Migrate wrote the key again", key, thisReleasesPhasesKey)
	}
	// The boot's Migrate keeps every row the release before left, every column as it stood: a row it
	// lost would be written again by the next write of its step, under the same key and claim, so
	// the walk below cannot see the loss.
	if rows := r.stepRowsNow(); !slices.Equal(rows, left) {
		gone, added := rowsApart(rows, left)
		t.Fatalf("after this release booted, want each row the release before left as it stood: %q is gone, and %q stands that it did not leave",
			gone, added)
	}
	// The booted daemon runs over a runtime of its own, and each pane outlived the daemon it was
	// launched by (reboot), so the launches of the claim count from what this runtime has made.
	launched = len(launchSpecsOf(r.rt, implementer.token))
	r.review("CHANGES_REQUESTED", phase.Implementing)
	r.relaunchedAs(implementer, launched, claim.RoleImplementer)
	launched++
	implementer.takeTask()
	if rows, want := r.stepRowsOf(claim.RoleImplementer), []string{"a 1 " + string(implementer.token)}; !slices.Equal(rows, want) {
		t.Fatalf("the implementer's rows after the return are %q, want %q: the row the release before left at step 1", rows, want)
	}
	// A row of step 1 written new, where the return found none, names no record and holds the
	// entry the claim's other rows give; the row the release before left still names the record of
	// step 1's first run, which no start clears, and the return raised its entry.
	if now := r.carriedOf(claim.RoleImplementer, "a", 1); now.lastHandoff != kept.lastHandoff || now.entry <= kept.entry {
		t.Fatalf("the implementer's row of step 1 after the return names the record %q at entry %d, want the row the release before left, "+
			"raised: the record %q and an entry above %d", now.lastHandoff, now.entry, kept.lastHandoff, kept.entry)
	}
	r.entryRose(implementer, claim.RoleImplementer, claim.RoleTester)
	r.finish(implementer, "implement", implementCompletion, phase.Testing)
	r.relaunchedAs(implementer, launched, claim.RoleTester)
	launched++
	if task := implementer.takeTask(); !strings.Contains(task, " Step 1 (implementer:a) reported: implement version 2.") {
		t.Fatalf("the tester's task after the return is %q, want it to name step 1's new report", task)
	}
	r.finish(implementer, "test", testCompletion, phase.Reviewing)
	r.review("APPROVED", phase.Retro)
	r.relaunchedAs(implementer, launched, claim.RoleImplementer)
	r.retro()
	r.merge()
	r.holdsClaimsOf(claim.RoleArchitect, claim.RoleImplementer, claim.RoleMerger, claim.RoleReviewer)
}

// stepRowsNow is each phases row of the walk's issue, `<role> <step task> <step place> <claim>
// <every other column>`, the last one JSON object, in the order of the role, the task and the
// place. It reads every column, so a row written again under its key and claim reads apart from
// the row it stands for: a run of a step writes its entry, the moment it raised it and the record
// it last named on its row, and a row written new holds none of the ones the lost row held.
func (r *stepRecordsRig) stepRowsNow() []string {
	r.t.Helper()
	return r.storeRows(fmt.Sprintf(`select concat_ws(' ', role, step_task, step_place, claim,
		to_jsonb(p) - array['issue', 'role', 'step_task', 'step_place', 'claim']) from phases p where issue = '%s'
		order by role, step_task, step_place`, r.key))
}

// rowsApart is each row of want that rows lacks, and each row of rows that want lacks.
func rowsApart(rows, want []string) (gone, added []string) {
	for _, row := range want {
		if !slices.Contains(rows, row) {
			gone = append(gone, row)
		}
	}
	for _, row := range rows {
		if !slices.Contains(want, row) {
			added = append(added, row)
		}
	}
	return gone, added
}

// carriedRow is what a phases row carries from one entry of its step to the next: the handoff
// record its role last named (record.PhaseRow.LastHandoff), which no start clears, and its entry,
// which each start raises.
type carriedRow struct {
	lastHandoff string
	entry       int64
}

// carriedOf is what role's row of step place of task carries on the walk's issue now.
func (r *stepRecordsRig) carriedOf(role claim.Role, task string, place int) carriedRow {
	r.t.Helper()
	var row carriedRow
	if err := r.st.Pool().QueryRow(context.Background(), `select last_handoff, entry from phases where issue = $1 and role = $2
		and step_task = $3 and step_place = $4`, r.key, string(role), task, place).Scan(&row.lastHandoff, &row.entry); err != nil {
		r.t.Fatalf("read the %s's row of step %d of task %s on %s: %v", role, place, task, r.key, err)
	}
	return row
}

// stepRowsOf is each phases row of role on the walk's issue, `<step task> <step place> <claim>`, in
// the order of the task and the place.
func (r *stepRecordsRig) stepRowsOf(role claim.Role) []string {
	r.t.Helper()
	return r.storeRows(fmt.Sprintf(`select concat_ws(' ', step_task, step_place, claim) from phases where issue = '%s' and role = '%s'
		order by step_task, step_place`, r.key, role))
}
