package routes

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/ratelimit"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/githubtest"
)

// gateRead is the GitHub proxy's read the gate tests make: one pull request of acme/web.
const gateRead = "/api/github/rest/repos/acme/web/pulls/1"

// TestTheGitHubProxyAnswersAHiddenRepositoryAsUninstalled: for alice, a full member PRIV's list
// does not name, acme/web, which the App is installed on and repo_projects maps to the private
// PRIV, answers exactly what acme/web answers on a deployment where the App is not installed on it
// and nothing maps it: equal status, body and Retry-After, with an App key, with no App key, and
// under a rate limit already met. GitHub is never read for her. carol, whom PRIV's list names,
// reads the same pull request through the same router, so the answer alice gets is the gate's
// and not a router that reads nothing.
func TestTheGitHubProxyAnswersAHiddenRepositoryAsUninstalled(t *testing.T) {
	hiding := newGateRig(t, true)
	uninstalled := newGateRig(t, false)
	for _, test := range []struct {
		name    string
		withKey bool
		limited bool
	}{
		{"with an App key", true, false},
		{"with no App key", false, false},
		{"under a met rate limit", true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			hidden := hiding.read(t, test.withKey, test.limited, "alice@example.com")
			missing := uninstalled.read(t, test.withKey, test.limited, "alice@example.com")
			if hidden != missing {
				t.Fatalf("%s, a hidden installed repository answers\n\t%s\nand an uninstalled one\n\t%s", test.name, hidden, missing)
			}
		})
	}
	if reads := hiding.fake.repositoryReads(); len(reads) != 0 {
		t.Fatalf("GitHub was read for alice: %q", reads)
	}
	if seen := hiding.read(t, true, false, "carol@example.com"); !strings.HasPrefix(seen, "200 ") {
		t.Fatalf("carol, whom PRIV names, read acme/web as %s, want GitHub's 200", seen)
	}
}

// gateRig is one deployment the gate test reads through: a database seeded with PRIV, alice and
// carol, and a fake GitHub. A hiding rig has the App installed on acme/web and maps it to PRIV; the
// other has neither.
type gateRig struct {
	database *store.Store
	fake     *gateGitHub
	base     string
	pem      string
}

func newGateRig(t *testing.T, hiding bool) *gateRig {
	t.Helper()
	database := storetest.Open(t)
	seed := `
		insert into people (email, member_kind) values
			('alice@example.com', 'full'), ('carol@example.com', 'full'), ('olga@example.com', 'full');
		insert into projects (key, name, default_assignee, visibility) values ('PRIV', 'Private', 'olga@example.com', 'private');
		insert into project_access (project_key, principal_kind, principal, added_by) values
			('PRIV', 'person', 'carol@example.com', '{"kind":"user","id":"olga@example.com"}');`
	if hiding {
		seed += `
		insert into repo_projects (repo, project, created_by) values ('acme/web', 'PRIV', '{"kind":"user","id":"olga@example.com"}');`
	}
	if _, err := database.Pool.Exec(context.Background(), seed); err != nil {
		t.Fatalf("seed the gate rig: %v", err)
	}
	fake := &gateGitHub{installed: hiding}
	_, pem := githubapptest.Key(t)
	return &gateRig{database: database, fake: fake, base: githubtest.Server(t, githubtest.Spec{Handle: fake.serve}), pem: pem}
}

// read answers the proxy's read as login, through a router of the rig with or without the App's
// key, and under a met limit of the App's scope when limited, as "<status> <Retry-After> <body>".
func (rig *gateRig) read(t *testing.T, withKey, limited bool, login string) string {
	t.Helper()
	people := store.NewPgPeopleStore(rig.database.Pool, testSigningKey, nil)
	options := AppContextOptions{
		SigningKey: testSigningKey,
		People:     people,
		Identity:   identity.HeaderIdentity{Header: "X-Dispatch-User", People: people},
		Store:      rig.database,
		Lifetime:   t.Context(),
	}
	if withKey {
		options.App, options.GitHubAPIBase = &auth.AppConfig{ClientID: "Iv1.gate", PEM: rig.pem}, rig.base
	}
	ctx, err := BuildAppContext(options)
	if err != nil {
		t.Fatalf("build context: %v", err)
	}
	handler, background := New(ctx)
	t.Cleanup(background.Wait)
	if limited {
		// An hour's limit on the App's own scope, recorded just before the read, so the read's
		// whole seconds left are the same for every rig.
		if err := ratelimit.New(rig.database.Pool).Record(context.Background(), githubapp.AppScope, time.Hour); err != nil {
			t.Fatalf("record the App's rate limit: %v", err)
		}
		t.Cleanup(func() {
			if _, err := rig.database.Pool.Exec(context.Background(), `delete from github_rate_limits`); err != nil {
				t.Errorf("lift the App's rate limit: %v", err)
			}
		})
	}
	request := httptest.NewRequest(http.MethodGet, "http://dispatch.test"+gateRead, nil)
	request.Header.Set("X-Dispatch-User", login)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var decoded any
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("the proxy's answer %q is no JSON: %v", response.Body.String(), err)
	}
	return fmt.Sprintf("%d %q %s", response.Code, response.Header().Get("Retry-After"), strings.TrimSpace(response.Body.String()))
}

// gateGitHub is GitHub for one repository, acme/web: the App's installation on it when installed,
// a token for that installation, and the pull request, recording every read of the repository.
type gateGitHub struct {
	installed bool
	mu        sync.Mutex
	reads     []string
}

func (g *gateGitHub) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/web/installation":
		if !g.installed {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{"id":11,"app_slug":"envoy","permissions":{"pull_requests":"read"}}`)
	case r.Method == http.MethodPost && r.URL.Path == "/app/installations/11/access_tokens":
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"ghs_gate","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	default:
		g.mu.Lock()
		g.reads = append(g.reads, r.Method+" "+r.URL.RequestURI())
		g.mu.Unlock()
		fmt.Fprint(w, `{"state":"open","title":"Ship it"}`)
	}
}

// repositoryReads is every read of acme/web beyond the installation lookup and the token mint.
func (g *gateGitHub) repositoryReads() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.reads...)
}
