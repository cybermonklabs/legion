package legionfacts

import (
	"testing"
	"time"
)

// TestAClosedIssueThatGainsPhaseWorkOnNoIssueGetsItsRow: a closed issue that gains phase work
// through a session title written on the project, with no event on the issue, must get a stored
// row.
//
// A session's newest title is read from every event it wrote, whichever of an issue, a project
// or an artifact that event owns (delivery.ResolveSessionTitles), so a session writing on the
// project under a closed issue's key gives that issue the work of every event it ever wrote.
// The issue has no event of its own after the write, and no stored row for the sweep to find.
//
// The pass reaches it through the titles arm of the session read: an event on no issue is kept
// where its actor is a session, and the issue the title that event carries names joins the
// pass's work (readEventPage, sessionIssues). The stored-row arm cannot reach this issue,
// because the test asserts it holds no row at that point.
func TestAClosedIssueThatGainsPhaseWorkOnNoIssueGetsItsRow(t *testing.T) {
	run := newConsumer(t, 3)
	run.pass(t)

	gain := ""
	if err := run.database.Pool.QueryRow(t.Context(), `
		select i.key from issues i
		left join issue_facts f on f.issue_key = i.key
		where i.project_key = $1 and i.closed_at is not null and f.issue_key is null
		order by i.key limit 1`, run.fixture.Project).Scan(&gain); err != nil {
		t.Fatalf("no closed issue without a row: %v", err)
	}
	const session = "s-one-role"
	other := run.fixture.PhaseWork[0]
	writeTitledEvent(t, run, session, gain, other, run.at.Add(-3*time.Hour))
	run.at = run.at.Add(time.Minute)
	run.pass(t)
	if _, has := run.rows(t)[gain]; has {
		t.Fatalf("%s has a row before the retitle, so this test examined nothing", gain)
	}
	writeProjectTitledEvent(t, run, session, gain, run.at)
	run.at = run.at.Add(time.Minute)
	run.pass(t)
	run.at = run.at.Add(time.Minute)
	run.pass(t)
	fresh, err := Compute(t.Context(), run.database.Pool, run.fixture.Project, []string{gain}, run.at)
	if err != nil {
		t.Fatalf("compute %s on its own: %v", gain, err)
	}
	_, has := run.rows(t)[gain]
	t.Logf("%s: computation answers %d row(s); stored row present=%t", gain, len(fresh), has)
	if len(fresh) != 1 {
		t.Fatalf("the computation answers %d rows for closed %s, want the one row the title it gained earns it, so this test examined nothing",
			len(fresh), gain)
	}
	if !has {
		t.Errorf("closed %s carries Legion phase work by the computation and has no stored row", gain)
	}
}
