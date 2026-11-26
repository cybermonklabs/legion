// The rig of the whole-daemon walks of step_records_test.go and plan_ratings_test.go
// (stepRecordsRig): the booted daemon over the real Dispatch, the issues of the run and their pull
// requests, each role's agent over the worker stream and the daemon's HTTP routes (stepClaimant),
// the read of an issue's steps and what a read must hold (stepsHold), and GitHub's REST API as the
// walk's daemon reads it (stepRecordsGitHub).
package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
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

// boot starts the whole daemon on the store as it stands, over a fake runtime of its own.
func (r *stepRecordsRig) boot() {
	r.t.Helper()
	r.rt, r.built = fake.NewRuntime(), &built{}
	// The runtime provisions each claim's workspace itself, so a launch reaches the fake without
	// the daemon cloning acme/widgets from github.com.
	r.rt.InPod = true
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
	r.d = startDaemonWithin(r.t, r.cfg, o, r.log(), stepRecordsWait)
}

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
// holds it: its row of graph_nodes and every row of graph_tuples that names it, at either end.
// The tuples of a run or a plan are the store's append-only log, so this is a delete no writer of
// Dispatch makes; the rig makes it in its own test database, to stand for a record Dispatch never
// received. It fails where no tuple names the node, or where the store holds other than one node
// of that id.
func (r *stepRecordsRig) deleteNode(id string) {
	t := r.t
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, r.dispatch.database)
	if err != nil {
		t.Fatalf("connect to Dispatch's database: %v", err)
	}
	defer conn.Close(ctx)
	tag, err := conn.Exec(ctx, `
		delete from graph_tuples
		where (from_kind in ('step', 'plan') and from_id = $1) or (to_kind in ('step', 'plan') and to_id = $1)
	`, id)
	if err != nil || tag.RowsAffected() == 0 {
		t.Fatalf("delete the tuples of %s: %d rows, %v", id, tag.RowsAffected(), err)
	}
	if tag, err = conn.Exec(ctx, "delete from graph_nodes where kind in ('step', 'plan') and id = $1", id); err != nil || tag.RowsAffected() != 1 {
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
}

// worker is role's claimant on the walk's issue (workerOf).
func (r *stepRecordsRig) worker(role claim.Role) *stepClaimant {
	r.t.Helper()
	return r.workerOf(r.key, role)
}

// workerOf is role's claimant on issue, an issue of the tree of the walk's issue: the one already
// registered, or the launch the daemon makes now, registered and ready under a session of its own.
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
		bootToken: launch.BootToken, sh: dialShim(t, address, launch.BootToken), prompts: make(chan shimwire.Prompt, 8)}
	status, body := r.d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: launch.BootToken, SessionID: c.session, OmpSessionFile: "/sessions/" + c.session + ".jsonl",
		AgentID: c.session, PluginContract: api.DaemonAPIVersion(),
	}, false)
	if status != http.StatusOK {
		t.Fatalf("register %s = %d %s, want 200", token, status, body)
	}
	var registered claim.RegisterResponse
	if err := json.Unmarshal(body, &registered); err != nil {
		t.Fatalf("decode the registration of %s: %v", token, err)
	}
	c.secret = registered.Secret
	sh, d := c.sh, r.d
	sh.serve(func() { c.serve(sh, d) })
	if status, body := r.d.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: token, SessionID: c.session, Secret: c.secret, Generation: launch.Generation,
	}, false); status != http.StatusNoContent {
		t.Fatalf("ready %s = %d %s, want 204", token, status, body)
	}
	r.claims[token] = c
	return c
}

// reconnect is the pane's shim connecting to the daemon that runs now with the boot token it has
// held since its launch, as it does after the daemon restarts. A daemon that acknowledges no hello
// of it fails the walk with what that daemon made of the claim (refusedHello).
func (c *stepClaimant) reconnect() {
	c.r.built.mu.Lock()
	address := c.r.built.address
	c.r.built.mu.Unlock()
	sh, err := ackedShim(c.r.t, address, c.bootToken)
	if err != nil {
		c.r.t.Fatal(c.refusedHello(err))
	}
	c.mu.Lock()
	c.sh = sh
	c.mu.Unlock()
	d := c.r.d
	sh.serve(func() { c.serve(sh, d) })
}

// refusedHello says why the daemon that runs now acknowledged no hello of c's pane, naming what it
// compared: the plugin contract the claim records against its own (api.DaemonAPIVersion), the
// claim as it holds it now, and the launch of the claim this boot made. A boot relaunches at once a
// claim of another contract that its store holds out of a turn (supervise's refreshPlugin), and
// that launch gives the claim a boot token the pane does not hold. The worker stream closes a hello
// of a token it does not know with no frame, which the shim reads as an EOF, so last, the shim's own
// refusal, says none of that.
func (c *stepClaimant) refusedHello(last error) string {
	c.r.t.Helper()
	held := c.r.d.claim(c.token)
	launch := "this boot made no launch of it under another boot token, so the daemon's log says why it refused the pane's (worker-stream: rejected hello)"
	for _, call := range slices.Backward(c.r.rt.Calls()) {
		if (call.Method == "Spawn" || call.Method == "Resume") && call.Spec.Claim == c.token {
			if call.Spec.BootToken != c.bootToken {
				launch = fmt.Sprintf("this boot launched it again (%s at generation %d) under a boot token of its own, so the pane's is one the daemon no longer knows",
					call.Method, call.Spec.Generation)
			}
			break
		}
	}
	return fmt.Sprintf("the daemon at contract %d acknowledged no hello of %s's pane, whose claim records plugin contract %d: the daemon holds the claim %s at generation %d, and %s; the pane holds boot token %s, and the shim's last refusal was %v",
		api.DaemonAPIVersion(), c.token, held.PluginContract, held.State, held.Generation, launch, c.bootToken, last)
}

// serve reads every frame the daemon d sends sh until the connection closes or the test ends it
// (shim.serve): it answers the protocol negotiation and the state probe, and acknowledges each
// prompt and hands it to the walk, whose take starts its turn. An answer that fails to write
// fails the test, unless the test had begun stopping d by then: the pane's connection outlives
// the daemon, as a tmux pane does, so a restart's stop closes it under whatever answer is on
// its way, which is no fault of the daemon's.
func (c *stepClaimant) serve(sh *shim, d *daemon) {
	reply := func(frame shimwire.Frame) {
		if err := sh.writer.WriteFrame(frame); err != nil && !d.stopped.Load() {
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

// waitState waits for the daemon to hold the claim in state, both as its machine publishes the
// claim and as its store holds it. A machine publishes a claim before the write that keeps it
// (supervise's putClaim and putClaimAndPending), and a restart's stop ends a write still in flight,
// so a walk that went on at the published state alone could stop the daemon while its store still
// held the claim as it was: the next boot reads only the store, and relaunches at once a claim of
// another plugin contract that it reads out of a turn (supervise's refreshPlugin), under a boot
// token the pane does not hold (refusedHello).
func (c *stepClaimant) waitState(state supervise.ClaimState) {
	c.r.t.Helper()
	var published string
	var stored supervise.ClaimState
	if !testwait.Holds(stepRecordsWait, 20*time.Millisecond, func() bool {
		published, stored = c.r.d.claim(c.token).State, storedClaimState(c.r.t, c.r.st.Pool(), c.token)
		return published == string(state) && stored == state
	}) {
		c.r.t.Fatalf("%s is %s, and %s in the store, want %s in both", c.token, published, stored, state)
	}
}

// answer is the agent's model (c.model, `<provider>/<model>`) answering, which Oh My Pi's
// message_end names as message.provider and message.model.
func (c *stepClaimant) answer() {
	t := c.r.t
	t.Helper()
	provider, model, _ := strings.Cut(c.model, "/")
	frame, err := json.Marshal(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "provider": provider, "model": model}})
	if err != nil {
		t.Fatal(err)
	}
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
