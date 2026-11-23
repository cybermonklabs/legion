package main

import (
	"slices"
	"strings"
	"sync"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
)

// recipientCounts is how many sessions, other than sender, a message published on topic is for,
// as listenerwire.PublishResponse carries the two numbers: recipients is every session the topic
// is for and connected how many of those are live now, a pointer each so that a count of zero is
// sent as the number it is. A count is nil when a cache it is read from cannot be served, and the
// publish route leaves each nil field out of its answer on its own, rather than send a number that
// means something narrower than the one it names (plan D16). The counts read three caches, and
// each is checked before it is read:
//
//   - the known-session store (session.KnownSessions.Opened: no store at all, one whose NATS grant
//     does not open the bucket yet, one whose first read of the bucket has not completed, or one
//     that stopped following it for longer than the stale bound) gates both counts;
//   - the interest copy (store.Registry.Servable: its first read of the bucket has not completed,
//     or it stopped following the bucket for longer than the stale bound, as after a reconnect
//     whose read has not completed in time) gates both counts too. recipients is the union of
//     these two stores, so with either one incomplete or stale it would count part of them, and
//     connected, a subset of recipients, can be no more right than it;
//   - the session registry's copy (session.SessionRegistry.Servable, by the same rule) gates
//     connected alone: recipients is still the whole union, and is sent without connected.
//
// recipients is the union of two stores, deduped by session id: the known-session records whose
// topics match the topic (session.KnownSessions.MatchEach), one per session that registered in
// the last 30 days, and the interest rows whose topics match it on any machine
// (store.Registry.MatchAllMachines). A session that registered through a listener of a release
// older than those records has a row and no record, and it is a recipient like any other, which
// is why the rows are in the union too. A session either store names through a topic its consumer
// filter leaves out by the fourth or fifth kind alone, which a row or a record an earlier build
// stored can hold until the session's next registration takes it out of both, is not counted: its
// consumer delivers nothing on that topic (carriedFor).
//
// connected is how many of those sessions the session registry holds a live entry for, by the
// rule every reader of a session's age reads (session.SessionRegistry.Lookup, session.StateOf): an
// unresponsive session is not counted, since a sender reading "running now" acts on it. It is the
// number a reader acts on: against 30 days of records a team topic almost never counts zero, so
// recipients alone would never again tell a sender that nobody is there to read its message.
//
// Every read here is of memory: each check reads its watcher's state alone, both matchers answer
// from their caches' subject indexes and the session registry from its cache, so counting makes
// no JetStream API call and the publish it precedes waits on no round trip of its own.
func recipientCounts(d *listenerDeps, topic, sender string) (recipients, connected *int) {
	if !d.known.Opened() || d.registry.Servable() != nil {
		return nil, nil
	}
	// connected stays nil, and no entry is looked up, while the session copy cannot be served.
	if d.sessions.Servable() == nil {
		connected = new(int)
	}
	counted := map[string]struct{}{}
	count := func(sessionID string) {
		if sessionID == sender {
			return
		}
		if _, dup := counted[sessionID]; dup {
			return
		}
		counted[sessionID] = struct{}{}
		if connected == nil {
			return
		}
		if _, state := d.sessions.Lookup(sessionID); state == session.Live {
			*connected++
		}
	}
	d.known.MatchEach(topic, func(record session.KnownSession) {
		if carriedFor(record.SessionID, record.Topics, topic) {
			count(record.SessionID)
		}
	})
	for _, interest := range d.registry.MatchAllMachines(topic) {
		if carriedFor(interest.SessionID, interest.Topics, topic) {
			count(interest.SessionID)
		}
	}
	total := len(counted)
	return &total, connected
}

// carriedFor reports whether a publish on subject, which some topic of topics matches, is one a
// session following topics is a recipient of: a topic of topics that matches subject, in one of
// its forms (contracts.TopicForms, as the stores' matchers read it), is not one the session's
// consumer filter leaves out by its fourth or fifth kind (sessionFilter). A registration of this
// build keeps such a topic out of the row and the record it writes (planWithoutLeftOut), but a row
// or a record an earlier build stored can hold one until the session's next registration, and its
// consumer delivers nothing on it.
//
// A set that can hold no such topic is judged without building the filter (mayLeaveOut), so a
// publish pays for the reduction only for a session whose topics could have fed one.
func carriedFor(sessionID string, topics []string, subject string) bool {
	return !mayLeaveOut(topics) || filterCarries(sessionID, topics, subject)
}

// mayLeaveOut reports whether the session's consumer filter can leave out any of topics by its
// fourth or fifth kind, false only where it cannot: the fourth kind is a topic that does not begin
// with bus.NotificationTopicPrefix, and a merge of the fifth takes two wildcard topics of the set.
// A literal merges with nothing as it is spelled; the only wildcard forms of a literal lie under
// the agent lanes, which a wildcard topic of the set overlaps only by reaching another session's
// private subjects, the third kind, or by containing that form; and the two forms of one topic
// never overlap. TestTheRecipientCountsShortcutLeavesOutNothingTheFullFilterWould holds the claim
// to the full filter over every set of up to three topics of a pool that holds each of those cases.
func mayLeaveOut(topics []string) bool {
	wildcards := 0
	for _, topic := range topics {
		if hasWildcardToken(topic) {
			wildcards++
		}
		if !strings.HasPrefix(topic, bus.NotificationTopicPrefix) || wildcards >= 2 {
			return true
		}
	}
	return false
}

// filterCarries is carriedFor judged on the session's consumer filter itself, with no shortcut: a
// topic of topics that matches subject, in one of its forms, is not one sessionFilter leaves out
// (filterLeftOuts, which keeps that reduction's answer).
func filterCarries(sessionID string, topics []string, subject string) bool {
	leftOut := filterLeftOuts.of(sessionID, topics)
	for _, topic := range topics {
		if slices.ContainsFunc(leftOut, func(left filterLeftOut) bool { return left.topic == topic }) {
			continue
		}
		if slices.ContainsFunc(contracts.TopicForms(topic), func(form string) bool { return bus.SubjectContains(form, subject) }) {
			return true
		}
	}
	return false
}

// leftOutCacheSize bounds leftOutCache: past it, an insert drops arbitrary entries until half the
// bound is left. One entry is one session's row as a matcher read it, so the bound holds the rows of
// that many sessions with a wildcard pair or a topic outside notifications. at once, far more than
// a listener's sessions; a row dropped is reduced again at its next message, which is the cost the
// cache saves, not a wrong answer.
const leftOutCacheSize = 4096

// leftOutCache keeps, for each session and topic list, the topics the session's consumer filter
// leaves out by its fourth or fifth kind (sessionFilter), so the durable path and the publish count,
// which ask it for each message and each matching row that may leave one out (mayLeaveOut), reduce
// a row once rather than once a message. sessionFilter's answer depends on the session id and the
// topic list alone, so the key is both: a write that changes the row's topics is a new key, which
// is reduced afresh, and a write that leaves them as they were keeps the answer, which is still
// true. A row read in another order is another key, reduced to the same answer.
type leftOutCache struct {
	mu      sync.Mutex
	entries map[string][]filterLeftOut
	// reduced counts the reductions the cache ran, which a test reads to tell a hit from a miss.
	reduced int
}

// of is the topics of topics sessionID's consumer filter leaves out by its fourth or fifth kind, as
// sessionFilter answers it, from the cache when it holds the session and the list. The slice it
// returns is shared: callers read it and do not change it.
func (c *leftOutCache) of(sessionID string, topics []string) []filterLeftOut {
	key := sessionID + "\x00" + strings.Join(topics, "\x00")
	c.mu.Lock()
	leftOut, ok := c.entries[key]
	c.mu.Unlock()
	if ok {
		return leftOut
	}
	_, leftOut = sessionFilter(sessionID, topics)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reduced++
	if c.entries == nil {
		c.entries = make(map[string][]filterLeftOut)
	}
	if len(c.entries) >= leftOutCacheSize {
		for dropped := range c.entries {
			delete(c.entries, dropped)
			if len(c.entries) <= leftOutCacheSize/2 {
				break
			}
		}
	}
	c.entries[key] = leftOut
	return leftOut
}

// isolate gives the cache a memory of its own, empty, with its count of reductions at zero, and
// returns the func that puts back what it had. A test isolates the cache it counts, so no entry
// another test stored answers for it.
func (c *leftOutCache) isolate() (restore func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entries, reduced := c.entries, c.reduced
	c.entries, c.reduced = nil, 0
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.entries, c.reduced = entries, reduced
	}
}

// reductions is how many reductions the cache ran since it was isolated.
func (c *leftOutCache) reductions() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reduced
}

// filterLeftOuts is the one cache of this process: the publish count (carriedFor) and the durable
// path (skipLeftOutInterest) share it, as they share the rows they read.
var filterLeftOuts leftOutCache
