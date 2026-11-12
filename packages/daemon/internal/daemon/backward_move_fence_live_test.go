//go:build e2e

// The live proof's driver for the one reading that outranks the heads in the completion route's
// entry fence: the move that raised the entry the phase stands on. A completion of a phase entry
// the issue has moved past is taken when the commit it names is the head the daemon's record
// holds, because a verdict is a fact about a head and the phase was entered again at that same
// head (api/handoff.go's behindAtRecordedHead) — unless the move that entered it again moved the
// issue back. Such a move sends the work back at the head that is already there, so the two heads
// read alike and the heads alone would take a verdict over an entry that exists to have the work
// done again. The record says which move it was (record.PhaseRow.EntryMovedBack, written by the
// transition that raises the entry), and the fence reads it.
//
// So this proof drives both readings at one head that never moves, against one booted daemon: the
// control is the behind completion the head takes, answered 200, and the subject is the same
// completion at the same head after the reviewer has moved the work back into testing, answered
// 409 HANDOFF_STALE_ASSIGNMENT with nothing recorded. The move back is the real one — the
// reviewer's own request over POST /legion/v1/phase/backward, from reviewing, under a grant its
// registered session minted — because the engine takes a backward move from no role but the one
// running the issue's phase (workflow's Engine.backward, BACKWARD_REFUSED). That registered
// reviewer is why this proof is a rig of its own rather than two more steps of
// ci_red_no_takeover_live_test.go, whose last step turns on a reviewer no session holds.
// scripts/e2e/backward-move-fence.sh builds this driver with `-tags e2e` and owns the nats-server
// and the PostgreSQL it runs against.
//
// The daemon is the whole daemon: run, the function `legion start` calls, through this package's
// own whole-daemon harness (startDaemonWithin), so the proof exercises the real configuration, the
// real store and its migrations, the real HTTP API, the real claim supervision and its worker
// stream, the real openWorkflow, the real JetStream consumers and the real outbox lanes. Facts
// enter only as production's do: GitHub envelopes on the ENVOY_NOTIFICATIONS stream for the
// settlement that sends the work back; the daemon's own HTTP routes for each role's registration,
// readiness, grants, completions and the reviewer's backward move; and the daemon's worker stream
// for each role's turns, which this driver speaks as `legion worker-shim` does (shimwire frames).
// Almost every assertion is a read of GET /legion/v1/state or of the operator's claim listing.
// What neither shows is the tester's phases row, which the proof reads from the daemon's own
// store for two things: the fence's own input, whether the entry the row stands on was raised by
// a move back, and what a refusal must leave unwritten there, the verdict and the handoff commit
// of the completion step 5 drives.
//
// What the proof stands in for lies outside the rule. The broker is a native nats-server the
// script starts and names in LEGION_E2E_NATS_URL. GitHub is backwardFenceGitHub, a REST stand-in
// for the one pull request the proof drives, whose base branch requires the check `ci` and whose
// head never moves: nothing is pushed in this proof at all, which is what leaves the two heads
// equal. Dispatch is the board the package's whole-daemon live proofs share (liveDispatch, in
// live_proof_test.go), and Envoy's listener is that harness's noticeListener. The two Apps' tokens
// are workflowTokenRecorder's placeholders. No agent runs: every claim is launched by the
// package's fake runtime, which says it provisions each workspace in the claim's own pod, as the
// Agent Sandbox runtime does, so the daemon clones nothing from GitHub, and each role the proof
// acts as is driven over the daemon's own worker stream and HTTP routes. The issue, its pull
// request, its slot and the implementer's claim are seeded through the store, as every daemon test
// seeds its fixture; what is driven is the change.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
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

// backwardFenceCheck is the one check the pull request's base branch requires, and so the only one
// a red verdict can stand on; backwardFenceBase is that base branch, the repository's default.
const (
	backwardFenceCheck = "ci"
	backwardFenceBase  = "main"
)

// backwardFenceHead is the one head this proof drives, and it never moves: every completion of it
// is a verdict on that head, and the entry fence reads each against the head the record holds,
// which is this one throughout. Nothing is pushed, so the proof's two behind completions differ in
// nothing a reading of the heads can see — only in the move that raised the entry standing when
// each was reported.
const backwardFenceHead = "bm1"

// backwardMoveFenceLive is one run of the proof: the booted daemon, the one issue it drives, the
// GitHub stand-in its facts are read against, and the three roles the driver acts as over the
// worker stream.
type backwardMoveFenceLive struct {
	liveProof
	rt     *fake.Runtime
	built  *built
	github *backwardFenceGitHub
	// key is the issue, its own tree, and number its pull request's — the run's own, since the
	// store a pod re-runs the proof against holds every earlier run's pull requests and a fact
	// names a pull request by its repository and number.
	key    string
	number int
	// imp, test and review are the implementer, the tester and the reviewer as their agents reach
	// the daemon. The reviewer is registered and holds a grant, because the move back is its own.
	imp, test, review *backwardFenceClaimant
	// seq numbers each settlement, so no two are the same report (classify.sameSettlement).
	seq int64
	// entry is the tester's entry each step leaves behind, so the step after it reads what the
	// one before recorded rather than a number of its own.
	entry int64
}

// TestABehindCompletionIsRefusedWhenAMoveBackRaisedTheEntryLive drives the five steps of the proof
// against one booted daemon, in the order each step's state follows from the last: the tester
// working the assignment its phase handed it, the red and the implementer's answer that hand
// testing out again at the head that did not move, the behind completion at that head taken, the
// registered reviewer's own move back into testing, and the same completion at the same head
// refused because a move back raised the entry the phase now stands on.
func TestABehindCompletionIsRefusedWhenAMoveBackRaisedTheEntryLive(t *testing.T) {
	fixtureenv.Live(t)
	r := newBackwardMoveFenceLive(t)
	r.step("step1-the-tester-works-the-assignment-its-phase-handed-it", r.testerWorksItsAssignment)
	r.step("step2-testing-is-handed-out-again-at-the-head-that-did-not-move", r.testingIsHandedOutAgain)
	r.step("step3-the-control-the-behind-completion-at-that-head-is-taken", r.behindAtTheHeadIsTaken)
	r.step("step4-the-registered-reviewer-moves-the-work-back-into-testing", r.reviewerMovesTheWorkBack)
	r.step("step5-the-same-completion-at-the-same-head-is-refused-after-the-move-back", r.behindAfterTheMoveBackIsRefused)
}

// newBackwardMoveFenceLive boots the daemon over the script's broker with the issue already
// recorded: one root implementing on an open pull request whose base requires ci, holding its
// tree's admission slot, and its implementer's claim as a launch a previous daemon began and never
// recorded, which this boot relaunches. That last one is how the proof reaches a live implementer
// session, serving run 1, without an operator's spawn.
func newBackwardMoveFenceLive(t *testing.T) *backwardMoveFenceLive {
	t.Helper()
	proof := newLiveProof(t, "scripts/e2e/backward-move-fence.sh")
	number, err := strconv.ParseInt(randomSuffix(t)[:6], 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	r := &backwardMoveFenceLive{
		liveProof: proof, rt: fake.NewRuntime(), built: &built{},
		key: proof.cfg.Project + "-1", number: int(number) + 1,
	}
	r.github = newBackwardFenceGitHub(t, r.key, r.number)
	r.seed()
	// The runtime provisions each claim's workspace itself (fake.Runtime's InPod), so a start
	// reaches the fake's launch without the daemon cloning acme/widgets from github.com, which the
	// tmux answer would have it try.
	r.rt.InPod = true
	o := fakeRuntime(r.rt, r.built)
	o.workflowTokens = &workflowTokenRecorder{}
	o.githubAPI = r.github.url
	r.d = startDaemonWithin(t, r.cfg, o, r.log(), 60*time.Second)
	return r
}

// seed records the issue as the daemon would hold it at the moment its implementer began work: a
// root handed to Legion, implementing, holding its tree's admission slot, its lifecycle open under
// the workflow, on an open pull request at the proof's one head whose base requires ci, and its
// implementer's claim mid-launch, serving run 1. No phases row is seeded: every entry the fence
// reads here is raised live, by the starts the completions and the move back make.
func (r *backwardMoveFenceLive) seed() {
	t := r.t
	t.Helper()
	ctx := context.Background()
	r.board.show(r.key, "in_progress")
	r.write("the issue, its pull request and its slot", func(tx pgx.Tx) error {
		if err := r.records.PutIssue(ctx, tx, record.Issue{
			Key: r.key, Repo: liveRepo, DefaultBranch: backwardFenceBase, GitHubRepo: liveRepo, Tree: r.key, Project: r.cfg.Project,
			Title: "the issue the reviewer sends back", Phase: phase.Implementing, Generation: 1, Status: "in_progress",
			DispatchStatus: "in_progress", Rank: "U", LastDispatchSeq: 1, HandedOver: true,
		}); err != nil {
			return err
		}
		if err := r.records.PutPullRequest(ctx, tx, record.PullRequest{
			State: record.PullRequestOpen, Issue: r.key, Repo: liveRepo, Number: r.number, Branch: "legion/" + r.key,
			HeadSHA: backwardFenceHead, Required: []string{backwardFenceCheck}, Failing: []string{},
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
		r.key, r.number, backwardFenceHead, backwardFenceBase, backwardFenceCheck)
}

// write runs body in one transaction against the daemon's own database, failing the test with what
// it was seeding on an error.
func (r *backwardMoveFenceLive) write(what string, body func(tx pgx.Tx) error) {
	r.t.Helper()
	if err := r.st.Tx(context.Background(), body); err != nil {
		r.t.Fatalf("seed %s: %v", what, err)
	}
}

// token is the claim token of role on the proof's issue, as the daemon spells it.
func (r *backwardMoveFenceLive) token(role claim.Role) claim.Token {
	r.t.Helper()
	token, err := claim.NewToken(r.project, r.key, role)
	if err != nil {
		r.t.Fatal(err)
	}
	return token
}

// awaitLaunch waits for the running daemon to launch token's claim and returns that launch.
func (r *backwardMoveFenceLive) awaitLaunch(token claim.Token) runtime.SpawnSpec {
	r.t.Helper()
	testwait.Eventually(r.t, "the launch of "+string(token), func() bool {
		for _, call := range r.rt.Calls() {
			if (call.Method == "Spawn" || call.Method == "Resume") && call.Spec.Claim == token {
				return true
			}
		}
		return false
	})
	return lastLaunch(r.t, r.rt, token)
}

// waitFor polls the state until want is true of the issue, failing with what it last saw.
func (r *backwardMoveFenceLive) waitFor(what string, want func(api.Issue) bool) api.Issue {
	r.t.Helper()
	var last api.Issue
	testwait.Eventually(r.t, what, func() bool {
		last = r.issue(r.key)
		return want(last)
	})
	return last
}

// phaseRow is role's phases row as the daemon's own store holds it. It is read for one thing no
// route shows, and that thing is the fence's own input: whether the start that raised the entry
// the row stands on moved the issue back (record.PhaseRow.EntryMovedBack). The entry number beside
// it is on the state route as well (api.Issue.PhaseEntry), and every step reads it there.
func (r *backwardMoveFenceLive) phaseRow(role claim.Role) record.PhaseRow {
	t := r.t
	t.Helper()
	ctx := context.Background()
	var row record.PhaseRow
	if err := r.st.Tx(ctx, func(tx pgx.Tx) error {
		rows, err := r.records.Phases(ctx, tx, r.key)
		if err != nil {
			return err
		}
		row = record.PhaseRowOf(rows, role)
		return nil
	}); err != nil {
		t.Fatalf("read the %s's phases row of %s: %v", role, r.key, err)
	}
	return row
}

// backwardFenceClaimant is one role's claim as its agent reaches it: the shim that dialled the
// worker stream with the boot token of the launch the fake runtime recorded, the registration that
// exchanged it for the session's capability secret, and the prompt whose turn is running.
type backwardFenceClaimant struct {
	r       *backwardMoveFenceLive
	token   claim.Token
	role    claim.Role
	session string
	secret  string
	sh      *shim
	// turn is the id of the prompt the running turn answers, "" for a turn of the agent's own.
	turn string
}

// claimant brings the launched claim of role to ready, as its pane's shim and its agent do: it
// waits for the launch, dials the worker stream with the launch's boot token, registers the
// session and reports it ready.
func (r *backwardMoveFenceLive) claimant(role claim.Role, session string) *backwardFenceClaimant {
	t := r.t
	t.Helper()
	token := r.token(role)
	launch := r.awaitLaunch(token)
	r.built.mu.Lock()
	address := r.built.address
	r.built.mu.Unlock()
	sh := dialShim(t, address, launch.BootToken)
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
	if status, body := r.d.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: token, SessionID: session, Secret: registered.Secret, Generation: launch.Generation,
	}, false); status != http.StatusNoContent {
		t.Fatalf("ready %s = %d %s, want 204", token, status, body)
	}
	return &backwardFenceClaimant{r: r, token: token, role: role, session: session, secret: registered.Secret, sh: sh}
}

// takeTask is the agent taking the daemon's next task: it reads the prompt the daemon sends,
// acknowledges it as Oh My Pi does and starts the turn that works it, and returns once the daemon
// has recorded the claim working on that delivery.
func (c *backwardFenceClaimant) takeTask(want string) shimwire.Prompt {
	t := c.r.t
	t.Helper()
	prompt := c.sh.prompt()
	if !strings.Contains(prompt.Message, want) {
		t.Fatalf("the %s was prompted %q, want a task saying %q", c.role, prompt.Message, want)
	}
	c.sh.send(shimwire.Response{ID: prompt.ID, Command: shimwire.TypePrompt, Success: true})
	c.startTurn(prompt.ID)
	return prompt
}

// startTurn is the agent beginning a turn, on the prompt id when the daemon sent one and on
// nothing when the turn is its own. It returns once the daemon has recorded the claim working.
func (c *backwardFenceClaimant) startTurn(prompt string) {
	t := c.r.t
	t.Helper()
	c.sh.send(shimwire.AgentStart{})
	c.turn = prompt
	testwait.Eventually(t, fmt.Sprintf("the %s's turn running", c.role), func() bool {
		return c.r.d.claim(c.token).State == string(supervise.StateWorking)
	})
}

// endTurn is the agent finishing its turn: the agent_end, and behind it the prompt's own result,
// which is what names the prompt the turn belonged to (shimwire.PromptResult) and retires the
// task. It returns once the daemon has recorded the claim out of its turn.
func (c *backwardFenceClaimant) endTurn() {
	t := c.r.t
	t.Helper()
	c.sh.send(shimwire.AgentEnd{})
	if c.turn != "" {
		c.sh.send(shimwire.PromptResult{ID: c.turn, AgentInvoked: true})
	}
	c.turn = ""
	testwait.Eventually(t, fmt.Sprintf("the %s out of its turn", c.role), func() bool {
		return c.r.d.claim(c.token).State == string(supervise.StateIdle)
	})
}

// grant is a capability grant minted for this session now, as its `legion` tool mints one per
// operation.
func (c *backwardFenceClaimant) grant() string {
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
func (c *backwardFenceClaimant) handoff(word string) string {
	return c.r.board.handoff(c.r.key, word, c.session)
}

// complete reports this role's completion of the phase it works, from the turn it is in, naming
// the handoff record of that phase this claim wrote, and returns the status and the refusal the
// daemon answered. Every record the board seeds is version 1 of its own id, so each completion
// names a record of its own and no two are the same one.
func (c *backwardFenceClaimant) complete(summary, commit, verdict, handoffID string) (int, api.Failure) {
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

// moveBack is this role requesting the issue's move back to an earlier phase, from the turn it is
// in and under a grant of its own, as the `legion` tool's request_backward_move does. The engine
// takes it from the role running the issue's phase and from no other (workflow's Engine.backward,
// BACKWARD_REFUSED), which is why the reviewer's claim here is registered and live.
func (c *backwardFenceClaimant) moveBack(to phase.Phase, reason string) (int, api.Failure) {
	t := c.r.t
	t.Helper()
	status, body := c.r.d.request(http.MethodPost, "/legion/v1/phase/backward",
		api.PhaseBackwardRequest{GrantID: c.grant(), To: to, Reason: reason}, false)
	if status == http.StatusOK {
		return status, api.Failure{}
	}
	var failure api.Failure
	if err := json.Unmarshal(body, &failure); err != nil {
		t.Fatalf("decode the %s's refused backward move %s: %v", c.role, body, err)
	}
	return status, failure
}

// Step 1. The seeded implementer completes implementing at the proof's one head, which hands the
// testing phase to the tester with an entry of its own and starts it. The tester registers, takes
// that task and works it: the turn every completion below is reported from.
func (r *backwardMoveFenceLive) testerWorksItsAssignment() {
	t := r.t
	r.imp = r.claimant(claim.RoleImplementer, "ses_implementer")
	r.note("the implementer is registered and ready on the launch this boot made for the claim the fixture left mid-launch")
	r.imp.startTurn("")
	if status, failure := r.imp.complete("implemented it", backwardFenceHead, "", r.imp.handoff("implement")); status != http.StatusOK {
		t.Fatalf("the implementer's completion = %d %s %s, want 200", status, failure.Code, failure.Error)
	}
	r.imp.endTurn()
	r.note("drove POST /legion/v1/handoff/complete for the implementer at %s, then its agent_end: 200", backwardFenceHead)
	got := r.waitFor("the issue in testing", func(got api.Issue) bool { return got.Phase == phase.Testing })

	r.test = r.claimant(claim.RoleTester, "ses_tester")
	prompt := r.test.takeTask("Phase: testing")
	working := r.d.claim(r.test.token)
	if working.State != string(supervise.StateWorking) || working.Pending == nil || working.Pending.ConfirmedAt == nil {
		t.Fatalf("the tester is %s holding %+v, want working on a confirmed delivery of its testing task", working.State, working.Pending)
	}
	r.entry = got.PhaseEntry
	r.note("read back: %s is in %s on entry %d at head %s, and the tester is %s on the delivery %s of that entry's task (%s)",
		r.key, got.Phase, r.entry, got.PullRequest.Head, working.State, working.Pending.ID, firstLine(prompt.Message))
}

// Step 2. CI settles that head red, which sends the work back to the implementer, and the
// implementer answers it with nothing pushed: the branch is sound, the red was the run's own, and
// CI run again passes at the very same head. (That green is what lets the issue stand in testing
// at all: a phase the issue enters while the record holds its head red is sent straight back out
// of it by the red the record already holds.) So the forward move into testing raises a second
// entry for the tester at the very head its turn is testing, and that entry was raised by no move
// back. The tester's turn has run throughout and still serves the entry before it.
func (r *backwardMoveFenceLive) testingIsHandedOutAgain() {
	t := r.t
	r.publishSettlement(backwardFenceHead, true)
	r.note("drove CI's settlement of %s at %s with %s failed, while the tester's turn runs", r.key, backwardFenceHead, backwardFenceCheck)
	fix := r.imp.takeTask("CI is red at " + backwardFenceHead)
	r.note("read back: the implementer was prompted its fix task while the tester's turn ran (%s)", firstLine(fix.Message))
	r.publishSettlement(backwardFenceHead, false)
	r.waitFor("the record's verdict green at "+backwardFenceHead, func(got api.Issue) bool {
		return got.PullRequest != nil && got.PullRequest.ChecksVerdict == "green"
	})
	r.note("drove CI's settlement of %s at %s with %s passed, nothing pushed: the record holds that head green", r.key, backwardFenceHead, backwardFenceCheck)
	if status, failure := r.imp.complete("the red was the run's own; CI passes at the same head", backwardFenceHead, "", r.imp.handoff("implement")); status != http.StatusOK {
		t.Fatalf("the implementer's second completion = %d %s %s, want 200", status, failure.Code, failure.Error)
	}
	r.imp.endTurn()
	r.note("drove POST /legion/v1/handoff/complete for the implementer at %s again, with no push between, then its agent_end: 200", backwardFenceHead)

	got := r.waitFor("the tester's row on a second entry", func(got api.Issue) bool {
		return got.Phase == phase.Testing && got.PhaseEntry > r.entry
	})
	if got.PullRequest == nil || got.PullRequest.Head != backwardFenceHead {
		t.Fatalf("the record holds %+v for %s, want the pull request still at %s: nothing was pushed", got.PullRequest, r.key, backwardFenceHead)
	}
	if row := r.phaseRow(claim.RoleTester); row.Entry != got.PhaseEntry || row.EntryMovedBack {
		t.Fatalf("the tester's phases row = entry %d, moved back %t; want entry %d raised by no move back: implementing moves forward into testing",
			row.Entry, row.EntryMovedBack, got.PhaseEntry)
	}
	held := r.d.claim(r.test.token)
	if held.State != string(supervise.StateWorking) {
		t.Fatalf("the tester is %s, want it still working the turn it began in step 1", held.State)
	}
	r.note("read back: %s is in testing on entry %d, above the entry %d whose task the tester took, raised by no move back, with the record still at head %s; the tester is still %s on delivery %s",
		r.key, got.PhaseEntry, r.entry, got.PullRequest.Head, held.State, held.Pending.ID)
	r.entry = got.PhaseEntry
}

// Step 3. The control. The tester reports from that same turn, behind the entry its row now
// names, a verdict on the head the record holds. Nothing moved the issue back into testing, so
// the work the standing entry's task asks for is the work this turn did: the daemon takes the
// completion (behindAtRecordedHead) and the pass moves the issue to reviewing. Step 5 is this
// same completion at this same head with one thing changed, the move that raised the entry.
func (r *backwardMoveFenceLive) behindAtTheHeadIsTaken() {
	t := r.t
	if status, failure := r.test.complete("tested it at "+backwardFenceHead, backwardFenceHead, "pass", r.test.handoff("test")); status != http.StatusOK {
		t.Fatalf("the behind completion at the head the record holds = %d %s %s, want 200", status, failure.Code, failure.Error)
	}
	moved := r.waitFor("the issue in reviewing", func(got api.Issue) bool { return got.Phase == phase.Reviewing })
	r.note("read back: the completion reported from the turn of an earlier entry, at %s, the head the record holds, was taken 200: %s is in %s",
		backwardFenceHead, r.key, moved.Phase)
	held := r.d.claim(r.test.token)
	if held.State != string(supervise.StateWorking) {
		t.Fatalf("the tester is %s after its completion was taken, want it still in the turn it began in step 1: step 5 reports from it", held.State)
	}
	r.note("read back: the tester is still %s on delivery %s — the turn step 5 reports from", held.State, held.Pending.ID)
}

// Step 4. The reviewer's own move back. Its claim was started by the tester's pass; it registers,
// takes its review task and requests the move to testing from the turn that task began, with a
// grant its session minted. The engine takes a backward move from the role running the issue's
// phase alone, so this is the one way the proof can reach a move back into testing. Nothing is
// pushed with it: the head the record holds is the one the tester reported on in step 3, and the
// entry the move raises says it was raised by a move back.
func (r *backwardMoveFenceLive) reviewerMovesTheWorkBack() {
	t := r.t
	r.review = r.claimant(claim.RoleReviewer, "ses_reviewer")
	prompt := r.review.takeTask("Phase: reviewing")
	r.note("the reviewer is registered, ready and working the review task the tester's pass started it with (%s)", firstLine(prompt.Message))
	const reason = "the fix needs a test of its own; back to testing"
	if status, failure := r.review.moveBack(phase.Testing, reason); status != http.StatusOK {
		t.Fatalf("the reviewer's backward move to testing = %d %s %s, want 200", status, failure.Code, failure.Error)
	}
	r.note("drove POST /legion/v1/phase/backward for the reviewer, to testing: 200")

	got := r.waitFor("the issue back in testing on a third entry", func(got api.Issue) bool {
		return got.Phase == phase.Testing && got.PhaseEntry > r.entry
	})
	if got.PullRequest == nil || got.PullRequest.Head != backwardFenceHead {
		t.Fatalf("the record holds %+v for %s, want the pull request still at %s: nothing was pushed with the move back", got.PullRequest, r.key, backwardFenceHead)
	}
	row := r.phaseRow(claim.RoleTester)
	if row.Entry != got.PhaseEntry || !row.EntryMovedBack {
		t.Fatalf("the tester's phases row = entry %d, moved back %t; want entry %d raised by a move back: reviewing came back to testing",
			row.Entry, row.EntryMovedBack, got.PhaseEntry)
	}
	r.note("read back: %s is in testing on entry %d, raised by a move back (phases.entry_moved_back), with the record still at head %s — the same head the completion step 3 took was of",
		r.key, row.Entry, got.PullRequest.Head)
	r.entry = got.PhaseEntry
}

// Step 5. The subject. The tester reports the same completion from the same turn, at the same
// head, which is still the head the record holds — and this time the entry the phase stands on was
// raised by a move back, an entry that exists to have this phase's work done again. The daemon
// refuses it 409 HANDOFF_STALE_ASSIGNMENT, the refusal says the move back is what refused it
// however the two heads read, nothing is recorded, and the issue stays in testing.
func (r *backwardMoveFenceLive) behindAfterTheMoveBackIsRefused() {
	t := r.t
	status, failure := r.test.complete("tested it at "+backwardFenceHead, backwardFenceHead, "pass", r.test.handoff("test"))
	if status != http.StatusConflict || failure.Code != "HANDOFF_STALE_ASSIGNMENT" {
		t.Fatalf("the behind completion after the move back = %d %s %s, want 409 HANDOFF_STALE_ASSIGNMENT", status, failure.Code, failure.Error)
	}
	for _, want := range []string{
		"this completion is at commit " + backwardFenceHead + " while the pull request the record holds is at head " + backwardFenceHead,
		"the move that handed the phase out again moved the issue back, which asks for this phase's work to be done again, so no verdict on either head answers for it",
		"nothing was recorded",
	} {
		if !strings.Contains(failure.Error, want) {
			t.Fatalf("the refusal = %q, want it to say %q", failure.Error, want)
		}
	}
	r.note("read back: the same completion at the same head = 409 %s — %s", failure.Code, failure.Error)

	after := r.issue(r.key)
	if after.Phase != phase.Testing || after.PhaseEntry != r.entry {
		t.Fatalf("%s is in %s on entry %d after the refused completion, want testing on entry %d: a refusal records nothing",
			r.key, after.Phase, after.PhaseEntry, r.entry)
	}
	row := r.phaseRow(claim.RoleTester)
	if row.Verdict != "" || row.HandoffCommit != "" {
		t.Fatalf("the tester's phases row holds verdict %q at commit %q after the refusal, want neither: the refusal is before the fact and records nothing",
			row.Verdict, row.HandoffCommit)
	}
	r.note("read back: %s is still in testing on entry %d, and the tester's phases row holds no verdict and no commit of this completion — nothing was recorded",
		r.key, after.PhaseEntry)
}

// publishSettlement publishes CI's settlement of head on the pull request with the one check ci,
// failed or passed, as Envoy relays one (intake's decodeChecks reads it).
func (r *backwardMoveFenceLive) publishSettlement(head string, failed bool) {
	r.t.Helper()
	group := func(checks ...string) map[string]any {
		return map[string]any{"count": len(checks), "checks": append([]string{}, checks...)}
	}
	failedGroup, passedGroup := group(), group(backwardFenceCheck)
	if failed {
		failedGroup, passedGroup = passedGroup, failedGroup
	}
	r.seq++
	payload, err := json.Marshal(map[string]any{
		"kind": "checks", "repo": liveRepo, "number": strconv.Itoa(r.number), "sha": head,
		"check_runs": []map[string]any{{"name": backwardFenceCheck, "id": r.seq}}, "generation": 1,
		"snapshot": fmt.Sprintf("%s-%s-%d", r.key, head, r.seq), "settled_at": time.Now().UnixMilli(),
		"failed": failedGroup, "passed": passedGroup, "running": group(), "queued": group(), "cancelled": group(), "skipped": group(),
	})
	if err != nil {
		r.t.Fatalf("encode the settlement payload: %v", err)
	}
	subject := fmt.Sprintf("notifications.github.%s.pr.%d.checks", r.subjectRepo(), r.number)
	r.publish(subject, "github", fmt.Sprintf("checks-%d-%s-%d", r.number, head, r.seq), string(payload))
}

// backwardFenceGitHub is GitHub's REST API for acme/widgets as this proof's daemon reads it: the
// one pull request at the proof's one head, which nothing moves, with its base and
// `mergeable: true` (readPullRequest), its reviews as none (readReviews), the base branch's
// rulesets requiring ci (requiredchecks.Required's /rules/branches/<base>) and the branch itself
// as unprotected (its /branches/<base>, which a 404 would make an error that keeps the set last
// read). It also answers what a launch and a disarm read: any repository, with default branch
// main, and a GraphQL root that knows no pull request. Anything else is answered 404, so a read
// this proof did not foresee fails the step that waited on it rather than reaching github.com.
type backwardFenceGitHub struct {
	t      *testing.T
	url    string
	key    string
	number int
}

func newBackwardFenceGitHub(t *testing.T, key string, number int) *backwardFenceGitHub {
	t.Helper()
	g := &backwardFenceGitHub{t: t, key: key, number: number}
	g.url = githubtest.Server(t, githubtest.Spec{Handle: g.ServeHTTP})
	return g
}

func (g *backwardFenceGitHub) ServeHTTP(w http.ResponseWriter, req *http.Request) {
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
func (g *backwardFenceGitHub) answer(w http.ResponseWriter, path string) bool {
	rest, ours := strings.CutPrefix(path, "/repos/"+liveRepo)
	if !ours {
		// Any other repository is answered as the launch reads it: a name and its default branch.
		other, isRepository := strings.CutPrefix(path, "/repos/")
		if parts := strings.Split(other, "/"); isRepository && len(parts) == 2 {
			g.write(w, map[string]any{"full_name": other, "default_branch": backwardFenceBase})
			return true
		}
		return false
	}
	switch {
	case rest == "":
		g.write(w, map[string]any{"full_name": liveRepo, "default_branch": backwardFenceBase})
	case rest == "/rules/branches/"+backwardFenceBase:
		g.write(w, []map[string]any{
			{"type": "pull_request"},
			{"type": "required_status_checks", "parameters": map[string]any{
				"required_status_checks": []map[string]string{{"context": backwardFenceCheck}},
			}},
		})
	case rest == "/branches/"+backwardFenceBase:
		g.write(w, map[string]any{"name": backwardFenceBase, "protected": false})
	case rest == fmt.Sprintf("/pulls/%d/reviews", g.number):
		g.write(w, []any{})
	case rest == fmt.Sprintf("/pulls/%d", g.number):
		g.write(w, map[string]any{
			"number": g.number, "state": "open", "mergeable": true,
			"base": map[string]string{"ref": backwardFenceBase},
			"head": map[string]string{"ref": "legion/" + g.key, "sha": backwardFenceHead},
		})
	default:
		return false
	}
	return true
}

// write encodes value as the answer.
func (g *backwardFenceGitHub) write(w http.ResponseWriter, value any) {
	if err := json.NewEncoder(w).Encode(value); err != nil {
		g.t.Errorf("write the GitHub stand-in's answer: %v", err)
	}
}
