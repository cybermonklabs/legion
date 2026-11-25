package routes

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// TestTheGitHubProxyHidesARepositoryAHiddenProjectHolds: a repository only a private project's
// issues resolve to is hidden from alice, who does not see that project, however she spells it, and
// GitHub is asked nothing about it. Over seedProxyGate's rows she reads, through a pull request
// path, acme/priv, which PRIV-1 names, ACME/Priv, the same in another case, acme/privdefault, PRIV's
// default, and acme/oldname, which PRIV-6 names. Each answers, in status and bytes, what
// acme/uninstalled answers, and GitHub sees no request for it at all: no installation lookup, no
// token mint, no read. carol, who sees PRIV, reads each as GitHub's
// answer, so the hidden answer is the gate's.
func TestTheGitHubProxyHidesARepositoryAHiddenProjectHolds(t *testing.T) {
	database := seedProxyGate(t)
	hidden := []string{"acme/priv", "ACME/Priv", "acme/privdefault", "acme/oldname"}
	fake := &gateGitHub{installed: slices.Concat(proxiedRepositories, hidden)}
	handler := gateRouter(t, database, fake)

	uninstalled := proxyAnswer(t, handler, "alice@example.com", uninstalledRepository, "pulls/1")
	if !strings.HasPrefix(uninstalled, `503 "" `) || !strings.Contains(uninstalled, "is not installed on <repo>") {
		t.Fatalf("alice's read of %s, which the App is not installed on, answered %s: the comparison proves nothing", uninstalledRepository, uninstalled)
	}
	for _, repository := range hidden {
		before := len(fake.requests())
		got := proxyAnswer(t, handler, "alice@example.com", repository, "pulls/1")
		if asked := fake.requests()[before:]; got != uninstalled || len(asked) != 0 {
			t.Errorf("alice reads %s, which only PRIV's issues resolve to, as\n\t%s\nGitHub asked %q\nwant what %s answers, with GitHub asked nothing:\n\t%s",
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
