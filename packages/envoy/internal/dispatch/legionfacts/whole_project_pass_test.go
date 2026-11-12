package legionfacts

import (
	"testing"
	"time"
)

// computedWholeAt is when each project was last computed whole, by project key: the mark the
// pass writes in the transaction that wrote that computation's rows (markComputedWhole). A
// project with no mark has an entry in neither the answer nor the table.
func computedWholeAt(t *testing.T, run *consumer) map[string]time.Time {
	t.Helper()
	rows, err := run.database.Pool.Query(t.Context(), `select project_key, computed_at from issue_facts_computed`)
	if err != nil {
		t.Fatalf("read the projects computed whole: %v", err)
	}
	defer rows.Close()
	marks := map[string]time.Time{}
	for rows.Next() {
		var project string
		var at time.Time
		if err := rows.Scan(&project, &at); err != nil {
			t.Fatalf("read the projects computed whole: %v", err)
		}
		marks[project] = at.UTC()
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the projects computed whole: %v", err)
	}
	return marks
}

// TestAProjectWhoseIssuesCarryNoWorkIsComputedWholeOnceAndNotAgain: a project the daemon reports
// and no Legion role has worked is computed whole on the pass that first meets it, and on no
// pass after that.
//
// It is the case R8 names: a project whose cards the daemon reports while no issue of it holds
// phase work. Its whole computation answers no row, so the pass writes none, and a loop that
// read "never computed" from "holds no stored row" would read the whole project again every
// SweepEvery for as long as the project held no worked issue. The mark is what clears the
// condition, and this check reads the mark's own moment: the second pass leaves it where the
// first pass wrote it.
func TestAProjectWhoseIssuesCarryNoWorkIsComputedWholeOnceAndNotAgain(t *testing.T) {
	run := newConsumer(t, 2)
	const quiet = "QUIET"
	run.exec(t, "seed a second project", `insert into projects (key, name) values ($1, 'The quiet project')`, quiet)
	run.exec(t, "seed the daemon's observation of "+quiet, `
		insert into legion_observations (project, issue_key, field, value, observed_at)
		values ($1, $2, 'phase', '"implementing"'::jsonb, $3)
	`, quiet, quiet+"-1", run.at.Add(-time.Hour))

	first := run.at
	run.pass(t)
	marked := computedWholeAt(t, run)
	stored, err := Read(t.Context(), run.database.Pool, quiet)
	if err != nil {
		t.Fatalf("read the stored facts of %s: %v", quiet, err)
	}
	if len(stored) > 0 {
		t.Fatalf("%s holds %d stored rows, so it is not a project whose computation answers none and this test examined nothing",
			quiet, len(stored))
	}
	at, has := marked[quiet]
	switch {
	case !has:
		t.Fatalf("the pass that computed %s whole recorded no mark, so nothing can clear the backfill", quiet)
	case !at.Equal(first):
		t.Errorf("%s is marked computed whole as of %s, want the moment the pass computed as of, %s", quiet, at, first.UTC())
	}

	run.at = run.at.Add(time.Minute)
	run.pass(t)
	again := computedWholeAt(t, run)
	if !again[quiet].Equal(at) {
		t.Errorf("the second pass computed %s whole again, marking it as of %s, and it had been computed whole at %s",
			quiet, again[quiet], at)
	}
	projects, err := legionProjects(t.Context(), run.database.Pool, run.deps.Build, false)
	if err != nil {
		t.Fatalf("read the projects Legion works: %v", err)
	}
	if backfill, known := projects[quiet]; !known || backfill {
		t.Errorf("%s stands as a backfill after two passes (known %t, backfill %t)", quiet, known, backfill)
	}
	t.Logf("%s holds a daemon observation, no issue with Legion phase work and %d stored rows: the first pass computed it whole as of %s, the second pass left that mark where it was and the rule now answers backfill=%t",
		quiet, len(stored), at, projects[quiet])
}

// TestALostChangeCursorCostsOneWholeProjectPass: a change cursor whose own row has been pruned
// out of the log costs one pass that computes every project whole, and not one every pass.
//
// delivery.PruneChanges deletes every change older than its retention, the newest included, so
// in a quiet deployment the cursor can stand below the oldest change the log holds while no
// newer change exists. That loss is answered by computing every project whole, which covers
// every change the log ever held, so the pass leaves the cursor at the log's head: a pass that
// wrote the pruned position back would read lost again every SweepEvery until some change
// committed.
func TestALostChangeCursorCostsOneWholeProjectPass(t *testing.T) {
	run := newConsumer(t, 2)
	run.pass(t)
	// The second pass moves the change cursor above the changes the first pass's own writes
	// logged, so the pruning below leaves the cursor pointing at a change the log no longer
	// holds.
	run.pass(t)
	run.exec(t, "prune the whole change log", `delete from delivery_changes`)
	if at := run.cursors(t)[changesCursor].position; at == 0 {
		t.Fatal("the change cursor stands at 0, which has read nothing and lost nothing: this test examined nothing")
	}
	before := computedWholeAt(t, run)

	run.at = run.at.Add(time.Minute)
	lostAt := run.at
	run.pass(t)
	after := computedWholeAt(t, run)
	cursor := run.cursors(t)[changesCursor].position
	project := run.fixture.Project
	if !after[project].Equal(lostAt) {
		t.Errorf("the pass that read a lost change cursor marked %s computed whole as of %s, want the moment it computed as of, %s; the mark before it was %s",
			project, after[project], lostAt.UTC(), before[project])
	}
	if cursor != 0 {
		t.Errorf("after the pass that acted on the loss the change cursor stands at %d, want 0, the newest seq the emptied log holds", cursor)
	}

	run.at = run.at.Add(time.Minute)
	run.pass(t)
	settled := computedWholeAt(t, run)
	if !settled[project].Equal(after[project]) {
		t.Errorf("the pass after the loss computed %s whole again, marking it as of %s: one loss cost more than one whole-project pass",
			project, settled[project])
	}
	t.Logf("a change cursor pruned out of an empty log: the pass that met the loss computed %s whole as of %s and left the cursor at %d, and the pass after it computed nothing whole (the mark stands at %s)",
		project, after[project], cursor, settled[project])
}
