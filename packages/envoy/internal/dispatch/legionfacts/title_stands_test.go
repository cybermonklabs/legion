package legionfacts

import (
	"slices"
	"testing"
	"time"
)

// TestAnEventOfASessionWhoseTitleStandsRecomputesNoOtherIssueThatSessionWroteOn is k31 (R2): a
// session's newest title decides which issues its events work (legion.PhaseWorkKeys), so only an
// event that changes that title moves work between the issues the session wrote on. A session
// that writes on 20 issues, each with a stored row, then writes once more on one of them under
// the title it already carries: the pass computes that issue and its ancestors, and none of the
// other 19. It then writes on the project under a title that names another issue: the pass
// computes every stored issue the session wrote on, and the issue the new title names.
//
// What a pass computed is read from the rows it rewrote. Every stored row of the project is made
// wrong by hand before each of the two passes (its review rounds raised by a number no
// computation answers), so each row the pass computes is rewritten (upsert compares every column)
// and each row it does not compute keeps the wrong value.
func TestAnEventOfASessionWhoseTitleStandsRecomputesNoOtherIssueThatSessionWroteOn(t *testing.T) {
	run := newConsumer(t, 5)
	run.pass(t)
	stored := run.rows(t)

	// The session's 20 issues: the first tree's implemented child, the issue its events name,
	// and 19 issues of the other trees, none of them above or below it.
	worked, root := run.fixture.Keys[1], run.fixture.Roots[0]
	firstTree := run.fixture.Keys[:6]
	wrote := []string{worked}
	for _, key := range run.fixture.PhaseWork {
		if len(wrote) < 20 && !slices.Contains(firstTree, key) {
			wrote = append(wrote, key)
		}
	}
	for _, key := range wrote {
		if _, has := stored[key]; !has {
			t.Fatalf("%s holds no stored row, and the session's 20 issues must each hold one", key)
		}
	}
	if len(wrote) != 20 {
		t.Fatalf("the rig holds %d issues with a stored row outside %s's tree, want 19", len(wrote)-1, worked)
	}
	// The issue the new title names: a sibling of the worked issue, which the session never wrote
	// on and which no tree walk from the 20 reaches.
	named := run.fixture.Keys[2]
	if _, has := stored[named]; !has || slices.Contains(wrote, named) {
		t.Fatalf("%s must hold a stored row the session did not write on", named)
	}

	const session = "s-broad-implementer"
	for index, key := range wrote {
		writeTitledEvent(t, run, session, key, worked, run.at.Add(-3*time.Hour+time.Duration(index)*time.Minute))
	}
	run.at = run.at.Add(time.Minute)
	run.pass(t)

	wrongRows := func() {
		t.Helper()
		run.exec(t, "make every stored row wrong", `update issue_facts set review_rounds = review_rounds + 1000 where project_key = $1`,
			run.fixture.Project)
	}

	// The title stands: one more event on the worked issue under the title the session carries.
	wrongRows()
	writeTitledEvent(t, run, session, worked, worked, run.at.Add(-time.Minute))
	run.at = run.at.Add(time.Minute)
	stands := run.pass(t)
	if want := slices.Sorted(slices.Values([]string{worked, root})); !slices.Equal(stands, want) {
		t.Errorf("an event on %s under the title its session carries computed %d issues %v, want %s and its root %s alone",
			worked, len(stands), stands, worked, root)
	}
	for _, key := range wrote[1:] {
		if slices.Contains(stands, key) {
			t.Errorf("an event on %s under the title its session carries computed %s, another issue the session wrote on", worked, key)
		}
	}

	// The title moves: an event on the project under a title that names another issue.
	wrongRows()
	writeProjectTitledEvent(t, run, session, named, run.at.Add(-time.Minute))
	run.at = run.at.Add(time.Minute)
	moves := run.pass(t)
	for _, key := range append(slices.Clone(wrote), named) {
		if !slices.Contains(moves, key) {
			t.Errorf("an event under a title that names %s left %s alone; the pass computed %d issues %v", named, key, len(moves), moves)
		}
	}
	t.Logf("a session that wrote on %d issues: an event under the title it carries computed %d issues %v; an event under a title that names %s computed %d issues",
		len(wrote), len(stands), stands, named, len(moves))
}
