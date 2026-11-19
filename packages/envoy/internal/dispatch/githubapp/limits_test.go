package githubapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/ratelimit"
	"github.com/sjawhar/envoy/internal/dispatch/githubreads"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/githubtest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// limitGitHub is a GitHub App API that records every request it is sent ("METHOD path
// Authorization"): one repository (acme/web) and the installation covering it, token mints, the
// App's installation list, and any other path answered as a read. A request to limitPath is
// answered as GitHub answers an exhausted rate limit: 403, X-RateLimit-Remaining: 0 and a
// Retry-After of a minute.
type limitGitHub struct {
	server *httptest.Server

	mu             sync.Mutex
	installationID int64
	mints          int
	limitPath      string
	requests       []string
	// mintsAwaited and mintsArrived, set by holdMints, hold each mint's answer until that many
	// mints have arrived.
	mintsAwaited int
	mintsArrived chan struct{}
	// graphQL is what POST /graphql answers, which a test sets to one of the shapes GitHub
	// answers a rate-limited query with (answerGraphQL); unset, the query is answered as every
	// other read is.
	graphQL *graphQLAnswer
}

// graphQLAnswer is one answer to a query: the status, the headers and the body GitHub gives.
// The three together are all a client may read a GraphQL rate limit from.
type graphQLAnswer struct {
	status int
	header map[string]string
	body   string
}

// answerGraphQL makes every later query answer this.
func (f *limitGitHub) answerGraphQL(answer graphQLAnswer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.graphQL = &answer
}

func startLimitGitHub(t *testing.T) *limitGitHub {
	t.Helper()
	fake := &limitGitHub{installationID: 7}
	fake.server = githubtest.Serve(t, githubtest.Spec{Handle: fake.serve})
	return fake
}

func (f *limitGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
	limited := r.URL.Path == f.limitPath
	installationID := f.installationID
	var held chan struct{}
	if !limited && strings.HasSuffix(r.URL.Path, "/access_tokens") {
		f.mints++
		if f.mintsArrived != nil {
			held = f.mintsArrived
			if f.mintsAwaited--; f.mintsAwaited == 0 {
				close(f.mintsArrived)
				f.mintsArrived = nil
			}
		}
	}
	mints := f.mints
	query := f.graphQL
	f.mu.Unlock()
	if held != nil {
		// A client that sends fewer mints than awaited is answered anyway, a little later.
		select {
		case <-held:
		case <-time.After(2 * time.Second):
		}
	}
	if limited {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
		return
	}
	if query != nil && r.URL.Path == "/graphql" {
		for name, value := range query.header {
			w.Header().Set(name, value)
		}
		w.WriteHeader(query.status)
		fmt.Fprint(w, query.body)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/web/installation":
		_ = json.NewEncoder(w).Encode(map[string]any{"id": installationID, "app_slug": "dispatch-test", "permissions": map[string]string{"contents": "read"}})
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/app/installations/") && strings.HasSuffix(r.URL.Path, "/access_tokens"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/app/installations/"), "/access_tokens")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": fmt.Sprintf("ghs_%s_%d", id, mints), "expires_at": time.Now().Add(time.Hour)})
	case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
		_ = json.NewEncoder(w).Encode([]map[string]any{{"id": installationID, "app_slug": "dispatch-test", "account": map[string]string{"login": "acme"}}})
	default:
		fmt.Fprint(w, `{"state":"open"}`)
	}
}

// limit makes every later request to path answer an exhausted rate limit; "" stops it.
func (f *limitGitHub) limit(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.limitPath = path
}

// holdMints holds each later mint's answer until n mints have arrived, so n callers that each
// found no token mint at once.
func (f *limitGitHub) holdMints(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mintsAwaited = n
	f.mintsArrived = make(chan struct{})
}

// moveInstallation makes acme/web's installation lookup answer id.
func (f *limitGitHub) moveInstallation(id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.installationID = id
}

// count is how many requests GitHub was sent whose line holds every part.
func (f *limitGitHub) count(parts ...string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, request := range f.requests {
		matched := true
		for _, part := range parts {
			matched = matched && strings.Contains(request, part)
		}
		if matched {
			n++
		}
	}
	return n
}

func (f *limitGitHub) newClient(t *testing.T, options ...Option) *Client {
	t.Helper()
	_, pemText := testKeyPEM(t)
	client, err := New(&auth.AppConfig{ClientID: "Iv1.limits", PEM: pemText}, f.server.URL, options...)
	if err != nil || client == nil {
		t.Fatalf("new client: %v", err)
	}
	return client
}

// twoProcesses is two clients on one database, each through a pool of its own, as two Dispatch
// processes hold them, both calling one GitHub.
func twoProcesses(t *testing.T, fake *limitGitHub) (database *store.Store, a, b *Client) {
	t.Helper()
	database = storetest.Open(t)
	a = fake.newClient(t, WithLimits(ratelimit.New(database.Pool)))
	b = fake.newClient(t, WithLimits(ratelimit.New(storetest.SecondProcess(t, database).Pool)))
	return database, a, b
}

// passLimits moves every recorded limit's end into the past, as waiting GitHub's wait out would.
func passLimits(t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `update github_rate_limits set until = now() - interval '1 second'`); err != nil {
		t.Fatalf("move the recorded limits back: %v", err)
	}
}

// requireHeld requires err to be a limit recorded by the other process: no request was sent.
func requireHeld(t *testing.T, err error, what string) {
	t.Helper()
	limited, ok := AsRateLimit(err)
	if !ok || !limited.Held || limited.Wait <= 0 {
		t.Fatalf("%s: err = %v, want the recorded limit, held without a request", what, err)
	}
}

// Which scope a rate-limited query is recorded under is which of GitHub's two limits it is.
//
// A primary limit is the App's GraphQL points spent, which GitHub budgets apart from the REST
// calls made with the same token: it holds every process's queries and leaves every process's
// REST reads going. A secondary limit -- too many points a minute, too many requests at once --
// GitHub shares between REST and GraphQL, so it holds the installation whose token met it,
// REST reads and all, and it is recorded with the doubling backoff GitHub asks of a client that
// keeps meeting one (a strike), where a primary limit ends at GitHub's own reset.
//
// Each shape here is one GitHub answers a limited query with, at either status.
func TestAGraphQLLimitIsRecordedUnderTheScopeGitHubBudgetsIt(t *testing.T) {
	reset := strconv.FormatInt(time.Now().Add(90*time.Second).Unix(), 10)
	primary := `{"data":null,"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded for installation"}]}`
	wording := `{"data":null,"errors":[{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}]}`
	// GitHub answers a secondary limit at 403 with its REST-shaped body as readily, the wording
	// in the top-level message and no retry-after of any kind, which its documentation says may
	// be absent.
	restWording := `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again.","documentation_url":"https://docs.github.com/rest/overview/rate-limits-for-the-rest-api"}`
	cases := []struct {
		name      string
		answer    graphQLAnswer
		secondary bool
	}{{
		name:   "the budget spent, at 200",
		answer: graphQLAnswer{status: http.StatusOK, header: map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset}, body: primary},
	}, {
		name:   "the budget spent, at 403",
		answer: graphQLAnswer{status: http.StatusForbidden, header: map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset}, body: primary},
	}, {
		name:      "a secondary limit with a retry-after, at 200",
		answer:    graphQLAnswer{status: http.StatusOK, header: map[string]string{"Retry-After": "60"}, body: wording},
		secondary: true,
	}, {
		name:      "a secondary limit worded alone, at 403",
		answer:    graphQLAnswer{status: http.StatusForbidden, body: wording},
		secondary: true,
	}, {
		name:      "a secondary limit in the body's own message, at 403",
		answer:    graphQLAnswer{status: http.StatusForbidden, body: restWording},
		secondary: true,
	}}
	for _, shape := range cases {
		t.Run(shape.name, func(t *testing.T) {
			fake := startLimitGitHub(t)
			database, a, b := twoProcesses(t, fake)
			ctx := context.Background()
			tokenA, err := a.Token(ctx, 7)
			if err != nil {
				t.Fatalf("A's token: %v", err)
			}
			tokenB, err := b.Token(ctx, 7)
			if err != nil {
				t.Fatalf("B's token: %v", err)
			}
			installation := InstallationScope(7)
			want := GraphQLScope
			if shape.secondary {
				want = installation
			}

			fake.answerGraphQL(shape.answer)
			_, _, _, err = a.GraphQL(ctx, tokenA, "query { viewer { login } }", nil)
			limited, ok := AsRateLimit(err)
			if !ok || limited.Held || limited.Wait <= 0 {
				t.Fatalf("A's limited query: err = %v, want GitHub's own answer as a *RateLimitError", err)
			}
			if limited.Scope != want {
				t.Errorf("the limit names scope %q, want %q", limited.Scope, want)
			}

			limits := ratelimit.New(database.Pool)
			recorded, found, err := limits.Get(ctx, want)
			if err != nil || !found {
				t.Fatalf("github_rate_limits holds no limit under %s: %v, %v", want, found, err)
			}
			if recorded.Wait <= 0 {
				t.Errorf("the limit under %s has %s left, want GitHub's own wait still in force", want, recorded.Wait)
			}
			wantStrikes := 0
			if shape.secondary {
				wantStrikes = 1
			}
			if recorded.Strikes != wantStrikes {
				t.Errorf("the limit under %s records %d strikes, want %d: a secondary limit is struck, so the next one doubles its wait, and a primary limit is recorded and ends at GitHub's own reset",
					want, recorded.Strikes, wantStrikes)
			}
			if other := GraphQLScope; shape.secondary {
				if _, found, err := limits.Get(ctx, other); err != nil || found {
					t.Errorf("a secondary limit was recorded under %s as well: %v, %v; GitHub shares it with the installation's REST calls", other, found, err)
				}
			} else if _, found, err := limits.Get(ctx, installation); err != nil || found {
				t.Errorf("a primary GraphQL limit was recorded under %s as well: %v, %v; the GraphQL budget is not the installation's REST budget",
					installation, found, err)
			}

			// What the other process may still do under that installation: its REST reads go on
			// inside a GraphQL budget spent, and stop inside a limit GitHub shares with them.
			_, status, _, err := b.Read(ctx, tokenB, "/repos/acme/web/issues/2")
			if shape.secondary {
				requireHeld(t, err, "B's REST read inside a secondary limit of the installation")
			} else if err != nil || status != http.StatusOK {
				t.Fatalf("B's REST read inside a primary GraphQL limit: status %d, %v; want GitHub's own answer", status, err)
			}
			// Either limit holds the other process's queries: GraphQL's own scope holds them,
			// and so does the installation's, which every query is sent under.
			_, _, _, err = b.GraphQL(ctx, tokenB, "query { viewer { login } }", nil)
			requireHeld(t, err, "B's query inside the limit A met")
		})
	}
}

// Every rate limit a request meets is noted on the attempt its context carries (githubreads),
// with the error its caller is given: a limit already recorded, which holds a REST request in
// send and a query in GraphQL before it is sent, and is noted though nothing was sent; and a limit
// GraphQLRateLimit reads in a query's answer, which GitHub gives with status 200, named with the
// query's method and target. That limit is noted once it is recorded, so a secondary limit's
// cause carries the wait its strike holds every process to, past GitHub's own. A limit read in a
// 200 holds GitHub's own wording of it in place of the answer's body, which is the query's data
// (GitHub answers the request that spends the last point with its data), since GET /healthz
// serves the cause with no credential: the envelope's entries that name the limit, or, where it
// holds none, the headers that name the limit, and never any of the data or an entry about
// something else. The note carries that wording, and so does the error its caller is given, which
// a loop notes wrapped. A query GitHub answers is not noted.
func TestEveryRateLimitARequestMeetsIsNotedOnItsAttempt(t *testing.T) {
	reset := strconv.FormatInt(time.Now().Add(90*time.Second).Unix(), 10)
	spent := `{"data":null,"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded for installation"}]}`
	secondaryLimit := `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`
	dataMarker := "a-private-repositorys-own-data-4324"
	lastPoint := `{"data":{"viewer":{"login":"` + dataMarker + `"}}}`
	// GitHub can answer a query's errors beside what data it read; the cause holds the errors.
	secondaryWithData := `{"data":{"viewer":{"login":"` + dataMarker + `"}},"errors":[{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}]}`
	// GitHub's RATE_LIMITED is in the data alone, beside an error about another thing, on the
	// answer that spends the last point: only the headers name the limit.
	limitedInData := `{"data":{"t":"RATE_LIMITED secret"},"errors":[{"type":"NOT_FOUND","path":["repository"],"message":"Could not resolve to a Repository with the name 'acme/x'."}]}`
	read := func(ctx context.Context, client *Client, token string) error {
		_, _, _, err := client.Read(ctx, token, "/repos/acme/web/issues/2")
		return err
	}
	query := func(ctx context.Context, client *Client, token string) error {
		_, _, _, err := client.GraphQL(ctx, token, "query { viewer { login } }", nil)
		return err
	}
	held := func(scope string) func(*limitGitHub, error) string {
		return func(_ *limitGitHub, err error) string {
			if limited, ok := AsRateLimit(err); !ok || !limited.Held || limited.Scope != scope {
				return fmt.Sprintf("(want a limit held under %s, got %v)", scope, err)
			}
			return err.Error()
		}
	}
	// answeredAt200 is the cause noted for a limit GitHub answers a query with at 200 and body:
	// the limit's own text with wording, GitHub's own words for the limit, in place of the body.
	// The caller is given the limit with that wording and an empty Body, worded as RateLimitError
	// words it, and never the body. The cause names the request by its path after the API base
	// (Client.shownTarget), since the base may carry a user and a password.
	answeredAt200 := func(body, wording string) func(*limitGitHub, error) string {
		return func(_ *limitGitHub, err error) string {
			limited, ok := AsRateLimit(err)
			if !ok || limited.Held || limited.Status != http.StatusOK || limited.Body != "" || limited.wording != wording ||
				err.Error() != fmt.Sprintf("rate-limited by GitHub (status 200), next request in %s: %s", limited.Wait, wording) {
				return fmt.Sprintf("(want GitHub's own 200 as a limit carrying GitHub's wording %q of it in place of its body %s, got %v)", wording, body, err)
			}
			return fmt.Sprintf("POST /graphql: rate-limited by GitHub (status 200), next request in %s: %s", limited.Wait, wording)
		}
	}
	cases := []struct {
		name string
		// prepare records the limit, or sets the answer, the request meets.
		prepare func(t *testing.T, limits *ratelimit.Store, fake *limitGitHub)
		request func(ctx context.Context, client *Client, token string) error
		// sent is whether the request reached GitHub, and cause the cause noted given the error
		// the caller was given, "" when nothing is noted.
		sent  bool
		cause func(fake *limitGitHub, err error) string
		// absent is text of GitHub's answer the cause must not hold.
		absent []string
	}{{
		name: "a REST request a recorded limit holds",
		prepare: func(t *testing.T, limits *ratelimit.Store, _ *limitGitHub) {
			if err := limits.Record(context.Background(), InstallationScope(7), time.Minute); err != nil {
				t.Fatalf("record the installation's limit: %v", err)
			}
		},
		request: read,
		cause:   held(InstallationScope(7)),
	}, {
		name: "a query a recorded GraphQL limit holds before it is sent",
		prepare: func(t *testing.T, limits *ratelimit.Store, _ *limitGitHub) {
			if err := limits.Record(context.Background(), GraphQLScope, time.Minute); err != nil {
				t.Fatalf("record the GraphQL limit: %v", err)
			}
		},
		request: query,
		cause:   held(GraphQLScope),
	}, {
		name: "a query GitHub answers 200 with its budget spent",
		prepare: func(_ *testing.T, _ *ratelimit.Store, fake *limitGitHub) {
			fake.answerGraphQL(graphQLAnswer{status: http.StatusOK, header: map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset}, body: spent})
		},
		request: query,
		sent:    true,
		cause:   answeredAt200(spent, "RATE_LIMITED: API rate limit exceeded for installation"),
	}, {
		name: "a query GitHub answers 200 with its data and a secondary limit in its errors",
		prepare: func(_ *testing.T, _ *ratelimit.Store, fake *limitGitHub) {
			fake.answerGraphQL(graphQLAnswer{status: http.StatusOK, body: secondaryWithData})
		},
		request: query,
		sent:    true,
		cause:   answeredAt200(secondaryWithData, "You have exceeded a secondary rate limit. Please wait a few minutes before you try again."),
		absent:  []string{dataMarker},
	}, {
		// The data of the answer is what a private repository's pull requests or check runs are
		// to delivery_reconcile and ci_reconcile, which no cause may carry to GET /healthz. Its
		// envelope holds no error, so the headers that say the budget is spent name the limit.
		name: "a query GitHub answers 200 with its data and its last point spent",
		prepare: func(_ *testing.T, _ *ratelimit.Store, fake *limitGitHub) {
			fake.answerGraphQL(graphQLAnswer{status: http.StatusOK, header: map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset}, body: lastPoint})
		},
		request: query,
		sent:    true,
		cause:   answeredAt200(lastPoint, "X-RateLimit-Remaining: 0; X-RateLimit-Reset: "+reset),
		absent:  []string{dataMarker},
	}, {
		name: "a query GitHub answers 200 with its last point spent and the limit's type in its data beside another error",
		prepare: func(_ *testing.T, _ *ratelimit.Store, fake *limitGitHub) {
			fake.answerGraphQL(graphQLAnswer{status: http.StatusOK, header: map[string]string{"X-RateLimit-Remaining": "0"}, body: limitedInData})
		},
		request: query,
		sent:    true,
		cause:   answeredAt200(limitedInData, "X-RateLimit-Remaining: 0"),
		absent:  []string{"secret", "NOT_FOUND"},
	}, {
		// GitHub asks for a second, and the strike holds the installation a minute at least.
		name: "a query GitHub answers 403 with a secondary limit, which its strike holds past GitHub's own wait",
		prepare: func(_ *testing.T, _ *ratelimit.Store, fake *limitGitHub) {
			fake.answerGraphQL(graphQLAnswer{status: http.StatusForbidden, header: map[string]string{"Retry-After": "1"}, body: secondaryLimit})
		},
		request: query,
		sent:    true,
		cause: func(_ *limitGitHub, err error) string {
			if limited, ok := AsRateLimit(err); !ok || limited.Held || limited.Scope != InstallationScope(7) || limited.Wait <= time.Second {
				return fmt.Sprintf("(want GitHub's 403 as a secondary limit struck under %s past GitHub's own second, got %v)", InstallationScope(7), err)
			}
			return "POST /graphql: " + err.Error()
		},
	}, {
		name:    "a query GitHub answers",
		prepare: func(*testing.T, *ratelimit.Store, *limitGitHub) {},
		request: query,
		sent:    true,
		cause:   func(*limitGitHub, error) string { return "" },
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := startLimitGitHub(t)
			limits := ratelimit.New(storetest.Open(t).Pool)
			client := fake.newClient(t, WithLimits(limits))
			token, err := client.Token(context.Background(), 7)
			if err != nil {
				t.Fatalf("token: %v", err)
			}
			tc.prepare(t, limits, fake)
			requests := fake.count()

			ctx, attempt := githubreads.Begin(context.Background())
			err = tc.request(ctx, client, token)
			sentRequests := 0
			if tc.sent {
				sentRequests = 1
			}
			if got := fake.count() - requests; got != sentRequests {
				t.Fatalf("GitHub got %d requests, want %d", got, sentRequests)
			}
			want := tc.cause(fake, err)
			sent, failed, cause := attempt.Outcome()
			if sent != tc.sent || failed != (want != "") || cause != want {
				t.Fatalf("the request answered %v: sent %v, failed %v, cause %q; want sent %v and cause %q", err, sent, failed, cause, tc.sent, want)
			}
			for _, text := range tc.absent {
				if strings.Contains(cause, text) {
					t.Fatalf("the cause %q holds %q, which is GitHub's answer and names no limit", cause, text)
				}
			}
		})
	}
}

// A limit GitHub answered a query with at 200 is noted with GitHub's wording of the limit alone,
// whatever builds the note: the body of such an answer is the query's data, which GET /healthz
// would serve with no credential. GraphQLRateLimit builds such a limit with that wording and no
// Body (answeredLimit), and limitNoted, the client's note of a limit GitHub answered, which GraphQL
// notes every limit it reads with (TestEveryRateLimitARequestMeetsIsNotedOnItsAttempt), carries
// the limit's text; here it is given each limit as GraphQLRateLimit reads it from the header and
// the body the query was answered with. At any other status the body is GitHub's refusal, which
// Body and the note carry whole, and a refusal with no body leaves the note ending at the wait.
func TestALimitNotedAt200NeverCarriesTheQuerysData(t *testing.T) {
	const marker = "a-private-repositorys-own-data-4324"
	const target = "https://api.github.com/graphql"
	rows := []struct {
		name   string
		status int
		header http.Header
		body   string
		// wording is GitHub's wording of the limit at a 2xx and its Body at any other status, and
		// what the note carries after the limit's wait, "" for nothing.
		wording string
	}{{
		name:    "the last point spent, with the query's data",
		status:  http.StatusOK,
		header:  http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {"1800000000"}},
		body:    `{"data":{"viewer":{"login":"` + marker + `"}}}`,
		wording: "X-RateLimit-Remaining: 0; X-RateLimit-Reset: 1800000000",
	}, {
		name:    "a secondary limit beside the query's data",
		status:  http.StatusOK,
		body:    `{"data":{"viewer":{"login":"` + marker + `"}},"errors":[{"message":"You have exceeded a secondary rate limit."}]}`,
		wording: "You have exceeded a secondary rate limit.",
	}, {
		name:    "the limit's type in the query's data, beside another error",
		status:  http.StatusOK,
		header:  http.Header{"X-Ratelimit-Remaining": {"0"}},
		body:    `{"data":{"t":"RATE_LIMITED ` + marker + `"},"errors":[{"type":"NOT_FOUND","message":"Could not resolve to a Repository with the name 'acme/x'."}]}`,
		wording: "X-RateLimit-Remaining: 0",
	}, {
		name:    "GitHub's refusal at 403",
		status:  http.StatusForbidden,
		body:    `{"message":"You have exceeded a secondary rate limit."}`,
		wording: `{"message":"You have exceeded a secondary rate limit."}`,
	}, {
		name:   "a refusal with no body",
		status: http.StatusTooManyRequests,
	}}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			limited, _ := GraphQLRateLimit(row.status, row.header, []byte(row.body))
			body, wording := row.wording, ""
			if row.status/100 == 2 {
				body, wording = "", row.wording
			}
			if limited == nil || limited.Status != row.status || limited.Body != body || limited.wording != wording {
				t.Fatalf("GraphQLRateLimit read %v, want a limit at %d whose Body is %q and whose wording is %q", limited, row.status, body, wording)
			}
			want := fmt.Sprintf("POST %s: rate-limited by GitHub (status %d), next request in %s", target, row.status, limited.Wait)
			if row.wording != "" {
				want += ": " + row.wording
			}
			if cause := limitNoted(http.MethodPost, target, limited).Error(); cause != want || strings.Contains(cause, marker) {
				t.Fatalf("the note reads %q, want %q, which holds nothing of the query's data", cause, want)
			}
		})
	}
}

// RateLimitError is an exported struct any package can build, not only answeredLimit, and its
// text reaches GET /healthz as a cause, served with no credential, however a loop wraps and notes
// it. At a 2xx status a Body is the data the request asked for -- a private repository's, or an
// installation token the mint answers 201 with -- so neither the error's text nor the client's
// note of it (limitNoted) carries Body there, whoever filled it. At 403 Body is GitHub's refusal,
// which the text and both notes end with, so no row passes by printing no body anywhere.
func TestARateLimitErrorAt2xxPrintsNoBodyWhoeverBuildsIt(t *testing.T) {
	const marker = "ghs_an-installation-token-4324"
	const refusal = `{"message":"API rate limit exceeded for installation ID 7."}`
	const target = "https://api.github.com/app/installations/7/access_tokens"
	rows := []struct {
		status int
		body   string
		// detail is what the text carries after the limit's wait, "" for nothing.
		detail string
	}{
		{status: http.StatusOK, body: marker},
		{status: http.StatusCreated, body: marker},
		{status: http.StatusNoContent, body: marker},
		{status: http.StatusForbidden, body: refusal, detail: refusal},
	}
	for _, row := range rows {
		t.Run(strconv.Itoa(row.status), func(t *testing.T) {
			limited := &RateLimitError{Status: row.status, Wait: time.Minute, Body: row.body}
			want := fmt.Sprintf("rate-limited by GitHub (status %d), next request in 1m0s", row.status)
			if row.detail != "" {
				want += ": " + row.detail
			}
			if text := limited.Error(); text != want || strings.Contains(text, marker) {
				t.Fatalf("the limit reads %q, want %q, which holds no 2xx body", text, want)
			}

			ctx, attempt := githubreads.Begin(context.Background())
			githubreads.NoteFailure(ctx, limited)
			if _, failed, cause := attempt.Outcome(); !failed || cause != want || strings.Contains(cause, marker) {
				t.Fatalf("the attempt failed %v with the cause %q, want the limit's text %q, which holds no 2xx body", failed, cause, want)
			}

			noted := "POST " + target + ": " + want
			ctx, attempt = githubreads.Begin(context.Background())
			githubreads.NoteFailure(ctx, limitNoted(http.MethodPost, target, limited))
			if _, failed, cause := attempt.Outcome(); !failed || cause != noted || strings.Contains(cause, marker) {
				t.Fatalf("the attempt failed %v with the client's note %q, want %q, which holds no 2xx body", failed, cause, noted)
			}
		})
	}
}

// A loop notes the error it gave up on in words of its own around it (delivery's recordRead,
// "<step>: %w"), and where that note is the first on its attempt, before any note the client
// makes, the error's own text is the cause GET /healthz serves with no credential. A limit GitHub
// answered a query with at 200 is such an error, and the body of that answer is the query's data.
// The error holds GitHub's wording of the limit in place of that body (answeredLimit), so neither
// its text nor the cause carries the data whichever note comes first, and the loop still finds
// the limit and its wait in the chain.
func TestA200LimitALoopNotesWrappedBeforeTheClientNeverCarriesTheQuerysData(t *testing.T) {
	const marker = "a-private-repositorys-own-data-4324"
	const step = "reconcile merged pull requests"
	rows := []struct {
		name    string
		header  http.Header
		body    string
		wording string
	}{{
		name:    "the last point spent, with the query's data",
		header:  http.Header{"X-Ratelimit-Remaining": {"0"}},
		body:    `{"data":{"search":{"nodes":[{"title":"` + marker + `"}]}}}`,
		wording: "X-RateLimit-Remaining: 0",
	}, {
		name:    "a secondary limit beside the query's data",
		header:  http.Header{},
		body:    `{"data":{"search":{"nodes":[{"title":"` + marker + `"}]}},"errors":[{"message":"You have exceeded a secondary rate limit."}]}`,
		wording: "You have exceeded a secondary rate limit.",
	}}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			limited, _ := GraphQLRateLimit(http.StatusOK, row.header, []byte(row.body))
			if limited == nil {
				t.Fatalf("GraphQLRateLimit read no limit in %s", row.body)
			}
			err := fmt.Errorf("%s: %w", step, limited)
			ctx, attempt := githubreads.Begin(context.Background())
			githubreads.NoteFailure(ctx, err)

			want := fmt.Sprintf("%s: rate-limited by GitHub (status 200), next request in %s: %s", step, limited.Wait, row.wording)
			if text := err.Error(); text != want || strings.Contains(text, marker) {
				t.Fatalf("the wrapped limit reads %q, want %q, which holds nothing of the query's data", text, want)
			}
			if _, failed, cause := attempt.Outcome(); !failed || cause != want || strings.Contains(cause, marker) {
				t.Fatalf("the attempt failed %v with the cause %q, want the loop's note %q, which holds nothing of the query's data", failed, cause, want)
			}
			if found, ok := AsRateLimit(err); !ok || found.Wait != limited.Wait || found.Wait <= 0 {
				t.Fatalf("the wrapped error %v holds no limit with GitHub's wait %s", err, limited.Wait)
			}
		})
	}
}

// GitHub budgets an installation, not a process. A limit one process meets reading under an
// installation's token holds the other process's reads under that installation, with no request,
// and leaves the App's own calls alone; once the limit's end has passed, the other reads again.
func TestAnInstallationsLimitMetByOneProcessHoldsTheOthersReads(t *testing.T) {
	fake := startLimitGitHub(t)
	database, a, b := twoProcesses(t, fake)
	ctx := context.Background()
	tokenA, err := a.Token(ctx, 7)
	if err != nil {
		t.Fatalf("A's token: %v", err)
	}
	tokenB, err := b.Token(ctx, 7)
	if err != nil {
		t.Fatalf("B's token: %v", err)
	}

	fake.limit("/repos/acme/web/pulls/1")
	if _, status, _, err := a.Read(ctx, tokenA, "/repos/acme/web/pulls/1"); err != nil || status != http.StatusForbidden {
		t.Fatalf("A's limited read: status %d, %v; want GitHub's 403", status, err)
	}
	_, _, _, err = b.Read(ctx, tokenB, "/repos/acme/web/issues/2")
	requireHeld(t, err, "B's read under the same installation")
	_, _, _, err = b.GraphQL(ctx, tokenB, "query { viewer { login } }", nil)
	requireHeld(t, err, "B's GraphQL query under the same installation")
	if got := fake.count("Bearer " + tokenB); got != 0 {
		t.Fatalf("GitHub got %d requests under B's token inside the installation's limit, want none", got)
	}
	if _, err := b.ListInstallations(ctx); err != nil {
		t.Fatalf("B's App call inside an installation's limit: %v, want GitHub's answer", err)
	}

	fake.limit("")
	passLimits(t, database)
	if _, status, _, err := b.Read(ctx, tokenB, "/repos/acme/web/issues/2"); err != nil || status != http.StatusOK {
		t.Fatalf("B's read once the limit passed: status %d, %v; want GitHub's 200", status, err)
	}
	if got := fake.count("GET /repos/acme/web/issues/2 Bearer " + tokenB); got != 1 {
		t.Fatalf("GitHub got B's read %d times once the limit passed, want once", got)
	}
}

// A token stays under its installation's limit until it expires, whatever mints after it: a
// caller still holding the token from before another caller refreshed the installation's (an
// architecture import mid-sync) is held by the installation's limit, with no request.
func TestATokenMintedBeforeARefreshStaysUnderItsInstallationsLimit(t *testing.T) {
	fake := startLimitGitHub(t)
	_, a, b := twoProcesses(t, fake)
	clock := &testClock{now: time.Now()}
	a.now = clock.Now
	ctx := context.Background()
	held, err := a.Token(ctx, 7)
	if err != nil {
		t.Fatalf("A's first token: %v", err)
	}
	// Inside tokenExpirySlack of the first token's expiry, so the next caller mints anew, while
	// the first token is still valid at GitHub.
	clock.advance(time.Hour - tokenExpirySlack + time.Minute)
	refreshed, err := a.Token(ctx, 7)
	if err != nil || refreshed == held {
		t.Fatalf("A's refreshed token %q (first %q), %v; want a new token", refreshed, held, err)
	}
	tokenB, err := b.Token(ctx, 7)
	if err != nil {
		t.Fatalf("B's token: %v", err)
	}

	fake.limit("/repos/acme/web/pulls/1")
	if _, status, _, err := b.Read(ctx, tokenB, "/repos/acme/web/pulls/1"); err != nil || status != http.StatusForbidden {
		t.Fatalf("B's limited read: status %d, %v; want GitHub's 403", status, err)
	}
	_, _, _, err = a.Read(ctx, held, "/repos/acme/web/issues/2")
	requireHeld(t, err, "A's read under the token minted before the refresh")
	_, _, _, err = a.Read(ctx, refreshed, "/repos/acme/web/issues/2")
	requireHeld(t, err, "A's read under the refreshed token")
	if got := fake.count("Bearer " + held); got != 0 {
		t.Fatalf("GitHub got %d requests under the token minted before the refresh inside the installation's limit, want none", got)
	}
}

// Two callers that find no token for an installation at once both mint, and each token is under
// the installation's limit: the second mint to land does not drop the first's scope.
func TestTwoConcurrentFirstMintsForOneInstallationAreBothUnderItsLimit(t *testing.T) {
	fake := startLimitGitHub(t)
	_, a, b := twoProcesses(t, fake)
	ctx := context.Background()
	fake.holdMints(2)
	tokens := make([]string, 2)
	errs := make([]error, 2)
	var minting sync.WaitGroup
	for i := range tokens {
		minting.Go(func() { tokens[i], errs[i] = a.Token(ctx, 7) })
	}
	minting.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("A's concurrent mint %d: %v", i, err)
		}
	}
	if got := fake.count("POST /app/installations/7/access_tokens"); got != 2 {
		t.Fatalf("mints that reached GitHub = %d, want both callers' (tokens %q)", got, tokens)
	}
	tokenB, err := b.Token(ctx, 7)
	if err != nil {
		t.Fatalf("B's token: %v", err)
	}

	fake.limit("/repos/acme/web/pulls/1")
	if _, status, _, err := b.Read(ctx, tokenB, "/repos/acme/web/pulls/1"); err != nil || status != http.StatusForbidden {
		t.Fatalf("B's limited read: status %d, %v; want GitHub's 403", status, err)
	}
	for _, token := range tokens {
		_, _, _, err = a.Read(ctx, token, "/repos/acme/web/issues/2")
		requireHeld(t, err, "A's read under the token "+token)
		if got := fake.count("Bearer " + token); got != 0 {
			t.Fatalf("GitHub got %d requests under %s inside the installation's limit, want none", got, token)
		}
	}
}

// The client remembers a token's installation only until the token expires: the next mint forgets
// every expired token, of any installation, so a long-running process holds no more than the
// tokens still valid.
func TestAnExpiredTokensInstallationIsForgottenAtTheNextMint(t *testing.T) {
	fake := startLimitGitHub(t)
	client := fake.newClient(t)
	clock := &testClock{now: time.Now()}
	client.now = clock.Now
	ctx := context.Background()
	var expired []string
	for _, installation := range []int64{7, 9} {
		token, err := client.Token(ctx, installation)
		if err != nil {
			t.Fatalf("token for installation %d: %v", installation, err)
		}
		expired = append(expired, token)
	}
	clock.advance(time.Hour + time.Minute)
	current, err := client.Token(ctx, 7)
	if err != nil {
		t.Fatalf("token past the first ones' expiry: %v", err)
	}
	for _, token := range expired {
		if scope := client.TokenScope(token); scope != "" {
			t.Fatalf("expired token %s still answers scope %q", token, scope)
		}
	}
	if scope := client.TokenScope(current); scope != InstallationScope(7) {
		t.Fatalf("current token's scope = %q, want %q", scope, InstallationScope(7))
	}
}

// A limit one process meets on the App's own calls (installation lookups, token mints, the
// installation list) holds the other process's App calls, with no request. A rate-limited mint is
// the typed *RateLimitError, so every caller tells it from a refusal.
func TestTheAppsLimitMetByOneProcessHoldsTheOthersAppCalls(t *testing.T) {
	fake := startLimitGitHub(t)
	database, a, b := twoProcesses(t, fake)
	ctx := context.Background()
	tokenB, err := b.Token(ctx, 7)
	if err != nil {
		t.Fatalf("B's token: %v", err)
	}

	fake.limit("/app/installations/9/access_tokens")
	_, err = a.Token(ctx, 9)
	if limited, ok := AsRateLimit(err); !ok || limited.Held || limited.Status != http.StatusForbidden || limited.Wait != time.Minute {
		t.Fatalf("A's limited mint: err = %v, want GitHub's own 403 as a *RateLimitError of a minute", err)
	}
	requests := fake.count()
	_, err = b.Token(ctx, 9)
	requireHeld(t, err, "B's token mint")
	_, err = b.ListInstallations(ctx)
	requireHeld(t, err, "B's installation list")
	_, err = b.Installation(ctx, "acme", "web")
	requireHeld(t, err, "B's installation lookup")
	if got := fake.count(); got != requests {
		t.Fatalf("GitHub got %d more requests inside the App's limit, want none", got-requests)
	}
	if _, status, _, err := b.Read(ctx, tokenB, "/repos/acme/web/issues/2"); err != nil || status != http.StatusOK {
		t.Fatalf("B's read under an installation's token inside the App's limit: status %d, %v; want GitHub's 200", status, err)
	}

	fake.limit("")
	passLimits(t, database)
	if _, err := b.Token(ctx, 9); err != nil {
		t.Fatalf("B's token mint once the limit passed: %v", err)
	}
	if installations, err := b.ListInstallations(ctx); err != nil || len(installations) != 1 {
		t.Fatalf("B's installation list once the limit passed: %v, %v", installations, err)
	}
}

// endingLimits is the shared ledger of a caller whose time ends while GitHub's limit is being
// recorded: Record first ends the caller's context, as a CI reconcile pass's bound falls just
// after GitHub answers, and then records through the real ledger.
type endingLimits struct {
	Limits
	end context.CancelFunc
}

func (l endingLimits) Record(ctx context.Context, scope string, wait time.Duration) error {
	l.end()
	return l.Limits.Record(ctx, scope, wait)
}

// A limit GitHub answers reaches the ledger every process shares even when its caller's time
// ends while it is recorded. The answer is a fact about the App's budget, not about the caller,
// and a limit left out of the ledger lets every other process ask GitHub inside it.
func TestALimitGitHubAnsweredIsRecordedWhenTheCallersTimeEnds(t *testing.T) {
	fake := startLimitGitHub(t)
	database := storetest.Open(t)
	ctx, end := context.WithCancel(context.Background())
	defer end()
	a := fake.newClient(t, WithLimits(endingLimits{Limits: ratelimit.New(database.Pool), end: end}))
	b := fake.newClient(t, WithLimits(ratelimit.New(storetest.SecondProcess(t, database).Pool)))

	fake.limit("/app/installations/9/access_tokens")
	_, err := a.Token(ctx, 9)
	if limited, ok := AsRateLimit(err); !ok || limited.Held || limited.Status != http.StatusForbidden {
		t.Fatalf("A's limited mint: err = %v, want GitHub's own 403 as a *RateLimitError", err)
	}
	if ctx.Err() == nil {
		t.Fatal("A's context did not end while the limit was recorded, so this test proves nothing")
	}
	requests := fake.count()
	_, err = b.Token(context.Background(), 9)
	requireHeld(t, err, "B's token mint after A's time ended as A recorded the limit")
	if got := fake.count(); got != requests {
		t.Fatalf("GitHub got %d more requests inside the limit A met, want none", got-requests)
	}
}

// The webhook redelivery sweep's own backoff doubles to an hour under its own scope; the App's
// token mints obey only GitHub's own wait, so a strike against redelivery never holds them.
func TestARedeliveryStrikeDoesNotHoldATokenMint(t *testing.T) {
	fake := startLimitGitHub(t)
	database := storetest.Open(t)
	limits := ratelimit.New(database.Pool)
	ctx := context.Background()
	for range 7 {
		if _, err := limits.Strike(ctx, "redelivery", time.Minute); err != nil {
			t.Fatalf("strike: %v", err)
		}
	}
	client := fake.newClient(t, WithLimits(limits))
	if _, err := client.Token(ctx, 7); err != nil {
		t.Fatalf("token mint under a redelivery backoff: %v", err)
	}
	if got := fake.count("POST /app/installations/7/access_tokens"); got != 1 {
		t.Fatalf("token mints that reached GitHub = %d, want 1", got)
	}
}

// A shared limit that cannot be read leaves the request to GitHub, whose own answer still limits
// it: the database failing never stops every GitHub call.
func TestAnUnreadableLimitLetsTheRequestGo(t *testing.T) {
	fake := startLimitGitHub(t)
	gone := storetest.SecondProcess(t, storetest.Open(t))
	gone.Pool.Close()
	client := fake.newClient(t, WithLimits(ratelimit.New(gone.Pool)))
	if _, err := client.Token(context.Background(), 7); err != nil {
		t.Fatalf("token mint with the limits unreadable: %v", err)
	}
	if got := fake.count("POST /app/installations/7/access_tokens"); got != 1 {
		t.Fatalf("token mints that reached GitHub = %d, want 1", got)
	}
}

// testClock is a clock a test moves by hand.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// A repository's installation is remembered for installationsCacheTTL and no longer: an App moved
// to another installation is found by every process within ten minutes, not only once a token for
// the old installation fails to mint (it still mints here: the old installation exists).
func TestARepositorysInstallationIsLookedUpAgainAfterTheTTL(t *testing.T) {
	fake := startLimitGitHub(t)
	client := fake.newClient(t)
	clock := &testClock{now: time.Now()}
	client.now = clock.Now
	ctx := context.Background()

	token, err := client.RepositoryToken(ctx, "acme", "web")
	if err != nil || !strings.HasPrefix(token, "ghs_7_") {
		t.Fatalf("first token %q, %v; want installation 7's", token, err)
	}
	fake.moveInstallation(8)
	clock.advance(installationsCacheTTL - time.Minute)
	if token, err := client.RepositoryToken(ctx, "acme", "web"); err != nil || !strings.HasPrefix(token, "ghs_7_") {
		t.Fatalf("token inside the TTL %q, %v; want installation 7's, from the remembered installation", token, err)
	}
	if got := fake.count("GET /repos/acme/web/installation"); got != 1 {
		t.Fatalf("installation lookups inside the TTL = %d, want 1", got)
	}

	clock.advance(2 * time.Minute)
	if token, err := client.RepositoryToken(ctx, "acme", "web"); err != nil || !strings.HasPrefix(token, "ghs_8_") {
		t.Fatalf("token past the TTL %q, %v; want installation 8's, looked up again", token, err)
	}
	if got := fake.count("GET /repos/acme/web/installation"); got != 2 {
		t.Fatalf("installation lookups past the TTL = %d, want 2", got)
	}
}
