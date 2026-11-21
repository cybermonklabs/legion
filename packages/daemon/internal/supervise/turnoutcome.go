package supervise

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/sjawhar/legion/daemon/internal/redact"
	"github.com/sjawhar/legion/daemon/internal/store/storetext"
)

// TurnOutcome is one turn of a claim's agent as the backend's producer reported it when that turn
// ended: the launch generation it ran at, the producer's own sequence within that generation,
// whether the provider refused the turn, and what the provider said about a refusal.
//
// The producer is the one counter. Oh My Pi's turns are reported by pi-legion's agent_end hook and
// Claude Code's by the Claude bridge, each over POST /legion/v1/claims/turn-outcome; the turn's end
// on the worker stream (StreamTurnEnd) and every turn ordinal the daemon keeps take no part in
// this, so no two counters can drift. A turn no report arrived for — the process died before it
// could send one — is a key the window never holds, which counts neither way.
//
// The tags are the shape the store keeps the window in (the claims table's turn_outcomes), which
// is the one place a report is written.
//
// Unapplied says that what the report says of the claim's task (reportedTask) was never done: the
// write of the report failed, which ends the decision before that step. It is the one thing that
// tells a report the producer sends again under the same key from a report already applied
// (turnOutcome), and it is written with the window, so a daemon that read the window back tells
// the two apart as the daemon that recorded it would.
//
// Task is the number of the task the report named, 0 for none, and ProviderError whether it
// carried the provider error its turn ended in, with that error's HTTP status
// (ProviderErrorStatus, 0 for none). ProviderErrorRetries is, for a report whose provider error
// reached the ladder (reachesLadder), the count of the task's sends the ladder's step makes it
// (Delivery.ProviderErrorRetries), and 0 for every other report. They are what tells a daemon
// that read the window back what an applied report did to the claim's pending task, where the
// store holds the report and not what it did (Claim.owedReport): a report is applied with its
// own write, before its finish or its take-back is written (turnOutcome, reportedTask), so a
// store that took the one and refused the other leaves a window naming an effect nobody has
// made, and the worker's report sent again is a repeat that makes none. An entry stored before
// they were kept reads back with none of them, a report naming no task, which did nothing to
// any task.
//
// Watermark is set on the window's oldest entry alone, and zero on every other: the key of the
// newest report the window dropped to keep to its bound (recordTurnOutcome), below which every
// report of its generation was applied, and every report of an earlier generation is of a process
// the claim has left. Every report at or below it that the window does not hold is a repeat,
// since the window applied it before it dropped it, or it is of a turn no take of the claim's
// process holds and older than every report a run is derived over. It rides on an entry so that
// it is written with the window, in the same column, and read back with it.
type TurnOutcome struct {
	Generation           uint64         `json:"generation"`
	Sequence             uint64         `json:"sequence"`
	Refused              bool           `json:"refused"`
	Message              string         `json:"message,omitempty"`
	Unapplied            bool           `json:"unapplied,omitempty"`
	Task                 uint64         `json:"task,omitempty"`
	ProviderError        bool           `json:"providerError,omitempty"`
	ProviderErrorStatus  int            `json:"providerErrorStatus,omitempty"`
	ProviderErrorRetries int            `json:"providerErrorRetries,omitempty"`
	Watermark            TurnOutcomeKey `json:"watermark,omitzero"`
}

// TurnOutcomeKey is the key a report is kept under in the window: the launch generation its turn
// ran at and its producer's sequence within that generation (RequestTurnOutcome). The zero key is
// none, since a producer counts its turns from 1.
type TurnOutcomeKey struct {
	Generation uint64 `json:"generation"`
	Sequence   uint64 `json:"sequence"`
}

// key is the key o is kept under.
func (o TurnOutcome) key() TurnOutcomeKey {
	return TurnOutcomeKey{Generation: o.Generation, Sequence: o.Sequence}
}

// compareKeys orders two keys as compareTurnOutcomes orders the reports kept under them.
func compareKeys(a, b TurnOutcomeKey) int {
	return cmp.Or(cmp.Compare(a.Generation, b.Generation), cmp.Compare(a.Sequence, b.Sequence))
}

// compareTurnOutcomes orders reports as their turns ran: by generation, and within a generation by
// the producer's own sequence. It is the order the window is kept in and the run is derived in, so
// a report that arrives late or out of order lands in its own place rather than at the end.
func compareTurnOutcomes(a, b TurnOutcome) int { return compareKeys(a.key(), b.key()) }

// maxRefusalMessage bounds, in runes, the provider's message a report carries. The message reaches
// the claim's failure, which is read back in notices and in every state answer, so a provider that
// returns a page of diagnostics leaves a sentence rather than a page in the store. A message past
// the bound keeps its first maxRefusalMessage-1 runes and "…" (redact.Runes).
const maxRefusalMessage = 500

// turnOutcomeWindow is how many reports a claim keeps: the launch failure limit
// (launch_failure_limit, 3 by default), which is the length the trailing run has to reach to fail
// the claim. A longer run cannot exist, since the claim fails the moment it reaches the limit, so
// the newest limit reports are everything RefusedRun can read; a report older than all of them
// cannot shorten a run derived over them, and is dropped. That bound is why a claim whose agent
// works for a week carries a handful of rows rather than a turn-by-turn history nothing reads.
func (m *Machine) turnOutcomeWindow() int { return m.deps.Limits.LaunchFailures }

// recordTurnOutcome puts a report in the claim's window, in (generation, sequence) order, and
// trims the window to its bound (turnOutcomeWindow). A report whose key the window already holds
// replaces it and answers repeat: the producer sent it again because it heard no answer, and its
// first attempt was applied. The one exception is a first attempt whose write failed
// (TurnOutcome.Unapplied), which never reached the task, so the report sent again is its first
// application rather than a repeat. No count is kept anywhere: every reader derives what it needs
// from the window as it stands (RefusedRun), which is what makes a late report and an in-order
// one leave the same answer.
//
// A report the window no longer holds, at or below its watermark (TurnOutcome.Watermark), is a
// repeat too, and is recorded nowhere: the window applied it and then dropped it to keep to its
// bound, so its key alone no longer says so. Its first attempt's answer can have been lost — a
// daemon restart in the middle of the request — and its sending again backs off, so newer reports
// can push it out before it comes. A late report newer than the watermark lands in its place as
// before, and is applied, though the window may drop it at once.
//
// The bound drops the oldest applied reports first, and the watermark rises only over reports the
// window applied: an entry is dropped only where every key from the one after the watermark up to
// its own is held (gapless), so a report missing below it — its first attempt lost on the way,
// never seen — stays above the watermark and is applied when it is sent again, however many newer
// reports of its generation landed meanwhile. Raised over it, the watermark would answer that
// report as a repeat, and one naming the take's number would finish nothing: the task would run
// again after the next relaunch. The window holds the reports above such a gap past its bound
// until the gap is filled, the claim's process moves to a later generation, whose first report
// follows every key of the earlier one (follows), or the window reaches turnOutcomeGapBound. One
// whose write failed (Unapplied) stays until it is sent again and applied, however old: dropped,
// it would fall under the watermark, and the report sent again, which is its first application,
// would be answered as a repeat.
func (m *Machine) recordTurnOutcome(o TurnOutcome) (repeat bool) {
	window, mark := m.claim.TurnOutcomes, m.turnOutcomeMark()
	at, found := slices.BinarySearchFunc(window, o, compareTurnOutcomes)
	if !found && mark != (TurnOutcomeKey{}) && compareKeys(o.key(), mark) <= 0 {
		return true
	}
	if found {
		repeat = !window[at].Unapplied
		window[at] = o
	} else {
		window = slices.Insert(window, at, o)
	}
	for len(window) > m.turnOutcomeWindow() {
		oldest := slices.IndexFunc(window, func(o TurnOutcome) bool { return !o.Unapplied })
		if oldest < 0 {
			break
		}
		if dropped := window[oldest].key(); compareKeys(dropped, mark) > 0 {
			if !gapless(mark, window[:oldest+1]) && len(window) <= turnOutcomeGapBound {
				break
			}
			mark = dropped
		}
		window = slices.Delete(window, oldest, oldest+1)
	}
	m.setTurnOutcomes(window, mark)
	return repeat
}

// turnOutcomeGapBound is how many reports the window holds while a report of their generation
// below them is still missing (recordTurnOutcome). Past it the window drops its oldest as it would
// with no gap, and the missing report, should it come after all, is answered as a repeat. A worker
// sends a report again within seconds of its first attempt (its back-off stops at 30 s), so one
// still missing after this many newer reports of its process landed is one no worker will send:
// the bound keeps a claim whose producer never sent a report from growing its window for the rest
// of its process's life.
const turnOutcomeGapBound = 64

// gapless is whether held, the window's oldest reports in order, holds every key from the one
// right after mark through its own last one (follows): no report between them is missing. Keys at
// or below mark are passed over.
func gapless(mark TurnOutcomeKey, held []TurnOutcome) bool {
	last := mark
	for _, o := range held {
		key := o.key()
		if compareKeys(key, last) <= 0 {
			continue
		}
		if !follows(last, key) {
			return false
		}
		last = key
	}
	return true
}

// follows is whether key is the one right after last: the next sequence of last's generation, or
// the first of a later generation, since a producer numbers each generation's turns from 1 and a
// report of an earlier generation is of a process the claim has left, which reaches no take of
// the process it runs now (reachesLadder). The zero key, no watermark, is followed by the first
// report of any generation.
func follows(last, key TurnOutcomeKey) bool {
	if key.Generation == last.Generation {
		return key.Sequence == last.Sequence+1
	}
	return key.Generation > last.Generation && key.Sequence == 1
}

// turnOutcomeMark is the window's watermark, the zero key for none (TurnOutcome.Watermark).
func (m *Machine) turnOutcomeMark() TurnOutcomeKey {
	if len(m.claim.TurnOutcomes) == 0 {
		return TurnOutcomeKey{}
	}
	return m.claim.TurnOutcomes[0].Watermark
}

// setTurnOutcomes makes window the claim's, its watermark mark on its oldest entry and on no other
// (TurnOutcome.Watermark).
func (m *Machine) setTurnOutcomes(window []TurnOutcome, mark TurnOutcomeKey) {
	for i := range window {
		window[i].Watermark = TurnOutcomeKey{}
	}
	if len(window) > 0 {
		window[0].Watermark = mark
	}
	m.claim.TurnOutcomes = window
}

// appliedTurnOutcome is the window's entry under the key of r, where the window holds one from an
// attempt that was applied (TurnOutcome.Unapplied false).
func (m *Machine) appliedTurnOutcome(r RequestTurnOutcome) (TurnOutcome, bool) {
	key := TurnOutcome{Generation: r.Generation, Sequence: r.Sequence}
	at, found := slices.BinarySearchFunc(m.claim.TurnOutcomes, key, compareTurnOutcomes)
	if !found || m.claim.TurnOutcomes[at].Unapplied {
		return TurnOutcome{}, false
	}
	return m.claim.TurnOutcomes[at], true
}

// unappliedTurnOutcome marks the window's entry for o's key as a report that never reached the
// claim's task (TurnOutcome.Unapplied). A report the window dropped as it recorded it — a late
// one, older than everything a full window keeps — goes back in, marked: the watermark its drop
// raised would otherwise answer its sending again as a repeat of a report never applied
// (recordTurnOutcome).
func (m *Machine) unappliedTurnOutcome(o TurnOutcome) {
	window, mark := m.claim.TurnOutcomes, m.turnOutcomeMark()
	at, found := slices.BinarySearchFunc(window, o, compareTurnOutcomes)
	if found {
		window[at].Unapplied = true
		return
	}
	o.Unapplied = true
	m.setTurnOutcomes(slices.Insert(window, at, o), mark)
}

// owedReport is the report whose effect on the claim's pending task the store does not hold, read
// from the claim as the store keeps it, for the machine a daemon start builds over it
// (NewMachine): the newest applied report of the claim's current generation that names the
// pending task's number, while that task's take stands accepted, its turn started or not. Such a
// report is of the take's turn (reachesLadder), and the daemon that applied it decided there what
// became of the task, which a store that took the report and refused the writes after it does not
// hold: the turn's start (startedUnseen) and then the task's finish, for a report that carried no
// provider error — a finish that landed would have retired the task — or the take-back and the
// ladder's step, for one that carried a provider error (providerErrored). That daemon kept what
// it owed in memory alone, and nothing else makes it: the worker's report sent again is a repeat,
// which applies nothing (recordTurnOutcome), and a plugin that names its task's number is given no
// end-frame bound (endFrameFinishes).
//
// A report that carried a provider error is owed only while the delivery does not record its
// ladder step (TurnOutcome.ProviderErrorRetries above Delivery.ProviderErrorRetries): once that
// step is written, the task was taken back, and a take of it standing now is the next take, given
// after the ladder's wait, whose turn the report is not of. A report of another generation is of
// a turn of another process than the one whose take this is (reachesLadder), and a task of no
// number (Delivery.Number) is named by no report, so an entry stored before the window kept a
// report's number (TurnOutcome.Task) owes nothing.
func (c Claim) owedReport() (TurnOutcome, bool) {
	p := c.Pending
	if p == nil || p.Number == 0 || !p.taken() {
		return TurnOutcome{}, false
	}
	for i := len(c.TurnOutcomes) - 1; i >= 0; i-- {
		o := c.TurnOutcomes[i]
		if o.Unapplied || o.Generation != c.Generation || o.Task != p.Number {
			continue
		}
		if o.ProviderError && o.ProviderErrorRetries <= p.ProviderErrorRetries {
			return TurnOutcome{}, false
		}
		return o, true
	}
	return TurnOutcome{}, false
}

// RefusedRun is the run of refused turns the claim's reports end with, in the order the turns ran.
// It is derived on every read and stored nowhere, so no count is ever mutated and a report that
// arrives late lands in its place and is counted there. The run carries across generations: a
// session the provider refused is refused again when it is resumed, which is why the retry of a
// claim this failed starts a new session (freshSession), and a new session starts a fresh run
// (freshRun).
func (c Claim) RefusedRun() int {
	run := 0
	for i := len(c.TurnOutcomes) - 1; i >= 0 && c.TurnOutcomes[i].Refused; i-- {
		run++
	}
	return run
}

// LastRefusal is what the provider said about the newest refused turn the claim keeps, and "" when
// it keeps none or the provider said nothing: the sentence the claim's failure names.
func (c Claim) LastRefusal() string {
	for i := len(c.TurnOutcomes) - 1; i >= 0; i-- {
		if c.TurnOutcomes[i].Refused {
			return c.TurnOutcomes[i].Message
		}
	}
	return ""
}

// freshRun drops every turn the claim has reports for, and the window's watermark with them
// (TurnOutcome.Watermark), so its refused run starts again. The run belongs to the session whose
// turns were refused, and a claim leaves a session only by forgetting it (forgetSession): every
// relaunch resumes the session the claim recorded, and a registration naming another is refused,
// so the forgetting is the one moment a fresh run begins.
func (m *Machine) freshRun() { m.claim.TurnOutcomes = nil }

// turnOutcome records the turn its producer reported and derives the claim's trailing run of
// refused turns from the window that leaves. At the launch failure limit the claim fails: the
// provider has refused every turn since the last one it took, so nothing of the issue's work is
// being done and every later turn of that session would be refused the same way
// (dispatch://INFRA-714). A run short of the limit is the report written, and then what the
// report says of the claim's task (reportedTask).
//
// A refused turn that carries a provider error is counted here only when no provider-error
// ladder takes it. The ladder takes the turn of the task a take of this claim holds: a report
// that names that task's number (reachesLadder) starts the ladder's wait and keeps the task
// (reportedTask, providererror.go), and the ladder has no attempt limit (dispatch://INFRA-956 R2)
// and tells the project's controller at a run of its own (providerErrorRunTold), so counting
// such a turn here as well would fail the claim out of the launch budget before that run could
// ever be reached. A refused turn whose report names no task, or a number no take of the claim
// holds — a reminder's, a card's, a background result's, a late report of a task already
// finished, a report of a process the claim has left — reaches no ladder, and nothing else
// counts it, so it counts toward the refused run whatever error it carries: that is the run of
// refusals INFRA-714 fails the claim at, and since both producers name a provider error beside
// every refused turn (claudebridge's turnEnded, pi-legion's turn-outcome), it is the only way
// such a run is counted at all. A refused turn that names no provider error counts as it always
// has.
//
// A repeat (recordTurnOutcome) is written where the window still holds it, and goes no further,
// whether the window holds it or its watermark covers it. The producer sends a report again
// when its first attempt had no answer, which a daemon restart in the middle of the request
// leaves too, so the repeat can find a claim the first attempt already changed: a task that
// attempt took back and that the restarted daemon has given again. Applying the repeat to that
// task would take back a take no turn has run on, and count the provider error a second time. A
// repeat that carries a provider error keeps the count its first attempt made, too, and the
// ladder step it recorded (TurnOutcome.ProviderErrorRetries): whether that turn reached a ladder
// was decided against the claim as the first attempt found it, and the take that first attempt
// took back reaches no ladder any more. A report whose write fails is marked as never applied
// (TurnOutcome.Unapplied), so the producer's repeat of it still does what it says of the task.
//
// The message is the provider's text, so it is kept as the daemon keeps a far side's text
// (storetext.FarSideText: made valid, then read whole by the URL rule) before redact.Runes cuts
// it, so the window in memory is the window the store holds, and the cut cannot leave half a
// credential.
func turnOutcome(m *Machine, ctx context.Context, ev Event) error {
	r := ev.(RequestTurnOutcome)
	o := TurnOutcome{Generation: r.Generation, Sequence: r.Sequence, Message: redact.Runes(storetext.FarSideText(r.Message), maxRefusalMessage), Task: r.Task}
	if pe := r.ProviderError; pe != nil {
		o.ProviderError, o.ProviderErrorStatus = true, pe.Status
	}
	reaches := m.reachesLadder(r)
	o.Refused = r.Refused && (r.ProviderError == nil || !reaches)
	if r.ProviderError != nil && reaches {
		o.ProviderErrorRetries = m.claim.Pending.ProviderErrorRetries + 1
	}
	if first, applied := m.appliedTurnOutcome(r); applied && r.ProviderError != nil {
		o.Refused, o.ProviderErrorRetries = first.Refused, first.ProviderErrorRetries
	}
	repeat := m.recordTurnOutcome(o)
	run := m.claim.RefusedRun()
	m.log.Info("supervise: a turn ended", "generation", r.Generation, "sequence", r.Sequence,
		"refused", o.Refused, "providerError", r.ProviderError != nil, "refusedRun", run, "limit", m.deps.Limits.LaunchFailures)
	if run >= m.deps.Limits.LaunchFailures {
		return m.fail(ctx, CauseProviderError, refusalCause(run, m.claim.LastRefusal()))
	}
	if err := m.persist(ctx); err != nil {
		if !repeat {
			m.unappliedTurnOutcome(o)
		}
		return err
	}
	if repeat {
		m.log.Info("supervise: a turn's report repeats one already applied; it changes nothing more",
			"generation", r.Generation, "sequence", r.Sequence, "task", r.Task)
		return nil
	}
	return m.reportedTask(ctx, r)
}

// reportedTask is what a turn-outcome report says of the claim's task. A report of the claim's
// generation that names the number of the task a take holds is that task's turn reported over
// (reachesLadder): one that ends in a provider error keeps the task under its number and starts
// the ladder's wait (providerErrored), and any other finishes it (finishTask) and ends the
// claim's run of provider-error turns. A report that names another number, or none, or that is of
// an earlier generation, finishes nothing: a late report of a task already finished must not
// touch the one that replaced it, a report of a process the claim has left is of no take this
// process holds, and a worker whose plugin names no number has its task finished by the end of
// the turn instead (finishAtTurnsEnd, endFrameFinishes). Where the take's turn had started, a
// report of the claim's generation from a plugin that names its task's number, naming none or
// another, says that turn was not the take's (notTheTakesTurn).
//
// A report can name the accepted take's number before this daemon saw that take's turn start:
// the report comes over HTTP and the start over the worker stream, and nothing orders the two.
// The turn ran all the same, so what its start does for the take is done first (startedUnseen),
// and the task is then finished, or taken back, as the turn's report says. A write of that start
// the store refuses leaves the start standing in memory, owed (startTakesTurn), and the finish or
// the take-back is made all the same: the report is applied already, so the worker's report sent
// again is a repeat that does nothing, and what the report says of the task is done here or
// nowhere. The finish's retirement writes the claim, and the take-back's writes the delivery and
// the claim, so the start is written with them, and its failure fails the decision only where
// theirs fails too; a finish whose retirement the store refused is owed (finishTask), and a
// take-back whose write it refused stands in memory with the ladder's step (providerErrored).
//
// A report that beats the end of its turn finishes the task while the claim is still working,
// and the end then runs what was held for the turn (turnEnded); one that follows the end runs it
// here (afterTurn).
//
// The report of the turn the daemon gave the take can also make the claim's count of its
// process's turns known again, where that count is not known (learnTurns): the report names the
// turn's sequence, and the take's number places it among the turns the machine saw.
func (m *Machine) reportedTask(ctx context.Context, r RequestTurnOutcome) error {
	p := m.claim.Pending
	if !m.reachesLadder(r) {
		if r.Task != 0 {
			m.log.Info("supervise: a turn's report names a task that no take of this claim holds; it finishes nothing",
				"task", r.Task, "generation", r.Generation, "pending", pendingNumber(p))
		}
		return m.notTheTakesTurn(ctx, r)
	}
	var started error
	if p.awaitsTurn() {
		started = m.startedUnseen(ctx, r)
	} else {
		m.learnTurns(ctx, r)
	}
	if r.ProviderError != nil {
		if err := m.providerErrored(ctx, *r.ProviderError); err != nil {
			return errors.Join(started, err)
		}
	} else {
		if m.claim.ProviderErrorRun != 0 {
			m.log.Info("supervise: a turn of this task ended with no provider error; the run of provider errors is over",
				"task", p.Number, "providerErrorRun", m.claim.ProviderErrorRun)
			m.claim.ProviderErrorRun = 0
		}
		if err := m.finishTask(ctx, finishedByReport); err != nil {
			return errors.Join(started, err)
		}
	}
	if (m.claim.State != StateReady && m.claim.State != StateIdle) || m.compacting != nil {
		return nil
	}
	return m.afterTurn(ctx)
}

// reachesLadder is whether a turn-outcome report is of the turn of the task a take of this claim
// holds: it names that task's number, the take stands accepted, its turn started or not, and the
// report is of the claim's current generation. A report of that turn is what finishes the task
// or, carrying a provider error, takes it to the ladder (reportedTask); any other report finishes
// nothing and reaches no ladder, so its refusal is the launch budget's (turnOutcome). A report of
// an earlier generation is of a turn of a process the claim has left, which held no take of this
// process: a take stands only within the generation that accepted it, since the process's end
// settles it (settle, interrupted). A daemon that reads the window back holds to the same rule
// (Claim.owedReport).
func (m *Machine) reachesLadder(r RequestTurnOutcome) bool {
	p := m.claim.Pending
	return r.Task != 0 && r.Generation == m.claim.Generation && p != nil && p.Number == r.Task && p.taken()
}

// notTheTakesTurnLine is the line of a take given its turn back (notTheTakesTurn), at Info, with
// the task's number.
const notTheTakesTurnLine = "supervise: a turn's report names no task of the take whose turn it was given; the take awaits its turn again"

// takesTurnMovedLine is the line of a take whose turn moves to the turn after the one it was
// given (notTheTakesTurn), at Info, with the task's number and the sequence of the turn that is
// the take's now.
const takesTurnMovedLine = "supervise: a turn's report names no task of the take whose turn it was given; the turn after it, started before the report came, is the take's"

// notTheTakesTurn is a turn-outcome report naming no task, or another task's number, of the turn
// the daemon gave the accepted take of the pending task — the first that started after the take
// was accepted (turnStarted) — while the take has its turn started: that turn was not the take's.
// The worker can be answered the take while a run no take started is running, a background
// result's or a reminder's, and it holds the text until that run is over and reports the run as
// no task's; or its answer was lost and it asked again, and was answered the same text after that
// run started (answeredFromRecord). Either way the text's own turn is the turn after the one the
// take was given: the worker starts the held text as soon as that run is over, before it sends
// that run's report (pi-legion's agent_end hook; the Claude bridge's turn end). So where the
// claim's plugin names its task's number (TakesNoTask is false: it reports every turn of a task
// with the number), the take goes back to awaiting its turn: the delivery's turn start is undone
// (TurnStarted) with the text it was answered kept, and the next turn that starts is the take's
// turn. No end-frame bound stands over such a claim's task to finish it meanwhile
// (endFrameFinishes): its report is what finishes it. R5's second wait is on the take again
// from this decision (secondWaitNow, armWaits), and the claim counts as in a turn for another
// role's adoption until that turn ends (Claim.InTurn).
//
// Where the turn after the one the take was given has started by the time the report comes — it
// runs, or it has ended too, which the turns counted say (turnCount.ended) — that turn is the
// take's, and nothing is undone: the take's turn stays started, and is that later turn
// (turnCount.take), so its own report, naming the take's number, is what settles the task, and a
// let-go at or after its end waits for that report (reportSettles) rather than give the task back
// as never run. Put back instead, the take would await a turn it already had: a suspension held
// over that turn would stop the claim at its end and give the task back unstarted, a death would
// give it back with no note, and the turn that starts next — the take's turn already reported or
// a foreign one — would be given the take. The turn lines are left as their starts wrote them, so
// the given turn's names the task and the later one's none: the number is counted one turn
// either way (costreading's turns per number).
//
// The report is of that turn only where its generation and sequence are that turn's
// (turnCount.take), as a count the machine knows says them. A report of any earlier turn says
// nothing of the take: a run's report sent late, after the take's own turn started or after its
// whole turn ran, would otherwise put back a take whose turn is running or over. While the count
// is not known (turnCount.known) — a daemon restarted under the claim's process, a turn that ended
// before the agent said ready, a start over a running turn — a report's sequence says nothing of
// which turn it is of, the turn given while the count was known included, and a guess one turn
// off puts back a take whose turn is running. Then only the producer's own word puts a take back:
// a report saying the take's text was still held unrun at its turn's end (RequestTurnOutcome.Held),
// which heldUnrun reads. A report without that word, from a producer that does not send it or of
// a turn that held no text, puts nothing back while the count is not known. The report of a take's
// own turn that places the count makes it known again (learnTurns), and from the next take on the
// report of the run given a take gives it its turn back as in a daemon that never lost the count.
// While the count is known the word is not read: the count says which turn a report is of, and so
// whether a later turn has started.
//
// A plugin below TaskTakeContract names no number in any report, so for its claim a report
// naming none says nothing of which turn ran, and the end of the take's turn finishes the task
// as before (finishAtTurnsEnd): that is what finishes the task a migrated old worker took
// (decision 8 of dispatch://INFRA-2811). A report of an earlier generation than the claim's is
// of a turn no take of this process held, and says nothing of it either; and a take whose text no
// record kept (Delivery) has no text to await its turn with.
//
// The undoing stands in memory whether or not its write lands: a write that fails leaves the
// delivery unsaved, and settle writes it again.
func (m *Machine) notTheTakesTurn(ctx context.Context, r RequestTurnOutcome) error {
	p := m.claim.Pending
	if p == nil || !p.taken() || p.TakenText == "" || r.Generation != m.claim.Generation ||
		m.endFrameFinishes() {
		return nil
	}
	count := m.counted()
	if !count.known {
		return m.heldUnrun(ctx, r)
	}
	if !p.turnStarted() || count.take == 0 || r.Sequence != count.take {
		return nil
	}
	if count.ended > count.take || (count.ended == count.take && m.inTurn()) {
		count.take++
		m.log.Info(takesTurnMovedLine, "task", p.Number, "sequence", count.take)
		return nil
	}
	return m.putBack(ctx, "count")
}

// putBack puts the pending take back to awaiting its turn (notTheTakesTurn, heldUnrun): the
// delivery's turn start is undone, the text it was answered kept, and the next turn that starts
// is the take's. by names what said the turn given was not the take's, for the line: "count",
// the count of the process's turns, or "held", the producer's word.
func (m *Machine) putBack(ctx context.Context, by string) error {
	p := m.claim.Pending
	m.log.Info(notTheTakesTurnLine, "task", p.Number, "by", by)
	p.TurnStarted, m.turns.take = false, 0
	return m.putPending(ctx)
}

// inTurn is whether a turn of the claim's process is running: the claim is working, and not for
// a compaction alone (Machine.compacting, whose inTurn is a turn started under it).
func (m *Machine) inTurn() bool {
	return m.claim.State == StateWorking && (m.compacting == nil || m.compacting.inTurn)
}

// heldPassedOverLine is the line of a turn's start the daemon gives no take, at Info, with the
// task's number, because the producer said that take's text was held unrun through that turn
// (heldUnrun, turnStarted).
const heldPassedOverLine = "supervise: a turn started that the producer said ran with the accepted take's text held; the turn is no task's, and the take awaits its turn"

// heldUnrun is a report read while the claim's count of its process's turns is not known
// (notTheTakesTurn), for what the producer said of the pending take: the take's text was still
// held unrun at the end of the report's turn (RequestTurnOutcome.Held). The producer holds the
// text from the take's answer until the text starts a turn of its own — pi-shared's take holds it
// through the run going when the answer came (task-take.ts, Accepted "held"), the Claude bridge
// through the turn running then (take.go, bridge.held) — and a report sent before the answer
// reached it names nothing held. So every report naming the take held is of a turn that ended
// before the take's own turn began, and none is of a turn that ran the text, whatever the count
// says or whenever the report comes: that is what makes the word exact, where a count lost to a
// restart is a guess.
//
// What the word does not say is whether the take's own turn has started by the time the report
// comes: a start the machine saw after the reported turn's end may be it, and that turn's own
// report then settles the task, while the take, put back, would await a turn it already had
// (notTheTakesTurn says what that costs). So the word is applied only where no start has followed
// the reported turn's end, and is otherwise moot. The frames carry no sequence, so the machine
// reads that from what it kept at the take's accept (takeAnchor), where the take is the first of
// its number this process made while the count was not known (anchorTake) and its connection is
// still the claim's (onAnchoredConn):
//
//   - the turns the producer said held the text are the ones that ended from the take's answer
//     to the take's own turn, consecutive in the producer's count, and each started after the
//     accept (one that started before it would have had the claim working, and the take refused
//     in-turn), so the turn of the first start after the accept is at most the smallest sequence
//     such a report named (takeAnchor.ceiling);
//   - on one connection no frame is dropped, so the starts the machine sees from that first one
//     on are of consecutive turns, and the turn of the last start seen is at most the ceiling plus
//     the starts seen after the one the daemon gave the take;
//   - where that is at most the highest sequence a report named held (takeAnchor.held), the last
//     start seen is of a turn that held the text, so the take's own turn, which follows every such
//     turn, has not started: the take is put back, and the next start is given to it. Anything
//     else is moot.
//
// A report that comes before the start of the turn given the take is kept (takeAnchor.heldReport)
// and read at that start, which is given no take where it is of a turn that held the text
// (turnStarted). A take of a number taken before — one a provider error's ladder, a compaction or
// the second wait gave back, taken again — has no anchor, so a report its earlier take's hold
// made, sent late, reaches no later take; nor does a report naming another number than the
// pending take's, a report at or below the sequences the anchor already accounts for
// (takeAnchor.reported), or one of a take whose connection the claim no longer has, where a frame
// may have been dropped. Each of them is read as a report without the word.
//
// A take the daemon before this one accepted, which a daemon start restored, is anchored where
// the store shows that daemon can have handled no turn start after the one it gave the take
// (anchorRestored), and its connection is the one the first hello after the start opened
// (bindRestoredAnchor); a report that comes before that hello is kept, and read at it.
func (m *Machine) heldUnrun(ctx context.Context, r RequestTurnOutcome) error {
	p := m.claim.Pending
	a := &m.turns.anchor
	unbound := a.restored && a.conn == 0
	if r.Held == 0 || r.Held != p.Number || a.number != p.Number || r.Sequence <= a.reported ||
		(!unbound && !m.onAnchoredConn()) {
		return nil
	}
	a.heldReport(r.Sequence)
	since, given := m.sinceGiven()
	if !given || unbound {
		return nil
	}
	if !a.unrun(since) {
		m.log.Info("supervise: a turn's report says the accepted take's text was held through it, but a turn may have started after it; the take keeps its turn",
			"task", p.Number, "sequence", r.Sequence)
		return nil
	}
	a.pass(since)
	return m.putBack(ctx, "held")
}

// sinceGiven is how many turn starts the machine has seen after the one it gave the pending take
// (takeAnchor.given), and whether that take has a given turn standing: its turn started, and a
// start of it the anchor placed. A turn running counts as started, a compaction alone does not
// (inTurn).
func (m *Machine) sinceGiven() (uint64, bool) {
	p, a := m.claim.Pending, m.turns.anchor
	if p == nil || !p.turnStarted() || a.given == 0 {
		return 0, false
	}
	seen := m.turns.ended
	if m.inTurn() {
		seen++
	}
	if seen < a.given {
		return 0, false
	}
	return seen - a.given, true
}

// onAnchoredConn is whether the claim's connection is the one the anchored take was accepted on
// (takeAnchor.conn), or, for a take a daemon start restored, the one its first hello opened.
func (m *Machine) onAnchoredConn() bool {
	conn, connected := m.deps.Conns.Conn(m.claim.Token)
	return connected && conn.Sequence() == m.turns.anchor.conn
}

// anchorRestored anchors the take of the pending task a daemon start restored (NewMachine), for
// a report saying that take's text was held to give it its turn back (heldUnrun) as one this
// daemon accepted would be, where that can never put back a take whose own turn ran. The
// producer's word is as exact as ever; what this daemon cannot see is the starts the daemon
// before it handled, whose frames reached that daemon and no other, so it anchors the take only
// where the store shows that daemon handled none after the turn it gave the take:
//
//   - a take awaiting its turn: that daemon handled no start after the accept, or after the report
//     that put the take back (a start it handled then would have been given the take), so every
//     start after that point reaches this daemon, behind the first hello, from the shim's backlog
//     or as the agent sends it, and the first one it sees is the one it gives the take
//     (turnStarted), as for a take it accepted itself; a run of the host's own whose setup spans
//     the restart is such a start;
//   - a take whose turn that daemon gave, where the claim is stored working and records no turn's
//     end after the take's accept (Claim.LastTurnEnd, Delivery.ConfirmedAt): that daemon handled
//     the given turn's start and nothing after it, since it writes every turn's end and start it
//     handles, and frames reach a machine in the order the shim sent them. The given turn counts
//     as the start running at this machine's build (takeAnchor.given 1).
//
// Every other restored take is refused the word: one whose given turn's end that daemon handled
// may have had its own turn start, run and end under that daemon as well, its report still on its
// way, and the store tells that from a take whose own turn is still to come by nothing. Put back
// there, the take's own report would then read it as a turn whose start is still to come
// (startedUnseen), whose mark names it in the next task's start, and a second wait that ran out
// before that report landed would take back a task whose turn ran. So its run of the host's own
// is given back by no report, as before this word, and that is the one take a restart leaves
// unbounded: the store would need the time of the given turn's start to tell the two apart.
//
// A take whose report the store owes (owedReport) is one whose own turn ran, and is not anchored.
// Reports at or below the newest the restored window holds were applied by that daemon or are of
// an earlier take of the number (takeAnchor.reported), and none of them is read. What this leaves
// is a daemon that received frames it never recorded — a write the store refused before it
// stopped, or frames it read and never handled — which no restart can see.
func (m *Machine) anchorRestored() {
	p := m.claim.Pending
	if p == nil || !p.taken() || p.TakenText == "" || m.finishOwed || m.ladderOwed != nil {
		return
	}
	count := m.counted()
	a := takeAnchor{number: p.Number, reported: m.claim.newestReported(), restored: true}
	switch {
	case !p.TurnStarted:
	case m.claim.State == StateWorking && !m.claim.LastTurnEnd.After(p.ConfirmedAt):
		a.given = count.ended + 1
	default:
		return
	}
	count.anchor = a
}

// bindRestoredAnchor gives a restored take's anchor (anchorRestored) the connection of the claim's
// first hello after the daemon start, and reads the reports kept before it (heldUnrun). A first
// hello whose connection is already gone leaves the anchor nothing to read starts on: frames may
// have been lost with it, so the anchor goes.
func (m *Machine) bindRestoredAnchor(ctx context.Context) error {
	a := &m.turns.anchor
	if !a.restored || a.conn != 0 || m.turns.generation != m.claim.Generation {
		return nil
	}
	conn, connected := m.deps.Conns.Conn(m.claim.Token)
	if !connected {
		m.turns.anchor = takeAnchor{}
		return nil
	}
	a.conn = conn.Sequence()
	if since, given := m.sinceGiven(); given && a.unrun(since) {
		a.pass(since)
		return m.putBack(ctx, "held")
	}
	return nil
}

// startedUnseen is the start of the accepted take's turn, done at that turn's report because the
// report came first (reportedTask). The turn is the take's: the delivery records its turn started
// and the claim is written with it (startTakesTurn), so the finish that follows earns the claim
// the serving credit a finished take's turn earns (retireTask) rather than reading the task as
// never run; and the prompt budgets go to zero, as at any turn's start (R5). The report's own
// turn is the one the take was given (turnCount.take).
//
// A ready or idle claim stays where it is: the report says the turn is over, and no turn's times
// are written. Where the start frame of the report's turn is still to come, the machine
// remembers that turn (Machine.reportedAhead), so its start names this task and starts no second
// turn of it (turnStarted). It is still to come where the machine has counted no end of that turn
// (turnCount.ended); one whose end it has counted started before the report came, while the take
// awaited its turn again because an earlier report said the turn it was given was not its own
// (notTheTakesTurn), so no start is still to come for it, and a mark for it would name this task
// in the start of the claim's next task's turn. A claim whose count of its process's turns is not
// known (turnCount.known) marks the next start, as it always did, and its report places no count
// (learnTurns), since the machine gave the take no turn it saw. A working claim's start frame has
// come already: the turn running is the report's.
func (m *Machine) startedUnseen(ctx context.Context, r RequestTurnOutcome) error {
	p := m.claim.Pending
	m.log.Info("supervise: a turn's report names the accepted take's task before its turn start was seen; the turn is the take's",
		"task", p.Number)
	m.resetPromptBudgets()
	count := m.counted()
	if m.claim.State != StateWorking && (!count.known || r.Sequence > count.ended) {
		m.reportedAhead = aheadReport{task: p.Number, sequence: r.Sequence}
	}
	count.take = r.Sequence
	return m.startTakesTurn(ctx)
}

// aheadReport is the turn whose report ran ahead of its start frame (Machine.reportedAhead): the
// task the report named, 0 for none, and the report's sequence.
type aheadReport struct {
	task, sequence uint64
}

// turnCount is a process's turns as this machine counts them from the worker stream, which is
// what tells which turn a turn-outcome report is of (RequestTurnOutcome.Sequence): the producer
// numbers the turns of a launch generation from 1 in the order they end, and the end of every turn
// that ends once its agent is ready reaches the machine as an end frame — of a turn it saw start
// (turnEnded), of one it did not (blindTurnEnded), or of one under a compaction — before the next
// one starts. So the turn starting now is the one after the ends counted, and a report whose
// sequence is at or below that count is of a turn whose start was seen.
//
// generation is the launch generation counted, ended the ends of its turns seen, and take the
// sequence of the turn the daemon gave the pending task's accepted take (turnStarted), or the one
// a report of that take's number named first (startedUnseen); 0 for none. known is whether ended
// is every end of the generation's turns: a machine a daemon start builds over a live process
// missed the turns that ended under the daemon before it (NewMachine), a start that arrives with a
// turn still running says an end was missed (turnStartedWhileBusy), and a turn that ends before
// the agent said ready is one the producer may or may not have numbered (turnEndedBeforeReady).
// A count not known goes on counting every end it sees, so it is the producer's count less the
// ends it missed, and take is read only while the count is known. It reads a report's turn start
// as unseen wherever the claim is not working (startedUnseen), as the machine read every report
// before it counted, and gives a take its turn back only on the producer's word that the take's
// text was held unrun through the report's turn (notTheTakesTurn, heldUnrun); anchor is what a
// take accepted meanwhile saw (takeAnchor), which that word is read against, and the report of
// that take's own turn makes the count known again where it places that turn among the ones the
// machine saw (learnTurns), placed until the agent's answer to the ask that placement makes has
// checked it (placedCount, checkPlacedCount). The next launch counts its generation afresh, known
// (counted).
type turnCount struct {
	generation, ended, take uint64
	known                   bool
	anchor                  takeAnchor
	placed                  placedCount
}

// lose is the count no longer known: what a placement still unchecked added to the ends counted
// goes back, so they are the ends seen again, and a take's anchor goes with it.
func (c *turnCount) lose() {
	c.ended -= c.placed.offset
	c.known, c.anchor, c.placed = false, takeAnchor{}, placedCount{}
}

// counted is the claim's count of its process's turns, begun afresh, known, for a launch
// generation it has not counted yet (turnCount). A count of an earlier generation goes whole,
// known or not, its take's anchor with it: the turns it counted were another process's.
func (m *Machine) counted() *turnCount {
	if m.turns.generation != m.claim.Generation {
		m.turns = turnCount{generation: m.claim.Generation, known: true}
	}
	return &m.turns
}

// takeAnchor is what the machine saw when it accepted the pending task's take while its count of
// the process's turns was not known (anchorTake), for that take's report to place the count
// (learnTurns) and for a report saying its text was held to give it its turn back (heldUnrun):
// number, the number taken, 0 for no anchor; reported, the newest sequence of the claim's
// generation its window of reports held then (Claim.newestReported), or the highest a report
// said held when a report or a start passed the given turn over (pass); conn, the sequence of the
// claim's connection then (runtime.Conn.Sequence); and given, the turn the daemon gave the take as
// the count numbers it (turnStarted: the ends it had counted, plus one), 0 while it has given
// none.
//
// ceiling and held are what the reports of the take's turns said (heldReport): ceiling, an upper
// bound on the producer's sequence of the turn the daemon gave the take, or of the next start
// while it gave none, 0 while none is known; held, the highest sequence of a report that said the
// take's text was held unrun at its turn's end, 0 for none. passed is whether a turn given the
// take, or a start, was passed over since the accept (pass): ceiling then counts on from that
// turn, and a report's sequence no longer lowers it.
//
// restored is a take the daemon before this one accepted, which a daemon start restored
// (anchorRestored): its accept is the point the store shows that daemon handled no start after,
// conn is 0 until the first hello after the start opens one (bindRestoredAnchor), and it places
// no count (learnTurns), since what that daemon counted is not this machine's.
type takeAnchor struct {
	number, reported, conn, given uint64
	ceiling, held                 uint64
	passed, restored              bool
}

// heldReport takes the sequence of a report that said the take's text was held unrun at its
// turn's end. Every such turn started after the accept, so until a turn is passed over the
// smallest such sequence bounds the turn of the first start after the accept (heldUnrun); after
// it, a report that comes late is of a turn that may have started before the one passed over, and
// lowers nothing.
func (a *takeAnchor) heldReport(sequence uint64) {
	if !a.passed && (a.ceiling == 0 || sequence < a.ceiling) {
		a.ceiling = sequence
	}
	a.held = max(a.held, sequence)
}

// unrun is whether the last start the machine has seen, since starts after the one it gave the
// take (or the start being decided, for since 0 with none given), is of a turn the producer said
// held the text: its turn is at most the ceiling plus since, and that is at most the highest
// sequence a report named held.
func (a takeAnchor) unrun(since uint64) bool {
	return a.held != 0 && a.ceiling != 0 && a.ceiling+since <= a.held
}

// pass is the turn the daemon gave the take, and since starts after it, passed over as turns that
// held the text: the next start is of a turn at most one past the last of them, and is the one
// given the take (turnStarted). Every turn up to the highest a report named held has ended, since
// its report came, so the take's own turn's report is right after that one where it follows them
// (learnTurns).
func (a *takeAnchor) pass(since uint64) {
	a.ceiling += since + 1
	a.given, a.passed = 0, true
	a.reported = max(a.reported, a.held)
}

// placedCount is a count of the process's turns a take's report placed (learnTurns) that the
// agent's answer to the ask the placement made has still to check (checkPlacedCount): ask, that
// ask's id (runtime.ConnState.Ask), "" for none; sequence, the turn the daemon gave the take, as
// the placed count numbers it; offset, what the placement added to the ends counted; and conn,
// the sequence of the connection the take was accepted on.
type placedCount struct {
	ask                    string
	sequence, offset, conn uint64
}

// turnsKnownAgainLine is the line of a count of the process's turns a take's report made known
// again (learnTurns), at Info, with the report's sequence and the ends the count now holds.
const turnsKnownAgainLine = "supervise: the report of a take's own turn places the turn the daemon gave it; the count of its process's turns is known again"

// turnsPlacementTakenBackLine is the line of a placed count the agent's answer took back, or that
// the agent did not answer the ask for (learnTurns, checkPlacedCount), at Info, with the sequence
// the count gave the turn given the take and why.
const turnsPlacementTakenBackLine = "supervise: the agent's answer does not bear out the count of its process's turns a take's report placed; the count is not known again"

// anchorTake keeps, for a take just accepted while the claim's count of its process's turns is
// not known, what learnTurns needs to place that take's turn (takeAnchor), and drops what an
// earlier take kept. first is whether the claim had never taken the take's number before
// (Claim.TaskTaken, the number of its last accepted take: numbers rise, and a pending number a
// take holds is the last one taken). A number taken before — a take a provider error, a
// compaction or the second wait gave back, then taken again — keeps nothing, and so does a take
// accepted while the claim has no connection: see learnTurns.
func (m *Machine) anchorTake(first bool) {
	count := m.counted()
	if count.known {
		return
	}
	count.anchor = takeAnchor{}
	conn, connected := m.deps.Conns.Conn(m.claim.Token)
	if !first || !connected {
		return
	}
	count.anchor = takeAnchor{number: m.claim.Pending.Number, reported: m.claim.newestReported(), conn: conn.Sequence()}
}

// learnTurns makes the claim's count of its process's turns known again from the report r of the
// turn the daemon gave the pending task's take (reportedTask), where the report places that turn
// among the turns the machine saw for certain. The frames carry no sequence, so a count lost —
// to a daemon restarted under the process (NewMachine), a turn ended before ready
// (turnEndedBeforeReady), a start over a running turn (turnStartedWhileBusy) — is learned from a
// report or not at all; and a report placed one turn off would leave the count one too high or
// one too low, and either lets the report of another turn give back a take whose own turn is
// running or has run (notTheTakesTurn). A sequence taken as the count whenever the claim is not
// working undercounts wherever a report comes late: a report sent again after a restart, or the
// report of one turn landing after the next one's end.
//
// The report places the turn where all of these hold, and each is why no other turn can be it:
//
//   - it names the number of the take the machine accepted while the count was not known, a number
//     the claim had not taken before (anchorTake): the report is then of the one turn that ran
//     that take's text, which the worker started on the take's answer, after the take — no
//     earlier take of the number left a turn whose late report names it, and no turn that ended
//     before the restart did;
//   - its sequence is the one right after the newest the window held at the take
//     (takeAnchor.reported): every turn up to that one had ended by then, its report having reached
//     the daemon, so the report's turn is the first of the process to start after them. A run of
//     the host's own that started first — the run given a take that notTheTakesTurn is for —
//     leaves the take's own turn a later sequence than this one, whether or not its report has come,
//     but where a report said such a run held the take's text and the turn it was given was passed
//     over (heldUnrun, takeAnchor.pass), reported is the highest such report's sequence, and the
//     take's own turn, which follows every turn that held its text, is the one right after it;
//   - the connection the take was accepted on is still the claim's (takeAnchor.conn): every frame
//     from the take on reached the machine in the order the shim wrote it, none dropped from the
//     backlog of a shim with no daemon to send to, so of the turns that started after the take,
//     or after the last turn passed over, that first turn's start is the first the machine sees;
//   - no start has come since the one the daemon gave the take (takeAnchor.given), at the report
//     and again at the agent's answer to the ask the placement makes (checkPlacedCount): the claim
//     works the given turn, its end still to come, or that end has come and nothing after it.
//
// The last is what tells the given turn from a start that reached the machine late, of a turn
// that ended before the take: one a shim replays behind its hello, of a turn that ran while no
// daemon heard it, or one queued behind the claim's earlier decisions while the report and the
// take came over HTTP, which nothing orders with the frames. The machine cannot tell such a start
// from any other — no frame says it is replayed, and its events come in the order of a live
// turn's — and behind it the take's own turn is the one after it, whose report can come before
// its start does. But the shim sends a turn's start before that turn's report can be sent, so
// before the agent answers an ask made at the report, and the worker stream hands the machine
// that answer behind every frame the shim sent before it, naming the ask it answers
// (StreamTurnAnswered.Ask), so no answer to an earlier ask stands in for it: by then the take's
// own turn's start has come, after the one given, and the placement goes back (lose). An agent
// that does not answer leaves nothing to check by, and the placement goes back at once.
//
// The producer then numbered the given turn r.Sequence where the count numbered it given, so
// every count moves by the difference: the ends seen, and the take's turn. The count stands from
// the report on, since the report of the run given the next take may come before the answer does.
// Until the answer, every start the machine handles is one the shim sent before it, so a take a
// wrong count would give back is one accepted after the report and given such a start, and the
// report that gives it back is of a later turn than that start, which has to run and be reported
// before the answer reaches the machine: the stream behind the reports by a whole run more, under
// which the first start after a take is not that take's turn however the turns are counted
// (turnStarted). Anything that fails a check leaves the count not known, the take's anchor spent
// either way, and the report of a later take's turn may place it.
func (m *Machine) learnTurns(ctx context.Context, r RequestTurnOutcome) {
	count := m.counted()
	anchor := count.anchor
	count.anchor = takeAnchor{}
	if count.known || anchor.number == 0 || anchor.number != r.Task || anchor.given == 0 || anchor.restored {
		return
	}
	conn, connected := m.deps.Conns.Conn(m.claim.Token)
	alone := count.ended < anchor.given || (count.ended == anchor.given && m.claim.State != StateWorking)
	if !connected || conn.Sequence() != anchor.conn || !alone || r.Sequence != anchor.reported+1 || r.Sequence < anchor.given {
		return
	}
	offset := r.Sequence - anchor.given
	count.ended += offset
	count.take, count.known = r.Sequence, true
	count.placed = placedCount{sequence: r.Sequence, offset: offset, conn: anchor.conn}
	m.log.Info(turnsKnownAgainLine, "sequence", r.Sequence, "ended", count.ended)
	state, answered := m.streaming(ctx, conn)
	if !answered || state.Ask == "" {
		m.log.Info(turnsPlacementTakenBackLine, "sequence", r.Sequence, "why", "the agent did not answer whether it is in a turn")
		count.lose()
		return
	}
	count.placed.ask = state.Ask
}

// checkPlacedCount checks a count a take's report placed (placedCount) at the worker stream's
// delivery of the agent's answer to the ask the placement made, which comes behind every frame the
// shim sent before that answer (StreamTurnAnswered); an answer to any other ask checks nothing.
// The count stands where no start has come since the turn the daemon gave the take and the
// take's connection is still the claim's; anything else takes it back (lose). A count of a
// generation the claim has left is the next count's to drop (counted).
func (m *Machine) checkPlacedCount(answer StreamTurnAnswered) {
	count := &m.turns
	placed := count.placed
	if placed.ask == "" || answer.Ask != placed.ask || count.generation != m.claim.Generation {
		return
	}
	conn, connected := m.deps.Conns.Conn(m.claim.Token)
	if !connected || conn.Sequence() != placed.conn {
		m.log.Info(turnsPlacementTakenBackLine, "sequence", placed.sequence, "why", "the take's connection is no longer the claim's")
		count.lose()
		return
	}
	if count.ended > placed.sequence || (count.ended == placed.sequence && m.claim.State == StateWorking) {
		m.log.Info(turnsPlacementTakenBackLine, "sequence", placed.sequence, "why", "a turn started after the one given the take")
		count.lose()
		return
	}
	count.placed = placedCount{}
}

// newestReported is the newest sequence of the claim's generation its window of reports holds,
// and 0 for none: every turn of its process up to that one has ended, since its report came. The
// window keeps the newest reports whatever its bound drops (recordTurnOutcome), in key order.
func (c Claim) newestReported() uint64 {
	if n := len(c.TurnOutcomes); n > 0 && c.TurnOutcomes[n-1].Generation == c.Generation {
		return c.TurnOutcomes[n-1].Sequence
	}
	return 0
}

// pendingNumber is the number of the task pending, 0 for none.
func pendingNumber(p *Delivery) uint64 {
	if p == nil {
		return 0
	}
	return p.Number
}

// refusalCause is the failure a spent run of refused turns names: what refused the claim, how many
// turns in a row, and the provider's own words for the newest of them, which is the one fact a
// person reading the failed claim can act on. The wording is the architect's own
// (dispatch://INFRA-714/artifact/review-rounds-md, item 2 of the refusal section), and the
// architect's notice of the failure quotes it.
func refusalCause(run int, message string) string {
	if message == "" {
		return fmt.Sprintf("the provider refused %d turns in a row, and said nothing about why", run)
	}
	return fmt.Sprintf("the provider refused %d turns in a row: %s", run, message)
}

// freshSession is whether a retry starts a new session rather than relaunching the recorded one,
// and why, for the line it logs. An operator asks for one with `legion claims retry
// --fresh-session`. A claim the provider refused out of its budget takes one whoever retried it —
// its architect's retry_or_escalate, the next start of its role, or that operator — because
// resuming the session those turns were refused in is what would be refused again.
func freshSession(ev Event, c Claim, limit int) (bool, string) {
	asked := false
	switch ev := ev.(type) {
	case RequestRetry:
		asked = ev.FreshSession
	case RequestOperatorRetry:
		asked = ev.FreshSession
	}
	switch {
	case asked:
		return true, "the retry asked for a new session"
	case c.RefusedRun() >= limit:
		return true, "the provider refused the recorded session out of its launch budget"
	}
	return false, ""
}
