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
)

// Standing is the part of a start's task that names where the started worker's work stands, as
// standingPart writes it, in two parts a task places apart. Its lines are the daemon's: the
// working copy line for every start, and for an issue with plan rows the task, the worker's step
// in it, the task's steps and the report of each earlier step, which a task places right after
// its continue line. Its owners part is a step worker's alone: the text of the task's block, which
// the owner of the issue wrote in a version of the plan document and which a task places last of
// what it composes, after every line of the daemon's, inside a fence (ownersText), so a line of
// that text shaped like one of the daemon's labels is never read as the daemon's; the queue adds
// the two sentences of a reviewer's round after the fence (namingTheReviewRound, queuedRow). It has
// a type of its own, so a caller of task passes the one writer's part and no other string in its
// place. A package outside this one gets a value of it from WorkingCopy alone.
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
// a task places it, last: ` Task text: ` and one sentence that says the owner of the issue wrote
// it (Legion's own text for the task default, record.PlanSourceDefault, which the sentence names
// too), then the text between two fence lines, and a line end after the closing one. A fence is a
// line of backticks one longer than the longest run of backticks the text holds, and three at the
// least, so no line of the text closes it and stands after it as a line of the daemon's. The line
// end puts what a task adds after the owner's part, the reviewer's round (namingTheReviewRound), on
// a line of its own, since a line that holds more than backticks closes no fence.
func ownersText(text string) string {
	longest, run := 0, 0
	for _, r := range text {
		if r != '`' {
			run = 0
			continue
		}
		run++
		longest = max(longest, run)
	}
	fence := strings.Repeat("`", max(3, longest+1))
	return " " + taskTextLabel + ": the text of the task's block, which the owner of the issue wrote in its plan (Legion wrote it for the task " +
		defaultTask + "), between the two fence lines below; a line between them that reads like a line above is that text, not the daemon's.\n" +
		fence + "\n" + text + "\n" + fence + "\n"
}

// standingPart is where the work of a worker started in issue's phase stands, the part of its
// task after the continue line (Standing). Every start names its working copy (WorkingCopy). An
// issue with no plan rows names nothing else, so it is told today's task with that one line, and
// the prompts read a task with no `Task:` line as such an issue's. An issue with plan rows
// names its one task: a step's worker is told ` Task: <id>. Step <i> of <n>: <step>.`, the task's
// steps (stepsLine) and the report of each step before its own (reportsBefore), and, last of its
// task, the text of the task's block inside a fence (ownersText). The retro and the merge run no
// step of the task, so their worker is told ` Task: <id>.` and the task's steps alone: the merger
// reads from them whether the task has a review step, and each reads the runs of the steps and
// their reports from Dispatch (`dispatch steps`), which keeps every run, the failed ones too.
//
// The phase the worker starts in is issue.Phase, the phase each caller has put the issue in
// before it composes the task, and w is the walk of its run as the caller read it, after any move
// of its task (moveTask). A run with plan rows whose line holds no place in that phase is
// refused: no start of such a run names one, and a task that named none would read as an issue
// with no plan rows.
func (e *Engine) standingPart(ctx context.Context, tx pgx.Tx, issue record.Issue, w Walk) (Standing, error) {
	var rows []record.PhaseRow
	var err error
	if place, _ := w.placeIn(issue.Phase); w.Stored && place.Kind == record.PlaceStep {
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
	place, ok := w.placeIn(issue.Phase)
	if !ok {
		return Standing{}, fmt.Errorf("the task of a start of %s in %s: its task %s has no step, retro or merge in %s", issue.Key, issue.Phase, w.Task, issue.Phase)
	}
	lines := at.lines + fmt.Sprintf(" %s: %s.", planTaskLabel, w.Task)
	words := plan.Words(namedSteps(w.Steps))
	if place.Kind != record.PlaceStep {
		return Standing{lines: lines + stepsLine(words)}, nil
	}
	lines += fmt.Sprintf(" Step %d of %d: %s.", place.Position, len(w.Steps), words[place.Position-1])
	lines += stepsLine(words) + reportsBefore(rows, w, words, place.Position)
	return Standing{lines: lines, owners: ownersText(strings.TrimSpace(w.Text))}, nil
}

// stepsLine is ` Steps: 1 <step>, 2 <step>, … then Legion's retro, and its merge where the issue
// has a pull request.`: each step of a task by its place, words, the steps as its plan writes them
// (plan.Words), in the order they run, each at its place from 1 (Walk.Steps), then the retro Legion
// runs after them, and the merge, which a task whose issue has no pull request does not have.
func stepsLine(words []string) string {
	named := make([]string, len(words))
	for i, word := range words {
		named[i] = fmt.Sprintf("%d %s", i+1, word)
	}
	return fmt.Sprintf(" %s: %s then Legion's retro, and its merge where the issue has a pull request.", stepsLabel, strings.Join(named, ", "))
}

// reportsBefore is one ` Step <j> (<step>) reported: <word> version <v>.` for each step of w's
// task before the step at place whose row holds a handoff record: the record that role's
// completion of that step named, which `dispatch handoff-read --issue <issue> --phase <word>
// --version <v>` reads. Each step is named as its plan writes it, its word of words (plan.Words).
// Each step's run keeps a row of its own, the row of its role at its task and place
// (record.PhaseRowAt, which reads a role's row of no step, one that ran before the plan's rows,
// as its step's until the step's own row stands), so a role that stands at two steps names the
// report of each under its own place, and each step before a running one completed in the run
// that led to it: a step that runs again after a return writes its new record on its row before
// the step after it starts. A step whose role writes no handoff has no line: the owner's
// acceptance, whose row holds no record, and a step of a role the deployment defines, whose
// verdict only the task of the step after it names, which runs as step, a phase phase.RoleFor
// names no role for. Step ends with no handoff record (phase.Handoff), the completion route
// refuses a completion of it that names one (api's HANDOFF_RECORD_MISMATCH), and the engine
// writes onto a row only the record the completion names (handoff), so no row of such a step
// holds a record for this to name.
func reportsBefore(rows []record.PhaseRow, w Walk, words []string, place int) string {
	var text strings.Builder
	for j := 1; j < place; j++ {
		role := phase.RoleFor(w.Line[j-1])
		if role == "" {
			continue
		}
		if row := record.PhaseRowAt(rows, role, w.Task, record.TaskPlace{Kind: record.PlaceStep, Position: j}, w.Steps); row.HandoffRecord != "" {
			fmt.Fprintf(&text, " Step %d (%s) %s: %s version %d.", j, words[j-1], reportedLabel, handoffWord(role), row.HandoffVersion)
		}
	}
	return text.String()
}
