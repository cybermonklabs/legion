package supervise

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// owedClaimWrite is one claim write the store did not take and the machine still owes: the whole
// of what a failed write leaves behind, held on the Machine until the write is made. It carries
// nothing of the claim, because the claim the repair writes is the one the machine holds when
// the repair runs rather than a copy taken when the write failed (repair), and it names no store
// method either: which call the repair makes is decided afresh from what the claim holds then,
// and the method of each failure is in that failure's own log line. What is left is the durable
// reports those failed decisions never made, the pause stops they never recorded, a retirement
// whose delete they never made, and what those decisions do outside the machine while the window
// after a refused write holds their writes back.
type owedClaimWrite struct {
	// reports are the durable reports the failed decisions owed, oldest first, made when the
	// write they waited on lands — at a repair, or at any claim write of a later decision that
	// lands first, which is the repair too (settleOwedWrite, makeOwedReports). There is one per
	// decision that owed a report rather than one per mark: a second failing decision while the
	// first is still owed adds its report here instead of replacing it, since the two name
	// different transitions and the daemon acts on each.
	reports []owedReport
	// stops are the project pause's stops whose transaction did not land (Deps.PauseStopped,
	// decision 6 of dispatch://INFRA-3759's plan), oldest first and one per pause, each with the
	// pause's id and whether the role is owed a launch. While any is owed, every claim write the
	// machine makes is that transaction (putStops): the repair, and each write that settles this
	// mark when it lands — putClaim, putClaimAndPending, and retirePending, which writes them
	// before its retirement — so no write of a stopped claim lands without the pause's records,
	// and the one that lands is the repair, as this mark's rule is. A write that settles no mark
	// leaves them where they are: queue, takeBackPending, interrupted and providerErrorClears
	// write the claim beside a delivery and settle nothing, and an admission (revive) and a
	// release of names (ReleaseNames) that land while a stop is owed leave the mark standing, so
	// the stop stays owed and the next repair makes it. Each goes off the mark as its own
	// transaction lands, so a stop of one pause is never written twice for a later one that
	// fails.
	stops []PauseStop
	// retire is the id of the delivery a retirement whose write was owed dropped from memory
	// (retirePending), "" for none. Memory holds the claim retired from that task, so the repair
	// makes the retirement itself (Store.RetireDelivery, repairRetirement), which deletes the
	// delivery the row still holds, rather than a write of the claim and of the delivery memory
	// holds (Store.PutClaimAndDelivery), which would leave it there. A claim write of another kind
	// that lands makes the claim row whole but deletes nothing, so it settles the mark's reports
	// and leaves this (settleOwedWrite); only a retirement that lands, or a row that holds no
	// delivery at all, clears it, and a row that holds another one refuses it (repairRetirement).
	retire string
	// after is what the decisions whose writes this mark owes do outside the machine once those
	// writes are made (afterRecord): the stop of a process, a revocation, the stop of a
	// registration, a forgotten session's report. It holds them only while the window after a
	// refused claim write holds every write back (errWritesWait, D5 of dispatch://INFRA-4099's
	// plan): the write that makes the owed write acts on them once it lands (settleOwedWrite) or
	// fails owed with the store's own answer (recorded), and never on the window's own answer, so
	// no act runs before its record's write was tried. A refusal drops them with the mark, since
	// the claim is built again over a row that still records what they would have ended.
	after afterRecord
}

// owedReport is one report a decision would have made had its write landed, kept until the write
// lands. The reports are the daemon's own record of what a claim durably became: it stamps a
// woken pod's first turn on working, and ends the workflow's phase on ready and on failed
// (workflowRuntime.claimDurableState in internal/daemon). One skipped by a failed write is a fact
// no later report makes good, because the fact is keyed on the claim's generation, so it is kept
// here rather than left to whatever the claim reports next.
type owedReport struct {
	// state is the durable transition to report.
	state ClaimState
	// claim is the claim copy of the decision that owed it, as it stood when the report was owed.
	// The report keeps its own copy because applyTerminal in internal/daemon reads the issue, the
	// role and the failure from it and keys the workflow fact on its generation, where the
	// repair's write uses the claim as the machine holds it then.
	claim Claim
}

// owedWriteLine is the one message a write left owed is logged under, at Warn, both where the
// write failed and where a repair of it failed again, and at Info where the daemon's own stop cut
// the write short (oweWrite). It is pinned as a constant because an operator and a production
// check read it whole: a search for this line is the search for every claim whose row is behind
// the daemon's memory, and a message that varied by call site would answer that search with a
// part of the answer.
const owedWriteLine = "supervise: a claim write the database did not take"

// repairedWriteLine is the close of that story, at Info: the owed write was made, and the row is
// what memory holds again. It is pinned for the same reason — the pair of lines is what an
// operator counts to see an outage end — and it is the line a check reads to tell a daemon that
// caught up from one that is still behind.
const repairedWriteLine = "supervise: a claim write the database did not take was made"

// refusedWriteLine is the one message a claim write the rule refuses is logged under, at Error
// (refusedWrite): the store, the server or the driver judged the row memory holds, so the same
// write fails again for as long as memory holds it (SortClaimWriteFailure). Its attributes are
// the store call, the state the row last held (Machine.rowState), the state memory held at the
// refusal, the claim's generation, the SQLSTATE (empty for a refusal the store made in Go), the
// constraint the server names or the store's rule word (RowRefusedError.Rule), and the error; a
// refused repair adds what the dropped mark held. It is pinned as a constant for the reason
// owedWriteLine is: a search for it is the search for every refused claim write, and each one is
// a defect of the daemon or of its schema that a person has to read.
const refusedWriteLine = "supervise: a claim write the store refused"

// claimWriteAgainLine is the line of a store call of a decision that the claim-write bound cut
// short and that the decision makes again at once (Machine.write), at Warn: the database answered
// that call past defaultClaimWrite, which a healthy one never does, so an operator sees a slow
// database here before any write is owed. A write made again that lands owes nothing and is
// followed by no owed line; one that is cut again is owed, under owedWriteLine.
const claimWriteAgainLine = "supervise: a claim write the bound cut short; making it again in this decision"

// oweWrite marks the claim write owed and says so in the log: the row the store holds is behind
// what memory holds, and the claim's next decision is to make that write again (repair). A
// machine that already owes one keeps the single mark, since the repair writes the claim as it
// stands rather than one write per failure; method names the call that failed for the log line
// alone, and each failure's line carries its own.
//
// It is split out of claimWriteFailed for the callers that owe a write without putting the
// failure to the rule here. One is a caller that has already put it to the rule and keeps the
// owed failure its own way (repair, retirePending, markRound, ReleaseUncertainLaunch,
// suspendOverADeadProcess). The other is a retirement the store refused because it had already
// made it (retirePending), which knows from the refusal itself that the claim write of that same
// transaction was rolled back with the delete — a fact about the store's transaction rather than
// about the shape of the error. Asking the rule there would be asking the wrong question, and
// would answer by accident: the sentinel carries no SQLSTATE, so the rule owes it, but it would
// stop owing it the day the refusal is spelled as a server error.
//
// A write the daemon's own stop cut short is logged at Info instead. The stop cancels the context
// the machine was built with (supervision.halt in internal/daemon), and the context of each
// decision a route still runs when the API's drain ends, so such a write ended because the daemon
// is going, not because the database refused it or did not answer. That is the stop ending a
// decision by design, which the daemon logs at Info too (supervisor.decisionFailed); at Warn the
// line would send whoever reads it to a database that refused nothing. The write is owed all the
// same: a route's decision on the claim during the drain makes it at its head (repair), and
// otherwise the mark goes with the process and the next boot takes the claim up from its row, as
// it takes up every decision the stop ends. A write that ran out its own deadline while the daemon
// stopped waited on the database, and stays at Warn.
func (m *Machine) oweWrite(method string, err error) {
	if m.owedWrite == nil {
		m.owedWrite = &owedClaimWrite{}
	}
	if errors.Is(err, context.Canceled) && m.ctx.Err() != nil {
		m.log.Info(owedWriteLine, "store", method, "error", err, "owed", true)
		return
	}
	m.log.Warn(owedWriteLine, "store", method, "error", err, "owed", true)
}

// claimWriteFailed puts err, the failure of the claim write method, to the rule
// (sortWriteFailure), and returns err as it was: the decision still fails, and its event is still
// dropped. A failure the rule owes marks the claim write owed (oweWrite), so the write itself is
// not lost with the event: memory keeps its state, and the claim's next decision makes the write
// (repair). A failure the rule refuses marks nothing: it is logged and told where the rule reads
// it, the same write would be refused again, and the decision ends by building the machine again
// over the claim's row (settleRefusal). A nil err is no failure, and is returned nil.
//
// It is the rule's form for a write whose failure leaves memory ahead of the row and nothing else
// to say so: persist, persistStop, a confirmation (confirmCarrying) and a provider error's note
// (providerErrorClears), whose claim and delivery memory both changed first, and a project pause's
// stop that a retirement writes first (retirePending). A write whose owed failure keeps a mark of
// its own takes writeFailed instead.
func (m *Machine) claimWriteFailed(method string, err error) error {
	if err != nil && m.sortWriteFailure(method, err) == WriteOwed {
		m.oweWrite(method, err)
	}
	return err
}

// writeFailed puts err, the failure of the claim write method, to the rule (sortWriteFailure),
// and returns err as it was, for a write whose owed failure leaves no claim write owed: the
// failure the rule owes is the caller's to keep, and a refusal is logged and told here as every
// refusal is. A nil err is no failure, and is returned nil.
//
// Its callers are the writes of D8 of dispatch://INFRA-4099's plan whose owed failure keeps a mark
// of their own or leaves memory as the row holds it: a delivery written with memory's change made
// first keeps its unsaved mark (saved: putPending, rewritePending, settle, takeBackPending, the
// failed send's record), and queue and interrupted change memory only once the store has taken
// their write; a take-back a let-go obliges the next claim write to carry keeps that obligation
// (writeCarried, carryPending); and an admission (revive) and a release of names (ReleaseNames)
// write a claim to which nothing of their decision has been added yet, since each joins memory
// only once its transaction lands, so a write that failed owed leaves memory as the row holds it
// and nothing to make again.
func (m *Machine) writeFailed(method string, err error) error {
	if err != nil {
		m.sortWriteFailure(method, err)
	}
	return err
}

// sortWriteFailure is the rule applied to err, a failure of the claim write method: the kind it
// is (SortClaimWriteFailure), with a refusal logged and told once, here (refusedWrite). It is the
// one place a machine reads the rule, so every claim write's failure is logged and told the same
// way, whichever call made it and whatever its caller does with the kind.
func (m *Machine) sortWriteFailure(method string, err error) WriteFailure {
	kind := SortClaimWriteFailure(err)
	if kind == WriteRefused {
		m.refusedWrite(method, err)
	}
	return kind
}

// refusedWrite logs a claim write the rule refused under refusedWriteLine, at Error, tells the
// daemon once through Deps.ClaimWriteRefused (D10 of dispatch://INFRA-4099's plan), and records the
// refusal on the machine (Machine.refusal): no other claim write of the decision reaches the store,
// and the decision ends by building the machine again over the claim's row (settleRefusal, D2).
// The claim is memory's as it stands at the refusal, which is the claim the store refused; stored
// is the state the row last held (Machine.rowState). more is what the caller adds to the line: what
// a refused repair dropped with its mark (repair).
//
// A refusal already recorded is the answer every later claim write of the decision gives
// (Machine.write), so it is the same refusal sorted again by the caller of that write, and is
// neither logged nor told a second time.
func (m *Machine) refusedWrite(method string, err error, more ...any) {
	if m.refusal != nil {
		return
	}
	sqlstate, constraint := refusalOf(err)
	m.refusal = &claimRefusal{store: method, err: err, sqlstate: sqlstate, constraint: constraint,
		state: m.claim.State, generation: m.claim.Generation}
	attrs := append([]any{"store", method, "stored", m.rowState, "refused", m.claim.State,
		"generation", m.claim.Generation, "sqlstate", sqlstate, "constraint", constraint, "error", err}, more...)
	m.log.Error(refusedWriteLine, attrs...)
	if m.deps.ClaimWriteRefused != nil {
		m.deps.ClaimWriteRefused(WriteRefusal{Claim: copyClaim(m.claim), Store: method, Stored: m.rowState,
			Refused: m.claim.State, Generation: m.claim.Generation, SQLState: sqlstate, Constraint: constraint, Err: err})
	}
}

// oweDurableState keeps the report a decision owed when its own write was left owed, made once
// that write lands, by the repair or by whatever claim write lands first (settleOwedWrite,
// makeOwedReports). Every report is made after the write of the transition it reports, so a
// write that failed skips the report with it, and the daemon never learns a claim fact its
// memory already holds — the working report of a turn that started is the one the measured
// incident lost.
//
// A claim that owes no write owes no report either: the write the rule refused will be refused
// again, so no write of this claim is coming to make the report, and a report of a transition
// the row will never hold would tell the daemon more than the store can say.
func (m *Machine) oweDurableState(state ClaimState) {
	if m.owedWrite == nil {
		return
	}
	m.owedWrite.reports = append(m.owedWrite.reports, owedReport{state: state, claim: copyClaim(m.claim)})
}

// repair makes the claim write the machine owes, at the head of the decision, before anything
// reads or writes the claim again (Handle). It is the whole of what this mark is for: a write the
// database did not take is made at the claim's next decision instead of being lost with the event
// that failed.
//
// It writes the claim as the machine holds it now, never a copy taken when the write failed.
// Every state the machine holds came from the worker's own frame — a claim that reads working is
// a claim whose agent said so — and no decision that marks a write owed puts its own field back,
// so the claim standing now is the claim every failed write was trying to record, together with
// whatever the claim has decided since. A copy would write an older claim over a newer one.
//
// It writes once: the claim with its pending delivery where one is pending, the claim alone where
// none is. One call covers both rows, so a claim that owes a claim write and holds an unsaved
// delivery is made whole here, and the settle that follows this call finds nothing unsaved and
// writes nothing — where two writes would leave the row in between, and the second would have to
// be owed in its turn. A mark that holds a retirement is made by the retirement itself
// (repairRetirement), after the pause's stops the mark holds, since those are the transaction
// every claim write of a stopped claim goes through first.
//
// It goes through the machine's own write helpers (putClaim, putClaimAndPending), so the write
// it makes is the write every other decision makes, under the same deadline and normalized the
// same way. What follows a write that landed is theirs too — the mark dropped, the line that
// says the write was made, and the reports those failed decisions skipped (settleOwedWrite) —
// because a claim write that lands is the repair whoever made it, and this function would
// otherwise be a second place that knows what landing means. What is left here is the
// head-of-decision entry point and the handling of a repair that failed; method names the call
// for those failure lines alone, since the line of a write that landed is written where it
// landed.
//
// It returns nothing. A repair that fails must not drop the event behind it: the error would
// reach supervisor.decisionFailed in internal/daemon, which logs it and drops the event, and an
// event dropped for a write the database did not take is the fault this change exists to end. A
// repair that failed keeps the mark, logs it, and the decision goes on to its own row, which will
// be owed too if the database is still away.
//
// A failure the rule refuses drops the mark: the store, the server or the driver judged the row
// memory holds, and would refuse the same write again, so keeping the mark would only buy the same
// refusal at every decision from here on. The reports the mark held go with it, and the refusal's
// line carries how many of them there were (droppedReports) and how many of the pause's stops
// (droppedStops), because that line is the whole record of what was lost: R6 asks it to say that
// the write and the reports it held are dropped, and a line naming only the write would leave no
// trace that anything went with it — the states and the generations those reports carried are
// gone from the daemon the moment the mark is.
func (m *Machine) repair(ctx context.Context) {
	if m.owedWrite == nil {
		return
	}
	if m.owedWrite.retire == "" || len(m.owedWrite.stops) > 0 {
		if !m.repairWrite(ctx) {
			return
		}
	}
	if m.owedWrite != nil && m.owedWrite.retire != "" {
		m.repairRetirement(ctx)
	}
}

// repairWrite is repair's write of the claim, the claim with its pending delivery where one is
// pending, and answers whether it landed.
func (m *Machine) repairWrite(ctx context.Context) bool {
	method, pending := "PutClaim", m.claim.Pending
	if pending != nil {
		method = "PutClaimAndDelivery"
	}
	if len(m.owedWrite.stops) > 0 {
		method = "PauseStopped"
	}
	var err error
	if pending != nil {
		err = m.putClaimAndPending(ctx)
	} else {
		err = m.putClaim(ctx)
	}
	// A write that landed settled the mark inside the helper that made it, reports and all.
	if err == nil {
		return true
	}
	m.repairFailed(method, err)
	return false
}

// repairRetirement is repair's retirement (owedClaimWrite.retire): the claim as memory holds it
// now, written with the delete of the delivery that retirement dropped from memory, in the
// store's one transaction (Store.RetireDelivery), as retirePending would have written it.
//
// A row that holds no delivery at all has nothing left to delete: a retirement whose answer was
// lost committed it (ErrNoSuchDelivery). The claim write that went with the delete was rolled back
// with it, so the mark stays owed for that alone, and the claim is written now as every owed claim
// write is (repairWrite).
//
// A row that holds another delivery under the claim (ErrOtherDelivery) is a refusal of this very
// retirement: the same RetireDelivery meets the same answer for as long as memory holds what it
// holds, so it is sorted as the rule sorts a refusal — logged and told once, the mark dropped with
// what it held (dropOwedWrite) — rather than made again at every decision.
func (m *Machine) repairRetirement(ctx context.Context) {
	id, c := m.owedWrite.retire, m.stored()
	err := m.writeClaim(ctx, func(ctx context.Context) error { return m.deps.Store.RetireDelivery(ctx, c, id) })
	switch {
	case err == nil:
		m.rowState = c.State
		m.owedWrite.retire = ""
		m.settleOwedWrite(ctx, "RetireDelivery")
		m.recorded(ctx, nil)
	case errors.Is(err, ErrNoSuchDelivery):
		m.log.Warn("supervise: the owed retirement's delivery is no longer in the row; the claim write it carried is made alone",
			"delivery", id, "error", err)
		m.owedWrite.retire = ""
		m.repairWrite(ctx)
	case errors.Is(err, ErrOtherDelivery):
		m.dropOwedWrite("RetireDelivery", err)
	default:
		m.recorded(ctx, err)
		m.repairFailed("RetireDelivery", err)
	}
}

// repairFailed is a repair whose write failed, by the rule: an owed failure keeps the mark and
// logs it again, and a refusal drops the mark with what it held (dropOwedWrite).
func (m *Machine) repairFailed(method string, err error) {
	if SortClaimWriteFailure(err) == WriteOwed {
		m.oweWrite(method, err)
		return
	}
	m.dropOwedWrite(method, err)
}

// dropOwedWrite is a repair the store refused: the refusal logged and told once (refusedWrite),
// with what the dropped mark held on its line, and the mark dropped.
func (m *Machine) dropOwedWrite(method string, err error) {
	m.refusedWrite(method, err, "droppedReports", len(m.owedWrite.reports), "droppedStops", len(m.owedWrite.stops))
	m.owedWrite = nil
}

// settleOwedWrite settles the write the machine owes, now that a claim write has landed, and
// returns at once when nothing is owed. A claim write that lands while a write is owed is the
// repair, whichever decision made it: every claim write that reaches here writes the claim as
// the machine holds it — putClaim and putClaimAndPending from m.stored(), retirePending its
// own claim write, written in the same transaction as the delete, and an admission (revive) and
// a release of names (ReleaseNames) the claim memory holds — which is exactly what the repair
// would have written. The row is no longer behind memory, so there is nothing left for a later
// repair to do, and nothing of the order to get wrong. The one thing a claim write does not make
// is a retirement the mark holds (owedClaimWrite.retire): the delivery is still in the row, so
// the mark stays for that alone until the retirement lands, and its caller clears it first where
// it made the retirement.
//
// That order is why the owed reports are made here rather than left to the next repair. They are
// made before the landing decision's own report, which follows its own write, so the daemon is
// told what the claim durably became in the order the decisions made it: the working of a turn
// whose write failed, then the ready of the decision whose write settled it. Left to a later
// repair, the landing decision's own report would come first and the owed ones after it, which
// is the order nothing lived.
//
// It settles the claim write and nothing else. The pending delivery's own mark (unsaved) is left
// where it is, because a claim write that lands says nothing about the delivery row: a landing
// putClaim wrote the claim alone, and clearing the delivery's mark there would drop a delivery
// write that was never made. The writes that do cover both rows clear it themselves, where it is
// true of what they wrote (putClaimAndPending, retirePending).
//
// method is the store call that landed, for the line that closes the story (repairedWriteLine):
// an operator counts the pair of lines, and the one that ends it names the write that made good.
//
// What the decisions whose writes the mark owed do outside the machine, which the window after a
// refused write held on the mark (owedClaimWrite.after), is done here too, first, since their
// record is now written (act, D5 and D6 of dispatch://INFRA-4099's plan).
func (m *Machine) settleOwedWrite(ctx context.Context, method string) {
	if m.owedWrite == nil {
		return
	}
	// The reports and the acts are taken before the mark goes, since the mark is what holds them.
	reports, retire, after := m.owedWrite.reports, m.owedWrite.retire, m.owedWrite.after
	m.owedWrite = nil
	if retire != "" {
		m.owedWrite = &owedClaimWrite{retire: retire}
	}
	m.log.Info(repairedWriteLine, "store", method)
	m.act(ctx, after)
	m.makeOwedReports(reports)
}

// makeOwedReports makes the reports the failed decisions owed, oldest first, now that the write
// they waited on has landed. Each is handed to the daemon exactly as its own decision would have
// handed it (report), so what the daemon learns from a claim whose database was away is what it
// would have learnt had every write been taken, only later.
//
// One kind of owed report is dropped instead, because what it would tell the daemon is no longer
// true: a report of a generation the claim has left. The daemon keys the workflow fact on the
// claim's generation (applyTerminal in internal/daemon), so such a report names a fact of a run
// that is over, which no report of the generation now running stands in for and which nothing
// running wants to hear.
//
// No report made here can be one the claim has reported past, so none is dropped as superseded.
// Every report follows a claim write that landed, and a claim write that lands while a write is
// owed makes the owed reports first (settleOwedWrite): between the write that failed and the
// next one that lands, the claim reports nothing at all, and when one lands the reports it owed
// go before the report of the decision that made it. So the oldest owed report is still the
// oldest fact the daemon has not been told, and the whole of what is owed is told in the order
// the decisions made it.
func (m *Machine) makeOwedReports(reports []owedReport) {
	for _, r := range reports {
		if r.claim.Generation != m.claim.Generation {
			m.log.Info("supervise: an owed report is dropped; the claim has left the generation it was owed at",
				"state", r.state, "generation", r.claim.Generation)
			continue
		}
		m.report(r.claim, r.state)
	}
}

// WriteFailure is the kind of a failed claim write (SortClaimWriteFailure). The zero value is no
// failure: the kind of a nil error.
type WriteFailure int

const (
	// WriteOwed is a failure that can end while memory stays as it is: the database or the schema
	// failed, not the row memory holds. Memory keeps its state, and the write is made again at the
	// claim's next decision (repair).
	WriteOwed WriteFailure = iota + 1
	// WriteRefused is a failure the row memory holds caused: the server, the store or the driver
	// judged it, so the same write fails again for as long as memory holds it.
	WriteRefused
)

// String is the kind's word, for a log line and a test's message.
func (f WriteFailure) String() string {
	switch f {
	case WriteOwed:
		return "owed"
	case WriteRefused:
		return "refused"
	default:
		return "none"
	}
}

// RowRefusedError is a claim write the store refused itself, in Go, before it sent a statement:
// a value that fits no column of its type, a value whose JSON does not encode, and a pause's stop
// whose records the store does not take (D8 of dispatch://INFRA-4099's plan). The store judged the
// row memory holds, as a server's CHECK does, so the rule refuses it (SortClaimWriteFailure);
// without the type it would carry no SQLSTATE, the rule would owe it, and the repair would make
// the same refused write at every decision of the claim. Rule is a short stable word naming the
// check (RuleBigintFit, RuleInt4Fit, RuleJSONEncode, RulePauseStop), which stands where a server's
// refusal names its constraint (refusalOf). Err is the store's own error, which this one wraps.
type RowRefusedError struct {
	Rule string
	Err  error
}

// Error is the store's own error's text.
func (e *RowRefusedError) Error() string {
	if e.Err == nil {
		return "the store refused the row (" + e.Rule + ")"
	}
	return e.Err.Error()
}

// Unwrap is the store's own error.
func (e *RowRefusedError) Unwrap() error { return e.Err }

// The rule words of the store's own refusals (RowRefusedError.Rule).
const (
	// RuleBigintFit is a uint64 past the bigint column that holds it.
	RuleBigintFit = "bigint-fit"
	// RuleInt4Fit is an int past the integer column that holds it, which the store checks before
	// the statement so the driver's own refusal of the value (pgx's pgtype) is never reached.
	RuleInt4Fit = "int4-fit"
	// RuleJSONEncode is a value of a json or jsonb column that encoding/json does not encode.
	RuleJSONEncode = "json-encode"
	// RulePauseStop is a project pause's stop whose records the store does not take
	// (Deps.PauseStopped): a stop that names no claim, a backstop of another issue or role, a
	// lock of no issue key, a pause no row holds.
	RulePauseStop = "pause-stop"
)

// errWritesWait is the answer of a claim write the machine does not send to the store while the
// writes of a refused claim wait out the probe interval after the read of its row (withheld,
// Machine.writesWaitUntil, D5 of dispatch://INFRA-4099's plan). The row memory holds was not
// judged, so the rule owes it, and the repair makes the write at the first decision after the
// window. It is no write that landed or that the store failed: nothing the owed mark holds for a
// write is settled by it.
var errWritesWait = errors.New("supervise: the claim's writes wait out the probe interval after a refused write")

// WriteRefusal is one claim write the rule refused, as the machine tells it (Deps.ClaimWriteRefused,
// D10 of dispatch://INFRA-4099's plan): the fields of refusedWriteLine, and the claim the store
// refused.
type WriteRefusal struct {
	// Claim is the claim as memory held it at the refusal: the claim the write tried to record.
	Claim Claim
	// Store is the store call that was refused (PutClaim, PutDelivery, PutClaimAndDelivery,
	// RetireDelivery, AdmitClaim, ReleaseHolds, PauseStopped).
	Store string
	// Stored is the state the claim's row last held (Machine.rowState).
	Stored ClaimState
	// Refused is the state memory held at the refusal.
	Refused ClaimState
	// Generation is the claim's generation in memory at the refusal.
	Generation uint64
	// SQLState is the server's code, and empty for a refusal the store made in Go.
	SQLState string
	// Constraint is the constraint the server names, or the store's rule word
	// (RowRefusedError.Rule) for a refusal it made in Go.
	Constraint string
	// Err is the refusal.
	Err error
}

// SortClaimWriteFailure is the one rule that sorts the failure of a claim write (R1 of
// dispatch://INFRA-4099's spec): owed, when the failure can end while memory stays as it is, or
// refused, when the row memory holds is what failed, so the same write would fail again for as
// long as memory holds it. A claim write is a store call of a machine that writes the claim's row
// or its pending delivery: PutClaim, PutDelivery, PutClaimAndDelivery, RetireDelivery, AdmitClaim,
// ReleaseHolds and the pause's stop (Deps.PauseStopped). A nil error is no failure, and sorts as
// the zero WriteFailure, which is neither kind; every caller sorts an error it has, and
// sortWriteFailure is reached only with one.
//
// This rule decides what is owed once Machine.write has stopped trying, not whether it tries again
// first: write makes a deadline cut of a write the database was handed once more in the same
// decision (cutByBound, claimWriteTries), and only a write whose last try still failed comes here.
// A deadline cut of a write the database was never handed (a dial, a queue's wait, a BEGIN) is
// owed at once, as the deadline row of TestTheOwedWriteErrorsAreTheOnesNoServerAnsweredAbout reads.
//
// The rule reads the error in steps, and their order is the substance of it rather than a style
// of writing.
//
// The wrapper comes first: an error that unwraps to a *pgconn.ConnectError is owed whatever it
// carries. The connection never opened, so no statement of this write ever reached a server, and
// nothing in such an error can be a judgement of it. That covers the dial that was refused, which
// carries no SQLSTATE at all, and equally a connection PostgreSQL itself refused, which pgx
// answers as the server's own *pgconn.PgError inside that wrapper (newPerDialConnectError in
// pgconn/pgconn.go, whose Unwrap keeps the chain open) and which can carry any SQLSTATE
// whatever — a restart's 57P03, and a rotated password's 28P01, whose class the SQLSTATE step
// would refuse.
//
// The store's own refusal comes next: an error that unwraps to a *RowRefusedError is refused.
// The store judged the row in Go before it sent a statement, so the error carries no SQLSTATE,
// and the steps below would owe it. Then errWritesWait, a write the machine held back itself, is
// owed: nothing judged that row.
//
// The SQLSTATE comes last, and only then is it the server's answer about a statement it was
// given: an error that unwraps to a *pgconn.PgError is owed when that code's class, its first two
// characters, is one of the six a server answers with about something other than the row:
//
//   - 08, connection exception: the connection itself is what failed, and the statement went down
//     with it.
//   - 40, transaction rollback: the server rolled its own transaction back — a serialization
//     failure, a deadlock it broke — so the statement was abandoned rather than judged.
//   - 53, insufficient resources: the memory, the disk or the connection slots the server had not
//     got for this statement, and will have again.
//   - 55, object not in prerequisite state: an object another session holds, a lock not available
//     chief among them, rather than a statement the server read and refused.
//   - 57, operator intervention: a backend terminated, a query cancelled, a database shutting down
//     or starting up — the platform's doing or an operator's, not the statement's.
//   - 58, system error: a file or an input/output error of the server's own, which says nothing
//     about what was asked of it.
//
// Three codes of other classes are owed too, because each stops when the database or the schema
// comes back, whatever the row holds:
//
//   - 25006, read_only_sql_transaction: a server a failover left read-only, or a replica the pool
//     reached, refuses every write until a writable primary answers again.
//   - 42703, undefined_column, and 42P01, undefined_table: a binary that runs ahead of its
//     migration names a column or a table the schema has not got yet, and the same write is
//     taken once the migration lands.
//
// Every other SQLSTATE is the server judging the row memory holds — a CHECK it breaks (23514), a
// unique index another row holds (23505), a foreign key (23503) — and is refused. A code shorter
// than two characters has no class to read and judges nothing, so it is owed with the rest rather
// than taken for a decision about the row. An error that unwraps to none of these types is owed
// too: the network cut, and a write the claim-write deadline cut short before any answer (R5).
//
// Read the other way round — the SQLSTATE first, the wrapper after — the rule refuses every
// connection the server itself refused, because such an error does carry a PgError and its class
// is whatever the refusal was: a rotated password's 28P01 would read as a statement judged, and
// the write of a claim fact only memory holds would be dropped for a statement no server ever
// read. That is this issue's own fault, reached through the rule written to end it. The outbox
// reads the wrapper first for the same reason when it tells a statement Postgres refused from a
// connection it refused (refusedStatement in packages/envoy's
// internal/dispatch/outbox/lookups.go); it is another module, so the two are kept in step by
// hand rather than one imported by the other.
//
// The two no longer sort the codes the same way. refusedStatement takes 25006, 42703 and 42P01
// for statements Postgres refused, as every code outside its server classes (08, 40, 53, 57, 58),
// because what it asks is whether a run of its statement over fewer ids may not meet the
// failure, so that splitting the batch keeps the ids that can still be read; a failure that fails
// every run alike is the other answer, and what it does with each answer is its own. This rule
// asks whether the row memory holds is what failed, because a refused claim write is put back to
// the row the store holds (R2) and an owed one is made again: a read-only server, a missing
// column and a missing table fail whatever the row holds, and end when the database or the
// migration does, so here they are owed. It owes class 55 as well, which refusedStatement does not
// name.
//
// The identities below are what pgx v5.11.0 returns, measured in this pod on 2026-11-07, and they
// are the evidence the connection steps of this rule rest on; the server's own refusals of a
// claim write, 25006, 42703 and 42P01 among them, are measured against the pod's server by
// TestTheServersRefusalsOfAClaimWriteAreSortedByTheRule in internal/store:
//
// |what was done|error|Go type|SQLSTATE|pgconn.SafeToRetry|
// |---|---|---|---|---|
// |a pool at 127.0.0.1:1, then Exec|failed to connect to `user=… database=…`: … dial error: … connection refused|*pgconn.ConnectError|none|false|
// |Exec under a one second context against a listener that accepts and never answers|context deadline exceeded|context.deadlineExceededError|none|false|
// |pg_terminate_backend of the pool's one backend, then Exec|FATAL: terminating connection due to administrator command (SQLSTATE 57P01)|*pgconn.PgError|57P01|false|
// |a listener answering the startup message with an ErrorResponse, as a restarting managed instance answers a new connection|failed to connect to `user=… database=…`: … server error: FATAL: the database system is starting up (SQLSTATE 57P03)|*pgconn.PgError inside a *pgconn.ConnectError|57P03|false|
// |the same listener answering the startup message with the refusal a rotated password draws|failed to connect to `user=… database=…`: … server error: FATAL: password authentication failed for user "…" (SQLSTATE 28P01)|*pgconn.PgError inside a *pgconn.ConnectError|28P01|false|
//
// The last two rows are the ones the order is for. Both are owed by the first step, because
// neither connection opened. Read the SQLSTATE first and the 57P03 row is owed only by the
// accident of its class being one of the six, while the 28P01 row is refused outright: the
// write is dropped, and the claim fact only memory holds is lost, for a statement no server
// ever read.
//
// The rows are also why the rule cannot be a list of error types. The first is a pgx type, the
// second one the standard library returns through a path that reaches no pgx type at all, the
// third is neither, and the last two are both at once; what the owed ones share is not a type but
// the absence of a judgement of the row, which no list can be kept in step with. They are also
// why the rule cannot be pgconn.SafeToRetry: it is false for all five — it reports whether pgx
// knows the statement never reached the server, not whether the server decided against it — so a
// rule built on it would drop the dial failure and the cut-short write, the two that are most
// plainly owed.
func SortClaimWriteFailure(err error) WriteFailure {
	if err == nil {
		return 0
	}
	// The wrapper first: a connection that never opened judged no statement, whatever the server
	// said in refusing the connection itself.
	var connErr *pgconn.ConnectError
	if errors.As(err, &connErr) {
		return WriteOwed
	}
	var rowErr *RowRefusedError
	if errors.As(err, &rowErr) {
		return WriteRefused
	}
	if errors.Is(err, errWritesWait) {
		return WriteOwed
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || len(pgErr.Code) < 2 {
		return WriteOwed
	}
	switch pgErr.Code[:2] {
	case "08", "40", "53", "55", "57", "58":
		return WriteOwed
	}
	switch pgErr.Code {
	case "25006", "42703", "42P01":
		return WriteOwed
	}
	return WriteRefused
}

// refusalOf is the SQLSTATE and the constraint of a refused claim write, for its line and its
// hook (refusedWrite): no code and the store's rule word for a refusal the store made in Go
// (RowRefusedError), and the server's code and the constraint it names for one the server made.
// The store's type is read first, in the order the rule reads it (SortClaimWriteFailure).
func refusalOf(err error) (sqlstate, constraint string) {
	var rowErr *RowRefusedError
	if errors.As(err, &rowErr) {
		return "", rowErr.Rule
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code, pgErr.ConstraintName
	}
	return "", ""
}
