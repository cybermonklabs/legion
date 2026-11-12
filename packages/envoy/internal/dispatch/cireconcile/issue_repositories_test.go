package cireconcile

import (
	"reflect"
	"testing"
)

// nameOnAnIssue writes an issue of a project of its own that names repository as its own, as an
// issue write of the API does, and maps nothing: an issue's repository needs no mapping.
func (h *harness) nameOnAnIssue(key, repository string) {
	h.t.Helper()
	if _, err := h.pool.Exec(h.ctx, `
		insert into projects (key, name) values ('ISS', 'Issues') on conflict (key) do nothing
	`); err != nil {
		h.t.Fatalf("seed the project: %v", err)
	}
	if _, err := h.pool.Exec(h.ctx, `
		insert into issues (key, project_key, number, title, created_by, rank, repo)
		values ($1, 'ISS', (select coalesce(max(number), 0) + 1 from issues where project_key = 'ISS'),
			'Works elsewhere', '{"kind":"system","id":"test"}'::jsonb, $1, $2)
	`, key, repository); err != nil {
		h.t.Fatalf("write %s naming %s: %v", key, repository, err)
	}
}

// A head in a repository that an issue names and no project maps settles after one pass, as a
// head in a mapped repository does: the pass reads that repository's open pull requests and the
// head's listings, records the completed run, and the listener publishes the settlement. No
// mapping is written in this test, so a pass that read the mappings alone would read GitHub
// nothing at all and the head would never settle.
func TestAPassSettlesAHeadInARepositoryOnlyAnIssueNames(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.nameOnAnIssue("ISS-1", rowRepository)
	h.listHead()
	h.github.setCheckRuns(gitHubRepository, headSHA,
		checkRun(1, "ci-ok", "completed", "failure", otherApp, "2026-11-05T03:01:00Z"),
	)
	h.github.setCheckSuites(gitHubRepository, headSHA,
		checkSuite(900, "completed", "failure", otherApp, "2026-11-05T03:01:20Z"),
	)
	settlements := h.subscribe()

	h.pass()

	if sent := h.github.countPath("/repos/" + gitHubRepository + "/pulls"); sent != 1 {
		t.Fatalf("the open pull requests of %s were read %d times, want once", gitHubRepository, sent)
	}
	settled := h.settlement(settlements, "after a pass over a repository only an issue names")
	if settled.Number != pullNumber || settled.SHA != headSHA {
		t.Fatalf("the settlement is of %s at %s, want %s at %s", settled.Number, settled.SHA, pullNumber, headSHA)
	}
	if settled.Failed.Count != 1 || len(settled.FailingChecks) != 1 || settled.FailingChecks[0].Name != "ci-ok" {
		t.Fatalf("settlement = %+v, want ci-ok among its failing checks", settled)
	}
}

// The repositories a pass reads are the mappings' and the issues' together, each under the name
// it has today and each once, in one order. An issue that names a name a mapping records a rename
// of reads as the renamed repository; an issue naming what a mapping maps adds nothing; an issue
// naming no repository adds nothing.
func TestAPassReadsTheIssuesRepositoriesBesideTheMappings(t *testing.T) {
	h := newHarness(t, reviewerAppID)
	h.mapRenamedRepository(renamedAwayRow, rowRepository)
	h.mapRepository(unrenamedRow)
	h.nameOnAnIssue("ISS-1", renamedAwayRow)
	h.nameOnAnIssue("ISS-2", unrenamedRow)
	h.nameOnAnIssue("ISS-3", "beta/only-an-issue")
	h.nameOnAnIssue("ISS-4", "beta/only-an-issue")
	if _, err := h.pool.Exec(h.ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('ISS-5', 'ISS', 5, 'Names none', '{"kind":"system","id":"test"}'::jsonb, 'ISS-5')
	`); err != nil {
		t.Fatalf("write an issue naming no repository: %v", err)
	}

	repositories, err := h.reconciler.repositories(h.ctx)
	if err != nil {
		t.Fatalf("read the repositories: %v", err)
	}

	want := []string{rowRepository, "beta/only-an-issue", unrenamedRow}
	if !reflect.DeepEqual(repositories, want) {
		t.Fatalf("the pass reads %v, want %v: the mappings' and the issues' repositories, each under the name it has now, once, in order", repositories, want)
	}
}
