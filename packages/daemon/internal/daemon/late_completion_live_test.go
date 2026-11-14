//go:build e2e

// The live proof that a completion the `legion` tool sends after the issue moved on is answered
// for the phase the role left (dispatch://INFRA-3146): a reviewer whose own approval moved the
// issue to accepting before it completed has its completion taken, naming its own review record,
// and a finished tester that completes again, naming its own test record, is refused
// `409 HANDOFF_NOT_CURRENT_PHASE` in one request. scripts/e2e/late-completion.sh builds this
// driver with `-tags e2e` and owns the nats-server and the PostgreSQL it runs against.
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
// handoff_records_live_test.go runs it (startHandoffDispatch), and the daemon and both panes are
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
// the status and code it was answered with), and GET /legion/v1/state. GitHub is a stand-in of
// this driver's own (serveGitHub) that answers the pull request at its head, mergeable and
// clean, with no reviews; the review that moves reviewing on is published as the GitHub envelope
// Envoy relays, written as the review App submits one: approved, by the App's bot login, with
// the Legion footer naming the reviewer's own session and the head it read. The two Apps' tokens
// are workflowTokenRecorder's placeholders, and Envoy's listener is the package's noticeListener,
// with each pane registering at a listener of its own (newPaneListener).
//
// The seed is one issue in testing, on an open pull request at a head of the run's own, with its
// tester's claim mid-launch holding the task its phase's start left pending; the tester's pass
// moves the issue to reviewing, whose start the daemon makes itself, so the reviewer it launches
// is a claim the daemon started on the issue, which the engine's leftItsPhase requires of a late
// completion. The panes' working directory is a jj repository, as a pane's workspace is, so a
// tool that reads the commit its workspace stands on (`@-`) reads it there: this tree's tool
// reads none for either role, and the negative control's, which names no record of a phase it
// left, reads it for both.
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
// talks to a stand-in that only ever answers text, never to the tester's or the reviewer's.
const (
	lateTesterProvider   = "testerpane"
	lateReviewerProvider = "reviewerpane"
	lateSpareProvider    = "sparepane"
)

// lateCompletionAnswered is the completion route's one line per answer (api's handoffComplete).
const lateCompletionAnswered = "api: handoff completion answered"

// lateWaitingReply is the text a stand-in answers every turn it makes no call in, the tool's
// result among them: a WAITING line, which the phase-stall reminder takes as a worker that said
// what it waits on and answers with its quiet wait rather than another turn
// (packages/pi-shared/src/phase-stall.ts, WAITING_REPLY).
const lateWaitingReply = "WAITING on the daemon's next task: the legion tool has answered."

// lateRepeatTask is the operator's task that starts the finished tester's second turn, and
// lateRepeatMarker the words of it that turn's call is armed on.
const (
	lateRepeatMarker = "report the testing completion again"
	lateRepeatTask   = "LATE-COMPLETION: " + lateRepeatMarker + " with the legion tool."
)

// lateCompletionLive is one run of the proof: the booted daemon over the real Dispatch, the one
// issue, the two panes' stand-ins, and what steps 2 and 3 recorded for step 4 to judge.
type lateCompletionLive struct {
	liveAgentProof
	dispatch *handoffDispatch
	tokens   *workflowTokenRecorder
	// key is the proof's one issue, as Dispatch keyed it; title its title; number its pull
	// request's; and head that pull request's head, which every record names.
	key, title, head string
	number           int
	// github is the URL of GitHub's stand-in (serveGitHub).
	github string
	// panes is each driven claim's model stand-in, and spare the one every other claim talks to.
	panes map[claim.Token]*completionStandin
	spare *paneStandin
	// home and profile are the Oh My Pi home and profile the panes run under; overlays is each
	// driven claim's PI_CONFIG_FILES, and spareOverlays every other claim's.
	home, profile string
	overlays      map[claim.Token][]string
	spareOverlays []string
	// testerCall and reviewerCall are the calls armed before the boot, on the task each pane's
	// first turn is started with.
	testerCall, reviewerCall *paneCall
	// testRecord and reviewRecord are the records steps 1 and 3 wrote as each pane's own session.
	testRecord, reviewRecord handoffRecord
	// repeat and late are what the finished tester's repeat (step 2) and the reviewer's late
	// completion (step 3) were answered, which step 4 judges.
	repeat, late lateAnswer
}

// lateAnswer is one completion as a step records it: what the tool answered the agent, and the
// daemon's own line for the one request that call made.
type lateAnswer struct {
	result toolResult
	line   loggedRecord
}

// TestLateCompletionsThroughTheRealLegionToolLive boots the daemon over the real Dispatch and two
// real panes, has the tester complete testing, has the finished tester call handoff_complete
// again, and has the reviewer complete after its own approval moved the issue to accepting, each
// call made by the `legion` tool the panes load; then judges the two late answers.
func TestLateCompletionsThroughTheRealLegionToolLive(t *testing.T) {
	fixtureenv.Live(t)
	r := newLateCompletionLive(t)
	r.step("step1-the-tester-pane-completes-testing-through-the-real-tool-naming-its-own-test-record", r.testerCompletes)
	r.step("step2-the-finished-tester-calls-handoff-complete-again-and-one-request-reaches-the-daemon", r.finishedTesterRepeats)
	r.step("step3-the-reviewer-pane-completes-after-its-own-approval-moved-the-issue-to-accepting", r.reviewerCompletesLate)
	r.step("step4-the-repeat-is-refused-not-current-phase-and-the-late-completion-is-taken-naming-the-review-record", r.eachAnswerIsTheBranchs)
}

// newLateCompletionLive readies everything the panes need before the daemon boots: the real
// Dispatch with the project, its repository mapping and the issue; the `legion` binary and the
// two plugins built from this checkout; an Oh My Pi profile routed to the stand-ins; the panes'
// jj workspace; the store seeded; and the daemon booted over the local runtime.
func newLateCompletionLive(t *testing.T) *lateCompletionLive {
	t.Helper()
	omp := os.Getenv(liveOmpPathEnv)
	if omp == "" {
		t.Skipf("%s is unset, so there is no Oh My Pi for a pane to run; scripts/e2e/late-completion.sh requires it", liveOmpPathEnv)
	}
	for _, tool := range []string{"bun", "jj"} {
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
		title:          "a reviewer completes after its own approval moved the issue on",
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
	r.head = headCommit(r.key, "pull request head")

	work := t.TempDir()
	r.legion = buildProofLegion(t, root, work)
	r.home = filepath.Join(work, "omp-home")
	r.profile = "legion-late-completion-" + strings.ToLower(r.cfg.Project)
	installProofPlugins(t, root, work, r.home, r.profile, omp)
	r.panes[r.token(claim.RoleTester)] = newCompletionStandin(t, lateTesterProvider)
	r.panes[r.token(claim.RoleReviewer)] = newCompletionStandin(t, lateReviewerProvider)
	r.spare = newPaneStandin(t, lateSpareProvider)
	r.writeOmpProfile()
	r.writeOverlays(work)
	r.github = r.serveGitHub()

	r.rt = newLocalRuntime(t, r.cfg.StateDir, r.legion, omp, r.home, r.profile)
	r.rt.daemonURL, r.rt.envoyURL, r.rt.natsURLs = r.cfg.DaemonURL, newPaneListener(t), r.cfg.NatsURLs
	r.rt.dispatchURL, r.rt.dispatchTokenFile = r.cfg.DispatchURL, r.cfg.DispatchTokenFile
	r.rt.overlays = func(token claim.Token) []string {
		if overlays, driven := r.overlays[token]; driven {
			return overlays
		}
		return r.spareOverlays
	}
	r.initWorkspace()

	// Each pane's first turn is armed before the boot, since the task that starts it is sent the
	// moment the pane reports ready. The tester's is held until step 1 has written the record it
	// reads; the reviewer's until step 3's approval has moved the issue to accepting.
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

// initWorkspace makes the panes' working directory a jj repository with jj's own `git init`, run
// under the panes' HOME, so the jj a pane's tool runs there answers the commit `@-` names.
func (r *lateCompletionLive) initWorkspace() {
	t := r.t
	t.Helper()
	dir := r.rt.workDir(r.key)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("make the panes' working directory %s: %v", dir, err)
	}
	create := exec.Command("jj", "git", "init", dir)
	create.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + r.home}
	if out, err := create.CombinedOutput(); err != nil {
		t.Fatalf("jj git init %s: %v\n%s", dir, err, out)
	}
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
		githubAPI:      r.github,
	}
}

// seed records the issue as the daemon would hold it at the moment its tester began: a root
// handed to Legion, in testing, on an open pull request at the run's head that GitHub would merge
// (mergeable and clean, so the approval of step 3 moves the issue on rather than waiting for
// GitHub's answer), holding its tree's admission slot with its lifecycle open, and its tester's
// claim mid-launch, which this boot relaunches. The claim holds the task its phase's start left
// pending, written as the daemon writes a phase's task line, which is what admission's repair of
// a slotted worker passes over. Both panes' role prompt is an operator's short prompt file
// (writePrompt): nothing here reads a prompt.
func (r *lateCompletionLive) seed() {
	t := r.t
	t.Helper()
	ctx := context.Background()
	listed := r.dispatch.issue(r.key)
	if err := r.st.Tx(ctx, func(tx pgx.Tx) error {
		if err := r.records.PutIssue(ctx, tx, record.Issue{
			Key: r.key, Repo: liveRepo, DefaultBranch: "main", GitHubRepo: liveRepo, Tree: r.key, Project: r.cfg.Project,
			Title: listed.Title, Phase: phase.Testing, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress",
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
	tester := r.token(claim.RoleTester)
	if _, err := r.st.AdmitClaim(ctx, supervise.Claim{
		Token: tester, Project: r.project, Tree: r.key, Issue: r.key, Role: claim.RoleTester,
		Generation: 1, State: supervise.StateLaunching,
		BootTokenHash: supervise.HashBootToken("interrupted-" + randomSuffix(t)), ServingGeneration: 1,
	}); err != nil {
		t.Fatalf("record the tester's unfinished launch: %v", err)
	}
	task := fmt.Sprintf("Continue %s. Issue: %s. Phase: testing. Pull request: #%d.", listed.Title, r.key, r.number)
	if err := r.st.PutDelivery(ctx, tester, supervise.Delivery{
		ID: "seeded-start-" + randomSuffix(t), Task: task, Generation: 1, Phase: phase.Testing, QueuedAt: time.Now(),
	}); err != nil {
		t.Fatalf("record the task the tester's start left pending: %v", err)
	}
	for _, role := range []claim.Role{claim.RoleTester, claim.RoleReviewer} {
		writePrompt(t, r.cfg, r.token(role))
	}
	r.note("seeded %s in %s on pull request #%d at head %s (mergeable, clean), with its tester mid-launch holding %q",
		r.key, phase.Testing, r.number, r.head, task)
}

// ---- the steps -----------------------------------------------------------------------------------

// Step 1. The tester's pane takes its testing task; the driver writes the tester's test record as
// that pane's own session; the stand-in's held turn calls handoff_complete with verdict pass; the
// completion is taken, naming that record, and the issue moves to reviewing. The tool and the
// daemon answer it alike on this tree and the negative control's: the tester works the issue's
// phase.
func (r *lateCompletionLive) testerCompletes() {
	t := r.t
	tester := r.token(claim.RoleTester)
	r.awaitReady(tester)
	task := r.testerCall.awaitReached(t, time.Minute)
	r.note("the tester's pane is in the turn its testing task began, held at the stand-in: %q", task)
	session := r.claim(tester).Session
	r.testRecord = r.writeRecord(session, "test")
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

// Step 2. The finished tester, whose testing phase already holds the completion of step 1, is
// given a second turn by the operator's own task (`legion claims deliver`), and the stand-in's
// turn calls handoff_complete again with verdict pass. The step records the tool's answer and the
// daemon's line for it, and holds that the call made one request: the daemon wrote exactly one
// line for the tester's claim from the delivery until a settle after the tool answered, so the
// plugin's retry, which runs only while the daemon gives no answer, sent nothing more. Step 4
// judges the answer.
func (r *lateCompletionLive) finishedTesterRepeats() {
	t := r.t
	tester := r.token(claim.RoleTester)
	r.await("the tester's turn of step 1 to end", time.Minute, func() bool {
		state := r.claim(tester).State
		return state == string(supervise.StateIdle) || state == string(supervise.StateReady)
	})
	call := r.panes[tester].arm(lateRepeatMarker,
		map[string]any{"op": "handoff_complete", "summary": "Testing is done; reporting it again.", "verdict": "pass"}, false)
	mark := time.Now()
	r.note("legion claims deliver --claim %s --task %q:\n%s", tester, lateRepeatTask,
		indentOutput(r.legionClaims("deliver", "--claim", string(tester), "--task", lateRepeatTask)))
	task := call.awaitReached(t, 2*time.Minute)
	r.note("the tester's pane is in the turn the operator's task began: %q", task)
	result := call.awaitResult(t, 2*time.Minute)
	r.note("the stand-in's turn called legion %s; the tool answered (error %t): %s", inputOf(call), result.isError, result.text)
	line := r.settledAnswer("the finished tester's repeat", mark, tester)
	r.note("daemon: %s", answeredLine(line))
	r.note("read back: the daemon answered the tester's claim once between the delivery and %s after the tool's answer", liveSettle)
	r.repeat = lateAnswer{result: result, line: line}
}

// Step 3. The reviewer the tester's pass started takes its reviewing task, and its turn is held
// at the stand-in. Meanwhile the driver writes the reviewer's review record as that pane's own
// session and publishes the review App's approval of the head, its footer naming the reviewer's
// session and that head, as Envoy relays a GitHub review; the daemon moves the issue to
// accepting. Then the turn goes on and calls handoff_complete. The step records the tool's answer
// and the daemon's one line for it, and holds that the issue stays in accepting. Step 4 judges
// the answer.
func (r *lateCompletionLive) reviewerCompletesLate() {
	t := r.t
	reviewer := r.token(claim.RoleReviewer)
	r.await("the daemon to start the reviewer", 2*time.Minute, func() bool {
		return slices.ContainsFunc(r.d.claims(), func(c api.OperatorClaim) bool { return c.Token == reviewer })
	})
	// The reviewer's task is sent the moment its pane reports ready, and its turn is held at the
	// stand-in, so the claim goes from registered to working with no ready state a poll could
	// read: the held turn reaching the stand-in is what says the pane registered and took its task.
	task := r.reviewerCall.awaitReached(t, paneReady)
	held := r.claim(reviewer)
	r.note("%s is %s in session %s, plugin contract %d, process %s", reviewer, held.State, held.Session, held.PluginContract, held.Locator.Label())
	r.note("the reviewer's pane is in the turn its reviewing task began, held at the stand-in: %q", task)
	session := held.Session
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
	r.late = lateAnswer{result: result, line: line}
}

// Step 4. The two late answers are this branch's: the finished tester's repeat names its own test
// record and is refused `409 HANDOFF_NOT_CURRENT_PHASE`, the refusal the tool passes to the
// agent; and the reviewer's late completion names its own review record and is taken, which
// GET /legion/v1/state shows as that record on the reviewer's phases row. Every mismatch is named
// in one failure, so a negative control's run reads both answers.
func (r *lateCompletionLive) eachAnswerIsTheBranchs() {
	t := r.t
	var wrong []string
	repeat := r.repeat.line.Attrs
	if repeat["status"] != float64(http.StatusConflict) || repeat["code"] != "HANDOFF_NOT_CURRENT_PHASE" ||
		repeat["handoffId"] != r.testRecord.ID || repeat["handoffVersion"] != float64(r.testRecord.Version) ||
		!r.repeat.result.isError || !strings.Contains(r.repeat.result.text, "409 HANDOFF_NOT_CURRENT_PHASE") {
		wrong = append(wrong, fmt.Sprintf("the finished tester's repeat was answered %v %v naming record %q version %v, and its tool answered %q; want 409 HANDOFF_NOT_CURRENT_PHASE naming its own test record %s version %d",
			repeat["status"], repeat["code"], repeat["handoffId"], repeat["handoffVersion"], r.repeat.result.text, r.testRecord.ID, r.testRecord.Version))
	}
	late := r.late.line.Attrs
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
	r.note("the finished tester's repeat: 409 HANDOFF_NOT_CURRENT_PHASE, naming its own test record %s (version %d), in one request", r.testRecord.ID, r.testRecord.Version)
	r.note("the reviewer's late completion: 200, naming its own review record %s (version %d), in one request", r.reviewRecord.ID, r.reviewRecord.Version)
	r.note("read back: GET /legion/v1/state shows the reviewer's handoffRecord %s version %d", held.ID, held.Version)
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

// settledAnswer is the daemon's one line for the completion a step's call made: it waits for the
// first line for token's claim written after mark, then watches liveSettle longer, and fails
// unless exactly one such line was written, which is one request for the call.
func (r *lateCompletionLive) settledAnswer(what string, mark time.Time, token claim.Token) loggedRecord {
	r.t.Helper()
	answered := func() []loggedRecord {
		return r.logs(func(l loggedRecord) bool {
			return l.Message == lateCompletionAnswered && l.Attrs["claim"] == string(token) && l.at.After(mark)
		})
	}
	r.await("the daemon's line for "+what, time.Minute, func() bool { return len(answered()) > 0 })
	time.Sleep(liveSettle)
	lines := answered()
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

// serveGitHub starts GitHub's REST API for acme/widgets as this proof's daemon reads it, and
// answers its URL: the pull request at the run's head, open, mergeable and clean on base main;
// its reviews as none, since the one review the proof reads is the envelope step 3 publishes;
// any repository as its name and default branch, which a launch reads; and a GraphQL root that
// knows no pull request. Every other read is a 404, which the daemon's own reads take as GitHub
// being unreadable, so no read of this proof reaches github.com.
func (r *lateCompletionLive) serveGitHub() string {
	return githubtest.Server(r.t, githubtest.Spec{Handle: func(w http.ResponseWriter, req *http.Request) {
		write := func(value any) {
			if err := json.NewEncoder(w).Encode(value); err != nil {
				r.t.Errorf("write the GitHub stand-in's answer: %v", err)
			}
		}
		if req.Method == http.MethodPost && req.URL.Path == "/graphql" {
			_, _ = io.WriteString(w, `{"data":{"repository":{"pullRequest":null}}}`)
			return
		}
		rest, ours := strings.CutPrefix(req.URL.Path, "/repos/"+liveRepo)
		other, isRepository := strings.CutPrefix(req.URL.Path, "/repos/")
		switch {
		case req.Method != http.MethodGet:
		case ours && rest == "":
			write(map[string]any{"full_name": liveRepo, "default_branch": "main"})
			return
		case ours && rest == fmt.Sprintf("/pulls/%d", r.number):
			write(map[string]any{
				"number": r.number, "state": "open", "mergeable": true, "mergeable_state": "clean",
				"base": map[string]string{"ref": "main"},
				"head": map[string]string{"ref": "legion/" + r.key, "sha": r.head},
			})
			return
		case ours && rest == fmt.Sprintf("/pulls/%d/reviews", r.number):
			write([]any{})
			return
		case !ours && isRepository && len(strings.Split(other, "/")) == 2:
			write(map[string]any{"full_name": other, "default_branch": "main"})
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	}})
}

// ---- the Oh My Pi profile and the overlays -----------------------------------------------------------

// writeOmpProfile writes the profile the panes share: the three stand-in providers, one model
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
		r.panes[r.token(claim.RoleTester)].modelStandin, r.panes[r.token(claim.RoleReviewer)].modelStandin, r.spare.modelStandin,
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
// serves (writeRoleOverlay): the tester's and the reviewer's own, and the spare's for every other
// claim.
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
	for role, provider := range map[claim.Role]string{claim.RoleTester: lateTesterProvider, claim.RoleReviewer: lateReviewerProvider} {
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
