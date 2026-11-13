package supervise

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A claim's task reaches its worker as a value the worker takes, not as a prompt the daemon
// pushes (dispatch://INFRA-2811). The record queues a task under a number (queue), writes the
// claim's value in the task bucket under that number (TaskValues), and the worker, once its host
// is idle, asks to take the number it was handed (RequestTake). The answer is decided here, from
// the record: accepted with the text to run, refused naming the hold that stands, or dropped with
// the value deleted. Everything a send used to do happens in that answer, and the task's turn is
// the first turn that starts after it (turnStarted). The task is finished by the turn-outcome
// report that names its number, or by the end of that turn when no report comes (finishTask).

// TaskValues is the task bucket as a machine writes it: one value per claim, keyed on the claim's
// token, naming the number of the task the record owes it. The daemon's implementation is the
// legion_tasks bucket on NATS (internal/daemon's task bucket); a fake stands in for it in tests.
//
// A write answers the bucket's epoch and the revision the write made, which the claim records as
// the daemon's own last write (Claim.TaskWrittenEpoch, TaskWrittenRevision). Each call is a store
// write of the machine's own, made under the machine's lock and the claim-write deadline, as its
// claim writes are (write).
type TaskValues interface {
	// Write puts the claim's value under number, and answers the epoch of the bucket it wrote
	// to and the revision the write made.
	Write(ctx context.Context, token claim.Token, number uint64) (epoch string, revision uint64, err error)
	// Delete deletes the claim's value.
	Delete(ctx context.Context, token claim.Token) error
	// Read is the claim's current value, and false where the bucket holds none for it.
	Read(ctx context.Context, token claim.Token) (TaskValue, bool, error)
}

// TaskValue is one claim's value as the bucket holds it: the task number, and the epoch and
// revision of the write that put it there. A revision is read only beside its epoch, because a
// bucket created again counts its revisions from 1 again.
type TaskValue struct {
	Number   uint64
	Epoch    string
	Revision uint64
}

// TaskTakeContract is the plugin contract at which a worker takes its task rather than being
// sent it (dispatch://INFRA-2811): the contract this change's entry in internal/api's
// version.go declares, and the one whose state body first carries `unseenTaskValues` (R5's
// claims whose value nobody reported seeing). internal/api serves a caller in a pod whose claims
// record a contract below it the state without that key (stateFor, withoutUnseenTaskValues).
//
// It lives here rather than beside DaemonAPIVersion because internal/api imports this package,
// as PodUpdateContract does. It is the first number above the base's 98; api.DaemonAPIVersion is
// raised to it with the routes a worker takes through, and a merge that renumbers this change
// moves both together.
const TaskTakeContract = 99

// The origins of a task: the source that queued it (Delivery.Origin). A request names its own
// (RequestDeliver.Origin); one that names none is read as originOf says.
const (
	OriginPhaseStart      = "phase-start"
	OriginResume          = "resume"
	OriginWake            = "wake"
	OriginNotice          = "notice"
	OriginUpdateRound     = "update-round"
	OriginOperator        = "operator"
	OriginCheckLook       = "check-look"
	OriginControllerStart = "controller-start"
)

// originOf is the origin a queued task is recorded under: the request's own, or, for a request
// that names none, a phase start where it names a phase and the operator's otherwise — the
// reading of a row written before the origin was recorded (decision 7 of INFRA-2811's plan).
func originOf(request RequestDeliver) string {
	switch {
	case request.Origin != "":
		return request.Origin
	case request.Phase != "":
		return OriginPhaseStart
	default:
		return OriginOperator
	}
}

// RequestTaskSeen is the worker reporting that it was handed the claim's value at this epoch and
// revision, whether or not its host is idle. Number is the task the value names. The claim
// records the newest such report (Claim.TaskSeenEpoch, TaskSeenRevision), and a revision it had
// not recorded leaves no take at it yet (Claim.TaskTakeState). Generation is the launch the
// reporting process runs at, and Session its session where the caller names one: the boot form
// of the claim's route names none, since its boot token already names the launch.
type RequestTaskSeen struct {
	Claim      claim.Token
	Generation uint64
	Session    string
	Number     uint64
	Epoch      string
	Revision   uint64
}

// RequestTake is the worker asking to take the task Number, the one its value at this epoch and
// revision names, because its host is idle. It is answered through Machine.Take, which answers
// every take (TakeAnswer). Generation and Session are fenced as RequestTaskSeen's are.
type RequestTake struct {
	Claim      claim.Token
	Generation uint64
	Session    string
	Number     uint64
	Epoch      string
	Revision   uint64
}

// TakeAdopted is the working copy's adoption a take made outside the machine's lock (Machine.Take),
// posted back with its outcome: Err nil for an adoption the shim made, and the failure otherwise.
// Number and Generation are the task and the launch the adoption was made for, so an adoption
// that outlived either answers the take refused rather than accepting a task the claim no longer
// holds that way.
type TakeAdopted struct {
	Claim      claim.Token
	Generation uint64
	Number     uint64
	Err        error
}

func (RequestTaskSeen) isEvent() {}
func (RequestTake) isEvent()     {}
func (TakeAdopted) isEvent()     {}

// TakeOutcome is how a take was answered.
type TakeOutcome string

const (
	// TakeOutcomeAccepted is a take the claim's worker may run: the answer carries the text.
	TakeOutcomeAccepted TakeOutcome = "accepted"
	// TakeOutcomeRefused is a take a hold refused: the answer names it, and the value is written
	// again once no hold stands (offerPending).
	TakeOutcomeRefused TakeOutcome = "refused"
	// TakeOutcomeDropped is a take of a task the record no longer owes the claim: its value is
	// deleted.
	TakeOutcomeDropped TakeOutcome = "dropped"
)

// The holds a take is refused by, by name. The first seven are the holds a send used to stop at;
// the rest are a take the claim's own state refuses.
const (
	// HoldIssueInTurn is another role of the claim's issue in a turn, or holding an accepted take
	// whose turn has not ended, while this task would adopt the working copy they share.
	HoldIssueInTurn = "issue-in-turn"
	// HoldSuspension is a suspension held over the claim (holdSuspension).
	HoldSuspension = "suspension"
	// HoldUpdateRound is an update round the claim has answered (answeredOpenRound).
	HoldUpdateRound = "update-round"
	// HoldLaunchWaits is a launch the wake pacing holds over the claim's process (launchWaits).
	HoldLaunchWaits = "launch-waits"
	// HoldStalePlugin is a plugin stale enough that the daemon refuses the claim's every `legion`
	// call (staleRefusesClaim).
	HoldStalePlugin = "stale-plugin"
	// HoldRootHears is a root architect's task whose root has no architect to hear it (rootHears).
	HoldRootHears = "root-hears"
	// HoldProviderError is the wait a provider error armed for the task (waitsForProviderError).
	HoldProviderError = "provider-error"
	// HoldAdoption is a working copy the shim did not adopt, or a role identity the daemon could
	// not mint for it (adopt).
	HoldAdoption = "adoption"
	// HoldNotOwed is a number the claim's record does not owe it: it records nothing.
	HoldNotOwed = "not-owed"
	// HoldInTurn is a claim in a turn: its own, or the turn of this very task, already started.
	HoldInTurn = "in-turn"
	// HoldNotReady is a claim whose agent has not said it is ready.
	HoldNotReady = "not-ready"
)

// TakeAnswer is a take's answer: its outcome, the text an accepted take runs, and the hold a
// refused one names.
type TakeAnswer struct {
	Outcome TakeOutcome
	Text    string
	Hold    string
}

// takeAnsweredLine is the line every answer to a take is logged under, at Info.
const takeAnsweredLine = "supervise: a take was answered"

// taking is a take being answered (Machine.Take): the request, and its answer once it has one.
// done closes when the answer is in, so a second take of the same claim that arrives meanwhile
// waits for it and answers from it (decision 11 of INFRA-2811's plan).
type taking struct {
	request RequestTake
	answer  TakeAnswer
	err     error
	done    chan struct{}
}

// takeDecision is what the take's row decided, read back by Machine.Take once the row returns:
// an answer, or the adoption the answer waits on.
type takeDecision struct {
	answer TakeAnswer
	adopt  *adoption
}

// adoption is the working copy's adoption a take makes before it is answered, captured under the
// machine's lock so the adoption acts on the claim the holds were decided against.
type adoption struct {
	number     uint64
	generation uint64
	delivery   string
	phase      phase.Phase
	token      claim.Token
	issue      string
	role       claim.Role
	loc        *runtime.Locator
}

// Take answers a take of the claim's task. The answer comes from the record and is applied in
// the same decision (take), save for the one part that is no decision of the machine's own: the
// adoption of the working copy a phase task makes for its role. That runs with the machine's lock
// let go, as a send's adoption ran on a goroutine of its own, because a shim's adoption can take
// seconds or its whole command timeout, and every other event of the claim would wait behind it.
// Its outcome comes back as an event of its own (TakeAdopted), where the generation and the
// number are fenced again and the take is accepted or refused.
//
// The issue's roles share one working copy, so a take that adopts holds the issue's lock from the
// in-turn check to its answer (Deps.LockIssue): two roles cannot both pass the check and both
// adopt. The lock is taken before the machine's own, and never the other way round, so no machine
// waits on an issue while holding its claim.
//
// A second take of this claim that arrives while one is being answered waits for that answer.
// One of the same number, epoch and revision — a worker sending again a take that had no answer
// in time — is answered from it, accepted text or refusal alike, and starts no second adoption;
// any other is decided afresh once the first is done.
func (m *Machine) Take(ctx context.Context, request RequestTake) (TakeAnswer, error) {
	for {
		m.mu.Lock()
		inFlight := m.taking
		if inFlight == nil {
			break
		}
		m.mu.Unlock()
		select {
		case <-inFlight.done:
		case <-ctx.Done():
			return TakeAnswer{}, ctx.Err()
		}
		if sameTake(inFlight.request, request) {
			return inFlight.answer, inFlight.err
		}
	}
	t := &taking{request: request, done: make(chan struct{})}
	m.taking = t
	issue := m.claim.Issue
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.taking = nil
		m.mu.Unlock()
		close(t.done)
	}()
	if m.deps.LockIssue != nil && issue != "" {
		unlock := m.deps.LockIssue(issue)
		defer unlock()
	}
	t.answer, t.err = m.answerTake(ctx, request)
	return t.answer, t.err
}

// sameTake is whether two takes are the one ask: the same number at the same revision of the
// same epoch.
func sameTake(a, b RequestTake) bool {
	return a.Number == b.Number && a.Epoch == b.Epoch && a.Revision == b.Revision
}

// answerTake decides the take, makes the adoption it waits on, and answers it. A decision that
// fails after the take was answered returns that answer beside its error: the worker's take was
// answered, and the failure is the rest of the decision's — a retirement the refused adoption's
// charge made that the tree lifecycle refused, a claim write the store did not take. That failure
// is logged here, since the caller answers the worker and has nothing of its own to do with it.
// One that fails before any answer returns the zero answer.
func (m *Machine) answerTake(ctx context.Context, request RequestTake) (TakeAnswer, error) {
	if err := m.Handle(ctx, request); err != nil {
		return m.answeredBeside(request, err)
	}
	decided := m.takeDecided()
	if decided.adopt == nil {
		return decided.answer, nil
	}
	a := decided.adopt
	err := m.adopt(ctx, *a)
	// The answer is the claim's whatever became of ctx: a caller that hung up still leaves the
	// adoption's outcome to record.
	if err := m.Handle(context.WithoutCancel(ctx), TakeAdopted{Claim: a.token, Generation: a.generation, Number: a.number, Err: err}); err != nil {
		return m.answeredBeside(request, err)
	}
	return m.takeDecided().answer, nil
}

// answeredBeside is the answer a failed decision left, if it left one, with the failure, which is
// logged where an answer was given.
func (m *Machine) answeredBeside(request RequestTake, err error) (TakeAnswer, error) {
	answer := m.takeDecided().answer
	if answer.Outcome != "" {
		m.log.Warn("supervise: the decision a take was answered in failed after its answer",
			"task", request.Number, "outcome", string(answer.Outcome), "error", err)
	}
	return answer, err
}

// takeDecided is the take's decision the last take row left, and clears it.
func (m *Machine) takeDecided() takeDecision {
	m.mu.Lock()
	defer m.mu.Unlock()
	decided := m.decided
	m.decided = takeDecision{}
	return decided
}

// answer records a take's answer for Machine.Take and logs it. Every take row ends here, so every
// answer is logged once.
func (m *Machine) answer(take RequestTake, answer TakeAnswer) error {
	m.decided = takeDecision{answer: answer}
	m.log.Info(takeAnsweredLine, "task", take.Number, "revision", take.Revision, "outcome", string(answer.Outcome), "hold", answer.Hold)
	return nil
}

// refuseTake answers a take refused by hold, recording the refusal at the take's revision.
func (m *Machine) refuseTake(ctx context.Context, take RequestTake, hold string) error {
	if m.recordTake(take, TakeRefused) {
		if err := m.persist(ctx); err != nil {
			return err
		}
	}
	return m.answer(take, TakeAnswer{Outcome: TakeOutcomeRefused, Hold: hold})
}

// recordTake records what came of a take at its revision (Claim.TaskTakeState), and reports
// whether it changed the claim. A take at a revision newer than the one the worker last reported
// seeing is the worker's word that it saw that one, so it is recorded as seen too; one at an older
// revision of the same epoch is an ask the worker has since replaced, and records nothing.
func (m *Machine) recordTake(take RequestTake, state TakeState) bool {
	c := &m.claim
	switch {
	case take.Epoch != c.TaskSeenEpoch || take.Revision > c.TaskSeenRevision:
		c.TaskSeenEpoch, c.TaskSeenRevision = take.Epoch, take.Revision
	case take.Revision < c.TaskSeenRevision:
		return false
	}
	if c.TaskTakeState == state {
		return false
	}
	c.TaskTakeState = state
	return true
}

// take is a take reaching a claim that is ready or idle, the one state a worker asks from. It
// runs what a send used to run, in that order (takeable): the holds, the two drops, the root's
// narrowing, and then the adoption of the working copy, which goes back to Machine.Take to make;
// a take that adopts nothing is accepted here.
//
// Three takes are answered before any of that, from the record alone. A number the claim's record
// does not owe it — no task pending, another one pending — is refused not-owed and records
// nothing. A number already taken whose turn has not started is a take sent again after its
// answer was lost: it is answered the same text, which the record kept (Delivery.TakenText), and
// applies nothing a second time — no adoption, no write, no telling. And a number whose turn has
// already started is refused in-turn.
func take(m *Machine, ctx context.Context, ev Event) error {
	request := ev.(RequestTake)
	if answered, err := m.answeredFromRecord(request); answered {
		return err
	}
	return m.takePending(ctx, request)
}

// answeredFromRecord answers the three takes the record alone decides (take), and reports whether
// it answered this one.
func (m *Machine) answeredFromRecord(request RequestTake) (bool, error) {
	p := m.claim.Pending
	switch {
	case request.Number == 0 || p == nil || p.Number != request.Number:
		return true, m.answer(request, TakeAnswer{Outcome: TakeOutcomeRefused, Hold: HoldNotOwed})
	case p.awaitsTurn():
		return true, m.answer(request, TakeAnswer{Outcome: TakeOutcomeAccepted, Text: p.TakenText})
	case p.taken():
		return true, m.answer(request, TakeAnswer{Outcome: TakeOutcomeRefused, Hold: HoldInTurn})
	}
	return false, nil
}

// takePending decides a take of the pending task, which no take has been accepted for: a hold or
// a drop answers it here, an adoption sends it back to Machine.Take, and anything else accepts
// it.
func (m *Machine) takePending(ctx context.Context, request RequestTake) error {
	m.recordTake(request, TakeMade)
	hold, dropped, err := m.takeable(ctx)
	switch {
	case err != nil:
		return err
	case dropped:
		m.recordTake(request, TakeRefused)
		if err := m.persist(ctx); err != nil {
			return err
		}
		return m.answer(request, TakeAnswer{Outcome: TakeOutcomeDropped})
	case hold != "":
		return m.refuseTake(ctx, request, hold)
	}
	p := m.claim.Pending
	if m.adopts(*p) {
		m.decided = takeDecision{adopt: &adoption{
			number: p.Number, generation: m.claim.Generation, delivery: p.ID, phase: p.Phase,
			token: m.claim.Token, issue: m.claim.Issue, role: m.claim.Role, loc: m.claim.Locator,
		}}
		return nil
	}
	return m.accept(ctx, request)
}

// takeInTurn is a take reaching a working claim. A working claim is in a turn, and a take into a
// turn is refused in-turn, but for one claim: the one a restart restored working with its task
// owed and no take of it accepted, whose turn this daemon has heard nothing of (askFirst). The
// store holds such a claim when a compaction the restart cut off had taken it, and its worker
// asks only once its host is idle, so the take is the worker's word that it is: the claim is
// idle again, the turn the store recorded is over, and the take is answered as an idle claim's
// is. Its turn then starts as any task's does, and moves the claim to working for that take.
func takeInTurn(m *Machine, ctx context.Context, ev Event) error {
	request := ev.(RequestTake)
	if answered, err := m.answeredFromRecord(request); answered {
		return err
	}
	if !m.askFirst {
		return m.refuseTake(ctx, request, HoldInTurn)
	}
	m.log.Info("supervise: a take reached a claim a restart restored working; the worker's take says its host is idle, so the turn the store recorded is over",
		"task", request.Number)
	m.claim.State = StateIdle
	m.askFirst, m.unseenTurn = false, false
	if err := m.persist(ctx); err != nil {
		return err
	}
	return m.takePending(ctx, request)
}

// takeNotReady is a take reaching a claim whose agent has not said it is ready: refused
// not-ready, and written again at the ready (finishReady, offerPending).
func takeNotReady(m *Machine, ctx context.Context, ev Event) error {
	request := ev.(RequestTake)
	if answered, err := m.answeredFromRecord(request); answered {
		return err
	}
	return m.refuseTake(ctx, request, HoldNotReady)
}

// adopted is the working copy's adoption a take made, back with its outcome. The claim may have
// moved while the adoption ran with the lock let go — a death, a suspension, a newer task — so
// the take is decided again against the claim as it stands: a task that is no longer the one
// the adoption was made for is refused not-owed, a hold that stands now refuses it by name, a
// failed adoption refuses it with the adoption's own hold, and anything else accepts it.
//
// A failed adoption is recorded and charged as a send's refused adoption was (refusedAdoption).
func adopted(m *Machine, ctx context.Context, ev Event) error {
	a := ev.(TakeAdopted)
	request := m.adoptedRequest(a)
	p := m.claim.Pending
	if a.Generation != m.claim.Generation || p == nil || p.Number != a.Number || p.taken() {
		return m.answer(request, TakeAnswer{Outcome: TakeOutcomeRefused, Hold: HoldNotOwed})
	}
	if a.Err != nil {
		return m.refusedAdoption(ctx, request, a.Err)
	}
	if hold := m.claimHold(); hold != "" {
		return m.refuseTake(ctx, request, hold)
	}
	return m.accept(ctx, request)
}

// adoptedTooLate is an adoption's outcome reaching a claim that is no longer ready or idle: the
// take is refused by the state the claim is in now, whatever the adoption did.
func adoptedTooLate(m *Machine, ctx context.Context, ev Event) error {
	a := ev.(TakeAdopted)
	request := m.adoptedRequest(a)
	hold := m.claimHold()
	if hold == "" {
		hold = HoldNotReady
	}
	if a.Generation != m.claim.Generation {
		return m.answer(request, TakeAnswer{Outcome: TakeOutcomeRefused, Hold: hold})
	}
	return m.refuseTake(ctx, request, hold)
}

// adoptedRequest is the take an adoption's outcome answers: the one Machine.Take is answering,
// or, posted without one, a take of the adoption's own number.
func (m *Machine) adoptedRequest(a TakeAdopted) RequestTake {
	if t := m.taking; t != nil && t.request.Number == a.Number {
		return t.request
	}
	return RequestTake{Claim: a.Claim, Generation: a.Generation, Number: a.Number}
}

// refusedAdoption is a take whose adoption of the working copy failed. It is recorded on the
// delivery as a failed send was (Delivery.SendFailures and its two companions, scrubbed), and the
// split of what costs budget is the one a send's refusal made: a refused adoption is the shim's
// own answer about this workspace and is charged (chargePrompt), so it cannot repeat without
// bound — the third charge retires the process — while a role identity the daemon could not mint
// and a transport that lost the call are no answer at all and charge nothing. The take is refused
// with the adoption's hold either way; the value is written again once no hold stands, at the next
// sweep, hello or ready (offerPending).
//
// The record's write is carried past rather than returned when it fails, so the charge is always
// made (the delivery is left unsaved, and settle writes it again).
func (m *Machine) refusedAdoption(ctx context.Context, request RequestTake, err error) error {
	p := m.claim.Pending
	p.SendFailures++
	p.LastSendFailureAt = m.deps.Clock.Now()
	p.LastSendError = scrubSendError(err.Error())
	if err := m.putPending(ctx); err != nil {
		m.log.Warn("supervise: the failed adoption was not recorded on the delivery; settle writes it again and the charge goes on",
			"task", p.Number, "error", err)
	}
	m.recordTake(request, TakeRefused)
	if err := m.answer(request, TakeAnswer{Outcome: TakeOutcomeRefused, Hold: HoldAdoption}); err != nil {
		return err
	}
	switch {
	case errors.Is(err, runtime.ErrAdoptRefused):
		m.log.Warn("supervise: the shim refused to adopt the working copy; the take is refused", "task", p.Number, "error", err)
		return m.chargePrompt(ctx, "the working copy was not adopted: "+err.Error())
	case errors.Is(err, runtime.ErrIdentityUnavailable):
		m.log.Warn("supervise: the role's GitHub App identity could not be minted; the take is refused and the task waits",
			"task", p.Number, "error", err)
	default:
		m.log.Warn("supervise: the adoption of the working copy was lost to the transport; the take is refused and the task waits",
			"task", p.Number, "error", err)
	}
	return m.persist(ctx)
}

// accept accepts the take of the pending task, and is the confirmation a turn's start used to
// make: the delivery records the take (ConfirmedAt) and the text the take is answered with
// (TakenText), composed here with the notes the claim's state puts in front of the task
// (Delivery.message) — so a take sent again is answered with exactly this text, and after a
// restart too; the claim records the number taken (TaskTaken) and the take at its revision; the
// record of failed sends is cleared, since this task got through; and an abandoned session's
// sentence, which this text carries, is told. It all goes in one write, which is where the store
// tells the decisions the task names (store's putDelivery), and once that write holds the
// workflow is told the agent took its work (Deps.TaskConfirmed).
//
// The prompt budgets are not cleared here: a take resets no count (R5), and they go to zero at
// the start of this take's turn, as at any turn's start or end (turnStarted). A write that fails
// tells no one and is owed, as the confirmation's was: the take stands in memory, and a take
// sent again is answered from it.
func (m *Machine) accept(ctx context.Context, request RequestTake) error {
	p := m.claim.Pending
	text := p.message(m.claim)
	p.ConfirmedAt, p.TakenText = m.deps.Clock.Now(), text
	p.SendFailures, p.LastSendFailureAt, p.LastSendError = 0, time.Time{}, ""
	if m.claim.Abandoned.Tells() {
		m.claim.Abandoned.Told = true
	}
	m.claim.TaskTaken = p.Number
	m.recordTake(request, TakeAccepted)
	if err := m.saved(m.owes("PutClaimAndDelivery", m.putClaimAndPending(ctx))); err != nil {
		return err
	}
	if m.deps.TaskConfirmed != nil {
		m.deps.TaskConfirmed(m.current())
	}
	return m.answer(request, TakeAnswer{Outcome: TakeOutcomeAccepted, Text: text})
}

// takeable decides whether the pending task, which no take has been accepted for, may be taken
// now: the name of the hold that stands, a drop the record made here, or neither. It is what a
// send used to decide before it sent, in the same order, and both a take (takePending) and the
// hold's end (offerPending) ask it, so a value is written again exactly where a take would be
// accepted.
//
// The claim's own holds come first (claimHold). Then the two drops, which retire the task and
// delete its value: an ask of a round the claim is not in now — another round replaced it, or
// the claim's process left that round's pod — and a task whose issue has left the phase it was
// queued for. Then a root architect's task of a run, held while its root has no architect to hear
// it and narrowed to what it still names once the root hears (currentRootTask), and retired when
// that leaves it naming nothing. Last, for a task that adopts the working copy the issue's roles
// share, another role of the issue in a turn, or holding an accepted take whose turn has not
// ended (issueInTurn).
func (m *Machine) takeable(ctx context.Context) (hold string, dropped bool, err error) {
	if hold := m.claimHold(); hold != "" {
		return hold, false, nil
	}
	p := m.claim.Pending
	round, asks := updateRoundOf(p.ID)
	stale := asks && (round != m.claim.UpdateRound || m.claim.LeftPodRound(round))
	holds := !stale
	if holds {
		if holds, err = m.phaseHolds(ctx, *p); err != nil {
			return "", false, err
		}
	}
	if !holds {
		if stale {
			m.log.Info("supervise: this task asks for a round the claim is not in now; it is dropped rather than taken",
				"task", p.Number, "delivery", p.ID, "asked", round, "round", m.claim.UpdateRound, "left", m.claim.UpdateLeftRound)
		} else {
			m.log.Info("supervise: the issue has left the phase this task was queued for; it is dropped rather than taken",
				"task", p.Number, "delivery", p.ID, "issue", m.claim.Issue, "queuedFor", p.Phase)
		}
		return "", true, m.dropPending(ctx, "dropped")
	}
	if m.asksRoot(*p) {
		hears, err := m.rootHears(ctx, *p)
		if err != nil {
			return "", false, err
		}
		if !hears {
			m.log.Info("supervise: the root of this task's tree has no architect to hear it; the task stays pending",
				"task", p.Number, "issue", m.claim.Issue, "run", p.Generation)
			return HoldRootHears, false, nil
		}
		taken, err := m.currentRootTask(ctx)
		if err != nil || !taken {
			return "", !taken && err == nil, err
		}
	}
	if m.adopts(*m.claim.Pending) {
		if other, inTurn := m.issueInTurn(); inTurn {
			m.log.Info("supervise: a claim of this issue is in a turn; the phase task waits rather than adopt the working copy they share",
				"task", p.Number, "issue", m.claim.Issue, "inTurn", other)
			return HoldIssueInTurn, false, nil
		}
	}
	return "", false, nil
}

// claimHold is the hold the claim's own state puts on a take of its pending task, "" for none:
// the claim not ready or idle, a suspension held over it, a launch the wake pacing holds over its
// process, an update round it has answered, a plugin stale enough that its every `legion` call is
// refused, and the wait a provider error armed for this task. It reads the claim and its memory
// alone, so it is asked again after an adoption that ran with the lock let go (adopted).
//
// Four of them hold the task instead of dropping it, for the reason each was a hold on a send: a
// worker that answered its round is about to lose its process, and so is one whose launch waits
// its turn, and a task started there would be retired as served at a turn it never finished; a
// stale plugin's every call is refused, so a task taken there buys a model turn nothing can come
// of and puts the claim in a turn, which postpones the relaunch it is waiting for; and the
// provider-error wait is the task's own ladder (providererror.go).
func (m *Machine) claimHold() string {
	switch m.claim.State {
	case StateReady, StateIdle:
	case StateWorking:
		return HoldInTurn
	default:
		return HoldNotReady
	}
	switch {
	case m.held != nil:
		return HoldSuspension
	case m.launchWaits():
		return HoldLaunchWaits
	case m.claim.answeredOpenRound():
		return HoldUpdateRound
	case m.staleRefusesClaim():
		return HoldStalePlugin
	case m.claim.Pending != nil && m.waitsForProviderError(m.claim.Pending.ID):
		return HoldProviderError
	}
	return ""
}

// offerPending writes the claim's value again under its pending task's number where the worker
// asked at the latest revision the daemon wrote and the task is free to take now: the worker
// asks once per revision, so a value its take was refused at — or one whose take a provider
// error, a compaction or a death took back — says nothing new until it is written again. It is
// the end of every hold that refused a take, and runs wherever a send used to be retried: the
// agent's ready, the shim's hello, the sweep's observation, the end of a turn on this claim or
// another of its issue, a start's arrival, the end of a provider error's wait and of a
// compaction, the end of an update round. Which hold ended is not asked: takeable says whether
// any still stands, and the value is written only where none does, so a hold that still stands
// costs the worker nothing and the sweep never writes for one.
//
// A value the worker has not reported seeing at that revision is left alone: the worker has not
// asked at it. A write the bucket refused is made here at the next chance, whatever the worker
// asked (valueUnwritten).
func (m *Machine) offerPending(ctx context.Context) error { return m.offerValue(ctx, "hold-ended") }

// offerValue is offerPending with the cause its write is logged under: "hold-ended" for the end
// of a hold, and "retake" for a task taken back in this same process that can be taken again —
// the second wait's take-back (turnStartBoundOver).
func (m *Machine) offerValue(ctx context.Context, cause string) error {
	p := m.claim.Pending
	if p == nil || p.taken() {
		return nil
	}
	c := m.claim
	asked := c.TaskSeenEpoch == c.TaskWrittenEpoch && c.TaskSeenRevision == c.TaskWrittenRevision &&
		(c.TaskTakeState == TakeAccepted || c.TaskTakeState == TakeRefused)
	if !asked && !m.valueUnwritten {
		return nil
	}
	hold, dropped, err := m.takeable(ctx)
	if err != nil || dropped || hold != "" {
		return err
	}
	return m.writeValue(ctx, cause)
}

// writeValue writes the claim's value under the pending task's number and records the write as
// the daemon's own last (Claim.TaskWrittenEpoch, TaskWrittenRevision). A write the bucket
// refuses fails nothing of the decision: it is logged, and made again at the claim's next chance
// (valueUnwritten, offerPending).
func (m *Machine) writeValue(ctx context.Context, cause string) error {
	p := m.claim.Pending
	epoch, revision, written := m.putValue(ctx, *p, cause)
	if !written {
		return nil
	}
	m.claim.TaskWrittenEpoch, m.claim.TaskWrittenRevision = epoch, revision
	return m.persist(ctx)
}

// putValue writes d's number as the claim's value, under the claim-write deadline, and reports
// the write: a claim the daemon has no bucket for writes nothing and reports none.
func (m *Machine) putValue(ctx context.Context, d Delivery, cause string) (string, uint64, bool) {
	if m.deps.TaskValues == nil || d.Number == 0 {
		return "", 0, false
	}
	var epoch string
	var revision uint64
	err := m.write(ctx, func(ctx context.Context) error {
		var err error
		epoch, revision, err = m.deps.TaskValues.Write(ctx, m.claim.Token, d.Number)
		return err
	})
	if err != nil {
		m.valueUnwritten = true
		m.log.Warn("supervise: the task value could not be written; it is written again at the claim's next chance",
			"task", d.Number, "origin", d.Origin, "cause", cause, "error", err)
		return "", 0, false
	}
	m.valueUnwritten, m.valueWrittenAt = false, m.deps.Clock.Now()
	m.log.Info("supervise: a task value was written", "task", d.Number, "origin", d.Origin, "cause", cause,
		"epoch", epoch, "revision", revision)
	return epoch, revision, true
}

// deleteValue deletes the claim's value: the record no longer owes the task it named. A delete
// the bucket refuses is logged; the daemon's watch of the bucket repairs a value the record does
// not hold (internal/daemon's task bucket).
func (m *Machine) deleteValue(ctx context.Context, number uint64, cause string) {
	if m.deps.TaskValues == nil {
		return
	}
	if err := m.write(ctx, func(ctx context.Context) error { return m.deps.TaskValues.Delete(ctx, m.claim.Token) }); err != nil {
		m.log.Warn("supervise: the task value could not be deleted", "task", number, "cause", cause, "error", err)
		return
	}
	m.log.Info("supervise: a task value was deleted", "task", number, "cause", cause)
}

// dropPending retires the pending task because the record no longer owes it, and deletes its
// value: the take that meets it is answered dropped, and a worker reading the bucket finds
// nothing to ask about.
func (m *Machine) dropPending(ctx context.Context, cause string) error {
	number := m.claim.Pending.Number
	if err := m.retirePending(ctx); err != nil {
		return err
	}
	m.deleteValue(ctx, number, cause)
	return nil
}

// taskSeen records the newest revision of the claim's value its worker reports seeing. A revision
// of its epoch it had already recorded, or an older one, changes nothing: the take state is the
// newest revision's (Claim.TaskTakeState), and a new revision has had no take yet.
func taskSeen(m *Machine, ctx context.Context, ev Event) error {
	seen := ev.(RequestTaskSeen)
	c := &m.claim
	if seen.Epoch == c.TaskSeenEpoch && seen.Revision <= c.TaskSeenRevision {
		return nil
	}
	c.TaskSeenEpoch, c.TaskSeenRevision, c.TaskTakeState = seen.Epoch, seen.Revision, ""
	m.log.Debug("supervise: a task value was seen", "task", seen.Number, "epoch", seen.Epoch, "revision", seen.Revision)
	return m.persist(ctx)
}

// takeBack returns a taken task to waiting under the number it has, for a reason that keeps the
// process: a provider error's turn and a compaction. The take goes (ConfirmedAt, TakenText), so
// the next take of the number is decided afresh and its text composed again with whatever note
// the cause left (Delivery.message); the end of the task's turn is no longer awaited. The take
// state the claim recorded stays, which is how the end of the cause's hold knows the worker asked
// at this revision and writes the value again (offerPending).
//
// What the take-back establishes holds in memory at once, whether or not its write lands: a
// write that fails leaves the delivery unsaved, and settle writes it again.
func (m *Machine) takeBack(ctx context.Context, cause string) error {
	m.disarm(TimerFinish)
	p := m.claim.Pending
	p.ConfirmedAt, p.TakenText = time.Time{}, ""
	m.log.Info("supervise: a task was taken back", "task", p.Number, "cause", cause)
	return m.saved(m.putDelivery(ctx, *p))
}

// startTakesTurn is the write of a turn's start that is the turn of the pending task's accepted
// take, as confirm's was: the claim and the delivery together. The take's text goes (TakenText),
// since a take sent again from here on is not a take whose turn has yet to start. The claim it
// writes carries the prompt budgets at zero, which is the reset every turn's start makes
// (turnStarted, R5): the take that came before it reset nothing.
func (m *Machine) startTakesTurn(ctx context.Context) error {
	p := m.claim.Pending
	p.TakenText = ""
	return m.saved(m.owes("PutClaimAndDelivery", m.putClaimAndPending(ctx)))
}

// finishTask finishes the pending task, whose take's turn ran: the delivery retires, the claim
// records the number finished and the session that finished it (Claim.TaskFinished,
// TaskFinishedSession) in the same write, and the value is deleted. by names what finished it,
// for the line: the turn-outcome report that named its number ("report"), or the end of its turn
// with no report after it ("end-frame").
//
// A task that was an ask of the claim's open round is the worker's answer to that round when this
// daemon saw its turn start (answerByTurnEnd), and is read so here, before it retires.
//
// A finish whose retirement the store refused leaves the task pending with its turn over, so a
// ready or idle claim's end-frame bound is armed again for it: the bound that fired, or the
// report that named it, has been spent, and nothing else would finish it there (settle finishes
// it in every other state).
func (m *Machine) finishTask(ctx context.Context, by string) error {
	p := m.claim.Pending
	number, id := p.Number, p.ID
	m.answerByTurnEnd()
	if err := m.retireTask(ctx, true); err != nil {
		if _, armed := m.timers[TimerFinish]; !armed && (m.claim.State == StateReady || m.claim.State == StateIdle) {
			m.arm(TimerFinish, m.deps.Timeouts.RPC, id)
		}
		return err
	}
	m.log.Info("supervise: a task was finished", "task", number, "by", by)
	m.deleteValue(ctx, number, "finished")
	return nil
}

// finishAtTurnsEnd is the end-frame bound over: the task's turn ended, and no turn-outcome
// report named its number within the RPC timeout (Timeouts.RPC), so the end of the turn finishes
// it — which is what finishes a task a worker whose plugin reports no number took. What the turn's
// end held for the task runs now (afterTurn), as the report's finish runs it.
func finishAtTurnsEnd(m *Machine, ctx context.Context, _ Event) error {
	if err := m.finishTask(ctx, "end-frame"); err != nil {
		return err
	}
	if m.claim.State == StateWorking {
		return nil
	}
	return m.afterTurn(ctx)
}

// CurrentTask is what a claim's worker is told of its task when it registers or asks: the value
// the bucket holds for the claim now, nil where it holds none, and the number of the last task
// the claim finished.
type CurrentTask struct {
	Value    *TaskValue
	Finished uint64
}

// CurrentTask reads the claim's value from the bucket and the finished number from the claim as
// its machine last published it, waiting on no decision (View).
func (m *Machine) CurrentTask(ctx context.Context) (CurrentTask, error) {
	view := m.View()
	current := CurrentTask{Finished: view.TaskFinished}
	if m.deps.TaskValues == nil {
		return current, nil
	}
	value, held, err := m.deps.TaskValues.Read(ctx, view.Token)
	if err != nil {
		return CurrentTask{}, fmt.Errorf("read the task value of %s: %w", view.Token, err)
	}
	if held {
		current.Value = &value
	}
	return current, nil
}
