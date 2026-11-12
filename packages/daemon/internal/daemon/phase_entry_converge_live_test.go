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
// stream for the settlements, the pushes and the synchronizes; the daemon's own HTTP routes for
// each role's registration, readiness, grants and completions; and the daemon's worker stream for
// each role's turns, which this driver speaks as `legion worker-shim` does (shimwire frames).
// Each check prints `== <name>`, what it observed and `ok <name>`; the first that fails ends the
// run non-zero, naming it.
//
// Run one, in two checks. The configuration holds the daemon to three wakes a minute in bursts of
// one (convergeWakeRate, convergeWakeBurst), and the boot's relaunch of the implementer spends the
// burst, so the daemon's wake pacing has not yet let the next role's launch run and that role's
// start waits for it. The implementer's completion hands testing to the tester, whose phases row
// is raised to entry 1 with that entry's own task, and whose claim is queued that task while its
// launch waits. The issue's phase then changes twice while the start waits: CI goes red at the
// first head, which sends the issue back to implementing, and the implementer's fix at a second
// head hands testing out again, which raises the tester's row to entry 2 and writes that entry's
// own task. The daemon then lets the launch run: the tester comes up on its own worker-stream
// connection and is sent the task it was queued, entry 1's, because the claim held it throughout
// (supervise's queue refuses a second delivery while one is pending); the completion the tester
// reports for it is refused 409 HANDOFF_STALE_ASSIGNMENT naming both entries and recording
// nothing, and the entry the phase is on then reaches the same connection as a task of its own,
// whose completion the daemon takes. That is the whole of R5 on a running daemon, and the entry on
// each delivery is what tells the two tasks apart (record.PhaseRow.Entry,
// supervise.Claim.GivenEntry, both on GET /legion/v1/state as `phaseEntry` and `givenEntry`).
//
// Run two. The same rig deletes a start row from the outbox before any lane runs it: the driver
// holds the advisory lock a lane takes before it claims a lifecycle row of the issue
// (record.Postgres.ClaimRow's `legion.outbox.lifecycle:<issue>`), publishes the red that raises
// the implementer's entry and writes that entry's start row, deletes that row, and lets the lock
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
// names in LEGION_E2E_NATS_URL. GitHub is convergeGitHub, a REST stand-in for the one pull request
// the proof drives, whose base branch requires the check `ci`. Dispatch is the board the package's
// whole-daemon live proofs share (liveDispatch, in live_proof_test.go). The two Apps' tokens are
// workflowTokenRecorder's placeholders. No agent runs: every claim is launched by the package's
// fake runtime, which says it provisions each workspace in the claim's own pod, as the Agent
// Sandbox runtime does, so the daemon clones nothing from GitHub. The issue, its pull request, its
// slot and the implementer's claim are seeded through the store, as every daemon test seeds its
// fixture; what is driven is the change.
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

// convergeCheck is the one check the pull request's base branch requires, and so the only one a
// red verdict can stand on; convergeBase is that base branch, the repository's default.
const (
	convergeCheck = "ci"
	convergeBase  = "main"
)

// The two heads the proof drives: the implementer's seeded head, which run one's red settles, and
// the fix it pushes for the second testing entry, which run two's red settles. Each red is at a
// head of its own, since a settlement that replays to the judgement already recorded changes
// nothing (classify.ApplyCheckSettlement).
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
	// imp and test are the implementer and the tester as their agents reach the daemon.
	imp, test *convergeClaimant
	// seq numbers each settlement, so no two are the same report (classify.sameSettlement).
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
// recorded: one root implementing on an open pull request whose base requires ci, holding its
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
	o := fakeRuntime(r.rt, r.built)
	o.workflowTokens = &workflowTokenRecorder{}
	o.githubAPI = r.github.url
	// The wall clock, in place of this package's still one (daemon.go's RealClock for a nil
	// override): the launch the pacing holds runs when its timer fires, and a wake parked on a
	// clock whose timers never fire would never take its turn.
	o.clock = nil
	o.entrySweep = convergeEntrySweep
	r.cfg.WorkerWakesPerMinute, r.cfg.WorkerWakeBurst = convergeWakeRate, convergeWakeBurst
	r.d = startDaemonWithin(t, r.cfg, o, r.log(), 60*time.Second)
	return r
}

// seed records the issue as the daemon would hold it at the moment its implementer began work: a
// root handed to Legion, implementing, holding its tree's admission slot, its lifecycle open under
// the workflow, on an open pull request at its first head whose base requires ci, and its
// implementer's claim mid-launch, serving run 1. No phases row is written: every entry this proof
// reads is raised live, by the transitions the completions and the reds make.
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
			HeadSHA: convergeFirstHead, Required: []string{convergeCheck}, Failing: []string{},
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
	r.note("seeded: %s implementing on pull request #%d at %s, base %s requiring [%s], its implementer mid-launch serving run 1",
		r.key, r.number, convergeFirstHead, convergeBase, convergeCheck)
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
// exchanged it for the session's capability secret, and a reader that answers what the daemon asks
// of the shim and hands each prompt to the step, starting its turn as the agent would.
type convergeClaimant struct {
	r       *phaseEntryConvergeLive
	token   claim.Token
	role    claim.Role
	session string
	secret  string
	sh      *shim
	mu      sync.Mutex
	// turn is whether a turn is running, which the daemon's state probe reads back, and prompt
	// the id of the prompt that started it, which the result at its end names.
	turn    bool
	prompt  string
	prompts chan shimwire.Prompt
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
		sh: dialShim(t, address, launch.BootToken), prompts: make(chan shimwire.Prompt, 8)}
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
	go c.serve()
	if status, body := r.d.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: token, SessionID: session, Secret: registered.Secret, Generation: launch.Generation,
	}, false); status != http.StatusNoContent {
		t.Fatalf("ready %s = %d %s, want 204", token, status, body)
	}
	return c
}

// serve reads every frame the daemon sends the shim until the connection closes: it answers the
// protocol negotiation and the state probe (idle, or streaming while a turn runs), and takes each
// prompt — answering it and starting the turn, as Oh My Pi does — for the step to read. It runs
// for the claim's whole life, because a step that left the connection unread would have the
// daemon's own requests time out and the task it was sending wait for a retry.
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
			c.mu.Lock()
			streaming := c.turn
			c.mu.Unlock()
			data, _ := json.Marshal(shimwire.StateData{IsStreaming: &streaming})
			c.send(shimwire.Response{ID: f.ID, Command: shimwire.TypeGetState, Success: true, Data: data})
		case shimwire.Prompt:
			c.send(shimwire.Response{ID: f.ID, Command: shimwire.TypePrompt, Success: true})
			c.mu.Lock()
			c.turn, c.prompt = true, f.ID
			c.mu.Unlock()
			c.send(shimwire.AgentStart{DeliveryID: f.DeliveryID})
			c.prompts <- f
		}
	}
}

// send writes one frame to the daemon; the reader and the step both write.
func (c *convergeClaimant) send(frame shimwire.Frame) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.sh.writer.WriteFrame(frame); err != nil {
		c.r.t.Errorf("send %s for %s: %v", frame.FrameType(), c.token, err)
	}
}

// startOwnTurn is the agent beginning a turn of its own, on no prompt of the daemon's: the turn
// the seeded implementer is in when the proof reaches it. It returns once the daemon has recorded
// the claim working.
func (c *convergeClaimant) startOwnTurn() {
	t := c.r.t
	t.Helper()
	c.mu.Lock()
	c.turn, c.prompt = true, ""
	c.mu.Unlock()
	c.send(shimwire.AgentStart{})
	testwait.Eventually(t, fmt.Sprintf("the %s's turn running", c.role), func() bool {
		return c.r.d.claim(c.token).State == string(supervise.StateWorking)
	})
}

// endTurn is the agent finishing its turn: the agent_end, and behind it the prompt's own result,
// which is what names the prompt the turn belonged to (shimwire.PromptResult) and retires the
// task. It returns once the daemon has recorded the claim out of its turn.
func (c *convergeClaimant) endTurn() {
	t := c.r.t
	t.Helper()
	c.mu.Lock()
	prompt := c.prompt
	c.turn, c.prompt = false, ""
	c.mu.Unlock()
	c.send(shimwire.AgentEnd{})
	if prompt != "" {
		c.send(shimwire.PromptResult{ID: prompt, AgentInvoked: true})
	}
	testwait.Eventually(t, fmt.Sprintf("the %s out of its turn", c.role), func() bool {
		return c.r.d.claim(c.token).State == string(supervise.StateIdle)
	})
}

// awaitPrompt waits for the next task the daemon sends this claim, within bound, and returns it.
func (c *convergeClaimant) awaitPrompt(what string, bound time.Duration) shimwire.Prompt {
	t := c.r.t
	t.Helper()
	select {
	case prompt := <-c.prompts:
		return prompt
	case <-time.After(bound):
		t.Fatalf("the %s was sent no task within %s: %s", c.role, bound, what)
		return shimwire.Prompt{}
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
// (api.State.WakesWaiting). The issue's phase then changes twice inside that wait — CI red at the
// first head sends it back to implementing, and the implementer's fix at a second head hands
// testing out again — which raises the tester's row to entry 2 with that entry's own task, while
// the claim still holds the task of entry 1 and the start row carrying entry 2 waits behind it.
func (r *phaseEntryConvergeLive) phaseChangesWhileTheStartWaits() {
	t := r.t
	tester := r.token(claim.RoleTester)
	r.imp = r.claimant(claim.RoleImplementer, "ses_implementer")
	r.note("the implementer is registered and ready on the launch this boot made for the claim the fixture left mid-launch; that launch spent the burst of %d wake(s) a minute in bursts of %d",
		convergeWakeRate, convergeWakeBurst)
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

	// The first phase change inside that wait: CI red at the head sends the issue back to
	// implementing, and the implementer, which is live, is prompted its fix task at once.
	r.publishSettlement(convergeFirstHead, true)
	r.waitFor("the issue back in implementing", func(got api.Issue) bool { return got.Phase == phase.Implementing })
	fix := r.imp.awaitPrompt("the implementer's fix task after the red", convergeOwedBound)
	r.note("drove CI's settlement of %s at %s with %s failed: %s is in implementing and the implementer was prompted its fix task (%s)",
		r.key, convergeFirstHead, convergeCheck, r.key, firstLine(fix.Message))

	// The second: the implementer's fix at a head of its own hands testing out again, which raises
	// the tester's row to entry 2 and writes that entry's own task.
	r.github.head(convergeSecondHead)
	r.publishPush(convergeFirstHead, convergeSecondHead)
	r.publishSynchronize(convergeSecondHead)
	r.waitFor("the pull request at the pushed head", func(got api.Issue) bool {
		return got.PullRequest != nil && got.PullRequest.Head == convergeSecondHead
	})
	if status, failure := r.imp.complete("fixed the red", convergeSecondHead, "", r.imp.handoff("implement")); status != http.StatusOK {
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
// throughout; the completion it reports for that work is refused 409 HANDOFF_STALE_ASSIGNMENT,
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
	first := r.test.awaitPrompt("the tester's first task", convergeOwedBound)
	if first.DeliveryID != stale.ID {
		t.Fatalf("the tester was sent delivery %s, want %s, the task of entry 1 its claim held while the phase changed", first.DeliveryID, stale.ID)
	}
	phaseEntry, givenEntry := r.entries()
	if phaseEntry != 2 || givenEntry != 1 {
		t.Fatalf("the state reads phaseEntry %d, givenEntry %d where the tester works entry 1's task, want 2 and 1", phaseEntry, givenEntry)
	}
	r.note("read back: the tester was sent %s on its own connection (%s) — the work of entry %d, with the phase on entry %d",
		first.DeliveryID, firstLine(first.Message), givenEntry, phaseEntry)

	status, failure := r.test.complete("tested the first head", convergeFirstHead, "pass", r.test.handoff("test"))
	if status != http.StatusConflict || failure.Code != "HANDOFF_STALE_ASSIGNMENT" {
		t.Fatalf("the completion of entry 1's work = %d %s %s, want 409 HANDOFF_STALE_ASSIGNMENT", status, failure.Code, failure.Error)
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
	second := r.test.awaitPrompt("the task of the entry the phase is on", convergeOwedBound)
	if second.DeliveryID == first.DeliveryID {
		t.Fatalf("the tester was sent delivery %s again, want a task of its own for entry 2", second.DeliveryID)
	}
	testwait.Eventually(t, "the tester given the entry the phase is on", func() bool {
		phaseEntry, givenEntry = r.entries()
		return phaseEntry == 2 && givenEntry == 2
	})
	r.note("read back: the tester was sent %s (%s), and the state reads phaseEntry %d, givenEntry %d",
		second.DeliveryID, firstLine(second.Message), phaseEntry, givenEntry)

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
// lock across the red that writes the row, so no lane can have run it. With the row gone, the
// implementer's phases row names an entry its claim was never given, and no queued start carries
// it — which is what the daemon reads as work owed (workflow.OwedEntry) — and no fact of the
// project arrives after the delete. The entry sweep is what sends it: on its own period the daemon
// applies an intake.EntrySweep and admission sends the role the work the entry its phases row
// names stands for (admit's resumeSlottedWorkers, workflow's SendOwedEntry).
func (r *phaseEntryConvergeLive) aDeletedStartRowLosesNoWork() {
	t := r.t
	ctx := context.Background()
	implementer := r.token(claim.RoleImplementer)
	before := r.d.claim(implementer)
	if before.State != string(supervise.StateIdle) || before.Pending != nil {
		t.Fatalf("the implementer is %s holding %+v, want it idle with no task: the row this step deletes is the only work owed to it",
			before.State, before.Pending)
	}

	// The lane's own lock, held on a connection of the driver's: while this transaction lives, no
	// lane of this daemon can claim a lifecycle row of the issue, so the row the red writes below
	// is a row no lane has run.
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
	r.note("holding the advisory lock a lane takes before it claims a lifecycle row of %s, so the start row the red writes cannot be run", r.key)

	r.publishSettlement(convergeSecondHead, true)
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
	if err := lock.Rollback(ctx); err != nil {
		t.Fatalf("let the lane's lock go: %v", err)
	}
	locked = false
	r.note("drove CI's settlement of %s at %s with %s failed, and deleted the start row it wrote (#%d, entry %d, %d attempt(s)) before any lane ran it; the state reads phaseEntry %d, givenEntry %d",
		r.key, convergeSecondHead, convergeCheck, deleted.id, deleted.entry, deleted.attempts, phaseEntry, givenEntry)
	if deleted.attempts != 0 || phaseEntry != deleted.entry || givenEntry >= phaseEntry {
		t.Fatalf("the deleted row %+v left the state at phaseEntry %d, givenEntry %d, want an untried row of the entry the phase is on with the claim behind it",
			deleted, phaseEntry, givenEntry)
	}
	if left := r.startRows(); len(left) != 0 {
		t.Fatalf("the outbox holds %+v after the delete, want no start row of %s at all", left, r.key)
	}

	// Nothing of the project is published from here on: what sends the entry's work is the
	// daemon's own sweep.
	task := r.imp.awaitPrompt("the work of the entry the implementer's phases row names", convergeOwedBound)
	if !strings.Contains(task.Message, "CI is red at "+convergeSecondHead) {
		t.Fatalf("the implementer was sent %q, want the task entry %d names, which the red at %s wrote", task.Message, deleted.entry, convergeSecondHead)
	}
	testwait.Eventually(t, "the implementer given the entry its phases row names", func() bool {
		phaseEntry, givenEntry = r.entries()
		return phaseEntry == deleted.entry && givenEntry == deleted.entry
	})
	// The delivery's id names the outbox row it came from (the outbox's outboxDeliveryPrefix), so
	// it says in one value that the task reached the worker from a row the sweep wrote and not
	// from the one this step deleted.
	if from := fmt.Sprintf("%s%d", outboxDeliveryPrefix, deleted.id); task.DeliveryID == from {
		t.Fatalf("the implementer was sent %s, the deleted row's own delivery, want one of a row the sweep wrote", from)
	}
	r.note("read back: with no fact of the project arriving, the sweep (period %s here, %s in the shipped binary) sent the implementer %s (%s) on its own connection, where the deleted row's delivery would have been %s%d, and the state reads phaseEntry %d, givenEntry %d",
		convergeEntrySweep, entrySweepInterval, task.DeliveryID, firstLine(task.Message), outboxDeliveryPrefix, deleted.id, phaseEntry, givenEntry)
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

// publishSettlement publishes CI's settlement of head on the pull request with the one check ci,
// failed or passed, as Envoy relays one (intake's decodeChecks reads it).
func (r *phaseEntryConvergeLive) publishSettlement(head string, failed bool) {
	r.t.Helper()
	group := func(checks ...string) map[string]any {
		return map[string]any{"count": len(checks), "checks": append([]string{}, checks...)}
	}
	failedGroup, passedGroup := group(), group(convergeCheck)
	if failed {
		failedGroup, passedGroup = passedGroup, failedGroup
	}
	r.seq++
	payload, err := json.Marshal(map[string]any{
		"kind": "checks", "repo": liveRepo, "number": strconv.Itoa(r.number), "sha": head,
		"check_runs": []map[string]any{{"name": convergeCheck, "id": r.seq}}, "generation": 1,
		"snapshot": fmt.Sprintf("%s-%s-%d", r.key, head, r.seq), "settled_at": time.Now().UnixMilli(),
		"failed": failedGroup, "passed": passedGroup, "running": group(), "queued": group(), "cancelled": group(), "skipped": group(),
	})
	if err != nil {
		r.t.Fatalf("encode the settlement payload: %v", err)
	}
	subject := fmt.Sprintf("notifications.github.%s.pr.%d.checks", r.subjectRepo(), r.number)
	r.publish(subject, "github", fmt.Sprintf("checks-%d-%s-%d", r.number, head, r.seq), string(payload))
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
// (readPullRequest), its reviews as none (readReviews), the base branch's rulesets requiring ci
// (requiredchecks.Required's /rules/branches/<base>) and the branch itself as unprotected (its
// /branches/<base>, which a 404 would make an error that keeps the set last read). It also answers
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

func (g *convergeGitHub) ServeHTTP(w http.ResponseWriter, req *http.Request) {
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
	case rest == "/rules/branches/"+convergeBase:
		g.write(w, []map[string]any{
			{"type": "pull_request"},
			{"type": "required_status_checks", "parameters": map[string]any{
				"required_status_checks": []map[string]string{{"context": convergeCheck}},
			}},
		})
	case rest == "/branches/"+convergeBase:
		g.write(w, map[string]any{"name": convergeBase, "protected": false})
	case rest == fmt.Sprintf("/pulls/%d/reviews", g.number):
		g.write(w, []any{})
	case rest == fmt.Sprintf("/pulls/%d", g.number):
		g.write(w, map[string]any{
			"number": g.number, "state": "open", "mergeable": true,
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
