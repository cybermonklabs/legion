package bus

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A fixed-mode process that started over a store the cutover had not converted, and was cut off
// while something changed the selection bucket's sentinel, makes no consumer call at its reconnect
// until a read of the marker on the new connection says the store is still not converted: a
// sentinel it cannot read (the server's STREAM.INFO answer stored over it through a request's
// reply subject, the deep review's reproduction) keeps every call refused with ErrMarkerUnread and
// today's fixed name uncreated, and a done marker stops the process and creates nothing. The
// control is today's reconnect check (the hook alone, gating nothing), under which the same
// reconnect over the unreadable sentinel creates s-<id>; the liveness row is a sentinel that still
// says not converted, which opens the gate again on the new connection.
func TestAFixedModeReconnectMakesNoConsumerCallUntilItReadsTheMarker(t *testing.T) {
	for _, tc := range []struct {
		name string
		// gate installs the marker gate; false runs today's reconnect check instead.
		gate bool
		// cutOff is what changes the sentinel while the process is cut off.
		cutOff func(t *testing.T, conn *natsgo.Conn)
		// created says whether the reconnect ends with today's fixed name on the server.
		created   bool
		converted bool
	}{
		{name: "control: today's check, the sentinel replaced by a STREAM.INFO answer", cutOff: storeStreamInfoOverTheSentinel, created: true},
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
			owner, err := ConnectOwningStream([]string{uri})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(owner.Close)
			consumers, err := ConnectSessionConsumers([]string{uri}, 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(consumers.Close)
			converted := make(chan error, 16)
			if tc.gate {
				consumers.GateOnMarker(func(err error) { converted <- err })
			} else {
				todaysReconnectCheck(consumers, func(err error) { converted <- err })
			}
			if err := consumers.ReadMarker(ctx); err != nil {
				t.Fatalf("the start's read of a store the cutover has not converted = %v", err)
			}
			const before = "ses_before_the_cut"
			if _, err := consumers.Ensure(ctx, before, []string{contracts.LegacyAgentSubject(before)}, 0); err != nil {
				t.Fatalf("a registration after the start's read: %v", err)
			}

			tc.cutOff(t, admin)
			srv.Stop(t)
			srv.Start(t)
			awaitConnected(t, consumers.client)

			const probe = "ses_after_the_cut"
			subjects := []string{contracts.LegacyAgentSubject(probe)}
			var refusedUnread bool
			for deadline := time.Now().Add(4 * time.Second); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
				callCtx, cancel := context.WithTimeout(ctx, time.Second)
				start, err := consumers.Ensure(callCtx, probe, subjects, 0)
				cancel()
				refusedUnread = refusedUnread || errors.Is(err, ErrMarkerUnread)
				if err == nil && start.Created {
					break
				}
			}
			check := testnats.Connect(t, uri)
			t.Cleanup(check.Close)
			js, err := jetstream.New(check)
			if err != nil {
				t.Fatal(err)
			}
			_, infoErr := js.Consumer(ctx, Stream, sessionConsumerPrefix+probe)
			if held := infoErr == nil; held != tc.created {
				t.Fatalf("after the reconnect the server holds %s%s: %t (%v); want %t", sessionConsumerPrefix, probe, held, infoErr, tc.created)
			}
			if tc.gate && !tc.created && !refusedUnread {
				t.Fatal("no call after the reconnect was refused by the marker gate")
			}
			select {
			case err := <-converted:
				if !tc.converted || !errors.Is(err, ErrConvertedStore) {
					t.Fatalf("the reconnect stopped the process: %v; want a stop %t", err, tc.converted)
				}
			default:
				if tc.converted {
					t.Fatal("a reconnect over the done marker did not stop the process")
				}
			}
			if tc.gate && !tc.created && !tc.converted {
				if _, hookErr := consumers.client.HookStall(); hookErr == nil || !errors.Is(hookErr, consumernames.ErrMalformedSentinel) {
					t.Fatalf("the reconnect's marker read = %v; want it failed on the malformed sentinel, and retried", hookErr)
				}
			}
		})
	}
}

// A marker read that no server answers (no responders: here a server with no JetStream, where
// every API request meets none) is a failed read: the gate stays shut and the caller reads again.
// Only the stream's own not-found answer is no bucket. The control is a JetStream server with no
// bucket, whose read opens the gate.
func TestAMarkerReadNoServerAnswersKeepsTheGateShut(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		jetstream bool
	}{{"control: a JetStream server with no bucket", true}, {"a server no JetStream answers on", false}} {
		t.Run(tc.name, func(t *testing.T) {
			var uri string
			if tc.jetstream {
				_, uri = testnats.Start(t)
			} else {
				_, uri = testnats.StartConfig(t, "", nil, true)
			}
			client, err := Connect([]string{uri})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(client.Close)
			client.GateOnMarker(nil)
			readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err = client.ReadMarker(readCtx)
			cancel()
			if tc.jetstream {
				if err != nil || client.MarkerUnread() != nil {
					t.Fatalf("control: the read with no bucket = %v, gate %v; want no marker and the gate open", err, client.MarkerUnread())
				}
				return
			}
			if err == nil || !errors.Is(err, natsgo.ErrNoResponders) {
				t.Fatalf("the read no server answered = %v; want a failed read wrapping nats.ErrNoResponders", err)
			}
			if !errors.Is(client.MarkerUnread(), ErrMarkerUnread) {
				t.Fatalf("the gate after a read no server answered = %v; want ErrMarkerUnread", client.MarkerUnread())
			}
		})
	}
}

// A consumer create published while the session consumers' connection is cut never reaches the
// server after the reconnect: the bus's connections hold no reconnect buffer (connect.go,
// options), so the publish fails at once, and nothing made under the old connection's marker read
// is sent on the new one, which no read has admitted. The gate is open before the cut. The control
// is a connection with nats.go's default reconnect buffer, which holds the same publish and sends
// it once the server is back, creating the consumer.
func TestACreatePublishedWhileCutOffNeverReachesTheServerAfterTheReconnect(t *testing.T) {
	for _, tc := range []struct {
		name     string
		buffered bool
		// durable takes the durable's connection, the listener's owning client, in place of the
		// session consumers'.
		durable bool
	}{
		{name: "control: nats.go's default reconnect buffer", buffered: true},
		{name: "the session consumers' connection"},
		{name: "the durable's connection", durable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, uri := testnats.Start(t)
			ctx := context.Background()
			admin := testnats.Connect(t, uri)
			t.Cleanup(admin.Close)
			if _, err := consumernames.CreateSessionBucket(ctx, admin, 1, consumernames.StoreState{Boundary: 1}); err != nil {
				t.Fatal(err)
			}
			owner, err := ConnectOwningStream([]string{uri})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(owner.Close)
			consumers, err := ConnectSessionConsumers([]string{uri}, 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(consumers.Close)
			gated := consumers.client
			if tc.durable {
				gated = owner
			}
			gated.GateOnMarker(nil)
			if err := gated.ReadMarker(ctx); err != nil || gated.MarkerUnread() != nil {
				t.Fatalf("the start's read = %v, gate %v; want the gate open", err, gated.MarkerUnread())
			}
			conn := gated.Connection()
			if tc.buffered {
				conn, err = natsgo.Connect(uri, natsgo.MaxReconnects(-1), natsgo.ReconnectWait(100*time.Millisecond))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(conn.Close)
			}

			srv.Stop(t)
			for deadline := time.Now().Add(10 * time.Second); conn.Status() == natsgo.CONNECTED; time.Sleep(10 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatal("the connection never saw the server stop")
				}
			}
			const name = sessionConsumerPrefix + "ses_sent_while_cut_off"
			body := []byte(`{"stream_name":"` + Stream + `","config":{"durable_name":"` + name + `","ack_policy":"explicit","deliver_policy":"all","filter_subject":"notifications.agent.ses_sent_while_cut_off"},"action":"create"}`)
			publishErr := conn.Publish(consumerAPI("CREATE", name), body)
			srv.Start(t)
			for deadline := time.Now().Add(30 * time.Second); conn.Status() != natsgo.CONNECTED; time.Sleep(20 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatal("the connection did not connect again within 30 s of the restart")
				}
			}
			if err := conn.Flush(); err != nil {
				t.Fatal(err)
			}

			check := testnats.Connect(t, uri)
			t.Cleanup(check.Close)
			js, err := jetstream.New(check)
			if err != nil {
				t.Fatal(err)
			}
			var held bool
			for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && !held; time.Sleep(50 * time.Millisecond) {
				_, infoErr := js.Consumer(ctx, Stream, name)
				held = infoErr == nil
			}
			if tc.buffered {
				if publishErr != nil || !held {
					t.Fatalf("control: the buffered publish answered %v and the server holds %s: %t; want it sent after the reconnect", publishErr, name, held)
				}
				return
			}
			if !errors.Is(publishErr, natsgo.ErrReconnectBufExceeded) || held {
				t.Fatalf("the publish while cut off answered %v and the server holds %s: %t; want it refused at once and never sent", publishErr, name, held)
			}
		})
	}
}

// todaysReconnectCheck is the fixed mode's reconnect check as it stood before the marker gate
// (WatchConversion at 300bee77fd): each connection reads the marker and calls converted on done,
// and a failed read fails the hook's run, but no consumer call waits for any of it.
func todaysReconnectCheck(s *SessionConsumers, converted func(error)) {
	s.client.AddReconnectHook(func(ctx context.Context, _ *natsgo.Conn) error {
		err := s.ReadMarker(ctx)
		switch {
		case err == nil, errors.Is(err, ErrPublishDenied):
			return nil
		case errors.Is(err, ErrConvertedStore):
			converted(err)
			return nil
		}
		return err
	})
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
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatal(err)
	}
	kv, err := js.KeyValue(ctx, consumernames.SessionBucket)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := kv.Get(ctx, consumernames.SentinelKey)
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

// awaitConnected waits until client's connection is connected again after a restart.
func awaitConnected(t *testing.T, client *Client) {
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
