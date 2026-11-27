package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testpg"
)

// The fixture every whole-daemon test of a check a CI job proves runs on: the real Dispatch this
// checkout builds, its GitHub App reading the check runs of a fake GitHub (githubtest.Checks), its
// events on the test's NATS server, and the whole daemon consuming them from there, its record
// holding the rig's one issue. A CI state reaches the daemon by the trigger production uses: the
// listener's settlement of a pull request's checks (githubtest.PublishChecksSettlement), which
// Dispatch's delivery intake takes and which marks the check states of the issue at that pull
// request's open head owed, and Dispatch's settle, which reads the check runs at the head from
// GitHub and publishes each state that moved as a check.state_changed event. No table stores a
// CI run, so nothing else tells Dispatch that a run concluded.
//
// The order each user keeps: write the delivery settings (writeDeliverySettings); write the head
// (stageHead); wait for the daemon to hold the check's first state there, which the head's own
// settle publishes and which is not a failure (awaitFirstState), the proof that the event path
// works end to end; then set the fake's run at the head; then publish the settlement, or publish
// none for a control.

// scratchCIRepo is the repository of the rig's pull request: acme/widgets, the one the daemon's
// GitHub stand-in answers for (blockersGitHub).
const scratchCIRepo = liveRepo

// scratchCIPull is the number of the rig's pull request.
const scratchCIPull = 7

// scratchCICheck is the rig's check, whose ci_check names the CI job scratchCIJob.
const (
	scratchCICheck = "k-ci"
	scratchCIJob   = "unit"
)

// scratchCIVerification is the rig's Verification document: k-ci alone, a before_merge check its
// CI job unit proves.
const scratchCIVerification = `:::check{#k-ci negative_control="A unit job that exits 0 on a failing test." ci_check="unit"}` + "\n" +
	"The unit tests pass at the head.\n" +
	":::\n"

// scratchCIWithin is the bound each answer here is held to, measured from the operation that
// asks for it: the write of the head, the set of the fake's run, the publish of the settlement.
const scratchCIWithin = 10 * time.Second

// The cases of a check a CI job proves that say no run concluded there (Dispatch's results_ci.go):
// no run of the job at the head, and a run that has not concluded.
const (
	scratchCIAbsent       = "absent"
	scratchCINotCompleted = "not_completed"
	scratchCIFailure      = "failure"
)

// scratchCIRig is one run of the fixture: the NATS server, the fake GitHub, the scratch Dispatch,
// the daemon booted over both with its configuration and a pool of its database, Dispatch's own
// database, and the issue, filed in Dispatch and recorded by the daemon.
type scratchCIRig struct {
	t          *testing.T
	natsURL    string
	github     *githubtest.Checks
	dispatch   *scratchDispatch
	dispatchDB *pgxpool.Pool
	cfg        config.Config
	daemonDB   *pgxpool.Pool
	logs       *syncBuffer
	issue      string
	d          *daemon
}

// newScratchCIRig takes the NATS server, starts the fake GitHub and the scratch Dispatch on it,
// files the issue in Dispatch with the rig's Verification document, records it in the daemon's
// store, and boots the daemon. withApp gives Dispatch its App on the fake GitHub: without it,
// Dispatch reads GitHub for nothing, and its delivery settings cannot be written.
func newScratchCIRig(t *testing.T, withApp bool) *scratchCIRig {
	t.Helper()
	// The NATS server is taken first: testnats.URL empties it of every stream, and Dispatch
	// reconciles ENVOY_NOTIFICATIONS on it on its first connection, which the daemon then
	// consumes from.
	r := &scratchCIRig{t: t, natsURL: testnats.URL(t), github: githubtest.ServeChecks(t), logs: &syncBuffer{}}
	options := scratchDispatchOptions{natsURL: r.natsURL}
	if withApp {
		options.github = r.github
	}
	r.dispatch = startScratchDispatchOn(t, options)
	var err error
	if r.dispatchDB, err = pgxpool.New(context.Background(), r.dispatch.database); err != nil {
		t.Fatalf("open Dispatch's database: %v", err)
	}
	t.Cleanup(r.dispatchDB.Close)

	r.cfg = testConfig(t)
	// Dispatch takes a project key of at most ten characters; testConfig's is longer.
	r.cfg.Project = "C" + randomSuffix(t)[:9]
	r.cfg.Projects = map[string]config.Project{r.cfg.Project: {}}
	// A clone of the package's template of its own: the scratch Dispatch numbers its events from
	// one in a database of its own, so a daemon sharing a database with another test's would take
	// this run's events for ones it had processed (blockersDatabase says the same).
	r.cfg.PostgresDSN = testpg.Clone(t, "legion_daemon_run")
	r.cfg.NatsURLs = []string{r.natsURL}
	r.cfg.DispatchURL = r.dispatch.base
	r.cfg.DispatchTokenFile = filepath.Join(t.TempDir(), "dispatch-token")
	if err := os.WriteFile(r.cfg.DispatchTokenFile, []byte(r.dispatch.token+"\n"), 0o600); err != nil {
		t.Fatalf("write the scratch Dispatch bearer: %v", err)
	}
	// No controller tick runs: the first is an hour after the runtime starts.
	r.cfg.ControllerWakeInterval = time.Hour
	if r.daemonDB, err = pgxpool.New(context.Background(), r.cfg.PostgresDSN); err != nil {
		t.Fatalf("open the daemon's database: %v", err)
	}
	t.Cleanup(r.daemonDB.Close)

	r.fileIssue()
	r.awaitNotificationStream()
	r.boot()
	return r
}

// fileIssue creates the rig's project, maps scratchCIRepo to it as its default, files the issue in
// progress and carrying the legion label, as a running daemon leaves a root, with the rig's
// Verification document, and records it in the daemon's store as a tree in implementing, at the
// sequence of Dispatch's last event of it, so the daemon applies only what comes after.
func (r *scratchCIRig) fileIssue() {
	t, base := r.t, r.dispatch.base
	t.Helper()
	dispatchCall(t, base, http.MethodPost, "/api/v1/projects", map[string]string{"key": r.cfg.Project, "name": "CI states"}, nil)
	dispatchCall(t, base, http.MethodPut, "/api/v1/projects/"+r.cfg.Project+"/default-repository", map[string]any{"repo": scratchCIRepo}, nil)
	var created struct {
		Key string `json:"key"`
	}
	dispatchCall(t, base, http.MethodPost, "/api/v1/issues", map[string]any{"project": r.cfg.Project, "title": "Unit tests prove the head"}, &created)
	if created.Key == "" {
		t.Fatal("Dispatch created the rig's issue with no key")
	}
	r.issue = created.Key
	dispatchCall(t, base, http.MethodPatch, "/api/v1/issues/"+r.issue, map[string]any{"status": "in_progress", "labels": []string{"legion"}}, nil)
	dispatchCall(t, base, http.MethodPost, "/api/v1/issues/"+r.issue+"/artifacts", map[string]any{"name": "verification.md", "content": scratchCIVerification}, nil)
	var filed struct {
		Title   string `json:"title"`
		Rank    string `json:"rank"`
		LastSeq int64  `json:"last_seq"`
	}
	dispatchCall(t, base, http.MethodGet, "/api/v1/issues/"+r.issue, nil, &filed)
	project, err := claim.ProjectToken(r.cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	seedWorkflowTree(t, r.cfg.PostgresDSN, project, record.Issue{
		Key: r.issue, Repo: scratchCIRepo, DefaultBranch: "main", GitHubRepo: scratchCIRepo, Project: r.cfg.Project, Tree: r.issue,
		Title: filed.Title, Phase: phase.Implementing, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress",
		HandedOver: true, Rank: filed.Rank, LastDispatchSeq: filed.LastSeq,
	})
	t.Logf("filed %s in Dispatch, in progress with the check %s that the CI job %s proves, and recorded it in the daemon at sequence %d",
		r.issue, scratchCICheck, scratchCIJob, filed.LastSeq)
}

// awaitNotificationStream waits for Dispatch to have made the notification stream, which it does
// in the background once it connects: the daemon refuses to boot without it.
func (r *scratchCIRig) awaitNotificationStream() {
	r.t.Helper()
	js, err := jetstream.New(testnats.Connect(r.t, r.natsURL))
	if err != nil {
		r.t.Fatalf("open JetStream on %s: %v", r.natsURL, err)
	}
	var last error
	if !scratchCIPoll(scratchCIWithin, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, last = js.Stream(ctx, "ENVOY_NOTIFICATIONS")
		return last == nil
	}) {
		r.t.Fatalf("Dispatch made no ENVOY_NOTIFICATIONS stream on %s within %s: %v\n%s", r.natsURL, scratchCIWithin, last, r.dispatchLog())
	}
}

// boot starts the daemon on the rig's configuration over the fake runtime, which launches
// nothing: a pods runtime provisions each claim's workspace itself, so the daemon clones nothing.
// The boot's gh is named through the environment the boot reads (overrides.environ), this test
// binary, as workflowConfigOn names it, without a change to the process's own environment.
func (r *scratchCIRig) boot() {
	t := r.t
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	rt := fake.NewRuntime()
	rt.InPod = true
	o := fakeRuntime(rt, &built{})
	o.workflowTokens = &workflowTokenRecorder{}
	o.githubAPI = blockersGitHub(t)
	o.controllerFirstWake = time.Hour
	o.environ = append(os.Environ(), "LEGION_GH_PATH="+executable)
	r.d = startDaemonLogging(t, r.cfg, o, slog.New(slog.NewJSONHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
}

// writeDeliverySettings writes Dispatch's delivery settings, deploying from scratchCIRepo, and
// returns once its delivery intake reads the settlements the listener publishes.
func (r *scratchCIRig) writeDeliverySettings() {
	r.t.Helper()
	r.dispatch.writeDeliverySettings(scratchCIRepo)
}

// scratchCIHead is a commit of the rig's pull request, named for word.
func scratchCIHead(issue, word string) string {
	sum := sha256.Sum256([]byte(issue + " " + word))
	return hex.EncodeToString(sum[:])[:40]
}

// stageHead stores the rig's pull request in Dispatch's database, open at head, as #4940's live
// proof stages one (check_states_live_drive_test.go's stageHead): one pull_request_heads row at
// the head, attributed to the issue and read just now, so Dispatch's reconcile has no reason to
// read it from GitHub. A second call moves the row to the new head, as a push does. The table's
// triggers mark the issue's check states owed, as the envelope's own write does. It answers the
// moment before the write, which a bound on what the write causes is measured from.
func (r *scratchCIRig) stageHead(head string) time.Time {
	r.t.Helper()
	written := time.Now()
	if _, err := r.dispatchDB.Exec(context.Background(), `
		insert into pull_request_heads (repo, number, head_sha, head_ref, base_ref, state, issue_key, attribution_source,
			title, url, event_at, checked_at)
		values ($1, $2, $3, $4, 'main', 'open', $5, 'title_body', $6, $7, now(), now())
		on conflict (repo, number) do update set head_sha = excluded.head_sha, event_at = excluded.event_at, checked_at = excluded.checked_at`,
		scratchCIRepo, scratchCIPull, head, claim.IssueBranch(r.issue), r.issue, r.issue+" Unit tests prove the head",
		fmt.Sprintf("https://github.com/%s/pull/%d", scratchCIRepo, scratchCIPull)); err != nil {
		r.t.Fatalf("stage %s#%d open at %s in Dispatch's database: %v", scratchCIRepo, scratchCIPull, head, err)
	}
	r.t.Logf("staged %s#%d of %s open at %s in Dispatch's database", scratchCIRepo, scratchCIPull, r.issue, head)
	return written
}

// scratchCIState is the state the daemon holds of the rig's check at one head, as it took it from
// Dispatch's check.state_changed (the daemon's check_states row).
type scratchCIState struct {
	State, Reason, CICase string
	Seq                   int64
}

func (s scratchCIState) String() string {
	return fmt.Sprintf("state %s, reason %q, ci_case %q, Dispatch sequence %d", s.State, s.Reason, s.CICase, s.Seq)
}

// heldState is the daemon's check_states row of the rig's check at head, and whether it holds one.
func (r *scratchCIRig) heldState(head string) (scratchCIState, bool) {
	r.t.Helper()
	var held scratchCIState
	err := r.daemonDB.QueryRow(context.Background(), `
		select state, reason, ci_case, dispatch_seq from check_states where issue = $1 and repo = $2 and head = $3 and check_id = $4`,
		r.issue, scratchCIRepo, head, scratchCICheck).Scan(&held.State, &held.Reason, &held.CICase, &held.Seq)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return scratchCIState{}, false
	case err != nil:
		r.t.Fatalf("read the daemon's state of %s at %s of %s: %v", scratchCICheck, head, r.issue, err)
	}
	return held, true
}

// awaitFirstState waits, for scratchCIWithin from written, for the daemon to hold the rig's check's
// first state at head, which the head's own settle publishes, and requires it to say that no run
// concluded there, absent or not_completed: the proof that a state Dispatch publishes reaches the
// daemon, made before anything the test sets could be what moved it. A read that finds no row by
// the bound is UNKNOWN, with what it read.
func (r *scratchCIRig) awaitFirstState(head string, written time.Time) scratchCIState {
	r.t.Helper()
	var held scratchCIState
	var ok bool
	scratchCIPoll(time.Until(written.Add(scratchCIWithin)), func() bool {
		held, ok = r.heldState(head)
		return ok
	})
	switch {
	case !ok:
		r.t.Fatalf("UNKNOWN: the daemon holds no state of %s at %s of %s within %s of the head's write, so the event path from Dispatch's settle to the daemon could not be read; %s",
			scratchCICheck, head, r.issue, scratchCIWithin, r.readout())
	case held.CICase != scratchCIAbsent && held.CICase != scratchCINotCompleted:
		r.t.Fatalf("the daemon's first state of %s at %s is %s, want ci_case %s or %s: the head's own settle, before any run was set; %s",
			scratchCICheck, head, held, scratchCIAbsent, scratchCINotCompleted, r.readout())
	}
	r.t.Logf("the daemon holds %s at %s %s after the head's write: %s", scratchCICheck, head, time.Since(written).Round(time.Millisecond), held)
	return held
}

// failRun sets the run of the job unit at head concluded failure, with its page, on the fake
// GitHub, and answers the run and the moment before it was set.
func (r *scratchCIRig) failRun(head string, id int64) (githubtest.CheckRun, time.Time) {
	r.t.Helper()
	run := githubtest.CheckRun{
		ID: id, Name: scratchCIJob, Status: "completed", Conclusion: scratchCIFailure, CompletedAt: time.Now(),
		HTMLURL: fmt.Sprintf("https://github.com/%s/actions/runs/%d/job/%d", scratchCIRepo, id, id+1),
		Output:  githubtest.CheckRunOutput{Title: "1 test failed", Summary: "TestTheUnit failed."}, AppSlug: "github-actions",
	}
	set := time.Now()
	r.github.SetCheckRun(scratchCIRepo, head, run)
	r.t.Logf("set the run %s (%d) at %s concluded %s, its page %s", run.Name, run.ID, head, run.Conclusion, run.HTMLURL)
	return run, set
}

// publishSettlement publishes the listener's settlement of the rig's pull request at head, with
// statuses and runs, and answers the moment before the publish and the settlement's sequence on
// the notification stream.
func (r *scratchCIRig) publishSettlement(head string, statuses []githubtest.CommitStatus, runs ...githubtest.CheckRun) (time.Time, uint64) {
	r.t.Helper()
	published := time.Now()
	seq := githubtest.PublishChecksSettlement(r.t, r.natsURL, githubtest.ChecksSettlement{Repo: scratchCIRepo, Number: scratchCIPull, SHA: head,
		Runs: runs, Statuses: statuses})
	r.t.Logf("published the settlement of %s#%d at %s, sequence %d of the notification stream", scratchCIRepo, scratchCIPull, head, seq)
	return published, seq
}

// scratchCIStatus is a commit status as a settlement's payload names it (cistore.Summary's
// statuses): its context, its state and GitHub's created_at.
type scratchCIStatus struct {
	Context   string `json:"context"`
	State     string `json:"state"`
	CreatedAt string `json:"created_at"`
}

// settledStatuses is each commit status the settlement at seq of the notification stream names,
// and whether its payload carries statuses at all.
func (r *scratchCIRig) settledStatuses(seq uint64) ([]scratchCIStatus, bool) {
	r.t.Helper()
	js, err := jetstream.New(testnats.Connect(r.t, r.natsURL))
	if err != nil {
		r.t.Fatalf("open JetStream on %s: %v", r.natsURL, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := js.Stream(ctx, "ENVOY_NOTIFICATIONS")
	if err != nil {
		r.t.Fatalf("open the notification stream: %v", err)
	}
	message, err := stream.GetMsg(ctx, seq)
	if err != nil {
		r.t.Fatalf("read sequence %d of the notification stream: %v", seq, err)
	}
	var envelope struct {
		Payload string `json:"payload"`
	}
	if err := json.Unmarshal(message.Data, &envelope); err != nil {
		r.t.Fatalf("decode the envelope at sequence %d of the notification stream: %v", seq, err)
	}
	var payload struct {
		Statuses *[]scratchCIStatus `json:"statuses"`
	}
	if err := json.Unmarshal([]byte(envelope.Payload), &payload); err != nil {
		r.t.Fatalf("decode the settlement at sequence %d of the notification stream: %v", seq, err)
	}
	if payload.Statuses == nil {
		return nil, false
	}
	return *payload.Statuses, true
}

// deliveryAcked reads Dispatch's delivery durable until scratchCIWithin from from, and answers
// whether its ack floor reached the message at seq of the notification stream within that bound,
// with the floor it read last: a message the intake handed back stays below the floor.
func (r *scratchCIRig) deliveryAcked(seq uint64, from time.Time) (bool, string) {
	r.t.Helper()
	js, err := jetstream.New(testnats.Connect(r.t, r.natsURL))
	if err != nil {
		r.t.Fatalf("open JetStream on %s: %v", r.natsURL, err)
	}
	last := "unread"
	acked := scratchCIPoll(time.Until(from.Add(scratchCIWithin)), func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		consumer, err := js.PushConsumer(ctx, "ENVOY_NOTIFICATIONS", githubtest.DispatchDeliveryDurable)
		if err != nil {
			last = err.Error()
			return false
		}
		info, err := consumer.Info(ctx)
		if err != nil {
			last = err.Error()
			return false
		}
		last = fmt.Sprintf("stream sequence %d, %d acknowledgements pending", info.AckFloor.Stream, info.NumAckPending)
		return info.AckFloor.Stream >= seq
	})
	return acked, last
}

// failureWithin reads the daemon's state of the rig's check at head until scratchCIWithin from
// from, and answers whether it held ci_case failure within that bound, with the newest row it
// read and whether it read one at all.
func (r *scratchCIRig) failureWithin(head string, from time.Time) (held scratchCIState, found, failed bool) {
	r.t.Helper()
	scratchCIPoll(time.Until(from.Add(scratchCIWithin)), func() bool {
		held, found = r.heldState(head)
		return found && held.CICase == scratchCIFailure
	})
	return held, found, found && held.CICase == scratchCIFailure
}

// resultsName reports whether Dispatch's read of the issue's results names page: the read derives
// the check's result from the run it reads at the head from GitHub, and that result's observed
// text ends with the run's page.
func (r *scratchCIRig) resultsName(page string) (bool, string) {
	r.t.Helper()
	var results json.RawMessage
	dispatchCall(r.t, r.dispatch.base, http.MethodGet, "/api/v1/issues/"+r.issue+"/results", nil, &results)
	body := string(results)
	return strings.Contains(body, page), body
}

// readout is what a read that judged nothing could read: each state the daemon holds of the issue,
// each state Dispatch published of it, each request the fake GitHub answered 401 or 404, and the
// lines of Dispatch's log and of the daemon's that name check states or the delivery intake.
func (r *scratchCIRig) readout() string {
	r.t.Helper()
	ctx := context.Background()
	var daemonRows, dispatchRows []string
	if rows, err := r.daemonDB.Query(ctx, `select head, check_id, state, reason, ci_case, dispatch_seq from check_states where issue = $1 order by dispatch_seq`, r.issue); err != nil {
		daemonRows = append(daemonRows, "read failed: "+err.Error())
	} else {
		var head, check, state, reason, ciCase string
		var seq int64
		_, err := pgx.ForEachRow(rows, []any{&head, &check, &state, &reason, &ciCase, &seq}, func() error {
			daemonRows = append(daemonRows, fmt.Sprintf("%s at %s: %s %q %q seq %d", check, head, state, reason, ciCase, seq))
			return nil
		})
		if err != nil {
			daemonRows = append(daemonRows, "read failed: "+err.Error())
		}
	}
	if rows, err := r.dispatchDB.Query(ctx, `
		select head_sha, check_block_id, coalesce(state, ''), coalesce(reason, ''), coalesce(ci_case, '') from check_states
		where issue_key = $1 order by head_sha, check_block_id`, r.issue); err != nil {
		dispatchRows = append(dispatchRows, "read failed: "+err.Error())
	} else {
		var head, check, state, reason, ciCase string
		_, err := pgx.ForEachRow(rows, []any{&head, &check, &state, &reason, &ciCase}, func() error {
			dispatchRows = append(dispatchRows, fmt.Sprintf("%q at %s: %s %q %q", check, head, state, reason, ciCase))
			return nil
		})
		if err != nil {
			dispatchRows = append(dispatchRows, "read failed: "+err.Error())
		}
	}
	var daemonLines []string
	for line := range strings.SplitSeq(r.logs.String(), "\n") {
		if strings.Contains(line, "check state") {
			daemonLines = append(daemonLines, line)
		}
	}
	return fmt.Sprintf("the daemon holds of %s: [%s]; Dispatch published of it: [%s]; the fake GitHub refused %v and answered 404 to %v\nthe daemon's log of check states:\n%s\n%s",
		r.issue, strings.Join(daemonRows, "; "), strings.Join(dispatchRows, "; "), r.github.Refused(), r.github.Unanswered(),
		strings.Join(scratchCILast(daemonLines, 20), "\n"), r.dispatchLog())
}

// dispatchLog is the lines of Dispatch's log that name check states, the delivery intake, or a
// warning or error, the last 40 of them.
func (r *scratchCIRig) dispatchLog() string {
	var lines []string
	for line := range strings.SplitSeq(r.dispatch.output.String(), "\n") {
		if strings.Contains(line, "check states") || strings.Contains(line, "delivery") || strings.Contains(line, "level=WARN") ||
			strings.Contains(line, "level=ERROR") {
			lines = append(lines, line)
		}
	}
	return "Dispatch's log of check states and its delivery intake:\n" + strings.Join(scratchCILast(lines, 40), "\n")
}

// scratchCILast is the last n of lines.
func scratchCILast(lines []string, n int) []string {
	return lines[max(0, len(lines)-n):]
}

// scratchCIPoll polls condition every 100 ms until it holds or within has passed, and reports
// whether it held; it asks once even when within has passed already.
func scratchCIPoll(within time.Duration, condition func() bool) bool {
	for deadline := time.Now().Add(within); ; time.Sleep(100 * time.Millisecond) {
		if condition() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
	}
}

// TestTheScratchDispatchPublishesACIStateFromTheSettlement is the fixture's own proof: with
// Dispatch's delivery settings and the head written and the daemon holding the check's first
// state there, a run of the check's job concluded failure at the head and the listener's
// settlement of the pull request reach the daemon as the check's state failure at the head within
// scratchCIWithin of the publish, and Dispatch's read of the issue's results names the run's page.
// The results are read after the daemon holds the failure, so no read of them is what moved it.
//
// Two controls, each kept. The same order with no settlement, where no failure arrives within
// scratchCIWithin of the run's set, so the settlement is what carried it, while the results read
// names the page, so Dispatch could read the run. A Dispatch with the settings left out, no App
// and so no delivery settings, where the daemon holds no first state at the head that says no run
// concluded there, but the state of a check whose runs GitHub could not be read, so the App is
// what the fixture's first state needs.
//
// The third case is no control but the road's own reading: a Dispatch with its App and no
// delivery settings, where the first state and the settlement's failure both arrive all the same.
// The settle reads the head's runs whatever the settings hold, and Dispatch's delivery intake
// binds its durable once the settings cache's first read has answered, whatever it found, and
// hands a CI settlement to its handler with no settings saved, since neither the head row nor the
// check states' owed mark reads a setting (Envoy's delivery.Intake.Run and route). So the
// settings are no part of the road the settlement takes, and the control that stands for that
// road is the one with no settlement.
func TestTheScratchDispatchPublishesACIStateFromTheSettlement(t *testing.T) {
	t.Parallel()
	t.Run("the settlement publishes the failure", func(t *testing.T) {
		r := newScratchCIRig(t, true)
		r.writeDeliverySettings()
		head := scratchCIHead(r.issue, "x")
		r.awaitFirstState(head, r.stageHead(head))
		run, _ := r.failRun(head, 7001)
		published, _ := r.publishSettlement(head, nil, run)
		held, found, failed := r.failureWithin(head, published)
		took := time.Since(published).Round(time.Millisecond)
		switch {
		case !found:
			t.Fatalf("UNKNOWN: the daemon holds no state of %s at %s; %s", scratchCICheck, head, r.readout())
		case !failed:
			t.Fatalf("the daemon holds %s at %s %s, not ci_case %s, %s after the settlement's publish; %s",
				scratchCICheck, head, held, scratchCIFailure, scratchCIWithin, r.readout())
		}
		if named, body := r.resultsName(run.HTMLURL); !named {
			t.Fatalf("Dispatch's read of %s's results does not name the run's page %s: %s", r.issue, run.HTMLURL, body)
		}
		t.Logf("the daemon holds %s at %s %s, %s after the settlement's publish, and Dispatch's read of the results names %s",
			scratchCICheck, head, held, took, run.HTMLURL)
	})

	t.Run("control: with no settlement no failure arrives", func(t *testing.T) {
		r := newScratchCIRig(t, true)
		r.writeDeliverySettings()
		head := scratchCIHead(r.issue, "x")
		r.awaitFirstState(head, r.stageHead(head))
		run, set := r.failRun(head, 7001)
		held, found, failed := r.failureWithin(head, set)
		switch {
		case !found:
			t.Fatalf("UNKNOWN: the daemon holds no state of %s at %s; %s", scratchCICheck, head, r.readout())
		case failed:
			t.Fatalf("the daemon holds %s at %s %s within %s of the run's set with no settlement published: something other than the settlement told Dispatch; %s",
				scratchCICheck, head, held, scratchCIWithin, r.readout())
		}
		// The control could pass blind if Dispatch could not read the run at all: its own read of
		// the results reads GitHub at the head now, and names the page.
		if named, body := r.resultsName(run.HTMLURL); !named {
			t.Fatalf("UNKNOWN: Dispatch's read of %s's results does not name the run's page %s, so it could not read the run the settlement would have told it of: %s",
				r.issue, run.HTMLURL, body)
		}
		t.Logf("with no settlement the daemon still holds %s at %s %s, %s after the run's set, while Dispatch's read of the results names %s",
			scratchCICheck, head, held, scratchCIWithin, run.HTMLURL)
	})

	t.Run("control: with no App and so no settings no first state arrives", func(t *testing.T) {
		r := newScratchCIRig(t, false)
		head := scratchCIHead(r.issue, "x")
		written := r.stageHead(head)
		var seen []string
		var held scratchCIState
		var found bool
		scratchCIPoll(time.Until(written.Add(scratchCIWithin)), func() bool {
			if held, found = r.heldState(head); found && !slices.Contains(seen, held.String()) {
				seen = append(seen, held.String())
			}
			return found && (held.CICase == scratchCIAbsent || held.CICase == scratchCINotCompleted)
		})
		switch {
		case !found:
			t.Fatalf("UNKNOWN: the daemon holds no state of %s at %s of %s within %s of the head's write, so whether the App is what the first state needs could not be read; %s",
				scratchCICheck, head, r.issue, scratchCIWithin, r.readout())
		case held.CICase == scratchCIAbsent || held.CICase == scratchCINotCompleted:
			t.Fatalf("with no App the daemon holds %s at %s %s: the first state the fixture waits for came without it; %s",
				scratchCICheck, head, held, r.readout())
		}
		t.Logf("with no App the daemon holds %s at %s only as %v within %s of the head's write", scratchCICheck, head, seen, scratchCIWithin)
	})

	t.Run("with no delivery settings the settlement carries the failure all the same", func(t *testing.T) {
		r := newScratchCIRig(t, true)
		head := scratchCIHead(r.issue, "x")
		first := r.awaitFirstState(head, r.stageHead(head))
		run, _ := r.failRun(head, 7001)
		published, _ := r.publishSettlement(head, nil, run)
		held, found, failed := r.failureWithin(head, published)
		switch {
		case !found:
			t.Fatalf("UNKNOWN: the daemon holds no state of %s at %s; %s", scratchCICheck, head, r.readout())
		case !failed:
			t.Fatalf("with no delivery settings the daemon holds %s at %s %s, not ci_case %s, %s after the settlement's publish: the intake no longer hands a CI settlement to its handler with no settings saved, so the fixture's settings are part of the settlement's road after all; %s",
				scratchCICheck, head, held, scratchCIFailure, scratchCIWithin, r.readout())
		}
		// The reading could stand on nothing if Dispatch could not read the run at all.
		if named, body := r.resultsName(run.HTMLURL); !named {
			t.Fatalf("UNKNOWN: Dispatch's read of %s's results does not name the run's page %s, so it could not read the run the settlement told of: %s",
				r.issue, run.HTMLURL, body)
		}
		t.Logf("with no delivery settings the daemon held %s at %s first as %s, and holds %s within %s of the settlement's publish, while Dispatch's read of the results names %s",
			scratchCICheck, head, first, held, scratchCIWithin, run.HTMLURL)
	})
}

// TestTheScratchDispatchTakesASettlementThatNamesACommitStatus drives a settlement that names a
// commit status through the fixture. With the delivery settings and the head written and the
// daemon holding the check's first state there, the listener's settlement of the pull request at
// the head names the failed run of the check's job and one commit status. The notification stream
// holds it with that status as the listener names one (cistore.Summary's statuses: its context,
// its state and GitHub's created_at); Dispatch's delivery intake acknowledges it within
// scratchCIWithin of its publish; and the daemon holds the check's state failure at the head
// within scratchCIWithin of the publish. Dispatch reads no commit status for a check a CI job
// proves (its results read lists the head's check runs alone), so the state is the run's, and the
// status is the settlement's for its other readers, the daemon's intake among them. The same
// order with no settlement, the control, is TestTheScratchDispatchPublishesACIStateFromTheSettlement's.
func TestTheScratchDispatchTakesASettlementThatNamesACommitStatus(t *testing.T) {
	t.Parallel()
	r := newScratchCIRig(t, true)
	r.writeDeliverySettings()
	head := scratchCIHead(r.issue, "x")
	r.awaitFirstState(head, r.stageHead(head))
	run, _ := r.failRun(head, 7001)
	status := githubtest.CommitStatus{Context: "deploy/preview", State: "failure", CreatedAt: time.Now().UTC().Truncate(time.Second)}
	published, seq := r.publishSettlement(head, []githubtest.CommitStatus{status}, run)
	want := []scratchCIStatus{{Context: status.Context, State: status.State, CreatedAt: status.CreatedAt.Format(time.RFC3339)}}
	if got, named := r.settledStatuses(seq); !named || !slices.Equal(got, want) {
		t.Fatalf("the settlement at sequence %d of the notification stream names the commit statuses %v (a statuses member: %t), want %v",
			seq, got, named, want)
	}
	if acked, floor := r.deliveryAcked(seq, published); !acked {
		t.Fatalf("Dispatch's delivery intake did not acknowledge the settlement at sequence %d within %s of its publish: its durable's ack floor is at %s; %s",
			seq, scratchCIWithin, floor, r.readout())
	}
	held, found, failed := r.failureWithin(head, published)
	switch {
	case !found:
		t.Fatalf("UNKNOWN: the daemon holds no state of %s at %s; %s", scratchCICheck, head, r.readout())
	case !failed:
		t.Fatalf("the daemon holds %s at %s %s, not ci_case %s, %s after the publish of a settlement that names a commit status; %s",
			scratchCICheck, head, held, scratchCIFailure, scratchCIWithin, r.readout())
	}
	t.Logf("the settlement at sequence %d names %v, Dispatch's delivery intake acknowledged it, and the daemon holds %s at %s %s, %s after its publish",
		seq, want, scratchCICheck, head, held, time.Since(published).Round(time.Millisecond))
}
