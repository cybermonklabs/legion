package cireconcile

import (
	"reflect"
	"testing"
)

// unrenamedRow is a mapping that was never renamed and sorts after the renamed pair, so a read
// that answered one repository for the whole table would drop it rather than the pair.
const unrenamedRow = "zed/other"

// Every mapped repository is read, whatever any other mapping was renamed to: a pass over a
// renamed pair and a mapping of its own reads two repositories, the pair contributing the name
// it has today once. A rename is one mapping's fact, so a reader that let one mapping's
// renamed_to stand for every row would leave every repository but that one out of the pass —
// no head read, no record mended, no settlement — for as long as any rename is recorded.
func TestEveryMappingIsReadWhenAnotherNamesARename(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRepository(rowRepository)
	h.mapRenamedRepository(renamedAwayRow, rowRepository)
	h.mapRepository(unrenamedRow)

	repositories, err := h.reconciler.mapped(h.ctx)
	if err != nil {
		t.Fatalf("read the mapped repositories: %v", err)
	}

	want := []string{rowRepository, unrenamedRow}
	if !reflect.DeepEqual(repositories, want) {
		t.Fatalf("the pass reads %v, want %v: each mapped repository under the name it has now, once", repositories, want)
	}
}

// A repository an issue names and no project maps is reconciled like any other, so a head there
// settles. An issue's own repository is any `owner/name`, and work on such an issue opens pull
// requests whose heads the listener records and whose settlements the Legion daemon waits for;
// a repair that read repo_projects alone would never read those heads, so a webhook GitHub
// never delivered there would hold the head short of a check for ever -- and silently, since
// nothing ever asked GitHub about that repository to skip it.
func TestAHeadSettlesInARepositoryOnlyAnIssueNames(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.issueInRepository(rowRepository)
	h.listHead()
	h.github.setCheckRuns(gitHubRepository, headSHA,
		checkRun(1, "ci-ok", "completed", "success", otherApp, "2026-11-12T04:01:00Z"),
	)
	h.github.setCheckSuites(gitHubRepository, headSHA,
		checkSuite(900, "completed", "success", otherApp, "2026-11-12T04:01:20Z"),
	)
	var mapped int
	if err := h.pool.QueryRow(h.ctx, `select count(*) from repo_projects`).Scan(&mapped); err != nil {
		t.Fatalf("count the repository mappings: %v", err)
	}
	if mapped != 0 {
		t.Fatalf("repo_projects holds %d rows, want none: this test is about a repository no project maps", mapped)
	}
	settlements := h.subscribe()

	h.pass()

	settled := h.settlement(settlements, "after a pass over the repository the issue names")
	if settled.Number != pullNumber || settled.SHA != headSHA {
		t.Fatalf("the settlement is of %s at %s, want %s at %s", settled.Number, settled.SHA, pullNumber, headSHA)
	}
	if settled.Failed.Count != 0 || settled.Passed.Count != 1 || len(settled.Passed.Checks) != 1 || settled.Passed.Checks[0] != "ci-ok" {
		t.Fatalf("settlement = %+v, want ci-ok alone passing", settled)
	}
}
