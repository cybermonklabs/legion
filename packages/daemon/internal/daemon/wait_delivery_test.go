package daemon

// What reaches a phase worker while its wait stands (dispatch://INFRA-2040 R3, D4 of its plan):
// a start or a send that hands the claim no new work brings the claim back and delivers nothing,
// and new work ends the wait at its send and is delivered. Each case drives the daemon's own
// pieces over a real PostgreSQL, as readyWorkerRig drives them: the outbox's executor, the claim's
// machine with the daemon's answers (phaseHolds, waitHolds), the ready fact its supervisor fires,
// the production fact handlers, and the API routes a worker declares its wait through and an
// operator gives a task through.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testlog"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

const (
	// waitingSession and waitingSecret are the tester's session and the secret its registration is
	// issued, which the wait route authenticates.
	waitingSession = "ses-tester"
	waitingSecret  = "the secret of ses-tester"
	// waitOperatorToken is the bearer the operator's routes compare against.
	waitOperatorToken = "wait-operator-token"
	// heldLine and quietStartLine are the two lines that say a task waits on a standing wait: the
	// machine's, for a task the claim had taken (supervise's waitHolds), and the outbox's, for a
	// start that hands no new work (startWaits).
	heldLine       = "supervise: a task the claim had taken waits: the claim's wait stands"
	quietStartLine = "outbox start delivered nothing: the claim's wait stands"
)

// waitDeliveryRig is a tester at work in its issue's testing phase, as the daemon runs one: its
// claim kept in Postgres, where the workflow reads it, its machine asking the daemon's own answers
// before every send, its ready applied as the workflow fact through the production handlers, the
// outbox's executor running the issue's start rows, and the API a worker and an operator call. The
// whole daemon logs to logs.
type waitDeliveryRig struct {
	t       *testing.T
	ctx     context.Context
	pool    *store.Pool
	st      *store.Store
	records record.Store
	logs    *testlog.Buffer
	log     *slog.Logger
	w       *workflowRuntime
	sup     *supervisor
	runner  *outbox
	api     http.Handler
	issue   record.Issue
	token   claim.Token
	machine *supervise.Machine
	// conns is every connection a process of the tester was given, oldest first: a restarted
	// daemon's process speaks over one of its own.
	conns []*fake.Conn
}

func newWaitDeliveryRig(t *testing.T) *waitDeliveryRig {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	st, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: "LEGION-208", Title: "Workflow", Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U", HandedOver: true}
	putOutboxIssue(t, pool, records, issue)
	if _, err := st.OpenTreeLifecycle(ctx, "legion", issue.Key, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("admit the tree: %v", err)
	}
	holdSlot(t, pool, issue.Key)
	logs := &testlog.Buffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	steps := config.DesignGateRootIssues.Steps()
	// The engine reads the pod tables of the idle release through the store's own view of the
	// project, as the daemon hands it under either runtime (openWorkflow): a wait's time wakes a
	// claim a release took (workflow's waitDue).
	podRecords, err := st.PodRecordsOf(issue.Project)
	if err != nil {
		t.Fatalf("read the project's pod records: %v", err)
	}
	engine := workflow.New(records, workflow.Config{Project: issue.Project, Repositories: testRepositories, Steps: steps,
		Pods: podRecords}, log)
	admission := admit.New(records, engine, 1, issue.Project, log)
	w := &workflowRuntime{pool: pool, records: records, engine: engine, admission: admission,
		handlers: factHandlers(records, issue.Project, engine, admission), gatedSteps: steps,
		dispatchProject: issue.Project, bootID: "test-boot", log: log}
	r := &waitDeliveryRig{t: t, ctx: ctx, pool: pool, st: st, records: records, logs: logs, log: log, w: w, issue: issue,
		token: mustClaimToken(t, issue.Key, claim.RoleTester)}
	r.runner = &outbox{log: log, dispatchProject: issue.Project, pool: pool, records: records, trees: st,
		tokens: outboxTokens{}, notices: &outboxPublisher{}, project: "legion", stateDir: t.TempDir(), handlers: w.handlers,
		gatedSteps: steps, now: func() time.Time { return time.Now().Add(time.Minute) },
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/" + issue.Key}, nil
		},
	}
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	r.supervise(sup)
	return r
}

// supervise wires sup as the daemon wires its supervisor (daemon.go's plan, the workflow's
// claimDurableState), over the rig's store, which the operator's routes read a claim's step from
// (supervisor.Step), gives the tester's next process a connection of its own, and points the
// outbox and the API at sup.
func (r *waitDeliveryRig) supervise(sup *supervisor) {
	sup.store, sup.deps.Store, sup.deps.Log = r.st, r.st, r.log
	sup.deps.PhaseHolds, sup.deps.WaitHolds = r.w.phaseHolds, r.w.waitHolds
	sup.OnDurableState(r.w.terminal)
	conn := fake.NewConn()
	sup.deps.Conns.(*fake.Conns).Register(r.token, conn)
	r.conns = append(r.conns, conn)
	r.sup, r.runner.supervisor = sup, sup
	r.api = api.NewServer("127.0.0.1", 0, api.Options{Steps: r.w.gatedSteps, Supervisor: sup, Project: "legion",
		DispatchProject: r.issue.Project, OperatorToken: waitOperatorToken, Pool: r.pool, Record: r.records,
		Handlers: r.w.handlers, Log: r.log}).Handler
	if machine, found := sup.Machine(r.token); found {
		r.machine = machine
	}
}

// restart is the daemon stopping and a new one starting over the same Postgres: new machines
// restored from the stored claims, a new runtime, and nothing of the first daemon's memory.
func (r *waitDeliveryRig) restart() {
	r.t.Helper()
	r.sup.stop()
	sup := newSupervisor(r.ctx, r.st, "legion", r.t.TempDir(), quietLogger())
	sup.deps = r.sup.deps
	sup.deps.Runtime, sup.deps.Conns = fake.NewRuntime(), fake.NewConns()
	r.t.Cleanup(sup.stop)
	stored, err := sup.Claims(r.ctx)
	if err != nil {
		r.t.Fatalf("read the stored claims: %v", err)
	}
	if _, err := sup.restore(r.ctx, stored); err != nil {
		r.t.Fatalf("restore the stored claims: %v", err)
	}
	r.supervise(sup)
}

// enter writes the tester's phases row on entry with task as that entry's work, as a transition
// that hands the tester an entry writes it (workflow's enter), and queues the start row that
// carries the task at that entry, as the transition's start does (workflow's started).
func (r *waitDeliveryRig) enter(entry int64, task string) {
	r.t.Helper()
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		if err := r.records.PutPhase(r.ctx, tx, record.PhaseRow{Issue: r.issue.Key, Role: claim.RoleTester, Claim: r.token,
			Entry: entry, EntryTask: task}); err != nil {
			return err
		}
		return r.records.Enqueue(r.ctx, tx, mustOutboxRow(r.t, r.issue.Key, record.SuperviseRequest{Op: "start", Tree: r.issue.Tree,
			Role: claim.RoleTester, Generation: r.issue.Generation, Phase: phase.Testing, Task: task, Entry: entry}, time.Now()))
	}); err != nil {
		r.t.Fatalf("hand the tester entry %d: %v", entry, err)
	}
}

// runStarts runs every start row the issue holds, oldest first, through the outbox's executor, and
// finishes each as the lane does (record.Store.FinishOutbox deletes it), so what the outbox holds
// afterwards is what the daemon still owes. It answers the rows it ran.
func (r *waitDeliveryRig) runStarts() []entryStartRow {
	r.t.Helper()
	starts := entryStartRows(r.t, r.pool, r.issue.Key)
	for _, start := range starts {
		if err := r.runner.execute(r.ctx, start.row); err != nil {
			r.t.Fatalf("run start row %d: %v", start.row.ID, err)
		}
		if _, err := r.pool.Exec(r.ctx, "delete from outbox where id = $1", start.row.ID); err != nil {
			r.t.Fatalf("finish start row %d: %v", start.row.ID, err)
		}
	}
	machine, found := r.sup.Machine(r.token)
	if !found {
		r.t.Fatal("the start rows left no claim for the tester")
	}
	r.machine = machine
	machine.Wait()
	return starts
}

// handle drives one event into the tester's machine and waits for what it set going.
func (r *waitDeliveryRig) handle(ev supervise.Event) {
	r.t.Helper()
	if err := r.machine.Handle(r.ctx, ev); err != nil {
		r.t.Fatalf("handle %T: %v", ev, err)
	}
	r.machine.Wait()
}

// boot drives the launched process through its shim's hello, its agent's registration on the
// tester's session, issued the secret the wait route authenticates, and its ready.
func (r *waitDeliveryRig) boot() {
	r.t.Helper()
	generation := r.machine.Claim().Generation
	secret := sha256.Sum256([]byte(waitingSecret))
	r.handle(supervise.StreamHello{Claim: r.token, Generation: generation})
	r.handle(supervise.RequestRegister{Claim: r.token, Generation: generation, Session: waitingSession,
		SessionFile: "/tmp/tester.jsonl", CapabilityHash: secret[:]})
	r.handle(supervise.RequestReady{Claim: r.token, Generation: generation, Session: waitingSession})
}

// working starts the tester on entry's task: the start row, the launch's boot, the send, and the
// turn that takes the task.
func (r *waitDeliveryRig) working(entry int64, task string) supervise.Delivery {
	r.t.Helper()
	r.enter(entry, task)
	r.runStarts()
	r.boot()
	return r.takeTask()
}

// takeTask starts the turn of the task the tester was sent.
func (r *waitDeliveryRig) takeTask() supervise.Delivery {
	r.t.Helper()
	pending := r.machine.Claim().Pending
	if pending == nil || pending.DeliveredAt.IsZero() {
		r.t.Fatalf("the tester holds %+v, want a task it was sent", pending)
	}
	r.handle(supervise.StreamTurnStart{Claim: r.token, DeliveryID: pending.ID})
	taken := r.machine.Claim()
	if taken.State != supervise.StateWorking || taken.Pending == nil || taken.Pending.ConfirmedAt.IsZero() {
		r.t.Fatalf("the tester after its turn started = %+v, want it working its confirmed task", taken)
	}
	return *taken.Pending
}

// endTurn ends the turn that took delivery, with that prompt's result behind it, as Oh My Pi
// writes them: the delivery's life ends at its result.
func (r *waitDeliveryRig) endTurn(delivery string) {
	r.t.Helper()
	r.handle(supervise.StreamTurnEnd{Claim: r.token})
	r.handle(supervise.StreamPromptResult{Claim: r.token, DeliveryID: delivery, AgentInvoked: true})
}

// request is one call of the daemon's API, with bearer as its Authorization when it names one.
func (r *waitDeliveryRig) request(target string, body any, bearer string) *httptest.ResponseRecorder {
	r.t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		r.t.Fatalf("encode the body of %s: %v", target, err)
	}
	request := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(encoded))
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	recorder := httptest.NewRecorder()
	r.api.ServeHTTP(recorder, request)
	return recorder
}

// declare is the tester's declaration of a wait through the daemon's route, made in its turn, and
// answers the wait the record holds: on the review of the head, its time two hours ahead.
func (r *waitDeliveryRig) declare() record.ClaimWait {
	r.t.Helper()
	return r.declareUntil("the review of the head", time.Now().Add(2*time.Hour))
}

// declareUntil is declare of a wait on waitingOn whose time is wakeAt.
func (r *waitDeliveryRig) declareUntil(waitingOn string, wakeAt time.Time) record.ClaimWait {
	r.t.Helper()
	recorder := r.request("/legion/v1/claims/wait", api.WaitRequest{ClaimToken: r.token, SessionID: waitingSession,
		Secret: waitingSecret, WaitingOn: waitingOn, WakeAt: wakeAt.UTC().Format(time.RFC3339)}, "")
	if recorder.Code != http.StatusOK {
		r.t.Fatalf("declare the wait = %d %s, want 200", recorder.Code, recorder.Body)
	}
	var answered api.WaitResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &answered); err != nil {
		r.t.Fatalf("decode the wait's answer: %v", err)
	}
	wait := r.wantStanding()
	if wait.ID != answered.Wait.ID {
		r.t.Fatalf("the standing wait is %s, want the one declared, %s", wait.ID, answered.Wait.ID)
	}
	return wait
}

// standing is the tester's wait and whether it stands, read from the record as every reader of a
// wait reads it (workflow.WaitStandsNow).
func (r *waitDeliveryRig) standing() (record.ClaimWait, bool) {
	r.t.Helper()
	var wait record.ClaimWait
	var stands bool
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		wait, stands, err = workflow.WaitStandsNow(r.ctx, tx, r.records, r.w.gatedSteps, r.token)
		return err
	}); err != nil {
		r.t.Fatalf("read the tester's wait: %v", err)
	}
	return wait, stands
}

// wantStanding asserts the tester's wait stands, and answers it.
func (r *waitDeliveryRig) wantStanding() record.ClaimWait {
	r.t.Helper()
	wait, stands := r.standing()
	if !stands {
		r.t.Fatal("the tester's wait does not stand, want it standing")
	}
	return wait
}

// wantEnded asserts wait ended with cause, ended by endedBy, and that its end was logged.
func (r *waitDeliveryRig) wantEnded(wait record.ClaimWait, cause, endedBy string) {
	r.t.Helper()
	var endedAt *time.Time
	var endCause, by string
	if err := r.pool.QueryRow(r.ctx, `select ended_at, coalesce(end_cause, ''), coalesce(ended_by, '')
		from claim_waits where id = $1`, wait.ID).Scan(&endedAt, &endCause, &by); err != nil {
		r.t.Fatalf("read wait %s: %v", wait.ID, err)
	}
	if endedAt == nil || endCause != cause || by != endedBy {
		r.t.Fatalf("wait %s ended at %v with cause %q by %q, want an end with cause %q by %q", wait.ID, endedAt, endCause, by, cause, endedBy)
	}
	if lines := r.lines(`msg="wait: ended"`, "wait="+wait.ID); len(lines) != 1 {
		r.t.Fatalf("the end of wait %s was logged %d times, want once; the log is:\n%s", wait.ID, len(lines), r.logs)
	}
	if _, stands := r.standing(); stands {
		r.t.Fatalf("wait %s still stands after its end", wait.ID)
	}
}

// lines is every log line holding each of fragments.
func (r *waitDeliveryRig) lines(fragments ...string) []string {
	var found []string
	for line := range strings.SplitSeq(r.logs.String(), "\n") {
		holds := line != ""
		for _, fragment := range fragments {
			holds = holds && strings.Contains(line, fragment)
		}
		if holds {
			found = append(found, line)
		}
	}
	return found
}

// quietStarts is every line saying a start of the tester delivered nothing because wait stands.
func (r *waitDeliveryRig) quietStarts(wait record.ClaimWait) []string {
	return r.lines(quietStartLine, "claim="+string(r.token), "issue="+r.issue.Key, "role=tester", "wait="+wait.ID)
}

// prompts is every prompt a process of the tester was sent, over every connection it was given.
func (r *waitDeliveryRig) prompts() []fake.Prompt {
	var sent []fake.Prompt
	for _, conn := range r.conns {
		sent = append(sent, conn.Prompts()...)
	}
	return sent
}

// wantPrompts asserts the tester's processes were sent n prompts, and answers them.
func (r *waitDeliveryRig) wantPrompts(n int, why string) []fake.Prompt {
	r.t.Helper()
	sent := r.prompts()
	if len(sent) != n {
		r.t.Fatalf("the tester was sent %d prompts (%+v), want %d: %s", len(sent), sent, n, why)
	}
	return sent
}

// admissionPasses runs admission's passes over the project as its facts run them, several times:
// the entry sweep's (convergeEntries), which sends a role behind its entry that entry's work, and
// the pass every other fact runs, which promotes and repairs the same way (promote).
func (r *waitDeliveryRig) admissionPasses() {
	r.t.Helper()
	for pass := range 3 {
		for _, fact := range []intake.Fact{intake.EntrySweep{}, intake.ControllerTick{}} {
			id := fmt.Sprintf("wait-pass-%d-%T", pass, fact)
			if _, err := intake.ApplyFact(r.ctx, r.pool, "test", id, fact, r.w.handlers...); err != nil {
				r.t.Fatalf("apply %s: %v", id, err)
			}
		}
	}
}

// A start whose task hands the claim no new work brings the claim back and delivers nothing while
// its wait stands, and what brings the claim back leaves the wait standing. The tester takes its
// task, declares a wait in that turn and ends the turn; then each way a claim comes back brings it
// back, and its ready queues the phase's resume task at entry 0 (workflow's readyWorkerTask, the
// catch-up), which the outbox runs and delivers nothing of, logging the wait it found.
//
//   - a relaunch of the same session, after its process died between turns;
//   - an update round's relaunch: the round opened over the pod, the pod replaced, the role back
//     on its own session through the displaced path, and the round ended;
//   - the resume of a cancelled idle release: the claim suspended as released, and the start row
//     the cancel writes for it (releaseResume), which carries no task;
//   - a daemon restart: the claim suspended by the daemon, a new daemon restored from the store,
//     and the boot's resume of that claim by rule, whose resume task delivers nothing either.
//
// Each leaves the wait standing, no task was prompted after the declaration, and the admission
// passes that follow write the waiting claim no new row.
func TestAStartThatHandsAWaitingClaimNoNewWorkBringsItBackAndDeliversNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		// quiet is how many start rows of the come-back carry a task the wait holds back.
		quiet    int
		comeBack func(*waitDeliveryRig)
	}{
		{name: "a relaunch of the same session", quiet: 1, comeBack: func(r *waitDeliveryRig) {
			r.handle(supervise.RuntimeObservation{Observation: runtime.Observation{Locator: *r.machine.Claim().Locator,
				Kind: runtime.Gone, At: time.Now()}})
			r.boot()
		}},
		{name: "an update round's relaunch", quiet: 1, comeBack: func(r *waitDeliveryRig) {
			const round = "pod:wait-delivery-round"
			r.handle(supervise.PodUpdateOpen{Claim: r.token, Round: round, Reasons: "the image moved"})
			if got := r.machine.Claim().UpdateRound; got != round {
				r.t.Fatalf("the tester's round = %q, want %q", got, round)
			}
			r.handle(supervise.RuntimeObservation{Observation: runtime.Observation{Locator: *r.machine.Claim().Locator,
				Kind: runtime.Gone, Displaced: true, At: time.Now()}})
			r.handle(supervise.PodUpdateEnded{Claim: r.token, Round: round})
			r.boot()
		}},
		{name: "the resume of a cancelled idle release", quiet: 1, comeBack: func(r *waitDeliveryRig) {
			r.handle(supervise.RequestSuspend{Claim: r.token, Reason: "the idle release stopped the role",
				By: supervise.SuspendedByReleased})
			resume, err := r.runner.releaseResume(r.issue.Key, r.issue.Tree)(claim.RoleTester, r.issue.Generation)
			if err != nil {
				r.t.Fatalf("build the cancel's resume row: %v", err)
			}
			if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
				return r.records.Enqueue(r.ctx, tx, record.OutboxRow{Issue: r.issue.Key, Kind: record.OutboxKind(resume.Kind),
					Payload: json.RawMessage(resume.Payload), NextAt: time.Now()})
			}); err != nil {
				r.t.Fatalf("queue the cancel's resume row: %v", err)
			}
			if ran := r.runStarts(); len(ran) != 1 || ran[0].request.Task != "" {
				r.t.Fatalf("the cancel queued %+v, want the one start that carries no task", ran)
			}
			r.boot()
		}},
		{name: "a daemon restart", quiet: 2, comeBack: func(r *waitDeliveryRig) {
			r.handle(supervise.RequestSuspend{Claim: r.token, Reason: "the daemon stopped the role", By: supervise.SuspendedByDaemon})
			r.restart()
			if err := r.w.booted(r.ctx, 2); err != nil {
				r.t.Fatalf("apply the boot: %v", err)
			}
			if ran := r.runStarts(); len(ran) != 1 || !ran[0].request.ResumeTask || !ran[0].request.RuleResume || ran[0].request.Entry != 0 {
				r.t.Fatalf("the boot queued %+v, want its one resume of the suspended claim by rule, at entry 0", ran)
			}
			r.boot()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newWaitDeliveryRig(t)
			taken := r.working(1, "Test it.")
			wait := r.declare()
			r.endTurn(taken.ID)
			r.wantPrompts(1, "the task the turn took is the one prompt before the wait")

			tc.comeBack(r)
			catchUp := r.runStarts()
			if len(catchUp) != 1 || !catchUp[0].request.ResumeTask || catchUp[0].request.Entry != 0 ||
				catchUp[0].request.Task != workflow.ResumePhaseTask(r.issue) {
				t.Fatalf("the ready queued %+v, want the catch-up's one start: the phase's resume task at entry 0", catchUp)
			}

			if got := r.machine.Claim(); got.State != supervise.StateReady && got.State != supervise.StateIdle || got.Pending != nil {
				t.Fatalf("the tester after the come-back = %s holding %+v, want it back, ready, holding nothing", got.State, got.Pending)
			}
			r.wantPrompts(1, "no start that hands no new work prompts a waiting tester")
			if standing := r.wantStanding(); standing.ID != wait.ID {
				t.Fatalf("the standing wait is %s after the come-back, want the one declared, %s", standing.ID, wait.ID)
			}
			if lines := r.quietStarts(wait); len(lines) != tc.quiet {
				t.Fatalf("%d starts were logged as delivering nothing for wait %s, want %d; the log is:\n%s", len(lines), wait.ID, tc.quiet, r.logs)
			}

			r.admissionPasses()
			if starts := entryStartRows(t, r.pool, r.issue.Key); len(starts) != 0 {
				t.Fatalf("the admission passes wrote the waiting tester %+v, want no row", starts)
			}
			r.wantStanding()
		})
	}
}

// A silence repair's row written before the declaration delivers nothing once the wait stands. The
// tester works its task and goes quiet past the silence bound, and the repair queues the phase's
// resume task at entry 0 (workflow's repairSilence). News then reaches the tester's session, and
// in the turn it starts the tester declares its wait. The repair's row, run after that turn,
// delivers nothing, and the wait stands.
func TestASilenceRepairWrittenBeforeTheWaitDeliversNothingOnceItStands(t *testing.T) {
	r := newWaitDeliveryRig(t)
	taken := r.working(1, "Test it.")
	r.endTurn(taken.ID)
	var last time.Time
	if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
		claims, err := r.records.RoleClaims(r.ctx, tx, []claim.Token{r.token})
		if err != nil {
			return err
		}
		last = claims[r.token].Step.LastStep()
		return nil
	}); err != nil {
		t.Fatalf("read the tester's last step: %v", err)
	}
	if _, err := intake.ApplyFact(r.ctx, r.pool, "supervise", "worker-silent-wait", intake.WorkerSilent{Issue: r.issue.Key,
		Role: claim.RoleTester, Generation: r.issue.Generation, LastStep: last, Bound: time.Nanosecond}, r.w.handlers...); err != nil {
		t.Fatalf("apply the tester's silence: %v", err)
	}
	repair := entryStartRows(t, r.pool, r.issue.Key)
	if len(repair) != 1 || !repair[0].request.ResumeTask || repair[0].request.Entry != 0 {
		t.Fatalf("the silence repair queued %+v, want the phase's resume task at entry 0", repair)
	}

	r.handle(supervise.StreamTurnStart{Claim: r.token})
	wait := r.declare()
	r.handle(supervise.StreamTurnEnd{Claim: r.token})

	if ran := r.runStarts(); len(ran) != 1 || ran[0].row.ID != repair[0].row.ID {
		t.Fatalf("ran %+v, want the repair's row", ran)
	}
	if got := r.machine.Claim(); got.Pending != nil {
		t.Fatalf("the tester holds %+v after the repair's row, want nothing: the wait stands", got.Pending)
	}
	r.wantPrompts(1, "the repair's resume task is no new work")
	if lines := r.quietStarts(wait); len(lines) != 1 {
		t.Fatalf("the repair's row was logged %d times as delivering nothing, want once; the log is:\n%s", len(lines), r.logs)
	}
	r.wantStanding()
}

// The task a death in the declaring turn took back is held while the wait stands, and sent once it
// ends. The tester declares its wait in the first turn of its task of entry 1, so the wait records
// that entry as taken; its process dies in that turn, and the death takes the task back
// unconfirmed and marked interrupted. The relaunched agent's ready, which sends a claim its held
// task, and every sweep of its live process after it send nothing while the wait stands: the
// machine logs once that the task waits, and its ready queues no other task, since the claim holds
// one. News then reaches the session and its report ends the wait (the wait-news route), and the
// next sweep sends the held task, behind the sentence that its turn was interrupted: the event
// that sends a held task once its wait has ended is the next send the claim makes.
func TestAWaitHoldsTheTaskADeathInItsDeclaringTurnTookBack(t *testing.T) {
	r := newWaitDeliveryRig(t)
	taken := r.working(1, "Test it.")
	wait := r.declare()
	if wait.TakenEntry != 1 {
		t.Fatalf("the wait records entry %d as taken, want 1: the task of entry 1 is the one its turn took", wait.TakenEntry)
	}

	r.handle(supervise.RuntimeObservation{Observation: runtime.Observation{Locator: *r.machine.Claim().Locator,
		Kind: runtime.Gone, At: time.Now()}})
	r.boot()
	held := r.machine.Claim().Pending
	if held == nil || !held.Interrupted || !held.ConfirmedAt.IsZero() || held.Entry != 1 {
		t.Fatalf("the relaunched tester holds %+v, want the task of entry 1 taken back unconfirmed and interrupted", held)
	}
	for range 2 {
		r.handle(supervise.RuntimeObservation{Observation: runtime.Observation{Locator: *r.machine.Claim().Locator,
			Kind: runtime.Alive, At: time.Now()}})
	}
	r.wantPrompts(1, "the held task goes to no process while the wait stands")
	if starts := r.runStarts(); len(starts) != 0 {
		t.Fatalf("the relaunched tester's ready queued %+v, want nothing: it holds its task", starts)
	}
	if lines := r.lines(heldLine, "claim="+string(r.token), "issue="+r.issue.Key, "role=tester", "delivery="+held.ID,
		"wait="+wait.ID); len(lines) != 1 {
		t.Fatalf("the held task was logged %d times, want once however often a send asked; the log is:\n%s", len(lines), r.logs)
	}
	r.wantStanding()

	recorder := r.request("/legion/v1/claims/wait-news", api.WaitNewsRequest{ClaimToken: r.token, SessionID: waitingSession,
		Secret: waitingSecret, WaitID: wait.ID}, "")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"ended":true`) {
		t.Fatalf("the news report = %d %s, want 200 ending the wait", recorder.Code, recorder.Body)
	}
	r.handle(supervise.RuntimeObservation{Observation: runtime.Observation{Locator: *r.machine.Claim().Locator,
		Kind: runtime.Alive, At: time.Now()}})
	resent := r.wantPrompts(2, "the held task goes once the wait ended")[1]
	if resent.DeliveryID != held.ID || !strings.HasSuffix(resent.Message, "Test it.") || resent.Message == "Test it." {
		t.Fatalf("once the wait ended the tester was sent %+v, want the held task %s behind the note that its turn was interrupted", resent, held.ID)
	}
	if taken.ID == held.ID {
		t.Fatalf("the held task kept the id %s its dead turn ran under", taken.ID)
	}
}

// New work ends the wait at its send, and its task is delivered (R3's third end): a task the claim
// had not taken when it declared. The end names the delivery, both in its cause and as what ended
// it, and is logged.
//
//   - The owed entry of a claim behind its entry: the promotion raises the tester's entry to 2
//     while its turn works entry 1, the outbox holds the promotion's resume task back from a claim
//     that holds its phase's task, and the tester declares its wait in that turn, having taken
//     entry 1. Once the turn is over, the admission pass sends the work of entry 2.
//   - A task queued during the declaring turn and not yet taken: entry 2's task reaches the
//     tester while its turn works entry 1, and the tester declares its wait in that turn. Entry 2
//     is the entry the claim was given, not the one it had taken (the check's negative control
//     records the given entry in its place, and holds this task).
//   - An operator's task, which reaches the machine through the operator's route with no outbox
//     row, given to the waiting tester between turns.
func TestNewWorkEndsTheWaitAtItsSendAndIsDelivered(t *testing.T) {
	for _, tc := range []struct {
		name string
		// declared brings the tester from its turn on entry 1's task, taken, to the send of the new
		// work, with the wait declared in that turn; it answers the new work's task.
		declared func(r *waitDeliveryRig, taken supervise.Delivery) (record.ClaimWait, string)
	}{
		{name: "the owed entry of a claim behind its entry", declared: func(r *waitDeliveryRig, taken supervise.Delivery) (record.ClaimWait, string) {
			issue := readIssue(r.t, r.pool, r.issue.Key)
			if err := pgx.BeginFunc(r.ctx, r.pool, func(tx pgx.Tx) error {
				return r.w.engine.StartPromotedWorker(r.ctx, tx, *issue, claim.RoleTester)
			}); err != nil {
				r.t.Fatalf("raise the tester's second entry: %v", err)
			}
			if promoted := r.runStarts(); len(promoted) != 1 || promoted[0].request.Entry != 2 {
				r.t.Fatalf("the promotion queued %+v, want its start of entry 2", promoted)
			}
			if got := r.machine.Claim(); got.GivenEntry != 1 {
				r.t.Fatalf("the tester stands given entry %d, want 1: behind the entry 2 its row names", got.GivenEntry)
			}
			wait := r.declare()
			r.endTurn(taken.ID)
			r.wantPrompts(1, "the promotion's resume task is held back from a claim holding its phase's task")
			r.w.applyEntrySweep(store.ForConcurrentWorker(r.ctx), time.Now())
			return wait, rolePhaseRow(r.t, r.pool, r.issue.Key, claim.RoleTester).EntryTask
		}},
		{name: "a task queued during the declaring turn and not yet taken", declared: func(r *waitDeliveryRig, taken supervise.Delivery) (record.ClaimWait, string) {
			const queued = "Test the new head."
			r.enter(2, queued)
			r.runStarts()
			if got := r.machine.Claim(); got.Pending == nil || got.Pending.Task != queued || !got.Pending.ConfirmedAt.IsZero() || got.GivenEntry != 2 {
				r.t.Fatalf("the tester in its turn holds %+v given entry %d, want entry 2's task queued and not taken", got.Pending, got.GivenEntry)
			}
			wait := r.declare()
			r.endTurn(taken.ID)
			return wait, queued
		}},
		{name: "an operator's task", declared: func(r *waitDeliveryRig, taken supervise.Delivery) (record.ClaimWait, string) {
			const operator = "Say what the tests found."
			wait := r.declare()
			r.endTurn(taken.ID)
			r.wantPrompts(1, "nothing is sent between the turn's end and the operator's task")
			recorder := r.request("/legion/v1/operator/claims/"+string(r.token)+"/deliver", api.DeliverRequest{Task: operator}, waitOperatorToken)
			if recorder.Code != http.StatusOK {
				r.t.Fatalf("the operator's deliver = %d %s, want 200", recorder.Code, recorder.Body)
			}
			r.machine.Wait()
			return wait, operator
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newWaitDeliveryRig(t)
			taken := r.working(1, "Test it.")
			wait, task := tc.declared(r, taken)
			r.runStarts()

			if lines := r.lines(heldLine); len(lines) != 0 {
				t.Fatalf("the new work was held as a task the claim had taken: %q", lines)
			}
			sent := r.wantPrompts(2, "the new work is delivered")[1]
			delivered := r.machine.Claim().Pending
			if delivered == nil || sent.DeliveryID != delivered.ID || !strings.HasSuffix(sent.Message, task) {
				t.Fatalf("the tester was sent %+v holding %+v, want the new work's task %q", sent, delivered, task)
			}
			r.wantEnded(wait, workflow.WaitEndNewWork(delivered.ID), "supervise/send:"+delivered.ID)
		})
	}
}
