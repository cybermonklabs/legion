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

// Delivery is a claim's pending task: at most one per claim, persisted, and sent only when the
// claim is ready or idle and its connection is registered.
//
// QueuedAt is when the task was handed to the claim; DeliveredAt is the latest acknowledgement of
// its prompt (MarkedBy is the delivery id that prompt carried), and with it the daemon's answer to
// whether the agent may have read this task; ConfirmedAt is the turn that send started. An
// acknowledgement is not a delivery — Oh My Pi acknowledges before it starts a turn, and can
// accept a prompt that starts none — so only ConfirmedAt says the task arrived. A confirmed delivery stays until the turn it confirmed ends,
// because a refusal that arrives after the acknowledgement takes the confirmation back.
//
// DeliveredAt carries that second meaning because a refusal of that prompt clears it: an agent
// that refused it, in a turn of its own or with no turn at all, never read the task. A task
// acknowledged and waiting for a turn keeps the mark — the turn may be this task's, starting late
// — and so does one the wait for its turn re-queued. It is what a worker's read of its task is
// judged by wherever the claim has a run behind it; a claim that has served no run at all is
// working the one task it holds whether or not that mark is set (Claim.ServingRun).
//
// The id is the daemon's and travels on the prompt frame; a shim that sees the same id twice
// answers it without a second turn. The id is kept across a send that failed in transit, so a
// retry is recognised, and rotated after a prompt that was acknowledged and started no turn, so
// the retry is a new prompt rather than an echo of the acknowledged one.
//
// Phase is the issue phase the task was queued for, which is what says whether the task is still
// the work to do: it is carried here, persisted, because the id is rewritten by that rotation and
// the task text is the workflow's to write. A delivery of no phase — an operator's own, an
// architect's — is never dropped for the phase its issue reaches, and a suspension does not take
// it either: it waits the suspension out and goes on the resume (settle).
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
	// CheckLook is the look and check instance this task gives the worker. ServingCheck uses it
	// to attribute an explicitly named check report to the look the worker took, not to choose
	// between a report and a phase completion. Phase tasks, operator tasks and resumes carry zero.
	CheckLook CheckLook
	Phase     phase.Phase
	// Branch is the GitHub branch of the task whose step this task is of, from the start row that
	// queued it (record.SuperviseRequest.Branch), and "" for the issue's own branch: every task of
	// an issue with no plan rows, of the task that holds the issue's branch, and of no start at all.
	// Each send's adoption takes the working copy of that branch (adopt, Runtime.AdoptWorkingCopy),
	// and a fresh session after an abandoned one is told to read it (abandonedTask). It is
	// persisted with the delivery, so a send after a restart of the daemon adopts the same copy.
	Branch      string
	QueuedAt    time.Time
	DeliveredAt time.Time
	// MarkedBy is the delivery id the marking prompt carried: the prompt whose acknowledgement set
	// DeliveredAt, the read mark. A late refusal is judged against the prompt it names: one naming
	// MarkedBy says the prompt that marked the task never ran, so the mark goes (markUnread), whatever
	// the claim's state and whichever connection carried it; one naming any other prompt says nothing
	// about the mark. Which prompt set the mark is a fact recorded when it is set and kept with the
	// task, so no ordering of sends, acknowledgements, refusals, reconnects and restarts has to be
	// reasoned about: only markRead and clearReadMark write it, each together with DeliveredAt. A task
	// that was replaced may still be named by a refusal, which then has nothing to clear.
	MarkedBy    string
	ConfirmedAt time.Time
	// Interrupted says a turn of the task was running when the process the claim recorded was let
	// go (interrupted) — a death, a displacement, a repoint, a stall, a stale plugin, a held
	// suspension's stop — so the task is sent behind the sentence that says so from then on: the
	// claim's untold record of that let-go while it tells, and interruptedTask after (message). The
	// task text stays as the workflow wrote it.
	Interrupted bool
	// Prompted says the daemon has put a prompt of this task to the agent, whatever came of it —
	// a refusal, a busy answer, an acknowledgement, a turn. It is set when the send goes out
	// (startSend) and by any outcome of such a send (markRead, clearReadMark), never cleared while
	// the task lives, and persisted, because it is what tells a task nobody has read from one the
	// daemon has not sent at all: a claim that has served no run is working the task it holds
	// unless a prompt of that task has already come to nothing (Claim.ServingRun). Since it is set
	// before the send adopts the working copy and mints the role's identity, a send that failed at
	// either sets it with no prompt sent, so it is not what says a prompt reached the agent: for
	// news of the approved design, the copy an acknowledged prompt carried says that
	// (Delivery.Carried, unsentNews).
	Prompted bool
	// OutOfMemory says the end that cut a turn of this task off was its pod running out of memory
	// (runtime.Observation.OutOfMemory), so the task is sent behind outOfMemoryTask as well as
	// behind the sentence of its interruption from then on (message). The task text stays as the
	// workflow wrote it.
	OutOfMemory bool
	// SendFailures, LastSendFailureAt and LastSendError are the diagnosis record of this task's
	// sends that came back from their own send goroutine without reaching a turn (table.go's
	// refused): how many of them failed, when the last one failed by the machine's clock, and
	// the error it failed with. They are a record of what happened, not a budget. The budget is
	// the claim's (Claim.Budgets.PromptFailures and PromptRetires), which only the failures
	// charged as the far side's answer about this workspace spend; this counts every send that
	// failed there, charged or not, and that difference is the point of recording it. A send
	// lost to the transport is charged nothing and waits for the next hello, sweep, turn end or
	// ready — for ever, by design — so a claim can sit for days with its budget untouched and
	// nothing on it saying why, and that is the case an operator most needs named (INFRA-1480:
	// two planning tasks held 31 and 33 hours, whose only record of each failed send was a log
	// line inside the pod, so diagnosing the stall took a person and a hand-run check in there).
	// A prompt the agent acknowledged and then refused is not counted here: it comes back as a
	// late refusal of its own (lateRefused), which charges the budget, so it bounds itself and
	// is read there rather than in this record of the silent ones. All three are written
	// together on every send that fails in refused, and cleared together where the budgets are
	// cleared (confirmCarrying), so a task that got through carries zeroes. LastSendError is the
	// far side's text as the daemon keeps any far side's error text (storetext.FarSideError), set
	// so in memory as well as in the store, and the whole text is in the log (table.go's refused).
	SendFailures      int
	LastSendFailureAt time.Time
	LastSendError     string
	// Kept is what the task names of the decisions a person made while nobody in the root's tree
	// could hear them, as the row that queued the task recorded them (record.KeptDecisions, encoded
	// JSON). It is opaque here — this package knows nothing of the record — and is carried and
	// persisted with the delivery because the delivery is what holds those rows: the store's write
	// of this delivery marks each held by this claim and delivery id, the write that records
	// ConfirmedAt tells them, and the write that retires it leaves them kept for whichever tree
	// owes them. It is rewritten, with the task text, by the send-time narrowing of what the task
	// still names of this claim's tree (Deps.RootTask). Empty for a task that names none.
	Kept []byte
	// News is what the task carries of news of the approved design (record.DeliveryNews, encoded
	// JSON): the start's own text, each item it tells and each opening it carries. It is opaque here
	// as Kept is, and carried and persisted with the delivery. It is rewritten, with the task text,
	// at every send, by the one render that writes both (Deps.DesignNews, and Deps.RootTask where a
	// root architect's task is narrowed), so the news a delivery holds is exactly what its text
	// names: no writer keeps one beside a text it did not render with it. Empty for a task that
	// carries none, which no send asks about.
	News []byte
	// Carried is the copy of News that the prompt whose turn this delivery's confirmation takes
	// carried, and, while the delivery is confirmed, its credit: what its audiences are read as
	// having taken from it (the claim store's design_news_told_in), held by this delivery's row and
	// by nothing else, so the write that takes the confirmation back, or the retirement, ends exactly
	// that credit, whatever has been credited since. News is rendered again at each send, so a later
	// prompt can carry more than the earlier one whose turn starts — the late turn of a prompt whose
	// wait ran out while a resend was answered busy, or, after a restart, the turn of a prompt an
	// earlier daemon sent — and that turn's confirmation must credit only what its own prompt said
	// (turnAnswers).
	//
	// It is written where Oh My Pi acknowledges a prompt of the task (acked): the send's adoption of
	// the working copy and its identity mint went through, and the prompt reached the agent. A
	// send whose adoption or mint failed, or that Oh My Pi refused, writes nothing, so a task no
	// acknowledged prompt carried holds none, and the question a restart left never takes a turn of
	// the agent's own for it (unsentNews). The confirmation fixes it to the prompt its turn answers.
	// It goes where the confirmation is taken back, or the task is taken back with no prompt of it
	// left outstanding, since no turn of a prompt that carried it can start any more. It stays once
	// the turn's result says the turn ran, which makes the credit final (Ran).
	Carried []byte
	// Ran says the result of the prompt whose turn this delivery's confirmation took said that turn
	// ran with no provider error (promptResulted): the agent read what Carried credits, so the
	// credit is final, and the write that records Ran folds it into the told rows (the claim store's
	// settleDesignNews), where it outlives the delivery. It is set only on a confirmed delivery with
	// a copy, and it goes with the copy: every write of Carried writes it false (credit).
	Ran bool
	// NewsOnly says the task was queued for news of the approved design alone
	// (RequestDeliver.NewsOnly, the daemon's news-only start). It names its member's phase and run,
	// so that it is dropped once the issue has left that phase (phaseHolds), but it is no task of
	// that phase: it hands the worker no work of the phase, only the news. So no reader of a
	// claim's pending delivery takes it for the phase's task (StillWorked, HoldsWork), and the start
	// or repair that sends the phase's task still sends it to a claim that holds one. The mark is
	// the delivery's own column (pending_task_deliveries.news_only), written and read beside News,
	// so no writer of the news can drop it.
	NewsOnly bool
	// ProviderErrorRetries is how many times this task has been sent again after a turn of it
	// ended in a provider error, from 0 (promptResulted's providerErrored). It is what the wait
	// before the next send is a function of (promptRetryDelay), so a daemon restarted in the
	// middle of a ladder goes on climbing it rather than starting again at the first step. It is
	// the task's own count, not the claim's: a new task starts at 0.
	ProviderErrorRetries int
	// ProviderErrorNote is the sentence such a task is sent behind, as an Interrupted task is
	// sent behind the sentence of its interruption: what ended the previous turn, and that the
	// daemon is sending the task again. It is built from structured fields alone — the HTTP
	// status the provider answered, else nothing more than "a provider error" — because a
	// refusal's own explanation is itself refused when it is sent back
	// (dispatch://INFRA-956/artifact/cause-evidence-md), so no provider text reaches a prompt.
	// Empty for a task no provider error has taken back.
	ProviderErrorNote string
	// RoundNamed is the typed fact that Task ends with the daemon's two sentences of a reviewer's
	// review round (reviewround.Sentences), from the start that queued it (RequestDeliver.RoundNamed):
	// the start row's own mark (record.SuperviseRequest.RoundNamed), or the round the start
	// executor names on a reviewer's start queued for news alone (the daemon's newsForStart). The
	// send of a task that carries news keeps the two last, after the news (the daemon's
	// renderNews). message reads it: a task of the reviewer without it, which a start row of the
	// release before this one, an operator or a delivery the release before stored can be, is sent
	// with the daemon's sentences naming no commit and no entry at its end.
	RoundNamed bool
}

// StillWorked says whether a claim in state will still work this task as the task of generation's
// run in phase p: it is that run's task for that phase, and either not yet confirmed (sent once the
// claim is ready, a task a death took back included) or confirmed in the turn the claim is working.
// A confirmed task on a claim not working is over: the claim's next decision retires it. A task of
// news alone (NewsOnly) names the phase and the run but is no task of the phase, so it is never
// the phase's task a claim still works: a start that retries a failed claim still sends the
// phase's task (the daemon's keepsStartTask), the silence repair still sees a worker that holds
// no phase task (workflow's unpromptedPhaseWorker), and the rest of the readers of this rule read
// it the same way.
func (d Delivery) StillWorked(generation uint64, p phase.Phase, state ClaimState) bool {
	return !d.NewsOnly && d.Generation == generation && d.Phase == p && (d.ConfirmedAt.IsZero() || state == StateWorking)
}

// SupersededBy is whether a task of the run generation at entry takes d's place in a tree root
// architect's hands by the root architect's rule: d is a task of an earlier run, or of the same run
// at a lower entry, and d carries a run and an entry of its own. A task of no run (generation 0),
// an operator's own and the daemon's delivery of a relaunch's record among them, is superseded by
// nothing here, and so is a task of no entry: a wake's task (workflow's wakeRow), which is the
// sentence that says why the pod came back and is no entry's work, and every task a daemon
// delivered before it numbered entries. A newer task is refused behind such a task while it is
// untaken, as behind any untaken task, and it is sent first (queue). Three tasks are the
// exceptions, each replaced by another rule than this one. The two a newer task replaces whatever
// they carry (replaceable): the task a provider error took back while the ladder's wait stands for
// it, and the daemon's own delivery of a relaunch's record while no prompt of it waits on a result.
// And a task of news of the approved design alone (NewsOnly), which carries no entry: the task of
// any entry takes its place, for every role (Machine.supersedes, whose first rule that is), since
// the start of an entry carries that same news itself.
//
// It is the one statement of this comparison, and two readers make it: the claim's machine,
// before it puts a newer task in the place of the one the claim holds unconfirmed
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

// sentences is which of the claim's two untold records a prompt carries the sentence of: the
// session the claim abandoned (AbandonedSession, abandonedTask), and what the last let-go of its
// process ended (EndedWork, relaunchSentence). Delivery.message answers it beside the prompt it
// composes, so what a prompt is marked as telling is what its text holds (Machine.carried, which
// confirmCarrying reads).
type sentences struct{ abandoned, ended bool }

// message is the prompt a delivery is sent as, and which of the claim's two untold records it
// tells (sentences): its task, behind the sentences the claim's state gives it that say what
// became of the agent's earlier work, and then the provider's. In order:
//
//   - The first task the agent that replaced an abandoned session is given names that session
//     (abandonedTask), and says that everything of the old session is gone. A record that still
//     tells beside it is one of the fresh session's own processes (forgetSession), and that
//     record's sentence follows the abandoned session's (the next item): the prompt tells both.
//     What the abandoned session's sentence stands in place of is the old session's turn: the
//     note of an interruption (interruptedTask) and the out-of-memory note behind it, which a
//     task taken back from a turn of the old session's process still carries. A turn of the
//     fresh process that was cut off on this task is the fresh process's own, and the record
//     that tells says so (EndedWork.TaskTurn): the let-go that wrote it took the task back from
//     that turn and wrote its interruption anew (recordEnd, interrupted). So the record's
//     sentence says that turn was interrupted, and the out-of-memory note follows it as it does
//     where no session was abandoned.
//   - While the claim's record of what its last let-go ended is untold (EndedWork.Tells), every
//     prompt carries that record's sentence in front of its task (relaunchSentence): the cause,
//     the turn and the background work that let-go cut off, whatever the task. It also says what
//     interruptedTask would say of a task taken back from that turn — it begins with the turn of
//     this task being interrupted where the record says the claim's task was in it
//     (EndedWork.TaskTurn) and this is the task that let-go took back (Interrupted) — so a task
//     carries the one sentence and not both. In front of any other task, such as the new task of
//     its phase that a retry of the stopped claim (endStopped's comment names each) gives a worker
//     a stop cut off in its task's turn, it says instead that the turn the agent was in was
//     interrupted (EndedWork.inFrontOf).
//   - A task taken back from a turn (Interrupted) whose record has been told, or that no record
//     names, still says its turn was interrupted, under the cause the claim's record names
//     (interruptedTask): the let-go that took it back, whichever path that was, rather than the
//     last death's words. With an abandoned session still to tell, that turn was the old
//     session's, and the abandoned session's sentence stands in its place (the first item).
//
// Composing a prompt tells nothing (confirmCarrying).
//
// Behind the interruption, when its end was the pod running out of memory, comes outOfMemoryTask
// — the agent reads that its turn was interrupted, then why, then the task. A task a provider
// error took back carries its own sentence last of all (ProviderErrorNote), so the agent reads
// what the provider did and then the task; each of these says something the others do not.
//
// The daemon's own delivery of a relaunch's record (Delivery.Relaunch, queueRelaunch) has no task:
// its text is the record's sentence alone (RelaunchSentence), composed from the record as it
// stands when the prompt goes, so a let-go recorded after it was queued is told in it too. One
// whose record no longer tells has nothing to say, and sendPending retires it rather than send it.
// The abandoned session's sentence, the out-of-memory note and the provider's go in front of that
// text as they go in front of a task, the abandoned session's without the task's instruction,
// since there is no task to continue (abandonedTask); the record's sentence does not, because it
// is the text.
//
// A task of the reviewer ends with the daemon's two sentences of its review round, the last two
// of each task the reviewer is sent (reviewround.Sentences), and this is the one place each
// prompt of a delivery is composed, so it is where the rule holds for every task, whatever queued
// it. A task that says it ends with them (RoundNamed), as each task the workflow queues for the
// reviewer does (workflow's queuedRow) and each the start executor names on a reviewer's start
// queued for news alone (the daemon's newsForStart), is sent as it stands. A task of the reviewer
// that does not say so is given them at its end, naming no commit and no entry: a start row or a
// delivery the release before this one stored, whose text can end with words of the same shape
// that no review recorded, or a task an operator wrote. Its reviewer reads the whole change and
// keeps the entry of its round. It is the fact and never the task's text that tells the two
// apart, since two such texts can be the same word for word. The relaunch's record is no task, and
// is sent as it is.
//
// One case still sends a stale note: the previous release's upsert carries no out_of_memory
// column, so if an image rollback takes a marked task back again for another end, the row keeps
// the mark, and a roll-forward sends the note for that end.
func (d Delivery) message(c Claim) (string, sentences) {
	text, carried := d.Task, sentences{}
	relaunch := d.Relaunch()
	if relaunch && c.EndedWork.Tells() {
		text, carried.ended = RelaunchSentence(c.EndedWork), true
	}
	if !relaunch && !d.RoundNamed && c.Role == claim.RoleReviewer {
		text += reviewround.Sentences("", 0)
	}
	var prefix string
	if c.Abandoned.Tells() {
		// The task's instruction ends the last sentence before the task, so the abandoned
		// session's carries it only where no record's sentence follows it.
		prefix, carried.abandoned = abandonedTask(c, d.Branch, !relaunch && !c.EndedWork.Tells()), true
	}
	// oldTurn is whether a turn this task was taken back from is the abandoned session's, whose
	// sentence stands in place of the notes of that turn: an abandoned session still tells, and
	// no record of the fresh process says the task was in a turn of its own.
	oldTurn := c.Abandoned.Tells() && !(c.EndedWork.Tells() && c.EndedWork.TaskTurn)
	switch {
	case relaunch:
		// Its text is the record's sentence already.
	case c.EndedWork.Tells():
		prefix, carried.ended = prefix+relaunchSentence(c.EndedWork.inFrontOf(d), true)+"\n\n", true
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
// was queued for. An outbox repeats its row id after a crash before FinishOutbox; the same task
// and id are therefore accepted without changing the persisted delivery.
//
// A request whose entry the claim has already been queued a task of — an Entry at or below
// Claim.GivenEntry — is accepted and queues nothing, whether that delivery is still pending, was
// taken back under a new id, or has been retired: that entry's work was given once, and a second
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
// A request a pending task refuses sends that task first (sendPending), so a task held back
// because its root had no architect to hear it never wedges the claim. A start that carries a
// newer entry's task supersedes a held task that carries an entry of its own, of an earlier run
// or of a lower entry of the same run (supersedes), which is retired unsent. Every other start
// behind an untaken held task is refused, retries on its backoff and sends the held task first: a
// start of no entry, a resend of the held task's own entry, and any start behind a held task of
// no entry, a wake's among them (workflow's wakeRow). That refusal is over once the architect's
// turn on the held task has ended and the delivery retired.
//
// A pending delivery whose task the worker has already taken refuses nothing: the turn took it
// (confirmCarrying writes ConfirmedAt), so there is no task left to give and the delivery is only
// the record of the turn in flight. Such a delivery is retired here and the newer task queued in
// its place. Refusing it instead is what left three supervise rows of the live daemon answered
// "a delivery is already pending" with nothing ever sent (dispatch://INFRA-1352, INFRA-1443,
// INFRA-1519): the phase's own start was the newer task, and the worker that would have taken it
// was holding a delivery it had finished with. The newer task is not sent into the running turn —
// queue sends nothing, and sendPending hands a working claim nothing — so it goes at the next
// point a turn may take it, once, and no turn is interrupted or started twice.
//
// A taken task retired so before its turn's result has come ends the credit its confirmation made
// of the news of the approved design (Delivery.Carried, which goes with the row the claim store's
// RetireDelivery deletes): the result of that turn now answers a task the claim no longer holds,
// and the fence drops it, so a provider error that ends the turn could take nothing back. The news
// is owed again, and the next start of the phase, or the reconcile once the claim holds no untaken
// task, tells it: told twice, never not at all.
//
// An untaken pending delivery still refuses a second one, as it always has: that task has reached
// nobody, so accepting over it would lose a task rather than save one. Four untaken deliveries are
// not refused. Two of them are replaceable. One is the task a provider error took back, while the
// ladder's wait stands for it and nothing works it. The next step of the ladder can be ten minutes
// away and a run of errors has no end of its own, so a refusal would keep every newer task of the
// claim behind a task the claim owes for as long as the provider keeps failing
// (dispatch://INFRA-956 R2: a newer task replaces the owed one). The newer task takes its place:
// it is work for this claim, queued since, and R2 has the newer of the two go. The other is the
// daemon's own delivery of a relaunch's record (queueRelaunch) while no prompt of it waits on a
// result: it holds no work of the claim's, only the sentence, and the newer task carries that same
// sentence in front of itself while the record is untold (message), so nothing is lost and the
// phase worker's resume task the ready queued is not held behind it. Once a prompt of it is
// outstanding, its turn is the session's next, and the newer task waits for that turn to end as
// behind any untaken task.
//
// The third is a tree's root architect's task of a run that a task of a newer entry supersedes
// (supersedes): the pending task carries a run and an entry and is of an earlier run, or of the
// same run at a lower entry, and the request carries a run and an entry of its own. Each task the
// architect is given says what is true of its root (dispatch://INFRA-2614 R3), and the transition
// that raised the newer entry wrote that entry's task (R4), so the older text is no longer what
// the daemon owes. In the case that was measured, the older task told a re-admitted architect
// that its tree was open at the generation the re-admission ended, that every child was still to
// be filed and that no message was coming, while the tree's children ran. A refusal leaves the
// older task to be sent at the next ready, so the architect's first turn goes to a run that is
// over. So the newer task takes its place, and only while nothing of the older task is in the
// agent's hands: no send of it in flight and no prompt of it waiting on a result, the condition
// replaceable reads for the relaunch's record. Once a prompt of it is outstanding, its turn is the
// session's next, and the newer task waits for that turn to end as behind any untaken task. The
// start that carries the newer task does not send the older one first either (StartedBy): a task
// held while its root had no architect to hear it would otherwise go at the very start whose own
// task supersedes it.
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
// task is refused behind it and the wake's task is sent first, as it was before
// dispatch://INFRA-2614 (TestAnUnconfirmedTaskTheRuleDoesNotReachStillRefusesANewerOne), and the
// entry's task is sent once the wake's turn has ended
// (TestAnEntrysTaskRefusedBehindAWakesTaskIsSentOnceTheWakesTurnEnds).
//
// The fourth, for every role, is a task of news of the approved design alone (Delivery.NewsOnly)
// that a task of an entry supersedes (supersedes). It is a task of no entry, but not of the kind
// the architect's rule leaves in place: a wake's task says something no entry's task says, and
// everything a task of news alone says, the start of an entry carries too (the daemon's
// newsForStart reads the news the record owes the claim when the start runs, and a task no turn
// confirmed raised none of it). So the entry's task takes its place while nothing of it is in the
// agent's hands, and the worker is sent its entry's work, with the news, first and in one turn,
// rather than a turn of news and then, after the start's backoff, the work it is owed. A start of
// no entry carries no news, so it is refused behind such a task as behind any untaken task.
//
// What the older task named of a person's decisions is not lost with it: its retirement releases
// them (the claim store's releaseKeptDecisions), so they are the tree's again, and the catch-up of
// the architect's next ready names them, as does the one the root's start writes for an architect
// it finds running (the daemon's outbox, workflow's claimReady). The daemon's
// TestADecisionASupersededTaskNamedIsToldToTheReadmittedArchitect follows one such decision to the
// catch-up that tells it.
//
// The replaced task's row ends as any other retirement ends it, and the question a restart armed
// for that task is answered in the same step, which is the one path every replacement of a pending
// task takes (dropPending): the delete releases what that task held of a person's
// decisions for whichever tree is owed them (the claim store's RetireDelivery and
// releaseKeptDecisions), where a row overwritten in place would leave them held by a delivery id
// that is gone, or move them to a task whose text never named them. The wait ends with the task,
// and the task queued behind it is never taken for the running turn's.
//
// What the ladder counted belongs to the delivery it counted for, and goes with it: the attempt
// count and the sentence the next prompt would have carried are the delivery's own fields
// (ProviderErrorRetries, ProviderErrorNote), so the task that replaces it starts its own ladder
// at the first step. The claim's run of provider-error turns is the claim's and stays
// (Claim.ProviderErrorRun): the provider failed this claim's turns whichever task they worked,
// and only a turn that ends with no error puts that run back to zero.
//
// A task queued for news of the approved design alone (RequestDeliver.NewsOnly) takes neither
// place: it holds no work of the claim's, so it replaces no task that does (HoldsTask), confirmed
// or not. A confirmed task is the work of the turn that is running. If that turn ends in a
// provider error, the machine takes the task back and sends it again (providerErrored), and it can
// do so only while the task is still pending. A retired task's prompt result is fenced as stale,
// and the phase's task is lost until a repair finds the silent worker. A task a provider error took
// back is owed work, and the ladder sends it again. So such a request is refused, as one behind an
// untaken task is, and its start waits on its row's backoff until the claim holds no task: the
// turn ended and its task was retired. The daemon's own delivery of a relaunch's record holds no
// work, and a task of news alone takes its place as any newer task does.
//
// Of a task an operator gave the claim, which carries no id of its own and no outbox row
// (operatorDelivery), neither thing accepted above can be true, so the second refusal below is
// exactly a claim that holds a pending delivery its worker has not finished with and that no
// newer task replaces. runParkedLaunchNow reads the same pending delivery before it runs a parked
// launch for such a task, so nothing runs for one this refuses; a task that replaces one runs that
// launch where it arrives, and retires the replaced delivery itself before it does — the launch
// takes the ladder's wait with the claim's timers and every prompt's wait for a result with the
// process (letGo), so the claim reaching here then holds no delivery at all.
//
// The task is as the store keeps it: a deliver's request comes through Handle, which gives every
// event's text through the store's rule, and the daemon's own delivery of a relaunch's record
// (queueRelaunch) is written from the claim's record, which holds only text the store keeps. An
// operator's task can hold a NUL, which the request holds as the stored delivery does and a
// restart reads back. So the comparison with the pending task, the delivery written and the
// prompt the agent is sent all read the stored text, before a restart as after one.
func (m *Machine) queue(ctx context.Context, request RequestDeliver) error {
	id := request.ID
	queued := m.given(request)
	if pending := m.claim.Pending; pending != nil {
		if (id != "" && pending.ID == id && pending.Task == request.Task) || queued {
			return nil
		}
		switch {
		case request.NewsOnly && HoldsTask(pending):
			// A task of news alone waits behind every task that holds work, confirmed or owed
			// again after a provider error (RequestDeliver.NewsOnly). An untaken task is sent first,
			// as below; a confirmed one is not sent, since its turn is running.
			return m.refuseBehindPending(ctx)
		case pending.ConfirmedAt.IsZero():
			owed, attempt := pending.ID, pending.ProviderErrorRetries
			replaced, err := m.replaceOwedTask(ctx)
			if err != nil {
				return err
			}
			switch {
			case replaced && relaunchTask(owed):
				m.log.Info("supervise: a newer task replaces the daemon's delivery of the relaunch's record, which no prompt waits on; it carries the record's sentence in its place",
					"replaced", owed, "delivery", id)
			case replaced:
				m.log.Info("supervise: a newer task replaces the one a provider error took back",
					"owed", owed, "attempt", attempt, "delivery", id)
			case m.supersedes(*pending, request):
				// supersedes decided which of its two rules holds; the line names that one.
				why := "supervise: the root architect's unconfirmed task of an earlier run or entry is superseded by the newer entry's task, which takes its place"
				if pending.NewsOnly {
					why = "supervise: the untaken task of news alone gives way to the newer entry's task, which carries that news itself"
				}
				m.log.Info(why,
					"superseded", pending.ID, "supersededGeneration", pending.Generation, "supersededEntry", pending.Entry,
					"delivery", id, "generation", request.Generation, "entry", request.Entry)
				if err := m.dropPending(ctx); err != nil {
					return err
				}
			default:
				return m.refuseBehindPending(ctx)
			}
		default:
			m.log.Info("supervise: the pending task was taken by a turn, so the newer task takes its place",
				"taken", pending.ID, "delivery", id, "phase", string(request.Phase), "generation", request.Generation)
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
		News: request.News, NewsOnly: request.NewsOnly, Entry: request.Entry, CheckLook: request.CheckLook, QueuedAt: m.deps.Clock.Now(),
		RoundNamed: request.RoundNamed, Branch: request.Branch}
	if request.Row > 0 || request.Entry > 0 {
		// The claim records the row, the entry and its handshake only once the store has them
		// with the delivery: a write that failed leaves the row undelivered and the entry ungiven,
		// so its retry queues the task.
		stored := m.stored()
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
		m.claim.DeliveredRows, m.claim.GivenEntry, m.claim.Handshake = stored.DeliveredRows, stored.GivenEntry, stored.Handshake
	} else if err := m.putDelivery(ctx, d); err != nil {
		return err
	}
	m.claim.Pending, m.unsaved = &d, false
	return nil
}

// refuseBehindPending is queue's refusal of a task that waits behind the claim's pending one: the
// pending task is sent where a send can go now (sendPending), and the request is refused
// ErrDeliveryPending, which keeps a start's row on its backoff until the claim can take its task.
func (m *Machine) refuseBehindPending(ctx context.Context) error {
	if err := m.sendPending(ctx); err != nil {
		return err
	}
	return &RefusedError{State: m.claim.State, Request: "deliver", Err: ErrDeliveryPending}
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
// has not confirmed, by one of two rules. Each asks first that request carries an entry
// (RequestDeliver.Entry) and is no work the claim was given already (given, which queues nothing
// and so takes nobody's place).
//
// The first holds for every role: p is a task of news of the approved design alone
// (Delivery.NewsOnly). It holds no work, only the news, and the start of an entry carries that
// same news itself: the start executor reads what the record owes the claim when the start runs
// (the daemon's newsForStart), and p's news is still owed then, since a task no turn confirmed
// holds no credit (the claim store counts only a confirmed row's copy, design_news_told_in), and
// the send renders it again from the record (currentNews). So the entry's task goes first and
// alone, and the work the role is owed never waits a turn and a backoff behind news. Kept in front
// of it, p would also hold a root architect's entry out of the first stall arm, which reads only a
// task of the row's entry (admit's architectStalls). A start of no entry carries no news (the
// daemon's startNewsOf), so p is not superseded by it: queue refuses it behind p and sends p
// first, as behind any untaken task, and the reconcile tells p's news where no later start of an
// entry does.
//
// The second is the rule only a tree's root architect is held to (queue): request carries a run,
// is no resend of an entry (RequestDeliver.ResendEntry), and p is a task of a run that is either
// earlier than request's or the same run at a lower entry (Delivery.SupersededBy). A task of no
// run, an operator's own and the daemon's delivery of a relaunch's record among them, is never
// superseded by it, and neither is a task of no entry, a wake's task (workflow's wakeRow) among
// them: queue refuses request behind it, unless it is one a newer task replaces whatever it
// carries (replaceable, asked before this), and StartedBy sends it. A request of no entry
// supersedes nothing. Two places read it, so each rule is decided once: queue, which retires p and
// queues request in its place, and StartedBy, which does not send p first when the start's own
// task is such a request.
//
// It answers false while anything of p is in the agent's hands: a send of it in flight
// (Machine.send), or a prompt of it Oh My Pi acknowledged that still waits on its result
// (awaitsResult). That prompt's turn is the session's next, and a newer task put in p's place
// would be confirmed by p's turn and retired by its end, unsent (replaceable says the same of the
// relaunch's record).
//
// A superseded p ends no credit, since it holds none: its retirement removes a row no turn
// confirmed, and the claim store counts only a confirmed row's copy as a credit
// (design_news_told_in). So what p carried of the approved design stays owed on the told rows,
// whatever became of p before — never sent, sent and acknowledged by a daemon before a restart, or
// confirmed by a turn whose confirmation was then taken back — and the task that takes its place,
// or the reconcile, tells it.
func (m *Machine) supersedes(p Delivery, request RequestDeliver) bool {
	if !p.ConfirmedAt.IsZero() || m.send != nil || m.awaitsResult() {
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
// starts after it, so the hold does too until the round ends (dispatch://INFRA-2981 R5, R9).
// sendPending hands such a claim nothing, and NewMachine reads it through openRoundHoldsTask.
func (c Claim) answeredOpenRound() bool {
	return c.UpdateRound != "" && c.UpdateSignalledAt != nil
}

// openRoundHoldsTask is whether the claim's open round holds the task the claim has pending: the
// hold above, over a task that is not an ask of this round (updatesRound). This daemon queues no
// ask of a round as a task: it publishes the ask to the Envoy topic of the claim it asks
// (record.UpdateAsk.Claim). A task under an `update:` delivery id (updateTaskPrefix) is one an
// older daemon queued and left in the store. An ask of this round is excepted, because that
// daemon may have sent it before the worker answered, so the round's hold is not what keeps it
// from the worker: the restore asks whether the agent holds it (askFirst), and no send of this
// daemon carries it (sendPending drops it, and the round's end takes it back). An older daemon's
// ask of another round, on a claim that answered its current round, is not excepted: the answered
// round holds it, and the sendPending that runs after the round ends drops it. It is built from
// sendPending's own hold rather than written out again, so the restore that reads it
// (NewMachine) can never judge a task held that sendPending would send, or the other way about.
func (c Claim) openRoundHoldsTask() bool {
	return c.answeredOpenRound() && !updatesRound(c.Pending, c.UpdateRound)
}

// sendPending sends the pending delivery if everything a send needs is true now: the claim is
// ready or idle, the delivery is neither confirmed nor already on its way, the issue is still in
// the phase the task was queued for, a task carrying news of the approved design still has some
// to say (currentNews, which renders it again from the record), the root of a root architect's
// task of a run has an architect to hear it, the claim's connection is registered, and — for a
// task that adopts the working copy the issue's roles share (adopts) — no other claim of the
// issue is in a turn or has a prompt on its way to one. A missing connection is not a failure —
// the shim's next hello sends it. A delivery an earlier daemon may have sent is sent again only
// after the agent says it is not already in a turn; if it is, that turn is taken as the
// delivery's — except for a root architect's task of a run, which may be a task nobody has sent at
// all (asksRoot), and a task carrying news of the approved design that no acknowledged prompt
// carried (unsentNews): no turn of the agent's own is ever taken for either (askFirstNeverTakes).
//
// Four states hand the claim nothing and hold the task instead of dropping it: a claim holding a
// suspension (holdSuspension), a claim that has answered its open update round
// (answeredOpenRound, opened by openPodUpdate), a claim whose launch waits its turn in the
// wake pacing over the process this send would reach (launchWaits), and a claim whose plugin is
// stale enough that this daemon refuses its every `legion` call (staleRefusesClaim). Starting
// the worker of an answered round on a task it is about to lose would waste both the turn and
// the work, and so would starting the worker of a process the next turn replaces: that launch
// lets the process go (launchNow), and the task it had just begun would be retired as served at
// the turn it never finished (settle, retirePending). Such a claim is decided as a claim whose
// launch is in flight, which is what it is — the gate left its state alone only so that a
// restart would not find a live process recorded launching — so its deliveries wait here exactly
// as a launching claim's are queued (deliverLater), and go at the ready of the process the
// launch starts.
// PodUpdateEnded releases the round's held task once the round is over (endPodUpdate), and the
// wake's turn is where the launch takes the other back.
//
// The fourth is the same waste a wake earlier: a stale claim's relaunch is often not even
// reserved yet — the refresh gate holds the lane to one minute of the pacing, so a fleet's
// worth of claims wait sweeps for their credit (dispatch://INFRA-2712 R1, R2) — and through
// all of it the daemon answers that claim's own routes 409 (internal/api's staleClaim). A task
// sent there buys a model turn the worker can finish nothing in, and worse, it puts the claim
// in a turn, which is exactly what postpones the relaunch it is waiting for (refreshPlugin's
// holdStaleRelaunch). So the task waits, and it goes when the replacement process registers on
// this daemon's contract and says it is ready, where every task held for a launch goes
// (finishReady, table.go). Nothing else is needed to release it: the registration records the
// daemon's own contract, which takes the claim out of the set this hold reads.
//
// One task never waits on that third hold: an operator's own, which runs the launch the pacing
// holds before the row that queues it is reached (runParkedLaunchNow). A person is waiting on
// that task and the paced turn is tens of minutes away in a wave, so the claim this sees is one
// whose launch has run: it is launching over the process that launch let go, and the task is
// queued for the process it started, which is where every task held here goes. A task the claim
// refuses is no such task — a claim already holding a delivery no provider error took back
// answers it "a delivery is already pending" and no launch runs for it (queue) — and the send
// this refusal makes first reaches the hold with the launch still waiting, so the task the claim
// is holding stays for that turn.
//
// It waits on the fourth all the same, and that is the spec as written (dispatch://INFRA-2712
// P6): a stale claim whose credit the gate has not granted holds no parked launch for that task
// to run, and the process it would reach refuses the operator's task every `legion` call it
// could be worked with. So such a task waits for the credit and then for the replacement
// process's ready, which is the same wait every other task of that claim is already in.
//
// This daemon queues no ask of an update round as a task: it publishes the ask to the Envoy topic
// of the claim it asks (record.UpdateAsk.Claim). A task under an `update:` delivery id
// (updateTaskPrefix) is one an older daemon queued, and a claim restored from the store can hold
// one. Such a task is dropped here and never sent, whichever round it names, the round the claim
// is in now included.
// Its text is the older daemon's, and that text can tell the worker that the daemon reads the end
// of the turn as its answer. This daemon reads no end of a turn as an answer: only the worker's
// ready_for_update call answers a round. A worker that read that text would end its turn and
// take the end for its answer, so this daemon never sends it; a worker that read it before the
// deploy gets the round's ask on its claim's Envoy topic as well. A claim that has answered its
// open round reaches no drop here (answeredOpenRound): the task waits for the round's end, which
// takes back an ask of that round (dropUpdateTask), and the send after the end drops an ask of any
// other round here.
// The delivery id names the round, and keeps it across every take-back (rotateTaskID), so an ask
// whose turn a death cut off comes back still an ask and is dropped here rather than sent again.
// That drop and the phase check's are one shape below, and both leave through the answer every
// drop of a delivery gives the mark armed for it (answerAskFirstAfterDrop).
//
// A third drop takes the same shape: the daemon's own delivery of a relaunch's record
// (Delivery.Relaunch, queueRelaunch) whose record no longer tells. Its text is that record's
// sentence and nothing else, so once the record is told it has nothing to say. The claim holds
// one in that state when its session is abandoned before the delivery's turn (forgetSession), and
// sending it would tell the fresh agent the old session's record in the text it was queued with.
// So does a fourth: a task queued for news of the approved design alone that has nothing left to
// say once its news is read again from the record (currentNews).
//
// A drop that leaves the claim holding nothing while its record of the last let-go still tells
// gives it the daemon's own delivery of that record and sends it (relaunchAfterDrop): the ready
// gives that delivery only to a claim that holds nothing then (finishReady, queueRelaunch), so a
// held task dropped at the ready's own send would otherwise leave the sentence waiting for
// whatever task comes next.
//
// Before all of it, a claim the project's pause holds is handed nothing at all (decision 8 of
// dispatch://INFRA-3759's plan): while the project is paused, no task leaves for any role but the
// controller (pauseHolds). The task stays pending, decided nothing about and dropped for nothing,
// and the resume's event sends it once the pause is over (ResumePause).
func (m *Machine) sendPending(ctx context.Context) error {
	if _, holds := m.pauseHolds(); holds {
		return nil
	}
	// A claim holding a suspension is handed nothing (holdSuspension), and neither is one whose
	// paced launch is about to replace the process a send would reach (launchWaits).
	if state := m.claim.State; (state != StateReady && state != StateIdle) || m.held != nil || m.launchWaits() {
		return nil
	}
	// A claim that answered its open update round is handed nothing until the round ends.
	if m.claim.answeredOpenRound() {
		return nil
	}
	p := m.claim.Pending
	if p == nil || !p.ConfirmedAt.IsZero() || m.send != nil {
		return nil
	}
	// Nor is a claim whose every `legion` call this daemon refuses for a stale plugin handed its
	// task: it waits for the process the refresh lane's relaunch starts (staleRefusesClaim). It is
	// asked below the three above and below the question of whether there is a task at all,
	// because it is the costliest of the four — Deps.NoPluginInstall reads the daemon's
	// configuration — and every send of a claim holding nothing would otherwise pay for it.
	if m.staleRefusesClaim() {
		return nil
	}
	if _, awaiting := m.timers[TimerTurn]; awaiting {
		return nil
	}
	// The ladder's wait a provider error arms is the delivery's own, so it is fenced on the id
	// the arming recorded (waitsForProviderError): the newer task that takes the place of the
	// one the wait is for (queue) is a task no wait names, so it goes at once rather than
	// waiting out a ladder step that was counted against the task it replaced.
	if m.waitsForProviderError(p.ID) {
		return nil
	}
	conn, ok := m.deps.Conns.Conn(m.claim.Token)
	if !ok {
		m.log.Info("supervise: no connection; the delivery waits for the shim's hello", "delivery", p.ID)
		return nil
	}
	// Every reason to drop the delivery rather than send it is decided here and leaves through
	// the one exit below, because a dropped delivery leaves no mark armed for it, here or at
	// any other drop (answerAskFirstAfterDrop): a drop that returned of its own would
	// leave the question a restart armed for that delivery standing (askFirst), and the next
	// task would be confirmed by whatever turn is running and retired unsent. Nothing here
	// sends, so an older daemon's ask of an update round is dropped before the connection
	// carries anything of it to the worker. The one drop that leaves elsewhere is a root
	// architect's held task that names nothing any more (asksRoot's branch below,
	// currentRootTask): that branch clears askFirst before anything else, so it leaves no
	// question armed and asks none. Each drop ends with the claim given the delivery of its
	// record of the last let-go when that record still tells (relaunchAfterDrop).
	//
	// holds is whether the delivery is still the claim's to send. An older daemon's ask of an
	// update round never is, whichever round it names, and its issue's phase is not asked about.
	// Nor is the daemon's own delivery of a relaunch's record whose record no longer tells
	// (spent), whose sentence is all it carries; it names no phase either. says is whether a task
	// that still holds has anything to say: a task queued for news of the approved design alone
	// can have nothing left once the record is read again (currentNews), which is asked only of a
	// task that holds, so a task its phase has left is dropped without a read of its news.
	round, asks := updateRoundOf(p.ID)
	spent := p.Relaunch() && !m.claim.EndedWork.Tells()
	holds, says := !asks && !spent, true
	if holds {
		var err error
		if holds, err = m.phaseHolds(ctx, *p); err != nil {
			return err
		}
	}
	if holds {
		var err error
		if says, err = m.currentNews(ctx); err != nil {
			return err
		}
	}
	if !holds || !says {
		switch {
		case asks:
			m.log.Info("supervise: this task is an ask an older daemon queued for an update round; it is dropped and never sent",
				"delivery", p.ID, "asked", round, "round", m.claim.UpdateRound, "left", m.claim.UpdateLeftRound)
		case spent:
			m.log.Info("supervise: the daemon's own delivery of a relaunch's record has nothing to say, since that record no longer tells; it is dropped rather than sent",
				"delivery", p.ID, "cause", m.claim.EndedWork.Cause, "ended", m.claim.EndedWork.Generation, "toldBy", m.claim.EndedWork.ToldBy)
		case !holds:
			m.log.Info("supervise: the issue has left the phase this task was queued for; it is dropped rather than sent",
				"delivery", p.ID, "issue", m.claim.Issue, "queuedFor", p.Phase)
		default:
			m.log.Info("supervise: this task was queued for news of the approved design and none of it stands now; it is retired rather than sent",
				"delivery", p.ID, "issue", m.claim.Issue)
		}
		if err := m.retirePending(ctx); err != nil {
			return err
		}
		if err := m.answerAskFirstAfterDrop(ctx); err != nil {
			return err
		}
		return m.relaunchAfterDrop(ctx)
	}
	// Nothing is sent to a process under a session its claim does not hold (holdRecorded, plan E's
	// t-e-54): a claim the boot could not suspend, its stop refused, keeps its live process, and a
	// task sent to it would put that process to work under another claim's session, or one no claim
	// holds (review round 12 of #4220). The task waits, unsent and charged nothing, and the next
	// decision asks again, so an operator's settle of the name sends it. A store that does not
	// answer sends nothing either. For a claim holding its session this asks the store once per
	// session (Machine.heldNames). The check comes before both sends below: a root architect's task
	// of a run (asksRoot), whose narrowing (currentRootTask) waits on it too, and every other task, a
	// task that carries news and a task of news alone among them. The news render above
	// (currentNews) rewrites the delivery and sends nothing.
	if err := m.holdRecorded(ctx, m.claim.Session, m.claim.SessionFile); err != nil {
		var refusal claim.Refusal
		switch {
		case !errors.As(err, &refusal):
			m.log.Warn("supervise: the store did not answer whether this claim holds its session; the task waits", "delivery", p.ID, "error", err)
		case m.unheldDelivery != p.ID:
			m.unheldDelivery = p.ID
			m.log.Error("supervise: this claim records a session it does not hold, so no task is sent to its process; the task waits until the claim holds it: an operator gives the claim a new session, or records it as the rightful holder of a name held for none",
				"delivery", p.ID, "session", m.claim.Session, "error", err)
		}
		return nil
	}
	// A root architect's task of a run (asksRoot) is one of the two deliveries the question a
	// restart left never answers (the other is below): such a task may be one nobody has ever
	// sent — it is held on the claim while its root has no architect to hear it, across restarts —
	// and the decisions it names are held by this delivery and told at its confirmation, so a turn
	// of the agent's own taken for this task's would confirm it unsent and tell a person's decision
	// to nobody. The question is resolved here rather than asked: while the root does not hear,
	// the task stays pending and unconfirmed and nothing is sent; once the root hears, it is sent.
	// A send can meet an agent still in a turn of its own, which answers it busy and leaves the
	// task waiting, charged nothing, with the claim recorded working for that turn (refused,
	// recordBusyTurn), and the turn's end sends it again: sending again is the lesser fault of the
	// two. Its phase check always holds — such a task names no phase — so a dropped task is never
	// this one.
	if m.asksRoot(*p) {
		m.askFirst = false
		hears, err := m.rootHears(ctx, *p)
		if err != nil {
			return err
		}
		if !hears {
			m.log.Info("supervise: the root of this task's tree has no architect to hear it; the task stays pending",
				"delivery", p.ID, "issue", m.claim.Issue, "run", p.Generation)
			return nil
		}
		sends, err := m.currentRootTask(ctx)
		if err != nil {
			return err
		}
		if !sends {
			return m.relaunchAfterDrop(ctx)
		}
		m.startSend(conn, *m.claim.Pending)
		return nil
	}
	// The question a restart left — whether the turn the agent may be in is the pending
	// delivery's — is asked once, here. Left armed, it would make the next task the running
	// turn's: confirmed, never prompted. A turn of the agent's own moves the claim to working and
	// confirms the delivery the earlier daemon sent, which is the turn's own task.
	// An agent that cannot say is taken as not streaming here: sending under the same id is what
	// the shim's dedupe and the next agent_start make safe.
	//
	// A task carrying news of the approved design that no acknowledged prompt carried (unsentNews)
	// is resolved here rather than asked, as a root architect's task of a run is above: no earlier
	// daemon's prompt of it reached the agent, so the turn the agent is in is not its turn, and its
	// confirmation would record news no prompt carried as taken. It is sent. An agent in a turn of
	// its own answers busy, which leaves it waiting, charged nothing, with the claim recorded
	// working for that turn (refused, recordBusyTurn), and the turn's end sends it again. A task an
	// earlier daemon's prompt did carry is asked about, and the agent's turn confirms it with what
	// that prompt carried (turnAnswers: the delivery's copy, Delivery.Carried), never with the news
	// the send above rendered again.
	askAgent := m.askFirst && !unsentNews(*p)
	m.askFirst = false
	if askAgent {
		if streaming, _ := m.streaming(ctx, conn); streaming {
			// The agent's answer is the daemon's first word of the turn, so the turn is dated
			// from it (Claim.LastTurnStart), as recordBusyTurn dates a turn from a busy answer,
			// and the confirmation's write carries the date: a let-go in this turn reads a report
			// older than it as no report of the turn (recordEnd), where the start of the turn
			// before it would count the report that turn's end left as one of this turn.
			m.claim.State, m.claim.LastTurnStart = StateWorking, m.deps.Clock.Now()
			// The turn this claim is put in is logged before the write that records it, as
			// turnStarted logs its own: a line written after the write would say nothing of the
			// turn whose write failed.
			m.startTurn(p.ID)
			m.log.Info("supervise: the agent is in a turn; confirming the delivery an earlier daemon sent", "delivery", p.ID)
			news, _ := m.turnAnswers()
			return m.confirmCarrying(ctx, news)
		}
	}
	// Every role that works one working copy works one jj workspace — the issue's own, which
	// every role of an issue with no plan rows and of the task that holds the issue's branch
	// works, or the working copy of a task's own branch (Delivery.Branch) — so a task that adopts
	// a working copy adopts it for every role working there: an adoption run while another role
	// is in a turn there moves that role's worker onto a fresh commit mid-write, and the work it
	// goes on to do lands authored by this role — a review App's adoption took the implementer's
	// next commit that way (dispatch://INFRA-714). The task waits for that turn instead. A turn in
	// another task's working copy writes nothing in this one, so it holds nothing: the reviewer of
	// one task is sent its task while the implementer works the step of another. A turn of no
	// known working copy (ClaimView.Copy) — a message, an operator's task, the agent's own turn —
	// may work any of them, and holds them all. Nothing is written and nothing moves: the delivery
	// is left exactly as it was, and the hold is decided again, from the claims as they stand then,
	// at the next trigger — the turn's end elsewhere on the issue, which the daemon posts here
	// (IssueTurnEnded), the sweep's observation, the shim's hello, this claim's own ready. That is
	// what keeps the task from being stranded by a turn that never ends: a claim whose process
	// died, that was suspended, or whose issue closed is not in a turn any more, so the next
	// trigger sends the task. It cannot be sent twice either, because every one of those triggers
	// comes back through here, where a delivery already on its way (m.send), one waiting for its
	// turn (TimerTurn), and a confirmed one each stop the send.
	//
	// The hold stays a check followed by an act, and counting a send in flight narrows that
	// window rather than closing it: this machine holds its own lock across both halves, but the
	// claim it asked about is another machine, free to start a send of its own the instant after
	// it answered. What changes is how long such a send stays invisible. It used to be invisible
	// for the whole of itself — an adoption of the working copy and a prompt RPC, seconds of it
	// — because only StateWorking counted and no turn had started yet, so the miss was the rule
	// rather than a race. Now it is invisible only from where startSend records it to where that
	// machine's decision publishes (ClaimView.SendInFlight), a tail of its own locked section
	// with no agent round trip in it. Two sends that start inside each other's tail are what
	// remains, and no answer read outside the other machine's lock can rule that out.
	if m.adopts(*p) {
		if other, inTurn := m.issueInTurn(p.Branch); inTurn {
			m.log.Info("supervise: a claim of this issue is in a turn; the phase task waits rather than adopt the working copy they share",
				"delivery", p.ID, "issue", m.claim.Issue, "branch", p.Branch, "inTurn", other)
			return nil
		}
	}
	m.startSend(conn, *p)
	return nil
}

// asksRoot says whether a delivery's send waits on whether the root of this claim's tree has an
// architect to hear it (rootHears): a root architect's task of a run — it names no phase and a
// run, and the claim is its tree's root — on a daemon with a workflow to answer for it. It is also
// one of the two deliveries the question a restart left cannot confirm by itself
// (askFirstNeverTakes, table.go's turnStarted): a task nobody has sent is no task in flight.
func (m *Machine) asksRoot(d Delivery) bool {
	return m.deps.RootHears != nil && d.Phase == "" && d.Generation != 0 && m.claim.treeRoot()
}

// unsentNews says whether d carries news of the approved design (Delivery.News) that no
// acknowledged prompt carried (Delivery.Carried). The write that records its confirmation makes
// what the prompt its turn answers carried the delivery's credit, its items and the opening it
// carried (the claim store reads the copy a confirmed row holds, design_news_told_in, and folds it
// once the turn's result says it ran, settleDesignNews), so its confirmation must come from a turn
// that such a prompt started. The mark is the copy, not Delivery.Prompted: Prompted is set as the
// send goes out, before the send adopts the working copy and mints its identity, so a send whose
// adoption was refused (runtime.ErrAdoptRefused) or whose mint failed
// (runtime.ErrIdentityUnavailable) is Prompted with no prompt ever sent, where the copy is written
// only at Oh My Pi's acknowledgement (acked). A restart that left such a task pending knows that no
// earlier daemon's prompt of it reached the agent, so the question that restart left (askFirst)
// never takes a turn of the agent's own for it, at the send (sendPending) or at a turn's start
// (beginTurn). The copy is persisted at that acknowledgement, so a prompt acknowledged just before
// a crash reads as never sent and the news is sent a second time: of the two faults, that is the
// one that loses nothing.
func unsentNews(d Delivery) bool { return len(d.News) > 0 && len(d.Carried) == 0 }

// askFirstNeverTakes says whether a turn the agent started by itself is never taken for d's turn
// by the question a restart left (askFirst): a root architect's task of a run (asksRoot), which
// may be a task nobody has sent, and a task carrying news no acknowledged prompt carried
// (unsentNews). The confirmation of either tells something — a person's decisions, or the news —
// so only a turn a send of this machine's own started may confirm it.
func (m *Machine) askFirstNeverTakes(d Delivery) bool { return m.asksRoot(d) || unsentNews(d) }

// rootHears asks whether the root of this claim's tree has an architect to hear the task now
// (Deps.RootHears). Only a root architect's task of a run is put to the answer (asksRoot): it names
// no phase, so the phase check never drops it, and the decisions it names are told at its own
// confirmation, so a prompt while the tree is parked, lingering or waiting for its slot would tell
// them to an architect no run of the root owns. Every other delivery — a phase worker's, an
// operator's, a production check's look, the controller's start message — is sent as it always was,
// and a daemon with no workflow configured supplies no answer, where every delivery is sent.
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
// whether there is still a task to send. Such a task names decisions this delivery holds, and an
// issue one of them was decided on can have left this claim's tree while the task waited: the tree
// it stands in now owes the person that answer, so the answer drops it from the task, releases
// this delivery's hold on it and writes that tree's architect a catch-up naming it. This is where
// the task stops naming it, because it is the machine that owns the pending delivery: the
// hand-over commits in a transaction of the admission's own, with no lock on this claim, and a
// task rewritten there would be overwritten by the machine's next write of what memory still held.
//
// A task left naming nothing is retired rather than sent: there is nothing to tell this architect,
// and the start row that queued it is recorded on the claim (Claim.DeliveredRows), so no retry
// gives it again. A task that carries news of the approved design is narrowed in the start's own
// text, which its news follows, and the answer renders both again through the one render a send of
// news goes through, so the news it carries is exactly what its text names and no later send
// renders a released decision from the text the start wrote. The id is kept across a rewrite of
// the text and the decisions alone: it is this delivery, under the row that queued it, with less in
// it than when it was queued, and the prompt that would be deduped against it carried more rather
// than less. A rewrite that changes the news takes a new id, as currentNews's does (renderedID). The
// narrowed task is persisted before it is sent, so a restart reads the same task this send carried.
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
		m.log.Info("supervise: everything this held task named is owed to another tree now; it is retired rather than sent",
			"delivery", p.ID, "issue", m.claim.Issue, "run", p.Generation)
		return false, m.retirePending(ctx)
	}
	was := p.ID
	id := renderedID(was, news, p.News)
	if err := m.rewritePending(ctx, id, task, kept, news); err != nil {
		return false, err
	}
	m.log.Info("supervise: a decision this held task named is owed to another tree now; the task is sent without it",
		"delivery", id, "was", was, "issue", m.claim.Issue, "run", p.Generation)
	return true, nil
}

// renderedID is the id a held task takes when a send rewrites it with news, held being the news
// it carried under was: the same id where the news is the same, and a new one (rotateTaskID) where
// it is not. A delivery id names one rendering of the news, and never two (currentNews says why).
func renderedID(was string, news, held []byte) string {
	if bytes.Equal(news, held) {
		return was
	}
	return rotateTaskID(was)
}

// rewritePending writes the pending delivery back under id with task as its text, and kept and
// news as the decisions and the news it carries, then holds that delivery in memory: the one
// persist-and-swap of a held task read again at its send (currentRootTask, currentNews), each of
// which keeps its own rule for when to rewrite and when to retire. Its callers render the task's
// text and its news together, so news is always what task names. The write comes first, so a
// restart reads the task the send that follows carries, and a failed write leaves memory as the
// record holds it.
func (m *Machine) rewritePending(ctx context.Context, id, task string, kept, news []byte) error {
	next := *m.claim.Pending
	next.ID, next.Task, next.Kept, next.News = id, task, kept, news
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
// delivery's confirmation can tell it first. So the text is rendered again here, where the machine
// owns the pending delivery, at every send — a send refused busy is rendered again when it is sent
// again — and what the answer no longer carries goes from the delivery as well as from the text,
// so the next prompt carries what the record owes now.
//
// What this renders is not what a confirmation credits. The confirmation credits what the prompt
// its turn answers carried (Delivery.Carried, turnAnswers), written as that prompt is
// acknowledged: a render here that adds an opening, before the restart's question is asked or for
// a resend Oh My Pi answers busy, adds nothing to the copy of a prompt that went out before it.
//
// A rewrite that changes the news takes a new id (renderedID), so a delivery id names one
// rendering of the news and never two. The acknowledgement of a prompt is what records the news it
// carried (acked), and the shim answers a send under a delivery id it already holds from its own
// record, without forwarding it to Oh My Pi (shimwire's Dedupe): the acknowledgement of such a
// resend is the first send's, and the agent read the first send's text. A resend under the same id
// carrying a new rendering would record news the agent never read — after a restart whose question
// finds the agent out of the turn, or after the answer to the first send was lost on the way back
// and the reconnect's hello sends the task again (refused keeps the id for that dedupe). Under a new
// id the shim forwards the new rendering, so Oh My Pi either takes it or answers it busy, and what
// is recorded is what it took. Keeping the first send's news for the id instead is not enough: the
// daemon learns what a send carried only from its acknowledgement, and a lost acknowledgement leaves
// it nothing to keep. The cost of the new id is a prompt sent twice where the earlier one's turn
// still runs or already ran: told twice, never not at all. A restart's question that finds the
// agent in the earlier prompt's turn confirms the task under the new id with the copy that prompt
// carried, and that prompt's result is then fenced as another id's, so its turn's end ends that
// credit with the delivery (the claim store's RetireDelivery) and a later task tells it again. A
// rewrite of the text alone keeps the id, as currentRootTask's does: what a confirmation credits is
// the news.
//
// A task queued for its news alone that is left with nothing to say is not sent, and the answer
// is false: sendPending retires it through the one exit every drop leaves by, which answers the
// question a restart left for it (answerAskFirstAfterDrop) and gives a claim that drop leaves
// holding nothing the delivery of its untold record of the last let-go (relaunchAfterDrop). The
// rewritten task is persisted before it is sent, so a restart reads the task this send carried. A
// task that carries no news is never asked about, and every task holds where nothing answers
// (Deps.DesignNews nil).
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
		return false, nil
	}
	if task == p.Task && bytes.Equal(news, p.News) {
		return true, nil
	}
	was := p.ID
	id := renderedID(was, news, p.News)
	if err := m.rewritePending(ctx, id, task, p.Kept, news); err != nil {
		return false, err
	}
	m.log.Info("supervise: the news this task carries was read again from the record; the task is sent as it reads now",
		"delivery", id, "was", was, "issue", m.claim.Issue)
	return true, nil
}

// answerAskFirstAfterDrop answers the question a restart armed for a delivery that is now gone
// (askFirst), and every drop of a delivery runs it in the same step as the drop: sendPending's
// four — the task its issue's phase has left behind, an older daemon's ask of an update round,
// the daemon's own delivery of a relaunch's record that no longer tells, and the task queued for
// news of which nothing stands now (currentNews) — the retirement's (machine.go's retire), and
// dropUpdateTask, which makes it of its own for the three drops a round that is over makes
// through it (leaveRound, endPodUpdate, openPodUpdate's round switch). The one drop that does not
// run it is currentRootTask's retirement of a root architect's held task that names nothing any
// more: sendPending's asksRoot branch clears askFirst before it, without asking, so no question
// is left to answer. Left armed over nothing pending, the question would make the claim's next
// task the running turn's — confirmed by a turn of another cause, never prompted, and retired
// unsent by that turn's end — and the worker would never receive that task at all.
//
// It asks what a send would have asked, and confirms nothing: no delivery is left to confirm, so
// an agent the ask finds in a turn only moves the claim to working, dated from the answer
// (Claim.LastTurnStart), and the claim is written. The ask is worth making with nothing pending
// because the claim's state is the daemon's record of whether its agent is busy, and the drop
// does not make that record current: discarded instead, the claim stays ready while its agent
// works, and the task that follows the dropped one is prompted into a busy agent, whose busy
// answer is what then records the claim working (recordBusyTurn). The turn is nobody's task's
// either way, and the next task waits for it to end.
//
// An agent that cannot say, and a claim whose shim is not connected, are taken as not streaming:
// the mark goes all the same, since it is the dropped delivery's, and one busy answer is what a
// send into a turn this daemon could not read costs, since that answer records the turn.
//
// Two claims are answered nothing. One that still holds a delivery is one no drop left empty: the
// question is that delivery's, and sendPending asks it against that delivery. A working claim's
// question is its turn's rather than its task's — no daemon ever sent the task a drop takes from a
// claim that is not ready or idle — and askTurnOver and turnEnded answer it there, where an agent
// that is not streaming ends the turn rather than leaving a working claim nobody watches.
func (m *Machine) answerAskFirstAfterDrop(ctx context.Context) error {
	if !m.askFirst || m.claim.Pending != nil {
		return nil
	}
	if state := m.claim.State; state != StateReady && state != StateIdle {
		return nil
	}
	m.askFirst = false
	conn, ok := m.deps.Conns.Conn(m.claim.Token)
	if !ok {
		return nil
	}
	if streaming, _ := m.streaming(ctx, conn); !streaming {
		return nil
	}
	m.log.Info("supervise: the agent is in a turn of its own after its task was dropped")
	// The turn is dated from the agent's answer, the daemon's first word of it, as the askFirst
	// branch of sendPending dates the turn it confirms a task by (Claim.LastTurnStart).
	m.claim.State, m.claim.LastTurnStart = StateWorking, m.deps.Clock.Now()
	// The guard above has already established that the claim holds no delivery, so this turn is
	// no task's and the line names none. It goes before the write, as the others do.
	m.startTurn("")
	return m.persist(ctx)
}

// dropPending retires the pending delivery (retirePending) and answers the question a restart
// armed for it (answerAskFirstAfterDrop), in that order, which is what every replacement of a
// pending task by a newer one does: replaceOwedTask for the task a provider error took back and
// for the relaunch's record, and queue for a root architect's task a newer entry supersedes
// (supersedes). The answer comes after the retirement because it asks only of a claim holding
// nothing, so the newer task queued next is never taken for the running turn's.
func (m *Machine) dropPending(ctx context.Context) error {
	if err := m.retirePending(ctx); err != nil {
		return err
	}
	return m.answerAskFirstAfterDrop(ctx)
}

// relaunchAfterDrop gives a claim that one of sendPending's drops left holding nothing the
// daemon's own delivery of its record of the last let-go (queueRelaunch), when that record still
// tells, and sends it. The ready queues that delivery only for a claim that holds nothing when it
// is ready (finishReady), before the send that ready makes; a held task that send then drops —
// its phase gone, an older daemon's ask of an update round, a task queued for news of the
// approved design alone with nothing left to say (currentNews), or a root architect's held task
// left naming nothing (currentRootTask) — leaves the claim holding nothing, and the sentence
// would wait for whatever task came next.
//
// It runs after the drop's retirement and after the answer to the question a restart armed for
// the dropped delivery (answerAskFirstAfterDrop), which asks only of a claim holding nothing, so
// that question is answered for the dropped delivery and never taken for the new one's. Nothing
// here recurses without end. queue sends nothing for a claim that holds no delivery, and the send
// below drops nothing it queues: the delivery has no phase, no round, no run and no news, so
// neither the phase check, the round's drops nor the read of its news drops it, it asks no root
// (asksRoot), and it is queued only while its record tells, so it is not spent either. A record
// that does not tell queues nothing, and the send then finds nothing to send.
//
// One order costs a turn and loses nothing. A newer task that queue refuses because the claim
// holds an untaken task runs sendPending first (queue), and when that send drops the held task
// the delivery queued here takes the slot and goes at once. The newer task, retried by its row,
// then waits for that delivery's turn, as it would behind any prompt still outstanding, rather than
// carrying the sentence in front of itself. The agent reads the sentence alone in that turn and
// the newer task in the next one.
func (m *Machine) relaunchAfterDrop(ctx context.Context) error {
	if err := m.queueRelaunch(ctx); err != nil {
		return err
	}
	return m.sendPending(ctx)
}

// phaseHolds asks whether the issue is still in the phase and run this task was queued for, and,
// at the phase step, at the step of this claim's role. A delivery that names no phase is nobody's
// to drop — an operator's own, an architect's — and is never put to the answer at all, so the
// exemption this package documents is this package's own rather than a promise every
// implementation has to keep. A daemon with no workflow configured supplies no answer, and every
// delivery holds.
func (m *Machine) phaseHolds(ctx context.Context, d Delivery) (bool, error) {
	if m.deps.PhaseHolds == nil || d.Phase == "" {
		return true, nil
	}
	holds, err := m.deps.PhaseHolds(ctx, m.claim, d)
	if err != nil {
		return false, fmt.Errorf("read the phase of %s: %w", m.claim.Token, err)
	}
	return holds, nil
}

// retirePending drops the pending delivery, and is the one place a delivery ends: settle calls
// it for a delivery whose life is over, sendPending for a task the issue's phase has left
// behind, for an older daemon's ask of an update round, for the daemon's own delivery of a
// relaunch's record that no longer tells and for a task queued for news of which nothing stands
// now, dropUpdateTask for the ask of a round that is over, retire for the ask the claim ends
// with, and queue for the task a newer one replaces. The claim keeps its state — the task is
// stale, not the agent.
//
// A confirmed task of a run leaves the run behind it, and its phase entry and its check look
// with it: the worker goes on working it in whatever turn comes next — one an Envoy notice
// starts, with no delivery of its own — and its completion belongs to that run, that entry and
// that look. Only a delivery whose turn actually ran moves them, so a confirmation taken back by
// a busy refusal never does, and only a task that carries them: an operator's task carries no
// generation, no entry and no look, a resume that raised no entry and a production check's look
// no entry, and a phase task no look, and each leaves what the claim is serving alone. Every
// retire of a confirmed task of a run writes the row, the second task of one run included: the
// write is unconditional on the value, not a diff. Which value each takes is the claim's own
// accessors, read here off the claim this delivery is still pending on, so what the claim is
// left serving is by construction what ServingRun, ServingEntry and ServingCheck answered while
// that delivery was pending.
//
// A task of an entry the worker never confirmed goes the other way: Claim.GivenEntry is put back
// to Claim.ServedEntry, so the entry stands above what this claim was given again, the admission
// pass reads this role as owed that entry's work (record.PhaseRow.Entry), and the daemon sends
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
// failure it is. The row holds a delivery this machine is not retiring: a write that rotated the
// id (interrupted, takeBackPending) had its answer cut off by the claim-write bound and
// committed all the same, so the store holds the new delivery while memory holds the old. Read
// as a retirement already made, it would drop the delivery memory holds and leave the stored one
// behind, pending, unconfirmed and never retired, with the decisions it keeps never released.
//
// So the decision fails, and the delivery memory holds is marked unsaved on the way out, because
// the refusal says exactly what the row is missing: that delivery. The next settle writes it
// before it retires anything (putClaimAndPending), which puts the row back under memory's own id
// — PutDelivery upserts on the claim token (internal/store's claims.go), so the row the rotation
// left is the one it writes over — and the retirement that follows it, where that settle
// retires at all, then finds the delivery it is of. What the rotated row held of the decisions
// it keeps is held under an id no row carries any more, and a hold whose delivery is not stored
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
func (m *Machine) retirePending(ctx context.Context) error {
	if len(m.owedStops()) > 0 {
		if err := m.putClaimAndPending(ctx); err != nil {
			return m.owes("PauseStopped", err)
		}
	}
	p := m.claim.Pending
	// The ladder's wait is this task's, so it ends with it (providererror.go).
	m.disarm(TimerPromptRetry)
	retired := m.stored()
	if !p.ConfirmedAt.IsZero() {
		retired.ServingGeneration, retired.ServedEntry, retired.ServingCheckLook =
			m.claim.ServingRun(), m.claim.ServingEntry(), m.claim.ServingCheck()
	} else if p.Entry != 0 && p.Entry == m.claim.GivenEntry {
		retired.GivenEntry = retired.ServedEntry
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
	// The prompts the retired task waited on a result for go with it (Machine.outstanding): their
	// results answer a task the claim no longer holds, and the fence drops them, so a newer task
	// queue puts in its place is never taken for the turn one of them runs.
	m.claim, m.unsaved = retired, false
	m.forgetOutstanding()
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
// - confirmCarrying's write covers both, and a delivery written confirmed beside a claim still
// ready is a turn a restart would find over. So once the store takes writes again, a restart reads
// what the machine decided rather than what it replaced; one before that reads the older rows, as
// a restart after a crash between a decision and its write does.
func (m *Machine) saved(err error) error {
	m.unsaved = err != nil
	return err
}

// putPending writes the pending delivery as memory holds it (saved).
func (m *Machine) putPending(ctx context.Context) error {
	return m.saved(m.putDelivery(ctx, *m.claim.Pending))
}

// streaming asks the agent whether a turn is in flight; answered is false when the agent could not
// say (the ask failed or timed out), and each caller decides what an unanswered ask means for it.
func (m *Machine) streaming(ctx context.Context, conn runtime.Conn) (streaming, answered bool) {
	asking, cancel := context.WithTimeout(ctx, m.deps.Timeouts.RPC)
	defer cancel()
	state, err := conn.GetState(asking)
	if err != nil {
		m.log.Warn("supervise: the agent could not say whether it is in a turn", "error", err)
		return false, false
	}
	return state.IsStreaming, true
}

// startSend runs one prompt on its own goroutine and posts its outcome back to the machine. The
// task is marked as put to the agent before the goroutine starts (Delivery.Prompted), whatever
// the agent then answers: from here on, a prompt of it has gone out. Of the claim's two untold
// records, this marks which the prompt carries the sentence of, as message composed it
// (Machine.carried), and tells neither, as confirmCarrying states. Whether the send adopts the
// working copy first is decided here, under the machine's lock, so the goroutine acts on the claim
// the hold in sendPending was decided against.
//
// The news of the approved design this prompt carries is the pending delivery's own (d.News, as
// sendPending's render left it), and nothing keeps a second copy of it: every rendering takes a new
// id (currentNews, currentRootTask) and the fence passes only an outcome of the pending id
// (fenceOutcome), so while this send is the pending delivery's, the pending delivery's News is what
// it carries. Its acknowledgement records that news with the prompt (acked), and a turn taken for
// the task's while the send is in flight credits the same (turnAnswers).
func (m *Machine) startSend(conn runtime.Conn, d Delivery) {
	token, generation, role, loc := m.claim.Token, m.claim.Generation, m.claim.Role, m.claim.Locator
	adopting := m.adopts(d)
	prompt, carried := d.message(m.claim)
	m.carried.abandoned = m.carried.abandoned || carried.abandoned
	m.carried.ended = m.carried.ended || carried.ended
	if p := m.claim.Pending; p != nil && p.ID == d.ID {
		p.Prompted = true
	}
	send := &sending{id: d.ID, generation: generation}
	m.send, m.helloDuringSend = send, false
	if seq := conn.Sequence(); seq > m.sentThrough {
		m.sentThrough = seq
	}
	m.goroutines++
	go func() {
		// The send runs on a goroutine of its own, so its adoption, its prompt and its outcome's
		// decision share one fresh context, one after another.
		working := m.worker()
		var err error
		if adopting {
			err = m.adopt(working, role, loc, &d)
		}
		if err == nil {
			sending, cancel := context.WithTimeout(working, m.deps.Timeouts.RPC)
			err = conn.Prompt(sending, d.ID, prompt)
			cancel()
		}
		var ev Event = PromptAcked{Claim: token, Generation: generation, DeliveryID: d.ID}
		if err != nil {
			ev = PromptRefused{Claim: token, Generation: generation, DeliveryID: d.ID, Err: err}
		}
		if err := m.Handle(working, ev); err != nil {
			m.log.Error("supervise: prompt outcome", "delivery", d.ID, "error", err)
		}
		m.mu.Lock()
		m.goroutines--
		m.idle.Broadcast()
		m.mu.Unlock()
	}()
}

// adopts is whether sending d hands the claim's working copy the role's App identity first
// (adopt): a task of an issue phase, which is the work this role commits for. Every other prompt
// adopts nothing — a message another role sent, a notice, a reminder, an operator's own task, the
// controller's start — because the role is not about to commit for a phase, and the working copy
// is the issue's rather than this claim's: an adoption made for a prompt that writes no phase
// commit only takes the workspace from whichever role is working in it (dispatch://INFRA-714). A
// daemon with no GitHub Apps (Deps.Identity nil) adopts nothing at all, and neither does the
// project's controller, which works no checkout.
func (m *Machine) adopts(d Delivery) bool {
	return m.deps.Identity != nil && m.claim.Role != claim.RoleController && d.Phase != ""
}

// issueInTurn names another claim of this claim's issue whose agent is in a turn now, or whose
// prompt is on its way to one, in the working copy of branch (Delivery.Branch) or in none known,
// when there is such a claim. The answer is the state those claims' machines keep
// (Deps.IssueInTurn reads StateWorking, which a turn's start sets and its end clears, and the send
// in flight and the working copy those machines publish beside it) rather than a second record of
// who is in a turn kept beside them: the machines' own state is the only account that moves with
// everything that ends a turn — the agent's own end of it, a process found dead, a suspension, a
// close — where a record of turn times would have to be told of each of those again.
func (m *Machine) issueInTurn(branch string) (claim.Token, bool) {
	if m.deps.IssueInTurn == nil || m.claim.Issue == "" {
		return "", false
	}
	return m.deps.IssueInTurn(m.claim.Issue, m.claim.Token, branch)
}

// adopt hands the working copy the role's App identity before the phase task d reaches the agent,
// so the task's commits are authored by the bot. A failure keeps the task from being sent. Who
// adopts, and when, is adopts and sendPending's hold.
//
// A mint that failed is marked as what it is (runtime.ErrIdentityUnavailable) rather than left to
// read as the transport's silence: it is raised here, before any frame leaves the daemon, and the
// send's refusal is logged by that name (table.go's refused).
//
// An adoption the shim answers ok is logged here, once, at Info: the task, its phase, the App's git
// name — never a token — and how long the call to adopt took (Runtime.AdoptWorkingCopy): the
// runtime's own check of where the claim's process runs, then the round trip to the shim and back,
// with the shim's own work inside it. The refusals are logged where the send's outcome is
// (refused); without this line nothing in the log tells which tasks adopted the working copy,
// which is what a production check reads to see the rule held (dispatch://INFRA-2695).
func (m *Machine) adopt(ctx context.Context, role claim.Role, loc *runtime.Locator, d *Delivery) error {
	if loc == nil {
		return fmt.Errorf("adopt the working copy of %s: the claim has no locator", m.claim.Token)
	}
	id, err := m.deps.Identity(ctx, role, m.claim.Issue)
	if err != nil {
		return fmt.Errorf("%w: the git identity of %s: %w", runtime.ErrIdentityUnavailable, m.claim.Token, err)
	}
	started := m.deps.Clock.Now()
	if err := m.deps.Runtime.AdoptWorkingCopy(ctx, *loc, id, d.Branch); err != nil {
		return err
	}
	m.log.Info("supervise: the working copy was adopted for the phase task",
		"delivery", d.ID, "phase", string(d.Phase), "branch", d.Branch, "identity", id.Name, "took", m.deps.Clock.Now().Sub(started))
	return nil
}

// confirmCarrying records that the pending delivery's turn started: the claim and the delivery
// together, because either half alone is a claim a restart reads wrongly — a working claim whose
// delivery is unconfirmed is re-sent under the same id and answered by the shim without a second
// turn, so its worker is never prompted again, and an idle claim holding a confirmed delivery holds
// one nothing retires. Once the write holds, the workflow is told the agent took its work
// (Deps.TaskConfirmed): a root whose wait that untaken task held is decided on this fact rather
// than at the next tick. A write that failed tells no one, and is owed: the claim it carries is
// the claim memory holds from here on — the turn started whether or not the row says so — so the
// write is marked and made again at the claim's next decision (owes). The mark is taken beside
// saved rather than instead of it, and saved still sees the same error, because the two say
// different things: saved is the delivery's own, and settle writes it again before the next
// decision, where the mark is the claim's.
//
// The send that reached this turn clears the claim's prompt budgets and, with them, the
// delivery's record of the sends that failed before it (Delivery.SendFailures and its two
// companions): a task that got through carries zeroes, so what an operator reads on a pending
// delivery is the run of failures standing in the way of the send it is still waiting for,
// never one an earlier turn of the same task already got past. It is cleared on the delivery the
// claim holds, which is the row putClaimAndPending writes, so the write below carries it.
//
// A task of the entry the claim's handshake names records its confirmation there too, the first
// one only (Claim.Handshake, EntryHandshake.confirmed): the delivery's own ConfirmedAt goes with
// the delivery when the turn ends, and the handshake is what keeps when the worker took that
// entry's work. The same write carries it, since the claim is half of it.
//
// This is also where the claim's two records of what became of the agent's earlier work — the
// session the claim abandoned (AbandonedSession) and what the last let-go of its process ended
// (EndedWork) — become told, and it is the rule every other mention of their telling points to.
// A record is told at the confirmation of a prompt that carried its sentence to this process
// (Delivery.message, Machine.carried), since the turn starting is the agent reading that prompt,
// and it records the delivery whose confirmation told it (ToldBy). A confirmation later taken
// back takes that telling back with it, so the next prompt carries the sentence again
// (moveTelling); one a let-go takes back once the prompt's result has come keeps it
// (interrupted). Neither is marked at the prompt's acknowledgement (acked): the pinned Oh My Pi
// acknowledges a prompt at its admission and can then answer it busy, and a sentence marked told
// there would never reach the agent, since the prompt sent after the busy answer would carry none.
// A turn whose prompt carried no sentence — one sent before the record was written, or one an
// earlier daemon sent, of which this machine knows nothing — tells nothing, and the next prompt
// carries the sentence again: told twice, never not at all. So it goes where a let-go cuts off a
// turn that confirmed the task while the send of its prompt was still in flight or a prompt of the
// task still waited on its result: that turn may have been another input's, so the let-go takes
// the telling back (untellInFlight). Both records are the claim's fields, so they go in the same
// write as the confirmation, and a restart reads both or neither. The one record told anywhere
// else is the old session's record of a let-go, which the abandonment of that session ends by no
// delivery (forgetSession, where that rule is written).
//
// News of the approved design is told the same way, by the copy this write makes the delivery's
// credit (Delivery.Carried): news, what the prompt this turn answers carried, never the news the
// delivery holds now, which a send since that prompt may have rendered again with more in it. A
// turn's start and a restart's question read it off the prompts this machine sent (turnAnswers),
// and a prompt's own result that confirms a task no turn was matched to names its prompt itself
// (promptResulted). A second confirmation of the same delivery writes its own copy over the first,
// so the delivery holds one credit, the last one written.
func (m *Machine) confirmCarrying(ctx context.Context, news []byte) error {
	p := m.claim.Pending
	p.ConfirmedAt = m.deps.Clock.Now()
	p.credit(news)
	m.claim.Handshake = m.claim.Handshake.confirmed(*p)
	m.disarm(TimerTurn)
	m.askFirst = false
	m.claim.Budgets.PromptFailures, m.claim.Budgets.PromptRetires = 0, 0
	p.SendFailures, p.LastSendFailureAt, p.LastSendError = 0, time.Time{}, ""
	if a := &m.claim.Abandoned; m.carried.abandoned && a.Tells() {
		a.Told, a.ToldBy = true, p.ID
		m.log.Info("supervise: a turn took the prompt that told the agent of the session its claim abandoned",
			"delivery", p.ID, "session", a.Session)
	}
	if e := &m.claim.EndedWork; m.carried.ended && e.Tells() {
		e.Told, e.ToldBy = true, p.ID
		m.log.Info("supervise: a turn took the prompt that told the agent what its relaunch ended",
			"delivery", p.ID, "cause", e.Cause, "ended", e.Generation)
	}
	if err := m.saved(m.owes("PutClaimAndDelivery", m.putClaimAndPending(ctx))); err != nil {
		return err
	}
	if m.deps.TaskConfirmed != nil {
		m.deps.TaskConfirmed(m.current())
	}
	return nil
}

// turnAnswers is the one answer to which prompt of the pending task a turn that starts now
// answers: the news of the approved design that prompt carried, which the turn's confirmation
// credits (confirmCarrying), and whether a turn that names no prompt is taken for the task's at all
// (inFlight). beginTurn confirms the task by it, and so does sendPending where a restart's question
// finds the agent in a turn, so a new reason a turn is the task's is written here once. In order:
//
//   - While a prompt of the task Oh My Pi acknowledged waits on its result (Machine.outstanding),
//     the turn is the first of them's: the pinned Oh My Pi runs one admitted prompt at a time, and
//     a later send it answered busy carried nothing that turn reads. So a resend that rendered an
//     opening the first prompt did not carry adds none to what the turn's confirmation credits.
//   - Otherwise, while a send of the pending task is in flight and no answer has made it moot
//     (sendUnanswered), the turn is that send's (the INFRA-264 race, where the prompt's own turn
//     starts before its acknowledgement), and its confirmation credits what that send carries,
//     the pending delivery's own news (startSend says why that is the send's). A turn of another
//     input's that starts while the send is still adopting the working copy or minting its
//     identity is taken the same way, and the send's own outcome takes it back with its credit
//     wherever the prompt does not then run: a refused or lost send, a busy answer or a result
//     that ran no turn, a let-go while the send or its prompt is outstanding (untellInFlight),
//     and a turn's end after a restart that no result answered (RetireDelivery). Where the prompt
//     does run, its news was read, so the credit stands.
//   - A send in flight under another id, its answer still to come, carried another delivery's
//     news, or this task's under an id it has left — a retired task that a newer one replaced
//     while its send was out, or one taken back under a new id since — so the turn is taken for
//     the task's, and credits nothing of it: told twice, never credited for a prompt that was not
//     this delivery's.
//   - A send whose answer has come (sending.resulted) is read as no send at all, under any id, so
//     a turn that starts after that answer is the task's only by the bullet below. The answer has
//     said what became of the prompt: a result that ran a turn says which turn that was
//     (promptResulted), and a no-turn answer takes the task back under a new id (promptFailed,
//     takeBackPending) or, where that write fails, leaves it under the send's own id with its
//     confirmation cleared, so the pending task's own send is read by its answer too, not by its
//     id alone. Taken for the task's, such a turn would confirm a task with nothing left to come
//     that could take the confirmation back: a let-go in that turn would vouch for it
//     (confirmationInDoubt), and the end of the turn a busy answer named would retire the task as
//     served (settle). A resend that an earlier prompt's result marked answered, that result's
//     turn having ended in a provider error, is read the same way, though the resend's own turn
//     can still run after that one: nothing confirms the task, the retry sends it again, and the
//     resend's result is fenced, as when the resend's acknowledgement is read before that result.
//   - Otherwise the turn is taken for the task's while the wait for an acknowledged prompt's turn
//     is armed, or while a restart's question stands for a task it may take (askFirst,
//     askFirstNeverTakes), and it credits the copy the delivery holds: the copy the earlier
//     daemon's acknowledged prompt wrote, none where no prompt was acknowledged. A turn a replay
//     names by the pending id credits the same.
func (m *Machine) turnAnswers() (news []byte, inFlight bool) {
	p := m.claim.Pending
	switch {
	case m.awaitsResult():
		return m.turnNews(), true
	case m.sendUnanswered() && p != nil && m.send.id == p.ID:
		return p.News, true
	case m.sendUnanswered():
		return nil, true
	}
	_, awaiting := m.timers[TimerTurn]
	inFlight = awaiting || (m.askFirst && (p == nil || !m.askFirstNeverTakes(*p)))
	if p != nil {
		news = p.Carried
	}
	return news, inFlight
}

// settle retires a delivery whose life is over, and runs around every decision, so that two things
// hold however the claim got where it is — the turn ending, a suspension — and hold again at once
// for a claim restored from a store a crash left in between: a confirmed delivery lives exactly as
// long as its turn, and a suspended claim holds no task the suspension finished. A death is not
// one of them: it takes the task whose turn it ended back first (interrupted), so settle finds
// nothing of it to retire.
//
// Which turn was the delivery's is what the result of its outstanding prompt answers
// (Machine.outstanding), and until it has, no turn that ends is taken for it (resultPending).
// Oh My Pi's agent_end names no prompt at all, and with background work on for every role a
// background result wakes a settled session and starts a turn the daemon never prompted: a
// confirmed task retired at that turn's end would be one the role never worked, and the turn that
// really is its own would serve no run. The result lands on the heels of the agent_end of the
// turn the prompt ran, so the wait is milliseconds; a prompt the agent refused or a daemon
// restart leaves none outstanding, and the turn's end decides as it always did.
//
// The same holds while the send of the task's prompt is in flight and no answer has made it moot
// (sending.resulted): its own result or its refusal after the acknowledgement, or a
// turn-reporting result of any prompt of the task (resultPending's send window). A turn that
// starts then is taken for the task's (turnAnswers), and its end can be decided before the send's
// acknowledgement, which reaches the machine on the send's own goroutine (sending). A task
// retired at that end would have its prompt's result dropped as stale (fence), and with it the
// final credit of the news the prompt carried, which would then be owed and sent again, or the
// provider error that leaves the work owed (providerErrored). The send's outcome and the prompt's
// result say which turn it was, as they do for a prompt acknowledged first.
//
// A daemon suspension ends the claim's phase, so a task queued for a phase is the finished phase's
// and goes with it; the next resume is handed its new phase's task, never that one. An operator
// suspension is different: the operator resumes this same claim, so its pending phase task waits
// through that suspension and is sent after the operator's resume. A released claim's is the same
// — the daemon released an idle pod, not the phase, and the role's next wake resumes this session
// in that phase with the task it kept. SuspendSource.RetiresPhaseTask is the one statement of
// which sources end the phase. A task of no phase is likewise not the workflow's to end.
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
	turnOver := !p.ConfirmedAt.IsZero() && m.claim.State != StateWorking && !m.resultPending()
	suspended := m.claim.State == StateSuspended && p.Phase != "" && m.claim.SuspendedBy.RetiresPhaseTask()
	if !turnOver && !suspended {
		return nil
	}
	return m.retirePending(ctx)
}

// promptFailed is a prompt that was acknowledged and then came to nothing: the wait for its turn
// ran out, or Oh My Pi answered it a failure with no turn of its own running. The task goes back
// to waiting and the prompt is charged, so an agent that never takes one walks its budget to the
// relaunch and the retirement. read says whether the agent may have read it: the bound's caller
// passes true, since the turn that has not started may still be this task's, and a refusal passes
// false, since a prompt the agent answered a failure never ran.
//
// The caller has already dropped from the task's outstanding prompts the one its answer names,
// where it names one (Machine.outstanding); the wait that ran out names none, and its prompt
// stays outstanding, since its turn at the pin is still the session's next. Whatever stays
// outstanding is what the take-back reads (takeBackPending).
func (m *Machine) promptFailed(ctx context.Context, why string, read bool) error {
	if err := m.takeBackPending(ctx, read); err != nil {
		return err
	}
	return m.chargePrompt(ctx, why)
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

// abandonedTask begins the first prompt given to the agent that replaced a session the claim
// abandoned (forgetSession): one whose last turns the provider refused, one an operator retried
// with --fresh-session, or one no session store holds any more. Nothing in a new session says
// that an older one existed, so a worker reading an empty transcript takes its task for a fresh
// start and does again what is already pushed (dispatch://INFRA-714, item 4 of the refusal
// section). It names the session that was left behind and why in the words the claim recorded,
// and then the two things that outlive a session: the issue's branch, and the handoff records its
// phases wrote on the Dispatch issue. The controller is told neither, since it works no checkout.
// last is whether this sentence is the last one before the task, and so ends in the task's own
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
func abandonedTask(c Claim, branch string, last bool) string {
	read := "read the issue"
	if branch == "" {
		branch = claim.IssueBranch(c.Issue)
	}
	if c.Role != claim.RoleController {
		read = fmt.Sprintf("read the issue, then your branch %s and the handoff records its phases wrote on "+
			"the issue, which `dispatch handoff-read --issue %s --phase <word>` reads (%s)",
			branch, c.Issue, handoffWords())
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

// interrupted takes back the pending task whose turn the claim's process can no longer finish — it
// died, or a forced stop is about to end it — so the relaunched agent's ready sends it again: Oh My
// Pi does not resume the turn itself. why names which, for the line it logs. The turn ran,
// so the agent read the task and it keeps its read mark; it goes back unconfirmed under a new id
// the new process's shim has no record of, marked Interrupted, so it is sent behind the sentence
// of its interruption (message). outOfMemory says that end was the pod running out of memory,
// which adds outOfMemoryTask to what is sent; it is written on every call, true or false, so a
// second interruption of the same task for another reason clears a note that no longer
// describes it.
// The task is neither retired nor its run marked served, since the turn never finished. A task
// whose turn ended while its prompt's result was still to come, which leaves the claim idle with
// the task confirmed, is taken back as one whose turn still runs: no result has said that turn was
// the task's (resultPending), and the process that would send it is going. A task no turn has
// confirmed is left as it is: the relaunch sends it as it was. An older daemon's ask of an update
// round is the one task the relaunch never sends: it keeps its round across the new id
// (rotateTaskID), and sendPending drops it.
//
// The taken-back task becomes the claim's only once its write has landed. A write that fails
// leaves the task confirmed in memory as in the store, and the process is still recorded, so the
// sweep finds it gone again and this takes the task back then — where a task taken back in memory
// alone would be skipped by that death, relaunched from a store still holding it confirmed, and
// retired as served by a restart.
//
// The wait for the task's prompts' results ends with that same write (Machine.outstanding),
// after it rather than before, and for every prompt of the task at once: the turn they ran is
// the one this ends, and the process that would answer them is going. While the task stands
// confirmed, the wait is what keeps the turn that ends next from being taken for a prompt of its
// (resultPending), and settle retires a confirmed task of a ready or idle claim the moment
// nothing is waiting on a result for it (turnOver). Ending the wait for a take-back that did not
// land would leave a claim whose task is confirmed, whose prompts are over as far as this machine
// is concerned, and whose next settlement retires a task no turn ever finished. A take-back that
// did land leaves the task unconfirmed, which settle retires in no state at all.
//
// A take-back that lands also marks the task, under its new id, as taken from a turn of the
// process the claim records (Machine.cutTurnOf), and the let-go that follows records from that
// mark that the task's turn was cut off, whatever the claim's state (recordEnd): the confirmation
// it clears is what that let-go would otherwise read, and it reads that only where a turn still
// runs. A decision that records its let-go before this take-back marks the task first, under the
// id it holds (recordGoneEnd), and this moves the mark to the new id.
//
// What the confirmation it clears told stays told (confirmCarrying) where the result of the prompt
// it confirmed has come: that result says a turn ran the prompt, so the agent read the sentence the
// prompt carried, and the let-go that cuts that turn off takes no telling back (moveTelling). A
// confirmation a turn took while the send of its prompt was still in flight, or while a prompt of
// the task still waited on its result, may be another input's, and goes back to untold before the
// id moves (untellInFlight, where that rule is), in the same write as the delivery when a telling
// moved. The untelling stands in memory whether or not that write lands, since untold is the safe
// way round, and the claim's next write carries it. The record of the let-go that cut the turn off
// is new words, and is told by a prompt of its own.
//
// The claim's handshake gives back the same confirmation by the same reading (Claim.Handshake,
// EntryHandshake.unconfirmed), where it was this task's: a turn that may be another input's is no
// proof that the worker took the task, and a handshake that kept it would give the state an
// `entryConfirmedAt` for an entry the worker may never have taken (dispatch://INFRA-2614 R6). The
// doubt is read here, before anything forgets the send or the prompts outstanding
// (confirmationInDoubt). A confirmation whose prompt's result has come, or for which no send or
// prompt of the task was outstanding, is one a turn of the task did take, and the handshake keeps
// it across the relaunch. Unlike the untelling, the handshake moves only with the take-back's
// write, as queue writes it: the claim goes with the delivery when either changed, and a write
// that fails leaves the handshake as the store holds it, for the take-back made again to read.
func (m *Machine) interrupted(ctx context.Context, why string, outOfMemory bool) error {
	p := m.claim.Pending
	if p == nil || p.ConfirmedAt.IsZero() {
		return nil
	}
	m.log.Warn("supervise: "+why+"; the task waits for the relaunch", "delivery", p.ID)
	next := *p
	next.ID, next.ConfirmedAt, next.Interrupted, next.OutOfMemory = rotateTaskID(p.ID), time.Time{}, true, outOfMemory
	// The confirmation goes, and with it the credit its copy held: the write ends it with the
	// copy, where the turn's result had not yet made it final, and the relaunch's prompt carries
	// the news again.
	next.credit(nil)
	doubt := m.confirmationInDoubt()
	untold := m.untellInFlight()
	stored := m.stored()
	var handshakeMoved bool
	if doubt {
		stored.Handshake, handshakeMoved = m.claim.Handshake.unconfirmed(*p)
	}
	var err error
	if untold || handshakeMoved {
		err = m.putClaimAndDelivery(ctx, stored, next)
	} else {
		err = m.putDelivery(ctx, next)
	}
	if err != nil {
		return err
	}
	*p = next
	m.claim.Handshake = stored.Handshake
	m.forgetOutstanding()
	m.cutTurnOf = next.ID
	return nil
}

// taskRead and taskUnread say whether the agent may have read a task being taken back: the task
// keeps its delivered mark when it may have been read, which is what lets a completion reported
// from a turn the daemon did not deliver be attributed to it.
const (
	taskRead   = true
	taskUnread = false
)

// ordinaryEnd is interrupted's outOfMemory at a call site that ends the turn of a process still
// running — one replaced mid-turn, for a stale address, a stale plugin, a stalled turn or a paced
// launch whose turn has come, or one a held suspension's deadline stops — where nothing ended the
// pod's container, so the end was not the pod running out of memory. A death and a displacement
// pass the observation's own OutOfMemory instead, and the suspension that lands over a process a
// report found gone passes what that report recorded (suspendOverADeadProcess, goneOutOfMemory).
const ordinaryEnd = false

// markRead and clearReadMark are the only writers of the pending task's read mark: its
// acknowledgement time (DeliveredAt) and the delivery id the acknowledged prompt carried
// (MarkedBy) are set together and cleared together, so the prompt a late refusal is judged against
// is always the one whose acknowledgement set the mark standing now. Each also records that a
// prompt of this task went out (Delivery.Prompted), as the send itself does: an outcome of either
// kind is proof that the agent was put to it.
//
// clearReadMark is a prompt that came to nothing unread, so the copy of the news an unconfirmed
// task holds (Delivery.Carried) goes back to that of the first prompt still outstanding, or to none
// (copyTurnNews): the prompt that set it starts no turn. A confirmed task keeps the copy its
// confirmation credited, which only that confirmation's own take-back moves.
func (m *Machine) markRead(p *Delivery) {
	p.DeliveredAt, p.MarkedBy, p.Prompted = m.deps.Clock.Now(), p.ID, true
}

func (m *Machine) clearReadMark(p *Delivery) {
	p.DeliveredAt, p.MarkedBy, p.Prompted = time.Time{}, "", true
	m.copyTurnNews(p)
}

// markUnread drops the pending task's read mark without touching its id or its budget: an
// outcome that arrived for a send this machine had already given up on says only that the agent
// never read the task.
func (m *Machine) markUnread(ctx context.Context) error {
	p := m.claim.Pending
	marked := !p.DeliveredAt.IsZero()
	m.clearReadMark(p)
	if !marked {
		return nil
	}
	return m.putPending(ctx)
}

// copyTurnNews writes, on a task no turn has confirmed, the copy of the news its next turn would
// credit: the news of the first prompt of it still outstanding, whose turn is the session's next,
// none where no prompt is (turnNews). A confirmed task keeps the copy its confirmation fixed. It is
// the one writer of the pending task's unconfirmed copy: for an acknowledgement (acked), a prompt
// that came to nothing (clearReadMark), a confirmation taken back (takeBackConfirmation) and a
// let-go (letGo). It answers whether the copy changed, so a caller that leaves the write of the
// delivery to settle marks it unsaved (letGo). A let-go's take-back of a confirmation (interrupted)
// writes no copy of the pending task: it drops the copy of the task that takes that task's place
// once its write lands (credit, with no news).
func (m *Machine) copyTurnNews(p *Delivery) bool {
	if !p.ConfirmedAt.IsZero() {
		return false
	}
	news := m.turnNews()
	changed := !bytes.Equal(p.Carried, news)
	p.credit(news)
	return changed
}

// credit writes news as the delivery's copy (Carried), not yet final (Ran). It is how every writer
// of the copy writes it — a confirmation (confirmCarrying), the writer of an unconfirmed copy
// (copyTurnNews) and a let-go's take-back of a confirmation (interrupted), which drops the copy
// and with it the credit the confirmation held — so no copy is ever written beside a stale mark
// that it is final.
func (d *Delivery) credit(news []byte) {
	d.Carried, d.Ran = news, false
}

// awaitResult records id as a prompt of the pending task Oh My Pi acknowledged, whose result is
// still to come (Machine.outstanding), with news, the news of the approved design that prompt
// carried. An id already held is held once, with the news its first acknowledgement reported: the
// shim answers a prompt it has a record of without a second turn (shimwire's dedupe), so its
// acknowledgement can come twice for one prompt, and the agent read what the send the shim
// forwarded carried. Every send under one id carries the same news (currentNews takes a new id for
// a new rendering), so keeping the first is that rule's backstop.
func (m *Machine) awaitResult(id string, news []byte) {
	if m.outstandingPrompt(id) {
		return
	}
	m.outstanding = append(m.outstanding, awaitedPrompt{id: id, news: news})
}

// forgetOutstanding ends the wait for every prompt of the pending task at once
// (Machine.outstanding): a turn's result, the sweep's answer that the agent is in no prompt, a
// compaction, a let-go, a take-back of a task whose turn its process can no longer finish, and a
// retirement each end the wait for all of them.
func (m *Machine) forgetOutstanding() {
	m.outstanding = nil
}

// answered drops id from the prompts the pending task waits on a result for: Oh My Pi answered
// it, a result or a refusal, and answers no prompt twice.
func (m *Machine) answered(id string) {
	m.outstanding = slices.DeleteFunc(m.outstanding, func(o awaitedPrompt) bool { return o.id == id })
}

// promptNews is the news of the approved design the outstanding prompt id carried, and whether id
// is outstanding at all.
func (m *Machine) promptNews(id string) ([]byte, bool) {
	at := slices.IndexFunc(m.outstanding, func(o awaitedPrompt) bool { return o.id == id })
	if at < 0 {
		return nil, false
	}
	return m.outstanding[at].news, true
}

// outstandingPrompt is whether id is a prompt of the pending task still waiting on its result.
func (m *Machine) outstandingPrompt(id string) bool {
	_, outstanding := m.promptNews(id)
	return outstanding
}

// awaitsResult is whether any prompt of the pending task is still waiting on its result.
func (m *Machine) awaitsResult() bool { return len(m.outstanding) > 0 }

// turnNews is the news of the approved design the first prompt of the pending task still
// outstanding carried, nil where none is outstanding: the pinned Oh My Pi runs one admitted prompt
// at a time, so the session's next turn is that prompt's (Machine.outstanding), and that is what
// a confirmation of it credits (turnAnswers).
func (m *Machine) turnNews() []byte {
	if !m.awaitsResult() {
		return nil
	}
	return m.outstanding[0].news
}

// takeBackPending returns the pending delivery to waiting after a prompt came to nothing, under a
// new id so the retry is a new prompt rather than an echo the shim answers from its record, and
// with the wait for its turn disarmed.
//
// While a prompt of the task is still outstanding once the caller has dropped the one its answer
// named (awaitsResult), the pinned Oh My Pi runs one admitted prompt at a time and refuses the
// rest, so the session's next turn, running or still to come, is that prompt's: the task keeps
// its confirmation and its read mark for it, and only the pending id moves. That is the busy
// answer of a send made while the first prompt's turn is late — refused because that very turn is
// being prepared or run — which takes back nothing of that turn. With none outstanding, the wait
// for the task's results is over, and the task goes back unconfirmed if a turn had confirmed it,
// and a task taken back unread loses the mark, so nothing the worker reports from whatever turn
// follows belongs to it. A caller whose prompt's turn is over or aborted therefore leaves nothing
// of the task outstanding before it calls, since the set is what this reads to tell the two
// apart: a provider error's result has ended the wait already (promptResulted), and a compaction
// ends it itself (compactStart).
//
// A confirmation it clears was taken by a turn that was not the task's — another input's, or one
// the prompt's own answer says came to nothing — so what it told goes back to untold with it, and
// the next prompt carries that sentence again (moveTelling). Two callers clear a confirmation a
// turn of the task itself may have taken, and the untelling is right for both: a provider error
// (providerErrored) is a model call that failed, often the turn's first, so the agent never read
// the prompt, and where an earlier call of the turn did, the sentence is told twice rather than
// lost; a compaction (compactStart) aborts the turn, and the sentence repeated in the next prompt
// is the safe way round. A confirmation kept for an outstanding prompt keeps its telling too,
// moved with it to the new id, so that a later take-back of the same confirmation under that id
// still finds it.
//
// The id moves either way: the prompt whose answer this is gets a new one, so its own late
// acknowledgement is fenced out (fenceOutcome) and adds nothing outstanding, and the shim's
// one-delivery record (shimwire's dedupe) is never asked to answer an old id again.
//
// The refusal or timeout behind it arrives once, and settle and ServingRun read the claim as memory
// holds it, so what it established — not confirmed, not read when taken back unread, and no longer
// told by that confirmation — holds at once, whether or not the write recording it lands. Only the
// new id waits for the write, and the telling moved to it with it, so the claim's id is always one
// the store holds; a write that fails leaves the delivery unsaved, and settle writes it again
// under the id it kept, with the claim (saved). Where none is outstanding, the wait for the
// prompts' results is over before that write, which is safe here, where interrupted ends the wait
// after its own write: this clears ConfirmedAt in memory first, so the claim a failed write leaves
// is one settle reads as a task still waiting rather than as a turn that is over (turnOver,
// resultPending). A telling that moved goes in the same write as the delivery, the claim and the
// delivery together, so a restart never reads a record told by a confirmation the delivery no
// longer holds.
//
// A confirmation taken back was never the worker's, so the claim's handshake gives back its own
// too (takeBackConfirmation); a confirmation kept for an outstanding prompt keeps the handshake's
// with it, since that prompt's turn is the task's. Where the handshake gives its own back, the
// claim goes with the delivery in one write, as confirmCarrying wrote the two and as a telling
// that moved goes: a write that fails leaves the delivery unsaved as before, and settle's write of
// it (putClaimAndPending) carries the claim, so the cleared handshake reaches the store with the
// first write the store takes and no failure is handled any other way.
func (m *Machine) takeBackPending(ctx context.Context, read bool) error {
	m.disarm(TimerTurn)
	p := m.claim.Pending
	id, kept := rotateTaskID(p.ID), m.awaitsResult()
	var moved, handshakeMoved bool
	if kept {
		moved = m.moveTelling(p.ID, id)
	} else {
		// No prompt of the task is left whose turn can start, so no copy of the news a prompt
		// carried stands either (Delivery.Carried); a confirmation it clears ends its credit with
		// the write.
		handshakeMoved = m.takeBackConfirmation(p)
		if !read {
			m.clearReadMark(p)
		}
		moved = m.moveTelling(p.ID, "")
	}
	next := *p
	next.ID = id
	var err error
	if moved || handshakeMoved {
		err = m.putClaimAndDelivery(ctx, m.stored(), next)
	} else {
		err = m.putDelivery(ctx, next)
	}
	if m.saved(err) != nil {
		if kept && moved {
			// The confirmation stays under the id the write could not move it from.
			m.moveTelling(id, p.ID)
		}
		return err
	}
	p.ID = id
	return nil
}

// takeBackConfirmation clears the pending delivery p's confirmation, and the claim's handshake's
// with it where that confirmation was p's own (EntryHandshake.unconfirmed). It answers whether the
// handshake changed, which is whether the write that records the take-back must carry the claim.
// A let-go's take-back (interrupted) does not come here, since its task becomes the claim's only
// once its write lands: it gives the handshake's confirmation back by the same reading where the
// confirmation is in doubt (confirmationInDoubt), and keeps it where a turn of the task did take
// it.
//
// Both callers take a confirmation back with no prompt of the task outstanding (takeBackPending,
// refused), so no turn of a prompt that carried news of the approved design can start any more,
// and the copy of that news goes with the confirmation (Delivery.Carried, copyTurnNews, which
// writes none with no prompt outstanding): the write that records the take-back ends the credit
// the copy held, and no other (the claim store's design_news_told_in).
func (m *Machine) takeBackConfirmation(p *Delivery) bool {
	handshake, moved := m.claim.Handshake.unconfirmed(*p)
	p.ConfirmedAt = time.Time{}
	m.copyTurnNews(p)
	if !moved {
		return false
	}
	m.claim.Handshake = handshake
	return true
}

// moveTelling hands what the confirmation of delivery from told (confirmCarrying) to to, the id
// that confirmation stands under from here on, and reports whether any record had a telling to
// hand: each of the claim's two records whose ToldBy is from names to instead. to "" is the
// confirmation taken back, which tells nothing any more, so such a record goes back to untold and
// the next prompt carries its sentence again. Its callers:
//
//   - refused, a send that reached no turn with no other prompt of the task outstanding: the
//     turn that confirmed the task was another input's. It writes the claim with the delivery
//     (putClaimAndPending) when this reports true.
//   - takeBackPending, a prompt that came to nothing: a late busy answer, refusal or result that
//     ran no turn (lateRefused, refusedElsewhere, refusedUnprompted, waitedOutAnswer,
//     promptResulted), a wait for a turn that ran out (noTurn; it and the answers that charge the
//     prompt go through promptFailed), a provider error (providerErrored) and a compaction
//     (compactStart). With no prompt of the task left outstanding it takes the telling back; with
//     one outstanding it keeps the confirmation and moves the telling to the new id. Either way it
//     writes the claim with the delivery (putClaimAndDelivery) when this reports true.
//   - takeBackPending again, where that write failed in the kept case: it moves the telling back
//     to the id the store still holds the confirmation under, and writes nothing, since the store
//     never took the move.
//   - untellInFlight, a let-go's take-back of a confirmation a turn took while the send of its
//     prompt was in flight or a prompt of the task still waited on its result. Of its two callers,
//     interrupted writes the claim with the delivery (putClaimAndDelivery) when this reports true,
//     and recordEnd writes nothing, since the let-go's own write carries the claim.
func (m *Machine) moveTelling(from, to string) bool {
	moved := false
	if a := &m.claim.Abandoned; a.Told && a.ToldBy == from {
		a.Told, a.ToldBy, moved = to != "", to, true
	}
	if e := &m.claim.EndedWork; e.Told && e.ToldBy == from {
		e.Told, e.ToldBy, moved = to != "", to, true
	}
	if moved && to == "" {
		m.log.Info("supervise: the confirmation of the prompt that told the agent what became of its earlier work was taken back; the next prompt tells it again",
			"delivery", from)
	}
	return moved
}

// confirmationInDoubt is whether the pending task's confirmation is one a let-go cannot vouch for:
// it stands while a send is in flight whose answer is still to come (sendUnanswered) or a prompt
// of the task still waits on its result (awaitsResult). untellInFlight states the rule and why;
// this is its one reading, which interrupted makes for the claim's handshake too. It reads the
// send and the prompts outstanding, so it is read before the let-go forgets them (letGo,
// forgetSend; interrupted).
func (m *Machine) confirmationInDoubt() bool {
	p := m.claim.Pending
	return (m.sendUnanswered() || m.awaitsResult()) && p != nil && !p.ConfirmedAt.IsZero()
}

// untellInFlight is the rule for a confirmation a let-go cannot vouch for (confirmationInDoubt).
// The pending task's confirmation is uncertain at a let-go while a send is in flight whose answer
// is still to come (sendUnanswered) or any prompt of the task still waits on its result
// (awaitsResult). A turn that starts then is taken for the task's (beginTurn): with a send in
// flight, the INFRA-264 race, and with an acknowledged prompt outstanding, however late the turn
// starts. Either way the turn may be another input's, an Envoy notice's, and what would say so is
// still to come: the send's busy answer (refused), or the prompt's own answer, a busy answer or a
// late refusal that ran no turn (promptResulted, lateRefused, takeBackPending), each of which
// takes the confirmation back. The let-go forgets the send and the prompts outstanding (letGo,
// forgetSend; interrupted), so that answer is fenced out (fenceOutcome, fence) and never takes
// anything back. So the let-go takes back what the confirmation told (moveTelling): the record
// goes back to untold, the record of this let-go joins it rather than replacing it (recordEnd,
// joined), and the next prompt carries its sentence again. Where the turn was the task's own, the
// agent is told twice, which is the accepted cost: told twice, never not at all. A confirmation
// whose prompt's result has come stands, whether or not the outcome of that prompt's send has
// been read yet (sending.resulted): that result says a turn ran the prompt (promptResulted), so
// the agent read the sentence the prompt carried, and a let-go after it, in that turn or later,
// keeps the record told. The take-back of the task gives the claim's handshake's confirmation
// back by the same reading (interrupted).
//
// It reports whether a telling moved, and it is idempotent: once it has run, no record is told by
// the pending id. recordEnd and interrupted both call it, and whichever of them a let-go runs first
// untells. recordEnd calls it before anything else, in memory only, since the let-go's own write
// carries the claim: a death and a displacement record the end before they take the task back
// (died, displaced). interrupted calls it before the id moves and writes the claim with the
// delivery when it reports true, for every path that takes the task back before the let-go records
// the end: the held suspension's deadline (suspendAtTimeout), a paced launch whose turn has come
// (runWake), a suspension over a dead process (suspendOverADeadProcess), and the replacement of a
// process still running (replaceProcess, replaceNow) for a stale address (repoint), a stale plugin
// (relaunchMarked) and the stall backstop (backstopStalledTurn).
func (m *Machine) untellInFlight() bool {
	if !m.confirmationInDoubt() {
		return false
	}
	return m.moveTelling(m.claim.Pending.ID, "")
}
