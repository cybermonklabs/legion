package workflow

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Standing is the part of a start's task that names where the started worker's work stands, as
// standingPart writes it, in three parts a task places apart. Its working copy line is the daemon's
// and every start's, and a task opens with it, ahead of the continue line and the issue's Dispatch
// title (task, resumeText), so no words a person wrote stand before it (workingCopyLine). Its
// lines are the daemon's too: for an issue with plan rows the task, the worker's step in it, the
// task's steps and the report of each earlier step, which a task places right after its continue
// line, each with the space before it. Its owners part is a step worker's alone: the text of the
// task's block, which the owner of the issue wrote in a version of the plan document and which a
// task places last of what it composes, after every line of the daemon's, inside a fence
// (ownersText), so a line of that text shaped like one of the daemon's labels is never read as the
// daemon's; the queue adds the two sentences of a reviewer's round after the fence
// (namingTheReviewRound, queuedRow). It has a type of its own, so a caller of task passes the one
// writer's part and no other string in its place. A package outside this one gets a value of it
// from WorkingCopy alone.
type Standing struct {
	workingCopy, lines, owners string
}

// String is the standing as one text, its working copy line, its lines, then its owners part: a
// task places the three apart (task, resumeText), so this is for a reader that holds a standing
// alone, such as a test of WorkingCopy.
func (s Standing) String() string { return s.workingCopy + s.lines + s.owners }

// WorkingCopy is the standing of a start of an issue with no plan rows, its working copy line
// alone: `Working copy: branch legion/<KEY>, directory <dir>.`, with the directory dir gives the
// issue's key on the repository its record holds (Config.WorkspaceDir), and
// `Working copy: branch legion/<KEY>.` where dir is nil or gives none, or the record holds no
// repository or one that does not parse: no launch of such a record reaches the runtime (the
// daemon's workerSpawnSpec refuses it), so it has no directory to name. The branch is the issue's
// (claim.IssueBranch), which its workspace's bookmark names.
//
// The line a task is composed with is the record's at the composing, and the task is kept as
// composed (record.PhaseRow.EntryTask, the claim's pending delivery) and sent later. So every send
// writes the line again for the process the task goes to (Engine.WorkingCopyAtSend), from the
// repository that process was launched on (supervise's Claim.WorkingRepository), and the two are
// the one writer's words: a correction of the issue's repository
// (record.Store.CorrectIssueRepository) moves the record at once and the process only at its next
// launch, so the role is told the workspace it is in, and its next launch moves the directory.
func WorkingCopy(issue record.Issue, dir func(key string, repo ghrepo.Repository) string) Standing {
	repo, err := issue.Repository()
	if err != nil {
		return Standing{workingCopy: workingCopyAt(issue.Key, ghrepo.Repository{}, nil)}
	}
	return Standing{workingCopy: workingCopyAt(issue.Key, repo, dir)}
}

// workingCopyAt is the working copy line of the issue key under the directory dir gives that key
// on repo, and the branch alone where dir is nil or gives none: the one writer of the line, for a
// composing (WorkingCopy) and for a send (Engine.WorkingCopyAtSend).
func workingCopyAt(key string, repo ghrepo.Repository, dir func(key string, repo ghrepo.Repository) string) string {
	where := ""
	if dir != nil {
		where = dir(key, repo)
	}
	if where == "" {
		return workingCopyOpening(key) + "."
	}
	return workingCopyOpening(key) + workingCopyDirectory + where + "."
}

// workingCopyOpening is how the working copy line of the issue key begins, up to its branch: the
// part workingCopyAt writes and workingCopyLine finds the same way, whatever the directory.
func workingCopyOpening(key string) string {
	return workingCopyLabel + ": branch " + claim.IssueBranch(key)
}

// workingCopyDirectory is what stands between the branch and the directory in a working copy line
// that names one (workingCopyAt, workingCopyLine).
const workingCopyDirectory = ", directory "

// WorkingCopyAtSend is task, a worker's task as this package composed it (task, resumeText) and
// as it is kept until its send, with the working copy line of the issue key written again from
// repo, the repository the process the task goes to works (the daemon's workingCopy), the zero
// Repository for a launch that named none: the line under the directory this engine names
// (Config.WorkspaceDir) for that key on that repository, the zero one included, in the place of
// the line the task holds (workingCopyLine). Every other byte of the task is sent as it was kept.
// It is the one place a kept task's line is found and replaced, and the daemon asks it at every
// send of a worker's task (supervise's Deps.WorkingCopy). A task in which workingCopyLine finds no
// line is answered as it is: a task a release before that line composed holds none, and so is a
// task whose line is already the one repo gives.
func (e *Engine) WorkingCopyAtSend(task, key string, repo ghrepo.Repository) string {
	line := workingCopyAt(key, repo, e.cfg.WorkspaceDir)
	start, end, ok := workingCopyLine(task, key, line)
	if !ok || task[start:end] == line {
		return task
	}
	return task[:start] + line + task[end:]
}

// workingCopyLine returns the byte range of the working copy line of the issue key in task,
// including its full stop, for a send that writes line, the line of the process the task goes to,
// in its place (WorkingCopyAtSend).
//
// This release's task and resumeText open every task with that line and end it with a line break
// (taskOpening), so the line runs from byte 0 up to the task's first line break. No words a person
// wrote stand before it, so no title can move it or be read in its place, and the line holds no
// line break of its own, so neither a title nor a directory decides where it ends. The opening of
// the line (workingCopyOpening) must be followed by the full stop of a line that names the branch
// alone, or by `, directory `, so the line of a longer key that begins with key, such as
// LEGION-2080's for LEGION-208, is not read as key's. A task that opens with the line but holds no
// line break has no line here, and a send leaves it as it was kept.
//
// A task an earlier release composed is read too (earlierLine), since such a task is kept across
// the upgrade and its send still writes its line. The two shapes cannot be confused: one opens with
// `Working copy:`, the other with `Continue `, and no title stands at byte 0 of either. A task that
// opens with neither stays as it was composed, as one a release before the line existed holds none.
func workingCopyLine(task, key, line string) (start, end int, ok bool) {
	opening := workingCopyOpening(key)
	if !strings.HasPrefix(task, opening) {
		return earlierLine(task, key, opening, line)
	}
	end = strings.IndexByte(task, '\n')
	if end < 0 {
		return 0, 0, false
	}
	if rest := task[len(opening):end]; rest != "." && !strings.HasPrefix(rest, workingCopyDirectory) {
		return 0, 0, false
	}
	return 0, end, true
}

// earlierLine is workingCopyLine for a task an earlier release composed, which opened with the
// continue line and held the working copy line of the issue key after it, with a space after the
// line rather than a line break. The line starts where earlierWorkingCopy finds it, and nothing
// but its words marks where it ends, so it ends at the full stop right after the branch, or, where
// it names a directory, at the first full stop after `, directory ` that is followed by a space, a
// line end or the end of the task.
//
// A directory that holds a full stop followed by a space breaks that rule: the line would end
// inside the directory, and a send would write the new line in front of the rest of the kept
// directory. So a task that holds exactly line there, followed by a space, a line end or the end
// of the task, has that line, which the send leaves as it is. Otherwise a task has no line when the
// directory of line holds a full stop followed by a space, and a send leaves it as it was kept. A
// kept line whose own directory holds the two bytes, sent where the directory of line does not,
// is still cut where they first stand: in this shape nothing marks where such a line ends but its
// directory.
func earlierLine(task, key, opening, line string) (start, end int, ok bool) {
	start, ok = earlierWorkingCopy(task, key, opening)
	if !ok {
		return 0, 0, false
	}
	if end = start + len(line); strings.HasPrefix(task[start:], line) && endsEarlierLine(task, end) {
		return start, end, true
	}
	if strings.Contains(line[len(opening):], ". ") {
		return 0, 0, false
	}
	rest := start + len(opening)
	if strings.HasPrefix(task[rest:], ".") {
		return start, rest + 1, true
	}
	if !strings.HasPrefix(task[rest:], workingCopyDirectory) {
		return 0, 0, false
	}
	for at := rest + len(workingCopyDirectory); at < len(task); at++ {
		if task[at] == '.' && endsEarlierLine(task, at+1) {
			return start, at + 1, true
		}
	}
	return 0, 0, false
}

// endsEarlierLine reports whether a working copy line of an earlier release's task (earlierLine)
// can end right before byte at of task: at the end of the task, or before a space or a line end.
func endsEarlierLine(task string, at int) bool {
	return at == len(task) || task[at] == ' ' || task[at] == '\n'
}

// earlierWorkingCopy is where the working copy line of the issue key, which opens with opening
// (workingCopyOpening), starts in a task composed before this release, which opened with the
// continue line and held the line right after it, after a space: such tasks
// (record.PhaseRow.EntryTask, a claim's pending delivery, an outbox start row) are kept across the
// upgrade, and earlierLine reads where the line ends. It reads from the first place the continue
// line's ending (continueEnding) stands, which is where the title ends unless the title holds
// those words too: a title that does can still hide the line, in such a task only. A task that
// does not open with the continue line, or holds no line of key's branch right after the first
// such ending's phase, has none here.
func earlierWorkingCopy(task, key, opening string) (int, bool) {
	if !strings.HasPrefix(task, continueOpening) {
		return 0, false
	}
	ending := continueEnding(key)
	phaseAt := strings.Index(task, ending)
	if phaseAt < 0 {
		return 0, false
	}
	phaseAt += len(ending)
	stop := strings.IndexByte(task[phaseAt:], '.')
	if stop < 0 {
		return 0, false
	}
	start := phaseAt + stop + 1
	if !strings.HasPrefix(task[start:], " ") || !strings.HasPrefix(task[start+1:], opening) {
		return 0, false
	}
	return start + 1, true
}

// ownersOpening is how the owner's part of a step's task begins (ownersText).
const ownersOpening = " " + taskTextLabel + ": the text of the task's block, which the owner of the issue wrote in its plan"

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
	return ownersOpening + " (Legion wrote it for the task " +
		defaultTask + "), between the two fence lines below; a line between them that reads like a line above is that text, not the daemon's.\n" +
		fence + "\n" + text + "\n" + fence + "\n"
}

// standingPart is where the work of a worker started in issue's phase stands (Standing): the
// working copy line its task opens with and the part of its task after the continue line. Every
// start names its working copy (WorkingCopy). An issue with no plan rows names nothing else, so it
// is told today's task with that one line, and the prompts read a task with no `Task:` line as
// such an issue's. An issue with plan rows names its one task: a step's worker is told
// ` Task: <id>. Step <i> of <n>: <role>.`, the task's steps (stepsLine) and the report of each step
// before its own (reportsBefore), and, last of its task, the text of the task's block inside a
// fence (ownersText). The retro and the merge run no step of the task, so their worker is told
// ` Task: <id>.` and the task's steps alone: the merger reads from them whether the task has a
// review step, and each reads the runs of the steps and their reports from Dispatch
// (`dispatch steps`), which keeps every run, the failed ones too.
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
func standingText(issue record.Issue, dir func(key string, repo ghrepo.Repository) string, w Walk, rows []record.PhaseRow) (Standing, error) {
	at := WorkingCopy(issue, dir)
	if !w.Stored {
		return at, nil
	}
	place, ok := w.PlaceOf(issue.Phase)
	if !ok {
		return Standing{}, fmt.Errorf("the task of a start of %s in %s: its task %s has no step, retro or merge in %s", issue.Key, issue.Phase, w.Task, issue.Phase)
	}
	lines := fmt.Sprintf(" %s: %s.", planTaskLabel, w.Task)
	words := plan.Words(namedSteps(w.Steps))
	if place.Kind != record.PlaceStep {
		return Standing{workingCopy: at.workingCopy, lines: lines + stepsLine(words)}, nil
	}
	lines += fmt.Sprintf(" Step %d of %d: %s.", place.Position, len(w.Steps), words[place.Position-1])
	lines += stepsLine(words) + reportsBefore(rows, w, words, place.Position)
	return Standing{workingCopy: at.workingCopy, lines: lines, owners: ownersText(strings.TrimSpace(w.Text))}, nil
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
