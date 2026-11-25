package delivery

import (
	"log/slog"
	"reflect"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
)

// Production's tuning is the values the intakes ran by when each was a package variable a test
// overwrote, and StartIntakes builds both intakes with it: a test now shortens a timer, hooks a
// step or reads the lines of its own intakes alone (tuningFor), so these are the values no test
// reads and every deployment runs by. The row pins each, that no hook is set, that the intakes log
// to the default logger their lines went to before they had a logger of their own, and that each
// call makes a tuning of its own.
func TestProductionsIntakeTuningIsTheShippedTimersAndNoHook(t *testing.T) {
	t.Parallel()
	tuning := newIntakeTuning()
	for _, field := range []struct {
		name      string
		got, want any
	}{
		{"messageTimeout", tuning.messageTimeout, time.Minute},
		{"maxAckPending", tuning.maxAckPending, 4},
		{"stopBudget", tuning.stopBudget, 15 * time.Second},
		{"headWriteMaxDeliveries", tuning.headWriteMaxDeliveries, uint64(30)},
		{"callWait", time.Duration(tuning.callWait.Load()), bus.ConnLockWait},
		{"drainWait", time.Duration(tuning.drainWait.Load()), natsgo.DefaultDrainTimeout},
		{"ackWait", tuning.ackWait(), 5 * time.Minute},
	} {
		if field.got != field.want {
			t.Errorf("production's %s = %v, want %v", field.name, field.got, field.want)
		}
	}
	if reflect.ValueOf(tuning.createDurable).Pointer() != reflect.ValueOf(addDurable).Pointer() {
		t.Error("production's createDurable is not addDurable, the create that sends the durable to the stream")
	}
	if tuning.stopArmed.Load() != nil || tuning.beforeRelease.Load() != nil {
		t.Error("production's tuning carries a test's hook")
	}
	if tuning.log != slog.Default() {
		t.Error("production's intakes do not log to the default logger")
	}
	if newIntakeTuning() == tuning {
		t.Error("newIntakeTuning returned the same tuning twice, so two intakes would share one")
	}
}
