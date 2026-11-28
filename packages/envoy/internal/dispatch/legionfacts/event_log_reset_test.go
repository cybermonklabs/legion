package legionfacts

import (
	"testing"
	"time"
)

// TestAnEventLogResetUnderTheEventsCursorIsReadFromItsFirstEvent: an event log reset and refilled
// below where the events cursor stands, as e2e/seed.ts resets it between browser scenarios
// (deleting its rows and restarting its ids, as `truncate events restart identity` does, with the
// stored facts, their records and the cursors emptied beside it), is read from its first event,
// as every other reader of the log by a global id reads it (events.EventLogWasReset). After the
// reset a pass reads the project whole over the empty log and stores no row; an event a Legion
// session then writes on one of its issues, the new log's first, is phase work of that issue, and
// the pass after it stores the issue's row.
//
// Two ways leave the events feed standing at the old log's head: the cursor row a pass that ran
// across the reset wrote back once the reset had committed (kept), and, with every cursor row
// gone, the loop's horizon, which names the old log's newest event until a reader forgets it
// (events.Horizon.Reset), and which a database no pass has committed on starts the feed at
// (readFeedHeads). Red where the loop never checks its cursor and its horizon against the log: the
// feed reads only the events above the old head, and the issue's event is below it.
func TestAnEventLogResetUnderTheEventsCursorIsReadFromItsFirstEvent(t *testing.T) {
	for _, kept := range []bool{true, false} {
		name := "no cursor row"
		if kept {
			name = "the cursor row a pass across the reset wrote"
		}
		t.Run(name, func(t *testing.T) {
			run := newConsumer(t, 1)
			run.pass(t)
			old := run.cursors(t)[eventsCursor].position
			if old < 2 {
				t.Fatalf("the events cursor stands at %d, so a reset log's first event is not below it", old)
			}

			run.exec(t, "reset the event log", `truncate events restart identity cascade`)
			// The reset takes every row a scenario wrote: the handoffs, the other phase work the
			// history holds, the stored facts and their records among them.
			run.exec(t, "empty the handoffs, the stored facts and their records", `truncate handoffs, issue_facts, issue_facts_computed`)
			if !kept {
				run.exec(t, "empty the cursors", `truncate issue_facts_cursor`)
			}
			run.pass(t)
			if stored := run.rows(t); len(stored) != 0 {
				t.Fatalf("the pass over the reset, empty log stored %d rows, want none", len(stored))
			}

			issue := run.fixture.PhaseWork[0]
			run.writeEvent(t, issue, "implementer", "message.created", `{}`, run.at.Add(-time.Hour))
			run.pass(t)
			if _, stored := run.rows(t)[issue]; !stored {
				t.Errorf("%s holds no row after its event, the reset log's first, though the events cursor stood at %d before the reset and stands at %d now",
					issue, old, run.cursors(t)[eventsCursor].position)
			}
			if at := run.cursors(t)[eventsCursor].position; at != run.head(t, `select coalesce(max(id), 0) from events`) {
				t.Errorf("the events cursor stands at %d, want the reset log's head", at)
			}
		})
	}
}
