package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// The lifecycle of one issue is the barrier a claim of this release binds and its cleanup
// reserves (decision 16 of dispatch://INFRA-4730): the barrier the tree lifecycle gave a whole
// tree, given to each issue alone, through the one treelifecycle implementation (Issues). A
// cleanup of one issue deletes that issue's resources and nothing of another's, an ancestor's
// least of all (R17 of dispatch://INFRA-3320). project is the project token, as the claims table
// holds it, here as for a tree.

// OpenIssueLifecycle opens issue's lifecycle in a transaction of its own and runs nothing beside
// the open; a test and a seed open an issue through it. Admission opens one inside its fact
// transaction through record.Store.OpenIssueLifecycle instead.
func (s *Store) OpenIssueLifecycle(ctx context.Context, project, issue string, authority treelifecycle.Authority) (treelifecycle.Lifecycle, error) {
	return s.openLifecycle(ctx, treelifecycle.Issues, project, issue, authority, runNothing)
}

// ReserveIssueCleanup records the workflow's cleanup of issue at the close of its run generation,
// before any resource snapshot. A retry of an unconfirmed reservation resumes it whatever the
// issue has done since; a retry after the cleanup confirmed reserves nothing; and a fresh
// reservation first proves the issue is still at that generation and out of Legion's work, so a
// cleanup naming a generation the issue never had, or no generation, or one of an issue that came
// back to work, reserves nothing either, and the outbox's ordinary fences finish it. So does a
// cleanup of an issue that never had a lifecycle, which holds nothing a cleanup releases.
// inLegionsWork is the workflow's rule for the phase and the status of an issue in Legion's work
// (record.InLegionsWork), which the store does not restate. The record read is the project's own
// (lower(project) = project), as AdmitClaim reads it.
func (s *Store) ReserveIssueCleanup(ctx context.Context, project, issue string, generation uint64,
	inLegionsWork func(at phase.Phase, status string) bool) (treelifecycle.Lifecycle, bool, error) {
	var lifecycle treelifecycle.Lifecycle
	var reserved bool
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		if err := treelifecycle.Serialize(ctx, tx); err != nil {
			return err
		}
		var started, confirmed bool
		err := tx.QueryRow(ctx, `select cleanup_started, cleanup_confirmed_at is not null from issue_lifecycles
			where project = $1 and issue = $2 for update`, project, issue).Scan(&started, &confirmed)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if started && !confirmed {
			lifecycle, err = treelifecycle.Issues.ReserveCleanup(ctx, tx, project, issue, treelifecycle.AuthorityWorkflow)
			reserved = true
			return err
		}
		if started && confirmed {
			return nil
		}
		if generation == 0 || generation > math.MaxInt64 {
			return nil
		}
		var stored int64
		var issuePhase, status string
		err = tx.QueryRow(ctx, `select generation, phase, status from issues
			where key = $1 and lower(project) = $2 for update`, issue, project).Scan(&stored, &issuePhase, &status)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read workflow issue %s: %w", issue, err)
		}
		if uint64(stored) != generation || inLegionsWork(phase.Phase(issuePhase), status) {
			return nil
		}
		lifecycle, err = treelifecycle.Issues.ReserveCleanup(ctx, tx, project, issue, treelifecycle.AuthorityWorkflow)
		reserved = err == nil
		return err
	})
	if err != nil {
		return treelifecycle.Lifecycle{}, false, err
	}
	return lifecycle, reserved, nil
}

// ReserveOperatorIssueCleanup is the operator's close of an issue its own spawn opened, called only
// once the operator route authenticated the close. It is not selected through a zero workflow
// generation. A close asked again after its cleanup confirmed reserves nothing, so the operator's
// retry answers as the finished close it is.
func (s *Store) ReserveOperatorIssueCleanup(ctx context.Context, project, issue string) (treelifecycle.Lifecycle, bool, error) {
	var lifecycle treelifecycle.Lifecycle
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		reserved, err := treelifecycle.Issues.ReserveCleanup(ctx, tx, project, issue, treelifecycle.AuthorityOperator)
		if err != nil {
			return err
		}
		lifecycle = reserved
		return nil
	})
	if errors.Is(err, treelifecycle.ErrLifecycleClosed) {
		return treelifecycle.Lifecycle{}, false, nil
	}
	if err != nil {
		return treelifecycle.Lifecycle{}, false, err
	}
	return lifecycle, true, nil
}

// pendingIssueClaims names every stored claim of issue that has not retired, as token:state. Read
// after the issue's cleanup reservation committed, it is the issue's complete runnable
// population: no later claim can bind to the reserved epoch, and a claim that won before the
// reservation is already stored, whether or not its process launched yet.
func pendingIssueClaims(ctx context.Context, tx pgx.Tx, project, issue string) ([]string, error) {
	rows, err := tx.Query(ctx, `select token || ':' || state from claims
		where project = $1 and issue = $2 and state <> 'retired' order by token`, project, issue)
	if err != nil {
		return nil, fmt.Errorf("census claims of issue %s: %w", issue, err)
	}
	pending, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("census claims of issue %s: %w", issue, err)
	}
	return pending, nil
}

// IssueReleaser is what a runtime holds for one issue beyond its claims' processes: the Sandbox
// runtime's issue Sandbox and the Secrets it owns. A runtime that holds nothing of the kind
// releases nothing, and releasing what is already gone succeeds.
type IssueReleaser interface {
	CleanupIssue(ctx context.Context, issue string) error
}

// CleanupReservedIssue finishes the cleanup of issue that its caller reserved at epoch, for the
// workflow (ReserveIssueCleanup) or the operator (ReserveOperatorIssueCleanup). One short
// transaction proves epoch is still the issue's unconfirmed reservation and reads the issue's
// claims: any claim that has not retired makes it a wait (pendingIssueClaims). Then the runtime
// releases the issue's resources and the reservation is confirmed, after which only a fresh open
// starts the issue's next epoch. A caller whose reservation is no longer current releases nothing.
// A retry after a failed release runs it again.
//
// The release deletes the issue's pod, so the transaction that confirms it also ends the pause's
// hold of it (endIssuePausePodHoldsTx, decision 13 of dispatch://INFRA-3759). The confirmation
// goes first, since it takes treelifecycle.Serialize, which every writer of a pod hold takes before
// the issue's lock (LockIssueForPause).
func (s *Store) CleanupReservedIssue(ctx context.Context, project, issue string, epoch uint64, releaser IssueReleaser) error {
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		if err := treelifecycle.Issues.CheckReservation(ctx, tx, project, issue, epoch); err != nil {
			return err
		}
		pending, err := pendingIssueClaims(ctx, tx, project, issue)
		if err != nil {
			return err
		}
		if len(pending) > 0 {
			return wait.Errorf("cleanup of issue %s waits for its claims %s to retire", issue, strings.Join(pending, ", "))
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := releaser.CleanupIssue(ctx, issue); err != nil {
		return fmt.Errorf("release the resources of issue %s: %w", issue, err)
	}
	return s.Tx(ctx, func(tx pgx.Tx) error {
		if err := treelifecycle.Issues.ConfirmCleanup(ctx, tx, project, issue, epoch); err != nil {
			return err
		}
		return endIssuePausePodHoldsTx(ctx, tx, project, issue, time.Now().UTC())
	})
}

// IssueLive is whether issue has a lifecycle whose cleanup is not confirmed: open, or reserved and
// releasing. Such an issue's resources are its own cleanup's to delete, and no orphan sweep's.
func (s *Store) IssueLive(ctx context.Context, project, issue string) (bool, error) {
	var live bool
	err := s.pool.QueryRow(ctx, `select exists (select 1 from issue_lifecycles
		where project = $1 and issue = $2 and cleanup_confirmed_at is null)`, project, issue).Scan(&live)
	if err != nil {
		return false, fmt.Errorf("read the lifecycle of issue %s: %w", issue, err)
	}
	return live, nil
}
