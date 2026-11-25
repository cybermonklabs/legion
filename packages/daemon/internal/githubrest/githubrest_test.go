package githubrest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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
	if err := client.Post(context.Background(), "/git/refs", map[string]string{"ref": "refs/heads/new"}, &created); err != nil || created.Ref != "refs/heads/new" {
		t.Fatalf("Post = %+v, %v; want the 201 answer read", created, err)
	}
	err := client.Post(context.Background(), "/git/refs?x=1", map[string]string{"ref": "refs/heads/taken"}, nil)
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
// x-ratelimit-resource names, or the REST API's core budget when it names none. Any other error is
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
			err := Client{Token: "token", API: server.URL + "/repos/acme/widgets"}.Get(context.Background(), "/git/ref/heads/main", nil)
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

// GetPages and GetListPages follow GitHub's next pages wherever GitHub names them on the API's own
// origin, its /repositories/<id>/ form included, and an answer on such a page names that page's
// path. A next page on another host is refused before the token is sent there.
func TestGetPagesFollowsNextPagesOnTheAPIsOriginOnly(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
		if strings.HasSuffix(r.URL.Path, "/check-runs") {
			w.Write([]byte(`{"total_count":1,"check_runs":["leaked"]}`))
			return
		}
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
		case "/repos/acme/widgets/commits/head/check-runs":
			next := *nextHost.Load() + "/repositories/42/commits/head/check-runs?per_page=100&page=2"
			w.Header().Set("Link", "<"+next+`>; rel="next"`)
			w.Write([]byte(`{"total_count":2,"check_runs":["first"]}`))
		case "/repositories/42/commits/head/check-runs":
			w.Write([]byte(`{"total_count":2,"check_runs":["second"]}`))
		default:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := Client{Token: "token", API: server.URL + "/repos/acme/widgets"}

	nextHost.Store(&server.URL)
	pageTwo.Store(http.StatusOK)
	if got, err := GetPages[string](context.Background(), client, "/rules/branches/main"); err != nil || !slices.Equal(got, []string{"first", "second"}) {
		t.Fatalf("GetPages = %v, %v; want both pages", got, err)
	}
	if got, err := GetListPages[string](context.Background(), client, "/commits/head/check-runs", "check_runs"); err != nil || !slices.Equal(got, []string{"first", "second"}) {
		t.Fatalf("GetListPages = %v, %v; want both pages' check runs", got, err)
	}

	pageTwo.Store(http.StatusForbidden)
	_, err := GetPages[string](context.Background(), client, "/rules/branches/main")
	var answer *Answer
	if !errors.As(err, &answer) || answer.Path != "/repositories/42/rules/branches/main" || answer.Status != http.StatusForbidden {
		t.Fatalf("GetPages with page 2 refused = %v, want an *Answer naming /repositories/42/rules/branches/main with 403", err)
	}

	nextHost.Store(&other.URL)
	got, err := GetPages[string](context.Background(), client, "/rules/branches/main")
	if err == nil || !strings.Contains(err.Error(), other.URL) || got != nil {
		t.Fatalf("GetPages with a next page on another host = %v, %v; want an error naming it and no items", got, err)
	}
	list, err := GetListPages[string](context.Background(), client, "/commits/head/check-runs", "check_runs")
	if err == nil || !strings.Contains(err.Error(), other.URL) || list != nil {
		t.Fatalf("GetListPages with a next page on another host = %v, %v; want an error naming it and no items", list, err)
	}
	if n := elsewhere.Load(); n != 0 {
		t.Fatalf("the other host saw %d requests, want none", n)
	}
}

// A list GitHub answers as an object's field is read from that field, and an answer without it is
// an error naming the field, never an empty list a caller would read as nothing required or run.
func TestGetListPagesRefusesAnAnswerWithoutTheField(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/acme/widgets/actions/runs" {
			w.Write([]byte(`{"total_count":1,"workflow_runs":[{"id":37}]}`))
			return
		}
		w.Write([]byte(`{"total_count":0,"message":"no list here"}`))
	}))
	defer server.Close()
	client := Client{Token: "token", API: server.URL + "/repos/acme/widgets"}
	type run struct {
		ID int `json:"id"`
	}
	if runs, err := GetListPages[run](context.Background(), client, "/actions/runs", "workflow_runs"); err != nil || len(runs) != 1 || runs[0].ID != 37 {
		t.Fatalf("GetListPages = %+v, %v; want the one run read from workflow_runs", runs, err)
	}
	runs, err := GetListPages[run](context.Background(), client, "/commits/head/check-runs", "check_runs")
	if err == nil || runs != nil || err.Error() != `GitHub answered GET /commits/head/check-runs with no "check_runs" list` {
		t.Fatalf("GetListPages of an answer without the field = %+v, %v; want an error naming check_runs", runs, err)
	}
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
			if err := client.Get(context.Background(), "/collaborators/a-writer/permission", nil); !errors.As(err, &answer) {
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
	if err := client.Get(context.Background(), "/collaborators/a-writer/permission", nil); !errors.As(err, &answer) {
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
	if err := client.Get(context.Background(), "", nil); !errors.As(err, &answer) {
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
// is GitHub's rate limit is RateLimited's rule: a 429, or a 403 with X-RateLimit-Remaining: 0, a
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := &http.Response{StatusCode: tc.status, Header: http.Header{}}
			for name, value := range tc.headers {
				response.Header.Set(name, value)
			}
			if limited, budget := Limit(response, []byte(tc.body)); limited != tc.limited || budget != tc.budget {
				t.Errorf("Limit = %v, %q; want %v, %q", limited, budget, tc.limited, tc.budget)
			}
			if wait := retryAfter(response, []byte(tc.body), now); wait != tc.wait {
				t.Errorf("the wait = %s, want %s", wait, tc.wait)
			}
		})
	}
}

// GitHub's answer that a query ran out of its time is read from the answer's body, however the
// caller wrapped the answer (NamesTimeout); an error that only quotes the words, and GitHub's
// other answers, are not it. Answered says which outcomes are GitHub's answers: its data, an
// answer other than 2xx and GraphQL errors, wrapped or not; a deadline, or any error that carries
// no answer of GitHub's, is none.
func TestATimeoutAndAnAnswerAreReadFromGitHubsAnswer(t *testing.T) {
	timedOut := &Answer{Method: http.MethodPost, Path: "/graphql", Status: http.StatusBadGateway,
		Body: `{"message":"We couldn't respond to your request in time. Sorry about that. Please try resubmitting your request and contact us if the problem persists."}`}
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
