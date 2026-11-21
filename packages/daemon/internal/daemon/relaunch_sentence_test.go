package daemon

// The sentence a relaunched process is told of what the last let-go of its process ended
// (dispatch://INFRA-3276 R3), end to end over the real supervision, the real workflow and a real
// Postgres, with a faked runtime: a root architect relaunched holding a task and holding none,
// either one's catch-up, and the catch-up of one whose relaunch also lost its session; the
// daemon-launched controller's start message; the one task a finished phase worker is written,
// the daemon's own delivery of the sentence; a phase worker's resume task in the place of that
// delivery; and the phase task the operator's retry gives a phase worker the operator stopped in
// its task's turn.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

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
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// relaunchDeliveryPrefix starts the id of the daemon's own delivery of a relaunch's sentence
// (supervise's relaunchTaskPrefix), for the checks that read an id on its own, as an accepted take
// records it (acceptedTake); a check of a delivery asks it (supervise.Delivery.Relaunch).
const relaunchDeliveryPrefix = "relaunch:"

// rootArchitectRig is a tree's root architect as the daemon runs one: its root in the workflow,
// its claim started by the outbox's start row and supervised over a real Postgres, its ready
// applied to the real workflow (workflowRuntime.terminal), whose catch-up is queued in the outbox,
// and its session's activity report read from the store as the daemon reads it
// (supervise.Deps.ClaimActivity). rt is the faked runtime the claim's processes run on.
type rootArchitectRig struct {
	t       *testing.T
	pool    *store.Pool
	st      *store.Store
	rt      *fake.Runtime
	machine *supervise.Machine
	conn    *fake.Conn
	token   claim.Token
}

// newRootArchitectRig starts the root architect of a running tree and brings its agent to ready.
func newRootArchitectRig(t *testing.T) *rootArchitectRig {
	t.Helper()
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	st, err := store.Open(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("open the store: %v", err)
	}
	t.Cleanup(st.Close)
	records := record.NewStore()
	root := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: "LEGION-208", Title: "Workflow", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"}
	putOutboxIssue(t, pool, records, root)
	if _, err := st.OpenTreeLifecycle(ctx, "legion", root.Key, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("admit the tree: %v", err)
	}
	sup, rt := newOutboxSupervisor(t, "legion", t.TempDir())
	sup.deps.Store = st
	sup.deps.ClaimActivity = st.ClaimActivity
	engine := workflow.New(records, workflow.Config{Project: root.Project, Repositories: testRepositories}, quietLogger())
	sup.OnDurableState((&workflowRuntime{pool: pool, records: records, handlers: []intake.Handler{engine}, log: quietLogger()}).terminal)
	runner := &outbox{log: quietLogger(), dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup,
		trees: st, tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), handlers: []intake.Handler{engine},
		now: time.Now,
		provision: func(context.Context, workspace.Request) (workspace.Workspace, error) {
			return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
		},
	}
	rig := &rootArchitectRig{t: t, pool: pool, st: st, rt: rt, token: mustClaimToken(t, root.Key, claim.RoleArchitect), conn: fake.NewConn()}
	sup.deps.Conns.(*fake.Conns).Register(rig.token, rig.conn)
	if err := runner.execute(ctx, mustOutboxRow(t, root.Key, record.SuperviseRequest{Op: "start", Tree: root.Tree,
		Role: claim.RoleArchitect, Generation: root.Generation}, time.Now())); err != nil {
		t.Fatalf("start the root architect: %v", err)
	}
	machine, found := sup.Machine(rig.token)
	if !found {
		t.Fatalf("the start left no claim for the architect of %s", root.Key)
	}
	rig.machine = machine
	rig.boot()
	return rig
}

func (r *rootArchitectRig) claim() supervise.Claim { return r.machine.Claim() }

// handle hands the machine its events in order and waits for what they set going.
func (r *rootArchitectRig) handle(events ...supervise.Event) {
	r.t.Helper()
	handleAll(r.t, r.machine, events...)
}

// handleAll hands m its events in order and waits for what they set going.
func handleAll(t *testing.T, m *supervise.Machine, events ...supervise.Event) {
	t.Helper()
	for _, ev := range events {
		if err := m.Handle(context.Background(), ev); err != nil {
			t.Fatalf("handle %T: %v", ev, err)
		}
	}
	m.Wait()
}

// boot drives the launched process through its shim's hello, its agent's registration and its
// ready, at the generation the claim is launching.
func (r *rootArchitectRig) boot() {
	r.t.Helper()
	generation := r.claim().Generation
	r.handle(supervise.StreamHello{Claim: r.token, Generation: generation},
		supervise.RequestRegister{Claim: r.token, Generation: generation, Session: "ses-architect", SessionFile: "/tmp/architect.jsonl"},
		supervise.RequestReady{Claim: r.token, Generation: generation, Session: "ses-architect"})
}

// report is the architect's session reporting a goal-red-team subagent running, through the
// store's own write onto the launch it runs now (putActivity).
func (r *rootArchitectRig) report() {
	r.t.Helper()
	putActivity(r.t, r.st, r.token, r.claim().Generation, "subagent")
}

// dies is the runtime finding the architect's process gone, and the relaunch that follows it
// booting the new process to ready. It returns the runtime's account of the death.
func (r *rootArchitectRig) dies() string {
	r.t.Helper()
	observation := runtime.Observation{Locator: *r.claim().Locator, Kind: runtime.Gone, At: time.Now(),
		Detail: "pod legion-legion-208 (uid 8a1f) ended: the node was drained"}
	r.handle(supervise.RuntimeObservation{Observation: observation})
	r.boot()
	return string(observation.Kind) + ": " + observation.Detail
}

// take is the architect's worker taking the task its claim holds, as its plugin takes one while
// its host is idle, and the take the daemon accepted; a take not accepted fails the test.
func (r *rootArchitectRig) take() acceptedTake {
	r.t.Helper()
	took, accepted := takeOffered(context.Background(), r.t, r.machine)
	if !accepted {
		r.t.Fatalf("the architect's take of %+v was not accepted", r.claim().Pending)
	}
	return took
}

// catchUpReasons is the reason of every catch-up notice the outbox holds, oldest first: the text
// the architect reads of its catch-up.
func catchUpReasons(t *testing.T, pool *store.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		"select payload->>'reason' from outbox where kind = $1 and payload->>'kind' = 'catch-up' order by id", string(record.OutboxKindNotice))
	if err != nil {
		t.Fatalf("read the catch-ups: %v", err)
	}
	reasons, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("scan the catch-ups: %v", err)
	}
	return reasons
}

// wantCatchUpOpensWith is the one catch-up the relaunch's ready queued, opening with sentence and
// going on with the tree as the daemon records it.
func wantCatchUpOpensWith(t *testing.T, pool *store.Pool, sentence string) {
	t.Helper()
	reasons := catchUpReasons(t, pool)
	if len(reasons) != 1 {
		t.Fatalf("the outbox holds catch-ups %q, want the relaunch's one", reasons)
	}
	if !strings.HasPrefix(reasons[0], sentence+"\n\n") || !strings.Contains(reasons[0], "the tree of LEGION-208 at generation 1") {
		t.Fatalf("the relaunched architect's catch-up reads %q, want it to open with the relaunch's sentence %q and then state the tree",
			reasons[0], sentence)
	}
}

// A root architect relaunched holding a task is given that task behind the sentence of what the
// relaunch ended — the turn of the task interrupted, the death in the runtime's own words, the
// goal-red-team subagent its report named — and no delivery of the daemon's own. The catch-up its
// ready queued opens with the same record's sentence. The take that accepts the task tells the
// record; the catch-up's copy tells nothing.
func TestARelaunchSentenceReachesARootArchitectRelaunchedHoldingATask(t *testing.T) {
	rig := newRootArchitectRig(t)
	const task = "Decide the acceptance of LEGION-209."
	rig.handle(supervise.RequestDeliver{Claim: rig.token, Task: task, ID: "outbox:7", Generation: 1})
	rig.take()
	rig.handle(supervise.StreamTurnStart{Claim: rig.token})
	rig.report()

	death := rig.dies()

	ended := rig.claim().EndedWork
	if !ended.Tells() || !ended.TaskTurn || len(ended.Kinds) != 1 || ended.Kinds[0] != "subagent" {
		t.Fatalf("the relaunch recorded %+v, want an untold record of the task's turn and the subagent", ended)
	}
	held := rig.claim().Pending
	if held == nil || held.Task != task || held.Relaunch() {
		t.Fatalf("the relaunched architect holds %+v, want its task and no delivery of the daemon's own", held)
	}
	wantCatchUpOpensWith(t, rig.pool, supervise.RelaunchSentence(ended))
	took := rig.take()
	if took.DeliveryID != held.ID || !strings.HasSuffix(took.Message, "\n\n"+task) {
		t.Fatalf("the relaunched architect took %+v, want its task under %s", took, held.ID)
	}
	if !strings.HasPrefix(took.Message, "Your previous turn on this task was interrupted.") {
		t.Errorf("the task was taken as %q, want it to open with the relaunch's sentence", took.Message)
	}
	for _, says := range []string{
		"Your process ended because the process died: " + strconv.Quote(death) + ".",
		"Each subagent that you started stopped with that process.",
		"No result of that work will come to you.",
		"Your transcript does not say so.",
	} {
		if !strings.Contains(took.Message, says) {
			t.Errorf("the task was taken as %q, want it to say %q", took.Message, says)
		}
	}
	if stored, err := rig.st.Claims(context.Background()); err != nil || !storedOf(t, stored, rig.token).EndedWork.Told {
		t.Fatalf("after the accepted take the store holds the record untold (%v), want it told", err)
	}
}

// A root architect relaunched holding nothing — its goal-red-team subagent reported running, its
// process gone — is given a delivery of the daemon's own that carries the sentence alone, with no
// phase, no run and no entry, numbered under origin notice, and its take is answered with that
// sentence. Its catch-up opens with the same sentence.
func TestARelaunchSentenceReachesARootArchitectRelaunchedHoldingNothing(t *testing.T) {
	rig := newRootArchitectRig(t)
	if held := rig.claim().Pending; held != nil {
		t.Fatalf("the started architect holds %+v, want nothing", held)
	}
	rig.report()

	rig.dies()

	ended := rig.claim().EndedWork
	if !ended.Tells() || ended.Turn || len(ended.Kinds) != 1 {
		t.Fatalf("the relaunch recorded %+v, want an untold record of the subagent alone", ended)
	}
	sentence := supervise.RelaunchSentence(ended)
	held := rig.claim().Pending
	if held == nil || !held.Relaunch() || held.Task != sentence || held.Origin != supervise.OriginNotice || held.Number == 0 ||
		held.Phase != "" || held.Generation != 0 || held.Entry != 0 {
		t.Fatalf("the relaunched architect holds %+v, want the daemon's own numbered delivery of the sentence, of origin notice and no phase, run or entry", held)
	}
	if took := rig.take(); took.DeliveryID != held.ID || took.Message != sentence {
		t.Fatalf("the relaunched architect took %+v, want the sentence alone under %s", took, held.ID)
	}
	if !strings.Contains(sentence, "Each subagent that you started stopped with that process.") {
		t.Errorf("the sentence %q does not name the subagent", sentence)
	}
	wantCatchUpOpensWith(t, rig.pool, sentence)
}

// A root architect whose relaunch also lost its session — the resume found the session in no
// store, so a fresh agent runs in its place (supervise's startFresh) — is not told the old
// session's record of what the relaunch ended: the forget of that session ends the record, told by
// no delivery (supervise's forgetSession), since the abandoned session's sentence says that
// everything of the old session is gone and the fresh session has no transcript of the work the
// record describes. So the catch-up its ready queues states the tree with no relaunch sentence in
// front of it, and the claim is given no delivery of the daemon's own.
func TestARelaunchSentenceGivesWayToTheAbandonedSessionInTheCatchUp(t *testing.T) {
	rig := newRootArchitectRig(t)
	rig.report()
	rig.rt.ScriptResume(fake.SpawnResult{Err: fmt.Errorf("resume %s: %w: the session table holds no such session",
		rig.token, runtime.ErrSessionFileMissing)})

	rig.handle(supervise.RuntimeObservation{Observation: runtime.Observation{
		Locator: *rig.claim().Locator, Kind: runtime.Gone, At: time.Now()}})
	generation := rig.claim().Generation
	rig.handle(supervise.StreamHello{Claim: rig.token, Generation: generation},
		supervise.RequestRegister{Claim: rig.token, Generation: generation, Session: "ses-fresh", SessionFile: "/tmp/fresh.jsonl"},
		supervise.RequestReady{Claim: rig.token, Generation: generation, Session: "ses-fresh"})

	c := rig.claim()
	if c.EndedWork.Cause == "" || !c.EndedWork.Told || c.EndedWork.ToldBy != "" || !c.Abandoned.Tells() {
		t.Fatalf("the relaunch left the record %+v and the abandoned session %+v, want the record ended by the forget, told by no delivery, and the abandoned session still to tell", c.EndedWork, c.Abandoned)
	}
	stored, err := rig.st.Claims(context.Background())
	if err != nil {
		t.Fatalf("read the claims: %v", err)
	}
	if ended := storedOf(t, stored, rig.token).EndedWork; !ended.Told || ended.ToldBy != "" {
		t.Fatalf("the store holds the record %+v, want it ended by the forget, told by no delivery", ended)
	}
	if c.Pending != nil {
		t.Fatalf("the fresh architect holds %+v, want no delivery of the daemon's own: the old session's record is ended and the abandoned session's sentence waits for its first task", c.Pending)
	}
	reasons := catchUpReasons(t, rig.pool)
	if len(reasons) != 1 {
		t.Fatalf("the outbox holds catch-ups %q, want the relaunch's one", reasons)
	}
	if strings.Contains(reasons[0], supervise.RelaunchSentence(c.EndedWork)) || !strings.Contains(reasons[0], "the tree of LEGION-208 at generation 1") {
		t.Fatalf("the fresh architect's catch-up reads %q, want the tree with no sentence of what the relaunch ended", reasons[0])
	}
}

// controllerReady connects launch's shim, registers its agent as session and says it is ready, as
// the controller's pod does, and returns the plugin of that session (takeDriver), which takes the
// controller's task over the claim routes and runs its turns on the shim.
func controllerReady(t *testing.T, d *daemon, record *built, launch runtime.SpawnSpec, session string) (*shim, *takeDriver) {
	t.Helper()
	sh, registration := registeredLaunch(t, d, record, launch, session)
	if status, body := d.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: launch.Claim, SessionID: session, Secret: registration.Secret, Generation: launch.Generation,
	}, false); status != http.StatusNoContent {
		t.Fatalf("ready = %d; body %s", status, body)
	}
	return sh, d.takeDriver(sh, launch.Claim, session, registration.Secret, launch.Generation)
}

// The daemon-launched controller is given no delivery of the daemon's own at a relaunch: its
// keeper sends the start message at every ready and only logs a refusal of it, so a delivery
// queued first would make the start procedure wait behind a sentence. The start message is the
// delivery that carries the sentence, in front of it, and the take that accepts it tells the
// record.
func TestARelaunchSentenceGoesInFrontOfTheDaemonLaunchedControllersStartMessage(t *testing.T) {
	cfg := testConfig(t)
	cfg.ControllerLaunch = config.ControllerLaunchDaemon
	rt := fake.NewRuntime()
	var record built
	d := startDaemon(t, cfg, fakeRuntime(rt, &record))
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		t.Fatal(err)
	}
	token := claim.ControllerToken(project)
	first := controllerLaunched(t, rt, token)
	sh, worker := controllerReady(t, d, &record, first, "ses_controller")
	if start := worker.mustTake(t, "the controller takes its start message"); start.Text != ControllerStartMessage {
		t.Fatalf("the controller's first launch took %q, want the start message alone", start.Text)
	}
	worker.mustStartTurn(t)
	worker.mustEndTurn(t)
	testwait.Eventually(t, "the start turn to end", func() bool {
		c := d.claim(token)
		return c.State == string(supervise.StateIdle) && c.Pending == nil
	})

	// The controller's process dies while it is idle. Its session left no report the daemon holds,
	// so what background work it ended is unknown, which the relaunch records.
	// Its shim goes with it.
	sh.conn.Close()
	rt.Emit(runtime.Observation{Locator: *d.claim(token).Locator, Kind: runtime.Gone})
	testwait.Eventually(t, "the controller's relaunch", func() bool {
		return lastLaunch(t, rt, token).Generation > first.Generation
	})
	_, worker = controllerReady(t, d, &record, lastLaunch(t, rt, token), "ses_controller")

	took := worker.mustTake(t, "the relaunched controller takes its start message")
	if !strings.HasPrefix(took.Text, "Your previous process ended because the process died") ||
		!strings.Contains(took.Text, "The daemon has no report of the background work of your last turn.") ||
		!strings.HasSuffix(took.Text, "\n\n"+ControllerStartMessage) {
		t.Fatalf("the relaunched controller took %q, want its start message behind the relaunch's sentence", took.Text)
	}
	// The take that carried the sentence told the record in its own write (supervise's accept).
	if stored := storedClaim(t, cfg, token); stored.Pending == nil || stored.Pending.Task != ControllerStartMessage ||
		stored.Pending.Relaunch() || !stored.EndedWork.Told || stored.EndedWork.ToldBy != stored.Pending.ID {
		t.Fatalf("the relaunched controller's claim holds %+v with record %+v, want the start message pending and the record told by its take",
			stored.Pending, stored.EndedWork)
	}

	worker.mustStartTurn(t)
	worker.mustEndTurn(t)
	testwait.Eventually(t, "the start turn to end with nothing left pending", func() bool {
		c := d.claim(token)
		return c.State == string(supervise.StateIdle) && c.Pending == nil
	})
	if !storedClaim(t, cfg, token).EndedWork.Told {
		t.Fatal("the start turn left the record untold, want it told by the take that accepted the start message")
	}
}

// A phase worker that finished its task in its session and is relaunched holding nothing, its
// subagent reported running in the process that died, is written one task: the daemon's own
// delivery of the sentence, numbered under origin notice. The ready read the claim holding nothing
// in the session it finished its task in, so the workflow queued it no resume task
// (dispatch://INFRA-2811 R4, finishedInItsSession), and the delivery is queued after that read
// (finishReady) because the record shows work the relaunch ended (D2). Its accepted take tells the
// record, and the turn and the report that names its number finish it; nothing more is written.
func TestARelaunchSentenceIsTheOneTaskWrittenToAFinishedWorkerWhoseSubagentDied(t *testing.T) {
	rig := newReadyWorkerRig(t)
	rig.workTheTask()
	rig.report("subagent")
	finished := rig.claim()
	if finished.TaskFinished != finished.TaskWritten || finished.TaskFinishedSession != testerSession {
		t.Fatalf("the tester that worked its task records task %d of %d finished in %q, want its last task finished in %s",
			finished.TaskFinished, finished.TaskWritten, finished.TaskFinishedSession, testerSession)
	}

	rig.observe(runtime.Gone, false)
	rig.boot(testerSession)

	relaunch := rig.claim().Pending
	if relaunch == nil || !relaunch.Relaunch() || relaunch.Origin != supervise.OriginNotice || relaunch.Number != finished.TaskWritten+1 {
		t.Fatalf("the relaunched tester holds %+v, want the daemon's own delivery of the relaunch's sentence, task %d of origin %s",
			relaunch, finished.TaskWritten+1, supervise.OriginNotice)
	}
	if rows := outboxRows(t, rig.pool); rows != 0 {
		t.Fatalf("the relaunch left %d outbox rows, want none: a worker that finished its task in its session is queued no resume task", rows)
	}
	sentence := supervise.RelaunchSentence(rig.claim().EndedWork)
	if took, accepted := takeOffered(context.Background(), t, rig.machine); !accepted || took.DeliveryID != relaunch.ID || took.Message != sentence {
		t.Fatalf("the relaunched tester took %+v (accepted %v), want the sentence alone under %s", took, accepted, relaunch.ID)
	}
	if c := rig.claim(); !c.EndedWork.Told || c.EndedWork.ToldBy != relaunch.ID {
		t.Fatalf("after the accepted take the record is %+v, want it told by %s", c.EndedWork, relaunch.ID)
	}

	handleAll(t, rig.machine, supervise.StreamTurnStart{Claim: rig.token}, supervise.StreamTurnEnd{Claim: rig.token},
		supervise.RequestTurnOutcome{Claim: rig.token, Generation: rig.claim().Generation, Sequence: turnSequence.Add(1), Task: relaunch.Number})
	rig.run()
	if c := rig.claim(); c.Pending != nil || !c.EndedWork.Told || c.TaskWritten != relaunch.Number {
		t.Fatalf("after the sentence's turn the tester holds %+v with record %+v and task %d written, want nothing pending, the record told and no task written past %d",
			c.Pending, c.EndedWork, c.TaskWritten, relaunch.Number)
	}
}

// A newer task takes the place of the daemon's own delivery of the sentence while no take of it is
// accepted, and carries the sentence in front of itself: the phase worker's resume task is not
// held behind a sentence nobody has read. The worker here was never written a task (its start
// carried none), so its first ready queued it its phase's resume task, which has not run when its
// process, a subagent reported running, dies. The relaunch's ready finds that row covering the
// phase and queues none of its own, and finishReady gives the claim, which holds nothing, the
// delivery of the sentence. The resume task's row then runs and replaces that delivery.
func TestARelaunchSentenceDeliveryGivesWayToAPhaseWorkersResumeTask(t *testing.T) {
	rig := newStartedTesterRig(t, "")
	if held := rig.claim().Pending; held != nil || rig.claim().TaskWritten != 0 {
		t.Fatalf("the tester started with no task holds %+v with task %d written, want nothing written", held, rig.claim().TaskWritten)
	}
	if rows := outboxRows(t, rig.pool); rows != 1 {
		t.Fatalf("the first ready of a tester never written a task left %d outbox rows, want the one start of its phase's resume task", rows)
	}
	rig.report("subagent")

	rig.observe(runtime.Gone, false)
	rig.boot(testerSession)

	relaunch := rig.claim().Pending
	if relaunch == nil || !relaunch.Relaunch() || !relaunch.ConfirmedAt.IsZero() {
		t.Fatalf("the relaunched tester holds %+v, want the daemon's own delivery of the relaunch's sentence, untaken", relaunch)
	}
	if rows := outboxRows(t, rig.pool); rows != 1 {
		t.Fatalf("the relaunch left %d outbox rows, want the one start of the phase's resume task its first ready queued", rows)
	}

	// The resume task's row takes the place of the sentence's delivery.
	rig.run()
	held := rig.claim().Pending
	if held == nil || held.Task != workflow.ResumePhaseTask(rig.issue) || held.Relaunch() || held.Origin != supervise.OriginResume {
		t.Fatalf("after the resume task's row the tester holds %+v, want the resume task in the place of the sentence's delivery", held)
	}
	took, accepted := takeOffered(context.Background(), t, rig.machine)
	if !accepted || took.DeliveryID != held.ID || !strings.HasPrefix(took.Message, "Your previous process ended because the process died") ||
		!strings.Contains(took.Message, "Each subagent that you started stopped with that process.") ||
		!strings.HasSuffix(took.Message, "\n\n"+workflow.ResumePhaseTask(rig.issue)) {
		t.Fatalf("the tester took %+v (accepted %v), want the resume task under %s behind the relaunch's sentence", took, accepted, held.ID)
	}
}

// The operator's stop of a phase worker in the turn of its phase's task, its session reporting a
// subagent, and the operator's plain retry after it (dispatch://INFRA-3276 R3, an operator's
// retry), each through the operator's own routes, as `legion claims stop` and `legion claims
// retry` send them. The stop records on the claim's row what it ended, under its own cause: the
// turn, that turn's task, and the subagent. The retry starts the worker's phase again (api's
// retryStart), whose rows relaunch the stopped claim on the session it recorded and hand it the
// phase's task, and the resumed process's first accepted take is that task behind the sentence
// naming the stop and the subagent. The stop retired the task its take held, so the phase's task
// was in no turn of the stopped process, and the sentence says that the turn the agent was in
// was interrupted, never its previous turn on this task (supervise's EndedWork.inFrontOf).
func TestARelaunchSentenceTellsTheOperatorsRetryOfAStoppedWorkerWhatTheStopEnded(t *testing.T) {
	rig := newReadyWorkerRig(t)
	ctx := context.Background()
	// The operator's own routes over this rig's supervisor and workflow, wired as the daemon wires
	// them (newFreshSessionRig). The answer of a claim route carries the claim's step record,
	// which the supervisor reads from the store itself (supervisor.Step), so the supervisor holds
	// the rig's store for it.
	rig.sup.store = rig.st
	operatorAPI := api.NewServer("127.0.0.1", 8437, api.Options{
		Supervisor: rig.sup, BootTokens: api.NewBootTokens(rig.st), Project: "legion", DispatchProject: "LEGION",
		OperatorToken: freshSessionOperatorToken, Controller: rig.st, Trees: rig.st, Releaser: rig.rt, Pool: rig.pool,
		Record: rig.runner.records, Handlers: rig.runner.handlers, RoleFor: workflow.RoleFor, Log: quietLogger(),
	}).Handler
	operator := func(request string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/legion/v1/operator/claims/"+string(rig.token)+"/"+request, nil)
		r.Header.Set("Authorization", "Bearer "+freshSessionOperatorToken)
		recorder := httptest.NewRecorder()
		operatorAPI.ServeHTTP(recorder, r)
		return recorder
	}

	// The issue is at work, so it holds the admission slot its work holds: the task half of the
	// retry's start, which relaunches the stopped claim, is claimable only while its issue holds
	// one (workflow.StartOrOwe, record.NeedsSlot), as newFreshSessionRig seeds it.
	if _, err := rig.pool.Exec(ctx, `insert into slots (issue, index, admitted_at) values ($1, 0, now())`, rig.issue.Key); err != nil {
		t.Fatalf("seed the issue's admission slot: %v", err)
	}

	// The tester is in the turn of its phase's task, which its take accepted, and its session
	// reports a subagent running.
	acceptTake(ctx, t, rig.machine)
	handleAll(t, rig.machine, supervise.StreamTurnStart{Claim: rig.token})
	rig.report("subagent")
	working := rig.claim()
	if working.State != supervise.StateWorking || working.Pending == nil || working.Pending.ConfirmedAt.IsZero() {
		t.Fatalf("the tester is %s holding %+v, want it working the task its take accepted", working.State, working.Pending)
	}
	resumes := rig.relaunched()

	if recorder := operator("stop"); recorder.Code != http.StatusOK {
		t.Fatalf("the operator's stop = %d, want 200; body %s", recorder.Code, recorder.Body)
	}
	stored, err := rig.st.Claims(ctx)
	if err != nil {
		t.Fatalf("read the claims: %v", err)
	}
	retired := storedOf(t, stored, rig.token)
	if retired.State != supervise.StateRetired || retired.Pending != nil {
		t.Fatalf("after the stop the store holds the tester %s holding %+v, want it retired with the task its turn confirmed retired",
			retired.State, retired.Pending)
	}
	ended := retired.EndedWork
	if ended.At.IsZero() {
		t.Errorf("the stop recorded %+v, want the moment of its let-go", ended)
	}
	ended.At = time.Time{}
	if want := (supervise.EndedWork{Cause: "its claim was stopped", Generation: working.Generation, Turn: true, TaskTurn: true,
		Background: true, Kinds: []string{"subagent"}, Reported: true}); !reflect.DeepEqual(ended, want) {
		t.Fatalf("the stop recorded %+v on the claim's row, want %+v", ended, want)
	}

	if recorder := operator("retry"); recorder.Code != http.StatusAccepted {
		t.Fatalf("the operator's retry of the stopped tester = %d, want 202; body %s", recorder.Code, recorder.Body)
	}
	// A start is two rows (workflow.StartOrOwe): the relaunch leaves a retired claim to the row
	// that carries the task, so the drain runs until both have run.
	for range 3 {
		rig.run()
	}
	if got := rig.relaunched(); got != resumes+1 {
		t.Fatalf("the retry resumed the tester's session %d times, want once", got-resumes)
	}
	rig.boot(testerSession)

	held := rig.claim().Pending
	took, accepted := takeOffered(ctx, t, rig.machine)
	if !accepted || held == nil || held.Relaunch() || held.Interrupted || held.Phase != phase.Testing || held.ID != took.DeliveryID ||
		!strings.HasSuffix(took.Message, "\n\n"+held.Task) {
		t.Fatalf("the resumed tester took %+v (accepted %v) holding %+v, want the testing phase's task behind the stop's sentence", took, accepted, held)
	}
	for _, says := range []string{
		"Your previous process ended because its claim was stopped.",
		"The turn you were in was interrupted.",
		"Each subagent that you started stopped with that process.",
		"Then continue the task.",
	} {
		if !strings.Contains(took.Message, says) {
			t.Errorf("the phase's task was taken as %q, want it to say %q", took.Message, says)
		}
	}
	if strings.Contains(took.Message, "Your previous turn on this task was interrupted.") {
		t.Errorf("the phase's task was taken as %q, which says its own turn was interrupted: it was in no turn of the stopped process", took.Message)
	}
}
