package store

import (
	"context"
	"testing"
)

// projectsDefaultRepoVersion adds projects.default_repo and copies into it the repository of each
// repo_projects row that carries is_default (INFRA-4402).
const projectsDefaultRepoVersion = 20261119000230

// issueRepoTerms is the base's refs.IssueRepoTerms over every issue, held here letter for letter
// with 32 for refs.IssueDepthCap. It is a copy and not the package's constants, which this package
// cannot import and which would move with the rule they are to be compared with.
const issueRepoTerms = `
	with recursive subjects as (select key from issues), repo_chain as (
		select s.key as start, i.parent_key, i.repo, 0 as depth
		from subjects s join issues i on i.key = s.key
		union all
		select c.start, p.parent_key, p.repo, c.depth + 1
		from repo_chain c join issues p on p.key = c.parent_key
		where c.repo is null and c.depth < 32
	), repo as (
		select c.start as key,
		       max(c.repo) as owned,
		       bool_or(c.depth = 32 and c.repo is null and c.parent_key is not null) as too_deep
		from repo_chain c
		group by c.start
	)`

// baseIssueRepoRead is every issue's resolved repository by the base's rule: its own, else its
// nearest ancestor's, else the repository of its project's mapping row that carries is_default
// (the base's refs.IssueRepoJoin and IssueRepoResolved, copied).
const baseIssueRepoRead = issueRepoTerms + `
	select string_agg(i.key || '=' || coalesce(coalesce(repo.owned, repo_default.repo), 'none'), ' ' order by i.key)
	from issues i
	left join repo on repo.key = i.key
	left join repo_projects repo_default on repo_default.project = i.project_key and repo_default.is_default`

// headIssueRepoRead is the same read by this release's rule, whose default is the project's own
// default_repo (refs.IssueRepoJoin and IssueRepoResolved, copied).
const headIssueRepoRead = issueRepoTerms + `
	select string_agg(i.key || '=' || coalesce(coalesce(repo.owned, repo_default.default_repo), 'none'), ' ' order by i.key)
	from issues i
	left join repo on repo.key = i.key
	left join projects repo_default on repo_default.key = i.project_key`

// The migration over rows shaped as production holds them: INFRA has a default mapping row and two
// other rows, one of them the rename pair; BETA has rows and no default; GAMMA has none. Issues
// own a repository, inherit one from an ancestor, or take their project's default. Stored
// settings.repo_project.updated events record the mapping's writes.
//
// Its census counts 0. After it, each default is on its project and on no other project, each
// issue resolves to the repository it resolved to before, and every row of repo_projects, every
// column projects had, and every stored event are as they were. The boot's pass then writes the
// pair's new name in each field that held the old one, and still writes no row of the mapping and
// appends no event.
func TestTheDefaultRepositoryMigrationKeepsEveryResolvedRepositoryAndThePassWritesTheNewName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openMigratedTestStore(t, projectsDefaultRepoVersion-1)
	if _, err := store.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values
			('INFRA', 'Infra', 'alice'), ('BETA', 'Beta', 'alice'), ('GAMMA', 'Gamma', 'alice');
		insert into repo_projects (repo, project, created_by, is_default, renamed_to) values
			('cybermonklabs/legion', 'INFRA', '{"kind":"user","id":"alice"}', true, null),
			('sjawhar/legion', 'INFRA', '{"kind":"user","id":"alice"}', false, 'cybermonklabs/legion'),
			('sjawhar/dotfiles', 'INFRA', '{"kind":"user","id":"alice"}', false, null),
			('acme/widgets', 'BETA', '{"kind":"user","id":"alice"}', false, null),
			('acme/gadgets', 'BETA', '{"kind":"user","id":"alice"}', false, null);
		insert into issues (key, project_key, number, title, parent_key, repo, created_by, rank) values
			('INFRA-1', 'INFRA', 1, 'Owns the old name', null, 'sjawhar/legion', '{"kind":"user","id":"alice"}', 'U1'),
			('INFRA-2', 'INFRA', 2, 'Inherits the old name', 'INFRA-1', null, '{"kind":"user","id":"alice"}', 'U2'),
			('INFRA-3', 'INFRA', 3, 'Takes the default', null, null, '{"kind":"user","id":"alice"}', 'U3'),
			('INFRA-4', 'INFRA', 4, 'Takes the default below', 'INFRA-3', null, '{"kind":"user","id":"alice"}', 'U4'),
			('INFRA-5', 'INFRA', 5, 'Owns another', null, 'sjawhar/dotfiles', '{"kind":"user","id":"alice"}', 'U5'),
			('BETA-1', 'BETA', 1, 'Owns one', null, 'acme/widgets', '{"kind":"user","id":"alice"}', 'U1'),
			('BETA-2', 'BETA', 2, 'Inherits one', 'BETA-1', null, '{"kind":"user","id":"alice"}', 'U2'),
			('BETA-3', 'BETA', 3, 'Has none', null, null, '{"kind":"user","id":"alice"}', 'U3'),
			('GAMMA-1', 'GAMMA', 1, 'Has none', null, null, '{"kind":"user","id":"alice"}', 'U1'),
			('GAMMA-2', 'GAMMA', 2, 'Owns one', null, 'acme/elsewhere', '{"kind":"user","id":"alice"}', 'U2');
		insert into issue_external_links (issue_key, url, kind) values
			('INFRA-1', 'https://github.com/SJawhar/Legion/issues/1955', 'github_issue');
		insert into pull_request_heads (repo, number, head_sha, state, issue_key, url) values
			('sjawhar/legion', 7, 'a7', 'merged', 'INFRA-1', 'https://github.com/sjawhar/legion/pull/7');
		insert into branch_commits (repo, branch, sha, on_branch) values ('sjawhar/legion', 'main', 'c1', true);
		insert into architecture_sources (project_key, repo, branch, installation_id, created_by) values
			('INFRA', 'sjawhar/legion', 'main', 1, '{"kind":"user","id":"alice"}');
		insert into events (project_key, seq, type, actor, payload, notify) values
			('INFRA', 1, 'settings.repo_project.updated', '{"kind":"user","id":"alice"}',
				'{"repo":"cybermonklabs/legion","project":"INFRA","is_default":true}', false),
			('INFRA', 2, 'settings.repo_project.updated', '{"kind":"user","id":"alice"}',
				'{"repo":"sjawhar/legion","project":"INFRA","renamed_to":"cybermonklabs/legion"}', false);
	`); err != nil {
		t.Fatalf("seed as the release before %d wrote: %v", projectsDefaultRepoVersion, err)
	}

	if got := censusCount(t, store, projectsDefaultRepoVersion); got != 0 {
		t.Fatalf("the census at %d = %d, want 0", projectsDefaultRepoVersion-1, got)
	}
	const wantResolved = "BETA-1=acme/widgets BETA-2=acme/widgets BETA-3=none GAMMA-1=none GAMMA-2=acme/elsewhere " +
		"INFRA-1=sjawhar/legion INFRA-2=sjawhar/legion INFRA-3=cybermonklabs/legion INFRA-4=cybermonklabs/legion INFRA-5=sjawhar/dotfiles"
	resolved := readText(t, store, baseIssueRepoRead)
	if resolved != wantResolved {
		t.Fatalf("the resolved repositories at %d = %s, want %s", projectsDefaultRepoVersion-1, resolved, wantResolved)
	}
	const mappingRows = `select jsonb_agg(to_jsonb(r) order by r.repo)::text from repo_projects r`
	const storedEvents = `select jsonb_agg(to_jsonb(e) order by e.id)::text from events e`
	mapping := readText(t, store, mappingRows)
	events := readText(t, store, storedEvents)
	projects := readText(t, store, `select jsonb_agg(to_jsonb(p) order by p.key)::text from projects p`)

	if err := store.migrate(ctx, migrationsThrough(t, projectsDefaultRepoVersion)); err != nil {
		t.Fatalf("migrate to %d: %v", projectsDefaultRepoVersion, err)
	}

	if got := readText(t, store, `select string_agg(key || '=' || coalesce(default_repo, 'none'), ' ' order by key) from projects`); got != "BETA=none GAMMA=none INFRA=cybermonklabs/legion" {
		t.Errorf("the defaults after %d = %s, want INFRA's on INFRA and none elsewhere", projectsDefaultRepoVersion, got)
	}
	if got := readText(t, store, headIssueRepoRead); got != resolved {
		t.Errorf("the resolved repositories after %d = %s, want the %s they were", projectsDefaultRepoVersion, got, resolved)
	}
	if got := readText(t, store, mappingRows); got != mapping {
		t.Errorf("repo_projects after %d:\n%s\nwant it as it was:\n%s", projectsDefaultRepoVersion, got, mapping)
	}
	if got := readText(t, store, `select jsonb_agg(to_jsonb(p) - 'default_repo' order by p.key)::text from projects p`); got != projects {
		t.Errorf("projects' columns but default_repo after %d:\n%s\nwant them as they were:\n%s", projectsDefaultRepoVersion, got, projects)
	}
	if got := readText(t, store, storedEvents); got != events {
		t.Errorf("the events after %d:\n%s\nwant them as they were:\n%s", projectsDefaultRepoVersion, got, events)
	}

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate to the head: %v", err)
	}
	pairs, err := ReadRepoRenames(ctx, store.Pool)
	if err != nil {
		t.Fatalf("read the pairs: %v", err)
	}
	if pass := RewriteRenamedRepositories(ctx, store.Pool, pairs); pass.Pairs != 1 || pass.Failures != 0 || pass.Left != noneLeft {
		t.Fatalf("the pass = %+v, want one pair, no failure and nothing left", pass)
	}
	for query, want := range map[string]string{
		headIssueRepoRead: "BETA-1=acme/widgets BETA-2=acme/widgets BETA-3=none GAMMA-1=none GAMMA-2=acme/elsewhere " +
			"INFRA-1=cybermonklabs/legion INFRA-2=cybermonklabs/legion INFRA-3=cybermonklabs/legion INFRA-4=cybermonklabs/legion INFRA-5=sjawhar/dotfiles",
		`select string_agg(key || '=' || coalesce(repo, 'none'), ' ' order by key) from issues where project_key = 'INFRA'`: "INFRA-1=cybermonklabs/legion INFRA-2=none INFRA-3=none INFRA-4=none INFRA-5=sjawhar/dotfiles",
		`select url from issue_external_links`:                                                            "https://github.com/cybermonklabs/legion/issues/1955",
		`select repo || ' ' || url from pull_request_heads`:                                               "cybermonklabs/legion https://github.com/cybermonklabs/legion/pull/7",
		`select repo from branch_commits`:                                                                 "cybermonklabs/legion",
		`select repo from architecture_sources`:                                                           "cybermonklabs/legion",
		`select string_agg(key || '=' || coalesce(default_repo, 'none'), ' ' order by key) from projects`: "BETA=none GAMMA=none INFRA=cybermonklabs/legion",
		mappingRows:  mapping,
		storedEvents: events,
	} {
		if got := readText(t, store, query); got != want {
			t.Errorf("after the pass, %s\n= %s\nwant %s", query, got, want)
		}
	}
}
