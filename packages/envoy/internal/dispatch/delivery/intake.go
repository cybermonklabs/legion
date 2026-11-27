// Intake (LEGION-567): a Dispatch server consumer of the GitHub events Envoy already relays over
// NATS (`notifications.github.>`), for live updates between the five-minute reconcile's passes.
// Every write here goes through the same upsert functions reconcile uses (store.go), so a NATS
// redelivery or a reconcile re-reading the same fact is a no-op, not a duplicate. A GitHub call
// that fails here is logged and the message is still acked, because the reconcile is the actual
// catch-up mechanism: what a missed event carried lands when a later pass reads that pull
// request, an open one's head once its row is due and that pass's stamp of its try succeeds
// (PullRequestHeadStaleAfter). The one failure that mechanism does not cover is a pull
// request's write that nothing else makes: its head row, which no pass can offer because the
// passes read the rows that exist (ErrHeadNotStored), or the mark a settled CI run owes its
// issue's check states, since no table stores a run (errCheckStatesNotMarked). That envelope is
// handed back to NATS for a bounded number of redeliveries instead (Intake.deliver,
// ownHandBackMaxDeliveries). The intake also hands a push of a branch to the architecture
// importer (PushImports), so a component source's branch is imported when it moves rather than at
// the importer's next five-minute pass; a hand-off that fails is logged and the message acked too,
// and that five-minute pass is its catch-up.
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
	"time"

	"github.com/jackc/pgx/v5"
	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
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

// Intake wires the NATS consumer to the database, the GitHub App client, the event broker the
// issue.deployed events go through (RecordDeployedEvents) and the importer a push of a branch is
// handed to (PushImports). StartIntakes builds it, and only with the App configured.
type Intake struct {
	pool   *store.Pool
	github *githubapp.Client
	events *events.Broker
	// pushes is what a push of a branch is handed to (handlePushEnvelope): the architecture
	// importer in the server, which StartIntakes sets. nil hands pushes nowhere. A field, and not
	// a parameter of newIntake, so a test that needs pushes sets it on the intake it builds.
	pushes PushImports
	// settings is the delivery settings every message is routed by, shared with the job intake
	// (StartIntakes).
	settings *SettingsCache
	// attributor attributes each pull request an envelope reports and keys its head row, by the
	// rename pairs the process read at boot.
	attributor *Attributor
	// heldDurable is this intake's delivery from bus.DispatchDeliveryDurable under the durable's
	// hold: the subscription, StopDelivering and the take-over.
	heldDurable
}

// newIntake builds an Intake on natsClient, a connected client StartIntakes opens for it alone,
// holding its durable and taking it over under a hold and a lease it takes on leases, routing by
// settings, and attributing each pull request by attributor, timed by tuning (intakeTuning):
// production's values, which StartIntakes passes, or a test's. Each take-over first marks every
// issue with an open head owed (markOpenHeadsOwed).
func newIntake(tuning *intakeTuning, natsClient *bus.Client, pool *store.Pool, leases intakeLeases, github *githubapp.Client, broker *events.Broker, settings *SettingsCache, attributor *Attributor) *Intake {
	in := &Intake{pool: pool, github: github, events: broker, settings: settings, attributor: attributor}
	in.heldDurable = newHeldDurable(tuning, natsClient, leases, deliveryConsumerConfig, intakeKey, in.deliver)
	in.heldDurable.beforeTakeOver = in.markOpenHeadsOwed
	return in
}

// PushImports is what the intake hands a push of a branch to: the architecture importer
// (architecture.Importer.Pushed), which imports every enabled component source on that repository
// and branch. repo is the push's payload's, `owner/name` in GitHub's case, and branch is the ref
// without `refs/heads/`. The delivery package declares it rather than importing the importer, so
// that it depends on no architecture package.
//
// Pushed runs on the push's key's goroutine (intakeKey) under the message's deadline, and the
// message holds one of the intake's in-flight slots (intakeMaxAckPending) until it returns, so it
// returns without waiting for the import it starts. ctx holds no connection of the pool (deliver),
// so Pushed may read through the pool. A failure it answers is logged at WARN (`process event`)
// and the message acknowledged, as deliver does for a handler's error other than ErrHeadNotStored.
type PushImports interface {
	Pushed(ctx context.Context, repo, branch string) error
}

// Run delivers GitHub events from the durable consumer while this process holds the durable's
// hold, and stands by for the hold while another process holds it, until ctx is cancelled; then
// it stops delivering and releases the hold (heldDurable.stop) under intakeStopBudget from ctx's
// end before it returns. It binds the durable whether or not delivery settings are saved: a pull
// request's head row (recordPullRequestHead) and a push of a branch (handlePushEnvelope) read no
// setting, and a deployment with none still compares its check results with pull request heads
// (api/results_ci.go, NewestPullRequestHead). Only the delivery timeline's own work reads the
// settings (route). It binds nothing before the settings cache's first read has answered
// (SettingsCache.Read), whatever that read found: until then the cache holds nil, and a deployment
// with settings would route a merge, a workflow run and the event freshness by no settings and drop
// them. A Run whose ctx ends before that read returns at once, having bound and held nothing.
//
// The durable carries one filter subject, githubIntakeSubject, which no setting changes, so no
// setting rebinds it. Each message is routed by the settings the cache holds when its handler
// runs, so a settings change -- saved settings, a different deploy repository or workflow path --
// takes effect as soon as the cache has read it, with no restart and no rebind. route does all
// the filtering, from those settings.
//
// Several envoy-dispatch processes run at once during a rolling release, of this build and of the
// one before it, and exactly one of them delivers from the durable: the one holding its hold
// (heldDurable). The others stand by, trying for the hold every durableBindInterval for the life
// of the process, with no deadline, and take the durable over within seconds of the holder
// releasing its hold, or of the hold lapsing when the holder dies. A holder checks every
// intakeHoldRenew that the durable still delivers to it, and stops delivering the moment its hold
// is lost.
//
// The PR-checks workflow path is live-reachable only for a fork-originated pull request: Envoy's
// own webhook normalizer (internal/contracts/normalize.go, untouched by this package) drops a
// workflow_run envelope whose pull_requests array is non-empty, which GitHub populates for a
// same-repo PR's run but reports empty for a cross-fork one. For the common same-repo case those
// runs never arrive here at all; the five-minute reconcile is the only path that ever records
// them. Routed rather than ignored because it is the only live-update path for the fork case,
// which does reach it.
func (in *Intake) Run(ctx context.Context) {
	select {
	case <-in.settings.Read():
	case <-ctx.Done():
		return
	}
	in.run(ctx)
}

// route dispatches one envelope payload by its kind field: a pull request to
// handlePullRequestEnvelope, a settlement of a pull request's CI runs to handleChecksEnvelope, a
// workflow run to routeWorkflowEnvelope, a push to handlePushEnvelope, any other kind nowhere. It
// is the whole filter: the durable is handed every GitHub notification on the bus
// (githubIntakeSubject), comments among them, and an envelope with no payload that a caller of the
// listener's publish route can still put there. The payload is decoded once, into its fields. An
// empty payload, or one of a kind route does not handle, is discarded before any GitHub call or
// write; only the string fields of a kind it handles are read, as the flat string map its handler
// takes (flatPayload), and every other member is skipped (a CI settlement's hold arrays and
// groups, cistore.Summary). settings is what the settings cache holds when the envelope arrives,
// nil while no delivery_settings row is saved: a pull request and a CI settlement are handed to
// their handlers either way, since neither the head row nor the check states' owed mark reads a
// setting, and a workflow run is the delivery timeline's alone, so with no settings it is
// discarded before any GitHub call (routeWorkflowEnvelope). handled reports whether the kind is
// one route hands to a handler of the delivery timeline, whatever that handler then decides, so
// deliver records the event's time for a pull request that was not merged or another
// repository's run as well; with no settings it is false, since the event freshness is a column
// of the delivery_settings row (RecordEventAt). A push is no delivery fact: route hands it to its
// handler and answers it not handled, with the handler's error, so it records no issue.deployed
// and does not move the Delivery page's event freshness. A payload that is not a JSON object, and
// a kind that is not a string, are errMalformedPayload.
//
// An envelope whose handler stored a fact the issue.deployed rule reads (a pull request's row, a
// run, its jobs) ends with recordDeployed, whether or not the handler then failed: a run written
// before its jobs fetch failed may still be the run that shipped a pull request stored earlier.
// A handler stores such a fact only by the settings, so one that reports it wrote had settings.
func (in *Intake) route(ctx context.Context, settings *DeliverySettings, raw string) (handled bool, err error) {
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
	var handle func(context.Context, *DeliverySettings, map[string]string) (bool, error)
	switch kind {
	case "pr":
		handle = in.handlePullRequestEnvelope
	case "checks":
		handle = in.handleChecksEnvelope
	case "workflow":
		handle = in.routeWorkflowEnvelope
	case "push":
		return false, in.handlePushEnvelope(ctx, flatPayload(fields))
	default:
		return false, nil
	}
	wrote, err := handle(ctx, settings, flatPayload(fields))
	if wrote {
		in.recordDeployed(ctx, *settings)
	}
	return settings != nil, err
}

// handlePushEnvelope hands a push of a branch to the importer (PushImports): the payload's repo,
// and its ref without `refs/heads/`. A push of any other ref, a tag's above all, is handed nowhere,
// and so is every push where no importer is wired (in.pushes is nil). It reads no setting and
// writes nothing of the delivery timeline, and the importer's failure is its answer, which deliver
// logs before it acknowledges the envelope: the importer's five-minute pass imports the source
// all the same.
//
// ctx is the message's own context (deliver), which derives from the delivery's handling context,
// made from context.Background (heldDurable.subscribe), and nothing between holds a connection of
// the pool on it, so the importer's read of its sources through the pool is not refused as a
// second connection (store.ErrNestedAcquire).
func (in *Intake) handlePushEnvelope(ctx context.Context, payload map[string]string) error {
	branch, isBranch := strings.CutPrefix(payload["ref"], "refs/heads/")
	if !isBranch || in.pushes == nil {
		return nil
	}
	return in.pushes.Pushed(ctx, payload["repo"], branch)
}

// recordDeployed writes the issue.deployed of every pull request the envelope just handled may have
// shipped or completed (RecordDeployedEvents). A failure is logged and does not fail the envelope,
// which is acknowledged as every handled envelope is: the reconcile records at the end of every
// pass too, so a row this call could not write gets its event within five minutes.
func (in *Intake) recordDeployed(ctx context.Context, settings DeliverySettings) {
	written, err := RecordDeployedEvents(ctx, in.pool, settings, in.events)
	if err != nil {
		in.tuning.log.Error("dispatch delivery: record issue.deployed after an envelope", "error", err)
	}
	if written > 0 {
		in.tuning.log.Info("dispatch delivery: recorded issue.deployed", "events", written, "by", "intake")
	}
}

// routeWorkflowEnvelope hands a workflow run to handleWorkflowEnvelope as the run kind its file is
// configured as. The runs this slice records are the deploy repository's own: another repository's
// run of a file with the same path is not one of them, and routing it would fetch a run this slice
// never stores. A run of a file neither setting names is not fetched either, and with no settings
// (nil) no run is.
func (in *Intake) routeWorkflowEnvelope(ctx context.Context, settings *DeliverySettings, payload map[string]string) (bool, error) {
	if settings == nil || payload["repo"] != settings.DeployRepo {
		return false, nil
	}
	switch payload["path"] {
	case settings.DeployWorkflowPath:
		return in.handleWorkflowEnvelope(ctx, *settings, DeliveryRunKindDeploy, payload)
	case settings.PRChecksWorkflowPath:
		// A pr_checks run is no part of the shipping rule (shippingRunJoin reads deploy runs
		// alone), so storing one can ship nothing: the envelope ends with no deployed pass.
		_, err := in.handleWorkflowEnvelope(ctx, *settings, DeliveryRunKindPRChecks, payload)
		return false, err
	default:
		return false, nil
	}
}

// errMalformedPayload is an envelope payload route cannot read: not a JSON object, or a kind that
// is not a string.
var errMalformedPayload = errors.New("malformed envelope payload")

// flatPayload is a pull request's, a workflow run's or a push's string fields as the flat string
// map its handler reads. A field that is not a string is left out: payload members are additive,
// so a newer publisher may add one, and no handler reads it. A handler reads a field left out as
// the empty string, as it reads a field the payload does not carry.
func flatPayload(fields map[string]json.RawMessage) map[string]string {
	payload := make(map[string]string, len(fields))
	for name, field := range fields {
		var value string
		if json.Unmarshal(field, &value) == nil {
			payload[name] = value
		}
	}
	return payload
}

// githubIntakeSubject is the durable's one filter subject: every GitHub event Envoy relays. The
// subscription's callback reads each message's subject first (intakeKey) and acknowledges every
// subject that is not a pull request's own, the settlement of its checks, a workflow run's or a
// push of a branch's without decoding it; the handler decides what to do with the rest (route),
// and fetches nothing for the ones this slice does not want.
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
// cancelled and logged, and acked for every failure but two: the five-minute reconcile is this
// package's catch-up mechanism, so one slow envelope must not hold the consumer. The two
// failures it does not cover are a deadline that falls on a pull request's head write, or on
// either of the reads before it, which leaves a head row no pass can offer (ErrHeadNotStored),
// and one that falls on a settlement's mark or the reads before it (errCheckStatesNotMarked);
// that envelope is handed back instead, for the bounded number of redeliveries deliver allows.
// It is the default of the intake's own messageTimeout (intakeTuning), which a test shortens on
// the intakes it builds.
const intakeMessageTimeout = time.Minute

// intakeMaxAckPending bounds how many messages NATS may have outstanding with this consumer at
// once, and so how many handlers run at once: one per key (intakeKey), and every message of a key
// waits for the one before it. All of them can be one key's, so the bound has to be what one
// handler can acknowledge in turn well inside the ack wait (intakeTuning.ackWait); the server's
// own default of 1,000 is far past it, and messages it redelivers under a handler still working
// accumulate in the client's pending buffer until that buffer drops them. It is the default of
// the intake's own maxAckPending (intakeTuning).
const intakeMaxAckPending = 4

// ackWait is how long NATS waits for an acknowledgement before redelivering. Derived from the
// intake's maxAckPending and messageTimeout rather than chosen: when every outstanding message is
// one key's, the last of maxAckPending waits for the ones before it, so the bound is their total
// handling time, plus a minute for the acknowledgement itself to land. A test that shortens either
// on its intakes gets an ack wait derived from what it set.
func (t *intakeTuning) ackWait() time.Duration {
	return time.Duration(t.maxAckPending)*t.messageTimeout + time.Minute
}

// deliveryConsumerConfig is the policy a take-over creates the durable with
// (bus.DispatchDeliveryDurable, one push consumer over githubIntakeSubject), unless the deployed
// durable carries a higher generation (takeOverConfig). The server holds a backlog undelivered
// until a subscriber attaches, so a restart with events waiting loses none of them and none of
// them count against MaxAckPending in the meantime.
//
// It asks for new messages only (DeliverNewPolicy), which a take-over that creates the durable
// with no start recorded turns into an explicit start at the stream's next sequence (takeOver;
// takeOverStart says why the start is explicit): the first bind of a deployment that never had the
// durable must not hand the intake the stream's whole retained history, 72 hours of every GitHub
// event Envoy relays, none of which it ever processed. Its consumer generation, 2, is the job
// durable's too (jobConsumerConfig).
//
// FilterSubject, AckWait and MaxAckPending keep the values of the build before generations: that
// build's processes, still running during a rolling release, update a durable whose three differ
// from theirs in place on every settings poll, so a change to any of them waits until no such
// process is left. A change to this config takes a new consumerConfigGeneration and a new row in
// TestTheConsumerConfigEqualsItsGenerationsPinnedRow. Its AckWait and MaxAckPending are tuning's,
// production's values outside tests.
func deliveryConsumerConfig(tuning *intakeTuning) natsgo.ConsumerConfig {
	return natsgo.ConsumerConfig{
		Durable:        bus.DispatchDeliveryDurable,
		DeliverSubject: natsgo.NewInbox(),
		FilterSubject:  githubIntakeSubject,
		DeliverPolicy:  natsgo.DeliverNewPolicy,
		AckPolicy:      natsgo.AckExplicitPolicy,
		AckWait:        tuning.ackWait(),
		MaxAckPending:  tuning.maxAckPending,
		Metadata:       consumerGenerationMetadata(),
	}
}

// consumerConfigGeneration is the generation of the intakes' consumer configs
// (deliveryConsumerConfig, jobConsumerConfig), stamped on each durable as its
// consumerGenerationKey metadata. A take-over that finds a durable of a higher generation
// recreates it with the deployed config rather than this build's (takeOverConfig), so while two
// builds run, the older one never reverts what the newer one set.
const consumerConfigGeneration = 2

// consumerGenerationKey is the consumer metadata key that holds the generation of its config. A
// durable without it was last created by a build before generations: generation 0.
const consumerGenerationKey = "dispatch.consumer-generation"

// consumerGenerationMetadata is the metadata this build creates a durable with: its generation.
func consumerGenerationMetadata() map[string]string {
	return map[string]string{consumerGenerationKey: strconv.Itoa(consumerConfigGeneration)}
}

// deliver handles one message on its key's goroutine: it routes the envelope's payload by the
// settings the cache holds, or by none while no settings are saved, under the intake's
// messageTimeout, and acks: a malformed envelope has nothing to retry, and a route error (a failed
// GitHub call, or that deadline passing) is caught up by the next reconcile pass rather than by
// NATS redelivery -- see the package doc comment -- as a push the importer failed to take is by
// the importer's own five-minute pass. One route error is not, and is the second case below. The
// deadline is what makes the ack wait
// (intakeTuning.ackWait) a bound rather than a hope: without it one stuck GitHub call could hold an
// in-flight slot past the ack wait and have the message redelivered underneath it. An envelope
// route answers not handled (an empty payload, a kind route sends to no handler, a push, which
// is no delivery fact, or any envelope while no settings are saved) is acknowledged with no
// freshness write, and with no log line unless its handler failed. One it handles records the
// event's time, whatever its handler decided.
//
// The one message it does not ack is one whose handling the stop gave up (handling cancelled,
// StopDelivering): that is handed back, so the next holder of the durable redoes it at once
// rather than this event waiting five minutes for the reconcile. Its writes are the upserts
// reconcile makes too, so redoing what had in fact finished just before the stop gave it up
// writes the same facts again. That hand-back is withheld once the hold's loss deadline has
// passed (intakeDelivery.handBack), as the stop's own hand-backs are, and the message is then
// the next holder's take-over to hand over.
//
// The other messages it does not acknowledge, for as many deliveries as the bound below allows,
// are two whose write nothing else makes. One is a pull request's envelope whose head row could
// not be stored at all (ErrHeadNotStored, handlePullRequestEnvelope): no reconcile pass catches
// that up, because the passes read the rows that exist (ListStalePullRequestHeads), so an
// acknowledged envelope loses that pull request's head for good and with it every
// `before_merge` result measured against it. The write is idempotent (both its statements are
// `on conflict … do update`), so a redelivery of an envelope whose write had in fact landed
// writes the same pair again. The other is a settlement of a pull request's checks whose issue
// could not be marked owed (errCheckStatesNotMarked, handleChecksEnvelope): no table stores a
// CI run, so no other write marks the issue when a run concludes, and an acknowledged settlement
// leaves the state of a check CI proves as it was while its run went on, until some other write
// of the issue marks it. A redelivery of a settlement whose mark had in fact landed marks the
// issue a second time, which costs one settle that publishes nothing. Each is handed back with a
// delay (intakeDelivery.handBackOwn), and handed back now rather than at the stop: this message
// wants redelivering to this same process, which still holds the durable's hold and still routes
// that key to the one goroutine it is on. The delay is what keeps a failure that repeats from
// spinning: the write is the same write each time, so asking again within the second would buy
// nothing.
//
// That hand-back is this process's own answer to the durable, and it is made only while the
// hold still covers it (intakeDelivery.mayAnswer): a hand-back a process makes after its hold
// has lapsed reaches the durable a take-over recreated under the same name, where it would move
// the new holder's pending entry for that message and have it delivered a second time. Withheld,
// the message is not lost: it stays outstanding, and the take-over's recreate at the durable's
// ack floor hands it to the new holder once. Outside any stop, the hand-back is published;
// on a reconnecting link between attempts it is refused at once, and while an
// attempt holds nats.go's lock it waits until the attempt ends (intakeDelivery.handBackOwn).
//
// It is handed back a bounded number of times and then acknowledged, with an Error naming the
// envelope and the deliveries it spent (ownHandBackMaxDeliveries): a message handed back with a
// delay stays in the consumer's ack-pending set for the whole of that delay, so a write that
// fails the same way every time would hold one of the four in-flight slots
// (intakeMaxAckPending) for ever. The count is the server's own, read off the message
// (Msg.Metadata, NumDelivered), so it is the count of this envelope's deliveries and not of
// this process's attempts at it. A message carrying no such metadata is not a JetStream
// message, so no count can cap it; it is handed back rather than acknowledged, since the
// whole argument of this path is that nothing else makes that write, and a hand-back the ack
// wait bounds is the safer of the two answers.
//
// handling is read from the delivery, which is also what says whether that hand-back may be
// made, and nothing else calls deliver.
func (in *Intake) deliver(d *intakeDelivery, message intakeMessage) {
	msg, handling := message.msg, d.handling
	// handOwnBack is set by the route errors NATS must redeliver, which the deferred answer below
	// reads: every other path acknowledges.
	handOwnBack := false
	defer func() {
		if handling.Err() != nil {
			in.tuning.log.Warn("dispatch delivery: the intake stopped before the event's handling finished; handing it back", "subject", msg.Subject)
			d.handBack(msg)
			return
		}
		if handOwnBack {
			d.handBackOwn(msg, ownHandBackAfter)
			return
		}
		d.acknowledge(msg)
	}()
	ctx, cancel := context.WithTimeout(handling, in.tuning.messageTimeout)
	defer cancel()

	envelope := message.envelope
	if envelope == nil {
		decoded, err := decodeEnvelope(msg.Data)
		if err != nil {
			in.tuning.log.Error("dispatch delivery: decode envelope", "subject", msg.Subject, "error", err)
			return
		}
		envelope = &decoded
	}
	handled, err := in.route(ctx, in.settings.Load(), envelope.Payload)
	switch {
	case errors.Is(err, errMalformedPayload):
		in.tuning.log.Error("dispatch delivery: decode envelope payload", "subject", msg.Subject, "error", err)
		return
	case errors.Is(err, ErrHeadNotStored):
		handOwnBack = in.ownHandBackAllowed(msg, err,
			"dispatch delivery: a pull request's head row could not be stored in the deliveries allowed; acknowledging the envelope, whose pull request now has no head row until a later `pull_request` event for it, or the read-through a recorded result makes, writes one",
			"dispatch delivery: a pull request's head row was not stored; handing the envelope back")
		return
	case errors.Is(err, errCheckStatesNotMarked):
		handOwnBack = in.ownHandBackAllowed(msg, err,
			"dispatch delivery: a settled CI run's issue could not be marked owed in the deliveries allowed; acknowledging the envelope, whose issue keeps the published state of each check CI proves until another write of the issue marks it owed",
			"dispatch delivery: a settled CI run's issue was not marked owed; handing the envelope back")
		return
	case err != nil:
		in.tuning.log.Warn("dispatch delivery: process event", "subject", msg.Subject, "error", err)
		return
	case !handled:
		return
	}
	if err := RecordEventAt(ctx, in.pool); err != nil {
		in.tuning.log.Error("dispatch delivery: record event freshness", "error", err)
	}
}

// ownHandBackAfter is how long NATS waits before handing back an envelope whose write nothing
// else makes (deliver): a pull request's head row that could not be stored, or the mark a settled
// CI run owes its issue's check states. It is not zero because the write that failed is the same
// write each time: a plain Nak would have the server redeliver at once, and a database that is
// refusing writes would then spin the consumer against it as fast as the round trip allows.
//
// What the delay does not do is give the in-flight slot back. A delayed Nak leaves the message
// in the consumer's ack-pending set — nats-server's processNak moves that pending entry's
// timestamp and nothing else — so one such envelope holds one of the four slots
// (intakeMaxAckPending) for every second it is waiting to come round again, and four of them
// hold all four. Bounding that is ownHandBackMaxDeliveries' work, not this constant's: ten
// seconds only sets the rate, which is short beside the five minutes of the reconcile this
// message cannot wait for, and long enough that the cap's whole run of redeliveries is
// measured in minutes rather than in round trips.
const ownHandBackAfter = 10 * time.Second

// ownHandBackMaxDeliveries is how many deliveries one envelope whose head row could not be
// stored, or whose settlement's mark could not be written, gets before deliver acknowledges it
// and logs the loss. Without it there is no bound at all: the durable sets no MaxDeliver, and a
// delayed Nak holds the message in the consumer's ack-pending set, so a write that fails the same
// way every time is redelivered every ownHandBackAfter for ever and holds an in-flight slot
// throughout. Four such envelopes stop the intake outright, and everything behind them — every
// other pull request and every workflow run — waits on a row nothing was ever going to write.
//
// Thirty is where the two costs it sits between cross. Too few, and a database away for a
// minute loses a head row that nothing else repairs: a minute is six deliveries at
// ownHandBackAfter, and an outage is counted in minutes rather than in seconds, so a
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
// own case's span still gets the head row written. A mark is the same write, one statement
// each time, so the same thirty bound it.
//
// The count it is measured against is the consumer's, so a take-over starts it afresh: the
// take-over deletes the durable and creates it again at its ack floor (takeOverConfig), and the
// recreated consumer has delivered nothing, so an envelope it redelivers arrives at
// NumDelivered 1 however many deliveries the holder before it spent. That is right for what
// this cap is for, which is the in-flight window of one consumer (intakeMaxAckPending): the
// window the new holder must not have held for ever is its own. It does mean the hand-backs are
// bounded per holder rather than for all time, so a head row nothing can write costs thirty
// deliveries again after each take-over — at a take-over per rolling release, beside a cap
// whose whole run is five minutes, that is a cost no reader of this slice can measure.
//
// It is the default of the intake's own ownHandBackMaxDeliveries (intakeTuning), which a test
// lowers on the intakes it builds: thirty deliveries of one envelope is minutes of wall clock no
// test should spend.
const ownHandBackMaxDeliveries uint64 = 30

// ownHandBackAllowed reports whether deliver hands msg back for err, the failure of a write
// nothing else makes: while the server has delivered msg fewer times than the intake's
// ownHandBackMaxDeliveries (intakeTuning) allows it logs handingBack at WARN, with the delivery
// and the delay, and answers true; at the last of them it logs spent at ERROR, with the
// deliveries msg spent, and answers false, so deliver acknowledges it. Both go to the intake's
// logger.
func (in *Intake) ownHandBackAllowed(msg *natsgo.Msg, err error, spent, handingBack string) bool {
	allowed := in.tuning.ownHandBackMaxDeliveries
	delivered, allSpent := ownHandBacksSpent(in.tuning.log, msg, allowed)
	if allSpent {
		in.tuning.log.Error(spent, "subject", msg.Subject, "deliveries", delivered, "error", err)
		return false
	}
	in.tuning.log.Warn(handingBack, "subject", msg.Subject, "delivery", delivered, "deliveries_allowed", allowed,
		"redeliver_after", ownHandBackAfter, "error", err)
	return true
}

// ownHandBacksSpent reports how many times the server has delivered msg and whether that is
// every delivery the intake allows its write to fail in (allowed, its ownHandBackMaxDeliveries),
// writing a metadata read that fails to logger, the intake's.
//
// A message whose metadata cannot be read is not a JetStream message at all (Msg.Metadata
// answers an error for one with no JetStream reply subject). No count can be read from it and
// so no cap can be applied, and the answer is "not spent": the caller hands it back, which is
// the answer that keeps the write's one chance of being made, and the ack wait bounds a
// hand-back the server never takes.
func ownHandBacksSpent(logger *slog.Logger, msg *natsgo.Msg, allowed uint64) (delivered uint64, spent bool) {
	meta, err := msg.Metadata()
	if err != nil {
		logger.Error("dispatch delivery: read how often the envelope to hand back has been delivered",
			"subject", msg.Subject, "error", err)
		return 0, false
	}
	return meta.NumDelivered, meta.NumDelivered >= allowed
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
// The head write reads no setting, so it runs with no settings saved (settings nil) as well.
// Only a merge (action "closed", merged "true") does anything for the delivery timeline
// after that, and only by the settings; its author and excluded-repository checks run on the
// envelope's own fields, so a merge by anyone outside the population costs no GitHub call of its
// own. It reports whether it stored the pull request's delivery row: a head write is not one, and
// neither is a row it deleted or a pull request outside the population, so neither ships with
// anything.
func (in *Intake) handlePullRequestEnvelope(ctx context.Context, settings *DeliverySettings, payload map[string]string) (wrote bool, err error) {
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
		in.tuning.log.Warn("dispatch delivery: record a pull request's head",
			"repo", repoFull, "number", number, "error", headErr)
		if errors.Is(headErr, ErrHeadNotStored) {
			return false, headErr
		}
	}
	if settings == nil || payload["action"] != "closed" || payload["merged"] != "true" {
		return false, nil
	}
	author := payload["author"]

	if containsString(settings.ExcludedRepos, repoFull) {
		return false, nil
	}
	if !containsString(settings.PopulationAuthors, author) {
		return false, nil
	}

	owner, repo, err := refs.SplitGitHubRepo(repoFull)
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
	isPopulation, err := IsPopulationPR(raw, *settings, liveIntakeWindow())
	if err != nil {
		// IsTaskPR's "neither label" error: a deploy-repo PR GitHub's own classifier workflow has
		// not labelled yet. Surfaced, not guessed; the reconcile retries this PR's label on its
		// next pass.
		return false, fmt.Errorf("classify %s#%d: %w", repoFull, number, err)
	}
	if !isPopulation {
		return false, DeletePullRequest(ctx, in.pool, repoFull, number)
	}

	if err := in.attributor.completePullRequest(ctx, in.pool, in.github, owner, repo, repoFull, number, fetched, messages); err != nil {
		return false, err
	}
	return true, nil
}

// errCheckStatesNotMarked marks a settlement of a pull request's checks whose issue could not be
// marked owed (handleChecksEnvelope). No table stores a CI run, so a run that concluded changes no
// row a trigger marks, and the settlement's mark is the only write that tells the check states
// settler to read it: deliver hands such an envelope back for redelivery rather than acknowledge
// it, as it hands back one whose head row was not stored.
var errCheckStatesNotMarked = errors.New("the check states of the settled pull request's issue were not marked owed")

// handleChecksEnvelope processes one `kind:"checks"` envelope: the settlement the listener's CI
// summary loop publishes once every check run of a commit of a pull request has finished
// (cistore). It marks the check states of the issue the pull request's open head belongs to owed
// (check_states_owe, migration 20261126005334), in a statement of its own, whose commit notifies
// the check states settler: the settler reads the check runs at the head from GitHub again and
// publishes the state of each check CI proves that changed (api/check_states.go). The settlement's
// own fields decide nothing past the pull request it names, since any caller of the listener's
// publish route can put an envelope here, and a mark only asks for a computation Dispatch makes
// from GitHub's answer.
//
// The head is read under the repository's canonical name (Attributor.canonical, over the rename
// pairs the boot read), the name its row is keyed by. A pull request that is not open, or that no
// issue holds, has no published state to change, and nothing is marked. A failure of the head's
// read or the mark is errCheckStatesNotMarked, which deliver hands back; a number that does not
// parse is not, since every delivery of the envelope carries it. It writes no row the
// issue.deployed rule reads, so it reports wrote false.
func (in *Intake) handleChecksEnvelope(ctx context.Context, _ *DeliverySettings, payload map[string]string) (wrote bool, err error) {
	number, err := strconv.Atoi(payload["number"])
	if err != nil {
		return false, fmt.Errorf("checks envelope: malformed number %q: %w", payload["number"], err)
	}
	repoFull := in.attributor.canonical(payload["repo"])
	var issue *string
	err = in.pool.QueryRow(ctx, `
		select issue_key from pull_request_heads where repo = $1 and number = $2 and state = 'open'
	`, repoFull, number).Scan(&issue)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("%w: read the issue of %s#%d: %w", errCheckStatesNotMarked, repoFull, number, err)
	case issue == nil:
		return false, nil
	}
	if _, err := in.pool.Exec(ctx, `select check_states_owe($1)`, *issue); err != nil {
		return false, fmt.Errorf("%w: mark the check states of %s owed for %s#%d: %w", errCheckStatesNotMarked, *issue, repoFull, number, err)
	}
	return false, nil
}

// markOpenHeadsOwed marks owed the check states of every issue that holds an open pull request
// head, in one statement, the backfill migration's (20261126005342). Each take-over of the
// durable runs it before it changes the durable (heldDurable.beforeTakeOver). The holder before
// may be a build that acknowledges a settlement of a pull request's checks with no mark, as every
// build before these marks does, and nothing else would read the run that settlement announced:
// the settle each mark starts reads the check runs at the issue's heads from GitHub again. Its
// cost is one settle of each such issue, with those reads, per take-over; a settle that finds a
// state unchanged publishes nothing.
func (in *Intake) markOpenHeadsOwed(ctx context.Context) error {
	if _, err := in.pool.Exec(ctx, `
		select check_states_owe(issue_key)
		from (select distinct issue_key from pull_request_heads where state = 'open' and issue_key is not null) owed
	`); err != nil {
		return fmt.Errorf("mark the check states of every issue with an open head owed: %w", err)
	}
	return nil
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
func (a *Attributor) completePullRequest(ctx context.Context, pool *store.Pool, github *githubapp.Client, owner, repo, repoFull string, number int, fetched FetchedPullRequest, read []string) error {
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
	inputs := a.attributionInputsFrom(attributionFacts{
		Repo: repoFull, URL: fetched.URL, Title: fetched.Title, Body: fetched.Body,
		HeadRef: fetched.HeadRef, CommitMessages: messages,
	})
	issueKey, _, err := a.resolveStoredIssueKey(ctx, pool, repoFull, fetched.URL, inputs)
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
	owner, repo, err := refs.SplitGitHubRepo(repoFull)
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

// maxCommitPages bounds fetchCommitMessages' pagination, and fetchComparison's: 500 commits is
// already an enormous pull request, and a PR beyond that is not worth the API cost of chasing its
// full history.
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

// containsString reports whether values contains s, exactly.
func containsString(values []string, s string) bool {
	for _, v := range values {
		if v == s {
			return true
		}
	}
	return false
}
