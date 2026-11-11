package legionfacts

import (
	"testing"
	"time"
)

// TestAClosedIssueThatLosesItsPhaseWorkLosesItsStoredRow: the deletion of a stale row reaches an
// open issue and not a closed one, so half of the defect it was written for still stands.
//
// A pass's stated set is what its feeds named, those issues' ancestors, the trees a moved parent
// link touched, and the sweep's own addition, which is the project's open *stored* rows
// (openStoredRows). An issue a role session left is named by none of those: no event was written
// on it, and the sweep reads it only while it is open. So the row of a closed issue whose session
// moved away stands for ever, and the table goes on saying Legion worked an issue it did not.
//
// Measured against a real Dispatch server of this head on 2026-11-11: one session wrote a comment
// on the closed LEGION-102 under that issue's title and its row appeared after 21 s; the same
// session then wrote on the open LEGION-10 under its title, and over 151 s and five sweeps
// LEGION-102's row did not go. The independent reading of R1 held 210 issues against the table's
// 211, the extra one being LEGION-102. The open half of the same drive passed: WIDGETS-1's row
// went, and no row outside the set went with it.
//
// TestAnIssueThatLosesItsPhaseWorkLosesItsStoredRow states the same rule where the issue is open.
// The fix has the shape the re-parenting already uses (leftTrees): the issue a session left is
// read from what the table holds rather than from the links, and joins the stated set.
func TestAClosedIssueThatLosesItsPhaseWorkLosesItsStoredRow(t *testing.T) {
	run := newConsumer(t, 3)
	run.pass(t)

	closed := ""
	if err := run.database.Pool.QueryRow(t.Context(), `
		select i.key from issues i
		left join issue_facts f on f.issue_key = i.key
		where i.project_key = $1 and i.closed_at is not null and f.issue_key is null
		order by i.key limit 1`, run.fixture.Project).Scan(&closed); err != nil {
		t.Fatalf("find a closed issue of %s with no stored row: %v", run.fixture.Project, err)
	}
	other := run.fixture.PhaseWork[0]
	const session = "s-one-role"

	// The session's first event, on the closed issue its own title names, gives that issue
	// Legion phase work, and a pass writes its row.
	writeSessionEvent(t, run, session, closed, run.at.Add(-2*time.Hour))
	run.at = run.at.Add(time.Minute)
	run.pass(t)
	if _, has := run.rows(t)[closed]; !has {
		t.Fatalf("%s carries Legion phase work and has no stored row, so this test examined nothing", closed)
	}

	// The same session writes next on another issue under that issue's title, so the work goes
	// with it and the closed issue carries none.
	writeSessionEvent(t, run, session, other, run.at.Add(-time.Hour))
	computed, err := ComputeProject(t.Context(), run.database.Pool, run.fixture.Project, run.at)
	if err != nil {
		t.Fatalf("compute the project's facts: %v", err)
	}
	for _, row := range computed {
		if row.Issue == closed {
			t.Fatalf("%s still carries Legion phase work after its session moved, so this test examined nothing", closed)
		}
	}

	// Three passes, each a sweep of its own, which is what the server runs every half minute.
	for range 3 {
		run.at = run.at.Add(time.Minute)
		run.pass(t)
	}
	stored := run.rows(t)
	if _, has := stored[closed]; has {
		t.Errorf("%s is closed, carries no Legion phase work and keeps its stored row after three passes: the computation answers %d issues and the table holds %d rows",
			closed, len(computed), len(stored))
	}
}

// writeSessionEvent writes one event of a named session on an issue, under a title that names
// that issue. The moment matters: a session's newest title is its newest titled event by
// created_at (delivery.ResolveSessionTitles), so two events of one session at one moment leave
// which title is newest undecided.
func writeSessionEvent(t *testing.T, run *consumer, session, issue string, at time.Time) {
	t.Helper()
	run.exec(t, "write session "+session+"'s event on "+issue, `
		insert into events (issue_key, seq, type, actor, payload, notify, created_at)
		values ($1, (select coalesce(max(seq), 0) + 1 from events where issue_key = $1),
		        'comment.created',
		        jsonb_build_object('kind', 'session', 'id', $2::text,
		          'origin', jsonb_build_object('session_title', 'Legion implementer · ' || $1)),
		        '{}'::jsonb, false, $3)`, issue, session, at)
}
