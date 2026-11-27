package workflow

import (
	"context"
	"crypto/rand"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
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
// (crypto/rand's Text) and stored with the look's number and the check instance, and one start
// row of that same look: the check's role, the look's number and the check's opening time, the
// generation the issue holds, the look id, the mark of the new task of an open look
// (record.SuperviseRequest.LookReissue) and the task checkTask writes now, from the record's last
// wake (LastWakeKind, LastWakeDetail) and its last mark (Last). The look keeps its number, its
// wake and its bound: the check_look row its start queued still ends it. The new task takes the
// place of the old one, which gives no look id and so never holds (supervise's queue and its
// send), and the old start row finishes at its fence for the same reason (the outbox's
// checkLookHolds). The start row of a look whose issue runs again finishes there too, since no
// look runs while the run holds the issue. A start row so marked that meets a hold of the
// operator's or the controller's waits for the resume, as a phase start does, and does not end
// the look (the outbox's start executor).
//
// An open check with no generation (record.ProductionCheck.OpenedGeneration is 0), which an older
// binary opened or the way back of docs/kubernetes.md left, gets one, read from its issue as the
// record holds it before the reconcile (bootGeneration), so a leave's test of whether the issue
// ran again (ranAgain) reads it from the first fact on.
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
		reissue := check.LookOpen && check.Looks > 0 && check.CurrentLookID() == ""
		if reissue {
			check.LookID, check.LookIDLook, check.LookIDOpenedAt = rand.Text(), check.Looks, check.OpenedAt
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
		if err := e.enqueue(ctx, tx, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: check.Role,
			Generation: issue.Generation, CheckLook: check.Looks, CheckOpenedAt: check.OpenedAt, LookID: check.LookID, LookReissue: true,
			Task: checkTask(*issue, check, check.LookID, check.LastWakeKind, check.LastWakeDetail, check.Last)}); err != nil {
			return 0, 0, err
		}
		e.logOnCommit(ctx, "workflow: the boot gave an open production check look an id of its own and a new task", "issue", issue.Key,
			"mergeCommit", check.MergeSHA, "look", check.Looks, "role", check.Role, "last", check.Last)
	}
	return looks, generations, nil
}

// bootGeneration is the generation the boot gives an open check that has none, read from its
// issue as the record holds it (RepairOpenChecks). A check opens only at a done, an older binary
// closed a child's check at its re-entry, and the way back of docs/kubernetes.md closes the open
// checks of the children that run again, so an issue in the workflow holds such a check only as a
// root an older binary re-admitted after the check's merge: its generation less one, which is
// below the generation it runs at however many times it was re-admitted, so its leave with no
// merge closes the check (ranAgain). An issue out of the workflow gets its own generation: a
// child's such check is the check of its last merge, and a root that an older binary re-admitted
// and that then left with no merge keeps its check open, as that binary left it, and closes it at
// its next run that leaves with no merge. 0 for an issue at no generation, which leaves the check
// with none.
func bootGeneration(issue record.Issue) uint64 {
	if record.InWorkflow(issue) && issue.Generation > 0 {
		return issue.Generation - 1
	}
	return issue.Generation
}
