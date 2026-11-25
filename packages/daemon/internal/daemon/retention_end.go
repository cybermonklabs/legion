package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/wait"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// retentionEnd is the retention_end executor (record.RetentionEnd, decision 17 of
// dispatch://INFRA-4730): at the deadline a linger of the release before left one issue
// (record.Issue.RetainedUntil), for that issue while it stays out of Legion's work, and after any
// production check it owes, it does what that linger's expiry did for it alone — each suspended or
// failed claim of it retires with its session kept (supervise.RequestIssueClose), as that expiry's
// close retired every claim it could stop, the decline of an idle release of it and the record of
// its pods' disks go (forgetIdleRelease), and its workspace on the daemon's own host is removed
// (removeRetainedWorkspace), called here directly, with no workspace_remove row, which waits for
// every claim of the issue to retire. A failed claim left standing would hold that removal, and
// the row, for good. Nothing of any other issue is touched. The row is fenced by the run and the
// phase entry it was queued at (record.FenceOf, run): an issue that came back to work since stands
// at a later entry, and the row finishes there without acting; one in Legion's work at this entry
// finishes too.
func (r *outbox) retentionEnd(ctx context.Context, row record.OutboxRow, payload record.RetentionEnd) error {
	if r.supervisor == nil {
		return errors.New("retention end executor has no claim supervisor")
	}
	issue, err := r.recordedIssue(ctx, row.Issue)
	if err != nil || issue == nil {
		return err
	}
	if record.InLegionsWork(*issue) {
		r.log.Info("outbox retention end of an issue back in Legion's work; finished without acting", "row", row.ID, "issue", issue.Key,
			"status", issue.Status, "phase", issue.Phase)
		return nil
	}
	if until := issue.RetainedUntil; until != nil && r.now().Before(*until) {
		return wait.Errorf("the retention of %s ends at %s", issue.Key, until.UTC().Format(time.RFC3339))
	}
	check, err := r.productionCheck(ctx, row.Issue)
	if err != nil {
		return err
	}
	if check != nil && check.Open() {
		return wait.Errorf("the retention of %s ends after the production check it owes", issue.Key)
	}
	for _, role := range claim.Roles {
		token, err := claim.NewToken(r.project, row.Issue, role)
		if err != nil {
			return fmt.Errorf("derive claim for outbox row %d: %w", row.ID, err)
		}
		machine, found := r.supervisor.Machine(token)
		if !found || machine.Claim().State != supervise.StateSuspended && machine.Claim().State != supervise.StateFailed {
			continue
		}
		if err := machine.Handle(ctx, supervise.RequestIssueClose{Claim: token}); err != nil {
			return fmt.Errorf("retire claim %s at the end of its issue's retention: %w", token, err)
		}
	}
	r.forgetIdleRelease(ctx, row.Issue)
	if err := r.removeRetainedWorkspace(ctx, *issue); err != nil {
		return err
	}
	r.log.Info("outbox retention end: the issue's suspended and failed claims retired, its idle release's decline forgotten and its workspace removed", "row", row.ID, "issue", issue.Key,
		"generation", payload.Generation, "entry", payload.Entry)
	return nil
}

// removeRetainedWorkspace removes issue's workspace on the daemon's own host at the end of its
// retention (retentionEnd), which is what reads the issue's retained_until: a runtime that
// provisions each pod's workspace itself holds none here (podsProvision). It waits until every
// claim of the issue has retired — a claim whose retirement the runtime refused still runs there
// — and then forgets the workspace in the shared clone under the issue's turn and the clone's
// store lock, in the order a provision takes them (provisionWorkspace), so it never runs under
// that issue's provision. A retired claim's relaunch provisions the workspace again.
func (r *outbox) removeRetainedWorkspace(ctx context.Context, issue record.Issue) error {
	if r.podsProvision() || issue.RetainedUntil == nil {
		return nil
	}
	for _, role := range claim.Roles {
		token, err := claim.NewToken(r.project, issue.Key, role)
		if err != nil {
			return fmt.Errorf("derive claim of %s: %w", issue.Key, err)
		}
		if machine, found := r.supervisor.Machine(token); found && machine.Claim().State != supervise.StateRetired {
			return wait.Errorf("the removal of %s's workspace waits for claim %s (%s) to retire", issue.Key, token, machine.Claim().State)
		}
	}
	repo, err := issue.Repository()
	if err != nil {
		return fmt.Errorf("locate the workspace of %s: %w", issue.Key, err)
	}
	working, err := workspace.Location(r.stateDir, repo, issue.Key)
	if err != nil {
		return fmt.Errorf("locate the workspace of %s: %w", issue.Key, err)
	}
	issueTurn, releaseIssue := r.provisions.issues.take(working.Dir)
	defer releaseIssue()
	issueTurn.Lock()
	defer issueTurn.Unlock()
	store, releaseStore := r.provisions.stores.take(working.Clone)
	defer releaseStore()
	if err := r.remove(ctx, working, store); err != nil {
		return fmt.Errorf("remove the workspace of %s: %w", issue.Key, err)
	}
	return nil
}
