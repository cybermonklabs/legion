package legionfacts

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/evals/evalfixture"
	"github.com/sjawhar/envoy/internal/dispatch/legion/legionseed"
	"github.com/sjawhar/envoy/internal/dispatch/legion/legiontest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestARetitleMovesPartOfAClosedRootsWorkToTheClosedChildItNames: the two moves k21 drives
// (dispatch://INFRA-2916's verification), each in a tree the sampler never saw, where the event
// log alone draws every hour. A root's architect files the tree's children and hands each one to
// Legion, so its session wrote on its reviewed child as well as on its root. A session's newest
// title decides which issue each of its events works (legion.PhaseWorkKeys), so a new title naming
// the reviewed child moves the architect's events off the root and onto that child.
//
// The root keeps its planner's and its merger's work, so it loses part of its work time and not
// all of it: the hours its work start moves by, from its architect's first event to its planner's.
// The child gains the hours from its hand-over to its implementer's first event. The hours from its
// filing in `triage` to its hand-over stay out of the workflow (metrics' outOfWorkflowByStatus), so
// a hand-over at the first worker event would leave nothing to gain. The first move gives the
// session its new title through an event on another issue of the tree, the implemented child. The
// second gives it through an event on the project, which carries no issue of its own, so the closed
// child gains its work through an event that names no issue. After each move's pass, every issue
// the move touched holds the row a computation of it answers.
//
// It runs over two histories: the rig's own, with two of its three trees unobserved, and the shape
// of the evals fixture the tester runs k21 on (evalfixture.LegionSpec: half of the trees
// unobserved, queued roots, parks, merges and filings), at 16 finished issues rather than the
// fixture's 240. So a defect that drops the gained hours fails here, and not only in a re-run of
// the check.
func TestARetitleMovesPartOfAClosedRootsWorkToTheClosedChildItNames(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	for _, shape := range []struct {
		name string
		spec legionseed.Spec
	}{
		{"the rig's history, two of three trees unobserved", legionseed.Spec{Project: "LEGION", Trees: 3, Unobserved: 2, Now: now}},
		{"the evals fixture's history", evalfixture.LegionSpec("LEGK", 16, now, 0)},
	} {
		t.Run(shape.name, func(t *testing.T) {
			database := storetest.Open(t)
			fixture := legiontest.Seed(t, database.Pool, shape.spec)
			run := startConsumer(history{database: database, fixture: fixture, now: now}, testBuild, now)
			run.pass(t)
			// The oldest Spec.Unobserved trees are the ones the sampler never saw.
			unobserved := fixture.Roots[len(fixture.Roots)-shape.spec.Unobserved:]
			if len(unobserved) < 2 {
				t.Fatalf("%s holds %d unobserved trees, and the two moves need one each", fixture.Project, len(unobserved))
			}
			for _, move := range []struct {
				what string
				root string
				// onChild writes the event on the tree's implemented child; otherwise it is on
				// the project.
				onChild bool
			}{
				{"an event on the tree's implemented child", unobserved[0], true},
				{"an event on the project, which carries no issue", unobserved[1], false},
			} {
				children := childrenByTitle(t, run, move.root)
				child, implemented := children["A reviewed child"], children["An implemented child"]
				if child == "" || implemented == "" {
					t.Fatalf("the root %s has the children %v, and no reviewed or no implemented child", move.root, children)
				}
				if observed := observationsOfTree(t, run, move.root); observed != 0 {
					t.Fatalf("the tree of %s holds %d observations, and this test moves work in a tree the sampler never saw", move.root, observed)
				}
				before := run.rows(t)
				for _, key := range []string{move.root, child} {
					if row, has := before[key]; !has || row.ClosedAt == nil || row.StartedAt == nil {
						t.Fatalf("%s holds no closed row with a work start before the move, so this test examines nothing", key)
					}
				}

				session := sessionOf(t, legionseed.SessionActor("architect", move.root))
				touched := []string{move.root, child}
				if move.onChild {
					writeTitledEvent(t, run, session, implemented, child, run.at)
					touched = append(touched, implemented)
				} else {
					writeProjectTitledEvent(t, run, session, child, run.at)
				}
				run.at = run.at.Add(time.Minute)
				changed := run.pass(t)
				after := run.rows(t)

				root, gainer := after[move.root], after[child]
				gained := gainer.WorkHours - before[child].WorkHours
				lost := before[move.root].WorkHours - root.WorkHours
				t.Logf("%s retitled to name %s by %s: %s %g -> %g work hours (gained %g), %s %g -> %g (lost %g); the pass changed %v",
					session, child, move.what, child, before[child].WorkHours, gainer.WorkHours, gained,
					move.root, before[move.root].WorkHours, root.WorkHours, lost, changed)
				if gained <= 0 {
					t.Errorf("closed %s gained %g work hours from the title that names it, want the hours from its hand-over to its implementer's first event",
						child, gained)
				}
				if lost <= 0 || root.WorkHours <= 0 {
					t.Errorf("closed %s went from %g to %g work hours, want part of its work moved and not all of it",
						move.root, before[move.root].WorkHours, root.WorkHours)
				}
				if root.StartedAt == nil {
					t.Fatalf("%s holds no work start after the move", move.root)
				}
				if moved := root.StartedAt.Sub(*before[move.root].StartedAt).Hours(); lost != moved {
					t.Errorf("closed %s lost %g work hours, want the %g its work start moved by (%s to %s)",
						move.root, lost, moved, before[move.root].StartedAt.UTC(), root.StartedAt.UTC())
				}
				for _, key := range touched {
					fresh := computedPlacement(t, run, key)
					// The moment a row was written is no fact: a row whose values did not move
					// keeps the one an earlier pass wrote.
					fresh.UpdatedAt = after[key].UpdatedAt
					if difference := rowDifference(after[key], fresh); difference != "" {
						t.Errorf("%s's stored row stands apart from its computation after the move: %s", key, difference)
					}
				}
				for _, key := range []string{move.root, child} {
					if !slices.Contains(changed, key) {
						t.Errorf("the pass that read the move changed %v, and not %s", changed, key)
					}
				}
			}
		})
	}
}

// childrenByTitle is the children of a root, keyed by the title the rig gave each
// (legiontest's tree).
func childrenByTitle(t *testing.T, run *consumer, root string) map[string]string {
	t.Helper()
	rows, err := run.database.Pool.Query(t.Context(), `select title, key from issues where parent_key = $1`, root)
	if err != nil {
		t.Fatalf("read the children of %s: %v", root, err)
	}
	defer rows.Close()
	children := map[string]string{}
	for rows.Next() {
		var title, key string
		if err := rows.Scan(&title, &key); err != nil {
			t.Fatalf("read a child of %s: %v", root, err)
		}
		children[title] = key
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the children of %s: %v", root, err)
	}
	return children
}

// observationsOfTree is how many observations the sampler recorded of a root and its children.
func observationsOfTree(t *testing.T, run *consumer, root string) int {
	t.Helper()
	var count int
	if err := run.database.Pool.QueryRow(t.Context(), `
		select count(*) from legion_observations
		where issue_key in (select key from issues where key = $1 or parent_key = $1)`, root).Scan(&count); err != nil {
		t.Fatalf("count the observations of the tree of %s: %v", root, err)
	}
	return count
}

// sessionOf is the session id an actor's JSON names.
func sessionOf(t *testing.T, actor string) string {
	t.Helper()
	var session struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(actor), &session); err != nil || session.ID == "" {
		t.Fatalf("read the session of the actor %s: %v", actor, err)
	}
	return session.ID
}
