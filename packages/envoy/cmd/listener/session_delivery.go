package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/listenerwire"
)

// deliveryModeConsumer is the one mode a registration is answered
// (listenerwire.DeliveryAnswer.Mode): the listener holds the session's consumer, filtered on the
// session's agent subject and its stored followed topics, and relays it on the session's relay
// subject. A registration the listener cannot give a consumer is refused (deliveryRefusal), since
// no other path reaches a session.
const deliveryModeConsumer = "consumer"

// The reasons a registration is refused for want of a session consumer (deliveryRefusal.Reason),
// short and machine-readable.
const (
	// deliveryReasonNoGrant: the listener's NATS user may not manage session consumers (a hub whose
	// grant predates them, bus.ErrConsumerNotGranted).
	deliveryReasonNoGrant = "no_grant"
	// deliveryReasonConsumerLimit: the stream holds as many consumers as its max_consumers allows,
	// so the server refuses to create this session's (bus.ErrConsumerLimitReached).
	deliveryReasonConsumerLimit = "consumer_limit"
	// deliveryReasonInvalidSessionID: the session id names no consumer (bus.SessionConsumerName).
	deliveryReasonInvalidSessionID = "invalid_session_id"
	// deliveryReasonConsumerError: the consumer could not be read, created or updated, or this
	// listener has no session-consumer connection.
	deliveryReasonConsumerError = "consumer_error"
	// deliveryReasonTooLarge: the consumer's filter makes a create or update request past the
	// server's max payload, which the bus refuses at every retry (bus.ErrTooLarge).
	deliveryReasonTooLarge = "too_large"
	// deliveryReasonNotRequested: the registration asked for no consumer, registered no serving
	// port, and came from no client this listener gives a consumer unasked (readsUnaskedRelay), so
	// nothing would deliver to the session it describes.
	deliveryReasonNotRequested = "not_requested"
)

// consumerPluginPackage and consumerPluginMinimumVersion name the release of Oh My Pi's plugin
// that first asked for the session's durable consumer, @sjawhar/pi-envoy 14.1.0
// (packages/pi-envoy/CHANGELOG.md, 14.1.0, "Consumer delivery"); every release before it receives
// from a plain NATS subscription, which this listener does not feed. A registration of that
// release or a later one is given a consumer whether or not it asks for one (readsUnaskedRelay),
// and noConsumerRemedy tells every other client that asks for none what it must do.
const (
	consumerPluginPackage        = "@sjawhar/pi-envoy"
	consumerPluginMinimumVersion = "14.1.0"
)

// sessionConsumerCallTimeout bounds the consumer calls that no client waits for: the calls of one
// relay restart, timed from when it holds its turn and the session's lock (sessionRelays.restart),
// the relays' list of every session consumer when this listener gains the durable binding
// (sessionRelays.listConsumers), the sweep's list (sessionConsumerSweep.pass), and the delete a
// reaped session's record, a deregistration or the sweep asks for, with the bucket read before it
// and the repair after it, timed from when it holds the session's lock (deleteConsumer). It also
// bounds the loop of a fenced filter write (storedFilterWrite), inside any earlier deadline of its
// ctx.
//
// It does not end a registration's consumer calls. The registration's budget bounds each of them
// and ends first: sendRequestBudget, 3.5 s from the request's arrival, which subscribeHandler takes
// (listenerDeps.registrationBudgetOr, unless a test set another budget).
//   - Each pass's read of the stream's identity and its create or update of the consumer
//     (consumerFor) also have registrationCallBudgetOr, timed from before the wait for the
//     session's lock. That bound is this timeout unless a test set another one.
//   - The repair after the session's entry is written (confirmRegistered) has the budget alone.
//   - The filter write that settles a registration the route did not answer (registerSession)
//     also has this timeout. So does an unsubscribe's filter write (refollow), inside its route's
//     sendRequestBudget.
const sessionConsumerCallTimeout = 5 * time.Second

// noGrantBackoff is how long, after a consumer call the server refused for want of a grant, every
// registration that asks for a consumer is refused no_grant without a call to the server; the next
// registration after it probes again. The backoff survives the refusal it once spared a session:
// without it a hub whose grant predates session consumers would have every heartbeat of every
// session call the server, and log a permissions violation there and here, for an answer this
// listener already knows.
const noGrantBackoff = 10 * time.Minute

// sessionConsumerDrain bounds the drain of the session-consumer connection at shutdown, inside
// what is left of the HTTP stop's budget.
const sessionConsumerDrain = 2 * time.Second

// deliveryRefusal is the answer to a registration the listener cannot give a session consumer.
// The registration is refused before anything is written, so it leaves no interest row and no
// session entry behind (subscribeHandler): a session nothing delivers to is not registered.
type deliveryRefusal struct {
	// status is 503 for a cause that may pass of itself - this listener holds no session-consumer
	// connection, or a consumer call failed - and 409 for one that will not: the registration
	// asked for no consumer, or its session id names none. The plugin's own half of this is
	// dispatch://INFRA-771: it starts with Envoy inbound unavailable, says so, and asks again at
	// each heartbeat.
	status int
	Error  string `json:"error"`
	Reason string `json:"reason"`
	// filtered is set on the one refusal decided after a consumer call was sent, the call that
	// failed (consumerFor): that call may have set the session consumer's filter to the plan it
	// was made for, so the registration that is not answered holds the filter to the row the
	// bucket holds before it returns (registerSession). Every other refusal is decided before
	// anything is sent.
	filtered bool
	// unanswered is set on the refusal of a registration whose consumer call failed because NATS
	// is reconnecting (bus.ErrReconnecting), and holds that call's error; status, Error and Reason
	// are then unset. Such a registration is not refused by name, since the bus did not answer it.
	// The route answers the error as it answers store work the bus did not answer
	// (registerSession, writeStoreError): 503 with Retry-After: 1 under busUnansweredPrefix, its
	// reason `bus: NATS is reconnecting`, which is what a new session's registration is answered
	// when its first bucket read meets the same reconnect.
	unanswered error
}

// sessionDeliveryOutcome is what sessionDelivery.register did for one registration.
type sessionDeliveryOutcome struct {
	// answer is the registration answer's delivery field, nil when the answer carries none.
	answer *listenerwire.DeliveryAnswer
	// epoch is the plugin's epoch, recorded on the entry when the session is relayed.
	epoch string
	// consumerStart is the stream sequence the session's consumer starts at, recorded on the entry
	// beside the epoch: a stored message below it is in no consumer, which the topic fan-out counts
	// as a delivery that failed (fanoutDelivery, session.SessionEntry.ConsumerStart). A message
	// on the session's own agent subject is acknowledged while the session has a record, whatever
	// this start is (agentSubjectNeedsDelivery).
	consumerStart uint64
	// ackFloor is the stream sequence the session's consumer had everything acked through when
	// this registration's consumer call read it (bus.SessionConsumerStart.AckFloor). It is where
	// the consumer comes back if a delete on another listener took it away while this
	// registration ran (confirmRegistered): the last floor anyone read of it, so the session is
	// not offered again what it had acked by then.
	ackFloor uint64
	// heard is the registration's mark (sessionDelivery.mark), taken once its topics were stored
	// and its consumer created or updated, when the session is relayed.
	heard uint64
}

// relayed reports whether the session's entry records consumer delivery (session.DeliveryConsumer):
// its consumer is held and the relay serves it, so the listener's durable skips each stored message
// on a followed topic that consumer holds. It is the answer's own mode, so the entry the
// registration writes and the answer the session reads can never say different things.
func (o sessionDeliveryOutcome) relayed() bool {
	return o.answer != nil && o.answer.Mode == deliveryModeConsumer
}

// sessionRelay is what the relay of session consumers hears from registrations, and, at the edge,
// from role claims through this listener.
type sessionRelay interface {
	// registered reports a registration of sessionID that recorded consumer delivery on its entry,
	// or, at the edge, a role claim refresh that wrote the session's record (api.go's roleSetHandler).
	// Either resumes a paused relay run and starts an ended one; a claim passes epochChanged false
	// and heard 0.
	// epochChanged is set when the entry it replaced did not hold consumer delivery with the same
	// epoch: the plugin connected anew, so the relay restarts the session's consumer at its ack
	// floor before it relays again, unless the relay's run restarted the consumer after heard,
	// the registration's mark (sessionDelivery.mark).
	registered(sessionID string, epochChanged bool, heard uint64)
}

// sessionDelivery manages the session consumers (bus.SessionConsumers) for the listener's
// registrations: it creates each one a registration asks for, keeps its filter on the session's
// agent subject and stored followed topics as they change, and tells the relay of each
// registration, and at the edge of each role claim refresh. A nil sessionDelivery, or one
// without consumers, refuses every registration with
// consumer_error; main's listener has neither, since its start ends when the session-consumer
// connection fails, and only a test builds a listener without one.
type sessionDelivery struct {
	consumers *bus.SessionConsumers
	// stream is the client whose verdict decides which topics /v1/interests/subscribe adds to a
	// session's row (splitUncapturedTopics, bus.Client.ReachesNoSession): every topic but one
	// neither the deployed stream, as the client last read its subjects, nor this build captures.
	// A consumer's filter does not read it: the filter carries every topic of the row whatever the
	// deployed stream captures (sessionConsumerFilter), so a narrowing of the stream takes no
	// topic out of it. main gives it the listener's own client. nil judges the subjects this build
	// compiles, which only a test's listener does.
	stream *bus.Client
	logger *logging.Logger
	// relay hears each registration that records consumer delivery, and at the edge each role claim
	// refresh. A sessionDelivery with consumers has one (startSessionRelays sets it before /v1 opens,
	// and main's start ends when that relay cannot start), so a registration answered consumer is relayed.
	relay sessionRelay
	// admissions is, on an edge listener, the projects of the token each session's latest
	// registration or role claim here was admitted on (subscribeHandler and the claim's admitHeld,
	// api.go), which the relay holds the session's frames to (edge_frames.go). Every other listener
	// records none.
	admissions edgeAdmissions
	// marks is the last mark taken (mark).
	marks atomic.Uint64
	// now is the clock of the no-grant backoff; time.Now unless a test sets it.
	now func() time.Time
	// clock is what a registration's consumer calls are bounded on (sessionConsumerCallTimeout in
	// consumerFor). It is nil in production and clk reads it as clock.Real then; a test that holds
	// the registration route's own bounds on a clocktest.Fake (listenerDeps.clock) hands the same
	// fake here, so how long a loaded machine takes over those calls is not what answers it.
	clock clock.Clock
	// beforeRegister runs at the top of each register call, before the session's lock is taken and
	// the consumer's filter set; nil unless a test sets it. The window between a registration's
	// plan and its filter is where another writer's row lands and makes the plan's
	// compare-and-swap fail, and only a test that can write in that window can drive the re-plan,
	// and the filter the pass after it sets, of registerSession's loop. It runs for every pass of
	// one registration, so a test that counts its calls drives each pass of that registration on
	// its own.
	beforeRegister func()
	// afterRegisterFilter runs at the end of each register call that was given a consumer, under
	// the session's lock that call returns held and before registerPass writes the interest row
	// the filter was set from; nil unless a test sets it. The window between a registration's
	// filter and its row is the one that lock closes, and only a test that can run in that window
	// can prove a relay restart never narrows the filter back to a row the registration has not
	// written yet. It runs for every pass that holds a consumer, as beforeRegister runs for every
	// pass. A call made in it that takes the same session's lock waits for the registration's pass
	// to release it, so a test drives such a call from a goroutine of its own.
	afterRegisterFilter func()
	// beforeRepairPlan and afterRepairPlan run in each pass of a repair of a session's consumer
	// (repairRegistered), the first before that pass reads the bucket's newest record and
	// interest row and the second after that read and before the consumer call that writes what
	// they ask for; nil unless a test sets them. A registration on another listener that sets
	// this consumer's filter before the read and stores its row after it is what the repair's
	// write would put an older row's filter over, and only a test holding both ends of that read
	// can build that order and prove the write is fenced on the row it was built from
	// (fenceFilterWrite). Both run for every pass of one repair, so a test that takes the window
	// once drives the first pass alone.
	beforeRepairPlan func()
	afterRepairPlan  func()
	// afterStoredRowRead runs in a write of the stored row's filter after the bucket's newest
	// interest row is read and before the consumer call that writes it: every pass of one
	// (storedFilterStep), and the relay restart's own first write, whose row the restart reads
	// (sessionRelays.storedFilter); nil unless a test sets it. A registration on another
	// listener that stores its row in that window is what the write puts an older row's filter
	// over, and only a test that can write there can build that order and prove the fence
	// corrects it (fenceFilterWrite). It runs for every such read, so a test that moves the row
	// once moves it under the first write alone.
	afterStoredRowRead func()
	// registrationCallBudget bounds a registration's own consumer call (consumerFor), timed from
	// before it waits for the session's lock, inside the registration's budget
	// (listenerDeps.registrationBudget), which alone bounds the repair its confirm makes
	// (confirmRegistered). Zero is sessionConsumerCallTimeout, which every listener main builds
	// runs with. It is a seam for a test whose own work runs inside that bound, as a session's
	// consumer lock it holds while it writes: such a test gives the call what is left of the test
	// binary's deadline here, so the bound does not count the test's work.
	registrationCallBudget time.Duration
	// locks keeps the consumer calls made for one session one at a time: a registration's create or
	// filter update, an unsubscribe's filter update, the relay's restart at the ack floor, which
	// deletes the consumer and creates it again when a message awaits an ack, and the one delete
	// a reaped session's record, a deregistration or the sweep asks for, which reads the bucket
	// under the lock, removes the consumer only where it holds no record of the session, and
	// gives it back to a session that registered meanwhile (deleteConsumer, repairRegistered).
	// An unsubscribe and a restart read the session's stored topics under the
	// lock; a registration filters on the plan it is about to store and holds the lock from that
	// filter through the plan's write and on through the write of its session entry
	// (registerPass), so a delete made on this listener runs wholly before such a
	// registration or wholly after it and never between its consumer and the entry that says the
	// session is registered. A delete on another listener takes no lock of this process, and the
	// bucket orders that one instead: each side reads what the other wrote once its own consumer
	// call is made, and whichever of the two acts last repairs (deleteConsumer,
	// confirmRegistered). So no call reads the row between a registration's filter and that
	// registration's write: a restart that did would set the filter back to a row without the topic
	// the registration is answered 200 for, and every message published on that topic until the
	// session's next heartbeat would be skipped. The lock does not order a registration's plan,
	// which is read before the lock is taken (registerSession, api.go): a pass whose plan predates
	// another registration's write stores a row that is not the plan it filtered on, and that pass
	// plans and filters again, so the filter of the pass that answers is the row it wrote. A create
	// landing between that delete and create would start the consumer at that registration's own
	// position - its resume point, or else what the stream holds for the session - rather than at
	// the floor the restart read. Under the lock a restart that read the floor and failed after its
	// delete or create leaves that floor pending, and the next call that finds the consumer missing,
	// a registration's create or the relay's retry, creates it there
	// (bus.SessionConsumers.RestartAtFloor), so in this process no restart loses the session's
	// position. A registration's hold runs on past its consumer call, until the session's entry
	// is written (register, deleteConsumer), so a delete of this listener's, which reads that
	// entry from the leader of the bucket's stream, can read the bucket neither before that
	// write nor after the entry it reads was replaced. The lock and the
	// pending start are this listener's alone: a second
	// listener (a rolling deploy's other task, or the one a session re-registers through) can still
	// create the consumer in that window at the session's resume point, or at what the stream holds
	// for the session, or run its own restart's calls between this one's, and across two tasks the
	// delete's own read of the bucket narrows that window rather than closing it.
	locks sessionLocks
	// deleteMu guards the two fields below. deletes counts the consumer deletes running off a
	// deregistration's goroutine (deleteAfterAnswer), and deletesOff is set once the shutdown
	// has stopped starting them (awaitDeletes): the deletes run on the connection the shutdown
	// drains, so one started after that drain would reach a closed connection. The three are
	// read and written in session_consumer_delete.go alone, where the delete lifecycle is; a
	// field of this type is declared with the type.
	deleteMu   sync.Mutex
	deletes    inflight
	deletesOff bool
	// grantMu guards the refusal episode below.
	grantMu sync.Mutex
	// noGrantUntil is when the registrations' calls may probe the grant again after a refusal for
	// want of it; zero while no refusal episode runs. noGrantLogged is set once the episode's one
	// warning is logged, so a hub without the grant costs one line per episode rather than one per
	// registration. A consumer call that succeeds ends the episode.
	noGrantUntil  time.Time
	noGrantLogged bool
}

// admitHeld records the projects of r's broker token as sessionID's latest admission at the edge
// (edgeAdmissions.admit), so its relay holds its frames to those projects and relays nothing to a
// session it admitted none of. It does nothing for a request no edge listener admitted
// (heldProjects), which every listener but an edge one is, nor for a nil delivery (a listener built
// without one). A registration and a role-claim refresh each record the admission of their own
// token so the relay they start here has it before the session's next registration.
func (d *sessionDelivery) admitHeld(r *http.Request, sessionID string) {
	if d == nil {
		return
	}
	if projects, held := heldProjects(r); held {
		d.admissions.admit(sessionID, projects)
	}
}

// resumeHeld, at the edge, resumes sessionID's relay as a registration does: a resume starts this
// replica's relay run for the session if it had ended for want of an admission, which the entry's
// change (a relayCheck) does not. It is for the claim refresh that just recorded the session's
// admission (admitHeld). It does nothing for a request no edge listener admitted (heldProjects),
// which every listener but an edge one is: on a hub a run ends for want of a consumer or a grant
// (restartFailed), which a claim cannot cure, and a session the claim moves here has no run, which
// apply starts on the entry's change (run == nil). It does nothing for a nil delivery or one with
// no relay either.
func (d *sessionDelivery) resumeHeld(r *http.Request, sessionID string) {
	if d == nil || d.relay == nil {
		return
	}
	if _, held := heldProjects(r); held {
		d.relay.registered(sessionID, false, 0)
	}
}

// connectSessionDelivery opens the session-consumer connection (bus.ConnectSessionConsumers) on
// urls, with inactiveThreshold the threshold every consumer created on it carries: it is given
// here, at the connection, because the threshold must be in hand before the first consumer is
// created, and this is the one place every one of them comes from. main derives it from the
// deployment's own windows (inactiveThresholdFor). The connection is handed logger's own
// slog.Logger (logging.Logger.Slog) for the same reason, so the lines its creates write of a
// start they lowered to a resume record or clamped from past the stream's end to its first
// sequence are this listener's JSON, with its machine_id. stream is the listener's own client,
// whose verdict decides which requested topics a registration adds to the session's row
// (sessionDelivery.stream); no consumer filter is judged on it (sessionConsumerFilter). options
// are the connection's own beside its name: in edge mode, the listener's inbox prefix
// (edgeGrant.connectOptions), and, from main's start, the start's credential wait
// (startSessionDelivery, natsStart.option).
//
// Every session takes its messages from the consumer this connection manages, so a failure is
// the whole start's: main ends the process with it, as it does for the live topics lane. main
// opens it through startSessionDelivery, as a connect of the start, so a refusal of the
// credential, as a hub that has yet to reload a new nkey on every server makes, is waited out
// under the start's bound before it fails; any other failure ends the start as before.
func connectSessionDelivery(urls []string, inactiveThreshold time.Duration, stream *bus.Client, logger *logging.Logger, options ...bus.ConnectOption) (*sessionDelivery, error) {
	consumers, err := bus.ConnectSessionConsumers(urls, inactiveThreshold, logger.Slog(), options...)
	if err != nil {
		return nil, err
	}
	// A change of a consumer's filter that drops a subject the new filter does not contain, while
	// the stream holds messages on it from the consumer's position on, passes them over
	// (bus.SessionConsumers.SetNarrowed): a topic of the row the filter now leaves out, which an
	// earlier build's filter carried or a later registration turned into the fifth kind. Those
	// messages were counted for the session at their publish, so the loss is named here, at every
	// change that makes it (leftOutByNarrowing), a restart's once its delete landed and before its
	// create: the dropped subjects (topics), the messages on each (pending_by_topic), and their sum
	// (pending_passed_over). Each count is read on its subject just before the change, an
	// observation that a message stored on the subject or delivered from it between the read and
	// the change makes differ from the real loss by one. A restart whose delete went unanswered
	// and whose read back of the consumer failed too reports the loss with delete_confirmed false:
	// the delete may or may not have made it. A create where no consumer stands counts nothing,
	// since nothing records the filter of the one it replaces (SetNarrowed states the bound).
	consumers.SetNarrowed(func(sessionID string, passedOver map[string]uint64, confirmed bool) {
		topics := slices.Sorted(maps.Keys(passedOver))
		var sum uint64
		for _, pending := range passedOver {
			sum += pending
		}
		attrs := []slog.Attr{slog.String("session_id", sessionID), slog.Any("topics", topics),
			slog.String("from", leftOutByNarrowing), slog.Any("pending_by_topic", passedOver), slog.Uint64("pending_passed_over", sum)}
		if !confirmed {
			attrs = append(attrs, slog.Bool("delete_confirmed", false))
		}
		logger.Warn(filterLeftOutLine, attrs...)
	})
	// Logged here rather than where it was derived: this is where the first consumer that
	// carries it becomes possible, so a listener that reaches this line has consumers to carry
	// it and one whose connection failed logs no threshold at all.
	logger.Info("session consumer inactive threshold resolved; the server removes a session consumer this long after its last pull, which stands above the sweep's own bound",
		slog.String("inactive_threshold", inactiveThreshold.String()))
	return &sessionDelivery{consumers: consumers, stream: stream, logger: logger, now: time.Now}, nil
}

// close closes the session-consumer connection, if one is open.
func (m *sessionDelivery) close() {
	if m != nil && m.consumers != nil {
		m.consumers.Close()
	}
}

// clk is the clock a registration's consumer calls are bounded on: the one a test gave m, and
// clock.Real otherwise, which every production listener is.
func (m *sessionDelivery) clk() clock.Clock {
	if m.clock == nil {
		return clock.Real()
	}
	return m.clock
}

// confirmRegistered makes sessionID's consumer what the bucket's newest record and interest row
// ask for once its registration has written the session's entry, under the lock that registration
// holds (registerPass): it creates the consumer again where a delete took it away, and sets its
// filter to the topics the newest row holds where the live one is not what those topics build. It
// is the registration's side of the invariant a session answered 200 with consumer delivery holds
// a consumer, filtered on the topics it was answered for, whatever delete raced it, on whatever
// listener.
//
// A consumer that exists is no confirmation of itself. A delete on another listener can take the
// consumer away between this registration's consumer call and its entry write and give it back
// from the record and the row an earlier registration wrote, which is a consumer without the
// topics this registration added: the session is answered 200 for them and receives nothing on
// them until its next heartbeat. That delete's own bucket read ran before this entry write, so it
// restored nothing or restored an older registration's topics; this read is after that write, so
// a delete that landed before it is repaired here, and one that lands after it reads the entry
// and the row this registration wrote and repairs itself (deleteConsumer).
//
// The repair goes through the bucket (registeredTarget) rather than through this registration's
// own topics: the record this registration wrote may itself have been deleted since, by a stop
// or a reaper pass that ran wholly after it, and a consumer created for a session the bucket no
// longer holds would stand until the stream's inactive threshold took it; and the newest row is
// the one every registration of that session was answered for, this one's and any another
// listener wrote since.
//
// The repair is the delete's own, down to the call (repairRegistered): the consumer's filters are
// judged by the call that writes them (bus.SessionConsumers.Ensure sends nothing where the live
// consumer is already what the row asks for), so this listener holds no second judgement of them
// and reads no consumer configuration of its own. ackFloor is the floor this registration's own
// consumer call read (bus.SessionConsumerStart.AckFloor), the last floor anyone read of the
// consumer, and it holds the start of a create as it does for the delete's repair; a consumer the
// stream holds keeps its own position whatever start the call names, so what a repair of it
// changes is the filter.
//
// ctx bounds the repair: it carries the registration's deadline and not its client's cancel
// (registerPass), so the repair ends inside the registration's budget (registrationBudgetOr), but
// for that budget's two gaps (subscribeHandler): each read of the connection's state made once ctx
// has ended can add up to 100 ms (bus.ConnLockWait), and a send or subscribe that meets a handshake
// begun just after its read waits for that attempt. The registration's answer waits for the repair.
func (m *sessionDelivery) confirmRegistered(ctx context.Context, d *listenerDeps, sessionID string, ackFloor uint64) {
	if m == nil || m.consumers == nil {
		return
	}
	m.repairRegistered(ctx, d, sessionID, ackFloor, repairAfterRegistration)
}

// registrationCallBudgetOr is the bound on a registration's own consumer call
// (registrationCallBudget): sessionConsumerCallTimeout unless a test set one.
func (m *sessionDelivery) registrationCallBudgetOr() time.Duration {
	if m.registrationCallBudget == 0 {
		return sessionConsumerCallTimeout
	}
	return m.registrationCallBudget
}

// repairAfterRegistration and repairAfterDelete name what a repair of a session's consumer
// follows, which its log line carries: the registration that wrote the session's entry
// (confirmRegistered) or the delete that took the consumer away (deleteConsumer).
const (
	repairAfterRegistration = "registration"
	repairAfterDelete       = "delete"
)

// repairRegistered makes sessionID's consumer what the bucket's newest record and interest row
// ask for (registeredTarget): one consumer call creates it where it is gone, sets its filter
// where the live one is not what the row builds, and sends nothing where it is already that,
// which is every repair of a session nothing raced (bus.SessionConsumers.Ensure, whose answer
// says which of the three it did). after names what this repair follows, for the log.
//
// The write is fenced on the interest row it was built from: a registration on another listener
// can store its row between this repair's read and its call, and an older row's filter landing
// over that registration's leaves the session answered 200 for a topic its consumer does not
// filter on, at the cost fenceFilterWrite's doc states.
//
// A pass that finds the bucket holding no record of the session ends the repair, and the fence
// with it: the record was deleted by a deregistration, a clean stop or a reaper pass, the same
// delete takes the consumer (deleteConsumer), and a session with no record
// has no next heartbeat to register
// again, so nothing is left to repair and nothing is left to repair it. The fence's line that
// leaves the filter to that heartbeat is wrong there, naming one that never comes, so the pass
// answers filterWriteSessionGone and the fence reports the one case that leaves something
// behind - a pass of this repair that wrote a filter before that delete - in a line of its own.
//
// A failure is logged, not returned: the caller's answer does not change, the record is deleted
// or not whatever this call did, and the session's next registration repairs the consumer again.
// The line names the session, which is what an operator reading a pause in its delivery has to go
// on.
func (m *sessionDelivery) repairRegistered(ctx context.Context, d *listenerDeps, sessionID string, ackFloor uint64, after string) {
	fenceFilterWrite(ctx, m.logger, d.registry, sessionID, after, func() (uint64, filterWriteOutcome) {
		if m.beforeRepairPlan != nil {
			m.beforeRepairPlan()
		}
		want, found := m.registeredTarget(ctx, d, sessionID, ackFloor)
		// Only registeredTargetBuilt goes on to the write below; each of the three findings
		// that build no target leaves the fence something different (registeredFinding).
		switch found {
		case registeredRecordGone:
			// The session is gone: the bucket holds no record of it, so no heartbeat of it
			// registers again and nothing sets a filter this repair leaves. Whether a pass of
			// it left one is the fence's own to know and to report (filterWriteSessionGone).
			return 0, filterWriteSessionGone
		case registeredNoConsumerDelivery:
			// The record asks for no consumer of this session - a record an earlier build
			// left, which nothing delivers to (session.DeliveryConsumer) - so this repair has
			// nothing to write and the fence nothing to judge: it leaves no delivery a filter
			// is a window in.
			return 0, filterWriteSettled
		case registeredReadFailed:
			// What the bucket holds went unread, so this pass plans nothing and puts no filter
			// anywhere; what stands is what the pass before it left (registeredTarget).
			return 0, filterWriteNotMade
		}
		if m.afterRepairPlan != nil {
			m.afterRepairPlan()
		}
		start, err := m.consumers.Ensure(ctx, sessionID, want.filter, want.start)
		switch {
		case err != nil:
			m.logger.Warn("listener could not repair the consumer of a session the bucket holds a record for",
				slog.String("session_id", sessionID), slog.String("after", after), slog.Any("filter", want.filter),
				slog.Uint64("consumer_start", want.start), slog.String("error", err.Error()))
			// A call the server answered changed nothing, so there is nothing for the fence to
			// hold; a call whose answer did not come back may have set this row's filter, and
			// the loop judges it as it judges a write that answered (consumerCallAnswered).
			if consumerCallAnswered(err) {
				return 0, filterWriteNotMade
			}
		case start.Created:
			m.logger.Warn("listener created the consumer of a session the bucket holds a record for again",
				slog.String("session_id", sessionID), slog.String("after", after), slog.Any("filter", want.filter),
				slog.Uint64("consumer_start", want.start), slog.Uint64("ack_floor", ackFloor))
		case start.Updated:
			m.logger.Warn("listener set the filter of the consumer of a session to the topics the bucket holds for it",
				slog.String("session_id", sessionID), slog.String("after", after), slog.Any("filter", want.filter))
		default:
			// The call sent nothing because the consumer is already what this row asks for, so
			// no filter of an older row stands and the fence ends here, on whatever pass. This
			// is every repair of a session nothing raced, and it costs one bucket read fewer
			// than one that wrote.
			return 0, filterWriteSettled
		}
		return want.revision, filterWriteLanded
	})
}

// registeredConsumer is what the bucket's newest record and interest row of a session ask of its
// consumer: the repair of both sides writes it (repairRegistered).
type registeredConsumer struct {
	// filter is the subjects the consumer filters on, built from the row's topics
	// (sessionConsumerFilter); a consumer call reduces them to the filters the server holds.
	filter []string
	// start is the stream sequence a create starts the consumer at. A consumer the stream holds
	// keeps the position it has, whatever start says (bus.SessionConsumers.Ensure).
	start uint64
	// revision is the KV revision of the interest row filter was built from, which the write of
	// that filter is fenced on (fenceFilterWrite); 0 where the bucket holds no row.
	revision uint64
}

// registeredFinding is what registeredTarget found of a session's record and interest row, which
// is what a repair's pass answers its fence with (repairRegistered, filterWriteOutcome). The
// three findings that build no target are three different things to that caller and not one
// false: each answers the fence with an outcome of its own (filterWriteSessionGone,
// filterWriteSettled, filterWriteNotMade), since a session the bucket holds no record of is
// gone, and the heartbeat the fence's other line promises is one that session will not make.
type registeredFinding int

const (
	// registeredTargetBuilt is a record that records consumer delivery with the session's
	// interest row read beside it: the repair writes what the two ask of the consumer.
	registeredTargetBuilt registeredFinding = iota
	// registeredRecordGone is a bucket that holds no record of the session
	// (session.SessionRegistry.Refresh answering nats.ErrKeyNotFound): a clean stop or a reaper
	// pass deleted it, and that delete takes the session's consumer too. Nothing of this
	// session is left to repair, and nothing will repair it.
	registeredRecordGone
	// registeredNoConsumerDelivery is a record that records no consumer delivery
	// (session.DeliveryConsumer): a record an earlier build left, which nothing delivers to and
	// which names no consumer to repair. No registration at this head writes one, since a
	// registration this listener can give no consumer is refused by name and the handler then
	// writes nothing (register, consumerFor).
	registeredNoConsumerDelivery
	// registeredReadFailed is a read of the record or of the interest row that did not answer.
	// What the bucket holds is unknown, so the caller plans nothing and leaves the consumer as
	// it is.
	registeredReadFailed
)

// registeredTarget reads sessionID's record and interest row from the bucket and builds what they
// ask of its consumer. It answers which of the four it found (registeredFinding): the target,
// built from a record that records consumer delivery and the row read beside it; a bucket that
// holds no record of the session; a record that records no consumer delivery and so names no
// consumer to repair; or a read that did not answer, after which the bucket's ask is unknown.
// The caller leaves the consumer as it is for the three that build no target, and tells them
// apart in what it leaves the fence driving it (repairRegistered).
//
// A record the bucket does not hold is session.SessionRegistry.Refresh's nats.ErrKeyNotFound,
// which is also its answer for a delete or purge marker, for a record past its TTL and for a
// value this build cannot decode; every other failure of that read is a read that did not
// answer, ctx's end among them.
//
// Both reads are of the bucket and not of the cache (session.SessionRegistry.Refresh,
// store.Registry.RefreshAt), since the watch that fills the cache may not have delivered the
// registration this repair answers for yet, and a cache that has not is exactly the reader that
// would leave the session with no consumer, or with one filtered on topics a registration has
// replaced: a topic it was answered 200 for would be left out, and the session would receive
// nothing on it until its next heartbeat. The row read answers the revision it read at, which the
// write built from it is fenced on (fenceFilterWrite), since the bucket orders the writers this
// listener's lock does not.
//
// Both reads are direct gets, which is where that limit costs this repair (bus.KeyValue.Get
// states it and why this build leaves it standing): with ENVOY_NATS_REPLICAS above 1 a copy one
// write behind the bucket's leader answers no record, and the session is left without a
// consumer, or answers the interest row the registration this repair answers for replaced, and
// the consumer is filtered on the older row's topics. The session's next heartbeat registers
// again, writing both and asking for the consumer its own answer promises, so the cost is the
// messages published until that heartbeat. No test here reads a lagging copy: that takes a
// cluster, and these tests run one server.
//
// The start is the later of the start the record names (session.SessionEntry.ConsumerStart) and
// the sequence after ackFloor, the floor the caller read of the consumer that is gone: the
// record's start is the sequence that registration's own answer promised the session, so nothing
// the answer named is skipped, and the floor is what the session has acked since, so nothing it
// has acked is offered again. A caller that read no floor passes 0 - the stream held no such
// consumer when it looked, or the read failed - and the start is the record's own. A record
// naming no start leaves the start to the consumer call, which then creates the consumer at the
// first message the stream holds for the session (bus.SessionConsumers.Ensure). The start this
// passes is a caller's start to that call, so a create lowers it to the floor the session's
// resume record holds plus one when that is lower and that resume record names the stream's
// identity, and clamps a start past the sequence after the stream's last to the stream's first
// sequence, so a start an earlier build or a restored stream left on the session's entry past
// the stream's end creates no consumer past it.
//
// A read that failed is logged rather than returned: the caller's answer does not change, and
// the session's next registration confirms the consumer again. The line names the session, which
// is what an operator reading a pause in its delivery has to go on. A record the bucket does not
// hold and one that records no consumer delivery are the bucket's answers and no failure, so
// neither is logged here.
func (m *sessionDelivery) registeredTarget(ctx context.Context, d *listenerDeps, sessionID string, ackFloor uint64) (registeredConsumer, registeredFinding) {
	entry, err := d.sessions.Refresh(ctx, sessionID)
	switch {
	case errors.Is(err, nats.ErrKeyNotFound):
		return registeredConsumer{}, registeredRecordGone
	case err != nil:
		m.logger.Warn("listener could not read the record of a session whose consumer it is repairing",
			slog.String("session_id", sessionID), slog.String("error", err.Error()))
		return registeredConsumer{}, registeredReadFailed
	case entry.Delivery != session.DeliveryConsumer:
		return registeredConsumer{}, registeredNoConsumerDelivery
	}
	topics, revision, err := storedTopics(ctx, d.registry, sessionID)
	if err != nil {
		m.logger.Warn("listener could not read the topics of a session whose consumer it is repairing",
			slog.String("session_id", sessionID), slog.String("error", err.Error()))
		return registeredConsumer{}, registeredReadFailed
	}
	start := entry.ConsumerStart
	if ackFloor > 0 {
		start = max(start, ackFloor+1)
	}
	filter, err := m.storedRowFilter(ctx, d.registry, sessionID, topics)
	if err != nil {
		m.logger.Warn("listener could not judge the topics of a session whose consumer it is repairing",
			slog.String("session_id", sessionID), slog.String("error", err.Error()))
		return registeredConsumer{}, registeredReadFailed
	}
	return registeredConsumer{filter: filter, start: start, revision: revision}, registeredTargetBuilt
}

// mark returns the next of the marks that order registrations against the relay's restarts: a
// registration takes one once its topics are stored and its consumer created or updated, and a
// relay restart takes one, under the session's lock, before it reads the session's topics and
// consumer. A restart whose mark is above a registration's read everything that registration
// wrote, after the plugin subscribed the connection the registration names, so every frame it
// sent reaches that connection.
func (m *sessionDelivery) mark() uint64 { return m.marks.Add(1) }

// drain drains the session-consumer connection, so the acks the relays sent reach the server,
// within sessionConsumerDrain or what is left of ctx if that is less; with nothing left of ctx it
// closes the connection and says so at WARN, since an ack the connection had not yet sent is then
// lost and the server delivers its message again. The relays are stopped first
// (sessionRelays.stop).
func (m *sessionDelivery) drain(ctx context.Context) {
	if m == nil || m.consumers == nil {
		return
	}
	timeout := sessionConsumerDrain
	if deadline, ok := ctx.Deadline(); ok {
		timeout = min(timeout, time.Until(deadline))
	}
	if timeout <= 0 {
		m.consumers.Close()
		m.logger.Warn("session consumer connection closed without a drain: the shutdown's HTTP window was spent")
		return
	}
	if err := m.consumers.Drain(timeout); err != nil {
		m.logger.Warn("session consumer connection drain error", slog.String("error", err.Error()))
	}
}

// register gives a registration the session consumer every session is delivered through,
// filtered on topics, the topics the registration's interest row will hold. One this listener
// cannot give a consumer is refused by name, and the handler then writes nothing.
//
// It returns the session's lock still held, as the func that releases it, and its caller
// releases that func once it has written the row this filter was set from and the session entry
// that follows it (registerPass): a relay restart, which rebuilds the filter from the stored
// row, then runs wholly before or wholly after a registration's pass and never between its
// filter and its row. The lock (sessionLocks) is no re-entrant one, so the pass takes it once
// and the caller releases it once, on every path out of the pass. A registration refused before
// the lock was taken returns noUnlock, so the caller releases what it was given either way.
func (m *sessionDelivery) register(ctx context.Context, body listenerwire.SubscribeRequest, topics []string) (sessionDeliveryOutcome, *deliveryRefusal, func()) {
	if m != nil && m.beforeRegister != nil {
		m.beforeRegister()
	}
	outcome, refusal, unlock := m.consumerFor(ctx, body, topics)
	if refusal == nil && m != nil && m.afterRegisterFilter != nil {
		m.afterRegisterFilter()
	}
	return outcome, refusal, unlock
}

// noUnlock releases nothing: it is the unlock a consumer call that refused a registration before
// it took the session's lock hands back.
func noUnlock() {}

// clientNamed names the client a registration came from, for the refusal above: its plugin
// package and build when it sent them (listenerwire.SubscribeRequest.Plugin, PluginVersion), and
// otherwise that it named none, which every build before those fields is. An operator reading the
// refusal needs the build to replace, and a session whose client names nothing at all is itself
// the finding.
func clientNamed(body listenerwire.SubscribeRequest) string {
	switch {
	case body.Plugin != "" && body.PluginVersion != "":
		return body.Plugin + " " + body.PluginVersion
	case body.Plugin != "":
		return body.Plugin + ", which names no version"
	case body.PluginVersion != "":
		return "a client naming version " + body.PluginVersion + " and no package"
	default:
		return "a client naming no plugin"
	}
}

// noConsumerRemedy says what the client of a registration refused not_requested must do, for the
// refusal below. That client is never @sjawhar/pi-envoy at or above consumerPluginMinimumVersion,
// which is given a consumer whether or not it asks (readsUnaskedRelay). An older pi-envoy, or one
// whose version this listener cannot date, is told the release to update to. Every other client is
// told to send the field, since only a build of its own can. Both answers name the plugin and
// version the registration carried, since an operator reading this refusal in the session's own
// transcript has nothing else to go on.
func noConsumerRemedy(body listenerwire.SubscribeRequest) string {
	named := clientNamed(body)
	if body.Plugin == consumerPluginPackage {
		return "update " + consumerPluginPackage + " to " + consumerPluginMinimumVersion +
			" or later, which this listener gives a consumer even when its registration asks for none (this registration came from " + named + ")"
	}
	return "update this client to a build that sends that field (this registration came from " + named +
		"); an Oh My Pi session needs " + consumerPluginPackage + " " + consumerPluginMinimumVersion + " or later"
}

// readsUnaskedRelay reports whether this listener gives the registration's client a consumer when
// the registration asks for none: @sjawhar/pi-envoy at or above consumerPluginMinimumVersion. Every
// such build opens its relay subject whenever it opens its agent subject, and hands each frame that
// arrives there to its consumer delivery, whether or not its registration asked for a consumer
// (receive in packages/envoy-client/src/consumer-delivery.ts, from 14.1.0 on). claude-envoy, and
// @sjawhar/pi-envoy from 17.8.0 on, ask at every registration they send, and send none while
// their relay subject is closed (registerWithRelay in
// packages/envoy-client/src/consumer-delivery.ts). The builds from 14.1.0 to 17.7.0 ask only
// while that subject is open, and those from 16.2.1 on only while no cursor of their replay
// engine is marked from_start. A pass that cannot reach NATS sets that mark, and only an answer of
// POST /v1/replay clears it, a route this listener does not serve. Refused, such a session would
// never register (dispatch://INFRA-3589, R21).
//
// Such a registration is given its consumer as if it had asked, with no epoch and no resume point.
// A consumer created for it starts where one with no resume point starts
// (bus.SessionConsumers.Ensure), which holds what is stored for the session and not yet acked. Its
// entry records the empty epoch, so its later heartbeats change no epoch and restart no relay.
// While its relay subject is closed the relay pauses, and the registration after that subject
// opens resumes it at the ack floor (sessionRelays.relay). The plugin can take a message on its
// agent subject twice, from the relay and from its core subscription, and its delivery dedupe
// shows it once (deliver in packages/pi-envoy/extensions/envoy.ts).
func readsUnaskedRelay(body listenerwire.SubscribeRequest) bool {
	return body.Plugin == consumerPluginPackage && versionAtLeast(body.PluginVersion, consumerPluginMinimumVersion)
}

// versionAtLeast reports whether version is floor or newer, both as major.minor.patch. A version
// that does not parse that way - a bundle commit, a build name, an empty field - is not: the
// refusal then names the upgrade, which is the answer that holds for a client this listener
// cannot date.
func versionAtLeast(version, floor string) bool {
	for i := range 3 {
		got, gotOK := versionPart(version, i)
		want, wantOK := versionPart(floor, i)
		if !gotOK || !wantOK {
			return false
		}
		if got != want {
			return got > want
		}
	}
	return true
}

// versionPart is the i-th dot-separated number of version, and whether it is one. A suffix on that
// number - a prerelease, a build - is left out of the comparison.
func versionPart(version string, i int) (int, bool) {
	parts := strings.Split(version, ".")
	if i >= len(parts) {
		return 0, false
	}
	digits := parts[i]
	if end := strings.IndexFunc(digits, func(r rune) bool { return r < '0' || r > '9' }); end >= 0 {
		digits = digits[:end]
	}
	number, err := strconv.Atoi(digits)
	if err != nil {
		return 0, false
	}
	return number, true
}

// consumerFor creates the session's consumer when the stream holds none and otherwise updates it
// to the current filter, before the registration answers; it refuses a registration that asks for
// none, unless its client reads its relay subject unasked (readsUnaskedRelay), and one that this
// listener can give none. The consumer is created at the sequence after its
// resume point's seq when that point carries a seq, 0 included (which gives sequence 1), and names
// the stream's current identity. The consumer call lowers that start to the floor the session's
// resume record holds plus one when that is lower and the record names the same identity, and
// clamps a start past the sequence after the stream's last to the stream's first sequence: a
// resume point above what the session's last delete recorded creates the consumer at that record,
// which hands the session everything stored since, and one past the stream's end, with no record
// to lower it, creates it at the stream's first sequence, which hands it every message the stream
// holds on its filters (bus.SessionConsumers.Ensure). The answer's consumer_start and the session
// entry's start are the start the consumer was created at (bus.SessionConsumerStart.StartSeq),
// the lowered or clamped one included. A registration with no resume point, or one with no seq or
// naming another identity, creates it at the first message the stream holds on the session's own
// agent subject, and at the stream's end only when it holds none (bus.SessionConsumers.Ensure).
// The filter is the topics the registration is about to store, so a message published on a newly
// followed topic once the registration is answered is the consumer's to deliver. It is a plan and
// not a row the bucket holds, so something judges it against the bucket afterwards every time:
// the repair that follows the session entry of a registration that is answered
// (confirmRegistered), and the settle of one that is not (registerSession).
//
// It returns the session's lock held over that filter, which the caller releases (register); a
// refusal decided before the lock was taken returns noUnlock. A refusal decided after the call
// was sent says the filter may be on the consumer (deliveryRefusal.filtered).
//
// ctx bounds its calls: the route bounds every consumer call of one registration, and its store
// work, from when it takes the request, by sendRequestBudget (subscribeHandler).
func (m *sessionDelivery) consumerFor(ctx context.Context, body listenerwire.SubscribeRequest, topics []string) (sessionDeliveryOutcome, *deliveryRefusal, func()) {
	var delivery listenerwire.SubscribeDelivery
	if body.Delivery != nil {
		delivery = *body.Delivery
	}
	if !delivery.Consumer && !readsUnaskedRelay(body) {
		// A registration that asks for no consumer, from a client that reads no relay frame it did
		// not ask for, asks for nothing this listener delivers. It is told so rather than left
		// registered and silent, with what to do about it (noConsumerRemedy, which says which
		// client each answer is for), since the session's own transcript is where whoever runs it
		// reads why its Envoy went quiet.
		return sessionDeliveryOutcome{}, &deliveryRefusal{
			status: http.StatusConflict,
			Error: fmt.Sprintf("%s asked for no session consumer: a session receives only what its own consumer holds, so register with delivery.consumer true; %s",
				body.SessionID, noConsumerRemedy(body)),
			Reason: deliveryReasonNotRequested,
		}, noUnlock
	}
	if _, ok := bus.SessionConsumerName(body.SessionID); !ok {
		return sessionDeliveryOutcome{}, &deliveryRefusal{
			status: http.StatusConflict,
			Error: fmt.Sprintf("%q names no session consumer: a session id holds no dot, star, angle bracket, slash or whitespace",
				body.SessionID),
			Reason: deliveryReasonInvalidSessionID,
		}, noUnlock
	}
	if m == nil || m.consumers == nil {
		return sessionDeliveryOutcome{}, &deliveryRefusal{
			status: http.StatusServiceUnavailable,
			Error:  "this listener holds no session-consumer connection, so it can give " + body.SessionID + " no consumer",
			Reason: deliveryReasonConsumerError,
		}, noUnlock
	}
	if m.grantRefused() {
		return sessionDeliveryOutcome{}, noGrantRefusal(body.SessionID), noUnlock
	}
	ctx, cancel := m.clk().WithTimeout(ctx, m.registrationCallBudgetOr())
	defer cancel()
	streamID, err := m.consumers.StreamID(ctx)
	if err != nil {
		return sessionDeliveryOutcome{}, m.refused(body.SessionID, err), noUnlock
	}
	var startSeq uint64
	epoch := delivery.Epoch
	if resume := delivery.Resume; resume != nil && resume.Seq != nil && resume.StreamID == streamID {
		startSeq = *resume.Seq + 1
	}
	unlock := m.locks.lock(body.SessionID)
	start, err := m.consumers.Ensure(ctx, body.SessionID, sessionConsumerFilter(body.SessionID, topics), startSeq)
	if err != nil {
		// A call the server answered - the grant it may not make, the stream's consumer cap, a
		// consumer it refused - set no filter, and the registration settles nothing for it: a
		// hub without the grant would otherwise call the server once per refused registration,
		// which the no-grant backoff exists to stop. A call whose answer did not come back may
		// have set the plan's filter, and that one is settled against the bucket's newest row
		// before the registration answers (registerSession, consumerCallAnswered).
		refusal := m.refused(body.SessionID, err)
		refusal.filtered = !consumerCallAnswered(err)
		return sessionDeliveryOutcome{}, refusal, unlock
	}
	m.granted()
	gap := start.Gap
	// The lock the caller is handed is released once, on every path out of its pass
	// (registerPass): a session lock held by nobody would stop every later call for that
	// session, this listener's relay restarts among them.
	return sessionDeliveryOutcome{
		answer: &listenerwire.DeliveryAnswer{
			Mode:          deliveryModeConsumer,
			Subject:       bus.DeliverSubject(body.SessionID),
			StreamID:      streamID,
			Gap:           &gap,
			ConsumerStart: start.StartSeq,
		},
		epoch:         epoch,
		consumerStart: start.StartSeq,
		ackFloor:      start.AckFloor,
		heard:         m.mark(),
	}, nil, unlock
}

// noGrantRefusal is the refusal of sessionID's registration for want of the grant that manages
// session consumers, from a call the server refused or from the backoff that call started.
func noGrantRefusal(sessionID string) *deliveryRefusal {
	return &deliveryRefusal{
		status: http.StatusServiceUnavailable,
		Error:  "this listener may not manage session consumers, so it can give " + sessionID + " none",
		Reason: deliveryReasonNoGrant,
	}
}

// consumerLimitRefusal is the refusal of sessionID's registration because the stream already
// holds as many consumers as its max_consumers allows, so the server refuses to create this
// session's. It names the cap, since that is the one thing an operator reading the session's
// transcript has to change; the session itself can do nothing about it.
func consumerLimitRefusal(sessionID string) *deliveryRefusal {
	return &deliveryRefusal{
		status: http.StatusServiceUnavailable,
		Error: "the " + bus.Stream + " stream holds as many consumers as its max_consumers allows, so it can give " +
			sessionID + " none and that session receives nothing: raise the stream's max_consumers",
		Reason: deliveryReasonConsumerLimit,
	}
}

// refused is the refusal of a registration whose consumer call for sessionID failed with err:
// every cause but one may pass of itself, so those are 503. A refusal for want of a grant starts
// the no-grant backoff (noGrantBackoff), or extends the episode running (grantRefusal). The one
// that cannot pass is a filter whose request the bus refuses for its size (bus.ErrTooLarge): the
// same registration is refused the same way at every retry, so it is a 413 naming the filter's
// size and the bound, which no client retries. A call that failed because NATS is reconnecting is
// no refusal by name (deliveryRefusal.unanswered).
func (m *sessionDelivery) refused(sessionID string, err error) *deliveryRefusal {
	if m.grantRefusal("create or update a session consumer", sessionID, err) {
		return noGrantRefusal(sessionID)
	}
	if errors.Is(err, bus.ErrTooLarge) {
		m.logger.Warn("the session consumer's filter is too large for the bus; the registration is refused",
			slog.String("session_id", sessionID), slog.String("error", err.Error()))
		return &deliveryRefusal{
			status: http.StatusRequestEntityTooLarge,
			Error: fmt.Sprintf("the session consumer of %s cannot be created or updated: %s. A wildcard topic that covers many of the topics makes the filter smaller",
				sessionID, err),
			Reason: deliveryReasonTooLarge,
		}
	}
	if consumerLimitReached(err) {
		// No backoff, unlike no_grant: the cap refuses only the creates, so a session whose
		// consumer the stream already holds is still answered, and a listener-wide backoff would
		// refuse those sessions too. The server answers each refused create as an ordinary API
		// error, with no permissions violation logged on either side, and the stream drops below
		// its cap the moment an operator raises max_consumers or an idle consumer is removed
		// (bus.SessionConsumerInactiveThreshold), so every heartbeat asks again and the session is
		// answered as soon as there is room. One WARN per refused registration is what the
		// consumer_error arm this takes over from already logged.
		m.logger.Warn("the stream holds as many consumers as its max_consumers allows; the registration is refused until max_consumers is raised",
			slog.String("session_id", sessionID), slog.String("stream", bus.Stream), slog.String("error", err.Error()))
		return consumerLimitRefusal(sessionID)
	}
	if errors.Is(err, bus.ErrReconnecting) {
		// The bus did not answer, so the route answers and logs the error as it does for store
		// work the bus did not answer (registerSession), and the client reads the reconnect.
		return &deliveryRefusal{unanswered: fmt.Errorf("give %s its session consumer: %w", sessionID, err)}
	}
	m.logger.Warn("session consumer call failed; the registration is refused",
		slog.String("session_id", sessionID), slog.String("error", err.Error()))
	return &deliveryRefusal{
		status: http.StatusServiceUnavailable,
		Error:  "the session consumer of " + sessionID + " could not be read or created",
		Reason: deliveryReasonConsumerError,
	}
}

// grantRefusal reports whether err is the server's refusal of a consumer call for want of a
// grant, and when it is, starts the backoff that episode runs under (noGrantBackoff) or extends
// the one running, and logs the episode's one warning. doing names the call that met the
// refusal and sessionID the session it was for, "" for a call that is for all of them. Every
// path that calls the server asks here - a registration's create or update, a deregistration's
// or the sweep's delete, the sweep's own list - so a listener whose NATS user may not manage
// session consumers at all, Dispatch's and every host's, costs one line and one probe per
// episode rather than one of each per call.
func (m *sessionDelivery) grantRefusal(doing, sessionID string, err error) bool {
	if !consumerGrantRefused(err) {
		return false
	}
	m.grantMu.Lock()
	m.noGrantUntil = m.now().Add(noGrantBackoff)
	first := !m.noGrantLogged
	m.noGrantLogged = true
	m.grantMu.Unlock()
	if !first {
		return true
	}
	attrs := []slog.Attr{
		slog.String("doing", doing),
		slog.String("error", err.Error()),
		slog.String("probe_again_in", noGrantBackoff.String()),
	}
	if sessionID != "" {
		attrs = append(attrs, slog.String("session_id", sessionID))
	}
	m.logger.Warn("the NATS user may not manage session consumers; every registration is refused until the grant carries them, and a consumer already on the stream waits out the server's inactive threshold", attrs...)
	return true
}

// consumerGrantRefused reports whether err is the server's refusal of a consumer call for want of a
// grant. bus.ErrConsumerNotGranted always wraps bus.ErrPublishDenied, so the one test covers both.
func consumerGrantRefused(err error) bool {
	return errors.Is(err, bus.ErrPublishDenied)
}

// consumerLimitReached reports whether err is the server's refusal of a consumer create because
// the stream holds as many consumers as its max_consumers allows (bus.ErrConsumerLimitReached,
// which bus wraps around the server's jetstream.ErrMaximumConsumersLimit).
func consumerLimitReached(err error) bool {
	return errors.Is(err, bus.ErrConsumerLimitReached)
}

// grantRefused reports whether a refusal for want of a grant was met less than noGrantBackoff
// ago, so a registration is answered no_grant without asking the server.
func (m *sessionDelivery) grantRefused() bool {
	m.grantMu.Lock()
	defer m.grantMu.Unlock()
	return !m.noGrantUntil.IsZero() && m.now().Before(m.noGrantUntil)
}

// granted ends a refusal episode once a consumer call succeeds: the grant now carries session
// consumers, and a later refusal is logged again.
func (m *sessionDelivery) granted() {
	m.grantMu.Lock()
	defer m.grantMu.Unlock()
	m.noGrantUntil = time.Time{}
	m.noGrantLogged = false
}

// refollow sets the filter of sessionID's consumer to its stored interest topics after a change
// that removed some (an unsubscribe), when the session's entry records consumer delivery; a
// session whose entry records none, an entry the previous build wrote, or whose consumer is gone,
// is left alone, and its next registration sets the filter. Every other failure than the one
// below is logged, not returned: the consumer then still delivers a topic the session no longer
// follows, and the plugin drops a frame no followed topic matches.
//
// The one failure it returns is a write whose last pass failed because NATS is reconnecting
// (bus.ErrReconnecting, storedFilterWrite), with the fence's end after it where storedFilterWrite
// names one. Every other end storedFilterWrite answers - the session's lock not free within the
// budget, the fence's own ends - is logged there and not returned here. The route answers the
// reconnect as store work the bus did not answer (writeStoreError), as a registration's consumer
// call that fails the same way is answered (deliveryRefusal.unanswered): the client reads the
// reconnect. envoy-client's retry removes the same topics again and sets the filter; a caller
// whose retry finds the topics already gone, as Dispatch's unsubscribeSession does, calls this
// route no more, and the filter waits for the session's next registration. ctx bounds the write:
// it is the route's request context, bounded by sendRequestBudget from when the route took the
// request.
//
// The topics are read from the bucket, not from the cache, and under the session's lock, as
// register's consumer call is made under it: the lock orders this listener's own calls, so a
// registration here that stored more topics and wrote its filter first is not overwritten with
// the set read before it, and the bucket read is what carries a registration on another
// listener, whose row this listener's watch need not have delivered yet. The write is fenced on
// the row it was built from; that fence, the session's lock, the call budget and the listener
// that holds no session consumers at all are storedFilterWrite's, since each of its callers
// needs the same four.
//
// The session's known-session record is not this function's: it names the topics the session
// follows, which an unsubscribe changes whatever the session's entry records, while this filter
// names the ones its consumer carries. Each call site writes the record itself
// (recordFollowedTopics).
func (m *sessionDelivery) refollow(ctx context.Context, d *listenerDeps, sessionID string) error {
	entry, err := d.sessions.Get(sessionID)
	if err != nil || entry.Delivery != session.DeliveryConsumer {
		return nil
	}
	if err := m.storedFilterWrite(ctx, d.registry, sessionID, filterAfterUnsubscribe); errors.Is(err, bus.ErrReconnecting) {
		return fmt.Errorf("set the filter of %s's session consumer: %w", sessionID, err)
	}
	return nil
}

// recordFollowedTopics brings sessionID's known-session record up to date with the topics its
// interest row now holds in the bucket (storedTopics), which is what a publish counts its
// recipients against (session.KnownSessions): every route that changes a session's interests
// calls it, so a topic a session has left stops being counted. The record is about the topics the
// session follows, not about the consumer's filter, so nothing here reads the entry's delivery
// mode, and a listener with no session-consumer connection writes it too; the filter is
// refollow's, beside this.
//
// The rest of the record is the session's entry, as the registration wrote it. A session the
// registry holds no entry for writes nothing: it is stopped or gone, and a stop keeps the record
// it had (deleteSessionHandler). A listener whose known-session bucket is not open writes nothing
// either. A failed write is this WARN and nothing else: the route answers whatever the write did.
func recordFollowedTopics(ctx context.Context, d *listenerDeps, sessionID string, logger *logging.Logger) {
	entry, err := d.sessions.Get(sessionID)
	if err != nil {
		return
	}
	topics, _, err := storedTopics(ctx, d.registry, sessionID)
	if err != nil {
		logger.Warn("known-session record left unchanged: the session's interests could not be read",
			slog.String("session_id", sessionID), slog.String("error", err.Error()))
		return
	}
	if err := d.known.RecordWithin(ctx, session.KnownSession{
		SessionID:    sessionID,
		Title:        entry.Title,
		Capabilities: entry.Capabilities,
		Topics:       topics,
	}); err != nil {
		logger.Warn("known-session record not written after an interest change",
			slog.String("session_id", sessionID), slog.String("error", err.Error()))
	}
}

// sessionLocks holds one lock per session id, kept only while somebody holds or waits for it.
type sessionLocks struct {
	mu    sync.Mutex
	locks map[string]*sessionLock
}

type sessionLock struct {
	sync.Mutex
	// users counts the callers holding or waiting for the lock, under sessionLocks.mu.
	users int
}

// lock takes sessionID's lock and returns the func that releases it.
func (l *sessionLocks) lock(sessionID string) (unlock func()) {
	held := l.join(sessionID)
	held.Lock()
	return l.releaser(sessionID, held)
}

// lockWithin is lock waiting no longer than ctx: it answers ctx's error, holding nothing, when
// the lock is not free before ctx ends. A wait it gave up on takes the lock once it is free and
// lets it go at once, so the lock passes on as it would have.
func (l *sessionLocks) lockWithin(ctx context.Context, sessionID string) (unlock func(), err error) {
	held := l.join(sessionID)
	release := l.releaser(sessionID, held)
	taken := make(chan struct{})
	go func() {
		held.Lock()
		close(taken)
	}()
	select {
	case <-taken:
		return release, nil
	case <-ctx.Done():
		go func() {
			<-taken
			release()
		}()
		return nil, ctx.Err()
	}
}

// join counts a caller in on sessionID's lock, creating the lock for its first caller.
func (l *sessionLocks) join(sessionID string) *sessionLock {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.locks == nil {
		l.locks = map[string]*sessionLock{}
	}
	held := l.locks[sessionID]
	if held == nil {
		held = &sessionLock{}
		l.locks[sessionID] = held
	}
	held.users++
	return held
}

// releaser is the func that releases held, sessionID's lock, and counts its caller out.
func (l *sessionLocks) releaser(sessionID string, held *sessionLock) func() {
	return func() {
		held.Unlock()
		l.mu.Lock()
		held.users--
		if held.users == 0 {
			delete(l.locks, sessionID)
		}
		l.mu.Unlock()
	}
}

// sessionConsumerFilter is the subjects sessionID's consumer filters on: its agent subject in the
// old form (contracts.LegacyAgentSubject), and every one of topics, the topics of the session's
// interest row, whatever the deployed stream captures. It reads no subjects of the deployed
// stream, so every build of the filter - a registration's (consumerFor), each rebuild from the
// stored row (storedRowFilter: storedFilterStep, sessionRelays.storedFilter, registeredTarget)
// and both passes of a fenced write - builds the same filter from the same row, and a narrowing of
// the stream, a hub move's quiesce or an operator's edit, takes no topic out of it. nats-server
// takes a filter subject the stream does not capture, on a create and on an update, and delivers
// on it once the stream captures it again, so a message stored on a topic of the row after a
// narrowing ends reaches the session (internal/bus's
// TestASessionConsumerKeepsANarrowedFilterSubjectOnAThreeCopyStream). A topic only a newer
// build's subjects capture, which this build stores on the session's row while the two builds
// run, is in the filter too, and a topic an earlier build's row left that no subject captures
// enters it and matches nothing stored. A Legion controller's notice lane
// (notifications.legion.<project>.controller) is one of the row's topics while the session holds
// that project's controller role, and this filter is the whole of how it reaches the controller,
// so a notice published while the controller was away is delivered from the stream when it
// returns. SessionConsumers reduces the list to filters that do not overlap.
//
// These kinds of topic are left out, though the row may store them:
//
//   - a topic the server would refuse as a filter subject (bus.CheckFilterSubject: an empty token,
//     as in a GitHub topic that spells a repository name with a dot, or a `>` before the last
//     token): with it in the list the server refuses the whole call and the session gets no
//     consumer at all, and no message can reach such a subject anyway;
//   - a role lane or the exception lane of one (bus.RoleLane): the stream never stores either, and
//     a role message reaches its holder as a forward on the holder's agent subject;
//   - a topic with a `*` or `>` token that reaches a private stream subject other than the
//     session's own (wildcardReachesAnotherSession), such as notifications.> or
//     notifications.agent.*.<another session>, which would hand the session another's messages. A
//     registration never stores one (store.MergedTopics), but an earlier build's row can still
//     hold one until a write of the row drops it. A literal topic is never of this kind, so a
//     controller notice lane the session holds stays;
//   - a topic that does not begin with the literal bus.NotificationTopicPrefix: a literal one
//     matches nothing the stream stores, and one with a wildcard first token, such as
//     *.team.foo, which matches notifications.team.foo too, would merge with a notification topic
//     it overlaps (notifications.team.*) into a filter over every subject;
//   - every wildcard topic that fed a filter of the reduction of the rest
//     (bus.SessionConsumerReduction) that reaches another session's private subjects
//     (leaveOutLeakingMerges): the reduction replaces two overlapping filters neither contains with
//     their common prefix and `>`, so two wildcards that each pass the private rule alone, such as
//     notifications.envoy.exceptions.foo.* and notifications.envoy.exceptions.*.bar, which an
//     earlier build's row may hold, would reduce to notifications.envoy.exceptions.>.
//
// The last two kinds are no topic the session can follow as its row spells it, so none of them
// leaves silently: sessionFilter returns each with what made it go (filterLeftOut), a
// registration keeps it out of the row it stores and names it in its answer and a WARN line
// (planWithoutLeftOut, subscribeHandler), a rebuild from a stored row an earlier build wrote
// logs the same line (storedRowFilter), a publish counts no session as a recipient through such a
// topic of a row or a known-session record an earlier build stored (recipientCounts), and the
// durable path skips such a session for a message it matches only through one, with the same line
// once per session and topic in fanoutLeftOutLogWindow (skipLeftOutInterest).
func sessionConsumerFilter(sessionID string, topics []string) []string {
	filter, _ := sessionFilter(sessionID, topics)
	return filter
}

// sessionFilter is sessionConsumerFilter of topics, and the topics of topics it leaves out by its
// fourth and fifth kinds, each with what made it go: those outside bus.NotificationTopicPrefix in
// the order of topics, then those leaveOutLeakingMerges leaves out, in the order it finds them.
func sessionFilter(sessionID string, topics []string) ([]string, []filterLeftOut) {
	filter := make([]string, 0, len(topics)+1)
	filter = append(filter, contracts.LegacyAgentSubject(sessionID))
	var leftOut []filterLeftOut
	for _, topic := range topics {
		if bus.CheckFilterSubject(topic) != nil || bus.RoleLane(topic) || wildcardReachesAnotherSession(sessionID, topic) {
			continue
		}
		if !strings.HasPrefix(topic, bus.NotificationTopicPrefix) {
			leftOut = append(leftOut, filterLeftOut{topic: topic})
			continue
		}
		filter = append(filter, topic)
	}
	filter, merges := leaveOutLeakingMerges(sessionID, filter)
	return filter, append(leftOut, merges...)
}

// wildcardReachesAnotherSession reports whether topic, a topic of sessionID's interest row, has a
// `*` or `>` token and reaches a private stream subject that is not the session's own
// (bus.ReachesPrivateStreamSubject, read with no roles). The session's own agent subject in the
// new form under a wildcard project (notifications.agent.*.<session>) is its own and does not. A
// literal topic never does here: the rule read with no roles counts a controller notice lane as
// private whatever the session holds, and the row keeps such a lane only while its claim names the
// session (store.MergedTopics), which is the rule's own reading with the session's roles.
func wildcardReachesAnotherSession(sessionID, topic string) bool {
	return hasWildcardToken(topic) && bus.ReachesPrivateStreamSubject(sessionID, topic, nil)
}

// hasWildcardToken reports whether topic has a `*` or `>` token.
func hasWildcardToken(topic string) bool {
	tokens := strings.Split(topic, ".")
	return slices.Contains(tokens, "*") || slices.Contains(tokens, ">")
}

// leaveOutLeakingMerges is filter without the wildcard topics that fed a filter of its reduction
// (bus.SessionConsumerReduction, the filter set the consumer carries, forms of each topic
// included) that has a wildcard token and reaches a private stream subject other than
// sessionID's own, and those topics with what made each go. The reduction tracks, for each filter
// it makes, the topics whose forms went into it (bus.ReducedSessionFilter.Fed): a filter that is
// one topic's form as it came is that topic's own, and one the reduction merged was fed by every
// topic a form of which went into that merge or a merge before it. So a merge that reaches
// another session takes out exactly the wildcard topics that fed it, through a chain of merges and
// through a topic's other form (contracts.TopicForms) alike, and a topic the merged filter only
// contains, such as notifications.envoy.exceptions.notifications.github.> beside the pair
// notifications.envoy.exceptions.foo.* and notifications.envoy.exceptions.*.bar, fed nothing and
// stays.
//
// A literal topic is never left out: it overlaps another topic only when that one contains it,
// so it merges with nothing as it is spelled, and the only wildcard forms a literal has, the old
// form of an agent subject or of its exception lane read under every project
// (notifications.agent.*.<session>), name only the subjects that literal names. Such a form that
// reaches another session, the form of another session's agent subject an earlier build's row
// left, is that literal's own filter and leaves nothing out. Every merge has a wildcard topic
// among what fed it, since two literals' forms never overlap without one containing the other.
//
// Each pass reduces the filter, leaves out every topic so found, and reduces again, until a pass
// finds none: each pass that finds one leaves out at least one wildcard topic, so the loop ends,
// and a row with no such filter is reduced once and left as it is.
func leaveOutLeakingMerges(sessionID string, filter []string) ([]string, []filterLeftOut) {
	var leftOut []filterLeftOut
	for {
		var found []filterLeftOut
		isFound := func(topic string) bool {
			return slices.ContainsFunc(found, func(left filterLeftOut) bool { return left.topic == topic })
		}
		for _, reduced := range bus.SessionConsumerReduction(filter) {
			if !hasWildcardToken(reduced.Subject) || !bus.ReachesPrivateStreamSubject(sessionID, reduced.Subject, nil) {
				continue
			}
			for _, topic := range reduced.Fed {
				if !hasWildcardToken(topic) || isFound(topic) {
					continue
				}
				mates := slices.DeleteFunc(slices.Clone(reduced.Fed), func(other string) bool { return other == topic })
				found = append(found, filterLeftOut{topic: topic, merged: reduced.Subject, mates: mates})
			}
		}
		if len(found) == 0 {
			return filter, leftOut
		}
		leftOut = append(leftOut, found...)
		filter = slices.DeleteFunc(slices.Clone(filter), isFound)
	}
}

// filterLeftOut is a topic of a session's interest row that its consumer filter leaves out by the
// fourth or fifth kind sessionConsumerFilter names, with what made it go. merged is "" for a topic
// that does not begin with the literal bus.NotificationTopicPrefix (the fourth kind); for a
// wildcard topic of the fifth kind it is the filter of the reduction that topic fed, which reaches
// another session's private subjects, and mates the other topics that fed it, in the order of the
// filter.
type filterLeftOut struct {
	topic  string
	merged string
	mates  []string
}

// filterLeftOutLine is the WARN line the listener writes for each topic a session's consumer
// filter leaves out by the fourth or fifth kind (logFilterLeftOut).
const filterLeftOutLine = "listener left a topic out of the session's consumer filter"

// The values of the from attribute of filterLeftOutLine: the path that left the topic out.
const (
	leftOutByRegistration = "registration"
	leftOutOfStoredRow    = "stored_row"
	leftOutByFanout       = "fanout"
	// leftOutByNarrowing is the line of a change of a consumer's filter, an update or a restart at
	// the ack floor, that passed over messages on subjects it dropped: it names them as topics,
	// with the messages read on each (pending_by_topic) and their sum (connectSessionDelivery).
	leftOutByNarrowing = "narrowing"
)

// kind is the kind attribute of filterLeftOutLine: no_notifications_prefix for the fourth kind,
// merges_into_private for the fifth.
func (l filterLeftOut) kind() string {
	if l.merged == "" {
		return "no_notifications_prefix"
	}
	return "merges_into_private"
}

// warning is the sentence a registration's answer names l's topic in (subscribeHandler): that it
// was not registered for sessionID, and why.
func (l filterLeftOut) warning(sessionID string) string {
	switch {
	case l.merged == "":
		return fmt.Sprintf("%s was not registered: the consumer filter of %s carries only topics that begin with the literal %s, since the stream stores nothing outside it and a topic with a wildcard first token could widen the filter through its reduction; follow a topic that begins with %s instead",
			l.topic, sessionID, bus.NotificationTopicPrefix, bus.NotificationTopicPrefix)
	case len(l.mates) == 0:
		return fmt.Sprintf("%s was not registered: the consumer filter of %s would carry it as %s, which reaches another session's private stream subjects",
			l.topic, sessionID, l.merged)
	default:
		return fmt.Sprintf("%s was not registered: the consumer filter of %s would merge it with %s into %s, which reaches another session's private stream subjects, so each wildcard topic of that merge is left out; follow topics that do not overlap instead",
			l.topic, sessionID, strings.Join(l.mates, ", "), l.merged)
	}
}

// logFilterLeftOut writes filterLeftOutLine at WARN for each of leftOut, naming sessionID, the
// topic, its kind, the merged filter of the fifth kind, and from, the path that left it out.
func logFilterLeftOut(logger *logging.Logger, sessionID string, leftOut []filterLeftOut, from string) {
	if logger == nil {
		return
	}
	for _, left := range leftOut {
		logger.Warn(filterLeftOutLine, leftOutAttrs(sessionID, left, from)...)
	}
}

// leftOutAttrs is the attributes of filterLeftOutLine for left, a topic of sessionID's row, left
// out by the path from.
func leftOutAttrs(sessionID string, left filterLeftOut, from string) []slog.Attr {
	attrs := []slog.Attr{slog.String("session_id", sessionID), slog.String("topic", left.topic), slog.String("kind", left.kind()), slog.String("from", from)}
	if left.merged != "" {
		attrs = append(attrs, slog.String("merged", left.merged))
	}
	return attrs
}

// fanoutLeftOutLogWindow is how often the durable path logs filterLeftOutLine for one session and
// one left-out topic of its row (skipLeftOutInterest): once per ten minutes. A live session's row
// holds such a topic only as an earlier build stored it, and the session's next registration, at
// its next heartbeat (90 s by default), takes the topic out, so each such row is named once.
//
// A stopped session's row stays until the interest reaper deletes it (store.Registry.Reap, a pass
// every 5 minutes), which it does once the row is older than store.InterestReapAllowance, 15
// minutes after its last write, and the session reads not live (sessionMayBeLive). That gives two
// cases. Where the session bucket keeps its default maximum age (session.BucketMaxAge, 5 minutes),
// the session's record lapses 5 minutes after its last registration, it then reads not live, and
// the row goes 15 to 20 minutes after its last write: one listener names it at most twice. Where
// the deployment sets a maximum age past the gone horizon (WithSessionMaxAge), the record stands
// and notGone holds the session live until session.GoneHorizon, an hour after its last
// registration: the row goes 60 to 65 minutes after it, and one listener names it at most seven
// times. In either case a busy topic costs at most six lines an hour for one session and topic,
// where a line per message would flood the log, and every such row is named at least once.
const fanoutLeftOutLogWindow = 10 * time.Minute

// registrationLeftOutLogWindow is how often a registration logs filterLeftOutLine for one session
// and one topic it leaves out (subscribeHandler): once an hour. A plugin sends every topic it
// follows again on each heartbeat (SESSION_HEARTBEAT_MS, 90 s), so a followed topic of the fourth
// or fifth kind would log a line every 90 s for as long as the session runs; and no heartbeat
// path reads the answer's warnings (pi-envoy's subscribe tool alone shows them, when a caller
// names a topic by hand), so the line tells the operator and the answer tells nobody more by
// being repeated. The answer itself names the topic on every registration.
const registrationLeftOutLogWindow = time.Hour

// leftOutLogPruneAt is the size at which a limiter drops the entries older than its window, which
// allow no longer reads, so its memory follows the pairs logged in the last window and not every
// pair logged since the listener started.
const leftOutLogPruneAt = 1024

// leftOutLogLimiter remembers when a path last logged each session and left-out topic, and lets
// it log one again once window has passed.
type leftOutLogLimiter struct {
	window time.Duration
	mu     sync.Mutex
	last   map[[2]string]time.Time
	// now is the limiter's clock, time.Now when nil; a test sets it, with a fresh last, through
	// isolate, under mu, since the handlers that log read both as they run.
	now func() time.Time
}

// allow reports whether sessionID's topic may be logged now, and records now when it may: it may
// when that session and topic were not logged within the limiter's window.
func (l *leftOutLogLimiter) allow(sessionID, topic string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if l.now != nil {
		now = l.now()
	}
	key := [2]string{sessionID, topic}
	if at, logged := l.last[key]; logged && now.Sub(at) < l.window {
		return false
	}
	if l.last == nil {
		l.last = make(map[[2]string]time.Time)
	}
	if len(l.last) >= leftOutLogPruneAt {
		for pair, at := range l.last {
			if now.Sub(at) >= l.window {
				delete(l.last, pair)
			}
		}
	}
	l.last[key] = now
	return true
}

// isolate gives the limiter the clock now and a memory of its own, with nothing logged yet, and
// returns the func that puts back the clock and the memory it had. A test isolates the limiter it
// reads, so a run sees none of the entries an earlier run or another test stored, whose times its
// moved clock would otherwise read as inside the window.
func (l *leftOutLogLimiter) isolate(now func() time.Time) (restore func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	previousNow, previousLast := l.now, l.last
	l.now, l.last = now, nil
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.now, l.last = previousNow, previousLast
	}
}

// fanoutLeftOutLogs is the one limiter of this process's durable path, and registrationLeftOutLogs
// the one of its registrations: every handler built from any configuration shares them, as they
// share the stream and the buckets they read.
var (
	fanoutLeftOutLogs       = leftOutLogLimiter{window: fanoutLeftOutLogWindow}
	registrationLeftOutLogs = leftOutLogLimiter{window: registrationLeftOutLogWindow}
)

// logFilterLeftOutLimited is logFilterLeftOut for the topics limiter lets through
// (leftOutLogLimiter.allow).
func logFilterLeftOutLimited(logger *logging.Logger, limiter *leftOutLogLimiter, sessionID string, leftOut []filterLeftOut, from string) {
	if logger == nil {
		return
	}
	for _, left := range leftOut {
		if limiter.allow(sessionID, left.topic) {
			logger.Warn(filterLeftOutLine, leftOutAttrs(sessionID, left, from)...)
		}
	}
}

// skipLeftOutInterest reports whether the stored message on subject is one interest's session is
// no recipient of: interest's row matches subject only through topics the session's consumer
// filter leaves out by its fourth or fifth kind (carriedFor), which a row an earlier build stored
// can hold until the session's next registration takes them out. Its consumer does not carry the
// message, so the durable path must not take it for a session whose consumer relays it; it skips
// the session, and says so: for each such topic of the row that matches subject, it logs
// filterLeftOutLine from fanout with the subject, at most once per session and topic within
// fanoutLeftOutLogWindow (fanoutLeftOutLogs). Both places the durable path judges an interest
// call it: durableNeedsDelivery, which acknowledges a message every matching session's consumer
// holds before decoding it, and fanoutDelivery. The row's left-out topics come from the cache the
// publish count shares (filterLeftOuts), so a busy topic reduces each such row once.
func skipLeftOutInterest(logger *logging.Logger, interest store.Interest, subject string) bool {
	if carriedFor(interest.SessionID, interest.Topics, subject) {
		return false
	}
	leftOut := filterLeftOuts.of(interest.SessionID, interest.Topics)
	for _, left := range leftOut {
		if !slices.ContainsFunc(contracts.TopicForms(left.topic), func(form string) bool { return bus.SubjectContains(form, subject) }) {
			continue
		}
		if logger != nil && fanoutLeftOutLogs.allow(interest.SessionID, left.topic) {
			logger.Warn(filterLeftOutLine, append(leftOutAttrs(interest.SessionID, left, leftOutByFanout), slog.String("subject", subject))...)
		}
	}
	return true
}

// filterTakes reports whether sessionFilter keeps topic on its own: a filter subject the server
// takes (bus.CheckFilterSubject), no role lane or exception lane of one (bus.RoleLane), and a topic
// that begins with the literal bus.NotificationTopicPrefix. What the deployed stream captures
// plays no part: the filter holds a stored topic whatever the stream captures now
// (sessionConsumerFilter). It leaves out the two kinds that turn on more than the topic: a wildcard
// reaching a private stream subject, which the row's rule leaves out with the controller roles the
// session holds (store.LaneHolders.Keeps), and a wildcard that fed a merge reaching another
// session's subjects, which only the whole row tells (sessionFilter, carriedFor).
func filterTakes(topic string) bool {
	return bus.CheckFilterSubject(topic) == nil && !bus.RoleLane(topic) && strings.HasPrefix(topic, bus.NotificationTopicPrefix)
}

// storedTopicFiltered reports whether the consumer filter built from a stored row of sessionID
// can hold topic, a topic of that row or of the session's known-session record: the row's rule
// keeps it, with the controller roles the session holds read from their claim records
// (store.LaneHolders.Keeps), and the filter takes it (filterTakes). It is the per-topic half of the
// one judgment of a stored topic: storedRowFilter builds every filter of a stored row by it, and a
// publish counts a session only through a topic it holds that the row's leaking merges leave in
// (recipientJudgment.keeps, carriedFor), so the count and the filter cannot judge a stored topic
// apart. holders is the claims of the lanes the caller's answer turns on
// (store.Registry.ReadLaneHolders); a lane whose role it weighed no claim of is held by nobody.
func storedTopicFiltered(holders store.LaneHolders, sessionID, topic string) bool {
	return holders.Keeps(sessionID, topic) && filterTakes(topic)
}

// storedRowFilter is the filter of sessionID's consumer built from topics, its interest row as
// read from registry: sessionConsumerFilter of the topics the row's rule keeps
// (store.LaneHolders.Keeps), so it leaves out each topic reaching a private stream subject that is
// not the session's own (another session's inbox, a failed-delivery lane, a controller notice lane
// whose role the session does not hold, a wildcard reaching one) as well as each kind
// sessionConsumerFilter leaves out. It counts each topic of the filter that is in the old form of
// its name (store.Registry.CountStoredLegacyTopics): a stored row read into a filter is a reader
// of the old form for as long as the row holds one. Every read of a stored row into a filter comes
// through here (registeredTarget, storedFilterStep, sessionRelays.storedFilter); a registration's
// own plan does not, since the subscribe route judged its topics and counted their old forms as it
// took them (store.MergedTopics). A row is a record any writer of the bucket can store, so the
// rule a registration applies is applied to what is read here again: a row under the session's own
// key that names another session's inbox would otherwise put that session's messages in this
// session's consumer at its next restart. The claims of the controller roles whose lanes the row
// names are read from the role bucket stream's leader within ctx (store.Registry.ReadLaneHolders),
// and an error is returned for a read that did not answer or failed: the caller builds no filter
// then. A topic the filter leaves out by its fourth or fifth kind is one a registration of this
// build keeps out of the row (planWithoutLeftOut), so the row that holds one is an earlier build's
// or another writer's, and each is logged (filterLeftOutLine, from stored_row) until the session's
// next registration takes it out of the row and names it.
func (m *sessionDelivery) storedRowFilter(ctx context.Context, registry *store.Registry, sessionID string, topics []string) ([]string, error) {
	holders, err := registry.ReadLaneHolders(ctx, topics)
	if err != nil {
		return nil, fmt.Errorf("decide whether %s holds a controller notice lane: %w", sessionID, err)
	}
	kept := slices.DeleteFunc(slices.Clone(topics), func(topic string) bool { return !holders.Keeps(sessionID, topic) })
	filter, leftOut := sessionFilter(sessionID, kept)
	if m != nil {
		logFilterLeftOut(m.logger, sessionID, leftOut, leftOutOfStoredRow)
	}
	registry.CountStoredLegacyTopics(sessionID, filter)
	return filter, nil
}
