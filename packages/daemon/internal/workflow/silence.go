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

// workerSilentNotice is the notice of a worker that stopped between turns, which the daemon
// repairs; `worker-stalled` is the one of a worker that stopped stepping inside a turn.
const workerSilentNotice record.NoticeKind = "worker-silent"

// workerSilent applies one intake.WorkerSilent, deciding on the record read inside the fact's own
// transaction (the sweep only filters: daemon's tellSilentPhaseWorkers).
//
// Every store error is returned and never written over with a notice. intake.ApplyFact commits
// the notice, the start row and the fact's applied mark together, so a returned error commits
// none of them and the next sweep applies the identical fact again, which is the retry. A notice
// written in place of the error would end this silence for good, since its id does not move.
//
// A claim unpromptedPhaseWorker holds but whose own session's report explains its quiet
// (ReportExplainsQuiet: background work running, or a reminder due later than now) leaves the
// fact unspent (intake.Result.Unspent): the report can move between the sweep's read and this
// transaction, and can clear — the reminder comes due and starts no turn — while the last step
// and the launch the id is keyed on stand still. Spent, that id would be skipped at every later
// sweep and the worker never repaired.
//
// A claim unpromptedPhaseWorker holds whose issue the daemon records as waiting on another issue
// (record.Issue.ReleasedForWaitAt) leaves the repair fact unspent too, at any length of quiet,
// and writes nothing. The repair's start cannot be delivered before the issue holds a slot
// again (startOrOwe marks it NeedsSlot), and while it waits it holds the pod from its idle
// release (IdleHoldDelivery). The mark clears when admission gives the issue a slot again
// (ClearReleasedForWait), and the first sweep after that applies this same fact under the same id,
// which then writes the start row and the notice, since SilenceBound reads the configured bound
// again. Such a worker's architect is told of the silence by the wait notice instead
// (workerSilentWaiting), once it is past WaitSilenceBound. Every other refusal spends the fact.
func (e *Engine) workerSilent(ctx context.Context, tx pgx.Tx, fact intake.WorkerSilent) (intake.Result, error) {
	read, err := e.phaseWorkerRecord(ctx, tx, fact.Issue, fact.Generation, fact.Role)
	if err != nil || read == nil {
		return intake.Result{}, err
	}
	issue, now := read.issue, e.now()
	if unpromptedPhaseWorker(issue, read.root, read.gate, fact.Role, read.run) {
		if ReportExplainsQuiet(read.run.Claim.ActivityBackground, read.run.Claim.ActivityNextReminderAt, now) {
			return intake.Result{Unspent: true}, nil
		}
		if issue.ReleasedForWaitAt != nil && !fact.Waiting {
			return intake.Result{Unspent: true}, nil
		}
	}
	if !SilentPhaseWorker(issue, read.root, read.gate, fact.Role, read.run, fact.Bound, now) {
		return intake.Result{}, nil
	}
	// A step after the fact's is a turn taken since the sweep read; that silence is over, and a
	// later one is a later fact's.
	if read.run.Claim.Step.LastStep().After(fact.LastStep) {
		return intake.Result{}, nil
	}
	if fact.Waiting {
		return intake.Result{}, e.workerSilentWaiting(ctx, tx, issue, fact, now)
	}
	return intake.Result{}, e.repairSilence(ctx, tx, issue, read.root, read.run, read.row, fact, now)
}

// workerSilentWaiting writes the wait notice of a silent worker whose issue the daemon records as
// waiting on another issue: one `worker-silent` notice to its architect that names
// WaitSilenceBound, and no start row. workerSilent has already judged the worker silent against
// that bound (SilentPhaseWorker, SilenceBound). The fact is spent once the notice is written, so
// the next minute's sweep, which skips an applied id (internal/daemon's applySweepFacts), writes
// no second notice of the same silence. One fact cannot both wait for the day and tell, because
// a fact left unspent is applied again at every sweep and Engine.notice writes a notice at every
// application.
//
// It writes nothing once the mark has cleared: the issue holds a slot again, and the repair fact
// of the same silence writes the start row and the notice at the next sweep.
func (e *Engine) workerSilentWaiting(ctx context.Context, tx pgx.Tx, issue record.Issue, fact intake.WorkerSilent, now time.Time) error {
	if issue.ReleasedForWaitAt == nil {
		return nil
	}
	found := workerSilentRecord(issue, fact.Role, fact.LastStep, now.Sub(fact.LastStep), waitSilence, workerSilentWaitDid)
	if err := e.notice(ctx, tx, issue.Key, record.Notice{Kind: workerSilentNotice, Role: fact.Role, Phase: issue.Phase, Reason: found}); err != nil {
		return err
	}
	e.warnOnCommit(ctx, "workflow: a phase worker of an issue waiting on another issue was found silent past the day; its architect is told",
		"issue", issue.Key, "role", fact.Role, "phase", issue.Phase, "record", found)
	return nil
}

// workerSilentWaitDid is what the wait notice says the daemon did: nothing, and when it does.
const workerSilentWaitDid = "the daemon wrote no start row: the issue gave its admission slot back to wait on another issue, and a start would wait for that slot and hold the pod meanwhile. " +
	"The first sweep after the issue holds a slot again sends the worker its phase's task if it is still quiet; to reach it before then, message it"

// phaseWorkerRecord reads what both silence facts are decided from: the issue, its tree root, the
// tree's design gate, the role's run and its phase row. It answers nil where there is nothing to
// do: no such issue, another run of it, a role that does not work its phase, a worker owed no
// turn (PhaseWorkerOwedTurn), or a key that yields no claim token. The two cheap refusals come
// before the other reads because the sweep cannot make them: a supervise.Claim carries no phase.
func (e *Engine) phaseWorkerRecord(ctx context.Context, tx pgx.Tx, key string, generation uint64, role claim.Role) (*phaseWorkerReading, error) {
	issue, err := e.store.Issue(ctx, tx, key)
	if err != nil || issue == nil {
		return nil, err
	}
	if issue.Generation != generation {
		return nil, nil
	}
	if working := OpenPhaseRole(issue.Phase); working == "" || working != role {
		return nil, nil
	}
	row, owed, err := PhaseWorkerOwedTurn(ctx, e.store, tx, *issue, role)
	if err != nil || !owed {
		return nil, err
	}
	root := issue
	if issue.Tree != issue.Key {
		if root, err = e.store.Issue(ctx, tx, issue.Tree); err != nil || root == nil {
			return nil, err
		}
	}
	rows, err := e.store.GatesOf(ctx, tx, issue.Tree)
	if err != nil {
		return nil, err
	}
	project, err := claim.ProjectToken(e.cfg.Project)
	if err != nil {
		return nil, err
	}
	// A key that yields no claim token has no run, and is passed over as admission's
	// resumeSlottedWorkers passes over one.
	token, err := claim.NewToken(project, issue.Key, role)
	if err != nil {
		return nil, nil
	}
	run, err := e.store.RoleRun(ctx, tx, token, issue.Key, role, issue.Generation)
	if err != nil {
		return nil, err
	}
	return &phaseWorkerReading{issue: *issue, root: *root, gate: record.TreeGate{Steps: e.cfg.Steps, Rows: rows}, run: run, row: row}, nil
}

// phaseWorkerReading is what phaseWorkerRecord answers.
type phaseWorkerReading struct {
	issue record.Issue
	root  record.Issue
	gate  record.TreeGate
	run   record.RoleRun
	row   record.PhaseRow
}

// repairSilence writes the start row that sends the silent worker its work again and the
// architect's notice, choosing that start by the entry the role's phases row names, as every
// start path of a phase worker does (catch_up.go's readyWorkerTask, resume.go's booted). row is
// that row, read once with the record the decision stands on (phaseWorkerRecord), below every
// skip workerSilent makes, so the row the rule is weighed against is the row whose work is sent.
//
// A claim behind that entry — the entry it was given is under the row's, and no queued start
// carries it (OwedEntry) — is sent the work of that entry at that entry (sendOwedEntry), the
// task the transition wrote when it raised the number. It carries no mark (startMarks{}), as
// admission's send of the same work does (SendOwedEntry): not resend, because the claim is
// behind the entry and the machine's entry test passes the task on its own; not resumeTask,
// because the task is that entry's own work rather than the phase's resume; and not ruleResume,
// because the claim is live, so no launch budget of a claim the daemon suspended by rule is at
// risk. The phase's resume task would be the wrong prompt there: it says to carry the existing
// phase work on, and the completion it answered with would be refused as behind that entry.
//
// A claim level with that entry is sent the phase's resume task (ResumePhaseTask) at entry 0,
// marked resumeTask, as readyWorkerTask's and the boot's level branches send it: entry 0 hands
// the role no number the record does not already name and moves no phases row, and resumeTask
// is what keeps the outbox from delivering a second prompt to a claim that holds its phase's
// task by the time the start runs (record.SuperviseRequest.ResumeTask). It is built through
// Engine.started, so it waits for the issue's admission slot as the boot's level start does.
//
// Neither start marks a movement of the issue (startMarks.reasserts). Both are the daemon
// re-asserting work the record already owes the worker: nothing outside the daemon happened, and
// the repair is the daemon reacting to a tree that has stopped, so marking it would make the
// tree's quiet time read the repair of a stop as the tree moving again and put the stalled-tree
// notice off for as long as the daemon kept repairing (startOrOwe says why). The send of an owed
// entry is unmarked where every such send is (sendOwedEntry); the resume task is marked so here.
// When the repair works, the worker's own steps move the tree through the quiet measure
// (record.QuietTrees), which is where a recovery is read.
//
// The log line goes through warnOnCommit, so a fact that rolls back logs no repair.
func (e *Engine) repairSilence(ctx context.Context, tx pgx.Tx, issue, root record.Issue, run record.RoleRun, row record.PhaseRow,
	fact intake.WorkerSilent, now time.Time,
) error {
	given := run.Claim.GivenEntry
	behind := OwedEntry(run, root, row.Entry)
	if behind {
		if err := e.sendOwedEntry(ctx, tx, issue, fact.Role, row, startMarks{}); err != nil {
			return err
		}
	} else if err := e.started(ctx, tx, issue, fact.Role, ResumePhaseTask(issue), 0, startMarks{resumeTask: true, reasserts: true}); err != nil {
		return err
	}
	found := workerSilentRecord(issue, fact.Role, fact.LastStep, now.Sub(fact.LastStep), configuredSilence, workerSilentDid(behind, row.Entry, given))
	if err := e.notice(ctx, tx, issue.Key, record.Notice{Kind: workerSilentNotice, Role: fact.Role, Phase: issue.Phase, Reason: found}); err != nil {
		return err
	}
	e.warnOnCommit(ctx, "workflow: a phase worker nothing will prompt was found silent; its architect is told",
		"issue", issue.Key, "role", fact.Role, "phase", issue.Phase, "entry", row.Entry, "given", given, "behind", behind, "record", found)
	return nil
}

// workerSilentDid is what the notice says the daemon did: which work the start row sends, read
// from the entry the phases row names (entry) and the one the claim was last given (given). It
// names the start row and not a prompt: a suspension held over the claim refuses the start when
// the outbox runs it (supervise's RuleHolds), which no record this handler reads shows.
func workerSilentDid(behind bool, entry, given int64) string {
	var did string
	switch {
	case behind:
		did = fmt.Sprintf("the daemon wrote the start row that sends the work of phase entry %d its phase row names, which its claim, last given entry %d, never had", entry, given)
	case entry > 0:
		did = fmt.Sprintf("the daemon wrote the start row that sends the phase's task again, on phase entry %d its phase row names and its claim was already given", entry)
	default:
		did = "the daemon wrote the start row that sends the phase's task again"
	}
	return did + ". A suspension an operator holds over the claim refuses that start when the outbox runs it"
}

// configuredSilence is the bound the repair's notice says the silence passed: this deployment's
// `worker_silence_minutes`, named by its setting alone.
const configuredSilence = "this deployment gives a phase worker (worker_silence_minutes)"

// waitSilence is the bound the wait notice says the silence passed: WaitSilenceBound, which a
// worker of an issue the daemon records as waiting on another issue is judged by (SilenceBound),
// named with its length in hours.
var waitSilence = fmt.Sprintf("the daemon gives a phase worker of an issue it records as waiting on another issue (WaitSilenceBound, %d hours)",
	int(WaitSilenceBound.Hours()))

// workerSilentRecord is the notice's and the log line's text: what the daemon read, the bound the
// silence passed, then what the daemon did, which workerSilentDid or workerSilentWaitDid composes.
func workerSilentRecord(issue record.Issue, role claim.Role, lastStep time.Time, silent time.Duration, bound, did string) string {
	return fmt.Sprintf("%s's %s is a phase worker nothing will prompt: its process is up and between turns, it holds no task of %s, and its agent last stepped at %s, %s ago, past the bound %s, so %s.",
		issue.Key, role, issue.Phase, lastStep.UTC().Format(time.RFC3339), silent.Round(time.Minute), bound, did)
}

// workerBackgroundNotice is the notice of a worker waiting on background work of its own that
// has stood too long, which the daemon tells of and never prompts.
const workerBackgroundNotice record.NoticeKind = "worker-background-stalled"

// workerBackgroundStalled applies one intake.WorkerBackgroundStalled, deciding on the record read
// inside the fact's own transaction as workerSilent does, and returning store errors as it does.
// It writes one notice and no start row: nothing in the daemon tells a long job from a hung one,
// so the architect decides.
func (e *Engine) workerBackgroundStalled(ctx context.Context, tx pgx.Tx, fact intake.WorkerBackgroundStalled) (intake.Result, error) {
	read, err := e.phaseWorkerRecord(ctx, tx, fact.Issue, fact.Generation, fact.Role)
	if err != nil || read == nil {
		return intake.Result{}, err
	}
	issue, now := read.issue, e.now()
	if !BackgroundStalledWorker(issue, read.root, read.gate, fact.Role, read.run, fact.Bound, now) {
		return intake.Result{}, nil
	}
	// A report of another moment than the fact's is another reading than the one the sweep
	// judged — a session sends one at its turns' ends, and an Oh My Pi session on each change of
	// what it reads too (packages/pi-legion/extensions/legion.ts,
	// packages/claude-envoy/src/legion-phase-hook.ts) — and its standing is a later fact's.
	if !read.run.Claim.ActivityAt.Equal(fact.ReportedAt) {
		return intake.Result{}, nil
	}
	found := workerBackgroundRecord(issue, fact.Role, fact.Kinds, fact.ReportedAt, now.Sub(fact.ReportedAt))
	if err := e.notice(ctx, tx, issue.Key, record.Notice{Kind: workerBackgroundNotice, Role: fact.Role, Phase: issue.Phase, Reason: found}); err != nil {
		return intake.Result{}, err
	}
	e.warnOnCommit(ctx, "workflow: a phase worker is waiting on background work that has stood past the bound; its architect is told",
		"issue", issue.Key, "role", fact.Role, "phase", issue.Phase, "record", found)
	return intake.Result{}, nil
}

// BackgroundStalledWorker is whether the record shows a claim unpromptedPhaseWorker holds whose
// session reports background work of its own running, with that report past bound. It is the
// claim SilentPhaseWorker refuses for that report. The `worker-background-stalled` handler reads
// it, and so does admission's tick list (admit's silentWorkers), which names such a worker too.
func BackgroundStalledWorker(issue, root record.Issue, gate record.TreeGate, role claim.Role, run record.RoleRun,
	bound time.Duration, now time.Time,
) bool {
	if !unpromptedPhaseWorker(issue, root, gate, role, run) {
		return false
	}
	c := run.Claim
	return c.ActivityBackground && PastBound(c.ActivityAt, bound, now)
}

// workerBackgroundDid is what the notice says the daemon did: nothing, and why.
const workerBackgroundDid = "The daemon sent that worker nothing and will send it nothing for this: nothing can tell a background job that hung from a long one, so read what its session says it is waiting on and decide whether that work needs a decision of its own"

// workerBackgroundRecord is the notice's and the log line's text: what the daemon read, then
// workerBackgroundDid.
func workerBackgroundRecord(issue record.Issue, role claim.Role, kinds []string, reported time.Time, stood time.Duration) string {
	return fmt.Sprintf("%s's %s, the worker of %s, is waiting on background work of its own: its session reported %s at %s, %s ago, and has reported nothing since, which is past the bound this deployment sets for it (worker_background_hours). %s.",
		issue.Key, role, issue.Phase, workerBackgroundWork(kinds), reported.UTC().Format(time.RFC3339), stood.Round(time.Minute), workerBackgroundDid)
}

// workerBackgroundWork names the work: the kinds the session reported, in its order, or a clause
// of its own for a report that named none.
func workerBackgroundWork(kinds []string) string {
	if len(kinds) == 0 {
		return "background work running, naming no kind of it"
	}
	return "background work running of kind " + strings.Join(kinds, ", ")
}
