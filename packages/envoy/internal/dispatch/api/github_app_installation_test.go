package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/ratelimit"
	"github.com/sjawhar/envoy/internal/githubtest"
)

// TestAHiddenRepositoryAnswersAsUninstalled: GET /api/v1/github/app-installation answers a
// repository the caller may not see (access.RepositoryClause) exactly as it answers one the App is
// not installed on, in status and in bytes, with an App key, with no App key, and under a rate
// limit already met on the App's scope. acme/priv, which the App is installed on, is mapped to the
// private PRIV; acme/gadgets, which it is not installed on, is mapped to nothing. alice, alice's
// token that lists PRIV, carol's token that lists no project and the shared token may not see
// acme/priv; bob, carol and carol's token that lists PRIV may, and read the App installed on it,
// so the answer the others get is the gate's and not a deployment that reads nothing.
func TestAHiddenRepositoryAnswersAsUninstalled(t *testing.T) {
	const hidden, uninstalled = "acme/priv", "acme/gadgets"
	keyed := installationSeed(t, true, hidden)
	keyless := installationSeed(t, false, hidden)
	// answer is what caller reads of repo, as "<status> <body>", with the repository's own name
	// written as <repo>, the one byte of an answer two repositories may differ in.
	answer := func(seed privateSeed, caller privateCaller, repo string) string {
		response := seed.request(t, caller, http.MethodGet, "/api/v1/github/app-installation/"+repo, nil)
		return fmt.Sprintf("%d %s", response.Code, strings.ReplaceAll(strings.TrimSpace(response.Body.String()), repo, "<repo>"))
	}
	for _, caller := range []privateCaller{callerBob, callerCarol, callerCarolPrivToken} {
		if got := answer(keyed, caller, hidden); got != `200 {"installed":true}` {
			t.Errorf("%s, who sees PRIV, read %s as %s, want 200 installed", caller, hidden, got)
		}
	}
	compare := func(name string, seed privateSeed, want string) {
		t.Helper()
		for _, caller := range []privateCaller{callerAlice, callerAlicePrivToken, callerCarolToken, callerSharedToken} {
			missing := answer(seed, caller, uninstalled)
			if !strings.HasPrefix(missing, want) {
				t.Fatalf("%s, %s read %s, which the App is not installed on, as %s, want %s: the comparison proves nothing", name, caller, uninstalled, missing, want)
			}
			if got := answer(seed, caller, hidden); got != missing {
				t.Errorf("%s, %s reads %s, which it may not see, as\n\t%s\nand %s, which the App is not installed on, as\n\t%s", name, caller, hidden, got, uninstalled, missing)
			}
		}
	}
	compare("with an App key", keyed, `200 {"installed":false}`)
	compare("with no App key", keyless, "503 ")
	if err := ratelimit.New(keyed.database.Pool).Record(context.Background(), githubapp.AppScope, time.Hour); err != nil {
		t.Fatalf("record a rate limit on the App's scope: %v", err)
	}
	compare("under a met rate limit", keyed, "502 ")
}

// installationSeed is verification v16's seed on a test server whose GitHub has the App installed
// on mapped alone, which carol maps to PRIV; with withKey, Dispatch holds the App's key.
func installationSeed(t *testing.T, withKey bool, mapped string) privateSeed {
	t.Helper()
	var options testServerOptions
	if withKey {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generate fixture key: %v", err)
		}
		options.app = &auth.AppConfig{ClientID: "Iv1.test", PEM: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))}
		options.githubAPIBase = githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/repos/"+mapped+"/installation" {
				fmt.Fprint(w, `{"id":77,"app_slug":"dispatch-test","permissions":{}}`)
				return
			}
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
		}})
	}
	seed := seedPrivateProjectWith(t, options)
	seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/settings/repo-projects/"+mapped, map[string]string{"project": seedPrivate}, http.StatusOK)
	return seed
}

// appInstallationServer is Dispatch holding an App key, its GitHub answering the installation
// lookup with github.
func appInstallationServer(t *testing.T, github http.HandlerFunc) http.Handler {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate fixture key: %v", err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	upstream := githubtest.Server(t, githubtest.Spec{Handle: github})
	handler, _, _ := newTestServer(t, testServerOptions{
		app:           &auth.AppConfig{ClientID: "Iv1.test", PEM: pemText},
		githubAPIBase: upstream,
	})
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
