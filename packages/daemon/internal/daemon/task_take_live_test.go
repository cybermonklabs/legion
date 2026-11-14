//go:build e2e

// The live proof that a task is a value the worker takes, not a prompt the daemon pushes, on a
// real NATS through a real Envoy listener (dispatch://INFRA-2811's R3 and R4; its check c8).
// scripts/e2e/task-take.sh builds this driver with `-tags e2e`, owns the nats-server and the
// PostgreSQL it runs against, and builds and runs this checkout's Envoy listener
// (packages/envoy/cmd/listener) on that nats-server, naming it in LEGION_E2E_ENVOY_URL.
//
// The daemon is the whole daemon: run, the function `legion start` calls, through this package's
// whole-daemon harness (startDaemonWithin), with its own task bucket (legion_tasks) on the run's
// NATS — the real configuration, store and migrations, HTTP API, claim supervision and worker
// stream, openWorkflow, JetStream consumers, outbox lanes, task bucket and its watch, and idle
// release. The claim's worker is driven as a pod runs it: each process of it says hello on the
// daemon's worker stream with its launch's boot token, as `legion worker-shim` does, registers its
// session with the real listener on a NATS connection of its own asking for its own consumer
// (delivery: consumer), subscribes the relay subject the listener publishes that session's frames
// on, registers with the daemon, claims its role on the listener (the claim token) and says ready.
// Its plugin holds the newest value it was handed — the registration's answer, or a task frame the
// listener relayed from the bucket — and takes it through the daemon's routes (takeDriver, given
// that value): claims/task-seen, claims/take, agent_start and agent_end on the stream, and
// claims/turn-outcome naming the number.
//
// Seven steps, in order, each printing what it drove and the daemon's own lines it read back:
//
//  1. the boot relaunches the seeded worker, whose ready gives it its phase's task, and the
//     driver sees that task's value relayed on its session's relay subject. This is the check's
//     negative control and the first assertion: against the base, which writes no value and whose
//     listener relays none, the run fails here;
//  2. the worker takes the number, starts and ends the take's turn, and the daemon's lines name the
//     number taken and finished;
//  3. the operator's next task is taken and its process is killed mid-turn; the daemon takes the
//     task back and relaunches the session, and the next registration takes the same number, once;
//  4. a newer task written meanwhile — the relaunched worker moves its phase back (retro to
//     implementing) while the killed turn's task is still owed, which writes the new phase's task
//     into the record — waits behind it and is taken after it, under the next number;
//  5. the daemon's own idle rule releases the issue's pod, which stops the worker; a message to its
//     role topic, which nobody holds, is the listener's wake; the wake relaunches the session and
//     its task is taken once;
//  6. after a daemon restart the finished number is not taken again: nothing is written, nothing is
//     relayed, and a take of it is refused not-owed;
//  7. a claim whose task is finished and whose phase waits registers again as a new process and no
//     value is written. The harness's fake runtime runs with InPod, as #3677's proof
//     (phase_entry_converge_live_test.go) runs it, and answers what an idle release asks of a pod
//     runtime (releaseRuntime), so this step is driven through an idle release cancelled after its
//     role stop, and the run says so.
//
// What stands in lies outside the rule. The broker is a native nats-server and Envoy is this
// checkout's listener, both of the script's. Dispatch is the board the whole-daemon drivers share
// (liveDispatch), GitHub answers any repository with main as its default branch
// (repositoriesGitHub), the Apps' tokens are placeholders, and the daemon's own Envoy publishes go to
// the package's noticeListener: what this proof reads of Envoy is the relay, which the daemon reaches
// through NATS alone. No agent runs: the claim is launched by the package's fake runtime, under the
// Kubernetes configuration whose idle release it reads (releaseRuntime, whose pod is on the configured
// image), and each of its processes is the driver above. The issue, its slot and the worker's
// interrupted launch are seeded through the store, as every daemon test seeds its fixture; the idle
// rule's quiet period is the claims' rows back-dated past it, as the package's release rigs do
// (cutover_merger_repair_test.go's releaseAgain).
package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/legiontasks"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// liveEnvoyURLEnv names the Envoy listener the script runs on the broker liveNATSURLEnv names.
const liveEnvoyURLEnv = "LEGION_E2E_ENVOY_URL"

// The idle release the configuration names (runtime.kubernetes.idle_release) and the tick the
// daemon decides pods on (overrides.idleTick). The period is long enough that no pod is released
// while a step works it, since every release this proof makes is one it back-dates the claims'
// rows for; the backstop is long enough that none fires; the tick is a quarter of a second, so a
// back-dated pod is decided within one.
const (
	taskTakeIdleMinutes     = 10
	taskTakeBackstopMinutes = 24 * 60
	taskTakeIdleTick        = 250 * time.Millisecond
)

// taskTakeBound is how long a step waits for anything the daemon does by itself. It outlasts the
// longest wait a step can owe: the outbox's retry backoff, capped at a minute (retryBackoff), for
// the start row step 4 leaves waiting behind the task the claim holds.
const taskTakeBound = 2 * time.Minute

// taskTakeQuiet is how long a step that must see nothing happen watches for it: several outbox
// sweeps, idle ticks and bucket watch deliveries fit in it.
const taskTakeQuiet = 5 * time.Second

// taskTakeLive is one run of the proof: the booted daemon, its runtime, the listener, the one
// issue and claim it drives, every task frame the listener relayed to any process of the claim,
// and the process that runs now.
type taskTakeLive struct {
	liveProof
	natsURL string
	envoy   string
	client  *http.Client
	rt      *releaseRuntime
	built   *built
	o       overrides
	// logs is the running boot's own log, in JSON; each boot starts a new one, so a step reads
	// the lines of the daemon that runs.
	logs *syncBuffer
	// key is the issue, its own tree; token its implementer's claim; session and sessionFile the
	// session every process of that claim runs, the first registering it and each relaunch
	// resuming it.
	key, session, sessionFile string
	token                     claim.Token

	framesMu sync.Mutex
	frames   []relayedFrame

	worker    *taskWorker
	processes int
}

// relayedFrame is one task frame a process of the claim was relayed on its relay subject: the
// claim it names, the bucket's epoch and revision, put or delete, and the number a put names.
type relayedFrame struct {
	process  int
	claim    string
	epoch    string
	op       string
	revision uint64
	number   uint64
}

func (f relayedFrame) String() string {
	return fmt.Sprintf("%s %s of task %d at epoch %s revision %d to process %d", f.claim, f.op, f.number, f.epoch, f.revision, f.process)
}

// value is the frame as the task value a plugin holds.
func (f relayedFrame) value() claim.TaskValue {
	return claim.TaskValue{Number: f.number, Epoch: f.epoch, Revision: f.revision}
}

// TestATaskIsTakenOnceOnARealNATSLive drives the proof's seven steps against one booted daemon, in
// the order each one's state follows from the last.
func TestATaskIsTakenOnceOnARealNATSLive(t *testing.T) {
	fixtureenv.Live(t)
	r := newTaskTakeLive(t)
	var first relayedFrame
	var killed, newer, woken takenTask
	r.step("step1-a-claim-is-given-a-task-and-the-driver-sees-its-value-relayed", func() { first = r.aTaskIsRelayed() })
	r.step("step2-the-worker-takes-the-number-and-the-daemon-names-it-taken-and-finished", func() { r.takenAndFinished(first) })
	r.step("step3-a-process-killed-mid-turn-has-its-next-registration-take-the-same-number-once", func() { killed = r.killedMidTurn(first.number) })
	r.step("step4-a-newer-task-written-meanwhile-is-taken-after-it", func() { newer = r.newerTakenAfter(killed) })
	r.step("step5-a-released-pods-wake-task-is-taken-once-across-a-relaunch", func() { woken = r.wokenAcrossARelaunch(newer) })
	r.step("step6-after-a-daemon-restart-a-finished-number-is-not-taken-again", func() { r.notTakenAgainAfterARestart(woken) })
	r.step("step7-a-finished-claim-whose-phase-waits-registers-again-and-no-value-is-written", func() { r.registersAgainAndNothingIsWritten(woken) })
}

// newTaskTakeLive boots the daemon over the script's broker, beside the script's listener, with
// the issue already recorded: one root in retro, holding its tree's admission slot, and its
// implementer's claim as a launch a previous daemon began and never recorded, which this boot
// relaunches.
func newTaskTakeLive(t *testing.T) *taskTakeLive {
	t.Helper()
	natsURL, envoyURL := os.Getenv(liveNATSURLEnv), os.Getenv(liveEnvoyURLEnv)
	if natsURL == "" || envoyURL == "" {
		t.Skipf("%s and %s are unset: scripts/e2e/task-take.sh starts a nats-server and an Envoy listener on it and names both", liveNATSURLEnv, liveEnvoyURLEnv)
	}
	proof := readyLiveProof(t, natsURL)
	// The listener owns the notification stream: it created it with its own subjects at its start,
	// before this daemon binds a consumer on it, as a deployment rolls them out.
	if _, err := proof.js.Stream(context.Background(), "ENVOY_NOTIFICATIONS"); err != nil {
		t.Fatalf("the Envoy listener at %s made no ENVOY_NOTIFICATIONS stream on %s: %v", envoyURL, natsURL, err)
	}
	r := &taskTakeLive{liveProof: proof, natsURL: natsURL, envoy: strings.TrimRight(envoyURL, "/"),
		client: &http.Client{Timeout: 20 * time.Second}, built: &built{}, key: proof.cfg.Project + "-1"}
	token, err := claim.NewToken(r.project, r.key, claim.RoleImplementer)
	if err != nil {
		t.Fatal(err)
	}
	r.token = token
	r.session = "ses_task_take_" + randomSuffix(t)
	r.sessionFile = "/sessions/" + r.session + ".jsonl"
	// The Kubernetes configuration, for the idle release it names and the pod runtime it reads.
	// Nothing dials its API: the runtime the boot is given is the fake (prepareSandbox returns
	// before it builds one), as cutover_merger_repair_test.go's rig boots.
	r.cfg.Runtime = kubernetesConfig(t, "https://127.0.0.1:1").Runtime
	r.cfg.Runtime.Kubernetes.IdleRelease = config.IdleReleaseConfig{IdleMinutes: taskTakeIdleMinutes, BackstopMinutes: taskTakeBackstopMinutes}
	r.rt = &releaseRuntime{Runtime: fake.NewRuntime(), onImage: true, sandboxName: sandbox.SandboxName(token)}
	// The runtime provisions each claim's workspace in its pod, as #3677's proof has it say, so a
	// launch reaches the fake without the daemon cloning acme/widgets from github.com.
	r.rt.InPod = true
	r.o = fakeRuntime(taskTakePods{r.rt}, r.built)
	// The daemon's own task bucket on the run's NATS, in place of the in-memory one: the bucket
	// the listener relays.
	r.o.taskValues = nil
	r.o.workflowTokens = idleRigAppTokens{workflowTokenRecorder: &workflowTokenRecorder{}}
	r.o.githubAPI = repositoriesGitHub(t)
	r.o.idleTick = taskTakeIdleTick
	relaunchable := true
	r.o.pluginRelaunchable = &relaunchable
	r.seed()
	r.listenerReady()
	r.boot()
	r.note("booted: the daemon on %s with its own task bucket %s, beside the Envoy listener %s", natsURL, legiontasks.Bucket, r.envoy)
	return r
}

// taskTakePods is the proof's runtime: releaseRuntime, whose pod a teardown that acts deletes and
// a launch makes again, as the Agent Sandbox runtime's does. Without it the runtime would answer
// the issue's Sandbox present after the idle release's teardown, and the rule for a pod no claim
// owns would release that pod again at every tick while the released claim waits for its wake.
type taskTakePods struct{ *releaseRuntime }

func (p taskTakePods) SuspendIssue(ctx context.Context, issue, tree string, authorize func(context.Context) (bool, error)) error {
	return p.releaseRuntime.SuspendIssue(ctx, issue, tree, func(ctx context.Context) (bool, error) {
		act, err := authorize(ctx)
		if err == nil && act {
			p.podGone(true)
		}
		return act, err
	})
}

func (p taskTakePods) Spawn(ctx context.Context, spec runtime.SpawnSpec) (runtime.Locator, error) {
	p.podGone(false)
	return p.releaseRuntime.Spawn(ctx, spec)
}

func (p taskTakePods) Resume(ctx context.Context, prev *runtime.Locator, spec runtime.SpawnSpec) (runtime.Locator, error) {
	p.podGone(false)
	return p.releaseRuntime.Resume(ctx, prev, spec)
}

// podGone records whether the runtime holds the issue's Sandbox (releaseRuntime.IssueSandbox).
func (p taskTakePods) podGone(gone bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sandboxGone = gone
}

// seed records the issue as the daemon would hold it with its implementer at work in retro: a root
// handed to Legion, holding its tree's admission slot, its lifecycle open under the workflow, and
// its implementer's claim mid-launch, serving run 1.
func (r *taskTakeLive) seed() {
	t := r.t
	t.Helper()
	ctx := context.Background()
	status := workflow.PhaseStatus(phase.Retro)
	r.board.show(r.key, status)
	if err := r.st.Tx(ctx, func(tx pgx.Tx) error {
		if err := r.records.PutIssue(ctx, tx, record.Issue{
			Key: r.key, Repo: liveRepo, DefaultBranch: "main", GitHubRepo: liveRepo, Tree: r.key, Project: r.cfg.Project,
			Title: "the issue whose task is taken", Phase: phase.Retro, Generation: 1, Status: status,
			DispatchStatus: status, Rank: "U", LastDispatchSeq: 1, HandedOver: true,
		}); err != nil {
			return err
		}
		return r.admit(ctx, tx, r.key)
	}); err != nil {
		t.Fatalf("seed the issue and its slot: %v", err)
	}
	if _, err := r.st.OpenTreeLifecycle(ctx, r.project, r.key, treelifecycle.AuthorityWorkflow); err != nil {
		t.Fatalf("open the lifecycle of %s: %v", r.key, err)
	}
	if _, err := r.st.AdmitClaim(ctx, supervise.Claim{
		Token: r.token, Project: r.project, Tree: r.key, Issue: r.key, Role: claim.RoleImplementer,
		Generation: 1, State: supervise.StateLaunching, BootTokenHash: supervise.HashBootToken("interrupted-" + randomSuffix(t)),
		ServingGeneration: 1,
	}); err != nil {
		t.Fatalf("record the implementer's unfinished launch: %v", err)
	}
	writePrompt(t, r.cfg, r.token)
	r.note("seeded: %s in retro holding its slot, its implementer %s mid-launch serving run 1", r.key, r.token)
}

// boot runs the daemon, its log kept at Debug in JSON both in memory, where a step reads it, and
// in the file the script names, appended to across a restart.
func (r *taskTakeLive) boot() {
	r.t.Helper()
	r.logs = &syncBuffer{}
	var out io.Writer = r.logs
	if file := r.logFile(); file != nil {
		out = io.MultiWriter(r.logs, file)
	}
	r.d = startDaemonWithin(r.t, r.cfg, r.o, slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: slog.LevelDebug})), 90*time.Second)
}

// Step 1. The boot relaunches the seeded implementer, and the driver brings that launch up as its
// pod would. Its ready finds a phase worker in its own phase with nothing pending, to which no task
// was ever written, so the workflow gives it its phase's resume task (workflow's readyWorkerTask),
// and the claim's machine writes that task's number into the claim's key of the bucket. The first
// assertion is that the listener relayed that value to the session holding the role: a frame on
// the session's relay subject, carrying the claim, the bucket's epoch and the write's revision,
// whose value names the number.
func (r *taskTakeLive) aTaskIsRelayed() relayedFrame {
	t := r.t
	t.Helper()
	launch := r.awaitLaunch(0)
	r.note("the boot relaunched %s: %s at generation %d", r.token, r.launchMethod(launch), launch.Generation)
	frames := r.frameMark()
	w := r.start(launch)
	frame := r.awaitFrame(frames, "the value of the claim's first task relayed to its session", func(f relayedFrame) bool {
		return f.process == w.n && f.claim == string(r.token) && f.op == legiontasks.OpPut
	})
	r.note("relayed on %s%s: %s", idleRigDeliverSubjectPrefix, r.session, frame)
	if frame.number == 0 {
		t.Fatalf("the relayed value names no task number: %s", frame)
	}
	written := r.said(0, "supervise: a task value was written", r.ofTask(frame.number))
	if len(written) == 0 {
		t.Fatalf("the daemon logged no write of task %d's value, which the listener relayed", frame.number)
	}
	r.quote(written...)
	r.quote(r.saidAny(0, "workflow: a ready worker that holds its issue's phase with nothing pending is given its phase's task", r.ofIssue)...)
	return frame
}

// Step 2. The worker takes the number it was relayed, starts the take's turn and ends it, and the
// turn's report names the number: the daemon accepts the take once, logs the turn as that task's,
// finishes the task, deletes the value, and records the number finished in this session.
func (r *taskTakeLive) takenAndFinished(first relayedFrame) {
	t := r.t
	t.Helper()
	w := r.worker
	taken := w.take(fmt.Sprintf("task %d", first.number))
	if taken.Number != first.number {
		t.Fatalf("the worker took task %d, want task %d, the number relayed", taken.Number, first.number)
	}
	r.note("taken: task %d, %q", taken.Number, taken.Text)
	w.work()
	if n := len(r.said(0, "supervise: a take was answered", r.acceptedFirst(taken.Number))); n != 1 {
		t.Fatalf("the daemon accepted %d takes of task %d, want one", n, taken.Number)
	}
	finished := r.said(0, "supervise: a task was finished", r.ofTask(taken.Number))
	if len(finished) != 1 {
		t.Fatalf("the daemon finished task %d %d times, want once", taken.Number, len(finished))
	}
	r.quote(r.said(0, "supervise: a take was answered", r.acceptedFirst(taken.Number))...)
	r.quote(r.said(0, "supervise: a turn started", r.ofTask(taken.Number))...)
	r.quote(r.said(0, "supervise: a turn ended", r.ofTask(taken.Number))...)
	r.quote(finished...)
	r.quote(r.said(0, "supervise: a task value was deleted", r.ofTask(taken.Number))...)
	stored := storedClaim(t, r.cfg, r.token)
	if stored.TaskFinished != taken.Number || stored.TaskFinishedSession != r.session || stored.Pending != nil {
		t.Fatalf("the record holds finished %d in session %q with %+v pending, want task %d finished in %s and nothing pending",
			stored.TaskFinished, stored.TaskFinishedSession, stored.Pending, taken.Number, r.session)
	}
	r.note("the record: written %d, taken %d, finished %d in session %s", stored.TaskWritten, stored.TaskTaken, stored.TaskFinished, stored.TaskFinishedSession)
}

// Step 3. The operator's next task reaches the worker as the claim's next number, the worker takes
// it and starts its turn, and the process is killed mid-turn: its stream and its NATS connection go
// with it, deregistering nothing, and the runtime finds the process gone. The daemon takes the task
// back under its number and relaunches the session; the next process registers, reads the number
// from its registration, and takes it, once.
//
// Before that take, the relaunched worker moves its phase back from retro to implementing (step 4),
// which writes that phase's task into the record while the killed turn's task is still owed.
func (r *taskTakeLive) killedMidTurn(previous uint64) takenTask {
	t := r.t
	t.Helper()
	w := r.worker
	frames := r.frameMark()
	r.deliver("Step three's task: change nothing and end the turn.")
	frame := r.awaitFrame(frames, "the operator's task relayed to the session", func(f relayedFrame) bool {
		return f.process == w.n && f.op == legiontasks.OpPut && f.number > previous
	})
	r.note("relayed: %s", frame)
	taken := w.take(fmt.Sprintf("task %d", frame.number))
	if taken.Number != frame.number {
		t.Fatalf("the worker took task %d, want task %d, the number relayed", taken.Number, frame.number)
	}
	w.driver.mustStartTurn(t)
	r.await(fmt.Sprintf("the turn of task %d running", taken.Number), func() bool {
		return r.d.claim(r.token).State == string(supervise.StateWorking)
	})
	r.note("taken and in its turn: task %d", taken.Number)
	kill := r.logMark()
	launches := r.launches()
	located := r.d.claim(r.token).Locator
	if located == nil {
		t.Fatalf("the working claim %s records no process to kill", r.token)
	}
	w.kill()
	r.rt.Emit(runtime.Observation{Locator: *located, Kind: runtime.Gone, At: time.Now().UTC(), Detail: "the worker's process was killed mid-turn"})
	r.note("killed: the process of generation %d, mid-turn, its stream and its NATS connection closed and nothing deregistered", w.generation)
	launch := r.awaitLaunch(launches)
	if launch.ResumeSessionFile != r.sessionFile {
		t.Fatalf("the relaunch resumes %q, want the session the killed process ran, %s", launch.ResumeSessionFile, r.sessionFile)
	}
	r.note("the daemon relaunched the session: %s at generation %d", r.launchMethod(launch), launch.Generation)
	r.quote(r.said(kill, "supervise: a task was taken back", r.ofTask(taken.Number))...)
	if len(r.said(kill, "supervise: a task was taken back", r.ofTask(taken.Number))) == 0 {
		t.Fatalf("the daemon logged no take-back of task %d at its process's death", taken.Number)
	}

	next := r.start(launch)
	if next.registered.Task == nil || next.registered.Task.Number != taken.Number {
		t.Fatalf("the relaunched process's registration answered value %+v, want task %d, the one the killed turn was taking", next.registered.Task, taken.Number)
	}
	r.note("the relaunched process's registration answered task %d at revision %d, finished %d",
		next.registered.Task.Number, next.registered.Task.Revision, next.registered.Finished)
	r.moveBack()
	again := next.take(fmt.Sprintf("task %d again", taken.Number))
	if again.Number != taken.Number {
		t.Fatalf("the relaunched process took task %d, want task %d, the one the killed turn was taking", again.Number, taken.Number)
	}
	r.note("taken again: task %d, %q", again.Number, again.Text)
	next.work()
	if n := len(r.said(kill, "supervise: a take was answered", r.acceptedFirst(taken.Number))); n != 1 {
		t.Fatalf("after the kill the daemon accepted %d takes of task %d, want one", n, taken.Number)
	}
	if n := len(r.said(0, "supervise: a task was finished", r.ofTask(taken.Number))); n != 1 {
		t.Fatalf("the daemon finished task %d %d times, want once", taken.Number, n)
	}
	r.quote(r.said(kill, "supervise: a take was answered", r.acceptedFirst(taken.Number))...)
	r.quote(r.said(kill, "supervise: a task was finished", r.ofTask(taken.Number))...)
	return again
}

// moveBack is the relaunched implementer moving its own phase back from retro to implementing,
// with a grant of its session, as its `legion` tool does: the transition writes the implementing
// task into the phases row and queues its start, which the claim refuses while it owes the task its
// killed turn was taking.
func (r *taskTakeLive) moveBack() {
	t := r.t
	t.Helper()
	w := r.worker
	status, body := r.d.request(http.MethodPost, "/legion/v1/grants",
		api.GrantRequest{SessionID: r.session, Secret: w.secret, Tree: r.key, Issue: r.key}, false)
	if status != http.StatusOK {
		t.Fatalf("the implementer's grant = %d %s, want 200", status, body)
	}
	var granted api.GrantResponse
	if err := json.Unmarshal(body, &granted); err != nil {
		t.Fatalf("decode the grant: %v", err)
	}
	status, body = r.d.request(http.MethodPost, "/legion/v1/phase/backward", api.PhaseBackwardRequest{GrantID: granted.GrantID,
		To: phase.Implementing, Reason: "the retro found work the head still lacks"}, false)
	if status != http.StatusOK {
		t.Fatalf("the implementer's move back to implementing = %d %s, want 200", status, body)
	}
	r.await("the issue in implementing", func() bool { return r.issue(r.key).Phase == phase.Implementing })
	owed := storedClaim(t, r.cfg, r.token).Pending
	if owed == nil {
		t.Fatalf("the move back found %s owing nothing, want the task its killed turn was taking", r.token)
	}
	r.note("moved back: %s from retro to implementing by its implementer, while task %d is owed again", r.key, owed.Number)
	r.quote(r.saidAny(0, "workflow: phase changed", r.ofIssue)...)
	var id int64
	var attempts int
	var waits string
	r.await("the implementing task's start refused while the claim owes its task", func() bool {
		err := r.st.Pool().QueryRow(context.Background(), `select id, attempts, coalesce(last_error, '') from outbox
			where issue = $1 and kind = 'supervise' and payload->>'op' = 'start' and payload->>'role' = $2 order by id desc limit 1`,
			r.key, string(claim.RoleImplementer)).Scan(&id, &attempts, &waits)
		return err == nil && waits != ""
	})
	if held := storedClaim(t, r.cfg, r.token).Pending; held == nil || held.Number != owed.Number {
		t.Fatalf("while the implementing start waits the claim holds %+v, want task %d still owed", held, owed.Number)
	}
	r.note("the implementing task's start (outbox row %d, %d attempt(s)) waits behind task %d: %s", id, attempts, owed.Number, waits)
}

// Step 4. The implementing task the move back wrote into the record waited behind the task the
// claim owed — its start row refused while that task was pending — and once that task is finished
// the claim's machine writes it as the claim's next number, the listener relays it, and the worker
// takes it: after the task before it, under a greater number, naming its phase.
func (r *taskTakeLive) newerTakenAfter(killed takenTask) takenTask {
	t := r.t
	t.Helper()
	w := r.worker
	r.note("driven: the newer task is the implementing task the relaunched worker's move back wrote, after the kill and the relaunch and before its take of task %d, while that task was owed again; no task was written while the process was down", killed.Number)
	frame := r.awaitFrame(0, "the implementing task relayed to the relaunched process", func(f relayedFrame) bool {
		return f.process == w.n && f.op == legiontasks.OpPut && f.number > killed.Number
	})
	r.note("relayed: %s", frame)
	waited := r.said(0, "supervise: a task value was written", r.ofTask(frame.number))
	r.quote(waited...)
	taken := w.take(fmt.Sprintf("task %d", frame.number))
	if taken.Number != frame.number || taken.Number <= killed.Number || !strings.Contains(taken.Text, "Phase: implementing") {
		t.Fatalf("the worker took %+v, want task %d, after task %d, naming its phase implementing", taken, frame.number, killed.Number)
	}
	r.note("taken: task %d, %q", taken.Number, taken.Text)
	w.work()
	finished := r.said(0, "supervise: a task was finished", r.ofTask(killed.Number))
	accepted := r.said(0, "supervise: a take was answered", r.acceptedFirst(taken.Number))
	if len(finished) != 1 || len(accepted) != 1 {
		t.Fatalf("the daemon finished task %d %d times and accepted task %d %d times, want each once", killed.Number, len(finished), taken.Number, len(accepted))
	}
	logs := r.logs.String()
	if strings.Index(logs, accepted[0]) < strings.Index(logs, finished[0]) {
		t.Fatalf("the daemon accepted task %d's take before it finished task %d", taken.Number, killed.Number)
	}
	r.quote(r.said(0, "supervise: a take was answered", r.acceptedFirst(taken.Number))...)
	r.quote(r.said(0, "supervise: a task was finished", r.ofTask(taken.Number))...)
	return taken
}

// Step 5. The daemon's own idle rule takes the issue's pod once its claims' rows read quiet past
// the idle period: its role stop suspends the implementer as released and has the runtime stop its
// process, whose Envoy plugin deregisters the session as it does at its end, and the teardown deletes
// the pod. A message to the implementer's role topic finds no holder, and the listener reports it
// as a wake; the daemon takes the wake, which resumes the released session in a new process with
// the wake's task, and that process takes the task once.
func (r *taskTakeLive) wokenAcrossARelaunch(previous takenTask) takenTask {
	t := r.t
	t.Helper()
	w := r.worker
	release := r.logMark()
	stops := r.stops()
	r.await("the idle release to stop the implementer and take its pod", func() bool {
		r.quiet()
		held := r.d.claim(r.token)
		return held.State == string(supervise.StateSuspended) && held.SuspendedBy == string(supervise.SuspendedByReleased) &&
			!r.releasing() && r.backstopArmed()
	})
	if r.stops() <= stops {
		t.Fatalf("the release suspended %s and asked the runtime to stop no process of it", r.token)
	}
	w.exit()
	r.note("released: the pod of %s; the implementer suspended as released, the runtime asked to stop its process, which deregistered its session, and the pod deleted", r.key)
	r.quote(r.saidAny(release, "idle release: releasing an issue's pod, which has had no turn in progress and no turn due for the idle period", r.ofIssue)...)
	r.quote(r.said(release, "supervise: suspended", nil)...)
	r.quote(r.saidAny(release, "outbox: released the idle pod of an issue; its roles resume in the same sessions at the next wake", r.ofIssue)...)

	wake := r.logMark()
	launches := r.launches()
	status, body := r.publishRole("a message to a role whose pod was released")
	answer := idleRigRoleAnswerOf(t, body)
	if status != http.StatusNotFound || answer.Reason != idleRigRoleUnclaimed {
		t.Fatalf("the role publish answered %d %s, want 404 %s: no live session holds the role", status, body, idleRigRoleUnclaimed)
	}
	r.note("the listener answered the role message %d %s, and reports it as a wake", status, strings.TrimSpace(body))
	launch := r.awaitLaunch(launches)
	r.note("the wake relaunched the session: %s at generation %d", r.launchMethod(launch), launch.Generation)
	r.quote(r.saidAny(wake, "wake: a released pod is coming back", r.ofIssue)...)
	frames := r.frameMark()
	next := r.start(launch)
	value := next.awaitValue("the wake's task handed to the relaunched process", func(v claim.TaskValue) bool { return v.Number > previous.Number })
	if f, relayed := r.frameOf(frames, next.n, value); relayed {
		r.note("relayed: %s", f)
	} else {
		r.note("handed by the registration's answer: task %d at revision %d", value.Number, value.Revision)
	}
	taken := next.take(fmt.Sprintf("task %d", value.Number))
	if taken.Number != value.Number || !strings.Contains(taken.Text, "this wake brought it back") {
		t.Fatalf("the relaunched process took %+v, want task %d, the wake's", taken, value.Number)
	}
	r.note("taken: task %d, %q", taken.Number, taken.Text)
	next.work()
	if n := len(r.said(wake, "supervise: a take was answered", r.acceptedFirst(taken.Number))); n != 1 {
		t.Fatalf("the daemon accepted %d takes of the wake's task %d, want one", n, taken.Number)
	}
	written := r.said(wake, "supervise: a task value was written", r.ofTask(taken.Number))
	if len(written) == 0 || !strings.Contains(written[0], `"origin":"wake"`) {
		t.Fatalf("the daemon wrote the wake's task %d as %v, want a write of origin wake", taken.Number, written)
	}
	r.quote(written...)
	r.quote(r.said(wake, "supervise: a take was answered", r.acceptedFirst(taken.Number))...)
	r.quote(r.said(wake, "supervise: a task was finished", r.ofTask(taken.Number))...)
	return taken
}

// Step 6. The daemon restarts. The worker's process outlives it, as a pod's does, and its shim
// reconnects to the daemon that started. The boot's read of the bucket finds no value for the
// claim, whose last task is finished, and writes none; nothing is relayed; the claim's value reads
// none with that number finished; and the plugin's take of the finished number, sent again at the
// value it took it at, is refused not-owed.
func (r *taskTakeLive) notTakenAgainAfterARestart(finished takenTask) {
	t := r.t
	t.Helper()
	w := r.worker
	w.driver.mu.Lock()
	took := w.driver.asked
	w.driver.mu.Unlock()
	if took.Number != finished.Number {
		t.Fatalf("the plugin last asked at %+v, want the value of task %d, which it took", took, finished.Number)
	}
	r.d.stop()
	rebindHeldPorts(t, &r.cfg)
	frames := r.frameMark()
	r.boot()
	w.reattach()
	r.await("the reattached worker idle on the restarted daemon", func() bool {
		return r.d.claim(r.token).State == string(supervise.StateIdle)
	})
	r.note("restarted: the daemon booted again on the same store and bucket; the worker's shim reconnected at generation %d", w.generation)
	time.Sleep(taskTakeQuiet)
	if written := r.said(0, "supervise: a task value was written", nil); len(written) != 0 {
		t.Fatalf("the restarted daemon wrote the claim's value: %v", written)
	}
	if puts := r.putsSince(frames); len(puts) != 0 {
		t.Fatalf("the listener relayed a value to the claim after the restart: %v", puts)
	}
	var current claim.TaskResponse
	if err := w.driver.post("/legion/v1/claims/task", claim.TaskRequest{AgentCaller: w.driver.caller, Generation: w.generation}, &current); err != nil {
		t.Fatalf("read the claim's value after the restart: %v", err)
	}
	if current.Task != nil || current.Finished != finished.Number {
		t.Fatalf("after the restart the claim's value is %+v with %d finished, want none with task %d finished", current.Task, current.Finished, finished.Number)
	}
	r.note("over %s the restarted daemon wrote nothing and the listener relayed nothing; claims/task answers no value, task %d finished", taskTakeQuiet, current.Finished)
	var answer claim.TakeResponse
	if err := w.driver.post("/legion/v1/claims/take", claim.TakeRequest{AgentCaller: w.driver.caller, Generation: w.generation,
		Number: took.Number, Epoch: took.Epoch, Revision: took.Revision}, &answer); err != nil {
		t.Fatalf("take the finished task %d: %v", finished.Number, err)
	}
	if answer.Outcome != string(supervise.TakeOutcomeRefused) || answer.Hold != supervise.HoldNotOwed {
		t.Fatalf("a take of the finished task %d was answered %+v, want it refused %q", finished.Number, answer, supervise.HoldNotOwed)
	}
	r.note("a take of the finished task %d was answered %s %s", finished.Number, answer.Outcome, answer.Hold)
	r.quote(r.said(0, "supervise: a take was answered", r.ofTask(finished.Number))...)
}

// Step 7. The claim's last task is finished and its phase, implementing, waits on nothing the
// daemon owes it. The idle rule takes the pod again: its role stop suspends the implementer as
// released and has the runtime stop its process, whose plugin deregisters the session; then,
// before the teardown asks the store again, the operator resumes the stopped role, which overtakes
// the release, so the teardown cancels it. The resume starts a new process in the same session,
// which registers and says ready: the workflow gives a ready worker that finished its last task in
// this session nothing (R4), and no value is written for the claim.
func (r *taskTakeLive) registersAgainAndNothingIsWritten(finished takenTask) {
	t := r.t
	t.Helper()
	w := r.worker
	r.note("driven through an idle release cancelled after its role stop: the harness's fake runtime runs with InPod and answers the pod reads and the teardown an idle release asks of it (releaseRuntime); the release is overtaken by the operator's resume of the stopped claim, made inside the teardown (releaseRuntime.beforeAuthorize) before it asks the store again, and that resume is what starts the new process")
	stops := r.stops()
	resumed := make(chan error, 1)
	var once sync.Once
	r.rt.mu.Lock()
	r.rt.beforeAuthorize = func() { once.Do(func() { resumed <- r.resumeDuringTheTeardown(w, stops) }) }
	r.rt.mu.Unlock()
	defer func() {
		r.rt.mu.Lock()
		r.rt.beforeAuthorize = nil
		r.rt.mu.Unlock()
	}()
	release := r.logMark()
	frames := r.frameMark()
	launches := r.launches()
	var hookErr error
	hooked := false
	r.await("the idle release's teardown, after its role stop", func() bool {
		select {
		case hookErr = <-resumed:
			hooked = true
			return true
		default:
		}
		r.quiet()
		return false
	})
	if hookErr != nil {
		t.Fatalf("the operator's resume during the teardown: %v", hookErr)
	}
	if !hooked {
		t.Fatal("the teardown never asked the store again")
	}
	w.awaitStopped("the release's stop of the implementer's process")
	r.await("the release cancelled", func() bool {
		return len(r.saidAny(release, "outbox: the idle release of an issue's pod did not finish; its roles go on in the same pod", r.ofIssue)) != 0
	})
	r.quote(r.saidAny(release, "idle release: releasing an issue's pod, which has had no turn in progress and no turn due for the idle period", r.ofIssue)...)
	r.quote(r.said(release, "supervise: suspended", nil)...)
	r.quote(r.saidAny(release, "outbox: the idle release of an issue's pod did not finish; its roles go on in the same pod", r.ofIssue)...)
	launch := r.awaitLaunch(launches)
	r.note("the resume relaunched the session: %s at generation %d", r.launchMethod(launch), launch.Generation)
	next := r.start(launch)
	if next.registered.Task != nil || next.registered.Finished != finished.Number {
		t.Fatalf("the new process's registration answered value %+v with %d finished, want none with task %d finished",
			next.registered.Task, next.registered.Finished, finished.Number)
	}
	r.note("the new process registered in session %s: no value, task %d finished", r.session, next.registered.Finished)
	given := "workflow: a ready worker that finished its last task in this session is given nothing; its own reminder wakes it"
	r.await("the workflow's reading of the new process's ready", func() bool { return len(r.said(release, given, nil)) != 0 })
	r.quote(r.said(release, given, nil)...)
	time.Sleep(taskTakeQuiet)
	if written := r.said(release, "supervise: a task value was written", nil); len(written) != 0 {
		t.Fatalf("a value was written for the claim after it registered again: %v", written)
	}
	if puts := r.putsSince(frames); len(puts) != 0 {
		t.Fatalf("the listener relayed a value to the claim after it registered again: %v", puts)
	}
	if held, _ := next.heldValue(); held != nil {
		t.Fatalf("the new process holds value %+v, want none", *held)
	}
	stored := storedClaim(t, r.cfg, r.token)
	if stored.Pending != nil || stored.TaskWritten != finished.Number {
		t.Fatalf("the record holds %+v pending and task %d written, want nothing pending and task %d the last written", stored.Pending, stored.TaskWritten, finished.Number)
	}
	r.note("over %s no value was written and nothing was relayed; the record's last task written is still task %d, finished", taskTakeQuiet, stored.TaskWritten)
}

// resumeDuringTheTeardown runs inside the release's teardown (releaseRuntime.beforeAuthorize),
// once its role stop has suspended the implementer as released and asked the runtime to stop its
// process, which the runtime's count of stops passing stops says: that process ends, its plugin
// deregistering the session (taskWorker.exit), and the operator resumes the claim, which the
// teardown's second read then finds overtaking the release (store's releaseOvertaken). It waits
// until the claim is no longer released. It runs on the daemon's outbox lane, so it answers a
// failure rather than failing the test.
func (r *taskTakeLive) resumeDuringTheTeardown(w *taskWorker, stops int) error {
	stopped, err := r.stoppedAsReleased()
	if err != nil {
		return err
	}
	if !stopped || r.stops() <= stops {
		return fmt.Errorf("the teardown ran before the role stop suspended %s as released and stopped its process", r.token)
	}
	w.exit()
	request, err := http.NewRequest(http.MethodPost, r.d.base+"/legion/v1/operator/claims/"+string(r.token)+"/resume", nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+testOperatorToken)
	response, err := r.client.Do(request)
	if err != nil {
		return fmt.Errorf("the operator's resume: %w", err)
	}
	read, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("the operator's resume = %d %s", response.StatusCode, read)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if stopped, err := r.stoppedAsReleased(); err == nil && !stopped {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("the operator's resume left %s released for 30s", r.token)
}

// stoppedAsReleased is whether the store holds the implementer suspended as released.
func (r *taskTakeLive) stoppedAsReleased() (bool, error) {
	var state, by string
	err := r.st.Pool().QueryRow(context.Background(), `select state, coalesce(suspended_by, '') from claims where token = $1`,
		string(r.token)).Scan(&state, &by)
	return state == string(supervise.StateSuspended) && by == string(supervise.SuspendedByReleased), err
}

// quiet makes the issue's pod read quiet past the idle period, as the package's release rigs do:
// the claims' rows last moved half an hour ago, and their sessions said since that they run no
// background work and have no reminder armed; and the Envoy listener's ready report is fresh. It
// writes both at every call, since a write of the claim's own moves its row forward again.
func (r *taskTakeLive) quiet() {
	r.t.Helper()
	if _, err := r.st.Pool().Exec(context.Background(), `update claims set updated_at = now() - interval '30 minutes',
		activity_at = now() - interval '25 minutes', activity_background = false, activity_kinds = '',
		activity_next_reminder_at = null where issue = $1`, r.key); err != nil {
		r.t.Fatalf("back-date the claims of %s: %v", r.key, err)
	}
	r.listenerReady()
}

// listenerReady records the Envoy listener's ready report as fresh, the gate the idle rule reads
// before it releases anything (R7): the listener published one at its start, before this daemon's
// durable consumer existed, which a consumer created after it does not read (intake's
// createConsumer), and it publishes the next five minutes on.
func (r *taskTakeLive) listenerReady() {
	r.t.Helper()
	if _, err := r.st.Pool().Exec(context.Background(), `insert into listener_reports (project, ready_at) values ($1, now())
		on conflict (project) do update set ready_at = excluded.ready_at`, r.project); err != nil {
		r.t.Fatalf("refresh the listener's ready report: %v", err)
	}
}

// stops is how many times the runtime was asked to stop a process of the claim (Suspend).
func (r *taskTakeLive) stops() int {
	n := 0
	for _, call := range r.rt.Calls() {
		if call.Method == "Suspend" && call.Locator.Claim == r.token {
			n++
		}
	}
	return n
}

// releasing is whether a release of the issue's pod is in flight (its mark stands).
func (r *taskTakeLive) releasing() bool {
	r.t.Helper()
	var marked bool
	if err := r.st.Pool().QueryRow(context.Background(), `select exists (select 1 from issue_releases where issue = $1)`, r.key).Scan(&marked); err != nil {
		r.t.Fatalf("read the release of %s: %v", r.key, err)
	}
	return marked
}

// backstopArmed is whether the release armed the implementer's backstop.
func (r *taskTakeLive) backstopArmed() bool {
	r.t.Helper()
	var armed bool
	if err := r.st.Pool().QueryRow(context.Background(), `select exists (select 1 from pod_wake_backstops
		where issue = $1 and role = $2 and not fired)`, r.key, string(claim.RoleImplementer)).Scan(&armed); err != nil {
		r.t.Fatalf("read the backstop of %s: %v", r.key, err)
	}
	return armed
}

// publishRole publishes message to the implementer's role topic through the listener, keeping
// nothing, as a sender that retries from its own queue does: the answer, and, with no live holder,
// the listener's wake report.
func (r *taskTakeLive) publishRole(message string) (int, string) {
	r.t.Helper()
	encoded, err := json.Marshal(map[string]any{
		"topic": notify.RoleTopicPrefix + string(r.token), "message": message, "keep": false,
		"source": "agent", "source_session": "task-take-live-sender", "idempotency_key": "task-take-wake-" + randomSuffix(r.t),
	})
	if err != nil {
		r.t.Fatal(err)
	}
	response, err := r.client.Post(r.envoy+"/v1/messages/publish", "application/json", strings.NewReader(string(encoded)))
	if err != nil {
		r.t.Fatalf("publish to the role topic: %v", err)
	}
	defer response.Body.Close()
	read, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		r.t.Fatal(err)
	}
	return response.StatusCode, string(read)
}

// deliver is the operator's task to the claim.
func (r *taskTakeLive) deliver(task string) {
	r.t.Helper()
	if status, body := r.d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(r.token)+"/deliver",
		api.DeliverRequest{Task: task}, true); status != http.StatusOK {
		r.t.Fatalf("deliver to %s = %d %s", r.token, status, body)
	}
}

// await polls condition every 50 ms until it holds, within taskTakeBound.
func (r *taskTakeLive) await(what string, condition func() bool) {
	r.t.Helper()
	testwait.WithinPolling(r.t, testwait.Bounded(r.t, taskTakeBound, time.Second), 50*time.Millisecond, what, condition)
}

// launches is how many processes of the claim the runtime was asked to start.
func (r *taskTakeLive) launches() int {
	n := 0
	for _, call := range r.rt.Calls() {
		if (call.Method == "Spawn" || call.Method == "Resume") && call.Spec.Claim == r.token {
			n++
		}
	}
	return n
}

// awaitLaunch waits for a launch of the claim beyond the first after it, and returns the newest.
func (r *taskTakeLive) awaitLaunch(after int) runtime.SpawnSpec {
	r.t.Helper()
	r.await(fmt.Sprintf("launch %d of %s", after+1, r.token), func() bool { return r.launches() > after })
	return lastLaunch(r.t, r.rt.Runtime, r.token)
}

// launchMethod is whether launch resumed the claim's session or started a fresh one.
func (r *taskTakeLive) launchMethod(launch runtime.SpawnSpec) string {
	if launch.ResumeSessionFile != "" {
		return "a resume of " + launch.ResumeSessionFile
	}
	return "a fresh start"
}

// logMark is where the running boot's log stands now, for a step that reads only what follows.
func (r *taskTakeLive) logMark() int { return len(r.logs.String()) }

// said is every line of the running boot's log, from since on, whose message is msg and which is
// the claim's own (its machine's logger, or a line naming it), and that keep holds of, each as the
// daemon wrote it.
func (r *taskTakeLive) said(since int, msg string, keep func(map[string]any) bool) []string {
	return r.lines(since, msg, func(line map[string]any) bool {
		return line["claim"] == string(r.token) && (keep == nil || keep(line))
	})
}

// saidAny is said for a line that names no claim: the workflow's and the outbox's lines of the
// issue, which keep reads.
func (r *taskTakeLive) saidAny(since int, msg string, keep func(map[string]any) bool) []string {
	return r.lines(since, msg, keep)
}

func (r *taskTakeLive) lines(since int, msg string, keep func(map[string]any) bool) []string {
	logs := r.logs.String()
	if since > len(logs) {
		since = len(logs)
	}
	var found []string
	for raw := range strings.SplitSeq(logs[since:], "\n") {
		var line map[string]any
		if json.Unmarshal([]byte(raw), &line) != nil || line["msg"] != msg {
			continue
		}
		if keep == nil || keep(line) {
			found = append(found, raw)
		}
	}
	return found
}

// ofTask keeps a line naming task number.
func (r *taskTakeLive) ofTask(number uint64) func(map[string]any) bool {
	return func(line map[string]any) bool { return line["task"] == float64(number) }
}

// acceptedFirst keeps the answer that accepted a take of task number and applied it, which a take
// sent again and answered from the record does not (its line says repeat).
func (r *taskTakeLive) acceptedFirst(number uint64) func(map[string]any) bool {
	return func(line map[string]any) bool {
		return line["task"] == float64(number) && line["outcome"] == string(supervise.TakeOutcomeAccepted) && line["repeat"] == false
	}
}

// ofIssue keeps a line naming the proof's issue.
func (r *taskTakeLive) ofIssue(line map[string]any) bool { return line["issue"] == r.key }

// quote prints each of the daemon's own lines under the step's heading.
func (r *taskTakeLive) quote(lines ...string) {
	for _, line := range lines {
		r.note("daemon: %s", line)
	}
}

// frameMark is how many task frames have been relayed so far, for a step that reads only what
// follows.
func (r *taskTakeLive) frameMark() int {
	r.framesMu.Lock()
	defer r.framesMu.Unlock()
	return len(r.frames)
}

// awaitFrame waits for a task frame relayed from since on that match holds, and returns it.
func (r *taskTakeLive) awaitFrame(since int, what string, match func(relayedFrame) bool) relayedFrame {
	r.t.Helper()
	var found relayedFrame
	r.await(what, func() bool {
		r.framesMu.Lock()
		defer r.framesMu.Unlock()
		for _, frame := range r.frames[min(since, len(r.frames)):] {
			if match(frame) {
				found = frame
				return true
			}
		}
		return false
	})
	return found
}

// frameOf is the frame relayed from since on to process that carried value, if one did.
func (r *taskTakeLive) frameOf(since, process int, value claim.TaskValue) (relayedFrame, bool) {
	r.framesMu.Lock()
	defer r.framesMu.Unlock()
	for _, frame := range r.frames[min(since, len(r.frames)):] {
		if frame.process == process && frame.op == legiontasks.OpPut && frame.value() == value {
			return frame, true
		}
	}
	return relayedFrame{}, false
}

// putsSince is every put of the claim's value relayed from since on.
func (r *taskTakeLive) putsSince(since int) []relayedFrame {
	r.framesMu.Lock()
	defer r.framesMu.Unlock()
	var puts []relayedFrame
	for _, frame := range r.frames[min(since, len(r.frames)):] {
		if frame.claim == string(r.token) && frame.op == legiontasks.OpPut {
			puts = append(puts, frame)
		}
	}
	return puts
}

// taskWorker is one process of the claim's worker as its pod runs it: the shim on the daemon's
// worker stream; the session, registered with the Envoy listener on a NATS connection of the
// process's own under the process's epoch and client instance, holding the claim's role and taking
// its relay subject; and the plugin that takes the claim's task (takeDriver) from the newest value
// it was handed.
type taskWorker struct {
	r          *taskTakeLive
	n          int
	generation uint64
	bootToken  string
	instance   string
	epoch      string
	secret     string
	registered claim.RegisterResponse
	driver     *takeDriver
	nc         *nats.Conn

	writeMu sync.Mutex
	conn    net.Conn
	lines   *bufio.Reader
	writer  *shimwire.Writer

	mu   sync.Mutex
	held *claim.TaskValue
	// stopped closes once the process has ended: the daemon shut it down, or the test killed it.
	stopped chan struct{}
	ended   bool
}

// start brings launch's process up as its pod's Oh My Pi does, and makes it the claim's process.
func (r *taskTakeLive) start(launch runtime.SpawnSpec) *taskWorker {
	t := r.t
	t.Helper()
	r.processes++
	w := &taskWorker{r: r, n: r.processes, generation: launch.Generation, bootToken: launch.BootToken,
		instance: fmt.Sprintf("task-take-%d-%s", r.processes, randomSuffix(t)),
		epoch:    fmt.Sprintf("task-take-epoch-%d-%s", r.processes, randomSuffix(t)), stopped: make(chan struct{})}
	nc, err := nats.Connect(r.natsURL, nats.Name("task-take-"+r.session), nats.MaxReconnects(-1))
	if err != nil {
		t.Fatalf("connect process %d's session to %s: %v", w.n, r.natsURL, err)
	}
	w.nc = nc
	t.Cleanup(nc.Close)
	if _, err := nc.Subscribe(idleRigDeliverSubjectPrefix+r.session, w.relayed); err != nil {
		t.Fatalf("subscribe process %d to its relay subject: %v", w.n, err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush process %d's subscription: %v", w.n, err)
	}
	w.dial()
	w.subscribe()
	status, body := r.d.request(http.MethodPost, "/legion/v1/claims/register", claim.RegisterRequest{
		BootToken: launch.BootToken, SessionID: r.session, OmpSessionFile: r.sessionFile, AgentID: "task-take-live",
		PluginContract: api.DaemonAPIVersion,
	}, false)
	if status != http.StatusOK {
		t.Fatalf("register process %d of %s = %d %s, want 200", w.n, r.token, status, body)
	}
	if err := json.Unmarshal(body, &w.registered); err != nil {
		t.Fatalf("decode process %d's registration: %v", w.n, err)
	}
	w.secret = w.registered.Secret
	if w.registered.Task != nil {
		w.hold(*w.registered.Task, false)
	}
	w.claimRole()
	w.mu.Lock()
	w.driver = newTakeDriver(func(path string, body, answer any) error { return r.d.post(path, body, answer) }, w.write,
		claim.AgentCaller{ClaimToken: r.token, SessionID: r.session, Secret: w.secret}, w.registered.Generation)
	w.driver.values = w.heldValue
	w.mu.Unlock()
	if status, body := r.d.request(http.MethodPost, "/legion/v1/claims/ready", claim.ReadyRequest{
		ClaimToken: r.token, SessionID: r.session, Secret: w.secret, Generation: w.registered.Generation,
	}, false); status != http.StatusNoContent {
		t.Fatalf("ready process %d of %s = %d %s, want 204", w.n, r.token, status, body)
	}
	r.worker = w
	r.note("process %d up at generation %d: hello on the worker stream, session %s registered with the listener on its own consumer (epoch %s) and with the daemon, role %s claimed, ready",
		w.n, w.generation, r.session, w.epoch, r.token)
	return w
}

// dial says hello on the daemon's worker stream with the launch's boot token, again after a hello
// refused while the stream still holds a connection of the claim it has not read the close of, and
// serves the daemon's frames.
func (w *taskWorker) dial() {
	t := w.r.t
	t.Helper()
	w.r.built.mu.Lock()
	address := w.r.built.address
	w.r.built.mu.Unlock()
	network, socket := "tcp", strings.TrimPrefix(address, "tcp://")
	if path, isUnix := strings.CutPrefix(address, "unix://"); isUnix {
		network, socket = "unix", path
	}
	var last error
	if !testwait.Holds(20*time.Second, 50*time.Millisecond, func() bool {
		conn, err := net.Dial(network, socket)
		if err != nil {
			last = err
			return false
		}
		lines, writer := bufio.NewReader(conn), shimwire.NewWriter(conn)
		if err := writer.WriteFrame(shimwire.Hello2{BootToken: w.bootToken}); err != nil {
			conn.Close()
			last = err
			return false
		}
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		line, err := lines.ReadBytes('\n')
		if err == nil {
			if frame, decodeErr := shimwire.Decode(line); decodeErr == nil && frame.FrameType() == shimwire.TypeHelloAck {
				_ = conn.SetReadDeadline(time.Time{})
				w.writeMu.Lock()
				w.conn, w.lines, w.writer = conn, lines, writer
				w.writeMu.Unlock()
				return true
			}
		}
		conn.Close()
		last = fmt.Errorf("hello refused: %v", err)
		return false
	}) {
		t.Fatalf("process %d's hello on %s was never acked: %v", w.n, address, last)
	}
	conn := w.conn
	t.Cleanup(func() { conn.Close() })
	go w.serve(w.conn, w.lines)
}

// reattach is the process's shim reaching the daemon that started after a restart: the same boot
// token, a new connection, and the plugin's posts going to the new daemon (its post reads the
// running one).
func (w *taskWorker) reattach() {
	w.r.t.Helper()
	w.dial()
}

// serve answers the daemon's frames on conn for as long as it lasts: the protocol negotiation, the
// state probe (idle, or streaming while a turn of the plugin's runs), an adoption of the working
// copy, and the shutdown, which is the process's end. A connection the daemon closed without one is
// a daemon that stopped, which the process outlives.
func (w *taskWorker) serve(conn net.Conn, lines *bufio.Reader) {
	for {
		line, err := lines.ReadBytes('\n')
		if err != nil {
			return
		}
		frame, err := shimwire.Decode(line)
		if err != nil {
			continue
		}
		switch f := frame.(type) {
		case shimwire.NegotiateProtocol:
			_ = w.write(shimwire.Response{ID: f.ID, Command: shimwire.TypeNegotiateProtocol, Success: true})
		case shimwire.GetState:
			w.mu.Lock()
			driver := w.driver
			w.mu.Unlock()
			streaming := driver != nil && driver.inATurn()
			data, _ := json.Marshal(shimwire.StateData{IsStreaming: &streaming})
			_ = w.write(shimwire.Response{ID: f.ID, Command: shimwire.TypeGetState, Success: true, Data: data})
		case shimwire.AdoptWorkingCopy:
			_ = w.write(shimwire.AdoptWorkingCopyResult{ID: f.ID, OK: true})
		case shimwire.Shutdown:
			w.exit()
			_ = conn.Close()
			return
		}
	}
}

// write writes one frame on the process's current connection.
func (w *taskWorker) write(frame shimwire.Frame) error {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	return w.writer.WriteFrame(frame)
}

// subscribe registers the session with the listener as pi-envoy does: no port, the topics it
// follows (none beyond its own agent subject), its client instance, and its own consumer relayed
// under this process's epoch. An answer that is not consumer delivery on the relay subject the
// process reads fails the process's start.
func (w *taskWorker) subscribe() {
	t := w.r.t
	t.Helper()
	var answer struct {
		Delivery *struct {
			Mode    string `json:"mode"`
			Subject string `json:"subject"`
		} `json:"delivery"`
	}
	w.r.envoyPost("/v1/interests/subscribe", map[string]any{
		"session_id": w.r.session, "dir": "/legion/workspaces/" + w.r.key, "topics": []string{},
		"host": "127.0.0.1", "title": string(claim.RoleImplementer), "self_subscribed": true,
		"capabilities": []string{}, "plugin": "task-take-live", "plugin_version": "0", "client_instance_id": w.instance,
		"delivery": map[string]any{"consumer": true, "epoch": w.epoch},
	}, &answer)
	if answer.Delivery == nil || answer.Delivery.Mode != idleRigDeliveryConsumer || answer.Delivery.Subject != idleRigDeliverSubjectPrefix+w.r.session {
		t.Fatalf("the listener registered %s on delivery %+v, want its own consumer relayed on %s%s",
			w.r.session, answer.Delivery, idleRigDeliverSubjectPrefix, w.r.session)
	}
}

// claimRole takes the claim's role on the listener for this process, last-claim-wins, as the
// Legion plugin does at ready: the holder of the role is the session the listener relays the
// claim's value to.
func (w *taskWorker) claimRole() {
	w.r.envoyPost("/v1/roles/set", map[string]any{
		"session_id": w.r.session, "role": string(w.r.token), "client_instance_id": w.instance,
	}, nil)
}

// relayed takes one frame of the session's relay subject. A task frame (legiontasks.HeaderClaim)
// is recorded, and one of the claim's that is newer than the value held is held; it is answered
// nothing, as a client that knows task frames answers none. Any other frame is a message of the
// session's own consumer, acknowledged as a plugin acknowledges one it has written.
func (w *taskWorker) relayed(msg *nats.Msg) {
	key := msg.Header.Get(legiontasks.HeaderClaim)
	if key == "" {
		if msg.Reply != "" {
			_ = w.nc.Publish(msg.Reply, []byte(idleRigRelayReceived))
			_ = w.nc.Publish(msg.Reply, []byte(idleRigRelayDone))
		}
		return
	}
	revision, _ := strconv.ParseUint(msg.Header.Get(legiontasks.HeaderRevision), 10, 64)
	frame := relayedFrame{process: w.n, claim: key, epoch: msg.Header.Get(legiontasks.HeaderEpoch),
		op: msg.Header.Get(legiontasks.HeaderOp), revision: revision}
	if frame.op == legiontasks.OpPut {
		if value, err := legiontasks.Decode(msg.Data); err == nil {
			frame.number = value.Task
		}
	}
	w.r.framesMu.Lock()
	w.r.frames = append(w.r.frames, frame)
	w.r.framesMu.Unlock()
	if key == string(w.r.token) {
		w.hold(frame.value(), frame.op == legiontasks.OpDelete)
	}
}

// hold keeps value as the one the plugin holds when it is newer than the one held: another epoch,
// or a later revision of the same. A delete that is newer leaves none held.
func (w *taskWorker) hold(value claim.TaskValue, deleted bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.held != nil && w.held.Epoch == value.Epoch && value.Revision <= w.held.Revision {
		return
	}
	if deleted {
		if w.held != nil && w.held.Epoch == value.Epoch {
			w.held = nil
		}
		return
	}
	w.held = &value
}

// heldValue is the value the plugin holds, nil for none.
func (w *taskWorker) heldValue() (*claim.TaskValue, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.held == nil {
		return nil, nil
	}
	held := *w.held
	return &held, nil
}

// awaitValue waits until the plugin holds a value match holds, and returns it.
func (w *taskWorker) awaitValue(what string, match func(claim.TaskValue) bool) claim.TaskValue {
	w.r.t.Helper()
	var found claim.TaskValue
	w.r.await(what, func() bool {
		held, _ := w.heldValue()
		if held != nil && match(*held) {
			found = *held
			return true
		}
		return false
	})
	return found
}

// take is the plugin taking the value it holds, failing the test unless a take is accepted within
// taskTakeBound.
func (w *taskWorker) take(what string) takenTask {
	w.r.t.Helper()
	taken, err := w.driver.takeWithin(testwait.Bounded(w.r.t, taskTakeBound, time.Second))
	if err != nil {
		w.r.t.Fatalf("process %d took no task (%s): %v", w.n, what, err)
	}
	return taken
}

// work is the agent running the take's turn: its start, its end with the report naming the task,
// and the session's report that it runs no background work, which follows every turn. It returns
// once the daemon has the claim idle with nothing pending.
func (w *taskWorker) work() {
	t := w.r.t
	t.Helper()
	w.driver.mustStartTurn(t)
	w.driver.mustEndTurn(t)
	w.r.await("the take's turn over and its task finished", func() bool {
		c := w.r.d.claim(w.r.token)
		return c.State == string(supervise.StateIdle) && c.Pending == nil
	})
	if err := w.driver.post("/legion/v1/claims/activity", claim.ActivityRequest{ClaimToken: w.r.token, SessionID: w.r.session,
		Secret: w.secret, Activity: claim.SessionActivity{At: time.Now().UTC()}}, nil); err != nil {
		t.Fatalf("report the session's activity: %v", err)
	}
}

// kill ends the process as a SIGKILL does: its stream and its NATS connection close, and nothing
// deregisters.
func (w *taskWorker) kill() {
	w.mu.Lock()
	ended := w.ended
	w.ended = true
	w.mu.Unlock()
	if ended {
		return
	}
	w.writeMu.Lock()
	_ = w.conn.Close()
	w.writeMu.Unlock()
	w.nc.Close()
	close(w.stopped)
}

// exit ends the process as a shutdown does: its Envoy plugin deregisters the session from the
// listener, which deletes its consumer and frees its role, and its shim's stream to the daemon and
// its NATS connection close with it.
func (w *taskWorker) exit() {
	w.mu.Lock()
	ended := w.ended
	w.ended = true
	w.mu.Unlock()
	if ended {
		return
	}
	for _, path := range []string{"/v1/sessions/", "/v1/interests/"} {
		request, err := http.NewRequest(http.MethodDelete, w.r.envoy+path+w.r.session, nil)
		if err != nil {
			continue
		}
		if response, err := w.r.client.Do(request); err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
		}
	}
	w.writeMu.Lock()
	_ = w.conn.Close()
	w.writeMu.Unlock()
	w.nc.Close()
	close(w.stopped)
}

// awaitStopped waits for the process to end.
func (w *taskWorker) awaitStopped(what string) {
	w.r.t.Helper()
	select {
	case <-w.stopped:
	case <-time.After(testwait.Bounded(w.r.t, taskTakeBound, time.Second)):
		w.r.t.Fatalf("timed out waiting for %s", what)
	}
}

// envoyPost posts body to the listener's route at path, failing the test unless it answers 2xx,
// and decodes the answer into answer (nil reads none).
func (r *taskTakeLive) envoyPost(path string, body, answer any) {
	r.t.Helper()
	if err := postJSON(r.client, r.envoy+path, body, answer); err != nil {
		r.t.Fatalf("the Envoy listener: %v", err)
	}
}
