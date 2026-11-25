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
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The cases here hold to one rule, the whole way from the fact to the told row: a confirmation
// raises what the prompt of the turn it confirms carried, never what the task holds when the turn
// starts. A send renders a task's news again from the record, so a later send can carry an opening
// an earlier prompt did not, and the turn that starts can be that earlier prompt's: after a
// restart, after a resend Oh My Pi answers busy, or after the gate closed and opened again. A send
// that never reached the agent carried nothing, and a task retired before its turn's result said
// the turn ran gives its news back. Each case runs through the daemon's own parts, as the
// newsPathRig cases do.

const (
	// carriedBusy is Oh My Pi's answer to a prompt while another prompt's turn is being prepared
	// or run.
	carriedBusy = "Agent is already processing. Use steer() or followUp() to queue messages, or wait for completion."
	// carriedOpened is the opening sentence of the rig's gate at version 4 of its spec.
	carriedOpened = "The design gate of LEGION-800 opened: a person approved version 4 of its spec document (artifact-800)"
	// pathChecks is the Verification document a rig's root registers beside its spec.
	pathChecks = "artifact-800-checks"
)

// reviewerBetweenTurns moves key from testing into reviewing, has its reviewer take the phase task
// that move starts and end its turn on it, and answers the reviewer's machine, connection and
// token. The move queued the reconcile of the tree's news, which has not run: the phase start
// carried none of the news the reviewer is owed, since what changed is not recorded yet.
func (r *newsPathRig) reviewerBetweenTurns(key string) (*supervise.Machine, *fake.Conn, claim.Token) {
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
// streaming says so: the connection registers, and the machine and the connection are answered. A
// hello, posted by the caller, is what sends the task the claim holds.
func (r *newsPathRig) reconnect(key string, role claim.Role, streaming bool) (*supervise.Machine, *fake.Conn) {
	r.t.Helper()
	token := mustClaimToken(r.t, key, role)
	machine, found := r.sup.Machine(token)
	if !found {
		r.t.Fatalf("the booted daemon restored no claim of %s's %s", key, role)
	}
	conn := fake.NewConn()
	conn.SetStreaming(streaming)
	r.conns.Register(token, conn)
	return machine, conn
}

// hello is the shim of machine's claim saying hello on its connection, which sends what the claim
// holds; it waits for the send's outcome.
func (r *newsPathRig) hello(machine *supervise.Machine) {
	r.t.Helper()
	held := machine.Claim()
	if err := machine.Handle(r.ctx, supervise.StreamHello{Claim: held.Token, Generation: held.Generation}); err != nil {
		r.t.Fatalf("hello of %s: %v", held.Token, err)
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

// An earlier daemon sent the reviewer its news task, the change list of version 4 and no opening,
// and Oh My Pi acknowledged it; the daemon stopped before the task's turn started. The gate then
// closed and opened again while the reviewer was still owed that news, so its row owes the opening
// and no opened message went. The booted daemon's send renders the task again, which takes the
// owed opening on, and only then asks the agent whether the turn it is in is the task's. The agent
// answers that it is streaming, and that turn is the earlier prompt's, which carried no opening:
// the confirmation raises the version that prompt told and leaves the opening owed.
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

	if held := restored.Claim().Pending; held == nil || held.ConfirmedAt.IsZero() {
		t.Fatalf("the booted daemon's reviewer holds %+v, want the task the earlier daemon sent confirmed by the turn the agent is in", held)
	}
	if prompted := conn.Prompts(); len(prompted) != 0 {
		t.Fatalf("the booted daemon prompted %+v, want nothing sent into the turn of the earlier prompt", prompted)
	}
	r.wantTold("after the restart's confirmation of a task sent without the opening", pathRoot, record.AudienceHolder, 4, true)
}

// The reviewer's news task, the change list of version 4 and no opening, is acknowledged, and its
// turn does not start within the wait (noTurn): the task is taken back under a new id, and the
// first prompt stays outstanding. The gate closes and opens again meanwhile, so the reviewer's row
// owes the opening. The next trigger sends the task again, rendered with the opening, and Oh My Pi
// answers that send busy, since it is preparing the first prompt's turn. That turn starts: it
// confirms the task, and raises what the first prompt carried, which said nothing of the opening.
func TestALateTurnOfAFirstPromptLeavesTheOpeningABusyResendRenderedOwed(t *testing.T) {
	t.Parallel()
	r := newNewsPathRig(t, 4, pathMember{key: pathRoot, at: phase.Testing})
	clock := &heldClock{}
	const wait = 7 * time.Second
	r.sup.deps.Clock, r.sup.deps.Timeouts.RPC = clock, wait
	r.told(pathRoot, record.AudienceHolder, 3)
	r.told(pathRoot, record.AudienceArchitect, 4)
	reviewer, conn, token := r.reviewerBetweenTurns(pathRoot)
	r.queueNewsTask(pathRoot)
	first := r.delivered(reviewer)

	r.closeAndReopen()
	r.wantTold("at the reopening", pathRoot, record.AudienceHolder, 3, true)
	if fired := clock.fire(wait); fired != 1 {
		t.Fatalf("timers fired at the wait for the first prompt's turn = %d, want that one wait", fired)
	}
	if held := reviewer.Claim().Pending; held == nil || held.ID == first.ID || !held.ConfirmedAt.IsZero() {
		t.Fatalf("the reviewer holds %+v after the wait ran out, want the task taken back under a new id", held)
	}

	conn.RefusePrompt(carriedBusy)
	before := len(conn.Prompts())
	r.hello(reviewer)
	resent := promptsAfter(conn, before)
	if len(resent) != 1 || !strings.Contains(resent[0].Message, carriedOpened) {
		t.Fatalf("the resend = %+v, want one prompt rendered with the opening the reopening owes", resent)
	}
	if state := reviewer.Claim().State; state != supervise.StateWorking {
		t.Fatalf("the reviewer is %s after the busy answer, want working for the first prompt's turn", state)
	}

	if err := reviewer.Handle(r.ctx, supervise.StreamTurnStart{Claim: token}); err != nil {
		t.Fatalf("the first prompt's turn starts: %v", err)
	}
	if held := reviewer.Claim().Pending; held == nil || held.ConfirmedAt.IsZero() {
		t.Fatalf("the reviewer holds %+v after the first prompt's turn started, want the task confirmed", held)
	}
	r.wantTold("after the first prompt's late turn", pathRoot, record.AudienceHolder, 4, true)
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

// The reviewer's news task is sent while the working copy's adoption is refused, or while the
// reviewer's GitHub App identity cannot be minted, so its prompt never reaches the agent, though
// the send marked the task prompted. The daemon is restarted, and the agent of the booted daemon
// is in a turn of its own, an Envoy message's, when its first turn start is read: that turn is not
// the task's, so it confirms nothing and raises nothing. The turn's end sends the task, and the
// turn its prompt starts confirms it.
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
			sent := r.delivered(restored)
			if prompted := booted.Prompts(); len(prompted) != 1 || prompted[0].DeliveryID != sent.ID ||
				!strings.Contains(prompted[0].Message, "version 4, since version 3, which your phase last took") {
				t.Fatalf("the booted daemon's prompts = %+v, want the news task, once", prompted)
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
	if err := reviewer.Handle(r.ctx, supervise.StreamPromptResult{Claim: token, DeliveryID: news.ID, AgentInvoked: true,
		Error: "overloaded_error: Overloaded", HTTPStatus: 529}); err != nil {
		t.Fatalf("the news task's prompt reports its provider error: %v", err)
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

// The reviewer's news task is sent, and Oh My Pi takes its prompt and runs its turn to its end,
// but the reviewer's machine is handed that turn's start and end before the send's
// acknowledgement: the stream's frames reach it on the stream's queue, and the acknowledgement on
// the send's own goroutine, in whatever order the two are scheduled. The turn's start is taken for
// the task's while the send is in flight, which confirms it, and its end retires nothing while
// that send has had no answer. The prompt's result, decided after the acknowledgement or before
// it, says the turn ran, which makes the credit of the news final: the reviewer took version 4
// once, and the sweep queues no reconcile to tell it again. A task retired at that end has its
// result dropped as stale and gives the news back, and the reviewer is told version 4 twice.
func TestANewsTaskWhoseTurnEndsBeforeItsSendIsAcknowledgedIsToldOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		resultFirst bool
	}{
		{name: "the acknowledgement, then the result"},
		{name: "the result, then the acknowledgement", resultFirst: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newNewsPathRig(t, 4, pathMember{key: pathRoot, at: phase.Testing})
			// The case holds the news task's send at the door while its turn starts and ends, so
			// the send's own deadline is a hang guard here and no part of what the case reads.
			r.sup.deps.Timeouts.RPC = testwait.Remaining(t)
			r.told(pathRoot, record.AudienceHolder, 3)
			r.told(pathRoot, record.AudienceArchitect, 4)
			reviewer, _, token := r.reviewerBetweenTurns(pathRoot)
			gated := newGatedPrompt()
			r.conns.Register(token, gated)
			r.queueNewsTask(pathRoot)
			news := testwait.Receive(t, "the send of the reviewer's news task", gated.entered)

			if err := reviewer.Handle(r.ctx, supervise.StreamTurnStart{Claim: token}); err != nil {
				t.Fatalf("the news task's turn starts: %v", err)
			}
			if held := reviewer.Claim().Pending; held == nil || held.ID != news || held.ConfirmedAt.IsZero() {
				t.Fatalf("the reviewer holds %+v after its turn started, want the news task %s confirmed: its send is in flight", held, news)
			}
			if err := reviewer.Handle(r.ctx, supervise.StreamTurnEnd{Claim: token}); err != nil {
				t.Fatalf("the news task's turn ends: %v", err)
			}
			acknowledge := func() {
				gated.release <- struct{}{}
				reviewer.Wait()
			}
			result := func() {
				if err := reviewer.Handle(r.ctx, supervise.StreamPromptResult{Claim: token, DeliveryID: news, AgentInvoked: true}); err != nil {
					t.Fatalf("the news task's prompt reports that its turn ran: %v", err)
				}
			}
			if tc.resultFirst {
				result()
				acknowledge()
			} else {
				acknowledge()
				result()
			}
			reviewer.Wait()
			if held := reviewer.Claim().Pending; held != nil {
				t.Fatalf("the reviewer holds %+v after its news task's result and acknowledgement, want nothing", held)
			}
			r.wantTold("after the news task's result and acknowledgement", pathRoot, record.AudienceHolder, 4, false)
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
// Oh My Pi acknowledges it; before its turn starts, the gate closes and opens again, and the daemon
// is restarted. The booted daemon's send renders the task again with the new opening, and asks the
// agent, which is in the earlier prompt's turn: that turn's confirmation raises what that prompt
// carried and leaves the new opening owed. The turn ends with no result the booted daemon was
// waiting on, so its retirement gives that news back, told twice rather than lost, and the next
// task, which the sweep's reconcile queues, carries the version and the reopening both; its
// confirmation raises the one and clears the other.
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
	if held := restored.Claim().Pending; held == nil || held.ConfirmedAt.IsZero() {
		t.Fatalf("the booted daemon's reviewer holds %+v, want the task the earlier daemon sent confirmed", held)
	}
	r.wantTold("after the restart's confirmation of the task that carried the earlier opening", pathRoot, record.AudienceHolder, 4, true)

	conn.SetStreaming(false)
	endTurn(r.ctx, t, restored, token)
	r.wantTold("after the turn ended with no result the booted daemon waited on", pathRoot, record.AudienceHolder, 3, true)

	r.sweepNews()
	r.run(ofKind(record.OutboxKindDesignChanges))
	if got := r.run(startOf(pathRoot, claim.RoleReviewer, true)); len(got) != 1 {
		t.Fatalf("the reviewer's news-only starts after the sweep = %d, want one", len(got))
	}
	told := r.delivered(restored)
	if prompted := conn.Prompts(); len(prompted) != 1 || prompted[0].DeliveryID != told.ID ||
		strings.Count(prompted[0].Message, carriedOpened) != 1 || !strings.Contains(prompted[0].Message, "version 4, since version 3") {
		t.Fatalf("the booted daemon's prompts = %+v, want one task telling the reopening and version 4", prompted)
	}
	r.startTurn(restored)
	r.wantTold("after the confirmation of the task that told the reopening", pathRoot, record.AudienceHolder, 4, false)
}
