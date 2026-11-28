package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// IssueClose is one workflow close of an issue, as its issue_suspend outbox row names it: the run
// it closed (the issue's own generation) and the row, written after the close's per-role stops. A
// later start supersedes it even within the same generation. Nothing of the tree the issue's record
// stores is read: a close is the issue's own (R17 of dispatch://INFRA-3320). TurnStart is the turn
// start a left pod's close fences on (record.IssueSuspend.TurnStart), nil for every other close.
type IssueClose struct {
	Issue      string
	Generation uint64
	Row        int64
	TurnStart  *time.Time
}

// IssueSuspension decides whether a workflow close may suspend its issue's Sandbox now, in one
// short transaction under the shared serializer: false, finishing the close without acting, once
// the run it closed is over (the issue is in the workflow again, by its own status and phase, or
// at another generation), a start that will start the issue was written, or the issue's own
// cleanup is reserved, which deletes the Sandbox this close would; a wait while one of the close's
// role stops (a suspend, an issue close, or a role stop of an idle release) is unfinished or a
// stored claim of the issue may still hold a process. The close deletes the issue's Sandbox once
// its pod is gone, which costs a claim of the issue nothing it still holds: the role Secrets go
// with it and the issue's next launch writes its own, and no session lives there — an issue pod's
// disk goes with the pod. The caller holds the runtime's launch turn of the issue's pod through
// this check and its Sandbox patch, never this transaction. outOfWorkflow is the workflow's rule
// for a status that takes an issue out of it (record.OutOfWorkflow), which the store does not
// restate. The record read is the project's own (lower(project) = project), as AdmitClaim reads it.
//
// A start that will start the issue is one a claim has already run (claims.last_start_row) or a
// queued look of a production check. Every other queued start row is one the outbox's own start
// fence will decline for an issue out of the workflow, so finishing the suspension on such a row
// would release nothing and leave the Sandbox, the pod and the disk under it standing with no row
// left to try. The look is the one queued start kept, because it is the one an issue out of the
// workflow still runs: a merged issue's close queues the suspensions and then the first look's
// start in the same transaction, so that start stands above them, and a suspension that acted
// there would delete the Sandbox and the pod immediately before the look resumed its role in
// them.
//
// A left pod's close that fences on a turn start (IssueClose.TurnStart) finishes without acting
// too where a claim of the issue started a turn after it (claims.last_turn_start, compared on the
// database's own precision): the stop of that claim's role held for the same turn, and that turn
// may have made work the close's read did not see. The claim may stop later by another route, an
// operator's suspension or its relaunches running out, and a suspension still waiting would then
// delete the disk with no read; finished now, nothing of the close outlives the hold, and the next
// idle pass decides the close again (workflow.LeftPodClose).
func (s *Store) IssueSuspension(ctx context.Context, project string, close IssueClose, outOfWorkflow func(status string) bool) (bool, error) {
	if close.Issue == "" || close.Generation == 0 || close.Row <= 0 {
		return false, errors.New("issue suspension requires the issue, its generation and its outbox row")
	}
	var act bool
	err := Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := treelifecycle.Serialize(ctx, tx); err != nil {
			return err
		}
		var generation int64
		var issuePhase, status string
		err := tx.QueryRow(ctx, `select generation, phase, status from issues where key = $1 and lower(project) = $2`,
			close.Issue, project).Scan(&generation, &issuePhase, &status)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		stopped := phase.Phase(issuePhase) == phase.Done || outOfWorkflow(status)
		if uint64(generation) != close.Generation || !stopped {
			return nil
		}
		var restarted bool
		if err := tx.QueryRow(ctx, `select exists (
			select 1 from claims where project = $1 and issue = $2 and last_start_row > $3)
			or exists (select 1 from outbox where issue = $2 and id > $3 and kind = 'supervise'
			and payload->>'op' = 'start' and coalesce((payload->>'checkLook')::bigint, 0) > 0)`,
			project, close.Issue, close.Row).Scan(&restarted); err != nil {
			return err
		}
		if restarted {
			return nil
		}
		if close.TurnStart != nil {
			var turned bool
			if err := tx.QueryRow(ctx, `select exists (
				select 1 from claims where project = $1 and issue = $2 and last_turn_start > $3)`,
				project, close.Issue, *close.TurnStart).Scan(&turned); err != nil {
				return err
			}
			if turned {
				return nil
			}
		}
		may, err := issueStopFences(ctx, tx, project, close.Issue, "suspension", close.Row, close.Generation)
		if err != nil {
			return err
		}
		act = may
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("authorize issue suspension: %w", err)
	}
	return act, nil
}

// issueStopFences are three checks a close's authorizer and a release's both make (a close's
// last), in the order both make them and against the caller's own transaction: the issue's own
// cleanup reservation, the issue's own unfinished role stops, and a stored claim of the issue that
// may still hold a process. what is the noun the caller's waits are read with ("suspension",
// "release").
//
// It answers false with no error for a reserved cleanup, which deletes the Sandbox the caller
// would and so finishes the caller without acting, and a wait error for each of the other two,
// which the caller is retried on. A close and a release order against each other here: both read
// the same `suspend`, `step_stop`, `issue_close` and `release` ops, so each waits on the other's
// role stops, and on the stop of a stopped task's step (workflow's stopTask).
func issueStopFences(ctx context.Context, tx pgx.Tx, project, issue, what string, row int64, generation uint64) (bool, error) {
	var reserved bool
	if err := tx.QueryRow(ctx, `select exists (select 1 from issue_lifecycles
		where project = $1 and issue = $2 and cleanup_started)`, project, issue).Scan(&reserved); err != nil {
		return false, err
	}
	if reserved {
		return false, nil
	}
	var stopping bool
	if err := tx.QueryRow(ctx, `select exists (
		select 1 from outbox where issue = $1 and id < $2 and kind = 'supervise'
		and payload->>'op' in ('suspend', 'step_stop', 'issue_close', 'release') and (payload->>'generation')::bigint = $3)`,
		issue, row, generation).Scan(&stopping); err != nil {
		return false, err
	}
	if stopping {
		return false, wait.Errorf("issue %s %s waits for its role stop effects", issue, what)
	}
	var pending string
	err := tx.QueryRow(ctx, `select token || ':' || state from claims where project = $1 and issue = $2 and `+claimMayRun+`
		order by token limit 1`, project, issue).Scan(&pending)
	if err == nil {
		return false, wait.Errorf("issue %s %s waits for stored claim %s", issue, what, pending)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	return true, nil
}
