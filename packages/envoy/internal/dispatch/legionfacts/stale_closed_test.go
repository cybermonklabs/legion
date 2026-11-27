package legionfacts

import (
	"testing"
	"time"
)

// TestAClosedIssueThatLosesItsPhaseWorkLosesItsStoredRow: the deletion of a stale row reaches a
// closed issue as it reaches an open one.
//
// A pass's stated set is what its feeds named, the issues above and below those issues and the
// trees a moved parent link touched, and no pass computes a row its feeds did not name. An issue
// a role session left has no event of its own, so the row of a closed issue whose session moved
// away would stand for ever, and the table would go on saying Legion worked an issue it did not.
//
// TestAnIssueThatLosesItsPhaseWorkLosesItsStoredRow states the same rule where the issue is open.
// The pass reads the issue a session left from what the table holds rather than from the links
// (sessionIssues' stored-row arm), as the re-parenting reads the tree a moved issue left
// (leftTrees), and that issue joins the stated set: the first pass after the session's move takes
// the closed issue's row, and the two after it leave it gone.
func TestAClosedIssueThatLosesItsPhaseWorkLosesItsStoredRow(t *testing.T) {
	run := newConsumer(t, 3)
	run.pass(t)

	closed := run.closedWithNoRow(t)
	other := run.fixture.PhaseWork[0]
	const session = "s-one-role"

	// The session's first event, on the closed issue its own title names, gives that issue
	// Legion phase work, and a pass writes its row.
	writeRoleEvent(t, run.history, session, closed, run.at.Add(-2*time.Hour))
	run.at = run.at.Add(time.Minute)
	run.pass(t)
	if _, has := run.rows(t)[closed]; !has {
		t.Fatalf("%s carries Legion phase work and has no stored row, so this test examined nothing", closed)
	}

	// The same session writes next on another issue under that issue's title, so the work goes
	// with it and the closed issue carries none.
	writeRoleEvent(t, run.history, session, other, run.at.Add(-time.Hour))
	computed, _, err := ComputeProject(t.Context(), run.database.Pool, run.fixture.Project, run.at)
	if err != nil {
		t.Fatalf("compute the project's facts: %v", err)
	}
	for _, row := range computed {
		if row.Issue == closed {
			t.Fatalf("%s still carries Legion phase work after its session moved, so this test examined nothing", closed)
		}
	}

	// Three passes, the first of them reading the session's event: the row goes on the first, and
	// stays gone on the two after it.
	for pass := range 3 {
		run.at = run.at.Add(time.Minute)
		run.pass(t)
		if stored := run.rows(t); stored[closed].Issue != "" {
			t.Errorf("%s is closed, carries no Legion phase work and keeps its stored row after pass %d of the three, the first of which read its session's event: the computation answers %d issues and the table holds %d rows",
				closed, pass+1, len(computed), len(stored))
		}
	}
}

// closedWithNoRow is the first closed issue of the project, by key, that holds no stored row: a
// closed issue whose row only the Legion phase work a test writes for it can bring in.
func (c *consumer) closedWithNoRow(t *testing.T) string {
	t.Helper()
	closed := ""
	if err := c.database.Pool.QueryRow(t.Context(), `
		select i.key from issues i
		left join issue_facts f on f.issue_key = i.key
		where i.project_key = $1 and i.closed_at is not null and f.issue_key is null
		order by i.key limit 1`, c.fixture.Project).Scan(&closed); err != nil {
		t.Fatalf("find a closed issue of %s with no stored row: %v", c.fixture.Project, err)
	}
	return closed
}
