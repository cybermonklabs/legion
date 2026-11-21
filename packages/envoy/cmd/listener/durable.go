package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
)

// Canonical policy for the listener's durable consumer. DeliverSubject is
// deliberately not part of the policy: it is fixed at creation and preserved
// for the consumer's lifetime (nats.Bind attaches to whatever subject the
// server has persisted).
//
// consumerInactiveThreshold lets the server delete a machine's durable
// consumer once its listener has been gone this long. The notification
// stream only retains 72h of messages, so a consumer inactive longer than
// that has already lost data and its durability protects nothing; without
// a threshold, every decommissioned machine leaves a consumer behind
// forever, silently accumulating pending state.
const (
	consumerAckWait           = 60 * time.Second
	consumerMaxAckPending     = 256
	consumerMaxDeliver        = 20
	consumerInactiveThreshold = 7 * 24 * time.Hour
)

// applyListenerConsumerPolicy stamps the canonical consumer policy onto
// config. Shared by the create and drift-correction paths so the policy has
// exactly one definition: the drift correction applies it to a copy of the
// durable's config and updates the durable when the copy differs. The policy
// has no idle heartbeat: the create path sets none, so a durable the listener
// creates has none. listenerDurableRefusal refuses an existing durable whose
// heartbeat or ack policy differs, since NATS cannot change either in place.
// The policy sets no deliver policy, start sequence or start time: the create
// path sets deliver-new itself (startListenerSubscription), and NATS refuses a
// change of deliver policy in place, so the drift correction leaves an
// existing durable's start as it is and the durable keeps its cursor.
func applyListenerConsumerPolicy(config *nats.ConsumerConfig, subjects []string) {
	config.FilterSubject = ""
	config.FilterSubjects = subjects
	config.AckPolicy = nats.AckExplicitPolicy
	config.AckWait = consumerAckWait
	config.MaxAckPending = consumerMaxAckPending
	config.MaxDeliver = consumerMaxDeliver
	config.InactiveThreshold = consumerInactiveThreshold
}

// errListenerDurableRefused marks a durable startListenerSubscription will not bind however often
// it is asked: no retry can succeed, so the listener's startup exits at once.
var errListenerDurableRefused = errors.New("listener durable refused")

// durableRefused reports whether err ends a start's durable at once, as no retry can get past it:
// the listener's own refusal of the durable (errListenerDurableRefused), or the server's refusal of
// a request of the durable's read, create or update under the connection's grant
// (bus.ErrPublishDenied), which the server gives again however often the request is sent.
func durableRefused(err error) bool {
	return errors.Is(err, errListenerDurableRefused) || errors.Is(err, bus.ErrPublishDenied)
}

// durableRefusedLine is the line a start logs when its durable is refused (durableRefused), at the
// check after the NATS connect and at the bind, with the refusal as its error. The deployed
// durable-refusal metric filter matches it exactly (defined in the deployment repository's listener
// infrastructure), so both sites use this constant.
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

// listenerDurable reads the machine's durable. It returns a nil info when there is none,
// listenerDurableRefusal's error when the listener's policy cannot use the one there is, and the
// lookup's error when NATS cannot say: bus.ErrReconnecting while nats.go reconnects the
// connection, since the read goes through the client's JetStream context, which waits for nothing
// and names that refusal itself (bus.Client.JS).
func listenerDurable(client *bus.Client, consumer string) (*nats.ConsumerInfo, error) {
	info, err := client.JS().ConsumerInfo(bus.Stream, consumer)
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

// startListenerSubscription preserves the durable consumer so restarts resume
// from the last ACKed non-role message instead of skipping pending work.
//
// The consumer is always created server-side and then bound, never created by
// js.Subscribe: the client library deletes consumers it created itself on
// Unsubscribe()/Drain(), and both the bus reconnect recovery and the SIGTERM
// drain paths trigger exactly that — silently resetting the durable cursor.
// The config drift-correction also stamps consumerInactiveThreshold onto
// consumers created before the threshold existed.
//
// Its requests go through the client's JetStream context and wait for no connection, so one made
// while nats.go reconnects fails at once as bus.ErrReconnecting (bus.Client.JS), and the bind or
// the rebuild that called it tries again.
func startListenerSubscription(client *bus.Client, consumer string, handler nats.MsgHandler) (*nats.Subscription, error) {
	subjects := bus.StreamSubjects()
	info, err := listenerDurable(client, consumer)
	switch {
	case err != nil:
		return nil, err
	case info == nil:
		// The deliver subject is a random inbox, not a derivable name: on a
		// shared NATS account a predictable subject would let any client
		// hold interest on it — shadow-reading deliveries and making the
		// consumer look push-bound so the listener could never attach.
		//
		// The durable starts at deliver policy new. A create happens only for a machine id with
		// no durable, or for one whose durable was deleted, so every message stored before it is
		// one this listener never held. Deliver-new needs no clock, where a start time would
		// compare this listener's clock with the server's, and a create during a rebuild replays
		// nothing either. What it gives up: a message stored between this process's start and the
		// create is not seen by this new durable; the session's own consumer still delivers the
		// message, and the other listeners' durables still decide its wake. On a self-health
		// rebuild that recreates a deleted durable, a message stored between the deletion and the
		// create is missed too, and where this listener holds the session's interest row the other
		// listeners acknowledge it with no wake, so that wake is not reported; this is accepted,
		// since the gap is one probe interval and the event is rare.
		config := nats.ConsumerConfig{
			Durable:        consumer,
			DeliverSubject: nats.NewInbox(),
			DeliverPolicy:  nats.DeliverNewPolicy,
		}
		applyListenerConsumerPolicy(&config, subjects)
		if _, err := client.JS().AddConsumer(bus.Stream, &config); err != nil {
			return nil, err
		}
	default:
		// Only a field the policy writes can differ: the copy shares every other one, the
		// server-set metadata included, and listenerDurable has already refused any ack policy
		// but explicit.
		corrected := info.Config
		applyListenerConsumerPolicy(&corrected, subjects)
		if !reflect.DeepEqual(corrected, info.Config) {
			if _, err := client.JS().UpdateConsumer(bus.Stream, &corrected); err != nil {
				return nil, err
			}
		}
	}
	return client.Subscribe(
		"",
		handler,
		nats.Bind(bus.Stream, consumer),
		nats.ManualAck(),
	)
}

// errListenerDurableBindExhausted marks a bind still refused at its deadline: something other than
// a rolling deploy's old task holds the durable, or JetStream keeps failing the lookup, so the start
// ends and the runtime starts the listener again on a fresh connection.
var errListenerDurableBindExhausted = errors.New("listener durable bind exhausted")

// During a rolling deploy the durable's push binding is the old task's until that task stops, which
// takes its deregistration from the load balancer and its own shutdown: 35 to 71 s in production.
// The bind is polled every durableBindInterval, so it lands within one interval of the old task's
// exit, and given up at durableBindDeadline, the 135 s the backoff it replaced took to exhaust, so
// the subscribe-exhausted page and the runtime's relaunch come no later than they did.
const (
	durableBindInterval = 2 * time.Second
	durableBindDeadline = 135 * time.Second
	durableBindLogEvery = 30 * time.Second
)

// durableBind is how many attempts a bind took and how long it waited.
type durableBind struct {
	attempts int
	waited   time.Duration
}

// bindListenerDurable binds consumer through startListenerSubscription every interval until it
// binds, the bus's auto-resubscribe after a reconnect binds it, the durable is refused
// (errListenerDurableRefused, returned as is), the connection's grant refuses a request of the
// bind (bus.ErrPublishDenied, returned as is: the grant refuses it again however often it is
// sent, so waiting out deadline would only delay the start's end), deadline passes
// (errListenerDurableBindExhausted, wrapping the last failure), or ctx ends (ctx.Err()). It logs
// the first failure and then one every durableBindLogEvery, and the bind itself.
func bindListenerDurable(ctx context.Context, client *bus.Client, consumer string, handler nats.MsgHandler, logger *logging.Logger, interval, deadline time.Duration) (durableBind, error) {
	start := time.Now()
	var bind durableBind
	bound := func(autoResubscribe bool) (durableBind, error) {
		logger.Info("durable bound",
			slog.Int("attempts", bind.attempts),
			slog.Int64("waited_ms", bind.waited.Milliseconds()),
			slog.Bool("auto_resubscribe", autoResubscribe),
		)
		return bind, nil
	}
	var logged time.Time
	for {
		if err := ctx.Err(); err != nil {
			bind.waited = time.Since(start)
			return bind, err
		}
		bind.attempts++
		_, err := startListenerSubscription(client, consumer, handler)
		bind.waited = time.Since(start)
		if err == nil {
			return bound(false)
		}
		if durableRefused(err) {
			return bind, err
		}
		if bind.waited >= deadline {
			return bind, fmt.Errorf("%w after %d attempts in %s: %w", errListenerDurableBindExhausted, bind.attempts, bind.waited.Round(time.Millisecond), err)
		}
		wait := min(interval, deadline-bind.waited)
		if logged.IsZero() || time.Since(logged) >= durableBindLogEvery {
			logged = time.Now()
			logger.Warn("subscribe failed, retrying",
				slog.String("error", err.Error()),
				slog.Int("attempt", bind.attempts),
				slog.String("retry_in", wait.String()),
				slog.String("deadline", deadline.String()),
			)
		}
		if err := waitUnlessDone(ctx, wait); err != nil {
			bind.waited = time.Since(start)
			return bind, err
		}
		// A reconnect during the wait re-subscribes the subscription the failed attempt registered
		// (bus.Client.afterReconnect, off nats.go's callback goroutine and never under a hook run's
		// context), which binds it once the durable is free. Another attempt would replace that
		// handle with a bind racing the server's release of it, which can be refused with "consumer
		// is already bound to a subscription".
		if client.SubOK() {
			bind.waited = time.Since(start)
			return bound(true)
		}
	}
}

// errDurableUnbound marks a durable consumer that exists but does not deliver to this listener:
// the health endpoint quotes it as "durable consumer unavailable: not bound to this listener's
// subscription", and the self-health monitor treats it as terminal so the rebuild runs.
var errDurableUnbound = errors.New("not bound to this listener's subscription")

// durableUnbound reports whether consumer info describes a durable that is not delivering to
// client's own subscription. PushBound alone is not enough: it says *some* subscriber is bound,
// not this one. During a rolling deploy two tasks of one machine id bind the same durable by name
// (cmd/listener/startup_test.go:142-166), so a durable deleted and recreated server-side and then
// taken by the replacement reads PushBound == true on the task that is receiving nothing at all.
// The deliver subject is what tells the two apart: a recreated durable delivers to a fresh inbox,
// while this listener's subscription still holds the old one.
func durableUnbound(info *nats.ConsumerInfo, client *bus.Client) bool {
	return info == nil || !info.PushBound || info.Config.DeliverSubject != client.SubSubject()
}

// durableProbeConfirm is how long durableProbe waits before its confirming read.
const durableProbeConfirm = 250 * time.Millisecond

// durableProbe is the self-health monitor's durable check. A consumer that is gone is reported as
// nats.ErrConsumerNotFound, and one that exists but delivers elsewhere as errDurableUnbound; both
// are terminal, so the monitor rebuilds. Any other error is deliberately reported as success and
// that is load-bearing: a JetStream deadline or a reconnect gap must not count as a probe failure,
// because the monitor splits its transient and terminal counts on exactly that.
//
// Each read runs under ctx, so the shutdown's cancellation cuts a read the server has not
// answered rather than leaving it to run out. ctx carries no deadline of its own, so nats.go puts
// the JetStream handle's 10 s MaxWait (internal/bus/nats.go:247) under it, and a read stays
// bounded as it was before. A read ctx cuts fails with ctx's own error, context.Canceled or
// context.DeadlineExceeded, which is no ErrConsumerNotFound and so reads as success like any
// other unclassified error.
//
// An unbound read is confirmed by a second read, because one is not enough: the rebuild
// unsubscribes the live handle before it binds (internal/bus/nats.go:341-348), so a single stale
// answer taken during somebody else's rebuild would cost this listener a working subscription, and
// the server's PushBound is one round trip old anyway. A confirming read that fails, or that reads
// bound, returns whatever the rules above return for it. ctx ending during the confirming wait
// reports success too: the unbound read was never confirmed, and an unconfirmed read is no more a
// probe failure than an unclassified error is.
func durableProbe(ctx context.Context, client *bus.Client, consumer string) error {
	for read := range 2 {
		if read > 0 && waitUnlessDone(ctx, durableProbeConfirm) != nil {
			return nil
		}
		info, err := client.JS().ConsumerInfo(bus.Stream, consumer, nats.Context(ctx))
		if err != nil {
			if errors.Is(err, nats.ErrConsumerNotFound) {
				return err
			}
			return nil
		}
		if !durableUnbound(info, client) {
			return nil
		}
	}
	return errDurableUnbound
}

// The rebind pair is deliberately far shorter than the startup pair above. durableBindInterval and
// durableBindDeadline (2 s / 135 s) wait out a rolling deploy's old task holding the durable; these
// run inside the self-health monitor, whose rebuild is synchronous and which shutdownListener waits
// for inside the one 10 s budget it shares with the HTTP stop (main.go:930-950), so a rebind that
// cannot land must give up long before that budget is spent. awaitDurableBound keeps to the
// deadline even against a server that never answers: each read is cut where the deadline falls,
// not at the JetStream handle's 10 s MaxWait (internal/bus/nats.go:247), which would stretch the
// 3 s here to 13 s.
const (
	durableRebindInterval = 250 * time.Millisecond
	durableRebindDeadline = 3 * time.Second
)

// awaitDurableBound waits until the server itself reports consumer delivering to client's own
// subscription, so a rebuild reports success only once the binding exists rather than once the
// subscribe call returned. It returns ctx.Err() when ctx ends first, an error wrapping
// errDurableUnbound, naming how long it waited, once deadline passes, and a read the connection's
// grant refuses (bus.ErrPublishDenied) at once, since no later read gets past it. Each read runs
// under ctx and ends no later than deadline, so neither a cancellation nor the deadline waits on
// a read the server has not answered.
func awaitDurableBound(ctx context.Context, client *bus.Client, consumer string, interval, deadline time.Duration) error {
	start := time.Now()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		waited := time.Since(start)
		if waited >= deadline {
			return fmt.Errorf("durable %s %w after %s", consumer, errDurableUnbound, waited.Round(time.Millisecond))
		}
		readCtx, cancel := context.WithTimeout(ctx, deadline-waited)
		info, err := client.JS().ConsumerInfo(bus.Stream, consumer, nats.Context(readCtx))
		cancel()
		if errors.Is(err, bus.ErrPublishDenied) {
			return fmt.Errorf("read durable %s: %w", consumer, err)
		}
		if err == nil && !durableUnbound(info, client) {
			return nil
		}
		if err := waitUnlessDone(ctx, min(interval, deadline-time.Since(start))); err != nil {
			return err
		}
	}
}

// waitUnlessDone waits d out and reports nil, or returns ctx.Err() when ctx ended first. Both the
// bind retry and the bound wait sleep this way, so neither holds a shutdown waiting for a timer
// the cancellation cannot cut.
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

// waitWithin waits for wg within ctx and reports whether wg finished before ctx ended.
func waitWithin(ctx context.Context, wg *sync.WaitGroup) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

// durableNeedsDelivery reports whether the stored message on subject, stored at stream sequence
// storedSeq, may need this listener's delivery handler, read from the subject alone, and when it
// does not, how many sessions of this machine take it another way. A role lane's subject always
// needs it. A session's agent subject does unless the session has a record or is another
// listener's (agentSubjectNeedsDelivery). Any other subject does when it matches a topic of an
// interest of this machine (the registry's subject index) whose session's consumer does not hold
// it (session.SessionEntry.ConsumerHoldsStored; a message stored below a consumer's start is in
// no consumer, and fanoutDelivery in cmd/listener/delivery.go decides it); with none, the fan-out
// would deliver it to nobody and report nothing.
func durableNeedsDelivery(cfg listenerDeliveryHandlerConfig, subject string, storedSeq uint64) (needed bool, skipped int) {
	if strings.HasPrefix(subject, contracts.RoleTopicPrefix) {
		return true, 0
	}
	if strings.HasPrefix(subject, contracts.AgentTopicPrefix) {
		return agentSubjectNeedsDelivery(cfg, agentSubjectSession(subject))
	}
	for _, interest := range cfg.registry.Match(cfg.machineID, subject) {
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
