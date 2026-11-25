package delivery

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// oneRuleRepositories are the repositories of the one-rule seed, each with an issue-less pull
// request Delivery holds to the rule.
var oneRuleRepositories = []string{"acme/a", "acme/adefault", "acme/b", "acme/bmapped", "acme/c", "acme/cdefault", "acme/none", "acme/shared"}

// oneRuleOwners is what the seed's issues give each project: PA's PA-1 names acme/a, its child
// PA-2 names none, PA-3 names none and takes PA's default acme/adefault, PA-4 names acme/shared;
// PB's PB-1, done, names acme/b and its child PB-3 names none, PB-2 names acme/shared, and PB maps
// acme/bmapped, which gives it nothing; PC's PC-1 names acme/c, and PC-2 and PC-3 are each the
// other's parent, a chain the walk cannot finish, so PC's default acme/cdefault, to which no issue
// of it resolves, is not its repository.
var oneRuleOwners = map[string][]string{
	"PA": {"acme/a", "acme/adefault", "acme/shared"},
	"PB": {"acme/b", "acme/shared"},
	"PC": {"acme/c"},
}

// seedOneRule writes the one-rule seed: projects PA, PB and PC, PA's and PC's defaults on the
// projects themselves (projects.default_repo, where #4457 keeps a default), their issues, PB's
// mapping, and an issue-less pull request in each repository.
func seedOneRule(t *testing.T) *store.Pool {
	t.Helper()
	pool, ctx := deliveryTestPool(t)
	for _, statement := range []string{
		`insert into projects (key, name, default_assignee, default_repo) values
			('PA', 'PA', 'olga', 'acme/adefault'), ('PB', 'PB', 'olga', null), ('PC', 'PC', 'olga', 'acme/cdefault')`,
		`insert into issues (key, project_key, number, title, created_by, rank, parent_key, repo, status) values
			('PA-1', 'PA', 1, 'a', '{"kind":"user","id":"olga"}', 'a', null, 'acme/a', 'triage'),
			('PA-2', 'PA', 2, 'a', '{"kind":"user","id":"olga"}', 'b', 'PA-1', null, 'triage'),
			('PA-3', 'PA', 3, 'a', '{"kind":"user","id":"olga"}', 'c', null, null, 'triage'),
			('PA-4', 'PA', 4, 'a', '{"kind":"user","id":"olga"}', 'd', null, 'acme/shared', 'triage'),
			('PB-1', 'PB', 1, 'b', '{"kind":"user","id":"olga"}', 'a', null, 'acme/b', 'done'),
			('PB-2', 'PB', 2, 'b', '{"kind":"user","id":"olga"}', 'b', null, 'acme/shared', 'triage'),
			('PB-3', 'PB', 3, 'b', '{"kind":"user","id":"olga"}', 'c', 'PB-1', null, 'triage'),
			('PC-1', 'PC', 1, 'c', '{"kind":"user","id":"olga"}', 'a', null, 'acme/c', 'triage'),
			('PC-2', 'PC', 2, 'c', '{"kind":"user","id":"olga"}', 'b', null, null, 'triage'),
			('PC-3', 'PC', 3, 'c', '{"kind":"user","id":"olga"}', 'c', 'PC-2', null, 'triage')`,
		`update issues set parent_key = 'PC-3' where key = 'PC-2'`,
		`insert into repo_projects (repo, project, is_default, created_by) values
			('acme/bmapped', 'PB', false, '{"kind":"user","id":"olga"}')`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed the one rule: %v\n%s", err, statement)
		}
	}
	merged := time.Date(2026, 10, 1, 7, 0, 0, 0, time.UTC)
	opened := merged.Add(-time.Hour)
	for _, repository := range oneRuleRepositories {
		if err := UpsertPullRequest(ctx, pool, DeliveryPullRequest{
			Repo: repository, Number: 1, Title: "change", Author: "octocat",
			URL: "https://github.com/" + repository + "/pull/1", CreatedAt: &opened, MergedAt: &merged,
		}); err != nil {
			t.Fatalf("store the pull request of %s: %v", repository, err)
		}
	}
	return pool
}

// TestOneRuleGivesEveryReaderTheSameRepositoriesOfAProject: refs.ProjectRepositories, the
// repository rule (refs.RepositoryClause) and Delivery's pull requests that name no issue
// (pullRequestShownSQL) read one text, so for each project of seedOneRule they give it the same
// repositories:
//
//   - ProjectRepositories(PA) and (PB) answer oneRuleOwners' lists, and (PC), whose chain the
//     walk cannot finish, refuses with an *UnresolvedChainError naming PC-2;
//   - dave, a restricted member who sees that project alone, reads exactly those repositories
//     (PC's unresolved chain gives him none of its own, so acme/c alone);
//   - olga, an open viewer who sees every project but that one, made private, is hidden exactly
//     the project's repositories that no project she sees also owns, by the rule and by Delivery
//     alike (so acme/shared stays shown to her while PA or PB is private); PC's unresolved chain
//     resolves to no repository (plan v89), so while PC is private only acme/c is hidden from her.
//
// Controls, each a one-line drift in one copy: Delivery's pull requests that name no issue shown to
// every open viewer (pullRequestShownSQL without refs.RepositoryClause), and olga is hidden nothing
// by Delivery; ProjectRepositories answering a project with an unresolved chain as its list, and
// PC answers [acme/c].
func TestOneRuleGivesEveryReaderTheSameRepositoriesOfAProject(t *testing.T) {
	pool := seedOneRule(t)
	ctx := context.Background()
	projects := slices.Sorted(maps.Keys(oneRuleOwners))
	for _, project := range projects {
		got, err := refs.ProjectRepositories(ctx, pool, project)
		var unresolved *refs.UnresolvedChainError
		switch {
		case project == "PC":
			if !errors.As(err, &unresolved) || unresolved.Issue != "PC-2" {
				t.Errorf("ProjectRepositories(PC) = %q, %v, want an *UnresolvedChainError naming PC-2", got, err)
			}
		case err != nil || !slices.Equal(got, oneRuleOwners[project]):
			t.Errorf("ProjectRepositories(%s) = %q, %v, want %q", project, got, err, oneRuleOwners[project])
		}
	}
	for _, project := range projects {
		if _, err := pool.Exec(ctx, `update projects set visibility = case when key = $1 then 'private' else 'public' end`, project); err != nil {
			t.Fatalf("make %s private: %v", project, err)
		}
		dave := refs.VisibilityArg(model.Actor{Viewer: &model.Viewer{Person: "dave", Restricted: true, Sees: []string{project}, AnyPrivate: true}})
		if got := oneRuleShown(t, pool, dave); !slices.Equal(got, oneRuleOwners[project]) {
			t.Errorf("while %s is private, dave, restricted and seeing %s alone, reads %q by the rule, want %q", project, project, got, oneRuleOwners[project])
		}
		others := slices.DeleteFunc(slices.Clone(projects), func(key string) bool { return key == project })
		seen := map[string]bool{}
		for _, other := range others {
			for _, repository := range oneRuleOwners[other] {
				seen[repository] = true
			}
		}
		want := []string{}
		for _, repository := range oneRuleRepositories {
			if !seen[repository] && slices.Contains(oneRuleOwners[project], repository) {
				want = append(want, repository)
			}
		}
		olga := refs.VisibilityArg(model.Actor{Viewer: &model.Viewer{Person: "olga", Open: true, Sees: others, AnyPrivate: true}})
		if got := oneRuleRead(t, pool, olga, `not `+refs.RepositoryClause("pr.repo", "$1")); !slices.Equal(got, want) {
			t.Errorf("while %s is private, olga, open and seeing %q, is hidden %q by the rule, want %q", project, others, got, want)
		}
		if got := oneRuleRead(t, pool, olga, `not `+pullRequestShownSQL("pr", "i", "$1")); !slices.Equal(got, want) {
			t.Errorf("while %s is private, olga, open and seeing %q, is hidden the pull requests of %q by Delivery, want %q", project, others, got, want)
		}
	}
}

// oneRuleShown is the repositories of oneRuleRepositories the viewer bound as viewer reads by the
// rule, sorted.
func oneRuleShown(t *testing.T, pool *store.Pool, viewer any) []string {
	t.Helper()
	return oneRuleRead(t, pool, viewer, refs.RepositoryClause("pr.repo", "$1"))
}

// oneRuleRead is the repositories whose issue-less pull request the condition rule, over the
// delivery_pull_requests row pr and its issue i, holds for the viewer bound as viewer, sorted.
func oneRuleRead(t *testing.T, pool *store.Pool, viewer any, rule string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), fmt.Sprintf(`
		select pr.repo from delivery_pull_requests pr left join issues i on i.key = pr.issue_key
		where %s order by pr.repo collate "C"`, rule), viewer)
	if err != nil {
		t.Fatalf("read the pull requests by %s: %v", rule[:min(len(rule), 80)], err)
	}
	defer rows.Close()
	repositories := []string{}
	for rows.Next() {
		var repository string
		if err := rows.Scan(&repository); err != nil {
			t.Fatal(err)
		}
		repositories = append(repositories, repository)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return repositories
}
