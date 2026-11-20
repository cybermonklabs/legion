// results_other_repository_test.go holds POST /api/v1/issues/{key}/results to R17 of the spec of
// several open pull requests: Dispatch takes a result at a commit that the issue's own repository
// holds, or that the repository of an open or merged pull request of the issue holds, and refuses a
// commit only when each of those repositories answers that it holds no such commit. It holds GET
// to R8 and R18 for such a pull request too: its head is read in its own repository, and its basis
// names that repository. The test reads each answer as JSON into the types of this file, so it
// builds at a commit before the rule and fails there on its own assertion.
package api

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/githubtest"
)

// The commits of this test: J, the head of acme/infra#9; K, a commit acme/infra holds that no head
// names; U and W, commits neither repository holds; and H, the head of acme/app#70.
const (
	otherRepositoryJ = "9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a"
	otherRepositoryK = "9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b9b"
	otherRepositoryU = "9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c9c"
	otherRepositoryW = "9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d9d"
	otherRepositoryH = "7070707070707070707070707070707070707070"
)

// otherRepositoryVerification is k1, a check a role proves, and c1, a check CI proves through the
// job go-checks.
const otherRepositoryVerification = ":::check{#k1 surface=\"cli\" run=\"go test ./...\" negative_control=\"Break it and watch it fail.\"}\n" +
	"The server records a result against a check.\n" +
	":::\n\n" +
	":::check{#c1 negative_control=\"A build whose server does not build.\" ci_check=\"go-checks\"}\n" +
	"The server builds at the head.\n" +
	":::\n"

// otherRepositoryGitHub is GitHub for acme/app and acme/infra: the commits each repository holds,
// each repository that answers 503 to a read of a commit, and the check runs of each repository at
// each commit, with the count of the reads of each.
type otherRepositoryGitHub struct {
	mu          sync.Mutex
	held        map[string]bool
	down        map[string]bool
	runs        map[string][]map[string]any
	commitReads map[string]int
	runReads    map[string]int
}

func otherRepositoryKey(repo, sha string) string { return repo + "@" + sha }

func (g *otherRepositoryGitHub) hold(repo, sha string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.held[otherRepositoryKey(repo, sha)] = true
}

func (g *otherRepositoryGitHub) setDown(repo string, down bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.down[repo] = down
}

func (g *otherRepositoryGitHub) setRuns(repo, sha string, runs ...map[string]any) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.runs[otherRepositoryKey(repo, sha)] = runs
}

// reads is how many times GitHub was asked whether repo holds sha, and how many times for its
// check runs at sha.
func (g *otherRepositoryGitHub) reads(repo, sha string) (commits, runs int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	key := otherRepositoryKey(repo, sha)
	return g.commitReads[key], g.runReads[key]
}

// serve is a Dispatch whose GitHub App's GitHub is g, and the database it runs on.
func (g *otherRepositoryGitHub) serve(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate fixture key: %v", err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	readable := map[string]bool{"acme/app": true, "acme/infra": true}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, r *http.Request) {
		if !readable[r.PathValue("owner")+"/"+r.PathValue("repo")] {
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
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}", func(w http.ResponseWriter, r *http.Request) {
		repo, sha := r.PathValue("owner")+"/"+r.PathValue("repo"), r.PathValue("sha")
		g.mu.Lock()
		g.commitReads[otherRepositoryKey(repo, sha)]++
		down, held := g.down[repo], g.held[otherRepositoryKey(repo, sha)]
		g.mu.Unlock()
		switch {
		case down:
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"message":"Service Unavailable"}`)
		case held:
			fmt.Fprintf(w, `{"sha":%q}`, sha)
		default:
			w.WriteHeader(http.StatusUnprocessableEntity)
			fmt.Fprintf(w, `{"message":"No commit found for SHA: %s"}`, sha)
		}
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}/pulls", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[]`)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}/check-runs", func(w http.ResponseWriter, r *http.Request) {
		key := otherRepositoryKey(r.PathValue("owner")+"/"+r.PathValue("repo"), r.PathValue("sha"))
		g.mu.Lock()
		g.runReads[key]++
		runs, listed := g.runs[key]
		g.mu.Unlock()
		if !listed {
			w.WriteHeader(http.StatusUnprocessableEntity)
			fmt.Fprintf(w, `{"message":"No commit found for SHA: %s"}`, r.PathValue("sha"))
			return
		}
		if r.URL.Query().Get("page") != "" && r.URL.Query().Get("page") != "1" {
			runs = nil
		}
		body, _ := json.Marshal(map[string]any{"total_count": len(runs), "check_runs": runs})
		_, _ = w.Write(body)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/compare/{basehead}", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"status":"diverged"}`)
	})
	upstream := githubtest.Server(t, githubtest.Spec{Handle: mux.ServeHTTP})
	handler, database, _, _ := newRegisteredTestServer(t, testServerOptions{
		app:           &auth.AppConfig{ClientID: "Iv1.test", PEM: pemText},
		githubAPIBase: upstream,
	})
	return handler, database
}

// otherRepositoryBasis is one basis of a check as the results read answers it, with only the
// members this test reads.
type otherRepositoryBasis struct {
	Kind        string `json:"kind"`
	Commit      string `json:"commit"`
	PullRequest *int   `json:"pull_request"`
	Repo        string `json:"repo"`
}

// String names the basis as a failure message reads it.
func (b otherRepositoryBasis) String() string {
	number := "no pull request"
	if b.PullRequest != nil {
		number = "#" + strconv.Itoa(*b.PullRequest)
	}
	return fmt.Sprintf("%s %s at %s, repo %q", b.Kind, number, b.Commit, b.Repo)
}

// otherRepositoryCheck is one check of the results read, with only the members this test reads.
type otherRepositoryCheck struct {
	BlockID      string                 `json:"block_id"`
	ComparedWith []otherRepositoryBasis `json:"compared_with"`
	PassStands   struct {
		Subjects []struct {
			Basis  otherRepositoryBasis `json:"basis"`
			Answer string               `json:"answer"`
			Reason string               `json:"reason"`
		} `json:"subjects"`
	} `json:"pass_stands"`
}

// otherRepositoryRead is the check blockID of the results read of issue.
func otherRepositoryRead(t *testing.T, handler http.Handler, issue, blockID string) otherRepositoryCheck {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue+"/results", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the results of %s: status=%d body=%s", issue, response.Code, response.Body.String())
	}
	var read struct {
		Checks []otherRepositoryCheck `json:"checks"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &read); err != nil {
		t.Fatalf("decode the results of %s: %v", issue, err)
	}
	for _, check := range read.Checks {
		if check.BlockID == blockID {
			return check
		}
	}
	t.Fatalf("the read of %s holds no check %s: %s", issue, blockID, response.Body.String())
	return otherRepositoryCheck{}
}

// otherRepositoryRecord records a pass of k1 at commit and answers the status and the body.
func otherRepositoryRecord(t *testing.T, handler http.Handler, issue, commit string) (int, string) {
	t.Helper()
	response := recordResult(t, handler, issue, resultBody("k1", "pass", commit, nil))
	return response.Code, response.Body.String()
}

// TestAResultAtTheHeadOfAPullRequestInAnotherRepository: R17, with R8 and R18 at that head.
// ACME-1's repository is acme/app, and its open pull request #9 is in acme/infra, at J. A result at
// J is taken, and so is one at a commit that acme/infra holds and no head names; a commit neither
// repository holds is refused, naming both; and a 503 of acme/infra stores the result, since it is
// no proof. A check CI proves reads #9's run in acme/infra alone, and #9's basis names acme/infra.
func TestAResultAtTheHeadOfAPullRequestInAnotherRepository(t *testing.T) {
	github := &otherRepositoryGitHub{
		held: map[string]bool{}, down: map[string]bool{}, runs: map[string][]map[string]any{},
		commitReads: map[string]int{}, runReads: map[string]int{},
	}
	github.hold("acme/infra", otherRepositoryJ)
	github.hold("acme/infra", otherRepositoryK)
	github.hold("acme/app", otherRepositoryH)
	handler, database := github.serve(t)
	issue := issueInRepository(t, handler, "ACME", "acme/app", "A pull request in another repository")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", otherRepositoryVerification)
	writeHead(t, database, "acme/infra", 9, otherRepositoryJ, "open", issue.Key)

	t.Run("a result at J is taken", func(t *testing.T) {
		if status, body := otherRepositoryRecord(t, handler, issue.Key, otherRepositoryJ); status != http.StatusCreated {
			t.Fatalf("a result at J, the head of acme/infra#9: status=%d body=%s; want 201", status, body)
		}
		if app, _ := github.reads("acme/app", otherRepositoryJ); app != 0 {
			t.Fatalf("GitHub was asked %d times whether acme/app holds J, want never: J is the head of a pull request of %s", app, issue.Key)
		}
	})

	t.Run("a result at a commit acme/infra holds is taken", func(t *testing.T) {
		if status, body := otherRepositoryRecord(t, handler, issue.Key, otherRepositoryK); status != http.StatusCreated {
			t.Fatalf("a result at K, which acme/infra holds and no head names: status=%d body=%s; want 201", status, body)
		}
	})

	t.Run("a commit neither repository holds is refused", func(t *testing.T) {
		response := recordResult(t, handler, issue.Key, resultBody("k1", "pass", otherRepositoryU, nil))
		code, text := refusalOf(t, response)
		if response.Code != http.StatusBadRequest || code != "RESULT_COMMIT_UNKNOWN" {
			t.Fatalf("a result at U, which neither repository holds: status=%d body=%s; want 400 RESULT_COMMIT_UNKNOWN",
				response.Code, response.Body.String())
		}
		for _, named := range []string{"acme/app", "acme/infra", otherRepositoryU} {
			if !strings.Contains(text, named) {
				t.Fatalf("the refusal reads %q, want %s named", text, named)
			}
		}
	})

	t.Run("a 503 of acme/infra stores the result", func(t *testing.T) {
		github.setDown("acme/infra", true)
		defer github.setDown("acme/infra", false)
		if status, body := otherRepositoryRecord(t, handler, issue.Key, otherRepositoryW); status != http.StatusCreated {
			t.Fatalf("a result at W while acme/infra answers 503: status=%d body=%s; want 201, since a 503 proves nothing", status, body)
		}
	})

	t.Run("a check that CI proves reads the run in acme/infra", func(t *testing.T) {
		github.setRuns("acme/infra", otherRepositoryJ, completedRun(91, "go-checks", "success"))
		check := otherRepositoryRead(t, handler, issue.Key, "c1")
		var answered string
		for _, subject := range check.PassStands.Subjects {
			if subject.Basis.Commit == otherRepositoryJ {
				answered = subject.Answer
			}
		}
		if answered != "yes" {
			t.Fatalf("c1's subject at J reads %q, want yes from the run in acme/infra: %+v", answered, check.PassStands.Subjects)
		}
		if _, app := github.reads("acme/app", otherRepositoryJ); app != 0 {
			t.Fatalf("GitHub was asked %d times for the check runs of J in acme/app, want never", app)
		}
	})

	t.Run("the basis of #9 names acme/infra", func(t *testing.T) {
		writeHead(t, database, "acme/app", 70, otherRepositoryH, "open", issue.Key)
		bases := map[string]otherRepositoryBasis{}
		for _, basis := range otherRepositoryRead(t, handler, issue.Key, "k1").ComparedWith {
			bases[basis.Commit] = basis
		}
		if got := bases[otherRepositoryJ]; got.PullRequest == nil || *got.PullRequest != 9 || got.Repo != "acme/infra" {
			t.Fatalf("k1's basis at J = %v, want #9 of acme/infra", got)
		}
		if got := bases[otherRepositoryH]; got.PullRequest == nil || *got.PullRequest != 70 || got.Repo != "" {
			t.Fatalf("k1's basis at H = %v, want #70 with no repository: acme/app is the issue's own", got)
		}
	})
}
