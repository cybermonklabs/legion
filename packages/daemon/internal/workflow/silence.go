package workflow

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// workerSilentNotice is the kind of the notice this handler writes (record's validNoticeKinds),
// beside `worker-stalled`, which is what the architect hears of a worker that stopped stepping
// inside a turn. This one is the worker that stopped between turns: nothing is running for it
// and nothing will prompt it, so the daemon sends its phase's task again and says so.
const workerSilentNotice record.NoticeKind = "worker-silent"

// workerSilent applies one silence the daemon's one-minute sweep read (intake.WorkerSilent). The
// sweep decides nothing: it reads the live claims over the supervisor, and the record can have
// moved since, so this handler reads the record again inside the fact's own transaction and is
// where the decision is made. Every read below answers nothing the moment it says there is
// nothing to do, and the rule the sweep and this handler share (SilentPhaseWorker) is asked
// once, with the values the record holds now and the moment and the bound the sweep wrote into
// the fact.
//
// The role the rule is asked about is the fact's, which is the claim the sweep read, and never
// the issue's own phase worker read in its place: the fact names one claim, and a step time of
// one claim says nothing about another's. What the issue's record settles is the other half of
// that term — PhaseWorkerRole, the role the work of the issue's own phase belongs to, which the
// fact's role must equal — and that half is asked first, before any other read, so a fact of a
// role that does not work the issue's phase starts nobody and reads no run of it. It still
// costs what every fact costs: one transaction under the fact lock, and the pass admission
// makes over the waiting line for every fact it is given (admit's Apply, promote).
//
// Any store error of any read or write below is returned as an error and never written over
// with a notice. The notice, the start row and the mark that this fact was applied are one
// transaction: intake.ApplyFact runs MarkProcessed inside the handlers' own transaction and
// rolls everything back when a handler returns an error. So a notice can never commit while the
// repair it describes failed. The handler returns the error, nothing commits, and the next
// sweep reads the same silence and applies the identical fact again, which is the retry — a
// notice written in place of the error would be the last word on this silence for ever, since
// the fact's identity is the claim token and a last step that does not move. The other half of
// that is what a reader of the architect's notice gets: a notice saying the start row was
// written is a notice whose start row is there, because the two commit together.
func (e *Engine) workerSilent(ctx context.Context, tx pgx.Tx, fact intake.WorkerSilent) (intake.Result, error) {
	read, err := e.phaseWorkerRecord(ctx, tx, fact.Issue, fact.Generation, fact.Role)
	if err != nil || read == nil {
		return intake.Result{}, err
	}
	issue, now := read.issue, e.now()
	if !SilentPhaseWorker(issue, read.root, read.gate, fact.Role, read.run, fact.Bound, now) {
		return intake.Result{}, nil
	}
	// The fact's own moment is held against the record's: a worker that took a turn between the
	// sweep's read and this transaction — an Envoy message, a person's steer, a background
	// result — is back to ready with a step of its own, and the silence this fact names is over.
	// The rule above judges the record's step and would name that worker silent again only once
	// a new bound had passed, which is a silence of its own for a later fact to carry; this
	// fact's notice would say it last stepped at a moment it has moved past.
	if read.run.Claim.Step.LastStep().After(fact.LastStep) {
		return intake.Result{}, nil
	}
	row, err := e.phaseRow(ctx, tx, issue.Key, fact.Role)
	if err != nil {
		return intake.Result{}, err
	}
	// A role whose completion of this phase entry the workflow accepted is left alone: an
	// implementing completion recorded with no pull request writes the row and moves the issue
	// nowhere (completed, implementationReady), and a reviewing round no review has decided is
	// the same. That worker finished its phase entry, so resuming it would tell a finished
	// worker to carry on and tell its architect that nothing will prompt a worker that is owed
	// no turn. The next phase's start clears the moment with the rest of the handoff
	// (clearHandoff), so a row carries it for the entry it was written in alone. The
	// controller's tick list does not ask this (admit's silentWorkers), so that list can name
	// an issue whose architect is told nothing.
	if !row.CompletedAt.IsZero() {
		return intake.Result{}, nil
	}
	return intake.Result{}, e.repairSilence(ctx, tx, issue, row, fact, now)
}

// phaseWorkerRecord is the record both facts the daemon's silence sweep writes are decided from,
// read inside the fact's own transaction: the issue, the root of its tree, that tree's design
// gate and the role's run. It answers nil for a fact there is nothing to do about — an issue the
// record holds none of, a run of the issue the sweep did not read, a role that does not work the
// issue's phase, a phase its own terminating fact has already met (PhaseFinished), or a key that
// yields no claim token — so each handler's own reading begins with a record that is worth
// deciding on. Every store error is returned, never written over.
//
// The role term (PhaseWorkerRole, which SilentPhaseWorker and backgroundStalled both ask again
// of the record) needs only the issue, which is in hand, so it is asked before the three reads
// below rather than among them. The sweep can filter on it for nobody: a supervise.Claim carries
// no issue phase, so it mints a fact for every quiet claim of a non-architect role, and most of
// those name a role that does not work their issue's phase. Each of them would otherwise spend a
// root, a gate and a run read inside the fact's own transaction, under the one advisory lock
// every fact of the daemon takes (intake.ApplyFact), only to be refused afterwards.
func (e *Engine) phaseWorkerRecord(ctx context.Context, tx pgx.Tx, key string, generation uint64, role claim.Role) (*phaseWorkerReading, error) {
	issue, err := e.store.Issue(ctx, tx, key)
	if err != nil || issue == nil {
		return nil, err
	}
	// A new run of the issue is not the run the sweep read: nothing of what it read survives
	// into it, since the generation's own claims, tasks and handoffs were cleared with it.
	if issue.Generation != generation {
		return nil, nil
	}
	if working := PhaseWorkerRole(*issue); working == "" || working != role {
		return nil, nil
	}
	// A phase its own terminating fact has already met has no worker owed a turn
	// (PhaseFinished): a merged root stands in merging, which record.AtWork holds at work, until
	// the rest of its tree leaves the workflow, which can be days. Its merger going quiet over
	// that stretch would otherwise be sent the task of a merge already done and its architect
	// told nothing will prompt it, again after every quiet spell, so neither handler writes
	// anything for it. The daemon's own resume paths skip the same issue for the same reason:
	// the boot's resume of every phase worker (booted, resume.go), the task of a worker that
	// says it is ready (readyWorkerTask, catch_up.go) and admission's start of a promoted tree's
	// members (admit's startMidPhaseMembers). It is asked right after the role term because it
	// reads a pull request only for an issue in merging, and before the three reads below,
	// which such an issue would otherwise spend for nothing.
	finished, err := e.phaseFinished(ctx, tx, *issue)
	if err != nil || finished {
		return nil, err
	}
	// The record of the root of the issue's tree, which is the issue's own row when the fact
	// names a tree root, read once rather than queried a second time for it (the rule treeRoot
	// states for a caller that has read neither).
	root := issue
	if issue.Tree != issue.Key {
		if root, err = e.store.Issue(ctx, tx, issue.Tree); err != nil || root == nil {
			return nil, err
		}
	}
	// The tree's design gate as the record builds one (record.Store.GatesOf), for the one issue
	// this fact names: the engine's own configured steps, and the rows of its root.
	rows, err := e.store.GatesOf(ctx, tx, issue.Tree)
	if err != nil {
		return nil, err
	}
	project, err := claim.ProjectToken(e.cfg.Project)
	if err != nil {
		return nil, err
	}
	// An issue whose key yields no claim token has no run to read, and a fact passes over it
	// rather than failing, as admission's repair of a slotted worker does (admit's
	// resumeSlottedWorkers).
	token, err := claim.NewToken(project, issue.Key, role)
	if err != nil {
		return nil, nil
	}
	run, err := e.store.RoleRun(ctx, tx, token, issue.Key, role, issue.Generation)
	if err != nil {
		return nil, err
	}
	return &phaseWorkerReading{issue: *issue, root: *root, gate: record.TreeGate{Steps: e.cfg.Steps, Rows: rows}, run: run}, nil
}

// phaseWorkerReading is what that read answers: the four values the rules over a quiet phase
// worker are asked with (SilentPhaseWorker, backgroundStalled), from one reading of the record
// inside one transaction.
type phaseWorkerReading struct {
	issue record.Issue
	root  record.Issue
	gate  record.TreeGate
	run   record.RoleRun
}

// repairSilence sends the phase's task again and tells the issue's architect what the daemon
// read and what it wrote. Both are the one transaction the handler runs in, which is what lets
// the notice say the start row is there (workerSilent).
//
// The resume goes through ResumeSlottedWorker, which hands the worker the assignment the row
// already names and mints none, so a completion still in flight is never refused for an
// assignment nothing ever delivered. A row naming none is resumed just the same, as admission's
// own repair of a slotted worker resumes one: the completion fence compares only when both
// sides are set (api/handoff.go), and re-admitting a tree empties every member's assignment
// (record.ClearGeneration), so a row with none is the ordinary shape of a re-admitted tree's
// member and not a worker that may not be started. row is the role's phase row the handler has
// already read, which this reads for the assignment the notice names alone; the start's own
// read of it is inside ResumeSlottedWorker's restart, the one named entry point both repairs of
// a slotted worker go through.
//
// The record it writes goes to the daemon's log at Warn as well, in this same step, through
// warnOnCommit: a fact that rolls back writes no line claiming a silence was repaired. An
// operator still sees a silence whose notice the store refused, because the sweep logs a failed
// apply of its own.
func (e *Engine) repairSilence(ctx context.Context, tx pgx.Tx, issue record.Issue, row record.PhaseRow, fact intake.WorkerSilent, now time.Time) error {
	if err := e.ResumeSlottedWorker(ctx, tx, issue, fact.Role); err != nil {
		return err
	}
	found := workerSilentRecord(issue, fact.Role, fact.LastStep, now.Sub(fact.LastStep), row.Assignment)
	if err := e.notice(ctx, tx, issue.Key, record.Notice{Kind: workerSilentNotice, Role: fact.Role, Phase: issue.Phase, Reason: found}); err != nil {
		return err
	}
	e.warnOnCommit(ctx, "workflow: a phase worker nothing will prompt was found silent; its architect is told",
		"issue", issue.Key, "role", fact.Role, "phase", issue.Phase, "record", found)
	return nil
}

// workerSilentDid is what the notice says the daemon did about the silence, and the whole of
// what it claims on that subject. It names the start row and never the worker's prompt: the
// outbox runs that row after this transaction commits, and a suspension an operator holds over
// the claim refuses it there (supervise's RuleHolds), which no record this handler reads can
// show. The assignment is named only where the phase row holds one.
func workerSilentDid(assignment string) string {
	did := "the daemon wrote the start row that sends the phase's task again"
	if assignment != "" {
		did += ", under the assignment " + assignment + " its phase row already names"
	}
	return did + ". A suspension an operator holds over the claim refuses that start when the outbox runs it"
}

// workerSilentRecord is what the architect's notice carries and what the log line beside it
// carries: the record the daemon read, in one sentence, and what it wrote. The issue, the role,
// the phase, the moment of the last step and the length of the silence are the record; the
// clause it closes with is workerSilentDid. The last step is the fact's own, which the handler
// has held to the record's (workerSilent), and never the zero time, the rule having refused a
// claim judged from no moment at all (SilentPhaseWorker).
func workerSilentRecord(issue record.Issue, role claim.Role, lastStep time.Time, silent time.Duration, assignment string) string {
	return fmt.Sprintf("%s's %s is a phase worker nothing will prompt: its process is up and between turns, it holds no task of %s, and its agent last stepped at %s, %s ago, past the bound this deployment gives a phase worker (worker_silence_minutes), so %s.",
		issue.Key, role, issue.Phase, lastStep.UTC().Format(time.RFC3339), silent.Round(time.Minute), workerSilentDid(assignment))
}

// workerBackgroundNotice is the kind of the notice the handler below writes (record's
// validNoticeKinds), beside `worker-silent`. That one is the worker nothing will prompt, which
// the daemon repairs; this one is the worker waiting on background work of its own that has
// stood too long, which the daemon tells and never prompts.
const workerBackgroundNotice record.NoticeKind = "worker-background-stalled"

// workerBackgroundStalled applies one reading of a worker waiting on background work that has
// stood past this deployment's bound (intake.WorkerBackgroundStalled), which the daemon's
// one-minute sweep made over the live claims. As with a silence, the sweep decides nothing and
// the record can have moved since, so the record is read again here, inside the fact's own
// transaction, and this is where the decision is made.
//
// It writes one notice and no start row, and that is the whole of what it does. The worker it
// names is one the silence repair must leave alone — its own session says background work of
// its own is running, and SilentPhaseWorker refuses every such claim — because nothing in the
// daemon can tell a worker with a forty-minute job from one whose job hung. So the architect is
// told what the session says and how long it has said it, and decides; the daemon sends
// nothing, and the notice says so (workerBackgroundDid).
//
// The structural half of the reading is the rule the silence handler asks as well
// (unpromptedPhaseWorker, through backgroundStalled): one record, one set of terms, so the two
// can never come to disagree about which claim is a phase worker nothing will reach. What this
// handler asks beyond it is the claim's own report — that it still says background work is
// running and still stands past the bound — and that the report is the one the fact names.
//
// Any store error of any read or write is returned and never written over with a notice, as the
// silence handler returns one: the notice and the mark that this fact was applied commit
// together (intake.ApplyFact), so a failed write leaves the fact unapplied and the next sweep
// reads the same report and applies the identical fact, which is the retry.
func (e *Engine) workerBackgroundStalled(ctx context.Context, tx pgx.Tx, fact intake.WorkerBackgroundStalled) (intake.Result, error) {
	read, err := e.phaseWorkerRecord(ctx, tx, fact.Issue, fact.Generation, fact.Role)
	if err != nil || read == nil {
		return intake.Result{}, err
	}
	issue, now := read.issue, e.now()
	if !backgroundStalled(issue, read.root, read.gate, fact.Role, read.run, fact.Bound, now) {
		return intake.Result{}, nil
	}
	// The fact's own moment is held against the record's, as a silence's is: a session sends a
	// report only when its reading changes, so a report at another moment is another reading —
	// the job ended and another began, or the worker took a turn and reported afresh — and the
	// standing of that reading is for the fact the sweep mints under its own moment to carry.
	// This fact's notice would say the work has stood since a moment the record no longer holds.
	if !read.run.Claim.ActivityAt.Equal(fact.ReportedAt) {
		return intake.Result{}, nil
	}
	found := workerBackgroundRecord(issue, fact.Role, fact.Kinds, fact.ReportedAt, now.Sub(fact.ReportedAt))
	if err := e.notice(ctx, tx, issue.Key, record.Notice{Kind: workerBackgroundNotice, Role: fact.Role, Phase: issue.Phase, Reason: found}); err != nil {
		return intake.Result{}, err
	}
	// The same record goes to the daemon's log at Warn, as the silence notice's does, and
	// through warnOnCommit: a fact that rolls back writes no line claiming an architect was
	// told.
	e.warnOnCommit(ctx, "workflow: a phase worker is waiting on background work that has stood past the bound; its architect is told",
		"issue", issue.Key, "role", fact.Role, "phase", issue.Phase, "record", found)
	return intake.Result{}, nil
}

// backgroundStalled is whether the record still shows what the sweep read: a phase worker
// nothing in the daemon will reach (unpromptedPhaseWorker, the half it shares with
// SilentPhaseWorker) whose own session reports background work of its own running, with that
// report older than bound. A bound of zero or less answers false for every run, as it does in
// SilentPhaseWorker and for the same reason: config.readHours refuses zero and every other
// non-positive value and the key defaults, so the only such bound is one a test passes. A
// claim whose session has reported nothing at all is judged from no moment and is left alone.
//
// It is the terms of this rule and not of SilentPhaseWorker because the two are opposite on one
// term and meet on every other: that rule refuses a claim whose session reports background work
// running, this one wants exactly that claim. The moments they judge differ for the same reason
// — the step a hung job leaves frozen against the report that job's own session last sent.
func backgroundStalled(issue, root record.Issue, gate record.TreeGate, role claim.Role, run record.RoleRun,
	bound time.Duration, now time.Time,
) bool {
	if bound <= 0 || !unpromptedPhaseWorker(issue, root, gate, role, run) {
		return false
	}
	c := run.Claim
	reported := c.ActivityAt
	return !reported.IsZero() && c.ActivityBackground && reported.Add(bound).Before(now)
}

// workerBackgroundDid is what the notice says the daemon did about the report, and the whole of
// what it claims on that subject: nothing was sent, nothing will be sent, and why no bound could
// decide it instead.
const workerBackgroundDid = "The daemon sent that worker nothing and will send it nothing for this: nothing can tell a background job that hung from a long one, so read what its session says it is waiting on and decide whether that work needs a decision of its own"

// workerBackgroundRecord is what the architect's notice carries and what the log line beside it
// carries: the record the daemon read, in one sentence, and what it did. The issue, the role,
// the phase, the kinds the session named and how long its report has stood are the record; the
// clause it closes with is workerBackgroundDid. The kinds are the fact's, which the sweep read
// off the claim beside the report itself, and the moment is the fact's too, which the handler
// has held to the record's (workerBackgroundStalled).
func workerBackgroundRecord(issue record.Issue, role claim.Role, kinds []string, reported time.Time, stood time.Duration) string {
	return fmt.Sprintf("%s's %s, the worker of %s, is waiting on background work of its own: its session reported %s at %s, %s ago, and has reported nothing since, which is past the bound this deployment sets for it (worker_background_hours). %s.",
		issue.Key, role, issue.Phase, workerBackgroundWork(kinds), reported.UTC().Format(time.RFC3339), stood.Round(time.Minute), workerBackgroundDid)
}

// workerBackgroundWork is how the notice names the work: the kinds the session reported, in the
// order it sent them (async-job, subagent, claude-background-task), and a clause of its own for
// a report that named none, since a notice cannot name what the session did not.
func workerBackgroundWork(kinds []string) string {
	if len(kinds) == 0 {
		return "background work running, naming no kind of it"
	}
	return "background work running of kind " + strings.Join(kinds, ", ")
}
