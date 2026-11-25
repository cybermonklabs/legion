// Intake (LEGION-567): a Dispatch server consumer of the GitHub events Envoy already relays over
// NATS (`notifications.github.>`), for live updates between the five-minute reconcile's passes.
// Every write here goes through the same upsert functions reconcile uses (store.go), so a NATS
// redelivery or a reconcile re-reading the same fact is a no-op, not a duplicate. A GitHub call
// that fails here is logged and the message is still acked, because the reconcile that runs
// every five minutes is the actual catch-up mechanism ("a missed event appears by the next
// five-minute reconcile"). The one failure that mechanism does not cover by itself
// is a pull request's head row the envelope's read of GitHub could not write, since the passes
// read the rows that exist: that delivery records the pull request as owed a read, which the
// reconcile's next pass makes, and the envelope is acknowledged once that record is written; an
// envelope whose record the database cannot take now is handed back to NATS, since it is then
// the only record of the change, and the stream keeps it 72 hours (ErrHeadNotStored,
// Intake.deliver, owePullRequestRead). The intake also hands a push of a branch to the
// architecture importer (PushImports), so a component source's branch is imported when it moves
// rather than at the importer's next five-minute pass; a hand-off that fails is logged and the
// message acked too, and that five-minute pass is its catch-up.
package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/logging"
)

// ompSessionTrailer matches a commit message's `Omp-Session: <id>` trailer line (the same shape
// LEGION-294's own collector reads).
var ompSessionTrailer = regexp.MustCompile(`(?m)^Omp-Session:\s*(\S+)\s*$`)

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
	// attributor attributes each pull request an envelope reports, keys its head row and owes its
	// reads, by the rename pairs the process read at boot (StartIntakes).
	attributor *Attributor
	// heldDurable is this intake's delivery from bus.DispatchDeliveryDurable under the durable's
	// hold: the subscription, StopDelivering and the take-over.
	heldDurable
}

// newIntake builds an Intake on natsClient, a connected client StartIntakes opens for it alone,
// holding its durable and taking it over under a hold and a lease it takes on leases, routing by
// settings, and attributing each pull request by attributor, timed by tuning (intakeTuning):
// production's values, which StartIntakes passes, or a test's.
func newIntake(tuning *intakeTuning, natsClient *bus.Client, pool *store.Pool, leases intakeLeases, github *githubapp.Client, broker *events.Broker, settings *SettingsCache, attributor *Attributor) *Intake {
	in := &Intake{pool: pool, github: github, events: broker, settings: settings, attributor: attributor}
	in.heldDurable = newHeldDurable(tuning, natsClient, leases, deliveryConsumerConfig, intakeKey, in.deliver)
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
// end before it returns.
//
// It binds whether or not delivery settings exist. A `pull_request` envelope's first write is
// the pull request's head row (recordPullRequestHead), which is what every `before_merge` check
// result is compared with (api/results_comparison.go), and that reader is served in every
// deployment, delivery configured or not. No other writer learns of a pull request when it opens:
// the reconcile's head pass reads only the rows that exist, and a result's read-through asks
// GitHub about one commit at most once every CommitPullReadStaleAfter. So the intake writes a head
// row with no settings at all, and leaves the delivery timeline's own facts, a merge of the
// population and a workflow run, to the settings (route, handlePullRequestEnvelope). A push of a
// branch is one more message it routes (handlePushEnvelope), and it reads no setting, so the
// importer is handed a push with settings or without.
//
// The durable is created with JetStream's DeliverAll (deliveryConsumerConfig), so the first bind
// in a deployment whose durable never existed is handed all the stream keeps (72 hours,
// ENVOY_NOTIFICATIONS). That backlog writes the head rows of the pull requests the deployment's
// earlier releases never stored, each in event order, and a write older than the row it lands on
// changes nothing (UpsertPullRequestHead). Without settings, no envelope of it reaches the
// delivery timeline.
//
// The durable carries one filter subject, githubIntakeSubject, which no setting changes, so no
// setting rebinds it. Each message is routed by the settings the cache holds when its handler
// runs, so a settings change -- delivery configured for the first time, a different deploy
// repository or workflow path -- takes effect as soon as the cache has read it, with no restart
// and no rebind. route does all the filtering, from those settings.
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
	in.run(ctx)
}

// route dispatches one envelope payload by its kind field: a pull request to
// handlePullRequestEnvelope, a workflow run to routeWorkflowEnvelope, a push to
// handlePushEnvelope, any other kind nowhere. It is the whole filter: the durable is handed every
// GitHub notification on the bus (githubIntakeSubject), CI settlements and comments among them,
// and an envelope with no payload that a caller of the listener's publish route can still put
// there. The payload is decoded once, into its fields. An empty payload, or one of a kind route
// does not handle, is discarded before any GitHub call or write, whatever its other fields hold (a
// CI settlement's hold arrays, cistore.Summary); only a pull request's, a workflow run's or a
// push's string fields are read, as the flat string map its handler takes (flatPayload), and every
// other member is skipped. handled reports whether the kind is one route hands to a handler of the
// delivery timeline, whatever that handler then decides, so deliver records the event's time for a
// pull request that was not merged or another repository's run as well. A push is no delivery
// fact: route hands it to its handler and answers it not handled, with the handler's error, so it
// records no issue.deployed and does not move the Delivery page's event freshness
// (RecordEventAt). A payload that is not a JSON object, and a kind that is not a string, are
// errMalformedPayload.
//
// settings is nil where delivery is not configured. A pull request is handled all the same, for
// its head row, and a workflow run is not handled at all: a run is the delivery timeline's own
// fact, read for the deploy repository alone. A push is handed to its handler with or without
// settings, since it reads none.
//
// An envelope whose handler stored a fact the issue.deployed rule reads (a pull request's row, a
// run, its jobs) ends with recordDeployed, whether or not the handler then failed: a run written
// before its jobs fetch failed may still be the run that shipped a pull request stored earlier.
// Both handlers store such a fact only under settings, so settings is not nil wherever wrote is.
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
	var wrote bool
	switch kind {
	case "pr":
		wrote, err = in.handlePullRequestEnvelope(ctx, settings, flatPayload(fields))
	case "workflow":
		if settings == nil {
			return false, nil
		}
		wrote, err = in.routeWorkflowEnvelope(ctx, *settings, flatPayload(fields))
	case "push":
		return false, in.handlePushEnvelope(ctx, flatPayload(fields))
	default:
		return false, nil
	}
	if wrote {
		in.recordDeployed(ctx, *settings)
	}
	return true, err
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
	written, err := in.attributor.RecordDeployedEvents(ctx, in.pool, settings, in.events)
	if err != nil {
		logging.FromContext(ctx).Error("dispatch delivery: record issue.deployed after an envelope", "error", err)
	}
	if written > 0 {
		logging.FromContext(ctx).Info("dispatch delivery: recorded issue.deployed", "events", written, "by", "intake")
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

// errMalformedPayload is an envelope payload route cannot read: not a JSON object, or a kind that
// is not a string.
var errMalformedPayload = errors.New("malformed envelope payload")

// errNoPullRequestNamed marks a pull request GitHub cannot have: a number that is not a number or
// is outside 1 to 2147483647, the range of the integer columns that key a pull request
// (pull_request_heads.number, pull_request_reads_owed.number), or a repository that is not a name
// GitHub can have (githubRepositoryName). pullRequestNamed answers it for a `pull_request`
// envelope before anything is read or written, and checkPullRequestKey answers it for each row
// that keys a pull request, where the row is built (owePullRequestRead, UpsertPullRequestHead),
// since the repository a row is keyed by is its canonical name, which comes from the rename pairs
// the boot read (refs.Renames) and not from the envelope. GitHub has no such pull request and no
// delivery of the envelope can get past it, so deliver acknowledges it at its first delivery and
// logs one line, and it owes no read. Any caller of Envoy's /v1 publish route can send one.
var errNoPullRequestNamed = errors.New("the envelope names no pull request GitHub can have")

// githubRepositoryName is the full name of a repository GitHub can have: an owner of 1 to 39
// letters, digits, hyphens and underscores, and a name of 1 to 100 letters, digits, hyphens,
// underscores and periods. 39 is GitHub's limit for a login, and a login holds only letters,
// digits and hyphens, except the login of a managed user account, which ends in an underscore and
// its enterprise's shortcode ("mona-cat_octo"); 100 is GitHub's limit for a repository name.
// "." and ".." are no repository names either (IsGitHubRepositoryName). So the values that key a
// pull request's rows are values the tables take, and an index of them holds the row: a name of
// 3,500 characters was refused by the btree of pull_request_reads_owed (SQLSTATE 54000) at every
// delivery (review of cybermonklabs/legion#4209 at ceeec40f46, B4).
var githubRepositoryName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,39}/[A-Za-z0-9._-]{1,100}$`)

// IsGitHubRepositoryName reports whether full, `owner/name`, is the full name of a repository
// GitHub can have (githubRepositoryName), and not "." or "..": the one rule of the names that key
// a pull request's rows (checkPullRequestKey). A rename pair's new name is held to the stricter
// rule of every read of GitHub (refs.SplitGitHubRepo, which takes no name this refuses) as the
// boot builds its Renames, which drops a pair that rule refuses (refs.NewRenames), so the
// canonical name of a name this takes is one it takes too.
func IsGitHubRepositoryName(full string) bool {
	_, name, _ := strings.Cut(full, "/")
	return githubRepositoryName.MatchString(full) && name != "." && name != ".."
}

// pullRequestNamed is the repository and the number a `pull_request` envelope's payload names,
// or errNoPullRequestNamed, which it answers before anything is read or written.
func pullRequestNamed(payload map[string]string) (repo string, number int, err error) {
	repo = payload["repo"]
	number, err = strconv.Atoi(payload["number"])
	if err != nil {
		return repo, 0, fmt.Errorf("%w: number %.40q: %w", errNoPullRequestNamed, payload["number"], err)
	}
	return repo, number, checkPullRequestKey(repo, number)
}

// checkPullRequestKey is nil where repo and number are a pull request GitHub can have, and
// errNoPullRequestNamed naming the bound they break otherwise: the one check of the values that
// key a pull request's rows, which pullRequestNamed makes of an envelope and each row's builder
// makes of the values it writes (owePullRequestRead, UpsertPullRequestHead).
func checkPullRequestKey(repo string, number int) error {
	if number < 1 || number > math.MaxInt32 {
		return fmt.Errorf("%w: number %d is outside 1 to %d", errNoPullRequestNamed, number, math.MaxInt32)
	}
	if !IsGitHubRepositoryName(repo) {
		// The name is quoted to its first 140 characters, the longest one GitHub can have, so a
		// name of any length costs the log line no more.
		return fmt.Errorf("%w: %.140q is not owner/name, with an owner of 1 to 39 letters, digits, hyphens and underscores and a name of 1 to 100 letters, digits, hyphens, underscores and periods",
			errNoPullRequestNamed, repo)
	}
	return nil
}

// decodePullRequestNamed is pullRequestNamed of an envelope's raw payload, decoded as route
// decodes it (flatPayload).
func decodePullRequestNamed(raw string) (repo string, number int, err error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return "", 0, fmt.Errorf("%w: %w", errMalformedPayload, err)
	}
	return pullRequestNamed(flatPayload(fields))
}

// recordRefused reports whether err is the owed record of an envelope's pull request refused
// before any statement (Intake.oweEnvelopesRead): the envelope names no pull request GitHub can
// have (checkPullRequestKey), or the payload does not decode. No
// delivery of such an envelope can get past that, so deliver ends it with one ERROR line. A
// failure of the record's statement is never one of these, whatever its SQLSTATE: the record
// holds the repository and the number alone, which its builder holds to values the table takes,
// so a failure of the insert says nothing of the envelope's values. A unique violation of its
// upsert, for one, is transient: PostgreSQL raises one while a unique index of the table is built
// or rebuilt concurrently (the page on INSERT, `ON CONFLICT`; review of cybermonklabs/legion#4209
// at ef5112db43, B5). So every failure of the statement is the database not taking the record
// now, and deliver hands the envelope back until it does: while Dispatch's database cannot take
// that record, the bus keeps the event, for as long as the bus keeps events.
func recordRefused(err error) bool {
	return errors.Is(err, errNoPullRequestNamed) || errors.Is(err, errMalformedPayload)
}

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
// subject that is not a pull request's own, a workflow run's or a push of a branch's without
// decoding it; the handler decides what to do with the rest (route), and fetches nothing for the
// ones this slice does not want.
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
// cancelled and logged, and acked: the five-minute reconcile is this package's catch-up
// mechanism, so one slow envelope must not hold the consumer. A deadline that falls on a pull
// request's head write, or on a read before it, leaves the head row unwritten (ErrHeadNotStored),
// and that delivery records the read the pull request is owed before it acknowledges (deliver).
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
// handling time, plus a minute for the acknowledgement itself to land. One handling is at most
// messageTimeout and, where its head write failed, owedReadWriteTimeout more for the write of the
// read it owes, which runs past the message's own deadline (Intake.oweEnvelopesRead):
// 4 × (60 s + 5 s) = 4 min 20 s, inside the 5 min this answers, so that write takes 20 s of the
// acknowledgement's minute. A test that shortens either on its intakes gets an ack wait derived
// from what it set.
func (t *intakeTuning) ackWait() time.Duration {
	return time.Duration(t.maxAckPending)*t.messageTimeout + time.Minute
}

// deliveryConsumerConfig is the policy a take-over creates the durable with
// (bus.DispatchDeliveryDurable, one push consumer over githubIntakeSubject), unless the deployed
// durable carries a higher generation (takeOverConfig). The server holds a backlog undelivered
// until a subscriber attaches, so a restart with events waiting loses none of them and none of
// them count against MaxAckPending in the meantime.
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
const consumerConfigGeneration = 1

// consumerGenerationKey is the consumer metadata key that holds the generation of its config. A
// durable without it was last created by a build before generations: generation 0.
const consumerGenerationKey = "dispatch.consumer-generation"

// consumerGenerationMetadata is the metadata this build creates a durable with: its generation.
func consumerGenerationMetadata() map[string]string {
	return map[string]string{consumerGenerationKey: strconv.Itoa(consumerConfigGeneration)}
}

// deliver handles one message on its key's goroutine: it routes the envelope's payload by the
// settings the cache holds, none where delivery is not configured (route), under the intake's
// messageTimeout, and acks: a malformed envelope has nothing to retry, and a route error (a
// failed GitHub call, or that deadline passing) is caught up by the next reconcile pass rather
// than by NATS redelivery -- see the package doc comment -- as a push the importer failed to take
// is by the importer's own five-minute pass. One route error is not, and is the second case
// below. The deadline is what makes the ack wait (intakeTuning.ackWait) a bound rather than a
// hope: without it one stuck GitHub call could hold an in-flight slot past the ack wait and have
// the message redelivered underneath it. An envelope route answers not handled (an empty payload,
// a kind route sends to no handler, or a push, which is no delivery fact) is acknowledged with no
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
// The other message it may not acknowledge is one whose pull request's head row could not be
// stored (ErrHeadNotStored, handlePullRequestEnvelope): the reconcile's passes over the stored
// rows do not catch that up, because they read the rows that exist (ListStalePullRequestHeads),
// and GitHub's answer is the only source of a head. So that
// delivery records that the envelope's pull request is owed a read of GitHub
// (owePullRequestRead), which the reconcile's next pass makes and makes again every pass until
// GitHub answers (Reconcile.readOwedPullRequests). The record names the pull request and nothing
// else the envelope says, and a later read that writes the head deletes it (recordPullRequestHead).
//
// The record is what lets the intake let go. Where it is written at this delivery, the envelope
// is acknowledged at once, a rate limit included: a delivery that waited for GitHub would hold one
// of the intakeMaxAckPending in-flight slots, which every repository and every installation
// shares, so four envelopes of one repository GitHub fails, or of one installation GitHub limits,
// would stop the events of all the others (review of cybermonklabs/legion#4209 at ff73c7358d,
// B3). The cost is the wait: the head comes at the reconcile's next pass, or sooner at the next
// event of that pull request or at a result's own commit lookup (ReadThroughCommitPulls).
//
// A record that is not written is refused before its statement (recordRefused) only where the
// envelope names no pull request GitHub can have, and
// the envelope then ends at that delivery with one ERROR line. Every failure of the statement is
// the database not taking a record now, whatever its SQLSTATE, since the record's builder holds
// its values to what the table takes (checkPullRequestKey), and while the database cannot take it,
// the envelope on the bus is the only memory of the change, for as long as the bus keeps events:
// the stream keeps a message 72 hours from its publish (bus.streamCfg's MaxAge,
// contracts.DeliveryDuplicateWindow), and a hand-back does not renew that. So the envelope is
// handed back with a delay (intakeDelivery.handBackOwn), for as long as that lasts: the durable
// sets no MaxDeliver, and no count of deliveries ends it, since R4 keeps which pull request
// changed until a read succeeds. Where the whole database refuses writes, no other envelope's
// head write could be stored either, but a lock of pull_request_reads_owed alone (a migration of
// it) refuses the record while other heads are still written, and for that lock's length each
// such envelope holds its slot, four of them every slot. The hand-back's WARN line names the
// record's failure, and is written at an envelope's first delivery and then once every
// handBackLineEvery deliveries (logHandBack). It is handed back now rather than at the stop:
// this message wants redelivering to this same process, which still holds the durable's hold and
// still routes that key to the one goroutine it is on. The write is idempotent (both its
// statements are `on conflict … do update`), so a redelivery of an envelope whose write had in
// fact landed writes the same pair again.
//
// That hand-back is this process's own answer to the durable, and it is made only while the
// hold still covers it (intakeDelivery.mayAnswer): a hand-back a process makes after its hold
// has lapsed reaches the durable a take-over recreated under the same name, where it would move
// the new holder's pending entry for that message and have it delivered a second time. Withheld,
// the message is not lost: it stays outstanding, and the take-over's recreate at the durable's
// ack floor hands it to the new holder once. Outside any stop, the hand-back is published;
// on a reconnecting link between attempts it is refused at once, and while an
// attempt holds nats.go's lock it waits until the attempt ends (intakeDelivery.handBackOwn).
// The acknowledgement is held to the hold the same way (intakeDelivery.holdCovers): one made
// after the lapse would settle on the new holder's consumer a message its handler had not
// handled, so a holder whose hold has lapsed answers nothing and the next holder takes the
// envelope, which redoes the same idempotent writes.
//
// An envelope no delivery can get past is not handed back at all: one that names no pull request
// GitHub can have (errNoPullRequestNamed) reads nothing and owes no read, and one whose owed record
// is refused before its statement (recordRefused), are acknowledged at their first delivery with
// one line, and the pull request is read again at its next event. A GitHub answer that holds a
// value no head row holds (errNoRowHolds, checkHeadRow) is no such envelope: it is a head not
// stored, owed a read as every other is, and the reconcile keeps that read owed until GitHub
// answers values a row holds.
//
// handling is read from the delivery, which is also what says whether that hand-back may be
// made, and nothing else calls deliver.
//
// Every line the delivery writes, its callees' included, is held until it knows its answer
// (deliveryLines): a delivery that hands its envelope back writes them under the hand-back's one
// cadence (handBackLineDue), and every other delivery at their own levels. They are written
// before NATS is answered, so a reader that sees the answer, the durable's ack floor past the
// message or its redelivery, finds the delivery's lines in the log; a line the answer itself
// writes goes straight to the log under the same decision.
func (in *Intake) deliver(d *intakeDelivery, message intakeMessage) {
	msg := message.msg
	lines := newDeliveryLines(in.tuning.log)
	log := lines.logger()
	handling := logging.NewContext(d.handling, log)
	// redeliver is set where the head row was not stored and the read it is owed could not be
	// recorded: the one answer NATS must redeliver, which the deferred answer below reads. Every
	// other path acknowledges.
	redeliver := false
	defer func() {
		const (
			answerAcknowledge = iota
			answerHandBack    // the intake stopped first: handed back for any holder's next delivery
			answerLeave       // the hold no longer covers it: left with the durable, unanswered
			answerHandBackOwn // redelivered to this process after the intake's redeliverAfter
		)
		answer := answerAcknowledge
		switch {
		case handling.Err() != nil:
			log.Warn("dispatch delivery: the intake stopped before the event's handling finished; handing it back", "subject", msg.Subject)
			answer = answerHandBack
		case !d.holdCovers(), redeliver && !d.mayAnswer():
			log.Warn("dispatch delivery: the hold no longer covers this delivery, so the envelope is left unanswered with the durable for the next holder's take-over",
				"subject", msg.Subject, "redeliver", redeliver)
			answer = answerLeave
		case redeliver:
			answer = answerHandBackOwn
		}
		_, due := handBackLineDue(msg)
		lines.write(answer == answerHandBackOwn && !due)
		switch answer {
		case answerAcknowledge:
			d.acknowledge(log, msg)
		case answerHandBack:
			d.handBack(log, msg)
		case answerHandBackOwn:
			d.handBackOwn(log, msg, in.tuning.redeliverAfter)
		}
	}()
	ctx, cancel := context.WithTimeout(handling, in.tuning.messageTimeout)
	defer cancel()

	envelope := message.envelope
	if envelope == nil {
		decoded, err := decodeEnvelope(msg.Data)
		if err != nil {
			log.Error("dispatch delivery: decode envelope", "subject", msg.Subject, "error", err)
			return
		}
		envelope = &decoded
	}
	handled, err := in.route(ctx, in.settings.Load(), envelope.Payload)
	switch {
	case errors.Is(err, errMalformedPayload):
		log.Error("dispatch delivery: decode envelope payload", "subject", msg.Subject, "error", err)
		return
	case errors.Is(err, errNoPullRequestNamed):
		log.Warn("dispatch delivery: acknowledging a pull-request envelope that names no pull request GitHub can have; it reads nothing and owes no read",
			"subject", msg.Subject, "error", err)
		return
	case errors.Is(err, ErrHeadNotStored):
		repo, number, oweErr := in.oweEnvelopesRead(handling, envelope.Payload)
		switch {
		case oweErr == nil:
			log.Warn("dispatch delivery: a pull request's head row was not stored, and the read it is owed is recorded; acknowledging the envelope, whose pull request the reconcile reads from GitHub every pass until GitHub answers",
				"subject", msg.Subject, "repo", repo, "number", number, "error", err)
		case recordRefused(oweErr):
			log.Error("dispatch delivery: the read a pull request is owed was refused before it was written; acknowledging the envelope, since no delivery of it can get past that, and the pull request is read again at its next event",
				"subject", msg.Subject, "repo", repo, "number", number, "error", err, "record_error", oweErr)
		default:
			logHandBack(log, msg, in.tuning.redeliverAfter, repo, number, err, oweErr)
			redeliver = true
		}
		return
	case err != nil:
		log.Warn("dispatch delivery: process event", "subject", msg.Subject, "error", err)
		return
	case !handled:
		return
	}
	if err := RecordEventAt(ctx, in.pool); err != nil {
		log.Error("dispatch delivery: record event freshness", "error", err)
	}
}

// headWriteRedeliverAfter is how long NATS waits before handing back an envelope whose pull
// request's head row could not be stored and whose owed read the database cannot take now either
// (deliver). It is not zero because the write that failed is the same write each time: a plain
// Nak would have the server redeliver at once, and a database that is refusing writes would then
// spin the consumer against it as fast as the round trip allows.
//
// What the delay does not do is give the in-flight slot back. A delayed Nak leaves the message
// in the consumer's ack-pending set — nats-server's processNak moves that pending entry's
// timestamp and nothing else — so one such envelope holds one of the four slots
// (intakeMaxAckPending) for every second it is waiting to come round again. No count of
// deliveries ends that: the durable sets no MaxDeliver, since such an envelope is the only record
// of its change while the database cannot take the owed record, and it is that only for as long as
// the bus keeps events, 72 hours from its publish (bus.streamCfg). What ends it is the database
// taking the owed record. Where the whole database refuses writes, no other envelope's write could
// be stored either; a lock of pull_request_reads_owed alone holds the slot while other envelopes'
// heads are written. It is the default of the intake's own redeliverAfter (intakeTuning), which a
// test shortens on the intakes it builds.
const headWriteRedeliverAfter = 10 * time.Second

// logHandBack logs through log that the envelope msg carries is handed back, to be delivered again
// in after, since its pull request's head row was not stored (headErr) and the database cannot
// take the read it is owed now (recordErr): at WARN where the hand-back's cadence is due
// (handBackLineDue), at its first delivery and once every handBackLineEvery deliveries after it,
// and at DEBUG at every other. Each envelope is delivered every headWriteRedeliverAfter while the
// fault lasts, for up to the 72 hours of the bus, and up to intakeMaxAckPending of them at once, so
// a line at each delivery would write up to 25,920 lines an envelope, where this writes it at most
// 865 times, one every 5 minutes or so. A delivery that writes it also writes, at their own
// levels, the other lines it held (deliveryLines), such as the read of the owed read's revision
// and the GitHub client's, so an envelope writes the lines of at most 865 of its deliveries above
// DEBUG, two to three lines each where such a line was held.
func logHandBack(log *slog.Logger, msg *natsgo.Msg, after time.Duration, repo string, number int, headErr, recordErr error) {
	delivered, due := handBackLineDue(msg)
	level := slog.LevelDebug
	if due {
		level = slog.LevelWarn
	}
	log.Log(context.Background(), level, "dispatch delivery: a pull request's head row was not stored, and the database cannot take the read it is owed now; handing the envelope back, since it is the only record of the change, which the stream keeps 72 hours",
		"subject", msg.Subject, "repo", repo, "number", number, "deliveries", delivered,
		"redeliver_after", after, "line_every", handBackLineEvery,
		"error", headErr, "record_error", recordErr)
}

// oweEnvelopesRead records that the pull request a `pull_request` envelope's raw payload names is
// owed a read of GitHub (owePullRequestRead, which bounds it by owedReadWriteTimeout), on
// handling and not on the message's context: the message's own deadline may be the failure of
// this delivery, and a stop still ends the write. It answers the repository and the number it read
// from the payload, which it decodes as route decoded it (decodePullRequestNamed), so deliver's
// line names them whether or not the record was written.
func (in *Intake) oweEnvelopesRead(handling context.Context, raw string) (repo string, number int, err error) {
	repo, number, err = decodePullRequestNamed(raw)
	if err != nil {
		return repo, number, err
	}
	return repo, number, in.attributor.owePullRequestRead(handling, in.pool, repo, number, owedFailure, "")
}

// handlePullRequestEnvelope processes one `kind:"pr"` envelope. Every action — opened,
// synchronize, reopened, edited and closed — records the pull request's head first
// (recordPullRequestHead, pull_request_heads.go), for any author and any repository, since a
// check result is measured against that head. The head and the state are GitHub's answer about
// the pull request the envelope names, never the envelope's own fields. A head write that could
// not be made at all is the one failure this returns to deliver (delivery.ErrHeadNotStored),
// which owes the pull request a read the reconcile makes, since the reconcile's passes read the
// rows that exist, and hands the envelope back only where the database cannot take that record
// now: both statements behind the write are `on conflict … do update`, so a redelivery writes the
// same pair. That covers the write itself, the reads of Postgres before it, and a read of GitHub
// that failed, a rate limit included. A pull request GitHub does not know, and a repository the
// App cannot read, write no row and are not failures here: the pull request is owed a read, which
// the reconcile makes again in case GitHub's answer had not caught up with its webhook
// (recordPullRequestHead). An envelope that names no pull request GitHub can have, a number
// outside 1 to 2147483647 or a repository that is no name GitHub can have (githubRepositoryName),
// is refused before anything is read or written (errNoPullRequestNamed) and returned to deliver,
// which acknowledges it, since the same envelope delivered again fails the same way.
// A GitHub answer that holds a value no head row holds is refused where the row is built, before
// any statement, and is a head not stored (errNoRowHolds, checkHeadRow).
// Only a merge (action "closed", merged "true") does anything for the delivery timeline
// after that, and only where delivery is configured (settings is not nil): a deployment with no
// settings has no population, so the head row is the whole of what an envelope is for there. The
// merge's author and excluded-repository checks run on the envelope's own fields, so a
// merge by anyone outside the population costs no GitHub call of its own. It reports whether
// it stored the pull request's delivery row: a head write is not one, and neither is a row it
// deleted or a pull request outside the population, so neither ships with anything.
func (in *Intake) handlePullRequestEnvelope(ctx context.Context, settings *DeliverySettings, payload map[string]string) (wrote bool, err error) {
	if payload["kind"] != "pr" {
		return false, nil
	}
	repoFull, number, err := pullRequestNamed(payload)
	if err != nil {
		return false, err
	}
	// What the head write read of GitHub, so the merge below reads the same pull request and
	// pages the same commits once rather than twice.
	read, headErr := in.recordPullRequestHead(ctx, payload, number)
	if errors.Is(headErr, ErrHeadNotStored) || errors.Is(headErr, errNoPullRequestNamed) {
		return false, headErr
	}
	if headErr != nil {
		logging.FromContext(ctx).Warn("dispatch delivery: record a pull request's head",
			"repo", repoFull, "number", number, "error", headErr)
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

	var fetched FetchedPullRequest
	var fetchErr error
	switch {
	case read.refused != nil:
		fetchErr = read.refused
	case read.pull != nil:
		fetched, fetchErr = completeFetchedPullRequest(ctx, in.github, read.token, owner, repo, *read.pull)
	default:
		fetched, fetchErr = FetchPullRequest(ctx, in.github, owner, repo, number)
	}
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

	if err := completePullRequest(ctx, in.pool, in.github, owner, repo, repoFull, number, fetched, read.messages); err != nil {
		return false, err
	}
	return true, nil
}

// completePullRequest finishes writing one merged pull request's complete row -- its session
// trailers and the upsert -- once its GitHub facts (FetchPullRequest's answer) are in hand. It
// resolves no issue: the statement gives the issue at each read, from the texts that the read of
// GitHub stored. The shared tail both intake's live handlePullRequestEnvelope and reconcile's
// completePartialPullRequest call, so "a live webhook completes a PR" and "reconcile completes a
// partial row" write through the exact same last steps, never two copies that can drift from each
// other. A commit fetch that fails, for any reason, returns its error without writing anything:
// completing the row from what was read would record its sessions short for good. The row stays
// as it was (still partial, for reconcile's own caller; unwritten, for intake's, whose merged-PR
// search then finds it), so the next pass retries the whole completion. A rate limit comes back
// as the *githubapp.RateLimitError, which stops reconcile's batch.
//
// read is the pull request's commit messages when the caller has already paged them — the read
// of GitHub that stored the texts did, where no higher source named a held issue — and nil when
// it has not; a non-nil read spares this the same paginated call a second time.
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
	return UpsertPullRequest(ctx, pool, DeliveryPullRequest{
		Repo: repoFull, Number: number, Title: fetched.Title, URL: fetched.URL, Author: fetched.Author,
		CreatedAt: &fetched.CreatedAt, MergedAt: fetched.MergedAt, FirstCommitAt: fetched.FirstCommitAt,
		MergeCommitSHA: fetched.MergeCommitSHA, Additions: fetched.Additions, Deletions: fetched.Deletions,
		Rework: IsRework(fetched.Title), Sessions: sessionTrailers(messages), Partial: false,
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

// maxCommitPages bounds fetchCommitMessages' pagination: 500 commits is already an enormous pull
// request, and a PR beyond that is not worth the API cost of chasing its full history.
const maxCommitPages = 5

// fetchCommitMessages reads a pull request's commit messages, in commit order: what its session
// trailers (sessionTrailers) are read from. Shared by Intake and Reconcile (both complete a PR the
// same way; reconcile must read them itself rather than carrying forward whatever a stale row
// already had, see store.go's UpsertPullRequest doc comment on why a partial row is never allowed
// to regress a complete one's sessions). On an error it answers the messages of the pages it read.
func fetchCommitMessages(ctx context.Context, client *githubapp.Client, owner, repo string, number int) ([]string, error) {
	token := func() (string, error) { return client.RepositoryToken(ctx, owner, repo) }
	messages, _, err := fetchCommitMessagesWithToken(ctx, client, token, owner, repo, number, 1)
	return messages, err
}

// fetchCommitMessagesWithToken is fetchCommitMessages asking token for each page's installation
// token, by readGitHubPage's rule. It reads from firstPage on: 1 for every message, and 2 for the
// messages after the 100 that one GraphQL query answers (readFactsBatch). It also answers how
// many commit pages it asked GitHub for, the failed one included, so a caller can count what a
// read cost.
func fetchCommitMessagesWithToken(ctx context.Context, client *githubapp.Client, token func() (string, error), owner, repo string, number, firstPage int) ([]string, int, error) {
	var messages []string
	for page := firstPage; page <= maxCommitPages; page++ {
		asked := page - firstPage + 1
		current, err := token()
		if err != nil {
			return messages, asked - 1, fmt.Errorf("mint installation token for %s/%s PR #%d commits page %d: %w", owner, repo, number, page, err)
		}
		commitsPath := fmt.Sprintf("/repos/%s/%s/pulls/%d/commits?per_page=100&page=%d", url.PathEscape(owner), url.PathEscape(repo), number, page)
		body, status, header, _, err := readGitHubPage(ctx, client, current, commitsPath)
		if err != nil {
			return messages, asked, fmt.Errorf("fetch commits of PR #%d page %d: %w", number, page, err)
		}
		if err := githubapp.CheckResponse(status, header, body); err != nil {
			return messages, asked, fmt.Errorf("fetch commits of PR #%d page %d: %w", number, page, err)
		}
		var commits []commitMessagePayload
		if err := json.Unmarshal(body, &commits); err != nil {
			return messages, asked, fmt.Errorf("decode commits of PR #%d page %d: %w", number, page, err)
		}
		for _, c := range commits {
			messages = append(messages, c.Commit.Message)
		}
		if len(commits) < 100 {
			return messages, asked, nil
		}
	}
	return messages, max(0, maxCommitPages-firstPage+1), nil
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
