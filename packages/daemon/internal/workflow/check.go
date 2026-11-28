package workflow

import (
	"context"
	"crypto/rand"
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

// openCheck opens issue's production check on its merged pull request pr and starts its first
// look, woken by the merge: the record (record.ProductionCheck, bound at the project's
// ProductionCheckBound from now, carrying the role that works it and the generation the issue
// holds, record.ProductionCheck.OpenedGeneration, which tells a later leave whether the issue ran
// again since: ranAgain), the check_bound row due at that bound, then the look (startLook). A
// project whose `production_check` is off has no role to work a check (checkRole), and opens
// nothing at all: no record, no bound row and no look, only the line saying the close had nothing
// to check.
// wake is only how the open is logged: a lingering tree's member has its claims suspended,
// so the look's start row is a wake of the checking role's claim, where a running tree's is a task
// handed to that role's live session; the rows are the same, since the start resumes either. One check
// per issue: a second merge of the same issue — a rerun's, whose check outlived the re-entry
// (ReenterChild) — replaces the earlier record, whose bound row the new check then defers to its
// own bound (checkBound). An earlier check still open closes first, as not run (markCheckClosed:
// its look's task ends with it, and no claim is closed and nothing suspended, since the merge's
// own close decides those and the new check's look starts the same role), and its close's line
// names the merge that replaced it, written before the line of the new check's open. It closes so
// in a project that checks nothing too, since the merge it waited for is superseded either way.
// The same merge seen again — a done child a person parks and sets done again, each move a leave —
// opens nothing and closes nothing, since the check it has, open or recorded, is that merge's
// (checksMerge). The check's OpenedAt is its identity (record.ProductionCheck.ID), so it is the
// clock truncated to the microsecond, Postgres's timestamptz precision: the value the rows and
// facts of this transaction carry equals the one every later read of the record returns.
func (e *Engine) openCheck(ctx context.Context, tx pgx.Tx, issue record.Issue, pr record.PullRequest, wake bool) error {
	existing, err := e.store.ProductionCheck(ctx, tx, issue.Key)
	if err != nil {
		return err
	}
	same := existing != nil && e.checksMerge(*existing, pr)
	if existing != nil && existing.Open() && !same {
		if err := e.markCheckClosed(ctx, tx, existing, record.CheckNotRun, pr.MergeSHA); err != nil {
			return err
		}
	}
	role := e.checkRole()
	if role == "" {
		e.logOnCommit(ctx, "workflow: no production check: this project checks nothing in production", "issue", issue.Key,
			"mergeCommit", pr.MergeSHA, "pullRequest", pr.Number)
		return nil
	}
	if same {
		state := "open"
		if !existing.Open() {
			state = "closed as " + string(existing.Outcome)
		}
		e.logOnCommit(ctx, "workflow: production check not opened again: the issue's check is this merge's", "issue", issue.Key,
			"mergeCommit", pr.MergeSHA, "pullRequest", pr.Number, "check", existing.OpenedAt, "state", state)
		return nil
	}
	now := e.now().Truncate(time.Microsecond)
	check := record.ProductionCheck{
		Issue: issue.Key, Project: issue.Project, Tree: issue.Tree, Repo: pr.Repo, Number: pr.Number, MergeSHA: pr.MergeSHA,
		OpenedAt: now, BoundAt: now.Add(e.cfg.ProductionCheckBound), Role: role, OpenedGeneration: issue.Generation,
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

// checksMerge says whether check is the check of pr's merge: the same pull request of the same
// repository, at the same merge commit where both name one. A check opened before the merge commit
// was recorded names none, and is that pull request's merge's.
func (e *Engine) checksMerge(check record.ProductionCheck, pr record.PullRequest) bool {
	return e.sameRepository(check.Repo, pr.Repo) && check.Number == pr.Number &&
		(check.MergeSHA == "" || pr.MergeSHA == "" || check.MergeSHA == pr.MergeSHA)
}

// ranAgain says whether issue ran again since check opened: its generation is above the one the
// check opened at (record.ProductionCheck.OpenedGeneration). A child's re-entry raises it
// (ReenterChild), and so does a root's re-admission (admit's readmit), while neither close_root
// nor a transition changes it, and a root's re-admission leaves the generation of every member of
// its tree as it was, a done one included. So it is read from the generations rather than from the
// phase a leave sees (close_root and transition move the issue out of the workflow before they
// call leave) or from the pull request (a root's re-admission deletes the merged pull request of
// every member of its tree, record.Store.ClearTreeGeneration). A check with no generation, one an
// older binary opened, reads as not run again; the boot gives each open check its generation
// before any leave reads it.
func ranAgain(issue record.Issue, check record.ProductionCheck) bool {
	return check.OpenedGeneration != 0 && issue.Generation > check.OpenedGeneration
}

// startLook starts the next look of issue's check, woken by cause with detail, last marking the
// bound's look: the record counts the look open from now with that wake and no wake standing —
// the row this look starts from is spent, and only a pending report names the next one — and
// holds the look's own look id (giveLookID). The check's role (record.ProductionCheck.Role, the
// role the check opened with, never the setting as it stands now) is started on the check task
// (lookStart), the one text that gives the role that id, and a check_look row due at the look
// bound, naming the check and the look, ends the look should nobody report it. The log line names
// the look by its number alone: no line carries the id or the task (R3 of dispatch://INFRA-3518).
func (e *Engine) startLook(ctx context.Context, tx pgx.Tx, issue record.Issue, check *record.ProductionCheck, cause, detail string, last bool) error {
	now := e.now()
	check.Looks++
	check.LookOpen, check.LookWakeAt = true, &now
	check.LastWakeKind, check.LastWakeDetail, check.LastWakeAt = cause, detail, &now
	check.WakeAt, check.Last = nil, last
	giveLookID(check)
	if err := e.store.PutProductionCheck(ctx, tx, *check); err != nil {
		return err
	}
	if err := e.enqueue(ctx, tx, issue.Key, lookStart(issue, *check)); err != nil {
		return err
	}
	if err := e.enqueueAt(ctx, tx, issue.Key, record.CheckLook{Look: check.Looks, OpenedAt: check.OpenedAt}, now.Add(CheckLookBound)); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: production check look started", "issue", issue.Key, "mergeCommit", check.MergeSHA, "look", check.Looks,
		"role", check.Role, "cause", cause, "detail", detail, "last", last)
	return nil
}

// giveLookID gives check's current look a look id of its own (record.ProductionCheck.LookID:
// crypto/rand's Text, 26 base32 characters, 130 bits, made afresh for each look and never derived
// from the check's OpenedAt, which every state shows) with the look and the check instance it was
// made for (LookIDLook, LookIDOpenedAt), so CurrentLookID names it. startLook gives each look it
// starts one, and the boot gives a new one to each open look that RepairOpenChecks names.
func giveLookID(check *record.ProductionCheck) {
	check.LookID, check.LookIDLook, check.LookIDOpenedAt = rand.Text(), check.Looks, check.OpenedAt
}

// lookStart is the start row of check's current look: a start of the check's role with no phase,
// fenced on the check, its look and the look's id (record.SuperviseRequest.CheckLook,
// CheckOpenedAt and LookID) rather than on the generation, stamped with the generation the issue
// holds as the delivery's run, and carrying the check task of that look from the record's last
// wake (LastWakeKind, LastWakeDetail) and its last mark (Last). startLook queues it for each look
// it starts, and the boot queues it, marked LookReissue, as the new task of an open look it gives
// an id (RepairOpenChecks).
func lookStart(issue record.Issue, check record.ProductionCheck) record.SuperviseRequest {
	return record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: check.Role,
		Generation: issue.Generation, CheckLook: check.Looks, CheckOpenedAt: check.OpenedAt, LookID: check.LookID,
		Task: checkTask(issue, check, check.LookID, check.LastWakeKind, check.LastWakeDetail, check.Last)}
}

// checkTaskLead opens every check task: a resumed session tells the check from its last phase
// task by this line (the phase tasks open with their `Working copy:` line), and a test pins it.
const checkTaskLead = "Production check of "

// checkTask is what the check's role is told at a look of the closed issue's production check. The
// session it reaches may be fresh — a woken pod after a lost volume — so the text stands on its
// own: the issue, the repository, the pull request, the merge commit, the look and what woke it
// (wokenBy), the check's OpenedAt in the form the report sends it back as check_opened_at,
// whether it is the last, then on a line of its own the look's id (lookID, the id startLook made
// for this look: record.ProductionCheck.LookID) and that the report gives check_opened_at and
// look_id from this task (intake.LookIDLine), and the rules of the check in prose: each
// requirement only the deploy can show observed as it names it, the three verdicts and what each
// asks of the pull request, no commit, the wake a pending names for its own next look and the
// suspension that follows it, and nothing to wait for in the session, since the look ends with
// the report. A merge production carries with such a requirement still unobserved is a pending
// while a look follows.
// A requirement observed as not met is a fail on every look. On the last look one still unobserved
// is a fail too, whether or not production carries the merge. The last look's task gives pending
// only for an issue with none of those requirements, a pending that closes the check as not run.
// The fail clause says so once, the same on every look, in terms of the look that follows.
// For an issue whose spec is a view on an ancestor's requirements, the line after the first says
// so (viewSentence), as every phase task of the issue does. This text is the one place the look
// id reaches the role (R3 of dispatch://INFRA-3518): the id is a secret, so no log line carries
// the text.
func checkTask(issue record.Issue, check record.ProductionCheck, lookID, cause, detail string, last bool) string {
	merge := check.MergeSHA
	if merge == "" {
		merge = "unknown, read it from the pull request on GitHub"
	}
	var text strings.Builder
	fmt.Fprintf(&text, "%s%s: %s.\n", checkTaskLead, issue.Key, issue.Title)
	if view := viewSentence(issue); view != "" {
		text.WriteString(view + "\n")
	}
	fmt.Fprintf(&text, "Repository %s, pull request #%d, merge commit %s. Look %d, woken by %s.", check.Repo, check.Number, merge, check.Looks, wokenBy(cause, detail))
	openedAt := check.OpenedAt.UTC().Format(time.RFC3339Nano)
	fmt.Fprintf(&text, " The check opened at `%s`: your report names it with check_opened_at `%s`.",
		openedAt, openedAt)
	if last {
		text.WriteString(" This is the last look: no look follows.")
	}
	text.WriteByte('\n')
	text.WriteString(intake.LookIDLine(lookID))
	text.WriteString("\nDo what the deployment instructions say to reach production and read whether production carries the merge. " +
		"Where it does, drive the changed path and observe each requirement of the issue that only the deploy can show, read from the spec version that bound the review, each as the requirement names it: observing something else is never that requirement observed. " +
		"Then report with the legion tool's handoff_complete, check_opened_at and look_id from this task and a verdict, committing nothing: " +
		"pass only once you drove the changed path in production and observed every one of those requirements, and recorded the issue's after_merge check with dispatch result at the merge commit, its observed naming each requirement and what you saw of it and what woke this look, and put the line that command prints in the pull request body; " +
		"fail for a defect, never an issue you file (skill://legion-worker/references/findings.md), and a requirement observed as not met, or still unobserved when no look follows this one, whether or not production carries the merge, is a defect: a defect with a clear fix goes to the controller of the project that repository maps to, or to your own project's controller, the one your Legion addressing line names, when Dispatch maps it to none: a dispatch comment on that controller's report issue with --mention role:<that controller's role token>, and that controller files it; a defect that needs a person's decision, a deploy window or work much larger than your own, and any other finding that needs a person's authority, taste or risk appetite, is a dispatch ask, whose reference you then publish to the controller topic so that the controller follows the ask and files what the answer asks for (skill://legion-worker/references/findings.md, Tell the controller about the ask). Either names this issue, the pull request, the merge commit, what you drove and what failed; record the after_merge check as a fail with dispatch result at the merge commit, and name the comment or the ask in its observed, in the line you put in the pull request body and in the summary.")
	if last {
		text.WriteString(" Report pending only for an issue with none of those requirements, the summary naming what you could not drive, with no result recorded. This last look's pending carries no wake_at and closes the check as not run: " +
			"before reporting it, write on the pull request why the check did not run within the bound.")
	} else {
		text.WriteString(" Report pending when production does not carry the merge yet, or carries it with one of those requirements still unobserved and a look still to follow, the summary naming what is missing and what you will look for next, with no result recorded and the pull request body saying the check is still open on the merge commit. A pending report also carries wake_at, an RFC 3339 time: when you want your next look. " +
			"Besides it, only the look at the check's bound and the retry of a look nobody reported start one. A time less than a minute ahead is moved out to a minute, and a time at or after the check's own bound starts no look of its own, since the look at the bound already stands. " +
			"At that report the daemon stops this process and keeps this session, and your next look resumes it with a task naming the summary you gave as what woke it.")
	}
	text.WriteString(" Wait for nothing in this session: end the turn with the report.")
	return text.String()
}

// wokenBy is the check task's wording of a look's wake, from the cause and its detail: the merge
// that opened the check (wakeMerge, the merge commit as its detail), its bound (wakeBound, the
// check's own bound and the issue's close it is counted from, and nothing of why the looks before
// it were pending), or a check_wake row (wakeNamed), whose reason is either the summary the pending
// report gave (checkReported) or the retry of a look nobody reported (endLook). The last two read
// as one sentence after the dash, so the lead names who named the wake rather than what it says.
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

// checkReported records the checking agent's report on a look (intake.CheckReport). A report on an
// issue with no open check is refused (CHECK_NOT_OPEN), and so is one that names a check instance
// a later merge replaced (CHECK_NOT_OPEN: record.ProductionCheck.NamedBy, the fact's OpenedAt), one
// naming a look that is not the check's current one (CHECK_LOOK_NOT_OPEN: the report is of an
// earlier look), and one carrying no verdict of pass, pending or fail (CHECK_VERDICT_REQUIRED).
// The route reads the check, holds the report's look id to the current look and sends that look
// and the check's OpenedAt (api's checkReport), so the refusals of a replaced check and of an
// earlier look answer a report that a later merge or a new look overtook between the route's read
// and this transaction: the report of the replaced check or of the ended look is never recorded
// on the check or the look after it. Each says that a report gives check_opened_at and look_id
// from the task of its look, in the route's own words for that rule (intake.LookIDRule), and
// neither holds a look id. The route always sets the fact's OpenedAt; a zero OpenedAt is not
// fenced (record.ProductionCheck.NamedBy, intake.CheckReport.OpenedAt): an older daemon applied
// its own reports, which carried none, in the transaction of their request, so no such fact waits
// to be applied here.
//
// The look ends: pass closes the check as recorded and fail as a defect (closeCheck), and pending
// ends the look as one with nothing to show (endLookPending), named for the next look at the time
// the report carries (intake.CheckReport.WakeAt, which the route holds a pending to), clamped
// into the record with the look's close, and the summary as its reason, which the route requires
// of every report (api.handoffComplete, before it reaches its checkReport). A pending that names
// no time is woken as a look nobody reported is, a look bound out, so the check is never left with
// no row to wake it. A report of any verdict ends the role's run of failed looks
// (record.ProductionCheck.FailedLooks): the role came up, so a later failure of its claim is named
// a look bound out again (failedLookRetry). The report ends the look's task too, as served: its
// retirement is queued marked reported (retireLook), and a pass or a fail queues the close's own
// beside it (markCheckClosed), which leaves the claim as the reported one does in either order.
//
// How the current look ended does not matter: a report of it counts while the check is open,
// however that look ended — its bound came due, a hold of the operator's or the controller's met
// its start, or the checking role's claim failed in it (the phase-workflow spec's rule R12). The
// checking agent drove the change in production and its answer is the fact; refusing it threw that
// away and left the check to close as not run. A report refused here leaves its event unmarked, so
// the agent's next send of it applies (intake's ApplyFact).
func (e *Engine) checkReported(ctx context.Context, tx pgx.Tx, fact intake.CheckReport) (intake.Result, error) {
	check, err := e.store.ProductionCheck(ctx, tx, fact.Issue)
	if err != nil {
		return intake.Result{}, err
	}
	if check == nil || !check.Open() {
		return refused("CHECK_NOT_OPEN", fmt.Sprintf("%s has no open production check; this report changed nothing", fact.Issue)), nil
	}
	if !check.NamedBy(fact.OpenedAt) {
		return refused("CHECK_NOT_OPEN", fmt.Sprintf("This report names the production check of %s that opened at %s. A later merge replaced that check with the check that opened at %s. The current look of that check is look %d. This report changed nothing. %s",
			fact.Issue, fact.OpenedAt.UTC().Format(time.RFC3339Nano), check.OpenedAt.UTC().Format(time.RFC3339Nano), check.Looks, intake.LookIDRule)), nil
	}
	if fact.Look != check.Looks {
		return refused("CHECK_LOOK_NOT_OPEN", fmt.Sprintf("Look %d of %s's production check is not the current look. The current look of the check is look %d. This report changed nothing. %s",
			fact.Look, fact.Issue, check.Looks, intake.LookIDRule)), nil
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
	if err := e.retireLook(ctx, tx, *check, true); err != nil {
		return intake.Result{}, err
	}
	e.logOnCommit(ctx, "workflow: production check look reported", "issue", check.Issue, "mergeCommit", check.MergeSHA, "look", check.Looks,
		"cause", check.LastWakeKind, "verdict", fact.Verdict)
	if outcome == "" {
		return intake.Result{}, e.endLookPending(ctx, tx, check, fact.Summary)
	}
	return intake.Result{}, e.closeCheck(ctx, tx, check, outcome)
}

// endLook ends the open look of a check nobody reported — its bound came due, the outbox met a
// suspension of the operator's or the controller's held over its start (checkLookEnded, which
// names the next look a look bound out, lookRetry), or its checking role's claim failed in it
// (endFailedLook) — as a report of pending would (endLookPending), and names the next look at at,
// clamped (clampWake), the retry with cause as its reason. cause is why, for the log too. The
// look's task ends with the look, unreported (retireLook), so no send, ready or resume of the
// checking role's claim carries it after this. The last look's end closes the check
// (endLookPending), whose close queues that same row (markCheckClosed), so endLook queues it only
// for a look before the last.
func (e *Engine) endLook(ctx context.Context, tx pgx.Tx, check *record.ProductionCheck, cause string, at time.Time) error {
	check.LookOpen = false
	check.WakeAt = e.clampWake(*check, at)
	if err := e.store.PutProductionCheck(ctx, tx, *check); err != nil {
		return err
	}
	if !check.Last {
		if err := e.retireLook(ctx, tx, *check, false); err != nil {
			return err
		}
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
// that starts the look after this one — nothing else reads production any more. An issue back in
// the workflow — a root reopened inside its linger, whose implementer now works the new run, or a
// done child set back to todo (ReenterChild) — writes that row and suspends nobody: the check
// waits, its wake row defers itself while the run holds the issue (checkWakeDue), and the check
// closes with the run's next merge (openCheck), at the run's leave with no merge (leave), or at
// its bound (checkBound).
// The claim of the check's role is then suspended, its session kept, and the issue's Sandbox is
// deleted with the pod it ran in (record.IssueSuspend): both rows follow the look's start row, so
// the suspension's fences find no later start and wait for the suspend (store.IssueSuspension),
// and the pod goes until the next wake, whose launch makes the Sandbox again.
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
		e.logOnCommit(ctx, "workflow: production check waits while its issue runs again", "issue", issue.Key, "mergeCommit", check.MergeSHA,
			"look", check.Looks, "phase", issue.Phase)
		return nil
	}
	if err := e.suspend(ctx, tx, issue, check.Role, fmt.Sprintf("%s's production check waits for its next look", issue.Key)); err != nil {
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

// closeCheck closes the check with outcome (markCheckClosed) and ends the checking role's part in it:
// its claim is closed (issue_close: retired, its session kept), and the issue's Sandbox is
// suspended and deleted with the pod it ran in, whether the issue is a child whose tree runs on or
// a root, or a child, of a lingering tree whose close would delete it anyway. A tree lingering past
// its deadline waited for this check (lingerExpired): once no check of the tree its close waits on
// is open (checksTheLingerWaitsOn) its close is re-armed with a LingerClose row due now, a
// duplicate of the waiting row's event, so applying both is harmless. An issue back in the
// workflow — a root reopened inside its linger, or a re-entered child — retires nobody and
// suspends nothing: the claims and the pod are the new run's.
func (e *Engine) closeCheck(ctx context.Context, tx pgx.Tx, check *record.ProductionCheck, outcome record.CheckOutcome) error {
	if err := e.markCheckClosed(ctx, tx, check, outcome, ""); err != nil {
		return err
	}
	issue, root, err := e.checkIssue(ctx, tx, *check)
	if err != nil {
		return err
	}
	if record.InWorkflow(issue) {
		e.logOnCommit(ctx, "workflow: production check closed while its issue runs again; its claims and pod are the run's", "issue", issue.Key,
			"mergeCommit", check.MergeSHA, "phase", issue.Phase)
		return nil
	}
	if err := e.enqueue(ctx, tx, issue.Key, record.SuperviseRequest{Op: "issue_close", Tree: issue.Tree, Role: check.Role,
		Generation: issue.Generation, Reason: fmt.Sprintf("%s's production check closed as %s", issue.Key, outcome)}); err != nil {
		return err
	}
	if err := e.enqueue(ctx, tx, issue.Key, record.IssueSuspend{Tree: issue.Tree, Generation: issue.Generation, TreeGeneration: root.Generation}); err != nil {
		return err
	}
	if root.LingerUntil == nil || e.now().Before(*root.LingerUntil) {
		return nil
	}
	open, err := e.checksTheLingerWaitsOn(ctx, tx, root.Tree)
	if err != nil || len(open) > 0 {
		return err
	}
	e.logOnCommit(ctx, "workflow: the last open production check of a tree past its linger closed; the tree's close is re-armed", "tree", root.Key,
		"generation", root.Generation)
	return e.enqueue(ctx, tx, root.Key, record.LingerClose{Generation: root.Generation})
}

// markCheckClosed writes the check closed now with outcome, its look closed with it, ends the
// task of that look (retireLook, unreported: a report's own end, when a report closed it, is
// checkReported's), and logs the close with the issue and the merge commit, and with replacedBy,
// the merge commit of the later merge whose check replaces it (openCheck), where one does: "" for
// every other close. It closes no claim and suspends nothing, which is what a close at a
// replacement and at a run's leave with no merge want (openCheck, leave), and what closeCheck adds
// for every other close.
func (e *Engine) markCheckClosed(ctx context.Context, tx pgx.Tx, check *record.ProductionCheck, outcome record.CheckOutcome, replacedBy string) error {
	now := e.now()
	check.ClosedAt, check.Outcome, check.LookOpen = &now, outcome, false
	if err := e.store.PutProductionCheck(ctx, tx, *check); err != nil {
		return err
	}
	if err := e.retireLook(ctx, tx, *check, false); err != nil {
		return err
	}
	args := []any{"issue", check.Issue, "mergeCommit", check.MergeSHA, "outcome", outcome, "looks", check.Looks}
	if replacedBy != "" {
		args = append(args, "replacedBy", replacedBy)
	}
	e.logOnCommit(ctx, "workflow: production check closed", args...)
	return nil
}

// retireLook queues the end of the task of check's current look (record.SuperviseRequest, op
// retire_look: the check's role, its look and the check instance) in the transaction that ends
// that task: a report of the look (checkReported, reported true), a look before the last that
// ends with no report (endLook), a close of the check (markCheckClosed, the last look's end with
// no report among them), and an issue that runs again with its check open (RetireOpenLook: a
// child's re-entry and a root's re-admission). The outbox runs the row through the claim of the
// check's role (supervise.Machine.RetireLookTask), whose look fence alone decides what goes: the
// task of that look goes once its look no longer holds, and every other task stays. reported says
// the look ended with the role's report, so the claim counts that look served and its prompt
// budgets start again. The row names no generation, since an issue that runs again is at a later
// one and its look's task must still end. A check that started no look holds no task to end, and
// queues nothing.
func (e *Engine) retireLook(ctx context.Context, tx pgx.Tx, check record.ProductionCheck, reported bool) error {
	if check.Looks <= 0 {
		return nil
	}
	return e.enqueue(ctx, tx, check.Issue, record.SuperviseRequest{Op: "retire_look", Tree: check.Tree, Role: check.Role,
		CheckLook: check.Looks, CheckOpenedAt: check.OpenedAt, Reported: reported})
}

// RetireOpenLook ends the task of the current look of key's open production check (retireLook,
// unreported) for an issue that runs again while its check stays open: a done child set back to
// todo (ReenterChild), and a root, or a member that becomes a root of its own, re-admitted at a
// new generation (admit's readmit). The check waits while the run holds the issue
// (endLookPending, checkWakeDue), but the look's task would reach the run's worker on the same
// claim, so it goes now. An issue with no open check queues nothing.
func (e *Engine) RetireOpenLook(ctx context.Context, tx pgx.Tx, key string) error {
	check, err := e.store.ProductionCheck(ctx, tx, key)
	if err != nil || check == nil || !check.Open() {
		return err
	}
	return e.retireLook(ctx, tx, *check, false)
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

// checkLookEnded is a look ending with no report (intake.CheckLookEnded: the look's bound, or a
// hold of the operator's or the controller's met by its start; a look whose checking role's claim
// failed is claimFailed's): the look it names, still open, ends as a pending report would
// (endLook); a look already ended, or a check closed, changes nothing, and so does a look of a
// check a later merge has replaced, one the fact does not name (record.ProductionCheck.NamedBy),
// whose look 1 the current check's would otherwise be taken for, logged.
func (e *Engine) checkLookEnded(ctx context.Context, tx pgx.Tx, fact intake.CheckLookEnded) (intake.Result, error) {
	check, err := e.store.ProductionCheck(ctx, tx, fact.Issue)
	if err != nil || check == nil || !check.Open() || !check.LookOpen || check.Looks != fact.Look {
		return intake.Result{}, err
	}
	if !check.NamedBy(fact.OpenedAt) {
		e.logOnCommit(ctx, "workflow: production check look end ignored: it names a check a later merge replaced", "issue", check.Issue, "mergeCommit", check.MergeSHA,
			"check", check.OpenedAt, "named", fact.OpenedAt, "look", fact.Look, "cause", fact.Cause)
		return intake.Result{}, nil
	}
	return intake.Result{}, e.endLook(ctx, tx, check, fact.Cause, e.lookRetry())
}

// checkWakeDue is a check's wake coming due (intake.CheckWakeDue, the check_wake row nameWake
// queued at ProductionCheck.WakeAt: the time the checking agent named for itself on a pending
// report, or the retry of a look nobody reported). The next look starts, woken by the row's
// reason. Five guards stand before that, in this order, and only the last queues the row again:
//
//   - no check, or one closed since: the row is spent.
//   - a check a later merge of the same issue replaced, which the row does not name
//     (record.ProductionCheck.NamedBy), and whose look 1 this row's would otherwise be taken for.
//   - a row of a look the check has left, or one whose look is open already: the bound started
//     the last look, or a report or a retry started another.
//   - a row of a wake the check no longer records: a failure of the checking role's claim
//     between looks named the next look later (failedBetweenLooks), whose own row starts it. A
//     row a release before the field queued names no wake (record.CheckWakeDue.WakeAt) and is
//     not fenced so.
//   - an issue back in the workflow — a root reopened inside its linger, whose implementer now
//     works the new run — keeps the wake rather than dropping it: no look can run while the run
//     holds the issue's claims and its pod, so the row is queued again a look bound out
//     (lookRetry, clamped by clampWake, so no row is ever queued past the check's bound), and a
//     check whose issue re-entered the workflow and closed again on the same merge still holds its
//     agent's wake. The check's own bound closes it where the run's next merge does not
//     (checkBound).
//
// The first four change nothing and are logged with why; without them the last one's queue
// would make an orphan row immortal.
func (e *Engine) checkWakeDue(ctx context.Context, tx pgx.Tx, fact intake.CheckWakeDue) (intake.Result, error) {
	check, err := e.store.ProductionCheck(ctx, tx, fact.Issue)
	if err != nil {
		return intake.Result{}, err
	}
	var why string
	switch {
	case check == nil:
		why = "the issue has no production check"
	case !check.Open():
		why = fmt.Sprintf("the check closed as %s", check.Outcome)
	case !check.NamedBy(fact.OpenedAt):
		why = fmt.Sprintf("the check it names was replaced by the one opened %s", check.OpenedAt.UTC().Format(time.RFC3339))
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
	if record.InWorkflow(issue) {
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

// checkBound is the check's bound coming due (intake.CheckBound, the check_bound row at its
// bound_at). A closed check, or one whose last look has started, changes nothing. A row that comes
// due before the check's bound is an earlier check's of the same issue, replaced by a later merge
// (openCheck), and is deferred to this check's bound. A look open at the bound defers it by the
// look bound: the look's own bound ends the look first. An issue back in the workflow — a root
// reopened inside its linger — closes the check as not run, since no look can run while its
// implementer works the new run, and the run's own merge opens its own. Otherwise the last look
// starts, told that it is the last and the check's own bound (BoundAt less OpenedAt, which a later
// change of the project's bound leaves alone) counted from the issue's close, which opened the
// check (openCheck) and comes after the merge for a merge that waited on the issues beneath it
// (closeMerged), and nothing of why the looks before it were pending, which the daemon does not
// know: production not carrying the merge yet, or a merge it carries with a requirement only the
// deploy can show still unobserved.
func (e *Engine) checkBound(ctx context.Context, tx pgx.Tx, fact intake.CheckBound) (intake.Result, error) {
	check, err := e.store.ProductionCheck(ctx, tx, fact.Issue)
	if err != nil || check == nil || !check.Open() || check.Last {
		return intake.Result{}, err
	}
	now := e.now()
	if now.Before(check.BoundAt) {
		e.logOnCommit(ctx, "workflow: a production check bound of an earlier check came due; deferred to the check's own", "issue", check.Issue, "bound", check.BoundAt)
		return intake.Result{}, e.enqueueAt(ctx, tx, check.Issue, record.CheckBound{}, check.BoundAt)
	}
	if check.LookOpen {
		e.logOnCommit(ctx, "workflow: production check bound deferred while a look is open", "issue", check.Issue, "mergeCommit", check.MergeSHA, "look", check.Looks)
		return intake.Result{}, e.enqueueAt(ctx, tx, check.Issue, record.CheckBound{}, now.Add(CheckLookBound))
	}
	issue, _, err := e.checkIssue(ctx, tx, *check)
	if err != nil {
		return intake.Result{}, err
	}
	if record.InWorkflow(issue) {
		e.logOnCommit(ctx, "workflow: production check bound came due while its issue runs again", "issue", issue.Key, "mergeCommit", check.MergeSHA, "phase", issue.Phase)
		return intake.Result{}, e.closeCheck(ctx, tx, check, record.CheckNotRun)
	}
	return intake.Result{}, e.startLook(ctx, tx, issue, check, wakeBound,
		fmt.Sprintf("%s since the issue closed", boundText(check.BoundAt.Sub(check.OpenedAt))), true)
}
