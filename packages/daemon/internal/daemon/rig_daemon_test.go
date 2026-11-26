package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// TestRigDaemon is the label's own case of the skill-scenario rig's daemon
// (packages/pi-legion/scripts/skill-scenarios/label-texts.sh build): the label's whole daemon on
// the rig of an issue that closes when its checks pass (done_by_checks_rig_test.go), with a real
// PostgreSQL, a real NATS, a Dispatch of its own and a GitHub stand-in of its own, over the fake
// runtime. The issue's task runs the steps `implementer reviewer acceptance:owner`, and fake
// claimants work each step. It reads no request. Each case runs alone by its name:
//
//	-run '^TestRigDaemon$/^the_route$'
//	-run '^TestRigDaemon$/^the_transfer$'
func TestRigDaemon(t *testing.T) {
	t.Parallel()

	// The route: in its retro, the implementer sends one report_defect through the daemon's HTTP
	// API with its claim's grant. The daemon records the defect, the issue stays in retro, and no
	// retro failed.
	t.Run("the route", func(t *testing.T) {
		t.Parallel()
		r := newRigDaemon(t, rigDaemonSteps)
		r.passSteps(len(rigDaemonSteps))
		implementer := r.worker(claim.RoleImplementer)
		implementer.take()
		const text = "rig route case: src/config.ts loadConfig returns DEFAULTS on a parse error"
		if status, body := r.reportDefect(implementer, text); status != http.StatusOK {
			t.Fatalf("POST %s as the implementer in retro = %d %s, want 200", rigDefectRoute, status, body)
		}

		got := r.rigRecord()
		if len(got.Defects) != 1 || got.Defects[0].Role != string(claim.RoleImplementer) || got.Defects[0].Text != text {
			t.Fatalf("the record's defects are %+v, want one of role implementer with the text sent", got.Defects)
		}
		if got.Phase != string(phase.Retro) {
			t.Errorf("the record's phase is %q, want retro", got.Phase)
		}
		if failed := slices.IndexFunc(got.Steps, func(s rigDaemonStep) bool { return s.Kind == "retro" && s.Result == "failed" }); failed >= 0 {
			t.Errorf("the record's steps hold a failed retro: %+v", got.Steps[failed])
		}
		if !slices.ContainsFunc(got.Calls, func(c rigDaemonCall) bool {
			return c.Method == http.MethodPost && c.Path == rigDefectRoute && c.Status == http.StatusOK
		}) {
			t.Errorf("the record's calls hold no POST %s with status 200", rigDefectRoute)
		}
		t.Logf("the route case: defect %s of %s recorded, %s stands in %s", got.Defects[0].ID, got.Issue, got.Issue, got.Phase)
	})

	// The transfer: a claim keeps the session that registered first, so a second session's
	// registration on the implementer's claim is refused (claim.SameAgentRefusal). The
	// controller's fresh session stops the claim and launches it again in a new session, which
	// then registers and reports ready, and the claim names it.
	t.Run("the transfer", func(t *testing.T) {
		t.Parallel()
		r := newRigDaemon(t, rigDaemonSteps)
		r.passSteps(1)
		token := r.token(claim.RoleImplementer)
		first := r.worker(claim.RoleImplementer)
		launch := lastLaunch(t, r.rt, token)

		second := "ses_second_" + randomSuffix(t)[:6]
		status, body := r.registerAs(launch.BootToken, second)
		if status != http.StatusConflict || refusalOf(body) != claim.SameAgentRefusal.Message {
			t.Fatalf("a second session's registration on %s = %d %s, want 409 naming SameAgentRefusal (%q)",
				token, status, body, claim.SameAgentRefusal.Message)
		}
		t.Logf("before the fresh session, %s's registration on %s answered %d %s", second, token, status, strings.TrimSpace(body))

		before := len(r.launchesOf(token))
		r.registerController()
		status, body = r.freshSession(claim.RoleImplementer)
		if status != http.StatusOK || strings.Contains(body, `"phaseNotRestarted"`) {
			t.Fatalf("the controller's fresh session of %s = %d %s, want 200 with no phaseNotRestarted", token, status, body)
		}
		first.dropConnection()
		spawned := r.awaitNewLaunch(token, before, launch.BootToken)

		delete(r.claims, token)
		taken := r.worker(claim.RoleImplementer)
		if held := r.d.claim(token); held.Session != taken.session {
			t.Fatalf("after the fresh session %s names session %q, want the new session %s", token, held.Session, taken.session)
		}
		t.Logf("the transfer case: %s moved from %s to %s at generation %d, boot token %s, registered (200) and ready (204)",
			token, first.session, taken.session, spawned.Spec.Generation, spawned.Spec.BootToken)
	})
}

// rigDaemonSteps is the Steps line of the task of both cases of TestRigDaemon.
var rigDaemonSteps = []string{"implementer", "reviewer", "acceptance:owner"}

// rigDefectRoute is the daemon's route of the `legion` tool's report_defect.
const rigDefectRoute = "/legion/v1/defects/report"

// rigDaemonWorld is the one issue a rigDaemon runs: its key, whose project is the daemon's, its
// title, the number of its pull request, that pull request's head, and its task's Steps line, as
// plan.Step strings.
type rigDaemonWorld struct {
	issue, title string
	number       int
	head         string
	steps        []string
}

// rigDaemon is the label's whole daemon on Task 11a's rig (doneByChecksRig) for one world. The
// owner gives no plan, so the task runs the project's default_steps, which are the world's steps.
// Every request the run sends to the daemon's HTTP API goes through a recording handler in front
// of the daemon's own (calls). The project's controller session, once registered, mints the grant
// of a fresh session.
type rigDaemon struct {
	*doneByChecksRig
	world                               rigDaemonWorld
	calls                               *rigCalls
	controllerSession, controllerSecret string
}

// newRigDaemon is TestRigDaemon's daemon: Task 11a's own issue ACME-1, its pull request at
// stepRecordsHead, on a Dispatch of the test's own whose App reads the rig's GitHub stand-in. The
// test makes the project, its default repository, the delivery settings, the issue and its
// Verification document in that Dispatch, as Task 11a's rig makes them, then starts the daemon
// over the fake runtime (start).
func newRigDaemon(t *testing.T, steps []string) *rigDaemon {
	t.Helper()
	natsURL := workflowNATS(t)
	standIn := newDoneByChecksGitHub(t)
	dispatch := startScratchDispatchOn(t, scratchDispatchOptions{natsURL: natsURL, github: standIn.url})
	r := rigDaemonOver(t, newLiveProofOn(t, natsURL), dispatch, standIn, rigDaemonWorld{
		issue: doneByChecksIssue, title: doneByChecksTitle, number: doneByChecksPull, head: stepRecordsHead, steps: steps,
	})
	if err := os.WriteFile(r.cfg.DispatchTokenFile, []byte(dispatch.token+"\n"), 0o600); err != nil {
		t.Fatalf("write the Dispatch token file: %v", err)
	}
	r.person(http.MethodPost, "/api/v1/projects", map[string]string{"key": r.cfg.Project, "name": "Rig daemon"}, http.StatusCreated, nil)
	r.person(http.MethodPut, "/api/v1/projects/"+r.cfg.Project+"/default-repository", map[string]any{"repo": liveRepo}, http.StatusOK, nil)
	r.person(http.MethodPut, "/api/v1/settings/delivery", map[string]any{
		"deploy_repo": liveRepo, "deploy_workflow_path": ".github/workflows/deploy.yaml", "production_job_names": []string{"deploy"},
		"pr_checks_workflow_path": ".github/workflows/ci.yaml", "population_authors": []string{stepRecordsPerson},
	}, http.StatusOK, nil)
	var created createdIssue
	r.person(http.MethodPost, "/api/v1/issues", map[string]string{"project": r.cfg.Project, "title": r.title}, http.StatusCreated, &created)
	if created.Key != r.key {
		t.Fatalf("Dispatch made the project's first issue %q, want %s", created.Key, r.key)
	}
	r.person(http.MethodPost, "/api/v1/issues/"+r.key+"/artifacts",
		map[string]string{"name": "verification.md", "content": doneByChecksVerification}, http.StatusCreated, nil)
	r.built = &built{}
	r.start(fakeRuntime(r.rt, r.built))
	return r
}

// rigDaemonOver readies the daemon of world over proof, dispatch and standIn before anything is
// written: the project is the issue's, whose default_steps are the world's steps, and the
// runtime of the fake claimants is a fake of the rig's own, which every launch reaches unless the
// caller boots the daemon over another runtime that holds it (start).
func rigDaemonOver(t *testing.T, proof liveProof, dispatch *scratchDispatch, standIn *doneByChecksGitHub, world rigDaemonWorld) *rigDaemon {
	t.Helper()
	project, _, ok := strings.Cut(world.issue, "-")
	if !ok {
		t.Fatalf("the issue %q is no Dispatch issue key", world.issue)
	}
	steps, err := plan.ParseSteps(strings.Join(world.steps, " "))
	if err != nil {
		t.Fatalf("the steps %q: %v", world.steps, err)
	}
	proof.cfg.Project = project
	proof.cfg.Projects = map[string]config.Project{project: {DefaultSteps: steps}}
	proof.cfg.DispatchURL = dispatch.base
	if proof.project, err = claim.ProjectToken(project); err != nil {
		t.Fatal(err)
	}
	r := &rigDaemon{world: world, doneByChecksRig: &doneByChecksRig{standIn: standIn, stepRecordsRig: &stepRecordsRig{
		liveProof: proof, dispatch: dispatch, github: &stepRecordsGitHub{t: t, url: standIn.url},
		claims: map[claim.Token]*stepClaimant{}, waits: make(chan claim.Token, 16),
	}}}
	r.rt = fake.NewRuntime()
	// The runtime provisions each claim's workspace itself, so a launch reaches the fake without
	// the daemon cloning acme/widgets from github.com.
	r.rt.InPod = true
	r.issues = []walkIssue{{key: world.issue, title: world.title, number: world.number}}
	r.on(world.issue)
	return r
}

// start hands the issue to Legion in Dispatch, seeds the daemon's store with it (seedAt), opens
// its pull request at the world's head in GitHub's stand-in and publishes GitHub's opened of it,
// boots the daemon over o, puts the recording handler in front of the daemon's HTTP API, and
// registers the tree's architect, a fake claimant, at the boot's relaunch.
func (r *rigDaemon) start(o overrides) {
	t := r.t
	t.Helper()
	var handed createdIssue
	r.person(http.MethodPatch, "/api/v1/issues/"+r.key, map[string]any{"status": "in_progress", "labels": []string{"legion"}}, http.StatusOK, &handed)
	r.standIn.open(r.number, claim.IssueBranch(r.key), r.world.head)
	r.seedAt(handed.Rank, handed.LastSeq)
	opened := r.publishPullRequest("opened")
	t.Logf("GitHub opened pull request #%d of %s from %s at %s: envelope %s", r.number, liveRepo, claim.IssueBranch(r.key), r.world.head, opened)
	o.workflowTokens = &workflowTokenRecorder{}
	o.githubAPI = r.github.url
	r.d = startDaemonWithin(t, r.cfg, o, r.log(), stepRecordsWait)
	r.calls = newRigCalls(t, r.d.base)
	r.d.base = r.calls.url
	r.worker(claim.RoleArchitect)
}

// seedAt records the issue as Task 11a's rig seeds it (stepRecordsRig.seed): a root handed to
// Legion at admitted, its gate approved, its lifecycle under the workflow, its pull request open
// and clean at the world's head, and its architect's claim a launch a previous daemon began and
// never recorded, which the boot relaunches. It writes no role prompt of its own (writePrompt), so
// each launch carries the label's own prompt.
func (r *rigDaemon) seedAt(rank string, seq int64) {
	t := r.t
	t.Helper()
	ctx := context.Background()
	approved := 1
	if err := store.Transact(ctx, r.st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		if err := r.records.PutIssue(ctx, tx, record.Issue{
			Key: r.key, Repo: liveRepo, DefaultBranch: "main", GitHubRepo: liveRepo, Tree: r.key, Project: r.cfg.Project,
			Title: r.title, Phase: phase.Admitted, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress",
			Rank: rank, LastDispatchSeq: seq, HandedOver: true,
		}); err != nil {
			return err
		}
		if err := r.records.PutGate(ctx, tx, record.DesignGate{
			Issue: r.key, ArtifactID: "spec-" + r.key, LatestVersion: 1, ApprovedVersion: &approved, EverOpened: true,
		}); err != nil {
			return err
		}
		return r.records.PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: r.key, Repo: liveRepo,
			Number: r.number, Branch: claim.IssueBranch(r.key), HeadSHA: r.world.head, MergeState: record.MergeStateClean, Failing: []string{}})
	}); err != nil {
		t.Fatalf("seed %s: %v", r.key, err)
	}
	if _, err := r.st.OpenTreeLifecycle(ctx, r.project, r.key, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("open the lifecycle of %s: %v", r.key, err)
	}
	if _, err := r.st.AdmitClaim(ctx, supervise.Claim{
		Token: r.token(claim.RoleArchitect), Project: r.project, Tree: r.key, Issue: r.key, Role: claim.RoleArchitect,
		Generation: 1, State: supervise.StateLaunching, BootTokenHash: supervise.HashBootToken("interrupted-" + randomSuffix(t)),
	}); err != nil {
		t.Fatalf("record the architect's unfinished launch on %s: %v", r.key, err)
	}
}

// passSteps has fake claimants pass the first n steps of the task, each step's agent taking its
// task and finishing it, and waits after each for the issue to stand in the next step's phase, or
// in retro after the last step.
func (r *rigDaemon) passSteps(n int) {
	r.t.Helper()
	for place, step := range r.world.steps[:n] {
		next := phase.Retro
		if place+1 < len(r.world.steps) {
			next = stepPhase(r.t, r.world.steps[place+1])
		}
		r.passStep(step, next)
		r.t.Logf("the walk: step %d, %s, passed; %s stands in %s", place+1, step, r.key, next)
	}
}

// passStep is one passing run of step by its fake claimant, and the wait for the issue to stand
// in next. Each completion and review that names a commit names the world's head.
func (r *rigDaemon) passStep(step string, next phase.Phase) {
	r.t.Helper()
	switch step {
	case "planner":
		r.work(claim.RolePlanner, "plan", api.HandoffCompleteRequest{Summary: "planned"}, next)
	case "implementer":
		r.work(claim.RoleImplementer, "implement", api.HandoffCompleteRequest{Summary: "implemented", Commit: r.world.head}, next)
	case "tester":
		r.work(claim.RoleTester, "test", testCompletion, next)
	case "reviewer":
		r.approve(next)
	case "acceptance:owner":
		r.signOffTo(next)
	default:
		r.t.Fatalf("the rig has no fake claimant for the step %q", step)
	}
}

// approve is the reviewer taking its task and answering, and then a person with write access
// approving the world's head on its pull request, as Envoy relays GitHub's review; the issue then
// reaches next.
func (r *rigDaemon) approve(next phase.Phase) {
	t := r.t
	t.Helper()
	c := r.worker(claim.RoleReviewer)
	c.take()
	c.endTurn()
	r.reviews++
	payload, err := json.Marshal(map[string]string{
		"action": "submitted", "kind": "review", "repo": liveRepo, "number": strconv.Itoa(r.number), "state": "APPROVED",
		"commit_id": r.world.head, "head_sha": r.world.head, "author": stepRecordsPerson, "body": "round " + strconv.Itoa(r.reviews),
		"review_id": strconv.Itoa(r.number*10 + r.reviews), "submitted_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatal(err)
	}
	subject := fmt.Sprintf("notifications.github.%s.pr.%d.review", r.subjectRepo(), r.number)
	r.publish(subject, "github", fmt.Sprintf("review-%d-%d", r.number, r.reviews), string(payload))
	r.waitPhase(next)
}

// signOffTo is the tree architect answering in a turn of its own and signing off the issue
// through its own route (stepRecordsRig.signOff); the issue then reaches next.
func (r *rigDaemon) signOffTo(next phase.Phase) {
	t := r.t
	t.Helper()
	c := r.worker(claim.RoleArchitect)
	c.ownTurn()
	if status, body := r.d.request(http.MethodPost, "/legion/v1/signoff", map[string]any{"grantId": c.grant(), "issue": r.key}, false); status != http.StatusOK {
		t.Fatalf("the architect's sign-off of %s = %d %s, want 200", r.key, status, body)
	}
	r.waitPhase(next)
}

// reportDefect is c's report_defect of text at the world's head, sent as its `legion` tool sends
// it, with a grant minted for c now, answered as the daemon's status and body.
func (r *rigDaemon) reportDefect(c *stepClaimant, text string) (int, string) {
	r.t.Helper()
	status, body := r.d.request(http.MethodPost, rigDefectRoute,
		map[string]string{"grantId": c.grant(), "issue": r.key, "defect": text, "head": r.world.head}, false)
	return status, string(body)
}

// registerAs is a registration on the launch of bootToken as session, answered as the daemon's
// status and body.
func (r *rigDaemon) registerAs(bootToken, session string) (int, string) {
	r.t.Helper()
	status, body := r.d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: bootToken, SessionID: session, OmpSessionFile: "/sessions/" + session + ".jsonl",
		AgentID: session, PluginContract: api.DaemonAPIVersion(),
	}, false)
	return status, string(body)
}

// refusalOf is the sentence of a refusal's body, {"error": <sentence>}, and "" for a body of any
// other shape.
func refusalOf(body string) string {
	var refusal struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(body), &refusal) != nil {
		return ""
	}
	return refusal.Error
}

// launchesOf is each launch, Spawn or Resume, of token's claim the fake runtime was asked for, in
// order.
func (r *rigDaemon) launchesOf(token claim.Token) []fake.Call {
	var launches []fake.Call
	for _, call := range r.rt.Calls() {
		if (call.Method == "Spawn" || call.Method == "Resume") && call.Spec.Claim == token {
			launches = append(launches, call)
		}
	}
	return launches
}

// awaitNewLaunch waits for a launch of token's claim after the first before of them, and fails
// unless exactly one came and it is a Spawn under a boot token other than old.
func (r *rigDaemon) awaitNewLaunch(token claim.Token, before int, old string) fake.Call {
	r.t.Helper()
	var launches []fake.Call
	testwait.WithinPolling(r.t, stepRecordsWait, 50*time.Millisecond, "a new launch of "+string(token), func() bool {
		launches = r.launchesOf(token)
		return len(launches) > before
	})
	launched := launches[before:]
	if len(launched) != 1 || launched[0].Method != "Spawn" || launched[0].Spec.BootToken == "" || launched[0].Spec.BootToken == old {
		methods := make([]string, len(launched))
		for place, call := range launched {
			methods[place] = fmt.Sprintf("%s (boot token %q)", call.Method, call.Spec.BootToken)
		}
		r.t.Fatalf("after the fresh session the fake runtime holds the new launches %v of %s, want one Spawn with a boot token other than %q",
			methods, token, old)
	}
	return launched[0]
}

// registerController registers a session as the project's controller, as `legion controller
// start` registers the operator's own (claimCompactionLive.registerController), and keeps the
// registration's secret, which mints the controller's grants (controllerGrant).
func (r *rigDaemon) registerController() {
	t := r.t
	t.Helper()
	status, body := r.d.request(http.MethodPost, "/legion/v1/controller/secret",
		api.ControllerSecretRequest{PluginContract: api.DaemonAPIVersion()}, true)
	if status != http.StatusOK {
		t.Fatalf("the controller's boot secret = %d %s, want 200", status, body)
	}
	var capability api.ControllerSecretResponse
	if err := json.Unmarshal(body, &capability); err != nil {
		t.Fatalf("decode the controller's boot secret %s: %v", body, err)
	}
	r.controllerSession = "ses_controller_" + randomSuffix(t)
	status, body = r.d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: capability.Secret, SessionID: r.controllerSession, OmpSessionFile: "/sessions/" + r.controllerSession + ".jsonl",
		AgentID: r.controllerSession, PluginContract: api.DaemonAPIVersion(),
	}, false)
	if status != http.StatusOK {
		t.Fatalf("register the controller = %d %s, want 200", status, body)
	}
	var registered api.ControllerRegisterResponse
	if err := json.Unmarshal(body, &registered); err != nil {
		t.Fatalf("decode the controller's registration %s: %v", body, err)
	}
	r.controllerSecret = registered.Secret
}

// freshSession is the controller's fresh_session of role's claim on the issue, with a controller
// grant minted now (claimCompactionLive.controllerGrant), answered as the daemon's status and body.
func (r *rigDaemon) freshSession(role claim.Role) (int, string) {
	t := r.t
	t.Helper()
	status, body := r.d.request(http.MethodPost, "/legion/v1/grants",
		api.GrantRequest{SessionID: r.controllerSession, Secret: r.controllerSecret}, false)
	if status != http.StatusOK {
		t.Fatalf("mint the controller's grant = %d %s, want 200", status, body)
	}
	var minted api.GrantResponse
	if err := json.Unmarshal(body, &minted); err != nil {
		t.Fatalf("decode the controller's grant %s: %v", body, err)
	}
	status, body = r.d.request(http.MethodPost, "/legion/v1/claims/fresh-session",
		api.ClaimRequest{GrantID: minted.GrantID, Issue: r.key, Role: string(role)}, false)
	return status, string(body)
}

// dropConnection closes the pane's shim connection, as the pane of a claim the daemon launched
// again in a new session ends. The test's own end of the connection (shim.end) still runs.
func (c *stepClaimant) dropConnection() {
	c.mu.Lock()
	sh := c.sh
	c.mu.Unlock()
	_ = sh.conn.Close()
}

// rigDaemonRecord is RigDaemonRecord of
// packages/pi-legion/scripts/skill-scenarios/scored-runs-contracts.ts: the issue as the daemon's
// state shows it, every call of the run (rigCalls), each defect of the issue, and the issue's step
// records as Dispatch serves them.
type rigDaemonRecord struct {
	Issue      string            `json:"issue"`
	Phase      string            `json:"phase"`
	Generation uint64            `json:"generation"`
	Calls      []rigDaemonCall   `json:"calls"`
	Defects    []rigDaemonDefect `json:"defects"`
	Steps      []rigDaemonStep   `json:"steps"`
}

// rigDaemonCall is one request to the daemon's HTTP API: when it came, its method, its path, the
// daemon's status, and the request's and the answer's bodies, each its JSON value, null for an
// empty body and a string for a body that is no JSON.
type rigDaemonCall struct {
	At     string          `json:"at"`
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
	Answer json.RawMessage `json:"answer"`
}

// rigDaemonDefect is one row of the daemon's defects of the issue, open, carried or withdrawn
// (record.Defect): CarriedBy is the generation of the change that carries it, null for none, and
// Withdrawn the time a withdrawal ended it, null while none has.
type rigDaemonDefect struct {
	ID         string  `json:"id"`
	Text       string  `json:"text"`
	Role       string  `json:"role"`
	Head       string  `json:"head"`
	Generation uint64  `json:"generation"`
	CarriedBy  *uint64 `json:"carriedBy"`
	Withdrawn  *string `json:"withdrawn"`
}

// rigDaemonStep is one step record of the issue, as GET /api/v1/issues/{key}/steps serves it.
type rigDaemonStep struct {
	Task    string `json:"task"`
	Step    int    `json:"step"`
	Kind    string `json:"kind"`
	Role    string `json:"role"`
	Attempt int    `json:"attempt"`
	Result  string `json:"result"`
}

// rigRecord is the run's record now. The calls are read last, so they hold the reads this makes.
func (r *rigDaemon) rigRecord() rigDaemonRecord {
	t := r.t
	t.Helper()
	issue, ok := r.d.state().Issues[r.key]
	if !ok {
		t.Fatalf("the daemon's state holds no issue %s", r.key)
	}
	read, err := r.readSteps()
	if err != nil {
		t.Fatalf("read the steps of %s: %v", r.key, err)
	}
	steps := []rigDaemonStep{}
	for _, step := range read.records() {
		steps = append(steps, rigDaemonStep{Task: step.Task, Step: step.Step, Kind: step.Kind, Role: step.Role, Attempt: step.Attempt, Result: step.Result})
	}
	return rigDaemonRecord{Issue: r.key, Phase: string(issue.Phase), Generation: issue.Generation, Defects: r.defects(), Steps: steps, Calls: r.calls.all()}
}

// defects is each defect of the issue in the daemon's store, open, carried or withdrawn, in the
// order it was recorded.
func (r *rigDaemon) defects() []rigDaemonDefect {
	t := r.t
	t.Helper()
	rows, err := r.st.Pool().Query(context.Background(), `select id, text, role, head, generation, coalesce(carried_by, 0), withdrawn_at
		from defects where issue = $1 order by id`, r.key)
	if err != nil {
		t.Fatalf("read the defects of %s: %v", r.key, err)
	}
	defer rows.Close()
	all := []rigDaemonDefect{}
	for rows.Next() {
		var id, generation, carriedBy int64
		var defect rigDaemonDefect
		var withdrawnAt *time.Time
		if err := rows.Scan(&id, &defect.Text, &defect.Role, &defect.Head, &generation, &carriedBy, &withdrawnAt); err != nil {
			t.Fatalf("read a defect of %s: %v", r.key, err)
		}
		defect.ID, defect.Generation = strconv.FormatInt(id, 10), uint64(generation)
		if carriedBy != 0 {
			carried := uint64(carriedBy)
			defect.CarriedBy = &carried
		}
		if withdrawnAt != nil {
			withdrawn := withdrawnAt.UTC().Format(time.RFC3339Nano)
			defect.Withdrawn = &withdrawn
		}
		all = append(all, defect)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the defects of %s: %v", r.key, err)
	}
	return all
}

// rigCalls is the recording handler in front of the daemon's HTTP API, served at url: it forwards
// each request to the daemon at target, and keeps it with the daemon's answer, whatever its status,
// before the caller has the answer.
type rigCalls struct {
	target string
	url    string
	client *http.Client
	mu     sync.Mutex
	calls  []rigDaemonCall
}

// newRigCalls serves a recording handler in front of target until the test ends, before the
// daemon behind it stops.
func newRigCalls(t *testing.T, target string) *rigCalls {
	t.Helper()
	transport := &http.Transport{}
	c := &rigCalls{target: target, client: &http.Client{Transport: transport}}
	server := httptest.NewServer(c)
	t.Cleanup(func() {
		server.Close()
		transport.CloseIdleConnections()
	})
	c.url = server.URL
	return c
}

func (c *rigCalls) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	at := time.Now().UTC().Format(time.RFC3339Nano)
	body, err := io.ReadAll(req.Body)
	if err != nil {
		c.keep(rigDaemonCall{At: at, Method: req.Method, Path: req.URL.Path, Status: http.StatusBadRequest, Body: json.RawMessage("null"), Answer: rigJSON([]byte(err.Error()))})
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	call := rigDaemonCall{At: at, Method: req.Method, Path: req.URL.Path, Body: rigJSON(body)}
	forward, err := http.NewRequestWithContext(req.Context(), req.Method, c.target+req.URL.RequestURI(), bytes.NewReader(body))
	if err == nil {
		forward.Header = req.Header.Clone()
		var response *http.Response
		if response, err = c.client.Do(forward); err == nil {
			defer response.Body.Close()
			var answer []byte
			if answer, err = io.ReadAll(response.Body); err == nil {
				call.Status, call.Answer = response.StatusCode, rigJSON(answer)
				c.keep(call)
				for name, values := range response.Header {
					if name != "Content-Length" && name != "Connection" && name != "Keep-Alive" {
						w.Header()[name] = values
					}
				}
				w.WriteHeader(response.StatusCode)
				_, _ = w.Write(answer)
				return
			}
		}
	}
	call.Status, call.Answer = http.StatusBadGateway, rigJSON([]byte(err.Error()))
	c.keep(call)
	http.Error(w, err.Error(), http.StatusBadGateway)
}

func (c *rigCalls) keep(call rigDaemonCall) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, call)
}

// all is every call kept so far, in the order each was answered.
func (c *rigCalls) all() []rigDaemonCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]rigDaemonCall{}, c.calls...)
}

// rigJSON is a body as a JSON value: null when it is empty, itself when it is JSON, and a string
// of it otherwise.
func rigJSON(body []byte) json.RawMessage {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return json.RawMessage("null")
	}
	if json.Valid(trimmed) {
		return json.RawMessage(trimmed)
	}
	encoded, err := json.Marshal(string(body))
	if err != nil {
		return json.RawMessage("null")
	}
	return encoded
}
