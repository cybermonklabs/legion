package daemon

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/redact"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// sinkTokens mints oauth2calltest.Token, the made-up credential the requests of this file's rows
// carry (their GitHub client's, their Dispatch client's, and the implement App's lease), for every
// App and owner, so each GitHub read the daemon makes on a lease carries it.
type sinkTokens struct{}

func (sinkTokens) Token(context.Context, appauth.AppRole, string) (appauth.Lease, error) {
	return appauth.Lease{Token: oauth2calltest.Token, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (sinkTokens) ReadToken(context.Context, appauth.AppRole, string) (appauth.Lease, error) {
	return appauth.Lease{Token: oauth2calltest.Token, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

// sinkRoute answers one path of a row's stand-in.
type sinkRoute func(w http.ResponseWriter, r *http.Request)

// sinkServer is GitHub's REST API, or Dispatch's, or whatever answers in their place, for one row:
// each path routes names is answered by its route, every other one 404 with nothing echoed, and
// the Authorization header of every request is kept, in order.
type sinkServer struct {
	*httptest.Server
	mu      sync.Mutex
	headers []string
}

func newSinkServer(t *testing.T, routes map[string]sinkRoute) *sinkServer {
	t.Helper()
	s := &sinkServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.headers = append(s.headers, r.Header.Get("Authorization"))
		s.mu.Unlock()
		if route, ok := routes[r.URL.Path]; ok {
			route(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(s.Server.Close)
	return s
}

// carried is the Authorization header of each request the stand-in was asked, in order.
func (s *sinkServer) carried() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.headers)
}

// sinkClient is the implement App's client of acme/widgets on the stand-in, with
// oauth2calltest.Token.
func sinkClient(github *sinkServer) githubrest.Client {
	return githubrest.Client{Token: oauth2calltest.Token, API: githubrest.RepositoryAPI(github.URL, ghrepo.MustParse("acme/widgets"))}
}

// sinkAnswer answers 200 with body, a JSON document.
func sinkAnswer(body string) sinkRoute {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

// sinkInTurn answers a path's first request with the first of routes, its second with the
// second, and every one after the last with the last.
func sinkInTurn(routes ...sinkRoute) sinkRoute {
	var mu sync.Mutex
	asked := 0
	return func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		route := routes[min(asked, len(routes)-1)]
		asked++
		mu.Unlock()
		route(w, r)
	}
}

// sinkMessage answers status with GitHub's refusal body, its message message.
func sinkMessage(w http.ResponseWriter, status int, message string) {
	body, _ := json.Marshal(map[string]string{"message": message})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// sinkRefusal refuses a request with status, which is not GitHub's rate limit (a 404 or a 500),
// echoing the request's Authorization header verbatim as the body's message and as the two
// rate-limit headers GitHub writes as text, X-RateLimit-Resource and a Retry-After that is no
// date (githubrest's limitHeaders reads both through oauth2call.Header).
func sinkRefusal(status int) sinkRoute {
	return func(w http.ResponseWriter, r *http.Request) {
		echo := r.Header.Get("Authorization")
		w.Header().Set("X-RateLimit-Resource", echo)
		w.Header().Set("Retry-After", echo)
		sinkMessage(w, status, echo)
	}
}

// sinkLimit refuses a request with GitHub's 403 for a spent primary rate limit - no call left
// (X-RateLimit-Remaining: 0) until a reset half an hour ahead - echoing the request's
// Authorization header as sinkRefusal does: as the message and as X-RateLimit-Resource and
// Retry-After. The Retry-After makes the limit GitHub's secondary one to githubrest (spent).
func sinkLimit() sinkRoute { return sinkSpent(true) }

// sinkPrimaryLimit is sinkLimit with no Retry-After: GitHub's primary limit, whose budget is the
// resource X-RateLimit-Resource names (githubrest's spent), here the echo.
func sinkPrimaryLimit() sinkRoute { return sinkSpent(false) }

// sinkSpent is sinkLimit, echoing the request's Authorization header as a Retry-After too where
// retryAfter says so.
func sinkSpent(retryAfter bool) sinkRoute {
	return func(w http.ResponseWriter, r *http.Request) {
		echo := r.Header.Get("Authorization")
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(30*time.Minute).Unix(), 10))
		w.Header().Set("X-RateLimit-Resource", echo)
		if retryAfter {
			w.Header().Set("Retry-After", echo)
		}
		sinkMessage(w, http.StatusForbidden, echo)
	}
}

// sinkPlanRefusal is GitHub's 403 that refuses rulesets to a private repository whose plan has
// none (planRefusal), its message echoing the request's Authorization header before GitHub's
// words, and X-RateLimit-Resource echoing it. It sends no Retry-After: one would make the 403
// GitHub's rate limit, which the rules read logs at Warn.
func sinkPlanRefusal() sinkRoute {
	return func(w http.ResponseWriter, r *http.Request) {
		echo := r.Header.Get("Authorization")
		w.Header().Set("X-RateLimit-Resource", echo)
		sinkMessage(w, http.StatusForbidden, echo+" Upgrade to GitHub Pro or make this repository public to enable this feature.")
	}
}

// sinkDispatchRefusal is Dispatch's 404 ISSUE_NOT_FOUND, which no retry changes, its sentence
// echoing the request's Authorization header verbatim.
func sinkDispatchRefusal() sinkRoute {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := json.Marshal(map[string]string{"code": dispatch.IssueNotFound, "error": r.Header.Get("Authorization")})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write(body)
	}
}

// sinkPull is pull request 86 as GitHub answers it, open on legion/CAPTURE-1 at a head the record
// of CAPTURE-1 (seedCapture, at code) has not recorded, changed after the record's clock.
const sinkPull = `{"number":86,"state":"open","html_url":"https://github.com/acme/widgets/pull/86","body":"Dispatch: CAPTURE-1",` +
	`"base":{"ref":"main"},"head":{"ref":"legion/CAPTURE-1","sha":"` + realCommit + `"},"updated_at":"2026-10-26T20:00:00Z","mergeable":true,"mergeable_state":"clean"}`

// sinkRepository is acme/widgets as GitHub names it, at main.
const sinkRepository = `{"full_name":"acme/widgets","default_branch":"main"}`

// sinkLog is a row's log: the handler `legion start` builds (slog.NewJSONHandler with
// redact.HandlerOptions, cmd/legion's start) writing the row's lines, and each record as its call
// passed it, before that handler's rule reads it, so the row holds the value its sink logged.
type sinkLog struct {
	lines syncBuffer
	mu    sync.Mutex
	kept  []slog.Record
}

func (l *sinkLog) logger() *slog.Logger {
	return slog.New(sinkHandler{Handler: slog.NewJSONHandler(&l.lines, redact.HandlerOptions(nil)), log: l})
}

// holds says whether a record of message was logged.
func (l *sinkLog) holds(message string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.ContainsFunc(l.kept, func(r slog.Record) bool { return r.Message == message })
}

// values is what the first record of message logged under each of keys, as its call passed it
// (nil for a key it did not log), and nil when no record of message was logged.
func (l *sinkLog) values(message string, keys ...string) []any {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.kept {
		if r.Message != message {
			continue
		}
		values := make([]any, len(keys))
		r.Attrs(func(a slog.Attr) bool {
			if i := slices.Index(keys, a.Key); i >= 0 && values[i] == nil {
				values[i] = a.Value.Any()
			}
			return true
		})
		return values
	}
	return nil
}

// sinkHandler keeps each record in its log, then hands it to the handler it wraps.
type sinkHandler struct {
	slog.Handler
	log *sinkLog
}

func (h sinkHandler) Handle(ctx context.Context, r slog.Record) error {
	h.log.mu.Lock()
	h.log.kept = append(h.log.kept, r.Clone())
	h.log.mu.Unlock()
	return h.Handler.Handle(ctx, r)
}

func (h sinkHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return sinkHandler{Handler: h.Handler.WithAttrs(attrs), log: h.log}
}

func (h sinkHandler) WithGroup(name string) slog.Handler {
	return sinkHandler{Handler: h.Handler.WithGroup(name), log: h.log}
}

// sinkLines is each line of logs, the daemon's JSON log, whose message is message at level.
func sinkLines(logs, message, level string) []string {
	var lines []string
	for line := range strings.SplitSeq(logs, "\n") {
		var decoded struct{ Msg, Level string }
		if json.Unmarshal([]byte(line), &decoded) == nil && decoded.Msg == message && decoded.Level == level {
			lines = append(lines, line)
		}
	}
	return lines
}

// sinkReadRuntime is readRuntime over pool reading github, whose every lease is
// oauth2calltest.Token.
func sinkReadRuntime(pool *store.Pool, github *sinkServer, log *slog.Logger) *workflowRuntime {
	w := readRuntime(pool, github.URL, log)
	w.tokens = sinkTokens{}
	return w
}

// seededCapture is a pool holding CAPTURE-1 in testing with its pull request 86 open at head code
// (seedCapture).
func seededCapture(t *testing.T) *store.Pool {
	t.Helper()
	pool := isolatedOutboxPool(t)
	seedCapture(t, pool, phase.Testing, "testing", record.PhaseRow{Issue: "CAPTURE-1", Role: claim.RoleTester, Claim: "test-claim"})
	return pool
}

// sinkAppTokens is the review App, keyed by a key made for the row, installed as installation 7
// on acme by configuration, minting on github: the token source the daemon builds (appauth.New).
func sinkAppTokens(t *testing.T, github *sinkServer) *appauth.Manager {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privatePEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	tokens := appauth.New(config.GitHubApps{Review: config.GitHubApp{AppID: "2", PrivateKey: privatePEM, Installations: map[string]string{"acme": "7"}}},
		appauth.Options{BaseURL: github.URL, HTTPClient: github.Client(), Stderr: io.Discard, Log: quietLogger()})
	t.Cleanup(tokens.Close)
	return tokens
}

// appJWT is the review App's JWT, the credential its installation read carried: the bearer the
// stand-in was asked with.
func appJWT(t *testing.T, github *sinkServer) string {
	t.Helper()
	carried := github.carried()
	if len(carried) == 0 {
		t.Fatal("the review App asked GitHub nothing")
	}
	return strings.TrimPrefix(carried[0], "Bearer ")
}

// sinkRow is one sink: the log line CodeQL names, its level, the attributes its value is logged
// under, what the line must hold of the refusal, and drive, which makes the request that carries
// the row's credential through the code that holds the line, logging to log, and answers that
// credential and the stand-in it was sent to.
type sinkRow struct {
	name    string
	capture string
	message string
	level   string
	keys    []string
	holds   []string
	drive   func(t *testing.T, log *sinkLog) (string, *sinkServer)
}

// TestTheClearTextLoggingSinksLogNoCredentialTheRequestCarried: each row is one of the daemon's
// log lines CodeQL's go/clear-text-logging names a sink of, driven through the code that holds it.
// The request the line's value is read from carries a made-up credential - oauth2calltest.Token,
// or the review App's JWT where the read is the App's own - and GitHub, Dispatch, or whatever
// answers in their place refuses it, echoing the request's Authorization header verbatim in its
// body and in each rate-limit header it writes as text. The credential so sits in the kept field
// of what the answer said (oauth2call.ReadAnswer's RemoteText: githubrest.Answer's Body and Limit,
// dispatch.Error's Said and Message, the body words of appauth's refusal), and every printer of
// that field subtracts it (RemoteText.String, through redact.Tokens). The line, written through
// the handler `legion start` builds (redact.HandlerOptions), names the refusal - its route and
// status, and "[redacted]" where the echo stood, which says the credential reached the value
// logged - and holds no run of the credential. Each row keeps the value its sink logged (kept) and
// the echo as a plain string (twin), and prints each kept value under every fmt verb and a bad
// verb, at every holding, beside twin (oauth2calltest.KeptPrintTest). Captures
// sink/daemon/<file>/<function>, and kept/daemon/<file>/<function>/<key>/<holding>/<verb>.
func TestTheClearTextLoggingSinksLogNoCredentialTheRequestCarried(t *testing.T) {
	for _, row := range []sinkRow{
		{
			// unreadRules' Unread of a rules read GitHub answered 404: GitHub's message, in Answer's
			// decoded refusal (oauth2call.GitHubRefusal.Message).
			name: "base_rules.go:readBaseRules github: read the base's rules (Warn)", capture: "sink/daemon/base_rules.go/readBaseRules/unread",
			message: rulesReadLine, level: "WARN", keys: []string{"unread"},
			holds: []string{"GitHub answered GET /rules/branches/main with 404", "[redacted]"},
			drive: func(t *testing.T, log *sinkLog) (string, *sinkServer) {
				github := newSinkServer(t, map[string]sinkRoute{"/repos/acme/widgets/rules/branches/main": sinkRefusal(http.StatusNotFound)})
				w := &workflowRuntime{log: log.logger()}
				if rules := w.readBaseRules(context.Background(), sinkClient(github), record.PullRequest{Repo: "acme/widgets", Number: 86},
					newGitHubText("main", oauth2calltest.Said("main"))); rules.Unread == "" {
					t.Fatalf("the rules read came to %+v, want them unread", rules)
				}
				return oauth2calltest.Token, github
			},
		},
		{
			// unreadRules of GitHub's 403 refusing rulesets to the plan: no rules, and an Unread of
			// "", which the line logs at Info.
			name: "base_rules.go:readBaseRules github: read the base's rules (Info)", capture: "sink/daemon/base_rules.go/readBaseRules/read",
			message: rulesReadLine, level: "INFO", keys: []string{"unread"}, holds: []string{`"unread":""`},
			drive: func(t *testing.T, log *sinkLog) (string, *sinkServer) {
				github := newSinkServer(t, map[string]sinkRoute{"/repos/acme/widgets/rules/branches/main": sinkPlanRefusal()})
				w := &workflowRuntime{log: log.logger()}
				if rules := w.readBaseRules(context.Background(), sinkClient(github), record.PullRequest{Repo: "acme/widgets", Number: 86},
					newGitHubText("main", oauth2calltest.Said("main"))); rules.Unread != "" || len(rules.Required) != 0 {
					t.Fatalf("the rules read came to %+v, want no rules, read", rules)
				}
				return oauth2calltest.Token, github
			},
		},
		{
			// readReviews' failed list of reviews, GitHub's rate limit: the *githubrest.Answer.
			name: "github_reads.go:logReadFailure read a pull request's reviews: GitHub's rate limit", capture: "sink/daemon/github_reads.go/logReadFailure/limited",
			message: "read a pull request's reviews: GitHub's rate limit; this read asks GitHub no more", level: "ERROR", keys: []string{"error"},
			holds: []string{"GET /pulls/86/reviews with 403", "[redacted]"},
			drive: func(t *testing.T, log *sinkLog) (string, *sinkServer) {
				github := newSinkServer(t, map[string]sinkRoute{"/repos/acme/widgets/pulls/86/reviews": sinkLimit()})
				w := &workflowRuntime{log: log.logger()}
				ctx := context.Background()
				if !w.readReviews(ctx, ctx, sinkClient(github), record.PullRequest{Repo: "acme/widgets", Number: 86, Issue: "CAPTURE-1"}) {
					t.Fatal("the reviews read went on through GitHub's rate limit, want it stopped")
				}
				return oauth2calltest.Token, github
			},
		},
		{
			// readReviews' failed list of reviews, GitHub's 500: the *githubrest.Answer.
			name: "github_reads.go:logReadFailure read a pull request's reviews; a decision", capture: "sink/daemon/github_reads.go/logReadFailure/failed",
			message: "read a pull request's reviews; a decision its webhook never delivered stays unread until a later read of the issue reads it", level: "ERROR",
			keys: []string{"error"}, holds: []string{"GET /pulls/86/reviews with 500", "[redacted]"},
			drive: func(t *testing.T, log *sinkLog) (string, *sinkServer) {
				github := newSinkServer(t, map[string]sinkRoute{"/repos/acme/widgets/pulls/86/reviews": sinkRefusal(http.StatusInternalServerError)})
				w := &workflowRuntime{log: log.logger()}
				ctx := context.Background()
				if w.readReviews(ctx, ctx, sinkClient(github), record.PullRequest{Repo: "acme/widgets", Number: 86, Issue: "CAPTURE-1"}) {
					t.Fatal("the reviews read stopped on GitHub's 500, want it to go on")
				}
				return oauth2calltest.Token, github
			},
		},
		{
			// followHead's error: the branch's activity read GitHub's rate limit refused, which ends
			// the follow (lostPushes, endsRead).
			name: "issue_reads.go:readIssuePullRequest follow the head GitHub shows", capture: "sink/daemon/issue_reads.go/readIssuePullRequest",
			message: "follow the head GitHub shows for an issue's pull request; this read judges it at the recorded head", level: "WARN", keys: []string{"error"},
			holds: []string{"read the activity of legion/CAPTURE-1", "GET /activity with 403", "[redacted]"},
			drive: func(t *testing.T, log *sinkLog) (string, *sinkServer) {
				github := newSinkServer(t, map[string]sinkRoute{
					"/repos/acme/widgets/pulls/86":         sinkAnswer(sinkPull),
					"/repos/acme/widgets/activity":         sinkLimit(),
					"/repos/acme/widgets/pulls/86/reviews": sinkAnswer(`[]`),
				})
				readCapture(t, sinkReadRuntime(seededCapture(t), github, log.logger()))
				return oauth2calltest.Token, github
			},
		},
		{
			// lostPushes' reason for a branch's activity GitHub answered 500: the read's error,
			// printed into the reason.
			name: "pull_request_reads.go:headPushes workflow: the read cannot rebuild from GitHub's activity", capture: "sink/daemon/pull_request_reads.go/headPushes",
			message: "workflow: the read cannot rebuild from GitHub's activity the pushes that moved a pull request's head; it applies none of them, and the synchronize",
			level:   "WARN", keys: []string{"reason"},
			holds: []string{"the read of the branch's activity failed", "GET /activity with 500", "[redacted]"},
			drive: func(t *testing.T, log *sinkLog) (string, *sinkServer) {
				github := newSinkServer(t, map[string]sinkRoute{
					"/repos/acme/widgets/pulls/86":         sinkAnswer(sinkPull),
					"/repos/acme/widgets/activity":         sinkRefusal(http.StatusInternalServerError),
					"/repos/acme/widgets/pulls/86/reviews": sinkAnswer(`[]`),
				})
				readCapture(t, sinkReadRuntime(seededCapture(t), github, log.logger()))
				return oauth2calltest.Token, github
			},
		},
		{
			// readIssuePullRequest's error at the start: the pull request read GitHub's rate limit
			// refused.
			name: "issue_reads.go:readIssuesAtStart read an issue's pull request as the daemon starts: GitHub's rate limit", capture: "sink/daemon/issue_reads.go/readIssuesAtStart/limited",
			message: "read an issue's pull request as the daemon starts: GitHub's rate limit; the start reads no more issues, and each issue's next handoff reads it",
			level:   "ERROR", keys: []string{"error"}, holds: []string{"GET /pulls/86 with 403", "[redacted]"},
			drive: func(t *testing.T, log *sinkLog) (string, *sinkServer) {
				github := newSinkServer(t, map[string]sinkRoute{"/repos/acme/widgets/pulls/86": sinkLimit()})
				sinkReadRuntime(seededCapture(t), github, log.logger()).readIssuesAtStart(context.Background())
				return oauth2calltest.Token, github
			},
		},
		{
			// readIssuePullRequest's error at the start: the pull request read GitHub answered 500.
			name: "issue_reads.go:readIssuesAtStart read an issue's pull request as the daemon starts; the issue's next handoff", capture: "sink/daemon/issue_reads.go/readIssuesAtStart/failed",
			message: "read an issue's pull request as the daemon starts; the issue's next handoff reads it", level: "WARN", keys: []string{"error"},
			holds: []string{"GET /pulls/86 with 500", "[redacted]"},
			drive: func(t *testing.T, log *sinkLog) (string, *sinkServer) {
				github := newSinkServer(t, map[string]sinkRoute{"/repos/acme/widgets/pulls/86": sinkRefusal(http.StatusInternalServerError)})
				sinkReadRuntime(seededCapture(t), github, log.logger()).readIssuesAtStart(context.Background())
				return oauth2calltest.Token, github
			},
		},
		{
			// readIssuePullRequest's error for a handoff's read: the pull request read GitHub
			// answered 500.
			name: "issue_reads.go:readHandedOff read the pull request of an issue whose agent handed off", capture: "sink/daemon/issue_reads.go/readHandedOff",
			message: "read the pull request of an issue whose agent handed off; the record stands as it was until the next read", level: "WARN", keys: []string{"error"},
			holds: []string{"GET /pulls/86 with 500", "[redacted]"},
			drive: func(t *testing.T, log *sinkLog) (string, *sinkServer) {
				github := newSinkServer(t, map[string]sinkRoute{"/repos/acme/widgets/pulls/86": sinkRefusal(http.StatusInternalServerError)})
				w := sinkReadRuntime(seededCapture(t), github, log.logger())
				w.handedOff = newWakeSet[string]()
				ctx, cancel := context.WithCancel(context.Background())
				stopped := make(chan struct{})
				go func() {
					w.readHandedOff(ctx)
					close(stopped)
				}()
				w.handoffCompleted("CAPTURE-1")
				testwait.Eventually(t, "the handoff's read to fail and say so", func() bool {
					return log.holds("read the pull request of an issue whose agent handed off; the record stands as it was until the next read")
				})
				cancel()
				<-stopped
				return oauth2calltest.Token, github
			},
		},
		{
			// readMergeState's error in a climb the workflow asked for: the pull request read
			// GitHub answered 500.
			name: "merge_state_reads.go:climbMergeState read GitHub's merge state for an issue whose read was asked for", capture: "sink/daemon/merge_state_reads.go/climbMergeState",
			message: "read GitHub's merge state for an issue whose read was asked for; the record stands as it was until the next read", level: "WARN", keys: []string{"error"},
			holds: []string{"GET /pulls/86 with 500", "[redacted]"},
			drive: func(t *testing.T, log *sinkLog) (string, *sinkServer) {
				github := newSinkServer(t, map[string]sinkRoute{"/repos/acme/widgets/pulls/86": sinkRefusal(http.StatusInternalServerError)})
				w := sinkReadRuntime(seededCapture(t), github, log.logger())
				w.mergeStateRetry = 10 * time.Millisecond
				sinkLadder(t, w, func() { w.queueMergeState("CAPTURE-1") }, func() bool {
					running, _ := ladders(w)
					return running == 0 && log.holds("read GitHub's merge state for an issue whose read was asked for; the record stands as it was until the next read")
				})
				return oauth2calltest.Token, github
			},
		},
		{
			// readChunk's error in the reads of a base, which take up a follow of head code after its
			// ladder (followOnBase): the merge-state query GitHub answered 500.
			name: "merge_state_batches.go:readChunk github: read the merge state of the open pull requests on a base whose reads were asked for", capture: "sink/daemon/merge_state_batches.go/readChunk",
			message: sinkBaseReadFailed, level: "WARN", keys: []string{"error"},
			holds: []string{"POST /graphql with 500", "[redacted]"},
			drive: func(t *testing.T, log *sinkLog) (string, *sinkServer) {
				github := newSinkServer(t, map[string]sinkRoute{"/graphql": sinkRefusal(http.StatusInternalServerError)})
				pool := seededCapture(t)
				w := sinkReadRuntime(pool, github, log.logger())
				w.mergeStateRetry = 10 * time.Millisecond
				follow := recordedPull(t, pool, "CAPTURE-1")
				follow.Base = "main"
				sinkLadder(t, w, func() { w.queueMergeStateFollow("CAPTURE-1", follow) }, func() bool { return log.holds(sinkBaseReadFailed) })
				return oauth2calltest.Token, github
			},
		},
		{
			// recordDropped's cause, Dispatch's refusal of a step record's PUT: the *dispatch.Error
			// (its Said and Message, oauth2call.CodedRefusal's Error).
			name: "outbox.go:recordDropped outbox step record refused by Dispatch and dropped", capture: "sink/daemon/outbox.go/recordDropped",
			message: stepRefusedLine, level: "WARN", keys: []string{"error"},
			holds: []string{dispatch.IssueNotFound, "[redacted]"},
			drive: func(t *testing.T, log *sinkLog) (string, *sinkServer) {
				rig := newStepRecordRig(t, nil)
				server := newSinkServer(t, map[string]sinkRoute{"/api/v1/issues/" + stepIssue + "/steps/" + stepRecordID: sinkDispatchRefusal()})
				rig.runner.log = log.logger()
				rig.runner.dispatch = dispatch.New(server.URL, dispatch.BearerTransport(oauth2calltest.Token, nil))
				rig.run()
				if left := rig.left(); left != 0 {
					t.Fatalf("rows left after Dispatch refused the record = %d, want none", left)
				}
				return oauth2calltest.Token, server
			},
		},
		{
			// defaultBranches.Named's error for a branch held through GitHub's rate limit, read
			// again after it: ghbranch.Read's refusal, GitHub's 500.
			name: "repositories.go:rereadHeldBranches workflow: the read of a default branch held through GitHub's rate limit failed", capture: "sink/daemon/repositories.go/rereadHeldBranches",
			message: "workflow: the read of a default branch held through GitHub's rate limit failed after the limit, so the next read of it asks GitHub again",
			level:   "WARN", keys: []string{"error"}, holds: []string{"read the default branch of acme/widgets", "with 500", "[redacted]"},
			drive: func(t *testing.T, log *sinkLog) (string, *sinkServer) {
				github := newSinkServer(t, map[string]sinkRoute{"/repos/acme/widgets": sinkInTurn(sinkAnswer(sinkRepository), sinkLimit(), sinkRefusal(http.StatusInternalServerError))})
				w, clock := sinkHeldRead(t, github, log)
				clock.advance(2 * time.Hour)
				w.rereadHeldBranches(context.Background(), clock.Now())
				return oauth2calltest.Token, github
			},
		},
		{
			// reconcileListing's error for a recovery's listing: the todo issue's preparation, whose
			// read of its repository's default branch GitHub's rate limit refused (prepareRead's
			// intake.RetryLater). The repository is one to recover, which GitHub answered once
			// since (readAnswered), not one still unread: the recovery defers an unrecorded issue
			// on an unread repository to a read of its own (deferUnreadIssues) and prepares it in
			// no listing.
			name: "repositories.go:recoverRepositories workflow: the issues of the repositories to recover were not recovered", capture: "sink/daemon/repositories.go/recoverRepositories",
			message: "workflow: the issues of the repositories to recover were not recovered from Dispatch's listing; recovering them again",
			level:   "WARN", keys: []string{"error"},
			holds: []string{"prepare the repository of CAPTURE-2 for admission", "GitHub's rate limit", "with 403", "[redacted]"},
			drive: func(t *testing.T, log *sinkLog) (string, *sinkServer) {
				github := newSinkServer(t, map[string]sinkRoute{"/repos/acme/widgets": sinkLimit()})
				listing := newSinkServer(t, map[string]sinkRoute{"/api/v1/issues": sinkAnswer(`[{"key":"CAPTURE-2","title":"CAPTURE-2","status":"todo","labels":["legion"],"resolved_repo":"acme/widgets"}]`)})
				repos := newRepositories("CAPTURE", ghrepo.Renames{})
				w := &workflowRuntime{pool: isolatedOutboxPool(t), records: projectRecords{Store: record.NewStore(), project: "CAPTURE"}, dispatchProject: "CAPTURE",
					dispatch: dispatch.New(listing.URL, dispatch.BearerTransport(oauth2calltest.Token, nil)), installs: &installations{}, repositories: repos, log: log.logger(),
					defaultBranches: newDefaultBranches(sinkTokens{}, repos, github.URL, defaultBranchTTL, time.Now)}
				w.leaveForRecovery(ghrepo.MustParse("acme/widgets"))
				w.readAnswered(ghrepo.MustParse("acme/widgets"))
				w.recoverRepositories(context.Background())
				return oauth2calltest.Token, github
			},
		},
		{
			// defaultBranches.Named's error for the repository an issue is recorded on, read again:
			// ghbranch.Read's refusal, GitHub's 500.
			name: "repositories.go:prepareUnrecorded workflow: the default branch of a repository an issue is recorded on", capture: "sink/daemon/repositories.go/prepareUnrecorded",
			message: "workflow: the default branch of a repository an issue is recorded on was not read again, so the issue is recorded at the branch last read",
			level:   "WARN", keys: []string{"error"}, holds: []string{"read the default branch of acme/widgets", "with 500", "[redacted]"},
			drive: func(t *testing.T, log *sinkLog) (string, *sinkServer) {
				github := newSinkServer(t, map[string]sinkRoute{"/repos/acme/widgets": sinkRefusal(http.StatusInternalServerError)})
				w := &workflowRuntime{log: log.logger(), repositories: servedAcme()}
				w.defaultBranches = newDefaultBranches(sinkTokens{}, w.repositories, github.URL, defaultBranchTTL, time.Now)
				if _, err := w.prepareUnrecorded(context.Background(), intake.DispatchIssue{Key: "CAPTURE-2", Seq: 1, Status: "todo", HandedOver: true, ResolvedRepo: "acme/widgets"}, false); err != nil {
					t.Fatalf("prepare a todo issue on acme/widgets = %v, want it prepared on the branch last read", err)
				}
				return oauth2calltest.Token, github
			},
		},
		{
			// limitAttributes of a read GitHub's rate limit refused, served on the branch read
			// before (defaultBranches.Named, onHold): the read's error and the limit's headers kept
			// as text (githubrest.LimitHeaders' Resource and RetryAfter, oauth2call.Header). The
			// echoed Retry-After makes the limit GitHub's secondary one (githubrest's spent).
			name: "repository_limits.go:heldThroughLimit workflow: GitHub's rate limit refused the read of a repository's default branch (secondary)", capture: "sink/daemon/repository_limits.go/heldThroughLimit/secondary",
			message: "workflow: GitHub's rate limit refused the read of a repository's default branch, so the daemon serves it on the default branch it holds and reads it again after the limit's reset",
			level:   "WARN", keys: []string{"error", "rateLimitResource", "retryAfter", "budget"},
			holds: []string{`"budget":"secondary"`, `"rateLimitRemaining":"0"`, "with 403", "[redacted]"},
			drive: func(t *testing.T, log *sinkLog) (string, *sinkServer) {
				github := newSinkServer(t, map[string]sinkRoute{"/repos/acme/widgets": sinkInTurn(sinkAnswer(sinkRepository), sinkLimit())})
				sinkHeldRead(t, github, log)
				return oauth2calltest.Token, github
			},
		},
		{
			// limitAttributes as for the secondary limit, of GitHub's primary limit with no
			// Retry-After, whose X-RateLimit-Resource, the echo, names no resource GitHub
			// documents: the budget githubrest read the limit as spending is then the core budget
			// (githubrest's spent), and the header is printed only as the rule reads it.
			name: "repository_limits.go:heldThroughLimit workflow: GitHub's rate limit refused the read of a repository's default branch (primary)", capture: "sink/daemon/repository_limits.go/heldThroughLimit/primary",
			message: "workflow: GitHub's rate limit refused the read of a repository's default branch, so the daemon serves it on the default branch it holds and reads it again after the limit's reset",
			level:   "WARN", keys: []string{"error", "rateLimitResource", "budget"},
			holds: []string{`"budget":"core"`, `"rateLimitRemaining":"0"`, `"retryAfter":""`, "with 403", "[redacted]"},
			drive: func(t *testing.T, log *sinkLog) (string, *sinkServer) {
				github := newSinkServer(t, map[string]sinkRoute{"/repos/acme/widgets": sinkInTurn(sinkAnswer(sinkRepository), sinkPrimaryLimit())})
				sinkHeldRead(t, github, log)
				return oauth2calltest.Token, github
			},
		},
		{
			// The review App's mint refused as not installed: its installation's permissions read
			// GitHub answered 404, whose body words the error carries (appauth's refusalBody).
			name: "review_body.go:reviewBody workflow: the review App is not installed on the owner", capture: "sink/daemon/review_body.go/reviewBody",
			message: "workflow: the review App is not installed on the owner GitHub holds its review's repository under, so its review's body is read as delivered",
			level:   "ERROR", keys: []string{"error"}, holds: []string{appauth.ErrNotInstalled.Error(), "permissions read failed (404)", "[redacted]"},
			drive: func(t *testing.T, log *sinkLog) (string, *sinkServer) {
				github := newSinkServer(t, map[string]sinkRoute{"/app/installations/7": sinkRefusal(http.StatusNotFound)})
				w := reviewPermissionRuntime(reviewPermissionPool(t), github.URL, log.logger())
				w.tokens = sinkAppTokens(t, github)
				review := intake.PullRequestReview{Repo: "acme/widgets", Number: 42, ID: 5, State: "approved", Author: w.reviewAppLogin, BodyTruncated: true, Body: "delivered body"}
				if body, err := w.reviewBody(context.Background(), review); err != nil || body != "delivered body" {
					t.Fatalf("the review App's own review body = %q, %v; want the delivered body and no error", body, err)
				}
				return appJWT(t, github), github
			},
		},
		{
			// The review App's mint refused as not installed, as for reviewBody.
			name: "review_permission.go:readPermission workflow: the review App is not installed on the owner", capture: "sink/daemon/review_permission.go/readPermission",
			message: "workflow: the review App is not installed on the owner GitHub holds the review's repository under, so the review decides nothing; install the review App there for its reviews to decide a round",
			level:   "ERROR", keys: []string{"error"}, holds: []string{appauth.ErrNotInstalled.Error(), "permissions read failed (404)", "[redacted]"},
			drive: func(t *testing.T, log *sinkLog) (string, *sinkServer) {
				github := newSinkServer(t, map[string]sinkRoute{"/app/installations/7": sinkRefusal(http.StatusNotFound)})
				w := reviewPermissionRuntime(reviewPermissionPool(t), github.URL, log.logger())
				w.tokens = sinkAppTokens(t, github)
				review := intake.PullRequestReview{Repo: "acme/widgets", Number: 42, State: "approved", Author: "a-writer"}
				if canWrite, err := w.reviewerCanWrite(context.Background(), review); err != nil || canWrite {
					t.Fatalf("a review on an owner with no review App = %v, %v; want no write access and no error", canWrite, err)
				}
				return appJWT(t, github), github
			},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			log := &sinkLog{}
			credential, server := row.drive(t, log)
			if len(credential) < 8 {
				t.Fatalf("the row planted %q, want a credential of at least 8 characters, which the check reads runs of", credential)
			}
			// twin is the echo the stand-in wrote back, as a plain string: the request's
			// Authorization header, which every request the stand-in was asked carried.
			twin := "Bearer " + credential
			carried := server.carried()
			if len(carried) == 0 {
				t.Fatal("the stand-in was asked nothing")
			}
			for _, header := range carried {
				if header != twin {
					t.Fatalf("the stand-in was asked with Authorization %q, want %q", header, twin)
				}
			}
			lines := sinkLines(log.lines.String(), row.message, row.level)
			if len(lines) == 0 {
				t.Fatalf("the daemon logged no %s line %q:\n%s", row.level, row.message, log.lines.String())
			}
			logged := strings.Join(lines, "\n")
			requireHolds(t, logged, row.holds...)
			oauth2calltest.RequireNoPiece(t, row.capture, logged, credential)
			// kept is the value the sink logged, as its call passed it, under each of row.keys.
			kept := log.values(row.message, row.keys...)
			if len(kept) != len(row.keys) {
				t.Fatalf("the sink's record logged %v under %q, want a value under each", kept, row.keys)
			}
			for i, value := range kept {
				if value == nil {
					t.Fatalf("the sink's record logged nothing under %q", row.keys[i])
				}
				oauth2calltest.KeptPrintTest(t, strings.TrimPrefix(row.capture, "sink/")+"/"+row.keys[i], value, twin, credential)
			}
		})
	}
}

// sinkBaseReadFailed is the line readChunk logs when the merge-state query of a base's reads
// failed.
const sinkBaseReadFailed = "github: read the merge state of the open pull requests on a base whose reads were asked for; their records stand as they were, and they are read again"

// sinkLadder runs w's merge-state reader until done holds, after queue asks it for a ladder, then
// stops it and waits for every ladder it started to end.
func sinkLadder(t *testing.T, w *workflowRuntime, queue func(), done func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		w.readMergeStates(ctx)
		close(stopped)
	}()
	queue()
	testwait.Eventually(t, "the ladder's read to fail and say so", done)
	cancel()
	<-stopped
	testwait.Eventually(t, "every ladder to end", func() bool {
		running, _ := ladders(w)
		return running == 0
	})
}

// sinkHeldRead is a runtime whose default branches github answers, wired as the boot wires them
// (heldThroughLimit told of each read served on a branch held), on a clock it answers: the
// repository's first read is answered, and its next, a ttl later, is refused by GitHub's rate limit
// and served on the branch the first read kept.
func sinkHeldRead(t *testing.T, github *sinkServer, log *sinkLog) (*workflowRuntime, *heldClock) {
	t.Helper()
	clock := microClock()
	w := &workflowRuntime{log: log.logger()}
	w.defaultBranches = newDefaultBranches(sinkTokens{}, nil, github.URL, defaultBranchTTL, clock.Now)
	w.defaultBranches.onHold = w.heldThroughLimit
	repo := ghrepo.MustParse("acme/widgets")
	if _, err := w.defaultBranches.Named(context.Background(), repo); err != nil {
		t.Fatalf("the first read of acme/widgets: %v", err)
	}
	clock.advance(defaultBranchTTL + time.Minute)
	if held, err := w.defaultBranches.Named(context.Background(), repo); err != nil || held.DefaultBranch != "main" {
		t.Fatalf("the read GitHub's rate limit refused = %+v, %v; want main, held", held, err)
	}
	return w, clock
}
