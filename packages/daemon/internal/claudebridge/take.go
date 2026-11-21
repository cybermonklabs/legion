package claudebridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// A claim's task reaches its worker as a value the worker takes, not as a prompt the daemon pushes
// (dispatch://INFRA-2811 R2). claude-envoy, which holds the claim and its Envoy consumer, keeps
// nothing of the values it is handed: it writes each one to the inbox as a task line (inbox.go) —
// the registration's value, a read of claims/task once the claim's ready has answered, at a regain
// of its role and at a new epoch of its relay, and each value a task frame relays — carrying the
// number the daemon's reading it came with says the claim finished (0 for a relayed value); writes
// that number alone as a finished line for a reading that holds no value; and writes the ready
// line once `claims/ready` has answered and the read of claims/task after it has answered too,
// behind that reading's line, or with no reading line after a read that failed (the plan's
// decision 16). The bridge, which knows whether Claude Code is in a turn, does the rest, as Oh My
// Pi's take does (packages/pi-shared/src/task-take.ts, createTaskTake), naming the claim with the
// launch's boot token (daemonLink):
//
//   - it holds the newest value, a later revision of the same epoch or any value of another one
//     (replaces), and the highest finished number any task line or finished line carried. A value
//     naming a number at or below that, or at or below the run-end mark (below), is owed nothing
//     (owes);
//   - it reports each value that names a task seen at once, owed or not, whatever Claude Code is
//     doing (claims/task-seen);
//   - from the ready line on, when Claude Code is in no turn and the seed is done, it asks the
//     daemon to take the task an owed value names, once per (epoch, revision) (claims/take). The
//     daemon refuses a take before the ready, so none is asked for before the ready line. The
//     pairs it asked at are kept in this process's memory alone, so a relaunched bridge asks again
//     at the value it is handed;
//   - until the ready line, every Envoy delivery waits in the queue, whatever value the bridge
//     holds, the session's own direct message included: the ready decision can write a value the
//     registration did not carry, and claude-envoy writes the reading of it ahead of the ready
//     line on the one inbox connection, so no card starts a turn before the task's (the ready
//     gate). A failed read writes no reading line and the ready line all the same, which opens
//     the gate. From the ready line on, while it holds an owed value it has not asked about, while
//     that take waits for its answer, and while an accepted take's text waits for its turn, an
//     Envoy delivery that would start a turn waits in the queue, since the turn the take goes into
//     must not start with a card (takePending). claude-envoy writes the ready line before it
//     claims the role, whose kept lane the listener hands over at the claim, so every card of
//     that lane waits behind the take as well. A value the daemon writes later — the resume its
//     outbox queues after the ready, a task queued later still — reaches the inbox as the
//     listener relays it, and a card that arrived before it may start a turn first, which the take
//     then waits behind;
//   - an accepted take's text starts a turn of its own ahead of every queued delivery, and that
//     turn is the task's: its outcome report names the number once Claude Code has re-emitted the
//     text in it (startTask, echoed, turnoutcome.go);
//   - nothing is sent into a turn that is running. A take the daemon accepts while Claude Code is
//     in a turn — one Claude Code started by itself after the daemon decided the take, or while a
//     take sent again after a lost answer waited — has its text held until that turn ends. The
//     daemon gave that turn to the take, as the first to start after its accept; its end is
//     reported naming no task and naming the held take's number as held (turnoutcome.go,
//     claim.TurnOutcomeRequest.Held), which puts the take back to awaiting its turn on the daemon
//     (the plan's decisions 8 and 9) also where the daemon's count of the process's turns is not
//     known, after a daemon restart; and the held text then starts a turn of its own when Claude
//     Code is idle, ahead of every queued delivery: the task's turn on both sides, whose report
//     names the number;
//   - a held text has not reached Claude Code, so a newer value of its number — the daemon took
//     the task back, or only wrote its value again — is a new offer (the plan's decision 9): the
//     held text is let go (valueHanded), the take is made at that value once Claude Code is idle,
//     and the text its accepted answer carries runs, once. A text the bridge writes has reached
//     Claude Code once Claude Code re-emits it (--replay-user-messages; echoed), as an Envoy
//     delivery has. The bridge starts the task's turn as it writes the text (startTask), so no
//     newer value is asked about while Claude Code still sets that text up, and the end of a turn
//     that re-emitted it holds the number finished (below). Nothing of the daemon's drops a
//     written text: the daemon compacts no Claude Code claim (internal/api's compact refuses it
//     CLAIM_NOT_ON_OMP), and the bridge serves no abort (fromDaemon). Claude Code itself may: a
//     task's turn that ends with no echo of the task's own text — an Envoy delivery's echo in that
//     turn is none — and that the provider did not fail is a text Claude Code dropped, as a step's
//     result with no echo of the delivery it carried is a delivery it dropped (errNotTaken). That
//     turn is reported naming no task (result), which puts the take back to awaiting its turn on
//     the daemon (the plan's decision 8); nothing is held finished, and the number is taken again
//     at its value's next revision, whose accepted text is written once, at idle, as Oh My Pi's
//     take hands a text again after its host dropped one (packages/pi-shared/src/task-take.ts,
//     hostDropped). A turn of the task the provider failed is reported naming the number with its
//     provider error whether or not the echo came, so the daemon's provider-error ladder takes
//     the task back and gives it again after its wait. A Claude
//     Code that dropped every text written to it would go round that loop uncharged — the
//     put-back, the text written again, and a turn of no cause each time, every turn's start and
//     end resetting R5's count — which the plan states and leaves open: nothing measured says
//     Claude Code drops a stdin message at all;
//   - an `accepted` answer for a number the claim is owed no more — one a finished line or the
//     run-end mark raised while the take was being sent — runs nothing;
//   - when a task's turn whose text Claude Code re-emitted ends with no provider error, its number
//     is held finished (the run-end mark, finishing) until the daemon answers that turn's report:
//     for a worker on the current contract that report alone finishes the task, so a newer
//     revision of the same number, which the daemon may write before the report lands (it writes
//     the value again from its record where the bucket lost it or holds another write) and would
//     answer with the same text, is reported seen and not taken. A 2xx finished the number in
//     the record, which deletes its value, so the mark stays; a 4xx refusal applied
//     nothing and the record still owes the number, so the mark is cleared (reportRefused) and a
//     newer revision of it is taken and runs once. A report carrying a provider error marks
//     nothing: the daemon takes the task back and writes its value again after its wait;
//   - a refused take, a dropped one and a refusal of the route itself are answers: the value is
//     asked about again only at its next revision. A take refused `in-turn` during a turn is one
//     the daemon writes the value of again at that turn's end, so it counts as pending until that
//     turn ends or a newer value is held (refusedInTurn);
//   - claude-envoy's hooks ask whether the take is pending, with a take-pending line on an inbox
//     connection of their own, which the loop answers at once (pendingAnswer): its Stop hook
//     blocks no stop while a take is pending, so the turn ends and the take goes ahead of any
//     reminder turn (dispatch://INFRA-4130 R6), and its PostToolUse hook names the task an owed
//     value waits with, during a turn that did not take it, in its note (R1).
//
// A seen report or a take the daemon gave no answer to is sent again (daemonLink.untilAnswered)
// until the daemon answers it or a newer value replaces it (replace): every failure
// daemonLink.unanswered names. Such a re-send is the same ask, not a second one, and the daemon
// answers a take repeated for a number it accepted with the same text.

// The task routes the bridge calls.
const (
	taskSeenPath = "/legion/v1/claims/task-seen"
	takePath     = "/legion/v1/claims/take"
)

// The outcomes a take is answered with (claim.TakeResponse, supervise's TakeOutcome), and the hold
// of a take refused because the claim's turn was running when the daemon decided it (supervise's
// HoldInTurn), whose value the daemon writes again at that turn's end.
const (
	takeAccepted = "accepted"
	takeRefused  = "refused"
	takeDropped  = "dropped"
	holdInTurn   = "in-turn"
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

// replaces is whether v takes the place of held, the one newest-value rule: any value when none is
// held or held is of another epoch, and a later revision of the same one.
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

// owes is whether the claim is owed the task number: one above the highest number a task line or
// a finished line said the claim finished, and above the run-end mark.
func (b *bridge) owes(number uint64) bool { return number > max(b.finished, b.finishing) }

// unasked is whether the held value names a task the claim is owed that the bridge has not asked
// to take at the value's pair.
func (b *bridge) unasked() bool {
	return b.task != nil && b.owes(b.task.number) && !b.asked[b.task.at()]
}

// valueHanded takes a task value the inbox handed the bridge, with the number the reading it came
// with says the claim finished, the highest of which the bridge holds. A value newer than the one
// held replaces it, and with it every seen report and take still being sent for another pair
// (replace) and the in-turn refusal of the value it replaces (refusedInTurn); one that names a
// task is reported seen at once, owed or not; and Claude Code in no turn takes it once the ready
// line came (idle). A newer value of the task whose accepted text is held through a turn, a text
// never written to Claude Code, is a new offer (the plan's decision 9): the held text is let go,
// and the take of this value, made once Claude Code is idle, brings the text that runs. A value no
// newer than the one held changes nothing but the finished number: the same value relayed again is
// not a new ask.
func (b *bridge) valueHanded(v taskValue, finished uint64) {
	if b.daemon == nil {
		b.log.Printf("ignoring task %d's value: this pane names no daemon to take it from", v.number)
		return
	}
	b.finished = max(b.finished, finished)
	if v.replaces(b.task) {
		b.task, b.refusedInTurn = &v, false
		b.replace(v.at())
		switch {
		case v.number == 0:
			b.log.Printf("the claim holds no task from revision %d on", v.revision)
		case b.owes(v.number):
			b.log.Printf("task %d's value reached the inbox at revision %d", v.number, v.revision)
		default:
			b.log.Printf("task %d's value reached the inbox at revision %d; that task is finished, so it is reported seen and not taken", v.number, v.revision)
		}
		if b.held != nil && v.number == b.held.number {
			b.held = nil
			b.log.Printf("task %d's held text is let go: its take is made again at revision %d once Claude Code is idle", v.number, v.revision)
		}
		if v.number != 0 {
			b.reportSeen(v)
		}
	}
	b.idle()
}

// finishedHanded takes a finished line: a reading of the daemon's that held no value, and the
// number it says the claim finished, the highest of which the bridge holds, as createTaskTake's
// read raises its own from such a reading. The value held stays held: a reading with no value
// says nothing of it. A held value that names a number at or below the new one is owed nothing
// from here on, so the cards its take held back are let go (idle).
func (b *bridge) finishedHanded(finished uint64) {
	if b.daemon == nil {
		return
	}
	b.finished = max(b.finished, finished)
	b.idle()
}

// readied takes the ready line: the claim's ready has answered, and so has the read of the claim's
// value after it, whose reading came ahead of this line on the inbox, if the read answered. The
// daemon answers a take from then on, so the task an owed value names is asked for once Claude Code
// is idle (ask), and the ready gate lets the queued deliveries go where no take holds them (idle).
func (b *bridge) readied() {
	if b.armed {
		return
	}
	b.armed = true
	if b.daemon != nil {
		b.log.Printf("the claim's ready has answered: its task is taken from now on")
	}
	b.idle()
}

// awaitingReady is the ready gate: a pane that names a daemon holds every queued delivery until the
// ready line, whatever value it holds, since the ready decision can write a value the reading
// ahead of that line carries and the registration did not. A pane that names none holds no claim,
// is never written a ready line, and holds nothing back.
func (b *bridge) awaitingReady() bool { return b.daemon != nil && !b.armed }

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
		status, answer, err := b.daemon.untilAnswered(ctx, fmt.Sprintf("claims/task-seen of task %d", v.number), taskSeenPath, request, nil)
		switch {
		case ctx.Err() != nil:
		case err != nil:
			b.log.Printf("could not report task %d seen at revision %d: %v", v.number, v.revision, err)
		case status != http.StatusNoContent:
			b.log.Printf("the daemon refused the seen report of task %d at revision %d: %s", v.number, v.revision, refusalText(status, answer))
		}
	}()
}

// ask asks the daemon to take the task the held value names (claims/take) where it may — the ready
// line come, Claude Code in no turn, the seed done, no take being sent, and a task the claim is
// owed that the bridge has not asked about at the value's pair — and says whether it did. The
// answer comes back on the loop (takeAnswered). A pane that names no daemon holds no value at all
// (valueHanded), so this never reads b.daemon without one.
func (b *bridge) ask() bool {
	if !b.armed || b.inRun || b.seeding || b.asking != nil || !b.unasked() {
		return false
	}
	v := b.task
	b.asked[v.at()] = true
	ctx, stop := context.WithCancel(b.requests)
	c := &call{value: *v, stop: stop}
	b.asking = c
	b.log.Printf("asking the daemon to take task %d at revision %d", v.number, v.revision)
	request := claim.TakeRequest{BootCaller: claim.BootCaller{BootToken: b.daemon.bootToken}, Generation: b.daemon.generation,
		Number: v.number, Epoch: v.epoch, Revision: v.revision}
	go func() {
		defer stop()
		status, body, err := b.daemon.untilAnswered(ctx, fmt.Sprintf("claims/take of task %d", c.value.number), takePath, request, nil)
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
// queued delivery, or, during a turn, holds its text until that turn ends: nothing is sent into a
// turn that is running. An accepted answer for a number the claim is owed no more runs nothing.
// Anything else lets go of the cards the take held back: the value is asked about again only at
// its next revision.
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
		switch {
		case !b.owes(number):
			b.log.Printf("the daemon accepted the take of task %d again; its text runs no second time", number)
		case b.inRun || b.seeding:
			b.held = &heldTake{number: number, text: a.answer.Text}
			b.log.Printf("the daemon accepted the take of task %d during a turn; that turn is reported as no task's, and the task starts a turn of its own once it ends", number)
			return
		default:
			b.log.Printf("the daemon accepted the take of task %d", number)
			b.startTask(number, a.answer.Text)
			return
		}
	case a.answer.Outcome == takeRefused:
		if a.answer.Hold == holdInTurn && b.inRun {
			b.refusedInTurn = true
		}
		b.log.Printf("the daemon refused the take of task %d (hold %s); it is asked again at the value's next revision", number, a.answer.Hold)
	case a.answer.Outcome == takeDropped:
		b.log.Printf("the daemon dropped task %d: the claim no longer owes it", number)
	default:
		b.log.Printf("the daemon answered the take of task %d with outcome %q, which the bridge does not know; it is asked again at the value's next revision", number, a.answer.Outcome)
	}
	b.idle()
}

// runningOwn is whether the held value names the task whose turn is running and whose text that
// turn took: Claude Code re-emitted it (taskRun.taken). Only such a turn's end holds the number
// finished (runEnded). A turn whose text Claude Code never re-emitted is reported as no task's
// (result), so its end marks nothing finished, and the bridge takes a newer revision of that
// number the moment the turn ends.
func (b *bridge) runningOwn() bool {
	return b.inRun && b.run.taken && b.task != nil && b.task.number == b.run.task
}

// takePending is whether the claim's take holds a task: an owed value not yet asked about — before
// the ready line, or while Claude Code is in a turn or seeding —, a take being sent, one accepted
// whose task has not started its turn yet, or a take of the held value refused in-turn during the
// turn running (refusedInTurn). It is what holds the queued deliveries back (deliver, idle), and
// what a take-pending query answers (pendingAnswer). The last term holds back no delivery the
// first three do not: it is set only while a turn runs and cleared as that turn ends (runEnded),
// and deliver and idle read this only while no turn runs.
//
// The daemon can write the running task's own number again during its turn. Once that turn has
// taken the task's text (runningOwn), such a value is not an unasked value here: the bridge takes
// nothing for it, since that turn's end holds the number finished (runEnded). In a turn whose text
// Claude Code dropped, the value counts as unasked, as in a turn of no task: that turn's end marks
// nothing finished, and the bridge takes the value as soon as the turn ends. That changes neither
// deliver nor idle: the term differs only while a turn runs, and neither reads this then.
func (b *bridge) takePending() bool {
	return (b.unasked() && !b.runningOwn()) || b.asking != nil || b.held != nil || b.refusedInTurn
}

// pendingAnswer is the answer to a take-pending query: whether the take is pending (takePending),
// and which task waits. While Claude Code is in a turn, Waiting is the number of the task the held
// value owes the claim, asked about or not, unless the turn is that task's own and took its text
// (runningOwn), or the bridge holds the task's accepted text for when the turn ends (held). A turn
// of the task whose text Claude Code dropped took nothing, so in it the answer names the task
// waiting, as in a turn of no task. Out of a turn, and in those two cases, Waiting is 0.
func (b *bridge) pendingAnswer() *TakePendingAnswer {
	answer := &TakePendingAnswer{Pending: b.takePending()}
	if b.inRun && b.task != nil && b.owes(b.task.number) && !b.runningOwn() &&
		(b.held == nil || b.task.number != b.held.number) {
		answer.Waiting = b.task.number
	}
	return answer
}

// startTask starts the turn of the task number, whose accepted take answered text: a turn of the
// bridge's own whose first user message is text, sent ahead of every queued delivery, and whose
// outcome report names number once Claude Code has re-emitted text in it (echoed), and no task if
// it never does (result). A delivery still queued is a later step of it.
func (b *bridge) startTask(number uint64, text string) {
	b.log.Printf("task %d starts a turn", number)
	b.startRun(text, nil)
	b.run = taskRun{task: number, text: text}
}

// runEnded takes the end of a turn, the task's turn when task is not 0: one the provider did not
// fail holds the task's number finished from here on (the run-end mark), before its report has even
// landed, since that report finishes the task on the daemon, and until the daemon refuses it
// (reportRefused). A turn the provider failed marks nothing. A take refused in-turn during the
// turn is pending no more (refusedInTurn): the daemon writes the value again after this end.
func (b *bridge) runEnded(task uint64, providerFailed bool) {
	b.refusedInTurn = false
	if task != 0 && !providerFailed {
		b.finishing = max(b.finishing, task)
	}
}

// reportRefused takes the daemon's 4xx refusal of the clean report of task number's turn
// (turnoutcome.go): that report applied nothing and the record still owes the number, so the
// bridge holds it finished no longer, and takes a newer revision of it — one it holds already or
// one the daemon writes later — which then runs once. A number a task line said finished stays
// finished, and a mark that has moved on to a later number since stays.
func (b *bridge) reportRefused(number uint64) {
	if b.finishing != number {
		return
	}
	b.finishing = 0
	b.log.Printf("the daemon refused the report of task %d's turn, so the claim still owes it: a newer value of it is taken", number)
	b.idle()
}

// idle runs what waits for Claude Code to be in no turn, whenever it may be: the seed's end, a
// turn's end, the ready line, a take's answer, a newer task value and a refused report. The text of
// a take accepted during the turn that ended starts first; then a take of the held value; and only
// with no take pending and the ready gate open (awaitingReady), the oldest queued Envoy delivery.
func (b *bridge) idle() {
	if b.inRun || b.seeding {
		return
	}
	if held := b.held; held != nil {
		b.held = nil
		b.startTask(held.number, held.text)
		return
	}
	if b.ask() || b.takePending() || b.awaitingReady() {
		return
	}
	if len(b.queue) > 0 {
		b.sendQueued()
	}
}
