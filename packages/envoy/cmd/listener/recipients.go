package main

import (
	"context"
	"slices"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// recipientLaneReadTimeout bounds the one bucket read a count can make, of the claim record of the
// controller role whose notice lane a publish names (recipientCounts), so a role bucket that does
// not answer leaves both counts out with most of sendRequestBudget still left to the sender stamp
// and the publish.
const recipientLaneReadTimeout = 500 * time.Millisecond

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
// is why the rows are in the union too.
//
// A session is counted only through a topic the consumer filter built from its stored row holds,
// by the one judgment every such filter is built with (storedTopicFiltered, which storedRowFilter
// applies): either store can name a topic that filter drops, and its consumer delivers nothing on
// it. A row an earlier build stored can name another session's inbox, a failed-delivery lane, a
// wildcard reaching one, or a topic the filter cannot take (one the deployed stream does not store,
// or the server refuses as a filter subject), and a row or a record can name the notice lane of a
// controller role the session no longer holds: a record keeps the lane of a role taken from its
// session for the record's 30 days when that session had stopped, since nothing rewrites a stopped
// session's record. A session the judgment leaves with no topic matching the message is no
// recipient of it, and a publish no session follows that way answers recipients 0, as any stored
// topic no session follows does.
//
// connected is how many of those sessions the session registry holds a live entry for, by the
// rule every reader of a session's age reads (session.SessionRegistry.Lookup, session.StateOf): an
// unresponsive session is not counted, since a sender reading "running now" acts on it. It is the
// number a reader acts on: against 30 days of records a team topic almost never counts zero, so
// recipients alone would never again tell a sender that nobody is there to read its message.
//
// Every read here is of memory but one: each check reads its watcher's state alone, both matchers
// answer from their caches' subject indexes and the session registry from its cache, so counting
// the publish of a topic that is no controller notice lane makes no JetStream API call and the
// publish it precedes waits on no round trip of its own. A publish on a controller notice lane
// reads that lane's role claim from the role bucket stream's leader, once, within
// recipientLaneReadTimeout of ctx (store.Registry.ReadLaneHolders), the read a filter built from a
// stored row makes for it; a read that did not answer or failed leaves both counts out, as a cache
// that cannot be served does.
func recipientCounts(ctx context.Context, d *listenerDeps, topic, sender string) (recipients, connected *int) {
	if !d.known.Opened() || d.registry.Servable() != nil {
		return nil, nil
	}
	judged := recipientJudgment{subject: topic}
	if d.consumers != nil {
		judged.stream = d.consumers.stream
	}
	// A lane is a literal, so the lane the message is published on is the one lane whose holder
	// can decide whether a topic matching it is kept; any other lane matches nothing here.
	if _, lane := bus.ControllerNoticeRole(topic); lane {
		readCtx, cancel := context.WithTimeout(ctx, recipientLaneReadTimeout)
		holders, err := d.registry.ReadLaneHolders(readCtx, []string{topic})
		cancel()
		if err != nil {
			return nil, nil
		}
		judged.holders = holders
	}
	judged.literal = !bus.OverlapsPrivateStreamSubject(topic) && filterTakes(judged.stream, topic)
	// connected stays nil, and no entry is looked up, while the session copy cannot be served.
	if d.sessions.Servable() == nil {
		connected = new(int)
	}
	counted := map[string]struct{}{}
	count := func(sessionID string, topics []string) {
		if sessionID == sender {
			return
		}
		if _, dup := counted[sessionID]; dup || !judged.keeps(sessionID, topics) {
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
	d.known.MatchEach(topic, func(record session.KnownSession) { count(record.SessionID, record.Topics) })
	for _, interest := range d.registry.MatchAllMachines(topic) {
		count(interest.SessionID, interest.Topics)
	}
	total := len(counted)
	return &total, connected
}

// recipientJudgment is what recipientCounts judges each matched session's topics by: the stream
// client the session consumers' filters are judged on (sessionDelivery.stream), the holders of the
// lane subject is published on (store.Registry.ReadLaneHolders), and whether a session naming
// subject itself follows it (literal).
type recipientJudgment struct {
	stream  *bus.Client
	holders store.LaneHolders
	subject string
	// literal says subject reaches no private stream subject, so every row keeps it, and a
	// consumer filter takes it: storedTopicFiltered holds subject for every session.
	literal bool
}

// keeps reports whether some topic of topics, a session's stored row or known-session record that
// a store's matcher visited for subject, matches subject in one of its forms
// (contracts.TopicForms), as the stores' matchers read a topic, and is one the filter built from
// the session's stored row holds (storedTopicFiltered). A set naming subject itself is answered
// first, with no topic split, where storedTopicFiltered holds subject for every session (literal):
// that is the set a team topic's publish matches.
func (j recipientJudgment) keeps(sessionID string, topics []string) bool {
	if j.literal && slices.Contains(topics, j.subject) {
		return true
	}
	return slices.ContainsFunc(topics, func(topic string) bool {
		return slices.ContainsFunc(contracts.TopicForms(topic), func(form string) bool { return bus.SubjectContains(form, j.subject) }) &&
			storedTopicFiltered(j.stream, j.holders, sessionID, topic)
	})
}
