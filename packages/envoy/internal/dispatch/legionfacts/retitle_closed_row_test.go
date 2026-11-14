package legionfacts

import (
	"maps"
	"slices"
	"testing"
	"time"
)

// TestARetitleThatTakesWorkFromAClosedIssueRewritesItsRow: a closed issue that keeps its phase
// work (a handoff) but loses one session's role events to a retitle must not keep a stored row
// the computation no longer answers.
//
// Which role events count for an issue is decided by each session's newest title, which the
// metrics read when the facts are computed (legion/metrics), so a session that writes anywhere
// under another issue's title takes its own events off the issue it worked. The issue keeps its
// row, because its handoff still carries phase work, and the row's hours are no longer the
// hours the computation answers. No event is written on that issue, and the sweep reads the
// open rows and the rows that lost their phase work, so neither names it.
//
// The pass reaches it through the stored-row arm of the session read: a page that names a
// session states every issue that session wrote an event on and that holds a stored row
// (readEventPage, sessionIssues). This issue holds one, because its handoff earned it.
func TestARetitleThatTakesWorkFromAClosedIssueRewritesItsRow(t *testing.T) {
	run := newConsumer(t, 3)
	run.pass(t)
	rows := run.rows(t)

	handed := map[string]bool{}
	hrows, err := run.database.Pool.Query(t.Context(), `select distinct issue_key from handoffs`)
	if err != nil {
		t.Fatalf("read the issues a handoff names: %v", err)
	}
	for hrows.Next() {
		var key string
		if err := hrows.Scan(&key); err != nil {
			t.Fatalf("scan an issue a handoff names: %v", err)
		}
		handed[key] = true
	}
	hrows.Close()
	if err := hrows.Err(); err != nil {
		t.Fatalf("read the issues a handoff names: %v", err)
	}

	target := ""
	for _, key := range slices.Sorted(maps.Keys(rows)) {
		row := rows[key]
		if row.ClosedAt != nil && handed[key] && row.WorkFirstAt != nil && row.WorkFirstAt.Sub(row.OpenedAt) > 2*time.Hour {
			target = key
			break
		}
	}
	if target == "" {
		t.Fatal("no closed handed-off issue whose work starts 2h after it opened")
	}
	before := rows[target]
	const session = "s-one-role"
	writeSessionEvent(t, run, session, target, before.OpenedAt.Add(time.Minute))
	run.at = run.at.Add(time.Minute)
	run.pass(t)
	gained := run.rows(t)[target]
	t.Logf("%s: work_hours before=%g after the session's event=%g", target, before.WorkHours, gained.WorkHours)

	other := run.fixture.PhaseWork[0]
	if other == target {
		other = run.fixture.PhaseWork[1]
	}
	writeProjectTitledEvent(t, run, session, other, run.at)
	run.at = run.at.Add(time.Minute)
	run.pass(t)
	run.at = run.at.Add(time.Minute)
	run.pass(t)
	stored, has := run.rows(t)[target]
	if !has {
		t.Fatalf("%s lost its row", target)
	}
	fresh, _, err := computeKeys(t.Context(), run.database.Pool, run.fixture.Project, []string{target}, nil, run.at)
	if err != nil {
		t.Fatalf("compute %s on its own: %v", target, err)
	}
	if len(fresh) != 1 {
		t.Fatalf("the computation answers %d rows for %s, want the one row its handoff earns it, so this test examined nothing",
			len(fresh), target)
	}
	t.Logf("%s after retitle and two passes: stored work_hours=%g first=%v; computed now work_hours=%g first=%v",
		target, stored.WorkHours, stored.WorkFirstAt, fresh[0].WorkHours, fresh[0].WorkFirstAt)
	if fresh[0].WorkHours == gained.WorkHours {
		t.Fatalf("%s holds %g work hours with the session's events and the same after the retitle took them, so this test examined nothing",
			target, gained.WorkHours)
	}
	if stored.WorkHours != fresh[0].WorkHours {
		t.Errorf("the stored row of closed %s says %g work hours and the computation says %g",
			target, stored.WorkHours, fresh[0].WorkHours)
	}
}
