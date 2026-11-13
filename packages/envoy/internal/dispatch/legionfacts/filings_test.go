package legionfacts

import (
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
)

// TestTheConsumerStoresEachLegionFilingAsMetricsClassesItAndMovesItsRootWithItsTree: the consumer
// writes one issue_filings row per issue a Legion session of the project filed, each carrying the
// class, the filer and the filer's phase metrics.Compute's splitting table gives that filing, and
// the filed issue's tree root as the same population places it. The first pass reads the history
// whole; a later pass reads four new filings off the events feed, one of each class beside the
// rig's own, one of them of an issue of another project; and a pass after a filed issue's parent
// link moves rewrites that filing's root and no other filing.
func TestTheConsumerStoresEachLegionFilingAsMetricsClassesItAndMovesItsRootWithItsTree(t *testing.T) {
	run := newConsumer(t, 3)
	project := run.fixture.Project
	run.pass(t)
	whole := run.filings(t)
	compareFilings(t, run, project, "the whole read", whole)
	if len(whole) == 0 {
		t.Fatal("the rig's history holds no Legion filing, so this check has nothing to compare")
	}

	// Four filings the events feed carries to the next pass. Tree 0 starts 60 hours before the
	// rig's end: its implemented child is implementing from hour 4 to hour 14, and its reviewed
	// child is reviewing from hour 26 to hour 28.
	treeStart := run.fixture.To.Add(-60 * time.Hour)
	at := func(hours float64) time.Time { return treeStart.Add(time.Duration(hours * float64(time.Hour))) }
	root, implemented, reviewed := run.fixture.Keys[0], run.fixture.Keys[1], run.fixture.Keys[2]
	otherRoot, otherImplemented := run.fixture.Keys[6], run.fixture.Keys[7]
	run.exec(t, "add a project of its own for a filing across projects",
		`insert into projects (key, name, next_number) values ('OTHER', 'OTHER', 2)`)
	controller := `{"kind":"session","id":"s-controller","origin":{"session_title":"Legion controller · ` + project + `"}}`
	byController := run.fileIssue(t, project, 901, "", controller, at(9))
	later := run.fileIssue(t, project, 902, root, roleSession("implementer", implemented), at(10))
	outside := run.fileIssue(t, project, 903, "", roleSession("reviewer", reviewed), at(27))
	across := run.fileIssue(t, "OTHER", 1, "", roleSession("implementer", otherImplemented), at(12))
	run.pass(t)
	added := run.filings(t)
	compareFilings(t, run, project, "the pass that read the new filings", added)
	for _, want := range []struct {
		key, class, filer, phase, root string
	}{
		{byController, metrics.ClassByTheController, "controller", "", byController},
		{later, metrics.ClassInTreeLater, "implementer", "implementing", root},
		{outside, metrics.ClassOutOfTree, "reviewer", "reviewing", outside},
		{across, metrics.ClassOutOfTree, "implementer", "implementing", across},
	} {
		got, has := added[want.key]
		switch {
		case !has:
			t.Errorf("%s was filed by a Legion session of %s and has no stored filing", want.key, project)
		case got.Class != want.class || got.FilerRole != want.filer || got.FilerPhase != want.phase || got.RootKey != want.root:
			t.Errorf("%s is stored as %s, want class %s by %s in %q under %s",
				want.key, showFiling(got), want.class, want.filer, want.phase, want.root)
		}
	}

	// The filing out of every tree moves under the other tree's root: its class stays the one
	// its parent at the filing gave it, and its root is the tree it stands in now.
	run.exec(t, "move the filed issue under another root", `update issues set parent_key = $1 where key = $2`, otherRoot, outside)
	run.writeEventAs(t, outside, `{"kind":"user","id":"alice"}`, "issue.updated", `{"changed":["parent"]}`, run.at.Add(-time.Hour))
	run.pass(t)
	moved := run.filings(t)
	compareFilings(t, run, project, "the pass after the move", moved)
	if got := moved[outside]; got.RootKey != otherRoot || got.Class != metrics.ClassOutOfTree {
		t.Errorf("%s moved under %s and is stored as %s, want root %s and the class out_of_tree",
			outside, otherRoot, showFiling(got), otherRoot)
	}
	for key, was := range added {
		if key != outside && !sameFiling(moved[key], was) {
			t.Errorf("%s moved from %s to %s, and nothing moved it", key, showFiling(was), showFiling(moved[key]))
		}
	}
	t.Logf("%s: %d filings after the whole read, %d after the events feed's four (%s %s, %s %s, %s %s, %s %s), and %s's root went %s to %s after its parent link moved",
		project, len(whole), len(added), byController, added[byController].Class, later, added[later].Class,
		outside, added[outside].Class, across, added[across].Class, outside, added[outside].RootKey, moved[outside].RootKey)
}

// compareFilings holds the stored filings of project to the ones metrics.Compute's splitting
// table answers for that project over the whole history, field by field, and each stored root to
// the tree the same population places the filed issue in: Compute's own roots scope keeps a
// filing exactly where its filed issue is the root of its tree.
func compareFilings(t *testing.T, run *consumer, project, when string, stored map[string]Filing) {
	t.Helper()
	population, err := metrics.Read(t.Context(), run.database.Pool, nil, project, run.fixture.From.Add(-24*time.Hour), run.at)
	if err != nil {
		t.Fatalf("%s: read the history of %s: %v", when, project, err)
	}
	report := metrics.Compute(population, metrics.ScopeAll, run.at)
	roots := map[string]bool{}
	for _, filed := range metrics.Compute(population, metrics.ScopeRoots, run.at).Splitting.Filed {
		roots[filed.Key] = true
	}
	want := map[string]Filing{}
	for _, filed := range report.Splitting.Filed {
		want[filed.Key] = Filing{
			Project: project, Issue: filed.Key, FiledAt: filed.At, FilerRole: filed.Filer,
			FilerIssue: filed.FilerIssue, FilerPhase: filed.FilerPhase, Class: filed.Class,
			RootKey: population.TreeRoot(filed.Key),
		}
	}
	if got, wanted := slices.Sorted(maps.Keys(stored)), slices.Sorted(maps.Keys(want)); !slices.Equal(got, wanted) {
		t.Errorf("%s: the stored filings are %v, want the %v Compute counts", when, got, wanted)
	}
	classes := map[string]int{}
	for key, filing := range want {
		classes[filing.Class]++
		got := stored[key]
		if !sameFiling(got, filing) {
			t.Errorf("%s: %s is stored as %s, want %s", when, key, showFiling(got), showFiling(filing))
		}
		if (got.RootKey == key) != roots[key] {
			t.Errorf("%s: %s is stored under the root %s, and Compute's roots scope keeps it %t", when, key, got.RootKey, roots[key])
		}
	}
	t.Logf("%s: %d stored filings of %s compared with Compute's splitting table, by class %v", when, len(want), project, classes)
}

// sameFiling is whether two filings hold the same values, the moment compared as an instant.
func sameFiling(got, want Filing) bool {
	return got.Project == want.Project && got.Issue == want.Issue && got.FiledAt.Equal(want.FiledAt) &&
		got.FilerRole == want.FilerRole && got.FilerIssue == want.FilerIssue &&
		got.FilerPhase == want.FilerPhase && got.Class == want.Class && got.RootKey == want.RootKey
}

// filings is every stored filing of the history's project, by filed issue.
func (c *consumer) filings(t *testing.T) map[string]Filing {
	t.Helper()
	return c.filingsOf(t, c.fixture.Project)
}

// filingsOf is every stored filing of project, by filed issue.
func (c *consumer) filingsOf(t *testing.T, project string) map[string]Filing {
	t.Helper()
	stored, err := ReadFilings(t.Context(), c.database.Pool, project)
	if err != nil {
		t.Fatalf("read the stored filings of %s: %v", project, err)
	}
	return stored
}

// fileIssue writes one issue of project and the `issue.created` event actor wrote of it, with
// the parent it was filed under, and answers its key.
func (c *consumer) fileIssue(t *testing.T, project string, number int, parent, actor string, at time.Time) string {
	t.Helper()
	key := fmt.Sprintf("%s-%d", project, number)
	var parentKey *string
	if parent != "" {
		parentKey = &parent
	}
	c.exec(t, "write the filed issue "+key, `
		insert into issues (key, project_key, number, title, status, rank, parent_key, created_by, created_at)
		values ($1, $2, $3, 'A filed issue', 'triage', $4, $5, $6::jsonb, $7)
	`, key, project, number, fmt.Sprintf("%08d", number), parentKey, actor, at)
	c.writeEventAs(t, key, actor, "issue.created", fmt.Sprintf(`{"status":"triage","parent":%q}`, parent), at)
	return key
}

// writeEventAs writes one event of the issue by actor at the moment given.
func (c *consumer) writeEventAs(t *testing.T, issue, actor, kind, payload string, at time.Time) {
	t.Helper()
	c.exec(t, "write an event of "+issue, `
		insert into events (issue_key, seq, type, actor, payload, notify, created_at)
		values ($1, (select coalesce(max(seq), 0) + 1 from events where issue_key = $1),
		        $2, $3::jsonb, $4::jsonb, false, $5)
	`, issue, kind, actor, payload, at)
}

// roleSession is the actor of the session a Legion role works issue under, as the rig titles it.
func roleSession(role, issue string) string {
	return fmt.Sprintf(`{"kind":"session","id":"s-%s-%s","origin":{"session_title":%q}}`, role, issue, "Legion "+role+" · "+issue)
}

// showFiling is one filing written for a failure message.
func showFiling(filing Filing) string {
	return fmt.Sprintf("%s filed %s by %s of %q in %q, class %s, root %q",
		filing.Issue, filing.FiledAt.UTC().Format(time.RFC3339), filing.FilerRole, filing.FilerIssue,
		filing.FilerPhase, filing.Class, filing.RootKey)
}
