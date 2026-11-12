package legionfacts

import (
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
)

// TestEveryIssueComputedAloneHoldsTheFactsTheProjectComputedWhole: an issue's row is the same row
// whichever keys the pass that computed it named. There are two computations and no third
// (Compute over a key set, ComputeProject over the project), and either may write any row, so a
// field that moved with the scope would leave the stored facts saying one thing after a feed named
// the issue and another after the next whole-project pass. R1 and R2 rest on that: a reader of a
// row reads one answer and not the answer of whichever pass wrote it last.
//
// The check is the invariant and not one rule of it. It computes the project whole, computes each
// issue of the project on its own, and compares the two rows of that issue field by field. A rule
// that reads another issue is what it catches: the waiting line counts a child its tree has not
// started on the nearest card above it (legion.WaitingLine's nearestCard), and that climb asks
// what the daemon reports of issues a narrow read holds no observation of. Any other
// scope-dependence is caught the same way.
//
// The history is the rig's with three observations added. The admission block makes the waiting
// reasons differ from each other, as TestEachStoredFactEqualsTheSourceItComesFrom writes it for
// the same reason. The other two are what make the line climb a tree at all: an `admitted` phase
// on a child that holds no slot, and a phase that stands in a column on the root above it. Every
// phase the rig leaves an issue in ends its work, so without them no issue is a child its tree
// has not started and this check would pass over the rule it exists to ask. The check states that
// the line counts the child on the root (CountedOn) before it compares a single row.
func TestEveryIssueComputedAloneHoldsTheFactsTheProjectComputedWhole(t *testing.T) {
	seeded := seed(t, 6)
	project := seeded.fixture.Project
	pool := seeded.database.Pool

	queued, blocked, blocker := seeded.fixture.Keys[3], seeded.fixture.Keys[2], seeded.fixture.Keys[1]
	seedAdmission(t, seeded, legion.Admission{
		Waiting: []string{queued},
		Blocked: []legion.BlockedIssue{{Issue: blocked, BlockedBy: []string{blocker}}},
	})
	// The first tree's root and its fourth child. The line names neither and no blocker link
	// holds either, so the `admitted` phase with no slot of its own makes the child one its tree
	// has not started, and the `implementing` phase gives the root the card the child is counted
	// on. Every phase the rig leaves an issue in is `done` or `held`, and `done` stands in no
	// column, so without the root's phase the climb finds no card and the child carries one of
	// its own.
	root, unstarted := seeded.fixture.Keys[0], seeded.fixture.Keys[4]
	seedPhase(t, seeded, root, legion.PhaseImplementing, seeded.now)
	seedPhase(t, seeded, unstarted, legion.PhaseAdmitted, seeded.now)
	countedOn := cardAbove(t, seeded, unstarted)
	if countedOn != root {
		t.Fatalf("the line counts %s on %q and not on the card of %s above it, so this test never made the line climb a tree",
			unstarted, countedOn, root)
	}

	whole, err := ComputeProject(t.Context(), pool, project, seeded.now)
	if err != nil {
		t.Fatalf("compute the project whole: %v", err)
	}
	wholeRows := make(map[string]Row, len(whole))
	for _, row := range whole {
		wholeRows[row.Issue] = row
	}

	differed, first := 0, ""
	note := func(difference string) {
		differed++
		if first == "" {
			first = difference
		}
	}
	for _, key := range seeded.fixture.Keys {
		alone, err := Compute(t.Context(), pool, project, []string{key}, seeded.now)
		if err != nil {
			t.Fatalf("compute %s on its own: %v", key, err)
		}
		if len(alone) > 1 {
			t.Fatalf("computing %s on its own answered %d rows, want the one row of that issue alone", key, len(alone))
		}
		row, has := wholeRows[key]
		switch {
		case len(alone) == 0 && has:
			note(fmt.Sprintf("%s has no row computed on its own and this row computed with its project: %s", key, show(row)))
		case len(alone) == 1 && !has:
			note(fmt.Sprintf("%s has no row computed with its project and this row computed on its own: %s", key, show(alone[0])))
		case len(alone) == 1:
			if difference := rowDifference(alone[0], row); difference != "" {
				note(fmt.Sprintf("%s computed on its own against the same issue computed with its project: %s", key, difference))
			}
		}
	}

	if len(wholeRows) == 0 || len(seeded.fixture.Keys) == 0 {
		t.Fatalf("the project holds %d issues and the whole-project computation answered %d rows, so this test examined nothing",
			len(seeded.fixture.Keys), len(wholeRows))
	}
	if differed > 0 {
		t.Errorf("%d of the %d issues hold different facts computed alone and computed with their project; the first is %s",
			differed, len(seeded.fixture.Keys), first)
	}
	t.Logf("%d issues compared field by field, each computed alone against the project computed whole; %d differed. The whole-project computation answered %d rows, %s stands in the waiting line, a blocker link holds %s, and %s is a child its tree has not started, counted on %s",
		len(seeded.fixture.Keys), differed, len(wholeRows), queued, blocked, unstarted, countedOn)
}

// seedPhase writes one phase observation of an issue as the sampler writes it, which makes that
// phase the newest the daemon reports of the issue. The moment is the caller's, so a phase written
// as of the computation's own moment is the one every rule reads.
func seedPhase(t *testing.T, seeded history, issue, phase string, at time.Time) {
	t.Helper()
	if _, err := seeded.database.Pool.Exec(t.Context(), `
		insert into legion_observations (project, issue_key, field, value, observed_at)
		values ($1, $2, $3, $4::jsonb, $5)
	`, seeded.fixture.Project, issue, legion.FieldPhase, fmt.Sprintf("%q", phase), at); err != nil {
		t.Fatalf("seed the %s phase of %s: %v", phase, issue, err)
	}
}

// cardAbove is the card the waiting line counts the issue on, from the line this package's own
// computation builds (rowsOf): the nearest issue above it in its tree that carries a card, and ""
// for an issue the line draws a card of its own for. A caller reads it to state that the line
// climbed a tree to answer the issue, which is the one rule of the facts that reads another issue.
func cardAbove(t *testing.T, seeded history, key string) string {
	t.Helper()
	population, err := metrics.ReadProject(t.Context(), seeded.database.Pool, nil, seeded.fixture.Project, seeded.now)
	if err != nil {
		t.Fatalf("read the project's population: %v", err)
	}
	admission, err := readAdmission(t.Context(), seeded.database.Pool, seeded.fixture.Project)
	if err != nil {
		t.Fatalf("read the admission block: %v", err)
	}
	line := legion.NewWaitingLine(legion.WaitingLineInput{
		Admission: admission,
		Issue:     func(key string) (legion.LineIssue, bool) { return lineIssue(population, key) },
		Tree:      population.TreeNode,
	})
	return line.CountedOn(key)
}
