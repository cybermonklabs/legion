package supervise

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/reviewround"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// CheckLook names one look of one production check: the look's number and the check instance the
// look belongs to (record.ProductionCheck.OpenedAt, its id), as the start row that opens the look
// carries them (record.SuperviseRequest.CheckLook and CheckOpenedAt). The two travel together
// because a later merge of the same issue replaces the check and counts its looks from one again,
// so a look number alone names no look. It is comparable, so the one rule that picks what a claim
// is serving reads it as it reads the run and the entry (serving). The zero value is a task,
// or a claim, that is no look's: every phase task, an operator's own, a resume.
type CheckLook struct {
	Look     int
	OpenedAt time.Time
}

// Delivery is a claim's pending task: at most one per claim, persisted, and handed to the claim's
// worker as a value it takes (take.go).
//
// Number is the task number the record queued it under, and the value the bucket holds for the
// claim names it. QueuedAt is when the task was queued; ConfirmedAt is when a take of it was
// accepted, TakenText the text that take was answered with, and TurnStarted whether the take's
// turn has started. The text stays for as long as the take does, through its turn, so a take of
// the number sent again after its answer was lost is answered it whenever it comes. So the three
// readings of a pending task are: ConfirmedAt zero, a task no take holds — queued, or taken back
// under its number; ConfirmedAt set and TurnStarted false, a take accepted whose turn has not
// started (awaitsStart); ConfirmedAt set and TurnStarted true, a take whose turn has started and
// which nothing has finished yet (startRecorded). TakenText is the text alone and says nothing of
// the turn: a confirmed take with no text is one whose text no record kept, which the migration
// that added the column filled its confirmed rows with, and that migration set those rows'
// TurnStarted (turn_started = confirmed_at is not null) before any other fill.
// A task lives until it is finished — by the turn-outcome report that names its number, or, for a
// plugin below TaskTakeContract or a finish the store refused, by the end of its turn with no
// report after it (finishTask, TakesNoTask) — or until the record drops it.
//
// The id is the daemon's and never changes: an outbox delivery id, an operator's minted one, or
// the round an update round's ask is of (updateTaskPrefix, updateRoundOf), which is what names
// that ask's round.
//
// Phase is the issue phase the task was queued for, which is what says whether the task is still
// the work to do: it is carried here, persisted, because the task text is the workflow's to
// write. A delivery of no phase — an operator's own, an architect's — is never dropped for the
// phase its issue reaches, and a suspension does not take it either: it waits the suspension out
// and is taken after the resume (settle).
type Delivery struct {
	ID   string
	Task string
	// Generation is the issue generation this task belongs to, from the start row that queued it.
	// A start delivers to whatever process the claim already has — a live worker is not
	// relaunched — so the pane's own environment names the run it was launched for, which may be
	// an earlier one. The work a worker reports is the task it was given, and this is that task's
	// run. Zero for a task of no run, such as an operator's own; a root architect's start carries
	// its root's generation, as a phase worker's start carries its issue's.
	Generation uint64
	// Entry is the phase entry this task is of, from the start row that queued it: the number the
	// transition raised on the role's phases row when that entry handed this role work, which
	// stands on the row beside the task itself. It is what tells one phase entry from the next
	// within a run, so a turn that outlived its phase cannot complete the phase's next entry
	// (api/handoff.go). Zero for a task of no entry: an operator's own, every start of no entry
	// (record.SuperviseRequest.Entry, which lists them), and every task queued before the daemon
	// was upgraded to number them. The promotion's resume of a mid-phase worker is no such task:
	// it carries the entry it raised (workflow.Engine.StartPromotedWorker).
	Entry int64
	// Number is the task number this task was queued under (Claim.TaskWritten), counting from 1,
	// and 0 for a task of none. A take names the number it takes, so a take of any other number is
	// no take of this task.
	Number uint64
	// Origin is the source that queued this task — phase-start, resume, silence, wake, notice, news,
	// operator, check-look or controller-start — and "" for a task queued before the record kept
	// one (migration 20261113175649_task_numbers). An update round queues no task, so it names no
	// origin: its ask is the sweep's Envoy message (record.UpdateAsk).
	Origin string
	// TakenText is the text the daemon's accepted take of this task was answered with, and ""
	// while no take of it stands accepted, or for a confirmed take whose text no record kept (see
	// above): it is never read for where the take stands, which TurnStarted says. A take of the
	// same number sent again after its answer was lost is answered this same text, before the
	// take's turn starts and after, across a daemon restart too, so it is persisted, and it goes
	// only with the take (interrupted, clearTake).
	TakenText string
	// TurnStarted is whether the turn of the accepted take has started (startTakesTurn): false
	// while no take stands accepted or the accepted take awaits its turn. A turn whose report then
	// says it was not this task's — it names no task, or another — sets it back, and the take
	// awaits its turn again (notTheTakesTurn).
	TurnStarted bool
	// TakeTold is which of the claim's two records the accepted take of this task marked told
	// (accept): the ones a give-back of that take untells (untell), and none while no take of it
	// stands accepted. It goes with the take (interrupted, clearTake), so a later take of the
	// same task, whose delivery id is the same and which carried nothing, gives back nothing an
	// earlier take told: a take cut off once its turn started keeps what it told on the claim's
	// records, since that turn ran its text, and its mark goes with it. It is persisted with the
	// take, so a daemon restarted between a take and its give-back gives back what that take
	// told and nothing else; the records' own ToldBy names the delivery, which every take of the
	// task shares, and cannot say which take told them.
	TakeTold Sentences
	// CheckLook is the look and check instance this task gives the worker. ServingCheck uses it
	// to attribute an explicitly named check report to the look the worker took, not to choose
	// between a report and a phase completion. Phase tasks, operator tasks and resumes carry zero.
	CheckLook CheckLook
	Phase     phase.Phase
	QueuedAt  time.Time
	// ConfirmedAt is when a take of this task was accepted (accept), and the zero time for a task
	// no take holds: one still queued, or one taken back under its number. It is what the store
	// tells the decisions the task names on (store's putDelivery).
	ConfirmedAt time.Time
	// Interrupted says a turn of the task was running when the process the claim recorded was let
	// go (interrupted) — a death, a displacement, a repoint, a stall, a stale plugin, a held
	// suspension's stop — so the task's next accepted take carries the sentence that says so in
	// front of it: the claim's untold record of that let-go while it tells, and interruptedTask
	// after (message). The task text stays as the workflow wrote it.
	Interrupted bool
	// OutOfMemory says the end that cut a turn of this task off was its pod running out of memory
	// (runtime.Observation.OutOfMemory), so the task's next accepted take carries outOfMemoryTask
	// as well as the sentence of its interruption (message). The task text stays as the workflow
	// wrote it.
	OutOfMemory bool
	// SendFailures, LastSendFailureAt and LastSendError are the diagnosis record of this task's
	// takes whose adoption of the working copy failed (refusedAdoption): how many of them failed,
	// when the last one failed by the machine's clock, and the error it failed with. They are a
	// record of what happened, not a budget. The budget is the claim's
	// (Claim.Budgets.PromptFailures and PromptRetires), which, of the adoptions that fail, only a
	// refused one — the shim's answer about this workspace — spends (the two take waits charge it
	// too, chargePrompt); this counts every adoption that failed, charged or
	// not, and that difference is the point of recording it. An adoption lost to the transport is
	// charged nothing and waits for the next take, so a claim can sit with its budget untouched and
	// nothing on it saying why, and that is the case an operator most needs named (INFRA-1480: two
	// planning tasks held 31 and 33 hours, whose only record of each failed send was a log line
	// inside the pod). All three are written together on every adoption that fails, and cleared
	// together where a take is accepted (accept), so a task that got through carries zeroes.
	// LastSendError is the far side's text as the daemon keeps any far side's error text
	// (storetext.FarSideError), set so in memory as well as in the store, and the whole text is in
	// the log (refusedAdoption).
	SendFailures      int
	LastSendFailureAt time.Time
	LastSendError     string
	// Kept is what the task names of the decisions a person made while nobody in the root's tree
	// could hear them, as the row that queued the task recorded them (record.KeptDecisions, encoded
	// JSON). It is opaque here — this package knows nothing of the record — and is carried and
	// persisted with the delivery because the delivery is what holds those rows: the store's write
	// of this delivery marks each held by this claim and delivery id, the write that records
	// ConfirmedAt — the accepted take — tells them, and the write that retires it leaves them kept
	// for whichever tree owes them. It is rewritten, with the task text, by the narrowing a take
	// makes of what the task still names of this claim's tree (Deps.RootTask). Empty for a task
	// that names none.
	Kept []byte
	// News is what the task carries of news of the approved design (record.DeliveryNews, encoded
	// JSON): the start's own text, each item it tells and each opening it carries. It is opaque here
	// as Kept is, and carried and persisted with the delivery. It is rewritten, with the task text,
	// at every take decided of the task (takeable), by the one render that writes both
	// (Deps.DesignNews, and Deps.RootTask where a root architect's task is narrowed), so the news a
	// delivery holds is exactly what its text names: no writer keeps one beside a text it did not
	// render with it. Empty for a task that carries none, which no take asks about.
	News []byte
	// Carried is the copy of News that the text of this task's accepted take carried, and, on a
	// taken delivery whose take's turn has started (turn_started), its credit: what its audiences
	// are read as having taken from it (the claim store's design_news_told_in), held by this
	// delivery's row and by nothing else, so the write that takes the take back, or the
	// retirement, ends exactly that credit, whatever has been credited since. News is rendered
	// again at each take, so a take after a take-back can carry more than the one taken back, and
	// each accepted take's copy holds only what its own text said.
	//
	// It is written by the accept itself (accept), in the one write that records the take and the
	// text it was answered with, since the text is composed from News at that moment: a take a hold
	// refused, or whose adoption of the working copy failed, writes nothing, so a task no accepted
	// take carried holds none, and no turn of the worker's own credits anything, since only an
	// accepted take confirms a task. It goes wherever the take goes (interrupted, clearTake), since
	// no turn of that text can start any more. It stays once the take's turn ran and its finish says
	// so, which makes the credit final (Ran).
	Carried []byte
	// Ran says the turn of this task's accepted take ran and was finished with no provider error
	// (finishTask): the worker read what Carried credits, so the credit is final, and the write that
	// records Ran folds it into the told rows (the claim store's settleDesignNews), where it outlives
	// the delivery, before the retirement deletes the row. It is set only on a taken delivery with a
	// copy, and it goes with the copy: every write of Carried writes it false (credit).
	Ran bool
	// NewsOnly says the task was queued for news of the approved design alone
	// (RequestDeliver.NewsOnly, the daemon's news-only start). It names its member's phase and run,
	// so that it is dropped once the issue has left that phase (phaseHolds), but it is no task of
	// that phase: it hands the worker no work of the phase, only the news. So no reader of a
	// claim's pending delivery takes it for the phase's task (StillWorked, HoldsWork), and the start
	// or repair that gives the phase's task still gives it to a claim that holds one. The mark is
	// the delivery's own column (pending_task_deliveries.news_only), written and read beside News,
	// so no writer of the news can drop it.
	NewsOnly bool
	// ProviderErrorRetries is how many times this task has been taken back after a turn of it
	// ended in a provider error, from 0 (providerErrored). It is what the wait before the value is
	// written again is a function of (promptRetryDelay), so a daemon restarted in the middle of a
	// ladder goes on climbing it rather than starting again at the first step. It is the task's
	// own count, not the claim's: a new task starts at 0.
	ProviderErrorRetries int
	// ProviderErrorNote is the sentence such a task's next accepted take carries, as an
	// Interrupted task's carries the sentence of its interruption: what ended the previous turn,
	// and that the daemon gives the task again. It is built from structured fields alone — the
	// HTTP status the provider answered, else nothing more than "a provider error" — because a
	// refusal's own explanation is itself refused when it is sent back
	// (dispatch://INFRA-956/artifact/cause-evidence-md), so no provider text reaches a task's
	// text. Empty for a task no provider error has taken back.
	ProviderErrorNote string
	// RoundNamed is the typed fact that Task ends with the daemon's two sentences of a reviewer's
	// review round (reviewround.Sentences), from the start that queued it (RequestDeliver.RoundNamed):
	// the start row's own mark (record.SuperviseRequest.RoundNamed), or the round the start
	// executor names on a reviewer's start queued for news alone (the daemon's newsForStart). The
	// render of a task that carries news keeps the two last, after the news (the daemon's
	// renderNews). message reads it: a task of the reviewer without it, which a start row of the
	// release before this one, an operator or a delivery the release before stored can be, is taken
	// with the daemon's sentences naming no commit and no entry at its end.
	RoundNamed bool
}

// taken is whether a take of the task was accepted and has not been taken back.
func (d Delivery) taken() bool { return !d.ConfirmedAt.IsZero() }

// awaitsStart is whether a take of the task was accepted and its stored mark of its turn's start
// (TurnStarted) is unset: what the delivery alone says, before a finish a report decided and the
// claim's plugin. Whether the take awaits its turn is the Machine's to say (takeAwaitsTurn, by the
// one rule of where the take stands, takePhase): a take whose report came with its finish refused
// by the store has run its turn, whatever this bit says.
func (d Delivery) awaitsStart() bool { return d.taken() && !d.TurnStarted }

// startRecorded is whether the task's accepted take has its turn's start recorded (TurnStarted):
// the turn is running, or it has ended and nothing has finished the task yet (finishTask).
// Whether the take's turn ran is the Machine's to say (takesTurnRan), which reads a finish a report
// decided as well.
func (d Delivery) startRecorded() bool { return d.taken() && d.TurnStarted }

// TakenWith is whether c's worker took this task: the claim's last accepted take is this task's
// number. It stays true after the task is taken back, since the worker read the task and may be
// carrying on its work (Claim.ServingRun).
func (d Delivery) TakenWith(c Claim) bool { return d.Number != 0 && c.TaskTaken == d.Number }

// credit writes news as the delivery's copy (Carried), not yet final (Ran). It is how every writer
// of the copy writes it — the accepted take (accept), and the two take-backs that drop the copy
// with the take (dropTake: interrupted, clearTake) — so no copy is ever written beside a stale
// mark that it is final.
func (d *Delivery) credit(news []byte) {
	d.Carried, d.Ran = news, false
}

// StillWorked says whether a claim in state will still work this task as the task of generation's
// run in phase p: it is that run's task for that phase, and its turn is still to come — queued, a
// task taken back included, or taken and awaiting its turn — or running in the turn the claim is
// working. A task whose turn started on a claim not working is over: what finishes it is the
// turn's report, or, for a plugin below TaskTakeContract, the end of that turn (finishTask), and
// a take of a plugin that names its number whose report never comes is taken back as a cut turn
// (interrupted), its turn over all the same. A task of news alone (NewsOnly) names the phase and
// the run but is no task of the phase, so it is never the phase's task a claim still works: a
// start that retries a failed claim still gives the phase's task (the daemon's keepsStartTask),
// the silence repair still sees a worker that holds no phase task (workflow's
// unpromptedPhaseWorker), and the rest of the readers of this rule read it the same way.
func (d Delivery) StillWorked(generation uint64, p phase.Phase, state ClaimState) bool {
	return !d.NewsOnly && d.Generation == generation && d.Phase == p && (!d.startRecorded() || state == StateWorking)
}

// SupersededBy is whether a task of the run generation at entry takes d's place in a tree root
// architect's hands by the root architect's rule: d is a task of an earlier run, or of the same run
// at a lower entry, and d carries a run and an entry of its own. A task of no run (generation 0),
// an operator's own and the daemon's delivery of a relaunch's record among them, is superseded by
// nothing here, and so is a task of no entry: a wake's task (workflow's wakeRow), which is the
// sentence that says why the pod came back and is no entry's work, and every task a daemon
// delivered before it numbered entries. A newer task is refused behind such a task while it is
// untaken and the record still owes it (Machine.dropUnowed), as behind any untaken task, and its
// value is written again first (queue, offerPending). Three tasks are the exceptions, each
// replaced by another rule than this one. The two a newer task replaces whatever they carry
// (replaceable): the task a provider error took back while the ladder's wait stands for it, and
// the daemon's own delivery of a relaunch's record while no take of it is accepted. And a task of
// news of the approved design alone (NewsOnly), which carries no entry: the task of any entry takes
// its place, for every role (Machine.supersedes, whose first rule that is), since the start of an
// entry carries that same news itself.
//
// It is the one statement of this comparison, and two readers make it: the claim's machine,
// before it puts a newer task in the place of the one the claim holds untaken
// (Machine.supersedes), and the admission sweep, which writes the start of the entry a root
// architect's phases row names unless the claim holds a task that start would not replace
// (workflow.ArchitectStartPending). The sweep never reads a task of news alone as one that holds
// its start back (supervise.HoldsWork, which its reading of a start on its way goes through), so
// the sweep and the machine agree on which held task a start of the root's run at that entry
// replaces, a task of news alone among them.
func (d Delivery) SupersededBy(generation uint64, entry int64) bool {
	if d.Generation == 0 || d.Entry <= 0 {
		return false
	}
	return d.Generation < generation || (d.Generation == generation && d.Entry < entry)
}

// Sentences is which of the claim's two untold records an accepted take's text carries the
// sentence of: the session the claim abandoned (Abandoned: AbandonedSession, abandonedTask), and
// what the last let-go of its process ended (Ended: EndedWork, relaunchSentence).
// Delivery.message answers it beside the text it composes, so what an accepted take marks told is
// what its text holds (accept), and the delivery keeps what its take marked (Delivery.TakeTold).
type Sentences struct{ Abandoned, Ended bool }

// message is the text an accepted take of a delivery is answered with, and which of the claim's
// two untold records it tells (Sentences): its task, behind the sentences the claim's state gives
// it that say what became of the agent's earlier work, and then the provider's. In order:
//
//   - The first task the agent that replaced an abandoned session takes names that session
//     (abandonedTask), and says that everything of the old session is gone. A record that still
//     tells beside it is one of the fresh session's own processes (forgetSession), and that
//     record's sentence follows the abandoned session's (the next item): the text tells both.
//     What the abandoned session's sentence stands in place of is the old session's turn: the
//     note of an interruption (interruptedTask) and the out-of-memory note behind it, which a
//     task taken back from a turn of the old session's process still carries. A turn of the
//     fresh process that was cut off on this task is the fresh process's own, and the record
//     that tells says so (EndedWork.TaskTurn): the let-go that wrote it took the task back from
//     that turn and wrote its interruption anew (recordEnd, interrupted). So the record's
//     sentence says that turn was interrupted, and the out-of-memory note follows it as it does
//     where no session was abandoned.
//   - While the claim's record of what its last let-go ended is untold (EndedWork.Tells), every
//     accepted take's text carries that record's sentence in front of its task
//     (relaunchSentence): the cause, the turn and the background work that let-go cut off,
//     whatever the task. It also says what interruptedTask would say of a task taken back from
//     that turn — it begins with the turn of this task being interrupted where the record says
//     the claim's task was in it (EndedWork.TaskTurn) and this is the task that let-go took back
//     (Interrupted) — so a task carries the one sentence and not both. In front of any other
//     task, such as the new task of its phase that a retry of the stopped claim (endStopped's
//     comment names each) gives a worker a stop cut off in its task's turn, it says instead that
//     the turn the agent was in was interrupted (EndedWork.inFrontOf).
//   - A task taken back from a turn (Interrupted) whose record has been told, or that no record
//     names, still says its turn was interrupted, under the cause the claim's record names
//     (interruptedTask): the let-go that took it back, whichever path that was, rather than the
//     last death's words. With an abandoned session still to tell, that turn was the old
//     session's, and the abandoned session's sentence stands in its place (the first item).
//
// Composing the text tells nothing: the take it answers marks what it carries told (accept).
//
// Behind the interruption, when its end was the pod running out of memory, comes outOfMemoryTask
// — the agent reads that its turn was interrupted, then why, then the task. A task a provider
// error took back carries its own sentence last of all (ProviderErrorNote), so the agent reads
// what the provider did and then the task; each of these says something the others do not.
//
// The daemon's own delivery of a relaunch's record (Delivery.Relaunch, queueRelaunch) has no task:
// its text is the record's sentence alone (RelaunchSentence), composed from the record as it
// stands when the take is accepted, so a let-go recorded after it was queued is told in it too.
// One whose record no longer tells has nothing to say, and takeable drops it rather than let it
// be taken. The abandoned session's sentence, the out-of-memory note and the provider's go in
// front of that text as they go in front of a task, the abandoned session's without the task's
// instruction, since there is no task to continue (abandonedTask); the record's sentence does
// not, because it is the text.
//
// A task of the reviewer ends with the daemon's two sentences of its review round, the last two
// of each task the reviewer takes (reviewround.Sentences), and this is the one place the text of
// each accepted take of a delivery is composed, so it is where the rule holds for every task,
// whatever queued it. A task that says it ends with them (RoundNamed), as each task the workflow
// queues for the reviewer does (workflow's queuedRow), is taken as it stands. A task of the
// reviewer that does not say so is given them at its end, naming no commit and no entry: a start
// row or a delivery the release before this one stored, whose text can end with words of the same
// shape that no review recorded, or a task an operator wrote. Its reviewer reads the whole change
// and keeps the entry of its round. It is the fact and never the task's text that tells the two
// apart, since two such texts can be the same word for word. The relaunch's record is no task,
// and is taken as it is.
//
// One case still carries a stale note: the previous release's upsert carries no out_of_memory
// column, so if an image rollback takes a marked task back again for another end, the row keeps
// the mark, and a roll-forward carries the note for that end.
func (d Delivery) message(c Claim) (string, Sentences) {
	text, carried := d.Task, Sentences{}
	relaunch := d.Relaunch()
	if relaunch && c.EndedWork.Tells() {
		text, carried.Ended = RelaunchSentence(c.EndedWork), true
	}
	if !relaunch && !d.RoundNamed && c.Role == claim.RoleReviewer {
		text += reviewround.Sentences("", 0)
	}
	var prefix string
	if c.Abandoned.Tells() {
		// The task's instruction ends the last sentence before the task, so the abandoned
		// session's carries it only where no record's sentence follows it.
		prefix, carried.Abandoned = abandonedTask(c, !relaunch && !c.EndedWork.Tells()), true
	}
	// oldTurn is whether a turn this task was taken back from is the abandoned session's, whose
	// sentence stands in place of the notes of that turn: an abandoned session still tells, and
	// no record of the fresh process says the task was in a turn of its own.
	oldTurn := c.Abandoned.Tells() && !(c.EndedWork.Tells() && c.EndedWork.TaskTurn)
	switch {
	case relaunch:
		// Its text is the record's sentence already.
	case c.EndedWork.Tells():
		prefix, carried.Ended = prefix+relaunchSentence(c.EndedWork.inFrontOf(d), true)+"\n\n", true
	case d.Interrupted && !oldTurn:
		prefix += interruptedTask(c.EndedWork.Cause)
	}
	if d.Interrupted && d.OutOfMemory && !oldTurn {
		prefix += outOfMemoryTask
	}
	if d.ProviderErrorNote != "" {
		prefix += d.ProviderErrorNote + "\n\n"
	}
	return prefix + text, carried
}

// HashBootToken is the one hash a boot token is stored and looked up by. A launch mints the token
// and puts its hash on the claim; the shim's hello and the agent's registration present the token,
// and whoever resolves them hashes it here.
func HashBootToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func pendingID(p *Delivery) string {
	if p == nil {
		return ""
	}
	return p.ID
}

// queue gives the claim the task a deliver request carries, with the phase and the entry it
// was queued for, under the next task number the claim's record has (Claim.TaskWritten), and
// writes the claim's value under that number (putValue): the value is the worker's one signal to
// ask. An outbox repeats its row id after a crash before FinishOutbox; the same task and id are
// therefore accepted without changing the persisted delivery, its number or its value.
//
// A request whose entry the claim has already been queued a task of — an Entry at or below
// Claim.GivenEntry — is accepted and queues nothing, whether that delivery is still pending, was
// taken back under its number, or has been retired: that entry's work was given once, and a second
// row carrying it must not give it again. The rows that carry one entry twice are a row written
// while an older row of the same entry was leased, and a row run again after a failure that
// followed its delivery or a crash before the row finished. GivenEntry is raised here, in the
// transaction that writes the delivery (putClaimAndDelivery), so what the claim was given is
// durable no later than the task itself; a request carrying an entry goes through that
// transaction whether or not it names an outbox row. The claim's handshake of that entry
// (Claim.Handshake) is written in the same write: a task of another entry starts it afresh at this
// delivery's queued time, and a second task of the entry it names keeps the time the first was
// queued at (EntryHandshake.queued).
//
// One request carrying an entry is not decided by it: one marked a resend
// (RequestDeliver.ResendEntry), which is the held phase's retry
// (record.SuperviseRequest.ResendEntry, workflow.Engine.restart). That start hands the role the
// work of the entry its phases row already names, with a task of its own that names the retry's
// reason, so the claim was given that entry and must still take the task. The entry cannot tell
// that case from the one above it: a second row of the same entry written while the first was
// leased must queue nothing, and this one must queue its task, and the two rows are alike in
// everything the claim can read. Only the writer of the row knows which it wrote, so it says so
// on the payload, and such a request is decided by its outbox row alone — the guard that still
// stops a retry of the retry's own row from giving the task twice.
//
// A request that carries no entry is decided by the outbox row it came from: a row the claim has
// already queued a task from (Claim.DeliveredRows) is accepted and queues nothing, for the same
// reason, and a request from a row no task came from writes the row with its delivery, in that
// same transaction. Every delivery of a start that carries no entry rests on that rule
// (record.SuperviseRequest.Entry, which lists those starts; the tree architect's, which carries
// a person's recorded decisions, is one of them), so the row-id guard is what keeps one of them
// reaching one claim once, rather than a leftover of what the entry replaced.
// Only that row's own retry is refused this way: the supervise rows of one issue do not run in
// row order (record's outboxInOrder), so an older row the claim has never heard from still gives
// its task.
//
// A request a pending task refuses offers that task first (offerPending), so a task held back
// because its root had no architect to hear it never wedges the claim. A start that carries a
// newer entry's task supersedes a held task that carries an entry of its own, of an earlier run
// or of a lower entry of the same run (supersedes), which is retired untaken. Every other start
// behind an untaken held task is refused, retries on its backoff and offers the held task first —
// its value is written again and the worker takes it: a start of no entry, a resend of the held
// task's own entry, and any start behind a held task of no entry, a wake's among them (workflow's
// wakeRow). That refusal is over once the held task is finished.
//
// A pending delivery whose task the worker has already taken refuses nothing: the take accepted
// it (accept writes ConfirmedAt), so there is no task left to give and the delivery is only the
// record of the take and its turn. Such a delivery is retired here and the newer task queued in
// its place, its value written over the taken one's. Refusing it instead is what left three
// supervise rows of the live daemon answered "a delivery is already pending" with nothing ever
// sent (dispatch://INFRA-1352, INFRA-1443, INFRA-1519): the phase's own start was the newer
// task, and the worker that would have taken it was holding a delivery it had finished with. The
// newer task is not taken into the running turn — a take into a turn is refused — so it is taken
// at the next point the worker's host is idle, once, and no turn is interrupted or started twice.
//
// An untaken pending delivery still refuses a second one, as it always has: that task has reached
// nobody, so accepting over it would lose a task rather than save one. Four untaken deliveries are
// not refused. Two of them are replaceable. One is the task a provider error took back, while the
// ladder's wait stands for it and nothing works it. The next step of the ladder can be ten minutes
// away and a run of errors has no end of its own, so a refusal would keep every newer task of the
// claim behind a task the claim owes for as long as the provider keeps failing
// (dispatch://INFRA-956 R2: a newer task replaces the owed one). The newer task takes its place:
// it is work for this claim, queued since, and R2 has the newer of the two go. The other is the
// daemon's own delivery of a relaunch's record (queueRelaunch) while no take of it is accepted: it
// holds no work of the claim's, only the sentence, and the newer task carries that same sentence
// in front of itself while the record is untold (message), so nothing is lost and the phase
// worker's resume task the ready queued is not held behind it. Once a take of it is accepted,
// its turn is the session's next, and the newer task waits behind it as behind any task its
// worker has taken (awaitsStart).
//
// The third is a tree's root architect's task of a run that a task of a newer entry supersedes
// (supersedes): the pending task carries a run and an entry and is of an earlier run, or of the
// same run at a lower entry, and the request carries a run and an entry of its own. Each task the
// architect is given says what is true of its root (dispatch://INFRA-2614 R3), and the transition
// that raised the newer entry wrote that entry's task (R4), so the older text is no longer what
// the daemon owes. In the case that was measured, the older task told a re-admitted architect
// that its tree was open at the generation the re-admission ended, that every child was still to
// be filed and that no message was coming, while the tree's children ran. A refusal leaves the
// older task to be taken at the next ready, so the architect's first turn goes to a run that is
// over. So the newer task takes its place, and only while nothing of the older task is in the
// agent's hands: no take of it accepted, the condition replaceable reads for the relaunch's
// record. Once a take of it is accepted, its turn is the session's next, and the newer task waits
// behind it as behind any task its worker has taken. The start that carries the newer task does
// not offer the older one first either (StartedBy): a task held while its root had no architect
// to hear it would otherwise be taken at the very start whose own task supersedes it.
//
// That rule is the root architect's alone. A phase worker that holds a task of another run is
// still refused and its start retried, which supervise's
// TestAnUnconfirmedTaskTheRuleDoesNotReachStillRefusesANewerOne pins for a phase worker's claim;
// the daemon's TestARetryOfAClaimThatKeptOtherWorkStillDeliversItsTask (its case "another run")
// starts with no entry, so supersedes answers false before it reads the role and that test pins
// nothing of the scope. Which of a phase worker's two tasks goes first is that worker's model of
// its phase entries, and dispatch://INFRA-2614 changes the architect's rule, not that model. Nor
// does the rule reach a root architect's task of no entry. A wake's task (workflow's wakeRow) is
// one: it says why the architect's pod came back, which no entry's task says, so a newer entry's
// task is refused behind it and the wake's task is offered first, as it was before
// dispatch://INFRA-2614 (TestAnUnconfirmedTaskTheRuleDoesNotReachStillRefusesANewerOne), and the
// entry's task is queued once the wake's turn has started
// (TestAnEntrysTaskRefusedBehindAWakesTaskIsSentOnceTheWakesTurnEnds).
//
// What the older task named of a person's decisions is not lost with it: its retirement releases
// them (the claim store's releaseKeptDecisions), so they are the tree's again, and the catch-up of
// the architect's next ready names them, as does the one the root's start writes for an architect
// it finds running (the daemon's outbox, workflow's claimReady). The daemon's
// TestADecisionASupersededTaskNamedIsToldToTheReadmittedArchitect follows one such decision to the
// catch-up that tells it.
//
// The fourth, read before the other three, is a task the record no longer owes: the task a take
// of it would drop rather than take (dropUnowed, which takeable asks too). Its issue has left the
// phase it was queued for, it is an ask of an update round an older daemon queued, or it is the
// daemon's own delivery of a relaunch's record that no longer tells. Nothing of it is owed, so
// accepting over it loses nothing: it is dropped and its value deleted, under the line a take's
// drop logs, and the newer task is queued in its place. A refusal held the newer task behind it
// until a take dropped it. In the case that was measured, a tester holding its testing task was
// stopped, which took the task back untaken, and the issue then merged and closed; the production
// check's look that started the tester again was refused on every retry, since with no worker to
// take nothing dropped the testing task, and with one the look would wait a take behind a task
// nobody was owed (the daemon's TestAWorkflowStartStartsAStoppedWorkerAgain). The record is asked
// as a take asks it (phaseHolds), in a read of its own that takes no lock and no claim-write
// deadline (write), and a read that fails fails the request, which its row retries. No delivery
// of a relaunch's record follows this drop, as one follows a take's (queueRelaunch): the newer
// task carries the record's sentence in front of itself while the record tells (message).
//
// A fifth, for every role, is a task of news of the approved design alone (Delivery.NewsOnly) that
// a task of an entry supersedes (supersedes). It is a task of no entry, but not of the kind the
// architect's rule leaves in place: a wake's task says something no entry's task says, and
// everything a task of news alone says, the start of an entry carries too (the daemon's
// newsForStart reads the news the record owes the claim when the start runs, and a task no take
// holds holds no credit of it). So the entry's task takes its place while no take of it is
// accepted, and the worker takes its entry's work, with the news, first and in one turn, rather
// than a turn of news and then, after the start's backoff, the work it is owed. A start of no
// entry carries no news, so it is refused behind such a task as behind any untaken task.
//
// A task queued for news of the approved design alone (RequestDeliver.NewsOnly) takes no place of
// a task that holds work of the claim's (HoldsTask), taken or not. A taken task's turn is to come,
// running, or run with its report to come, and a report of a provider error takes the task back to
// the ladder (providerErrored), which it can do only while the task is still pending: a retired
// task's report reaches no take, and the phase's task is lost until a repair finds the silent
// worker. A task a provider error took back is owed work, and the ladder writes its value again.
// So such a request is refused, as one behind an untaken task is, and its start waits on its row's
// backoff until the claim holds no such task: its turn was finished and the task retired. The
// daemon's own delivery of a relaunch's record holds no work, and a task of news alone takes its
// place as any newer task does. A task the record no longer owes is dropped first, as for any
// request.
//
// The replaced task's row ends as any other retirement ends it, on the one path both places a
// task replaces an owed one take (replaceOwedTask): the delete releases what that task held of a
// person's decisions for whichever tree is owed them (the claim store's RetireDelivery and
// releaseKeptDecisions), where a row overwritten in place would leave them held by a delivery id
// that is gone, or move them to a task whose text never named them. The wait ends with the task,
// and the newer task's own value is what the worker is handed next.
//
// What the ladder counted belongs to the delivery it counted for, and goes with it: the attempt
// count and the sentence the next take would have carried are the delivery's own fields
// (ProviderErrorRetries, ProviderErrorNote), so the task that replaces it starts its own ladder
// at the first step. The claim's run of provider-error turns is the claim's and stays
// (Claim.ProviderErrorRun): the provider failed this claim's turns whichever task they worked,
// and only a turn that ends with no error puts that run back to zero.
//
// The claim's record goes with the delivery in one write, since the number it is queued under
// is the record's (Claim.TaskWritten), and so does the epoch and revision of the value written
// for it, which is written first: a value whose write the store then refused names a number the
// record does not owe, and a take of it is refused not-owed, while the retry of the request
// queues the task again under that same number and writes the value again.
//
// Of a task an operator gave the claim, which carries no id of its own and no outbox row
// (operatorDelivery), neither thing accepted above can be true, so the refusal below is exactly
// a claim that holds a pending delivery its worker has not finished with, that the record still
// owes and that no newer task replaces. runParkedLaunchNow reads the same pending delivery before
// it runs a parked launch for such a task, so nothing runs for one this refuses. A task that takes
// the place of an untaken one — dropped as one the record no longer owes, or replaced
// (replaceOwedTask) — runs that launch where it arrives, and drops or retires that delivery itself
// before it does, by the same test and in the same order as here: the launch takes the ladder's
// wait with the claim's timers (letGo), which is what makes the ladder's task replaceable, so the
// claim reaching here then holds no delivery at all. So does a task over a delivery whose take's
// turn has run on a claim in no turn: runParkedLaunchNow retires that delivery as the last case
// below does, before the launch, which would otherwise take it back as a cut turn. Each of these
// is made only once that launch's lifecycle recheck has passed: a refused recheck changes nothing,
// and the operator's task is refused before it reaches here. Over a claim that is working, such a
// delivery is retired below and the launch keeps its turn (runParkedLaunchNow says why).
//
// The task is as the store keeps it: a deliver's request comes through Handle, which gives every
// event's text through the store's rule, and the daemon's own delivery of a relaunch's record
// (queueRelaunch) is written from the claim's record, which holds only text the store keeps. An
// operator's task can hold a NUL, which the request holds as the stored delivery does and a
// restart reads back. So the comparison with the pending task, the delivery written and the text
// a take of it is answered with (accept, Delivery.TakenText) all read the stored text, before a
// restart as after one.
func (m *Machine) queue(ctx context.Context, request RequestDeliver) error {
	id := request.ID
	queued := m.given(request)
	if pending := m.claim.Pending; pending != nil {
		if (id != "" && pending.ID == id && pending.Task == request.Task) || queued {
			return nil
		}
		switch {
		case !pending.taken():
			// A task the record no longer owes is dropped, as a take of it would be, and the
			// newer task is queued in its place.
			dropped, err := m.dropUnowed(ctx)
			if err != nil {
				return err
			}
			if dropped {
				break
			}
			if request.NewsOnly && HoldsTask(pending) {
				// A task of news alone waits behind every task that holds work, the one a provider
				// error took back among them (RequestDeliver.NewsOnly): the held task's value is
				// written again first, as below.
				if err := m.offer(ctx, true); err != nil {
					return err
				}
				return &RefusedError{State: m.claim.State, Request: "deliver", Err: ErrDeliveryPending}
			}
			owed, attempt, relaunch := pending.Number, pending.ProviderErrorRetries, pending.Relaunch()
			replaced, err := m.replaceOwedTask(ctx)
			if err != nil {
				return err
			}
			switch {
			case replaced && relaunch:
				m.log.Info("supervise: a newer task replaces the daemon's delivery of the relaunch's record, which no take holds; it carries the record's sentence in its place",
					"task", pending.Number, "delivery", pending.ID, "newer", id)
			case replaced:
				m.log.Info("supervise: a newer task replaces the one a provider error took back",
					"owed", owed, "attempt", attempt, "delivery", id)
			case m.supersedes(*pending, request):
				// supersedes decided which of its two rules holds; the line names that one.
				why := "supervise: the root architect's untaken task of an earlier run or entry is superseded by the newer entry's task, which takes its place"
				if pending.NewsOnly {
					why = "supervise: the untaken task of news alone gives way to the newer entry's task, which carries that news itself"
				}
				m.log.Info(why,
					"task", pending.Number, "superseded", pending.ID, "supersededGeneration", pending.Generation, "supersededEntry", pending.Entry,
					"delivery", id, "generation", request.Generation, "entry", request.Entry)
				if err := m.retirePending(ctx); err != nil {
					return err
				}
			default:
				// The record was asked above whether it owes the task, and it does.
				if err := m.offer(ctx, true); err != nil {
					return err
				}
				return &RefusedError{State: m.claim.State, Request: "deliver", Err: ErrDeliveryPending}
			}
		case request.NewsOnly && HoldsTask(pending):
			// A task of news alone waits behind a taken task, whatever became of its take's turn
			// (RequestDeliver.NewsOnly): a report of a provider error takes that task back to the
			// ladder only while it is pending.
			return &RefusedError{State: m.claim.State, Request: "deliver", Err: ErrDeliveryPending}
		case m.takeAwaitsTurn():
			// The take is in the worker's hands and its turn has not run: the task is not over, and
			// a newer one waits behind it as behind any task its worker has not done. A take whose
			// report came, its finish owed, has run (takesTurnRan), and is finished below.
			return &RefusedError{State: m.claim.State, Request: "deliver", Err: ErrDeliveryPending}
		default:
			m.log.Info("supervise: the pending task's turn has run, so the newer task takes its place",
				"taken", pending.Number, "delivery", id, "phase", string(request.Phase), "generation", request.Generation)
			if err := m.retirePending(ctx); err != nil {
				return err
			}
		}
	}
	if queued {
		m.log.Info("supervise: the claim was given this work already; not queued again",
			"row", request.Row, "entry", request.Entry, "given", m.claim.GivenEntry, "delivery", id)
		return nil
	}
	if id == "" {
		id = rand.Text()
	}
	d := Delivery{ID: id, Task: request.Task, Phase: request.Phase, Generation: request.Generation, Kept: request.Kept,
		News: request.News, NewsOnly: request.NewsOnly, Entry: request.Entry, CheckLook: request.CheckLook,
		QueuedAt: m.deps.Clock.Now(), RoundNamed: request.RoundNamed, Number: m.claim.TaskWritten + 1, Origin: originOf(request)}
	// The claim records the row, the entry, its handshake and the number only once the store has
	// them with the delivery: a write that failed leaves the row undelivered, the entry ungiven and
	// the number unwritten, so its retry queues the task under the same number.
	stored := m.stored()
	stored.TaskWritten = d.Number
	if revision, written := m.putValue(ctx, d, TaskValueQueued); written {
		stored.TaskWrittenRevision = revision
	}
	if request.Row > 0 {
		stored.DeliveredRows = append(slices.Clone(m.claim.DeliveredRows), request.Row)
	}
	if request.Entry > 0 {
		stored.GivenEntry = request.Entry
		stored.Handshake = m.claim.Handshake.queued(request.Entry, request.Generation, d.QueuedAt)
	}
	if err := m.putClaimAndDelivery(ctx, stored, d); err != nil {
		return err
	}
	m.claim, m.unsaved = stored, false
	m.claim.Pending = &d
	// A task pending over a view that is not whole is bounded by the reconnect deadline, which a
	// close made while the claim held no task left unarmed (connview.go).
	m.armReconnectForTask()
	return nil
}

// given is whether the claim has already been given the work request carries, so queue queues
// nothing for it. A request that carries an entry of its own is decided by that entry, and one
// that carries none — or that resends the entry its role's row already names — by the outbox row
// it came from.
func (m *Machine) given(request RequestDeliver) bool {
	if request.Entry > 0 && !request.ResendEntry {
		return request.Entry <= m.claim.GivenEntry
	}
	return request.Row > 0 && slices.Contains(m.claim.DeliveredRows, request.Row)
}

// supersedes is whether request's task takes the place of p, the task the claim holds pending and
// no take of which is accepted, by one of two rules. Each asks first that request carries an entry
// (RequestDeliver.Entry) and is no work the claim was given already (given, which queues nothing
// and so takes nobody's place).
//
// The first holds for every role: p is a task of news of the approved design alone
// (Delivery.NewsOnly). It holds no work, only the news, and the start of an entry carries that
// same news itself: the start executor reads what the record owes the claim when the start runs
// (the daemon's newsForStart), and p's news is still owed then, since a task no take holds holds
// no credit — the claim store counts only the copy of a taken delivery whose take's turn has
// started (turn_started, design_news_told_in) — and the take renders it again from the record
// (currentNews). So the entry's task goes first and alone, and the work the role is owed never
// waits a turn and a backoff behind news. Kept in front of it, p would also hold a root
// architect's entry out of the first stall arm, which reads only a task of the row's entry
// (admit's architectStalls). A start of no entry carries no news (the daemon's startNewsOf), so p
// is not superseded by it: queue refuses it behind p and writes p's value first, as behind any
// untaken task, and the reconcile tells p's news where no later start of an entry does.
//
// The second is the rule only a tree's root architect is held to (queue): request carries a run,
// is no resend of an entry (RequestDeliver.ResendEntry), and p is a task of a run that is either
// earlier than request's or the same run at a lower entry (Delivery.SupersededBy). A task of no
// run, an operator's own and the daemon's delivery of a relaunch's record among them, is never
// superseded by it, and neither is a task of no entry, a wake's task (workflow's wakeRow) among
// them: queue refuses request behind it, unless it is one a newer task replaces whatever it
// carries (replaceable, asked before this), and StartedBy offers it. A request of no entry
// supersedes nothing. Two places read it, so each rule is decided once: queue, which retires p and
// queues request in its place, and StartedBy, which does not offer p first when the start's own
// task is such a request.
//
// It answers false once a take of p is accepted (taken): that take's turn is the session's next,
// and the text the worker runs in it is p's (replaceable says the same of the relaunch's record).
// A superseded p ends no credit, since it holds none: its retirement removes a row no take holds,
// and the claim store counts as a credit only the copy of a taken delivery whose take's turn has
// started (turn_started, design_news_told_in). So what p carried of the approved design stays owed
// on the told rows, whatever became of p before — never taken, or taken and then taken back — and
// the task that takes its place, or the reconcile, tells it.
func (m *Machine) supersedes(p Delivery, request RequestDeliver) bool {
	if p.taken() {
		return false
	}
	if request.Entry <= 0 || m.given(request) {
		return false
	}
	if p.NewsOnly {
		return true
	}
	if !m.claim.treeRoot() || request.ResendEntry || request.Generation == 0 {
		return false
	}
	return p.SupersededBy(request.Generation, request.Entry)
}

// answeredOpenRound is the hold an open update round puts on its claim: the round is open and
// the worker has answered it (ready_for_update), so that worker has said its work is pushed and
// its process may be replaced at any moment. The answer stands for the round, whatever turn
// starts after it, so the hold does too until the round ends (dispatch://INFRA-2981 R5, R9). A
// take of such a claim's task is refused by it (claimHold).
func (c Claim) answeredOpenRound() bool {
	return c.UpdateRound != "" && c.UpdateSignalledAt != nil
}

// asksRoot says whether a take of a delivery waits on whether the root of this claim's tree has an
// architect to hear it (rootHears): a root architect's task of a run — it names no phase and a
// run, and the claim is its tree's root — on a daemon with a workflow to answer for it.
func (m *Machine) asksRoot(d Delivery) bool {
	return m.deps.RootHears != nil && d.Phase == "" && d.Generation != 0 && m.claim.treeRoot()
}

// rootHears asks whether the root of this claim's tree has an architect to hear the task now
// (Deps.RootHears). Only a root architect's task of a run is put to the answer (asksRoot): it names
// no phase, so the phase check never drops it, and the decisions it names are told at its own
// accepted take, so a take while the tree is parked, lingering or waiting for its slot would tell
// them to an architect no run of the root owns. Every other delivery — a phase worker's, an
// operator's, a production check's look, the controller's start message — is taken as it always
// was sent, and a daemon with no workflow configured supplies no answer, where every take goes.
func (m *Machine) rootHears(ctx context.Context, d Delivery) (bool, error) {
	if !m.asksRoot(d) {
		return true, nil
	}
	hears, err := m.deps.RootHears(ctx, m.claim, d)
	if err != nil {
		return false, fmt.Errorf("read whether the root of %s has an architect to hear its task: %w", m.claim.Token, err)
	}
	return hears, nil
}

// currentRootTask writes the held task back to what it says now (Deps.RootTask) and answers
// whether there is still a task to take. Such a task names decisions this delivery holds, and an
// issue one of them was decided on can have left this claim's tree while the task waited: the tree
// it stands in now owes the person that answer, so the answer drops it from the task, releases
// this delivery's hold on it and writes that tree's architect a catch-up naming it. This is where
// the task stops naming it, because it is the machine that owns the pending delivery: the
// hand-over commits in a transaction of the admission's own, with no lock on this claim, and a
// task rewritten there would be overwritten by the machine's next write of what memory still held.
//
// A task left naming nothing is dropped rather than taken: there is nothing to tell this
// architect, and the start row that queued it is recorded on the claim (Claim.DeliveredRows), so
// no retry gives it again. A task that carries news of the approved design is narrowed in the
// start's own text, which its news follows, and the answer renders both again through the one
// render a take of news goes through, so the news it carries is exactly what its text names and no
// later take renders a released decision from the text the start wrote. The id and the number are
// kept across the rewrite, whatever it changes: it is this delivery, under the row that queued it,
// with less in it than when it was queued, and a take names the number it takes, never a rendering
// of the text. The narrowed task is persisted before it is taken, so a restart reads the same task
// the take answered.
func (m *Machine) currentRootTask(ctx context.Context) (bool, error) {
	p := m.claim.Pending
	if m.deps.RootTask == nil {
		return true, nil
	}
	task, kept, news, err := m.deps.RootTask(ctx, m.claim, *p)
	if err != nil {
		return false, fmt.Errorf("read what the task held for %s says now: %w", m.claim.Token, err)
	}
	if task == p.Task && bytes.Equal(kept, p.Kept) && bytes.Equal(news, p.News) {
		return true, nil
	}
	if task == "" {
		m.log.Info("supervise: everything this held task named is owed to another tree now; it is dropped rather than taken",
			"task", p.Number, "delivery", p.ID, "issue", m.claim.Issue, "run", p.Generation)
		return false, m.dropPending(ctx, valueDeletedDropped)
	}
	if err := m.rewritePending(ctx, task, kept, news); err != nil {
		return false, err
	}
	m.log.Info("supervise: a decision this held task named is owed to another tree now; the task is taken without it",
		"task", p.Number, "delivery", p.ID, "issue", m.claim.Issue, "run", p.Generation)
	return true, nil
}

// rewritePending writes the pending delivery back with task as its text, and kept and news as the
// decisions and the news it carries, then holds that delivery in memory: the one persist-and-swap
// of a held task read again at its take (currentRootTask, currentNews), each of which keeps its own
// rule for when to rewrite and when to drop. Its callers render the task's text and its news
// together, so news is always what task names. The write comes first, so a restart reads the task
// the take that follows answers, and a failed write leaves memory as the record holds it. The id
// and the number stay: a take names the number, and the record answers it with whatever the task
// says at that take (accept), so a rendering needs no id of its own.
func (m *Machine) rewritePending(ctx context.Context, task string, kept, news []byte) error {
	next := *m.claim.Pending
	next.Task, next.Kept, next.News = task, kept, news
	if err := m.saved(m.putDelivery(ctx, next)); err != nil {
		return err
	}
	*m.claim.Pending = next
	return nil
}

// currentNews writes a task that carries news of the approved design back to what it says now
// (Deps.DesignNews) and answers whether it still has anything to say. Such a task was composed
// from the record when the start that queued it ran, and the record can move while it waits: the
// gate it names can close again, a later approval can replace an item it tells, and another
// delivery's take can tell it first. So the text is rendered again here, where the machine owns the
// pending delivery, at every decision of a take of it — a take refused by a hold is rendered again
// at the take after the hold's end — and what the answer no longer carries goes from the delivery
// as well as from the text, so the accepted take carries what the record owes then.
//
// What this renders is what the accepted take carries: the accept composes its text from the task
// this leaves and makes the news this leaves its copy (Delivery.Carried), its credit once the
// take's turn has started, in the one write that records the take. A take accepted is never
// rendered again: a take of its number sent again is answered the text the record kept
// (answeredFromRecord), and the news it carried is that text's. News that changes after the take
// is owed by the told rows' own reading, beside the credit, and a later task carries it.
//
// A task queued for its news alone that is left with nothing to say is not taken, and the answer
// is false: it is dropped through the one exit every drop leaves by (dropPending), which deletes
// its value, and takeable then gives a claim that drop leaves holding nothing the delivery of its
// untold record of the last let-go (queueRelaunch). The rewritten task is persisted before it is
// taken, so a restart reads the task this take answered. A task that carries no news is never
// asked about, and every task holds where nothing answers (Deps.DesignNews nil).
func (m *Machine) currentNews(ctx context.Context) (bool, error) {
	p := m.claim.Pending
	if m.deps.DesignNews == nil || len(p.News) == 0 {
		return true, nil
	}
	task, news, err := m.deps.DesignNews(ctx, m.claim, *p)
	if err != nil {
		return false, fmt.Errorf("read what the news task held for %s says now: %w", m.claim.Token, err)
	}
	if task == "" {
		m.log.Info("supervise: this task was queued for news of the approved design and none of it stands now; it is dropped rather than taken",
			"task", p.Number, "delivery", p.ID, "issue", m.claim.Issue)
		return false, m.dropPending(ctx, valueDeletedDropped)
	}
	if task == p.Task && bytes.Equal(news, p.News) {
		return true, nil
	}
	if err := m.rewritePending(ctx, task, p.Kept, news); err != nil {
		return false, err
	}
	m.log.Info("supervise: the news this task carries was read again from the record; the task is taken as it reads now",
		"task", p.Number, "delivery", p.ID, "issue", m.claim.Issue)
	return true, nil
}

// RetireStoppedTask retires the task this claim holds for phase p of run generation, at an entry
// no later than entry (0 for a stop that names no entry), deletes its value, and answers whether
// it held one. It is the retirement of the task of a phase the workflow stopped under its worker:
// the admission pass's move of a parent into awaiting_children, which holds that phase's start on
// the record and gives it back later on an entry of its own (internal/workflow's StopForChildren
// and ReturnFromChildren). Its one caller is the outbox, which runs the move's `retire_task` row
// (internal/daemon's outbox).
//
// The task goes whether or not a take holds it. Untaken, every take already drops it while the
// issue waits (takeable asks phaseHolds first), but the return puts the issue back in that same
// phase at the same run, where phaseHolds holds it again, and a task held back the whole wait —
// behind a suspension, an answered update round, a paced launch, a stale plugin or a turn that
// outlives the wait — would then be taken before the return's own start is given its slot. Taken,
// a death or a let-go that takes it back would put it in front of the worker again the same way.
// So it goes here, through the one path a newer task retires a task a take holds by
// (retirePending, queue), in every state of the take: accepted with its turn not started, its turn
// running, and its turn over with its report to come. The claim keeps its state, its turn and its
// session: the task is stale, not the agent. The turn such a take starts later, or the turn that
// runs, and the report that ends it are then no task's: the report names a number no take holds
// and finishes nothing (reportedTask). A take whose report came and whose finish the store refused
// is finished by that report instead (makeOwedFinish): the report decided it, and its finish
// deletes the value.
//
// A task of another phase or run, one of a later entry — the return's own start, should it reach
// the claim before this row runs — and a task of no phase (an operator's own, an architect's, the
// daemon's own delivery of a relaunch's record) are not this phase's work and are left where they
// are. Nothing of the task is on its way to the worker here: a take is decided under the lock,
// save for its adoption of the working copy, and the adoption's answer is fenced on the task's
// number (adopted), so a take whose adoption runs while this retires the task is refused as not
// owed.
//
// Work held for the take's turn or its report — a held suspension, a stale-plugin relaunch, a held
// replacement, the operator's held stop — waits for nothing of the task any more, and runs as at
// the report's finish (settleThenAfterTurn): at once over a ready or idle claim, and at the end of
// the turn that runs otherwise. A record of the last let-go that the take told stays told, as where
// a newer task retires a taken one (queue): the worker read the sentence the text carried. A
// record that still tells is handled as at each of takeable's drops: the claim, which holds
// nothing now, is queued the daemon's own delivery of that record and its value is written
// (queueRelaunch), since the task that would have carried the sentence in front of itself is gone.
// That delivery names no phase, so this row never retires it.
func (m *Machine) RetireStoppedTask(ctx context.Context, p phase.Phase, generation uint64, entry int64) (bool, error) {
	m.mu.Lock()
	defer m.unlock()
	d := m.claim.Pending
	if d == nil || d.Phase == "" || d.Phase != p || d.Generation != generation || (entry > 0 && d.Entry > entry) {
		return false, nil
	}
	m.log.Info("supervise: the workflow stopped the phase this task was queued for; it is retired rather than kept",
		"task", d.Number, "delivery", d.ID, "issue", m.claim.Issue, "phase", p, "entry", d.Entry, "taken", d.taken(),
		"turnStarted", d.TurnStarted)
	return true, m.settleThenAfterTurn(ctx, func() error {
		if owed, err := m.makeOwedFinish(ctx); owed {
			return err
		}
		if err := m.dropPending(ctx, valueDeletedStopped); err != nil {
			return err
		}
		return m.queueRelaunch(ctx)
	})
}

// phaseHolds asks whether the issue is still in the phase and run this task was queued for. A delivery
// that names no phase is nobody's to drop — an operator's own, an architect's — and is never put
// to the answer at all, so the exemption this package documents is this package's own rather
// than a promise every implementation has to keep. A daemon with no workflow configured supplies
// no answer, and every delivery holds.
func (m *Machine) phaseHolds(ctx context.Context, d Delivery) (bool, error) {
	if m.deps.PhaseHolds == nil || d.Phase == "" {
		return true, nil
	}
	holds, err := m.deps.PhaseHolds(ctx, m.claim.Issue, d.Phase, d.Generation)
	if err != nil {
		return false, fmt.Errorf("read the phase of %s: %w", m.claim.Token, err)
	}
	return holds, nil
}

// retirePending drops the pending delivery and leaves its value to whoever writes the next one:
// queue, for the task a newer one replaces; replaceOwedTask, for the task a provider error took
// back that a newer one replaces; and runParkedLaunchNow, for the task whose take's turn has run
// that an operator's task replaces before the launch it runs (retireTask, which every end of a
// delivery goes through).
//
// A task whose report's finish the store refused (takePhase, waitFinish) is finished by that
// report instead (makeOwedFinish), whichever turn its take awaits: the report decided it, and
// dropped unfinished the claim would record no finish of it. Its value goes with the finish,
// before the newer task's is written.
func (m *Machine) retirePending(ctx context.Context) error {
	if owed, err := m.makeOwedFinish(ctx); owed {
		return err
	}
	return m.retireTask(ctx, false)
}

// retireTask ends the pending delivery, and is the one place a delivery ends: finishTask for a
// task whose take's turn ran, dropPending for a task the record no longer owes — the task its
// issue's phase has left, an older daemon's ask of an update round, a root architect's task that
// names nothing, the ask the claim retires with, a phase task a suspension ended (settle), and the
// task of a phase the workflow stopped under its worker (RetireStoppedTask) — and retirePending
// for a task a newer one replaces. The claim keeps its state — the task is over, not the agent.
// finished says the task's turn ran, so the claim records it as the last task it finished
// (Claim.TaskFinished) and the session that finished it (TaskFinishedSession), in the same write.
//
// A task of a run whose take's turn ran leaves the run behind it, and its phase entry and its
// check look with it: the worker goes on working it in whatever turn comes next — one an Envoy
// notice starts, with no delivery of its own — and its completion belongs to that run, that entry
// and that look. A take's turn ran where it started, or where a report named the take's number and
// the store refused its finish, which that report decides whichever turn the take awaits
// (takesTurnRan). Only a delivery whose take's turn ran moves them, and only a task that
// carries them: an operator's task carries no generation, no entry and no look, a resume that
// raised no entry and a production check's look no entry, and a phase task no look, and each
// leaves what the claim is serving alone. Every such retire writes the row, the second task of one
// run included: the write is unconditional on the value, not a diff. Which value each takes is
// the claim's own accessors, read here off the claim this delivery is still pending on, so what
// the claim is left serving is by construction what ServingRun, ServingEntry and ServingCheck
// answered while that delivery was pending.
//
// A task of an entry no turn of a take ran goes the other way — untaken, or taken and retired
// before its turn ran, which never ran its text: Claim.GivenEntry is put back to
// Claim.ServedEntry, so the entry stands above what this claim was given again, the admission
// pass reads this role as owed that entry's work (record.PhaseRow.Entry), and the daemon queues
// that entry's task a second time. It is put back only when the retired delivery's Entry is the
// claim's GivenEntry: a retirement of an older task must not lower a number a later delivery has
// already raised. A retired delivery of no entry leaves both numbers where they are, as it leaves
// the run and the look.
//
// The claim goes with the delete, in the store's one transaction, and is durable there rather
// than left for the claim's next write: the wait that follows can be hours long — a tester on CI,
// a merger on a person — and a daemon that crashed between the two writes, or was restarted in
// that wait, rebuilds the machine from the store, where a run left unwritten would refuse the
// completion the worker's next turn reports.
//
// A refusal that says the claim holds no such delivery (ErrNoSuchDelivery) is read as the
// retirement the store has already made rather than as a failure. The store runs the claim write
// and the fenced delete in one transaction, so a delete that removed no row with nothing stored
// under the claim at all is a retirement some earlier call committed — this one, whose answer a
// broken connection lost. Memory moves to match the store, because the delivery is gone there: a
// machine that went on holding it would call this function again at the head of every decision
// (settle) and be refused the same way every time, and each refusal drops that claim's event for
// good (supervisor.decisionFailed in internal/daemon), which is the opposite of what the owed
// write exists to do. The claim write is marked owed all the same, because the same transaction
// rolled it back with the delete, so what the store holds of the claim is older than what this
// retirement wrote; the repair at the claim's next decision is what records it.
//
// The store's other refusal of a delete that removed no row — the claim's row holding a delivery
// under another id (ErrOtherDelivery) — is no retirement already made, and is returned as the
// failure it is. The row holds a delivery this machine is not retiring: it was written by a
// write whose answer the claim-write bound cut off and which committed all the same, so the store
// holds another delivery while memory holds this one. Read as a retirement already made, it
// would drop the delivery memory holds and leave the stored one behind, pending, unconfirmed and
// never retired, with the decisions it keeps never released.
//
// So the decision fails, and the delivery memory holds is marked unsaved on the way out, because
// the refusal says exactly what the row is missing: that delivery. The next settle writes it
// before it retires anything (putClaimAndPending), which puts the row back under memory's own id
// — PutDelivery upserts on the claim token (internal/store's claims.go), so the row the other
// write left is the one it writes over — and the retirement that follows it, where that settle
// retires at all, then finds the delivery it is of. What the other row held of the decisions it
// keeps is held under an id no row carries any more, and a hold whose delivery is not stored
// unconfirmed counts for nothing (keptCounts in internal/record), so the tree's next start or
// catch-up names those decisions again: at worst one is told twice, and none is lost.
//
// Without the mark nothing would ever write the row: this path owes no claim write, so no repair
// makes one, and the settle at the head of every decision from here on would meet the same
// refusal and drop that claim's event (supervisor.decisionFailed in internal/daemon), which is
// the wedge the owed write exists to end. One event is lost, the one whose decision met the
// refusal; the claim decides the next and every one after it.
//
// A retirement that landed is the repair of any claim write the machine owed: the store wrote
// the claim beside the delete, so the row is no longer behind memory (settleOwedWrite).
//
// A project pause's stop the machine owes is written first, through its own transaction, with the
// claim and this delivery as memory holds them now — the repair's own write (putClaimAndPending,
// putStops) — and the retirement after it, as it would have run: the pause's records land with the
// first claim write that lands, and the retirement keeps the store's fenced answers
// (ErrNoSuchDelivery, ErrOtherDelivery) whole. A stop that fails again stops the retirement here,
// with nothing retired, for the next decision's repair and settle to make in that same order.
func (m *Machine) retireTask(ctx context.Context, finished bool) error {
	p := m.claim.Pending
	if p == nil {
		return nil
	}
	if len(m.owedStops()) > 0 {
		if err := m.putClaimAndPending(ctx); err != nil {
			return m.owes("PauseStopped", err)
		}
	}
	retired := m.stored()
	if m.takesTurnRan() {
		retired.ServingGeneration, retired.ServedEntry, retired.ServingCheckLook =
			m.claim.ServingRun(), m.claim.ServingEntry(), m.claim.ServingCheck()
	} else if p.Entry != 0 && p.Entry == m.claim.GivenEntry {
		retired.GivenEntry = retired.ServedEntry
	}
	if finished {
		retired.TaskFinished, retired.TaskFinishedSession = p.Number, m.claim.Session
	}
	// The deaths were charged against this task, so they end with it, in the same write.
	retired.Budgets.Deaths = 0
	err := m.write(ctx, func(ctx context.Context) error {
		return m.deps.Store.RetireDelivery(ctx, retired, p.ID)
	})
	// The row holds a delivery this machine is not retiring, so what it lacks is exactly the
	// delivery memory holds; the mark is what has the next settle write it there.
	if errors.Is(err, ErrOtherDelivery) {
		m.unsaved = true
		return err
	}
	if err != nil && !errors.Is(err, ErrNoSuchDelivery) {
		return err
	}
	// The ladder's wait and the end-frame bound are this task's, so they end with it — once it has
	// ended: a retirement the store refused leaves the task pending, and the end-frame bound is
	// what finishes it later. So does a reconnect deadline armed for it over a closed view
	// (dropReconnectWithTask).
	m.disarm(TimerPromptRetry)
	m.disarm(TimerFinish)
	m.claim, m.unsaved, m.finishOwed = retired, false, false
	m.dropReconnectWithTask()
	if err != nil {
		m.log.Warn("supervise: the store holds no such delivery; the refusal is of a retirement it has already made",
			"delivery", p.ID, "error", err)
		m.oweWrite("RetireDelivery", err)
		return nil
	}
	m.settleOwedWrite("RetireDelivery")
	return nil
}

// saved records the outcome of a write of the pending delivery that memory changed first, since
// the decision it records has been made whether or not the store takes it. A write that failed
// leaves the delivery unsaved, and settle writes it again, with the claim, before the next decision
// - accept's write covers both, and a delivery written taken beside a claim that has not recorded
// the number taken is a take a restart would read wrongly. So once the store takes writes again, a
// restart reads what the machine decided rather than what it replaced; one before that reads the
// older rows, as a restart after a crash between a decision and its write does.
func (m *Machine) saved(err error) error {
	m.unsaved = err != nil
	return err
}

// putPending writes the pending delivery as memory holds it (saved).
func (m *Machine) putPending(ctx context.Context) error {
	return m.saved(m.putDelivery(ctx, *m.claim.Pending))
}

// putClaimAndPendingOwed writes the claim with its pending delivery as memory holds them, for a
// decision that leaves both standing in memory: a write that fails owes the claim write (owes)
// and leaves the delivery unsaved (saved), so the next decision writes both again.
func (m *Machine) putClaimAndPendingOwed(ctx context.Context) error {
	return m.saved(m.owes("PutClaimAndDelivery", m.putClaimAndPending(ctx)))
}

// streaming asks the agent whether a turn is in flight, and answers what it said with the id of
// the ask (runtime.ConnState); answered is false when the agent could not say (the ask failed or
// timed out, Timeouts.RPC bounding it). Its callers are the asks over a connection whose frames the
// machine cannot vouch for: every reconnect hello of a claim holding a pending task (askView) and a
// daemon start's ask over a claim stored ready or idle whose stale-plugin relaunch waits for the
// answer (askRestoredTurn), both of which act on the answer only as the worker stream delivers it
// again behind the frames read before it (StreamTurnAnswered); a daemon start's ask over a claim
// stored working (askTurnOver); and the count of the process's turns a take's report placed,
// which the delivery of the answer to its ask checks (learnTurns, checkPlacedCount).
func (m *Machine) streaming(ctx context.Context, conn runtime.Conn) (state runtime.ConnState, answered bool) {
	asking, cancel := context.WithTimeout(ctx, m.deps.Timeouts.RPC)
	defer cancel()
	state, err := conn.GetState(asking)
	if err != nil {
		m.log.Warn("supervise: the agent could not say whether it is in a turn", "error", err)
		return runtime.ConnState{}, false
	}
	return state, true
}

// adopts is whether a take of d hands the claim's working copy the role's App identity first
// (adopt): a task of an issue phase, which is the work this role commits for. Every other task
// adopts nothing — an operator's own, the controller's start, a resume, an update round's ask —
// because the role is not about to commit for a phase, and the working copy is the issue's rather
// than this claim's: an adoption made for a task that writes no phase commit only takes the
// workspace from whichever role is working in it (dispatch://INFRA-714). A daemon with no GitHub
// Apps (Deps.Identity nil) adopts nothing at all, and neither does the project's controller,
// which works no checkout.
func (m *Machine) adopts(d Delivery) bool {
	return m.deps.Identity != nil && m.claim.Role != claim.RoleController && d.Phase != ""
}

// issueInTurn names another claim of this claim's issue whose agent is in a turn now, or holds an
// accepted take whose turn has not started yet, when there is such a claim. The answer is the
// state those claims' machines keep (Deps.IssueInTurn reads Claim.InTurn of what they publish)
// rather than a second record of who is in a turn kept beside them: the machines' own state is
// the only account that moves with everything that ends a turn — the agent's own end of it, a
// process found dead, a suspension, a close — where a record of turn times would have to be told
// of each of those again.
func (m *Machine) issueInTurn() (claim.Token, bool) {
	if m.deps.IssueInTurn == nil || m.claim.Issue == "" {
		return "", false
	}
	return m.deps.IssueInTurn(m.claim.Issue, m.claim.Token)
}

// adoptionWaits names the claim of this issue in a turn that a take of the pending task waits
// behind (issueInTurn), when there is one. Only a task that adopts the working copy the issue's
// roles share waits for another role's turn (adopts).
func (m *Machine) adoptionWaits() (claim.Token, bool) {
	if !m.adopts(*m.claim.Pending) {
		return "", false
	}
	return m.issueInTurn()
}

// mergeForwardOwed names the role a merge forward of this claim's issue is owed to, when one is
// owed and has not landed (Deps.MergeForwardOwed), read from the record at each ask: a take of the
// pending task that would adopt the working copy waits for that merge to land (takeable), since
// the merge writes the working copy between runs no turn of the issue covers. A daemon with no
// workflow supplies no answer, and no merge is owed. A read that fails is the caller's to answer:
// it decides nothing. Its error is the reader's as it came, which names the read
// (workflowRuntime.mergeForwardOwed in internal/daemon).
func (m *Machine) mergeForwardOwed(ctx context.Context) (claim.Role, bool, error) {
	if m.deps.MergeForwardOwed == nil || m.claim.Issue == "" {
		return "", false, nil
	}
	role, owed, err := m.deps.MergeForwardOwed(ctx, m.claim.Issue)
	if err != nil {
		return "", false, err
	}
	return role, owed, nil
}

// adopt hands the working copy the role's App identity before the phase task's take is
// answered, so the task's commits are authored by the bot. It runs with the machine's lock let
// go (Machine.Take), so it reads only what the take captured under the lock (adoption). A failure
// refuses the take (refusedAdoption).
//
// A mint that failed is marked as what it is (runtime.ErrIdentityUnavailable) rather than left to
// read as the transport's silence: it is raised here, before any frame leaves the daemon, and the
// take's refusal is logged by that name (refusedAdoption).
//
// An adoption the shim answers ok is logged here, once, at Info: the task, its phase, the App's git
// name — never a token — and how long the call to adopt took (Runtime.AdoptWorkingCopy): the
// runtime's own check of where the claim's process runs, then the round trip to the shim and back,
// with the shim's own work inside it. The refusals are logged where the take's refusal is
// (refusedAdoption); without this line nothing in the log tells which tasks adopted the working
// copy, which is what a production check reads to see the rule held (dispatch://INFRA-2695).
func (m *Machine) adopt(ctx context.Context, a adoption) error {
	if a.loc == nil {
		return fmt.Errorf("adopt the working copy of %s: the claim has no locator", a.token)
	}
	id, err := m.deps.Identity(ctx, a.role, a.issue)
	if err != nil {
		return fmt.Errorf("%w: the git identity of %s: %w", runtime.ErrIdentityUnavailable, a.token, err)
	}
	started := m.deps.Clock.Now()
	if err := m.deps.Runtime.AdoptWorkingCopy(ctx, *a.loc, id); err != nil {
		return err
	}
	m.log.Info("supervise: the working copy was adopted for the phase task",
		"task", a.number, "delivery", a.delivery, "phase", string(a.phase), "identity", id.Name, "took", m.deps.Clock.Now().Sub(started))
	return nil
}

// settle runs around every decision, so that three things hold however the claim got where it is,
// and hold again at once for a claim restored from a store a crash left in between: a delivery an
// earlier write left unsaved is written, a task whose take's turn is over is finished where no
// end-frame bound can finish it, and a suspended claim holds no task the suspension finished. A
// death is not one of them: it takes the task whose turn it ended back first (interrupted). A
// task whose turn ended in a ready or idle claim is not either: what finishes it there is the
// report naming its number, or the end-frame bound with none after it (finishTask,
// finishAtTurnsEnd), never a turn's end alone — with background work on for every role, a
// background result wakes a settled session and starts a turn of its own, whose end says nothing
// about the task.
//
// A claim that is neither working nor ready nor idle holds no process that is in or between turns
// of its own — it is launching, suspended, failed, its launch uncertain — so the turn of a task
// whose take's turn started there is over, and no end-frame bound watches it: the bound is armed
// at a turn's end and at the worker's first act after a ready or idle restore only (turnEnded,
// armFinishAtAct). Such a task is settled here by its claim's contract, as interrupted settles a
// take whose turn is over: finished by its end frame where the end frame finishes a task
// (TakesNoTask), as the bound would have finished it, or by the report whose finish the
// store refused (finishOwed, overTurnFinish), and taken back with the note a cut turn gets where
// only a report naming its number finishes it and none has. Left, it would stand started for
// good, and the take of it the next process makes would be answered again the text of a turn
// that already ran. A take awaiting its turn whose report's finish the store refused is finished
// here by that report too (takePhase, waitFinish): the let-go that found it finished it, and a
// refusal of that finish leaves it to this.
//
// A claim a daemon start restored over a provider error's report the store took and whose
// take-back it refused (Machine.ladderOwed, Claim.owedReport) has that report's take-back and
// ladder step made here, at its first decision, the first point that can write them: the claim's
// run of provider-error turns counts the turn, as the report's own decision counted it in memory
// alone, and the take goes to the ladder as that decision sent it (takeToLadder). The controller
// is not told again: that decision told it before any write. Only a claim ready, idle or working
// holds the take for the ladder; any other has it settled by its let-go, below. A write of that
// step the store refuses fails nothing here: the take-back and the step stand in memory, their
// write owed (providerErrorClears), and the next decision makes it (repair, and the unsaved
// delivery above), as it makes every write a step left owed, so the decision goes on to its fence
// and its row — the claim's first decision after a daemon start can be its shim's hello, which
// opens its view and arms its deadline there (helloView, connview.go). Only a step that failed the
// claim, for an error that cannot clear, returns its write's error, as the report's own
// decision returns it (providerErrored): the failed claim's process is let go, so the event's row
// has no view to open and no bound to arm over it.
//
// A daemon suspension ends the claim's phase, so a task queued for a phase is the finished phase's
// and goes with it, its value deleted; the next resume is handed its new phase's task, never that
// one. An operator suspension is different: the operator resumes this same claim, so its pending
// phase task waits through that suspension and is taken after the operator's resume. A released
// claim's is the same — the daemon released an idle pod, not the phase, and the role's next wake
// resumes this session in that phase with the task it kept. SuspendSource.RetiresPhaseTask is the
// one statement of which sources end the phase. A task of no phase is likewise not the
// workflow's to end.
func (m *Machine) settle(ctx context.Context) error {
	p := m.claim.Pending
	if p == nil {
		return nil
	}
	if m.unsaved {
		if err := m.saved(m.putClaimAndPending(ctx)); err != nil {
			m.log.Warn("supervise: the pending delivery is still not written", "delivery", p.ID, "error", err)
		}
	}
	if owed := m.ladderOwed; owed != nil {
		m.ladderOwed = nil
		if SaidReady(m.claim.State) && p.taken() {
			m.claim.ProviderErrorRun++
			m.log.Warn(ladderOwedLine, "task", p.Number, "httpStatus", owed.ProviderErrorStatus, "code", owed.ProviderErrorCode,
				"providerErrorRun", m.claim.ProviderErrorRun)
			if err := m.takeToLadder(ctx, owed.ProviderErrorStatus, owed.ProviderErrorCode); err != nil {
				if m.claim.State == StateFailed {
					return err
				}
				m.log.Warn("supervise: the pending delivery is still not written", "delivery", p.ID, "error", err)
			}
			if p = m.claim.Pending; p == nil {
				return nil
			}
		}
	}
	if !SaidReady(m.claim.State) && m.takesTurnRan() {
		if by, finishes := m.overTurnFinish(); finishes {
			return m.finishTask(ctx, by)
		}
		if err := m.interrupted(ctx, "the turn of the task's take is over and no report named its number", ordinaryEnd); err != nil {
			return err
		}
	}
	if m.claim.State != StateSuspended || p.Phase == "" || !m.claim.SuspendedBy.RetiresPhaseTask() {
		return nil
	}
	return m.dropPending(ctx, valueDeletedSuspended)
}

// interruptedTask begins a task taken back from a turn its process could no longer finish
// (interrupted), once nothing untold stands in front of it: the claim's record of the let-go that
// cut the turn off has been told, or no record names it (message). Oh My Pi does not continue an
// interrupted turn when its session is resumed, and the resumed session already holds the task,
// so the agent is told to carry on from where the turn stopped, not start over. It is the record's
// own sentence for a task whose turn was cut off (relaunchSentence), under cause, the cause the
// claim's record names (EndedWork.Cause): the let-go that took the task back, whichever path that
// was — a death, a displacement, a repoint, a stall, a stale plugin, a suspension — rather than
// the last death's words, which named a death for every one of them. It says what that end took
// with it: a tool call in flight answered nobody, and the daemon cannot say which subagents and
// background jobs went with the process, so it says that every one of them is gone, none of which
// the resumed transcript shows as missing — a worker relaunched six times in an hour and told none
// of this read its own transcript as work in progress (dispatch://INFRA-714 R6). A record with no
// cause says so plainly rather than inventing one.
func interruptedTask(cause string) string {
	return relaunchSentence(EndedWork{Cause: cause, Turn: true, TaskTurn: true}, true) + "\n\n"
}

// abandonedTask begins the first task given to the agent that replaced a session the claim
// abandoned (forgetSession): one whose last turns the provider refused, one left behind by a
// fresh session the operator or the controller asked for or by the operator's forget-session
// (forgottenBy), or one no session store holds any more. Nothing in a new session says that an
// older one existed, so a worker reading an empty transcript takes its task for a fresh start and
// does again what is already pushed (item 4 of the refusal section of the phase-workflow spec). It
// names the session that was left behind and why in the words the claim recorded, and then the
// two things that outlive a session: the issue's branch, and the handoff records its phases wrote
// on the Dispatch issue. The controller is told neither, since it works no checkout. last is
// whether this sentence is the last one before the task, and so ends in the task's own
// instruction: in front of a task with no record's sentence after it. Where the record's sentence
// follows it (EndedWork.Tells), that sentence is the last and carries the instruction instead
// (relaunchSentence); in front of the daemon's own delivery of a relaunch's record, which has no
// task (Delivery.Relaunch), neither carries one (Delivery.message).
//
// The reason can be far-side text read into an agent's prompt — a runtime's account of a death,
// or the provider's own words in a failure an earlier daemon wrote — so it is quoted as a Go
// string with %q, the way a check run's name is where the daemon reads one into a prompt
// (workflow's namedConclusions): the quotes bound it, and a newline or a quote inside it is
// escaped rather than ending the sentence that carries it, so no refusal text can read as an
// instruction of the daemon's own. The abandoned session's id is quoted the same way, with %q.
func abandonedTask(c Claim, last bool) string {
	read := "read the issue"
	if c.Role != claim.RoleController {
		read = fmt.Sprintf("read the issue, then your branch %s and the handoff records its phases wrote on "+
			"the issue, which `dispatch handoff-read --issue %s --phase <word>` reads (%s)",
			claim.IssueBranch(c.Issue), c.Issue, handoffWords())
	}
	sentence := fmt.Sprintf("The session this claim ran before this one, %q, was abandoned where it stood: %q. "+
		"This session is a new one: the turns, tool calls, subagents and background jobs of that session are "+
		"gone, and only what it pushed and committed survives. Before you repeat any of that work, %s, and "+
		"tell the architect your Legion addressing line names what you found, unless that is you or it names none.",
		c.Abandoned.Session, c.Abandoned.Why, read)
	if last {
		sentence += " Then continue the task."
	}
	return sentence + "\n\n"
}

// handoffWords is every phase word an issue's handoff records are written under, in the
// transition table's order: the records the replacement of an abandoned session is told to read.
// It is built from the phases themselves (phase.All, phase.Handoff) rather than spelled here, so
// a phase that gains a handoff later is named in this prompt without anyone remembering to come
// back.
func handoffWords() string {
	names := make([]string, 0, len(phase.All))
	for _, p := range phase.All {
		if word, _, ok := phase.Handoff(p); ok {
			names = append(names, word)
		}
	}
	return strings.Join(names, ", ")
}

// outOfMemoryTask follows the sentence of a task's interruption on a task whose turn ended when
// its pod ran out of memory. The agent cannot see why the kubelet ended its container, and the
// heavy work that filled the pod is the thing it must stop doing there, so it is told what
// happened and where the rule about work a pod does not run is written.
const outOfMemoryTask = "Your pod ran out of memory. Kubernetes ended the container that runs " +
	"every role of this issue, with the reason OOMKilled, and that end is what stopped your turn. " +
	"The work that filled the pod can be another role's, not yours. An issue pod runs no heavy " +
	"build and no whole test suite, so read skill://legion-worker/references/tools.md before you " +
	"run such work again, and send it to CI on the pull request or where your deployment's " +
	"instructions say.\n\n"

// interrupted takes back the pending task whose take the claim's process can no longer see
// through — it died, or a forced stop is about to end it — so the relaunched agent reads its
// value at registration and takes it again under the same number: Oh My Pi does not resume the
// turn itself. why names which, for the line it logs. A take whose turn was running goes back
// marked Interrupted, so its next accepted take carries the sentence of its interruption
// (message); outOfMemory says that end was the pod running out of memory, which adds
// outOfMemoryTask, and it is written on every such call, true or false, so a second interruption
// of the same task for another reason clears a note that no longer describes it. A take whose
// turn had not started goes back with no note, since the worker ran nothing of it, except one
// whose turn may have run with no frame of it reaching the machine (turnMayHaveRunUnseen): the
// claim's connection does not show every frame of its turns — no hello since a daemon start or a
// close, or neither the agent's answer nor a turn frame since the hello — or the take is marked so
// (Machine.turnUnseen), by the reconnect deadline, by a stale relaunch decided behind a cut
// backlog, by a turn's end the daemon never saw begin, or by the record of a let-go made while the
// view was not whole (recordEnd). It goes back as a turn cut off, by the one rule both take-backs
// of a take follow (dropTake), since the worker may have run it whole; so a relaunch of a live
// process, whose take-back comes before its let-go records anything (replaceNow), reads the view
// as every let-go does (row 26 of dispatch://INFRA-2811's plan). The task is neither retired nor
// its run marked served, since the turn never finished; the claim's TaskTaken stays, since the
// worker read it. A task no take holds is left as it is. An older daemon's ask of an update round
// is the one task no take is accepted for again: it keeps its delivery id, which names its round,
// and the next take of it drops it (takeable).
//
// A take whose report named its number and whose finish the store refused (Machine.finishOwed) is
// not taken back but finished, by that report, whichever turn it awaits, whether or not a turn
// runs, and whatever the claim's plugin: the report decides (makeOwedFinish), and a finish the
// store refuses again is retried as this let-go's own refusals are. A take whose turn has otherwise
// ended (takesTurnRan) is settled by its claim's contract. Where the end frame finishes a task — a
// plugin below TaskTakeContract, whose reports name no number (TakesNoTask) — it is not taken back
// but finished: its turn ran whole, and only the report of it is lost with the process
// (finishTask, by the end frame). Where only a report naming the number finishes it, and none has,
// the end of a run says nothing of whose run it was, so it is taken back with the note a cut turn
// gets, as though its turn were still running: the next accepted take carries the sentence of its
// interruption, and the let-go that follows records the task's turn as cut off
// (Machine.cutTurnOf). Whether a turn runs is the one answer every let-go reads (cutsTurn, as
// recordEnd and the stall backstop read it): a claim stored working for nothing that runs is in
// no turn, so a compaction Oh My Pi started after the take's turn ended and before its report
// leaves that take to be settled here too, whichever let-go comes.
//
// The taken-back task becomes the claim's only once its write has landed. A write that fails
// leaves the task taken in memory as in the store, and the process is still recorded, so the
// sweep finds it gone again and this takes the task back then — where a task taken back in memory
// alone would be skipped by that death and relaunched from a store still holding it taken.
//
// A take-back that sends the take back as a cut turn also marks the task as taken from a turn of
// the process the claim records (Machine.cutTurnOf, dropTake), once its write lands, and the let-go
// that follows records from that mark that the task's turn was cut off, whatever the claim's state
// (recordEnd): the take it clears is what that let-go would otherwise read, and it reads that only
// where a turn still runs. A decision that records its let-go before this take-back (died,
// displaced) reads the take itself there and needs no mark.
//
// What the accepted take told stays told where the take's turn started: that turn ran the take's
// text, so the agent read the sentence it carried, and the let-go that cuts the turn off takes no
// telling back. The take's mark of what it told (Delivery.TakeTold) goes with the take, so no
// later take of the task gives that telling back. A take whose turn had not started ran its text
// in no turn the daemon saw, so what it told is given back with it, and the claim's handshake
// gives back its confirmation of the take's entry (giveBackTake), a take whose turn may have run
// unseen (turnMayHaveRunUnseen) as well: the next accepted take carries the sentence again, and
// the state gives no `entryConfirmedAt` for an entry no turn the daemon saw ran
// (dispatch://INFRA-2614 R6). The untelling stands in memory whether or not the write lands,
// since untold is the safe way round, and the claim's next write carries it; the handshake moves
// only with the take-back's write, which carries the claim with the delivery where either moved,
// so a write that fails leaves the handshake as the store holds it, for the take-back made again
// to read.
//
// The take's copy of the news of the approved design goes with it (Delivery.Carried, credit), and
// the write ends the credit it held, whether or not the take's turn started: no finish says that
// turn ran, so nothing made the credit final, and the next accepted take carries the news again —
// told twice, never not at all.
func (m *Machine) interrupted(ctx context.Context, why string, outOfMemory bool) error {
	p := m.claim.Pending
	if p == nil || !p.taken() {
		return nil
	}
	inTurn := m.cutsTurn()
	if owed, err := m.makeOwedFinish(ctx); owed {
		return err
	}
	if m.takesTurnRan() && !inTurn {
		if TakesNoTask(m.claim.PluginContract, m.deps.TaskTakeContract) {
			return m.finishTask(ctx, finishedByEndFrame)
		}
		inTurn = true
	}
	m.log.Warn("supervise: "+why+"; the task waits for the relaunch", "task", p.Number, "delivery", p.ID)
	next := *p
	handshake := m.claim.Handshake
	claimMoved := m.takeAwaitsTurn() && m.giveBackTake(p)
	cut := m.dropTake(&next, inTurn, outOfMemory)
	var err error
	if claimMoved {
		err = m.putClaimAndDelivery(ctx, m.stored(), next)
	} else {
		err = m.putDelivery(ctx, next)
	}
	if err != nil {
		m.claim.Handshake = handshake
		return err
	}
	*p = next
	m.disarm(TimerFinish)
	if cut {
		m.cutTurnOf = p.ID
	}
	m.log.Info(takenBackLine, "task", p.Number, "cause", why)
	return nil
}

// ordinaryEnd is interrupted's outOfMemory at a call site that ends the turn of a process still
// running — one replaced mid-turn, for a stale address, a stale plugin, a stalled turn or a paced
// launch whose turn has come, or one a held suspension's deadline stops — where nothing ended the
// pod's container, so the end was not the pod running out of memory. A death and a displacement
// pass the observation's own OutOfMemory instead, and the suspension that lands over a process a
// report found gone passes what that report recorded (suspendOverADeadProcess, goneOutOfMemory).
const ordinaryEnd = false
