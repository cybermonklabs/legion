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
// message's (judgeCrowded, recordWhole). A message that holds the owed read, whatever store it is
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
	"slices"
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

// ReadRetryAfter is how long a project's whole read waits after the one before it began, for
// every reason, the starting read included (read-rule-cases v11). The cursor keeps the start of the
// project's newest whole read (last_read_started_at), and a write of the setting does not reset
// it. A read called for inside the minute is owed rather than made (cursor.readFor). The first
// message that owes it goes back to the stream unacknowledged for the time left until it is due,
// and its redelivery makes the read; every later message is acknowledged once the cursor records
// its reason, and a settings change or a take-over makes no read of its own while that message
// holds the read. So where a message holds the owed read, a project that goes quiet is
// ReadRetryAfter behind at the most. A read that fails sends that message back until
// ReadRetryAfter after the read began, holdAtLeast at the least, so a read the daemon is slow to
// answer, or never answers within RequestTimeout, adds no time to the minute. It does so at most
// stateMaxHolds times in all, and then lets the message go. A failed read that no message
// holds - a starting read's that no message made, or one whose message has gone back
// stateMaxHolds times - waits for the project's next message, boot, settings change or
// take-over, which makes it once it is due. A message of a store the cursor drops (cursor.drops)
// calls for no read of its own, and holds one only where, past stateMaxWaiting while a read of
// the project was in flight, it took the hold of the gap read after it (judgeCrowded): its
// redelivery then makes that read, as every holder's does. It waits on the project's runner behind
// a read in flight, as every message does, and is judged against the cursor that read leaves: it
// queues a call for a read that is due and that no message holds behind itself, whatever store the
// cursor holds, which the runner makes after the message is acknowledged (callRead). No recurring
// timer makes a read. A starting read that a write's wake or a take-over finds not due is made when
// the minute ends, by one deferred call of that wake or take-over (startingPoint). A project with
// no read yet has no read to wait after.
const ReadRetryAfter = time.Minute

// The reasons a whole read is made, which legion_reads.reason records and the whole read's log
// line names. There are no others: no recurring timer reads a daemon. A reason that calls for a
// read while one is owed joins the owed read (cursor.readReasons), and the read is recorded under
// the first reason it was owed for and logged with every one it joined (owedRead).
const (
	// ReadReasonStart is a project with a setting and no starting point: no cursor, or one that
	// follows another daemon than the setting names, or none at all, as a write of the setting
	// leaves it (settingWritten). It is read at the intake's take-over (which covers the durable's
	// first creation), on a settings change, and at the project's next message if neither has read
	// it yet, and at none of them while a Dispatch process of an earlier build is in the fleet
	// (processes.EarlierLegionIntake). It is due ReadRetryAfter after the project's newest whole
	// read began, as every read is (cursor.readFor).
	ReadReasonStart = "start"
	// ReadReasonBoot is a boot event that does not follow the cursor: its sequence is not the next
	// one, or it is at or below the cursor under another run (a daemon whose counter went back).
	ReadReasonBoot = "boot"
	// ReadReasonSchema is a boot event above the cursor whose schemaVersion is not the one the
	// cursor holds: the boot moved the store's schema, and a migration may have rewritten what
	// Dispatch observes outside the writes that publish events.
	ReadReasonSchema = "schema"
	// ReadReasonGap is a message whose first sequence is past the next one: events the stream no
	// longer holds. A message that does not continue the parts of a split transaction the intake
	// holds (part) is one too, since the transaction cannot be completed.
	ReadReasonGap = "gap"
	// ReadReasonRetry is a read owed since the last one failed.
	ReadReasonRetry = "retry"
	// ReadReasonStore is a message whose stateId is not the cursor's: it is counted in another
	// store than the one the starting point read, so its sequences say nothing of the cursor's.
	// The read finds the store the daemon the setting names counts in now.
	ReadReasonStore = "store"
	// ReadReasonRefused is a message the database refused for good (refusedForGood): it is not
	// applied, and the read puts the project's observations at the daemon's state. The message is
	// acknowledged, or goes back to the stream where it holds the owed read (cursor.readFor).
	ReadReasonRefused = "refused"
)

// Why a whole read the daemon answered is a failed read all the same. Each leaves the read owed;
// all but the first record the read's observations, since the state is the project's own.
var (
	// ErrStateOfAnotherProject is a state whose daemon.project is not the setting's project.
	ErrStateOfAnotherProject = errors.New("the daemon serves another project's state")
	// ErrNoStateSeq is a state with no daemon.stateSeq: a daemon without state events.
	ErrNoStateSeq = errors.New("the daemon serves no daemon.stateSeq, so it publishes no state events")
	// ErrStateSeqZero is a state whose daemon.stateSeq is 0: a daemon that has committed no state
	// event, which a daemon with a workflow never serves, since its boot event commits first.
	ErrStateSeqZero = errors.New("the daemon serves daemon.stateSeq 0: it has published no state event")
	// ErrNoDispatchURL is a state whose settings carry no dispatch_url: a daemon that runs no
	// workflow now, so it publishes no state events, whatever it published before. A daemon that
	// never ran one serves daemon.stateSeq 0 too, and this is the reason its read names.
	ErrNoDispatchURL = errors.New("the daemon's settings carry no dispatch_url: it runs no workflow, so it publishes no state events")
	// ErrNoStateID is a state with a daemon.stateSeq and no daemon.stateId: no message can be
	// matched to the store that sequence is counted in.
	ErrNoStateID = errors.New("the daemon serves no daemon.stateId, so its state messages cannot be matched to its store")
)

// wholeReadLine is the log line of every whole read, at INFO.
const wholeReadLine = "legion intake: whole read"

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
// write of the project's Legion setting ended (cursor.releasedHolder), at INFO, on the one delivery
// after the write that acknowledges it unapplied, naming its stream sequence and its store.
const writeReleasedLine = "legion intake: a write of the project's Legion setting ended a state message's hold of the owed whole read, so it is acknowledged unapplied"

// releasedLine is the log line of a message that went back to the stream stateMaxHolds times
// holding its project's owed whole read, at WARN, on the delivery that acknowledges it, naming
// the hand-backs the cursor counted (handBacks) and how many times this durable has delivered the
// message (delivery). The cursor names no holder from then on, and the read stays owed for the
// project's next message, boot, settings change or take-over.
const releasedLine = "legion intake: a state message went back to the stream the most times allowed for its project's owed whole read, so it is acknowledged and the read stays owed"

// holdAtLeast is the shortest time a message that holds its project's owed read goes back to the
// stream for (cursor.readFor), so a message that comes back a moment before the read is due on
// Dispatch's clock does not come straight back again.
const holdAtLeast = time.Second

// stateMaxHolds is how many times one message goes back to the stream holding its project's owed
// whole read (cursor.readFor): the hand-back that waits for the read to be due, where the message
// arrived inside ReadRetryAfter of the start of the last read, and one for each read its
// deliveries made that failed, each until ReadRetryAfter after that read began. The cursor counts
// the hand-backs beside the holder (cursor.readHolds): 1 when it records the message as the
// holder, one more at each hand-back after it (owe), and 0 once it names no holder. A take-over
// creates the durable again, which starts the message's own NumDelivered again at 1, and keeps
// the cursor's count, so the bound holds for the message across every take-over. Neither a
// take-over's call for the read nor a settings change's makes a read while the message holds it
// (cursor.readFor), so neither adds a read to the hold. A write of the setting that names the same
// daemon leaves the holder and its count as they are, and one that names another daemon ends the
// hold: the message is acknowledged unapplied at its next delivery (settingWritten,
// cursor.releasedHolder, outcomeReleased), and a message of the newly named daemon may hold the
// read from a count of 1. A holder that has gone back stateMaxHolds times holds the read no more:
// that delivery is acknowledged, the cursor names no holder from then on (readAnswer.release), and
// the read stays owed (ReadReasonRetry) for the project's next message, boot, settings change or
// take-over, which makes it once it is due, as R3 makes every failed read. So each message holds
// the durable's ack floor below it for about 11 minutes at the most, however many take-overs and
// writes of the setting there are, and a take-over replays at most that long of messages
// (consumerConfig).
const stateMaxHolds = 10

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

// stateMaxWaiting is how many messages one project keeps waiting on its runner, the one running
// included (Intake.waiting). A message that arrives while that many wait does not wait: it runs on
// its project's crowded key (crowdedKey) and owes a read (judgeCrowded). A read the daemon does not
// answer holds the runner for RequestTimeout, and every message that waits holds one message of the
// durable's window (stateMaxAckPending) until it is acknowledged, a message handed back with a
// delay included. So a project in such a read holds stateMaxWaiting messages that wait and one that
// holds the owed read, 17 of the 256, and 15 projects can be in such a read at once before the
// window is full.
const stateMaxWaiting = 16

// crowdedKeyPrefix begins each project's crowded key (crowdedKey). Its first byte is not UTF-8, so
// no project and no subject is a key that begins with it: messageKey decodes a project from JSON,
// which gives UTF-8 alone, and every subject of the durable begins with "notifications.".
const crowdedKeyPrefix = "\xffcrowded:"

// crowdedKey is the runner key of each message of project that arrives while stateMaxWaiting of its
// messages wait on its runner: a key of its own beside the runner, so the message waits for no read
// of the project. Its handler makes no read (judgeCrowded).
func crowdedKey(project string) string {
	return crowdedKeyPrefix + project
}

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

// readInFlight is the whole read in flight on one project's runner (Intake.reading). started is
// when it began, on the intake's clock, so a message past stateMaxWaiting that holds the read after
// it goes back to the stream until ReadRetryAfter after that start. pastCap is the stream sequence
// of the first message past stateMaxWaiting the intake judged since it began, 0 for none: that
// message takes the hold of the read after it (judgeCrowded, Intake.inFlight, cursor.readFor).
type readInFlight struct {
	started time.Time
	pastCap int64
}

// reportKey is one line of reportEvery for one project.
type reportKey struct {
	line, project string
}

// waitingCounts is, for one subscription of the durable, how many messages of each project are
// keyed to the project's runner and not yet finished (projects), and the project of each such
// message (keyed). messageKey counts a message up as it keys it to its project's runner, and
// deliver counts it down as it returns for it (finished). A message the stop of a subscription
// hands back never reaches deliver, so a take-over starts new counts at 0 (takenOver), and a
// message keyed under the counts before is counted down in none.
type waitingCounts struct {
	projects map[string]int
	keyed    map[*natsgo.Msg]string
}

// newWaitingCounts is a subscription's counts before any message is keyed.
func newWaitingCounts() waitingCounts {
	return waitingCounts{projects: map[string]int{}, keyed: map[*natsgo.Msg]string{}}
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
// read at all. Where a message holds the owed read, the call joins that read, which the message's
// redelivery makes; a write of the same URL leaves such a cursor as it is (settingWritten), so its
// wake joins the read too. A settings read that fails queues nothing: a project with no starting
// point is read at its next message all the same, and the listener's wake fires again once it
// listens again. A process that does not hold the durable queues nothing: the process that holds
// it heard the same notification.
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
// message holds the owed read, the message's redelivery makes it. A settings change and a
// take-over queue it (SettingsChanged), and so does a message of a store the project's cursor drops
// (callRead). It takes no starting point while a Dispatch process of an earlier build is in
// the fleet (startBlocked). It runs on the project's runner; a database step that fails is tried
// again in place until it succeeds or the handling ends.
//
// wake is whether a write's wake or a take-over calls it (SettingsChanged). Such a call that finds
// the starting read not due, since the project's newest whole read began less than ReadRetryAfter
// ago, queues the starting point once more for when the minute ends (startLater), so the read is
// made then and not at the project's next message alone. The deferred call and a message's call
// (callRead) pass false: each wake and each take-over defers one call at the most, and no timer
// recurs.
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
			if wake && reason == ReadReasonStart {
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
// due (startingPoint). The deferred call defers no further call. A process that no longer delivers
// from the durable when d ends queues nothing (delivery.HeldDurable.Queue), and the process that
// took the durable over has queued the starting point of its own take-over.
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

// finished counts msg down among the messages that wait on its project's runner, where messageKey
// counted it under the counts of the subscription the durable delivers on now (waiting). deliver
// calls it as it returns for msg.
func (in *Intake) finished(msg *natsgo.Msg) {
	in.mu.Lock()
	defer in.mu.Unlock()
	project, keyed := in.waiting.keyed[msg]
	if !keyed {
		return
	}
	delete(in.waiting.keyed, msg)
	if in.waiting.projects[project]--; in.waiting.projects[project] <= 0 {
		delete(in.waiting.projects, project)
	}
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

// judgeCrowded is one attempt at a message on its project's crowded key (crowdedKey), which runs
// beside the project's runner while stateMaxWaiting of the project's messages wait there, behind a
// read in flight for example. It makes no read and waits for none. In one transaction that holds
// the project's position (lockCursor), it answers outcomeDropped for a project with no setting,
// finishes a message whose hold a write of the setting ended (cursor.releases, endHold), and
// answers outcomeForgotten, writing nothing, for a message of a store the cursor drops
// (cursor.drops) while no whole read of the project is in flight (readsNow), unless the cursor
// names that message as the owed read's holder: a holder is never finished unapplied while it
// holds the read. A read in flight can find that store behind the setting's URL (the same store
// behind a new URL, a marked store a redeploy started there), and the cursor it leaves then
// applies that store's later events, so a message of it past the cap is judged as any other: its
// events may be past that read's snapshot. A boot of such a store, while the cursor follows the
// setting's URL in a store a read verified, is no message the cursor drops (read-rule-cases row
// 3.5): it is judged as any other message of another store, read in flight or not. Every other
// message owes a read: ReadReasonStore for a message of another store than the one a read verified
// behind the setting's URL, as judge answers it; otherwise the one its position calls for against
// the cursor as it stands (stateMessage.unfollowed: a boot that does not follow calls for
// ReadReasonBoot), and ReadReasonGap where it calls for none, since its events may be past the
// snapshot of the read in flight. The read rule decides the rest as for a delivery that has made
// its read already (cursor.readFor), so it never makes the read now. The first such message since
// the read in flight began takes the hold of the read after it (cursor.readFor, read-rule-cases row
// 8.1): it goes back to the stream until ReadRetryAfter after the start of the read in flight, and
// its redelivery, keyed to the runner once fewer messages wait there, makes the read on the runner.
// Where no read is in flight, a message holds the read where no message holds it, and goes back
// until ReadRetryAfter after the last read. Where another message holds it, this message joins it
// and is acknowledged unapplied (outcomeCrowded), and the cursor keeps its sequence as the highest
// it skipped in its store (cursor.skips), so the read in flight, which clears the reasons it was
// made for, leaves the gap read owed where that sequence is past its snapshot, and the store read
// owed where it is of another store that the cursor the read leaves does not drop (recordWhole). A
// project with no cursor yet, whose starting read is queued or in flight, is given one that follows
// no daemon (lockCursor), so the read it owes is recorded.
func (in *Intake) judgeCrowded(ctx context.Context, attempt *messageAttempt) (outcome, error) {
	message := attempt.message
	project := message.Project
	tx, err := in.pool.Begin(ctx)
	if err != nil {
		return outcomeDone, fmt.Errorf("open the transaction of the Legion state events of %s: %w", project, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	stateURL, current, found, err := lockCursor(ctx, tx, project)
	attempt.cursor = current
	switch {
	case err != nil:
		return outcomeDone, err
	case !found:
		return outcomeDropped, nil
	case current.releases(attempt.seq):
		return endHold(ctx, tx, project, current, message.StateID)
	case current.drops(message, stateURL) && !in.readsNow(project) && !current.heldBy(attempt.seq):
		return outcomeForgotten, nil
	}
	reason := ReadReasonGap
	switch unfollowed := message.unfollowed(current, nil); {
	case current.follows(stateURL) && current.stateID != nil && !current.holdsStore(message.StateID) &&
		!current.drops(message, stateURL):
		reason = ReadReasonStore
	case unfollowed != "":
		reason = unfollowed
	}
	attempt.reason = reason
	// made: a message on the crowded key never makes a read, as a delivery that made one already.
	flight := in.inFlight(project, attempt.seq)
	answer := current.readFor(reason, in.now(), attempt.seq, true, flight)
	if err := owe(ctx, tx, project, answer, attempt.seq); err != nil {
		return outcomeDone, err
	}
	if answer.back == 0 {
		stateIDs, seqs := current.skips(message.StateID, message.Seq)
		if _, err := tx.Exec(ctx, `
			update legion_event_cursors set skipped_state_ids = $2, skipped_seqs = $3 where project = $1
		`, project, stateIDs, seqs); err != nil {
			return outcomeDone, fmt.Errorf("record the state message of %s skipped past the cap: %w", project, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return outcomeDone, fmt.Errorf("commit the Legion state events of %s: %w", project, err)
	}
	attempt.answered(answer)
	return outcomeCrowded, nil
}

// readsNow reports whether a whole read of project is in flight on its runner (reads).
func (in *Intake) readsNow(project string) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	_, reading := in.reading[project]
	return reading
}

// inFlight is the whole read in flight on project's runner as judgeCrowded judges the message at
// stream sequence seq beside it, and nil where no whole read of project is in flight (reading).
// Where the read has no first message past stateMaxWaiting yet (readInFlight.pastCap), inFlight
// records seq as that message. The read rule decides the rest from what inFlight answers
// (cursor.readFor): the read after the read in flight waits ReadRetryAfter after that read's
// start, and the read's first message past the cap takes the hold of the read after it. Every
// later message past the cap joins that hold, so the project holds one message of the durable's
// window beside those that wait on its runner (R3-h).
func (in *Intake) inFlight(project string, seq int64) *readInFlight {
	in.mu.Lock()
	defer in.mu.Unlock()
	flight, reading := in.reading[project]
	if !reading {
		return nil
	}
	if flight.pastCap == 0 {
		flight.pastCap = seq
		in.reading[project] = flight
	}
	return &flight
}

// endHold finishes, inside tx, the message whose hold of project's owed read a write of the
// setting ended (cursor.releases): it clears released_holder, marks stateID, the message's store,
// as no daemon of the project (foreign_state_ids, cursor.marking) unless it is the store the write
// recorded or the cursor's own store, and commits. The message is acknowledged unapplied and holds
// no read (outcomeReleased).
func endHold(ctx context.Context, tx pgx.Tx, project string, current cursor, stateID string) (outcome, error) {
	if _, err := tx.Exec(ctx, `
		update legion_event_cursors set released_holder = null, foreign_state_ids = $2 where project = $1
	`, project, current.marking(stateID)); err != nil {
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

// makeRead makes read, the whole read the attempt's message calls for, outside any transaction
// (readWhole), and answers outcomeAgain, so the attempt is made again on the cursor the read left.
// It sets readMade once readWhole has recorded the read, after which the message calls for no read
// of its own (ownRead), and readGood where the read recorded was a good one. applyMessage and
// readRefused call it.
func (in *Intake) makeRead(ctx context.Context, attempt *messageAttempt, read owedRead) (outcome, error) {
	// A read takes up to RequestTimeout: the message is still in hand.
	inProgress(attempt.msg)
	recorded, err := in.readWhole(ctx, read, &attempt.pending)
	if err != nil {
		return outcomeDone, err
	}
	if recorded != nil {
		attempt.readMade, attempt.readGood = true, recorded.failure == nil
	}
	return outcomeAgain, nil
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

// owedRead is a whole read to make: the setting whose daemon it reads, the reason its legion_reads
// row records, and every reason it is made for in the order they called for it, reason first
// (joined), which its log line names (finishWhole). clears is the reasons the cursor owed when the
// read was decided, which its record clears (cursor.afterRead). A reason that a message adds while
// the read is in flight (judgeCrowded) stays owed after a failed read. A good read clears it too,
// but for two cases:
//   - The cursor's skipped record holds a sequence of the read's store above the read's stateSeq
//     (cursor.skippedAbove): the reason stays owed, and gap is owed with it.
//   - That record holds another store, which the cursor the read leaves does not drop
//     (cursor.skippedElsewhere): the reason stays owed, and store is owed with it.
//
// store is the store of the message that makes the read where that message is of another store
// than the cursor's (ReadReasonStore), and "" for every other read, a read that no message makes
// among them: a good read that finds the cursor's store unchanged marks it as no daemon of the
// project (cursor.afterRead, foreign_state_ids).
type owedRead struct {
	setting Settings
	reason  string
	joined  []string
	clears  []string
	store   string
}

// readOf is the whole read of setting's daemon made for reason alone, decided while the cursor owed
// no read: its record clears no reason.
func readOf(setting Settings, reason string) owedRead {
	return owedRead{setting: setting, reason: reason, joined: []string{reason}}
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

// owe records on project's cursor, through db (a batch's transaction, or the pool), what answer
// owes (cursor.readFor): its reason after the reasons the read is owed for already; the message at
// stream sequence seq as the owed read's holder where answer records it so (readAnswer.hold); the
// hand-backs of the holder counted to answer's holds where answer sends the message back; and no
// holder and no hand-back at all where answer releases the message that held the read. It writes
// nothing where answer does none of these.
func owe(ctx context.Context, db Execer, project string, answer readAnswer, seq int64) error {
	if answer.owe == "" && answer.back == 0 && !answer.release {
		return nil
	}
	reasons := []string{}
	if answer.owe != "" {
		reasons = append(reasons, answer.owe)
	}
	var holder *int64
	if answer.hold {
		holder = &seq
	}
	var holds *int
	if answer.back > 0 {
		holds = &answer.holds
	}
	if _, err := db.Exec(ctx, `
		update legion_event_cursors
		set read_reasons = read_reasons || $2::text[],
			read_holder = case when $4::boolean then null else coalesce($3, read_holder) end,
			read_holds = case when $4::boolean then 0 else coalesce($5, read_holds) end
		where project = $1
	`, project, reasons, holder, answer.release, holds); err != nil {
		return fmt.Errorf("record the whole read %s owes: %w", project, err)
	}
	return nil
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

// cursor is one project's legion_event_cursors row: how far its observations have followed its
// daemon (migration 20261118212431). exists is false for a project with no row.
type cursor struct {
	exists   bool
	stateURL string
	run      *string
	lastSeq  *int64
	// readReasons is every reason a whole read is owed for, in the order they called for it (owe),
	// and empty while none is owed (readOwed). A read's record clears the reasons owed when the read
	// was decided (afterRead). After a failed read it holds retry and each reason a message added
	// while the read was in flight. A good read clears those reasons too, but for two cases. Where
	// the skipped record holds a sequence of the read's store above its stateSeq (skippedAbove), they
	// stay, and gap with them. Where that record holds another store that the cursor the read leaves
	// does not drop (skippedElsewhere), they stay, and store with them. lastReadStartedAt is when the
	// project's newest whole read began, whatever its reason and whatever it came to: the next read
	// waits ReadRetryAfter after it (readDueIn). Every whole read's record writes it, and a write of
	// the setting does not reset it, so it holds across a write's reset of the cursor. It is nil
	// only for a project with no read recorded since its cursor was created.
	// readHolder is the stream sequence of the message that holds the owed read, which goes back to
	// the stream until the read is due, and again after each read it makes that fails,
	// stateMaxHolds times at the most (readFor), and nil where no message holds it. readHolds is
	// how many times that message has gone back holding the read, which neither a take-over nor a
	// write of the setting that names the same daemon resets, and 0 where no message holds it.
	readReasons       []string
	lastReadStartedAt *time.Time
	readHolder        *int64
	readHolds         int
	// skippedStateIDs and skippedSeqs are, pair by pair, a store and the highest sequence of a
	// message of that store that judgeCrowded acknowledged unapplied past stateMaxWaiting (skips),
	// the store skipped last at the end, maxSkippedStores at the most. Every whole read empties
	// both as it begins, since it holds every event published before it (readWhole), so what they
	// hold as it lands was skipped while it was in flight. A good read owes the gap read where they
	// hold a sequence of its own store above its stateSeq (skippedAbove), and the store read where
	// they hold another store that the cursor it leaves does not drop (skippedElsewhere); otherwise
	// it leaves no reason owed for them. Either way it empties both, and a failed read keeps them
	// (afterRead). Every write that resets the cursor empties them (settingWritten).
	skippedStateIDs []string
	skippedSeqs     []int64
	// stateID is the store the cursor's sequence is counted in: nil until a good read sets it, and
	// again once a write of the setting clears it (settingWritten). schemaVersion is the store's
	// schema version as the cursor last saw it, nil for none. previousStateID is the store of the
	// daemon the setting named before a write that named another daemon (settingWritten), nil for
	// none. A write back to that daemon's URL clears it, and so does a good read that gives the
	// cursor that store (afterRead). previousStateURL is the URL the cursor followed when the write
	// recorded previousStateID, and goes with it: that URL is the one a write back names.
	// foreignStateIDs is each store that is no daemon of the project, the newest last,
	// maxForeignStores at the most: the store of a message whose good store read found the cursor's
	// store unchanged (afterRead), and the store of a message whose hold a write of the setting
	// ended (endHold). Every write that resets the cursor empties it (settingWritten), and a good
	// read whose store is in it takes that store out. The intake finishes each message of a store
	// in either unapplied while the cursor holds no store or another store (forgets).
	stateID          *string
	schemaVersion    *int64
	previousStateID  *string
	previousStateURL *string
	foreignStateIDs  []string
	// releasedHolder is the stream sequence of the message that held the owed read when a write of
	// the setting reset a cursor that followed another daemon (settingWritten), nil for none: that
	// message is finished unapplied at its next delivery, which clears it (releases, endHold).
	releasedHolder *int64
}

// maxForeignStores is how many stores a cursor marks as no daemon of its project
// (foreignStateIDs). Past it the oldest mark goes, and that store's next message makes one store
// read again, at most once a ReadRetryAfter.
const maxForeignStores = 8

// maxSkippedStores is how many stores a cursor keeps the highest sequence skipped past
// stateMaxWaiting of (skippedStateIDs). During a whole read that may verify a store the cursor does
// not hold (read-rule-cases rows 4.4, 5.2 and 5.6), messages of the cursor's store, of the store a
// write recorded and of the stores marked as no daemon of the project can all arrive past the cap,
// so the cursor keeps several, bounded as the marks are (maxForeignStores). Past it the store
// skipped longest ago goes: a read that then finds that store owes no gap read for it, and that
// store's next message calls for one.
const maxSkippedStores = maxForeignStores

// readOwed reports whether the cursor owes a whole read: whether it names a reason for one, which
// is what the column read_owed, generated from read_reasons, says too.
func (c cursor) readOwed() bool {
	return len(c.readReasons) > 0
}

// readDueIn is how long after now a whole read of the project may start, whatever its reason, a
// starting read's included: until ReadRetryAfter after the project's newest whole read began
// (lastReadStartedAt). It is 0 where that moment has passed, or where the project has no read to
// wait after.
func (c cursor) readDueIn(now time.Time) time.Duration {
	if c.lastReadStartedAt == nil {
		return 0
	}
	return dueAfter(*c.lastReadStartedAt, now)
}

// dueAfter is how long after now a whole read is due where the project's newest whole read began
// at started: ReadRetryAfter after started, and 0 once that moment has passed.
func dueAfter(started, now time.Time) time.Duration {
	return max(started.Add(ReadRetryAfter).Sub(now), 0)
}

// readAnswer is what the read rule answers a call for a whole read (cursor.readFor).
type readAnswer struct {
	// read is a whole read made now, for reason, the one its legion_reads row records, joined,
	// every reason it is made for in the order they called for it, reason first, and clears, the
	// reasons the cursor owes now, which the read's record clears (owedRead.clears). store is the
	// store of the message that makes the read, where it is of another store (answerRead).
	read   bool
	reason string
	joined []string
	clears []string
	store  string
	// owe is the reason the call adds to the owed read, and "" where it adds none: no read is owed,
	// the read is made now, or the cursor owes it for that reason already.
	owe string
	// back is how long the message that called goes back to the stream for, unacknowledged, where it
	// holds the owed read: the time left until the read is due, holdAtLeast at the least and
	// ReadRetryAfter at the most. It is 0 where the message is acknowledged. hold is whether the
	// cursor records the message as the read's holder, which it does where it names none yet, and
	// where the message takes the hold over from the holder of a read in flight (readFor, row
	// 8.1). release is whether the cursor stops naming the message as the holder, which it does
	// once the message has gone back stateMaxHolds times. holds is the hand-backs the cursor counts
	// once back is recorded, which this one makes, and the hand-backs it counted where release is
	// set.
	back    time.Duration
	hold    bool
	release bool
	holds   int
}

// of is the read answer makes now, of setting's daemon.
func (a readAnswer) of(setting Settings) owedRead {
	return owedRead{setting: setting, reason: a.reason, joined: a.joined, clears: a.clears, store: a.store}
}

// readFor is the one rule of every whole read, which each call for one decides by: a message's
// (applyBatch, readRefused, judgeCrowded) and a starting point's (startingPoint). reason is the
// read the call needs, "" for none (a call that only makes a read owed already), now is the
// intake's clock, seq is the stream sequence of the message that calls, 0 for a call no message
// makes, and made whether the delivery of that message has made a whole read already
// (messageAttempt.readMade). flight is the whole read in flight on the project's runner where a
// message is judged beside the runner while that read is in flight (judgeCrowded,
// Intake.inFlight), and nil for every other call. A message of a store the cursor drops
// (outcomeForgotten) calls it as a call no message makes, to decide whether to queue the project's
// starting point, which calls it again on the project's runner (callRead): so such a message never
// makes a read for itself or owes one. Where the cursor names it as the holder, which it can be
// after it owed a gap read past stateMaxWaiting while a read was in flight, it calls it with no
// reason as the holder, so it makes the held read once that read is due and goes back to the
// stream until then (applyBatch). A message on its project's crowded key calls it as a delivery
// that made its read already, so it never makes the read (judgeCrowded).
//
// A read is due ReadRetryAfter after the project's newest whole read began (readDueIn). Where
// flight names a read in flight, that read is the newest: the read after it is due ReadRetryAfter
// after the start of the read in flight.
//
//   - ReadReasonStart is made now and alone where it is due and the delivery has made no read
//     yet: each reason its cursor owes is for a daemon the setting no longer names, or for events
//     the read's snapshot holds, if it has a cursor at all. A call no message makes makes it too
//     where the cursor names a holder, as a holder's redelivery would. Where it is not due, the
//     project's newest whole read, of any kind, began less than ReadRetryAfter ago
//     (read-rule-cases v11, row 3.2): the starting read is owed and held as every read below.
//   - A call with no reason, where no read is owed, makes no read and owes none.
//   - A call no message makes, where the cursor names a holder, makes no read, due or not: it
//     joins the holder's read, which the holder's redelivery makes, the cursor counting its
//     hand-backs on from readHolds. So neither a take-over nor a settings change (startingPoint)
//     adds a read to the hold, and the hold's bound (stateMaxHolds) stands however many of them
//     come.
//   - A read that is due is made now, for every reason the cursor owes it for, in the order they
//     called for it, then for reason where it is not among them. It is recorded under the first.
//     Its record clears the reasons the cursor owes now (readAnswer.clears). A reason that a
//     message adds while the read is in flight stays owed where the read fails. A good read clears
//     it too, but for the two cases of owedRead.clears, which owe gap or store with it
//     (cursor.afterRead). A delivery makes one read at the most: where it made one already, the
//     read is held as one that is not due. The minute counts from the read's start, so a read that
//     the daemon answers only after the minute, or never answers within RequestTimeout, is due
//     again as soon as it fails, and its message goes back to the stream rather than reading at
//     once.
//   - Otherwise the read is owed, and reason joins it once: where it is not among the reasons the
//     cursor owes it for. The message that holds the owed read goes back to the stream for the
//     time left until the read is due, holdAtLeast at the least, so that its redelivery makes the
//     read. Every message that holds no read is acknowledged.
//   - A read in flight changes the holder (read-rule-cases row 8.1). The first message past
//     stateMaxWaiting since that read began (flight.pastCap) holds the read after it, from a count
//     of 1, whatever message the cursor names as the holder, unless the cursor names that message
//     already. The holder the cursor names holds the read in flight, which clears the reasons it
//     was made for. The read after it is for the events past its snapshot, and only the messages
//     past the cap since it began can carry them.
//   - In every other case, the message that holds the owed read is the message the cursor names as
//     the holder, or the message that calls where the cursor names none. The cursor counts each
//     hand-back (readHolds), and a holder that has gone back stateMaxHolds times holds the read no
//     more: the cursor names no holder from then on (release).
//
// So each project that owes a read holds one message of the durable's window. Where a message
// holds the read, a project that goes quiet is ReadRetryAfter behind at the most, and a read that
// fails sends the message back until ReadRetryAfter after the read began, stateMaxHolds times at
// the most in all, across take-overs too, which make no read of their own meanwhile. A failed
// read that no message holds, a starting point's or one whose message went back stateMaxHolds
// times, is made at the project's next message, boot, settings change or take-over, once it is
// due: the next message to call holds it. A starting read that a write's wake or a take-over finds
// not due is made when it is due by one deferred call (startingPoint). A message of a store the
// cursor drops neither makes nor holds that read unless the cursor names it as the holder:
// whatever store the cursor holds, it queues a call for it on the project's runner, which makes it
// as a call no message makes (callRead).
func (c cursor) readFor(reason string, now time.Time, seq int64, made bool, flight *readInFlight) readAnswer {
	dueIn := c.readDueIn(now)
	if flight != nil {
		dueIn = dueAfter(flight.started, now)
	}
	switch {
	case reason == ReadReasonStart && dueIn == 0 && !made:
		return readAnswer{read: true, reason: ReadReasonStart, joined: []string{ReadReasonStart}, clears: c.readReasons}
	case reason == "" && !c.readOwed():
		return readAnswer{}
	case dueIn == 0 && !made && (seq != 0 || c.readHolder == nil):
		// A call no message makes joins the read a holder holds rather than making it. A call
		// with no reason reaches here only where a read is owed, so the read has a reason to be
		// recorded under: the first it was owed for, or the call's own.
		answer := readAnswer{read: true, reason: reason, joined: []string{reason}, clears: c.readReasons}
		if c.readOwed() {
			answer.reason, answer.joined = c.readReasons[0], c.readReasons
			if reason != "" && !slices.Contains(c.readReasons, reason) {
				answer.joined = append(slices.Clone(c.readReasons), reason)
			}
		}
		return answer
	}
	var answer readAnswer
	if reason != "" && !slices.Contains(c.readReasons, reason) {
		answer.owe = reason
	}
	switch {
	case seq == 0:
		// No message calls: none holds the read.
	case flight != nil && flight.pastCap == seq && !c.heldBy(seq):
		// Row 8.1: the first message past the cap since the read in flight began takes the hold
		// of the read after it, from a count of 1, whatever message the cursor names.
		answer.back = min(max(dueIn, holdAtLeast), ReadRetryAfter)
		answer.hold, answer.holds = true, 1
	case c.readHolder != nil && *c.readHolder != seq:
		// The cursor names another message as the holder: this one holds none.
	case c.readHolder != nil && c.readHolds >= stateMaxHolds:
		answer.release, answer.holds = true, c.readHolds
	default:
		answer.back = min(max(dueIn, holdAtLeast), ReadRetryAfter)
		answer.hold, answer.holds = c.readHolder == nil, c.readHolds+1
	}
	return answer
}

// follows reports whether the cursor follows the daemon at stateURL. A cursor that is missing, or
// that follows another daemon than the setting names, or none at all, as a write of the setting
// leaves it (settingWritten), is no starting point.
func (c cursor) follows(stateURL string) bool {
	return c.exists && c.stateURL == stateURL
}

// forgets reports whether a message of the store stateID is one of a daemon the setting no longer
// names: its store is the one a write that named another daemon recorded (previousStateID), or one
// marked as no daemon of the project (foreignStateIDs), and the cursor holds no store or another
// store. R3 applies no message of a daemon the setting named before once the setting names another.
// Such a message calls for no read of its own, but for a boot (drops), and makes one only as the
// holder of a read it took past stateMaxWaiting (judgeCrowded, applyBatch). It waits on the
// project's runner behind a read in flight, as every message does, and is judged against the
// cursor that read leaves. The message may be the setting's own daemon's, behind the setting's URL
// in this store, so it calls for the owed read where that read is due and no message holds it,
// whatever store the cursor holds, which the runner makes after it, and adds no reason (callRead).
// A write back to the URL recorded with that store clears the record, every write that resets the
// cursor clears the marks (settingWritten), and a good read whose store is that one clears the
// record or the mark (afterRead): the daemon's messages are then the setting's daemon's again,
// taken as every message of the cursor's store is.
func (c cursor) forgets(stateID string) bool {
	if c.holdsStore(stateID) {
		return false
	}
	return c.isPrevious(stateID) || c.marks(stateID)
}

// drops reports whether the cursor finishes message unapplied as a message of a daemon the
// setting no longer names, with no read of its own (outcomeForgotten): a message of a store it
// forgets, but a boot of such a store while the cursor follows stateURL, the setting's URL, in a
// store a read verified (read-rule-cases row 3.5). Such a boot may say that a redeploy started the
// setting's daemon in that store with no write of the setting. R3 reads another store so that a
// daemon whose store was replaced is found again, so the boot is judged as a message of another
// store (ReadReasonStore): on the runner (judge), past stateMaxWaiting (judgeCrowded) and when the
// database refuses it (readRefused). A message the cursor names as the owed read's holder is never
// finished unapplied so: applyBatch, judgeCrowded and readRefused check heldBy beside drops.
func (c cursor) drops(message stateMessage, stateURL string) bool {
	return c.forgets(message.StateID) && !(message.boots() && c.follows(stateURL) && c.stateID != nil)
}

// marks reports whether the cursor marks the store stateID as no daemon of the project
// (foreignStateIDs).
func (c cursor) marks(stateID string) bool {
	return slices.Contains(c.foreignStateIDs, stateID)
}

// holdsStore reports whether the cursor's sequence is counted in the store stateID.
func (c cursor) holdsStore(stateID string) bool {
	return c.stateID != nil && *c.stateID == stateID
}

// isPrevious reports whether stateID is the store that a write of the setting recorded as the
// store of the daemon the setting named before (previousStateID).
func (c cursor) isPrevious(stateID string) bool {
	return c.previousStateID != nil && *c.previousStateID == stateID
}

// marking is the cursor's marks (foreignStateIDs) with the store stateID marked as no daemon of
// the project, as the newest (marked). The cursor's own store and the store a write recorded
// (isPrevious) are never marked: for either of them, marking is the marks without stateID. Two
// steps mark a store. A good store read that finds the cursor's store unchanged marks the store
// of the message that called for it (afterRead, read-rule-cases row 5.7). The message whose hold a
// write of the setting ended marks its own store at its next delivery (endHold, row 6.3b).
func (c cursor) marking(stateID string) []string {
	if c.holdsStore(stateID) || c.isPrevious(stateID) {
		return without(c.foreignStateIDs, stateID)
	}
	return marked(c.foreignStateIDs, stateID)
}

// releases reports whether the message at stream sequence seq is the one whose hold of the owed
// read a write of the setting ended (releasedHolder). 0, a message with no JetStream metadata, is
// none.
func (c cursor) releases(seq int64) bool {
	return seq != 0 && c.releasedHolder != nil && *c.releasedHolder == seq
}

// heldBy reports whether the message at stream sequence seq is the one that holds the owed read
// (readHolder). 0, a message with no JetStream metadata, holds none.
func (c cursor) heldBy(seq int64) bool {
	return seq != 0 && c.readHolder != nil && *c.readHolder == seq
}

// skippedAbove reports whether a message of the store stateID that the intake acknowledged
// unapplied past stateMaxWaiting carried a sequence above seq (skippedStateIDs, skippedSeqs).
func (c cursor) skippedAbove(stateID string, seq int64) bool {
	i := slices.Index(c.skippedStateIDs, stateID)
	return i >= 0 && c.skippedSeqs[i] > seq
}

// skippedElsewhere reports whether the cursor's skipped record holds a store other than the one
// left holds that left does not drop (forgets): left is the cursor a good read leaves.
func (c cursor) skippedElsewhere(left cursor) bool {
	return slices.ContainsFunc(c.skippedStateIDs, func(stateID string) bool {
		return !left.holdsStore(stateID) && !left.forgets(stateID)
	})
}

// with is reasons with each of more after it that it does not hold yet, in more's order.
func with(reasons []string, more ...string) []string {
	out := slices.Clone(reasons)
	for _, reason := range more {
		if !slices.Contains(out, reason) {
			out = append(out, reason)
		}
	}
	return out
}

// skips is the cursor's skipped sequences with seq skipped in the store stateID: that store's
// highest, at the end as the store skipped last, and once. Past maxSkippedStores the stores
// skipped longest ago go. Never nil: the array columns are not null.
func (c cursor) skips(stateID string, seq int64) (stateIDs []string, seqs []int64) {
	stateIDs, seqs = make([]string, 0, len(c.skippedStateIDs)+1), make([]int64, 0, len(c.skippedSeqs)+1)
	for i, one := range c.skippedStateIDs {
		if one == stateID {
			seq = max(seq, c.skippedSeqs[i])
			continue
		}
		stateIDs, seqs = append(stateIDs, one), append(seqs, c.skippedSeqs[i])
	}
	stateIDs, seqs = append(stateIDs, stateID), append(seqs, seq)
	if n := len(stateIDs); n > maxSkippedStores {
		stateIDs, seqs = stateIDs[n-maxSkippedStores:], seqs[n-maxSkippedStores:]
	}
	return stateIDs, seqs
}

// marked is stores, the stores a cursor marks as no daemon of its project (foreignStateIDs), with
// stateID marked as the newest: at the end, and once. Past maxForeignStores the oldest marks go.
func marked(stores []string, stateID string) []string {
	kept := without(stores, stateID)
	kept = append(kept, stateID)
	if n := len(kept); n > maxForeignStores {
		kept = kept[n-maxForeignStores:]
	}
	return kept
}

// without is values without value, in their order and never nil: the array columns
// foreign_state_ids and read_reasons are not null.
func without(values []string, value string) []string {
	kept := make([]string, 0, len(values)+1)
	for _, one := range values {
		if one != value {
			kept = append(kept, one)
		}
	}
	return kept
}

// emptyCursor is a cursor that follows no daemon (an empty stateURL), with every column at its
// empty value: no run, sequence, reason, read start, holder, store, schema version, recorded
// store, mark, released holder or skipped sequence. Each array is empty and never nil, since the
// array columns are not null. It is the cursor of a project with no row (scanCursor), and the
// base of the cursor that a write of the setting resets a row to (cursor.reset).
func emptyCursor() cursor {
	return cursor{readReasons: []string{}, foreignStateIDs: []string{}, skippedStateIDs: []string{}, skippedSeqs: []int64{}}
}

// cursorColumns is the legion_event_cursors select list scanCursor reads, in scan order.
const cursorColumns = `state_url, run, last_seq, read_reasons, last_read_started_at, read_holder, read_holds, state_id,
	schema_version, previous_state_id, previous_state_url, foreign_state_ids, released_holder, skipped_state_ids, skipped_seqs`

// scanCursor decodes one cursorColumns row, or no row as a cursor that does not exist
// (emptyCursor). The array columns are not null, so a row's arrays are never nil either. So no
// caller guards a cursor's array against nil, and no cursor computed from one writes NULL into an
// array column (writeCursor).
func scanCursor(row pgx.Row) (cursor, error) {
	current := cursor{exists: true}
	err := row.Scan(&current.stateURL, &current.run, &current.lastSeq, &current.readReasons, &current.lastReadStartedAt,
		&current.readHolder, &current.readHolds, &current.stateID, &current.schemaVersion, &current.previousStateID,
		&current.previousStateURL, &current.foreignStateIDs, &current.releasedHolder, &current.skippedStateIDs, &current.skippedSeqs)
	if errors.Is(err, pgx.ErrNoRows) {
		return emptyCursor(), nil
	}
	return current, err
}

// writeCursor writes c as project's legion_event_cursors row inside tx, in one statement: every
// column of cursorColumns from c, but last_read_started_at, which takes the later of the row's
// value and c's. So no write moves the start of the project's newest whole read back. A project
// with no row is given one. recordWhole writes the cursor a read leaves (cursor.afterRead), and
// settingWritten writes the cursor a write of the setting resets a row to (cursor.reset).
func writeCursor(ctx context.Context, tx pgx.Tx, project string, c cursor) error {
	_, err := tx.Exec(ctx, `
		insert into legion_event_cursors (project, state_url, run, last_seq, read_reasons, last_read_started_at, read_holder,
			read_holds, state_id, schema_version, previous_state_id, previous_state_url, foreign_state_ids, released_holder,
			skipped_state_ids, skipped_seqs)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
		on conflict (project) do update set
			state_url = excluded.state_url, run = excluded.run, last_seq = excluded.last_seq,
			read_reasons = excluded.read_reasons,
			last_read_started_at = greatest(legion_event_cursors.last_read_started_at, excluded.last_read_started_at),
			read_holder = excluded.read_holder, read_holds = excluded.read_holds, state_id = excluded.state_id,
			schema_version = excluded.schema_version, previous_state_id = excluded.previous_state_id,
			previous_state_url = excluded.previous_state_url, foreign_state_ids = excluded.foreign_state_ids,
			released_holder = excluded.released_holder, skipped_state_ids = excluded.skipped_state_ids,
			skipped_seqs = excluded.skipped_seqs
	`, project, c.stateURL, c.run, c.lastSeq, c.readReasons, c.lastReadStartedAt, c.readHolder, c.readHolds, c.stateID,
		c.schemaVersion, c.previousStateID, c.previousStateURL, c.foreignStateIDs, c.releasedHolder, c.skippedStateIDs,
		c.skippedSeqs)
	return err
}

// rowReader is what readPosition reads through: the pool, outside any transaction, or a
// transaction.
type rowReader interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// readPosition reads project's setting and then its cursor (cursorColumns, scanCursor) through db:
// inside a transaction with the setting `for share` and the cursor `for update` where locked is
// set (lockPosition), and outside any transaction with no lock otherwise (position). found is
// false for a project with no setting.
func readPosition(ctx context.Context, db rowReader, project string, locked bool) (stateURL string, current cursor, found bool, err error) {
	settingLock, cursorLock := "", ""
	if locked {
		settingLock, cursorLock = " for share", " for update"
	}
	switch err := db.QueryRow(ctx, `select state_url from legion_settings where project = $1`+settingLock, project).Scan(&stateURL); {
	case errors.Is(err, pgx.ErrNoRows):
		return "", cursor{}, false, nil
	case err != nil:
		return "", cursor{}, false, fmt.Errorf("read the Legion setting of %s: %w", project, err)
	}
	current, err = scanCursor(db.QueryRow(ctx, `select `+cursorColumns+` from legion_event_cursors where project = $1`+cursorLock, project))
	if err != nil {
		return "", cursor{}, false, fmt.Errorf("read where the Legion events of %s stand: %w", project, err)
	}
	return stateURL, current, true, nil
}

// position reads project's setting and its cursor outside any transaction (readPosition), for a
// decision whether to read the daemon whole (startingPoint, readRefused), which the read's record
// checks again under the locks it takes (recordWhole); found is false for a project with no
// setting.
func (in *Intake) position(ctx context.Context, project string) (setting Settings, current cursor, found bool, err error) {
	stateURL, current, found, err := readPosition(ctx, in.pool, project, false)
	if err != nil || !found {
		return Settings{}, cursor{}, false, err
	}
	return Settings{Project: project, StateURL: stateURL}, current, true, nil
}

// lockPosition reads project's setting `for share` and its cursor `for update` inside tx
// (readPosition), which is how each write of the project's observations opens (applyBatch,
// recordWhole): a write of the setting (settingWritten) waits for the transaction, and so does
// each other write of the cursor. found is false for a project with no setting.
func lockPosition(ctx context.Context, tx pgx.Tx, project string) (stateURL string, current cursor, found bool, err error) {
	return readPosition(ctx, tx, project, true)
}

// lockCursor is lockPosition for a write that needs the project's cursor row locked whether or not
// the project has one yet (judgeCrowded, recordWhole). Where the project has a setting and no
// cursor, it inserts one that follows no daemon (an empty state_url, as a write of the setting
// leaves it), does nothing where another transaction inserted the row first, and locks the row
// again. So two such writes of one project serialize on the row: neither computes what it writes
// from no row while the other inserts one, so no write takes the place of the other.
func lockCursor(ctx context.Context, tx pgx.Tx, project string) (stateURL string, current cursor, found bool, err error) {
	stateURL, current, found, err = lockPosition(ctx, tx, project)
	if err != nil || !found || current.exists {
		return stateURL, current, found, err
	}
	if _, err := tx.Exec(ctx, `
		insert into legion_event_cursors (project, state_url) values ($1, '') on conflict (project) do nothing
	`, project); err != nil {
		return "", cursor{}, false, fmt.Errorf("start where the Legion events of %s stand: %w", project, err)
	}
	return lockPosition(ctx, tx, project)
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
//     record of the store the setting named before (recordWhole); a read that finds the cursor's
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
		result, err := endHold(ctx, tx, project, current, message.StateID)
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
	// sets the store (recordWhole).
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

// part is one held message of a transaction the daemon split over several (legion_event_parts,
// migration 20261118212431): every message of the transaction but its last carries `more`, and
// none of its events is applied until the last arrives (applyBatch), so no observation holds a
// value of a transaction Dispatch has not read whole. A part is held only where it follows the
// cursor or the part before it (stateMessage.continues), so the parts of a project are one run of
// sequences under one run and one store. A good whole read deletes them, since its snapshot holds
// them, and so does every write of the project's setting that leaves the cursor no starting point,
// and the delete of the setting, which deletes the cursor they belong to (settingWritten); a failed
// read keeps them (recordWhole).
type part struct {
	firstSeq, lastSeq int64
	run, stateID      string
	bytes             int
	events            []stateEvent
}

// heldParts reads project's held parts inside tx, which holds the project's cursor `for update`
// (lockPosition), in sequence order, each part's events decoded and observed as its message's
// were.
func heldParts(ctx context.Context, tx pgx.Tx, project string) ([]part, error) {
	rows, err := tx.Query(ctx, `
		select first_seq, last_seq, run, state_id, bytes, events from legion_event_parts
		where project = $1 order by first_seq
	`, project)
	if err != nil {
		return nil, fmt.Errorf("read the held state events of %s: %w", project, err)
	}
	held, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (part, error) {
		var one part
		var events string
		if err := row.Scan(&one.firstSeq, &one.lastSeq, &one.run, &one.stateID, &one.bytes, &events); err != nil {
			return part{}, err
		}
		one.events, err = decodeEvents([]byte(events))
		return one, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the held state events of %s: %w", project, err)
	}
	return held, nil
}

// holdPart holds message, a part of a split transaction, inside tx: its sequences, run, store,
// commit time, bytes and its events as the daemon sent them. A part held already, delivered
// again, is held once.
func holdPart(ctx context.Context, tx pgx.Tx, project string, message stateMessage, bytes int) error {
	if _, err := tx.Exec(ctx, `
		insert into legion_event_parts (project, first_seq, last_seq, run, state_id, at, bytes, events)
		values ($1, $2, $3, $4, $5, $6, $7, $8)
		on conflict (project, first_seq) do nothing
	`, project, message.FirstSeq, message.Seq, message.Run, message.StateID, message.At, bytes, string(message.RawEvents)); err != nil {
		return fmt.Errorf("hold the state events of %s: %w", project, err)
	}
	return nil
}

// recordEvents appends, inside a batch's transaction, a row for each observation whose value
// differs from legion_current's for its field, and answers how many it wrote. A first `null` writes
// nothing, as a whole read's does (recordObservations), and a field the batch does not name is left
// as it was: an event names what moved and nothing else.
//
// A row is observed at the batch's `at`, the daemon's commit time, or 1 µs after the field's newest
// row where that is later: a whole read stamps its rows with Dispatch's own clock, which can be
// past the daemon's, and legion_current moves only forward, so an event after the read must land
// after it to be the field's newest. The rule holds the other way round too: a whole read stamps
// all its rows at one moment, 1 µs after the project's newest row where that is later than the
// read's time (recordObservations), since the daemon's clock can as well be past Dispatch's. So
// whichever of the two Dispatch writes last is each field's newest, whatever the two clocks say.
// A batch's rows keep their own times: the batch is one daemon transaction, at one `at`.
func recordEvents(ctx context.Context, tx pgx.Tx, project string, at time.Time, observations []Observation) (int64, error) {
	issueKeys, fields, values := observationColumns(observations)
	tag, err := tx.Exec(ctx, `
		with moved as (
			select moved.issue_key, moved.field, moved.value::jsonb as value
			from unnest($2::text[], $3::text[], $4::text[]) as moved(issue_key, field, value)
		)
		insert into legion_observations (project, issue_key, field, value, observed_at)
		select $1, moved.issue_key, moved.field, moved.value,
			greatest($5::timestamptz, newest.observed_at + interval '1 microsecond')
		from moved
		left join legion_current newest
			on newest.project = $1 and newest.issue_key = moved.issue_key and newest.field = moved.field
		where coalesce(newest.value, 'null'::jsonb) is distinct from moved.value
	`, project, issueKeys, fields, values, at)
	if err != nil {
		return 0, fmt.Errorf("store the Legion observations of %s's state events: %w", project, err)
	}
	return tag.RowsAffected(), nil
}

// lastValues is each field the events name at its last value, in sequence order: one fact can
// write a field twice in one transaction (a slot released and granted again), and only the value
// the transaction committed is one the daemon's state ever held.
func lastValues(events []stateEvent) []Observation {
	type field struct{ issue, name string }
	at := map[field]int{}
	var values []Observation
	for _, event := range events {
		for _, observation := range event.observations {
			key := field{observation.IssueKey, observation.Field}
			if index, seen := at[key]; seen {
				values[index].Value = observation.Value
				continue
			}
			at[key] = len(values)
			values = append(values, observation)
		}
	}
	return values
}

// wholeRead is one whole read of a daemon's state as fetchWhole made it, held until it is recorded
// (recordWhole), so a record the database refuses is made again without reading the daemon again.
type wholeRead struct {
	stateURL string
	// stateSeq is the daemon.stateSeq the state serves, stateID its daemon.stateId, stateRun its
	// daemon.stateRun and schemaVersion its daemon.schemaVersion, each nil for none.
	stateSeq      *int64
	stateID       *string
	stateRun      *string
	schemaVersion *int64
	// observations is what the state observes (State.Observations), recorded where observed is
	// true: the daemon answered with the setting's project's state, failed read or not. bytes is
	// the answer's size as the read knows it (Client.State).
	observations []Observation
	observed     bool
	bytes        int64
	// started is Dispatch's clock before the call to the daemon: the read's start, which the next
	// read waits ReadRetryAfter after (last_read_started_at), however long the daemon takes to
	// answer. at is Dispatch's clock when the daemon had answered: the read's time, which its
	// legion_reads row and its observations take. took is how long the call took.
	started time.Time
	at      time.Time
	took    time.Duration
	// failure is why the read is a failed one, nil for a good read.
	failure error
}

// fetchWhole reads setting's daemon's state and judges it: a read the daemon did not answer, or
// answered with another project's state, or with a state that cannot be a starting point
// (ErrNoDispatchURL, ErrNoStateSeq, ErrStateSeqZero, ErrNoStateID, judged in that order), is a
// failed read. A daemon that runs no workflow is judged by its settings first, whatever stateSeq it
// serves: it commits no state event, so it serves stateSeq 0 too. The read's start is started, the
// intake's clock before the call, and its time now's once the daemon has answered. A call presents
// no credential: `GET /legion/v1/state` authenticates nobody. Every Client shares the default
// transport, so a client per read still reuses the connection the last read left.
func fetchWhole(ctx context.Context, setting Settings, started time.Time, now func() time.Time) *wholeRead {
	call := time.Now()
	state, bytes, err := New(setting.StateURL).State(ctx)
	read := &wholeRead{stateURL: setting.StateURL, bytes: bytes, started: started, at: now().UTC(), took: time.Since(call)}
	if state.Daemon != nil {
		read.stateSeq, read.schemaVersion = state.Daemon.StateSeq, state.Daemon.SchemaVersion
		if state.Daemon.StateID != "" {
			read.stateID = &state.Daemon.StateID
		}
		if state.Daemon.StateRun != "" {
			read.stateRun = &state.Daemon.StateRun
		}
	}
	switch {
	case err != nil:
		read.failure = err
		return read
	case state.Daemon == nil || state.Daemon.Project != setting.Project:
		project := ""
		if state.Daemon != nil {
			project = state.Daemon.Project
		}
		read.failure = fmt.Errorf("%w: it serves %q, and the setting is %s's", ErrStateOfAnotherProject, project, setting.Project)
		return read
	}
	read.observations, read.observed = state.Observations(), true
	switch seq := read.stateSeq; {
	case JSONString(state.Settings["dispatch_url"]) == "":
		read.failure = ErrNoDispatchURL
	case seq == nil:
		read.failure = ErrNoStateSeq
	case *seq == 0:
		read.failure = ErrStateSeqZero
	case read.stateID == nil:
		read.failure = ErrNoStateID
	}
	return read
}

// ReadWhole is the intake's whole read (Intake.readWhole) of setting's daemon for reason, made once
// and on Dispatch's wall clock, for a caller that is not on the project's runner: the record's
// tests outside the intake drive the intake's own read through it, and the intake calls readWhole.
// Its record clears every reason the project's cursor owes as it begins, as the record of a read
// the read rule makes does (owedRead.clears). It answers an error only for a cursor it could not
// read, for the emptying of the skipped sequences that the database refused, for a record the
// database refused, refused again as a failed read, and for a context that ended, and records
// nothing when the setting no longer names setting.StateURL by the time it is written.
func ReadWhole(ctx context.Context, pool *store.Pool, setting Settings, reason string) error {
	in := &Intake{pool: pool, now: time.Now, reading: map[string]readInFlight{}}
	_, current, _, err := in.position(ctx, setting.Project)
	if err != nil {
		return err
	}
	owed := readOf(setting, reason)
	owed.clears = current.readReasons
	var pending *wholeRead
	_, err = in.readWhole(ctx, owed, &pending)
	return err
}

// readWhole reads the daemon of owed's setting whole and records it (D9's whole read, made for a
// reason and never on a timer), for owed's reasons, which name the one legion_reads records
// (owedRead): the state through Client.State (fetchWhole), its observations at one moment, the
// intake's now or just after the project's newest row (recordObservations), a legion_reads row of
// kind `read` at the intake's now, and the project's cursor set to the daemon's own
// daemon.stateSeq, daemon.stateId and daemon.schemaVersion, which takes the cursor back too after a
// database restore took the daemon's counter back, and to its daemon.stateRun where it serves one
// (recordWhole). A read that fails, and one fetchWhole judges failed, records a failed legion_reads
// row naming why, its observations where the state is the project's own, and leaves the read owed.
// Each read is announced on StateChannel and logged at INFO with its reasons (finishWhole). From
// the read's start until readWhole returns, the intake knows that start as the start of the read in
// flight on the project's runner (reading). Before it calls the daemon, it empties the sequences
// the cursor skipped past stateMaxWaiting (cursor.skippedStateIDs): the daemon commits each event
// before it publishes it, so the read holds every event of a message published before it began,
// and only a message skipped while it is in flight can be past its snapshot (recordWhole). So a
// read owes no gap read for a message skipped before it, whatever sequence that message claims.
//
// It keeps the read it fetched in pending until it is recorded, so an attempt made again after a
// refused record records that read rather than reading the daemon again. It answers the read as it
// was recorded, failed or good, and nil where it was not recorded: when the setting moved
// meanwhile. It answers an error only for a write the database refused, the record or the emptying
// of the skipped sequences, and for a stop that cut the read short.
func (in *Intake) readWhole(ctx context.Context, owed owedRead, pending **wholeRead) (*wholeRead, error) {
	read := *pending
	if read == nil || read.stateURL != owed.setting.StateURL {
		if _, err := in.pool.Exec(ctx, `
			update legion_event_cursors set skipped_state_ids = '{}', skipped_seqs = '{}'
			where project = $1 and cardinality(skipped_state_ids) > 0
		`, owed.setting.Project); err != nil {
			return nil, fmt.Errorf("empty the state messages of %s skipped past the cap before a whole read: %w", owed.setting.Project, err)
		}
		started := in.now().UTC()
		in.reads(owed.setting.Project, &started)
		defer in.reads(owed.setting.Project, nil)
		read = fetchWhole(ctx, owed.setting, started, in.now)
		if err := ctx.Err(); err != nil {
			// The read was cut short by the stop, not by the daemon: nothing is recorded.
			return nil, err
		}
		*pending = read
	}
	recorded, err := finishWhole(ctx, in.pool, owed, read)
	if err != nil {
		return nil, err
	}
	*pending = nil
	return recorded, nil
}

// reads records started as the start of the whole read in flight on project's runner (reading),
// with no message past stateMaxWaiting judged since it began, and nil as no read in flight.
func (in *Intake) reads(project string, started *time.Time) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if started == nil {
		delete(in.reading, project)
		return
	}
	in.reading[project] = readInFlight{started: *started}
}

// finishWhole records read, made for owed (recordWhole, which announces it on StateChannel), then
// logs it, naming owed's recorded reason as its reason and every reason it joined as its reasons,
// comma-separated, and answers the read as it was recorded, nil where it was not recorded. A
// record Postgres refused is recorded as a failed read instead, its failure `record: <why>` and
// none of its observations, so the last good observations stand and the read is owed rather than
// its project's runner held on a value the database will never take. An error is answered only
// when that record is refused too: the database itself is failing, and the caller tries the record
// again in place.
func finishWhole(ctx context.Context, pool *store.Pool, owed owedRead, read *wholeRead) (*wholeRead, error) {
	project := owed.setting.Project
	recorded, written, err := recordWhole(ctx, pool, owed, read)
	if err != nil && ctx.Err() == nil {
		slog.Warn("legion intake: record a whole read; recording it as a failed read", "project", project, "reason", owed.reason, "error", err)
		refused := *read
		refused.failure, refused.observed = fmt.Errorf("record: %w", err), false
		if recorded, written, err = recordWhole(ctx, pool, owed, &refused); err == nil {
			read = &refused
		}
	}
	if err != nil || !recorded {
		if err == nil {
			slog.Info("legion intake: the Legion setting changed during a whole read; it is not recorded", "project", project, "reason", owed.reason)
		}
		return nil, err
	}
	attributes := []any{"project", project, "reason", owed.reason, "reasons", strings.Join(owed.joined, ","),
		"stateSeq", logValue(read.stateSeq), "stateId", logValue(read.stateID), "stateRun", logValue(read.stateRun),
		"bytes", read.bytes, "duration", read.took, "ok", read.failure == nil, "rows", written}
	if read.failure != nil {
		attributes = append(attributes, "error", read.failure.Error())
	}
	slog.Info(wholeReadLine, attributes...)
	return read, nil
}

// recordWhole writes read, made for owed, in one transaction that holds the project's position
// (lockCursor): the step every whole read's record takes (recordRead: its observations where it
// observed them, its legion_reads row naming owed's recorded reason, and its announcement), then
// the cursor the read leaves (cursor.afterRead), in one statement (writeCursor). A project with no
// cursor is given one that follows no daemon before anything is computed, so a message past
// stateMaxWaiting that judgeCrowded judges while the read is in flight either waits for this
// record or is seen by it, and nothing it recorded is written over (R3-h, read-rule-cases' second
// invariant). A good read deletes the project's held parts (part), which its snapshot holds, and
// so does a read of a cursor that followed another daemon. A failed read of the daemon the cursor
// follows keeps them: the read holds none of the events, and a part held is applied whole when its
// transaction's last part arrives.
//
// recorded is false when the setting no longer names the daemon read. Then nothing of the read is
// written but its start (last_read_started_at): the daemon was read all the same, so the read that
// the setting's change calls for waits ReadRetryAfter after it.
func recordWhole(ctx context.Context, pool *store.Pool, owed owedRead, read *wholeRead) (recorded bool, written int64, err error) {
	project := owed.setting.Project
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, 0, fmt.Errorf("open the transaction of the Legion state read of %s: %w", project, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	stateURL, current, found, err := lockCursor(ctx, tx, project)
	if err != nil || !found {
		return false, 0, err
	}
	if stateURL != read.stateURL {
		if _, err := tx.Exec(ctx, `
			update legion_event_cursors set last_read_started_at = greatest(last_read_started_at, $2) where project = $1
		`, project, read.started); err != nil {
			return false, 0, fmt.Errorf("record the start of a whole read of %s whose setting moved: %w", project, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return false, 0, fmt.Errorf("commit the start of a whole read of %s whose setting moved: %w", project, err)
		}
		return false, 0, nil
	}
	if written, err = recordRead(ctx, tx, project, owed.reason, read); err != nil {
		return false, 0, err
	}
	if err := writeCursor(ctx, tx, project, current.afterRead(owed, read)); err != nil {
		return false, 0, fmt.Errorf("set where the Legion events of %s stand: %w", project, err)
	}
	if read.failure == nil || !current.follows(stateURL) {
		if _, err := tx.Exec(ctx, `delete from legion_event_parts where project = $1`, project); err != nil {
			return false, 0, fmt.Errorf("delete the held state events of %s: %w", project, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, 0, fmt.Errorf("commit the Legion state read of %s: %w", project, err)
	}
	return true, written, nil
}

// afterRead is the cursor that read, made for owed, leaves (recordWhole). c is the cursor as the
// read's record finds it, and read.stateURL is the setting's URL: the cursor left follows the
// setting's daemon. Every column that the rules below do not name keeps c's value,
// released_holder among them.
//
// Every read clears the reasons the cursor owed when it was decided (owed.clears). A failed one
// owes the read for retry (ReadReasonRetry), and after it each reason that a message added while
// the read was in flight (owedSince). A good one clears only the reasons its stateSeq answers
// (read-rule-cases row 5.1). readWhole empties the skipped record as the read begins, so each
// sequence the record holds is of a message that judgeCrowded acknowledged unapplied past
// stateMaxWaiting while the read was in flight:
//   - Where such a message is of the read's store and carried a sequence above its stateSeq
//     (skippedAbove), its events are past the read's snapshot. The gap read stays owed, whatever
//     reasons were owed when the read was decided, and so does each reason added while the read
//     was in flight.
//   - Where such a message is of another store, one the cursor the read leaves does not drop
//     (skippedElsewhere), it came after the read began, so the read's snapshot does not answer the
//     read it calls for. The store read stays owed, and so does each reason added while the read
//     was in flight.
//   - Otherwise the read clears those reasons too. The messages that added them are at or below
//     its snapshot or of a store the cursor drops, but for the one that holds the read, whose
//     redelivery is then judged as any message.
//
// A good read empties the skipped record once it has so decided, and a failed one keeps it.
//
// Where a good read leaves no reason owed, it clears the message that held the read with its count
// of hand-backs. Where it leaves one owed, the message that holds the read, the first message past
// stateMaxWaiting since the read began (judgeCrowded), holds that reason, so it stays the holder
// with its count. A failed read keeps the message that holds the read with its count: the holder
// comes back to make the read again ReadRetryAfter after this one began, until it has gone back
// stateMaxHolds times (readFor). Then the cursor names no holder, and the read waits for the
// project's next message, boot, settings change or take-over. The holder and its count go only
// with a good read that leaves no reason owed, or with the message: its release after
// stateMaxHolds hand-backs (owe), or a write of the setting that resets the cursor
// (settingWritten).
//
// A good read sets the cursor to its stateSeq, its stateId and its schemaVersion. A failed read
// keeps the cursor's sequence, store and schema version where c follows the same daemon, and sets
// none where it does not. A good read whose state serves a stateRun sets the cursor's run to it:
// the run of the daemon that served the read, which is the run of every message that daemon run
// publishes. So a boot of that run at or below the read's stateSeq, one that reaches Dispatch
// after the read, is one the read holds and not a daemon whose counter went back
// (stateMessage.unfollowed). Any other read keeps the cursor's run where c follows the same
// daemon, and sets none where it does not.
//
// Every read, a good starting read's included, sets last_read_started_at to the read's start, and
// the next read waits ReadRetryAfter after it (readDueIn). So the minute of a read the daemon is
// slow to answer, or never answers, counts from when the read began and not from when it ended.
// And a gap or a boot that a message calls for right after a good starting read is made when that
// minute ends, through the delay of the message that holds it (read-rule-cases v11).
//
// A good read whose store is the one a write recorded (isPrevious) clears that store and its URL
// (previous_state_id, previous_state_url): the setting's daemon is the daemon the setting named
// before, behind another URL, so its messages are taken from the read on as every message of the
// cursor's store is. Every other read keeps both (forgets). A good read takes its own store out of
// the stores marked as no daemon of the project (foreign_state_ids). A good read that a message of
// another store made (owed.store), and that finds the cursor's store unchanged, marks that
// message's store (marking): the store's later messages are finished unapplied and call for no
// read of their own.
func (c cursor) afterRead(owed owedRead, read *wholeRead) cursor {
	next := c
	next.exists, next.stateURL, next.lastReadStartedAt = true, read.stateURL, &read.started
	if !c.follows(read.stateURL) {
		next.run, next.lastSeq, next.stateID, next.schemaVersion = nil, nil, nil, nil
	}
	since := owedSince(c.readReasons, owed.clears)
	if read.failure != nil {
		next.readReasons = append([]string{ReadReasonRetry}, without(since, ReadReasonRetry)...)
		return next
	}
	next.lastSeq, next.stateID, next.schemaVersion = read.stateSeq, read.stateID, read.schemaVersion
	if read.stateRun != nil {
		next.run = read.stateRun
	}
	if c.isPrevious(*read.stateID) {
		next.previousStateID, next.previousStateURL = nil, nil
	}
	next.foreignStateIDs = without(c.foreignStateIDs, *read.stateID)
	if owed.store != "" && c.follows(read.stateURL) && c.holdsStore(*read.stateID) {
		next.foreignStateIDs = next.marking(owed.store)
	}
	var more []string
	if c.skippedAbove(*read.stateID, *read.stateSeq) {
		more = append(more, ReadReasonGap)
	}
	if c.skippedElsewhere(next) {
		more = append(more, ReadReasonStore)
	}
	next.readReasons = []string{}
	if len(more) > 0 {
		next.readReasons = with(since, more...)
	}
	if len(next.readReasons) == 0 {
		next.readHolder, next.readHolds = nil, 0
	}
	next.skippedStateIDs, next.skippedSeqs = []string{}, []int64{}
	return next
}

// owedSince is reasons, the reasons a cursor owes as a read's record is written, less one of each
// reason in clears, the reasons it owed as the read was decided: the reasons a message added while
// the read was in flight, in the order they were added. Never nil: the column read_reasons is not
// null.
func owedSince(reasons, clears []string) []string {
	left := slices.Clone(clears)
	since := []string{}
	for _, reason := range reasons {
		if index := slices.Index(left, reason); index >= 0 {
			left = slices.Delete(left, index, index+1)
			continue
		}
		since = append(since, reason)
	}
	return since
}

// stateMessage is one message of a daemon's state lane (packages/contracts' legionStateMessage):
// the state events of one transaction of the daemon, or of one part of a transaction the daemon
// split over several (More), which share the transaction's project, run, store and commit time.
// Every member is read loosely: a member or an event kind a newer daemon adds is ignored, as every
// consumer of the bus ignores what it does not know.
type stateMessage struct {
	// Project is the Dispatch project key the daemon publishes for.
	Project string `json:"project"`
	// Run is the run id (a UUID) of the daemon process that wrote the events.
	Run string `json:"run"`
	// StateID is the id of the store the events are counted in (the state route's
	// daemon.stateId).
	StateID string `json:"stateId"`
	// FirstSeq and Seq are the first and the last event's state sequence.
	FirstSeq int64 `json:"firstSeq"`
	Seq      int64 `json:"seq"`
	// At is the transaction's commit time on the daemon's clock.
	At time.Time `json:"at"`
	// More is a part of a transaction whose next part is still to come.
	More bool `json:"more"`
	// RawEvents is the message's events as the daemon sent them, which a held part keeps
	// (holdPart), and Events the same decoded and observed (decodeEvents).
	RawEvents json.RawMessage `json:"events"`
	Events    []stateEvent    `json:"-"`
}

// continues reports whether the message is the next part of the transaction whose last part held
// is last: its first sequence the one after last's, under last's run and store.
func (m stateMessage) continues(last part) bool {
	return m.FirstSeq == last.lastSeq+1 && m.Run == last.run && m.StateID == last.stateID
}

// The kinds of state event (packages/daemon's state lane).
const (
	eventChange    = "change"
	eventAdmission = "admission"
	eventBoot      = "boot"
)

// stateEvent is one state event, in the members of its three kinds Dispatch reads: a change names
// its issue and the fields that moved; an admission event carries the whole admission block the
// state route serves, of which Dispatch keeps its own members (Admission); a boot carries the
// settings the state route serves and the schema version of the daemon's store once the boot's
// migrations ran.
type stateEvent struct {
	Seq           int64                      `json:"seq"`
	Kind          string                     `json:"kind"`
	Issue         string                     `json:"issue"`
	Changes       []stateChange              `json:"changes"`
	Admission     *Admission                 `json:"admission"`
	Settings      map[string]json.RawMessage `json:"settings"`
	SchemaVersion int64                      `json:"schemaVersion"`

	// observations is what the event observes, each value through its field's one conversion
	// (state.go's phaseValue and the rest), which decodeEvents fills.
	observations []Observation
}

// stateChange is one field of a change event: the field and its new value, the value the state
// route serves at that issue. A claim names its role. The old value is not read: legion_current
// holds what Dispatch observed last.
type stateChange struct {
	Field string          `json:"field"`
	Role  string          `json:"role"`
	New   json.RawMessage `json:"new"`
}

// The fields a change event names (packages/contracts' legionStateChange).
const (
	changePhase     = "phase"
	changeTree      = "tree"
	changeSlot      = "slot"
	changeWaitingOn = "waitingOn"
	changeClaim     = "claim"
)

// decodeStateMessage decodes one message's envelope, unless keying decoded it already, and its
// payload, and converts each event into the observations it makes (decodeEvents). A message with
// no project, no store, no sequence, sequences that do not run forwards or no time, events that do
// not run from firstSeq to seq one sequence each, or an event whose value is not of its field's
// type, does not decode. The events' rule is the contract's (legionStateMessage's refine): the
// cursor moves to a message's seq (applyBatch), so a seq its events do not reach would move it
// past every later event, and no whole read would follow.
func decodeStateMessage(envelope *contracts.Envelope, data []byte) (stateMessage, error) {
	if envelope == nil {
		var decoded contracts.Envelope
		if err := json.Unmarshal(data, &decoded); err != nil {
			return stateMessage{}, fmt.Errorf("decode the envelope: %w", err)
		}
		envelope = &decoded
	}
	var message stateMessage
	if err := json.Unmarshal([]byte(envelope.Payload), &message); err != nil {
		return stateMessage{}, fmt.Errorf("decode the payload: %w", err)
	}
	switch {
	case message.Project == "":
		return stateMessage{}, errors.New("the message names no project")
	case message.StateID == "":
		return stateMessage{}, errors.New("the message names no store (stateId)")
	case message.FirstSeq < 1 || message.Seq < message.FirstSeq:
		return stateMessage{}, fmt.Errorf("the message's sequences %d to %d do not run forwards from 1", message.FirstSeq, message.Seq)
	case message.At.IsZero():
		return stateMessage{}, errors.New("the message carries no time")
	}
	events, err := decodeEvents(message.RawEvents)
	if err != nil {
		return stateMessage{}, err
	}
	if int64(len(events)) != message.Seq-message.FirstSeq+1 {
		return stateMessage{}, fmt.Errorf("the message's sequences %d to %d carry %d events: its events must run from firstSeq to seq, one sequence each",
			message.FirstSeq, message.Seq, len(events))
	}
	for index, event := range events {
		if want := message.FirstSeq + int64(index); event.Seq != want {
			return stateMessage{}, fmt.Errorf("the message's event %d is at sequence %d, not %d: its events must run from firstSeq to seq, one sequence each",
				index, event.Seq, want)
		}
	}
	message.Events = events
	return message, nil
}

// decodeEvents decodes the JSON array of a message's events, in the order the daemon sent them,
// which is sequence order (decodeStateMessage holds a message to it, and a held part is a message
// that decoded), and fills each event's observations (observe): a message's as it arrives, and a
// held part's as it was held.
func decodeEvents(raw json.RawMessage) ([]stateEvent, error) {
	var events []stateEvent
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &events); err != nil {
			return nil, fmt.Errorf("decode the events: %w", err)
		}
	}
	for index := range events {
		if err := events[index].observe(); err != nil {
			return nil, fmt.Errorf("event %d: %w", events[index].Seq, err)
		}
	}
	return events, nil
}

// observe fills the event's observations. An event of a kind this build does not know, and a
// change of a field it does not know, observes nothing.
func (e *stateEvent) observe() error {
	switch e.Kind {
	case eventChange:
		if e.Issue == "" {
			return errors.New("a change names no issue")
		}
		for _, change := range e.Changes {
			observation, known, err := changeObservation(e.Issue, change)
			if err != nil {
				return fmt.Errorf("%s of %s: %w", change.Field, e.Issue, err)
			}
			if known {
				e.observations = append(e.observations, observation)
			}
		}
	case eventAdmission:
		e.observations = []Observation{{IssueKey: AdmissionKey, Field: FieldAdmission, Value: admissionValue(e.Admission)}}
	case eventBoot:
		e.observations = []Observation{{IssueKey: AdmissionKey, Field: FieldSettings, Value: settingsValue(e.Settings)}}
	}
	return nil
}

// changeObservation is one change as the observation of its field, its new value decoded into the
// type the state document carries that field in and converted by the field's one conversion, so
// it is the value a whole read of the same state records. A `null` is the field the issue carries
// none of, which a whole read records as `null` too. known is false for a field this build does not
// observe.
func changeObservation(issue string, change stateChange) (observation Observation, known bool, err error) {
	observation.IssueKey = issue
	switch change.Field {
	case changePhase:
		var phase *string
		if err := decodeNew(change.New, &phase); err != nil {
			return Observation{}, false, err
		}
		observation.Field, observation.Value = FieldPhase, encodeValue(nil)
		if phase != nil {
			observation.Value = phaseValue(*phase)
		}
	case changeTree:
		var tree *string
		if err := decodeNew(change.New, &tree); err != nil {
			return Observation{}, false, err
		}
		observation.Field, observation.Value = FieldTree, treeValue(tree)
	case changeSlot:
		var index *int
		if err := decodeNew(change.New, &index); err != nil {
			return Observation{}, false, err
		}
		var slot *Slot
		if index != nil {
			slot = &Slot{Index: *index}
		}
		observation.Field, observation.Value = FieldSlot, slotValue(slot)
	case changeWaitingOn:
		observation.Field, observation.Value = FieldWaitingOn, waitingOnValue(change.New)
	case changeClaim:
		if change.Role == "" {
			return Observation{}, false, errors.New("a claim names no role")
		}
		var state *string
		if err := decodeNew(change.New, &state); err != nil {
			return Observation{}, false, err
		}
		claim := ""
		if state != nil {
			claim = *state
		}
		observation.Field, observation.Value = ClaimFieldPrefix+change.Role, claimValue(claim)
	default:
		return Observation{}, false, nil
	}
	return observation, true, nil
}

// decodeNew decodes a change's new value into target, leaving it nil where the value is absent.
func decodeNew(raw json.RawMessage, target any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, target)
}

// eventsAfter is each of events above lastSeq, all of them while the cursor holds no sequence.
func eventsAfter(events []stateEvent, lastSeq *int64) []stateEvent {
	if lastSeq == nil {
		return events
	}
	var after []stateEvent
	for _, event := range events {
		if event.Seq > *lastSeq {
			after = append(after, event)
		}
	}
	return after
}

// standsAt is the position a message is judged on (unfollowed, movesSchema): the last held part's
// sequence and run where parts are held, and the cursor's otherwise. seq is nil for a position
// that holds no sequence, which any message follows.
func standsAt(current cursor, held []part) (seq *int64, run *string) {
	if n := len(held); n > 0 {
		return &held[n-1].lastSeq, &held[n-1].run
	}
	return current.lastSeq, current.run
}

// unfollowed is the whole read a message calls for because it does not follow the position
// current and the parts held for it stand at (standsAt), or "" for a message that follows it. A
// boot above the position that is not the next sequence is ReadReasonBoot. A boot at or below the
// position is a replay of one the position already holds when its run is the position's run - the
// run of the newest message applied or held, or the stateRun of the daemon that served the last
// good whole read (recordWhole) - and otherwise a daemon whose sequence went back (a database
// restore), which is ReadReasonBoot too. A message whose first sequence is past the next one is
// ReadReasonGap: events the stream no longer holds. So is a message above the held parts that does
// not continue them (stateMessage.continues): a part of another run or another store, or one that
// overlaps them, after which the transaction they began cannot be completed.
func (m stateMessage) unfollowed(current cursor, held []part) string {
	last, run := standsAt(current, held)
	if last == nil {
		return ""
	}
	for _, event := range m.Events {
		if event.Kind != eventBoot {
			continue
		}
		switch {
		case event.Seq > *last && event.Seq != *last+1:
			return ReadReasonBoot
		case event.Seq <= *last && (run == nil || *run != m.Run):
			return ReadReasonBoot
		}
	}
	if m.FirstSeq > *last+1 {
		return ReadReasonGap
	}
	if n := len(held); n > 0 && m.Seq > *last && !m.continues(held[n-1]) {
		return ReadReasonGap
	}
	return ""
}

// boots reports whether the message carries a boot event: its daemon run started (judge reads a
// boot of a store the cursor drops as a sign that the setting's daemon may now count in it).
func (m stateMessage) boots() bool {
	for _, event := range m.Events {
		if event.Kind == eventBoot {
			return true
		}
	}
	return false
}

// movesSchema reports whether the message carries a boot above the position current and the parts
// held for it stand at (standsAt) whose schemaVersion is not the one the cursor holds, where the
// cursor holds one and the position holds a sequence: the boot moved the store's schema, and a
// migration may have rewritten what Dispatch observes outside the writes that publish events
// (ReadReasonSchema). judge asks it only of a message that follows the position (unfollowed). A
// boot that follows with the cursor's schema version moves nothing, whatever run the cursor holds.
func (m stateMessage) movesSchema(current cursor, held []part) bool {
	last, _ := standsAt(current, held)
	if last == nil || current.schemaVersion == nil {
		return false
	}
	for _, event := range m.Events {
		if event.Kind == eventBoot && event.Seq > *last && event.SchemaVersion != *current.schemaVersion {
			return true
		}
	}
	return false
}
