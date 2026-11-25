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
// cursor drops (cursor.drops) runs on the runner too: it applies nothing and calls for no read
// of its own, and the one read it can call for is queued on the runner behind it (callRead). A
// project keeps at most stateMaxWaiting messages waiting on its runner: a message past that runs on
// a key of its own beside the runner, makes no read and owes one. The read in flight leaves the gap
// read owed where a message past the cap that joined it carries a sequence of the read's store
// above its snapshot, the store read owed where such a message is of another store that the cursor
// the read leaves does not drop, and otherwise the first such message's redelivery is judged as any
// message's (judgeCrowded, cursor.afterRead). A message that holds the owed read, whatever store it is
// of, makes it at its redelivery or goes back to the stream again: no message is acknowledged while
// the cursor names it as the holder, unless the read rule releases it (cursor.readFor), a write of
// the setting ended its hold (endHold), or a Dispatch process of an earlier build keeps the intake
// from the starting point the message calls for (applyMessage), where the holder is released first.
// So a project has at most one whole read in flight; it has at most one owed (cursor.readReasons),
// and one rule decides whether a call for a read makes it now or owes it (cursor.readFor). Every
// two whole reads of a project start ReadRetryAfter apart at the least, whatever their reasons,
// the starting read included: a starting read that a write's wake or a take-over finds not due is
// made when the minute ends, by one deferred call (startingPoint). legion_event_cursors says how
// far the project's observations have followed its daemon (cursor), and legion_event_parts holds
// the messages of a transaction the daemon split over several until the last one arrives (part).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
// stateAckWait only redelivers a message the runner never reached in two minutes. stateMaxAckPending
// is how many messages may wait for an acknowledgement at once. One project's messages run one at a
// time, and INFRA's measured rate is about 53,000 messages a day, so 256 messages are about seven
// minutes of INFRA's traffic. A database that refuses a project's writes holds its messages that
// long and fills the window, and NATS then delivers no message of any project until those writes go
// through again. The messages wait in the stream meanwhile: none is lost. A whole read holds its
// project's runner for one read at the most, since a message that calls for a read inside
// ReadRetryAfter of the last one makes none (cursor.readFor): the first such message goes back to
// the stream until the read is due, and every later one is acknowledged once the cursor records its
// reason. Meanwhile the project keeps at most stateMaxWaiting messages waiting on its runner: each
// message past that owes a read, one of them holds it, and the others are acknowledged
// (judgeCrowded). So a burst of messages, forged or from a bug, or a read the daemon does not
// answer, holds stateMaxWaiting+1 messages of the window for one read at the most, and each project
// that owes a read holds one message of it, for stateMaxHolds hand-backs at the most.
const (
	stateAckWait       = 2 * time.Minute
	stateMaxAckPending = 256
)

// consumerConfig is the policy a take-over creates the durable with: one push consumer over every
// state lane, acknowledged by hand, delivering new messages at its first creation, since a whole
// read of each configured project is the starting point and a message older than the durable says
// nothing that read does not. A take-over of a durable that exists creates it again at its ack
// floor (delivery's takeOverConfig), so a stopped process's messages are replayed.
//
// Three settings hold the rule of an owed read (cursor.readFor): the message that holds the read
// goes back to the stream unacknowledged (delivery.HeldMessage.HandBackAfter) for the time left
// until the read is due, and its redelivery makes the read. A read that fails sends the message
// back until ReadRetryAfter after that read began. Once the message has gone back stateMaxHolds
// times, its next delivery is acknowledged whatever its read comes to, and the cursor names no
// holder from then on.
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
// project after it. stateMaxHolds bounds that: the cursor counts the message's hand-backs, so a
// take-over does not reset the count, and each hand-back after a read lasts until ReadRetryAfter
// after that read began, holdAtLeast at the least. A take-over's own call for the project's read
// (takenOver, startingPoint) makes no read while the message holds it, so no take-over adds a read
// to the hold. A read lasts RequestTimeout at the most, a minute, so the message holds the floor
// for about 11 minutes at the most in all, whether or not the daemon answers its reads and however
// many take-overs there are, and a take-over replays at most that long of messages.
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
// message that goes back to the stream logs none of them: its delivery logs heldLine alone.
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
	// setting named another daemon, while the cursor holds no store or another store
	// (cursor.drops): R3 applies no message of the daemon the setting named before.
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
// project's owed read, logs heldLine alone, and the message's next delivery is refused again.
const refusedLine = "legion intake: the database refuses a state message for good, so it is finished and the project is read whole"

// heldLine is the log line of a message that goes back to the stream unacknowledged because it
// holds its project's owed whole read (cursor.readFor), at INFO, once each time it goes back: once
// a ReadRetryAfter for each project that owes a read, and more often only where the message comes
// back before the read is due. It names the read the message calls for (reason, "" for none), how
// many times this durable has delivered the message (delivery, which a take-over starts again at
// 1), and which of the hold's hand-backs this is as the cursor counts them (handBack, which a
// take-over keeps; stateMaxHolds). It is the one line of that delivery, and is logged only for a
// hand-back made (delivery.HeldMessage.HandBackAfter): such a message is not logged as
// acknowledged unapplied (reportEvery) or as refused.
const heldLine = "legion intake: a state message goes back to the stream until its project's owed whole read is due"

// writeReleasedLine is the log line of a message whose hold of its project's owed whole read a
// write or a delete of the project's Legion setting ended (cursor.releasedHolders), at INFO, on the
// one delivery after the write that acknowledges it unapplied, naming its stream sequence and its
// store.
const writeReleasedLine = "legion intake: a write or a delete of the project's Legion setting ended a state message's hold of the owed whole read, so it is acknowledged unapplied"

// releasedLine is the log line of a message that went back to the stream stateMaxHolds times
// holding its project's owed whole read, at WARN, on the delivery that acknowledges it, naming
// the hand-backs the cursor counted (handBacks) and how many times this durable has delivered the
// message (delivery). The cursor names no holder from then on, and the read stays owed for the
// project's next message, boot, settings change or take-over.
const releasedLine = "legion intake: a state message went back to the stream the most times allowed for its project's owed whole read, so it is acknowledged and the read stays owed"

// dropLogEvery bounds how often one line of reportEvery is logged for one project, and pruneEvery
// how often one process prunes legion_reads (PruneReads).
const (
	dropLogEvery = time.Hour
	pruneEvery   = time.Hour
)

// A database step that fails is tried again in place, after applyFirstRetry and twice as long
// after each failure after it, up to applyLastRetry, for as long as the handling lasts, unless the
// database refused it for good (refusedForGood). Vars so a test can shrink them.
var (
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
	in := &Intake{pool: pool, settingsWake: settingsWake, now: time.Now, after: func(d time.Duration, f func()) { time.AfterFunc(d, f) },
		reported: map[reportKey]time.Time{}, calling: map[string]bool{}, waiting: newWaitingCounts(), reading: map[string]readInFlight{}}
	in.durable = delivery.NewHeldDurable(natsClient, leases, consumerConfig, in.messageKey, in.deliver, in.takenOver)
	return in
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
// cursor it leaves. A project with no starting point (no read yet, or a write of its setting that
// left its cursor following no daemon: PutSettings) is read whole once, when that read is due: at
// once, or ReadRetryAfter after the project's newest whole read began, for which the call queues
// the starting point once more (startLater). A project whose cursor owes a read that is due and
// that no message holds is read for the reasons it owes it for, and every other project is not
// read at all. Where a message holds the owed read, the starting read included, the call joins
// that read, which the message's redelivery makes, and queues nothing; a write of the URL the
// setting names already leaves such a cursor as it is (settingWritten), so its wake joins the read
// too. A settings read that fails queues nothing: a project with no starting point is read at its
// next message all the same, and the listener's wake fires again once it listens again. A process
// that does not hold the durable queues nothing: the process that holds it heard the same
// notification.
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
// (startLater), so the read is made then and not at the project's next message alone. Where a
// message holds the starting read, it queues nothing: that message's redelivery makes the read
// once it is due (R3: "its redelivery makes the read"), and a deferred call would only join it. A
// wake or a take-over makes the starting read only where no message holds it: before a message
// takes it, or once the holder is released (its stateMaxHolds hand-backs, a write of another URL
// or a delete of the setting, or a starting read an earlier build blocks). The deferred call and a
// message's call (callRead) pass false: each wake and each take-over defers one call at the most,
// and no timer recurs.
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
		now := in.now()
		answer := current.readFor(reason, now, 0, false, nil)
		if !answer.read {
			if wake && reason == ReadReasonStart && current.readHolder == nil {
				in.startLater(project, current.readDueIn(now))
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
// events applied on it. The next message, settings change or take-over after that process has left
// the fleet takes the starting point; no timer does.
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
	// outcomeForgotten is a message of a store the project's cursor drops (cursor.drops): the
	// store the project followed before a write of its setting named another daemon, or a store a
	// whole read found is no daemon of the project. A boot of such a store, while the cursor
	// follows the setting's URL in a store a read verified, is none: it is judged as a message of
	// another store (outcomeOtherStore). It is never applied. It waits on the project's
	// runner behind a read in flight, as every message does, and is judged against the cursor that
	// read leaves: it queues a call for the owed read behind itself where that read is due and no
	// message holds it, whatever store the cursor holds (callRead;
	// judge, applyBatch). On its project's crowded key it calls for none while no whole read of the
	// project is in flight, and otherwise owes the read as every message past stateMaxWaiting does
	// (judgeCrowded). A message the cursor names as the owed read's holder, which one past
	// stateMaxWaiting of any store can be (judgeCrowded), is not finished so: it makes that read
	// when it is due and goes back to the stream otherwise, as every holder does (applyBatch,
	// answerRead, readRefused).
	outcomeForgotten
	// outcomeReleased is a message whose hold of its project's owed read a write of the setting
	// that named another daemon ended (cursor.releases, endHold): it is finished unapplied at its
	// next delivery and holds no read.
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
// and logs that hand-back and nothing else (heldLine). A hand-back withheld because a stop began or
// the hold lapsed is not made, so deliver logs no heldLine for it: the hand-back's own line is the
// delivery's one line (delivery.HeldMessage.HandBackAfter), and the next holder's take-over
// delivers the message again. A message that went back stateMaxHolds times
// holds the read no more: it is acknowledged, and logged at WARN (releasedLine). A message whose
// hold a write of the setting ended is acknowledged unapplied, and logged at INFO
// (writeReleasedLine). A message on its project's crowded key is judged there and applies nothing
// (judgeCrowded): it is acknowledged, or goes back to the stream where it holds the read it owes. A
// message that does not decode is acknowledged and logged: the next one of its project is then a
// gap, which a whole read covers. A message the runner could not apply before the handling ended
// is handed back rather than acknowledged, so the next holder applies it. A message the database
// refused for good is logged at WARN once it is acknowledged (refusedLine), and a message the
// intake acknowledges unapplied is logged once per reason and project per dropLogEvery
// (reportEvery). As it returns, deliver counts the message down among those that wait on its
// project's runner (finished).
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

// endHold finishes, inside tx, the message at stream sequence seq whose hold of project's owed read
// a write of the setting ended (cursor.releases): it takes seq out of released_holders and keeps
// every other message recorded there, each of which its own next delivery finishes; it marks
// stateID, the message's store, as no daemon of the project (foreign_state_ids, cursor.marking)
// unless it is the store the write recorded or the cursor's own store, and commits. current is the
// cursor tx locked, so no other write of the row lands between. The message is acknowledged
// unapplied and holds no read (outcomeReleased).
func endHold(ctx context.Context, tx pgx.Tx, project string, current cursor, seq int64, stateID string) (outcome, error) {
	if _, err := tx.Exec(ctx, `
		update legion_event_cursors set released_holders = $2, foreign_state_ids = $3 where project = $1
	`, project, without(current.releasedHolders, seq), current.marking(stateID)); err != nil {
		return outcomeDone, fmt.Errorf("end the hold of a state message of %s: %w", project, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return outcomeDone, fmt.Errorf("commit the end of the hold of a state message of %s: %w", project, err)
	}
	return outcomeReleased, nil
}

// callRead is the read a message of a store project's cursor drops (outcomeForgotten) calls for, on
// current, the cursor its batch holds (applyBatch): the message has waited on the project's runner
// behind any read in flight, so current is the cursor that read left. The message calls for no read
// of its own, but it may be the setting's own daemon's: until a good read of that daemon has given
// the cursor a store, no read has shown whether it serves another store or the same store behind
// another URL, and a daemon the cursor follows in another store can be redeployed into the store a
// write recorded (previousStateID) with no write of the setting, after which each of its messages
// is of a store the cursor drops (read-rule-cases row 4.3). Its id is the old daemon's, so the
// intake cannot tell the two apart. So where a call no message makes would make a read now
// (cursor.readFor: an owed read that is due and that no message holds), whatever store the cursor
// holds, callRead queues the project's starting point on the project's runner behind the message
// (startingPoint), which makes that read as such a call. Such a read is owed already, and the read
// rule makes it at most once a minute, so an old daemon that still publishes adds no read the
// project does not owe. The message does not wait for it, never holds it, and is acknowledged
// unapplied at once; the read rule starts the project's whole reads ReadRetryAfter apart. At most
// one such call waits per project (calling), until it has run, so the messages of such a store that
// the runner judges meanwhile join it and queue none: a read the daemon never answers lasts
// RequestTimeout, which is ReadRetryAfter, so each call queued behind it would find the read due
// again and make one more, back to back on the project's runner.
func (in *Intake) callRead(project string, current cursor) {
	if !current.readFor("", in.now(), 0, false, nil).read {
		return
	}
	in.mu.Lock()
	if in.calling[project] {
		in.mu.Unlock()
		return
	}
	in.calling[project] = true
	in.mu.Unlock()
	called := func() {
		in.mu.Lock()
		delete(in.calling, project)
		in.mu.Unlock()
	}
	if !in.durable.Queue(project, func(ctx context.Context) {
		defer called()
		in.startingPoint(ctx, project, false)
	}) {
		called()
	}
}

// messageAttempt is one message on its runner, across the attempts untilDone makes at it.
type messageAttempt struct {
	message stateMessage
	bytes   int
	msg     *natsgo.Msg
	// seq is the message's sequence in the stream, which names it as the message that holds its
	// project's owed read (cursor.readHolder), and delivered how many times this durable has
	// delivered it, which the log lines name: a take-over creates the durable again and starts it
	// again at 1, so the hold's own count is the cursor's (stateMaxHolds). Both are read off the
	// message's JetStream metadata (deliveryOf).
	seq       int64
	delivered uint64
	// pending is a whole read this message made that is not recorded yet (readWhole), readMade
	// whether one was recorded (makeRead), after which the message calls for no read of its own
	// (ownRead) and its delivery makes no other read (cursor.readFor), and readGood whether that
	// read was a good one, after which a message still past the next sequence is no event of the
	// daemon read (judge).
	pending  *wholeRead
	readMade bool
	readGood bool
	// cursor is the project's cursor as the last batch read it (applyBatch, judgeCrowded), and
	// reason the whole read that batch found the message calls for (judge, judgeCrowded), or the
	// read a message the database refused calls for (readRefused), which the log line of a message
	// held or acknowledged unapplied names.
	cursor cursor
	reason string
	// back is how long the message goes back to the stream for, unacknowledged: above zero where it
	// holds its project's owed read, and 0 where it is acknowledged. released is whether it went
	// back stateMaxHolds times and the cursor no longer names it as the read's holder. holds is
	// which of the hold's hand-backs the cursor counts this one as, where it goes back, and how
	// many it counted, where it is released. answered sets the three from the read rule's answer
	// (answerRead), and applyBatch and readRefused call it only once the write that records the
	// answer has committed.
	back     time.Duration
	released bool
	holds    int
	// refusal is the database's refusal for good of the message's batch, once there is one: the
	// message is not tried again, and one whole read is made of its project (readRefused).
	refusal *pgconn.PgError
}

// answered sets the message's back, released and holds from answer, the read rule's answer for it
// (answerRead). applyBatch and readRefused call it once the write that recorded answer has
// committed, and never before.
func (a *messageAttempt) answered(answer readAnswer) {
	a.back, a.released, a.holds = answer.back, answer.release, answer.holds
}

// deliveryOf is msg's sequence in the stream (bus.Stream), which tells the message that holds its
// project's owed read from every other one (cursor.readHolder), and how many times this durable
// has delivered it (NumDelivered), which the log lines name. It is 0 and 0 for a message with no
// JetStream metadata, which no read rule holds: no delivery of the durable is one.
func deliveryOf(msg *natsgo.Msg) (seq int64, delivered uint64) {
	meta, err := msg.Metadata()
	if err != nil {
		return 0, 0
	}
	return int64(meta.Sequence.Stream), meta.NumDelivered
}

// applyMessage is one attempt at a message: its batch (applyBatch), or the whole read the batch
// found is to be made first (cursor.readFor). The read waits on the daemon, so it is made outside
// the batch's transaction (makeRead), and the attempt is then made again: the batch decides once
// more on the cursor the read left. A read that is owed and not due is recorded owed by the batch
// (answerRead), which takes the message or leaves it unapplied as judge says, and the message goes
// back to the stream where it holds the owed read (messageAttempt.back). A starting point is not
// taken while a Dispatch process of an earlier build is in the fleet (startBlocked): the message is
// acknowledged unapplied, and where the cursor names it as the owed read's holder (a message past
// stateMaxWaiting holds the read of a cursor that follows no daemon: judgeCrowded), the cursor
// names no holder from then on (owe, release), since the message is acknowledged with no read
// made. A batch the database refuses for good (refusedForGood) is not tried again: it calls for one
// whole read of the project (readRefused), and deliver logs the refusal.
func (in *Intake) applyMessage(ctx context.Context, attempt *messageAttempt) (outcome, error) {
	if attempt.refusal != nil {
		return in.readRefused(ctx, attempt)
	}
	result, read, err := in.applyBatch(ctx, attempt)
	if refusal := refusedForGood(err); refusal != nil {
		attempt.refusal = refusal
		return in.readRefused(ctx, attempt)
	}
	if err != nil || read == nil {
		return result, err
	}
	if blocked, err := in.startBlocked(ctx, read.reason); err != nil || blocked {
		if blocked && attempt.cursor.heldBy(attempt.seq) {
			err = owe(ctx, in.pool, attempt.message.Project, readAnswer{release: true}, attempt.seq)
		}
		return outcomeEarlierBuild, err
	}
	return in.makeRead(ctx, attempt, *read)
}

// readRefused is the whole read a message the database refused for good calls for
// (ReadReasonRefused): the read's rows then hold the daemon's state, the refused message's events
// among it, since the daemon commits each event before it publishes it. The read rule decides it as
// it decides every read a message calls for (answerRead), and what the rule owes is recorded
// through the pool, since no batch is open. A read that is due is made (makeRead), and readRefused
// then decides again on the cursor the read left, the message calling for no read of its own once
// one was made for it (ownRead). A read that is not due is recorded owed, and once that is written
// the message goes back to the stream where it holds the owed read, and is acknowledged otherwise
// (messageAttempt.answered). Nothing is read where the project has no setting, or a setting whose
// starting point its change queues (a cursor that does not follow it), or where the message is of
// a store the cursor drops (cursor.drops), which calls for no read of its own, unless the cursor
// names it as the owed read's holder: a holder makes the read it holds, adds no reason, and is
// never finished unapplied while it holds the read (judge, applyBatch). Nothing is read either
// where the database refuses for good the read's record as well, the record of the owed read, or
// the project's position itself (a key it does not take): the message is acknowledged all the
// same. Any other failure of the database is tried again in place, as every database step is.
func (in *Intake) readRefused(ctx context.Context, attempt *messageAttempt) (outcome, error) {
	setting, current, found, err := in.position(ctx, attempt.message.Project)
	dropped := current.drops(attempt.message, setting.StateURL)
	if err != nil || !found || !current.follows(setting.StateURL) || (dropped && !current.heldBy(attempt.seq)) {
		if refusedForGood(err) != nil {
			err = nil
		}
		return outcomeDone, err
	}
	attempt.reason = ownRead(ReadReasonRefused, attempt.readMade)
	if dropped {
		attempt.reason = ""
	}
	answer, err := in.answerRead(ctx, in.pool, attempt, current, attempt.reason)
	if err != nil {
		if refusedForGood(err) != nil {
			err = nil
		}
		return outcomeDone, err
	}
	if !answer.read {
		attempt.answered(answer)
		return outcomeDone, nil
	}
	result, err := in.makeRead(ctx, attempt, answer.of(setting))
	if refusedForGood(err) != nil {
		return outcomeDone, nil
	}
	return result, err
}

// refusedForGood is the Postgres error err carries where it is one no retry can write: SQLSTATE
// class 22, a data exception (a value its column's type does not take), or class 23, an
// integrity constraint violation. nil for every other error, a lost connection, a lock timeout
// or a statement a trigger raised among them, which a retry may write.
func refusedForGood(err error) *pgconn.PgError {
	var refusal *pgconn.PgError
	if errors.As(err, &refusal) && (strings.HasPrefix(refusal.Code, "22") || strings.HasPrefix(refusal.Code, "23")) {
		return refusal
	}
	return nil
}

// answerRead runs the read rule (cursor.readFor) on current for reason, the whole read the
// attempt's message calls for, and answers what the rule decides. A read made now for a message of
// another store carries that message's store (readAnswer.store). Where the rule makes no read now,
// answerRead first records what the rule owes (owe) through db: the batch's transaction
// (applyBatch), or the pool (readRefused). The caller sets the message's answer from it
// (messageAttempt.answered) only once that write has committed.
func (in *Intake) answerRead(ctx context.Context, db Execer, attempt *messageAttempt, current cursor, reason string) (readAnswer, error) {
	answer := current.readFor(reason, in.now(), attempt.seq, attempt.readMade, nil)
	if answer.read {
		if reason == ReadReasonStore {
			answer.store = attempt.message.StateID
		}
		return answer, nil
	}
	if err := owe(ctx, db, attempt.message.Project, answer, attempt.seq); err != nil {
		return readAnswer{}, err
	}
	return answer, nil
}

// untilDone runs step until it answers with no error, at once again for outcomeAgain, and after a
// wait for an error: a database step that fails is retried in place, so a runner never moves past
// a message it could not apply. While it waits it tells NATS the message is still in hand
// (InProgress), so the ack wait does not redeliver it. It reports false when ctx, the handling,
// ended first.
func (in *Intake) untilDone(ctx context.Context, msg *natsgo.Msg, project string, step func(context.Context) (outcome, error)) (outcome, bool) {
	wait := applyFirstRetry
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
		wait = min(2*wait, applyLastRetry)
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

// judge is what a message comes to on the position its project's transaction holds: the cursor
// and the parts of a split transaction held for it. It answers the outcome of the message where no
// whole read is made first, outcomeDone for a message whose events above the position are taken,
// and the reason of the whole read the message calls for, "" for none, which applyBatch hands to
// the read rule (answerRead). A message whose read was made (readMade) is judged again on the
// cursor the read left. judge answers every reason but ReadReasonStart through ownRead, so that
// message calls for no read of its own other than a starting point:
//
//   - A message of a store the cursor drops (cursor.drops), the store a write that named another
//     daemon recorded or a store marked as no daemon of the project, is never applied, whatever
//     else holds (outcomeForgotten): R3 applies none of the messages of a daemon the setting named
//     before once the setting names another. Such a message has waited on the project's runner
//     behind any read in flight, as every message does, so it is judged against the cursor that
//     read left. It calls for no read of its own, not even a starting point: the setting's daemon
//     may not have published it, and R3 says a failed starting read is one that no message holds.
//     It holds the owed read only where it took that hold past stateMaxWaiting while a read was in
//     flight (judgeCrowded), and applyBatch then has it make that read or go back as the holder.
//     Otherwise it is finished unapplied: applyBatch queues the read the cursor owes on the
//     project's runner behind the message where that read is due and no message holds it, whatever
//     store the cursor holds (callRead), and the message does not wait for it. The cursor does not
//     drop a boot of such a store while it follows the setting's daemon in a store a read verified:
//     the boot may say that the daemon behind the setting's URL now counts in that store (a
//     redeploy that started it where the verified daemon ran). R3 reads another store so that a
//     daemon whose store was replaced is found again, so that boot is judged as a message of
//     another store below (outcomeOtherStore, ReadReasonStore): it calls for a store read and holds
//     it as any message does. A good read that finds that store takes the mark off, or clears the
//     record of the store the setting named before (cursor.afterRead); a read that finds the cursor's
//     store unchanged leaves it.
//   - A project with no starting point calls for a read whatever else holds (ReadReasonStart). It
//     is due ReadRetryAfter after the project's newest whole read began, as every read is, and
//     where it is not due the message holds it as any owed read (cursor.readFor). Nothing of the
//     message is applied where no read is made first (outcomeNoStore): no read since the write has
//     said which store the daemon the setting names counts in, and the starting read holds the
//     message's events.
//   - A cursor that holds no store, one whose every read since the setting was written failed,
//     takes no message (outcomeNoStore), since no good read has said which store the daemon the
//     setting names counts in. The message may be of the daemon the setting named before, which
//     still publishes under the same project, and R3 applies none of its messages after the
//     setting names another. The read owed since the last one failed holds the message's events.
//   - A message whose stateId is not the cursor's is counted in another store, and nothing of it
//     is applied (outcomeOtherStore). It calls for a read (ReadReasonStore), so a daemon whose
//     store was replaced is found again.
//   - After a good whole read the message called for (readGood), a message whose first sequence
//     is still past the next one is no event of the daemon read: the daemon commits each event
//     before it publishes it, so the read holds every event published before it (outcomeBeyond),
//     whatever reason a message past the cap added while the read was in flight (judgeCrowded).
//     Any other message is taken.
//   - Otherwise a message that does not follow the position (stateMessage.unfollowed: a gap, a
//     boot that does not follow, a part that does not continue the held parts) calls for a read,
//     and where no read is made first it is finished unapplied (outcomeUnfollowed). The read holds
//     its events, and taking it would move the cursor past events never applied: R1 applies no
//     message beyond the starting point's stateSeq and what the stream has delivered since. A boot
//     that follows the position and whose schema version moved (stateMessage.movesSchema) calls
//     for a read (ReadReasonSchema) and is taken, as is every message that follows.
func judge(current cursor, held []part, stateURL string, message stateMessage, readMade, readGood bool) (outcome, string) {
	if current.drops(message, stateURL) {
		return outcomeForgotten, ""
	}
	if !current.follows(stateURL) {
		return outcomeNoStore, ReadReasonStart
	}
	result, reason := outcomeDone, ""
	switch {
	case current.stateID == nil:
		result = outcomeNoStore
	case *current.stateID != message.StateID:
		result, reason = outcomeOtherStore, ReadReasonStore
	case readGood:
		if current.lastSeq != nil && message.FirstSeq > *current.lastSeq+1 {
			result = outcomeBeyond
		}
	default:
		reason = message.unfollowed(current, held)
		switch {
		case reason != "":
			result = outcomeUnfollowed
		case message.movesSchema(current, held):
			reason = ReadReasonSchema
		}
	}
	return result, ownRead(reason, readMade)
}

// ownRead is the rule that a message whose whole read was made calls for no read of its own: it
// answers "" where readMade is set, and reason, the read the message calls for, otherwise. The
// read that was made holds the message's events, since the daemon commits each event before it
// publishes it, and the message is judged again on the cursor that read left. A read the cursor
// still owes, the read that was made having failed, is held and not made again in the same
// delivery (cursor.readFor). judge answers every reason but ReadReasonStart through it, and
// readRefused its ReadReasonRefused.
func ownRead(reason string, readMade bool) string {
	if readMade {
		return ""
	}
	return reason
}

// applyBatch takes the attempt's message in one transaction that holds the project's position
// (lockPosition, and the parts held for it, heldParts). A message whose hold of the owed read a
// write of the setting that named another daemon ended (cursor.releases) is finished unapplied at
// this delivery and holds no read, and its store is marked as no daemon of the project (endHold).
// Any other message is judged (judge), and applyBatch hands the read it calls for to the read rule
// (answerRead). A message of a store the cursor drops (outcomeForgotten) writes nothing and hands
// no read to the rule: it is acknowledged unapplied and holds no read, and it queues the call for
// the owed read that callRead decides, whatever store the cursor holds, on the project's runner
// behind it. Unless the cursor names it as the owed read's holder, which a message past
// stateMaxWaiting of any store can become (judgeCrowded): a held read is made by its holder's
// redelivery, whatever store the holder is of, so such a message goes on to the rule with no
// reason of its own (answerRead), and makes the read where it is due, goes back to the stream as
// the holder where it is not, or is released after stateMaxHolds hand-backs; it is never applied.
// Where the rule makes a read now, the batch writes nothing and answers that read: the caller makes
// it outside the transaction (makeRead) and tries again. Otherwise the transaction records what the
// rule owes (answerRead, owe), and the message comes to judge's outcome: it is acknowledged once
// its batch commits, or goes back to the stream where it holds the owed read, which is set once the
// commit succeeds (messageAttempt.answered). A message judge leaves unapplied writes nothing else
// (outcomeNoStore, outcomeOtherStore, outcomeUnfollowed, outcomeBeyond), and neither does one whose
// project has no setting (outcomeDropped), one at or below the position, or one with more of its
// transaction to come, which is held (holdPart).
//
// The last message of a transaction applies every event of it above the cursor, the held parts'
// and its own, each field at its last value in sequence order, written where it differs from
// legion_current's (recordEvents); one legion_reads row of kind `events` at the message's `at`
// with the bytes of every message of the transaction; the cursor moved to the message's sequence
// and run, and to the schema version of a boot among the events; the parts deleted; and the
// batch's announcement on StateChannel, which Postgres sends at the commit.
func (in *Intake) applyBatch(ctx context.Context, attempt *messageAttempt) (outcome, *owedRead, error) {
	message := attempt.message
	project := message.Project
	tx, err := in.pool.Begin(ctx)
	if err != nil {
		return outcomeDone, nil, fmt.Errorf("open the transaction of the Legion state events of %s: %w", project, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	stateURL, current, found, err := lockPosition(ctx, tx, project)
	if err != nil {
		return outcomeDone, nil, err
	}
	if !found {
		return outcomeDropped, nil, nil
	}
	if current.releases(attempt.seq) {
		attempt.cursor = current
		result, err := endHold(ctx, tx, project, current, attempt.seq, message.StateID)
		return result, nil, err
	}
	held, err := heldParts(ctx, tx, project)
	if err != nil {
		return outcomeDone, nil, err
	}
	result, reason := judge(current, held, stateURL, message, attempt.readMade, attempt.readGood)
	attempt.cursor, attempt.reason = current, reason
	if result == outcomeForgotten && !current.heldBy(attempt.seq) {
		in.callRead(project, current)
		return result, nil, nil
	}
	answer, err := in.answerRead(ctx, tx, attempt, current, reason)
	if err != nil {
		return outcomeDone, nil, err
	}
	if answer.read {
		read := answer.of(Settings{Project: project, StateURL: stateURL})
		return outcomeDone, &read, nil
	}
	// Every return from here commits: a read this batch recorded owed is part of it, and so is the
	// holder it recorded or released, so the message's answer is set once the commit succeeds.
	finish := func(result outcome) (outcome, *owedRead, error) {
		if err := tx.Commit(ctx); err != nil {
			return outcomeDone, nil, fmt.Errorf("commit the Legion state events of %s: %w", project, err)
		}
		attempt.answered(answer)
		return result, nil, nil
	}
	if result != outcomeDone {
		return finish(result)
	}
	// A message judge takes that has parts held before it continues them (unfollowed), or is one
	// of them delivered again.
	if n := len(held); n > 0 && message.Seq <= held[n-1].lastSeq {
		return finish(outcomeDone)
	}
	// The transaction's events in sequence order: the held parts', then the message's own.
	var transaction []stateEvent
	bytes := attempt.bytes
	for _, one := range held {
		transaction = append(transaction, one.events...)
		bytes += one.bytes
	}
	transaction = append(transaction, message.Events...)
	events := eventsAfter(transaction, current.lastSeq)
	if len(events) == 0 {
		return finish(outcomeDone)
	}
	if message.More {
		if err := holdPart(ctx, tx, project, message, attempt.bytes); err != nil {
			return outcomeDone, nil, err
		}
		return finish(outcomeDone)
	}
	if _, err := recordEvents(ctx, tx, project, message.At, lastValues(events)); err != nil {
		return outcomeDone, nil, err
	}
	if _, err := tx.Exec(ctx, insertRead, project, message.At, true, nil, bytes, readKindEvents, nil); err != nil {
		return outcomeDone, nil, fmt.Errorf("store the Legion state events of %s: %w", project, err)
	}
	var run *string
	if message.Run != "" {
		run = &message.Run
	}
	var schemaVersion *int64
	for _, event := range events {
		if event.Kind == eventBoot {
			schemaVersion = &event.SchemaVersion
		}
	}
	// The cursor keeps its store: judge takes only a message of the cursor's own store, and no
	// message at all while the cursor holds none. A message cannot give the cursor a store, since
	// it cannot show it is of the daemon the setting names: after a setting names another daemon,
	// the one it named before may still publish under the same project, and its stateId would then
	// pass for the named daemon's store. Only a good whole read of the daemon the setting names
	// sets the store (cursor.afterRead).
	if _, err := tx.Exec(ctx, `
		update legion_event_cursors
		set last_seq = $2, run = $3, schema_version = coalesce($4, schema_version)
		where project = $1
	`, project, message.Seq, run, schemaVersion); err != nil {
		return outcomeDone, nil, fmt.Errorf("move where the Legion events of %s stand: %w", project, err)
	}
	if len(held) > 0 {
		if _, err := tx.Exec(ctx, `delete from legion_event_parts where project = $1`, project); err != nil {
			return outcomeDone, nil, fmt.Errorf("delete the held state events of %s: %w", project, err)
		}
	}
	if err := NotifyRead(ctx, tx, project); err != nil {
		return outcomeDone, nil, err
	}
	return finish(outcomeDone)
}
