package delivery

import (
	"log/slog"
	"sync/atomic"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
)

// intakeTuning is what one intake times its work by, the logger it writes its lines to, and the
// steps a test reaches into it at: newIntakeTuning gives production's values, which StartIntakes
// builds both intakes with. Each intake holds its tuning (heldDurable.tuning) and every delivery
// it makes holds the same one (intakeDelivery.tuning), so a test that shortens a timer, hooks a
// step or reads the lines of its own intakes runs beside tests that do not: no test changes a
// value another test's intake reads. The durations, createDurable and log are read once the
// intake runs, so a test sets them before it starts its intakes; the waits and hooks a test
// changes while its intakes run are atomic.
type intakeTuning struct {
	// messageTimeout bounds one handler's own work (Intake.deliver) and one job read
	// (JobIntake.fire): intakeMessageTimeout.
	messageTimeout time.Duration
	// maxAckPending is the durable's MaxAckPending: intakeMaxAckPending. Together with
	// messageTimeout it sets the durable's ack wait (ackWait).
	maxAckPending int
	// stopBudget is the one deadline of the intake's stop, counted from the moment Run's context
	// ends (stopContext): intakeStopBudget.
	stopBudget time.Duration
	// headWriteMaxDeliveries is how many deliveries an envelope whose head row could not be stored
	// gets before deliver acknowledges it (headWriteDeliveriesSpent): headWriteMaxDeliveries.
	headWriteMaxDeliveries uint64
	// createDurable creates the durable a take-over configured on the notification stream
	// (heldDurable.finishTakeOver): addDurable, unless a test fails the create after its delete.
	createDurable func(js natsgo.JetStreamContext, config *natsgo.ConsumerConfig, opts ...natsgo.JSOpt) (*natsgo.ConsumerInfo, error)
	// callWait is how long callNATS waits for a call's answer, in nanoseconds: bus.ConnLockWait.
	// A test sets it to 0, so the timer is ready at once and a call may outlive the wait, to show
	// the bookkeeping inside each call completes either way.
	callWait atomic.Int64
	// drainWait is how long the stop waits for its subscription's drain once the last handler
	// returned, in nanoseconds (intakeDelivery.stop): nats.go's DefaultDrainTimeout, the bound
	// nats.go itself gives a connection's drain (bus connections never set their own), rather than
	// messageTimeout, which bounds a handler, and the handlers are done by then. All that is left
	// is nats.go's: the callback handing back what the client had buffered, which never blocks, and
	// the drain's flush to the server. The stop's context ending cuts it shorter. A test raises it
	// to keep it from ending the wait and see what else does.
	drainWait atomic.Int64
	// stopArmed, when set, runs in the stop once its give-up is armed (intakeDelivery.stop), where
	// a test waits for the handler the give-up wakes to decide its hand-back. Nil outside tests.
	stopArmed atomic.Pointer[func()]
	// beforeRelease, when set, runs as a finalizer finds every hand-back of its delivery settled,
	// before it releases the hold (heldDurable.finalizeHandBacks), so a test sees that
	// stopIntakes waits for it. Nil outside tests.
	beforeRelease atomic.Pointer[func()]
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
		messageTimeout:         intakeMessageTimeout,
		maxAckPending:          intakeMaxAckPending,
		stopBudget:             intakeStopBudget,
		headWriteMaxDeliveries: headWriteMaxDeliveries,
		createDurable:          addDurable,
		log:                    slog.Default(),
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
