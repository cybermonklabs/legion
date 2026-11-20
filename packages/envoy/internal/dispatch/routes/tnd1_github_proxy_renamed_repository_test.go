package routes

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// renamedGateRepositories are the repositories tnd1's rows add to seedProxyGate's: PRIV-7 names
// acme/privrenamed, which GitHub renames to acme/renamednow; PRIV-8, done, names acme/privmoved,
// which GitHub moves to other/privmoved; PRIV-9 names acme/privlate, renamed to acme/latenow before
// Dispatch learned its id. Each pair is one repository, with one GitHub id.
var renamedGateIDs = map[string]int64{
	"acme/privrenamed": 7007, "acme/renamednow": 7007,
	"acme/privmoved": 7008, "other/privmoved": 7008,
	"acme/privlate": 7011, "acme/latenow": 7011,
}

// seedRenamedGate is seedProxyGate's rows with PRIV-7, PRIV-8 and PRIV-9.
func seedRenamedGate(t *testing.T) *store.Store {
	t.Helper()
	database := seedProxyGate(t)
	if _, err := database.Pool.Exec(context.Background(), `
		insert into issues (key, project_key, number, title, created_by, rank, parent_key, repo, status) values
			('PRIV-7', 'PRIV', 7, 'Names acme/privrenamed', '{"kind":"user","id":"olga@example.com"}', 'g', null, 'acme/privrenamed', 'triage'),
			('PRIV-8', 'PRIV', 8, 'Names acme/privmoved', '{"kind":"user","id":"olga@example.com"}', 'h', null, 'acme/privmoved', 'done'),
			('PRIV-9', 'PRIV', 9, 'Names acme/privlate', '{"kind":"user","id":"olga@example.com"}', 'i', null, 'acme/privlate', 'triage');
	`); err != nil {
		t.Fatalf("seed PRIV-7, PRIV-8 and PRIV-9: %v", err)
	}
	return database
}

// TestTheGitHubProxyHidesAPrivateRepositoryUnderTheNameOrOwnerGitHubMovedItTo: a private
// project's repository is hidden from an open viewer who does not see the project under any name
// GitHub gives it, since GitHub keeps its id. alice first reads acme/none, which no issue names: the
// proxy learns the id of each repository the private projects' issues resolve to, PRIV-7's
// acme/privrenamed and PRIV-8's acme/privmoved among them. GitHub then renames acme/privrenamed to
// acme/renamednow and moves acme/privmoved to other/privmoved, and the App reads neither old name
// any more. alice reads each new name, through a pull request path and a check path, as what
// acme/uninstalled answers, with GitHub asked nothing of its pull requests or checks; so too
// acme/latenow, PRIV-9's acme/privlate renamed before its id was learned, which GitHub still
// answers under the old name. carol, who sees PRIV, reads all three as GitHub answers them, so the
// hidden answer is the gate's.
//
// Control (the name rule): refs.RepositoryClause without its id arm (`learned.github_id = <id>`)
// shows alice all three, and the test fails naming each.
func TestTheGitHubProxyHidesAPrivateRepositoryUnderTheNameOrOwnerGitHubMovedItTo(t *testing.T) {
	database := seedRenamedGate(t)
	before := slices.Concat(proxiedRepositories, []string{"acme/privrenamed", "acme/privmoved", "acme/privlate", "acme/latenow"})
	fake := &gateGitHub{installed: before, ids: renamedGateIDs, renamed: map[string]string{"acme/privlate": "acme/latenow"}}
	handler := gateRouter(t, database, fake)
	if got := proxyAnswer(t, handler, "alice@example.com", "acme/none", "pulls/1"); got != `200 "" {"repository":"<repo>"}` {
		t.Fatalf("alice reads acme/none, which no issue names, as %s, want GitHub's answer", got)
	}
	var learned int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from repository_ids where (repo, github_id) in (('acme/privrenamed', 7007), ('acme/privmoved', 7008))
	`).Scan(&learned); err != nil || learned != 2 {
		t.Fatalf("after alice's read the proxy has learned %d of PRIV's two ids (%v), want both", learned, err)
	}
	fake.install(slices.Concat(proxiedRepositories, []string{"acme/renamednow", "other/privmoved", "acme/privlate", "acme/latenow"})...)

	moved := []string{"acme/renamednow", "other/privmoved", "acme/latenow"}
	for _, path := range []struct{ name, suffix string }{
		{"pull request", "pulls/1"},
		{"check", "commits/0123abcd/check-runs"},
	} {
		uninstalled := proxyAnswer(t, handler, "alice@example.com", uninstalledRepository, path.suffix)
		if !strings.HasPrefix(uninstalled, `503 "" `) || !strings.Contains(uninstalled, "is not installed on <repo>") {
			t.Fatalf("alice's %s read of %s answered %s: the comparison proves nothing", path.name, uninstalledRepository, uninstalled)
		}
		for _, repository := range moved {
			reads := len(fake.repositoryReads())
			got := proxyAnswer(t, handler, "alice@example.com", repository, path.suffix)
			if read := fake.repositoryReads()[reads:]; got != uninstalled || len(read) != 0 {
				t.Errorf("alice reads %s, PRIV's repository under the name GitHub gives it now, through its %s path as\n\t%s (GitHub read %q)\nwant what %s answers, with nothing of it read:\n\t%s",
					repository, path.name, got, read, uninstalledRepository, uninstalled)
			}
			if seen := proxyAnswer(t, handler, "carol@example.com", repository, path.suffix); seen != `200 "" {"repository":"<repo>"}` {
				t.Errorf("carol, who sees PRIV, reads %s through its %s path as %s, want GitHub's answer", repository, path.name, seen)
			}
		}
	}
}

// TestTheGitHubProxyHidesARepositoryWhoseIDItCannotTellApart: while a project is private, an open
// viewer whom no issue gives a repository reads it only when the proxy can tell it from every
// private project's repository by GitHub's id, and otherwise reads it as one the App is not
// installed on. alice reads acme/flaky, whose id GitHub fails to answer, as acme/uninstalled
// answers; carol, who sees every project, reads it, since no project hides anything from her. Then
// PRIV-10 names acme/privgone, which the App reads nothing under, so its id cannot be known: it
// could be any repository under a new name, and alice reads acme/none, which no issue names, as not
// installed too, while acme/pub, which PUB-1 gives her, still reads as GitHub answers it. This is
// the cost of an App that cannot read a private project's repository: every repository no issue
// alice sees names is hidden from her until that issue names one the App reads.
//
// Controls: let a failed id read pass the repository (routes' repositoryHidden answering false), and
// alice reads acme/flaky; let a name with no known id hide nothing (refs.RepositoryClause without
// `learned.github_id is null`), and alice reads acme/none.
func TestTheGitHubProxyHidesARepositoryWhoseIDItCannotTellApart(t *testing.T) {
	database := seedRenamedGate(t)
	installed := slices.Concat(proxiedRepositories, []string{"acme/privrenamed", "acme/privmoved", "acme/privlate", "acme/flaky"})
	fake := &gateGitHub{installed: installed, ids: renamedGateIDs, noID: []string{"acme/flaky"}}
	handler := gateRouter(t, database, fake)
	uninstalled := proxyAnswer(t, handler, "alice@example.com", uninstalledRepository, "pulls/1")
	if !strings.HasPrefix(uninstalled, `503 "" `) || !strings.Contains(uninstalled, "is not installed on <repo>") {
		t.Fatalf("alice's read of %s answered %s: the comparison proves nothing", uninstalledRepository, uninstalled)
	}
	if got := proxyAnswer(t, handler, "alice@example.com", "acme/flaky", "pulls/1"); got != uninstalled {
		t.Errorf("alice reads acme/flaky, whose GitHub id cannot be read, as\n\t%s\nwant what %s answers:\n\t%s", got, uninstalledRepository, uninstalled)
	}
	if got := proxyAnswer(t, handler, "alice@example.com", "acme/none", "pulls/1"); got != `200 "" {"repository":"<repo>"}` {
		t.Fatalf("alice reads acme/none, which no issue names, as %s, want GitHub's answer: the row below proves nothing", got)
	}
	if _, err := database.Pool.Exec(context.Background(), `
		insert into issues (key, project_key, number, title, created_by, rank, parent_key, repo, status) values
			('PRIV-10', 'PRIV', 10, 'Names acme/privgone', '{"kind":"user","id":"olga@example.com"}', 'j', null, 'acme/privgone', 'triage');
	`); err != nil {
		t.Fatalf("seed PRIV-10: %v", err)
	}
	if got := proxyAnswer(t, handler, "alice@example.com", "acme/none", "pulls/1"); got != uninstalled {
		t.Errorf("alice reads acme/none while PRIV's acme/privgone has no known id as\n\t%s\nwant what %s answers:\n\t%s", got, uninstalledRepository, uninstalled)
	}
	if got := proxyAnswer(t, handler, "alice@example.com", "acme/pub", "pulls/1"); got != `200 "" {"repository":"<repo>"}` {
		t.Errorf("alice reads acme/pub, which PUB-1 gives her, as %s, want GitHub's answer", got)
	}
	var recorded *int64
	if err := database.Pool.QueryRow(context.Background(), `select github_id from repository_ids where repo = 'acme/privgone'`).Scan(&recorded); err != nil || recorded != nil {
		t.Errorf("acme/privgone's record = %v (%v), want a row with no id: GitHub answered that the App reads nothing there", recorded, err)
	}
	for _, repository := range []string{"acme/flaky", "acme/none"} {
		if got := proxyAnswer(t, handler, "carol@example.com", repository, "pulls/1"); got != `200 "" {"repository":"<repo>"}` {
			t.Errorf("carol, who sees every project, reads %s as %s, want GitHub's answer", repository, got)
		}
	}
}
