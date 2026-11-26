//go:build e2e

// The live proof's driver for INFRA-5602 (A1 and A2): when an issue Legion started closes and its
// last run had no retro, Legion runs that retro after the close, in the way it runs a production
// check after a merge, and the retro of an open issue whose pull request merged sends its findings
// to the tree's root architect the same way. Each retro's agent gets one task after the close,
// whose text says the issue is closed, that the retro moves it nowhere and that the retro skill's
// steps that move work back do not apply, and where a finding goes; it completes with a plan
// rating, which the daemon takes and the Dispatch stand-in records beside the retro's row; the
// issue's status stays as the close left it; and an issue whose last run had its retro, or that
// Legion never started, gets none. scripts/e2e/retro-after-close.sh builds this driver with
// `-tags e2e` and owns the nats-server and the PostgreSQL it runs against, through the run
// lib/live-daemon-proof.sh makes around every whole-daemon live proof.
//
// The daemon is the whole daemon: run, the function `legion start` calls, through this package's
// whole-daemon harness (startDaemonWithin). Facts enter only by the transports production has: the
// Dispatch envelopes of the ENVOY_NOTIFICATIONS stream, the daemon's own HTTP routes, its worker
// stream, GitHub's pull-request events, and the runtime's own report of a process. A step reads
// GET /legion/v1/state, the operator's claim listing, the prompts each claim's process was given,
// what the daemon wrote to the Dispatch stand-in (the step rows and the plan ratings
// liveDispatch keeps, and every status it set), the launches the fake runtime was asked for, and
// the daemon's own PostgreSQL.
//
// What the proof stands in for lies outside the change. Dispatch is liveDispatch, the board the
// package's whole-daemon live proofs share (dispatch_board_test.go), fronted by linkedDispatch for
// the blocked_by links the driver gave an issue; GitHub is stepRecordsGitHub, which holds the pull
// request of each branch the driver opens and merges; Envoy's listener for the daemon's notices is
// the package's noticeListener; and no agent runs: each claim is launched by the package's fake
// runtime, and the driver plays each claim over the daemon's own worker stream and HTTP routes, as
// a pane's shim and its agent do (retroAgent). The one real Envoy is the listener of
// packages/envoy/cmd/listener, built with testbuild.EnvoyBinary and run on the proof's
// nats-server beside the stand-ins (startIdleRigListener): it owns the notification stream, and
// it is the one component that sees a message published to a role topic no live session holds and
// reports the wake the daemon acts on, so the root's retro agent sends its finding through it.
//
// The driver names no symbol of this change: it reads what the daemon records of a retro through
// the Dispatch stand-in's step rows, the daemon's HTTP routes, the prompts the agents were given
// and the claims the runtime launched, and three tables through SQL: run_retros, for an issue's
// retros after a close (retrosAfterClose, which reads none where the table does not exist, as on
// main), tree_lifecycles, for a lifecycle's epoch (lifecycleEpoch), and outbox, for the
// linger_close row a park queues, which it brings due with the issue's linger_until (endLinger).
// So the same file builds and runs on main. That is what the script's negative control needs.
//
// Each step runs on its own and prints `== <step>`, then `ok <step>` or `FAIL <step> at
// <assertion>`; a step that fails does not stop the next, and the test fails at the end if any
// step did.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

const (
	// retroWait bounds each wait for something the daemon must do: long enough for a loaded
	// runner, short enough that the proof against main, where none of the retros come, ends in
	// minutes, each step failing at the assertion that its retro task came.
	retroWait = 20 * time.Second
	// retroPoll is how often a wait asks its question.
	retroPoll = 50 * time.Millisecond
	// retroCap is the admission cap: a slot for each of the three roots and for every child that
	// runs at once.
	retroCap = 12
	// retroLinger is the linger every root's park or close begins, the project's default: long
	// enough that T's and R2's lingers outlast the proof, and R3's is brought forward by hand.
	retroLinger = 72 * time.Hour
)

// The clauses R2 says a retro task after a close holds, in the words the daemon's task gives them
// (workflow's retroAfterCloseTask and findingsClause): the issue is closed, the retro moves it
// nowhere and the retro skill's steps that move work back do not apply, a requirement the retro
// finds unmet goes to the tree's root architect, and a requirement another open child of the same
// parent carries is pending and goes to no one.
const (
	retroClosedClause  = "This issue is closed"
	retroNowhereClause = "the retro moves it nowhere"
	retroSkillClause   = "steps that move work back do not apply"
	retroFindingClause = "goes to the tree's root architect"
	retroPendingClause = "another open child of the same parent carries is pending and goes to no one"
)

// retroArchitectWakeOpening is the opening of the daemon's wake task to a closed tree's root
// architect (workflow's retroArchitectWakeTask).
const retroArchitectWakeOpening = "Your tree closed, and the daemon stopped your session with it. A retro Legion runs after the close of an issue of your tree is open"

// retroStepFailure is what a step's assertion ends the step with: the assertion that did not hold.
type retroStepFailure struct{ at string }

// retroLive is one run of the proof: the booted daemon, the three roots it runs, the processes the
// driver plays for the claims, and the stand-ins each step reads the daemon's writes back from.
type retroLive struct {
	liveProof
	rt       *fake.Runtime
	built    *built
	github   *stepRecordsGitHub
	links    *linkedDispatch
	listener *idleRigListener
	natsURL  string
	// tree is the test root T, whose children each step closes; r2 and r3 are the two more roots
	// the last steps park and close. Each is seeded at admitted with its design gate approved, so
	// the boot starts its first phase.
	tree, r2, r3 string
	root         *retroAgent
	// seq is the Dispatch sequence the next envelope the driver publishes carries; number is the
	// next pull request number; issues numbers the issues the steps make; reviews numbers the
	// reviews the driver submits.
	mu      sync.Mutex
	seq     int64
	number  int
	issues  int
	reviews int
	// agents is the process of each claim the driver plays now, and registrations counts every
	// process it has played, so a mark says which came after it.
	agents        map[claim.Token]*retroAgent
	registrations int
	// failed names each step that did not hold.
	failed []string
	// member is step 3's started child, closed by a person, whose retro agent leaves its retro
	// unreported until step 10; memberAgent is that agent.
	member      string
	memberAgent *retroAgent
	// child is step 1's child C, whose implementer pushed a head and which step 2 closes.
	child string
}

// TestTheRetroRunsAfterTheCloseLive drives the proof's steps against one booted daemon: steps 1 to 9,
// 17, 14, then the root's (11) and the member's retro it leaves open (10), then the parked roots (12
// and 13). Steps 15 and 16 are the plan's after INFRA-3570's branch is merged in.
func TestTheRetroRunsAfterTheCloseLive(t *testing.T) {
	fixtureenv.Live(t)
	r := newRetroLive(t)
	r.run("grandchild-closed", r.grandchildClosed)
	r.run("child-closed", r.childClosed)
	r.run("person-closed", r.personClosed)
	r.run("backlog-then-closed", r.backlogThenClosed)
	r.run("retro-had", r.retroHad)
	r.run("never-started", r.neverStarted)
	r.run("parent-closed", r.parentClosed)
	r.run("closed-in-retro", r.closedInRetro)
	r.run("merged-early", r.mergedEarly)
	r.run("sent-back-then-closed", r.sentBackThenClosed)
	r.run("todo-ends-retro", r.todoEndsRetro)
	// Steps 15 and 16 go here (t9, after INFRA-3570's branch is merged into this one: the retro's
	// agent is then the one record.PlaceWorker names, and the daemon's task goes to it).
	r.run("root-closed", r.rootClosed)
	r.run("member-open-at-root-close", r.memberOpenAtRootClose)
	r.run("root-closed-in-linger", r.rootClosedInLinger)
	r.run("expired-root-closed", r.expiredRootClosed)
	if len(r.failed) > 0 {
		t.Fatalf("%d steps did not hold: %s", len(r.failed), strings.Join(r.failed, ", "))
	}
}

// newRetroLive boots the daemon over the script's broker with the three roots recorded, and
// registers and readies the test root's architect. The Envoy listener starts first: it creates the
// notification stream with its own subjects, which the daemon's consumers then bind, as a
// deployment rolls them in.
func newRetroLive(t *testing.T) *retroLive {
	t.Helper()
	natsURL := os.Getenv(liveNATSURLEnv)
	if natsURL == "" {
		t.Skipf("%s is unset, so there is no broker to drive the daemon's intake through; scripts/e2e/retro-after-close.sh starts a nats-server and sets it", liveNATSURLEnv)
	}
	listener := startIdleRigListener(t, natsURL, "retro-proof")
	cfg := workflowConfig(t, natsURL)
	p := liveProof{t: t, cfg: cfg, board: newLiveDispatch(t, cfg.Project), notices: newNoticeListener(), records: record.NewStore()}
	envoyServer := httptest.NewServer(p.notices)
	t.Cleanup(envoyServer.Close)
	p.cfg.EnvoyURL = envoyServer.URL
	var err error
	if p.project, err = claim.ProjectToken(cfg.Project); err != nil {
		t.Fatal(err)
	}
	p.js = workflowJetStream(t, natsURL)
	ctx := context.Background()
	if p.st, err = store.Open(ctx, p.cfg.PostgresDSN); err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(p.st.Close)
	if _, err := p.st.Migrate(ctx); err != nil {
		t.Fatalf("migrate the store: %v", err)
	}
	p.cfg.AdmissionCap = retroCap
	p.cfg.Linger = retroLinger
	links := &linkedDispatch{board: p.board, links: map[string][]string{}}
	front := httptest.NewServer(links)
	t.Cleanup(front.Close)
	p.cfg.DispatchURL = front.URL
	r := &retroLive{liveProof: p, rt: fake.NewRuntime(), built: &built{}, github: newStepRecordsGitHub(t), links: links, listener: listener,
		natsURL: natsURL, seq: 1, number: 100, issues: 3, agents: map[claim.Token]*retroAgent{}}
	r.rt.InPod = true
	key := func(n int) string { return fmt.Sprintf("%s-%d", p.cfg.Project, n) }
	r.tree, r.r2, r.r3 = key(1), key(2), key(3)
	r.seedRoot(r.tree, "A")
	r.seedRoot(r.r2, "B")
	r.seedRoot(r.r3, "C")
	o := fakeRuntime(r.rt, r.built)
	o.workflowTokens = &workflowTokenRecorder{}
	o.githubAPI = r.github.url
	r.d = startDaemonWithin(t, r.cfg, o, r.log(), 60*time.Second)
	r.root = r.register(r.tree, r.tree, claim.RoleArchitect)
	r.note("booted the daemon over the proof's nats-server and its own PostgreSQL, with the Envoy listener beside the stand-ins; registered and readied %s's architect as session %s", r.tree, r.root.session)
	return r
}

// seedRoot records key as the daemon holds a root the moment its architect's spec is approved: at
// admitted, handed to Legion, its gate opened, its lifecycle under the workflow, and its
// architect's claim a launch a previous daemon began and never recorded, which the boot relaunches.
// The boot's admission pass then starts its first phase, which is the start of its run.
func (r *retroLive) seedRoot(key, rank string) {
	t := r.t
	t.Helper()
	ctx := context.Background()
	r.board.show(key, "in_progress")
	approved := 1
	if err := store.Transact(ctx, r.st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		if err := r.records.PutIssue(ctx, tx, record.Issue{
			Key: key, Repo: liveRepo, DefaultBranch: "main", GitHubRepo: liveRepo, Tree: key, Project: r.cfg.Project,
			Title: key, Phase: phase.Admitted, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress", Rank: rank,
			LastDispatchSeq: 1, HandedOver: true,
		}); err != nil {
			return err
		}
		return r.records.PutGate(ctx, tx, record.DesignGate{Issue: key, ArtifactID: "spec-" + key, LatestVersion: 1, ApprovedVersion: &approved, EverOpened: true})
	}); err != nil {
		t.Fatalf("seed %s: %v", key, err)
	}
	if _, err := r.st.OpenTreeLifecycle(ctx, r.project, key, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("open the lifecycle of %s: %v", key, err)
	}
	architect := r.token(key, claim.RoleArchitect)
	if _, err := r.st.AdmitClaim(ctx, supervise.Claim{
		Token: architect, Project: r.project, Tree: key, Issue: key, Role: claim.RoleArchitect,
		Generation: 1, State: supervise.StateLaunching, BootTokenHash: supervise.HashBootToken("interrupted-" + randomSuffix(t)),
	}); err != nil {
		t.Fatalf("record the architect's unfinished launch on %s: %v", key, err)
	}
	writePrompt(t, r.cfg, architect)
}

// run is one step of the proof: it prints `== name`, then `ok name` or `FAIL name at <assertion>`.
// The body runs on a goroutine of its own, so that an assertion that does not hold, or a helper
// that stops the test's goroutine, ends this step alone.
func (r *retroLive) run(name string, body func()) {
	fmt.Printf("== %s\n", name)
	started := time.Now()
	var failure *retroStepFailure
	completed := false
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if v := recover(); v != nil {
				if f, ok := v.(retroStepFailure); ok {
					failure = &f
					return
				}
				failure = &retroStepFailure{at: fmt.Sprintf("a panic: %v", v)}
			}
		}()
		body()
		completed = true
	}()
	<-done
	switch {
	case failure != nil:
		fmt.Printf("FAIL %s at %s\n", name, failure.at)
		r.failed = append(r.failed, name)
	case !completed:
		fmt.Printf("FAIL %s at a call that stopped the step; the test's log above says which\n", name)
		r.failed = append(r.failed, name)
	default:
		fmt.Printf("ok %s (%ds)\n", name, int(time.Since(started).Seconds()))
	}
}

// fail ends the step with the assertion that did not hold.
func (r *retroLive) fail(format string, args ...any) {
	panic(retroStepFailure{at: fmt.Sprintf(format, args...)})
}

// until waits up to bound for cond to hold, and ends the step with what it waited for otherwise.
func (r *retroLive) until(what string, bound time.Duration, cond func() bool) {
	r.t.Helper()
	if !testwait.Holds(bound, retroPoll, cond) {
		r.fail("%s: not within %s", what, bound)
	}
}

// ---- the steps -----------------------------------------------------------------------------------

// grandchild-closed (A1): T's architect releases child C and C's first phase starts; C's architect
// files and releases grandchild G and G's first phase starts; C's implementer pushes a head; C's
// architect writes done on G. G's retro agent gets one task after the close, whose text holds each
// clause of R2 and names T's architect's role topic, not C's; the retro completes with a plan
// rating; G's status stays done.
func (r *retroLive) grandchildClosed() {
	c, g := r.newKey(), r.newKey()
	r.child = c
	r.board.place(c, "triage", r.tree, false)
	r.releaseBy(r.root, c)
	r.startsPlanning(c)
	childArchitect := r.register(c, r.tree, claim.RoleArchitect)
	r.board.place(g, "triage", c, false)
	r.releaseBy(childArchitect, g)
	r.startsPlanning(g)
	r.note("T's architect released %s and %s's architect released %s: each started its first phase", c, c, g)
	r.walkTo(c, phase.Testing)
	r.note("%s's implementer pushed head %s on pull request #%d: %s is in testing", c, stepRecordsHead[:7], r.pullRequestOf(c), c)

	mark := r.markOf(g, claim.RoleImplementer)
	r.sessionWrite(childArchitect, g, "done")
	r.note("%s's architect wrote done on %s: an issue.updated envelope, actor %s", c, g, childArchitect.session)
	agent, task := r.retroTask(g, r.tree, mark)
	r.requireR2(g, r.tree, task.Message, childArchitect)
	r.note("read back: one retro task went to %s (the implementer of %s, %s) after the close: %s", agent.token, g, r.launchKind(agent.token), firstLine(task.Message))
	r.completesRetro(g, agent, mark, 4)
	r.requireStatusStays(g, "done", mark)
}

// child-closed (A1): T's architect writes done on C, whose implementer pushed a head. The same as
// the grandchild's: one task, the clauses of R2, a retro that completes, C's status stays done; and
// a backward move of C with the retro agent's grant is refused BACKWARD_REFUSED.
func (r *retroLive) childClosed() {
	c := r.child
	mark := r.markOf(c, claim.RoleImplementer)
	r.sessionWrite(r.root, c, "done")
	r.note("%s's architect wrote done on %s: an issue.updated envelope, actor %s", r.tree, c, r.root.session)
	agent, task := r.retroTask(c, r.tree, mark)
	r.requireR2(c, r.tree, task.Message)
	r.note("read back: one retro task went to %s (the implementer of %s, which pushed the head) after the close: %s", agent.token, c, firstLine(task.Message))
	status, body := agent.post("/legion/v1/phase/backward", api.PhaseBackwardRequest{GrantID: agent.grant(), To: phase.Implementing, Reason: "the retro found a gap"})
	if status == http.StatusOK || !strings.Contains(body, "BACKWARD_REFUSED") {
		r.fail("the retro agent's backward move of the closed %s was answered %d %s, want BACKWARD_REFUSED", c, status, body)
	}
	r.note("read back: the backward move of %s with the retro agent's grant was refused %d %s", c, status, strings.TrimSpace(body))
	r.completesRetro(c, agent, mark, 4)
	r.requireStatusStays(c, "done", mark)
}

// person-closed: a second started child, closed by a person's done: one task to its retro agent.
// The retro is left unreported; it completes in step 10, after the test root's close.
func (r *retroLive) personClosed() {
	c := r.todoChild(r.tree)
	mark := r.markOf(c, claim.RoleImplementer)
	r.person(c, "done", r.tree, false)
	r.note("a person closed %s: an issue.updated envelope, status done", c)
	agent, task := r.retroTask(c, r.tree, mark)
	r.requireTaskOnce(c, mark)
	r.requireRetroStarted(c, 0)
	r.member, r.memberAgent = c, agent
	r.note("read back: one retro task went to %s after the close: %s; its retro row started and is left open", agent.token, firstLine(task.Message))
}

// backlog-then-closed: a third child runs into implementing, so its implementer has launched and has
// a session, and a person moves it to backlog: no retro task. A person's done then gets one task to
// the implementer, resumed in its kept session; the retro completes.
func (r *retroLive) backlogThenClosed() {
	c := r.todoChild(r.tree)
	r.walkTo(c, phase.Implementing)
	implementer := r.registerTask(c, r.tree, claim.RoleImplementer)
	implementer.endTurn()
	file := r.d.claim(implementer.token).SessionFile
	r.person(c, "backlog", r.tree, false)
	r.waitStatus(c, "backlog")
	r.until("the implementer of "+c+" suspended with its session kept by the park", retroWait, func() bool {
		held := r.d.claim(implementer.token)
		return held.State == string(supervise.StateSuspended) && held.SuspendedBy == string(supervise.SuspendedByDaemon)
	})
	implementer.end()
	mark := r.markOf(c, claim.RoleImplementer)
	time.Sleep(liveSettle)
	r.requireNoRetro(c, mark, "the move to backlog")
	r.note("read back: %s moved to backlog, its implementer suspended with session %s kept, and no retro task or row over %s", c, implementer.session, liveSettle)
	r.person(c, "done", r.tree, false)
	r.note("a person closed %s: an issue.updated envelope, status done", c)
	agent, _ := r.retroTask(c, r.tree, mark)
	if kind := r.launchKind(agent.token); kind != "Resume" || r.launchOf(agent.token).ResumeSessionFile != file {
		r.fail("the retro's agent of %s was launched by a %s of %q, want a Resume of its kept session file %q", c, kind, r.launchOf(agent.token).ResumeSessionFile, file)
	}
	r.note("read back: one retro task went to %s, resumed in its kept session file %s", agent.token, file)
	r.completesRetro(c, agent, mark, 4)
}

// retro-had: a fourth child runs its plan through its retro, which completes; it then closes by its
// merge, and no task follows that close.
func (r *retroLive) retroHad() {
	c := r.todoChild(r.tree)
	r.walkTo(c, phase.Retro)
	implementer := r.agents[r.token(c, claim.RoleImplementer)]
	retro := implementer.next()
	r.note("%s reached its retro: its implementer was given %s", c, firstLine(retro.Message))
	implementer.completeRetroInPhase(4)
	r.waitPhase(c, phase.Merging)
	rows := len(r.retroRows(c))
	r.awaitRetroRow(c, 0, retroRowWant{result: "passed", rating: 4})
	mark := r.markOf(c, claim.RoleImplementer)
	r.mergeOf(c)
	r.waitPhase(c, phase.Done)
	time.Sleep(liveSettle)
	r.requireNoRetro(c, mark, "its merge")
	if got := len(r.retroRows(c)); got != rows {
		r.fail("%s has %d retro rows after its merge, want the %d its own retro wrote", c, got, rows)
	}
	r.note("read back: %s's retro passed, it closed by its merge, and over %s no task, row or launch followed", c, liveSettle)
}

// never-started: a fifth child, held by a blocked_by link to an open issue, is closed by a person
// before its first phase: no retro task.
func (r *retroLive) neverStarted() {
	c := r.heldChild(r.tree)
	mark := r.markOf(c, claim.RoleImplementer)
	r.person(c, "done", r.tree, false)
	r.waitStatus(c, "done")
	time.Sleep(liveSettle)
	r.requireNoRetro(c, mark, "its close")
	r.note("read back: %s closed before its first phase, and over %s no retro task, row or launch of its implementer", c, liveSettle)
}

// parent-closed: a test parent P on the default steps is held short of its first phase by a
// blocked_by link to an open issue of the test, while its child PC's first phase starts; a person's
// done on P while PC runs gets one task, to P's implementer, which the daemon launches for it (a
// Spawn); the retro completes.
func (r *retroLive) parentClosed() {
	p := r.heldChild(r.tree)
	pc := r.newKey()
	r.person(pc, "todo", p, false)
	r.startsPlanning(pc)
	r.note("%s is held at admitted by a blocked_by link while its child %s started its first phase", p, pc)
	mark := r.markOf(p, claim.RoleImplementer)
	r.person(p, "done", r.tree, false)
	r.note("a person closed %s while %s runs: an issue.updated envelope, status done", p, pc)
	agent, task := r.retroTask(p, r.tree, mark)
	if kind := r.launchKind(agent.token); kind != "Spawn" {
		r.fail("the retro's agent of %s was launched by a %s, want a Spawn: no implementer of it ever launched", p, kind)
	}
	r.requireTaskOnce(p, mark)
	r.note("read back: one retro task went to %s, which the fake runtime recorded as a Spawn: %s", agent.token, firstLine(task.Message))
	r.completesRetro(p, agent, mark, 3)
}

// closed-in-retro: a sixth child stands in its retro, and a person's done comes while that turn
// runs. The agent's grant, which its `legion` tool mints for the operation it is about to make, is
// minted before the close. The step first sends the retro's completion with a rating and records the
// answer, whatever its code, then holds that from the close to the answer the turn kept its claim
// and session (no suspend, no Resume and no Spawn of it), no second retro task came, and the retro
// completes.
func (r *retroLive) closedInRetro() {
	c := r.todoChild(r.tree)
	r.walkTo(c, phase.Retro)
	implementer := r.agents[r.token(c, claim.RoleImplementer)]
	retro := implementer.next()
	if !strings.Contains(retro.Message, "Phase: retro.") {
		r.fail("%s's implementer was given %q in its retro, want the retro's task", c, firstLine(retro.Message))
	}
	session := r.d.claim(implementer.token).Session
	calls := len(r.rt.Calls())
	mark := r.markOf(c, claim.RoleImplementer)
	grant := implementer.grant()
	r.person(c, "done", r.tree, false)
	r.note("a person closed %s while its retro turn runs: an issue.updated envelope, status done", c)
	r.waitStatus(c, "done")
	status, body := implementer.post("/legion/v1/handoff/complete", api.HandoffCompleteRequest{GrantID: grant, Summary: "retro written", PlanRating: new(4), PlanReason: new("The plan fit the work.")})
	answered := len(r.rt.Calls())
	if status != http.StatusOK {
		r.fail("the retro's completion after the close of %s was answered %d %s, want 200", c, status, strings.TrimSpace(body))
	}
	for _, call := range r.rt.Calls()[calls:answered] {
		switch call.Method {
		case "Suspend", "Release":
			if call.Locator.Claim == implementer.token || call.Released.Claim == implementer.token {
				r.fail("the close stopped the retro's turn: the runtime was asked for a %s of %s before the report", call.Method, implementer.token)
			}
		case "Spawn", "Resume":
			if call.Spec.Claim == implementer.token {
				r.fail("the close launched the retro's agent again: the runtime was asked for a %s of %s before the report", call.Method, implementer.token)
			}
		}
	}
	if held := r.d.claim(implementer.token); held.Session != session {
		r.fail("the retro's agent of %s runs session %q after the close, want %q kept", c, held.Session, session)
	}
	implementer.endTurn()
	r.note("read back: the completion was answered 200, and from the close to that answer the runtime was asked for no suspend, release, resume or spawn of %s", implementer.token)
	r.awaitRetroRow(c, 0, retroRowWant{result: "passed", rating: 4})
	time.Sleep(liveSettle)
	if got := r.retroTasksSince(c, claim.RoleImplementer, taskMark{}, false); got != 1 {
		r.fail("%s's implementer was given %d retro tasks, want the one it was working when the close came", c, got)
	}
	r.requireStatusStays(c, "done", mark)
	r.note("read back: no second retro task came, the retro row ended passed with the plan rating, and %s stays done", c)
}

// merged-early: a seventh child's pull request merges before its merging phase. Its retro task,
// while the issue is open, names the test root's architect as where a requirement it finds unmet
// goes and says that one another open child carries is pending; the retro completes; the child closes
// by its merge at merging, and no retro task follows that close.
func (r *retroLive) mergedEarly() {
	c := r.todoChild(r.tree)
	r.walkTo(c, phase.Accepting)
	number := r.pullRequestOf(c)
	r.github.merged(number)
	r.waitProcessed(r.publishMergeOf(number, c, stepRecordsHead, "main"))
	r.note("%s's pull request #%d merged while the issue stands in accepting: a pr-merged notice, no close", c, number)
	r.signOff(c)
	implementer := r.agents[r.token(c, claim.RoleImplementer)]
	retro := implementer.next()
	topic := notify.RoleTopicPrefix + string(r.token(r.tree, claim.RoleArchitect))
	r.requireTaskHolds(c, retro.Message, "the root architect's role topic "+topic, topic, retroFindingClause, retroPendingClause)
	if strings.Contains(retro.Message, retroClosedClause) {
		r.fail("the retro task of the open issue %s says %q: %s", c, retroClosedClause, firstLine(retro.Message))
	}
	r.note("read back: %s's retro task, while it is open, names %s and says one another open child carries is pending", c, topic)
	implementer.completeRetroInPhase(4)
	r.awaitRetroRow(c, 0, retroRowWant{result: "passed", rating: 4})
	r.waitPhase(c, phase.Done)
	mark := r.markOf(c, claim.RoleImplementer)
	time.Sleep(liveSettle)
	r.requireNoRetro(c, mark, "its merge")
	r.note("read back: the retro passed, %s closed by its merge, and over %s no retro task followed the close", c, liveSettle)
}

// sent-back-then-closed (step 17): another child runs to its retro, which moves the work back; a
// move back takes no rating, so the retro's row ends failed with none. A person's done then gets no
// task: a retro row of that run started before the close.
func (r *retroLive) sentBackThenClosed() {
	c := r.todoChild(r.tree)
	r.walkTo(c, phase.Retro)
	implementer := r.agents[r.token(c, claim.RoleImplementer)]
	implementer.next()
	status, body := implementer.post("/legion/v1/phase/backward", api.PhaseBackwardRequest{GrantID: implementer.grant(), To: phase.Implementing, Reason: "the retro found a gap"})
	if status != http.StatusOK {
		r.fail("the retro's backward move of %s was answered %d %s, want 200", c, status, strings.TrimSpace(body))
	}
	implementer.endTurn()
	r.waitPhase(c, phase.Implementing)
	r.awaitRetroRow(c, 0, retroRowWant{result: "failed"})
	rows := len(r.retroRows(c))
	r.note("read back: %s's retro moved the work back to implementing; its retro row ended failed, with no rating", c)
	mark := r.markOf(c, claim.RoleImplementer)
	r.person(c, "done", r.tree, false)
	r.waitStatus(c, "done")
	time.Sleep(liveSettle)
	r.requireNoRetro(c, mark, "its close")
	if got := len(r.retroRows(c)); got != rows {
		r.fail("%s has %d retro rows after its close, want the %d of its own retro", c, got, rows)
	}
	r.note("read back: a person closed %s, and over %s no retro task, row or launch followed: a retro row of its run began before the close", c, liveSettle)
}

// todo-ends-retro: another child is closed: one task after the close and its retro row's start.
// The retro is left open, and the child is set back to todo while T is in the workflow: the row ends
// stopped with no rating. The re-entered child, held before its second run's first phase, is closed
// again: no retro task follows, since a retro row of its first run began before it.
func (r *retroLive) todoEndsRetro() {
	c := r.todoChild(r.tree)
	mark := r.markOf(c, claim.RoleImplementer)
	r.person(c, "done", r.tree, false)
	agent, task := r.retroTask(c, r.tree, mark)
	r.requireTaskOnce(c, mark)
	r.requireRetroStarted(c, 0)
	r.note("a person closed %s: one retro task went to %s (%s), and its retro row started", c, agent.token, firstLine(task.Message))
	blocker := r.newKey()
	r.waitProcessedAny(r.publishIssue(userActor, blocker, "in_progress", "", nil, false))
	r.waitProcessedAny(r.publishIssue(userActor, c, "todo", r.tree, []string{blocker}, false))
	r.note("a person set %s back to todo under %s, blocked_by %s: an issue.updated envelope", c, r.tree, blocker)
	r.awaitRetroRow(c, 0, retroRowWant{result: "stopped"})
	r.waitIssue(c+" re-entered at admitted, held by "+blocker, c, func(i api.Issue) bool { return i.Phase == phase.Admitted && i.Status == "todo" })
	rows := len(r.retroRows(c))
	r.note("read back: %s's retro row ended stopped with no rating, and the child stands at admitted", c)
	marked := r.markOf(c, claim.RoleImplementer)
	r.person(c, "done", r.tree, false)
	r.waitStatus(c, "done")
	time.Sleep(liveSettle)
	r.requireNoRetro(c, marked, "its second close")
	if got := len(r.retroRows(c)); got != rows {
		r.fail("%s has %d retro rows after its second close, want the %d of its first run", c, got, rows)
	}
	r.note("read back: closed again before its second run's first phase, %s got no retro task, row or launch over %s", c, liveSettle)
}

// root-closed (A2): a person's done on the test root after its first phase: one task to T's retro
// agent. T's architect, suspended by the close, is resumed in its kept session when the retro agent
// publishes one finding to its role topic through the real Envoy listener, which keeps the message
// for the role no live session holds and reports the wake. The driver stands in for the resumed
// session: it registers with the listener as the holder of T's architect's role, reads the kept
// finding delivered to it and receives the daemon's one retro wake task. T's retro completes.
func (r *retroLive) rootClosed() {
	t := r.tree
	token := r.root.token
	file := r.d.claim(token).SessionFile
	mark := r.markOf(t, claim.RoleImplementer)
	r.person(t, "done", "", true)
	r.note("a person closed %s after its first phase: an issue.updated envelope, status done", t)
	agent, task := r.retroTask(t, t, mark)
	r.requireR2(t, t, task.Message)
	r.until("T's architect suspended by the close", retroWait, func() bool {
		held := r.d.claim(token)
		return held.State == string(supervise.StateSuspended) && held.SuspendedBy == string(supervise.SuspendedByDaemon)
	})
	r.root.end()
	r.note("read back: one retro task went to %s; %s's architect is suspended by the close with session file %s kept", agent.token, t, file)

	finding := "a requirement the retro of " + t + " found unmet"
	status, body := r.listener.publish(map[string]any{
		"topic": notify.RoleTopicPrefix + string(token), "message": finding,
		"source": "agent", "source_session": agent.session, "idempotency_key": "retro-finding-" + t,
	})
	var answer struct {
		Kept       bool   `json:"kept"`
		KeptReason string `json:"kept_reason"`
	}
	if err := json.Unmarshal([]byte(body), &answer); err != nil || status != http.StatusAccepted || !answer.Kept {
		r.fail("the finding's publish to %s answered %d %s, want 202 kept for a role no live session holds", notify.RoleTopicPrefix+string(token), status, body)
	}
	r.note("the retro agent published its finding to %s through the Envoy listener: %d, kept (%s)", notify.RoleTopicPrefix+string(token), status, answer.KeptReason)
	var resumed *retroAgent
	r.until("the daemon resuming T's architect in its kept session on the listener's wake", retroWait, func() bool {
		launches := launchesSince(r.rt, token, 0)
		last := launches[len(launches)-1]
		return last.Method == "Resume" && last.Spec.ResumeSessionFile == file && last.Spec.BootToken != r.root.boot
	})
	resumed = r.register(t, t, claim.RoleArchitect)
	envoy := r.joinEnvoy(resumed)
	r.until("the kept finding delivered to the resumed architect's session", retroWait, func() bool { return envoy.got(finding) == 1 })
	wake := resumed.next()
	if prompts := resumed.promptsFrom(0); len(prompts) != 1 || !strings.Contains(wake.Message, retroArchitectWakeOpening) {
		r.fail("the resumed architect %s got %d tasks (first %q), want one retro wake task holding %q",
			token, len(prompts), wake.Message, retroArchitectWakeOpening)
	}
	r.note("read back: the daemon resumed %s from its kept session file %s on the listener's wake; the session took the role, was delivered the finding once and received the daemon's retro wake task once", token, file)
	r.root = resumed
	r.completesRetro(t, agent, mark, 5)
}

// member-open-at-root-close: step 3's child C2 left its retro unreported while the test root
// closed; the retro's agent kept its claim, and the report is taken although the tree lingers.
func (r *retroLive) memberOpenAtRootClose() {
	c, agent := r.member, r.memberAgent
	if c == "" || agent == nil {
		r.fail("step 3 left no retro to complete: it got no retro task after the close")
	}
	if held := r.d.claim(agent.token); held.State == string(supervise.StateRetired) || held.State == string(supervise.StateSuspended) {
		r.fail("the root's close stopped the retro's agent of %s: its claim is %s, want it kept while its retro is open", c, held.State)
	}
	r.note("read back: %s's retro agent %s stands %s after the root's close", c, agent.token, r.d.claim(agent.token).State)
	r.completesRetro(c, agent, taskMark{}, 4)
}

// root-closed-in-linger: a second root R2 runs into implementing, a person moves it to icebox and
// then closes it inside its linger: one task to its implementer, resumed in its kept session; the
// retro completes.
func (r *retroLive) rootClosedInLinger() {
	r.parkedRootClosed(r.r2, "icebox", false)
}

// expired-root-closed: a third root R3 runs into implementing and a person moves it to triage; the
// driver brings R3's linger end forward, so that T's and R2's lingers keep the default, and the
// linger's end retires R3's claims. A person's done then gets one task to its retro agent,
// relaunched in its kept session in a reopened lifecycle; the retro completes.
func (r *retroLive) expiredRootClosed() {
	r.parkedRootClosed(r.r3, "triage", true)
}

// parkedRootClosed is steps 12 and 13: root runs into implementing, a person parks it at status,
// and a person's done follows, inside its linger or, when expired, after the linger's end.
func (r *retroLive) parkedRootClosed(root, status string, expired bool) {
	r.waitIssue(root+" in planning", root, func(i api.Issue) bool { return i.Phase == phase.Planning })
	r.walkTo(root, phase.Implementing)
	implementer := r.registerTask(root, root, claim.RoleImplementer)
	implementer.endTurn()
	file := r.d.claim(implementer.token).SessionFile
	r.person(root, status, "", true)
	r.waitStatus(root, status)
	r.until("the implementer of "+root+" suspended with its session kept by the park", retroWait, func() bool {
		held := r.d.claim(implementer.token)
		return held.State == string(supervise.StateSuspended) && held.SuspendedBy == string(supervise.SuspendedByDaemon)
	})
	implementer.end()
	epoch := r.lifecycleEpoch(root)
	if expired {
		endLinger(r.t, r.st.Pool(), root, func(what string, cond func() bool) { r.until(what, retroWait, cond) })
		r.until("every claim of "+root+" retired by its linger's end", retroWait*3, func() bool {
			return r.d.claim(implementer.token).State == string(supervise.StateRetired)
		})
		r.note("read back: %s's linger ended early (its linger_close row brought due), and its implementer's claim is retired with its session kept", root)
	}
	mark := r.markOf(root, claim.RoleImplementer)
	time.Sleep(liveSettle)
	r.requireNoRetro(root, mark, "the move to "+status)
	r.person(root, "done", "", true)
	r.note("a person moved %s to %s, then closed it: issue.updated envelopes", root, status)
	agent, task := r.retroTask(root, root, mark)
	if kind := r.launchKind(agent.token); kind != "Resume" || r.launchOf(agent.token).ResumeSessionFile != file {
		r.fail("the retro's agent of %s was launched by a %s of %q, want a Resume of its kept session file %q", root, kind, r.launchOf(agent.token).ResumeSessionFile, file)
	}
	if expired {
		if after := r.lifecycleEpoch(root); after <= epoch {
			r.fail("%s's lifecycle stands at epoch %d after the close, want it reopened above epoch %d for the retro", root, after, epoch)
		}
	}
	r.requireTaskOnce(root, mark)
	r.note("read back: one retro task went to %s, resumed in its kept session file %s: %s", agent.token, file, firstLine(task.Message))
	r.completesRetro(root, agent, mark, 4)
}

// ---- the processes the driver plays --------------------------------------------------------------

// retroAgent is one claim's process as the driver plays it: the worker stream connection its shim
// holds, the session its registration named and the capability secret that registration returned,
// and every prompt the daemon sent it. An architect's turns end as soon as a task starts them, as
// Oh My Pi runs a task the daemon sends a session in no turn; every other role's turn runs until the
// driver ends it.
type retroAgent struct {
	r       *retroLive
	token   claim.Token
	issue   string
	tree    string
	role    claim.Role
	session string
	file    string
	boot    string
	secret  string
	sh      *shim
	n       int
	// writing serialises the frames serve and a step write; mu guards the rest.
	writing sync.Mutex
	mu      sync.Mutex
	prompts []shimwire.Prompt
	open    []string
	inTurn  bool
	taken   int
}

// register brings the launched claim of role on issue to ready, as its pane's shim and its agent
// do: it waits for a launch of it that no process of the driver's holds yet, dials the worker stream
// with that launch's boot token, registers the session (the one the claim kept, where it has one) and
// reports it ready.
func (r *retroLive) register(issue, tree string, role claim.Role) *retroAgent {
	r.t.Helper()
	token := r.token(issue, role)
	r.mu.Lock()
	prev := r.agents[token]
	r.mu.Unlock()
	known, session := "", "ses_"+string(role)+"_"+issue
	if prev != nil {
		known, session = prev.boot, prev.session
	}
	var spec runtime.SpawnSpec
	r.until("the runtime launching "+string(token), retroWait, func() bool {
		launches := launchesSince(r.rt, token, 0)
		if len(launches) == 0 {
			return false
		}
		spec = launches[len(launches)-1].Spec
		return spec.BootToken != known
	})
	return r.bring(issue, tree, role, session, spec)
}

// bring is register for the launch spec.
func (r *retroLive) bring(issue, tree string, role claim.Role, session string, spec runtime.SpawnSpec) *retroAgent {
	t := r.t
	t.Helper()
	token := r.token(issue, role)
	r.built.mu.Lock()
	address := r.built.address
	r.built.mu.Unlock()
	r.mu.Lock()
	r.registrations++
	n := r.registrations
	r.mu.Unlock()
	a := &retroAgent{r: r, token: token, issue: issue, tree: tree, role: role, session: session, file: "/sessions/" + session + ".jsonl",
		boot: spec.BootToken, n: n}
	a.sh = dialShim(t, address, spec.BootToken)
	r.until(string(token)+" with its shim connected", retroWait, func() bool {
		return r.d.claim(token).State == string(supervise.StateShimConnected)
	})
	status, body := r.d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: spec.BootToken, SessionID: session, OmpSessionFile: a.file, AgentID: session, PluginContract: api.DaemonAPIVersion(),
	}, false)
	if status != http.StatusOK {
		r.fail("register %s = %d %s, want 200", token, status, body)
	}
	var registered claim.RegisterResponse
	if err := json.Unmarshal(body, &registered); err != nil {
		r.fail("decode the registration of %s: %v", token, err)
	}
	a.secret = registered.Secret
	a.sh.serve(a.serve)
	if status, body := r.d.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: token, SessionID: session, Secret: a.secret, Generation: spec.Generation,
	}, false); status != http.StatusNoContent {
		r.fail("ready %s = %d %s, want 204", token, status, body)
	}
	r.mu.Lock()
	r.agents[token] = a
	r.mu.Unlock()
	return a
}

// serve reads every frame the daemon sends the shim until the connection closes or the test ends
// it: it answers the protocol negotiation and the state probe (idle, or streaming while a turn
// runs), and takes each prompt, answering it, naming the model, starting the turn as Oh My Pi does
// and recording it for a step to read.
func (a *retroAgent) serve() {
	reader := a.sh.lines
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
			if !a.send(shimwire.Response{ID: f.ID, Command: shimwire.TypeNegotiateProtocol, Success: true}) {
				return
			}
		case shimwire.GetState:
			a.mu.Lock()
			streaming := len(a.open) > 0 || a.inTurn
			a.mu.Unlock()
			data, _ := json.Marshal(shimwire.StateData{IsStreaming: &streaming})
			if !a.send(shimwire.Response{ID: f.ID, Command: shimwire.TypeGetState, Success: true, Data: data}) {
				return
			}
		case shimwire.Prompt:
			if !a.send(shimwire.Response{ID: f.ID, Command: shimwire.TypePrompt, Success: true}) {
				return
			}
			a.mu.Lock()
			a.prompts = append(a.prompts, f)
			alone := a.role == claim.RoleArchitect && !a.inTurn
			if !alone {
				a.open = append(a.open, f.ID)
			}
			a.mu.Unlock()
			a.answer()
			if !a.send(shimwire.AgentStart{DeliveryID: f.DeliveryID}) {
				return
			}
			if alone {
				a.send(shimwire.AgentEnd{})
				a.send(shimwire.PromptResult{ID: f.ID, AgentInvoked: true})
			}
		}
	}
}

// send writes one frame to the daemon, and says whether the connection took it.
func (a *retroAgent) send(frame shimwire.Frame) bool {
	a.writing.Lock()
	defer a.writing.Unlock()
	return a.sh.writer.WriteFrame(frame) == nil
}

// answer is the agent's model answering, which Oh My Pi's message_end names as message.provider and
// message.model.
func (a *retroAgent) answer() {
	provider, model, _ := strings.Cut(stepRecordsModel, "/")
	frame, err := json.Marshal(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "provider": provider, "model": model}})
	if err != nil {
		a.r.fail("encode the model's answer: %v", err)
	}
	a.send(shimwire.Raw{Type: "message_end", JSON: frame})
}

// endTurn ends every turn a prompt started: the agent_end, and the result of each prompt.
func (a *retroAgent) endTurn() {
	a.mu.Lock()
	open := a.open
	a.open = nil
	a.mu.Unlock()
	if len(open) == 0 {
		return
	}
	a.send(shimwire.AgentEnd{})
	for _, id := range open {
		a.send(shimwire.PromptResult{ID: id, AgentInvoked: true})
	}
}

// end is the agent's process ending: its shim's connection closes with it, which frees the claim's
// worker stream for the hello of the process that replaces it.
func (a *retroAgent) end() {
	_ = a.sh.conn.Close()
}

// next waits for the next prompt of this agent the step has not read, and answers it.
func (a *retroAgent) next() shimwire.Prompt {
	a.r.t.Helper()
	var prompt shimwire.Prompt
	a.r.until("a task for "+string(a.token), retroWait, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		if len(a.prompts) <= a.taken {
			return false
		}
		prompt = a.prompts[a.taken]
		a.taken++
		return true
	})
	return prompt
}

// promptsFrom is the prompts of this agent from the n-th on.
func (a *retroAgent) promptsFrom(n int) []shimwire.Prompt {
	a.mu.Lock()
	defer a.mu.Unlock()
	if n >= len(a.prompts) {
		return nil
	}
	return slices.Clone(a.prompts[n:])
}

// grant is a capability grant minted for this session now, as its `legion` tool mints one per
// operation: in its tree, for its own issue.
func (a *retroAgent) grant() string {
	a.r.t.Helper()
	status, body := a.r.d.request(http.MethodPost, "/legion/v1/grants",
		api.GrantRequest{SessionID: a.session, Secret: a.secret, Tree: a.tree, Issue: a.issue}, false)
	if status != http.StatusOK {
		a.r.fail("the grant of %s = %d %s, want 200", a.token, status, body)
	}
	var granted api.GrantResponse
	if err := json.Unmarshal(body, &granted); err != nil {
		a.r.fail("decode the grant of %s: %v", a.token, err)
	}
	return granted.GrantID
}

// post sends one request of this agent's and answers the status and body.
func (a *retroAgent) post(path string, body any) (int, string) {
	a.r.t.Helper()
	status, answered := a.r.d.request(http.MethodPost, path, body, false)
	return status, string(answered)
}

// complete reports this role's completion, as the `legion` tool's handoff_complete does, with a
// grant minted for it now: the status and the answer.
func (a *retroAgent) complete(req api.HandoffCompleteRequest) (int, string) {
	a.r.t.Helper()
	req.GrantID = a.grant()
	return a.post("/legion/v1/handoff/complete", req)
}

// completeRetroInPhase is the retro of an open issue, whose completion names the head the issue
// stands on and rates the plan, and the end of its turn.
func (a *retroAgent) completeRetroInPhase(rating int) {
	a.r.t.Helper()
	status, body := a.complete(api.HandoffCompleteRequest{Summary: "retro written", Commit: stepRecordsHead, PlanRating: new(rating), PlanReason: new("The plan fit the work.")})
	if status != http.StatusOK {
		a.r.fail("the retro's completion of %s was answered %d %s, want 200", a.issue, status, strings.TrimSpace(body))
	}
	a.endTurn()
}

// ownTurn is a turn of the architect's own, in which its model answers; it returns once the claim is
// idle again. The architect's sign-off follows such a turn (stepRecordsRig.signOff).
func (a *retroAgent) ownTurn() {
	a.r.t.Helper()
	a.mu.Lock()
	a.inTurn = true
	a.mu.Unlock()
	a.send(shimwire.AgentStart{})
	a.r.waitState(a.token, supervise.StateWorking)
	a.answer()
	a.send(shimwire.AgentEnd{})
	a.mu.Lock()
	a.inTurn = false
	a.mu.Unlock()
	a.r.waitState(a.token, supervise.StateIdle)
}

// waitState waits for the daemon's listing to hold the claim in state.
func (r *retroLive) waitState(token claim.Token, state supervise.ClaimState) {
	r.t.Helper()
	r.until(string(token)+" in state "+string(state), retroWait, func() bool { return r.d.claim(token).State == string(state) })
}

// retroEnvoy is the resumed root architect as a session of the real Envoy listener: its own NATS
// connection, subscribed to the relay subject the listener publishes each message of its durable
// consumer on, as a plugin in consumer mode is (idle_rig_agent_test.go's idleRigRoleProcess).
type retroEnvoy struct {
	mu       sync.Mutex
	messages []string
	seen     map[uint64]bool
}

// got is how many times the session was delivered a message holding text.
func (e *retroEnvoy) got(text string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, message := range e.messages {
		if strings.Contains(message, text) {
			n++
		}
	}
	return n
}

// joinEnvoy registers agent's session with the listener on the proof's broker and claims the role
// of its claim, which makes the listener hand the session the messages it kept for that role.
func (r *retroLive) joinEnvoy(agent *retroAgent) *retroEnvoy {
	t := r.t
	t.Helper()
	e := &retroEnvoy{seen: map[uint64]bool{}}
	bus := testnats.Connect(t, r.natsURL)
	reply := func(msg *nats.Msg, word string) {
		if err := bus.Publish(msg.Reply, []byte(word)); err == nil {
			_ = bus.Flush()
		}
	}
	if _, err := bus.Subscribe(idleRigDeliverSubjectPrefix+agent.session, func(msg *nats.Msg) {
		if msg.Header.Get(idleRigProbeHeader) == "1" {
			if msg.Reply != "" {
				reply(msg, idleRigRelayDone)
			}
			return
		}
		var item idleRigEnvelope
		if err := json.Unmarshal(msg.Data, &item); err != nil || msg.Reply == "" {
			return
		}
		sequence, err := strconv.ParseUint(msg.Header.Get(idleRigStreamSeqHeader), 10, 64)
		if err != nil {
			return
		}
		reply(msg, idleRigRelayReceived)
		e.mu.Lock()
		if !e.seen[sequence] {
			e.seen[sequence] = true
			e.messages = append(e.messages, item.text())
		}
		e.mu.Unlock()
		reply(msg, idleRigRelayDone)
	}); err != nil {
		r.fail("subscribe to the relay subject of %s: %v", agent.session, err)
	}
	if err := bus.Flush(); err != nil {
		r.fail("flush the subscription of %s: %v", agent.session, err)
	}
	status, body := r.listener.call("/v1/interests/subscribe", map[string]any{
		"session_id": agent.session, "dir": "/legion/workspaces/" + agent.issue, "topics": []string{},
		"host": "127.0.0.1", "title": string(agent.role), "self_subscribed": true, "capabilities": []string{"prompt_async"},
		"plugin": "retro-proof", "plugin_version": "0",
		"delivery": map[string]any{"consumer": true, "epoch": fmt.Sprintf("retro-proof-%d-%d", os.Getpid(), time.Now().UnixNano())},
	})
	var answer struct {
		Delivery *struct {
			Mode    string `json:"mode"`
			Subject string `json:"subject"`
		} `json:"delivery"`
	}
	if err := json.Unmarshal([]byte(body), &answer); err != nil || status != http.StatusOK || answer.Delivery == nil ||
		answer.Delivery.Mode != idleRigDeliveryConsumer || answer.Delivery.Subject != idleRigDeliverSubjectPrefix+agent.session {
		r.fail("the listener's registration of %s answered %d %s, want its own consumer relayed on %s", agent.session, status, body, idleRigDeliverSubjectPrefix+agent.session)
	}
	if status, body := r.listener.call("/v1/roles/set", map[string]any{"role": string(agent.token), "session_id": agent.session}); status != http.StatusOK {
		r.fail("the listener's claim of the role %s by %s answered %d %s, want 200", agent.token, agent.session, status, body)
	}
	return e
}

// ---- what a step drives --------------------------------------------------------------------------

// userActor is the actor of a person's move on Dispatch's dashboard.
var userActor = map[string]string{"kind": "user", "id": "operator"}

// newKey is the key of the next issue the proof makes.
func (r *retroLive) newKey() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.issues++
	return fmt.Sprintf("%s-%d", r.cfg.Project, r.issues)
}

// token is the claim token of role on issue, as the daemon spells it.
func (r *retroLive) token(issue string, role claim.Role) claim.Token {
	r.t.Helper()
	token, err := claim.NewToken(r.project, issue, role)
	if err != nil {
		r.t.Fatal(err)
	}
	return token
}

// publishIssue publishes Dispatch's issue.updated event of key at status under parent ("" for
// none), blocked_by blockers, on the next sequence, as actor's move makes one, and answers its event
// id. The event stands for a write already on Dispatch's board, so the board shows it first, and
// Dispatch's reads of key answer its links (linkedDispatch). A root carries the Legion label.
func (r *retroLive) publishIssue(actor map[string]string, key, status, parent string, blockers []string, handed bool) string {
	r.t.Helper()
	r.board.place(key, status, parent, handed)
	r.links.link(key, blockers)
	r.mu.Lock()
	r.seq++
	seq := r.seq
	r.mu.Unlock()
	var parentKey any
	if parent != "" {
		parentKey = parent
	}
	issue := map[string]any{
		"key": key, "status": status, "title": key, "parent": parentKey, "rank": "M",
		"labels": liveDispatchLabels(handed), "last_seq": seq, "project": r.cfg.Project, "resolved_repo": liveRepo,
	}
	if len(blockers) > 0 {
		issue["blocked_by"] = blockers
	}
	payload, err := json.Marshal(map[string]any{
		"id": seq, "issue_key": key, "project": r.cfg.Project, "seq": seq, "type": "issue.updated",
		"actor": actor, "notify": true, "created_at": time.Now().UTC().Format(time.RFC3339Nano), "payload": issue,
	})
	if err != nil {
		r.fail("encode the Dispatch event: %v", err)
	}
	return r.publish("notifications.dispatch.issue."+key+".issue.updated", "dispatch", fmt.Sprintf("%s-%s-%d", key, status, seq), string(payload))
}

// person is a person's move of key to status from the Dispatch dashboard.
func (r *retroLive) person(key, status, parent string, handed bool) {
	r.t.Helper()
	r.publishIssue(userActor, key, status, parent, r.links.of(key), handed)
}

// sessionWrite is by's own write of status on key in Dispatch, as Dispatch delivers it: the issue
// as the board holds it, with by's session as its actor. An architect writes a status in Dispatch
// itself with `dispatch issue-update`, and the daemon reads that write only from this event.
func (r *retroLive) sessionWrite(by *retroAgent, key, status string) {
	r.t.Helper()
	r.board.mu.Lock()
	issue := r.board.issues[key]
	r.board.mu.Unlock()
	r.publishIssue(map[string]string{"kind": "session", "id": by.session}, key, status, issue.parent, r.links.of(key), issue.handed)
}

// echo publishes the event of the daemon's own status write on key as Dispatch delivers it.
func (r *retroLive) echo(key string) {
	r.t.Helper()
	r.board.mu.Lock()
	issue := r.board.issues[key]
	r.board.mu.Unlock()
	r.publishIssue(map[string]string{"kind": "session", "id": dispatch.DaemonSession(key)}, key, issue.status, issue.parent, r.links.of(key), issue.handed)
}

// releaseBy is by's release_children of keys, as POST /legion/v1/waves/release with a grant by's
// session buys now, and the daemon's write of todo to each child echoed back as Dispatch delivers it.
func (r *retroLive) releaseBy(by *retroAgent, keys ...string) {
	r.t.Helper()
	status, body := by.post("/legion/v1/waves/release", api.WaveReleaseRequest{GrantID: by.grant(), Issues: keys})
	if status != http.StatusOK {
		r.fail("release_children of %v by %s's architect = %d %s, want 200", keys, by.issue, status, strings.TrimSpace(body))
	}
	for _, key := range keys {
		if !r.board.wrote(key, "todo") {
			r.fail("the release wrote no todo of %s to the Dispatch stand-in", key)
		}
		r.echo(key)
	}
}

// todoChild is a person's move of a new child under parent to todo, which starts its first phase;
// it answers the child's key once it is planning.
func (r *retroLive) todoChild(parent string) string {
	r.t.Helper()
	key := r.newKey()
	r.person(key, "todo", parent, false)
	r.startsPlanning(key)
	return key
}

// heldChild is a person's move of a new child under parent to todo with a blocked_by link to an open
// issue Legion does not work: it waits at admitted, its first phase not started.
func (r *retroLive) heldChild(parent string) string {
	r.t.Helper()
	blocker, key := r.newKey(), r.newKey()
	r.waitProcessedAny(r.publishIssue(userActor, blocker, "in_progress", "", nil, false))
	r.waitProcessedAny(r.publishIssue(userActor, key, "todo", parent, []string{blocker}, false))
	r.waitIssue(key+" at admitted, blocked on "+blocker, key, func(i api.Issue) bool {
		return i.Phase == phase.Admitted && i.Why != nil && i.Why.State == api.WhyBlocked && strings.Contains(i.Why.Reason, blocker)
	})
	return key
}

// startsPlanning waits for key's first phase to start: the daemon shows it in planning.
func (r *retroLive) startsPlanning(key string) {
	r.t.Helper()
	r.waitIssue(key+" in planning", key, func(i api.Issue) bool { return i.Phase == phase.Planning })
}

// waitIssue waits for GET /legion/v1/state to show key as want says.
func (r *retroLive) waitIssue(what, key string, want func(api.Issue) bool) {
	r.t.Helper()
	var last api.Issue
	if !testwait.Holds(retroWait, retroPoll, func() bool {
		issue, ok := r.d.state().Issues[key]
		last = issue
		return ok && want(issue)
	}) {
		r.fail("%s: %s is %s/%s, not within %s", what, key, last.Phase, last.Status, retroWait)
	}
}

func (r *retroLive) waitPhase(key string, want phase.Phase) {
	r.t.Helper()
	r.waitIssue(key+" in "+string(want), key, func(i api.Issue) bool { return i.Phase == want })
}

func (r *retroLive) waitStatus(key, want string) {
	r.t.Helper()
	r.waitIssue(key+" at "+want, key, func(i api.Issue) bool { return i.Status == want })
}

// waitProcessedAny waits for the daemon's processed_events row of the envelope eventID.
func (r *retroLive) waitProcessedAny(eventID string) {
	r.t.Helper()
	r.until("the daemon taking the envelope "+eventID, retroWait, func() bool {
		var taken bool
		if err := r.st.Pool().QueryRow(context.Background(), "select exists(select 1 from processed_events where event_id = $1)", eventID).Scan(&taken); err != nil {
			r.t.Fatalf("read the processed event %s: %v", eventID, err)
		}
		return taken
	})
}

// ---- the walk of an issue through its steps ------------------------------------------------------

// walkTo plays key, an issue whose first phase started, through its steps up to the phase to: the
// planner's plan, the implementer's push, the tester's pass, the reviewer's turn and a person's
// approval, and the architect's sign-off. Each role's process registers when the daemon launches its
// claim and takes the task the daemon sends it.
func (r *retroLive) walkTo(key string, to phase.Phase) {
	r.t.Helper()
	tree := r.treeOf(key)
	for _, step := range []struct {
		at   phase.Phase
		next phase.Phase
		do   func()
	}{
		{phase.Planning, phase.Implementing, func() {
			r.complete(key, tree, claim.RolePlanner, "plan", api.HandoffCompleteRequest{Summary: "planned"})
		}},
		{phase.Implementing, phase.Testing, func() {
			r.openPullRequest(key)
			r.complete(key, tree, claim.RoleImplementer, "implement", api.HandoffCompleteRequest{Summary: "implemented", Commit: stepRecordsHead})
		}},
		{phase.Testing, phase.Reviewing, func() {
			r.complete(key, tree, claim.RoleTester, "test", api.HandoffCompleteRequest{Summary: "every criterion holds", Verdict: "pass"})
		}},
		{phase.Reviewing, phase.Accepting, func() {
			reviewer := r.registerTask(key, tree, claim.RoleReviewer)
			reviewer.endTurn()
			r.publishReview(r.pullRequestOf(key))
		}},
		{phase.Accepting, phase.Retro, func() { r.signOff(key) }},
	} {
		if r.d.state().Issues[key].Phase != step.at {
			continue
		}
		step.do()
		r.waitPhase(key, step.next)
		if step.next == to {
			return
		}
	}
	if got := r.d.state().Issues[key].Phase; got != to {
		r.fail("%s stands in %s, want it walked to %s", key, got, to)
	}
}

// treeOf is the tree key belongs to: a root's own, the test root's for every child.
func (r *retroLive) treeOf(key string) string {
	if key == r.r2 || key == r.r3 {
		return key
	}
	return r.tree
}

// registerTask brings role's claim on key up and answers it once the daemon has sent it its task.
func (r *retroLive) registerTask(key, tree string, role claim.Role) *retroAgent {
	r.t.Helper()
	agent := r.register(key, tree, role)
	agent.next()
	return agent
}

// complete plays role's turn on key: it takes the task, writes its handoff record of word to
// Dispatch under its own session (none for word ""), completes, and ends its turn.
func (r *retroLive) complete(key, tree string, role claim.Role, word string, req api.HandoffCompleteRequest) {
	r.t.Helper()
	agent := r.registerTask(key, tree, role)
	if word != "" {
		req.HandoffID, req.HandoffVersion = r.board.handoff(key, word, agent.session), 1
	}
	if status, body := agent.complete(req); status != http.StatusOK {
		r.fail("the %s's completion of %s's %s = %d %s, want 200", role, key, word, status, strings.TrimSpace(body))
	}
	agent.endTurn()
}

// openPullRequest opens key's pull request on its issue branch at the head the implementer pushes.
func (r *retroLive) openPullRequest(key string) {
	r.t.Helper()
	r.mu.Lock()
	r.number++
	number := r.number
	r.mu.Unlock()
	r.github.open(claim.IssueBranch(key), number)
}

// pullRequestOf is the number of key's pull request.
func (r *retroLive) pullRequestOf(key string) int {
	r.t.Helper()
	for place, answer := range r.github.pullRequests() {
		var pull struct {
			Number int `json:"number"`
			Head   struct {
				Ref string `json:"ref"`
			} `json:"head"`
		}
		if err := json.Unmarshal([]byte(answer), &pull); err != nil {
			r.fail("decode pull request %d: %v", place, err)
		}
		if pull.Head.Ref == claim.IssueBranch(key) {
			return pull.Number
		}
	}
	r.fail("GitHub holds no pull request on %s", claim.IssueBranch(key))
	return 0
}

// publishReview is a person with write access approving pull request number at stepRecordsHead, as
// Envoy relays GitHub's review.
func (r *retroLive) publishReview(number int) {
	r.t.Helper()
	r.mu.Lock()
	r.reviews++
	round := r.reviews
	r.mu.Unlock()
	payload, err := json.Marshal(map[string]string{
		"action": "submitted", "kind": "review", "repo": liveRepo, "number": strconv.Itoa(number), "state": "approved",
		"commit_id": stepRecordsHead, "head_sha": stepRecordsHead, "author": stepRecordsPerson, "body": "round " + strconv.Itoa(round),
		"review_id": strconv.Itoa(number*10 + round), "submitted_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		r.fail("encode the review: %v", err)
	}
	r.publish(fmt.Sprintf("notifications.github.%s.pr.%d.review", r.subjectRepo(), number), "github", fmt.Sprintf("review-%d-%d", number, round), string(payload))
}

// signOff is the test root's architect answering in a turn of its own and signing off issue, a
// member of its tree, through its own route; issue then reaches its retro.
func (r *retroLive) signOff(issue string) {
	r.t.Helper()
	r.root.ownTurn()
	status, body := r.root.post("/legion/v1/signoff", api.SignOffRequest{GrantID: r.root.grant(), Issue: issue})
	if status != http.StatusOK {
		r.fail("the architect's sign-off of %s = %d %s, want 200", issue, status, strings.TrimSpace(body))
	}
	r.waitPhase(issue, phase.Retro)
}

// mergeOf waits for the merger's launch and publishes GitHub's merge of key's pull request.
func (r *retroLive) mergeOf(key string) {
	r.t.Helper()
	token := r.token(key, claim.RoleMerger)
	r.until("the runtime launching "+string(token), retroWait, func() bool { return len(launchesSince(r.rt, token, 0)) > 0 })
	number := r.pullRequestOf(key)
	r.github.merged(number)
	r.publishMergeOf(number, key, stepRecordsHead, "main")
}

// ---- what a step reads ---------------------------------------------------------------------------

// taskMark is what the driver held of a claim's process when an issue closed, so that what came after
// the close is told from what came before it: the process the claim had (nil for none), how many
// prompts it held, how many launches the claim had had, how many processes the driver had played,
// how many status writes Dispatch's stand-in held, and how many of the issue's run_retros rows held
// a retro after a close (retrosAfterClose).
type taskMark struct {
	agent         *retroAgent
	prompts       int
	launches      int
	registrations int
	writes        int
	retros        int
}

// markOf is the mark of role's claim on issue now.
func (r *retroLive) markOf(issue string, role claim.Role) taskMark {
	r.t.Helper()
	token := r.token(issue, role)
	retros, _ := r.retrosAfterClose(issue)
	r.mu.Lock()
	defer r.mu.Unlock()
	mark := taskMark{agent: r.agents[token], launches: len(launchesSince(r.rt, token, 0)), registrations: r.registrations, writes: len(r.board.writes()),
		retros: retros}
	if mark.agent != nil {
		mark.prompts = len(mark.agent.promptsFrom(0))
	}
	return mark
}

// retrosAfterClose is how many of issue's run_retros rows hold a retro after a close, and how many
// of those are open, read through SQL. A database with no run_retros table, main's, holds none.
func (r *retroLive) retrosAfterClose(issue string) (opened, open int) {
	r.t.Helper()
	ctx := context.Background()
	var table bool
	if err := r.st.Pool().QueryRow(ctx, "select to_regclass('run_retros') is not null").Scan(&table); err != nil {
		r.fail("look for the run_retros table: %v", err)
	}
	if !table {
		return 0, 0
	}
	if err := r.st.Pool().QueryRow(ctx, `select count(*) filter (where opened_at is not null),
		count(*) filter (where opened_at is not null and ended_at is null) from run_retros where issue = $1`, issue).Scan(&opened, &open); err != nil {
		r.fail("read the run_retros rows of %s: %v", issue, err)
	}
	return opened, open
}

// retroTasksSince is how many retro tasks role's claim on issue was given after mark: a prompt
// naming the phase retro, and, for a retro after a close, saying that the issue is closed.
func (r *retroLive) retroTasksSince(issue string, role claim.Role, mark taskMark, closed bool) int {
	r.t.Helper()
	token := r.token(issue, role)
	r.mu.Lock()
	agents := slices.Collect(func(yield func(*retroAgent) bool) {
		for _, a := range r.agents {
			if a.token == token && !yield(a) {
				return
			}
		}
	})
	r.mu.Unlock()
	count := 0
	for _, a := range agents {
		from := 0
		switch {
		case a == mark.agent:
			from = mark.prompts
		case a.n <= mark.registrations && mark.registrations > 0:
			continue
		}
		for _, p := range a.promptsFrom(from) {
			if retroTaskText(p.Message, closed) {
				count++
			}
		}
	}
	return count
}

// retroTaskText says whether message is a retro's task, and, with closed, one after a close.
func retroTaskText(message string, closed bool) bool {
	return strings.Contains(message, "Phase: retro.") && (!closed || strings.Contains(message, retroClosedClause))
}

// retroTask waits for the retro task the daemon sends issue's implementer after its close, and
// answers the process that was given it and the prompt: the claim's process the driver holds, or the
// one it plays now for a launch the close made, a spawn of a claim that never launched or a resume of
// one the close or the park suspended.
func (r *retroLive) retroTask(issue, tree string, mark taskMark) (*retroAgent, shimwire.Prompt) {
	r.t.Helper()
	token := r.token(issue, claim.RoleImplementer)
	var found *retroAgent
	var task shimwire.Prompt
	if !testwait.Holds(retroWait, retroPoll, func() bool {
		if launches := launchesSince(r.rt, token, 0); len(launches) > mark.launches {
			r.mu.Lock()
			current := r.agents[token]
			r.mu.Unlock()
			if latest := launches[len(launches)-1].Spec; current == nil || current.boot != latest.BootToken {
				session := "ses_" + string(claim.RoleImplementer) + "_" + issue
				if current != nil {
					session = current.session
				}
				r.bring(issue, tree, claim.RoleImplementer, session, latest)
			}
		}
		r.mu.Lock()
		a := r.agents[token]
		r.mu.Unlock()
		if a == nil {
			return false
		}
		from := 0
		if a == mark.agent {
			from = mark.prompts
		}
		for _, p := range a.promptsFrom(from) {
			if retroTaskText(p.Message, true) {
				found, task = a, p
				return true
			}
		}
		return false
	}) {
		r.fail("no retro task came to the implementer of %s after its close, within %s", issue, retroWait)
	}
	return found, task
}

// requireTaskOnce holds that, after the settle, the implementer of issue was given exactly one retro
// task after mark (requireOneRetroTask).
func (r *retroLive) requireTaskOnce(issue string, mark taskMark) {
	r.t.Helper()
	time.Sleep(liveSettle)
	r.requireOneRetroTask(issue, mark)
}

// requireOneRetroTask holds that the implementer of issue has been given exactly one retro task
// after mark, now.
func (r *retroLive) requireOneRetroTask(issue string, mark taskMark) {
	r.t.Helper()
	if got := r.retroTasksSince(issue, claim.RoleImplementer, mark, true); got != 1 {
		r.fail("the implementer of %s was given %d retro tasks after the close, want exactly one", issue, got)
	}
}

// requireNoRetro holds that nothing of a retro came to issue after mark: no retro task to its
// implementer, no launch of it, and no retro row: its run_retros rows hold no retro after a close
// that they did not hold at mark, and none open (what is the cause names).
func (r *retroLive) requireNoRetro(issue string, mark taskMark, cause string) {
	r.t.Helper()
	token := r.token(issue, claim.RoleImplementer)
	if got := r.retroTasksSince(issue, claim.RoleImplementer, mark, true); got != 0 {
		r.fail("the implementer of %s was given %d retro tasks after %s, want none", issue, got, cause)
	}
	if launches := len(launchesSince(r.rt, token, 0)); launches != mark.launches {
		r.fail("the runtime was asked for %d launches of %s's implementer after %s, want none", launches-mark.launches, issue, cause)
	}
	if opened, open := r.retrosAfterClose(issue); opened != mark.retros || open != 0 {
		r.fail("%s's run_retros rows hold %d retros after a close, %d of them open, after %s; want the %d they held before and none open",
			issue, opened, open, cause, mark.retros)
	}
}

// requireR2 holds that task, the retro task after issue's close, says each clause of R2 and names
// the root architect's role topic of tree; owning is the architect of the issue above, whose role
// topic it must not name.
func (r *retroLive) requireR2(issue, tree, task string, owning ...*retroAgent) {
	r.t.Helper()
	topic := notify.RoleTopicPrefix + string(r.token(tree, claim.RoleArchitect))
	r.requireTaskHolds(issue, task, "the test root's architect's role topic "+topic, topic, retroClosedClause, retroNowhereClause, retroSkillClause, retroFindingClause, retroPendingClause)
	for _, architect := range owning {
		if other := notify.RoleTopicPrefix + string(architect.token); strings.Contains(task, other) {
			r.fail("%s's retro task names %s, the topic of the architect above it, want the test root's %s alone", issue, other, topic)
		}
	}
}

// requireTaskHolds holds that task says each of wants, naming the first it does not, with what it
// is.
func (r *retroLive) requireTaskHolds(issue, task, what string, wants ...string) {
	r.t.Helper()
	for place, want := range wants {
		if !strings.Contains(task, want) {
			if place == 0 {
				r.fail("%s's retro task does not name %s: %s", issue, what, firstLine(task))
			}
			r.fail("%s's retro task does not say %q: %s", issue, want, task)
		}
	}
}

// requireStatusStays holds that key stands in the status the close left it: done in the daemon's
// state and on the board, with no status write of the daemon's after mark, the moment before the
// close.
func (r *retroLive) requireStatusStays(key, status string, mark taskMark) {
	r.t.Helper()
	got := r.d.state().Issues[key]
	if got.Status != status || got.Phase != phase.Done {
		r.fail("%s stands in %s/%s after its retro, want it %s as the close left it", key, got.Phase, got.Status, status)
	}
	r.board.mu.Lock()
	shown := r.board.issues[key].status
	r.board.mu.Unlock()
	if shown != status {
		r.fail("Dispatch shows %s at %s after its retro, want %s", key, shown, status)
	}
	for _, write := range r.board.writes()[mark.writes:] {
		if write.Issue == key && write.Status != "" {
			r.fail("the daemon set %s to %s after its close", key, write.Status)
		}
	}
	r.note("read back: %s stays %s in the daemon's state and on Dispatch's board", key, status)
}

// launchOf is the spec of the claim's latest launch.
func (r *retroLive) launchOf(token claim.Token) runtime.SpawnSpec {
	launches := launchesSince(r.rt, token, 0)
	if len(launches) == 0 {
		return runtime.SpawnSpec{}
	}
	return launches[len(launches)-1].Spec
}

// launchKind is whether the claim's latest launch was a Spawn or a Resume.
func (r *retroLive) launchKind(token claim.Token) string {
	launches := launchesSince(r.rt, token, 0)
	if len(launches) == 0 {
		return "no launch"
	}
	return launches[len(launches)-1].Method
}

// retroRows is every retro record of issue the daemon wrote to the Dispatch stand-in, in the order of
// their first write, each as its fields stand.
func (r *retroLive) retroRows(issue string) []map[string]any {
	b := r.board
	b.mu.Lock()
	defer b.mu.Unlock()
	var rows []map[string]any
	for _, id := range b.stepOrder {
		row := b.steps[id]
		if row["issue"] != issue || row["kind"] != "retro" {
			continue
		}
		copied := map[string]any{}
		for field, value := range row {
			copied[field] = value
		}
		rows = append(rows, copied)
	}
	return rows
}

// retroRowWant is what a retro's row must show: its result, and, for a retro that rated the plan, the
// rating (none for a rating of 0, which no row carries).
type retroRowWant struct {
	result string
	rating int
}

// awaitRetroRow waits for a retro record of issue after the first `from` to show its start, its end
// with want's result and, for a rated retro, the plan rating the daemon recorded with it; a row that
// ends with a result of its own and carries no rating must say none.
func (r *retroLive) awaitRetroRow(issue string, from int, want retroRowWant) {
	r.t.Helper()
	var last []map[string]any
	if !testwait.Holds(retroWait, retroPoll, func() bool {
		rows := r.retroRows(issue)
		if len(rows) > from {
			last = rows[from:]
		}
		for _, row := range last {
			if row["started_at"] == nil || row["ended_at"] == nil || row["result"] != want.result {
				continue
			}
			if want.rating == 0 {
				return row["rating"] == nil
			}
			return row["rating"] == float64(want.rating) && row["rating_reason"] != nil
		}
		return false
	}) {
		r.fail("%s's retro row did not show its start and its end %s%s in Dispatch's step rows within %s; the rows: %v", issue, want.result,
			map[bool]string{true: fmt.Sprintf(" with the plan rating %d", want.rating)}[want.rating != 0], retroWait, last)
	}
}

// requireRetroStarted holds that issue has a retro record after the first `from` that started and
// has not ended.
func (r *retroLive) requireRetroStarted(issue string, from int) {
	r.t.Helper()
	var rows []map[string]any
	if !testwait.Holds(retroWait, retroPoll, func() bool {
		if rows = r.retroRows(issue); len(rows) <= from {
			return false
		}
		return rows[from]["started_at"] != nil && rows[from]["ended_at"] == nil
	}) {
		r.fail("%s's retro row did not show its start and no end in Dispatch's step rows within %s; the rows: %v", issue, retroWait, rows)
	}
}

// completesRetro is the retro of issue completing, as its agent's report: the completion with a plan
// rating and its reason, from the process that was given the task, answered 200; the retro's row
// ending passed with the rating the daemon recorded for it; and then the retro's task, once,
// counted from mark, the one the agent was given (requireOneRetroTask). The count is read once the
// retro has ended, so no settle of its own is spent on it: a second task would have come while
// the completion and the row were waited for, and none comes after the end.
func (r *retroLive) completesRetro(issue string, agent *retroAgent, mark taskMark, rating int) {
	r.t.Helper()
	before := len(r.retroRows(issue))
	status, body := agent.complete(api.HandoffCompleteRequest{Summary: "retro written", PlanRating: new(rating), PlanReason: new("The plan fit the work.")})
	if status != http.StatusOK {
		r.fail("the retro's completion with the plan rating %d was answered %d %s, want 200", rating, status, strings.TrimSpace(body))
	}
	agent.endTurn()
	r.awaitRetroRow(issue, max(before-1, 0), retroRowWant{result: "passed", rating: rating})
	r.requireOneRetroTask(issue, mark)
	r.note("read back: the retro's completion by %s was answered 200; Dispatch's retro row of %s shows its start and its end passed, rated %d; it was given the one retro task", agent.token, issue, rating)
}

// lifecycleEpoch is the epoch of root's tree lifecycle now.
func (r *retroLive) lifecycleEpoch(root string) int64 {
	r.t.Helper()
	var epoch int64
	if err := r.st.Pool().QueryRow(context.Background(), "select epoch from tree_lifecycles where tree = $1", root).Scan(&epoch); err != nil {
		r.fail("read the lifecycle of %s: %v", root, err)
	}
	return epoch
}
