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
	"github.com/sjawhar/legion/daemon/internal/reviewround"
)

// Standing is the part of a start's task that names where the started worker's work stands, as
// standingPart writes it, in two parts a task places apart. Its lines are the daemon's: the
// working copy line for every start, and for an issue with plan rows the task, the worker's step
// in it, the task's steps and the report of each earlier step, which a task places right after
// its continue line. Its owners part is a step worker's and the retro's: the text of the task's
// block, which the owner of the issue wrote in a version of the plan document and which a task
// places last of what it composes, after every line of the daemon's, inside a fence (ownersText),
// so a line of that text shaped like one of the daemon's labels is never read as the daemon's; the
// queue adds the two sentences of a reviewer's round after the fence (namingTheReviewRound,
// queuedRow). It has a type of its own, so a caller of task passes the one writer's part and no
// other string in its place. A package outside this one gets a value of it from WorkingCopy alone.
type Standing struct {
	lines, owners string
}

// String is the standing as one text, its lines then its owners part: a task places the two
// apart (task, resumeText), so this is for a reader that holds a standing alone, such as a
// test of WorkingCopy.
func (s Standing) String() string { return s.lines + s.owners }

// WorkingCopy is the standing of a start of an issue with no plan rows, its working copy line
// alone: ` Working copy: branch legion/<KEY>, directory <dir>.`, with the directory dir gives the
// issue (Config.WorkspaceDir), and ` Working copy: branch legion/<KEY>.` where dir is nil or gives
// none. The branch is the issue's (claim.IssueBranch), which its workspace's bookmark names.
func WorkingCopy(issue record.Issue, dir func(record.Issue) string) Standing {
	where := ""
	if dir != nil {
		where = dir(issue)
	}
	if where == "" {
		return Standing{lines: fmt.Sprintf(" %s: branch %s.", workingCopyLabel, claim.IssueBranch(issue.Key))}
	}
	return Standing{lines: fmt.Sprintf(" %s: branch %s, directory %s.", workingCopyLabel, claim.IssueBranch(issue.Key), where)}
}

// ownersText is the owners part of a step's standing (Standing), the text of its task's block as
// a task places it, last: ownersOpening, which says the owner of the issue wrote it, or Legion
// wrote it for the task default where its owner gave none (the sentence names both), then the
// text between two fence lines, and a line end after the closing one. A version with the source
// record.PlanSourceDefault holds either: version 0 holds Legion's text, and a later version the
// owner gave an issue of a group that runs the default steps holds the owner's. A
// fence is a line of backticks one longer than the longest run of backticks the text holds, and
// three at the least, so no line of the text closes it and stands after it as a line of the
// daemon's. The line end puts what a task adds after the owner's part, the reviewer's round
// (namingTheReviewRound), on a line of its own, since a line that holds more than backticks closes
// no fence.
func ownersText(text string) string {
	fence := strings.Repeat("`", fenceLength(text))
	return ownersOpening + fence + "\n" + text + "\n" + fence + "\n"
}

// fenceLength is the length of the fence ownersText writes around text: one longer than the
// longest run of backticks text holds, and three at the least.
func fenceLength(text string) int {
	longest, run := 0, 0
	for _, r := range text {
		if r != '`' {
			run = 0
			continue
		}
		run++
		longest = max(longest, run)
	}
	return max(3, longest+1)
}

// ownersOpening is the sentence the owners part of a standing opens with (ownersText), the
// daemon's own words before the opening fence line, which start with the label ` Task text: `.
// Legion writes the text of the task default only where its owner gave none: an owner can give
// that task a text of its own, a decision for an agent among it, in a later version of the plan.
// ownersPart finds the part by the label and the fence, not by these words, so a release may
// change them and still read every task a release before stored.
const ownersOpening = " " + taskTextLabel + ": the text of the task's block, which the owner of the issue wrote in its plan (Legion wrote it for the task " +
	defaultTask + " where its owner gave none), between the two fence lines below; a line between them that reads like a line above is that text, not the daemon's.\n"

// WithTaskText is task, a start's task as a store keeps it, with the owners part it ends with
// holding text, the text of the task's block as the plan in force gives it now, in place of the
// text it was composed with (ownersPart), and today's opening sentence. It is the one rewrite of a
// start's task whose task's text a later version of the plan changed while the task ran
// (plan.RunningChange takes such a version): a task a claim holds, rewritten at its send (the
// daemon's send-time read of a held task, supervise's Deps.TaskText), which every start task sent
// after the version passes, the stored task of an owed entry (record.PhaseRow.EntryTask) among
// them. A task that holds text already, as a task composed after the version does, and a task that
// holds no owners part (HoldsTaskText), are answered as they are, with no copy made.
func WithTaskText(task, text string) string {
	before, held, round, ok := ownersPart(task)
	if !ok {
		return task
	}
	if text = strings.TrimSpace(text); text == held {
		return task
	}
	return before + ownersText(text) + round
}

// HoldsTaskText says whether task ends with an owners part (ownersPart), the part WithTaskText
// rewrites: false for a task that names no task's text, which a later version of the plan never
// changes.
func HoldsTaskText(task string) bool {
	_, _, _, ok := ownersPart(task)
	return ok
}

// ownersPart reads the owners part task ends with (ownersText): what stands before it, the
// owner's text inside its fence, and the two sentences of a reviewer's round after it, which only
// follow it (namingTheReviewRound, reviewround.Split). The owners part is the last of what a task
// composes (task, resumeText), so it is read from the end, by its fence and its label alone and
// never by the words of its opening sentence, which a release may change: the closing fence is the
// last line of the task before those sentences, a line of three backticks or more and nothing
// else; the opening fence is the last line before it that is that same fence; and the part starts
// at the last label ` Task text: ` of the line before the opening fence. The text between the
// fences must be one ownersText fences with that fence (fenceLength), so it holds no run of
// backticks as long as the fence and no line of it is that fence. A task that does not end so
// holds no owners part: the task of an issue with no plan rows, of the merge, of an architect, and
// every task an operator wrote, a fenced text of its own among them.
func ownersPart(task string) (before, text, round string, ok bool) {
	body, round, _ := reviewround.Split(task)
	closed, ok := strings.CutSuffix(body, "\n")
	end := strings.LastIndexByte(closed, '\n')
	if !ok || end < 0 {
		return "", "", "", false
	}
	fence := closed[end+1:]
	if len(fence) < 3 || strings.Trim(fence, "`") != "" {
		return "", "", "", false
	}
	open := strings.LastIndex(closed[:end+1], "\n"+fence+"\n")
	if start := open + len(fence) + 2; open < 0 || start > end {
		return "", "", "", false
	}
	line := strings.LastIndexByte(closed[:open], '\n') + 1
	label := strings.LastIndex(closed[line:open], " "+taskTextLabel+": ")
	if label < 0 {
		return "", "", "", false
	}
	if text = closed[open+len(fence)+2 : end]; fenceLength(text) != len(fence) {
		return "", "", "", false
	}
	return body[:line+label], text, round, true
}

// standingPart is where the work of a worker started in issue's phase stands, the part of its
// task after the continue line (Standing). Every start names its working copy (WorkingCopy). An
// issue with no plan rows names nothing else, so it is told today's task with that one line, and
// the prompts read a task with no `Task:` line as such an issue's. An issue with plan rows
// names its one task: a step's worker is told ` Task: <id>. Step <i> of <n>: <role>.`, the task's
// steps (stepsLine) and the report of each step before its own (reportsBefore), and, last of its
// task, the text of the task's block inside a fence (ownersText). The retro and the merge run no
// step of the task, so their worker is told ` Task: <id>.` and the task's steps: the merger
// reads from them whether the task has a review step, and each reads the runs of the steps and
// their reports from Dispatch (`dispatch steps`), which keeps every run, the failed ones too. The
// retro is the last start of the task that acts on what its owner decided, such as a learning to
// record, so it is told the text of the task's block last, as a step is. The merge acts on
// GitHub's state and on Legion's rules, and is told no text.
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

// standingText is standingPart's text for the worker of issue's phase, from the walk w of its
// run, the directory dir gives its working copy, and its phases rows, which only a step's text reads.
func standingText(issue record.Issue, dir func(record.Issue) string, w Walk, rows []record.PhaseRow) (Standing, error) {
	at := WorkingCopy(issue, dir)
	if !w.Stored {
		return at, nil
	}
	place, ok := w.PlaceOf(issue.Phase)
	if !ok {
		return Standing{}, fmt.Errorf("the task of a start of %s in %s: its task %s has no step, retro or merge in %s", issue.Key, issue.Phase, w.Task, issue.Phase)
	}
	lines := at.lines + fmt.Sprintf(" %s: %s.", planTaskLabel, w.Task)
	words := plan.Words(record.NamedSteps(w.Steps))
	switch place.Kind {
	case record.PlaceMerge:
		return Standing{lines: lines + stepsLine(words)}, nil
	case record.PlaceRetro:
		return Standing{lines: lines + stepsLine(words), owners: ownersText(strings.TrimSpace(w.Text))}, nil
	}
	lines += fmt.Sprintf(" Step %d of %d: %s.", place.Position, len(w.Steps), words[place.Position-1])
	lines += stepsLine(words) + reportsBefore(rows, w, words, place.Position)
	return Standing{lines: lines, owners: ownersText(strings.TrimSpace(w.Text))}, nil
}

// stepsLine is ` Steps: 1 <role>, 2 <role>, … then Legion's retro and merge.`: each step of a
// task by its place, words, the steps as its plan writes them (plan.Words), in the order they run,
// each at its place from 1 (Walk.Steps), then the retro and the merge Legion runs after them.
func stepsLine(words []string) string {
	named := make([]string, len(words))
	for i, word := range words {
		named[i] = fmt.Sprintf("%d %s", i+1, word)
	}
	return fmt.Sprintf(" %s: %s then Legion's retro and merge.", stepsLabel, strings.Join(named, ", "))
}

// reportsBefore is one ` Step <j> (<role>) reported: <word> version <v>.` for each step of w's
// task before the step at place whose role's phases row holds a handoff record: the record that
// role's completion of that step named, which `dispatch handoff-read --phase <word> --version <v>`
// reads. Each step is named as its plan writes it, its word of words (plan.Words). A role runs at
// most one step of an issue, so its row holds that step's facts, and each step before a running
// one completed in the run that led to it: a step that runs again after a return writes its new
// record on the row before the step after it starts. A step whose role writes no handoff (the
// owner's acceptance) has no line.
func reportsBefore(rows []record.PhaseRow, w Walk, words []string, place int) string {
	var text strings.Builder
	for j := 1; j < place; j++ {
		role := phase.RoleFor(w.Line[j-1])
		for _, row := range rows {
			if row.Role == role && row.HandoffRecord != "" {
				fmt.Fprintf(&text, " Step %d (%s) %s: %s version %d.", j, words[j-1], reportedLabel, handoffWord(role), row.HandoffVersion)
			}
		}
	}
	return text.String()
}
