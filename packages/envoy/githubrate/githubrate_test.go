package githubrate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/githubtest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// limitServer is a GitHub stand-in that answers every request 200 with the X-RateLimit-Resource
// and X-RateLimit-Reset headers answer names for it, the X-RateLimit-Used and -Limit headers
// beside them, and, at a path ending /rate_limit, a body whose resources.graphql holds graphql. A
// request that carries If-None-Match is answered 304 Not Modified with the same headers, as
// GitHub answers a conditional read of an unchanged answer.
type limitServer struct {
	mu     sync.Mutex
	answer func(*http.Request) (resource string, reset time.Time)
}

// graphql is what the stand-in's GET /rate_limit answers as resources.graphql.
var graphql = struct {
	Used, Limit int
	Reset       time.Time
}{Used: 812, Limit: 5000, Reset: time.Unix(1_900_000_600, 0).UTC()}

func newLimitServer(t *testing.T, answer func(*http.Request) (string, time.Time)) (*limitServer, *httptest.Server) {
	t.Helper()
	limits := &limitServer{answer: answer}
	server := githubtest.Serve(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		limits.mu.Lock()
		resource, reset := limits.answer(r)
		limits.mu.Unlock()
		w.Header().Set("X-RateLimit-Resource", resource)
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		w.Header().Set("X-RateLimit-Used", "4213")
		w.Header().Set("X-RateLimit-Limit", "15000")
		if r.Header.Get("If-None-Match") != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/rate_limit") {
			_, _ = io.WriteString(w, `{"resources":{"core":{"limit":15000,"used":4213,"remaining":10787,"reset":1900000000},"graphql":{"limit":`+
				strconv.Itoa(graphql.Limit)+`,"used":`+strconv.Itoa(graphql.Used)+`,"remaining":4188,"reset":`+
				strconv.FormatInt(graphql.Reset.Unix(), 10)+`}}}`)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}})
	return limits, server
}

// answering answers every request with resource and reset.
func answering(resource string, reset time.Time) func(*http.Request) (string, time.Time) {
	return func(*http.Request) (string, time.Time) { return resource, reset }
}

// byPath answers a request to a path holding "graphql" as the graphql counter, one holding
// "search" as the search counter, which no count takes, and every other request as core, each
// with reset.
func byPath(reset time.Time) func(*http.Request) (string, time.Time) {
	return func(r *http.Request) (string, time.Time) {
		if strings.Contains(r.URL.Path, "graphql") {
			return "graphql", reset
		}
		if strings.Contains(r.URL.Path, "search") {
			return "search", reset
		}
		return "core", reset
	}
}

// call sends GET url through client with authorization as its Authorization header, under
// caller when it is not empty, and reads the answer whole.
func call(t *testing.T, client *http.Client, url, authorization, caller string) http.Header {
	t.Helper()
	return send(t, client, url, http.Header{"Authorization": {authorization}}, caller)
}

// send is call with every header of header, for a request that carries more than a credential.
func send(t *testing.T, client *http.Client, url string, header http.Header, caller string) http.Header {
	t.Helper()
	ctx := context.Background()
	if caller != "" {
		ctx = WithCaller(ctx, caller)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, values := range header {
		if len(values) > 0 && values[0] != "" {
			request.Header[name] = values
		}
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatal(err)
	}
	return response.Header
}

func wantCounts(t *testing.T, what string, got, want map[string]int) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s: Own answered a nil map; want an empty one", what)
	}
	if !maps.Equal(got, want) {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
}

func TestTransportCountsByInstallationCounterAndCaller(t *testing.T) {
	reset := time.Now().Add(time.Hour).Truncate(time.Second)
	_, server := newLimitServer(t, byPath(reset))
	counter := NewCounter()
	counter.Register("ghs_first", "101")
	counter.Register("ghs_second", "202")
	client := &http.Client{Transport: counter.Transport(nil)}

	call(t, client, server.URL+"/repos/acme/widgets/pulls", "Bearer ghs_first", "outbox")
	call(t, client, server.URL+"/repos/acme/widgets/pulls/1", "token ghs_first", "outbox")
	call(t, client, server.URL+"/repos/acme/widgets/pulls/2", "Bearer ghs_first", "pull_request_reads")
	call(t, client, server.URL+"/graphql", "Bearer ghs_first", "")
	call(t, client, server.URL+"/search/issues", "Bearer ghs_first", "outbox")
	call(t, client, server.URL+"/repos/acme/widgets/pulls", "Bearer ghs_second", "outbox")

	core, graphql := counter.Own("101")
	wantCounts(t, "installation 101's core", core, map[string]int{"outbox": 2, "pull_request_reads": 1})
	wantCounts(t, "installation 101's graphql", graphql, map[string]int{"other": 1})
	core, graphql = counter.Own("202")
	wantCounts(t, "installation 202's core", core, map[string]int{"outbox": 1})
	wantCounts(t, "installation 202's graphql", graphql, map[string]int{})
	core, graphql = counter.Own("303")
	wantCounts(t, "installation 303's core", core, map[string]int{})
	wantCounts(t, "installation 303's graphql", graphql, map[string]int{})
}

// testCountsStartAgainAtANewReset is one installation's hour through a second's drift either way,
// the next hour, a late answer from the hour before, and an hour that ends with no call after it.
func testCountsStartAgainAtANewReset(t *testing.T) {
	first := time.Now().Add(30 * time.Minute).Truncate(time.Second)
	second := first.Add(time.Hour)
	limits, server := newLimitServer(t, answering("core", first))
	counter := NewCounter()
	counter.Register("ghs_first", "101")
	client := &http.Client{Transport: counter.Transport(nil)}

	for _, row := range []struct {
		name   string
		reset  time.Time
		caller string
		want   map[string]int
	}{
		{"the first call", first, "outbox", map[string]int{"outbox": 1}},
		{"a reset drifting one second later", first.Add(time.Second), "outbox", map[string]int{"outbox": 2}},
		{"a reset drifting one second earlier", first.Add(-time.Second), "default_branch", map[string]int{"outbox": 2, "default_branch": 1}},
		{"the next hour", second, "outbox", map[string]int{"outbox": 1}},
		{"a late answer from the previous hour", first, "default_branch", map[string]int{"outbox": 1}},
	} {
		limits.mu.Lock()
		limits.answer = answering("core", row.reset)
		limits.mu.Unlock()
		call(t, client, server.URL+"/repos/acme/widgets/pulls", "Bearer ghs_first", row.caller)
		core, _ := counter.Own("101")
		wantCounts(t, row.name, core, row.want)
		if row.reset.Before(second) && !row.reset.Equal(first) {
			counter.now = func() time.Time { return first.Add(500 * time.Millisecond) }
			core, _ = counter.Own("101")
			wantCounts(t, "the window keeps the latest drifted expiry", core, row.want)
			counter.now = time.Now
		}
	}

	counter.now = func() time.Time { return second.Add(time.Second) }
	core, _ := counter.Own("101")
	wantCounts(t, "a window that expired without another call", core, map[string]int{})
}

// measuredReset is a reset GitHub answered, and measuredDrift the second by which its next answer
// for the same installation and counter named it later, within one hour.
const (
	measuredReset = 1795202520
	measuredDrift = 1795202521 - measuredReset
)

// Both sides of resetDrift's edge, for one installation and counter: a reset named later by
// GitHub's measured drift of a second, by 4 min 59 s or by exactly 5 minutes keeps the counts as
// the same hour, and one 5 min 1 s later starts them again. An answer naming a reset 4 min 59 s
// or exactly 5 minutes earlier than the one held is counted in the held hour, and one 5 min 1 s
// earlier is a late answer from the hour before, which is not counted.
func TestCountsStartAgainAtANewReset(t *testing.T) {
	t.Run("an hour's drift and the next hour", testCountsStartAgainAtANewReset)
	first := time.Unix(measuredReset, 0).UTC()
	for _, row := range []struct {
		name  string
		shift time.Duration
		want  map[string]int
	}{
		{"a reset GitHub's measured second later", time.Duration(measuredDrift) * time.Second, map[string]int{"outbox": 2}},
		{"a reset 4 min 59 s later", 4*time.Minute + 59*time.Second, map[string]int{"outbox": 2}},
		{"a reset exactly 5 min later", 5 * time.Minute, map[string]int{"outbox": 2}},
		{"a reset 5 min 1 s later", 5*time.Minute + time.Second, map[string]int{"outbox": 1}},
		{"a reset 4 min 59 s earlier", -(4*time.Minute + 59*time.Second), map[string]int{"outbox": 2}},
		{"a reset exactly 5 min earlier", -5 * time.Minute, map[string]int{"outbox": 2}},
		{"a reset 5 min 1 s earlier", -(5*time.Minute + time.Second), map[string]int{"outbox": 1}},
	} {
		t.Run(row.name, func(t *testing.T) {
			limits, server := newLimitServer(t, answering("core", first))
			counter := NewCounter()
			// Half an hour before the first reset, so neither window has expired when read.
			counter.now = func() time.Time { return first.Add(-30 * time.Minute) }
			counter.Register("ghs_first", "101")
			client := &http.Client{Transport: counter.Transport(nil)}
			call(t, client, server.URL+"/repos/acme/widgets/pulls", "Bearer ghs_first", "outbox")
			limits.mu.Lock()
			limits.answer = answering("core", first.Add(row.shift))
			limits.mu.Unlock()
			call(t, client, server.URL+"/repos/acme/widgets/pulls", "Bearer ghs_first", "outbox")
			core, _ := counter.Own("101")
			wantCounts(t, row.name, core, row.want)
		})
	}
}

func TestAnUnregisteredTokenIsNotCounted(t *testing.T) {
	reset := time.Now().Add(time.Hour).Truncate(time.Second)
	_, server := newLimitServer(t, answering("core", reset))
	counter := NewCounter()
	counter.Register("ghs_first", "101")
	client := &http.Client{Transport: counter.Transport(nil)}

	// The App's JWT, a token nobody registered, no credential at all, and a registered token
	// under a scheme GitHub reads no token from.
	call(t, client, server.URL+"/app/installations", "Bearer app-jwt", "rate_reading")
	call(t, client, server.URL+"/repos/acme/widgets", "Bearer ghs_someone_else", "outbox")
	call(t, client, server.URL+"/repos/acme/widgets", "", "outbox")
	call(t, client, server.URL+"/repos/acme/widgets", "Basic ghs_first", "outbox")
	// GET /rate_limit costs nothing, so it is no call against the counter.
	call(t, client, server.URL+"/rate_limit", "Bearer ghs_first", "rate_reading")

	core, graphql := counter.Own("101")
	wantCounts(t, "core", core, map[string]int{})
	wantCounts(t, "graphql", graphql, map[string]int{})
}

// A registration lasts past the life GitHub gives an installation token, and no longer, whether
// or not anything registers another token after it: the count reads the registration's age
// itself. The window's reset is four hours out, so an empty count is the lapsed registration's
// and not a window that ended.
func TestARegistrationLapsesWithNoLaterRegister(t *testing.T) {
	registered := time.Now().Truncate(time.Second)
	_, server := newLimitServer(t, answering("core", registered.Add(4*time.Hour)))
	counter := NewCounter()
	counter.now = func() time.Time { return registered }
	counter.Register("ghs_first", "101")
	client := &http.Client{Transport: counter.Transport(nil)}

	counter.now = func() time.Time { return registered.Add(time.Hour) }
	call(t, client, server.URL+"/repos/acme/widgets", "Bearer ghs_first", "outbox")
	core, _ := counter.Own("101")
	wantCounts(t, "a token an hour old", core, map[string]int{"outbox": 1})

	counter.now = func() time.Time { return registered.Add(3 * time.Hour) }
	call(t, client, server.URL+"/repos/acme/widgets", "Bearer ghs_first", "outbox")
	core, _ = counter.Own("101")
	wantCounts(t, "a token three hours old", core, map[string]int{"outbox": 1})
}

// GitHub charges an authenticated conditional read that it answers 304 Not Modified nothing
// (its REST best practices), so no 304 is counted; the same read answered 200 is.
func TestANotModifiedAnswerIsNotCounted(t *testing.T) {
	_, server := newLimitServer(t, answering("core", time.Now().Add(time.Hour)))
	counter := NewCounter()
	counter.Register("ghs_first", "101")
	client := &http.Client{Transport: counter.Transport(nil)}

	call(t, client, server.URL+"/repos/acme/widgets/pulls/1", "Bearer ghs_first", "ci_reconcile")
	send(t, client, server.URL+"/repos/acme/widgets/pulls/1", http.Header{
		"Authorization": {"Bearer ghs_first"}, "If-None-Match": {`"abc"`},
	}, "ci_reconcile")
	core, _ := counter.Own("101")
	wantCounts(t, "a 200 and a 304", core, map[string]int{"ci_reconcile": 1})
}

// GET /rate_limit costs nothing, at GitHub's API root or at a GitHub Enterprise Server's /api/v3
// root, so neither is counted. A repository named rate_limit is a repository like any other: a read
// of it is charged and counted at either root, as is a read of a path under it.
func TestOnlyGitHubsRateLimitEndpointIsNotCounted(t *testing.T) {
	_, server := newLimitServer(t, answering("core", time.Now().Add(time.Hour)))
	counter := NewCounter()
	counter.Register("ghs_first", "101")
	client := &http.Client{Transport: counter.Transport(nil)}

	counted := 0
	for _, row := range []struct {
		path    string
		counted bool
	}{
		{"/rate_limit", false},
		{"/api/v3/rate_limit", false},
		{"/repos/acme/rate_limit", true},
		{"/api/v3/repos/acme/rate_limit", true},
		{"/repos/acme/widgets/contents/rate_limit", true},
		{"/api/v3/repos/acme/widgets", true},
	} {
		call(t, client, server.URL+row.path, "Bearer ghs_first", "rate_reading")
		if row.counted {
			counted++
		}
		want := map[string]int{}
		if counted > 0 {
			want["rate_reading"] = counted
		}
		core, _ := counter.Own("101")
		wantCounts(t, "after GET "+row.path, core, want)
	}
}

// Each installation's core and GraphQL windows start again at their own reset alone: the core
// window of one installation rolling into its next hour leaves that installation's GraphQL window
// and the other installation's core window counting where they were.
func TestTheCoreAndGraphQLWindowsOfTwoInstallationsRollApart(t *testing.T) {
	coreReset := time.Now().Add(20 * time.Minute).Truncate(time.Second)
	graphQLReset := coreReset.Add(25 * time.Minute)
	next := map[string]time.Time{}
	limits, server := newLimitServer(t, nil)
	limits.answer = func(r *http.Request) (string, time.Time) {
		installation := r.Header.Get("Authorization")
		if strings.Contains(r.URL.Path, "graphql") {
			return "graphql", graphQLReset
		}
		if reset, ok := next[installation]; ok {
			return "core", reset
		}
		return "core", coreReset
	}
	counter := NewCounter()
	counter.Register("ghs_first", "101")
	counter.Register("ghs_second", "202")
	client := &http.Client{Transport: counter.Transport(nil)}
	for _, token := range []string{"Bearer ghs_first", "Bearer ghs_second"} {
		call(t, client, server.URL+"/repos/acme/widgets", token, "ci_reconcile")
		call(t, client, server.URL+"/graphql", token, "ci_reconcile")
	}

	limits.mu.Lock()
	next["Bearer ghs_first"] = coreReset.Add(time.Hour)
	limits.mu.Unlock()
	call(t, client, server.URL+"/repos/acme/widgets", "Bearer ghs_first", "api")
	call(t, client, server.URL+"/graphql", "Bearer ghs_first", "api")
	call(t, client, server.URL+"/repos/acme/widgets", "Bearer ghs_second", "api")

	core, graphql := counter.Own("101")
	wantCounts(t, "installation 101's core, rolled into its next hour", core, map[string]int{"api": 1})
	wantCounts(t, "installation 101's GraphQL, in the same hour", graphql, map[string]int{"ci_reconcile": 1, "api": 1})
	core, graphql = counter.Own("202")
	wantCounts(t, "installation 202's core, in the same hour", core, map[string]int{"ci_reconcile": 1, "api": 1})
	wantCounts(t, "installation 202's GraphQL", graphql, map[string]int{"ci_reconcile": 1})
}

// Calls counted from many goroutines at once, as a process's loops and requests make them, add up
// exactly: every call of every installation and kind, none lost and none counted twice.
func TestCallsCountedAtOnceAddUpExactly(t *testing.T) {
	_, server := newLimitServer(t, byPath(time.Now().Add(time.Hour)))
	counter := NewCounter()
	counter.Register("ghs_first", "101")
	counter.Register("ghs_second", "202")
	client := &http.Client{Transport: counter.Transport(nil)}
	const goroutines, calls = 8, 25
	var running sync.WaitGroup
	for worker := range goroutines {
		running.Go(func() {
			token := []string{"Bearer ghs_first", "Bearer ghs_second"}[worker%2]
			ctx := WithCaller(context.Background(), fmt.Sprintf("kind_%d", worker%4))
			for range calls {
				for _, path := range []string{"/repos/acme/widgets", "/graphql"} {
					request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+path, nil)
					if err != nil {
						t.Error(err)
						return
					}
					request.Header.Set("Authorization", token)
					response, err := client.Do(request)
					if err != nil {
						t.Error(err)
						return
					}
					_, _ = io.Copy(io.Discard, response.Body)
					response.Body.Close()
				}
			}
		})
	}
	running.Wait()
	for installation, kinds := range map[string]map[string]int{
		"101": {"kind_0": 2 * calls, "kind_2": 2 * calls},
		"202": {"kind_1": 2 * calls, "kind_3": 2 * calls},
	} {
		core, graphql := counter.Own(installation)
		wantCounts(t, "installation "+installation+"'s core", core, kinds)
		wantCounts(t, "installation "+installation+"'s GraphQL", graphql, kinds)
	}
}

// Next's contract: its caller passes only the resets it has not yet read in their last minute
// (InLastMinute at the read that named each). A reset in the future and within LastMinute of now
// is due at once; otherwise the earliest reset less BeforeReset, when that comes first, is due
// with beforeReset true; otherwise the next reading is an Interval out. A reset the caller already
// read in its last minute is not passed, so it asks for nothing, whatever a later header names it
// as: the last row is the caller's whose every reset was read there, drift and all.
func TestNextReadsInTheLastMinuteBeforeAReset(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	soon := now.Add(5 * time.Minute)
	_, server := newLimitServer(t, answering("core", soon))
	headers := call(t, http.DefaultClient, server.URL+"/repos/acme/widgets/actions/workflows?per_page=1", "Bearer ghs_first", "")
	core, err := CoreBucket(headers)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name        string
		resets      []time.Time
		at          time.Time
		beforeReset bool
	}{
		{"a reset inside the interval", []time.Time{now.Add(40 * time.Minute), core.Reset, {}}, soon.Add(-BeforeReset), true},
		{"no reset inside the interval", []time.Time{now.Add(40 * time.Minute)}, now.Add(Interval), false},
		{"a reset just outside the last minute", []time.Time{now.Add(70 * time.Second)}, now.Add(25 * time.Second), true},
		{"a reset in its last minute, not yet read in it", []time.Time{now.Add(30 * time.Second)}, now, true},
		{"a reset at the last minute's boundary", []time.Time{now.Add(LastMinute)}, now, true},
		{"a reset in its last minute beside a later one", []time.Time{now.Add(5 * time.Minute), now.Add(20 * time.Second)}, now, true},
		{"a reset at now", []time.Time{now}, now.Add(Interval), false},
		{"a reset that passed", []time.Time{now.Add(-time.Second)}, now.Add(Interval), false},
		{"no reset the caller has not read in its last minute", nil, now.Add(Interval), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at, beforeReset := Next(now, tc.resets)
			if !at.Equal(tc.at) || beforeReset != tc.beforeReset {
				t.Fatalf("Next = %v, %v; want %v, %v", at, beforeReset, tc.at, tc.beforeReset)
			}
		})
	}
}

// A reading is in its last minute by its own resets alone, which a line's before_reset is: in one
// pass, the reading of an installation whose core reset is 40 s out is in it, and the reading of
// another whose resets are an hour and two out is not, though the first's reset is in the same
// pass. The rows hold the window's edges.
func TestInLastMinuteUsesTheReadingsOwnReset(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	for _, row := range []struct {
		name   string
		resets []time.Time
		want   bool
	}{
		{"no reset", nil, false},
		{"an absent reset", []time.Time{{}}, false},
		{"a passed reset", []time.Time{now.Add(-time.Second)}, false},
		{"at the reset", []time.Time{now}, false},
		{"in the last minute", []time.Time{now.Add(50 * time.Second)}, true},
		{"at the last minute's boundary", []time.Time{now.Add(time.Minute)}, true},
		{"outside the last minute", []time.Time{now.Add(70 * time.Second)}, false},
		{"only GraphQL is in its last minute", []time.Time{now.Add(time.Hour), now.Add(30 * time.Second)}, true},
	} {
		if got := InLastMinute(now, row.resets...); got != row.want {
			t.Errorf("%s: InLastMinute = %t, want %t", row.name, got, row.want)
		}
	}
	inIt := []time.Time{now.Add(40 * time.Second), now.Add(time.Hour)}
	another := []time.Time{now.Add(time.Hour), now.Add(2 * time.Hour)}
	if !InLastMinute(now, inIt...) || InLastMinute(now, another...) {
		t.Errorf("in one pass, the reading in its last minute is %t and the other %t, want true and false",
			InLastMinute(now, inIt...), InLastMinute(now, another...))
	}
}

func TestWriteIsOneJSONRecord(t *testing.T) {
	reset := time.Unix(1_900_000_000, 0).UTC()
	_, server := newLimitServer(t, answering("core", reset))
	counter := NewCounter()
	counter.Register("ghs_first", "101")
	client := &http.Client{Transport: counter.Transport(nil)}
	call(t, client, server.URL+"/repos/acme/widgets/pulls", "Bearer ghs_first", "outbox")
	core, err := CoreBucket(call(t, client, server.URL+"/repos/acme/widgets/actions/workflows?per_page=1", "Bearer ghs_first", "rate_reading"))
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodGet, server.URL+"/rate_limit", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	graphQL, err := GraphQLBucket(body)
	if err != nil {
		t.Fatal(err)
	}
	core.Own, graphQL.Own = counter.Own("101")

	var out logtest.Buffer
	log := slog.New(slog.NewJSONHandler(&out, nil))
	Write(log, Reading{
		Program: "daemon", Process: "legion-acme-daemon-0", Project: "ACME",
		AppID: "4242", AppSlug: "example-app",
		Owner: "acme", InstallationID: "101", Installations: 3,
		Permissions: map[string]string{"contents": "read", "checks": "write", "metadata": "read"},
		BeforeReset: true,
		Core:        core, GraphQL: graphQL,
	})
	Write(log, Reading{
		Program: "dispatch", Process: "dispatch-0",
		AppID: "4242", AppSlug: "example-app",
		Owner: "widgets-inc", InstallationID: "202", Installations: 3,
		Error: "mint a token for installation 202: GitHub answered 403",
	})
	Write(log, Reading{
		Program: "dispatch", Process: "dispatch-0",
		AppID: "4242", AppSlug: "example-app",
		Error: "list the App's installations: GitHub answered 503",
	})

	lines := logtest.JSONLines(t, out.String())
	if len(lines) != 3 {
		t.Fatalf("Write wrote %d lines, want one for each reading:\n%s", len(lines), out.String())
	}
	read, failed, unlisted := lines[0], lines[1], lines[2]
	want := map[string]any{
		"msg": "github_rate_reading", "level": "INFO",
		"program": "daemon", "process": "legion-acme-daemon-0", "project": "ACME",
		"app_id": "4242", "app_slug": "example-app",
		"owner": "acme", "installation_id": "101", "installations": float64(3),
		"permissions":  map[string]any{"contents": "read", "checks": "write", "metadata": "read"},
		"before_reset": true,
		"core_used":    float64(4213), "core_limit": float64(15000), "core_reset": "2030-03-17T17:46:40Z",
		"core_own":     map[string]any{"outbox": float64(1), "rate_reading": float64(1)},
		"graphql_used": float64(812), "graphql_limit": float64(5000), "graphql_reset": "2030-03-17T17:56:40Z",
		"graphql_own": map[string]any{},
	}
	delete(read, "time")
	assertRecord(t, read, want)

	// A failed reading of a listed installation names its permissions too, as an empty object
	// where the list named none.
	delete(failed, "time")
	assertRecord(t, failed, map[string]any{
		"msg": "github_rate_reading", "level": "INFO",
		"program": "dispatch", "process": "dispatch-0", "project": "",
		"app_id": "4242", "app_slug": "example-app",
		"owner": "widgets-inc", "installation_id": "202", "installations": float64(3),
		"permissions":  map[string]any{},
		"before_reset": false,
		"error":        "mint a token for installation 202: GitHub answered 403",
	})

	// A line that names no installation, a failed list's, has no permissions to name.
	delete(unlisted, "time")
	assertRecord(t, unlisted, map[string]any{
		"msg": "github_rate_reading", "level": "INFO",
		"program": "dispatch", "process": "dispatch-0", "project": "",
		"app_id": "4242", "app_slug": "example-app",
		"owner": "", "installation_id": "", "installations": float64(0),
		"before_reset": false,
		"error":        "list the App's installations: GitHub answered 503",
	})
}

func assertRecord(t *testing.T, got, want map[string]any) {
	t.Helper()
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Fatalf("record\n got %s\nwant %s", gotJSON, wantJSON)
	}
}

func TestTheCountersAreReadFromGitHubsAnswers(t *testing.T) {
	headers := http.Header{}
	headers.Set("X-RateLimit-Resource", "core")
	headers.Set("X-RateLimit-Used", "7")
	headers.Set("X-RateLimit-Limit", "5000")
	headers.Set("X-RateLimit-Reset", "1900000000")
	core, err := CoreBucket(headers)
	if err != nil {
		t.Fatal(err)
	}
	if core.Used != 7 || core.Limit != 5000 || !core.Reset.Equal(time.Unix(1_900_000_000, 0)) {
		t.Fatalf("CoreBucket = %+v", core)
	}

	for _, tc := range []struct{ name, header, value string }{
		{"another counter", "X-RateLimit-Resource", "search"},
		{"no used count", "X-RateLimit-Used", ""},
		{"a negative used count", "X-RateLimit-Used", "-1"},
		{"no limit", "X-RateLimit-Limit", ""},
		{"a reset that is no epoch second", "X-RateLimit-Reset", "soon"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			broken := headers.Clone()
			broken.Set(tc.header, tc.value)
			if _, err := CoreBucket(broken); err == nil {
				t.Fatalf("CoreBucket read %s=%q as a reading", tc.header, tc.value)
			}
		})
	}

	if _, err := GraphQLBucket([]byte(`{"resources":{"core":{"limit":5000,"used":1,"reset":1900000000}}}`)); err == nil {
		t.Fatal("GraphQLBucket read an answer with no resources.graphql as a reading")
	}
	if _, err := GraphQLBucket([]byte(`not json`)); err == nil {
		t.Fatal("GraphQLBucket read an answer that is not JSON as a reading")
	}
}

func TestGraphQLBucketRefusesAnAbsentOrNegativeUsedCount(t *testing.T) {
	for _, body := range []string{
		`{"resources":{"graphql":{"limit":5000,"reset":1900000000}}}`,
		`{"resources":{"graphql":{"limit":5000,"used":null,"reset":1900000000}}}`,
		`{"resources":{"graphql":{"limit":5000,"used":-1,"reset":1900000000}}}`,
	} {
		if _, err := GraphQLBucket([]byte(body)); err == nil {
			t.Errorf("GraphQLBucket accepted an unreported or negative used count: %s", body)
		}
	}
	if bucket, err := GraphQLBucket([]byte(`{"resources":{"graphql":{"limit":5000,"used":0,"reset":1900000000}}}`)); err != nil || bucket.Used != 0 {
		t.Errorf("GraphQLBucket refused a reported zero: %+v, %v", bucket, err)
	}
}

// refusingServer answers every request with status, header and body, as GitHub answers a request
// it refuses.
func refusingServer(t *testing.T, status int, header http.Header, body string) string {
	t.Helper()
	return githubtest.Serve(t, githubtest.Spec{Handle: func(w http.ResponseWriter, r *http.Request) {
		for name, values := range header {
			w.Header()[name] = values
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}}).URL
}

// A secondary limit GitHub answers a registered token with is noted for that token's installation
// until the end GitHub asked for, from the answer alone: a 403 or 429 whose message names the
// secondary limit, whatever X-RateLimit-Remaining says, or, with requests remaining, one that
// carries Retry-After, is a 429, or whose message names a rate limit. The wait is Retry-After's
// seconds, else a minute. A primary limit, no requests remaining and no secondary wording, is not
// noted, since its refusal still names the spent counter; neither is a 403 that is a refusal and
// no limit, nor an answer to a token nobody registered. A body the noting read is still the
// caller's to read whole.
func TestASecondaryLimitIsNotedForItsInstallationFromGitHubsAnswer(t *testing.T) {
	now := time.Unix(measuredReset, 0).UTC().Add(-30 * time.Minute)
	secondary := `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`
	refusal := `{"message":"Resource not accessible by integration"}`
	primary := `{"message":"API rate limit exceeded for installation ID 101."}`
	for _, row := range []struct {
		name        string
		status      int
		header      http.Header
		body, token string
		want        time.Duration
		noted       bool
	}{
		{"a 403 with Retry-After", http.StatusForbidden, http.Header{"Retry-After": {"120"}, "X-Ratelimit-Remaining": {"4000"}}, secondary, "ghs_first", 2 * time.Minute, true},
		{"a 429 with Retry-After", http.StatusTooManyRequests, http.Header{"Retry-After": {"30"}, "X-Ratelimit-Remaining": {"4000"}}, secondary, "ghs_first", 30 * time.Second, true},
		{"a 403 naming a secondary limit with no Retry-After", http.StatusForbidden, http.Header{"X-Ratelimit-Remaining": {"4000"}}, secondary, "ghs_first", time.Minute, true},
		{"a 429 with no Retry-After", http.StatusTooManyRequests, http.Header{"X-Ratelimit-Remaining": {"12"}}, `{}`, "ghs_first", time.Minute, true},
		{"a 403 naming the secondary limit with no requests remaining", http.StatusForbidden, http.Header{"Retry-After": {"120"}, "X-Ratelimit-Remaining": {"0"}}, secondary, "ghs_first", 2 * time.Minute, true},
		{"a 403 with no requests remaining", http.StatusForbidden, http.Header{"X-Ratelimit-Remaining": {"0"}}, primary, "ghs_first", 0, false},
		{"a 429 with no requests remaining", http.StatusTooManyRequests, http.Header{"X-Ratelimit-Remaining": {"0"}}, primary, "ghs_first", 0, false},
		{"a 403 that refuses and is no limit", http.StatusForbidden, http.Header{"X-Ratelimit-Remaining": {"4000"}}, refusal, "ghs_first", 0, false},
		{"a 403 with Retry-After to a token nobody registered", http.StatusForbidden, http.Header{"Retry-After": {"120"}, "X-Ratelimit-Remaining": {"4000"}}, secondary, "ghs_someone_else", 0, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			url := refusingServer(t, row.status, row.header, row.body)
			counter := NewCounter()
			counter.now = func() time.Time { return now }
			counter.Register("ghs_first", "101")
			client := &http.Client{Transport: counter.Transport(nil)}
			request, err := http.NewRequest(http.MethodGet, url+"/repos/acme/widgets/actions/workflows?per_page=1", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+row.token)
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || string(body) != row.body {
				t.Errorf("the caller read %q, %v; want GitHub's whole body %q", body, err, row.body)
			}
			until := counter.SecondaryUntil("101")
			if !row.noted {
				if !until.IsZero() || len(counter.secondary) != 0 {
					t.Errorf("SecondaryUntil = %s and the Counter noted %v, want nothing noted", until, counter.secondary)
				}
				return
			}
			if want := now.Add(row.want); !until.Equal(want) {
				t.Errorf("SecondaryUntil = %s, want %s, %s after the answer", until, want, row.want)
			}
			if other := counter.SecondaryUntil("202"); !other.IsZero() {
				t.Errorf("another installation's SecondaryUntil = %s, want none", other)
			}
		})
	}
}

// A later secondary limit that asks for less never shortens one already noted: the end noted is
// the latest any answer asked for.
func TestASecondaryLimitNotedIsNeverShortened(t *testing.T) {
	now := time.Unix(measuredReset, 0).UTC().Add(-30 * time.Minute)
	long := refusingServer(t, http.StatusForbidden, http.Header{"Retry-After": {"300"}, "X-Ratelimit-Remaining": {"4000"}}, `{}`)
	short := refusingServer(t, http.StatusForbidden, http.Header{"Retry-After": {"10"}, "X-Ratelimit-Remaining": {"4000"}}, `{}`)
	counter := NewCounter()
	counter.now = func() time.Time { return now }
	counter.Register("ghs_first", "101")
	client := &http.Client{Transport: counter.Transport(nil)}
	call(t, client, long+"/repos/acme/widgets", "Bearer ghs_first", "api")
	call(t, client, short+"/repos/acme/widgets", "Bearer ghs_first", "api")
	if until, want := counter.SecondaryUntil("101"), now.Add(5*time.Minute); !until.Equal(want) {
		t.Errorf("SecondaryUntil = %s, want the longer limit's end %s", until, want)
	}
}

// A line's error is held to ErrorLimit bytes, as a recorded GitHub read's cause is: a refusal
// carries GitHub's whole body, up to a megabyte, which a log store cuts or drops. It is cut at a
// whole character, so its head is a prefix of the error and no broken character is written, and
// ends with the length it was cut from; a shorter error is written whole. The two long errors put
// their two-byte characters at odd and at even offsets, so one of them would be cut inside a
// character wherever the limit falls.
func TestWriteHoldsTheErrorToItsLimit(t *testing.T) {
	for _, prefix := range []string{"GET /repos/acme/widgets: status 502: ", "GET /repos/acme/widgets: status 502:  "} {
		long := prefix + strings.Repeat("é", 1<<19)
		var out logtest.Buffer
		Write(slog.New(slog.NewJSONHandler(&out, nil)), Reading{Program: "dispatch", InstallationID: "101", Error: long})
		lines := logtest.JSONLines(t, out.String())
		if len(lines) != 1 {
			t.Fatalf("Write wrote %d lines, want 1", len(lines))
		}
		cut, _ := lines[0]["error"].(string)
		marker := " … (cut from " + strconv.Itoa(len(long)) + " bytes)"
		head, marked := strings.CutSuffix(cut, marker)
		if len(cut) > ErrorLimit || !marked || !strings.HasPrefix(long, head) || len(head) < ErrorLimit-len(marker)-1 {
			t.Errorf("the %d-byte error is written as %d bytes ending %q, want at most %d: a prefix of it cut at a whole character, then %q",
				len(long), len(cut), cut[max(0, len(cut)-40):], ErrorLimit, marker)
		}
		if !utf8.ValidString(cut) {
			t.Error("the cut error is not valid UTF-8")
		}
	}
	var out logtest.Buffer
	Write(slog.New(slog.NewJSONHandler(&out, nil)), Reading{Program: "dispatch", InstallationID: "101", Error: "short"})
	if lines := logtest.JSONLines(t, out.String()); len(lines) != 1 || lines[0]["error"] != "short" {
		t.Errorf("the short error is written as %v, want it whole", lines)
	}
}
