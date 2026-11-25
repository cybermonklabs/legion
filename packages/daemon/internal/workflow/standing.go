package workflow

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// Standing is where a started worker's work stands, as standingPart reads it, in the two parts a
// task places apart. Its facts are the daemon's (texts.Standing): the working copy for every
// start, and for an issue with plan rows the task, the worker's step in it, the task's steps and
// the report of each earlier step, which a task states as its fields. Its text is a step worker's
// alone: the text of the task's block, which the owner of the issue wrote in a version of the plan
// document, and which a task carries last, in a block of its own after its fields, by the architect
// claim that gave that version (texts.PhaseAssignmentRecord.TaskText), so no line of that text is
// read as the daemon's. It has a type of its own, so a caller of task passes the one reader's part
// and no other value in its place. A package outside this one gets a value of it from WorkingCopy
// or StandingOf alone.
type Standing struct {
	facts texts.Standing
	text  *texts.Words
}

// Facts is the daemon's part of the standing, the fields a task states: for a reader that holds a
// standing alone, such as a test of WorkingCopy.
func (s Standing) Facts() texts.Standing { return s.facts }

// WorkingCopy is the standing of a start of an issue with no plan rows, its working copy alone:
// the issue's branch (claim.IssueBranch), which its workspace's bookmark names, and the directory
// dir gives the issue (Config.WorkspaceDir), none where dir is nil or gives none.
func WorkingCopy(issue record.Issue, dir func(record.Issue) string) Standing {
	at := texts.Standing{Branch: claim.IssueBranch(issue.Key)}
	if dir != nil {
		at.Directory = dir(issue)
	}
	return Standing{facts: at}
}

// ownersText is the text part of a step's standing (Standing), the text of its task's block as the
// owner of the issue wrote it in a version of its plan, carried whole in a block of its own by the
// architect claim that gave that version, at the time it gave it (w.Version). Legion writes the
// text of the task default itself (record.PlanSourceDefault), the same words for every such task,
// which names its task (defaultTask) and its steps in its fields, so a run on the default steps
// carries no text.
func ownersText(w Walk) *texts.Words {
	text := strings.TrimSpace(w.Text)
	if w.Version.Source != record.PlanSourceOwner || text == "" {
		return nil
	}
	words := texts.Words{Text: text, By: string(w.Version.GivenBy)}
	if !w.Version.GivenAt.IsZero() {
		words.At = w.Version.GivenAt.UTC()
	}
	return &words
}

// standingPart is where the work of a worker started in issue's phase stands, the part of its
// task after the phase (Standing). Every start names its working copy (WorkingCopy). An issue
// with no plan rows names nothing else, so it is told today's task with that one field, and the
// prompts read a task with no task field as such an issue's. An issue with plan rows names its one
// task: a step's worker is told the task, its step's place in it, the task's steps (stepsOf) and
// the report of each step before its own (reportsBefore), and, last of its task, the text of the
// task's block (ownersText). The retro and the merge run no step of the task, so their worker is
// told the task and its steps alone: the merger reads from them whether the task has a review
// step, and each reads the runs of the steps and their reports from Dispatch (`dispatch steps`),
// which keeps every run, the failed ones too.
//
// The phase the worker starts in is issue.Phase, the phase each caller has put the issue in
// before it composes the task, and w is the walk of its run as the caller read it, after any move
// of its task (moveTask). A run with plan rows whose line holds no place in that phase is
// refused: no start of such a run names one, and a task that named none would read as an issue
// with no plan rows.
func (e *Engine) standingPart(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk) (Standing, error) {
	var rows []record.PhaseRow
	var err error
	if place, _ := w.PlaceOf(issue.Phase); w.Stored && place.Kind == record.PlaceStep {
		if rows, err = e.store.Phases(ctx, tx, issue.Key); err != nil {
			return Standing{}, err
		}
	}
	return standingText(issue, e.cfg.WorkspaceDir, w, rows)
}

// standingText is standingPart's standing for the worker of issue's phase, from the walk w of its
// run, the directory dir gives its working copy, and its phases rows, which only a step's standing
// reads.
func standingText(issue record.Issue, dir func(record.Issue) string, w Walk, rows []record.PhaseRow) (Standing, error) {
	at := WorkingCopy(issue, dir)
	if !w.Stored {
		return at, nil
	}
	place, ok := w.PlaceOf(issue.Phase)
	if !ok {
		return Standing{}, fmt.Errorf("the task of a start of %s in %s: its task %s has no step, retro or merge in %s", issue.Key, issue.Phase, w.Task, issue.Phase)
	}
	words := plan.Words(namedSteps(w.Steps))
	at.facts.Task, at.facts.Steps = w.Task, words
	if place.Kind != record.PlaceStep {
		return at, nil
	}
	at.facts.Step = place.Position
	at.facts.Reported = reportsBefore(rows, w, words, place.Position)
	at.text = ownersText(w)
	return at, nil
}

// StandingOf is standingText for a reader outside this package that holds a walk and its phases
// rows itself: internal/daemon's golden samples of a step's, a retro's and a merge's task, which
// compose the standing the way standingPart does without a store.
func StandingOf(issue record.Issue, dir func(record.Issue) string, w Walk, rows []record.PhaseRow) (Standing, error) {
	return standingText(issue, dir, w, rows)
}

// reportsBefore is one report for each step of w's task before the step at place whose role's
// phases row holds a handoff record: the step's place, its word of words (plan.Words), as its plan
// writes it, and the record that role's completion of that step named, by its phase word and its
// version, which `dispatch handoff-read --phase <word> --version <v>` reads. A role runs at most
// one step of an issue, so its row holds that step's facts, and each step before a running one
// completed in the run that led to it: a step that runs again after a return writes its new record
// on the row before the step after it starts. A step whose role writes no handoff (the owner's
// acceptance) has no report.
func reportsBefore(rows []record.PhaseRow, w Walk, words []string, place int) []texts.StepReport {
	var reports []texts.StepReport
	for j := 1; j < place; j++ {
		role := phase.RoleFor(w.Line[j-1])
		for _, row := range rows {
			if row.Role == role && row.HandoffRecord != "" {
				reports = append(reports, texts.StepReport{Step: j, Role: words[j-1], HandoffPhase: handoffWord(role), HandoffVersion: row.HandoffVersion})
			}
		}
	}
	return reports
}
