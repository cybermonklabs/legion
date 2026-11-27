package workflow

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Standing is the part of a start's task that names where the started worker's work stands, as
// standingPart writes it, in three parts a task places apart. Its lines are the daemon's: the
// working copy line for every start, and for an issue with plan rows the task, the worker's step
// in it, the task's steps and the report of each earlier step, which a task places right after
// its continue line. Its pull request part is the daemon's too: the pull request of the worker's
// task (pullRequestPart), which a start places after the report of the step before it (task) and a
// resume right after the lines (resumeText). Its owners part is a step worker's alone: the text of
// the task's block, which the owner of the issue wrote in a version of the plan document and which
// a task places last of what it composes, after every line of the daemon's, inside a fence
// (ownersText), so a line of that text shaped like one of the daemon's labels is never read as the
// daemon's; the queue adds the two sentences of a reviewer's round after the fence
// (namingTheReviewRound, queuedRow). It has a type of its own, so a caller of task passes the one
// writer's part and no other string in its place. A package outside this one gets a value of it
// from WorkingCopy alone.
type Standing struct {
	lines, pull, owners string
}

// String is the standing as one text, its lines, its pull request part, then its owners part: a
// task places the three apart (task, resumeText), so this is for a reader that holds a standing
// alone, such as a test of WorkingCopy.
func (s Standing) String() string { return s.lines + s.pull + s.owners }

// WorkingCopy is the standing of a start of an issue with no plan rows, its working copy line
// alone: ` Working copy: branch legion/<KEY>, directory <dir>.`, with the directory dir gives the
// issue's own branch (Config.WorkspaceDir), and ` Working copy: branch legion/<KEY>.` where dir is
// nil or gives none. The branch is the issue's (claim.IssueBranch), which its workspace's bookmark
// names.
func WorkingCopy(issue record.Issue, dir func(record.Issue, string) string) Standing {
	return taskWorkingCopy(issue, claim.IssueBranch(issue.Key), dir)
}

// taskWorkingCopy is the working copy line of a start of issue's task on branch, the one writer of
// that line: ` Working copy: branch <branch>, directory <dir>.`, with the directory dir gives that
// branch's working copy (Config.WorkspaceDir), and ` Working copy: branch <branch>.` where dir is
// nil or gives none. A task on the issue's own branch is told the issue's line (WorkingCopy).
func taskWorkingCopy(issue record.Issue, branch string, dir func(record.Issue, string) string) Standing {
	where := ""
	if dir != nil {
		where = dir(issue, branch)
	}
	if where == "" {
		return Standing{lines: fmt.Sprintf(" %s: branch %s.", workingCopyLabel, branch)}
	}
	return Standing{lines: fmt.Sprintf(" %s: branch %s, directory %s.", workingCopyLabel, branch, where)}
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
// task after the continue line (Standing). Every start names its working copy: the issue's for an
// issue with no plan rows (WorkingCopy), and its task's for one with plan rows (taskWorkingCopy,
// on the task's branch). An issue with no plan rows names nothing else, so it is told today's task
// with that one line, and the prompts read a task with no `Task:` line as such an issue's. An
// issue with plan rows names the task of the worker's step: a step's worker is told
// ` Task: <id>. Step <i> of <n>: <role>.`, the task's steps (stepsLine), at the first step of a task
// that needs other tasks the work it starts from (startsFrom) and the join it repairs where GitHub
// refused one as a conflict (joinToRepair), at a step of the type implementation the base of its
// task's pull request where that is not the issue's default branch (pullRequestBase), the report of
// each step before its own (reportsBefore), and, last of its task, the text of the task's block
// inside a fence (ownersText). The retro and the merge run no step of the task, so their worker is
// told ` Task: <id>.` and the task's steps alone: the merger reads from them whether the task has a
// review step, and each reads the runs of the steps and their reports from Dispatch
// (`dispatch steps`), which keeps every run, the failed ones too.
//
// Every start, and every resume, names the pull requests of s's step (pullRequestText): its task's
// own first, a task of a plan its own row and an issue with no plan rows the row of its merge
// (taskPullRequest), then each other pull request the step works or that holds the issue's close. A
// worker resumed in a fresh session has no earlier task to read the number from, and with none it
// would read the pull request on legion/<KEY>, which is another task's wherever that task holds the
// issue's own branch. pull is the task's own row as the start's handler has it (taskPull): the row
// it read to judge its move, which the standing names with no read of its own, so the transaction
// reads the row once; or none read, which is read here as its row alone (taskPullRequestRow), since
// the standing names its number and its merge forward and writes nothing of it.
//
// The phase the worker starts in is the phase of s's copy, which each caller has put there before
// it composes the task, and s's walk is the walk of its task as the caller read it, after any move
// of its task (moveTask). What the plan tells the step is read on s's plan, which the caller read
// in its transaction, or the start read again once it gave the task its branch (startTaskCopy). A
// run with plan rows whose line holds no place in that phase is refused: no start of such a run
// names one, and a task that named none would read as an issue with no plan rows.
func (e *Engine) standingPart(ctx context.Context, tx pgx.Tx, s TaskScope, pull taskPull) (Standing, error) {
	issue, w := s.issue, s.walk
	var rows []record.PhaseRow
	var told planTold
	if place, _ := w.placeIn(issue.Phase); w.Stored && place.Kind == record.PlaceStep {
		var err error
		if rows, err = e.store.Phases(ctx, tx, issue.Key); err != nil {
			return Standing{}, err
		}
		if told, err = planToldOf(issue, s.plan, w.Task); err != nil {
			return Standing{}, err
		}
	}
	pr := pull.row
	if !pull.read {
		var err error
		if pr, err = e.taskPullRequestRow(ctx, tx, s); err != nil {
			return Standing{}, err
		}
	}
	text, err := e.pullRequestText(ctx, tx, s, pr)
	if err != nil {
		return Standing{}, err
	}
	return standingText(issue, e.cfg.WorkspaceDir, w, rows, told, text)
}

// pullRequestText is what a step of s's task is told of its issue's pull requests
// (pullRequestPart): own, its task's own pull request (nil for none), and the pull requests the
// step works or that hold the issue's close beside it (scopePullRequests). The start and the resume
// of every step read it (standingPart).
func (e *Engine) pullRequestText(ctx context.Context, tx pgx.Tx, s TaskScope, own *record.PullRequest) (string, error) {
	pulls, err := e.scopePullRequests(ctx, tx, s, own)
	if err != nil {
		return "", err
	}
	return pullRequestPart(s.issue, own, pulls), nil
}

// acceptancePart is the `Pull request:` line of the phase-finished notice that hands the architect
// its acceptance of s's task (transition, dispatch://INFRA-5910 decision 21, r1): each open pull
// request of the issue's set (record.Store.PullRequestSetsOf), each named as owner/name#number at
// its head, so the architect accepts the code each one stands at; own, the task's own pull request
// as the move read it, first where it is open, and a pull request the issue links marked so; then
// each link that holds the issue's close with no row to name a head of (setHolds), its read owed
// or Legion unable to read it, with why it holds the close (heldLink.holdsClause). "" where it
// names none.
func (e *Engine) acceptancePart(ctx context.Context, tx pgx.Tx, s TaskScope, own *record.PullRequest) (string, error) {
	sets, err := e.store.PullRequestSetsOf(ctx, tx, []string{s.issue.Key})
	if err != nil {
		return "", err
	}
	holds, err := setHolds(ctx, e.store, tx, s.issue.Key)
	if err != nil {
		return "", err
	}
	at := func(pr record.PullRequest) string {
		named := prName(pr.Repo, pr.Number) + " at " + pr.HeadSHA
		if pr.Linked {
			named += " (linked on the issue)"
		}
		return named
	}
	var named []string
	if own != nil && own.State == record.PullRequestOpen {
		named = append(named, at(*own))
	}
	for _, pr := range sets[s.issue.Key] {
		if pr.State == record.PullRequestOpen && (own == nil || !e.samePullRequest(pr, *own)) {
			named = append(named, at(pr))
		}
	}
	for _, held := range holds {
		if held.row == nil {
			named = append(named, fmt.Sprintf("%s (linked on the issue; %s)", prName(held.link.Repo, held.link.Number), held.holdsClause()))
		}
	}
	if len(named) == 0 {
		return "", nil
	}
	return fmt.Sprintf(" %s: %s.", pullRequestLabel, strings.Join(named, "; ")), nil
}

// taskPull is the pull request of a started step's task as the start has it. A handler that read
// that row in its transaction to judge its move (a transition: whether the move needs a pull
// request, the review round, whether GitHub merged it; the retry of a held phase: its review
// round; a ready task's first start, startReady: the merge forward after it) hands its read on
// (readPull), and the start's task names that row (standingPart) with no read of its own. Every
// other start has read none (the zero taskPull), and its standing reads it.
type taskPull struct {
	row  *record.PullRequest
	read bool
}

// readPull is the pull request of a step's task as its handler read it, nil where the task has
// none (taskPull).
func readPull(row *record.PullRequest) taskPull { return taskPull{row: row, read: true} }

// planTold is what a step's task is told from its issue's plan beside the walk of its task
// (standingPart reads it): base, the base of the task's pull request where the plan gives one other
// than the issue's default branch (record.StoredPlan.BaseOf), and "" where it gives that branch;
// join, that base where it is the task's join branch, the branch the join of its needs is made on,
// and "" for a task with no join branch; conflicts, each task whose work GitHub refused to join as a
// conflict (record.PlanTask.JoinConflicts): a need, or for a part the task it is a part of, with the
// branch the task starts from that holds its work (record.StoredPlan.StartsFrom,
// record.StartBranch.Holds), in the order the row holds them: the need's own branch, the default
// branch where its work merged, since GitHub can have deleted its branch at that merge, or the
// branch of the part's task; and
// unmerged, whether GitHub refused as a conflict the merge of the task's join branch into its
// branch (record.PlanTask.JoinUnmerged), false for a task with no join branch.
type planTold struct {
	base      string
	join      string
	conflicts []joinConflict
	unmerged  bool
}

// joinConflict is a task whose work GitHub refused to join into a task as a conflict, a need or a
// part's task, and the branch the task starts from that holds its work (record.StoredPlan.StartsFrom).
type joinConflict struct {
	task, branch string
}

// planToldOf is planTold of task of stored, the plan of issue's run, and nothing for no plan. A task
// or a need the plan does not hold is an error (record.StoredPlan.BaseOf). A conflict of a need the
// task does not start from, which no join of its needs now made, is not told.
func planToldOf(issue record.Issue, stored *record.StoredPlan, task string) (planTold, error) {
	if stored == nil {
		return planTold{}, nil
	}
	var told planTold
	base, join, err := stored.BaseOf(issue, task)
	if err != nil {
		return planTold{}, err
	}
	if base != issue.DefaultBranch {
		told.base = base
	}
	if join {
		told.join = base
	}
	starts, err := stored.StartsFrom(issue, task)
	if err != nil {
		return planTold{}, err
	}
	of, _ := stored.TaskOf(task)
	for _, need := range of.JoinConflicts {
		if at := slices.IndexFunc(starts, func(start record.StartBranch) bool { return slices.Contains(start.Holds, need) }); at >= 0 {
			told.conflicts = append(told.conflicts, joinConflict{task: need, branch: starts[at].Branch})
		}
	}
	told.unmerged = join && of.JoinUnmerged
	return told, nil
}

// standingText is standingPart's text for the worker of issue's phase, from the walk w of its
// run, the directory dir gives each working copy, its phases rows and what its plan tells its
// step, which only a step's text reads, and pull, what it is told of its pull requests
// (pullRequestPart), "" for none.
func standingText(issue record.Issue, dir func(record.Issue, string) string, w Walk, rows []record.PhaseRow, told planTold, pull string) (Standing, error) {
	if !w.Stored {
		at := WorkingCopy(issue, dir)
		at.pull = pull
		return at, nil
	}
	at := taskWorkingCopy(issue, w.Branch, dir)
	place, ok := w.placeIn(issue.Phase)
	if !ok {
		return Standing{}, fmt.Errorf("the task of a start of %s in %s: its task %s has no step, retro or merge in %s", issue.Key, issue.Phase, w.Task, issue.Phase)
	}
	lines := at.lines + fmt.Sprintf(" %s: %s.", planTaskLabel, w.Task)
	words := plan.Words(namedSteps(w.Steps))
	// The task merges where its line ends with the retro and the merge after its steps (Walk.steps).
	merges := len(w.steps()) < len(w.Line)
	if place.Kind != record.PlaceStep {
		return Standing{lines: lines + stepsLine(words, merges), pull: pull}, nil
	}
	lines += fmt.Sprintf(" Step %d of %d: %s.", place.Position, len(w.Steps), words[place.Position-1])
	lines += stepsLine(words, merges)
	if place.Position == 1 {
		where := ""
		if dir != nil {
			where = dir(issue, w.Branch)
		}
		lines += startsFrom(w.Needs) + joinToRepair(issue.Key, w.Branch, where, w.Needs, told)
	}
	if w.Steps[place.Position-1].Type == plan.TypeImplementation {
		lines += pullRequestBase(told.base)
	}
	lines += reportsBefore(rows, w, words, place.Position)
	return Standing{lines: lines, pull: pull, owners: ownersText(strings.TrimSpace(w.Text))}, nil
}

// pullRequestPart is what the worker of issue's phase is told of its pull requests (Standing.pull),
// pr being its task's own and pulls the others its step works or that hold the issue's close
// (scopePullRequests): ` Pull request: <own>.`, each pull request named as owner/name#number, since
// an issue's pull requests can be of several repositories and a number alone names none of them
// (dispatch://INFRA-5910 decision 21, r1). After its task's own, or `none of its own` where the
// task has none, come each pull request the issue links that the step's scope works, `(linked on
// the issue)`, and then each other that holds the issue's close, with why it holds it; the merger,
// in merging, is told of each linked pull request of its scope whether an approval stands at its
// head (approvalStands), and asked to merge it only where one does, since it merges a pull request
// only when its review allows it (D6, decision 6). Nothing where the task has no pull request and
// none other is named. A merger in merging at the head a merge forward landed on where the task's
// reviewer step already ran (mergeForwardLanded), while the review that landing asked of the
// reviewer stands for that head (mergeForwardReviewOwed), is told after it to merge that head only
// once the reviewer's approval of it is on the pull request: the daemon records no decision outside
// reviewing, so the approval is the merger's to read on GitHub. A merger resumed in a fresh session
// is told the same as one started at retro's completion, the one move into merging, or at the retry
// of a held merging (retryPhase): each composes its task from the row its own transaction reads
// (standingPart), so the part says what the record holds.
func pullRequestPart(issue record.Issue, pr *record.PullRequest, pulls scopedPulls) string {
	var named []string
	switch {
	case pr != nil && pr.Number != 0:
		named = append(named, prName(pr.Repo, pr.Number))
	case len(pulls.scoped) > 0 || len(pulls.holding) > 0:
		named = append(named, "none of its own")
	}
	for _, linked := range pulls.scoped {
		clause := "linked on the issue"
		if issue.Phase == phase.Merging {
			clause += "; no approval stands at its head, so do not merge it"
			if approvalStands(linked) {
				clause = "linked on the issue; an approval stands at its head, so merge it too"
			}
		}
		named = append(named, fmt.Sprintf("%s (%s)", prName(linked.Repo, linked.Number), clause))
	}
	for _, held := range pulls.holding {
		named = append(named, fmt.Sprintf("%s (linked on the issue; %s)", prName(held.link.Repo, held.link.Number), held.holdsClause()))
	}
	var text string
	if len(named) > 0 {
		text = fmt.Sprintf(" %s: %s.", pullRequestLabel, strings.Join(named, "; "))
	}
	if pr != nil && issue.Phase == phase.Merging && mergeForwardReviewOwed(*pr) {
		text += fmt.Sprintf(" Head %s is the merge of %s forward that landed after the review, and the reviewer is asked to read it: merge head %s only once the reviewer's approval of it is on the pull request.",
			pr.HeadSHA, pr.MergeForward.Base, pr.HeadSHA)
	}
	return text
}

// approvalStands says whether the decision pr holds of its own (record.PullRequest.ReviewDecision,
// dispatch://INFRA-5910 decision 7) approves the code at pr's head (classify.ApprovalStands): what
// the merger is told before it merges a linked pull request of its scope (pullRequestPart).
func approvalStands(pr record.PullRequest) bool {
	d := pr.ReviewDecision
	return d != nil && d.State == "approved" && classify.ApprovalStands(pr, d.Head)
}

// stepsLine is ` Steps: 1 <role>, 2 <role>, ….`: each step of a task by its place, words, the steps
// as its plan writes them (plan.Words), in the order they run, each at its place from 1
// (Walk.Steps). Where merges, the task's line ends with Legion's retro and merge after its last
// step (plan.Line), and so does the steps line: ` Steps: 1 <role>, 2 <role>, … then Legion's retro
// and merge.` A task that does not merge is done when its last step passes, and its line ends
// there.
func stepsLine(words []string, merges bool) string {
	named := make([]string, len(words))
	for i, word := range words {
		named[i] = fmt.Sprintf("%d %s", i+1, word)
	}
	if !merges {
		return fmt.Sprintf(" %s: %s.", stepsLabel, strings.Join(named, ", "))
	}
	return fmt.Sprintf(" %s: %s then Legion's retro and merge.", stepsLabel, strings.Join(named, ", "))
}

// startsFrom is what the first step of a task is told of the tasks the task needs (Walk.Needs),
// each of which is done before the task starts, and whose work its working copy starts from:
// ` This task starts from the joined work of tasks <ids>, which are done.` for several, in the
// order the task's block names them, ` This task starts from the work of task <id>, which is
// done.` for one, and nothing for a task that needs none.
func startsFrom(needs []string) string {
	switch len(needs) {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf(" This task starts from the work of task %s, which is done.", needs[0])
	}
	return fmt.Sprintf(" This task starts from the joined work of tasks %s, which are done.", strings.Join(needs, ", "))
}

// joinToRepair is what the first step of a task is told where GitHub refused part of the join of its
// needs as a conflict (planTold), so that its agent repairs it before its own work, at where, the
// directory the runtime places the task's working copy at (`jj` with no `-R` where none is known),
// each repair a merge commit that names what it joins, pushed with the commands that push it, and
// the agent's own work then starts on a new commit, so no push of that work rewrites the pushed
// repair.
//
// Where the task has a join branch (planTold.join), the base of its pull request, the conflicts of
// its needs are repaired on that join branch and pushed there, so the pull request, against it,
// shows the task's own work alone; then the task's own work starts on its branch, whose head the
// join made at the join branch's unrepaired head, from the repaired join: a new commit on the heads
// of the task's branch and the join branch, which is the repaired join alone at a first start and
// the merge of the two where the branch holds work of its own. Where GitHub could not merge the join
// branch into the task's branch, for the task's own work there (planTold.unmerged), the agent merges
// the join branch, repaired first where its needs conflicted, into the task's branch, and the join
// branch takes none of it. A task with no join branch, the task that merges and one none of whose
// steps implements, joins its conflicting needs on its own branch, in its working copy, as its
// first commit. Nothing for a join GitHub ended with no conflict.
func joinToRepair(key, branch, where string, needs []string, told planTold) string {
	if len(told.conflicts) == 0 && !told.unmerged {
		return ""
	}
	jj := "jj"
	if where != "" {
		jj = "jj -R " + where
	}
	resolve := fmt.Sprintf("repair each path that `%s resolve --list` lists", jj)
	push := func(to string) string {
		return fmt.Sprintf("then `%s bookmark set %s -r @` and `%s git push --bookmark %s`", jj, to, jj, to)
	}
	var lines string
	if len(told.conflicts) > 0 {
		tasks := make([]string, len(told.conflicts))
		heads := make([]string, len(told.conflicts))
		for i, conflict := range told.conflicts {
			tasks[i] = "task " + conflict.task
			heads[i] = conflict.branch + "@origin"
		}
		named := strings.Join(tasks, ", ")
		joins := fmt.Sprintf("%s: join the work of %s", key, named)
		if told.join == "" {
			return fmt.Sprintf(" GitHub could not join the work of %s into this branch: their changes conflict."+
				" Before your own work, join it in your working copy: `%s git fetch`, then `%s new @- %s -m \"%s\"`, %s, %s."+
				" That join is the first commit of your step: start your own work after it with `%s new`.",
				named, jj, jj, strings.Join(heads, " "), joins, resolve, push(branch), jj)
		}
		lines = fmt.Sprintf(" GitHub could not join the work of %s into the join branch %s, the base of this task's pull request: their changes conflict."+
			" Before your own work, repair that join on %s, so the pull request shows this task's own work alone:"+
			" `%s git fetch`, `%s bookmark track %s@origin`, then `%s new %s@origin %s -m \"%s\"`, %s, %s.",
			named, told.join, told.join, jj, jj, told.join, jj, told.join, strings.Join(heads, " "), joins, resolve, push(told.join))
		if !told.unmerged {
			return lines + fmt.Sprintf(" Then start your own work on the branch %s from the repaired join: `%s new 'heads(%s@origin | %s@origin)'`,"+
				" and %s there; your first commit on %s is from that join.", branch, jj, branch, told.join, resolve, branch)
		}
		lines += " Then merge the repaired join into this branch, which GitHub could not do: its changes conflict with this task's own work:"
	} else {
		lines = fmt.Sprintf(" GitHub could not merge the join branch %s into this branch: its changes conflict with this task's own work."+
			" Before your own work, merge it here: `%s git fetch`, then", told.join, jj)
	}
	return lines + fmt.Sprintf(" `%s new %s@origin %s@origin -m \"%s: join the work of tasks %s\"`, %s, %s."+
		" That merge is the first commit of your step, and the join branch takes none of it: start your own work after it with `%s new`.",
		jj, branch, told.join, key, strings.Join(needs, ", "), resolve, push(branch), jj)
}

// pullRequestBase is what a step of the type implementation is told of the base of its task's
// pull request, base, where the plan gives one other than the issue's default branch
// (planTold.base): the branch to open it against, and the command that does, so the review of the
// task reads its own work alone. Nothing where base is "", the issue's default branch.
func pullRequestBase(base string) string {
	if base == "" {
		return ""
	}
	return fmt.Sprintf(" Open the pull request of this task against %s: gh pr create --base %s.", base, base)
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
