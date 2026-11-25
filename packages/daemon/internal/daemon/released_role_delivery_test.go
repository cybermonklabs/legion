package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testlog"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The cases here are R1 and R2 of dispatch://INFRA-4480, the hook prompt and the role message
// that each meet a released pod, cases (a) to (e) of its R3. Each runs one outbox over an isolated
// database holding LEGION-208 (runnerIn), the rows it drains written as the workflow writes them,
// against a listener that keeps a kept publish for a role no live session holds
// (holderPublisher with keeps), as the listener of cybermonklabs/legion#4079 does.

// releasedRoleRig is one such outbox and the role its cases are about: its claim's token and Envoy
// topic, the topic no live session holds, and the daemon's log. ahead is how far the runner's clock
// runs ahead of the wall clock at its next drain, raised at each drain past every backoff a row
// was retried on (record.RetryBackoff is never above 64 s).
type releasedRoleRig struct {
	t         *testing.T
	runner    *outbox
	sup       *supervisor
	pool      *store.Pool
	publisher *holderPublisher
	logs      *testlog.Buffer
	role      claim.Role
	token     claim.Token
	topic     string
	ahead     time.Duration
}

// newReleasedRoleRig is the rig of role on LEGION-208 standing in at, with no claim of the role
// yet (release creates it) and, as runnerIn leaves it, no admission slot.
func newReleasedRoleRig(t *testing.T, at phase.Phase, role claim.Role) *releasedRoleRig {
	t.Helper()
	logs := &testlog.Buffer{}
	publisher := &holderPublisher{keeps: true}
	runner, sup, pool := runnerIn(t, at, "in_progress", publisher, slog.New(slog.NewJSONHandler(logs, nil)))
	runner.provision = func(context.Context, workspace.Request) (workspace.Workspace, error) {
		return workspace.Workspace{Dir: t.TempDir(), Bookmark: "legion/LEGION-208"}, nil
	}
	runner.remove = func(context.Context, workspace.Workspace, sync.Locker) error { return nil }
	token := mustClaimToken(t, "LEGION-208", role)
	topic := notify.RoleTopicPrefix + string(token)
	publisher.setAbsent(topic)
	return &releasedRoleRig{t: t, runner: runner, sup: sup, pool: pool, publisher: publisher, logs: logs, role: role,
		token: token, topic: topic}
}

// release creates the role's claim as an idle release left it (releasedClaim).
func (r *releasedRoleRig) release() *supervise.Machine {
	r.t.Helper()
	return releasedClaim(r.t, r.sup, r.role)
}

// enqueue writes payload as a row of LEGION-208, due a minute ago, and answers its id.
func (r *releasedRoleRig) enqueue(payload record.OutboxPayload) int64 {
	r.t.Helper()
	var id int64
	r.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, err = r.runner.records.EnqueueID(ctx, tx, mustOutboxRow(r.t, "LEGION-208", payload, time.Now().Add(-time.Minute)))
		return err
	})
	return id
}

func (r *releasedRoleRig) inTx(write func(context.Context, pgx.Tx) error) {
	r.t.Helper()
	if err := store.Transact(context.Background(), r.pool, write); err != nil {
		r.t.Fatalf("write the record: %v", err)
	}
}

// drain runs every row due on the runner's clock, that clock moved past the backoff of every
// attempt before it.
func (r *releasedRoleRig) drain(what string) {
	r.t.Helper()
	r.ahead += 2 * time.Minute
	drainAt(r.t, r.runner, r.ahead, what)
}

// takeWake takes one wake of the role's released pod as the store takes every wake
// (store.Store.TakePodWake): the role's resume, open until its first turn. It resumes nothing
// itself, as the workflow's wake resumes nothing but through the start it queues.
func (r *releasedRoleRig) takeWake(kind string) {
	r.t.Helper()
	taken, err := r.runner.trees.TakePodWake(context.Background(), r.runner.project, "LEGION-208", r.token, kind, time.Now().UTC(), 0)
	if err != nil || !taken {
		r.t.Fatalf("take a %s wake of the released %s = %t, %v; want it taken", kind, r.role, taken, err)
	}
}

// still says whether the outbox still holds row id.
func (r *releasedRoleRig) still(id int64) bool {
	r.t.Helper()
	var held int
	if err := r.pool.QueryRow(context.Background(), `select count(*) from outbox where id = $1`, id).Scan(&held); err != nil {
		r.t.Fatalf("read outbox row %d: %v", id, err)
	}
	return held == 1
}

// lineCount is how many lines of the daemon's log carry msg.
func (r *releasedRoleRig) lineCount(msg string) int {
	return strings.Count(r.logs.String(), fmt.Sprintf("%q", msg))
}

// ciMessage is a CI settlement's role message as workflow's tellChecks writes it for LEGION-208's
// pull request.
func ciMessage(token claim.Token) record.RoleMessage {
	return record.RoleMessage{Role: string(token), Body: `Pull request #42: CI settled at head1: "build" failed. ` +
		"Nothing moved on it: decide whether to re-run the checks, wait for them, or fix the code."}
}

// releasedLine is the line a role message to a released role with no wake under way ends on.
const releasedLine = "outbox role message finished undelivered: its role's pod is released, and the message is not worth a wake"

// taskAtPublishPublisher is the rig's listener that reads, at each kept publish, the task the
// machine holds then: what a kept message to a woken role is published after.
type taskAtPublishPublisher struct {
	*holderPublisher
	machine *supervise.Machine
	mu      sync.Mutex
	tasks   []string
}

func (p *taskAtPublishPublisher) PublishKept(ctx context.Context, topic, message string, payload any, key string) (bool, error) {
	task := ""
	if pending := p.machine.View().Pending; pending != nil {
		task = pending.Task
	}
	p.mu.Lock()
	p.tasks = append(p.tasks, task)
	p.mu.Unlock()
	return p.holderPublisher.PublishKept(ctx, topic, message, payload, key)
}

// R3 (a) of dispatch://INFRA-4480, R1: the architect moves LEGION-208 back from accepting to
// implementing while the issue holds no admission slot (accepting keeps none, record.KeepsSlot),
// and its pod is released. The move writes, in its own transaction and in its own order, the
// work-back hook's prompt for the implementer (workflow's recordPass and fireHook) and then the
// start of implementing (workflow.StartOrOwe), which waits for the issue's slot. Over several
// outbox passes the prompt wakes no role and publishes nothing. Once the issue takes a slot and its
// phase start has been given to the implementer, the prompt is published to the implementer, with
// keep, after its phase task.
//
// At the pull request's base the prompt's first attempt wakes the released implementer
// (wakeForDelivery) while the issue holds no slot, which is the wake a production daemon logged
// for the work-back hook's prompt; the test fails where it reads the wake after the first pass.
func TestAHookPromptOfAMoveBackFromAcceptingWaitsForTheIssuesSlot(t *testing.T) {
	t.Parallel()
	r := newReleasedRoleRig(t, phase.Implementing, claim.RoleImplementer)
	implementer := r.release()
	listener := &taskAtPublishPublisher{holderPublisher: r.publisher, machine: implementer}
	r.runner.notices = listener

	const task = "Implement the change again: the architect sent it back from its acceptance."
	prompt := record.HookPrompt{Hook: "back-to-implementing", Role: claim.RoleImplementer, From: phase.Accepting, To: phase.Implementing,
		Count: 1, Counts: map[string]int{phase.Pair(phase.Accepting, phase.Implementing): 1},
		Prompt: "Read what the architect sent back before you write anything."}
	var promptRow int64
	r.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if promptRow, err = r.runner.records.EnqueueID(ctx, tx, mustOutboxRow(t, "LEGION-208", prompt, time.Now().Add(-time.Minute))); err != nil {
			return err
		}
		return workflow.StartOrOwe(ctx, tx, r.runner.records, "LEGION-208", record.SuperviseRequest{Op: "start", Tree: "LEGION-208",
			Role: claim.RoleImplementer, Generation: 1, Phase: phase.Implementing, Task: task}, time.Now().Add(-time.Minute))
	})

	for pass := 1; pass <= 3; pass++ {
		r.drain(fmt.Sprintf("outbox pass %d with no slot", pass))
		if wakes := podResumes(t, r.pool, "LEGION-208"); wakes != 0 {
			t.Fatalf("pass %d: the hook prompt took %d wakes of the released implementer while the issue holds no slot, want none: a woken role works past the admission cap",
				pass, wakes)
		}
		if held := implementer.View(); !held.Released() {
			t.Fatalf("pass %d: the implementer is %s by %q, want it still released while the issue holds no slot", pass, held.State, held.SuspendedBy)
		}
		if attempts, _ := r.publisher.snapshot(); attempts != 0 {
			t.Fatalf("pass %d: the hook prompt was published %d times while the issue holds no slot, want none", pass, attempts)
		}
		if !r.still(promptRow) || outboxRows(t, r.pool) != 2 {
			t.Fatalf("pass %d: the outbox holds the prompt %t and %d rows, want the prompt and the start both waiting", pass, r.still(promptRow), outboxRows(t, r.pool))
		}
	}

	holdSlot(t, r.pool, "LEGION-208")
	for pass := 1; pass <= 5 && r.still(promptRow); pass++ {
		r.drain(fmt.Sprintf("outbox pass %d with the slot", pass))
	}
	implementer.Wait()
	kept := r.publisher.keptCopies()
	if attempts, delivered := r.publisher.snapshot(); attempts != 1 || len(delivered) != 0 || len(kept) != 1 {
		t.Fatalf("the hook prompt was published %d times, %d delivered and %d kept, want once, kept for the woken implementer", attempts, len(delivered), len(kept))
	}
	if kept[0].topic != r.topic || kept[0].message != prompt.Message("LEGION-208") {
		t.Fatalf("the listener keeps %#v, want the prompt's words for %s", kept[0], r.topic)
	}
	listener.mu.Lock()
	tasks := append([]string(nil), listener.tasks...)
	listener.mu.Unlock()
	if len(tasks) != 1 || !strings.Contains(tasks[0], task) {
		t.Fatalf("the implementer held the tasks %q when the prompt was published, want its phase task %q: the prompt follows the task of the round it was written for",
			tasks, task)
	}
	if left := outboxRows(t, r.pool); left != 0 {
		t.Fatalf("outbox rows left = %d, want the start and the prompt both finished", left)
	}
}

// R3 (b) of dispatch://INFRA-4480, R2: a CI settlement's role message to LEGION-208's released
// merger, whose wake is already under way: the role's resume is open (a role message's wake, as
// the workflow takes it) and the wake's start is queued, held by the tree's closed design gate
// (mergerWaitsForTheDesignGate). While the claim reads released the message is published nowhere
// and wakes nothing, over several passes. Once the gate opens, the start resumes the merger, and
// the message is published once, with keep, so the listener holds it for the woken session.
//
// At the pull request's base the message's first attempt ends with "its role's pod is released,
// and the message is not worth a wake", as a production daemon ended a merger's role message 40 s
// after a wake of that merger whose start waited for a closed design gate, and 28 s before that
// wake's resume; the test fails where it reads the row after that attempt.
func TestARoleMessageToAReleasedRoleWaitsForTheWakeUnderWay(t *testing.T) {
	t.Parallel()
	r := newReleasedRoleRig(t, phase.Merging, claim.RoleMerger)
	r.runner.gatedSteps = config.DesignGateRootIssues.Steps()
	r.inTx(func(ctx context.Context, tx pgx.Tx) error {
		if err := r.runner.records.PutGate(ctx, tx, record.DesignGate{Issue: "LEGION-208", Step: record.DesignStepSpec, ArtifactID: "spec-208",
			LatestVersion: 2, ApprovedVersion: new(1), EverOpened: true}); err != nil {
			return err
		}
		return r.runner.records.RaiseTold(ctx, tx, "LEGION-208", record.AudienceHolder,
			raisedTo(map[record.DesignStep]int{record.DesignStepSpec: 2}), time.Now())
	})
	merger := r.release()
	r.takeWake(pods.WakeRoleMessage)
	start := r.enqueue(record.SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleMerger, Generation: 1,
		Task: workflow.WakeTask(pods.WakeRoleMessage, string(r.token), workflow.StoppedByRelease), WakeKind: pods.WakeRoleMessage})
	message := ciMessage(r.token)
	row := r.enqueue(message)

	for pass := 1; pass <= 3; pass++ {
		r.drain(fmt.Sprintf("outbox pass %d under the closed gate", pass))
		if !r.still(row) {
			t.Fatalf("pass %d: the role message's row ended (%d lines %q), want it waiting for the wake already under way", pass,
				r.lineCount(releasedLine), releasedLine)
		}
		if !r.still(start) {
			t.Fatalf("pass %d: the wake's start ended, want it held by the closed design gate", pass)
		}
		if attempts, _ := r.publisher.snapshot(); attempts != 0 {
			t.Fatalf("pass %d: the role message was published %d times while the merger reads released, want none: a publish makes the listener raise a wake", pass, attempts)
		}
		if held := merger.View(); !held.Released() {
			t.Fatalf("pass %d: the merger is %s by %q, want it released while its start waits at the gate", pass, held.State, held.SuspendedBy)
		}
		if wakes := podResumes(t, r.pool, "LEGION-208"); wakes != 1 {
			t.Fatalf("pass %d: the merger took %d wakes, want the one already under way", pass, wakes)
		}
	}

	r.inTx(func(ctx context.Context, tx pgx.Tx) error {
		return r.runner.records.PutGate(ctx, tx, record.DesignGate{Issue: "LEGION-208", Step: record.DesignStepSpec, ArtifactID: "spec-208",
			LatestVersion: 2, ApprovedVersion: new(2), EverOpened: true})
	})
	for pass := 1; pass <= 5 && r.still(row); pass++ {
		r.drain(fmt.Sprintf("outbox pass %d after the gate opened", pass))
	}
	if r.still(start) {
		t.Fatalf("the wake's start is still queued after the gate opened, want it run")
	}
	kept := r.publisher.keptCopies()
	if attempts, delivered := r.publisher.snapshot(); attempts != 1 || len(delivered) != 0 || len(kept) != 1 {
		t.Fatalf("the role message was published %d times, %d delivered and %d kept, want once, kept for the merger's woken session", attempts, len(delivered), len(kept))
	}
	if kept[0].topic != r.topic || kept[0].message != message.Body || kept[0].key != record.OutboxKey(row) {
		t.Fatalf("the listener keeps %#v, want the message to %s under its row's key", kept[0], r.topic)
	}
	if wakes := podResumes(t, r.pool, "LEGION-208"); wakes != 1 {
		t.Fatalf("the merger took %d wakes, want the one already under way: the message is no wake", wakes)
	}
}

// R3 (c) and (d) of dispatch://INFRA-4480, R2: a role message to a released role whose wake is not
// under way ends as it always has, with "its role's pod is released, and the message is not worth
// a wake", and starts no wake: (c) with no wake of the role at all, and (d) with the role's resume
// open but no start of the role queued, which is no wake bringing the claim back. Both guard
// today's line, and pass at the pull request's base.
func TestARoleMessageToAReleasedRoleWithNoWakeUnderWayEndsAsBefore(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		woken bool
	}{
		{name: "no wake of the role"},
		{name: "an open resume and no start of the role queued", woken: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newReleasedRoleRig(t, phase.Implementing, claim.RoleImplementer)
			holdSlot(t, r.pool, "LEGION-208")
			implementer := r.release()
			wakes := 0
			if tc.woken {
				r.takeWake(pods.WakeChecks)
				wakes = 1
			}
			row := r.enqueue(ciMessage(r.token))

			r.drain("the role message's attempt")
			if r.still(row) {
				t.Fatalf("the role message's row is still queued, want it ended: no wake of the role is under way")
			}
			if n := r.lineCount(releasedLine); n != 1 {
				t.Fatalf("the daemon logged %q %d times, want once:\n%s", releasedLine, n, r.logs.String())
			}
			if attempts, _ := r.publisher.snapshot(); attempts != 0 {
				t.Fatalf("the role message was published %d times, want none", attempts)
			}
			if held := implementer.View(); !held.Released() {
				t.Fatalf("the implementer is %s by %q, want it still released: the message is no wake", held.State, held.SuspendedBy)
			}
			if got := podResumes(t, r.pool, "LEGION-208"); got != wakes {
				t.Fatalf("the implementer has %d wakes, want %d: the message took none", got, wakes)
			}
		})
	}
}

// R3 (e) of dispatch://INFRA-4480, R2: a role message written while LEGION-208's implementer reads
// released, whose pod a delivery's wake then brings back (wakeForDelivery). The wake pacing parks
// the woken claim's launch over no process, its turn far off (parkedTurn), and while it holds the
// launch the message is published nowhere. Once the turn launches the claim, the message is
// published once, with keep, so the listener holds it until the woken session claims its topic.
//
// At the pull request's base the message's first attempt after the wake publishes it with keep:
// false while the launch is parked, the listener answers that no live session holds the role, and
// the row ends with the message kept nowhere; the test fails where it reads the publishes of that
// attempt.
func TestARoleMessageToARoleWhoseWakeIsParkedWaitsForTheLaunch(t *testing.T) {
	t.Parallel()
	r := newReleasedRoleRig(t, phase.Implementing, claim.RoleImplementer)
	holdSlot(t, r.pool, "LEGION-208")
	// The machines' time is held from the start, so every wait computed from the wake's turn, a
	// re-park's included (supervise's reparkWake, which arms the turn less now), is parkedTurn
	// exactly, and fire(parkedTurn) finds it.
	clock := &heldClock{}
	clock.advance(0)
	r.sup.deps.Clock = clock
	r.sup.deps.WakePace = supervise.WakePace{Rate: 1, Burst: 1, Reserve: func(now time.Time) time.Time { return now.Add(parkedTurn) }}
	implementer := r.release()
	message := ciMessage(r.token)
	row := r.enqueue(message)

	waits, _, err := r.runner.wakeForDelivery(context.Background(), r.token, "a notice for LEGION-208", 0)
	if err != nil || !waits {
		t.Fatalf("the delivery's wake of the released implementer = %t, %v; want it waiting for the pod", waits, err)
	}
	if held := implementer.View(); held.State != supervise.StateLaunching || held.Locator != nil || held.WakeWaiting == nil {
		t.Fatalf("the implementer is %s recording %+v with wake %+v, want its woken launch parked by the wake pacing over no process",
			held.State, held.Locator, held.WakeWaiting)
	}
	if live, total := clock.armed(parkedTurn); live != 1 {
		t.Fatalf("%d live timers of %d armed for the wake's turn, want one: the pacing parks the launch with one", live, total)
	}

	for pass := 1; pass <= 2; pass++ {
		r.drain(fmt.Sprintf("outbox pass %d while the launch is parked", pass))
		if attempts, _ := r.publisher.snapshot(); attempts != 0 {
			t.Fatalf("pass %d: the role message was published %d times while the woken launch is parked, want none: a copy published now meets no holder or starts the listener's hour before the pod is up",
				pass, attempts)
		}
		if !r.still(row) {
			t.Fatalf("pass %d: the role message's row ended, want it waiting for the woken launch", pass)
		}
	}

	if live, total := clock.armed(parkedTurn); live != 1 {
		t.Fatalf("%d live timers of %d armed for the wake's turn after the parked passes, want one: a timer stopped or re-armed at another wait would never fire", live, total)
	}
	if fired := clock.fire(parkedTurn); fired != 1 {
		t.Fatalf("fired %d timers for the wake's turn, want the one the pacing armed", fired)
	}
	if held := implementer.View(); held.WakeWaiting != nil || held.Locator == nil {
		t.Fatalf("the implementer is %s recording %+v with wake %+v after the turn, want the launch run", held.State, held.Locator, held.WakeWaiting)
	}
	r.ahead = parkedTurn
	r.drain("the outbox pass after the launch")
	keptOnce(t, r.publisher, r.pool, claim.RoleImplementer, row, r.topic, message.Body, "the pass after the launch")
}

// R2 of dispatch://INFRA-4480, the claim that has left released after its wake's resume, whatever
// its state: LEGION-208's woken implementer is in its first turn (working), its resume still open
// because the stamp of that turn, which closes the resume, is written on a goroutine of its own
// (workflowRuntime.podResume) and has not landed, and its plugin has not claimed the role's topic,
// which it does inside that turn (supervise.SaidReady). The role message is published once, with
// keep, so the listener holds it until the session claims the topic.
//
// Its negative control is wakeUnderWay passing over a working or idle claim's resume, as an earlier
// draft of this change did: the message is then published with keep: false, the listener answers
// that no live session holds the role, the row ends with the message kept nowhere, and the test
// fails where it reads the publish. It fails at the pull request's base the same way.
func TestARoleMessageToARoleWhoseWakeReachedItsFirstTurnIsKept(t *testing.T) {
	t.Parallel()
	r := newReleasedRoleRig(t, phase.Implementing, claim.RoleImplementer)
	holdSlot(t, r.pool, "LEGION-208")
	if _, _, err := r.sup.Create(context.Background(), supervise.Claim{Token: r.token, Project: "legion", Tree: "LEGION-208",
		Issue: "LEGION-208", Role: claim.RoleImplementer, Generation: 1, ServingGeneration: 1, State: supervise.StateWorking,
		Session: "ses-implementer", SessionFile: t.TempDir() + "/session.jsonl",
		Locator: &runtime.Locator{Runtime: runtime.RuntimeTmux, Claim: r.token, Incarnation: "4201:1",
			Tmux: &runtime.TmuxLocator{Window: "@1", Pane: "%1"}}}, ""); err != nil {
		t.Fatalf("create the implementer in its first turn: %v", err)
	}
	r.takeWake(pods.WakeChecks)
	message := ciMessage(r.token)
	row := r.enqueue(message)

	r.drain("the role message's attempt in the woken role's first turn")
	keptOnce(t, r.publisher, r.pool, claim.RoleImplementer, row, r.topic, message.Body, "the attempt in the first turn")
}

// R1 of dispatch://INFRA-4480: a start can leave the outbox without being given. The architect
// moves LEGION-208 back from accepting to testing while the issue holds no slot and its tester's
// pod is released: the move writes the work-back hook's prompt for the tester and the tester's
// start of testing, which waits for the slot. Before that start runs, the tester fails the issue
// back to implementing, and the implementer's start takes the tester's out of the outbox
// (record.Store.EnqueueStart, which takes out the issue's pending phase start of the same slot
// mark). The prompt's round never opened, so over several passes it wakes nobody and publishes
// nothing, and its row ends with a line of its own.
//
// At 1103560a the prompt reads no queued start of testing as a start given, and its first attempt
// wakes the released tester while the issue holds no slot; the test fails where it counts the
// tester's wakes.
func TestAHookPromptWhoseMovesStartWasTakenOutWithoutBeingGivenWakesNobody(t *testing.T) {
	t.Parallel()
	r := newReleasedRoleRig(t, phase.Testing, claim.RoleTester)
	tester := r.release()
	prompt := record.HookPrompt{Hook: "back-to-testing", Role: claim.RoleTester, From: phase.Accepting, To: phase.Testing,
		Count: 1, Counts: map[string]int{phase.Pair(phase.Accepting, phase.Testing): 1}, Prompt: "Read what the architect sent back."}
	var promptRow int64
	r.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if promptRow, err = r.runner.records.EnqueueID(ctx, tx, mustOutboxRow(t, "LEGION-208", prompt, time.Now().Add(-time.Minute))); err != nil {
			return err
		}
		return workflow.StartOrOwe(ctx, tx, r.runner.records, "LEGION-208", record.SuperviseRequest{Op: "start", Tree: "LEGION-208",
			Role: claim.RoleTester, Generation: 1, Phase: phase.Testing, Task: "Test the change again."}, time.Now().Add(-time.Minute))
	})
	// The tester's fail moves the issue to implementing, whose start takes the tester's out.
	moved := *readIssue(t, r.pool, "LEGION-208")
	moved.Phase = phase.Implementing
	putOutboxIssue(t, r.pool, r.runner.records, moved)
	r.inTx(func(ctx context.Context, tx pgx.Tx) error {
		return workflow.StartOrOwe(ctx, tx, r.runner.records, "LEGION-208", record.SuperviseRequest{Op: "start", Tree: "LEGION-208",
			Role: claim.RoleImplementer, Generation: 1, Phase: phase.Implementing, Task: "Fix what the tester found."}, time.Now().Add(-time.Minute))
	})

	for pass := 1; pass <= 3 && r.still(promptRow); pass++ {
		r.drain(fmt.Sprintf("outbox pass %d", pass))
		if wakes := podResumes(t, r.pool, "LEGION-208"); wakes != 0 {
			t.Fatalf("pass %d: the hook prompt took %d wakes of the released tester, want none: its round never opened and the issue holds no slot", pass, wakes)
		}
	}
	if held := tester.View(); !held.Released() {
		t.Fatalf("the tester is %s by %q, want it still released", held.State, held.SuspendedBy)
	}
	if attempts, _ := r.publisher.snapshot(); attempts != 0 {
		t.Fatalf("the hook prompt was published %d times, want none: the round it was written for never opened", attempts)
	}
	if r.still(promptRow) {
		t.Fatalf("the hook prompt's row is still queued, want it ended: the issue left the phase its move back went to")
	}
	const left = "outbox hook prompt finished unpublished: the round its move back opened is no longer the issue's"
	if n := r.lineCount(left); n != 1 {
		t.Fatalf("the daemon logged %q %d times, want once:\n%s", left, n, r.logs.String())
	}
}

// landingRoleRun is the outbox's record store with one event run just before its first RoleRun
// read: the order a lane's start can take between the role message's read of the claim and its
// read of the role's queued starts.
type landingRoleRun struct {
	record.Store
	once sync.Once
	land func()
}

func (s *landingRoleRun) RoleRun(ctx context.Context, tx pgx.Tx, token claim.Token, issue string, role claim.Role,
	generation uint64) (record.RoleRun, error) {
	s.once.Do(s.land)
	return s.Store.RoleRun(ctx, tx, token, issue, role, generation)
}

// R2 of dispatch://INFRA-4480: the wake's start runs between the role message's read of the claim
// and its read of the role's queued starts. LEGION-208's implementer is released with its resume
// open and its wake's start queued; the role message reads the claim released, and before it reads
// the queued starts, the start resumes the claim and leaves the outbox, as a lane does (it writes
// the claim, then deletes the row). The row waits rather than ending, and its next attempt
// publishes the message once, with keep.
//
// At 1103560a the row reads no start queued beside the claim it read as released, ends with "its
// role's pod is released, and the message is not worth a wake", and the woken session never gets
// the message; the test fails where it reads the row after that attempt.
func TestARoleMessageWhoseWakeLandsBetweenItsReadsWaitsForIt(t *testing.T) {
	t.Parallel()
	r := newReleasedRoleRig(t, phase.Implementing, claim.RoleImplementer)
	holdSlot(t, r.pool, "LEGION-208")
	implementer := r.release()
	r.takeWake(pods.WakeChecks)
	start := r.enqueue(record.SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleImplementer, Generation: 1,
		Task: workflow.WakeTask(pods.WakeChecks, "head1 failed build", workflow.StoppedByRelease), WakeKind: pods.WakeChecks})
	message := ciMessage(r.token)
	row := r.enqueue(message)
	r.runner.records = &landingRoleRun{Store: r.runner.records, land: func() {
		if err := implementer.Handle(context.Background(), supervise.RequestRuleResume{Claim: r.token, Reason: "the wake's start"}); err != nil {
			t.Errorf("resume the implementer as the wake's start does: %v", err)
		}
		if _, err := r.pool.Exec(context.Background(), `delete from outbox where id = $1`, start); err != nil {
			t.Errorf("finish the wake's start: %v", err)
		}
	}}

	if err := r.runner.execute(context.Background(), mustRow(t, r.pool, row)); err == nil {
		t.Fatalf("the role message's attempt as its wake lands ended the row (%d lines %q), want it waiting for the woken session",
			r.lineCount(releasedLine), releasedLine)
	}
	if attempts, _ := r.publisher.snapshot(); attempts != 0 {
		t.Fatalf("the attempt as the wake lands published %d times, want none: it read the claim released", attempts)
	}
	r.drain("the role message's next attempt")
	keptOnce(t, r.publisher, r.pool, claim.RoleImplementer, row, r.topic, message.Body, "the next attempt")
}

// mustRow is outbox row id as the store holds it.
func mustRow(t *testing.T, pool *store.Pool, id int64) record.OutboxRow {
	t.Helper()
	var row record.OutboxRow
	var kind string
	if err := pool.QueryRow(context.Background(), `select id, kind, issue, payload from outbox where id = $1`, id).Scan(&row.ID, &kind, &row.Issue, &row.Payload); err != nil {
		t.Fatalf("read outbox row %d: %v", id, err)
	}
	row.Kind = record.OutboxKind(kind)
	return row
}
