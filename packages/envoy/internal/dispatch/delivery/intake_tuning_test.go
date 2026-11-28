package delivery

import (
	"log/slog"
	"reflect"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock"
)

// Production's tuning is the values the intakes ran by when each was a package variable a test
// overwrote, and StartIntakes builds both intakes with it: a test now shortens a timer, hooks a
// step or reads the lines of its own intakes alone (tuningFor), so these are the values no test
// reads and every deployment runs by. The row pins each, that no hook is set, that the intakes log
// to the default logger their lines went to before they had a logger of their own, and that each
// call makes a tuning of its own. It also pins the clocks: the tuning's, which startIntakes gives
// both intakes for their own deadlines, and those an intake is built with (newHeldDurable), each
// the real one, for its own deadlines (heldDurable.clock) and for its hold (heldDurable.holdClock),
// which a test replaces with a fake.
func TestProductionsIntakeTuningIsTheShippedTimersAndNoHook(t *testing.T) {
	t.Parallel()
	tuning := newIntakeTuning()
	held := newHeldDurable(tuning, nil, nil, deliveryConsumerConfig, intakeKey, func(*intakeDelivery, intakeMessage) {})
	for _, field := range []struct {
		name      string
		got, want any
	}{
		{"messageTimeout", tuning.messageTimeout, time.Minute},
		{"maxAckPending", tuning.maxAckPending, 4},
		{"ackLanding", tuning.ackLanding, time.Minute},
		{"stopBudget", tuning.stopBudget, 15 * time.Second},
		{"clock", any(tuning.clock), any(clock.Real())},
		{"headWriteMaxDeliveries", tuning.headWriteMaxDeliveries, uint64(30)},
		{"headWriteRedeliverAfter", tuning.headWriteRedeliverAfter, 10 * time.Second},
		{"callWait", time.Duration(tuning.callWait.Load()), bus.ConnLockWait},
		{"drainWait", time.Duration(tuning.drainWait.Load()), natsgo.DefaultDrainTimeout},
		{"ackWait", tuning.ackWait(), 5 * time.Minute},
		{"an intake's clock", any(held.clock), any(clock.Real())},
		{"an intake's holdClock", any(held.holdClock), any(clock.Real())},
	} {
		if field.got != field.want {
			t.Errorf("production's %s = %v, want %v", field.name, field.got, field.want)
		}
	}
	if reflect.ValueOf(tuning.createDurable).Pointer() != reflect.ValueOf(addDurable).Pointer() {
		t.Error("production's createDurable is not addDurable, the create that sends the durable to the stream")
	}
	for name, set := range map[string]bool{
		"stopArmed":        tuning.stopArmed.Load() != nil,
		"beforeRelease":    tuning.beforeRelease.Load() != nil,
		"attempted":        tuning.attempted.Load() != nil,
		"checked":          tuning.checked.Load() != nil,
		"awaitingSettings": tuning.awaitingSettings.Load() != nil,
		"settingsWaiting":  tuning.settingsWaiting.Load() != nil,
	} {
		if set {
			t.Errorf("production's tuning carries a test's %s hook", name)
		}
	}
	if tuning.log != slog.Default() {
		t.Error("production's intakes do not log to the default logger")
	}
	if newIntakeTuning() == tuning {
		t.Error("newIntakeTuning returned the same tuning twice, so two intakes would share one")
	}
}

// A subtest's intakes run by what its parent test set on its tuning (tuningFor, which starts a
// subtest from a copy of its nearest ancestor's, copyTuning). The row sets every field of a
// parent's tuning to a value production's does not hold, and reads each back from a copy and from
// a subtest's tuning. It names every field intakeTuning has, and fails when the struct has one it
// does not name, so a field added to the tuning is added to copyTuning and to this row with it.
func TestACopiedTuningHoldsEveryFieldOfItsParent(t *testing.T) {
	t.Parallel()
	parent := tuningFor(t)
	logger := slog.New(slog.DiscardHandler)
	createDurable := func(natsgo.JetStreamContext, *natsgo.ConsumerConfig, ...natsgo.JSOpt) (*natsgo.ConsumerInfo, error) {
		return nil, nil
	}
	stopArmed, beforeRelease, attempted, checked, awaitingSettings := func() {}, func() {}, func() {}, func() {}, func() {}
	settingsWaiting := func(bool) {}
	fake := fakeIntakeClock()
	parent.messageTimeout = 7 * time.Second
	parent.maxAckPending = 9
	parent.ackLanding = 23 * time.Second
	parent.stopBudget = 11 * time.Second
	parent.clock = fake
	parent.headWriteMaxDeliveries = 13
	parent.headWriteRedeliverAfter = 29 * time.Second
	parent.createDurable = createDurable
	parent.log = logger
	parent.callWait.Store(int64(17 * time.Millisecond))
	parent.drainWait.Store(int64(19 * time.Millisecond))
	parent.stopArmed.Store(&stopArmed)
	parent.beforeRelease.Store(&beforeRelease)
	parent.attempted.Store(&attempted)
	parent.checked.Store(&checked)
	parent.awaitingSettings.Store(&awaitingSettings)
	parent.settingsWaiting.Store(&settingsWaiting)

	holds := func(t *testing.T, whose string, got *intakeTuning) {
		t.Helper()
		fields := []struct {
			name      string
			got, want any
		}{
			{"messageTimeout", got.messageTimeout, 7 * time.Second},
			{"maxAckPending", got.maxAckPending, 9},
			{"ackLanding", got.ackLanding, 23 * time.Second},
			{"stopBudget", got.stopBudget, 11 * time.Second},
			{"clock", any(got.clock), any(fake)},
			{"headWriteMaxDeliveries", got.headWriteMaxDeliveries, uint64(13)},
			{"headWriteRedeliverAfter", got.headWriteRedeliverAfter, 29 * time.Second},
			{"createDurable", reflect.ValueOf(got.createDurable).Pointer(), reflect.ValueOf(createDurable).Pointer()},
			{"log", got.log, logger},
			{"callWait", time.Duration(got.callWait.Load()), 17 * time.Millisecond},
			{"drainWait", time.Duration(got.drainWait.Load()), 19 * time.Millisecond},
			{"stopArmed", got.stopArmed.Load(), &stopArmed},
			{"beforeRelease", got.beforeRelease.Load(), &beforeRelease},
			{"attempted", got.attempted.Load(), &attempted},
			{"checked", got.checked.Load(), &checked},
			{"awaitingSettings", got.awaitingSettings.Load(), &awaitingSettings},
			{"settingsWaiting", got.settingsWaiting.Load(), &settingsWaiting},
		}
		if all := reflect.TypeFor[intakeTuning]().NumField(); all != len(fields) {
			t.Fatalf("intakeTuning has %d fields and the row reads %d: name the new one here and copy it in copyTuning", all, len(fields))
		}
		for _, field := range fields {
			if field.got != field.want {
				t.Errorf("%s %s = %v, want the parent's %v", whose, field.name, field.got, field.want)
			}
		}
	}
	holds(t, "a copy's", copyTuning(parent))
	t.Run("a subtest", func(t *testing.T) {
		holds(t, "a subtest's", tuningFor(t))
	})
}
