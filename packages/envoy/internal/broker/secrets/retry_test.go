package secrets_test

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/metrics"
)

// retryClock is the clock a test sets for a Retry: Sleep records each wait and moves the clock on
// by it, and no real time passes.
type retryClock struct {
	mu    sync.Mutex
	now   time.Time
	waits []time.Duration
}

func (c *retryClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *retryClock) Sleep(ctx context.Context, wait time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.waits = append(c.waits, wait)
	c.now = c.now.Add(wait)
	return nil
}

// slept is every wait the Retry waited, in order.
func (c *retryClock) slept() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.waits)
}

// testRetry is a Retry on a clock the test sets, drawing its jitter with draw (the real draw when
// nil), and counting its attempts on a registry of its own.
func testRetry(draw func(time.Duration) time.Duration) (*secrets.Retry, *retryClock, *metrics.Registry) {
	clock := &retryClock{now: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)}
	registry := metrics.New()
	retry := secrets.NewRetry(registry)
	retry.Now, retry.Sleep, retry.Draw = clock.Now, clock.Sleep, draw
	return retry, clock, registry
}

// atTheTop is a jitter whose draw is the top of its range: the ceiling itself.
func atTheTop(ceiling time.Duration) time.Duration { return ceiling }

// atTheBottom is a jitter whose draw is no wait at all, so a wait a row records is the server's.
func atTheBottom(time.Duration) time.Duration { return 0 }

// attemptSamples is every sample of broker_mint_attempts_total the registry serves.
func attemptSamples(registry *metrics.Registry) string {
	recorder := httptest.NewRecorder()
	registry.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	var samples strings.Builder
	for line := range strings.Lines(recorder.Body.String()) {
		if strings.HasPrefix(line, "broker_mint_attempts_total{") {
			samples.WriteString(line)
		}
	}
	return samples.String()
}

// reply is one answer a scripted upstream gives.
type reply struct {
	status int
	header http.Header
	body   string
}

// sentRequest is what one request to a scripted upstream sent.
type sentRequest struct {
	method, path, body string
}

// scripted is a fake upstream that answers the requests it gets with its replies, one each in
// turn, and records what each request sent. A request past the last reply fails the test.
type scripted struct {
	server *httptest.Server

	mu      sync.Mutex
	replies []reply
	sent    []sentRequest
}

func newScripted(t *testing.T, replies ...reply) *scripted {
	t.Helper()
	s := &scripted{replies: replies}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body bytes.Buffer
		_, _ = body.ReadFrom(r.Body)
		s.mu.Lock()
		s.sent = append(s.sent, sentRequest{method: r.Method, path: r.URL.Path, body: body.String()})
		if len(s.replies) == 0 {
			s.mu.Unlock()
			t.Errorf("%s %s came past the end of the script", r.Method, r.URL.Path)
			http.Error(w, "past the script", http.StatusTeapot)
			return
		}
		next := s.replies[0]
		s.replies = s.replies[1:]
		s.mu.Unlock()
		for name, values := range next.header {
			w.Header()[name] = values
		}
		w.WriteHeader(next.status)
		_, _ = w.Write([]byte(next.body))
	}))
	t.Cleanup(s.server.Close)
	return s
}

// requests is what every request the upstream got sent, in order.
func (s *scripted) requests() []sentRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.sent)
}

// tokenRequests is what every access_tokens request the upstream got sent, in order.
func (s *scripted) tokenRequests() []sentRequest {
	var tokens []sentRequest
	for _, request := range s.requests() {
		if request.method == http.MethodPost && strings.HasSuffix(request.path, "/access_tokens") {
			tokens = append(tokens, request)
		}
	}
	return tokens
}

// The replies of a GitHub that lists one installation, acme's 7, and mints its token.
var (
	installed   = reply{status: http.StatusOK, body: `[{"id":7,"account":{"login":"acme"}}]`}
	tokenMinted = reply{status: http.StatusCreated, body: `{"token":"token-7","expires_at":"2026-11-14T13:00:00Z"}`}
)

// answered is a reply of status with a body that names no rate limit, and header.
func answered(status int, header http.Header) reply {
	return reply{status: status, header: header, body: `{"message":"no"}`}
}

// mintGitHub mints acme's token from up under retry, as a release of a github-app secret does.
func mintGitHub(ctx context.Context, t *testing.T, up *scripted, retry *secrets.Retry, narrowing *secrets.GitHubNarrowing) (secrets.Minted, error) {
	t.Helper()
	key, err := testAppKey()
	if err != nil {
		t.Fatal(err)
	}
	return secrets.GitHubApp{Base: up.server.URL, HTTP: up.server.Client(), Retry: retry}.Mint(ctx, secrets.MintRequest{
		Kind: "github-app", Value: githubAppValue(t, "1", key), GitHubOwners: []string{"acme"}, GitHubNarrowing: narrowing,
	})
}

// TestMintCallRetriesA503TwiceAndMintsAtTheThird: a token call GitHub answers 503 twice and then
// 201 mints at its third attempt. With the draw at the top of its range the two waits are their
// ceilings, 2 s and then 4 s, so the ceilings grow; with the real draw each wait falls under its
// ceiling, the two add up to no more than the mint's 10 s, and two runs wait differently. Each
// attempt is counted by kind, outcome and number.
func TestMintCallRetriesA503TwiceAndMintsAtTheThird(t *testing.T) {
	var ceilings []time.Duration
	retry, clock, registry := testRetry(func(ceiling time.Duration) time.Duration {
		ceilings = append(ceilings, ceiling)
		return ceiling
	})
	up := newScripted(t, installed, answered(http.StatusServiceUnavailable, nil), answered(http.StatusServiceUnavailable, nil), tokenMinted)
	minted, err := mintGitHub(t.Context(), t, up, retry, nil)
	if err != nil || !strings.Contains(minted.Value, "token-7") {
		t.Fatalf("Mint after 503, 503, 201 = %q, %v; want acme's token", minted.Value, err)
	}
	if calls := len(up.tokenRequests()); calls != 3 {
		t.Fatalf("the mint made %d token requests; want 3", calls)
	}
	want := []time.Duration{2 * time.Second, 4 * time.Second}
	if waits := clock.slept(); !slices.Equal(waits, want) || !slices.Equal(ceilings, want) {
		t.Fatalf("with the draw at the top the mint waited %v under ceilings %v; want %v for both", waits, ceilings, want)
	}
	samples := attemptSamples(registry)
	for _, sample := range []string{
		`broker_mint_attempts_total{kind="github-app",outcome="ok",attempt="1"} 1`,
		`broker_mint_attempts_total{kind="github-app",outcome="upstream",attempt="1"} 1`,
		`broker_mint_attempts_total{kind="github-app",outcome="upstream",attempt="2"} 1`,
		`broker_mint_attempts_total{kind="github-app",outcome="ok",attempt="3"} 1`,
	} {
		if !strings.Contains(samples, sample+"\n") {
			t.Errorf("the attempts counter holds\n%s\nwant the sample %s", samples, sample)
		}
	}

	var runs [2][]time.Duration
	for run := range runs {
		retry, clock, _ := testRetry(nil)
		up := newScripted(t, installed, answered(http.StatusServiceUnavailable, nil), answered(http.StatusServiceUnavailable, nil), tokenMinted)
		if _, err := mintGitHub(t.Context(), t, up, retry, nil); err != nil {
			t.Fatalf("run %d: Mint after 503, 503, 201: %v", run, err)
		}
		waits := clock.slept()
		if len(waits) != 2 || waits[0] > 2*time.Second || waits[1] > 4*time.Second || waits[0]+waits[1] > 10*time.Second {
			t.Fatalf("run %d with the real draw waited %v; want two waits under 2 s and 4 s adding up to 10 s or less", run, waits)
		}
		runs[run] = waits
	}
	if slices.Equal(runs[0], runs[1]) {
		t.Fatalf("two runs with the real draw both waited %v; want the jitter to differ between runs", runs[0])
	}
}

// TestMintCallRetriesWhatItsClassifierRetries: the one classifier retries a 5xx and a rate limit,
// and nothing else: not a 4xx that is no rate limit, an answer of a status the call did not want,
// or an answer past its size cap. The failure of a call is its last attempt's, naming how many
// attempts it made; a mint that fails before it calls anything made none.
func TestMintCallRetriesWhatItsClassifierRetries(t *testing.T) {
	past := reply{status: http.StatusOK, body: "[" + strings.Repeat(" ", 1<<20) + "]"}
	for _, tc := range []struct {
		name    string
		replies []reply
		// class is the failure's class, "" for a mint.
		class    string
		attempts int
		outcomes []string
	}{
		{name: "a 500, then a 201", replies: []reply{installed, answered(http.StatusInternalServerError, nil), tokenMinted}, outcomes: []string{"upstream", "ok"}},
		{name: "a 502 three times", replies: []reply{installed, answered(http.StatusBadGateway, nil), answered(http.StatusBadGateway, nil), answered(http.StatusBadGateway, nil)}, class: "upstream", attempts: 3, outcomes: []string{"upstream", "upstream", "upstream"}},
		{name: "a 429 asking a second, then a 201", replies: []reply{installed, answered(http.StatusTooManyRequests, http.Header{"Retry-After": {"1"}}), tokenMinted}, outcomes: []string{"rate_limited", "ok"}},
		{name: "a 403 that is no rate limit", replies: []reply{installed, reply{status: http.StatusForbidden, body: `{"message":"Resource not accessible by integration"}`}}, class: "refused", attempts: 1, outcomes: []string{"refused"}},
		{name: "a 404", replies: []reply{installed, answered(http.StatusNotFound, nil)}, class: "refused", attempts: 1, outcomes: []string{"refused"}},
		{name: "a 200 the token call does not want", replies: []reply{installed, reply{status: http.StatusOK, body: tokenMinted.body}}, class: "upstream", attempts: 1, outcomes: []string{"upstream"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			retry, _, registry := testRetry(atTheTop)
			up := newScripted(t, tc.replies...)
			minted, err := mintGitHub(t.Context(), t, up, retry, nil)
			if len(up.requests()) != len(tc.replies) {
				t.Fatalf("the mint made %d requests; want the %d its script answers", len(up.requests()), len(tc.replies))
			}
			if tc.class == "" && (err != nil || minted.Value == "") {
				t.Fatalf("Mint = %q, %v; want a token", minted.Value, err)
			}
			if tc.class != "" && (secrets.MintErrorClass(err) != tc.class || secrets.MintAttempts(err) != tc.attempts) {
				t.Fatalf("Mint = %v (class %s, %d attempts); want class %s after %d attempts", err, secrets.MintErrorClass(err), secrets.MintAttempts(err), tc.class, tc.attempts)
			}
			// The token call's attempts, each counted once: no row's token call is ok at its
			// first attempt, which the installations call's own ok would share.
			samples := attemptSamples(registry)
			for number, outcome := range tc.outcomes {
				sample := `broker_mint_attempts_total{kind="github-app",outcome="` + outcome + `",attempt="` + strconv.Itoa(number+1) + `"} 1`
				if !strings.Contains(samples, sample+"\n") {
					t.Errorf("the attempts counter holds\n%s\nwant the sample %s", samples, sample)
				}
			}
		})
	}

	t.Run("an answer past its cap", func(t *testing.T) {
		retry, _, _ := testRetry(atTheTop)
		up := newScripted(t, past)
		_, err := mintGitHub(t.Context(), t, up, retry, nil)
		if len(up.requests()) != 1 || secrets.MintErrorClass(err) != "upstream" || secrets.MintAttempts(err) != 1 || !strings.Contains(err.Error(), "more than 1048576 bytes") {
			t.Fatalf("Mint against an answer past its cap = %v after %d requests (%d attempts); want one upstream attempt naming the cap", err, len(up.requests()), secrets.MintAttempts(err))
		}
	})
	t.Run("a stored value the minter refuses", func(t *testing.T) {
		retry, _, _ := testRetry(atTheTop)
		_, err := secrets.GitHubApp{Retry: retry}.Mint(t.Context(), secrets.MintRequest{Value: "not json"})
		if secrets.MintErrorClass(err) != "stored-value" || secrets.MintAttempts(err) != 0 {
			t.Fatalf("Mint of a value that is no JSON = %v (%d attempts); want stored-value and no attempt", err, secrets.MintAttempts(err))
		}
	})
}

// TestANilRetryMakesOneAttempt: a minter built with no Retry, as agent-secrets' own code and a
// row about something else build one, makes one attempt of each call.
func TestANilRetryMakesOneAttempt(t *testing.T) {
	up := newScripted(t, installed, answered(http.StatusServiceUnavailable, nil))
	_, err := mintGitHub(t.Context(), t, up, nil, nil)
	if len(up.tokenRequests()) != 1 || secrets.MintErrorClass(err) != "upstream" || secrets.MintAttempts(err) != 1 {
		t.Fatalf("Mint with no Retry = %v after %d token requests (%d attempts); want one upstream attempt", err, len(up.tokenRequests()), secrets.MintAttempts(err))
	}
}

// failingFirst is a transport whose first access_tokens call fails before any answer, as a
// connection refused does, and which carries every other call through.
type failingFirst struct {
	failed atomic.Bool
}

func (f *failingFirst) RoundTrip(request *http.Request) (*http.Response, error) {
	if strings.HasSuffix(request.URL.Path, "/access_tokens") && f.failed.CompareAndSwap(false, true) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	}
	return http.DefaultTransport.RoundTrip(request)
}

// TestMintCallRetriesATransportFailure: a call that got no answer at all, while its context
// lives, is made again.
func TestMintCallRetriesATransportFailure(t *testing.T) {
	key, err := testAppKey()
	if err != nil {
		t.Fatal(err)
	}
	retry, clock, registry := testRetry(atTheTop)
	up := newScripted(t, installed, tokenMinted)
	minted, err := secrets.GitHubApp{Base: up.server.URL, HTTP: &http.Client{Transport: &failingFirst{}}, Retry: retry}.
		Mint(t.Context(), secrets.MintRequest{Kind: "github-app", Value: githubAppValue(t, "1", key), GitHubOwners: []string{"acme"}})
	if err != nil || !strings.Contains(minted.Value, "token-7") {
		t.Fatalf("Mint after a transport failure = %q, %v; want acme's token", minted.Value, err)
	}
	if waits := clock.slept(); !slices.Equal(waits, []time.Duration{2 * time.Second}) {
		t.Fatalf("the mint waited %v; want one wait at the first ceiling", waits)
	}
	if samples := attemptSamples(registry); !strings.Contains(samples, `broker_mint_attempts_total{kind="github-app",outcome="upstream",attempt="1"} 1`+"\n") {
		t.Fatalf("the attempts counter holds\n%s\nwant the transport failure as attempt 1's upstream outcome", samples)
	}
}

// TestMintCallTriesACancelledContextOnce: a call whose caller's context ends while it waits for
// the answer is not made again, and is counted as cancelled.
func TestMintCallTriesACancelledContextOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var tokenCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(installed.body))
			return
		}
		tokenCalls.Add(1)
		cancel()
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	defer server.Close()
	retry, clock, registry := testRetry(atTheTop)
	key, err := testAppKey()
	if err != nil {
		t.Fatal(err)
	}
	_, err = secrets.GitHubApp{Base: server.URL, HTTP: server.Client(), Retry: retry}.
		Mint(ctx, secrets.MintRequest{Kind: "github-app", Value: githubAppValue(t, "1", key), GitHubOwners: []string{"acme"}})
	if tokenCalls.Load() != 1 || secrets.MintAttempts(err) != 1 || !strings.Contains(err.Error(), "was cancelled before it answered") {
		t.Fatalf("Mint under a context cancelled mid-call = %v after %d token calls (%d attempts); want one attempt, named cancelled", err, tokenCalls.Load(), secrets.MintAttempts(err))
	}
	if waits := clock.slept(); len(waits) != 0 {
		t.Fatalf("the mint waited %v; want no wait", waits)
	}
	if samples := attemptSamples(registry); !strings.Contains(samples, `broker_mint_attempts_total{kind="github-app",outcome="cancelled",attempt="1"} 1`+"\n") {
		t.Fatalf("the attempts counter holds\n%s\nwant attempt 1 counted as cancelled", samples)
	}
}

// TestMintCallWaitsTheWaitTheServerNamesInFull: a wait the answer names is waited whole on the
// clock the test sets, never the jitter's: Retry-After in seconds, Retry-After as an HTTP date,
// and X-RateLimit-Reset when X-RateLimit-Remaining is 0.
func TestMintCallWaitsTheWaitTheServerNamesInFull(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer func(now time.Time) reply
		want   time.Duration
	}{
		{"Retry-After: 2 on a 503", func(time.Time) reply {
			return answered(http.StatusServiceUnavailable, http.Header{"Retry-After": {"2"}})
		}, 2 * time.Second},
		{"Retry-After as an HTTP date on a 429", func(now time.Time) reply {
			return answered(http.StatusTooManyRequests, http.Header{"Retry-After": {now.Add(3 * time.Second).Format(http.TimeFormat)}})
		}, 3 * time.Second},
		{"X-RateLimit-Reset on a 403 with none remaining", func(now time.Time) reply {
			return answered(http.StatusForbidden, http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {strconv.FormatInt(now.Add(4*time.Second).Unix(), 10)}})
		}, 4 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			retry, clock, _ := testRetry(atTheBottom)
			up := newScripted(t, installed, tc.answer(clock.Now()), tokenMinted)
			if _, err := mintGitHub(t.Context(), t, up, retry, nil); err != nil {
				t.Fatalf("Mint after the named wait: %v", err)
			}
			if waits := clock.slept(); !slices.Equal(waits, []time.Duration{tc.want}) {
				t.Fatalf("the mint waited %v; want the %v the answer named, in full", waits, tc.want)
			}
		})
	}
}

// TestMintCallEndsAtANamedWaitPastWhatIsLeft: a wait the answer names that does not fit in what
// is left of the mint's 10 s ends the mint with no further attempt, and none of it is waited: a
// wait longer than the whole budget, a rate limit that names no wait (a minute), and a wait that
// fits the budget but not what an earlier wait of the same mint left of it.
func TestMintCallEndsAtANamedWaitPastWhatIsLeft(t *testing.T) {
	for _, tc := range []struct {
		name     string
		replies  []reply
		waits    []time.Duration
		attempts int
	}{
		{"a 429 asking 11 s", []reply{installed, answered(http.StatusTooManyRequests, http.Header{"Retry-After": {"11"}})}, nil, 1},
		{"a secondary rate limit naming no wait", []reply{installed, {status: http.StatusForbidden, body: `{"message":"You have exceeded a secondary rate limit."}`}}, nil, 1},
		{"a 429 naming no wait", []reply{installed, answered(http.StatusTooManyRequests, nil)}, nil, 1},
		{"a 429 asking 2 s after a 503 asked 9 s", []reply{installed,
			answered(http.StatusServiceUnavailable, http.Header{"Retry-After": {"9"}}),
			answered(http.StatusTooManyRequests, http.Header{"Retry-After": {"2"}}),
		}, []time.Duration{9 * time.Second}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			retry, clock, _ := testRetry(atTheTop)
			up := newScripted(t, tc.replies...)
			_, err := mintGitHub(t.Context(), t, up, retry, nil)
			if len(up.requests()) != len(tc.replies) || secrets.MintErrorClass(err) != "upstream" || secrets.MintAttempts(err) != tc.attempts {
				t.Fatalf("Mint = %v after %d requests (%d attempts); want class upstream after %d attempts and no further request",
					err, len(up.requests()), secrets.MintAttempts(err), tc.attempts)
			}
			if waits := clock.slept(); !slices.Equal(waits, tc.waits) {
				t.Fatalf("the mint waited %v; want %v", waits, tc.waits)
			}
		})
	}
}

// TestMintCallEndsWhenTheDeadlineIsShorterThanTheWait: a caller's deadline that falls before the
// next wait ends ends the mint at once, with no further attempt and no wait.
func TestMintCallEndsWhenTheDeadlineIsShorterThanTheWait(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	retry, clock, _ := testRetry(atTheTop)
	up := newScripted(t, installed, answered(http.StatusServiceUnavailable, nil))
	_, err := mintGitHub(ctx, t, up, retry, nil)
	if len(up.tokenRequests()) != 1 || secrets.MintErrorClass(err) != "upstream" || secrets.MintAttempts(err) != 1 {
		t.Fatalf("Mint under a 1 s deadline before a 2 s wait = %v after %d token requests (%d attempts); want one attempt", err, len(up.tokenRequests()), secrets.MintAttempts(err))
	}
	if waits := clock.slept(); len(waits) != 0 {
		t.Fatalf("the mint waited %v; want no wait", waits)
	}
}

// TestMintCallSendsItsBodyAgain: a retried call sends the body it sent first, whole: a narrowed
// token request's access_tokens body.
func TestMintCallSendsItsBodyAgain(t *testing.T) {
	retry, _, _ := testRetry(atTheTop)
	narrowed := reply{status: http.StatusCreated, body: `{"token":"token-7","expires_at":"2026-11-14T13:00:00Z","repository_selection":"selected","repositories":[{"name":"proofs"}]}`}
	up := newScripted(t, installed, answered(http.StatusServiceUnavailable, nil), narrowed)
	if _, err := mintGitHub(t.Context(), t, up, retry, &secrets.GitHubNarrowing{Owner: "acme", Repositories: []string{"proofs"}}); err != nil {
		t.Fatalf("Mint of a narrowed token after a 503: %v", err)
	}
	tokens := up.tokenRequests()
	if len(tokens) != 2 || tokens[0].body != `{"repositories":["proofs"]}` || tokens[1].body != tokens[0].body {
		t.Fatalf("the token requests sent %+v; want the narrowing's body twice", tokens)
	}
}

// TestGoogleMintersRetryA503AndAnswerInvalidGrantAtOnce: both Google minters share the one retry:
// each mints through a 503, sending its body again, and a refusal - a login's invalid_grant, a
// service account's permission denied - is answered at its first attempt.
func TestGoogleMintersRetryA503AndAnswerInvalidGrantAtOnce(t *testing.T) {
	unavailable := reply{status: http.StatusServiceUnavailable, body: `{"error":{"code":503,"status":"UNAVAILABLE"}}`}
	login := func(retry *secrets.Retry, up *scripted) (secrets.Minted, error) {
		return secrets.GoogleLogin{TokenURL: up.server.URL, HTTP: up.server.Client(), Retry: retry}.
			Mint(t.Context(), secrets.MintRequest{Kind: "google-login", Value: googleLoginValue()})
	}
	account := func(retry *secrets.Retry, up *scripted) (secrets.Minted, error) {
		minter := &secrets.GoogleServiceAccount{
			BaseURL: up.server.URL, HTTP: up.server.Client(), Source: brokerSource(),
			Accounts: map[string]string{"GCP_FLEET": serviceAccountEmail}, Retry: retry,
		}
		return minter.Mint(t.Context(), secrets.MintRequest{Name: "GCP_FLEET", Kind: "google-service-account", Value: serviceAccountValue})
	}
	for _, tc := range []struct {
		name    string
		mint    func(*secrets.Retry, *scripted) (secrets.Minted, error)
		kind    string
		replies []reply
		class   string
	}{
		{"a login through a 503", login, "google-login", []reply{unavailable, {status: http.StatusOK, body: `{"access_token":"minted"}`}}, ""},
		{"a login's invalid_grant", login, "google-login", []reply{{status: http.StatusBadRequest, body: `{"error":"invalid_grant"}`}}, "refused"},
		{"a service account through a 503", account, "google-service-account", []reply{unavailable, {status: http.StatusOK, body: `{"accessToken":"minted","expireTime":"2026-11-14T13:00:00Z"}`}}, ""},
		{"a service account's permission denied", account, "google-service-account", []reply{{status: http.StatusForbidden, body: `{"error":{"code":403,"status":"PERMISSION_DENIED"}}`}}, "refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			retry, _, registry := testRetry(atTheTop)
			up := newScripted(t, tc.replies...)
			minted, err := tc.mint(retry, up)
			sent := up.requests()
			if len(sent) != len(tc.replies) {
				t.Fatalf("the mint made %d requests; want %d", len(sent), len(tc.replies))
			}
			if tc.class != "" {
				if secrets.MintErrorClass(err) != tc.class || secrets.MintAttempts(err) != 1 {
					t.Fatalf("Mint = %v (class %s, %d attempts); want class %s at the first attempt", err, secrets.MintErrorClass(err), secrets.MintAttempts(err), tc.class)
				}
				return
			}
			if err != nil || minted.Value != "minted" {
				t.Fatalf("Mint through a 503 = %q, %v; want the minted token", minted.Value, err)
			}
			if sent[0].body == "" || sent[1].body != sent[0].body {
				t.Fatalf("the two attempts sent %q and %q; want the one body twice", sent[0].body, sent[1].body)
			}
			if samples := attemptSamples(registry); !strings.Contains(samples, `broker_mint_attempts_total{kind="`+tc.kind+`",outcome="ok",attempt="2"} 1`+"\n") {
				t.Fatalf("the attempts counter holds\n%s\nwant attempt 2's ok under kind %s", samples, tc.kind)
			}
		})
	}
}

// TestRetryBudgetStopsManyMintsFailingAtOnce: every mint of the process draws its retries from one
// budget of 60, so 50 mints failing at once make their 50 first attempts and 60 retries between
// them, not the 150 three attempts each would make; a mint that finds the budget spent ends at its
// first failure. The budget refills one a second: two seconds on, one mint retries twice and the
// next not at all.
func TestRetryBudgetStopsManyMintsFailingAtOnce(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	var mu sync.Mutex
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	retry := secrets.NewRetry(metrics.New())
	retry.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	retry.Sleep = func(context.Context, time.Duration) error { return nil }
	mint := func() error {
		_, err := secrets.GoogleLogin{TokenURL: server.URL, HTTP: server.Client(), Retry: retry}.
			Mint(t.Context(), secrets.MintRequest{Kind: "google-login", Value: googleLoginValue()})
		return err
	}

	const mints = 50
	var wg sync.WaitGroup
	for range mints {
		wg.Go(func() {
			if err := mint(); secrets.MintErrorClass(err) != "upstream" {
				t.Errorf("a mint against a 503 = %v; want upstream", err)
			}
		})
	}
	wg.Wait()
	if got, want := calls.Load(), int64(mints+60); got != want {
		t.Fatalf("%d mints failing at once made %d calls; want %d, their first attempts and the budget's 60 retries", mints, got, want)
	}
	if err := mint(); calls.Load() != mints+61 || secrets.MintAttempts(err) != 1 {
		t.Fatalf("a mint once the budget is spent made %d attempts; want 1", secrets.MintAttempts(err))
	}

	mu.Lock()
	now = now.Add(2 * time.Second)
	mu.Unlock()
	for _, want := range []int{3, 1} {
		if err := mint(); secrets.MintAttempts(err) != want {
			t.Fatalf("a mint two seconds on made %d attempts (%v); want %d", secrets.MintAttempts(err), err, want)
		}
	}
}
