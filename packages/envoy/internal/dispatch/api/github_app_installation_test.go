package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/ratelimit"
	"github.com/sjawhar/envoy/internal/githubtest"
)

// TestAppInstallationAnswersAHiddenRepositoryAsNotInstalled: GET /api/v1/github/app-installation
// answers a repository its caller may not see (repositoriesHidden) exactly as one the App is not
// installed on, as the GitHub proxy does, and asks GitHub nothing about it. acme/priv, which the App
// is installed on, is the repository PRIV's issue names: bob, carol and carol's token that lists
// PRIV see PRIV and read it installed; alice, alice's token that lists PRIV, carol's token that
// lists no project and the shared token do not, and each reads, in status and bytes, what
// acme/elsewhere, which the App is not installed on, answers, with no request to GitHub. Under a
// rate limit already met on the App's scope the two answer alike too: the held limit, which the
// uninstalled repository's lookup meets without asking GitHub.
func TestAppInstallationAnswersAHiddenRepositoryAsNotInstalled(t *testing.T) {
	const installed, uninstalled = "acme/priv", "acme/elsewhere"
	var mu sync.Mutex
	var asked []string
	seed := seedPrivateProjectWith(t, testServerOptions{
		app: fakeGitHubAppConfig(t),
		githubAPIBase: githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			asked = append(asked, r.Method+" "+r.URL.Path)
			mu.Unlock()
			if r.URL.Path == "/repos/"+installed+"/installation" {
				fmt.Fprint(w, `{"id":77,"app_slug":"dispatch-test","permissions":{}}`)
				return
			}
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
		}}),
	})
	requests := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), asked...)
	}
	if _, err := seed.database.Pool.Exec(context.Background(), `update issues set repo = $2 where key = $1`,
		seed.issue[seedPrivate], installed); err != nil {
		t.Fatalf("name %s on PRIV's issue: %v", installed, err)
	}
	answer := func(caller privateCaller, repository string) string {
		response := seed.request(t, caller, http.MethodGet, "/api/v1/github/app-installation/"+repository, nil)
		return strings.ReplaceAll(fmt.Sprintf("%d %s", response.Code, strings.TrimSpace(response.Body.String())), repository, "<repo>")
	}
	for _, caller := range []privateCaller{callerBob, callerCarol, callerCarolPrivToken} {
		if got := answer(caller, installed); got != `200 {"installed":true}` {
			t.Errorf("%s, who sees PRIV, reads whether the App is installed on %s as %s, want 200 installed", caller, installed, got)
		}
	}
	// read compares each caller that may not see acme/priv against acme/elsewhere, whose answer
	// must start with uninstalledAnswer, or the comparison would prove nothing.
	read := func(state, uninstalledAnswer string) {
		for _, caller := range []privateCaller{callerAlice, callerAlicePrivToken, callerCarolToken, callerSharedToken} {
			missing := answer(caller, uninstalled)
			if !strings.HasPrefix(missing, uninstalledAnswer) {
				t.Fatalf("%s %s reads %s, which the App is not installed on, as %s, want %s…: the comparison proves nothing",
					caller, state, uninstalled, missing, uninstalledAnswer)
			}
			before := len(requests())
			got := answer(caller, installed)
			if newly := requests()[before:]; got != missing || len(newly) != 0 {
				t.Errorf("%s %s reads whether the App is installed on %s, which it may not see, as\n\t%s\nGitHub asked %q\nwant what %s answers, with GitHub asked nothing:\n\t%s",
					caller, state, installed, got, newly, uninstalled, missing)
			}
		}
	}
	read("with no rate limit", `200 {"installed":false}`)
	if err := ratelimit.New(seed.database.Pool).Record(context.Background(), githubapp.AppScope, time.Hour); err != nil {
		t.Fatalf("record a rate limit on the App's scope: %v", err)
	}
	read("under a met App rate limit", `502 {"code":"GITHUB_UNREADABLE","error":"read the GitHub App's installation on <repo>: GitHub rate limit on app`)
}

// appInstallationServer is Dispatch holding an App key, its GitHub answering the installation
// lookup with github.
func appInstallationServer(t *testing.T, github http.HandlerFunc) http.Handler {
	t.Helper()
	upstream := githubtest.Server(t, githubtest.Spec{Handle: github})
	handler, _, _ := newTestServer(t, testServerOptions{app: fakeGitHubAppConfig(t), githubAPIBase: upstream})
	return handler
}

// An agent asks whether Dispatch's App is installed on a repository: GitHub's installation is
// true, whatever permissions it grants, and GitHub's 404 is false.
func TestAgentReadsWhetherTheAppInstallationExists(t *testing.T) {
	handler := appInstallationServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widgets/installation":
			fmt.Fprint(w, `{"id":77,"app_slug":"dispatch-test","permissions":{}}`)
		case "/repos/acme/gadgets/installation":
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
		default:
			t.Errorf("GitHub request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
		}
	})
	for repo, want := range map[string]bool{"acme/widgets": true, "acme/gadgets": false} {
		response := agentRequest(t, handler, http.MethodGet, "/api/v1/github/app-installation/"+repo, nil, "agent-token")
		if response.Code != http.StatusOK {
			t.Fatalf("GET app-installation %s: status=%d body=%s", repo, response.Code, response.Body.String())
		}
		if got := decodeStrict[appInstallationResponse](t, response); got.Installed != want {
			t.Fatalf("app-installation %s = %+v, want installed %v", repo, got, want)
		}
	}
}

// Dispatch with no App key cannot ask GitHub, which is no answer about the repository.
func TestAppInstallationWithoutAnAppKeyAnswers503(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	response := agentRequest(t, handler, http.MethodGet, "/api/v1/github/app-installation/acme/widgets", nil, "agent-token")
	body := decodeBody[map[string]string](t, response)
	if response.Code != http.StatusServiceUnavailable || body["code"] != "GITHUB_APP_UNCONFIGURED" {
		t.Fatalf("no app key: status=%d body=%s", response.Code, response.Body.String())
	}
}

// GitHub failing the lookup is no answer either: 502 naming the repository and GitHub's error.
func TestAppInstallationGitHubFailureAnswers502(t *testing.T) {
	handler := appInstallationServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"message":"Server Error"}`)
	})
	response := agentRequest(t, handler, http.MethodGet, "/api/v1/github/app-installation/acme/widgets", nil, "agent-token")
	body := decodeBody[map[string]string](t, response)
	if response.Code != http.StatusBadGateway || body["code"] != "GITHUB_UNREADABLE" ||
		!strings.Contains(body["error"], "acme/widgets") || !strings.Contains(body["error"], "status 500") {
		t.Fatalf("GitHub 500: status=%d body=%s", response.Code, response.Body.String())
	}
}
