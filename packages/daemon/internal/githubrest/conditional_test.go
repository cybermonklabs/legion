package githubrest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// A client with ETags sends every GET conditional on the ETag GitHub gave the last answer for the
// same URL, and reads GitHub's 304 - which costs the installation's rate limit nothing - as that
// answer, its next page too, so every page of a list GitHub has not changed reads as before. A
// changed answer is read whole and kept in place of the old one, and an answer with no ETag keeps
// nothing, so the read after it is whole again. A client without ETags sends no condition.
func TestAConditionalGetReadsA304AsTheAnswerItKept(t *testing.T) {
	var mu sync.Mutex
	versions := map[string]string{"/repos/acme/widgets/pulls/86": "v1", "/repos/acme/widgets/rules/branches/main": "r1", "/repositories/42/rules/branches/main": "p1"}
	var conditions []string
	var notModified int
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		version, ok := versions[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		conditions = append(conditions, r.Header.Get("If-None-Match"))
		if r.URL.Path == "/repos/acme/widgets/rules/branches/main" {
			w.Header().Set("Link", "<"+server.URL+`/repositories/42/rules/branches/main?per_page=100&page=2>; rel="next"`)
		}
		etag := `"` + version + `"`
		if version != "none" {
			w.Header().Set("ETag", etag)
		}
		if version != "none" && r.Header.Get("If-None-Match") == etag {
			notModified++
			w.WriteHeader(http.StatusNotModified)
			return
		}
		switch r.URL.Path {
		case "/repos/acme/widgets/pulls/86":
			w.Write([]byte(`{"number":86,"mergeable_state":"` + version + `"}`))
		default:
			w.Write([]byte(`["` + version + `"]`))
		}
	}))
	defer server.Close()
	etags := NewETags(1 << 20)
	client := Client{Token: "token", API: server.URL + "/repos/acme/widgets", Scope: "implement write acme"}
	conditional := client.WithETags(etags)
	read := func(c Client) string {
		t.Helper()
		var pull struct {
			State string `json:"mergeable_state"`
		}
		if err := c.Get(context.Background(), "/pulls/86", &pull); err != nil {
			t.Fatalf("Get = %v", err)
		}
		return pull.State
	}
	last := func() string {
		mu.Lock()
		defer mu.Unlock()
		return conditions[len(conditions)-1]
	}
	unchanged := func() int {
		mu.Lock()
		defer mu.Unlock()
		return notModified
	}

	if got := read(conditional); got != "v1" || last() != "" {
		t.Fatalf("the first read answered %q sending %q, want v1 and no condition: nothing is kept yet", got, last())
	}
	if got := read(conditional); got != "v1" || last() != `"v1"` || unchanged() != 1 {
		t.Fatalf("the second read answered %q sending %q after %d 304s, want the kept v1 read from GitHub's 304", got, last(), unchanged())
	}
	mu.Lock()
	versions["/repos/acme/widgets/pulls/86"] = "v2"
	mu.Unlock()
	if got := read(conditional); got != "v2" || last() != `"v1"` {
		t.Fatalf("the read of a changed pull request answered %q sending %q, want v2 read whole", got, last())
	}
	if got := read(conditional); got != "v2" || last() != `"v2"` || unchanged() != 2 {
		t.Fatalf("the read after the change answered %q sending %q, want v2 kept in v1's place and read from a 304", got, last())
	}
	mu.Lock()
	versions["/repos/acme/widgets/pulls/86"] = "none"
	mu.Unlock()
	read(conditional)
	if got := read(conditional); got != "none" || last() != "" {
		t.Fatalf("the read after an answer with no ETag answered %q sending %q, want it read whole with no condition", got, last())
	}
	if got := read(client); got != "none" || last() != "" {
		t.Fatalf("a client without ETags answered %q sending %q, want no condition", got, last())
	}

	for pass := 1; pass <= 2; pass++ {
		got, err := GetPages[string](context.Background(), conditional, "/rules/branches/main")
		if err != nil || !slices.Equal(got, []string{"r1", "p1"}) {
			t.Fatalf("pass %d of GetPages = %v, %v; want both pages", pass, got, err)
		}
	}
	if got := unchanged(); got != 4 {
		t.Fatalf("GitHub answered %d reads 304, want the two pulls reads and both pages of the second pass", got)
	}
}

// ETags holds at most its limit of answer bodies: keeping one past it drops the URL read least
// recently, a body larger than the limit is not kept at all, and a URL read again is the most
// recent one.
func TestETagsDropsTheAnswerReadLeastRecently(t *testing.T) {
	etags := NewETags(10)
	etags.keep("a", `"a"`, []byte("aaaa"), "")
	etags.keep("b", `"b"`, []byte("bbbb"), "")
	if _, ok := etags.lookup("a"); !ok {
		t.Fatalf("a is not kept, want both a and b under the limit")
	}
	etags.keep("c", `"c"`, []byte("cccc"), "")
	if _, ok := etags.lookup("b"); ok {
		t.Fatalf("b is kept after c went past the limit, want b, the one read least recently, dropped")
	}
	for _, url := range []string{"a", "c"} {
		if kept, ok := etags.lookup(url); !ok || kept.etag != `"`+url+`"` {
			t.Fatalf("%s is kept %t as %+v, want it kept", url, ok, kept)
		}
	}
	etags.keep("big", `"big"`, []byte("more than ten bytes"), "")
	if _, ok := etags.lookup("big"); ok {
		t.Fatalf("a body over the limit is kept, want it left out")
	}
	if _, ok := etags.lookup("a"); !ok {
		t.Fatalf("a body over the limit dropped a, want what was kept left as it was")
	}
}

// GraphQL posts the query and its variables to the GraphQL endpoint under the installation token,
// asking for the merge-info preview its merge-state reads need, and reads the answer's data. An
// answer whose errors list is not empty is GraphQLErrors, naming each error's type, message and
// path, after the data it carries is read; a refusal is an *Answer, a rate limit as a REST call's
// is, and so is GraphQL's own rate limit, which GitHub answers 200, with the wait its headers ask.
func TestGraphQLReadsTheDataOrTheErrors(t *testing.T) {
	var mu sync.Mutex
	answer, status := `{"data":{"viewer":{"login":"legion-implementer[bot]"}}}`, http.StatusOK
	headers := map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var sent struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if r.Method != http.MethodPost || r.URL.Path != "/graphql" || r.Header.Get("Authorization") != "Bearer token" ||
			r.Header.Get("Accept") != graphQLAccept || json.NewDecoder(r.Body).Decode(&sent) != nil ||
			sent.Query != "query { viewer { login } }" || sent.Variables["owner"] != "acme" {
			http.Error(w, `{"message":"bad request"}`, http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		for name, value := range headers {
			w.Header().Set(name, value)
		}
		w.WriteHeader(status)
		w.Write([]byte(answer))
	}))
	defer server.Close()
	if got := GraphQLEndpoint(""); got != "https://api.github.com/graphql" {
		t.Fatalf("GraphQLEndpoint of the default root = %q", got)
	}
	client := Client{Token: "token", API: GraphQLEndpoint(server.URL + "/")}
	query := func() (string, error) {
		var data struct {
			Viewer struct {
				Login string `json:"login"`
			} `json:"viewer"`
		}
		err := client.GraphQL(context.Background(), "query { viewer { login } }", map[string]any{"owner": "acme"}, &data)
		return data.Viewer.Login, err
	}
	if login, err := query(); err != nil || login != "legion-implementer[bot]" {
		t.Fatalf("GraphQL = %q, %v; want the data's login", login, err)
	}

	mu.Lock()
	answer = `{"data":null,"errors":[{"type":"NOT_FOUND","message":"Could not resolve to a Repository"},{"message":"and another"}]}`
	mu.Unlock()
	_, err := query()
	var graphQLErrors GraphQLErrors
	if !errors.As(err, &graphQLErrors) || len(graphQLErrors) != 2 || !strings.Contains(err.Error(), "NOT_FOUND: Could not resolve to a Repository") {
		t.Fatalf("GraphQL with errors = %v, want GraphQLErrors naming both", err)
	}

	mu.Lock()
	answer = `{"data":{"viewer":{"login":"partial"}},"errors":[{"type":"NOT_FOUND","path":["viewer","p87"],"message":"Could not resolve to a PullRequest with the number of 87."}]}`
	mu.Unlock()
	login, err := query()
	if !errors.As(err, &graphQLErrors) || login != "partial" || len(graphQLErrors) != 1 || len(graphQLErrors[0].Path) != 2 || graphQLErrors[0].Path[1] != "p87" {
		t.Fatalf("GraphQL with data and an error = %q, %v; want the data read and GraphQLErrors naming the path [viewer p87]", login, err)
	}

	mu.Lock()
	reset := time.Now().Add(2 * time.Minute).Unix()
	answer, headers = `{"data":null,"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded for installation ID 1."}]}`,
		map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": strconv.FormatInt(reset, 10), "X-RateLimit-Resource": "graphql"}
	mu.Unlock()
	_, err = query()
	var limited *Answer
	if !errors.As(err, &limited) || !limited.RateLimited || limited.Budget != BudgetGraphQL || limited.RetryAfter < time.Minute || limited.RetryAfter > 2*time.Minute {
		t.Fatalf("GraphQL's own rate limit = %v, want an *Answer that is the GraphQL rate limit, waiting until its reset two minutes on", err)
	}
	mu.Lock()
	headers = map[string]string{}
	mu.Unlock()

	// GitHub may answer a query 200 for its secondary limit with errors of any type, or none: the
	// signals a REST limit carries make it a limit all the same, with the wait they ask, and a
	// message alone waits a minute.
	for _, limit := range []struct {
		name    string
		answer  string
		headers map[string]string
		wait    time.Duration
		budget  Budget
	}{
		{name: "no type, the secondary limit's message and a Retry-After",
			answer:  `{"data":null,"errors":[{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}]}`,
			headers: map[string]string{"Retry-After": "2"}, wait: 2 * time.Second, budget: BudgetSecondary},
		{name: "another type and a Retry-After",
			answer:  `{"data":null,"errors":[{"type":"FORBIDDEN","message":"Try again later."}]}`,
			headers: map[string]string{"Retry-After": "3"}, wait: 3 * time.Second, budget: BudgetSecondary},
		{name: "the secondary limit's message alone",
			answer:  `{"data":null,"errors":[{"message":"You have exceeded a secondary rate limit."}]}`,
			headers: map[string]string{}, wait: time.Minute, budget: BudgetSecondary},
	} {
		mu.Lock()
		answer, headers = limit.answer, limit.headers
		mu.Unlock()
		_, err := query()
		var held *Answer
		if !errors.As(err, &held) || !held.RateLimited || held.Status != http.StatusOK || held.RetryAfter != limit.wait || held.Budget != limit.budget {
			t.Fatalf("GraphQL answered 200 with %s = %v (%+v), want an *Answer that is the %s limit, waiting %s", limit.name, err, held, limit.budget, limit.wait)
		}
	}
	mu.Lock()
	headers = map[string]string{}
	mu.Unlock()

	mu.Lock()
	answer, status = `{"message":"You have exceeded a secondary rate limit"}`, http.StatusForbidden
	mu.Unlock()
	_, err = query()
	var refused *Answer
	if !errors.As(err, &refused) || !refused.RateLimited || refused.Budget != BudgetSecondary || refused.Path != "/graphql" {
		t.Fatalf("GraphQL refused for a secondary limit = %v, want an *Answer that is the secondary rate limit", err)
	}
}

// Clients of two scopes that share one ETags keep their answers apart: a client of one scope
// sends no ETag another scope's read kept, so it never reads that scope's answer from a 304, and
// each scope's own second read is conditional on its own first. A client with no scope reads
// whole every time, its ETags or not, and keeps nothing: no scope keeps its answers apart.
func TestTwoScopesKeepTheirConditionalAnswersApart(t *testing.T) {
	var mu sync.Mutex
	var sent []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sent = append(sent, r.Header.Get("Authorization")+" "+r.Header.Get("If-None-Match"))
		mu.Unlock()
		etag := `"` + strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") + `"`
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Write([]byte(`{"seen_by":` + etag + `}`))
	}))
	defer server.Close()
	etags := NewETags(1 << 20)
	read := func(token, scope string) string {
		t.Helper()
		var answer struct {
			SeenBy string `json:"seen_by"`
		}
		client := Client{Token: token, API: server.URL + "/repos/acme/widgets", Scope: scope}.WithETags(etags)
		if err := client.Get(context.Background(), "/pulls/86", &answer); err != nil {
			t.Fatalf("Get under %s = %v", scope, err)
		}
		return answer.SeenBy
	}
	if got := read("implement", "implement write acme"); got != "implement" {
		t.Fatalf("the implement scope read %q, want its own answer", got)
	}
	if got := read("review", "review read acme"); got != "review" {
		t.Fatalf("the review scope read %q, want its own answer, not the implement scope's", got)
	}
	if got := read("implement", "implement write acme"); got != "implement" {
		t.Fatalf("the implement scope's second read answered %q, want its own kept answer", got)
	}
	for range 2 {
		if got := read("bare", ""); got != "bare" {
			t.Fatalf("a client with no scope read %q, want its own answer read whole", got)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"Bearer implement ", "Bearer review ", `Bearer implement "implement"`, "Bearer bare ", "Bearer bare "}
	if !slices.Equal(sent, want) {
		t.Fatalf("GitHub was sent %q, want %q: no scope sends another scope's ETag, and no client without a scope sends one", sent, want)
	}
}
