package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
)

// A rate limit met again soon after the installation's last hold ended - here 10 s after it, as
// the queries resume - holds twice as long as the last hold, up to queryHoldMost, as GitHub asks of
// a client its secondary limit keeps meeting, and never less than GitHub asks. A query GitHub
// answers with no limit ends the backoff, and so does a limit that comes long after the hold.
func TestARepeatedLimitDoublesTheInstallationsHoldUpToItsCap(t *testing.T) {
	t.Parallel()
	gate := newQueryGate(config.DefaultGitHubGraphQLShare)
	limit := func(after time.Duration) error {
		return &githubrest.Answer{Method: http.MethodPost, Path: "/graphql", Status: http.StatusForbidden, RateLimited: true, RetryAfter: after, Budget: githubrest.BudgetSecondary}
	}
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	held := func(step string, err error, want time.Duration) {
		t.Helper()
		gate.settle(err, 0, now)
		if got := gate.hold.Sub(now); got != want || gate.held != want {
			t.Fatalf("%s: the installation is held %s (its backoff %s), want %s", step, got, gate.held, want)
		}
	}

	held("the first limit", limit(time.Minute), time.Minute)
	for _, want := range []time.Duration{2 * time.Minute, 4 * time.Minute, 8 * time.Minute, queryHoldMost, queryHoldMost} {
		now = gate.hold.Add(10 * time.Second)
		held("a limit 10 s after the hold ended", limit(time.Minute), want)
	}
	now = gate.hold.Add(10 * time.Second)
	held("GitHub asking for longer than the cap", limit(30*time.Minute), 30*time.Minute)

	now = gate.hold.Add(time.Second)
	gate.settle(nil, 0, now)
	if gate.held != 0 {
		t.Fatalf("after a query GitHub answered with no limit the backoff is %s, want none", gate.held)
	}
	now = now.Add(time.Second)
	held("a limit after a query that was not refused", limit(time.Minute), time.Minute)
	now = gate.hold.Add(2 * time.Minute)
	held("a limit longer after the hold than the hold lasted", limit(time.Minute), time.Minute)
}

// A query that reached no answer of GitHub's - a connection that failed, its own deadline, a token
// that could not be minted for it, whose error queryChunk hands the gate as the mint returned it -
// says nothing of whether GitHub still limits the installation, so it leaves the backoff as it
// stands: a limit, then such a failure, then a limit within one hold-length of the hold's end
// doubles the hold. An answer of GitHub's own that is no limit - a 502, GraphQL errors - ends the
// backoff as an answer with the data does.
func TestATransportFailureKeepsTheBackoff(t *testing.T) {
	t.Parallel()
	limit := &githubrest.Answer{Method: http.MethodPost, Path: "/graphql", Status: http.StatusForbidden, RateLimited: true, RetryAfter: time.Minute, Budget: githubrest.BudgetSecondary}
	start := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	for _, outcome := range []struct {
		name    string
		err     error
		answers bool
	}{
		{"a connection that failed", &url.Error{Op: "Post", URL: "https://api.github.com/graphql", Err: errors.New("connection reset by peer")}, false},
		{"the query's own deadline", &url.Error{Op: "Post", URL: "https://api.github.com/graphql", Err: context.DeadlineExceeded}, false},
		{"a token that could not be minted", mintTimedOut(appauth.Implement, "acme"), false},
		{"a 502 of GitHub's", &githubrest.Answer{Method: http.MethodPost, Path: "/graphql", Status: http.StatusBadGateway, Body: `{"message":"Server Error"}`}, true},
		{"GraphQL errors", githubrest.GraphQLErrors{{Type: "NOT_FOUND", Message: "Could not resolve to a PullRequest with the number of 87."}}, true},
	} {
		gate := newQueryGate(config.DefaultGitHubGraphQLShare)
		now := start
		gate.settle(limit, 0, now)
		now = gate.hold.Add(10 * time.Second)
		gate.settle(outcome.err, 0, now)
		want, again := time.Minute, 2*time.Minute
		if outcome.answers {
			want, again = 0, time.Minute
		}
		if gate.held != want {
			t.Fatalf("after a limit and then %s the backoff is %s, want %s", outcome.name, gate.held, want)
		}
		now = now.Add(time.Second)
		gate.settle(limit, 0, now)
		if got := gate.hold.Sub(now); got != again {
			t.Fatalf("a limit 11 s after the hold ended, with %s between, holds %s, want %s", outcome.name, got, again)
		}
	}
}

// The pause after each query is (1 - share) / share of the time it took: the same again at the
// default share of one half, three times it at the share of a quarter a deployment of four daemons
// sets, nothing at 1, and at most queryHoldMost. At the floor share, 0.01, a long query is capped;
// at a share far below it, which the loader refuses, whose pause a time.Duration could not hold,
// the pause is still the cap, never a wrapped value below nothing. A runtime takes the share the
// configuration names and the default when it names none.
func TestAShareOfAQuarterWaitsThreeTimesTheQuerysTime(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		share      float64
		took, want time.Duration
	}{
		{0.5, 40 * time.Millisecond, 40 * time.Millisecond},
		{0.25, 40 * time.Millisecond, 120 * time.Millisecond},
		{1, 40 * time.Millisecond, 0},
		{config.MinGitHubGraphQLShare, 5 * time.Second, 495 * time.Second},
		{config.MinGitHubGraphQLShare, 10 * time.Second, queryHoldMost},
		{config.MinGitHubGraphQLShare, time.Hour, queryHoldMost},
		{1e-12, 40 * time.Millisecond, queryHoldMost},
		{1e-300, time.Hour, queryHoldMost},
	} {
		got := newQueryGate(tc.share).pause(tc.took)
		if got < 0 || got > queryHoldMost || (got-tc.want).Abs() > time.Millisecond {
			t.Errorf("the pause after a query of %s at a share of %v = %s, want %s", tc.took, tc.share, got, tc.want)
		}
	}

	gate := newQueryGate(0.25)
	ctx := context.Background()
	// The query times itself, from its first statement to its last: the gate's own timing, which
	// it paces by, begins before the first and ends after the last, so it is no shorter.
	var end, start time.Time
	var took time.Duration
	err := gate.run(ctx, func() error {
		began := time.Now()
		time.Sleep(40 * time.Millisecond)
		end = time.Now()
		took = end.Sub(began)
		return nil
	})
	if err != nil {
		t.Fatalf("the first query: %v", err)
	}
	if err := gate.run(ctx, func() error { start = time.Now(); return nil }); err != nil {
		t.Fatalf("the second query: %v", err)
	}
	if gap := start.Sub(end); gap < 3*took || gap > 3*took+time.Second {
		t.Fatalf("the second query began %s after the first ended, which took %s: want three times that at a share of a quarter", gap, took)
	}

	w := readRuntime(nil, "", quietLogger())
	if got := w.queryGate("implement write acme").share; got != config.DefaultGitHubGraphQLShare {
		t.Fatalf("a runtime with no share configured gates its queries at %v, want the default %v", got, config.DefaultGitHubGraphQLShare)
	}
	w = readRuntime(nil, "", quietLogger())
	w.graphQLShare = 0.25
	if got := w.queryGate("implement write acme").share; got != 0.25 {
		t.Fatalf("a runtime configured with a share of 0.25 gates its queries at %v, want 0.25", got)
	}
}

// A ctx that has ended makes no query, even when the gate's turn is free and nothing holds it.
func TestAnEndedContextMakesNoQuery(t *testing.T) {
	t.Parallel()
	gate := newQueryGate(config.DefaultGitHubGraphQLShare)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 100 {
		queried := false
		if err := gate.run(ctx, func() error { queried = true; return nil }); queried || !errors.Is(err, context.Canceled) {
			t.Fatalf("with its ctx ended the gate queried %t and answered %v, want no query and the ctx's end", queried, err)
		}
	}
}
