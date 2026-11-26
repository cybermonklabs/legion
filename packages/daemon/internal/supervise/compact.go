package supervise

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A claim whose session a provider keeps refusing is compacted rather than started again: the
// session, its workspace and everything it knows stay, and what goes is the length of the
// transcript the provider is being asked to read (dispatch://INFRA-956 R3). `legion claims
// compact` and the controller's own compact both run it (api's compact), and this file is the
// machine's half: taking the claim, asking Oh My Pi for a handoff (runtime.Conn's Compact), and
// giving the claim back with the value of its owed task written again, for the worker to take at
// its next idle point, unless the worker's take of it was accepted before the compaction, whose
// own turn then runs it. Oh My Pi's `compact` is never asked for: Anthropic refuses its summary
// request for a long Claude session (dispatch://INFRA-5798).

// ErrCompactInstructions is a compaction asked for with nothing to write the handoff document
// to. The instructions are what keep the reviewer's entry in the document (api's
// compactInstructions), and a document that dropped it leaves the reviewer no entry to write, so
// a compaction with none is refused before anything is sent.
var ErrCompactInstructions = errors.New("a compaction carries custom instructions, which keep the reviewer's entry in the handoff document")

// ErrCompactNotTaken is a compaction nothing was asked for: the claim could not be written as
// working, it lost its process between the take and the ask, its shim is not connected, a
// launch the wake pacing holds waits its turn to replace the process its session runs on, or the
// claim's session is not held to it (holdRecorded), so the daemon had nothing to ask through,
// nothing worth asking or nothing it may ask (compactStart). The claim is where the request found
// it and no task's value was written, where every other outcome gives the claim back and writes
// the value of the task it owes again, which the worker takes at its next idle point
// (compactEnded). A claim a suspension is held over is not one of these: it is refused in
// its state, as every other action refuses such a claim (RefusedError, compactStart).
var ErrCompactNotTaken = errors.New("the claim was not taken for a compaction, so nothing was asked of its session")

// ErrCompactNotEnded is a compaction whose end did not finish: Oh My Pi answered and a step of
// the hand-back failed, or the daemon stopped before Oh My Pi answered, so the task the claim owes
// is not offered (offerPending). It is the daemon's own failure and not a compaction that did not
// run, whose answer promises the claim is back and its task offered again (api's compact).
//
// Three shapes reach it, and the claim is in a different place in each. The write that gives the
// claim back failed, and the claim is left working, which is where the compaction took it. Or
// that write landed and what follows it failed — the stale-plugin relaunch held for this end, or
// the offer of the owed task itself (afterTurn) — and the claim is back with its agent, as a claim
// whose turn ended is, with the task still to go. Or the daemon stopped while Oh My Pi ran the
// handoff, and the claim is left stored working on purpose, for the next start to read as a take
// a handoff may still run over (Machine.Compact, NewMachine). The error says the hand-back did not
// finish, which is true of each, rather than naming one of them.
//
// What a daemon that stops here leaves behind is a shape it already knows: a claim stored
// working, whose agent's own answer to "are you streaming" ends it (askTurnOver), or a ready
// or idle claim holding a task, whose value the next thing that reaches that claim writes again —
// its agent's hello, its ready, the end of a turn it takes of its own (offerPending's callers).
var ErrCompactNotEnded = errors.New("the compaction ended and the claim could not be given back, so the task it owes was not offered")

// ErrCompactProcessGone is a compaction whose end found it was no longer the claim's own
// (ownsCompaction): the process it ran on was found gone while Oh My Pi wrote the summary
// (judge's gone verdict, compactionLostProcess), it was let go for a suspension or a failure
// (letGo), or a relaunch has moved the claim past the generation the compaction was taken at.
// So compactEnded never ran: the claim was not given back and the task it owes was not offered.
//
// What happens to the claim next is its own path's, and that path is not always a relaunch. A
// claim whose death the daemon relaunches has its new process take the owed task once its agent
// is ready again (CurrentTask, claimHold). An agent whose death failed its claim runs again only
// on a retry, the workflow's or one the operator or the controller asks (retry, askedRetry), and
// an agent a held suspension stopped runs again only on a resume that may lift it: the operator's
// lifts every one, and the controller's every one but the operator's own (Claim.HeldAgainst). The
// task waits for that.
var ErrCompactProcessGone = errors.New("the claim lost its process while its session was being compacted, so it was not given back and the task it owes was not offered")

// ErrCompactSuspended is a compaction whose end found a suspension held over the claim: one
// that arrived while Oh My Pi wrote the summary, which the machine holds as it holds a
// suspension that arrives in a turn (holdSuspension). The compaction ran and its end was the
// claim's own, so this is neither ErrCompactProcessGone nor a store that would not write
// (ErrCompactNotEnded); what it is is a claim the end did not give back. The hold runs there
// and the claim is suspended; or the claim is working for a turn Oh My Pi started under the
// compaction and the hold runs at that turn's end instead; or the claim is idle over a take whose
// turn ended before the compaction with its report still to come, and the hold runs at that
// report's finish or at its deadline (compactEnded). Each way the task the claim owes was not
// offered.
//
// What becomes of that task is the hold's own, and settle is what decides it. A suspension by
// a source whose resume restarts this same agent in the same phase — one that holds, and a
// release — keeps the task through the suspension, and it goes when the agent runs
// again (SuspendSource.RetiresPhaseTask, settle). The daemon's own suspension ends the
// claim's phase, so settle retires the task queued for that phase with it and nothing writes its
// value again; the claim's next phase brings a task of its own. A hold of the daemon's that a
// start drops before it runs is neither: the claim runs on, and the turn this answer left it
// working for ends as any other turn does, with the task's value written again for the worker to
// take at its next idle point (StartedBy, dropHeld, turnEnded).
//
// It is the end's answer rather than a failure of it, as ErrSuspendHeld is the answer of a
// suspension the machine holds: nothing of the claim is left half written, and the caller is
// told what the claim is rather than that the compaction worked (api's compact).
var ErrCompactSuspended = errors.New("a suspension held over the claim ended its compaction, so the claim was not given back and the task it owes was not offered")

// compacting is the compaction a claim is in: the connection it runs over and the generation it
// was taken at. Those two, and the claim recording a compaction at all, are what the end of a
// compaction is fenced on (ownsCompaction). It is memory only, as a held suspension is: a
// daemon that restarts mid-compaction leaves a claim stored working, which its agent's own
// answer to "are you streaming" then ends (askTurnOver). An untaken owed task is offered on that
// end; a take accepted before the compaction keeps waiting for its own turn, which the restart
// protects until its start plus runtime.CompactTimeout (handoffGuard, NewMachine).
type compacting struct {
	conn       runtime.Conn
	generation uint64
	// back is the state the claim is given back in: the state the take found it in
	// (compactStart), so a claim that was ready is ready again rather than idle, which would
	// say a turn of it had ended. A turn that starts or ends under the compaction makes it
	// idle, since the claim has had a turn by then (turnStartedWhileBusy, turnEnded).
	back ClaimState
	// inTurn is whether a turn Oh My Pi started under the compaction is still running: an
	// Envoy follow-up it queued, or the turn of a take accepted before the handoff, whose text
	// Oh My Pi parks until the handoff ends; either starts as the compaction ends. The claim is
	// left working for that turn and its end gives the claim back (compactEnded, turnEnded).
	inTurn bool
}

// Compact compacts the claim's Oh My Pi session and writes the value of the task it owes again,
// for the worker to take at its next idle point. It is no event of its own, as
// ReleaseUncertainLaunch and PodUpdateTold are none: the compaction is a model
// call over the whole transcript, up to runtime.CompactTimeout, and a decision that waited it out
// would hold the machine's lock for ten minutes — every other request on the claim, a
// fresh_session's read of its state among them, would wait rather than be answered. So it is
// three steps: the claim is taken under the lock (RequestCompact), Oh My Pi is asked with no lock
// held, and the claim is given back under the lock (CompactEnded).
//
// While it runs the claim is working, which is the daemon's one record of a busy agent: the
// ladder's re-send waits, a newer task waits, a second compaction and a fresh session are
// refused, and the idle release does not take the pod — each because the site that decides it
// already asks whether the claim is working, and none of them has learned a second word for busy
// (compactStart).
//
// Nil is a session shortened and the claim given back. A handoff Oh My Pi refused — one asked
// for in a turn, a session with nothing to hand off, a model call that failed — carries its
// words, and one it answered with no document the daemon's (runtime.RefusalReason); a wait that
// ran out, or a transport that failed, is an error of its own. The claim is given back in every
// one of those cases, and the value of the task it owes is written again with it — or waits for
// the turn of a launch the pacing parked over its process, where every take of such a claim waits
// (claimHold), or is left to the take accepted before the compaction, whose own turn runs it —
// and only a compaction that ran puts the claim's run of provider-error turns back to zero
// (compactEnded). A take that did not happen at all is ErrCompactNotTaken or a *RefusedError, and
// leaves the claim where it was with its task unsent.
//
// A compaction that ran and did not end is none of those, and the end's own answer is what
// tells them apart: ErrCompactNotEnded is the end the store would not write,
// ErrCompactProcessGone the end the machine judged no longer the claim's own — the process it
// ran over found gone under it, or the claim moved past the generation it was taken at
// (ownsCompaction) — and ErrCompactSuspended the end that found a suspension held over the
// claim and gave it back to that hold rather than to its agent. That judgement is made under
// the machine's lock, where the claim cannot move under it, and it is the whole of what is
// read here: nothing of the claim is read from this side of the lock, where a relaunch the end
// itself raises would be taken for the process the compaction lost (afterTurn's
// refreshPlugin). Each leaves the owed task unsent, and each says so in the daemon's own
// words: what Oh My Pi answered such a compaction goes to the log rather than to the caller,
// which no model's words reach through (api's reasonBound).
func (m *Machine) Compact(ctx context.Context, customInstructions string) error {
	if strings.TrimSpace(customInstructions) == "" {
		return ErrCompactInstructions
	}
	token := m.View().Token
	var refused *RefusedError
	switch err := m.Handle(ctx, RequestCompact{Claim: token}); {
	case errors.As(err, &refused):
		// The claim is in no state to be compacted, and the refusal names that state: its
		// caller reads which refusal it is from there (api's refuseState).
		return err
	case err != nil:
		return errors.Join(ErrCompactNotTaken, err)
	}
	run, taken := m.compaction()
	if !taken {
		// The claim lost its process between the take and this read (letGo), which leaves it
		// suspended, failed or relaunching rather than working: its own path decides what
		// happens to it next, and there is no compaction to end.
		return fmt.Errorf("%w: %s lost its process before its session could be compacted", ErrCompactNotTaken, token)
	}
	err := run.conn.Compact(ctx, customInstructions, runtime.CompactTimeout)
	if err != nil && !handoffAnswered(err) && m.ctx.Err() != nil {
		// The daemon is stopping, and Oh My Pi has not answered the handoff: the stop's halt ended
		// every machine's own context before its drain ended this route's decision and the worker
		// stream (api's decision). The handoff may still run in the session, and the loss of its
		// answer releases no protection of the take it may run over (handoffGuard), so the claim
		// is left as the compaction's start stored it, working over whatever take it holds. The
		// next daemon start reads that record as a take a handoff may still run over, protects it
		// until its start plus runtime.CompactTimeout and asks the agent whether it is in a turn
		// (NewMachine); a task owed untaken is offered once that answer ends the stored turn.
		// Given back here, as #5567's daemon gave it back at a stop, the claim would be stored
		// ready or idle over a take whose turn the handoff holds, which the next start bounds by
		// the ordinary reconnect deadline and second wait alone.
		m.log.Warn("supervise: the daemon stopped while Oh My Pi ran the handoff; the claim stays stored working for the next start",
			"generation", run.generation, "error", err)
		return fmt.Errorf("%w: the daemon stopped while Oh My Pi ran the handoff, so the claim is left working for the next start: %w", ErrCompactNotEnded, err)
	}
	// The end runs whatever the compaction did and whatever became of the caller — a caller that
	// hung up still leaves a claim to give back — so it is posted on a context the caller cannot
	// cancel.
	ended := CompactEnded{Claim: token, Generation: run.generation, Compacted: err == nil, Answered: handoffAnswered(err)}
	endErr := m.Handle(context.WithoutCancel(ctx), ended)
	if endErr == nil {
		// compactEnded ran: the claim is back and the task it owes has gone again, so what this
		// answers is the compaction itself.
		return err
	}
	// The end gave the claim back to nothing, whichever end it was, so what the caller is told
	// is what became of the claim; what Oh My Pi made of the compaction itself goes to the log
	// (api's reasonBound).
	if err != nil {
		m.log.Warn("supervise: the compaction whose claim the end did not give back was itself refused or unanswered",
			"generation", run.generation, "error", err)
	}
	if errors.Is(endErr, ErrCompactSuspended) {
		// The end ran and a suspension held over the claim took it: the claim is suspended, or
		// working for a turn whose end suspends it, and nothing was sent (compactEnded).
		return ErrCompactSuspended
	}
	if errors.Is(endErr, ErrCompactProcessGone) {
		// The machine answered, under its lock, that the compaction is no longer the claim's own
		// (ownsCompaction): compactEnded did not run and sent nothing.
		return ErrCompactProcessGone
	}
	return fmt.Errorf("%w: %w", ErrCompactNotEnded, endErr)
}

// compaction is the compaction in flight, and false when none is.
func (m *Machine) compaction() (compacting, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.compacting == nil {
		return compacting{}, false
	}
	return *m.compacting, true
}

// ownsCompaction is whether the compaction taken at generation is still the claim's own, which
// is the one thing the end of a compaction turns on and the only place it can be read: the
// machine's lock is what makes the two facts one answer, and Machine.Compact holds no lock
// while Oh My Pi writes the summary. It is asked where the end is fenced (Machine.fence), so
// every caller of it reads the claim as the end will find it.
//
// A compaction is the claim's own when the claim still records one, since a process let go and
// a process found gone each drop it (letGo, compactionLostProcess), and when it is the one this
// end names, since launchTries' bump leaves an earlier generation's behind.
//
// A process still running that a parked launch is to replace is no such loss: the compaction
// ran over a live session, so the claim records it to the end, that end gives the claim back
// (compactEnded), and the task it owes waits for the launch's turn where every take of such a
// claim waits (claimHold's launchWaits).
func (m *Machine) ownsCompaction(generation uint64) bool {
	return m.compacting != nil && m.compacting.generation == generation
}

// compactionLostProcess drops the compaction in flight, where one is, because the process it
// runs over is gone: its end then finds no compaction recorded, gives the claim back nothing
// and sends nothing (ownsCompaction, ErrCompactProcessGone). It is called once, where the
// runtime's verdict that the process is gone is read (judge), so it covers both judgements that
// verdict splits into — a death and a displacement (died, displaced) — and neither of the two
// that replace a process still running (repoint, refreshPlugin), whose compaction keeps the
// session it is compacting. A death the machine acts on at once drops the compaction with the
// process anyway (letGo); this is the drop a death whose relaunch the pacing parks needs, since
// the park writes nothing of the claim at all (reserveWake).
//
// The compaction is dropped rather than marked as having lost its process, so nothing of it is
// left for the rest of the daemon to read: a claim the park leaves working over the dead process
// would otherwise report itself being compacted until the wake's turn, and a second compaction
// asked for in that window would be refused as one already running, after Machine.Compact had
// already answered the first caller that its compaction was over (api's refuseState).
//
// The one reader it is kept for is the record of what the process's end cut off (recordEnd),
// which runs after the drop: in the death or the displacement that follows it, and again at the
// let-go, which a parked relaunch makes at the wake's turn. The compaction put the claim in
// working with no turn running (compactStart), and with nothing left to say so that record would
// read the working as a turn cut off and tell the next process its turn was interrupted. So the
// dropped compaction stays on the machine for that record alone until the let-go clears it
// (Machine.lostCompaction, endState), and a second gone verdict over the same dead process,
// which finds no compaction in flight, leaves it as it is.
//
// What the drop gives up is the compaction's one reader that is not its end or that record: a
// late agent_end from the dead connection is no longer recorded on the compaction
// (turnStartedWhileBusy, turnEnded) but read as the end of the claim's own turn, which writes the
// turn's end line and idles the claim. Nothing is taken on it, since every take of a claim whose
// launch waits its turn waits with it (claimHold's launchWaits), and the wake's turn relaunches
// an idle claim exactly as it relaunches a working one. The record then reads the idle claim as
// it stands.
func (m *Machine) compactionLostProcess() {
	if m.compacting != nil {
		m.lostCompaction, m.compacting = m.compacting, nil
	}
}

// compactStart takes the claim for a compaction: it is working until the compaction ends
// (compactEnded), and that is the whole of how the rest of the daemon knows not to disturb it.
// Working is the state every one of those readers already asks about — a take of a claim that is
// not ready or idle is refused, so the ladder's re-take and any newer task wait; the idle rule
// holds the pod (workflow's idleStateReleasable); a fresh session is refused in a turn (api's
// freshSession); and this row is an ignore in working, so a second compaction is refused — and
// the claim is in no turn, so nothing is lost by saying the agent is busy in the one word the
// daemon already has for it. The one reader that must not take that word for a turn is the
// record of what a let-go of the process cut off, which reads the compaction instead (endState).
//
// It is written, because the idle rule and the daemon's sweeps read the claim from the store
// rather than from the machine. A claim left working by a daemon that stopped mid-compaction is
// the shape a restart already knows: it asks the agent whether the turn is still running and
// ends it when the agent says it is not (askTurnOver), and a take from it is the worker's word
// that the compaction is over (takeInTurn). A stop before Oh My Pi answers the handoff leaves
// that record as it is (Machine.Compact): the next start reads it, over a take accepted whose
// turn has not started, as the handoff the take may still be parked behind, and protects it
// (NewMachine, handoffGuard).
//
// A take accepted whose turn has not started is kept as it is: its number, its text, the
// handshake's confirmation of its entry and what it told stay, and nothing offers the task again
// (departure 10 of dispatch://INFRA-2811's plan). The compaction is Oh My Pi's handoff
// (stream's Conn.Compact), which aborts no turn: the pinned Oh My Pi refuses a handoff while a
// turn runs, and parks a prompt that reaches it while a handoff runs, starting that prompt's turn
// only once the handoff is over (waitForMaintenanceCleanup in
// packages/coding-agent/src/session/session-maintenance.ts at the pinned tag). So the text the
// worker hands Oh My Pi starts the take's own turn, before the compaction's end reaches the
// machine or after it, and the report of that turn finishes the task once. A start under the
// compaction is the take's as any start of a ready or idle claim is (turnStartedWhileBusy,
// turnStarted). Taken back, as the daemon did around Oh My Pi's `compact`, which could abort the
// turn, the take's turn report would name a task no take held, and the task would run again.
//
// Such a take is protected until the handoff's own deadline, runtime.CompactTimeout from the ask
// (protectTake, handoffGuard): the handoff may run for that long, and Oh My Pi answers no ask
// whether it is in a turn until the handoff is over, since both wait on its one serial queue. A
// second handoff is refused while a protected take's deadline stands with no handoff answer
// (protectedRecovery).
//
// A claim whose launch waits its turn in the wake pacing is taken for nothing either
// (launchWaits). What becomes of the process its session runs on is already decided — the turn
// lets it go and starts the next (wakeNow, launchNow) — and such a process is asked nothing
// anywhere else: no take of it is accepted (claimHold), no observation of it is judged and no
// probe is made of it (judge, probe). A compaction is the longest ask of them all, up to ten
// minutes of a model reading the transcript, over a connection that turn is about to close. It
// is answered as the shim that is not connected above is, a take that did not happen
// (ErrCompactNotTaken): the claim is left exactly where the request found it, nothing is asked
// and nothing is offered, and its caller answers 409 COMPACTION_FAILED carrying the sentence this
// returns (api's compact).
//
// A claim a suspension is held over is taken for nothing either, whatever state the hold found
// it in. The take reaches a ready or idle claim, and either can carry a hold: a stop the
// runtime refused at the agent's ready leaves the claim ready with the hold intact, and one it
// refused at a turn's end leaves it idle with the hold intact, each retried at the probe
// interval (ready, turnEnded, suspendHeld). What such a claim waits for is the hold's end, and
// that end stops its process more often than not: the compaction would spend ten minutes of a
// model on a session the hold is about to take, and its own end would hand the claim to that
// hold rather than back to its agent (ErrCompactSuspended). Unlike the two above, this is a
// refusal of the claim in the state the hold found it, as every other action refuses a held
// claim (RefusedError, holdSuspension, stop): the one door that answers a hold then answers
// both recoveries, in one sentence, rather than this guard writing a second copy of it
// (api's refuseState, refuseHeldSuspension).
func compactStart(m *Machine, ctx context.Context, _ Event) error {
	conn, ok := m.deps.Conns.Conn(m.claim.Token)
	if !ok {
		// No refusal of the claim's state: the state takes a compaction, and what is missing is
		// the connection to ask through. Machine.Compact answers it as a take that did not
		// happen, so its caller reports the one failure it is rather than reading the claim's
		// state for a cause that state does not hold (api's compact).
		return errors.New("the claim's shim is not connected, so nothing can be asked of its session")
	}
	if m.launchWaits() {
		// No refusal of the claim's state either: a launch the pacing holds is about to let this
		// process go, so there is nothing of its session worth asking.
		return errors.New("a launch waits its turn to replace the claim's process, so nothing is asked of its session")
	}
	if m.held != nil {
		// The state takes a compaction, and what stops this one is the suspension waiting over
		// the claim, so the refusal carries that state for the door to answer the hold from.
		return &RefusedError{State: m.claim.State, Request: "compact",
			Reason: "a suspension is held over the claim, so nothing is asked of a session that hold may stop"}
	}
	// Nothing is asked of a session the claim does not hold (holdRecorded, plan E's t-e-54): a
	// compaction runs Oh My Pi's summarizer over the session and can start a turn, which would put
	// a process the boot could not suspend to work under another claim's session, or one no claim
	// holds, as a task it took would (takeable; review round 13 of #4220). No refusal of the claim's
	// state either: the state takes a compaction, and what stops it is the session, so
	// Machine.Compact answers it as a take that did not happen, carrying the hold's own refusal,
	// or the store's error when the store did not answer.
	if err := m.holdRecorded(ctx, m.claim.Session, m.claim.SessionFile); err != nil {
		if errors.Is(err, errHoldUnanswered) {
			return fmt.Errorf("the store did not answer whether the claim holds its session, so nothing is asked of that session: %w", err)
		}
		return fmt.Errorf("the claim's session is not held to it, so nothing is asked of that session: %w", err)
	}
	if m.compacting == nil && m.protectedRecovery() {
		// The handoff asked for before may still run in this session: its answer never came, and
		// its bound has not passed (handoffGuard). A second one would queue behind it in Oh My
		// Pi, and its end would give back a claim the first handoff still holds.
		return errors.New("a handoff asked for earlier may still run in the claim's session, since no answer of it came, so no second one is asked before its bound passes")
	}
	if p := m.claim.Pending; p != nil && p.awaitsStart() {
		// A take whose start is not recorded and whose report's finish the store refused
		// (takePhase, waitFinish) is finished by that report (makeOwedFinish), and no handoff
		// protects it: its report decided.
		if _, err := m.makeOwedFinish(ctx); err != nil {
			return err
		}
	}
	was := m.claim.State
	m.compacting = &compacting{conn: conn, generation: m.claim.Generation, back: was}
	m.claim.State = StateWorking
	m.log.Info("supervise: the claim's session is being compacted; it is busy until the compaction ends",
		"generation", m.claim.Generation, "session", m.claim.Session)
	if err := m.persist(ctx); err != nil {
		m.compacting = nil
		m.claim.State = was
		return err
	}
	if p := m.claim.Pending; p != nil && m.takeAwaitsTurn() {
		m.log.Info("supervise: the compaction keeps the accepted take; a handoff aborts no turn, and the take's turn starts once the handoff is over",
			"task", p.Number)
		m.protectTake(m.deps.Clock.Now().Add(runtime.CompactTimeout))
	}
	return nil
}

// compactEnded gives the claim back once Oh My Pi has answered, or once the wait for it ran out.
// It is turnEnded's shape, for the same reason: the claim was working, nothing of it is working
// now, and what was held for that is owed now — the suspension an operator asked for while the
// compaction ran, and the offer of the task the claim owes untaken, whose value is written again
// whether the compaction ran or was refused (dispatch://INFRA-956 R3). A take accepted before the
// compaction is kept, not offered again: its own turn, which the handoff aborted none of, runs the
// task (compactStart). A claim that owes nothing is offered nothing. Oh My Pi's answer, a
// shortened session or a refusal, also ends the handoff guard (handoffGuard): the handoff is over.
// A wait that ran out or a transport that failed ends no guard, since the handoff may still run.
// With nothing held, the end is a turn's end's own (endNothingHeld): a stale-plugin relaunch held
// for the compaction, or for the take's turn before it, runs here only where the task that turn
// was of is settled, and otherwise at that task's finish by the end-frame bound or by the report
// of the take's turn, as at the end of the turn.
//
// A compaction that ran puts the claim's run of provider-error turns back to zero, so the notice
// at providerErrorRunTold can go a second time if the compacted session's turns go the same way;
// without it the notice would go once and not again until another of the run's ends
// (Claim.ProviderErrorRun) came. It is this action's own reset and this action's own write:
// forgetSession's reset belongs to a claim that is losing its session, and a compaction keeps it.
// A compaction that was refused or skipped resets nothing — the session is as it was, so the run
// of refused turns it carried still stands.
//
// The ladder's wait goes whatever the compaction did (TimerPromptRetry), so the owed task is
// offered from here rather than at the end of a wait that was counted against the transcript this
// operation just shortened: the whole point of compacting a claim the provider keeps refusing
// is to put the task in front of it again, and the next step of that ladder can be ten minutes
// away (promptRetryDelay). What the ladder counted stays on the delivery
// (Delivery.ProviderErrorRetries), so a turn that fails again waits the step it had reached.
//
// A turn Oh My Pi started under the compaction and has not ended — a follow-up an Envoy
// delivery queued while the session was being compacted, or the turn of a take accepted before
// the handoff, whose text Oh My Pi parks until the handoff ends — leaves the claim working for
// that turn, and that turn's end gives the claim back and offers the owed task, or the report of
// the take's turn settles the task (turnEnded, reportedTask). Idling the claim here would offer
// the owed task while a turn runs and leave the daemon reading the agent as free while it works.
//
// A suspension held over the claim is the one end that gives the claim back to nothing, and it
// says so to whoever asked for the compaction (ErrCompactSuspended). The hold runs here, as
// the turn's end runs it, and leaves the claim suspended with its process stopped; where a
// turn is running it is left to that turn's end, and the claim is working for it; and where the
// compaction began after a take's turn had ended with its report still to come, it waits for that
// report as at the turn's end, behind the wait for the take's report (reportwait.go, takePhase):
// the claim goes back idle holding it, and the report's finish runs it (reportedTask,
// settleThenAfterTurn), or that wait's bound or its own deadline (reportBoundOver,
// suspendAtTimeout). Run here, its stop would come before that report, which the suspended claim
// ignores, and settle would take back as a cut turn a task whose turn ran whole. None of these is
// a claim back on the task it owes, and that task is not offered each way, so an answer of nil —
// which its caller reads as the claim back and its task offered again (Machine.Compact, api's
// CompactResponse) — would be false of each. The hold that runs here stops a process in no turn,
// so what its let-go records reads the claim in the state the compaction gives it back in
// (compacting.back, suspendHeldAtEnd), and the next process
// is not told a turn was cut off. A stop the runtime refuses gives the claim back over a task whose
// turn is over, with the end-frame bound armed for it where none stands, as the end with nothing
// held arms it (runHeldAtEnd, endNothingHeld).
func compactEnded(m *Machine, ctx context.Context, ev Event) error {
	ended := ev.(CompactEnded)
	// The fence let this end through, so the claim holds the compaction it names (ownsCompaction).
	run := *m.compacting
	m.compacting = nil
	m.disarm(TimerPromptRetry)
	if ended.Answered {
		// Oh My Pi answered the handoff, so it is over, and its turn's start is the only wait of
		// the take: the bounds are the usual ones from here (handoffGuard).
		m.dropHandoff()
	}
	if ended.Compacted {
		m.claim.ProviderErrorRun = 0
	}
	m.log.Info("supervise: the compaction of the claim's session is over", "compacted", ended.Compacted,
		"providerErrorRun", m.claim.ProviderErrorRun, "inTurn", run.inTurn)
	if run.inTurn {
		// The claim stays working and keeps whatever is held for a turn's end, the suspension
		// among it: this is not that end.
		if err := m.persist(ctx); err != nil {
			return err
		}
		if m.held != nil {
			return ErrCompactSuspended
		}
		return nil
	}
	if m.held != nil {
		// A suspension that arrived while the compaction ran was held for it, as one that
		// arrives in a turn is (holdSuspension); it runs now, or waits for the take's report,
		// exactly as the turn's end runs it.
		if err := m.runHeldAtEnd(ctx, run.back, m.armEndFrameBoundUnlessArmed); err != nil {
			return err
		}
		return ErrCompactSuspended
	}
	return m.endNothingHeld(ctx, run.back, m.armEndFrameBoundUnlessArmed)
}

// handoffAnswered is whether err is Oh My Pi's own answer to a handoff (runtime.Conn's Compact):
// nil for a session shortened, or a refusal that carries its words (runtime.RefusalReason), a
// handoff that made no document among them. A wait that ran out and a transport that failed are
// no answer: the handoff may still run in the session.
func handoffAnswered(err error) bool {
	var refusal interface{ RefusalReason() string }
	return err == nil || errors.As(err, &refusal)
}

// handoffProtectsLine is the line of a take the handoff guard protects (protectTake), at Info,
// with the take's number and the guard's deadline.
const handoffProtectsLine = "supervise: a handoff may run over the accepted take, so its bounds wait for the handoff's deadline"

// handoffAskLine is the line of the one ask a protected hello makes (askProtected), at Info, with
// the take's number and the ask's deadline.
const handoffAskLine = "supervise: the shim reconnected over a take a handoff protects; the daemon asks its agent whether it is in a turn and does not wait for the answer"

// handoffGuard is the one fixed deadline of a take awaiting its turn that a handoff may still run
// over (departure 10 of dispatch://INFRA-2811's plan): its process generation and number, the
// deadline, and the cancel of the one ask a protected hello made (askProtected), nil for none. The
// pinned Oh My Pi runs a handoff and a get_state on one serial queue, so while a handoff runs no
// ask of whether the agent is in a turn is answered, and the usual reconnect deadline
// (reconnectBound) and a short second wait can pass before the handoff ends and the take's text
// starts its turn. A take the guard protects is charged, relaunched and offered again by nothing
// before its deadline:
//
//   - the reconnect deadline of a close (reconnectFromClose), of a hello (helloView) and of a
//     daemon start (NewMachine) is the later of its usual time and the guard's deadline, and a new
//     hello does not renew the guard;
//   - a hello asks once on its connection, with no wait inside the decision and a lifetime through
//     its reconnect deadline (askProtected), and the answer is applied as every answer is
//     (turnAnswered); the next hello or a close cancels it;
//   - the second wait, once the view is whole, is due no earlier than the guard's deadline
//     (armWaits).
//
// The live path's deadline is the handoff request's own, runtime.CompactTimeout from the ask
// (compactStart). A daemon start reads a claim stored working with a take accepted and its turn
// not started as one a handoff may still run over, and gives it the deadline of the start plus
// runtime.CompactTimeout (NewMachine). A compaction's start writes that record, and so does a take
// put back behind a turn that was not its own while that turn runs (putBack). The stored state
// cannot tell the two apart, so the start protects both: the second's bounds wait at most until
// that deadline, and an answer that its agent is streaming gives neither take the turn
// (answerView).
//
// Oh My Pi's answer to the handoff, a shortened session or a refusal, ends the guard
// (compactEnded): the handoff is over. A wait that ran out and a transport that failed end
// nothing, since the handoff may still run. The take's turn starting, its finish and its take-back
// make the guard moot (handoffUntil), and a let-go drops it (letGo). At the deadline the ordinary
// paths run: the reconnect deadline's over a view that is not whole, and the second wait's over a
// whole one.
type handoffGuard struct {
	generation uint64
	number     uint64
	until      time.Time
	ask        context.CancelFunc
	// anchorConn is the sequence of the connection whose protected hello left a restored take's
	// anchor to the ask's answer rather than a probe that would wait behind the handoff
	// (bindRestoredAnchor, bindAnchorAtAnswer), 0 for none.
	anchorConn uint64
}

// protectTake gives the pending take, which awaits its turn, the handoff deadline until for the
// claim's process generation. A take the guard protects already keeps the deadline it has.
func (m *Machine) protectTake(until time.Time) {
	p := m.claim.Pending
	if g := m.handoff; g != nil && g.generation == m.claim.Generation && g.number == p.Number {
		return
	}
	m.dropHandoff()
	m.handoff = &handoffGuard{generation: m.claim.Generation, number: p.Number, until: until}
	m.log.Info(handoffProtectsLine, "task", p.Number, "until", until)
}

// handoffUntil is the handoff guard's deadline, and whether it protects the pending take: it is
// this process generation's and this take's, and the take still awaits its turn.
func (m *Machine) handoffUntil() (time.Time, bool) {
	g, p := m.handoff, m.claim.Pending
	if g == nil || p == nil || g.generation != m.claim.Generation || g.number != p.Number || !m.takeAwaitsTurn() {
		return time.Time{}, false
	}
	return g.until, true
}

// protectedRecovery is whether a take the handoff guard protects is still short of its deadline.
func (m *Machine) protectedRecovery() bool {
	until, protected := m.handoffUntil()
	return protected && m.deps.Clock.Now().Before(until)
}

// laterOfHandoff is due, or the handoff guard's deadline where it protects the pending take and is
// later.
func (m *Machine) laterOfHandoff(due time.Time) time.Time {
	if until, protected := m.handoffUntil(); protected && until.After(due) {
		return until
	}
	return due
}

// dropHandoff ends the handoff guard, and the ask its hello made with it.
func (m *Machine) dropHandoff() {
	m.cancelProtectedAsk()
	m.handoff = nil
}

// cancelProtectedAsk cancels the ask a protected hello made, where one is pending: a new hello's
// connection, a close, the reconnect deadline and a let-go each end it.
func (m *Machine) cancelProtectedAsk() {
	if g := m.handoff; g != nil && g.ask != nil {
		g.ask()
		g.ask = nil
	}
}

// askProtected makes the one ask of a hello over a take the handoff guard protects, on conn:
// whether the agent is in a turn, with a lifetime through the reconnect deadline the hello armed,
// on a goroutine of its own, so the decision does not wait behind the handoff on Oh My Pi's queue.
// The answer reaches the machine as every answer does, behind the frames before it
// (StreamTurnAnswered, turnAnswered); the stream delivers it only while this request still waits
// for it, on the connection it was made on. The ask is not counted among the machine's goroutines
// (Machine.Wait): it may wait for minutes, and its context ends with the machine's.
func (m *Machine) askProtected(conn runtime.Conn) {
	m.cancelProtectedAsk()
	until := m.handoff.until
	if a, armed := m.timers[TimerRegistration]; armed && a.due.After(until) {
		until = a.due
	}
	asking, cancel := context.WithDeadline(m.ctx, time.Now().Add(until.Sub(m.deps.Clock.Now())))
	m.handoff.ask = cancel
	m.log.Info(handoffAskLine, "task", m.claim.Pending.Number, "until", until)
	log := m.log
	go func() {
		defer cancel()
		if _, err := conn.GetState(asking); err != nil && asking.Err() == nil {
			log.Warn("supervise: the agent could not say whether it is in a turn after its shim reconnected over a take a handoff protects", "error", err)
		}
	}()
}

// armNoEarlierThanHandoff arms timer kind after from now, or at the handoff guard's deadline where
// it protects the pending take and is later (laterOfHandoff).
func (m *Machine) armNoEarlierThanHandoff(kind TimerKind, after time.Duration, deliveryID string) {
	if until, protected := m.handoffUntil(); protected && until.After(m.deps.Clock.Now().Add(after)) {
		m.armAt(kind, until, deliveryID)
		return
	}
	m.arm(kind, after, deliveryID)
}
