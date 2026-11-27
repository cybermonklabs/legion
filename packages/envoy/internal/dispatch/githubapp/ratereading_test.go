package githubapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/githubrate"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/ratelimit"
	"github.com/sjawhar/envoy/internal/dispatch/githubreads"
	"github.com/sjawhar/envoy/internal/githubtest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// rateGitHub is a GitHub App API for the rate reading's own methods: the App (GET /app, answered
// with app), its installation list (installations, read under the App's JWT and counted), token
// mints of any installation, and the calls a token makes, each answered with the installation's
// counter in GitHub's X-RateLimit headers. limitedPath is answered as GitHub answers a spent
// budget: 403 with no requests remaining; secondaryPath as it answers a secondary limit: 403 with
// requests remaining and Retry-After 90.
type rateGitHub struct {
	mu            sync.Mutex
	app           string
	appStatus     int
	listings      int
	installed     []int64
	minted        int
	limitedPath   string
	secondaryPath string
	requests      []string
	reset         int64
}

func (g *rateGitHub) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.requests = append(g.requests, r.Method+" "+r.URL.Path)
	switch {
	case r.URL.Path == "/app":
		if g.appStatus != 0 {
			w.WriteHeader(g.appStatus)
		}
		_, _ = fmt.Fprint(w, g.app)
		return
	case r.URL.Path == "/app/installations":
		g.listings++
		var listed []string
		if r.URL.Query().Get("page") == "1" {
			for _, id := range g.installed {
				listed = append(listed, fmt.Sprintf(`{"id":%d,"app_slug":"example-app","account":{"login":"acme"}}`, id))
			}
		}
		_, _ = fmt.Fprint(w, "["+strings.Join(listed, ",")+"]")
		return
	case strings.HasPrefix(r.URL.Path, "/app/installations/") && strings.HasSuffix(r.URL.Path, "/access_tokens"):
		g.minted++
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/app/installations/"), "/access_tokens")
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"token":"ghs_rate_%s_%d","expires_at":%q}`, id, g.minted, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		return
	}
	resource := githubrate.Core
	if r.URL.Path == "/graphql" {
		resource = githubrate.GraphQL
	}
	header := w.Header()
	header.Set("X-RateLimit-Resource", resource)
	header.Set("X-RateLimit-Limit", "5000")
	header.Set("X-RateLimit-Reset", strconv.FormatInt(g.reset, 10))
	if r.URL.Path == g.limitedPath {
		header.Set("X-RateLimit-Used", "5000")
		header.Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, `{"message":"API rate limit exceeded for installation ID 7."}`)
		return
	}
	if r.URL.Path == g.secondaryPath {
		header.Set("X-RateLimit-Used", "10")
		header.Set("X-RateLimit-Remaining", "4990")
		header.Set("Retry-After", "90")
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`)
		return
	}
	header.Set("X-RateLimit-Used", "10")
	header.Set("X-RateLimit-Remaining", "4990")
	if r.URL.Path == "/graphql" {
		_, _ = fmt.Fprint(w, `{"data":{"viewer":{"login":"example-app[bot]"}}}`)
		return
	}
	_, _ = fmt.Fprint(w, `{"state":"open"}`)
}

// startRateGitHub starts the fake with one installation, 7, and builds a client against it with
// options.
func startRateGitHub(t *testing.T, options ...Option) (*rateGitHub, *Client) {
	t.Helper()
	fake := &rateGitHub{app: `{"id":4242,"slug":"example-app"}`, installed: []int64{7}, reset: time.Now().Add(time.Hour).Unix()}
	base := githubtest.Server(t, githubtest.Spec{Handle: fake.serve})
	_, pemText := testKeyPEM(t)
	client, err := New(&auth.AppConfig{ClientID: "Iv1.rate", PEM: pemText}, base, options...)
	if err != nil {
		t.Fatalf("build the client: %v", err)
	}
	return fake, client
}

// sent is the requests the fake was sent, in order.
func (g *rateGitHub) sent() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.requests...)
}

// A client built with a Counter counts each call made with a token it minted under the token's
// installation, by the counter GitHub's answer names and the caller kind the call's context
// carries, and one untagged as other. A call under the App's JWT and a call with a token the client
// did not mint are in no count.
func TestARateCounterCountsAMintedTokensCallsUnderItsInstallationAndKind(t *testing.T) {
	counter := githubrate.NewCounter()
	_, client := startRateGitHub(t, WithRateCounter(counter))
	ctx := context.Background()
	token, err := client.Token(ctx, 7)
	if err != nil {
		t.Fatalf("mint installation 7's token: %v", err)
	}
	for range 2 {
		if _, status, _, err := client.Read(githubrate.WithCaller(ctx, "api"), token, "/repos/acme/web/pulls/1"); err != nil || status != http.StatusOK {
			t.Fatalf("read as the API: status %d, %v", status, err)
		}
	}
	if _, status, _, err := client.GraphQL(ctx, token, "query { viewer { login } }", nil); err != nil || status != http.StatusOK {
		t.Fatalf("query with no caller: status %d, %v", status, err)
	}
	if _, err := client.ReadInstallations(githubrate.WithCaller(ctx, "api")); err != nil {
		t.Fatalf("list the installations under the App's JWT: %v", err)
	}
	if _, _, _, err := client.Read(githubrate.WithCaller(ctx, "api"), "ghs_not_minted_here", "/repos/acme/web/pulls/1"); err != nil {
		t.Fatalf("read with a token the client did not mint: %v", err)
	}
	core, graphql := client.OwnCalls(7)
	if fmt.Sprint(core) != "map[api:2]" || fmt.Sprint(graphql) != "map[other:1]" {
		t.Errorf("installation 7's own calls are core %v and GraphQL %v, want map[api:2] and map[other:1]", core, graphql)
	}
	if core, graphql := counter.Own("7"); fmt.Sprint(core) != "map[api:2]" || fmt.Sprint(graphql) != "map[other:1]" {
		t.Errorf("the Counter holds core %v and GraphQL %v for installation 7, want what OwnCalls answers", core, graphql)
	}
}

// A client built without a Counter reads as before and counts nothing: its OwnCalls answers two
// empty maps, never nil, as does a nil client's.
func TestAClientWithoutARateCounterReadsAndCountsNothing(t *testing.T) {
	_, client := startRateGitHub(t)
	ctx := context.Background()
	token, err := client.Token(ctx, 7)
	if err != nil {
		t.Fatalf("mint installation 7's token: %v", err)
	}
	body, status, _, err := client.Read(githubrate.WithCaller(ctx, "api"), token, "/repos/acme/web/pulls/1")
	if err != nil || status != http.StatusOK || body.Words().String() != `{"state":"open"}` {
		t.Fatalf("read: %d %q %v, want GitHub's 200 answer", status, body.Words(), err)
	}
	for name, own := range map[string]*Client{"a client without a Counter": client, "a nil client": nil} {
		core, graphql := own.OwnCalls(7)
		if core == nil || graphql == nil || len(core) != 0 || len(graphql) != 0 {
			t.Errorf("%s's own calls are %#v and %#v, want two empty maps, never nil", name, core, graphql)
		}
	}
}

// ReadInstallations asks GitHub at each call, so it names an installation added since the call
// before, and leaves ListInstallations' cache as it was: the list the cache took stays its answer
// for the cache's ten minutes, read from no request.
func TestReadInstallationsAsksGitHubEachCallAndLeavesTheCacheAsItWas(t *testing.T) {
	fake, client := startRateGitHub(t)
	ctx := context.Background()
	ids := func(installations []Installation, err error) string {
		t.Helper()
		if err != nil {
			t.Fatalf("list the installations: %v", err)
		}
		var listed []string
		for _, installation := range installations {
			listed = append(listed, strconv.FormatInt(installation.ID, 10)+"@"+installation.AccountLogin)
		}
		return strings.Join(listed, ",")
	}
	if cached := ids(client.ListInstallations(ctx)); cached != "7@acme" {
		t.Fatalf("ListInstallations answered %s, want 7@acme", cached)
	}
	fake.mu.Lock()
	fake.installed = []int64{7, 8}
	fake.mu.Unlock()
	for range 2 {
		if read := ids(client.ReadInstallations(ctx)); read != "7@acme,8@acme" {
			t.Errorf("ReadInstallations answered %s, want GitHub's list now, 7@acme,8@acme", read)
		}
	}
	if cached := ids(client.ListInstallations(ctx)); cached != "7@acme" {
		t.Errorf("ListInstallations answered %s after ReadInstallations, want its cached 7@acme", cached)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.listings != 3 {
		t.Errorf("GitHub listed the installations %d times, want 3: one for the cache and one for each ReadInstallations", fake.listings)
	}
}

// A listed installation holds every permission GitHub's GET /app/installations named for it, as
// GitHub named it, those the client reads nothing of among them (Granted), and the ones the client
// reads as before (Permissions), and the App's id the list names for it. An installation listed
// with no permissions holds an empty map.
func TestAListedInstallationHoldsEveryPermissionGitHubNamed(t *testing.T) {
	base := githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations" {
			githubtest.Unexpected(t, w, r)
			return
		}
		listed := `[]`
		if r.URL.Query().Get("page") == "1" {
			listed = `[{"id":7,"app_id":4242,"app_slug":"example-app","account":{"login":"acme"},
				"permissions":{"contents":"read","checks":"write","administration":"read","pull_requests":"write"}},
				{"id":8,"app_slug":"example-app","account":{"login":"example-org"}}]`
		}
		_, _ = fmt.Fprint(w, listed)
	}})
	_, pemText := testKeyPEM(t)
	client, err := New(&auth.AppConfig{ClientID: "Iv1.rate", PEM: pemText}, base)
	if err != nil {
		t.Fatalf("build the client: %v", err)
	}
	installations, err := client.ReadInstallations(context.Background())
	if err != nil || len(installations) != 2 {
		t.Fatalf("ReadInstallations = %v, %v; want two installations", installations, err)
	}
	if got, want := fmt.Sprint(installations[0].Granted), "map[administration:read checks:write contents:read pull_requests:write]"; got != want {
		t.Errorf("installation 7 grants %s, want every permission GitHub named, %s", got, want)
	}
	if got := installations[0].Permissions; got.Contents != "read" || got.PullRequests != "write" {
		t.Errorf("installation 7's read permissions are %+v, want contents read and pull requests write", got)
	}
	if installations[0].AppID != 4242 || installations[1].AppID != 0 {
		t.Errorf("the listed App ids are %d and %d, want the list's 4242, and 0 where it names none", installations[0].AppID, installations[1].AppID)
	}
	if got := installations[1].Granted; got == nil || len(got) != 0 {
		t.Errorf("installation 8, listed with no permissions, grants %#v, want an empty map", got)
	}
}

// A permission GitHub names with a value that is no string fails no decode of an installation:
// the list keeps the permissions whose access is a string, and the repository's installation
// lookup, behind RepositoryToken, DeliveryPermissions, CheckSource and Installed, answers as
// before.
func TestAPermissionThatIsNoStringFailsNoDecode(t *testing.T) {
	permissions := `{"contents":"read","some_future_flag":true,"limits":{"rate":5}}`
	base := githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app/installations" && r.URL.Query().Get("page") == "1":
			_, _ = fmt.Fprintf(w, `[{"id":7,"app_slug":"example-app","account":{"login":"acme"},"permissions":%s}]`, permissions)
		case r.URL.Path == "/app/installations":
			_, _ = fmt.Fprint(w, `[]`)
		case r.URL.Path == "/repos/acme/widgets/installation":
			_, _ = fmt.Fprintf(w, `{"id":7,"app_slug":"example-app","permissions":%s}`, permissions)
		default:
			githubtest.Unexpected(t, w, r)
		}
	}})
	_, pemText := testKeyPEM(t)
	client, err := New(&auth.AppConfig{ClientID: "Iv1.rate", PEM: pemText}, base)
	if err != nil {
		t.Fatalf("build the client: %v", err)
	}
	installations, err := client.ReadInstallations(context.Background())
	if err != nil || len(installations) != 1 {
		t.Fatalf("ReadInstallations = %v, %v; want the one installation", installations, err)
	}
	if got := fmt.Sprint(installations[0].Granted); got != "map[contents:read]" {
		t.Errorf("installation 7 grants %s, want the permissions whose access is a string, map[contents:read]", got)
	}
	if installed, err := client.Installed(context.Background(), "acme", "widgets"); err != nil || !installed {
		t.Errorf("Installed = %t, %v; want true", installed, err)
	}
}

// SecondaryUntil is the end of the secondary limit the client's Counter noted for an installation
// (githubrate.Counter.SecondaryUntil), and the zero time for a client with no Counter.
func TestSecondaryUntilIsTheCountersNote(t *testing.T) {
	counter := githubrate.NewCounter()
	fake, client := startRateGitHub(t, WithRateCounter(counter))
	fake.secondaryPath = "/repos/acme/web/actions/workflows"
	ctx := context.Background()
	token, err := client.Token(ctx, 7)
	if err != nil {
		t.Fatalf("mint installation 7's token: %v", err)
	}
	before := time.Now()
	if _, status, _, err := client.ReadPastHold(ctx, token, "/repos/acme/web/actions/workflows?per_page=1"); err != nil || status != http.StatusForbidden {
		t.Fatalf("ReadPastHold = %d, %v; want GitHub's 403", status, err)
	}
	if until := client.SecondaryUntil(7); until.Before(before.Add(90*time.Second)) || until.After(time.Now().Add(90*time.Second)) {
		t.Errorf("SecondaryUntil(7) = %s, want 90 s after the answer", until)
	}
	if until := (*Client)(nil).SecondaryUntil(7); !until.IsZero() {
		t.Errorf("a nil client's SecondaryUntil = %s, want zero", until)
	}
	_, without := startRateGitHub(t)
	if until := without.SecondaryUntil(7); !until.IsZero() {
		t.Errorf("a client with no Counter's SecondaryUntil = %s, want zero", until)
	}
}

// AppIdentity is the id and slug GitHub's GET /app answers to the App's JWT; a refused answer and
// an answer that names no id or no slug are errors, never a zero id or an empty slug.
func TestAppIdentityIsTheIDAndSlugGitHubAnswers(t *testing.T) {
	for _, row := range []struct {
		name    string
		status  int
		body    string
		refused string
	}{
		{"an answer", 0, `{"id":4242,"slug":"example-app","name":"Example App"}`, ""},
		{"a refusal", http.StatusUnauthorized, `{"message":"A JSON web token could not be decoded"}`, "401"},
		{"an answer with no slug", 0, `{"id":4242}`, "no App id or slug"},
		{"an answer with no id", 0, `{"slug":"example-app"}`, "no App id or slug"},
		{"an answer that is no JSON", 0, `<html>`, "decode the App"},
	} {
		t.Run(row.name, func(t *testing.T) {
			fake, client := startRateGitHub(t)
			fake.app, fake.appStatus = row.body, row.status
			id, slug, err := client.AppIdentity(context.Background())
			if row.refused == "" {
				if err != nil || id != 4242 || slug != "example-app" {
					t.Fatalf("AppIdentity = %d, %q, %v; want 4242, example-app", id, slug, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), row.refused) {
				t.Fatalf("AppIdentity = %d, %q, %v; want an error naming %q", id, slug, err, row.refused)
			}
		})
	}
	if _, _, err := (*Client)(nil).AppIdentity(context.Background()); !errors.Is(err, ErrNoAppKey) {
		t.Errorf("a nil client's AppIdentity: %v, want ErrNoAppKey", err)
	}
}

// ReadPastHold is sent while the shared ledger holds the token's installation, where Read is not
// sent at all, and it answers GitHub's refusal at the limit as an answer, with the headers that name
// the counter. The limit it met is noted on the attempt its context carries and recorded under the
// installation's scope, as Read's would be, so every other read of the installation holds. An
// answer under the limit is the answer GitHub gave.
func TestReadPastHoldIsSentWhileTheSharedLimitHoldsAndRecordsTheLimitItMeets(t *testing.T) {
	limits := &holdingLimits{held: InstallationScope(7)}
	fake, client := startRateGitHub(t, WithLimits(limits))
	fake.limitedPath = "/repos/acme/web/actions/workflows"
	ctx := context.Background()
	token, err := client.Token(ctx, 7)
	if err != nil {
		t.Fatalf("mint installation 7's token: %v", err)
	}
	if _, _, _, err := client.Read(ctx, token, "/repos/acme/web/actions/workflows?per_page=1"); err == nil {
		t.Fatal("control: Read was not held by the limit the ledger holds on installation 7")
	}
	if sent := fake.sent(); strings.Contains(strings.Join(sent, "\n"), "/actions/workflows") {
		t.Fatalf("control: Read was sent while the ledger held the installation: %v", sent)
	}

	attempted, attempt := githubreads.Begin(ctx, githubreads.CIReconcile)
	body, status, header, err := client.ReadPastHold(attempted, token, "/repos/acme/web/actions/workflows?per_page=1")
	if err != nil || status != http.StatusForbidden {
		t.Fatalf("ReadPastHold = %d %q %v, want GitHub's 403 answered", status, body.Words(), err)
	}
	if bucket, err := githubrate.CoreBucket(header); err != nil || bucket.Used != 5000 || bucket.Limit != 5000 {
		t.Errorf("the refusal's headers read as %+v, %v; want the core counter at 5000 of 5000", bucket, err)
	}
	if want := []string{InstallationScope(7)}; fmt.Sprint(limits.recordedScopes()) != fmt.Sprint(want) {
		t.Errorf("the ledger was asked to record %v, want the limit GitHub answered under %v", limits.recordedScopes(), want)
	}
	if _, failed, cause := attempt.Outcome(); !failed || !strings.Contains(cause, "rate-limited by GitHub (status 403)") {
		t.Errorf("the attempt failed %t with %q, want the limit GitHub answered noted", failed, cause)
	}

	body, status, _, err = client.ReadPastHold(ctx, token, "/rate_limit")
	if err != nil || status != http.StatusOK || body.Words().String() != `{"state":"open"}` {
		t.Errorf("ReadPastHold of an answer under the limit = %d %q %v, want GitHub's 200 answer", status, body.Words(), err)
	}
	if _, _, _, err := (*Client)(nil).ReadPastHold(ctx, token, "/rate_limit"); !errors.Is(err, ErrNoAppKey) {
		t.Errorf("a nil client's ReadPastHold: %v, want ErrNoAppKey", err)
	}
}

// A token can expire and be removed by another mint while its last request is still in flight.
// The refusal belongs to the installation that sent the request even after that cache cleanup.
func TestReadPastHoldRecordsALimitAfterTheTokensScopeIsPruned(t *testing.T) {
	limits := &holdingLimits{}
	fake := &rateGitHub{limitedPath: "/repos/acme/web/actions/workflows", reset: time.Now().Add(time.Hour).Unix()}
	started, release := make(chan struct{}), make(chan struct{})
	var released sync.Once
	defer released.Do(func() { close(release) })
	base := githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == fake.limitedPath {
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		fake.serve(w, r)
	}})
	_, pemText := testKeyPEM(t)
	client, err := New(&auth.AppConfig{ClientID: "Iv1.rate", PEM: pemText}, base, WithLimits(limits))
	if err != nil {
		t.Fatal(err)
	}
	clock := &testClock{now: time.Now()}
	client.now = clock.Now
	token, err := client.Token(t.Context(), 7)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, status, _, err := client.ReadPastHold(t.Context(), token, fake.limitedPath)
		if err == nil && status != http.StatusForbidden {
			err = fmt.Errorf("ReadPastHold status %d, want 403", status)
		}
		done <- err
	}()
	testwait.Eventually(t, "the request sent before the token expired", func() bool {
		select {
		case <-started:
			return true
		default:
			return false
		}
	})
	clock.advance(time.Hour + time.Second)
	if _, err := client.Token(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
	if scope := client.TokenScope(token); scope != "" {
		t.Fatalf("control: the expired token still has scope %q after the mint", scope)
	}
	released.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := limits.recordedScopes(); len(got) != 1 || got[0] != InstallationScope(7) {
		t.Errorf("the in-flight refusal recorded scopes %v, want installation:7", got)
	}
}

// holdingLimits is the shared ledger (Limits) with a limit in force on held for the next half hour
// and on no other scope, which keeps the scope of each limit it is asked to record.
type holdingLimits struct {
	held     string
	mu       sync.Mutex
	recorded []string
}

func (l *holdingLimits) Wait(_ context.Context, scope string) (time.Duration, time.Time, error) {
	if scope == l.held {
		return 30 * time.Minute, time.Now().Add(-time.Minute), nil
	}
	return 0, time.Time{}, nil
}

func (l *holdingLimits) Record(_ context.Context, scope string, _ time.Duration) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recorded = append(l.recorded, scope)
	return nil
}

func (l *holdingLimits) Strike(_ context.Context, scope string, wait time.Duration) (ratelimit.Limit, error) {
	return ratelimit.Limit{Scope: scope, Until: time.Now().Add(wait)}, l.Record(context.Background(), scope, wait)
}

func (l *holdingLimits) recordedScopes() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.recorded...)
}
