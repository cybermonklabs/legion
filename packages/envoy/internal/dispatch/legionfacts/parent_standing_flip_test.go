package legionfacts

import (
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
)

// A parent's Legion standing places every issue beneath it, and it moves both ways. The
// placement rule stops a chain below a parent that is not a Legion issue (legion.TreeChain), so
// an issue stands under its parent while that parent is a Legion issue and under itself once
// that parent is not. An issue is a Legion issue where the daemon reports it or where it carries
// Legion phase work (legion.TreeNode's Legion field holds the rule), and a parent gains that
// standing and loses it.
//
// No flip writes a change that names any issue beneath the parent. The pass reaches those
// issues by descending the links from the parent (rowsTouchedBy), and the three checks below
// are that descent read from the stored rows: each one flips the parent one way and then holds
// the stored row of every issue beneath the parent against a computation of that issue.
//
// The issue beneath is closed in all three checks. A closed issue is the case the descent
// answers for: the sweep recomputes the open issues alone (openStoredRows), so an open issue's
// row is rewritten on the next pass whatever the scope states, and a closed issue's row stands
// as the pass left it.
//
// The three drive three of the ways the standing flips: a worker session's event on the parent,
// a retitle that takes the parent's phase work away, and the daemon's first report of the
// parent, which is the one flip no event names.

// trackingParentOverAClosedChild gives the rig a tracking parent with a closed child beneath it
// and answers the two keys.
//
// The child is a closed issue the rig gave Legion phase work, so the pass holds a stored row of
// it. The parent is a new issue of the project that no Legion role worked and that the daemon
// never reported, so it is no Legion issue until a session writes on it. It copies the child's
// row for the columns a test has no reason to choose, and takes a number and a rank of its own.
//
// The child's reported `tree` goes, because the tree rule reads the daemon's `tree` before the
// parent link (legion.TreeChain) and the parent link is what places the child here.
func trackingParentOverAClosedChild(t *testing.T, run *consumer) (parent, child string) {
	t.Helper()
	project := run.fixture.Project
	if err := run.database.Pool.QueryRow(t.Context(), `
		select key from issues
		where project_key = $1 and closed_at is not null and parent_key is not null and key = any($2::text[])
		order by key limit 1`, project, run.fixture.PhaseWork).Scan(&child); err != nil {
		t.Fatalf("no closed child with phase work: %v", err)
	}
	var number int
	if err := run.database.Pool.QueryRow(t.Context(),
		`select max(number) + 1 from issues where project_key = $1`, project).Scan(&number); err != nil {
		t.Fatalf("read the next issue number of %s: %v", project, err)
	}
	parent = fmt.Sprintf("%s-%d", project, number)
	run.exec(t, "insert a tracking parent", `
		insert into issues (key, project_key, number, title, status, rank, priority, parent_key, created_by, created_at, closed_at)
		select $2, project_key, $3, 'A tracking issue', status, rank || 'x', priority, null, created_by, created_at, closed_at
		from issues where key = $1`, child, parent, number)
	run.exec(t, "reparent the child", `update issues set parent_key = $2 where key = $1`, child, parent)
	run.exec(t, "drop the child's reported tree",
		`delete from legion_observations where issue_key = $1 and field = $2`, child, legion.FieldTree)
	return parent, child
}

// issuesBeneath is every issue that stands beneath the key through the project's tree links,
// sorted. It reads the links (metrics.TreeLinks) and descends them here rather than calling
// rowsTouchedBy, because rowsTouchedBy is what these checks examine: a check that read it would
// answer the same keys as the code under test and could state nothing about it.
func issuesBeneath(t *testing.T, run *consumer, key string) []string {
	t.Helper()
	above, err := metrics.TreeLinks(t.Context(), run.database.Pool, run.fixture.Project)
	if err != nil {
		t.Fatalf("read the tree links of %s: %v", run.fixture.Project, err)
	}
	children := map[string][]string{}
	for below, up := range above {
		if up != "" {
			children[up] = append(children[up], below)
		}
	}
	beneath := map[string]bool{}
	descending := slices.Clone(children[key])
	for len(descending) > 0 {
		next := descending[len(descending)-1]
		descending = descending[:len(descending)-1]
		if beneath[next] {
			continue
		}
		beneath[next] = true
		descending = append(descending, children[next]...)
	}
	return slices.Sorted(maps.Keys(beneath))
}

// storedRowsBeneathAgree holds the stored row of every issue beneath the parent against a
// computation of that issue, made as of the moment the pass computed as of. what names the flip
// the parent just made, for the log line and for the failures.
//
// It records how many issues stood beneath the parent, and a run where none did examined
// nothing: the descent it reads has no issue to carry the parent's standing to.
func storedRowsBeneathAgree(t *testing.T, run *consumer, parent, what string) {
	t.Helper()
	beneath := issuesBeneath(t, run, parent)
	if len(beneath) == 0 {
		t.Fatalf("no issue stood beneath %s %s, so this test examined nothing", parent, what)
	}
	fresh, err := Compute(t.Context(), run.database.Pool, run.fixture.Project, beneath, run.at)
	if err != nil {
		t.Fatalf("compute the %d issue(s) beneath %s: %v", len(beneath), parent, err)
	}
	computed := make(map[string]Row, len(fresh))
	for _, row := range fresh {
		computed[row.Issue] = row
	}
	stored := run.rows(t)
	t.Logf("%d issue(s) stood beneath %s %s: %v; the computation answers a row for %d of them",
		len(beneath), parent, what, beneath, len(computed))
	for _, key := range beneath {
		row, holds := stored[key]
		want, answered := computed[key]
		switch {
		case holds && !answered:
			t.Errorf("%s stands beneath %s %s and holds the stored row %s, and its computation answers no row",
				key, parent, what, show(row))
		case !holds && answered:
			t.Errorf("%s stands beneath %s %s and holds no stored row, and its computation places it under %q at depth %d",
				key, parent, what, want.RootKey, want.TreeDepth)
		case holds && (row.RootKey != want.RootKey || row.TreeDepth != want.TreeDepth):
			t.Errorf("%s stands beneath %s %s: its stored row places it under %q at depth %d, and its computation places it under %q at depth %d",
				key, parent, what, row.RootKey, row.TreeDepth, want.RootKey, want.TreeDepth)
		}
	}
}

// computedPlacement is the one row a computation of the key, made as of the consumer's clock,
// answers for it.
func computedPlacement(t *testing.T, run *consumer, key string) Row {
	t.Helper()
	fresh, err := Compute(t.Context(), run.database.Pool, run.fixture.Project, []string{key}, run.at)
	if err != nil {
		t.Fatalf("compute %s: %v", key, err)
	}
	if len(fresh) != 1 {
		t.Fatalf("the computation answers %d rows for %s, want the one row its phase work earns it, so this test examined nothing",
			len(fresh), key)
	}
	return fresh[0]
}

// TestAParentGainingLegionStandingMovesEveryIssueBeneathIt: a tracking parent that no Legion
// role worked gains Legion phase work, and the stored row of every issue beneath it then places
// that issue where a computation of it places it.
//
// A session's event on the parent, under the parent's own title, is the gain. It names the
// parent and no issue beneath it. The pass before the event is the project's whole computation,
// so every stored row is right before the gain, and the clock moves one minute, which no
// placement reads.
func TestAParentGainingLegionStandingMovesEveryIssueBeneathIt(t *testing.T) {
	run := newConsumer(t, 3)
	parent, child := trackingParentOverAClosedChild(t, run)
	const session = "s-flip-role"

	run.pass(t)
	before := run.rows(t)[child]
	if before.RootKey != child {
		t.Fatalf("the stored row of %s places it under %q while no Legion role worked %s, want itself, so this test examined nothing",
			child, before.RootKey, parent)
	}

	writeSessionEvent(t, run, session, parent, run.at.Add(-time.Hour))
	run.at = run.at.Add(time.Minute)
	run.pass(t)

	placed := computedPlacement(t, run, child)
	if placed.RootKey != parent {
		t.Fatalf("the computation places %s under %q and not under %s, so the parent's standing did not move and this test examined nothing",
			child, placed.RootKey, parent)
	}
	storedRowsBeneathAgree(t, run, parent, "after it gained Legion phase work")
}

// TestAParentLosingLegionStandingMovesEveryIssueBeneathItBack: a parent that carries Legion
// phase work loses it, and the stored row of every issue beneath it then places that issue where
// a computation of it places it, which is where each stood before the parent was Legion's.
//
// The loss is the one way a parent's standing moves with no event of its own: the session that
// worked the parent writes next on the project under another issue's title, and a session's
// newest title decides which issue its events work (legion.PhaseWork). The page that reads that
// event states the parent, because the session wrote on it and it holds a stored row
// (sessionIssues), and nothing names any issue beneath the parent.
//
// The parent carries the work before the first pass, so the whole-project pass that follows
// places the child under it. The stored row this check starts from is therefore the whole
// computation's and not a descent's.
func TestAParentLosingLegionStandingMovesEveryIssueBeneathItBack(t *testing.T) {
	run := newConsumer(t, 3)
	parent, child := trackingParentOverAClosedChild(t, run)
	const session = "s-flip-role"
	other := run.fixture.PhaseWork[0]
	if other == child {
		other = run.fixture.PhaseWork[1]
	}

	writeSessionEvent(t, run, session, parent, run.at.Add(-2*time.Hour))
	run.pass(t)
	before := run.rows(t)[child]
	if before.RootKey != parent {
		t.Fatalf("the stored row of %s places it under %q while %s carries Legion phase work, want that parent, so this test examined nothing",
			child, before.RootKey, parent)
	}

	writeProjectTitledEvent(t, run, session, other, run.at)
	run.at = run.at.Add(time.Minute)
	run.pass(t)

	placed := computedPlacement(t, run, child)
	if placed.RootKey != child {
		t.Fatalf("the computation places %s under %q and not under itself, so %s is a Legion issue still and this test examined nothing",
			child, placed.RootKey, parent)
	}
	storedRowsBeneathAgree(t, run, parent, "after it stopped being a Legion issue")
}

// TestTheDaemonsFirstReportOfAParentMovesEveryIssueBeneathIt: the daemon reports a tracking
// parent no Legion role worked, and the stored row of every issue beneath that parent then
// places that issue where a computation of it places it.
//
// This is the one flip of the standing that no event names. An issue is a Legion issue where
// the daemon reports it or where it carries Legion phase work, so one row of
// legion_observations under the parent's key is the whole gain: the rig writes no event of the
// parent and no event of any issue beneath it. The pass reads that row from the observations
// feed (readObservationPage), which names the parent and marks no move, because the field is
// `phase` and not `tree`.
//
// The pass before the report is the project's whole computation, so every stored row is right
// before the gain, and the clock moves one minute, which no placement reads.
func TestTheDaemonsFirstReportOfAParentMovesEveryIssueBeneathIt(t *testing.T) {
	run := newConsumer(t, 3)
	parent, child := trackingParentOverAClosedChild(t, run)

	run.pass(t)
	before := run.rows(t)[child]
	if before.RootKey != child {
		t.Fatalf("the stored row of %s places it under %q while the daemon reports %s not at all, want itself, so this test examined nothing",
			child, before.RootKey, parent)
	}
	var reported int
	if err := run.database.Pool.QueryRow(t.Context(),
		`select count(*) from legion_observations where project = $1 and issue_key = $2`,
		run.fixture.Project, parent).Scan(&reported); err != nil {
		t.Fatalf("count what the daemon reports of %s: %v", parent, err)
	}
	if reported != 0 {
		t.Fatalf("the daemon already reports %s in %d row(s) of legion_observations, so the row below is no first report and this test examined nothing",
			parent, reported)
	}

	run.exec(t, "report "+parent+" for the first time", `
		insert into legion_observations (project, issue_key, field, value, observed_at)
		values ($1, $2, $3, $4::jsonb, $5)
	`, run.fixture.Project, parent, legion.FieldPhase, `"implementing"`, run.at.Add(-time.Hour))
	run.at = run.at.Add(time.Minute)
	run.pass(t)

	placed := computedPlacement(t, run, child)
	if placed.RootKey != parent {
		t.Fatalf("the computation places %s under %q and not under %s, so the report did not move the parent's standing and this test examined nothing",
			child, placed.RootKey, parent)
	}
	storedRowsBeneathAgree(t, run, parent, "after the daemon first reported it")
}
