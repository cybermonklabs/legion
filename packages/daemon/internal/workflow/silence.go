package workflow

import (
	"context"
	"fmt"
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
// role that does not work the issue's phase starts nobody and costs nothing more.
//
// Any store error of any read or write below is returned as an error and never written over
// with a notice. The notice, the start row and the mark that this fact was applied are one
// transaction: intake.ApplyFact runs MarkProcessed inside the handlers' own transaction and
// rolls everything back when a handler returns an error. So a notice can never commit while the
// repair it describes failed. The handler returns the error, nothing commits, and the next
// sweep reads the same silence and applies the identical fact again, which is the retry — a
// notice written in place of the error would be the last word on this silence for ever, since
// the fact's identity is the claim token and a last step that does not move. The other half of
// that is what a reader of the architect's notice gets: a notice saying the phase's task was
// sent again is a notice whose start row is there, because the two commit together.
func (e *Engine) workerSilent(ctx context.Context, tx pgx.Tx, fact intake.WorkerSilent) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Issue)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	// A new run of the issue is not the run the sweep read: nothing of that silence survives
	// into it, since the generation's own claims, tasks and handoffs were cleared with it.
	if issue.Generation != fact.Generation {
		return intake.Result{}, nil
	}
	// The role term of the rule (SilentPhaseWorker, PhaseWorkerRole) needs only the issue, which
	// is in hand, so it is asked before the three reads below rather than among them. The sweep
	// can filter on it for nobody: a supervise.Claim carries no issue phase, so it mints a fact
	// for every quiet claim of a non-architect role, and most of those name a role that does not
	// work their issue's phase. Each of them would otherwise spend a root, a gate and a run read
	// inside this fact's own transaction, under the one advisory lock every fact of the daemon
	// takes (intake.ApplyFact), only to be refused here.
	if role := PhaseWorkerRole(*issue); role == "" || role != fact.Role {
		return intake.Result{}, nil
	}
	// The record of the root of the issue's tree, which is the issue's own row when the fact
	// names a tree root, read once rather than queried a second time for it (the rule treeRoot
	// states for a caller that has read neither).
	root := issue
	if issue.Tree != issue.Key {
		if root, err = e.store.Issue(ctx, tx, issue.Tree); err != nil || root == nil {
			return intake.Result{}, err
		}
	}
	// The tree's design gate as the record builds one (record.Store.GatesOf), for the one issue
	// this fact names: the engine's own configured steps, and the rows of its root.
	rows, err := e.store.GatesOf(ctx, tx, issue.Tree)
	if err != nil {
		return intake.Result{}, err
	}
	gate := record.TreeGate{Steps: e.cfg.Steps, Rows: rows}
	project, err := claim.ProjectToken(e.cfg.Project)
	if err != nil {
		return intake.Result{}, err
	}
	// An issue whose key yields no claim token has no run to read, and this fact passes over it
	// rather than failing, as admission's repair of a slotted worker does (admit's
	// resumeSlottedWorkers).
	token, err := claim.NewToken(project, issue.Key, fact.Role)
	if err != nil {
		return intake.Result{}, nil
	}
	run, err := e.store.RoleRun(ctx, tx, token, issue.Key, fact.Role, issue.Generation)
	if err != nil {
		return intake.Result{}, err
	}
	now := e.now()
	if !SilentPhaseWorker(*issue, *root, gate, fact.Role, run, fact.LastStep, fact.Bound, now) {
		return intake.Result{}, nil
	}
	return intake.Result{}, e.repairSilence(ctx, tx, *issue, fact, now)
}

// repairSilence sends the phase's task again where the record lets it and tells the issue's
// architect what happened either way. It is the one place both outcomes are written, so the
// notice of a silence the daemon could not repair is written beside the one it did repair
// rather than at a site of its own.
//
// The phase row is read for the assignment that row already names (the read restart makes), and
// that assignment decides the outcome. A row that names one is resumed through
// ResumeSlottedWorker, which hands the worker that same assignment and mints none, so a
// completion still in flight is never refused for an assignment nothing ever delivered. A row
// that names none gets no start row at all: a start under no assignment would hand the worker a
// fence it can never satisfy, and the notice says plainly that the worker is left with nothing.
// The row is therefore read twice, once here and once inside ResumeSlottedWorker's own restart:
// the repair goes through the one named entry point both its callers go through rather than
// through a copy of it that reads the row once.
//
// The record it writes goes to the daemon's log at Warn as well, in this same step, through
// warnOnCommit: a fact that rolls back writes no line claiming a silence was repaired. An
// operator still sees a silence whose notice the store refused, because the sweep logs a failed
// apply of its own.
func (e *Engine) repairSilence(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.WorkerSilent, now time.Time) error {
	row, err := e.phaseRow(ctx, tx, issue.Key, fact.Role)
	if err != nil {
		return err
	}
	outcome := workerSilentNoAssignment
	if row.Assignment != "" {
		if err := e.ResumeSlottedWorker(ctx, tx, issue, fact.Role); err != nil {
			return err
		}
		outcome = workerSilentTaskSent
	}
	found := workerSilentRecord(issue, fact.Role, fact.LastStep, now.Sub(fact.LastStep), outcome, row.Assignment)
	if err := e.notice(ctx, tx, issue.Key, record.Notice{Kind: workerSilentNotice, Role: fact.Role, Phase: issue.Phase, Reason: found}); err != nil {
		return err
	}
	e.warnOnCommit(ctx, "workflow: a phase worker nothing will prompt was found silent; its architect is told",
		"issue", issue.Key, "role", fact.Role, "phase", issue.Phase, "record", found)
	return nil
}

// workerSilentOutcome is what the daemon did about one silence, and the one value the clause
// the notice closes with is derived from (workerSilentDid). It is a value rather than two texts
// written beside the two branches so that no wording can claim a task was sent where the code
// sent none: the branch that sends chooses the outcome, and the clause is read from it.
type workerSilentOutcome int

const (
	// workerSilentTaskSent is the phase row that names an assignment, whose start
	// ResumeSlottedWorker wrote under that same assignment.
	workerSilentTaskSent workerSilentOutcome = iota
	// workerSilentNoAssignment is the phase row that names none, where no start was written at
	// all.
	workerSilentNoAssignment
)

// workerSilentDid is what the notice says the daemon did about the silence, and the whole of
// what it claims on that subject.
func workerSilentDid(outcome workerSilentOutcome, assignment string) string {
	if outcome == workerSilentTaskSent {
		return "the daemon sent the phase's task again, under the assignment " + assignment + " its phase row already names"
	}
	return "the daemon sent no task: the phase row names no assignment to send one under, so this worker is left with nothing and no later pass writes it a start"
}

// workerSilentRecord is what the architect's notice carries and what the log line beside it
// carries: the record the daemon read, in one sentence, and what it did about it. The issue, the
// role, the phase, the moment of the last step and the length of the silence are the record; the
// clause it closes with is the outcome (workerSilentDid). The last step is never the zero time,
// the rule having refused a claim judged from no moment at all (SilentPhaseWorker), so there is
// no reading here for a moment the record does not hold.
func workerSilentRecord(issue record.Issue, role claim.Role, lastStep time.Time, silent time.Duration, outcome workerSilentOutcome, assignment string) string {
	return fmt.Sprintf("%s's %s is a phase worker nothing will prompt: its process is up and between turns, it holds no task of %s, and its agent last stepped at %s, %s ago, past the bound this deployment gives a phase worker (worker_silence_minutes), so %s.",
		issue.Key, role, issue.Phase, lastStep.UTC().Format(time.RFC3339), silent.Round(time.Minute), workerSilentDid(outcome, assignment))
}
