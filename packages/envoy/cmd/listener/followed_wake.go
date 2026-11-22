// The followed-topic wake: a message on a topic a released session follows brings its pod back.

package main

import (
	"strconv"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// followedWakeInterval is the interval a followed-topic wake report is named by (followedWakeID):
// every message on one topic for one session inside one interval raises the one report, which
// every listener names the same way and the stream, which keeps a message id for 72 hours, stores
// once. A burst of events on one pull request is one report a topic, and a pod one of them woke
// is on its way back already: the daemon resumes a role once while its resume is open. It is the
// role lane sweep's interval (roleWakeInterval), the other wake every listener raises alike.
const followedWakeInterval = roleWakeInterval

// followedWakeID is the message id and the dedupe key of the followed-topic wake report of
// sessionID for a message on topic at now:
// legion.wake.followed.<session>.<topic>.<the start of the followedWakeInterval now falls in, in
// Unix seconds>.
func followedWakeID(sessionID, topic string, now time.Time) string {
	return "legion.wake.followed." + sessionID + "." + topic + "." +
		strconv.FormatInt(now.Truncate(followedWakeInterval).Unix(), 10)
}

// reportFollowedWakes raises a followed-topic wake (wakeKindFollowed) for each session that is gone
// and follows item's topic: each of owned, the rows of this listener's machine that match it
// (fanoutDelivery's Match), whose session the session registry holds no live or unresponsive
// record of (session.Gone: its clean stop deleted the record, or the gone horizon passed), and
// each stopped row that matches it (store.Registry.MatchStopped), which every listener reads alike
// and names alike. The daemon resumes the released claim of that session, and ignores a report
// naming a session no claim of its own is released under.
//
// No private stream subject wakes a session this way (bus.ReachesPrivateStreamSubject, judged with
// no role): another session's inbox, a failed-delivery lane, a controller's notice lane, and this
// very wake subject, which a row holding a wildcard an earlier build stored could match. A
// session's own inbox has a wake of its own (wakeKindAgent), and a stopped row keeps none of these
// (store's keptTopics). Nor does a message the session published itself.
func reportFollowedWakes(cfg listenerDeliveryHandlerConfig, item contracts.Envelope, owned []store.Interest) {
	if cfg.wakes == nil {
		return
	}
	wake := func(sessionID string) {
		if item.SourceSession == sessionID || bus.ReachesPrivateStreamSubject(sessionID, item.Topic, nil) {
			return
		}
		cfg.wakes.reportKeyed(cfg.logger, wakeReport{Kind: wakeKindFollowed, Session: sessionID, Topic: item.Topic},
			followedWakeID(sessionID, item.Topic, cfg.wakes.now()))
	}
	for _, interest := range owned {
		if _, state := cfg.sessions.Lookup(interest.SessionID); state == session.Gone {
			wake(interest.SessionID)
		}
	}
	for _, stopped := range cfg.registry.MatchStopped(item.Topic) {
		wake(stopped.SessionID)
	}
}
