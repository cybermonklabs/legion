package legionfacts

import (
	"maps"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// TestEachStoredRowNamesTheRepositoryItOwns: a stored row's repo is the repository the issue owns
// by the server's one rule — its own, or the nearest one up its parent chain — and DefaultRepo
// (':default') where its chain names none, which stands for its project's default repository and
// is what the issue page resolves to there (refs.IssueRepository). The history's project has a
// default repository; the first tree's implemented child names another as its own, so it stores
// that one while the rest of its tree stores ':default'.
func TestEachStoredRowNamesTheRepositoryItOwns(t *testing.T) {
	seeded := seed(t, 2)
	project := seeded.fixture.Project
	own := seeded.fixture.Keys[1]
	exec := func(what, statement string, args ...any) {
		t.Helper()
		if _, err := seeded.database.Pool.Exec(t.Context(), statement, args...); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	exec("give the project a default repository", `update projects set default_repo = 'acme/widgets' where key = $1`, project)
	exec("give one issue a repository of its own", `update issues set repo = 'acme/gadgets' where key = $1`, own)

	rows, _, err := ComputeProject(t.Context(), seeded.database.Pool, project, seeded.now)
	if err != nil {
		t.Fatalf("compute the facts of %s: %v", project, err)
	}
	if _, err := WriteProject(t.Context(), seeded.database.Pool, ProjectWrite{Project: project, Rows: rows, Whole: true}); err != nil {
		t.Fatalf("write the facts of %s: %v", project, err)
	}
	stored, err := readWhole(t.Context(), seeded.database.Pool, project)
	if err != nil {
		t.Fatalf("read the facts of %s: %v", project, err)
	}
	byRepo := map[string]int{}
	for _, key := range slices.Sorted(maps.Keys(stored)) {
		resolved, err := refs.IssueRepository(t.Context(), seeded.database.Pool, key)
		if err != nil {
			t.Fatalf("resolve the repository of %s: %v", key, err)
		}
		shown := ""
		if resolved != nil {
			shown = *resolved
		}
		got := stored[key].Repo
		// A row that owns no repository stores ':default', which the issue page resolves to the
		// project's default repository; every other row stores what the page resolves.
		want := shown
		if got == DefaultRepo {
			want = "acme/widgets"
		}
		if shown != want {
			t.Errorf("%s stores the repository %q while the issue page resolves %q", key, got, shown)
		}
		byRepo[got]++
	}
	if stored[own].Repo != "acme/gadgets" || byRepo[DefaultRepo] == 0 {
		t.Errorf("%s stores %q and %d rows store %s, want its own acme/gadgets and the rest %s",
			own, stored[own].Repo, byRepo[DefaultRepo], DefaultRepo, DefaultRepo)
	}
	t.Logf("%s: %d stored rows read against refs.IssueRepository, by repository %v", project, len(stored), byRepo)
}

// TestADefaultRepositoryChangeRewritesNoStoredRepository (dispatch://INFRA-2916, R15 and plan
// D1 (vi)): a stored row's repo is the repository the issue owns, else DefaultRepo, so a write of
// the project's default repository (the project.updated event
// PUT /api/v1/projects/{key}/default-repository appends) resolves no row again and rewrites none.
// Only the cut of the Legion day rows that reach a repository through the default moves, at read
// time, under the published default the evals' reads take (evals.DayWindowIn).
func TestADefaultRepositoryChangeRewritesNoStoredRepository(t *testing.T) {
	c := newConsumer(t, 3)
	c.pass(t)
	project := c.fixture.Project
	closed := 0
	before := map[string]string{}
	for key, row := range c.rows(t) {
		if row.Repo != DefaultRepo {
			t.Fatalf("%s stores %q, want %s: no issue of the history owns a repository", key, row.Repo, DefaultRepo)
		}
		before[key] = row.Repo
		if row.ClosedAt != nil {
			closed++
		}
	}
	if closed == 0 {
		t.Fatal("the history holds no closed issue's row, so this test checks no row only a re-resolve would reach")
	}
	// The route's write: the project's row, then its project.updated event, in one transaction.
	c.exec(t, "give the project a default repository", `
		with written as (update projects set default_repo = 'acme/widgets' where key = $1 returning key)
		insert into events (project_key, seq, type, actor, payload, notify)
		select key, (select coalesce(max(seq), 0) + 1 from events where project_key = $1), 'project.updated',
		       '{"kind":"user","id":"alice"}'::jsonb, jsonb_build_object('key', key, 'default_repo', 'acme/widgets'), true
		from written
	`, project)
	changed := c.pass(t)
	for key, row := range c.rows(t) {
		resolved, err := refs.IssueRepository(t.Context(), c.database.Pool, key)
		if err != nil {
			t.Fatalf("resolve the repository of %s: %v", key, err)
		}
		if row.Repo != before[key] || resolved == nil || *resolved != "acme/widgets" {
			shown := "none"
			if resolved != nil {
				shown = *resolved
			}
			t.Errorf("%s (closed %t) stores %q after its project's default repository became acme/widgets, where it stored %q; the issue page resolves %s",
				key, row.ClosedAt != nil, row.Repo, before[key], shown)
		}
	}
	if len(changed) != 0 {
		t.Errorf("the pass after the default repository's write changed %d rows, want none", len(changed))
	}
	t.Logf("the pass after the default repository's write changed no row; its %d rows, %d of them closed, still store %s",
		len(before), closed, DefaultRepo)
}
