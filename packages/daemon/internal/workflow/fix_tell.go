package workflow

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// When a fix starts, each other step of its task that runs or was handed work in the run, and the
// owner in its acceptance, is told of the fix once, by one fix-started row
// (record.CINewsFixStarted): the fix runs, the failure is the fix's, and, for each role but the
// reviewer, where the step pushes until it ends (tellStepsOfFix, tellOwnerOfFix, tellFixStarted).

// tellStepsOfFix tells each other step of w's task that runs or was handed work in this run (its
// phases row holds the task of an entry), and the owner where the issue stands in the owner's
// acceptance, of fix, by one fix-started row each (record.CINewsFixStarted, tellFixStarted): the fix
// runs, the failure is the fix's, and, for each role but the reviewer, which creates no commit, that
// until its step ends it pushes only to its own branch beside the fix, which the row names and the
// step's open start is bound to here (toldBranch). Each row is for the step's current work, its
// entry where its step runs, or the owner's acceptance, so the outbox wakes a released reader and
// finishes a row whose work is no longer current undelivered.
//
// A step whose current start named this fix (toldByStart: it started while the fix read its job)
// was told by its own start's task, and the owner by the row of its move into accepting
// (tellOwnerOfFix), so each is told once. A step that has not started is told by its start (task).
// The tester's testing step still judges each check a CI job runs, this one among them, so its
// report can fail for it, and a step that fails beside a fix moves nothing until the way after the
// fix (besideFix); no other step's report fails for it.
//
// Each other step's open start is marked as one the fix runs beside (record.StepStart.BesideFix),
// whether or not its step is told: what ReviewBesideFix reads of a review step that started before
// the fix, never the clock. toldByStart judges the start as it was read before the mark, as its own
// start left it.
func (e *Engine) tellStepsOfFix(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, fix record.TaskFix) error {
	news, err := e.fixStartedOf(ctx, tx, issue, w, fix)
	if err != nil {
		return err
	}
	// Who reads the task's CI news, and for which work, is ciReaders' alone: a step told here is
	// told for its current work where it is a current reader, and for none otherwise.
	readers, err := e.ciReaders(ctx, tx, issue, w)
	if err != nil {
		return err
	}
	for i, at := range w.steps() {
		role := RoleFor(at)
		if i+1 == fix.Place || role == "" {
			continue
		}
		place := record.StepPlace{Task: w.Task, Kind: record.PlaceStep, Place: i + 1}
		start, err := e.store.StepStarts(ctx, tx, issue.Key, place)
		if err != nil {
			return err
		}
		if start.Run == record.RunOpen && start.BesideFix != fix.Fix {
			if err := e.store.MarkStepBesideFix(ctx, tx, issue.Key, place, start.Starts, fix.Fix); err != nil {
				return err
			}
		}
		to := roleRecipient{issue: issue.Key, role: role, row: issue.Key}
		if role == claim.RoleArchitect {
			// The owner's acceptance is no worker's step: the owner is told while the issue
			// stands in it, the acceptance being its work.
			if record.StandsAt(issue) != at || start.Run != record.RunOpen {
				continue
			}
			if to.issue, err = e.owningArchitect(ctx, tx, issue); err != nil {
				return err
			}
		} else {
			row, err := e.phaseRow(ctx, tx, issue.Key, role)
			if err != nil {
				return err
			}
			if row.Entry == 0 || row.EntryTask == "" {
				continue
			}
		}
		reader := stepReaderOf(readers, to)
		if e.toldByStart(start, fix) {
			continue
		}
		branch, err := e.toldBranch(ctx, tx, issue, place, start, fix, role)
		if err != nil {
			return err
		}
		if err := e.tellFixStarted(ctx, tx, issue, w, news, to, reader, branch); err != nil {
			return err
		}
	}
	return nil
}

// tellOwnerOfFix tells the owner, whose acceptance step starts at step while a fix of the task
// reads its job or runs (startNamedStep), of that fix by the fix-started row tellStepsOfFix gives
// each step beside it: the acceptance has no task of its own to name the fix, so the row is its one
// notice of it, naming the owner's branch beside the fix, which the start bound (bindBesideFix). Its
// acceptance is not held by the fix's check. A fix that opens after the acceptance started tells
// the owner at its own start instead.
func (e *Engine) tellOwnerOfFix(ctx context.Context, tx pgx.Tx, issue record.Issue, step standing) error {
	if step.place.Kind != record.PlaceStep {
		return nil
	}
	fix, err := e.store.RunningFix(ctx, tx, issue.Key, issue.Generation, step.place.Task)
	if err != nil || fix == nil || fix.Place == step.place.Place {
		return err
	}
	w, err := e.walk(ctx, tx, issue)
	if err != nil {
		return err
	}
	news, err := e.fixStartedOf(ctx, tx, issue, w, *fix)
	if err != nil {
		return err
	}
	start, err := e.store.StepStarts(ctx, tx, issue.Key, step.place)
	if err != nil {
		return err
	}
	owner, err := e.owningArchitect(ctx, tx, issue)
	if err != nil {
		return err
	}
	return e.tellFixStarted(ctx, tx, issue, w, news, roleRecipient{issue: owner, role: claim.RoleArchitect, row: issue.Key},
		&record.CIReader{Phase: step.at}, start.PushBranch)
}

// fixStarted is what every fix-started row of fix carries beside its receiver: the repository and
// the number of the task's pull request, how the fix's check's CI job ended at the fix's head, and
// the word of the step the fix runs again in the task's plan.
type fixStarted struct {
	fix    record.TaskFix
	repo   string
	number int
	ciCase string
	step   string
}

// fixStartedOf is the fixStarted of fix, a fix of w's task of issue.
func (e *Engine) fixStartedOf(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, fix record.TaskFix) (fixStarted, error) {
	pr, err := e.store.PullRequest(ctx, tx, issue.Key)
	if err != nil {
		return fixStarted{}, err
	}
	news := fixStarted{fix: fix, repo: prRepo(issue, pr)}
	if pr != nil {
		news.number = pr.Number
	}
	held, err := e.store.HeadStates(ctx, tx, issue.Key, news.repo, fix.Head)
	if err != nil {
		return fixStarted{}, err
	}
	news.ciCase = held.States[fix.Check].CICase
	if words := plan.Words(namedSteps(w.Steps)); fix.Place >= 1 && fix.Place <= len(words) {
		news.step = words[fix.Place-1]
	}
	return news, nil
}

// tellFixStarted writes the fix-started row of news to to, a step beside the fix or the owner, for
// reader, its current work, nil where its step does not run, with branch, the branch beside the fix
// it pushes to, "" for the reviewer, which creates no commit. The row names the fix's check, head,
// job's case and number (Assigned) and that branch, which a test reads; its body
// (fixStartedBody) is built from the same values.
func (e *Engine) tellFixStarted(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk, news fixStarted, to roleRecipient,
	reader *record.CIReader, branch string) error {
	addressed, err := e.addressRoles(issue.Project, []roleRecipient{to}, nil)
	if err != nil {
		return err
	}
	fix := news.fix
	for _, a := range addressed {
		if err := e.enqueue(ctx, tx, a.issue, record.CINews{Purpose: record.CINewsFixStarted, Role: string(a.token), Generation: issue.Generation,
			Task: w.Task, Repo: news.repo, PullRequest: news.number, Head: fix.Head, Reader: reader, Branch: branch,
			Assigned: &record.CIAssigned{Check: fix.Check, Head: fix.Head, CICase: news.ciCase, Fix: fix.Fix},
			Body:     fixStartedBody(issue.Key, news, to.role, branch)}); err != nil {
			return err
		}
	}
	return nil
}

// fixStartedBody is the body of the fix-started row to role, a step beside the fix of news, or the
// owner for the architect, branch being its branch beside the fix ("" for the reviewer): the fix,
// its check, head and job, and the step it runs again in its own working copy; that the failure is
// the fix's, so the step's report does not fail for it (the testing step leaves each check a CI job
// runs to its task's end); and how it pushes until its step ends, where it records each result and
// what it does where GitHub refuses its push. The owner is told what it accepts on instead of where
// it records.
func fixStartedBody(key string, news fixStarted, role claim.Role, branch string) string {
	fix := news.fix
	body := fmt.Sprintf("A fix of task %s of %s runs: check %s failed at %s (CI job %s). It is step %d (%s) again, in a working copy of its own.",
		fix.Task, key, fix.Check, fix.Head, jobOf(fix), fix.Place, news.step)
	body += " Leave that failure to the fix: your report does not fail for it, and you run its job no more."
	if branch == "" {
		return body + " Until your step ends, push nothing to the branch of the task."
	}
	body += pushBesideSentence(branch)
	if role == claim.RoleArchitect {
		return body + fmt.Sprintf(" Accept only where each check but %s reads yes at the head of the pull request. %s is the fix's, and it does not hold your acceptance. "+
			"A result at a commit of your own branch counts for nothing.", fix.Check, fix.Check) + refusedPushSentence
	}
	return body + recordAtHead(fix.Head)
}
