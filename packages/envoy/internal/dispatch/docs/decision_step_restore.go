package docs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// ErrAnotherBuildLive refuses the decisions step's third phase while processes.Live lists a
// process of a commit other than the caller's (DecisionStep.anotherBuildLive): any other build can
// write a row without its node, and reads the documents the phase moves, so the phase waits until
// the fleet is one build. It refuses the phase before it starts, and stops a pass at the issue
// whose transaction finds such a process live, which it rolls back (restoreIssue).
//
// kept for one release: decisions-step
var ErrAnotherBuildLive = errors.New("docs: a process of another build is live, so the decisions step's third phase waits")

// DecisionIssueFailure is an issue the third phase could not restore or convert: its transaction
// rolled back, nothing of it was written, and the next pass tries it again.
//
// kept for one release: decisions-step
type DecisionIssueFailure struct {
	Issue string
	Err   error
}

// RestoreAndConvert is the decisions step's third phase over the issues of project, or of every
// project for "", on a fleet of this one build: ErrAnotherBuildLive otherwise, with nothing done.
// Where convert, it first rewrites each list of gated steps that holds `decisions` without it, a
// list of `decisions` alone becoming `[spec]` (F6; no design_gates row is deleted), and then, for
// each issue that holds a decisions document (decisions_documents()) or an answered ask the
// restore owes its spec (graphcensus.ReadRestoreWork), oldest issue first, restores those asks and
// converts the document's sections, in one transaction for each issue (restoreIssueDecisions).
// Without convert it restores the asks alone, for each issue the restore owes. dryRun does all of
// it and rolls each transaction back. It answers each issue it touched and each issue that failed,
// whose transaction rolled back and which the next pass tries again; its error is one that stops
// the whole phase. An issue a document of which owes its block index is neither: it is left, with
// nothing written, for a later pass (errBlockIndexOwed). A process of another build that goes live
// during the pass stops it at the next issue's transaction, which reads the fleet again
// (restoreIssue): it answers the issues it touched before that one with ErrAnotherBuildLive, and
// converts nothing more until a later pass finds the fleet one build. It writes through the
// document service the step was given (UseDocuments).
//
// kept for one release: decisions-step
func (s *DecisionStep) RestoreAndConvert(ctx context.Context, project string, convert, dryRun bool) ([]DecisionIssue, []DecisionIssueFailure, error) {
	if s.documents == nil {
		return nil, nil, errors.New("docs: the decisions step's third phase has no document service to write through")
	}
	other, err := s.anotherBuildLive(ctx, s.database.Pool)
	if err != nil {
		return nil, nil, err
	}
	if other {
		return nil, nil, ErrAnotherBuildLive
	}
	if convert {
		if err := s.rewriteGatedSteps(ctx, project, dryRun); err != nil {
			return nil, nil, err
		}
	}
	work, err := graphcensus.ReadRestoreWork(ctx, s.database.Pool)
	if err != nil {
		return nil, nil, err
	}
	keys := make([]string, 0, len(work))
	byIssue := map[string][]graphcensus.RestoreWork{}
	for _, ask := range work {
		if _, listed := byIssue[ask.Issue]; !listed {
			keys = append(keys, ask.Issue)
		}
		byIssue[ask.Issue] = append(byIssue[ask.Issue], ask)
	}
	if convert {
		// kept for one release: decisions-documents-fn
		rows, err := s.database.Pool.Query(ctx, `select distinct issue_key from decisions_documents()`)
		if err != nil {
			return nil, nil, fmt.Errorf("list the decisions documents: %w", err)
		}
		documents, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return nil, nil, fmt.Errorf("list the decisions documents: %w", err)
		}
		for _, issue := range documents {
			if _, listed := byIssue[issue]; !listed {
				keys = append(keys, issue)
			}
		}
	}
	rows, err := s.database.Pool.Query(ctx, `
		select key from issues where key = any($1::text[]) and ($2 = '' or project_key = $2)
		order by created_at, key
	`, keys, project)
	if err != nil {
		return nil, nil, fmt.Errorf("order the issues the decisions step owes: %w", err)
	}
	issues, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, nil, fmt.Errorf("order the issues the decisions step owes: %w", err)
	}
	var done []DecisionIssue
	var failed []DecisionIssueFailure
	for _, issue := range issues {
		if err := ctx.Err(); err != nil {
			return done, failed, err
		}
		result, err := s.restoreIssue(ctx, issue, byIssue[issue], convert, dryRun)
		if errors.Is(err, ErrAnotherBuildLive) {
			s.logger.Info("dispatch: decisions step stopped its third phase: a process of another build went live during the pass",
				"issue", issue, "touched", len(done), "dry_run", dryRun)
			return done, failed, err
		}
		if errors.Is(err, errBlockIndexOwed) {
			s.logger.Info("dispatch: decisions step left an issue for a later pass", "issue", issue, "reason", err, "dry_run", dryRun)
			continue
		}
		if err != nil {
			failed = append(failed, DecisionIssueFailure{Issue: issue, Err: err})
			continue
		}
		s.logger.Info("dispatch: decisions step restored and converted an issue",
			"issue", issue, "restored", len(result.Restored), "declined", len(result.Declined), "decisions", result.Decisions,
			"readings", result.Readings, "dropped", result.Dropped, "removed", result.Removed, "left", result.Left,
			"retired", result.Retired, "spec_approval", result.SpecApproval, "superseded", len(result.Superseded), "dry_run", dryRun)
		for _, restored := range result.Restored {
			s.logger.Info("dispatch: decisions step restored an answered ask",
				"issue", issue, "ask", restored.Ask, "shape", restored.Shape, "to", restored.To, "read", restored.Read, "dry_run", dryRun)
		}
		for _, declined := range result.Declined {
			s.logger.Info("dispatch: decisions step declined to restore an answered ask a person removed",
				"issue", issue, "ask", declined.Ask, "person", declined.Person, "version", declined.Version, "dry_run", dryRun)
		}
		for _, superseded := range result.Superseded {
			s.logger.Info("dispatch: decisions step superseded an approval",
				"issue", issue, "document", superseded.Document, "review", superseded.Approval.ReviewID,
				"approved_version", superseded.Approval.ApprovedVersion, "asks", superseded.Asks, "nodes", superseded.Nodes)
		}
		done = append(done, result)
	}
	return done, failed, nil
}

// restoreIssue runs the third phase on one issue in a transaction of its own, which it commits,
// or, under dryRun, rolls back. The transaction is that one issue's restore and no more: it begins
// and ends here, and no other issue's work and no request runs in it. It first reads the fleet
// (anotherBuildLive) and answers ErrAnotherBuildLive, with nothing written, where a process of
// another build has gone live since the pass began: no write of the phase meets a build that reads
// the documents it moves. Its writes run at once, in the order the restore makes them
// (joinAtOnce), since the restore reads what it wrote earlier in the same transaction: the ask
// rows it pointed at the spec, the versions it wrote before the events that name them, the nodes
// it stated. It logs how long the transaction stood open, from its begin to its commit, or to its
// rollback, which bounds how long it held a transaction id.
//
// kept for one release: decisions-step
func (s *DecisionStep) restoreIssue(ctx context.Context, issue string, work []graphcensus.RestoreWork, convert, dryRun bool) (DecisionIssue, error) {
	began := time.Now()
	tx, err := s.database.Pool.Begin(ctx)
	if err != nil {
		return DecisionIssue{Issue: issue}, fmt.Errorf("begin the restore of %s: %w", issue, err)
	}
	defer tx.Rollback(context.Background())
	if other, err := s.anotherBuildLive(ctx, tx); err != nil || other {
		if err == nil {
			err = ErrAnotherBuildLive
		}
		return DecisionIssue{Issue: issue}, err
	}
	joined, ledger := s.documents.joinAtOnce(ctx, tx)
	defer ledger.Discard()
	result, err := s.documents.restoreIssueDecisions(joined, tx, issue, work, convert)
	if err == nil && !dryRun {
		if err = ledger.Commit(ctx); err != nil {
			err = fmt.Errorf("commit the restore of %s: %w", issue, err)
		}
	}
	s.logger.Info("dispatch: decisions step ended the transaction of an issue's restore",
		"issue", issue, "held", time.Since(began), "committed", err == nil && !dryRun, "dry_run", dryRun)
	return result, err
}

// rewriteGatedSteps takes `decisions` out of each project's list of gated steps that holds it,
// for project or every project, in code rather than by a migration (F6): a list of `decisions`
// alone becomes `[spec]`, so no project is left ungated, and every other list keeps its other
// steps. dryRun rolls it back.
//
// kept for one release: decisions-step
func (s *DecisionStep) rewriteGatedSteps(ctx context.Context, project string, dryRun bool) error {
	tx, err := s.database.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the rewrite of the gated steps: %w", err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `
		update projects set gated_steps = case
			when cardinality(array_remove(gated_steps, 'decisions')) = 0 then array['spec']::text[]
			else array_remove(gated_steps, 'decisions')
		end
		where 'decisions' = any(gated_steps) and ($1 = '' or key = $1)
	`, project); err != nil {
		return fmt.Errorf("rewrite the gated steps without decisions: %w", err)
	}
	if dryRun {
		return nil
	}
	return tx.Commit(ctx)
}

// ErrDryRunOwesBackfill refuses a dry run of the decisions step (RunByHand) over a database whose
// rows the step's first phase has still to state (graphcensus.AsksOwed): that phase commits what it
// states, and a dry run writes nothing.
//
// kept for one release: decisions-step
var ErrDryRunOwesBackfill = errors.New("docs: the decisions step's first phase owes rows the release before wrote, " +
	"which it commits, and a dry run writes nothing; run the step without the dry run, or boot a server of this build, first")

// RunByHand is the step as a command runs it (`envoy-dispatch restore-answered-asks` and
// `convert-decisions`): it waits for the step's lock, so no process's tick runs the first two
// phases beside it, runs phase 1 where a row stands unmarked and phase 2 once, then releases that
// lock and waits for phase 3's (takeRestoreLock), so no tick runs phase 3 beside it and no booting
// process waits behind it, and runs phase 3 over the issues of project (RestoreAndConvert),
// whatever DISPATCH_TEST_HOLD_DECISIONS_STEP says, since that setting holds a server's boot and
// tick alone. Phase 3 still waits for a fleet of the step's own build (ErrAnotherBuildLive).
//
// dryRun writes nothing: it refuses a database phase 1 owes a row of (ErrDryRunOwesBackfill), since
// phase 1 commits, runs no phase 2, which commits each ask it catches up, and runs phase 3 with
// each of its transactions rolled back. A dry run's phase 3 reads an answer another build wrote
// since the last catch-up as the node holds it, as a tick's would before its catch-up. Its context
// is marked for the pool's nested-acquire guard, as Boot's and Tick's are.
//
// kept for one release: decisions-step
func (s *DecisionStep) RunByHand(ctx context.Context, project string, convert, dryRun bool) ([]DecisionIssue, []DecisionIssueFailure, error) {
	ctx = store.WithTransactionTracking(ctx)
	conn, err := s.acquireStepConn(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer conn.release(s.logger)
	if _, err := conn.lock(ctx, decisionStepLock, true); err != nil {
		return nil, nil, fmt.Errorf("take the decision step's lock: %w", err)
	}
	owed, err := graphcensus.AsksOwed(ctx, s.database.Pool)
	if err != nil {
		return nil, nil, err
	}
	if owed && dryRun {
		return nil, nil, ErrDryRunOwesBackfill
	}
	if owed {
		if err := s.backfill(ctx); err != nil {
			return nil, nil, err
		}
	}
	// kept for one release: catch-up
	if !dryRun {
		if failed, err := s.catchUp(ctx); err != nil {
			return nil, nil, err
		} else if failed {
			return nil, nil, errors.New("docs: the decisions step's catch-up failed on an ask; run the command again")
		}
	}
	if _, err := conn.takeRestoreLock(ctx, true); err != nil {
		return nil, nil, err
	}
	return s.RestoreAndConvert(ctx, project, convert, dryRun)
}
