package routes

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/githubtest"
)

// renamingRouter is gateRouter over a GitHub that also answers two old names with a 301, as
// GitHub answers a renamed or transferred repository's old name: acme/old's pull request moved to
// acme/new's, and acme/stale's to acme/priv's, a repository only PRIV's issues resolve to. Every
// other request is fake's.
func renamingRouter(t *testing.T, database *store.Store, fake *gateGitHub) http.Handler {
	t.Helper()
	handle := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/old/pulls/1":
			http.Redirect(w, r, "/repos/acme/new/pulls/1", http.StatusMovedPermanently)
		case "/repos/acme/stale/pulls/1":
			http.Redirect(w, r, "/repos/acme/priv/pulls/1", http.StatusMovedPermanently)
		default:
			fake.serve(w, r)
		}
	}
	_, pem := githubapptest.Key(t)
	people := store.NewPgPeopleStore(database.Pool, testSigningKey, nil)
	ctx, err := BuildAppContext(AppContextOptions{
		SigningKey:    testSigningKey,
		People:        people,
		Identity:      identity.HeaderIdentity{Header: "X-Dispatch-User", People: people},
		Store:         database,
		App:           &auth.AppConfig{ClientID: "Iv1.rename", PEM: pem},
		GitHubAPIBase: githubtest.Server(t, githubtest.Spec{Handle: handle}),
		Lifetime:      t.Context(),
	})
	if err != nil {
		t.Fatalf("build context: %v", err)
	}
	handler, background := New(ctx)
	t.Cleanup(background.Wait)
	return handler
}

// TestTheGitHubProxyFollowsARenameOnlyWhereNoProjectIsPrivate: while PRIV is private, alice, who
// sees acme/old and acme/stale (no issue resolves to either), reads each through the proxy as a
// repository the App is not installed on, in status and bytes, and GitHub's redirect is not
// followed: neither acme/new nor acme/priv is read. Once every project is public (R9) the same two
// reads follow GitHub's redirect and answer GitHub's 200 for acme/new and acme/priv, as every read
// did before a project could be private.
func TestTheGitHubProxyFollowsARenameOnlyWhereNoProjectIsPrivate(t *testing.T) {
	database := seedProxyGate(t)
	fake := &gateGitHub{installed: []string{"acme/old", "acme/stale"}}
	handler := renamingRouter(t, database, fake)
	uninstalled := proxyAnswer(t, handler, "alice@example.com", uninstalledRepository, "pulls/1")
	if !strings.HasPrefix(uninstalled, `503 "" `) || !strings.Contains(uninstalled, "is not installed on <repo>") {
		t.Fatalf("alice's read of %s, which the App is not installed on, answered %s: the comparison proves nothing", uninstalledRepository, uninstalled)
	}
	renamed := []struct{ old, now string }{{"acme/old", "acme/new"}, {"acme/stale", "acme/priv"}}
	for _, rename := range renamed {
		before := len(fake.repositoryReads())
		got := proxyAnswer(t, handler, "alice@example.com", rename.old, "pulls/1")
		if read := fake.repositoryReads()[before:]; got != uninstalled || len(read) != 0 {
			t.Errorf("while PRIV is private, alice reads %s, which GitHub redirects to %s, as\n\t%s\nwith GitHub read %q\nwant what %s answers, with nothing read:\n\t%s",
				rename.old, rename.now, got, read, uninstalledRepository, uninstalled)
		}
	}

	if _, err := database.Pool.Exec(context.Background(), `update projects set visibility = 'public'`); err != nil {
		t.Fatalf("make every project public: %v", err)
	}
	for _, rename := range renamed {
		before := len(fake.repositoryReads())
		want := fmt.Sprintf(`200 "" {"repository":%q}`, rename.now)
		got := proxyAnswer(t, handler, "alice@example.com", rename.old, "pulls/1")
		if read := fake.repositoryReads()[before:]; got != want || !slices.Contains(read, "GET /repos/"+rename.now+"/pulls/1") {
			t.Errorf("with every project public, alice reads %s, which GitHub redirects to %s, as %s with GitHub read %q; want GitHub's %s, read from %s",
				rename.old, rename.now, got, read, want, rename.now)
		}
	}
}
