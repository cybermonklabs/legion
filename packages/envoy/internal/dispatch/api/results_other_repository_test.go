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
	mux.HandleFunc("POST /graphql", commitPullsThroughMux(t, mux))
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
	t.Parallel()
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

// TestAFailedReadOfThePullRequestHeadsRefusesTheRecord: R17, where Dispatch cannot read its own
// rows of the issue's pull requests. The issue's repository is acme/app, and its open pull request
// #9 is in acme/infra, at J, which acme/app does not hold. The table of the heads is renamed out of
// the read's reach, so the record can tell neither that J is #9's head nor that acme/infra is a
// repository to ask; the issue's links stay readable, and name nothing. A result at J, which GitHub
// proves acme/app does not hold, is refused 500 INTERNAL and stores nothing, as a failed read of
// the links is: a 400 RESULT_COMMIT_UNKNOWN there would name acme/app alone, though acme/infra
// holds J, and a stored result would be taken to be acme/app's. A result at H, which GitHub says
// acme/app holds, is stored, since no other repository is asked about a commit the issue's own
// repository holds.
func TestAFailedReadOfThePullRequestHeadsRefusesTheRecord(t *testing.T) {
	t.Parallel()
	github := &otherRepositoryGitHub{
		held: map[string]bool{}, down: map[string]bool{}, runs: map[string][]map[string]any{},
		commitReads: map[string]int{}, runReads: map[string]int{},
	}
	github.hold("acme/infra", otherRepositoryJ)
	github.hold("acme/app", otherRepositoryH)
	handler, database := github.serve(t)
	issue := issueInRepository(t, handler, "ACME", "acme/app", "Pull request heads Dispatch cannot read")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", otherRepositoryVerification)
	writeHead(t, database, "acme/infra", 9, otherRepositoryJ, "open", issue.Key)
	if _, err := database.Pool.Exec(t.Context(),
		`alter table pull_request_heads rename to pull_request_heads_unreadable`); err != nil {
		t.Fatalf("make the pull request heads unreadable: %v", err)
	}
	eventsBefore := resultEventsOn(t, database, issue.Key)

	refused := recordResult(t, handler, issue.Key, resultBody("k1", "pass", otherRepositoryJ, nil))

	if code, _ := refusalOf(t, refused); refused.Code != http.StatusInternalServerError || code != "INTERNAL" {
		t.Fatalf("a result at J, the head of acme/infra#9, with the heads unreadable: status=%d body=%s; want 500 INTERNAL",
			refused.Code, refused.Body.String())
	}
	if stored := resultEventsOn(t, database, issue.Key) - eventsBefore; stored != 0 {
		t.Errorf("the refused result appended %d result.recorded events, want none", stored)
	}
	if rows := resultRowsOn(t, database, issue.Key); rows != 0 {
		t.Errorf("the refused result left %d check_results rows, want none", rows)
	}
	if status, body := otherRepositoryRecord(t, handler, issue.Key, otherRepositoryH); status != http.StatusCreated {
		t.Fatalf("a result at H, which acme/app holds, with the heads unreadable: status=%d body=%s; want 201", status, body)
	}
}

// otherRepositoryS is a commit both acme/app and acme/infra hold, as two repositories that share
// history do.
const otherRepositoryS = "9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e9e"

// otherRepositoryStanding is where each result of a check stands, by result id, and the state of
// each subject, by its commit, as the results read answers them.
type otherRepositoryStanding struct {
	results  map[string]otherRepositoryCommitStands
	subjects map[string]string
}

// otherRepositoryCommitStands is one result's commit_stands, with only the members this test reads.
type otherRepositoryCommitStands struct {
	Answer string                `json:"answer"`
	Basis  *otherRepositoryBasis `json:"basis"`
}

// otherRepositoryStandingOf is where each result of k1 of issue stands, and each subject's answer.
func otherRepositoryStandingOf(t *testing.T, handler http.Handler, issue string) otherRepositoryStanding {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue+"/results", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the results of %s: status=%d body=%s", issue, response.Code, response.Body.String())
	}
	var read struct {
		Checks []struct {
			BlockID string `json:"block_id"`
			Results []struct {
				ID           string                      `json:"id"`
				CommitStands otherRepositoryCommitStands `json:"commit_stands"`
			} `json:"results"`
			PassStands struct {
				Subjects []struct {
					Basis  otherRepositoryBasis `json:"basis"`
					Answer string               `json:"answer"`
				} `json:"subjects"`
			} `json:"pass_stands"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &read); err != nil {
		t.Fatalf("decode the results of %s: %v", issue, err)
	}
	standing := otherRepositoryStanding{results: map[string]otherRepositoryCommitStands{}, subjects: map[string]string{}}
	for _, check := range read.Checks {
		if check.BlockID != "k1" {
			continue
		}
		for _, result := range check.Results {
			standing.results[result.ID] = result.CommitStands
		}
		for _, subject := range check.PassStands.Subjects {
			standing.subjects[subject.Basis.Commit] = subject.Answer
		}
	}
	return standing
}

// TestAResultStandsOnlyAtAHeadOfItsOwnRepository: a `before_merge` result stands only at a head of
// the repository its commit is taken to be of. ACME-1's repository is acme/app, and acme/app and
// acme/infra both hold S. A pass of k1 recorded at S while no head names S is taken to be
// acme/app's, since acme/app holds it. Then acme/infra#9 opens at S. That pass is no pass at #9's
// head, a head of another repository: it reads other_commit, and #9 reads no. Compared by its
// commit alone it stood at #9's head and read #9 yes. The control in the same test: a pass recorded
// at S once #9's head is S is taken to be acme/infra's, stands at #9's head, and #9 reads yes.
func TestAResultStandsOnlyAtAHeadOfItsOwnRepository(t *testing.T) {
	t.Parallel()
	github := &otherRepositoryGitHub{
		held: map[string]bool{}, down: map[string]bool{}, runs: map[string][]map[string]any{},
		commitReads: map[string]int{}, runReads: map[string]int{},
	}
	github.hold("acme/app", otherRepositoryS)
	github.hold("acme/infra", otherRepositoryS)
	handler, database := github.serve(t)
	issue := issueInRepository(t, handler, "ACME", "acme/app", "A commit two repositories hold")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", otherRepositoryVerification)
	own := recordResult(t, handler, issue.Key, resultBody("k1", "pass", otherRepositoryS, map[string]any{"role": "tester"}))
	if own.Code != http.StatusCreated {
		t.Fatalf("the tester's pass of k1 at S: status=%d body=%s", own.Code, own.Body.String())
	}
	ownID := decodeBody[recordedResult](t, own).Result.ID
	writeHead(t, database, "acme/infra", 9, otherRepositoryS, "open", issue.Key)

	standing := otherRepositoryStandingOf(t, handler, issue.Key)
	if got := standing.results[ownID]; got.Answer != "other_commit" {
		t.Fatalf("the pass of acme/app at S stands %+v, want other_commit: S is the head of acme/infra#9, another repository's pull request", got)
	}
	if got := standing.subjects[otherRepositoryS]; got != "no" {
		t.Fatalf("#9's head reads %q, want no: the one pass at its commit is of acme/app", got)
	}

	linked := recordResult(t, handler, issue.Key, resultBody("k1", "pass", otherRepositoryS, map[string]any{"role": "tester"}))
	if linked.Code != http.StatusCreated {
		t.Fatalf("the tester's pass of k1 at #9's head: status=%d body=%s", linked.Code, linked.Body.String())
	}
	linkedID := decodeBody[recordedResult](t, linked).Result.ID
	standing = otherRepositoryStandingOf(t, handler, issue.Key)
	if got := standing.results[linkedID]; got.Answer != "at_basis" || got.Basis == nil || got.Basis.PullRequest == nil ||
		*got.Basis.PullRequest != 9 || got.Basis.Repo != "acme/infra" {
		t.Fatalf("the pass at #9's head stands %+v, want at_basis at acme/infra#9", got)
	}
	if got := standing.subjects[otherRepositoryS]; got != "yes" {
		t.Fatalf("#9's head reads %q after the pass at its head, want yes", got)
	}
}
