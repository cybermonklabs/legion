package supervise

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"

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
	// run. Zero for a task of no run: an operator's own, an architect's.
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
	// CheckLook is the look of a closed issue's production check this task is of, from the start
	// row that queued it (record.SuperviseRequest.CheckLook and CheckOpenedAt). It is what says a
	// turn's completion is a check report rather than a phase completion: the API routes on the
	// look the claim's worker took (Claim.ServingCheck), and the report is of that look rather
	// than of whichever look the check has reached (api/handoff.go, checkReport). The zero value
	// is a task of no look: every phase task, an operator's own, a resume.
	CheckLook   CheckLook
	Phase       phase.Phase
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
	// unless a prompt of that task has already come to nothing (Claim.ServingRun).
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
	// cleared (confirm), so a task that got through carries zeroes. LastSendError is never the
	// far side's text as it arrived: the write that records it scrubs and bounds it first
	// (scrubSendError), because the text is the far side's own and the store takes only some of
	// what the far side can say.
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
}

// maxSendErrorRunes bounds the text of a failed send kept on a delivery (scrubSendError). What a
// reader of that record needs is why the send failed: the first line of the far side's answer and
// enough of what follows to name the failure — the shim's words name the stale lock's path and
// its age within the first two hundred characters — so this is more room than the diagnosis asks
// for and far less than the text can be. What it cuts is the rest of a relayed command's output:
// a refused adoption carries the wedged command's whole stderr, measured at about a megabyte
// (INFRA-1480), which would otherwise be stored on every failed send of the task and served
// again in every operator listing of the claim.
const maxSendErrorRunes = 2048

// scrubSendError is the text of a failed send as a delivery may hold it. The text is the far
// side's own and is not this daemon's to trust: a refused adoption carries the wedged command's
// whole stderr, relayed by the shim, and a refused prompt carries the `error` Oh My Pi decoded
// from JSON, where a `\u0000` in it becomes a NUL byte.
//
// The rule the store holds is that the text is valid UTF-8. Postgres refuses any byte its server
// encoding cannot read — on the UTF8 encoding production runs, `select E'wedged\000lock'` is
// "invalid byte sequence for encoding \"UTF8\": 0x00" and a lone 0xff is the same refusal — so
// the NUL is one instance of that rule rather than the rule itself, and an SQL_ASCII server,
// which takes the 0xff and refuses the NUL, draws the line in another place. So the text is made
// valid UTF-8, with each run of bytes that reads as no character replaced by the replacement
// character rather than dropped, so the record still shows where the far side's words were
// unreadable; the NUL — valid UTF-8 and still refused by every encoding — goes; and what is left
// is cut to maxSendErrorRunes. The start is what is kept, because that is where the far side
// names the failure: the shim's own words give the stale lock's path and its age there. The cut
// counts runes rather than bytes, so it can never make the text invalid again by ending inside a
// character.
//
// Nothing of the far side's answer is lost this way, only moved: the whole unscrubbed text goes
// to the log line this record stands beside (table.go's refused), which has no store to refuse
// it, so the operator who needs the megabyte still reads it in the pod's log.
func scrubSendError(text string) string {
	text = strings.ReplaceAll(strings.ToValidUTF8(text, "\uFFFD"), "\x00", "")
	if runes := []rune(text); len(runes) > maxSendErrorRunes {
		return string(runes[:maxSendErrorRunes]) + "…"
	}
	return text
}

// StillWorked says whether a claim in state will still work this task as the task of generation's
// run in phase p: it is that run's task for that phase, and either not yet confirmed (sent once the
// claim is ready, a task a death took back included) or confirmed in the turn the claim is working.
// A confirmed task on a claim not working is over: the claim's next decision retires it.
func (d Delivery) StillWorked(generation uint64, p phase.Phase, state ClaimState) bool {
	return d.Generation == generation && d.Phase == p && (d.ConfirmedAt.IsZero() || state == StateWorking)
}

// sentences is which of the claim's two untold records a prompt carries the sentence of: the
// session the claim abandoned (AbandonedSession, abandonedTask), and what the last let-go of its
// process ended (EndedWork, relaunchSentence). Delivery.message answers it beside the prompt it
// composes, so what a prompt is marked as telling is what its text holds (Machine.carried, which
// confirm reads).
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
//     this task being interrupted where the record says the task was in it (EndedWork.TaskTurn) —
//     so a task carries the one sentence and not both.
//   - A task taken back from a turn (Interrupted) whose record has been told, or that no record
//     names, still says its turn was interrupted, under the cause the claim's record names
//     (interruptedTask): the let-go that took it back, whichever path that was, rather than the
//     last death's words. With an abandoned session still to tell, that turn was the old
//     session's, and the abandoned session's sentence stands in its place (the first item).
//
// Composing a prompt tells nothing (confirm).
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
// One case still sends a stale note: the previous release's upsert carries no out_of_memory
// column, so if an image rollback takes a marked task back again for another end, the row keeps
// the mark, and a roll-forward sends the note for that end.
func (d Delivery) message(c Claim) (string, sentences) {
	text, carried := d.Task, sentences{}
	relaunch := d.Relaunch()
	if relaunch && c.EndedWork.Tells() {
		text, carried.ended = RelaunchSentence(c.EndedWork), true
	}
	var prefix string
	if c.Abandoned.Tells() {
		// The task's instruction ends the last sentence before the task, so the abandoned
		// session's carries it only where no record's sentence follows it.
		prefix, carried.abandoned = abandonedTask(c, !relaunch && !c.EndedWork.Tells()), true
	}
	// oldTurn is whether a turn this task was taken back from is the abandoned session's, whose
	// sentence stands in place of the notes of that turn: an abandoned session still tells, and
	// no record of the fresh process says the task was in a turn of its own.
	oldTurn := c.Abandoned.Tells() && !(c.EndedWork.Tells() && c.EndedWork.TaskTurn)
	switch {
	case relaunch:
		// Its text is the record's sentence already.
	case c.EndedWork.Tells():
		prefix, carried.ended = prefix+relaunchSentence(c.EndedWork, true)+"\n\n", true
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
// transaction whether or not it names an outbox row.
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
// because its root had no architect to hear it never wedges the claim: the start of the root's
// next run, retrying on its backoff, is what releases it, and the refusal that start is answered
// with is over once the architect's turn on the held task has ended and the delivery retired.
//
// A pending delivery whose task the worker has already taken refuses nothing: the turn took it
// (confirm writes ConfirmedAt), so there is no task left to give and the delivery is only the
// record of the turn in flight. Such a delivery is retired here and the newer task queued in its
// place. Refusing it instead is what left three supervise rows of the live daemon answered "a
// delivery is already pending" with nothing ever sent (dispatch://INFRA-1352, INFRA-1443,
// INFRA-1519): the phase's own start was the newer task, and the worker that would have taken it
// was holding a delivery it had finished with. The newer task is not sent into the running turn —
// queue sends nothing, and sendPending hands a working claim nothing — so it goes at the next
// point a turn may take it, once, and no turn is interrupted or started twice.
//
// An untaken pending delivery still refuses a second one, as it always has: that task has reached
// nobody, so accepting over it would lose a task rather than save one. Two untaken deliveries are
// not refused (replaceable). One is the task a provider error took back, while the ladder's wait
// stands for it and nothing works it. The next step of the ladder can be ten minutes away and a
// run of errors has no end of its own, so a refusal would keep every newer task of the claim
// behind a task the claim owes for as long as the provider keeps failing (dispatch://INFRA-956
// R2: a newer task replaces the owed one). The newer task takes its place: it is work for this
// claim, queued since, and R2 has the newer of the two go. The other is the daemon's own delivery
// of a relaunch's record (queueRelaunch) while no prompt of it waits on a result: it holds no work
// of the claim's, only the sentence, and the newer task carries that same sentence in front of
// itself while the record is untold (message), so nothing is lost and the phase worker's resume
// task the ready queued is not held behind it. Once a prompt of it is outstanding, its turn is the
// session's next, and the newer task waits for that turn to end as behind any untaken task.
//
// An update round's own ask (updateRoundOf) takes no task's place, taken or owed: that ask exists
// to catch the worker between turns, with its work pushed, so it waits for the turn in flight to
// end rather than taking its place, and its post is re-made at the next tick. askForUpdate holds
// that question back while the claim holds a task at all, whichever task that is, and the reason
// it does is written there (table.go); the refusal here is what answers an ask that reaches this
// far. Only the work of the issue displaces work the worker has finished with.
//
// The replaced task's row ends as any other retirement ends it, and the question a restart armed
// for that task is answered in the same step, which is the one path both places a task replaces
// an owed one take (replaceOwedTask): the delete releases what that task held of a person's
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
// Of a task an operator gave the claim, which carries no id of its own and no outbox row
// (operatorDelivery), neither thing accepted above can be true, so the refusal below is exactly
// a claim that holds a pending delivery its worker has not finished with and that no newer task
// replaces. runParkedLaunchNow reads the same pending delivery before it runs a parked launch
// for such a task, so nothing runs for one this refuses; a task that replaces one runs that
// launch where it arrives, and retires the replaced delivery itself before it does — the launch
// takes the ladder's wait with the claim's timers and every prompt's wait for a result with the
// process (letGo), so the claim reaching here then holds no delivery at all.
func (m *Machine) queue(ctx context.Context, request RequestDeliver) error {
	id, task := request.ID, request.Task
	// A request that carries an entry of its own is decided by that entry, and one that carries
	// none — or that resends the entry its role's row already names — by the outbox row it came
	// from.
	var queued bool
	if request.Entry > 0 && !request.ResendEntry {
		queued = request.Entry <= m.claim.GivenEntry
	} else {
		queued = request.Row > 0 && slices.Contains(m.claim.DeliveredRows, request.Row)
	}
	if pending := m.claim.Pending; pending != nil {
		if (id != "" && pending.ID == id && pending.Task == task) || queued {
			return nil
		}
		_, asksUpdate := updateRoundOf(id)
		switch {
		case asksUpdate:
			if err := m.sendPending(ctx); err != nil {
				return err
			}
			return &RefusedError{State: m.claim.State, Request: "deliver", Err: ErrDeliveryPending}
		case pending.ConfirmedAt.IsZero():
			owed, attempt := pending.ID, pending.ProviderErrorRetries
			replaced, err := m.replaceOwedTask(ctx)
			if err != nil {
				return err
			}
			if !replaced {
				if err := m.sendPending(ctx); err != nil {
					return err
				}
				return &RefusedError{State: m.claim.State, Request: "deliver", Err: ErrDeliveryPending}
			}
			if relaunchTask(owed) {
				m.log.Info("supervise: a newer task replaces the daemon's delivery of the relaunch's record, which no prompt waits on; it carries the record's sentence in its place",
					"replaced", owed, "delivery", id)
			} else {
				m.log.Info("supervise: a newer task replaces the one a provider error took back",
					"owed", owed, "attempt", attempt, "delivery", id)
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
	d := Delivery{ID: id, Task: task, Phase: request.Phase, Generation: request.Generation, Kept: request.Kept,
		Entry: request.Entry, CheckLook: request.CheckLook, QueuedAt: m.deps.Clock.Now()}
	if request.Row > 0 || request.Entry > 0 {
		// The claim records the row and the entry only once the store has them with the
		// delivery: a write that failed leaves the row undelivered and the entry ungiven, so its
		// retry queues the task.
		stored := m.stored()
		if request.Row > 0 {
			stored.DeliveredRows = append(slices.Clone(m.claim.DeliveredRows), request.Row)
		}
		if request.Entry > 0 {
			stored.GivenEntry = request.Entry
		}
		if err := m.putClaimAndDelivery(ctx, stored, d); err != nil {
			return err
		}
		m.claim.DeliveredRows, m.claim.GivenEntry = stored.DeliveredRows, stored.GivenEntry
	} else if err := m.putDelivery(ctx, d); err != nil {
		return err
	}
	m.claim.Pending, m.unsaved = &d, false
	return nil
}

// answeredOpenRound is the hold an open update round puts on its claim: the round is open and
// the worker has answered it (ready_for_update, or the turn's end of a worker that cannot call
// it), so that worker has said its work is pushed and its process may be replaced at any moment.
// sendPending hands such a claim nothing, and NewMachine reads it through openRoundHoldsTask.
func (c Claim) answeredOpenRound() bool {
	return c.UpdateRound != "" && c.UpdateSignalledAt != nil
}

// openRoundHoldsTask is whether the claim's open round holds the task the claim has pending: the
// hold above, over a task that is not an ask of the round itself (updatesRound), which the round
// never holds back from the worker it is asking. It is built from sendPending's own hold rather
// than written out again, so the restore that reads it (NewMachine) can never judge a task held
// that sendPending would send, or the other way about.
func (c Claim) openRoundHoldsTask() bool {
	return c.answeredOpenRound() && !updatesRound(c.Pending, c.UpdateRound)
}

// sendPending sends the pending delivery if everything a send needs is true now: the claim is
// ready or idle, the delivery is neither confirmed nor already on its way, the issue is still in
// the phase the task was queued for, the root of a root architect's task of a run has an architect
// to hear it, the claim's connection is registered, and — for a task that adopts the working
// copy the issue's roles share (adopts) — no other claim of the issue is in a turn or has a
// prompt on its way to one. A missing connection is not a failure — the shim's next hello sends
// it. A delivery an earlier daemon may have sent is sent again only after the agent says it is
// not already in a turn; if it is, that turn is taken as the delivery's — except for a root
// architect's task of a run, which no turn of the agent's own is ever taken for, since it may be
// a task nobody has sent at all (asksRoot).
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
// An ask the claim can no longer answer is dropped here rather than sent, and there are two of
// them: an ask of a round another round has replaced, and an ask of a round whose pod the
// claim's process no longer runs in (Claim.LeftPodRound). The worker that read either would be
// asked to push its work for a pod replacement this daemon has already made, and it would
// refuse the answer the ask asks for. An ask names the round it asked in its own delivery id,
// and keeps it across every take-back (rotateTaskID), so the ask of a round a turn had
// confirmed comes back under that round when the turn ends in a death or a provider error,
// with the claim already in the round that replaced it. Sending it again would cost a model
// turn and make the current round's question wait behind a question nobody wants. That drop
// and the phase check's are one shape below, and both leave through the answer every drop of a
// delivery gives the mark armed for it (answerAskFirstAfterDrop).
//
// A third drop takes the same shape: the daemon's own delivery of a relaunch's record
// (Delivery.Relaunch, queueRelaunch) whose record no longer tells. Its text is that record's
// sentence and nothing else, so once the record is told it has nothing to say. The claim holds
// one in that state when its session is abandoned before the delivery's turn (forgetSession), and
// sending it would tell the fresh agent the old session's record in the text it was queued with.
//
// A drop that leaves the claim holding nothing while its record of the last let-go still tells
// gives it the daemon's own delivery of that record and sends it (relaunchAfterDrop): the ready
// gives that delivery only to a claim that holds nothing then (finishReady, queueRelaunch), so a
// held task dropped at the ready's own send would otherwise leave the sentence waiting for
// whatever task comes next.
func (m *Machine) sendPending(ctx context.Context) error {
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
	// sends, so the ask of a round this claim's process has left is still dropped before the
	// connection carries anything to the worker that cannot answer it. The one drop that leaves
	// elsewhere is a root architect's held task that names nothing any more (asksRoot's branch
	// below, currentRootTask): that branch clears askFirst before anything else, so it leaves no
	// question armed and asks none. Each drop ends with the claim given the delivery of its
	// record of the last let-go when that record still tells (relaunchAfterDrop).
	//
	// holds is whether the delivery is still the claim's to send; the ask of a round the claim
	// is not in now never is, and its issue's phase is not asked about, since the round's words
	// are what it is. Nor is the daemon's own delivery of a relaunch's record whose record no
	// longer tells (spent), whose sentence is all it carries; it names no phase either.
	round, asks := updateRoundOf(p.ID)
	stale := asks && (round != m.claim.UpdateRound || m.claim.LeftPodRound(round))
	spent := p.Relaunch() && !m.claim.EndedWork.Tells()
	holds := !stale && !spent
	if holds {
		var err error
		if holds, err = m.phaseHolds(ctx, *p); err != nil {
			return err
		}
	}
	if !holds {
		switch {
		case stale:
			m.log.Info("supervise: this task asks for a round the claim is not in now; it is dropped rather than sent",
				"delivery", p.ID, "asked", round, "round", m.claim.UpdateRound, "left", m.claim.UpdateLeftRound)
		case spent:
			m.log.Info("supervise: the daemon's own delivery of a relaunch's record has nothing to say, since that record no longer tells; it is dropped rather than sent",
				"delivery", p.ID, "cause", m.claim.EndedWork.Cause, "ended", m.claim.EndedWork.Generation, "toldBy", m.claim.EndedWork.ToldBy)
		default:
			m.log.Info("supervise: the issue has left the phase this task was queued for; it is dropped rather than sent",
				"delivery", p.ID, "issue", m.claim.Issue, "queuedFor", p.Phase)
		}
		if err := m.retirePending(ctx); err != nil {
			return err
		}
		if err := m.answerAskFirstAfterDrop(ctx); err != nil {
			return err
		}
		return m.relaunchAfterDrop(ctx)
	}
	// A root architect's task of a run (asksRoot) is the one delivery the question a restart left
	// never answers: such a task may be one nobody has ever sent — it is held on the claim while
	// its root has no architect to hear it, across restarts — and the decisions it names are held
	// by this delivery and told at its confirmation, so a turn of the agent's own taken for this
	// task's would confirm it unsent and tell a person's decision to nobody. The question is
	// resolved here rather than asked: while the root does not hear, the task stays pending and
	// unconfirmed and nothing is sent; once the root hears, it is sent. A send can meet an agent
	// still in a turn of its own, which answers it busy and leaves the task waiting, charged
	// nothing, with the claim recorded working for that turn (refused, recordBusyTurn), and the
	// turn's end sends it again: sending again is the lesser fault of the two. Its phase check
	// always holds — such a task names no phase — so a dropped task is never this one.
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
	if m.askFirst {
		m.askFirst = false
		if streaming, _ := m.streaming(ctx, conn); streaming {
			// The agent's answer is the daemon's first word of the turn, so the turn is dated
			// from it (Claim.LastTurnStart), as recordBusyTurn dates a turn from a busy answer,
			// and the confirmation's write carries the date: a let-go in this turn reads a report
			// older than it as no report of the turn (recordEnd), where the start of the turn
			// before it would count the report that turn's end left as one of this turn.
			m.claim.State, m.claim.LastTurnStart = StateWorking, m.deps.Clock.Now()
			// The turn is the agent's own word, not an agent_start this daemon read, so nothing
			// here knows what it has already done (Machine.unseenTurn).
			m.unseenTurn = true
			// The turn this claim is put in is logged before the write that records it, as
			// turnStarted logs its own: a line written after the write would say nothing of the
			// turn whose write failed.
			m.startTurn(p.ID)
			m.log.Info("supervise: the agent is in a turn; confirming the delivery an earlier daemon sent", "delivery", p.ID)
			return m.confirm(ctx)
		}
	}
	// Every role of an issue works one jj workspace, so a task that adopts its working copy adopts
	// it for all of them: an adoption run while another role is in a turn moves that role's worker
	// onto a fresh commit mid-write, and the work it goes on to do lands authored by this role —
	// a review App's adoption took the implementer's next commit that way (dispatch://INFRA-714).
	// The task waits for that turn instead. Nothing is written and nothing moves: the delivery is
	// left exactly as it was, and the hold is decided again, from the claims as they stand then,
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
		if other, inTurn := m.issueInTurn(); inTurn {
			m.log.Info("supervise: a claim of this issue is in a turn; the phase task waits rather than adopt the working copy they share",
				"delivery", p.ID, "issue", m.claim.Issue, "inTurn", other)
			return nil
		}
	}
	m.startSend(conn, *p)
	return nil
}

// asksRoot says whether a delivery's send waits on whether the root of this claim's tree has an
// architect to hear it (rootHears): a root architect's task of a run — it names no phase and a
// run, and the claim is its tree's root — on a daemon with a workflow to answer for it. It is also
// what says the question a restart left cannot confirm that task by itself (table.go's
// turnStarted): a task nobody has sent is no task in flight.
func (m *Machine) asksRoot(d Delivery) bool {
	return m.deps.RootHears != nil && d.Phase == "" && d.Generation != 0 && m.claim.treeRoot()
}

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
// gives it again. The id is kept across the rewrite: it is this delivery, under the row that
// queued it, with less in it than when it was queued, and the prompt that would be deduped against
// it carried more rather than less. The narrowed task is persisted before it is sent, so a restart
// reads the same task this send carried.
func (m *Machine) currentRootTask(ctx context.Context) (bool, error) {
	p := m.claim.Pending
	if m.deps.RootTask == nil {
		return true, nil
	}
	task, kept, err := m.deps.RootTask(ctx, m.claim, *p)
	if err != nil {
		return false, fmt.Errorf("read what the task held for %s says now: %w", m.claim.Token, err)
	}
	if task == p.Task && bytes.Equal(kept, p.Kept) {
		return true, nil
	}
	if task == "" {
		m.log.Info("supervise: everything this held task named is owed to another tree now; it is retired rather than sent",
			"delivery", p.ID, "issue", m.claim.Issue, "run", p.Generation)
		return false, m.retirePending(ctx)
	}
	next := *p
	next.Task, next.Kept = task, kept
	if err := m.saved(m.putDelivery(ctx, next)); err != nil {
		return false, err
	}
	*p = next
	m.log.Info("supervise: a decision this held task named is owed to another tree now; the task is sent without it",
		"delivery", p.ID, "issue", m.claim.Issue, "run", p.Generation)
	return true, nil
}

// answerAskFirstAfterDrop answers the question a restart armed for a delivery that is now gone
// (askFirst), and every drop of a delivery runs it in the same step as the drop: sendPending's
// three — the task its issue's phase has left behind, the ask of a round the claim is not in
// now, and the daemon's own delivery of a relaunch's record that no longer tells — the
// retirement's (machine.go's retire), and dropUpdateTask, which makes it of its own
// for the three drops a round that is over makes through it (leaveRound, endPodUpdate,
// openPodUpdate's round switch). The one drop that does not run it is
// currentRootTask's retirement of a root architect's held task that names nothing any more:
// sendPending's asksRoot branch clears askFirst before it, without asking, so no question is left
// to answer. Left armed over nothing pending, the question would make the claim's next task the
// running turn's — confirmed by a turn of another cause, never prompted, and retired unsent by
// that turn's end — and the worker would never receive that task at all.
//
// It asks what a send would have asked, and confirms nothing: no delivery is left to confirm, so
// an agent the ask finds in a turn only moves the claim to working, with the turn marked one this
// daemon never saw begin (Machine.unseenTurn) and dated from the answer (Claim.LastTurnStart),
// and the claim written. The ask is worth making with nothing pending because the claim's state
// is the daemon's record of whether its agent is busy, and the drop does not make that record
// current: discarded instead, the claim stays ready while its agent works, and the task that
// follows the dropped one is prompted into a busy agent, whose busy answer is what then records
// the claim working (recordBusyTurn). The turn is nobody's task's either way, and the next task
// waits for it to end.
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
	m.log.Info("supervise: the agent is in a turn of its own after its task was dropped", "claim", m.claim.Token)
	// The turn is dated from the agent's answer, the daemon's first word of it, as the askFirst
	// branch of sendPending dates the turn it confirms a task by (Claim.LastTurnStart).
	m.claim.State, m.claim.LastTurnStart, m.unseenTurn = StateWorking, m.deps.Clock.Now(), true
	// The guard above has already established that the claim holds no delivery, so this turn is
	// no task's and the line names none. It goes before the write, as the others do.
	m.startTurn("")
	return m.persist(ctx)
}

// relaunchAfterDrop gives a claim that one of sendPending's drops left holding nothing the
// daemon's own delivery of its record of the last let-go (queueRelaunch), when that record still
// tells, and sends it. The ready queues that delivery only for a claim that holds nothing when it
// is ready (finishReady), before the send that ready makes; a held task that send then drops —
// its phase or round gone, or a root architect's held task left naming nothing (currentRootTask)
// — leaves the claim holding nothing, and the sentence would wait for whatever task came next.
//
// It runs after the drop's retirement and after the answer to the question a restart armed for
// the dropped delivery (answerAskFirstAfterDrop), which asks only of a claim holding nothing, so
// that question is answered for the dropped delivery and never taken for the new one's. Nothing
// here recurses without end. queue sends nothing for a claim that holds no delivery, and the send
// below drops nothing it queues: the delivery has no phase, no round and no run, so neither the
// phase check nor the round's drops it, it asks no root (asksRoot), and it is queued only while
// its record tells, so it is not spent either. A record that does not tell queues nothing, and
// the send then finds nothing to send.
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

// retirePending drops the pending delivery, and is the one place a delivery ends: settle calls
// it for a delivery whose life is over, sendPending for a task the issue's phase has left
// behind and for the ask of a round the claim is not in now, dropUpdateTask for the ask of a
// round that is over, retire for the ask the claim ends with, and queue for the task a newer one
// replaces. The claim keeps its state — the task is stale, not the agent.
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
func (m *Machine) retirePending(ctx context.Context) error {
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
	m.claim, m.unsaved, m.outstanding = retired, false, nil
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
// - confirm's write covers both, and a delivery written confirmed beside a claim still ready is a
// turn a restart would find over. So once the store takes writes again, a restart reads what the
// machine decided rather than what it replaced; one before that reads the older rows, as a restart
// after a crash between a decision and its write does.
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
// (Machine.carried), and tells neither, as confirm states. Whether the send adopts the working
// copy first is decided here, under the machine's lock, so the goroutine acts on the claim the
// hold in sendPending was decided against.
func (m *Machine) startSend(conn runtime.Conn, d Delivery) {
	token, generation, role, loc := m.claim.Token, m.claim.Generation, m.claim.Role, m.claim.Locator
	adopting := m.adopts(d)
	prompt, carried := d.message(m.claim)
	m.carried.abandoned = m.carried.abandoned || carried.abandoned
	m.carried.ended = m.carried.ended || carried.ended
	if p := m.claim.Pending; p != nil && p.ID == d.ID {
		p.Prompted = true
	}
	m.send, m.helloDuringSend = &sending{id: d.ID, generation: generation}, false
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
// prompt is on its way to one, when there is such a claim. The answer is the state those claims'
// machines keep (Deps.IssueInTurn reads StateWorking, which a turn's start sets and its end
// clears, and the send in flight those machines publish beside it) rather than a second record of
// who is in a turn kept beside them: the machines' own state is the only account that moves with
// everything that ends a turn — the agent's own end of it, a process found dead, a suspension, a
// close — where a record of turn times would have to be told of each of those again.
func (m *Machine) issueInTurn() (claim.Token, bool) {
	if m.deps.IssueInTurn == nil || m.claim.Issue == "" {
		return "", false
	}
	return m.deps.IssueInTurn(m.claim.Issue, m.claim.Token)
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
	if err := m.deps.Runtime.AdoptWorkingCopy(ctx, *loc, id); err != nil {
		return err
	}
	m.log.Info("supervise: the working copy was adopted for the phase task",
		"delivery", d.ID, "phase", string(d.Phase), "identity", id.Name, "took", m.deps.Clock.Now().Sub(started))
	return nil
}

// confirm records that the pending delivery's turn started: the claim and the delivery together,
// because either half alone is a claim a restart reads wrongly — a working claim whose delivery
// is unconfirmed is re-sent under the same id and answered by the shim without a second turn, so
// its worker is never prompted again, and an idle claim holding a confirmed delivery holds one
// nothing retires. Once the write holds, the workflow is told the agent took its work
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
func (m *Machine) confirm(ctx context.Context) error {
	p := m.claim.Pending
	p.ConfirmedAt = m.deps.Clock.Now()
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
// abandoned (forgetSession): one the provider refused out of its budget, one an operator retried
// with --fresh-session, or one no session store holds any more. Nothing in a new session says
// that an older one existed, so a worker reading an empty transcript takes its task for a fresh
// start and does again what is already pushed (dispatch://INFRA-714, item 4 of the refusal
// section). It names the session that was left behind and why in the words the claim recorded —
// for a refused session the provider's own, the size it refused at among them — and then the two
// things that outlive a session: the issue's branch, and the handoff records its phases wrote on
// the Dispatch issue. The controller is told neither, since it works no checkout. last is whether
// this sentence is the last one before the task, and so ends in the task's own instruction: in
// front of a task with no record's sentence after it. Where the record's sentence follows it
// (EndedWork.Tells), that sentence is the last and carries the instruction instead
// (relaunchSentence); in front of the daemon's own delivery of a relaunch's record, which has no
// task (Delivery.Relaunch), neither carries one (Delivery.message).
//
// The reason is the provider's own text read into an agent's prompt, so it is quoted as a Go
// string (strconv.Quote), the way a check run's name is where the daemon reads one into a prompt
// (workflow's namedConclusions): the quotes bound it, and a newline or a quote inside it is
// escaped rather than ending the sentence that carries it, so no refusal text can read as an
// instruction of the daemon's own.
func abandonedTask(c Claim, last bool) string {
	read := "read the issue"
	if c.Role != claim.RoleController {
		read = fmt.Sprintf("read the issue, then your branch %s and the handoff records its phases wrote on "+
			"the issue, which `dispatch handoff-read --issue %s --phase <word>` reads (%s)",
			claim.IssueBranch(c.Issue), c.Issue, handoffWords())
	}
	sentence := fmt.Sprintf("The session this claim ran before this one, %s, was abandoned where it stood: %s. "+
		"This session is a new one: the turns, tool calls, subagents and background jobs of that session are "+
		"gone, and only what it pushed and committed survives. Before you repeat any of that work, %s, and "+
		"tell your architect what you found.",
		c.Abandoned.Session, strconv.Quote(c.Abandoned.Why), read)
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
// with no turn running is left as it is: the relaunch sends it as it was.
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
// process the claim records (Machine.cutTurnOf): the confirmation it clears is what the let-go
// that follows would otherwise read that from, and that let-go records it (recordEnd).
//
// What the confirmation it clears told stays told (confirm) where the result of the prompt it
// confirmed has come: that result says a turn ran the prompt, so the agent read the sentence the
// prompt carried, and the let-go that cuts that turn off takes no telling back (moveTelling). A
// confirmation a turn took while the send of its prompt was still in flight, or while a prompt of
// the task still waited on its result, may be another input's, and goes back to untold before the
// id moves (untellInFlight, where that rule is), in the same write as the delivery when a telling
// moved. The untelling stands in memory whether or not that write lands, since untold is the safe
// way round, and the claim's next write carries it. The record of the let-go that cut the turn off
// is new words, and is told by a prompt of its own.
func (m *Machine) interrupted(ctx context.Context, why string, outOfMemory bool) error {
	p := m.claim.Pending
	if p == nil || p.ConfirmedAt.IsZero() {
		return nil
	}
	m.log.Warn("supervise: "+why+"; the task waits for the relaunch", "delivery", p.ID)
	next := *p
	next.ID, next.ConfirmedAt, next.Interrupted, next.OutOfMemory = rotateTaskID(p.ID), time.Time{}, true, outOfMemory
	var err error
	if m.untellInFlight() {
		err = m.putClaimAndDelivery(ctx, m.stored(), next)
	} else {
		err = m.putDelivery(ctx, next)
	}
	if err != nil {
		return err
	}
	*p = next
	m.outstanding = nil
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
func (m *Machine) markRead(p *Delivery) {
	p.DeliveredAt, p.MarkedBy, p.Prompted = m.deps.Clock.Now(), p.ID, true
}

func (m *Machine) clearReadMark(p *Delivery) {
	p.DeliveredAt, p.MarkedBy, p.Prompted = time.Time{}, "", true
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

// awaitResult records id as a prompt of the pending task Oh My Pi acknowledged, whose result is
// still to come (Machine.outstanding). An id already held is held once: the shim answers a prompt
// it has a record of without a second turn (shimwire's dedupe), so its acknowledgement can come
// twice for one prompt.
func (m *Machine) awaitResult(id string) {
	if !slices.Contains(m.outstanding, id) {
		m.outstanding = append(m.outstanding, id)
	}
}

// answered drops id from the prompts the pending task waits on a result for: Oh My Pi answered
// it, a result or a refusal, and answers no prompt twice.
func (m *Machine) answered(id string) {
	m.outstanding = slices.DeleteFunc(m.outstanding, func(o string) bool { return o == id })
}

// outstandingPrompt is whether id is a prompt of the pending task still waiting on its result.
func (m *Machine) outstandingPrompt(id string) bool { return slices.Contains(m.outstanding, id) }

// awaitsResult is whether any prompt of the pending task is still waiting on its result.
func (m *Machine) awaitsResult() bool { return len(m.outstanding) > 0 }

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
func (m *Machine) takeBackPending(ctx context.Context, read bool) error {
	m.disarm(TimerTurn)
	p := m.claim.Pending
	id, kept := rotateTaskID(p.ID), m.awaitsResult()
	var moved bool
	if kept {
		moved = m.moveTelling(p.ID, id)
	} else {
		p.ConfirmedAt = time.Time{}
		if !read {
			m.clearReadMark(p)
		}
		moved = m.moveTelling(p.ID, "")
	}
	next := *p
	next.ID = id
	var err error
	if moved {
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

// moveTelling hands what the confirmation of delivery from told (confirm) to to, the id that
// confirmation stands under from here on, and reports whether any record had a telling to hand:
// each of the claim's two records whose ToldBy is from names to instead. to "" is the
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

// untellInFlight is the rule for a confirmation a let-go cannot vouch for. The pending task's
// confirmation is uncertain at a let-go while the send of its prompt is in flight (Machine.send)
// or any prompt of the task still waits on its result (awaitsResult). A turn that starts then is
// taken for the task's (beginTurn): with the send in flight, the INFRA-264 race, and with an
// acknowledged prompt outstanding, however late the turn starts. Either way the turn may be
// another input's, an Envoy notice's, and what would say so is still to come: the send's busy
// answer (refused), or the prompt's own answer, a busy answer or a late refusal that ran no turn
// (promptResulted, lateRefused, takeBackPending), each of which takes the confirmation back. The
// let-go forgets the send and the prompts outstanding (letGo, forgetSend; interrupted), so that
// answer is fenced out (fenceOutcome, fence) and never takes anything back. So the let-go takes
// back what the confirmation told (moveTelling): the record goes back to untold, the record of
// this let-go joins it rather than replacing it (recordEnd, joined), and the next prompt carries
// its sentence again. Where the turn was the task's own, the agent is told twice, which is the
// accepted cost: told twice, never not at all. A confirmation whose prompt's result has come
// stands: that result says a turn ran the prompt (promptResulted), so the agent read the sentence
// the prompt carried, and a let-go after it, in that turn or later, keeps the record told.
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
	p := m.claim.Pending
	if (m.send == nil && !m.awaitsResult()) || p == nil || p.ConfirmedAt.IsZero() {
		return false
	}
	return m.moveTelling(p.ID, "")
}
