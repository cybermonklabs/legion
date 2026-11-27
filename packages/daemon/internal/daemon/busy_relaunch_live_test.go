//go:build e2e

// The live proof of dispatch://INFRA-3276 R3 on the binary that has it, carried onto the take
// (dispatch://INFRA-2811): a relaunched process reads, in the first prompt it runs — the text of
// the first take its plugin had accepted — what the relaunch ended of its subagents, its
// background jobs and its turn. scripts/e2e/busy-relaunch-delivery.sh builds this driver with
// `-tags e2e` and owns the nats-server and the PostgreSQL it runs against; with `--base-commit` it
// lays this one file over a `git archive` of that commit and runs it there, which is the proof's
// negative control. INFRA-3276's R4 steps, which measured how a worker in a turn was sent its
// task, went with the prompt frame: a worker takes its task while its host is idle, and a take
// into a turn is refused. The driver's test keeps its name.
//
// It is a sibling of claim_compaction_live_test.go and provider_error_retry_live_test.go, and it
// takes what they share as it is: liveAgentProof's reads of a claim and of the daemon's own log
// (live_agent_proof_test.go), the run's own database, `legion` and the two plugins built from the
// checkout, paneStandin (claim_compaction_live_test.go), which answers every turn alike and tells
// the plugin's side question and a compaction's summary apart, and the local runtime that starts a
// pane where there is no tmux (localruntime_live_test.go). Every stand-in this proof adds is in
// this file. The list below names those stand-ins and the shared helpers the proof calls besides,
// which the negative control's base commit must hold, the task numbers of the take among them
// (supervise.Claim.TaskWritten, supervise.Delivery.Number):
//
//   - a scripted answer in front of paneStandin (scriptedStandin): a `tool_use` answer, which the
//     live proofs' shared streamAnthropicToolUse writes (live_agent_proof_test.go) as the plugins'
//     own pane harness scripts one (packages/pi-shared/test/omp-harness.ts, messageStream), so a
//     turn starts a `task` subagent or an `async` bash job, and a held answer, so a turn or a
//     subagent's own request stays open until the process that asked dies.
//   - the Envoy listener a pane registers with: the live proofs' shared newPaneListener
//     (live_agent_proof_test.go), which answers that registration with a consumer of the
//     session's own, as a session has needed since cybermonklabs/legion#2603, and relays each
//     claim's task value to the session that claimed its role.
//
// Each step runs as a subtest of its own (run), so a step that fails ends there and the next
// step still runs: the negative control's one run shows every step's own failure, each at the
// assertion the step names (fails). Each step drives a claim no earlier step left in a state of
// its own, and waits for it to hold no task and run no turn before it drives it (awaitQuiet), so
// a step that failed leaves the next one a claim it can read.
//
// What a step reads of the relaunch it reads where no new Go symbol is needed: the words of the
// sentence as literals (relaunchDiedWords and its siblings), and the claim row's `ended_work`
// through SQL (readEndedWork), so over a base commit whose schema has no such column, the read
// itself is the failure of the assertion that reads it rather than a driver that does not build.
package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
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
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// relaunchFirstPromptWithin is how long a relaunch step waits, from the relaunched process's
// ready, for the first prompt that process runs. At this head the relaunched process's plugin
// takes a task at the ready (supervise's queueRelaunch, or the held task itself); the bound is
// what lets the base, which gives a root architect holding nothing no task at all, fail the step
// rather than the run.
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
	// relaunchNoTurnReportWords and relaunchTurnGoneWords are what the sentence of a turn cut off
	// says when no report covers that turn: the daemon has none, so its subagents and background
	// jobs are gone (supervise's relaunchSentence).
	relaunchNoTurnReportWords = "The daemon has no report of the background work of that turn."
	relaunchTurnGoneWords     = "So the subagents and background jobs of that turn are gone."
	// endedWorkDiedCause opens the cause the claim row records for a death (endDied).
	endedWorkDiedCause = "the process died: "
)

// The three panes, by the provider name each one's stand-in is given in the run's models.yml.
const (
	relaunchWorkerProvider = "workerpane"
	relaunchRootProvider   = "architectpane"
	relaunchTaskProvider   = "taskpane"
)

// busyRelaunchLive is one run of the proof: the booted daemon and its three panes.
type busyRelaunchLive struct {
	liveAgentProof
	// panes is the three live claims, in the order they are seeded.
	panes []*busyPane
	// worker, root and task are the three issues, one pane each and each its own tree.
	worker, root, task string
	// home and profile are the Oh My Pi home and profile every pane runs under.
	home, profile string
	// overlays is each claim's PI_CONFIG_FILES, in order.
	overlays map[claim.Token][]string
	// failedAt is the assertion the running step failed at (fails), which run prints.
	failedAt string
}

// busyPane is one live claim of the proof: the issue it works, the phase that issue stands in,
// the role that works it, and the stand-in its Oh My Pi talks to.
type busyPane struct {
	issue    string
	title    string
	at       phase.Phase
	role     claim.Role
	provider string
	token    claim.Token
	stand    *scriptedStandin
}

// TestARelaunchSaysWhatItEndedLive drives the three relaunch steps, each on a claim of its own.
// scripts/e2e/busy-relaunch-delivery.sh runs it.
func TestARelaunchSaysWhatItEndedLive(t *testing.T) {
	// Not parallel: it gives the process back the operator's own environment, as t.Setenv does (fixtureenv.Live).
	fixtureenv.Live(t)
	r := newBusyRelaunchLive(t)
	r.run("relaunch-subagent-worker", r.relaunchSubagentWorker)
	r.run("relaunch-subagent-architect", r.relaunchSubagentArchitect)
	r.run("relaunch-task", r.relaunchTask)
}

// newBusyRelaunchLive readies everything a pane needs before the daemon boots — the `legion`
// binary and the two plugins built from the checkout, an Oh My Pi profile of the run's own routed
// to the three stand-ins, the issues seeded with their worker mid-launch — boots the daemon over
// the local runtime, and waits for every pane to take and answer what its seed left pending.
func newBusyRelaunchLive(t *testing.T) *busyRelaunchLive {
	t.Helper()
	omp := os.Getenv(liveOmpPathEnv)
	if omp == "" {
		t.Skipf("%s is unset, so there is no Oh My Pi for a pane to run; scripts/e2e/busy-relaunch-delivery.sh requires it", liveOmpPathEnv)
	}
	proof := newLiveProof(t, "scripts/e2e/busy-relaunch-delivery.sh")
	key := func(n int) string { return fmt.Sprintf("%s-%d", proof.cfg.Project, n) }
	r := &busyRelaunchLive{
		liveAgentProof: liveAgentProof{liveProof: proof, omp: omp},
		worker:         key(3), root: key(4), task: key(5),
	}
	r.panes = []*busyPane{
		{issue: r.worker, title: "the issue whose planner dies with a subagent running", at: phase.Planning,
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
	// Three trees hold a slot each, which is one over the fixture's cap (testConfig): admission
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
	for _, pane := range r.panes {
		pane.stand = newScriptedStandin(t, pane.provider)
	}
	r.writeOmpProfile()
	r.overlays = r.writeOverlays(work)

	r.rt = newLocalRuntime(t, r.cfg.StateDir, r.legion, omp, r.home, r.profile)
	r.rt.daemonURL, r.rt.envoyURL, r.rt.natsURLs = r.cfg.DaemonURL, newPaneListener(t, r.cfg.NatsURLs), r.cfg.NatsURLs
	r.rt.overlays = func(token claim.Token) []string { return r.overlays[token] }

	r.seed()
	r.daemon = newRecordingLog(r.log())
	r.d = startDaemonLogging(t, r.cfg, r.overrides(), slog.New(r.daemon))
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

// overrides is the daemon this proof boots: the local runtime's own wiring (daemonOverrides), and
// the workflow's App tokens stood in for. The clock is the real one.
func (r *busyRelaunchLive) overrides() overrides {
	o := r.rt.daemonOverrides(r.rt)
	o.workflowTokens = &workflowTokenRecorder{}
	return o
}

// seed records the three issues as the daemon would hold them at the moment each one's worker
// began, as claim_compaction_live_test.go's seed does and for its reasons: each a root handed to
// Legion, in the phase its worker works, with its lifecycle open, an admission slot the seed
// writes (liveProof.admit), and that worker's claim mid-launch, which this boot relaunches. Each
// phase worker's claim holds the task its phase's start left pending, which is what keeps
// admission's repair of a slotted worker holding no task off it (internal/admit/admit.go's
// resumeSlottedWorkers) — that repair would give the claim its phase's resume task, and every
// deliver of this driver would be refused `a delivery is already pending` — and every pane
// answers it before the first step. The root architect's issue stands in accepting, where no
// phase worker runs, so the repair passes it over as it passes over every architect; its claim
// holds nothing, as an architect's start leaves it. Its slot is the one a daemon would not hold:
// accepting keeps none (record.KeepsSlot), so admission's release takes it back (admit's
// releaseSlots).
func (r *busyRelaunchLive) seed() {
	t := r.t
	t.Helper()
	ctx := context.Background()
	for _, pane := range r.panes {
		r.board.show(pane.issue, "in_progress")
	}
	if err := store.Transact(ctx, r.st.Pool(), func(ctx context.Context, tx pgx.Tx) error {
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
		written := uint64(1)
		if pane.role == claim.RoleArchitect {
			written = 0
		}
		if _, err := r.st.AdmitClaim(ctx, supervise.Claim{
			Token: pane.token, Project: r.project, Tree: pane.issue, Issue: pane.issue, Role: pane.role,
			Generation: 1, State: supervise.StateLaunching, TaskWritten: written,
			BootTokenHash: supervise.HashBootToken("seeded-" + randomSuffix(t)), ServingGeneration: 1,
		}); err != nil {
			t.Fatalf("record the claim %s: %v", pane.token, err)
		}
		writePrompt(t, r.cfg, pane.token)
		if pane.role == claim.RoleArchitect {
			continue
		}
		// Untaken: the launch this seed interrupts never had the task taken, so the relaunched pane
		// takes it once it reports ready. It is numbered as a claim's first task is queued
		// (Claim.TaskWritten, Delivery.Number), so the boot writes its value to the task bucket.
		if err := r.st.PutDelivery(ctx, pane.token, supervise.Delivery{
			ID: "seeded-start-" + randomSuffix(t), Task: startTask(pane.role), Generation: 1,
			Phase: pane.at, QueuedAt: time.Now(), Number: 1, Origin: supervise.OriginPhaseStart,
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

// relaunchSubagentWorker is a phase worker holding no delivery whose process dies while a `task`
// subagent it started runs. The record shows that running work, so the relaunch's ready queues
// the daemon's own delivery of the sentence (supervise's queueRelaunch), and the first prompt the
// relaunched process runs, the text of its first accepted take, carries the sentence naming the
// subagent and the death (sentence); the claim row holds the record (claim-row).
func (r *busyRelaunchLive) relaunchSubagentWorker() {
	pane := r.pane(r.worker)
	r.awaitQuiet(pane.token, 3*time.Minute)
	r.startsASubagent(pane, "SUBAGENT-WORKER-"+randomSuffix(r.t))
	killed, ready := r.killAndRelaunch(pane)
	prompt := r.firstPromptAfter(pane, killed, ready)
	r.holdsTheSentence(prompt, relaunchDiedWords, relaunchSubagentWords)
	r.note("the first prompt the relaunched process ran, %s after the death, reads:\n%s",
		prompt.at.Sub(killed).Round(100*time.Millisecond), indentOutput(prompt.last))
	ended := r.claimRowHolds(pane.token, func(e endedWorkRow) bool {
		return slices.Contains(e.Kinds, "subagent") && !e.Turn && e.Reported
	})
	r.note("the claim row records %s", ended.describe())
}

// relaunchSubagentArchitect is the same death at a root architect, twice: once holding no
// delivery, and once holding a task in its turn when the process dies. Each time the first prompt
// the relaunched process runs carries the sentence (sentence) — the daemon's own delivery of it,
// and then the task taken back from its turn behind it — the catch-up notice the daemon publishes
// at that process's ready opens with it (catch-up), and the claim row holds the record
// (claim-row). The second time the subagent was started in the turn before the task's, and Oh My
// Pi reports only when what runs changes and at a turn's end (pi-legion's legion.ts), so the
// newest report, which names the subagent, is older than the task's turn: the row keeps the
// subagent and records no report of that turn (supervise's recordEnd), and the sentence says that
// the daemon has no report of that turn and that its subagents and background jobs are gone.
func (r *busyRelaunchLive) relaunchSubagentArchitect() {
	pane := r.pane(r.root)
	r.awaitQuiet(pane.token, 3*time.Minute)
	suffix := randomSuffix(r.t)

	told := len(r.architectNotices(pane.issue, "catch-up"))
	r.startsASubagent(pane, "SUBAGENT-ROOT-"+suffix)
	killed, ready := r.killAndRelaunch(pane)
	prompt := r.firstPromptAfter(pane, killed, ready)
	r.holdsTheSentence(prompt, relaunchDiedWords, relaunchSubagentWords)
	r.note("holding no task: the first prompt the relaunched process ran, %s after the death, reads:\n%s",
		prompt.at.Sub(killed).Round(100*time.Millisecond), indentOutput(prompt.last))
	r.catchUpOpensWithTheSentence(pane, told)
	ended := r.claimRowHolds(pane.token, func(e endedWorkRow) bool {
		return slices.Contains(e.Kinds, "subagent") && !e.TaskTurn && e.Reported
	})
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
	r.holdsTheSentence(prompt, relaunchCutTaskWords, relaunchDiedWords, relaunchSubagentWords,
		relaunchNoTurnReportWords, relaunchTurnGoneWords, held)
	r.note("holding a task in its turn: the first prompt the relaunched process ran, %s after the death, reads:\n%s",
		prompt.at.Sub(killed).Round(100*time.Millisecond), indentOutput(prompt.last))
	r.catchUpOpensWithTheSentence(pane, told)
	ended = r.claimRowHolds(pane.token, func(e endedWorkRow) bool {
		return slices.Contains(e.Kinds, "subagent") && e.Turn && e.TaskTurn && !e.Reported
	})
	r.note("the claim row records %s", ended.describe())
}

// relaunchTask is a worker whose process dies in its task's turn with an `async` bash job
// running. The claim row holds the record of the turn and the job (claim-row), and the task is
// taken again behind the sentence naming the turn, the job and the death (sentence).
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
		return slices.Contains(e.Kinds, "async-job") && e.Turn && e.TaskTurn && e.Reported
	})
	r.note("the claim row records %s", ended.describe())
	prompt := r.firstPromptAfter(pane, killed, ready)
	r.holdsTheSentence(prompt, relaunchCutTaskWords, relaunchDiedWords, relaunchJobWords, marker)
	r.note("the first prompt the relaunched process ran, %s after the death, is the task behind the sentence:\n%s",
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
// deliver` posts to, which writes the claim's value for its plugin to take.
func (r *busyRelaunchLive) deliver(token claim.Token, task string) {
	r.t.Helper()
	status, body := r.d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/deliver",
		api.DeliverRequest{Task: task}, true)
	if status != http.StatusOK {
		r.t.Fatalf("deliver %q to %s = %d %s, want 200", task, token, status, body)
	}
}

// awaitQuiet waits until the claim holds no task and is ready or idle, and has stayed so for
// liveSettle: an Oh My Pi phase worker's turn can run on past the report that finished its task
// (the plugin's phase reminder continues the run once), so a claim read idle once may be read
// working again.
func (r *busyRelaunchLive) awaitQuiet(token claim.Token, within time.Duration) {
	r.t.Helper()
	if !r.quietWithin(token, within) {
		r.t.Fatalf("timed out after %s waiting for %s to hold no task and run no turn", within, token)
	}
}

// quietWithin is awaitQuiet's wait, answering whether the claim settled within the bound.
func (r *busyRelaunchLive) quietWithin(token claim.Token, within time.Duration) bool {
	r.t.Helper()
	deadline := time.Now().Add(within)
	var since time.Time
	for {
		held := r.claim(token)
		if held.State == string(supervise.StateFailed) {
			r.t.Fatalf("%s failed; its workers' output is under %s", token, r.rt.paneLogs)
		}
		switch {
		case held.Pending != nil || (held.State != string(supervise.StateReady) && held.State != string(supervise.StateIdle)):
			since = time.Time{}
		case since.IsZero():
			since = time.Now()
		case time.Since(since) >= liveSettle:
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
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
// whose last message is the prompt itself, the text of the first take the relaunched process's
// plugin had accepted — waited for from ready for relaunchFirstPromptWithin. A relaunched process
// that takes no task in that time fails the step's sentence assertion: the sentence is what the
// first take was to carry.
func (r *busyRelaunchLive) firstPromptAfter(pane *busyPane, killed, ready time.Time) seenRequest {
	r.t.Helper()
	deadline := ready.Add(relaunchFirstPromptWithin)
	for {
		if prompt, ok := pane.stand.firstPromptAfter(killed); ok {
			return prompt
		}
		if time.Now().After(deadline) {
			r.fails("sentence", "the relaunched process of %s ran no turn of a take in the %s after its ready", pane.token, relaunchFirstPromptWithin)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// holdsTheSentence holds that the prompt carries every one of words.
func (r *busyRelaunchLive) holdsTheSentence(prompt seenRequest, words ...string) {
	r.t.Helper()
	for _, said := range words {
		if !strings.Contains(prompt.last, said) {
			r.fails("sentence", "the first prompt the relaunched process ran does not say %q; it reads:\n%s", said, indentOutput(prompt.last))
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

// claimRowHolds reads the claim row's `ended_work` and holds that it records a death and that want
// holds of it, which says whether a report covers the process's last turn (Reported). The read is
// SQL, so over the base commit — whose schema has no such column — the read's own error is this
// assertion's failure.
func (r *busyRelaunchLive) claimRowHolds(token claim.Token, want func(endedWorkRow) bool) endedWorkRow {
	r.t.Helper()
	ended, err := readEndedWork(r.st, token)
	if err != nil {
		r.fails("claim-row", "read %s's ended_work: %v", token, err)
	}
	if !strings.HasPrefix(ended.Cause, endedWorkDiedCause) || !want(ended) {
		r.fails("claim-row", "%s's row records %s", token, ended.describe())
	}
	return ended
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

// writeOmpProfile writes the profile every pane of this proof shares: the three stand-in
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
// `retry.enabled` off so the host walks no chain of its own; and the claim's own
// (writeRoleOverlay), the one model its stand-in serves for every model role, `deep` — the model
// of the deep-worker agent a step's subagent runs — among them.
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

// ---- the scripted model stand-in ---------------------------------------------------------------------

// scriptedStandin is one pane's provider endpoint: paneStandin, which answers every turn alike and
// answers the plugin's side question and a compaction's summary apart, behind the answers a step
// scripts in front of it — a `tool_use` answer, or a request held until the process that asked
// dies. It keeps every Messages request it was sent, scripted or not, which is what a step reads
// back.
type scriptedStandin struct {
	*paneStandin

	mu      sync.Mutex
	rules   []*standinRule
	seen    []seenRequest
	stopped chan struct{}
}

// standinRule is one scripted answer: the request it takes — the first one it matches, and no
// other — and what it does with it.
type standinRule struct {
	match func(seenRequest) bool
	spent bool
	// call, when set, is the tool the request is answered with a call of.
	call *toolCall
	// hold, when set, holds the request until the process that asked dies.
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
	// conversation is the text of every message of the request. Only the rules read it, as the
	// request arrives (ruleFor), so the request kept in seen drops it.
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
	rule := s.ruleFor(seen)
	seen.conversation = ""
	s.seen = append(s.seen, seen)
	id := fmt.Sprintf("toolu_%s_%d", s.name, len(s.seen))
	s.mu.Unlock()
	if rule != nil && rule.hold != nil && !rule.hold.take(req.Context(), s.stopped) {
		return
	}
	if rule != nil && rule.call != nil {
		streamAnthropicToolUse(w, s.name, id, rule.call.name, rule.call.input)
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
	maps.Copy(called, input)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules = append(s.rules, &standinRule{match: match, call: &toolCall{name: name, input: called}})
}

// hold holds the first request match takes until the process that asked dies, which is how a
// subagent is kept alive until its process is killed.
func (s *scriptedStandin) hold(match func(seenRequest) bool) *heldRequest {
	held := &heldRequest{arrived: make(chan struct{})}
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

// heldRequest is the one request a hold takes: arrived closes when the stand-in takes it. A step
// holds a request until the process that asked dies, which is how a subagent, or the turn of a
// task, is kept running until its process is killed.
type heldRequest struct {
	arrived chan struct{}
	at      time.Time
}

// take holds the request until the asker goes away or the run ends, and answers whether the
// request is still to be answered.
func (h *heldRequest) take(asked context.Context, stopped <-chan struct{}) bool {
	h.at = time.Now()
	close(h.arrived)
	select {
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
	// The newest message the session sent: past any date/cwd reminder the host sends after it
	// (isDateCwdReminder), which on a run that crosses midnight would read as a prompt.
	newest := len(request.Messages) - 1
	for newest > 0 && isDateCwdReminder(request.Messages[newest].Content) {
		newest--
	}
	last := request.Messages[newest]
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
