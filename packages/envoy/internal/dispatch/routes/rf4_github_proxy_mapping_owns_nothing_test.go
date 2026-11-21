package routes

import (
	"context"
	"strings"
	"testing"
)

// TestARepositoryMappingNeitherShowsNorHidesARepository: a repository mapping gives no project a
// repository, so it shows nothing to a restricted member and hides nothing from an open viewer;
// only issues own repositories. Every project public first: PUB-3 names acme/pubonly, and bob, a
// restricted member PRIV's list names, reads it as one the App is not installed on, before and after
// a full member maps acme/pubonly to PRIV, and GitHub is asked nothing. Then with PRIV private: PRIV
// maps acme/privonlymapped, which no issue resolves to, and alice, an open viewer who does not see
// PRIV, reads it as GitHub answers it; carol reads it too.
//
// At the merge head before this change the mapping arm made PRIV an owner of each mapped
// repository, so bob read acme/pubonly once it was mapped and alice did not read
// acme/privonlymapped.
func TestARepositoryMappingNeitherShowsNorHidesARepository(t *testing.T) {
	database := seedProxyGate(t)
	if _, err := database.Pool.Exec(context.Background(), `
		update projects set visibility = 'public';
		insert into issues (key, project_key, number, title, created_by, rank, parent_key, repo, status) values
			('PUB-3', 'PUB', 3, 'Names acme/pubonly', '{"kind":"user","id":"olga@example.com"}', 'c', null, 'acme/pubonly', 'triage');
	`); err != nil {
		t.Fatalf("make every project public and name acme/pubonly on PUB-3: %v", err)
	}
	fake := &gateGitHub{installed: append([]string{"acme/pubonly", "acme/privonlymapped"}, proxiedRepositories...)}
	handler := gateRouter(t, database, fake)
	uninstalled := proxyAnswer(t, handler, "bob@example.com", uninstalledRepository, "pulls/1")
	if !strings.HasPrefix(uninstalled, `503 "" `) || !strings.Contains(uninstalled, "is not installed on <repo>") {
		t.Fatalf("bob's read of %s, which the App is not installed on, answered %s: the comparison proves nothing", uninstalledRepository, uninstalled)
	}
	read := func(state string) {
		before := len(fake.requests())
		if got := proxyAnswer(t, handler, "bob@example.com", "acme/pubonly", "pulls/1"); got != uninstalled || len(fake.requests()) != before {
			t.Errorf("%s, bob reads acme/pubonly, which only PUB-3 names, as\n\t%s\nGitHub asked %q\nwant what %s answers, with GitHub asked nothing:\n\t%s",
				state, got, fake.requests()[before:], uninstalledRepository, uninstalled)
		}
	}
	read("before any mapping")
	if _, err := database.Pool.Exec(context.Background(), `
		insert into repo_projects (repo, project, is_default, renamed_to, created_by)
		values ('acme/pubonly', 'PRIV', false, null, '{"kind":"user","id":"alice@example.com"}')
	`); err != nil {
		t.Fatalf("map acme/pubonly to PRIV: %v", err)
	}
	read("after a full member maps acme/pubonly to PRIV")

	if _, err := database.Pool.Exec(context.Background(), `
		update projects set visibility = 'private' where key in ('PRIV', 'PRIV2');
		insert into repo_projects (repo, project, is_default, renamed_to, created_by)
		values ('acme/privonlymapped', 'PRIV', false, null, '{"kind":"user","id":"olga@example.com"}')
	`); err != nil {
		t.Fatalf("make PRIV private and map acme/privonlymapped to it: %v", err)
	}
	for _, login := range []string{"alice@example.com", "carol@example.com"} {
		if got := proxyAnswer(t, handler, login, "acme/privonlymapped", "pulls/1"); got != `200 "" {"repository":"<repo>"}` {
			t.Errorf("%s reads acme/privonlymapped, which PRIV maps and no issue resolves to, as %s, want GitHub's answer", login, got)
		}
	}
}
