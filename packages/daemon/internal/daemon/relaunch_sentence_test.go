package daemon

// The sentence a relaunched process is told of what the last let-go of its process ended
// (dispatch://INFRA-3276 R3), end to end over the real supervision, the real workflow and a real
// Postgres, with a faked runtime: a root architect relaunched holding a task and holding none,
// either one's catch-up, and the catch-up of one whose relaunch also lost its session; the
// daemon-launched controller's start message; and a phase worker's resume task behind the daemon's
// own delivery of the sentence.

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
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
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// relaunchDeliveryPrefix starts the id of the daemon's own delivery of a relaunch's sentence
// (supervise's relaunchTaskPrefix), for the checks that read an id on its own, as a prompt the
// shim was sent carries it; a check of a delivery asks it (supervise.Delivery.Relaunch).
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

// lastPrompt is the last prompt the architect's connection was sent.
func (r *rootArchitectRig) lastPrompt() fake.Prompt {
	r.t.Helper()
	prompts := r.conn.Prompts()
	if len(prompts) == 0 {
		r.t.Fatal("the architect was sent no prompt")
	}
	return prompts[len(prompts)-1]
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

// A root architect relaunched holding a task is sent that task behind the sentence of what the
// relaunch ended — the turn of the task interrupted, the death in the runtime's own words, the
// goal-red-team subagent its report named — and no delivery of the daemon's own. The catch-up its
// ready queued opens with the same record's sentence. The turn that takes the task tells the
// record; the catch-up's copy tells nothing.
func TestARelaunchSentenceReachesARootArchitectRelaunchedHoldingATask(t *testing.T) {
	rig := newRootArchitectRig(t)
	const task = "Decide the acceptance of LEGION-209."
	rig.handle(supervise.RequestDeliver{Claim: rig.token, Task: task, ID: "outbox:7", Generation: 1},
		supervise.StreamTurnStart{Claim: rig.token})
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
	sent := rig.lastPrompt()
	if sent.DeliveryID != held.ID || !strings.HasSuffix(sent.Message, "\n\n"+task) {
		t.Fatalf("the relaunched architect was sent %+v, want its task under %s", sent, held.ID)
	}
	if !strings.HasPrefix(sent.Message, "Your previous turn on this task was interrupted.") {
		t.Errorf("the task was sent as %q, want it to open with the relaunch's sentence", sent.Message)
	}
	for _, says := range []string{
		"Your process ended because the process died: " + strconv.Quote(death) + ".",
		"Each subagent that you started stopped with that process.",
		"No result of that work will come to you.",
		"Your transcript does not say so.",
	} {
		if !strings.Contains(sent.Message, says) {
			t.Errorf("the task was sent as %q, want it to say %q", sent.Message, says)
		}
	}
	wantCatchUpOpensWith(t, rig.pool, supervise.RelaunchSentence(ended))

	rig.handle(supervise.StreamTurnStart{Claim: rig.token})
	if stored, err := rig.st.Claims(context.Background()); err != nil || !storedOf(t, stored, rig.token).EndedWork.Told {
		t.Fatalf("after the task's turn began the store holds the record untold (%v), want it told", err)
	}
}

// A root architect relaunched holding nothing — its goal-red-team subagent reported running, its
// process gone — is given a delivery of the daemon's own that carries the sentence alone, with no
// phase, no run and no entry, and is sent it at once. Its catch-up opens with the same sentence.
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
	if held == nil || !held.Relaunch() || held.Task != sentence ||
		held.Phase != "" || held.Generation != 0 || held.Entry != 0 {
		t.Fatalf("the relaunched architect holds %+v, want the daemon's own delivery of the sentence, of no phase, run or entry", held)
	}
	if sent := rig.lastPrompt(); sent.DeliveryID != held.ID || sent.Message != sentence {
		t.Fatalf("the relaunched architect was sent %+v, want the sentence alone under %s", sent, held.ID)
	}
	if !strings.Contains(sentence, "Each subagent that you started stopped with that process.") {
		t.Errorf("the sentence %q does not name the subagent", sentence)
	}
	wantCatchUpOpensWith(t, rig.pool, sentence)
}

// A root architect whose relaunch also lost its session — the resume found the session in no
// store, so a fresh agent runs in its place (supervise's startFresh) — has two things to be told:
// what the relaunch ended, and the session it abandoned. The abandoned session's sentence takes
// precedence, as it does in every prompt the claim's machine sends (supervise's Delivery.message,
// queueRelaunch): it says that everything of the old session is gone, and the fresh session has
// no transcript of the work the relaunch's sentence describes. So the catch-up its ready queues
// states the tree with no relaunch sentence in front of it, and the claim is given no delivery of
// the daemon's own.
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
	if !c.EndedWork.Tells() || !c.Abandoned.Tells() {
		t.Fatalf("the relaunch left the record %+v and the abandoned session %+v, want both still to tell", c.EndedWork, c.Abandoned)
	}
	if c.Pending != nil {
		t.Fatalf("the fresh architect holds %+v, want no delivery of the daemon's own: the abandoned session's sentence waits for its first task", c.Pending)
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
// the controller's pod does, and returns the shim the daemon prompts it on.
func controllerReady(t *testing.T, d *daemon, record *built, launch runtime.SpawnSpec, session string) *shim {
	t.Helper()
	sh, registration := registeredLaunch(t, d, record, launch, session)
	if status, body := d.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: launch.Claim, SessionID: session, Secret: registration.Secret, Generation: launch.Generation,
	}, false); status != http.StatusNoContent {
		t.Fatalf("ready = %d; body %s", status, body)
	}
	return sh
}

// workTurn is the controller's agent taking prompt: Oh My Pi's acknowledgement, the turn's start
// and end, and the prompt's result behind it.
func workTurn(sh *shim, prompt shimwire.Prompt) {
	sh.send(shimwire.Response{ID: prompt.ID, Command: shimwire.TypePrompt, Success: true})
	sh.send(shimwire.AgentStart{})
	sh.send(shimwire.AgentEnd{})
	sh.send(shimwire.PromptResult{ID: prompt.ID, AgentInvoked: true})
}

// The daemon-launched controller is given no delivery of the daemon's own at a relaunch: its
// keeper sends the start message at every ready and only logs a refusal of it, so a delivery
// queued first would make the start procedure wait behind a sentence. The start message is the
// delivery that carries the sentence, in front of it, and the turn that takes it tells the record.
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
	sh := controllerReady(t, d, &record, first, "ses_controller")
	start := sh.prompt()
	if start.Message != ControllerStartMessage {
		t.Fatalf("the controller's first launch was prompted %q, want the start message alone", start.Message)
	}
	workTurn(sh, start)
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
	sh = controllerReady(t, d, &record, lastLaunch(t, rt, token), "ses_controller")

	prompt := sh.prompt()
	if strings.HasPrefix(prompt.DeliveryID, relaunchDeliveryPrefix) {
		t.Fatalf("the relaunched controller was sent the daemon's own delivery %s, want its start message", prompt.DeliveryID)
	}
	if !strings.HasPrefix(prompt.Message, "Your previous process ended because the process died") ||
		!strings.Contains(prompt.Message, "The daemon has no report of your background work in that process.") ||
		!strings.HasSuffix(prompt.Message, "\n\n"+ControllerStartMessage) {
		t.Fatalf("the relaunched controller was prompted %q, want its start message behind the relaunch's sentence", prompt.Message)
	}
	if stored := storedClaim(t, cfg, token); stored.Pending == nil || stored.Pending.Task != ControllerStartMessage || !stored.EndedWork.Tells() {
		t.Fatalf("the relaunched controller's claim holds %+v with record %+v, want the start message pending and the record untold",
			stored.Pending, stored.EndedWork)
	}

	workTurn(sh, prompt)
	testwait.Eventually(t, "the start turn to end with nothing left pending", func() bool {
		c := d.claim(token)
		return c.State == string(supervise.StateIdle) && c.Pending == nil
	})
	// The record is told by the confirmation's own write, at the turn's start, before the end
	// that leaves the claim idle (supervise's confirm), so it is read once the turn is over.
	if !storedClaim(t, cfg, token).EndedWork.Told {
		t.Fatal("the start turn left the record untold, want it told by the turn that took the start message")
	}
}

// A phase worker relaunched holding nothing is sent the daemon's own delivery of the sentence, and
// still gets its phase's resume task: the ready read the claim holding nothing before that
// delivery was queued (finishReady), so the workflow queued the task's row, which waits while the
// delivery's acknowledged prompt is the session's next and goes once its turn is over. That turn
// tells the record, so the resume task goes as it was queued.
func TestARelaunchSentenceDeliveryLeavesAPhaseWorkerItsResumeTask(t *testing.T) {
	rig := newReadyWorkerRig(t)
	rig.workTheTask()
	rig.report("subagent")

	rig.observe(runtime.Gone, false)
	rig.boot()

	relaunch := rig.claim().Pending
	if relaunch == nil || !relaunch.Relaunch() {
		t.Fatalf("the relaunched tester holds %+v, want the daemon's own delivery of the relaunch's sentence", relaunch)
	}
	if rows := outboxRows(t, rig.pool); rows != 1 {
		t.Fatalf("the relaunch left %d outbox rows, want the one start that carries the phase's resume task", rows)
	}
	prompts := rig.conn.Prompts()
	if sent := prompts[len(prompts)-1]; sent.DeliveryID != relaunch.ID || sent.Message != supervise.RelaunchSentence(rig.claim().EndedWork) {
		t.Fatalf("the relaunched tester was sent %+v, want the sentence alone under %s", sent, relaunch.ID)
	}

	// The resume task's row meets the acknowledged delivery and waits for its turn.
	rig.run()
	if held := rig.claim().Pending; held == nil || held.ID != relaunch.ID {
		t.Fatalf("the tester holds %+v after the resume task's row ran, want the acknowledged delivery of the sentence still", held)
	}

	handleAll(t, rig.machine, supervise.StreamTurnStart{Claim: rig.token}, supervise.StreamTurnEnd{Claim: rig.token},
		supervise.StreamPromptResult{Claim: rig.token, DeliveryID: relaunch.ID, AgentInvoked: true})
	if c := rig.claim(); c.Pending != nil || !c.EndedWork.Told {
		t.Fatalf("after the sentence's turn the tester holds %+v with record %+v, want nothing pending and the record told", c.Pending, c.EndedWork)
	}

	rig.dueNow()
	rig.run()
	held := rig.claim().Pending
	if held == nil || held.Task != workflow.ResumePhaseTask(rig.issue) || held.Phase != phase.Testing {
		t.Fatalf("after the sentence's turn the tester holds %+v, want the testing phase's resume task", held)
	}
	prompts = rig.conn.Prompts()
	if sent := prompts[len(prompts)-1]; sent.DeliveryID != held.ID || sent.Message != workflow.ResumePhaseTask(rig.issue) {
		t.Fatalf("the resume task was sent as %+v, want it as it was queued: the record was told", sent)
	}
}

// A newer task takes the place of the daemon's own delivery of the sentence when no prompt of it
// waits on a result — here its send was answered busy, the relaunched agent in a turn of its own —
// and carries the sentence in front of itself: the phase worker's resume task is not held behind
// a sentence nobody is reading.
func TestARelaunchSentenceDeliveryGivesWayToAPhaseWorkersResumeTask(t *testing.T) {
	rig := newReadyWorkerRig(t)
	rig.workTheTask()
	rig.report("subagent")

	rig.observe(runtime.Gone, false)
	rig.conn.RefusePrompt(keptBusyRefusal)
	rig.boot()
	rig.conn.FailPrompt(nil)

	relaunch := rig.claim().Pending
	if relaunch == nil || !relaunch.Relaunch() {
		t.Fatalf("the relaunched tester holds %+v, want the daemon's own delivery of the relaunch's sentence", relaunch)
	}

	// The resume task's row takes the place of the sentence's delivery, and nothing goes into the
	// turn the busy answer named.
	sentBefore := len(rig.conn.Prompts())
	rig.run()
	held := rig.claim().Pending
	if held == nil || held.Task != workflow.ResumePhaseTask(rig.issue) || held.Relaunch() {
		t.Fatalf("after the resume task's row the tester holds %+v, want the resume task in the place of the sentence's delivery", held)
	}
	if sent := rig.conn.Prompts(); len(sent) != sentBefore {
		t.Fatalf("the tester was sent %+v while the turn the busy answer named ran, want nothing more", sent[sentBefore:])
	}

	// The turn the busy answer named ends, and the resume task goes behind the sentence, once.
	handleAll(t, rig.machine, supervise.StreamTurnEnd{Claim: rig.token})
	prompts := rig.conn.Prompts()
	if len(prompts) != sentBefore+1 {
		t.Fatalf("the tester was sent %+v once the turn ended, want the resume task alone", prompts[sentBefore:])
	}
	sent := prompts[sentBefore]
	if sent.DeliveryID != held.ID || !strings.HasPrefix(sent.Message, "Your previous process ended because the process died") ||
		!strings.Contains(sent.Message, "Each subagent that you started stopped with that process.") ||
		!strings.HasSuffix(sent.Message, "\n\n"+workflow.ResumePhaseTask(rig.issue)) {
		t.Fatalf("the resume task was sent as %+v, want it under %s behind the relaunch's sentence", sent, held.ID)
	}
}

// dueNow brings every outbox row's next attempt to now, so the next pass runs a row that waited.
func (r *readyWorkerRig) dueNow() {
	r.t.Helper()
	if _, err := r.pool.Exec(context.Background(), "update outbox set next_at = now() - interval '1 second'"); err != nil {
		r.t.Fatalf("bring the waiting rows forward: %v", err)
	}
}
