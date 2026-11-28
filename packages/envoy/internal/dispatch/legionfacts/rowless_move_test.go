package legionfacts

import (
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
)

// reportedIssueWithNoRow gives the rig an issue under root that the daemon reports and that no
// Legion role worked, moves child beneath it, and answers its key. A person files it, so it
// carries no Legion phase work and the pass holds no stored row of it (computeKeys); the daemon's
// `phase` of it makes it a Legion issue (legion.IsLegionIssue), so the tree rule climbs through it
// from child to root (legion.TreeChain), and child's row records root as its tree.
func reportedIssueWithNoRow(t *testing.T, run *consumer, root, child string) string {
	t.Helper()
	filed := run.at.Add(-time.Hour)
	middle := run.fileIssue(t, run.fixture.Project, 901, root, `{"kind":"user","id":"alice"}`, filed)
	run.exec(t, "move "+child+" under "+middle, `update issues set parent_key = $1 where key = $2`, middle, child)
	run.observe(t, middle, legion.FieldPhase, legion.PhaseImplementing, filed)
	return middle
}

// computedWhole is every row a computation of the whole project answers as of the consumer's
// clock, by issue key.
func computedWhole(t *testing.T, run *consumer) map[string]Row {
	t.Helper()
	computed, _, err := ComputeProject(t.Context(), run.database.Pool, run.fixture.Project, run.at)
	if err != nil {
		t.Fatalf("compute %s whole: %v", run.fixture.Project, err)
	}
	whole := make(map[string]Row, len(computed))
	for _, row := range computed {
		whole[row.Issue] = row
	}
	return whole
}

// storedRowsAreTheWholeComputation holds every stored row of the project to the row of whole, a
// computation of the whole project (computedWhole), and holds the two to the same issues. what
// names the input the pass before it read, for the failures. The write's own moment is no fact: a
// rewrite stamps updated_at with the pass's moment, which no computation answers.
func storedRowsAreTheWholeComputation(t *testing.T, run *consumer, whole map[string]Row, what string) {
	t.Helper()
	stored := run.rows(t)
	for _, key := range slices.Sorted(maps.Keys(eitherSide(stored, whole))) {
		row, holds := stored[key]
		want, answered := whole[key]
		switch {
		case !answered:
			t.Errorf("after %s, %s holds the stored row %s, and a computation of the whole project answers none", what, key, show(row))
		case !holds:
			t.Errorf("after %s, %s holds no stored row, and a computation of the whole project answers %s", what, key, show(want))
		default:
			want.UpdatedAt = row.UpdatedAt
			if difference := rowDifference(row, want); difference != "" {
				t.Errorf("after %s, the stored row of %s is not the row a computation of the whole project answers: %s", what, key, difference)
			}
		}
	}
}

// TestAMovedIssueWithNoRowChangesTheRowsOfTheTreeItLeft: an issue the daemon reports and no
// Legion role worked holds no stored row, and the tree rule climbs through it all the same
// (legion.TreeChain), so the rows below it record the tree it stands in. A move of that issue out
// of its tree takes those issues with it, and the root they left counts them among its children,
// and the hours it waited while they worked among its own, until its row is computed again.
//
// The history is the rig's with one issue added: a middle issue under the first tree's root,
// filed by a person and reported by the daemon, with the root's implemented child moved beneath
// it (reportedIssueWithNoRow). Both the root and the child are closed, so no input of their own
// brings them into a pass. The first pass reads the project whole, so every stored row is right
// before the move, and the child's row records the first root as its tree. The middle issue then
// moves to the second tree, once by its parent link and once by the `tree` the daemon reports,
// the two rows that move an issue between trees (touch). The move names the middle issue alone,
// which holds no row whose root_key a pass could read: the child's row is the one record of the
// tree the move left (leftTrees).
func TestAMovedIssueWithNoRowChangesTheRowsOfTheTreeItLeft(t *testing.T) {
	t.Parallel()
	for _, move := range []struct {
		name  string
		write func(t *testing.T, run *consumer, middle, joined string)
	}{
		{"by its parent link", func(t *testing.T, run *consumer, middle, joined string) {
			run.exec(t, "move "+middle+" under "+joined, `update issues set parent_key = $1 where key = $2`, joined, middle)
			run.writeEventAs(t, middle, `{"kind":"user","id":"alice"}`, "issue.updated", `{"changed":["parent"]}`, run.at)
		}},
		{"by a tree observation", func(t *testing.T, run *consumer, middle, joined string) {
			run.observe(t, middle, legion.FieldTree, joined, run.at)
		}},
	} {
		t.Run(move.name, func(t *testing.T) {
			t.Parallel()
			run := newConsumer(t, 3)
			project := run.fixture.Project
			left, joined, child := run.fixture.Roots[0], run.fixture.Roots[1], run.fixture.Keys[1]
			middle := reportedIssueWithNoRow(t, run, left, child)

			run.pass(t)
			readWhole := computedWholeAt(t, run)[project]
			before := run.rows(t)
			if row, holds := before[middle]; holds {
				t.Fatalf("%s carries no Legion phase work and holds the stored row %s, so this test moves no issue without a row", middle, show(row))
			}
			for _, key := range []string{left, child} {
				if before[key].ClosedAt == nil {
					t.Fatalf("%s holds no closed row before the move, and this check needs the root and the child closed", key)
				}
			}
			if before[child].Tree != left {
				t.Fatalf("%s stores the root %q before the move, want %s through %s", child, before[child].Tree, left, middle)
			}

			move.write(t, run, middle, joined)
			stated := statedSet(t, run, []string{middle}, map[string]bool{middle: true})
			run.at = run.at.Add(time.Minute)
			changed := run.pass(t)
			after := run.rows(t)

			if at := computedWholeAt(t, run)[project]; !at.Equal(readWhole) {
				t.Fatalf("the pass after the move read %s whole (its record went from %s to %s), so it computed every row and this test examined nothing",
					project, readWhole.UTC(), at.UTC())
			}
			if after[child].Tree != joined {
				t.Fatalf("%s moved %s and %s stores the root %q, want %s, so this test moved nothing between trees",
					move.name, middle, child, after[child].Tree, joined)
			}
			whole := computedWhole(t, run)
			if whole[left].Children >= before[left].Children {
				t.Fatalf("%s counts %d children before the move and %d by a computation after it, so the move took nothing out of its tree and this test examined nothing",
					left, before[left].Children, whole[left].Children)
			}

			fresh := whole[left]
			fresh.UpdatedAt = after[left].UpdatedAt
			if difference := rowDifference(after[left], fresh); difference != "" {
				t.Errorf("%s is the root %s and %s left %s, and its stored row is not the row a computation of the whole project answers: %s",
					left, middle, child, move.name, difference)
			}
			if !slices.Contains(stated, left) {
				t.Errorf("the set the pass stated is %v, which does not hold the root %s left", stated, left)
			}
			if !slices.Contains(changed, left) {
				t.Errorf("%s is the root %s left and the pass left its row alone; it changed %v", left, child, changed)
			}
			if strayed := outside(changed, stated); len(strayed) > 0 {
				t.Errorf("the pass changed %d rows outside the set it stated: %v", len(strayed), strayed)
			}
			storedRowsAreTheWholeComputation(t, run, whole, "the move of "+middle+" "+move.name)
			t.Logf("%s, with no row, moved from %s to %s %s, carrying %s: the pass stated %d keys (%v) and changed %d rows (%v); %s counts %d children before and %d after, and waited on children %g h before and %g h after across its bars",
				middle, left, joined, move.name, child, len(stated), stated, len(changed), changed,
				left, before[left].Children, after[left].Children, waitedOnChildren(before[left]), waitedOnChildren(after[left]))
		})
	}
}

// waitedOnChildren is the hours a row's bars hold waiting while a child worked, over every bar.
func waitedOnChildren(row Row) float64 {
	hours := 0.0
	for _, bar := range row.Bars {
		hours += bar.Segments.WaitingOnChildren
	}
	return hours
}
