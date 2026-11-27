//go:build e2e

// The live proof that the tester's move is decided by the check results in Dispatch alone: the
// tester records its results with the real `dispatch result`, its real `legion` tool completes
// testing with no verdict, Dispatch publishes each change of a check's state as a
// check.state_changed event, and the daemon moves the issue to reviewing when every check reads
// yes at the head the tester's test record names, or back to implementing with a task that names
// every check that does not pass. scripts/e2e/check-states.sh builds this driver with `-tags e2e`
// and owns the nats-server and the PostgreSQL it runs against. This file holds the run, its rig and
// its stand-ins; the cases are in check_states_live_cases_test.go, what a case drives in
// check_states_live_drive_test.go, what it reads of the daemon in check_states_live_reads_test.go,
// Dispatch's own records in check_states_live_records_test.go, and the broker's stop and continue
// in check_states_live_broker_test.go.
//
// The unit tests hold each half: Dispatch's own tests (packages/envoy/internal/dispatch/api's
// check_states_publish_test.go and check_states_settle_test.go) that a write publishes the state
// it changes, the intake's that the event decodes, and the workflow's that the move reads the
// states at the tested head. None of them runs the real Dispatch, the real daemon and a real
// tester's pane against each other, so none says that what a tester's pane does moves the issue
// so. This driver does.
//
// Nothing between the tester and the move is stood in for. Dispatch is the real server this
// checkout builds, on a database of its own, as handoff_records_live_test.go runs it, but with its
// events on: it publishes them on the run's own broker, LEGION_E2E_NATS_URL, where the daemon's
// durable consumer of Dispatch's issue events takes them, as in a deployment. The daemon is the
// whole daemon (run, through startDaemonWithin), booted with dispatch_url at that server. Each
// tester is a real `omp --mode rpc` under the real `legion worker-shim`, started by
// localruntime_live_test.go's runtime with this checkout's `@sjawhar/pi-envoy` and
// `@sjawhar/pi-legion` in an Oh My Pi profile of the run's own, and its model is
// late_completion_live_test.go's completionStandin, which answers the turn a case arms with one
// `tool_use` block calling the `legion` tool's handoff_complete, so the completion the daemon
// answers is the request the shipped plugin builds. Every result and every test record is written
// with the real `dispatch` CLI (packages/envoy-client/bin/dispatch.ts under bun) as the session
// the tester's pane registered, as that pane's own shell runs it.
//
// What is stood in for lies outside the move. The open pull request of each issue is staged in
// Dispatch's database as the `pull_request` envelope Envoy relays would have stored it (one
// pull_request_heads row, attributed to the issue and read just now), since the scratch server
// reads no GitHub; the trigger on that table marks the issue owed, as the envelope's write does.
// GitHub's REST API, as the daemon reads it, is a stand-in of this driver's own for those pull
// requests. Envoy's listener is the package's noticeListener, the Apps' tokens are
// workflowTokenRecorder's placeholders, and every pane but a tester's talks to a model stand-in
// that answers text alone. The issues, their pull requests, their trees' slots and each tester's
// claim mid-launch are seeded through the store, as every daemon test seeds its fixture.
//
// The cases are chosen with CHECK_STATES_E2E_CASES (a comma- or space-separated list; every case
// when it is empty), and each runs on issues of its own, in the order statesCaseOrder gives. Each
// prints `== <case>`, what it drove and read, and `ok <case>`; the first that fails ends the run,
// its failure naming the case and the assertion (`FAIL <case> at <assertion>: ...`). A read that
// finds nothing it could judge says UNKNOWN and what it could not read, and its case fails. Each
// bound of 10 seconds is measured from the operation that triggers the answer, never from the
// read, and an answer that comes later than the bound fails at `<assertion>-within`.
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
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

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

// The model stand-ins, by the provider name each is given in the run's models.yml: the testers'
// one, which calls the `legion` tool where a case arms it; the implementers' one, which a case
// holds while it reads the task the daemon queued for the implementer; and the spare, which every
// other pane talks to.
const (
	statesTesterProvider      = "statestester"
	statesImplementerProvider = "statesimplementer"
	statesSpareProvider       = "statesspare"
)

// statesCasesEnv names the cases a run makes; empty, it makes every one.
const statesCasesEnv = "CHECK_STATES_E2E_CASES"

// statesBrokerPIDEnv names the pid of the run's own nats-server: the variable
// scripts/e2e/lib/live-daemon-proof.sh holds it in, which scripts/e2e/check-states.sh exports
// before the run starts, so the driver stops exactly the broker the run started and no other.
const statesBrokerPIDEnv = "nats_pid"

// statesHeld is the workflow's line for each state event it takes (workflow's checkState).
const statesHeld = "workflow: a check state is held"

// statesWithin is the bound the approved checks give the daemon's answer: the line after a result,
// the move and the reviewer's start after the tester's completion, and the move after the broker
// comes back. It is measured from the operation that triggers the answer: the start of the
// result's write, the moment complete takes while the stand-in holds the answer that makes the
// tool's call (so before the tool's request), or the SIGCONT.
const statesWithin = 10 * time.Second

// statesLate is how long a case reads on for an answer past statesWithin, so an answer that comes
// late fails at the bound, naming how late it came, apart from one that never comes.
const statesLate = time.Minute

// statesCompletion, statesWrite and statesContinue name the operations a bound is measured from.
const (
	statesCompletion = "the tester's completion"
	statesWrite      = "the start of the result's write"
	statesContinue   = "the SIGCONT"
)

// statesVerification is the Verification document of every issue but the no-check case's: two
// `before_merge` checks, k1 and k2, that a role proves by its own result.
const statesVerification = `:::check{#k1 when="before_merge" negative_control="A server whose route records nothing."}
The server records a result against a check.
:::

:::check{#k2 when="before_merge" negative_control="A server whose read answers nothing."}
The server answers a read of the results.
:::
`

// statesNoBeforeMerge is the no-check case's Verification document: a document that exists and
// holds a check, k3, which is proved after the merge, so it holds no `before_merge` check. Dispatch
// reads its blocks and keeps none of them in the head's set. An issue with no document at all
// would not reach that path: Dispatch answers it before it reads a block.
const statesNoBeforeMerge = `Nothing of this issue is proved before its merge.

:::check{#k3 when="after_merge" negative_control="A released server whose route records nothing."}
The released server records a result against a check.
:::
`

// statesVerificationCI is the Verification document of ci-failure-tells: k1 and k2 as
// statesVerification has them, and k3, a `before_merge` check whose ci_check names the CI job
// acme-unit, so Dispatch takes its state from that job's run at the head (statesCIJob).
const statesVerificationCI = statesVerification + `
:::check{#k3 when="before_merge" ci_check="acme-unit" negative_control="A build whose unit tests fail."}
The unit tests pass at the head.
:::
`

// statesCIJob is the CI job k3 of ci-failure-tells names, as the stand-in of GitHub that the run's
// Dispatch reads answers its run at that issue's head: completed, failure.
const statesCIJob = "acme-unit"

// statesCaseOrder is every case, in the order a run makes them. completion-before-event is last,
// since it stops the broker every other case's events cross.
var statesCaseOrder = []string{
	"one-record", "daemon-records-nothing", "dispatch-tells", "pass-goes-to-review", "no-check",
	"fail-sends-back", "ci-failure-tells", "completion-before-event",
}

// statesCaseIssues is the issues each case drives, by the title each is filed under, and whether
// its Verification document holds the two before_merge checks k1 and k2 (statesVerification) or
// no before_merge check (statesNoBeforeMerge).
var statesCaseIssues = map[string][]statesIssueSpec{
	"one-record": {
		{title: "One record decides: the tester's fail of k2 sends the issue back", beforeMerge: true},
		{title: "One record decides: the tester's passes send the issue to review", beforeMerge: true},
	},
	"daemon-records-nothing": {
		{title: "The daemon records nothing: a round that fails k2", beforeMerge: true},
		{title: "The daemon records nothing: a round that passes both checks", beforeMerge: true},
	},
	"dispatch-tells":      {{title: "Dispatch tells the daemon of a fail of k1", beforeMerge: true}},
	"pass-goes-to-review": {{title: "Every check passes, recorded by the implementer and the tester", beforeMerge: true}},
	"no-check":            {{title: "No before_merge check in the Verification document", beforeMerge: false}},
	"fail-sends-back": {
		{title: "A fail of k2 is named in the implementer's task", beforeMerge: true},
		{title: "A pass and a fail of k1 are named in the implementer's task", beforeMerge: true},
	},
	"ci-failure-tells":        {{title: "A failed CI job of k3 reaches the implementer alone", beforeMerge: true, ciJob: true}},
	"completion-before-event": {{title: "The completion arrives before Dispatch's events", beforeMerge: true}},
}

// statesIssueSpec is one issue a case drives, as statesCaseIssues writes it.
type statesIssueSpec struct {
	title       string
	beforeMerge bool
	// ciJob is whether the issue's document is statesVerificationCI, whose k3 a CI job runs.
	ciJob bool
}

// statesIssue is one issue of the run: its key as Dispatch filed it, its title, its pull request's
// number and head, and the session its tester's pane registered.
type statesIssue struct {
	key, title, head string
	number           int
	tester           string
	// seededTask is the task seed left pending on the tester's claim, the test's own input, which
	// readyTester finds the tester's first turn by: no word of it is the daemon's.
	seededTask string
	// staged is whether the issue's pull request is staged in Dispatch's database (stageHead).
	staged bool
	// recorded is each result the case recorded on the issue, as statesRecordedAs spells it.
	recorded []string
}

// statesProof is one run of the proof: the booted daemon over the real Dispatch, the issues of the
// cases it makes, the stand-ins the panes talk to, and what the case being made has found it could
// not read.
type statesProof struct {
	liveAgentProof
	dispatch *handoffDispatch
	// dispatchDB is Dispatch's own database, which a case stages pull requests in and reads the
	// results and the states from.
	dispatchDB *pgxpool.Pool
	tokens     *workflowTokenRecorder
	cases      []string
	issues     map[string][]*statesIssue
	// root is the checkout the CLI runs from, and cliHome the home, configuration and session
	// state of every CLI run.
	root, cliHome string
	// github is the stand-in of GitHub's REST API (serveGitHub).
	github string
	// tester is the stand-in every tester's pane talks to, implementer every implementer's, and
	// spare every other pane's.
	tester      *completionStandin
	implementer *paneStandin
	spare       *paneStandin
	// home and profile are the Oh My Pi home and profile the panes run under; overlays is each
	// role's PI_CONFIG_FILES.
	home, profile string
	overlays      map[claim.Role][]string
	// current is the case being made, and blind what its reads could not see, which fails the
	// case once it has made its own judgement.
	current string
	blind   []string
	// stopped is the pid the case completion-before-event stopped, 0 while none is stopped.
	stopped int
	// ciFailedHead is the head at which the stand-in of GitHub the run's Dispatch reads answers
	// statesCIJob completed, failure: ci-failure-tells's issue's, set once it is filed; "" before.
	ciFailedHead atomic.Pointer[string]
}

// TestCheckStatesLive makes each chosen case, in statesCaseOrder. Its name is short on purpose: the
// daemon's worker socket is made under the test's own temporary directory, which carries the name,
// and a unix socket's path holds 107 bytes.
func TestCheckStatesLive(t *testing.T) {
	fixtureenv.Live(t)
	r := newStatesProof(t)
	bodies := map[string]func(){
		"one-record":              r.oneRecord,
		"daemon-records-nothing":  r.daemonRecordsNothing,
		"dispatch-tells":          r.dispatchTells,
		"pass-goes-to-review":     r.passGoesToReview,
		"no-check":                r.noCheck,
		"fail-sends-back":         r.failSendsBack,
		"ci-failure-tells":        r.ciFailureTells,
		"completion-before-event": r.completionBeforeEvent,
	}
	for _, name := range r.cases {
		r.current, r.blind = name, nil
		r.step(name, func() {
			bodies[name]()
			if len(r.blind) > 0 {
				r.fail("unknown", "UNKNOWN: %s", strings.Join(r.blind, "; "))
			}
		})
	}
}

// statesSelected is the cases CHECK_STATES_E2E_CASES names, in statesCaseOrder, and every case when
// it names none. A name that is no case fails the run before anything is built, naming the cases.
func statesSelected(t *testing.T) []string {
	t.Helper()
	named := strings.FieldsFunc(os.Getenv(statesCasesEnv), func(r rune) bool { return r == ',' || r == ' ' })
	if len(named) == 0 {
		return slices.Clone(statesCaseOrder)
	}
	for _, name := range named {
		if !slices.Contains(statesCaseOrder, name) {
			t.Fatalf("%s names %q, which is no case of this proof; the cases are %s", statesCasesEnv, name, strings.Join(statesCaseOrder, ", "))
		}
	}
	var chosen []string
	for _, name := range statesCaseOrder {
		if slices.Contains(named, name) {
			chosen = append(chosen, name)
		}
	}
	return chosen
}

// newStatesProof readies everything the panes need before the daemon boots: the real Dispatch with
// its events on the run's broker, the project, its repository mapping and every chosen case's
// issues; the `legion` binary and the two plugins built from this checkout; an Oh My Pi profile
// routed to the stand-ins; GitHub's stand-in; the store seeded; and the daemon booted over the
// local runtime.
func newStatesProof(t *testing.T) *statesProof {
	t.Helper()
	omp := os.Getenv(liveOmpPathEnv)
	if omp == "" {
		t.Skipf("%s is unset, so there is no Oh My Pi for a pane to run; scripts/e2e/check-states.sh requires it", liveOmpPathEnv)
	}
	for _, tool := range []string{"bun", "jj", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is not on PATH: %v", tool, err)
		}
	}
	cases := statesSelected(t)
	proof := newLiveProof(t, "scripts/e2e/check-states.sh")
	// A Dispatch project key is at most ten characters (contracts ProjectKeyPattern), and the
	// daemon's project is its Dispatch project, so the run's project is a short one of its own.
	proof.cfg.Project = "CS" + randomSuffix(t)[:6]
	proof.cfg.Projects = map[string]config.Project{proof.cfg.Project: {}}
	// The loader's own default (config's resolve), which workflowConfig leaves at zero: a pane's
	// tool reads GET /legion/v1/state through the daemon API contract, which takes a positive cap
	// alone.
	proof.cfg.ReviewRoundCap = 3
	// Each issue is a tree of its own and takes a slot; ten is the most a run seeds.
	proof.cfg.AdmissionCap = 16
	var err error
	if proof.project, err = claim.ProjectToken(proof.cfg.Project); err != nil {
		t.Fatal(err)
	}
	r := &statesProof{
		liveAgentProof: liveAgentProof{liveProof: proof, omp: omp},
		tokens:         &workflowTokenRecorder{},
		cases:          cases,
		issues:         map[string][]*statesIssue{},
		cliHome:        t.TempDir(),
	}
	version, err := exec.Command(omp, "--version").Output()
	if err != nil {
		t.Fatalf("read the Oh My Pi version of %s: %v", omp, err)
	}
	r.note("Oh My Pi %s at %s; cases %s", strings.TrimSpace(string(version)), omp, strings.Join(cases, ", "))

	r.root = proofCheckoutRoot(t)
	// Only ci-failure-tells reads CI, so only a run that makes it gives Dispatch a GitHub App and
	// the stand-in of GitHub it calls (dispatchGitHub); every other case reads no CI run.
	var dispatchEnv []string
	if slices.Contains(cases, "ci-failure-tells") {
		dispatchEnv = r.dispatchGitHub()
	}
	r.dispatch = startStatesDispatch(t, r.root, os.Getenv(liveNATSURLEnv), dispatchEnv)
	if err := os.WriteFile(r.cfg.DispatchTokenFile, []byte(r.dispatch.token+"\n"), 0o600); err != nil {
		t.Fatalf("write the Dispatch token file: %v", err)
	}
	r.cfg.DispatchURL = r.dispatch.base
	if r.dispatchDB, err = pgxpool.New(context.Background(), r.dispatch.database); err != nil {
		t.Fatalf("open Dispatch's database: %v", err)
	}
	t.Cleanup(r.dispatchDB.Close)
	r.setUpDispatch()

	work := t.TempDir()
	r.legion = buildProofLegion(t, r.root, work)
	r.home = filepath.Join(work, "omp-home")
	r.profile = "legion-check-states-" + strings.ToLower(r.cfg.Project)
	installProofPlugins(t, r.root, work, r.home, r.profile, omp)
	r.tester = newCompletionStandin(t, statesTesterProvider)
	r.implementer = newPaneStandin(t, statesImplementerProvider)
	r.spare = newPaneStandin(t, statesSpareProvider)
	r.writeOmpProfile()
	r.writeOverlays(work)

	r.rt = newLocalRuntime(t, r.cfg.StateDir, r.legion, omp, r.home, r.profile)
	r.rt.daemonURL, r.rt.envoyURL, r.rt.natsURLs = r.cfg.DaemonURL, newPaneListener(t), r.cfg.NatsURLs
	r.rt.dispatchURL, r.rt.dispatchTokenFile = r.cfg.DispatchURL, r.cfg.DispatchTokenFile
	r.rt.overlays = func(token claim.Token) []string {
		role, ok := token.Role()
		if overlays, driven := r.overlays[role]; ok && driven {
			return overlays
		}
		return r.overlays[""]
	}
	r.serveGitHub()
	r.seed()
	r.daemon = newRecordingLog(r.log())
	r.d = startDaemonWithin(t, r.cfg, r.overrides(), slog.New(r.daemon), 2*time.Minute)
	r.note("Dispatch %s (database of its own, events on %s), project %s mapped to %s; daemon at dispatch_url %s, each pane told it as DISPATCH_URL",
		r.dispatch.base, os.Getenv(liveNATSURLEnv), r.cfg.Project, liveRepo, r.cfg.DispatchURL)
	return r
}

// startStatesDispatch starts the run's Dispatch server from root's packages/envoy on a database of
// its own, as startHandoffDispatch does, with its events published on natsURL, the run's broker,
// where the daemon consumes them, and env's settings of its own (a GitHub App, where a case reads
// CI). Dispatch's own log goes beside the daemon's in the evidence directory when the script
// names one.
func startStatesDispatch(t *testing.T, root, natsURL string, env []string) *handoffDispatch {
	t.Helper()
	if natsURL == "" {
		t.Fatalf("%s names no broker for Dispatch to publish its events on", liveNATSURLEnv)
	}
	options := scratchDispatchOptions{root: root, natsURL: natsURL, env: env}
	if daemonLog := os.Getenv(liveDaemonLogEnv); daemonLog != "" {
		file, err := os.OpenFile(filepath.Join(filepath.Dir(daemonLog), "dispatch.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatalf("open Dispatch's log: %v", err)
		}
		t.Cleanup(func() { file.Close() })
		options.log = file
	}
	return &handoffDispatch{scratchDispatch: startScratchDispatchOn(t, options), client: &http.Client{Timeout: 30 * time.Second}}
}

// setUpDispatch creates the run's project on Dispatch, maps liveRepo to it as its default, and
// files each chosen case's issues, in progress and carrying the legion label, as a running daemon
// leaves them, each with the Verification document its case gives it. The duplicate gate is
// forced: the titles of one case share words on purpose.
func (r *statesProof) setUpDispatch() {
	t := r.t
	t.Helper()
	r.dispatch.user(http.MethodPost, "/api/v1/projects", map[string]string{"key": r.cfg.Project, "name": "Check states live proof"}, http.StatusCreated, nil)
	r.dispatch.user(http.MethodPut, "/api/v1/projects/"+r.cfg.Project+"/default-repository", map[string]any{"repo": liveRepo}, http.StatusOK, nil)
	base, err := strconv.ParseInt(randomSuffix(t)[:5], 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range r.cases {
		for _, spec := range statesCaseIssues[name] {
			var created struct {
				Key string `json:"key"`
			}
			r.dispatch.user(http.MethodPost, "/api/v1/issues", map[string]any{"project": r.cfg.Project, "title": spec.title, "force": true}, http.StatusCreated, &created)
			if created.Key == "" {
				t.Fatalf("Dispatch created the issue %q with no key", spec.title)
			}
			r.dispatch.user(http.MethodPatch, "/api/v1/issues/"+created.Key, map[string]any{"status": "in_progress", "labels": []string{"legion"}}, http.StatusOK, nil)
			content, document := statesNoBeforeMerge, "a Verification document whose one check, k3, is after_merge: no before_merge check"
			if spec.beforeMerge {
				content, document = statesVerification, "a Verification document of two before_merge checks, k1 and k2"
			}
			if spec.ciJob {
				content, document = statesVerificationCI, "a Verification document of k1, k2 and k3, whose ci_check names "+statesCIJob
			}
			r.dispatch.user(http.MethodPost, "/api/v1/issues/"+created.Key+"/artifacts", map[string]any{"name": "verification.md", "content": content}, http.StatusCreated, nil)
			base++
			issue := &statesIssue{key: created.Key, title: spec.title, head: headCommit(created.Key, "test"), number: int(base)}
			r.issues[name] = append(r.issues[name], issue)
			if spec.ciJob {
				head := issue.head
				r.ciFailedHead.Store(&head)
			}
			r.note("%s filed %s %q with %s; its pull request #%d is open at %s", name, issue.key, issue.title, document, issue.number, issue.head)
		}
	}
}

// seed records each issue as the daemon would hold it while its tester works: a root handed to
// Legion, in testing, on an open pull request at its head that GitHub would merge, holding its
// tree's admission slot with its lifecycle open, and its tester's claim mid-launch, which this
// boot relaunches, holding the task its phase's start left pending, written as the daemon writes
// a phase's task line (admission's repair of a slotted worker passes over a claim that holds one).
// Every pane's role prompt is an operator's short prompt file (writePrompt).
func (r *statesProof) seed() {
	t := r.t
	t.Helper()
	ctx := context.Background()
	all := r.allIssues()
	if err := store.Transact(ctx, r.st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		for _, issue := range all {
			listed := r.dispatch.issue(issue.key)
			if err := r.records.PutIssue(ctx, tx, record.Issue{
				Key: issue.key, Repo: liveRepo, DefaultBranch: "main", GitHubRepo: liveRepo, Tree: issue.key, Project: r.cfg.Project,
				Title: listed.Title, Phase: phase.Testing, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress",
				Rank: listed.Rank, LastDispatchSeq: listed.LastSeq,
			}); err != nil {
				return err
			}
			if err := r.records.PutPullRequest(ctx, tx, record.PullRequest{
				State: record.PullRequestOpen, Issue: issue.key, Repo: liveRepo, Number: issue.number, Branch: claim.IssueBranch(issue.key),
				HeadSHA: issue.head, Base: "main", Failing: []string{},
				Mergeability: record.MergeabilityMergeable, MergeState: record.MergeStateClean,
			}); err != nil {
				return err
			}
			if err := r.admit(ctx, tx, issue.key); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the issues: %v", err)
	}
	for _, issue := range all {
		if _, err := r.st.OpenTreeLifecycle(ctx, r.project, issue.key, treelifecycle.AuthorityWorkflow); err != nil {
			t.Fatalf("open the lifecycle of %s: %v", issue.key, err)
		}
		tester := r.token(issue.key, claim.RoleTester)
		if _, err := r.st.AdmitClaim(ctx, supervise.Claim{
			Token: tester, Project: r.project, Tree: issue.key, Issue: issue.key, Role: claim.RoleTester,
			Generation: 1, State: supervise.StateLaunching,
			BootTokenHash: supervise.HashBootToken("interrupted-" + randomSuffix(t)), ServingGeneration: 1,
		}); err != nil {
			t.Fatalf("record the tester's unfinished launch on %s: %v", issue.key, err)
		}
		task := fmt.Sprintf("Continue %s. Issue: %s. Phase: testing. Pull request: #%d.", issue.title, issue.key, issue.number)
		issue.seededTask = task
		if err := r.st.PutDelivery(ctx, tester, supervise.Delivery{
			ID: "seeded-start-" + randomSuffix(t), Task: task, Generation: 1, Phase: phase.Testing, QueuedAt: time.Now(),
		}); err != nil {
			t.Fatalf("record the task the tester's start left pending on %s: %v", issue.key, err)
		}
		for _, role := range []claim.Role{claim.RoleTester, claim.RoleImplementer, claim.RoleReviewer} {
			writePrompt(t, r.cfg, r.token(issue.key, role))
		}
	}
	r.note("seeded %d issues in testing, each on its open pull request (mergeable, clean) with its tester mid-launch", len(all))
}

// allIssues is every issue of the run, in the order of the cases that drive them.
func (r *statesProof) allIssues() []*statesIssue {
	var all []*statesIssue
	for _, name := range r.cases {
		all = append(all, r.issues[name]...)
	}
	return all
}

// token is the claim token of role on issue, as the daemon spells it.
func (r *statesProof) token(issue string, role claim.Role) claim.Token {
	r.t.Helper()
	token, err := claim.NewToken(r.project, issue, role)
	if err != nil {
		r.t.Fatal(err)
	}
	return token
}

// overrides is the daemon this proof boots: the local runtime over the real worker stream, the
// API listener the fixture holds, the workflow's App tokens stood in for, and GitHub's stand-in.
func (r *statesProof) overrides() overrides {
	return overrides{
		runtime: func(_ context.Context, listener *stream.Listener, address string, _ appauth.Source,
			_ func(context.Context, string) (ghrepo.Repository, string, error), _ *store.Store, _ func() []ghrepo.Repository) (runtime.Runtime, error) {
			r.rt.conns, r.rt.stream = listener, address
			return r.rt, nil
		},
		listen:         heldListen,
		workflowTokens: r.tokens,
		githubAPI:      r.github,
	}
}

// fail ends the run at the case being made, naming the assertion that did not hold.
func (r *statesProof) fail(assertion, format string, args ...any) {
	r.t.Helper()
	r.t.Fatalf("FAIL %s at %s: %s", r.current, assertion, fmt.Sprintf(format, args...))
}

// ---- GitHub, the Oh My Pi profile and the overlays ----------------------------------------------------

// serveGitHub starts the stand-in of GitHub's REST API for the run's pull requests, as the daemon
// reads them: each open on base main at its issue's head, mergeable and clean, with no reviews, the
// repository with default branch main, every other repository the same, and a GraphQL root that
// knows no pull request. Anything else is answered 404, so a read the proof did not foresee fails
// the case that waited on it rather than reaching github.com.
func (r *statesProof) serveGitHub() {
	pulls := map[string]map[string]any{}
	var listed []any
	for _, issue := range r.allIssues() {
		pull := map[string]any{
			"number": issue.number, "state": "open", "mergeable": true, "mergeable_state": "clean",
			"base": map[string]string{"ref": "main"},
			"head": map[string]string{"ref": claim.IssueBranch(issue.key), "sha": issue.head},
		}
		pulls[fmt.Sprintf("/pulls/%d", issue.number)] = pull
		pulls[fmt.Sprintf("/pulls/%d/reviews", issue.number)] = nil
		listed = append(listed, pull)
	}
	r.github = githubtest.Server(r.t, githubtest.Spec{Handle: func(w http.ResponseWriter, req *http.Request) {
		write := func(value any) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(value)
		}
		if req.Method == http.MethodPost && req.URL.Path == "/graphql" {
			write(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": nil}}})
			return
		}
		rest, ours := strings.CutPrefix(req.URL.Path, "/repos/"+liveRepo)
		switch pull, isPull := pulls[rest]; {
		case req.Method != http.MethodGet:
		case ours && rest == "":
			write(map[string]any{"full_name": liveRepo, "default_branch": "main"})
			return
		case ours && rest == "/pulls":
			write(listed)
			return
		case ours && isPull && pull == nil:
			write([]any{})
			return
		case ours && isPull:
			write(pull)
			return
		case !ours:
			if other, isRepository := strings.CutPrefix(req.URL.Path, "/repos/"); isRepository && len(strings.Split(other, "/")) == 2 {
				write(map[string]any{"full_name": other, "default_branch": "main"})
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	}})
}

// writeOmpProfile writes the profile the panes share: the three stand-in providers, one model
// each, and the settings a pane needs to run unattended.
func (r *statesProof) writeOmpProfile() {
	t := r.t
	t.Helper()
	agent := filepath.Join(r.home, ".omp", "profiles", r.profile, "agent")
	if err := os.MkdirAll(agent, 0o700); err != nil {
		t.Fatalf("make the profile's agent directory %s: %v", agent, err)
	}
	var models, enabled strings.Builder
	models.WriteString("# Written by packages/daemon/internal/daemon/check_states_live_test.go.\nproviders:\n")
	for _, stand := range []*modelStandin{r.tester.modelStandin, r.implementer.modelStandin, r.spare.modelStandin} {
		fmt.Fprintf(&models, "  %s:\n    baseUrl: %s\n    api: anthropic-messages\n    apiKey: a-literal-key-no-endpoint-checks\n"+
			"    models:\n      - id: %s\n        name: %s\n        contextWindow: 200000\n        maxTokens: 8192\n",
			stand.name, stand.url, stand.name+standinModelID, stand.name)
		fmt.Fprintf(&enabled, "  - %s\n", standinModel(stand.name))
	}
	writeProofFile(t, filepath.Join(agent, "models.yml"), models.String())
	writeProofFile(t, filepath.Join(agent, "config.yml"),
		"# Written by packages/daemon/internal/daemon/check_states_live_test.go.\n"+
			"enabledModels:\n"+enabled.String()+
			// A fresh profile opens Oh My Pi's setup wizard, and a pane waiting on it never starts.
			"startup:\n  setupWizard: false\n")
}

// writeOverlays writes the Oh My Pi overlays each pane is told through PI_CONFIG_FILES: the
// operator's own, which every pane names first and which turns `retry.enabled` off so the host
// walks no fallback chain of its own, then one naming the single model of the role's stand-in:
// the testers', the implementers', and the spare's for every other role (keyed "").
func (r *statesProof) writeOverlays(work string) {
	t := r.t
	t.Helper()
	dir := filepath.Join(work, "overlays")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	operator := filepath.Join(dir, "operator.yml")
	writeProofFile(t, operator, "# The operator's pane overlay (runtime.tmux.pi_config_files).\nretry:\n  enabled: false\n")
	r.overlays = map[claim.Role][]string{}
	for role, provider := range map[claim.Role]string{
		claim.RoleTester: statesTesterProvider, claim.RoleImplementer: statesImplementerProvider, "": statesSpareProvider,
	} {
		path := filepath.Join(dir, provider+".yml")
		writeRoleOverlay(t, path, standinModel(provider))
		r.overlays[role] = []string{operator, path}
	}
}
