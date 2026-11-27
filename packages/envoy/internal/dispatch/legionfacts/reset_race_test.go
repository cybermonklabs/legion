package legionfacts

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/testwait"
)

// resetStatements are the statements of e2e/seed.ts's reset that matter to the facts consumer: it
// deletes the rows of the event log, the Legion observations and every table of the facts, the
// cursors among them, and starts the ids of both logs again at 1. It deletes rather than truncates,
// and in one transaction.
var resetStatements = []string{
	`delete from events`,
	`delete from session_titles`,
	`delete from legion_observations`,
	`delete from issue_facts`,
	`delete from issue_facts_cursor`,
	`delete from issue_facts_computed`,
	`delete from issue_filings`,
	`alter sequence events_id_seq restart`,
	`alter sequence legion_observations_id_seq restart`,
}

// read is the first half of the pass's one page (readPage), over the horizon the pass reads, as
// the pass reads it: what a page that has read the feeds and computed its rows holds before its
// write begins. The test puts a reset between the two halves.
func (c *consumer) read(t *testing.T) pageRead {
	t.Helper()
	if _, err := c.deps.Horizon.Await(t.Context()); err != nil {
		t.Fatalf("await the event log's safe horizon: %v", err)
	}
	safe, err := safeHorizon(t.Context(), c.deps)
	if err != nil {
		t.Fatalf("read the event log's safe horizon: %v", err)
	}
	page, err := readPage(t.Context(), c.deps, c.sched, safe, c.sched.now(), true)
	if err != nil {
		t.Fatalf("read a page of the feeds: %v", err)
	}
	return page
}

// requireNoCursorAboveItsLog fails t for a cursor row that stands above the newest id of the log it
// pages, which no page of the log would ever read below: the state a reset leaves behind when a
// pass writes the old log's positions into it.
func (c *consumer) requireNoCursorAboveItsLog(t *testing.T, when string) {
	t.Helper()
	heads := c.feedHeads(t)
	for _, name := range []string{eventsCursor, observationsCursor} {
		if at, has := c.cursors(t)[name]; has && at.position > heads[name] {
			t.Errorf("%s the %s cursor stands at %d, above the reset log's newest id %d, so the new log's rows below it are never read",
				when, name, at.position, heads[name])
		}
	}
}

// requireTheResetLogsAreRead writes the first event and the first observation of the reset logs,
// the ids the sequences hand out again from 1, and requires the next pass to read both: each of
// its cursors then stands at the id its log's first row took.
func (c *consumer) requireTheResetLogsAreRead(t *testing.T) {
	t.Helper()
	issue := c.fixture.PhaseWork[0]
	c.at = c.at.Add(time.Minute)
	c.writeEvent(t, issue, "implementer", "comment.created", `{}`, c.at.Add(-2*time.Second))
	c.observe(t, issue, legion.FieldPhase, legion.PhaseTesting, c.at.Add(-time.Second))
	heads := c.feedHeads(t)
	if heads[eventsCursor] != 1 || heads[observationsCursor] != 1 {
		t.Fatalf("the reset logs' first rows took the ids %d and %d, want 1 and 1: the sequences did not restart",
			heads[eventsCursor], heads[observationsCursor])
	}
	c.pass(t)
	cursors := c.cursors(t)
	for _, name := range []string{eventsCursor, observationsCursor} {
		if cursors[name].position != 1 {
			t.Errorf("the pass after the new log's first row leaves the %s cursor at %d, want 1: it never read that row", name, cursors[name].position)
		}
	}
}

// TestAPassThatStraddlesAResetWritesNothingAndTheNextPassReadsTheNewLogs: a pass reads its
// cursors and its feeds and computes its rows before its write's transaction opens, and
// e2e/seed.ts's reset can commit between the two. The pass's write would then put the old logs'
// positions into issue_facts_cursor, above every id the reset logs hand out, and the rows the new
// logs write below them would never be read, nor would the rows it computed from the old logs be
// true. So the write finds the reset, rolls back, and says it held an input back, which the loop
// answers with a pass soon (runPass), and that pass reads the logs from their heads.
//
// The page is read and then written with the reset between, once with cursor rows at both
// moments, which a reset deletes, and once with none at either, which a reset leaves as it finds
// them: the positions the page would write are then the only trace of the old logs.
func TestAPassThatStraddlesAResetWritesNothingAndTheNextPassReadsTheNewLogs(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(t *testing.T, run *consumer)
	}{
		{"with the cursor rows of an earlier pass", func(t *testing.T, run *consumer) {
			run.pass(t)
			run.at = run.at.Add(time.Minute)
			run.writeEvent(t, run.fixture.PhaseWork[0], "implementer", "comment.created", `{}`, run.at.Add(-time.Second))
		}},
		{"with no cursor row at either moment", func(t *testing.T, run *consumer) {}},
	} {
		t.Run(test.name, func(t *testing.T) {
			run := newConsumer(t, 1)
			test.setup(t, run)
			page := run.read(t)
			if len(page.computed) == 0 {
				t.Fatalf("the page computed no project, so its write has nothing for a reset to make untrue")
			}
			for _, statement := range resetStatements {
				run.exec(t, "reset the logs as e2e/seed.ts does: "+statement, statement)
			}

			changed, next, err := writePage(t.Context(), run.deps, run.sched, page)
			if err != nil {
				t.Fatalf("the write of a page that straddled the reset failed: %v", err)
			}
			if !next.held || next.more {
				t.Errorf("the write of a page that straddled the reset answers %+v, want held with no page after it, so the loop runs the next pass soon", next)
			}
			if len(changed) != 0 || len(run.rows(t)) != 0 {
				t.Errorf("the write of a page that straddled the reset changed %v and left %d rows, want nothing written", changed, len(run.rows(t)))
			}
			if cursors := run.cursors(t); len(cursors) != 0 {
				t.Errorf("the write of a page that straddled the reset left the cursors %v, want none", cursors)
			}
			run.requireNoCursorAboveItsLog(t, "after the write of a page that straddled the reset,")

			run.pass(t)
			run.requireNoCursorAboveItsLog(t, "after the pass that follows it,")
			run.requireTheResetLogsAreRead(t)
		})
	}
}

// TestAWriteWaitsOutAResetThatHasDeletedTheCursorsAndWritesNothingAfterIt: the reset's delete of
// the cursor rows may not have committed when the pass's write begins, so a write that read the
// cursor rows and found them as the page left them could still write after the reset commits. The
// write takes a lock of the cursor table that the reset's delete conflicts with before it reads
// them, so it waits for the reset's commit and reads the rows as the reset left them.
//
// The reset runs in a second connection and stays open while the write starts on a goroutine of
// its own. The test waits until the write is blocked on a lock of this database (pg_locks), which
// it can only be because of the reset: the table lock it takes first, and without that statement
// the row of the cursor the reset deleted, which its upsert waits on; the one backend the test
// excludes is the reset's. Only then does the reset commit. The write must then answer held and
// leave no cursor row, where a write that read the rows before the lock would write the old
// positions back.
func TestAWriteWaitsOutAResetThatHasDeletedTheCursorsAndWritesNothingAfterIt(t *testing.T) {
	run := newConsumer(t, 1)
	run.pass(t)
	run.at = run.at.Add(time.Minute)
	page := run.read(t)
	if len(page.computed) != 0 || !page.committed {
		t.Fatalf("the page read after a pass with no input after it computed %d projects with cursor rows %t, want none and true: its write would wait on more than the cursors",
			len(page.computed), page.committed)
	}

	reset, err := run.database.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin the reset: %v", err)
	}
	t.Cleanup(func() { _ = reset.Rollback(context.Background()) })
	var resetBackend int
	if err := reset.QueryRow(t.Context(), `select pg_backend_pid()`).Scan(&resetBackend); err != nil {
		t.Fatalf("read the reset's backend: %v", err)
	}
	for _, statement := range resetStatements {
		if _, err := reset.Exec(t.Context(), statement); err != nil {
			t.Fatalf("reset the logs as e2e/seed.ts does: %s: %v", statement, err)
		}
	}

	type write struct {
		changed []string
		next    nextPage
		err     error
	}
	done := make(chan write, 1)
	go func() {
		changed, next, err := writePage(t.Context(), run.deps, run.sched, page)
		done <- write{changed, next, err}
	}()
	testwait.Until(t, "the pass's write to wait on the reset's locks", func() (bool, bool) {
		select {
		case early := <-done:
			t.Fatalf("the pass's write ended while the reset was open, answering %+v, so it did not wait for it", early)
		default:
		}
		var blocked bool
		if err := run.database.Pool.QueryRow(t.Context(), `
			select exists (
				select 1 from pg_locks lock join pg_stat_activity backend on backend.pid = lock.pid
				where not lock.granted and lock.pid <> $1 and backend.datname = current_database())
		`, resetBackend).Scan(&blocked); err != nil {
			t.Fatalf("read the locks the database waits on: %v", err)
		}
		return blocked, blocked
	})
	if err := reset.Commit(t.Context()); err != nil {
		t.Fatalf("commit the reset: %v", err)
	}

	written := testwait.Receive(t, "the pass's write to end once the reset committed", done)
	if written.err != nil {
		t.Fatalf("the write that waited out the reset failed: %v", written.err)
	}
	if !written.next.held || len(written.changed) != 0 {
		t.Errorf("the write that waited out the reset changed %v and answers %+v, want nothing changed and held", written.changed, written.next)
	}
	if cursors := run.cursors(t); len(cursors) != 0 {
		t.Errorf("the write that waited out the reset left the cursors %v, want none: it wrote the old logs' positions after the reset", cursors)
	}
	run.requireNoCursorAboveItsLog(t, "after the write that waited out the reset,")
}
