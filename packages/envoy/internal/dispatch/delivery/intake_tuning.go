package delivery

import (
	"log/slog"
	"sync/atomic"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock"
)

// intakeTuning is what one intake times its work by, the logger it writes its lines to, and the
// steps a test reaches into it at: newIntakeTuning gives production's values, which StartIntakes
// builds both intakes with. Each intake holds its tuning (heldDurable.tuning) and every delivery
// it makes holds the same one (intakeDelivery.tuning), so a test that shortens a timer, hooks a
// step or reads the lines of its own intakes runs beside tests that do not: no test changes a
// value another test's intake reads. The durations, the clock, createDurable and log are read once the
// intake runs, so a test sets them before it starts its intakes; the waits and hooks a test
// changes while its intakes run are atomic. The clock startIntakes gives both intakes for their own
// deadlines and waits is the tuning's (clock); the hold's clock is the intake's own
// (heldDurable.holdClock), beside it. A test of this package moves a fake past such a span rather
// than shortening it.
type intakeTuning struct {
	// messageTimeout bounds one handler's own work (Intake.deliver) and one job read
	// (JobIntake.fire) on the intake's clock (heldDurable.clock): intakeMessageTimeout.
	messageTimeout time.Duration
	// maxAckPending is the durable's MaxAckPending: intakeMaxAckPending. Together with
	// messageTimeout and ackLanding it sets the durable's ack wait (ackWait).
	maxAckPending int
	// ackLanding is what the durable's ack wait leaves an acknowledgement to land in once every
	// message outstanding has had its messageTimeout (ackWait): intakeAckLanding. It is the
	// server's own span, which no clock of this module reaches, so a row that counts each message
	// handled once and needs the server's redelivery inside its run sets it, with messageTimeout,
	// to an ack wait above one handling and its acknowledgement's trip.
	ackLanding time.Duration
	// stopBudget is the one deadline of the intake's stop, counted on the intake's clock from the
	// moment Run's context ends (stopContext): intakeStopBudget.
	stopBudget time.Duration
	// clock is the time startIntakes gives both intakes for their own deadlines and waits
	// (heldDurable.clock): clock.Real, unless a test sets a clocktest.Fake it moves before it starts
	// its intakes.
	clock intakeClock
	// headWriteMaxDeliveries is how many deliveries an envelope whose head row could not be stored
	// gets before deliver acknowledges it (headWriteDeliveriesSpent): headWriteMaxDeliveries.
	headWriteMaxDeliveries uint64
	// headWriteRedeliverAfter is how long the server waits before it delivers again an envelope
	// whose head row could not be stored, which deliver hands back with that delay
	// (intakeDelivery.handBackOwn): headWriteRedeliverAfter. It is the server's own span (a delayed
	// negative acknowledgement), which no clock of this module reaches, so a row that waits for the
	// hand-back's redelivery sets it short, where no verdict of the row reads its length.
	headWriteRedeliverAfter time.Duration
	// createDurable creates the durable a take-over configured on the notification stream
	// (heldDurable.finishTakeOver): addDurable, unless a test fails the create after its delete.
	createDurable func(js natsgo.JetStreamContext, config *natsgo.ConsumerConfig, opts ...natsgo.JSOpt) (*natsgo.ConsumerInfo, error)
	// callWait is how long callNATS waits for a call's answer, in nanoseconds: bus.ConnLockWait.
	// A test sets it to 0, so the timer is ready at once and a call may outlive the wait, to show
	// the bookkeeping inside each call completes either way.
	callWait atomic.Int64
	// drainWait is how long the stop waits for its subscription's drain once the last handler
	// returned, in nanoseconds (intakeDelivery.stop), read on the intake's clock
	// (intakeDelivery.clock): nats.go's DefaultDrainTimeout, the bound nats.go itself gives a
	// connection's drain (bus connections never set their own), rather than messageTimeout, which
	// bounds a handler, and the handlers are done by then. All that is left is nats.go's: the
	// callback handing back what the client had buffered, which never blocks, and the drain's flush
	// to the server. The stop's context ending cuts it shorter. A test raises it to keep it from
	// ending the wait and see what else does.
	drainWait atomic.Int64
	// stopArmed, when set, runs in the stop once its give-up is armed (intakeDelivery.stop), where
	// a test waits for the handler the give-up wakes to decide its hand-back. Nil outside tests.
	stopArmed atomic.Pointer[func()]
	// beforeRelease, when set, runs as a finalizer finds every hand-back of its delivery settled,
	// before it releases the hold (heldDurable.finalizeHandBacks), so a test sees that
	// stopIntakes waits for it. Nil outside tests.
	beforeRelease atomic.Pointer[func()]
	// attempted, when set, runs after each of the intake's tries for the hold and the durable
	// (heldDurable.attempt), so a test waits for the tries it asserts about rather than for a
	// span of the clock. Nil outside tests.
	attempted atomic.Pointer[func()]
	// checked, when set, runs after each of the intake's checks of its durable (heldDurable.run's
	// check arm, heldDurable.check), so a test waits for the checks it asserts about rather than for
	// a span of the clock. Nil outside tests.
	checked atomic.Pointer[func()]
	// awaitingSettings, when set, runs as Intake.Run begins to wait for its settings cache's first
	// read (SettingsCache.Read), so a test knows the intake is waiting for it. Nil outside tests.
	awaitingSettings atomic.Pointer[func()]
	// settingsWaiting, when set, runs each time a SettingsCache built with this tuning
	// (newSettingsCache, which startIntakes calls with it) blocks in Run's wait, told whether a
	// retry of a read that failed is armed there, so a test reads that the cache is idle and what
	// can wake it. Nil outside tests.
	settingsWaiting atomic.Pointer[func(retrying bool)]
	// log is where the intake side writes its lines: both intakes, their deliveries, callNATS,
	// and the SettingsCache startIntakes builds for them. newIntakeTuning sets the default logger
	// as it is at that moment, which is what those lines went to before, so a program that sets
	// no logger of its own writes exactly what it wrote. A test sets a logger over a buffer of its
	// own (captureIntakeLogs) and reads its own intakes' lines without the default's.
	log *slog.Logger
}

// newIntakeTuning is production's tuning, a new one for each call, logging to the default logger
// as it is when newIntakeTuning is called.
func newIntakeTuning() *intakeTuning {
	tuning := &intakeTuning{
		messageTimeout:          intakeMessageTimeout,
		maxAckPending:           intakeMaxAckPending,
		ackLanding:              intakeAckLanding,
		stopBudget:              intakeStopBudget,
		clock:                   clock.Real(),
		headWriteMaxDeliveries:  headWriteMaxDeliveries,
		headWriteRedeliverAfter: headWriteRedeliverAfter,
		createDurable:           addDurable,
		log:                     slog.Default(),
	}
	tuning.callWait.Store(int64(bus.ConnLockWait))
	tuning.drainWait.Store(int64(natsgo.DefaultDrainTimeout))
	return tuning
}

// addDurable creates the durable config describes on the notification stream: what a take-over's
// create sends outside tests (intakeTuning.createDurable).
func addDurable(js natsgo.JetStreamContext, config *natsgo.ConsumerConfig, opts ...natsgo.JSOpt) (*natsgo.ConsumerInfo, error) {
	return js.AddConsumer(bus.Stream, config, opts...)
}
