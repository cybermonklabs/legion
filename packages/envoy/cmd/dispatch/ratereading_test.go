package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/githubrate"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/ratelimit"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/githubtest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// rateInstallation is one installation the fake GitHub below lists for the App: its account, the
// permissions the list names for it, its first repository, the counters GitHub answers its token's
// calls with, and how GitHub fails its reading, if at all. coreReset and graphQLReset, where set,
// are this installation's resets in place of the fake's. The repository listing's headers name
// listingUsed as the core used count, and GET /rate_limit's body names rateLimitUsed for core,
// each coreUsed where 0, so a reading that took core from the wrong answer shows. tokenLife, an
// hour where 0, is how long a token GitHub mints for it lasts: one that lasts less than the
// client's slack before expiry is minted again at every read.
type rateInstallation struct {
	id                         int64
	owner, repository          string
	permissions                map[string]string
	coreUsed, coreLimit        int
	graphQLUsed, graphQLLimit  int
	coreReset, graphQLReset    int64
	listingUsed, rateLimitUsed int
	tokenLife                  time.Duration
	failure                    string
}

// How GitHub fails an installation's reading in the fake below (rateInstallation.failure).
const (
	mintRefused        = "the token mint refused"
	noCoreCounter      = "a workflows answer with no core counter"
	rateLimitRefused   = "GET /rate_limit refused"
	rateLimitMalformed = "a GET /rate_limit answer with no GraphQL counter"
	rateLimitSecondary = "GET /rate_limit answered with a secondary limit"
	workflowsSecondary = "the workflows read answered with a secondary limit"
	listingRefused     = "the repository listing refused"
	listingEmpty       = "a repository listing of no repository"
	listingUnparsable  = "a repository listing of a name with no owner"
	listingNotJSON     = "a repository listing that is no JSON"
)

// secondaryRetryAfter is the Retry-After, in seconds, of the secondary limit the fake answers.
const secondaryRetryAfter = 60

// rateReadingGitHub is a fake GitHub for the rate reading: the App's own GET /app, its installation
// list, token mints, and the reads an installation's token makes. Every answer to a token's call
// carries that installation's counter in GitHub's X-RateLimit headers, GET /rate_limit included,
// so a count of a call the reading must not count shows. A workflows read of a repository that is
// not the installation's first is answered 404 with the counter, as GitHub answers a repository
// that went. A secondary limit is answered 403 with requests remaining, Retry-After
// secondaryRetryAfter and GitHub's wording. It counts the App reads, the installation listings,
// each installation's repository listings and each installation's token calls, and keeps the
// workflows reads in order. listRefused answers the installation list 503, appRefused answers
// GET /app 500, and listNamesNoApp leaves the App's id out of each listed installation.
type rateReadingGitHub struct {
	t                       *testing.T
	mu                      sync.Mutex
	installations           []rateInstallation
	coreReset, graphQLReset int64
	listRefused             bool
	appRefused              bool
	listNamesNoApp          bool
	minted                  int
	tokens                  map[string]int64
	appReads, listings      int
	repositoryListings      map[int64]int
	tokenCalls              map[int64]int
	workflowReads           []string
}

// newRateReadingGitHub serves a fake GitHub listing installations, whose counters reset at
// coreReset and graphQLReset unless an installation names its own, and answers its base URL.
func newRateReadingGitHub(t *testing.T, coreReset, graphQLReset time.Time, installations ...rateInstallation) (*rateReadingGitHub, string) {
	t.Helper()
	github := &rateReadingGitHub{
		t: t, installations: installations, coreReset: coreReset.Unix(), graphQLReset: graphQLReset.Unix(),
		tokens: map[string]int64{}, repositoryListings: map[int64]int{}, tokenCalls: map[int64]int{},
	}
	return github, githubtest.Server(t, githubtest.Spec{Handle: github.serve})
}

// change changes the installations GitHub lists and answers for, under the fake's lock, for a
// test that changes GitHub between two passes.
func (g *rateReadingGitHub) change(change func([]rateInstallation) []rateInstallation) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.installations = change(g.installations)
}

// listed is the installation id as GitHub lists it now.
func (g *rateReadingGitHub) listed(id int64) (rateInstallation, bool) {
	for _, installation := range g.installations {
		if installation.id == id {
			return installation, true
		}
	}
	return rateInstallation{}, false
}

func (g *rateReadingGitHub) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/app":
		g.appReads++
		if g.appRefused {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprint(w, `{"message":"Server Error"}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"id":4242,"slug":"example-app","name":"Example App"}`)
		return
	case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
		g.listings++
		if g.listRefused {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprint(w, `{"message":"Service Unavailable"}`)
			return
		}
		listed := []map[string]any{}
		if r.URL.Query().Get("page") == "1" {
			for _, installation := range g.installations {
				entry := map[string]any{
					"id": installation.id, "app_id": 4242, "app_slug": "example-app", "account": map[string]any{"login": installation.owner},
				}
				if g.listNamesNoApp {
					delete(entry, "app_id")
				}
				if installation.permissions != nil {
					entry["permissions"] = installation.permissions
				}
				listed = append(listed, entry)
			}
		}
		_ = json.NewEncoder(w).Encode(listed)
		return
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/app/installations/") && strings.HasSuffix(r.URL.Path, "/access_tokens"):
		id, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/app/installations/"), "/access_tokens"), 10, 64)
		installation, listed := g.listed(id)
		if err != nil || !listed {
			githubtest.Unexpected(g.t, w, r)
			return
		}
		if installation.failure == mintRefused {
			w.WriteHeader(http.StatusForbidden)
			_, _ = fmt.Fprint(w, `{"message":"This installation has been suspended"}`)
			return
		}
		g.minted++
		token := fmt.Sprintf("ghs_example_%d_%d", id, g.minted)
		g.tokens[token] = id
		w.WriteHeader(http.StatusCreated)
		expires := time.Now().Add(cmpOr(installation.tokenLife, time.Hour)).UTC().Format(time.RFC3339)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": token, "expires_at": expires})
		return
	}
	id, minted := g.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	installation, listed := g.listed(id)
	if !minted || !listed {
		g.t.Errorf("GitHub was asked %s %s with no token of an installation it lists", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	g.tokenCalls[id]++
	secondary := func() {
		w.Header().Set("Retry-After", strconv.Itoa(secondaryRetryAfter))
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`)
	}
	coreReset, graphQLReset := cmpOr(installation.coreReset, g.coreReset), cmpOr(installation.graphQLReset, g.graphQLReset)
	counter := func(resource string, used, limit int, reset int64) {
		header := w.Header()
		header.Set("X-RateLimit-Resource", resource)
		header.Set("X-RateLimit-Used", strconv.Itoa(used))
		header.Set("X-RateLimit-Limit", strconv.Itoa(limit))
		header.Set("X-RateLimit-Remaining", strconv.Itoa(limit-used))
		header.Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
	}
	path := r.URL.Path
	switch {
	case r.Method == http.MethodGet && path == "/installation/repositories" && r.URL.Query().Get("per_page") == "1":
		g.repositoryListings[id]++
		counter(githubrate.Core, cmpOr(installation.listingUsed, installation.coreUsed), installation.coreLimit, coreReset)
		switch installation.failure {
		case listingRefused:
			w.WriteHeader(http.StatusForbidden)
			_, _ = fmt.Fprint(w, `{"message":"API rate limit exceeded for installation ID 101."}`)
		case listingEmpty:
			_, _ = fmt.Fprint(w, `{"total_count":0,"repositories":[]}`)
		case listingUnparsable:
			_, _ = fmt.Fprint(w, `{"total_count":1,"repositories":[{"full_name":"no-owner"}]}`)
		case listingNotJSON:
			_, _ = fmt.Fprint(w, `<html>`)
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "repositories": []map[string]any{{"full_name": installation.repository}}})
		}
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/repos/") && strings.HasSuffix(path, "/actions/workflows") && r.URL.Query().Get("per_page") == "1":
		g.workflowReads = append(g.workflowReads, path)
		if installation.failure != noCoreCounter {
			counter(githubrate.Core, installation.coreUsed, installation.coreLimit, coreReset)
		}
		if path != "/repos/"+installation.repository+"/actions/workflows" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, `{"message":"Not Found"}`)
			return
		}
		if installation.failure == workflowsSecondary {
			secondary()
			return
		}
		_, _ = fmt.Fprint(w, `{"total_count":0,"workflows":[]}`)
	case r.Method == http.MethodGet && path == "/rate_limit":
		counter(githubrate.Core, installation.coreUsed, installation.coreLimit, coreReset)
		switch installation.failure {
		case rateLimitRefused:
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprint(w, `{"message":"Service Unavailable"}`)
		case rateLimitSecondary:
			secondary()
		case rateLimitMalformed:
			_, _ = fmt.Fprint(w, `{"resources":{}}`)
		default:
			used := cmpOr(installation.rateLimitUsed, installation.coreUsed)
			_ = json.NewEncoder(w).Encode(map[string]any{"resources": map[string]any{
				"core":    map[string]any{"limit": installation.coreLimit, "used": used, "remaining": installation.coreLimit - used, "reset": coreReset},
				"graphql": map[string]any{"limit": installation.graphQLLimit, "used": installation.graphQLUsed, "remaining": installation.graphQLLimit - installation.graphQLUsed, "reset": graphQLReset},
			}})
		}
	case r.Method == http.MethodGet && path == "/repos/"+installation.repository+"/pulls/1":
		counter(githubrate.Core, installation.coreUsed, installation.coreLimit, coreReset)
		_, _ = fmt.Fprint(w, `{"number":1}`)
	case r.Method == http.MethodPost && path == "/graphql":
		counter(githubrate.GraphQL, installation.graphQLUsed, installation.graphQLLimit, graphQLReset)
		_, _ = fmt.Fprint(w, `{"data":{"viewer":{"login":"example-app[bot]"}}}`)
	default:
		githubtest.Unexpected(g.t, w, r)
	}
}

// cmpOr is value, or otherwise where value is the zero value.
func cmpOr[T comparable](value, otherwise T) T {
	var zero T
	if value == zero {
		return otherwise
	}
	return value
}

// steppedClock is a rate reading's clock that a test moves (rateClock): its now is at, which a wait
// moves to the time the wait was for, so a pass due at a reset's last minute runs at once and reads
// the time it was due at, and which set moves for a test whose GitHub takes time to answer. It
// keeps how long each wait was for, and ends the reading at its passes'th wait. between, where
// set, runs between one pass and the next with the number of the pass that ended, for a test that
// changes GitHub there.
type steppedClock struct {
	mu      sync.Mutex
	at      time.Time
	passes  int
	waits   []time.Duration
	between func(pass int)
}

// set moves the clock to at, from any goroutine: a fake GitHub's handler sets it to say how long
// its answer took.
func (c *steppedClock) set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = at
}

func (c *steppedClock) clock() rateClock {
	return rateClock{
		now: func() time.Time {
			c.mu.Lock()
			defer c.mu.Unlock()
			return c.at
		},
		wait: func(_ context.Context, until time.Time) bool {
			c.mu.Lock()
			c.waits = append(c.waits, until.Sub(c.at))
			pass := len(c.waits)
			c.mu.Unlock()
			if pass >= c.passes {
				return false
			}
			if c.between != nil {
				c.between(pass)
			}
			c.set(until)
			return true
		},
	}
}

// readPasses runs the rate reading with client on clock until its passes are done, its lines as
// JSON into written, and answers the lines.
func readPasses(t *testing.T, client *githubapp.Client, written *logtest.Buffer, clock *steppedClock) []map[string]any {
	t.Helper()
	readRates(t.Context(), client, slog.New(slog.NewJSONHandler(written, nil)), clock.clock())
	return logtest.JSONLines(t, written.String())
}

// installationsOf is the installation ids lines name, in order.
func installationsOf(lines []map[string]any) []string {
	var ids []string
	for _, line := range lines {
		ids = append(ids, fmt.Sprint(line["installation_id"]))
	}
	return ids
}

// The rate reading of Dispatch (the approved plan's K-2, check C9), with a real App client against
// a fake GitHub that lists three installations, writes one JSON line per listed installation at
// each pass through the logger it is handed, and none through the process's own text logger. Each
// line names the App by GitHub's GET /app, the installation and its owner, how many installations
// the list held, the installation's permissions as the list named them (one the client reads none
// of among them), both counters' used count, limit and RFC 3339 reset, and this process's own
// calls on each counter by caller kind: a call another client of the process made with a token it
// minted and tagged api, an untagged GraphQL query counted as other, and the reading's own calls
// as rate_reading, while GET /rate_limit, which costs nothing, counts as none. The second
// installation's token is minted by the reading alone, and the third's mint is refused, so its
// line carries the refusal, its listed permissions and no counter. The core reset GitHub answers
// is 70 s after the reading's first pass, on the reading's own clock, so the second pass is due
// 25 s later by Next's before-reset branch: it runs in the last minute before that reset, and the
// two installations whose counters were read say before_reset. Each pass lists the installations
// afresh, and each installation's repository is listed once and remembered. A last case reads one
// pass over two installations whose resets differ: only the line of the one in the last minute
// before a reset of its own says before_reset.
func TestRateReadingIsOneJSONLine(t *testing.T) {
	start := time.Now().Truncate(time.Second)
	github, base := newRateReadingGitHub(t, start.Add(70*time.Second), start.Add(time.Hour),
		rateInstallation{id: 101, owner: "acme", repository: "acme/widgets", coreUsed: 120, coreLimit: 5000, graphQLUsed: 40, graphQLLimit: 5000,
			permissions: map[string]string{"contents": "read", "metadata": "read", "checks": "write"}},
		rateInstallation{id: 102, owner: "example-org", repository: "example-org/gadgets", coreUsed: 7, coreLimit: 12500, graphQLUsed: 3, graphQLLimit: 10000,
			permissions: map[string]string{"actions": "read", "metadata": "read"}},
		rateInstallation{id: 103, owner: "example-user", repository: "example-user/tools", failure: mintRefused,
			permissions: map[string]string{"metadata": "read"}},
	)
	counter := githubrate.NewCounter()

	// Another client of the same process, as the API's and the CI reconcile's are, counting into
	// the one Counter: it mints the first installation's token and spends it.
	other := rateReadingClient(t, base, githubapp.WithRateCounter(counter))
	token, err := other.Token(t.Context(), 101)
	if err != nil {
		t.Fatalf("mint installation 101's token: %v", err)
	}
	if _, status, _, err := other.Read(githubrate.WithCaller(t.Context(), "api"), token, "/repos/acme/widgets/pulls/1"); err != nil || status != http.StatusOK {
		t.Fatalf("read a pull request as the API: status %d, %v", status, err)
	}
	if _, status, _, err := other.GraphQL(t.Context(), token, "query { viewer { login } }", nil); err != nil || status != http.StatusOK {
		t.Fatalf("query as no caller: status %d, %v", status, err)
	}

	client := rateReadingClient(t, base, githubapp.WithRateCounter(counter))
	// The process's own logger writes text, as main's does, into the same buffer as the reading's
	// JSON logger: a reading line written through it is a line no JSON decodes.
	var written logtest.Buffer
	logtest.SetDefault(t, slog.NewTextHandler(&written, nil))
	clock := &steppedClock{at: start, passes: 2}
	lines := readPasses(t, client, &written, clock)
	if len(lines) != 6 {
		t.Fatalf("the log holds %d lines, want two passes of 3, one for each installation listed:\n%s", len(lines), written.String())
	}
	if want := 70*time.Second - githubrate.BeforeReset; clock.waits[0] != want {
		t.Errorf("the second pass was due %s after the first, want %s: the last minute before the core reset", clock.waits[0], want)
	}
	process, err := os.Hostname()
	if err != nil {
		t.Fatalf("read the host name: %v", err)
	}
	coreReset := time.Unix(github.coreReset, 0).UTC().Format(time.RFC3339)
	graphQLReset := time.Unix(github.graphQLReset, 0).UTC().Format(time.RFC3339)
	wantOwn := []map[int64][2]map[string]int{
		{101: {{"api": 1, rateReadingCaller: 2}, {githubrate.Other: 1}}, 102: {{rateReadingCaller: 2}, {}}},
		{101: {{"api": 1, rateReadingCaller: 3}, {githubrate.Other: 1}}, 102: {{rateReadingCaller: 3}, {}}},
	}
	for index, line := range lines {
		pass, installation := index/3, github.installations[index%3]
		where := fmt.Sprintf("pass %d, installation %d: %v", pass+1, installation.id, line)
		for field, want := range map[string]any{
			"level": "INFO", "msg": githubrate.Msg, "program": "dispatch", "process": process, "project": "",
			"app_id": "4242", "app_slug": "example-app", "owner": installation.owner,
			"installation_id": strconv.FormatInt(installation.id, 10), "installations": float64(3),
			"before_reset": pass == 1 && installation.failure == "",
		} {
			if got, present := line[field]; !present || got != want {
				t.Errorf("%s: %s is %#v (present %t), want %#v", where, field, got, present, want)
			}
		}
		if got, want := fmt.Sprint(line["permissions"]), fmt.Sprint(installation.permissions); got != want {
			t.Errorf("%s: permissions are %s, want the list's %s", where, got, want)
		}
		if installation.failure != "" {
			if cause, _ := line["error"].(string); !strings.Contains(cause, "installation 103") || !strings.Contains(cause, "403") {
				t.Errorf("%s: error is %q, want the refused mint of installation 103", where, cause)
			}
			if _, present := line["core_used"]; present {
				t.Errorf("%s: a failed reading carries a counter", where)
			}
			continue
		}
		if _, present := line["error"]; present {
			t.Errorf("%s: a reading GitHub answered carries an error", where)
		}
		own := wantOwn[pass][installation.id]
		for field, want := range map[string]any{
			"core_used": float64(installation.coreUsed), "core_limit": float64(installation.coreLimit), "core_reset": coreReset,
			"graphql_used": float64(installation.graphQLUsed), "graphql_limit": float64(installation.graphQLLimit), "graphql_reset": graphQLReset,
			"core_own": counts(own[0]), "graphql_own": counts(own[1]),
		} {
			if got := fmt.Sprint(line[field]); got != fmt.Sprint(want) {
				t.Errorf("%s: %s is %s, want %s", where, field, got, fmt.Sprint(want))
			}
		}
	}
	github.mu.Lock()
	defer github.mu.Unlock()
	if github.listings != 2 {
		t.Errorf("GitHub listed the App's installations %d times, want once a pass, 2", github.listings)
	}
	if github.appReads != 0 {
		t.Errorf("GitHub was asked GET /app %d times, want none: the installation list names the App's id and slug", github.appReads)
	}
	for _, id := range []int64{101, 102} {
		if listed := github.repositoryListings[id]; listed != 1 {
			t.Errorf("installation %d's repositories were listed %d times, want once: the first is remembered", id, listed)
		}
	}

	t.Run("one pass over two installations whose resets differ", func(t *testing.T) {
		start := time.Now().Truncate(time.Second)
		_, base := newRateReadingGitHub(t, start.Add(time.Hour), start.Add(2*time.Hour),
			rateInstallation{id: 101, owner: "acme", repository: "acme/widgets", coreLimit: 5000, graphQLLimit: 5000},
			rateInstallation{id: 102, owner: "example-org", repository: "example-org/gadgets", coreLimit: 5000, graphQLLimit: 5000,
				coreReset: start.Add(40 * time.Second).Unix()},
			rateInstallation{id: 103, owner: "example-user", repository: "example-user/tools", coreLimit: 5000, graphQLLimit: 5000},
		)
		var written logtest.Buffer
		lines := readPasses(t, rateReadingClient(t, base), &written, &steppedClock{at: start, passes: 1})
		got := map[string]any{}
		for _, line := range lines {
			got[fmt.Sprint(line["installation_id"])] = line["before_reset"]
		}
		if want := map[string]any{"101": false, "102": true, "103": false}; fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("the pass's lines say before_reset %v, want %v: installation 102's alone, whose core reset is 40 s out", got, want)
		}
	})
}

// The reading in a counter's last minute is taken even when the pass that read the installation
// began outside that minute and ended inside it (the audit's sequence): installation 101 is read
// with 70 s left of its core counter's hour, and installation 102's GraphQL read takes 30 s, so
// the pass ends with 40 s left. The next pass is due at once, reads 101 first, and its line says
// before_reset; the pass after it is the interval away, since that last minute was read.
func TestAPassThatEndsInsideALastMinuteItBeganOutsideReadsItAtOnce(t *testing.T) {
	start := time.Now().Truncate(time.Second)
	github, _ := newRateReadingGitHub(t, start.Add(70*time.Second), start.Add(time.Hour),
		rateInstallation{id: 101, owner: "acme", repository: "acme/widgets", coreLimit: 5000, graphQLLimit: 5000},
		rateInstallation{id: 102, owner: "example-org", repository: "example-org/gadgets", coreLimit: 5000, graphQLLimit: 5000,
			coreReset: start.Add(time.Hour).Unix()},
	)
	clock := &steppedClock{at: start, passes: 3}
	base := githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		github.serve(w, r)
		if r.URL.Path == "/rate_limit" && strings.Contains(r.Header.Get("Authorization"), "ghs_example_102_") && clock.clock().now().Before(start.Add(30*time.Second)) {
			clock.set(start.Add(30 * time.Second))
		}
	}})
	var written logtest.Buffer
	lines := readPasses(t, rateReadingClient(t, base), &written, clock)
	if clock.waits[0] != 0 {
		t.Errorf("the pass after the one that ended 40 s before 101's reset was due %s later, want at once", clock.waits[0])
	}
	if got := installationsOf(lines); len(got) < 4 || !slices.Equal(got[:4], []string{"101", "102", "101", "102"}) {
		t.Fatalf("the passes read installations %v, want 101 and 102, then 101, in its last minute, first", got)
	}
	if lines[0]["before_reset"] != false || lines[2]["before_reset"] != true || lines[3]["before_reset"] != false {
		t.Errorf("installation 101's lines say before_reset %v then %v, and 102's second %v; want false, true and false",
			lines[0]["before_reset"], lines[2]["before_reset"], lines[3]["before_reset"])
	}
	if clock.waits[1] != 10*time.Minute {
		t.Errorf("the pass after the last-minute one was due %s later, want the interval, 10m0s", clock.waits[1])
	}
}

// A reset read in its last minute asks for no second reading, though GitHub's next answer names
// it a second later (its measured drift): the first pass reads the core reset 70 s out, the
// before-reset pass reads it at 25 s with its header a second later, and the pass after that is
// the interval away, not at once.
func TestAResetReadInItsLastMinuteThatDriftsASecondSchedulesNoSecondPass(t *testing.T) {
	start := time.Now().Truncate(time.Second)
	github, base := newRateReadingGitHub(t, start.Add(70*time.Second), start.Add(time.Hour),
		rateInstallation{id: 101, owner: "acme", repository: "acme/widgets", coreLimit: 5000, graphQLLimit: 5000})
	clock := &steppedClock{at: start, passes: 3, between: func(pass int) {
		if pass == 1 {
			github.mu.Lock()
			github.coreReset = start.Add(71 * time.Second).Unix()
			github.mu.Unlock()
		}
	}}
	var written logtest.Buffer
	lines := readPasses(t, rateReadingClient(t, base), &written, clock)
	if want := 70*time.Second - githubrate.BeforeReset; clock.waits[0] != want {
		t.Fatalf("the before-reset pass was due %s after the first, want %s", clock.waits[0], want)
	}
	if len(lines) < 2 || lines[1]["before_reset"] != true || lines[1]["core_reset"] != start.Add(71*time.Second).UTC().Format(time.RFC3339) {
		t.Fatalf("the before-reset pass's line is %v, want before_reset and the drifted reset", lines)
	}
	if clock.waits[1] != 10*time.Minute {
		t.Errorf("the pass after the before-reset one was due %s later, want the interval, 10m0s", clock.waits[1])
	}
}

// A pass that tries an installation inside a counter's last minute and fails to read it has had
// that minute: the next pass is the interval away, not at once, so a refusal there is not asked
// again in a loop until the reset. Rows: the workflows answer names no core counter, and GitHub
// refuses the installation list.
func TestAFailedTryInsideALastMinuteSchedulesNoPassAtOnce(t *testing.T) {
	for _, row := range []struct {
		name  string
		apply func(*rateReadingGitHub)
	}{
		{noCoreCounter, func(g *rateReadingGitHub) { g.change(failing(101, noCoreCounter)) }},
		{"the installation list refused", func(g *rateReadingGitHub) {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.listRefused = true
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			start := time.Now().Truncate(time.Second)
			github, base := newRateReadingGitHub(t, start.Add(70*time.Second), start.Add(time.Hour),
				rateInstallation{id: 101, owner: "acme", repository: "acme/widgets", coreLimit: 5000, graphQLLimit: 5000})
			clock := &steppedClock{at: start, passes: 3, between: func(pass int) {
				if pass == 1 {
					row.apply(github)
				}
			}}
			var written logtest.Buffer
			lines := readPasses(t, rateReadingClient(t, base), &written, clock)
			if want := 70*time.Second - githubrate.BeforeReset; clock.waits[0] != want {
				t.Fatalf("the before-reset pass was due %s after the first, want %s", clock.waits[0], want)
			}
			if len(lines) < 2 || lines[1]["error"] == nil {
				t.Fatalf("the before-reset pass logged %v, want its failure", lines)
			}
			if clock.waits[1] != 10*time.Minute {
				t.Errorf("after the failed try in the last minute the next pass was due %s later, want the interval, 10m0s", clock.waits[1])
			}
		})
	}
}

// A reading that fails in a counter's last minute is a before_reset line carrying its error: that
// minute was tried and read nothing, which a reader of a day's before_reset lines must see, where a
// false says no reading was tried there. Where the failed reading read no counter, before_reset
// comes from the installation's known resets: the core reset the first pass read 70 s out is 45 s
// out at the before-reset pass, where GitHub refuses the token mint in one row (the first pass's
// token lasts a minute, so the client mints again) and answers the workflows read with no core
// counter in the other. An installation with no known reset whose reading fails says false
// (installation 103 of TestRateReadingIsOneJSONLine).
func TestAFailedReadingInALastMinuteIsABeforeResetErrorLine(t *testing.T) {
	for failure, cause := range map[string]string{
		mintRefused:   "mint a token of installation 101",
		noCoreCounter: "read the core counter",
	} {
		t.Run(failure, func(t *testing.T) {
			start := time.Now().Truncate(time.Second)
			github, base := newRateReadingGitHub(t, start.Add(70*time.Second), start.Add(time.Hour),
				rateInstallation{id: 101, owner: "acme", repository: "acme/widgets", coreLimit: 5000, graphQLLimit: 5000, tokenLife: time.Minute})
			clock := &steppedClock{at: start, passes: 2, between: func(pass int) {
				if pass == 1 {
					github.change(failing(101, failure))
				}
			}}
			var written logtest.Buffer
			lines := readPasses(t, rateReadingClient(t, base), &written, clock)
			if want := 70*time.Second - githubrate.BeforeReset; clock.waits[0] != want {
				t.Fatalf("the before-reset pass was due %s after the first, want %s", clock.waits[0], want)
			}
			if len(lines) != 2 {
				t.Fatalf("the passes logged %d lines, want one each:\n%s", len(lines), written.String())
			}
			if lines[0]["before_reset"] != false || lines[0]["error"] != nil {
				t.Fatalf("the first pass's line is %v, want a reading 70 s before the reset, not before_reset", lines[0])
			}
			if text, _ := lines[1]["error"].(string); !strings.Contains(text, cause) {
				t.Errorf("the before-reset pass's error is %q, want it to name %q", text, cause)
			}
			if lines[1]["before_reset"] != true {
				t.Errorf("the failed reading 45 s before the installation's known core reset says before_reset %v, want true: %v", lines[1]["before_reset"], lines[1])
			}
		})
	}
}

// With no reset in the next ten minutes, the next pass is due ten minutes after the one before:
// the interval the plan fixes, held here to its literal value and not to the constant.
func TestTheNextPassIsDueTenMinutesOutWithNoResetNear(t *testing.T) {
	start := time.Now().Truncate(time.Second)
	_, base := newRateReadingGitHub(t, start.Add(time.Hour), start.Add(time.Hour),
		rateInstallation{id: 101, owner: "acme", repository: "acme/widgets", coreLimit: 5000, graphQLLimit: 5000})
	var written logtest.Buffer
	clock := &steppedClock{at: start, passes: 1}
	readPasses(t, rateReadingClient(t, base), &written, clock)
	if clock.waits[0] != 10*time.Minute {
		t.Errorf("the next pass is due %s after the first, want 10m0s", clock.waits[0])
	}
}

// The next pass is due at the earliest reset of any counter of any installation, less the margin
// before it: here the GraphQL counter of the second installation, whose reset is three minutes
// out while every other counter resets in an hour.
func TestAnotherInstallationsGraphQLResetWakesTheReading(t *testing.T) {
	start := time.Now().Truncate(time.Second)
	_, base := newRateReadingGitHub(t, start.Add(time.Hour), start.Add(time.Hour),
		rateInstallation{id: 101, owner: "acme", repository: "acme/widgets", coreLimit: 5000, graphQLLimit: 5000},
		rateInstallation{id: 102, owner: "example-org", repository: "example-org/gadgets", coreLimit: 5000, graphQLLimit: 5000,
			graphQLReset: start.Add(3 * time.Minute).Unix()},
	)
	var written logtest.Buffer
	clock := &steppedClock{at: start, passes: 1}
	readPasses(t, rateReadingClient(t, base), &written, clock)
	if want := 3*time.Minute - githubrate.BeforeReset; clock.waits[0] != want {
		t.Errorf("the next pass is due %s after the first, want %s: the last minute before installation 102's GraphQL reset", clock.waits[0], want)
	}
}

// An installation whose reading fails, between two whose readings GitHub answers, logs its line
// with the failure in its place and no counter, and the two around it carry their counters: a
// refused token mint, a workflows answer that names no core counter, a GET /rate_limit GitHub
// refuses, and one whose body holds no GraphQL counter.
func TestAFailedInstallationBetweenTwoGoodOnesLogsItsOwnFailure(t *testing.T) {
	for failure, cause := range map[string][]string{
		mintRefused:        {"mint a token of installation 102", "status 403", "This installation has been suspended"},
		noCoreCounter:      {"read the core counter", "not core"},
		rateLimitRefused:   {"read the GraphQL counter", "GET /rate_limit", "status 503", "Service Unavailable"},
		rateLimitMalformed: {"read the GraphQL counter", "resources.graphql"},
	} {
		t.Run(failure, func(t *testing.T) {
			start := time.Now().Truncate(time.Second)
			_, base := newRateReadingGitHub(t, start.Add(time.Hour), start.Add(time.Hour),
				rateInstallation{id: 101, owner: "acme", repository: "acme/widgets", coreUsed: 11, coreLimit: 5000, graphQLUsed: 12, graphQLLimit: 5000},
				rateInstallation{id: 102, owner: "example-org", repository: "example-org/gadgets", coreLimit: 5000, graphQLLimit: 5000, failure: failure},
				rateInstallation{id: 103, owner: "example-user", repository: "example-user/tools", coreUsed: 31, coreLimit: 5000, graphQLUsed: 32, graphQLLimit: 5000},
			)
			var written logtest.Buffer
			lines := readPasses(t, rateReadingClient(t, base), &written, &steppedClock{at: start, passes: 1})
			if got := installationsOf(lines); !slices.Equal(got, []string{"101", "102", "103"}) {
				t.Fatalf("the pass logged installations %v, want 101, 102 and 103:\n%s", got, written.String())
			}
			failed := lines[1]
			for _, part := range cause {
				if text, _ := failed["error"].(string); !strings.Contains(text, part) {
					t.Errorf("installation 102's error is %q, want it to name %q", text, part)
				}
			}
			if _, present := failed["core_used"]; present {
				t.Errorf("installation 102's failed reading carries a counter: %v", failed)
			}
			for index, want := range map[int][2]float64{0: {11, 12}, 2: {31, 32}} {
				line := lines[index]
				if _, present := line["error"]; present || line["core_used"] != want[0] || line["graphql_used"] != want[1] {
					t.Errorf("installation %v's reading is %v, want core %v and GraphQL %v and no error", line["installation_id"], line, want[0], want[1])
				}
			}
		})
	}
}

// A pass that reads nothing of a counter keeps the reset it read before, so the reading before a
// reset it already knows of is still taken. The counter that resets eleven and a half minutes
// after the start is read at the first pass, and at the second, ten minutes in, a workflows answer
// with no core counter, a refused GET /rate_limit, or a refused list of the installations reads
// nothing of it: the pass after is still due 45 s before that reset, where the other counter's
// reset, two hours out, would leave it the interval away. (A refused token mint is no row here:
// the client keeps the token it minted at the first pass for its hour.) An installation no longer
// listed takes its resets with it: the next pass is ten minutes out again.
func TestAReadingThatFailsKeepsItsInstallationsKnownResets(t *testing.T) {
	const soon = 11*time.Minute + 30*time.Second
	for _, row := range []struct {
		name          string
		core, graphQL time.Duration
		apply         func(*rateReadingGitHub)
		want          time.Duration
	}{
		{noCoreCounter, soon, 2 * time.Hour, func(g *rateReadingGitHub) { g.change(failing(101, noCoreCounter)) }, 45 * time.Second},
		{rateLimitRefused, 2 * time.Hour, soon, func(g *rateReadingGitHub) { g.change(failing(101, rateLimitRefused)) }, 45 * time.Second},
		{"the installation list refused", soon, 2 * time.Hour, func(g *rateReadingGitHub) {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.listRefused = true
		}, 45 * time.Second},
		{"the installation no longer listed", soon, 2 * time.Hour, func(g *rateReadingGitHub) {
			g.change(func([]rateInstallation) []rateInstallation {
				return []rateInstallation{{id: 102, owner: "example-org", repository: "example-org/gadgets", coreLimit: 5000, graphQLLimit: 5000,
					coreReset: time.Now().Add(time.Hour).Unix()}}
			})
		}, 10 * time.Minute},
	} {
		t.Run(row.name, func(t *testing.T) {
			start := time.Now().Truncate(time.Second)
			github, base := newRateReadingGitHub(t, start.Add(row.core), start.Add(row.graphQL),
				rateInstallation{id: 101, owner: "acme", repository: "acme/widgets", coreLimit: 5000, graphQLLimit: 5000})
			var written logtest.Buffer
			clock := &steppedClock{at: start, passes: 2, between: func(int) { row.apply(github) }}
			readPasses(t, rateReadingClient(t, base), &written, clock)
			if clock.waits[0] != 10*time.Minute {
				t.Fatalf("the second pass was due %s after the first, want the interval", clock.waits[0])
			}
			if clock.waits[1] != row.want {
				t.Errorf("after the failed pass the next is due %s later, want %s:\n%s", clock.waits[1], row.want, written.String())
			}
		})
	}
}

// failing sets installation id's failure.
func failing(id int64, failure string) func([]rateInstallation) []rateInstallation {
	return func(installations []rateInstallation) []rateInstallation {
		for index := range installations {
			if installations[index].id == id {
				installations[index].failure = failure
			}
		}
		return installations
	}
}

// A pass reads the installations in their last minute before a reset first, so the reading that
// must land before a reset is not kept behind the others; every installation is still read, one
// line each.
func TestTheInstallationsInTheirLastMinuteAreReadFirst(t *testing.T) {
	start := time.Now().Truncate(time.Second)
	_, base := newRateReadingGitHub(t, start.Add(20*time.Minute), start.Add(2*time.Hour),
		rateInstallation{id: 101, owner: "acme", repository: "acme/widgets", coreLimit: 5000, graphQLLimit: 5000},
		rateInstallation{id: 102, owner: "example-org", repository: "example-org/gadgets", coreLimit: 5000, graphQLLimit: 5000,
			coreReset: start.Add(70 * time.Second).Unix()},
	)
	var written logtest.Buffer
	lines := readPasses(t, rateReadingClient(t, base), &written, &steppedClock{at: start, passes: 2})
	if got := installationsOf(lines); !slices.Equal(got, []string{"101", "102", "102", "101"}) {
		t.Fatalf("the passes read installations %v, want 101 and 102, then 102, in its last minute, before 101", got)
	}
	if lines[2]["before_reset"] != true || lines[3]["before_reset"] != false {
		t.Errorf("the second pass's lines say before_reset %v and %v, want installation 102's alone", lines[2]["before_reset"], lines[3]["before_reset"])
	}
}

// The core counter is the one the workflows read's headers name, never the repository listing's
// or GET /rate_limit's body's, which here each name another used count. Where the listing gives
// no repository to read, though GitHub answered it (a refusal, no repository, a name with no
// owner, a body that is no JSON), the counter is the one the listing's own headers name, and the
// reading carries it and no error.
func TestTheCoreCounterIsTheWorkflowsReadsOrTheListingsWithNoRepository(t *testing.T) {
	for failure, want := range map[string]float64{
		"":                22,
		listingRefused:    11,
		listingEmpty:      11,
		listingUnparsable: 11,
		listingNotJSON:    11,
	} {
		t.Run(cmpOr(failure, "a repository listed"), func(t *testing.T) {
			start := time.Now().Truncate(time.Second)
			github, base := newRateReadingGitHub(t, start.Add(time.Hour), start.Add(time.Hour),
				rateInstallation{id: 101, owner: "acme", repository: "acme/widgets", coreLimit: 5000, graphQLLimit: 5000,
					listingUsed: 11, coreUsed: 22, rateLimitUsed: 33, failure: failure})
			var written logtest.Buffer
			lines := readPasses(t, rateReadingClient(t, base), &written, &steppedClock{at: start, passes: 1})
			if len(lines) != 1 {
				t.Fatalf("the pass logged %d lines, want 1:\n%s", len(lines), written.String())
			}
			if _, present := lines[0]["error"]; present || lines[0]["core_used"] != want {
				t.Errorf("the reading is %v, want core_used %v and no error", lines[0], want)
			}
			github.mu.Lock()
			defer github.mu.Unlock()
			if read := len(github.workflowReads); (failure == "") != (read == 1) {
				t.Errorf("the workflows were read %d times, want once with a repository listed and never without", read)
			}
		})
	}
}

// Each pass reads the list GitHub answers then: an installation added since is read and its
// repository listed, one removed is no longer read. A remembered repository that GitHub answers
// 404 still gives the counter its answer names, and is listed again at the next pass, which reads
// the repository GitHub lists first by then.
func TestAListThatChangesAndARepositoryThatWentAreReadAgain(t *testing.T) {
	start := time.Now().Truncate(time.Second)
	github, base := newRateReadingGitHub(t, start.Add(time.Hour), start.Add(time.Hour),
		rateInstallation{id: 101, owner: "acme", repository: "acme/widgets", coreUsed: 11, coreLimit: 5000, graphQLLimit: 5000},
		rateInstallation{id: 102, owner: "example-org", repository: "example-org/gadgets", coreUsed: 21, coreLimit: 5000, graphQLLimit: 5000},
	)
	clock := &steppedClock{at: start, passes: 3, between: func(pass int) {
		if pass != 1 {
			return
		}
		github.change(func(installations []rateInstallation) []rateInstallation {
			first := installations[0]
			first.repository = "acme/tools"
			return []rateInstallation{first, {id: 103, owner: "example-user", repository: "example-user/tools", coreUsed: 31, coreLimit: 5000, graphQLLimit: 5000}}
		})
	}}
	var written logtest.Buffer
	lines := readPasses(t, rateReadingClient(t, base), &written, clock)
	if got := installationsOf(lines); !slices.Equal(got, []string{"101", "102", "101", "103", "101", "103"}) {
		t.Fatalf("the passes read installations %v, want 101 and 102, then 101 and 103 twice", got)
	}
	for index, line := range lines {
		if _, present := line["error"]; present || line["installations"] != float64(2) {
			t.Errorf("line %d is %v, want a reading of two installations listed and no error", index, line)
		}
	}
	if lines[2]["core_used"] != float64(11) {
		t.Errorf("the reading of the repository GitHub answered 404 is %v, want the counter that answer named", lines[2])
	}
	github.mu.Lock()
	defer github.mu.Unlock()
	if want := map[int64]int{101: 2, 102: 1, 103: 1}; !maps.Equal(github.repositoryListings, want) {
		t.Errorf("the repositories were listed %v times, want %v", github.repositoryListings, want)
	}
	var first []string
	for _, read := range github.workflowReads {
		if strings.HasPrefix(read, "/repos/acme/") {
			first = append(first, read)
		}
	}
	if want := []string{"/repos/acme/widgets/actions/workflows", "/repos/acme/widgets/actions/workflows", "/repos/acme/tools/actions/workflows"}; !slices.Equal(first, want) {
		t.Errorf("installation 101's workflows were read at %v, want %v", first, want)
	}
}

// A refused GraphQL read omits both counters from the error line, but a core reset already read
// still schedules the last-minute pass and labels the installation's own last-minute reading. A
// core reset 70 s out is due for Next; one 50 s out was read in its last minute, so it is not.
func TestARateReadingKeepsTheResetOfACounterReadBeforeAnotherReadFails(t *testing.T) {
	for _, until := range []time.Duration{50 * time.Second, 70 * time.Second} {
		t.Run(until.String(), func(t *testing.T) {
			start := time.Now().Truncate(time.Second)
			reset := start.Add(until)
			_, base := newRateReadingGitHub(t, reset, time.Time{},
				rateInstallation{id: 101, owner: "acme", repository: "acme/widgets", coreLimit: 5000, failure: rateLimitRefused})
			var written logtest.Buffer
			reading := newRateReading(rateReadingClient(t, base), slog.New(slog.NewJSONHandler(&written, nil)), "", (&steppedClock{at: start}).clock())
			resets := reading.pass(t.Context())
			if want := []time.Time{reset}; until == 50*time.Second {
				if len(resets) != 0 {
					t.Errorf("the partial reading left resets %v due, want none: the core reset was read in its last minute", resets)
				}
			} else if !slices.EqualFunc(resets, want, time.Time.Equal) {
				t.Errorf("the partial reading left resets %v due, want the core reset %v alone", resets, reset)
			}
			lines := logtest.JSONLines(t, written.String())
			if len(lines) != 1 || lines[0]["error"] == nil {
				t.Fatalf("the partial reading logged %v, want one error line", lines)
			}
			if _, present := lines[0]["core_used"]; present {
				t.Errorf("the failed reading carries a counter: %v", lines[0])
			}
			if got, want := lines[0]["before_reset"], until == 50*time.Second; got != want {
				t.Errorf("before_reset = %v, want %t for the installation's own reset", got, want)
			}
		})
	}
}

// A core answer can belong to the hour that ended while the GraphQL read was in flight. Its
// own calls must not be taken from the new hour, or from Own's empty expired window.
func TestARateReadingRefusesACounterWhoseResetPassedBeforeTheOwnSnapshot(t *testing.T) {
	start := time.Now().Truncate(time.Second)
	_, base := newRateReadingGitHub(t, start.Add(-time.Second), start.Add(time.Hour),
		rateInstallation{id: 101, owner: "acme", repository: "acme/widgets", coreLimit: 5000, graphQLLimit: 5000})
	var written logtest.Buffer
	reading := newRateReading(rateReadingClient(t, base, githubapp.WithRateCounter(githubrate.NewCounter())),
		slog.New(slog.NewJSONHandler(&written, nil)), "", (&steppedClock{at: start}).clock())
	reading.pass(t.Context())
	lines := logtest.JSONLines(t, written.String())
	if len(lines) != 1 {
		t.Fatalf("logged %v, want one error line", lines)
	}
	if cause, _ := lines[0]["error"].(string); !strings.Contains(cause, "reset") {
		t.Errorf("the elapsed reset was reported as %v, want an error naming the reset", lines[0])
	}
	if _, present := lines[0]["core_own"]; present {
		t.Errorf("the old core reading carries a current own-call snapshot: %v", lines[0])
	}
}

// A listed installation's slug is the App's current one, which the list names at every pass, and
// not the slug GET /app named when the reading first read it.
func TestARateReadingUsesTheListedAppsCurrentSlug(t *testing.T) {
	start := time.Now().Truncate(time.Second)
	github, _ := newRateReadingGitHub(t, start.Add(time.Hour), start.Add(time.Hour),
		rateInstallation{id: 101, owner: "acme", repository: "acme/widgets", coreLimit: 5000, graphQLLimit: 5000})
	base := githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/installations" {
			_, _ = fmt.Fprint(w, `[{"id":101,"app_slug":"renamed-app","account":{"login":"acme"}}]`)
			return
		}
		github.serve(w, r)
	}})
	var written logtest.Buffer
	reading := newRateReading(rateReadingClient(t, base), slog.New(slog.NewJSONHandler(&written, nil)), "", (&steppedClock{at: start}).clock())
	reading.appID, reading.appSlug = "4242", "example-app"
	reading.pass(t.Context())
	lines := logtest.JSONLines(t, written.String())
	if len(lines) != 1 || lines[0]["app_slug"] != "renamed-app" || lines[0]["error"] != nil {
		t.Errorf("the reading after the App was renamed is %v, want its freshly listed slug", lines)
	}
}

// A line takes the App's id and slug from the installation list, which names both for each
// installation, so a GET /app GitHub refuses fails no reading: every line still carries its
// counters and the list's App. GET /app is read only where the list names no App id, once, and
// remembered.
func TestARateReadingTakesTheAppFromTheInstallationList(t *testing.T) {
	for _, row := range []struct {
		name                       string
		appRefused, listNamesNoApp bool
		appID                      string
		appReads                   int
	}{
		{"GET /app refused, the list naming the App", true, false, "4242", 2},
		{"the list naming no App id", false, true, "4242", 1},
		{"GET /app refused, the list naming no App id", true, true, "", 2},
	} {
		t.Run(row.name, func(t *testing.T) {
			start := time.Now().Truncate(time.Second)
			github, base := newRateReadingGitHub(t, start.Add(time.Hour), start.Add(time.Hour),
				rateInstallation{id: 101, owner: "acme", repository: "acme/widgets", coreUsed: 11, coreLimit: 5000, graphQLUsed: 12, graphQLLimit: 5000})
			github.appRefused, github.listNamesNoApp = row.appRefused, row.listNamesNoApp
			var written logtest.Buffer
			lines := readPasses(t, rateReadingClient(t, base), &written, &steppedClock{at: start, passes: 2})
			if len(lines) != 2 {
				t.Fatalf("the passes logged %d lines, want 2:\n%s", len(lines), written.String())
			}
			for _, line := range lines {
				if _, failed := line["error"]; failed || line["core_used"] != float64(11) || line["graphql_used"] != float64(12) {
					t.Errorf("the line is %v, want both counters and no error", line)
				}
				if line["app_id"] != row.appID || line["app_slug"] != "example-app" {
					t.Errorf("the line names App %v %v, want %q example-app", line["app_id"], line["app_slug"], row.appID)
				}
			}
			github.mu.Lock()
			defer github.mu.Unlock()
			if want := map[bool]int{true: row.appReads, false: 0}[row.listNamesNoApp]; github.appReads != want {
				t.Errorf("GitHub was asked GET /app %d times, want %d", github.appReads, want)
			}
		})
	}
}

// A secondary limit GitHub answers an installation's token with holds that installation's
// reading in this process until it ends: GitHub answers installation 101's GET /rate_limit with a
// secondary limit at the first pass, the before-reset pass 25 s later, inside the minute GitHub
// asked for, sends 101 nothing and writes its line with an error naming the limit and its end,
// while installation 102 is read, and the pass after the limit ended reads 101 again. A secondary
// limit answered to the workflows read stops the same pass's GET /rate_limit too.
func TestASecondaryLimitHoldsItsInstallationsReadingUntilItEnds(t *testing.T) {
	t.Run("across passes", func(t *testing.T) {
		start := time.Now().Truncate(time.Second)
		github, base := newRateReadingGitHub(t, start.Add(70*time.Second), start.Add(time.Hour),
			rateInstallation{id: 101, owner: "acme", repository: "acme/widgets", coreUsed: 11, coreLimit: 5000, graphQLLimit: 5000, failure: rateLimitSecondary},
			rateInstallation{id: 102, owner: "example-org", repository: "example-org/gadgets", coreUsed: 21, coreLimit: 5000, graphQLLimit: 5000,
				coreReset: start.Add(time.Hour).Unix()},
		)
		calls := []int{}
		clock := &steppedClock{at: start, passes: 3, between: func(pass int) {
			github.mu.Lock()
			calls = append(calls, github.tokenCalls[101])
			if pass == 2 {
				// The third pass is in the core counter's next hour.
				github.coreReset = start.Add(2 * time.Hour).Unix()
			}
			github.mu.Unlock()
			if pass == 1 {
				github.change(failing(101, ""))
			}
		}}
		var written logtest.Buffer
		lines := readPasses(t, rateReadingClient(t, base, githubapp.WithRateCounter(githubrate.NewCounter())), &written, clock)
		if want := 70*time.Second - githubrate.BeforeReset; clock.waits[0] != want {
			t.Fatalf("the second pass was due %s after the first, want %s", clock.waits[0], want)
		}
		if got := installationsOf(lines); !slices.Equal(got, []string{"101", "102", "101", "102", "101", "102"}) {
			t.Fatalf("the passes logged installations %v, want 101 and 102 at each of three", got)
		}
		if cause, _ := lines[0]["error"].(string); !strings.Contains(cause, "403") {
			t.Errorf("the first pass's line of 101 is %v, want GitHub's secondary limit as its error", lines[0])
		}
		if len(calls) != 2 || calls[1] != calls[0] {
			t.Errorf("installation 101's token calls after each pass were %v, want none in the second pass", calls)
		}
		held, _ := lines[2]["error"].(string)
		if !strings.Contains(held, "secondary rate limit") || !strings.Contains(held, "installation 101") || !strings.Contains(held, "until 20") {
			t.Errorf("the second pass's line of 101 has error %q, want the secondary limit and its end", held)
		}
		if _, present := lines[2]["core_used"]; present {
			t.Errorf("the held line carries a counter: %v", lines[2])
		}
		if _, failed := lines[3]["error"]; failed {
			t.Errorf("installation 102's line in the second pass is %v, want a reading: the limit is 101's alone", lines[3])
		}
		if _, failed := lines[4]["error"]; failed || lines[4]["core_used"] != float64(11) {
			t.Errorf("the third pass's line of 101, after the limit ended, is %v, want a reading", lines[4])
		}
	})
	t.Run("within a pass", func(t *testing.T) {
		start := time.Now().Truncate(time.Second)
		github, base := newRateReadingGitHub(t, start.Add(time.Hour), start.Add(time.Hour),
			rateInstallation{id: 101, owner: "acme", repository: "acme/widgets", coreLimit: 5000, graphQLLimit: 5000, failure: workflowsSecondary})
		var written logtest.Buffer
		lines := readPasses(t, rateReadingClient(t, base, githubapp.WithRateCounter(githubrate.NewCounter())), &written, &steppedClock{at: start, passes: 1})
		if cause, _ := lines[0]["error"].(string); len(lines) != 1 || !strings.Contains(cause, "secondary rate limit") {
			t.Errorf("the pass logged %v, want one line whose error names the secondary limit", lines)
		}
		github.mu.Lock()
		defer github.mu.Unlock()
		if github.tokenCalls[101] != 2 {
			t.Errorf("installation 101's token made %d calls, want 2, the listing and the workflows read: GET /rate_limit is held", github.tokenCalls[101])
		}
	})
}

// counts is want as the JSON object a line carries it as: each count a number.
func counts(want map[string]int) map[string]any {
	decoded := map[string]any{}
	for kind, count := range want {
		decoded[kind] = float64(count)
	}
	return decoded
}

// rateReadingClient is an App client against the fake GitHub at base, built with options.
func rateReadingClient(t *testing.T, base string, options ...githubapp.Option) *githubapp.Client {
	t.Helper()
	_, pemText := githubapptest.Key(t)
	client, err := githubapp.New(&auth.AppConfig{ClientID: "Iv1.rate-reading", PEM: pemText}, base, options...)
	if err != nil {
		t.Fatalf("build the App client: %v", err)
	}
	return client
}

// A pass that has no installation to read logs one line saying why, so no pass is silent: a list
// GitHub refuses, with GitHub's status and message, and a list that holds no installation. Either
// line names the App and carries no counter.
func TestARateReadingPassWithNoInstallationToReadLogsOneErrorLine(t *testing.T) {
	for _, row := range []struct {
		name   string
		want   []string
		status int
		body   string
	}{
		{"a refused list", []string{"list the App's installations", "status 503", "Service Unavailable"}, http.StatusServiceUnavailable, `{"message":"Service Unavailable"}`},
		{"an empty list", []string{"listed no installation"}, http.StatusOK, `[]`},
	} {
		t.Run(row.name, func(t *testing.T) {
			base := githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/app":
					_, _ = fmt.Fprint(w, `{"id":4242,"slug":"example-app"}`)
				case "/app/installations":
					w.WriteHeader(row.status)
					_, _ = fmt.Fprint(w, row.body)
				default:
					githubtest.Unexpected(t, w, r)
				}
			}})
			client := rateReadingClient(t, base, githubapp.WithRateCounter(githubrate.NewCounter()))
			var written logtest.Buffer
			lines := readPasses(t, client, &written, &steppedClock{at: time.Now(), passes: 1})
			if len(lines) != 1 {
				t.Fatalf("the pass logged %d lines, want 1:\n%s", len(lines), written.String())
			}
			line := lines[0]
			for _, part := range row.want {
				if cause, _ := line["error"].(string); !strings.Contains(cause, part) {
					t.Errorf("error is %q, want it to say %q", cause, part)
				}
			}
			if line["msg"] != githubrate.Msg || line["app_slug"] != "example-app" || line["installations"] != float64(0) || line["installation_id"] != "" {
				t.Errorf("the line is %v, want %s naming the App, no installation and installations 0", line, githubrate.Msg)
			}
			if _, present := line["core_used"]; present {
				t.Errorf("a pass with no installation read carries a counter: %v", line)
			}
		})
	}
}

// A refused list writes one error line for each installation the reading holds, with its owner,
// its id, the permissions and the count the App's last answered list gave, so an owner whose list
// fails has a line for that hour, and R10 reads it as UNKNOWN rather than finding no line at all.
// The second pass's list is refused: installation 101, in its core counter's last minute, and 102
// each get their line, and 101's says before_reset. The daemon's reading does the same
// (TestRateReadingWritesALineForEachHeldInstallationOfARefusedList).
func TestARefusedListWritesALineForEachHeldInstallation(t *testing.T) {
	start := time.Now().Truncate(time.Second)
	github, base := newRateReadingGitHub(t, start.Add(70*time.Second), start.Add(time.Hour),
		rateInstallation{id: 101, owner: "acme", repository: "acme/widgets", coreUsed: 11, coreLimit: 5000, graphQLLimit: 5000,
			permissions: map[string]string{"contents": "read", "metadata": "read"}},
		rateInstallation{id: 102, owner: "example-org", repository: "example-org/gadgets", coreUsed: 21, coreLimit: 5000, graphQLLimit: 5000,
			coreReset: start.Add(time.Hour).Unix(), permissions: map[string]string{"metadata": "read"}},
	)
	clock := &steppedClock{at: start, passes: 2, between: func(int) {
		github.mu.Lock()
		defer github.mu.Unlock()
		github.listRefused = true
	}}
	var written logtest.Buffer

	lines := readPasses(t, rateReadingClient(t, base), &written, clock)

	if got := installationsOf(lines); !slices.Equal(got, []string{"101", "102", "101", "102"}) {
		t.Fatalf("the passes logged installations %v, want 101 and 102 at each of two:\n%s", got, written.String())
	}
	for n, want := range []struct {
		owner       string
		permissions map[string]any
		beforeReset bool
	}{
		{"acme", map[string]any{"contents": "read", "metadata": "read"}, true},
		{"example-org", map[string]any{"metadata": "read"}, false},
	} {
		line := lines[2+n]
		cause, _ := line["error"].(string)
		if !strings.Contains(cause, "list the App's installations") {
			t.Errorf("the refused pass's line of %s has error %q, want the refused list", want.owner, cause)
		}
		if line["owner"] != want.owner || line["installations"] != float64(2) || line["before_reset"] != want.beforeReset {
			t.Errorf("the refused pass's line is %v, want owner %s, installations 2 and before_reset %t", line, want.owner, want.beforeReset)
		}
		if got, _ := line["permissions"].(map[string]any); !maps.Equal(got, want.permissions) {
			t.Errorf("%s's permissions are %v, want the last list's %v", want.owner, got, want.permissions)
		}
		if _, present := line["core_used"]; present {
			t.Errorf("the refused pass's line carries a counter: %v", line)
		}
	}
}

// An installation that spent its core budget is the one whose reading before the reset matters
// most, and it is the one the shared ledger holds (github_rate_limits): every read under the
// installation's scope fails there with no request sent. The reading is sent past that hold, and
// takes the counter from GitHub's refusal, which names it as any answer does: the line carries the
// core counter at its limit and the GraphQL counter, and no error. GitHub refuses the workflows
// read in one row, and in the other the listing of the installation's repositories before it,
// where there is no repository to read the counter on and the refusal names it instead.
func TestARateReadingOfAnInstallationTheSharedLimitHoldsCarriesTheCounterGitHubAnswered(t *testing.T) {
	for _, row := range []struct {
		name    string
		refused string
	}{
		{"the workflows read refused", "/repos/acme/widgets/actions/workflows"},
		{"the repository listing refused", "/installation/repositories"},
	} {
		t.Run(row.name, func(t *testing.T) {
			reset := time.Now().Add(20 * time.Minute).Unix()
			base := githubtest.Server(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/app":
					_, _ = fmt.Fprint(w, `{"id":4242,"slug":"example-app"}`)
					return
				case r.URL.Path == "/app/installations":
					listed := `[]`
					if r.URL.Query().Get("page") == "1" {
						listed = `[{"id":101,"app_slug":"example-app","account":{"login":"acme"}}]`
					}
					_, _ = fmt.Fprint(w, listed)
					return
				case r.URL.Path == "/app/installations/101/access_tokens":
					w.WriteHeader(http.StatusCreated)
					_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_example_101", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
					return
				}
				header := w.Header()
				header.Set("X-RateLimit-Resource", githubrate.Core)
				header.Set("X-RateLimit-Used", "5000")
				header.Set("X-RateLimit-Limit", "5000")
				header.Set("X-RateLimit-Remaining", "0")
				header.Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
				switch r.URL.Path {
				case row.refused:
					w.WriteHeader(http.StatusForbidden)
					_, _ = fmt.Fprint(w, `{"message":"API rate limit exceeded for installation ID 101."}`)
				case "/installation/repositories":
					_, _ = fmt.Fprint(w, `{"total_count":1,"repositories":[{"full_name":"acme/widgets"}]}`)
				case "/rate_limit":
					_ = json.NewEncoder(w).Encode(map[string]any{"resources": map[string]any{
						"graphql": map[string]any{"limit": 5000, "used": 12, "remaining": 4988, "reset": reset},
					}})
				default:
					githubtest.Unexpected(t, w, r)
				}
			}})
			held := heldInstallation{scope: githubapp.InstallationScope(101)}
			client := rateReadingClient(t, base, githubapp.WithLimits(held), githubapp.WithRateCounter(githubrate.NewCounter()))
			var written logtest.Buffer
			lines := readPasses(t, client, &written, &steppedClock{at: time.Now(), passes: 1})
			if len(lines) != 1 {
				t.Fatalf("the pass logged %d lines, want 1:\n%s", len(lines), written.String())
			}
			line := lines[0]
			if cause, present := line["error"]; present {
				t.Errorf("the reading of an installation the shared limit holds is an error: %v", cause)
			}
			for field, want := range map[string]any{
				"installation_id": "101", "core_used": float64(5000), "core_limit": float64(5000),
				"core_reset": time.Unix(reset, 0).UTC().Format(time.RFC3339), "graphql_used": float64(12), "graphql_limit": float64(5000),
			} {
				if line[field] != want {
					t.Errorf("%s is %#v, want %#v: %v", field, line[field], want, line)
				}
			}
		})
	}
}

// heldInstallation is the shared ledger of GitHub's rate limits (githubapp.Limits) with a limit
// in force on scope for the next half hour and on no other scope, and that records nothing.
type heldInstallation struct{ scope string }

func (h heldInstallation) Wait(_ context.Context, scope string) (time.Duration, time.Time, error) {
	if scope == h.scope {
		return 30 * time.Minute, time.Now().Add(-time.Minute), nil
	}
	return 0, time.Time{}, nil
}

func (h heldInstallation) Record(context.Context, string, time.Duration) error { return nil }

func (h heldInstallation) Strike(context.Context, string, time.Duration) (ratelimit.Limit, error) {
	return ratelimit.Limit{}, nil
}

// The rate reading as the real envoy-dispatch binary runs it: main starts it with the App's key,
// writes its lines to stderr as JSON among the process's text lines, and every GitHub client of
// the process that calls with an installation token counts on the one Counter under the caller
// kind of the code path that called. A loopback GitHub with one installation answers every call
// an installation token makes with its counters, and its core reset is 70 s after the first token
// it mints, so the reading before that reset comes about 25 seconds in. By then the architecture
// import, the delivery reconcile and the CI reconcile have each read GitHub with a token, at start
// or on a one-second interval, and the test has made two calls through the API, a delivery
// settings save, whose access check reads the deploy repository, and a read through the GitHub
// proxy, and driven both delivery intakes: a merged pull request, which the GitHub intake fetches,
// and a job event of a deploy run Dispatch tracks, whose jobs the job intake lists. The reading at
// start counts no call of the intakes, and the reading before the reset counts every one of their
// reads as webhook_intake, the API's as two calls, the reading's own among the rest, and no call as
// other. The webhook redelivery sweep reads with the App's JWT alone, which no installation pays
// for, so it is in no count.
func TestTheServersRateReadingCountsEachCallerUnderItsKind(t *testing.T) {
	database := storetest.Open(t)
	seedCIReconcileDefault(t, database)
	seedDeliverySettings(t, database)
	started := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	if err := delivery.UpsertRun(t.Context(), database.Pool, delivery.DeliveryRun{
		Repo: "acme/deploy", RunID: 9001, Kind: delivery.DeliveryRunKindDeploy, HeadSHA: "sha9001",
		HeadCommitAt: started.Add(-time.Minute), StartedAt: started, URL: "https://github.com/acme/deploy/actions/runs/9001",
	}); err != nil {
		t.Fatalf("seed the running deploy run the job intake tracks: %v", err)
	}
	if _, err := database.Pool.Exec(t.Context(), `
		insert into projects (key, name, default_assignee) values ('ARCH', 'ARCH', 'alice');
		insert into architecture_sources (project_key, repo, branch, installation_id, created_by)
		values ('ARCH', 'acme/arch', 'main', 1, '{"kind":"session","id":"test"}');
	`); err != nil {
		t.Fatalf("seed the architecture source: %v", err)
	}
	_, natsURL := testnats.Start(t)
	makeCIStateBucket(t, natsURL)
	fake := &serverRateGitHub{t: t, graphQLReset: time.Now().Add(time.Hour).Unix()}
	github := githubtest.Serve(t, githubtest.Spec{Handle: fake.serve})

	env := githubAppEnv(t, github.URL)
	env["DISPATCH_TEST_CI_RECONCILE_INTERVAL"] = "1s"
	env["DISPATCH_TEST_RECONCILE_AT_START"] = "1"
	env["DISPATCH_TEST_ARCHITECTURE_IMPORT_AT_START"] = "1"
	process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{NATSURL: natsURL, Env: env})
	process.waitHealthy(t)
	process.call(t, http.MethodPut, "/api/v1/settings/delivery", `{"deploy_repo":"acme/deploy",
		"deploy_workflow_path":".github/workflows/deploy.yaml","pr_checks_workflow_path":".github/workflows/pr.yaml",
		"production_job_names":["production"],"population_authors":["octocat"]}`, http.StatusOK)
	process.call(t, http.MethodGet, "/api/github/rest/repos/acme/widgets/pulls/1", "", http.StatusOK)

	first := serverReading(t, process, "the reading at start", func(map[string]any) bool { return true })
	if intake := ownCounts(first)[intakeKind]; intake != 0 {
		t.Errorf("the reading at start counts %v calls of the intakes, which have read nothing yet: %v", intake, first)
	}
	publisher, err := bus.Connect([]string{natsURL})
	if err != nil {
		t.Fatalf("connect the publisher: %v", err)
	}
	t.Cleanup(publisher.Close)
	testwait.Eventually(t, "both intakes' durables bound", func() bool {
		for _, durable := range []string{bus.DispatchDeliveryDurable, bus.DispatchJobDurable} {
			if info, err := publisher.JS().ConsumerInfo(bus.Stream, durable); err != nil || !info.PushBound {
				return false
			}
		}
		return true
	})
	fake.mu.Lock()
	fake.intakePaths = map[string]int{}
	fake.mu.Unlock()
	publishGitHubEvent(t, publisher, contracts.GithubResourceSubject("acme", "deploy", "pr", "77"), map[string]string{
		"kind": "pr", "action": "closed", "merged": "true", "repo": "acme/deploy", "number": "77", "author": "octocat",
		"head_repo": "acme/deploy", "head_ref": "a-branch", "head_sha": fmt.Sprintf("%040d", 77),
	})
	publishGitHubEvent(t, publisher, "notifications.github.acme.deploy.job.9001.completed", map[string]string{
		"kind": "job", "repo": "acme/deploy", "run_id": "9001", "job_id": "1", "name": "production", "status": "completed", "conclusion": "success",
	})
	var intakeReads int
	testwait.Eventually(t, "the GitHub intake's read of the pull request and the job intake's of the run's jobs", func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		intakeReads = fake.intakePaths[intakePullRequest] + fake.intakePaths[intakeJobs]
		return fake.intakePaths[intakePullRequest] > 0 && fake.intakePaths[intakeJobs] > 0
	})

	reading := serverReading(t, process, "the reading before the reset", func(line map[string]any) bool { return line["before_reset"] == true })
	t.Logf("the reading before the reset: %v", reading)
	if !process.logged(`msg="dispatch: reading the GitHub rate counters of each installation of the App"`) {
		t.Errorf("the process logged no text line saying it reads the rate counters:\n%s", process.Output.String())
	}
	for field, want := range map[string]any{
		"program": "dispatch", "project": "", "app_id": "4242", "app_slug": "example-app",
		"owner": "acme", "installation_id": "1", "installations": float64(1),
	} {
		if reading[field] != want {
			t.Errorf("%s is %#v, want %#v", field, reading[field], want)
		}
	}
	own := ownCounts(reading)
	for _, kind := range []string{"architecture_import", "delivery_reconcile", "ci_reconcile", rateReadingCaller} {
		if own[kind] == 0 {
			t.Errorf("the reading counts no call of %s: %v", kind, own)
		}
	}
	if own[intakeKind] < float64(intakeReads) {
		t.Errorf("the reading counts %v calls of the intakes, want at least their %d reads of the pull request and the run's jobs: %v", own[intakeKind], intakeReads, own)
	}
	if own["api"] != 2 {
		t.Errorf("the reading counts %v calls of the API, want the settings save's and the proxy's, 2: %v", own["api"], own)
	}
	if _, untagged := own[githubrate.Other]; untagged {
		t.Errorf("the reading counts calls no caller tagged: %v", own)
	}
	if _, counted := own["webhook_redelivery"]; counted {
		t.Errorf("the reading counts calls of the webhook redelivery sweep, which reads with the App's JWT alone: %v", own)
	}
}

// intakeKind is the caller kind both delivery intakes tag their calls with (delivery's
// intakeCaller), which the binary test reads its counts under.
const intakeKind = "webhook_intake"

// The intakes' reads the binary test's GitHub counts: the GitHub intake's of the merged pull
// request, its commits among them, and the job intake's of the deploy run's jobs.
const (
	intakePullRequest = "/repos/acme/deploy/pulls/77"
	intakeJobs        = "/repos/acme/deploy/actions/runs/9001/jobs"
)

// serverReading waits for the process's first github_rate_reading line that holds says yes to,
// what naming what it waits for, and answers it decoded.
func serverReading(t *testing.T, process *dispatchProcess, what string, holds func(map[string]any) bool) map[string]any {
	t.Helper()
	var reading map[string]any
	testwait.WithinPolling(t, testwait.Bounded(t, 2*time.Minute, 5*time.Second), testwait.Poll, what, func() bool {
		for line := range strings.Lines(process.Output.String()) {
			if strings.HasPrefix(line, "{") && strings.Contains(line, `"msg":"`+githubrate.Msg+`"`) {
				if decoded := logtest.JSONLines(t, line)[0]; holds(decoded) {
					reading = decoded
					return true
				}
			}
		}
		return false
	})
	return reading
}

// ownCounts is a reading line's own counts of both counters, added up by caller kind.
func ownCounts(reading map[string]any) map[string]float64 {
	own := map[string]float64{}
	for _, counter := range []string{"core_own", "graphql_own"} {
		counts, _ := reading[counter].(map[string]any)
		for kind, count := range counts {
			own[kind] += count.(float64)
		}
	}
	return own
}

// publishGitHubEvent publishes a GitHub-shaped envelope on subject, as the Envoy listener
// publishes a webhook's, with payload's fields as its payload.
func publishGitHubEvent(t *testing.T, publisher *bus.Client, subject string, payload map[string]string) {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode the payload: %v", err)
	}
	id := "rate-reading-" + strings.ReplaceAll(subject, ".", "-")
	if err := publisher.Publish(contracts.Envelope{
		EventID: id, Source: "github", SourceEventID: id, TraceID: id, DedupeKey: id,
		Topic: subject, Payload: string(encoded), IssuedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatalf("publish %s: %v", subject, err)
	}
}

// serverRateGitHub is the loopback GitHub of the binary's rate reading test: one installation,
// 1, of the account acme, covering every repository the process reads; the App's own reads under
// its JWT; and an answer to every call an installation token makes that carries the
// installation's counter, GET /rate_limit and every 404 included. The core reset is 70 s after
// the first token it mints, and the GraphQL reset graphQLReset. Once intakePaths is set, it counts
// the calls under each intake path, prefix and all, that an installation token makes.
type serverRateGitHub struct {
	t            *testing.T
	graphQLReset int64
	mu           sync.Mutex
	minted       int
	coreReset    int64
	intakePaths  map[string]int
}

func (g *serverRateGitHub) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	installation := map[string]any{"id": 1, "app_slug": "example-app", "account": map[string]any{"login": "acme"}, "permissions": map[string]string{
		"contents": "read", "actions": "read", "pull_requests": "read", "checks": "read", "metadata": "read", "statuses": "read",
	}}
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ghs_") {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app":
			_, _ = fmt.Fprint(w, `{"id":4242,"slug":"example-app"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			listed := []any{}
			if r.URL.Query().Get("page") == "1" {
				listed = append(listed, installation)
			}
			_ = json.NewEncoder(w).Encode(listed)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/") && strings.HasSuffix(r.URL.Path, "/installation"):
			_ = json.NewEncoder(w).Encode(installation)
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/1/access_tokens":
			g.minted++
			if g.coreReset == 0 {
				g.coreReset = time.Now().Add(70 * time.Second).Unix()
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"token": fmt.Sprintf("ghs_server_%d", g.minted), "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
		case r.Method == http.MethodGet && r.URL.Path == "/app/hook/deliveries":
			_, _ = fmt.Fprint(w, `[]`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, `{"message":"Not Found"}`)
		}
		return
	}
	for _, path := range []string{intakePullRequest, intakeJobs} {
		if g.intakePaths != nil && strings.HasPrefix(r.URL.Path, path) {
			g.intakePaths[path]++
		}
	}
	resource, limit, used, reset := githubrate.Core, 5000, 250, g.coreReset
	if r.URL.Path == "/graphql" {
		resource, used, reset = githubrate.GraphQL, 60, g.graphQLReset
	}
	header := w.Header()
	header.Set("X-RateLimit-Resource", resource)
	header.Set("X-RateLimit-Used", strconv.Itoa(used))
	header.Set("X-RateLimit-Limit", strconv.Itoa(limit))
	header.Set("X-RateLimit-Remaining", strconv.Itoa(limit-used))
	header.Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
	owner, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/repos/"), "/")
	name, _, _ := strings.Cut(rest, "/")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/installation/repositories":
		_, _ = fmt.Fprint(w, `{"total_count":1,"repositories":[{"full_name":"acme/widgets"}]}`)
	case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/actions/workflows":
		_, _ = fmt.Fprint(w, `{"total_count":0,"workflows":[]}`)
	case r.Method == http.MethodGet && r.URL.Path == "/rate_limit":
		_ = json.NewEncoder(w).Encode(map[string]any{"resources": map[string]any{
			"core":    map[string]any{"limit": limit, "used": 250, "remaining": limit - 250, "reset": g.coreReset},
			"graphql": map[string]any{"limit": limit, "used": 60, "remaining": limit - 60, "reset": g.graphQLReset},
		}})
	case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/1":
		_, _ = fmt.Fprint(w, `{"number":1,"state":"open"}`)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/") && rest != "" && !strings.Contains(rest, "/"):
		_ = json.NewEncoder(w).Encode(map[string]any{"full_name": owner + "/" + name, "id": 4242})
	case r.Method == http.MethodPost && r.URL.Path == "/graphql":
		_, _ = fmt.Fprint(w, `{"data":null,"errors":[{"message":"the stand-in answers no query"}]}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprint(w, `{"message":"Not Found"}`)
	}
}
