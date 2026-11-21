//go:build odatrace

package intake

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/testnatsinprocess"

	"github.com/sjawhar/legion/daemon/internal/testnats"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The daemon's race regressions on the hooked nats-server (packages/envoy/scripts/forced-order.sh
// -C packages/daemon ./internal/intake/), on the in-process server testnats.StartInProcess runs.
// Each holds the server at one point and so forces an order the server's scheduling only sometimes
// produces, then checks what the daemon's next boot makes of it. Each runs its control first: the
// same order on fixed names, where it loses messages or fails the boot, so the hold is shown to
// reach what it is meant to. On main's code the treatment is that control, and fails.

// collisionOrder is one order of family 1 at the daemon's creation site, a missing current made
// again: the predecessor's state write is held at predecessor, the predecessor is deleted, and the
// daemon's next boot creates the replacement; when replacement is set, the replacement's own store
// is held there too until the predecessor's write has been released and has finished. collides
// says whether the order hands the fixed name made again the predecessor's state.
type collisionOrder struct {
	name        string
	predecessor testnatsinprocess.Point
	replacement testnatsinprocess.Point
	collides    bool
}

var collisionOrders = []collisionOrder{
	{name: "the predecessor's write held before its IO, released after the replacement's create", predecessor: testnatsinprocess.WritePreIO, collides: true},
	// Held before its rename, the predecessor's write has its o.dat.tmp in the directory the delete
	// removes, so its late rename fails and lands nothing, whichever name the replacement has: the
	// order is a compatibility row here, both names delivering from their own start.
	{name: "the predecessor's write held before its rename, released after the replacement's create", predecessor: testnatsinprocess.WritePreRename},
	{name: "the predecessor's write released before the replacement loads its state", predecessor: testnatsinprocess.WritePreIO, replacement: testnatsinprocess.StoreCreate, collides: true},
	{name: "the create-error order: the predecessor's write released while the replacement's waits to rename", predecessor: testnatsinprocess.WritePreIO, replacement: testnatsinprocess.WritePreRename, collides: true},
}

// collision is what one run of an order ended with: the third boot's error, and, when it opened,
// the stream sequences the wake consumer it selected delivers after a restart of the server.
type collision struct {
	boot      error
	delivered []uint64
}

// writeDone is the hooked server's report of a consumer state write that has finished.
const writeDone testnatsinprocess.Point = "write.done"

// ackFloorOf takes the events carrying a consumer state whose ack floor is floor.
func ackFloorOf(floor uint64) testnatsinprocess.Match {
	return testnatsinprocess.Where(fmt.Sprintf("ack floor %d", floor), func(e testnatsinprocess.Event) bool {
		return e.State != nil && e.State.AckFloor.Stream == floor
	})
}

// generation takes the events of the consumer file store of generation gen.
func generation(gen uint64) testnatsinprocess.Match {
	return testnatsinprocess.Where(fmt.Sprintf("store generation %d", gen), func(e testnatsinprocess.Event) bool { return e.Gen == gen })
}

// inProcessJetStream is a client of the in-process server at url, named for t and closed when t
// ends. It is no client of the package's shared server, so the shared server's leak check does
// not hold it.
func inProcessJetStream(t *testing.T, url string) jetstream.JetStream {
	t.Helper()
	conn, err := nats.Connect(url, nats.Name(t.Name()), nats.MaxReconnects(-1), nats.ReconnectWait(50*time.Millisecond))
	if err != nil {
		t.Fatalf("connect to the in-process server: %v", err)
	}
	t.Cleanup(conn.Close)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("JetStream on the in-process server: %v", err)
	}
	return js
}

// ackThrough fetches name's next messages up to stream sequence through, acknowledging each and
// waiting for the server to take each acknowledgement.
func ackThrough(t *testing.T, stream jetstream.Stream, name string, through uint64) {
	t.Helper()
	consumer, err := stream.Consumer(context.Background(), name)
	if err != nil {
		t.Fatalf("bind %s: %v", name, err)
	}
	for {
		batch, err := consumer.Fetch(1, jetstream.FetchMaxWait(5*time.Second))
		if err != nil {
			t.Fatalf("fetch from %s: %v", name, err)
		}
		var last uint64
		for msg := range batch.Messages() {
			metadata, err := msg.Metadata()
			if err != nil {
				t.Fatalf("read a message's metadata: %v", err)
			}
			if err := msg.DoubleAck(context.Background()); err != nil {
				t.Fatalf("ack stream sequence %d: %v", metadata.Sequence.Stream, err)
			}
			last = metadata.Sequence.Stream
		}
		if err := batch.Error(); err != nil {
			t.Fatalf("fetch from %s: %v", name, err)
		}
		if last == 0 {
			t.Fatalf("%s delivered nothing before stream sequence %d", name, through)
		}
		if last >= through {
			return
		}
	}
}

// collide runs order with the daemon on spec, fresh saying whether spec names fresh names: the
// wake consumer A of a project acknowledges 1 to 20 of 30 messages, with the daemon's checkpoint
// at 20, as the daemon leaves it after applying message 20; the daemon boots again; A's state write
// carrying floor 21 is held at order.predecessor; A is deleted outside the daemon; the daemon boots
// a third time, which makes the wake consumer again, A's write is released and finishes, and the
// server restarts on its store.
func collide(t *testing.T, order collisionOrder, spec ConsumerSpec, fresh bool) collision {
	t.Helper()
	for _, point := range []testnatsinprocess.Point{order.predecessor, order.replacement} {
		if point == "" {
			continue
		}
		if ok, why := point.Exists(); !ok {
			t.Skipf("the linked nats-server has no %s: %s", point, why)
		}
	}
	server, url := testnats.StartInProcess(t)
	hooks := testnatsinprocess.Hook(t)
	js := inProcessJetStream(t, url)
	stream, _ := identifiedStream(t, js)
	pool := migratedPool(t)
	cutover(t, pool, consumernames.CutoverDone)
	wake := wakeConsumerName(spec.Project)
	current := func(pool *pgxpool.Pool) string {
		if fresh {
			return currentName(t, pool, wake)
		}
		return wake
	}
	ctx := context.Background()

	openIntake(t, js, pool, spec)
	publishWakes(t, js, 30)
	predecessor := current(pool)
	ackThrough(t, stream, predecessor, 20)
	applied, err := stream.GetMsg(ctx, 20)
	if err != nil {
		t.Fatalf("read message 20: %v", err)
	}
	if _, err := pool.Exec(ctx, `insert into consumer_positions (consumer, stream_seq, stream_time) values ($1, 20, $2)`,
		wake, applied.Time.Truncate(time.Microsecond)); err != nil {
		t.Fatalf("keep the checkpoint of message 20: %v", err)
	}
	openIntake(t, js, pool, spec)

	held := hooks.Hold(order.predecessor, testnatsinprocess.OnServer(server), testnatsinprocess.Consumer(predecessor), ackFloorOf(21))
	ackThrough(t, stream, predecessor, 21)
	parked := held.Reached(t)
	t.Logf("held %s", parked)
	if err := stream.DeleteConsumer(ctx, predecessor); err != nil {
		t.Fatalf("delete %s outside the daemon: %v", predecessor, err)
	}
	var replacement *testnatsinprocess.Hold
	if order.replacement != "" {
		replacement = hooks.Hold(order.replacement, testnatsinprocess.OnServer(server),
			testnatsinprocess.Where("a later store of "+wake, func(e testnatsinprocess.Event) bool {
				return consumernames.LogicalOf(e.Consumer) == wake && e.Gen > parked.Gen
			}))
	}
	mark := hooks.Mark()
	booted := make(chan error, 1)
	go func() {
		boot, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		_, err := OpenConsumers(boot, js, pool, spec)
		booted <- err
	}()
	var result collision
	if replacement != nil {
		t.Logf("held %s", replacement.Reached(t))
		held.Release()
		t.Logf("finished %s", hooks.Await(t, writeDone, testnatsinprocess.Consumer(predecessor), generation(parked.Gen), testnatsinprocess.After(mark)))
		replacement.Release()
		result.boot = <-booted
	} else {
		result.boot = <-booted
		held.Release()
		t.Logf("finished %s", hooks.Await(t, writeDone, testnatsinprocess.Consumer(predecessor), generation(parked.Gen), testnatsinprocess.After(mark)))
	}
	if result.boot != nil {
		return result
	}
	selected := current(pool)
	server.Stop(t)
	server.Start(t)
	restarted := inProcessJetStream(t, server.URL())
	after, err := restarted.Stream(ctx, notificationStream)
	if err != nil {
		t.Fatalf("open %s after the restart: %v", notificationStream, err)
	}
	result.delivered = fetchSeqs(t, after, selected)
	return result
}

// Family 1 at the daemon's missing-current site, in each order: on fixed names the predecessor's
// late state write lands in the state of the consumer made again under its name, so after the
// restart it delivers from past the predecessor's floor and loses the messages it was created to
// deliver (the control, M4), or, in the create-error order, the create fails and the boot with it.
// On fresh names the consumer made again is another name, created at the predecessor's last floor
// + 1, and after the restart every message from there arrives.
func TestForcedOrderAMissingCurrentIsMadeAgainOutOfReachOfItsPredecessorsLateWrite(t *testing.T) {
	for _, order := range collisionOrders {
		t.Run(order.name, func(t *testing.T) {
			t.Run("control: the fixed name made again", func(t *testing.T) {
				spec := consumerSpec(&lockedBuffer{})
				got := collide(t, order, spec, false)
				switch {
				case order.replacement == testnatsinprocess.WritePreRename:
					if got.boot == nil || !strings.Contains(got.boot.Error(), "10012") {
						t.Fatalf("the boot in the create-error order = %v, delivered %v; want the create refused with 10012", got.boot, got.delivered)
					}
				case got.boot != nil:
					t.Fatalf("the third boot on fixed names: %v", got.boot)
				case order.collides && !slices.Equal(got.delivered, seqRange(22, 30)):
					t.Fatalf("the fixed name made again at message 20 delivered %v after the restart, want 22 to 30: the predecessor's floor 21 in its state", got.delivered)
				case !order.collides && !slices.Equal(got.delivered, seqRange(20, 30)):
					t.Fatalf("the fixed name made again at message 20 delivered %v after the restart, want 20 to 30: the predecessor's late rename lands nothing", got.delivered)
				}
			})
			t.Run("treatment: a fresh name made again", func(t *testing.T) {
				got := collide(t, order, freshSpec(&lockedBuffer{}), true)
				if got.boot != nil {
					t.Fatalf("the third boot: %v, want the wake consumer made again", got.boot)
				}
				if want := seqRange(21, 30); !slices.Equal(got.delivered, want) {
					t.Fatalf("the wake consumer made again delivered %v after the restart, want %v: every message from the predecessor's last floor + 1", got.delivered, want)
				}
			})
		})
	}
}

// Family 2, a paused creator: a boot's create of its wake consumer's fresh name is held before the
// server serves it, so the boot never hears an answer and stops with the operation open. The next
// boot finds the candidate nowhere, retires it once its reservation is two seconds old, and creates
// another; the held create is then served and the candidate lands, late. A third boot keeps the
// second boot's consumer, the candidate is never selected, no name is created twice, and the
// operator's status lists the candidate as retired and cleanup deletes it. On main the hold is
// never reached: main creates the fixed name, which is no fresh name.
func TestForcedOrderACreateNobodyAnsweredIsRetiredAndNeverSelected(t *testing.T) {
	server, url := testnats.StartInProcess(t)
	hooks := testnatsinprocess.Hook(t)
	js := inProcessJetStream(t, url)
	stream, _ := identifiedStream(t, js)
	pool := migratedPool(t)
	cutover(t, pool, consumernames.CutoverDone)
	logs := &lockedBuffer{}
	spec := freshSpec(logs)
	wake := wakeConsumerName(spec.Project)
	calls := watchConsumerCalls(t, js.Conn())
	createPrefix := "$JS.API.CONSUMER.CREATE." + notificationStream + "." + wake + "~"

	held := hooks.Hold(testnatsinprocess.APIWorker, testnatsinprocess.OnServer(server),
		testnatsinprocess.Where("the create of a fresh name of "+wake, func(e testnatsinprocess.Event) bool { return strings.HasPrefix(e.Subject, createPrefix) }))
	paused, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() {
		_, err := OpenConsumers(paused, js, pool, spec)
		stopped <- err
	}()
	parked := held.Reached(t)
	cancel()
	if err := <-stopped; err == nil {
		t.Fatal("the boot whose create was held opened, want it stopped with no answer")
	} else {
		t.Logf("the paused boot stopped: %v", err)
	}
	candidate := strings.Split(strings.TrimPrefix(parked.Subject, "$JS.API.CONSUMER.CREATE."+notificationStream+"."), ".")[0]
	if op := selectionOf(t, pool, wake).Op; op == nil || op.Kind != consumernames.OpCreate || op.Candidate != candidate {
		t.Fatalf("the stopped boot left %+v, want the create of %s open", op, candidate)
	}

	openIntake(t, js, pool, spec)
	settled := selectionOf(t, pool, wake)
	if settled.Current == candidate || !isFresh(wake, settled.Current) || !slices.Contains(settled.Retired, candidate) {
		t.Fatalf("the next boot left %+v, want %s retired and another fresh name selected", settled, candidate)
	}
	held.Release()
	testwait.Eventually(t, "the held create of "+candidate+" served", func() bool {
		_, err := stream.Consumer(context.Background(), candidate)
		return err == nil
	})

	openIntake(t, js, pool, spec)
	if again := selectionOf(t, pool, wake).Current; again != settled.Current {
		t.Fatalf("the third boot selected %s, want %s kept: the late candidate is never selected", again, settled.Current)
	}
	calls.noNameCreatedTwice(t, spec.Project)

	listed, err := consumernames.NewPGStore(pool).List(context.Background())
	if err != nil {
		t.Fatalf("status: list the daemon's records: %v", err)
	}
	if !slices.Contains(listed[wake].Retired, candidate) {
		t.Fatalf("status lists %s as %+v, want %s retired", wake, listed[wake], candidate)
	}
	cleanup := consumernames.New(consumernames.Config{
		Store: consumernames.NewPGStore(pool), Server: operatorServer{stream},
		Prefix: func(logical string) string { return logical }, Owner: "the operator's cleanup",
		Floor:    func(info *jetstream.ConsumerInfo) uint64 { return info.AckFloor.Stream },
		StreamID: func(context.Context) (string, error) { return "", nil },
	})
	if deleted, err := cleanup.Cleanup(context.Background(), wake); err != nil || deleted != 1 {
		t.Fatalf("cleanup = %d, %v; want the late candidate deleted", deleted, err)
	}
	if _, err := stream.Consumer(context.Background(), candidate); !errors.Is(err, jetstream.ErrConsumerNotFound) {
		t.Fatalf("%s after cleanup: %v, want it gone", candidate, err)
	}
}
