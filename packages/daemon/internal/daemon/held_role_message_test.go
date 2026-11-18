package daemon

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// While a claim keeps the session it recorded and Envoy has no holder for its role, the daemon
// holds each role message and hook prompt for it until a session holds the role again, the claim
// ends, or its phase moves on, in place of finishing it undelivered (R5): the claim is one the
// daemon still runs and the controller may relaunch, and the resumed session reads the held message
// with its time. A phase-ended message keeps today's end.

// sessionKeptClaim restores role's claim on LEGION-208 in state on its recorded session: the claim
// of a session Envoy has stopped hearing from, which the daemon still runs.
func sessionKeptClaim(t *testing.T, sup *supervisor, role claim.Role, state supervise.ClaimState) *supervise.Machine {
	t.Helper()
	token := mustClaimToken(t, "LEGION-208", role)
	if _, err := sup.restore(context.Background(), []supervise.Claim{{Token: token,
		Project: "legion", Tree: "LEGION-208", Issue: "LEGION-208", Role: role, Generation: 1, ServingGeneration: 1,
		State: state, Session: "ses-" + string(role), SessionFile: t.TempDir() + "/session.jsonl",
		Locator: pluginTestLocator(token)}}); err != nil {
		t.Fatalf("restore the %s's claim: %v", role, err)
	}
	machine, ok := sup.Machine(token)
	if !ok {
		t.Fatalf("the %s's claim is not supervised", role)
	}
	return machine
}

// moveHookIssuePhase moves hookRunner's LEGION-208 to to, as the workflow moves an issue on.
func moveHookIssuePhase(t *testing.T, pool *store.Pool, records record.Store, to phase.Phase) {
	t.Helper()
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: to, Generation: 2, Status: "in_progress", Rank: "U", LastDispatchSeq: 6})
}

// dueNow brings every waiting outbox row forward, as its backoff running out would.
func dueNow(t *testing.T, pool *store.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), "update outbox set next_at = now() - interval '1 second'"); err != nil {
		t.Fatalf("bring the waiting rows forward: %v", err)
	}
}

func drain(t *testing.T, runner *outbox) {
	t.Helper()
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("drain the outbox: %v", err)
	}
}

func TestAHookPromptForAClaimThatKeepsItsSessionIsHeldUntilItsRoleIsHeld(t *testing.T) {
	publisher := &holderPublisher{}
	runner, sup, pool := hookRunner(t, publisher)
	sessionKeptClaim(t, sup, claim.RoleImplementer, supervise.StateWorking)
	topic := notify.RoleTopicPrefix + string(mustClaimToken(t, "LEGION-208", claim.RoleImplementer))
	publisher.setAbsent(topic)
	enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", firedHook, time.Now().Add(-time.Minute)))

	drain(t, runner)
	if left := outboxRows(t, pool); left != 1 {
		t.Fatalf("outbox rows left = %d, want the prompt held for the claim that keeps its session", left)
	}

	// A session holds the role again: the held prompt reaches it.
	publisher.setAbsent()
	dueNow(t, pool)
	drain(t, runner)
	if _, delivered := publisher.snapshot(); len(delivered) != 1 || delivered[0].topic != topic {
		t.Fatalf("delivered = %#v, want the held prompt delivered to %s", delivered, topic)
	}
	if left := outboxRows(t, pool); left != 0 {
		t.Fatalf("outbox rows left = %d, want the delivered prompt finished", left)
	}
}

// A listener that keeps the prompt for the role's next holder (cybermonklabs/legion#4079) is its
// one keeper, so the row ends there, and is not held beside the kept copy, though the claim keeps
// its session: the relaunched process on that session takes it from the kept lane when it holds
// the role again.
func TestAHookPromptTheListenerKeepsIsNotHeldForAClaimThatKeepsItsSession(t *testing.T) {
	publisher := &holderPublisher{keeps: true}
	runner, sup, pool := hookRunner(t, publisher)
	sessionKeptClaim(t, sup, claim.RoleImplementer, supervise.StateWorking)
	topic := notify.RoleTopicPrefix + string(mustClaimToken(t, "LEGION-208", claim.RoleImplementer))
	publisher.setAbsent(topic)
	enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", firedHook, time.Now().Add(-time.Minute)))

	drain(t, runner)
	if left := outboxRows(t, pool); left != 0 {
		t.Fatalf("outbox rows left = %d, want the row ended once the listener kept the prompt", left)
	}
	publisher.mu.Lock()
	kept := publisher.kept
	publisher.mu.Unlock()
	if len(kept) != 1 || kept[0].topic != topic || !kept[0].keep {
		t.Fatalf("kept = %#v, want the prompt kept once on %s", kept, topic)
	}
}

func TestAHeldHookPromptFinishesWhenTheClaimEndsOrItsPhaseMovesOn(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(t *testing.T, runner *outbox, pool *store.Pool, implementer *supervise.Machine)
	}{
		{name: "the claim ends", end: func(t *testing.T, _ *outbox, _ *store.Pool, implementer *supervise.Machine) {
			if err := implementer.Handle(context.Background(), supervise.RequestStop{Claim: implementer.Claim().Token}); err != nil {
				t.Fatalf("end the implementer's claim: %v", err)
			}
		}},
		{name: "its phase moves on", end: func(t *testing.T, runner *outbox, pool *store.Pool, _ *supervise.Machine) {
			moveHookIssuePhase(t, pool, runner.records, phase.Reviewing)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			publisher := &holderPublisher{}
			runner, sup, pool := hookRunner(t, publisher)
			implementer := sessionKeptClaim(t, sup, claim.RoleImplementer, supervise.StateIdle)
			publisher.setAbsent(notify.RoleTopicPrefix + string(implementer.Claim().Token))
			enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", firedHook, time.Now().Add(-time.Minute)))
			drain(t, runner)
			if left := outboxRows(t, pool); left != 1 {
				t.Fatalf("outbox rows left = %d, want the prompt held", left)
			}

			tc.end(t, runner, pool, implementer)
			dueNow(t, pool)
			drain(t, runner)
			if left := outboxRows(t, pool); left != 0 {
				t.Fatalf("outbox rows left = %d, want the prompt finished undelivered once %s", left, tc.name)
			}
			if _, delivered := publisher.snapshot(); len(delivered) != 0 {
				t.Fatalf("delivered = %#v, want nothing", delivered)
			}
		})
	}
}

// A daemon that ends during a relaunch's launch restores the claim as launch_uncertain, and a
// claim waiting for its launch is queued; the outbox starts before the boot reconciles either.
// Neither has left the session it records, so a role message or hook prompt for it is held until a
// session holds the role again, as it is while the claim runs (R5), not finished as a claim that
// ended.
func TestARowIsHeldForAClaimRestoredMidLaunch(t *testing.T) {
	for _, state := range []supervise.ClaimState{supervise.StateLaunchUncertain, supervise.StateQueued} {
		for name, payload := range storedRows(t) {
			t.Run(string(state)+"/"+name, func(t *testing.T) {
				publisher := &holderPublisher{}
				runner, sup, pool := hookRunner(t, publisher)
				implementer := sessionKeptClaim(t, sup, claim.RoleImplementer, state)
				if got := implementer.View().State; got != state {
					t.Fatalf("the restored claim reads %s, want %s", got, state)
				}
				topic := notify.RoleTopicPrefix + string(implementer.Claim().Token)
				publisher.setAbsent(topic)
				enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", payload, time.Now().Add(-time.Minute)))

				drain(t, runner)
				if left := outboxRows(t, pool); left != 1 {
					t.Fatalf("outbox rows left = %d, want the row held for a claim %s on its session", left, state)
				}
				// Tried again while the claim is still so: the hold has not ended.
				dueNow(t, pool)
				drain(t, runner)
				if left := outboxRows(t, pool); left != 1 {
					t.Fatalf("outbox rows left = %d after a second try, want the row still held for a claim %s", left, state)
				}
				publisher.setAbsent()
				dueNow(t, pool)
				drain(t, runner)
				if _, delivered := publisher.snapshot(); len(delivered) != 1 || delivered[0].topic != topic {
					t.Fatalf("delivered = %#v, want the held row delivered to %s once its role is held", delivered, topic)
				}
			})
		}
	}
}

// suspendHeld is the fake runtime with a Suspend that waits, once entered is told, until release
// closes: the runtime's stop of a relaunch that waits on the cluster.
type suspendHeld struct {
	*fake.Runtime
	entered chan struct{}
	release chan struct{}
}

func (r *suspendHeld) Suspend(ctx context.Context, loc runtime.Locator) error {
	close(r.entered)
	<-r.release
	return r.Runtime.Suspend(ctx, loc)
}

// The controller's relaunch of a claim Envoy cannot reach stops its process only to start it
// again on the same session, so a role message or hook prompt held for that claim, tried while the
// relaunch's stop waits on the runtime, stays held: the claim keeps its role through the stop, and
// the session the relaunch brings back is the one the hold waits for (R5).
func TestAHeldRowTriedWhileTheRelaunchsStopWaitsStaysHeld(t *testing.T) {
	for name, payload := range storedRows(t) {
		t.Run(name, func(t *testing.T) {
			publisher := &holderPublisher{}
			runner, sup, pool := hookRunner(t, publisher)
			held := &suspendHeld{Runtime: sup.deps.Runtime.(*fake.Runtime), entered: make(chan struct{}), release: make(chan struct{})}
			sup.deps.Runtime = held
			sup.deps.ClaimActivity = func(context.Context, claim.Token, uint64) (supervise.ActivityReport, bool, error) {
				return supervise.ActivityReport{At: time.Now().Add(-time.Hour)}, true, nil
			}
			implementer := sessionKeptClaim(t, sup, claim.RoleImplementer, supervise.StateIdle)
			topic := notify.RoleTopicPrefix + string(implementer.Claim().Token)
			publisher.setAbsent(topic)
			enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", payload, time.Now().Add(-time.Minute)))
			drain(t, runner)
			if left := outboxRows(t, pool); left != 1 {
				t.Fatalf("outbox rows left = %d, want the row held for the claim that keeps its session", left)
			}

			relaunched := make(chan error, 1)
			go func() {
				relaunched <- implementer.Handle(context.Background(), supervise.RequestRelaunch{Claim: implementer.Claim().Token,
					Cause: "Envoy could not reach its session", At: time.Now()})
			}()
			select {
			case <-held.entered:
			case err := <-relaunched:
				t.Fatalf("the relaunch returned %v before its stop reached the runtime", err)
			case <-time.After(10 * time.Second):
				t.Fatal("the relaunch's stop never reached the runtime")
			}
			dueNow(t, pool)
			drain(t, runner)
			left := outboxRows(t, pool)
			close(held.release)
			if err := <-relaunched; err != nil {
				t.Fatalf("the relaunch: %v", err)
			}
			if left != 1 {
				t.Fatalf("outbox rows left = %d after a try during the relaunch's stop, want the row still held", left)
			}
		})
	}
}

func TestARoleMessageForAClaimThatKeepsItsSessionIsHeldUntilItsPhaseMovesOn(t *testing.T) {
	publisher := &holderPublisher{}
	runner, sup, pool := hookRunner(t, publisher)
	implementer := sessionKeptClaim(t, sup, claim.RoleImplementer, supervise.StateWorking)
	publisher.setAbsent(notify.RoleTopicPrefix + string(implementer.Claim().Token))
	message := record.RoleMessage{Role: string(implementer.Claim().Token), Body: "LEGION-208: the head's checks settled green."}
	enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", message, time.Now().Add(-time.Minute)))

	drain(t, runner)
	if left := outboxRows(t, pool); left != 1 {
		t.Fatalf("outbox rows left = %d, want the role message held for the claim that keeps its session", left)
	}

	moveHookIssuePhase(t, pool, runner.records, phase.Testing)
	dueNow(t, pool)
	drain(t, runner)
	if left := outboxRows(t, pool); left != 0 {
		t.Fatalf("outbox rows left = %d, want the role message finished once the implementer's phase moved on", left)
	}
}

// roleMessage reads phase.RoleFor to decide whether a held message's phase moved on, and at
// admitted RoleFor names the architect. The architect holds its role through every phase of its
// issue, so a message held for it stays held at admitted as at any phase; a worker's message held
// while its issue stands at admitted is of a phase that worker does not work, so it is finished
// undelivered, as it was while RoleFor named no role there.
func TestARoleMessageHeldAtAdmittedStaysForTheArchitectAndEndsForAWorker(t *testing.T) {
	for _, tc := range []struct {
		role claim.Role
		left int
	}{
		{role: claim.RoleArchitect, left: 1},
		{role: claim.RoleImplementer, left: 0},
	} {
		t.Run(string(tc.role), func(t *testing.T) {
			publisher := &holderPublisher{}
			runner, sup, pool := hookRunner(t, publisher)
			held := sessionKeptClaim(t, sup, tc.role, supervise.StateWorking)
			publisher.setAbsent(notify.RoleTopicPrefix + string(held.Claim().Token))
			message := record.RoleMessage{Role: string(held.Claim().Token), Body: "LEGION-208: the head's checks settled green."}
			enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", message, time.Now().Add(-time.Minute)))
			drain(t, runner)
			if left := outboxRows(t, pool); left != 1 {
				t.Fatalf("outbox rows left = %d, want the role message held for the %s's claim that keeps its session", left, tc.role)
			}

			moveHookIssuePhase(t, pool, runner.records, phase.Admitted)
			dueNow(t, pool)
			drain(t, runner)
			if left := outboxRows(t, pool); left != tc.left {
				t.Fatalf("outbox rows left = %d at admitted for the %s, want %d", left, tc.role, tc.left)
			}
			if _, delivered := publisher.snapshot(); len(delivered) != 0 {
				t.Fatalf("delivered = %#v, want nothing: no session holds the role", delivered)
			}
		})
	}
}

// A hold ends at the first of its three ends, so a message whose phase moved on, or whose claim
// ended, while it was held is finished undelivered even when a session holds its role again before
// its next try: the next try asks first whether the hold still stands, and publishes only then.
func TestAHeldMessageWhoseHoldEndedIsNotPublishedWhenItsHolderComesBack(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload func(implementer claim.Token) record.OutboxPayload
		end     func(t *testing.T, runner *outbox, pool *store.Pool, implementer *supervise.Machine)
	}{
		{name: "a hook prompt whose phase moved on",
			payload: func(claim.Token) record.OutboxPayload { return firedHook },
			end: func(t *testing.T, runner *outbox, pool *store.Pool, _ *supervise.Machine) {
				moveHookIssuePhase(t, pool, runner.records, phase.Reviewing)
			}},
		{name: "a role message whose phase moved on",
			payload: func(implementer claim.Token) record.OutboxPayload {
				return record.RoleMessage{Role: string(implementer), Body: "LEGION-208: the head's checks settled green."}
			},
			end: func(t *testing.T, runner *outbox, pool *store.Pool, _ *supervise.Machine) {
				moveHookIssuePhase(t, pool, runner.records, phase.Testing)
			}},
		{name: "a hook prompt whose claim ended",
			payload: func(claim.Token) record.OutboxPayload { return firedHook },
			end: func(t *testing.T, _ *outbox, _ *store.Pool, implementer *supervise.Machine) {
				if err := implementer.Handle(context.Background(), supervise.RequestStop{Claim: implementer.Claim().Token}); err != nil {
					t.Fatalf("end the implementer's claim: %v", err)
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			publisher := &holderPublisher{}
			runner, sup, pool := hookRunner(t, publisher)
			implementer := sessionKeptClaim(t, sup, claim.RoleImplementer, supervise.StateWorking)
			publisher.setAbsent(notify.RoleTopicPrefix + string(implementer.Claim().Token))
			enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", tc.payload(implementer.Claim().Token), time.Now().Add(-time.Minute)))
			drain(t, runner)
			if left := outboxRows(t, pool); left != 1 {
				t.Fatalf("outbox rows left = %d, want the message held", left)
			}

			tc.end(t, runner, pool, implementer)
			// A session holds the role again before the row's next try.
			publisher.setAbsent()
			dueNow(t, pool)
			drain(t, runner)
			if _, delivered := publisher.snapshot(); len(delivered) != 0 {
				t.Fatalf("delivered = %#v, want nothing once %s", delivered, tc.name)
			}
			if left := outboxRows(t, pool); left != 0 {
				t.Fatalf("outbox rows left = %d, want the message finished undelivered once %s", left, tc.name)
			}
		})
	}
}

// issueReadsFailing is the outbox's record whose next read of an issue fails once it is armed, as
// a database that does not answer one read does.
type issueReadsFailing struct {
	record.Store
	armed atomic.Bool
}

func (s *issueReadsFailing) Issue(ctx context.Context, tx pgx.Tx, key string) (*record.Issue, error) {
	if s.armed.CompareAndSwap(true, false) {
		return nil, errors.New("the database did not answer the read of the issue")
	}
	return s.Store.Issue(ctx, tx, key)
}

// A hold is a fact of the row's own record, so a try after it that fails for another reason does
// not lose it: a held message whose phase moved on while a try could not read its issue is still
// finished undelivered at the next try, and not published to the holder that came back.
func TestAHeldMessageStaysHeldAcrossATryThatCouldNotReadItsIssue(t *testing.T) {
	publisher := &holderPublisher{}
	runner, sup, pool := hookRunner(t, publisher)
	reads := &issueReadsFailing{Store: runner.records}
	runner.records = reads
	implementer := sessionKeptClaim(t, sup, claim.RoleImplementer, supervise.StateWorking)
	publisher.setAbsent(notify.RoleTopicPrefix + string(implementer.Claim().Token))
	enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", firedHook, time.Now().Add(-time.Minute)))
	drain(t, runner)
	if left := outboxRows(t, pool); left != 1 {
		t.Fatalf("outbox rows left = %d, want the prompt held", left)
	}

	moveHookIssuePhase(t, pool, runner.records, phase.Reviewing)
	publisher.setAbsent()
	reads.armed.Store(true)
	dueNow(t, pool)
	drain(t, runner)
	if left := outboxRows(t, pool); left != 1 {
		t.Fatalf("outbox rows left = %d, want the prompt kept by the try whose read failed", left)
	}
	if reads.armed.Load() {
		t.Fatal("the try after the hold read no issue, so this test proves nothing")
	}

	dueNow(t, pool)
	drain(t, runner)
	if _, delivered := publisher.snapshot(); len(delivered) != 0 {
		t.Fatalf("delivered = %#v, want nothing: the prompt was held when its phase moved on", delivered)
	}
	if left := outboxRows(t, pool); left != 0 {
		t.Fatalf("outbox rows left = %d, want the prompt finished undelivered", left)
	}
}

// flakyPublisher is holderPublisher whose first failures publishes fail as a listener that did not
// answer fails them.
type flakyPublisher struct {
	*holderPublisher
	failures int
}

func (p *flakyPublisher) Publish(ctx context.Context, topic, message string, payload any, key string) error {
	p.mu.Lock()
	failing := p.failures > 0
	p.failures--
	p.mu.Unlock()
	if failing {
		return errors.New("the listener did not answer")
	}
	return p.holderPublisher.Publish(ctx, topic, message, payload, key)
}

// A message its last try did not hold is published as before, whatever its phase: the implementer
// is told of a head's checks while its issue is in testing, and a try that failed for another
// reason was no hold.
func TestAMessageItsLastTryDidNotHoldIsPublishedWhateverItsPhase(t *testing.T) {
	publisher := &flakyPublisher{holderPublisher: &holderPublisher{}, failures: 1}
	runner, sup, pool := hookRunner(t, publisher)
	implementer := sessionKeptClaim(t, sup, claim.RoleImplementer, supervise.StateWorking)
	moveHookIssuePhase(t, pool, runner.records, phase.Testing)
	enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208",
		record.RoleMessage{Role: string(implementer.Claim().Token), Body: "LEGION-208: the head's checks settled green."}, time.Now().Add(-time.Minute)))
	drain(t, runner)
	if left := outboxRows(t, pool); left != 1 {
		t.Fatalf("outbox rows left = %d, want the failed publish kept for its retry", left)
	}
	dueNow(t, pool)
	drain(t, runner)
	if _, delivered := publisher.snapshot(); len(delivered) != 1 {
		t.Fatalf("delivered = %#v, want the message delivered on its retry", delivered)
	}
}

// A role message to a role no claim of this daemon keeps a session for ends as today.
func TestARoleMessageToARoleNoClaimKeepsEndsAsToday(t *testing.T) {
	publisher := &holderPublisher{}
	runner, _, pool := hookRunner(t, publisher)
	token := mustClaimToken(t, "LEGION-208", claim.RoleImplementer)
	publisher.setAbsent(notify.RoleTopicPrefix + string(token))
	enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208",
		record.RoleMessage{Role: string(token), Body: "LEGION-208: the head's checks settled green."}, time.Now().Add(-time.Minute)))

	drain(t, runner)
	if left := outboxRows(t, pool); left != 0 {
		t.Fatalf("outbox rows left = %d, want the role message finished undelivered", left)
	}
}

// A phase-ended message keeps today's end: the role's next task names its phase, so nothing is
// held for it, a claim that keeps its session included.
func TestAPhaseEndedMessageIsNotHeldForAClaimThatKeepsItsSession(t *testing.T) {
	publisher := &holderPublisher{}
	runner, sup, pool := hookRunner(t, publisher)
	implementer := sessionKeptClaim(t, sup, claim.RoleImplementer, supervise.StateWorking)
	publisher.setAbsent(notify.RoleTopicPrefix + string(implementer.Claim().Token))
	ended := record.PhaseEnded{Role: claim.RoleImplementer, From: phase.Implementing, To: phase.Reviewing, Reason: "the head's checks went red"}
	enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", ended, time.Now().Add(-time.Minute)))

	drain(t, runner)
	if left := outboxRows(t, pool); left != 0 {
		t.Fatalf("outbox rows left = %d, want the phase-ended message finished as today", left)
	}
}
