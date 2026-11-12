package legionfacts

import (
	"maps"
	"slices"
	"testing"
	"time"
)

// writeTitledEvent writes one event of a session on an issue, under a title that names the issue
// titled, which need not be the issue the event is on. A session's newest title decides which
// issue every event of that session works (legion.PhaseWork), so an event written here under
// another issue's title is how a role session takes its phase work with it. writeRoleEvent
// (stale_row_test.go) is the same write with the title always naming the issue written on.
//
// The moment matters: the newest title is the newest titled event by created_at
// (delivery.ResolveSessionTitles), so two events of one session at one moment leave which title
// is newest undecided.
func writeTitledEvent(t *testing.T, run *consumer, session, issue, titled string, at time.Time) {
	t.Helper()
	run.exec(t, "write session "+session+"'s event on "+issue, `
		insert into events (issue_key, seq, type, actor, payload, notify, created_at)
		values ($1, (select coalesce(max(seq), 0) + 1 from events where issue_key = $1),
		        'comment.created',
		        jsonb_build_object('kind', 'session', 'id', $2::text,
		          'origin', jsonb_build_object('session_title', 'Legion implementer · ' || $3)),
		        '{}'::jsonb, false, $4)
	`, issue, session, titled, at)
}

// staleRow writes one stored row by hand for an issue the computation answers no row for: the row
// a session leaves behind when it takes its phase work to another issue. It is written rather
// than computed because no computation answers such a row, which is the whole of what makes it
// stale.
//
// The row is closed, so the sweep's own set does not hold it: the sweep states the open issues'
// stored rows (openStoredRows), and a check of what a pass deletes inside the set it stated needs
// a row that enters that set only where the check means it to.
func staleRow(t *testing.T, run *consumer, issue string) {
	t.Helper()
	run.exec(t, "write a stale row for "+issue, `
		insert into issue_facts (project_key, issue_key, opened_at, closed_at, hours_by_phase,
		                         work_hours, review_rounds, root_key, tree_depth, slot_hours,
		                         updated_at)
		values ($1, $2, $3, $4, '{"implementing": 1.5}'::jsonb, 1.5, 1, $2, 0, 1, $5)
	`, run.fixture.Project, issue, run.at.Add(-48*time.Hour), run.at.Add(-24*time.Hour), run.at)
}

// deletedBetween is every key whose stored row before holds and after does not, sorted.
func deletedBetween(before, after map[string]Row) []string {
	deleted := []string{}
	for key := range before {
		if _, stands := after[key]; !stands {
			deleted = append(deleted, key)
		}
	}
	slices.Sort(deleted)
	return deleted
}

// movedOutside is every key outside stated whose stored row is not the row it was, sorted: what a
// pass that deleted beyond the set it stated would answer.
func movedOutside(before, after map[string]Row, stated []string) []string {
	moved := []string{}
	for key, was := range before {
		if slices.Contains(stated, key) {
			continue
		}
		is, stands := after[key]
		if !stands || !sameRow(is, was) {
			moved = append(moved, key)
		}
	}
	slices.Sort(moved)
	return moved
}

// TestANarrowPassDeletesTheStaleRowOfItsStatedSetAndLeavesEveryRowOutsideItAlone: a pass that
// computed a set of keys deletes the row of a key of that set its computation answered no row
// for, and leaves every row outside that set exactly as it was.
//
// The issue loses its phase work the way production loses it: one session gives an issue its
// only phase work, then writes again under another issue's title, and the rule reads the newest
// title, so every event of that session works the other issue and the issue it left carries
// none. The event is on the issue that lost the work, so the pass's feeds name it and the set
// the pass states holds it (rowsToCompute).
//
// The row that must stand is a stale row of another tree, closed, which no feed named and the
// sweep does not reach: a pass that deleted every row its computation answered none for, rather
// than every such row inside the set it stated, would take that row too.
func TestANarrowPassDeletesTheStaleRowOfItsStatedSetAndLeavesEveryRowOutsideItAlone(t *testing.T) {
	run := newConsumer(t, 3)
	run.pass(t)
	if len(run.fixture.NoPhaseWork) < 2 || len(run.fixture.PhaseWork) == 0 {
		t.Fatalf("the rig wrote %d issues no role worked and %d with phase work, want two and one: this test examined nothing",
			len(run.fixture.NoPhaseWork), len(run.fixture.PhaseWork))
	}
	worked, survivor := run.fixture.NoPhaseWork[0], run.fixture.NoPhaseWork[1]
	other := run.fixture.PhaseWork[0]
	const session = "s-one-role"

	// The session's first event, under a title naming the issue it writes on: that issue
	// carries Legion phase work from here on, and the pass gives it the row a reader takes for
	// "Legion worked this issue".
	writeTitledEvent(t, run, session, worked, worked, run.at.Add(-3*time.Hour))
	run.pass(t)
	if _, has := run.rows(t)[worked]; !has {
		t.Fatalf("%s carries Legion phase work and has no stored row, so this test examined nothing", worked)
	}

	staleRow(t, run, survivor)
	before := run.rows(t)

	// The same session writes again, under the other issue's title: its newest title names that
	// issue now, so its events work that issue and the issue it left has no phase work at all.
	writeTitledEvent(t, run, session, worked, other, run.at.Add(-2*time.Hour))
	stated := statedSet(t, run, []string{worked}, nil)
	if slices.Contains(stated, survivor) {
		t.Fatalf("the set the pass states is %v, which holds the stale row %s this check needs outside it", stated, survivor)
	}

	changed := run.pass(t)
	after := run.rows(t)
	deleted := deletedBetween(before, after)
	strayed := outside(changed, stated)
	moved := movedOutside(before, after, stated)

	if _, stands := after[worked]; stands {
		t.Errorf("%s carries no Legion phase work and keeps its stored row: the table holds %d rows", worked, len(after))
	}
	if !slices.Equal(deleted, []string{worked}) {
		t.Errorf("the pass deleted %v, want %s alone: the one key of the set it stated its computation answers no row for",
			deleted, worked)
	}
	if !slices.Contains(changed, worked) {
		t.Errorf("the pass deleted %s and answered the changed keys %v, which does not hold it: a row that went is a change",
			worked, changed)
	}
	stood, stands := after[survivor]
	switch {
	case !stands:
		t.Errorf("the stale row of %s stands outside the set the pass stated and the pass deleted it: %s",
			survivor, show(before[survivor]))
	case !sameRow(stood, before[survivor]):
		t.Errorf("the stale row of %s stands outside the set the pass stated and the pass rewrote it as\n\tgot  %s\n\twant %s",
			survivor, show(stood), show(before[survivor]))
	}
	if len(strayed) > 0 {
		t.Errorf("the pass changed %d rows outside the set it stated: %v", len(strayed), strayed)
	}
	if len(moved) > 0 {
		t.Errorf("%d rows outside the set the pass stated are no longer the rows they were: %v", len(moved), moved)
	}
	t.Logf("a narrow pass after %s lost its phase work to %s: the set it stated is %v (%d keys), %d row(s) deleted (%v), %d row(s) changed (%v), %d rows outside the stated set changed, and the stale row of %s outside it stands",
		worked, other, stated, len(stated), len(deleted), deleted, len(changed), changed, len(moved), survivor)
}

// TestAWholeProjectPassDeletesAStaleRowNoFeedNamed: a pass that computed the project whole
// states every issue of the project, so it deletes a stale row anywhere in that project,
// including the row of a closed issue no feed of the pass named and the sweep never reaches.
// That is the pass that answers for a row no narrow pass will ever state.
//
// It deletes nothing outside the set it stated either: a stored row whose key names no issue of
// the project was read by no computation, because the whole-project computation reads the
// project's issues (metrics.ReadProject), and it stands.
//
// The pass is made a whole-project pass the way production makes one: the change log is pruned
// below where the cursor stands, so the changes the cursor passed cannot be known and every
// project is computed whole (readChangePage, legionProjects).
func TestAWholeProjectPassDeletesAStaleRowNoFeedNamed(t *testing.T) {
	run := newConsumer(t, 3)
	run.pass(t)
	// The second pass moves the change cursor above the changes the first pass's own writes
	// logged, so the pruning below leaves the cursor pointing at a change the log no longer
	// holds.
	run.pass(t)
	if len(run.fixture.NoPhaseWork) == 0 {
		t.Fatalf("the rig wrote %d issues no role worked, want one: this test examined nothing", len(run.fixture.NoPhaseWork))
	}

	stale := run.fixture.NoPhaseWork[0]
	unknown := run.fixture.Project + "-9001"
	issues, err := treeLinks(t.Context(), run.database.Pool, run.fixture.Project)
	if err != nil {
		t.Fatalf("read the project's issues: %v", err)
	}
	if _, is := issues[unknown]; is {
		t.Fatalf("%s is an issue of the project, so a row of it is inside the set a whole-project pass states", unknown)
	}
	staleRow(t, run, stale)
	staleRow(t, run, unknown)
	before := run.rows(t)
	if _, written := before[stale]; !written {
		t.Fatalf("the stale row of %s was not stored, so this test examined nothing", stale)
	}

	run.exec(t, "prune the change log", `delete from delivery_changes`)
	changed := run.pass(t)
	after := run.rows(t)
	deleted := deletedBetween(before, after)
	stated := slices.Sorted(maps.Keys(issues))
	strayed := outside(changed, stated)
	moved := movedOutside(before, after, stated)

	if _, stands := after[stale]; stands {
		t.Errorf("%s carries no Legion phase work, no feed of the pass named it, and it keeps its stored row", stale)
	}
	if !slices.Equal(deleted, []string{stale}) {
		t.Errorf("the whole-project pass deleted %v, want %s alone", deleted, stale)
	}
	if !slices.Contains(changed, stale) {
		t.Errorf("the pass deleted %s and answered the changed keys %v, which does not hold it: a row that went is a change",
			stale, changed)
	}
	if is, stands := after[unknown]; !stands || !sameRow(is, before[unknown]) {
		t.Errorf("the row of %s names no issue of the project, so no computation read it, and the pass changed it: stands %t",
			unknown, stands)
	}
	if len(strayed) > 0 {
		t.Errorf("the pass changed %d rows outside the set it stated: %v", len(strayed), strayed)
	}
	if len(moved) > 0 {
		t.Errorf("%d rows outside the set the pass stated are no longer the rows they were: %v", len(moved), moved)
	}
	t.Logf("a whole-project pass over %s: the set it stated is every issue of the project (%d keys), %d row(s) deleted (%v), %d row(s) changed (%v), %d rows outside the stated set changed, and the row of %s, which names no issue of the project, stands",
		run.fixture.Project, len(stated), len(deleted), deleted, len(changed), changed, len(moved), unknown)
}
