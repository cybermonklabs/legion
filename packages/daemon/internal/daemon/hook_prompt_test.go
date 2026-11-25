package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// firedHook is the row a hook of the deployment writes when it fires on a move back: the words of
// the hook, the move, the count that fired it and every backward-move count of the issue.
var firedHook = record.HookPrompt{Hook: "back-to-implementing", Role: claim.RoleImplementer,
	From: phase.Reviewing, To: phase.Implementing, Count: 3,
	Counts:      map[string]int{"reviewing-implementing": 3, "testing-implementing": 1},
	PullRequest: "acme/widgets#86", Prompt: "Read the reviewer's thread before you write anything."}

// A hook prompt is the deployment's own words for the round a move back opened, and the role the
// hook named is who they were written for: the row publishes its own message — the daemon's facts
// of the firing and then the hook's words — to that role's Envoy topic, under the row's own key and
// with keep, so the listener keeps it for a role no live session holds, and the row ends there.
func TestAHookPromptReachesItsRolesOwnTopicAndEndsTheRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	publisher := &outboxPublisher{}
	runner, _, pool := hookRunner(t, publisher)
	enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", firedHook, time.Now().Add(-time.Minute)))
	row := noticeRowID(t, pool)

	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("drain the hook prompt: %v", err)
	}

	topic := notify.RoleTopicPrefix + string(mustClaimToken(t, "LEGION-208", claim.RoleImplementer))
	published := publisher.publishes()
	if len(published) != 1 || published[0].topic != topic {
		t.Fatalf("publishes = %#v, want the one hook prompt to %s", published, topic)
	}
	want := firedHook.Message("LEGION-208")
	if published[0].message != want || published[0].payload != any(want) {
		t.Fatalf("the implementer was told\n\t%q\nwant\n\t%q", published[0].message, want)
	}
	t.Logf("the implementer was told: %s", published[0].message)
	if !published[0].keep {
		t.Fatalf("the prompt was published with keep: false, want keep: the listener is its keeper for a role no live session holds")
	}
	if want := record.OutboxKey(row); published[0].key != want {
		t.Fatalf("the prompt went under key %q, want its row's own, %q: a retry under it while the copy is kept stores nothing new (notify.HTTPPublisher.PublishKept)", published[0].key, want)
	}
	if left := outboxRows(t, pool); left != 0 {
		t.Fatalf("outbox rows left = %d, want the row finished", left)
	}
}

// A role topic with no live holder is the ordinary state for a role an issue has never had: the
// row ends with one log line rather than waiting for a holder, and the listener's hour is the one
// bound on how late the hook's words reach a session. It ends against either listener: one with no
// kept lane, as a listener before sjawhar/legion#2588 has none, keeps nothing for a role nobody
// claims (notify.ErrNoHolder), and one with that lane keeps the prompt for an hour for the role's
// next holder.
func TestAHookPromptNoSessionHoldsEndsTheRow(t *testing.T) {
	t.Parallel()
	for _, keeps := range []bool{false, true} {
		t.Run(fmt.Sprintf("the listener keeps it %t", keeps), func(t *testing.T) {
			ctx := context.Background()
			publisher := &holderPublisher{keeps: keeps}
			runner, _, pool := hookRunner(t, publisher)
			topic := notify.RoleTopicPrefix + string(mustClaimToken(t, "LEGION-208", claim.RoleImplementer))
			publisher.setAbsent(topic)
			enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", firedHook, time.Now().Add(-time.Minute)))

			if err := runner.RunOnce(ctx); err != nil {
				t.Fatalf("drain the hook prompt: %v", err)
			}

			attempts, delivered := publisher.snapshot()
			if attempts != 1 || len(delivered) != 0 {
				t.Fatalf("publish attempts = %d with %#v delivered, want the one attempt and no delivery", attempts, delivered)
			}
			if kept, want := len(publisher.keptCopies()), map[bool]int{false: 0, true: 1}[keeps]; kept != want {
				t.Fatalf("the listener keeps %d copies of the prompt, want %d", kept, want)
			}
			if left := outboxRows(t, pool); left != 0 {
				t.Fatalf("outbox rows left = %d, want the prompt finished rather than held", left)
			}
		})
	}
}

// A role whose pod was released holds its session still, and its pod is one wake away: the row
// wakes the pod and waits on its backoff. The wake is the daemon's own, so the pod comes back
// whatever the listener does with a publish; the row's first attempt after the resume has
// launched is what publishes (TestAHookPromptForAReleasedPodIsKeptForItsWokenSession,
// TestAHookPromptWaitsOutTheParkedWakeOfItsReleasedPod).
func TestAHookPromptForAReleasedPodWakesItAndWaits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	publisher := &outboxPublisher{}
	runner, sup, pool := hookRunner(t, publisher)
	implementer := releasedClaim(t, sup, claim.RoleImplementer)
	enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", firedHook, time.Now().Add(-time.Minute)))

	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("drain the hook prompt: %v", err)
	}

	if published := publisher.publishes(); len(published) != 0 {
		t.Fatalf("publishes = %#v, want none: the pod is coming back for the prompt", published)
	}
	if left := outboxRows(t, pool); left != 1 {
		t.Fatalf("outbox rows left = %d, want the prompt waiting for the woken pod", left)
	}
	if held := implementer.Claim(); held.Released() {
		t.Fatalf("the implementer is %s by %s, want the wake to have resumed it", held.State, held.SuspendedBy)
	}
	if wakes := podResumes(t, pool, "LEGION-208"); wakes != 1 {
		t.Fatalf("recorded wakes = %d, want the one this row took", wakes)
	}
}

// A released role's woken session claims its role only after the row's next attempt has
// published: the first attempt wakes the pod and waits, and the next meets a role no live session
// holds. The row publishes with keep, so the listener keeps the prompt for the role's next holder,
// which that woken session is, and the row ends with the listener as the prompt's keeper; the pod
// is woken once. Published with keep: false, the prompt would meet the no-holder answer there and
// the row would end with the prompt kept nowhere, which is how a woken role lost it.
func TestAHookPromptForAReleasedPodIsKeptForItsWokenSession(t *testing.T) {
	t.Parallel()
	keptAcrossTheWake(t, firedHook, claim.RoleImplementer, firedHook.Message("LEGION-208"))
}

// A released role's resume can wait for its turn in the wake pacing, which over a wave is more
// than an hour away, and meanwhile the claim reads launching with no process (supervise's
// parkWake): it is released no more, and no session of it can hold the role before that turn.
// The row publishes nothing while the wake waits, since a kept publish then would start the
// listener's hour long before the pod comes back. It waits as a routine wait, to the turn or to
// parkedWakeBound on, whichever comes first, and reads the claim again then: the attempt that
// finds the park makes the row due at the bound, a drain before the bound runs nothing, an
// attempt past it while the park holds waits to the next bound, and an attempt past a turn the
// wake has not run yet waits on its backoff. Once the turn has launched the resume, the row's
// next attempt publishes the prompt with keep, once, and ends.
func TestAHookPromptWaitsOutTheParkedWakeOfItsReleasedPod(t *testing.T) {
	t.Parallel()
	keptAfterTheParkedWake(t, firedHook, claim.RoleImplementer, firedHook.Message("LEGION-208"))
}

// The turn a parked resume waits for lives in the machine's memory alone, and an operator's
// resume ends the park long before it: the request drops the waiting wake and launches at once
// (supervise's dropWakeFor). The row that found the park reads the claim again at the bound, so
// its next attempt publishes the prompt with keep, once, and ends, the pacing's turn still far
// off. The negative control is parkedWait without the bound, the row due at the turn: the attempt
// at the bound then finds the row not due and publishes nothing.
func TestAHookPromptFollowsAnOperatorsResumeOfItsParkedWakeWithinTheBound(t *testing.T) {
	t.Parallel()
	keptAfterAnOperatorsResume(t, firedHook, claim.RoleImplementer, firedHook.Message("LEGION-208"))
}

// The three answers of the confirmed publish route are other failures, and keep the ordinary retry:
// a holder that did not confirm the prompt in time can take it on the next attempt, a holder that
// does not advertise the delivery mode can be replaced by another session on the role, and a
// forward the listener's own NATS grant denies goes through once an operator changes that grant.
// Only a role with no live holder ends the row.
func TestAHookPromptRetriesEveryFailureButNoHolder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	row := mustOutboxRow(t, "LEGION-208", firedHook, time.Now())
	row.ID = 61
	publisher := &outboxPublisher{}
	runner := &outbox{project: "legion", notices: publisher, log: quietLogger()}
	const denial = `envoy: the message bus denied the publish under this listener's own permissions: bus: NATS denied the publish to "notifications.agent.ses_t"`
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "a holder that did not confirm it", err: notify.ErrNotDelivered{Reason: notify.ReasonReceiptTimeout, Holder: "ses_t"}},
		{name: "a holder that does not advertise the delivery mode", err: notify.ErrRefusedByHolder{Holder: "ses_t"}},
		{name: "a forward the listener's grant denies", err: notify.ErrPublishDenied{Message: denial}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			publisher.err = fmt.Errorf("publish to the implementer's topic: %w", tc.err)
			err := runner.execute(ctx, row)
			if err == nil || !errors.Is(err, tc.err) {
				t.Fatalf("the hook prompt answered %v = %v, want the answer as a failure, retried", tc.err, err)
			}
			t.Logf("%s: %v", tc.name, err)
		})
	}
	publisher.err = fmt.Errorf("publish to the implementer's topic: %w: no holder", notify.ErrNoHolder)
	if err := runner.execute(ctx, row); err != nil {
		t.Fatalf("the hook prompt no session holds = %v, want the row finished", err)
	}
}

// A relaunch the wake pacing parks over a process the claim still records holds no row: the
// machine leaves that claim as it stands, idle on its process (supervise's parkWake), and its
// live session can take a publish now, so the row's first attempt publishes. The claim is one of
// the wave TestARelaunchKeepsTheRunningProcessUntilItsTurnAndNoSweepReapsIt reads, a process on an
// earlier plugin contract whose stale-plugin relaunch waits its turn (parkedOverItsProcess). A row
// held there would hold every notice, hook prompt, merge-queue removal and merge forward's message
// to a live session behind that session's paced refresh. The negative control is
// wakeForDelivery's park check without its first half, the claim launching with no process: each
// row then waits for the relaunch (parkedWait) and publishes nothing.
func TestARelaunchParkedOverALiveProcessHoldsNoRow(t *testing.T) {
	t.Parallel()
	t.Run("a hook prompt", func(t *testing.T) {
		ctx := context.Background()
		publisher := &holderPublisher{}
		runner, sup, pool := hookRunner(t, publisher)
		parkedOverItsProcess(t, sup, "LEGION-208", claim.RoleImplementer)
		enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", firedHook, time.Now().Add(-time.Minute)))

		if err := runner.RunOnce(ctx); err != nil {
			t.Fatalf("the row's first attempt: %v", err)
		}
		topic := notify.RoleTopicPrefix + string(mustClaimToken(t, "LEGION-208", claim.RoleImplementer))
		attempts, delivered := publisher.snapshot()
		if attempts != 1 || len(delivered) != 1 || delivered[0].topic != topic || delivered[0].message != firedHook.Message("LEGION-208") {
			t.Fatalf("the first attempt made %d publishes and delivered %+v, want the prompt delivered to %s: the relaunch waits over a live session",
				attempts, delivered, topic)
		}
		if left := outboxRows(t, pool); left != 0 {
			t.Fatalf("outbox rows left = %d, want the row finished by its first attempt", left)
		}
	})
	t.Run("an architect's notice", func(t *testing.T) {
		ctx := context.Background()
		pool := isolatedOutboxPool(t)
		records := record.NewStore()
		noticeTree(t, pool, records, false)
		sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
		parkedOverItsProcess(t, sup, "LEGION-1", claim.RoleArchitect)
		publisher := &holderPublisher{}
		row := leasedOutboxRow(t, pool, records, mustOutboxRow(t, "LEGION-1", record.Notice{Kind: "phase-finished", Role: claim.RoleTester, Phase: phase.Testing}, time.Now()))
		runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup, project: "legion", now: time.Now}

		if err := runner.execute(ctx, row); err != nil {
			t.Fatalf("the notice's first attempt = %v, want it published: the relaunch waits over a live session", err)
		}
		want := architectTopic(t, "LEGION-1")
		if _, delivered := publisher.snapshot(); len(delivered) != 1 || delivered[0].topic != want || delivered[0].key != record.OutboxKey(row.ID) {
			t.Fatalf("notice publishes = %+v, want one to %s under the row's key", delivered, want)
		}
	})
}

// parkedOverItsProcess creates role's claim on issue, the root of its own tree, as the stale-plugin
// wave of TestARelaunchKeepsTheRunningProcessUntilItsTurnAndNoSweepReapsIt leaves one: idle on
// the process it records, which speaks the contract before the daemon's (liveClaim), under the
// relaunch that lane raises for it, parked in the wake pacing over that process. The pacing gives
// the relaunch a turn an hour ahead and the machines' clock fires no timer the test does not fire
// (heldClock), so the relaunch waits through the whole test. It is posted as the daemon's pass
// posts an idle claim's, holding a credit of the refresh gate (StalePlugin.Credit).
func parkedOverItsProcess(t *testing.T, sup *supervisor, issue string, role claim.Role) *supervise.Machine {
	t.Helper()
	ctx := context.Background()
	// A machine takes its deps where it is built, so the clock and the pacing go in before the
	// claim is created.
	sup.deps.Clock = &heldClock{}
	sup.deps.WakePace = supervise.WakePace{Rate: 1, Burst: 1, Interval: time.Minute, Reserve: func(now time.Time) time.Time { return now.Add(time.Hour) }}
	token := mustClaimToken(t, issue, role)
	live := liveClaim(1, token, sup.deps.PluginContract-1)
	live.Token, live.Project, live.Tree, live.Issue, live.Role, live.State = token, "legion", issue, issue, role, supervise.StateIdle
	machine, _, err := sup.Create(ctx, live, "")
	if err != nil {
		t.Fatalf("create the live %s's claim: %v", role, err)
	}
	if err := machine.Handle(ctx, supervise.StalePlugin{Claim: token, Trigger: "sweep", Credit: sup.deps.RefreshGate.Admit()}); err != nil {
		t.Fatalf("raise the stale-plugin relaunch of the %s: %v", role, err)
	}
	if held := machine.View(); held.State != supervise.StateIdle || held.Locator == nil ||
		held.WakeWaiting == nil || held.WakeWaiting.Kind != supervise.WakePluginRefresh {
		t.Fatalf("the %s is %s recording %+v with wake %+v, want it idle on its process under the stale-plugin relaunch the pacing parked",
			role, held.State, held.Locator, held.WakeWaiting)
	}
	return machine
}

// hookRunner is an outbox runner over an isolated database holding LEGION-208 in implementing,
// the phase a move back to implementing leaves it in, with the admission slot its work holds. It
// drains on the caller's goroutine (RunOnce).
func hookRunner(t *testing.T, publisher notify.Publisher) (*outbox, *supervisor, *store.Pool) {
	t.Helper()
	runner, sup, pool := runnerIn(t, phase.Implementing, "in_progress", publisher, quietLogger())
	holdSlot(t, pool, "LEGION-208")
	return runner, sup, pool
}

// runnerIn is an outbox runner over an isolated database holding LEGION-208 in phase at with
// Dispatch status status, its tree admitted, and a supervisor with no claim on it yet. It
// publishes to publisher, logs to log, and drains on the caller's goroutine (RunOnce).
func runnerIn(t *testing.T, at phase.Phase, status string, publisher notify.Publisher, log *slog.Logger) (*outbox, *supervisor, *store.Pool) {
	t.Helper()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: "LEGION-208", Title: "Workflow", Phase: at, Generation: 1, Status: status, Rank: "U", LastDispatchSeq: 5}
	putOutboxIssue(t, pool, records, issue)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	runner := &outbox{
		dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup, trees: outboxTreeStore(t, pool, issue.Tree),
		tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), notices: publisher,
		now: time.Now, log: log,
	}
	return runner, sup, pool
}

// releasedClaim creates role's claim on LEGION-208 as an idle release left it: suspended with the
// source `released`, its session intact, and nothing but a wake to bring its pod back.
func releasedClaim(t *testing.T, sup *supervisor, role claim.Role) *supervise.Machine {
	t.Helper()
	machine, _, err := sup.Create(context.Background(), supervise.Claim{Token: mustClaimToken(t, "LEGION-208", role),
		Project: "legion", Tree: "LEGION-208", Issue: "LEGION-208", Role: role, Generation: 1, ServingGeneration: 1,
		State: supervise.StateSuspended, SuspendedBy: supervise.SuspendedByReleased,
		Session: "ses-" + string(role), SessionFile: t.TempDir() + "/session.jsonl"}, "")
	if err != nil {
		t.Fatalf("create the released %s's claim: %v", role, err)
	}
	if held := machine.Claim(); !held.Released() {
		t.Fatalf("the %s is %s by %s, want it released", role, held.State, held.SuspendedBy)
	}
	return machine
}

// keptAcrossTheWake drives one role-message row for role's released claim on LEGION-208 through
// the two attempts a released role meets, against a listener that answers a keep: false publish
// to the role's topic notify.ErrNoHolder and keeps a kept publish for the role's next holder
// (holderPublisher with keeps), as the listener of cybermonklabs/legion#4079 answers a released
// role whose woken session has not claimed it yet. The first attempt wakes the pod and waits,
// publishing nothing; the next, run past the row's backoff on the runner's clock, publishes
// message with keep and ends the row (keptOnce).
func keptAcrossTheWake(t *testing.T, payload record.OutboxPayload, role claim.Role, message string) {
	t.Helper()
	ctx := context.Background()
	publisher := &holderPublisher{keeps: true}
	runner, sup, pool := hookRunner(t, publisher)
	topic := notify.RoleTopicPrefix + string(mustClaimToken(t, "LEGION-208", role))
	publisher.setAbsent(topic)
	machine := releasedClaim(t, sup, role)
	enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", payload, time.Now().Add(-time.Minute)))
	id := noticeRowID(t, pool)

	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("the row's first attempt: %v", err)
	}
	if attempts, _ := publisher.snapshot(); attempts != 0 {
		t.Fatalf("the first attempt published %d times, want none: it wakes the released pod and waits", attempts)
	}
	if left := outboxRows(t, pool); left != 1 {
		t.Fatalf("outbox rows left after the first attempt = %d, want the row waiting for the woken pod", left)
	}
	if held := machine.Claim(); held.Released() {
		t.Fatalf("the %s is %s by %s after the first attempt, want the wake to have resumed it", role, held.State, held.SuspendedBy)
	}

	// The row's next attempt, past its backoff. The woken session has claimed nothing yet, so no
	// live session holds the role.
	runner.now = func() time.Time { return time.Now().Add(time.Minute) }
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("the row's next attempt: %v", err)
	}
	keptOnce(t, publisher, pool, role, id, topic, message, "the next attempt")
}

const (
	// parkedTurn is how far ahead the wake pacing of parkTheWake gives every wake its turn.
	parkedTurn = 2 * time.Hour
	// parkFound is how far ahead of the wall clock the runner's clock reads at the row's attempt
	// that finds the park: past the backoff the first attempt set, which is never above 64 s
	// (record.RetryBackoff).
	parkFound = 2 * time.Minute
)

// parkedWake is one role-message row for a released claim on LEGION-208 whose resume the wake
// pacing parked, as parkTheWake leaves it: the runner, its database and the listener of
// keptAcrossTheWake, the claim's machine and the clock its timers wait on, the row as it was
// queued under its id, the role's topic, the wake's turn, and the moment the runner's clock read
// at the attempt that found the park.
type parkedWake struct {
	t         *testing.T
	runner    *outbox
	pool      *store.Pool
	publisher *holderPublisher
	machine   *supervise.Machine
	clock     *heldClock
	row       record.OutboxRow
	topic     string
	turn      time.Time
	found     time.Time
}

// parkTheWake queues one role-message row for role's released claim on LEGION-208 and runs it to
// the park, against the listener keptAcrossTheWake meets. The pacing gives every wake a turn
// parkedTurn ahead and the machines' clock fires a timer only when the test fires it
// (heldClock), so the resume the row's first attempt makes waits, its claim launching with no
// process and its view naming the waiting launch. The row's next attempt, parkFound on, finds
// the park and waits for that launch, publishing nothing.
func parkTheWake(t *testing.T, payload record.OutboxPayload, role claim.Role) *parkedWake {
	t.Helper()
	publisher := &holderPublisher{keeps: true}
	runner, sup, pool := hookRunner(t, publisher)
	// A machine takes its deps where it is built, so the clock and the pacing go in before the
	// claim is created.
	clock := &heldClock{}
	sup.deps.Clock = clock
	sup.deps.WakePace = supervise.WakePace{Rate: 1, Burst: 1, Interval: time.Minute, Reserve: func(now time.Time) time.Time { return now.Add(parkedTurn) }}
	topic := notify.RoleTopicPrefix + string(mustClaimToken(t, "LEGION-208", role))
	publisher.setAbsent(topic)
	machine := releasedClaim(t, sup, role)
	row := mustOutboxRow(t, "LEGION-208", payload, time.Now().Add(-time.Minute))
	enqueueOutbox(t, pool, runner.records, row)
	row.ID = noticeRowID(t, pool)
	p := &parkedWake{t: t, runner: runner, pool: pool, publisher: publisher, machine: machine, clock: clock, row: row, topic: topic}

	drainAt(t, runner, 0, "the row's first attempt")
	held := machine.View()
	if held.State != supervise.StateLaunching || held.Locator != nil ||
		held.WakeWaiting == nil || held.WakeWaiting.Kind != supervise.WakeLaunch {
		t.Fatalf("the %s is %s recording %+v with wake %+v after the first attempt, want it launching with no process under the resume the pacing parked",
			role, held.State, held.Locator, held.WakeWaiting)
	}
	p.turn = held.WakeWaiting.At.UTC()

	p.found = drainAt(t, runner, parkFound, "the row's attempt that finds the park")
	if tries, waited, _ := p.waiting(); tries != 2 || !strings.Contains(waited, "launch, which the wake pacing holds until") {
		t.Fatalf("the row has run %d times and last waited on %q, want two attempts, the second waiting for the launch the wake pacing holds", tries, waited)
	}
	if attempts, _ := publisher.snapshot(); attempts != 0 {
		t.Fatalf("the attempts up to the park published %d times, want none: the pod has not started, and a kept copy's hour runs from its publish", attempts)
	}
	return p
}

// drainAt drains runner's outbox with its clock held ahead of the wall clock by ahead, past the
// backoff of every attempt before it, and answers the moment that clock reads through the drain.
func drainAt(t *testing.T, runner *outbox, ahead time.Duration, what string) time.Time {
	t.Helper()
	at := time.Now().Add(ahead).UTC()
	runner.now = func() time.Time { return at }
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	return at
}

// waiting is the row as its last attempt left it: how often it has run, what it last waited on,
// and when it is due.
func (p *parkedWake) waiting() (tries int, waited string, due time.Time) {
	p.t.Helper()
	if err := p.pool.QueryRow(context.Background(), "select attempts, last_error, next_at from outbox").Scan(&tries, &waited, &due); err != nil {
		p.t.Fatalf("read the waiting row: %v", err)
	}
	return tries, waited, due
}

// keptAfterTheParkedWake drives one role-message row for role's released claim on LEGION-208
// through a resume the wake pacing parks (parkTheWake) and the park's whole wait. The attempt
// that finds the park makes the row due parkedWakeBound on, not at the wake's turn, and what it
// waits on is routine (routineWait), so settle logs none of its attempts as a warning. A drain
// before the bound does not run the row; an attempt past the bound, the park still holding,
// waits to the next bound; and an attempt past the turn while the wake still waits for its timer
// falls back to the backoff. Once the test fires the wake's turn and the resume launches, the
// next attempt publishes message with keep and ends the row (keptOnce).
func keptAfterTheParkedWake(t *testing.T, payload record.OutboxPayload, role claim.Role, message string) {
	t.Helper()
	p := parkTheWake(t, payload, role)
	_, _, due := p.waiting()
	if want := p.found.Add(parkedWakeBound); !due.Equal(want.Truncate(time.Microsecond)) {
		t.Fatalf("the row is due at %s, want %s, parkedWakeBound after the attempt that found the park rather than at the wake's turn, %s: the turn lives in the machine's memory alone, and a park that ends early would hold the row to it",
			due, want, p.turn)
	}
	// The drain keeps no error, so the wait is read from the executor run once more on the same
	// clock: over a claim parked with no process it reads the claim's view and writes nothing.
	waited := p.runner.execute(context.Background(), p.row)
	if at, named := wait.UntilAt(waited); !errors.Is(waited, wait.ErrWaiting) || !routineWait(waited) || !named || !at.Equal(p.found.Add(parkedWakeBound)) {
		t.Fatalf("the parked attempt answers %v, naming %s (%t), want a routine wait (routineWait) naming the bound, %s: settle warns at every attempt from the seventh of a wait that is not routine, about once a minute over an hour's park",
			waited, at, named, p.found.Add(parkedWakeBound))
	}
	drainAt(p.t, p.runner, parkFound+parkedWakeBound/2, "a drain before the bound")
	if tries, _, _ := p.waiting(); tries != 2 {
		t.Fatalf("the row has run %d times after a drain before the bound, want the two it had: it is due at the bound", tries)
	}
	again := drainAt(p.t, p.runner, parkFound+parkedWakeBound+time.Minute, "the row's attempt past the bound")
	tries, _, due := p.waiting()
	if want := again.Add(parkedWakeBound); tries != 3 || !due.Equal(want.Truncate(time.Microsecond)) {
		t.Fatalf("the row has run %d times and is due at %s after an attempt past the bound, want three attempts and the row due at the next bound, %s, the park still holding",
			tries, due, want)
	}
	// The row can come due a moment before the machine's own timer runs the wake: its turn is
	// past then, and the row waits on its backoff rather than at a moment already gone.
	drainAt(p.t, p.runner, parkedTurn+time.Minute, "an attempt past the turn whose wake has not run yet")
	tries, _, due = p.waiting()
	if tries != 4 || !due.After(p.turn) {
		t.Fatalf("the row has run %d times and is due at %s after an attempt past the turn %s, want four attempts and the row due on its backoff, past the turn",
			tries, due, p.turn)
	}
	if attempts, _ := p.publisher.snapshot(); attempts != 0 {
		t.Fatalf("the attempts while the wake waits published %d times, want none: the pod has not started, and a kept copy's hour runs from its publish", attempts)
	}

	// The wake's turn, the one timer armed for the pacing's wait: the resume launches.
	if fired := p.clock.fire(parkedTurn); fired != 1 {
		t.Fatalf("fired %d timers for the wake's turn, want the one the pacing armed", fired)
	}
	if held := p.machine.View(); held.WakeWaiting != nil || held.Locator == nil {
		t.Fatalf("the %s is %s recording %+v with wake %+v after the wake's turn, want the resume launched", role, held.State, held.Locator, held.WakeWaiting)
	}
	drainAt(p.t, p.runner, parkedTurn+3*time.Minute, "the row's attempt after the resume launched")
	keptOnce(t, p.publisher, p.pool, role, p.row.ID, p.topic, message, "the attempt after the launch")
}

// keptAfterAnOperatorsResume drives one role-message row for role's released claim on LEGION-208
// to the park the wake pacing makes of its resume (parkTheWake), and then resumes the claim as an
// operator does through the API's resume route (supervise.RequestResume): the request drops the
// waiting wake and launches at once (supervise's dropWakeFor), the pacing's timer never fired.
// The row's next attempt, at the bound after the attempt that found the park, publishes message
// with keep and ends the row (keptOnce).
func keptAfterAnOperatorsResume(t *testing.T, payload record.OutboxPayload, role claim.Role, message string) {
	t.Helper()
	p := parkTheWake(t, payload, role)
	if err := p.machine.Handle(context.Background(), supervise.RequestResume{Claim: mustClaimToken(t, "LEGION-208", role)}); err != nil {
		t.Fatalf("the operator's resume of the parked %s: %v", role, err)
	}
	if held := p.machine.View(); held.WakeWaiting != nil || held.Locator == nil {
		t.Fatalf("the %s is %s recording %+v with wake %+v after the operator's resume, want the resume launched at once", role, held.State, held.Locator, held.WakeWaiting)
	}
	drainAt(p.t, p.runner, parkFound+parkedWakeBound, "the row's attempt at the bound")
	if attempts, _ := p.publisher.snapshot(); attempts == 0 {
		_, _, due := p.waiting()
		t.Fatalf("the row's attempt at the bound after the operator's resume published nothing, the row due at %s: a row due at the wake's turn, %s, waits out a turn the operator's resume has already ended",
			due, p.turn)
	}
	keptOnce(t, p.publisher, p.pool, role, p.row.ID, p.topic, message, "the attempt at the bound")
}

// keptOnce is the end every driver checks once the attempt that publishes has run, which what
// names in a failure: one publish, kept rather than delivered, of message to topic with keep and
// under the key of row, the row's own; the row finished, with the listener as the message's
// keeper; and the one resume of role's pod, the wake the row's first attempt took.
func keptOnce(t *testing.T, publisher *holderPublisher, pool *store.Pool, role claim.Role, row int64, topic, message, what string) {
	t.Helper()
	attempts, delivered := publisher.snapshot()
	kept := publisher.keptCopies()
	if attempts != 1 || len(delivered) != 0 || len(kept) != 1 {
		t.Fatalf("%s made %d publishes, %d delivered and %d kept, want the one publish, kept for the role's next holder: a keep: false publish meets the no-holder answer and the message is kept nowhere",
			what, attempts, len(delivered), len(kept))
	}
	if kept[0].topic != topic || !kept[0].keep || kept[0].message != message || kept[0].payload != any(message) {
		t.Fatalf("the listener keeps %#v, want the message to %s, published with keep:\n\t%q", kept[0], topic, message)
	}
	if want := record.OutboxKey(row); kept[0].key != want {
		t.Fatalf("the message went under key %q, want its row's own, %q, which a retry of the row repeats", kept[0].key, want)
	}
	if left := outboxRows(t, pool); left != 0 {
		t.Fatalf("outbox rows left after %s = %d, want the row finished with the listener as the message's keeper", what, left)
	}
	if resumes := podResumes(t, pool, "LEGION-208"); resumes != 1 {
		t.Fatalf("recorded resumes of the %s = %d, want the one wake the row's first attempt took", role, resumes)
	}
}
