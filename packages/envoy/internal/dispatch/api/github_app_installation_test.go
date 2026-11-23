package api

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/githubtest"
)

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

// A lookup GitHub fails with a page that is no JSON carries the page in its error as
// githubapp.ShownBody shows it, so the 502 words it as every GITHUB_UNREADABLE does
// (gitHubReason): the start of the page and its size, never the page whole. The page here is
// ASCII, so its first 512 characters and its size come to fewer than gitHubReasonBytes with the
// rest of the sentence: ShownBody's bound is the one that acts, and gitHubReason's cut, which
// bounds the whole reason, finds nothing to cut.
func TestAppInstallationGitHubFailureWithAPageIsCutToTheBound(t *testing.T) {
	page := "<html>" + strings.Repeat("x", 4*gitHubReasonBytes) + "</html>"
	handler := appInstallationServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, page)
	})
	response := agentRequest(t, handler, http.MethodGet, "/api/v1/github/app-installation/acme/widgets", nil, "agent-token")
	body := decodeBody[map[string]string](t, response)
	if response.Code != http.StatusBadGateway || body["code"] != "GITHUB_UNREADABLE" {
		t.Fatalf("GitHub 502 page: status=%d body=%.300s", response.Code, response.Body.String())
	}
	said := body["error"]
	if !strings.Contains(said, "acme/widgets") || !strings.Contains(said, "<html>xxxx") ||
		!strings.Contains(said, fmt.Sprintf("… (%d bytes)", len(page))) {
		t.Fatalf("the refusal reads %.300q, want the repository, the start of the page and its size named", said)
	}
	if longest := gitHubReasonBytes + 512; len(said) > longest {
		t.Fatalf("the refusal is %d bytes, want at most %d: GitHub's page went in whole", len(said), longest)
	}
}

// A rate limit GitHub answers the lookup with is worded by the `message` of its JSON in place of
// its body, read from the whole body (gitHubErrorText), so a message of a few kilobytes reaches
// gitHubReason whole, past githubapp.ShownBody's bound: gitHubReason's cut is the bound that acts
// on it, and the 502 names the start of the message and says it was cut, never the message whole.
// The lookup words every other refusal by its status and its body (githubapp's installation
// lookup, StatusText), which ShownBody bounds, so a rate limit is the refusal whose message
// reaches the cut.
func TestAppInstallationGitHubFailureWithALongMessageIsCutToTheBound(t *testing.T) {
	message := "API rate limit exceeded for installation ID 77. " + strings.Repeat("y", 4*gitHubReasonBytes)
	handler := appInstallationServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"message":"`+message+`"}`)
	})
	response := agentRequest(t, handler, http.MethodGet, "/api/v1/github/app-installation/acme/widgets", nil, "agent-token")
	body := decodeBody[map[string]string](t, response)
	if response.Code != http.StatusBadGateway || body["code"] != "GITHUB_UNREADABLE" {
		t.Fatalf("GitHub 429 with a long message: status=%d body=%.300s", response.Code, response.Body.String())
	}
	said := body["error"]
	if !strings.Contains(said, "acme/widgets") || !strings.Contains(said, "status 429") ||
		!strings.Contains(said, "API rate limit exceeded for installation ID 77. yyyy") ||
		!strings.Contains(said, "(GitHub's error, cut to its first") {
		t.Fatalf("the refusal reads %.300q, want the repository, the status, the start of the message and the cut named", said)
	}
	if strings.Contains(said, message) {
		t.Fatalf("the refusal holds GitHub's %d-byte message whole", len(message))
	}
	if longest := gitHubReasonBytes + 512; len(said) > longest {
		t.Fatalf("the refusal is %d bytes, want at most %d: GitHub's message went in whole", len(said), longest)
	}
}

// The installation read's path is held to the names GitHub allows, as a source's is
// (githubRepoPath): its halves go into the lookup's own path as they are, and `...git` and
// `.git` are no dot segments, so the mux does not clean them, while the trailing `.git` trimmed
// leaves `..` or an empty name, which would ask GitHub about `/repos/acme/../installation`.
// `.git.git` and `...git.git` are the same names once canonicalising has trimmed one `.git`. Each
// is refused 400 INVALID_REPOSITORY, and GitHub is asked nothing.
func TestAppInstallationPathIsHeldToTheNamesGitHubAllows(t *testing.T) {
	var asked atomic.Int32
	handler := appInstallationServer(t, func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		t.Errorf("GitHub was asked %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	})
	for _, path := range []string{"acme/...git", "acme/.git", "acme/.git.git", "acme/...git.git"} {
		response := agentRequest(t, handler, http.MethodGet, "/api/v1/github/app-installation/"+path, nil, "agent-token")
		if code, _ := refusalOf(t, response); response.Code != http.StatusBadRequest || code != "INVALID_REPOSITORY" {
			t.Errorf("GET app-installation %s: status=%d body=%s, want 400 INVALID_REPOSITORY", path, response.Code, response.Body.String())
		}
	}
	if requests := asked.Load(); requests != 0 {
		t.Fatalf("the refused paths sent GitHub %d requests, want none", requests)
	}
}
