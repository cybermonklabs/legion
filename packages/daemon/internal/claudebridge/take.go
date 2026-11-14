package claudebridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// A claim's task reaches its worker as a value the worker takes, not as a prompt the daemon pushes
// (dispatch://INFRA-2811 R2). claude-envoy, which holds the claim and its Envoy consumer, hands the
// bridge every value it is handed — a relayed task frame, the registration's value, a read of
// claims/task at a new epoch of its relay or a regain of its role — as a task line of the inbox
// (inbox.go). The bridge, which knows whether Claude Code is in a turn, does the rest, naming the
// claim with the launch's boot token (daemonLink):
//
//   - it reports each value seen at once, whatever Claude Code is doing (claims/task-seen);
//   - when Claude Code is in no turn and the seed is done, it asks the daemon to take the number the
//     newest value names, once per (epoch, revision) (claims/take). The pairs it asked at are kept
//     in this process's memory alone, so a relaunched bridge asks again at the value it is handed;
//   - while that take waits for its answer, an Envoy delivery that would start a turn waits in the
//     queue, since the turn the take goes into must not start with a card;
//   - an accepted take's text starts a turn of its own ahead of every queued delivery, and that
//     turn is the task's: its outcome report names the number (startTask, turnoutcome.go);
//   - a take accepted while a turn runs — one Claude Code started by itself while the take was in
//     flight — holds its text until that turn ends and starts it then, ahead of the queue, as Oh My
//     Pi's taker holds an accepted text until the run's agent_end
//     (packages/pi-shared/src/task-take.ts);
//   - a refused take, a dropped one and a refusal of the route itself are answers: the value is
//     asked about again only at its next revision.
//
// A seen report or a take the daemon gave no answer to is sent again (daemonLink.untilAnswered)
// until the daemon answers it or a newer value replaces it (replace): departure 7 of INFRA-2811's
// plan. Such a re-send is the same ask, not a second one, and the daemon answers a take repeated
// for a number it accepted with the same text.

// The task routes the bridge calls.
const (
	taskSeenPath = "/legion/v1/claims/task-seen"
	takePath     = "/legion/v1/claims/take"
)

// The outcomes a take is answered with (claim.TakeResponse, supervise's TakeOutcome).
const (
	takeAccepted = "accepted"
	takeRefused  = "refused"
	takeDropped  = "dropped"
)

// taskValue is a claim's task value as a task line carries it: the number of the task the claim is
// owed, 0 for none (the value was deleted at this revision: the claim has no task from it on), and
// the bucket's epoch and revision of the write (claim.TaskValue).
type taskValue struct {
	number   uint64
	epoch    string
	revision uint64
}

// pair is the (epoch, revision) a value was written at. A revision is read only beside its epoch,
// since a bucket created again counts its revisions from 1 again.
type pair struct {
	epoch    string
	revision uint64
}

func (v taskValue) at() pair { return pair{epoch: v.epoch, revision: v.revision} }

// replaces is whether v takes the place of held: any value when none is held or held is of another
// epoch, and a later revision of the same one.
func (v taskValue) replaces(held *taskValue) bool {
	return held == nil || held.epoch != v.epoch || v.revision > held.revision
}

// call is one seen report or take being sent until the daemon answers it: the value it names, and
// how to stop sending it.
type call struct {
	value taskValue
	stop  context.CancelFunc
}

// takeAnswer is the daemon's answer to a take, back on the loop: the take it answers, the answer,
// and the failure that stood for one — a refusal of the route, or a request that could not be sent.
type takeAnswer struct {
	call   *call
	answer claim.TakeResponse
	err    error
}

// valueHanded takes a task value the inbox handed the bridge. A value newer than the one held
// replaces it, and with it every seen report and take still being sent for another pair (replace);
// it is reported seen at once; and Claude Code in no turn takes it (idle). A value no newer than the
// one held changes nothing: the same value relayed again is not a new ask.
func (b *bridge) valueHanded(v taskValue) {
	if b.daemon == nil {
		b.log.Printf("ignoring task %d's value: this pane names no daemon to take it from", v.number)
		return
	}
	if !v.replaces(b.task) {
		return
	}
	b.task = &v
	b.replace(v.at())
	if v.number == 0 {
		b.log.Printf("the claim holds no task from revision %d on", v.revision)
	} else {
		b.log.Printf("task %d's value reached the inbox at revision %d", v.number, v.revision)
		b.reportSeen(v)
	}
	b.idle()
}

// replace stops sending the seen report and the take of any pair but at: a newer value says
// something new, and a take of an older one would ask about a value the daemon has replaced. Cards
// a take held back are let go by the idle that follows.
func (b *bridge) replace(at pair) {
	if b.seeing != nil && b.seeing.value.at() != at {
		b.seeing.stop()
		b.seeing = nil
	}
	if b.asking != nil && b.asking.value.at() != at {
		b.log.Printf("the take of task %d at revision %d is replaced by a newer value and sent no more", b.asking.value.number, b.asking.value.revision)
		b.asking.stop()
		b.asking = nil
	}
}

// reportSeen reports v seen, once per pair, until the daemon answers (claims/task-seen). Nothing
// waits for it: the daemon answers 204 and the bridge acts on nothing it says, so a refusal is
// logged and nothing else.
func (b *bridge) reportSeen(v taskValue) {
	if b.seen[v.at()] {
		return
	}
	b.seen[v.at()] = true
	ctx, stop := context.WithCancel(b.requests)
	b.seeing = &call{value: v, stop: stop}
	request := claim.TaskSeenRequest{BootCaller: claim.BootCaller{BootToken: b.daemon.bootToken}, Generation: b.daemon.generation,
		Number: v.number, Epoch: v.epoch, Revision: v.revision}
	go func() {
		defer stop()
		status, answer, err := b.daemon.untilAnswered(ctx, fmt.Sprintf("claims/task-seen of task %d", v.number), taskSeenPath, request)
		switch {
		case ctx.Err() != nil:
		case err != nil:
			b.log.Printf("could not report task %d seen at revision %d: %v", v.number, v.revision, err)
		case status != http.StatusNoContent:
			b.log.Printf("the daemon refused the seen report of task %d at revision %d: %s", v.number, v.revision, refusalText(status, answer))
		}
	}()
}

// ask asks the daemon to take the task the held value names (claims/take) where it may — Claude
// Code in no turn, the seed done, no take being sent, and a task the value names that the bridge
// has not asked about at the value's pair — and says whether it did. The answer comes back on the
// loop (takeAnswered).
func (b *bridge) ask() bool {
	v := b.task
	if b.daemon == nil || v == nil || v.number == 0 || b.inRun || b.seeding || b.asking != nil || b.asked[v.at()] {
		return false
	}
	b.asked[v.at()] = true
	ctx, stop := context.WithCancel(b.requests)
	c := &call{value: *v, stop: stop}
	b.asking = c
	b.log.Printf("asking the daemon to take task %d at revision %d", v.number, v.revision)
	request := claim.TakeRequest{BootCaller: claim.BootCaller{BootToken: b.daemon.bootToken}, Generation: b.daemon.generation,
		Number: v.number, Epoch: v.epoch, Revision: v.revision}
	go func() {
		defer stop()
		status, body, err := b.daemon.untilAnswered(ctx, fmt.Sprintf("claims/take of task %d", c.value.number), takePath, request)
		if ctx.Err() != nil {
			return
		}
		answered := takeAnswer{call: c}
		switch {
		case err != nil:
			answered.err = err
		case status != http.StatusOK:
			answered.err = fmt.Errorf("the daemon refused it: %s", refusalText(status, body))
		default:
			if err := json.Unmarshal(body, &answered.answer); err != nil {
				answered.err = fmt.Errorf("its answer could not be read: %w", err)
			}
		}
		send(b.takeAnswers, b.stop, answered)
	}()
	return true
}

// takeAnswered acts on the daemon's answer to the take being sent; the answer of a take a newer
// value replaced is no longer anyone's. An accepted take starts the task's turn ahead of every
// queued delivery, or, during a turn, once that turn ends. Anything else lets go of the cards the
// take held back: the value is asked about again only at its next revision.
func (b *bridge) takeAnswered(a takeAnswer) {
	if b.asking != a.call {
		return
	}
	b.asking = nil
	number := a.call.value.number
	switch {
	case a.err != nil:
		b.log.Printf("the take of task %d failed (%v); it is asked again at the value's next revision", number, a.err)
	case a.answer.Outcome == takeAccepted:
		if b.inRun || b.seeding {
			b.heldText, b.heldTask = a.answer.Text, number
			b.log.Printf("the daemon accepted the take of task %d during a turn; the task starts a turn of its own once this one ends", number)
			return
		}
		b.log.Printf("the daemon accepted the take of task %d", number)
		b.startTask(number, a.answer.Text)
		return
	case a.answer.Outcome == takeRefused:
		b.log.Printf("the daemon refused the take of task %d (hold %s); it is asked again at the value's next revision", number, a.answer.Hold)
	case a.answer.Outcome == takeDropped:
		b.log.Printf("the daemon dropped task %d: the claim no longer owes it", number)
	default:
		b.log.Printf("the daemon answered the take of task %d with outcome %q, which the bridge does not know; it is asked again at the value's next revision", number, a.answer.Outcome)
	}
	b.idle()
}

// takePending is whether a take holds the queued deliveries back: one being sent, or one accepted
// whose task has not started its turn yet.
func (b *bridge) takePending() bool { return b.asking != nil || b.heldText != "" }

// startTask starts the turn of the task number, whose accepted take answered text: a turn of the
// bridge's own whose first user message is text, sent ahead of every queued delivery, and whose
// outcome report names number. A delivery still queued is a later step of it.
func (b *bridge) startTask(number uint64, text string) {
	b.log.Printf("task %d starts a turn", number)
	b.startRun(text, nil)
	b.runTask = number
}

// idle runs what waits for Claude Code to be in no turn, whenever it may be: the seed's end, a
// turn's end, a take's answer and a newer task value. The text of a take accepted during the turn
// that ended starts first; then a take of the held value; and only with no take pending, the oldest
// queued Envoy delivery.
func (b *bridge) idle() {
	if b.inRun || b.seeding {
		return
	}
	if b.heldText != "" {
		text, number := b.heldText, b.heldTask
		b.heldText, b.heldTask = "", 0
		b.startTask(number, text)
		return
	}
	if b.ask() || b.takePending() {
		return
	}
	if len(b.queue) > 0 {
		b.sendQueued()
	}
}
