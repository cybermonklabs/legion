package refs

import (
	"context"
	"maps"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// clauseRepositories are the repositories the clause's seed (seedRepositoryIssues) names, each
// for one way an issue resolves to a repository or none does.
var clauseRepositories = []string{
	"acme/pub", "acme/priv", "acme/privdone", "acme/privparent", "acme/privdefault", "acme/none", "acme/both",
	"acme/oldname", "acme/newname",
}

// seedRepositoryIssues writes PUB, public, and PRIV and PRIV2, private, with an issue for each
// way an issue resolves to a repository: PUB-1 names acme/pub; PRIV-1 names acme/priv; PRIV-2
// names acme/privdone and is done; PRIV-3 names none and is a child of PRIV2-1, which names
// acme/privparent; PRIV-4 names none and resolves to PRIV's default repository, acme/privdefault;
// PUB-2 and PRIV-5 both name acme/both; and PRIV-6 names acme/oldname, which a mapping of PRIV
// records as renamed to acme/newname. No issue names acme/none or acme/newname. The default and the
// rename are stored as this build stores them, in repo_projects, in the one statement below.
func seedRepositoryIssues(t *testing.T) *store.Store {
	t.Helper()
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(context.Background(), `
		insert into projects (key, name, default_assignee, visibility) values
			('PUB', 'Public', 'alice', 'public'), ('PRIV', 'Private', 'alice', 'private'), ('PRIV2', 'Private two', 'alice', 'private');
		insert into issues (key, project_key, number, title, created_by, rank, parent_key, repo, status) values
			('PUB-1', 'PUB', 1, 'Names acme/pub', '{"kind":"user","id":"alice"}', 'a', null, 'acme/pub', 'triage'),
			('PUB-2', 'PUB', 2, 'Names acme/both', '{"kind":"user","id":"alice"}', 'b', null, 'acme/both', 'triage'),
			('PRIV2-1', 'PRIV2', 1, 'Names acme/privparent', '{"kind":"user","id":"alice"}', 'a', null, 'acme/privparent', 'triage'),
			('PRIV-1', 'PRIV', 1, 'Names acme/priv', '{"kind":"user","id":"alice"}', 'a', null, 'acme/priv', 'triage'),
			('PRIV-2', 'PRIV', 2, 'Names acme/privdone', '{"kind":"user","id":"alice"}', 'b', null, 'acme/privdone', 'done'),
			('PRIV-3', 'PRIV', 3, 'Under PRIV2-1', '{"kind":"user","id":"alice"}', 'c', 'PRIV2-1', null, 'triage'),
			('PRIV-4', 'PRIV', 4, 'Names none', '{"kind":"user","id":"alice"}', 'd', null, null, 'triage'),
			('PRIV-5', 'PRIV', 5, 'Names acme/both', '{"kind":"user","id":"alice"}', 'e', null, 'acme/both', 'triage'),
			('PRIV-6', 'PRIV', 6, 'Names acme/oldname', '{"kind":"user","id":"alice"}', 'f', null, 'acme/oldname', 'triage');
		insert into repo_projects (repo, project, is_default, renamed_to, created_by) values
			('acme/privdefault', 'PRIV', true, null, '{"kind":"user","id":"alice"}'),
			('acme/oldname', 'PRIV', false, 'acme/newname', '{"kind":"user","id":"alice"}');
	`); err != nil {
		t.Fatalf("seed the repositories' issues: %v", err)
	}
	return database
}

// TestRepositoryClauseShowsARepositoryByTheIssuesThatResolveToIt: a repository is hidden from a
// viewer when a private project the viewer does not see owns it: an issue of that project resolves
// to it, through the issue's own repository, its parent's or its project's default, whether the
// issue is open or done, or the project maps it, under any name a rename gives it. Any other
// repository passes for an open viewer, and for any other viewer only when a project it sees owns
// it. alice, open and seeing PUB, reads acme/pub and the unnamed acme/none, and not acme/both,
// which PRIV-5 names beside PUB-2, nor acme/newname, PRIV's acme/oldname under its new name; bob, a
// restricted member seeing PRIV, reads every repository PRIV owns that no other private project
// does: acme/newname among them, and acme/both, which public PUB shares, and not acme/privparent,
// which private PRIV2's PRIV2-1 names; carol, seeing every project, and the server's own viewer
// read all nine; the closed viewer reads none. Names are compared without case, and the clause is
// never null.
func TestRepositoryClauseShowsARepositoryByTheIssuesThatResolveToIt(t *testing.T) {
	database := seedRepositoryIssues(t)
	for _, viewer := range []struct {
		name  string
		who   model.Actor
		reads []string
	}{
		{"alice", model.Actor{Viewer: &model.Viewer{Person: "alice", Open: true, Sees: []string{"PUB"}, AnyPrivate: true}},
			[]string{"acme/pub", "acme/none"}},
		{"bob", model.Actor{Viewer: &model.Viewer{Person: "bob", Restricted: true, Sees: []string{"PRIV"}, AnyPrivate: true}},
			[]string{"acme/priv", "acme/privdone", "acme/privdefault", "acme/both", "acme/oldname", "acme/newname"}},
		{"carol", model.Actor{Viewer: &model.Viewer{Person: "carol", Open: true, Sees: []string{"PRIV", "PRIV2", "PUB"}, AnyPrivate: true}},
			clauseRepositories},
		{"the server's own viewer", model.Actor{Viewer: model.ServerViewer}, clauseRepositories},
		{"the closed viewer", model.Actor{}, nil},
	} {
		for _, repository := range clauseRepositories {
			if got, want := repositoryPasses(t, database, viewer.who, repository), slices.Contains(viewer.reads, repository); got != want {
				t.Errorf("RepositoryClause(%s) for %s = %t, want %t", repository, viewer.name, got, want)
			}
		}
	}
	bob := model.Actor{Viewer: &model.Viewer{Person: "bob", Restricted: true, Sees: []string{"PRIV"}, AnyPrivate: true}}
	if !repositoryPasses(t, database, bob, "ACME/Priv") {
		t.Errorf("RepositoryClause(ACME/Priv) for bob = false, want true: GitHub spells a name in any case")
	}
	var answered *bool
	if err := database.Pool.QueryRow(context.Background(), `select `+RepositoryClause("null::text", "$1"),
		VisibilityArg(model.Actor{})).Scan(&answered); err != nil || answered == nil || *answered {
		t.Errorf("RepositoryClause over no repository for the closed viewer = %v (%v), want false and never null", answered, err)
	}
}

// repositoryPasses answers RepositoryClause for who over repository.
func repositoryPasses(t *testing.T, database *store.Store, who model.Actor, repository string) bool {
	t.Helper()
	var passes bool
	if err := database.Pool.QueryRow(context.Background(), `select `+RepositoryClause("$2", "$1"),
		VisibilityArg(who), repository).Scan(&passes); err != nil {
		t.Fatalf("RepositoryClause(%s): %v", repository, err)
	}
	return passes
}

// seedHiddenRepositories adds to seedRepositoryIssues' rows one of each way a repository was shown
// to a viewer who does not see a project that holds it: PUB-3, which names acme/privmapped, a
// repository PRIV maps (an issue's own repository may be any owner/name, so any full member can
// write it); acme/privonlymapped, which PRIV maps and no issue resolves to; PUB-5, which names
// acme/pubwalk, and its child PRIV-8, which names none and so resolves to it; and acme/movedold,
// which a PUB mapping records as renamed to acme/movednew, the repository PRIV-7 names.
func seedHiddenRepositories(t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into issues (key, project_key, number, title, created_by, rank, parent_key, repo, status) values
			('PUB-3', 'PUB', 3, 'Names acme/privmapped', '{"kind":"user","id":"alice"}', 'c', null, 'acme/privmapped', 'triage'),
			('PUB-5', 'PUB', 5, 'Names acme/pubwalk', '{"kind":"user","id":"alice"}', 'e', null, 'acme/pubwalk', 'triage'),
			('PRIV-7', 'PRIV', 7, 'Names acme/movednew', '{"kind":"user","id":"alice"}', 'g', null, 'acme/movednew', 'triage'),
			('PRIV-8', 'PRIV', 8, 'Under PUB-5', '{"kind":"user","id":"alice"}', 'h', 'PUB-5', null, 'triage');
		insert into repo_projects (repo, project, is_default, renamed_to, created_by) values
			('acme/privmapped', 'PRIV', false, null, '{"kind":"user","id":"alice"}'),
			('acme/privonlymapped', 'PRIV', false, null, '{"kind":"user","id":"alice"}'),
			('acme/movedold', 'PUB', false, 'acme/movednew', '{"kind":"user","id":"alice"}');
	`); err != nil {
		t.Fatalf("seed the hidden repositories: %v", err)
	}
}

// TestProjectRepositoriesSQLAnswersEveryProjectThatOwnsARepository: ProjectRepositoriesSQL, the rule
// every repository check reads, answers for a repository each project that owns it, apart from any
// viewer: a project owns each repository an issue of it resolves to, through the issue's own
// repository, its parent's or its project's default, open or done, and each repository it maps,
// under every name a rename gives it and in any case. Over seedRepositoryIssues' and
// seedHiddenRepositories' rows: PRIV2-1 and its PRIV child PRIV-3 make acme/privparent both PRIV's
// and PRIV2's; PUB-2 and PRIV-5 make acme/both both PUB's and PRIV's; PUB-3, naming the repository
// PRIV maps, makes acme/privmapped PUB's too; PRIV-8 makes its parent PUB-5's acme/pubwalk PRIV's
// too; a mapping of PRIV renames acme/oldname to acme/newname, and one of PUB acme/movedold to
// acme/movednew, which PRIV-7 names, so each pair of names answers alike. acme/none, which nothing
// names, has no owner and is not in the answer. With a filter, only the projects it passes answer:
// PUB alone owns acme/both, acme/privmapped and acme/pubwalk among them, and no PRIV issue is walked.
func TestProjectRepositoriesSQLAnswersEveryProjectThatOwnsARepository(t *testing.T) {
	database := seedRepositoryIssues(t)
	seedHiddenRepositories(t, database)
	want := map[string][]string{
		"acme/pub":            {"PUB"},
		"acme/priv":           {"PRIV"},
		"acme/privdone":       {"PRIV"},
		"acme/privparent":     {"PRIV", "PRIV2"},
		"acme/privdefault":    {"PRIV"},
		"acme/both":           {"PRIV", "PUB"},
		"acme/oldname":        {"PRIV"},
		"acme/newname":        {"PRIV"},
		"ACME/NewName":        {"PRIV"},
		"acme/privmapped":     {"PRIV", "PUB"},
		"acme/privonlymapped": {"PRIV"},
		"acme/pubwalk":        {"PRIV", "PUB"},
		"acme/movedold":       {"PRIV", "PUB"},
		"acme/movednew":       {"PRIV", "PUB"},
	}
	asked := slices.Concat(slices.Sorted(maps.Keys(want)), []string{"acme/none"})
	if got := projectOwners(t, database, "true", asked); !maps.EqualFunc(got, want, slices.Equal) {
		t.Errorf("ProjectRepositoriesSQL(true) over %q =\n\t%v\nwant\n\t%v", asked, got, want)
	}
	public := map[string][]string{
		"acme/pub": {"PUB"}, "acme/both": {"PUB"}, "acme/privmapped": {"PUB"}, "acme/pubwalk": {"PUB"},
		"acme/movedold": {"PUB"}, "acme/movednew": {"PUB"},
	}
	if got := projectOwners(t, database, "project_key = 'PUB'", asked); !maps.EqualFunc(got, public, slices.Equal) {
		t.Errorf("ProjectRepositoriesSQL(project_key = 'PUB') over %q =\n\t%v\nwant\n\t%v", asked, got, public)
	}
}

// projectOwners runs ProjectRepositoriesSQL(projects) as its comment embeds it: the projects that
// own each of repositories, keyed by the name as asked, sorted.
func projectOwners(t *testing.T, database *store.Store, projects string, repositories []string) map[string][]string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select distinct asked.repo, owned.project
		from unnest($1::text[]) as asked(repo)
		join (`+ProjectRepositoriesSQL(projects)+`) owned on owned.repo = lower(`+CanonicalRepoSQL("asked.repo")+`)
		order by asked.repo, owned.project`, repositories)
	if err != nil {
		t.Fatalf("ProjectRepositoriesSQL(%s): %v", projects, err)
	}
	defer rows.Close()
	owners := map[string][]string{}
	for rows.Next() {
		var repository, project string
		if err := rows.Scan(&repository, &project); err != nil {
			t.Fatalf("ProjectRepositoriesSQL(%s): %v", projects, err)
		}
		owners[repository] = append(owners[repository], project)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("ProjectRepositoriesSQL(%s): %v", projects, err)
	}
	return owners
}

// TestRepositoryClauseHidesARepositoryAProjectTheViewerCannotSeeHolds: a repository an issue of a
// project alice does not see resolves to, or such a project maps, is hidden from her, whatever an
// issue she sees names. Each row is one way seedHiddenRepositories' rows, or seedRepositoryIssues',
// showed a repository to alice before; carol, who sees every project, reads every one of them.
func TestRepositoryClauseHidesARepositoryAProjectTheViewerCannotSeeHolds(t *testing.T) {
	database := seedRepositoryIssues(t)
	seedHiddenRepositories(t, database)
	alice := model.Actor{Viewer: &model.Viewer{Person: "alice", Open: true, Sees: []string{"PUB"}, AnyPrivate: true}}
	carol := model.Actor{Viewer: &model.Viewer{Person: "carol", Open: true, Sees: []string{"PRIV", "PRIV2", "PUB"}, AnyPrivate: true}}
	for _, row := range []struct{ name, repository string }{
		{"a repository PRIV maps, which alice names on a PUB issue", "acme/privmapped"},
		{"the same, spelled in another case", "ACME/PrivMapped"},
		{"a repository PRIV-5 names, which PUB-2 names too", "acme/both"},
		{"a repository PRIV maps and no issue resolves to", "acme/privonlymapped"},
		{"PRIV's acme/oldname under the new name its mapping records", "acme/newname"},
		{"acme/movednew, which PRIV-7 names, under the old name a mapping records", "acme/movedold"},
		{"a repository PUB-5 names and its PRIV child resolves to", "acme/pubwalk"},
	} {
		if repositoryPasses(t, database, alice, row.repository) {
			t.Errorf("%s: RepositoryClause(%s) for alice = true, want false", row.name, row.repository)
		}
		if !repositoryPasses(t, database, carol, row.repository) {
			t.Errorf("%s: RepositoryClause(%s) for carol = false, want true: she sees every project", row.name, row.repository)
		}
	}
	for _, repository := range []string{"acme/pub", "acme/none"} {
		if !repositoryPasses(t, database, alice, repository) {
			t.Errorf("RepositoryClause(%s) for alice = false, want true: no project she does not see holds it", repository)
		}
	}
}

// TestRepositoryClauseCountsOnlyPrivateOwnersTheViewerCannotSee: a repository a project the
// viewer sees owns is hidden from that viewer when a private project the viewer does not see owns
// it too, and passes when the only other owners are public projects. To seedRepositoryIssues' and
// seedHiddenRepositories' rows it adds PRIV2-2, which names acme/privdefault, PRIV's default. For
// bob (a restricted member seeing PRIV), carol's token listing PRIV and an open viewer seeing PRIV
// and PUB alike: acme/privparent, which PRIV-3 resolves to and private PRIV2's PRIV2-1 names, and
// acme/privdefault, which PRIV maps and PRIV2-2 names, are hidden; acme/both and acme/privmapped,
// which PRIV shares with public PUB, and acme/pubwalk, which PUB-5 names and its PRIV child resolves
// to, pass. carol, who sees PRIV2 as well, reads every one of them.
func TestRepositoryClauseCountsOnlyPrivateOwnersTheViewerCannotSee(t *testing.T) {
	database := seedRepositoryIssues(t)
	seedHiddenRepositories(t, database)
	if _, err := database.Pool.Exec(context.Background(), `
		insert into issues (key, project_key, number, title, created_by, rank, parent_key, repo, status) values
			('PRIV2-2', 'PRIV2', 2, 'Names acme/privdefault', '{"kind":"user","id":"alice"}', 'b', null, 'acme/privdefault', 'triage')
	`); err != nil {
		t.Fatalf("seed PRIV2-2: %v", err)
	}
	hidden := []string{"acme/privparent", "acme/privdefault"}
	shown := []string{"acme/both", "acme/privmapped", "acme/pubwalk"}
	for _, viewer := range []struct {
		name string
		who  model.Actor
	}{
		{"bob", model.Actor{Viewer: &model.Viewer{Person: "bob", Restricted: true, Sees: []string{"PRIV"}, AnyPrivate: true}}},
		{"carol's token listing PRIV", model.Actor{Viewer: &model.Viewer{Person: "carol", Sees: []string{"PRIV"}, AnyPrivate: true, Lists: []string{"PRIV"}}}},
		{"an open viewer seeing PRIV and PUB", model.Actor{Viewer: &model.Viewer{Person: "dave", Open: true, Sees: []string{"PRIV", "PUB"}, AnyPrivate: true}}},
	} {
		for _, repository := range hidden {
			if repositoryPasses(t, database, viewer.who, repository) {
				t.Errorf("RepositoryClause(%s) for %s = true, want false: private PRIV2, which %s does not see, owns it", repository, viewer.name, viewer.name)
			}
		}
		for _, repository := range shown {
			if !repositoryPasses(t, database, viewer.who, repository) {
				t.Errorf("RepositoryClause(%s) for %s = false, want true: PRIV owns it and no private project %s does not see does", repository, viewer.name, viewer.name)
			}
		}
	}
	carol := model.Actor{Viewer: &model.Viewer{Person: "carol", Open: true, Sees: []string{"PRIV", "PRIV2", "PUB"}, AnyPrivate: true}}
	for _, repository := range slices.Concat(hidden, shown) {
		if !repositoryPasses(t, database, carol, repository) {
			t.Errorf("RepositoryClause(%s) for carol = false, want true: she sees every project", repository)
		}
	}
}

// TestRepositoryClauseWalksNoIssueWhereNoProjectIsPrivate (R9): for an open viewer of a deployment
// with no private project, the clause passes every repository and reads no issue: no plan node that
// scans issues runs. The same viewer, were a project private, walks the issues, which shows the
// count can see the walk.
func TestRepositoryClauseWalksNoIssueWhereNoProjectIsPrivate(t *testing.T) {
	database := seedRepositoryIssues(t)
	everyProject := []string{"PRIV", "PRIV2", "PUB"}
	public := model.Actor{Viewer: &model.Viewer{Person: "alice", Open: true, Sees: everyProject}}
	for _, repository := range slices.Concat(clauseRepositories, []string{"acme/unknown"}) {
		if !repositoryPasses(t, database, public, repository) {
			t.Errorf("RepositoryClause(%s) for an open viewer with no private project = false, want true", repository)
		}
	}
	private := model.Actor{Viewer: &model.Viewer{Person: "alice", Open: true, Sees: everyProject, AnyPrivate: true}}
	if loops := issueScanLoops(t, database, private, "acme/both"); loops == 0 {
		t.Fatalf("an open viewer with a private project walks no issue for acme/both: the count cannot see a walk")
	}
	if loops := issueScanLoops(t, database, public, "acme/both"); loops != 0 {
		t.Errorf("an open viewer with no private project scans issues in %d loops for acme/both, want none", loops)
	}
}

// issueScanLoops explains RepositoryClause for who over repository and answers how many loops the
// plan's nodes that scan issues ran in all; a plan with no such node ran none.
func issueScanLoops(t *testing.T, database *store.Store, who model.Actor, repository string) int {
	t.Helper()
	var plan []map[string]any
	if err := database.Pool.QueryRow(context.Background(), `explain (analyze, costs off, timing off, format json) select `+RepositoryClause("$2", "$1"),
		VisibilityArg(who), repository).Scan(&plan); err != nil {
		t.Fatalf("explain RepositoryClause(%s): %v", repository, err)
	}
	total := 0
	var walk func(node map[string]any)
	walk = func(node map[string]any) {
		if node["Relation Name"] == "issues" {
			loops, _ := node["Actual Loops"].(float64)
			total += int(loops)
		}
		children, _ := node["Plans"].([]any)
		for _, child := range children {
			if child, ok := child.(map[string]any); ok {
				walk(child)
			}
		}
	}
	for _, root := range plan {
		if node, ok := root["Plan"].(map[string]any); ok {
			walk(node)
		}
	}
	return total
}
