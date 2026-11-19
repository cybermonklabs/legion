package refs

import (
	"context"
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

// TestRepositoryClauseShowsARepositoryByTheIssuesThatResolveToIt: a repository passes for a viewer
// that sees an issue resolving to it, through the issue's own repository, its parent's or its
// project's default, whether the issue is open or done; and for an open viewer when no issue of a
// project it does not see resolves to it. alice, open and seeing PUB, reads acme/pub, acme/both,
// the unnamed acme/none, and acme/newname, which only a rename names, so no rename is read; bob, a
// restricted member seeing PRIV, reads every repository a PRIV issue resolves to and no other;
// carol, seeing every project, and the server's own viewer read all nine; the closed viewer reads
// none. Names are compared without case, and the clause is never null.
func TestRepositoryClauseShowsARepositoryByTheIssuesThatResolveToIt(t *testing.T) {
	database := seedRepositoryIssues(t)
	for _, viewer := range []struct {
		name  string
		who   model.Actor
		reads []string
	}{
		{"alice", model.Actor{Viewer: &model.Viewer{Person: "alice", Open: true, Sees: []string{"PUB"}, AnyPrivate: true}},
			[]string{"acme/pub", "acme/none", "acme/both", "acme/newname"}},
		{"bob", model.Actor{Viewer: &model.Viewer{Person: "bob", Restricted: true, Sees: []string{"PRIV"}, AnyPrivate: true}},
			[]string{"acme/priv", "acme/privdone", "acme/privparent", "acme/privdefault", "acme/both", "acme/oldname"}},
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
