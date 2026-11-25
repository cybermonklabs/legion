package supervise

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A claim's task reaches its worker as a value the worker takes (dispatch://INFRA-2811). The
// record queues a task under a number (queue), writes the claim's value in the task bucket under
// that number (TaskValues, taskvalue.go), and the worker, once its host is idle, asks to take the
// number it was handed (RequestTake). The answer is decided here, from the record: accepted with
// the text to run, refused naming the hold that stands, or dropped with the value deleted. The
// holds, the drops, the adoption of the working copy and the telling of the workflow all happen
// in that answer, and the task's turn is the first turn that starts after it (turnStarted). The
// task is finished by the turn-outcome report that names its number, or, for a worker whose
// plugin names no number, by the end of that turn when no report comes (finishTask,
// TakesNoTask).

// TaskTakeContract is the plugin contract at which a worker takes its task rather than being
// sent it (dispatch://INFRA-2811): the contract whose entry in internal/api's version.go brings
// the take, task and task-seen routes, and the one whose state body first carries
// `unseenTaskValues` (R5's claims whose value nobody reported seeing). internal/api serves a
// caller below it the state without that key and with `never_reconnected` served as
// `never_registered`, and the registration without `task` and `finished` (shaping.go's
// stateNarrowings, valueStrips and shapeAnswer), and a claim whose plugin declares a contract
// below it cannot take a task at all.
//
// It lives here rather than beside DaemonAPIVersion because internal/api imports this package,
// as PodUpdateContract does, and internal/api's own test pins the two to one number
// (TestTaskTakeContractIsTheContractThatIntroducedTheTake): a merge that renumbers this change
// moves both together.
const TaskTakeContract = 291

// TakesNoTask is whether a plugin that declared contract takes no task: it is below
// TaskTakeContract, so its worker waits for a prompt this daemon never sends and never asks to
// take the task its claim is owed. A claim that recorded no contract at all (0) is one of them.
//
// Three rules read it. Two would each leave such a worker taking nothing at the rollout that moves
// the daemon and the worker image together, which opens an update round over every issue pod
// (dispatch://INFRA-2811, decision 18 and departure 3). The stale-plugin refusal and its lane
// hold no such claim out for a pod round: a round serves a claim on its old contract until the
// round ends, and on that contract the claim takes no task, so it is relaunched onto the current
// plugin inside the round (StaleRefusesCalls, internal/daemon's staleForTheLane). And the idle
// release takes the pod of such a claim whose plugin no relaunch moves, a Claude Code role in a
// pod, past the holds that would keep it (workflow's IdleRelease), since its pod's image is the
// one thing that can bring it a plugin that takes. The third is the end-frame finish
// (overTurnFinish, takePhase, interrupted, NewMachine's finishAtAct): such a worker's report names
// no task's number, so its report can finish nothing, and the end of its take's turn is what
// finishes the task where no report named the number within the RPC timeout. A claim whose plugin
// names its task's number has its task finished by the report that names it and by nothing else:
// the end of a run says nothing of whose run it was, since the daemon gives a take the first run
// that starts after it and the worker may run its text in the run after that (decision 8 of
// dispatch://INFRA-2811; notTheTakesTurn).
func TakesNoTask(contract int) bool { return contract < TaskTakeContract }

// The origins of a task: the source that queued it (Delivery.Origin). A request names its own
// (RequestDeliver.Origin); one that names none is read as originOf says. OriginSilence is the
// silence repair's (workflow's repairSilence), whichever task it writes — the work of an owed
// entry or the phase's resume task — so its writes are counted apart from a resume's: the repair
// is the backstop of a worker whose own report does not explain its quiet, not a relaunch's
// resume (dispatch://INFRA-2811). OriginNews is a task queued for news of the approved design
// alone (RequestDeliver.NewsOnly, the daemon's news-only start), whose writes are counted apart
// too: it hands the worker no work of its phase, only what changed in the design it stands on, so
// its writes are neither a phase start's nor a notice's.
const (
	OriginPhaseStart      = "phase-start"
	OriginResume          = "resume"
	OriginSilence         = "silence"
	OriginWake            = "wake"
	OriginNotice          = "notice"
	OriginNews            = "news"
	OriginOperator        = "operator"
	OriginCheckLook       = "check-look"
	OriginControllerStart = "controller-start"
)

// originOf is the origin a queued task is recorded under: the request's own, or, for a request
// that names none, the news origin where it is queued for news alone (RequestDeliver.NewsOnly, a
// start row of main's #3995, which named no origin), a phase start where it names a phase and the
// operator's otherwise — the reading of a row written before the origin was recorded
// (dispatch://INFRA-2811).
func originOf(request RequestDeliver) string {
	switch {
	case request.Origin != "":
		return request.Origin
	case request.NewsOnly:
		return OriginNews
	case request.Phase != "":
		return OriginPhaseStart
	default:
		return OriginOperator
	}
}

// RequestTake is the worker asking to take the task Number, the one its value at this revision
// names, because its host is idle. It is answered through Machine.Take, which answers every take
// (TakeAnswer). Generation and Session are fenced as RequestTaskSeen's are.
type RequestTake struct {
	Claim      claim.Token
	Generation uint64
	Session    string
	Number     uint64
	Revision   TaskRevision
	// t is the take Machine.Take is answering, which the take's rows decide it in (taking).
	t *taking
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
	// t is the take the adoption was made for, which its rows answer (taking).
	t *taking
}

func (RequestTake) isEvent() {}
func (TakeAdopted) isEvent() {}

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

// The holds a take is refused by, by name. The first nine are a state of the claim, its issue,
// its tree or its project that the task waits out; the rest are a take the claim's own state
// refuses.
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
	// HoldSessionNotHeld is a claim recording a session it does not hold, or one the store did not
	// answer the hold of (holdRecorded): another claim recorded a name of it first, or two did.
	HoldSessionNotHeld = "session-not-held"
	// HoldProjectPaused is the project's pause holding the claim (pauseHolds): while the project
	// is paused no task is taken by any role but the controller (decision 8 of
	// dispatch://INFRA-3759's plan).
	HoldProjectPaused = "project-paused"
	// HoldAdoption is a working copy the shim did not adopt, or a role identity the daemon could
	// not mint for it (adopt).
	HoldAdoption = "adoption"
	// HoldNotOwed is a number the claim's record does not owe it: it records nothing.
	HoldNotOwed = "not-owed"
	// HoldInTurn is a claim in a turn: its own, or the turn of this very task, already started. It
	// is also a merge forward of the claim's issue owed to a role and not landed, which a task that
	// adopts the working copy waits for whatever role takes it (Deps.MergeForwardOwed): the merge
	// runs in turns of the asked role that no take started, and writes that working copy between
	// them.
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

// The lines of a task's way through the claim, each at Info, and each pinned as a constant for the
// reason the turn lines are (turnStartedLine): an operator and the production check read them
// whole, and the daemon's cost reading counts them (ReadTaskCost, costreading.go), so a message
// reworded or extended at its call site would answer that reading with a part of the answer. What
// differs between the call sites belongs in the attributes, never in the message:
//
//   - taskSeenLine is every seen report that moves the revision the claim records seen (taskSeen):
//     the number and that revision with its epoch, which is where the cost reading's clock of R5's
//     first wait starts;
//   - takeAnsweredLine is every answer to a take: the number, the revision it was asked at, the
//     outcome, the hold a refusal names, and whether it repeats an accepted answer from the record
//     (answerAgain), which a reading counts as no second take;
//   - valueWrittenLine is every write of the claim's value (putValue): the number, the task's
//     origin, whether the claim had finished the last task written to it in the session it runs
//     (Claim.finishedLastTask), that session, the write's epoch and revision, and its cause;
//   - valueDeletedLine is every delete of it (deleteValue), with the number and the cause;
//   - takenBackLine is every take that goes while its task stays pending, with the cause, which
//     is what opens a new time the record owes the number;
//   - taskFinishedLine is every finish of a task (finishTask), naming what finished it.
const (
	taskSeenLine     = "supervise: a task value was reported seen"
	takeAnsweredLine = "supervise: a take was answered"
	valueWrittenLine = "supervise: a task value was written"
	valueDeletedLine = "supervise: a task value was deleted"
	takenBackLine    = "supervise: a task was taken back"
	taskFinishedLine = "supervise: a task was finished"
)

// takeUndecidedLine is a take the daemon failed to decide or record (undecided), at Warn, with the
// number, the revision it was asked at and the failure. It is pinned as the lines above are: an
// operator reads it for a take that had no answer, and the cost reading stops the first wait's
// clock at it, as at a refused take.
const takeUndecidedLine = "supervise: a take could not be decided or recorded; it is refused at its revision, and the value is written again under its number once the task is free to take"

// What finished a task, as its line names it (taskFinishedLine), which the cost reading counts
// the finished numbers by: the turn-outcome report that named its number (reportedTask), or the
// end of its turn with no report after it (finishAtTurnsEnd, and settle and interrupted for a
// turn no end-frame bound watches).
const (
	finishedByReport   = "report"
	finishedByEndFrame = "end-frame"
)

// The causes a take-back is logged under (takenBackLine): a provider error's turn
// (providerErrored), a compaction that took the claim (compactStart), and a retirement and a
// suspension that keep the task (retire, suspended, takeBackUnstarted). The second wait's
// take-back is logged under its wait's name (WaitTurnStart), and a death's or a forced stop's under
// the sentence its own Warn line gives (interrupted).
const (
	takenBackProviderError = "provider-error"
	takenBackCompaction    = "compaction"
	takenBackRetired       = "retired"
	takenBackSuspended     = "suspended"
)

// taking is a take being answered (Machine.Take): the request, what the take's rows decided of it
// — the answer, or the adoption of the working copy the answer waits on — and the failure its
// decision ended in. The take's two events carry it (RequestTake.t, TakeAdopted.t), so a row
// writes what it decided on the take it decides, and Machine.Take returns it from there. done
// closes when the answer is in, so a second take of the same claim that arrives meanwhile waits
// for it and answers from it (dispatch://INFRA-2811).
type taking struct {
	request RequestTake
	answer  TakeAnswer
	adopt   *adoption
	err     error
	done    chan struct{}
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
// let go, because a shim's adoption can take seconds or its whole command timeout, and every other
// event of the claim would wait behind it. Its outcome comes back as an event of its own
// (TakeAdopted), where the generation and the number are fenced again and the take is accepted or
// refused. Every take returns an answer with an outcome, an error, or both where the decision
// failed after its answer (answerTake).
//
// The issue's roles share one working copy, so a take that adopts holds the issue's lock from the
// in-turn check to its answer (Deps.LockIssue): two roles cannot both pass the check and both
// adopt. The lock is taken before the machine's own, and never the other way round, so no machine
// waits on an issue while holding its claim.
//
// A second take of this claim that arrives while one is being answered waits for that answer.
// One of the same number, epoch and revision from the same process — a worker sending again a
// take that had no answer in time — is answered from it, accepted text or refusal alike, and
// starts no second adoption; any other is decided afresh once the first is done, a relaunched
// process's take of the same revision among them.
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
	t := &taking{done: make(chan struct{})}
	request.t = t
	t.request = request
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
	t.err = m.answerTake(ctx, t)
	return t.answer, t.err
}

// sameTake is whether two takes are the one ask: the same number at the same revision of the
// same epoch, from the same process — the same launch generation and session. A relaunched
// process reads the same revision its predecessor asked at, since nothing is written at a
// relaunch, so a take of the new process can meet the old one's still in flight (waiting on the
// issue's lock, or in its own adoption): it is not that ask, and the old one's answer — the
// fence's refusal of a generation the claim has left, or an adoption made for it — is not its
// answer. It waits for the old one and is decided afresh, so what it asked is recorded.
func sameTake(a, b RequestTake) bool {
	return a.Number == b.Number && a.Revision == b.Revision &&
		a.Generation == b.Generation && a.Session == b.Session
}

// answerTake decides the take, makes the adoption it waits on, and leaves its answer in t. A
// decision that fails after the take was answered leaves that answer beside its error: the
// worker's take was answered, and the failure is the rest of the decision's — a retirement the
// refused adoption's charge made that the tree lifecycle refused, a claim write the store did not
// take. That failure is logged here, since the caller answers the worker and has nothing of its
// own to do with it. One that fails before any answer leaves none, and leaves the take refused at
// its revision, so the value is written again for the worker to ask at (undecided).
//
// Every take ends in an answer or in an error. Each row of the take's two events answers it,
// sends it to its adoption, or fails (take, takeInTurn, takeNotReady, adopted, adoptedTooLate),
// and a take that reaches no row fails in Handle: a fence's refusal, or a state that refuses the
// request.
func (m *Machine) answerTake(ctx context.Context, t *taking) error {
	err := m.Handle(ctx, t.request)
	if a := t.adopt; err == nil && a != nil {
		adoptErr := m.adopt(ctx, *a)
		// The answer is the claim's whatever became of ctx: a caller that hung up still leaves the
		// adoption's outcome to record.
		err = m.Handle(context.WithoutCancel(ctx), TakeAdopted{Claim: a.token, Generation: a.generation, Number: a.number, Err: adoptErr, t: t})
	}
	if err != nil && t.answer.Outcome != "" {
		m.log.Warn("supervise: the decision a take was answered in failed after its answer",
			"task", t.request.Number, "outcome", string(t.answer.Outcome), "error", err)
	}
	return err
}

// answer is the answer this decision made to a take: an acceptance it applied, a refusal or a
// drop. Its line says it is no repeat (repeat false), so a reading of the log counts an accepted
// one as a take of its number (ReadTaskCost). Every take row ends here or in answerAgain, so
// every answer is logged once.
func (m *Machine) answer(take RequestTake, answer TakeAnswer) {
	m.answerAs(take, answer, false)
}

// answerAgain is the answer to a take of a number the record holds accepted and unfinished, before
// its turn starts or after: a take sent again after its answer was lost, answered the text the
// record kept and applying nothing (answeredFromRecord). Its line says it repeats (repeat), so a
// reading of the log counts the number taken once for the take that applied it (ReadTaskCost).
func (m *Machine) answerAgain(take RequestTake, answer TakeAnswer) {
	m.answerAs(take, answer, true)
}

// answerAs is answer and answerAgain's one body: it records the answer on the take (taking) and
// writes the take's line (takeAnsweredLine), repeat saying which of the two it is.
func (m *Machine) answerAs(take RequestTake, answer TakeAnswer, repeat bool) {
	take.t.answer = answer
	m.log.Info(takeAnsweredLine, "task", take.Number, "revision", take.Revision.Revision, "outcome", string(answer.Outcome),
		"hold", answer.Hold, "repeat", repeat)
}

// refuseTake answers a take refused by hold, recording the refusal at the take's revision.
func (m *Machine) refuseTake(ctx context.Context, take RequestTake, hold string) error {
	if m.claim.recordTake(take, TakeRefused) {
		if err := m.persist(ctx); err != nil {
			return err
		}
	}
	m.answer(take, TakeAnswer{Outcome: TakeOutcomeRefused, Hold: hold})
	return nil
}

// undecided is a take row's failure before any answer: a read or a write of the record that the
// holds, the drops and the root's narrowing make (takeable), the write of an accepted take
// (accept), the write of the worker's word that a claim a restart restored working is idle
// (takeInTurn). The take route answers such a take with a coded failure (internal/api's
// taskRefusal, CLAIM_WRITE_FAILED), and no worker sends a take again after an answer that carries
// a code: the plugin and the bridge ask again only at a newer revision of the value (departure 7
// of dispatch://INFRA-2811). The record still owes the task, so the take is recorded refused at
// its revision, as a hold's refusal is, and the write a refused take waits on (offerPending)
// writes the value again under its number where the task is free to take: at the claim's next
// chance, the sweep's observation at the latest, once the store answers the reads takeable makes.
// That newer revision is the worker's signal to ask again. Neither of R5's waits charges the
// worker meanwhile: the first waits only on a revision no take came at (firstWaitNow), and no
// take stands accepted for the second (secondWaitNow).
//
// A row that answered before it failed is no such take (answerTake), and neither is a take
// of a number the claim no longer holds pending and untaken, nor one at a revision the worker has
// since replaced (recordTake). A refusal records itself before its write (refuseTake), so a
// failure there leaves the take refused already, and the hold's end writes the value. The
// machine remembers the number (Machine.rewrite), so the write is logged under the cause it is
// (TaskValueTakeFailed).
//
// The refusal is written with the claim, unless a claim write is owed already: the failed write
// that owes it — the accept's, or takeInTurn's — is made again from memory, refusal included, by
// this decision's own settle (accept leaves the delivery unsaved) or the next decision's repair,
// so a write here would only wait out a second claim-write deadline under the issue's lock.
func (m *Machine) undecided(ctx context.Context, request RequestTake, err error) error {
	if err == nil || request.t.answer.Outcome != "" {
		return err
	}
	p := m.claim.Pending
	if p == nil || p.Number != request.Number || p.taken() || !m.claim.recordTake(request, TakeRefused) {
		return err
	}
	m.rewrite = valueRewrite{number: p.Number, cause: TaskValueTakeFailed}
	m.log.Warn(takeUndecidedLine, "task", p.Number, "revision", request.Revision.Revision, "error", err)
	if m.owedWrite != nil {
		return err
	}
	return errors.Join(err, m.persist(ctx))
}

// recordTake records what came of a take at its revision (Claim.TaskTakeState), and reports
// whether it changed the claim. A take at a revision newer than the one the worker last reported
// seeing is the worker's word that it saw that one, so it is recorded as seen too; one at an older
// revision of the same epoch is an ask the worker has since replaced, and records nothing.
func (c *Claim) recordTake(take RequestTake, state TakeState) bool {
	switch {
	case take.Revision.after(c.TaskSeenRevision):
		c.TaskSeenRevision = take.Revision
	case take.Revision.before(c.TaskSeenRevision):
		return false
	}
	if c.TaskTakeState == state {
		return false
	}
	c.TaskTakeState = state
	return true
}

// take is a take reaching a claim that is ready or idle, the one state a worker asks from. It
// runs the holds, the three drops and the root's narrowing, in that order (takeable), and then the
// adoption of the working copy, which goes back to Machine.Take to make; a take that adopts
// nothing is accepted here.
//
// Four takes are answered before any of that, from the record alone. A number the claim's record
// does not owe it — no task pending, another one pending — is refused not-owed and records
// nothing. So is a number whose take's report came and whose finish the store refused (takePhase,
// waitFinish): no run of the task is owed, and handed its text again — by the same process
// sending its take again after a lost answer, or by a process relaunched after a let-go — the
// worker would run the task again; it charges nobody, and the end-frame bound's finish deletes
// the value. A number the claim holds as its accepted, unfinished take, before the take's turn
// starts or after, is a take sent again after its answer was lost: it is answered the same text,
// which the record kept (Delivery.TakenText), and applies nothing a second time — no adoption,
// no write, no telling. A run that started meanwhile and was given to the take (turnStarted) may
// be a run no take started, and the worker that asks again holds no text to run, so a refusal
// in-turn there would leave the text never run (dispatch://INFRA-2811); the report of that run,
// which names no task, then gives the take back its turn (notTheTakesTurn). And a number taken
// whose take kept no text — a take a migration filled from an earlier release, which answered
// none — is refused in-turn.
func take(m *Machine, ctx context.Context, ev Event) error {
	request := ev.(RequestTake)
	if m.answeredFromRecord(request) {
		return nil
	}
	return m.undecided(ctx, request, m.takePending(ctx, request))
}

// answeredFromRecord answers the four takes the record alone decides (take), and reports whether
// it answered this one.
func (m *Machine) answeredFromRecord(request RequestTake) bool {
	p := m.claim.Pending
	switch {
	case request.Number == 0 || p == nil || p.Number != request.Number || m.takePhase() == waitFinish:
		m.answer(request, TakeAnswer{Outcome: TakeOutcomeRefused, Hold: HoldNotOwed})
	case p.taken() && p.TakenText != "":
		m.answerAgain(request, TakeAnswer{Outcome: TakeOutcomeAccepted, Text: p.TakenText})
	case p.taken():
		m.answer(request, TakeAnswer{Outcome: TakeOutcomeRefused, Hold: HoldInTurn})
	default:
		return false
	}
	return true
}

// takePending decides a take of the pending task, which no take has been accepted for: a hold or
// a drop answers it here, an adoption sends it back to Machine.Take, and anything else accepts
// it. Its contract is that the caller holds such a task, which every caller has asked first
// (answeredFromRecord answers a take of any other, no task pending among them): it answers the
// take on every path, so a nil check returning nothing would leave the worker unanswered and hide
// the caller's fault.
func (m *Machine) takePending(ctx context.Context, request RequestTake) error {
	m.claim.recordTake(request, TakeMade)
	hold, dropped, err := m.takeable(ctx, false)
	switch {
	case err != nil:
		return err
	case dropped:
		m.claim.recordTake(request, TakeRefused)
		if err := m.persist(ctx); err != nil {
			return err
		}
		m.answer(request, TakeAnswer{Outcome: TakeOutcomeDropped})
		return nil
	case hold != "":
		return m.refuseTake(ctx, request, hold)
	}
	p := m.claim.Pending
	if m.adopts(*p) {
		request.t.adopt = &adoption{
			number: p.Number, generation: m.claim.Generation, delivery: p.ID, phase: p.Phase,
			token: m.claim.Token, issue: m.claim.Issue, role: m.claim.Role, loc: m.claim.Locator,
		}
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
	if m.answeredFromRecord(request) {
		return nil
	}
	if !m.askFirst {
		return m.refuseTake(ctx, request, HoldInTurn)
	}
	m.log.Info("supervise: a take reached a claim a restart restored working; the worker's take says its host is idle, so the turn the store recorded is over",
		"task", request.Number)
	m.claim.State = StateIdle
	m.askFirst = false
	err := m.persist(ctx)
	if err == nil {
		err = m.takePending(ctx, request)
	}
	return m.undecided(ctx, request, err)
}

// takeNotReady is a take reaching a claim whose agent has not said it is ready: refused
// not-ready, and written again at the ready (finishReady, offerPending).
func takeNotReady(m *Machine, ctx context.Context, ev Event) error {
	request := ev.(RequestTake)
	if m.answeredFromRecord(request) {
		return nil
	}
	return m.refuseTake(ctx, request, HoldNotReady)
}

// adopted is the working copy's adoption a take made, back with its outcome. The claim may have
// moved while the adoption ran with the lock let go — a death, a suspension, a newer task — so
// the take is decided again against the claim as it stands: a task that is no longer the one
// the adoption was made for is refused not-owed, a hold that stands now refuses it by name, a
// failed adoption refuses it with the adoption's own hold, and anything else accepts it.
//
// A failed adoption is recorded on the delivery and charged where the shim refused it
// (refusedAdoption).
func adopted(m *Machine, ctx context.Context, ev Event) error {
	a := ev.(TakeAdopted)
	request := a.t.request
	p := m.claim.Pending
	if a.Generation != m.claim.Generation || p == nil || p.Number != a.Number || p.taken() {
		m.answer(request, TakeAnswer{Outcome: TakeOutcomeRefused, Hold: HoldNotOwed})
		return nil
	}
	if a.Err != nil {
		return m.refusedAdoption(ctx, request, a.Err)
	}
	if hold := m.claimHold(); hold != "" {
		return m.refuseTake(ctx, request, hold)
	}
	return m.undecided(ctx, request, m.accept(ctx, request))
}

// adoptedTooLate is an adoption's outcome reaching a claim that is no longer ready or idle: the
// take is refused by the state the claim is in now, whatever the adoption did (claimHold, which
// names a hold for every such state).
func adoptedTooLate(m *Machine, ctx context.Context, ev Event) error {
	a := ev.(TakeAdopted)
	request := a.t.request
	hold := m.claimHold()
	if a.Generation != m.claim.Generation {
		m.answer(request, TakeAnswer{Outcome: TakeOutcomeRefused, Hold: hold})
		return nil
	}
	return m.refuseTake(ctx, request, hold)
}

// refusedAdoption is a take whose adoption of the working copy failed. It is recorded on the
// delivery (Delivery.SendFailures and its two companions), and only some failures cost budget: a
// refused adoption is the shim's own answer about this workspace and is charged (chargePrompt), so
// it cannot repeat without bound — the third charge retires the process — while a role identity
// the daemon could not mint and a transport that lost the call are no answer at all and charge
// nothing. The take is refused with the adoption's hold either way; the value is written again
// once no hold stands, at the next sweep, hello or ready (offerPending).
//
// The text is the far side's own — a refused adoption carries the wedged command's whole stderr —
// so the delivery holds it, in memory as in the store, as the daemon keeps any far side's error
// text (storetext.FarSideError), a text the store takes whatever the far side wrote: the text the
// failure's line carries, which logFarSideError writes and returns, so the line, the record and
// the charge's own sentence carry one text. That is why the line is chosen before the record is
// written. The whole text follows the line under a message of its own (logFarSideError).
//
// The record's write is carried past rather than returned when it fails, so the charge is always
// made (the delivery is left unsaved, and settle writes it again).
func (m *Machine) refusedAdoption(ctx context.Context, request RequestTake, err error) error {
	p := m.claim.Pending
	// The line that names this failure, and for a charged one the words its charge puts before
	// the far side's text ("" for a failure charged nothing).
	msg, charge := "supervise: the adoption of the working copy was lost to the transport; the take is refused and the task waits", ""
	switch {
	case errors.Is(err, runtime.ErrAdoptRefused):
		msg, charge = "supervise: the shim refused to adopt the working copy; the take is refused", "the working copy was not adopted: "
	case errors.Is(err, runtime.ErrIdentityUnavailable):
		msg = "supervise: the role's GitHub App identity could not be minted; the take is refused and the task waits"
	}
	p.SendFailures++
	p.LastSendFailureAt = m.deps.Clock.Now()
	p.LastSendError = m.logFarSideError(ctx, slog.LevelWarn, msg, err, "task", p.Number)
	if err := m.putPending(ctx); err != nil {
		m.log.Warn("supervise: the failed adoption was not recorded on the delivery; settle writes it again and the charge goes on",
			"task", p.Number, "error", err)
	}
	m.claim.recordTake(request, TakeRefused)
	m.answer(request, TakeAnswer{Outcome: TakeOutcomeRefused, Hold: HoldAdoption})
	if charge != "" {
		return m.chargePrompt(ctx, WaitAdoption, charge+p.LastSendError)
	}
	return m.persist(ctx)
}

// accept accepts the take of the pending task: the delivery records the take (ConfirmedAt) and
// the text the take is answered with (TakenText), composed here with the notes the claim's state
// puts in front of the task (Delivery.message) — so a take sent again is answered with exactly
// this text, and after a restart too; the claim records the number taken (TaskTaken) and the take
// at its revision; the record of failed working-copy adoptions is cleared, since this task got
// through; and the records whose sentences this text carries are told. It all goes in one write,
// which is where the store tells the decisions the task names (store's putDelivery), and once
// that write holds the workflow is told the agent took its work (Deps.TaskConfirmed).
//
// This is where the claim's two records of what became of the agent's earlier work — the session
// the claim abandoned (AbandonedSession) and what the last let-go of its process ended
// (EndedWork) — become told, and it is the rule every other mention of their telling points to. A
// record is told by the accepted take whose text carried its sentence to this process
// (Delivery.message answers which it carries), since the worker runs that text as its turn's
// message, and it records the delivery whose take told it (ToldBy), and the take records which it
// told (Delivery.TakeTold). A take later taken back gives that telling back with it, so the next
// accepted take carries the sentence again (giveBackTake) — what this take told and nothing an
// earlier take of the same delivery told, whose id is the same; one a let-go cuts off once its
// turn started keeps it, since that turn ran the text (interrupted). A take sent again and
// answered from the record (answerAgain) marks nothing: the take it repeats marked it. Both
// records are the claim's fields and the mark is the delivery's, so they go in the same write as
// the take, and a restart reads all of them or none. The one record told anywhere else is the
// old session's record of a let-go, which the abandonment of that session ends by no delivery
// (forgetSession, where that rule is written).
//
// A task of the entry the claim's handshake names records its confirmation there too, the first
// one only (Claim.Handshake, EntryHandshake.confirmed): the delivery's own ConfirmedAt goes with
// the delivery when its task is finished, and the handshake is what keeps when the worker took
// that entry's work. The same write carries it, since the claim is half of it.
//
// News of the approved design is told the same way, by the copy this write gives the delivery
// (Delivery.Carried), its credit once the take's turn starts: the news the text answered here
// carries, which is the delivery's News as the decision of this take rendered it from the record
// (currentNews, takeable), since the text is composed from the task that render left beside it.
// The store reads the copy of a taken delivery whose take's turn has started (turn_started) as what
// its audiences took (design_news_told_in), and the finish of the take's turn makes it final
// (Delivery.Ran, finishTask); a take-back ends it with the take (interrupted, clearTake). Only an
// accepted take confirms a task, so no turn of the worker's own credits news its text never
// carried, and a take sent again is answered the text the record kept and credits nothing anew
// (answeredFromRecord).
//
// The prompt budgets are not cleared here: a take resets no count (R5), and they go to zero at
// the start of this take's turn, as at any turn's start or end (turnStarted).
//
// A take accepted while the claim's count of its process's turns is not known keeps what it saw,
// in memory, for the report of its own turn to make that count known again (anchorTake,
// learnTurns).
//
// None of it is the claim's until the store holds it. The take is composed on a copy of the
// claim and one of its delivery, which memory takes only once the write lands, as queue's write
// is, and everything else the take does follows that: its lines, the workflow told
// (Deps.TaskConfirmed), the answer. So a write that fails leaves memory holding no part of the
// take: no ConfirmedAt or TakenText, hence no second wait armed (secondWaitNow) and no other role
// held behind it (Claim.InTurn), no telling, no handshake's confirmation, no number taken. The
// worker is answered the failure and never sends that take again, so the take is refused at its
// revision instead (undecided) and the value written again for the worker to ask at. The one
// thing done before the write that no write undoes is the adoption of the working copy a phase
// task made first (adopted): it is the shim's, and harmless to leave, since it hands the working
// copy the identity this same role adopts again at its next take of the task. A write whose
// answer was lost may have committed, so the failure leaves the marks a failed
// putClaimAndPendingOwed leaves: the delivery unsaved, and the claim write owed where the write's
// fate is unknown (owes). The next decision writes both rows from memory over whatever it
// committed, and the store's write of an unconfirmed delivery over a confirmed one holds again
// the decisions that confirmation told (internal/store's putBackARetakenConfirmation).
//
// Its contract is that the caller holds a pending task no take has been accepted for, which every
// caller has asked first (takePending, behind answeredFromRecord; adopted): it answers the take,
// so a nil check returning nothing would leave the worker unanswered and hide the caller's fault.
func (m *Machine) accept(ctx context.Context, request RequestTake) error {
	p := m.claim.Pending
	text, told := p.message(m.claim)
	taken := *p
	taken.ConfirmedAt, taken.TakenText, taken.TurnStarted = m.deps.Clock.Now(), text, false
	taken.SendFailures, taken.LastSendFailureAt, taken.LastSendError = 0, time.Time{}, ""
	taken.credit(taken.News)
	c := m.stored()
	c.Handshake = c.Handshake.confirmed(taken)
	toldAbandoned := told.Abandoned && c.Abandoned.Tells()
	if toldAbandoned {
		c.Abandoned.Told, c.Abandoned.ToldBy = true, taken.ID
	}
	toldEnded := told.Ended && c.EndedWork.Tells()
	if toldEnded {
		c.EndedWork.Told, c.EndedWork.ToldBy = true, taken.ID
	}
	taken.TakeTold = Sentences{Abandoned: toldAbandoned, Ended: toldEnded}
	first := m.claim.TaskTaken != taken.Number
	c.TaskTaken = taken.Number
	c.recordTake(request, TakeAccepted)
	if err := m.saved(m.owes("PutClaimAndDelivery", m.putClaimAndDelivery(ctx, c, taken))); err != nil {
		return err
	}
	*p = taken
	c.Pending = p
	m.claim = c
	m.settleOwedWrite("PutClaimAndDelivery")
	m.anchorTake(first)
	m.restoredTake = false
	m.setTurnUnseen(false)
	m.cutTurnOf = ""
	if toldAbandoned {
		m.log.Info("supervise: an accepted take told the agent of the session its claim abandoned",
			"task", p.Number, "delivery", p.ID, "session", c.Abandoned.Session)
	}
	if toldEnded {
		m.log.Info("supervise: an accepted take told the agent what its relaunch ended",
			"task", p.Number, "delivery", p.ID, "cause", c.EndedWork.Cause, "ended", c.EndedWork.Generation)
	}
	if m.deps.TaskConfirmed != nil {
		m.deps.TaskConfirmed(m.current())
	}
	m.answer(request, TakeAnswer{Outcome: TakeOutcomeAccepted, Text: text})
	return nil
}

// takeable decides whether the pending task, which no take has been accepted for, may be taken
// now: the name of the hold that stands, a drop the record made here, or neither. Both a take
// (takePending) and the hold's end (offerPending) ask it, so a value is written again exactly
// where a take would be accepted. owed says the decision has already asked the record whether it
// owes the task and was told it does (queue's dropUnowed), so the three drops are not asked again.
//
// The claim's own holds come first (claimHold). Then the three drops, which retire the task and
// delete its value (dropUnowed): an ask of an update round that an older daemon queued as a task,
// whichever round it names; the daemon's own delivery of a relaunch's record (Delivery.Relaunch,
// queueRelaunch) whose record no longer tells, since its text is that record's sentence and
// nothing else; and a task whose issue has left the phase it was queued for. Then, for a task that
// carries news of the approved design, that news rendered again from the record, with a task
// queued for its news alone dropped where nothing of it stands now (currentNews), whose drop
// deletes its value too. Then the claim's hold of the session it records (holdRecorded): a claim
// that does not hold it, or whose store does not answer, has its task wait. Then a root
// architect's task of a run, held while its root
// has no architect to hear it and narrowed to what it still names once the root hears
// (currentRootTask), and retired when that leaves it naming nothing. Last, for a task that adopts
// the working copy the issue's roles share, another role of the issue in a turn, or holding an
// accepted take whose turn has not ended (issueInTurn), and then a merge forward of the issue owed
// to a role and not landed (mergeForwardOwed), which refuses the take in-turn whatever role takes,
// the asked role's own included: the merge is a role message and no take, so between two of its
// runs, or before its first run starts, no role of the issue is in a turn, and an adoption then
// would leave an empty commit on the merge in progress. A take that adopts nothing never reads it.
// A read of it that fails decides nothing, and the take is undecided (undecided).
//
// This daemon queues no ask of an update round as a task: it publishes the ask to the role's
// Envoy topic (record.UpdateAsk). A task under an `update:` delivery id (updateTaskPrefix) is one
// an older daemon queued, and a claim restored from the store can hold one. Such a task is
// dropped here and never taken, the round the claim is in now included: its text is the older
// daemon's, and that text can tell the worker that the daemon reads the end of the turn as its
// answer. This daemon reads no end of a turn as an answer: only the worker's ready_for_update
// call answers a round, and a role that read that text before the deploy gets the round's Envoy
// ask as well. A claim that has answered its open round reaches no drop here (answeredOpenRound,
// claimHold): the task waits for the round's end, which drops an ask of that round
// (dropUpdateTask), and the next take after the end drops an ask of any other round here.
//
// A relaunch's record that no longer tells leaves its delivery nothing to say. The claim holds
// one in that state when its session is abandoned before the delivery is taken (forgetSession),
// and a take of it would tell the fresh agent the old session's record in the text it was queued
// with.
//
// A drop that leaves the claim holding nothing while its record of the last let-go still tells of
// running work gives it the daemon's own delivery of that record (queueRelaunch): the ready
// queues that delivery only for a claim that holds nothing then (finishReady), so a held task
// dropped at a take or a hold's end would otherwise leave the sentence waiting for whatever task
// comes next.
func (m *Machine) takeable(ctx context.Context, owed bool) (hold string, dropped bool, err error) {
	if hold := m.claimHold(); hold != "" {
		return hold, false, nil
	}
	if !owed {
		dropped, err = m.dropUnowed(ctx)
		if err != nil {
			return "", dropped, err
		}
	}
	if dropped {
		return "", true, m.queueRelaunch(ctx)
	}
	// A task that carries news of the approved design is rendered again from the record here,
	// where the take is decided, and one queued for its news alone that has nothing left to say is
	// dropped (currentNews): what the accepted take's text carries is what the record owes now, and
	// that is the copy the accept writes (Delivery.Carried).
	says, err := m.currentNews(ctx)
	if err != nil {
		return "", false, err
	}
	if !says {
		return "", true, m.queueRelaunch(ctx)
	}
	p := m.claim.Pending
	// Nothing is taken by a process under a session its claim does not hold (holdRecorded, plan
	// E's t-e-54): a claim the boot could not suspend, its stop refused, keeps its live process, and
	// a task it took would put that process to work under another claim's session, or one no claim
	// holds (review round 12 of #4220). The task waits, untaken and charged nothing, and the next
	// hold's end asks again (offerPending, at every sweep among them), so an operator's settle of
	// the name writes the value for the worker to take. A store that does not answer lets nothing
	// be taken either. For a claim holding its session this asks the store once per session
	// (Machine.heldNames).
	if err := m.holdRecorded(ctx, m.claim.Session, m.claim.SessionFile); err != nil {
		var refusal claim.Refusal
		switch {
		case !errors.As(err, &refusal):
			m.log.Warn("supervise: the store did not answer whether this claim holds its session; the task waits", "task", p.Number, "delivery", p.ID, "error", err)
		case m.unheldDelivery != p.ID:
			m.unheldDelivery = p.ID
			m.log.Error("supervise: this claim records a session it does not hold, so no task is taken by its process; the task waits until the claim holds it: an operator gives the claim a new session, or records it as the rightful holder of a name held for none",
				"task", p.Number, "delivery", p.ID, "session", m.claim.Session, "error", err)
		}
		return HoldSessionNotHeld, false, nil
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
		if err != nil {
			return "", !taken, err
		}
		if !taken {
			return "", true, m.queueRelaunch(ctx)
		}
	}
	if other, inTurn := m.adoptionWaits(); inTurn {
		m.log.Info("supervise: a claim of this issue is in a turn; the phase task waits rather than adopt the working copy they share",
			"task", p.Number, "issue", m.claim.Issue, "inTurn", other)
		return HoldIssueInTurn, false, nil
	}
	if m.adopts(*p) {
		asked, merging, err := m.mergeForwardOwed(ctx)
		if err != nil {
			return "", false, err
		}
		if merging {
			m.log.Info("supervise: a merge forward of this issue is owed and has not landed; the phase task waits rather than adopt the working copy the merge writes",
				"task", p.Number, "issue", m.claim.Issue, "asked", asked)
			return HoldInTurn, false, nil
		}
	}
	return "", false, nil
}

// dropUnowed drops the pending task, which no take has been accepted for, where the record no
// longer owes it, and reports whether it did: an ask of an update round that an older daemon
// queued as a task, the daemon's own delivery of a relaunch's record whose record no longer
// tells, and a task whose issue has left the phase it was queued for (phaseHolds, a read of the
// issue's record in a transaction of its own). The task is retired and its value deleted
// (dropPending), under the line that says which of the three it was. Every place that asks it
// gets the record's one answer: a take or a hold's end (takeable), once no hold of the claim's
// stands, which then gives the claim the daemon's own delivery of a record that still tells
// (queueRelaunch); a newer task that arrives behind the task (queue), whatever holds stand and
// before any other case of a held task, which then takes its place and carries that sentence
// itself, and where nothing was dropped hands that answer to the hold's end its refusal makes
// (offer), so that decision asks the record once; and an operator's task over a launch the pacing
// parked above the claim's process (runParkedLaunchNow), which asks before the row's queue does,
// so that the launch runs for the operator's task where queue would drop the task it arrived
// behind. A read of the record that fails drops nothing.
func (m *Machine) dropUnowed(ctx context.Context) (bool, error) {
	p := m.claim.Pending
	if p == nil {
		return false, nil
	}
	round, asks := updateRoundOf(p.ID)
	spent := p.Relaunch() && !m.claim.EndedWork.Tells()
	if !asks && !spent {
		if holds, err := m.phaseHolds(ctx, *p); err != nil || holds {
			return false, err
		}
	}
	switch {
	case asks:
		m.log.Info("supervise: this task is an ask an older daemon queued for an update round; it is dropped and never taken",
			"task", p.Number, "delivery", p.ID, "asked", round, "round", m.claim.UpdateRound, "left", m.claim.UpdateLeftRound)
	case spent:
		m.log.Info("supervise: the daemon's own delivery of a relaunch's record has nothing to say, since that record no longer tells; it is dropped rather than taken",
			"task", p.Number, "delivery", p.ID, "cause", m.claim.EndedWork.Cause, "ended", m.claim.EndedWork.Generation,
			"toldBy", m.claim.EndedWork.ToldBy)
	default:
		m.log.Info("supervise: the issue has left the phase this task was queued for; it is dropped rather than taken",
			"task", p.Number, "delivery", p.ID, "issue", m.claim.Issue, "queuedFor", p.Phase)
	}
	return true, m.dropPending(ctx, valueDeletedDropped)
}

// claimHold is the hold the claim's own state puts on a take of its pending task, "" for none:
// the project's pause holding the claim, the claim not ready or idle, a suspension held over it, a
// launch the wake pacing holds over its process, an update round it has answered, a plugin stale
// enough that its every `legion` call is refused, and the wait a provider error armed for this
// task. It reads the claim and its memory alone, so it is asked again after an adoption that ran
// with the lock let go (adopted).
//
// Five of them hold the task instead of dropping it, each for a reason of its own: while the
// project is paused no task is taken by any role but the controller (decision 8 of
// dispatch://INFRA-3759's plan), so the task stays pending, decided nothing about and dropped for
// nothing, and the resume's event writes its value again once the pause is over (resumePause,
// offerPending); a worker that answered its round is about to lose its process, and so is one
// whose launch waits its turn, and a task started there would be retired as served at a turn it
// never finished; a stale plugin's every call is refused, so a task taken there buys a model turn
// nothing can come of and puts the claim in a turn, which postpones the relaunch it is waiting
// for; and the provider-error wait is the task's own ladder (providererror.go).
func (m *Machine) claimHold() string {
	if _, holds := m.pauseHolds(); holds {
		return HoldProjectPaused
	}
	return m.stateHold()
}

// stateHold is claimHold beneath the project's pause: the hold the claim's own state and memory put
// on a take of its task, whether or not the project is paused. R5's second wait reads it where
// work waits behind a take a report put back (secondWaitNow): the pause holds the take, and the
// pause's own stop can be that work.
func (m *Machine) stateHold() string {
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
