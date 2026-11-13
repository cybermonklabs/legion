package legionfacts

import (
	"maps"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// TestEachStoredRowNamesTheRepositoryTheIssuePageResolves: a stored row's repo is the repository
// the issue resolves to by the server's one rule, which the issue page reads and
// refs.IssueRepository reads for one key: the issue's own, the nearest one up its parent chain,
// else its project's default mapping. The history's project maps a default repository and a
// second one; the first tree's implemented child names the second as its own, so it reads that
// one while the rest of its tree reads the default.
func TestEachStoredRowNamesTheRepositoryTheIssuePageResolves(t *testing.T) {
	seeded := seed(t, 2)
	project := seeded.fixture.Project
	own := seeded.fixture.Keys[1]
	exec := func(what, statement string, args ...any) {
		t.Helper()
		if _, err := seeded.database.Pool.Exec(t.Context(), statement, args...); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	exec("map the project's repositories", `
		insert into repo_projects (repo, project, created_by, is_default)
		values ('acme/widgets', $1, '{"kind":"user","id":"alice"}', true),
		       ('acme/gadgets', $1, '{"kind":"user","id":"alice"}', false)
	`, project)
	exec("give one issue a repository of its own", `update issues set repo = 'acme/gadgets' where key = $1`, own)

	rows, err := ComputeProject(t.Context(), seeded.database.Pool, project, seeded.now)
	if err != nil {
		t.Fatalf("compute the facts of %s: %v", project, err)
	}
	if _, err := WriteProject(t.Context(), seeded.database.Pool, ProjectWrite{Project: project, Rows: rows, Whole: true}); err != nil {
		t.Fatalf("write the facts of %s: %v", project, err)
	}
	stored, err := Read(t.Context(), seeded.database.Pool, project)
	if err != nil {
		t.Fatalf("read the facts of %s: %v", project, err)
	}
	byRepo := map[string]int{}
	for _, key := range slices.Sorted(maps.Keys(stored)) {
		resolved, err := refs.IssueRepository(t.Context(), seeded.database.Pool, key)
		if err != nil {
			t.Fatalf("resolve the repository of %s: %v", key, err)
		}
		want := ""
		if resolved != nil {
			want = *resolved
		}
		if got := stored[key].Repo; got != want {
			t.Errorf("%s stores the repository %q, want the %q the issue page resolves", key, got, want)
		}
		byRepo[stored[key].Repo]++
	}
	if stored[own].Repo != "acme/gadgets" || byRepo["acme/widgets"] == 0 {
		t.Errorf("%s stores %q and %d rows store the default, want its own acme/gadgets and the rest the default",
			own, stored[own].Repo, byRepo["acme/widgets"])
	}
	t.Logf("%s: %d stored rows read against refs.IssueRepository, by repository %v", project, len(stored), byRepo)
}
