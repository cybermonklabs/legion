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

// Delivery is a claim's pending task: at most one per claim, persisted, and handed to the claim's
// worker as a value it takes (take.go) rather than a prompt the daemon sends.
//
// Number is the task number the record queued it under, and the value the bucket holds for the
// claim names it. QueuedAt is when the task was queued; ConfirmedAt is when a take of it was
// accepted, and TakenText the text that take was answered with, which stays until the take's turn
// starts. So the three readings of a pending task are: ConfirmedAt zero, a task no take holds —
// queued, or taken back under its number; ConfirmedAt set with TakenText, a take accepted whose
// turn has not started (awaitsTurn); ConfirmedAt set with no TakenText, a take whose turn has
// started and which nothing has finished yet (turnStarted). A task lives until it is finished —
// by the turn-outcome report that names its number, or by the end of its turn with no report
// after it (finishTask) — or until the record drops it.
//
// The id is the daemon's and never changes: an outbox delivery id, an operator's minted one, or
// the round an update round's ask is of (updateTaskID), which is what names that ask's round.
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
	// Number is the task number this task was queued under (Claim.TaskWritten), counting from 1,
	// and 0 for a task of none. A take names the number it takes, so a take of any other number is
	// no take of this task.
	Number uint64
	// Origin is the source that queued this task — phase-start, resume, silence, wake, notice,
	// update-round, operator, check-look or controller-start — and "" for a task queued before
	// the record kept one (migration 20261113175649_task_numbers).
	Origin string
	// TakenText is the text the daemon's accepted take of this task was answered with, and ""
	// while no take of it has been accepted. A take of the same number sent again after its answer
	// was lost is answered this same text, across a daemon restart too, so it is persisted (the
	// store writes "" as null).
	TakenText string
	// CheckLook is the look of a closed issue's production check this task is of, from the start
	// row that queued it (record.SuperviseRequest.CheckLook and CheckOpenedAt). It is what says a
	// turn's completion is a check report rather than a phase completion: the API routes on the
	// look the claim's worker took (Claim.ServingCheck), and the report is of that look rather
	// than of whichever look the check has reached (api/handoff.go, checkReport). The zero value
	// is a task of no look: every phase task, an operator's own, a resume.
	CheckLook CheckLook
	Phase     phase.Phase
	QueuedAt  time.Time
	// ConfirmedAt is when a take of this task was accepted (accept), and the zero time for a task
	// no take holds: one still queued, or one taken back under its number. It is what the store
	// tells the decisions the task names on (store's putDelivery).
	ConfirmedAt time.Time
	// Interrupted says a turn of the task was running when its process died (interrupted), so the
	// task's next accepted take carries interruptedTask in front of it (message). The task text
	// stays as the workflow wrote it.
	Interrupted bool
	// OutOfMemory says the end that cut a turn of this task off was its pod running out of memory
	// (runtime.Observation.OutOfMemory), so the task's next accepted take carries outOfMemoryTask
	// as well as interruptedTask (message). The task text stays as the workflow wrote it.
	OutOfMemory bool
	// SendFailures, LastSendFailureAt and LastSendError are the diagnosis record of this task's
	// takes whose adoption of the working copy failed (refusedAdoption): how many of them failed,
	// when the last one failed by the machine's clock, and the error it failed with. They are a
	// record of what happened, not a budget. The budget is the claim's
	// (Claim.Budgets.PromptFailures and PromptRetires), which only a refused adoption — the shim's
	// answer about this workspace — spends; this counts every adoption that failed, charged or
	// not, and that difference is the point of recording it. An adoption lost to the transport is
	// charged nothing and waits for the next take, so a claim can sit with its budget untouched and
	// nothing on it saying why, and that is the case an operator most needs named (INFRA-1480: two
	// planning tasks held 31 and 33 hours, whose only record of each failed send was a log line
	// inside the pod). All three are written together on every adoption that fails, and cleared
	// together where a take is accepted (accept), so a task that got through carries zeroes.
	// LastSendError is never the far side's text as it arrived: the write that records it scrubs
	// and bounds it first (scrubSendError), because the text is the far side's own and the store
	// takes only some of what the far side can say.
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
	// ProviderErrorRetries is how many times this task has been taken back after a turn of it
	// ended in a provider error, from 0 (providerErrored). It is what the wait before the value is
	// written again is a function of (promptRetryDelay), so a daemon restarted in the middle of a
	// ladder goes on climbing it rather than starting again at the first step. It is the task's
	// own count, not the claim's: a new task starts at 0.
	ProviderErrorRetries int
	// ProviderErrorNote is the sentence such a task's next accepted take carries, as Interrupted
	// carries interruptedTask: what ended the previous turn, and that the daemon gives the task
	// again. It is built from structured fields alone — the HTTP status the provider answered,
	// else nothing more than "a provider error" — because a refusal's own explanation is itself
	// refused when it is sent back (dispatch://INFRA-956/artifact/cause-evidence-md), so no
	// provider text reaches a task's text. Empty for a task no provider error has taken back.
	ProviderErrorNote string
}

// taken is whether a take of the task was accepted and has not been taken back.
func (d Delivery) taken() bool { return !d.ConfirmedAt.IsZero() }

// awaitsTurn is whether a take of the task was accepted and its turn has not started: the
// window in which a take sent again is answered the same text, and in which the claim counts as
// in a turn for another role's adoption (Claim.InTurn).
func (d Delivery) awaitsTurn() bool { return d.taken() && d.TakenText != "" }

// turnStarted is whether the task's accepted take has had its turn start: the turn is running,
// or it has ended and nothing has finished the task yet (finishTask).
func (d Delivery) turnStarted() bool { return d.taken() && d.TakenText == "" }

// TakenWith is whether c's worker took this task: the claim's last accepted take is this task's
// number. It stays true after the task is taken back, since the worker read the task and may be
// carrying on its work (Claim.ServingRun).
func (d Delivery) TakenWith(c Claim) bool { return d.Number != 0 && c.TaskTaken == d.Number }

// maxSendErrorRunes bounds the text of a failed send kept on a delivery (scrubSendError). What a
// reader of that record needs is why the send failed: the first line of the far side's answer and
// enough of what follows to name the failure — the shim's words name the stale lock's path and
// its age within the first two hundred characters — so this is more room than the diagnosis asks
// for and far less than the text can be. What it cuts is the rest of a relayed command's output:
// a refused adoption carries the wedged command's whole stderr, measured at about a megabyte
// (INFRA-1480), which would otherwise be stored on every failed send of the task and served
// again in every operator listing of the claim.
const maxSendErrorRunes = 2048

// scrubSendError is the text of a failed adoption as a delivery may hold it. The text is the far
// side's own and is not this daemon's to trust: a refused adoption carries the wedged command's
// whole stderr, relayed by the shim, where a `\u0000` in it can become a NUL byte.
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
// to the log line this record stands beside (refusedAdoption), which has no store to refuse
// it, so the operator who needs the megabyte still reads it in the pod's log.
func scrubSendError(text string) string {
	text = strings.ReplaceAll(strings.ToValidUTF8(text, "\uFFFD"), "\x00", "")
	if runes := []rune(text); len(runes) > maxSendErrorRunes {
		return string(runes[:maxSendErrorRunes]) + "…"
	}
	return text
}

// StillWorked says whether a claim in state will still work this task as the task of generation's
// run in phase p: it is that run's task for that phase, and its turn is still to come — queued, a
// task taken back included, or taken and awaiting its turn — or running in the turn the claim is
// working. A task whose turn started on a claim not working is over: what finishes it is the
// turn's report, or the end of that turn (finishTask).
func (d Delivery) StillWorked(generation uint64, p phase.Phase, state ClaimState) bool {
	return d.Generation == generation && d.Phase == p && (!d.turnStarted() || state == StateWorking)
}

// message is the text an accepted take of a delivery is answered with: its task, behind the
// sentence the claim's state gives it. The first task the agent that replaced an abandoned
// session takes names that session (abandonedTask); a task whose turn a process died in names the
// death's cause (interruptedTask, Claim.LastDeathCause), and behind that note, when the death was
// the pod running out of memory, outOfMemoryTask — the agent reads that its turn was
// interrupted, then why, then the task. A fresh session cannot carry on the turn it never read,
// so the abandonment stands in place of the interruption when a task is both. A task a provider
// error took back carries its own sentence last of all (ProviderErrorNote), so the agent reads
// what the provider did and then the task; a task that took more than one of these keeps every
// sentence, in that order, and each says something the others do not. One case still carries a
// stale note: the previous release's upsert carries no out_of_memory column, so if an image
// rollback takes a marked task back again for another end, the row keeps the mark, and a
// roll-forward carries the note for that end.
func (d Delivery) message(c Claim) string {
	var prefix string
	switch {
	case c.Abandoned.Tells():
		prefix = abandonedTask(c)
	case d.Interrupted:
		prefix = interruptedTask(c.LastDeathCause)
		if d.OutOfMemory {
			prefix += outOfMemoryTask
		}
	}
	if d.ProviderErrorNote != "" {
		prefix += d.ProviderErrorNote + "\n\n"
	}
	return prefix + d.Task
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
// writes the claim's value under that number (writeValue): the value is the worker's one signal to
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
// A request a pending task refuses offers that task first (offerPending), so a task held back
// because its root had no architect to hear it never wedges the claim: the start of the root's
// next run, retrying on its backoff, is what releases it — its value is written again and the
// worker takes it — and the refusal that start is answered with is over once the task is finished.
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
// nobody, so accepting over it would lose a task rather than save one. One untaken task is not
// refused: the task a provider error took back, while the ladder's wait stands for it and nothing
// works it (replaceable). The next step of the ladder can be ten minutes away and a run of errors
// has no end of its own, so a refusal would keep every newer task of the claim behind a task the
// claim owes for as long as the provider keeps failing (dispatch://INFRA-956 R2: a newer task
// replaces the owed one). The newer task takes its place: it is work for this claim, queued
// since, and R2 has the newer of the two go.
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
// a claim that holds a pending delivery its worker has not finished with and no provider error
// took back. runParkedLaunchNow reads the same pending delivery before it runs a parked launch
// for such a task, so nothing runs for one this refuses; a task that replaces an owed one runs
// that launch where it arrives, and retires the owed task itself before it does — the launch
// takes the ladder's wait with the claim's timers (letGo), so the claim reaching here then holds
// no delivery at all.
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
			if err := m.offerPending(ctx); err != nil {
				return err
			}
			return &RefusedError{State: m.claim.State, Request: "deliver", Err: ErrDeliveryPending}
		case !pending.taken():
			owed, attempt := pending.Number, pending.ProviderErrorRetries
			replaced, err := m.replaceOwedTask(ctx)
			if err != nil {
				return err
			}
			if !replaced {
				if err := m.offerPending(ctx); err != nil {
					return err
				}
				return &RefusedError{State: m.claim.State, Request: "deliver", Err: ErrDeliveryPending}
			}
			m.log.Info("supervise: a newer task replaces the one a provider error took back",
				"owed", owed, "attempt", attempt, "delivery", id)
		case pending.awaitsTurn():
			// The take is in the worker's hands and its turn has not started: the task is not
			// over, and a newer one waits behind it as behind any task its worker has not done.
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
	d := Delivery{ID: id, Task: task, Phase: request.Phase, Generation: request.Generation, Kept: request.Kept,
		Entry: request.Entry, CheckLook: request.CheckLook, QueuedAt: m.deps.Clock.Now(),
		Number: m.claim.TaskWritten + 1, Origin: originOf(request)}
	// The claim records the row, the entry and the number only once the store has them with the
	// delivery: a write that failed leaves the row undelivered, the entry ungiven and the number
	// unwritten, so its retry queues the task under the same number.
	stored := m.stored()
	stored.TaskWritten = d.Number
	if epoch, revision, written := m.putValue(ctx, d, "queued"); written {
		stored.TaskWrittenEpoch, stored.TaskWrittenRevision = epoch, revision
	}
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
	m.claim.TaskWritten = stored.TaskWritten
	m.claim.TaskWrittenEpoch, m.claim.TaskWrittenRevision = stored.TaskWrittenEpoch, stored.TaskWrittenRevision
	m.claim.Pending, m.unsaved = &d, false
	return nil
}

// answeredOpenRound is the hold an open update round puts on its claim: the round is open and
// the worker has answered it (ready_for_update, or the turn's end of a worker that cannot call
// it), so that worker has said its work is pushed and its process may be replaced at any moment.
// A take of such a claim's task is refused by it (claimHold).
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
// no retry gives it again. The id and the number are kept across the rewrite: it is this
// delivery, under the row that queued it, with less in it than when it was queued. The narrowed
// task is persisted before it is taken, so a restart reads the same task the take answered.
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
		m.log.Info("supervise: everything this held task named is owed to another tree now; it is dropped rather than taken",
			"task", p.Number, "delivery", p.ID, "issue", m.claim.Issue, "run", p.Generation)
		return false, m.dropPending(ctx, "dropped")
	}
	next := *p
	next.Task, next.Kept = task, kept
	if err := m.saved(m.putDelivery(ctx, next)); err != nil {
		return false, err
	}
	*p = next
	m.log.Info("supervise: a decision this held task named is owed to another tree now; the task is taken without it",
		"task", p.Number, "delivery", p.ID, "issue", m.claim.Issue, "run", p.Generation)
	return true, nil
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
// queue, for the task a newer one replaces, and replaceOwedTask, for the task a provider error
// took back that a newer one replaces (retireTask, which every end of a delivery goes through).
func (m *Machine) retirePending(ctx context.Context) error {
	return m.retireTask(ctx, false)
}

// retireTask ends the pending delivery, and is the one place a delivery ends: finishTask for a
// task whose take's turn ran, dropPending for a task the record no longer owes — the task its
// issue's phase has left, the ask of a round the claim is not in now, a root architect's task
// that names nothing, the ask the claim retires with, and a phase task a suspension ended
// (settle) — and retirePending for a task a newer one replaces. The claim keeps its state — the
// task is over, not the agent. finished says the task's turn ran, so the claim records it as the
// last task it finished (Claim.TaskFinished) and the session that finished it
// (TaskFinishedSession), in the same write.
//
// A task of a run whose take's turn started leaves the run behind it, and its phase entry and its
// check look with it: the worker goes on working it in whatever turn comes next — one an Envoy
// notice starts, with no delivery of its own — and its completion belongs to that run, that entry
// and that look. Only a delivery whose take's turn started moves them, and only a task that
// carries them: an operator's task carries no generation, no entry and no look, a resume that
// raised no entry and a production check's look no entry, and a phase task no look, and each
// leaves what the claim is serving alone. Every such retire writes the row, the second task of one
// run included: the write is unconditional on the value, not a diff. Which value each takes is
// the claim's own accessors, read here off the claim this delivery is still pending on, so what
// the claim is left serving is by construction what ServingRun, ServingEntry and ServingCheck
// answered while that delivery was pending.
//
// A task of an entry no turn of a take ran goes the other way — untaken, or taken and retired
// before its turn started, which never ran its text: Claim.GivenEntry is put back to
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
func (m *Machine) retireTask(ctx context.Context, finished bool) error {
	p := m.claim.Pending
	retired := m.stored()
	if p.turnStarted() {
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
	// what finishes it later.
	m.disarm(TimerPromptRetry)
	m.disarm(TimerFinish)
	m.claim, m.unsaved = retired, false
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

// streaming asks the agent whether a turn is in flight; answered is false when the agent could not
// say (the ask failed or timed out). Its one caller is the question a restart leaves over a claim
// stored working (askTurnOver).
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
// name — never a token — and how long the shim took. The refusals are logged where the take's
// refusal is (refusedAdoption); without this line nothing in the log tells which tasks adopted the
// working copy, which is what a production check reads to see the rule held (dispatch://INFRA-2695).
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
// at a turn's end and on a ready or idle restore only (turnEnded, NewMachine). Such a task is
// finished here, by its end frame, as the bound would have finished it; left, it would stand
// started for good, and the take of it the next process makes would be refused in-turn.
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
	switch m.claim.State {
	case StateWorking, StateReady, StateIdle:
	default:
		if p.turnStarted() {
			return m.finishTask(ctx, "end-frame")
		}
	}
	if m.claim.State != StateSuspended || p.Phase == "" || !m.claim.SuspendedBy.RetiresPhaseTask() {
		return nil
	}
	return m.dropPending(ctx, "suspended")
}

// interruptedTask begins the text of a task taken again because the process running its turn
// died. Oh My Pi does not continue an interrupted turn when its session is resumed, and the
// resumed session already holds the task, so the agent is told to carry on from where the turn
// stopped, not start over.
// It says why the process died, in the runtime's own words (Claim.LastDeathCause), and what the
// death took with it: a tool call in flight answered nobody, and every subagent and background job
// of that turn is gone, neither of which the resumed transcript shows as missing — a worker
// relaunched six times in an hour and told none of this read its own transcript as work in
// progress (dispatch://INFRA-714 R6). A death the daemon recorded no cause for says so plainly
// rather than inventing one.
func interruptedTask(cause string) string {
	if cause == "" {
		cause = "the daemon recorded no cause for it"
	}
	return "Your previous turn on this task was interrupted when your process died: " + cause + ". " +
		"Your tool calls in flight were never answered, and the subagents and background jobs of that " +
		"turn are gone; your transcript does not say so. " +
		"Before repeating anything, check what that turn already did in your workspace and on the " +
		"issue's branch, then continue the task.\n\n"
}

// abandonedTask begins the first task given to the agent that replaced a session the claim
// abandoned (forgetSession): one the provider refused out of its budget, one an operator retried
// with --fresh-session, or one no session store holds any more. Nothing in a new session says
// that an older one existed, so a worker reading an empty transcript takes its task for a fresh
// start and does again what is already pushed (dispatch://INFRA-714, item 4 of the refusal
// section). It names the session that was left behind and why in the words the claim recorded —
// for a refused session the provider's own, the size it refused at among them — and then the two
// things that outlive a session: the issue's branch, and the handoff records its phases wrote on
// the Dispatch issue. The controller is told neither, since it works no checkout.
//
// The reason is the provider's own text read into an agent's prompt, so it is quoted as a Go
// string (strconv.Quote), the way a check run's name is where the daemon reads one into a prompt
// (workflow's namedConclusions): the quotes bound it, and a newline or a quote inside it is
// escaped rather than ending the sentence that carries it, so no refusal text can read as an
// instruction of the daemon's own.
func abandonedTask(c Claim) string {
	read := "read the issue"
	if c.Role != claim.RoleController {
		read = fmt.Sprintf("read the issue, then your branch %s and the handoff records its phases wrote on "+
			"the issue, which `dispatch handoff-read --issue %s --phase <word>` reads (%s)",
			claim.IssueBranch(c.Issue), c.Issue, handoffWords())
	}
	return fmt.Sprintf("The session this claim ran before this one, %s, was abandoned where it stood: %s. "+
		"This session is a new one: the turns, tool calls, subagents and background jobs of that session are "+
		"gone, and only what it pushed and committed survives. Before you repeat any of that work, %s, and "+
		"tell your architect what you found. Then continue the task.\n\n",
		c.Abandoned.Session, strconv.Quote(c.Abandoned.Why), read)
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

// outOfMemoryTask follows interruptedTask on a task whose turn ended when its pod ran out of
// memory. The agent cannot see why the kubelet ended its container, and the heavy work that
// filled the pod is the thing it must stop doing there, so it is told what happened and where
// the rule about work a pod does not run is written.
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
// marked Interrupted, so its next accepted take carries interruptedTask; outOfMemory says that
// end was the pod running out of memory, which adds outOfMemoryTask, and it is written on every
// such call, true or false, so a second interruption of the same task for another reason clears
// a note that no longer describes it. A take whose turn had not started goes back with no note,
// since the worker ran nothing of it. The task is neither retired nor its run marked served,
// since the turn never finished; the claim's TaskTaken stays, since the worker read it. A task no
// take holds is left as it is.
//
// A take whose turn has already ended is not taken back but finished: its turn ran whole, and
// only the report of it is lost with the process (finishTask, by the end frame).
//
// The taken-back task becomes the claim's only once its write has landed. A write that fails
// leaves the task taken in memory as in the store, and the process is still recorded, so the
// sweep finds it gone again and this takes the task back then — where a task taken back in memory
// alone would be skipped by that death and relaunched from a store still holding it taken.
func (m *Machine) interrupted(ctx context.Context, why string, outOfMemory bool) error {
	p := m.claim.Pending
	if p == nil || !p.taken() {
		return nil
	}
	inTurn := m.claim.State == StateWorking
	if p.turnStarted() && !inTurn {
		return m.finishTask(ctx, "end-frame")
	}
	m.log.Warn("supervise: "+why+"; the task waits for the relaunch", "task", p.Number, "delivery", p.ID)
	next := *p
	next.ConfirmedAt, next.TakenText = time.Time{}, ""
	if inTurn {
		next.Interrupted, next.OutOfMemory = true, outOfMemory
	}
	if err := m.putDelivery(ctx, next); err != nil {
		return err
	}
	*p = next
	m.disarm(TimerFinish)
	m.log.Info("supervise: a task was taken back", "task", p.Number, "cause", why)
	return nil
}

// ordinaryEnd is interrupted's outOfMemory at a call site that ends the turn of a process still
// running — one replaced mid-turn, for a stale address, a stale plugin, a stalled turn or a paced
// launch whose turn has come, or one a held suspension's deadline stops — where nothing ended the
// pod's container, so the end was not the pod running out of memory. A death and a displacement
// pass the observation's own OutOfMemory instead, and the suspension that lands over a process a
// report found gone passes what that report recorded (suspendOverADeadProcess, goneOutOfMemory).
const ordinaryEnd = false
