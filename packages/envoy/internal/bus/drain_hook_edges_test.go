package bus_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// discardingCache is a kvwatch.Cache that keeps nothing: these tests read the watcher's consumers,
// not what it delivers.
type discardingCache struct{}

func (discardingCache) Apply(kvwatch.Entry, kvwatch.Read) {}
func (discardingCache) BeginRead(kvwatch.Read)            {}
func (discardingCache) CompleteRead(kvwatch.Read)         {}

// watchedBucket opens a bucket on client's connection and returns a watcher on it that has
// finished its first scan, with the server's own view of the bucket's consumers through admin.
func watchedBucket(t *testing.T, client *bus.Client, uri, bucket string) (*kvwatch.Watcher, func() []string) {
	t.Helper()
	kv, err := bus.EnsureKeyValue(client.Conn, client.JS(), &natsgo.KeyValueConfig{Bucket: bucket})
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	watcher := kvwatch.New("drain-test", kv, discardingCache{})
	watcher.Start()
	if err := watcher.WaitReady(testwait.Context(t)); err != nil {
		t.Fatalf("wait for the watcher: %v", err)
	}
	admin := testnats.Connect(t, uri)
	adminJS, err := admin.JetStream(testnats.MaxWait(t))
	if err != nil {
		t.Fatalf("admin JetStream: %v", err)
	}
	consumers := func() []string { return consumerNames(adminJS, "KV_"+bucket) }
	if got := consumers(); len(got) != 1 {
		t.Fatalf("consumers before the drain = %v, want the watcher's one", got)
	}
	return watcher, consumers
}

// awaitClosedRecord waits for the record the client named name logs from its connection's closed
// callback. nats.go runs a connection's callbacks one after another, in the order it queued them,
// and queues the closed callback at the close, so once that record is in, every report nats.go
// queued before the close has been logged too.
func awaitClosedRecord(t *testing.T, logs *busLogs, name string) {
	t.Helper()
	testwait.Next(t, "the closed callback's record of "+name, logs.Stream(), func(line string) bool {
		var record struct{ Msg, Name string }
		return json.Unmarshal([]byte(line), &record) == nil && record.Msg == "envoy nats connection closed" && record.Name == name
	})
}

// A connection lost while the drain hooks run leaves each watcher's consumer delete unsent, or
// sent with no answer coming: nats.go refuses a publish while reconnecting with the reconnect
// buffer off ("outbound buffer limit exceeded"), and a request already sent waits out its context.
// Neither is a failure of the watcher: the server deletes an ordered consumer whose interest is
// gone on its own (its inactive threshold, 5 s for an ephemeral), so the end is a WARN that says
// the delete was left to the server, never an ERROR.
func TestADrainHookLosingTheConnectionWarns(t *testing.T) {
	for _, test := range []struct {
		name string
		// lose cuts the link at the point the case names and returns once nats.go reconnects.
		lose func(t *testing.T, proxy *relayProxy, client *bus.Client) func()
	}{
		{
			name: "the delete is sent while reconnecting",
			lose: func(t *testing.T, proxy *relayProxy, client *bus.Client) func() {
				return func() {
					proxy.refuse.Store(true)
					proxy.drop()
					testwait.Eventually(t, "the client to start reconnecting", client.Conn.IsReconnecting)
				}
			},
		},
		{
			name: "the link drops with the delete in flight",
			lose: func(t *testing.T, proxy *relayProxy, client *bus.Client) func() {
				return func() {
					proxy.arm("$JS.API.CONSUMER.DELETE.")
					ended := t.Context()
					go func() {
						select {
						case <-proxy.swallowed:
						case <-ended.Done():
							return
						}
						proxy.refuse.Store(true)
						proxy.drop()
					}()
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, uri := testnats.Start(t)
			proxy := startRelayProxy(t, uri)
			logs := captureBusLogs(t)
			name := t.Name()
			client, err := bus.ConnectOwningStream([]string{proxy.url()}, bus.WithReplicas(1), bus.WithName(name))
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			t.Cleanup(client.Close)
			watcher, consumers := watchedBucket(t, client, uri, "drain_hook_lost")
			lose := test.lose(t, proxy, client)
			client.AddDrainHook(func(ctx context.Context) {
				lose()
				watcher.End(ctx)
			})
			watcher.Stop()
			_ = client.Drain(listenerDrain)
			awaitClosedRecord(t, logs, name)
			records := warnOrErrorRecords(t, logs, name)
			for _, record := range records {
				if strings.HasPrefix(record, "ERROR") {
					t.Fatalf("a connection lost during the drain hooks logged %q", records)
				}
			}
			if got, want := strings.Join(records, ", "), "WARN drain-test watcher's consumer was left to the server: the connection was lost before its delete"; got != want {
				t.Fatalf("the drain logged %q, want %q", got, want)
			}
			testwait.Eventually(t, "the server to delete the consumer it lost interest in", func() bool { return len(consumers()) == 0 })
		})
	}
}

// A delivery that outlasts the deliveries' share of a short drain leaves the hooks their share of
// it: the delivery subscriptions drain within all but the drain's last 3 s, or its last 30% when
// that is less, so the hooks and the connection's drain always keep it, however long a delivery
// takes. The drain is on the client's clock, which the test moves to the deliveries' share once
// the drain has armed it; the hooks then run with the other 600 ms of a 2 s drain left, read off
// the same clock.
func TestASlowDeliveryLeavesTheDrainHooksTheirShare(t *testing.T) {
	const drain = 2 * time.Second
	_, uri := testnats.Start(t)
	clock := clocktest.New(fakeStart)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1), bus.WithClock(clock))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	started := make(chan struct{})
	held := heldUntilCleanup(t)
	if _, err := client.SubscribeCore("notifications.role.drain-slow", func(*natsgo.Msg) {
		close(started)
		<-held
	}, "drain-slow"); err != nil {
		t.Fatalf("subscribe the role lane: %v", err)
	}
	if err := client.Conn.Publish("notifications.role.drain-slow", nil); err != nil {
		t.Fatalf("publish: %v", err)
	}
	testwait.Receive(t, "the delivery to reach its handler", started)
	left := make(chan time.Duration, 1)
	client.AddDrainHook(func(context.Context) {
		left <- fakeStart.Add(drain).Sub(clock.Now())
	})
	drained := drainOn(client, drain)
	share := fakeStart.Add(deliveriesShare(drain))
	awaitArmedAt(t, clock, "the drain's wait for the slow delivery", share.Equal)
	clock.Set(share)
	testwait.Receive(t, "the drain to end at its deadline", drained)
	select {
	case remaining := <-left:
		if want := drain * 3 / 10; remaining != want {
			t.Fatalf("the drain hooks ran with %s of the drain's %s left, want the %s share they keep", remaining, drain, want)
		}
	default:
		t.Fatal("the drain never ran its hooks")
	}
}

// The listener drains for 13 s, and its deliveries keep the 10 s they had before the drain hooks:
// a delivery that takes 9.5 s finishes, the drain reports no error, and the hooks still have their
// 3 s once it has (bus's drainHookReserve). The delivery's 9.5 s pass on the client's clock, which
// the test moves before it lets the delivery finish.
func TestADeliveryKeepsTenSecondsOfTheListenersDrain(t *testing.T) {
	_, uri := testnats.Start(t)
	clock := clocktest.New(fakeStart)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1), bus.WithClock(clock))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	started := make(chan struct{})
	finish := make(chan struct{})
	var delivered atomic.Bool
	if _, err := client.SubscribeCore("notifications.role.drain-window", func(*natsgo.Msg) {
		close(started)
		<-finish
		delivered.Store(true)
	}, "drain-window"); err != nil {
		t.Fatalf("subscribe the role lane: %v", err)
	}
	t.Cleanup(func() {
		select {
		case <-finish:
		default:
			close(finish)
		}
	})
	if err := client.Conn.Publish("notifications.role.drain-window", nil); err != nil {
		t.Fatalf("publish: %v", err)
	}
	testwait.Receive(t, "the delivery to reach its handler", started)
	type observed struct {
		delivered bool
		left      time.Duration
	}
	ran := make(chan observed, 1)
	client.AddDrainHook(func(context.Context) {
		ran <- observed{delivered: delivered.Load(), left: fakeStart.Add(listenerDrain).Sub(clock.Now())}
	})
	drained := drainOn(client, listenerDrain)
	awaitArmedAt(t, clock, "the drain's wait for the delivery, its 10 s share", fakeStart.Add(10*time.Second).Equal)
	clock.Set(fakeStart.Add(9500 * time.Millisecond))
	close(finish)
	if err := testwait.Receive(t, "the drain to end once the 9.5 s delivery finished", drained); err != nil {
		t.Fatalf("a 13 s drain of a 9.5 s delivery returned %v, want none", err)
	}
	select {
	case run := <-ran:
		if !run.delivered || run.left != 3500*time.Millisecond {
			t.Fatalf("the hooks ran with the delivery finished %v and %s of the drain left, want it finished and 3.5s left", run.delivered, run.left)
		}
	default:
		t.Fatal("the drain never ran its hooks")
	}
}

// A delivery that outlasts the drain's share for deliveries still lets the hooks end the KV
// watchers, within what is left of the deadline, before the connection closes: closing it with
// the watchers still subscribed would leave their consumers for the server to time out. The share
// is on the client's clock, which the test moves to it once the drain has armed it.
func TestAStuckDeliveryStillEndsTheKVWatchers(t *testing.T) {
	_, uri := testnats.Start(t)
	logs := captureBusLogs(t)
	name := t.Name()
	clock := clocktest.New(fakeStart)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1), bus.WithName(name), bus.WithClock(clock))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	watcher, consumers := watchedBucket(t, client, uri, "drain_hook_stuck")
	client.AddDrainHook(watcher.End)
	started := make(chan struct{})
	held := heldUntilCleanup(t)
	if _, err := client.SubscribeCore("notifications.role.drain-stuck", func(*natsgo.Msg) {
		close(started)
		<-held
	}, "drain-stuck"); err != nil {
		t.Fatalf("subscribe the role lane: %v", err)
	}
	if err := client.Conn.Publish("notifications.role.drain-stuck", nil); err != nil {
		t.Fatalf("publish: %v", err)
	}
	testwait.Receive(t, "the delivery to reach its handler", started)
	watcher.Stop()
	drained := drainOn(client, listenerDrain)
	share := fakeStart.Add(deliveriesShare(listenerDrain))
	awaitArmedAt(t, clock, "the drain's wait for the stuck delivery", share.Equal)
	clock.Set(share)
	if err := testwait.Receive(t, "the drain to end at its deadline", drained); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain error = %v, want deadline exceeded", err)
	}
	if got := consumers(); len(got) != 0 {
		t.Fatalf("consumers right after a drain a stuck delivery held = %v, want none", got)
	}
	if got := strings.Join(warnOrErrorRecords(t, logs, name), ", "); got != "" {
		t.Fatalf("the drain logged %q, want nothing", got)
	}
}
