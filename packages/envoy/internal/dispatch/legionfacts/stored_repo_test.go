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

// TestAMappingChangeReResolvesEveryStoredRepository: a stored row's repository falls back to its
// project's default mapping, so the pass that reads a mapping's change (the `settings` change
// repo_projects logs, naming the repository) resolves every stored row again: once the project
// gains a default repository, the closed issues' rows, which no sweep recomputes and no feed
// names, store it, as the open ones do.
func TestAMappingChangeReResolvesEveryStoredRepository(t *testing.T) {
	c := newConsumer(t, 3)
	c.pass(t)
	project := c.fixture.Project
	closed := 0
	for key, row := range c.rows(t) {
		if row.Repo != "" {
			t.Fatalf("%s stores %q before the project maps any repository", key, row.Repo)
		}
		if row.ClosedAt != nil {
			closed++
		}
	}
	if closed == 0 {
		t.Fatal("the history holds no closed issue's row, so this test checks only rows the sweep recomputes")
	}
	c.exec(t, "give the project a default repository", `
		insert into repo_projects (repo, project, created_by, is_default) values ('acme/widgets', $1, '{"kind":"user","id":"alice"}', true)
	`, project)
	changed := c.pass(t)
	byRepo := map[string]int{}
	for key, row := range c.rows(t) {
		if row.Repo != "acme/widgets" {
			t.Errorf("%s (closed %t) stores %q after its project's default mapping became acme/widgets", key, row.ClosedAt != nil, row.Repo)
		}
		if row.ClosedAt != nil {
			byRepo[row.Repo]++
		}
	}
	t.Logf("the pass after the mapping changed %d rows; the %d closed rows by repository: %v", len(changed), closed, byRepo)
}
