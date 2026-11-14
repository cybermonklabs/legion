package cireconcile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/ratelimit"
	"github.com/sjawhar/envoy/internal/githubtest"
)

// fakeGitHub is an httptest stand-in for the GitHub App and REST APIs a reconcile pass calls,
// built on the shape of delivery's own newFakeGitHub: it mounts the installation lookup and the
// token mint, and over them the four reads this package makes -- a repository's identity, its
// open pull requests, and a commit's check runs and check suites -- each paged as GitHub pages
// them, each answered with an ETag of its own, and each recorded.
//
// Its ETag is a digest of the body the URL (path and query together) would answer with, so the
// ETag of a URL changes exactly when a test changes what that URL answers, and a request whose
// If-None-Match matches is answered 304 with no body, as GitHub answers one.
//
// Every field is held under mu, so two passes may run against one stand-in.
type fakeGitHub struct {
	t   *testing.T
	mux *http.ServeMux

	mu        sync.Mutex
	installID int64
	mints     int
	// repos answers GET /repos/{owner}/{repo}, keyed by the owner/repo the request names, which
	// is not always the full_name the answer gives: a repository read under a name it has been
	// renamed from answers with the name GitHub gives it now.
	repos map[string]fakeRepository
	// pulls, runs and suites are keyed by the owner/repo the request names, and runs and suites
	// by "owner/repo@sha".
	pulls  map[string][]fakePull
	runs   map[string][]object
	suites map[string][]object
	// etags is the ETag the stand-in last answered each URL with, so a test can read one.
	etags map[string]string
	// requests is every request the stand-in received, oldest first.
	requests []fakeRequest
	// limitEverything answers every request as GitHub's rate limit; limitOnce answers that many
	// more requests to one path so, and no others.
	limitEverything bool
	limitOnce       map[string]int
	// stalls is how long one path's answer is held back before it is given, which is how a test
	// makes GitHub slower than the pass's own bound (stall).
	stalls map[string]time.Duration
	// The state the GraphQL stand-in holds beside the REST one (graphql_test.go): the queries
	// it was asked, the heads whose Checks fields it refuses, the heads whose node or head
	// commit it answers null, the pages of one repository it refuses whole, the rate limits it
	// answers the next queries with, and how long one page's answer is held back.
	queries       []fakeQuery
	refusedChecks map[string]struct{}
	nulled        map[string]nulledHead
	refusedPages  map[string]map[int]struct{}
	graphQLLimits []graphQLLimit
	graphQLStalls map[string]time.Duration
	// queryTakes is how long every query's answer takes, which is what a test of the pass's
	// GraphQL pacing measures the pass against, and inFlight and mostInFlight how many queries
	// the stand-in was answering at once, which for a pass that queries serially is one.
	queryTakes   time.Duration
	inFlight     int
	mostInFlight int
	// verbatim is the captured answer the next query about one repository is given byte for
	// byte, and cursorsGiven what each cursor such an answer carried names, since GitHub's own
	// cursor is opaque text this stand-in cannot write or read (answerQueryWith).
	verbatim     map[string]string
	cursorsGiven map[string]queryAfter
}

// fakeRepository is what GET /repos/{owner}/{repo} answers: the name GitHub itself gives the
// repository and its id. unreadable answers that read 404 instead, while the installation
// lookup still answers for the repository: GitHub showing the App a repository it then does not
// answer for.
type fakeRepository struct {
	fullName   string
	id         int64
	unreadable bool
}

// fakePull is one open pull request of GET /repos/{owner}/{repo}/pulls.
type fakePull struct {
	number int64
	sha    string
}

// fakeRequest is one request the stand-in received: the URL is the path and query together, as
// an ETag is held by.
type fakeRequest struct {
	method      string
	url         string
	ifNoneMatch string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{
		t:             t,
		mux:           http.NewServeMux(),
		installID:     1,
		repos:         map[string]fakeRepository{},
		pulls:         map[string][]fakePull{},
		runs:          map[string][]object{},
		suites:        map[string][]object{},
		etags:         map[string]string{},
		limitOnce:     map[string]int{},
		stalls:        map[string]time.Duration{},
		refusedChecks: map[string]struct{}{},
		nulled:        map[string]nulledHead{},
		refusedPages:  map[string]map[int]struct{}{},
		graphQLStalls: map[string]time.Duration{},
		cursorsGiven:  map[string]queryAfter{},
	}
	f.mux.HandleFunc("POST /graphql", f.answerQuery)
	f.mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		_, known := f.repos[r.PathValue("owner")+"/"+r.PathValue("repo")]
		installID := f.installID
		f.mu.Unlock()
		if !known {
			// GitHub answers 404 for a repository the App is not installed on, which the client
			// reads as ErrNoInstallation.
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		f.encode(w, map[string]any{
			"id":          installID,
			"app_slug":    "cireconcile-test",
			"permissions": map[string]string{"checks": "read", "pull_requests": "read", "contents": "read"},
		})
	})
	f.mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.mints++
		token := fmt.Sprintf("ghs_fake_%s_%d", r.PathValue("id"), f.mints)
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		f.encode(w, map[string]any{"token": token, "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339Nano)})
	})
	f.mux.HandleFunc("GET /repos/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		repository, known := f.repos[r.PathValue("owner")+"/"+r.PathValue("repo")]
		f.mu.Unlock()
		if !known || repository.unreadable {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		f.answer(w, r, map[string]any{"full_name": repository.fullName, "id": repository.id})
	})
	f.mux.HandleFunc("GET /repos/{owner}/{repo}/pulls", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		pulls := f.pulls[r.PathValue("owner")+"/"+r.PathValue("repo")]
		f.mu.Unlock()
		payload := make([]map[string]any, 0, len(pulls))
		for _, pull := range onePage(r, pulls) {
			payload = append(payload, map[string]any{
				"number": pull.number,
				"state":  "open",
				"head":   map[string]any{"sha": pull.sha},
			})
		}
		f.answer(w, r, payload)
	})
	f.mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}/check-runs", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		runs := f.runs[commitKey(r)]
		f.mu.Unlock()
		page := onePage(r, runs)
		f.answer(w, r, map[string]any{"total_count": len(runs), "check_runs": page})
	})
	f.mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}/check-suites", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		suites := f.suites[commitKey(r)]
		f.mu.Unlock()
		page := onePage(r, suites)
		f.answer(w, r, map[string]any{"total_count": len(suites), "check_suites": page})
	})
	return f
}

// commitKey is the key a commit's check objects are held under: "owner/repo@sha".
func commitKey(r *http.Request) string {
	return r.PathValue("owner") + "/" + r.PathValue("repo") + "@" + r.PathValue("sha")
}

// serve records one request, answers it as a rate limit when a test asked for that, and
// otherwise hands it to the routes above.
func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, fakeRequest{
		method:      r.Method,
		url:         r.URL.RequestURI(),
		ifNoneMatch: r.Header.Get("If-None-Match"),
	})
	limited := f.limitEverything
	if left := f.limitOnce[r.URL.Path]; left > 0 {
		f.limitOnce[r.URL.Path] = left - 1
		limited = true
	}
	stall := f.stalls[r.URL.Path]
	f.mu.Unlock()
	if stall > 0 {
		// GitHub is slower to answer this than the pass has left. The caller giving up on the
		// request closes its context here, so the answer is abandoned at once rather than held
		// until the wait is over, which would keep the server's own Close waiting at the end of
		// the test.
		select {
		case <-time.After(stall):
		case <-r.Context().Done():
			return
		}
	}
	if limited {
		// GitHub's own answer to an exhausted rate limit.
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(90*time.Second).Unix(), 10))
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
		return
	}
	f.mux.ServeHTTP(w, r)
}

// answer writes one JSON answer with the ETag of its body, or 304 and no body when the request
// carries that ETag already.
func (f *fakeGitHub) answer(w http.ResponseWriter, r *http.Request, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		f.t.Errorf("encode the answer to %s: %v", r.URL.RequestURI(), err)
		return
	}
	digest := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(digest[:8]) + `"`
	f.mu.Lock()
	f.etags[r.URL.RequestURI()] = etag
	f.mu.Unlock()
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if _, err := w.Write(body); err != nil {
		f.t.Errorf("write the answer to %s: %v", r.URL.RequestURI(), err)
	}
}

// encode writes one JSON answer that carries no ETag: the App endpoints, which no caller reads
// conditionally.
func (f *fakeGitHub) encode(w http.ResponseWriter, payload any) {
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		f.t.Errorf("encode an App answer: %v", err)
	}
}

// onePage is the slice of items the request's per_page and page name, as GitHub pages a listing.
func onePage[T any](r *http.Request, items []T) []T {
	perPage := queryNumber(r.URL.Query(), "per_page", 30)
	page := queryNumber(r.URL.Query(), "page", 1)
	if perPage < 1 {
		perPage = 30
	}
	if page < 1 {
		page = 1
	}
	from := (page - 1) * perPage
	if from >= len(items) {
		return nil
	}
	return items[from:min(from+perPage, len(items))]
}

func queryNumber(values url.Values, name string, fallback int) int {
	number, err := strconv.Atoi(values.Get(name))
	if err != nil {
		return fallback
	}
	return number
}

// client is a githubapp.Client on a throwaway key, pointed at this stand-in, obeying and
// recording the shared rate limits as every Dispatch process's client does.
func (f *fakeGitHub) client(limits *ratelimit.Store) *githubapp.Client {
	f.t.Helper()
	_, text := githubapptest.Key(f.t)
	base := githubtest.Server(f.t, githubtest.Spec{Handle: f.serve})
	client, err := githubapp.New(&auth.AppConfig{ClientID: "Iv1.cireconcile", PEM: text}, base, githubapp.WithLimits(limits))
	if err != nil {
		f.t.Fatalf("new client: %v", err)
	}
	if client == nil {
		f.t.Fatal("new client: got nil for a configured app")
	}
	return client
}

// setRepository makes GET /repos/{name} answer with fullName and id. name is the owner/repo a
// request names it by, which a renamed repository has two of.
func (f *fakeGitHub) setRepository(name, fullName string, id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repos[name] = fakeRepository{fullName: fullName, id: id}
}

// setPullRequests makes repository's open pull requests exactly these.
func (f *fakeGitHub) setPullRequests(repository string, pulls ...fakePull) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulls[repository] = pulls
}

// setCheckRuns makes the check runs of one commit exactly these, which changes the ETag of the
// URL that lists them.
func (f *fakeGitHub) setCheckRuns(repository, sha string, runs ...object) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs[repository+"@"+sha] = runs
}

// setCheckSuites makes the check suites of one commit exactly these.
func (f *fakeGitHub) setCheckSuites(repository, sha string, suites ...object) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.suites[repository+"@"+sha] = suites
}

// rateLimitEverything answers every request from now on as GitHub answers an exhausted limit.
func (f *fakeGitHub) rateLimitEverything() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.limitEverything = true
}

// rateLimitNext answers the next request to path so, and no other request.
func (f *fakeGitHub) rateLimitNext(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.limitOnce[path]++
}

// stall holds back every answer to path by wait, as GitHub answering a poller slowly does. A
// wait of zero answers it as fast as every other path again.
func (f *fakeGitHub) stall(path string, wait time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if wait <= 0 {
		delete(f.stalls, path)
		return
	}
	f.stalls[path] = wait
}

// received is every request the stand-in has been sent, oldest first.
func (f *fakeGitHub) received() []fakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeRequest(nil), f.requests...)
}

// count is how many requests the stand-in has been sent.
func (f *fakeGitHub) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// countPath is how many of them named path.
func (f *fakeGitHub) countPath(path string) int {
	sent := 0
	for _, request := range f.received() {
		if before, _, _ := strings.Cut(request.url, "?"); before == path {
			sent++
		}
	}
	return sent
}

// sincePath is every request from index from onwards that named path, oldest first: what one
// pass sent to one listing, where from is count() read before that pass.
func (f *fakeGitHub) sincePath(from int, path string) []fakeRequest {
	var found []fakeRequest
	for _, request := range f.received()[from:] {
		if before, _, _ := strings.Cut(request.url, "?"); before == path {
			found = append(found, request)
		}
	}
	return found
}

// etagOf is the ETag the stand-in last answered one URL (path and query together) with.
func (f *fakeGitHub) etagOf(requestURI string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.etags[requestURI]
}

// hideRepository makes GET /repos/{name} answer 404 while the App is still installed on the
// repository, so a pass holds a token for it and GitHub answers nothing about it.
func (f *fakeGitHub) hideRepository(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repos[name] = fakeRepository{unreadable: true}
}

// installationID is the installation every token is minted for, the id GitHub budgets this
// stand-in's reads under (githubapp.InstallationScope).
func (f *fakeGitHub) installationID() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.installID
}

// forgetRequests empties the request log, so what a test asserts on afterwards is what the
// passes after it sent and nothing else.
func (f *fakeGitHub) forgetRequests() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = nil
}
