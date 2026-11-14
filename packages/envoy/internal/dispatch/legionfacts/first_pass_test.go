package legionfacts

import (
	"testing"
	"time"
)

// TestTheFirstPassOverASeededLogReadsOnePage: a database holding no cursor row at all reads one
// page, whatever the log behind it holds.
//
// That first pass computes every project whole, because no project has been computed, and a
// whole project is computed from the history itself. The pages below the feeds' heads hold
// nothing it has not already answered, so the cursors start at those heads. A pass that read
// them instead would recompute every project once per page - about 813 pages of a log the size
// of INFRA's - and no sweep runs until a pass ends, so every open row would stand still while
// it replayed.
//
// The page is one event wide, so a pass that read the log at all would answer that its page was
// full and go on to the next. passPage answers that flag, which is what pass loops on.
func TestTheFirstPassOverASeededLogReadsOnePage(t *testing.T) {
	run := newConsumer(t, 3)
	// A merged pull request, whose insert the delivery trigger logs as a change, so the third
	// feed has a head above 0 for the cursor to start at.
	run.exec(t, "seed a merged pull request", `
		insert into delivery_pull_requests (repo, number, title, url, author, created_at, merged_at, issue_key)
		values ('cybermonklabs/legion', 1, 'A pull request', 'https://example.test/pull/1', 'alice', $1, $2, $3)
	`, run.at.Add(-2*time.Hour), run.at.Add(-time.Hour), run.fixture.Keys[1])
	if _, err := run.deps.Horizon.Await(t.Context()); err != nil {
		t.Fatalf("await the event log's safe horizon: %v", err)
	}
	safe, err := run.deps.Horizon.SafeOrAwait(t.Context())
	if err != nil {
		t.Fatalf("read the event log's safe horizon: %v", err)
	}
	logged := run.head(t, `select count(*) from events`)
	if logged < 2 {
		t.Fatalf("the log holds %d events, so a page of one event is not narrower than it", logged)
	}

	narrow := schedule{every: SweepEvery, page: 1, now: func() time.Time { return run.at }}
	changed, full, err := passPage(t.Context(), run.deps, narrow, safe, run.at, true)
	if err != nil {
		t.Fatalf("the first page of the first pass failed: %v", err)
	}
	if full {
		t.Errorf("the first page of the first pass answers that a feed holds more, so the pass reads the %d events of the log a page at a time",
			logged)
	}

	heads, cursors := run.feedHeads(t), run.cursors(t)
	for _, name := range cursorNames {
		at, has := cursors[name]
		switch {
		case !has:
			t.Errorf("the %s cursor has no row after the first page", name)
		case at.position == 0:
			t.Errorf("the %s cursor stands at 0, so the first pass left that feed at its start", name)
		case at.position > heads[name]:
			t.Errorf("the %s cursor stands at %d, past its feed's head %d", name, at.position, heads[name])
		// The facts feed is the one exception: the page's own write appends an issue_facts
		// change for every row it wrote, after it read that feed's head.
		case at.position < heads[name] && name != changesCursor:
			t.Errorf("the %s cursor stands at %d, want its feed's head %d", name, at.position, heads[name])
		}
	}
	stored := run.rows(t)
	for _, key := range run.fixture.PhaseWork {
		if _, has := stored[key]; !has {
			t.Errorf("%s carries Legion phase work and the first page stored no row for it", key)
		}
	}
	t.Logf("one page of a database with no cursor row, over a log of %d events with a page of one: the page answers that no feed holds more, the cursors stand at events=%d observations=%d delivery_changes=%d, and the backfill wrote %d rows (%d changed keys)",
		logged, cursors[eventsCursor].position, cursors[observationsCursor].position,
		cursors[changesCursor].position, len(stored), len(changed))
}
