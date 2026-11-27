package main

import (
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/store"
)

// The publish count (carriedFor) and the durable path (skipLeftOutInterest) ask, for each message
// and each matching row that may leave a topic out, which topics the session's consumer filter
// leaves out, and the answer is a whole reduction of the row (sessionFilter). The cache
// (filterLeftOuts) keeps it per session and topic list: a row asked again, by either, is not
// reduced again, and a write that changes the row's topics is reduced afresh, so the answer follows
// the row and never a list it no longer holds.
//
// The row holds the exception-lane pair the filter's reduction merges into every session's failed
// deliveries, so a message on notifications.envoy.exceptions.foo.bar reaches the session through
// no topic its consumer carries. The row's next write drops the pair's second topic and adds a
// wildcard that merges with nothing, so the same message reaches it through the first, now carried:
// a cache that kept the first row's answer for the session would still call it left out.
func TestALeftOutSetIsReducedOncePerSessionAndRow(t *testing.T) {
	// Not parallel: it counts the reductions of filterLeftOuts, the one cache that every publish
	// count and durable read of the process shares (leftOutCache.isolate, reductions).
	const (
		session = "ses_left_out_cache"
		pairFoo = "notifications.envoy.exceptions.foo.*"
		pairBar = "notifications.envoy.exceptions.*.bar"
		github  = "notifications.envoy.exceptions.notifications.github.>"
		subject = "notifications.envoy.exceptions.foo.bar"
	)
	t.Cleanup(filterLeftOuts.isolate())
	agent := contracts.LegacyAgentSubject(session)
	row := []string{agent, pairBar, pairFoo, "notifications.team.kept"}
	written := []string{agent, github, pairFoo, "notifications.team.kept"}
	for _, topics := range [][]string{row, written} {
		if !mayLeaveOut(topics) {
			t.Fatalf("the shortcut passes over %v, so no reduction is asked of it", topics)
		}
	}

	for i := range 3 {
		if carriedFor(session, row, subject) {
			t.Fatalf("publish %d on %s counts the session following %v, whose filter leaves out the pair", i+1, subject, row)
		}
	}
	if !skipLeftOutInterest(nil, store.Interest{SessionID: session, Topics: row}, subject) {
		t.Fatalf("the durable path takes %s for the session following %v", subject, row)
	}
	if got := filterLeftOuts.reductions(); got != 1 {
		t.Fatalf("three publishes and a durable read of one row ran %d reductions, want one", got)
	}

	if !carriedFor(session, written, subject) {
		t.Errorf("a publish on %s after the row's write counts no session following %v, whose filter carries %s", subject, written, pairFoo)
	}
	if got := filterLeftOuts.reductions(); got != 2 {
		t.Errorf("the row's new topics ran %d reductions in all, want a second one for them", got)
	}
	if carriedFor(session, row, subject) {
		t.Errorf("the first row, asked again, counts the session")
	}
	if got := filterLeftOuts.reductions(); got != 2 {
		t.Errorf("the first row asked again ran %d reductions in all, want none more", got)
	}
}
