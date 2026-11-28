package cireconcile

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/githubtest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// fakeGitHub is an httptest stand-in for the GitHub App and REST APIs a reconcile pass calls,
// built on the shape of delivery's own newFakeGitHub: it mounts the installation lookup and the
// token mint, and over them the five reads this package makes -- a repository's identity, its
// open pull requests, and a commit's check runs, check suites and commit statuses -- each paged
// as GitHub pages them, each answered with an ETag of its own, and each recorded with the route
// it was answered by.
//
// Its ETag is a digest of the body the URL (path and query together) would answer with, so the
// ETag of a URL changes exactly when a test changes what that URL answers, and a request whose
// If-None-Match matches is answered 304 with no body, as GitHub answers one.
//
// Every field is held under mu, so two passes may run against one stand-in.
type fakeGitHub struct {
	t   *testing.T
	mux *http.ServeMux
	// clock is the passes' own clock (harness.clock), which stamps each query's start and end
	// and the reset of each limit the stand-in answers, and which a query's answer moves on by
	// the time GitHub takes over it (everyQueryTakes).
	clock *clocktest.Fake
	// recordClock is the clock the stand-in's client bounds each write of a limit GitHub answered
	// on (githubapp.LimitRecordTimeout): a fake of its own, which no test here moves, so that
	// write's bound never falls while a test holds it. On clock it would fall at every move a test
	// makes past it while the write is in flight -- to a wait the pass armed, or to the pass's own
	// bound -- and a limit GitHub answered as that bound fell would not reach the ledger
	// (TestALimitAnsweredAsTheBoundFallsIsALimit). The client's rate-limit clock is recordClock
	// with clock's moment (clocktest.WithNow, client): two fakes in one, sound only for the two
	// uses githubapp makes of it, WithTimeout for that bound (limitRecordContext, client.go),
	// reckoned from recordClock's own moment, and Now for a limit's wait (limitNow,
	// deliveries.go), which clock answers. A use that mixed the two -- a deadline taken from Now
	// and armed through WithDeadline, or a span reckoned from Now and armed as a timer -- would
	// set one fake's moment against the other's, so a new use of that clock in githubapp needs
	// this read again.
	recordClock *clocktest.Fake
	// held names each request the stand-in holds back (hold, holdQuery) as it is held: what a
	// test waits on (testwait.Next) before it ends the pass's bound or cancels the pass's caller,
	// so the pass ends while it waits on GitHub and never before it has asked.
	held testwait.Stream[string]

	mu        sync.Mutex
	installID int64
	mints     int
	// repos answers GET /repos/{owner}/{repo}, keyed by the owner/repo the request names, which
	// is not always the full_name the answer gives: a repository read under a name it has been
	// renamed from answers with the name GitHub gives it now.
	repos map[string]fakeRepository
	// pulls, runs, suites and statuses are keyed by the owner/repo the request names, and runs,
	// suites and statuses by "owner/repo@sha".
	pulls    map[string][]fakePull
	runs     map[string][]object
	suites   map[string][]object
	statuses map[string][]object
	// fullRuns is the commits whose check runs answer every page with one full page encoded
	// once, keyed as runs is (setFullCheckRunPages).
	fullRuns map[string]fullPage
	// tokenPermissions is what every token the stand-in mints from now on holds, as GitHub's
	// mint answers it, and minted what each token it has minted holds: a read of commit statuses
	// with a token that holds no Commit statuses is refused, as GitHub refuses it.
	// refusedStatuses is the commits whose statuses are refused whatever the token holds, which
	// is a grant of Commit statuses taken away while a token minted with it lives.
	tokenPermissions map[string]string
	minted           map[string]map[string]string
	refusedStatuses  map[string]struct{}
	// etags is the ETag the stand-in last answered each URL with, so a test can read one.
	etags map[string]string
	// requests is every request the stand-in received, oldest first.
	requests []fakeRequest
	// limitEverything answers every request as GitHub's rate limit; limitOnce answers that many
	// more requests to one path so, and no others. reset is the moment the last such answer said
	// the limit resets at.
	limitEverything bool
	limitOnce       map[string]int
	reset           time.Time
	// failOnce answers that many more requests to one path with GitHub's 502, which is no rate
	// limit and no answer about anything: GitHub's own trouble (failNext).
	failOnce map[string]int
	// holds is the paths whose answers are held back, each with the channel release closes to let
	// the requests held there go: how a test makes GitHub slower than the pass has left (hold).
	holds map[string]chan struct{}
	// answerCodes is the status one path is answered with in place of its route's answer, which
	// is how a test makes GitHub give up on or refuse that one read (answerStatus).
	answerCodes map[string]int
	// The state the GraphQL stand-in holds beside the REST one (graphql_test.go): the queries
	// it was asked, the heads whose Checks fields it refuses, the heads whose node or head
	// commit it answers null, the pages of one repository it refuses whole, the rate limits it
	// answers the next queries with, and the pages whose answers it holds back (holdQuery).
	queries       []fakeQuery
	refusedChecks map[string]struct{}
	nulled        map[string]nulledHead
	refusedPages  map[string]map[int]struct{}
	graphQLLimits []graphQLLimit
	heldQueries   map[string]chan struct{}
	// queryTakes is how long every query's answer takes on the passes' clock, which is what a
	// test of the pass's GraphQL pacing measures the pass against, and inFlight and mostInFlight
	// how many queries the stand-in was answering at once, which for a pass that queries
	// serially is one.
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

// fullPage is a listing GitHub reports full however many of its pages are read: body, the answer
// to every page, one full page and the listing's total_count encoded once, and size, how many
// objects that page holds, the page size a read must ask for that page to be a full one.
type fullPage struct {
	body []byte
	size int
}

// fakeRequest is one request the stand-in received: the URL is the path and query together, as
// an ETag is held by, and route is the pattern of the stand-in's own mux that answered it.
type fakeRequest struct {
	method      string
	url         string
	route       string
	ifNoneMatch string
}

// The routes of the stand-in's mux, which a test counts requests by (fakeGitHub.byRoute).
const (
	installationRoute = "GET /repos/{owner}/{repo}/installation"
	tokenRoute        = "POST /app/installations/{id}/access_tokens"
	repositoryRoute   = "GET /repos/{owner}/{repo}"
	pullsRoute        = "GET /repos/{owner}/{repo}/pulls"
	checkRunsRoute    = "GET /repos/{owner}/{repo}/commits/{sha}/check-runs"
	checkSuitesRoute  = "GET /repos/{owner}/{repo}/commits/{sha}/check-suites"
	statusRoute       = "GET /repos/{owner}/{repo}/commits/{sha}/status"
	graphQLRoute      = "POST /graphql"
)

func newFakeGitHub(t *testing.T, clock *clocktest.Fake) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{
		t:           t,
		mux:         http.NewServeMux(),
		clock:       clock,
		recordClock: clocktest.New(harnessStart),
		installID:   1,
		repos:       map[string]fakeRepository{},
		pulls:       map[string][]fakePull{},
		runs:        map[string][]object{},
		fullRuns:    map[string]fullPage{},
		suites:      map[string][]object{},
		statuses:    map[string][]object{},
		// The permissions the installation lookup below answers, with metadata, which GitHub
		// gives every token: no Commit statuses, so a pass reads no status unless a test grants
		// one (setTokenPermissions).
		tokenPermissions: map[string]string{"checks": "read", "contents": "read", "metadata": "read", "pull_requests": "read"},
		minted:           map[string]map[string]string{},
		refusedStatuses:  map[string]struct{}{},
		etags:            map[string]string{},
		limitOnce:        map[string]int{},
		failOnce:         map[string]int{},
		holds:            map[string]chan struct{}{},
		answerCodes:      map[string]int{},
		refusedChecks:    map[string]struct{}{},
		nulled:           map[string]nulledHead{},
		refusedPages:     map[string]map[int]struct{}{},
		heldQueries:      map[string]chan struct{}{},
		cursorsGiven:     map[string]queryAfter{},
	}
	f.mux.HandleFunc(graphQLRoute, f.answerQuery)
	f.mux.HandleFunc(installationRoute, func(w http.ResponseWriter, r *http.Request) {
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
	f.mux.HandleFunc(tokenRoute, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.mints++
		token := fmt.Sprintf("ghs_fake_%s_%d", r.PathValue("id"), f.mints)
		permissions := maps.Clone(f.tokenPermissions)
		f.minted[token] = permissions
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		f.encode(w, map[string]any{
			"token": token,
			// The token's expiry is a stamp on the real clock that no verdict reads, which
			// INFRA-3149's architect allowed (dispatch://INFRA-3149/comment/3464cf0b-d808-46dd-9c83-35f6649c5a5b).
			"expires_at":  time.Now().Add(time.Hour).Format(time.RFC3339Nano),
			"permissions": permissions,
		})
	})
	f.mux.HandleFunc(repositoryRoute, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		repository, known := f.repos[r.PathValue("owner")+"/"+r.PathValue("repo")]
		f.mu.Unlock()
		if !known || repository.unreadable {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		f.answer(w, r, map[string]any{"full_name": repository.fullName, "id": repository.id})
	})
	f.mux.HandleFunc(pullsRoute, func(w http.ResponseWriter, r *http.Request) {
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
	f.mux.HandleFunc(checkRunsRoute, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		runs := f.runs[commitKey(r)]
		full, endless := f.fullRuns[commitKey(r)]
		f.mu.Unlock()
		if endless {
			if perPage := queryNumber(r.URL.Query(), "per_page", 30); perPage != full.size {
				f.t.Errorf("a read of %s asked for %d check runs a page, and the full page answering every page holds %d", r.URL.RequestURI(), perPage, full.size)
			}
			f.answerBody(w, r, full.body)
			return
		}
		page := onePage(r, runs)
		f.answer(w, r, map[string]any{"total_count": len(runs), "check_runs": page})
	})
	f.mux.HandleFunc(checkSuitesRoute, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		suites := f.suites[commitKey(r)]
		f.mu.Unlock()
		page := onePage(r, suites)
		f.answer(w, r, map[string]any{"total_count": len(suites), "check_suites": page})
	})
	f.mux.HandleFunc(statusRoute, func(w http.ResponseWriter, r *http.Request) {
		if !f.statusesReadable(r, r.PathValue("owner")+"/"+r.PathValue("repo"), r.PathValue("sha")) {
			// GitHub's answer to a read of statuses the token may not make.
			http.Error(w, `{"message":"Resource not accessible by integration"}`, http.StatusForbidden)
			return
		}
		f.mu.Lock()
		statuses := f.statuses[commitKey(r)]
		f.mu.Unlock()
		// GitHub's combined status: the newest status of each context, and the state it makes of
		// them, which the pass does not read.
		f.answer(w, r, map[string]any{
			"state":       combinedState(statuses),
			"sha":         r.PathValue("sha"),
			"total_count": len(statuses),
			"statuses":    onePage(r, statuses),
		})
	})
	return f
}

// statusesReadable reports whether the request's token may read one commit's statuses: the token
// holds Commit statuses at read or write, and the stand-in was not told to refuse that commit's
// statuses (refuseStatuses), which is a grant taken away while the token lives.
func (f *fakeGitHub) statusesReadable(r *http.Request, repository, sha string) bool {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, refused := f.refusedStatuses[repository+"@"+sha]; refused {
		return false
	}
	held := f.minted[token]["statuses"]
	return held == "read" || held == "write"
}

// combinedState is GitHub's state of a commit's statuses taken together: failure where any is a
// failure or an error, pending where any is pending or none was posted, and success otherwise.
func combinedState(statuses []object) string {
	state := "success"
	if len(statuses) == 0 {
		return "pending"
	}
	for _, status := range statuses {
		switch status["state"] {
		case "failure", "error":
			return "failure"
		case "pending":
			state = "pending"
		}
	}
	return state
}

// commitKey is the key a commit's check objects are held under: "owner/repo@sha".
func commitKey(r *http.Request) string {
	return r.PathValue("owner") + "/" + r.PathValue("repo") + "@" + r.PathValue("sha")
}

// serve records one request, answers it as a rate limit, as GitHub's own failure, or with its
// path's status when a test asked for that, and otherwise hands it to the routes above.
func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	_, route := f.mux.Handler(r)
	f.mu.Lock()
	f.requests = append(f.requests, fakeRequest{
		method:      r.Method,
		url:         r.URL.RequestURI(),
		route:       route,
		ifNoneMatch: r.Header.Get("If-None-Match"),
	})
	limited := f.limitEverything
	if left := f.limitOnce[r.URL.Path]; left > 0 {
		f.limitOnce[r.URL.Path] = left - 1
		limited = true
	}
	failed := false
	if left := f.failOnce[r.URL.Path]; left > 0 {
		f.failOnce[r.URL.Path] = left - 1
		failed = true
	}
	var reset time.Time
	if limited {
		// A limit's reset is GitHub's own moment, to the second the header carries, stamped here
		// on the passes' own clock, which the client reads the wait against
		// (githubapp.WithRateLimitClock, client): the wait is the reset less that clock's moment,
		// exactly.
		reset = time.Unix(f.clock.Now().Add(90*time.Second).Unix(), 0)
		f.reset = reset
	}
	released, held := f.holds[r.URL.Path]
	status := f.answerCodes[r.URL.Path]
	f.mu.Unlock()
	if held {
		// GitHub is slower to answer this than the pass has left. The answer waits until the test
		// releases the path, or until the caller gives up on the request, which closes its context
		// here: the pass's bound falling, or its caller cancelling it.
		f.held.Add(r.URL.Path)
		select {
		case <-released:
		case <-r.Context().Done():
			return
		}
	}
	if limited {
		// GitHub's own answer to an exhausted rate limit.
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
		return
	}
	if failed {
		http.Error(w, `{"message":"Server Error"}`, http.StatusBadGateway)
		return
	}
	if status != 0 {
		// No rate limit: the answer carries no limit header and no wording of one, so the
		// client reads the status alone.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"message":%q}`, http.StatusText(status))
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
	f.answerBody(w, r, body)
}

// answerBody is answer for a body encoded already (fullPage).
func (f *fakeGitHub) answerBody(w http.ResponseWriter, r *http.Request, body []byte) {
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

// appKeyPEM is the PEM of the one App key every client of this test binary signs with: each
// stand-in's and each replica's (fakeGitHub's client) and graphql_test.go's own. The stand-ins
// check no signature (the token mint answers any App JWT, and githubtest.Server compares only a
// bearer token), so one key changes no test's meaning, while a fresh 2048-bit key for every
// harness and again for every replica costs 80 to 400 ms each. TestMain makes it before any test
// runs, so no test's own time carries it.
var appKeyPEM = sync.OnceValues(func() (string, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})), nil
})

// client is a githubapp.Client on the binary's App key (appKeyPEM), pointed at this stand-in,
// obeying and recording the shared rate limits as every Dispatch process's client does. It reads
// a limit's wait against the passes' clock and bounds the limit's write on recordClock
// (clocktest.WithNow; recordClock says why), and its calls end at the test's give-up
// (githubapptest.RequestDeadlineAtTheGiveUp), not at githubapp's own RequestTimeout: so a call
// the stand-in holds (hold, holdQuery) ends where the test releases it, where the pass's bound or
// a caller's deadline falls, or where the caller cancels it.
func (f *fakeGitHub) client(limits githubapp.Limits) *githubapp.Client {
	f.t.Helper()
	text, err := appKeyPEM()
	if err != nil {
		f.t.Fatalf("generate App key: %v", err)
	}
	base := githubtest.Server(f.t, githubtest.Spec{Handle: f.serve})
	client, err := githubapp.New(&auth.AppConfig{ClientID: "Iv1.cireconcile", PEM: text}, base, githubapp.WithLimits(limits),
		githubapp.WithRateLimitClock(clocktest.WithNow(f.recordClock, f.clock.Now)),
		githubapp.WithRequestDeadline(githubapptest.RequestDeadlineAtTheGiveUp(f.t)))
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
	delete(f.fullRuns, repository+"@"+sha)
}

// setFullCheckRunPages makes every page of one commit's check runs answer with page, a full one,
// and total as the listing's total_count: GitHub reporting a full page however many pages are
// read, which a listing that does not end within maxPages is. The answer is the same for every
// page, so it is encoded once, and a read of a hundred pages costs one encoding of a page rather
// than a hundred. The GraphQL stand-in answers the commit as holding no check run; setCheckRuns
// gives it runs of its own again.
func (f *fakeGitHub) setFullCheckRunPages(repository, sha string, total int, page ...object) {
	body, err := json.Marshal(map[string]any{"total_count": total, "check_runs": page})
	if err != nil {
		f.t.Fatalf("encode the full page of check runs: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.runs, repository+"@"+sha)
	f.fullRuns[repository+"@"+sha] = fullPage{body: body, size: len(page)}
}

// setCheckSuites makes the check suites of one commit exactly these.
func (f *fakeGitHub) setCheckSuites(repository, sha string, suites ...object) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.suites[repository+"@"+sha] = suites
}

// setStatuses makes the commit statuses of one commit exactly these, each the newest of its
// context as GitHub's combined status lists it, which changes the ETag of the URL that lists them.
func (f *fakeGitHub) setStatuses(repository, sha string, statuses ...object) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses[repository+"@"+sha] = statuses
}

// setTokenPermissions makes every token the stand-in mints from now on hold exactly these
// permissions, as GitHub's mint answers them: the permissions the App's installation grants. A
// token minted before keeps the permissions it was minted with.
func (f *fakeGitHub) setTokenPermissions(permissions map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenPermissions = maps.Clone(permissions)
}

// refuseStatuses makes GitHub refuse one commit's statuses whatever the token holds: the query
// answers that head's status field null with an errors entry against its path, and the REST
// read of its statuses answers 403. It is a grant of Commit statuses taken away while a token
// minted with it lives.
func (f *fakeGitHub) refuseStatuses(repository, sha string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refusedStatuses[repository+"@"+sha] = struct{}{}
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

// failNext answers the next request to path with GitHub's 502 Bad Gateway, and no other request.
func (f *fakeGitHub) failNext(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failOnce[path]++
}

// hold holds back every answer to path until the test releases it (release) or the request is
// given up, as GitHub answering a poller more slowly than the pass has left does, and names each
// such request on held as it is held. A path already held stays held as it was.
func (f *fakeGitHub) hold(path string) {
	f.mu.Lock()
	holdIn(f.holds, path)
	f.mu.Unlock()
}

// release answers path as fast as every other path again, and lets each request held there go
// on to the answer its route gives.
func (f *fakeGitHub) release(path string) {
	f.mu.Lock()
	releaseIn(f.holds, path)
	f.mu.Unlock()
}

// holdIn holds back the answers at key of m, the paths (holds) or the pages (heldQueries) whose
// answers are held, until releaseIn lets them go. A key already held stays held as it was. The
// caller holds mu.
func holdIn(m map[string]chan struct{}, key string) {
	if _, held := m[key]; !held {
		m[key] = make(chan struct{})
	}
}

// releaseIn lets each request held at key of m go on to its answer, and holds key no longer. The
// caller holds mu.
func releaseIn(m map[string]chan struct{}, key string) {
	if released, held := m[key]; held {
		close(released)
		delete(m, key)
	}
}

// lastReset is the moment the stand-in's last rate-limited REST answer said the limit resets at.
func (f *fakeGitHub) lastReset() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reset
}

// answerStatus answers every request to path with status and GitHub's short message for it,
// in place of what its route would answer, as GitHub answers a read it gave up on (a 5xx) or
// refused (a 4xx). A status of zero answers the path from its route again.
func (f *fakeGitHub) answerStatus(path string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if status == 0 {
		delete(f.answerCodes, path)
		return
	}
	f.answerCodes[path] = status
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

// byRoute is how many of the requests the stand-in has been sent each of its routes answered,
// keyed by the route's pattern: what one pass costs GitHub, read one kind of request at a time.
func (f *fakeGitHub) byRoute() map[string]int {
	sent := map[string]int{}
	for _, request := range f.received() {
		sent[request.route]++
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
