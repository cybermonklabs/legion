package supervise

import (
	"cmp"
	"context"
	"errors"
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
// (ProviderErrorStatus, 0 for none) and the class the producer gave it (ProviderErrorCode,
// RequestTurnOutcome.Code: codeCyberPolicy, codeCostCap or none). ProviderErrorRetries is, for a report whose
// provider error reached the ladder (Claim.reportOfTake), the count of the task's sends the
// ladder's step makes it (Delivery.ProviderErrorRetries), and 0 for every other report. They are
// what tells a daemon that read the window back what an applied report did to the claim's pending
// task, where the store holds the report and not what it did (Claim.owedReport): a report is
// applied with its own write, before its finish or its take-back is written (turnOutcome,
// reportedTask), so a store that took the one and refused the other leaves a window naming an
// effect nobody has made, and the worker's report sent again is a repeat that makes none. An entry
// stored before they were kept reads back with none of them, a report naming no task, which did
// nothing to any task; one stored before the class was kept reads back with none, which a
// cyber-policy refusal's report could not have been, since that daemon read no class.
type TurnOutcome struct {
	Generation           uint64 `json:"generation"`
	Sequence             uint64 `json:"sequence"`
	Refused              bool   `json:"refused"`
	Message              string `json:"message,omitempty"`
	Unapplied            bool   `json:"unapplied,omitempty"`
	Task                 uint64 `json:"task,omitempty"`
	ProviderError        bool   `json:"providerError,omitempty"`
	ProviderErrorStatus  int    `json:"providerErrorStatus,omitempty"`
	ProviderErrorCode    string `json:"providerErrorCode,omitempty"`
	ProviderErrorRetries int    `json:"providerErrorRetries,omitempty"`
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

// maxRefusalMessage bounds, in runes, the provider's message a report carries. The message is
// kept with its report in the claim's window, which the store holds, so a provider that returns a
// page of diagnostics leaves a sentence rather than a page in the store. A message past the bound
// keeps its first maxRefusalMessage-1 runes and "…" (redact.Runes).
const maxRefusalMessage = 500

// turnOutcomeWindow is how many reports a claim keeps: the launch failure limit
// (launch_failure_limit, 3 by default). RefusedRun reads only the newest of them and counts up to
// that bound, so a longer run reads as the limit, and a report older than all of them cannot
// shorten a run derived over them, and is dropped. The run is what the daemon answers each report
// with (claim.TurnOutcomeResponse's RefusedRun, api's turnOutcome), for the producer's log, and it
// decides nothing: no claim fails on it, and no retry reads it (freshSession). That bound is why a
// claim whose agent works for a week carries a handful of rows rather than a turn-by-turn history
// nothing reads.
func (m *Machine) turnOutcomeWindow() int { return m.deps.Limits.LaunchFailures }

// recordTurnOutcome puts a report in the claim's window, in (generation, sequence) order, and
// trims the window to its bound (turnOutcomeWindow). A report whose key the window already holds
// replaces it and answers repeat: the producer sent it again because it heard no answer, and its
// first attempt was applied. The one exception is a first attempt whose write failed
// (TurnOutcome.Unapplied), which never reached the task, so the report sent again is its first
// application rather than a repeat, and what a report decides (reportedTask) is so decided once
// for its turn. No count is kept in the window: every reader derives what it needs from it as it
// stands (RefusedRun), which is what makes a late report and an in-order one leave the same
// answer.
//
// A report the window no longer holds, at or below its watermark (Claim.TurnOutcomeMark), is a
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
// until the gap is filled or the window reaches turnOutcomeGapBound, and a later generation ends
// no such hold: the bound drops from the oldest entry, and the oldest is the earlier generation's
// report above its gap, which does not follow the watermark (follows) whatever reports land after
// it, so every report of the later generation is held behind it up to that bound too. With a limit
// of 3, a generation that reports 1, 2, 3 and 5 and then one that reports 1 to 10 leave 11
// entries, the oldest (1, 5) and the watermark (1, 3). Nothing reads them wrongly: a report of an
// earlier generation reaches no take of the process the claim runs now (Claim.reportOfTake), and the
// run of refused turns ends with the newest entry (RefusedRun). One whose write failed (Unapplied)
// stays until it is sent again and applied, however old: dropped, it would fall under the
// watermark, and the report sent again, which is its first application, would be answered as a
// repeat. So does the report that named the pending take's number with no provider error while
// the finish it decided is owed (Machine.finishOwed): a daemon start reads it to make that finish
// (owedReport), and dropped, the start would read a later provider error's report naming that
// number, which takes the task to the ladder, or none, which leaves it unfinished. Both are held
// beside the bound rather than in it (keptPastBound), so the run of refused turns still reads the
// newest reports the bound keeps: in the bound, either would cut a run of refusals one short of the
// limit for as long as it stayed. A report never applied of a generation earlier than the claim's
// is dropped as an applied one is, since no process sends it again and it reaches no take
// (reportOfTake), so a producer that died before its report was sent again leaves no entry for
// good; except while it is in the trailing run of refused turns, which counts across generations
// (RefusedRun) and would be cut one short without it.
func (m *Machine) recordTurnOutcome(o TurnOutcome) (repeat bool) {
	window, mark := m.claim.TurnOutcomes, m.claim.TurnOutcomeMark
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
	for {
		runFrom := len(window) - refusedRun(window)
		bound, oldest := m.turnOutcomeWindow(), -1
		for i, held := range window {
			switch {
			case m.keptPastBound(held, i >= runFrom):
				bound++
			case oldest < 0:
				oldest = i
			}
		}
		if len(window) <= bound {
			break
		}
		// Past the bound, the entries not kept outnumber the window's limit, so oldest is one.
		if dropped := window[oldest].key(); compareKeys(dropped, mark) > 0 {
			if !gapless(mark, window[:oldest+1]) && len(window) <= turnOutcomeGapBound {
				break
			}
			mark = dropped
		}
		window = slices.Delete(window, oldest, oldest+1)
	}
	m.claim.TurnOutcomes, m.claim.TurnOutcomeMark = window, mark
	return repeat
}

// keptPastBound is whether the window keeps o whatever reports land after it, held beside its
// bound rather than in it (recordTurnOutcome): a report whose write failed (Unapplied), until it is
// sent again and applied, where it is of the claim's generation or in the trailing run of refused
// turns (inRefusedRun); and, while the finish it decided is owed (Machine.finishOwed), the applied
// report of the pending take's turn (reportOfTake) that carried no provider error, which a daemon
// start reads to make that finish (owedReport).
func (m *Machine) keptPastBound(o TurnOutcome, inRefusedRun bool) bool {
	if o.Unapplied {
		return o.Generation >= m.claim.Generation || inRefusedRun
	}
	return m.finishOwed && !o.ProviderError && m.claim.reportOfTake(o.Generation, o.Task)
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
// the process it runs now (Claim.reportOfTake). The zero key, no watermark, is followed by the first
// report of any generation.
func follows(last, key TurnOutcomeKey) bool {
	if key.Generation == last.Generation {
		return key.Sequence == last.Sequence+1
	}
	return key.Generation > last.Generation && key.Sequence == 1
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
	window := m.claim.TurnOutcomes
	at, found := slices.BinarySearchFunc(window, o, compareTurnOutcomes)
	if found {
		window[at].Unapplied = true
		return
	}
	o.Unapplied = true
	m.claim.TurnOutcomes = slices.Insert(window, at, o)
}

// owedReport is the report whose effect on the claim's pending task the store does not hold, read
// from the claim as the store keeps it, for the machine a daemon start builds over it
// (NewMachine): an applied report of the claim's current generation that names the pending task's
// number, while that task's take stands accepted, its turn started or not. Such a report is of the
// take's turn (reportOfTake), and the daemon that applied it decided there what became of the
// task, which a store that took the report and refused the writes after it does not hold: the
// turn's start (startedUnseen) and then the task's finish, for a report that carried no provider
// error — a finish that landed would have retired the task — or the take-back and the ladder's
// step, for one that carried a provider error (providerErrored). That daemon kept what it owed in
// memory alone, and nothing else makes it: the worker's report sent again is a repeat, which
// applies nothing (recordTurnOutcome), and a plugin that names its task's number is given no
// end-frame bound (TakesNoTask).
//
// A report that carried no provider error is read before any later one that did: its finish,
// refused, was owed from then on (Machine.finishOwed), and a later report naming the number with
// a provider error finished the task by it rather than take it to the ladder (takeToLadder,
// waitFinish), so that later report decided nothing of its own. The window keeps such a report
// while its finish is owed for this read (recordTurnOutcome). Otherwise the newest report naming
// the number is the one owed.
//
// A report that carried a provider error is owed only while the delivery does not record its
// ladder step (TurnOutcome.ProviderErrorRetries above Delivery.ProviderErrorRetries): once that
// step is written, the task was taken back, and a take of it standing now is the next take, given
// after the ladder's wait, whose turn neither that report nor any before it is of. A report of
// another generation is of a turn of another process than the one whose take this is, and a task
// of no number (Delivery.Number) is named by no report (reportOfTake), so an entry stored before
// the window kept a report's number (TurnOutcome.Task) owes nothing. A claim with no task pending
// owes no report.
func (c Claim) owedReport() (TurnOutcome, bool) {
	p := c.Pending
	if p == nil {
		return TurnOutcome{}, false
	}
	var errored TurnOutcome
	found := false
	for i := len(c.TurnOutcomes) - 1; i >= 0; i-- {
		o := c.TurnOutcomes[i]
		if o.Unapplied || !c.reportOfTake(o.Generation, o.Task) {
			continue
		}
		if !o.ProviderError {
			return o, true
		}
		if o.ProviderErrorRetries <= p.ProviderErrorRetries {
			break
		}
		if !found {
			errored, found = o, true
		}
	}
	return errored, found
}

// RefusedRun is the run of refused turns the claim's reports end with, in the order the turns ran.
// It is derived on every read and stored nowhere, so no count is ever mutated and a report that
// arrives late lands in its place and is counted there. It is what the daemon answers each report
// with (claim.TurnOutcomeResponse's RefusedRun, api's turnOutcome), which the producer logs, and
// the retry that leaves a session behind logs it too (retry); it decides nothing. The run carries
// across generations, as a resumed session's turns follow the ones before them, and a new session
// starts a fresh run (freshRun).
func (c Claim) RefusedRun() int { return refusedRun(c.TurnOutcomes) }

// refusedRun is the run of refused turns window ends with (Claim.RefusedRun), for the trim that
// keeps that run whole (recordTurnOutcome).
func refusedRun(window []TurnOutcome) int {
	run := 0
	for i := len(window) - 1; i >= 0 && window[i].Refused; i-- {
		run++
	}
	return run
}

// LastRefusal is what the provider said about the newest refused turn the claim keeps, and "" when
// it keeps none or the provider said nothing. The retry that leaves a refused session behind logs
// it (retry). The words are stored for more than that line: for the take's turn, whose end report
// names the take's number (reportedTask), and for a turn that ran none of the daemon's tasks, a
// phase reminder among them, alike, the turn's report is the daemon's one record of what the
// provider answered that turn. The report's status, never these words, is what judges the turn
// (turnOutcome).
func (c Claim) LastRefusal() string {
	for i := len(c.TurnOutcomes) - 1; i >= 0; i-- {
		if c.TurnOutcomes[i].Refused {
			return c.TurnOutcomes[i].Message
		}
	}
	return ""
}

// freshRun drops every turn the claim has reports for, and the window's watermark with them
// (Claim.TurnOutcomeMark), so its refused run starts again. The run belongs to the session whose
// turns were refused, and a claim leaves a session only by forgetting it (forgetSession): every
// relaunch resumes the session the claim recorded, and a registration naming another is refused,
// so the forgetting is the one moment a fresh run begins.
func (m *Machine) freshRun() { m.claim.TurnOutcomes, m.claim.TurnOutcomeMark = nil, TurnOutcomeKey{} }

// turnOutcome records the turn its producer reported and derives the claim's trailing run of
// refused turns from the window that leaves. The run fails no claim, however long
// (dispatch://INFRA-4508), and decides nothing: it is what the daemon answers the report with
// (RefusedRun). The report written, what it says of the claim's task is done next (reportedTask).
//
// What a refused turn's provider error does is decided once for each turn, by its report, in one
// of two ways (reportedTask). The report that names the number of the task a take holds
// (Claim.reportOfTake) is that take's turn: its provider error keeps the task under its number and
// starts the ladder's wait (providerErrored, providererror.go). The report of any other turn of
// the generation the claim runs now — one that names no task, or a number no take of the claim
// holds: a reminder's, a card's, a background result's, a late report of a task already finished
// — keeps no task, and its refusal is judged as that turn's alone (unpromptedTurnEnded). A report
// marked unprompted says its turn ran no task the daemon handed, which a plugin from before the
// task's number sends (claim.TurnOutcomeRequest), and is read as one naming no task whatever its
// Task says: its refusal is of the second kind, and it finishes nothing. Either way the turn counts
// in the claim's one run of provider-error turns (ProviderErrorRun), whose providerErrorRunTold-th
// is handed to Deps.ProviderErrorRunReached once, and a refusal that cannot clear (classifyProviderError) —
// a status that cannot clear, or one the producer classed as OpenAI's cyber-policy refusal
// (RequestTurnOutcome.Code), whatever its status — fails the claim at once, under
// CauseProviderError or CauseCyberPolicy, so the workflow holds the issue for its architect, and
// the answer's failed says so (HandleTurnOutcome); the take's task, where the report is of its
// turn, is taken back first (takeBackAndFail). A refusal that can clear, or whose report carries
// no status and no such class, fails nothing: the take's task goes on the ladder, and any other
// turn's refusal is counted and nothing more. A current plugin names a provider error
// beside every refused turn (claudebridge's turnEnded, pi-legion's turn-outcome); a plugin from
// before the task's number names the refused outcome and its status (RequestTurnOutcome's
// refusedByProvider and status). The window's run of refused turns counts every refused report,
// the take's among them, since it decides nothing. A report of a generation the claim has left is
// of a turn of a process that is gone: it is recorded in its place and judged by nothing, and it
// reaches no take (reportOfTake).
//
// A clean report ends that run whoever ran the turn (providerErrorRunOver), since the provider
// answered that turn either way; which clean report finishes a task is reportedTask's, the one
// naming the take's number. It ends the run only when it is the newest report the window holds
// (newestTurnOutcome): a clean report that reaches the daemon after a later turn's report is of a
// turn before that one, and ends no later run whose report has arrived.
//
// A repeat (recordTurnOutcome) is written where the window still holds it, and goes no further,
// whether the window holds it or its watermark covers it. The producer sends a report again
// when its first attempt had no answer, which a daemon restart in the middle of the request
// leaves too, so the repeat can find a claim the first attempt already changed: a task that
// attempt took back and that the restarted daemon has given again. Applying the repeat to that
// task would take back a take no turn has run on, and count the provider error a second time. A
// repeat that carries a provider error keeps the ladder step its first attempt recorded, too
// (TurnOutcome.ProviderErrorRetries): whether that turn reached a ladder was decided against the
// claim as the first attempt found it, and the take that first attempt took back reaches no
// ladder any more. A report whose write fails is marked as never applied
// (TurnOutcome.Unapplied), so the producer's repeat of it still does what it says of the task.
//
// The message is the provider's text, so it is kept as the daemon keeps a far side's text
// (storetext.FarSideText: made valid, then read whole by the URL rule) before redact.Runes cuts
// it, so the window in memory is the window the store holds, and the cut cannot leave half a
// credential.
func turnOutcome(m *Machine, ctx context.Context, ev Event) error {
	r := ev.(RequestTurnOutcome)
	m.turnOutcomeFailed = false
	if r.Unprompted && r.Task != 0 {
		m.log.Info("supervise: a turn's report is marked unprompted and names a task; it is read as naming none",
			"task", r.Task, "generation", r.Generation, "sequence", r.Sequence)
		r.Task = 0
	}
	o := TurnOutcome{Generation: r.Generation, Sequence: r.Sequence, Refused: r.Refused,
		Message: redact.Runes(storetext.FarSideText(r.Message), maxRefusalMessage), Task: r.Task}
	if r.ProviderError != nil {
		o.ProviderError, o.ProviderErrorStatus, o.ProviderErrorCode = true, r.status(), r.Code
	}
	if r.ProviderError != nil && m.claim.reportOfTake(r.Generation, r.Task) {
		o.ProviderErrorRetries = m.claim.Pending.ProviderErrorRetries + 1
	}
	if first, applied := m.appliedTurnOutcome(r); applied && r.ProviderError != nil {
		o.ProviderErrorRetries = first.ProviderErrorRetries
	}
	repeat := m.recordTurnOutcome(o)
	m.log.Info("supervise: a turn ended", "generation", r.Generation, "sequence", r.Sequence,
		"refused", o.Refused, "providerError", r.ProviderError != nil, "httpStatus", r.status(), "code", r.Code,
		"unprompted", r.Unprompted, "task", r.Task, "refusedRun", m.claim.RefusedRun())
	if !repeat && r.Generation == m.claim.Generation && !r.refusedByProvider() && m.newestTurnOutcome(o) {
		m.providerErrorRunOver("supervise: a turn ended with no provider error; the run of provider errors is over",
			"generation", r.Generation, "sequence", r.Sequence, "task", r.Task, "unprompted", r.Unprompted)
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
// (Claim.reportOfTake): one that ends in a provider error keeps the task under its number and starts
// the ladder's wait (providerErrored), and any other finishes it (finishTask); a clean one ended
// the claim's run of provider-error turns already (turnOutcome). A report that names another
// number, or none, or that is of an earlier generation, finishes nothing: a late report of a task
// already finished must not touch the one that replaced it, a report of a process the claim has
// left is of no take this process holds, and a worker whose plugin names no number has its task
// finished by the end of the turn instead (finishAtTurnsEnd, TakesNoTask). Where the take's turn
// had started, a report of the claim's generation from a plugin that names its task's number,
// naming none or another, says that turn was not the take's (notTheTakesTurn). Such a report of
// the claim's generation that carries a provider error is then judged as its own turn's
// (unpromptedTurnEnded): counted in the claim's run, failing the claim for a status that cannot
// clear, and writing no task again.
//
// A report can name the accepted take's number before this daemon saw that take's turn start:
// the report comes over HTTP and the start over the worker stream, and nothing orders the two.
// The turn ran all the same, so what its start does for the take is done first (startedUnseen),
// where the take awaits its turn as the one rule of where it stands reads it (takeAwaitsTurn),
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
// here (settleThenAfterTurn, afterTurn).
//
// The report of the turn the daemon gave the take can also make the claim's count of its
// process's turns known again, where that count is not known (learnTurns): the report names the
// turn's sequence, and the take's number places it among the turns the machine saw.
//
// A cost-cap report can also describe a drop before the turn starts. If the take still awaits
// its start, do not infer a start or reserve the next frame for it. A drop uses a report sequence
// but sends no frame. A report that beat its start frame looks the same. The turn count is no
// longer known until a later take's report places it (learnTurns).
func (m *Machine) reportedTask(ctx context.Context, r RequestTurnOutcome) error {
	p := m.claim.Pending
	if !m.claim.reportOfTake(r.Generation, r.Task) {
		if r.Task != 0 {
			m.log.Info("supervise: a turn's report names a task that no take of this claim holds; it finishes nothing",
				"task", r.Task, "generation", r.Generation, "pending", pendingNumber(p))
		}
		err := m.notTheTakesTurn(ctx, r)
		if r.Generation != m.claim.Generation || !r.refusedByProvider() {
			return err
		}
		return errors.Join(err, m.unpromptedTurnEnded(ctx, r))
	}
	return m.settleThenAfterTurn(ctx, func() error {
		var started error
		if m.takeAwaitsTurn() {
			if r.Code == codeCostCap && r.ProviderError != nil {
				m.counted().lose()
			} else {
				started = m.startedUnseen(ctx, r)
			}
		} else {
			m.learnTurns(ctx, r)
		}
		if r.ProviderError != nil {
			if err := m.providerErrored(ctx, r); err != nil {
				return errors.Join(started, err)
			}
			return nil
		}
		if err := m.finishTask(ctx, finishedByReport); err != nil {
			return errors.Join(started, err)
		}
		return nil
	})
}

// reportOfTake is whether a report of the given generation naming task is of the turn of the take
// the claim's pending task holds, the one rule of it: it names that task's number, the take stands
// accepted, its turn started or not, and the report is of the claim's current generation. A report
// of that turn is what finishes the task or, carrying a provider error, takes it to the ladder
// (reportedTask); any other report finishes nothing and reaches no ladder, so its refusal is
// judged as its own turn's (unpromptedTurnEnded). A daemon that reads the window back asks the
// same rule of each report it holds (owedReport). A report of an earlier generation is of a turn
// of a process the claim has left, which held no take of this process: a take stands only within
// the generation that accepted it, since the process's end settles it (settle, interrupted). A
// report naming no number is of no take's turn, and a task of no number (Delivery.Number) is
// named by no report.
func (c Claim) reportOfTake(generation, task uint64) bool {
	p := c.Pending
	return task != 0 && generation == c.Generation && p != nil && p.Number == task && p.taken()
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
// (TakesNoTask): its report is what finishes it. R5's second wait is on the take again
// from this decision (secondWaitNow, armWaits), and the claim counts as in a turn for another
// role's adoption until that turn ends (Claim.InTurn). Work held for the report of the turn given
// the take goes on waiting, now for the take's own turn or for that second wait's bound
// (reportwait.go, putBack).
//
// Where the turn after the one the take was given has started by the time the report comes — it
// runs, or it has ended too, which the turns counted say (turnCount.ended) — that turn is the
// take's, and nothing is undone: the take's turn stays started, and is that later turn
// (turnCount.take), so its own report, naming the take's number, is what settles the task, and a
// let-go at or after its end waits for that report (reportwait.go) rather than give the task back
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
// record kept (Delivery) has no text to await its turn with. A take whose report naming its number
// came and whose finish the store refused is not put back either (putBack, waitFinish).
//
// The undoing stands in memory whether or not its write lands: a write that fails leaves the
// delivery unsaved, and settle writes it again.
func (m *Machine) notTheTakesTurn(ctx context.Context, r RequestTurnOutcome) error {
	p := m.claim.Pending
	if p == nil || !p.taken() || p.TakenText == "" || r.Generation != m.claim.Generation || TakesNoTask(m.claim.PluginContract, m.deps.TaskTakeContract) {
		return nil
	}
	count := m.counted()
	if !count.known {
		return m.heldUnrun(ctx, r)
	}
	if !p.startRecorded() || count.take == 0 || r.Sequence != count.take {
		return nil
	}
	if m.startedSince(count.take) {
		count.take++
		m.log.Info(takesTurnMovedLine, "task", p.Number, "sequence", count.take)
		return nil
	}
	return m.putBack(ctx, "count")
}

// putBack puts the pending take back to awaiting its turn (notTheTakesTurn, heldUnrun,
// bindRestoredAnchor): the delivery's turn start is undone, the text it was answered kept, and the
// next turn that starts is the take's. by names what said the turn given was not the take's, for
// the line: "count", the count of the process's turns, or "held", the producer's word.
//
// It decides the wait for the take's report again rather than end it (reportwait.go): no report
// of the run given the take is owed any more, but the take's own turn's is, and that turn may
// already have started, its start frame on its way behind the report. So work held for the
// report — a held suspension, a stale-plugin relaunch, a held replacement, the operator's held
// stop — goes on waiting, for that turn's start or for R5's second bound, which this decision arms
// from here (secondWaitNow), and the wait's own bound goes (armReportWait).
//
// It puts back no take whose report naming its number came and whose finish the store refused
// (takePhase, waitFinish), whichever of its three callers asks: that report decides, the take
// stays where it put it, and the end-frame bound makes its finish again. Put back, the take would
// await a turn that has run, and the start of that turn, late, would be given it as a second run
// of the task.
func (m *Machine) putBack(ctx context.Context, by string) error {
	p := m.claim.Pending
	if p == nil || m.takePhase() == waitFinish {
		return nil
	}
	m.log.Info(notTheTakesTurnLine, "task", p.Number, "by", by)
	p.TurnStarted, m.turns.take = false, 0
	if m.workHeld() {
		m.log.Info(putBackWaitsLine, "task", p.Number, "suspension", m.held != nil, "relaunch", m.staleRelaunch != nil,
			"replacement", m.heldReplacement != nil, "stop", m.heldStop)
	}
	return m.putPending(ctx)
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
// (learnTurns), since the machine gave the take no turn it saw; except where a hello of the
// process said its shim's backlog is not whole (turnCount.cut, readBacklog): the start may be
// among the frames the backlog dropped, the take's whole turn run while no daemon was connected,
// and the mark would take the next start, the next task's own among them. There no start is
// marked, and one of the reported turn that does come is read as any other (turnStarted): given
// to a take that awaits its turn, which its own report then settles, or no task's, its line naming
// no task. A claim in a turn marks nothing: its start frame has come already, and the turn
// running is the report's. A claim working for no turn — a compaction with no turn started under
// it, or a take a handoff may still run over (workingForNoTurn) — marks the start still to come as
// a ready or idle claim does, since that start is given the take as theirs is (turnStartedWhileBusy).
func (m *Machine) startedUnseen(ctx context.Context, r RequestTurnOutcome) error {
	p := m.claim.Pending
	if p == nil {
		return nil
	}
	m.log.Info("supervise: a turn's report names the accepted take's task before its turn start was seen; the turn is the take's",
		"task", p.Number)
	m.resetPromptBudgets()
	count := m.counted()
	toCome := r.Sequence > count.ended
	if !count.known {
		toCome = !count.cut
	}
	if (!m.busy() || m.workingForNoTurn()) && toCome {
		m.reportedAhead = aheadReport{task: p.Number, sequence: r.Sequence}
	}
	count.take = r.Sequence
	return m.startTakesTurn(ctx)
}

// pendingNumber is the number of the task pending, 0 for none.
func pendingNumber(p *Delivery) uint64 {
	if p == nil {
		return 0
	}
	return p.Number
}

// newestTurnOutcome is whether o is the newest report the claim's window holds, in the order the
// turns ran (compareTurnOutcomes): no report of a later turn has reached the daemon before it.
func (m *Machine) newestTurnOutcome(o TurnOutcome) bool {
	window := m.claim.TurnOutcomes
	return len(window) > 0 && compareTurnOutcomes(window[len(window)-1], o) == 0
}

// freshSession is whether a retry of c starts a new session rather than relaunching the recorded
// one, and why, for the line it logs: when the retry asks for one, with `legion claims retry
// --fresh-session` or the controller's fresh_session, since a new session is an operator's or the
// controller's choice (dispatch://INFRA-956's Design), and for every retry of a claim held for
// OpenAI's cyber-policy refusal (Claim.HeldForCyberPolicy), whoever makes it. Every other retry —
// its architect's retry_or_escalate, the next start of its role, an operator's retry that asks for
// nothing — resumes the recorded session with fresh budgets, but for the two cases at the end,
// whatever the claim's run of refused turns (dispatch://INFRA-4508's R5). That run is no sign that
// the session is bad: a refusal that cannot clear fails the claim on the turn it ends, and holds
// the issue, so a run is mostly refusals that can clear or carry no status, such as a team limit's
// 429, which refuses every session of the team alike. A claim failed on a refusal that cannot clear
// resumes too; if its session is refused again, it fails again at once.
//
// A cyber-policy refusal is the one failure that a resume would repeat by itself: the retry's
// first task is input from outside the session, which ends Oh My Pi's hold, so the session's
// refused content goes to OpenAI again, the claim fails again, and its architect, told again,
// retries again. The retries that reach such a claim are the ones someone decided (retry's
// RetryHeld refuses the rest), and none of them decided to send that content again, so each
// leaves the session behind: the claim's failure sentence is the reason the abandoned session is
// recorded under (retry), and the run of provider-error turns starts over with it
// (Machine.forgetSession).
//
// The two cases at the end are the project controller's, and each is decided in the launch, not
// here: whatever this answers, the launch after the session's second end in a row since a
// finished turn forgets the session and spawns a fresh agent (launchNow's replaceEndedSession,
// dispatch://INFRA-2045 R1), and so does a launch no operator's request raised — the keeper's
// retry among them — of a session older than its age (replaceAgedSession, R3).
func freshSession(c Claim, ev Event) (bool, string) {
	asked := false
	switch ev := ev.(type) {
	case RequestRetry:
		asked = ev.FreshSession
	case RequestAskedRetry:
		asked = ev.FreshSession
	}
	switch {
	case asked:
		return true, "the retry asked for a new session"
	case c.HeldForCyberPolicy():
		return true, "the claim failed on OpenAI's cyber_policy refusal of its session's content, which a relaunch of that session would send again"
	}
	return false, ""
}
