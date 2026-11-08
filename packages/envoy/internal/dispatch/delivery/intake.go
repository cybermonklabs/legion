// Intake (LEGION-567): a Dispatch server consumer of the GitHub events Envoy already relays over
// NATS (`notifications.github.>`), for live updates between the five-minute reconcile's passes.
// Every write here goes through the same upsert functions reconcile uses (store.go), so a NATS
// redelivery or a reconcile re-reading the same fact is a no-op, not a duplicate. A GitHub call
// that fails here is logged and the message is still acked, because the reconcile that runs
// every five minutes is the actual catch-up mechanism ("a missed event appears by the next
// five-minute reconcile", LEGION-567). The one failure that mechanism does not cover is a pull
// request's head row that was never written, which no pass can offer because the passes read
// the rows that exist: that envelope is handed back to NATS for a bounded number of
// redeliveries instead (ErrHeadNotStored, Intake.deliver, headWriteMaxDeliveries).
package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// ompSessionTrailer matches a commit message's `Omp-Session: <id>` trailer line (the same shape
// LEGION-294's own collector reads).
var ompSessionTrailer = regexp.MustCompile(`(?m)^Omp-Session:\s*(\S+)\s*$`)

// issueKeyCandidate matches a bare Dispatch issue key mention (e.g. ACME-1072), the same shape
// this codebase's own issueKeyPattern uses elsewhere (packages/envoy/internal/dispatch/api/server.go,
// packages/envoy/internal/dispatch/text/refs.go) -- duplicated here rather than imported because
// both are unexported in their own packages and this package does not depend on either.
var issueKeyCandidate = regexp.MustCompile(`\b[A-Z][A-Z0-9]{1,9}-[1-9][0-9]*\b`)

// Intake wires the NATS consumer to the database, the GitHub App client and the event broker the
// issue.deployed events go through (RecordDeployedEvents). StartIntakes builds it, and only with
// the App configured.
type Intake struct {
	nats   *bus.Client
	pool   *store.Pool
	github *githubapp.Client
	events *events.Broker
	// settings is the delivery settings every message is routed by, shared with the job intake
	// (StartIntakes).
	settings *SettingsCache
	// leases is the pool bind takes this durable's consumer lease on (consumerLease), so that one
	// process at a time reconciles this durable's configuration and subscribes to it
	// (bindDurableConsumer).
	leases duty.Leases
	// throttle is this intake's own bind-poll state: how recently it reconciled its durable and
	// how recently it said it was standing by for it (bindThrottle).
	throttle *bindThrottle

	mu sync.Mutex
	// delivering is the latest subscription subscribe made, which StopDelivering stops.
	delivering *intakeDelivery
	// boundHook, when not nil, runs once a bind has subscribed, before the bind returns. It is nil
	// outside tests: a test sets it to hold a bind that has subscribed until past Run's ctx's end.
	boundHook func()
}

// newIntake builds an Intake on natsClient, a connected client StartIntakes opens for it alone,
// reconciling its durable under a lease it takes on leases and routing by settings.
func newIntake(natsClient *bus.Client, pool *store.Pool, leases duty.Leases, github *githubapp.Client, broker *events.Broker, settings *SettingsCache) *Intake {
	return &Intake{nats: natsClient, pool: pool, github: github, events: broker, settings: settings, leases: leases, throttle: &bindThrottle{}}
}

// Run binds the durable consumer and processes GitHub events until ctx is cancelled, then stops
// delivering (StopDelivering) under intakeStopBudget from ctx's end before it returns. Idle until
// the settings cache holds settings.
//
// The durable carries one filter subject, githubIntakeSubject, which no setting changes, so no
// settings change rebinds it. Each message is routed by the settings the cache holds when its
// handler runs, so a settings change -- a different deploy repository or workflow path -- takes
// effect as soon as the cache has read it, with no restart and no rebind. route does all the
// filtering, from those settings.
//
// Three things wake an unbound intake: the cache storing settings, since an intake with none has
// nothing to route by, a connection event on its client, since a bind with no connection is a
// ConsumerInfo that blocks for the client's whole JetStream request timeout (10 s) and is then
// logged at ERROR, every durableBindInterval, and, once it has both, durableBindInterval's
// ticker. The durable admits one subscriber, so a second envoy-dispatch process stands by on that
// ticker -- for the life of the process, with no deadline -- and takes the durable over within
// seconds of the process holding it going away.
//
// A bound intake waits on ctx and on the next connection event, which is what takes the durable
// away from this process: nats.go re-sends a subscription inside its own reconnect, and the
// client binds its registration again on a connection it dialled to replace a closed one
// (internal/bus/recovery.go), neither of them asking whether the durable is still this process's
// to deliver. A process back from an outage would then deliver every message of it beside the
// process that took the durable over while it was away (INFRA-1551). So the subscription is given
// up at the event (giveUpOnConnectionChange) -- at the disconnect, the first connection event of
// an outage, before nats.go can re-send it -- and the intake goes back to the bind poll, which
// takes the durable again only while it is free.
//
// The PR-checks workflow path is live-reachable only for a fork-originated pull request: Envoy's
// own webhook normalizer (internal/contracts/normalize.go, untouched by this package) drops a
// workflow_run envelope whose pull_requests array is non-empty, which GitHub populates for a
// same-repo PR's run but reports empty for a cross-fork one. For the common same-repo case those
// runs never arrive here at all; the five-minute reconcile is the only path that ever records
// them. Routed rather than ignored because it is the only live-update path for the fork case,
// which does reach it.
func (in *Intake) Run(ctx context.Context) {
	// Armed before the first bind, so a bind under way as ctx ends -- a standby's poll, whose
	// JetStream calls take their own time -- spends the stop's budget rather than starting it late.
	stopping, release := stopContext(ctx)
	defer release()
	binds := time.NewTicker(durableBindInterval)
	defer binds.Stop()
	for {
		// Both signals are taken before the state they report is read, as ConnectionChanged's own
		// contract asks, so settings stored or a connection installed during the bind wake the
		// loop rather than being missed by it.
		changed, connection := in.settings.Changed(), in.nats.ConnectionChanged()
		// poll stays nil, and so never ready, while the cache holds no settings or the client has
		// no connection: a tick then has nothing to route by or nothing to ask the server over,
		// and what the loop waits on instead is the signal that ends that.
		var poll <-chan time.Time
		if in.settings.Load() != nil && in.nats.Connected() {
			if in.bind(ctx, stopping) == bindBound {
				// Nothing polls while this process holds the durable, so the ticker stops here
				// rather than waking every two seconds; a give-up starts it again, a full
				// durableBindInterval from the Reset, since a tick prepared before a Stop or a
				// Reset is never received after it (Go 1.23's timer channels).
				binds.Stop()
				select {
				case <-ctx.Done():
					in.StopDelivering(stopping)
					return
				case <-connection:
					in.giveUpOnConnectionChange(ctx)
					binds.Reset(durableBindInterval)
					continue
				}
			}
			poll = binds.C
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		case <-connection:
		case <-poll:
		}
	}
}

// giveUpOnConnectionChange gives this process's delivery subscription up because its client's
// connection changed under it, which for an outage is the disconnect: the stop drains the
// subscription and gives up the registration the client kept of it, and while the link is down
// that is an unsubscribe, so nats.go's own reconnect finds nothing left to re-send
// (bus.Client.DrainSubscription). What the intake had in flight is handed back to the process
// that keeps the durable, and the bind poll it returns to takes the durable again only while it
// is free.
//
// The stop runs under a budget of its own -- intakeStopBudget from the event, on a context Run's
// ctx ending does not cut short -- rather than under Run's stop deadline: one handler that will
// not return must not keep the intake out of its poll for the life of the process, and a ctx that
// ends in the middle of the give-up must not cut the hand-back shorter than the budget that
// bounds it. A handler still running at the bound is given up as a stop gives it up, its message
// handed back with the rest (StopDelivering).
func (in *Intake) giveUpOnConnectionChange(ctx context.Context) {
	slog.Info(intakeConnectionChangedMessage, "name", bus.DispatchDeliveryDurable)
	stopping, cancel := context.WithTimeout(context.WithoutCancel(ctx), intakeStopBudget)
	defer cancel()
	in.StopDelivering(stopping)
}

// route dispatches one envelope payload by its kind field: a pull request to
// handlePullRequestEnvelope, a workflow run to routeWorkflowEnvelope, any other kind nowhere. It is
// the whole filter: the durable is handed every GitHub notification on the bus
// (githubIntakeSubject), CI settlements, comments and the events Envoy builds no payload for among
// them. The payload is decoded once, into its fields. An empty payload, or one of a kind route does
// not handle, is discarded before any GitHub call or write, whatever its other fields hold (a CI
// settlement's hold arrays, cistore.Summary); only a pull request's or a workflow run's fields are
// read as the flat string map its handler takes. handled reports whether the kind is one route
// hands to a handler, whatever that handler then decides, so deliver records the event's time for
// a pull request that was not merged or another repository's run as well. A payload that is not a
// JSON object, a kind that is not a string, and a handled kind's payload holding anything but
// strings are errMalformedPayload.
//
// An envelope whose handler stored a fact the issue.deployed rule reads (a pull request's row, a
// run, its jobs) ends with recordDeployed, whether or not the handler then failed: a run written
// before its jobs fetch failed may still be the run that shipped a pull request stored earlier.
func (in *Intake) route(ctx context.Context, settings DeliverySettings, raw string) (handled bool, err error) {
	if raw == "" {
		return false, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return false, fmt.Errorf("%w: %w", errMalformedPayload, err)
	}
	var kind string
	if field, ok := fields["kind"]; ok {
		if err := json.Unmarshal(field, &kind); err != nil {
			return false, fmt.Errorf("%w: kind: %w", errMalformedPayload, err)
		}
	}
	var handle func(context.Context, DeliverySettings, map[string]string) (bool, error)
	switch kind {
	case "pr":
		handle = in.handlePullRequestEnvelope
	case "workflow":
		handle = in.routeWorkflowEnvelope
	default:
		return false, nil
	}
	payload, err := flatPayload(fields)
	var wrote bool
	if err == nil {
		wrote, err = handle(ctx, settings, payload)
	}
	if wrote {
		in.recordDeployed(ctx, settings)
	}
	return true, err
}

// recordDeployed writes the issue.deployed of every pull request the envelope just handled may have
// shipped or completed (RecordDeployedEvents). A failure is logged and does not fail the envelope,
// which is acknowledged as every handled envelope is: the reconcile records at the end of every
// pass too, so a row this call could not write gets its event within five minutes.
func (in *Intake) recordDeployed(ctx context.Context, settings DeliverySettings) {
	written, err := RecordDeployedEvents(ctx, in.pool, settings, in.events)
	if err != nil {
		slog.Error("dispatch delivery: record issue.deployed after an envelope", "error", err)
	}
	if written > 0 {
		slog.Info("dispatch delivery: recorded issue.deployed", "events", written, "by", "intake")
	}
}

// routeWorkflowEnvelope hands a workflow run to handleWorkflowEnvelope as the run kind its file is
// configured as. The runs this slice records are the deploy repository's own: another repository's
// run of a file with the same path is not one of them, and routing it would fetch a run this slice
// never stores. A run of a file neither setting names is not fetched either.
func (in *Intake) routeWorkflowEnvelope(ctx context.Context, settings DeliverySettings, payload map[string]string) (bool, error) {
	if payload["repo"] != settings.DeployRepo {
		return false, nil
	}
	switch payload["path"] {
	case settings.DeployWorkflowPath:
		return in.handleWorkflowEnvelope(ctx, settings, DeliveryRunKindDeploy, payload)
	case settings.PRChecksWorkflowPath:
		// A pr_checks run is no part of the shipping rule (shippingRunJoin reads deploy runs
		// alone), so storing one can ship nothing: the envelope ends with no deployed pass.
		_, err := in.handleWorkflowEnvelope(ctx, settings, DeliveryRunKindPRChecks, payload)
		return false, err
	default:
		return false, nil
	}
}

// errMalformedPayload is an envelope payload route cannot read: not a JSON object, a kind that is
// not a string, or a pull request's or workflow run's payload holding anything but strings.
var errMalformedPayload = errors.New("malformed envelope payload")

// flatPayload is a pull request's or workflow run's fields as the flat string map its handler
// reads, or errMalformedPayload when a field is not a string. The field's name is left out of the
// error, since an envelope's keys are its publisher's to choose.
func flatPayload(fields map[string]json.RawMessage) (map[string]string, error) {
	payload := make(map[string]string, len(fields))
	for name, field := range fields {
		var value string
		if err := json.Unmarshal(field, &value); err != nil {
			return nil, fmt.Errorf("%w: %w", errMalformedPayload, err)
		}
		payload[name] = value
	}
	return payload, nil
}

// githubIntakeSubject is the durable's one filter subject: every GitHub event Envoy relays. The
// subscription's callback reads each message's subject first (intakeKey) and acknowledges every
// subject that is not a pull request's own or a workflow run's without decoding it; the handler
// decides what to do with the rest (route), and fetches nothing for the ones this slice does not
// want.
//
// One subject rather than the set this once carried -- the wildcard pull-request subject
// notifications.github.*.*.pr.* beside one notifications.github.<owner>.<repo>.workflow.<file>.>
// per configured workflow file, through FilterSubjects -- because nats-server 2.10.29, the server
// these tests ran before v2.15.1, does not hand that set's workflow messages over. Measured there
// on one stream holding five pull-request and twelve workflow-run messages
// (TestWhichFilterShapeDeliversAWorkflowSubject): a durable carrying the set reported all of them
// in NumPending, then delivered the five pull requests and none of the runs, by pull fetch or push
// subscription alike, in either order, while each filter alone delivered its own messages. A
// withheld message was silent -- the consumer reported it pending and never errored -- which is
// why the durable an earlier release left recorded no workflow run. v2.15.1 hands that set's
// workflow messages over, as the same test now measures, so the one subject is no longer the only
// shape that works; it stays because it is the simplest, and because the filter then does not
// depend on the settings row, so a settings change needs no rebind.
const githubIntakeSubject = "notifications.github.>"

// intakeMessageTimeout bounds how long one envelope's own handling may take -- its GitHub calls
// (a pull request plus its commit pages, or a workflow run plus its jobs pages, each with this
// package's own bounded page retries) and its Postgres writes. A handler that runs past it is
// cancelled and logged, and acked for every failure but one: the five-minute reconcile is this
// package's catch-up mechanism, so one slow envelope must not hold the consumer. The one
// failure it does not cover is a deadline that falls on a pull request's head write, or on
// either of the reads before it, which leaves a head row no pass can offer (ErrHeadNotStored);
// that envelope is handed back instead, for the bounded number of redeliveries deliver allows.
// A var so a test can shrink it.
var intakeMessageTimeout = time.Minute

// intakeMaxAckPending bounds how many messages NATS may have outstanding with this consumer at
// once, and so how many handlers run at once: one per key (intakeKey), and every message of a key
// waits for the one before it. All of them can be one key's, so the bound has to be what one
// handler can acknowledge in turn well inside intakeAckWait; the server's own default of 1,000 is
// far past it, and messages it redelivers under a handler still working accumulate in the
// client's pending buffer until that buffer drops them. A var so a test can shrink it.
var intakeMaxAckPending = 4

// intakeAckWait is how long NATS waits for an acknowledgement before redelivering. Derived from
// the two values above rather than chosen: when every outstanding message is one key's, the last
// of intakeMaxAckPending waits for the ones before it, so the bound is their total handling time,
// plus a minute for the acknowledgement itself to land. A function rather than a value so a test
// that shrinks either input gets an ack wait derived from it.
func intakeAckWait() time.Duration {
	return time.Duration(intakeMaxAckPending)*intakeMessageTimeout + time.Minute
}

// deliveryConsumerConfig is the policy bind stamps on the durable (bus.DispatchDeliveryDurable,
// one push consumer over githubIntakeSubject), whether it creates it or finds one an earlier
// release left with another filter or the server's own flow control. The server holds a backlog
// undelivered until a subscriber attaches, so a restart with events waiting loses none of them
// and none of them count against MaxAckPending in the meantime.
func deliveryConsumerConfig() natsgo.ConsumerConfig {
	return natsgo.ConsumerConfig{
		Durable:        bus.DispatchDeliveryDurable,
		DeliverSubject: natsgo.NewInbox(),
		FilterSubject:  githubIntakeSubject,
		AckPolicy:      natsgo.AckExplicitPolicy,
		AckWait:        intakeAckWait(),
		MaxAckPending:  intakeMaxAckPending,
	}
}

// durableBindInterval is how often an intake with nothing bound tries its durable again. It is
// cmd/listener/durable.go's interval, for the reason measured there: a push durable's binding is
// its holder's until that process stops, which took 35 to 71 s in production, so a two-second
// poll lands the bind within one interval of the holder going away. The dead holder's work
// resumes in seconds rather than up to the 30-second settings poll the bind once rode on.
//
// Unlike the listener, which gives up at its durableBindDeadline and exits so the runtime starts
// it again, Dispatch's standby polls for the life of the process: there is no deadline and no
// exit. A durable another envoy-dispatch process holds is not a failed start but the ordinary
// state of the second process, which has every other thing a Dispatch server does to get on with
// meanwhile.
const durableBindInterval = 2 * time.Second

// standbyLogEvery bounds how often one intake says it is standing by for its durable, and for
// the same reason. The first refusal is logged as it happens, so an operator sees at once which
// process stood by and why; the bind is then retried every durableBindInterval, and an
// unthrottled line would be 1,800 an hour per durable, each saying exactly what the first said.
const standbyLogEvery = 30 * time.Second

// standbyConsumerHeldMessage is what a poll says when the server reports the durable bound to a
// subscriber: another process holds it, and this one stands by.
const standbyConsumerHeldMessage = "dispatch delivery: NATS consumer held by another process; standing by"

// standbyConsumerMissingMessage is what a poll says when the server has no such durable yet: the
// leased pass that creates it has not landed, and this one stands by.
const standbyConsumerMissingMessage = "dispatch delivery: NATS consumer not created yet; standing by"

// standbyLeaseHeldMessage is what a poll says when it could not take the durable's consumer
// lease: another process is inside this durable's bind pass, and this one stands by. A take that
// Postgres refused ends the same way, after duty.Exclusive has logged the refusal at ERROR
// ("dispatch duty: take the lease"), since Exclusive does not tell its caller which of the two
// it was.
//
// All three are declared here, and in one place, because bindThrottle.reportStandby throttles on
// message equality: two copies of one sentence that drift by a character are two throttled lines
// rather than one, at twice the INFO rate the throttle is for.
const standbyLeaseHeldMessage = "dispatch delivery: NATS consumer lease held by another process; standing by"

// intakeConnectionChangedMessage is what either intake says when a connection event takes its
// subscription away from it: it gave its durable up and is back in its bind poll, so a process
// that took that durable over while this one was away keeps it. Both log it with the durable's
// name, which is what tells the delivery intake's line from the job intake's. Declared here with
// the three standby messages, and in one place, because one sentence serving both intakes is one
// sentence rather than two copies that drift from each other by a character.
const intakeConnectionChangedMessage = "dispatch delivery: NATS connection changed; durable given up; standing by"

// consumerBindConfirmTimeout is how long a bind waits, still holding the durable's consumer
// lease, for the server to report the durable push-bound after its own subscribe returned. That
// wait is what makes PushBound truthful before the lease is given back, so the lease -- and not
// the client's own refusal of a bind -- is what keeps a second process out of the durable.
//
// Five seconds because the wait it bounds is milliseconds: 50 binds measured in one two-CPU pod
// against the in-process test server took 2.48 ms at their fastest, 4.26 ms at p50, 8.66 ms at
// p90 and 35.75 ms at their slowest from subscribe returning to the server reporting PushBound,
// and every one of the 50 was confirmed on its second ConsumerInfo read. Five seconds is about
// 140 times the slowest of them, so the bound is a hang guard and not a race: no ordinary bind
// comes near it, and one that reaches it has something wrong with it rather than something slow.
// It is half the client's JetStream request timeout (10 s, nats.MaxWait, internal/bus/nats.go:506)
// so that a read which hangs is cut by this bound -- the subscription given up and the poll made
// again a durableBindInterval later -- rather than by that one.
const consumerBindConfirmTimeout = 5 * time.Second

// consumerBindConfirmInterval is how often that confirmation reads ConsumerInfo. Every one of the
// 50 binds measured above was confirmed on its second read, so this interval, not the timeout, is
// what an ordinary bind adds to the time the lease is held: 10 ms is about the p90 of the wait
// itself (8.66 ms), so an ordinary bind holds the lease for roughly one extra read, while the
// interval leaves at most 500 reads in the worst case consumerBindConfirmTimeout allows.
const consumerBindConfirmInterval = 10 * time.Millisecond

// durablePushBound reports whether the server has a subscriber on the durable's deliver subject.
// It is the one question bindDurableConsumer asks before it takes a durable and after it has
// subscribed to one, asked of the server rather than of the client, because the client's own
// refusal of a second binder is itself derived from this flag and lags the SUB that sets it. A
// var so a test can drive the confirm to its bound
// (TestABindTheServerNeverConfirmsIsUnsubscribedAndFails).
var durablePushBound = func(ctx context.Context, js natsgo.JetStreamContext, name string) (bool, error) {
	info, err := js.ConsumerInfo(bus.Stream, name, natsgo.Context(ctx))
	if err != nil {
		return false, err
	}
	return info.PushBound, nil
}

// consumerLease names the lease one durable's bind attempt runs under (bindDurableConsumer): one
// process at a time reads that durable's configuration, corrects it and subscribes to it. Two
// processes -- two builds during a rolling update, each re-running the reconciliation on every
// bind poll -- would otherwise reconcile one durable against each other, the second undoing what
// the first had just done, and two polling at once would both take its push binding.
//
// The name carries the durable because that, and only that, is what the exclusion is about: two
// processes over one durable. One name over both delivery durables coupled two independent
// reconciliations, and INFRA-66 measured what that cost against a real nats-server and the real
// envoy-dispatch binary, with the Dispatch user denied exactly
// $JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.delivery-events.> (envoy-infra's live row "without
// the delivery consumer create Dispatch leaves only that intake unbound"). A denied AddConsumer
// draws no reply at all, so the create blocks for the client's whole JetStream MaxWait -- 10 s
// (internal/bus/nats.go, nats.MaxWait) -- and the lease is held for every second of it. A pass
// that failed is deliberately left unthrottled (bindLeasedDurable records no pass then), so the
// events intake took the lease again on its next durableBindInterval poll and held it for another
// 10 s, round after round:
//
//	17:57:16.232 ERROR dispatch delivery: create NATS consumer name=delivery-events error="context deadline exceeded"
//	17:57:16.232 INFO  dispatch duty: leased pass ran  lease=delivery-consumers
//	17:57:26.235 ERROR dispatch delivery: create NATS consumer name=delivery-events error="context deadline exceeded"
//	17:57:26.236 INFO  dispatch duty: leased pass ran  lease=delivery-consumers
//
// Under that one name the job intake, whose own durable is created by a pass under the same lease,
// could only get in during the milliseconds between those holds: in CI's run of that row it never
// did, and the row failed with "consumer delivery-jobs was not push-bound within 20s". A lease per
// durable keeps a stuck reconciliation to the durable it is stuck on.
func consumerLease(durable string) string { return "delivery-consumer-" + durable }

// bindOutcome is what one attempt at a durable did, which is three things rather than two: a
// durable another process holds, or has not created yet, is neither a bind nor a fault.
type bindOutcome int

const (
	// bindFailed is an attempt that got nowhere and says nothing about who holds the durable: the
	// lookup, the create, the update or the subscribe failed, and the failure is logged at ERROR;
	// or the server never reported the subscription bound, which is logged at WARN and given up;
	// or ctx ended before the subscribe, which a stopping process does not make, and which says
	// nothing. The zero value, so an outcome nothing set is never read as a bind.
	bindFailed bindOutcome = iota
	// bindBound is this process holding the durable's push binding, with its subscription live.
	bindBound
	// bindStandby is the durable not being this process's to bind on this poll, for a reason
	// that is about the durable rather than about this process: another process is bound to it
	// (the server reports it PushBound, or NATS refuses the bind, consumerHeldElsewhere), it does
	// not exist yet (natsgo.ErrConsumerNotFound, the leased reconciliation not having created
	// it), or another process is inside its bind pass (standbyLeaseHeldMessage). None is an
	// error: the caller polls on, and binds once the durable is free and there.
	bindStandby
)

// consumerHeldElsewhere reports whether err is NATS refusing a bind because the push durable
// already has a subscription on it ("consumer is already bound to a subscription"): the one
// refusal that means another process holds the durable rather than something being wrong with it.
// Matched on the message because the server sends it as a plain error with no code the client
// surfaces -- cmd/listener/durable.go and cmd/listener/main.go rely on the same string.
func consumerHeldElsewhere(err error) bool {
	return err != nil && strings.Contains(err.Error(), "consumer is already bound")
}

// bindThrottle is one intake's own bind-poll state for its own durable: when this process last
// reconciled that durable's configuration, and when it last said it was standing by for it.
//
// Per intake rather than a package-global map keyed by consumer name, because an intake is
// exactly what these windows are about -- one process's repeated bind polls of one durable --
// and a global outlives the durable it throttles: a second intake of the same name (a test's, or
// a process that reconnected to another NATS account) would skip the reconciliation its own
// durable has never had and never create it, and would stand by silently for a durable it has
// said nothing about.
type bindThrottle struct {
	mu sync.Mutex
	// reconciled is when this intake's last reconciliation pass over its durable was recorded.
	reconciled time.Time
	// standby is when the last standby line was logged and standbyReason the message it carried,
	// so a change of state -- the durable going from not created yet to held by another process
	// -- is said at once rather than waiting out the window of the line before it.
	standby       time.Time
	standbyReason string
}

// consumerReconcileInterval is the least time between two reconciliations of one durable's
// configuration by one intake (bindThrottle.reconcileDue): 30 s, the period the intake once polled
// its settings at, which is how often the loop the bind poll replaced reconciled at all.
const consumerReconcileInterval = 30 * time.Second

// reconcileDue reports whether this durable's reconciliation is to run now: the first attempt is,
// and a later one within consumerReconcileInterval of the last pass recorded is not.
//
// Throttled because the reconciliation runs on every bind poll, and a standby polls every
// durableBindInterval (2 s) for the life of the process: each poll of each durable would
// otherwise take that durable's consumer lease (consumerLease) -- two Postgres round trips --
// and write a "dispatch duty: leased pass ran" line, 1,800 of each an hour per durable, for a
// configuration that changes only with a release. The poll itself stays at durableBindInterval,
// which is what makes a hand-over from a dead holder fast.
func (t *bindThrottle) reconcileDue() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.reconciled.IsZero() || time.Since(t.reconciled) >= consumerReconcileInterval
}

// recordReconcile records a pass over this durable, which throttles the next
// consumerReconcileInterval of attempts.
func (t *bindThrottle) recordReconcile() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.reconciled = time.Now()
}

// reportStandby logs message at INFO -- this durable is not this process's to bind yet, for the
// reason message names -- unless this intake logged that same message within standbyLogEvery.
func (t *bindThrottle) reportStandby(name, message string) {
	t.mu.Lock()
	if t.standbyReason == message && time.Since(t.standby) < standbyLogEvery {
		t.mu.Unlock()
		return
	}
	t.standby, t.standbyReason = time.Now(), message
	t.mu.Unlock()
	slog.Info(message, "name", name)
}

// bind creates (on first run) or resumes (on a restart) the delivery durable and subscribes to it
// (subscribe, at the deliver subject the durable carries), throttling its own reconciliation and
// standby line on this intake's own bindThrottle. stopping is Run's stop deadline
// (bindDurableConsumer).
//
// The undo it hands back is StopDelivering and not a bare Unsubscribe: a bind the server never
// confirms is given up whole, which for this intake means draining the subscription, giving up
// the client registration a reconnect would otherwise bind again, and stopping the keyed runner
// whose goroutines are handling what the subscription was already handed.
func (in *Intake) bind(ctx, stopping context.Context) bindOutcome {
	_, outcome := bindDurableConsumer(ctx, stopping, in.nats, in.leases, in.throttle, deliveryConsumerConfig(), func() (*natsgo.Subscription, func(), error) {
		sub, err := in.subscribe("")
		if err != nil {
			return nil, nil, err
		}
		if in.boundHook != nil {
			in.boundHook()
		}
		return sub, func() { in.StopDelivering(stopping) }, nil
	})
	return outcome
}

// bindDurableConsumer creates (on first run) or resumes (on a restart) a durable and subscribes to
// it through subscribe, and reports which of three things happened: this process bound it, another
// process holds it (bindStandby), or the attempt failed (bindFailed).
//
// The invariant: one process subscribes to a delivery durable at a time, and no process subscribes
// to one the server already reports bound. What excludes the second subscriber is this durable's
// own consumer lease (consumerLease), which pg_try_advisory_lock makes exclusive, and not NATS.
// It holds among processes of this release: a process of an earlier release running beside it
// during a rolling deploy still subscribes outside the lease. A holder whose NATS connection drops
// would come back subscribed outside this function -- by nats.go re-sending the subscription
// inside its own reconnect, or by the client binding its registration again on a replacement
// connection (internal/bus/recovery.go) -- so it gives that subscription up at the connection
// event instead (Intake.Run, giveUpOnConnectionChange) and takes the durable again only through
// this function, under the lease, or not at all.
// The "consumer is already bound" refusal (consumerHeldElsewhere) is the client's own, made only
// once ConsumerInfo reports PushBound, and the server sets PushBound a goroutine hop after the
// first subscriber's SUB lands: a bind that gave the lease back before it subscribed left that hop
// open, and two processes polling one durable at once both took the binding and both delivered
// every message of it (TestTwoProcessesPollingOneDurableNeverBothBindIt). So the whole bind
// attempt runs under the lease -- the reconciliation, the read of whether the durable is free, the
// subscribe, and the confirmation that the server now reports the durable bound
// (consumerBindConfirmTimeout) -- and PushBound is made truthful before the lease goes back. A
// binding the confirmation does not get is given up through the unbind that subscribe handed back,
// which is why a subscriber hands one back at all: a bind is all of itself or none of itself, and
// only the caller knows how its own subscription is given up.
//
// The lease covers one bind attempt and not the life of the binding: it is given back as this
// function returns, bound or not. Nothing a process holds afterwards depends on Postgres, and a
// holder that dies leaves no lease for anyone to wait out. What a standby waits on then is the
// server releasing the dead holder's interest in the durable, which it finds on its next
// durableBindInterval poll -- the hand-over is as fast as it was, the server's own speed plus one
// poll (TestAHolderThatStopsIsReplacedWithinTheBoundTheCodeStates).
//
// A poll with no reconciliation owed -- fourteen of a standby's fifteen, durableBindInterval
// against consumerReconcileInterval (throttle, the caller's own bindThrottle) -- asks the server
// that one question before it takes any lease, and stands by on it: a durable another process is
// bound to, or one that does not exist yet, costs one NATS round trip, no Postgres and no
// subscribe. That read is the lookup the subscribe itself used to make on every poll, so a
// standby's steady-state NATS cost is one round trip per durableBindInterval, exactly what it was,
// while its Postgres cost stays where it was: one lease per consumerReconcileInterval, which is
// the only window the lease was ever taken in, plus one on each poll that gets past this read
// with the durable there and free -- the whole hand-over window after a holder dies. Only such a
// poll goes on to the leased pass, which reads the durable again: the state can have changed
// between this read and the take, and a process that held the lease until a moment ago is the
// likeliest one to have bound it.
//
// The configuration reconciliation (reconcileDurableConsumer) runs in that same leased pass, so
// one process at a time reconciles a durable -- two builds during a rolling update do not
// reconcile one durable against each other -- and a durable whose reconciliation is stuck blocks
// no other durable's. A standby makes that pass as much as a holder does: it takes the lease every
// consumerReconcileInterval whether the durable is free or not, so the in-place correction of a
// durable another process holds (the UpdateConsumer branch: an earlier release's filter or flow
// control) still happens during a rolling deploy, from the process that is standing by.
//
// A durable that does not exist yet is the other refusal a caller waits out rather than fails on
// (natsgo.ErrConsumerNotFound): the reconciliation that creates it is leased, so on a fresh bus
// exactly one process creates the durable and another polling meanwhile asks the server for a
// durable whose creation has not landed yet. That is a "not yet", like the standby above, not a
// fault in this process -- so it polls on at durableBindInterval and binds the durable as soon as
// it is there. It subscribes through client.Subscribe (subscribeDurable), which refuses a second
// durable on one bus.Client (bus.ErrSubscriptionHeld), so each durable bound here takes a client
// of its own.
//
// A bind under way when ctx ends -- the caller stopping -- ends by stopping's end, its caller's
// stop deadline (stopContext): the reconciliation's JetStream calls and the confirmation run under
// stopping, each within its own request timeout until then, and no subscribe starts once ctx has
// ended. The confirmation's own bound, consumerBindConfirmTimeout, is well inside
// intakeStopBudget, so a bind does not hold its caller's stop past that deadline.
func bindDurableConsumer(ctx, stopping context.Context, client *bus.Client, leases duty.Leases, throttle *bindThrottle, wanted natsgo.ConsumerConfig, subscribe func() (sub *natsgo.Subscription, unbind func(), err error)) (*natsgo.Subscription, bindOutcome) {
	name := wanted.Durable
	// A stopping process binds nothing: the subscription would only take the durable from the
	// process taking over, be handed messages and give them straight back.
	if ctx.Err() != nil {
		return nil, bindFailed
	}
	if !throttle.reconcileDue() {
		if outcome, decided := standbyForDurable(stopping, client.JS(), throttle, name); decided {
			return nil, outcome
		}
	}
	// The pass records what it did in these, because duty.Exclusive hands its pass's answer
	// nowhere and its own answer is not that answer: it reports false for a pass that ran and
	// ended after ctx did, which a bind its subscriber holds open until the stop is
	// (TestABindThatReturnsAfterTheSignalStopsByTheBudgetFromTheSignal). So ran, which the pass
	// itself sets, is what says whether the pass was made at all.
	//
	// No period (0): this is not a periodic loop but a step of every bind poll, and what keeps it
	// off most of them is the throttle above. So the lease is asked for mutual exclusion and
	// nothing else, and no tick is due (the zero time).
	var sub *natsgo.Subscription
	var outcome bindOutcome
	var ran bool
	duty.Exclusive(ctx, leases, consumerLease(name), time.Time{}, 0, func(context.Context) {
		ran = true
		sub, outcome = bindLeasedDurable(ctx, stopping, client, throttle, wanted, subscribe)
	})
	if ran {
		return sub, outcome
	}
	if ctx.Err() != nil {
		return nil, bindFailed
	}
	// Not the "held by another process" line above, and deliberately a different sentence: that
	// one means the server reports the durable bound to a subscriber, while this one means only
	// that this poll did not get the durable's lease -- another process is inside this durable's
	// bind pass right now, or Postgres refused the take, which duty.Exclusive has logged at ERROR.
	// Either way the durable may well still be free -- the other pass's reconciliation failed,
	// its own subscribe was refused -- and this process binds it on its next poll.
	throttle.reportStandby(name, standbyLeaseHeldMessage)
	return nil, bindStandby
}

// standbyForDurable asks the server whether a durable is this process's to take and reports the
// outcome to answer the poll with when it is not: a durable with a subscriber on it, or one that
// does not exist yet, is a standby (reported on throttle), and a lookup that failed is a
// bindFailed logged at ERROR. decided is false for the one answer that is not an outcome -- the
// durable is there and free -- which is its caller's to act on.
func standbyForDurable(ctx context.Context, js natsgo.JetStreamContext, throttle *bindThrottle, name string) (outcome bindOutcome, decided bool) {
	bound, err := durablePushBound(ctx, js, name)
	switch {
	case errors.Is(err, natsgo.ErrConsumerNotFound):
		// Matched with errors.Is rather than on the message, since nats.go exports the sentinel
		// for a durable the server does not have.
		throttle.reportStandby(name, standbyConsumerMissingMessage)
		return bindStandby, true
	case err != nil:
		slog.Error("dispatch delivery: look up NATS consumer", "name", name, "error", err)
		return bindFailed, true
	case bound:
		throttle.reportStandby(name, standbyConsumerHeldMessage)
		return bindStandby, true
	}
	return bindFailed, false
}

// bindLeasedDurable is one bind attempt under this durable's consumer lease, which its caller
// holds for every step of it: the reconciliation this process still owes, the read of whether the
// durable is free, the subscribe, and the confirmation that the server reports the durable bound
// before the lease goes back (bindDurableConsumer). Its caller keeps what it answers, since
// duty.Exclusive takes no return value from its pass.
func bindLeasedDurable(ctx, stopping context.Context, client *bus.Client, throttle *bindThrottle, wanted natsgo.ConsumerConfig, subscribe func() (*natsgo.Subscription, func(), error)) (*natsgo.Subscription, bindOutcome) {
	name, subject := wanted.Durable, wanted.FilterSubject
	if throttle.reconcileDue() {
		if !reconcileDurableConsumer(stopping, client.JS(), wanted) {
			// Nothing recorded: a pass that ran and failed is made again on the next poll rather
			// than skipped for consumerReconcileInterval. The window is how long this durable's
			// configuration is taken as settled, and a failed pass settled nothing -- a durable
			// the holder's own pass did not create is then created by whichever process polls
			// next, instead of every process standing by for a durable nobody is making.
			return nil, bindFailed
		}
		throttle.recordReconcile()
	}
	if ctx.Err() != nil {
		return nil, bindFailed
	}
	// Read again under the lease: the read in front of the lease was made before this process
	// took it, and a process that held it until a moment ago is the likeliest one to have bound
	// the durable meanwhile. A poll whose reconciliation was due made no such read at all.
	if outcome, decided := standbyForDurable(stopping, client.JS(), throttle, name); decided {
		return nil, outcome
	}
	sub, unbind, err := subscribe()
	switch {
	case consumerHeldElsewhere(err):
		throttle.reportStandby(name, standbyConsumerHeldMessage)
		return nil, bindStandby
	case errors.Is(err, natsgo.ErrConsumerNotFound):
		// Matched with errors.Is rather than on the message, since nats.go exports the sentinel
		// its own subscribe returns for a bind of a durable the server does not have.
		throttle.reportStandby(name, standbyConsumerMissingMessage)
		return nil, bindStandby
	case err != nil:
		slog.Error("dispatch delivery: bind NATS consumer", "name", name, "subject", subject, "error", err)
		return nil, bindFailed
	}
	bound, confirmErr := awaitDurablePushBound(stopping, client.JS(), name)
	if !bound {
		// The lease is about to go back, and with it the only thing keeping another process out of
		// this durable, so a binding the server does not report is given up whole rather than left
		// to deliver beside whichever process binds next. The next poll tries again.
		unbind()
		given := []any{"name", name, "subject", subject, "within", consumerBindConfirmTimeout}
		if confirmErr != nil {
			// Only when a read in fact failed: reads that all answered "not bound" have no cause
			// to name, and an empty one on the line would read as though they had.
			given = append(given, "error", confirmErr)
		}
		slog.Warn("dispatch delivery: the server did not report the NATS consumer bound; subscription given up", given...)
		return nil, bindFailed
	}
	slog.Info("dispatch delivery: NATS consumer bound", "name", name, "subject", subject)
	return sub, bindBound
}

// awaitDurablePushBound waits for the server to report the durable push-bound, reading it at once
// and then every consumerBindConfirmInterval until it does, consumerBindConfirmTimeout has passed,
// or ctx ends. A read that fails is left for the next read to settle: the only answer it acts on
// is the true one, and a bound it never gets is its caller's to give the subscription up for.
//
// Beside the bool it hands back the last error a read failed with, so the caller giving the
// subscription up can say what the reads met rather than only that they never said bound. It is
// nil when every read answered "not bound" with no error of its own, and never the confirming
// context's own expiry: an error read once that context is done is this function's own bound
// arriving, not something the durable's lookup met.
func awaitDurablePushBound(ctx context.Context, js natsgo.JetStreamContext, name string) (bool, error) {
	confirming, cancel := context.WithTimeout(ctx, consumerBindConfirmTimeout)
	defer cancel()
	reads := time.NewTicker(consumerBindConfirmInterval)
	defer reads.Stop()
	var last error
	for {
		bound, err := durablePushBound(confirming, js, name)
		switch {
		case err == nil && bound:
			return true, nil
		case err != nil && confirming.Err() == nil:
			last = err
		}
		select {
		case <-confirming.Done():
			return false, last
		case <-reads.C:
		}
	}
}

// reconcileDurableConsumer brings a durable's configuration to wanted and reports whether it
// could: it creates the durable when there is none, replaces one an earlier release left carrying
// a filter-subject set, and corrects a filter or flow control in place. It checks ConsumerInfo
// before AddConsumer, like cmd/listener/durable.go's startListenerSubscription: a durable's
// DeliverSubject is a random inbox chosen once at creation, and calling AddConsumer again with a
// freshly generated one on every restart would try to change a durable's config out from under
// itself rather than resuming its cursor.
//
// Its caller runs it under that durable's consumer lease (consumerLease), so one process at a
// time is in here for one durable.
//
// PushBound is read once, by the ConsumerInfo immediately before the delete, and that read
// settles the question for the whole pass among processes of this release: each of them
// subscribes only under this same lease (bindDurableConsumer), by a pass that reads PushBound
// again after taking it, so none of them can bind the durable between that read and the delete.
// The read is kept close to the delete because the pass's first read is two round trips back
// and the holder it reported may have gone away since.
//
// Every JetStream call runs under ctx, each within the JetStream context's own request timeout
// while ctx carries no deadline: bindDurableConsumer passes its caller's stop deadline.
func reconcileDurableConsumer(ctx context.Context, js natsgo.JetStreamContext, wanted natsgo.ConsumerConfig) bool {
	name, subject := wanted.Durable, wanted.FilterSubject
	bounded := natsgo.Context(ctx)
	info, err := js.ConsumerInfo(bus.Stream, name, bounded)
	switch {
	case errors.Is(err, natsgo.ErrConsumerNotFound):
		if _, err := js.AddConsumer(bus.Stream, &wanted, bounded); err != nil {
			slog.Error("dispatch delivery: create NATS consumer", "name", name, "subject", subject, "error", err)
			return false
		}
	case err != nil:
		slog.Error("dispatch delivery: look up NATS consumer", "name", name, "error", err)
		return false
	case len(info.Config.FilterSubjects) > 0:
		// A durable an earlier release left carrying a filter-subject set, which withholds the
		// workflow runs this slice exists to record (githubIntakeSubject). It is replaced rather
		// than updated in place, and replaced at its own ack floor, so the replacement neither
		// replays what the old durable acknowledged nor skips what it had not
		// (TestBindReplacesAPluralFilterDurableAtItsOwnAckFloor).
		//
		// Re-read the durable immediately before the delete: the read above is two round trips
		// back, and the holder it reported may have gone away since -- or, during a rolling
		// deploy, a process of an earlier release, which binds without taking the lease, may have
		// bound it. The ack floor comes from this read for the same reason -- it is the last
		// thing the durable acknowledged before the delete.
		//
		// That fresh read is the only thing that decides whether another process is bound to
		// this durable: one question, answered once, by the read closest to the delete, so the
		// standby line below reports a state no read of this pass contradicts. It also decides
		// it in the direction of acting: a durable whose binder went away between the two reads
		// is replaced on this pass rather than left plural for another consumerReconcileInterval,
		// which is what the second read was added for. The cost is the ConsumerInfo itself, one
		// per durable per pass the throttle admits (reconcileDue), not one per bind poll.
		bound, err := js.ConsumerInfo(bus.Stream, name, bounded)
		switch {
		case err != nil:
			slog.Error("dispatch delivery: look up NATS consumer", "name", name, "error", err)
			return false
		case bound.PushBound:
			slog.Info("dispatch delivery: another process is bound to the NATS consumer; leaving its configuration alone", "name", name)
			return true
		}
		recreate := wanted
		recreate.DeliverPolicy = natsgo.DeliverByStartSequencePolicy
		recreate.OptStartSeq = bound.AckFloor.Stream + 1
		if err := js.DeleteConsumer(bus.Stream, name, bounded); err != nil {
			slog.Error("dispatch delivery: replace multi-subject NATS consumer", "name", name, "error", err)
			return false
		}
		if _, err := js.AddConsumer(bus.Stream, &recreate, bounded); err != nil {
			slog.Error("dispatch delivery: create replacement NATS consumer", "name", name, "start_seq", recreate.OptStartSeq, "error", err)
			return false
		}
		slog.Info("dispatch delivery: multi-subject NATS consumer replaced", "name", name, "subject", subject, "start_seq", recreate.OptStartSeq)
	case info.Config.FilterSubject != wanted.FilterSubject ||
		info.Config.AckWait != wanted.AckWait ||
		info.Config.MaxAckPending != wanted.MaxAckPending:
		// The durable carries an earlier release's filter or flow control: update it in place,
		// preserving its delivery cursor, so this needs no operator to notice a log line and
		// restart the server. Every field here is one NATS lets an existing consumer change, on a
		// consumer another process is bound to as much as on a free one; nothing touches its
		// deliver policy or ack policy, which it does not.
		config := info.Config
		config.FilterSubject = wanted.FilterSubject
		config.AckWait = wanted.AckWait
		config.MaxAckPending = wanted.MaxAckPending
		if _, err := js.UpdateConsumer(bus.Stream, &config, bounded); err != nil {
			slog.Error("dispatch delivery: update NATS consumer policy", "name", name, "configured_subject", info.Config.FilterSubject, "wanted_subject", wanted.FilterSubject, "error", err)
			return false
		}
		slog.Info("dispatch delivery: NATS consumer policy updated", "name", name, "subject", wanted.FilterSubject, "ack_wait", wanted.AckWait, "max_ack_pending", wanted.MaxAckPending)
	}
	return true
}

// subscribeDurable subscribes deliver to wanted's durable through client.Subscribe, acknowledging
// by hand. With no deliverSubject, nats.go looks the durable up and subscribes at the deliver
// subject the durable carries, refusing a durable another subscriber holds ("consumer is already
// bound"). With one, it subscribes at deliverSubject without the lookup: for a caller that has just
// created the durable with that deliver subject, under its consumer lease.
func subscribeDurable(client *bus.Client, wanted natsgo.ConsumerConfig, deliverSubject string, deliver natsgo.MsgHandler) (*natsgo.Subscription, error) {
	options := []natsgo.SubOpt{natsgo.Bind(bus.Stream, wanted.Durable), natsgo.ManualAck()}
	if deliverSubject != "" {
		options = append(options, natsgo.DeliverSubject(deliverSubject), natsgo.SkipConsumerLookup())
	}
	// The subject must be the consumer's own filter subject: nats.go checks a bound
	// subscription's subject against it, and refuses an empty one ("subject does not match
	// consumer").
	return client.Subscribe(wanted.FilterSubject, deliver, options...)
}

// deliver handles one message on its key's goroutine: it routes the envelope's payload by the
// settings the cache holds under intakeMessageTimeout, and answers it through the delivery, which
// acknowledges it and holds that acknowledgement for the stop only where the bus refused it
// (acknowledgeOrHold): a malformed envelope has nothing to retry, and a route error (a failed
// GitHub call, or that deadline passing) is caught up by the next reconcile pass rather than by
// NATS redelivery -- see the package doc comment. One route error is not, and is the second
// case below. The deadline is what makes intakeAckWait a
// bound rather than a hope: without it one stuck GitHub call could hold an in-flight slot past
// the ack wait and have the message redelivered underneath it. An envelope route does not handle
// (an empty payload, or a kind route sends to no handler) is acknowledged and nothing else: no
// log line and no freshness write. One it handles records the event's time, whatever its handler
// decided.
//
// The one message it does not acknowledge is one whose handling the stop gave up (handling
// cancelled, StopDelivering): that is handed back through the delivery (giveBack), so the next
// holder of the durable redoes it at once rather than this event waiting five minutes for the
// reconcile. Its writes are the upserts reconcile makes too, so redoing what had in fact
// finished just before the stop gave it up writes the same facts again.
//
// The other message it does not acknowledge, for as many deliveries as the bound below
// allows, is one whose pull request's head row could not be stored at all (ErrHeadNotStored,
// handlePullRequestEnvelope): no reconcile pass catches that up, because the passes read the
// rows that exist (ListStalePullRequestHeads), so an acknowledged envelope loses that pull
// request's head for good and with it every `before_merge` result measured against it. It is
// handed straight back to NATS rather than through the delivery (redeliverAfter): the hold
// giveBack takes is for the stop, and this message wants redelivering now, to this same
// process, which still holds the durable and still routes that key to the one goroutine it is
// on. The delay is what keeps a failure that repeats from spinning: the write is the same
// write each time, so asking again within the second would buy nothing. The write is
// idempotent (both its statements are `on conflict … do update`), so a redelivery of an
// envelope whose write had in fact landed writes the same pair again.
//
// It is handed back a bounded number of times and then acknowledged, with an Error naming the
// envelope and the deliveries it spent (headWriteMaxDeliveries): a message handed back with a
// delay stays in the consumer's ack-pending set for the whole of that delay, so a write that
// fails the same way every time would hold one of the four in-flight slots
// (intakeMaxAckPending) for ever. The count is the server's own, read off the message
// (Msg.Metadata, NumDelivered), so it is the count of this envelope's deliveries and not of
// this process's attempts at it. A message carrying no such metadata is not a JetStream
// message, so no count can cap it; it is handed back rather than acknowledged, since the
// whole argument of this path is that nothing else writes that row, and a hand-back the ack
// wait bounds is the safer of the two answers.
//
// The stop's two answers go through the delivery rather than straight to NATS, and for one
// reason: the
// stop a connection event runs begins with the link down, where the bus refuses a publish. A
// hand-back the delivery holds until the stop has a live connection and makes it then
// (giveBack); an acknowledgement is made at once and held only where the bus refused it
// (acknowledgeOrHold), since a message whose handling is done holds one of the next holder's
// in-flight slots for the whole ack wait while it is neither acknowledged nor held, and holding
// one the bus took protects nothing. handling is read from the delivery too, and nothing else
// calls deliver.
func (in *Intake) deliver(d *intakeDelivery, message intakeMessage) {
	msg, handling := message.msg, d.handling
	// handBack is set by the one route error NATS must redeliver, which the deferred answer
	// below reads: every other path acknowledges.
	handBack := false
	defer func() {
		if handling.Err() != nil {
			slog.Warn("dispatch delivery: the intake stopped before the event's handling finished; handing it back", "subject", msg.Subject)
			d.giveBack(msg)
			return
		}
		if handBack {
			redeliverAfter(msg, headWriteRedeliverAfter)
			return
		}
		d.acknowledgeOrHold(msg)
	}()
	ctx, cancel := context.WithTimeout(handling, intakeMessageTimeout)
	defer cancel()

	envelope := message.envelope
	if envelope == nil {
		decoded, err := decodeEnvelope(msg.Data)
		if err != nil {
			slog.Error("dispatch delivery: decode envelope", "subject", msg.Subject, "error", err)
			return
		}
		envelope = &decoded
	}
	settings := in.settings.Load()
	if settings == nil {
		return
	}
	handled, err := in.route(ctx, *settings, envelope.Payload)
	switch {
	case errors.Is(err, errMalformedPayload):
		slog.Error("dispatch delivery: decode envelope payload", "subject", msg.Subject, "error", err)
		return
	case errors.Is(err, ErrHeadNotStored):
		delivered, spent := headWriteDeliveriesSpent(msg)
		if spent {
			slog.Error("dispatch delivery: a pull request's head row could not be stored in the deliveries allowed; acknowledging the envelope, whose pull request now has no head row until a later `pull_request` event for it, or the read-through a recorded result makes, writes one",
				"subject", msg.Subject, "deliveries", delivered, "error", err)
			return
		}
		slog.Warn("dispatch delivery: a pull request's head row was not stored; handing the envelope back",
			"subject", msg.Subject, "delivery", delivered, "deliveries_allowed", headWriteMaxDeliveries,
			"redeliver_after", headWriteRedeliverAfter, "error", err)
		handBack = true
		return
	case err != nil:
		slog.Warn("dispatch delivery: process event", "subject", msg.Subject, "error", err)
		return
	case !handled:
		return
	}
	if err := RecordEventAt(ctx, in.pool); err != nil {
		slog.Error("dispatch delivery: record event freshness", "error", err)
	}
}

// headWriteRedeliverAfter is how long NATS waits before handing back an envelope whose pull
// request's head row could not be stored (deliver). It is not zero because the write that
// failed is the same write each time: a plain Nak would have the server redeliver at once, and
// a database that is refusing writes would then spin the consumer against it as fast as the
// round trip allows.
//
// What the delay does not do is give the in-flight slot back. A delayed Nak leaves the message
// in the consumer's ack-pending set — nats-server's processNak moves that pending entry's
// timestamp and nothing else — so one such envelope holds one of the four slots
// (intakeMaxAckPending) for every second it is waiting to come round again, and four of them
// hold all four. Bounding that is headWriteMaxDeliveries' work, not this constant's: ten
// seconds only sets the rate, which is short beside the five minutes of the reconcile this
// message cannot wait for, and long enough that the cap's whole run of redeliveries is
// measured in minutes rather than in round trips.
const headWriteRedeliverAfter = 10 * time.Second

// headWriteMaxDeliveries is how many deliveries one envelope whose head row could not be
// stored gets before deliver acknowledges it and logs the loss. Without it there is no bound
// at all: the durable sets no MaxDeliver, and a delayed Nak holds the message in the
// consumer's ack-pending set, so a write that fails the same way every time is redelivered
// every headWriteRedeliverAfter for ever and holds an in-flight slot throughout. Four such
// envelopes stop the intake outright, and everything behind them — every other pull request
// and every workflow run — waits on a row nothing was ever going to write.
//
// Thirty is where the two costs it sits between cross. Too few, and a database away for a
// minute loses a head row that nothing else repairs: a minute is six deliveries at
// headWriteRedeliverAfter, and an outage is counted in minutes rather than in seconds, so a
// cap of five or ten would throw away heads that waiting a little longer was always going to
// write. Too many, and four envelopes hold the consumer's whole in-flight window for longer
// than the thing that catches every other failure up. At thirty deliveries and a ten-second
// delay, one envelope's hand-backs are 29 × 10 s = 4 min 50 s of wall clock, plus what each
// refused attempt itself spends. A write Postgres refuses outright spends milliseconds, so
// the window is held for about five minutes — the reconcile's own period, which is already
// the longest any other failure of this intake waits. A database that hangs rather than
// refusing spends up to intakeMessageTimeout on each attempt instead, which is a minute, so
// thirty of them hold the window for about 35 minutes. That is the worst, and the cap is what
// bounds it: without one the window is held for ever. A database away for anything up to its
// own case's span still gets the head row written.
//
// A var so a test can shrink it, as intakeMessageTimeout and intakeMaxAckPending are vars for
// the same reason: thirty deliveries of one envelope is minutes of wall clock no test should
// spend.
var headWriteMaxDeliveries uint64 = 30

// headWriteDeliveriesSpent reports how many times the server has delivered msg and whether
// that is every delivery headWriteMaxDeliveries allows a head write to fail in.
//
// A message whose metadata cannot be read is not a JetStream message at all (Msg.Metadata
// answers an error for one with no JetStream reply subject). No count can be read from it and
// so no cap can be applied, and the answer is "not spent": the caller hands it back, which is
// the answer that keeps the head row's one chance of being written, and the ack wait bounds a
// hand-back the server never takes.
func headWriteDeliveriesSpent(msg *natsgo.Msg) (delivered uint64, spent bool) {
	meta, err := msg.Metadata()
	if err != nil {
		slog.Error("dispatch delivery: read how often the envelope whose head row was not stored has been delivered",
			"subject", msg.Subject, "error", err)
		return 0, false
	}
	return meta.NumDelivered, meta.NumDelivered >= headWriteMaxDeliveries
}

// redeliverAfter hands msg back to the server for redelivery in after — at once where after is
// zero, which is the same message to the server, since nats.go adds a delay to the negative
// acknowledgement only for a delay above zero (Msg.NakWithDelay, js.go) — and logs a hand-back
// the server refused: the ack wait (intakeAckWait) then redelivers it, later than asked but
// not never.
func redeliverAfter(msg *natsgo.Msg, after time.Duration) {
	if err := msg.NakWithDelay(after); err != nil {
		slog.Error("dispatch delivery: hand a message back for redelivery",
			"subject", msg.Subject, "delay", after, "error", err)
	}
}

// acknowledge acks msg. An acknowledgement that does not reach the server is never discarded: it
// leaves the message outstanding, and intakeMaxAckPending of those stop the consumer being given
// anything until the ack wait expires. The Nak gives the slot back at once; when it fails too (the
// connection is down, which is why the ack failed), the ack wait is the backstop.
func acknowledge(msg *natsgo.Msg) {
	if err := msg.Ack(); err != nil {
		slog.Error("dispatch delivery: acknowledge message", "subject", msg.Subject, "error", err)
		redeliverAfter(msg, 0)
	}
}

// handlePullRequestEnvelope processes one `kind:"pr"` envelope. Every action — opened,
// synchronize, reopened, edited and closed — records the pull request's head first
// (recordPullRequestHead, pull_request_heads.go), for any author and any repository, since a
// check result is measured against that head. A head write that could not be made at all is
// the one failure this hands the envelope back for (delivery.ErrHeadNotStored): nothing else
// repairs a head row that was never stored, since the reconcile's passes read the rows that
// exist, and both statements behind that write are `on conflict … do update`, so the
// redelivery writes the same pair. That covers the write itself and the two reads of Postgres
// before it, the repository's canonical name and the attribution (recordPullRequestHead).
// Two other failures of it are logged here and the merge handling goes on from them. A rate
// limit the attribution hit leaves no row either, and no pass offers a row that was never
// written, but it is acknowledged all the same: handing every rate-limited envelope back
// would spend the whole limit over again, and the next `pull_request` event of that pull
// request — a synchronize, a label, the merge — writes the row the limit cost this one. A
// repository name that is not `owner/repo` is malformed in the envelope itself, so the same
// envelope delivered again fails on the same name. A read of GitHub the App refused for any
// other reason is no failure of this write at all: the row is written from the envelope
// alone, incomplete and unstamped, and the reconcile's own pass reads it again.
// Only a merge (action "closed", merged "true") does anything for the delivery timeline
// after that; its author and excluded-repository checks run on the envelope's own fields, so a
// merge by anyone outside the population costs no GitHub call of its own. It reports whether
// it stored the pull request's delivery row: a head write is not one, and neither is a row it
// deleted or a pull request outside the population, so neither ships with anything.
func (in *Intake) handlePullRequestEnvelope(ctx context.Context, settings DeliverySettings, payload map[string]string) (wrote bool, err error) {
	if payload["kind"] != "pr" {
		return false, nil
	}
	repoFull := payload["repo"]
	number, err := strconv.Atoi(payload["number"])
	if err != nil {
		return false, fmt.Errorf("pull-request envelope: malformed number %q: %w", payload["number"], err)
	}
	// The commit messages the head write read, when it needed them, so the merge below pages
	// the same commits once rather than twice.
	messages, headErr := in.recordPullRequestHead(ctx, payload, number)
	if headErr != nil {
		slog.Warn("dispatch delivery: record a pull request's head",
			"repo", repoFull, "number", number, "error", headErr)
		if errors.Is(headErr, ErrHeadNotStored) {
			return false, headErr
		}
	}
	if payload["action"] != "closed" || payload["merged"] != "true" {
		return false, nil
	}
	author := payload["author"]

	if containsString(settings.ExcludedRepos, repoFull) {
		return false, nil
	}
	if !containsString(settings.PopulationAuthors, author) {
		return false, nil
	}

	owner, repo, err := splitRepo(repoFull)
	if err != nil {
		return false, err
	}

	fetched, fetchErr := FetchPullRequest(ctx, in.github, owner, repo, number)
	if fetchErr != nil {
		// The population/task-label decision needs the fetch (labels aren't in the webhook
		// payload); without it we cannot tell whether this PR belongs in the population at all,
		// so nothing is written here. The reconcile's merged-PR search will find and complete
		// this PR on its own within five minutes.
		return false, fmt.Errorf("complete merged PR %s#%d: %w", repoFull, number, fetchErr)
	}
	if fetched.MergedAt == nil {
		return false, fmt.Errorf("merged-PR envelope for %s#%d, but GitHub reports it unmerged", repoFull, number)
	}

	raw := RawPullRequest{Repo: repoFull, Number: number, Title: fetched.Title, Author: fetched.Author, Labels: fetched.Labels, MergedAt: *fetched.MergedAt}
	isPopulation, err := IsPopulationPR(raw, settings, liveIntakeWindow())
	if err != nil {
		// IsTaskPR's "neither label" error: a deploy-repo PR GitHub's own classifier workflow has
		// not labelled yet. Surfaced, not guessed; the reconcile retries this PR's label on its
		// next pass.
		return false, fmt.Errorf("classify %s#%d: %w", repoFull, number, err)
	}
	if !isPopulation {
		return false, DeletePullRequest(ctx, in.pool, repoFull, number)
	}

	if err := completePullRequest(ctx, in.pool, in.github, owner, repo, repoFull, number, fetched, messages); err != nil {
		return false, err
	}
	return true, nil
}

// completePullRequest finishes writing one merged pull request's complete row -- session
// trailers, attribution inputs, issue resolution, and the upsert -- once its GitHub facts
// (FetchPullRequest's answer) are in hand. The shared tail both intake's live
// handlePullRequestEnvelope and reconcile's completePartialPullRequest call, so "a live webhook
// completes a PR" and "reconcile completes a partial row" write through the exact same last
// steps, never two copies that can drift from each other. A commit fetch that fails, for any
// reason, returns its error without writing anything: completing the row from what was read
// would record its sessions and commit-message keys short for good. The row stays as it was
// (still partial, for reconcile's own caller; unwritten, for intake's, whose merged-PR search then
// finds it), so the next pass retries the whole completion. A rate limit comes back as the
// *githubapp.RateLimitError, which stops reconcile's batch.
//
// read is the pull request's commit messages when the caller has already paged them — intake's
// head write does, for a pull request its title, body and branch left unattributed — and nil
// when it has not, which is reconcile's case and intake's commoner one; a non-nil read spares
// this the same paginated call a second time.
func completePullRequest(ctx context.Context, pool *store.Pool, github *githubapp.Client, owner, repo, repoFull string, number int, fetched FetchedPullRequest, read []string) error {
	messages := read
	if messages == nil {
		paged, err := fetchCommitMessages(ctx, github, owner, repo, number)
		if err != nil {
			if limited, ok := githubapp.AsRateLimit(err); ok {
				return limited
			}
			return fmt.Errorf("complete %s#%d: %w", repoFull, number, err)
		}
		messages = paged
	}
	inputs := attributionInputsFrom(attributionFacts{
		Repo: repoFull, URL: fetched.URL, Title: fetched.Title, Body: fetched.Body,
		HeadRef: fetched.HeadRef, CommitMessages: messages,
	})
	issueKey, _, err := resolveStoredIssueKey(ctx, pool, repoFull, fetched.URL, inputs)
	if err != nil {
		return err
	}
	return UpsertPullRequest(ctx, pool, DeliveryPullRequest{
		Repo: repoFull, Number: number, Title: fetched.Title, URL: fetched.URL, Author: fetched.Author,
		CreatedAt: &fetched.CreatedAt, MergedAt: fetched.MergedAt, FirstCommitAt: fetched.FirstCommitAt,
		MergeCommitSHA: fetched.MergeCommitSHA, Additions: fetched.Additions, Deletions: fetched.Deletions,
		Rework: IsRework(fetched.Title), IssueKey: issueKey, Sessions: sessionTrailers(messages),
		Attribution: &inputs, Partial: false,
	})
}

// liveIntakeWindow is the window IsPopulationPR is checked against for a live merge event. The
// 28-day rolling window is the reconcile backfill's concern (which historical PRs to import), not
// a live event's: a merge event is, by construction, happening now, so this window is wide enough
// to always contain it and exists only to satisfy IsPopulationPR's signature.
func liveIntakeWindow() TimeWindow {
	return TimeWindow{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}
}

// handleWorkflowEnvelope processes one `kind:"workflow"` envelope for the workflow bound to kind
// (the deploy workflow or the PR-checks workflow, whichever subject this consumer was bound to).
// The envelope's repo/run_id are used only to know which run to ask GitHub about: every stored
// field (head SHA, timing, conclusion, URL, PR number) comes from FetchWorkflowRun's own answer,
// never trusted from the envelope directly -- an Envoy /v1 bearer (any agent session) can publish
// an arbitrary envelope naming any repo/run_id/conclusion/timing, and this is the one place a
// forged value could otherwise rewrite a real run's stored facts. A run row is written whatever
// its current state (so an in-progress run appears before it concludes, per CONTRACT.md); jobs
// are fetched and written only once GitHub reports the run concluded. It reports whether it stored
// the run, with or without its jobs.
func (in *Intake) handleWorkflowEnvelope(ctx context.Context, settings DeliverySettings, kind DeliveryRunKind, payload map[string]string) (wrote bool, err error) {
	if payload["kind"] != "workflow" {
		return false, nil
	}
	repoFull := payload["repo"]
	owner, repo, err := splitRepo(repoFull)
	if err != nil {
		return false, err
	}
	runID, err := strconv.ParseInt(payload["run_id"], 10, 64)
	if err != nil {
		return false, fmt.Errorf("workflow envelope: malformed run_id %q: %w", payload["run_id"], err)
	}

	fetched, err := FetchWorkflowRun(ctx, in.github, owner, repo, runID)
	if err != nil {
		return false, fmt.Errorf("verify run %d against GitHub: %w", runID, err)
	}

	if err := UpsertRun(ctx, in.pool, StoredRun(repoFull, kind, fetched)); err != nil {
		return false, fmt.Errorf("upsert run %d: %w", runID, err)
	}

	if fetched.CompletedAt == nil {
		return true, nil
	}
	fetchedJobs, err := ListWorkflowRunJobs(ctx, in.github, owner, repo, runID)
	if err != nil {
		// The run row is already written; the reconcile's own run listing fetches this run's
		// jobs too and will complete them.
		return true, fmt.Errorf("fetch jobs of run %d: %w", runID, err)
	}
	jobs := RunJobs(repoFull, runID, fetchedJobs, settings.Production())
	return true, UpsertRunJobs(ctx, in.pool, repoFull, runID, jobs, JobsComplete(jobs))
}

// commitMessagePayload is one element of GET /repos/{owner}/{repo}/pulls/{number}/commits,
// limited to the message field fetchCommitMessages reads (distinct from github_prs.go's
// commitPayload, which reads the same endpoint's dates instead).
type commitMessagePayload struct {
	Commit struct {
		Message string `json:"message"`
	} `json:"commit"`
}

// maxCommitPages bounds fetchCommitMessages' pagination: 500 commits is already an enormous pull
// request, and a PR beyond that is not worth the API cost of chasing its full history.
const maxCommitPages = 5

// fetchCommitMessages reads a pull request's commit messages, in commit order: what its session
// trailers (sessionTrailers) and its commit-message issue keys (AttributionInputs.CommitKeys) are
// read from. Shared by Intake and Reconcile (both complete a PR the same way; reconcile must read
// them itself rather than carrying forward whatever a stale row already had, see store.go's
// UpsertPullRequest doc comment on why a partial row is never allowed to regress a complete one's
// attribution). On an error it answers the messages of the pages it read.
func fetchCommitMessages(ctx context.Context, client *githubapp.Client, owner, repo string, number int) ([]string, error) {
	token := func() (string, error) { return client.RepositoryToken(ctx, owner, repo) }
	messages, _, err := fetchCommitMessagesWithToken(ctx, client, token, owner, repo, number)
	return messages, err
}

// fetchCommitMessagesWithToken is fetchCommitMessages asking token for each page's installation
// token, by readGitHubPage's rule. It also answers how many commit pages it asked GitHub for, the
// failed one included, so a caller can count what a read cost (reconcileAttributionInputs' call
// budget).
func fetchCommitMessagesWithToken(ctx context.Context, client *githubapp.Client, token func() (string, error), owner, repo string, number int) ([]string, int, error) {
	var messages []string
	for page := 1; page <= maxCommitPages; page++ {
		current, err := token()
		if err != nil {
			return messages, page - 1, fmt.Errorf("mint installation token for %s/%s PR #%d commits page %d: %w", owner, repo, number, page, err)
		}
		commitsPath := fmt.Sprintf("/repos/%s/%s/pulls/%d/commits?per_page=100&page=%d", url.PathEscape(owner), url.PathEscape(repo), number, page)
		body, status, header, err := readGitHubPage(ctx, client, current, commitsPath)
		if err != nil {
			return messages, page, fmt.Errorf("fetch commits of PR #%d page %d: %w", number, page, err)
		}
		if err := githubapp.CheckResponse(status, header, body); err != nil {
			return messages, page, fmt.Errorf("fetch commits of PR #%d page %d: %w", number, page, err)
		}
		var commits []commitMessagePayload
		if err := json.Unmarshal(body, &commits); err != nil {
			return messages, page, fmt.Errorf("decode commits of PR #%d page %d: %w", number, page, err)
		}
		for _, c := range commits {
			messages = append(messages, c.Commit.Message)
		}
		if len(commits) < 100 {
			return messages, page, nil
		}
	}
	return messages, maxCommitPages, nil
}

// sessionTrailers is every `Omp-Session:` commit trailer across messages (LEGION-294's rule,
// ported from the prototype's `session_ids`), first-seen order, no repeats.
func sessionTrailers(messages []string) []string {
	sessions := []string{}
	for _, message := range messages {
		for _, match := range ompSessionTrailer.FindAllStringSubmatch(message, -1) {
			sessions = appendUnique(sessions, match[1])
		}
	}
	return sessions
}

// mapRunConclusion maps GitHub's raw run conclusion to this schema's narrower check constraint
// (success, failure, cancelled only); any other value (including an empty string for a run still
// in progress) is nil, matching FetchedRun's documented policy.
func mapRunConclusion(raw string) *DeliveryRunConclusion {
	switch raw {
	case "success":
		c := DeliveryRunConclusionSuccess
		return &c
	case "failure":
		c := DeliveryRunConclusionFailure
		return &c
	case "cancelled":
		c := DeliveryRunConclusionCancelled
		return &c
	default:
		return nil
	}
}

// mapJobConclusion is mapRunConclusion's job-level counterpart: a job's schema accepts a wider
// set (skipped, timed_out in addition to the three a run accepts).
func mapJobConclusion(raw *string) *DeliveryJobConclusion {
	if raw == nil {
		return nil
	}
	switch *raw {
	case "success":
		c := DeliveryJobConclusionSuccess
		return &c
	case "failure":
		c := DeliveryJobConclusionFailure
		return &c
	case "cancelled":
		c := DeliveryJobConclusionCancelled
		return &c
	case "skipped":
		c := DeliveryJobConclusionSkipped
		return &c
	case "timed_out":
		c := DeliveryJobConclusionTimedOut
		return &c
	default:
		return nil
	}
}

// splitRepo splits "owner/repo" into its two segments.
func splitRepo(repoFull string) (owner, repo string, err error) {
	parts := strings.SplitN(repoFull, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("malformed repository %q: want owner/repo", repoFull)
	}
	return parts[0], parts[1], nil
}

// containsString reports whether values contains s, exactly.
func containsString(values []string, s string) bool {
	for _, v := range values {
		if v == s {
			return true
		}
	}
	return false
}
