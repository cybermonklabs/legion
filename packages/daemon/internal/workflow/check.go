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

// CheckLookBound bounds every look of a production check: a check_look row due this long after
// the look's wake ends a look nobody reported (checkLookEnded). A look that began before the end
// of the project's latest pause counts it instead from the later of that end and its role's last
// ready, and counts nothing while that role's launch is still coming up: its row, due earlier,
// waits until then (internal/daemon's lookBoundAfterPause). A first look publishes to the deploy
// role and reports pending in minutes; a woken look that drives the change in production is one
// agent turn, and the longest production checks measured ran within an hour. Without a bound a
// silent checking agent keeps its pod for the whole of the check's bound, the cost the check after
// the close exists to remove.
const CheckLookBound = 2 * time.Hour

// CheckWakeFloor is the soonest a look may follow the report that named it: a wake named less
// than this far ahead is moved out to it (clampWake). A look costs a pod launch and an agent
// turn, so a report naming its own moment would spend both faster than a look takes to start.
const CheckWakeFloor = time.Minute

// The causes a look is woken by: what the check's record names as its last wake, and what the
// check task tells the checking agent woke it.
const (
	wakeMerge = "merge"
	wakeBound = "bound"
	wakeNamed = "check wake"
)

// checkRole is Config.ProductionCheckRole read in one place, so no two sites can disagree on who
// a check is for.
func (e *Engine) checkRole() claim.Role { return e.cfg.ProductionCheckRole }

// checkClaim is the claim of the agent that works check, the one reader of a check's claim, and
// each row of the check that names a claim names this one: the start of each look (startLook), the
// suspend that follows a pending look (endLookPending) and the issue_close of its close
// (closeCheck). For a check of a task (check.Task set) it is the claim of the check's agent
// (record.CheckAgent) over the plan of the check's run (check.Generation): the task's last agent
// of the check's role, else a fresh agent of the task. The plan alone decides it, so the claim a
// leave keeps for a check it is about to open is the claim that check's rows will name. For a check
// of an issue with no plan rows, or one a release before checks carried their task wrote, it is the
// claim of the worker on the role's step (record.WorkerClaim), the role's own claim there.
func (e *Engine) checkClaim(ctx context.Context, tx pgx.Tx, check record.ProductionCheck) (claim.Token, error) {
	project, err := claim.ProjectToken(check.Project)
	if err != nil {
		return "", fmt.Errorf("the claim of %s's production check: %w", check.Issue, err)
	}
	if check.Task == "" {
		rows, err := e.store.Phases(ctx, tx, check.Issue)
		if err != nil {
			return "", err
		}
		return record.WorkerClaim(project, check.Issue, check.Role, rows)
	}
	stored, err := e.store.Plan(ctx, tx, check.Issue, check.Generation)
	if err != nil {
		return "", err
	}
	var steps []record.PlanStep
	if stored != nil {
		steps = stored.Steps[check.Task]
	}
	return claim.AgentToken(project, check.Issue, record.CheckAgent(steps, check.Task, check.Role))
}

// openCheck opens issue's production check on its merged pull request pr and starts its first
// look, woken by the merge: the record (record.ProductionCheck, bound at the project's
// ProductionCheckBound from now, carrying the role that works it and the task whose merge it
// checks, pr's), the check_bound row due at that bound, then the look (startLook). A project whose
// `production_check` is off has no role to work a check (checkRole), and opens nothing at all: no
// record, no bound row and no look, only the line saying the merge had nothing to check.
// wake is only how the open is logged: a lingering tree's member has its claims suspended,
// so the look's start row is a wake of the checking role's claim, where a running tree's is a task
// handed to that role's live session; the rows are the same, since the start resumes either. Each
// merge opens a check of its own, of the run the issue holds (record.ProductionCheck.Generation):
// the merge of a task that is not the issue's last opens it at that merge while the issue's other
// tasks go on (closeMerged), and the merge that ends the issue at the issue's leave, so a run that
// merges the work of several tasks holds a check of each merge, side by side. A
// merge of a later run — a rerun's, after a re-entry closed the first — also closes each check
// of an earlier run still open as not run (markCheckClosed), since nothing would look at it
// again; the bound row of such a check then finds it closed and changes nothing (checkBound). The
// same merge seen again — at its own close and again at the leave that checks each merge of the
// run, or a done child a person parks and sets done again, each move a leave — opens nothing,
// since one of the issue's checks, open or recorded, of any run, is that merge's. The check's
// OpenedAt is its identity (record.ProductionCheck.ID), so it is the clock truncated to the
// microsecond, Postgres's timestamptz precision, or a microsecond after the latest check of the
// issue where that is not earlier: the value the rows and facts of this transaction carry equals
// the one every later read of the record returns, and two checks opened in one transaction are two.
func (e *Engine) openCheck(ctx context.Context, tx pgx.Tx, issue record.Issue, pr record.PullRequest, wake bool) error {
	role := e.checkRole()
	if role == "" {
		e.logOnCommit(ctx, "workflow: no production check: this project checks nothing in production", "issue", issue.Key,
			"mergeCommit", pr.MergeSHA, "pullRequest", pr.Number)
		return nil
	}
	checks, err := e.store.ProductionChecksOf(ctx, tx, issue.Key)
	if err != nil {
		return err
	}
	for _, existing := range checks {
		if e.checksMerge(existing, pr) {
			state := "open"
			if !existing.Open() {
				state = "closed as " + string(existing.Outcome)
			}
			e.logOnCommit(ctx, "workflow: production check not opened again: one of the issue's checks is this merge's", "issue", issue.Key,
				"mergeCommit", pr.MergeSHA, "pullRequest", pr.Number, "check", existing.OpenedAt, "state", state)
			return nil
		}
	}
	for i := range checks {
		if !checks[i].Open() || checks[i].Generation >= issue.Generation {
			continue
		}
		if err := e.markCheckClosed(ctx, tx, &checks[i], record.CheckNotRun); err != nil {
			return err
		}
	}
	// The new check's moment is later than each check of the issue: the store keys a check by its
	// issue and that moment, so a second check opened at the same reading of the clock, as a leave
	// that checks two merges opens them, would be written over the first.
	now := e.now().Truncate(time.Microsecond)
	for _, existing := range checks {
		if !existing.OpenedAt.Before(now) {
			now = existing.OpenedAt.Add(time.Microsecond)
		}
	}
	check := record.ProductionCheck{
		Issue: issue.Key, Project: issue.Project, Tree: issue.Tree, Repo: pr.Repo, Number: pr.Number, MergeSHA: pr.MergeSHA,
		OpenedAt: now, BoundAt: now.Add(e.cfg.ProductionCheckBound), Role: role, Generation: issue.Generation, Task: pr.Task,
	}
	if err := e.enqueueAt(ctx, tx, issue.Key, record.CheckBound{}, check.BoundAt); err != nil {
		return err
	}
	first := "live"
	if wake {
		first = "wake"
	}
	e.logOnCommit(ctx, "workflow: production check opened", "issue", issue.Key, "mergeCommit", pr.MergeSHA, "pullRequest", pr.Number,
		"role", role, "bound", check.BoundAt, "firstLook", first)
	return e.startLook(ctx, tx, issue, &check, wakeMerge, pr.MergeSHA, false)
}

// checksMerge says whether check is the check of the merge pr: the same repository (sameRepository,
// which reads GitHub's renames), the same pull request number and the same merge commit, where both
// name one. It is the one compare of "a check of the issue checks this merge": openCheck opens no
// second check of a merge one of the issue's checks checks, and the issue's leave keeps the claim of
// each check it will open for a merge no check checks yet (leave).
func (e *Engine) checksMerge(check record.ProductionCheck, pr record.PullRequest) bool {
	return e.sameRepository(check.Repo, pr.Repo) && check.Number == pr.Number &&
		(check.MergeSHA == "" || pr.MergeSHA == "" || check.MergeSHA == pr.MergeSHA)
}

// startLook starts the next look of issue's check, woken by cause with detail, last marking the
// bound's look: the record counts the look open from now with that wake and no wake standing —
// the row this look starts from is spent, and only a pending report names the next one — the
// check's role (record.ProductionCheck.Role, the role the check opened with, never the setting as
// it stands now) is started on the check task, on the claim of the check's agent (checkClaim) — a
// start with no phase, fenced on the check and its look (record.SuperviseRequest.CheckLook and
// CheckOpenedAt) rather than on the generation, and stamped with the generation the issue holds as
// the delivery's run — and a check_look row due at the look bound, naming the check and the look,
// ends the look should nobody report it.
func (e *Engine) startLook(ctx context.Context, tx pgx.Tx, issue record.Issue, check *record.ProductionCheck, cause, detail string, last bool) error {
	now := e.now()
	check.Looks++
	check.LookOpen, check.LookWakeAt = true, &now
	check.LastWakeKind, check.LastWakeDetail, check.LastWakeAt = cause, detail, &now
	check.WakeAt, check.Last = nil, last
	if err := e.store.PutProductionCheck(ctx, tx, *check); err != nil {
		return err
	}
	token, err := e.checkClaim(ctx, tx, *check)
	if err != nil {
		return err
	}
	if err := e.enqueue(ctx, tx, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: check.Role, Claim: token,
		Generation: issue.Generation, CheckLook: check.Looks, CheckOpenedAt: check.OpenedAt, Task: checkTask(issue, *check, cause, detail, last)}); err != nil {
		return err
	}
	if err := e.enqueueAt(ctx, tx, issue.Key, record.CheckLook{Look: check.Looks, OpenedAt: check.OpenedAt}, now.Add(CheckLookBound)); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: production check look started", "issue", issue.Key, "mergeCommit", check.MergeSHA, "look", check.Looks,
		"role", check.Role, "cause", cause, "detail", detail, "last", last)
	return nil
}

// checkTaskLead opens every check task: a resumed session tells the check from its last phase
// task by this line (the phase tasks open with "Continue"), and a test pins it.
const checkTaskLead = "Production check of "

// checkTask is what the check's role is told at a look of the production check of a merge of the
// issue, which may still be in the workflow with other tasks to run, or closed. The session it
// reaches may be fresh — a woken pod after a lost volume — so the text stands on its own: the
// issue, the repository, the pull request, the merge commit, the look and what woke it (wokenBy),
// whether it is the last, and the rules of the check in prose: each requirement only the deploy can
// show observed as it names it, the three verdicts and what each asks of the pull request, no
// commit, the wake a pending names for its own next look, the suspension that follows it once the
// issue has left the workflow (endLookPending), and nothing to wait for in the session, since the
// look ends with the report. A merge production carries with such a requirement still unobserved
// is a pending while a look follows. A requirement observed as not met is a fail on every look. On
// the last look one still unobserved is a fail too, whether or not production carries the merge.
// The last look's task gives pending only for an issue with none of those requirements, a pending
// that closes the check as not run. The fail clause says so once, the same on every look, in terms
// of the look that follows.
func checkTask(issue record.Issue, check record.ProductionCheck, cause, detail string, last bool) string {
	merge := check.MergeSHA
	if merge == "" {
		merge = "unknown, read it from the pull request on GitHub"
	}
	var text strings.Builder
	fmt.Fprintf(&text, "%s%s: %s.\n", checkTaskLead, issue.Key, issue.Title)
	fmt.Fprintf(&text, "Repository %s, pull request #%d, merge commit %s. Look %d, woken by %s.", check.Repo, check.Number, merge, check.Looks, wokenBy(cause, detail))
	openedAt := check.OpenedAt.UTC().Format(time.RFC3339Nano)
	fmt.Fprintf(&text, " The check opened at `%s`: your report names it with check_opened_at `%s`.",
		openedAt, openedAt)
	if last {
		text.WriteString(" This is the last look: no look follows.")
	}
	text.WriteString("\nDo what the deployment instructions say to reach production and read whether production carries the merge. " +
		"Where it does, drive the changed path and observe each requirement of the issue that only the deploy can show, read from the spec version that bound the review, each as the requirement names it: observing something else is never that requirement observed. " +
		"Then report with the legion tool's handoff_complete, check_opened_at from this task and a verdict, committing nothing: " +
		"pass only once you drove the changed path in production and observed every one of those requirements, and recorded the issue's after_merge check with dispatch result at the merge commit, its observed naming each requirement and what you saw of it and what woke this look, and put the line that command prints in the pull request body; " +
		"fail for a defect, never an issue you file (skill://legion-worker/references/findings.md), and a requirement observed as not met, or still unobserved when no look follows this one, whether or not production carries the merge, is a defect: a defect with a clear fix goes to the controller topic your Legion addressing line names, with its evidence posted as a comment on the controller's report issue; a defect that needs a decision is a dispatch ask on that report issue, whose reference you then publish to the controller topic. Either names this issue, the pull request, the merge commit, what you drove and what failed; record the after_merge check as a fail with dispatch result at the merge commit, and name the message or the ask in its observed, in the line you put in the pull request body and in the summary.")
	if last {
		text.WriteString(" Report pending only for an issue with none of those requirements, the summary naming what you could not drive, with no result recorded. This last look's pending carries no wake_at and closes the check as not run: " +
			"before reporting it, write on the pull request why the check did not run within the bound.")
	} else {
		text.WriteString(" Report pending when production does not carry the merge yet, or carries it with one of those requirements still unobserved and a look still to follow, the summary naming what is missing and what you will look for next, with no result recorded and the pull request body saying the check is still open on the merge commit. A pending report also carries wake_at, an RFC 3339 time: when you want your next look. " +
			"Besides it, only the look at the check's bound and the retry of a look nobody reported start one. A time less than a minute ahead is moved out to a minute, and a time at or after the check's own bound starts no look of its own, since the look at the bound already stands. " +
			"Once the issue has left the workflow, at that report the daemon stops this process and keeps this session. " +
			"Your next look comes to this session as a task naming the summary you gave as what woke it.")
	}
	text.WriteString(" Wait for nothing in this session: end the turn with the report.")
	return text.String()
}

// wokenBy is the check task's wording of a look's wake, from the cause and its detail: the merge
// that opened the check (wakeMerge, the merge commit as its detail), its bound (wakeBound, the
// check's own bound and the open of the check it is counted from, and nothing of why the looks
// before it were pending), or a check_wake row (wakeNamed), whose reason is either the summary the
// pending report gave (checkReported) or the retry of a look nobody reported (endLook). The last two
// read as one sentence after the dash, so the lead names who named the wake rather than what it says.
func wokenBy(cause, detail string) string {
	switch cause {
	case wakeMerge:
		return "the merge"
	case wakeBound:
		return "the bound: " + detail
	case wakeNamed:
		return "the wake named at the last look — " + detail
	default:
		return cause + " " + detail
	}
}

// boundText is the check's bound as the bound's look is told it: whole days or hours as such, any
// other duration as Go prints it.
func boundText(bound time.Duration) string {
	switch {
	case bound > 0 && bound%(24*time.Hour) == 0:
		return fmt.Sprintf("%d days", bound/(24*time.Hour))
	case bound > 0 && bound%time.Hour == 0:
		return fmt.Sprintf("%d hours", bound/time.Hour)
	default:
		return bound.String()
	}
}

// checkReported records the checking agent's report on a look (intake.CheckReport) of the check
// the report names (its OpenedAt; none names the issue's newest). A report of no such check, or
// of one closed, is refused (CHECK_NOT_OPEN), and so is one naming a look that is not the check's
// current one (CHECK_LOOK_NOT_OPEN: the report is of an earlier look) or carrying no
// verdict of pass, pending or fail (CHECK_VERDICT_REQUIRED). The look ends;
// pass closes the check as recorded and fail as a defect (closeCheck), and pending ends the look
// as one with nothing to show (endLookPending), named for the next look at the time the report
// carries (intake.CheckReport.WakeAt, which the route holds a pending to), clamped into the
// record with the look's close, and the summary as its reason, which the route requires of every
// report (api.handoffComplete, before it reaches its checkReport). A pending that names no time
// is woken as a look nobody reported is, a look bound out, so the check is never left with no row
// to wake it. A report of any verdict ends the role's run of failed looks
// (record.ProductionCheck.FailedLooks): the role came up, so a later failure of its claim is named
// a look bound out again (failedLookRetry).
//
// How the current look ended does not matter: a report of it counts while the check is open,
// however that look ended — its bound came due, an operator's hold met its start, or the
// checking role's claim failed in it (dispatch://INFRA-714/spec R12). The checking agent drove
// the change in production and its answer is the fact; refusing it threw that away and left the
// check to close as not run. A report refused here leaves its event unmarked, so the agent's next
// send of it applies (intake's ApplyFact).
func (e *Engine) checkReported(ctx context.Context, tx pgx.Tx, fact intake.CheckReport) (intake.Result, error) {
	check, err := e.store.ProductionCheckOf(ctx, tx, fact.Issue, fact.OpenedAt)
	if err != nil {
		return intake.Result{}, err
	}
	if check == nil || !check.Open() {
		return refused("CHECK_NOT_OPEN", fmt.Sprintf("%s has no open production check; this report changed nothing", fact.Issue)), nil
	}
	if fact.Look != check.Looks {
		return refused("CHECK_LOOK_NOT_OPEN", fmt.Sprintf("look %d of %s's production check is not the look to report: the check is at look %d; this report changed nothing",
			fact.Look, fact.Issue, check.Looks)), nil
	}
	var outcome record.CheckOutcome
	switch fact.Verdict {
	case "pass":
		outcome = record.CheckRecorded
	case "fail":
		outcome = record.CheckDefect
	case "pending":
	default:
		return refused("CHECK_VERDICT_REQUIRED", fmt.Sprintf("a production check report names a verdict of pass, pending or fail, not %q; this report changed nothing", fact.Verdict)), nil
	}
	check.LookOpen, check.FailedLooks = false, 0
	if outcome == "" {
		var wake time.Time
		if fact.WakeAt == nil {
			wake = e.lookRetry()
		} else {
			wake = *fact.WakeAt
		}
		check.WakeAt = e.clampWake(*check, wake)
	}
	if err := e.store.PutProductionCheck(ctx, tx, *check); err != nil {
		return intake.Result{}, err
	}
	e.logOnCommit(ctx, "workflow: production check look reported", "issue", check.Issue, "mergeCommit", check.MergeSHA, "look", check.Looks,
		"cause", check.LastWakeKind, "verdict", fact.Verdict)
	if outcome == "" {
		return intake.Result{}, e.endLookPending(ctx, tx, check, fact.Summary)
	}
	return intake.Result{}, e.closeCheck(ctx, tx, check, outcome)
}

// endLook ends the open look of a check nobody reported — its bound came due, the outbox met an
// operator's hold on its start (checkLookEnded, which names the next look a look bound out,
// lookRetry), or its checking role's claim failed in it (endFailedLook) — as a report of pending
// would (endLookPending), and names the next look at at, clamped (clampWake), the retry with cause
// as its reason. cause is why, for the log too.
func (e *Engine) endLook(ctx context.Context, tx pgx.Tx, check *record.ProductionCheck, cause string, at time.Time) error {
	check.LookOpen = false
	check.WakeAt = e.clampWake(*check, at)
	if err := e.store.PutProductionCheck(ctx, tx, *check); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: production check look ended without a report", "issue", check.Issue, "mergeCommit", check.MergeSHA,
		"look", check.Looks, "cause", cause, "failedLooks", check.FailedLooks, "wake", check.WakeAt)
	return e.endLookPending(ctx, tx, check, "the retry of a look nobody reported: "+cause)
}

// endFailedLook ends the open look whose checking role's claim failed in it (endLook) and counts
// it a failed look (record.ProductionCheck.FailedLooks), so the next look is named the backoff out
// (failedLookRetry) rather than a look bound: the circuit breaker's open state, after which that
// look retries the claim with one attempt left of the budget its failure spent (the daemon's
// outbox, supervise.RequestRetry.OneAttemptLeft).
func (e *Engine) endFailedLook(ctx context.Context, tx pgx.Tx, check *record.ProductionCheck) error {
	check.FailedLooks++
	return e.endLook(ctx, tx, check, failedLookCause(check.FailedLooks), e.failedLookRetry(*check))
}

// failedLookCause is the cause a look whose checking role's claim failed ends with, the failedth
// such look in a row.
func failedLookCause(failed int) string {
	if failed <= 1 {
		return "claim failed"
	}
	return fmt.Sprintf("claim failed, %d looks in a row", failed)
}

// failedLookRetry is when the look after a failed one is due (record.ProductionCheck.FailedLooks,
// the looks in a row whose checking role's claim failed): a look bound from now for the first,
// doubled for each failed look in a row before it — 2 h, 4 h, 8 h and so on — and back to a look
// bound once a look is reported (checkReported). The doubling stops once the wait reaches the
// check's bound, which clampWake then turns into no wake at all, since the bound's own row starts
// the last look. It is the half-open circuit breaker's growing wait: a role that cannot start is
// tried once at each of these looks rather than at every look bound until the check's bound.
func (e *Engine) failedLookRetry(check record.ProductionCheck) time.Time {
	wait := CheckLookBound
	for failed := 1; failed < check.FailedLooks && e.now().Add(wait).Before(check.BoundAt); failed++ {
		wait *= 2
	}
	return e.now().Add(wait)
}

// failedBetweenLooks counts a failure of the checking role's claim after its look ended, with no
// look open, as a failed look (record.ProductionCheck.FailedLooks): the look before it ended at its
// bound or on a report while the claim was still failing its way through a budget, and its role
// never came up. The next look is then named the backoff out from this failure (failedLookRetry),
// as a failure inside a look names it, unless the wake standing falls later still — a pending
// report's own time — which stays, so the failure only ever puts the next look later. A moved wake
// is a new check_wake row (nameWake), and the row the look's end queued comes due to find the
// check no longer records its wake and starts nothing (checkWakeDue). A row a release before
// check_wake rows carried their wake queued names none and cannot be fenced so, and would start the
// next look at the time the moved wake replaced: it is dropped with the move
// (record.Store.DropUnfencedCheckWakes). No wake standing stays no
// wake: the bound's own row starts the last look. The look the wake starts retries the claim with
// one attempt left (the daemon's outbox), so the next failure is named a longer backoff out.
func (e *Engine) failedBetweenLooks(ctx context.Context, tx pgx.Tx, check *record.ProductionCheck) error {
	check.FailedLooks++
	moved := false
	if at := e.failedLookRetry(*check); check.WakeAt != nil && at.After(*check.WakeAt) {
		check.WakeAt, moved = e.clampWake(*check, at), true
	}
	if err := e.store.PutProductionCheck(ctx, tx, *check); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: production check role's claim failed between looks", "issue", check.Issue, "mergeCommit", check.MergeSHA,
		"look", check.Looks, "role", check.Role, "failedLooks", check.FailedLooks, "wake", check.WakeAt, "moved", moved)
	if !moved {
		return nil
	}
	if err := e.store.DropUnfencedCheckWakes(ctx, tx, check.Issue); err != nil {
		return err
	}
	return e.nameWake(ctx, tx, check, "the retry of a look nobody reported: "+failedLookCause(check.FailedLooks)+" after the look ended")
}

// checkRoleFailedReason is what a worker-died of the checking role's failed claim says after the
// look it names (claimFailed): how the claim ended — the supervisor's sentence, then the pod's end
// or the first line of the runtime's account of the death (deathReason) — the cause word the
// failure carries, and what the check does next, from check as the failure left it. That is the
// look that retries the claim and when, or the check's bound where no wake stands before it, and
// whether that retry has one attempt left of the budget the failure spent (the daemon's outbox,
// supervise.RequestRetry.OneAttemptLeft) or starts a fresh run, the failure having spent none; or,
// once the check's last look lost its role, that the check closed as not run. The count of failed
// looks in a row closes it, which is what the backoff doubles by (failedLookRetry). A claim that
// failed on OpenAI's cyber-policy refusal is retried by no look, since no start of the daemon's
// own launches its agent again (supervise.ErrHeldForCyberPolicy), so its notice says only that
// it does not start again by itself (noStartOfItsOwn) where the look would be named.
func checkRoleFailedReason(fact intake.ClaimFailed, check record.ProductionCheck) string {
	said := deathReason(fact, false)
	if said == "" {
		said = fmt.Sprintf("the %s's claim failed before it reported the look", check.Role)
	}
	if fact.Cause != "" {
		said += "; its cause is " + string(fact.Cause)
	}
	if !check.Open() {
		return said + "; that was the check's last look, so the check closed as " + string(check.Outcome)
	}
	if clause := noStartOfItsOwn(fact); clause != "" {
		return fmt.Sprintf("%s; %s (failed looks in a row: %d)", said, clause, check.FailedLooks)
	}
	how := "with a fresh run, since its failure spent no budget"
	if fact.Budgets.Spent(fact.Limits) {
		how = "with one attempt left of the budget its failure spent"
	}
	var next string
	switch {
	case check.WakeAt != nil:
		next = fmt.Sprintf("look %d retries the claim at %s %s", check.Looks+1, check.WakeAt.UTC().Format(time.RFC3339), how)
	default:
		next = fmt.Sprintf("the check's last look retries the claim at its bound, %s, %s", check.BoundAt.UTC().Format(time.RFC3339), how)
	}
	return fmt.Sprintf("%s; %s (failed looks in a row: %d)", said, next, check.FailedLooks)
}

// endLookPending is what follows a look that ended pending, reported so or with no report
// (endLook). A look before the last pends a merge production does not carry yet, or carries with a
// requirement only the deploy can show still unobserved. The last look's task gives pending only
// for an issue with no such requirement. The last look, reported pending or not reported, closes
// the check as not run. Every other look names when the check is next looked at (nameWake: the
// clamped wake the record already carries, with reason as what wakes it), which is the one thing
// that starts the look after this one — nothing else reads production any more. An issue in the
// workflow writes that row and suspends nobody, since the claims and the pod are its run's: a check
// of the run that holds the issue, opened at the merge of one of its tasks while its other tasks go
// on, looks again at that wake; a check of an earlier run — a root reopened inside its linger, whose
// implementer now works the new run — waits, its wake row deferring itself while the later run
// holds the issue (checkWakeDue), and closes with that run's next merge (openCheck) or at its bound
// (checkBound).
// For an issue out of the workflow the claim of the check's role is then suspended, its session
// kept, and the issue's Sandbox is deleted with the pod it ran in (record.IssueSuspend): both rows
// follow the look's start row, so the suspension's fences find no later start and wait for the
// suspend (store.IssueSuspension), and the pod goes until the next wake, whose launch makes the
// Sandbox again.
func (e *Engine) endLookPending(ctx context.Context, tx pgx.Tx, check *record.ProductionCheck, reason string) error {
	if check.Last {
		return e.closeCheck(ctx, tx, check, record.CheckNotRun)
	}
	if err := e.nameWake(ctx, tx, check, reason); err != nil {
		return err
	}
	issue, root, err := e.checkIssue(ctx, tx, *check)
	if err != nil {
		return err
	}
	if record.InWorkflow(issue) {
		e.logOnCommit(ctx, "workflow: production check waits while its issue is in the workflow; nobody is suspended", "issue", issue.Key, "mergeCommit", check.MergeSHA,
			"look", check.Looks, "phase", issue.Phase)
		return nil
	}
	token, err := e.checkClaim(ctx, tx, *check)
	if err != nil {
		return err
	}
	if err := e.enqueue(ctx, tx, issue.Key, record.SuperviseRequest{Op: "suspend", Tree: issue.Tree, Role: check.Role, Claim: token,
		Generation: issue.Generation, Reason: fmt.Sprintf("%s's production check waits for its next look", issue.Key)}); err != nil {
		return err
	}
	if err := e.enqueue(ctx, tx, issue.Key, record.IssueSuspend{Tree: issue.Tree, Generation: issue.Generation, TreeGeneration: root.Generation}); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: production check waits", "issue", issue.Key, "mergeCommit", check.MergeSHA, "look", check.Looks)
	return nil
}

// clampWake is when check's next look is due from a wake named at at, and nil for one that names
// no row at all. Three rules, and one helper for them so that no caller can hold a different set:
// a time less than CheckWakeFloor after now is moved out to that floor, a time at or past the
// check's own bound names nothing, since the check's bound row already stands and starts its last
// look, and the time is cut to the microsecond, Postgres's timestamptz precision, so the wake the
// record returns on every later read equals the one its check_wake row carries (nameWake,
// checkWakeDue).
func (e *Engine) clampWake(check record.ProductionCheck, at time.Time) *time.Time {
	if floor := e.now().Add(CheckWakeFloor); at.Before(floor) {
		at = floor
	}
	if !at.Before(check.BoundAt) {
		return nil
	}
	at = at.Truncate(time.Microsecond)
	return &at
}

// lookRetry is a look bound out from now: when a check is looked at again after a look nobody
// reported, and when a pending report that named no time of its own is looked at next.
func (e *Engine) lookRetry() time.Time { return e.now().Add(CheckLookBound) }

// nameWake queues the one row that starts check's next look: a check_wake row
// (record.CheckWakeDue) due at the clamped moment the record already carries (clampWake, written
// with the look's close), carrying the look the wake was named at, the check instance it was
// named for, that moment, and reason, which the look it starts is woken by. A wake at or past the
// check's bound is no wake: WakeAt is unset and no row is written, so the record holds whichever
// row stands and nothing else.
func (e *Engine) nameWake(ctx context.Context, tx pgx.Tx, check *record.ProductionCheck, reason string) error {
	if check.WakeAt == nil {
		e.logOnCommit(ctx, "workflow: production check names no wake: it falls at or past the check's bound", "issue", check.Issue,
			"mergeCommit", check.MergeSHA, "look", check.Looks, "bound", check.BoundAt, "reason", reason)
		return nil
	}
	if err := e.enqueueAt(ctx, tx, check.Issue, record.CheckWakeDue{Look: check.Looks, OpenedAt: check.OpenedAt, WakeAt: *check.WakeAt, Reason: reason}, *check.WakeAt); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: production check wake named", "issue", check.Issue, "mergeCommit", check.MergeSHA, "look", check.Looks,
		"wake", *check.WakeAt, "reason", reason)
	return nil
}

// closeCheck closes the check with outcome (markCheckClosed) and ends the checking agent's part in
// it: its claim (checkClaim) is closed (issue_close: retired, its session kept), and the issue's
// Sandbox is suspended and deleted with the pod it ran in, whether the issue is a child whose tree
// runs on or a root, or a child, of a lingering tree whose close would delete it anyway. A tree
// lingering past its deadline waited for this check (lingerExpired): once no check of the tree is
// open its close is re-armed with a LingerClose row due now, a duplicate of the waiting row's
// event, so applying both is harmless. An issue in the workflow — one whose other tasks go on, or
// a root reopened inside its linger — retires nobody and suspends nothing: the claims and the pod
// are the run's.
func (e *Engine) closeCheck(ctx context.Context, tx pgx.Tx, check *record.ProductionCheck, outcome record.CheckOutcome) error {
	if err := e.markCheckClosed(ctx, tx, check, outcome); err != nil {
		return err
	}
	issue, root, err := e.checkIssue(ctx, tx, *check)
	if err != nil {
		return err
	}
	if record.InWorkflow(issue) {
		e.logOnCommit(ctx, "workflow: production check closed while its issue is in the workflow; its claims and pod are the run's", "issue", issue.Key,
			"mergeCommit", check.MergeSHA, "phase", issue.Phase)
		return nil
	}
	token, err := e.checkClaim(ctx, tx, *check)
	if err != nil {
		return err
	}
	if err := e.enqueue(ctx, tx, issue.Key, record.SuperviseRequest{Op: "issue_close", Tree: issue.Tree, Role: check.Role, Claim: token,
		Generation: issue.Generation, Reason: fmt.Sprintf("%s's production check closed as %s", issue.Key, outcome)}); err != nil {
		return err
	}
	if err := e.enqueue(ctx, tx, issue.Key, record.IssueSuspend{Tree: issue.Tree, Generation: issue.Generation, TreeGeneration: root.Generation}); err != nil {
		return err
	}
	if root.LingerUntil == nil || e.now().Before(*root.LingerUntil) {
		return nil
	}
	open, err := e.store.OpenProductionChecksOfTree(ctx, tx, root.Tree)
	if err != nil || len(open) > 0 {
		return err
	}
	e.logOnCommit(ctx, "workflow: the last open production check of a tree past its linger closed; the tree's close is re-armed", "tree", root.Key,
		"generation", root.Generation)
	return e.enqueue(ctx, tx, root.Key, record.LingerClose{Generation: root.Generation})
}

// markCheckClosed writes the check closed now with outcome, its look closed with it, and logs the
// close with the issue and the merge commit.
func (e *Engine) markCheckClosed(ctx context.Context, tx pgx.Tx, check *record.ProductionCheck, outcome record.CheckOutcome) error {
	now := e.now()
	check.ClosedAt, check.Outcome, check.LookOpen = &now, outcome, false
	if err := e.store.PutProductionCheck(ctx, tx, *check); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: production check closed", "issue", check.Issue, "mergeCommit", check.MergeSHA, "outcome", outcome, "looks", check.Looks)
	return nil
}

// checkIssue is the check's issue and its tree root as recorded.
func (e *Engine) checkIssue(ctx context.Context, tx pgx.Tx, check record.ProductionCheck) (record.Issue, record.Issue, error) {
	issue, err := e.store.Issue(ctx, tx, check.Issue)
	if err != nil {
		return record.Issue{}, record.Issue{}, err
	}
	if issue == nil {
		return record.Issue{}, record.Issue{}, fmt.Errorf("production check of %s: its issue is not recorded", check.Issue)
	}
	root, err := e.store.Issue(ctx, tx, check.Tree)
	if err != nil {
		return record.Issue{}, record.Issue{}, err
	}
	if root == nil {
		return record.Issue{}, record.Issue{}, fmt.Errorf("production check of %s: its tree root %s is not recorded", check.Issue, check.Tree)
	}
	return *issue, *root, nil
}

// checkLookEnded is a look ending with no report (intake.CheckLookEnded: the look's bound, or an
// operator's hold met by its start; a look whose checking role's claim failed is claimFailed's):
// the look it names, of the check it names (its OpenedAt; none names the issue's newest), still
// open, ends as a pending report would (endLook); a look already ended, or a check closed, changes
// nothing.
func (e *Engine) checkLookEnded(ctx context.Context, tx pgx.Tx, fact intake.CheckLookEnded) (intake.Result, error) {
	check, err := e.store.ProductionCheckOf(ctx, tx, fact.Issue, fact.OpenedAt)
	if err != nil || check == nil || !check.Open() || !check.LookOpen || check.Looks != fact.Look {
		return intake.Result{}, err
	}
	return intake.Result{}, e.endLook(ctx, tx, check, fact.Cause, e.lookRetry())
}

// checkWakeDue is a check's wake coming due (intake.CheckWakeDue, the check_wake row nameWake
// queued at ProductionCheck.WakeAt: the time the checking agent named for itself on a pending
// report, or the retry of a look nobody reported), read on the check the row names (its OpenedAt;
// none names the issue's newest). The next look starts, woken by the row's reason. Four guards
// stand before that, in this order, and only the last queues the row again:
//
//   - no such check, or one closed since: the row is spent.
//   - a row of a look the check has left, or one whose look is open already: the bound started
//     the last look, or a report or a retry started another.
//   - a row of a wake the check no longer records: a failure of the checking role's claim
//     between looks named the next look later (failedBetweenLooks), whose own row starts it. A
//     row a release before the field queued names no wake (record.CheckWakeDue.WakeAt) and is
//     not fenced so.
//   - a check of an earlier run (checkOfAnEarlierRun) — its issue is back in the workflow on a
//     later run, a root reopened inside its linger whose implementer now works the new run —
//     keeps the wake rather than dropping it: no look can run while that run holds the issue's
//     claims and its pod, so the row is queued again a look bound out (lookRetry, clamped by
//     clampWake, so no row is ever queued past the check's bound), and a check whose issue
//     re-entered the workflow and closed again on the same merge still holds its agent's wake.
//     The check's own bound closes it where the run's next merge does not (checkBound). A check
//     of the run that holds the issue looks while that run goes on.
//
// The first three change nothing and are logged with why; without them the last one's queue
// would make an orphan row immortal.
func (e *Engine) checkWakeDue(ctx context.Context, tx pgx.Tx, fact intake.CheckWakeDue) (intake.Result, error) {
	check, err := e.store.ProductionCheckOf(ctx, tx, fact.Issue, fact.OpenedAt)
	if err != nil {
		return intake.Result{}, err
	}
	var why string
	switch {
	case check == nil:
		why = "the issue has no such production check"
	case !check.Open():
		why = fmt.Sprintf("the check closed as %s", check.Outcome)
	case check.Looks != fact.Look:
		why = fmt.Sprintf("the check is at look %d", check.Looks)
	case check.LookOpen:
		why = fmt.Sprintf("look %d is open", check.Looks)
	case !fact.WakeAt.IsZero() && (check.WakeAt == nil || !check.WakeAt.Equal(fact.WakeAt)):
		why = fmt.Sprintf("the check no longer names the wake at %s", fact.WakeAt.UTC().Format(time.RFC3339))
	}
	if why != "" {
		e.logOnCommit(ctx, "workflow: production check wake starts no look", "issue", fact.Issue, "look", fact.Look,
			"reason", fact.Reason, "why", why)
		return intake.Result{}, nil
	}
	issue, _, err := e.checkIssue(ctx, tx, *check)
	if err != nil {
		return intake.Result{}, err
	}
	if checkOfAnEarlierRun(issue, *check) {
		e.logOnCommit(ctx, "workflow: production check wake deferred while its issue runs again", "issue", issue.Key,
			"mergeCommit", check.MergeSHA, "look", check.Looks, "phase", issue.Phase, "reason", fact.Reason)
		check.WakeAt = e.clampWake(*check, e.lookRetry())
		if err := e.store.PutProductionCheck(ctx, tx, *check); err != nil {
			return intake.Result{}, err
		}
		return intake.Result{}, e.nameWake(ctx, tx, check, fact.Reason)
	}
	return intake.Result{}, e.startLook(ctx, tx, issue, check, wakeNamed, fact.Reason, false)
}

// checkBound is a check's bound coming due (intake.CheckBound, the check_bound row each check's
// open queues at its bound_at, which names no check). The record decides which bounds came due:
// each open check of the issue whose last look has not started and whose BoundAt is not after now,
// oldest first; a row due before every open check's bound changes nothing, since each check's own
// row comes at its own bound. A due check with a look open waits for that look's own bound to end
// it first, and the issue gets one row at the look bound for all such checks, so the rows never
// multiply. A check of an earlier run whose issue is back in the workflow (checkOfAnEarlierRun),
// a root reopened inside its linger, is closed as not run, since no look can run while its
// implementer works the new run, and the run's own merge opens its own. Otherwise, a check of the
// run that holds the issue among them, the check's last look starts, told that it is the last and the
// check's own bound (BoundAt less OpenedAt, which a later change of the project's bound leaves
// alone) counted from the check's open: the merge of its task, or the issue's leave for the merge
// that ended the issue, which comes after the merge where that merge waited on the issues beneath
// it (closeMerged). It is told nothing of why the looks before it were pending, which the daemon
// does not know: production not carrying the merge yet, or a merge it carries with a requirement
// only the deploy can show still unobserved.
func (e *Engine) checkBound(ctx context.Context, tx pgx.Tx, fact intake.CheckBound) (intake.Result, error) {
	checks, err := e.store.ProductionChecksOf(ctx, tx, fact.Issue)
	if err != nil {
		return intake.Result{}, err
	}
	now := e.now()
	var due []*record.ProductionCheck
	for i := len(checks) - 1; i >= 0; i-- {
		if check := &checks[i]; check.Open() && !check.Last && !now.Before(check.BoundAt) {
			due = append(due, check)
		}
	}
	if len(due) == 0 {
		e.logOnCommit(ctx, "workflow: a production check bound came due before any open check's own; nothing to do", "issue", fact.Issue)
		return intake.Result{}, nil
	}
	issue, _, err := e.checkIssue(ctx, tx, *due[0])
	if err != nil {
		return intake.Result{}, err
	}
	deferred := false
	for _, check := range due {
		switch {
		case check.LookOpen:
			e.logOnCommit(ctx, "workflow: production check bound deferred while a look is open", "issue", check.Issue, "mergeCommit", check.MergeSHA, "look", check.Looks)
			deferred = true
		case checkOfAnEarlierRun(issue, *check):
			e.logOnCommit(ctx, "workflow: production check bound came due while its issue runs again", "issue", issue.Key, "mergeCommit", check.MergeSHA, "phase", issue.Phase)
			if err := e.closeCheck(ctx, tx, check, record.CheckNotRun); err != nil {
				return intake.Result{}, err
			}
		default:
			if err := e.startLook(ctx, tx, issue, check, wakeBound,
				fmt.Sprintf("%s since the check opened", boundText(check.BoundAt.Sub(check.OpenedAt))), true); err != nil {
				return intake.Result{}, err
			}
		}
	}
	if !deferred {
		return intake.Result{}, nil
	}
	return intake.Result{}, e.enqueueAt(ctx, tx, fact.Issue, record.CheckBound{}, now.Add(CheckLookBound))
}

// checkOfAnEarlierRun reports whether check belongs to a run of issue that is over: the issue is
// back in the workflow (record.InWorkflow) on a later run than the one whose merge opened the
// check (record.ProductionCheck.Generation). Such a check waits — its wake is queued again and its
// bound closes it as not run — since that later run holds the issue's claims and its pod. A check
// of the run that holds the issue, opened at a merge of one of its tasks while its other tasks go
// on, looks as a check of an issue that left the workflow does. A check recorded before checks
// carried their run reads run 0, so it is of an earlier run of any issue back in the workflow, as
// every check of such an issue was before.
func checkOfAnEarlierRun(issue record.Issue, check record.ProductionCheck) bool {
	return record.InWorkflow(issue) && issue.Generation > check.Generation
}
