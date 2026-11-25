// The skeleton of the rig a whole-daemon test of a fix beside a review stands on (fixRig), and
// its one walk (TestTheFixRigSkeleton): the real Dispatch this checkout builds, with its events on
// the test's NATS server and its GitHub App reading the check runs of the fake GitHub of
// githubtest.Checks; the whole daemon over the fake runtime, reading GitHub from a stand-in of its
// own (fixRigGitHub, fix_rig_github_test.go) that serves the issue's pull request in step with the
// events the rig sends; and GitHub's events and the listener's settlements published on the
// notification stream, the path they take into the daemon and into Dispatch in production.
//
// The walk proves no rule of a fix. It drives one issue with no fix from its plan to done and logs
// each fact the rig produces as it arrives, on lines that start `RIG FACT:`, so a later test's
// checks can be written against facts a run of the rig showed. It asserts only that each step of
// the walk happened.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/reviewround"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testpg"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// fixRigPull is the number of the walk's pull request, and fixRigTitle the walk's issue's title,
// which the pull request's title carries after the issue's key.
const (
	fixRigPull  = 7
	fixRigTitle = "Fix rig skeleton walks one issue"
)

// fixRigVerification is the walk's Verification document: k1, a before_merge check the tester
// proves by its own result, and k-ci, a before_merge check the CI job unit proves.
const fixRigVerification = `:::check{#k1 when="before_merge" negative_control="A tester that records nothing."}` + "\n" +
	"The tester records a pass of the change at the head.\n" +
	":::\n\n" +
	`:::check{#k-ci when="before_merge" negative_control="A unit job that exits 0 on a failing test." ci_check="unit"}` + "\n" +
	"The unit tests pass at the head.\n" +
	":::\n"

// fixRigFactPrefix starts each line a fact of the walk is logged on.
const fixRigFactPrefix = "RIG FACT:"

// fixRig is the walk's rig: the walk of an owner's plan over the real Dispatch (ownerStepsRig),
// that Dispatch's events on the run's NATS server and its App on the fake GitHub of check runs
// (checks), its own database open (dispatchDB), the daemon's GitHub stand-in (pulls), the daemon's
// log (logs), the clock GitHub's events of the pull requests are stamped with (clock), the seam a
// control sets on the daemon's engine at each boot (engine, nil for none), and the watcher that
// logs each fact as it arrives (watch). Its steps act on one issue of the run at a time, as
// ownerStepsRig's do (on).
type fixRig struct {
	*ownerStepsRig
	natsURL    string
	checks     *githubtest.Checks
	pulls      *fixRigGitHub
	dispatchDB *pgxpool.Pool
	logs       *syncBuffer
	started    time.Time
	clock      time.Time
	engine     func(*workflow.Engine)
	watch      *fixRigWatch
}

// fixRigIssue is one issue a fix rig files: its title, which no other issue of the run shares a
// word that counts with (walkTitles says why), the number of its pull request, and its
// Verification document.
type fixRigIssue struct {
	title        string
	number       int
	verification string
}

// newFixRig takes the run's NATS server, starts the fake GitHub of check runs and the scratch
// Dispatch on it with its App and its delivery settings, files each of issues with its spec and
// its Verification document, seeds each in the daemon's store at admitted with its architect's
// launch begun (seedAdmitted), boots the daemon on a database of its own with engine set on its
// engine (nil for no seam), registers each architect, and starts the watcher. Its steps act on the
// first issue until on moves them.
func newFixRig(t *testing.T, engine func(*workflow.Engine), issues ...fixRigIssue) *fixRig {
	t.Helper()
	ctx := context.Background()
	// The NATS server is taken first: testnats.URL empties it of every stream, and Dispatch
	// reconciles ENVOY_NOTIFICATIONS on it on its first connection, which the daemon consumes.
	natsURL := testnats.URL(t)
	checks := githubtest.ServeChecks(t)
	dispatch := startScratchDispatchOn(t, scratchDispatchOptions{natsURL: natsURL, github: checks})
	dispatchDB, err := pgxpool.New(ctx, dispatch.database)
	if err != nil {
		t.Fatalf("open Dispatch's database: %v", err)
	}
	t.Cleanup(dispatchDB.Close)
	awaitDispatchStream(t, natsURL, dispatch)
	dispatch.writeDeliverySettings(liveRepo)

	cfg := testConfig(t)
	// Dispatch takes a project key of at most ten characters; testConfig's is longer.
	cfg.Project = "FX" + randomSuffix(t)[:8]
	cfg.Projects = map[string]config.Project{cfg.Project: {}}
	// Each issue of the run holds a slot of its own.
	cfg.AdmissionCap = max(4, len(issues))
	// A clone of the package's template of its own: the scratch Dispatch numbers its events from
	// one in a database of its own, so a daemon sharing a database with another test's would take
	// this run's events for ones it had processed (blockersDatabase says the same).
	cfg.PostgresDSN = testpg.Clone(t, "legion_daemon_run")
	cfg.NatsURLs = []string{natsURL}
	cfg.DispatchURL = dispatch.base
	cfg.DispatchTokenFile = filepath.Join(t.TempDir(), "dispatch-token")
	if err := os.WriteFile(cfg.DispatchTokenFile, []byte(dispatch.token+"\n"), 0o600); err != nil {
		t.Fatalf("write the scratch Dispatch bearer: %v", err)
	}
	// No controller tick runs: the first is an hour after the runtime starts.
	cfg.ControllerWakeInterval = time.Hour

	proof := liveProof{t: t, cfg: cfg, notices: newNoticeListener(), records: record.NewStore()}
	envoy := httptest.NewServer(proof.notices)
	t.Cleanup(envoy.Close)
	proof.cfg.EnvoyURL = envoy.URL
	if proof.project, err = claim.ProjectToken(cfg.Project); err != nil {
		t.Fatal(err)
	}
	proof.js = workflowJetStream(t, natsURL)
	if proof.st, err = store.Open(ctx, proof.cfg.PostgresDSN); err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(proof.st.Close)
	if _, err := proof.st.Migrate(ctx); err != nil {
		t.Fatalf("migrate the store: %v", err)
	}

	r := &fixRig{
		ownerStepsRig: &ownerStepsRig{stepRecordsRig: &stepRecordsRig{liveProof: proof, dispatch: dispatch,
			claims: map[claim.Token]*stepClaimant{}, waits: make(chan claim.Token, 16)}, specs: map[string]string{}},
		natsURL: natsURL, checks: checks, dispatchDB: dispatchDB, logs: &syncBuffer{}, started: time.Now(), engine: engine,
		pulls: newFixRigGitHub(t),
	}
	r.person(http.MethodPost, "/api/v1/projects", map[string]string{"key": r.cfg.Project, "name": "Fix rig"}, http.StatusCreated, nil)
	r.person(http.MethodPut, "/api/v1/projects/"+r.cfg.Project+"/default-repository", map[string]any{"repo": liveRepo}, http.StatusOK, nil)
	for _, issue := range issues {
		r.fileIssue(issue)
	}
	r.boot()
	for _, issue := range r.issues {
		r.on(issue.key)
		r.worker(claim.RoleArchitect)
	}
	r.on(r.issues[0].key)
	r.watch = r.startWatch()
	return r
}

// awaitDispatchStream waits for Dispatch to have made the notification stream, which it does in
// the background once it connects: the daemon refuses to boot without it, and the delivery
// settings' durable is made on it.
func awaitDispatchStream(t *testing.T, natsURL string, dispatch *scratchDispatch) {
	t.Helper()
	js, err := jetstream.New(testnats.Connect(t, natsURL))
	if err != nil {
		t.Fatalf("open JetStream on %s: %v", natsURL, err)
	}
	var last error
	if !testwait.Holds(testwait.Bounded(t, scratchCIWithin, time.Second), 100*time.Millisecond, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, last = js.Stream(ctx, "ENVOY_NOTIFICATIONS")
		return last == nil
	}) {
		t.Fatalf("Dispatch made no ENVOY_NOTIFICATIONS stream on %s within %s: %v\n%s", natsURL, scratchCIWithin, last, dispatch.deliveryLog())
	}
}

// fileIssue files issue in the run's project with its spec and its Verification document, in
// progress and carrying the legion label, gives the daemon's GitHub stand-in its pull request,
// unopened, and seeds it in the daemon's store at admitted at the sequence of Dispatch's last event
// of it, so the daemon applies only what comes after. The rig's steps then act on it.
func (r *fixRig) fileIssue(issue fixRigIssue) {
	t := r.t
	t.Helper()
	var spec struct {
		createdIssue
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}
	r.person(http.MethodPost, "/api/v1/issues", map[string]string{"project": r.cfg.Project, "title": issue.title,
		"spec": "The issue's change, as its owner states it.\n"}, http.StatusCreated, &spec)
	if spec.PrimaryArtifactID == "" {
		t.Fatalf("Dispatch created %s with no primary document", spec.Key)
	}
	r.person(http.MethodPost, "/api/v1/issues/"+spec.Key+"/artifacts", map[string]string{"name": "verification.md", "content": issue.verification},
		http.StatusCreated, nil)
	var filed createdIssue
	r.person(http.MethodPatch, "/api/v1/issues/"+spec.Key, map[string]any{"status": "in_progress", "labels": []string{"legion"}}, http.StatusOK, &filed)
	r.specs[spec.Key] = spec.PrimaryArtifactID
	r.issues = append(r.issues, walkIssue{key: spec.Key, title: issue.title, number: issue.number})
	r.on(spec.Key)
	r.pulls.add(r.number, claim.IssueBranch(r.key), r.prTitle())
	r.seedAdmitted(filed.Rank, filed.LastSeq)
	r.fact("issue-filed", "%s in Dispatch with its spec and its Verification document, pull request #%d; seeded in the daemon at admitted at Dispatch sequence %d",
		r.key, r.number, filed.LastSeq)
}

// boot starts the whole daemon on the store as it stands, over a fake runtime of its own that
// provisions each claim's workspace itself, with GitHub the rig's stand-in, its log at debug in
// logs, and the rig's seam set on its engine before the engine serves a fact. The boot's gh is
// this test binary, named through the environment the boot reads.
func (r *fixRig) boot() {
	t := r.t
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r.rt, r.built = fake.NewRuntime(), &built{}
	r.rt.InPod = true
	o := fakeRuntime(r.rt, r.built)
	o.workflowTokens = &workflowTokenRecorder{}
	o.githubAPI = r.pulls.url
	o.controllerFirstWake = time.Hour
	o.environ = append(os.Environ(), "LEGION_GH_PATH="+executable)
	o.engine = r.engine
	r.d = startDaemonWithin(t, r.cfg, o, slog.New(slog.NewJSONHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug})), stepRecordsWait)
}

// restart stops the daemon and boots it again on the same store, the same Dispatch and the same
// ports (boot). Each pane outlives the daemon, as a tmux pane does: the new daemon re-observes each
// live claim rather than launching it again, and each pane's shim connects to it with the boot
// token it has held since its launch, its turn and its session as they were.
func (r *fixRig) restart() {
	r.t.Helper()
	r.d.stop()
	rebindHeldPorts(r.t, &r.cfg)
	r.boot()
	for _, c := range r.claims {
		c.reconnect()
	}
}

// prTitle is the title of the pull request of the issue the rig's steps act on: the issue's key,
// which Dispatch attributes the pull request to the issue by, and the issue's title.
func (r *fixRig) prTitle() string {
	return r.key + " " + r.title()
}

// fact logs one fact of the walk on a line that starts fixRigFactPrefix, with the wall clock in UTC
// and the time since the rig began. The watcher calls it from its own goroutine.
func (r *fixRig) fact(name, format string, args ...any) {
	now := time.Now()
	r.t.Logf("%s %s at %s (+%s): %s", fixRigFactPrefix, name, now.UTC().Format("15:04:05.000"), now.Sub(r.started).Round(time.Millisecond),
		fmt.Sprintf(format, args...))
}

// walk runs one step of the walk between two facts that name it, each with Dispatch's
// check_states_owed rows of the issue as they stand then.
func (r *fixRig) walk(name string, body func()) {
	r.t.Helper()
	r.fact("step-begins", "%s; Dispatch's check_states_owed rows of %s: %s", name, r.key, r.owed())
	began := time.Now()
	body()
	r.fact("step-ends", "%s, after %s; Dispatch's check_states_owed rows of %s: %s", name, time.Since(began).Round(time.Millisecond), r.key, r.owed())
}

// await waits, for stepRecordsWait at most, for holds, a fact the walk's next assertion stands
// on, and logs that it came; it fails the test with the rig's readout when it does not.
func (r *fixRig) await(what string, holds func() bool) {
	r.t.Helper()
	began := time.Now()
	if !testwait.Holds(testwait.Bounded(r.t, stepRecordsWait, time.Second), 50*time.Millisecond, holds) {
		r.t.Fatalf("waited %s for %s, which never came; %s", time.Since(began).Round(time.Millisecond), what, r.readout())
	}
	r.fact("barrier", "%s, %s after the wait began", what, time.Since(began).Round(time.Millisecond))
}

// tick is GitHub's clock for the pull request's next event: now to the second, as GitHub stamps
// updated_at, and always a second past the one before, so the daemon's lifecycle fence
// (classify.LateLifecycle) orders each event after the one before and a read of an older answer
// after it moves nothing.
func (r *fixRig) tick() time.Time {
	at := time.Now().UTC().Truncate(time.Second)
	if !at.After(r.clock) {
		at = r.clock.Add(time.Second)
	}
	r.clock = at
	return at
}

// publishPullRequest publishes GitHub's pull_request event of action on the walk's pull request at
// head, stamped updated, with more on top of its fields, as Envoy relays it
// (packages/envoy/internal/contracts/normalize.go), and answers its event id. The daemon's intake
// (decodePullRequest) and Dispatch's delivery intake (handlePullRequestEnvelope) both read it. It
// names no author: the delivery intake's merge handling reads only a merge by an author of the
// delivery settings' population, and the fake GitHub of check runs serves no pull request.
func (r *fixRig) publishPullRequest(action, head string, updated time.Time, more map[string]string) string {
	r.t.Helper()
	fields := map[string]string{
		"action": action, "kind": "pr", "repo": liveRepo, "number": strconv.Itoa(r.number), "title": r.prTitle(),
		"url":      fmt.Sprintf("https://github.com/%s/pull/%d", liveRepo, r.number),
		"head_sha": head, "head_ref": claim.IssueBranch(r.key), "head_repo": liveRepo, "base_ref": "main", "base_sha": fixRigBase,
		"merged": "false", "updated_at": updated.Format(time.RFC3339),
	}
	maps.Copy(fields, more)
	payload, err := json.Marshal(fields)
	if err != nil {
		r.t.Fatalf("encode the %s payload: %v", action, err)
	}
	subject := fmt.Sprintf("notifications.github.%s.pr.%d", r.subjectRepo(), r.number)
	id := r.publish(subject, "github", fmt.Sprintf("pr-%d-%s-%s", r.number, action, head[:8]), string(payload))
	r.fact("sent", "the pull_request %s of #%d at %s, updated_at %s, on %s, event %s", action, r.number, head, updated.Format(time.RFC3339), subject, id)
	return id
}

// publishPush publishes GitHub's push of to onto from on the issue's branch, by the implement App,
// as Envoy relays it (intake's decodePush), and answers its event id.
func (r *fixRig) publishPush(from, to string) string {
	r.t.Helper()
	payload, err := json.Marshal(map[string]string{
		"kind": "push", "repo": liveRepo, "ref": "refs/heads/" + claim.IssueBranch(r.key), "before": from, "after": to,
		"changed_paths": "src/widget.go", "changed_paths_truncated": "false", "forced": "false",
		"pusher": implementerLogin, "sender": implementerLogin, "commit_count": "1", "head_subject": "change the widget again",
	})
	if err != nil {
		r.t.Fatalf("encode the push payload: %v", err)
	}
	subject := fmt.Sprintf("notifications.github.%s.push.branch.%s", r.subjectRepo(), claim.IssueBranch(r.key))
	id := r.publish(subject, "github", fmt.Sprintf("push-%d-%s", r.number, to[:8]), string(payload))
	r.fact("sent", "the push of %s onto %s on %s, event %s", to, from, subject, id)
	return id
}

// publishReview publishes the review App's review of state of the walk's pull request at head,
// its body ending with footer, as Envoy relays a GitHub review (intake's decodeReview), and
// answers its event id. Each review of the run takes an id of its own.
func (r *fixRig) publishReview(state, head, footer string) string {
	r.t.Helper()
	r.reviews++
	payload, err := json.Marshal(map[string]string{
		"kind": "review", "repo": liveRepo, "number": strconv.Itoa(r.number), "action": "submitted", "state": state,
		"commit_id": head, "head_sha": head, "author": reviewAppLogin, "body": "The reviewer's " + state + " of " + head + footer,
		"body_truncated": "false", "review_id": strconv.Itoa(1000 + r.reviews), "submitted_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		r.t.Fatalf("encode the review payload: %v", err)
	}
	subject := fmt.Sprintf("notifications.github.%s.pr.%d.review", r.subjectRepo(), r.number)
	id := r.publish(subject, "github", fmt.Sprintf("review-%d-%s", r.number, head[:8]), string(payload))
	r.fact("sent", "the review App's %s of #%d at %s, footer %q, event %s", state, r.number, head, strings.TrimSpace(footer), id)
	return id
}

// unitRun is the run id of the job unit at a head, in the state status with conclusion.
func unitRun(id int64, status, conclusion string) githubtest.CheckRun {
	run := githubtest.CheckRun{ID: id, Name: scratchCIJob, Status: status, Conclusion: conclusion, AppSlug: "github-actions",
		HTMLURL: fmt.Sprintf("https://github.com/%s/actions/runs/%d/job/%d", liveRepo, id, id+1)}
	if status == "completed" {
		run.CompletedAt = time.Now()
		run.Output = githubtest.CheckRunOutput{Title: "unit " + conclusion, Summary: "TestTheUnit ran."}
	}
	return run
}

// setRun sets run on head on the fake GitHub of check runs, from its next read on.
func (r *fixRig) setRun(head string, run githubtest.CheckRun) {
	r.t.Helper()
	r.checks.SetCheckRun(liveRepo, head, run)
	r.fact("check-run-set", "the fake GitHub's run %s (%d) at %s is %s %s", run.Name, run.ID, head, run.Status, run.Conclusion)
}

// settle publishes the listener's settlement of the walk's pull request at head with run, and
// answers its sequence on the notification stream.
func (r *fixRig) settle(head string, run githubtest.CheckRun) uint64 {
	r.t.Helper()
	seq := githubtest.PublishChecksSettlement(r.t, r.natsURL, githubtest.ChecksSettlement{Repo: liveRepo, Number: r.number, SHA: head,
		Runs: []githubtest.CheckRun{run}})
	r.fact("sent", "the listener's settlement of #%d at %s, %s %s %s, sequence %d of the notification stream", r.number, head, run.Name,
		run.Status, run.Conclusion, seq)
	return seq
}

// recordResult is c, the tester, recording a pass of check at commit through Dispatch's result
// route, as `dispatch result` records it under its session with the agent bearer.
func (r *fixRig) recordResult(c *stepClaimant, check, commit string) {
	t := r.t
	t.Helper()
	encoded, err := json.Marshal(map[string]any{
		"actor": map[string]string{"kind": "session", "id": c.session}, "check": check, "role": string(c.role), "verdict": "pass",
		"commit": commit, "observed": "The tester ran the change at " + commit + " and it held.",
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, r.dispatch.base+"/api/v1/issues/"+r.key+"/results", strings.NewReader(string(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+r.dispatch.token)
	status, answer := r.send(request)
	if status != http.StatusCreated {
		t.Fatalf("the %s's result of %s at %s = %d %s, want 201", c.role, check, commit, status, answer)
	}
	r.fact("sent", "the %s's pass of %s at %s through POST /api/v1/issues/%s/results: %d", c.role, check, commit, r.key, status)
}

// processed is when the daemon's processed_events row of the GitHub event id was written, and
// whether there is one.
func (r *fixRig) processed(id string) (time.Time, bool) {
	r.t.Helper()
	var at time.Time
	err := r.st.Pool().QueryRow(context.Background(), `select processed_at from processed_events where source = 'github' and event_id = $1`, id).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false
	}
	if err != nil {
		r.t.Fatalf("read the processed event %s: %v", id, err)
	}
	return at, true
}

// awaitProcessed waits for the daemon's processed_events row of the GitHub event id, which its
// intake writes in the transaction that applies the event (intake.ApplyFact, MarkProcessed), and
// logs it.
func (r *fixRig) awaitProcessed(id string) {
	r.t.Helper()
	var at time.Time
	r.await("the daemon's processed_events row of "+id, func() bool {
		var ok bool
		at, ok = r.processed(id)
		return ok
	})
	r.fact("processed", "processed_events holds github %s, processed_at %s", id, at.UTC().Format("15:04:05.000"))
}

// heldState is the daemon's check_states row of check at head, and whether it holds one.
func (r *fixRig) heldState(check, head string) (scratchCIState, bool) {
	r.t.Helper()
	var held scratchCIState
	err := r.st.Pool().QueryRow(context.Background(), `
		select state, reason, ci_case, dispatch_seq from check_states where issue = $1 and repo = $2 and head = $3 and check_id = $4`,
		r.key, liveRepo, head, check).Scan(&held.State, &held.Reason, &held.CICase, &held.Seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return scratchCIState{}, false
	}
	if err != nil {
		r.t.Fatalf("read the daemon's state of %s at %s: %v", check, head, err)
	}
	return held, true
}

// awaitState waits for the daemon to hold a state of check at head that want accepts, and answers
// it.
func (r *fixRig) awaitState(check, head, what string, want func(scratchCIState) bool) scratchCIState {
	r.t.Helper()
	var held scratchCIState
	r.await(fmt.Sprintf("the daemon's state of %s at %s %s", check, head, what), func() bool {
		var ok bool
		held, ok = r.heldState(check, head)
		return ok && want(held)
	})
	r.fact("held-state", "the daemon holds %s at %s: %s", check, head, held)
	return held
}

// owed is Dispatch's check_states_owed rows of the issue, read from Dispatch's own database, as
// text.
func (r *fixRig) owed() string {
	rows, err := fixRigOwed(context.Background(), r.dispatchDB, r.key)
	if err != nil {
		return "read failed: " + err.Error()
	}
	return rows
}

// fixRigOwed is Dispatch's check_states_owed rows of issue, each `#<id> due <time> failures <n>`,
// or `none`.
func fixRigOwed(ctx context.Context, pool *pgxpool.Pool, issue string) (string, error) {
	rows, err := pool.Query(ctx, `select id, due_at, failures from check_states_owed where issue_key = $1 order by id`, issue)
	if err != nil {
		return "", err
	}
	var owed []string
	var id int64
	var due time.Time
	var failures int
	if _, err := pgx.ForEachRow(rows, []any{&id, &due, &failures}, func() error {
		owed = append(owed, fmt.Sprintf("#%d due %s failures %d", id, due.UTC().Format("15:04:05.000"), failures))
		return nil
	}); err != nil {
		return "", err
	}
	if len(owed) == 0 {
		return "none", nil
	}
	return strings.Join(owed, "; "), nil
}

// fixRigPullRequest is pr as a fact names it: its state, head, GitHub's clock of it, its merge
// state with the read that stamped it, and the settlement fence of its checked head.
func fixRigPullRequest(pr *record.PullRequest) string {
	if pr == nil {
		return "no pull request recorded"
	}
	runs := make([]string, len(pr.CheckRuns))
	for i, run := range pr.CheckRuns {
		runs[i] = fmt.Sprintf("%s %d %s at %s", run.Name, run.ID, run.Conclusion, run.Head)
	}
	read := "never"
	if !pr.StateReadAt.IsZero() {
		read = pr.StateReadAt.UTC().Format("15:04:05.000")
	}
	return fmt.Sprintf("#%d %s, head_sha %s (updated_at %s), mergeability %q, merge_state %q read %s, checked_head %q, check_runs [%s]",
		pr.Number, pr.State, pr.HeadSHA, pr.HeadUpdatedAt.UTC().Format(time.RFC3339), pr.Mergeability, pr.MergeState, read,
		pr.CheckedHead, strings.Join(runs, "; "))
}

// fixRigEntry is the entry of the review round task names, read as an agent reads it: the number
// its last ` Review entry: <n>.` names (reviewround.EntryLabel).
func fixRigEntry(t *testing.T, task string) int64 {
	t.Helper()
	label := " " + reviewround.EntryLabel + ": "
	if at := strings.LastIndex(task, label); at >= 0 {
		if number, last := strings.CutSuffix(strings.TrimSpace(task[at+len(label):]), "."); last {
			if entry, err := strconv.ParseInt(number, 10, 64); err == nil && entry > 0 {
				return entry
			}
		}
	}
	t.Fatalf("the reviewer's task %q does not end naming its %q", task, reviewround.EntryLabel)
	return 0
}

// dispatchIssue is the issue's status and closed_at in Dispatch's own database.
func (r *fixRig) dispatchIssue() (string, *time.Time) {
	r.t.Helper()
	var status string
	var closed *time.Time
	if err := r.dispatchDB.QueryRow(context.Background(), `select status, closed_at from issues where key = $1`, r.key).Scan(&status, &closed); err != nil {
		r.t.Fatalf("read %s in Dispatch's database: %v", r.key, err)
	}
	return status, closed
}

// dispatchHead is the head row Dispatch holds of the walk's pull request: its head, its state and
// the issue it is attributed to, "" for none.
func (r *fixRig) dispatchHead() (head, state, issue string) {
	r.t.Helper()
	err := r.dispatchDB.QueryRow(context.Background(), `select head_sha, state, coalesce(issue_key, '') from pull_request_heads
		where repo = $1 and number = $2`, liveRepo, r.number).Scan(&head, &state, &issue)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		r.t.Fatalf("read Dispatch's head of %s#%d: %v", liveRepo, r.number, err)
	}
	return head, state, issue
}

// readout is what a wait that never came could read: the issue, its pull request and its states
// in the daemon, the check states Dispatch published, what the two fakes of GitHub answered 401 or
// 404, and the last lines of the daemon's and Dispatch's logs that warn or fail.
func (r *fixRig) readout() string {
	ctx := context.Background()
	issue := "unread"
	if err := store.TransactWith(ctx, r.st.Pool(), pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		recorded, err := r.records.Issue(ctx, tx, r.key)
		if err != nil || recorded == nil {
			return err
		}
		issue = fmt.Sprintf("%s, hold %v", recorded.Phase, recorded.Hold)
		return nil
	}); err != nil {
		issue = "read failed: " + err.Error()
	}
	var pr *record.PullRequest
	if err := store.TransactWith(ctx, r.st.Pool(), pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		pr, err = r.records.PullRequest(ctx, tx, r.key)
		return err
	}); err != nil {
		issue += "; the pull request's read failed: " + err.Error()
	}
	states, _ := fixRigDaemonStates(ctx, r.st.Pool(), r.key)
	var daemonStates []string
	for _, key := range slices.Sorted(maps.Keys(states)) {
		daemonStates = append(daemonStates, key+" "+states[key])
	}
	var warnings []string
	for line := range strings.SplitSeq(r.logs.String(), "\n") {
		if strings.Contains(line, `"level":"WARN"`) || strings.Contains(line, `"level":"ERROR"`) {
			warnings = append(warnings, fixRigCut(line, 500))
		}
	}
	return fmt.Sprintf("the daemon holds %s in %s; its pull request %s; its states [%s]; Dispatch's owed rows %s; the stand-in answered 404 to %v; the fake of check runs refused %v and answered 404 to %v\nthe daemon's warnings and errors:\n%s\n%s",
		r.key, issue, fixRigPullRequest(pr), strings.Join(daemonStates, "; "), r.owed(), r.pulls.notAnswered(), r.checks.Refused(),
		r.checks.Unanswered(), strings.Join(scratchCILast(warnings, 30), "\n"), r.dispatch.deliveryLog())
}

// fixRigCut is line cut to at most n bytes.
func fixRigCut(line string, n int) string {
	if len(line) <= n {
		return line
	}
	return line[:n] + "…"
}

// fixRigDaemonStates is each check_states row the daemon holds of issue, by `<check> at <head>`.
func fixRigDaemonStates(ctx context.Context, pool *store.Pool, issue string) (map[string]string, error) {
	rows, err := pool.Query(ctx, `select head, check_id, state, reason, ci_case, dispatch_seq from check_states where issue = $1`, issue)
	if err != nil {
		return nil, err
	}
	states := map[string]string{}
	var head, check, state, reason, ciCase string
	var seq int64
	_, err = pgx.ForEachRow(rows, []any{&head, &check, &state, &reason, &ciCase, &seq}, func() error {
		states[check+" at "+head] = fmt.Sprintf("state %s, reason %q, ci_case %q, dispatch_seq %d", state, reason, ciCase, seq)
		return nil
	})
	return states, err
}

// fixRigWatch is the watcher of the walk: a goroutine that reads, every fixRigWatchPoll, what the
// walk's facts are made of, and logs each fact as it first sees it (poll): of each issue of the
// run, its phase in the daemon, its pull request's record, its reviewer's row, each check state the
// daemon holds, each step-record, status, plan-record, fix-job and notice outbox row of it, as it
// appears and as it finishes, Dispatch's check_states_owed rows of it, each check.state_changed
// Dispatch appended, its status and closed_at in Dispatch, and Dispatch's head row of its pull
// request; each processed_events row of an event the walk sent or of a read the daemon built of
// GitHub; each read of a commit's check runs the fake GitHub answered and each request the daemon's
// GitHub stand-in answered; each notice the daemon published to the Envoy listener's stand-in; and
// the lines of the daemon's and Dispatch's logs that name a fact of the walk. The issues and the
// project are taken once, as the watcher starts, since the walk's steps move between issues (on).
type fixRigWatch struct {
	r        *fixRig
	project  string
	issues   []walkIssue
	stopping chan struct{}
	done     chan struct{}
	once     sync.Once

	seen                            map[string]string
	processed                       map[string]bool
	states                          map[string]string
	eventSeq                        map[string]int64
	checkReads, githubRequests      int
	outbox                          map[int64]string
	daemonLog, dispatchLog, notices int
	failed                          map[string]bool
}

// fixRigWatchPoll is how often the watcher reads.
const fixRigWatchPoll = 50 * time.Millisecond

// fixRigOutboxKinds are the outbox rows of each issue the watcher follows.
var fixRigOutboxKinds = []string{string(record.OutboxKindStepRecord), string(record.OutboxKindDispatchStatus),
	string(record.OutboxKindPlanRecord), string(record.OutboxKindFixJobRead), string(record.OutboxKindNotice), string(record.OutboxKindSupervise)}

// fixRigDaemonLines are the messages of the daemon's log whose lines are facts of the walk: the
// supervise machine's hold of a phase task for another claim's turn (supervise/delivery.go,
// sendPending), the testing move's wait (workflow/check_states.go, testingWaits), a review set
// aside (workflow/review.go, review), the hold of a task's end before its retro and its release
// (workflow/engine.go, holdBeforeRetro; workflow/retro_hold.go, startHeldRetro and rehold), and
// the merge state reads (daemon/merge_state_reads.go).
var fixRigDaemonLines = []string{
	"the phase task waits rather than adopt the working copy they share",
	"workflow: the testing move waits for check states",
	"workflow: a review decides nothing",
	"workflow: a review of the review App waits",
	"workflow: the reviewer's own review moves no reviewed commit",
	"is held before its retro",
	"holds a task before its retro",
	"merge state",
	// The start executor's lines (daemon/outbox.go), the machine's lines of a delivery it holds or
	// sends (supervise/delivery.go), and every warning and error, so a start that waits names why.
	"outbox start",
	"supervise: no connection",
	"supervise: the issue has left",
	`"level":"WARN"`,
	`"level":"ERROR"`,
}

// fixRigNotices is each notice listener accepted, in the order it took them.
func fixRigNotices(listener *noticeListener) []listenerPublish {
	listener.mu.Lock()
	defer listener.mu.Unlock()
	return slices.Clone(listener.accepted)
}

// startWatch starts the watcher of the run's issues, which stops when the test ends, before
// anything the rig opened closes.
func (r *fixRig) startWatch() *fixRigWatch {
	w := &fixRigWatch{r: r, project: r.cfg.Project, issues: slices.Clone(r.issues), stopping: make(chan struct{}), done: make(chan struct{}),
		seen: map[string]string{}, processed: map[string]bool{}, states: map[string]string{}, eventSeq: map[string]int64{},
		outbox: map[int64]string{}, failed: map[string]bool{}}
	go func() {
		defer close(w.done)
		for {
			w.poll()
			select {
			case <-w.stopping:
				w.poll()
				return
			case <-time.After(fixRigWatchPoll):
			}
		}
	}()
	r.t.Cleanup(w.stop)
	return w
}

// stop ends the watcher after one last read, and waits for it.
func (w *fixRigWatch) stop() {
	w.once.Do(func() { close(w.stopping) })
	<-w.done
}

// fail logs a read the watcher could not make, once for each read.
func (w *fixRigWatch) fail(read string, err error) {
	if !w.failed[read] {
		w.failed[read] = true
		w.r.fact("watch-read-failed", "the watcher could not read %s: %v", read, err)
	}
}

// changed logs name with now when now is not what the watcher last saw of name for issue.
func (w *fixRigWatch) changed(issue, name, now string) {
	if w.seen[issue+" "+name] != now {
		w.seen[issue+" "+name] = now
		w.r.fact(name, "%s", now)
	}
}

func (w *fixRigWatch) poll() {
	r := w.r
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	w.pollDaemon(ctx)
	w.pollDispatch(ctx)
	for _, read := range r.checks.Reads()[w.checkReads:] {
		w.checkReads++
		served := make([]string, len(read.Runs))
		for i, run := range read.Runs {
			served[i] = strings.TrimSpace(fmt.Sprintf("%s %d %s %s", run.Name, run.ID, run.Status, run.Conclusion))
		}
		if len(served) == 0 {
			served = []string{"no run"}
		}
		r.fact("check-runs-read", "read %d of the fake GitHub's check runs, at %s, of %s at %s: it served %s", w.checkReads,
			read.At.UTC().Format("15:04:05.000"), read.Repo, read.SHA, strings.Join(served, ", "))
	}
	for _, request := range r.pulls.requests()[w.githubRequests:] {
		w.githubRequests++
		r.fact("github-request", "request %d the daemon's GitHub stand-in answered, at %s: %s", w.githubRequests,
			request.at.UTC().Format("15:04:05.000"), request)
	}
	for _, published := range fixRigNotices(r.notices)[w.notices:] {
		w.notices++
		r.fact("notice", "the daemon published on %s: %s", published.Topic, fixRigCut(published.Payload, 600))
	}
	w.daemonLog = w.logLines("daemon-log", r.logs.String(), w.daemonLog, fixRigDaemonLines)
	w.dispatchLog = w.logLines("dispatch-log", r.dispatch.output.String(), w.dispatchLog, []string{"check states", "check_states"})
}

// pollDaemon reads the daemon's store, for each issue of the run.
func (w *fixRigWatch) pollDaemon(ctx context.Context) {
	r := w.r
	numbers := make([]string, len(w.issues))
	keys := make([]string, len(w.issues))
	for i, walked := range w.issues {
		numbers[i] = fmt.Sprintf("checks-acme-widgets-%d-%%", walked.number)
		keys[i] = walked.key
		w.pollIssue(ctx, walked.key)
	}
	rows, err := r.st.Pool().Query(ctx, `select event_id, processed_at from processed_events where source = 'github'
		and (strpos(event_id, $1) > 0 or event_id like any($2)) order by processed_at, event_id`, w.project, numbers)
	if err != nil {
		w.fail("processed_events", err)
	} else {
		var id string
		var at time.Time
		if _, err := pgx.ForEachRow(rows, []any{&id, &at}, func() error {
			if !w.processed[id] {
				w.processed[id] = true
				r.fact("processed-event", "processed_events holds github %s, processed_at %s", id, at.UTC().Format("15:04:05.000"))
			}
			return nil
		}); err != nil {
			w.fail("processed_events", err)
		}
	}
	outbox, err := r.st.Pool().Query(ctx, `select id, kind, payload::text from outbox where issue = any($1) and kind = any($2) order by id`, keys, fixRigOutboxKinds)
	if err != nil {
		w.fail("the outbox", err)
		return
	}
	standing := map[int64]bool{}
	var id int64
	var kind, payload string
	if _, err := pgx.ForEachRow(outbox, []any{&id, &kind, &payload}, func() error {
		standing[id] = true
		if _, seen := w.outbox[id]; !seen {
			w.outbox[id] = kind
			r.fact("outbox-row", "the outbox holds %s row %d: %s", kind, id, fixRigCut(payload, 600))
		}
		return nil
	}); err != nil {
		w.fail("the outbox", err)
		return
	}
	for _, id := range slices.Sorted(maps.Keys(w.outbox)) {
		if kind := w.outbox[id]; kind != "" && !standing[id] {
			r.fact("outbox-row-finished", "the %s row %d is finished (gone from the outbox)", kind, id)
			w.outbox[id] = ""
		}
	}
}

// pollIssue reads key's record, its pull request's record, its reviewer's row and the check
// states the daemon holds of it.
func (w *fixRigWatch) pollIssue(ctx context.Context, key string) {
	r := w.r
	if err := store.TransactWith(ctx, r.st.Pool(), pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		issue, err := r.records.Issue(ctx, tx, key)
		if err != nil {
			return err
		}
		if issue != nil {
			hold := "none"
			if issue.Hold != nil {
				hold = fmt.Sprintf("%+v", *issue.Hold)
			}
			w.changed(key, "phase", fmt.Sprintf("%s stands in %s, hold %s, status %s, running fix %d", issue.Key, issue.Phase, hold, issue.Status, issue.RunningFix))
		}
		pr, err := r.records.PullRequest(ctx, tx, key)
		if err != nil {
			return err
		}
		if pr != nil {
			w.changed(key, "pull-request-record", key+": "+fixRigPullRequest(pr))
		}
		rows, err := r.records.Phases(ctx, tx, key)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if row.Role != claim.RoleReviewer {
				continue
			}
			decision := "none"
			if row.Decision != nil {
				decision = fmt.Sprintf("state %q head %q refused %q", row.Decision.State, row.Decision.Head, row.Decision.Refused)
			}
			w.changed(key, "review-decision", fmt.Sprintf("%s's reviewer's row: entry %d, ReviewDecision %s, reviewed_head %q, completed %t",
				key, row.Entry, decision, row.ReviewedHead, row.Completed()))
		}
		return nil
	}); err != nil {
		w.fail("the daemon's record of "+key, err)
	}
	if states, err := fixRigDaemonStates(ctx, r.st.Pool(), key); err != nil {
		w.fail("the daemon's check_states of "+key, err)
	} else {
		for _, at := range slices.Sorted(maps.Keys(states)) {
			if seen := key + ": " + at; w.states[seen] != states[at] {
				w.states[seen] = states[at]
				r.fact("daemon-holds-state", "the daemon's check_states of %s holds %s: %s", key, at, states[at])
			}
		}
	}
}

// pollDispatch reads Dispatch's own database, for each issue of the run.
func (w *fixRigWatch) pollDispatch(ctx context.Context) {
	r := w.r
	for _, walked := range w.issues {
		key := walked.key
		if owed, err := fixRigOwed(ctx, r.dispatchDB, key); err != nil {
			w.fail("check_states_owed of "+key, err)
		} else {
			w.changed(key, "dispatch-owed", "Dispatch's check_states_owed rows of "+key+": "+owed)
		}
		rows, err := r.dispatchDB.Query(ctx, `select seq, coalesce(payload->>'check', ''), coalesce(payload->>'head', ''), coalesce(payload->>'state', ''),
			coalesce(payload->>'reason', ''), coalesce(payload->>'ci_case', ''), coalesce(payload->>'serial', ''), created_at
			from events where issue_key = $1 and type = 'check.state_changed' and seq > $2 order by seq`, key, w.eventSeq[key])
		if err != nil {
			w.fail("Dispatch's events of "+key, err)
		} else {
			var seq int64
			var check, head, state, reason, ciCase, serial string
			var created time.Time
			if _, err := pgx.ForEachRow(rows, []any{&seq, &check, &head, &state, &reason, &ciCase, &serial, &created}, func() error {
				w.eventSeq[key] = seq
				if check == "" {
					check = "the set row"
				}
				r.fact("dispatch-published-state", "Dispatch appended check.state_changed of %s seq %d at %s: %s at %s, state %q, reason %q, ci_case %q, serial %s",
					key, seq, created.UTC().Format("15:04:05.000"), check, head, state, reason, ciCase, serial)
				return nil
			}); err != nil {
				w.fail("Dispatch's events of "+key, err)
			}
		}
		var status string
		var closed *time.Time
		if err := r.dispatchDB.QueryRow(ctx, `select status, closed_at from issues where key = $1`, key).Scan(&status, &closed); err != nil {
			w.fail("Dispatch's issue "+key, err)
		} else {
			closedAt := "null"
			if closed != nil {
				closedAt = closed.UTC().Format("15:04:05.000")
			}
			w.changed(key, "dispatch-issue", fmt.Sprintf("Dispatch's %s: status %s, closed_at %s", key, status, closedAt))
		}
		var head, state, issue, source string
		err = r.dispatchDB.QueryRow(ctx, `select head_sha, state, coalesce(issue_key, ''), coalesce(attribution_source, '') from pull_request_heads
			where repo = $1 and number = $2`, liveRepo, walked.number).Scan(&head, &state, &issue, &source)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			w.fail("Dispatch's pull_request_heads of "+key, err)
		default:
			w.changed(key, "dispatch-head", fmt.Sprintf("Dispatch's pull_request_heads row of %s#%d: head %s, state %s, issue %q (by %s)",
				liveRepo, walked.number, head, state, issue, source))
		}
	}
}

// logLines logs each line of text past offset that names one of messages, and answers the
// offset of the text it read through: the last whole line.
func (w *fixRigWatch) logLines(name, text string, offset int, messages []string) int {
	end := strings.LastIndexByte(text, '\n') + 1
	if end <= offset {
		return offset
	}
	for line := range strings.SplitSeq(text[offset:end], "\n") {
		for _, message := range messages {
			if strings.Contains(line, message) {
				w.r.fact(name, "%s", fixRigCut(line, 700))
				break
			}
		}
	}
	return end
}

// TestTheFixRigSkeleton walks one issue with no fix from its owner's plan to done through the
// whole rig, and logs each fact the rig produces as it arrives. The plan is `implementer tester
// reviewer`; the Verification document holds k1, which the tester proves, and k-ci, which the CI
// job unit proves. The implementer completes at X with the pull request open; the tester records
// k1 and completes; unit concludes at X success and its settlement is published; the issue reaches
// reviewing; the head moves to Y by the push and the synchronize GitHub sends; the reviewer
// approves at Y with its own session and Y in its footer while unit still runs at Y; the round
// ends, and the task's end is held for its CI (record.HoldForCI); unit concludes at Y success with
// its settlement, which ends the hold while the reviewer's turn is still open, so the retro's task
// waits for that turn; the reviewer completes; the implementer's retro rates the plan; the merger
// is launched and GitHub's merge of the pull request lands; and the issue is done in Dispatch, its
// step records written.
//
// It asserts that each step happened, and nothing of any rule of a fix.
func TestTheFixRigSkeleton(t *testing.T) {
	t.Parallel()
	r := newFixRig(t, nil, fixRigIssue{title: fixRigTitle, number: fixRigPull, verification: fixRigVerification})
	x, y := scratchCIHead(r.key, "x"), scratchCIHead(r.key, "y")
	r.fact("heads", "X is %s and Y is %s, on pull request #%d of %s", x, y, r.number, liveRepo)

	r.walk("plan-and-gate", func() {
		artifact := r.givePlan("implementer", "tester", "reviewer")
		r.fact("plan-registered", "POST /legion/v1/plans/register took plan.md %s at version 1 for %s: 200", artifact, r.key)
		r.openGate(phase.Implementing)
	})

	implementer := r.worker(claim.RoleImplementer)
	r.walk("implementer-completes-at-x", func() {
		task := implementer.take()
		r.fact("task", "the implementer was prompted: %q", fixRigCut(task.Message, 300))
		// CI starts on the push that opens the pull request, so unit runs at X before Dispatch first
		// reads X.
		r.setRun(x, unitRun(9001, "in_progress", ""))
		opened := r.tick()
		r.pulls.open(r.number, x, opened)
		id := r.publishPullRequest("opened", x, opened, nil)
		r.awaitProcessed(id)
		r.await("the daemon's pull request record at X", func() bool {
			pr := r.pullRequest()
			return pr != nil && pr.HeadSHA == x
		})
		r.fact("pull-request-recorded", "the record after the opened event: %s", fixRigPullRequest(r.pullRequest()))
		handoff, version := r.writeHandoff(implementer.session, "implement")
		if status, body := r.complete(implementer, api.HandoffCompleteRequest{Summary: "implemented", Commit: x, HandoffID: handoff, HandoffVersion: version}); status != http.StatusOK {
			t.Fatalf("the implementer's completion at %s = %d %s, want 200", x, status, body)
		}
		implementer.endTurn()
		r.waitPhase(phase.Testing)
	})

	tester := r.worker(claim.RoleTester)
	r.walk("tester-records-k1-and-completes-at-x", func() {
		task := tester.take()
		r.fact("task", "the tester was prompted: %q", fixRigCut(task.Message, 300))
		r.await("Dispatch's head row of the pull request at X, attributed to "+r.key, func() bool {
			head, _, issue := r.dispatchHead()
			return head == x && issue == r.key
		})
		r.awaitState(scratchCICheck, x, "that says no run concluded there", func(held scratchCIState) bool {
			return held.CICase == scratchCINotCompleted || held.CICase == scratchCIAbsent
		})
		r.awaitState("k1", x, "of any word", func(scratchCIState) bool { return true })
		r.recordResult(tester, "k1", x)
		r.awaitState("k1", x, "yes", func(held scratchCIState) bool { return held.State == "yes" })
		handoff, version := r.writeHandoff(tester.session, "test")
		if status, body := r.complete(tester, api.HandoffCompleteRequest{Summary: "k1 holds", Commit: x, HandoffID: handoff, HandoffVersion: version}); status != http.StatusOK {
			t.Fatalf("the tester's completion at %s = %d %s, want 200", x, status, body)
		}
		tester.endTurn()
		r.await("the tester's row completed", func() bool { return r.phaseOf(claim.RoleTester).Completed() })
		r.fact("after-tester", "the daemon holds %s in %s after the tester's completion", r.key, r.d.state().Issues[r.key].Phase)
	})

	r.walk("unit-concludes-at-x", func() {
		run := unitRun(9001, "completed", "success")
		r.setRun(x, run)
		r.settle(x, run)
		r.awaitState(scratchCICheck, x, "yes", func(held scratchCIState) bool { return held.State == "yes" })
		r.waitPhase(phase.Reviewing)
		r.fact("reviewing", "%s stands in reviewing", r.key)
	})

	reviewer := r.worker(claim.RoleReviewer)
	var entry int64
	r.walk("head-moves-to-y", func() {
		task := reviewer.take()
		entry = fixRigEntry(t, task.Message)
		r.fact("task", "the reviewer was prompted, entry %d: %q", entry, fixRigCut(task.Message, 300))
		// GitHub moves the pull request first, as it does on github.com, and CI starts on the push.
		r.setRun(y, unitRun(9002, "in_progress", ""))
		moved := r.tick()
		r.pulls.move(r.number, y, moved)
		push := r.publishPush(x, y)
		synchronize := r.publishPullRequest("synchronize", y, moved, map[string]string{"before": x, "sender": implementerLogin})
		r.awaitProcessed(push)
		r.awaitProcessed(synchronize)
		r.await("the daemon's pull request record at Y", func() bool {
			pr := r.pullRequest()
			return pr != nil && pr.HeadSHA == y
		})
		r.fact("head-moved", "the record after the push and the synchronize: %s", fixRigPullRequest(r.pullRequest()))
	})

	r.walk("reviewer-approves-at-y-while-unit-runs", func() {
		r.await("Dispatch's head row of the pull request at Y", func() bool {
			head, _, _ := r.dispatchHead()
			return head == y
		})
		r.awaitState(scratchCICheck, y, "that says no run concluded there", func(held scratchCIState) bool {
			return held.CICase == scratchCINotCompleted || held.CICase == scratchCIAbsent
		})
		r.await("the daemon's pull request record at Y with GitHub's merge state of Y", func() bool {
			pr := r.pullRequest()
			return pr != nil && pr.HeadSHA == y && pr.MergeState.Mergeable()
		})
		r.fact("ready-for-review", "the record before the review: %s", fixRigPullRequest(r.pullRequest()))
		review := r.publishReview("approved", y, legionEntryFooter(reviewer.session, y, entry))
		r.awaitProcessed(review)
		decision := r.phaseOf(claim.RoleReviewer).Decision
		if decision == nil || decision.State != "approved" || decision.Head != y {
			t.Fatalf("the reviewer's ReviewDecision after its approval at %s is %+v, want approved at %s; %s", y, decision, y, r.readout())
		}
		r.fact("review-decided", "the reviewer's ReviewDecision after the approval: %+v", *decision)
		var stands api.Issue
		r.await(r.key+" held before its retro for its CI, or in its retro", func() bool {
			stands = r.d.state().Issues[r.key]
			return stands.Phase == phase.Held || stands.Phase == phase.Retro
		})
		if stands.Phase != phase.Held || stands.HoldReason != string(record.HoldForCI) {
			t.Fatalf("%s stands in %s held for %q after the approval at %s while unit runs there, want held for %q; %s",
				r.key, stands.Phase, stands.HoldReason, y, record.HoldForCI, r.readout())
		}
		r.fact("end-held", "%s stands in %s held for %q after the round ended", r.key, stands.Phase, stands.HoldReason)
	})

	r.walk("unit-concludes-at-y-and-the-hold-ends", func() {
		run := unitRun(9002, "completed", "success")
		r.setRun(y, run)
		r.settle(y, run)
		r.awaitState(scratchCICheck, y, "yes", func(held scratchCIState) bool { return held.State == "yes" })
		r.waitPhase(phase.Retro)
		r.fact("end-hold-ended", "%s stands in its retro once k-ci passed at %s", r.key, y)
	})

	r.walk("reviewer-completes", func() {
		handoff, version := r.writeHandoff(reviewer.session, "review")
		status, body := r.complete(reviewer, api.HandoffCompleteRequest{Summary: "approved", Commit: y, HandoffID: handoff, HandoffVersion: version})
		if status != http.StatusOK {
			t.Fatalf("the reviewer's completion at %s = %d %s, want 200", y, status, body)
		}
		r.fact("reviewer-completed", "the reviewer's completion at %s: %d", y, status)
		reviewer.endTurn()
	})

	r.walk("retro", func() {
		task := implementer.take()
		r.fact("task", "the implementer was prompted: %q", fixRigCut(task.Message, 300))
		rating, reason := 4, "The plan fit the work."
		if status, body := r.complete(implementer, api.HandoffCompleteRequest{Summary: "retro written", Commit: y, PlanRating: &rating, PlanReason: &reason}); status != http.StatusOK {
			t.Fatalf("the implementer's retro at %s = %d %s, want 200", y, status, body)
		}
		implementer.endTurn()
		r.waitPhase(phase.Merging)
	})

	r.walk("merge", func() {
		r.awaitLaunch(r.token(claim.RoleMerger))
		r.fact("merger-launched", "the daemon launched %s", r.token(claim.RoleMerger))
		merged := r.tick()
		mergeSHA := scratchCIHead(r.key, "merge")
		// GitHub merges first, as it does on github.com, and then sends the event.
		r.pulls.merge(r.number, mergeSHA, merged)
		id := r.publishPullRequest("closed", y, merged, map[string]string{"merged": "true", "merge_commit_sha": mergeSHA})
		r.awaitProcessed(id)
		r.waitPhase(phase.Done)
		r.fact("done-in-daemon", "the record after the merge: %s", fixRigPullRequest(r.pullRequest()))
	})

	r.walk("done-in-dispatch", func() {
		r.await(r.key+" done in Dispatch with its closed_at", func() bool {
			status, closed := r.dispatchIssue()
			return status == record.DoneStatus && closed != nil
		})
		status, closed := r.dispatchIssue()
		r.fact("dispatch-done", "Dispatch holds %s %s, closed_at %s", r.key, status, closed.UTC().Format("15:04:05.000"))
		r.await("every step-record and status row of "+r.key+" finished", func() bool {
			return r.unfinishedRows(record.OutboxKindStepRecord) == 0 && r.unfinishedRows(record.OutboxKindDispatchStatus) == 0
		})
		var steps json.RawMessage
		if err := r.personRead("/api/v1/issues/"+r.key+"/steps", &steps); err != nil {
			t.Fatalf("read the steps of %s: %v", r.key, err)
		}
		r.fact("steps", "GET /api/v1/issues/%s/steps: %s", r.key, steps)
	})

	r.watch.stop()
	r.fact("test-time", "the walk took %s from the rig's start", time.Since(r.started).Round(time.Millisecond))
}
