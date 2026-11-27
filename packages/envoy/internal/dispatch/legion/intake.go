package legion

// intake.go is Dispatch's intake of each Legion daemon's state events (dispatch://INFRA-3931, the
// plan's D9). A daemon publishes one message per transaction it committed that moved what
// Dispatch observes, on notifications.legion.<token>.state: the project, the daemon's run, the
// first and the last state sequence of the transaction, its commit time and its events (a change
// of an issue's fields, the admission block, a boot). The intake takes them from one held durable
// (bus.DispatchLegionStateDurable, delivery.HeldDurable), so of every envoy-dispatch process one
// applies them, and writes legion_observations from them as a whole read would have written them.
//
// Each project's messages run one at a time on that project's runner (messageKey), and so does
// every whole read of the project (readWhole): a take-over's and a settings change's are queued on
// the same runner (delivery.HeldDurable.Queue), so a read never interleaves with a message the
// project applies, and its snapshot is never older than a message already applied. A message that
// arrives while a read is in flight waits on the runner behind it, for one RequestTimeout at the
// most, and is judged against the cursor that read leaves. A message of a store the project's
// cursor drops runs on the runner too, and applies nothing (cursor.drops). A project keeps at most
// stateMaxWaiting messages waiting on its runner: a message past that runs on a key of its own
// beside the runner and makes no read (judgeCrowded), and what the read in flight then leaves
// owed is cursor.afterRead's rule. A project has at most one whole read in flight, since its reads
// run on its runner, and at most one owed (cursor.readReasons). One rule decides whether a call
// for a read makes it now or owes it, and which message holds an owed read (cursor.readFor), and
// a project's whole reads start ReadRetryAfter apart at the least, whatever their reasons, the
// starting read included. legion_event_cursors says how far the project's observations have
// followed its daemon (cursor), and legion_event_parts holds the messages of a transaction the
// daemon split over several until the last one arrives (part).
//
// This file holds the intake's lifecycle and wiring: the durable and its policy, the intake's run,
// its take-over and its starting points, each message's key and delivery and what an attempt at it
// comes to (outcome), the retry of a database step, the log lines and the hourly prune of
// legion_reads. Each message's apply path is in apply.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/processes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// intakeConnectionName is the name StartIntake asks connect for, so the NATS server's connection
// listings tell the intake's connection from the delivery intakes' and from Dispatch's publisher.
const intakeConnectionName = "dispatch-legion-intake"

// stateSubject is the durable's one filter subject: every Legion deployment's state lane, from
// the generated contract the daemon's publish names too (contracts.LegionStateSubject).
const stateSubject = contracts.LegionNoticeTopicPrefix + "*" + contracts.LegionStateSuffix

// The durable's flow control. A message is applied in one short transaction, and a message whose
// apply waits on the database says so every applyLastRetry at the most (InProgress), so
// stateAckWait only redelivers a message the runner never reached in two minutes.
// stateMaxAckPending is how many messages may wait for an acknowledgement at once. One project's
// messages run one at a time, and INFRA's measured rate is about 53,000 messages a day, so 256
// messages are about seven minutes of INFRA's traffic. A database that refuses a project's writes
// holds its messages that long and fills the window, and NATS then delivers no message of any
// project until those writes go through again. The messages wait in the stream meanwhile: none is
// lost. A whole read holds its project's runner for one read at the most, since a message that
// calls for a read inside ReadRetryAfter of the last one makes none (cursor.readFor). Meanwhile the
// project keeps at most stateMaxWaiting messages waiting on its runner, and judges each message
// past that beside the runner (judgeCrowded). So a burst of messages, forged or from a bug, or a
// read the daemon does not answer, holds the part of the window that stateMaxWaiting states, and a
// message that holds its project's owed read holds one message of the window for as long as
// stateMaxHolds states.
const (
	stateAckWait       = 2 * time.Minute
	stateMaxAckPending = 256
)

// consumerConfig is the policy a take-over creates the durable with: one push consumer over every
// state lane, acknowledged by hand, delivering new messages at its first creation, since a whole
// read of each configured project is the starting point and a message older than the durable says
// nothing that read does not. A take-over of a durable that exists creates it again at its ack
// floor (delivery's takeOverStart), so a stopped process's messages are replayed.
//
// Three settings hold the rule of an owed read (cursor.readFor), by which the message that holds
// the read goes back to the stream unacknowledged (delivery.HeldMessage.HandBackAfter) until the
// read is due, and its redelivery makes the read.
//   - MaxDeliver is unset, which the server takes as -1, no limit: a redelivery never counts toward
//     a drop, so the server never drops the message that holds the read, which would leave the
//     cursor naming a holder that never comes back. The intake lets the message go itself, after
//     stateMaxHolds hand-backs, and clears the cursor's holder as it does.
//   - The delay is ReadRetryAfter at the most, a minute, below AckWait (stateAckWait, two minutes):
//     the delay and not the ack wait says when the message comes back, and a hand-back that does
//     not reach the server still brings the message back within one ack wait.
//   - MaxAckPending (stateMaxAckPending) counts the held message while it waits to come back, so
//     each project that owes a read holds one message of the window, and every other project's
//     messages go on being delivered.
//
// While the message waits, the durable's ack floor stays below it, so a take-over, which creates
// the durable again at that floor, delivers the message again and replays every message of every
// project after it, for as long as stateMaxHolds bounds the hold.
func consumerConfig() natsgo.ConsumerConfig {
	return natsgo.ConsumerConfig{
		Durable:        bus.DispatchLegionStateDurable,
		DeliverSubject: natsgo.NewInbox(),
		FilterSubject:  stateSubject,
		DeliverPolicy:  natsgo.DeliverNewPolicy,
		AckPolicy:      natsgo.AckExplicitPolicy,
		AckWait:        stateAckWait,
		MaxAckPending:  stateMaxAckPending,
	}
}

// The log lines of a state message the intake acknowledges and does not apply, and of a starting
// point it does not take, each at INFO, once per line and project per dropLogEvery (reportEvery). A
// message that goes back to the stream logs none of them (heldLine).
const (
	// droppedLine is a message for a project with no Legion setting.
	droppedLine = "legion intake: a state message for a project with no Legion setting is dropped"
	// noStoreLine is a message of a project whose cursor holds no store: every whole read since
	// the setting was written failed, or the starting read is owed and not due yet, so no read has
	// said which store its daemon counts in.
	noStoreLine = "legion intake: a state message is dropped while no good whole read has given its project a store"
	// otherStoreLine is a message whose stateId is not the cursor's, after the read it called
	// for or inside ReadRetryAfter of the last read, and a message of a store the cursor marks as
	// no daemon of the project (cursor.marks), which is finished as outcomeForgotten.
	otherStoreLine = "legion intake: a state message of another store than the starting point's is dropped"
	// forgottenLine is a message of the store the project followed before a write of its Legion
	// setting named another daemon, or before a delete of the setting, while the cursor holds no
	// store or another store (cursor.drops): R3 applies no message of the daemon the setting
	// named before.
	forgottenLine = "legion intake: a state message of the daemon the project's Legion setting named before is dropped"
	// unfollowedLine is a message that does not follow the cursor (a gap, a boot that does not
	// follow, a part that does not continue the held parts) while the read it calls for is owed:
	// the read holds its events.
	unfollowedLine = "legion intake: a state message that does not follow the cursor is dropped while a whole read is owed"
	// beyondLine is a message whose first sequence is still past the next one after a good whole
	// read it called for: the read holds every event the daemon published before it.
	beyondLine = "legion intake: a state message past the sequence of the whole read it called for is dropped"
	// crowdedLine is a message that arrived while its project's runner had stateMaxWaiting messages
	// waiting, and that joined the whole read it owes (judgeCrowded): the read holds its events.
	crowdedLine = "legion intake: a state message is dropped while its project's runner has the most messages waiting and a whole read is owed"
	// earlierBuildLine is a starting point not taken while a Dispatch process of an earlier build
	// is in the fleet, and a message of such a project, which is dropped.
	earlierBuildLine = "legion intake: a Dispatch process of an earlier build is in the fleet, so no starting point is taken"
)

// refusedLine is the log line of a message the database refused for good, at WARN, on the delivery
// that acknowledges it. A delivery that sends the message back to the stream, because it holds its
// project's owed read, logs heldLine instead, and the message's next delivery is refused again.
const refusedLine = "legion intake: the database refuses a state message for good, so it is finished and the project is read whole"

// heldLine is the log line of a message that goes back to the stream unacknowledged because it
// holds its project's owed whole read (cursor.readFor), at INFO, once each time it goes back: once
// a ReadRetryAfter for each project that owes a read, and more often only where the message comes
// back before the read is due. It names the read the message calls for (reason, "" for none), how
// many times this durable has delivered the message (delivery, which a take-over starts again at
// 1), and which of the hold's hand-backs this is as the cursor counts them (handBack, which a
// take-over keeps; stateMaxHolds). It is logged only for a hand-back HandBackAfter admits
// (delivery.HeldMessage.HandBackAfter), and such a delivery logs no line of reportEvery and no
// refusedLine; a whole read it made logs its own line before it (finishWhole).
const heldLine = "legion intake: a state message goes back to the stream until its project's owed whole read is due"

// writeReleasedLine is the log line of a message whose hold of its project's owed whole read a
// write or a delete of the project's Legion setting ended (cursor.releasedHolders), at INFO, on the
// one delivery after the write that acknowledges it unapplied, naming its stream sequence and its
// store.
const writeReleasedLine = "legion intake: a write or a delete of the project's Legion setting ended a state message's hold of the owed whole read, so it is acknowledged unapplied"

// releasedLine is the log line of a delivery that stops its message's hold of the project's owed
// whole read after stateMaxHolds hand-backs with the read still owed (readAnswer.release), at
// WARN, as it acknowledges the message, naming the hand-backs the cursor counted (handBacks) and
// how many times this durable has delivered the message (delivery). The cursor names no holder
// from then on, and the read stays owed for the project's next message, boot, settings change or
// take-over. What that delivery reads is stateMaxHolds' rule; a good read that leaves nothing owed
// clears the holder with no such line (cursor.afterRead).
const releasedLine = "legion intake: a state message went back to the stream the most times allowed for its project's owed whole read, so it is acknowledged and the read stays owed"

// dropLogEvery bounds how often one line of reportEvery is logged for one project, and pruneEvery
// how often one process prunes legion_reads (PruneReads).
const (
	dropLogEvery = time.Hour
	pruneEvery   = time.Hour
)

// A database step that fails is tried again in place, after applyFirstRetry and twice as long
// after each failure after it, up to applyLastRetry, for as long as the handling lasts, unless the
// database refused it for good (refusedForGood). NewIntake gives each intake these two as its
// firstRetry and lastRetry, which a test shrinks on its own intake.
const (
	applyFirstRetry = time.Second
	applyLastRetry  = 30 * time.Second
)

// settingsKey is the runner key a take-over queues its settings read on (takenOver). No message
// uses it: messageKey keys a message by its project, by its project's crowded key (crowdedKey) or
// by its subject, and none of them is empty.
const settingsKey = ""

// Intake applies every Legion daemon's state events to legion_observations from the durable
// bus.DispatchLegionStateDurable, while this process holds the durable's hold, and reads a
// daemon's state whole where the events cannot carry it (ReadReasonStart and the rest). It runs
// whenever Dispatch has NATS, whether or not the GitHub App is configured.
type Intake struct {
	pool    *store.Pool
	durable *delivery.HeldDurable
	// settingsWake is signalled on each write of a Legion setting (SettingsChannel); nil reads the
	// settings only at each take-over.
	settingsWake <-chan struct{}
	// now is Dispatch's clock: a whole read's rows are stamped by it, and an owed read's retry is
	// spaced by it. after runs f once, d from now on that clock (time.AfterFunc): the deferred
	// starting point of a wake or a take-over that found the starting read not due runs on it
	// (startLater). A test moves both.
	now   func() time.Time
	after func(d time.Duration, f func())
	// firstRetry is how long a database step that failed waits before it is tried again the first
	// time, and lastRetry how long at the most, the wait doubling after each failure between
	// (untilDone): applyFirstRetry and applyLastRetry. A test shrinks both.
	firstRetry, lastRetry time.Duration

	mu sync.Mutex
	// reported is when each line of reportEvery was last logged for each project, for the lines
	// logged in the last dropLogEvery: reportEvery deletes each older entry.
	reported map[reportKey]time.Time
	// pruned is when this process last pruned legion_reads.
	pruned time.Time
	// calling is each project for which a message of a store its cursor drops has queued a call for
	// the owed read on the project's runner (callRead), until that call has run, so a burst of such
	// messages queues one call. A take-over empties it (takenOver): the delivery before the
	// take-over dropped each call that still waited when it stopped (delivery.HeldDurable.Queue).
	calling map[string]bool
	// waiting counts the messages that wait on each project's runner, for the subscription the
	// durable delivers on now (waitingCounts). A take-over starts it again (takenOver).
	waiting waitingCounts
	// reading is the whole read in flight on each project's runner (readWhole, readInFlight).
	reading map[string]readInFlight
}

// reportKey is one line of reportEvery for one project.
type reportKey struct {
	line, project string
}

// NewIntake builds the intake on natsClient, a client opened for it alone (a bus.Client holds one
// JetStream subscription), holding its durable under a hold and a consumer lease it takes on leases
// (ReserveLeases), and reading the settings again on each signal of settingsWake: in the server,
// the shared listener's wake on SettingsChannel.
func NewIntake(natsClient *bus.Client, pool *store.Pool, leases delivery.DurableLeases, settingsWake <-chan struct{}) *Intake {
	in := newIntake(pool, settingsWake)
	in.durable = delivery.NewHeldDurable(natsClient, leases, consumerConfig, in.messageKey, in.deliver, in.takenOver)
	return in
}

// newIntake is an intake on pool, reading the settings again on each signal of settingsWake, with
// every field NewIntake makes but the durable: the wall clock, its deferred calls, the retry waits
// and every map. NewIntake completes it with the durable, and ReadWhole uses it as it is: its read
// runs on no runner of a durable.
func newIntake(pool *store.Pool, settingsWake <-chan struct{}) *Intake {
	return &Intake{pool: pool, settingsWake: settingsWake, now: time.Now, after: func(d time.Duration, f func()) { time.AfterFunc(d, f) },
		firstRetry: applyFirstRetry, lastRetry: applyLastRetry,
		reported: map[reportKey]time.Time{}, calling: map[string]bool{}, waiting: newWaitingCounts(), reading: map[string]readInFlight{}}
}

// ReserveLeases reserves the intake's callers of the lease pool: three, as every held durable's
// (delivery.ReserveDurableLeases) - the consumer lease its take-over runs under, its hold's
// statements and its hold's renewal, each of bus.DispatchLegionStateDurable. The server reserves
// them while it wires itself, before the lease pool's callers are complete, whether or not it then
// starts the intake, so every configuration reserves the same callers.
func ReserveLeases(callers *store.LeaseCallers) (delivery.DurableLeases, error) {
	return delivery.ReserveDurableLeases(callers, bus.DispatchLegionStateDurable)
}

// StartIntake runs an Intake until ctx is cancelled, on a bus client of its own that connect opens
// under intakeConnectionName, and returns the error of a connect that fails with nothing started.
// leases is the intake's reservation of the lease pool (ReserveLeases), settingsWake the wake
// NewIntake reads the settings again on.
//
// The returned stop, called once ctx is cancelled, waits for Run, which returns once the intake has
// stopped delivering under the intakes' stop budget from ctx's end. Then it closes the client and
// waits for each hand-back finalizer Run left behind to release the hold
// (delivery.HeldDurable.Stop). A caller that closes the database pool after stop returns
// closes it under none of the intake's writes. A second call returns once the first has.
func StartIntake(ctx context.Context, connect func(name string) (*bus.Client, error), pool *store.Pool, leases delivery.DurableLeases, settingsWake <-chan struct{}) (stop func(), err error) {
	client, err := connect(intakeConnectionName)
	if err != nil {
		return nil, fmt.Errorf("connect the Legion state intake: %w", err)
	}
	intake := NewIntake(client, pool, leases, settingsWake)
	done := make(chan struct{})
	go func() {
		defer close(done)
		intake.Run(ctx)
	}()
	return sync.OnceFunc(func() {
		<-done
		intake.durable.Stop()
	}), nil
}

// Run delivers the state events from the durable while this process holds its hold, and stands by
// for it otherwise, until ctx ends; meanwhile each signal of the settings wake queues every
// configured project's starting point (SettingsChanged). It returns once the durable's delivery
// has stopped and its hold is released (delivery.HeldDurable.Run).
func (in *Intake) Run(ctx context.Context) {
	var wakes sync.WaitGroup
	if in.settingsWake != nil {
		wakes.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-in.settingsWake:
					in.SettingsChanged(ctx)
				}
			}
		})
	}
	in.durable.Run(ctx)
	wakes.Wait()
}

// SettingsChanged queues each configured project's starting point on the project's runner
// (startingPoint). A write of a Legion setting wakes it (SettingsChannel), and each take-over
// queues it (takenOver). A message that arrives meanwhile, a message of a store the project's
// cursor drops included, waits on the runner behind that starting point and is judged against the
// cursor it leaves. Whether the starting point reads the daemon whole, and when, is the read
// rule's (startingPoint, cursor.readFor). A settings read that fails queues nothing: a project
// with no starting point is read at its next message all the same, and the listener's wake fires
// again once it listens again. A process that does not hold the durable queues nothing: the
// process that holds it heard the same notification.
func (in *Intake) SettingsChanged(ctx context.Context) {
	settings, err := ListSettings(ctx, in.pool)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("legion intake: read the Legion settings to check each project's starting point", "error", err)
		}
		return
	}
	for _, setting := range settings {
		project := setting.Project
		in.durable.Queue(project, func(ctx context.Context) { in.startingPoint(ctx, project, true) })
	}
}

// takenOver is the durable's take-over callback (delivery.NewHeldDurable): once a take-over has
// bound the durable, its first creation included, it empties the projects whose call for the owed
// read is queued (calling), since the delivery before the take-over dropped each call that still
// waited when it stopped, starts the counts of the messages that wait on each runner again at 0
// (waiting), since that delivery handed back each message that still waited, and queues
// SettingsChanged on settingsKey's runner. The callback runs on the durable's run loop, which
// waits for it, so it only queues the settings read and does not make it.
func (in *Intake) takenOver() {
	in.mu.Lock()
	clear(in.calling)
	in.waiting = newWaitingCounts()
	in.mu.Unlock()
	in.durable.Queue(settingsKey, in.SettingsChanged)
}

// startingPoint reads project's daemon whole where its observations have no starting point
// (ReadReasonStart), or where a read is owed and due and no message holds it, for the reasons it
// is owed for, and reads nothing otherwise (cursor.readFor, called with no message): where a
// message holds the owed read, the starting read included, the message's redelivery makes it. A
// settings change and a take-over queue it (SettingsChanged), and so does a message of a store the
// project's cursor drops (callRead). It takes no starting point while a Dispatch process of an
// earlier build is in the fleet (startBlocked). It runs on the project's runner; a database step
// that fails is tried again in place until it succeeds or the handling ends.
//
// wake is whether a write's wake or a take-over calls it (SettingsChanged). Such a call that finds
// the starting read not due, since the project's newest whole read began less than ReadRetryAfter
// ago, and no message holding it, queues the starting point once more for when the minute ends
// (startLater), so the read is made then and not at the project's next message alone: the read
// rule answers such a call the time left until the read is due (readAnswer.later). Where a
// message holds the starting read, it queues nothing: that message's redelivery makes the read
// once it is due (R3: "its redelivery makes the read"), and a deferred call would only join it. A
// wake or a take-over makes the starting read only where no message holds it (cursor.readFor). The
// deferred call and a message's call (callRead) pass false: each wake and each take-over defers
// one call at the most, and no timer recurs.
func (in *Intake) startingPoint(ctx context.Context, project string, wake bool) {
	var pending *wholeRead
	in.untilDone(ctx, nil, project, func(ctx context.Context) (outcome, error) {
		setting, current, found, err := in.position(ctx, project)
		if err != nil || !found {
			return outcomeDone, err
		}
		reason := ""
		if !current.follows(setting.StateURL) {
			reason = ReadReasonStart
		}
		answer := current.readFor(reason, in.now(), 0, false, nil)
		if !answer.read {
			if wake && answer.later > 0 {
				in.startLater(project, answer.later)
			}
			return outcomeDone, nil
		}
		if blocked, err := in.startBlocked(ctx, answer.reason); err != nil || blocked {
			if blocked {
				in.reportEvery(earlierBuildLine, project)
			}
			return outcomeDone, err
		}
		recorded, err := in.readWhole(ctx, answer.of(setting), &pending)
		if err != nil {
			return outcomeDone, err
		}
		if recorded == nil {
			return outcomeAgain, nil
		}
		return outcomeDone, nil
	})
}

// startLater queues project's starting point on the project's runner once more, d from now on
// the intake's clock (after): the call of a wake or a take-over that found the starting read not
// due and held by no message (startingPoint). The deferred call defers no further call, and makes
// no read where a message holds the starting read by the time it runs (cursor.readFor). A process
// that no longer delivers from the durable when d ends queues nothing (delivery.HeldDurable.Queue),
// and the process that took the durable over has queued the starting point of its own take-over.
func (in *Intake) startLater(project string, d time.Duration) {
	in.after(d, func() {
		in.durable.Queue(project, func(ctx context.Context) { in.startingPoint(ctx, project, false) })
	})
}

// startBlocked reports whether a whole read made for reason is a starting point the intake does
// not take now: ReadReasonStart while a Dispatch process of an earlier build is in the fleet
// (processes.EarlierLegionIntake). Such a build reads every daemon whole each minute and writes
// its rows at its own clock, so a row of it could land after the starting point and after the
// events applied on it. A starting point it blocks reads nothing (startingPoint), and a message
// whose starting read it blocks is acknowledged unapplied (outcomeEarlierBuild): where the cursor
// names that message as the owed read's holder, it names no holder from then on, since the
// message is acknowledged with no read made (applyMessage). The next message, settings change or
// take-over after that process has left the fleet takes the starting point; no timer does.
func (in *Intake) startBlocked(ctx context.Context, reason string) (bool, error) {
	if reason != ReadReasonStart {
		return false, nil
	}
	return processes.EarlierLegionIntake(ctx, in.pool)
}

// messageKey keys a state message by the project its payload names, so one runner applies a
// project's messages one at a time, and counts it among the messages that wait on that runner
// (waiting). A message that arrives while stateMaxWaiting of its project's messages wait is keyed
// by the project's crowded key instead (crowdedKey), and is not counted: it waits on no read of the
// project (judgeCrowded). A message whose envelope or payload does not decode, or that names no
// project, is keyed by its subject, and deliver reports it.
func (in *Intake) messageKey(msg *natsgo.Msg) (string, *contracts.Envelope, bool) {
	var envelope contracts.Envelope
	if err := json.Unmarshal(msg.Data, &envelope); err != nil {
		return msg.Subject, nil, true
	}
	var head struct {
		Project string `json:"project"`
	}
	if err := json.Unmarshal([]byte(envelope.Payload), &head); err != nil || head.Project == "" {
		return msg.Subject, &envelope, true
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.waiting.projects[head.Project] >= stateMaxWaiting {
		return crowdedKey(head.Project), &envelope, true
	}
	in.waiting.projects[head.Project]++
	in.waiting.keyed[msg] = head.Project
	return head.Project, &envelope, true
}

// outcome is what one attempt at a message, or at a project's starting point, came to.
type outcome int

const (
	// outcomeDone is a message taken: its events above the position applied, or held while more of
	// its transaction is to come (holdPart), or none of them above the position. Or work finished.
	outcomeDone outcome = iota
	// outcomeDropped is a message for a project with no Legion setting.
	outcomeDropped
	// outcomeNoStore is a message of a project whose cursor holds no store: its reads since the
	// setting was written failed, or its starting read is owed and not due yet (judge).
	outcomeNoStore
	// outcomeOtherStore is a message whose stateId is not the cursor's (judge).
	outcomeOtherStore
	// outcomeForgotten is a message of a store the project's cursor drops (cursor.drops), which is
	// never applied: drops says what it calls for and when it holds a read.
	outcomeForgotten
	// outcomeReleased is a message whose hold of its project's owed read a write of the setting
	// that named another URL, or a delete of the setting, ended (cursor.releases, endHold): it is
	// finished unapplied at its next delivery and holds no read.
	outcomeReleased
	// outcomeCrowded is a message on its project's crowded key that owes a read and joined it
	// (judgeCrowded).
	outcomeCrowded
	// outcomeUnfollowed is a message that does not follow the position, not applied while the read
	// it calls for is owed (judge).
	outcomeUnfollowed
	// outcomeBeyond is a message still past the next sequence after a good whole read it called
	// for (judge).
	outcomeBeyond
	// outcomeEarlierBuild is a message of a project with no starting point while a Dispatch
	// process of an earlier build is in the fleet (startBlocked).
	outcomeEarlierBuild
	// outcomeAgain is an attempt to make again at once: a whole read the project's setting moved
	// under, or a message whose whole read was just made (applyMessage).
	outcomeAgain
)

// deliver applies one state message on its project's runner and acknowledges it (applyMessage),
// or, where the message holds its project's owed whole read (cursor.readFor), hands it back to the
// stream unacknowledged for the time left until the read is due, so its redelivery makes the read,
// and logs that hand-back (heldLine). A hand-back HandBackAfter does not admit is not made, so
// deliver logs no heldLine for it: HandBackAfter logs its own line, and the next holder's
// take-over delivers the message again. A message that went back stateMaxHolds times is
// acknowledged at its next delivery, which logs releasedLine at WARN only where it ends the hold
// with the read still owed. A message whose hold a write of the setting ended is acknowledged
// unapplied, and logged at INFO (writeReleasedLine). A message on its project's crowded key is
// judged there and applies nothing (judgeCrowded): it is
// acknowledged, or goes back to the stream where it holds the read it owes. A message that does
// not decode is acknowledged and logged: the next one of its project is then a gap, which a whole
// read covers. A message the runner could not apply before the handling ended is handed back
// rather than acknowledged, so the next holder applies it. A message the database refused for
// good is logged at WARN once it is acknowledged (refusedLine), and a message the intake
// acknowledges unapplied is logged once per reason and project per dropLogEvery (reportEvery). As
// it returns, deliver counts the message down among those that wait on its project's runner
// (finished).
func (in *Intake) deliver(m delivery.HeldMessage) {
	defer in.finished(m.Msg)
	message, err := decodeStateMessage(m.Envelope, m.Msg.Data)
	if err != nil {
		slog.Error("legion intake: decode a state message; acknowledging it", "subject", m.Msg.Subject, "error", err)
		m.Ack()
		return
	}
	ctx := m.Context()
	seq, delivered := deliveryOf(m.Msg)
	attempt := messageAttempt{message: message, bytes: len(m.Msg.Data), msg: m.Msg, seq: seq, delivered: delivered}
	step := func(ctx context.Context) (outcome, error) { return in.applyMessage(ctx, &attempt) }
	if strings.HasPrefix(m.Key, crowdedKeyPrefix) {
		step = func(ctx context.Context) (outcome, error) { return in.judgeCrowded(ctx, &attempt) }
	}
	result, finished := in.untilDone(ctx, m.Msg, message.Project, step)
	if !finished {
		slog.Warn("legion intake: the intake stopped before a state message was applied; handing it back",
			"project", message.Project, "seq", message.Seq)
		m.HandBack()
		return
	}
	sequences := []any{"run", message.Run, "firstSeq", message.FirstSeq, "seq", message.Seq}
	if attempt.back > 0 {
		if m.HandBackAfter(attempt.back) {
			slog.Info(heldLine, append([]any{"project", message.Project, "streamSeq", attempt.seq, "delivery", attempt.delivered,
				"handBack", attempt.holds, "delay", attempt.back, "reason", attempt.reason}, sequences...)...)
		}
		return
	}
	m.Ack()
	if attempt.released {
		slog.Warn(releasedLine, append([]any{"project", message.Project, "streamSeq", attempt.seq, "delivery", attempt.delivered,
			"handBacks", attempt.holds}, sequences...)...)
	}
	if refusal := attempt.refusal; refusal != nil {
		slog.Warn(refusedLine, append([]any{"project", message.Project, "sqlstate", refusal.Code, "error", refusal.Message},
			sequences...)...)
	}
	switch result {
	case outcomeDone:
		in.pruneHourly(ctx)
	case outcomeDropped:
		in.reportEvery(droppedLine, message.Project)
	case outcomeNoStore:
		in.reportEvery(noStoreLine, message.Project, append(sequences, "stateId", message.StateID)...)
	case outcomeForgotten:
		if !attempt.cursor.marks(message.StateID) {
			in.reportEvery(forgottenLine, message.Project, append(sequences, "stateId", message.StateID)...)
			break
		}
		// A store the cursor marks as no daemon of the project is another store than the cursor's.
		fallthrough
	case outcomeOtherStore:
		in.reportEvery(otherStoreLine, message.Project, append(sequences,
			"stateId", message.StateID, "cursorStateId", logValue(attempt.cursor.stateID))...)
	case outcomeReleased:
		slog.Info(writeReleasedLine, append([]any{"project", message.Project, "streamSeq", attempt.seq, "stateId", message.StateID},
			sequences...)...)
	case outcomeCrowded:
		in.reportEvery(crowdedLine, message.Project, append(sequences, "reason", attempt.reason)...)
	case outcomeUnfollowed:
		in.reportEvery(unfollowedLine, message.Project, append(sequences,
			"lastSeq", logValue(attempt.cursor.lastSeq), "reason", attempt.reason)...)
	case outcomeBeyond:
		in.reportEvery(beyondLine, message.Project, append(sequences, "stateSeq", logValue(attempt.cursor.lastSeq))...)
	case outcomeEarlierBuild:
		in.reportEvery(earlierBuildLine, message.Project, sequences...)
	}
}

// untilDone runs step until it answers with no error, at once again for outcomeAgain, and after a
// wait for an error: a database step that fails is retried in place, so a runner never moves past
// a message it could not apply. While it waits it tells NATS the message is still in hand
// (InProgress), so the ack wait does not redeliver it. It reports false when ctx, the handling,
// ended first.
func (in *Intake) untilDone(ctx context.Context, msg *natsgo.Msg, project string, step func(context.Context) (outcome, error)) (outcome, bool) {
	wait := in.firstRetry
	for ctx.Err() == nil {
		result, err := step(ctx)
		if err == nil {
			if result == outcomeAgain {
				continue
			}
			return result, true
		}
		if ctx.Err() != nil {
			break
		}
		slog.Warn("legion intake: a database step failed; trying it again", "project", project, "retry_in", wait, "error", err)
		inProgress(msg)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
		wait = min(2*wait, in.lastRetry)
	}
	return outcomeDone, false
}

// inProgress tells NATS a message is still in hand, which restarts its ack wait. nil is work that
// holds no message.
func inProgress(msg *natsgo.Msg) {
	if msg == nil {
		return
	}
	if err := msg.InProgress(); err != nil {
		slog.Warn("legion intake: tell NATS a state message is still in hand", "subject", msg.Subject, "error", err)
	}
}

// reportEvery logs line at INFO for project, with attributes, at most once per dropLogEvery for
// each line and project. Any session may publish on a state subject, so the projects it remembers
// would grow with every name ever published: each entry older than dropLogEvery is deleted when an
// entry is written, and the map holds only the lines and projects logged in the last dropLogEvery.
func (in *Intake) reportEvery(line, project string, attributes ...any) {
	now := in.now()
	key := reportKey{line: line, project: project}
	in.mu.Lock()
	last, logged := in.reported[key]
	if logged && now.Sub(last) < dropLogEvery {
		in.mu.Unlock()
		return
	}
	maps.DeleteFunc(in.reported, func(_ reportKey, last time.Time) bool { return now.Sub(last) >= dropLogEvery })
	in.reported[key] = now
	in.mu.Unlock()
	slog.Info(line, append([]any{"project", project}, attributes...)...)
}

// logValue is *value as a log line's attribute, or "none" for nil, which a text handler would
// otherwise print as the pointer.
func logValue[T any](value *T) any {
	if value == nil {
		return "none"
	}
	return *value
}

// pruneHourly prunes legion_reads (PruneReads) after a message the intake took, at most once per
// pruneEvery in this process.
func (in *Intake) pruneHourly(ctx context.Context) {
	now := in.now()
	in.mu.Lock()
	if !in.pruned.IsZero() && now.Sub(in.pruned) < pruneEvery {
		in.mu.Unlock()
		return
	}
	in.pruned = now
	in.mu.Unlock()
	if pruned, err := PruneReads(ctx, in.pool); err != nil {
		slog.Warn("legion intake: prune the Legion state reads", "error", err)
	} else if pruned > 0 {
		slog.Info("legion intake: pruned Legion state reads", "rows", pruned)
	}
}
