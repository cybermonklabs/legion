package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The cases here put the shim's own delivery dedupe (shimwire.Dedupe) between the daemon and the
// agent, as every real worker connection has it: a send under a delivery id the shim already
// carried is answered from the shim's record and never reaches Oh My Pi. So what a turn's
// confirmation records as told must be the news of the send under that id that reached the agent,
// never a rendering the daemon made later and sent under the same id. The daemon gives every new
// rendering of a task's news a new id (supervise's currentNews), so the shim forwards it, and Oh My
// Pi takes it or answers it busy.

// shimConn is a worker connection through the shim's delivery dedupe: a send reaches the agent —
// the embedded fake connection, which stands for Oh My Pi — only where the dedupe forwards it, and
// the agent's answer to it advances the dedupe's record, as the shim's pump does
// (shimwire.Dedupe.AgentFrame). A send the dedupe answers itself returns that answer and reaches
// nobody. The dedupe is the shim's and outlives a daemon restart, as the shim process does. Where
// lose is set, the agent takes the prompt and its answer is lost on the way back to the daemon.
type shimConn struct {
	*fake.Conn
	dedupe   shimwire.Dedupe
	requests atomic.Int64
	lose     atomic.Bool
}

func (s *shimConn) Prompt(ctx context.Context, deliveryID, message string) error {
	request := fmt.Sprintf("request-%d", s.requests.Add(1))
	forward, replies := s.dedupe.DaemonFrame(shimwire.Prompt{ID: request, DeliveryID: deliveryID, Message: message})
	if !forward {
		for _, reply := range replies {
			if answer, ok := reply.(shimwire.Response); ok && answer.Success {
				return nil
			}
		}
		return errors.New("the shim holds the send until the agent answers the first one")
	}
	err := s.Conn.Prompt(ctx, deliveryID, message)
	s.dedupe.AgentFrame(shimwire.Response{ID: request, Command: shimwire.TypePrompt, Success: err == nil})
	if err == nil && s.lose.Load() {
		return errors.New("the connection closed before the agent's answer came back")
	}
	return err
}

// sentToTheAgent is each prompt the shim forwarded to the agent, in order.
func (s *shimConn) sentToTheAgent() []fake.Prompt { return s.Prompts() }

// The earlier prompt's turn, which Oh My Pi starts with no delivery id, and that prompt's result,
// which says the turn ran: what each of the two paths below ends on.
func earlierPromptsTurn(t *testing.T, r *newsPathRig, machine *supervise.Machine, earlier string) {
	t.Helper()
	token := machine.Claim().Token
	if err := machine.Handle(r.ctx, supervise.StreamTurnStart{Claim: token}); err != nil {
		t.Fatalf("the earlier prompt's turn starts: %v", err)
	}
	if err := machine.Handle(r.ctx, supervise.StreamPromptResult{Claim: token, DeliveryID: earlier, AgentInvoked: true}); err != nil {
		t.Fatalf("the earlier prompt's result: %v", err)
	}
	machine.Wait()
}

// theReopeningIsToldByTheTaskSentAgain ends the earlier prompt's turn, sends the task again at its
// end, where the agent takes it, and has its turn confirm it: that task leads with the opening the
// reopening owes, and its confirmation raises the version and clears the opening.
func theReopeningIsToldByTheTaskSentAgain(t *testing.T, r *newsPathRig, machine *supervise.Machine, shim *shimConn) {
	t.Helper()
	shim.FailPrompt(nil)
	before := len(shim.sentToTheAgent())
	endTurn(r.ctx, t, machine, machine.Claim().Token)
	// The task can hold an earlier prompt's read mark, so the send's own outcome is waited for
	// rather than the mark.
	machine.Wait()
	sent := machine.Claim().Pending
	if sent == nil || sent.MarkedBy != sent.ID {
		t.Fatalf("the reviewer holds %+v after its turn's end, want the task sent again and acknowledged under its id", sent)
	}
	reached := shim.sentToTheAgent()[before:]
	if len(reached) != 1 || reached[0].DeliveryID != sent.ID || !newsTells(t, reached[0].Message, carriedOpened) {
		t.Fatalf("the prompts that reached the agent at the turn's end = %+v, want the task under %s leading with the opening", reached, sent.ID)
	}
	r.startTurn(machine)
	r.wantTold("after the confirmation of the task the agent took with the opening", pathRoot, record.AudienceHolder, 4, false)
}

// Path A: a restart. An earlier daemon's prompt X of the reviewer's news task, the change list of
// version 4 and no opening, reached the agent and was acknowledged, and Oh My Pi has admitted it and
// not started its turn when that daemon stops. The gate closes and opens again while the reviewer is
// still owed that news, so its row owes a new opening. The booted daemon renders the task again with
// the opening, asks the agent, which answers that it is not streaming, and sends the task. Under X
// the shim answers that send from its record and the agent never reads it, so the turn that starts
// is X's, with no opening; the daemon then records nothing of the opening for it. X's turn and its
// result leave the reviewer owed the opening, and the task the agent does take is told it.
func TestARestartsResendTheShimAnswersFromItsRecordRecordsNoNewsTheAgentNeverRead(t *testing.T) {
	t.Parallel()
	r := newNewsPathRig(t, 4, pathMember{key: pathRoot, at: phase.Testing})
	r.told(pathRoot, record.AudienceHolder, 3)
	r.told(pathRoot, record.AudienceArchitect, 4)
	reviewer, _, token := r.reviewerBetweenTurns(pathRoot)
	shim := &shimConn{Conn: fake.NewConn()}
	r.conns.Register(token, shim)
	r.queueNewsTask(pathRoot)
	first := r.delivered(reviewer)
	if reached := shim.sentToTheAgent(); len(reached) != 1 || reached[0].DeliveryID != first.ID || newsTells(t, reached[0].Message, carriedOpened) {
		t.Fatalf("the prompts that reached the agent = %+v, want the news task %s, without an opening", reached, first.ID)
	}

	r.closeAndReopen()
	r.wantTold("at the reopening", pathRoot, record.AudienceHolder, 3, true)
	r.restartDaemon()
	restored, found := r.sup.Machine(token)
	if !found {
		t.Fatalf("the booted daemon restored no claim of %s", token)
	}
	// Oh My Pi is preparing X's turn: it answers the question that it is not streaming, and answers
	// any prompt that reaches it now busy.
	shim.RefusePrompt(carriedBusy)
	r.conns.Register(token, shim)
	r.hello(restored)

	earlierPromptsTurn(t, r, restored, first.ID)
	r.wantTold("after the turn and the result of the prompt the earlier daemon sent", pathRoot, record.AudienceHolder, 3, true)
	theReopeningIsToldByTheTaskSentAgain(t, r, restored, shim)
}

// Path B: one daemon. The reviewer's news task X, the change list of version 4 and no opening,
// reaches the agent, and the agent's answer is lost on the way back, so the task keeps its id for
// the next send (supervise's refused). The gate closes and opens again, so the reviewer's row owes a
// new opening. The reconnect's hello renders the task again with the opening and sends it. Under X
// the shim answers that send from its record, so the turn that starts is X's, with no opening; the
// daemon then records nothing of the opening for it. X's turn and its result leave the reviewer owed
// the opening, and the task the agent does take is told it.
func TestAResendAfterALostAnswerTheShimAnswersFromItsRecordRecordsNoNewsTheAgentNeverRead(t *testing.T) {
	t.Parallel()
	r := newNewsPathRig(t, 4, pathMember{key: pathRoot, at: phase.Testing})
	r.told(pathRoot, record.AudienceHolder, 3)
	r.told(pathRoot, record.AudienceArchitect, 4)
	reviewer, _, token := r.reviewerBetweenTurns(pathRoot)
	shim := &shimConn{Conn: fake.NewConn()}
	shim.lose.Store(true)
	r.conns.Register(token, shim)
	r.queueNewsTask(pathRoot)
	reviewer.Wait()
	first := reviewer.Claim().Pending
	if first == nil || !first.ConfirmedAt.IsZero() || !first.DeliveredAt.IsZero() {
		t.Fatalf("the reviewer holds %+v after the lost answer, want its news task waiting, unread", first)
	}
	if reached := shim.sentToTheAgent(); len(reached) != 1 || reached[0].DeliveryID != first.ID || newsTells(t, reached[0].Message, carriedOpened) {
		t.Fatalf("the prompts that reached the agent = %+v, want the news task %s, without an opening", reached, first.ID)
	}

	r.closeAndReopen()
	r.wantTold("at the reopening", pathRoot, record.AudienceHolder, 3, true)
	// Oh My Pi is preparing X's turn, and answers any prompt that reaches it now busy.
	shim.lose.Store(false)
	shim.RefusePrompt(carriedBusy)
	r.hello(reviewer)

	earlierPromptsTurn(t, r, reviewer, first.ID)
	r.wantTold("after the turn and the result of the prompt whose answer was lost", pathRoot, record.AudienceHolder, 3, true)
	theReopeningIsToldByTheTaskSentAgain(t, r, reviewer, shim)
}
