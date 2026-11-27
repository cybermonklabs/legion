package daemon

// A merge forward asked of a role stays that role's work until it lands, whatever ends the role's
// claim on the way. The workflow hears no fact of two of those ends: an operator's stop and the
// agent's report of its own exit each retire the claim (supervise's stop and exit), and a row of
// the ask queued for a retired claim ends undelivered (deliverHeld). So the ask is sent again at
// the ready of the role's next launch, a launch later than the one a row of it was last delivered
// to (record.MergeForward.SentLaunch). These tests end the implementer's claim by each cause through
// its supervise machine, as the daemon does, and relaunch it through the operator's retry.
//
// The ready sends nothing while a row of the ask is still queued, and leaves the ask to that row.
// The row's executor decides its end from reads made before the end lands, and the role can be
// relaunched, and the new launch come ready, in between. The last tests drive each such order and
// hold that the new launch is told the ask once all the same.

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/record/fencetest"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// owedAskHead is the head of the pull request these tests hold an ask of.
const owedAskHead = "sha-1"

// newOwedAskRig is freshSessionRig's issue moved on into testing while the implementer's session
// of its first launch still runs: its pull request #86 is open at owedAskHead onto main, and the
// record holds the implementer's ask, made in testing, to merge main forward onto it, so the
// tester is held. The ask's row was delivered to that launch, which the record keeps, and left the
// outbox, as a delivered row does.
func newOwedAskRig(t *testing.T) *freshSessionRig {
	t.Helper()
	rig := newFreshSessionRig(t)
	rig.issue.Phase, rig.issue.Status = phase.Testing, "testing"
	asked := &record.MergeForward{Head: owedAskHead, Base: "main", Role: claim.RoleImplementer, Phase: phase.Testing,
		Why: record.ConflictCheckNotGreen, AskedAt: time.Now(), SentLaunch: rig.machine.Claim().Generation}
	if err := store.Transact(context.Background(), rig.pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := rig.records.PutIssue(ctx, tx, rig.issue); err != nil {
			return err
		}
		return rig.records.PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: rig.issue.Key,
			Repo: "acme/widgets", Number: 86, Branch: "legion/" + rig.issue.Key, HeadSHA: owedAskHead, HeadUpdatedAt: time.Now(),
			Failing: []string{}, CheckRuns: []record.AttemptRun{}, Base: "main", MergeForward: asked})
	}); err != nil {
		t.Fatalf("move %s into testing with the implementer's ask: %v", rig.issue.Key, err)
	}
	return rig
}

// owedAsk is the implementer's ask of owedAskHead on #86 in testing, as the act queues it.
var owedAsk = record.MergeForwardMessage{Role: claim.RoleImplementer, Step: record.MergeForwardAsk, Head: owedAskHead, Base: "main",
	Phase: phase.Testing, PullRequest: 86, Why: record.ConflictCheckNotGreen}

// wokenByTheAsk is mergeForwardRunner's LEGION-208 in testing with the implementer's ask of
// owedAskHead owed and its row queued, the implementer's claim as an idle release left it, and the
// workflow behind the supervisor, so each launch's ready is applied as the daemon applies it
// (workflowRuntime.terminal). The ask's first attempt has run: it woke the implementer, whose new
// launch is coming up, and published nothing, since no session holds the role yet.
func wokenByTheAsk(t *testing.T) (*outbox, *store.Pool, *holderPublisher, *supervise.Machine) {
	t.Helper()
	publisher := &holderPublisher{}
	asked := &record.MergeForward{Head: owedAskHead, Base: "main", Role: claim.RoleImplementer, Phase: phase.Testing,
		Why: record.ConflictCheckNotGreen, AskedAt: time.Now()}
	runner, sup, pool, _ := mergeForwardRunner(t, publisher, asked)
	engine := workflow.New(runner.records, workflow.Config{Project: "legion", Repositories: testRepositories}, quietLogger())
	sup.OnDurableState((&workflowRuntime{pool: pool, records: runner.records, handlers: []intake.Handler{engine}, log: quietLogger()}).terminal)
	token := mustClaimToken(t, "LEGION-208", claim.RoleImplementer)
	publisher.setAbsent(notify.RoleTopicPrefix + string(token))
	machine := releasedClaim(t, sup, claim.RoleImplementer)
	sup.deps.Conns.(*fake.Conns).Register(token, fake.NewConn())
	// The listener resolves the role to the session the claim records, as it does once that
	// session claimed its role.
	publisher.holdBy(func() string { return machine.View().Claim.Session })
	enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", owedAsk, time.Now().Add(-time.Minute)))
	drainAt(t, runner, 0, "the ask's first attempt")
	machine.Wait()
	if held := machine.Claim(); held.Released() || held.Generation < 2 {
		t.Fatalf("after the ask's first attempt the implementer is %s at launch %d, want it woken into a new launch", held.State, held.Generation)
	}
	return runner, pool, publisher, machine
}

// comesUp drives machine's current launch through its shim's hello and its agent's registration,
// and its ready too where ready is set.
func comesUp(t *testing.T, machine *supervise.Machine, ready bool) {
	t.Helper()
	if err := launchComesUp(machine, ready); err != nil {
		t.Fatal(err)
	}
}

// launchComesUp is comesUp's body, which answers what failed rather than failing the test, for an
// act that runs where the test may not stop: inside an executor's run, or on a goroutine of its
// own.
func launchComesUp(machine *supervise.Machine, ready bool) error {
	c := machine.Claim()
	events := []supervise.Event{
		supervise.StreamHello{Claim: c.Token, Generation: c.Generation},
		supervise.RequestRegister{Claim: c.Token, Generation: c.Generation, Session: c.Session, SessionFile: c.SessionFile},
	}
	if ready {
		events = append(events, supervise.RequestReady{Claim: c.Token, Generation: c.Generation, Session: c.Session})
	}
	for _, ev := range events {
		if err := machine.Handle(context.Background(), ev); err != nil {
			return fmt.Errorf("bring up the %s's launch %d: handle %T: %w", c.Role, c.Generation, ev, err)
		}
	}
	machine.Wait()
	return nil
}

// relaunched is the operator's retry of machine's claim, which failed or retired, and its new
// launch brought up through its ready (launchComesUp), so the ready is applied as the daemon
// applies it.
func relaunched(machine *supervise.Machine) error {
	ended := machine.Claim()
	if err := machine.Handle(context.Background(), supervise.RequestAskedRetry{Claim: ended.Token, By: supervise.SuspendedByOperator}); err != nil {
		return fmt.Errorf("the operator's retry of the %s: %w", ended.Role, err)
	}
	machine.Wait()
	if got := machine.Claim(); got.State != supervise.StateLaunching || got.Generation <= ended.Generation {
		return fmt.Errorf("after the retry the %s is %s at launch %d, want it launching past launch %d", ended.Role, got.State,
			got.Generation, ended.Generation)
	}
	return launchComesUp(machine, true)
}

// askToldOnce fails the test unless ask's role was told ask once in all, the outbox holds no
// merge-forward row of LEGION-208 any more, and the record's ask keeps launch as the one it was
// told to.
func askToldOnce(t *testing.T, pool *store.Pool, publisher *holderPublisher, ask record.MergeForwardMessage, launch uint64) {
	t.Helper()
	_, delivered := publisher.snapshot()
	if len(delivered) != 1 || delivered[0].message != ask.Message("LEGION-208", record.PullRequest{Repo: "acme/widgets"}) {
		t.Errorf("the %s was told %d messages (%+v), want the ask once, at its launch %d", ask.Role, len(delivered), delivered, launch)
	}
	if rows := outboxRowsOfKind(t, pool, "LEGION-208", record.OutboxKindMergeForward); rows != 0 {
		t.Errorf("the outbox holds %d merge-forward rows of LEGION-208, want none: no second ask", rows)
	}
	if sent := recordedSentLaunch(t, pool); sent != launch {
		t.Errorf("the record's ask was sent at launch %d, want launch %d, the one it was told to", sent, launch)
	}
}

// recordedSentLaunch is the launch the record's ask of LEGION-208 keeps as the one it was told to
// (record.MergeForward.SentLaunch), 0 for none.
func recordedSentLaunch(t *testing.T, pool *store.Pool) uint64 {
	t.Helper()
	var sent uint64
	if err := pool.QueryRow(context.Background(), `select coalesce((merge_forward->>'sentLaunch')::bigint, 0) from pull_requests where issue = 'LEGION-208'`).
		Scan(&sent); err != nil {
		t.Fatalf("read the record's ask: %v", err)
	}
	return sent
}

// The ask's own row wakes the released implementer, and the woken launch comes ready while the row
// is still queued: the row reaches that launch by its own delivery, so its ready writes no second
// ask, and the implementer is told the ask once.
func TestAnAskWhoseRowWakesItsRoleIsToldOnce(t *testing.T) {
	t.Parallel()
	runner, pool, publisher, machine := wokenByTheAsk(t)
	comesUp(t, machine, true)
	publisher.setAbsent()
	drainAt(t, runner, 10*time.Minute, "the attempt once the woken session holds the role")
	drainAt(t, runner, 20*time.Minute, "a later attempt")
	askToldOnce(t, pool, publisher, owedAsk, machine.Claim().Generation)
}

// The ask's row reaches the woken launch before that launch's ready: the delivery records the
// launch on the record's ask, so the ready of the same launch sends nothing more.
func TestTheReadyOfTheLaunchAnAskWasDeliveredToSendsNothing(t *testing.T) {
	t.Parallel()
	runner, pool, publisher, machine := wokenByTheAsk(t)
	comesUp(t, machine, false)
	publisher.setAbsent()
	drainAt(t, runner, 10*time.Minute, "the attempt once the woken session holds the role")
	launch := machine.Claim().Generation
	if err := machine.Handle(context.Background(), supervise.RequestReady{Claim: machine.Claim().Token, Generation: launch,
		Session: machine.Claim().Session}); err != nil {
		t.Fatalf("the implementer's ready: %v", err)
	}
	machine.Wait()
	drainAt(t, runner, 20*time.Minute, "a later attempt")
	askToldOnce(t, pool, publisher, owedAsk, launch)
}

// A launch that resumes the session an earlier launch was told the ask in can be told the ask a
// second time. The second ask names the same head, and its delivery changes nothing the daemon
// holds: after the second row of the ask reaches the launch the first reached, the record's ask is
// as the first left it, and the outbox holds no row more.
func TestASecondAskOfTheSameHeadChangesNothingTheDaemonHolds(t *testing.T) {
	t.Parallel()
	runner, pool, publisher, machine := wokenByTheAsk(t)
	comesUp(t, machine, true)
	publisher.setAbsent()
	drainAt(t, runner, 10*time.Minute, "the attempt that delivers the first ask")
	recorded := func() record.PullRequest {
		t.Helper()
		var pr *record.PullRequest
		if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) (err error) {
			pr, err = runner.records.PullRequest(ctx, tx, "LEGION-208")
			return err
		}); err != nil || pr == nil {
			t.Fatalf("read the pull request of LEGION-208: %+v, %v", pr, err)
		}
		return *pr
	}
	first, rows := recorded(), outboxRows(t, pool)
	enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", owedAsk, time.Now()))
	drainAt(t, runner, 20*time.Minute, "the attempt that delivers the second ask")
	if _, delivered := publisher.snapshot(); len(delivered) != 2 || delivered[0].message != delivered[1].message {
		t.Errorf("the implementer was told %d messages (%+v), want the same ask twice", len(delivered), delivered)
	}
	if second := recorded(); !reflect.DeepEqual(second.MergeForward, first.MergeForward) {
		t.Errorf("after the second ask the record's ask is %+v, want it as the first ask left it, %+v", second.MergeForward, first.MergeForward)
	}
	if left := outboxRows(t, pool); left != rows {
		t.Errorf("after the second ask the outbox holds %d rows of LEGION-208, want %d, as after the first", left, rows)
	}
	if first.MergeForward == nil || first.MergeForward.SentLaunch != machine.Claim().Generation {
		t.Errorf("the first ask left the record's ask %+v, want it told launch %d", first.MergeForward, machine.Claim().Generation)
	}
}

// implementersAsk is the record's ask of owedAskHead to the implementer, made in testing, as the
// act writes it.
func implementersAsk() record.MergeForward {
	return record.MergeForward{Head: owedAskHead, Base: "main", Role: claim.RoleImplementer, Phase: phase.Testing,
		Why: record.ConflictCheckNotGreen, AskedAt: time.Now()}
}

// failedWithTheAsk is mergeForwardRunnerAt's LEGION-208 standing in at, its pull request #86 open
// at owedAskHead with asked as the record's merge forward, the claim of ask's role as a failure of
// its first launch left it, the workflow behind the supervisor as wokenByTheAsk has it, and ask's
// row queued. The listener takes every publish.
func failedWithTheAsk(t *testing.T, at phase.Phase, asked record.MergeForward, ask record.MergeForwardMessage) (*outbox, *store.Pool, *holderPublisher, *supervise.Machine) {
	t.Helper()
	publisher := &holderPublisher{}
	runner, sup, pool, _ := mergeForwardRunnerAt(t, publisher, at, workflow.PhaseStatus(at), record.PullRequestOpen, &asked)
	engine := workflow.New(runner.records, workflow.Config{Project: "legion", Repositories: testRepositories}, quietLogger())
	sup.OnDurableState((&workflowRuntime{pool: pool, records: runner.records, handlers: []intake.Handler{engine}, log: quietLogger()}).terminal)
	token := mustClaimToken(t, "LEGION-208", ask.Role)
	machine, _, err := sup.Create(context.Background(), supervise.Claim{Token: token, Project: "legion", Tree: "LEGION-208",
		Issue: "LEGION-208", Role: ask.Role, Generation: 1, ServingGeneration: 1, State: supervise.StateFailed,
		Failure: "launch failures ran out", Session: "ses-" + string(ask.Role), SessionFile: t.TempDir() + "/session.jsonl"}, "")
	if err != nil {
		t.Fatalf("create the failed %s's claim: %v", ask.Role, err)
	}
	sup.deps.Conns.(*fake.Conns).Register(token, fake.NewConn())
	publisher.holdBy(func() string { return machine.View().Claim.Session })
	enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", ask, time.Now().Add(-time.Minute)))
	return runner, pool, publisher, machine
}

// relaunchBeforeTheEnd runs the row failedWithTheAsk queued with the operator's retry of the failed
// claim, and the ready of its new launch, made between the executor's read that ends the row and
// that end (beforeUndeliveredEnd), after between where it is set; then it runs what is still due.
// It fails the test unless that came in between once, and the ready left the row queued and wrote
// no second one: the ready left the ask to the row.
func relaunchBeforeTheEnd(t *testing.T, runner *outbox, pool *store.Pool, machine *supervise.Machine, between func() error) {
	t.Helper()
	came := 0
	runner.beforeUndeliveredEnd = func(record.OutboxRow) {
		if came++; came > 1 {
			return
		}
		if between != nil {
			if err := between(); err != nil {
				t.Error(err)
				return
			}
		}
		if err := relaunched(machine); err != nil {
			t.Error(err)
			return
		}
		if rows := outboxRowsOfKind(t, pool, "LEGION-208", record.OutboxKindMergeForward); rows != 1 {
			t.Errorf("after the ready of launch %d the outbox holds %d merge-forward rows of LEGION-208, want the one row still queued",
				machine.Claim().Generation, rows)
		}
	}
	drainAt(t, runner, 0, "the attempt that reads the row's end")
	if came != 1 {
		t.Fatalf("the executor read the row's end %d times, want once: the relaunch came between nothing", came)
	}
	drainAt(t, runner, 10*time.Minute, "a later attempt")
}

// The implementer, asked to merge the base forward while the issue stays in testing, has failed
// when the ask's row runs: the executor reads the claim failed, and before the row ends the
// operator's retry relaunches the implementer and the new launch comes ready while the row is
// still queued, so that ready sends nothing. The end reads the claim again under the fact lock,
// where it holds a live launch the ask was not told, so the ask is sent to that launch, which is
// told it once. An end decided on the first read would leave the new launch told nothing.
func TestAnAskWhoseFailedClaimIsRetriedBeforeItsRowEndsReachesTheNewLaunch(t *testing.T) {
	t.Parallel()
	runner, pool, publisher, machine := failedWithTheAsk(t, phase.Testing, implementersAsk(), owedAsk)
	relaunchBeforeTheEnd(t, runner, pool, machine, nil)
	askToldOnce(t, pool, publisher, owedAsk, machine.Claim().Generation)
}

// retriedIntoAFreshSession is the operator's retry of machine's failed claim into launch 2, whose
// resume of the session the claim records the runtime refuses (runtime.ErrSessionFileMissing), so
// launch 2 forgets that session and starts a fresh one in the same launch (supervise's startFresh).
// It fails the test unless launch 2 is launching with no session recorded.
func retriedIntoAFreshSession(t *testing.T, runner *outbox, machine *supervise.Machine) {
	t.Helper()
	failed := machine.Claim()
	runner.supervisor.deps.Runtime.(*fake.Runtime).ScriptResume(fake.SpawnResult{Err: fmt.Errorf("resume %s: %w: the session table holds no such session",
		failed.Token, runtime.ErrSessionFileMissing)})
	if err := machine.Handle(context.Background(), supervise.RequestAskedRetry{Claim: failed.Token, By: supervise.SuspendedByOperator}); err != nil {
		t.Fatalf("the operator's retry of the %s: %v", failed.Role, err)
	}
	machine.Wait()
	if launching := machine.Claim(); launching.State != supervise.StateLaunching || launching.Generation != 2 || launching.Session != "" {
		t.Fatalf("after the retry the %s is %s at launch %d recording session %q, want launch 2 launching a fresh session",
			failed.Role, launching.State, launching.Generation, launching.Session)
	}
}

// freshSessionTold brings machine's launch 2 up on fresh, a session its claim never recorded,
// which registers and comes ready; runs the row's attempts while the session of launch 1 still
// holds the role, then once fresh holds it; and answers how many times fresh was told want.
func freshSessionTold(t *testing.T, runner *outbox, publisher *holderPublisher, machine *supervise.Machine, fresh, want string) int {
	t.Helper()
	token := machine.Claim().Token
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: token, Generation: 2},
		supervise.RequestRegister{Claim: token, Generation: 2, Session: fresh, SessionFile: t.TempDir() + "/fresh.jsonl"},
		supervise.RequestReady{Claim: token, Generation: 2, Session: fresh},
	} {
		if err := machine.Handle(context.Background(), ev); err != nil {
			t.Fatalf("bring up the %s's launch 2: handle %T: %v", machine.Claim().Role, ev, err)
		}
	}
	machine.Wait()
	drainAt(t, runner, 10*time.Minute, "an attempt while the old session still holds the role")
	publisher.holdBy(func() string { return fresh })
	drainAt(t, runner, 20*time.Minute, "an attempt once the fresh session holds the role")
	drainAt(t, runner, 30*time.Minute, "a later attempt")
	_, delivered := publisher.snapshot()
	told := 0
	for _, published := range delivered {
		if published.message == want && published.holder == fresh {
			told++
		}
	}
	return told
}

// The ask's row runs while the implementer's launch 2 is still coming up and the listener still
// names the session of launch 1 as the role's holder: that session's process died, and its clean
// stop at the listener has not landed (the daemon's stop of a session is asynchronous and may
// fail, leaving the registration until it expires). Launch 2 cannot resume that session and starts
// a fresh one in the same launch (supervise's startFresh), which registers and comes ready, and
// claims the role only later. A copy stored for the old session tells launch 2 nothing, so the
// fresh session is told the ask once, by the row, once it holds the role, and the record keeps
// launch 2 as the one told. Crediting launch 2 with the copy the old session took would leave the
// fresh session told nothing.
func TestAnAskReachesTheFreshSessionOfTheLaunchWhileTheOldSessionHoldsTheRole(t *testing.T) {
	t.Parallel()
	runner, pool, publisher, machine := failedWithTheAsk(t, phase.Testing, implementersAsk(), owedAsk)
	publisher.holdBy(func() string { return "ses-implementer" })
	retriedIntoAFreshSession(t, runner, machine)
	drainAt(t, runner, 0, "the ask's attempt while launch 2 comes up")
	want := owedAsk.Message("LEGION-208", record.PullRequest{Repo: "acme/widgets"})
	if told := freshSessionTold(t, runner, publisher, machine, "ses-fresh-implementer", want); told != 1 {
		_, delivered := publisher.snapshot()
		t.Errorf("the fresh session of launch 2 was told the ask %d times (every delivery: %+v), want once", told, delivered)
	}
	if rows := outboxRowsOfKind(t, pool, "LEGION-208", record.OutboxKindMergeForward); rows != 0 {
		t.Errorf("the outbox holds %d merge-forward rows of LEGION-208, want none", rows)
	}
	if sent := recordedSentLaunch(t, pool); sent != 2 {
		t.Errorf("the record's ask was told launch %d, want launch 2", sent)
	}
}

// A merge-queue removal is the merger's to read, and is delivered as a merge forward's message is
// (deliverHeld): its row runs while the merger's launch 2 is still coming up and the listener still
// names the session of launch 1 as the role's holder. Launch 2 cannot resume that session and
// starts a fresh one in the same launch, which registers and comes ready, and claims the role only
// later. A copy stored for the old session tells launch 2 nothing, so the row waits, and the fresh
// session is told the removal once it holds the role. A row that ended on the copy the old session
// took would leave the merger never told that its pull request left the merge queue.
func TestARemovalReachesTheFreshSessionOfTheMergersLaunchWhileTheOldSessionHoldsTheRole(t *testing.T) {
	t.Parallel()
	publisher := &holderPublisher{}
	runner, sup, pool, _ := mergeForwardRunnerAt(t, publisher, phase.Merging, workflow.PhaseStatus(phase.Merging), record.PullRequestOpen, nil)
	token := mustClaimToken(t, "LEGION-208", claim.RoleMerger)
	machine, _, err := sup.Create(context.Background(), supervise.Claim{Token: token, Project: "legion", Tree: "LEGION-208",
		Issue: "LEGION-208", Role: claim.RoleMerger, Generation: 1, ServingGeneration: 1, State: supervise.StateFailed,
		Failure: "deaths with work outstanding ran out", Session: "ses-merger", SessionFile: t.TempDir() + "/session.jsonl"}, "")
	if err != nil {
		t.Fatalf("create the failed merger's claim: %v", err)
	}
	sup.deps.Conns.(*fake.Conns).Register(token, fake.NewConn())
	removal := record.MergeQueueRemoval{Number: 86, Head: "sha-1"}
	publisher.holdBy(func() string { return "ses-merger" })
	retriedIntoAFreshSession(t, runner, machine)
	enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", removal, time.Now().Add(-time.Minute)))
	drainAt(t, runner, 0, "the removal's attempt while launch 2 comes up")
	if told := freshSessionTold(t, runner, publisher, machine, "ses-fresh-merger", removal.Message("LEGION-208", "acme/widgets")); told != 1 {
		_, delivered := publisher.snapshot()
		t.Errorf("the fresh session of the merger's launch 2 was told the removal %d times (every delivery: %+v), want once", told, delivered)
	}
	if rows := outboxRowsOfKind(t, pool, "LEGION-208", record.OutboxKindMergeQueueRemoval); rows != 0 {
		t.Errorf("the outbox holds %d merge-queue removal rows of LEGION-208, want none", rows)
	}
}

// The merger was asked to merge the base forward in merging, and the architect moved the issue
// back to accepting, where that ask is owed to nobody, before the ask's row ran; the merger's
// claim has failed. The row's attempt reads that the workflow no longer delivers it. Before the
// row ends, the issue moves into merging again, where the ask is owed once more and the move asks
// nothing again (mergeForwardOnEntry), and the operator's retry relaunches the merger, whose new
// launch comes ready while the row is still queued and sends nothing. The end reads the ask's
// owing again under the fact lock, so the merger's new launch is told the ask once.
func TestAMergersAskOwedAgainBeforeItsRowEndsReachesItsNewLaunch(t *testing.T) {
	t.Parallel()
	ask := record.MergeForwardMessage{Role: claim.RoleMerger, Step: record.MergeForwardAsk, Head: owedAskHead, Base: "main",
		Phase: phase.Merging, PullRequest: 86, Why: record.ConflictMergeNext}
	runner, pool, publisher, machine := failedWithTheAsk(t, phase.Accepting, record.MergeForward{Head: owedAskHead, Base: "main",
		Role: claim.RoleMerger, Phase: phase.Merging, Why: record.ConflictMergeNext, AskedAt: time.Now()}, ask)
	relaunchBeforeTheEnd(t, runner, pool, machine, func() error {
		return store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
			issue, err := runner.records.Issue(ctx, tx, "LEGION-208")
			if err != nil || issue == nil {
				return fmt.Errorf("read LEGION-208 to move it into merging: %+v, %w", issue, err)
			}
			issue.Phase, issue.Status = phase.Merging, workflow.PhaseStatus(phase.Merging)
			return runner.records.PutIssue(ctx, tx, *issue)
		})
	})
	askToldOnce(t, pool, publisher, ask, machine.Claim().Generation)
}

// The lines an ask's row the fence refuses is read by: what it logs once its end commits
// (lateMergeForwardAsk), what every other late row logs (outlived), and what an ask's end logs
// where it sent the ask again (endMergeForwardAsk).
const (
	lateAskLine    = "outbox merge-forward ask of an earlier run or phase entry of its issue finished undelivered"
	outlivedLine   = "outbox row of an earlier run or phase entry of its issue; finished without acting"
	askedAgainLine = "outbox merge-forward ask sent again, to a launch its row did not reach"
)

// queuedAsk is one merge-forward row of LEGION-208 as the outbox holds it: its id, and its payload
// with the run and the phase entry it was queued at.
type queuedAsk struct {
	id      int64
	payload record.MergeForwardMessage
}

// queuedAsks is every merge-forward row of LEGION-208, oldest first.
func queuedAsks(t *testing.T, pool *store.Pool) []queuedAsk {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select id, kind, issue, payload from outbox where issue = 'LEGION-208' and kind = $1 order by id`,
		string(record.OutboxKindMergeForward))
	if err != nil {
		t.Fatalf("read the merge-forward rows: %v", err)
	}
	defer rows.Close()
	var asks []queuedAsk
	for rows.Next() {
		var row record.OutboxRow
		if err := rows.Scan(&row.ID, &row.Kind, &row.Issue, &row.Payload); err != nil {
			t.Fatalf("scan a merge-forward row: %v", err)
		}
		asks = append(asks, queuedAsk{id: row.ID, payload: mustDecode(t, row).(record.MergeForwardMessage)})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the merge-forward rows: %v", err)
	}
	return asks
}

// recordedPullRequest is LEGION-208's pull request as the record holds it.
func recordedPullRequest(t *testing.T, runner *outbox, pool *store.Pool) record.PullRequest {
	t.Helper()
	var pr *record.PullRequest
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) (err error) {
		pr, err = runner.records.PullRequest(ctx, tx, "LEGION-208")
		return err
	}); err != nil || pr == nil {
		t.Fatalf("read the pull request of LEGION-208: %+v, %v", pr, err)
	}
	return *pr
}

// moveOn writes LEGION-208 and its pull request as move leaves them, and the phases row move
// answers, which the start of the phase it moves into raises, in one transaction.
func moveOn(t *testing.T, runner *outbox, pool *store.Pool, move func(*record.Issue, *record.PullRequest) *record.PhaseRow) {
	t.Helper()
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		issue, err := runner.records.Issue(ctx, tx, "LEGION-208")
		if err != nil || issue == nil {
			return fmt.Errorf("read LEGION-208: %+v, %w", issue, err)
		}
		pr, err := runner.records.PullRequest(ctx, tx, "LEGION-208")
		if err != nil || pr == nil {
			return fmt.Errorf("read the pull request of LEGION-208: %+v, %w", pr, err)
		}
		started := move(issue, pr)
		if err := runner.records.PutIssue(ctx, tx, *issue); err != nil {
			return err
		}
		if err := runner.records.PutPullRequest(ctx, tx, *pr); err != nil {
			return err
		}
		if started == nil {
			return nil
		}
		return runner.records.PutPhase(ctx, tx, *started)
	}); err != nil {
		t.Fatalf("move LEGION-208 on: %v", err)
	}
}

// The implementer is asked to merge the base forward while the issue stays in testing, and its
// claim was released before the ask's row was delivered: the row wakes it, and the woken launch
// comes ready while the row is still queued, so that ready sends nothing and leaves the ask to the
// row (the workflow's askAgainAtLaunch). Then the issue moves on before the row runs again, to a
// later phase entry or a new run, where the fence refuses the row (record.LateAt, decision 10 of
// dispatch://INFRA-4730): it is never published, and it records no launch as told. It still ends
// through the ask's end (lateMergeForwardAsk): where the issue and its pull request still owe the
// implementer an ask, one new row of it is queued at the fence the issue stands at, for the ready
// launch, and that launch is told it once; where they owe none, nothing is queued or told. A row
// the fence finished without that end, as it finished every late row before, left the ready
// launch never asked while the ask stayed owed.
func TestAnAskOfAnEarlierEntryOrRunIsSentAgainToTheReadyLaunchOnlyWhileOwed(t *testing.T) {
	t.Parallel()
	// intoReviewing is the move forward into reviewing, whose start raises the reviewer's entry;
	// the implementer's ask made in testing stays owed there.
	intoReviewing := func(issue *record.Issue, _ *record.PullRequest) *record.PhaseRow {
		issue.Phase, issue.Status = phase.Reviewing, workflow.PhaseStatus(phase.Reviewing)
		return &record.PhaseRow{Issue: issue.Key, Role: claim.RoleReviewer, Entry: 1}
	}
	for _, tc := range []struct {
		name string
		move func(*record.Issue, *record.PullRequest) *record.PhaseRow
		// owed is the ask the issue then owes the implementer, as its row says it with no launch
		// and no fence, nil where it owes none.
		owed *record.MergeForwardMessage
	}{
		{
			name: "an ask still owed at a later phase entry is sent once to the ready launch",
			move: intoReviewing,
			owed: &record.MergeForwardMessage{Role: claim.RoleImplementer, Step: record.MergeForwardAsk, Head: owedAskHead, Base: "main",
				Phase: phase.Reviewing, PullRequest: 86, Why: record.ConflictCheckNotGreen},
		},
		{
			name: "an ask that landed before a later phase entry is sent to nobody",
			move: func(issue *record.Issue, pr *record.PullRequest) *record.PhaseRow {
				landed := *pr.MergeForward
				landed.Landed, pr.HeadSHA = "sha-2", "sha-2"
				pr.MergeForward = &landed
				return intoReviewing(issue, pr)
			},
		},
		{
			name: "an ask of an earlier run is sent once to the ready launch only as the new run's own ask",
			move: func(issue *record.Issue, pr *record.PullRequest) *record.PhaseRow {
				issue.Generation++
				pr.MergeForward = &record.MergeForward{Head: owedAskHead, Base: "main", Role: claim.RoleImplementer, Phase: phase.Testing,
					Why: record.ConflictRulesUnread, AskedAt: time.Now()}
				return nil
			},
			owed: &record.MergeForwardMessage{Role: claim.RoleImplementer, Step: record.MergeForwardAsk, Head: owedAskHead, Base: "main",
				Phase: phase.Testing, PullRequest: 86, Why: record.ConflictRulesUnread},
		},
		{
			name: "an ask of an earlier run whose new run owes none is sent to nobody",
			move: func(issue *record.Issue, pr *record.PullRequest) *record.PhaseRow {
				issue.Generation++
				pr.MergeForward = nil
				return nil
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner, pool, publisher, machine := wokenByTheAsk(t)
			logs := &syncBuffer{}
			runner.log = slog.New(slog.NewJSONHandler(logs, nil))
			queued := queuedAsks(t, pool)
			if len(queued) != 1 {
				t.Fatalf("after the ask's first attempt the outbox holds the asks %+v, want its one row", queued)
			}
			old := queued[0]
			comesUp(t, machine, true)
			launch := machine.Claim().Generation
			if asks := queuedAsks(t, pool); len(asks) != 1 || asks[0].id != old.id {
				t.Fatalf("after the ready of launch %d the outbox holds the asks %+v, want row %d alone: the ready leaves the ask to it", launch, asks, old.id)
			}
			moveOn(t, runner, pool, tc.move)
			now, err := runner.fenceOf(context.Background(), "LEGION-208", false)
			if err != nil {
				t.Fatal(err)
			}
			if !record.LateAt(now, old.payload) {
				t.Fatalf("after the move LEGION-208 stands at %+v, where the ask's row of %+v is not late", now, old.payload)
			}
			moved := recordedPullRequest(t, runner, pool).MergeForward
			publisher.setAbsent()
			attempts, _ := publisher.snapshot()

			drainAt(t, runner, 10*time.Minute, "the late row's attempt")
			if published, _ := publisher.snapshot(); published != attempts {
				t.Errorf("the late row's attempt made %d publishes, want none: the fence refuses the row", published-attempts)
			}
			if sent := recordedSentLaunch(t, pool); sent != 0 {
				t.Errorf("after the late row's end the record's ask names launch %d as told, want none: the row reached no launch", sent)
			}
			var fresh int64
			asks := queuedAsks(t, pool)
			if tc.owed == nil {
				if len(asks) != 0 {
					t.Errorf("after the late row's end the outbox holds the asks %+v, want none: the issue and its pull request owe no ask", asks)
				}
			} else {
				want := *tc.owed
				want.SentLaunch, want.Generation, want.Entry = launch, now.Generation, now.Entry
				if len(asks) != 1 || asks[0].id == old.id || asks[0].payload != want {
					t.Errorf("after the late row's end the outbox holds the asks %+v, want one new row of %+v: the ask still owed, at the fence the issue stands at, for the ready launch %d; without it nothing asks that launch",
						asks, want, launch)
				} else {
					fresh = asks[0].id
				}
			}
			late := loggedLines(t, logs.String(), lateAskLine)
			if len(late) != 1 || late[0]["row"] != float64(old.id) || late[0]["current_generation"] != float64(now.Generation) ||
				late[0]["current_entry"] != float64(now.Entry) {
				t.Errorf("the late row logged %v, want one line naming row %d and the fence %+v the issue stands at", late, old.id, now)
			}
			for _, line := range loggedLines(t, logs.String(), outlivedLine) {
				if line["row"] == float64(old.id) {
					t.Errorf("the late row logged %v, want no line that it finished without acting: its end settled the ask", line)
				}
			}
			sentAgain := 0
			if tc.owed != nil {
				sentAgain = 1
			}
			if again := loggedLines(t, logs.String(), askedAgainLine); len(again) != sentAgain {
				t.Errorf("the late row's end logged %v, want the ask sent again %d times", again, sentAgain)
			}

			drainAt(t, runner, 20*time.Minute, "the attempt after the late row's end")
			_, delivered := publisher.snapshot()
			if rows := outboxRowsOfKind(t, pool, "LEGION-208", record.OutboxKindMergeForward); rows != 0 {
				t.Errorf("the outbox holds %d merge-forward rows of LEGION-208, want none", rows)
			}
			if tc.owed == nil {
				if len(delivered) != 0 {
					t.Errorf("the implementer was told %+v, want nothing", delivered)
				}
				if after := recordedPullRequest(t, runner, pool).MergeForward; !reflect.DeepEqual(after, moved) {
					t.Errorf("the record's ask is %+v, want it as the move left it, %+v", after, moved)
				}
				return
			}
			if want := tc.owed.Message("LEGION-208", record.PullRequest{Repo: "acme/widgets"}); len(delivered) != 1 ||
				delivered[0].message != want || delivered[0].key != record.OutboxKey(fresh) || delivered[0].key == record.OutboxKey(old.id) {
				t.Errorf("the implementer was told %+v, want the ask still owed once, %q, under the new row's key %s and never the late row's %s",
					delivered, want, record.OutboxKey(fresh), record.OutboxKey(old.id))
			}
			if sent := recordedSentLaunch(t, pool); sent != launch {
				t.Errorf("the record's ask names launch %d as told, want launch %d, which the new row reached", sent, launch)
			}
		})
	}
}

// endHeld is a record.Store that runs before once, inside the transaction that ends an outbox row,
// after every read that end makes and before it commits: at the first read of an issue in the
// transaction that finished the row under its lease, which is the last read of a merge-forward
// ask's end (endMergeForwardAsk, after the claim's, and workflow.MergeForwardAskEnded), or at a
// plain finish, a row's settle, which reads nothing after it.
type endHeld struct {
	record.Store
	before func()
	ending pgx.Tx
}

func (s *endHeld) run() {
	if before := s.before; before != nil {
		s.before = nil
		before()
	}
}

func (s *endHeld) FinishLeasedOutbox(ctx context.Context, tx pgx.Tx, id int64, leaseToken string) error {
	if s.before != nil {
		s.ending = tx
	}
	return s.Store.FinishLeasedOutbox(ctx, tx, id, leaseToken)
}

func (s *endHeld) Issue(ctx context.Context, tx pgx.Tx, key string) (*record.Issue, error) {
	if s.ending != nil && tx == s.ending {
		s.ending = nil
		s.run()
	}
	return s.Store.Issue(ctx, tx, key)
}

func (s *endHeld) FinishOutbox(ctx context.Context, tx pgx.Tx, id int64, leaseToken string) error {
	s.run()
	return s.Store.FinishOutbox(ctx, tx, id, leaseToken)
}

// factLockWaitedOn is whether a backend of pool's database waits for an advisory lock now: the
// fact lock is the one such lock the rig's facts and its outbox take.
func factLockWaitedOn(pool *store.Pool) bool {
	var waiting int
	err := pool.QueryRow(context.Background(), `select count(*) from pg_locks
		where locktype = 'advisory' and not granted
		  and database = (select oid from pg_database where datname = current_database())`).Scan(&waiting)
	return err == nil && waiting > 0
}

// The implementer's claim has failed when the ask's row runs, and is still failed at every read
// of the executor's, the last of them under the fact lock that holds the row's end. After those
// reads and before the end commits, the operator's retry relaunches the implementer, and the new
// launch's ready waits for the lock; applied once the end has committed, the ready finds no row of
// the ask queued and sends the ask itself, so the new launch is told it once. A ready that read the
// row before its end landed would leave the ask to a row that never reached the launch.
func TestAReadyThatComesWhileAnAsksRowEndsAtAFailedClaimSendsTheAsk(t *testing.T) {
	t.Parallel()
	runner, pool, publisher, machine := failedWithTheAsk(t, phase.Testing, implementersAsk(), owedAsk)
	held := &endHeld{Store: runner.records}
	runner.records = held
	readied := make(chan error, 1)
	bound := testwait.Bounded(t, 30*time.Second, time.Second)
	held.before = func() {
		go func() { readied <- relaunched(machine) }()
		if !testwait.Holds(bound, 10*time.Millisecond, func() bool { return len(readied) > 0 || factLockWaitedOn(pool) }) {
			t.Errorf("within %s of the relaunch its ready was neither applied nor waiting on the fact lock", bound)
		}
	}
	drainAt(t, runner, 0, "the attempt that ends the row at the failed claim")
	if held.before != nil {
		t.Fatalf("the row's end ran nothing between its reads and its commit: the relaunch came between nothing")
	}
	select {
	case err := <-readied:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(bound):
		t.Fatalf("the relaunch had not come ready %s after the row's end", bound)
	}
	drainAt(t, runner, 10*time.Minute, "the attempt after the new launch's ready")
	askToldOnce(t, pool, publisher, owedAsk, machine.Claim().Generation)
}

// publishedThen is holderPublisher with then run once, inside the first publish the listener takes,
// after it took it and before the publish returns: what the test makes come between a delivery and
// what the executor reads after it.
type publishedThen struct {
	*holderPublisher
	then func()
}

func (p *publishedThen) Publish(ctx context.Context, topic, message string, payload any, key string) (string, error) {
	holder, err := p.holderPublisher.Publish(ctx, topic, message, payload, key)
	if then := p.then; err == nil && then != nil {
		p.then = nil
		then()
	}
	return holder, err
}

// endedAndRetried is an operator's stop of machine's live claim, which retires it, and the
// operator's retry, which relaunches it; with ready, the new launch is brought up through its
// ready too.
func endedAndRetried(machine *supervise.Machine, ready bool) error {
	told := machine.Claim()
	if err := machine.Handle(context.Background(), supervise.RequestStop{Claim: told.Token, By: supervise.SuspendedByOperator}); err != nil {
		return fmt.Errorf("the operator's stop of the %s's launch %d: %w", told.Role, told.Generation, err)
	}
	machine.Wait()
	if !ready {
		if err := machine.Handle(context.Background(), supervise.RequestAskedRetry{Claim: told.Token, By: supervise.SuspendedByOperator}); err != nil {
			return fmt.Errorf("the operator's retry of the %s: %w", told.Role, err)
		}
		machine.Wait()
		return nil
	}
	return relaunched(machine)
}

// toldAtTwoLaunches fails the test unless the implementer was told the ask twice in all, once at
// each of its launches the publish of the ask's first row straddled, the outbox holds no
// merge-forward row of LEGION-208 any more, and the record's ask keeps the later launch.
func toldAtTwoLaunches(t *testing.T, pool *store.Pool, publisher *holderPublisher, first, second uint64) {
	t.Helper()
	_, delivered := publisher.snapshot()
	want := owedAsk.Message("LEGION-208", record.PullRequest{Repo: "acme/widgets"})
	if len(delivered) != 2 || delivered[0].message != want || delivered[1].message != want {
		t.Errorf("the implementer was told %d messages (%+v), want the ask once at its launch %d and once at its launch %d",
			len(delivered), delivered, first, second)
	}
	if rows := outboxRowsOfKind(t, pool, "LEGION-208", record.OutboxKindMergeForward); rows != 0 {
		t.Errorf("the outbox holds %d merge-forward rows of LEGION-208, want none", rows)
	}
	if sent := recordedSentLaunch(t, pool); sent != second {
		t.Errorf("the record's ask was told launch %d, want launch %d", sent, second)
	}
}

// The ask's row reaches the woken implementer's launch 2, and that launch ends - an operator stops
// it - and the operator's retry relaunches the claim into launch 3, all before the publish returns.
// The launch the delivery records is the one read before the publish, launch 2, so launch 3, which
// comes ready after the row's end, is sent the ask and told it once. A launch read after the
// publish would be launch 3, told nothing.
func TestAnAskDeliveredAsItsLaunchEndsIsSentToTheNextLaunch(t *testing.T) {
	t.Parallel()
	runner, pool, publisher, machine := wokenByTheAsk(t)
	comesUp(t, machine, true)
	first := machine.Claim().Generation
	publisher.setAbsent()
	var ended error
	runner.notices = &publishedThen{holderPublisher: publisher, then: func() { ended = endedAndRetried(machine, false) }}
	drainAt(t, runner, 10*time.Minute, "the attempt that delivers the ask to launch 2")
	if ended != nil {
		t.Fatal(ended)
	}
	comesUp(t, machine, true)
	second := machine.Claim().Generation
	drainAt(t, runner, 20*time.Minute, "the attempt after launch 3's ready")
	toldAtTwoLaunches(t, pool, publisher, first, second)
}

// The ask's row reaches the woken implementer's launch 2, and before the publish returns, that
// launch ends, the operator's retry relaunches the claim and launch 3 comes ready, while the row
// is still queued, so launch 3's ready sends nothing. The row's end reads the claim again under
// the fact lock, where it holds launch 3, later than the launch the row reached, so the ask is
// sent to launch 3, which is told it once.
func TestAnAskDeliveredWhileItsRoleIsRelaunchedIsSentToTheNewLaunch(t *testing.T) {
	t.Parallel()
	runner, pool, publisher, machine := wokenByTheAsk(t)
	comesUp(t, machine, true)
	first := machine.Claim().Generation
	publisher.setAbsent()
	var ended error
	runner.notices = &publishedThen{holderPublisher: publisher, then: func() {
		if ended = endedAndRetried(machine, true); ended != nil {
			return
		}
		if rows := outboxRowsOfKind(t, pool, "LEGION-208", record.OutboxKindMergeForward); rows != 1 {
			ended = fmt.Errorf("after the ready of launch %d the outbox holds %d merge-forward rows, want the one row still queued",
				machine.Claim().Generation, rows)
		}
	}}
	drainAt(t, runner, 10*time.Minute, "the attempt that delivers the ask to launch 2")
	if ended != nil {
		t.Fatal(ended)
	}
	second := machine.Claim().Generation
	drainAt(t, runner, 20*time.Minute, "a later attempt")
	toldAtTwoLaunches(t, pool, publisher, first, second)
}

// An operator's stop of the implementer, asked to merge the base forward while the issue stays in
// testing, retires its claim: the stop moves no issue, the merge stays owed to the implementer and
// the tester stays held. The operator's retry relaunches the implementer, and the ready of that
// launch sends the ask once.
func TestAnAskOwedToAStoppedClaimIsSentAgainAtItsRelaunch(t *testing.T) {
	t.Parallel()
	rig := newOwedAskRig(t)
	if recorder := rig.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(rig.implementer)+"/stop", nil); recorder.Code != http.StatusOK {
		t.Fatalf("the operator's stop = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	rig.relaunchSendsTheOwedAskOnce("the operator's stop")
}

// The implementer, asked to merge the base forward while the issue stays in testing, reports its
// own exit, which retires its claim: the merge stays owed to the implementer and the tester stays
// held. The operator's retry relaunches the implementer, and the ready of that launch sends the
// ask once.
func TestAnAskOwedToAClaimThatExitedIsSentAgainAtItsRelaunch(t *testing.T) {
	t.Parallel()
	rig := newOwedAskRig(t)
	rig.must(supervise.RequestExit{Claim: rig.implementer, Generation: rig.machine.Claim().Generation, Session: oldSession,
		Reason: "the agent ended its session"})
	rig.relaunchSendsTheOwedAskOnce("the implementer's exit")
}

// relaunchSendsTheOwedAskOnce is what follows an end of the implementer's claim, named by ended,
// while the ask is owed: the claim is retired and the end queued no ask, the issue is still in
// testing and the tester still held; then the operator's retry relaunches the implementer, the
// ready of its new launch queues the ask once, carrying that launch, and leaves the record's ask
// told the launch before until the row's delivery, and the tester is still held.
func (r *freshSessionRig) relaunchSendsTheOwedAskOnce(ended string) {
	r.t.Helper()
	retired := r.machine.Claim()
	if retired.State != supervise.StateRetired {
		r.t.Fatalf("after %s the implementer is %s, want retired", ended, retired.State)
	}
	if current, from := r.recordedPhase(); current != phase.Testing || from != "" {
		r.t.Fatalf("after %s the issue is in %s held from %q, want testing and not held", ended, current, from)
	}
	if asks := r.queuedMergeForwards(); len(asks) != 0 {
		r.t.Fatalf("after %s the outbox holds the merge-forward messages %+v, want none", ended, asks)
	}
	r.testerHeld("after " + ended)
	told := r.recordedAsk().SentLaunch

	if recorder := r.operator(http.MethodPost, "/legion/v1/operator/claims/"+string(r.implementer)+"/retry", nil); recorder.Code != http.StatusOK &&
		recorder.Code != http.StatusAccepted {
		r.t.Fatalf("the operator's retry of the implementer = %d, want 200 or 202; body %s", recorder.Code, recorder.Body)
	}
	r.machine.Wait()
	if got := r.machine.Claim(); got.State != supervise.StateLaunching || got.Generation <= retired.Generation {
		r.t.Fatalf("after the retry the implementer is %s at launch %d, want it launching past launch %d", got.State, got.Generation, retired.Generation)
	}
	r.register(oldSession, oldSessionFile)
	launch := r.machine.Claim().Generation

	want := []record.MergeForwardMessage{{Role: claim.RoleImplementer, Step: record.MergeForwardAsk, Head: owedAskHead, Base: "main",
		Phase: phase.Testing, PullRequest: 86, Why: record.ConflictCheckNotGreen, SentLaunch: launch}}
	if asks := r.queuedMergeForwards(); !slices.Equal(asks, want) {
		r.t.Errorf("after the ready of the implementer's launch %d the outbox holds the merge-forward messages %+v, want %+v", launch, asks, want)
	}
	if asked := r.recordedAsk(); asked.SentLaunch != told {
		r.t.Errorf("after the ready of the implementer's launch %d the record's ask is %+v, want it still told launch %d: the row's delivery records launch %d",
			launch, asked, told, launch)
	}
	r.testerHeld("after the relaunch")
}

// queuedMergeForwards is every merge-forward message the outbox holds for the rig's issue, oldest
// first, each as the outbox decodes it, less the run and the phase entry it was queued at
// (fencetest.Unfenced): what the row says of the merge, which a case of the fence reads instead.
func (r *freshSessionRig) queuedMergeForwards() []record.MergeForwardMessage {
	r.t.Helper()
	rows, err := r.pool.Query(context.Background(), `select id, kind, issue, payload from outbox where kind = $1 and issue = $2 order by id`,
		string(record.OutboxKindMergeForward), r.issue.Key)
	if err != nil {
		r.t.Fatalf("read the merge-forward rows: %v", err)
	}
	defer rows.Close()
	var messages []record.MergeForwardMessage
	for rows.Next() {
		var row record.OutboxRow
		if err := rows.Scan(&row.ID, &row.Kind, &row.Issue, &row.Payload); err != nil {
			r.t.Fatalf("scan a merge-forward row: %v", err)
		}
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			r.t.Fatalf("decode merge-forward row %d: %v", row.ID, err)
		}
		messages = append(messages, fencetest.Unfenced(payload).(record.MergeForwardMessage))
	}
	if err := rows.Err(); err != nil {
		r.t.Fatalf("read the merge-forward rows: %v", err)
	}
	return messages
}

// recordedAsk is the merge forward the record holds for the rig's issue's pull request.
func (r *freshSessionRig) recordedAsk() record.MergeForward {
	r.t.Helper()
	var pr *record.PullRequest
	if err := store.Transact(context.Background(), r.pool, func(ctx context.Context, tx pgx.Tx) (err error) {
		pr, err = r.records.PullRequest(ctx, tx, r.issue.Key)
		return err
	}); err != nil {
		r.t.Fatalf("read the pull request of %s: %v", r.issue.Key, err)
	}
	if pr == nil || pr.MergeForward == nil {
		r.t.Fatalf("the record holds the pull request %+v of %s, want one holding a merge forward", pr, r.issue.Key)
	}
	return *pr.MergeForward
}

// testerHeld fails the test unless the merge of owedAskHead is still owed where the issue stands,
// at when: the tester's hold is still one the executor delivers (workflow.MergeForwardAt, the rule
// it reads at each attempt of a row), and the record's ask has neither landed nor been withdrawn.
func (r *freshSessionRig) testerHeld(when string) {
	r.t.Helper()
	var left string
	if err := store.Transact(context.Background(), r.pool, func(ctx context.Context, tx pgx.Tx) (err error) {
		_, _, left, err = workflow.MergeForwardAt(ctx, r.records, tx, r.issue.Key, claim.RoleTester, record.MergeForwardHold, owedAskHead)
		return err
	}); err != nil {
		r.t.Fatalf("read the rule of the tester's hold: %v", err)
	}
	if left != "" {
		r.t.Errorf("%s the tester's hold of %s is no longer delivered (%s), want the tester held", when, owedAskHead, left)
	}
	if asked := r.recordedAsk(); asked.Landed != "" || asked.Withdrawn || asked.Asked() != claim.RoleImplementer {
		r.t.Errorf("%s the record's ask is %+v, want the implementer's, neither landed nor withdrawn", when, asked)
	}
}
