package githubapi

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/githubtest"
)

// privateAnswer is what GitHub answers for a repository a redirect leads to: a pull request of a
// private project's repository, which no answer the proxy gives a held read may carry.
const privateAnswer = `{"state":"open","title":"The private project's pull request"}`

// renamedAnswer is what GitHub answers for acme/new, the repository acme/old was renamed to.
const renamedAnswer = `{"state":"open","title":"The renamed repository's pull request"}`

// redirectingGitHub is a fake GitHub that answers an old name of a moved repository with a 301 to
// the repository as it is now, as GitHub answers a renamed or transferred repository's old name,
// and records every request it is sent. acme/old's pull request moved to acme/new, by name, and
// acme/oldid's to /repositories/4242, by the moved repository's id; acme/moved's installation
// lookup moved to other/priv's; acme/web's pull request answers a 301 to ACME/Web, the same
// repository in another case. acme/none has no installation.
type redirectingGitHub struct {
	mu       sync.Mutex
	requests []string
}

func (f *redirectingGitHub) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	installation := func(id int64) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "app_slug": "envoy", "permissions": map[string]string{"pull_requests": "read"}})
	}
	switch path := r.URL.Path; {
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/app/installations/"):
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_installation_11", "expires_at": time.Now().Add(time.Hour)})
	case path == "/repos/acme/old/installation", path == "/repos/acme/oldid/installation", path == "/repos/acme/web/installation":
		installation(11)
	case path == "/repos/acme/moved/installation":
		http.Redirect(w, r, "/repos/other/priv/installation", http.StatusMovedPermanently)
	case path == "/repos/other/priv/installation":
		installation(11)
	case path == "/repos/acme/none/installation":
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	case path == "/repos/acme/old/pulls/7":
		http.Redirect(w, r, "/repos/acme/new/pulls/7", http.StatusMovedPermanently)
	case path == "/repos/acme/oldid/pulls/7":
		http.Redirect(w, r, "/repositories/4242/pulls/7", http.StatusMovedPermanently)
	case path == "/repos/acme/web/pulls/7":
		http.Redirect(w, r, "/repos/ACME/Web/pulls/7", http.StatusMovedPermanently)
	case path == "/repos/ACME/Web/pulls/7":
		_, _ = w.Write([]byte(`{"state":"open","title":"Ship it"}`))
	case path == "/repos/acme/new/pulls/7":
		_, _ = w.Write([]byte(renamedAnswer))
	case path == "/repos/other/priv/pulls/7", path == "/repositories/4242/pulls/7":
		_, _ = w.Write([]byte(privateAnswer))
	default:
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}
}

// sent answers every request the fake was sent, in order.
func (f *redirectingGitHub) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

// newRedirectRig is a client of a fresh redirectingGitHub.
func newRedirectRig(t *testing.T) (*githubapp.Client, *redirectingGitHub) {
	t.Helper()
	fake := &redirectingGitHub{}
	base := githubtest.Server(t, githubtest.Spec{Handle: fake.handle})
	_, pemText := githubapptest.Key(t)
	client, err := githubapp.New(&auth.AppConfig{ClientID: "Iv1.redirect", PEM: pemText}, base)
	if err != nil {
		t.Fatalf("githubapp.New: %v", err)
	}
	return client, fake
}

// readsMoved reports the requests that reached a repository a redirect leads to.
func readsMoved(requests []string) []string {
	return slices.DeleteFunc(slices.Clone(requests), func(request string) bool {
		return !strings.Contains(request, "/repos/acme/new/") && !strings.Contains(request, "/repos/other/priv/") &&
			!strings.Contains(request, "/repositories/")
	})
}

// heldGate is the gate's answer for a person who sees the repository while the deployment holds a
// private project: shown, and held to it.
func heldGate(string, string) (Gate, error) { return Gate{Held: true}, nil }

// TestTheProxyFollowsNoRedirectToAnotherRepository: a held read the proxy's gate passed (a
// deployment that holds a private project), of a name GitHub answers with a redirect to another
// repository, answers exactly what a repository the App is not installed on answers (status,
// content type and bytes, the name substituted), and the repository the redirect leads to is never
// read: a pull request of acme/old that GitHub moved to acme/new, one of acme/oldid that it moved
// to /repositories/4242, and acme/moved, whose installation lookup GitHub moved to other/priv's. A
// redirect to the same repository in another case, acme/web to ACME/Web, is followed as before.
// A read that is not held (a deployment with no private project) follows GitHub's redirect from
// acme/old to acme/new and answers GitHub's 200, as before any project could be private, and so
// does a read outside the proxy.
func TestTheProxyFollowsNoRedirectToAnotherRepository(t *testing.T) {
	client, fake := newRedirectRig(t)
	notInstalled := proxyAs(client, http.MethodGet, "/api/github/rest/repos/acme/none/pulls/7", heldGate)
	if notInstalled.Code != http.StatusServiceUnavailable || errorCode(t, notInstalled) != "GITHUB_TOKEN_UNAVAILABLE" {
		t.Fatalf("the uninstalled repository: %d %s, want 503 GITHUB_TOKEN_UNAVAILABLE", notInstalled.Code, notInstalled.Body.String())
	}
	for _, moved := range []string{"acme/old", "acme/oldid", "acme/moved"} {
		before := len(fake.sent())
		response := proxyAs(client, http.MethodGet, "/api/github/rest/repos/"+moved+"/pulls/7", heldGate)
		want := strings.ReplaceAll(notInstalled.Body.String(), "acme/none", moved)
		if response.Code != notInstalled.Code || response.Header().Get("Content-Type") != notInstalled.Header().Get("Content-Type") || response.Body.String() != want {
			t.Errorf("%s, held, which GitHub redirects to another repository: %d %q %s, want %d %q %s",
				moved, response.Code, response.Header().Get("Content-Type"), response.Body.String(),
				notInstalled.Code, notInstalled.Header().Get("Content-Type"), want)
		}
		if strings.Contains(response.Body.String(), "pull request") {
			t.Errorf("%s, held: the answer carries the moved repository's pull request: %s", moved, response.Body.String())
		}
		if reached := readsMoved(fake.sent()[before:]); len(reached) > 0 {
			t.Errorf("%s, held: the proxy read the repository GitHub's redirect leads to: %v", moved, reached)
		}
	}

	before := len(fake.sent())
	sameRepository := proxyAs(client, http.MethodGet, "/api/github/rest/repos/acme/web/pulls/7", heldGate)
	if sameRepository.Code != http.StatusOK || sameRepository.Body.String() != `{"state":"open","title":"Ship it"}` {
		t.Errorf("acme/web, held, which GitHub redirects to ACME/Web: %d %s, want 200 with its pull request", sameRepository.Code, sameRepository.Body.String())
	}
	if !slices.Contains(fake.sent()[before:], "GET /repos/ACME/Web/pulls/7") {
		t.Errorf("the redirect to ACME/Web was not followed: %v", fake.sent()[before:])
	}

	// R9: with no private project the read is not held, and GitHub's redirect is followed.
	before = len(fake.sent())
	unheld := proxy(client, http.MethodGet, "/api/github/rest/repos/acme/old/pulls/7")
	if unheld.Code != http.StatusOK || unheld.Header().Get("Content-Type") != "application/json" || unheld.Body.String() != renamedAnswer {
		t.Errorf("acme/old, not held, which GitHub redirects to acme/new: %d %q %s, want GitHub's 200 %s",
			unheld.Code, unheld.Header().Get("Content-Type"), unheld.Body.String(), renamedAnswer)
	}
	if !slices.Contains(fake.sent()[before:], "GET /repos/acme/new/pulls/7") {
		t.Errorf("the read that is not held did not follow GitHub's redirect to acme/new: %v", fake.sent()[before:])
	}

	// The instrument's own control: the redirect a held read refuses is a real one, which a read
	// that is not the proxy's follows to the moved repository.
	body, status, _, err := client.Read(context.Background(), "ghs_installation_11", "/repos/acme/oldid/pulls/7")
	if err != nil || status != http.StatusOK || string(body) != privateAnswer {
		t.Fatalf("a read outside the proxy of acme/oldid = %d %s %v, want GitHub's redirect followed to the moved pull request", status, body, err)
	}
}
