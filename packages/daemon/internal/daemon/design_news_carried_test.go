package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The cases here hold to one rule, the whole way from the fact to the told row: under the take, a
// copy is the news a take's answer carried, written at the accept, and it is credited only once
// that take's turn has started and folded only when a report naming its number finishes it. A
// take renders a task's news again from the record, so a retake can carry an opening an earlier
// take did not. A take the second bound took back carries no credit, so a late turn of its text is
// no take's turn and the news it read is told again, never missed; a report naming the number
// after a retake was accepted finishes the retake and folds the retake's copy (plan.md v138). A
// take that never reached the agent carried nothing, and a task retired before its turn's report
// said the turn ran gives its news back. Each case runs through the daemon's own parts, as the
// newsPathRig cases do.

const (
	// carriedOpened is the opening sentence of the rig's gate at version 4 of its spec.
	carriedOpened = "The design gate of LEGION-800 opened: a person approved version 4 of its spec document (artifact-800)"
	// pathChecks is the Verification document a rig's root registers beside its spec.
	pathChecks = "artifact-800-checks"
)

// reviewerBetweenTurns moves key from testing into reviewing, has its reviewer take the phase task
// that move starts and end its turn on it, and answers the reviewer's machine, connection and
// token. The move queued the reconcile of the tree's news, which has not run: the phase start
// carried none of the news the reviewer is owed, since what changed is not recorded yet.
func (r *newsPathRig) reviewerBetweenTurns(key string) (*supervise.Machine, *newsConn, claim.Token) {
	r.t.Helper()
	token := mustClaimToken(r.t, key, claim.RoleReviewer)
	r.complete(key)
	r.run(startOf(key, claim.RoleReviewer, false))
	reviewer, conn := r.connect(key, claim.RoleReviewer)
	r.delivered(reviewer)
	r.startTurn(reviewer)
	endTurn(r.ctx, r.t, reviewer, token)
	if held := reviewer.Claim().Pending; held != nil {
		r.t.Fatalf("the reviewer of %s holds %+v after its phase task's turn, want nothing: the case's own premise", key, held)
	}
	return reviewer, conn, token
}

// queueNewsTask runs the reconcile the move into reviewing queued, which records what changed and
// queues the news-only start of key's reviewer, and runs that start, which hands the reviewer its
// news task: sent at once to a reviewer between turns, or held behind a turn the reviewer is in.
func (r *newsPathRig) queueNewsTask(key string) {
	r.t.Helper()
	if got := r.run(ofKind(record.OutboxKindDesignChanges)); len(got) == 0 {
		r.t.Fatal("no design_changes row ran, want the one the move into reviewing queued")
	}
	if got := r.run(startOf(key, claim.RoleReviewer, true)); len(got) != 1 {
		r.t.Fatalf("the reviewer's news-only starts = %d, want the one the reconcile queued", len(got))
	}
}

// closeAndReopen is a person requesting changes to version 4 of the root's spec, which closes the
// gate and tells every holder so, and then approving version 4 again, which opens it while the
// reviewer is still owed news, so the reviewer is sent no opened message and its row owes the
// opening.
func (r *newsPathRig) closeAndReopen() {
	r.t.Helper()
	r.dispatchEvent(intake.DispatchArtifactChangesRequested, 4)
	r.dispatchEvent(intake.DispatchArtifactApproved, 4)
	r.run(ofKind(record.OutboxKindRoleMessage))
}

// restartDaemon is the daemon stopped and booted again: a supervisor of its own, with the same
// dependencies and a connection directory of its own, restores every claim the store holds, as the
// boot does (supervisor.restore), and the outbox runs its rows against it from here on.
func (r *newsPathRig) restartDaemon() {
	r.t.Helper()
	st, ok := r.sup.deps.Store.(*store.Store)
	if !ok {
		r.t.Fatalf("the rig's claim store is %T, want the daemon's", r.sup.deps.Store)
	}
	stored, err := st.Claims(r.ctx)
	if err != nil {
		r.t.Fatalf("read the stored claims: %v", err)
	}
	deps := r.sup.deps
	r.sup.stop()
	sup := newSupervisor(context.Background(), nil, "legion", r.t.TempDir(), quietLogger())
	deps.Conns = fake.NewConns()
	sup.deps = deps
	r.t.Cleanup(sup.stop)
	if _, err := sup.restore(r.ctx, stored); err != nil {
		r.t.Fatalf("restore the stored claims: %v", err)
	}
	r.sup, r.conns, r.runner.supervisor = sup, deps.Conns.(*fake.Conns), sup
}

// reconnect is the shim of key's role connecting to the booted daemon, its agent in a turn where
// streaming says so: the connection registers, and the machine and the worker's connection are
// answered. A hello, posted by the caller, makes the claim's view of its connection whole.
func (r *newsPathRig) reconnect(key string, role claim.Role, streaming bool) (*supervise.Machine, *newsConn) {
	r.t.Helper()
	token := mustClaimToken(r.t, key, role)
	machine, found := r.sup.Machine(token)
	if !found {
		r.t.Fatalf("the booted daemon restored no claim of %s's %s", key, role)
	}
	conn := newNewsConn(r.t, r.ctx, r.conns, machine)
	conn.SetStreaming(streaming)
	conn.busy = streaming
	return machine, conn
}

// hello is the shim of machine's claim saying hello on its connection; it waits for what the hello
// decides. A claim holding a pending task asks its agent at the hello whether it is in a turn
// (supervise's askView), and the shim answers that ask on its stream, which the daemon's pump reads
// as a turn answer (supervisor's mapping of stream.TurnAnswered): the fake connection's get_state
// answers only the call, so hello posts that answer as the pump would, from the connection's state.
// An answer no ask waits on changes nothing.
func (r *newsPathRig) hello(machine *supervise.Machine) {
	r.t.Helper()
	held := machine.Claim()
	if err := machine.Handle(r.ctx, supervise.StreamHello{Claim: held.Token, Generation: held.Generation}); err != nil {
		r.t.Fatalf("hello of %s: %v", held.Token, err)
	}
	machine.Wait()
	conn, ok := newsConns.Load(machine)
	if !ok {
		return
	}
	state, err := conn.(*newsConn).Conn.GetState(r.ctx)
	if err != nil {
		r.t.Fatalf("the state of %s's agent: %v", held.Token, err)
	}
	if err := machine.Handle(r.ctx, supervise.StreamTurnAnswered{Claim: held.Token, Streaming: state.IsStreaming}); err != nil {
		r.t.Fatalf("the answer of %s's shim to the hello's ask: %v", held.Token, err)
	}
	machine.Wait()
}

// sweepNews is the entry sweep's backstop for news of the approved design, as its tick runs it
// (queueOwedDesignNews): a design_changes row for each tree owed news that no row will reconcile.
func (r *newsPathRig) sweepNews() {
	r.t.Helper()
	w := &workflowRuntime{pool: r.pool, records: r.records, gatedSteps: config.DesignGateRootIssues.Steps(), dispatchProject: "LEGION",
		log: quietLogger()}
	if err := w.queueOwedDesignNews(r.ctx); err != nil {
		r.t.Fatalf("the sweep's queue of owed news: %v", err)
	}
}

// An earlier daemon accepted the reviewer's take of its news task, the change list of version 4
// and no opening; the daemon stopped before the take's turn started. The copy the accept wrote is
// no standing credit until that turn starts (20261122160000_design_news_credit_at_turn_start), so
// when the gate then closed and opened again the reviewer was still owed that news: its row owes
// the opening and no opened message went. The booted daemon restores the take with the text it
// kept, and the agent answers its hello's ask that it is streaming: over a take awaiting its turn
// that answer is the take's turn start (supervise's answerView), and the turn carried no
// opening, so its start makes the take's credit stand, version 4 taken, and leaves the opening owed.
func TestARestartsConfirmationOfATaskSentWithoutTheOpeningLeavesTheOpeningOwed(t *testing.T) {
	t.Parallel()
	r := newNewsPathRig(t, 4, pathMember{key: pathRoot, at: phase.Testing})
	r.told(pathRoot, record.AudienceHolder, 3)
	r.told(pathRoot, record.AudienceArchitect, 4)
	reviewer, _, _ := r.reviewerBetweenTurns(pathRoot)
	r.queueNewsTask(pathRoot)
	r.delivered(reviewer)
	if news := pendingNewsOf(t, reviewer); len(news.Items) != 1 || len(news.Openings) != 0 {
		t.Fatalf("the reviewer's news task carries %+v, want the item of version 4 and no opening", news)
	}

	r.closeAndReopen()
	r.wantTold("at the reopening", pathRoot, record.AudienceHolder, 3, true)

	r.restartDaemon()
	restored, conn := r.reconnect(pathRoot, claim.RoleReviewer, true)
	r.hello(restored)

	if held := restored.Claim().Pending; held == nil || held.ConfirmedAt.IsZero() || !held.TurnStarted {
		t.Fatalf("the booted daemon's reviewer holds %+v, want the take the earlier daemon accepted, its turn the one the agent is in", held)
	}
	if prompted := conn.Prompts(); len(prompted) != 0 {
		t.Fatalf("the booted daemon's worker took %+v, want nothing taken in the turn of the restored take", prompted)
	}
	r.wantTold("after the restart's confirmation of a task sent without the opening", pathRoot, record.AudienceHolder, 4, true)
}

// The reviewer's take of its news task X, the change list of version 4 and no opening, is
// accepted over a connection whose view is whole, which arms R5's second bound
// (Timeouts.TurnStart), and X's turn does not start within it while Oh My Pi prepares it: the
// worker is charged and the task taken back untaken, under the same id and number. The gate
// closes and opens again meanwhile, so the reviewer's row owes the opening. The worker, which
// handed X's text to its host, takes nothing more (decision 9). X's turn then starts late, and
// under the take it is no take's turn, since the take-back ended X's take and its credit: it
// confirms nothing, and a take while it runs is refused in-turn with no text (the take's
// counterpart of Oh My Pi's busy answer to a resend). The record says version 3 with the opening
// owed: version 4 is under-credited and told twice, never missed, and the opening the task now
// renders is raised by no turn that did not carry it. Once the host is idle the worker takes the
// task again, and that retake leads with the opening. What follows depends on which reaches the
// daemon first, the late turn's report, which names the task's number, or the retake:
//
//   - the late report first: no take holds the number, so it finishes nothing and the record
//     stays at version 3 with the opening owed; the retake's turn and its report then leave
//     version 4 taken and nothing owed;
//   - the retake first: the late report names the number the retake holds, so it finishes the
//     task and folds the retake's copy, the opening included, though the retake's text has not
//     run; the record ends at version 4 with the opening cleared, and the worker still holds the
//     retake's text.
func TestALateTurnOfAFirstPromptLeavesTheOpeningABusyResendRenderedOwed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		retakeFirst bool
	}{
		{name: "the late report lands before the retake"},
		{name: "the retake is accepted before the late report lands", retakeFirst: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newNewsPathRig(t, 4, pathMember{key: pathRoot, at: phase.Testing})
			clock := &heldClock{}
			const wait = 7 * time.Second
			r.sup.deps.Clock, r.sup.deps.Timeouts.TurnStart = clock, wait
			r.told(pathRoot, record.AudienceHolder, 3)
			r.told(pathRoot, record.AudienceArchitect, 4)
			reviewer, conn, token := r.reviewerBetweenTurns(pathRoot)
			r.queueNewsTask(pathRoot)
			first := r.delivered(reviewer)
			if news := pendingNewsOf(t, reviewer); len(news.Items) != 1 || len(news.Openings) != 0 {
				t.Fatalf("the reviewer's news task carries %+v, want the item of version 4 and no opening", news)
			}

			r.closeAndReopen()
			r.wantTold("at the reopening", pathRoot, record.AudienceHolder, 3, true)
			if fired := clock.fire(wait); fired != 1 {
				t.Fatalf("timers fired at the second bound over the first take = %d, want that one wait", fired)
			}
			if held := reviewer.Claim().Pending; held == nil || held.ID != first.ID || held.Number != first.Number || !held.ConfirmedAt.IsZero() {
				t.Fatalf("the reviewer holds %+v after the second bound passed, want the task taken back untaken, its id %s and number %d kept",
					held, first.ID, first.Number)
			}

			// Oh My Pi is still preparing X's turn, so the worker takes nothing until its host is idle.
			conn.SetStreaming(true)
			before := len(conn.Prompts())
			if err := reviewer.Handle(r.ctx, supervise.StreamTurnStart{Claim: token}); err != nil {
				t.Fatalf("the first take's late turn starts: %v", err)
			}
			if state := reviewer.Claim().State; state != supervise.StateWorking {
				t.Fatalf("the reviewer is %s after the first take's late turn started, want working", state)
			}
			c := reviewer.Claim()
			answer, err := reviewer.Take(r.ctx, supervise.RequestTake{Claim: c.Token, Generation: c.Generation, Session: c.Session, Number: first.Number})
			if err != nil || answer.Outcome != supervise.TakeOutcomeRefused || answer.Hold != supervise.HoldInTurn || answer.Text != "" {
				t.Fatalf("a take of task %d while the first take's late turn runs = %+v, %v; want it refused in-turn with no text", first.Number, answer, err)
			}
			reviewer.Wait()
			if held := reviewer.Claim().Pending; held == nil || held.Number != first.Number || !held.ConfirmedAt.IsZero() {
				t.Fatalf("the reviewer holds %+v in the first take's late turn, want the task untaken: the take-back ended that take", held)
			}
			r.wantTold("after the first take's late turn started", pathRoot, record.AudienceHolder, 3, true)

			if err := reviewer.Handle(r.ctx, supervise.StreamTurnEnd{Claim: token}); err != nil {
				t.Fatalf("the first take's late turn ends: %v", err)
			}
			// lateReport is the worker's report of X's late turn, which names the task's number, as
			// the worker ran X's text under it.
			lateReport := func() {
				t.Helper()
				if err := reviewer.Handle(r.ctx, supervise.RequestTurnOutcome{Claim: token, Generation: reviewer.Claim().Generation,
					Sequence: turnSequence.Add(1), Task: first.Number}); err != nil {
					t.Fatalf("the first take's late turn reports task %d: %v", first.Number, err)
				}
				reviewer.Wait()
			}
			if !tc.retakeFirst {
				lateReport()
				if held := reviewer.Claim().Pending; held == nil || held.Number != first.Number || !held.ConfirmedAt.IsZero() {
					t.Fatalf("the reviewer holds %+v after the late turn's report, want the task still owed and untaken: no take holds that turn", held)
				}
				r.wantTold("after the first take's late turn and its report", pathRoot, record.AudienceHolder, 3, true)
			}

			conn.SetStreaming(false)
			resent := promptsAfter(conn, before)
			if len(resent) != 1 || resent[0].DeliveryID != first.ID || !strings.Contains(resent[0].Message, carriedOpened) {
				t.Fatalf("the takes once the host is idle = %+v, want one take of %s rendered with the opening the reopening owes", resent, first.ID)
			}
			if tc.retakeFirst {
				r.wantTold("after the retake was accepted", pathRoot, record.AudienceHolder, 3, true)
				lateReport()
				if held := reviewer.Claim().Pending; held != nil {
					t.Fatalf("the reviewer holds %+v after the late report, want the task finished: the report names the number the retake holds", held)
				}
				if taken := conn.Prompts(); len(taken) != before+1 || taken[before].Message != resent[0].Message {
					t.Fatalf("the worker's takes after the late report = %+v, want the retake's text, opening included, still the last it holds", taken[before:])
				}
				r.wantTold("after the late report finished the retake's task", pathRoot, record.AudienceHolder, 4, false)
				return
			}
			r.startTurn(reviewer)
			endTurn(r.ctx, t, reviewer, token)
			if held := reviewer.Claim().Pending; held != nil {
				t.Fatalf("the reviewer holds %+v after the retake's report, want the news task finished", held)
			}
			r.wantTold("after the retake's turn and its report", pathRoot, record.AudienceHolder, 4, false)
		})
	}
}

// The reviewer is in a turn of its own when the reconcile queues its news task, and the gate
// closes and opens again while the task waits, so the task, sent at the turn's end, carries the
// opening. Before that task's turn starts the gate closes and opens again: the reviewer is told the
// close, and the reopening owes it a new opening, which no message tells. The task's turn confirms
// it: the version it told is raised, and the new opening stays owed. The reviewer is then told the
// reopening by a task of its own, which the sweep's reconcile queues once the reviewer holds no
// task, and that task's confirmation clears it.
func TestAConfirmationOfAnOpeningACloseAndReopeningReplacedLeavesTheReopeningOwedAndItIsTold(t *testing.T) {
	t.Parallel()
	r := newNewsPathRig(t, 4, pathMember{key: pathRoot, at: phase.Testing})
	r.told(pathRoot, record.AudienceHolder, 3)
	r.told(pathRoot, record.AudienceArchitect, 4)
	reviewer, conn, token := r.reviewerBetweenTurns(pathRoot)
	ownTurn(r.ctx, t, reviewer, token)
	r.queueNewsTask(pathRoot)
	r.closeAndReopen()
	before := len(conn.Prompts())
	endTurn(r.ctx, t, reviewer, token)
	sent := r.delivered(reviewer)
	if prompted := promptsAfter(conn, before); len(prompted) != 1 || prompted[0].DeliveryID != sent.ID ||
		!strings.Contains(prompted[0].Message, carriedOpened) {
		t.Fatalf("the reviewer's prompts at its turn's end = %+v, want the news task leading with the opening", prompted)
	}

	r.closeAndReopen()
	r.wantTold("at the second reopening", pathRoot, record.AudienceHolder, 3, true)
	r.startTurn(reviewer)
	r.wantTold("after the confirmation of the task that carried the first opening", pathRoot, record.AudienceHolder, 4, true)

	endTurn(r.ctx, t, reviewer, token)
	before = len(conn.Prompts())
	r.sweepNews()
	r.run(ofKind(record.OutboxKindDesignChanges))
	if got := r.run(startOf(pathRoot, claim.RoleReviewer, true)); len(got) != 1 {
		t.Fatalf("the reviewer's news-only starts after the sweep = %d, want the one that tells the reopening", len(got))
	}
	told := r.delivered(reviewer)
	prompted := promptsAfter(conn, before)
	if len(prompted) != 1 || prompted[0].DeliveryID != told.ID || strings.Count(prompted[0].Message, carriedOpened) != 1 {
		t.Fatalf("the reviewer's prompts after the sweep = %+v, want one task telling the reopening", prompted)
	}
	r.startTurn(reviewer)
	r.wantTold("after the confirmation of the task that told the reopening", pathRoot, record.AudienceHolder, 4, false)
}

// The reviewer's news task is queued while the working copy's adoption is refused, or while the
// reviewer's GitHub App identity cannot be minted. Under the take both are decided before the take
// is answered, so the worker's take is refused and the task stays pending, unconfirmed, its text
// never handed to the agent. The daemon is restarted, and the agent of the booted daemon is in a
// turn of its own, an Envoy message's, when its first turn start is read: that turn is not the
// task's, so it confirms nothing and raises nothing. At the turn's end the worker takes the task,
// and the turn the take starts makes its credit stand.
func TestARestartNeverConfirmsANewsTaskWhoseSendFailedBeforeItsPrompt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		adoption bool
	}{
		{name: "its adoption of the working copy refused", adoption: true},
		{name: "its identity not minted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newNewsPathRig(t, 4, pathMember{key: pathRoot, at: phase.Testing})
			var mintDown atomic.Bool
			r.sup.deps.Identity = func(context.Context, claim.Role, string) (runtime.GitIdentity, error) {
				if mintDown.Load() {
					return runtime.GitIdentity{}, errors.New("the GitHub App answered 503")
				}
				return runtime.GitIdentity{Name: "legion-reviewer[bot]", Email: "reviewer@example.com"}, nil
			}
			r.told(pathRoot, record.AudienceHolder, 3)
			r.told(pathRoot, record.AudienceArchitect, 4)
			reviewer, conn, token := r.reviewerBetweenTurns(pathRoot)
			if tc.adoption {
				r.rt.FailAdoptWorkingCopy(fmt.Errorf("%w: jj is wedged on a stale lock", runtime.ErrAdoptRefused))
			} else {
				mintDown.Store(true)
			}
			before := len(conn.Prompts())
			r.queueNewsTask(pathRoot)
			reviewer.Wait()
			if held := reviewer.Claim().Pending; held == nil || !held.ConfirmedAt.IsZero() {
				t.Fatalf("the reviewer holds %+v after the failed send, want its news task unconfirmed", held)
			}
			if prompted := promptsAfter(conn, before); len(prompted) != 0 {
				t.Fatalf("the reviewer was prompted %+v, want nothing: the send failed before its prompt", prompted)
			}

			r.rt.FailAdoptWorkingCopy(nil)
			mintDown.Store(false)
			r.restartDaemon()
			restored, booted := r.reconnect(pathRoot, claim.RoleReviewer, true)
			if err := restored.Handle(r.ctx, supervise.StreamTurnStart{Claim: token}); err != nil {
				t.Fatalf("a turn of the agent's own starts: %v", err)
			}
			if held := restored.Claim().Pending; held == nil || !held.ConfirmedAt.IsZero() {
				t.Fatalf("the booted daemon's reviewer holds %+v after a turn of the agent's own, want the news task unconfirmed: no prompt of it reached the agent", held)
			}
			r.wantTold("after a turn of the agent's own", pathRoot, record.AudienceHolder, 3, false)

			booted.SetStreaming(false)
			if err := restored.Handle(r.ctx, supervise.StreamTurnEnd{Claim: token}); err != nil {
				t.Fatalf("the agent's own turn ends: %v", err)
			}
			restored.Wait()
			prompted := booted.Prompts()
			sent := r.delivered(restored)
			if len(prompted) != 1 || prompted[0].DeliveryID != sent.ID ||
				!strings.Contains(prompted[0].Message, "version 4, since version 3, which your phase last took") {
				t.Fatalf("the booted daemon's takes = %+v, want the news task, once", prompted)
			}
			r.startTurn(restored)
			r.wantTold("after the turn the news task's prompt started", pathRoot, record.AudienceHolder, 4, false)
		})
	}
}

// The reviewer's news task is confirmed by its turn, which raises what it told. While that turn
// runs, an operator gives the reviewer a task, which takes the confirmed task's place. The turn then
// ends in a provider error, whose result names a task the claim no longer holds, so the fence drops
// it as stale and nothing takes the news back there. The retirement gave it back: the reviewer is
// owed the news again, and once its operator's task is done, the sweep's reconcile sends it again.
func TestAConfirmedNewsTaskANewerTaskReplacedBeforeItsTurnsResultGivesItsNewsBack(t *testing.T) {
	t.Parallel()
	r := newNewsPathRig(t, 4, pathMember{key: pathRoot, at: phase.Testing})
	r.told(pathRoot, record.AudienceHolder, 3)
	r.told(pathRoot, record.AudienceArchitect, 4)
	reviewer, conn, token := r.reviewerBetweenTurns(pathRoot)
	r.queueNewsTask(pathRoot)
	news := r.delivered(reviewer)
	r.startTurn(reviewer)
	r.wantTold("after the news task's confirmation", pathRoot, record.AudienceHolder, 4, false)

	if err := reviewer.Handle(r.ctx, supervise.RequestDeliver{Claim: token, Task: "Say where the review stands."}); err != nil {
		t.Fatalf("the operator's task: %v", err)
	}
	if held := reviewer.Claim().Pending; held == nil || held.ID == news.ID {
		t.Fatalf("the reviewer holds %+v after the operator's task, want that task in the news task's place", held)
	}
	if err := reviewer.Handle(r.ctx, supervise.StreamTurnEnd{Claim: token}); err != nil {
		t.Fatalf("the news task's turn ends: %v", err)
	}
	if err := reviewer.Handle(r.ctx, supervise.RequestTurnOutcome{Claim: token, Generation: reviewer.Claim().Generation,
		Sequence: turnSequence.Add(1), Task: news.Number,
		ProviderError: &supervise.ProviderError{Message: "overloaded_error: Overloaded", Status: 529}}); err != nil {
		t.Fatalf("the news task's turn reports its provider error: %v", err)
	}
	reviewer.Wait()
	r.wantTold("after the provider error the fence dropped", pathRoot, record.AudienceHolder, 3, false)

	r.delivered(reviewer)
	r.startTurn(reviewer)
	endTurn(r.ctx, t, reviewer, token)
	before := len(conn.Prompts())
	r.sweepNews()
	r.run(ofKind(record.OutboxKindDesignChanges))
	if got := r.run(startOf(pathRoot, claim.RoleReviewer, true)); len(got) != 1 {
		t.Fatalf("the reviewer's news-only starts after the sweep = %d, want the one that carries the news again", len(got))
	}
	again := r.delivered(reviewer)
	if prompted := promptsAfter(conn, before); len(prompted) != 1 || prompted[0].DeliveryID != again.ID ||
		!strings.Contains(prompted[0].Message, "version 4, since version 3, which your phase last took") {
		t.Fatalf("the reviewer's prompts after the sweep = %+v, want the news task again, once", prompted)
	}
	r.startTurn(reviewer)
	r.wantTold("after the confirmation of the news sent again", pathRoot, record.AudienceHolder, 4, false)
}

// The reviewer takes its news task and runs the take's turn to its end, but the reviewer's
// machine is handed that turn's end before the turn-outcome report that names the task: the end
// comes over the worker stream and the report over HTTP, in whatever order the two arrive. The
// end finishes nothing, since only the report naming the task's number does (reportedTask), and
// the report, read after the end or before it, says the turn ran, which makes the credit of the
// news final: the reviewer took version 4 once, and the sweep queues no reconcile to tell it
// again. A task finished at that end would have its credit made final with no report to say the
// turn ran, and one taken back there would give the news back and tell the reviewer version 4
// twice. A pause held over that turn waits for the report too (reportwait.go): a stop before it
// would delete the delivery's only credit for version 4. The report folds the credit into the told
// row before the pause stops the claim and the delivery is retired.
func TestANewsTaskWhoseTurnEndsBeforeItsReportIsToldOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		reportFirst bool
		pause       bool
	}{
		{name: "the turn's end, then the report"},
		{name: "the report, then the turn's end", reportFirst: true},
		{name: "a held pause, the turn's end, then the report", pause: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newNewsPathRig(t, 4, pathMember{key: pathRoot, at: phase.Testing})
			// The worker's plugin names its task's number in its reports, so the report alone
			// finishes the task; a plugin below the contract is finished by the end frame.
			r.sup.deps.PluginContract = r.sup.deps.TaskTakeContract
			flag := &orderPause{}
			if tc.pause {
				st := r.sup.deps.Store.(*store.Store)
				pods, err := st.PodRecordsOf("LEGION")
				if err != nil {
					t.Fatal(err)
				}
				pauses := &projectPause{
					store: st, records: projectRecords{Store: r.records, project: "LEGION"},
					pods: pods, project: "LEGION", token: "legion", now: time.Now,
					log: quietLogger(), workflow: &workflowRuntime{},
				}
				r.sup.deps.Paused = flag.Paused
				r.sup.deps.PauseStopped = pauses.stopped
			}
			r.told(pathRoot, record.AudienceHolder, 3)
			r.told(pathRoot, record.AudienceArchitect, 4)
			reviewer, _, token := r.reviewerBetweenTurns(pathRoot)
			r.queueNewsTask(pathRoot)
			news := r.delivered(reviewer)
			r.startTurn(reviewer)
			if tc.pause {
				id := openLegionPause(t, r.pool, time.Now())
				flag.id.Store(id)
				err := reviewer.Handle(r.ctx, supervise.RequestSuspend{Claim: token, By: supervise.SuspendedByReleased, Pause: id})
				if !errors.Is(err, supervise.ErrSuspendHeld) {
					t.Fatalf("pause stop during the news task's turn = %v, want it held", err)
				}
			}
			end := func() {
				if err := reviewer.Handle(r.ctx, supervise.StreamTurnEnd{Claim: token}); err != nil {
					t.Fatalf("the news task's turn ends: %v", err)
				}
			}
			report := func() {
				if err := reviewer.Handle(r.ctx, supervise.RequestTurnOutcome{Claim: token, Generation: reviewer.Claim().Generation,
					Sequence: turnSequence.Add(1), Task: news.Number}); err != nil {
					t.Fatalf("the report of the news task's turn: %v", err)
				}
			}
			if tc.reportFirst {
				report()
				end()
			} else {
				end()
				if held := reviewer.Claim().Pending; held == nil || held.Number != news.Number || held.ConfirmedAt.IsZero() {
					t.Fatalf("the reviewer holds %+v after the turn's end, want the news task %d still taken for its report", held, news.Number)
				}
				if tc.pause && reviewer.Claim().State != supervise.StateIdle {
					t.Fatalf("reviewer state after turn end = %s, want idle until the report decides the task", reviewer.Claim().State)
				}
				report()
			}
			reviewer.Wait()
			if held := reviewer.Claim().Pending; held != nil {
				t.Fatalf("the reviewer holds %+v after its news task's turn and report, want nothing", held)
			}
			if tc.pause {
				if state := reviewer.Claim().State; state != supervise.StateSuspended {
					t.Fatalf("reviewer state after the report = %s, want suspended by the held pause stop", state)
				}
				var pending bool
				if err := r.pool.QueryRow(r.ctx, `select exists (select 1 from pending_task_deliveries where claim_token = $1)`, token).Scan(&pending); err != nil {
					t.Fatalf("read the reviewer's pending delivery: %v", err)
				}
				if pending {
					t.Fatal("the store still holds the delivery, so the told version might only be its temporary credit")
				}
				endLegionPause(t, r.pool, flag.id.Load(), time.Now())
				flag.id.Store(0)
			}
			r.wantTold("after the news task's turn and report", pathRoot, record.AudienceHolder, 4, false)
			r.sweepNews()
			if rows := r.held(ofKind(record.OutboxKindDesignChanges)); len(rows) != 0 {
				t.Fatalf("the sweep queued %d reconciles of the tree's news, want none: the reviewer took version 4 once", len(rows))
			}
		})
	}
}

// checksEvent applies a Dispatch event of the root's Verification document (pathChecks) through
// the engine and then admission, as dispatchEvent applies one of its spec.
func (r *newsPathRig) checksEvent(kind intake.DispatchArtifactKind, version int) {
	r.t.Helper()
	r.marks++
	fact := intake.DispatchArtifact{Key: r.root, ArtifactID: pathChecks, Kind: kind, Version: version, Mark: record.EventMark{Seq: r.marks}}
	if kind == intake.DispatchArtifactApproved {
		fact.Actor = dispatch.Actor{Kind: "user", ID: "alice"}
	}
	applyFact(r.t, r.pool, "dispatch", fmt.Sprintf("checks-%s-%d-%d", kind, version, r.marks), fact, r.engine, r.admission)
}

// A child's reviewer holds a news task (spec 3 to 4, no opening), waiting behind a turn of its
// own. A Verification version closes the gate and its approval opens it while the reviewer is
// still owed spec 4, so its row owes the opening. Another Verification version closes the gate,
// which settles that opening, and the news task is sent while the gate is closed, so it says
// nothing of an opening, and its confirmation raises spec to 4. The Verification document is
// approved: the reviewer is owed nothing now, so it is sent the opened message, which settles the
// opening too. A version of the spec that changes decision blocks alone, which Dispatch carries
// approved, then sends the reviewer nothing: the reconcile raises what it took.
func TestAnOpenedMessageAndACloseSettleTheOpeningSoADecisionOnlyVersionSendsNothing(t *testing.T) {
	t.Parallel()
	r := newNewsPathRig(t, 4, pathMember{key: pathRoot, at: phase.Implementing},
		pathMember{key: pathChild, parent: pathRoot, at: phase.Testing})
	r.inTx(func(tx pgx.Tx) error {
		if err := r.records.PutGate(r.ctx, tx, record.DesignGate{Issue: r.root, Step: record.DesignStepCriteria, ArtifactID: pathChecks,
			LatestVersion: 1, ApprovedVersion: new(1), ApprovedMark: record.EventMark{Seq: r.marks}, EverOpened: true}); err != nil {
			return err
		}
		for _, told := range []struct {
			issue    string
			audience record.DesignAudience
			versions map[record.DesignStep]int
		}{
			{pathRoot, record.AudienceHolder, map[record.DesignStep]int{record.DesignStepSpec: 4, record.DesignStepCriteria: 1}},
			{pathRoot, record.AudienceArchitect, map[record.DesignStep]int{record.DesignStepSpec: 4, record.DesignStepCriteria: 1}},
			{pathChild, record.AudienceHolder, map[record.DesignStep]int{record.DesignStepSpec: 3}},
		} {
			if err := r.records.RaiseTold(r.ctx, tx, told.issue, told.audience, raisedTo(told.versions), time.Now()); err != nil {
				return err
			}
		}
		return nil
	})

	// Step 1: the news task waits behind a turn of the reviewer's own.
	reviewer, conn, token := r.reviewerBetweenTurns(pathChild)
	ownTurn(r.ctx, t, reviewer, token)
	r.queueNewsTask(pathChild)
	if news := pendingNewsOf(t, reviewer); len(news.Items) != 1 || news.Items[0].To != 4 || len(news.Openings) != 0 {
		t.Fatalf("the news task carries %+v, want spec 4 and no opening", news)
	}

	// Step 2: a Verification version closes the gate, and its approval opens it.
	r.checksEvent(intake.DispatchArtifactVersion, 2)
	r.checksEvent(intake.DispatchArtifactApproved, 2)
	r.run(ofKind(record.OutboxKindRoleMessage))
	r.wantTold("at the opening", pathChild, record.AudienceHolder, 3, true)

	// Step 3: another Verification version closes the gate, and the news task is sent while it is
	// closed.
	r.checksEvent(intake.DispatchArtifactVersion, 3)
	r.run(ofKind(record.OutboxKindRoleMessage))
	before := len(conn.Prompts())
	endTurn(r.ctx, t, reviewer, token)
	sent := r.delivered(reviewer)
	if prompted := promptsAfter(conn, before); len(prompted) != 1 || prompted[0].DeliveryID != sent.ID || strings.Contains(prompted[0].Message, "opened") {
		t.Fatalf("the reviewer's prompts while the gate is closed = %+v, want the news task, saying nothing of an opening", prompted)
	}
	r.startTurn(reviewer)
	endTurn(r.ctx, t, reviewer, token)
	if took, _ := r.toldOf(pathChild, record.AudienceHolder).Took(record.DesignStepSpec); took != 4 {
		t.Fatalf("the reviewer took spec %d after the news task's confirmation, want 4", took)
	}

	// Step 4: the Verification document is approved, and the reviewer, owed nothing, is sent the
	// opened message.
	r.checksEvent(intake.DispatchArtifactApproved, 3)
	r.run(ofKind(record.OutboxKindRoleMessage))
	if published := r.publishedTo(token); len(published) == 0 || !strings.HasPrefix(published[len(published)-1], "The design gate of LEGION-800 opened") {
		t.Fatalf("the reviewer's role messages = %q, want the opened message last", published)
	}

	// Step 5: a decision-only version of the spec, which Dispatch carries approved, sends nothing.
	r.client.answer = func(pair record.VersionPair) (dispatch.ArtifactChanges, error) {
		if pair.ArtifactID == pathDoc && pair.To == 5 {
			return dispatch.ArtifactChanges{ArtifactID: pair.ArtifactID, From: pair.From, To: pair.To, DecisionOnly: true}, nil
		}
		return dispatch.ArtifactChanges{ArtifactID: pair.ArtifactID, From: pair.From, To: pair.To,
			Sections: []dispatch.SectionChange{{Heading: "Checks", Change: dispatch.ChangeChanged}}}, nil
	}
	mark, before := r.lastRowID(), len(conn.Prompts())
	r.dispatchEvent(intake.DispatchArtifactApproved, 5)
	r.dispatchEvent(intake.DispatchArtifactVersion, 5)
	r.run(ofKind(record.OutboxKindDesignChanges))
	r.run(ofKind(record.OutboxKindRoleMessage))
	if addressed := r.seenSince(mark, addressedTo(pathChild, claim.RoleReviewer, token)); len(addressed) != 0 {
		t.Fatalf("outbox rows addressed to the reviewer after a carried decision-only version = %+v, want none", addressed)
	}
	if prompted := promptsAfter(conn, before); len(prompted) != 0 {
		t.Fatalf("the reviewer's prompts after a carried decision-only version = %+v, want none", prompted)
	}
	r.wantTold("after the carried decision-only version", pathChild, record.AudienceHolder, 5, false)
}

// A restart and a close and reopening together. The reviewer's news task carries the opening, and
// the reviewer's take of it is accepted; before its turn starts, the gate closes and opens again,
// and the daemon is restarted. The take's copy is no standing credit until its turn starts, so the
// reopening finds the reviewer owed and its row owes the new opening. The booted daemon restores
// the take, and the agent's answer to its hello's ask says it is streaming, which over a take
// awaiting its turn is that take's turn start (supervise's answerView): its start makes the
// credit stand, version 4 taken, and the earlier opening it carried clears nothing of the new one.
// The turn ends and the worker's report naming the take's number finishes the task, which makes
// that credit final (main's push path ended this turn with no result the booted daemon waited on,
// so its retirement gave the version back; under the take every turn's report reaches the daemon,
// and a clean one is the proof the turn ran). The sweep's reconcile then queues a task that tells
// the reopening alone, and the finish of its take's turn clears it.
func TestARestartBetweenAReopeningAndTheTurnOfATaskThatCarriedTheEarlierOpening(t *testing.T) {
	t.Parallel()
	r := newNewsPathRig(t, 4, pathMember{key: pathRoot, at: phase.Testing})
	r.told(pathRoot, record.AudienceHolder, 3)
	r.told(pathRoot, record.AudienceArchitect, 4)
	reviewer, _, token := r.reviewerBetweenTurns(pathRoot)
	ownTurn(r.ctx, t, reviewer, token)
	r.queueNewsTask(pathRoot)
	r.closeAndReopen()
	endTurn(r.ctx, t, reviewer, token)
	r.delivered(reviewer)
	if news := pendingNewsOf(t, reviewer); len(news.Openings) != 1 {
		t.Fatalf("the reviewer's news task carries %+v, want the opening", news)
	}

	r.closeAndReopen()
	r.restartDaemon()
	restored, conn := r.reconnect(pathRoot, claim.RoleReviewer, true)
	r.hello(restored)
	if held := restored.Claim().Pending; held == nil || held.ConfirmedAt.IsZero() || !held.TurnStarted {
		t.Fatalf("the booted daemon's reviewer holds %+v, want the take the earlier daemon accepted, its turn the one the agent is in", held)
	}
	r.wantTold("after the restart's confirmation of the task that carried the earlier opening", pathRoot, record.AudienceHolder, 4, true)

	conn.SetStreaming(false)
	// Under the take, endTurn sends the end frame and then the worker's report naming the take's
	// number, which finishes the task and folds its copy into what the holder was told: without
	// that report nothing folds.
	endTurn(r.ctx, t, restored, token)
	r.wantTold("after the take's turn and its clean report", pathRoot, record.AudienceHolder, 4, true)

	r.sweepNews()
	r.run(ofKind(record.OutboxKindDesignChanges))
	if got := r.run(startOf(pathRoot, claim.RoleReviewer, true)); len(got) != 1 {
		t.Fatalf("the reviewer's news-only starts after the sweep = %d, want one", len(got))
	}
	prompted := conn.Prompts()
	told := r.delivered(restored)
	if len(prompted) != 1 || prompted[0].DeliveryID != told.ID ||
		strings.Count(prompted[0].Message, carriedOpened) != 1 || strings.Contains(prompted[0].Message, "version 4, since version 3") {
		t.Fatalf("the booted daemon's worker took %+v, want one task telling the reopening alone: version 4 was taken", prompted)
	}
	r.startTurn(restored)
	endTurn(r.ctx, t, restored, token)
	r.wantTold("after the finish of the task that told the reopening", pathRoot, record.AudienceHolder, 4, false)
}
