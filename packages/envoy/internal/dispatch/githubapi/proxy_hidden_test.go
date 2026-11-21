package githubapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/ratelimit"
	"github.com/sjawhar/envoy/internal/githubtest"
)

// TestTheGitHubProxyAnswersAHiddenRepositoryAsUninstalled: a repository hidden from the person
// asking, which the App is installed on, answers exactly what a repository the App is not
// installed on answers: equal status, Retry-After and body, the repository's name aside, with an
// App key, with no App key, and under a rate limit another process already met on the App's
// scope. GitHub is never read for it. The same repository, for a person it is not hidden from,
// answers GitHub's 200, so the answer the hidden read gets is the gate's and not a client that
// reads nothing.
func TestTheGitHubProxyAnswersAHiddenRepositoryAsUninstalled(t *testing.T) {
	const hiddenRepository, uninstalled = "acme/web", "acme/elsewhere"
	hides := func(owner, repo string) (Gate, error) {
		return Gate{Hidden: owner+"/"+repo == hiddenRepository, Held: true}, nil
	}
	for _, test := range []struct {
		name   string
		client func(t *testing.T) (*githubapp.Client, *fakeGitHub)
	}{
		{"with an App key", func(t *testing.T) (*githubapp.Client, *fakeGitHub) { return limitedProxyRig(t, nil) }},
		{"with no App key", func(*testing.T) (*githubapp.Client, *fakeGitHub) { return nil, &fakeGitHub{} }},
		{"under a met rate limit", func(t *testing.T) (*githubapp.Client, *fakeGitHub) {
			return limitedProxyRig(t, metAppLimit{recorded: time.Now()})
		}},
	} {
		client, fake := test.client(t)
		hidden := proxyAnswer(proxyAs(client, http.MethodGet, "/api/github/rest/repos/"+hiddenRepository+"/pulls/1", hides), hiddenRepository)
		missing := proxyAnswer(proxyAs(client, http.MethodGet, "/api/github/rest/repos/"+uninstalled+"/pulls/1", hides), uninstalled)
		if hidden != missing {
			t.Errorf("%s, a hidden installed repository answers\n\t%s\nand an uninstalled one\n\t%s", test.name, hidden, missing)
		}
		if reads := fake.seen(); len(reads) != 0 {
			t.Errorf("%s, GitHub was read for a hidden repository: %q", test.name, reads)
		}
	}
	client, _ := limitedProxyRig(t, nil)
	if seen := proxyAnswer(proxy(client, http.MethodGet, "/api/github/rest/repos/"+hiddenRepository+"/pulls/1"), hiddenRepository); !strings.HasPrefix(seen, "200 ") {
		t.Errorf("a person %s is not hidden from reads it as %s, want GitHub's 200", hiddenRepository, seen)
	}
}

// limitedProxyRig is newProxyRig's client and fake GitHub, the client obeying limits as every
// process's shared rate limits when limits is not nil.
func limitedProxyRig(t *testing.T, limits githubapp.Limits) (*githubapp.Client, *fakeGitHub) {
	t.Helper()
	fake := &fakeGitHub{installations: map[string]int64{"acme/web": 11}}
	base := githubtest.Server(t, githubtest.Spec{Handle: fake.handler(t).ServeHTTP})
	_, pemText := githubapptest.Key(t)
	var options []githubapp.Option
	if limits != nil {
		options = append(options, githubapp.WithLimits(limits))
	}
	client, err := githubapp.New(&auth.AppConfig{ClientID: "Iv1.proxy", PEM: pemText}, base, options...)
	if err != nil {
		t.Fatalf("githubapp.New: %v", err)
	}
	return client, fake
}

// metAppLimit is a rate limit another process met on the App's scope an hour before it ends,
// recorded at recorded; no other scope is limited.
type metAppLimit struct{ recorded time.Time }

func (l metAppLimit) Wait(_ context.Context, scope string) (time.Duration, time.Time, error) {
	if scope != githubapp.AppScope {
		return 0, time.Time{}, nil
	}
	return time.Hour, l.recorded, nil
}

func (metAppLimit) Record(context.Context, string, time.Duration) error { return nil }

func (metAppLimit) Strike(context.Context, string, time.Duration) (ratelimit.Limit, error) {
	return ratelimit.Limit{}, nil
}

// proxyAnswer is an answer of the proxy as "<status> <Retry-After> <body>", with repository's own
// name written as <repo>, the one byte of an answer two repositories may differ in.
func proxyAnswer(response *httptest.ResponseRecorder, repository string) string {
	return strings.ReplaceAll(fmt.Sprintf("%d %q %s", response.Code, response.Header().Get("Retry-After"),
		strings.TrimSpace(response.Body.String())), repository, "<repo>")
}
