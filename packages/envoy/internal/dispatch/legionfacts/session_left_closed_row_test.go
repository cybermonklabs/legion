package legionfacts

import (
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
)

// TestASessionsEventOnTheProjectTakesTheRowOfAClosedIssueItLeftAndKeepsTheWorkedOnes: a role
// session that writes on the project under another issue's title takes its Legion phase work off
// the closed issue it worked, and the pass that reads that event takes that issue's stored row
// and no worked row.
//
// The event names no issue, and the issue it takes the work from is closed, so neither the
// event's own issue nor the clock can bring that row into a pass. The event's actor can: a page
// keeps an event on no issue where its actor is a session, and states every issue that session
// wrote on that holds a stored row and the issue its title names (readEventPage, sessionIssues).
// That is the one way the row goes, since no pass computes a row its feeds did not name, and this
// test fails without it: the row stands and the counts below answer 0 deleted.
//
// The second half is the closed issue that still carries Legion phase work. Its row stands,
// unchanged: a pass that deleted every closed row it could not name, rather than the rows its
// computation answered none for inside the set it stated, would take that one too.
func TestASessionsEventOnTheProjectTakesTheRowOfAClosedIssueItLeftAndKeepsTheWorkedOnes(t *testing.T) {
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
	titled := run.fixture.PhaseWork[0]
	writeProjectTitledEvent(t, run, session, titled, run.at)
	before := run.rows(t)
	stated := statedSet(t, run, []string{left, titled}, nil)

	// The row that must stand: a closed issue that still carries Legion phase work, outside the
	// set the event states. It must stand either way, because the issue carries Legion phase
	// work and the computation answers a row for it.
	work, err := legion.PhaseWorkKeys(t.Context(), run.database.Pool)
	if err != nil {
		t.Fatalf("read the issues that carry Legion phase work: %v", err)
	}
	kept := ""
	for _, key := range slices.Sorted(maps.Keys(before)) {
		_, works := work[key]
		if before[key].ClosedAt != nil && works && !slices.Contains(stated, key) {
			kept = key
			break
		}
	}
	if kept == "" {
		t.Fatalf("no closed issue of %s has a stored row, carries Legion phase work and stands outside %v: the half of this test that watches such a row examined nothing",
			run.fixture.Project, stated)
	}

	run.at = run.at.Add(time.Minute)
	changed := run.pass(t)
	after := run.rows(t)
	deleted := deletedBetween(before, after)
	moved := movedOutside(before, after, stated)

	if _, stands := after[left]; stands {
		t.Errorf("%s is closed, carries no Legion phase work and keeps its stored row after the pass that read its session's event: the table holds %d rows",
			left, len(after))
	}
	if !slices.Equal(deleted, []string{left}) {
		t.Errorf("the pass deleted %v, want %s alone: the one row whose issue lost its work", deleted, left)
	}
	if !slices.Contains(changed, left) {
		t.Errorf("the pass deleted %s and answered the changed keys %v, which does not hold it: a row that went is a change",
			left, changed)
	}
	stood, stands := after[kept]
	switch {
	case !stands:
		t.Errorf("%s is closed and still carries Legion phase work, and the pass deleted its row: %s", kept, show(before[kept]))
	case !sameRow(stood, before[kept]):
		t.Errorf("%s is closed and still carries Legion phase work, and the pass rewrote its row as\n\tgot  %s\n\twant %s",
			kept, show(stood), show(before[kept]))
	}
	if len(moved) > 0 {
		t.Errorf("%d rows outside the set the event states are no longer the rows they were: %v",
			len(moved), moved)
	}
	t.Logf("the session's event on the project states %d rows (%v); the pass deleted %d of them (%v), and %d row(s) outside them changed; the closed %s carries Legion phase work still and its row stands",
		len(stated), stated, len(deleted), deleted, len(moved), kept)
}

// writeProjectTitledEvent writes one event of a session on the project rather than on an issue,
// under a title that names the issue titled. An event owns one of an issue, an artifact or a
// project (the events table's own check), and a session's newest title is read from every event
// it wrote, whichever of the three that event owns (delivery.ResolveSessionTitles). So this is
// how a session takes its Legion phase work away from an issue while no event names that issue.
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
