package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// Canonical policy for the listener's durable consumer. DeliverSubject is
// deliberately not part of the policy: it is fixed at creation and preserved
// for the consumer's lifetime, and the lease holder subscribes to whatever
// subject the server has persisted (durableReceiver).
//
// consumerAckWait is short because the holder keeps every message it holds alive: it sends
// InProgress every inProgressEvery for each message it has received and not yet settled, queued
// or in hand, so a message waits out the ack wait only once the pod holding it has died or lost
// the lease, and then returns to the next holder within seconds.
//
// consumerInactiveThreshold lets the server delete a machine's durable
// consumer once its listener has been gone this long. The notification
// stream only retains 72h of messages, so a consumer inactive longer than
// that has already lost data and its durability protects nothing; without
// a threshold, every decommissioned machine leaves a consumer behind
// forever, silently accumulating pending state.
const (
	consumerAckWait           = 6 * time.Second
	consumerMaxAckPending     = 256
	consumerMaxDeliver        = 20
	consumerInactiveThreshold = 7 * 24 * time.Hour
)

// applyListenerConsumerPolicy stamps the canonical consumer policy onto
// config. Shared by the create and drift-correction paths so the policy has
// exactly one definition: the drift correction applies it to a copy of the
// durable's config and updates the durable when the copy differs. The policy
// filters on the full wildcard (consumerFilterAll), so the durable takes every
// message the stream stores and no build's own list of stream subjects narrows
// it: two builds whose stream subjects differ share one durable during a roll.
// The policy has no idle heartbeat: the create path sets none, so a durable the
// listener creates has none. listenerDurableRefusal refuses an existing durable
// whose heartbeat or ack policy differs, since NATS cannot change either in
// place. The policy sets no deliver policy, start sequence or start time: the
// create path sets deliver-new itself (correctListenerDurable), and NATS refuses
// a change of deliver policy in place, so the drift correction leaves an
// existing durable's start as it is and the durable keeps its cursor.
func applyListenerConsumerPolicy(config *nats.ConsumerConfig) {
	config.FilterSubject = consumerFilterAll
	config.FilterSubjects = nil
	config.AckPolicy = nats.AckExplicitPolicy
	config.AckWait = consumerAckWait
	config.MaxAckPending = consumerMaxAckPending
	config.MaxDeliver = consumerMaxDeliver
	config.InactiveThreshold = consumerInactiveThreshold
}

// consumerFilterAll is the durable's filter: every subject, which is every message the stream
// stores. It is a filter rather than none because nats-server 2.10 (2.10.29 measured, the version
// this module built its test server from until v2.15.1) accepts an update that clears a push
// durable's filter and then delivers nothing published after it, and every durable an earlier
// listener created carries a filter (its build's stream subjects); an update to the full wildcard
// delivers everything. The durable keeps a filter whatever the server runs: the full wildcard
// costs nothing on a server that takes an empty one, and one durable is shared by pods of
// different builds.
const consumerFilterAll = ">"

// errListenerDurableRefused marks a durable the listener will not receive from however often it is
// asked: no retry can succeed, so the listener exits.
var errListenerDurableRefused = errors.New("listener durable refused")

// durableRefusedLine is the line a listener logs when it refuses its durable, at the check after
// the NATS connect and when the lease holder reads the durable. The deployed durable-refusal metric
// filter matches it exactly (defined in the deployment repository's listener infrastructure), so
// both sites use this constant.
const durableRefusedLine = "subscribe refused, shutting down"

// listenerDurableRefusal refuses an existing durable carrying a setting the listener's consumer
// policy fixes and NATS cannot change in place, so the drift correction could never apply it:
//   - An idle heartbeat. The bus logs nats.ErrConsumerNotActive at WARN because only KV watchers'
//     ordered consumers report it, and only while disconnected; a heartbeat here would make a
//     stalled durable report that same WARN.
//   - An ack policy other than explicit. The delivery handler acks each message or NAKs it for a
//     delayed retry, one at a time; under ack all, a later message's ack would also ack an earlier
//     one still waiting for its retry.
//
// Recreating the durable would drop its cursor, so the listener leaves it to an operator, and the
// refusal names every such setting the durable carries. It says what a delete costs, since the
// listener recreates a deleted durable at deliver policy new, and how to recreate it at its cursor.
func listenerDurableRefusal(consumer string, config nats.ConsumerConfig) error {
	var settings []string
	if config.Heartbeat != 0 {
		settings = append(settings, fmt.Sprintf("an idle heartbeat of %s", config.Heartbeat))
	}
	if config.AckPolicy != nats.AckExplicitPolicy {
		settings = append(settings, fmt.Sprintf("ack policy %s", config.AckPolicy))
	}
	if len(settings) == 0 {
		return nil
	}
	without := "that setting"
	if len(settings) > 1 {
		without = "those settings"
	}
	return fmt.Errorf("%w: durable consumer %s has %s, which the listener's consumer policy forbids and NATS cannot change in place; "+
		"deleting it lets the listener recreate it at deliver policy new, which skips every message stored before the create, "+
		"including any the old durable had not acked, so to keep its cursor recreate it from its own config without %s, "+
		"at deliver policy by_start_sequence with opt_start_seq one past its ack_floor.stream_seq (packages/envoy/deploy/README.md, Recreating a listener's durable)",
		errListenerDurableRefused, consumer, strings.Join(settings, " and "), without)
}

// listenerDurable reads the machine's durable under ctx. It returns a nil info when there is none,
// listenerDurableRefusal's error when the listener's policy cannot use the one there is, and the
// lookup's error when NATS cannot say: bus.ErrReconnecting while nats.go reconnects the
// connection, since the read goes through the client's JetStream context, which waits for nothing
// and names that refusal itself (bus.Client.JS).
func listenerDurable(ctx context.Context, client *bus.Client, consumer string) (*nats.ConsumerInfo, error) {
	info, err := client.JS().ConsumerInfo(bus.Stream, consumer, nats.Context(ctx))
	if errors.Is(err, nats.ErrConsumerNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := listenerDurableRefusal(consumer, info.Config); err != nil {
		return nil, err
	}
	return info, nil
}

// ensureListenerDurable preserves the durable consumer so restarts resume from the last ACKed
// message instead of skipping pending work, and returns it as the server holds it: it reads the
// durable (listenerDurable) and writes it to the consumer policy (correctListenerDurable). Every
// request runs under ctx. Only a receiving term calls it, on a resubscribe: the lease holder's,
// or the term of a listener that runs without the lease bucket's grant. A standby neither creates
// nor updates the durable. Its requests go through the client's JetStream context and wait for no
// connection, so one made while nats.go reconnects fails at once as bus.ErrReconnecting
// (bus.Client.JS), and the resubscribe that called it tries again.
func ensureListenerDurable(ctx context.Context, client *bus.Client, consumer string) (*nats.ConsumerInfo, error) {
	info, err := listenerDurable(ctx, client, consumer)
	if err != nil {
		return nil, err
	}
	return correctListenerDurable(ctx, client, consumer, info)
}

// correctListenerDurable writes the durable listenerDurable read as info to the consumer policy and
// returns it as the server then holds it. A durable that is not there (info nil) is created
// server-side, at a random deliver inbox; one that is there is updated when it has drifted from
// the policy, which also stamps consumerInactiveThreshold onto durables created before the
// threshold existed. Every request runs under ctx.
//
// The consumer is always created server-side, never by js.Subscribe: the client library deletes
// a consumer it created itself on Unsubscribe()/Drain(), which would silently reset the cursor.
func correctListenerDurable(ctx context.Context, client *bus.Client, consumer string, info *nats.ConsumerInfo) (*nats.ConsumerInfo, error) {
	if info == nil {
		// The deliver subject is a random inbox, not a derivable name: on a shared NATS account a
		// predictable subject would let any client hold interest on it, shadow-reading deliveries.
		// It is the connection's own inbox, under the connection's inbox prefix, which an edge
		// listener's grant subscribes in place of every reply inbox.
		//
		// The durable starts at deliver policy new. A create happens only for a machine id with
		// no durable, or for one whose durable was deleted, so every message stored before it is
		// one this listener never held. Deliver-new needs no clock, where a start time would
		// compare this listener's clock with the server's, and a create during a resubscribe
		// replays nothing either. What it gives up: a message stored between this process's start
		// and the create is not seen by this new durable; the session's own consumer still
		// delivers the message, and the other listeners' durables still decide its wake. On a
		// resubscribe that recreates a deleted durable, a message stored between the deletion and
		// the create is missed too, and where this listener holds the session's interest row the
		// other listeners acknowledge it with no wake, so that wake is not reported; this is
		// accepted, since the gap is one probe interval and the event is rare. A durable that is
		// there keeps its own deliver policy and cursor: the drift correction below writes none.
		config := nats.ConsumerConfig{
			Durable:        consumer,
			DeliverSubject: client.Connection().NewInbox(),
			DeliverPolicy:  nats.DeliverNewPolicy,
		}
		applyListenerConsumerPolicy(&config)
		return client.JS().AddConsumer(bus.Stream, &config, nats.Context(ctx))
	}
	// Only a field the policy writes can differ: the copy shares every other one, the server-set
	// metadata included, and listenerDurable has already refused any ack policy but explicit.
	corrected := info.Config
	applyListenerConsumerPolicy(&corrected)
	if reflect.DeepEqual(corrected, info.Config) {
		return info, nil
	}
	return client.JS().UpdateConsumer(bus.Stream, &corrected, nats.Context(ctx))
}

// errDurableUnbound marks a durable consumer that exists but does not deliver to the lease
// holder's subscription: the holder's /healthz quotes it as "durable consumer unavailable: not
// bound to this listener's subscription", and the self-health monitor treats it as terminal, so
// its rebuild subscribes again (durableReceiver.resubscribe). A standby has no subscription and
// never reports it.
var errDurableUnbound = errors.New("not bound to this listener's subscription")

// durableProbeConfirm is how long the holder's durable probe waits before its confirming read.
const durableProbeConfirm = 250 * time.Millisecond

// waitUnlessDone waits d out and reports nil, or returns ctx.Err() when ctx ended first. Every
// wait of the receive loop sleeps this way, so none holds a shutdown or a lost lease waiting for a
// timer the cancellation cannot cut.
func waitUnlessDone(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// awaitDone waits for done and reports nil, or returns ctx.Err() when ctx ends first. A wait whose
// work finished just as ctx ran out reports it finished (closedWithin reads done again once ctx
// has ended). Every wait of the shutdown on a loop that is stopping reads it this way
// (lease.Release, durableReceiver.Stop, waitLoops, shutdownListener).
func awaitDone(ctx context.Context, done <-chan struct{}) error {
	if closedWithin(ctx, done) {
		return nil
	}
	return ctx.Err()
}

// durableNeedsDelivery reports whether the stored message on subject, stored at stream sequence
// storedSeq, may need this listener's delivery handler, read from the subject alone, and when it
// does not, how many sessions of this machine take it another way. The listener's own kept lane
// (bus.KeptTopicPrefix) never needs it and counts nothing: a kept copy carries the role topic its
// sender published it on, so a listener that delivered it from the durable would arbitrate that
// role message again on every pod of every machine, where the copy is the role's next holder's
// alone and the kept lane's drain hands it over (role_keep.go). The durable takes every subject
// the stream stores (consumerFilterAll), so this is what keeps the lane out of delivery, rather
// than a filter no build-independent durable can carry. A role lane's subject always needs it. A
// session's agent subject does unless the session has a record or is another listener's
// (agentSubjectNeedsDelivery). Any other subject does when it matches a topic of an interest of
// this machine (the registry's subject index) whose session's consumer does not hold it
// (session.SessionEntry.ConsumerHoldsStored; a message stored below a consumer's start is in no
// consumer, and fanoutDelivery in cmd/listener/delivery.go decides it); with none, the fan-out
// would deliver it to nobody and report nothing. An interest that matches subject only through a
// topic its session's consumer filter leaves out is no recipient at all, an earlier build's row,
// and is skipped with its WARN line (skipLeftOutInterest): were it read as a consumer that holds
// the message, the message would be acknowledged here, before the fan-out could see it, with no
// word.
func durableNeedsDelivery(cfg listenerDeliveryHandlerConfig, subject string, storedSeq uint64) (needed bool, skipped int) {
	if strings.HasPrefix(subject, bus.KeptTopicPrefix) {
		return false, 0
	}
	if strings.HasPrefix(subject, contracts.RoleTopicPrefix) {
		return true, 0
	}
	if strings.HasPrefix(subject, contracts.AgentTopicPrefix) {
		return agentSubjectNeedsDelivery(cfg, agentSubjectSession(subject))
	}
	for _, interest := range cfg.registry.Match(cfg.machineID, subject) {
		if skipLeftOutInterest(cfg.logger, interest, subject) {
			continue
		}
		entry, err := cfg.sessions.Get(interest.SessionID)
		if err != nil || !entry.ConsumerHoldsStored(storedSeq) {
			return true, 0
		}
		skipped++
	}
	return false, skipped
}

// agentSubjectNeedsDelivery is durableNeedsDelivery for sessionID's agent subject. The session's
// own consumer delivers every message on it, so this listener has nothing to deliver. It
// acknowledges the message, counting one skipped delivery, when its cached record of the session
// reads live or unresponsive, whatever that consumer holds: the consumer delivers the message, or
// the consumer that the session's next registration starts does. A record past the gone horizon
// counts as none. It acknowledges the message with no count when its cached interest row names
// another listener, which decides the message itself (session.AgentOwnedElsewhere). Every other
// message goes to agentSubjectDelivery, which reads the session bucket itself, since this cache
// trails it, and waits once before it reports a wake.
func agentSubjectNeedsDelivery(cfg listenerDeliveryHandlerConfig, sessionID string) (needed bool, skipped int) {
	if _, state := cfg.sessions.Lookup(sessionID); notGone(state) {
		return false, 1
	}
	interest, cached := cfg.registry.CachedInterest(sessionID)
	return !cached || interest.MachineID == "" || interest.MachineID == cfg.machineID, 0
}

// agentSubjectSession is the session an agent subject names, in either form: the session alone
// (notifications.agent.<session>) or under a project (notifications.agent.<PROJECT or _>.<session>),
// read through the new form's old twin (contracts.LegacyTwin). A reader that cut the prefix off the
// new form would look up a session named <PROJECT>.<session>, which none is.
func agentSubjectSession(subject string) string {
	if twin, ok := contracts.LegacyTwin(subject); ok {
		subject = twin
	}
	return strings.TrimPrefix(subject, contracts.AgentTopicPrefix)
}
