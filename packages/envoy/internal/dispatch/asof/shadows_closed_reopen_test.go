package asof_test

import (
	"context"
	"testing"
	"time"
)

// A read at a cut answers what the read answered then, whatever is written after the cut. A close
// stamps the row's closed_at again as its transaction ends (clock_timestamp, api/issue_patch.go).
// The issue.closed of a close written before INFRA-4620 holds the first stamp, the transaction's
// start, and the issue.closed of a later close holds the row's final stamp. The test holds for
// both: it writes a close of the first kind, whose event and row differ, and a close of the second
// kind holds one stamp in both. A reopen written straight after the close sets the row's
// closed_at to null. So the same cut, after the close and before the reopen, is read twice here:
// once before the reopen is written, where the row still holds the second stamp, and once after
// it. Both reads are of the same past, so they must answer the same closed_at, whichever value
// the shadow chooses (or none, with the member named absent through the API).
func TestTheShadowClosedAtAtACutDoesNotMoveWhenTheIssueIsReopenedLater(t *testing.T) {
	w := newWorld(t)
	made := when(t, "2026-11-10T09:00:00Z")
	closedAt := when(t, "2026-11-10T10:00:00Z")
	stamped := closedAt.Add(250 * time.Millisecond)
	cut := "2026-11-10T11:00:00Z"
	w.project("ASOF", "carol", made.Add(-time.Hour))

	issue := newIssue("ASOF-1", "ASOF", 1, "Closed, then reopened", made)
	present := issue
	present.Status, present.ClosedAt = "done", &stamped
	w.insertIssue(present)
	w.issueEvent("issue.created", issue, made)
	closed := issue
	closed.Status, closed.ClosedAt = "done", &closedAt
	w.issueEvent("issue.closed", closed, closedAt)

	closedAtTheCut := func(t *testing.T) *time.Time {
		t.Helper()
		var got *time.Time
		if err := w.database.Pool.QueryRow(atCut(t, cut), `select closed_at from issues where key = $1`, issue.Key).Scan(&got); err != nil {
			t.Fatalf("read the issue at the cut: %v", err)
		}
		return got
	}
	beforeReopen := closedAtTheCut(t)

	// The reopen as api/issue_patch.go writes it: the row's status and its closed_at nulled, and
	// an issue.updated whose payload is the reopened issue.
	if _, err := w.database.Pool.Exec(context.Background(),
		`update issues set status = 'todo', closed_at = null where key = $1`, issue.Key); err != nil {
		t.Fatalf("reopen the issue's row: %v", err)
	}
	reopened := issue
	reopened.Status = "todo"
	w.issueEvent("issue.updated", reopened, when(t, "2026-11-10T12:00:00Z"))

	afterReopen := closedAtTheCut(t)
	if !sameTime(beforeReopen, afterReopen) {
		t.Errorf("closed_at at the cut %s reads %v before a later reopen is written and %v after it: one cut, two answers", cut, beforeReopen, afterReopen)
	}
}
