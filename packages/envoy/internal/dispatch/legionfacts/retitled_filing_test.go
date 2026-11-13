package legionfacts

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion/legiontest"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
)

// TestARetitledFilingSessionMovesItsFilingToTheProjectItsNewTitleNamesInOnePass: a filing is the
// project's whose Legion session filed it, by that session's newest title, as metrics reads it
// (legion.SessionTitle's OfProject). A session titled for an issue of LEGION files an issue, and
// later writes on an issue of OTHER under a title that names it: from then on metrics counts the
// filing for OTHER and not for LEGION. The one pass that reads the retitling event takes the
// filing out of LEGION's issue_filings and writes it into OTHER's, classified by the rule
// metrics classifies it by, while neither project is read whole.
func TestARetitledFilingSessionMovesItsFilingToTheProjectItsNewTitleNamesInOnePass(t *testing.T) {
	run := newConsumer(t, 2)
	project := run.fixture.Project
	other := legiontest.Seed(t, run.database.Pool, legiontest.Spec{Project: "OTHER", Trees: 1, Now: run.now})
	run.pass(t)

	// Tree 0 of each project starts 60 hours before the end of the history: LEGION's reviewed
	// child is reviewing from hour 26 to hour 28, and OTHER's root is accepting from hour 20 to
	// hour 30.
	treeStart := run.fixture.To.Add(-60 * time.Hour)
	at := func(hours float64) time.Time { return treeStart.Add(time.Duration(hours * float64(time.Hour))) }
	reviewed, otherRoot := run.fixture.Keys[2], other.Roots[0]
	const session = "s-retitled-filer"
	before := `{"kind":"session","id":"` + session + `","origin":{"session_title":"Legion reviewer · ` + reviewed + `"}}`
	after := `{"kind":"session","id":"` + session + `","origin":{"session_title":"Legion implementer · ` + otherRoot + `"}}`
	filed := run.fileIssue(t, project, 905, "", before, at(27))
	run.pass(t)
	if got, has := run.filings(t)[filed]; !has || got.FilerIssue != reviewed {
		t.Fatalf("%s was filed by a session titled for %s and %s stores %s (stored %t), want its filing",
			filed, reviewed, project, showFiling(got), has)
	}
	wholeReads := run.wholeReads(t)

	run.at = run.at.Add(time.Minute)
	run.writeEventAs(t, otherRoot, after, "comment.created", `{}`, run.at.Add(-time.Hour))
	run.pass(t)

	left, joined := run.filings(t), run.filingsOf(t, other.Project)
	compareFilings(t, run, project, "after the retitle", left)
	compareFilings(t, run, other.Project, "after the retitle", joined)
	if got, has := left[filed]; has {
		t.Errorf("%s's session is titled for %s now and %s still stores its filing: %s", filed, otherRoot, project, showFiling(got))
	}
	got, has := joined[filed]
	switch {
	case !has:
		t.Errorf("%s's session is titled for %s now and %s stores no filing of it", filed, otherRoot, other.Project)
	case got.FilerRole != "implementer" || got.FilerIssue != otherRoot || got.FilerPhase != "accepting" ||
		got.Class != metrics.ClassOutOfTree || got.RootKey != filed:
		t.Errorf("%s is stored in %s as %s, want the implementer of %s in accepting, out_of_tree, under its own root",
			filed, other.Project, showFiling(got), otherRoot)
	}
	if now := run.wholeReads(t); !sameWholeReads(now, wholeReads) {
		t.Errorf("the pass that read the retitle read a project whole: the records were %v and are %v", wholeReads, now)
	}
	_, stillLeft := left[filed]
	t.Logf("%s filed by %s, titled for %s then for %s: after one pass %s holds %d filings (%s among them: %t) and %s holds %d, %s among them as %s; whole reads before and after %v",
		filed, session, reviewed, otherRoot, project, len(left), filed, stillLeft, other.Project, len(joined), filed, showFiling(got), wholeReads)
}

// wholeReads is when each project was last read whole, from the record each whole read writes
// (markComputedWhole).
func (c *consumer) wholeReads(t *testing.T) map[string]time.Time {
	t.Helper()
	rows, err := c.database.Pool.Query(t.Context(), `select project_key, computed_at from issue_facts_computed`)
	if err != nil {
		t.Fatalf("read the whole reads: %v", err)
	}
	defer rows.Close()
	reads := map[string]time.Time{}
	for rows.Next() {
		var project string
		var at time.Time
		if err := rows.Scan(&project, &at); err != nil {
			t.Fatalf("read the whole reads: %v", err)
		}
		reads[project] = at
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the whole reads: %v", err)
	}
	return reads
}

// sameWholeReads is whether two records of the whole reads name the same projects at the same
// moments.
func sameWholeReads(got, want map[string]time.Time) bool {
	if len(got) != len(want) {
		return false
	}
	for project, at := range want {
		if !got[project].Equal(at) {
			return false
		}
	}
	return true
}
