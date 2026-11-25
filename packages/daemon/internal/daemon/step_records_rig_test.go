// The rig of the whole-daemon walks of step_records_test.go and plan_ratings_test.go
// (stepRecordsRig): the booted daemon over the real Dispatch, the issues of the run and their pull
// requests, each role's agent over the worker stream and the daemon's HTTP routes (stepClaimant),
// the read of an issue's steps and what a read must hold (stepsHold), and GitHub's REST API as the
// walk's daemon reads it (stepRecordsGitHub).
//
// Each claimant counts its agent's calls as the shim does, and the rig reads the daemon's log of
// its reads of them (usedReads, emptyReads).
package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/steprecord"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// stepRecordsRig is one walk: the booted daemon over the real Dispatch, the stand-ins it reads,
// the issues of the run and their pull requests, and each role's agent as the test plays it. The
// walk's steps act on one issue of issues at a time, its walkIssue, which on moves to another.
type stepRecordsRig struct {
	liveProof
	walkIssue
	rt       *fake.Runtime
	built    *built
	dispatch *scratchDispatch
	github   *stepRecordsGitHub
	issues   []walkIssue
	claims   map[claim.Token]*stepClaimant
	// reviews counts the reviews published, which numbers each.
	reviews int
	// waits names the claim of each agent's request that began its wait for the claim's machine
	// (overrides.streamWaitBegan), as heldRequest reads it; a wait sent while it is full is not
	// kept.
	waits chan claim.Token
	// held is the answer heldRequest holds on its claim's feed (overrides.beforeDecision), nil
	// while it holds none.
	held atomic.Pointer[heldAnswer]
	// waited is whether the request heldRequest sent last began its wait for its claim's machine
	// (overrides.streamWaitBegan) before it answered: what a test reads to tell a request that
	// waited for its agent's stream from one that applied at once.
	waited bool
	// logs is the daemon's JSON log across every boot of the rig, which usedReads and emptyReads
	// read.
	logs *syncBuffer
	// readClock bounds each read of an agent's calls at every boot (overrides.totalsClock). It
	// starts on the wall clock, and use swaps it.
	readClock *switchClock
	// readHook is the hook onRead set, which each boot's overrides.totalsRead calls. It is nil
	// while none is set.
	readHook atomic.Pointer[func(agents []claim.Token, reads workflow.FactReads)]
	// spawnHold is the hold holdSpawns arms, which each boot's fake runtime runs at the start of
	// every Spawn (fake.Runtime.OnSpawn). It is nil while none is armed.
	spawnHold atomic.Pointer[func(ctx context.Context) error]
}

// heldAnswer is the answer heldRequest holds: the feed of token's claim stops before its machine
// decides the answer naming model, says so by closing reached, and decides it once release closes.
type heldAnswer struct {
	token   claim.Token
	model   string
	reached chan struct{}
	release chan struct{}
}

// walkIssue is one issue of the run, the title Dispatch was given for it, and the number of its
// pull request.
type walkIssue struct {
	key    string
	title  string
	number int
}

// newStepRecordsRig is newStepRecordsRigWith for a run of issues issues over the package's broker
// and a Dispatch of this checkout, which the package's own tests run. The live drivers call
// newStepRecordsRigWith themselves, through newStepRecordsLive (step_records_live_test.go).
func newStepRecordsRig(t *testing.T, issues int) *stepRecordsRig {
	t.Helper()
	return newStepRecordsRigWith(t, newLiveProofOn(t, workflowNATS(t)), startScratchDispatchOn(t, scratchDispatchOptions{}), issues)
}

// newStepRecordsRigWith sets up dispatch, a running Dispatch, with the run's project, its
// default repository and issues issues, seeds the daemon's store of proof with each issue at
// admitted, its gate approved, on an open pull request of its own GitHub reports clean, boots the
// daemon at Dispatch and registers each tree's architect. The admission pass then starts planning,
// the first step of each walk. The walk's steps act on the first issue until on moves them.
func newStepRecordsRigWith(t *testing.T, proof liveProof, dispatch *scratchDispatch, issues int) *stepRecordsRig {
	t.Helper()
	// A Dispatch project key is at most ten characters, and the daemon's project is its Dispatch
	// project, so the run's project is a short one of its own.
	proof.cfg.Project = "SR" + randomSuffix(t)[:6]
	proof.cfg.Projects = map[string]config.Project{proof.cfg.Project: {}}
	proof.cfg.AdmissionCap = 4
	var err error
	if proof.project, err = claim.ProjectToken(proof.cfg.Project); err != nil {
		t.Fatal(err)
	}
	number, err := strconv.ParseInt(randomSuffix(t)[:6], 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	r := &stepRecordsRig{liveProof: proof, dispatch: dispatch, github: newStepRecordsGitHub(t), claims: map[claim.Token]*stepClaimant{},
		waits: make(chan claim.Token, 16)}
	if err := os.WriteFile(r.cfg.DispatchTokenFile, []byte(r.dispatch.token+"\n"), 0o600); err != nil {
		t.Fatalf("write the Dispatch token file: %v", err)
	}
	r.cfg.DispatchURL = r.dispatch.base
	r.person(http.MethodPost, "/api/v1/projects", map[string]string{"key": r.cfg.Project, "name": "Step records"}, http.StatusCreated, nil)
	r.person(http.MethodPut, "/api/v1/projects/"+r.cfg.Project+"/default-repository", map[string]any{"repo": liveRepo}, http.StatusOK, nil)
	for place := range issues {
		var created createdIssue
		r.person(http.MethodPost, "/api/v1/issues", map[string]string{"project": r.cfg.Project, "title": walkTitles[place]}, http.StatusCreated, &created)
		r.person(http.MethodPatch, "/api/v1/issues/"+created.Key, map[string]any{"status": "in_progress", "labels": []string{"legion"}}, http.StatusOK, &created)
		r.issues = append(r.issues, walkIssue{key: created.Key, title: walkTitles[place], number: int(number) + 1 + place})
		r.on(created.Key)
		r.github.open("legion/"+r.key, r.number)
		r.seed(created.Rank, created.LastSeq)
	}
	r.boot()
	// The architect's pane is met at the boot's relaunch, so it is a live session before any
	// restart: a restarted daemon re-observes a claim it finds live and does not launch it again.
	for _, issue := range r.issues {
		r.on(issue.key)
		r.worker(claim.RoleArchitect)
	}
	r.on(r.issues[0].key)
	return r
}

// walkTitles is the title of each issue of a run, in order. Dispatch refuses a new issue whose
// title is a near copy of an open one's (POSSIBLE_DUPLICATE), so no two share a word that counts.
var walkTitles = []string{"One issue walks to done", "A second change rates its plan", "Third piece merges after its retro", "Fourth job refused unrated",
	"Fifth ticket runs reordered"}

// on moves the walk's steps to issue key of the run.
func (r *stepRecordsRig) on(key string) {
	r.t.Helper()
	for _, issue := range r.issues {
		if issue.key == key {
			r.walkIssue = issue
			return
		}
	}
	r.t.Fatalf("%s is no issue of the run %v", key, r.issues)
}

// createdIssue is an issue as Dispatch answers its creation: its key, its rank and the sequence
// the daemon's store records as the last it saw of the issue.
type createdIssue struct {
	Key     string `json:"key"`
	Rank    string `json:"rank"`
	LastSeq int64  `json:"last_seq"`
}

// stepRecordsPullRequest is pull request number of issue, open at stepRecordsHead on the branch
// legion/<issue>, which GitHub reports clean, as the daemon's store records it.
func stepRecordsPullRequest(issue string, number int) record.PullRequest {
	return record.PullRequest{State: record.PullRequestOpen, Issue: issue, Repo: liveRepo, Number: number,
		Branch: "legion/" + issue, HeadSHA: stepRecordsHead, MergeState: record.MergeStateClean, Failing: []string{}}
}

// seed records the issue as the daemon holds it the moment its architect's spec is approved: a
// root handed to Legion at admitted, its gate opened, its lifecycle under the workflow and its
// architect's claim a launch a previous daemon began and never recorded, which the boot relaunches.
// Its pull request is recorded open at stepRecordsHead, which GitHub reports clean, so a review
// that approves that head ends its round and the implementer's completions find it.
func (r *stepRecordsRig) seed(rank string, seq int64) {
	t := r.t
	t.Helper()
	ctx := context.Background()
	approved := 1
	if err := store.Transact(ctx, r.st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		if err := r.records.PutIssue(ctx, tx, record.Issue{
			Key: r.key, Repo: liveRepo, DefaultBranch: "main", GitHubRepo: liveRepo, Tree: r.key, Project: r.cfg.Project,
			Title: "One issue walks to done", Phase: phase.Admitted, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress",
			Rank: rank, LastDispatchSeq: seq, HandedOver: true,
		}); err != nil {
			return err
		}
		if err := r.records.PutGate(ctx, tx, record.DesignGate{
			Issue: r.key, ArtifactID: "spec-" + r.key, LatestVersion: 1, ApprovedVersion: &approved, EverOpened: true,
		}); err != nil {
			return err
		}
		return r.records.PutPullRequest(ctx, tx, stepRecordsPullRequest(r.key, r.number))
	}); err != nil {
		t.Fatalf("seed %s: %v", r.key, err)
	}
	if _, err := r.st.OpenTreeLifecycle(ctx, r.project, r.key, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("open the lifecycle of %s: %v", r.key, err)
	}
	architect := r.token(claim.RoleArchitect)
	if _, err := r.st.AdmitClaim(ctx, supervise.Claim{
		Token: architect, Project: r.project, Tree: r.key, Issue: r.key, Role: claim.RoleArchitect,
		Generation: 1, State: supervise.StateLaunching, BootTokenHash: supervise.HashBootToken("interrupted-" + randomSuffix(t)),
	}); err != nil {
		t.Fatalf("record the architect's unfinished launch on %s: %v", r.key, err)
	}
	writePrompt(t, r.cfg, architect)
}

// boot starts the whole daemon on the store as it stands, over a fake runtime of its own. Every
// boot writes to one log and bounds its reads of an agent's calls by one clock. The first boot
// makes both, so the rigs built by a literal of their own (newOwnerStepsRig, inFlightRig) get
// them too.
func (r *stepRecordsRig) boot() {
	r.t.Helper()
	if r.logs == nil {
		r.logs, r.readClock = &syncBuffer{}, newSwitchClock(supervise.RealClock{})
	}
	r.rt, r.built = fake.NewRuntime(), &built{}
	// The runtime provisions each claim's workspace itself, so a launch reaches the fake without
	// the daemon cloning acme/widgets from github.com.
	r.rt.InPod = true
	r.rt.OnSpawn = func(ctx context.Context) error {
		if hold := r.spawnHold.Load(); hold != nil {
			return (*hold)(ctx)
		}
		return nil
	}
	o := fakeRuntime(r.rt, r.built)
	o.workflowTokens = &workflowTokenRecorder{}
	o.githubAPI = r.github.url
	o.streamWaitBegan = func(token claim.Token) {
		select {
		case r.waits <- token:
		default:
		}
	}
	o.beforeDecision = func(token claim.Token, ev supervise.Event) {
		held := r.held.Load()
		if answered, ok := ev.(supervise.StreamAnswered); ok && held != nil && token == held.token && answered.Model == held.model {
			close(held.reached)
			<-held.release
		}
	}
	o.totalsRead = func(agents []claim.Token, reads workflow.FactReads) {
		if hook := r.readHook.Load(); hook != nil {
			(*hook)(agents, reads)
		}
	}
	o.totalsClock = r.readClock
	logger := slog.New(slog.NewMultiHandler(r.log().Handler(), slog.NewJSONHandler(r.logs, nil)))
	r.d = startDaemonWithin(r.t, r.cfg, o, logger, stepRecordsWait)
}

// onRead sets f as the hook of each read of an agent's calls (overrides.totalsRead). A nil f
// clears it. The daemon calls it on the fact's goroutine, after the reads and before the fact's
// transaction.
func (r *stepRecordsRig) onRead(f func(agents []claim.Token, reads workflow.FactReads)) {
	if f == nil {
		r.readHook.Store(nil)
		return
	}
	r.readHook.Store(&f)
}

// holdSpawns holds each Spawn of the fake runtime from now on, once the runtime has recorded the
// call. The claim then stays launching, with no process recorded. It answers the function that
// lets every held Spawn go on, which the test's end calls too. A Spawn whose context ends first
// answers that context's error.
func (r *stepRecordsRig) holdSpawns() (release func()) {
	let := make(chan struct{})
	release = sync.OnceFunc(func() {
		r.spawnHold.Store(nil)
		close(let)
	})
	hold := func(ctx context.Context) error {
		select {
		case <-let:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.spawnHold.Store(&hold)
	r.t.Cleanup(release)
	return release
}

// switchClock is a supervise.Clock that answers as the clock it holds now, which use swaps. A
// timer armed before a swap stays on the clock it was armed on.
type switchClock struct {
	held atomic.Pointer[supervise.Clock]
}

// newSwitchClock is a switchClock holding clock.
func newSwitchClock(clock supervise.Clock) *switchClock {
	c := &switchClock{}
	c.use(clock)
	return c
}

// use makes clock the one c answers as, from the next call on.
func (c *switchClock) use(clock supervise.Clock) { c.held.Store(&clock) }

// now is the clock c holds now.
func (c *switchClock) now() supervise.Clock { return *c.held.Load() }

// Now is the time of the clock held now.
func (c *switchClock) Now() time.Time { return c.now().Now() }

// AfterFunc arms f on the clock held now.
func (c *switchClock) AfterFunc(d time.Duration, f func()) supervise.Cancel {
	return c.now().AfterFunc(d, f)
}

// At arms f for at on the clock held now.
func (c *switchClock) At(at time.Time, f func()) supervise.Cancel { return c.now().At(at, f) }

// Sleep sleeps on the clock held now.
func (c *switchClock) Sleep(ctx context.Context, d time.Duration) error { return c.now().Sleep(ctx, d) }

// restart stops the daemon and boots it again (reboot).
func (r *stepRecordsRig) restart() {
	r.t.Helper()
	r.restartWith(func() {})
}

// restartWith is restart with between run on the store while no daemon runs, which is where a
// deploy leaves what the daemon of the release before wrote and the next one boots on.
func (r *stepRecordsRig) restartWith(between func()) {
	r.t.Helper()
	r.d.stop()
	between()
	r.reboot()
}

// reboot boots the stopped daemon again on the same store, the same Dispatch and the same ports.
// Each pane outlives the daemon, as a tmux pane does: the new daemon re-observes each live claim
// rather than launching it again, and each pane's shim connects to it with the boot token it has
// held since its launch, its turn and its session as they were.
func (r *stepRecordsRig) reboot() {
	r.t.Helper()
	rebindHeldPorts(r.t, &r.cfg)
	r.boot()
	for _, c := range r.claims {
		c.reconnect()
	}
}

// plan is the planner taking its task, answering, writing its plan to Dispatch and completing.
func (r *stepRecordsRig) plan() {
	r.t.Helper()
	r.work(claim.RolePlanner, "plan", api.HandoffCompleteRequest{Summary: "planned"}, phase.Implementing)
}

// implement is one round of the implementer on the pull request's head.
func (r *stepRecordsRig) implement() {
	r.t.Helper()
	r.work(claim.RoleImplementer, "implement", api.HandoffCompleteRequest{Summary: "implemented", Commit: stepRecordsHead}, phase.Testing)
}

// testCompletion is the tester's completion of a round it passes.
var testCompletion = api.HandoffCompleteRequest{Summary: "every criterion holds", Verdict: "pass"}

// test is one round of the tester, which passes.
func (r *stepRecordsRig) test() {
	r.t.Helper()
	r.work(claim.RoleTester, "test", testCompletion, phase.Reviewing)
}

// retro is the implementer's retro rating the plan 4 (rate).
func (r *stepRecordsRig) retro() {
	r.t.Helper()
	r.rate(&record.PlanRating{Rating: 4, Reason: "The plan fit the work."})
}

// rate is the implementer's retro, whose completion names no record, the head it leaves, as each
// completion of the implementer's does, and the rating of the plan with its reason, which a pane
// at api.PlanRatingContract or later must give (PLAN_RATING_REQUIRED). A nil rating is a retro
// with none, a pane's of the plugin before that contract.
func (r *stepRecordsRig) rate(rating *record.PlanRating) {
	r.t.Helper()
	r.work(claim.RoleImplementer, "", r.retroCompletion(rating), phase.Merging)
}

// retroCompletion is the implementer's completion of its retro with rating and its reason, and with
// neither for a nil rating.
func (r *stepRecordsRig) retroCompletion(rating *record.PlanRating) api.HandoffCompleteRequest {
	req := api.HandoffCompleteRequest{Summary: "retro written", Commit: stepRecordsHead}
	if rating != nil {
		score, reason := rating.Rating, rating.Reason
		req.PlanRating, req.PlanReason = &score, &reason
	}
	return req
}

// work is one role's turn of the walk: it takes the task, its agent's model answering (c.model),
// and finishes it.
func (r *stepRecordsRig) work(role claim.Role, word string, req api.HandoffCompleteRequest, next phase.Phase) {
	r.t.Helper()
	c := r.worker(role)
	c.take()
	r.finish(c, word, req, next)
}

// finish is the end of c's turn: it writes its handoff record of word to Dispatch under its own
// session (none for word ""), completes, ends its turn, and waits for the issue to reach next.
func (r *stepRecordsRig) finish(c *stepClaimant, word string, req api.HandoffCompleteRequest, next phase.Phase) {
	t := r.t
	t.Helper()
	if word != "" {
		req.HandoffID, req.HandoffVersion = r.writeHandoff(c.session, word)
	}
	if status, body := r.complete(c, req); status != http.StatusOK {
		t.Fatalf("the %s's completion = %d %s, want 200", c.role, status, body)
	}
	c.endTurn()
	r.waitPhase(next)
}

// complete posts c's completion req with a grant minted for it now, and answers the daemon's
// status and body.
func (r *stepRecordsRig) complete(c *stepClaimant, req api.HandoffCompleteRequest) (int, string) {
	r.t.Helper()
	req.GrantID = c.grant()
	status, body := r.d.request(http.MethodPost, "/legion/v1/handoff/complete", req, false)
	return status, string(body)
}

// review is the reviewer taking its task and answering, and then a person submitting state on the
// walk's pull request (publishReview); the issue then reaches next.
func (r *stepRecordsRig) review(state string, next phase.Phase) {
	r.t.Helper()
	c := r.worker(claim.RoleReviewer)
	c.take()
	c.endTurn()
	r.publishReview(r.number, state)
	r.waitPhase(next)
}

// publishReview is a person with write access submitting state on pull request number at
// stepRecordsHead, as Envoy relays GitHub's review.
func (r *stepRecordsRig) publishReview(number int, state string) {
	t := r.t
	t.Helper()
	r.reviews++
	payload, err := json.Marshal(map[string]string{
		"action": "submitted", "kind": "review", "repo": liveRepo, "number": strconv.Itoa(number), "state": state,
		"commit_id": stepRecordsHead, "head_sha": stepRecordsHead, "author": stepRecordsPerson, "body": "round " + strconv.Itoa(r.reviews),
		"review_id": strconv.Itoa(number*10 + r.reviews), "submitted_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatal(err)
	}
	subject := fmt.Sprintf("notifications.github.%s.pr.%d.review", r.subjectRepo(), number)
	r.publish(subject, "github", fmt.Sprintf("review-%d-%d", number, r.reviews), string(payload))
}

// accept is the tree architect answering and signing off the walk's issue (signOff).
func (r *stepRecordsRig) accept() {
	r.t.Helper()
	r.signOff(r.key)
}

// signOff is the tree architect answering in a turn of its own and signing off issue, a member of
// its tree, through its own route; issue then reaches its retro.
func (r *stepRecordsRig) signOff(issue string) {
	t := r.t
	t.Helper()
	c := r.worker(claim.RoleArchitect)
	c.ownTurn()
	status, body := r.d.request(http.MethodPost, "/legion/v1/signoff", map[string]any{"grantId": c.grant(), "issue": issue}, false)
	if status != http.StatusOK {
		t.Fatalf("the architect's sign-off of %s = %d %s, want 200", issue, status, body)
	}
	r.waitPhaseOf(issue, phase.Retro)
}

// merge waits for the merger's launch and publishes GitHub's merge of the pull request
// (publishMergeOf), which ends the walk at done.
func (r *stepRecordsRig) merge() {
	r.t.Helper()
	r.awaitLaunch(r.token(claim.RoleMerger))
	r.github.merged(r.number)
	r.publishMergeOf(r.number, r.key, stepRecordsHead, "main")
	r.waitPhase(phase.Done)
}

// waitPhase waits for the issue to stand in want.
func (r *stepRecordsRig) waitPhase(want phase.Phase) {
	r.t.Helper()
	r.waitPhaseOf(r.key, want)
}

// waitPhaseOf waits for issue to stand in want.
func (r *stepRecordsRig) waitPhaseOf(issue string, want phase.Phase) {
	r.t.Helper()
	var last string
	if !testwait.Holds(stepRecordsWait, 100*time.Millisecond, func() bool {
		got, ok := r.d.state().Issues[issue]
		last = string(got.Phase)
		return ok && last == string(want)
	}) {
		r.t.Fatalf("%s stands in %q, want %s", issue, last, want)
	}
}

// token is the claim token of role on the walk's issue, as the daemon spells it.
func (r *stepRecordsRig) token(role claim.Role) claim.Token {
	r.t.Helper()
	return r.tokenOf(r.key, role)
}

// tokenOf is the claim token of role on issue, as the daemon spells it.
func (r *stepRecordsRig) tokenOf(issue string, role claim.Role) claim.Token {
	r.t.Helper()
	token, err := claim.NewToken(r.project, issue, role)
	if err != nil {
		r.t.Fatal(err)
	}
	return token
}

// awaitLaunch waits for the running daemon to launch token's claim.
func (r *stepRecordsRig) awaitLaunch(token claim.Token) {
	r.t.Helper()
	testwait.WithinPolling(r.t, stepRecordsWait, 50*time.Millisecond, "the daemon's launch of "+string(token), func() bool {
		for _, call := range r.rt.Calls() {
			if (call.Method == "Spawn" || call.Method == "Resume") && call.Spec.Claim == token {
				return true
			}
		}
		return false
	})
}

// person sends one request to Dispatch as the run's person, through the identity header the
// scratch server trusts, requires want, and decodes the answer into into when it is not nil.
func (r *stepRecordsRig) person(method, path string, body any, want int, into any) {
	t := r.t
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(method, r.dispatch.base+path, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", "step-records")
	status, answer := r.send(request)
	if status != want {
		t.Fatalf("%s %s = %d %s, want %d", method, path, status, answer, want)
	}
	if into != nil {
		if err := json.Unmarshal(answer, into); err != nil {
			t.Fatalf("decode %s %s: %v", method, path, err)
		}
	}
}

// personMoves is a person setting the walk's issue, titled title, to status in Dispatch. Dispatch
// then relays the event of that write, of type kind. The daemon's consumer reads it as it reads any
// issue event.
func (r *stepRecordsRig) personMoves(status, kind, title string) {
	t := r.t
	t.Helper()
	var moved createdIssue
	r.person(http.MethodPatch, "/api/v1/issues/"+r.key, map[string]any{"status": status}, http.StatusOK, &moved)
	payload, err := json.Marshal(map[string]any{
		"id": moved.LastSeq, "issue_key": r.key, "project": r.cfg.Project, "seq": moved.LastSeq, "type": kind,
		"actor": map[string]string{"kind": "user", "id": "a-person"}, "notify": true, "created_at": time.Now().UTC().Format(time.RFC3339Nano),
		"payload": map[string]any{
			"key": r.key, "status": status, "title": title, "parent": nil, "rank": moved.Rank,
			"labels": liveDispatchLabels(true), "last_seq": moved.LastSeq, "project": r.cfg.Project, "resolved_repo": liveRepo,
		},
	})
	if err != nil {
		t.Fatalf("encode the Dispatch event of %s: %v", r.key, err)
	}
	r.publish("notifications.dispatch.issue."+r.key+"."+kind, "dispatch", fmt.Sprintf("%s-%s-%d", r.key, status, moved.LastSeq), string(payload))
}

// writeHandoff writes the issue's handoff record of word under session, as `dispatch
// handoff-write` does with the agent's bearer, and answers its id and version.
func (r *stepRecordsRig) writeHandoff(session, word string) (string, int) {
	t := r.t
	t.Helper()
	body := fmt.Sprintf(`{"schemaVersion": 1, "phase": %q, "issue": %q, "summary": "written by %s"}`, word, r.key, session)
	encoded, err := json.Marshal(map[string]any{"actor": map[string]string{"kind": "session", "id": session}, "body": body})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, r.dispatch.base+"/api/v1/issues/"+r.key+"/handoffs/"+word, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+r.dispatch.token)
	status, answer := r.send(request)
	if status != http.StatusCreated {
		t.Fatalf("write the %s handoff of %s as %s = %d %s, want 201", word, r.key, session, status, answer)
	}
	var written struct {
		ID      string `json:"id"`
		Version int    `json:"version"`
	}
	if err := json.Unmarshal(answer, &written); err != nil || written.ID == "" {
		t.Fatalf("decode the written handoff %s: %v", answer, err)
	}
	return written.ID, written.Version
}

func (r *stepRecordsRig) send(request *http.Request) (int, []byte) {
	r.t.Helper()
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		r.t.Fatalf("%s %s: %v", request.Method, request.URL.Path, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		r.t.Fatalf("read %s %s: %v", request.Method, request.URL.Path, err)
	}
	return response.StatusCode, answer
}

// readSteps is GET /api/v1/issues/{key}/steps of the walk's issue as a person of the project reads
// it, or the error that the read met.
func (r *stepRecordsRig) readSteps() (stepRecordsRead, error) { return r.readStepsOf(r.key) }

// readStepsOf is GET /api/v1/issues/{issue}/steps as a person of the project reads it, or the
// error that the read met.
func (r *stepRecordsRig) readStepsOf(issue string) (stepRecordsRead, error) {
	var read stepRecordsRead
	if err := r.personRead("/api/v1/issues/"+issue+"/steps", &read); err != nil {
		return stepRecordsRead{}, err
	}
	return read, nil
}

// personRead is GET path on Dispatch as a person of the project reads it, decoded into into, or
// the error that the read met.
func (r *stepRecordsRig) personRead(path string, into any) error {
	request, err := http.NewRequest(http.MethodGet, r.dispatch.base+path, nil)
	if err != nil {
		return err
	}
	request.Header.Set("X-Dispatch-User", "step-records")
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		answer, _ := io.ReadAll(response.Body)
		return fmt.Errorf("GET %s = %d %s", path, response.StatusCode, answer)
	}
	return json.NewDecoder(response.Body).Decode(into)
}

// waitSteps waits for the walk's issue's read to satisfy hold, and fails with hold's last answer.
func (r *stepRecordsRig) waitSteps(what string, hold func(stepRecordsRead) error) {
	r.t.Helper()
	r.waitStepsOf(r.key, what, hold)
}

// waitStepsOf waits for issue's read to satisfy hold, and fails with hold's last answer.
func (r *stepRecordsRig) waitStepsOf(issue, what string, hold func(stepRecordsRead) error) {
	r.t.Helper()
	waitRead(r, what, "Dispatch", time.Second, func() (stepRecordsRead, error) { return r.readStepsOf(issue) }, hold)
}

// waitRead waits for read to answer something that satisfies hold, reading again every interval,
// and fails naming where the read goes and hold's last answer.
func waitRead[T any](r *stepRecordsRig, what, where string, interval time.Duration, read func() (T, error), hold func(T) error) {
	r.t.Helper()
	var last error
	if !testwait.Holds(stepRecordsWait, interval, func() bool {
		got, err := read()
		if err != nil {
			last = err
			return false
		}
		last = hold(got)
		return last == nil
	}) {
		r.t.Fatalf("%s never reached %s: %v", what, where, last)
	}
}

// deleteNode deletes node id, a step record or a plan, from Dispatch's own store, as the store
// holds it: its node and every tuple that names it.
func (r *stepRecordsRig) deleteNode(id string) {
	t := r.t
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, r.dispatch.database)
	if err != nil {
		t.Fatalf("connect to Dispatch's database: %v", err)
	}
	defer conn.Close(ctx)
	tag, err := conn.Exec(ctx, "delete from workflow_tuples where from_id = $1 or to_id = $1", id)
	if err != nil || tag.RowsAffected() == 0 {
		t.Fatalf("delete the tuples of %s: %d rows, %v", id, tag.RowsAffected(), err)
	}
	if tag, err = conn.Exec(ctx, "delete from workflow_nodes where id = $1", id); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("delete the node %s: %d rows, %v", id, tag.RowsAffected(), err)
	}
}

// unfinishedRows is how many rows of kind of the issue the daemon's outbox holds unfinished.
func (r *stepRecordsRig) unfinishedRows(kind record.OutboxKind) int {
	r.t.Helper()
	var count int
	if err := r.st.Pool().QueryRow(context.Background(),
		"select count(*) from outbox where issue = $1 and kind = $2", r.key, string(kind)).Scan(&count); err != nil {
		r.t.Fatalf("count the issue's %s rows in the outbox: %v", kind, err)
	}
	return count
}

// The messages of the daemon's two lines about its reads of an agent's calls. The test spells the
// words itself, so a change to the daemon's words fails it.
const (
	// usedReadMessage is the line of each read a step's start or end used, logged at the fact's
	// commit.
	usedReadMessage = "workflow: a step used a read of its agent's calls"
	// emptyReadMessage is the line of each read that gave no totals, logged at once.
	emptyReadMessage = "workflow: a read of an agent's calls gave no totals"
)

// usedRead is one line the daemon logs at the commit of a fact, for each read a step's start or
// end used. EndedBy is set on an end's line alone.
type usedRead struct {
	Step        string                `json:"step"`
	Issue       string                `json:"issue"`
	Task        string                `json:"task"`
	Kind        string                `json:"kind"`
	Place       int                   `json:"place"`
	StartedAt   string                `json:"startedAt"`
	Side        string                `json:"side"`
	Claim       string                `json:"claim"`
	Generation  uint64                `json:"generation"`
	Read        uint64                `json:"read"`
	Final       bool                  `json:"final"`
	SetModel    string                `json:"setModel"`
	Counts      []shimwire.ModelCount `json:"counts"`
	OutsideForm uint64                `json:"outsideForm"`
	Overflowed  bool                  `json:"overflowed"`
	ReadAt      time.Time             `json:"readAt"`
	EndedBy     string                `json:"endedBy"`
}

// emptyRead is one line the daemon logs when a read of an agent's calls gave no totals. It names
// the launch read, why, and how long the read waited.
type emptyRead struct {
	Claim      string        `json:"claim"`
	Generation uint64        `json:"generation"`
	Reason     string        `json:"reason"`
	Waited     time.Duration `json:"waited"`
}

// usedReads is each line of the daemon's log for a read a step's start or end used, in log order.
func (r *stepRecordsRig) usedReads() []usedRead {
	r.t.Helper()
	return logLines[usedRead](r, usedReadMessage)
}

// emptyReads is each line of the daemon's log for a read that gave no totals, in log order.
func (r *stepRecordsRig) emptyReads() []emptyRead {
	r.t.Helper()
	return logLines[emptyRead](r, emptyReadMessage)
}

// logLines is each line of the rig's log whose message is msg, decoded into a T, in log order. A
// line of msg that does not decode fails the test.
func logLines[T any](r *stepRecordsRig, msg string) []T {
	r.t.Helper()
	var lines []T
	for line := range strings.Lines(r.logs.String()) {
		var entry struct {
			Msg string `json:"msg"`
		}
		if json.Unmarshal([]byte(line), &entry) != nil || entry.Msg != msg {
			continue
		}
		var decoded T
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			r.t.Fatalf("decode the log line %q: %v", line, err)
		}
		lines = append(lines, decoded)
	}
	return lines
}

// stepRecordsRead is the read of an issue's steps, as Dispatch answers it.
type stepRecordsRead struct {
	Issue string `json:"issue"`
	Tasks []struct {
		Task  string           `json:"task"`
		Steps []stepRecordRead `json:"steps"`
	} `json:"tasks"`
}

// stepRecordRead is one record of the read.
type stepRecordRead struct {
	ID         string     `json:"id"`
	Task       string     `json:"task"`
	Step       int        `json:"step"`
	Kind       string     `json:"kind"`
	Role       string     `json:"role"`
	Type       string     `json:"type"`
	Agent      string     `json:"agent"`
	Session    string     `json:"session"`
	Model      string     `json:"model"`
	Generation uint64     `json:"generation"`
	Attempt    int        `json:"attempt"`
	StartedAt  *time.Time `json:"started_at"`
	EndedAt    *time.Time `json:"ended_at"`
	Result     string     `json:"result"`
	ReturnedTo *int       `json:"returned_to"`
	// SetModel is the model the record's session was set to, "" for none.
	SetModel string `json:"set_model"`
	// Answers is each model that answered the record, with its calls. It is nil where the record
	// does not know them, and empty for a step with no call.
	Answers *[]stepAnswerRead `json:"answers"`
}

// stepAnswerRead is one model of a record's answers and the calls it answered.
type stepAnswerRead struct {
	Model string `json:"model"`
	Calls int64  `json:"calls"`
}

// records is every record of the read, task by task.
func (read stepRecordsRead) records() []stepRecordRead {
	var all []stepRecordRead
	for _, task := range read.Tasks {
		all = append(all, task.Steps...)
	}
	return all
}

// find is the record of kind, step and attempt, the zero record when the read holds none.
func (read stepRecordsRead) find(kind string, step, attempt int) stepRecordRead {
	for _, record := range read.records() {
		if record.Kind == kind && record.Step == step && record.Attempt == attempt {
			return record
		}
	}
	return stepRecordRead{}
}

// stepWant is one record a read must hold.
type stepWant struct {
	kind              string
	step, attempt     int
	role, typ, result string
	returnedTo        *int
	// owner, when set, is whose the record is exactly: its agent, its session and its model, an
	// empty session and model meaning none (the merger the merge launches never registers, and a run
	// whose end the roll forward of a rollback wrote). Unset, the record is any agent's, and one of
	// kind step names a session and stepRecordsModel.
	owner *stepOwner
}

// stepOwner is whose a record is: the claim token of its agent, that claim's session and the
// model that answered it.
type stepOwner struct {
	agent, session, model string
}

// withOwners is want with owners[i] the owner of want[i], want itself left as it is; it fails the
// test, naming both counts, when owners is not one owner for each record of want.
func withOwners(t *testing.T, want []stepWant, owners ...*stepOwner) []stepWant {
	t.Helper()
	if len(owners) != len(want) {
		t.Fatalf("%d owners for %d records, want one for each, owners[i] want[i]'s", len(owners), len(want))
	}
	owned := slices.Clone(want)
	for i := range owned {
		owned[i].owner = owners[i]
	}
	return owned
}

// stepsHold says whether read holds exactly the records of want, each of the task default, ended,
// with its role, type, agent, start, end, result and return; each of an owner named for that owner
// exactly (owner), and each other of kind step with its session and the model that answered it.
// The error names the first record that is missing or wrong.
func stepsHold(read stepRecordsRead, want []stepWant) error {
	for _, w := range want {
		got := read.find(w.kind, w.step, w.attempt)
		name := fmt.Sprintf("%s %d attempt %d", w.kind, w.step, w.attempt)
		switch {
		case got.ID == "":
			return fmt.Errorf("the read holds no record of %s", name)
		case got.Task != "default" || got.Role != w.role || got.Type != w.typ || got.Agent == "":
			return fmt.Errorf("%s is task %q, role %q, type %q, agent %q; want task default, role %q, type %q and an agent", name, got.Task, got.Role, got.Type, got.Agent, w.role, w.typ)
		case got.StartedAt == nil || got.EndedAt == nil || got.Result != w.result:
			return fmt.Errorf("%s started %v, ended %v with result %q; want a start, an end and %q", name, got.StartedAt, got.EndedAt, got.Result, w.result)
		case (got.ReturnedTo == nil) != (w.returnedTo == nil) || got.ReturnedTo != nil && *got.ReturnedTo != *w.returnedTo:
			return fmt.Errorf("%s returned to %v, want %v", name, got.ReturnedTo, w.returnedTo)
		case w.owner != nil && (got.Agent != w.owner.agent || got.Session != w.owner.session || got.Model != w.owner.model):
			return fmt.Errorf("%s names agent %q, session %q and model %q; want %q, %q and %q", name, got.Agent, got.Session, got.Model, w.owner.agent, w.owner.session, w.owner.model)
		case w.kind == "step" && w.owner == nil && (got.Session == "" || got.Model != stepRecordsModel):
			return fmt.Errorf("%s names session %q and model %q, want its agent's session and %s", name, got.Session, got.Model, stepRecordsModel)
		}
	}
	if got := len(read.records()); got != len(want) {
		return fmt.Errorf("the read holds %d records, want %d", got, len(want))
	}
	return nil
}

// stepClaimant is one role's claim as its pane reaches the daemon: the shim connected with the
// launch's boot token, the session registered and ready, and a reader that answers what the daemon
// asks of the shim and hands each prompt to the walk, starting its turn as Oh My Pi does — but the
// architect's, whose pane works each of its tasks in a turn of its own (workAlone).
type stepClaimant struct {
	r     *stepRecordsRig
	token claim.Token
	issue string
	// tree is the tree of issue: the walk's issue when the claimant started, of which issue is
	// that issue itself or a child the walk seeded under it.
	tree      string
	role      claim.Role
	session   string
	secret    string
	bootToken string
	prompts   chan shimwire.Prompt
	mu        sync.Mutex
	// model is the model the agent's answers name, `<provider>/<model>`: stepRecordsModel until
	// its test gives the agent another.
	model string
	// sh is the shim's connection to the daemon now running, which a restart replaces.
	sh *shim
	// open is the id of each prompt whose turn has started and not ended.
	open []string
	// inTurn is whether the session is in a turn the walk began on it (send's agent_start, until
	// its agent_end): a task the daemon sends meanwhile joins that turn (workAlone).
	inTurn bool
	// setModel is the launch's set model, which each read of its totals reports. It is
	// stepRecordsModel at creation, and serve reads it under mu.
	setModel string
	// counts, outsideForm and overflowed are the launch's calls, counted by the shim's rule
	// (count). read numbers the reads answered and the final totals. mu guards all four, and
	// relaunched sets each back to zero.
	counts      map[string]uint64
	outsideForm uint64
	overflowed  bool
	read        uint64
	// holdReads, while true, has serve answer no read of the totals. serve still reads every
	// frame.
	holdReads atomic.Bool
	// hold is armed by holdTheSocket and spent by the next get_state, whose answer starts the
	// socket hold (holdSocket).
	hold atomic.Bool
	// held closes once the socket hold has read its 4096 bytes. holdTheSocket makes it anew under
	// mu.
	held chan struct{}
	// cut is the last connection the test closed itself (closeConn). A write that fails on it
	// fails no test.
	cut atomic.Pointer[shim]
}

// worker is role's claimant on the walk's issue (workerOf).
func (r *stepRecordsRig) worker(role claim.Role) *stepClaimant {
	r.t.Helper()
	return r.workerOf(r.key, role)
}

// workerOf is role's claimant on issue, an issue of the tree of the walk's issue: the one already
// registered, or the launch the daemon makes now, registered and ready under a session of its own.
// Its shim counts the agent's calls (dialCountingShim).
func (r *stepRecordsRig) workerOf(issue string, role claim.Role) *stepClaimant {
	t := r.t
	t.Helper()
	token := r.tokenOf(issue, role)
	if c, ok := r.claims[token]; ok {
		return c
	}
	r.awaitLaunch(token)
	launch := lastLaunch(t, r.rt, token)
	r.built.mu.Lock()
	address := r.built.address
	r.built.mu.Unlock()
	c := &stepClaimant{r: r, token: token, issue: issue, tree: r.key, role: role, session: "ses_" + string(role) + "_" + randomSuffix(t)[:6], model: stepRecordsModel,
		setModel: stepRecordsModel, bootToken: launch.BootToken, sh: dialCountingShim(t, address, launch.BootToken), prompts: make(chan shimwire.Prompt, 8)}
	c.secret = c.register(launch.BootToken)
	sh, d := c.sh, r.d
	sh.serve(func() { c.serve(sh, d) })
	c.ready(launch.Generation)
	r.claims[token] = c
	return c
}

// reconnect is the pane's shim connecting to the daemon that runs now, as it does after the daemon
// restarts. It uses the boot token it has held since its launch, and says again that it counts the
// agent's calls.
func (c *stepClaimant) reconnect() {
	c.r.built.mu.Lock()
	address := c.r.built.address
	c.r.built.mu.Unlock()
	sh := dialCountingShim(c.r.t, address, c.bootToken)
	c.mu.Lock()
	c.sh = sh
	c.mu.Unlock()
	d := c.r.d
	sh.serve(func() { c.serve(sh, d) })
}

// serve reads every frame the daemon d sends sh until the connection closes or the test ends it
// (shim.serve): it answers the protocol negotiation and the state probe, and acknowledges each
// prompt and hands it to the walk, whose take starts its turn. An answer that fails to write
// fails the test, unless the test had begun stopping d by then: the pane's connection outlives
// the daemon, as a tmux pane does, so a restart's stop closes it under whatever answer is on
// its way, which is no fault of the daemon's.
//
// It also answers each read of the totals, unless holdReads, and starts the socket hold
// holdTheSocket armed. A write that fails on a connection the test closed itself fails nothing
// (cut).
func (c *stepClaimant) serve(sh *shim, d *daemon) {
	reply := func(frame shimwire.Frame) {
		if err := sh.writer.WriteFrame(frame); err != nil && !d.stopped.Load() && c.cut.Load() != sh {
			c.r.t.Errorf("send %s for %s: %v", frame.FrameType(), c.token, err)
		}
	}
	reader := sh.lines
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
			reply(shimwire.Response{ID: f.ID, Command: shimwire.TypeNegotiateProtocol, Success: true})
		case shimwire.GetState:
			c.mu.Lock()
			streaming := len(c.open) > 0
			c.mu.Unlock()
			data, _ := json.Marshal(shimwire.StateData{IsStreaming: &streaming})
			reply(shimwire.Response{ID: f.ID, Command: shimwire.TypeGetState, Success: true, Data: data})
			if c.hold.CompareAndSwap(true, false) {
				c.holdSocket(sh)
				return
			}
		case shimwire.ModelTotals:
			if !c.holdReads.Load() {
				reply(c.readTotals(f.ID))
			}
		case shimwire.Prompt:
			reply(shimwire.Response{ID: f.ID, Command: shimwire.TypePrompt, Success: true})
			if c.workAlone(reply, f) {
				continue
			}
			c.mu.Lock()
			c.open = append(c.open, f.ID)
			c.mu.Unlock()
			select {
			case c.prompts <- f:
			case <-sh.stopping:
				return
			}
		}
	}
}

// workAlone is the architect's pane working prompt in a turn of its own that ends, as Oh My Pi
// runs a task the daemon sends a session in no turn: the turn's agent_start naming its delivery,
// its agent_end and the prompt's result, each through reply. It answers false and writes nothing
// for every other role, whose task the walk takes (take) and whose turn the walk ends (finish),
// and for a prompt that arrives while a turn the walk began on the architect's session is open
// (inTurn), which joins that turn as the pane would run it.
//
// The walk takes no task of the architect's: its steps are turns the walk begins itself (ownTurn).
// A turn a task of the architect's began and nothing ended, or the architect's send in flight, is
// what the daemon reads as the issue's shared working copy in use (supervise's IssueInTurn), so the
// issue's next phase task waited on it, and only a turn's end wakes that task (IssueTurnEnded):
// the first step of a root whose gate opened as its architect's start task went out waited for
// ever. The model answers nothing in this turn, so a record of the architect's step names the
// model of the walk's own turn. The check of inTurn and the three frames are one hold of c.mu,
// which send takes around the walk's agent_start and agent_end, so the walk's turn never begins
// or ends inside this one.
func (c *stepClaimant) workAlone(reply func(shimwire.Frame), prompt shimwire.Prompt) bool {
	if c.role != claim.RoleArchitect {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inTurn {
		return false
	}
	reply(shimwire.AgentStart{DeliveryID: prompt.DeliveryID})
	reply(shimwire.AgentEnd{})
	reply(shimwire.PromptResult{ID: prompt.ID, AgentInvoked: true})
	return true
}

// send writes one frame to the daemon that runs now. An agent_start begins the walk's turn on the
// session before it is written and an agent_end ends it once it is (inTurn), so no turn of the
// architect's pane (workAlone) lands between the two.
func (c *stepClaimant) send(frame shimwire.Frame) {
	_, starts := frame.(shimwire.AgentStart)
	_, ends := frame.(shimwire.AgentEnd)
	c.mu.Lock()
	c.inTurn = c.inTurn || starts
	sh := c.sh
	c.mu.Unlock()
	if err := sh.writer.WriteFrame(frame); err != nil {
		c.r.t.Errorf("send %s for %s: %v", frame.FrameType(), c.token, err)
	}
	if ends {
		c.mu.Lock()
		c.inTurn = false
		c.mu.Unlock()
	}
}

// take waits for the daemon's next task of this claim and starts the turn that works it, its
// model answering first: the claim's machine reads a connection's frames in order, so once the
// turn's start shows the claim working, the answer before it has been read too. It answers the
// prompt that carried the task, which a walk reads where it holds what the claim was told.
func (c *stepClaimant) take() shimwire.Prompt {
	c.r.t.Helper()
	prompt := c.receive()
	c.answer()
	c.send(shimwire.AgentStart{DeliveryID: prompt.DeliveryID})
	c.waitState(supervise.StateWorking)
	return prompt
}

// takeQuietly is take with no answer of the model, so the turn starts and nothing is counted.
func (c *stepClaimant) takeQuietly() shimwire.Prompt {
	c.r.t.Helper()
	prompt := c.receive()
	c.send(shimwire.AgentStart{DeliveryID: prompt.DeliveryID})
	c.waitState(supervise.StateWorking)
	return prompt
}

// receive waits for the daemon's next task of this claim, the prompt the pane has acknowledged
// and holds until a turn works it.
func (c *stepClaimant) receive() shimwire.Prompt {
	c.r.t.Helper()
	select {
	case prompt := <-c.prompts:
		return prompt
	case <-time.After(stepRecordsWait):
		c.r.t.Fatalf("%s was prompted with no task within %s", c.token, stepRecordsWait)
		return shimwire.Prompt{}
	}
}

// endTurnUnseen ends every turn a prompt started with no daemon of this release there to read it,
// as the earlier release runs the agent after a rollback: the pane holds no open turn when its
// shim next connects, so the daemon's state probe finds it idle.
func (c *stepClaimant) endTurnUnseen() {
	c.mu.Lock()
	c.open, c.inTurn = nil, false
	c.mu.Unlock()
}

// ownTurn is a turn of the agent's own in which its model answers: it returns once the claim is
// out of it, so the answer, read before the turn's end, is the claim's last.
func (c *stepClaimant) ownTurn() {
	c.r.t.Helper()
	c.send(shimwire.AgentStart{})
	c.waitState(supervise.StateWorking)
	c.answer()
	c.send(shimwire.AgentEnd{})
	c.waitState(supervise.StateIdle)
}

// waitState waits for the daemon to hold the claim in state.
func (c *stepClaimant) waitState(state supervise.ClaimState) {
	c.r.t.Helper()
	var last string
	if !testwait.Holds(stepRecordsWait, 20*time.Millisecond, func() bool {
		last = c.r.d.claim(c.token).State
		return last == string(state)
	}) {
		c.r.t.Fatalf("%s is %s, want %s", c.token, last, state)
	}
}

// answer is the agent's model (c.model, `<provider>/<model>`) answering, which Oh My Pi's
// message_end names as message.provider and message.model. It counts the call first, as the shim
// counts each frame before the daemon reads it.
func (c *stepClaimant) answer() {
	t := c.r.t
	t.Helper()
	provider, model, _ := strings.Cut(c.model, "/")
	frame, err := json.Marshal(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "provider": provider, "model": model}})
	if err != nil {
		t.Fatal(err)
	}
	c.count(provider, model)
	c.send(shimwire.Raw{Type: "message_end", JSON: frame})
}

// endTurn ends every turn a prompt started: the agent_end, and the result of each prompt.
func (c *stepClaimant) endTurn() {
	c.mu.Lock()
	open := c.open
	c.open = nil
	c.mu.Unlock()
	if len(open) == 0 {
		return
	}
	c.send(shimwire.AgentEnd{})
	for _, id := range open {
		c.send(shimwire.PromptResult{ID: id, AgentInvoked: true})
	}
}

// grant is a capability grant minted for this session now, as its `legion` tool mints one per
// operation.
func (c *stepClaimant) grant() string {
	t := c.r.t
	t.Helper()
	status, body := c.r.d.request(http.MethodPost, "/legion/v1/grants",
		api.GrantRequest{SessionID: c.session, Secret: c.secret, Tree: c.tree, Issue: c.issue}, false)
	if status != http.StatusOK {
		t.Fatalf("the grant of %s = %d %s, want 200", c.token, status, body)
	}
	var granted api.GrantResponse
	if err := json.Unmarshal(body, &granted); err != nil {
		t.Fatalf("decode the grant of %s: %v", c.token, err)
	}
	return granted.GrantID
}

// register registers the claimant's session with the launch of bootToken, as its plugin does, and
// answers the secret the daemon gave it.
func (c *stepClaimant) register(bootToken string) string {
	t := c.r.t
	t.Helper()
	status, body := c.r.d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: bootToken, SessionID: c.session, OmpSessionFile: "/sessions/" + c.session + ".jsonl",
		AgentID: c.session, PluginContract: api.DaemonAPIVersion,
	}, false)
	if status != http.StatusOK {
		t.Fatalf("register %s = %d %s, want 200", c.token, status, body)
	}
	var registered claim.RegisterResponse
	if err := json.Unmarshal(body, &registered); err != nil {
		t.Fatalf("decode the registration of %s: %v", c.token, err)
	}
	return registered.Secret
}

// ready is the claimant's agent saying it is ready at generation, its launch's.
func (c *stepClaimant) ready(generation uint64) {
	t := c.r.t
	t.Helper()
	if status, body := c.r.d.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: c.token, SessionID: c.session, Secret: c.secret, Generation: generation,
	}, false); status != http.StatusNoContent {
		t.Fatalf("ready %s = %d %s, want 204", c.token, status, body)
	}
}

// count counts one call that provider and model answered, by the shim's rule (internal/shim's
// totals.call). A model outside the form raises outsideForm, and a new model past
// shimwire.MaxTotalsModels sets overflowed.
func (c *stepClaimant) count(provider, model string) {
	if model == "" || model == shimwire.SyntheticModel {
		return
	}
	name := model
	if provider != "" {
		name = provider + "/" + model
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !steprecord.AgentTextHolds(name) {
		c.outsideForm++
		return
	}
	if _, counted := c.counts[name]; !counted && len(c.counts) >= shimwire.MaxTotalsModels {
		c.overflowed = true
		return
	}
	if c.counts == nil {
		c.counts = map[string]uint64{}
	}
	c.counts[name]++
}

// readTotals is the answer to the daemon's read id: the claimant's totals, the read number raised
// by 1.
func (c *stepClaimant) readTotals(id string) shimwire.ModelTotalsResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.read++
	totals := c.totalsLocked()
	return shimwire.ModelTotalsResult{ID: id, OK: true, Totals: &totals}
}

// totalsLocked is the claimant's totals as the wire carries them. The counts are sorted by model,
// and an empty list where no call came. The caller holds mu.
func (c *stepClaimant) totalsLocked() shimwire.Totals {
	counts := make([]shimwire.ModelCount, 0, len(c.counts))
	for model, calls := range c.counts {
		counts = append(counts, shimwire.ModelCount{Model: model, Calls: calls})
	}
	slices.SortFunc(counts, func(a, b shimwire.ModelCount) int { return strings.Compare(a.Model, b.Model) })
	return shimwire.Totals{Read: c.read, SetModel: c.setModel, Counts: counts, OutsideForm: c.outsideForm, Overflowed: c.overflowed}
}

// exit ends the launch as its harness exiting does. With final, the shim first sends its final
// totals unasked, the read number raised by 1. The fake then answers the next probe of this claim
// gone (fake.Runtime.ScriptProbeOf), and the connection closes, so the claim's machine probes and
// relaunches the claim. It returns once the daemon has launched the claim at a higher generation,
// which relaunched connects.
func (c *stepClaimant) exit(final bool) {
	t := c.r.t
	t.Helper()
	before := c.r.d.claim(c.token).Generation
	if final {
		c.mu.Lock()
		c.read++
		totals := c.totalsLocked()
		c.mu.Unlock()
		c.send(shimwire.ModelTotalsFinal{Totals: totals})
	}
	rt := c.r.rt
	rt.ScriptProbeOf(c.token, fake.ProbeResult{Kind: runtime.Gone})
	c.closeConn()
	testwait.WithinPolling(t, stepRecordsWait, 50*time.Millisecond, fmt.Sprintf("the launch of %s past generation %d", c.token, before), func() bool {
		return slices.ContainsFunc(rt.Calls(), func(call fake.Call) bool {
			return (call.Method == "Spawn" || call.Method == "Resume") && call.Spec.Claim == c.token && call.Spec.Generation > before
		})
	})
}

// relaunched is the pane of the launch exit waited for. A counting shim connects with the new
// launch's boot token. The agent registers in the same session and says ready at the new
// generation. The new launch counts from zero, its read number too. The daemon then sends the task
// the claim held again, which take takes.
func (c *stepClaimant) relaunched() {
	t := c.r.t
	t.Helper()
	launch := lastLaunch(t, c.r.rt, c.token)
	c.r.built.mu.Lock()
	address := c.r.built.address
	c.r.built.mu.Unlock()
	sh := dialCountingShim(t, address, launch.BootToken)
	c.bootToken, c.secret = launch.BootToken, c.register(launch.BootToken)
	c.mu.Lock()
	c.sh, c.open, c.inTurn = sh, nil, false
	c.counts, c.outsideForm, c.overflowed, c.read = nil, 0, false, 0
	c.mu.Unlock()
	d := c.r.d
	sh.serve(func() { c.serve(sh, d) })
	c.ready(launch.Generation)
}

// disconnect closes the shim's connection while its process lives on. No probe is scripted, so
// the fake answers alive. It returns once the daemon's connection directory holds no connection of
// the claim.
func (c *stepClaimant) disconnect() {
	t := c.r.t
	t.Helper()
	c.closeConn()
	testwait.WithinPolling(t, stepRecordsWait, 20*time.Millisecond, "the daemon's drop of the connection of "+string(c.token), func() bool {
		c.r.built.mu.Lock()
		conns := c.r.built.conns
		c.r.built.mu.Unlock()
		_, connected := conns.Conn(c.token)
		return !connected
	})
}

// closeConn closes the shim's connection now running, as a pane's exit or a broken socket does.
// Its serve then returns.
func (c *stepClaimant) closeConn() {
	c.mu.Lock()
	sh := c.sh
	c.mu.Unlock()
	c.cut.Store(sh)
	_ = sh.conn.Close()
}

// holdTheSocket arms the socket hold. serve answers the next get_state, reads 4096 more bytes and
// closes held (holdSocket). It then reads nothing more until the test ends, so the daemon's writes
// back up once the socket's buffers fill. held is made anew here, so a test reads it after the
// call.
func (c *stepClaimant) holdTheSocket() {
	c.mu.Lock()
	c.held = make(chan struct{})
	c.mu.Unlock()
	c.hold.Store(true)
}

// holdSocket is the socket hold on sh. It reads 4096 more bytes from sh's buffered reader, so
// bytes already buffered count. It then closes held and waits for the test's end of sh. A read
// that fails ends it with held open.
func (c *stepClaimant) holdSocket(sh *shim) {
	c.mu.Lock()
	held := c.held
	c.mu.Unlock()
	if _, err := io.ReadFull(sh.lines, make([]byte, 4096)); err != nil {
		return
	}
	close(held)
	<-sh.stopping
}

// dialCountingShim dials the worker stream at address as a shim that counts its launch's calls
// (Hello2.ModelTotals). It dials again after a refused hello until one is acked
// (dialHelloUntilAcked).
func dialCountingShim(t *testing.T, address, bootToken string) *shim {
	t.Helper()
	return dialHelloUntilAcked(t, address, shimwire.Hello2{BootToken: bootToken, ModelTotals: true})
}

// stepRecordsGitHub is GitHub's REST API as the walk's daemon reads it: every repository with
// default branch main, the pull request of each issue's branch at stepRecordsHead, clean until it
// merges, the person of the reviews with write access, and a GraphQL root that knows no pull
// request. Every other route is a 404, so nothing reaches github.com.
type stepRecordsGitHub struct {
	// t is the walk's test, which a merge of a pull request the walk never opened fails (merged).
	t   *testing.T
	url string
	mu  sync.Mutex
	// pulls is each pull request opened, in the order they were opened.
	pulls []stepRecordsPull
}

// stepRecordsPull is one pull request of the walk: its number, the branch it is open on, and
// whether GitHub has merged it.
type stepRecordsPull struct {
	number int
	branch string
	merged bool
}

func newStepRecordsGitHub(t *testing.T) *stepRecordsGitHub {
	t.Helper()
	g := &stepRecordsGitHub{t: t}
	g.url = githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		rest, isRepository := strings.CutPrefix(r.URL.Path, "/repos/"+liveRepo)
		pull, reviews := strings.CutSuffix(strings.TrimPrefix(rest, "/pulls/"), "/reviews")
		answer, known := "", false
		if number, err := strconv.Atoi(pull); err == nil && strings.HasPrefix(rest, "/pulls/") {
			answer, known = g.pullRequest(number)
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			_, _ = io.WriteString(w, `{"data":{"repository":{"pullRequest":null}}}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/") && strings.Count(r.URL.Path, "/") == 3:
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/repos/"), "/")
			_, _ = io.WriteString(w, `{"full_name":"`+parts[0]+"/"+parts[1]+`","default_branch":"main"}`)
		case r.Method == http.MethodGet && isRepository && rest == "/git/ref/heads/main":
			_, _ = io.WriteString(w, `{"ref":"refs/heads/main","object":{"sha":"base-main"}}`)
		case r.Method == http.MethodPost && isRepository && rest == "/git/refs":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodGet && isRepository && rest == "/pulls":
			_, _ = fmt.Fprintf(w, "[%s]", strings.Join(g.pullRequests(), ","))
		case r.Method == http.MethodGet && isRepository && known && !reviews:
			_, _ = io.WriteString(w, answer)
		case r.Method == http.MethodGet && isRepository && known && reviews:
			_, _ = io.WriteString(w, `[]`)
		case r.Method == http.MethodGet && isRepository && rest == "/collaborators/"+stepRecordsPerson+"/permission":
			_, _ = io.WriteString(w, `{"permission":"write","user":{"login":"`+stepRecordsPerson+`"}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
		}
	}})
	return g
}

// open is the pull request number of branch, open at stepRecordsHead.
func (g *stepRecordsGitHub) open(branch string, number int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pulls = append(g.pulls, stepRecordsPull{number: number, branch: branch})
}

// opened says whether the walk opened pull request number (find).
func (g *stepRecordsGitHub) opened(number int) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.find(number) != nil
}

// merged is GitHub's merge of pull request number: every read from now on answers it merged. A
// number the walk never opened fails the walk's test, naming the numbers it opened, and the
// package's other tests still run.
func (g *stepRecordsGitHub) merged(number int) {
	g.t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	pull := g.find(number)
	if pull == nil {
		opened := make([]int, len(g.pulls))
		for place, open := range g.pulls {
			opened[place] = open.number
		}
		g.t.Fatalf("GitHub merged pull request #%d, which the walk never opened; it opened %v", number, opened)
	}
	pull.merged = true
}

// find is the pull request number, nil where none was opened. Its caller holds g.mu.
func (g *stepRecordsGitHub) find(number int) *stepRecordsPull {
	for place := range g.pulls {
		if g.pulls[place].number == number {
			return &g.pulls[place]
		}
	}
	return nil
}

// pullRequests is each pull request as GitHub answers it now, in the order they were opened.
func (g *stepRecordsGitHub) pullRequests() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	answers := make([]string, len(g.pulls))
	for place, pull := range g.pulls {
		answers[place] = pull.answer()
	}
	return answers
}

// pullRequest is pull request number as GitHub answers it now, and whether it was opened.
func (g *stepRecordsGitHub) pullRequest(number int) (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	pull := g.find(number)
	if pull == nil {
		return "", false
	}
	return pull.answer(), true
}

// answer is the pull request as GitHub's REST API answers it: open at stepRecordsHead and clean
// until it merges, closed and merged from then on.
func (p stepRecordsPull) answer() string {
	state, merged, mergeable := "open", false, "clean"
	if p.merged {
		state, merged, mergeable = "closed", true, "unknown"
	}
	return fmt.Sprintf(`{"number":%d,"state":%q,"merged":%t,"merge_commit_sha":%q,"mergeable":true,"mergeable_state":%q,"head":{"ref":%q,"sha":%q},"base":{"ref":"main"},"updated_at":"2026-11-17T00:00:00Z"}`,
		p.number, state, merged, "merge-"+stepRecordsHead, mergeable, p.branch, stepRecordsHead)
}
