//go:build e2e

// The live proof of dispatch://INFRA-3276 R4 and R3 on the binary that has them: a worker in a
// turn is sent its task once and again only after that turn, the turn that serves a task settles
// it, a newer task takes its place, and a relaunched process reads, in the first prompt it is
// sent, what the relaunch ended of its subagents, its background jobs and its turn.
// scripts/e2e/busy-relaunch-delivery.sh builds this driver with `-tags e2e` and owns the
// nats-server and the PostgreSQL it runs against; with `--base-commit` it lays this one file over
// a `git archive` of that commit and runs it there, which is the proof's negative control.
//
// It is a sibling of claim_compaction_live_test.go and provider_error_retry_live_test.go, and it
// takes what they share as it is: liveAgentProof's reads of a claim and of the daemon's own log
// (live_agent_proof_test.go), the run's own database, `legion` and the two plugins built from the
// checkout, paneStandin (claim_compaction_live_test.go), which answers every turn alike and tells
// the plugin's side question and a compaction's summary apart, and the local runtime that starts a
// pane where there is no tmux (localruntime_live_test.go). Every stand-in this proof adds is in
// this file, so the file compiles over the base commit, where none of the shared files has it:
//
//   - a Hindsight stand-in (hindsightStandin). One pane's overlay turns Oh My Pi's memory backend
//     on at it, and a step arms it to hold the recall of the next prompt. At the pin Oh My Pi
//     acknowledges a prompt at its admission and then waits on that recall before the turn's
//     agent_start (session/agent-session.ts, #promptWithMessage then #prepareAgentStart), and the
//     session counts as streaming the whole time, so a held recall is the measured wait between
//     the acknowledgement and the turn (dispatch://INFRA-3276/artifact/r4-repeat-source-md). The
//     stand-in answers every recall 503, held or not: a recall that succeeds is committed to the
//     session and the next prompt recalls nothing, and one that fails is not, which a run of the
//     pin made for this proof read (a recall answered 200 made the next prompt start its turn at
//     once; one answered 503 made the next prompt recall again).
//   - a scripted answer in front of paneStandin (scriptedStandin): a `tool_use` answer, ported
//     from the plugins' own pane harness (packages/pi-shared/test/omp-harness.ts, messageStream),
//     so a turn starts a `task` subagent or an `async` bash job, and a held answer, so a turn —
//     the late turn, a turn of the agent's own, a subagent's own request — stays open until the
//     step lets it go or the process that asked dies.
//   - the Envoy listener a pane registers with (newConsumerPaneListener), which answers that
//     registration with a consumer of the session's own, as a session has needed since
//     cybermonklabs/legion#2603; the shared newPaneListener answers none, at this head and at the
//     base alike, and every pane that registers with it ends its claim boot.
//
// Each step runs as a subtest of its own (run), so a step that fails ends there and the next
// step still runs: the negative control's one run shows every step's own failure, each at the
// assertion the step names (fails). Each step drives a claim no earlier step left in a state of
// its own, and waits for it to hold no task and run no turn before it drives it (awaitQuiet), so
// a step that failed leaves the next one a claim it can read.
//
// What a step reads of the relaunch it reads where no new Go symbol is needed: the words of the
// sentence as literals (relaunchDiedWords and its siblings), and the claim row's `ended_work`
// through SQL (readEndedWork), so over the base commit, whose schema has no such column, the read
// itself is the failure of the assertion that reads it rather than a driver that does not build.
package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/stream"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// busyRecallHold is how long the Hindsight stand-in holds the recall of a prompt a step armed it
// for. It must outlast the daemon's own wait for the prompt's turn (`worker_rpc_timeout_seconds`,
// the machine's Timeouts.RPC: supervise's noTurn) by more than one observation of the local
// runtime (localObserveInterval), so that wait runs out, the task is taken back, and the next
// sweep sends it again into the recall, which is the measured loop; and it must stay under Oh My
// Pi's own bound on a recall (busyRecallClientBound), past which the client gives the recall up
// by itself. The setup refuses a configuration this no longer sits between.
const busyRecallHold = 20 * time.Second

// busyRecallClientBound is `hindsight.recallTimeoutMs` at the pin (30 s), which no overlay of
// this proof changes.
const busyRecallClientBound = 30 * time.Second

// busyConfirmedWithin is how long a step reads the operator's claim listing, with the late turn's
// model request held, for the task confirmed: the turn's agent_start reaches the daemon before its
// model request reaches the stand-in, and the confirmation is written as that start is handled.
const busyConfirmedWithin = 10 * time.Second

// busyOwnJobSeconds is how long the `async` bash job of busy-own-turn sleeps: long enough that the
// turn which started it has ended before the job's result starts a turn of Oh My Pi's own.
const busyOwnJobSeconds = 20

// busyOwnTurnWatch is how long busy-own-turn holds that turn of the agent's own open with a task
// waiting: two observations of the local runtime, each of which sends what a ready or idle claim
// holds (supervise's observe), and the settle every proof watches for what must not happen.
const busyOwnTurnWatch = 2*localObserveInterval + liveSettle

// relaunchFirstPromptWithin is how long a relaunch step waits, from the relaunched process's
// ready, for the first prompt that process is sent. At this head the prompt goes at the ready
// (supervise's queueRelaunch, or the held task itself); the bound is what lets the base, which
// sends a root architect holding nothing no prompt at all, fail the step rather than the run.
const relaunchFirstPromptWithin = 90 * time.Second

// relaunchReportWithin bounds the wait for the pane's activity report to reach the store. The
// pane reads its host's jobs and reports what changed every five seconds
// (pi-legion/extensions/legion.ts, SESSION_ACTIVITY_PERIOD_MS).
const relaunchReportWithin = time.Minute

// deepWorkerSystemLine is the first line of packages/pi-legion/agents/deep-worker.md's body, which
// Oh My Pi puts in the system prompt of a `task` subagent of that agent, and nowhere else: it is
// how the stand-in tells the subagent's own request from a turn of the role's, as the plugins'
// own pin test does (packages/pi-legion/extensions/legion-session-activity-omp.test.ts).
const deepWorkerSystemLine = "You are an autonomous coding worker"

// The words of the relaunch sentence a step reads, as supervise's relaunchSentence
// (supervise/relaunch.go) writes them at this head for a death (endDied, supervise/endedwork.go).
// They are literals rather than calls of supervise.RelaunchSentence because this driver is also
// built over the base commit, which has no such function, and a sentence whose words change fails
// this proof here, naming the words it looked for.
const (
	// relaunchDiedWords is the cause, in either sentence: "Your previous process ended because
	// the process died: …" alone, and "Your process ended because the process died: …" in front
	// of a task whose turn the death cut off.
	relaunchDiedWords = "ended because the process died: "
	// relaunchCatchUpOpens is how the sentence alone opens, which is how a root architect's
	// catch-up reason opens (workflow's QueueCatchUp).
	relaunchCatchUpOpens = "Your previous process ended because the process died: "
	// relaunchSubagentWords and relaunchJobWords name each kind of background work the process's
	// own last report named (backgroundKinds).
	relaunchSubagentWords = "Each subagent that you started stopped with that process."
	relaunchJobWords      = "Each background job that you started stopped with that process."
	// relaunchCutTaskWords opens the sentence in front of a task the death cut off in its turn.
	relaunchCutTaskWords = "Your previous turn on this task was interrupted."
	// endedWorkDiedCause opens the cause the claim row records for a death (endDied).
	endedWorkDiedCause = "the process died: "
)

// The five panes, by the provider name each one's stand-in is given in the run's models.yml.
const (
	busyLateProvider       = "latepane"
	busyOwnProvider        = "ownpane"
	relaunchWorkerProvider = "workerpane"
	relaunchRootProvider   = "architectpane"
	relaunchTaskProvider   = "taskpane"
)

// busyRelaunchLive is one run of the proof: the booted daemon, its five panes, and the Hindsight
// stand-in one of them recalls from.
type busyRelaunchLive struct {
	liveAgentProof
	// panes is the five live claims, in the order they are seeded.
	panes []*busyPane
	// late, own, worker, root and task are the five issues, one pane each and each its own tree.
	late, own, worker, root, task string
	// home and profile are the Oh My Pi home and profile every pane runs under.
	home, profile string
	// overlays is each claim's PI_CONFIG_FILES, in order.
	overlays map[claim.Token][]string
	// hindsight is the memory backend the late pane's overlay names.
	hindsight *hindsightStandin
	// failedAt is the assertion the running step failed at (fails), which run prints.
	failedAt string
}

// busyPane is one live claim of the proof: the issue it works, the phase that issue stands in,
// the role that works it, the stand-in its Oh My Pi talks to, and whether its overlay turns the
// memory backend on.
type busyPane struct {
	issue    string
	title    string
	at       phase.Phase
	role     claim.Role
	provider string
	recalls  bool
	token    claim.Token
	stand    *scriptedStandin
}

// TestABusyAgentIsSentItsTaskOnceAndARelaunchSaysWhatItEndedLive drives the seven steps, each on a
// claim of its own or on one an earlier step left holding nothing: the late turn of a held
// recall, a turn of the agent's own, a newer task in a late turn, a worker in no turn, and the
// three relaunches.
func TestABusyAgentIsSentItsTaskOnceAndARelaunchSaysWhatItEndedLive(t *testing.T) {
	fixtureenv.Live(t)
	r := newBusyRelaunchLive(t)
	r.run("busy-late-turn", r.busyLateTurn)
	r.run("busy-own-turn", r.busyOwnTurn)
	r.run("newer-task", r.newerTask)
	r.run("idle-control", r.idleControl)
	r.run("relaunch-subagent-worker", r.relaunchSubagentWorker)
	r.run("relaunch-subagent-architect", r.relaunchSubagentArchitect)
	r.run("relaunch-task", r.relaunchTask)
}

// newBusyRelaunchLive readies everything a pane needs before the daemon boots — the `legion`
// binary and the two plugins built from the checkout, an Oh My Pi profile of the run's own routed
// to the five stand-ins, the issues seeded with their worker mid-launch — boots the daemon over
// the local runtime, and waits for every pane to answer what its seed left pending.
func newBusyRelaunchLive(t *testing.T) *busyRelaunchLive {
	t.Helper()
	omp := os.Getenv(liveOmpPathEnv)
	if omp == "" {
		t.Skipf("%s is unset, so there is no Oh My Pi for a pane to run; scripts/e2e/busy-relaunch-delivery.sh requires it", liveOmpPathEnv)
	}
	takeOwnDatabase(t)
	proof := newLiveProof(t, "scripts/e2e/busy-relaunch-delivery.sh")
	key := func(n int) string { return fmt.Sprintf("%s-%d", proof.cfg.Project, n) }
	r := &busyRelaunchLive{
		liveAgentProof: liveAgentProof{liveProof: proof, omp: omp},
		late:           key(1), own: key(2), worker: key(3), root: key(4), task: key(5),
	}
	r.panes = []*busyPane{
		{issue: r.late, title: "the issue whose implementer's turns wait on a held memory recall", at: phase.Implementing,
			role: claim.RoleImplementer, provider: busyLateProvider, recalls: true},
		{issue: r.own, title: "the issue whose tester's background job starts a turn of its own", at: phase.Testing,
			role: claim.RoleTester, provider: busyOwnProvider},
		{issue: r.worker, title: "the issue whose planner is idle, then dies with a subagent running", at: phase.Planning,
			role: claim.RolePlanner, provider: relaunchWorkerProvider},
		{issue: r.root, title: "the issue whose root architect dies with a subagent running", at: phase.Accepting,
			role: claim.RoleArchitect, provider: relaunchRootProvider},
		{issue: r.task, title: "the issue whose implementer dies in its task's turn with a background job running", at: phase.Implementing,
			role: claim.RoleImplementer, provider: relaunchTaskProvider},
	}
	for _, pane := range r.panes {
		token, err := claim.NewToken(r.project, pane.issue, pane.role)
		if err != nil {
			t.Fatal(err)
		}
		pane.token = token
	}
	if busyRecallHold <= r.cfg.WorkerRPCTimeout+localObserveInterval || busyRecallHold >= busyRecallClientBound {
		t.Fatalf("the held recall (%s) must outlast the daemon's wait for a turn (%s) and one observation (%s), and stay under Oh My Pi's own bound on a recall (%s)",
			busyRecallHold, r.cfg.WorkerRPCTimeout, localObserveInterval, busyRecallClientBound)
	}
	// Five trees hold a slot each, which is one over the fixture's cap (testConfig): admission
	// reads the cap over the slots it repairs, and every tree here must be one it holds.
	r.cfg.AdmissionCap = len(r.panes)
	version, err := exec.Command(omp, "--version").Output()
	if err != nil {
		t.Fatalf("read the Oh My Pi version of %s: %v", omp, err)
	}
	r.note("Oh My Pi %s at %s (the pod's build, which is the fleet's pin and not this branch's fetch)",
		strings.TrimSpace(string(version)), omp)

	root := proofCheckoutRoot(t)
	r.note("the daemon, legion and both plugins are this checkout's: %s", root)
	work := t.TempDir()
	r.legion = buildProofLegion(t, root, work)
	r.home = filepath.Join(work, "omp-home")
	r.profile = "legion-busy-relaunch-" + strings.ToLower(r.cfg.Project)
	installProofPlugins(t, root, work, r.home, r.profile, omp)
	r.hindsight = newHindsightStandin(t)
	for _, pane := range r.panes {
		pane.stand = newScriptedStandin(t, pane.provider)
	}
	r.writeOmpProfile()
	r.overlays = r.writeOverlays(work)

	r.rt = newLocalRuntime(t, r.cfg.StateDir, r.legion, omp, r.home, r.profile)
	r.rt.daemonURL, r.rt.envoyURL, r.rt.natsURLs = r.cfg.DaemonURL, newConsumerPaneListener(t), r.cfg.NatsURLs
	r.rt.overlays = func(token claim.Token) []string { return r.overlays[token] }

	r.seed()
	r.daemon = newRecordingLog(r.log())
	r.d = startDaemonWithin(t, r.cfg, r.overrides(), slog.New(r.daemon), 60*time.Second)
	for _, pane := range r.panes {
		r.awaitReady(pane.token)
	}
	for _, pane := range r.panes {
		r.awaitQuiet(pane.token, 4*time.Minute)
		if pane.role != claim.RoleArchitect && len(pane.stand.promptsCarrying(startTask(pane.role))) == 0 {
			t.Fatalf("%s's stand-in answered no turn carrying the task its start left pending, %q", pane.token, startTask(pane.role))
		}
	}
	r.note("every pane is ready and holds nothing: each phase worker answered the task its start left pending, and the root architect owes none")
	return r
}

// overrides is the daemon this proof boots: the local runtime over the real worker stream, the API
// listener the fixture holds, and the workflow's App tokens stood in for. The clock is the real
// one: the daemon's wait for a turn is what a held recall outlasts, and it is measured where it is
// real.
func (r *busyRelaunchLive) overrides() overrides {
	return overrides{
		runtime: func(_ context.Context, listener *stream.Listener, address string, _ appauth.Tokens,
			_ func(context.Context, string) (ghrepo.Repository, string, error), _ *store.Store, _ func() []ghrepo.Repository) (runtime.Runtime, error) {
			r.rt.conns, r.rt.stream = listener, address
			return r.rt, nil
		},
		listen:         heldListen,
		workflowTokens: &workflowTokenRecorder{},
	}
}

// seed records the five issues as the daemon would hold them at the moment each one's worker
// began, as claim_compaction_live_test.go's seed does and for its reasons: each a root handed to
// Legion, in the phase its worker works, holding its tree's admission slot with its lifecycle
// open, and that worker's claim mid-launch, which this boot relaunches. Each phase worker's claim
// holds the task its phase's start left pending, which is what keeps admission's repair of a
// slotted worker holding no task off it (internal/admit/admit.go's resumeSlottedWorkers) — that
// repair would give the claim its phase's resume task, and every deliver of this driver would be
// refused `a delivery is already pending` — and every pane answers it before the first step. The
// root architect's issue stands in accepting, where no phase worker runs, so the repair passes it
// over as it passes over every architect; its claim holds nothing, as an architect's start leaves
// it.
func (r *busyRelaunchLive) seed() {
	t := r.t
	t.Helper()
	ctx := context.Background()
	for _, pane := range r.panes {
		r.board.show(pane.issue, "in_progress")
	}
	if err := r.st.Tx(ctx, func(tx pgx.Tx) error {
		keys := make([]string, 0, len(r.panes))
		for _, pane := range r.panes {
			if err := r.records.PutIssue(ctx, tx, record.Issue{
				Key: pane.issue, Repo: liveRepo, DefaultBranch: "main", GitHubRepo: liveRepo, Tree: pane.issue,
				Project: r.cfg.Project, Title: pane.title, Phase: pane.at, Generation: 1, Status: "in_progress",
				DispatchStatus: "in_progress", Rank: "U", LastDispatchSeq: 1, HandedOver: true,
			}); err != nil {
				return err
			}
			keys = append(keys, pane.issue)
		}
		return r.admit(ctx, tx, keys...)
	}); err != nil {
		t.Fatalf("seed the issues: %v", err)
	}
	for _, pane := range r.panes {
		if _, err := r.st.OpenTreeLifecycle(ctx, r.project, pane.issue, treelifecycle.AuthorityWorkflow); err != nil {
			t.Fatalf("open the lifecycle of %s: %v", pane.issue, err)
		}
		if _, err := r.st.AdmitClaim(ctx, supervise.Claim{
			Token: pane.token, Project: r.project, Tree: pane.issue, Issue: pane.issue, Role: pane.role,
			Generation: 1, State: supervise.StateLaunching,
			BootTokenHash: supervise.HashBootToken("seeded-" + randomSuffix(t)), ServingGeneration: 1,
		}); err != nil {
			t.Fatalf("record the claim %s: %v", pane.token, err)
		}
		writePrompt(t, r.cfg, pane.token)
		if pane.role == claim.RoleArchitect {
			continue
		}
		// Unconfirmed and unread: the launch this seed interrupts never put the task in front of
		// an agent, so the relaunched pane is sent it once it reports ready.
		if err := r.st.PutDelivery(ctx, pane.token, supervise.Delivery{
			ID: "seeded-start-" + randomSuffix(t), Task: startTask(pane.role), Generation: 1,
			Phase: pane.at, QueuedAt: time.Now(),
		}); err != nil {
			t.Fatalf("record the task %s's start left pending: %v", pane.token, err)
		}
	}
	for _, pane := range r.panes {
		r.note("seeded %s (%s, %s) with its claim %s mid-launch", pane.issue, pane.at, pane.role, pane.token)
	}
}

// ---- how a step runs and fails ------------------------------------------------------------------

// run runs one step as a subtest of its own, between its `== name` and `ok name (<s>s)` lines,
// with the proof's reads failing that subtest rather than the whole run: every read of
// liveAgentProof goes through r.t, which is the step's own testing.T while the step runs. A step
// that fails prints `FAIL name at <assertion> (<s>s)`, naming the assertion it failed at (fails),
// and the next step runs.
func (r *busyRelaunchLive) run(name string, body func()) {
	parent := r.t
	parent.Run(name, func(t *testing.T) {
		r.t, r.failedAt = t, ""
		started := time.Now()
		defer func() {
			r.t = parent
			if t.Failed() {
				at := r.failedAt
				if at == "" {
					at = "none of its named assertions (a wait or a read of its own)"
				}
				fmt.Printf("FAIL %s at %s (%ds)\n", name, at, int(time.Since(started).Seconds()))
			}
		}()
		fmt.Printf("== %s\n", name)
		body()
		fmt.Printf("ok %s (%ds)\n", name, int(time.Since(started).Seconds()))
	})
}

// fails ends the running step at its named assertion, which run prints and the script's negative
// control reads back.
func (r *busyRelaunchLive) fails(assertion, format string, args ...any) {
	r.t.Helper()
	r.failedAt = assertion
	r.t.Fatalf(assertion+": "+format, args...)
}

// ---- the steps ----------------------------------------------------------------------------------

// busyLateTurn is R4's measured case: a phase worker's task whose turn waits on a held recall,
// past the daemon's own wait for it. That wait runs out and takes the task back, the next sweep
// sends it again into the recall, Oh My Pi answers that send busy, and the turn of the first
// prompt starts late. At this head that late turn is the task's own: one busy answer, the turn
// confirms the task while it runs and its prompt's result retires it, and nothing is sent after
// it. The base drops that result at the fence and sends the task again at the turn's end
// (stale-result).
func (r *busyRelaunchLive) busyLateTurn() {
	pane := r.pane(r.late)
	r.awaitQuiet(pane.token, 3*time.Minute)
	marker := "LATE-" + randomSuffix(r.t)
	task := "Say the word " + marker + " and nothing else."
	r.hindsight.holdNext(busyRecallHold)
	late := pane.stand.hold(promptCarrying(marker))
	from := r.daemon.count()
	sent := time.Now()
	r.deliver(pane.token, task)
	arrived := late.await(r, "the late turn's model request", busyRecallHold+2*time.Minute)
	view := r.awaitListedConfirmed(pane.token, marker)
	r.note("the task went at +0s; its turn's model request came at +%s, and while it was held `legion claims list --json` read the task %s",
		arrived.Sub(sent).Round(100*time.Millisecond), describePending(view.Pending, marker))
	ended := r.daemon.count()
	late.let()
	turnEnd := r.awaitLogFrom("the late turn's end", ended, 2*time.Minute, streamTurnEnded(pane.token))
	r.awaitQuiet(pane.token, 3*time.Minute)

	recall, held := r.hindsight.recallCarrying(marker)
	if !held || recall.held < busyRecallHold || arrived.Before(recall.answered) {
		r.fails("recall-held", "the task's prompt was to wait %s on its memory recall before its turn, and the stand-in read %s",
			busyRecallHold, recall.describe(held))
	}
	r.note("Oh My Pi recalled for the prompt at +%s and the stand-in answered 503 after %s, at +%s",
		recall.at.Sub(sent).Round(100*time.Millisecond), recall.held, recall.answered.Sub(sent).Round(100*time.Millisecond))
	waited := r.daemon.firstAfter(from, promptWaitRanOut(pane.token))
	if waited == nil || waited.at.After(recall.answered) {
		r.fails("wait-ran-out", "the daemon's %s wait for the prompt's turn did not run out inside the recall: %v", r.cfg.WorkerRPCTimeout, waited)
	}
	r.note("the daemon's own wait for the turn ran out at +%s and charged the prompt: %q",
		waited.at.Sub(sent).Round(100*time.Millisecond), waited.Attrs["why"])
	// What the step read of the busy answers and the turns is noted before any of it is judged, so
	// a run that fails at the first assertion below — the base's, at stale-result — still prints
	// the loop it measured.
	busy := r.busyDeliveries(pane.token, from)
	turns := pane.stand.promptsCarrying(marker)
	r.note("the agent answered %d sends of the task busy; %d turns carried the task, at %v; the late turn ended at +%s",
		len(busy), len(turns), requestTimes(turns, sent), turnEnd.at.Sub(sent).Round(100*time.Millisecond))
	if stale := r.logsFrom(from, staleResult(pane.token)); len(stale) > 0 {
		r.fails("stale-result", "the daemon dropped %d prompt results of %s at the fence, results of deliveries it no longer held, the first at +%s: %v",
			len(stale), pane.token, stale[0].at.Sub(sent).Round(100*time.Millisecond), recordAttrs(stale))
	}
	r.note("no prompt result of the claim was dropped as stale")
	if len(busy) > 1 {
		r.fails("busy-answers", "the agent answered %d sends of the task busy (%v), and a send after the first busy answer goes into a turn the daemon has been told is running",
			len(busy), busy)
	}
	if view.Pending == nil || view.Pending.ConfirmedAt == nil || !strings.Contains(view.Pending.Task, marker) {
		r.fails("confirmed-in-turn", "while the late turn ran the claim listing read the task %s, and that turn is the task's own",
			describePending(view.Pending, marker))
	}
	if len(turns) != 1 {
		r.fails("one-turn", "%d turns carried the task, want the late one alone: %v", len(turns), requestTimes(turns, sent))
	}
	after := r.claim(pane.token)
	if after.Pending != nil {
		r.fails("retired", "after the late turn ended the claim still holds a task: %s", describePending(after.Pending, marker))
	}
	r.note("one turn carried the task; it ended at +%s, the claim retired the task by that turn's result and has been sent nothing since",
		turnEnd.at.Sub(sent).Round(100*time.Millisecond))
}

// busyOwnTurn is a turn of the agent's own that the daemon did not prompt: the task before it
// starts an `async` bash job and its turn ends, and the job's result starts a turn of Oh My Pi's
// own, which the stand-in holds open. A task queued in that turn is answered busy at most once and
// sent only after the turn ends.
//
// The turn is held at its model request rather than at a recall: at the pin the turn a job's
// result starts runs no recall — a run of the pin made for this proof saw the job's result start
// that turn with no recall asked, where every prompt before it asked one — and its agent_start
// reaches the daemon at once, so the daemon has seen the turn begin and sends the task nothing
// while it runs, which this step reads as zero busy answers.
func (r *busyRelaunchLive) busyOwnTurn() {
	pane := r.pane(r.own)
	r.awaitQuiet(pane.token, 3*time.Minute)
	suffix := randomSuffix(r.t)
	starts, done := "ASYNC-START-"+suffix, "OWN-TURN-"+suffix
	pane.stand.callTool(promptCarrying(starts), "bash", map[string]any{
		"command": fmt.Sprintf("sleep %d; echo %s", busyOwnJobSeconds, done), "async": true,
	})
	own := pane.stand.hold(promptCarrying(done))
	started := time.Now()
	r.deliver(pane.token, "Start the background job. "+starts)
	r.awaitQuiet(pane.token, 2*time.Minute)
	r.note("the task that starts `sleep %d` as an async bash job was answered and retired at +%s",
		busyOwnJobSeconds, time.Since(started).Round(100*time.Millisecond))
	arrived := own.await(r, "the turn the job's result starts", time.Duration(busyOwnJobSeconds)*time.Second+time.Minute)
	r.note("the job's result started a turn of Oh My Pi's own at +%s, held at its model request",
		arrived.Sub(started).Round(100*time.Millisecond))

	marker := "AFTER-OWN-" + suffix
	from := r.daemon.count()
	queued := time.Now()
	r.deliver(pane.token, "Say the word "+marker+" and nothing else.")
	time.Sleep(busyOwnTurnWatch)
	ended := r.daemon.count()
	own.let()
	turnEnd := r.awaitLogFrom("the turn of the agent's own to end", ended, 2*time.Minute, streamTurnEnded(pane.token))
	r.awaitQuiet(pane.token, 3*time.Minute)

	busy := r.busyDeliveries(pane.token, from)
	if len(busy) > 1 {
		r.fails("busy-once", "the agent answered %d sends of the task busy during its own turn (%v), want one at most", len(busy), busy)
	}
	turns := pane.stand.promptsCarrying(marker)
	for _, turn := range turns {
		if !turn.at.After(turnEnd.at) {
			r.fails("sent-after-turn", "the task queued at +0s was put in front of the agent at +%s, inside the turn of its own that ended at +%s",
				turn.at.Sub(queued).Round(100*time.Millisecond), turnEnd.at.Sub(queued).Round(100*time.Millisecond))
		}
	}
	if len(turns) != 1 {
		r.fails("one-turn", "%d turns carried the task, want one after the agent's own turn: %v", len(turns), requestTimes(turns, queued))
	}
	r.note("the task queued in that turn was answered busy %d times (%v) over %s, and went once, at +%s, after the turn ended at +%s",
		len(busy), busy, busyOwnTurnWatch, turns[0].at.Sub(queued).Round(100*time.Millisecond), turnEnd.at.Sub(queued).Round(100*time.Millisecond))
}

// newerTask queues an operator's task (`legion claims deliver`) while a late turn of the claim's
// task runs — a second held recall, on the pane busy-late-turn drove — and holds that it is taken,
// and sent after that turn ends. At this head the late turn confirmed its task, so the newer task
// takes its place (supervise's queue, "the pending task was taken by a turn"); the base confirms
// nothing in a late turn and refuses it `a delivery is already pending` (accepted).
func (r *busyRelaunchLive) newerTask() {
	pane := r.pane(r.late)
	r.awaitQuiet(pane.token, 3*time.Minute)
	suffix := randomSuffix(r.t)
	marker, newer := "LATER-"+suffix, "NEWER-"+suffix
	r.hindsight.holdNext(busyRecallHold)
	late := pane.stand.hold(promptCarrying(marker))
	sent := time.Now()
	r.deliver(pane.token, "Say the word "+marker+" and nothing else.")
	arrived := late.await(r, "the late turn's model request", busyRecallHold+2*time.Minute)
	out, err := r.legionClaimsTry("deliver", "--claim", string(pane.token), "--task", "Say the word "+newer+" and nothing else.")
	r.note("the late turn's model request came at +%s; `legion claims deliver` of a newer task while it was held answered %v:\n%s",
		arrived.Sub(sent).Round(100*time.Millisecond), err, indentOutput(out))
	ended := r.daemon.count()
	late.let()
	turnEnd := r.awaitLogFrom("the late turn's end", ended, 2*time.Minute, streamTurnEnded(pane.token))

	if err != nil || strings.Contains(out, "already pending") {
		r.fails("accepted", "the newer task was refused while the late turn ran (%v): %s", err, out)
	}
	var turns []seenRequest
	r.await("the newer task's turn", 2*time.Minute, func() bool {
		turns = pane.stand.promptsCarrying(newer)
		return len(turns) > 0
	})
	r.awaitQuiet(pane.token, 3*time.Minute)
	turns = pane.stand.promptsCarrying(newer)
	if !turns[0].at.After(turnEnd.at) {
		r.fails("sent-after", "the newer task was put in front of the agent at +%s, inside the late turn that ended at +%s",
			turns[0].at.Sub(sent).Round(100*time.Millisecond), turnEnd.at.Sub(sent).Round(100*time.Millisecond))
	}
	if len(turns) != 1 {
		r.fails("one-turn", "%d turns carried the newer task, want one: %v", len(turns), requestTimes(turns, sent))
	}
	if lates := pane.stand.promptsCarrying(marker); len(lates) != 1 {
		r.fails("one-turn", "%d turns carried the task the late turn served, want that turn alone: %v", len(lates), requestTimes(lates, sent))
	}
	r.note("the late turn ended at +%s; the newer task went once, at +%s, and the claim holds nothing now",
		turnEnd.at.Sub(sent).Round(100*time.Millisecond), turns[0].at.Sub(sent).Round(100*time.Millisecond))
}

// idleControl is the control of the busy steps: a worker in no turn is sent its task at once —
// its turn begins before the daemon's own wait for it runs out — with no busy answer and no
// prompt charged.
func (r *busyRelaunchLive) idleControl() {
	pane := r.pane(r.worker)
	r.awaitQuiet(pane.token, 3*time.Minute)
	marker := "IDLE-" + randomSuffix(r.t)
	from := r.daemon.count()
	sent := time.Now()
	r.deliver(pane.token, "Say the word "+marker+" and nothing else.")
	var turns []seenRequest
	r.await("the idle worker's turn of the task", time.Minute, func() bool {
		turns = pane.stand.promptsCarrying(marker)
		return len(turns) > 0
	})
	r.awaitQuiet(pane.token, 2*time.Minute)
	if waited := turns[0].at.Sub(sent); waited >= r.cfg.WorkerRPCTimeout {
		r.fails("at-once", "the idle worker's turn of the task began %s after the deliver, past the daemon's own %s wait for it",
			waited.Round(100*time.Millisecond), r.cfg.WorkerRPCTimeout)
	}
	if busy := r.busyDeliveries(pane.token, from); len(busy) > 0 {
		r.fails("no-busy", "a worker in no turn answered %d sends busy: %v", len(busy), busy)
	}
	if charged := r.daemon.firstAfter(from, promptCharged(pane.token)); charged != nil {
		r.fails("no-charge", "a worker in no turn was charged a prompt failure: %v", charged.Attrs)
	}
	if turns = pane.stand.promptsCarrying(marker); len(turns) != 1 {
		r.fails("one-turn", "%d turns carried the task, want one: %v", len(turns), requestTimes(turns, sent))
	}
	r.note("the idle worker's turn of the task began %s after the deliver, inside the daemon's %s wait, with no busy answer and nothing charged",
		turns[0].at.Sub(sent).Round(10*time.Millisecond), r.cfg.WorkerRPCTimeout)
}

// relaunchSubagentWorker is a phase worker holding no delivery whose process dies while a `task`
// subagent it started runs. The first prompt its relaunched process is sent carries the sentence
// naming the subagent and the death (sentence), and the claim row holds the record (claim-row).
func (r *busyRelaunchLive) relaunchSubagentWorker() {
	pane := r.pane(r.worker)
	r.awaitQuiet(pane.token, 3*time.Minute)
	r.startsASubagent(pane, "SUBAGENT-WORKER-"+randomSuffix(r.t))
	killed, ready := r.killAndRelaunch(pane)
	prompt := r.firstPromptAfter(pane, killed, ready)
	r.holdsTheSentence(prompt, relaunchDiedWords, relaunchSubagentWords)
	r.note("the first prompt the relaunched process was sent, %s after the death, reads:\n%s",
		prompt.at.Sub(killed).Round(100*time.Millisecond), indentOutput(prompt.last))
	ended := r.claimRowHolds(pane.token, func(e endedWorkRow) bool { return slices.Contains(e.Kinds, "subagent") && !e.Turn })
	r.note("the claim row records %s", ended.describe())
}

// relaunchSubagentArchitect is the same death at a root architect, twice: once holding no
// delivery, and once holding a task in its turn when the process dies. Each time the first prompt
// the relaunched process is sent carries the sentence (sentence), the catch-up notice the daemon
// publishes at that process's ready opens with it (catch-up), and the claim row holds the record
// (claim-row).
func (r *busyRelaunchLive) relaunchSubagentArchitect() {
	pane := r.pane(r.root)
	r.awaitQuiet(pane.token, 3*time.Minute)
	suffix := randomSuffix(r.t)

	told := len(r.architectNotices(pane.issue, "catch-up"))
	r.startsASubagent(pane, "SUBAGENT-ROOT-"+suffix)
	killed, ready := r.killAndRelaunch(pane)
	prompt := r.firstPromptAfter(pane, killed, ready)
	r.holdsTheSentence(prompt, relaunchDiedWords, relaunchSubagentWords)
	r.note("holding no task: the first prompt the relaunched process was sent, %s after the death, reads:\n%s",
		prompt.at.Sub(killed).Round(100*time.Millisecond), indentOutput(prompt.last))
	r.catchUpOpensWithTheSentence(pane, told)
	ended := r.claimRowHolds(pane.token, func(e endedWorkRow) bool { return slices.Contains(e.Kinds, "subagent") && !e.TaskTurn })
	r.note("the claim row records %s", ended.describe())

	r.awaitQuiet(pane.token, 3*time.Minute)
	r.startsASubagent(pane, "SUBAGENT-AGAIN-"+suffix)
	held := "HELD-TASK-" + suffix
	turn := pane.stand.hold(promptCarrying(held))
	r.deliver(pane.token, "Wait for the scratch check, then say the word "+held+".")
	turn.await(r, "the architect's turn of the task it holds", time.Minute)
	r.awaitReported(pane.token, "subagent")
	told = len(r.architectNotices(pane.issue, "catch-up"))
	killed, ready = r.killAndRelaunch(pane)
	prompt = r.firstPromptAfter(pane, killed, ready)
	r.holdsTheSentence(prompt, relaunchCutTaskWords, relaunchDiedWords, relaunchSubagentWords, held)
	r.note("holding a task in its turn: the first prompt the relaunched process was sent, %s after the death, reads:\n%s",
		prompt.at.Sub(killed).Round(100*time.Millisecond), indentOutput(prompt.last))
	r.catchUpOpensWithTheSentence(pane, told)
	ended = r.claimRowHolds(pane.token, func(e endedWorkRow) bool {
		return slices.Contains(e.Kinds, "subagent") && e.Turn && e.TaskTurn
	})
	r.note("the claim row records %s", ended.describe())
}

// relaunchTask is a worker whose process dies in its task's turn with an `async` bash job
// running. The claim row holds the record of the turn and the job (claim-row), and the task is
// sent again behind the sentence naming the turn, the job and the death (sentence).
func (r *busyRelaunchLive) relaunchTask() {
	pane := r.pane(r.task)
	r.awaitQuiet(pane.token, 3*time.Minute)
	marker := "ASYNC-TASK-" + randomSuffix(r.t)
	pane.stand.callTool(promptCarrying(marker), "bash", map[string]any{"command": "sleep 600", "async": true})
	turn := pane.stand.hold(turnContinuing(marker))
	r.deliver(pane.token, "Run the long check in the background, then report on it. "+marker)
	turn.await(r, "the task's turn after its job was backgrounded", 2*time.Minute)
	r.awaitReported(pane.token, "async-job")
	killed, ready := r.killAndRelaunch(pane)
	ended := r.claimRowHolds(pane.token, func(e endedWorkRow) bool {
		return slices.Contains(e.Kinds, "async-job") && e.Turn && e.TaskTurn
	})
	r.note("the claim row records %s", ended.describe())
	prompt := r.firstPromptAfter(pane, killed, ready)
	r.holdsTheSentence(prompt, relaunchCutTaskWords, relaunchDiedWords, relaunchJobWords, marker)
	r.note("the first prompt the relaunched process was sent, %s after the death, is the task behind the sentence:\n%s",
		prompt.at.Sub(killed).Round(100*time.Millisecond), indentOutput(prompt.last))
}

// ---- what a step drives --------------------------------------------------------------------------

// pane is the live claim of issue, and fails the step when issue names none.
func (r *busyRelaunchLive) pane(issue string) *busyPane {
	r.t.Helper()
	for _, pane := range r.panes {
		if pane.issue == issue {
			return pane
		}
	}
	r.t.Fatalf("this proof drives no pane for %s", issue)
	return nil
}

// deliver gives the claim one task of the operator's own, through the route `legion claims
// deliver` posts to.
func (r *busyRelaunchLive) deliver(token claim.Token, task string) {
	r.t.Helper()
	status, body := r.d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/deliver",
		api.DeliverRequest{Task: task}, true)
	if status != http.StatusOK {
		r.t.Fatalf("deliver %q to %s = %d %s, want 200", task, token, status, body)
	}
}

// awaitQuiet waits until the claim holds no task and is ready or idle, and has stayed so for
// liveSettle: an Oh My Pi phase worker's turn can run on past its prompt's result (the plugin's
// phase reminder continues the run once), so a claim read idle once may be read working again.
func (r *busyRelaunchLive) awaitQuiet(token claim.Token, within time.Duration) {
	r.t.Helper()
	var since time.Time
	r.await(string(token)+" to hold no task and run no turn", within, func() bool {
		held := r.claim(token)
		if held.State == string(supervise.StateFailed) {
			r.t.Fatalf("%s failed; its workers' output is under %s", token, r.rt.paneLogs)
		}
		if held.Pending != nil || (held.State != string(supervise.StateReady) && held.State != string(supervise.StateIdle)) {
			since = time.Time{}
			return false
		}
		if since.IsZero() {
			since = time.Now()
		}
		return time.Since(since) >= liveSettle
	})
}

// startsASubagent gives the claim a task whose turn starts a `task` subagent, whose own model
// request the stand-in holds, and lets the turn end, so the claim holds no delivery while the
// subagent runs; it returns once the pane's report of the subagent has reached the store, which
// is what a relaunch reads the record of its background work from.
func (r *busyRelaunchLive) startsASubagent(pane *busyPane, marker string) {
	r.t.Helper()
	pane.stand.callTool(promptCarrying(marker), "task", subagentCall(marker))
	subagent := pane.stand.hold(func(seen seenRequest) bool { return seen.subagent })
	r.deliver(pane.token, "Start a subagent for the scratch check. "+marker)
	subagent.await(r, "the subagent's own model request", 2*time.Minute)
	r.awaitQuiet(pane.token, 2*time.Minute)
	r.awaitReported(pane.token, "subagent")
	r.note("%s's turn started a task subagent and ended; the claim holds no task, and the subagent's own model request is held",
		pane.token)
}

// awaitReported waits for the claim row's activity report — the one the pane posts to
// `/legion/v1/claims/activity` for its current launch — to name kind, which is what the
// relaunch's record is read from (supervise's launchActivity).
func (r *busyRelaunchLive) awaitReported(token claim.Token, kind string) {
	r.t.Helper()
	var kinds string
	r.await(fmt.Sprintf("%s's activity report to name %s", token, kind), relaunchReportWithin, func() bool {
		if err := r.st.Pool().QueryRow(context.Background(),
			"select activity_kinds from claims where token = $1", string(token)).Scan(&kinds); err != nil {
			r.t.Fatalf("read %s's activity report: %v", token, err)
		}
		return slices.Contains(strings.Split(kinds, ","), kind)
	})
	r.note("the claim row's activity report of this launch names %q", kinds)
}

// killAndRelaunch SIGKILLs the claim's process group — the shim, Oh My Pi and every job and
// subagent Oh My Pi started, as a process killed under it dies — and waits for the daemon to
// relaunch it and the new process to report ready. It answers when the process was killed and
// when the relaunched one was read ready.
func (r *busyRelaunchLive) killAndRelaunch(pane *busyPane) (killed, ready time.Time) {
	r.t.Helper()
	was := r.claim(pane.token)
	r.rt.mu.Lock()
	running, ok := r.rt.panes[pane.token]
	r.rt.mu.Unlock()
	if !ok {
		r.t.Fatalf("the runtime runs no process for %s", pane.token)
	}
	killed = time.Now()
	if err := syscall.Kill(-running.pid, syscall.SIGKILL); err != nil {
		r.t.Fatalf("kill %s's process group %d: %v", pane.token, running.pid, err)
	}
	if !waitGone(running, localStopGrace) {
		r.t.Fatalf("%s's process %d did not end after SIGKILL", pane.token, running.pid)
	}
	var now api.OperatorClaim
	r.await(string(pane.token)+" to be relaunched and report ready", paneReady, func() bool {
		now = r.claim(pane.token)
		if now.State == string(supervise.StateFailed) {
			r.t.Fatalf("%s failed after its process was killed; its workers' output is under %s", pane.token, r.rt.paneLogs)
		}
		return now.Generation > was.Generation && (now.State == string(supervise.StateReady) ||
			now.State == string(supervise.StateIdle) || now.State == string(supervise.StateWorking))
	})
	ready = time.Now()
	r.note("killed %s's process group %d (generation %d); the daemon relaunched it as generation %d, ready %s later",
		pane.token, running.pid, was.Generation, now.Generation, ready.Sub(killed).Round(100*time.Millisecond))
	return killed, ready
}

// firstPromptAfter is the first prompt the stand-in received after killed — a role turn's request
// whose last message is the prompt itself — waited for from ready for
// relaunchFirstPromptWithin. A relaunched process that is sent no prompt in that time fails the
// step's sentence assertion: the sentence is what the first prompt was to carry.
func (r *busyRelaunchLive) firstPromptAfter(pane *busyPane, killed, ready time.Time) seenRequest {
	r.t.Helper()
	deadline := ready.Add(relaunchFirstPromptWithin)
	for {
		if prompt, ok := pane.stand.firstPromptAfter(killed); ok {
			return prompt
		}
		if time.Now().After(deadline) {
			r.fails("sentence", "the relaunched process of %s was sent no prompt in the %s after its ready", pane.token, relaunchFirstPromptWithin)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// holdsTheSentence holds that the prompt carries every one of words.
func (r *busyRelaunchLive) holdsTheSentence(prompt seenRequest, words ...string) {
	r.t.Helper()
	for _, said := range words {
		if !strings.Contains(prompt.last, said) {
			r.fails("sentence", "the first prompt the relaunched process was sent does not say %q; it reads:\n%s", said, indentOutput(prompt.last))
		}
	}
}

// catchUpOpensWithTheSentence waits for the catch-up the daemon publishes to the root architect at
// the relaunched process's ready — read at the run's Envoy listener stand-in, which records every
// notice the daemon publishes — and holds that its reason opens with the sentence.
func (r *busyRelaunchLive) catchUpOpensWithTheSentence(pane *busyPane, told int) {
	r.t.Helper()
	var notices []listenerPublish
	r.await(pane.issue+"'s architect's catch-up for the relaunched process", time.Minute, func() bool {
		notices = r.architectNotices(pane.issue, "catch-up")
		return len(notices) > told
	})
	reason := notices[len(notices)-1].Payload
	if !strings.HasPrefix(reason, relaunchCatchUpOpens) || !strings.Contains(reason, relaunchSubagentWords) {
		r.fails("catch-up", "the catch-up published at the relaunched process's ready does not open with the sentence naming the subagent; it reads:\n%s",
			indentOutput(reason))
	}
	r.note("the catch-up published at the relaunched process's ready opens with the sentence:\n%s", indentOutput(firstParagraph(reason)))
}

// claimRowHolds reads the claim row's `ended_work` and holds that it records a death and that
// want holds of it. The read is SQL, so over the base commit — whose schema has no such column —
// the read's own error is this assertion's failure.
func (r *busyRelaunchLive) claimRowHolds(token claim.Token, want func(endedWorkRow) bool) endedWorkRow {
	r.t.Helper()
	ended, err := readEndedWork(r.st, token)
	if err != nil {
		r.fails("claim-row", "read %s's ended_work: %v", token, err)
	}
	if !strings.HasPrefix(ended.Cause, endedWorkDiedCause) || !ended.Reported || !want(ended) {
		r.fails("claim-row", "%s's row records %s", token, ended.describe())
	}
	return ended
}

// awaitListedConfirmed reads the claim through `legion claims list --json`, the built binary
// against the daemon, until the task carrying marker reads confirmed or busyConfirmedWithin runs
// out, and answers the last reading: the step judges it after the turn, so an unconfirmed reading
// fails the assertion that names it rather than this read.
func (r *busyRelaunchLive) awaitListedConfirmed(token claim.Token, marker string) api.OperatorClaim {
	r.t.Helper()
	deadline := time.Now().Add(busyConfirmedWithin)
	for {
		var list api.OperatorClaims
		out := r.legionClaims("list", "--json")
		if err := json.Unmarshal([]byte(out), &list); err != nil {
			r.t.Fatalf("decode `legion claims list --json` %s: %v", out, err)
		}
		var held api.OperatorClaim
		for _, listed := range list.Claims {
			if listed.Token == token {
				held = listed
			}
		}
		if p := held.Pending; (p != nil && p.ConfirmedAt != nil && strings.Contains(p.Task, marker)) || time.Now().After(deadline) {
			return held
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// ---- reads of the daemon's own log ----------------------------------------------------------------

// logsFrom is every record the daemon wrote from the mark from on that matches want.
func (r *busyRelaunchLive) logsFrom(from int, want func(loggedRecord) bool) []loggedRecord {
	r.daemon.mu.Lock()
	defer r.daemon.mu.Unlock()
	var matched []loggedRecord
	for _, kept := range r.daemon.records[from:] {
		if want(kept) {
			matched = append(matched, kept)
		}
	}
	return matched
}

// busyDeliveries is each delivery id the agent answered a send of busy from the mark from on,
// oldest first. One busy answer can be read twice — Oh My Pi answers a prompt it refuses busy
// with a refusal and then a prompt result that ran no turn, and the daemon writes a line for
// each — so the answers are counted by the delivery they name.
func (r *busyRelaunchLive) busyDeliveries(token claim.Token, from int) []string {
	var deliveries []string
	for _, busy := range r.logsFrom(from, busyRefusal(token)) {
		if id := fmt.Sprint(busy.Attrs["delivery"]); !slices.Contains(deliveries, id) {
			deliveries = append(deliveries, id)
		}
	}
	return deliveries
}

// busyRefusal matches the daemon's lines for Oh My Pi's busy answer to a send of the claim's task:
// supervise's refused, lateRefused and promptResulted, at the base and at this head alike, and
// this head's waitedOutAnswer, which reads such an answer to a prompt whose wait for its turn had
// already run out and names Oh My Pi's own words in its why.
func busyRefusal(token claim.Token) func(loggedRecord) bool {
	return func(record loggedRecord) bool {
		if record.Attrs["claim"] != string(token) || !strings.HasPrefix(record.Message, "supervise: ") {
			return false
		}
		return strings.Contains(record.Message, "it is in a turn of its own") ||
			strings.Contains(record.Message, "it is busy with the turn of an earlier prompt of this task") ||
			(strings.HasPrefix(record.Message, "supervise: a prompt whose wait for its turn ran out was answered with no turn") &&
				strings.Contains(fmt.Sprint(record.Attrs["why"]), "Agent is already processing"))
	}
}

// staleResult matches a prompt result of the claim the daemon dropped at its fence (the
// machine's dropStale): the result of a delivery the claim no longer holds.
func staleResult(token claim.Token) func(loggedRecord) bool {
	return func(record loggedRecord) bool {
		return record.Message == "supervise: dropped a stale event" && record.Attrs["claim"] == string(token) &&
			record.Attrs["event"] == "StreamPromptResult"
	}
}

// streamTurnEnded matches the end of a turn of the claim as the worker stream showed it
// (supervise's turnEndedLine). The turn-outcome report the plugin posts is logged under the same
// message with a sequence of its own, and is not this line.
func streamTurnEnded(token claim.Token) func(loggedRecord) bool {
	return func(record loggedRecord) bool {
		_, reported := record.Attrs["sequence"]
		return record.Message == "supervise: a turn ended" && record.Attrs["claim"] == string(token) && !reported
	}
}

// promptCharged matches a prompt failure the claim was charged (supervise's chargePrompt).
func promptCharged(token claim.Token) func(loggedRecord) bool {
	return func(record loggedRecord) bool {
		return record.Message == "supervise: prompt failure" && record.Attrs["claim"] == string(token)
	}
}

// promptWaitRanOut matches the charge of the daemon's wait for a prompt's turn running out
// (supervise's noTurn).
func promptWaitRanOut(token claim.Token) func(loggedRecord) bool {
	return func(record loggedRecord) bool {
		return promptCharged(token)(record) && strings.HasPrefix(fmt.Sprint(record.Attrs["why"]), "acknowledged, and no turn started within")
	}
}

// recordAttrs is each record's attributes, for a failure that names what the daemon wrote.
func recordAttrs(records []loggedRecord) []map[string]any {
	attrs := make([]map[string]any, 0, len(records))
	for _, kept := range records {
		attrs = append(attrs, kept.Attrs)
	}
	return attrs
}

// describePending is a pending task as a step reports it: whether it is the task carrying marker,
// and whether a turn has confirmed it.
func describePending(p *api.DeliveryView, marker string) string {
	switch {
	case p == nil:
		return "absent (the claim holds no task)"
	case !strings.Contains(p.Task, marker):
		return fmt.Sprintf("%s, another task than this step's (%q)", p.ID, p.Task)
	case p.ConfirmedAt == nil:
		return fmt.Sprintf("%s unconfirmed", p.ID)
	}
	return fmt.Sprintf("%s confirmed at %s", p.ID, p.ConfirmedAt.Format(time.RFC3339Nano))
}

// requestTimes is when each request came, from start, for a failure that counts turns.
func requestTimes(requests []seenRequest, start time.Time) []string {
	times := make([]string, 0, len(requests))
	for _, seen := range requests {
		times = append(times, "+"+seen.at.Sub(start).Round(100*time.Millisecond).String())
	}
	return times
}

// firstParagraph is text up to its first blank line: the sentence a catch-up opens with.
func firstParagraph(text string) string {
	paragraph, _, _ := strings.Cut(text, "\n\n")
	return paragraph
}

// ---- the claim row's record of a relaunch ----------------------------------------------------------

// endedWorkRow is the claim row's `ended_work` as this driver reads it: the JSON supervise's
// EndedWork is stored as (supervise/endedwork.go), decoded here field by field so the driver
// names no symbol the base commit lacks.
type endedWorkRow struct {
	At         time.Time `json:"at"`
	Cause      string    `json:"cause"`
	Generation uint64    `json:"generation"`
	Turn       bool      `json:"turn"`
	TaskTurn   bool      `json:"taskTurn"`
	Background bool      `json:"background"`
	Kinds      []string  `json:"kinds"`
	Reported   bool      `json:"reported"`
	Told       bool      `json:"told"`
}

func (e endedWorkRow) describe() string {
	return fmt.Sprintf("cause %q, launch %d, turn %t, task's turn %t, background %t, kinds %v, reported %t, told %t",
		e.Cause, e.Generation, e.Turn, e.TaskTurn, e.Background, e.Kinds, e.Reported, e.Told)
}

// readEndedWork reads the claim row's `ended_work`. A row that records nothing reads as the zero
// record, which no assertion of a relaunch holds.
func readEndedWork(st *store.Store, token claim.Token) (endedWorkRow, error) {
	var raw *string
	if err := st.Pool().QueryRow(context.Background(),
		"select ended_work::text from claims where token = $1", string(token)).Scan(&raw); err != nil {
		return endedWorkRow{}, err
	}
	var ended endedWorkRow
	if raw == nil {
		return ended, nil
	}
	if err := json.Unmarshal([]byte(*raw), &ended); err != nil {
		return endedWorkRow{}, fmt.Errorf("decode %s: %w", *raw, err)
	}
	return ended, nil
}

// ---- the Oh My Pi profile and the overlays -----------------------------------------------------------

// writeOmpProfile writes the profile every pane of this proof shares: the five stand-in
// providers, one model each, and the settings a pane needs to run unattended.
func (r *busyRelaunchLive) writeOmpProfile() {
	t := r.t
	t.Helper()
	agent := filepath.Join(r.home, ".omp", "profiles", r.profile, "agent")
	if err := os.MkdirAll(agent, 0o700); err != nil {
		t.Fatalf("make the profile's agent directory %s: %v", agent, err)
	}
	var models, enabled strings.Builder
	models.WriteString("# Written by packages/daemon/internal/daemon/busy_relaunch_live_test.go.\nproviders:\n")
	for _, pane := range r.panes {
		fmt.Fprintf(&models, "  %s:\n    baseUrl: %s\n    api: anthropic-messages\n    apiKey: a-literal-key-no-endpoint-checks\n"+
			"    models:\n      - id: %s\n        name: %s\n        contextWindow: 200000\n        maxTokens: 8192\n",
			pane.provider, pane.stand.url, pane.provider+standinModelID, pane.provider)
		fmt.Fprintf(&enabled, "  - %s\n", standinModel(pane.provider))
	}
	writeProofFile(t, filepath.Join(agent, "models.yml"), models.String())
	writeProofFile(t, filepath.Join(agent, "config.yml"),
		"# Written by packages/daemon/internal/daemon/busy_relaunch_live_test.go.\n"+
			"enabledModels:\n"+enabled.String()+
			// A fresh profile opens Oh My Pi's setup wizard, and a pane waiting on it never starts.
			"startup:\n  setupWizard: false\n")
}

// writeOverlays writes the Oh My Pi overlays each pane is told through PI_CONFIG_FILES, in the
// order a deployment composes them (runtime/tmux's PaneVariables): the operator's own, which turns
// `retry.enabled` off so the host walks no chain of its own; the claim's own (writeRoleOverlay),
// the one model its stand-in serves for every model role, `deep` — the model of the deep-worker
// agent a step's subagent runs — among them; and for the late pane, the memory overlay a
// deployment renders when Hindsight is on (dispatch://INFRA-3328): the backend, the run's
// stand-in as its API, and the two features that would call it beside the recall — the mental
// models read at the session's start and the retain after a turn — off, so the recall is the one
// call it is asked.
func (r *busyRelaunchLive) writeOverlays(work string) map[claim.Token][]string {
	t := r.t
	t.Helper()
	dir := filepath.Join(work, "overlays")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	operator := filepath.Join(dir, "operator.yml")
	writeProofFile(t, operator,
		"# The operator's pane overlay (runtime.tmux.pi_config_files), which every pane names first.\nretry:\n  enabled: false\n")
	overlays := map[claim.Token][]string{}
	for _, pane := range r.panes {
		role := filepath.Join(dir, pane.provider+".yml")
		writeRoleOverlay(t, role, standinModel(pane.provider))
		overlays[pane.token] = []string{operator, role}
		if !pane.recalls {
			continue
		}
		memory := filepath.Join(dir, pane.provider+"-memory.yml")
		writeProofFile(t, memory, fmt.Sprintf("# The pane's memory overlay: Hindsight, at the run's own stand-in.\n"+
			"memory:\n  backend: hindsight\nhindsight:\n  apiUrl: %s\n  mentalModelsEnabled: false\n  autoRetain: false\n", r.hindsight.url))
		overlays[pane.token] = append(overlays[pane.token], memory)
	}
	return overlays
}

// subagentCall is the `task` tool call a turn answers to start one deep-worker subagent, in the
// shape the plugins' own pin test calls it (packages/pi-legion/extensions/
// legion-session-activity-omp.test.ts), with marker in its context so the subagent's own request
// names the step that started it.
func subagentCall(marker string) map[string]any {
	return map[string]any{
		"context": "The scratch check of this proof, " + marker + ".",
		"tasks": []map[string]any{{
			"name": "DeepWorker", "agent": "deep-worker",
			"task":          "Run the scratch check in the workspace and report what it printed.",
			"solutionSpace": "one check: the command is given",
		}},
	}
}

// ---- the pane's Envoy listener -----------------------------------------------------------------------

// newConsumerPaneListener is the Envoy listener a pane is told through ENVOY_URL: newPaneListener's
// answer (live_agent_proof_test.go) — the session, a machine, the caller's directory and no topics,
// for every path — with the one thing a registration has needed since cybermonklabs/legion#2603
// (dispatch://INFRA-771), a consumer of the session's own. A session receives only what its own
// consumer holds, so pi-envoy's client refuses a `POST /v1/interests/subscribe` answered with no
// `delivery` (packages/envoy-client/src/transport.ts, parseSubscribeAnswer: "the listener answered
// no delivery for <session>: it predates session consumers"), and pi-legion's claim boot then ends
// the session so the daemon launches it again — which this proof's first run read on every pane,
// three launches each, until the third death failed the claim. The answer is the one every stand-in
// listener of the plugins builds (packages/envoy-client/test/subscribe-answer.ts,
// consumerDelivery): consumer mode on the session's own deliver subject
// (`notifications.envoy.deliver.<session>`, consumer-delivery.ts's deliverSubject), with no gap. No
// frame is ever relayed on it: nothing this proof reads reaches a pane through Envoy.
//
// newPaneListener answers the same since this change, which is what the other live proofs register
// through. This copy is the driver's own all the same, because the driver is built over the base
// commit too, whose newPaneListener answers no consumer.
func newConsumerPaneListener(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var sent struct {
			SessionID string `json:"session_id"`
			Dir       string `json:"dir"`
		}
		if body, err := io.ReadAll(req.Body); err == nil {
			_ = json.Unmarshal(body, &sent)
		}
		dir := sent.Dir
		if dir == "" {
			dir = "/"
		}
		answer := map[string]any{"session_id": sent.SessionID, "machine_id": "busy-relaunch-proof", "dir": dir, "topics": []string{}}
		if req.URL.Path == "/v1/interests/subscribe" {
			answer["delivery"] = map[string]any{
				"mode": "consumer", "subject": "notifications.envoy.deliver." + sent.SessionID,
				"stream_id": "busy-relaunch-proof", "gap": false,
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(answer)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// ---- the scripted model stand-in ---------------------------------------------------------------------

// scriptedStandin is one pane's provider endpoint: paneStandin, which answers every turn alike and
// answers the plugin's side question and a compaction's summary apart, behind the answers a step
// scripts in front of it — a `tool_use` answer, or a request held until the step lets it go. It
// keeps every Messages request it was sent, scripted or not, which is what a step reads back.
type scriptedStandin struct {
	*paneStandin

	mu      sync.Mutex
	rules   []*standinRule
	seen    []seenRequest
	calls   int
	stopped chan struct{}
}

// standinRule is one scripted answer: the request it takes — the first one it matches, and no
// other — and what it does with it.
type standinRule struct {
	match func(seenRequest) bool
	spent bool
	// call, when set, is the tool the request is answered with a call of.
	call *toolCall
	// hold, when set, holds the request until the step lets it go.
	hold *heldRequest
}

// toolCall is one `tool_use` block: the tool and its input.
type toolCall struct {
	name  string
	input map[string]any
}

// seenRequest is one Messages request as a step reads it back.
type seenRequest struct {
	at time.Time
	// last is the text of the request's last message, a tool result's included, and prompt
	// whether that message is a user's text — the prompt of a turn, or what a turn of Oh My Pi's
	// own was started on — rather than a tool's result alone.
	last   string
	prompt bool
	// conversation is the text of every message of the request.
	conversation string
	// side is the plugin's own side question (isSideQuestion), and subagent a `task` subagent's
	// own request (deepWorkerSystemLine): neither is a turn of the role's.
	side, subagent bool
}

func newScriptedStandin(t *testing.T, name string) *scriptedStandin {
	t.Helper()
	s := &scriptedStandin{paneStandin: &paneStandin{modelStandin: &modelStandin{name: name}}, stopped: make(chan struct{})}
	s.serve(t, s)
	// Registered after the server's own close, so it runs first: a request still held when the
	// run ends is let go, and the server's close does not wait on it.
	t.Cleanup(func() { close(s.stopped) })
	return s
}

func (s *scriptedStandin) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/v1/messages") {
		s.paneStandin.ServeHTTP(w, req)
		return
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	seen := readStandinRequest(body, time.Now())
	s.mu.Lock()
	s.seen = append(s.seen, seen)
	rule := s.ruleFor(seen)
	s.calls++
	id := fmt.Sprintf("toolu_%s_%d", s.name, s.calls)
	s.mu.Unlock()
	if rule != nil && rule.hold != nil && !rule.hold.take(req.Context(), s.stopped) {
		return
	}
	if rule != nil && rule.call != nil {
		streamAnthropicToolCall(w, s.name, id, rule.call.name, rule.call.input)
		return
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	s.paneStandin.ServeHTTP(w, req)
}

// ruleFor is the first rule that takes seen, which is spent by it. A side question is never
// scripted: paneStandin answers it as the plugins' pane harness does.
func (s *scriptedStandin) ruleFor(seen seenRequest) *standinRule {
	if seen.side {
		return nil
	}
	for _, rule := range s.rules {
		if rule.spent || !rule.match(seen) {
			continue
		}
		rule.spent = true
		return rule
	}
	return nil
}

// callTool answers the first request match takes with a call of the tool name, input with the
// one-line intent every Oh My Pi tool requires.
func (s *scriptedStandin) callTool(match func(seenRequest) bool, name string, input map[string]any) {
	called := map[string]any{"i": "Running the proof's step"}
	for key, value := range input {
		called[key] = value
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules = append(s.rules, &standinRule{match: match, call: &toolCall{name: name, input: called}})
}

// hold holds the first request match takes until the step lets it go, or until the process that
// asked dies, which is how a subagent is kept alive until its process is killed.
func (s *scriptedStandin) hold(match func(seenRequest) bool) *heldRequest {
	held := &heldRequest{arrived: make(chan struct{}), release: make(chan struct{})}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules = append(s.rules, &standinRule{match: match, hold: held})
	return held
}

// promptsCarrying is every request whose prompt carries marker: each one a turn the prompt began,
// since a later request of the same turn ends in a tool's result and one of a later turn ends in
// that turn's own prompt.
func (s *scriptedStandin) promptsCarrying(marker string) []seenRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	var carrying []seenRequest
	for _, seen := range s.seen {
		if promptCarrying(marker)(seen) {
			carrying = append(carrying, seen)
		}
	}
	return carrying
}

// firstPromptAfter is the first role turn's prompt the stand-in received after at.
func (s *scriptedStandin) firstPromptAfter(at time.Time) (seenRequest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, seen := range s.seen {
		if seen.at.After(at) && seen.prompt && !seen.side && !seen.subagent {
			return seen, true
		}
	}
	return seenRequest{}, false
}

// promptCarrying takes a role turn's request whose prompt carries marker.
func promptCarrying(marker string) func(seenRequest) bool {
	return func(seen seenRequest) bool {
		return seen.prompt && !seen.side && !seen.subagent && strings.Contains(seen.last, marker)
	}
}

// turnContinuing takes a role turn's request that goes on with the turn a prompt carrying marker
// began: its last message is a tool's result.
func turnContinuing(marker string) func(seenRequest) bool {
	return func(seen seenRequest) bool {
		return !seen.prompt && !seen.side && !seen.subagent && strings.Contains(seen.conversation, marker)
	}
}

// heldRequest is the one request a hold takes: arrived closes when the stand-in takes it, and let
// lets it go.
type heldRequest struct {
	arrived chan struct{}
	release chan struct{}
	once    sync.Once
	at      time.Time
}

// take holds the request until the step lets it go, the asker goes away, or the run ends, and
// answers whether the request is still to be answered.
func (h *heldRequest) take(asked context.Context, stopped <-chan struct{}) bool {
	h.at = time.Now()
	close(h.arrived)
	select {
	case <-h.release:
		return true
	case <-stopped:
		return true
	case <-asked.Done():
		return false
	}
}

// await waits for the held request, failing the step with what it waited for, and answers when
// it came.
func (h *heldRequest) await(r *busyRelaunchLive, what string, within time.Duration) time.Time {
	r.t.Helper()
	select {
	case <-h.arrived:
		return h.at
	case <-time.After(within):
		r.t.Fatalf("timed out after %s waiting for %s", within, what)
		return time.Time{}
	}
}

// let lets the held request go, or the one the hold will take answer at once.
func (h *heldRequest) let() {
	h.once.Do(func() { close(h.release) })
}

// readStandinRequest reads a Messages request's body as a step reads it back.
func readStandinRequest(body []byte, at time.Time) seenRequest {
	seen := seenRequest{at: at, side: isSideQuestion(string(body))}
	var request struct {
		System   json.RawMessage `json:"system"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &request) != nil || len(request.Messages) == 0 {
		return seen
	}
	seen.subagent = strings.Contains(string(request.System), deepWorkerSystemLine)
	var conversation strings.Builder
	for _, message := range request.Messages {
		text, _ := contentText(message.Content)
		conversation.WriteString(text)
	}
	seen.conversation = conversation.String()
	last := request.Messages[len(request.Messages)-1]
	text, prompted := contentText(last.Content)
	seen.last, seen.prompt = text, prompted && last.Role == "user"
	return seen
}

// contentText is a message's text — its text blocks and its tools' results, each on a line — and
// whether any of it is a text block rather than a tool's result.
func contentText(content json.RawMessage) (string, bool) {
	var plain string
	if json.Unmarshal(content, &plain) == nil {
		return plain, true
	}
	var blocks []struct {
		Type    string          `json:"type"`
		Text    string          `json:"text"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return "", false
	}
	var text strings.Builder
	prompted := false
	for _, block := range blocks {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
			prompted = true
		case "tool_result":
			result, _ := contentText(block.Content)
			text.WriteString(result)
		}
		text.WriteString("\n")
	}
	return text.String(), prompted
}

// streamAnthropicToolCall answers one Anthropic Messages request with a single `tool_use` block,
// in the events the plugins' own pane harness writes for one (packages/pi-shared/test/
// omp-harness.ts, messageStream): the block opened with an empty input, the input as one
// `input_json_delta`, and a `tool_use` stop.
func streamAnthropicToolCall(w http.ResponseWriter, provider, id, name string, input map[string]any) {
	arguments, err := json.Marshal(input)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	event := func(name, data string) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
		if flusher != nil {
			flusher.Flush()
		}
	}
	event("message_start", fmt.Sprintf(`{"type":"message_start","message":{"id":%q,"type":"message","role":"assistant",`+
		`"model":%q,"content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1000,"output_tokens":1}}}`,
		"msg_"+id, provider+standinModelID))
	event("content_block_start", fmt.Sprintf(
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":%q,"name":%q,"input":{}}}`, id, name))
	event("content_block_delta", fmt.Sprintf(
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":%q}}`, arguments))
	event("content_block_stop", `{"type":"content_block_stop","index":0}`)
	event("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":16}}`)
	event("message_stop", `{"type":"message_stop"}`)
}

// ---- the Hindsight stand-in --------------------------------------------------------------------------

// hindsightStandin is the memory backend the late pane's overlay names (writeOverlays): Oh My
// Pi's Hindsight client (hindsight/client.ts at the pin) asks it
// `POST /v1/default/banks/<bank>/memories/recall` before each prompt's turn, and nothing else at
// the overlay's settings. Every recall is answered 503, after the hold a step armed for the next
// one (holdNext) or at once: a failed recall commits nothing to the session, so the next prompt
// recalls again and a later step can hold that one too.
type hindsightStandin struct {
	url string

	mu      sync.Mutex
	hold    time.Duration
	recalls []hindsightRecall
}

// hindsightRecall is one recall the stand-in was asked: the prompt it was asked for, when it came,
// how long it was held and when it was answered.
type hindsightRecall struct {
	query    string
	at       time.Time
	held     time.Duration
	answered time.Time
}

func (h hindsightRecall) describe(asked bool) string {
	if !asked {
		return "no recall carrying the task"
	}
	return fmt.Sprintf("a recall held %s, answered at %s", h.held, h.answered.Format(time.RFC3339Nano))
}

func newHindsightStandin(t *testing.T) *hindsightStandin {
	t.Helper()
	h := &hindsightStandin{}
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	h.url = server.URL
	return h
}

func (h *hindsightStandin) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/memories/recall") {
		// No other call reaches here with mental models and the retain off; one that did is
		// answered empty rather than failed, so it changes no turn.
		_, _ = io.WriteString(w, `{"items":[],"results":[]}`)
		return
	}
	body, _ := io.ReadAll(req.Body)
	var asked struct {
		Query string `json:"query"`
	}
	_ = json.Unmarshal(body, &asked)
	h.mu.Lock()
	held := h.hold
	h.hold = 0
	index := len(h.recalls)
	h.recalls = append(h.recalls, hindsightRecall{query: asked.Query, at: time.Now(), held: held})
	h.mu.Unlock()
	if held > 0 {
		select {
		case <-time.After(held):
		case <-req.Context().Done():
			return
		}
	}
	h.mu.Lock()
	h.recalls[index].answered = time.Now()
	h.mu.Unlock()
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = io.WriteString(w, `{"detail":"this stand-in keeps no memories"}`)
}

// holdNext holds the next recall the stand-in is asked for held before its answer.
func (h *hindsightStandin) holdNext(held time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hold = held
}

// recallCarrying is the recall asked for the prompt carrying marker, and whether there was one.
func (h *hindsightStandin) recallCarrying(marker string) (hindsightRecall, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, recall := range h.recalls {
		if strings.Contains(recall.query, marker) {
			return recall, true
		}
	}
	return hindsightRecall{}, false
}
