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
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/asof"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
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
// comparison and of CI's run, each check's version and digest and whether the words of each
// result moved, and names each in Dispatch-As-Of-Absent. The tracer finds no write of it to
// pull_request_heads, branch_commits or events, and the counts of those rows do not move. ?at=
// with a cut is refused 400 AS_OF_NOT_SERVED naming at, and asks GitHub nothing either.
func TestPastTimeResultsWriteNothingAndAskGitHubNothing(t *testing.T) {
	t.Parallel()
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
	cut := settledResultsCut(t, database, served)

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
		"/checks/*/results/*/check_words_moved,/checks/*/results/*/commit_stands,/checks/*/results/*/currency," +
		"/checks/*/sha256,/checks/*/version"
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
		for _, member := range []string{"compared_with", "compared_nothing", "pass_stands", "ci", "version", "sha256"} {
			if value, given := check[member]; given {
				t.Errorf("check %v gives %s %v at the cut, want it left out", check["block_id"], member, value)
			}
		}
		held, _ := check["results"].([]any)
		if want := map[bool]int{true: 0, false: 1}[check["block_id"] == "c1"]; len(held) != want {
			t.Errorf("check %v holds the results %v at the cut, want %d: the read at the cut derives no result from CI's run", check["block_id"], held, want)
		}
		for _, element := range held {
			result, _ := element.(map[string]any)
			for _, member := range []string{"commit_stands", "currency", "check_words_moved"} {
				if value, given := result[member]; given {
					t.Errorf("a result of check %v gives %s %v at the cut, want it left out", check["block_id"], member, value)
				}
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

// TestARespelledChecksWordsAreLeftOutAtACut: a check whose text another renderer version
// stored keeps its changed version when the next version writes it in this build's spelling, and
// its digest moves (refs.BlockIndexing.KeepRespelled). A result recorded against the check before
// the respelling and one recorded after it each read check_words_moved false at the present: the
// words moved only where the digest and the version both differ from the result's
// (comparisonReader.answer). A version that rewords the check moves both, and each result reads
// check_words_moved true. A read of the results at a cut after the respelling, and at one after
// the rewording, leaves out the check's version and sha256 and whether the words of each result
// moved, and names each in Dispatch-As-Of-Absent, though the present read gave them then: the
// block index can be written after a version is saved, and no record gives the time of that
// write. So does the read at the cut after the respelling once the block index holds the
// rewording's version.
func TestARespelledChecksWordsAreLeftOutAtACut(t *testing.T) {
	t.Parallel()
	handler, database, served := resultsGitHubServed(t, map[string]bool{"acme/widgets": true}, nil, "main",
		func(string, string) (int, string) { return http.StatusOK, "diverged" }, nil)
	issue := issueInRepository(t, handler, "RSPC", "acme/widgets", "Respelled at a cut")
	owner := "/api/v1/issues/" + issue.Key
	uploaded := uploadDocument(t, handler, owner, "verification.md", respelledCheck)
	writeHead(t, database, "acme/widgets", 65, headCommit, "open", issue.Key)
	respellVerification(t, database, uploaded.Artifact.ID, uploaded.Version.Number)
	record := func(what string) model.CheckResult {
		t.Helper()
		recorded := recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil))
		if recorded.Code != http.StatusCreated {
			t.Fatalf("record a pass of k1 %s: status=%d body=%s", what, recorded.Code, recorded.Body.String())
		}
		return decodeBody[recordedResult](t, recorded).Result
	}
	earlier := record("against the other renderer's spelling")
	uploadDocument(t, handler, owner, "verification.md",
		strings.Replace(respelledCheck, "A sibling check nobody reworded.", "A sibling check, reworded.", 1))
	later := record("against this build's spelling")
	if earlier.CheckVersion != uploaded.Version.Number || later.CheckVersion != uploaded.Version.Number ||
		earlier.CheckSHA256 != refs.BlockTextSum(respelledCheckBlock) || later.CheckSHA256 == earlier.CheckSHA256 {
		t.Fatalf("the passes recorded version %d digest %s and version %d digest %s, want version %d for both, the first under the digest of %q and the second under another",
			earlier.CheckVersion, earlier.CheckSHA256, later.CheckVersion, later.CheckSHA256, uploaded.Version.Number, respelledCheckBlock)
	}

	results := owner + "/results"
	// wordsMoved is check_words_moved of each result of k1 in read, by the result's id: "absent"
	// where the read leaves it out.
	wordsMoved := func(t *testing.T, read issueResults) map[string]string {
		t.Helper()
		moved := map[string]string{}
		for _, result := range checkOf(t, read, "k1").Results {
			moved[result.ID] = "absent"
			if result.CheckWordsMoved != nil {
				moved[result.ID] = strconv.FormatBool(*result.CheckWordsMoved)
			}
		}
		return moved
	}
	both := func(moved string) map[string]string {
		return map[string]string{earlier.ID: moved, later.ID: moved}
	}
	takeCut := func() asof.Cut { return settledResultsCut(t, database, served) }
	// requireLeftOut fails unless the read of the results at cut holds check k1 with both passes,
	// leaves out its version and its sha256 and whether the words of each pass moved, and names
	// each in Dispatch-As-Of-Absent.
	requireLeftOut := func(t *testing.T, when string, cut asof.Cut) {
		t.Helper()
		checks, absent := readChecksAtTheCut(t, handler, results, cut)
		k1 := checks["k1"]
		passes := map[string]bool{}
		for _, result := range k1.results {
			id, _ := result["id"].(string)
			passes[id] = true
		}
		if !maps.Equal(passes, map[string]bool{earlier.ID: true, later.ID: true}) {
			t.Fatalf("%s, the read at the cut holds no check k1 with both passes: %v", when, checks)
		}
		requireTheDigestLeftOut(t, when, k1, absent)
	}

	if then := wordsMoved(t, readResults(t, handler, issue.Key)); !maps.Equal(then, both("false")) {
		t.Fatalf("the present read after the respelling answers check_words_moved %v, want false for both passes", then)
	}
	respelled := takeCut()
	requireLeftOut(t, "at a cut after the respelling", respelled)

	uploadDocument(t, handler, owner, "verification.md",
		strings.Replace(respelledCheck, respelledCheckParagraph, "The server records a result against a check, reworded.", 1))
	if now := wordsMoved(t, readResults(t, handler, issue.Key)); !maps.Equal(now, both("true")) {
		t.Fatalf("the present read after the rewording answers check_words_moved %v, want true for both passes", now)
	}
	reworded := takeCut()
	requireLeftOut(t, "at a cut after the rewording", reworded)
	// The block index holds the rewording's version now.
	requireLeftOut(t, "at the cut after the respelling, read after the rewording", respelled)
}

// TestAChecksDigestThatAReindexChangedInPlaceIsLeftOutAtACut: a re-index of a stored version in
// place, the block index's reconciliation of a document owed an index (docs' indexOwedDocument),
// indexes the live text at that version and writes no version. Where it writes in this build's
// spelling a check whose text another renderer version recorded, it keeps the check's changed
// version, records no text and moves the check's digest (refs.BlockIndexing.KeepRespelled), and
// no record gives the time. A read of the results at a cut taken before it leaves out the version
// and the sha256 of both checks and whether the words of the pass moved, and names each in
// Dispatch-As-Of-Absent, whatever the block index holds: the version at the cut under the digest
// the read then gave, no version while the document is owed its index, the digest the re-index
// moved, and a later version.
func TestAChecksDigestThatAReindexChangedInPlaceIsLeftOutAtACut(t *testing.T) {
	t.Parallel()
	handler, database, served := resultsGitHubServed(t, map[string]bool{"acme/widgets": true}, nil, "main",
		func(string, string) (int, string) { return http.StatusOK, "diverged" }, nil)
	issue := issueInRepository(t, handler, "RIDX", "acme/widgets", "Re-indexed in place")
	owner := "/api/v1/issues/" + issue.Key
	uploaded := uploadDocument(t, handler, owner, "verification.md", respelledCheck)
	document, number := uploaded.Artifact.ID, uploaded.Version.Number
	writeHead(t, database, "acme/widgets", 65, headCommit, "open", issue.Key)
	respellVerification(t, database, document, number)
	recorded := recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil))
	if recorded.Code != http.StatusCreated {
		t.Fatalf("record a pass of k1: status=%d body=%s", recorded.Code, recorded.Body.String())
	}
	pass := decodeBody[recordedResult](t, recorded).Result
	then := readResults(t, handler, issue.Key)
	k1, k3 := checkOf(t, then, "k1"), checkOf(t, then, "k3")
	if k1.Version != number || k1.SHA256 != refs.BlockTextSum(respelledCheckBlock) || pass.CheckSHA256 != k1.SHA256 {
		t.Fatalf("before the cut check k1 reads version %d and sha256 %s and its pass sha256 %s, want version %d and the digest of %q",
			k1.Version, k1.SHA256, pass.CheckSHA256, number, respelledCheckBlock)
	}
	cut := settledResultsCut(t, database, served)

	// requireLeftOut fails unless the read at the cut holds check k3 and check k1 with its pass,
	// leaves out the version and the sha256 of both and whether the words of the pass moved, and
	// names each in Dispatch-As-Of-Absent.
	requireLeftOut := func(t *testing.T, when string) {
		t.Helper()
		checks, absent := readChecksAtTheCut(t, handler, owner+"/results", cut)
		if _, held := checks["k3"]; !held || len(checks["k1"].results) != 1 {
			t.Fatalf("%s, the read at the cut holds no check k3, or no check k1 with the one pass recorded before the cut: %v", when, checks)
		}
		requireTheDigestLeftOut(t, when, checks["k1"], absent)
		requireTheDigestLeftOut(t, when, checks["k3"], absent)
	}

	// The block index holds the version at the cut under the digest the read then gave.
	requireLeftOut(t, "before the re-index")

	// The re-index in place: the document is owed its block index, and the reconciliation indexes
	// the live text, in this build's spelling, at the same version. The test server runs no
	// reconciliation loop, so the document stays owed until the reconciliation below.
	if _, err := database.Pool.Exec(t.Context(), `update document_block_versions set version = null where artifact_id = $1`, document); err != nil {
		t.Fatalf("owe the document its block index: %v", err)
	}
	requireLeftOut(t, "while the document is owed its block index")

	reconcileBlockIndexPass(t, served.deps)
	requireIndexedAt(t, database, document, number)
	if latest := latestVersion(t, database, document).Number; latest != number {
		t.Fatalf("the re-index wrote version %d, want none past version %d", latest, number)
	}
	var kept string
	if err := database.Pool.QueryRow(t.Context(), `
		select text from document_block_texts where artifact_id = $1 and block_id = 'k1' and version = $2
	`, document, number).Scan(&kept); err != nil || kept != respelledCheckBlock {
		t.Fatalf("after the re-index the text recorded of check k1 at version %d is %q (%v), want the other renderer's %q kept",
			number, kept, err, respelledCheckBlock)
	}
	reindexed := readResults(t, handler, issue.Key)
	if now := checkOf(t, reindexed, "k1"); now.Version != number || now.SHA256 == k1.SHA256 {
		t.Fatalf("after the re-index the present read gives check k1 version %d and sha256 %s, want version %d under a sha256 other than %s",
			now.Version, now.SHA256, number, k1.SHA256)
	}
	if now := checkOf(t, reindexed, "k3"); now.SHA256 != k3.SHA256 {
		t.Fatalf("after the re-index the present read gives check k3 sha256 %s, want %s, which the re-index left alone", now.SHA256, k3.SHA256)
	}

	requireLeftOut(t, "after the re-index")

	// A later version, which rewords k3 alone: the block index holds it now, and check k1 keeps
	// its changed version under the digest the re-index moved.
	later := uploadDocument(t, handler, owner, "verification.md",
		strings.Replace(respelledCheck, "A sibling check nobody reworded.", "A sibling check, reworded.", 1))
	requireIndexedAt(t, database, document, later.Version.Number)
	if now := checkOf(t, readResults(t, handler, issue.Key), "k1"); now.Version != number || now.SHA256 == k1.SHA256 {
		t.Fatalf("after the later version the present read gives check k1 version %d and sha256 %s, want version %d under a sha256 other than %s",
			now.Version, now.SHA256, number, k1.SHA256)
	}
	requireLeftOut(t, "once the block index holds a later version")
}

// TestAVersionFirstIndexedAfterTheCutGivesNoCheckDigestAtTheCut: a version of the Verification
// document that holds more blocks than a write indexes in its own request is saved with its block
// index owed (docs' deferBlockIndexBlocks), and the block-index reconciliation indexes it later.
// No record gives the time of that index. A read of the results at a cut taken while the version
// is owed its index leaves out each check's version and sha256 and whether the words of the pass
// moved, and names each in Dispatch-As-Of-Absent, before the reconciliation and after it: it gives
// no value from the index that the reconciliation wrote after the cut.
func TestAVersionFirstIndexedAfterTheCutGivesNoCheckDigestAtTheCut(t *testing.T) {
	t.Parallel()
	handler, database, served := resultsGitHubServed(t, map[string]bool{"acme/widgets": true}, nil, "main",
		func(string, string) (int, string) { return http.StatusOK, "diverged" }, nil)
	issue := issueInRepository(t, handler, "RLTX", "acme/widgets", "Indexed after the cut")
	owner := "/api/v1/issues/" + issue.Key
	document := uploadDocument(t, handler, owner, "verification.md", pastResultsChecks).Artifact.ID
	writeHead(t, database, "acme/widgets", 65, headCommit, "open", issue.Key)
	recorded := recordResult(t, handler, issue.Key, resultBody("k1", "pass", headCommit, nil))
	if recorded.Code != http.StatusCreated {
		t.Fatalf("record a pass of k1: status=%d body=%s", recorded.Code, recorded.Body.String())
	}
	pass := decodeBody[recordedResult](t, recorded).Result

	// Version N rewords k1, and its list takes it past the block bound, so its write leaves its
	// block index owed to the reconciliation.
	reworded := strings.Replace(pastResultsChecks, "The server records a result against a check.",
		"The server records a result against a check, reworded.", 1)
	number := uploadDocument(t, handler, owner, "verification.md",
		reworded+"\n"+strings.Repeat("- x\n", itemsOverTheBlockBound)).Version.Number
	if indexed := blockIndexVersion(t, database, document); indexed != nil {
		t.Fatalf("the write of version %d indexed its blocks at version %d, want them owed to the reconciliation", number, *indexed)
	}
	cut := settledResultsCut(t, database, served)

	// requireLeftOut fails unless the read at the cut holds check k2 and check k1 with its pass,
	// leaves out the version and the sha256 of both and whether the words of the pass moved, and
	// names each in Dispatch-As-Of-Absent.
	requireLeftOut := func(t *testing.T, when string) {
		t.Helper()
		checks, absent := readChecksAtTheCut(t, handler, owner+"/results", cut)
		if _, held := checks["k2"]; !held || len(checks["k1"].results) != 1 {
			t.Fatalf("%s, the read at the cut holds no check k2, or no check k1 with the one pass recorded before the cut: %v", when, checks)
		}
		for _, id := range []string{"k1", "k2"} {
			requireTheDigestLeftOut(t, when, checks[id], absent)
		}
	}
	requireLeftOut(t, fmt.Sprintf("while version %d is owed its block index", number))

	reconcileBlockIndexPass(t, served.deps)
	requireIndexedAt(t, database, document, number)
	if now := checkOf(t, readResults(t, handler, issue.Key), "k1"); now.Version != number || now.SHA256 == pass.CheckSHA256 {
		t.Fatalf("after the reconciliation the present read gives check k1 version %d and sha256 %s, want version %d under a sha256 other than its pass's %s",
			now.Version, now.SHA256, number, pass.CheckSHA256)
	}
	requireLeftOut(t, fmt.Sprintf("after the reconciliation indexed version %d", number))
}

// checkReadAtTheCut is one check of a read of the results at a cut: its place in the answer, its
// members as the answer's JSON gives them, and the members of each of its results.
type checkReadAtTheCut struct {
	place   string
	members map[string]any
	results []map[string]any
}

// readChecksAtTheCut reads results, the results route of an issue, at cut as alice: each check of
// the answer as its JSON gives it, by block id, and the answer's Dispatch-As-Of-Absent. It fails
// the test unless the answer is 200 and names the cut.
func readChecksAtTheCut(t *testing.T, handler http.Handler, results string, cut asof.Cut) (map[string]checkReadAtTheCut, string) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, withAsOf(t, results, cut), nil, "alice")
	if response.Code != http.StatusOK || response.Header().Get(contracts.DispatchAsOfHeader) != cut.String() {
		t.Fatalf("the read at the cut %s: status=%d %s=%q body=%s", cut, response.Code, contracts.DispatchAsOfHeader,
			response.Header().Get(contracts.DispatchAsOfHeader), response.Body.String())
	}
	var answer struct {
		Checks []map[string]any `json:"checks"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the read at the cut %s: %v", cut, err)
	}
	checks := map[string]checkReadAtTheCut{}
	for index, members := range answer.Checks {
		check := checkReadAtTheCut{place: "/checks/" + strconv.Itoa(index), members: members}
		held, _ := members["results"].([]any)
		for _, element := range held {
			result, _ := element.(map[string]any)
			check.results = append(check.results, result)
		}
		id, _ := members["block_id"].(string)
		checks[id] = check
	}
	return checks, response.Header().Get(contracts.DispatchAsOfAbsentHeader)
}

// requireTheDigestLeftOut fails unless check, of a read of the results at a cut whose
// Dispatch-As-Of-Absent is absent, leaves out its version, its sha256 and whether the words of
// each of its results moved, as its JSON gives it, and names each at its place.
func requireTheDigestLeftOut(t *testing.T, when string, check checkReadAtTheCut, absent string) {
	t.Helper()
	if check.place == "" {
		t.Errorf("%s, the read at the cut holds no such check", when)
		return
	}
	id := check.members["block_id"]
	if version, given := check.members["version"]; given {
		t.Errorf("%s, check %v reads version %v at the cut, want it left out", when, id, version)
	}
	if sha256, given := check.members["sha256"]; given {
		t.Errorf("%s, check %v reads sha256 %v at the cut, want it left out", when, id, sha256)
	}
	places := []string{check.place + "/version", check.place + "/sha256"}
	for number, result := range check.results {
		if moved, given := result["check_words_moved"]; given {
			t.Errorf("%s, result %d of check %v reads check_words_moved %v at the cut, want it left out with the version and the digest it compares",
				when, number, id, moved)
		}
		places = append(places, check.place+"/results/"+strconv.Itoa(number)+"/check_words_moved")
	}
	named := absentPointers(absent)
	for _, place := range places {
		if !namedAbsent(named, place) {
			t.Errorf("%s, %s %q does not name %s", when, contracts.DispatchAsOfAbsentHeader, absent, place)
		}
	}
}

// settledResultsCut waits for the database's document and background writes to settle, reads a cut
// from the database and pauses past it so a later write stands strictly after the cut.
func settledResultsCut(t *testing.T, database *store.Store, served *server) asof.Cut {
	t.Helper()
	awaitSettledDocuments(t, database)
	settleServed(t, served)
	cut := databaseCut(t, database)
	time.Sleep(5 * time.Millisecond)
	return cut
}
