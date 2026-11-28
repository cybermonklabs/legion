package workflow

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// RepairOpenChecks is the boot's one pass over the open production checks of the project (R6 of
// dispatch://INFRA-3518). The daemon runs it at each boot, in one transaction under the
// lock every fact applies under, before the listing's reconcile and before any fact
// (internal/daemon's repairOpenChecks), so no leave, re-entry or start row of the boot reads a
// check before it has run. It closes no check. It answers how many looks it gave a look id and
// how many checks it gave a generation.
//
// An open look with no look id of its own (record.ProductionCheck.CurrentLookID is ""), which an
// older daemon opened or left behind after a rollback, gets one, made as startLook makes it
// (giveLookID), and one start row of that same look as startLook queues it (lookStart): the
// check's role, the look's number and the check's opening time, the generation the issue holds,
// the look id and the task checkTask writes now, from the record's last wake (LastWakeKind,
// LastWakeDetail) and its last mark (Last), with the mark of the new task of an open look
// (record.SuperviseRequest.LookReissue). The look keeps its number, its wake and its bound: the
// check_look row its start queued still ends it. The new task takes the place of the old one,
// which gives no look id, or one that is not the current look's, and so never holds (supervise's
// queue and its send), and the old start row finishes at its fence for the same reason (the
// outbox's checkLookHolds). The start row of a look whose issue runs again finishes there too,
// since no look runs while the run holds the issue. A start row so marked that meets a hold of the
// operator's or the controller's waits for the resume, as a phase start does, and does not end the
// look (the outbox's start executor).
//
// An open look that has an id of its own, of an issue out of the workflow, gets a new id and the
// same start row where a task of that look gives no look id or another one, as
// lookTaskGivesAnotherID reads the tasks of the look. After a rollback the release before can
// leave such a task: a delivery it writes, or a start row whose look id the way back took off
// (docs/kubernetes.md, "Rolling back the release whose look report gives its look id", says
// how). The send drops such a task and the fence finishes such a row unrun, so the look would end
// at its bound with no task of this release. A look whose issue runs again keeps its id: its
// checking role can hold that id from the look's task and report with it while the run holds the
// issue (decision 7 of dispatch://INFRA-3518). Its leave, however it reaches the record (the
// listing's reconcile after this pass among them), closes its check as not run, or as replaced
// where a later merge opens one (leave: closeCheckOfARunThatLeft, openCheck), since this pass
// gives the check, which the way back left with no generation, the issue's generation less one
// (bootGeneration), so no open look of it is left that needs a task.
//
// An open check with no generation (record.ProductionCheck.OpenedGeneration is 0), which an older
// binary opened or the way back left, gets one, read from its issue as the record holds it before
// the reconcile (bootGeneration), so a leave's test of whether the issue ran again (ranAgain)
// reads it from the first fact on.
//
// Its journal names each check it changed by its issue and its look, and never the look id or the
// task (R3 of dispatch://INFRA-3518).
func (e *Engine) RepairOpenChecks(ctx context.Context, tx pgx.Tx) (looks, generations int, err error) {
	open, err := e.store.OpenProductionChecks(ctx, tx)
	if err != nil {
		return 0, 0, err
	}
	for _, check := range open {
		if check.Project != e.cfg.Project {
			continue
		}
		issue, err := e.store.Issue(ctx, tx, check.Issue)
		if err != nil {
			return 0, 0, err
		}
		if issue == nil {
			// No path of the daemon deletes an issue's record, so only a hand-made row reaches
			// here. Such a check starts no look (checkIssue reads its issue first), and one row
			// must not stop the boot of the whole project, so it is named and left as it is.
			e.logOnCommit(ctx, "workflow: the boot left an open production check whose issue is not recorded", "issue", check.Issue,
				"mergeCommit", check.MergeSHA, "look", check.Looks)
			continue
		}
		generation := false
		if opened := bootGeneration(*issue); check.OpenedGeneration == 0 && opened != 0 {
			check.OpenedGeneration, generation = opened, true
			generations++
			e.logOnCommit(ctx, "workflow: the boot gave an open production check the generation it opened at", "issue", issue.Key,
				"mergeCommit", check.MergeSHA, "openedGeneration", opened, "generation", issue.Generation, "inWorkflow", record.InWorkflow(*issue))
		}
		reissue, why := false, ""
		switch {
		case !check.LookOpen || check.Looks <= 0:
		case check.CurrentLookID() == "":
			reissue, why = true, "the look has no look id of its own"
		case !record.InWorkflow(*issue):
			if reissue, err = e.lookTaskGivesAnotherID(ctx, tx, issue, &check); err != nil {
				return 0, 0, err
			}
			why = "a task of the look gives another look id or none"
		}
		if reissue {
			giveLookID(&check)
			looks++
		}
		if !generation && !reissue {
			continue
		}
		if err := e.store.PutProductionCheck(ctx, tx, check); err != nil {
			return 0, 0, err
		}
		if !reissue {
			continue
		}
		start := lookStart(*issue, check)
		start.LookReissue = true
		if err := e.enqueue(ctx, tx, issue.Key, start); err != nil {
			return 0, 0, err
		}
		e.logOnCommit(ctx, "workflow: the boot gave an open production check look an id of its own and a new task", "issue", issue.Key,
			"mergeCommit", check.MergeSHA, "look", check.Looks, "role", check.Role, "last", check.Last, "why", why)
	}
	return looks, generations, nil
}

// lookTaskGivesAnotherID says whether a task of check's current look gives no look id or one that
// is not the look's current id (record.LookGivesAnotherID) while no task of that look gives the
// current id (record.LookHolds), which is when the boot gives the look a new id and a new task
// (RepairOpenChecks). The tasks are each start row of the look queued for check's role on issue,
// which carries its id and which the fence finishes unrun where the id does not hold (the
// outbox's checkLookHolds), and the task the role's claim holds, whose delivery holds its id and
// which the send drops where the id does not hold. A task that gives the current id reaches the
// role and takes the place of any other, as the boot's new task would, so a second boot before
// such a row runs gives the look no other id. A task of no look, of another look or of the check
// a later merge replaced counts for neither: it gives no id of the current look, and the release
// before writes a look's task only for the current look of the check (its start fence), so such a
// task changes nothing of what the role holds of the current look. A claim the store does not
// hold holds no task, and a key or a role that makes no claim token has no claim, as admission's
// resumeSlottedWorkers passes one over.
func (e *Engine) lookTaskGivesAnotherID(ctx context.Context, tx pgx.Tx, issue *record.Issue, check *record.ProductionCheck) (bool, error) {
	starts, err := e.store.QueuedLookStarts(ctx, tx, issue.Key, check.Role)
	if err != nil {
		return false, err
	}
	other := false
	for _, start := range starts {
		look := supervise.CheckLook{Look: start.CheckLook, OpenedAt: start.CheckOpenedAt}
		if record.LookHolds(issue, check, look, start.LookID) {
			return false, nil
		}
		other = other || record.LookGivesAnotherID(issue, check, look, start.LookID)
	}
	project, err := claim.ProjectToken(e.cfg.Project)
	if err != nil {
		return false, err
	}
	token, err := claim.NewToken(project, issue.Key, check.Role)
	if err != nil {
		return other, nil
	}
	held, err := e.store.RoleClaims(ctx, tx, []claim.Token{token})
	if err != nil {
		return false, err
	}
	if role := held[token]; role != nil && role.Pending != nil {
		if record.LookHolds(issue, check, role.Pending.CheckLook, role.Pending.LookID) {
			return false, nil
		}
		other = other || record.LookGivesAnotherID(issue, check, role.Pending.CheckLook, role.Pending.LookID)
	}
	return other, nil
}

// bootGeneration is the generation the boot gives an open check that has none, read from its
// issue as the record holds it (RepairOpenChecks). A check opens only at a done, an older binary
// closed a child's check at its re-entry, and the way back (docs/kubernetes.md, "Rolling back the
// release whose look report gives its look id") closes the open checks of the children that run
// again, so an issue in the workflow holds such a check only as a root an older binary re-admitted
// after the check's merge: its generation less one, which is below the generation it runs at
// however many times it was re-admitted, so its leave with no merge closes the check (ranAgain).
// An issue out of the workflow gets its own generation: a child's such check is the check of its
// last merge, and a root that an older binary re-admitted and that then left with no merge keeps
// its check open, as that binary left it, and closes it at its next run that leaves with no
// merge. 0 for an issue at no generation, which leaves the check with none.
func bootGeneration(issue record.Issue) uint64 {
	if record.InWorkflow(issue) && issue.Generation > 0 {
		return issue.Generation - 1
	}
	return issue.Generation
}
