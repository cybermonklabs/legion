package api

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// awaitCheckStatesSettled is the fence of a test handoff: it reads the ids of issue's marks that
// are due before the handoff's transaction opens, and of each mark a settle kept in the wait
// (check_states_owed.reads_from), waits until the settler has deleted every one of them, reading
// every fencePoll for at most fenceWait, and answers the issue's serial then. A settle deletes a
// mark in the transaction that appends the mark's events, so each event the tester's results
// caused has a serial at or below the answer. A kept mark is not due until the next settle's turn,
// or until its backoff ends, and it holds the handoff all the same: the state published at a head
// whose read of GitHub's check runs has not started since the mark's change may be from before
// it, a pass of a run that has since failed, and nothing published says so; and while a read of
// which commits are a pull request's is unresolved, failed for a cause a retry can change or left
// to another settle, a candidate is no former head, so nothing published replaces the state a
// reader holds at that commit. A read no retry answers (no App key, no installation, a token
// GitHub refuses the repository) keeps no mark, so it never holds the handoff. The settle that has
// read each such head, or resolved each such read, deletes the mark. A settle keeps each mark it
// read for as long as such a read is unresolved, not one in the others' place: a mark can commit
// after this read of the ids, with an id lower than theirs, and this wait reads only the ids it
// read here. A mark that waits for its retry after a failed read of check runs is not due and not
// kept, so that outage does not hold the handoff: its state reads read_failed with the CI case
// github_unreadable, on which Legion's daemon's testing move waits, as on every CI job whose end
// Dispatch could not read, until the retry's settle publishes the job's end. So the wait for
// GitHub is the testing move's, not the handoff's. Past fenceWait it refuses with 503
// CHECK_STATES_UNSETTLED, and the caller writes the handoff again.
func (s *server) awaitCheckStatesSettled(ctx context.Context, issue string) (serial int64, err error) {
	pool, err := s.readyPool()
	if err != nil {
		return 0, err
	}
	schedule := s.checkStatesSchedule
	var marks []int64
	if err := pool.QueryRow(ctx, `
		select coalesce(array_agg(id), '{}') from check_states_owed
		where issue_key = $1 and (due_at <= now() or reads_from is not null)`, issue).Scan(&marks); err != nil {
		return 0, fmt.Errorf("read the check states marks of %s the handoff waits for: %w", issue, err)
	}
	deadline := time.Now().Add(schedule.fenceWait)
	waiting := false
	for len(marks) > 0 {
		var left bool
		if err := pool.QueryRow(ctx, `select exists (select 1 from check_states_owed where id = any($1))`, marks).Scan(&left); err != nil {
			return 0, fmt.Errorf("read the check states marks of %s left: %w", issue, err)
		}
		if !left {
			break
		}
		if !waiting {
			waiting = true
			if s.checkStatesFenceWaits != nil {
				s.checkStatesFenceWaits(issue)
			}
		}
		if !time.Now().Before(deadline) {
			return 0, errorf(http.StatusServiceUnavailable, "CHECK_STATES_UNSETTLED",
				"the check states the results of %s changed are not published yet after %s: write the handoff again", issue, schedule.fenceWait)
		}
		timer := time.NewTimer(schedule.fencePoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return 0, ctx.Err()
		case <-timer.C:
		}
	}
	if err := pool.QueryRow(ctx, `select coalesce((select serial from check_state_serials where issue_key = $1), 0)`, issue).Scan(&serial); err != nil {
		return 0, fmt.Errorf("read the check states serial of %s: %w", issue, err)
	}
	return serial, nil
}
