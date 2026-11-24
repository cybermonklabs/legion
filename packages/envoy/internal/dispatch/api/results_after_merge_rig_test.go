// results_after_merge_rig_test.go builds the Dispatch the tests of a check compared with a branch
// drive (results_branch_newest_test.go, results_after_merge_test.go): issues of one repository,
// acme/widgets, whose default branch is main, and a GitHub stand-in that answers each compare read
// from the test's own table and each pull request read with the whole pull request. A test writes
// a merged pull request's row with writeHeadOnBase, which stores no merge commit, and the stand-in
// gives the read that refreshes it the merge commit and its time. The rig reads the route's answer
// as JSON into the types of this file and is made of helpers older than the merge rule, so a test
// on it builds at a commit before that rule and fails there on its own assertion.
package api

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/githubtest"
)

// afterMergeProject is the project of every issue of an afterMergeRig, so its issues are ACME-1,
// ACME-2 and on, in the order a test makes them.
const afterMergeProject = "ACME"

// afterMergeGitHub is the GitHub of an afterMergeRig. Its tables are the test's to fill, under mu,
// before the read that asks; each read is counted.
type afterMergeGitHub struct {
	mu sync.Mutex
	// carries is the commits each branch carries, by branch: compare/<branch>...<sha> answers
	// behind for one of them and diverged for every other commit.
	carries map[string][]string
	// holds is the merge commits each commit's history holds, by commit: compare/<sha>...<merge>
	// answers behind for one of them and diverged for every other commit. A compare base that is
	// no branch of carries is a commit.
	holds map[string][]string
	// failing is the commits whose every compare read answers 502, a read of GitHub's that did not
	// answer.
	failing []string
	// pulls is GET /repos/acme/widgets/pulls/{number}'s answer for each pull request it names, and
	// pullStatus a status a pull request read answers in place of it; a number neither names
	// answers 404.
	pulls      map[int]map[string]any
	pullStatus map[int]int
	// compares counts the compare reads by `base...head`, and pullReads the pull request reads by
	// number.
	compares  map[string]int
	pullReads map[int]int
}

// carry says that branch carries each of commits.
func (g *afterMergeGitHub) carry(branch string, commits ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.carries[branch] = append(g.carries[branch], commits...)
}

// hold says that the history of each of commits holds merge.
func (g *afterMergeGitHub) hold(merge string, commits ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, commit := range commits {
		g.holds[commit] = append(g.holds[commit], merge)
	}
}

// fail makes every compare read naming commit answer 502.
func (g *afterMergeGitHub) fail(commit string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failing = append(g.failing, commit)
}

// setPull is the answer of the read of pull request number; status, where it is not 0, is the
// status that read answers in place of it.
func (g *afterMergeGitHub) setPull(number int, pull map[string]any, status int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pulls[number] = pull
	if status != 0 {
		g.pullStatus[number] = status
	}
}

// comparesOf is how often GitHub was asked compare/<base>...<head>, so far.
func (g *afterMergeGitHub) comparesOf(base, head string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.compares[base+"..."+head]
}

// pullReadsOf is how often GitHub was asked for pull request number, so far.
func (g *afterMergeGitHub) pullReadsOf(number int) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pullReads[number]
}

// compare answers compare/<base>...<head> from the tables.
func (g *afterMergeGitHub) compare(w http.ResponseWriter, r *http.Request) {
	base, head, _ := strings.Cut(r.PathValue("basehead"), "...")
	g.mu.Lock()
	g.compares[base+"..."+head]++
	failing := slices.Contains(g.failing, base) || slices.Contains(g.failing, head)
	carried, branch := g.carries[base]
	behind := slices.Contains(carried, head)
	if !branch {
		behind = slices.Contains(g.holds[base], head)
	}
	g.mu.Unlock()
	switch {
	case failing:
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, `{"message":"Bad Gateway"}`)
	case behind:
		fmt.Fprint(w, `{"status":"behind"}`)
	default:
		fmt.Fprint(w, `{"status":"diverged"}`)
	}
}

// pull answers GET /repos/acme/widgets/pulls/{number} from the table. The pull request's
// updated_at is the time of the read, so a refresh of a row whose last event is older applies.
func (g *afterMergeGitHub) pull(w http.ResponseWriter, r *http.Request) {
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	g.mu.Lock()
	g.pullReads[number]++
	pull, held := g.pulls[number]
	status := g.pullStatus[number]
	g.mu.Unlock()
	switch {
	case status != 0:
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"message":%q}`, http.StatusText(status))
		return
	case !held:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
		return
	}
	answer := map[string]any{"updated_at": time.Now().UTC().Format(time.RFC3339Nano)}
	for name, value := range pull {
		answer[name] = value
	}
	body, err := json.Marshal(answer)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(body)
}

// afterMergePull is pull request number of acme/widgets as GitHub answers it once it merged into
// base at mergedAt as merge, from the branch headRef at its final head head, its title naming
// issue, so a refresh of it attributes it to issue as before. An empty merge is a merged pull
// request whose answer names no merge commit.
func afterMergePull(issue string, number int, head, headRef, base, merge, mergedAt string) map[string]any {
	var mergeCommit any
	if merge != "" {
		mergeCommit = merge
	}
	return map[string]any{
		"number": number, "title": issue + ": the work", "body": "",
		"html_url": fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number),
		"user":     map[string]any{"login": "alice"}, "labels": []any{},
		"state": "closed", "merged": true, "merged_at": mergedAt, "merge_commit_sha": mergeCommit,
		"created_at": "2026-11-20T09:00:00Z",
		"head":       map[string]any{"ref": headRef, "sha": head, "repo": map[string]any{"full_name": "acme/widgets"}},
		"base":       map[string]any{"ref": base},
	}
}

// afterMergeRig is a Dispatch holding an App key whose GitHub is github: every repository is
// installed and answers main as its default branch, and holds every commit asked about.
type afterMergeRig struct {
	handler  http.Handler
	database *store.Store
	github   *afterMergeGitHub
	issues   int
}

func newAfterMergeRig(t *testing.T) *afterMergeRig {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate fixture key: %v", err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	github := &afterMergeGitHub{
		carries: map[string][]string{}, holds: map[string][]string{},
		pulls: map[int]map[string]any{}, pullStatus: map[int]int{},
		compares: map[string]int{}, pullReads: map[int]int{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":77,"app_slug":"dispatch-test","permissions":{"contents":"read","pull_requests":"read"}}`)
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
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}/pulls", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[]`)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/compare/{basehead}", github.compare)
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}", github.pull)
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}/commits", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[]`)
	})
	upstream := githubtest.Server(t, githubtest.Spec{Handle: mux.ServeHTTP})
	handler, database, _, _ := newRegisteredTestServer(t, testServerOptions{
		app:           &auth.AppConfig{ClientID: "Iv1.test", PEM: pemText},
		githubAPIBase: upstream,
	})
	return &afterMergeRig{handler: handler, database: database, github: github}
}

// issue is a new issue of acme/widgets holding verification as its Verification document, and
// its key: ACME-1 for the rig's first, ACME-2 for its second, and on.
func (r *afterMergeRig) issue(t *testing.T, title, verification string) string {
	t.Helper()
	r.issues++
	if r.issues == 1 {
		key := issueInRepository(t, r.handler, afterMergeProject, "acme/widgets", title).Key
		uploadDocument(t, r.handler, "/api/v1/issues/"+key, "verification.md", verification)
		return key
	}
	return issueOfProject(t, r.handler, afterMergeProject, title, verification)
}

// record records role's verdict of check of issue at commit, and answers the stored result's id.
func (r *afterMergeRig) record(t *testing.T, issue, check, role, verdict, commit string) string {
	t.Helper()
	return recordAfterMerge(t, r.handler, issue, check, role, verdict, commit)
}

// read is each check of one results read of issue, by its block id.
func (r *afterMergeRig) read(t *testing.T, issue string) map[string]afterMergeCheck {
	t.Helper()
	return readAfterMerge(t, r.handler, issue)
}

// recordAfterMerge records role's verdict of check of issue at commit through handler, and answers
// the stored result's id.
func recordAfterMerge(t *testing.T, handler http.Handler, issue, check, role, verdict, commit string) string {
	t.Helper()
	fields := map[string]any{"role": role}
	if verdict == "fail" {
		fields["failed"] = 1
	}
	response := recordResult(t, handler, issue, resultBody(check, verdict, commit, fields))
	if response.Code != http.StatusCreated {
		t.Fatalf("record %s's %s of %s at %s: status=%d body=%s", role, verdict, check, commit, response.Code, response.Body.String())
	}
	var recorded struct {
		Result struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &recorded); err != nil || recorded.Result.ID == "" {
		t.Fatalf("decode the recorded %s: %v (body %s)", verdict, err, response.Body.String())
	}
	return recorded.Result.ID
}

// afterMergeStands is a check's pass_stands, with the members these tests read.
type afterMergeStands struct {
	Answer  string   `json:"answer"`
	Reason  string   `json:"reason"`
	Roles   []string `json:"roles"`
	Results []string `json:"results"`
	Commit  string   `json:"commit"`
}

// afterMergeBasis is one basis of a check, or the one a result's commit stands at.
type afterMergeBasis struct {
	Kind        string `json:"kind"`
	Repo        string `json:"repo"`
	Branch      string `json:"branch"`
	MergeCommit string `json:"merge_commit"`
}

// afterMergeResult is one result of a check, with where its commit stands.
type afterMergeResult struct {
	ID           string `json:"id"`
	CommitStands *struct {
		Answer string           `json:"answer"`
		Reason string           `json:"reason"`
		Repo   string           `json:"repo"`
		Basis  *afterMergeBasis `json:"basis"`
	} `json:"commit_stands"`
}

// afterMergeCheck is one check of a results read, with the members these tests read.
type afterMergeCheck struct {
	ComparedWith    []afterMergeBasis  `json:"compared_with"`
	ComparedNothing string             `json:"compared_nothing"`
	PassStands      afterMergeStands   `json:"pass_stands"`
	Results         []afterMergeResult `json:"results"`
}

// result is the result of the check with that id.
func (c afterMergeCheck) result(t *testing.T, id string) afterMergeResult {
	t.Helper()
	for _, result := range c.Results {
		if result.ID == id {
			if result.CommitStands == nil {
				t.Fatalf("the result %s carries no commit_stands", id)
			}
			return result
		}
	}
	t.Fatalf("the check holds no result %s: %+v", id, c.Results)
	return afterMergeResult{}
}

// readAfterMerge is each check of one results read of issue through handler, by its block id.
func readAfterMerge(t *testing.T, handler http.Handler, issue string) map[string]afterMergeCheck {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue+"/results", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the results of %s: status=%d body=%s", issue, response.Code, response.Body.String())
	}
	var read struct {
		Checks []struct {
			BlockID string `json:"block_id"`
			afterMergeCheck
		} `json:"checks"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &read); err != nil {
		t.Fatalf("decode the results of %s: %v", issue, err)
	}
	checks := map[string]afterMergeCheck{}
	for _, check := range read.Checks {
		checks[check.BlockID] = check.afterMergeCheck
	}
	return checks
}
