// results_ci_only_test.go holds GET /api/v1/issues/{key}/results for an issue whose one check CI
// proves and nobody records a result of, in a deployment with no delivery settings: production's
// shape on 22 November, where such an issue read `no_head` until an unrelated read wrote its pull
// request's head row. A result recorded at a commit makes the record route and the results read
// ask GitHub which pull requests hold that commit; a check only CI proves holds no result, so
// neither asks, and the head row comes from the pull request's own writers alone: the intake, from
// GitHub's `pull_request` event, and the reconcile, from the read a link by hand owes. The file
// builds its own stand-in for GitHub and uses helpers main holds, so it runs at main too.
package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/githubtest"
)

// ciOnlyBound is how long a case waits for the results read to compare its check with the pull
// request's head once the writer's input is in: the start pass of the reconcile, or the intake's
// handling of one envelope, each a read of GitHub on a test machine.
const ciOnlyBound = 60 * time.Second

// ciOnlyWorld is a Dispatch with no delivery settings whose GitHub serves acme/tools: the App is
// installed there, GitHub names no pull request for any commit (so no read-through can write the
// head row), it answers pull request #12 as open at headCommit with the branch and the body pull
// gives, and it lists CI's run ciCheckRun as passed at headCommit. The issue's one check is c1,
// which CI proves, and no result of it is recorded.
type ciOnlyWorld struct {
	t        *testing.T
	handler  http.Handler
	database *store.Store
	served   *server
	issue    string
}

func newCIOnlyWorld(t *testing.T, project string, pull func(issue string) (branch, body string)) ciOnlyWorld {
	t.Helper()
	runs := newCheckRunsFake()
	runs.set(headCommit, completedRun(31, ciCheckRun, "success"))
	var issueKey string
	handler, database, served := oneRepositoryGitHub(t, "acme/tools", runs, func(number string) string {
		if number != "12" {
			return ""
		}
		branch, body := pull(issueKey)
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Errorf("encode the body of #12: %v", err)
		}
		return fmt.Sprintf(`{"number":12,"title":"A change","body":%s,"html_url":"https://github.com/acme/tools/pull/12",
			"state":"open","merged":false,"merged_at":null,"updated_at":"2026-11-22T06:31:03Z",
			"head":{"ref":%q,"sha":%q,"repo":{"full_name":"acme/tools"}},"base":{"ref":"main"}}`,
			encoded, branch, headCommit)
	})
	issue := issueInRepository(t, handler, project, "acme/tools", "CI proves it")
	issueKey = issue.Key
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md",
		":::check{#c1 "+ciCheckKept+"}\nThe tests pass at the head.\n:::\n")
	if _, err := delivery.GetSettings(context.Background(), database.Pool); err == nil {
		t.Fatal("the deployment holds delivery settings, want none: production held none")
	}
	return ciOnlyWorld{t: t, handler: handler, database: database, served: served, issue: issue.Key}
}

// oneRepositoryGitHub is a Dispatch with no delivery settings whose GitHub serves repo alone: the
// App is installed there and nowhere else, GitHub names no pull request for any commit (so no
// read-through writes a head row), each pull request's commits are an empty page, runs lists each
// commit's check runs, and pull answers GET /repos/{owner}/{repo}/pulls/{number} of repo, ""
// being GitHub's 404. A case that reads a check CI proves and a pull request the intake or the
// reconcile reads needs both: readThroughGitHub serves no check runs, and resultsGitHubCheckRuns
// no read of a pull request and no server for the intakes.
func oneRepositoryGitHub(t *testing.T, repo string, runs *checkRunsFake, pull func(number string) string) (http.Handler, *store.Store, *server) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate fixture key: %v", err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("owner")+"/"+r.PathValue("repo") != repo {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return
		}
		fmt.Fprint(w, `{"id":77,"app_slug":"dispatch-test","permissions":{"contents":"read","pull_requests":"read","checks":"read"}}`)
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"ghs_fake_77","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"full_name":%q,"default_branch":"main"}`, r.PathValue("owner")+"/"+r.PathValue("repo"))
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}/pulls", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[]`)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		body := ""
		if r.PathValue("owner")+"/"+r.PathValue("repo") == repo {
			body = pull(r.PathValue("number"))
		}
		if body == "" {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return
		}
		fmt.Fprint(w, body)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}/commits", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[]`)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}/check-runs", runs.serve)
	mux.HandleFunc("POST /graphql", commitPullsThroughMux(t, mux))
	upstream := githubtest.Server(t, githubtest.Spec{Handle: mux.ServeHTTP})
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{
		app:           &auth.AppConfig{ClientID: "Iv1.test", PEM: pemText},
		githubAPIBase: upstream,
	})
	return handler, database, served
}

// ci is c1's CI reading: its case, its state and its line.
func (w ciOnlyWorld) ci() string {
	w.t.Helper()
	check := checkOf(w.t, readResults(w.t, w.handler, w.issue), "c1")
	if check.CI == nil {
		return "no ci reading"
	}
	return fmt.Sprintf("case %q, state %q, line %q", check.CI.Case, check.CI.State, check.CI.Line)
}

// requireNoHead fails the test unless c1 reads no_head: before its writer's input, nothing gave
// Dispatch the pull request.
func (w ciOnlyWorld) requireNoHead(before string) {
	w.t.Helper()
	if check := checkOf(w.t, readResults(w.t, w.handler, w.issue), "c1"); check.CI == nil || check.CI.Case != "no_head" {
		w.t.Fatalf("before %s, c1 reads %s; want no_head, since nothing has written acme/tools#12's head", before, w.ci())
	}
}

// requireTheHead fails the test unless, within ciOnlyBound of since, c1 reads CI's pass at
// headCommit, the head of acme/tools#12.
func (w ciOnlyWorld) requireTheHead(since time.Time, after string) {
	w.t.Helper()
	for {
		check := checkOf(w.t, readResults(w.t, w.handler, w.issue), "c1")
		if check.CI != nil && check.CI.Case != "no_head" && check.CI.State == "pass" && strings.Contains(check.CI.Line, headCommit) {
			w.t.Logf("after %s, c1 reads %s, %s on", after, w.ci(), time.Since(since).Round(time.Millisecond))
			return
		}
		if time.Since(since) > ciOnlyBound {
			w.t.Fatalf("%s after %s, c1 reads %s; want CI's pass at %s, the head of acme/tools#12",
				ciOnlyBound, after, w.ci(), headCommit)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// An issue whose one before_merge check CI proves, and which holds no recorded result, compares
// that check with the head of its pull request in a deployment with no delivery settings, whether
// the pull request reached Dispatch by a link by hand or by GitHub's `pull_request` event. Before
// either, the check reads no_head (each case's control): no read-through runs for it, since no
// result names a commit. At a build whose link write owes no read and whose reconcile refreshes
// only rows that exist, the linked case stays no_head; at a build whose intake binds only under
// delivery settings, the event case does.
func TestACheckOnlyCIProvesComparesWithTheHeadOfItsPullRequestWithNoResultRecorded(t *testing.T) {
	t.Parallel()
	t.Run("a link by hand", func(t *testing.T) {
		world := newCIOnlyWorld(t, "CIOL", func(string) (string, string) { return "feature/no-key", "" })
		world.requireNoHead("the link")
		if response := dispatchRequest(t, world.handler, http.MethodPatch, "/api/v1/issues/"+world.issue,
			map[string]any{"external_links": []map[string]string{{"url": "https://github.com/acme/tools/pull/12"}}}, "alice"); response.Code != http.StatusOK {
			t.Fatalf("link acme/tools#12 to %s: status=%d body=%s", world.issue, response.Code, response.Body.String())
		}
		callers, err := world.database.Pool.Leases()
		if err != nil {
			t.Fatalf("open the lease pool: %v", err)
		}
		leases, err := callers.For("delivery reconcile")
		if err != nil {
			t.Fatalf("reserve the reconcile's caller of the lease pool: %v", err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		since := time.Now()
		go func() {
			defer close(done)
			delivery.NewReconcile(world.database.Pool, world.served.deps.GitHub, world.served.deps.Events, world.served.deps.Attributor).
				FirstPassAtStart().Run(ctx, leases)
		}()
		t.Cleanup(func() {
			cancel()
			<-done
		})
		world.requireTheHead(since, "the link and the reconcile's start pass")
	})
	t.Run("GitHub's pull_request event", func(t *testing.T) {
		world := newCIOnlyWorld(t, "CIOE", func(issue string) (string, string) {
			return "legion/" + issue, "Dispatch: " + issue + "\n\nThe change."
		})
		world.requireNoHead("the event")
		publisher := startTheIntakes(t, world.database, world.served)
		payload, err := json.Marshal(map[string]string{
			"kind": "pr", "action": "opened", "repo": "acme/tools", "number": "12",
			"title": "A change", "body": "Dispatch: " + world.issue + "\n\nThe change.",
			"url": "https://github.com/acme/tools/pull/12", "author": "implementer[bot]",
			"head_sha": headCommit, "head_ref": "legion/" + world.issue, "head_repo": "acme/tools",
			"base_ref": "main", "merged": "false", "updated_at": "2026-11-22T06:31:03Z",
		})
		if err != nil {
			t.Fatalf("encode the pull request's payload: %v", err)
		}
		since := time.Now()
		if err := publisher.Publish(contracts.Envelope{
			EventID: "acme-tools-pr-12-opened", Source: "github", SourceEventID: "acme-tools-pr-12-opened",
			TraceID: "acme-tools-pr-12-opened", Topic: contracts.GithubResourceSubject("acme", "tools", "pr", "12"),
			Payload: string(payload), IssuedAt: time.Now().Unix(),
		}); err != nil {
			t.Fatalf("publish the pull request's opening: %v", err)
		}
		world.requireTheHead(since, "GitHub's pull_request event")
	})
}
