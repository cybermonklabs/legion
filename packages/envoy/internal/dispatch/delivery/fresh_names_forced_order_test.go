//go:build odatrace

package delivery

// §7 family 1 for the intakes' two durables, on the hooked nats-server (scripts/forced-order.sh):
// a held state write of the take-over's predecessor, released after its successor is created,
// with the store restarted. The build before the fresh mode creates its successor under the
// predecessor's own name, so the late write lands in the successor's file and the successor,
// once restarted, has skipped a message nobody acknowledged on it (M4). A fresh name sends the late
// write to a directory no consumer reads (M5).

import (
	"fmt"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// ackFloorAt takes the state writes whose ack floor is seq.
func ackFloorAt(seq uint64) testnats.Match {
	return testnats.Where(fmt.Sprintf("ack floor %d", seq), func(e testnats.Event) bool {
		return e.State != nil && e.State.AckFloor.Stream == seq
	})
}

// heldServer is the hub's server whose store holds the stream's one copy: the lone hub, or the
// cluster member the stream's R1 copy is placed on.
func heldServer(t *testing.T, hub *freshHub) *testnats.Server {
	t.Helper()
	if hub.cluster == nil {
		return hub.server
	}
	_, js := hub.admin(t)
	info, err := js.StreamInfo(bus.Stream)
	if err != nil || info.Cluster == nil {
		t.Fatalf("read where the stream is placed: %v", err)
	}
	member := hub.cluster.Member(info.Cluster.Leader)
	if member < 0 {
		t.Fatalf("the stream's leader %q is no member", info.Cluster.Leader)
	}
	return hub.cluster.Servers[member]
}

// restartStore stops and starts server, keeping its store, and waits for the hub to serve again.
func restartStore(t *testing.T, hub *freshHub, server *testnats.Server) {
	t.Helper()
	server.Stop(t)
	server.Start(t)
	if hub.cluster != nil {
		hub.cluster.AwaitReady(t)
	}
}

// ackThrough acknowledges, on the gone holder's subscription, each message up to stream sequence
// last, and answers the next one it is handed, unacknowledged.
func ackThrough(t *testing.T, sub *natsgo.Subscription, last uint64) *natsgo.Msg {
	t.Helper()
	for {
		msg, err := sub.NextMsg(10 * time.Second)
		if err != nil {
			t.Fatalf("the gone holder was handed nothing more: %v", err)
		}
		meta, err := msg.Metadata()
		if err != nil {
			t.Fatalf("read a delivery's metadata: %v", err)
		}
		if meta.Sequence.Stream > last {
			return msg
		}
		if err := msg.AckSync(); err != nil {
			t.Fatalf("acknowledge %d: %v", meta.Sequence.Stream, err)
		}
	}
}

// Race regression, a held write at the take-over of each durable, on one copy and an R1 cluster.
// The gone holder has acknowledged 1 to 20, and the take-over reads that floor and so starts its
// successor at 21. Its next request -- the successor's create, or the old build's delete of its
// fixed name -- is held where the server takes it while the gone holder acknowledges 21 late, and
// the state write carrying that floor is held before its I/O. Released in turn, after the
// successor's own first write, then restarted, the successor must still owe 21: nobody
// acknowledged 21 on it. Control: a same-name re-creation on the same server, under the same holds,
// comes back from the restart with 21 acknowledged (M4).
func TestForcedOrderAPredecessorsHeldWriteReachesNoSuccessor(t *testing.T) {
	for _, topology := range hubTopologies[:2] {
		for _, kind := range durableKinds {
			t.Run(topology.name+"/"+kind.logical, func(t *testing.T) {
				hub, pool := freshRigOn(t, topology, 0)
				hooks := testnats.Hook(t)

				t.Run("control: a same-name re-creation", func(t *testing.T) {
					sameNameRecreationLoses(t, hub, hooks, kind)
				})

				old, gone := seedPreviousHolder(t, hub, pool, kind)
				sent := hub.publish(t, kind, 30)
				late := ackThrough(t, gone, sent[19])
				if meta, _ := late.Metadata(); meta.Sequence.Stream != sent[20] {
					t.Fatalf("the gone holder's next message is %d, want %d", meta.Sequence.Stream, sent[20])
				}
				server := heldServer(t, hub)
				request := hooks.Hold(testnats.APIWorker, testnats.Where("the take-over's create or delete after its read", func(e testnats.Event) bool {
					return e.Subject == "$JS.API.CONSUMER.DELETE."+bus.Stream+"."+old ||
						strings.HasPrefix(e.Subject, "$JS.API.CONSUMER.CREATE."+bus.Stream+"."+kind.logical+"~")
				}))
				process := startDurableProcess(t, hub, pool, kind, freshNames, blockingHandlers())
				request.Reached(t)

				write := hooks.Hold(testnats.WritePreIO, testnats.OnServer(server), testnats.Consumer(old), ackFloorAt(sent[20]))
				if err := late.Ack(); err != nil {
					t.Fatalf("the gone holder's late acknowledgement: %v", err)
				}
				write.Reached(t)
				mark := hooks.Mark()
				request.Release()

				var successor string
				waitFor(t, 30*time.Second, func() bool {
					successor = successorOf(t, pool, kind.logical, old)
					if successor == "" {
						// The old build makes its successor under the same name.
						if len(hooks.Events(testnats.StoreCreate, testnats.Consumer(old), testnats.After(mark))) > 0 {
							successor = old
						}
					}
					return successor != "" && len(hooks.Events(testnats.Point("write.done"), testnats.Consumer(successor), testnats.After(mark))) > 0
				}, func() string { return "the take-over made no successor with a write of its own" })

				released := hooks.Mark()
				write.Release()
				hooks.Await(t, testnats.Point("write.done"), testnats.Consumer(old), testnats.After(released))
				restartStore(t, hub, server)

				_, js := hub.admin(t)
				var info *natsgo.ConsumerInfo
				waitFor(t, 30*time.Second, func() bool {
					var err error
					info, err = js.ConsumerInfo(bus.Stream, successor)
					return err == nil
				}, func() string { return "the successor " + successor + " did not come back from the restart" })
				if info.AckFloor.Stream >= sent[20] {
					t.Fatalf("the successor %s came back from the restart with ack floor %d: message %d, which nobody acknowledged on it, is skipped",
						successor, info.AckFloor.Stream, sent[20])
				}
				process.unblock()
				awaitHandled(t, sent[20:], process)
			})
		}
	}
}

// sameNameRecreationLoses is the control of family 1: on the hub's server, consumer C acknowledged
// to 20, its write of floor 21 held, C deleted and created again at 21 under its own name, its
// first write done, the held write released and the store restarted -- and C comes back with 21
// acknowledged. It shows the holds reach the order M4 measured, so the treatment's clean result
// is no blind spot.
func sameNameRecreationLoses(t *testing.T, hub *freshHub, hooks *testnats.Hooks, kind durableKind) {
	t.Helper()
	conn, js := hub.admin(t)
	name := "control-" + kind.logical
	config := natsgo.ConsumerConfig{Durable: name, DeliverSubject: natsgo.NewInbox(), FilterSubject: kind.config().FilterSubject, AckPolicy: natsgo.AckExplicitPolicy}
	sub, err := conn.SubscribeSync(config.DeliverSubject)
	if err != nil {
		t.Fatalf("subscribe the control's deliver subject: %v", err)
	}
	if _, err := js.AddConsumer(bus.Stream, &config); err != nil {
		t.Fatalf("create the control consumer: %v", err)
	}
	sent := hub.publish(t, kind, 30)
	late := ackThrough(t, sub, sent[19])
	server := heldServer(t, hub)
	write := hooks.Hold(testnats.WritePreIO, testnats.OnServer(server), testnats.Consumer(name), ackFloorAt(sent[20]))
	if err := late.Ack(); err != nil {
		t.Fatalf("the late acknowledgement: %v", err)
	}
	write.Reached(t)
	if err := js.DeleteConsumer(bus.Stream, name); err != nil {
		t.Fatalf("delete the control consumer: %v", err)
	}
	mark := hooks.Mark()
	again := startingAt(config, sent[20])
	again.DeliverSubject = natsgo.NewInbox()
	if _, err := js.AddConsumer(bus.Stream, &again); err != nil {
		t.Fatalf("create the control consumer again: %v", err)
	}
	hooks.Await(t, testnats.Point("write.done"), testnats.Consumer(name), testnats.After(mark))
	released := hooks.Mark()
	write.Release()
	hooks.Await(t, testnats.Point("write.done"), testnats.Consumer(name), testnats.After(released))
	restartStore(t, hub, server)
	var info *natsgo.ConsumerInfo
	waitFor(t, 30*time.Second, func() bool {
		info, err = js.ConsumerInfo(bus.Stream, name)
		return err == nil
	}, func() string { return fmt.Sprintf("the control consumer did not come back from the restart: %v", err) })
	if info.AckFloor.Stream < sent[20] {
		t.Fatalf("control: the same-name re-creation came back with ack floor %d, below %d: the holds did not reach M4's order", info.AckFloor.Stream, sent[20])
	}
	if err := js.DeleteConsumer(bus.Stream, name); err != nil {
		t.Fatalf("delete the control consumer: %v", err)
	}
}
