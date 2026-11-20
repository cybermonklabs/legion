package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// TestTheGitHubProxyHidesARepositoryAHiddenProjectHolds: a repository a private project holds is
// hidden from alice, who does not see that project, however she names it, and GitHub is asked
// nothing about it. Over seedProxyGate's rows PRIV maps acme/privmapped and acme/privonlymapped,
// and alice writes a PUB issue naming acme/privmapped through the API, as any full member may: an
// issue's own repository is any owner/name. She then reads, through a pull request path, each
// hidden repository: acme/privmapped, which her own issue names; acme/privonlymapped, which no
// issue resolves to; and acme/newname, PRIV's acme/oldname under the new name its mapping records.
// Each answers, in status and bytes, what acme/uninstalled answers, and GitHub sees no request for
// it at all: no installation lookup, no token mint, no read. carol, who sees PRIV, reads each as
// GitHub's answer, so the hidden answer is the gate's.
func TestTheGitHubProxyHidesARepositoryAHiddenProjectHolds(t *testing.T) {
	database := seedProxyGate(t)
	if _, err := database.Pool.Exec(context.Background(), `
		insert into repo_projects (repo, project, is_default, renamed_to, created_by) values
			('acme/privmapped', 'PRIV', false, null, '{"kind":"user","id":"olga@example.com"}'),
			('acme/privonlymapped', 'PRIV', false, null, '{"kind":"user","id":"olga@example.com"}');
		update projects set next_number = 3 where key = 'PUB';
	`); err != nil {
		t.Fatalf("map PRIV's repositories: %v", err)
	}
	hidden := []string{"acme/privmapped", "acme/privonlymapped", "acme/newname"}
	fake := &gateGitHub{installed: slices.Concat(proxiedRepositories, hidden)}
	handler := gateRouter(t, database, fake)

	create := httptest.NewRequest(http.MethodPost, "http://dispatch.test/api/v1/issues",
		strings.NewReader(`{"project":"PUB","title":"Work in acme/privmapped","repo":"acme/privmapped"}`))
	create.Header.Set("X-Dispatch-User", "alice@example.com")
	create.Header.Set("Content-Type", "application/json")
	created := httptest.NewRecorder()
	handler.ServeHTTP(created, create)
	if created.Code != http.StatusCreated {
		t.Fatalf("alice creates a PUB issue naming acme/privmapped: %d %s, want 201", created.Code, created.Body.String())
	}

	uninstalled := proxyAnswer(t, handler, "alice@example.com", uninstalledRepository, "pulls/1")
	if !strings.HasPrefix(uninstalled, `503 "" `) || !strings.Contains(uninstalled, "is not installed on <repo>") {
		t.Fatalf("alice's read of %s, which the App is not installed on, answered %s: the comparison proves nothing", uninstalledRepository, uninstalled)
	}
	for _, repository := range hidden {
		before := len(fake.requests())
		got := proxyAnswer(t, handler, "alice@example.com", repository, "pulls/1")
		if asked := fake.requests()[before:]; got != uninstalled || len(asked) != 0 {
			t.Errorf("alice reads %s, which PRIV holds, as\n\t%s\nGitHub asked %q\nwant what %s answers, with GitHub asked nothing:\n\t%s",
				repository, got, asked, uninstalledRepository, uninstalled)
		}
		if seen := proxyAnswer(t, handler, "carol@example.com", repository, "pulls/1"); seen != `200 "" {"repository":"<repo>"}` {
			t.Errorf("carol, who sees PRIV, reads %s as %s, want GitHub's answer", repository, seen)
		}
	}
}

// TestTheGitHubProxyShowsEveryRepositoryWhereNoProjectIsPrivate (R9): with every project public,
// each full member reads every repository as GitHub answers it, as before any project could be
// private: alice and carol read each repository of seedProxyGate's rows and the ones PRIV maps,
// and each read reaches GitHub.
func TestTheGitHubProxyShowsEveryRepositoryWhereNoProjectIsPrivate(t *testing.T) {
	database := seedProxyGate(t)
	if _, err := database.Pool.Exec(context.Background(), `
		insert into repo_projects (repo, project, is_default, renamed_to, created_by) values
			('acme/privmapped', 'PRIV', false, null, '{"kind":"user","id":"olga@example.com"}'),
			('acme/privonlymapped', 'PRIV', false, null, '{"kind":"user","id":"olga@example.com"}');
		update projects set visibility = 'public';
	`); err != nil {
		t.Fatalf("map PRIV's repositories and make every project public: %v", err)
	}
	repositories := slices.Concat(proxiedRepositories, []string{"acme/privmapped", "acme/privonlymapped"})
	fake := &gateGitHub{installed: repositories}
	handler := gateRouter(t, database, fake)
	for _, login := range []string{"alice@example.com", "carol@example.com"} {
		for _, repository := range repositories {
			before := len(fake.repositoryReads())
			if got := proxyAnswer(t, handler, login, repository, "pulls/1"); got != `200 "" {"repository":"<repo>"}` || len(fake.repositoryReads()) == before {
				t.Errorf("with every project public, %s reads %s as %s, want GitHub's answer, read from GitHub", login, repository, got)
			}
		}
	}
}
