package daemon

// The note a relaunched process is told of what the last let-go of its process ended
// (dispatch://INFRA-3276 R3, supervise.RelaunchNote), end to end over the real supervision, the
// real workflow and a real Postgres, with a faked runtime: a root architect relaunched holding a
// task and holding none, either one's catch-up, and the catch-up of one whose relaunch also lost
// its session; the daemon-launched controller's start message; a phase worker's resume task
// behind the daemon's own delivery of the note; and the phase task the operator's retry gives a
// phase worker the operator stopped in its task's turn.

import (
	"context"
	"encoding/json"
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
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/texts"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// relaunchDeliveryPrefix starts the id of the daemon's own delivery of a relaunch's note
// (supervise's relaunchTaskPrefix), for the checks that read an id on its own, as a prompt the
// shim was sent carries it; a check of a delivery asks it (supervise.Delivery.Relaunch).
const relaunchDeliveryPrefix = "relaunch:"

// relaunchNote is the relaunch note of issue for the record ended, as a prompt carries it: in
// front of a task (task true), or as the whole text of the daemon's own delivery of it
// (supervise.RelaunchNote).
func relaunchNote(t *testing.T, issue string, ended supervise.EndedWork, task bool) string {
	t.Helper()
	text, err := texts.Render(supervise.RelaunchNote(issue, ended, task))
	if err != nil {
		t.Fatalf("render the relaunch note of %s: %v", issue, err)
	}
	return text
}

// causeLine is the line of a relaunch note that names cause, as its record renders it.
func causeLine(t *testing.T, cause string) string {
	t.Helper()
	text, err := texts.Render(texts.RelaunchFacts{Cause: cause})
	if err != nil {
		t.Fatalf("render the cause %q: %v", cause, err)
	}
	return text
}

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

// catchUps is every catch-up notice the outbox holds, oldest first, as its payload decodes.
func catchUps(t *testing.T, pool *store.Pool) []record.Notice {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		"select payload from outbox where kind = $1 and payload->>'kind' = 'catch-up' order by id", string(record.OutboxKindNotice))
	if err != nil {
		t.Fatalf("read the catch-ups: %v", err)
	}
	payloads, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		t.Fatalf("scan the catch-ups: %v", err)
	}
	notices := make([]record.Notice, len(payloads))
	for i, payload := range payloads {
		if err := json.Unmarshal(payload, &notices[i]); err != nil {
			t.Fatalf("decode the catch-up %s: %v", payload, err)
		}
	}
	return notices
}

// wantCatchUpCarries is the one catch-up the relaunch's ready queued, of the tree at generation 1,
// carrying relaunch, the facts of the relaunch note (supervise.EndedWork.Facts), or none.
func wantCatchUpCarries(t *testing.T, pool *store.Pool, relaunch *texts.RelaunchFacts) {
	t.Helper()
	notices := catchUps(t, pool)
	if len(notices) != 1 {
		t.Fatalf("the outbox holds catch-ups %+v, want the relaunch's one", notices)
	}
	if got := notices[0]; got.CatchUp == nil || got.CatchUp.Generation != 1 || !reflect.DeepEqual(got.Relaunch, relaunch) {
		t.Fatalf("the relaunched architect's catch-up is %+v carrying the relaunch %+v, want the tree at generation 1 and the relaunch %+v",
			got.CatchUp, got.Relaunch, relaunch)
	}
}

// A root architect relaunched holding a task is sent that task behind the note of what the
// relaunch ended — the turn of the task cut off, the death in the runtime's own words, the
// goal-red-team subagent its report named — and no delivery of the daemon's own. The catch-up its
// ready queued carries the same record's facts. The turn that takes the task tells the record; the
// catch-up's copy tells nothing.
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
	if want := relaunchNote(t, rig.claim().Issue, ended, true) + "\n\n" + task; sent.Message != want {
		t.Errorf("the task was sent as %q, want the relaunch note in front of it, %q", sent.Message, want)
	}
	for _, says := range []string{"kind: relaunch-note", causeLine(t, "the process died: "+strconv.Quote(death)),
		"task_turn: true", "stopped[1]: subagent"} {
		if !strings.Contains(sent.Message, says) {
			t.Errorf("the task was sent as %q, want it to say %q", sent.Message, says)
		}
	}
	facts := ended.Facts(false)
	wantCatchUpCarries(t, rig.pool, &facts)

	rig.handle(supervise.StreamTurnStart{Claim: rig.token})
	if stored, err := rig.st.Claims(context.Background()); err != nil || !storedOf(t, stored, rig.token).EndedWork.Told {
		t.Fatalf("after the task's turn began the store holds the record untold (%v), want it told", err)
	}
}

// A root architect relaunched holding nothing — its goal-red-team subagent reported running, its
// process gone — is given a delivery of the daemon's own that carries the note alone, with no
// phase, no run and no entry, and is sent it at once. Its catch-up carries the same facts.
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
	note := relaunchNote(t, rig.claim().Issue, ended, false)
	held := rig.claim().Pending
	if held == nil || !held.Relaunch() || held.Task != note ||
		held.Phase != "" || held.Generation != 0 || held.Entry != 0 {
		t.Fatalf("the relaunched architect holds %+v, want the daemon's own delivery of the note, of no phase, run or entry", held)
	}
	if sent := rig.lastPrompt(); sent.DeliveryID != held.ID || sent.Message != note {
		t.Fatalf("the relaunched architect was sent %+v, want the note alone under %s", sent, held.ID)
	}
	if !strings.Contains(note, "stopped[1]: subagent") {
		t.Errorf("the note %q does not name the subagent", note)
	}
	facts := ended.Facts(false)
	wantCatchUpCarries(t, rig.pool, &facts)
}

// A root architect whose relaunch also lost its session — the resume found the session in no
// store, so a fresh agent runs in its place (supervise's startFresh) — is not told the old
// session's record of what the relaunch ended: the forget of that session ends the record, told by
// no delivery (supervise's forgetSession), since the abandoned session's note says that the old
// session was abandoned where it stood and the fresh session has no transcript of the work the
// record describes. So the catch-up its ready queues states the tree with no relaunch facts, and
// the claim is given no delivery of the daemon's own.
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
		t.Fatalf("the fresh architect holds %+v, want no delivery of the daemon's own: the old session's record is ended and the abandoned session's note waits for its first task", c.Pending)
	}
	wantCatchUpCarries(t, rig.pool, nil)
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
// queued first would make the start procedure wait behind a note. The start message is the
// delivery that carries the note, in front of it, and the turn that takes it tells the record.
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
	stored := storedClaim(t, cfg, token)
	if stored.Pending == nil || stored.Pending.Task != ControllerStartMessage || !stored.EndedWork.Tells() {
		t.Fatalf("the relaunched controller's claim holds %+v with record %+v, want the start message pending and the record untold",
			stored.Pending, stored.EndedWork)
	}
	if want := relaunchNote(t, "", stored.EndedWork, true) + "\n\n" + ControllerStartMessage; prompt.Message != want {
		t.Fatalf("the relaunched controller was prompted %q, want its start message behind the relaunch note, %q", prompt.Message, want)
	}
	if strings.Contains(prompt.Message, "reported: true") {
		t.Errorf("the relaunched controller was prompted %q, want no report of its last turn on record", prompt.Message)
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

// A phase worker relaunched holding nothing is sent the daemon's own delivery of the note, and
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
		t.Fatalf("the relaunched tester holds %+v, want the daemon's own delivery of the relaunch's note", relaunch)
	}
	if rows := outboxRows(t, rig.pool); rows != 1 {
		t.Fatalf("the relaunch left %d outbox rows, want the one start that carries the phase's resume task", rows)
	}
	prompts := rig.conn.Prompts()
	if sent := prompts[len(prompts)-1]; sent.DeliveryID != relaunch.ID || sent.Message != relaunchNote(t, rig.claim().Issue, rig.claim().EndedWork, false) {
		t.Fatalf("the relaunched tester was sent %+v, want the note alone under %s", sent, relaunch.ID)
	}

	// The resume task's row meets the acknowledged delivery and waits for its turn.
	rig.run()
	if held := rig.claim().Pending; held == nil || held.ID != relaunch.ID {
		t.Fatalf("the tester holds %+v after the resume task's row ran, want the acknowledged delivery of the note still", held)
	}

	handleAll(t, rig.machine, supervise.StreamTurnStart{Claim: rig.token}, supervise.StreamTurnEnd{Claim: rig.token},
		supervise.StreamPromptResult{Claim: rig.token, DeliveryID: relaunch.ID, AgentInvoked: true})
	if c := rig.claim(); c.Pending != nil || !c.EndedWork.Told {
		t.Fatalf("after the note's turn the tester holds %+v with record %+v, want nothing pending and the record told", c.Pending, c.EndedWork)
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

// A newer task takes the place of the daemon's own delivery of the note when no prompt of it
// waits on a result — here its send was answered busy, the relaunched agent in a turn of its own —
// and carries the note in front of itself: the phase worker's resume task is not held behind a
// note nobody is reading.
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
		t.Fatalf("the relaunched tester holds %+v, want the daemon's own delivery of the relaunch's note", relaunch)
	}

	// The resume task's row takes the place of the sentence's delivery, and nothing goes into the
	// turn the busy answer named.
	sentBefore := len(rig.conn.Prompts())
	rig.run()
	held := rig.claim().Pending
	if held == nil || held.Task != workflow.ResumePhaseTask(rig.issue) || held.Relaunch() {
		t.Fatalf("after the resume task's row the tester holds %+v, want the resume task in the place of the note's delivery", held)
	}
	if sent := rig.conn.Prompts(); len(sent) != sentBefore {
		t.Fatalf("the tester was sent %+v while the turn the busy answer named ran, want nothing more", sent[sentBefore:])
	}

	// The turn the busy answer named ends, and the resume task goes behind the note, once.
	handleAll(t, rig.machine, supervise.StreamTurnEnd{Claim: rig.token})
	prompts := rig.conn.Prompts()
	if len(prompts) != sentBefore+1 {
		t.Fatalf("the tester was sent %+v once the turn ended, want the resume task alone", prompts[sentBefore:])
	}
	sent := prompts[sentBefore]
	if sent.DeliveryID != held.ID || !strings.HasPrefix(sent.Message, "kind: relaunch-note\n") ||
		!strings.Contains(sent.Message, "stopped[1]: subagent") || !strings.Contains(sent.Message, "task: true") ||
		strings.Contains(sent.Message, "task_turn: true") ||
		!strings.HasSuffix(sent.Message, "\n\n"+workflow.ResumePhaseTask(rig.issue)) {
		t.Fatalf("the resume task was sent as %+v, want it under %s behind the relaunch's note", sent, held.ID)
	}
}

// dueNow brings every outbox row's next attempt to now, so the next pass runs a row that waited.
func (r *readyWorkerRig) dueNow() {
	r.t.Helper()
	if _, err := r.pool.Exec(context.Background(), "update outbox set next_at = now() - interval '1 second'"); err != nil {
		r.t.Fatalf("bring the waiting rows forward: %v", err)
	}
}

// The operator's stop of a phase worker in the turn of its phase's task, its session reporting a
// subagent, and the operator's plain retry after it (dispatch://INFRA-3276 R3, an operator's
// retry), each through the operator's own routes, as `legion claims stop` and `legion claims
// retry` send them. The stop records on the claim's row what it ended, under its own cause: the
// turn, that turn's task, and the subagent. The retry starts the worker's phase again (api's
// retryStart), whose rows relaunch the stopped claim on the session it recorded and hand it the
// phase's task, and the resumed process's first prompt is that task behind the note naming the
// stop and the subagent. The stop retired the task its turn confirmed, so the phase's task was in
// no turn of the stopped process, and the note states that the turn the agent was in was cut off,
// never that it was this task's (supervise's EndedWork.inFrontOf).
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

	// The tester is in the turn of its phase's task, and its session reports a subagent running.
	handleAll(t, rig.machine, supervise.StreamTurnStart{Claim: rig.token})
	rig.report("subagent")
	working := rig.claim()
	if working.State != supervise.StateWorking || working.Pending == nil || working.Pending.ConfirmedAt.IsZero() {
		t.Fatalf("the tester is %s holding %+v, want it working the task its turn confirmed", working.State, working.Pending)
	}
	before, resumes := len(rig.conn.Prompts()), rig.relaunched()

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
	rig.boot()

	prompts := rig.conn.Prompts()
	if len(prompts) != before+1 {
		t.Fatalf("the resumed tester was sent %+v, want the phase's task alone", prompts[before:])
	}
	held, sent := rig.claim().Pending, prompts[before]
	if held == nil || held.Relaunch() || held.Interrupted || held.Phase != phase.Testing || held.ID != sent.DeliveryID ||
		!strings.HasSuffix(sent.Message, "\n\n"+held.Task) {
		t.Fatalf("the resumed tester was sent %+v holding %+v, want the testing phase's task behind the stop's note", sent, held)
	}
	for _, says := range []string{causeLine(t, "its claim was stopped"), "in_turn: true", "stopped[1]: subagent", "task: true"} {
		if !strings.Contains(sent.Message, says) {
			t.Errorf("the phase's task was sent as %q, want it to say %q", sent.Message, says)
		}
	}
	if strings.Contains(sent.Message, "task_turn: true") {
		t.Errorf("the phase's task was sent as %q, which says its own turn was cut off: it was in no turn of the stopped process", sent.Message)
	}
}
