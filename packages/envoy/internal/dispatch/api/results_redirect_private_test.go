package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/githubtest"
)

// TestAResultReadsNoRepositoryGitHubRedirectsAnIssuesRepositoryTo: while a project is private the
// GitHub client holds every request that names a repository to that repository
// (githubapp.WithPrivateProjects), so a read keyed by an issue's repository name never reads the
// repository GitHub answers an old name with: the redirect is answered as a repository the App is
// not installed on. GitHub answers every read of acme/old with a redirect to acme/new, as it
// answers the old name of a renamed repository, and acme/new's pull request 12 has the commit as
// its head. PUB's issue names acme/old. carol records a result of PUB's check at that commit:
// while PRIV is private the record's reads (refuseUnknownCommit's commit read and
// readThroughPullRequests) ask acme/new nothing, and no pull request is stored under acme/old. With
// no project private the reads follow GitHub's redirect, as before (R9), and pull request 12 is
// stored under acme/old.
//
// Control: the client built without WithPrivateProjects; the read-through follows the redirect
// while PRIV is private, stores acme/new's pull request under acme/old, and the test fails.
func TestAResultReadsNoRepositoryGitHubRedirectsAnIssuesRepositoryTo(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	asked := map[string]int{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, r *http.Request) {
		noteRepositoryAsk(&mu, asked, r)
		fmt.Fprint(w, `{"id":77,"app_slug":"dispatch-test","permissions":{"contents":"read","pull_requests":"read"}}`)
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"ghs_fake_77","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}", func(w http.ResponseWriter, r *http.Request) {
		noteRepositoryAsk(&mu, asked, r)
		fmt.Fprintf(w, `{"sha":%q}`, r.PathValue("sha"))
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}/pulls", func(w http.ResponseWriter, r *http.Request) {
		noteRepositoryAsk(&mu, asked, r)
		fmt.Fprintf(w, `[{"number":12,"title":"Renamed work","body":"","html_url":"https://github.com/acme/new/pull/12",
			"state":"open","merged":false,"updated_at":"2026-01-01T00:00:00Z",
			"head":{"ref":"legion/x","sha":%q,"repo":{"full_name":"acme/new"}},"base":{"ref":"main"}}]`, r.PathValue("sha"))
	})
	seed := seedPrivateProjectWith(t, testServerOptions{
		app: fakeGitHubAppConfig(t),
		githubAPIBase: githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
			if rest, moved := strings.CutPrefix(r.URL.Path, "/repos/acme/old"); moved {
				http.Redirect(w, r, "/repos/acme/new"+rest, http.StatusMovedPermanently)
				return
			}
			mux.ServeHTTP(w, r)
		}}),
	})
	ctx := context.Background()
	if _, err := seed.database.Pool.Exec(ctx, `update issues set repo = 'acme/old' where key = $1`, seed.issue[seedPublic]); err != nil {
		t.Fatalf("name acme/old on PUB's issue: %v", err)
	}
	seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues/"+seed.issue[seedPublic]+"/artifacts",
		map[string]any{"name": "verification.md", "content": twoChecks}, http.StatusCreated)
	stored := func() int {
		t.Helper()
		var heads int
		if err := seed.database.Pool.QueryRow(ctx, `select count(*) from pull_request_heads where repo = 'acme/old'`).Scan(&heads); err != nil {
			t.Fatalf("read the pull requests stored under acme/old: %v", err)
		}
		return heads
	}

	seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues/"+seed.issue[seedPublic]+"/results",
		resultBody("k1", "pass", headCommit, nil), http.StatusCreated)
	mu.Lock()
	reached := asked["acme/new"]
	mu.Unlock()
	if heads := stored(); heads != 0 || reached != 0 {
		t.Errorf("while PRIV is private a result of PUB's issue, which names acme/old, stored %d pull requests under acme/old and asked acme/new %d times, want none of either", heads, reached)
	}

	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues/"+seed.issue[seedPublic]+"/results",
		resultBody("k1", "pass", headCommit, nil), http.StatusCreated)
	if heads := stored(); heads != 1 {
		t.Errorf("with no project private a result of PUB's issue stored %d pull requests under acme/old, want acme/new's pull request 12 read through GitHub's redirect (R9)", heads)
	}
}

// noteRepositoryAsk notes one request of the repository its path names.
func noteRepositoryAsk(mu *sync.Mutex, asked map[string]int, r *http.Request) {
	mu.Lock()
	defer mu.Unlock()
	asked[r.PathValue("owner")+"/"+r.PathValue("repo")]++
}
