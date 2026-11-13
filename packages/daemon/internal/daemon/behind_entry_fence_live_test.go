//go:build e2e

// The live proof's driver for the completion route's entry fence on a completion behind its phase
// entry: a completion of an entry the issue has moved past is taken when the commit it names is the
// head the daemon's record holds, because a verdict is a fact about a head and the phase was
// entered again at that same head (api/handoff.go's behindAtRecordedHead), and is refused when it
// names any other commit or none, or when the move that entered the phase again moved the issue
// back. Such a move sends the work back at the head that is already there, so the two heads read
// alike and the heads alone would take a verdict over an entry that exists to have the work done
// again. The record says which move it was (record.PhaseRow.EntryMovedBack, written by the
// transition that raises the entry), and the fence reads it.
//
// So every completion the proof drives is of the tester's first entry, reported from a turn that
// began before any later entry's task reached the tester, and each reading has its control beside
// it. Steps 3, 4 and 5 are the same behind completion behind the same forward entry, naming the
// head the tester tested, which the record has left, then no commit at all, then the head the
// record holds: only the commit differs, and only the last is taken. Steps 5 and 7 are the same
// completion at the same head, which never moves between them: only what raised the entry
// differs, a forward move for step 5 and the registered reviewer's real move back for step 7, and
// only step 5 is taken. scripts/e2e/behind-entry-fence.sh builds this driver with `-tags e2e` and
// owns the nats-server and the PostgreSQL it runs against.
//
// The forward entry of step 2 is raised on the path the daemon has for it: the tester asks for the
// work back with its own backward move to implementing and ends its turn, and the implementer
// pushes a head of its own and completes, which hands testing out again. The tester's first turn
// cannot still be running then, because the daemon sends no role a task that adopts the issue's
// working copy while another role of the issue is in a turn (supervise's sendPending), so the
// implementer's task waits for that turn to end; the tester's completions come instead from a
// turn of its own, begun while the implementer works and so before the new entry's task could
// reach it. Neither of the moves the daemon makes on its own serves here. A CI red moves no issue,
// since the daemon judges no check, so the proof publishes no settlement; and the daemon reads a
// head's mergeability at its start, after a handoff, and for an approved review round in reviewing
// alone, so a head that starts conflicting with its base while the tester tests is not read until
// the next handoff (dispatch://INFRA-3164). The move back into testing is the reviewer's own
// request over POST /legion/v1/phase/backward, from reviewing, under a grant its registered
// session minted, because the engine takes a backward move from no role but the one running the
// issue's phase (workflow's Engine.backward, BACKWARD_REFUSED).
//
// The daemon is the whole daemon: run, the function `legion start` calls, through this package's
// own whole-daemon harness (startDaemonWithin), so the proof exercises the real configuration, the
// real store and its migrations, the real HTTP API, the real claim supervision and its worker
// stream, the real openWorkflow, the real JetStream consumers and the real outbox lanes. Facts
// enter only as production's do: GitHub envelopes on the ENVOY_NOTIFICATIONS stream for the
// implementer's push and its synchronize; the daemon's own HTTP routes for each role's
// registration, readiness, grants, completions and backward moves; and the daemon's worker stream
// for each role's turns, which this driver speaks as `legion worker-shim` does (shimwire frames).
// Almost every assertion is a read of GET /legion/v1/state or of the operator's claim listing.
// What neither shows is the tester's phases row, which the proof reads from the daemon's own store
// for two things: the fence's own input, whether the entry the row stands on was raised by a move
// back, and what a refusal must leave unwritten there, the verdict and the handoff commit of the
// completion it refused.
//
// What the proof stands in for lies outside the rule. The broker is a native nats-server the
// script starts and names in LEGION_E2E_NATS_URL. GitHub is behindFenceGitHub, a REST stand-in for
// the one pull request the proof drives, at the head the implementer last pushed. Dispatch is the
// board the package's whole-daemon live proofs share (liveDispatch, in live_proof_test.go), which
// holds the handoff record each completion names, and Envoy's listener is that harness's
// noticeListener. The two Apps' tokens are workflowTokenRecorder's placeholders. No agent runs:
// every claim is launched by the package's fake runtime, which says it provisions each workspace
// in the claim's own pod, as the Agent Sandbox runtime does, so the daemon clones nothing from
// GitHub, and each role the proof acts as is driven over the daemon's own worker stream and HTTP
// routes. The issue, its pull request, its slot and the implementer's claim are seeded through the
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

// behindFenceBase is the pull request's base branch, the repository's default.
const behindFenceBase = "main"

// The two heads the proof drives: the implementer's seeded head, which the tester's first turn
// tests, and the head the implementer pushes once the tester has sent the work back, which the
// record holds from step 2 to the end. Nothing is pushed after it, so steps 5 and 7 differ in
// nothing a reading of the heads can see.
const (
	behindFenceFirstHead  = "bf1"
	behindFenceSecondHead = "bf2"
)

// behindEntryFenceLive is one run of the proof: the booted daemon, the one issue it drives, the
// GitHub stand-in its facts are read against, and the three roles the driver acts as over the
// worker stream.
type behindEntryFenceLive struct {
	liveProof
	rt     *fake.Runtime
	built  *built
	github *behindFenceGitHub
	// key is the issue, its own tree, and number its pull request's — the run's own, since the
	// store a pod re-runs the proof against holds every earlier run's pull requests and a fact
	// names a pull request by its repository and number.
	key    string
	number int
	// imp, test and review are the implementer, the tester and the reviewer as their agents reach
	// the daemon. The reviewer is registered and holds a grant, because the move back is its own.
	imp, test, review *behindFenceClaimant
	// served is the entry whose task the tester's one turn took, and entry the entry the tester's
	// phases row stands on as the last step left it, so the step after it reads what the one
	// before recorded rather than a number of its own.
	served, entry int64
	// delivery is the delivery of the served entry's task, the last task the tester's turn took.
	delivery string
}

// TestABehindCompletionIsDecidedByItsHeadAndTheMoveThatRaisedTheEntryLive drives the seven steps
// of the proof against one booted daemon, in the order each step's state follows from the last: the
// tester working its first entry, the tester's own move back and the implementer's push and
// completion that hand testing out again by a forward move, the behind completion refused at the
// head the record has left, refused naming no commit, and taken at the head the record holds, the
// registered reviewer's own move back into testing, and the same completion at the same head
// refused because a move back raised the entry the phase now stands on.
func TestABehindCompletionIsDecidedByItsHeadAndTheMoveThatRaisedTheEntryLive(t *testing.T) {
	r := newBehindEntryFenceLive(t)
	r.step("the tester works its entry", r.testerWorksItsEntry)
	r.step("testing is entered again by a forward move", r.testingIsEnteredAgain)
	r.step("stale head refused", r.staleHeadRefused)
	r.step("behind with no head", r.behindWithNoHead)
	r.step("behind at the head", r.behindAtTheHead)
	r.step("the reviewer moves the work back", r.reviewerMovesTheWorkBack)
	r.step("behind after a backward move", r.behindAfterABackwardMove)
}

// newBehindEntryFenceLive boots the daemon over the script's broker with the issue already
// recorded: one root implementing on an open pull request, holding its tree's admission slot, and
// its implementer's claim as a launch a previous daemon began and never recorded, which this boot
// relaunches. That last one is how the proof reaches a live implementer session, serving run 1,
// without an operator's spawn.
func newBehindEntryFenceLive(t *testing.T) *behindEntryFenceLive {
	t.Helper()
	proof := newLiveProof(t, "scripts/e2e/behind-entry-fence.sh")
	number, err := strconv.ParseInt(randomSuffix(t)[:6], 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	r := &behindEntryFenceLive{
		liveProof: proof, rt: fake.NewRuntime(), built: &built{},
		key: proof.cfg.Project + "-1", number: int(number) + 1,
	}
	r.github = newBehindFenceGitHub(t, r.key, r.number)
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
// the workflow, on an open pull request at the first head, and its implementer's claim mid-launch,
// serving run 1. No phases row is seeded: every entry the fence reads here is raised live, by the
// starts the completions and the moves back make.
func (r *behindEntryFenceLive) seed() {
	t := r.t
	t.Helper()
	ctx := context.Background()
	r.board.show(r.key, "in_progress")
	r.write("the issue, its pull request and its slot", func(tx pgx.Tx) error {
		if err := r.records.PutIssue(ctx, tx, record.Issue{
			Key: r.key, Repo: liveRepo, DefaultBranch: behindFenceBase, GitHubRepo: liveRepo, Tree: r.key, Project: r.cfg.Project,
			Title: "the issue whose tester reports behind its entry", Phase: phase.Implementing, Generation: 1, Status: "in_progress",
			DispatchStatus: "in_progress", Rank: "U", LastDispatchSeq: 1, HandedOver: true,
		}); err != nil {
			return err
		}
		if err := r.records.PutPullRequest(ctx, tx, record.PullRequest{
			State: record.PullRequestOpen, Issue: r.key, Repo: liveRepo, Number: r.number, Branch: "legion/" + r.key,
			HeadSHA: behindFenceFirstHead, Base: behindFenceBase, Failing: []string{},
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
		r.key, r.number, behindFenceFirstHead, behindFenceBase)
}

// write runs body in one transaction against the daemon's own database, failing the test with what
// it was seeding on an error.
func (r *behindEntryFenceLive) write(what string, body func(tx pgx.Tx) error) {
	r.t.Helper()
	if err := r.st.Tx(context.Background(), body); err != nil {
		r.t.Fatalf("seed %s: %v", what, err)
	}
}

// token is the claim token of role on the proof's issue, as the daemon spells it.
func (r *behindEntryFenceLive) token(role claim.Role) claim.Token {
	r.t.Helper()
	token, err := claim.NewToken(r.project, r.key, role)
	if err != nil {
		r.t.Fatal(err)
	}
	return token
}

// awaitLaunch waits for the running daemon to launch token's claim and returns that launch.
func (r *behindEntryFenceLive) awaitLaunch(token claim.Token) runtime.SpawnSpec {
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
func (r *behindEntryFenceLive) waitFor(what string, want func(api.Issue) bool) api.Issue {
	r.t.Helper()
	var last api.Issue
	testwait.Eventually(r.t, what, func() bool {
		last = r.issue(r.key)
		return want(last)
	})
	return last
}

// phaseRow is role's phases row as the daemon's own store holds it. It is read for what no route
// shows: the fence's own input, whether the start that raised the entry the row stands on moved the
// issue back (record.PhaseRow.EntryMovedBack), and the verdict and handoff commit a refused
// completion must leave unwritten. The entry number beside them is on the state route as well
// (api.Issue.PhaseEntry), and every step reads it there.
func (r *behindEntryFenceLive) phaseRow(role claim.Role) record.PhaseRow {
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

// behindFenceClaimant is one role's claim as its agent reaches it: the shim that dialled the
// worker stream with the boot token of the launch the fake runtime recorded, the registration that
// exchanged it for the session's capability secret, and the prompt whose turn is running.
type behindFenceClaimant struct {
	r       *behindEntryFenceLive
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
func (r *behindEntryFenceLive) claimant(role claim.Role, session string) *behindFenceClaimant {
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
	return &behindFenceClaimant{r: r, token: token, role: role, session: session, secret: registered.Secret, sh: sh}
}

// takeTask is the agent taking the daemon's next task: it reads the prompt the daemon sends,
// acknowledges it as Oh My Pi does and starts the turn that works it, and returns once the daemon
// has recorded the claim working on that delivery.
func (c *behindFenceClaimant) takeTask(want string) shimwire.Prompt {
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
func (c *behindFenceClaimant) startTurn(prompt string) {
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
func (c *behindFenceClaimant) endTurn() {
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
func (c *behindFenceClaimant) grant() string {
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
func (c *behindFenceClaimant) handoff(word string) string {
	return c.r.board.handoff(c.r.key, word, c.session)
}

// complete reports this role's completion of the phase it works, from the turn it is in, naming
// the handoff record of that phase this claim wrote, and returns the status and the refusal the
// daemon answered. Every record the board seeds is version 1 of its own id, so each completion
// names a record of its own and no two are the same one.
func (c *behindFenceClaimant) complete(summary, commit, verdict, handoffID string) (int, api.Failure) {
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
// BACKWARD_REFUSED), which is why the tester asks for it in testing and the registered reviewer in
// reviewing.
func (c *behindFenceClaimant) moveBack(to phase.Phase, reason string) (int, api.Failure) {
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

// Step 1. The seeded implementer completes implementing at the first head, which hands the testing
// phase to the tester with an entry of its own and starts it. The tester registers, takes that
// task and works it: the entry every completion below is of.
func (r *behindEntryFenceLive) testerWorksItsEntry() {
	t := r.t
	r.imp = r.claimant(claim.RoleImplementer, "ses_implementer")
	r.note("the implementer is registered and ready on the launch this boot made for the claim the fixture left mid-launch")
	r.imp.startTurn("")
	if status, failure := r.imp.complete("implemented it", behindFenceFirstHead, "", r.imp.handoff("implement")); status != http.StatusOK {
		t.Fatalf("the implementer's completion = %d %s %s, want 200", status, failure.Code, failure.Error)
	}
	r.imp.endTurn()
	r.note("drove POST /legion/v1/handoff/complete for the implementer at %s, then its agent_end: 200", behindFenceFirstHead)
	got := r.waitFor("the issue in testing", func(got api.Issue) bool { return got.Phase == phase.Testing })

	r.test = r.claimant(claim.RoleTester, "ses_tester")
	prompt := r.test.takeTask("Phase: testing")
	working := r.d.claim(r.test.token)
	if working.State != string(supervise.StateWorking) || working.Pending == nil || working.Pending.ConfirmedAt == nil {
		t.Fatalf("the tester is %s holding %+v, want working on a confirmed delivery of its testing task", working.State, working.Pending)
	}
	r.served, r.entry, r.delivery = got.PhaseEntry, got.PhaseEntry, working.Pending.ID
	r.note("read back: %s is in %s on entry %d at head %s, and the tester is %s on the delivery %s of that entry's task (%s)",
		r.key, got.Phase, r.entry, got.PullRequest.Head, working.State, r.delivery, firstLine(prompt.Message))
}

// Step 2. The tester asks for the work back with its own backward move to implementing, from the
// turn it is in, and ends that turn: the daemon sends no role a task that adopts the issue's
// working copy while another role of the issue is in a turn (supervise's sendPending), so the
// implementer's task waits for it. The implementer is prompted, and while it works the tester
// begins a turn of its own, as an agent that goes on working does, before any task of a later
// entry has reached it. The implementer pushes a head of its own and completes there, which moves
// the issue forward into testing and raises the tester's second entry, at the head the implementer
// pushed and by no move back. That entry's task waits on the claim, since the tester is in a turn,
// and the claim still serves the entry before it: every completion of steps 3 to 7 is reported
// from a turn that began before its entry's task reached the session.
func (r *behindEntryFenceLive) testingIsEnteredAgain() {
	t := r.t
	const reason = "the tests show a fault in the code; back to implementing"
	if status, failure := r.test.moveBack(phase.Implementing, reason); status != http.StatusOK {
		t.Fatalf("the tester's backward move to implementing = %d %s %s, want 200", status, failure.Code, failure.Error)
	}
	r.note("drove POST /legion/v1/phase/backward for the tester, to implementing, from the turn it is in: 200")
	r.test.endTurn()
	r.note("the tester's turn on entry %d's task ended", r.served)
	fix := r.imp.takeTask("Phase: implementing")
	r.note("read back: the implementer was prompted its implementing task once the tester's turn ended (%s)", firstLine(fix.Message))
	r.test.startTurn("")
	r.note("the tester began a turn of its own while the implementer works")
	r.pushes(behindFenceFirstHead, behindFenceSecondHead)
	if status, failure := r.imp.complete("fixed the fault the tests showed", behindFenceSecondHead, "", r.imp.handoff("implement")); status != http.StatusOK {
		t.Fatalf("the implementer's second completion = %d %s %s, want 200", status, failure.Code, failure.Error)
	}
	r.imp.endTurn()
	r.note("drove POST /legion/v1/handoff/complete for the implementer at %s, then its agent_end: 200", behindFenceSecondHead)

	got := r.waitFor("the tester's row on a second entry", func(got api.Issue) bool {
		return got.Phase == phase.Testing && got.PhaseEntry > r.entry
	})
	if got.PullRequest == nil || got.PullRequest.Head != behindFenceSecondHead {
		t.Fatalf("the record holds %+v for %s, want the pull request at %s, the head the implementer pushed", got.PullRequest, r.key, behindFenceSecondHead)
	}
	if row := r.phaseRow(claim.RoleTester); row.Entry != got.PhaseEntry || row.EntryMovedBack {
		t.Fatalf("the tester's phases row = entry %d, moved back %t; want entry %d raised by no move back: implementing moves forward into testing",
			row.Entry, row.EntryMovedBack, got.PhaseEntry)
	}
	r.entry = got.PhaseEntry
	r.heldOn()
	r.note("read back: %s is in testing on entry %d, raised by no move back (phases.entry_moved_back false), with the record at head %s",
		r.key, r.entry, got.PullRequest.Head)
}

// Step 3. A behind completion at a head the record has left is refused. The tester reports from
// its own turn its verdict on the head its entry tested, which is the head before the
// implementer's push: the daemon refuses it 409 HANDOFF_STALE_ENTRY, the refusal names that head
// and the head the record holds, nothing is recorded, and the issue stays in testing.
func (r *behindEntryFenceLive) staleHeadRefused() {
	r.refused(behindFenceFirstHead,
		"this completion is at commit "+behindFenceFirstHead+" while the pull request the record holds is at head "+behindFenceSecondHead)
}

// Step 4. A behind completion naming no commit is refused. The same completion from the same turn
// names no commit at all, so it says nothing about a head and nothing makes it the work the current
// entry's task asks for: the daemon refuses it 409 HANDOFF_STALE_ENTRY, saying it named no commit
// and what head the record holds, and nothing is recorded.
func (r *behindEntryFenceLive) behindWithNoHead() {
	r.refused("", "this completion names no commit while the pull request the record holds is at head "+behindFenceSecondHead)
}

// Step 5. A behind completion at the head the record holds is taken. The same completion from the
// same turn names the head the record holds. Nothing moved the issue back into testing, so the work
// the standing entry's task asks for is the work this turn did: the daemon takes the completion
// (behindAtRecordedHead) and the pass moves the issue to reviewing. Steps 3 and 4 are this same
// completion with another commit; step 7 is this same completion at this same head with one thing
// changed, the move that raised the entry.
func (r *behindEntryFenceLive) behindAtTheHead() {
	t := r.t
	if status, failure := r.test.complete("tested it at "+behindFenceSecondHead, behindFenceSecondHead, "pass", r.test.handoff("test")); status != http.StatusOK {
		t.Fatalf("the behind completion at the head the record holds = %d %s %s, want 200", status, failure.Code, failure.Error)
	}
	moved := r.waitFor("the issue in reviewing", func(got api.Issue) bool { return got.Phase == phase.Reviewing })
	r.note("read back: the completion of entry %d at %s, the head the record holds, reported while the row stands on entry %d, was taken 200: %s is in %s",
		r.served, behindFenceSecondHead, r.entry, r.key, moved.Phase)
	r.heldOn()
}

// Step 6. The reviewer's own move back. Its claim was started by the tester's pass, and its task
// waits while the tester's turn runs, so that turn ends first; the reviewer then registers, takes
// its review task, and the tester begins another turn of its own before the move, which no task
// of its has reached. The reviewer requests the move to testing from the turn its task began, with
// a grant its session minted. The engine takes a backward move from the role running the issue's
// phase alone, so this is the one way the proof can reach a move back into testing. Nothing is
// pushed with it: the head the record holds is the one the tester reported on in step 5, and the
// entry the move raises says it was raised by a move back.
func (r *behindEntryFenceLive) reviewerMovesTheWorkBack() {
	t := r.t
	r.test.endTurn()
	r.note("the tester's own turn ended")
	r.review = r.claimant(claim.RoleReviewer, "ses_reviewer")
	prompt := r.review.takeTask("Phase: reviewing")
	r.note("the reviewer is registered, ready and working the review task the tester's pass started it with (%s)", firstLine(prompt.Message))
	r.test.startTurn("")
	r.note("the tester began another turn of its own while the reviewer reviews")
	const reason = "the fix needs a test of its own; back to testing"
	if status, failure := r.review.moveBack(phase.Testing, reason); status != http.StatusOK {
		t.Fatalf("the reviewer's backward move to testing = %d %s %s, want 200", status, failure.Code, failure.Error)
	}
	r.note("drove POST /legion/v1/phase/backward for the reviewer, to testing: 200")

	got := r.waitFor("the issue back in testing on a third entry", func(got api.Issue) bool {
		return got.Phase == phase.Testing && got.PhaseEntry > r.entry
	})
	if got.PullRequest == nil || got.PullRequest.Head != behindFenceSecondHead {
		t.Fatalf("the record holds %+v for %s, want the pull request still at %s: nothing was pushed with the move back", got.PullRequest, r.key, behindFenceSecondHead)
	}
	row := r.phaseRow(claim.RoleTester)
	if row.Entry != got.PhaseEntry || !row.EntryMovedBack {
		t.Fatalf("the tester's phases row = entry %d, moved back %t; want entry %d raised by a move back: reviewing came back to testing",
			row.Entry, row.EntryMovedBack, got.PhaseEntry)
	}
	r.entry = got.PhaseEntry
	r.heldOn()
	r.note("read back: %s is in testing on entry %d, raised by a move back (phases.entry_moved_back true), with the record still at head %s — the head the completion step 5 took was of",
		r.key, row.Entry, got.PullRequest.Head)
}

// Step 7. A behind completion is refused whatever head it names when a move back raised the entry.
// The tester reports step 5's completion again from its own turn, at the same head, which is still
// the head the record holds — and this time the entry the phase stands on was raised by a move
// back, an entry that exists to have this phase's work done again. The daemon refuses it 409
// HANDOFF_STALE_ENTRY, the refusal says the move back is what refused it however the two heads
// read, nothing is recorded, and the issue stays in testing.
func (r *behindEntryFenceLive) behindAfterABackwardMove() {
	r.refused(behindFenceSecondHead,
		"this completion is at commit "+behindFenceSecondHead+" while the pull request the record holds is at head "+behindFenceSecondHead,
		"the move that handed the phase out again moved the issue back, which asks for this phase's work to be done again, so no verdict on either head answers for it")
}

// refused is the tester's behind completion at commit, from the turn of its first entry, refused:
// 409 HANDOFF_STALE_ENTRY, its text naming the entries, saying each of says and that nothing was
// recorded; the issue still in testing on the entry it stood on; and the tester's phases row
// holding no verdict and no handoff commit, since a refusal before the fact records nothing.
func (r *behindEntryFenceLive) refused(commit string, says ...string) {
	t := r.t
	t.Helper()
	status, failure := r.test.complete("tested it", commit, "pass", r.test.handoff("test"))
	if status != http.StatusConflict || failure.Code != "HANDOFF_STALE_ENTRY" {
		t.Fatalf("the behind completion at commit %q = %d %s %s, want 409 HANDOFF_STALE_ENTRY", commit, status, failure.Code, failure.Error)
	}
	behind := fmt.Sprintf("handed the tester phase testing again: this completion is of entry %d, which is %d behind the entry %d the phase is on",
		r.served, r.entry-r.served, r.entry)
	for _, want := range append([]string{behind, "nothing was recorded"}, says...) {
		if !strings.Contains(failure.Error, want) {
			t.Fatalf("the refusal = %q, want it to say %q", failure.Error, want)
		}
	}
	r.note("read back: the behind completion at commit %q = 409 %s — %s", commit, failure.Code, failure.Error)
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

// heldOn holds that the tester is in a turn while the phases row stands on a later entry than the
// one whose task the tester took in step 1, and that no task of a later entry has reached it: the
// task the claim holds, if any, is unconfirmed and undelivered, so the claim still serves the
// first entry. That is the state every completion of steps 3 to 7 is reported from, and it says so.
func (r *behindEntryFenceLive) heldOn() {
	t := r.t
	t.Helper()
	held := r.d.claim(r.test.token)
	if held.State != string(supervise.StateWorking) || (held.Pending != nil && (held.Pending.ConfirmedAt != nil || held.Pending.DeliveredAt != nil)) {
		t.Fatalf("the tester is %s holding %+v, want it in a turn of its own with no later task delivered: its claim serves entry %d",
			held.State, held.Pending, r.served)
	}
	waiting := "no task"
	if held.Pending != nil {
		waiting = "the task " + held.Pending.ID + ", undelivered,"
	}
	r.note("read back: the tester is %s in a turn of its own holding %s while its row stands on entry %d; the last task it took is %s, of entry %d",
		held.State, waiting, r.entry, r.delivery, r.served)
}

// pushes is the implementer's push of to onto from: GitHub's own copy of the pull request moves
// first, as it does on github.com, then the push and the synchronize Envoy relays, and it returns
// once the head the daemon's record holds is the pushed one.
func (r *behindEntryFenceLive) pushes(from, to string) {
	r.t.Helper()
	r.github.head(to)
	r.publishPush(from, to)
	r.publishSynchronize(to)
	r.note("drove the implementer's push of %s onto %s and GitHub's synchronize of #%d to it", to, from, r.number)
	r.waitFor("the pull request at the pushed head "+to, func(got api.Issue) bool {
		return got.PullRequest != nil && got.PullRequest.Head == to
	})
}

// publishPush publishes the implementer's push of to onto from on the issue's branch, changing
// code, as Envoy relays a GitHub push (intake's decodePush reads it).
func (r *behindEntryFenceLive) publishPush(from, to string) {
	r.t.Helper()
	payload, err := json.Marshal(map[string]string{
		"kind": "push", "repo": liveRepo, "ref": "refs/heads/legion/" + r.key, "before": from, "after": to,
		"changed_paths": "src/widget.go", "changed_paths_truncated": "false", "forced": "false",
		"pusher": "legion-implement[bot]", "commit_count": "1", "head_subject": "fix the fault the tests showed",
	})
	if err != nil {
		r.t.Fatalf("encode the push payload: %v", err)
	}
	subject := fmt.Sprintf("notifications.github.%s.push.branch.legion/%s", r.subjectRepo(), r.key)
	r.publish(subject, "github", fmt.Sprintf("push-%d-%s", r.number, to), string(payload))
}

// publishSynchronize publishes GitHub's synchronize of the pull request to head, as Envoy relays a
// GitHub pull-request event (intake's decodePullRequest reads it).
func (r *behindEntryFenceLive) publishSynchronize(head string) {
	r.t.Helper()
	payload, err := json.Marshal(map[string]string{
		"action": "synchronize", "kind": "pr", "repo": liveRepo, "number": strconv.Itoa(r.number),
		"head_sha": head, "base_ref": behindFenceBase, "head_ref": "legion/" + r.key, "title": "the issue whose tester reports behind its entry",
		"url":        "https://github.com/" + liveRepo + "/pull/" + strconv.Itoa(r.number),
		"updated_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		r.t.Fatalf("encode the synchronize payload: %v", err)
	}
	subject := fmt.Sprintf("notifications.github.%s.pr.%d", r.subjectRepo(), r.number)
	r.publish(subject, "github", fmt.Sprintf("pr-%d-synchronize-%s", r.number, head), string(payload))
}

// behindFenceGitHub is GitHub's REST API for acme/widgets as this proof's daemon reads it: the one
// pull request at the head the driver last pushed, with its base and `mergeable: true`
// (readPullRequest), and its reviews as none (readReviews). It also answers what a launch reads:
// any repository, with default branch main, and a GraphQL root that knows no pull request.
// Anything else is answered 404, so a read this proof did not foresee fails the step that waited
// on it rather than reaching github.com.
type behindFenceGitHub struct {
	t      *testing.T
	url    string
	key    string
	number int
	mu     sync.Mutex
	// sha is the pull request's head on GitHub, which a push moves.
	sha string
}

func newBehindFenceGitHub(t *testing.T, key string, number int) *behindFenceGitHub {
	t.Helper()
	g := &behindFenceGitHub{t: t, key: key, number: number, sha: behindFenceFirstHead}
	g.url = githubtest.Server(t, githubtest.Spec{Handle: g.ServeHTTP})
	return g
}

// head moves the pull request to sha, as a push does on GitHub.
func (g *behindFenceGitHub) head(sha string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sha = sha
}

func (g *behindFenceGitHub) ServeHTTP(w http.ResponseWriter, req *http.Request) {
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
func (g *behindFenceGitHub) answer(w http.ResponseWriter, path string) bool {
	rest, ours := strings.CutPrefix(path, "/repos/"+liveRepo)
	if !ours {
		// Any other repository is answered as the launch reads it: a name and its default branch.
		other, isRepository := strings.CutPrefix(path, "/repos/")
		if parts := strings.Split(other, "/"); isRepository && len(parts) == 2 {
			g.write(w, map[string]any{"full_name": other, "default_branch": behindFenceBase})
			return true
		}
		return false
	}
	switch {
	case rest == "":
		g.write(w, map[string]any{"full_name": liveRepo, "default_branch": behindFenceBase})
	case rest == fmt.Sprintf("/pulls/%d/reviews", g.number):
		g.write(w, []any{})
	case rest == fmt.Sprintf("/pulls/%d", g.number):
		g.write(w, map[string]any{
			"number": g.number, "state": "open", "mergeable": true,
			"base": map[string]string{"ref": behindFenceBase},
			"head": map[string]string{"ref": "legion/" + g.key, "sha": g.sha},
		})
	default:
		return false
	}
	return true
}

// write encodes value as the answer.
func (g *behindFenceGitHub) write(w http.ResponseWriter, value any) {
	if err := json.NewEncoder(w).Encode(value); err != nil {
		g.t.Errorf("write the GitHub stand-in's answer: %v", err)
	}
}
