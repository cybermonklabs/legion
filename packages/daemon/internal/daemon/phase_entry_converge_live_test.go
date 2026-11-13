//go:build e2e

// The live proof that the work of a phase entry reaches the worker that entry names, on a running
// daemon (dispatch://INFRA-2800's R3, R4, R5, R6 and R9). scripts/e2e/phase-entry-converge.sh
// builds this driver with `-tags e2e` and owns the nats-server and the PostgreSQL it runs against.
//
// The daemon is the whole daemon: run, the function `legion start` calls, through this package's
// own whole-daemon harness (startDaemonWithin), so the proof exercises the real configuration, the
// real store and its migrations, the real HTTP API, the real claim supervision and its worker
// stream, the real openWorkflow, the real JetStream consumers, the real outbox lanes and the real
// entry sweep. Facts enter only as production's do: GitHub envelopes on the ENVOY_NOTIFICATIONS
// stream for the pushes onto the base branch that arm each conflict, the implementer's own
// pushes and the synchronizes; the daemon's own HTTP routes for each role's registration,
// readiness, grants and completions; and the daemon's worker stream for each role's turns,
// which this driver speaks as `legion worker-shim` does (shimwire frames).
// Each check prints `== <name>`, what it observed and `ok <name>`; the first that fails ends the
// run non-zero, naming it.
//
// Run one, in two checks. The configuration holds the daemon to six wakes a minute in bursts of
// one (convergeWakeRate, convergeWakeBurst), and the boot's relaunch of the implementer spends the
// burst, so the daemon's wake pacing has not yet let the next role's launch run and that role's
// start waits for it. The implementer's completion hands testing to the tester, whose phases row
// is raised to entry 1 with that entry's own task, and whose claim is queued that task while its
// launch waits. The issue's phase then changes twice while the start waits: the head starts
// conflicting with its base, which sends the issue back to implementing, and the implementer's
// merge forward at a second head hands testing out again, which raises the tester's row to entry
// 2 and writes that entry's
// own task. The daemon then lets the launch run: the tester comes up on its own worker-stream
// connection and is sent the task it was queued, entry 1's, because the claim held it throughout
// (supervise's queue refuses a second delivery while one is pending); the completion the tester
// reports for it is refused 409 HANDOFF_STALE_ENTRY naming both entries and recording
// nothing, and the entry the phase is on then reaches the same connection as a task of its own,
// whose completion the daemon takes. That is the whole of R5 on a running daemon, and the entry on
// each delivery is what tells the two tasks apart (record.PhaseRow.Entry,
// supervise.Claim.GivenEntry, both on GET /legion/v1/state as `phaseEntry` and `givenEntry`).
//
// Run two. The same rig deletes a start row from the outbox before any lane runs it: the driver
// holds the advisory lock a lane takes before it claims a lifecycle row of the issue
// (record.Postgres.ClaimRow's `legion.outbox.lifecycle:<issue>`), drives the conflicting head
// that raises the implementer's entry and writes that entry's start row, deletes that row, and
// lets the lock
// go, so the row's work exists only in the record. With no fact of the project arriving after it,
// the daemon's own entry sweep (intake.EntrySweep, workflowRuntime.tickEntrySweep) sends the
// entry's own task to the implementer's live connection. The sweep's period is a Go constant with
// a test override (the overrides' entrySweep, convergeEntrySweep here); the shipped binary keeps
// entrySweepInterval's minute, which is why the driver sets the override rather than waiting one.
//
// One more read. The driver reads GET /legion/v1/state once and holds that the issue carries both
// numbers on the wire, which is R6 on a booted daemon.
//
// What stands in lies outside the rule. The broker is a native nats-server the script starts and
// names in LEGION_E2E_NATS_URL. GitHub is convergeGitHub, a REST stand-in for the one pull
// request the proof drives, which serves that pull request's state, head, base and mergeable —
// the answer the driver arms to drive a conflict — and no branch rule of any kind. Dispatch is
// the board the package's whole-daemon live proofs share (liveDispatch, in live_proof_test.go).
// The two Apps' tokens are workflowTokenRecorder's placeholders. No agent runs: every claim is
// launched by the package's fake runtime, which says it provisions each workspace in the
// claim's own pod, as the Agent Sandbox runtime does, so the daemon clones nothing from GitHub.
// The issue, its pull request, its slot and the implementer's claim are seeded through the
// store, as every daemon test seeds its fixture; what is driven is the change.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// convergeBase is the pull request's base branch, the repository's default: a head that
// conflicts with it is the one answer of a pull request that still moves an issue (workflow's
// mergeability), and this proof's two phase changes back to implementing are that answer.
const convergeBase = "main"

// The two heads the proof drives: the implementer's seeded head, which run one's conflict stands
// at, and the merge forward it pushes for the second testing entry, which run two's conflict
// stands at.
const (
	convergeFirstHead  = "pe1"
	convergeSecondHead = "pe2"
)

// The pace the daemon is held to, set on the configuration as daemon_test.go sets it
// (config.Config.WorkerWakesPerMinute, WorkerWakeBurst): six wakes a minute in bursts of one, so
// one wake costs ten seconds. The boot's relaunch of the seeded implementer spends the burst, so
// the launch of the next role the daemon starts cannot run until that interval has passed, and
// run one drives the issue's two phase changes inside it, which it does in well under a second;
// the check holds that the launch is still waiting when they are done, so a pace this proof
// outran would fail it rather than pass quietly.
const (
	convergeWakeRate  = 6
	convergeWakeBurst = 1
)

// convergeEntrySweep is the period this driver gives the daemon's entry sweep (the overrides'
// entrySweep, read by workflowRuntime.tickEntrySweep) in place of the minute the shipped binary
// keeps (entrySweepInterval): run two waits out one sweep, and the minute would be most of this
// proof's time. Nothing else of the sweep is changed.
const convergeEntrySweep = 2 * time.Second

// convergeOwedBound is how long a step waits for a task the daemon sends by itself. It outlasts
// the longest wait either run can owe: the outbox's own retry backoff, which doubles to a minute
// and is capped there (retryBackoff), for the start row run one leaves waiting behind the task the
// claim already holds, and one sweep period for run two.
const convergeOwedBound = 2 * time.Minute

// phaseEntryConvergeLive is one run of the proof: the booted daemon, the one issue it drives, the
// GitHub stand-in its facts are read against, and the two roles the driver acts as over the worker
// stream.
type phaseEntryConvergeLive struct {
	liveProof
	rt     *fake.Runtime
	built  *built
	github *convergeGitHub
	// key is the issue, its own tree, and number its pull request's — the run's own, since the
	// store a pod re-runs the proof against holds every earlier run's pull requests and a fact
	// names a pull request by its repository and number.
	key    string
	number int
	// imp, test and rev are the implementer, the tester and the reviewer as their agents reach
	// the daemon.
	imp, test, rev *convergeClaimant
	// seq numbers the base pushes armConflict publishes, so each is its own envelope with its
	// own before and after commits rather than a second report of the one before it.
	seq int64
}

// TestAPhaseEntryConvergesOnARunningDaemonLive drives the proof's four checks against one booted
// daemon, in the order each one's state follows from the last: the phase changing twice while the
// tester's start waits for the launch the pacing holds, the launch running and the entry the phase
// is on reaching the tester after the entry it had been queued is refused, a start row deleted
// before any lane runs it whose work the sweep sends all the same, and the state's two numbers.
func TestAPhaseEntryConvergesOnARunningDaemonLive(t *testing.T) {
	fixtureenv.Live(t)
	r := newPhaseEntryConvergeLive(t)
	r.step("run1-the-phase-changes-twice-while-the-testers-start-waits-for-its-launch", r.phaseChangesWhileTheStartWaits)
	r.step("run1-the-launch-runs-and-the-entry-the-phase-is-on-is-worked", r.theLaunchRunsAndTheEntryIsWorked)
	r.step("run2-a-start-row-deleted-before-any-lane-ran-it-loses-no-work", r.aDeletedStartRowLosesNoWork)
	r.step("the-state-carries-the-phase-entry-and-the-entry-the-role-was-given", r.stateCarriesBothNumbers)
}

// newPhaseEntryConvergeLive boots the daemon over the script's broker with the issue already
// recorded: one root implementing on an open pull request at its first head, holding its
// tree's admission slot, and its implementer's claim as a launch a previous daemon began and never
// recorded, which this boot relaunches. That relaunch is how the proof reaches a live implementer
// session without an operator's spawn, and it is the wake that spends the burst.
func newPhaseEntryConvergeLive(t *testing.T) *phaseEntryConvergeLive {
	t.Helper()
	proof := newLiveProof(t, "scripts/e2e/phase-entry-converge.sh")
	number, err := strconv.ParseInt(randomSuffix(t)[:6], 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	r := &phaseEntryConvergeLive{
		liveProof: proof, rt: fake.NewRuntime(), built: &built{},
		key: proof.cfg.Project + "-1", number: int(number) + 1,
	}
	r.github = newConvergeGitHub(t, r.key, r.number)
	r.seed()
	// The runtime provisions each claim's workspace itself (fake.Runtime's InPod), so a start
	// reaches the fake's launch without the daemon cloning acme/widgets from github.com, which the
	// tmux answer would have it try.
	r.rt.InPod = true
	o := withTaskValues(fakeRuntime(r.rt, r.built))
	o.workflowTokens = &workflowTokenRecorder{}
	o.githubAPI = r.github.url
	// The wall clock, in place of this package's still one (daemon.go's RealClock for a nil
	// override): the launch the pacing holds runs when its timer fires, and a wake held on a
	// clock whose timers never fire would never take its turn.
	o.clock = nil
	o.entrySweep = convergeEntrySweep
	r.cfg.WorkerWakesPerMinute, r.cfg.WorkerWakeBurst = convergeWakeRate, convergeWakeBurst
	r.d = startDaemonWithin(t, r.cfg, o, r.log(), 60*time.Second)
	return r
}

// seed records the issue as the daemon would hold it at the moment its implementer began work: a
// root handed to Legion, implementing, holding its tree's admission slot, its lifecycle open under
// the workflow, on an open pull request at its first head, and its implementer's claim
// mid-launch, serving run 1. No phases row is written: every entry this proof reads is raised
// live, by the transitions the completions and the conflicting heads make.
func (r *phaseEntryConvergeLive) seed() {
	t := r.t
	t.Helper()
	ctx := context.Background()
	r.board.show(r.key, "in_progress")
	r.write("the issue, its pull request and its slot", func(tx pgx.Tx) error {
		if err := r.records.PutIssue(ctx, tx, record.Issue{
			Key: r.key, Repo: liveRepo, DefaultBranch: convergeBase, GitHubRepo: liveRepo, Tree: r.key, Project: r.cfg.Project,
			Title: "the issue whose phase entries converge", Phase: phase.Implementing, Generation: 1, Status: "in_progress",
			DispatchStatus: "in_progress", Rank: "U", LastDispatchSeq: 1, HandedOver: true,
		}); err != nil {
			return err
		}
		if err := r.records.PutPullRequest(ctx, tx, record.PullRequest{
			State: record.PullRequestOpen, Issue: r.key, Repo: liveRepo, Number: r.number, Branch: "legion/" + r.key,
			HeadSHA: convergeFirstHead, Base: convergeBase, Failing: []string{},
		}); err != nil {
			return err
		}
		return r.admit(ctx, tx, r.key)
	})
	if _, err := r.st.OpenTreeLifecycle(ctx, r.project, r.key, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("open the lifecycle of %s: %v", r.key, err)
	}
	implementer := r.token(claim.RoleImplementer)
	if _, err := r.st.AdmitClaim(ctx, supervise.Claim{
		Token: implementer, Project: r.project, Tree: r.key, Issue: r.key, Role: claim.RoleImplementer,
		Generation: 1, State: supervise.StateLaunching, BootTokenHash: supervise.HashBootToken("interrupted-" + randomSuffix(t)),
		ServingGeneration: 1,
	}); err != nil {
		t.Fatalf("record the implementer's unfinished launch: %v", err)
	}
	// Every role the proof launches needs its prompt on disk, as the daemon's own launch writes it.
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer} {
		writePrompt(t, r.cfg, r.token(role))
	}
	r.note("seeded: %s implementing on pull request #%d at %s, base %s, its implementer mid-launch serving run 1",
		r.key, r.number, convergeFirstHead, convergeBase)
}

// write runs body in one transaction against the daemon's own database, failing the test with what
// it was seeding on an error.
func (r *phaseEntryConvergeLive) write(what string, body func(tx pgx.Tx) error) {
	r.t.Helper()
	if err := r.st.Tx(context.Background(), body); err != nil {
		r.t.Fatalf("seed %s: %v", what, err)
	}
}

// token is the claim token of role on the proof's issue, as the daemon spells it.
func (r *phaseEntryConvergeLive) token(role claim.Role) claim.Token {
	r.t.Helper()
	token, err := claim.NewToken(r.project, r.key, role)
	if err != nil {
		r.t.Fatal(err)
	}
	return token
}

// awaitLaunch waits for the running daemon to launch token's claim and returns that launch.
func (r *phaseEntryConvergeLive) awaitLaunch(token claim.Token) runtime.SpawnSpec {
	r.t.Helper()
	testwait.Eventually(r.t, "the launch of "+string(token), func() bool { return r.launched(token) })
	return lastLaunch(r.t, r.rt, token)
}

// launched is whether the fake runtime has been asked to start a process for token.
func (r *phaseEntryConvergeLive) launched(token claim.Token) bool {
	for _, call := range r.rt.Calls() {
		if (call.Method == "Spawn" || call.Method == "Resume") && call.Spec.Claim == token {
			return true
		}
	}
	return false
}

// waitFor polls the state until want is true of the issue, and returns what it last saw.
func (r *phaseEntryConvergeLive) waitFor(what string, want func(api.Issue) bool) api.Issue {
	r.t.Helper()
	var last api.Issue
	testwait.Eventually(r.t, what, func() bool {
		last = r.issue(r.key)
		return want(last)
	})
	return last
}

// waitingLaunch is the wake the daemon's pacing holds for token, as the state publishes it
// (api.State.WakesWaiting): the launch it has not let run yet, and the moment it is due. False is
// no such wake, which is a launch that has already run or one never raised.
func (r *phaseEntryConvergeLive) waitingLaunch(token claim.Token) (api.WakeWaiting, bool) {
	r.t.Helper()
	for _, waiting := range r.d.state().WakesWaiting {
		if waiting.Claim == token && waiting.Kind == "launch" {
			return waiting, true
		}
	}
	return api.WakeWaiting{}, false
}

// convergeStart is one supervise start row of the proof's issue as the outbox holds it: its id,
// the role it starts, the phase entry its task is of (record.SuperviseRequest.Entry), how many
// attempts it has made and the error the last one left. No route shows the table.
type convergeStart struct {
	id       int64
	role     claim.Role
	entry    int64
	attempts int
	lastErr  string
}

// startRows is every supervise start row of the proof's issue, oldest first.
func (r *phaseEntryConvergeLive) startRows() []convergeStart {
	r.t.Helper()
	rows, err := r.st.Pool().Query(context.Background(),
		`select id, coalesce(payload->>'role', ''), coalesce((payload->>'entry')::bigint, 0), attempts, coalesce(last_error, '')
			from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = 'start' order by id`, r.key)
	if err != nil {
		r.t.Fatalf("list the start rows of %s: %v", r.key, err)
	}
	defer rows.Close()
	var starts []convergeStart
	for rows.Next() {
		var start convergeStart
		var role string
		if err := rows.Scan(&start.id, &role, &start.entry, &start.attempts, &start.lastErr); err != nil {
			r.t.Fatalf("read a start row of %s: %v", r.key, err)
		}
		start.role = claim.Role(role)
		starts = append(starts, start)
	}
	if err := rows.Err(); err != nil {
		r.t.Fatalf("list the start rows of %s: %v", r.key, err)
	}
	return starts
}

// entries is the issue's phase entry and the entry the role of its phase was given, as GET
// /legion/v1/state carries them (api.Issue.PhaseEntry, GivenEntry).
func (r *phaseEntryConvergeLive) entries() (phaseEntry, givenEntry int64) {
	r.t.Helper()
	issue := r.issue(r.key)
	return issue.PhaseEntry, issue.GivenEntry
}

// convergeClaimant is one role's claim as its agent reaches it: the shim that dialled the worker
// stream with the boot token of the launch the fake runtime recorded, the registration that
// exchanged it for the session's capability secret, a reader that answers what the daemon asks of
// the shim, and the plugin that takes each task as its value comes and starts the turn that works
// it, handing the task to the step (takeDriver.watch).
type convergeClaimant struct {
	r       *phaseEntryConvergeLive
	token   claim.Token
	role    claim.Role
	session string
	secret  string
	sh      *shim
	worker  *takeDriver
	tasks   chan takenTask
}

// convergeTask is a task the claim's worker took: the number and the text of the take, and the
// id of the delivery it is of, which names the outbox row that queued it (supervise.Delivery.ID,
// as the operator's claim listing shows the task the claim holds), "" for one the claim no longer
// holds when it is read.
type convergeTask struct {
	takenTask
	DeliveryID string
}

// claimant brings the launched claim of role to ready, as its pane's shim and its agent do: it
// waits for the launch, dials the worker stream with the launch's boot token, registers the
// session, serves the connection and reports the claim ready.
func (r *phaseEntryConvergeLive) claimant(role claim.Role, session string) *convergeClaimant {
	t := r.t
	t.Helper()
	token := r.token(role)
	launch := r.awaitLaunch(token)
	r.built.mu.Lock()
	address := r.built.address
	r.built.mu.Unlock()
	c := &convergeClaimant{r: r, token: token, role: role, session: session,
		sh: dialShim(t, address, launch.BootToken), tasks: make(chan takenTask, 8)}
	status, body := r.d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: launch.BootToken, SessionID: session, OmpSessionFile: "/sessions/" + session + ".jsonl",
		AgentID: session, PluginContract: api.DaemonAPIVersion,
	}, false)
	if status != http.StatusOK {
		t.Fatalf("register %s = %d %s, want 200", token, status, body)
	}
	var registered claim.RegisterResponse
	if err := json.Unmarshal(body, &registered); err != nil {
		t.Fatalf("decode the registration of %s: %v", token, err)
	}
	c.secret = registered.Secret
	c.worker = r.d.takeDriver(c.sh, token, session, registered.Secret, launch.Generation)
	go c.serve()
	c.worker.watch(t, func(task takenTask) { c.tasks <- task })
	if status, body := r.d.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: token, SessionID: session, Secret: registered.Secret, Generation: launch.Generation,
	}, false); status != http.StatusNoContent {
		t.Fatalf("ready %s = %d %s, want 204", token, status, body)
	}
	return c
}

// serve reads every frame the daemon sends the shim until the connection closes: it answers the
// protocol negotiation and the state probe (idle, or streaming while a turn runs). It runs for the
// claim's whole life, because a step that left the connection unread would have the daemon's own
// requests time out.
func (c *convergeClaimant) serve() {
	// The reader dialShim read the hello_ack through, which may already hold the frames after it.
	reader := c.sh.lines
	_ = c.sh.conn.SetReadDeadline(time.Time{})
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		frame, err := shimwire.Decode(line)
		if err != nil {
			continue
		}
		switch f := frame.(type) {
		case shimwire.NegotiateProtocol:
			c.send(shimwire.Response{ID: f.ID, Command: shimwire.TypeNegotiateProtocol, Success: true})
		case shimwire.GetState:
			streaming := c.worker.inATurn()
			data, _ := json.Marshal(shimwire.StateData{IsStreaming: &streaming})
			c.send(shimwire.Response{ID: f.ID, Command: shimwire.TypeGetState, Success: true, Data: data})
		}
	}
}

// send writes one frame to the daemon.
func (c *convergeClaimant) send(frame shimwire.Frame) {
	if err := c.sh.writer.WriteFrame(frame); err != nil {
		c.r.t.Errorf("send %s for %s: %v", frame.FrameType(), c.token, err)
	}
}

// startOwnTurn is the agent beginning a turn of its own, on no task it took: the turn the seeded
// implementer is in when the proof reaches it. It returns once the daemon has recorded the claim
// working.
func (c *convergeClaimant) startOwnTurn() {
	t := c.r.t
	t.Helper()
	c.worker.mustStartTurn(t)
	testwait.Eventually(t, fmt.Sprintf("the %s's turn running", c.role), func() bool {
		return c.r.d.claim(c.token).State == string(supervise.StateWorking)
	})
}

// endTurn is the agent finishing its turn: the agent_end, and behind it the turn's report naming
// the task the turn ran, which finishes that task. It returns once the daemon has recorded the
// claim out of its turn.
func (c *convergeClaimant) endTurn() {
	t := c.r.t
	t.Helper()
	c.worker.mustEndTurn(t)
	testwait.Eventually(t, fmt.Sprintf("the %s out of its turn", c.role), func() bool {
		return c.r.d.claim(c.token).State == string(supervise.StateIdle)
	})
}

// awaitTask waits for the next task this claim's worker takes, within bound, and returns it with
// the delivery it is of.
func (c *convergeClaimant) awaitTask(what string, bound time.Duration) convergeTask {
	t := c.r.t
	t.Helper()
	select {
	case task := <-c.tasks:
		return c.ofDelivery(task)
	case <-time.After(bound):
		t.Fatalf("the %s took no task within %s: %s", c.role, bound, what)
		return convergeTask{}
	}
}

// ofDelivery is task with the id of the delivery the claim holds it as: the take's turn runs, so
// the claim still holds the task it took until that turn's report.
func (c *convergeClaimant) ofDelivery(task takenTask) convergeTask {
	c.r.t.Helper()
	held := convergeTask{takenTask: task}
	if pending := c.r.d.claim(c.token).Pending; pending != nil {
		held.DeliveryID = pending.ID
	}
	return held
}

// drainTasks takes every task the worker has already taken and the steps have not read, oldest
// first, and returns them. It never waits: a task still on its way is left for the next
// awaitTask, so a step drains only where no task of the claim can be on its way, as run two does
// while it holds the lane's lock (aDeletedStartRowLosesNoWork).
func (c *convergeClaimant) drainTasks() []convergeTask {
	var drained []convergeTask
	for {
		select {
		case task := <-c.tasks:
			drained = append(drained, c.ofDelivery(task))
		default:
			return drained
		}
	}
}

// grant is a capability grant minted for this session now, as its `legion` tool mints one per
// operation.
func (c *convergeClaimant) grant() string {
	t := c.r.t
	t.Helper()
	status, body := c.r.d.request(http.MethodPost, "/legion/v1/grants",
		api.GrantRequest{SessionID: c.session, Secret: c.secret, Tree: c.r.key, Issue: c.r.key}, false)
	if status != http.StatusOK {
		t.Fatalf("the grant of %s = %d %s, want 200", c.token, status, body)
	}
	var granted api.GrantResponse
	if err := json.Unmarshal(body, &granted); err != nil {
		t.Fatalf("decode the grant of %s: %v", c.token, err)
	}
	return granted.GrantID
}

// handoff seeds on the run's Dispatch board version 1 of this claim's record of phase word,
// written by this claim's own registered session, as `dispatch handoff-write` leaves it, and
// answers its id. A completion of a file-backed phase names a record, and the daemon reads it
// back from Dispatch and refuses one another session wrote (api/handoff.go's handoffRecord), so
// every completion below writes its own first, as a worker does.
func (c *convergeClaimant) handoff(word string) string {
	return c.r.board.handoff(c.r.key, word, c.session)
}

// complete reports this role's completion of the phase it works, from the turn it is in, naming a
// handoff record of that phase this claim wrote, and returns the status and the refusal the daemon
// answered. Every record the board seeds is version 1 of its own id, so each completion names a
// record of its own and no two are the same one.
func (c *convergeClaimant) complete(summary, commit, verdict, handoffID string) (int, api.Failure) {
	t := c.r.t
	t.Helper()
	status, body := c.r.d.request(http.MethodPost, "/legion/v1/handoff/complete",
		api.HandoffCompleteRequest{GrantID: c.grant(), Summary: summary, Commit: commit, Verdict: verdict,
			HandoffID: handoffID, HandoffVersion: 1}, false)
	if status == http.StatusOK {
		return status, api.Failure{}
	}
	var failure api.Failure
	if err := json.Unmarshal(body, &failure); err != nil {
		t.Fatalf("decode the %s's refused completion %s: %v", c.role, body, err)
	}
	return status, failure
}

// Check 1 of run one. The seeded implementer completes implementing, which hands testing to the
// tester: the transition raises the tester's phases row to entry 1 and writes that entry's own
// task in its own transaction (workflow.Engine.enter), and the start it writes creates the
// tester's claim and queues it that task. The launch of that claim does not run: the boot's
// relaunch of the implementer spent the burst of the pace this daemon is held to, so the daemon's
// wake pacing has not let this one run yet and the start waits for it, which the state names
// (api.State.WakesWaiting). The issue's phase then changes twice inside that wait — the daemon's
// read of the pull request answers the first head as conflicting with its base, which sends the
// issue back to implementing, and the implementer's merge forward at a second head hands
// testing out again — which raises the tester's row to entry 2 with that entry's own task, while
// the claim still holds the task of entry 1 and the start row carrying entry 2 waits behind it.
func (r *phaseEntryConvergeLive) phaseChangesWhileTheStartWaits() {
	t := r.t
	tester := r.token(claim.RoleTester)
	r.imp = r.claimant(claim.RoleImplementer, "ses_implementer")
	r.note("the implementer is registered and ready on the launch this boot made for the claim the fixture left mid-launch; that launch spent the burst of %d wake(s) a minute in bursts of %d",
		convergeWakeRate, convergeWakeBurst)
	// The conflict is armed before this completion, and the read that completion queues is held
	// at the stand-in while this check reads the state the completion left (armConflict,
	// convergeGitHub.pause): the read is where an issue in testing learns its head conflicts,
	// and held, it cannot land between the completion and the reads below it.
	releaseRead := r.github.pause()
	defer releaseRead()
	r.armConflict()
	r.imp.startOwnTurn()
	if status, failure := r.imp.complete("implemented it", convergeFirstHead, "", r.imp.handoff("implement")); status != http.StatusOK {
		t.Fatalf("the implementer's completion = %d %s %s, want 200", status, failure.Code, failure.Error)
	}
	r.imp.endTurn()
	r.note("drove POST /legion/v1/handoff/complete for the implementer at %s, then its agent_end: 200", convergeFirstHead)
	got := r.waitFor("the issue in testing", func(got api.Issue) bool { return got.Phase == phase.Testing })
	r.note("read back: %s is in %s, status %s", r.key, got.Phase, got.Status)

	// The task of entry 1 is on the tester's claim, and its launch has not run: the claim records
	// the entry it was queued, and the state names the wake the pacing holds and when it is due.
	// The claim itself is what the start row creates, so the wait reads the listing rather than
	// asking for one token: a read made before that row's lane has run lists the implementer
	// alone, which is this wait's own first answer and not a failure.
	var queued *api.DeliveryView
	testwait.Eventually(t, "the tester's claim queued the task of its first entry", func() bool {
		for _, c := range r.d.claims() {
			if c.Token == tester {
				queued = c.Pending
				return queued != nil
			}
		}
		return false
	})
	waiting, held := r.waitingLaunch(tester)
	if !held {
		t.Fatalf("the daemon holds no launch for %s, want the one its pace has not let run: the proof turns on that wait", tester)
	}
	if r.launched(tester) {
		t.Fatalf("the fake runtime was asked to start a process for %s, want no launch while the pace holds it", tester)
	}
	phaseEntry, givenEntry := r.entries()
	if phaseEntry != 1 || givenEntry != 1 || queued.Phase != phase.Testing {
		t.Fatalf("the state reads phaseEntry %d, givenEntry %d with the tester holding %+v, want entry 1 queued as the testing phase's task",
			phaseEntry, givenEntry, queued)
	}
	r.note("read back: the tester's claim holds delivery %s, the %s phase's task, with phaseEntry %d and givenEntry %d; its launch is due at %s and no process has been started for it",
		queued.ID, queued.Phase, phaseEntry, givenEntry, waiting.DueAt.UTC().Format(time.RFC3339Nano))

	// The first phase change inside that wait: the held read runs and answers the head
	// conflicting with its base, which sends the issue back to implementing, and the
	// implementer, which is live, is prompted its merge-forward task at once.
	releaseRead()
	r.waitFor("the issue back in implementing", func(got api.Issue) bool { return got.Phase == phase.Implementing })
	fix := r.imp.awaitTask("the implementer's merge-forward task after the conflict", convergeOwedBound)
	r.note("let the held read of #%d run, with GitHub answering %s conflicting with %s: %s is in implementing and the implementer was prompted its merge-forward task (%s)",
		r.number, convergeFirstHead, convergeBase, r.key, firstLine(fix.Text))

	// The second: the implementer's merge forward at a head of its own hands testing out again,
	// which raises the tester's row to entry 2 and writes that entry's own task. GitHub answers
	// the new head mergeable, as it does once the base is merged forward into it.
	r.github.conflicts(false)
	r.github.head(convergeSecondHead)
	r.publishPush(convergeFirstHead, convergeSecondHead)
	r.publishSynchronize(convergeSecondHead)
	r.waitFor("the pull request at the pushed head", func(got api.Issue) bool {
		return got.PullRequest != nil && got.PullRequest.Head == convergeSecondHead
	})
	if status, failure := r.imp.complete("merged the base forward", convergeSecondHead, "", r.imp.handoff("implement")); status != http.StatusOK {
		t.Fatalf("the implementer's second completion = %d %s %s, want 200", status, failure.Code, failure.Error)
	}
	r.imp.endTurn()
	r.waitFor("the issue in testing again", func(got api.Issue) bool { return got.Phase == phase.Testing })
	testwait.Eventually(t, "the tester's phases row raised to entry 2", func() bool {
		phaseEntry, _ = r.entries()
		return phaseEntry == 2
	})
	phaseEntry, givenEntry = r.entries()
	held = false
	if _, held = r.waitingLaunch(tester); !held || r.launched(tester) {
		t.Fatalf("the launch of %s has run while the proof drove two phase changes, want it still held by the pace", tester)
	}
	if now := r.d.claim(tester).Pending; now == nil || now.ID != queued.ID {
		t.Fatalf("the tester holds %+v, want the task of entry 1 (%s) it was queued before the phase changed", now, queued.ID)
	}
	starts := r.startRows()
	if len(starts) != 1 || starts[0].role != claim.RoleTester || starts[0].entry != 2 {
		t.Fatalf("the start rows of %s = %+v, want the one the second testing entry wrote for the tester", r.key, starts)
	}
	r.note("read back: phaseEntry %d, givenEntry %d — the tester still holds entry 1's task (%s), and the start row of entry 2 (#%d, %d attempt(s)) waits: %s",
		phaseEntry, givenEntry, queued.ID, starts[0].id, starts[0].attempts, starts[0].lastErr)
}

// Check 2 of run one. The pace lets the tester's launch run. The tester comes up on its own
// worker-stream connection and is sent the task it was queued, entry 1's, which the claim held
// throughout; the completion it reports for that work is refused 409 HANDOFF_STALE_ENTRY,
// which names the entry the phase is on and says that entry's task is owed and arrives by itself,
// and records nothing. The turn ends, the task of entry 2 reaches the same connection, and the
// completion reported for it is taken: the issue moves to reviewing.
func (r *phaseEntryConvergeLive) theLaunchRunsAndTheEntryIsWorked() {
	t := r.t
	tester := r.token(claim.RoleTester)
	stale := r.d.claim(tester).Pending
	if stale == nil {
		t.Fatalf("the tester holds no task where the check before this one left it one")
	}
	r.awaitLaunch(tester)
	r.note("the pace let the tester's launch run: the fake runtime started its process")
	r.test = r.claimant(claim.RoleTester, "ses_tester")
	first := r.test.awaitTask("the tester's first task", convergeOwedBound)
	if first.DeliveryID != stale.ID {
		t.Fatalf("the tester was sent delivery %s, want %s, the task of entry 1 its claim held while the phase changed", first.DeliveryID, stale.ID)
	}
	phaseEntry, givenEntry := r.entries()
	if phaseEntry != 2 || givenEntry != 1 {
		t.Fatalf("the state reads phaseEntry %d, givenEntry %d where the tester works entry 1's task, want 2 and 1", phaseEntry, givenEntry)
	}
	r.note("read back: the tester was sent %s on its own connection (%s) — the work of entry %d, with the phase on entry %d",
		first.DeliveryID, firstLine(first.Text), givenEntry, phaseEntry)

	status, failure := r.test.complete("tested the first head", convergeFirstHead, "pass", r.test.handoff("test"))
	if status != http.StatusConflict || failure.Code != "HANDOFF_STALE_ENTRY" {
		t.Fatalf("the completion of entry 1's work = %d %s %s, want 409 HANDOFF_STALE_ENTRY", status, failure.Code, failure.Error)
	}
	r.note("read back: the completion the tester reported for entry 1's work = 409 %s — %s", failure.Code, failure.Error)
	if after := r.issue(r.key); after.Phase != phase.Testing {
		t.Fatalf("%s is in %s after the refused completion, want testing: a refusal records nothing", r.key, after.Phase)
	}
	r.note("read back: %s is still in testing — the refusal recorded nothing", r.key)

	// The turn ends, which retires the task it worked, and the entry the phase is on reaches the
	// same connection as a task of its own: the start row of entry 2, which waited behind the task
	// the claim held, hands it over on its next attempt.
	r.test.endTurn()
	second := r.test.awaitTask("the task of the entry the phase is on", convergeOwedBound)
	if second.DeliveryID == first.DeliveryID {
		t.Fatalf("the tester was sent delivery %s again, want a task of its own for entry 2", second.DeliveryID)
	}
	testwait.Eventually(t, "the tester given the entry the phase is on", func() bool {
		phaseEntry, givenEntry = r.entries()
		return phaseEntry == 2 && givenEntry == 2
	})
	r.note("read back: the tester was sent %s (%s), and the state reads phaseEntry %d, givenEntry %d",
		second.DeliveryID, firstLine(second.Text), phaseEntry, givenEntry)

	if status, failure := r.test.complete("tested the fix", convergeSecondHead, "pass", r.test.handoff("test")); status != http.StatusOK {
		t.Fatalf("the completion of entry 2's work = %d %s %s, want 200", status, failure.Code, failure.Error)
	}
	r.test.endTurn()
	moved := r.waitFor("the issue in reviewing", func(got api.Issue) bool { return got.Phase == phase.Reviewing })
	r.note("drove POST /legion/v1/handoff/complete in the turn entry 2's task started: 200, and %s is in %s", r.key, moved.Phase)
}

// Run two. A start row is deleted before any lane runs it, and the work it carried reaches its
// worker all the same, from the record and on the daemon's own sweep.
//
// The row is held unclaimable while the driver deletes it: a lane takes an advisory lock keyed by
// the issue before it claims a lifecycle row (record.Postgres.ClaimRow), and this step holds that
// lock across the conflict that writes the row, so no lane can have run it. The conflict reaches
// the issue as it reaches any issue short of an approval standing in reviewing: the daemon's own
// read of the pull request after a worker's completion (armConflict), which here is the
// reviewer's completion of the round the issue is in. The reviewer is brought up before the lock
// is taken, since no start of this issue runs while that lock is held. With the row gone, the
// implementer's phases row names an entry its claim was never given, and no queued start carries
// it — which is what the daemon reads as work owed (workflow.OwedEntry) — and no fact of the
// project arrives after the delete. The entry sweep is what sends it: on its own period the daemon
// applies an intake.EntrySweep and admission sends the role the work the entry its phases row
// names stands for (admit's resumeSlottedWorkers, workflow's SendOwedEntry).
//
// The implementer's connection may still hold a prompt run one sent it and no step read, and such
// a prompt carries the same conflict's task, so read as the sweep's delivery it would let this run
// pass on a daemon that re-sends nothing. The step therefore drains every prompt the connection
// has received before it lets the lock go, while no start of the issue can be on its way, and
// then holds that the task which arrives was minted from an outbox row written after the deleted
// one: an `outbox:<id>` delivery whose row id is above the deleted row's, which outbox ids, drawn
// from one sequence, give only to a row the sweep wrote after the delete.
func (r *phaseEntryConvergeLive) aDeletedStartRowLosesNoWork() {
	t := r.t
	ctx := context.Background()
	implementer := r.token(claim.RoleImplementer)
	before := r.d.claim(implementer)
	if before.State != string(supervise.StateIdle) || before.Pending != nil {
		t.Fatalf("the implementer is %s holding %+v, want it idle with no task: the row this step deletes is the only work owed to it",
			before.State, before.Pending)
	}

	// The reviewer, on the start the move to reviewing wrote. Its completion is what asks the
	// daemon to read the pull request, and that read is what carries the conflicting head.
	r.rev = r.claimant(claim.RoleReviewer, "ses_reviewer")
	r.rev.awaitTask("the reviewer's task", convergeOwedBound)
	r.note("the reviewer came up on the start the move to reviewing wrote, before the lock below")

	// The lane's own lock, held on a connection of the driver's: while this transaction lives, no
	// lane of this daemon can claim a lifecycle row of the issue, so the row the conflict writes
	// below is a row no lane has run.
	lock, err := r.st.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("take the lane's lock: %v", err)
	}
	locked := true
	defer func() {
		if locked {
			_ = lock.Rollback(ctx)
		}
	}()
	if _, err := lock.Exec(ctx, "select pg_advisory_xact_lock(hashtext('legion.outbox.lifecycle:' || $1::text))", r.key); err != nil {
		t.Fatalf("take the lane's lock on %s: %v", r.key, err)
	}
	r.note("holding the advisory lock a lane takes before it claims a lifecycle row of %s, so the start row the conflict writes cannot be run", r.key)

	r.armConflict()
	r.rev.startOwnTurn()
	if status, failure := r.rev.complete("reviewed the fix", "", "", r.rev.handoff("review")); status != http.StatusOK {
		t.Fatalf("the reviewer's completion = %d %s %s, want 200: it is what asks the daemon to read the pull request", status, failure.Code, failure.Error)
	}
	r.rev.endTurn()
	r.waitFor("the issue back in implementing", func(got api.Issue) bool { return got.Phase == phase.Implementing })
	var starts []convergeStart
	testwait.Eventually(t, "the start row of the implementer's new entry", func() bool {
		starts = r.startRows()
		return len(starts) == 1 && starts[0].role == claim.RoleImplementer
	})
	deleted := starts[0]
	phaseEntry, givenEntry := r.entries()
	if _, err := r.st.Pool().Exec(ctx, "delete from outbox where id = $1", deleted.id); err != nil {
		t.Fatalf("delete the start row %d: %v", deleted.id, err)
	}
	// Every prompt run one left unread is drained here, under the lock: no lane can have run a
	// start of the issue since the lock was taken, so nothing drained is the sweep's delivery,
	// and the sweep's delivery cannot arrive until the lock goes.
	for _, left := range r.imp.drainTasks() {
		r.note("drained a task the implementer's worker took before the delete and no step read: %s (%s)",
			left.DeliveryID, firstLine(left.Text))
	}
	if err := lock.Rollback(ctx); err != nil {
		t.Fatalf("let the lane's lock go: %v", err)
	}
	locked = false
	r.note("drove the base of %s past %s and the reviewer's completion, whose read of #%d answers the head conflicting, and deleted the start row that move wrote (#%d, entry %d, %d attempt(s)) before any lane ran it; the state reads phaseEntry %d, givenEntry %d",
		r.key, convergeSecondHead, r.number, deleted.id, deleted.entry, deleted.attempts, phaseEntry, givenEntry)
	if deleted.attempts != 0 || phaseEntry != deleted.entry || givenEntry >= phaseEntry {
		t.Fatalf("the deleted row %+v left the state at phaseEntry %d, givenEntry %d, want an untried row of the entry the phase is on with the claim behind it",
			deleted, phaseEntry, givenEntry)
	}
	if left := r.startRows(); len(left) != 0 {
		t.Fatalf("the outbox holds %+v after the delete, want no start row of %s at all", left, r.key)
	}

	// Nothing of the project is published from here on: what sends the entry's work is the
	// daemon's own sweep.
	task := r.imp.awaitTask(fmt.Sprintf("the work of entry %d, which the implementer's phases row names and whose start row %s%d this step deleted",
		deleted.entry, outboxDeliveryPrefix, deleted.id), convergeOwedBound)
	// The delivery's id names the outbox row it came from (the outbox's outboxDeliveryPrefix), so
	// it says in one value that the task reached the worker from a row written after the deleted
	// one, which is the sweep's, and not from the deleted row or any row before it.
	rest, minted := strings.CutPrefix(task.DeliveryID, outboxDeliveryPrefix)
	if !minted {
		t.Fatalf("the implementer was sent delivery %q, which names no outbox row, want %s<id> of a row the sweep wrote after the deleted row %s%d",
			task.DeliveryID, outboxDeliveryPrefix, outboxDeliveryPrefix, deleted.id)
	}
	row, err := strconv.ParseInt(rest, 10, 64)
	if err != nil {
		t.Fatalf("the implementer was sent delivery %q, whose row id does not parse (%v), want %s<id> of a row the sweep wrote after the deleted row %s%d",
			task.DeliveryID, err, outboxDeliveryPrefix, outboxDeliveryPrefix, deleted.id)
	}
	if row <= deleted.id {
		t.Fatalf("the implementer was sent delivery %s, of row %d, at or below the deleted row %s%d, want a row the sweep wrote after the delete",
			task.DeliveryID, row, outboxDeliveryPrefix, deleted.id)
	}
	if !strings.Contains(task.Text, "the head conflicts with "+convergeBase) {
		t.Fatalf("the implementer took %q, want the task entry %d names, which the conflict at %s wrote", task.Text, deleted.entry, convergeSecondHead)
	}
	testwait.Eventually(t, "the implementer given the entry its phases row names", func() bool {
		phaseEntry, givenEntry = r.entries()
		return phaseEntry == deleted.entry && givenEntry == deleted.entry
	})
	r.note("read back: with no fact of the project arriving, the sweep (period %s here, %s in the shipped binary) sent the implementer %s (%s) on its own connection, row %d above the deleted row %s%d, and the state reads phaseEntry %d, givenEntry %d",
		convergeEntrySweep, entrySweepInterval, task.DeliveryID, firstLine(task.Text), row, outboxDeliveryPrefix, deleted.id, phaseEntry, givenEntry)
	r.imp.endTurn()
}

// The one read of GET /legion/v1/state as a reader takes it: the issue carries both numbers on the
// wire (api.Issue.PhaseEntry, GivenEntry), which is what tells a role holding the work its row
// names from one the daemon has still to reach. No Go unit test reaches that on a booted daemon.
func (r *phaseEntryConvergeLive) stateCarriesBothNumbers() {
	t := r.t
	status, body := r.d.request(http.MethodGet, "/legion/v1/state", nil, false)
	if status != http.StatusOK {
		t.Fatalf("GET /legion/v1/state = %d %s, want 200", status, body)
	}
	var state struct {
		Issues map[string]map[string]json.RawMessage `json:"issues"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("decode the state: %v", err)
	}
	issue, recorded := state.Issues[r.key]
	if !recorded {
		t.Fatalf("the state records no issue %s", r.key)
	}
	phaseEntry, carriesPhase := issue["phaseEntry"]
	givenEntry, carriesGiven := issue["givenEntry"]
	if !carriesPhase || !carriesGiven {
		t.Fatalf("the state's %s carries phaseEntry %t and givenEntry %t, want both keys", r.key, carriesPhase, carriesGiven)
	}
	r.note("read back from GET /legion/v1/state: %s carries phaseEntry %s and givenEntry %s", r.key, phaseEntry, givenEntry)
}

// armConflict makes GitHub answer the pull request's head as conflicting with its base, and
// publishes the push onto that base that a live daemon sees first: the push records that the
// base moved (workflow's basePush), which is what makes the daemon's next read of the pull
// request apply its answer even where that answer only confirms what is stored (daemon's
// applyMergeability).
//
// What delivers the conflict is the read, not the push, and an issue short of an approval
// standing in reviewing has one source for that read: the one the daemon makes after a worker's
// completion (the API's handoffCompleted, daemon's readHandedOff, readIssuePullRequest). No
// GitHub envelope carries mergeability at all (intake's decode), and the base push asks for a
// merge-state read only for an approved issue in reviewing (workflow's askMergeStateFor,
// reviewRound's waitsOnMergeState), which is why every caller here arms the answer before the
// completion whose read is to carry it rather than after it.
func (r *phaseEntryConvergeLive) armConflict() {
	r.t.Helper()
	r.github.conflicts(true)
	r.seq++
	payload, err := json.Marshal(map[string]string{
		"kind": "push", "repo": liveRepo, "ref": "refs/heads/" + convergeBase,
		"before": fmt.Sprintf("base-%d", r.seq-1), "after": fmt.Sprintf("base-%d", r.seq),
		"changed_paths": "src/widget.go", "changed_paths_truncated": "false", "forced": "false",
		"pusher": "someone-else", "commit_count": "1", "head_subject": "another pull request merged",
	})
	if err != nil {
		r.t.Fatalf("encode the base push payload: %v", err)
	}
	subject := fmt.Sprintf("notifications.github.%s.push.branch.%s", r.subjectRepo(), convergeBase)
	r.publish(subject, "github", fmt.Sprintf("base-push-%d-%d", r.number, r.seq), string(payload))
}

// publishPush publishes the implementer's push of to onto from on the issue's branch, changing
// code, as Envoy relays a GitHub push (intake's decodePush reads it).
func (r *phaseEntryConvergeLive) publishPush(from, to string) {
	r.t.Helper()
	payload, err := json.Marshal(map[string]string{
		"kind": "push", "repo": liveRepo, "ref": "refs/heads/legion/" + r.key, "before": from, "after": to,
		"pusher": "legion-implement[bot]", "commit_count": "1", "head_subject": "fix the red",
	})
	if err != nil {
		r.t.Fatalf("encode the push payload: %v", err)
	}
	subject := fmt.Sprintf("notifications.github.%s.push.branch.legion/%s", r.subjectRepo(), r.key)
	r.publish(subject, "github", fmt.Sprintf("push-%d-%s", r.number, to), string(payload))
}

// publishSynchronize publishes GitHub's synchronize of the pull request to head, as Envoy relays a
// GitHub pull-request event (intake's decodePullRequest reads it).
func (r *phaseEntryConvergeLive) publishSynchronize(head string) {
	r.t.Helper()
	payload, err := json.Marshal(map[string]string{
		"action": "synchronize", "kind": "pr", "repo": liveRepo, "number": strconv.Itoa(r.number),
		"head_sha": head, "base_ref": convergeBase, "head_ref": "legion/" + r.key,
		"title":      "the issue whose phase entries converge",
		"url":        "https://github.com/" + liveRepo + "/pull/" + strconv.Itoa(r.number),
		"updated_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		r.t.Fatalf("encode the synchronize payload: %v", err)
	}
	subject := fmt.Sprintf("notifications.github.%s.pr.%d", r.subjectRepo(), r.number)
	r.publish(subject, "github", fmt.Sprintf("pr-%d-synchronize-%s", r.number, head), string(payload))
}

// convergeGitHub is GitHub's REST API for acme/widgets as this proof's daemon reads it: the one
// pull request at the head the driver last pushed, with its base and `mergeable: true`
// (readPullRequest, whose `mergeable` is what the daemon's merge-state read judges), and its
// reviews as none (readReviews). It also answers
// what a launch and a disarm read: any repository, with default branch main, and a GraphQL root
// that knows no pull request. Anything else is answered 404, which the daemon's own reads take as
// GitHub being unreadable — the implementer's completion is applied unread on that answer
// (api/handoff.go's implementationPullRequest) — so no read of this proof reaches github.com.
type convergeGitHub struct {
	t      *testing.T
	url    string
	key    string
	number int
	mu     sync.Mutex
	// sha is the pull request's head on GitHub, which a push moves.
	sha string
	// conflicting is what GitHub's lazily computed `mergeable` answers: false once the base has
	// moved past the head, which is the one answer of a pull request that moves an issue.
	conflicting bool
	// held holds every read of the pull request while a check means that read to land later
	// (pause); nil holds none.
	held chan struct{}
}

func newConvergeGitHub(t *testing.T, key string, number int) *convergeGitHub {
	t.Helper()
	g := &convergeGitHub{t: t, key: key, number: number, sha: convergeFirstHead}
	g.url = githubtest.Server(t, githubtest.Spec{Handle: g.ServeHTTP})
	return g
}

// head moves the pull request to sha, as a push does on GitHub.
func (g *convergeGitHub) head(sha string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sha = sha
}

// conflicts makes GitHub answer `mergeable: false` for the head, as it does once the base has
// moved past it, and answer it mergeable again once the implementer has merged the base forward.
func (g *convergeGitHub) conflicts(yes bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.conflicting = yes
}

// pause holds every read of the pull request itself at this stand-in until the function it
// answers with is called, which is how a check reads the state a completion left before the read
// that completion queued lands: the read is the one place an issue short of reviewing learns its
// head conflicts (armConflict), so the proof arms the answer, drives the completion, reads, and
// then lets the read run. It holds nothing else GitHub answers, and the release may be called
// twice — a check defers it and calls it where it means the read to land.
func (g *convergeGitHub) pause() func() {
	g.mu.Lock()
	defer g.mu.Unlock()
	held := make(chan struct{})
	g.held = held
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			if g.held == held {
				g.held = nil
			}
			g.mu.Unlock()
			close(held)
		})
	}
}

// waitForRelease holds a read of the pull request while pause holds one, and holds nothing else.
// It runs before the lock below, since a handler waiting under that lock would stop every other
// answer this stand-in gives.
func (g *convergeGitHub) waitForRelease(path string) {
	if !strings.HasSuffix(path, fmt.Sprintf("/pulls/%d", g.number)) {
		return
	}
	g.mu.Lock()
	held := g.held
	g.mu.Unlock()
	if held != nil {
		<-held
	}
}

func (g *convergeGitHub) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	g.waitForRelease(req.URL.Path)
	g.mu.Lock()
	defer g.mu.Unlock()
	if req.Method == http.MethodPost && req.URL.Path == "/graphql" {
		_, _ = io.WriteString(w, `{"data":{"repository":{"pullRequest":null}}}`)
		return
	}
	if req.Method == http.MethodGet && g.answer(w, req.URL.Path) {
		return
	}
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, `{"message":"Not Found"}`)
}

// answer writes the stand-in's answer to a GET of path, and says whether it had one.
func (g *convergeGitHub) answer(w http.ResponseWriter, path string) bool {
	rest, ours := strings.CutPrefix(path, "/repos/"+liveRepo)
	if !ours {
		// Any other repository is answered as the launch reads it: a name and its default branch.
		other, isRepository := strings.CutPrefix(path, "/repos/")
		if parts := strings.Split(other, "/"); isRepository && len(parts) == 2 {
			g.write(w, map[string]any{"full_name": other, "default_branch": convergeBase})
			return true
		}
		return false
	}
	switch {
	case rest == "":
		g.write(w, map[string]any{"full_name": liveRepo, "default_branch": convergeBase})
	case rest == fmt.Sprintf("/pulls/%d/reviews", g.number):
		g.write(w, []any{})
	case rest == fmt.Sprintf("/pulls/%d", g.number):
		g.write(w, map[string]any{
			"number": g.number, "state": "open", "mergeable": !g.conflicting,
			"base": map[string]string{"ref": convergeBase},
			"head": map[string]string{"ref": "legion/" + g.key, "sha": g.sha},
		})
	default:
		return false
	}
	return true
}

// write encodes value as the answer.
func (g *convergeGitHub) write(w http.ResponseWriter, value any) {
	if err := json.NewEncoder(w).Encode(value); err != nil {
		g.t.Errorf("write the GitHub stand-in's answer: %v", err)
	}
}
