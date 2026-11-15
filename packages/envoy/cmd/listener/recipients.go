package main

import (
	"github.com/sjawhar/envoy/internal/session"
)

// recipientCounts is how many sessions, other than sender, a message published on topic is for,
// as publishResponse carries the two numbers: recipients is every session the topic is for and
// connected how many of those are live now, a pointer each so that a count of zero is sent as the
// number it is. A count is nil when a cache it is read from cannot be served, and the publish
// route leaves each nil field out of its answer on its own, rather than send a number that means
// something narrower than the one it names (plan D16). The counts read three caches, and each is
// checked before it is read:
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
// is why the rows are in the union too.
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
	d.known.MatchEach(topic, func(record session.KnownSession) { count(record.SessionID) })
	for _, interest := range d.registry.MatchAllMachines(topic) {
		count(interest.SessionID)
	}
	total := len(counted)
	return &total, connected
}
