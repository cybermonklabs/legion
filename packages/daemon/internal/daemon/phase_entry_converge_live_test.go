//go:build e2e

// The live proof that the work of a phase entry reaches the worker that entry names, on a running
// daemon (dispatch://INFRA-2800's R3, R4, R5, R6 and R9). scripts/e2e/phase-entry-converge.sh
// builds this driver with `-tags e2e` and owns the nats-server and the PostgreSQL it runs against.
//
// The daemon is the whole daemon: run, the function `legion start` calls, through this package's
// own whole-daemon harness (startDaemonLogging), so the proof exercises the real configuration, the
// real store and its migrations, the real HTTP API, the real claim supervision and its worker
// stream, the real openWorkflow, the real JetStream consumers, the real outbox lanes and the real
// entry sweep. Facts enter only as production's do: GitHub envelopes on the ENVOY_NOTIFICATIONS
// stream for the implementer's push and its synchronize; the daemon's own HTTP routes for each
// role's registration, readiness, grants, completions and backward moves; and the daemon's worker
// stream for each role's turns, which this driver speaks as `legion worker-shim` does (shimwire
// frames). Each check prints `== <name>`, what it observed and `ok <name>`; the first that fails
// ends the run non-zero, naming it.
//
// Run one, in three checks. The configuration holds the daemon to six wakes a minute in bursts of
// one (convergeWakeRate, convergeWakeBurst), and the boot's relaunch of the implementer spends the
// burst, so the daemon's wake pacing has not yet let the next role's launch run and that role's
// start waits for it. The implementer's completion, made from a turn of its own that it goes on in,
// hands testing to the tester, whose phases row is raised to entry 1 with that entry's own task,
// and whose claim is queued that task while its launch waits. The pace then lets the launch run,
// and the tester comes up on its own worker-stream connection with that task still held on its
// claim: the daemon sends no role a task that adopts the issue's working copy while another role
// of the issue is in a turn (supervise's sendPending), and the implementer is in its. The issue's
// phase then changes twice while the claim holds that task. The tester asks for the work back with
// its own backward move (POST /legion/v1/phase/backward, behind_entry_fence_live_test.go's
// moveBack), from a turn of its own, since the engine takes a backward move from no role but the
// one running the issue's phase (workflow's Engine.backward, BACKWARD_REFUSED); and the
// implementer, still in its turn, pushes a second head and completes there, which hands testing
// out again, raises the tester's row to entry 2 and writes that entry's own task. The claim still
// holds the task of entry 1 (supervise's queue refuses a second delivery while one is pending), so
// the start row of entry 2 waits behind it. Once both turns end, the tester is sent the task it
// held, entry 1's; the completion the tester reports for it is refused 409 HANDOFF_STALE_ENTRY
// naming both entries and recording nothing, and the entry the phase is on then reaches the same
// connection as a task of its own, whose completion the daemon takes. That is the whole of R5 on a
// running daemon, and the entry on each delivery is what tells the two tasks apart
// (record.PhaseRow.Entry, supervise.Claim.GivenEntry, both on GET /legion/v1/state as `phaseEntry`
// and `givenEntry`).
//
// Run two. The same rig deletes a start row from the outbox before any lane runs it: the driver
// holds the advisory lock a lane takes before it claims a lifecycle row of the issue
// (record.Postgres.ClaimRow's `legion.outbox.lifecycle:<issue>`), drives the reviewer's own
// backward move that raises the implementer's entry and writes that entry's start row, deletes that
// row, and lets the lock go, so the row's work exists only in the record. With no fact of the
// project arriving after it, the daemon's own entry sweep (intake.EntrySweep,
// workflowRuntime.tickEntrySweep) sends the entry's own task to the implementer's live connection.
// The sweep's period is a Go constant with a test override (the overrides' entrySweep,
// convergeEntrySweep here); the shipped binary keeps entrySweepInterval's minute, which is why the
// driver sets the override rather than waiting one.
//
// One more read. The driver reads GET /legion/v1/state once and holds that the issue carries both
// numbers on the wire, which is R6 on a booted daemon.
//
// What stands in lies outside the rule. The broker is a native nats-server the script starts and
// names in LEGION_E2E_NATS_URL. GitHub is the harness's REST stand-in for the one pull request the
// proof drives (liveGitHub, in live_issue_proof_test.go), which serves that pull request's state,
// head, base and mergeable, the last of them mergeable throughout, and no branch rule of any kind:
// what it does not serve it answers 404, which the daemon's own reads take as GitHub being
// unreadable — the implementer's completion is applied unread on that answer (api/handoff.go's
// implementationPullRequest) — so no read of this proof reaches github.com. Dispatch is the board
// the package's whole-daemon live proofs share (liveDispatch, in live_proof_test.go), which holds
// the handoff record each completion names and takes the reason each backward move posts. The two
// Apps' tokens are workflowTokenRecorder's placeholders. No agent runs: every claim is launched by
// the package's fake runtime, which says it provisions each workspace in the claim's own pod, as
// the Agent Sandbox runtime does, so the daemon clones nothing from GitHub. The issue, its pull
// request, its slot and the implementer's claim are seeded through the store, as every daemon test
// seeds its fixture; what is driven is the change.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// convergeBase is the pull request's base branch, the repository's default.
const convergeBase = "main"

// The two heads the proof drives: the implementer's seeded head, which the tester's first entry
// is for, and the head the implementer pushes once the tester has asked for the work back, which
// the second testing entry is for and the record holds from then on.
const (
	convergeFirstHead  = "pe1"
	convergeSecondHead = "pe2"
)

// The reasons the two backward moves of the proof give, each of which the move posts on the issue
// and composes into the task of the implementer's entry it raises (workflow's task, `Reason:`).
const (
	convergeTesterReason   = "the tests show a fault in the code; back to implementing"
	convergeReviewerReason = "the review finds a fault in the code; back to implementing"
)

// The pace the daemon is held to, set on the configuration as daemon_test.go sets it
// (config.Config.WorkerWakesPerMinute, WorkerWakeBurst): six wakes a minute in bursts of one, so
// one wake costs ten seconds. The boot's relaunch of the seeded implementer spends the burst, so
// the launch of the next role the daemon starts cannot run until that interval has passed. Run
// one's first check reads the tester's start waiting for that launch, which it does in well under
// a second, and holds that the launch is still waiting when it is done, so a pace this proof
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
// and is capped there (record.RetryBackoffMost), for the start row run one leaves waiting behind the
// task the claim already holds, and one sweep period for run two.
const convergeOwedBound = 2 * time.Minute

// phaseEntryConvergeLive is one run of the proof: the booted daemon, the one issue it drives, the
// GitHub stand-in its facts are read against, and the three roles the driver acts as over the
// worker stream.
type phaseEntryConvergeLive struct {
	liveIssueProof
	// imp, test and rev are the implementer, the tester and the reviewer as their agents reach
	// the daemon.
	imp, test, rev *convergeClaimant
	// queued is the delivery of entry 1's task, which the tester's claim is queued while its
	// launch waits and holds until run one's last check sends it.
	queued string
}

// TestAPhaseEntryConvergesOnARunningDaemonLive drives the proof's five checks against one booted
// daemon, in the order each one's state follows from the last: the tester's start waiting for the
// launch the pacing holds, the phase changing twice while the tester's claim, up on that launch,
// holds the task of entry 1, the entry the phase is on reaching the tester after that held task
// is sent and its completion refused, a start row deleted before any lane runs it whose work the
// sweep sends all the same, and the state's two numbers.
func TestAPhaseEntryConvergesOnARunningDaemonLive(t *testing.T) {
	fixtureenv.Live(t)
	r := newPhaseEntryConvergeLive(t)
	r.step("run1-the-testers-start-waits-for-the-launch-the-pace-holds", r.theStartWaitsForTheLaunch)
	r.step("run1-the-phase-changes-twice-while-the-testers-claim-holds-the-task-of-entry-1", r.phaseChangesWhileTheClaimHoldsItsTask)
	r.step("run1-the-held-task-is-refused-and-the-entry-the-phase-is-on-is-worked", r.theHeldTaskIsRefusedAndTheEntryIsWorked)
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
	r := &phaseEntryConvergeLive{liveIssueProof: newLiveIssueProof(t, "scripts/e2e/phase-entry-converge.sh",
		convergeBase, "the issue whose phase entries converge")}
	r.serveGitHub(&liveGitHub{sha: convergeFirstHead})
	r.seed()
	r.cfg.WorkerWakesPerMinute, r.cfg.WorkerWakeBurst = convergeWakeRate, convergeWakeBurst
	r.boot(func(o *overrides) {
		// The wall clock, in place of this package's still one (daemon.go's RealClock for a nil
		// override): the launch the pacing holds runs when its timer fires, and a wake held on a
		// clock whose timers never fire would never take its turn.
		o.clock = nil
		o.entrySweep = convergeEntrySweep
	})
	return r
}

// seed records the issue as seedImplementing does, on an open pull request at its first head. No
// phases row is written: every entry this proof reads is raised live, by the transitions the
// completions and the backward moves make.
func (r *phaseEntryConvergeLive) seed() {
	r.t.Helper()
	r.seedImplementing(convergeFirstHead)
	r.note("seeded: %s implementing on pull request #%d at %s, base %s, its implementer mid-launch serving run 1",
		r.key, r.number, convergeFirstHead, convergeBase)
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

// convergeClaimant is one role's claim as its agent reaches it: the registered session
// (liveSession), and a reader that answers what the daemon asks of the shim and hands each prompt
// to the step, starting its turn as the agent would.
type convergeClaimant struct {
	*liveSession
	mu sync.Mutex
	// turn is whether a turn is running, which the daemon's state probe reads back, and prompt
	// the id of the prompt that started it, which the result at its end names.
	turn    bool
	prompt  string
	prompts chan shimwire.Prompt
}

// servedClaimant brings the launched claim of role to ready, as its pane's shim and its agent do:
// it waits for the launch, dials the worker stream with the launch's boot token, registers the
// session, serves the connection and reports the claim ready.
func (r *phaseEntryConvergeLive) servedClaimant(role claim.Role, session string) *convergeClaimant {
	r.t.Helper()
	c := &convergeClaimant{liveSession: r.register(role, session), prompts: make(chan shimwire.Prompt, 8)}
	c.sh.serve(c.serve)
	c.ready()
	return c
}

// serve reads every frame the daemon sends the shim until the connection closes or the test ends
// it (shim.serve): it answers the protocol negotiation and the state probe (idle, or streaming
// while a turn runs), and takes each prompt — answering it and starting the turn, as Oh My Pi
// does — for the step to read. It runs for the claim's whole life, because a step that left the
// connection unread would have the daemon's own requests time out and the task it was sending wait
// for a retry.
func (c *convergeClaimant) serve() {
	// The reader dialShim read the hello_ack through, which may already hold the frames after it.
	reader := c.sh.lines
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
			select {
			case c.prompts <- f:
			case <-c.sh.stopping:
				return
			}
		}
	}
}

// send writes one frame to the daemon; the reader and the step both write.
func (c *convergeClaimant) send(frame shimwire.Frame) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.sh.writer.WriteFrame(frame); err != nil {
		c.p.t.Errorf("send %s for %s: %v", frame.FrameType(), c.token, err)
	}
}

// startOwnTurn is the agent beginning a turn of its own, on no prompt of the daemon's: the turn
// the seeded implementer is in when the proof reaches it. It returns once the daemon has recorded
// the claim working.
func (c *convergeClaimant) startOwnTurn() {
	t := c.p.t
	t.Helper()
	c.mu.Lock()
	c.turn, c.prompt = true, ""
	c.mu.Unlock()
	c.send(shimwire.AgentStart{})
	testwait.Eventually(t, fmt.Sprintf("the %s's turn running", c.role), func() bool {
		return c.p.d.claim(c.token).State == string(supervise.StateWorking)
	})
}

// endTurn is the agent finishing its turn: the agent_end, and behind it the prompt's own result,
// which is what names the prompt the turn belonged to (shimwire.PromptResult) and retires the
// task. It returns once the daemon has recorded the claim out of its turn.
func (c *convergeClaimant) endTurn() {
	t := c.p.t
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
		return c.p.d.claim(c.token).State == string(supervise.StateIdle)
	})
}

// awaitPrompt waits for the next task the daemon sends this claim, within bound, and returns it.
func (c *convergeClaimant) awaitPrompt(what string, bound time.Duration) shimwire.Prompt {
	t := c.p.t
	t.Helper()
	select {
	case prompt := <-c.prompts:
		return prompt
	case <-time.After(bound):
		t.Fatalf("the %s was sent no task within %s: %s", c.role, bound, what)
		return shimwire.Prompt{}
	}
}

// drainPrompts takes every prompt the reader has already handed this claim and the steps have not
// read, oldest first, and returns them. It never waits: a prompt still on its way is left for the
// next awaitPrompt, so a step drains only where no task of the claim can be on its way, as run
// two does while it holds the lane's lock (aDeletedStartRowLosesNoWork).
func (c *convergeClaimant) drainPrompts() []shimwire.Prompt {
	var drained []shimwire.Prompt
	for {
		select {
		case prompt := <-c.prompts:
			drained = append(drained, prompt)
		default:
			return drained
		}
	}
}

// Check 1 of run one. The seeded implementer completes implementing from a turn of its own, and
// goes on in that turn, as an agent does that has more to do once it has reported: the completion
// hands testing to the tester. The transition raises the tester's phases row to entry 1 and writes
// that entry's own task in its own transaction (workflow.Engine.enter), and the start it writes
// creates the tester's claim and queues it that task. The launch of that claim does not run: the
// boot's relaunch of the implementer spent the burst of the pace this daemon is held to, so the
// daemon's wake pacing has not let this one run yet and the start waits for it, which the state
// names (api.State.WakesWaiting).
func (r *phaseEntryConvergeLive) theStartWaitsForTheLaunch() {
	t := r.t
	tester := r.token(claim.RoleTester)
	r.imp = r.servedClaimant(claim.RoleImplementer, "ses_implementer")
	r.note("the implementer is registered and ready on the launch this boot made for the claim the fixture left mid-launch; that launch spent the burst of %d wake(s) a minute in bursts of %d",
		convergeWakeRate, convergeWakeBurst)
	r.imp.startOwnTurn()
	if status, failure := r.imp.complete("implemented it", convergeFirstHead, "", r.imp.handoff("implement")); status != http.StatusOK {
		t.Fatalf("the implementer's completion = %d %s %s, want 200", status, failure.Code, failure.Error)
	}
	r.note("drove POST /legion/v1/handoff/complete for the implementer at %s from a turn of its own, which it goes on in: 200", convergeFirstHead)
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
	if _, held = r.waitingLaunch(tester); !held || r.launched(tester) {
		t.Fatalf("the launch of %s has run while this check read the start, want it still held by the pace", tester)
	}
	r.queued = queued.ID
	r.note("read back: the tester's claim holds delivery %s, the %s phase's task, with phaseEntry %d and givenEntry %d; its launch is due at %s and no process has been started for it",
		queued.ID, queued.Phase, phaseEntry, givenEntry, waiting.DueAt.UTC().Format(time.RFC3339Nano))
}

// Check 2 of run one. The pace lets the tester's launch run, and the tester comes up on its own
// worker-stream connection with the task of entry 1 still held on its claim and sent nowhere: the
// task adopts the working copy every role of the issue shares, and the daemon sends no such task
// while another role of the issue is in a turn (supervise's sendPending), as the implementer is.
// The issue's phase then changes twice while the claim holds that task. The tester, in a turn of
// its own that no task of the daemon's began, asks for the work back with its own backward move
// to implementing, which raises the implementer's entry; that entry's task waits on the
// implementer's claim, since the implementer is in its turn and the tester in its own. The
// implementer, still in the turn its first completion was made from, pushes a head of its own and
// completes there — its claim has served no entry, so its completion is of the entry the task it
// holds carries (supervise's serving) — which hands testing out again, raises the tester's row to
// entry 2 and writes that entry's own task. The claim still holds the task of entry 1 (supervise's
// queue refuses a second delivery while one is pending), so the start row carrying entry 2 waits
// behind it.
func (r *phaseEntryConvergeLive) phaseChangesWhileTheClaimHoldsItsTask() {
	t := r.t
	tester, implementer := r.token(claim.RoleTester), r.token(claim.RoleImplementer)
	r.awaitLaunch(tester)
	r.note("the pace let the tester's launch run: the fake runtime started its process")
	r.test = r.servedClaimant(claim.RoleTester, "ses_tester")
	time.Sleep(liveSettle)
	if sent := r.test.drainPrompts(); len(sent) != 0 {
		t.Fatalf("the tester was sent %s while the implementer's turn ran, want no task: one that adopts the working copy waits for that turn", sent[0].DeliveryID)
	}
	ready := r.d.claim(tester)
	if (ready.State != string(supervise.StateReady) && ready.State != string(supervise.StateIdle)) ||
		ready.Pending == nil || ready.Pending.ID != r.queued || ready.Pending.DeliveredAt != nil {
		t.Fatalf("the tester is %s holding %+v, want it ready with the task of entry 1 (%s) held and unsent", ready.State, ready.Pending, r.queued)
	}
	r.note("read back over %s: the tester is %s on its own connection, holding delivery %s unsent while the implementer's turn runs", liveSettle, ready.State, r.queued)

	// The first phase change: the tester's own move back, from a turn of its own.
	r.test.startOwnTurn()
	if status, failure := r.test.moveBack(phase.Implementing, convergeTesterReason); status != http.StatusOK {
		t.Fatalf("the tester's backward move to implementing = %d %s %s, want 200", status, failure.Code, failure.Error)
	}
	r.waitFor("the issue back in implementing", func(got api.Issue) bool { return got.Phase == phase.Implementing })
	var owed *api.DeliveryView
	testwait.Eventually(t, "the implementer's claim queued the task of the entry the move back raised", func() bool {
		owed = r.d.claim(implementer).Pending
		return owed != nil && owed.Phase == phase.Implementing
	})
	if owed.DeliveredAt != nil || len(r.imp.drainPrompts()) != 0 {
		t.Fatalf("the implementer was sent its new entry's task %+v while it and the tester were in their turns, want it held", owed)
	}
	r.note("drove POST /legion/v1/phase/backward for the tester, to implementing, from a turn of its own: 200; %s is in implementing, and the implementer's claim holds the task %s of that entry unsent",
		r.key, owed.ID)

	// The second: the implementer, still in its turn, pushes a head of its own and completes there.
	r.push(convergeFirstHead, convergeSecondHead, "fix the fault the tests showed")
	if status, failure := r.imp.complete("fixed the fault the tests showed", convergeSecondHead, "", r.imp.handoff("implement")); status != http.StatusOK {
		t.Fatalf("the implementer's second completion = %d %s %s, want 200", status, failure.Code, failure.Error)
	}
	r.note("drove the implementer's push of %s and GitHub's synchronize of #%d to it, then its completion there, still in its first turn: 200", convergeSecondHead, r.number)
	r.waitFor("the issue in testing again", func(got api.Issue) bool { return got.Phase == phase.Testing })
	testwait.Eventually(t, "the tester's phases row raised to entry 2", func() bool {
		phaseEntry, _ := r.entries()
		return phaseEntry == 2
	})
	phaseEntry, givenEntry := r.entries()
	if now := r.d.claim(tester).Pending; now == nil || now.ID != r.queued || now.DeliveredAt != nil {
		t.Fatalf("the tester holds %+v, want the task of entry 1 (%s) it was queued before the phase changed, still unsent", now, r.queued)
	}
	if phaseEntry != 2 || givenEntry != 1 {
		t.Fatalf("the state reads phaseEntry %d, givenEntry %d where the tester's claim holds entry 1's task, want 2 and 1", phaseEntry, givenEntry)
	}
	starts := r.startRows()
	if len(starts) != 1 || starts[0].role != claim.RoleTester || starts[0].entry != 2 {
		t.Fatalf("the start rows of %s = %+v, want the one the second testing entry wrote for the tester", r.key, starts)
	}
	r.note("read back: phaseEntry %d, givenEntry %d — the tester still holds entry 1's task (%s) unsent, and the start row of entry 2 (#%d, %d attempt(s)) waits: %s",
		phaseEntry, givenEntry, r.queued, starts[0].id, starts[0].attempts, starts[0].lastErr)
}

// Check 3 of run one. The tester's turn ends, and the task of entry 1 still waits, since the
// implementer's turn runs. Then the implementer's turn ends: the task its move back raised, queued
// for the implementing phase the issue has left, is dropped unsent (supervise's phaseHolds), and
// the tester is sent the task its claim held throughout, entry 1's. The completion it reports for
// that work is refused 409 HANDOFF_STALE_ENTRY, which names the entry the phase is on and says
// that entry's task is owed and arrives by itself, and records nothing. The turn ends, the task of
// entry 2 reaches the same connection, and the completion reported for it is taken: the issue moves
// to reviewing.
func (r *phaseEntryConvergeLive) theHeldTaskIsRefusedAndTheEntryIsWorked() {
	t := r.t
	implementer := r.token(claim.RoleImplementer)
	r.test.endTurn()
	r.note("the tester's own turn ended; the task of entry 1 still waits, since the implementer is in its turn")
	r.imp.endTurn()
	r.note("the implementer's turn ended")
	first := r.test.awaitPrompt("the task of entry 1 the tester's claim held", convergeOwedBound)
	if first.DeliveryID != r.queued {
		t.Fatalf("the tester was sent delivery %s, want %s, the task of entry 1 its claim held while the phase changed", first.DeliveryID, r.queued)
	}
	testwait.Eventually(t, "the implementer's task of the implementing phase dropped unsent", func() bool {
		return r.d.claim(implementer).Pending == nil
	})
	if sent := r.imp.drainPrompts(); len(sent) != 0 {
		t.Fatalf("the implementer was sent %s, want no task: the one its entry held was for implementing, which the issue has left", sent[0].DeliveryID)
	}
	phaseEntry, givenEntry := r.entries()
	if phaseEntry != 2 || givenEntry != 1 {
		t.Fatalf("the state reads phaseEntry %d, givenEntry %d where the tester works entry 1's task, want 2 and 1", phaseEntry, givenEntry)
	}
	r.note("read back: the tester was sent %s on its own connection (%s) — the work of entry %d, with the phase on entry %d; the implementer's implementing task was dropped unsent",
		first.DeliveryID, firstLine(first.Message), givenEntry, phaseEntry)

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
// lock across the move that writes the row, so no lane can have run it. The move is the
// reviewer's own backward move to implementing, from the turn its review task began and under a
// grant its session minted (behind_entry_fence_live_test.go's moveBack): the issue stands in
// reviewing, and the engine takes a backward move from no role but the one running the issue's
// phase (workflow's Engine.backward, BACKWARD_REFUSED). The move raises the implementer's entry
// and writes that entry's start row in the transaction that applies it, as every transition
// writes its start. The reviewer is brought up before the lock is taken, since no start of this
// issue runs while that lock is held. With the row gone, the implementer's phases row names an
// entry its claim was never given, and no queued start carries it — which is what the daemon
// reads as work owed (workflow.OwedEntry) — and no fact of the project arrives after the delete.
// The entry sweep is what sends it: on its own period the daemon applies an intake.EntrySweep and
// admission sends the role the work the entry its phases row names stands for (admit's
// resumeSlottedWorkers, workflow's SendOwedEntry).
//
// The implementer's connection may hold a prompt no step read. Run one leaves none there, since
// the task its move back raised was dropped unsent, but such a prompt, read as the sweep's
// delivery, would let this run pass on a daemon that re-sends nothing. The step therefore drains
// every prompt the connection has received before it lets the lock go, while no start of the issue
// can be on its way, and then holds that the task which arrives was minted from an outbox row
// written after the deleted one: an `outbox:<id>` delivery whose row id is above the deleted row's,
// which outbox ids, drawn from one sequence, give only to a row the sweep wrote after the delete,
// and that it is the task of the entry the reviewer's move raised, its reason behind `Reason:`.
func (r *phaseEntryConvergeLive) aDeletedStartRowLosesNoWork() {
	t := r.t
	ctx := context.Background()
	implementer := r.token(claim.RoleImplementer)
	before := r.d.claim(implementer)
	if before.State != string(supervise.StateIdle) || before.Pending != nil {
		t.Fatalf("the implementer is %s holding %+v, want it idle with no task: the row this step deletes is the only work owed to it",
			before.State, before.Pending)
	}

	// The reviewer, on the start the move to reviewing wrote. Its own backward move is what
	// raises the implementer's entry, from the turn its review task began.
	r.rev = r.servedClaimant(claim.RoleReviewer, "ses_reviewer")
	r.rev.awaitPrompt("the reviewer's task", convergeOwedBound)
	r.note("the reviewer came up on the start the move to reviewing wrote, and took its task, before the lock below")

	// The lane's own lock, held on a connection of the driver's: while this transaction lives, no
	// lane of this daemon can claim a lifecycle row of the issue, so the row the move writes below
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
	r.note("holding the advisory lock a lane takes before it claims a lifecycle row of %s, so the start row the move writes cannot be run", r.key)

	if status, failure := r.rev.moveBack(phase.Implementing, convergeReviewerReason); status != http.StatusOK {
		t.Fatalf("the reviewer's backward move to implementing = %d %s %s, want 200", status, failure.Code, failure.Error)
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
	for _, left := range r.imp.drainPrompts() {
		r.note("drained a prompt the implementer's connection received before the delete and no step read: %s (%s)",
			left.DeliveryID, firstLine(left.Message))
	}
	if err := lock.Rollback(ctx); err != nil {
		t.Fatalf("let the lane's lock go: %v", err)
	}
	locked = false
	r.note("drove POST /legion/v1/phase/backward for the reviewer, to implementing, from its review task's turn, and deleted the start row that move wrote (#%d, entry %d, %d attempt(s)) before any lane ran it; the state reads phaseEntry %d, givenEntry %d",
		deleted.id, deleted.entry, deleted.attempts, phaseEntry, givenEntry)
	if deleted.attempts != 0 || phaseEntry != deleted.entry || givenEntry >= phaseEntry {
		t.Fatalf("the deleted row %+v left the state at phaseEntry %d, givenEntry %d, want an untried row of the entry the phase is on with the claim behind it",
			deleted, phaseEntry, givenEntry)
	}
	if left := r.startRows(); len(left) != 0 {
		t.Fatalf("the outbox holds %+v after the delete, want no start row of %s at all", left, r.key)
	}

	// Nothing of the project is published from here on: what sends the entry's work is the
	// daemon's own sweep.
	task := r.imp.awaitPrompt(fmt.Sprintf("the work of entry %d, which the implementer's phases row names and whose start row %s%d this step deleted",
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
	if !strings.Contains(task.Message, "Reason: "+convergeReviewerReason) {
		t.Fatalf("the implementer was sent %q, want the task entry %d names, which the reviewer's move back wrote", task.Message, deleted.entry)
	}
	testwait.Eventually(t, "the implementer given the entry its phases row names", func() bool {
		phaseEntry, givenEntry = r.entries()
		return phaseEntry == deleted.entry && givenEntry == deleted.entry
	})
	r.note("read back: with no fact of the project arriving, the sweep (period %s here, %s in the shipped binary) sent the implementer %s (%s) on its own connection, row %d above the deleted row %s%d, and the state reads phaseEntry %d, givenEntry %d",
		convergeEntrySweep, entrySweepInterval, task.DeliveryID, firstLine(task.Message), row, outboxDeliveryPrefix, deleted.id, phaseEntry, givenEntry)
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
