package legionfacts

import (
	"testing"
	"time"
)

// TestAPassAfterTheEventLogIsResetReadsTheEventsTheNewLogWrites: e2e/seed.ts resets the event log
// between browser scenarios: it deletes the log's rows with the facts' own tables, cursors among
// them, and starts the log's ids again at 1. The shared horizon still holds the old log's H then,
// since it only grows until a reader forgets it (events.Horizon.Reset), and the relay forgets it
// only on its first read after a new event. The first pass after the reset finds no cursor row and
// reads the feeds from their heads (readFeedHeads), and the events feed's head is the safe horizon.
// A pass that took the old log's H there would leave its events cursor above every id the new log
// hands out until the new log climbed back past it, and the events below it would never be read.
// So the pass forgets a horizon that stands above the log's newest event and reads it again, as
// the keyword fill does: its cursor stands in the new log, and the next pass reads the new log's
// first event.
func TestAPassAfterTheEventLogIsResetReadsTheEventsTheNewLogWrites(t *testing.T) {
	run := newConsumer(t, 1)
	run.pass(t)
	old := run.cursors(t)[eventsCursor].position
	if old == 0 {
		t.Fatalf("the first pass leaves the events cursor at 0, so the old log gives the horizon no id to stand above the new log's")
	}

	run.exec(t, "reset the event log as e2e/seed.ts does", `
		delete from events;
		delete from session_titles;
		delete from issue_facts;
		delete from issue_facts_cursor;
		delete from issue_facts_computed;
		delete from issue_filings;
		alter sequence events_id_seq restart;
	`)
	run.at = run.at.Add(time.Minute)
	run.pass(t)
	if at, newest := run.cursors(t)[eventsCursor].position, run.head(t, `select coalesce(max(id), 0) from events`); at > newest {
		t.Errorf("the first pass after the reset leaves the events cursor at %d, above the reset log's newest event %d: it took the old log's horizon for the new log's head", at, newest)
	}

	run.at = run.at.Add(time.Minute)
	run.writeEvent(t, run.fixture.PhaseWork[0], "implementer", "comment.created", `{}`, run.at.Add(-time.Second))
	written := run.head(t, `select max(id) from events`)
	run.pass(t)
	if at := run.cursors(t)[eventsCursor].position; at != written {
		t.Errorf("the pass after the new log's first event %d leaves the events cursor at %d, so it never read that event (the old log's cursor stood at %d)", written, at, old)
	}
}
