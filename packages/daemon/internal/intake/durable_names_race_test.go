package intake

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/consumernames"
)

// The daemon's race regressions that need no hold point: each leaves the daemon's database and the
// stream as a race or a kill leaves them, boots the daemon, and checks what the next boot does. On
// main's code each fails (durable_names_mode_test.go says how that run is made): main creates a
// missing consumer at the next message, or at the stored time, under the fixed name.

// openIntake boots the daemon's intake once and fails t when it refuses.
func openIntake(t *testing.T, js jetstream.JetStream, pool *pgxpool.Pool, spec ConsumerSpec) *Consumers {
	t.Helper()
	consumers, err := OpenConsumers(context.Background(), js, pool, spec)
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	return consumers
}

// Family 2, a native first create interrupted: the first boot of a project reserved its wake
// consumer's create at the stream's end and was killed before the create reached the server. A
// message published after that reservation is delivered by the consumer the next boot creates:
// Step 0 retires the candidate and creates a fresh one by start sequence at the saved boundary,
// never at the next message again. On main the next boot creates at the next message, and the
// message is never delivered.
func TestAFirstCreateLostBeforeItsPublishIsCreatedAgainAtItsBoundary(t *testing.T) {
	pool := migratedPool(t)
	cutover(t, pool, consumernames.CutoverDone)
	js, stream, _ := freshJetStream(t)
	logs := &lockedBuffer{}
	spec := freshSpec(logs)
	wake := wakeConsumerName(spec.Project)

	before := publishWakes(t, js, 2)
	interruptedFirstCreate(t, pool, wake, before[len(before)-1]+1)
	after := publishWakes(t, js, 2)

	openIntake(t, js, pool, spec)
	if got := fetchSeqs(t, stream, currentName(t, pool, wake)); !slices.Equal(got, after) {
		t.Fatalf("the wake consumer delivered %v, want %v: the messages published after the interrupted reservation", got, after)
	}
}

// Family 4, a floor of 0: the wake consumer delivered nothing, its floor 0 recorded at the second
// boot, and was then deleted outside the daemon with a message pending. The next boot creates it
// again at that floor + 1, and the pending message arrives. On main the next boot creates the
// fixed name at the next message, as a first create does, and the message is never delivered.
func TestAMissingCurrentStartsAtItsOwnFloorOfZero(t *testing.T) {
	pool := migratedPool(t)
	cutover(t, pool, consumernames.CutoverDone)
	js, stream, _ := freshJetStream(t)
	spec := freshSpec(&lockedBuffer{})
	wake := wakeConsumerName(spec.Project)

	openIntake(t, js, pool, spec)
	pending := publishWakes(t, js, 1)
	openIntake(t, js, pool, spec)
	if err := stream.DeleteConsumer(context.Background(), currentName(t, pool, wake)); err != nil {
		t.Fatalf("delete the wake consumer outside the daemon: %v", err)
	}
	openIntake(t, js, pool, spec)
	if got := fetchSeqs(t, stream, currentName(t, pool, wake)); !slices.Equal(got, pending) {
		t.Fatalf("the wake consumer made again delivered %v, want the message pending when it went, %v", got, pending)
	}
}

// Family 4, no safe start: a record that selects a consumer the server does not hold, and proves
// no start for it (no saved start, no last floor of it), stops the boot with the ERROR line, and no
// consumer of that logical name is created: never at the next message, never by time. On main the
// boot creates the missing consumer at the next message.
func TestAMissingCurrentWithNoSafeStartIsRefused(t *testing.T) {
	pool := migratedPool(t)
	cutover(t, pool, consumernames.CutoverDone)
	js, stream, _ := freshJetStream(t)
	logs := &lockedBuffer{}
	spec := freshSpec(logs)
	wake := wakeConsumerName(spec.Project)
	selectedAndLost(t, pool, wake)
	calls := watchConsumerCalls(t, js.Conn())

	_, err := OpenConsumers(context.Background(), js, pool, spec)
	if !errors.Is(err, consumernames.ErrNoSafeStart) {
		t.Fatalf("OpenConsumers = %v, want the refusal for no safe start", err)
	}
	for _, name := range calls.created(t) {
		if consumernames.LogicalOf(name) == wake {
			t.Fatalf("the boot created %s, a consumer of %s, whose record proves no start", name, wake)
		}
	}
	for _, name := range consumerNames(t, stream) {
		if consumernames.LogicalOf(name) == wake {
			t.Fatalf("the stream holds %s, a consumer of %s, whose record proves no start", name, wake)
		}
	}
	oneErrorLine(t, logs, wake, consumernames.OperationMissingCurrent)
}

// Family 6, a checkpoint with no selection record: the daemon applied facts on its Dispatch
// consumer (consumer_positions keeps the logical name's position), and its selection record is
// not there. The boot stops with the ERROR line before it creates anything, and writes no record:
// a lost record is never taken for a new project. On main the boot creates the consumer at the
// stored time.
func TestACheckpointWithNoSelectionRecordIsRefused(t *testing.T) {
	pool := migratedPool(t)
	cutover(t, pool, consumernames.CutoverDone)
	js, stream, _ := freshJetStream(t)
	logs := &lockedBuffer{}
	spec := freshSpec(logs)
	dispatch := dispatchConsumerName(spec.Project)
	if _, err := pool.Exec(context.Background(), `insert into consumer_positions (consumer, stream_seq, stream_time) values ($1, 7, $2)`,
		dispatch, time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("keep the Dispatch consumer's checkpoint: %v", err)
	}
	calls := watchConsumerCalls(t, js.Conn())

	if consumers, err := OpenConsumers(context.Background(), js, pool, spec); err == nil {
		t.Fatalf("OpenConsumers opened %v over a checkpoint with no selection record, want a refusal", consumers)
	}
	if created := calls.created(t); len(created) != 0 {
		t.Fatalf("the refused boot created %v, want nothing", created)
	}
	if names := consumerNames(t, stream); len(names) != 0 {
		t.Fatalf("the stream holds %v after the refused boot, want no consumer", names)
	}
	oneErrorLine(t, logs, dispatch, consumernames.OperationSelect)
}
