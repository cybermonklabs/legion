package legionfacts

import (
	"maps"
	"slices"
	"testing"
	"time"
)

// TestTheFirstPassOfANewBuildFillsTheWidenedColumns: the per-issue part columns of issue_facts
// (backlog_or_icebox_hours to repo, migration 20261117100000) hold their defaults in every row the
// release before this one wrote, and the filings table is empty, and the first pass of this
// release fills both. It does because the project's record names the build that last read it
// whole (computed_build, migration 20261117100003), and the first pass of a process whose build is
// another one reads the project whole (legionProjects), which writes every column of every row
// (upsert compares them all) and every filing (WriteFilings).
//
// The rows the earlier release left are made here by putting every widened column of every row
// back to its default and emptying the filings. A pass under the build that wrote the record, with
// no input moving, writes nothing: no row, open or closed, and no filing, which is what an earlier
// release's rows would meet under a build that had read the project whole already. A process of
// another build, started after that record was written, reads the project whole and stores every
// row and filing the first pass stored.
func TestTheFirstPassOfANewBuildFillsTheWidenedColumns(t *testing.T) {
	run := newConsumer(t, 3)
	project := run.fixture.Project
	run.pass(t)
	full, filings := run.rows(t), run.filings(t)
	if len(filings) == 0 {
		t.Fatal("the first pass stored no filing, so this check has nothing to refill")
	}

	run.exec(t, "put every widened column back to its default, as the earlier release left it", `
		update issue_facts set backlog_or_icebox_hours = 0, queued_hours = 0, started_at = null,
			merged_at = null, bars = '{}', idle_by_role = '{}', slot_wait_hours = 0, sent_back = false,
			implementing_after_first_review = 0, testing_after_first_review = 0, children = 0, repo = null
	`)
	run.exec(t, "empty the filings, which the earlier release never wrote", `delete from issue_filings`)

	run.at = run.at.Add(time.Minute)
	if changed := run.pass(t); len(changed) != 0 {
		t.Errorf("a pass under the recorded build with no input moving changed %d rows, want none: %v", len(changed), changed)
	}
	held := run.rows(t)
	emptied := 0
	for _, row := range held {
		if len(row.Bars) == 0 {
			emptied++
		}
	}
	if emptied != len(full) {
		t.Errorf("a pass under the recorded build with no input moving refilled %d of %d rows", len(full)-emptied, len(full))
	}
	if left := run.filings(t); len(left) != 0 {
		t.Errorf("a pass under the recorded build stored %d filings, want none: no feed named them", len(left))
	}

	run.at = run.at.Add(time.Minute)
	run.restart()

	run.exec(t, "record another build against the project", `
		update issue_facts_computed set computed_build = $1 where project_key = $2
	`, otherBuild, project)
	run.pass(t)
	filled, refiled := run.rows(t), run.filings(t)
	values := 0
	for _, key := range slices.Sorted(maps.Keys(full)) {
		want, got := full[key], filled[key]
		want.UpdatedAt = got.UpdatedAt
		if difference := rowDifference(got, want); difference != "" {
			t.Errorf("%s after the new build's pass: %s", key, difference)
		}
		values += 12
	}
	if len(refiled) != len(filings) {
		t.Errorf("the new build's pass stored %d filings, want the %d the first pass stored", len(refiled), len(filings))
	}
	for key, want := range filings {
		if !sameFiling(refiled[key], want) {
			t.Errorf("%s is stored as %s after the new build's pass, want %s", key, showFiling(refiled[key]), showFiling(want))
		}
	}
	t.Logf("%s: %d rows, %d of them left at the defaults by the pass under the recorded build; the pass of a process of another build refilled the %d widened values of every row and %d filings",
		project, len(full), emptied, values, len(refiled))
}
