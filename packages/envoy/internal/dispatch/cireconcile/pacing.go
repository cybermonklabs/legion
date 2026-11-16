package cireconcile

import (
	"context"
	"time"
)

const (
	// RequestPace is the pause a running pass leaves between the requests it sends GitHub: two a
	// second, the pace the webhook redelivery sweep's own requests keep. A caller sets it on Pace;
	// a test sets Pace to zero and waits for nothing.
	RequestPace = 500 * time.Millisecond

	// FastestRequestPace is the shortest gap a pass will ever leave between its requests,
	// however much work it has to pace itself over: ten requests a second. GitHub's secondary
	// rate limits allow an App no more than 900 points a minute and a read of this kind costs
	// one point, so ten a second sits inside that limit with a third of it to spare for the
	// other readers the App pays for -- the listener's own reads, the webhook redelivery sweep,
	// every API call a person's click makes.
	FastestRequestPace = 100 * time.Millisecond
)

// gap is the pause this pass leaves between its REST requests: the REST work it expects to do,
// spread over the share of its own bound it may spend waiting.
//
// The work is what the last pass learned there is -- the pages of each listing of each head
// the queries could not answer whole, and one read of each mapped repository's identity -- so a
// pass paces itself by the size of the job rather than by a fixed pause, which is what makes a
// pass of 1,681 requests take 26 minutes of a 5-minute interval. Most of a pass is queries,
// which GitHub's own secondary limit on GraphQL time paces instead (graphQLPace), so the REST
// work left is the handful of heads one query cannot hold and the gap is usually the ceiling.
// The share is nine tenths of the bound, the tenth left over being the round trips themselves.
//
// The gap is clamped at both ends. FastestRequestPace is the floor whatever the arithmetic says,
// because the secondary limit it sits inside is the App's and this pass is not the only reader
// the App pays for; Pace is the ceiling, so the pass is never slower than the pause a deployment
// configures and the work only ever buys GitHub more room, never less.
func (r *Reconciler) gap(rows int) time.Duration {
	if r.Pace <= 0 {
		// A caller that waits for nothing, which is what every test of this package is.
		return 0
	}
	if r.PassTimeout <= 0 {
		// An unbounded pass has no budget to spread its requests over, so it keeps the pause its
		// deployment configured.
		return r.Pace
	}
	work := r.restWork()
	if r.known() == 0 && work == 0 {
		// The first pass of a process knows of no work yet, and the floor is the safe answer to
		// that: it is the fastest this pass is ever allowed to read, which is inside GitHub's
		// secondary limit however much work there turns out to be, and the pass after it knows
		// the real count and paces itself by it.
		return FastestRequestPace
	}
	expected := work + rows
	if expected == 0 {
		// A cycle whose heads the queries all answered whole leaves no REST work behind, while
		// the heads it listed are still counted, so the guard above does not fire; a mapping
		// that has since been emptied leaves no repository to read either, and there is nothing
		// at all for this pass to spread over its bound. The floor is the answer to no expected
		// work, as it is above, and the arithmetic below would divide by zero: nothing recovers
		// a panic of the pass's goroutine, so it would take the process down.
		return FastestRequestPace
	}
	return min(max(r.PassTimeout*9/10/time.Duration(expected), FastestRequestPace), r.Pace)
}

// restWork is the REST requests the last pass learned this one will make: every head a query
// could not answer whole is read again over REST, one request for each page of each of its two
// listings.
//
// A head a query answered whole is no REST work at all, and the Reconciler holds it with no
// page of either listing and a fingerprint instead (headState), so it adds nothing here. The
// pass that then cannot read such a head in a query reads it over REST with no ETag to send,
// which is two requests this did not expect: the gap is what the last pass learned, and a head
// that has just overflowed is news.
func (r *Reconciler) restWork() int {
	requests := 0
	for _, state := range r.heads {
		requests += len(state.runs) + len(state.suites)
	}
	return requests
}

// pace leaves the pause between REST requests GitHub asks a poller for, which is the gap this
// pass set itself (counts.pace). It waits before each REST request but the pass's first, so a
// pass of one such request takes no longer than that request. A query is paced by GitHub's
// GraphQL budget instead and is no part of this count (graphQLPace).
func (r *Reconciler) pace(ctx context.Context, c *counts) {
	if c.pace <= 0 || c.restRequests == 0 {
		return
	}
	timer := time.NewTimer(c.pace)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
