//go:build natslive

package natsgrant_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/natsgrant"
)

// seedEnvironment is a bus environment that names seed as the connection's nkey seed and nothing
// else, so a bus client connects as that nkey user.
func seedEnvironment(seed string) bus.ConnectOption {
	return bus.WithEnvironment(func(name string) (string, bool) {
		if name == bus.NkeySeedEnvVar {
			return seed, true
		}
		return "", false
	})
}

// An expiry notice (bus.KeptExpiredSubject) is written only by a NATS user whose grant publishes
// every notification topic - a listener's, and Dispatch's own, whose envoy-listener writes it beside
// envoy-dispatch - and read only by a stored-message read, which Dispatch's grant holds and no
// session's does. On the hub's image, with the stream a listener's user owns:
//   - the listener's user stores a notice under its copy's message id (bus.Client.PublishKeptCopyExpiry),
//     and Dispatch's user reads it back with the stored-message read its outbox makes;
//   - a publish on the subject by the listener's and Dispatch's users is accepted, and the same
//     publish by a session's user (natsgrant.Session), the users every session on a host shares
//     (natsgrant.SessionPattern), the Legion daemon's (natsgrant.Daemon) and an edge listener's
//     (natsgrant.Edge) is each refused naming the subject;
//   - a subscription to the subject by each of those four is refused naming it, and so is a session's
//     stored-message read of the stream, so no session reads a notice by either road.
//
// The reader that tells an accepted publish from a refused one subscribes every notification topic
// on a user of its own; the listener's accepted publishes are the control that it sees one.
func TestOnlyAListenersUserStoresAnExpiryNoticeAndNoSessionReadsOne(t *testing.T) {
	subject := bus.KeptExpiredSubject("expiry-grant")
	listenerSeed, listenerKey := testnats.User(t)
	dispatchSeed, dispatchKey := testnats.User(t)
	sessionSeed, sessionKey := testnats.User(t)
	sharedSeed, sharedKey := testnats.User(t)
	daemonSeed, daemonKey := testnats.User(t)
	edgeSeed, edgeKey := testnats.User(t)
	readerSeed, readerKey := testnats.User(t)
	listener, dispatch, daemon, shared := natsgrant.Listener("m"), natsgrant.Dispatch("m"), natsgrant.Daemon(), natsgrant.SessionPattern()
	own, err := natsgrant.Session("ses_expiry_grant")
	require.NoError(t, err)
	edge := edge(t, "m", "PRIV")
	_, uri := testnats.StartImageConfig(t, testnats.HubImage, testnats.GrantedUsersConfig(
		testnats.StaticUser{NKey: listenerKey, Publish: listener.Publish, Subscribe: listener.Subscribe},
		testnats.StaticUser{NKey: dispatchKey, Publish: dispatch.Publish, Subscribe: dispatch.Subscribe},
		testnats.StaticUser{NKey: sessionKey, Publish: own.Publish, Subscribe: own.Subscribe},
		testnats.StaticUser{NKey: sharedKey, Publish: shared.Publish, Subscribe: shared.Subscribe},
		testnats.StaticUser{NKey: daemonKey, Publish: daemon.Publish, Subscribe: daemon.Subscribe},
		testnats.StaticUser{NKey: edgeKey, Publish: edge.Publish, Subscribe: edge.Subscribe},
		testnats.StaticUser{NKey: readerKey, Publish: []string{"example.unused"}, Subscribe: []string{"notifications.>"}},
	), []string{"-js"}, false)
	owner, err := bus.ConnectOwningStream([]string{uri}, bus.WithName("listener"), seedEnvironment(listenerSeed))
	require.NoError(t, err, "the listener's user creates the stream")
	t.Cleanup(owner.Close)
	reader, _ := grantedConnection(t, uri, readerSeed)

	t.Run("the listener's user stores a notice and Dispatch's user reads it back", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		notice := bus.KeptCopyExpiry{EventID: "dispatch-7", SourceEventID: "7", Role: "expiry-grant", Attempt: 1, DedupeKey: "dispatch-7", Sequence: 3, KeptAt: 1, DroppedAt: 2}
		require.NoError(t, owner.PublishKeptCopyExpiry(ctx, notice))
		outbox, err := bus.Connect([]string{uri}, bus.WithName("dispatch"), seedEnvironment(dispatchSeed))
		require.NoError(t, err, "Dispatch's user connects")
		t.Cleanup(outbox.Close)
		stored, err := outbox.NextStoredMessage(ctx, bus.KeptExpiredSubjectPrefix+">", 1)
		require.NoError(t, err, "Dispatch's user reads the notice")
		var read bus.KeptCopyExpiry
		require.NoError(t, json.Unmarshal(stored.Data, &read))
		require.Equal(t, notice, read)
		require.Equal(t, bus.KeptExpiryMsgID("expiry-grant", 3), stored.MsgID)
	})

	t.Run("only the listener's and Dispatch's users publish on the subject", func(t *testing.T) {
		// Subscribed after the first row, whose stored notice the reader would otherwise count
		// as a publish of this row's.
		received, err := reader.SubscribeSync("notifications.>")
		require.NoError(t, err)
		require.NoError(t, reader.Flush())
		for _, writer := range []struct{ name, seed string }{{"listener", listenerSeed}, {"dispatch", dispatchSeed}} {
			accepted, refused := publishOutcome(t, uri, writer.seed, received, []string{subject})
			require.Empty(t, refused, "the %s's user was refused its publish of %s", writer.name, subject)
			require.Equal(t, []string{subject}, accepted, "the reader did not receive the %s's publish", writer.name)
		}
		// A session's and the daemon's users publish no notification topic at all, so the
		// sentinel publishOutcome sends would be refused too: each refusal is read off its own
		// connection instead, and the listener's publish afterwards is the first message the
		// reader takes, which shows none of the refused ones reached it.
		for _, other := range []struct{ name, seed string }{
			{"a session", sessionSeed}, {"the sessions sharing a host", sharedSeed}, {"the Legion daemon", daemonSeed}, {"an edge listener", edgeSeed},
		} {
			conn, errs := grantedConnection(t, uri, other.seed)
			require.NoError(t, conn.Publish(subject, []byte(other.name)))
			require.NoError(t, conn.Flush())
			refusedNaming(t, errs, `Publish to "`+subject+`"`)
		}
		control, _ := grantedConnection(t, uri, listenerSeed)
		require.NoError(t, control.Publish(subject, []byte("control")))
		require.NoError(t, control.Flush())
		msg, err := received.NextMsg(10 * time.Second)
		require.NoError(t, err, "the listener's control publish did not reach the reader")
		require.Equal(t, "control", string(msg.Data), "a refused publish on %s reached the reader", subject)
	})

	t.Run("no session, daemon or edge user subscribes to it or reads it from the stream", func(t *testing.T) {
		for _, other := range []struct{ name, seed string }{
			{"a session", sessionSeed}, {"the sessions sharing a host", sharedSeed}, {"the Legion daemon", daemonSeed}, {"an edge listener", edgeSeed},
		} {
			conn, errs := grantedConnection(t, uri, other.seed)
			_, err := conn.SubscribeSync(subject)
			require.NoError(t, err)
			require.NoError(t, conn.Flush())
			refusedNaming(t, errs, `Subscription to "`+subject+`"`)
		}
		conn, errs := grantedConnection(t, uri, sessionSeed)
		require.NoError(t, conn.Publish("$JS.API.STREAM.MSG.GET."+bus.Stream, []byte(`{"seq":1,"next_by_subj":"`+bus.KeptExpiredSubjectPrefix+`>"}`)))
		require.NoError(t, conn.Flush())
		refusedNaming(t, errs, `Publish to "$JS.API.STREAM.MSG.GET.`+bus.Stream+`"`)
	})
}
