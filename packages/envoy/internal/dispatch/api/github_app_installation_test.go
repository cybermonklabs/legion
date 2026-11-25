package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/ratelimit"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/githubtest"
)

// TestAppInstallationAnswersAHiddenRepositoryAsNotInstalled: GET /api/v1/github/app-installation
// holds a repository to the GitHub proxy's rule (refs.RepositoryClause): one its caller may not
// read answers, in status and bytes, exactly what acme/elsewhere, which the App is not installed
// on, answers, and GitHub is asked nothing about it. The App is installed on acme/priv, which PRIV's
// issue names, acme/pub, which PUB's issue names, and acme/unowned, which no issue names. bob,
// bob's token that lists PRIV, carol and carol's token that lists PRIV see PRIV and read acme/priv
// installed; alice, alice's token that lists PRIV, carol's token that lists no project and the
// shared token do not, and read it as not installed. Every open viewer reads
// acme/pub and acme/unowned installed, the two full members' tokens that list PRIV among them,
// since a full member's list decides only which private projects it reads
// (access.CredentialCovers); bob and bob's token that lists PRIV, a restricted member's, are not
// open, and read only a repository an issue they see resolves to. Under a rate limit already met
// on the App's scope a hidden repository answers alike too: the held limit, which the uninstalled
// repository's lookup meets without asking GitHub.
//
// With no project private, every full member's browser and token, whatever it lists, and the
// shared token read every repository from GitHub, as before any project could be private, and the
// route reads no statement of the repository rule for them (R9).
//
// Control: the route answering every caller alike (GitHub asked for every repository, whoever
// asks): alice reads acme/priv as installed, and the test fails naming her.
func TestAppInstallationAnswersAHiddenRepositoryAsNotInstalled(t *testing.T) {
	const uninstalled = "acme/elsewhere"
	installed := []string{"acme/priv", "acme/pub", "acme/unowned"}
	var mu sync.Mutex
	var asked []string
	trace := &existenceTrace{}
	seed := seedPrivateProjectWith(t, testServerOptions{
		store: storetest.TracedStore(t, storetest.Open(t), trace),
		app:   fakeGitHubAppConfig(t),
		githubAPIBase: githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			asked = append(asked, r.Method+" "+r.URL.Path)
			mu.Unlock()
			for _, repository := range installed {
				if r.URL.Path == "/repos/"+repository+"/installation" {
					fmt.Fprint(w, `{"id":77,"app_slug":"dispatch-test","permissions":{}}`)
					return
				}
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
	for project, repository := range map[string]string{seedPrivate: "acme/priv", seedPublic: "acme/pub"} {
		if _, err := seed.database.Pool.Exec(context.Background(), `update issues set repo = $2 where key = $1`,
			seed.issue[project], repository); err != nil {
			t.Fatalf("name %s on %s's issue: %v", repository, project, err)
		}
	}
	// answer is caller's answer for repository, with the repository's name written <repo>, and the
	// statements of the repository rule the request read.
	answer := func(caller privateCaller, repository string) (string, []string) {
		trace.take()
		response := seed.requestIn(t, context.WithValue(context.Background(), recordingKey{}, true), caller,
			http.MethodGet, "/api/v1/github/app-installation/"+repository, nil)
		rule := slices.DeleteFunc(trace.take(), func(statement string) bool { return !strings.Contains(statement, "as asked(repo)") })
		return strings.ReplaceAll(fmt.Sprintf("%d %s", response.Code, strings.TrimSpace(response.Body.String())), repository, "<repo>"), rule
	}
	// read holds each caller to what it may read of each repository, readers naming the callers
	// that read it installed: each of those is answered from GitHub's lookup, and every other
	// caller is answered what acme/elsewhere answers it, whose answer must start with
	// uninstalledAnswer or the comparison proves nothing, with GitHub asked nothing.
	read := func(state, uninstalledAnswer string, readers map[string][]privateCaller) {
		t.Helper()
		for _, repository := range installed {
			for _, caller := range privateCallers {
				before := len(requests())
				got, _ := answer(caller, repository)
				newly := requests()[before:]
				if slices.Contains(readers[repository], caller) {
					if got != `200 {"installed":true}` || !slices.Contains(newly, "GET /repos/"+repository+"/installation") {
						t.Errorf("%s %s reads whether the App is installed on %s as %s with GitHub asked %q, want 200 installed from GitHub's lookup",
							caller, state, repository, got, newly)
					}
					continue
				}
				missing, _ := answer(caller, uninstalled)
				if !strings.HasPrefix(missing, uninstalledAnswer) {
					t.Fatalf("%s %s reads %s, which the App is not installed on, as %s, want %s…: the comparison proves nothing",
						caller, state, uninstalled, missing, uninstalledAnswer)
				}
				if got != missing || len(newly) != 0 {
					t.Errorf("%s %s reads whether the App is installed on %s, which it may not read, as\n\t%s\nGitHub asked %q\nwant what %s answers, with GitHub asked nothing:\n\t%s",
						caller, state, repository, got, newly, uninstalled, missing)
				}
			}
		}
	}
	openViewers := []privateCaller{callerAlice, callerCarol, callerCarolPrivToken, callerAlicePrivToken, callerCarolToken, callerSharedToken}
	privateReaders := map[string][]privateCaller{
		"acme/priv":    {callerBob, callerCarol, callerCarolPrivToken, callerBobPrivToken},
		"acme/pub":     openViewers,
		"acme/unowned": openViewers,
	}
	read("while PRIV is private", `200 {"installed":false}`, privateReaders)
	// The trace sees the repository rule's statement where the rule is read, so that none is read
	// below says something.
	if _, rule := answer(callerAlice, "acme/priv"); len(rule) != 1 {
		t.Fatalf("alice's read of acme/priv while PRIV is private read %d statements of the repository rule, want 1: the trace does not see the rule", len(rule))
	}

	// With no project private, every full member's caller reads every repository as before.
	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	for _, caller := range openViewers {
		for _, repository := range append(slices.Clone(installed), uninstalled) {
			want := `200 {"installed":true}`
			if repository == uninstalled {
				want = `200 {"installed":false}`
			}
			before := len(requests())
			got, rule := answer(caller, repository)
			if newly := requests()[before:]; got != want || !slices.Contains(newly, "GET /repos/"+repository+"/installation") || len(rule) != 0 {
				t.Errorf("%s with no project private reads whether the App is installed on %s as %s with GitHub asked %q and %d statements of the repository rule read, want %s from GitHub's lookup and none read",
					caller, repository, got, newly, len(rule), want)
			}
		}
	}

	seed.setVisibility(t, seedPrivate, model.ProjectPrivate)
	if err := ratelimit.New(seed.database.Pool).Record(context.Background(), githubapp.AppScope, time.Hour); err != nil {
		t.Fatalf("record a rate limit on the App's scope: %v", err)
	}
	// Under the limit a repository its caller may read is held unsent too, so every caller,
	// readers included, answers the held limit, and each is compared with acme/elsewhere's answer.
	read("under a met App rate limit", `502 {"code":"GITHUB_UNREADABLE","error":"read the GitHub App's installation on <repo>: GitHub rate limit on app`, nil)
}

// TestAppInstallationFollowsNoRedirectToAnotherRepositoryWhileAProjectIsPrivate: GitHub answers an
// old name of a renamed or transferred repository with a redirect to that repository as it is now,
// which the repository rule never judged. While a project is private the lookup is held to the
// path's repository (githubapp.HoldToRepository), as the GitHub proxy holds its reads: alice, an
// open viewer no issue's repository hides acme/moved from, reads acme/moved, whose installation
// lookup GitHub moves to other/priv's, as not installed. With no project private the lookup
// follows GitHub's redirect and answers other/priv's installation, as before any project could be
// private (R9).
//
// Control: the lookup not held to its repository; alice reads acme/moved as installed while PRIV
// is private, and the test fails.
func TestAppInstallationFollowsNoRedirectToAnotherRepositoryWhileAProjectIsPrivate(t *testing.T) {
	seed := seedPrivateProjectWith(t, testServerOptions{
		app: fakeGitHubAppConfig(t),
		githubAPIBase: githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/repos/acme/moved/installation":
				http.Redirect(w, r, "/repos/other/priv/installation", http.StatusMovedPermanently)
			case "/repos/other/priv/installation":
				fmt.Fprint(w, `{"id":77,"app_slug":"dispatch-test","permissions":{}}`)
			default:
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"message":"Not Found"}`)
			}
		}}),
	})
	read := func() string {
		response := seed.request(t, callerAlice, http.MethodGet, "/api/v1/github/app-installation/acme/moved", nil)
		return fmt.Sprintf("%d %s", response.Code, strings.TrimSpace(response.Body.String()))
	}
	if got := read(); got != `200 {"installed":false}` {
		t.Errorf("alice reads acme/moved, which GitHub redirects to other/priv, while PRIV is private as %s, want 200 not installed", got)
	}
	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	if got := read(); got != `200 {"installed":true}` {
		t.Errorf("alice reads acme/moved with no project private as %s, want 200 installed from GitHub's redirect, as before", got)
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
	t.Parallel()
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
	t.Parallel()
	handler, _, _ := newTestServer(t, testServerOptions{})
	response := agentRequest(t, handler, http.MethodGet, "/api/v1/github/app-installation/acme/widgets", nil, "agent-token")
	body := decodeBody[map[string]string](t, response)
	if response.Code != http.StatusServiceUnavailable || body["code"] != "GITHUB_APP_UNCONFIGURED" {
		t.Fatalf("no app key: status=%d body=%s", response.Code, response.Body.String())
	}
}

// GitHub failing the lookup is no answer either: 502 naming the repository and GitHub's error.
func TestAppInstallationGitHubFailureAnswers502(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
