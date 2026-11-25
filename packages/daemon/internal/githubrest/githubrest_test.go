package githubrest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/oauth2call"
	"github.com/sjawhar/envoy/oauth2call/oauth2calltest"
	"github.com/sjawhar/legion/daemon/internal/githubtest"
)

// Post sends its body as JSON with the installation token, reads a 2xx answer such as GitHub's
// 201 Created, and gives any other answer back as an *Answer naming the method, the path and the
// status, so a caller can tell GitHub's refusal from a failed request.
func TestPostSendsJSONAndAnswersWithTheStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var sent struct {
			Ref string `json:"ref"`
		}
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer token" || r.Header.Get("Content-Type") != "application/json" ||
			json.NewDecoder(r.Body).Decode(&sent) != nil {
			http.Error(w, `{"message":"bad request"}`, http.StatusBadRequest)
			return
		}
		if sent.Ref == "refs/heads/taken" {
			http.Error(w, `{"message":"Reference already exists"}`, http.StatusUnprocessableEntity)
			return
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"ref":"` + sent.Ref + `"}`))
	}))
	defer server.Close()
	client := Client{Token: "token", API: server.URL + "/repos/acme/widgets"}

	var created struct {
		Ref string `json:"ref"`
	}
	if err := client.Post(context.Background(), "/git/refs", "", map[string]string{"ref": "refs/heads/new"}, &created); err != nil || created.Ref != "refs/heads/new" {
		t.Fatalf("Post = %+v, %v; want the 201 answer read", created, err)
	}
	err := client.Post(context.Background(), "/git/refs?x=1", "", map[string]string{"ref": "refs/heads/taken"}, nil)
	var answer *Answer
	if !errors.As(err, &answer) || answer.Method != http.MethodPost || answer.Path != "/git/refs" || answer.Status != http.StatusUnprocessableEntity {
		t.Fatalf("Post of a taken ref = %v, want an *Answer for POST /git/refs with 422", err)
	}
}

// RetryAfter reads GitHub's rate limit from the error a call returns, wrapped or not: an *Answer
// for an answer other than 2xx. It names how long GitHub asks the caller to wait and the budget
// the limit spent: a Retry-After, a limit that reports no primary budget spent, or one whose
// message names the secondary limit, whatever x-ratelimit-remaining says, is GitHub's secondary
// limit, which every call of the token meets; a primary limit spends the budget
// x-ratelimit-resource names where it is a resource GitHub documents, or the REST API's core budget
// when it names none or any other (TestARateLimitsBudgetIsAResourceGitHubDocumentsOrTheCoreBudget). Any other error is
// no limit.
func TestRetryAfterNamesTheBudgetALimitSpent(t *testing.T) {
	reset := strconv.FormatInt(time.Now().Add(10*time.Minute).Unix(), 10)
	const limited = `{"message":"API rate limit exceeded"}`
	for _, tc := range []struct {
		name        string
		status      int
		headers     map[string]string
		body        string
		budget      Budget
		least, most time.Duration
	}{
		{"a REST core limit", http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset, "X-RateLimit-Resource": "core"},
			limited, BudgetCore, 9 * time.Minute, 10 * time.Minute},
		{"a REST limit naming no resource", http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset},
			limited, BudgetCore, 9 * time.Minute, 10 * time.Minute},
		{"a secondary limit's 429", http.StatusTooManyRequests, map[string]string{"Retry-After": "17"}, limited, BudgetSecondary, 17 * time.Second, 17 * time.Second},
		{"a secondary limit named by its message", http.StatusForbidden, map[string]string{"X-RateLimit-Remaining": "4321"},
			`{"message":"You have exceeded a secondary rate limit."}`, BudgetSecondary, time.Minute, time.Minute},
		{"a secondary limit named by its message, with no requests left", http.StatusForbidden,
			map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset, "X-RateLimit-Resource": "core"},
			`{"message":"You have exceeded a secondary rate limit."}`, BudgetSecondary, 9 * time.Minute, 10 * time.Minute},
		{"an installation refused", http.StatusForbidden, nil, `{"message":"Resource not accessible by integration"}`, "", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for name, value := range tc.headers {
					w.Header().Set(name, value)
				}
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer server.Close()
			err := Client{Token: "token", API: server.URL + "/repos/acme/widgets"}.Get(context.Background(), "/git/ref/heads/main", "", nil)
			after, budget := RetryAfter(fmt.Errorf("wrapped: %w", err))
			if budget != tc.budget || after < tc.least || after > tc.most {
				t.Fatalf("RetryAfter(%v) = %s, %q; want %q between %s and %s", err, after, budget, tc.budget, tc.least, tc.most)
			}
		})
	}
	if after, budget := RetryAfter(errors.New("not GitHub's")); after != 0 || budget != "" {
		t.Fatalf("RetryAfter of another error = %s, %q; want no limit", after, budget)
	}
}

// GetPages follows GitHub's next pages wherever GitHub names them on the API's own origin, its
// /repositories/<id>/ form included, and an answer on such a page names that page by its number
// under the caller's path. A next page on another host is refused before the token is sent there,
// naming the origin the token is sent to and nothing of the page GitHub named.
func TestGetPagesFollowsNextPagesOnTheAPIsOriginOnly(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		elsewhere.Add(1)
		w.Write([]byte(`["leaked"]`))
	}))
	defer other.Close()
	var nextHost atomic.Pointer[string]
	var pageTwo atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widgets/rules/branches/main":
			next := *nextHost.Load() + "/repositories/42/rules/branches/main?per_page=100&page=2"
			w.Header().Set("Link", "<"+next+`>; rel="next", <`+next+`>; rel="last"`)
			w.Write([]byte(`["first"]`))
		case "/repositories/42/rules/branches/main":
			if status := int(pageTwo.Load()); status != http.StatusOK {
				http.Error(w, `{"message":"Resource not accessible by integration"}`, status)
				return
			}
			w.Write([]byte(`["second"]`))
		default:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := Client{Token: "token", API: server.URL + "/repos/acme/widgets"}

	nextHost.Store(&server.URL)
	pageTwo.Store(http.StatusOK)
	if got, err := GetPages[string](context.Background(), client, "/rules/branches/main", ""); err != nil || !slices.Equal(got, []string{"first", "second"}) {
		t.Fatalf("GetPages = %v, %v; want both pages", got, err)
	}

	pageTwo.Store(http.StatusForbidden)
	_, err := GetPages[string](context.Background(), client, "/rules/branches/main", "")
	var answer *Answer
	if !errors.As(err, &answer) || answer.Path != "/rules/branches/main (page 2)" || answer.Status != http.StatusForbidden {
		t.Fatalf("GetPages with page 2 refused = %v, want an *Answer naming /rules/branches/main (page 2) with 403", err)
	}

	nextHost.Store(&other.URL)
	got, err := GetPages[string](context.Background(), client, "/rules/branches/main", "")
	if err == nil || !strings.Contains(err.Error(), "is not on "+server.URL) || strings.Contains(err.Error(), other.URL) || got != nil {
		t.Fatalf("GetPages with a next page on another host = %v, %v; want an error naming the API's origin alone and no items", got, err)
	}
	if n := elsewhere.Load(); n != 0 {
		t.Fatalf("the other host saw %d requests, want none", n)
	}
}

// TestAPageThatDoesNotDecodePrintsNoTokenItEchoes: a page GitHub answers, or a stand-in answers in
// its place, can echo the call's token into a time its items hold, and a decoder's error quotes
// such a time whole. GetPages refuses the page with an error that holds no run of the token.
func TestAPageThatDoesNotDecodePrintsNoTokenItEchoes(t *testing.T) {
	token := githubtest.InstallationToken(t.Name())
	server := githubtest.Serve(t, githubtest.Spec{Token: token, Handle: func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/widgets/pulls/7/reviews" {
			githubtest.Unexpected(t, w, r)
			return
		}
		echo := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		fmt.Fprintf(w, `[{"id":1,"submitted_at":%q}]`, echo)
	}})
	client := Client{Token: token, API: server.URL + "/repos/acme/widgets"}
	type review struct {
		ID          int64     `json:"id"`
		SubmittedAt time.Time `json:"submitted_at"`
	}
	reviews, err := GetPages[review](context.Background(), client, "/pulls/7/reviews", "")
	var answer *Answer
	if err == nil || reviews != nil || errors.As(err, &answer) {
		t.Fatalf("GetPages of a page that does not decode = %+v, %v; want the page refused as not the shape the call reads", reviews, err)
	}
	oauth2calltest.RequireNoPiece(t, "githubrest.decode-echo/page", err.Error(), token)
}

// GitHub's secondary rate limit can answer a 403 with only its message to say so: no retry-after,
// and x-ratelimit-remaining well above 0. That answer is a rate limit, waited the minute GitHub's
// documentation names for a limit that names no wait. A 403 whose message is anything else is not.
func TestASecondaryRateLimitNamedOnlyByItsMessageIsARateLimit(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		limited   bool
		wantAfter time.Duration
	}{
		{name: "a secondary rate limit", body: `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`,
			limited: true, wantAfter: time.Minute},
		{name: "an installation refused", body: `{"message":"Resource not accessible by integration"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-RateLimit-Remaining", "4321")
				http.Error(w, tc.body, http.StatusForbidden)
			}))
			defer server.Close()
			client := Client{Token: "token", API: server.URL + "/repos/acme/widgets"}

			var answer *Answer
			if err := client.Get(context.Background(), "/collaborators/a-writer/permission", "", nil); !errors.As(err, &answer) {
				t.Fatalf("Get = %v, want an *Answer", err)
			}
			if answer.RateLimited != tc.limited || answer.RetryAfter != tc.wantAfter {
				t.Fatalf("the 403 = rate limited %v, retry after %s; want %v, %s", answer.RateLimited, answer.RetryAfter, tc.limited, tc.wantAfter)
			}
		})
	}
}

// A 403 naming x-ratelimit-reset far in the future - a host clock behind GitHub's, or a limit
// GitHub's documentation allows up to an hour out - waits no longer than an hour, GitHub's primary
// rate limit's window, rather than stretching the nak delay and the held limit out past it.
func TestARateLimitsResetFarInTheFutureWaitsNoLongerThanAnHour(t *testing.T) {
	reset := strconv.FormatInt(time.Now().Add(6*time.Hour).Unix(), 10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", reset)
		http.Error(w, `{"message":"rate limited"}`, http.StatusForbidden)
	}))
	defer server.Close()
	client := Client{Token: "token", API: server.URL + "/repos/acme/widgets"}

	var answer *Answer
	if err := client.Get(context.Background(), "/collaborators/a-writer/permission", "", nil); !errors.As(err, &answer) {
		t.Fatalf("Get = %v, want an *Answer", err)
	}
	if !answer.RateLimited || answer.RetryAfter != time.Hour {
		t.Fatalf("a reset six hours out = rate limited %v, retry after %s; want true and an hour's cap", answer.RateLimited, answer.RetryAfter)
	}
}

// Every answer other than 2xx carries its rate-limit headers as GitHub sent them, which decide
// nothing: a 403 naming the primary limit in its body alone, with calls left in its headers, is no
// rate limit to this client, as before, and still carries what GitHub said for its caller's log.
func TestAnAnswerCarriesTheRateLimitHeadersGitHubSent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "15000")
		w.Header().Set("X-RateLimit-Remaining", "14671")
		w.Header().Set("X-RateLimit-Reset", "1700000000")
		w.Header().Set("X-RateLimit-Resource", "core")
		http.Error(w, `{"message":"API rate limit exceeded for installation ID 7."}`, http.StatusForbidden)
	}))
	defer server.Close()
	client := Client{Token: "token", API: server.URL + "/repos/acme/widgets"}

	var answer *Answer
	if err := client.Get(context.Background(), "", "", nil); !errors.As(err, &answer) {
		t.Fatalf("Get = %v, want an *Answer", err)
	}
	if answer.RateLimited || answer.RetryAfter != 0 || answer.Budget != "" {
		t.Fatalf("the answer = rate limited %v, retry after %s, budget %q; want no limit", answer.RateLimited, answer.RetryAfter, answer.Budget)
	}
	if want := (LimitHeaders{Limit: "15000", Remaining: "14671", Reset: "1700000000", Resource: "core"}); answer.Limit != want {
		t.Fatalf("the answer's rate-limit headers = %+v, want %+v as GitHub sent them", answer.Limit, want)
	}
}

// Limit reads an answer another client received as this client reads its own. Whether the answer
// is GitHub's rate limit is rateLimited's rule: a 429, or a 403 with X-RateLimit-Remaining: 0, a
// Retry-After, or a message naming the secondary limit (spelled "rate limit" or "rate-limit") or
// GitHub's abuse detection mechanism. Which limit it is, is spent's rule, which never reads the
// status: the secondary limit where the message names it, whatever X-RateLimit-Remaining says
// (octokit's throttling plugin reads a message saying "secondary rate" before that header, and
// this package reads the older "abuse detection mechanism" the same way), or where the answer has
// a Retry-After or no X-RateLimit-Remaining: 0; and otherwise the core limit (or the budget
// X-RateLimit-Resource names). So only a 429 or a 403 with X-RateLimit-Remaining: 0, no Retry-After
// and no such message is the core limit. The wait is retryAfter's, the one this client's own
// Answer carries: the Retry-After, else until the reset where no requests are left, as GitHub's
// documentation asks of the secondary limit too, else a minute. An answer that is no limit asks
// for no wait.
func TestLimitSaysWhichLimitAnAnswerIs(t *testing.T) {
	now := time.Date(2026, 11, 15, 16, 0, 0, 0, time.UTC)
	reset := strconv.FormatInt(now.Add(10*time.Minute).Unix(), 10)
	const (
		secondary = `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`
		hyphen    = `{"message":"You have exceeded a secondary rate-limit. Please wait a few minutes before you try again."}`
		abuse     = `{"message":"You have triggered an abuse detection mechanism. Please wait a few minutes before you try again."}`
	)
	for _, tc := range []struct {
		name    string
		status  int
		headers map[string]string
		body    string
		limited bool
		budget  Budget
		wait    time.Duration
	}{
		{name: "a 429 with nothing else", status: http.StatusTooManyRequests, body: `{"message":"invented"}`,
			limited: true, budget: BudgetSecondary, wait: time.Minute},
		{name: "a 429 with no requests left and a reset is the core limit", status: http.StatusTooManyRequests,
			headers: map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset},
			body:    `{"message":"invented"}`, limited: true, budget: BudgetCore, wait: 10 * time.Minute},
		{name: "a 403 with no requests left and a reset", status: http.StatusForbidden,
			headers: map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset, "X-RateLimit-Resource": "core"},
			body:    `{"message":"API rate limit exceeded for installation ID 1."}`, limited: true, budget: BudgetCore, wait: 10 * time.Minute},
		{name: "a 403 with a Retry-After", status: http.StatusForbidden, headers: map[string]string{"Retry-After": "30"},
			body: `{"message":"invented"}`, limited: true, budget: BudgetSecondary, wait: 30 * time.Second},
		{name: "a 403 with requests left, naming the secondary limit", status: http.StatusForbidden,
			headers: map[string]string{"X-RateLimit-Remaining": "21", "X-RateLimit-Reset": reset},
			body:    secondary, limited: true, budget: BudgetSecondary, wait: time.Minute},
		{name: "a 403 naming the secondary rate-limit, with a hyphen and no headers", status: http.StatusForbidden,
			body: hyphen, limited: true, budget: BudgetSecondary, wait: time.Minute},
		{name: "a 403 naming the abuse detection mechanism", status: http.StatusForbidden,
			body: abuse, limited: true, budget: BudgetSecondary, wait: time.Minute},
		{name: "a 403 that is no limit", status: http.StatusForbidden,
			headers: map[string]string{"X-RateLimit-Remaining": "4321", "X-RateLimit-Reset": reset},
			body:    `{"message":"Resource not accessible by integration"}`},
		{name: "a 403 naming the secondary limit with no requests left and no Retry-After is the secondary limit", status: http.StatusForbidden,
			headers: map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset},
			body:    secondary, limited: true, budget: BudgetSecondary, wait: 10 * time.Minute},
		{name: "a 403 naming the secondary rate-limit, with a hyphen, with no requests left and no Retry-After", status: http.StatusForbidden,
			headers: map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset},
			body:    hyphen, limited: true, budget: BudgetSecondary, wait: 10 * time.Minute},
		{name: "a 429 naming the secondary limit with no requests left and no Retry-After", status: http.StatusTooManyRequests,
			headers: map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset, "X-RateLimit-Resource": "core"},
			body:    secondary, limited: true, budget: BudgetSecondary, wait: 10 * time.Minute},
		// A Retry-After past what a time.Duration holds in nanoseconds wrapped negative when its
		// seconds were multiplied whole, and a caller slept the negative wait as none. The seconds
		// are clamped to a day first (maxRetryAfter).
		{name: "a Retry-After past what a duration holds", status: http.StatusForbidden,
			headers: map[string]string{"Retry-After": "9223372037"}, body: `{"message":"invented"}`,
			limited: true, budget: BudgetSecondary, wait: maxRetryAfter},
		{name: "a Retry-After past an int64", status: http.StatusForbidden,
			headers: map[string]string{"Retry-After": "99999999999999999999"}, body: `{"message":"invented"}`,
			limited: true, budget: BudgetSecondary, wait: time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := &http.Response{StatusCode: tc.status, Header: http.Header{}}
			for name, value := range tc.headers {
				response.Header.Set(name, value)
			}
			limited, budget := Limit(response, []byte(tc.body))
			if limited != tc.limited || budget != tc.budget {
				t.Errorf("Limit = %v, %q; want %v, %q", limited, budget, tc.limited, tc.budget)
			}
			if wait := retryAfter(response, limited, now); wait != tc.wait {
				t.Errorf("the wait = %s, want %s", wait, tc.wait)
			}
		})
	}
}

// GitHub's answer that a query ran out of its time is read from the answer's body as it came,
// however the caller wrapped the answer (NamesTimeout); an error that only quotes the words, and
// GitHub's other answers, are not it. The answer is built as the client builds one (AnswerOf of
// the body it read): NamesTimeout decides on the body as it came (Answer.Says), never on Body, the
// rule's print of it. Answered says which outcomes are GitHub's answers: its data, an answer other
// than 2xx and GraphQL errors, wrapped or not; a deadline, or any error that carries no answer of
// GitHub's, is none.
func TestATimeoutAndAnAnswerAreReadFromGitHubsAnswer(t *testing.T) {
	const said = `{"message":"We couldn't respond to your request in time. Sorry about that. Please try resubmitting your request and contact us if the problem persists."}`
	response := &http.Response{StatusCode: http.StatusBadGateway, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(said))}
	read, err := oauth2call.ReadAnswer(response, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	timedOut := AnswerOf(http.MethodPost, "/graphql", response, read)
	for _, tc := range []struct {
		name     string
		err      error
		timeout  bool
		answered bool
	}{
		{"no error", nil, false, true},
		{"GitHub's timeout", timedOut, true, true},
		{"GitHub's timeout, wrapped", fmt.Errorf("read pull requests [86 87] of acme/widgets: %w", timedOut), true, true},
		{"a 502 of another body", &Answer{Method: http.MethodPost, Path: "/graphql", Status: http.StatusBadGateway, Body: `{"message":"Server Error"}`}, false, true},
		{"GraphQL errors, wrapped", fmt.Errorf("query: %w", GraphQLErrors{{Type: "NOT_FOUND", Message: "Could not resolve to a PullRequest with the number of 87."}}), false, true},
		{"the words outside an answer", errors.New("we couldn't respond to your request in time"), false, false},
		{"a deadline", fmt.Errorf("post: %w", context.DeadlineExceeded), false, false},
	} {
		if got := NamesTimeout(tc.err); got != tc.timeout {
			t.Errorf("%s: NamesTimeout = %t, want %t", tc.name, got, tc.timeout)
		}
		if got := Answered(tc.err); got != tc.answered {
			t.Errorf("%s: Answered = %t, want %t", tc.name, got, tc.answered)
		}
	}
}
