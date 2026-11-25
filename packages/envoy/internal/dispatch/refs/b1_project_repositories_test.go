package refs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestProjectRepositoriesAnswersTheRepositoriesItsIssuesResolveTo: ProjectRepositories is each
// repository an issue of the project resolves to, open or done, through its own repository, its
// parent's or its project's default, lower-cased, once each and sorted, and nothing a mapping or a
// recorded rename gives the project. Over seedRepositoryIssues' and seedHiddenRepositories' rows,
// and PRIV-20, which stores Acme/Upper (a name only a direct write stores in capitals: the API
// lower-cases, api/issue_repo.go), PRIV answers the nine its issues resolve to (acme/pubwalk through
// PRIV-8's PUB parent, acme/upper for PRIV-20, and not acme/privmapped or acme/privonlymapped,
// which it only maps, nor acme/newname, its mapping's rename); PUB and PRIV2 answer theirs; a
// project with no issue answers [] and an unknown project access.ErrProjectNotFound. Once PRIV
// holds a looped chain (PRIV-9 and PRIV-10, each the other's parent), it answers an
// *UnresolvedChainError naming PRIV-9, the first by number.
//
// Controls: drop the unresolved chain's refusal from ProjectRepositories, and PRIV answers its
// list with the loop in it; drop lower() from ProjectRepositoriesSQL, and PRIV answers Acme/Upper
// in place of acme/upper. The test fails naming PRIV either way.
func TestProjectRepositoriesAnswersTheRepositoriesItsIssuesResolveTo(t *testing.T) {
	database := seedRepositoryIssues(t)
	seedHiddenRepositories(t, database)
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('EMPTY', 'No issues', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank, parent_key, repo, status) values
			('PRIV-20', 'PRIV', 20, 'Names Acme/Upper', '{"kind":"user","id":"alice"}', 'u', null, 'Acme/Upper', 'triage');
	`); err != nil {
		t.Fatalf("seed EMPTY and PRIV-20: %v", err)
	}
	for project, want := range map[string][]string{
		"PRIV":  {"acme/both", "acme/movednew", "acme/oldname", "acme/priv", "acme/privdefault", "acme/privdone", "acme/privparent", "acme/pubwalk", "acme/upper"},
		"PRIV2": {"acme/privparent"},
		"PUB":   {"acme/both", "acme/privmapped", "acme/pub", "acme/pubwalk"},
		"EMPTY": {},
	} {
		got, err := ProjectRepositories(ctx, database.Pool, project)
		if err != nil || got == nil || !slices.Equal(got, want) {
			t.Errorf("ProjectRepositories(%s) = %q, %v, want %q", project, got, err, want)
		}
	}
	if _, err := ProjectRepositories(ctx, database.Pool, "NOPE"); !errors.Is(err, access.ErrProjectNotFound) {
		t.Errorf("ProjectRepositories(NOPE) = %v, want access.ErrProjectNotFound", err)
	}
	seedLoopedChain(t, database, "PRIV", 9)
	got, err := ProjectRepositories(ctx, database.Pool, "PRIV")
	var unresolved *UnresolvedChainError
	if !errors.As(err, &unresolved) || *unresolved != (UnresolvedChainError{Project: "PRIV", Issue: "PRIV-9"}) || got != nil {
		t.Errorf("ProjectRepositories(PRIV) with a looped chain = %q, %v, want an *UnresolvedChainError naming PRIV-9", got, err)
	}
}

// seedLoopedChain writes project's issues number and number+1, naming no repository, each the
// other's parent: a chain the walk cannot finish, which only a direct database write builds.
func seedLoopedChain(t *testing.T, database *store.Store, project string, number int) {
	t.Helper()
	first, second := fmt.Sprintf("%s-%d", project, number), fmt.Sprintf("%s-%d", project, number+1)
	if _, err := database.Pool.Exec(context.Background(), `
		insert into issues (key, project_key, number, title, created_by, rank, parent_key, repo, status) values
			($1, $3, $4, 'Looped', '{"kind":"user","id":"alice"}', 'y', null, null, 'triage'),
			($2, $3, $4 + 1, 'Looped', '{"kind":"user","id":"alice"}', 'z', $1, null, 'triage')
	`, first, second, project, number); err != nil {
		t.Fatalf("seed %s and %s: %v", first, second, err)
	}
	if _, err := database.Pool.Exec(context.Background(), `update issues set parent_key = $2 where key = $1`, first, second); err != nil {
		t.Fatalf("loop %s under %s: %v", first, second, err)
	}
}

// TestAPublicProjectsPeopleReadARepositoryAPrivateProjectWorksInToo (BuildAG11's probe): where
// issues of a private project and of a public project work in one repository, the public project's
// people read it. OTHER is public and OTHER-2 names acme/two; PRIV is private, and its PRIV-3, a
// child of OTHER-2 naming none, resolves to acme/two too. alice, open and seeing OTHER, and carl, a
// restricted member seeing only OTHER, each read acme/two; ProjectRepositories lists acme/two for
// OTHER and for PRIV alike, so the edge and the proxy agree.
//
// Control: the rule before this change (a private owner the viewer does not see hides the
// repository whatever the viewer sees, `not bool_or(not seen and owner.visibility = 'private')`),
// run over the same rows, answers false for both; the test fails if it does not, since the rows
// would then prove nothing.
func TestAPublicProjectsPeopleReadARepositoryAPrivateProjectWorksInToo(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee, visibility) values ('OTHER', 'Other', 'olga', 'public'), ('PRIV', 'Private', 'olga', 'private');
		insert into issues (key, project_key, number, title, created_by, rank, parent_key, repo, status) values
			('OTHER-2', 'OTHER', 2, 'Names acme/two', '{"kind":"user","id":"olga"}', 'a', null, 'acme/two', 'triage'),
			('PRIV-3', 'PRIV', 3, 'Under OTHER-2', '{"kind":"user","id":"olga"}', 'a', 'OTHER-2', null, 'triage');
	`); err != nil {
		t.Fatalf("seed the probe: %v", err)
	}
	for _, project := range []string{"OTHER", "PRIV"} {
		if got, err := ProjectRepositories(ctx, database.Pool, project); err != nil || !slices.Equal(got, []string{"acme/two"}) {
			t.Errorf("ProjectRepositories(%s) = %q, %v, want [acme/two]", project, got, err)
		}
	}
	oldRule := `coalesce((select not coalesce(bool_or(not ` + access.ProjectClause("owned.project", "$1") + ` and owner.visibility = 'private'), false)
			and (` + access.ViewerFlag("$1", "open") + ` or coalesce(bool_or(` + access.ProjectClause("owned.project", "$1") + `), false))
		from (` + ProjectRepositoriesSQL("true") + `) owned join projects owner on owner.key = owned.project
		where owned.repo = lower($2::text)), false)`
	for _, viewer := range []struct {
		name string
		who  model.Actor
	}{
		{"alice, open and seeing OTHER", model.Actor{Viewer: &model.Viewer{Person: "alice", Open: true, Sees: []string{"OTHER"}, AnyPrivate: true}}},
		{"carl, restricted and seeing only OTHER", model.Actor{Viewer: &model.Viewer{Person: "carl", Restricted: true, Sees: []string{"OTHER"}, AnyPrivate: true}}},
	} {
		if !repositoryPasses(t, database, viewer.who, "acme/two") {
			t.Errorf("RepositoryClause(acme/two) for %s = false, want true: OTHER-2, which the viewer sees, names it", viewer.name)
		}
		var old bool
		if err := database.Pool.QueryRow(ctx, `select `+oldRule, VisibilityArg(viewer.who), "acme/two").Scan(&old); err != nil {
			t.Fatalf("the old rule for %s: %v", viewer.name, err)
		}
		if old {
			t.Errorf("the old rule shows acme/two to %s: the row cannot tell the two rules apart", viewer.name)
		}
	}
}

// seedTooDeepChain writes project's issues number to number+IssueDepthCap+1, naming no repository,
// each the parent of the next: the last sits IssueDepthCap+1 links below its root, a chain the
// walk cannot finish, which only a direct database write builds (the API refuses it,
// api/issue_parent.go's refuseParentTooDeep). It answers that last issue's key.
func seedTooDeepChain(t *testing.T, database *store.Store, project string, number int) string {
	t.Helper()
	last := number + IssueDepthCap + 1
	if _, err := database.Pool.Exec(context.Background(), `
		insert into issues (key, project_key, number, title, created_by, rank, parent_key, repo, status)
		select $1 || '-' || n, $1, n, 'Deep ' || n, '{"kind":"user","id":"alice"}', lpad(n::text, 8, '0'),
			case when n = $2 then null else $1 || '-' || (n - 1) end, null, 'triage'
		from generate_series($2::int, $3::int) n
		order by n
	`, project, number, last); err != nil {
		t.Fatalf("seed a %d-link chain in %s: %v", IssueDepthCap+1, project, err)
	}
	return fmt.Sprintf("%s-%d", project, last)
}

// TestAnUnresolvedChainResolvesToNoRepository (plan v89): an issue whose chain the walk cannot
// finish resolves to no repository at the gate, never to every repository, so it hides nothing.
// PRIV, private, takes a looped chain (PRIV-9 and PRIV-10, each the other's parent) or a chain
// one link deeper than IssueDepthCap (PRIV-100 to PRIV-133). With either, every viewer reads each
// repository as before it: alice, open and seeing PUB, still reads acme/pub and acme/none; bob,
// who sees PRIV, gains nothing; and olive, open and seeing no project, still reads acme/pub and
// acme/none. For olive, RepositoryClause is exactly `not repositoryHeld`, the clause an outsider's
// issue write is refused by (RoundRepoWrite's RepositoryHeldClause), so that write naming acme/pub
// is not refused. ProjectRepositories alone refuses PRIV, naming the unresolved issue.
//
// Control: put `owned.repo is null or` back in repositoryHeld (c680dad75d's text), and alice and
// olive lose acme/none, olive loses acme/pub and every other repository; the test fails naming
// each.
func TestAnUnresolvedChainResolvesToNoRepository(t *testing.T) {
	viewers := []struct {
		name string
		who  model.Actor
	}{
		{"alice", model.Actor{Viewer: &model.Viewer{Person: "alice", Open: true, Sees: []string{"PUB"}, AnyPrivate: true}}},
		{"bob", model.Actor{Viewer: &model.Viewer{Person: "bob", Restricted: true, Sees: []string{"PRIV"}, AnyPrivate: true}}},
		{"olive", model.Actor{Viewer: &model.Viewer{Person: "olive", Open: true, AnyPrivate: true}}},
	}
	asked := slices.Concat(clauseRepositories, []string{"acme/unknown"})
	for _, chain := range []struct {
		name string
		seed func(*testing.T, *store.Store) string
	}{
		{"looped", func(t *testing.T, database *store.Store) string {
			seedLoopedChain(t, database, "PRIV", 9)
			return "PRIV-9"
		}},
		{"deeper than the cap", func(t *testing.T, database *store.Store) string { return seedTooDeepChain(t, database, "PRIV", 100) }},
	} {
		t.Run(chain.name, func(t *testing.T) {
			database := seedRepositoryIssues(t)
			before := map[string]map[string]bool{}
			for _, viewer := range viewers {
				before[viewer.name] = map[string]bool{}
				for _, repository := range asked {
					before[viewer.name][repository] = repositoryPasses(t, database, viewer.who, repository)
				}
			}
			for _, viewer := range []string{"alice", "olive"} {
				if !before[viewer]["acme/pub"] || !before[viewer]["acme/none"] {
					t.Fatalf("%s reads acme/pub %t and acme/none %t before the chain, want both: the row proves nothing",
						viewer, before[viewer]["acme/pub"], before[viewer]["acme/none"])
				}
			}
			unresolved := chain.seed(t, database)
			for _, viewer := range viewers {
				for _, repository := range asked {
					if got := repositoryPasses(t, database, viewer.who, repository); got != before[viewer.name][repository] {
						t.Errorf("%s reads %s while PRIV holds a %s chain = %t, want %t as before it", viewer.name, repository, chain.name, got, before[viewer.name][repository])
					}
				}
			}
			_, err := ProjectRepositories(context.Background(), database.Pool, "PRIV")
			var refused *UnresolvedChainError
			if !errors.As(err, &refused) || refused.Issue != unresolved {
				t.Errorf("ProjectRepositories(PRIV) with a %s chain = %v, want an *UnresolvedChainError naming %s", chain.name, err, unresolved)
			}
		})
	}
}
