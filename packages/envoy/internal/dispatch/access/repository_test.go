package access

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestTheRepositoryClauseOverTheSeededCallers: a repository a mapping names, by its mapped name or
// by the name it was renamed to and in any case, passes for a viewer that sees the mapped project
// and for no other; a repository no mapping names passes for an open viewer alone; the server's own
// viewer passes every one.
func TestTheRepositoryClauseOverTheSeededCallers(t *testing.T) {
	ctx := context.Background()
	database := seedAccess(t)
	if _, err := database.Pool.Exec(ctx, `
		insert into repo_projects (repo, project, created_by, renamed_to) values
			('acme/pub', 'PUB', '{"kind":"user","id":"olga@example.com"}', null),
			('acme/priv-old', 'PRIV', '{"kind":"user","id":"olga@example.com"}', 'Acme/Priv-New');
	`); err != nil {
		t.Fatalf("seed the repository mappings: %v", err)
	}
	callers := map[string]model.Actor{
		"alice": cookie(alice), "bob": cookie(bob), "carol's token naming PRIV": personalToken(carol, "PRIV"),
	}
	for _, probe := range []struct {
		repository string
		want       map[string]bool
	}{
		{"acme/pub", map[string]bool{"alice": true, "bob": false, "carol's token naming PRIV": false}},
		{"Acme/Priv-Old", map[string]bool{"alice": false, "bob": true, "carol's token naming PRIV": true}},
		{"acme/priv-new", map[string]bool{"alice": false, "bob": true, "carol's token naming PRIV": true}},
		{"acme/unmapped", map[string]bool{"alice": true, "bob": false, "carol's token naming PRIV": false}},
	} {
		for name, actor := range callers {
			viewer, err := Resolve(ctx, database.Pool, actor)
			if err != nil {
				t.Fatalf("%s: Resolve: %v", name, err)
			}
			if got := repositoryPasses(t, database.Pool, viewerArg(t, viewer), probe.repository); got != probe.want[name] {
				t.Errorf("RepositoryClause %s for %s = %t, want %t", probe.repository, name, got, probe.want[name])
			}
		}
		if !repositoryPasses(t, database.Pool, viewerArg(t, *model.ServerViewer), probe.repository) {
			t.Errorf("RepositoryClause %s for the server's own viewer = false, want true", probe.repository)
		}
	}
	if fragment := RepositoryClause("$2", "$1"); !strings.HasPrefix(fragment, Marker+"RepositoryClause */") {
		t.Errorf("RepositoryClause = %q, want it to start with the marker and its name", fragment[:60])
	}
}

func repositoryPasses(t *testing.T, q Querier, viewer []byte, repository string) bool {
	t.Helper()
	var passes bool
	if err := q.QueryRow(context.Background(), `select `+RepositoryClause("$2", "$1"), viewer, repository).Scan(&passes); err != nil {
		t.Fatalf("RepositoryClause %s: %v", repository, err)
	}
	return passes
}
