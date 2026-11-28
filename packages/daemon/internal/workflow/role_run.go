package workflow

import (
	"slices"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// StopActs is whether a queued stop, row id, still acts on its claim when the outbox runs it, with
// lastStart the newest start the outbox ran against the claim (its last_start_row), and root the
// issue's tree root as recorded, read only for a tree close. The outbox asks it of every suspend
// and issue close of the issue's own tree, of every step stop, and of a tree close whose tree holds
// no cleanup reservation. StartFor asks it of every queued stop, so all read one rule. The other
// executor fences are listed on record.SuperviseRequest.Generation. A reserved cleanup close still
// retires its claim and drives the cleanup however stale its generation (reservedTreeClose). A tree
// close acts while its tree lingers at the root generation it names (record.Issue.LingersAt): never
// once re-admission has moved the root on. A suspend — an issue's close, a child's leave, or a
// re-entered child's interrupted run — a step stop — the process of the step a stopped task ran
// (stopTask) — a release — the idle pod's, which keeps the session and the phase — and an issue
// close — a child's leave as done — act unless a newer start has already run: that start replaced
// the run the stop was written for, so acting would stop the run it began and retire the task with
// it, whatever the retry timing was. A row with no id is not older than anything: the store gives
// every row one, and an unknown id must not silently drop a stop. Any other operation is not a
// stop.
func StopActs(id int64, stop record.SuperviseRequest, lastStart int64, root *record.Issue) bool {
	switch stop.Op {
	case "tree_close":
		return root != nil && root.LingersAt(stop.Linger)
	case "suspend", "step_stop", "release", "issue_close":
		return id <= 0 || id >= lastStart
	default:
		return false
	}
}

// actingStop is the newest queued stop of run that will still act when the outbox runs it
// (StopActs), by row id, and 0 when no queued stop will. root is the issue's tree root as
// recorded, which StopActs reads for a tree close, and the last start the outbox ran against the
// claim (record.RoleClaim.LastStartRow, 0 for a role the daemon holds no claim row of) is what
// every other stop is weighed against.
//
// Such a stop undoes every start older than it, and it may run before them, so every rule that
// reads it counts only a queued start newer than it: StartFor, asking what a re-admitted tree's
// promotion must start, OwedEntry, asking whether a role is owed its entry's work,
// unpromptedPhaseWorker, asking whether nothing will prompt a live claim, ArchitectRelaunchOf,
// asking whether a start is a root architect's next move, and catch_up.go's queuedRowCovers,
// asking whether a queued row covers a ready claim. One body is what keeps them from reading the
// same rows differently.
func actingStop(run record.RoleRun, root record.Issue) int64 {
	lastStart := lastStartRow(run)
	var stop int64
	for _, queued := range run.Queued {
		if StopActs(queued.ID, queued.Request, lastStart, &root) {
			stop = max(stop, queued.ID)
		}
	}
	return stop
}

// lastStartRow is the newest start the outbox ran against run's claim, 0 when run has no claim.
func lastStartRow(run record.RoleRun) int64 {
	if run.Claim == nil {
		return 0
	}
	return run.Claim.LastStartRow
}

// StartFor is whether a re-admitted tree's promotion starts the role of a mid-phase child, from
// run, the role's queued rows and claim for generation, with the child in phase current and root
// its re-admitted tree root. It does
// not when the role is already started for this run: the newest of its operations that will still
// act is a start. A stop that still acts (actingStop) undoes any older start, which may run first,
// so past one only a newer queued start for current counts. With none, a queued start for current
// counts, and so does a claim of root's tree that may have a process: a live one, or one whose
// launch is uncertain holding current's task for generation undelivered or unconfirmed (the task
// its start gave it). No start acts while the tree lingers, and the tree's close queues a suspend
// of every claim, so such a claim was started since the tree ran again. A claim still in another
// tree was not: a carried child's role whose turn in its old tree has not ended is live there, and
// promotion starts it, a start that waits until the claim has stopped in that tree (the outbox's
// stopElsewhere) and then resumes it in this one. A failed, retired, suspended or queued claim runs
// nothing, so promotion starts it whatever task it holds. Whether the start's resume task is
// delivered is the outbox executor's decision when the start runs: not to a claim that holds a task
// for the same generation and phase by then (record.SuperviseRequest's ResumeTask).
//
// Stranded reads the same rule for a failed claim, and for a suspended one that no holding
// suspension holds: a claim for which StartFor is true has no queued start that will wake it. A
// suspension that holds — the operator's or the controller's (supervise's
// RuleHolds) — is woken by no start, queued or not, since each waits for a resume to lift it, so
// Stranded lists it without asking. The architect's start names no phase, so its current is "".
//
// Its reading of run.Queued is queuedStartActs, which unpromptedPhaseWorker and catch_up.go's
// queuedRowCovers share.
func StartFor(run record.RoleRun, root record.Issue, generation uint64, current phase.Phase) bool {
	stop := actingStop(run, root)
	if queuedStartActs(run, stop, current) {
		return false
	}
	held := run.Claim
	if stop != 0 || held == nil || held.Tree != root.Key {
		return true
	}
	holds := held.Pending != nil && held.Pending.StillWorked(generation, current, held.State)
	return !slices.Contains(supervise.LiveStates(), held.State) && !(holds && held.State == supervise.StateLaunchUncertain)
}

// queuedStartActs is whether a queued start of run acts on the claim (startActs). StartFor,
// unpromptedPhaseWorker and catch_up.go's queuedRowCovers all read it, so they answer that term
// alike, and effects.go's architectStartQueued reads it with a stop of 0, which every row's id is
// above (the outbox's ids start at one), so any queued start for current counts.
func queuedStartActs(run record.RoleRun, stop int64, current phase.Phase) bool {
	for _, queued := range run.Queued {
		if startActs(queued, stop, current) {
			return true
		}
	}
	return false
}

// startActs is whether queued is a start for current with an id above stop, the newest stop that
// still acts (actingStop), which is a start that will reach the claim. queuedStartActs and
// ArchitectRelaunchOf both read it, so they count the same starts. A start queued for news alone
// is no such start (startsWork): it finishes without acting at a claim that failed or retired, so
// it is no relaunch of an architect's claim either.
func startActs(queued record.QueuedSupervise, stop int64, current phase.Phase) bool {
	return startsWork(queued.Request) && queued.Request.Phase == current && queued.ID > stop
}

// startsWork says whether a queued supervise row is a start that brings its role the work it names:
// every start but one queued for news of the approved design alone (record.SuperviseRequest.News).
// Such a start names the member's phase only so that it finishes once the member has left that
// phase. It carries no task of the phase and no entry. It reaches only a claim that was already
// given the phase's current entry, and it finishes without acting at a claim that failed or
// retired, or that a suspension that holds (the operator's or the controller's) is
// over (the daemon's newsOnlyRefusal), and wherever the record owes nothing when it runs. So every
// rule that asks whether a queued start will bring a role its phase's work reads the role's queued
// rows through this: the ready worker's task (queuedRowCovers), a promotion's start and the report
// of the claims nothing will wake (StartFor), the silence repair (unpromptedPhaseWorker), the
// second stall arm's next move for a root architect's claim (ArchitectRelaunchOf), the root's wait
// on its architect's work and the sweep's reading of a root architect's start on its way (owed,
// queuedStart), and the cutover's start of a stopped merger (oweCutoverMerger). Counted as a start,
// such a row would turn each of them off for as long as it waits, and a merger's waits for the
// whole time its design gate stays closed. An architect's would hold back the sweep's start of the
// entry its row names while the news waits behind a turn of the architect's own.
func startsWork(request record.SuperviseRequest) bool {
	return request.Op == "start" && !request.News
}

// OwedEntry is whether a role is owed the work the entry its phases row names stands for: entry
// is that number (record.PhaseRow.Entry), run the role's queued rows and claim for the issue's
// generation, and root the issue's tree root as recorded, which StopActs reads for a tree close.
// Admission asks it of every role of an issue at work and sends the ones it answers true for the
// work of that entry (admit's owedEntryRoles and resumeSlottedWorkers, SendOwedEntry).
//
// It answers true when the entry the claim was given (GivenEntry: record.RoleClaim.GivenEntry,
// and 0 for a role the daemon holds no claim row of at all, which was given nothing) is below
// entry, and no queued start of the role carries an entry at or above it. That second term is
// what keeps one entry to one start row: the given number moves when the outbox queues the
// delivery, not when the transition writes the row, and more than one reading of the entries can
// come between the two — an admission pass on each fact that writes a record, the tick's and a
// hold release's (the pass rule on admit's Admission.Apply), and the entry sweep's every minute —
// so without it each of them would write another start for the same entry. A stop that still
// acts (actingStop) undoes the start it outlives, which may run first, so past one only a queued
// start newer than the stop counts, which is the body StartFor reads the same rows with.
//
// Two tests the caller holds are deliberately not here, because each is about the issue rather
// than about the role's entry: that the role is the one that works the issue's current phase
// (record.Issue.RoleAt: phase.RoleFor, or at step the role of the running step), and that a worker of the issue has work where it stands (record.AtWork).
// admit's owedEntryRoles answers both from the records it has already read for the whole pass,
// and asking them here would mean reading the issue again per role.
//
// Where StartFor asks what a re-admitted tree's promotion must start, and so weighs whether the
// claim may already have a process, this reads nothing of the claim's state: a registered, ready
// or idle claim holding none of entry's work is the very claim this exists to close the
// difference for. The states no start may reach — a suspension that holds, a retirement — are
// the caller's own skips.
func OwedEntry(run record.RoleRun, root record.Issue, entry int64) bool {
	if GivenEntry(run) >= entry {
		return false
	}
	stop := actingStop(run, root)
	for _, queued := range run.Queued {
		if queued.Request.Op == "start" && queued.Request.Entry >= entry && queued.ID > stop {
			return false
		}
	}
	return true
}

// GivenEntry is the phase entry run's claim was last queued a task of
// (record.RoleClaim.GivenEntry), and 0 where the daemon holds no claim row of the role, which was
// given nothing. It is the one reading of that number the rules over a role's entry make:
// OwedEntry, resumeOwed and the admission pass's record of what each owed role was given (admit's
// owedOf).
func GivenEntry(run record.RoleRun) int64 {
	if run.Claim == nil {
		return 0
	}
	return run.Claim.GivenEntry
}

// queuedStart says whether a start of the role that brings it work still waits in the outbox
// (startsWork), which is one arm of owed and the only one the sweep's own reading keeps for a role
// with no claim at all (ArchitectStartPending). A start queued for news alone is not one: it
// reaches no claim that does not exist, and it hands no entry's work to one that does.
func queuedStart(run record.RoleRun) bool {
	for _, queued := range run.Queued {
		if startsWork(queued.Request) {
			return true
		}
	}
	return false
}

// startBringsBack is whether a queued start of the claim resumes it once the pod that refused the
// claim's launch is replaced (podRefusedLaunch): any start that reaches the claim's token at the
// issue's generation, whatever role its payload names (record.Store.ClaimQueued), the outbox start
// whose own launch the pod refused among them, but a wake's. A keyed agent's claim holds the role
// of each of its steps in turn, so the start of its next step reaches it as much as a start of the
// role it holds now. A wake's start (record.SuperviseRequest.WakeKind) finishes without acting on a
// claim the daemon suspended, and the daemon suspends every claim whose launch its pod refused
// (supervise's podRefused). A start of an earlier generation finishes without acting too, and
// ClaimQueued reads none.
func startBringsBack(queued []record.QueuedSupervise) bool {
	for _, row := range queued {
		if row.Request.Op == "start" && row.Request.WakeKind == "" {
			return true
		}
	}
	return false
}
