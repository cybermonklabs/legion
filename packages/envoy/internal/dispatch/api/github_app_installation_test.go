package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/githubtest"
)

// TestAppInstallationAnswersEveryCallerAlike: whether Dispatch's App is installed on a repository
// is no project's content, since a repository's name is not, so GET
// /api/v1/github/app-installation answers every caller alike, whatever projects it sees. acme/priv,
// which the App is installed on, is the repository PRIV's issue names; bob, carol and carol's token
// that lists PRIV see PRIV, and alice, alice's token that lists PRIV, carol's token that lists no
// project and the shared token do not, and every one of them reads it installed.
func TestAppInstallationAnswersEveryCallerAlike(t *testing.T) {
	const installed = "acme/priv"
	seed := seedPrivateProjectWith(t, testServerOptions{
		app: fakeGitHubAppConfig(t),
		githubAPIBase: githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/repos/"+installed+"/installation" {
				fmt.Fprint(w, `{"id":77,"app_slug":"dispatch-test","permissions":{}}`)
				return
			}
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
		}}),
	})
	if _, err := seed.database.Pool.Exec(context.Background(), `update issues set repo = $2 where key = $1`,
		seed.issue[seedPrivate], installed); err != nil {
		t.Fatalf("name %s on PRIV's issue: %v", installed, err)
	}
	for _, caller := range []privateCaller{callerBob, callerCarol, callerCarolPrivToken, callerAlice, callerAlicePrivToken,
		callerCarolToken, callerSharedToken} {
		response := seed.request(t, caller, http.MethodGet, "/api/v1/github/app-installation/"+installed, nil)
		if got := fmt.Sprintf("%d %s", response.Code, strings.TrimSpace(response.Body.String())); got != `200 {"installed":true}` {
			t.Errorf("%s reads whether the App is installed on %s as %s, want 200 installed", caller, installed, got)
		}
	}
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
