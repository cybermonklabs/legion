//go:build e2e

// The live proof that a completion the `legion` tool sends after the issue moved on is answered
// for the phase the role left (dispatch://INFRA-3146): a finished implementer that completes
// again while the issue is in testing, with the tester's commit as the commit the shared
// workspace stands on, is answered by the daemon and not by the tool's push checks, which a phase
// the role left runs none of: `409 HANDOFF_NOT_CURRENT_PHASE` in one request, naming its own
// implement record; a finished tester that completes again, naming its own test record, is
// refused the same in one request; and a reviewer whose own approval moved the issue to
// accepting before it completed has its completion taken, naming its own review record.
// scripts/e2e/late-completion.sh builds this driver with `-tags e2e` and owns the nats-server and
// the PostgreSQL it runs against.
//
// The unit tests already hold each half: pi-shared's handoff-record.test.ts the record the tool's
// findHandoffReport reads once the issue has moved on, and internal/api's handoff_test.go and
// internal/workflow's handoff_test.go what the route and the engine answer such a completion.
// None of them runs the tool a pane loads against the daemon a pane talks to, so none says that
// the call a real agent makes is answered so. This driver makes that call. Each pane is a real
// `omp --mode rpc` under the real `legion worker-shim`, started by localruntime_live_test.go's
// runtime with this checkout's `@sjawhar/pi-envoy` and `@sjawhar/pi-legion` in an Oh My Pi
// profile of the run's own, and the model stand-in that pane talks to (completionStandin)
// answers the turn a step arms with a `tool_use` block calling `legion` with
// `{op: "handoff_complete", ...}`, in the events the plugins' own pane harness scripts for one
// (packages/pi-shared/test/omp-harness.ts, messageStream). So the request the daemon answers is
// the one the shipped plugin builds: its record lookup, its grant and its post. The tool's answer
// comes back to the stand-in as the next request's tool_result, which is what the step records,
// and the stand-in answers that with a WAITING line, so the phase-stall reminder adds no turn of
// its own (packages/pi-shared/src/phase-stall.ts).
//
// Dispatch is the real server this checkout builds, on a database of its own, as
// handoff_records_live_test.go runs it (startHandoffDispatch), and the daemon and every pane are
// pointed at it: the daemon through dispatch_url, each pane through DISPATCH_URL and
// DISPATCH_TOKEN_FILE, which the local runtime tells a pane as the tmux runtime does. Each
// handoff record is written to Dispatch's own route as the pane's own Oh My Pi session, the
// session its claim registered, with a body naming the pull request's head under `head`; the
// tool reads it back through Dispatch's `/handoffs/<word>/latest`, and the daemon reads the
// record a completion names through `/handoffs/<id>` and holds it to that session.
//
// The daemon is the whole daemon (run, through startDaemonWithin). What the proof reads is the
// tool's answer, the daemon's own line for each completion it answers (api's handoffComplete,
// `api: handoff completion answered`, which names the record and the commit the call carried and
// the status and code it was answered with), and GET /legion/v1/state. GitHub is the harness's
// REST stand-in for the one pull request (liveGitHub, in live_issue_proof_test.go), which this
// driver starts itself (serveGitHub): the pull request at its head, mergeable and clean, with no
// reviews, and listed open on the issue's branch, which the implementer's completion reads. The
// review that moves reviewing on is published as the GitHub envelope Envoy relays, written as the
// review App submits one: approved, by the App's bot login, with the Legion footer naming the
// reviewer's own session and the head it read. The two Apps' tokens are workflowTokenRecorder's
// placeholders, and Envoy's listener is the package's noticeListener, with each pane registering
// at a listener of its own (newPaneListener).
//
// The seed is one issue in implementing, on an open pull request at the implementer's pushed
// head, with its implementer's claim mid-launch holding the task its phase's start left pending.
// The implementer's completion moves the issue to testing and the tester's pass moves it to
// reviewing, each a start the daemon makes itself, so the tester and the reviewer it launches are
// claims the daemon started on the issue, which the engine's leftItsPhase requires of a late
// completion. The panes' working directory is the issue's one jj workspace, as a pod's is, which
// every role of the issue shares: its `@-` is first the implementer's commit, on `legion/<issue>`
// of an origin of the run's own as a pushed round leaves it, which the implementer's completion in
// implementing is held to; then the tester's commit on top of it, pushed nowhere. This tree's
// tool reads nothing of that workspace for a phase the role left; the negative control's, at
// 9f1d4789, holds every implementer completion to `@-`, so it refuses the implementer's repeat
// naming the push of the tester's commit and sends nothing.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/fixturegit"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/stream"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// The panes, by the provider name each one's stand-in is given in the run's models.yml. The spare
// is the model of any claim this proof drives no pane for, so a launch the proof did not foresee
// talks to a stand-in that only ever answers text, never to a driven pane's.
const (
	lateImplementerProvider = "implementerpane"
	lateTesterProvider      = "testerpane"
	lateReviewerProvider    = "reviewerpane"
	lateSpareProvider       = "sparepane"
)

// lateCompletionAnswered is the completion route's one line per answer (api's handoffComplete).
const lateCompletionAnswered = "api: handoff completion answered"

// lateWaitingReply is the text a stand-in answers every turn it makes no call in, the tool's
// result among them: a WAITING line, which the phase-stall reminder takes as a worker that said
// what it waits on and answers with its quiet wait rather than another turn
// (packages/pi-shared/src/phase-stall.ts, WAITING_REPLY).
const lateWaitingReply = "WAITING on the daemon's next task: the legion tool has answered."

// The words of the operator's task that starts a finished role's second turn (lateRepeatTask),
// which that turn's call is armed on: the implementer's and the tester's.
const (
	lateImplementerRepeat = "report the implementing completion again"
	lateTesterRepeat      = "report the testing completion again"
)

// lateRepeatTask is the operator's task that starts a finished role's second turn, carrying
// marker.
func lateRepeatTask(marker string) string {
	return "LATE-COMPLETION: " + marker + " with the legion tool."
}

// lateCompletionLive is one run of the proof: the booted daemon over the real Dispatch, the one
// issue, the three panes' stand-ins, and what steps 2, 4 and 5 recorded for step 6 to judge.
type lateCompletionLive struct {
	liveAgentProof
	dispatch *handoffDispatch
	tokens   *workflowTokenRecorder
	// key is the proof's one issue, as Dispatch keyed it; title its title; number its pull
	// request's; and head that pull request's head, the implementer's pushed commit, which every
	// record names.
	key, title, head string
	number           int
	// github is GitHub's stand-in (serveGitHub).
	github *liveGitHub
	// panes is each driven claim's model stand-in, and spare the one every other claim talks to.
	panes map[claim.Token]*completionStandin
	spare *paneStandin
	// home and profile are the Oh My Pi home and profile the panes run under; overlays is each
	// driven claim's PI_CONFIG_FILES, and spareOverlays every other claim's.
	home, profile string
	overlays      map[claim.Token][]string
	spareOverlays []string
	// workspace is the panes' shared working directory, a jj repository (initWorkspace), and
	// testerCommit the tester's commit step 2 makes its `@-`.
	workspace, testerCommit string
	// run is the runner of every git and jj command the driver makes (initWorkspace).
	run fixturegit.Runner
	// implementerCall, testerCall and reviewerCall are the calls armed before the boot, on the
	// task each pane's first turn is started with.
	implementerCall, testerCall, reviewerCall *paneCall
	// implementRecord, testRecord and reviewRecord are the records steps 1, 3 and 5 wrote as each
	// pane's own session.
	implementRecord, testRecord, reviewRecord handoffRecord
	// implementerRepeat, testerRepeat and late are what the finished implementer's repeat (step
	// 2), the finished tester's (step 4) and the reviewer's late completion (step 5) were
	// answered, which step 6 judges.
	implementerRepeat, testerRepeat, late lateAnswer
}

// lateAnswer is one completion as a step records it: what the tool answered the agent, and the
// daemon's own line for each request that call made, one when the call reached the daemon once.
type lateAnswer struct {
	result toolResult
	lines  []loggedRecord
}

// TestLateCompletionsThroughTheRealLegionToolLive boots the daemon over the real Dispatch and
// three real panes, has the implementer complete implementing, has the finished implementer call
// handoff_complete again over the tester's commit, has the tester complete testing, has the
// finished tester call handoff_complete again, and has the reviewer complete after its own
// approval moved the issue to accepting, each call made by the `legion` tool the panes load; then
// judges the three late answers.
func TestLateCompletionsThroughTheRealLegionToolLive(t *testing.T) {
	fixtureenv.Live(t)
	r := newLateCompletionLive(t)
	r.step("step1-the-implementer-pane-completes-implementing-through-the-real-tool-at-its-pushed-head", r.implementerCompletes)
	r.step("step2-the-finished-implementer-calls-handoff-complete-again-over-the-testers-commit-in-the-shared-workspace", r.finishedImplementerRepeats)
	r.step("step3-the-tester-pane-completes-testing-through-the-real-tool-naming-its-own-test-record", r.testerCompletes)
	r.step("step4-the-finished-tester-calls-handoff-complete-again-and-one-request-reaches-the-daemon", r.finishedTesterRepeats)
	r.step("step5-the-reviewer-pane-completes-after-its-own-approval-moved-the-issue-to-accepting", r.reviewerCompletesLate)
	r.step("step6-each-repeat-is-refused-not-current-phase-in-one-request-and-the-late-completion-is-taken-naming-the-review-record", r.eachAnswerIsTheBranchs)
}

// newLateCompletionLive readies everything the panes need before the daemon boots: the real
// Dispatch with the project, its repository mapping and the issue; the `legion` binary and the
// two plugins built from this checkout; an Oh My Pi profile routed to the stand-ins; the panes'
// jj workspace at the implementer's pushed head; GitHub's stand-in at that head; the store
// seeded; and the daemon booted over the local runtime.
func newLateCompletionLive(t *testing.T) *lateCompletionLive {
	t.Helper()
	omp := os.Getenv(liveOmpPathEnv)
	if omp == "" {
		t.Skipf("%s is unset, so there is no Oh My Pi for a pane to run; scripts/e2e/late-completion.sh requires it", liveOmpPathEnv)
	}
	for _, tool := range []string{"bun", "jj", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is not on PATH: %v", tool, err)
		}
	}
	takeOwnDatabase(t)
	proof := newLiveProof(t, "scripts/e2e/late-completion.sh")
	// A Dispatch project key is at most ten characters (contracts ProjectKeyPattern), and the
	// daemon's project is its Dispatch project, so the run's project is a short one of its own.
	proof.cfg.Project = "LC" + randomSuffix(t)[:6]
	proof.cfg.Projects = map[string]config.Project{proof.cfg.Project: {}}
	// The loader's own default (config's resolve), which workflowConfig leaves at zero: a pane's
	// tool reads GET /legion/v1/state through the daemon API contract, which takes a positive cap
	// alone and refuses the whole state otherwise.
	proof.cfg.ReviewRoundCap = 3
	var err error
	if proof.project, err = claim.ProjectToken(proof.cfg.Project); err != nil {
		t.Fatal(err)
	}
	number, err := strconv.ParseInt(randomSuffix(t)[:6], 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	r := &lateCompletionLive{
		liveAgentProof: liveAgentProof{liveProof: proof, omp: omp},
		tokens:         &workflowTokenRecorder{},
		title:          "a role completes a phase the issue has left",
		number:         int(number) + 1,
		panes:          map[claim.Token]*completionStandin{},
	}
	version, err := exec.Command(omp, "--version").Output()
	if err != nil {
		t.Fatalf("read the Oh My Pi version of %s: %v", omp, err)
	}
	r.note("Oh My Pi %s at %s (the pod's build, which is the fleet's pin and not this branch's fetch)",
		strings.TrimSpace(string(version)), omp)

	root := proofCheckoutRoot(t)
	r.dispatch = startHandoffDispatch(t, root)
	if err := os.WriteFile(r.cfg.DispatchTokenFile, []byte(r.dispatch.token+"\n"), 0o600); err != nil {
		t.Fatalf("write the Dispatch token file: %v", err)
	}
	r.cfg.DispatchURL = r.dispatch.base
	r.setUpDispatch()

	work := t.TempDir()
	r.legion = buildProofLegion(t, root, work)
	r.home = filepath.Join(work, "omp-home")
	r.profile = "legion-late-completion-" + strings.ToLower(r.cfg.Project)
	installProofPlugins(t, root, work, r.home, r.profile, omp)
	r.panes[r.token(claim.RoleImplementer)] = newCompletionStandin(t, lateImplementerProvider)
	r.panes[r.token(claim.RoleTester)] = newCompletionStandin(t, lateTesterProvider)
	r.panes[r.token(claim.RoleReviewer)] = newCompletionStandin(t, lateReviewerProvider)
	r.spare = newPaneStandin(t, lateSpareProvider)
	r.writeOmpProfile()
	r.writeOverlays(work)

	r.rt = newLocalRuntime(t, r.cfg.StateDir, r.legion, omp, r.home, r.profile)
	r.rt.daemonURL, r.rt.envoyURL, r.rt.natsURLs = r.cfg.DaemonURL, newPaneListener(t), r.cfg.NatsURLs
	r.rt.dispatchURL, r.rt.dispatchTokenFile = r.cfg.DispatchURL, r.cfg.DispatchTokenFile
	r.rt.overlays = func(token claim.Token) []string {
		if overlays, driven := r.overlays[token]; driven {
			return overlays
		}
		return r.spareOverlays
	}
	r.initWorkspace(work)
	r.serveGitHub()

	// Each pane's first turn is armed before the boot, since the task that starts it is sent the
	// moment the pane reports ready. The implementer's is held until step 1 has written the record
	// it reads; the tester's until step 3 has; the reviewer's until step 5's approval has moved the
	// issue to accepting.
	r.implementerCall = r.panes[r.token(claim.RoleImplementer)].arm("Phase: implementing.",
		map[string]any{"op": "handoff_complete", "summary": "Implemented the change at the head the implement record names, pushed to the branch."}, true)
	r.testerCall = r.panes[r.token(claim.RoleTester)].arm("Phase: testing.",
		map[string]any{"op": "handoff_complete", "summary": "Tested the head the test record names: it passes.", "verdict": "pass"}, true)
	r.reviewerCall = r.panes[r.token(claim.RoleReviewer)].arm("Phase: reviewing.",
		map[string]any{"op": "handoff_complete", "summary": "Reviewed the head the review record names and approved it."}, true)

	r.seed()
	r.daemon = newRecordingLog(r.log())
	r.d = startDaemonWithin(t, r.cfg, r.overrides(), slog.New(r.daemon), 2*time.Minute)
	r.note("Dispatch %s (database of its own, NATS off), project %s mapped to %s, issue %s; daemon at dispatch_url %s, each pane told it as DISPATCH_URL",
		r.dispatch.base, r.cfg.Project, liveRepo, r.key, r.cfg.DispatchURL)
	return r
}

// setUpDispatch creates the run's project on Dispatch, maps liveRepo to it as its default, and
// creates the issue, in progress and carrying the legion label, as a running daemon leaves it.
func (r *lateCompletionLive) setUpDispatch() {
	t := r.t
	t.Helper()
	r.dispatch.user(http.MethodPost, "/api/v1/projects", map[string]string{"key": r.cfg.Project, "name": "Late completion live proof"}, http.StatusCreated, nil)
	r.dispatch.user(http.MethodPut, "/api/v1/settings/repo-projects/"+liveRepo, map[string]any{"project": r.cfg.Project, "default": true}, http.StatusOK, nil)
	var created struct {
		Key string `json:"key"`
	}
	r.dispatch.user(http.MethodPost, "/api/v1/issues", map[string]string{"project": r.cfg.Project, "title": r.title}, http.StatusCreated, &created)
	if created.Key == "" {
		t.Fatalf("Dispatch created the issue %q with no key", r.title)
	}
	r.dispatch.user(http.MethodPatch, "/api/v1/issues/"+created.Key, map[string]any{"status": "in_progress", "labels": []string{"legion"}}, http.StatusOK, nil)
	r.key = created.Key
}

// token is the claim token of role on the proof's issue, as the daemon spells it.
func (r *lateCompletionLive) token(role claim.Role) claim.Token {
	r.t.Helper()
	token, err := claim.NewToken(r.project, r.key, role)
	if err != nil {
		r.t.Fatal(err)
	}
	return token
}

// initWorkspace makes the panes' working directory the issue's jj workspace as its implementer
// leaves a pushed round: a repository made with jj's own `git init`, colocated as jj makes one by
// default, whose `@-` is the implementer's commit, on `legion/<issue>` of an origin of the run's
// own (a bare git repository under work), with `legion/<issue>@origin` at that commit, so the jj a
// pane's tool runs there finds `@-` on the branch at origin. That commit is the pull request's
// head, which every record names. The origin takes the branch with a fetch of its own from the
// workspace, and the workspace then fetches the origin, which leaves both where a push leaves
// them: a pod's git runs its leak guard on every push (/opt/legion/bin/git), which needs the pod's
// own environment, and a run's own origin under TMPDIR publishes nothing for it to guard. Every
// command runs under fixturegit.Isolated of the panes' HOME: the panes are told that HOME and no
// XDG_CONFIG_HOME (localRuntime's paneEnvironment), so their jj reads the repository
// configuration under the same home as the driver's, and opens the same repository in the same
// working directory.
func (r *lateCompletionLive) initWorkspace(work string) {
	t := r.t
	t.Helper()
	r.run = fixturegit.Isolated(t, r.home)
	r.workspace = r.rt.workDir(r.key)
	if err := os.MkdirAll(r.workspace, 0o700); err != nil {
		t.Fatalf("make the panes' working directory %s: %v", r.workspace, err)
	}
	origin := filepath.Join(work, "origin.git")
	branch := "legion/" + r.key
	r.run.Run(r.workspace, fixturegit.JJInit("", true)...)
	writeProofFile(t, filepath.Join(r.workspace, "implementation.txt"), "The change "+r.key+" asks for.\n")
	r.workspaceJJ("commit", "-m", "implement "+r.key, "implementation.txt")
	r.workspaceJJ("bookmark", "set", branch, "-r", "@-")
	r.head = r.workspaceJJ("log", "-r", "@-", "--no-graph", "-T", "commit_id")
	ref := "refs/heads/" + branch
	r.run.Run(work, fixturegit.InitBare(origin, "")...)
	r.run.Run(work, "git", "--git-dir", origin, "fetch", "--quiet", r.workspaceJJ("git", "root"), ref+":"+ref)
	r.workspaceJJ("git", "remote", "add", "origin", origin)
	r.workspaceJJ("git", "fetch", "--remote", "origin")
	if pushed := r.pushedHead(); pushed != r.head {
		t.Fatalf("%s@origin is %q, want the implementer's commit %s", branch, pushed, r.head)
	}
	r.note("the panes' workspace %s stands on the implementer's commit %s, which %s@origin is (%s)", r.workspace, r.head, branch, origin)
}

// workspaceJJ runs jj in the panes' workspace under the driver's runner and answers its trimmed
// stdout; a failure ends the step with its stderr.
func (r *lateCompletionLive) workspaceJJ(args ...string) string {
	r.t.Helper()
	return strings.TrimSpace(r.run.Out(r.workspace, append([]string{"jj"}, args...)...))
}

// pushedHead is `legion/<issue>@origin` as the workspace's jj records it, the revset the tool's
// push check reads.
func (r *lateCompletionLive) pushedHead() string {
	r.t.Helper()
	return r.workspaceJJ("log", "-r", fmt.Sprintf("remote_bookmarks(exact:%q, exact:\"origin\")", "legion/"+r.key), "--no-graph", "-T", "commit_id")
}

// overrides is the daemon this proof boots: the local runtime over the real worker stream, the
// API listener the fixture holds, the workflow's App tokens stood in for, and GitHub's stand-in.
func (r *lateCompletionLive) overrides() overrides {
	return overrides{
		runtime: func(_ context.Context, listener *stream.Listener, address string, _ appauth.Tokens,
			_ func(context.Context, string) (ghrepo.Repository, string, error), _ *store.Store, _ func() []ghrepo.Repository) (runtime.Runtime, error) {
			r.rt.conns, r.rt.stream = listener, address
			return r.rt, nil
		},
		listen:         heldListen,
		workflowTokens: r.tokens,
		githubAPI:      r.github.url,
	}
}

// seed records the issue as the daemon would hold it at the moment its implementer began: a root
// handed to Legion, in implementing, on an open pull request at the implementer's pushed head that
// GitHub would merge (mergeable and clean, so the approval of step 5 moves the issue on rather
// than waiting for GitHub's answer), holding its tree's admission slot with its lifecycle open,
// and its implementer's claim mid-launch, which this boot relaunches. The claim holds the task its
// phase's start left pending, written as the daemon writes a phase's task line, which is what
// admission's repair of a slotted worker passes over. Every pane's role prompt is an operator's
// short prompt file (writePrompt): nothing here reads a prompt.
func (r *lateCompletionLive) seed() {
	t := r.t
	t.Helper()
	ctx := context.Background()
	listed := r.dispatch.issue(r.key)
	if err := r.st.Tx(ctx, func(tx pgx.Tx) error {
		if err := r.records.PutIssue(ctx, tx, record.Issue{
			Key: r.key, Repo: liveRepo, DefaultBranch: "main", GitHubRepo: liveRepo, Tree: r.key, Project: r.cfg.Project,
			Title: listed.Title, Phase: phase.Implementing, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress",
			Rank: listed.Rank, LastDispatchSeq: listed.LastSeq,
		}); err != nil {
			return err
		}
		if err := r.records.PutPullRequest(ctx, tx, record.PullRequest{
			State: record.PullRequestOpen, Issue: r.key, Repo: liveRepo, Number: r.number, Branch: "legion/" + r.key,
			HeadSHA: r.head, Base: "main", Failing: []string{},
			Mergeability: record.MergeabilityMergeable, MergeState: record.MergeStateClean,
		}); err != nil {
			return err
		}
		return r.admit(ctx, tx, r.key)
	}); err != nil {
		t.Fatalf("seed %s: %v", r.key, err)
	}
	if _, err := r.st.OpenTreeLifecycle(ctx, r.project, r.key, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("open the lifecycle of %s: %v", r.key, err)
	}
	implementer := r.token(claim.RoleImplementer)
	if _, err := r.st.AdmitClaim(ctx, supervise.Claim{
		Token: implementer, Project: r.project, Tree: r.key, Issue: r.key, Role: claim.RoleImplementer,
		Generation: 1, State: supervise.StateLaunching,
		BootTokenHash: supervise.HashBootToken("interrupted-" + randomSuffix(t)), ServingGeneration: 1,
	}); err != nil {
		t.Fatalf("record the implementer's unfinished launch: %v", err)
	}
	task := fmt.Sprintf("Continue %s. Issue: %s. Phase: implementing. Pull request: #%d.", listed.Title, r.key, r.number)
	if err := r.st.PutDelivery(ctx, implementer, supervise.Delivery{
		ID: "seeded-start-" + randomSuffix(t), Task: task, Generation: 1, Phase: phase.Implementing, QueuedAt: time.Now(),
	}); err != nil {
		t.Fatalf("record the task the implementer's start left pending: %v", err)
	}
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer} {
		writePrompt(t, r.cfg, r.token(role))
	}
	r.note("seeded %s in %s on pull request #%d at head %s (mergeable, clean), with its implementer mid-launch holding %q",
		r.key, phase.Implementing, r.number, r.head, task)
}

// ---- the steps -----------------------------------------------------------------------------------

// Step 1. The implementer's pane takes its implementing task, its turn held at the stand-in; the
// driver writes the implementer's implement record as that pane's own session, naming the
// workspace's `@-`, which `legion/<issue>@origin` carries; then the turn calls handoff_complete;
// the completion is taken, naming that record at that head, and the issue moves to testing. The
// tool and the daemon answer it alike on this tree and the negative control's: the implementer
// works the issue's phase, and its round is pushed and named.
func (r *lateCompletionLive) implementerCompletes() {
	t := r.t
	implementer := r.token(claim.RoleImplementer)
	held := r.heldTurn(claim.RoleImplementer, phase.Implementing, r.implementerCall)
	r.implementRecord = r.writeRecord(held.Session, "implement")
	mark := time.Now()
	r.implementerCall.release()
	result := r.implementerCall.awaitResult(t, 2*time.Minute)
	r.note("the stand-in's turn called legion %s; the tool answered (error %t): %s", inputOf(r.implementerCall), result.isError, result.text)
	line := r.settledAnswer("the implementer's completion", mark, implementer)
	r.note("daemon: %s", answeredLine(line))
	if result.isError || line.Attrs["status"] != float64(http.StatusOK) || line.Attrs["handoffId"] != r.implementRecord.ID || line.Attrs["commit"] != r.head {
		t.Fatalf("the implementer's completion of implementing was answered %v %v naming record %v at commit %v, and its tool answered %q; want 200 naming its own implement record %s at its pushed head %s",
			line.Attrs["status"], line.Attrs["code"], line.Attrs["handoffId"], line.Attrs["commit"], result.text, r.implementRecord.ID, r.head)
	}
	got := r.waitPhase(phase.Testing)
	r.note("read back: %s is in %s; the implementer's phases row holds record %s", r.key, got.Phase, heldRecord(got, claim.RoleImplementer))
}

// Step 2. The finished implementer, whose implementing phase already holds the completion of
// step 1, while the issue is in testing. The driver first makes the tester's commit in the shared
// workspace (commitAsTester), so the commit the workspace stands on is one `legion/<issue>@origin`
// does not carry; then the operator's own task (`legion claims deliver`) gives the implementer a
// second turn, and the stand-in's turn calls handoff_complete again. The step records the tool's
// answer and every line the daemon wrote for the implementer's claim from the delivery until a
// settle after the tool answered, and holds that the issue is still in testing. Step 6 judges
// both, the number of requests among them: the negative control's tool refuses this call itself,
// naming the push of the tester's commit, so its run reaches step 6 with no line for the call.
func (r *lateCompletionLive) finishedImplementerRepeats() {
	t := r.t
	r.testerCommit = r.commitAsTester()
	result, mark := r.repeatCompletion(claim.RoleImplementer, lateImplementerRepeat,
		map[string]any{"op": "handoff_complete", "summary": "Implementing is done; reporting it again."})
	implementer := r.token(claim.RoleImplementer)
	lines := r.settledAnswers(mark, implementer)
	for _, line := range lines {
		r.note("daemon: %s", answeredLine(line))
	}
	r.note("read back: requests the daemon answered for the implementer's claim between the delivery and %s after the tool's answer: %d", liveSettle, len(lines))
	if after := r.issue(r.key); after.Phase != phase.Testing {
		t.Fatalf("%s is in %s after the finished implementer's repeat, want testing: a completion of a phase the issue left moves nothing", r.key, after.Phase)
	}
	r.note("read back: %s is still in %s", r.key, phase.Testing)
	r.implementerRepeat = lateAnswer{result: result, lines: lines}
}

// Step 3. The tester the implementer's completion started takes its testing task, its turn held
// at the stand-in; the driver writes the tester's test record as that pane's own session; then
// the turn calls handoff_complete with verdict pass; the completion is taken, naming that record,
// and the issue moves to reviewing. The tool and the daemon answer it alike on this tree and the
// negative control's: the tester works the issue's phase.
func (r *lateCompletionLive) testerCompletes() {
	t := r.t
	tester := r.token(claim.RoleTester)
	held := r.heldTurn(claim.RoleTester, phase.Testing, r.testerCall)
	r.testRecord = r.writeRecord(held.Session, "test")
	mark := time.Now()
	r.testerCall.release()
	result := r.testerCall.awaitResult(t, 2*time.Minute)
	r.note("the stand-in's turn called legion %s; the tool answered (error %t): %s", inputOf(r.testerCall), result.isError, result.text)
	line := r.settledAnswer("the tester's completion", mark, tester)
	r.note("daemon: %s", answeredLine(line))
	if result.isError || line.Attrs["status"] != float64(http.StatusOK) || line.Attrs["handoffId"] != r.testRecord.ID {
		t.Fatalf("the tester's completion of testing was answered %v %v naming record %v, and its tool answered %q; want 200 naming its own test record %s",
			line.Attrs["status"], line.Attrs["code"], line.Attrs["handoffId"], result.text, r.testRecord.ID)
	}
	got := r.waitPhase(phase.Reviewing)
	r.note("read back: %s is in %s; the tester's phases row holds record %s", r.key, got.Phase, heldRecord(got, claim.RoleTester))
}

// Step 4. The finished tester, whose testing phase already holds the completion of step 3, is
// given a second turn by the operator's own task (`legion claims deliver`), and the stand-in's
// turn calls handoff_complete again with verdict pass. The step records the tool's answer and the
// daemon's line for it, and holds that the call made one request: the daemon wrote exactly one
// line for the tester's claim from the delivery until a settle after the tool answered, so the
// plugin's retry, which runs only while the daemon gives no answer, sent nothing more. Step 6
// judges the answer.
func (r *lateCompletionLive) finishedTesterRepeats() {
	result, mark := r.repeatCompletion(claim.RoleTester, lateTesterRepeat,
		map[string]any{"op": "handoff_complete", "summary": "Testing is done; reporting it again.", "verdict": "pass"})
	line := r.settledAnswer("the finished tester's repeat", mark, r.token(claim.RoleTester))
	r.note("daemon: %s", answeredLine(line))
	r.note("read back: the daemon answered the tester's claim once between the delivery and %s after the tool's answer", liveSettle)
	r.testerRepeat = lateAnswer{result: result, lines: []loggedRecord{line}}
}

// Step 5. The reviewer the tester's pass started takes its reviewing task, and its turn is held
// at the stand-in. Meanwhile the driver writes the reviewer's review record as that pane's own
// session and publishes the review App's approval of the head, its footer naming the reviewer's
// session and that head, as Envoy relays a GitHub review; the daemon moves the issue to
// accepting. Then the turn goes on and calls handoff_complete. The step records the tool's answer
// and the daemon's one line for it, and holds that the issue stays in accepting. Step 6 judges
// the answer.
func (r *lateCompletionLive) reviewerCompletesLate() {
	t := r.t
	reviewer := r.token(claim.RoleReviewer)
	session := r.heldTurn(claim.RoleReviewer, phase.Reviewing, r.reviewerCall).Session
	r.reviewRecord = r.writeRecord(session, "review")
	login, err := r.tokens.AppLogin(context.Background(), appauth.Review)
	if err != nil {
		t.Fatalf("read the review App's login: %v", err)
	}
	event := r.publishApproval(login, session)
	r.waitProcessed(event)
	r.note("published the review App's approval of head %s on pull request #%d as %s, its footer naming session %s (event %s), and the daemon processed it",
		r.head, r.number, login, session, event)
	moved := r.waitPhase(phase.Accepting)
	r.note("read back: %s is in %s while the reviewer's turn is still held; the reviewer's phases row holds record %s",
		r.key, moved.Phase, heldRecord(moved, claim.RoleReviewer))
	mark := time.Now()
	r.reviewerCall.release()
	result := r.reviewerCall.awaitResult(t, 2*time.Minute)
	r.note("the stand-in's turn called legion %s; the tool answered (error %t): %s", inputOf(r.reviewerCall), result.isError, result.text)
	line := r.settledAnswer("the reviewer's late completion", mark, reviewer)
	r.note("daemon: %s", answeredLine(line))
	if after := r.issue(r.key); after.Phase != phase.Accepting {
		t.Fatalf("%s is in %s after the reviewer's late completion, want accepting: a completion of a phase the issue left moves nothing", r.key, after.Phase)
	}
	r.note("read back: %s is still in %s", r.key, phase.Accepting)
	r.late = lateAnswer{result: result, lines: []loggedRecord{line}}
}

// Step 6. The three late answers are this branch's. Each finished role's repeat made one request,
// naming its own record at the head that record names, and was refused
// `409 HANDOFF_NOT_CURRENT_PHASE`, the refusal the tool passes to the agent: the implementer's
// carried the implementer's head and not the tester's commit its workspace stands on, so the tool
// ran none of its push checks and the daemon answered it. The reviewer's late completion names its
// own review record and is taken, which GET /legion/v1/state shows as that record on the
// reviewer's phases row. Every mismatch is named in one failure, so a negative control's run
// reads every answer.
func (r *lateCompletionLive) eachAnswerIsTheBranchs() {
	t := r.t
	var wrong []string
	if problem := refusedNotCurrent("the finished implementer's repeat", r.implementerRepeat, r.implementRecord, r.head); problem != "" {
		wrong = append(wrong, problem)
	}
	if problem := refusedNotCurrent("the finished tester's repeat", r.testerRepeat, r.testRecord, r.head); problem != "" {
		wrong = append(wrong, problem)
	}
	late := r.late.lines[0].Attrs
	if late["status"] != float64(http.StatusOK) || late["handoffId"] != r.reviewRecord.ID ||
		late["handoffVersion"] != float64(r.reviewRecord.Version) || r.late.result.isError {
		wrong = append(wrong, fmt.Sprintf("the reviewer's late completion was answered %v %v naming record %q version %v, and its tool answered %q; want 200 naming its own review record %s version %d",
			late["status"], late["code"], late["handoffId"], late["handoffVersion"], r.late.result.text, r.reviewRecord.ID, r.reviewRecord.Version))
	}
	held := r.issue(r.key).Workers[claim.RoleReviewer].HandoffRecord
	if held == nil || held.ID != r.reviewRecord.ID || held.Version != r.reviewRecord.Version {
		wrong = append(wrong, fmt.Sprintf("GET /legion/v1/state shows the reviewer's handoffRecord %+v, want %s version %d", held, r.reviewRecord.ID, r.reviewRecord.Version))
	}
	if len(wrong) > 0 {
		t.Fatal(strings.Join(wrong, "; "))
	}
	r.note("the finished implementer's repeat: 409 HANDOFF_NOT_CURRENT_PHASE, naming its own implement record %s (version %d) at its head %s, not the tester's commit %s its workspace stands on, in one request",
		r.implementRecord.ID, r.implementRecord.Version, r.head, r.testerCommit)
	r.note("the finished tester's repeat: 409 HANDOFF_NOT_CURRENT_PHASE, naming its own test record %s (version %d), in one request", r.testRecord.ID, r.testRecord.Version)
	r.note("the reviewer's late completion: 200, naming its own review record %s (version %d), in one request", r.reviewRecord.ID, r.reviewRecord.Version)
	r.note("read back: GET /legion/v1/state shows the reviewer's handoffRecord %s version %d", held.ID, held.Version)
}

// refusedNotCurrent says how what, a finished role's repeat, was answered other than this branch
// answers it: one request, naming want at head, refused 409 HANDOFF_NOT_CURRENT_PHASE, with that
// code in the tool's answer. It is "" for a repeat answered so.
func refusedNotCurrent(what string, answer lateAnswer, want handoffRecord, head string) string {
	if len(answer.lines) != 1 {
		return fmt.Sprintf("%s made %d requests to the daemon, and its tool answered %q; want one request, refused 409 HANDOFF_NOT_CURRENT_PHASE naming record %s version %d at commit %s",
			what, len(answer.lines), answer.result.text, want.ID, want.Version, head)
	}
	got := answer.lines[0].Attrs
	if got["status"] != float64(http.StatusConflict) || got["code"] != "HANDOFF_NOT_CURRENT_PHASE" ||
		got["handoffId"] != want.ID || got["handoffVersion"] != float64(want.Version) || got["commit"] != head ||
		!answer.result.isError || !strings.Contains(answer.result.text, "409 HANDOFF_NOT_CURRENT_PHASE") {
		return fmt.Sprintf("%s was answered %v %v naming record %q version %v at commit %q, and its tool answered %q; want 409 HANDOFF_NOT_CURRENT_PHASE naming record %s version %d at commit %s",
			what, got["status"], got["code"], got["handoffId"], got["handoffVersion"], got["commit"], answer.result.text, want.ID, want.Version, head)
	}
	return ""
}

// ---- what a step drives and reads ----------------------------------------------------------------

// writeRecord writes the issue's handoff of word to Dispatch's own route as session, the pane's
// own Oh My Pi session, with a body naming the pull request's head, reads it back as the newest
// record of word, and prints it.
func (r *lateCompletionLive) writeRecord(session, word string) handoffRecord {
	t := r.t
	t.Helper()
	status, answer := r.dispatch.agentPost("/api/v1/issues/"+r.key+"/handoffs/"+word, map[string]any{
		"actor": map[string]string{"kind": "session", "id": session}, "body": string(headBody(r.key, word, r.head)),
	})
	if status != http.StatusCreated {
		t.Fatalf("the write of %s's %s handoff as session %s = %d %s, want 201", r.key, word, session, status, answer)
	}
	written := r.dispatch.latest(r.key, word)
	if written.Author.Kind != "session" || written.Author.ID != session || recordHeadOf(written.Body) != r.head {
		t.Fatalf("the newest %s record of %s = %+v, want one by session %s naming head %s", word, r.key, written, session, r.head)
	}
	r.note("wrote %s's %s record as session %s: version %d (id %s), head %s", r.key, word, session, written.Version, written.ID, r.head)
	return written
}

// recordHeadOf is the `head` a record's body names, "" when it names none.
func recordHeadOf(body string) string {
	var named struct {
		Head string `json:"head"`
	}
	_ = json.Unmarshal([]byte(body), &named)
	return named.Head
}

// publishApproval publishes the review App's approval of the head as Envoy relays a GitHub review
// (intake's decodeReview reads it): submitted by login, at the head, with the Legion footer
// naming session and that head (legionFooter), which is what lets a review App's review decide
// the round (workflow's DecidesRound and refusesRound). It returns the event id.
func (r *lateCompletionLive) publishApproval(login, session string) string {
	r.t.Helper()
	n := strconv.Itoa(r.number)
	payload, err := json.Marshal(map[string]string{
		"kind": "review", "action": "submitted", "repo": liveRepo, "number": n, "title": r.title, "parent_kind": "pr",
		"author": login, "url": "https://github.com/" + liveRepo + "/pull/" + n + "#review", "state": "approved",
		"review_id": n, "submitted_at": time.Now().UTC().Format(time.RFC3339), "commit_id": r.head, "head_sha": r.head,
		"body": "Approved: the head is the code this review read." + legionFooter(session, r.head),
	})
	if err != nil {
		r.t.Fatalf("encode the review payload: %v", err)
	}
	subject := fmt.Sprintf("notifications.github.%s.pr.%d.review", r.subjectRepo(), r.number)
	return r.publish(subject, "github", fmt.Sprintf("review-%d-approved", r.number), string(payload))
}

// waitPhase waits for the issue to reach want.
func (r *lateCompletionLive) waitPhase(want phase.Phase) api.Issue {
	r.t.Helper()
	var got api.Issue
	r.await(fmt.Sprintf("%s in %s", r.key, want), time.Minute, func() bool {
		got = r.issue(r.key)
		return got.Phase == want
	})
	return got
}

// heldTurn waits for the daemon to hold role's claim and for the turn call is armed on to reach
// role's stand-in, prints the claim and the task that turn began with, and answers the claim,
// whose session is the one its pane registered. The task of p is sent the moment the pane reports
// ready and the turn is held at the stand-in, so the claim goes from registered to working with
// no ready state a poll could read: the held turn reaching the stand-in is what says the pane
// registered and took its task.
func (r *lateCompletionLive) heldTurn(role claim.Role, p phase.Phase, call *paneCall) api.OperatorClaim {
	r.t.Helper()
	token := r.token(role)
	r.await("the daemon to start the "+string(role), 2*time.Minute, func() bool {
		return slices.ContainsFunc(r.d.claims(), func(c api.OperatorClaim) bool { return c.Token == token })
	})
	task := call.awaitReached(r.t, paneReady)
	held := r.claim(token)
	r.note("%s is %s in session %s, plugin contract %d, process %s", token, held.State, held.Session, held.PluginContract, held.Locator.Label())
	r.note("the %s's pane is in the turn its %s task began, held at the stand-in: %q", role, p, task)
	return held
}

// repeatCompletion waits for the finished role's last turn to end, gives it a second turn by the
// operator's own task (`legion claims deliver`, lateRepeatTask), and has the stand-in's turn call
// the `legion` tool with input. It answers the tool's answer, and the moment before the delivery,
// after which every line of the daemon's for the role's claim is of that call.
func (r *lateCompletionLive) repeatCompletion(role claim.Role, marker string, input map[string]any) (toolResult, time.Time) {
	t := r.t
	t.Helper()
	token := r.token(role)
	r.await(fmt.Sprintf("the %s's last turn to end", role), time.Minute, func() bool {
		state := r.claim(token).State
		return state == string(supervise.StateIdle) || state == string(supervise.StateReady)
	})
	call := r.panes[token].arm(marker, input, false)
	task := lateRepeatTask(marker)
	mark := time.Now()
	r.note("legion claims deliver --claim %s --task %q:\n%s", token, task,
		indentOutput(r.legionClaims("deliver", "--claim", string(token), "--task", task)))
	r.note("the %s's pane is in the turn the operator's task began: %q", role, call.awaitReached(t, 2*time.Minute))
	result := call.awaitResult(t, 2*time.Minute)
	r.note("the stand-in's turn called legion %s; the tool answered (error %t): %s", inputOf(call), result.isError, result.text)
	return result, mark
}

// commitAsTester makes the tester's commit in the panes' shared workspace, as a tester that
// commits its notes in the issue's one checkout does: on top of the implementer's head, and
// pushed nowhere, so the workspace's `@-` is a commit `legion/<issue>@origin` does not carry. It
// reads that back and answers the commit.
func (r *lateCompletionLive) commitAsTester() string {
	t := r.t
	t.Helper()
	writeProofFile(t, filepath.Join(r.workspace, "test-notes.txt"), "The tester's notes on "+r.head+".\n")
	r.workspaceJJ("commit", "-m", "test "+r.key+": notes on the head under test", "test-notes.txt")
	commit := r.workspaceJJ("log", "-r", "@-", "--no-graph", "-T", "commit_id")
	parent := r.workspaceJJ("log", "-r", "@--", "--no-graph", "-T", "commit_id")
	pushed := r.pushedHead()
	if commit == r.head || parent != r.head || pushed != r.head {
		t.Fatalf("the shared workspace stands on %s, whose parent is %s, with legion/%s@origin at %s; want the tester's commit on top of the implementer's head %s, which the branch at origin still is",
			commit, parent, r.key, pushed, r.head)
	}
	r.note("read back: the shared workspace stands on the tester's commit %s, on top of the implementer's head %s; legion/%s@origin is still %s, so it does not carry the tester's commit",
		commit, parent, r.key, pushed)
	return commit
}

// answersSince is every line the daemon wrote for token's claim after mark, oldest first.
func (r *lateCompletionLive) answersSince(mark time.Time, token claim.Token) []loggedRecord {
	return r.logs(func(l loggedRecord) bool {
		return l.Message == lateCompletionAnswered && l.Attrs["claim"] == string(token) && l.at.After(mark)
	})
}

// settledAnswers is every line the daemon wrote for token's claim after mark, read liveSettle
// after the tool answered the call a step made, so the line of each request that call made has
// been written: none when the tool refused the call itself.
func (r *lateCompletionLive) settledAnswers(mark time.Time, token claim.Token) []loggedRecord {
	time.Sleep(liveSettle)
	return r.answersSince(mark, token)
}

// settledAnswer is the daemon's one line for the completion a step's call made: it waits for the
// first line for token's claim written after mark, then reads settledAnswers, and fails unless
// exactly one such line was written, which is one request for the call.
func (r *lateCompletionLive) settledAnswer(what string, mark time.Time, token claim.Token) loggedRecord {
	r.t.Helper()
	r.await("the daemon's line for "+what, time.Minute, func() bool { return len(r.answersSince(mark, token)) > 0 })
	lines := r.settledAnswers(mark, token)
	if len(lines) != 1 {
		described := make([]string, 0, len(lines))
		for _, line := range lines {
			described = append(described, answeredLine(line))
		}
		r.t.Fatalf("the daemon answered %d requests for %s, want one:\n%s", len(lines), what, strings.Join(described, "\n"))
	}
	return lines[0]
}

// answeredLine is one of the completion route's lines as a step prints it.
func answeredLine(l loggedRecord) string {
	return fmt.Sprintf("%s issue=%v role=%v handoffId=%q handoffVersion=%v commit=%q status=%v code=%q",
		l.Message, l.Attrs["issue"], l.Attrs["role"], l.Attrs["handoffId"], l.Attrs["handoffVersion"], l.Attrs["commit"],
		l.Attrs["status"], l.Attrs["code"])
}

// heldRecord is the handoff record role's phases row holds in issue, as a note prints it.
func heldRecord(issue api.Issue, role claim.Role) string {
	held := issue.Workers[role].HandoffRecord
	if held == nil {
		return "none"
	}
	return fmt.Sprintf("%s (version %d)", held.ID, held.Version)
}

// inputOf is the input a call carried, as a note prints it.
func inputOf(call *paneCall) string {
	encoded, _ := json.Marshal(call.input)
	return string(encoded)
}

// serveGitHub starts the harness's GitHub stand-in (liveGitHub) for this proof's pull request as
// its daemon reads it, and holds it as the proof's github: the pull request at the implementer's
// pushed head, open on base main, mergeable and clean (its merge state, which the harness's
// stand-in answers only where a proof sets one); its reviews as none, since the one review the
// proof reads is the envelope step 5 publishes; and, beyond the stand-in's own answers (its
// also), the same pull request listed open on the issue's branch, which is the read the
// implementer's completion in implementing makes of GitHub (api's implementationPullRequest).
func (r *lateCompletionLive) serveGitHub() {
	branch := "legion/" + r.key
	r.github = &liveGitHub{
		t: r.t, key: r.key, number: r.number, base: "main", sha: r.head, mergeState: "clean",
		also: map[string]any{"/pulls": []any{map[string]any{
			"number": r.number, "state": "open",
			"base": map[string]string{"ref": "main"},
			"head": map[string]string{"ref": branch, "sha": r.head},
		}}},
	}
	r.github.url = githubtest.Server(r.t, githubtest.Spec{Handle: r.github.ServeHTTP})
}

// ---- the Oh My Pi profile and the overlays -----------------------------------------------------------

// writeOmpProfile writes the profile the panes share: the four stand-in providers, one model
// each, and the settings a pane needs to run unattended.
func (r *lateCompletionLive) writeOmpProfile() {
	t := r.t
	t.Helper()
	agent := filepath.Join(r.home, ".omp", "profiles", r.profile, "agent")
	if err := os.MkdirAll(agent, 0o700); err != nil {
		t.Fatalf("make the profile's agent directory %s: %v", agent, err)
	}
	var models, enabled strings.Builder
	models.WriteString("# Written by packages/daemon/internal/daemon/late_completion_live_test.go.\nproviders:\n")
	for _, stand := range []*modelStandin{
		r.panes[r.token(claim.RoleImplementer)].modelStandin, r.panes[r.token(claim.RoleTester)].modelStandin,
		r.panes[r.token(claim.RoleReviewer)].modelStandin, r.spare.modelStandin,
	} {
		fmt.Fprintf(&models, "  %s:\n    baseUrl: %s\n    api: anthropic-messages\n    apiKey: a-literal-key-no-endpoint-checks\n"+
			"    models:\n      - id: %s\n        name: %s\n        contextWindow: 200000\n        maxTokens: 8192\n",
			stand.name, stand.url, stand.name+standinModelID, stand.name)
		fmt.Fprintf(&enabled, "  - %s\n", standinModel(stand.name))
	}
	writeProofFile(t, filepath.Join(agent, "models.yml"), models.String())
	writeProofFile(t, filepath.Join(agent, "config.yml"),
		"# Written by packages/daemon/internal/daemon/late_completion_live_test.go.\n"+
			"enabledModels:\n"+enabled.String()+
			// A fresh profile opens Oh My Pi's setup wizard, and a pane waiting on it never starts.
			"startup:\n  setupWizard: false\n")
}

// writeOverlays writes the Oh My Pi overlays each pane is told through PI_CONFIG_FILES: the
// operator's own, which every pane names first and which turns `retry.enabled` off so the host
// walks no fallback chain of its own, then one naming the single model the claim's stand-in
// serves (writeRoleOverlay): the implementer's, the tester's and the reviewer's own, and the
// spare's for every other claim.
func (r *lateCompletionLive) writeOverlays(work string) {
	t := r.t
	t.Helper()
	dir := filepath.Join(work, "overlays")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	operator := filepath.Join(dir, "operator.yml")
	writeProofFile(t, operator, "# The operator's pane overlay (runtime.tmux.pi_config_files).\nretry:\n  enabled: false\n")
	r.overlays = map[claim.Token][]string{}
	for role, provider := range map[claim.Role]string{
		claim.RoleImplementer: lateImplementerProvider, claim.RoleTester: lateTesterProvider, claim.RoleReviewer: lateReviewerProvider,
	} {
		path := filepath.Join(dir, string(role)+".yml")
		writeRoleOverlay(t, path, standinModel(provider))
		r.overlays[r.token(role)] = []string{operator, path}
	}
	spare := filepath.Join(dir, "spare.yml")
	writeRoleOverlay(t, spare, standinModel(lateSpareProvider))
	r.spareOverlays = []string{operator, spare}
}

// ---- the stand-in that calls the tool ----------------------------------------------------------------

// completionStandin is the model endpoint of one pane that a step can have call the `legion` tool:
// it answers the turn whose task carries the marker a step armed (arm) with one tool_use block
// calling `legion` with that call's input, held first while the call is held; it answers the
// next request, which carries the tool's result, with lateWaitingReply, handing that result to the
// call; and it answers every other turn, and the plugin's own side question, with text alone, as
// paneStandin does. A turn is matched on what starts it rather than on its count, because Oh My
// Pi's own requests and the reminders a settle earns are turns too. What starts it is what the
// request carries after its last assistant message (turnOf), since the host and the plugins may
// put messages of their own beside the task or the tool's result.
type completionStandin struct {
	*modelStandin

	mu sync.Mutex
	// armed is the calls no turn has made yet, in the order they were armed; made the ones a turn
	// has, which a tool_result names by id.
	armed, made []*paneCall
}

// paneCall is one call of the `legion` tool a stand-in makes: the marker of the task whose turn
// makes it, the input it carries, its tool_use id, the name the turn's request offered the tool
// under, the hold it waits on, and what the turn and the tool's result tell the step.
type paneCall struct {
	pane   *completionStandin
	marker string
	input  map[string]any
	id     string
	tool   string
	// held, when not nil, holds the turn's answer until release closes it.
	held    chan struct{}
	release func()
	// reached is closed when the turn that makes the call has asked for its answer, with task the
	// text of the message that started it.
	reached chan struct{}
	task    string
	// result receives the tool's answer once.
	result chan toolResult
}

// toolResult is the tool's answer as the pane sent it back: its text and whether the host marked
// it an error, which a refusal the tool throws is.
type toolResult struct {
	text    string
	isError bool
}

func newCompletionStandin(t *testing.T, name string) *completionStandin {
	t.Helper()
	s := &completionStandin{modelStandin: &modelStandin{name: name}}
	s.serve(t, s)
	return s
}

// arm has the next turn whose starting message carries marker, and that offers the `legion`
// tool, call it with input; held holds that turn's answer until the call's release.
func (s *completionStandin) arm(marker string, input map[string]any, held bool) *paneCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	call := &paneCall{
		pane: s, marker: marker, input: input, id: fmt.Sprintf("toolu_%s_%d", s.name, len(s.armed)+len(s.made)+1),
		release: func() {}, reached: make(chan struct{}), result: make(chan toolResult, 1),
	}
	if held {
		hold := make(chan struct{})
		var once sync.Once
		call.held, call.release = hold, func() { once.Do(func() { close(hold) }) }
	}
	s.armed = append(s.armed, call)
	return call
}

// awaitReached waits for the turn that makes the call, and answers the line of that turn's text
// that carries the marker: the task, among what the host and the plugins put beside it. A wait
// that runs out names the last request the stand-in was asked, which is what says why no turn
// matched.
func (c *paneCall) awaitReached(t *testing.T, within time.Duration) string {
	t.Helper()
	select {
	case <-c.reached:
		for line := range strings.SplitSeq(c.task, "\n") {
			if strings.Contains(line, c.marker) {
				return line
			}
		}
		return c.task
	case <-time.After(within):
		t.Fatalf("no turn whose task carries %q reached the stand-in %s within %s; %s", c.marker, c.pane.name, within, c.pane.lastRequest())
		return ""
	}
}

// lastRequest says what the stand-in was last asked: how many requests it has answered, and of
// the last, the tools it offered and the text and block types of its turn (turnOf).
func (s *completionStandin) lastRequest() string {
	s.modelStandin.mu.Lock()
	defer s.modelStandin.mu.Unlock()
	if len(s.requests) == 0 {
		return "it was asked nothing"
	}
	var request messagesRequest
	if err := json.Unmarshal([]byte(s.requests[len(s.requests)-1]), &request); err != nil {
		return fmt.Sprintf("it answered %d requests, the last of them no Messages request (%v)", len(s.requests), err)
	}
	tools := make([]string, 0, len(request.Tools))
	for _, tool := range request.Tools {
		tools = append(tools, tool.Name)
	}
	turn := turnOf(request)
	types := make([]string, 0, len(turn))
	for _, block := range turn {
		types = append(types, block.Type)
	}
	return fmt.Sprintf("it answered %d requests, the last offering tools %v with %d messages, whose turn holds blocks %v and the text %q",
		len(s.requests), tools, len(request.Messages), types, blocksText(turn))
}

// awaitResult waits for the tool's answer to the call.
func (c *paneCall) awaitResult(t *testing.T, within time.Duration) toolResult {
	t.Helper()
	select {
	case result := <-c.result:
		return result
	case <-time.After(within):
		t.Fatalf("the pane sent no result of its legion call %s within %s", c.id, within)
		return toolResult{}
	}
}

func (s *completionStandin) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, ok := s.take(w, req)
	if !ok {
		return
	}
	// Only a Messages request is a turn. Oh My Pi asks the same base for a token count of its own,
	// which is answered a count.
	if !strings.HasSuffix(req.URL.Path, "/v1/messages") {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"input_tokens":1000}`)
		return
	}
	if isSideQuestion(body) {
		streamAnthropicText(w, s.name, "PROCEEDING")
		return
	}
	var request messagesRequest
	if err := json.Unmarshal([]byte(body), &request); err != nil || len(request.Messages) == 0 {
		streamAnthropicText(w, s.name, lateWaitingReply)
		return
	}
	turn := turnOf(request)
	if call, result, answered := s.resultIn(turn); answered {
		select {
		case call.result <- result:
		default:
		}
		streamAnthropicText(w, s.name, lateWaitingReply)
		return
	}
	if call := s.callFor(request, turn); call != nil {
		close(call.reached)
		if call.held != nil {
			select {
			case <-call.held:
			case <-req.Context().Done():
				return
			}
		}
		streamAnthropicToolUse(w, s.name, call.id, call.tool, call.input)
		return
	}
	streamAnthropicText(w, s.name, lateWaitingReply)
}

// resultIn is the result of a call this stand-in made, when the turn's blocks carry one.
func (s *completionStandin) resultIn(turn []requestBlock) (*paneCall, toolResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, block := range turn {
		if block.Type != "tool_result" {
			continue
		}
		for _, call := range s.made {
			if call.id == block.ToolUseID {
				return call, toolResult{text: blocksText(contentBlocks(block.Content)), isError: block.IsError}, true
			}
		}
	}
	return nil, toolResult{}, false
}

// callFor takes the first armed call whose marker the turn's text carries, when the request offers
// the `legion` tool and the turn carries no tool's result, and records that text as the call's
// task and the name the tool is offered under as the one it calls. Oh My Pi offers each tool to
// an Anthropic Messages endpoint under a name of its own spelling, `_legion` on the build this
// proof was measured on, and maps the call back to the tool by that name, so the call names it as
// offered.
func (s *completionStandin) callFor(request messagesRequest, turn []requestBlock) *paneCall {
	at := slices.IndexFunc(request.Tools, func(tool offeredTool) bool { return strings.TrimLeft(tool.Name, "_") == "legion" })
	resumed := slices.ContainsFunc(turn, func(block requestBlock) bool { return block.Type == "tool_result" })
	if at < 0 || resumed {
		return nil
	}
	text := blocksText(turn)
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, call := range s.armed {
		if strings.Contains(text, call.marker) {
			s.armed = slices.Delete(s.armed, i, i+1)
			s.made = append(s.made, call)
			call.task, call.tool = text, request.Tools[at].Name
			return call
		}
	}
	return nil
}

// messagesRequest is the part of an Anthropic Messages request the stand-in reads: the tools it
// offers and its messages.
type messagesRequest struct {
	Tools    []offeredTool `json:"tools"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

// offeredTool is one tool a Messages request offers the model, by name.
type offeredTool struct {
	Name string `json:"name"`
}

// turnOf is the blocks of every message after the request's last assistant message: the task, or
// the tool's result, that asks for this answer, with whatever the host or a plugin put beside it.
func turnOf(request messagesRequest) []requestBlock {
	start := 0
	for i, message := range request.Messages {
		if message.Role == "assistant" {
			start = i + 1
		}
	}
	var turn []requestBlock
	for _, message := range request.Messages[start:] {
		turn = append(turn, contentBlocks(message.Content)...)
	}
	return turn
}

// requestBlock is one content block of a message: text, or the result of a tool call.
type requestBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// contentBlocks is a message's content as blocks: content that is a string is one text block.
func contentBlocks(raw json.RawMessage) []requestBlock {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []requestBlock{{Type: "text", Text: text}}
	}
	var blocks []requestBlock
	_ = json.Unmarshal(raw, &blocks)
	return blocks
}

// blocksText is the text of a message's text blocks, one per line.
func blocksText(blocks []requestBlock) string {
	var texts []string
	for _, block := range blocks {
		if block.Type == "text" {
			texts = append(texts, block.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// streamAnthropicToolUse answers one Anthropic Messages request with a single call of the tool
// name, whose id is id and whose input is input, as the plugins' own pane harness scripts one
// (packages/pi-shared/test/omp-harness.ts, messageStream's tool_use arm): the block opens with an
// empty input, one input_json_delta carries the whole input, and the message stops for tool_use.
func streamAnthropicToolUse(w http.ResponseWriter, provider, id, name string, input map[string]any) {
	arguments, _ := json.Marshal(input)
	block, _ := json.Marshal(map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}})
	delta, _ := json.Marshal(map[string]string{"type": "input_json_delta", "partial_json": string(arguments)})
	streamAnthropicBlock(w, provider, "tool_use", string(block), string(delta))
}
