//go:build natslive

package natsgrant_test

import (
	"context"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/natsgrant"
)

// The expiry notice's subject for the role these rows use, and the filter every notice matches,
// spelled out rather than read from bus: these rows are the grant's preservation rows, which run at
// a tree that has no expiry notice yet as well as at one that has, so they use only what both have
// - natsgrant's sets, a raw subject, a plain publish and the stored-message read. The head's own
// rows hold these strings to bus's (TestTheGrantRowsNameTheExpirySubjectBusWrites), so they cannot
// drift from the subject the listener writes.
const (
	expiryNoticeRole     = "expiry-grant"
	expiryNoticeSubject  = "notifications.envoy.kept.expired." + expiryNoticeRole
	expiryNoticeSubjects = "notifications.envoy.kept.expired.>"
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

// expiryGrantHub is a hub on the hub's image whose users hold the sets the expiry notice's grant
// rows test, each by its nkey seed, with the stream a listener's user owns.
type expiryGrantHub struct {
	uri                                               string
	owner                                             *bus.Client
	listener, dispatch, session, shared, daemon, edge string
	listenerHost, host, reader                        string
}

// startExpiryGrantHub starts an expiryGrantHub: the listener's (natsgrant.Listener), Dispatch's
// (natsgrant.Dispatch), one session's (natsgrant.Session), the users every session on a host shares
// (natsgrant.SessionPattern), the Legion daemon's (natsgrant.Daemon), an edge listener's
// (natsgrant.Edge), a listener host's (natsgrant.ListenerHost) and a host's (natsgrant.Host), and a
// reader of every notification topic of the test's own.
func startExpiryGrantHub(t *testing.T) expiryGrantHub {
	t.Helper()
	var hub expiryGrantHub
	listenerSeed, listenerKey := testnats.User(t)
	dispatchSeed, dispatchKey := testnats.User(t)
	sessionSeed, sessionKey := testnats.User(t)
	sharedSeed, sharedKey := testnats.User(t)
	daemonSeed, daemonKey := testnats.User(t)
	edgeSeed, edgeKey := testnats.User(t)
	listenerHostSeed, listenerHostKey := testnats.User(t)
	hostSeed, hostKey := testnats.User(t)
	readerSeed, readerKey := testnats.User(t)
	hub.listener, hub.dispatch, hub.session, hub.shared, hub.daemon, hub.edge = listenerSeed, dispatchSeed, sessionSeed, sharedSeed, daemonSeed, edgeSeed
	hub.listenerHost, hub.host, hub.reader = listenerHostSeed, hostSeed, readerSeed
	listener, dispatch, daemon, shared := natsgrant.Listener("m"), natsgrant.Dispatch("m"), natsgrant.Daemon(), natsgrant.SessionPattern()
	listenerHost, host := natsgrant.ListenerHost("m"), natsgrant.Host("m")
	own, err := natsgrant.Session("ses_expiry_grant")
	require.NoError(t, err)
	edge := edge(t, "m", "PRIV")
	_, hub.uri = testnats.StartImageConfig(t, testnats.HubImage, testnats.GrantedUsersConfig(
		testnats.StaticUser{NKey: listenerKey, Publish: listener.Publish, Subscribe: listener.Subscribe},
		testnats.StaticUser{NKey: dispatchKey, Publish: dispatch.Publish, Subscribe: dispatch.Subscribe},
		testnats.StaticUser{NKey: sessionKey, Publish: own.Publish, Subscribe: own.Subscribe},
		testnats.StaticUser{NKey: sharedKey, Publish: shared.Publish, Subscribe: shared.Subscribe},
		testnats.StaticUser{NKey: daemonKey, Publish: daemon.Publish, Subscribe: daemon.Subscribe},
		testnats.StaticUser{NKey: edgeKey, Publish: edge.Publish, Subscribe: edge.Subscribe},
		testnats.StaticUser{NKey: listenerHostKey, Publish: listenerHost.Publish, Subscribe: listenerHost.Subscribe},
		testnats.StaticUser{NKey: hostKey, Publish: host.Publish, Subscribe: host.Subscribe},
		testnats.StaticUser{NKey: readerKey, Publish: []string{"example.unused"}, Subscribe: []string{"notifications.>"}},
	), []string{"-js"}, false)
	hub.owner, err = bus.ConnectOwningStream([]string{hub.uri}, bus.WithName("listener"), seedEnvironment(listenerSeed))
	require.NoError(t, err, "the listener's user creates the stream")
	t.Cleanup(hub.owner.Close)
	return hub
}

// An expiry notice (notifications.envoy.kept.expired.<role>) is written only by a NATS user whose
// grant publishes every notification topic - a listener's (natsgrant.Listener), Dispatch's own
// (natsgrant.Dispatch), whose envoy-listener writes it beside envoy-dispatch, and the two legacy
// sets built on the listener's, natsgrant.ListenerHost and natsgrant.Host - and read by
// Dispatch's stored-message read and by the subscription to every notification topic that
// ListenerHost and Host hold, and by neither of a session's own sets (natsgrant.Session,
// natsgrant.SessionPattern). A legacy host's sessions that share its Host user share that user's
// reach. On the hub's image, with the stream a listener's user owns:
//   - the listener's user stores a message on the subject, and Dispatch's user reads it back with
//     the stored-message read its outbox makes;
//   - a publish on the subject by the listener's, Dispatch's, a listener host's and a host's users
//     is accepted, and the same publish by a session's user, the users every session on a host
//     shares, the Legion daemon's and an edge listener's is each refused naming the subject;
//   - a listener host's and a host's users receive a notice on their subscription to every
//     notification topic;
//   - a subscription to the subject by each of the four refused users is refused naming it, and so
//     is a session's stored-message read of the stream, so no session's user reads a notice by
//     either road.
//
// These rows use only what a tree with no expiry notice has (expiryNoticeSubject), so they run at
// legion main as well: there they show that the grant this change relies on is the grant main
// already deploys, which is why this change adds none. The reader that tells an accepted publish
// from a refused one subscribes every notification topic on a user of its own; the listener's
// accepted publishes are the control that it sees one.
func TestOnlyUsersOnTheListenersGrantWriteOrReadAnExpiryNotice(t *testing.T) {
	hub := startExpiryGrantHub(t)
	subject := expiryNoticeSubject
	reader, _ := grantedConnection(t, hub.uri, hub.reader)

	t.Run("the listener's user stores a message on the subject and Dispatch's user reads it back", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		body := []byte(`{"role":"` + expiryNoticeRole + `","sequence":3}`)
		_, err := hub.owner.JS().Publish(subject, body, natsgo.MsgId("expiry-grant-stored-row"), natsgo.Context(ctx))
		require.NoError(t, err, "the listener's user stores a message on %s", subject)
		outbox, err := bus.Connect([]string{hub.uri}, bus.WithName("dispatch"), seedEnvironment(hub.dispatch))
		require.NoError(t, err, "Dispatch's user connects")
		t.Cleanup(outbox.Close)
		stored, err := outbox.NextStoredMessage(ctx, expiryNoticeSubjects, 1)
		require.NoError(t, err, "Dispatch's user reads the message")
		require.Equal(t, subject, stored.Subject)
		require.Equal(t, body, stored.Data)
	})

	t.Run("only the listener's, Dispatch's, a listener host's and a host's users publish on the subject", func(t *testing.T) {
		// Subscribed after the first row, whose stored message the reader would otherwise count
		// as a publish of this row's.
		received, err := reader.SubscribeSync("notifications.>")
		require.NoError(t, err)
		require.NoError(t, reader.Flush())
		for _, writer := range []struct{ name, seed string }{
			{"listener", hub.listener}, {"dispatch", hub.dispatch}, {"listener host", hub.listenerHost}, {"host", hub.host},
		} {
			accepted, refused := publishOutcome(t, hub.uri, writer.seed, received, []string{subject})
			require.Empty(t, refused, "the %s's user was refused its publish of %s", writer.name, subject)
			require.Equal(t, []string{subject}, accepted, "the reader did not receive the %s's publish", writer.name)
		}
		// A session's and the daemon's users publish no notification topic at all, so the
		// sentinel publishOutcome sends would be refused too: each refusal is read off its own
		// connection instead, and the listener's publish afterwards is the first message the
		// reader takes, which shows none of the refused ones reached it.
		for _, other := range []struct{ name, seed string }{
			{"a session", hub.session}, {"the sessions sharing a host", hub.shared}, {"the Legion daemon", hub.daemon}, {"an edge listener", hub.edge},
		} {
			conn, errs := grantedConnection(t, hub.uri, other.seed)
			require.NoError(t, conn.Publish(subject, []byte(other.name)))
			require.NoError(t, conn.Flush())
			refusedNaming(t, errs, `Publish to "`+subject+`"`)
		}
		control, _ := grantedConnection(t, hub.uri, hub.listener)
		require.NoError(t, control.Publish(subject, []byte("control")))
		require.NoError(t, control.Flush())
		msg, err := received.NextMsg(10 * time.Second)
		require.NoError(t, err, "the listener's control publish did not reach the reader")
		require.Equal(t, "control", string(msg.Data), "a refused publish on %s reached the reader", subject)
	})

	t.Run("a listener host's and a host's users receive a notice on their subscription", func(t *testing.T) {
		for _, other := range []struct{ name, seed string }{{"listener host", hub.listenerHost}, {"host", hub.host}} {
			conn, _ := grantedConnection(t, hub.uri, other.seed)
			sub, err := conn.SubscribeSync("notifications.>")
			require.NoError(t, err)
			require.NoError(t, conn.Flush())
			control, _ := grantedConnection(t, hub.uri, hub.listener)
			require.NoError(t, control.Publish(subject, []byte(other.name)))
			require.NoError(t, control.Flush())
			msg, err := sub.NextMsg(10 * time.Second)
			require.NoError(t, err, "the %s's user did not receive a notice", other.name)
			require.Equal(t, subject, msg.Subject)
		}
	})

	t.Run("no session, daemon or edge user subscribes to it or reads it from the stream", func(t *testing.T) {
		for _, other := range []struct{ name, seed string }{
			{"a session", hub.session}, {"the sessions sharing a host", hub.shared}, {"the Legion daemon", hub.daemon}, {"an edge listener", hub.edge},
		} {
			conn, errs := grantedConnection(t, hub.uri, other.seed)
			_, err := conn.SubscribeSync(subject)
			require.NoError(t, err)
			require.NoError(t, conn.Flush())
			refusedNaming(t, errs, `Subscription to "`+subject+`"`)
		}
		conn, errs := grantedConnection(t, hub.uri, hub.session)
		require.NoError(t, conn.Publish("$JS.API.STREAM.MSG.GET."+bus.Stream, []byte(`{"seq":1,"next_by_subj":"`+expiryNoticeSubjects+`"}`)))
		require.NoError(t, conn.Flush())
		refusedNaming(t, errs, `Publish to "$JS.API.STREAM.MSG.GET.`+bus.Stream+`"`)
	})
}
