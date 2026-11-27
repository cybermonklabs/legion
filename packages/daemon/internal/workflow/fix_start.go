package workflow

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// What a start of a step of a task is told of the task's fix and of its CI. The answer of the read
// of a fix's failed job starts the fix (fixJobRead, startFix), whose own start is told the fix's
// task (fixTask). Every start's sentences of a fix and of the task's CI are built from the start's
// typed facts alone (startFacts, fixLines): the fix's own, a step's beside the fix, and a step's
// whose branch beside the fix a way 2 kept.

// fixJobRead applies the answer of the read of a fix's failed job (record.FixJobRead): the fix
// keeps the job the read found, or the read's error where it found none, and starts (startFix). A
// fact whose fix no longer reads its job — it ended, or the issue left the run it was of — changes
// nothing, which the log says.
func (e *Engine) fixJobRead(ctx context.Context, tx pgx.Tx, fact intake.FixJobRead) (intake.Result, error) {
	issue, err := e.store.Issue(ctx, tx, fact.Key)
	if err != nil || issue == nil {
		return intake.Result{}, err
	}
	fix, err := e.store.RunningFix(ctx, tx, fact.Key, fact.Generation, fact.Task)
	if err != nil {
		return intake.Result{}, err
	}
	if issue.Generation != fact.Generation || fix == nil || fix.Fix != fact.Fix || fix.State != record.FixReading {
		e.logOnCommit(ctx, "workflow: the read of a fix's job changes nothing: the fix no longer reads its job", "issue", fact.Key,
			"generation", fact.Generation, "task", fact.Task, "fix", fact.Fix)
		return intake.Result{}, nil
	}
	fix.JobName, fix.JobURL, fix.ReadFailed = fact.Name, fact.URL, fact.ReadFailed
	if err := e.store.UpdateFix(ctx, tx, *fix); err != nil {
		return intake.Result{}, err
	}
	return intake.Result{}, e.startFix(ctx, tx, *issue, *fix)
}

// startFix starts fix, whose job's read has answered, in one transaction: the implementer's row
// raises an entry at the fix's place with the fix's task (enterAt), whose start writes the start
// of a new run of that step naming the fix's check; the fix runs at that entry, so the issue
// record names it (Issue.RunningFix) and the fences of a start and of a delivery hold its task
// (record.PhaseHolds); and its start is queued for the phase of its step, beside the line of
// starts that wait for a slot, with its working copy made at the fix's head, or at the head of the
// fix's own branch where origin holds it (started, fixBranchOf). The issue stays where it stands,
// and each other step of the task that was handed work in this run is told of the fix
// (tellStepsOfFix).
func (e *Engine) startFix(ctx context.Context, tx pgx.Tx, issue record.Issue, fix record.TaskFix) error {
	w, err := e.walk(ctx, tx, issue)
	if err != nil {
		return err
	}
	if !w.Stored || fix.Place < 1 || fix.Place > len(w.Steps) {
		return fmt.Errorf("start fix %d of task %s of %s: its place %d is no step of the task", fix.Fix, fix.Task, issue.Key, fix.Place)
	}
	at := w.steps()[fix.Place-1]
	role := RoleFor(at)
	task, err := e.fixTask(ctx, tx, issue, w, fix, at)
	if err != nil {
		return err
	}
	entry, sent, err := e.enterAt(ctx, tx, issue, &w, role, at, task, false, false, fix.Check)
	if err != nil {
		return err
	}
	now := e.now()
	fix.State, fix.Entry, fix.StartedAt = record.FixRunning, entry, &now
	if err := e.store.UpdateFix(ctx, tx, fix); err != nil {
		return err
	}
	fixing := issue
	fixing.Phase, fixing.Hold = at, nil
	marks := startMarks{stored: true, fix: fix.Fix, fixFrom: fix.Head, fixBranch: e.fixBranchOf(issue, fix)}
	if err := e.started(ctx, tx, fixing, role, sent, entry, marks); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: a fix starts beside the task's steps", "issue", issue.Key, "task", fix.Task, "fix", fix.Fix,
		"check", fix.Check, "head", fix.Head, "job", fix.JobName, "readFailed", fix.ReadFailed, "phase", issue.Phase)
	return e.tellStepsOfFix(ctx, tx, issue, w, fix)
}

// fixTask is the task of fix, a run of the step that runs as at: the task a start of that step
// is told (task), in that step's phase, its working copy line naming the fix's own working copy
// (fixDirOf), and right after it the sentences of the fix, each built from the start's typed facts
// (startFacts): what it fixes (fixSentence), each other check it holds, the merge forward its
// implementer owes, which the fix makes first (owedMergeSentence), and how it pushes
// (fixPushSentence).
func (e *Engine) fixTask(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, fix record.TaskFix, at phase.Phase) (string, error) {
	fixing := issue
	fixing.Phase, fixing.Hold = at, nil
	rows, err := e.store.Phases(ctx, tx, issue.Key)
	if err != nil {
		return "", err
	}
	facts, err := e.startFacts(ctx, tx, fixing, w)
	if err != nil {
		return "", err
	}
	if !facts.Own || facts.Fix != fix.Fix {
		return "", fmt.Errorf("compose the task of fix %d of task %s of %s: the start at its place stands beside fix %d", fix.Fix, fix.Task, issue.Key, facts.Fix)
	}
	dirOf := func(record.Issue) string { return facts.FixDir }
	standing, err := standingText(fixing, dirOf, w, rows)
	if err != nil {
		return "", err
	}
	pr, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil {
		return "", err
	}
	copyLine := WorkingCopy(fixing, dirOf).lines
	sentences := fixSentence(issue.Key, facts)
	for _, check := range facts.Failed {
		sentences += fmt.Sprintf(" Check %s failed at %s too: CI job of the check ended %s.", check, facts.Head, facts.Cases[check])
	}
	// The reports of the steps that failed to the fix's own step before it started wait on its row
	// (record.TaskFix.Reports, besideFix): this task is the first thing that can name the entry its
	// start raises, so it speaks them, in the words a running fix's message carries.
	sentences += fix.Reports + owedMergeSentence(facts) + fixPushSentence(issue, fix, facts.Branch, facts.FixDir)
	standing.lines = copyLine + sentences + strings.TrimPrefix(standing.lines, copyLine)
	return task(fixing, standing, record.PhaseRow{}, pr, ""), nil
}

// owedMergeSentence is what a fix's task says of the merge forward its implementer owes
// (startFacts' MergeHead and MergeBase): the fix makes it first, in its own working copy, and
// pushes it with its one end push; "" where none is owed.
func owedMergeSentence(facts startFacts) string {
	if facts.MergeHead == "" {
		return ""
	}
	return fmt.Sprintf(" Legion already asked you to merge %s into the task's branch at %s. Make that merge your first act in this fix's copy. "+
		"Include it in your fix's one end push, and push nothing from the task's shared copy.", facts.MergeBase, facts.MergeHead)
}

// fixPushSentence is how fix pushes, which its task says after the failures it holds: from its own
// working copy at dir, its work in progress only to its own branch own (F), never to the task's
// branch; once, when it ends, to the task's branch, then it completes. Before that push it takes the
// commits each step beside it pushed to a branch of its own, and the head of the task's branch
// where it moved (workspace.BesideTakeCommands): a rebase of its own commits onto them, which
// rewrites none of theirs and which a run again leaves as it stands, so the commit under its working
// copy, `@-`, which the push and the completion read, holds each of them once. A commit a step names
// by its id and could not push is copied under the working copy (`--insert-before @`), so it becomes
// `@-`; a copy `--onto @-` would be a sibling of `@`. A fix whose own branch cannot be named, which
// the fix rule refuses to open (refuseFixName), pushes to the task's branch alone.
func fixPushSentence(issue record.Issue, fix record.TaskFix, own, dir string) string {
	if dir == "" {
		dir = "."
	}
	taskBranch := claim.IssueBranch(issue.Key)
	prefix, err := claim.BesideFixPrefix(issue.Key, issue.Generation, fix.Task, fix.Fix)
	if own == "" || err != nil {
		return " Push only from this working copy, to the branch of the task, once, when your fix ends, then complete; before you push, fetch the branch and rebase your commits onto its head where it moved. " +
			"A commit a step beside you names in its report is in this clone by its id: copy it onto your head with jj duplicate <commit> --insert-before @, and never rebase or rewrite it."
	}
	return fmt.Sprintf(" Push only from this working copy. Until your fix ends, push your commits only to your own branch %s, never to the branch of the task: run %s, then %s. "+
		"When your fix ends, push it to the branch of the task once, then complete. "+
		"Before that push, take the commits of the steps beside you and the head of the branch of the task where it moved: %s. "+
		"That puts their commits under yours as they are and rebases only yours, and a run of it again changes nothing; never rebase or rewrite one of their commits, since another working copy may stand on it. "+
		"A commit that a step names by its id and could not push is in this clone by that id: copy it under your working copy once, with jj duplicate <commit> --insert-before @.",
		own, shellprefix.Command([]string{"jj", "-R", dir, "bookmark", "set", own, "-r", "@-"}),
		shellprefix.Command([]string{"jj", "-R", dir, "git", "push", "--remote", "origin", "--bookmark", own}),
		strings.Join(workspace.BesideTakeCommands(dir, prefix, own, taskBranch), ", then "))
}

// startFacts is what a start of a step of issue's task, standing in issue.Phase on w, is told of a
// fix and of the task's CI, as typed values; each sentence such a start's task says of them is
// built from these alone (fixTask, fixLines):
//
//   - the start of a fix (Own), the step at the place of the task's open fix: its number, its
//     check, the head it failed at with its job's case, its job, the job's page or the read's
//     failure, each other check it holds that fails at that head (Failed, with Cases), and the
//     head and base of the merge forward its implementer owes there, which the fix took as its own
//     first work (owedMerge, takeOwedMerge);
//   - a start of another step while a fix reads its job or runs: the fix's number, check and head,
//     and, for each role but the reviewer, which creates no commit, the step's own branch beside
//     the fix (Branch) and the head the daemon holds of a push of it (Pushed, record.TaskFix.Pushed);
//     a start whose step is bound to a branch already keeps that one (record.StepStart.PushBranch),
//     and so does a resume of a step that still runs after its fix ended with its way pending;
//   - a start of an implementation step of a task that takes a fix, with no fix open: the head and
//     each check a CI job runs that fails there (Failed).
//
// The fix's own start holds its own branch F as Branch, and the directory of its own working copy
// beside the task's (FixDir: workspace.FixDir of the directory the runtime gives the issue's working
// copy, Config.WorkspaceDir, "" where the runtime names none), which its task's working copy line
// and its push commands name. Every other start holds none.
type startFacts struct {
	Fix                                        int
	Own                                        bool
	Check, Head, CICase, Job, Page, ReadFailed string
	Branch, Pushed, FixDir                     string
	// Kept is a start of a step that a way 2 runs again after fix Fix ended, whose branch beside
	// that fix (Branch) the daemon holds a push of (Pushed): the way keeps that branch, and the step
	// takes its commits from it.
	Kept                 bool
	MergeHead, MergeBase string
	Failed               []string
	// Cases is the ci_case of each check of Failed that a fix holds.
	Cases map[string]string
}

// startFacts is the typed facts of a start of the step of issue's task standing in issue.Phase on w
// (the type's doc).
func (e *Engine) startFacts(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk) (startFacts, error) {
	place, ok := w.PlaceOf(issue.Phase)
	if !w.Stored || !ok || place.Kind != record.PlaceStep || place.Position > len(w.Steps) {
		return startFacts{}, nil
	}
	open, err := e.store.RunningFix(ctx, tx, issue.Key, issue.Generation, w.Task)
	if err != nil {
		return startFacts{}, err
	}
	if open != nil && open.Place != place.Position {
		return e.besideFacts(ctx, tx, issue, w, place.Position, *open)
	}
	if open == nil {
		if facts, bound, err := e.boundFacts(ctx, tx, issue, w, place.Position); err != nil || bound {
			return facts, err
		}
		if facts, kept, err := e.keptFacts(ctx, tx, issue, w, place.Position); err != nil || kept {
			return facts, err
		}
	}
	pr, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil {
		return startFacts{}, err
	}
	if open != nil {
		return e.fixFacts(ctx, tx, issue, w, *open, pr)
	}
	if w.Steps[place.Position-1].Type != plan.TypeImplementation {
		return startFacts{}, nil
	}
	fixesCI, err := e.store.RunFixesCI(ctx, tx, issue.Key, issue.Generation)
	if err != nil || !fixesCI {
		return startFacts{}, err
	}
	repo, head := taskHead(pr)
	if head == "" {
		return startFacts{}, nil
	}
	held, err := e.store.HeadStates(ctx, tx, issue.Key, repo, head)
	if err != nil {
		return startFacts{}, err
	}
	facts := startFacts{Head: head}
	for _, state := range failedChecks(held) {
		facts.Failed = append(facts.Failed, state.Check)
	}
	return facts, nil
}

// besideFacts is the start facts of a step at place, a step of w's task of issue other than the
// fix's own, while open, a fix of the task, reads its job or runs (startFacts): the fix's number,
// check and head, and, for a role whose step pushes beside a fix (besideBranchOf, which names the
// reviewer none), the step's branch: the one its open start is bound to already
// (record.StepStart.PushBranch), which a later fix's start keeps, else its own branch beside open,
// which its start binds (bindBesideFix); and the head the daemon holds of a push of that branch,
// read from the fix the branch is of. The role is the role of the step's phase, a phase of a step of
// the plan, which admitted never is.
func (e *Engine) besideFacts(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, place int, open record.TaskFix) (startFacts, error) {
	facts := startFacts{Fix: open.Fix, Check: open.Check, Head: open.Head}
	role := RoleFor(w.steps()[place-1])
	bound, pushed, err := e.boundBranch(ctx, tx, issue, w, place)
	if err != nil {
		return startFacts{}, err
	}
	if bound != "" {
		facts.Branch, facts.Pushed = bound, pushed
		return facts, nil
	}
	if branch, ok := e.besideBranchOf(issue, w.Task, open.Fix, role); ok {
		facts.Branch, facts.Pushed = branch, open.Pushed[string(role)]
	}
	return facts, nil
}

// keptFacts is the start facts of a step at place of w's task of issue that a way 2 runs again
// after the task's latest fix ended (startFacts, while no fix of the task is open): where the
// daemon holds a push of the step's own branch beside that fix (record.TaskFix.Pushed), the way kept
// that branch, as every way keeps the branches beside its fix (queueBesideDelete), and the start
// takes its commits from it (Kept). The fix's Way, which afterFix writes before the way's start, is
// the record that the way ran, so no step start's moment is read: no step of the task runs when the
// way is taken (taskStepRuns), so every start and resume of a step after the fix's place from then
// on comes after the fix's end. The way's own start is among them, and its task is composed before
// that start is counted (start, stepStanding), when the step's latest start is still its start from
// before the fix ended. The role is the role of the step's phase, a phase of a step of the plan,
// which admitted never is; the implementer's branch beside the fix is the fix's own, which its end
// push took whole. false for every other start.
func (e *Engine) keptFacts(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, place int) (startFacts, bool, error) {
	fix, err := e.store.LatestFix(ctx, tx, issue.Key, issue.Generation, w.Task)
	if err != nil || fix == nil || fix.Way != wayAfterFix || place <= fix.Place {
		return startFacts{}, false, err
	}
	role := RoleFor(w.steps()[place-1])
	pushed := fix.Pushed[string(role)]
	if role == claim.RoleImplementer || pushed == "" {
		return startFacts{}, false, nil
	}
	branch, ok := e.besideBranchOf(issue, w.Task, fix.Fix, role)
	if !ok {
		return startFacts{}, false, nil
	}
	return startFacts{Fix: fix.Fix, Check: fix.Check, Head: fix.EndHead, Branch: branch, Pushed: pushed, Kept: true}, true, nil
}

// boundFacts is the start facts of a step at place of w's task of issue whose open start is bound
// to a branch beside a fix that no longer reads its job or runs (startFacts, while no fix of the task
// is open): a resume of a step that still runs after its fix ended with its way pending, told of
// that fix and of its branch until its step ends. false where the start is bound to none.
func (e *Engine) boundFacts(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, place int) (startFacts, bool, error) {
	bound, pushed, err := e.boundBranch(ctx, tx, issue, w, place)
	if err != nil || bound == "" {
		return startFacts{}, false, err
	}
	_, _, _, number, _, _ := claim.MatchBesideBranch(bound, []string{w.Task})
	fix, err := e.store.FixAt(ctx, tx, issue.Key, issue.Generation, w.Task, number)
	if err != nil || fix == nil {
		return startFacts{}, false, err
	}
	return startFacts{Fix: fix.Fix, Check: fix.Check, Head: fix.Head, Branch: bound, Pushed: pushed}, true, nil
}

// boundBranch is the branch beside a fix that the open start of the step at place of w's task of
// issue is bound to (record.StepStart.PushBranch), "" for none, and the head the daemon holds of a
// push of it, read from the fix the branch names (record.Store.FixAt, record.TaskFix.Pushed), ""
// for none.
func (e *Engine) boundBranch(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, place int) (branch, pushed string, err error) {
	start, err := e.store.StepStarts(ctx, tx, issue.Key, record.StepPlace{Task: w.Task, Kind: record.PlaceStep, Place: place})
	if err != nil || start.Run != record.RunOpen || start.PushBranch == "" {
		return "", "", err
	}
	_, _, _, number, role, ok := claim.MatchBesideBranch(start.PushBranch, []string{w.Task})
	if !ok {
		return start.PushBranch, "", nil
	}
	fix, err := e.store.FixAt(ctx, tx, issue.Key, issue.Generation, w.Task, number)
	if err != nil || fix == nil {
		return start.PushBranch, "", err
	}
	return start.PushBranch, fix.Pushed[string(role)], nil
}

// fixFacts is the start facts of fix's own start (startFacts), pr being issue's pull request on
// record.
func (e *Engine) fixFacts(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, fix record.TaskFix, pr *record.PullRequest) (startFacts, error) {
	held, err := e.store.HeadStates(ctx, tx, issue.Key, prRepo(issue, pr), fix.Head)
	if err != nil {
		return startFacts{}, err
	}
	facts := startFacts{Fix: fix.Fix, Own: true, Check: fix.Check, Head: fix.Head, CICase: held.States[fix.Check].CICase,
		Job: fix.JobName, Page: fix.JobURL, ReadFailed: fix.ReadFailed}
	facts.Branch, _ = e.besideBranchOf(issue, fix.Task, fix.Fix, claim.RoleImplementer)
	if dir := fixDirOf(e.cfg.WorkspaceDir); dir != nil {
		facts.FixDir = dir(issue)
	}
	for _, state := range failedChecks(held) {
		if state.Check == fix.Check {
			continue
		}
		row, err := e.store.CIFailureAt(ctx, tx, issue.Key, issue.Generation, fix.Task, state.Check, fix.Head)
		if err != nil {
			return startFacts{}, err
		}
		if row != nil && row.Fix == fix.Fix {
			if facts.Cases == nil {
				facts.Cases = map[string]string{}
			}
			facts.Failed, facts.Cases[state.Check] = append(facts.Failed, state.Check), state.CICase
		}
	}
	ours := record.MergeForwardFix{Generation: issue.Generation, Task: w.Task, Number: fix.Fix}
	if owed := owedMerge(pr, fix.Head); owed != nil && (owed.Fix == nil || *owed.Fix == ours) {
		facts.MergeHead, facts.MergeBase = owed.Head, owed.Base
	}
	return facts, nil
}

// fixLines is what the task of a start of a step of issue's task, standing in issue.Phase on w,
// says of a fix and of the task's CI (startFacts): beside a fix that reads its job or runs, that
// the failure is the fix's and where the step pushes until it ends (besideStepSentence); and, for
// an implementation step of a task that takes a fix, each check that a CI job runs and that fails
// at the task's head, each of which goes to that step (record.CIFailureToImplementer), so the fix
// rule tells its implementer of it no second time. Every other start says nothing more, the fix's
// own among them, whose task is the fix's (fixTask).
func (e *Engine) fixLines(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk) (string, error) {
	facts, err := e.startFacts(ctx, tx, issue, w)
	switch {
	case err != nil || facts.Own:
		return "", err
	case facts.Kept:
		return keptSentence(facts, claim.IssueBranch(issue.Key)), nil
	case facts.Fix > 0:
		return besideStepSentence(facts), nil
	case len(facts.Failed) == 0:
		return "", nil
	}
	for _, check := range facts.Failed {
		failure := record.CIFailure{Issue: issue.Key, Generation: issue.Generation, Task: w.Task, Check: check, Head: facts.Head,
			WentTo: record.CIFailureToImplementer, At: e.now()}
		if _, err := e.store.PutCIFailure(ctx, tx, failure); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf(" Failed CI checks at %s: %s.", facts.Head, strings.Join(facts.Failed, ", ")), nil
}

// prRepo is the repository of issue's pull request on record, or the issue's own where it has none.
func prRepo(issue record.Issue, pr *record.PullRequest) string {
	if pr != nil {
		return pr.Repo
	}
	return issue.Repo
}

// fixDirOf is dir, the directory the runtime gives an issue's working copy, as the directory of a
// fix's own working copy beside it (workspace.FixDir, which owns the rule): nil stays none.
func fixDirOf(dir func(record.Issue) string) func(record.Issue) string {
	if dir == nil {
		return nil
	}
	return func(issue record.Issue) string { return workspace.FixDir(dir(issue)) }
}

// fixSentence is the sentence of a fix's task that names what it fixes, from its start facts: its
// check, the head the check failed at, and its CI job as the read found it: its name, how it ended
// (CICase) and its page; its name and how to find its run where Dispatch holds no page; and the
// read's error and how to find the run where the read found no job. The job's name and page are a
// CI system's, and the read's error can carry Dispatch's own answer, so each is quoted, as other
// writers' text inside a sentence of the daemon's is (quotedList): it stays one value on one line,
// and nothing in it reads as the daemon's own words.
func fixSentence(key string, facts startFacts) string {
	find := fmt.Sprintf("find its run from %s with dispatch results --issue %s", facts.Head, key)
	switch {
	case facts.Job == "" && facts.ReadFailed != "":
		return fmt.Sprintf(" This is a fix: check %s failed at %s (Dispatch could not be read for its job: %q; %s).", facts.Check, facts.Head, facts.ReadFailed, find)
	case facts.Page == "":
		return fmt.Sprintf(" This is a fix: check %s failed at %s (CI job %s; Dispatch holds no address of its page, so %s).", facts.Check, facts.Head,
			jobOf(record.TaskFix{JobName: facts.Job}), find)
	}
	return fmt.Sprintf(" This is a fix: check %s failed at %s (CI job %s ended %s, page %q).", facts.Check, facts.Head,
		jobOf(record.TaskFix{JobName: facts.Job}), facts.CICase, facts.Page)
}

// jobOf is the name of fix's CI job, quoted, since a CI system names it (fixSentence), or "of the
// check" where the read found none.
func jobOf(fix record.TaskFix) string {
	if fix.JobName == "" {
		return "of the check"
	}
	return strconv.Quote(fix.JobName)
}

// besideStepSentence is what the task of a step that starts beside a fix says of it (fixLines),
// from the start's facts (startFacts): the fix runs, its failure is the fix's, and, where the step
// has a branch of its own (facts.Branch, "" for the reviewer, which creates no commit), that it
// pushes only there, names that branch and its commits in its report, records each result at the
// pull request's head, and what it does where GitHub refuses the push; and, where the daemon holds a
// push of that branch (facts.Pushed), where those commits stand for a new pod. No text names a ref
// the daemon holds no push of.
func besideStepSentence(facts startFacts) string {
	sentence := fmt.Sprintf(" A fix of this task runs: check %s failed at %s. Leave that failure to the fix.", facts.Check, facts.Head)
	if facts.Branch == "" {
		return sentence
	}
	sentence += pushBesideSentence(facts.Branch) + recordAtHead(facts.Head)
	if facts.Pushed != "" {
		sentence += fmt.Sprintf(" Your pushed commits are on %s at %s: on a new pod, run jj -R \"$LEGION_WORKSPACE\" git fetch --remote origin --branch %s first and go on at %s@origin.",
			facts.Branch, facts.Pushed, facts.Branch, facts.Branch)
	}
	return sentence
}

// pushBesideSentence is the sentence that tells a step beside a fix where it pushes until its step
// ends: to branch, its own branch beside the fix, with the command that pushes there
// (workspace.BesidePushCommand), never to the task's branch, naming that branch and each commit in
// its report.
func pushBesideSentence(branch string) string {
	return fmt.Sprintf(" Until your step ends, push nothing to the branch of the task: push your commits to your own branch %s with %s, and name that branch and each commit in your report.",
		branch, workspace.BesidePushCommand(branch))
}

// recordAtHead is the two sentences a step beside a fix that pushes is told of where it records
// and what it does where GitHub refuses its push: each result, and its head, at head, the pull
// request's, never at a commit of its own branch.
func recordAtHead(head string) string {
	return fmt.Sprintf(" Record each result, and name your head, at %s, the head of the pull request, never at a commit of your own branch, where a result counts for nothing.", head) +
		refusedPushSentence
}

// refusedPushSentence is what a step beside a fix does where GitHub refuses the push to its own
// branch: it pushes nothing else, and names each commit by its id, with GitHub's answer.
const refusedPushSentence = " If GitHub refuses that push, push nothing else, and name each commit by its id, with GitHub's answer, in your report."

// keptSentence is what the task of a step that a way 2 runs again says of its kept branch beside
// the fix (fixLines), from the start's facts (startFacts, Kept): the fix ended, and the commits the
// step pushed to its own branch while the fix ran stay there, at the pushed head, kept by the way.
// The step takes first, by ancestry, those of them that the task's branch taskBranch does not hold
// (workspace.KeptTakeCommands), since the fix's end push took some as they are, and a take run
// again on a resumed turn takes nothing more; then it pushes to the task's branch as its task says.
func keptSentence(facts startFacts, taskBranch string) string {
	return fmt.Sprintf(" Fix %d of this task ended at %s, and the steps after it run again. Your own branch %s keeps the commits you pushed there beside the fix, at %s. "+
		"Take first those of them that the branch of the task does not hold: run %s. "+
		"That puts them under your working copy as they are, and a run of it again changes nothing; never rebase or rewrite one. Then push to the branch of the task as this task says.",
		facts.Fix, facts.Head, facts.Branch, facts.Pushed, strings.Join(workspace.KeptTakeCommands(facts.Branch, taskBranch), ", then "))
}
