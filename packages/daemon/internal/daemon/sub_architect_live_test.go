//go:build e2e

// The live proof's driver for INFRA-3915: every child that starts runs under an architect of its
// own. A child's first phase start starts that architect beside the child's planner, whichever
// door brought the child in: its architect's release_children, a person's todo under a live tree,
// or the close of the issue it waited on (workflow's Engine.StartFirstPhase). At each launch's
// ready the child's architect is told its own issue and every issue under it, in a catch-up
// published to its own topic alone, and the daemon writes one `outbox notice published` line for
// each notice it publishes. The child's architect files its own children under its issue and
// releases them with its own grant, and a grandchild's notices reach it rather than the root's
// architect. It may rerun and release only issues under its own issue, and is answered 403
// ISSUE_NOT_OWNED for any other. Its own status write in Dispatch on an issue under its own issue
// stands, and the daemon sets back its write on its sibling and on the root. When its claim fails
// because its launches ran out, the architect above it is told, the child's notices go there too,
// and the child's next phase start retries the claim in the same session with fresh budgets. A
// child's architect is suspended with the child when the root's architect writes backlog on the
// child, and the child's rerun resumes that session.
// scripts/e2e/sub-architect.sh builds this driver with `-tags e2e` and owns the nats-server and
// the PostgreSQL it runs against, through the run lib/live-daemon-proof.sh makes around every
// whole-daemon live proof.
//
// The daemon is the whole daemon: run, the function `legion start` calls, through this package's
// whole-daemon harness (startDaemonLogging), so the proof exercises the real configuration load,
// the real store and its migrations, the real HTTP API, the real claim supervision, the real
// openWorkflow, the real JetStream consumers and the real outbox lanes. Facts enter only by the
// transports production has: the Dispatch envelopes of the ENVOY_NOTIFICATIONS stream, the
// daemon's own HTTP routes, its worker stream, and the runtime's own report of a process. A step
// reads GET /legion/v1/state for each issue's phase, generation, slot and architect, the
// operator's claim listing for a claim's state, session and budgets, what the notice listener
// stand-in took on each architect's topic, what the daemon wrote to the Dispatch stand-in, the
// launches the fake runtime was asked for, the daemon's own log records, and, for an issue's
// recorded parent, which no route shows, the daemon's own record of it in its PostgreSQL.
//
// What the proof stands in for lies outside the change. The broker is a native nats-server the
// script starts and names in LEGION_E2E_NATS_URL. Dispatch is liveDispatch, the board the
// package's whole-daemon live proofs share (dispatch_board_test.go), fronted by linkedDispatch,
// which adds to each read of an issue the blocked_by links the driver gave it, and Envoy's
// listener is the package's noticeListener. GitHub is child_adoption_live_test.go's
// childAdoptionGitHub, which knows each repository's default branch and takes each issue branch
// the workflow creates. No agent runs: every claim is launched by the package's fake runtime
// (subArchitectRuntime, which refuses the relaunches the failure step names), and the driver plays
// each claim a step needs over the daemon's own worker stream and HTTP routes
// (subArchitectSession), as a pane's shim and its agent do. The daemon's timers never fire
// (fakeRuntime's stillClock), so a claim the driver does not play stays launching rather than
// failing on its boot deadline. The tree's root, its design gate, its slot and its architect's
// unfinished launch are seeded through the store, as every daemon test seeds its fixture; every
// child is recorded by the daemon from its own envelopes.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

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
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

const (
	// subArchitectCap is the admission cap: one slot for the tree's root and one for each of the
	// five children the proof starts, so no child waits in the line for a slot. A child takes a
	// slot of its own (INFRA-980), and its architect starts only with its first phase, which is
	// what this proof reads.
	subArchitectCap = 6
	// subArchitectGate is the root's approved design document, which lets its children start.
	subArchitectGate = "7d2e4f6a-8b1c-4e3d-9a5f-0c2b4d6e8f1a"
	// subArchitectOwnership is the sentence every child-status notice of a child's start ends
	// with (workflow.ChildStartedNotice): the child's own architect owns it while that architect
	// holds its role, so the architect told hears only what goes above that architect.
	subArchitectOwnership = "Its own architect starts with it and owns it while that architect holds its role: its " +
		"decomposition, its children and its acceptance. You hear its child-status and child-closed notices, a worker-died or " +
		"worker-stalled of its architect, and its other notices while its architect does not hold its role, which the daemon's " +
		"holdsRole decides"
)

// subArchitectLive is one run of the proof: the booted daemon, the one live tree it runs, the
// claims the driver plays, and the stand-ins the steps read the daemon's writes back from.
type subArchitectLive struct {
	liveProof
	rt     *subArchitectRuntime
	built  *built
	logged *recordingLog
	links  *linkedDispatch
	// tree is the root, T, in planning with its slot and its approved design gate. child is C,
	// which T's architect releases and whose own architect the later steps play; sibling is S,
	// released beside it; grandchild is G, which C's architect files and releases; todoChild is D,
	// which a person sets todo; heldChild is E, which a blocked_by link to blocker, B, holds; B
	// is an issue outside the tree, which Legion does not work.
	tree, child, sibling, grandchild, todoChild, heldChild, blocker string
	// root, childArchitect and todoArchitect are the architect sessions of T, C and D.
	root, childArchitect, todoArchitect *subArchitectSession
	// seq is the Dispatch sequence the next envelope the driver publishes carries.
	seq int64
}

// TestEveryChildThatStartsRunsUnderItsOwnArchitectLive drives the seven steps of the proof
// against one booted daemon.
func TestEveryChildThatStartsRunsUnderItsOwnArchitectLive(t *testing.T) {
	// Not parallel: it gives the process back the operator's own environment, as t.Setenv does (fixtureenv.Live).
	fixtureenv.Live(t)
	r := newSubArchitectLive(t)
	r.step("release", r.releaseStep)
	r.step("grandchild", r.grandchildStep)
	r.step("ownership", r.ownershipStep)
	r.step("todo", r.todoStep)
	r.step("blocker", r.blockerStep)
	r.step("failure", r.failureStep)
	r.step("rerun", r.rerunStep)
}

// newSubArchitectLive boots the daemon over the script's broker with the live tree recorded, and
// registers and readies the tree's architect.
func newSubArchitectLive(t *testing.T) *subArchitectLive {
	t.Helper()
	proof := newLiveProof(t, "scripts/e2e/sub-architect.sh")
	proof.cfg.AdmissionCap = subArchitectCap
	links := &linkedDispatch{board: proof.board, links: map[string][]string{}}
	front := httptest.NewServer(links)
	t.Cleanup(front.Close)
	proof.cfg.DispatchURL = front.URL
	rt := &subArchitectRuntime{Runtime: fake.NewRuntime(), refusals: map[claim.Token]*resumeRefusal{}}
	rt.InPod = true
	key := func(n int) string { return fmt.Sprintf("%s-%d", proof.cfg.Project, n) }
	r := &subArchitectLive{
		liveProof: proof, rt: rt, built: &built{}, links: links,
		tree: key(1), child: key(2), sibling: key(3), grandchild: key(4), todoChild: key(5), heldChild: key(6), blocker: key(7),
		seq: 1,
	}
	r.seed()
	o := fakeRuntime(r.rt, r.built)
	o.workflowTokens = &workflowTokenRecorder{}
	o.githubAPI = childAdoptionGitHub(t)
	r.logged = newRecordingLog(r.log())
	r.d = startDaemonLogging(t, r.cfg, o, slog.New(r.logged))
	r.root = r.register(r.tree, claim.RoleArchitect, "ses_root_architect", 0)
	r.note("registered and readied %s's architect as session %s", r.tree, r.root.session)
	return r
}

// seed records T as the daemon would hold it mid-run: in planning with its slot, its approved
// design gate, its lifecycle open under the workflow and its architect claim as a launch a
// previous daemon began and never recorded, which the boot relaunches.
func (r *subArchitectLive) seed() {
	t := r.t
	t.Helper()
	ctx := context.Background()
	r.board.show(r.tree, "in_progress")
	approved := 1
	if err := store.Transact(ctx, r.st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		if err := r.records.PutIssue(ctx, tx, record.Issue{
			Key: r.tree, Repo: liveRepo, DefaultBranch: "main", GitHubRepo: liveRepo, Tree: r.tree, Project: r.cfg.Project,
			Title: r.tree, Phase: phase.Planning, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress", Rank: "A",
			LastDispatchSeq: r.seq, HandedOver: true,
		}); err != nil {
			return err
		}
		if err := r.admit(ctx, tx, r.tree); err != nil {
			return err
		}
		return r.records.PutGate(ctx, tx, record.DesignGate{Issue: r.tree, ArtifactID: subArchitectGate, LatestVersion: 1, ApprovedVersion: &approved})
	}); err != nil {
		t.Fatalf("seed the live tree %s: %v", r.tree, err)
	}
	if _, err := r.st.OpenTreeLifecycle(ctx, r.project, r.tree, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("open the lifecycle of %s: %v", r.tree, err)
	}
	architect := r.token(r.tree, claim.RoleArchitect)
	if _, err := r.st.AdmitClaim(ctx, supervise.Claim{
		Token: architect, Project: r.project, Tree: r.tree, Issue: r.tree, Role: claim.RoleArchitect,
		Generation: 1, State: supervise.StateLaunching, BootTokenHash: supervise.HashBootToken("interrupted-" + randomSuffix(t)),
	}); err != nil {
		t.Fatalf("record the architect's unfinished launch: %v", err)
	}
	writePrompt(t, r.cfg, architect)
	r.note("seeded: %s in planning on its admission slot (cap %d), its design gate approved, its lifecycle open, its architect's launch unfinished",
		r.tree, subArchitectCap)
}

// ---- the steps -----------------------------------------------------------------------------------

// release: T's architect releases C and S, two triage children Dispatch shows under T, with its
// own grant. Each starts planning on a slot of its own with an architect claim of its own, which
// its first phase start starts. C's architect, once ready, is told in a catch-up on its own topic,
// naming C alone, and the daemon logs that publish with the catch-up's generation, its launch and
// its one issue; T's architect hears that C started and that C's own architect owns it while that
// architect holds its role.
func (r *subArchitectLive) releaseStep() {
	t := r.t
	c, s := r.child, r.sibling
	r.board.place(c, "triage", r.tree, false)
	r.board.place(s, "triage", r.tree, false)
	r.note("the Dispatch stand-in shows %s and %s in triage under %s; the daemon records neither", c, s, r.tree)
	if status, failure := r.release(r.root, c, s); status != http.StatusOK {
		t.Fatalf("release_children of %s and %s by %s's architect = %d %s %s, want 200", c, s, r.tree, status, failure.Code, failure.Error)
	}
	r.note("drove POST /legion/v1/waves/release {%s, %s} with a grant of %s's architect: 200", c, s, r.tree)
	for _, key := range []string{c, s} {
		if !r.board.wrote(key, "todo") {
			t.Fatalf("the release wrote no todo of %s to the Dispatch stand-in; writes %+v", key, r.board.writes())
		}
		r.echo(key)
	}
	r.note("read back: the stand-in took the release's PATCH of %s and of %s to todo; drove each write's own event, actor %s",
		c, s, dispatch.DaemonSession(c))
	for _, key := range []string{c, s} {
		r.startsWithItsArchitect(key)
	}
	r.childArchitect = r.register(c, claim.RoleArchitect, "ses_c_architect", 0)
	r.note("registered and readied %s's architect as session %s on its launch %d", c, r.childArchitect.session, r.childArchitect.launch)
	told := r.waitCatchUp(c, r.childArchitect.launch)
	r.catchUpNames(c, told, c)
	r.catchUpOnItsOwnTopicAlone(c)
	line := r.loggedPublish("catch-up", c, r.token(c, claim.RoleArchitect))
	want := fmt.Sprintf("generation=%d launch=%d issues=1", told.CatchUp.Generation, r.childArchitect.launch)
	if got := fmt.Sprintf("generation=%v launch=%v issues=%v", line.Attrs["generation"], line.Attrs["launch"], line.Attrs["issues"]); got != want {
		t.Fatalf("the daemon's line for %s's catch-up names %s, want %s", c, got, want)
	}
	started := r.waitTold(r.tree, "the child-status notice that "+c+" started", func(n record.Notice) bool {
		return n.Kind == "child-status" && strings.HasPrefix(n.Reason, c+" starts planning under "+r.tree)
	})
	if !strings.HasSuffix(started.Reason, ". "+subArchitectOwnership) {
		t.Fatalf("%s's architect was told that %s started as %q, want it to end %q", r.tree, c, started.Reason, subArchitectOwnership)
	}
	r.note("read back: a child-status notice on %s's architect topic — %s", r.tree, started.Reason)
}

// grandchild: C's architect files G under C and releases it with its own grant. G starts planning
// with an architect of its own, and its child-status reaches C's architect topic, not T's.
func (r *subArchitectLive) grandchildStep() {
	t := r.t
	c, g := r.child, r.grandchild
	r.board.place(g, "triage", c, false)
	r.note("C's architect files %s: the Dispatch stand-in shows it in triage under %s", g, c)
	if status, failure := r.release(r.childArchitect, g); status != http.StatusOK {
		t.Fatalf("release_children of %s by %s's architect = %d %s %s, want 200", g, c, status, failure.Code, failure.Error)
	}
	r.note("drove POST /legion/v1/waves/release {%s} with a grant of %s's architect: 200", g, c)
	if !r.board.wrote(g, "todo") {
		t.Fatalf("the release wrote no todo of %s to the Dispatch stand-in; writes %+v", g, r.board.writes())
	}
	r.echo(g)
	r.note("read back: the stand-in took the release's PATCH of %s to todo; drove that write's own event", g)
	r.startsWithItsArchitect(g)
	if recorded := r.recorded(g); recorded == nil || recorded.Parent == nil || *recorded.Parent != c || recorded.Tree != r.tree {
		t.Fatalf("the daemon records %s as %+v, want it under %s in the tree of %s", g, recorded, c, r.tree)
	}
	r.note("read back: the daemon records %s under %s in the tree of %s", g, c, r.tree)
	startsG := func(n record.Notice) bool {
		return n.Kind == "child-status" && strings.HasPrefix(n.Reason, g+" starts planning under "+r.tree)
	}
	told := r.waitTold(c, "the child-status notice that "+g+" started", startsG)
	r.note("read back: a child-status notice on %s's architect topic — %s", c, told.Reason)
	if root := slices.IndexFunc(r.told(r.tree), startsG); root >= 0 {
		t.Fatalf("%s's architect topic was also told that %s started; want it told to %s's architect alone", r.tree, g, c)
	}
	r.loggedPublish("child-status", g, r.token(c, claim.RoleArchitect))
	r.note("read back: %s's architect topic holds no child-status of %s", r.tree, g)
}

// ownership: C's architect's rerun_child of S, its sibling, and of T, the root, is each answered
// 403 ISSUE_NOT_OWNED, in a message naming the issue and C, with nothing written to Dispatch. C's
// architect then writes backlog on S and on T in Dispatch itself. The daemon sets each write back:
// it writes the issue's own status to Dispatch again, and the issue stays in its phase. C's
// architect's backlog on G, under its own issue, stands: the daemon takes it as it takes a
// person's move, and G leaves the workflow with its slot freed.
func (r *subArchitectLive) ownershipStep() {
	t := r.t
	for _, key := range []string{r.sibling, r.tree} {
		writes := len(r.board.writes())
		status, failure := r.rerun(r.childArchitect, key)
		if status != http.StatusForbidden || failure.Code != "ISSUE_NOT_OWNED" {
			t.Fatalf("rerun_child of %s by %s's architect = %d %s %s, want 403 ISSUE_NOT_OWNED", key, r.child, status, failure.Code, failure.Error)
		}
		if !strings.Contains(failure.Error, key) || !strings.Contains(failure.Error, r.child) {
			t.Fatalf("the refusal of %s's rerun by %s's architect says %q, want it to name %s and %s", key, r.child, failure.Error, key, r.child)
		}
		if wrote := r.board.writes()[writes:]; len(wrote) != 0 {
			t.Fatalf("the refused rerun of %s wrote %+v to Dispatch, want nothing", key, wrote)
		}
		r.note("drove POST /legion/v1/children/rerun of %s with a grant of %s's architect: %d %s — %s; the stand-in took no write",
			key, r.child, status, failure.Code, failure.Error)
	}
	for _, key := range []string{r.sibling, r.tree} {
		before := r.issue(key)
		writes := len(r.board.writes())
		r.sessionWrite(r.childArchitect, key, "backlog")
		r.note("drove %s's architect's own write of %s to backlog: an issue.updated envelope, actor %s", r.child, key, r.childArchitect.session)
		setBack := liveDispatchWrite{Issue: key, Status: before.Status}
		testwait.Eventually(t, fmt.Sprintf("the daemon's write of %s back to %s over the backlog of %s's architect", key, before.Status, r.child), func() bool {
			return slices.Contains(r.board.writes()[writes:], setBack)
		})
		if got := r.issue(key); got.Phase != before.Phase || got.Status != before.Status {
			t.Fatalf("%s after the backlog of %s's architect = phase %s, status %s; want it set back, still phase %s, status %s",
				key, r.child, got.Phase, got.Status, before.Phase, before.Status)
		}
		r.note("read back: the stand-in took the daemon's PATCH of %s back to %s, and %s is still %s", key, before.Status, key, before.Phase)
	}
	g := r.grandchild
	r.sessionWrite(r.childArchitect, g, "backlog")
	r.note("drove %s's architect's own write of %s to backlog: an issue.updated envelope, actor %s", r.child, g, r.childArchitect.session)
	got := r.waitFor(g+" out of the workflow in backlog", g, func(got api.Issue) bool {
		return got.Phase == phase.Done && got.Status == "backlog" && got.Slot == nil
	})
	r.note("read back: after that write's event %s is %s, status %s, with no slot", g, got.Phase, got.Status)
}

// todo: a person moves D, under T, to todo from the Dispatch dashboard. D starts planning with an
// architect of its own, which, once ready, is told D alone.
func (r *subArchitectLive) todoStep() {
	d := r.todoChild
	r.publishIssue(d, "todo", r.tree, nil, false)
	r.note("drove a person's move of %s to todo under %s: a notifications.dispatch.issue.%s.issue.updated envelope, actor operator", d, r.tree, d)
	r.startsWithItsArchitect(d)
	r.todoArchitect = r.register(d, claim.RoleArchitect, "ses_d_architect", 0)
	r.note("registered and readied %s's architect as session %s on its launch %d", d, r.todoArchitect.session, r.todoArchitect.launch)
	r.catchUpNames(d, r.waitCatchUp(d, r.todoArchitect.launch), d)
}

// blocker: E, a person's todo under T, is held by its blocked_by link to B, which Legion does not
// work and which stands in progress: E waits admitted with no architect, its why blocked on B.
// Once a person closes B, E starts planning with an architect of its own, and T's architect is
// told that it started because every issue it waited on, B, is done.
func (r *subArchitectLive) blockerStep() {
	t := r.t
	b, e := r.blocker, r.heldChild
	r.waitProcessedEvent(r.publishIssue(b, "in_progress", "", nil, false))
	r.note("drove %s in progress with no parent and no Legion label: an issue.updated envelope, which the daemon took", b)
	r.publishIssue(e, "todo", r.tree, []string{b}, false)
	r.note("drove a person's move of %s to todo under %s, blocked_by %s: an issue.updated envelope", e, r.tree, b)
	held := r.waitFor(e+" recorded and admitted, blocked on "+b, e, func(got api.Issue) bool {
		return got.Phase == phase.Admitted && got.Why != nil && got.Why.State == api.WhyBlocked && strings.Contains(got.Why.Reason, b)
	})
	architect := r.token(e, claim.RoleArchitect)
	deadline := time.Now().Add(liveSettle)
	for time.Now().Before(deadline) {
		if got := r.issue(e); got.Phase != phase.Admitted || got.Architect != nil || r.launched(architect) {
			t.Fatalf("while %s works, %s is %s with architect %+v (launched %v); want it admitted with no architect", b, e, got.Phase, got.Architect, r.launched(architect))
		}
		time.Sleep(100 * time.Millisecond)
	}
	r.note("read back over %s: %s stays %s with no architect entry and no launch of %s; why: %s — %s",
		liveSettle, e, phase.Admitted, architect, held.Why.State, held.Why.Reason)
	r.publishIssue(b, "done", "", nil, false)
	r.note("drove a person's close of %s: an issue.updated envelope, status done", b)
	r.startsWithItsArchitect(e)
	told := r.waitTold(r.tree, "the child-status notice that "+e+" started", func(n record.Notice) bool {
		return n.Kind == "child-status" && strings.HasPrefix(n.Reason, e+" starts planning under "+r.tree)
	})
	if want := fmt.Sprintf("%s starts planning under %s: every issue it waited on (%s) is done", e, r.tree, b); !strings.HasPrefix(told.Reason, want) {
		t.Fatalf("%s's architect was told that %s started as %q, want it to begin %q", r.tree, e, told.Reason, want)
	}
	r.note("read back: a child-status notice on %s's architect topic — %s", r.tree, told.Reason)
}

// failure: C's architect's process is reported gone and the runtime refuses every relaunch of it,
// which spends its launch budget and fails the claim. T's architect is told in a worker-died, and
// C's next notice, its planner's lost session, goes to T's architect too; nothing relaunches C's
// architect meanwhile. The planner's completion then moves C to implementing, a phase start, which
// retries C's architect claim with fresh budgets in the same session, and its relaunch's ready is
// told C and G in a catch-up on C's own topic.
func (r *subArchitectLive) failureStep() {
	t := r.t
	c := r.child
	planner := r.register(c, claim.RolePlanner, "ses_c_planner", 0)
	task := planner.awaitPrompt(1)
	r.note("registered and readied %s's planner as session %s; it took its task: %q", c, planner.session, firstLine(task.Message))

	architect := r.token(c, claim.RoleArchitect)
	booted := r.d.claim(architect)
	if (booted.State != string(supervise.StateReady) && booted.State != string(supervise.StateIdle)) || booted.Locator == nil {
		t.Fatalf("%s is %s with locator %v before the failure, want ready on a process", architect, booted.State, booted.Locator)
	}
	r.rt.refuseResumes(architect, errors.New("the pod did not start"))
	r.childArchitect.end()
	r.rt.Emit(runtime.Observation{Locator: *booted.Locator, Kind: runtime.Gone, Detail: "the driver ended the process"})
	r.note("drove the end of %s's process: its shim's connection closed and the runtime's report of it gone, with every relaunch of it refused", architect)
	r.waitClaim(architect, "failed", func(held api.OperatorClaim) bool { return held.State == string(supervise.StateFailed) })
	failed := r.d.claim(architect)
	if failed.Session != booted.Session || failed.SessionFile != booted.SessionFile || failed.Budgets.LaunchFailures != r.cfg.LaunchFailureLimit {
		t.Fatalf("%s failed as %+v, want its session %s kept with %d launch failures", architect, failed, booted.Session, r.cfg.LaunchFailureLimit)
	}
	r.rt.allowResumes(architect)
	r.note("read back: %s is failed after %d refused relaunches, %d launch failures of %d, session %s kept",
		architect, r.rt.refused(architect), failed.Budgets.LaunchFailures, r.cfg.LaunchFailureLimit, failed.Session)
	diedReason := c + "'s own architect failed"
	died := r.waitTold(r.tree, "the worker-died of "+c+"'s architect", func(n record.Notice) bool {
		return n.Kind == "worker-died" && n.Role == claim.RoleArchitect && strings.HasPrefix(n.Reason, diedReason)
	})
	if slices.ContainsFunc(r.told(c), func(n record.Notice) bool { return n.Kind == "worker-died" && n.Role == claim.RoleArchitect }) {
		t.Fatalf("%s's own topic was told its architect's worker-died, want it told to the architect above", c)
	}
	r.note("read back: a worker-died notice about the architect on %s's architect topic, none on %s's — %s", r.tree, c, died.Reason)

	mark := len(r.rt.Calls())
	plannerToken := r.token(c, claim.RolePlanner)
	held := r.d.claim(plannerToken)
	if held.Locator == nil {
		t.Fatalf("%s records no process", plannerToken)
	}
	r.rt.refuseResumes(plannerToken, fmt.Errorf("the session store holds no %s: %w", held.SessionFile, runtime.ErrSessionFileMissing))
	planner.end()
	r.rt.Emit(runtime.Observation{Locator: *held.Locator, Kind: runtime.Gone, Detail: "the driver ended the process"})
	r.note("drove the end of %s's process, as for the architect, its session file found in no store at the relaunch", plannerToken)
	fresh := r.launchSince(plannerToken, mark)
	r.rt.allowResumes(plannerToken)
	lostReason := fmt.Sprintf("The %s of %s lost the session it recorded", claim.RolePlanner, c)
	lost := r.waitTold(r.tree, "the session-lost notice of "+c+"'s planner", func(n record.Notice) bool {
		return n.Kind == "session-lost" && n.Role == claim.RolePlanner && strings.HasPrefix(n.Reason, lostReason)
	})
	if slices.ContainsFunc(r.told(c), func(n record.Notice) bool { return n.Kind == "session-lost" }) {
		t.Fatalf("%s's own failed architect was told its planner's session-lost, want it told to the architect above", c)
	}
	r.note("read back: the planner relaunched fresh (launch %d, empty resume file %q); its session-lost notice on %s's architect topic, none on %s's — %s",
		fresh.Generation, fresh.ResumeSessionFile, r.tree, c, firstLine(lost.Reason))
	if launches := launchesSince(r.rt.Runtime, architect, mark); len(launches) != 0 {
		t.Fatalf("%s's architect was launched %v before its child's next phase start, want nothing", c, launches)
	}
	if again := r.d.claim(architect); again.State != string(supervise.StateFailed) {
		t.Fatalf("%s is %s before %s's next phase start, want still failed", architect, again.State, c)
	}
	r.note("read back: no launch of %s since its failure, and it is still failed", architect)

	planner = r.register(c, claim.RolePlanner, "ses_c_planner_fresh", mark)
	task = planner.awaitPrompt(1)
	r.note("registered and readied the fresh planner as session %s; it took its task: %q", planner.session, firstLine(task.Message))
	handoff := r.board.handoff(c, "plan", planner.session)
	mark = len(r.rt.Calls())
	if status, failure := planner.complete("planned", handoff); status != http.StatusOK {
		t.Fatalf("the planner's completion of %s = %d %s %s, want 200", c, status, failure.Code, failure.Error)
	}
	r.note("drove POST /legion/v1/handoff/complete of %s's planner naming its plan record %s: 200", c, handoff)
	r.waitFor(c+" in implementing", c, func(got api.Issue) bool { return got.Phase == phase.Implementing })
	relaunch := r.launchSince(architect, mark)
	launches := launchesSince(r.rt.Runtime, architect, mark)
	if len(launches) != 1 || launches[0].Method != "Resume" || relaunch.ResumeSessionFile != booted.SessionFile {
		t.Fatalf("the move into implementing launched %s %+v, want one resume of its session file %s", architect, launches, booted.SessionFile)
	}
	r.waitClaim(architect, "relaunched on its session with fresh budgets", func(held api.OperatorClaim) bool {
		return held.State != string(supervise.StateFailed) && held.Session == booted.Session && held.Budgets == (api.BudgetsView{})
	})
	retried := r.d.claim(architect)
	r.note("read back: %s is in implementing; the fake runtime was asked to resume %s from %s (launch %d), the claim %s on session %s with budgets %+v",
		c, architect, relaunch.ResumeSessionFile, relaunch.Generation, retried.State, retried.Session, retried.Budgets)
	r.childArchitect = r.register(c, claim.RoleArchitect, booted.Session, mark)
	r.note("registered and readied the relaunched %s's architect as its session %s", c, booted.Session)
	r.catchUpNames(c, r.waitCatchUp(c, r.childArchitect.launch), c, r.grandchild)
}

// rerun: T's architect writes backlog on D in Dispatch, and D's architect is suspended by the
// daemon with its session kept; T's architect then reruns D, whose next run resumes that
// architect's session rather than spawning a fresh one, and its ready is told D at its new
// generation.
func (r *subArchitectLive) rerunStep() {
	t := r.t
	d := r.todoChild
	architect := r.token(d, claim.RoleArchitect)
	before := r.d.claim(architect)
	started := r.issue(d)
	mark := len(r.rt.Calls())
	r.sessionWrite(r.root, d, "backlog")
	r.note("drove %s's architect's own write of %s to backlog: an issue.updated envelope, actor %s", r.tree, d, r.root.session)
	r.waitFor(d+" out of the workflow in backlog", d, func(got api.Issue) bool {
		return got.Phase == phase.Done && got.Status == "backlog" && got.Slot == nil
	})
	r.waitClaim(architect, "suspended by the daemon", func(held api.OperatorClaim) bool {
		return held.State == string(supervise.StateSuspended) && held.SuspendedBy == string(supervise.SuspendedByDaemon)
	})
	// The suspension stopped the agent's process, and its shim's connection went with it.
	r.todoArchitect.end()
	suspended := r.d.claim(architect)
	if suspended.Session != before.Session || suspended.SessionFile != before.SessionFile {
		t.Fatalf("%s after the backlog = %+v, want its session %s kept", architect, suspended, before.Session)
	}
	suspends := 0
	for _, call := range r.rt.Calls()[mark:] {
		if call.Method == "Suspend" && call.Locator.Claim == architect {
			suspends++
		}
	}
	if suspends != 1 {
		t.Fatalf("the backlog suspended %s's process %d times, want once", architect, suspends)
	}
	r.note("read back: %s is done; %s is %s by %s with its session %s kept, its process suspended once", d, architect, suspended.State, suspended.SuspendedBy, suspended.Session)

	mark = len(r.rt.Calls())
	if status, failure := r.rerun(r.root, d); status != http.StatusOK {
		t.Fatalf("rerun_child of %s by %s's architect = %d %s %s, want 200", d, r.tree, status, failure.Code, failure.Error)
	}
	if !r.board.wrote(d, "todo") {
		t.Fatalf("the rerun wrote no todo of %s to the Dispatch stand-in; writes %+v", d, r.board.writes())
	}
	r.echo(d)
	r.note("drove POST /legion/v1/children/rerun of %s with a grant of %s's architect: 200, and that write's own event", d, r.tree)
	again := r.waitFor(d+" planning again", d, func(got api.Issue) bool {
		return planning(got) && got.Generation > started.Generation && got.Architect != nil
	})
	resume := r.launchSince(architect, mark)
	launches := launchesSince(r.rt.Runtime, architect, mark)
	if len(launches) != 1 || launches[0].Method != "Resume" || resume.ResumeSessionFile != before.SessionFile {
		t.Fatalf("the rerun launched %s %+v, want one resume of its session file %s", architect, launches, before.SessionFile)
	}
	if resumed := r.d.claim(architect); resumed.Session != before.Session {
		t.Fatalf("%s after the rerun is on session %s, want %s", architect, resumed.Session, before.Session)
	}
	r.note("read back: %s is in %s at generation %d (was %d); the fake runtime was asked to resume %s from %s, and nothing was spawned",
		d, again.Phase, again.Generation, started.Generation, architect, resume.ResumeSessionFile)
	r.todoArchitect = r.register(d, claim.RoleArchitect, before.Session, mark)
	r.note("registered and readied the resumed %s's architect as its session %s", d, before.Session)
	r.catchUpNames(d, r.waitCatchUp(d, r.todoArchitect.launch), d)
}

// ---- what a step reads ---------------------------------------------------------------------------

// token is the claim token of role on issue, as the daemon spells it.
func (r *subArchitectLive) token(issue string, role claim.Role) claim.Token {
	r.t.Helper()
	token, err := claim.NewToken(r.project, issue, role)
	if err != nil {
		r.t.Fatal(err)
	}
	return token
}

// launched says whether the fake runtime was asked to launch token's claim.
func (r *subArchitectLive) launched(token claim.Token) bool {
	return len(launchesSince(r.rt.Runtime, token, 0)) > 0
}

// launchSince waits for a launch of token among the fake runtime's calls after its first since,
// and answers the spec of the latest: the boot token its pane carries and its generation.
func (r *subArchitectLive) launchSince(token claim.Token, since int) runtime.SpawnSpec {
	r.t.Helper()
	var spec runtime.SpawnSpec
	testwait.Eventually(r.t, "a launch of "+string(token), func() bool {
		launches := launchesSince(r.rt.Runtime, token, since)
		if len(launches) == 0 {
			return false
		}
		spec = launches[len(launches)-1].Spec
		return true
	})
	return spec
}

// waitClaim waits for the operator's listing of token to read as want says.
func (r *subArchitectLive) waitClaim(token claim.Token, what string, want func(api.OperatorClaim) bool) {
	r.t.Helper()
	testwait.Eventually(r.t, string(token)+" "+what, func() bool { return want(r.d.claim(token)) })
}

// recorded is the daemon's own record of key in its PostgreSQL, nil when it records none.
func (r *subArchitectLive) recorded(key string) *record.Issue {
	r.t.Helper()
	var issue *record.Issue
	if err := store.Transact(context.Background(), r.st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		issue, err = r.records.Issue(context.Background(), tx, key)
		return err
	}); err != nil {
		r.t.Fatalf("read the record of %s: %v", key, err)
	}
	return issue
}

// waitFor polls GET /legion/v1/state until want is true of key; when the wait times out, it logs
// what the state last showed of key.
func (r *subArchitectLive) waitFor(what, key string, want func(api.Issue) bool) api.Issue {
	r.t.Helper()
	var last api.State
	held := false
	defer func() {
		if !held {
			issue, ok := last.Issues[key]
			r.t.Logf("%s as last seen (in the state %v): %+v; admission.waiting %v", key, ok, issue, last.Admission.Waiting)
		}
	}()
	testwait.Eventually(r.t, what, func() bool {
		last = r.d.state()
		issue, ok := last.Issues[key]
		return ok && want(issue)
	})
	held = true
	return last.Issues[key]
}

// startsWithItsArchitect requires key, as GET /legion/v1/state shows it, to plan in T on a slot of
// its own with its planner's claim and an architect entry, the fake runtime asked to launch that
// architect: its first phase start started its own architect beside its planner.
func (r *subArchitectLive) startsWithItsArchitect(key string) api.Issue {
	r.t.Helper()
	got := r.waitFor(key+" planning on a slot of its own with its own architect's claim", key, func(got api.Issue) bool {
		return planning(got) && got.Architect != nil && got.Slot != nil
	})
	if got.Tree != r.tree {
		r.t.Fatalf("the state shows %s in the tree of %s, want %s", key, got.Tree, r.tree)
	}
	architect := r.token(key, claim.RoleArchitect)
	testwait.Eventually(r.t, "the launch of "+string(architect), func() bool { return r.launched(architect) })
	r.note("read back: GET /legion/v1/state shows %s in %s at generation %d in the tree of %s on admission slot %d, its planner's claim %s and an architect entry, claim %s; the fake runtime was asked to launch %s",
		key, got.Phase, got.Generation, got.Tree, got.Slot.Index, got.Workers[claim.RolePlanner].Claim.State, got.Architect.State, architect)
	return got
}

// told is every notice the daemon published to the role topic of key's architect, oldest first,
// as it wrote each one.
func (r *subArchitectLive) told(key string) []record.Notice {
	r.t.Helper()
	var notices []record.Notice
	for _, published := range r.notices.acceptedOn(notify.RoleTopicPrefix + string(r.token(key, claim.RoleArchitect))) {
		var notice record.Notice
		if err := json.Unmarshal([]byte(published.Payload), &notice); err != nil {
			r.t.Fatalf("decode the notice payload %q: %v", published.Payload, err)
		}
		notices = append(notices, notice)
	}
	return notices
}

// waitTold waits for the daemon to tell key's architect a notice want holds of, and answers it.
func (r *subArchitectLive) waitTold(key, what string, want func(record.Notice) bool) record.Notice {
	r.t.Helper()
	var found record.Notice
	testwait.Eventually(r.t, what+" on "+key+"'s architect topic", func() bool {
		at := slices.IndexFunc(r.told(key), want)
		if at < 0 {
			return false
		}
		found = r.told(key)[at]
		return true
	})
	return found
}

// waitCatchUp waits for the catch-up of launch on key's architect topic.
func (r *subArchitectLive) waitCatchUp(key string, launch uint64) record.Notice {
	r.t.Helper()
	return r.waitTold(key, fmt.Sprintf("the catch-up of launch %d", launch), func(n record.Notice) bool {
		return n.Kind == "catch-up" && n.CatchUp != nil && n.CatchUp.Launch == launch
	})
}

// catchUpNames requires told, a catch-up of key's architect, to list exactly want at key's
// generation as the state shows it, with no gate, and prints it.
func (r *subArchitectLive) catchUpNames(key string, told record.Notice, want ...string) record.Notice {
	r.t.Helper()
	keys := make([]string, 0, len(told.CatchUp.Issues))
	for _, listed := range told.CatchUp.Issues {
		keys = append(keys, listed.Key)
	}
	generation := r.issue(key).Generation
	if !slices.Equal(keys, want) || told.CatchUp.Generation != generation || len(told.CatchUp.Gates) != 0 {
		r.t.Fatalf("%s's architect was told a catch-up of %v at generation %d with gates %+v, want %v at %s's generation %d and no gate",
			key, keys, told.CatchUp.Generation, told.CatchUp.Gates, want, key, generation)
	}
	reason, _, _ := strings.Cut(told.Reason, ";")
	r.note("read back: a catch-up on %s's architect topic, launch %d, generation %d, no gate, issues %v — %s",
		key, told.CatchUp.Launch, told.CatchUp.Generation, keys, reason)
	return told
}

// catchUpOnItsOwnTopicAlone requires every catch-up the listener took that lists key first to be
// on key's architect topic: a child's catch-up goes to the architect of its own issue and to no
// other. The release step reads it while C's architect holds its role, when the owner walk from C
// ends at that same architect, so it cannot tell that rule (D6) from the walk:
// TestASubArchitectsCatchUpGoesToItAlone holds the rule for a catch-up whose architect has stopped.
func (r *subArchitectLive) catchUpOnItsOwnTopicAlone(key string) {
	r.t.Helper()
	own := notify.RoleTopicPrefix + string(r.token(key, claim.RoleArchitect))
	r.notices.mu.Lock()
	accepted := slices.Clone(r.notices.accepted)
	r.notices.mu.Unlock()
	for _, published := range accepted {
		var notice record.Notice
		if err := json.Unmarshal([]byte(published.Payload), &notice); err != nil || notice.Kind != "catch-up" || notice.CatchUp == nil ||
			len(notice.CatchUp.Issues) == 0 || notice.CatchUp.Issues[0].Key != key {
			continue
		}
		if published.Topic != own {
			r.t.Fatalf("a catch-up of %s was published to %s, want it on %s alone", key, published.Topic, own)
		}
	}
	r.note("read back: no topic but %s holds a catch-up of %s", own, key)
}

// loggedPublish requires the daemon's own `outbox notice published` line for the notice of kind
// on issue that reached architect, prints its fields, and answers it.
func (r *subArchitectLive) loggedPublish(kind, issue string, architect claim.Token) loggedRecord {
	r.t.Helper()
	var line *loggedRecord
	testwait.Eventually(r.t, fmt.Sprintf("the daemon's line for the %s of %s published to %s", kind, issue, architect), func() bool {
		matched := r.logged.matching(func(kept loggedRecord) bool {
			return kept.Message == "outbox notice published" && fmt.Sprint(kept.Attrs["kind"]) == kind &&
				fmt.Sprint(kept.Attrs["issue"]) == issue && fmt.Sprint(kept.Attrs["architect"]) == string(architect)
		})
		if len(matched) == 0 {
			return false
		}
		line = &matched[0]
		return true
	})
	fields := []string{}
	for _, name := range []string{"row", "kind", "issue", "architect", "generation", "launch", "issues"} {
		if value, ok := line.Attrs[name]; ok {
			fields = append(fields, fmt.Sprintf("%s=%v", name, value))
		}
	}
	r.note("read back: the daemon logged %q %s", line.Message, strings.Join(fields, " "))
	return *line
}

// waitProcessedEvent waits for the daemon's processed_events row of the envelope eventID, which
// the intake writes in the transaction that applies the event, so the next envelope a step
// publishes follows that event's fact. No route shows the row.
func (r *subArchitectLive) waitProcessedEvent(eventID string) {
	r.t.Helper()
	testwait.Eventually(r.t, "the daemon's processed_events row for "+eventID, func() bool {
		var taken bool
		if err := r.st.Pool().QueryRow(context.Background(),
			"select exists(select 1 from processed_events where event_id = $1)", eventID).Scan(&taken); err != nil {
			r.t.Fatalf("read the processed event %s: %v", eventID, err)
		}
		return taken
	})
}

// ---- what a step drives --------------------------------------------------------------------------

// release is by's release_children of keys, as POST /legion/v1/waves/release with a grant by's
// session buys now: the status and, for any answer but 200, the refusal.
func (r *subArchitectLive) release(by *subArchitectSession, keys ...string) (int, api.Failure) {
	r.t.Helper()
	return r.answer(r.d.request(http.MethodPost, "/legion/v1/waves/release", api.WaveReleaseRequest{GrantID: by.grant(), Issues: keys}, false))
}

// rerun is by's rerun_child of key, as POST /legion/v1/children/rerun: the status and, for any
// answer but 200, the refusal.
func (r *subArchitectLive) rerun(by *subArchitectSession, key string) (int, api.Failure) {
	r.t.Helper()
	return r.answer(r.d.request(http.MethodPost, "/legion/v1/children/rerun", api.ChildRequest{GrantID: by.grant(), Issue: key}, false))
}

// answer is a route's status and, for any answer but 200, the refusal its body names.
func (r *subArchitectLive) answer(status int, body []byte) (int, api.Failure) {
	r.t.Helper()
	var failure api.Failure
	if status != http.StatusOK {
		if err := json.Unmarshal(body, &failure); err != nil {
			r.t.Fatalf("decode the answer %d %s: %v", status, body, err)
		}
	}
	return status, failure
}

// echo publishes the event of the daemon's own status write on key as Dispatch delivers it: the
// issue as the board holds it, the write applied, with its links and the daemon's own actor, on
// the next sequence. The stand-in answers a status write with no sequence, so a route leaves the
// record to that event (api's recordOwnStatusWrite), which the driver delivers as Dispatch would.
func (r *subArchitectLive) echo(key string) {
	r.t.Helper()
	r.board.mu.Lock()
	issue := r.board.issues[key]
	r.board.mu.Unlock()
	r.publishIssue(key, issue.status, issue.parent, r.links.of(key), true)
}

// sessionWrite publishes the event of by's own write of status on key in Dispatch, as Dispatch
// delivers it: the issue as the board holds it, with status, its links and by's session as its
// actor, on the next sequence, and answers its event id. An architect writes a status in Dispatch
// itself with `dispatch issue-update`, and the daemon reads that write only from this event.
func (r *subArchitectLive) sessionWrite(by *subArchitectSession, key, status string) string {
	r.t.Helper()
	r.board.mu.Lock()
	issue := r.board.issues[key]
	r.board.mu.Unlock()
	return r.publishIssueBy(map[string]string{"kind": "session", "id": by.session}, key, status, issue.parent, r.links.of(key))
}

// publishIssue publishes Dispatch's issue.updated event of key at status under parent ("" for
// none), blocked_by blockers, as a person's move from the Dispatch dashboard makes one — or, when
// daemon, as the echo of the daemon's own write — on the next sequence, and answers its event id
// (publishIssueBy).
func (r *subArchitectLive) publishIssue(key, status, parent string, blockers []string, daemon bool) string {
	r.t.Helper()
	actor := map[string]string{"kind": "user", "id": "operator"}
	if daemon {
		actor = map[string]string{"kind": "session", "id": dispatch.DaemonSession(key)}
	}
	return r.publishIssueBy(actor, key, status, parent, blockers)
}

// publishIssueBy is publishIssue with the event's actor given: a session's own write carries
// {"kind": "session", "id": <its session>}. No issue it publishes carries the Legion label: a child
// needs none, and the blocker is one Legion does not work. The event stands for a write already on
// Dispatch's board, so the board shows it first, and Dispatch's reads of key answer its links
// (linkedDispatch).
func (r *subArchitectLive) publishIssueBy(actor map[string]string, key, status, parent string, blockers []string) string {
	r.t.Helper()
	r.board.place(key, status, parent, false)
	r.links.link(key, blockers)
	r.seq++
	var parentKey any
	if parent != "" {
		parentKey = parent
	}
	issue := map[string]any{
		"key": key, "status": status, "title": key, "parent": parentKey, "rank": "M",
		"labels": liveDispatchLabels(false), "last_seq": r.seq, "project": r.cfg.Project, "resolved_repo": liveRepo,
	}
	if len(blockers) > 0 {
		issue["blocked_by"] = blockers
	}
	payload, err := json.Marshal(map[string]any{
		"id": r.seq, "issue_key": key, "project": r.cfg.Project, "seq": r.seq, "type": "issue.updated",
		"actor": actor, "notify": true, "created_at": time.Now().UTC().Format(time.RFC3339Nano), "payload": issue,
	})
	if err != nil {
		r.t.Fatalf("encode the Dispatch event: %v", err)
	}
	return r.publish("notifications.dispatch.issue."+key+".issue.updated", "dispatch", fmt.Sprintf("%s-%s-%d", key, status, r.seq), string(payload))
}

// register brings the launched claim of role on issue to ready, as its pane's shim and its agent
// do: it waits for a launch of it after the fake runtime's first since calls, dials the worker
// stream with that launch's boot token, registers session with its session file and reports it
// ready. The connection is then answered as Oh My Pi answers it (serve). A relaunch registers
// the session the claim recorded, as an agent resuming it does.
func (r *subArchitectLive) register(issue string, role claim.Role, session string, since int) *subArchitectSession {
	t := r.t
	t.Helper()
	token := r.token(issue, role)
	launch := r.launchSince(token, since)
	r.built.mu.Lock()
	address := r.built.address
	r.built.mu.Unlock()
	c := &subArchitectSession{r: r, token: token, issue: issue, session: session, launch: launch.Generation, sh: dialShim(t, address, launch.BootToken)}
	r.waitClaim(token, "with its shim connected", func(held api.OperatorClaim) bool { return held.State == string(supervise.StateShimConnected) })
	status, body := r.d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: launch.BootToken, SessionID: session, OmpSessionFile: "/sessions/" + session + ".jsonl",
		AgentID: session, PluginContract: api.DaemonAPIVersion(),
	}, false)
	if status != http.StatusOK {
		t.Fatalf("register %s = %d %s, want 200", token, status, body)
	}
	var registered claim.RegisterResponse
	if err := json.Unmarshal(body, &registered); err != nil {
		t.Fatalf("decode the registration of %s: %v", token, err)
	}
	c.secret = registered.Secret
	c.sh.serve(c.serve)
	if status, body := r.d.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: token, SessionID: session, Secret: registered.Secret, Generation: launch.Generation,
	}, false); status != http.StatusNoContent {
		t.Fatalf("ready %s = %d %s, want 204", token, status, body)
	}
	return c
}

// subArchitectSession is one claim's agent as the driver plays it: the session its registration
// named on the launch it registered from, the capability secret that registration returned, which
// buys its grants, and the worker stream connection its shim holds, with every prompt the daemon
// sent over it.
type subArchitectSession struct {
	r       *subArchitectLive
	token   claim.Token
	issue   string
	session string
	launch  uint64
	secret  string
	sh      *shim
	// writing serialises the frames serve and a step write; mu guards what serve records.
	writing sync.Mutex
	mu      sync.Mutex
	turn    bool
	prompts []shimwire.Prompt
}

// serve reads every frame the daemon sends the shim until the connection closes or the test ends
// it (shim.serve): it answers the protocol negotiation and the state probe (idle, or streaming
// while a turn runs), and takes each prompt — answering it and starting the turn, as Oh My Pi
// does — for a step to read. A write that fails ends it too: the connection closed under the
// process the runtime reported gone.
func (c *subArchitectSession) serve() {
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
			if !c.send(shimwire.Response{ID: f.ID, Command: shimwire.TypeNegotiateProtocol, Success: true}) {
				return
			}
		case shimwire.GetState:
			c.mu.Lock()
			streaming := c.turn
			c.mu.Unlock()
			data, _ := json.Marshal(shimwire.StateData{IsStreaming: &streaming})
			if !c.send(shimwire.Response{ID: f.ID, Command: shimwire.TypeGetState, Success: true, Data: data}) {
				return
			}
		case shimwire.Prompt:
			if !c.send(shimwire.Response{ID: f.ID, Command: shimwire.TypePrompt, Success: true}) {
				return
			}
			c.mu.Lock()
			c.turn = true
			c.prompts = append(c.prompts, f)
			c.mu.Unlock()
			if !c.send(shimwire.AgentStart{DeliveryID: f.DeliveryID}) {
				return
			}
		}
	}
}

// end is the agent's process ending: its shim's connection closes with it, which frees the claim's
// worker stream for the hello of the process that replaces it.
func (c *subArchitectSession) end() {
	_ = c.sh.conn.Close()
}

// send writes one frame to the daemon, and says whether the connection took it.
func (c *subArchitectSession) send(frame shimwire.Frame) bool {
	c.writing.Lock()
	defer c.writing.Unlock()
	return c.sh.writer.WriteFrame(frame) == nil
}

// awaitPrompt waits for the daemon's n-th prompt over this connection, and answers it.
func (c *subArchitectSession) awaitPrompt(n int) shimwire.Prompt {
	c.r.t.Helper()
	var prompt shimwire.Prompt
	testwait.Eventually(c.r.t, fmt.Sprintf("prompt %d of %s", n, c.token), func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		if len(c.prompts) < n {
			return false
		}
		prompt = c.prompts[n-1]
		return true
	})
	return prompt
}

// grant is a capability grant minted for this session now, as its `legion` tool mints one per
// operation: in T's tree, for the session's own issue.
func (c *subArchitectSession) grant() string {
	t := c.r.t
	t.Helper()
	status, body := c.r.d.request(http.MethodPost, "/legion/v1/grants",
		api.GrantRequest{SessionID: c.session, Secret: c.secret, Tree: c.r.tree, Issue: c.issue}, false)
	if status != http.StatusOK {
		t.Fatalf("the grant of %s = %d %s, want 200", c.token, status, body)
	}
	var granted api.GrantResponse
	if err := json.Unmarshal(body, &granted); err != nil {
		t.Fatalf("decode the grant of %s: %v", c.token, err)
	}
	return granted.GrantID
}

// complete reports this role's completion of the phase it works, from the turn it is in, naming
// version 1 of handoffID, a handoff record this claim's session wrote, as the `legion` tool's
// handoff_complete does: the status and, for any answer but 200, the refusal.
func (c *subArchitectSession) complete(summary, handoffID string) (int, api.Failure) {
	c.r.t.Helper()
	return c.r.answer(c.r.d.request(http.MethodPost, "/legion/v1/handoff/complete",
		api.HandoffCompleteRequest{GrantID: c.grant(), Summary: summary, HandoffID: handoffID, HandoffVersion: 1}, false))
}

// linkedDispatch is the run's Dispatch as the daemon reads it: the package's board (liveDispatch),
// with the blocked_by links of each issue an event of the driver's gave it added to every read of
// that issue, as Dispatch's own issue detail carries them (dispatch.Client.GetIssue). The board
// keeps no links, and the daemon prepares a todo event of an unrecorded issue from Dispatch's
// view of it rather than from the event (repositories.go's prepareIssue), so without them the
// link the event carried would never reach the daemon's mirror. Every other request is the
// board's own.
type linkedDispatch struct {
	board *liveDispatch
	mu    sync.Mutex
	links map[string][]string
}

// link records the issues key is blocked by now; none takes its links away.
func (d *linkedDispatch) link(key string, blockers []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(blockers) == 0 {
		delete(d.links, key)
		return
	}
	d.links[key] = slices.Clone(blockers)
}

// of is the issues key is blocked by now.
func (d *linkedDispatch) of(key string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.links[key])
}

func (d *linkedDispatch) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	key := issueOfDispatchPath(req.URL.Path)
	blockers := d.of(key)
	if req.Method != http.MethodGet || key == "" || req.URL.Path != "/api/v1/issues/"+key || len(blockers) == 0 {
		d.board.ServeHTTP(w, req)
		return
	}
	read := httptest.NewRecorder()
	d.board.ServeHTTP(read, req)
	var issue map[string]any
	if read.Code != http.StatusOK || json.Unmarshal(read.Body.Bytes(), &issue) != nil {
		w.WriteHeader(read.Code)
		_, _ = w.Write(read.Body.Bytes())
		return
	}
	issue["blocked_by"] = blockers
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(issue); err != nil {
		d.board.t.Errorf("write the Dispatch issue %s: %v", key, err)
	}
}

// subArchitectRuntime is the fake runtime with each resume of a claim the failure step names
// refused with the error it names — a pod the cluster would not open, a session file no store
// holds — until the step allows that claim's resumes again. A refused resume never reaches the
// fake, so the fake records only the launches that started a process.
type subArchitectRuntime struct {
	*fake.Runtime
	mu       sync.Mutex
	refusals map[claim.Token]*resumeRefusal
}

// resumeRefusal is the error a claim's resumes are refused with while it is armed, and how many
// resumes it has refused.
type resumeRefusal struct {
	err     error
	armed   bool
	refused int
}

var _ runtime.Runtime = (*subArchitectRuntime)(nil)

func (r *subArchitectRuntime) Resume(ctx context.Context, prev *runtime.Locator, spec runtime.SpawnSpec) (runtime.Locator, error) {
	r.mu.Lock()
	refusal := r.refusals[spec.Claim]
	if refusal != nil && refusal.armed {
		refusal.refused++
		r.mu.Unlock()
		return runtime.Locator{}, refusal.err
	}
	r.mu.Unlock()
	return r.Runtime.Resume(ctx, prev, spec)
}

// refuseResumes refuses every resume of token with err until allowResumes.
func (r *subArchitectRuntime) refuseResumes(token claim.Token, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refusals[token] = &resumeRefusal{err: err, armed: true}
}

// allowResumes lets every later resume of token through to the fake.
func (r *subArchitectRuntime) allowResumes(token claim.Token) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if refusal := r.refusals[token]; refusal != nil {
		refusal.armed = false
	}
}

// refused is how many resumes of token were refused.
func (r *subArchitectRuntime) refused(token claim.Token) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if refusal := r.refusals[token]; refusal != nil {
		return refusal.refused
	}
	return 0
}
