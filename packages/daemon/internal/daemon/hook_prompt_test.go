package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
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
	ctx := context.Background()
	publisher := &outboxPublisher{}
	runner, _, pool := hookRunner(t, publisher)
	enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", firedHook, time.Now().Add(-time.Minute)))

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
	if _, ok := record.ParseOutboxKey(published[0].key); !ok {
		t.Fatalf("the prompt went under key %q, want its row's own (legion-outbox:<id>): a retry under it while the copy is kept stores nothing new (notify.HTTPPublisher.PublishKept)", published[0].key)
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
	keptAcrossTheWake(t, firedHook, claim.RoleImplementer, firedHook.Message("LEGION-208"))
}

// A released role's resume can wait for its turn in the wake pacing, which over a wave is more
// than an hour away, and meanwhile the claim reads launching with no process (supervise's
// parkWake): it is released no more, and no session of it can hold the role before that turn.
// The row publishes nothing while the wake waits, since a kept publish then would start the
// listener's hour long before the pod comes back, and it waits for the turn by name: the attempt
// that finds the park makes the row due at the turn, so the row runs once then rather than
// climbing its backoff to it, and an attempt past a turn the wake has not run yet waits on its
// backoff. Once the turn has launched the resume, the row's next attempt publishes the prompt with
// keep, once, and ends.
func TestAHookPromptWaitsOutTheParkedWakeOfItsReleasedPod(t *testing.T) {
	keptAfterTheParkedWake(t, firedHook, claim.RoleImplementer, firedHook.Message("LEGION-208"))
}

// hookRunner is an outbox runner over an isolated database holding LEGION-208 in implementing,
// the phase a move back to implementing leaves it in, with the admission slot its work holds. It
// drains on the caller's goroutine (RunOnce), as the red's runner does.
func hookRunner(t *testing.T, publisher notify.Publisher) (*outbox, *supervisor, *store.Pool) {
	t.Helper()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	issue := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION",
		Tree: "LEGION-208", Title: "Workflow", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 5}
	putOutboxIssue(t, pool, records, issue)
	holdSlot(t, pool, issue.Key)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	runner := &outbox{
		dispatchProject: "LEGION", pool: pool, records: records, supervisor: sup, trees: outboxTreeStore(t, pool, issue.Tree),
		tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), notices: publisher,
		now: func() time.Time { return time.Now() }, log: quietLogger(),
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
	keptOnce(t, publisher, pool, role, topic, message, "the next attempt")
}

// keptAfterTheParkedWake drives one role-message row for role's released claim on LEGION-208
// through a resume the wake pacing parks, against the listener keptAcrossTheWake meets. The
// pacing gives every wake a turn parkedTurn ahead and the machines' clock fires a timer only when
// the test fires it (heldClock), so the resume the row's first attempt makes waits, its claim
// launching with no process and its view naming the waiting launch. The row's next attempt, past
// the first one's backoff on the runner's clock, publishes nothing and makes the row due at the
// wake's turn; a drain before the turn does not run the row, and an attempt past the turn while
// the wake still waits for its timer falls back to the backoff. Once the test fires the wake's
// turn and the resume launches, the next attempt publishes message with keep and ends the row
// (keptOnce).
func keptAfterTheParkedWake(t *testing.T, payload record.OutboxPayload, role claim.Role, message string) {
	t.Helper()
	const parkedTurn = 2 * time.Hour
	ctx := context.Background()
	publisher := &holderPublisher{keeps: true}
	runner, sup, pool := hookRunner(t, publisher)
	// A machine takes its deps where it is built, so the clock and the pacing go in before the
	// claim is created.
	clock := &heldClock{}
	sup.deps.Clock = clock
	sup.deps.WakePace = supervise.WakePace{Rate: 1, Burst: 1, Reserve: func(now time.Time) time.Time { return now.Add(parkedTurn) }}
	topic := notify.RoleTopicPrefix + string(mustClaimToken(t, "LEGION-208", role))
	publisher.setAbsent(topic)
	machine := releasedClaim(t, sup, role)
	enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", payload, time.Now().Add(-time.Minute)))

	// attemptAt drains the outbox with the runner's clock ahead of the wall clock by ahead.
	attemptAt := func(ahead time.Duration, what string) {
		t.Helper()
		runner.now = func() time.Time { return time.Now().Add(ahead) }
		if err := runner.RunOnce(ctx); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	// waiting is the row as its last attempt left it: how often it has run, what it last waited
	// on, and when it is due.
	waiting := func() (tries int, waited string, due time.Time) {
		t.Helper()
		if err := pool.QueryRow(ctx, "select attempts, last_error, next_at from outbox").Scan(&tries, &waited, &due); err != nil {
			t.Fatalf("read the waiting row: %v", err)
		}
		return tries, waited, due
	}
	attemptAt(0, "the row's first attempt")
	held := machine.View()
	if held.State != supervise.StateLaunching || held.Locator != nil ||
		held.WakeWaiting == nil || held.WakeWaiting.Kind != supervise.WakeLaunch {
		t.Fatalf("the %s is %s recording %+v with wake %+v after the first attempt, want it launching with no process under the resume the pacing parked",
			role, held.State, held.Locator, held.WakeWaiting)
	}
	turn := held.WakeWaiting.At

	// The next attempt runs two minutes on, past the backoff the first set, which is never above
	// 64 s (retryBackoff), and finds the park.
	attemptAt(2*time.Minute, "the row's attempt that finds the park")
	tries, waited, due := waiting()
	if tries != 2 || !strings.Contains(waited, "launch, which the wake pacing holds until") {
		t.Fatalf("the row has run %d times and last waited on %q, want two attempts, the second waiting for the launch the wake pacing holds", tries, waited)
	}
	if !due.Equal(turn.Truncate(time.Microsecond)) {
		t.Fatalf("the row is due at %s, want the wake's turn, %s: a row that climbs its backoff to the turn tries every 64 s and warns at every attempt from the seventh",
			due, turn)
	}
	attemptAt(parkedTurn/2, "a drain before the wake's turn")
	if tries, _, _ := waiting(); tries != 2 {
		t.Fatalf("the row has run %d times after a drain before the wake's turn, want the two it had: it is due at the turn", tries)
	}
	// The row can come due a moment before the machine's own timer runs the wake: its turn is
	// past then, and the row waits on its backoff rather than at a moment already gone.
	attemptAt(parkedTurn+time.Minute, "an attempt past the turn whose wake has not run yet")
	tries, _, due = waiting()
	if tries != 3 || !due.After(turn) {
		t.Fatalf("the row has run %d times and is due at %s after an attempt past the turn %s, want three attempts and the row due on its backoff, past the turn",
			tries, due, turn)
	}
	if attempts, _ := publisher.snapshot(); attempts != 0 {
		t.Fatalf("the attempts while the wake waits published %d times, want none: the pod has not started, and a kept copy's hour runs from its publish", attempts)
	}

	// The wake's turn, the one timer armed for the pacing's wait: the resume launches.
	if fired := clock.fire(parkedTurn); fired != 1 {
		t.Fatalf("fired %d timers for the wake's turn, want the one the pacing armed", fired)
	}
	if held := machine.View(); held.WakeWaiting != nil || held.Locator == nil {
		t.Fatalf("the %s is %s recording %+v with wake %+v after the wake's turn, want the resume launched", role, held.State, held.Locator, held.WakeWaiting)
	}
	attemptAt(parkedTurn+3*time.Minute, "the row's attempt after the resume launched")
	keptOnce(t, publisher, pool, role, topic, message, "the attempt after the launch")
}

// keptOnce is the end both drivers check once the attempt that publishes has run, which what
// names in a failure: one publish, kept rather than delivered, of message to topic with keep and
// under the row's own key; the row finished, with the listener as the message's keeper; and the
// one resume of role's pod, the wake the row's first attempt took.
func keptOnce(t *testing.T, publisher *holderPublisher, pool *store.Pool, role claim.Role, topic, message, what string) {
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
	if _, ok := record.ParseOutboxKey(kept[0].key); !ok {
		t.Fatalf("the message went under key %q, want its row's own (legion-outbox:<id>), which a retry of the row repeats", kept[0].key)
	}
	if left := outboxRows(t, pool); left != 0 {
		t.Fatalf("outbox rows left after %s = %d, want the row finished with the listener as the message's keeper", what, left)
	}
	if resumes := podResumes(t, pool, "LEGION-208"); resumes != 1 {
		t.Fatalf("recorded resumes of the %s = %d, want the one wake the row's first attempt took", role, resumes)
	}
}
