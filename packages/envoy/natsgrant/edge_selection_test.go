package natsgrant_test

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/consumernames"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/natsgrant"
)

// An edge listener's user, rendered natsgrant.Edge on a real server, writes through
// consumernames.KVStore what its fresh mode writes: a session's selection record and its own
// durable's. It writes neither the bucket's sentinel, here by ending replay mode as the sweep does
// (KVStore.EndReplay), nor another listener's durable record, so a holder of an edge credential can
// neither mark the bucket converted, which stops every fixed-mode listener at its next connect, nor
// rewrite another listener's durable. The server names each refused write. The control is the
// listener's own user (natsgrant.Listener), which writes all four. Before, the edge held Listener's
// write of every key of the bucket ($KV.envoy_session_consumers.>).
func TestTheEdgeWritesSessionRecordsAndItsOwnDurablesAlone(t *testing.T) {
	const machine = "m"
	kv := "$KV." + consumernames.SessionBucket + "."
	for _, tc := range []struct {
		name   string
		grant  natsgrant.Permissions
		prefix string
		edge   bool
	}{
		{name: "the edge's user", grant: edge(t, machine, "PRIV"), prefix: natsgrant.EdgeInboxPrefix(machine), edge: true},
		{name: "control: the listener's user", grant: natsgrant.Listener(machine), prefix: "_INBOX"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			seed, public := testnats.User(t)
			_, admin := testnats.User(t)
			quote := func(subjects []string) string {
				out := make([]string, len(subjects))
				for i, subject := range subjects {
					out[i] = strconv.Quote(subject)
				}
				return strings.Join(out, ", ")
			}
			config := fmt.Sprintf("jetstream {}\nauthorization {\n  users = [ { nkey: %q, permissions: { publish: { allow: [%s] }, subscribe: { allow: [%s] } } }, { nkey: %q } ]\n}\nno_auth_user: %q\n",
				public, quote(tc.grant.Publish), quote(tc.grant.Subscribe), admin, admin)
			_, uri := testnats.StartConfig(t, config, []string{"-js"}, true)
			adminConn := testnats.Connect(t, uri)
			t.Cleanup(adminConn.Close)
			if _, err := consumernames.CreateSessionBucket(ctx, adminConn, 1, consumernames.StoreState{Cutover: consumernames.CutoverDone, Boundary: 1, ReplayFrom: 4}); err != nil {
				t.Fatal(err)
			}
			violations := make(chan error, 16)
			options := []natsgo.Option{natsgo.ErrorHandler(func(_ *natsgo.Conn, _ *natsgo.Subscription, err error) { violations <- err })}
			if tc.edge {
				options = append(options, natsgo.CustomInboxPrefix(tc.prefix))
			}
			conn, err := testnats.ConnectSeed(t, uri, seed, options...)
			require.NoError(t, err)
			t.Cleanup(conn.Close)
			store := consumernames.NewKVStore(consumernames.ConnRequester{Conn: conn})
			within := func() (context.Context, context.CancelFunc) { return context.WithTimeout(ctx, 3*time.Second) }

			own, durable := "ses_edge_selection", consumernames.DurableKey(bus.ListenerDurable(machine))
			for _, key := range []string{own, durable} {
				callCtx, cancel := within()
				_, err := store.Write(callCtx, key, consumernames.Record{Start: 1}, 0)
				cancel()
				require.NoError(t, err, "%s's write of %s's record", tc.name, key)
			}

			other := consumernames.DurableKey(bus.ListenerDurable("other-machine"))
			callCtx, cancel := within()
			_, otherErr := store.Write(callCtx, other, consumernames.Record{Start: 1}, 0)
			cancel()
			callCtx, cancel = within()
			over, replayErr := store.EndReplay(callCtx, func(string) bool { return false })
			cancel()
			state, err := consumernames.NewKVStore(consumernames.ConnRequester{Conn: adminConn}).State(ctx)
			require.NoError(t, err)
			if !tc.edge {
				require.NoError(t, otherErr, "control: the listener's write of %s", other)
				require.True(t, replayErr == nil && over && state.ReplayFrom == 0, "control: the listener's EndReplay = %t, %v, sentinel %+v; want replay mode ended", over, replayErr, state)
				return
			}
			require.Error(t, otherErr, "the edge wrote another listener's durable record %s", other)
			require.Error(t, replayErr, "the edge's EndReplay wrote the sentinel")
			require.Equal(t, uint64(4), state.ReplayFrom, "the sentinel after the edge's EndReplay")
			for _, subject := range []string{kv + other, kv + consumernames.SentinelKey} {
				deadline := time.After(10 * time.Second)
			wait:
				for {
					select {
					case err := <-violations:
						if strings.Contains(err.Error(), "Permissions Violation for Publish to") && strings.Contains(err.Error(), strconv.Quote(subject)) {
							break wait
						}
					case <-deadline:
						t.Fatalf("the server named no publish violation for %q", subject)
					}
				}
			}
		})
	}
}
