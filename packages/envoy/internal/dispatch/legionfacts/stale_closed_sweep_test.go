package legionfacts

import (
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
)

// TestASweepTakesTheRowOfAClosedIssueWhoseSessionLeftAndKeepsTheWorkedOnes: the sweep is the one
// reader that can find the stored row of a closed issue whose role session moved away, and it
// takes that row alone.
//
// Nothing else a pass reads names such an issue. The feeds name the issues their own rows name,
// the links reach those issues' ancestors and the trees a moved parent link touched, and the
// sweep's open rows are the rows of the open issues. A closed issue whose session was renamed
// is in none of the three: the rename is written somewhere else, and the issue is closed. Its
// row therefore stood for ever and the table went on saying Legion worked an issue it did not.
// staleStoredRows is the fourth source, read from the table beside them, and this test fails
// without it: the row stands and the counts below answer 0 deleted.
//
// The rename is a write on the project rather than on an issue, which is what makes the counts
// exact. An event that names no issue is read by no feed of the pass (readEventPage joins the
// issues), so the sweep is the only thing that can name a row of this project, and the clock
// stands still over the measured pass, so an open issue's hours stand still with it.
//
// The second half is the closed issue that still carries Legion phase work. Its row stands,
// unchanged: a read that took every closed row, rather than the rows the phase-work set no
// longer holds, would take that one too.
func TestASweepTakesTheRowOfAClosedIssueWhoseSessionLeftAndKeepsTheWorkedOnes(t *testing.T) {
	run := newConsumer(t, 3)
	run.pass(t)

	left := ""
	if err := run.database.Pool.QueryRow(t.Context(), `
		select i.key from issues i
		left join issue_facts f on f.issue_key = i.key
		where i.project_key = $1 and i.closed_at is not null and f.issue_key is null
		order by i.key limit 1`, run.fixture.Project).Scan(&left); err != nil {
		t.Fatalf("find a closed issue of %s with no stored row: %v", run.fixture.Project, err)
	}
	const session = "s-one-role"

	// The session's first event, on the closed issue its own title names, gives that issue
	// Legion phase work, and the next pass writes its row.
	writeSessionEvent(t, run, session, left, run.at.Add(-2*time.Hour))
	run.at = run.at.Add(time.Minute)
	run.pass(t)
	if _, has := run.rows(t)[left]; !has {
		t.Fatalf("%s carries Legion phase work and has no stored row, so this test examined nothing", left)
	}

	// The same session writes next on the project, under another issue's title. Its newest
	// title is what the rule reads, so the work goes with it and the closed issue carries none.
	writeProjectTitledEvent(t, run, session, run.fixture.PhaseWork[0], run.at)
	before := run.rows(t)
	found, err := staleStoredRows(t.Context(), run.database.Pool)
	if err != nil {
		t.Fatalf("read the stored rows whose issue lost its Legion phase work: %v", err)
	}
	stale := found[run.fixture.Project]
	slices.Sort(stale)
	if !slices.Contains(stale, left) {
		t.Fatalf("the sweep found the stale rows %v, which does not hold %s: this test examined nothing", stale, left)
	}
	stated := statedSet(t, run, stale, nil)

	// The row that must stand: a closed issue that still carries Legion phase work, outside the
	// set the stale rows state, so the pass computes it at all only if it deletes beyond its set.
	work, err := legion.PhaseWorkKeys(t.Context(), run.database.Pool)
	if err != nil {
		t.Fatalf("read the issues that carry Legion phase work: %v", err)
	}
	kept := ""
	for _, key := range slices.Sorted(maps.Keys(before)) {
		_, works := work.Keys[key]
		if before[key].ClosedAt != nil && works && !slices.Contains(stated, key) {
			kept = key
			break
		}
	}
	if kept == "" {
		t.Fatalf("no closed issue of %s has a stored row, carries Legion phase work and stands outside %v: the half of this test that watches such a row examined nothing",
			run.fixture.Project, stated)
	}

	changed := run.pass(t)
	after := run.rows(t)
	deleted := deletedBetween(before, after)
	moved := movedOutside(before, after, stated)

	if _, stands := after[left]; stands {
		t.Errorf("%s is closed, carries no Legion phase work and keeps its stored row after a sweep: the table holds %d rows",
			left, len(after))
	}
	if !slices.Equal(deleted, []string{left}) {
		t.Errorf("the sweep deleted %v, want %s alone: the one stale row it found", deleted, left)
	}
	if !slices.Contains(changed, left) {
		t.Errorf("the pass deleted %s and answered the changed keys %v, which does not hold it: a row that went is a change",
			left, changed)
	}
	stood, stands := after[kept]
	switch {
	case !stands:
		t.Errorf("%s is closed and still carries Legion phase work, and the sweep deleted its row: %s", kept, show(before[kept]))
	case !sameRow(stood, before[kept]):
		t.Errorf("%s is closed and still carries Legion phase work, and the sweep rewrote its row as\n\tgot  %s\n\twant %s",
			kept, show(stood), show(before[kept]))
	}
	if len(moved) > 0 {
		t.Errorf("%d rows outside the stale rows and the rows those keys state are no longer the rows they were: %v",
			len(moved), moved)
	}
	t.Logf("the sweep found %d stale row(s) (%v) and deleted %d of them (%v); those keys state %d rows (%v), and %d row(s) outside them changed; the closed %s carries Legion phase work still and its row stands",
		len(stale), stale, len(deleted), deleted, len(stated), stated, len(moved), kept)
}

// writeProjectTitledEvent writes one event of a session on the project rather than on an issue,
// under a title that names the issue titled. An event owns one of an issue, an artifact or a
// project (the events table's own check), and a session's newest title is read from every event
// it wrote, whichever of the three that event owns (delivery.ResolveSessionTitles). So this is
// how a session takes its Legion phase work away from an issue while no feed of the facts' pass
// names any issue at all.
func writeProjectTitledEvent(t *testing.T, run *consumer, session, titled string, at time.Time) {
	t.Helper()
	run.exec(t, "write session "+session+"'s event on the project", `
		insert into events (project_key, seq, type, actor, payload, notify, created_at)
		values ($1, (select coalesce(max(seq), 0) + 1 from events where project_key = $1),
		        'comment.created',
		        jsonb_build_object('kind', 'session', 'id', $2::text,
		          'origin', jsonb_build_object('session_title', 'Legion implementer · ' || $3)),
		        '{}'::jsonb, false, $4)
	`, run.fixture.Project, session, titled, at)
}
