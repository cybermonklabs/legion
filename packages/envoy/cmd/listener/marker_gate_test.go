package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A fixed-mode listener's main client, on which its durable is created and bound, makes no create
// of its durable at a reconnect until a read of the marker on the new connection says the store is
// still not converted. The window removes listener-<m> (convert replaces it with a fresh name) while
// the listener is cut off, and leaves a sentinel the listener cannot read (the server's STREAM.INFO
// answer stored over it, the deep review's reproduction) or one that says done; the rebuild and bind
// path (startListenerSubscription) then never creates listener-<m> again. The control is today's
// main client, which reads no marker at all and creates it at deliver policy all; the liveness row
// is a sentinel that still says not converted, on which the bind creates it again.
func TestAFixedModeReconnectCreatesNoDurableUntilItReadsTheMarker(t *testing.T) {
	for _, tc := range []struct {
		name      string
		gate      bool
		cutOff    func(t *testing.T, conn *natsgo.Conn)
		created   bool
		converted bool
	}{
		{name: "control: today's client, the sentinel replaced by a STREAM.INFO answer", cutOff: storeStreamInfoOverTheSentinel, created: true},
		{name: "the sentinel replaced by a STREAM.INFO answer", gate: true, cutOff: storeStreamInfoOverTheSentinel},
		{name: "the cutover's done marker", gate: true, cutOff: markTheCutoverDone, converted: true},
		{name: "liveness: a sentinel that still says not converted", gate: true, cutOff: func(*testing.T, *natsgo.Conn) {}, created: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, uri := testnats.Start(t)
			ctx := context.Background()
			admin := testnats.Connect(t, uri)
			t.Cleanup(admin.Close)
			if _, err := consumernames.CreateSessionBucket(ctx, admin, 1, consumernames.StoreState{Boundary: 1}); err != nil {
				t.Fatal(err)
			}
			client, err := bus.ConnectOwningStream([]string{uri})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(client.Close)
			converted := make(chan error, 16)
			if tc.gate {
				client.GateOnMarker(func(err error) { converted <- err })
			}
			if err := client.ReadMarker(ctx); err != nil {
				t.Fatalf("the start's read of a store the cutover has not converted = %v", err)
			}
			durable := bus.ListenerDurable("marker-gate-" + id.New())
			handler := func(msg *natsgo.Msg) { _ = msg.Ack() }
			sub, err := startListenerSubscription(client, durable, handler)
			if err != nil {
				t.Fatalf("the start's bind after the marker read: %v", err)
			}
			if err := sub.Unsubscribe(); err != nil {
				t.Fatal(err)
			}

			js, err := admin.JetStream()
			if err != nil {
				t.Fatal(err)
			}
			if err := js.DeleteConsumer(bus.Stream, durable); err != nil {
				t.Fatalf("remove %s as the window's convert does: %v", durable, err)
			}
			tc.cutOff(t, admin)
			srv.Stop(t)
			srv.Start(t)
			awaitClientConnected(t, client)

			var refusedUnread bool
			for deadline := time.Now().Add(4 * time.Second); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
				sub, err := startListenerSubscription(client, durable, handler)
				refusedUnread = refusedUnread || errors.Is(err, bus.ErrMarkerUnread)
				if err == nil {
					_ = sub.Unsubscribe()
					break
				}
			}
			check := testnats.Connect(t, uri)
			t.Cleanup(check.Close)
			checkJS, err := check.JetStream()
			if err != nil {
				t.Fatal(err)
			}
			info, infoErr := checkJS.ConsumerInfo(bus.Stream, durable)
			if held := infoErr == nil; held != tc.created {
				t.Fatalf("after the reconnect the server holds %s: %t (%v); want %t", durable, held, infoErr, tc.created)
			}
			if tc.created && info.Config.DeliverPolicy != natsgo.DeliverAllPolicy {
				t.Fatalf("%s was created at %v; want today's deliver policy all", durable, info.Config.DeliverPolicy)
			}
			if tc.gate && !tc.created && !refusedUnread {
				t.Fatal("no bind after the reconnect was refused by the marker gate")
			}
			select {
			case err := <-converted:
				if !tc.converted || !errors.Is(err, bus.ErrConvertedStore) {
					t.Fatalf("the reconnect stopped the listener: %v; want a stop %t", err, tc.converted)
				}
			default:
				if tc.converted {
					t.Fatal("a reconnect over the done marker did not stop the listener")
				}
			}
		})
	}
}

// A registration the marker gate refuses is answered 503 consumer_error, naming the marker and the
// bucket. The control is another failed consumer call, answered with today's text.
func TestARegistrationTheMarkerGateRefusesNamesTheMarker(t *testing.T) {
	delivery := &sessionDelivery{logger: logging.NewWithWriter("test", io.Discard), now: time.Now}
	refusal := delivery.refused("ses_gated", fmt.Errorf("bus: create: %w", bus.ErrMarkerUnread))
	if refusal.status != http.StatusServiceUnavailable || refusal.Reason != deliveryReasonConsumerError ||
		!strings.Contains(refusal.Error, consumernames.SentinelKey) || !strings.Contains(refusal.Error, consumernames.SessionBucket) {
		t.Fatalf("the gate's refusal = %d %s %q; want 503 %s naming %s of %s", refusal.status, refusal.Reason, refusal.Error, deliveryReasonConsumerError, consumernames.SentinelKey, consumernames.SessionBucket)
	}
	control := delivery.refused("ses_gated", errors.New("bus: create: the server did not answer"))
	if control.Reason != deliveryReasonConsumerError || strings.Contains(control.Error, consumernames.SentinelKey) {
		t.Fatalf("control: another failed call = %s %q; want %s naming no marker", control.Reason, control.Error, deliveryReasonConsumerError)
	}
}

// storeStreamInfoOverTheSentinel has the server store its STREAM.INFO answer for the selection
// bucket's stream under the sentinel's key, through a request's reply subject, and waits until the
// sentinel reads as malformed.
func storeStreamInfoOverTheSentinel(t *testing.T, conn *natsgo.Conn) {
	t.Helper()
	if err := conn.PublishRequest("$JS.API.STREAM.INFO.KV_"+consumernames.SessionBucket, "$KV."+consumernames.SessionBucket+"."+consumernames.SentinelKey, nil); err != nil {
		t.Fatal(err)
	}
	store := consumernames.NewKVStore(consumernames.ConnRequester{Conn: conn})
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err := store.State(context.Background())
		if errors.Is(err, consumernames.ErrMalformedSentinel) && strings.Contains(err.Error(), "unknown field") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stored answer never made the sentinel unreadable: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// markTheCutoverDone writes the selection bucket's sentinel with the cutover's done marker, as the
// cutover's convert does.
func markTheCutoverDone(t *testing.T, conn *natsgo.Conn) {
	t.Helper()
	ctx := context.Background()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	kv, err := js.KeyValue(consumernames.SessionBucket)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := kv.Get(consumernames.SentinelKey)
	if err != nil {
		t.Fatal(err)
	}
	store := consumernames.NewKVStore(consumernames.ConnRequester{Conn: conn})
	state, err := store.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state.Cutover = consumernames.CutoverDone
	if _, err := store.WriteSentinel(ctx, state, entry.Revision()); err != nil {
		t.Fatal(err)
	}
}

// awaitClientConnected waits until client's connection is connected again after a restart.
func awaitClientConnected(t *testing.T, client *bus.Client) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if conn := client.Connection(); conn != nil && conn.Status() == natsgo.CONNECTED {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the client did not connect again within 30 s of the restart")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
