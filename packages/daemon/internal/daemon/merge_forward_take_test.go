package daemon

// A merge forward asked of a role holds every take of its issue that would adopt the working copy
// until it lands (supervise's Deps.MergeForwardOwed, workflowRuntime.mergeForwardOwed), so the ask
// is sent again to a relaunched asked role, once for each launch (workflow's askAgainAtLaunch and
// MergeForwardAskEnded), or the hold would wait for ever on a process nobody told the merge. The
// executor ends a row of the ask in a transaction of its own under the fact lock
// (endMergeForwardAsk), because its reads - the claim it ends on, the launch the publish reached -
// are made before the end lands, and the role can be relaunched and its new launch come ready in
// between; the new launch is told the ask once all the same. The last test is rows 45 and 46 of
// plan v154 end to end over real records: the ask owed, the asked role's process relaunched, the
// take held, the ask sent again, the landing, and the value written again.
//
// Where a test names a negative control it is 6ed3e46f98896fb4f010554d9be7d9bd4e93a81f, where no
// ready sends an ask and no landing ends a hold.

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/supervise/supervisetest"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// takeAskHead is the head of the pull request these tests hold an ask of.
const takeAskHead = "sha-1"

// takeAsk is the implementer's ask of takeAskHead on #86 in testing, as the act queues it.
var takeAsk = record.MergeForwardMessage{Role: claim.RoleImplementer, Step: record.MergeForwardAsk, Head: takeAskHead, Base: "main",
	Phase: phase.Testing, PullRequest: 86, Why: record.ConflictCheckNotGreen}

// takeImplementersAsk is the record's ask of takeAskHead, made in testing, as the act writes it.
func takeImplementersAsk() record.MergeForward {
	return record.MergeForward{Head: takeAskHead, Base: "main", Role: claim.RoleImplementer, Phase: phase.Testing,
		Why: record.ConflictCheckNotGreen, AskedAt: time.Now()}
}

// takeWokenByTheAsk is mergeForwardRunner's LEGION-208 in testing with the implementer's ask owed
// and its row queued, the implementer's claim as an idle release left it, and the workflow behind
// the supervisor, so each launch's ready is applied as the daemon applies it
// (workflowRuntime.terminal). The ask's first attempt has run: it woke the implementer, whose new
// launch is coming up, and published nothing, since no session holds the role yet.
func takeWokenByTheAsk(t *testing.T) (*outbox, *store.Pool, *holderPublisher, *supervise.Machine) {
	t.Helper()
	publisher := &holderPublisher{}
	asked := takeImplementersAsk()
	runner, sup, pool, _ := mergeForwardRunner(t, publisher, &asked)
	engine := workflow.New(runner.records, workflow.Config{Project: "legion", Repositories: testRepositories}, quietLogger())
	sup.OnDurableState((&workflowRuntime{pool: pool, records: runner.records, handlers: []intake.Handler{engine}, log: quietLogger()}).terminal)
	token := mustClaimToken(t, "LEGION-208", claim.RoleImplementer)
	publisher.setAbsent(notify.RoleTopicPrefix + string(token))
	machine := releasedClaim(t, sup, claim.RoleImplementer)
	sup.deps.Conns.(*fake.Conns).Register(token, fake.NewConn())
	enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", takeAsk, time.Now().Add(-time.Minute)))
	drainAt(t, runner, 0, "the ask's first attempt")
	machine.Wait()
	if held := machine.Claim(); held.Released() || held.Generation < 2 {
		t.Fatalf("after the ask's first attempt the implementer is %s at launch %d, want it woken into a new launch", held.State, held.Generation)
	}
	return runner, pool, publisher, machine
}

// takeComesUp drives machine's current launch through its shim's hello and its agent's registration,
// and its ready too where ready is set.
func takeComesUp(t *testing.T, machine *supervise.Machine, ready bool) {
	t.Helper()
	if err := takeLaunchComesUp(machine, ready); err != nil {
		t.Fatal(err)
	}
}

// takeLaunchComesUp is takeComesUp's body, which answers what failed rather than failing the test,
// for an act that runs where the test may not stop: inside an executor's run, or on a goroutine of
// its own.
func takeLaunchComesUp(machine *supervise.Machine, ready bool) error {
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

// takeRelaunched is the operator's retry of machine's claim, which failed or retired, and its new
// launch brought up through its ready (takeLaunchComesUp), so the ready is applied as the daemon
// applies it.
func takeRelaunched(machine *supervise.Machine) error {
	ended := machine.Claim()
	if err := machine.Handle(context.Background(), supervise.RequestOperatorRetry{Claim: ended.Token}); err != nil {
		return fmt.Errorf("the operator's retry of the %s: %w", ended.Role, err)
	}
	machine.Wait()
	if got := machine.Claim(); got.State != supervise.StateLaunching || got.Generation <= ended.Generation {
		return fmt.Errorf("after the retry the %s is %s at launch %d, want it launching past launch %d", ended.Role, got.State,
			got.Generation, ended.Generation)
	}
	return takeLaunchComesUp(machine, true)
}

// takeSentLaunch is the launch the record's ask of LEGION-208 was last told, 0 for none.
func takeSentLaunch(t *testing.T, pool *store.Pool) uint64 {
	t.Helper()
	var sent uint64
	if err := pool.QueryRow(context.Background(), `select coalesce((merge_forward->>'sentLaunch')::bigint, 0) from pull_requests where issue = 'LEGION-208'`).
		Scan(&sent); err != nil {
		t.Fatalf("read the record's ask: %v", err)
	}
	return sent
}

// takeAskToldOnce fails the test unless ask's role was told ask once in all, the outbox holds no
// merge-forward row of LEGION-208 any more, and the record's ask keeps launch as the one it was
// told to.
func takeAskToldOnce(t *testing.T, pool *store.Pool, publisher *holderPublisher, ask record.MergeForwardMessage, launch uint64) {
	t.Helper()
	_, delivered := publisher.snapshot()
	if len(delivered) != 1 || delivered[0].message != ask.Message("LEGION-208", record.PullRequest{Repo: "acme/widgets"}) {
		t.Errorf("the %s was told %d messages (%+v), want the ask once, at its launch %d", ask.Role, len(delivered), delivered, launch)
	}
	if rows := outboxRowsOfKind(t, pool, "LEGION-208", record.OutboxKindMergeForward); rows != 0 {
		t.Errorf("the outbox holds %d merge-forward rows of LEGION-208, want none: no second ask", rows)
	}
	if sent := takeSentLaunch(t, pool); sent != launch {
		t.Errorf("the record's ask was sent at launch %d, want launch %d, the one it was told to", sent, launch)
	}
}

// The ask's own row wakes the released implementer, and the woken launch comes ready while the row
// is still queued: the row reaches that launch by its own delivery, so the ready writes no second
// ask and the implementer is told the ask once. The ready of the launch the row was delivered to,
// when it comes after the delivery, sends nothing either: the delivery recorded the launch.
//
// The negative control is the ready sending the ask whatever is queued: the implementer is told
// twice.
func TestTakeAskWhoseRowWakesItsRoleIsToldOnce(t *testing.T) {
	t.Parallel()
	for _, readyFirst := range []bool{true, false} {
		name := "the launch comes ready before the row delivers"
		if !readyFirst {
			name = "the row delivers before the launch comes ready"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, pool, publisher, machine := takeWokenByTheAsk(t)
			takeComesUp(t, machine, readyFirst)
			publisher.setAbsent()
			drainAt(t, runner, 10*time.Minute, "the attempt once the woken session holds the role")
			launch := machine.Claim().Generation
			if !readyFirst {
				if err := machine.Handle(context.Background(), supervise.RequestReady{Claim: machine.Claim().Token, Generation: launch,
					Session: machine.Claim().Session}); err != nil {
					t.Fatalf("the implementer's ready: %v", err)
				}
				machine.Wait()
			}
			drainAt(t, runner, 20*time.Minute, "a later attempt")
			takeAskToldOnce(t, pool, publisher, takeAsk, launch)
		})
	}
}

// takeFailedWithTheAsk is mergeForwardRunnerAt's LEGION-208 standing in at, its pull request #86
// open at takeAskHead with asked as the record's merge forward, the claim of ask's role as a failure
// of its first launch left it, the workflow behind the supervisor as takeWokenByTheAsk has it, and
// ask's row queued. The listener takes every publish.
func takeFailedWithTheAsk(t *testing.T, at phase.Phase, asked record.MergeForward, ask record.MergeForwardMessage) (*outbox, *store.Pool, *holderPublisher, *supervise.Machine) {
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
	enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", ask, time.Now().Add(-time.Minute)))
	return runner, pool, publisher, machine
}

// The implementer, asked to merge the base forward while the issue stays in testing, has failed
// when the ask's row runs: the executor reads the claim failed, and before the row ends the
// operator's retry relaunches the implementer and the new launch comes ready while the row is
// still queued, so that ready sends nothing. The end reads the claim again under the fact lock,
// where it holds a live launch the ask was not told, so the ask is sent to that launch, which is
// told it once. An end decided on the first read would leave the new launch told nothing.
//
// The negative control is the end deciding on the first read (launch 0 at a failed claim): the
// launch the retry brought up is told nothing, and the hold waits for ever.
func TestTakeAskWhoseFailedClaimIsRetriedBeforeItsRowEndsReachesTheNewLaunch(t *testing.T) {
	t.Parallel()
	runner, pool, publisher, machine := takeFailedWithTheAsk(t, phase.Testing, takeImplementersAsk(), takeAsk)
	came := 0
	runner.beforeUndeliveredEnd = func(record.OutboxRow) {
		if came++; came > 1 {
			return
		}
		if err := takeRelaunched(machine); err != nil {
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
	takeAskToldOnce(t, pool, publisher, takeAsk, machine.Claim().Generation)
}

// takeEndHeld is a record.Store that runs before once, inside the transaction that ends an outbox
// row, after every read that end makes and before it commits: at the first read of an issue in the
// transaction that finished the row under its lease, which is the last read of a merge-forward
// ask's end (endMergeForwardAsk, after the claim's, and workflow.MergeForwardAskEnded).
type takeEndHeld struct {
	record.Store
	before func()
	ending pgx.Tx
}

func (s *takeEndHeld) FinishLeasedOutbox(ctx context.Context, tx pgx.Tx, id int64, leaseToken string) error {
	if s.before != nil {
		s.ending = tx
	}
	return s.Store.FinishLeasedOutbox(ctx, tx, id, leaseToken)
}

func (s *takeEndHeld) Issue(ctx context.Context, tx pgx.Tx, key string) (*record.Issue, error) {
	if s.ending != nil && tx == s.ending {
		s.ending = nil
		if before := s.before; before != nil {
			s.before = nil
			before()
		}
	}
	return s.Store.Issue(ctx, tx, key)
}

// takeFactLockWaitedOn is whether a backend of pool's database waits for an advisory lock now: the
// fact lock is the one such lock the rig's facts and its outbox take.
func takeFactLockWaitedOn(pool *store.Pool) bool {
	var waiting int
	err := pool.QueryRow(context.Background(), `select count(*) from pg_locks
		where locktype = 'advisory' and not granted
		  and database = (select oid from pg_database where datname = current_database())`).Scan(&waiting)
	return err == nil && waiting > 0
}

// The implementer's claim has failed when the ask's row runs, and is still failed at every read of
// the executor's, the last of them under the fact lock that holds the row's end. After those reads
// and before the end commits, the operator's retry relaunches the implementer, and the new launch's
// ready waits for the lock; applied once the end has committed, the ready finds no row of the ask
// queued and sends the ask itself, so the new launch is told it once. A ready that read the row
// before its end landed would leave the ask to a row that never reached the launch.
//
// The negative control is the end made with no fact lock (no intake.SerializeFacts): the ready is
// applied beside the end, finds the row queued, and sends nothing, and the end then reads a failed
// claim.
func TestTakeReadyThatComesWhileAnAsksRowEndsAtAFailedClaimSendsTheAsk(t *testing.T) {
	t.Parallel()
	runner, pool, publisher, machine := takeFailedWithTheAsk(t, phase.Testing, takeImplementersAsk(), takeAsk)
	held := &takeEndHeld{Store: runner.records}
	runner.records = held
	readied := make(chan error, 1)
	bound := testwait.Bounded(t, 30*time.Second, time.Second)
	held.before = func() {
		go func() { readied <- takeRelaunched(machine) }()
		if !testwait.Holds(bound, 10*time.Millisecond, func() bool { return len(readied) > 0 || takeFactLockWaitedOn(pool) }) {
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
	takeAskToldOnce(t, pool, publisher, takeAsk, machine.Claim().Generation)
}

// takePublishedThen is holderPublisher with then run once, inside the first publish the listener
// takes, after it took it and before the publish returns: what the test makes come between a
// delivery and what the executor reads after it.
type takePublishedThen struct {
	*holderPublisher
	then func()
}

func (p *takePublishedThen) Publish(ctx context.Context, topic, message string, payload any, key string) error {
	err := p.holderPublisher.Publish(ctx, topic, message, payload, key)
	if then := p.then; err == nil && then != nil {
		p.then = nil
		then()
	}
	return err
}

// The ask's row reaches the woken implementer's launch 2, and before the publish returns, that
// launch ends - an operator stops it - the operator's retry relaunches the claim, and launch 3 comes
// ready, while the row is still queued, so launch 3's ready sends nothing. The row's end records
// the launch read before the publish, launch 2, and reads the claim again under the fact lock,
// where it holds launch 3, later than the launch the row reached, so the ask is sent to launch 3,
// which is told it once: the implementer is told twice in all, once at each launch the publish
// straddled, and the record keeps the later launch. A launch read after the publish would be launch
// 3, and no launch would be told the ask a second time.
//
// The negative control is the launch read after the publish: launch 3 is recorded told and the
// implementer is told once, at a launch that has gone.
func TestTakeAskDeliveredWhileItsRoleIsRelaunchedIsSentToTheNewLaunch(t *testing.T) {
	t.Parallel()
	runner, pool, publisher, machine := takeWokenByTheAsk(t)
	takeComesUp(t, machine, true)
	first := machine.Claim().Generation
	publisher.setAbsent()
	var ended error
	runner.notices = &takePublishedThen{holderPublisher: publisher, then: func() {
		told := machine.Claim()
		if err := machine.Handle(context.Background(), supervise.RequestStop{Claim: told.Token}); err != nil {
			ended = fmt.Errorf("the operator's stop of the %s's launch %d: %w", told.Role, told.Generation, err)
			return
		}
		machine.Wait()
		if ended = takeRelaunched(machine); ended != nil {
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

	_, delivered := publisher.snapshot()
	want := takeAsk.Message("LEGION-208", record.PullRequest{Repo: "acme/widgets"})
	if len(delivered) != 2 || delivered[0].message != want || delivered[1].message != want {
		t.Errorf("the implementer was told %d messages (%+v), want the ask once at its launch %d and once at its launch %d",
			len(delivered), delivered, first, second)
	}
	if rows := outboxRowsOfKind(t, pool, "LEGION-208", record.OutboxKindMergeForward); rows != 0 {
		t.Errorf("the outbox holds %d merge-forward rows, want none", rows)
	}
	if sent := takeSentLaunch(t, pool); sent != second {
		t.Errorf("the record's ask was told launch %d, want launch %d", sent, second)
	}
}

// takeRig is rows 45 and 46 of plan v154 over real records: LEGION-208 in implementing with its
// pull request #86 open at takeAskHead, the implementer's claim of the issue under a supervisor
// wired as the daemon wires it - the phase and merge-forward readers (workflowRuntime.phaseHolds and
// mergeForwardOwed), the App identity and the turn answer, a task bucket - behind the workflow
// whose end of a merge forward's hold reaches the issue's claims through the daemon's own path
// (Config.MergeForwardEnded, workflowRuntime.mergeForwardEnded, supervisor.wakeIssueClaims).
type takeRig struct {
	t           *testing.T
	ctx         context.Context
	pool        *store.Pool
	records     record.Store
	sup         *supervisor
	rt          *fake.Runtime
	engine      *workflow.Engine
	values      *supervisetest.TaskValues
	implementer claim.Token
	machine     *supervise.Machine
}

const takePhaseTask = "Continue Workflow. Issue: LEGION-208. Phase: implementing."

func newTakeRig(t *testing.T) *takeRig {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: "LEGION-208", Title: "Workflow", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"}
	putOutboxIssue(t, pool, records, issue)
	if _, err := pool.Exec(ctx, `insert into slots (issue, index, admitted_at) values ($1, 0, now())`, issue.Key); err != nil {
		t.Fatalf("seed the issue's admission slot: %v", err)
	}
	if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		return records.PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: issue.Key, Repo: "acme/widgets",
			Number: 86, Branch: "legion/" + issue.Key, HeadSHA: takeAskHead, HeadUpdatedAt: time.Now(), Failing: []string{},
			CheckRuns: []record.AttemptRun{}, Base: "main"})
	}); err != nil {
		t.Fatalf("seed the pull request: %v", err)
	}
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.store = openOutboxStore(t, pool)
	values := supervisetest.NewTaskValues()
	sup.deps.TaskValues = values
	adoptingAsTheDaemonDoes(sup)
	w := &workflowRuntime{pool: pool, records: records, log: quietLogger()}
	sup.deps.PhaseHolds, sup.deps.MergeForwardOwed = w.phaseHolds, w.mergeForwardOwed
	engine := workflow.New(records, workflow.Config{Project: "legion", Repositories: testRepositories, MergeForwardEnded: w.mergeForwardEnded}, quietLogger())
	w.handlers = []intake.Handler{engine}
	sup.OnDurableState(w.terminal)
	trees := outboxTreeStore(t, pool, issue.Key)
	w.outboxWake.Store(&outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, project: "legion",
		stateDir: t.TempDir(), tokens: outboxTokens{}, notices: &outboxPublisher{}, handlers: w.handlers, now: time.Now, trees: trees})
	rig := &takeRig{t: t, ctx: ctx, pool: pool, records: records, sup: sup, rt: rt, engine: engine, values: values,
		implementer: mustClaimToken(t, issue.Key, claim.RoleImplementer)}
	machine, _, err := sup.Create(ctx, supervise.Claim{Token: rig.implementer, Project: "legion", Tree: issue.Tree, Issue: issue.Key,
		Role: claim.RoleImplementer, State: supervise.StateQueued}, "")
	if err != nil {
		t.Fatalf("create the implementer: %v", err)
	}
	rig.machine = machine
	sup.deps.Conns.(*fake.Conns).Register(rig.implementer, fake.NewConn())
	if err := machine.Handle(ctx, supervise.RequestSpawn{Claim: rig.implementer}); err != nil {
		t.Fatalf("spawn the implementer: %v", err)
	}
	rig.comesReady()
	return rig
}

// comesReady drives the implementer's launched claim through its shim's hello, its agent's
// registration and its ready, at the claim's current generation, and waits out the decisions its
// ready starts: the ready is a fact the workflow applies (workflowRuntime.terminal).
func (r *takeRig) comesReady() {
	r.t.Helper()
	generation := r.machine.Claim().Generation
	for _, ev := range []supervise.Event{
		supervise.StreamHello{Claim: r.implementer, Generation: generation},
		supervise.RequestRegister{Claim: r.implementer, Generation: generation, Session: "ses-implementer", SessionFile: "/tmp/ses-implementer.jsonl"},
		supervise.RequestReady{Claim: r.implementer, Generation: generation, Session: "ses-implementer"},
	} {
		if err := r.machine.Handle(r.ctx, ev); err != nil {
			r.t.Fatalf("boot the implementer: handle %T: %v", ev, err)
		}
	}
	r.machine.Wait()
}

// take is the implementer's worker taking the task at the value the daemon wrote last, answered as
// the daemon answers it.
func (r *takeRig) take() supervise.TakeAnswer {
	r.t.Helper()
	r.machine.Wait()
	current, err := r.machine.CurrentTask(r.ctx)
	if err != nil || current.Value == nil {
		r.t.Fatalf("the implementer's value = %+v, %v, want one written", current, err)
	}
	held := r.machine.Claim()
	answer, err := r.machine.Take(r.ctx, supervise.RequestTake{Claim: r.implementer, Generation: held.Generation, Session: held.Session,
		Number: current.Value.Number, Revision: supervise.TaskRevision{Epoch: current.Value.Epoch, Revision: current.Value.Revision}})
	if err != nil {
		r.t.Fatalf("the take of task %d: %v", current.Value.Number, err)
	}
	r.machine.Wait()
	return answer
}

// asks is the merge-forward messages the outbox holds for LEGION-208, oldest first.
func (r *takeRig) asks() []record.MergeForwardMessage {
	r.t.Helper()
	rows, err := r.pool.Query(r.ctx, `select id, kind, issue, payload from outbox where kind = $1 and issue = 'LEGION-208' order by id`,
		string(record.OutboxKindMergeForward))
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
		messages = append(messages, payload.(record.MergeForwardMessage))
	}
	if err := rows.Err(); err != nil {
		r.t.Fatalf("read the merge-forward rows: %v", err)
	}
	return messages
}

// asked is the daemon asking the implementer to merge the base forward in implementing, its row
// delivered to launch 1 (the row left the outbox, and the record keeps the launch): the record's
// merge forward owed and not landed.
func (r *takeRig) asked() {
	r.t.Helper()
	if err := store.Transact(r.ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
		pr, err := r.records.PullRequest(ctx, tx, "LEGION-208")
		if err != nil || pr == nil {
			return fmt.Errorf("read the pull request: %+v, %w", pr, err)
		}
		pr.MergeForward = &record.MergeForward{Head: takeAskHead, Base: "main", Role: claim.RoleImplementer, Phase: phase.Implementing,
			Why: record.ConflictCheckNotGreen, AskedAt: time.Now(), SentLaunch: 1}
		return r.records.PutPullRequest(ctx, tx, *pr)
	}); err != nil {
		r.t.Fatalf("record the ask: %v", err)
	}
}

// lands is GitHub's synchronize of #86 from takeAskHead to sha-2, applied through the workflow: the
// merge forward landing.
func (r *takeRig) lands() {
	r.t.Helper()
	result, err := intake.ApplyFact(r.ctx, r.pool, "test", "landing-"+r.t.Name(), intake.PullRequestSynchronized{Repo: "acme/widgets", Number: 86,
		Branch: "legion/LEGION-208", Base: "main", HeadSHA: "sha-2", Before: takeAskHead, Sender: "legion-implementer[bot]"}, r.engine)
	if err != nil || result.Refusal != nil {
		r.t.Fatalf("apply the landing = %+v, %v", result, err)
	}
}

// Rows 45 and 46: the implementer holds its phase task in implementing, and the daemon asks it to
// merge the base forward; the ask is delivered to launch 1. The implementer's process is relaunched
// before the landing, by a death and, in a case of its own, by a provider error that the operator's
// stop and retry carried on. At the new launch's ready the ask is sent again once and that launch
// recorded; a second ready of the same launch sends nothing. The new process's take of its phase
// task is refused in-turn, with no adoption over the merge in progress. The merge lands: the daemon
// posts the issue's claims the end of the hold, the value is written again, and the take that
// follows is accepted once, adopting the merged working copy.
//
// The negative control is 6ed3e46f: the ready sends no ask, the new process's take is accepted at
// once and adopts over the merge (the first want of the refusal fails), and the landing writes
// nothing.
func TestTakeOfARelaunchedAskedRoleIsHeldAskedAgainAndTakenOnceAtTheLanding(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// ends ends the implementer's process with its task pending, and brings the new launch's
		// process to its ready.
		ends func(r *takeRig)
	}{
		{name: "a death", ends: func(r *takeRig) {
			if err := r.machine.Handle(r.ctx, supervise.RequestDeliver{Claim: r.implementer, Task: takePhaseTask, ID: "outbox:7",
				Phase: phase.Implementing, Generation: 1}); err != nil {
				t.Fatalf("deliver the phase task: %v", err)
			}
			r.machine.Wait()
			if answer := r.take(); answer.Outcome != supervise.TakeOutcomeAccepted {
				t.Fatalf("the take before the ask = %+v, want it accepted", answer)
			}
			r.asked()
			if err := r.machine.Handle(r.ctx, supervise.RuntimeObservation{Observation: runtime.Observation{Locator: *r.machine.Claim().Locator,
				Kind: runtime.Gone, At: time.Now()}}); err != nil {
				t.Fatalf("the implementer's death: %v", err)
			}
			r.machine.Wait()
			if got := r.machine.Claim(); got.State != supervise.StateLaunching || got.Generation != 2 {
				t.Fatalf("after the death the implementer is %s at launch %d, want it launching at 2", got.State, got.Generation)
			}
			r.comesReady()
		}},
		{name: "a provider error's retry", ends: func(r *takeRig) {
			if err := r.machine.Handle(r.ctx, supervise.RequestDeliver{Claim: r.implementer, Task: takePhaseTask, ID: "outbox:7",
				Phase: phase.Implementing, Generation: 1}); err != nil {
				t.Fatalf("deliver the phase task: %v", err)
			}
			r.machine.Wait()
			if answer := r.take(); answer.Outcome != supervise.TakeOutcomeAccepted {
				t.Fatalf("the take before the ask = %+v, want it accepted", answer)
			}
			number := r.machine.Claim().Pending.Number
			r.asked()
			for _, ev := range []supervise.Event{
				supervise.StreamTurnStart{Claim: r.implementer},
				supervise.StreamTurnEnd{Claim: r.implementer},
				supervise.RequestTurnOutcome{Claim: r.implementer, Generation: 1, Sequence: turnSequence.Add(1), Task: number,
					ProviderError: &supervise.ProviderError{Message: "overloaded_error", Status: 529}},
				supervise.RequestStop{Claim: r.implementer},
				supervise.RequestOperatorRetry{Claim: r.implementer},
			} {
				if err := r.machine.Handle(r.ctx, ev); err != nil {
					t.Fatalf("handle %T: %v", ev, err)
				}
			}
			r.machine.Wait()
			if got := r.machine.Claim(); got.State != supervise.StateLaunching || got.Generation != 2 {
				t.Fatalf("after the retry the implementer is %s at launch %d, want it launching at 2", got.State, got.Generation)
			}
			r.comesReady()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newTakeRig(t)
			tc.ends(r)

			want := []record.MergeForwardMessage{{Role: claim.RoleImplementer, Step: record.MergeForwardAsk, Head: takeAskHead, Base: "main",
				Phase: phase.Implementing, PullRequest: 86, Why: record.ConflictCheckNotGreen}}
			if got := r.asks(); !slices.Equal(got, want) {
				t.Fatalf("after the ready of launch 2 the outbox holds %+v, want the ask once", got)
			}
			if sent := takeSentLaunch(t, r.pool); sent != 2 {
				t.Fatalf("after the ready of launch 2 the record's ask was told launch %d, want 2", sent)
			}
			result, err := intake.ApplyFact(r.ctx, r.pool, "test", "ready-again", intake.ClaimReady{Issue: "LEGION-208", Role: claim.RoleImplementer, Launch: 2}, r.engine)
			if err != nil || result.Refusal != nil {
				t.Fatalf("apply the second ready of launch 2 = %+v, %v", result, err)
			}
			if got := r.asks(); !slices.Equal(got, want) {
				t.Fatalf("after a second ready of launch 2 the outbox holds %+v, want the one ask as before", got)
			}

			adoptions := len(r.rt.CallsOf("AdoptWorkingCopy"))
			if answer := r.take(); answer.Outcome != supervise.TakeOutcomeRefused || answer.Hold != supervise.HoldInTurn {
				t.Fatalf("the new process's take while the merge is owed = %+v, want it refused %s", answer, supervise.HoldInTurn)
			}
			if got := len(r.rt.CallsOf("AdoptWorkingCopy")); got != adoptions {
				t.Fatalf("%d working copies adopted while the merge was owed, want none past the %d made before it", got-adoptions, adoptions)
			}
			written := valueWritesOf(r.values, r.implementer)

			r.lands()

			testwait.Eventually(t, "the implementer's value to be written again once the merge landed", func() bool {
				return valueWritesOf(r.values, r.implementer) > written
			})
			if answer := r.take(); answer.Outcome != supervise.TakeOutcomeAccepted {
				t.Fatalf("the take after the landing = %+v, want it accepted", answer)
			}
			if got := len(r.rt.CallsOf("AdoptWorkingCopy")); got != adoptions+1 {
				t.Fatalf("%d working copies adopted in all, want %d: the take after the landing adopts the merged working copy once", got, adoptions+1)
			}
		})
	}
}
