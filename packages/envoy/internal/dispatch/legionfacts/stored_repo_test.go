package legionfacts

import (
	"maps"
	"slices"
	"testing"
	"time"

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
		// The issue that names a repository of its own stores it, and the issue page resolves
		// it; every other row owns none, stores ':default' and resolves the project's default.
		wantStored, wantShown := DefaultRepo, "acme/widgets"
		if key == own {
			wantStored, wantShown = "acme/gadgets", "acme/gadgets"
		}
		if got != wantStored || shown != wantShown {
			t.Errorf("%s stores the repository %q and the issue page resolves %q, want %q and %q", key, got, shown, wantStored, wantShown)
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
// PUT /api/v1/projects/{key}/default-repository appends) resolves no row again and rewrites none,
// and a reader takes DefaultRepo for the project's default at read time. The pass after the write
// reads the project whole, so it computes every row again and a computation that stored the
// default it resolved would rewrite each of them.
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
	// The project's record of its whole read goes too, so the pass after the write computes
	// every row again.
	c.exec(t, "give the project a default repository", `
		with written as (update projects set default_repo = 'acme/widgets' where key = $1 returning key)
		insert into events (project_key, seq, type, actor, payload, notify)
		select key, (select coalesce(max(seq), 0) + 1 from events where project_key = $1), 'project.updated',
		       '{"kind":"user","id":"alice"}'::jsonb, jsonb_build_object('key', key, 'default_repo', 'acme/widgets'), true
		from written
	`, project)
	c.exec(t, "delete the project's record of its whole read", `delete from issue_facts_computed where project_key = $1`, project)
	c.at = c.at.Add(time.Minute)
	changed := c.pass(t)
	if at := computedWholeAt(t, c)[project]; !at.Equal(c.at) {
		t.Fatalf("the pass after the default repository's write did not read %s whole (its record stands at %s, the pass ran at %s), so it computed no row again",
			project, at.UTC(), c.at.UTC())
	}
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
		t.Errorf("the pass after the default repository's write computed every row again and changed %d of them, want none: %v", len(changed), changed)
	}
	t.Logf("the pass after the default repository's write read %s whole and changed no row; its %d rows, %d of them closed, still store %s",
		project, len(before), closed, DefaultRepo)
}

// TestARepositoryChangeMovesTheStoredRepositoryOfTheIssuesBelow (k1): a stored row's repo is the
// repository its issue resolves to by its parent chain (ReadRepos, refs.IssueRepoTerms), so a
// repository change of an issue moves the stored repo of each issue below it by that chain that
// names no repository of its own. The tree a pass descends is another walk: it takes the `tree`
// the daemon reports before the parent link (metrics.TreeLinks), so an issue the daemon reports
// under a root above its parent stands below the root there and not below the parent.
//
// The history is the rig's with one issue added: a middle issue under the first tree's root,
// filed by a person, with the root's implemented child moved beneath it and reported by the
// daemon under the root. The root names a repository of its own and the middle issue none, so
// the child stores the root's. The first pass reads the project whole, so every stored row is
// right before the change. The change gives the middle issue a repository of its own as the API
// writes one: the issue's row and its issue.updated event, whose `changed` names `repo`, in one
// statement. It names the middle issue alone, and the child is closed, so no input of its own
// brings it into the pass.
func TestARepositoryChangeMovesTheStoredRepositoryOfTheIssuesBelow(t *testing.T) {
	t.Parallel()
	run := newConsumer(t, 3)
	project := run.fixture.Project
	root, child := run.fixture.Roots[0], run.fixture.Keys[1]
	middle := run.fileIssue(t, project, 901, root, `{"kind":"user","id":"alice"}`, run.at.Add(-time.Hour))
	run.exec(t, "move "+child+" under "+middle, `update issues set parent_key = $1 where key = $2`, middle, child)
	run.exec(t, "give "+root+" a repository of its own", `update issues set repo = 'acme/roots' where key = $1`, root)
	seedTree(t, run.history, child, root)

	run.pass(t)
	readWhole := computedWholeAt(t, run)[project]
	before := run.rows(t)
	if row := before[child]; row.ClosedAt == nil || row.Repo != "acme/roots" || row.Tree != root {
		t.Fatalf("%s stores %s before the change, want a closed row under %s storing acme/roots", child, show(row), root)
	}

	run.exec(t, "give "+middle+" a repository of its own, as the API writes it", `
		with written as (update issues set repo = 'acme/middle' where key = $1 returning key, parent_key, repo)
		insert into events (issue_key, seq, type, actor, payload, notify, created_at)
		select key, (select coalesce(max(seq), 0) + 1 from events where issue_key = $1), 'issue.updated',
		       '{"kind":"user","id":"alice"}'::jsonb,
		       jsonb_build_object('key', key, 'parent', parent_key, 'repo', repo, 'resolved_repo', repo,
		                          'changed', jsonb_build_array('repo'), 'ancestors', jsonb_build_array(parent_key)),
		       true, $2
		from written
	`, middle, run.at)
	resolved, err := refs.IssueRepository(t.Context(), run.database.Pool, child)
	if err != nil {
		t.Fatalf("resolve the repository of %s: %v", child, err)
	}
	if resolved == nil || *resolved != "acme/middle" {
		t.Fatalf("the issue page resolves %s to %v after %s took acme/middle, so the change moved no repository below it and this test examined nothing",
			child, resolved, middle)
	}
	run.at = run.at.Add(time.Minute)
	changed := run.pass(t)

	if at := computedWholeAt(t, run)[project]; !at.Equal(readWhole) {
		t.Fatalf("the pass after the change read %s whole (its record went from %s to %s), so it computed every row and this test examined nothing",
			project, readWhole.UTC(), at.UTC())
	}
	after := run.rows(t)
	if after[child].Repo != "acme/middle" {
		t.Errorf("%s stands below %s by its parent chain and stores %q after %s took acme/middle, which the issue page resolves it to",
			child, middle, after[child].Repo, middle)
	}
	storedRowsAreTheWholeComputation(t, run, computedWhole(t, run), "the repository change of "+middle)
	t.Logf("%s took acme/middle; %s, below it by its parent chain and reported under %s, stored %q before the pass and %q after; the pass changed %d rows (%v)",
		middle, child, root, before[child].Repo, after[child].Repo, len(changed), changed)
}
