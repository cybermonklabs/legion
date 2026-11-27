// as_of_results_test.go holds the read of an issue's results at a cut to what the equality
// harness (as_of_equality_test.go) cannot see: the GitHub reads and
// the writes of the present read, which the harness's server, holding no App key, never makes.
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
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/githubtest"
)

// pastResultsGitHub is the GitHub of TestPastTimeResultsWriteNothingAndAskGitHubNothing. The App
// is installed on acme/widgets, whose default branch is main and which holds every commit asked
// about. A commit's pull requests are none until the test names one (pulls); a compare answers
// that main carries the commit; and every commit's check runs hold one passing run of
// ciCheckRun. It counts every request, and while it is shut it fails the test on each one.
type pastResultsGitHub struct {
	t     *testing.T
	shut  atomic.Bool
	asked atomic.Int64
	mu    sync.Mutex
	pulls func(repo, sha string) string
}

func (g *pastResultsGitHub) serve(mux *http.ServeMux) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		g.asked.Add(1)
		if g.shut.Load() {
			g.t.Errorf("GitHub was asked %s %s while the test held it shut", r.Method, r.URL.Path)
		}
		mux.ServeHTTP(w, r)
	}
}

// newPastResultsRig is the package's test server on a traced store, holding an App key whose
// GitHub is a pastResultsGitHub.
func newPastResultsRig(t *testing.T, statements *markedStatements) (http.Handler, *pastResultsGitHub, *server) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate fixture key: %v", err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	github := &pastResultsGitHub{t: t}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, _ *http.Request) {
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
		fmt.Fprintf(w, `{"sha":%q}`, r.PathValue("sha"))
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}/pulls", func(w http.ResponseWriter, r *http.Request) {
		github.mu.Lock()
		answer := github.pulls
		github.mu.Unlock()
		if answer == nil {
			fmt.Fprint(w, `[]`)
			return
		}
		fmt.Fprint(w, answer(r.PathValue("owner")+"/"+r.PathValue("repo"), r.PathValue("sha")))
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}/commits", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[]`)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/compare/{basehead}", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"status":"behind"}`)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}/check-runs", func(w http.ResponseWriter, _ *http.Request) {
		body, _ := json.Marshal(map[string]any{"total_count": 1, "check_runs": []any{completedRun(81, ciCheckRun, "success")}})
		_, _ = w.Write(body)
	})
	handler, _, _, served := newRegisteredTestServer(t, testServerOptions{
		store:         storetest.TracedStore(t, storetest.Open(t), statements),
		app:           &auth.AppConfig{ClientID: "Iv1.test", PEM: pemText},
		githubAPIBase: githubtest.Server(t, githubtest.Spec{Handle: github.serve(mux)}),
	})
	return handler, github, served
}

// pastResultsChecks is the Verification document's two recorded checks: k1, which its pull request
// heads answer, and k2, which the default branch answers, so the present read compares k2's
// commit with the default branch and stores that branch's answer (branch_commits). An after-merge
// check is compared with a merged pull request alone, which this issue has none of.
const pastResultsChecks = ":::check{#k1 surface=\"cli\" run=\"go test ./...\" negative_control=\"Break it and watch it fail.\"}\n" +
	"The server records a result against a check.\n" +
	":::\n\n" +
	":::check{#k2 when=\"default_branch\"}\n" +
	"The default branch answers at the merged commit.\n" +
	":::\n"

// TestPastTimeResultsWriteNothingAndAskGitHubNothing: a read of an issue's results at a cut asks
// GitHub nothing and writes nothing (decision 11), though the same read at the present asks GitHub
// and writes on the same state. The issue's Verification document holds a check its pull request
// heads answer, a check its default branch answers and a check CI proves. A result of the first
// stands at the head of a pull request that no row holds, and a result of the second at a commit
// the default branch carries, which no row records. So the present read asks GitHub for the
// commit's pull requests, for a compare and for check runs, and stores a pull request head, a
// branch answer and the head's attribution event. The read at the cut is a writer's read, made
// with GitHub shut: it answers each check and result of the cut, leaves out every field of the
// comparison and of CI's run and names each in Dispatch-As-Of-Absent, and gives whether each
// check's words moved. The tracer finds no write of it to pull_request_heads, branch_commits or
// events, and the counts of those rows do not move. ?at= with a cut is refused 400
// AS_OF_NOT_SERVED naming at, and asks GitHub nothing either.
func TestPastTimeResultsWriteNothingAndAskGitHubNothing(t *testing.T) {
	statements := &markedStatements{}
	handler, github, served := newPastResultsRig(t, statements)
	database := served.deps.Store
	issue := issueInRepository(t, handler, "RPST", "acme/widgets", "Results read at a cut")
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", pastResultsChecks+"\n"+ciCheckBlock("c1", ciCheckKept))
	for _, result := range []struct{ check, commit string }{{"k1", newHeadCommit}, {"k2", mergeCommit}} {
		if recorded := recordResult(t, handler, issue.Key, resultBody(result.check, "pass", result.commit, nil)); recorded.Code != http.StatusCreated {
			t.Fatalf("record a result of %s: status=%d body=%s", result.check, recorded.Code, recorded.Body.String())
		}
	}
	writeHead(t, database, "acme/widgets", 7, headCommit, "open", issue.Key)
	// The pull request opens at the first result's commit after the record, and nothing tells
	// Dispatch: no row heads the commit, and the present read asks GitHub.
	github.mu.Lock()
	github.pulls = openPullAt(91, issue.Key)
	github.mu.Unlock()
	awaitSettledDocuments(t, database)
	awaitBackgroundWrites(t, served)
	safeHorizon(t, served)
	cut := databaseCut(t, database)
	time.Sleep(5 * time.Millisecond)

	results := "/api/v1/issues/" + issue.Key + "/results"
	read := func(target string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequestWithContext(context.WithValue(t.Context(), countedRead{}, true), http.MethodGet, target, nil)
		request.Header.Set("X-Dispatch-User", "alice")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	before := countStoredRows(t, database)
	statements.take()
	github.shut.Store(true)
	asked := github.asked.Load()

	pinned := read(withAsOf(t, results+"?at="+headCommit, cut))
	var refusal refusalAnswer
	_ = json.Unmarshal(pinned.Body.Bytes(), &refusal)
	if pinned.Code != http.StatusBadRequest || refusal.Code != contracts.DispatchAsOfNotServed || !strings.HasPrefix(refusal.Error, "at:") {
		t.Errorf("a read at the cut that names a commit: status=%d body=%s, want 400 %s naming at", pinned.Code, pinned.Body.String(), contracts.DispatchAsOfNotServed)
	}

	response := read(withAsOf(t, results, cut))
	if response.Code != http.StatusOK || response.Header().Get(contracts.DispatchAsOfHeader) != cut.String() {
		t.Fatalf("the read at the cut: status=%d %s=%q body=%s", response.Code, contracts.DispatchAsOfHeader,
			response.Header().Get(contracts.DispatchAsOfHeader), response.Body.String())
	}
	const absent = "/checks/*/ci,/checks/*/compared_nothing,/checks/*/compared_with,/checks/*/pass_stands," +
		"/checks/*/results/*/commit_stands,/checks/*/results/*/currency"
	if named := response.Header().Get(contracts.DispatchAsOfAbsentHeader); named != absent {
		t.Errorf("the read at the cut names %s %q, want %q", contracts.DispatchAsOfAbsentHeader, named, absent)
	}
	var answer struct {
		Checks []map[string]any `json:"checks"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the read at the cut: %v", err)
	}
	if len(answer.Checks) != 3 {
		t.Fatalf("the read at the cut holds %d checks, want k1, k2 and c1: %s", len(answer.Checks), response.Body.String())
	}
	for _, check := range answer.Checks {
		for _, member := range []string{"compared_with", "compared_nothing", "pass_stands", "ci"} {
			if value, given := check[member]; given {
				t.Errorf("check %v gives %s %v at the cut, want it left out", check["block_id"], member, value)
			}
		}
		if check["version"] == nil || check["sha256"] == "" {
			t.Errorf("check %v gives version %v and sha256 %v at the cut, want the version and digest of its words then", check["block_id"], check["version"], check["sha256"])
		}
		held, _ := check["results"].([]any)
		if want := map[bool]int{true: 0, false: 1}[check["block_id"] == "c1"]; len(held) != want {
			t.Errorf("check %v holds the results %v at the cut, want %d: the read at the cut derives no result from CI's run", check["block_id"], held, want)
		}
		for _, element := range held {
			result, _ := element.(map[string]any)
			for _, member := range []string{"commit_stands", "currency"} {
				if value, given := result[member]; given {
					t.Errorf("a result of check %v gives %s %v at the cut, want it left out", check["block_id"], member, value)
				}
			}
			if moved, given := result["check_words_moved"]; !given || moved != false {
				t.Errorf("a result of check %v gives check_words_moved %v at the cut, want false: its words did not move", check["block_id"], moved)
			}
		}
	}
	readResults := false
	for _, statement := range statements.take() {
		readResults = readResults || strings.Contains(statement, "from check_results")
		writes, _ := writesOf(statement)
		for _, write := range writes {
			if _, table, _ := strings.Cut(write, " "); table == "pull_request_heads" || table == "branch_commits" || table == "events" {
				t.Errorf("the read at the cut wrote %s: %s", table, abbreviateAnswer([]byte(statement)))
			}
		}
	}
	if !readResults {
		t.Error("the tracer saw no read of check_results: it did not see the read at the cut")
	}
	if stored := countStoredRows(t, database); stored != before {
		t.Errorf("the read at the cut moved the stored rows from %+v to %+v, want none moved", before, stored)
	}
	if more := github.asked.Load() - asked; more != 0 {
		t.Errorf("the read at the cut asked GitHub %d times, want none", more)
	}

	// The control: the present read on the same state asks GitHub and stores what it learns.
	github.shut.Store(false)
	if present := read(results); present.Code != http.StatusOK {
		t.Fatalf("the present read: status=%d body=%s", present.Code, present.Body.String())
	}
	stored := countStoredRows(t, database)
	if github.asked.Load() == asked || stored.heads <= before.heads || stored.branches <= before.branches || stored.events <= before.events {
		t.Errorf("the present read asked GitHub %d times and moved the stored rows from %+v to %+v, want GitHub asked and a head, a branch answer and an event stored",
			github.asked.Load()-asked, before, stored)
	}
}
