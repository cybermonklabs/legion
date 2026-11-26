package githubapi

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/githubtest"
)

// TestTheGitHubProxyAsksGitHubNothingForAHiddenRepository: the proxy decides whether the person
// may see the repository before it sends GitHub anything, so a hidden repository the App is
// installed on sends GitHub no request at all: no installation lookup, no token mint, no read. It
// answers what acme/elsewhere, which the App is not installed on, answers: not installed with an
// App key, and the rate limit under a limit already met on the App's scope, which the uninstalled
// repository's lookup meets without asking GitHub. The same repository, for a person it is not
// hidden from, is read from GitHub, so the count can see a request.
func TestTheGitHubProxyAsksGitHubNothingForAHiddenRepository(t *testing.T) {
	const hiddenRepository, uninstalled = "acme/web", "acme/elsewhere"
	hides := func(owner, repo string) (Gate, error) {
		return Gate{Hidden: owner+"/"+repo == hiddenRepository, Held: true}, nil
	}
	for _, test := range []struct {
		name   string
		limits githubapp.Limits
		answer string
	}{
		{"with an App key", nil, `503 "" {"code":"GITHUB_TOKEN_UNAVAILABLE","error":"the GitHub App is not installed on <repo>"}`},
		{"under a met rate limit", metAppLimit{recorded: time.Now()}, `503 "3600" {"code":"GITHUB_RATE_LIMITED","error":"GitHub rate-limited the App; retry after 1h0m0s"}`},
	} {
		client, asked := countedProxyRig(t, test.limits)
		missing := proxyAnswer(proxyAs(client, http.MethodGet, "/api/github/rest/repos/"+uninstalled+"/pulls/1", hides), uninstalled)
		if missing != test.answer {
			t.Fatalf("%s, the uninstalled %s answers %s, want %s: the comparison proves nothing", test.name, uninstalled, missing, test.answer)
		}
		before := len(asked())
		hidden := proxyAnswer(proxyAs(client, http.MethodGet, "/api/github/rest/repos/"+hiddenRepository+"/pulls/1", hides), hiddenRepository)
		if requests := asked()[before:]; hidden != missing || len(requests) != 0 {
			t.Errorf("%s, the hidden %s answers\n\t%s\nwith GitHub asked %q\nwant what %s answers, with GitHub asked nothing:\n\t%s",
				test.name, hiddenRepository, hidden, requests, uninstalled, missing)
		}
	}
	client, asked := countedProxyRig(t, nil)
	if seen := proxyAnswer(proxy(client, http.MethodGet, "/api/github/rest/repos/"+hiddenRepository+"/pulls/1"), hiddenRepository); seen[:4] != "200 " || len(asked()) == 0 {
		t.Errorf("a person %s is not hidden from reads it as %s with GitHub asked %q, want GitHub's 200 and requests counted", hiddenRepository, seen, asked())
	}
}

// countedProxyRig is limitedProxyRig's client, its GitHub recording every request it is sent,
// the installation lookups and token mints included, which asked answers.
func countedProxyRig(t *testing.T, limits githubapp.Limits) (*githubapp.Client, func() []string) {
	t.Helper()
	fake := &fakeGitHub{installations: map[string]int64{"acme/web": 11}}
	served := fake.handler(t)
	var mu sync.Mutex
	var requests []string
	base := githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		mu.Unlock()
		served.ServeHTTP(w, r)
	}})
	_, pemText := githubapptest.Key(t)
	var options []githubapp.Option
	if limits != nil {
		options = append(options, githubapp.WithLimits(limits))
	}
	client, err := githubapp.New(&auth.AppConfig{ClientID: "Iv1.proxy", PEM: pemText}, base, options...)
	if err != nil {
		t.Fatalf("githubapp.New: %v", err)
	}
	return client, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), requests...)
	}
}
