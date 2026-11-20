package daemon

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The Dispatch project and the issue whose implementer the cases below drive.
const (
	errorRunProject = "CAPTURE"
	errorRunIssue   = "CAPTURE-7"
)

// After six turns in a row of one worker end in a provider error, the project's controller is
// told once, with the issue, the role and the error. By default an operator launches the
// controller (config's `controller: operator`), so no controller may be registered at the sixth
// turn. The notice is then owed, not dropped: the controller's registration tells it at once, and
// it is told once — neither a later turn of the same run, nor the publish of the notice, nor the
// end of the run tells it again.
func TestAControllerRegisteredAfterTheSixthProviderErrorTurnIsToldOnce(t *testing.T) {
	r := newErrorRunRig(t)
	for range 6 {
		r.errorTurn()
	}
	if notices := r.notices(); len(notices) != 0 {
		t.Fatalf("with no controller registered the outbox holds %d provider-errors notices, want none: %+v", len(notices), notices)
	}

	r.registerController()
	r.wantOneNotice("after six turns with no controller and then a controller's registration,", 6)

	r.errorTurn()
	r.wantOneNotice("after the seventh turn,", 6)
	r.publish()
	r.errorTurn()
	r.errorTurn()
	r.cleanTurn()
	if notices := r.notices(); len(notices) != 0 {
		t.Fatalf("after the notice was published, two more turns of the same run failed and a clean turn ended it, the outbox holds %d provider-errors notices, want none: the controller is told of a run once: %+v",
			len(notices), notices)
	}
}

// A run reaches six with no controller registered, a controller registers, and the worker's next
// turn ends with no error, so the run is over before another turn fails. The run is owed its
// notice whatever comes after it (the gates' case of the first round: a notice offered only at a
// failed turn was never told here).
func TestARunOfSixIsToldWhenAControllerRegistersAndTheNextTurnIsClean(t *testing.T) {
	r := newErrorRunRig(t)
	for range 6 {
		r.errorTurn()
	}
	if notices := r.notices(); len(notices) != 0 {
		t.Fatalf("with no controller registered the outbox holds %d provider-errors notices, want none: %+v", len(notices), notices)
	}
	r.registerController()
	r.cleanTurn()
	r.wantOneNotice("after six turns with no controller, a registration during the run and a clean seventh turn,", 6)
}

// A notice whose write failed is not lost with it. The controller is registered from the start,
// and the transaction that would write the notice at the sixth turn fails, as a database that does
// not answer fails it. The worker's next turn that fails the same way writes it.
func TestAProviderErrorsNoticeWhoseWriteFailedIsWrittenAtTheNextTurn(t *testing.T) {
	r := newErrorRunRig(t)
	r.registerController()
	for range 5 {
		r.errorTurn()
	}
	r.refuseNotices()
	r.errorTurn()
	if notices := r.notices(); len(notices) != 0 {
		t.Fatalf("the outbox holds %d provider-errors notices while their write fails, want none: %+v", len(notices), notices)
	}
	r.takeNotices()

	r.errorTurn()
	r.wantOneNotice("after the sixth turn's notice failed to write and the seventh turn in a row ended in a provider error,", 7)
}

// The same failed write, and then a turn with no error instead of another provider error: the end
// of the run offers the notice the run still owes.
func TestARunOfSixWhoseNoticeWriteFailedIsToldWhenItsNextTurnIsClean(t *testing.T) {
	r := newErrorRunRig(t)
	r.registerController()
	for range 5 {
		r.errorTurn()
	}
	r.refuseNotices()
	r.errorTurn()
	if notices := r.notices(); len(notices) != 0 {
		t.Fatalf("the outbox holds %d provider-errors notices while their write fails, want none: %+v", len(notices), notices)
	}
	r.takeNotices()

	r.cleanTurn()
	r.wantOneNotice("after the sixth turn's notice failed to write and a clean seventh turn,", 6)
}

// The control of the cases above: the controller is registered before the sixth turn, so the
// sixth turn tells it, and the clean turn that ends the run tells it nothing more.
func TestARunToldWhileItLastedIsNotToldAgainWhenItEnds(t *testing.T) {
	r := newErrorRunRig(t)
	for range 5 {
		r.errorTurn()
	}
	r.registerController()
	r.errorTurn()
	r.wantOneNotice("after a controller registered before the sixth turn,", 6)
	r.cleanTurn()
	r.wantOneNotice("after the clean turn that ended the run,", 6)
}

// The count starts again after a clean turn, and the controller can be told of the next run: a
// run of six that follows a clean turn is a run of its own, told once as the first was.
func TestARunOfProviderErrorsAfterACleanTurnIsToldAgain(t *testing.T) {
	r := newErrorRunRig(t)
	r.registerController()
	for range 6 {
		r.errorTurn()
	}
	r.wantOneNotice("after the sixth turn in a row ended in a provider error,", 6)
	r.publish()
	r.errorTurn()
	if notices := r.notices(); len(notices) != 0 {
		t.Fatalf("a seventh turn of the run that was told wrote %d more provider-errors notices, want none: %+v", len(notices), notices)
	}

	r.cleanTurn()
	r.give()
	for turn := range 6 {
		r.errorTurn()
		if turn < 5 {
			if notices := r.notices(); len(notices) != 0 {
				t.Fatalf("turn %d of the new run wrote %d provider-errors notices, want none before the sixth: %+v", turn+1, len(notices), notices)
			}
		}
	}
	r.wantOneNotice("after a clean turn and six more turns in a row that ended in a provider error,", 6)
}

// A controller is registered throughout. The first run is told, and its notice stays unsent; a
// second run reaches six and ends with a clean turn before the first notice is published. Each
// run of six owes the controller its own notice, so the second is written at once beside the
// first rather than behind it (the gates' case of the first round: a pending notice of the claim
// held the second one back, and the run's end then lost it).
func TestASecondRunOfSixIsToldWhileTheFirstRunsNoticeWaitsUnsent(t *testing.T) {
	r := newErrorRunRig(t)
	r.registerController()
	for range 6 {
		r.errorTurn()
	}
	r.wantOneNotice("after the first run reached six,", 6)
	r.cleanTurn()
	r.give()
	for range 6 {
		r.errorTurn()
	}
	r.cleanTurn()
	notices := r.notices()
	if len(notices) != 2 {
		t.Fatalf("after two runs of six with a controller registered, the second ended while the first notice was unsent, the outbox holds %d provider-errors notices, want 2 (one per run): %+v", len(notices), notices)
	}
	for _, notice := range notices {
		if !strings.Contains(notice.reason, "6 turns in a row") {
			t.Errorf("a notice's reason is %q, want each run told at its sixth turn", notice.reason)
		}
	}
}

// Two runs of six end while no controller is registered (the default: an operator launches the
// controller), one after the other, each ended by a clean turn. Then a controller registers.
// Each run reached six, so each owes the controller its notice, and each is told once: the
// second run's owed notice stands beside the first's rather than in its place (the gates' case of
// the second round: the claim kept one owed notice, and the second run's replaced the first's).
// The first run is seven turns long and the second six, so the words of each notice say which
// run it tells, and the notice names them oldest first. Once that notice is published, the next
// admission pass tells neither run again.
func TestEachRunOfSixBeforeAControllerRegistersIsToldOnce(t *testing.T) {
	r := newErrorRunRig(t)
	for range 7 {
		r.errorTurn()
	}
	r.cleanTurn()
	r.give()
	for range 6 {
		r.errorTurn()
	}
	r.cleanTurn()
	if notices := r.notices(); len(notices) != 0 {
		t.Fatalf("with no controller registered the outbox holds %d provider-errors notices, want none: %+v", len(notices), notices)
	}
	r.registerController()
	r.wantEachRunToldOnce("after two runs of six, each ended by a clean turn while no controller was registered, and then a controller's registration,",
		"7 turns in a row", "6 turns in a row")
	r.publish()
	r.admissionPass("boot:after-the-notice")
	if notices := r.notices(); len(notices) != 0 {
		t.Fatalf("after both runs were told and their notice published, the next admission pass wrote %d provider-errors notices, want none: %+v",
			len(notices), notices)
	}
}

// The same two runs, with the second still going when the controller registers: both runs are
// told at the registration, once each, and the second run's next failed turn and the clean turn
// that ends it tell neither again.
func TestEachRunOfSixIsToldOnceWhenTheSecondGoesOnAfterTheRegistration(t *testing.T) {
	r := newErrorRunRig(t)
	for range 7 {
		r.errorTurn()
	}
	r.cleanTurn()
	r.give()
	for range 6 {
		r.errorTurn()
	}
	r.registerController()
	r.wantEachRunToldOnce("after two runs of six with no controller registered, the second still going, and then a controller's registration,",
		"7 turns in a row", "6 turns in a row")
	r.publish()
	r.errorTurn()
	r.cleanTurn()
	if notices := r.notices(); len(notices) != 0 {
		t.Fatalf("after both runs were told, the second run's seventh turn and the clean turn that ended it wrote %d provider-errors notices, want none: %+v",
			len(notices), notices)
	}
}

// A new session puts the run back to zero, and the notice the run of six owes outlives it: the
// controller registered after the reset is told of the run all the same.
func TestARunOfSixEndedByANewSessionIsToldWhenAControllerRegisters(t *testing.T) {
	r := newErrorRunRig(t)
	for range 6 {
		r.errorTurn()
	}
	r.handle(supervise.StreamTurnEnd{Claim: r.token},
		supervise.RequestSuspend{Claim: r.token, Reason: "a fresh session was asked for", By: supervise.SuspendedByOperator},
		supervise.RequestForgetSession{Claim: r.token, Session: "ses-implementer"})
	if got := r.machine.Claim().ProviderErrorRun; got != 0 {
		t.Fatalf("the run of provider errors is %d after the session was forgotten, want 0", got)
	}
	if notices := r.notices(); len(notices) != 0 {
		t.Fatalf("with no controller registered the outbox holds %d provider-errors notices, want none: %+v", len(notices), notices)
	}
	r.registerController()
	r.wantOneNotice("after six turns, a new session and then a controller's registration,", 6)
}

// The notice is owed in the store, not in the daemon's memory: a daemon that restarts reads the
// claim back owing it, and the admission pass of its boot tells a controller registered while it
// was down, which no registration of this daemon's ever saw.
func TestARunOfSixIsToldAtTheBootAfterARestart(t *testing.T) {
	r := newErrorRunRig(t)
	for range 6 {
		r.errorTurn()
	}
	r.settled()
	claims, err := r.st.Claims(r.ctx)
	if err != nil {
		t.Fatalf("read the claims back: %v", err)
	}
	var restored supervise.Claim
	for _, c := range claims {
		if c.Token == r.token {
			restored = c
		}
	}
	if owed := restored.ProviderErrorsOwed; owed.Run != 1 || !strings.Contains(owed.Reason, "6 turns in a row") {
		t.Fatalf("the claim read back from the store owes %+v, want run 1's notice of six turns", owed)
	}

	r.recordRegistration()
	if _, err := intake.ApplyFact(r.ctx, r.runtime.pool, "daemon", "boot:after-restart", intake.DaemonBooted{Boot: 2}, r.runtime.handlers...); err != nil {
		t.Fatalf("apply the boot: %v", err)
	}
	r.wantOneNotice("after six turns, a restart and a boot that found a controller registered,", 6)
}

// errorRunRig is one implementer whose turns end in a provider error, over the parts the daemon
// wires between such a turn and the controller's notice of a run of them: the implementer's
// machine on the daemon's own claim store, whose Deps.ProviderErrorsOwed is the workflow
// runtime's own (providerErrorsOwed, as daemon.go wires it), the registration's own hook
// (controllerRegistered, which daemon.go hands the API), and admission over the record store, on
// a database of the rig's own. The machine's clock is held, so the ladder's next send goes when a
// case fires the wait the error armed.
type errorRunRig struct {
	t       *testing.T
	ctx     context.Context
	pool    *store.Pool
	st      *store.Store
	project string
	clock   *heldClock
	machine *supervise.Machine
	conn    *fake.Conn
	token   claim.Token
	runtime *workflowRuntime
	// tasks counts the tasks the implementer has been given, so each has an id of its own.
	tasks int
	// offered counts the calls of providerErrorsOwed and controllerRegistered, and ended the
	// transactions of the workflow runtime's pool that have ended. Each call writes on a
	// goroutine of its own, so a case reads the outbox only once every call it made has ended
	// (settled): a read before that could miss a notice still on its way, and a check that no
	// second notice was written would then pass for that reason alone.
	offered, ended atomic.Int64
}

func newErrorRunRig(t *testing.T) *errorRunRig {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	project, err := claim.ProjectToken(errorRunProject)
	if err != nil {
		t.Fatal(err)
	}
	records := record.NewStore()
	putOutboxIssue(t, pool, records, record.Issue{Key: errorRunIssue, Repo: "acme/widgets", DefaultBranch: "main",
		GitHubRepo: "acme/widgets", Project: errorRunProject, Tree: errorRunIssue, Title: "Implement it",
		Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "A", HandedOver: true})
	st, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	if _, err := st.OpenTreeLifecycle(ctx, project, errorRunIssue, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("admit the tree of %s: %v", errorRunIssue, err)
	}
	r := &errorRunRig{t: t, ctx: ctx, pool: pool, st: st, project: project, clock: &heldClock{}, conn: fake.NewConn()}

	// The workflow runtime writes on a pool of its own, which counts each transaction it ends.
	poolConfig, err := pgxpool.ParseConfig(pool.Config().ConnString())
	if err != nil {
		t.Fatalf("read the database's address: %v", err)
	}
	poolConfig.AfterRelease = func(*pgx.Conn) bool {
		r.ended.Add(1)
		return true
	}
	written, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatalf("open the workflow runtime's pool: %v", err)
	}
	t.Cleanup(written.Close)
	log := quietLogger()
	engine := workflow.New(records, workflow.Config{Project: errorRunProject, Repositories: testRepositories,
		Steps: config.DesignGateRootIssues.Steps()}, log)
	admission := admit.New(records, engine, 1, errorRunProject, log)
	r.runtime = &workflowRuntime{pool: store.NewPool(written), records: records, admission: admission, engine: engine,
		handlers: []intake.Handler{engine, admission}, dispatchProject: errorRunProject, log: log}

	sup, _ := newOutboxSupervisor(t, project, t.TempDir())
	sup.deps.Store = st
	sup.deps.Clock = r.clock
	sup.deps.ProviderErrorsOwed = func(c supervise.Claim) {
		r.offered.Add(1)
		r.runtime.providerErrorsOwed(c)
	}
	r.token, err = claim.NewToken(project, errorRunIssue, claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	sup.deps.Conns.(*fake.Conns).Register(r.token, r.conn)
	r.machine, _, err = sup.Create(ctx, supervise.Claim{Token: r.token, Project: project, Tree: errorRunIssue,
		Issue: errorRunIssue, Role: claim.RoleImplementer, State: supervise.StateQueued}, "")
	if err != nil {
		t.Fatalf("create the implementer: %v", err)
	}
	r.handle(supervise.RequestSpawn{Claim: r.token})
	generation := r.machine.Claim().Generation
	r.handle(supervise.StreamHello{Claim: r.token, Generation: generation},
		supervise.RequestRegister{Claim: r.token, Generation: generation, Session: "ses-implementer", SessionFile: "/tmp/implementer.jsonl"},
		supervise.RequestReady{Claim: r.token, Generation: generation, Session: "ses-implementer"})
	r.give()
	return r
}

// handle hands the implementer's machine each of events in turn, and waits out any send one starts.
func (r *errorRunRig) handle(events ...supervise.Event) {
	r.t.Helper()
	for _, ev := range events {
		if err := r.machine.Handle(r.ctx, ev); err != nil {
			r.t.Fatalf("handle %T: %v", ev, err)
		}
		r.machine.Wait()
	}
}

// give hands the implementer a task of its phase, and the agent starts its turn on it.
func (r *errorRunRig) give() {
	r.t.Helper()
	r.tasks++
	sent := len(r.conn.Prompts())
	r.handle(supervise.RequestDeliver{Claim: r.token, Task: "Implement the plan.", ID: fmt.Sprintf("outbox:%d", r.tasks),
		Phase: phase.Implementing, Generation: 1})
	if got := len(r.conn.Prompts()); got != sent+1 {
		r.t.Fatalf("the implementer was sent %d prompts for its task, want 1", got-sent)
	}
	r.handle(supervise.StreamTurnStart{Claim: r.token})
}

// errorTurn ends the turn the implementer is working in an overload the provider answered 529, as
// Oh My Pi reports it: the agent's end of turn, then the prompt's result. The ladder's wait for
// the next send is then fired, and the agent starts its turn on the task sent again.
func (r *errorRunRig) errorTurn() {
	r.t.Helper()
	pending := r.machine.Claim().Pending
	if pending == nil {
		r.t.Fatal("the implementer holds no task, so no turn of it can end in a provider error")
	}
	run := r.machine.Claim().ProviderErrorRun
	r.handle(supervise.StreamTurnEnd{Claim: r.token},
		supervise.StreamPromptResult{Claim: r.token, DeliveryID: pending.ID, AgentInvoked: true,
			Error: "overloaded_error", HTTPStatus: 529, Provider: "anthropic", Model: "claude-opus-5"})
	if got := r.machine.Claim().ProviderErrorRun; got != run+1 {
		r.t.Fatalf("the run of provider errors is %d after one more such turn, want %d", got, run+1)
	}
	pending = r.machine.Claim().Pending
	if pending == nil {
		r.t.Fatal("the turn's provider error took the task away, want it kept on the ladder")
	}
	sent := len(r.conn.Prompts())
	wait := ladderWait(pending.ProviderErrorRetries)
	if fired := r.clock.fire(wait); fired != 1 {
		r.t.Fatalf("fired %d timers armed for the ladder's wait of %s, want the one the provider error armed", fired, wait)
	}
	r.machine.Wait()
	if got := len(r.conn.Prompts()); got != sent+1 {
		r.t.Fatalf("the ladder's wait sent %d prompts, want the task once", got-sent)
	}
	r.handle(supervise.StreamTurnStart{Claim: r.token})
}

// cleanTurn ends the turn the implementer is working with no error: the task is done, and the run
// of provider errors goes back to zero.
func (r *errorRunRig) cleanTurn() {
	r.t.Helper()
	pending := r.machine.Claim().Pending
	if pending == nil {
		r.t.Fatal("the implementer holds no task to end a turn on")
	}
	r.handle(supervise.StreamTurnEnd{Claim: r.token},
		supervise.StreamPromptResult{Claim: r.token, DeliveryID: pending.ID, AgentInvoked: true})
	if got := r.machine.Claim().ProviderErrorRun; got != 0 {
		r.t.Fatalf("the run of provider errors is %d after a turn with no error, want 0", got)
	}
}

// ladderWait is the wait before the attempt-th send of a task a provider error took back, as the
// daemon's ladder arms it: 30 s, doubling to at most 10 minutes.
func ladderWait(attempt int) time.Duration {
	return min(30*time.Second<<min(max(attempt-1, 0), 5), 10*time.Minute)
}

// registerController registers a session as the project's controller as the registration route
// does (api's registerController): the registration is recorded, and then the daemon is told of
// it (Options.ControllerRegistered, which daemon.go wires to controllerRegistered).
func (r *errorRunRig) registerController() {
	r.t.Helper()
	r.recordRegistration()
	r.offered.Add(1)
	r.runtime.controllerRegistered()
}

// recordRegistration records a session as the project's controller and tells nothing more: a
// registration this daemon never saw, made while it was down.
func (r *errorRunRig) recordRegistration() {
	r.t.Helper()
	generation, err := r.st.MintController(r.ctx, r.project, []byte("capability"))
	if err != nil {
		r.t.Fatalf("mint the controller capability: %v", err)
	}
	if ok, err := r.st.RegisterController(r.ctx, r.project, generation, "ses-controller", []byte("secret"), time.Now(), 0); err != nil || !ok {
		r.t.Fatalf("register the controller = %t, %v", ok, err)
	}
}

// refuseNotices makes every write of a controller notice fail, as a database that does not answer
// fails it, and takeNotices lets the outbox take them again.
func (r *errorRunRig) refuseNotices() {
	r.t.Helper()
	for _, statement := range []string{
		`create function refuse_the_notice() returns trigger language plpgsql as $$
			begin raise exception 'the notice''s write did not commit'; end $$`,
		`create trigger refuse_the_notice before insert on outbox for each row
			when (new.kind = 'controller_notice') execute function refuse_the_notice()`,
	} {
		if _, err := r.pool.Exec(r.ctx, statement); err != nil {
			r.t.Fatalf("make the notice's write fail: %v", err)
		}
	}
}

func (r *errorRunRig) takeNotices() {
	r.t.Helper()
	r.settled()
	for _, statement := range []string{"drop trigger refuse_the_notice on outbox", "drop function refuse_the_notice()"} {
		if _, err := r.pool.Exec(r.ctx, statement); err != nil {
			r.t.Fatalf("let the outbox take writes again: %v", err)
		}
	}
}

// settled waits until every call of providerErrorsOwed and controllerRegistered has ended its
// transaction, so what it wrote, if anything, can be read.
func (r *errorRunRig) settled() {
	r.t.Helper()
	testwait.Eventually(r.t, "every notice offered to end its transaction", func() bool {
		return r.ended.Load() >= r.offered.Load()
	})
}

// errorRunNotice is one provider-errors notice the outbox holds for the controller.
type errorRunNotice struct {
	issue  string
	role   claim.Role
	reason string
}

// notices are the provider-errors notices the outbox holds for the controller, read once every
// notice offered has ended its transaction.
func (r *errorRunRig) notices() []errorRunNotice {
	r.t.Helper()
	r.settled()
	rows, err := r.pool.Query(r.ctx, "select id, kind, issue, payload from outbox where kind = $1 order by id",
		string(record.OutboxKindControllerNotice))
	if err != nil {
		r.t.Fatalf("read the controller notices: %v", err)
	}
	defer rows.Close()
	var notices []errorRunNotice
	for rows.Next() {
		var row record.OutboxRow
		if err := rows.Scan(&row.ID, &row.Kind, &row.Issue, &row.Payload); err != nil {
			r.t.Fatalf("scan a controller notice: %v", err)
		}
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			r.t.Fatalf("decode controller notice %d: %v", row.ID, err)
		}
		if notice, ok := payload.(record.ControllerNotice); ok && notice.Kind == record.ProviderErrorsNotice {
			notices = append(notices, errorRunNotice{issue: row.Issue, role: notice.Role, reason: notice.Reason})
		}
	}
	if err := rows.Err(); err != nil {
		r.t.Fatalf("read the controller notices: %v", err)
	}
	return notices
}

// publish finishes every controller notice the outbox holds, as its lane does once the publish
// is made (FinishOutbox deletes the row).
func (r *errorRunRig) publish() {
	r.t.Helper()
	r.settled()
	if _, err := r.pool.Exec(r.ctx, "delete from outbox where kind = $1", string(record.OutboxKindControllerNotice)); err != nil {
		r.t.Fatalf("publish the controller notices: %v", err)
	}
}

// wantOneNotice fails the case unless the outbox holds exactly one provider-errors notice: for the
// implementer of the issue, naming the run of turns it was written at, the model and the error.
func (r *errorRunRig) wantOneNotice(when string, run int) {
	r.t.Helper()
	notices := r.notices()
	if len(notices) != 1 {
		r.t.Fatalf("%s the outbox holds %d provider-errors notices for the controller, want one: %+v", when, len(notices), notices)
	}
	notice := notices[0]
	if notice.issue != errorRunIssue || notice.role != claim.RoleImplementer {
		r.t.Errorf("the notice names the %s of %s, want the implementer of %s", notice.role, notice.issue, errorRunIssue)
	}
	for _, want := range []string{fmt.Sprintf("%d turns in a row", run), "claude-opus-5", "ended in a provider error (HTTP 529)"} {
		if !strings.Contains(notice.reason, want) {
			r.t.Errorf("the notice's reason is %q, want it to name %q", notice.reason, want)
		}
	}
}

// wantEachRunToldOnce fails the case unless the outbox's provider-errors notices name the
// implementer of the issue and the error, and the words of each run, runs, stand in them exactly
// once in all, in the order runs gives them, the oldest run first: each run is told, none twice,
// and the notice names the runs oldest first, as admission writes them (admit's toldRunsReason).
func (r *errorRunRig) wantEachRunToldOnce(when string, runs ...string) {
	r.t.Helper()
	notices := r.notices()
	if len(notices) == 0 {
		r.t.Fatalf("%s the outbox holds no provider-errors notice for the controller, want each of %q told once", when, runs)
	}
	var told strings.Builder
	for _, notice := range notices {
		if notice.issue != errorRunIssue || notice.role != claim.RoleImplementer || !strings.Contains(notice.reason, "ended in a provider error (HTTP 529)") {
			r.t.Errorf("%s a notice names the %s of %s and says %q, want the implementer of %s and the error (HTTP 529)",
				when, notice.role, notice.issue, notice.reason, errorRunIssue)
		}
		told.WriteString(notice.reason + "\n")
	}
	last := -1
	for _, run := range runs {
		if n := strings.Count(told.String(), run); n != 1 {
			r.t.Errorf("%s the run of %q is told %d times in the outbox's provider-errors notices, want once: %+v", when, run, n, notices)
			continue
		}
		at := strings.Index(told.String(), run)
		if at < last {
			r.t.Errorf("%s the run of %q is told before a run older than it, want the runs oldest first, %q: %+v", when, run, runs, notices)
		}
		last = at
	}
}

// admissionPass applies a fact, as the daemon's boot applies one, so admission's pass runs over
// what the claim owes (admit's TellOwedProviderErrors in its pass), on the caller's goroutine.
func (r *errorRunRig) admissionPass(key string) {
	r.t.Helper()
	r.settled()
	if _, err := intake.ApplyFact(r.ctx, r.runtime.pool, "daemon", key, intake.DaemonBooted{Boot: 2}, r.runtime.handlers...); err != nil {
		r.t.Fatalf("apply the fact %s: %v", key, err)
	}
}
