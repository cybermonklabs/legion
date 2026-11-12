package legionfacts

import (
	"testing"
	"time"
)

// A change cursor that a later page (not the sweep page) finds pruned must still be answered by
// a whole-project read, because the page moves the cursor past the gap.
func TestALossMetOnALaterPageIsAnswered(t *testing.T) {
	run := newConsumer(t, 3)
	run.pass(t)
	run.at = run.at.Add(time.Minute)
	run.pass(t)
	project := run.fixture.Project
	var marked time.Time
	if err := run.database.Pool.QueryRow(t.Context(),
		`select computed_at from issue_facts_computed where project_key = $1`, project).Scan(&marked); err != nil {
		t.Fatal(err)
	}
	cursorAt := run.cursors(t)[changesCursor].position
	if cursorAt == 0 {
		t.Fatal("the change cursor stands at 0, which has read nothing and lost nothing: this test examined nothing")
	}
	if len(run.fixture.PhaseWork) < 2 {
		t.Fatalf("the rig wrote %d issues with Legion phase work, want the two that carry the pruned pull requests, so this test examined nothing",
			len(run.fixture.PhaseWork))
	}
	for number, issue := range []string{run.fixture.PhaseWork[0], run.fixture.PhaseWork[1]} {
		run.exec(t, "seed a merged pull request", `
			insert into delivery_pull_requests (repo, number, title, url, author, created_at, merged_at, issue_key)
			values ('cybermonklabs/legion', $1, 'A pull request', 'https://example.test/pull', 'alice', $2, $3, $4)
		`, number+100, run.at.Add(-2*time.Hour), run.at.Add(-time.Hour), issue)
	}
	var first int64
	if err := run.database.Pool.QueryRow(t.Context(),
		`select min(seq) from delivery_changes where seq > $1 and kind = 'pull_request'`, cursorAt).Scan(&first); err != nil {
		t.Fatal(err)
	}
	run.exec(t, "prune the oldest changes", `delete from delivery_changes where seq <= $1`, first)
	lost, err := changesLost(t.Context(), run.database.Pool, cursorAt)
	if err != nil {
		t.Fatal(err)
	}
	if !lost {
		t.Fatalf("the change at the cursor, %d, is still in the log after the prune of everything at or below %d, so no cursor was lost and this test examined nothing",
			cursorAt, first)
	}
	run.at = run.at.Add(time.Minute)
	if _, err := run.deps.Horizon.Await(t.Context()); err != nil {
		t.Fatal(err)
	}
	safe, err := safeHorizon(t.Context(), run.deps.Horizon)
	if err != nil {
		t.Fatal(err)
	}
	// A later page of a pass: sweep is false.
	if _, _, err := passPage(t.Context(), run.deps, run.sched, safe, run.at, false); err != nil {
		t.Fatal(err)
	}
	t.Logf("after the later page the change cursor stands at %d (it stood at %d, the pruned gap ends at %d)",
		run.cursors(t)[changesCursor].position, cursorAt, first)
	run.at = run.at.Add(time.Minute)
	run.pass(t)
	var after time.Time
	if err := run.database.Pool.QueryRow(t.Context(),
		`select computed_at from issue_facts_computed where project_key = $1`, project).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after.Equal(marked) {
		t.Errorf("the project's last whole read is still %s after the loss and the next pass: the loss met on a later page was never answered", marked.UTC())
	}
}
