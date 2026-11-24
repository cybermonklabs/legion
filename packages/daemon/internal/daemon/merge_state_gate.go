package daemon

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
)

// queryGate is one installation's GraphQL queries, as every base's batch makes them with that
// installation's token (workflowRuntime.queryGate, keyed by the implement App's scope for the
// repository's owner, implementScope, which names the installation before any token is minted):
// GitHub budgets the installation, not the base, so its rate limit and the time its queries take
// are shared by every base of every repository of that owner. turn lets one query run at a time;
// share is the share of the time this process's queries may keep GitHub at work (the daemon's
// `github_graphql_share`, config.Config.GitHubGraphQLShare); hold is the moment GitHub's rate
// limit asked the installation's queries to wait until, held the length of that hold, which a
// limit met again soon after it doubles and a query GitHub answers without a limit sets back to
// zero; and next the moment the last query's pace lets the next one go (settle). It coordinates
// the batches of this process alone: another daemon process reading the same installation keeps
// a gate of its own, which is what share is for.
type queryGate struct {
	turn  chan struct{}
	share float64
	mu    sync.Mutex
	hold  time.Time
	held  time.Duration
	next  time.Time
}

// newQueryGate is a gate whose queries keep GitHub at work share of the time.
func newQueryGate(share float64) *queryGate {
	return &queryGate{turn: make(chan struct{}, 1), share: share}
}

// queryGate is the gate of the installation scope names, made the first time it is asked for.
func (w *workflowRuntime) queryGate(scope string) *queryGate {
	w.gateMu.Lock()
	defer w.gateMu.Unlock()
	if w.gates == nil {
		w.gates = map[string]*queryGate{}
	}
	gate := w.gates[scope]
	if gate == nil {
		gate = newQueryGate(w.queryShare())
		w.gates[scope] = gate
	}
	return gate
}

// queryShare is the share of the time this daemon's queries of one installation may keep GitHub
// at work, as the configuration set it (workflowRuntime.graphQLShare, which the loader already
// holds at least config.MinGitHubGraphQLShare and at most 1: readShare), or its default when it is
// zero, as it is for a runtime built with none, a test's.
func (w *workflowRuntime) queryShare() float64 {
	if w.graphQLShare > 0 {
		return w.graphQLShare
	}
	return config.DefaultGitHubGraphQLShare
}

// queryHoldMost is the longest a limit met again soon after the last one holds the installation's
// queries once the gate has doubled its hold (settle), unless GitHub itself asks for longer, and
// the longest pause after one query (pause).
const queryHoldMost = 15 * time.Minute

// run makes query once its turn comes and the gate's hold and pace have passed, and answers its
// error. The gate times the query, paces the next one by that time and reads a rate limit from the
// error (settle). A ctx that ends while it waits ends the wait, and one that has ended once the
// turn and the wait are won makes no query: either can be won as ctx ends, since a select takes
// any case that is ready.
func (g *queryGate) run(ctx context.Context, query func() error) error {
	select {
	case g.turn <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-g.turn }()
	g.mu.Lock()
	until := g.hold
	if g.next.After(until) {
		until = g.next
	}
	g.mu.Unlock()
	if wait := time.Until(until); wait > 0 {
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	start := time.Now()
	err := query()
	took := time.Since(start)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.settle(err, took, time.Now())
	return err
}

// settle records a query that took took and ended at now with err, under mu. The next query waits
// (1 - share) / share of took (pause), so this process's queries keep GitHub at work share of any
// stretch of time, inside the limit GitHub documents on query processing time (60 s of it in 60
// s, which every daemon reading with the installation shares). A rate limit, GraphQL's own or the
// secondary limit (githubrest.RetryAfter), holds every query of the installation as long as GitHub
// asks; one met again less than the last hold's length after that hold ended - the limit came back
// as soon as the queries did - holds twice the longer of GitHub's wait and the last hold, up to
// queryHoldMost, as GitHub asks of a client its secondary limit keeps meeting, and never less than
// GitHub asks. A query GitHub answered without a limit - its data, or any other answer of its own
// (githubrest.Answered) - ends the backoff; one that got no answer from GitHub at all - a transport
// failure, its own deadline, or a token that could not be minted for it - leaves the backoff as it
// stands, since it says nothing of whether GitHub still limits the installation.
func (g *queryGate) settle(err error, took time.Duration, now time.Time) {
	g.next = now.Add(g.pause(took))
	after, _ := githubrest.RetryAfter(err)
	if after <= 0 {
		if githubrest.Answered(err) {
			g.held = 0
		}
		return
	}
	hold := after
	if g.held > 0 && now.Sub(g.hold) < g.held {
		hold = max(after, min(2*max(after, g.held), queryHoldMost))
	}
	g.held = hold
	if until := now.Add(hold); until.After(g.hold) {
		g.hold = until
	}
}

// pause is how long the gate lets no query go after one that took took: (1 - share) / share of
// it, the same again at the default share of one half, three times it at a quarter, nothing at a
// share of 1, and at most queryHoldMost. The product is checked before it becomes a
// time.Duration: one at or past queryHoldMost, an infinity among them, is that cap, so a share
// small enough to ask for more than a time.Duration holds never wraps round to a pause below
// nothing, and one that is not a positive number (NaN, of a share of 0 after a query of no time)
// is no pause.
func (g *queryGate) pause(took time.Duration) time.Duration {
	wait := float64(took) * (1 - g.share) / g.share
	switch {
	case math.IsNaN(wait) || wait <= 0:
		return 0
	case wait >= float64(queryHoldMost):
		return queryHoldMost
	}
	return time.Duration(wait)
}
