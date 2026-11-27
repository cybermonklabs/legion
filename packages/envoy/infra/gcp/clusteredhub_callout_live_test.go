//go:build natslive

package gcp_test

import (
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

func authConfOf(t *testing.T, args *gcp.ClusteredNatsHubArgs) string {
	t.Helper()
	s, err := gcp.ResolveClusteredNatsHub(args)
	require.NoError(t, err)
	return s.AuthConf
}

// The measurement behind envoy-infra.sjawhar.dev/auth-callout: nats-server v2.15.1, the version
// the clustered hub's chart runs and this test binary links in, takes a users change in auth.conf
// on a reload, as the chart's reloader signals one, but refuses the whole reload when the callout
// changes, adding it or changing its lines alike ("config reload not supported for AuthCallout"),
// and keeps serving what it had. So a callout change reaches a server only when it restarts, which
// the annotation forces by changing the pod template (TestClusteredNatsHubRollsThePodsForACalloutChange).
// The users-change reload is the control: it shows a reload is taken at all, so each refusal is
// the callout's and not the path's.
func TestClusteredNatsHubReloadRefusesACalloutChange(t *testing.T) {
	without := withoutTailnet(clusteredArgs())
	without.Users = append(without.Users, authServiceUser())
	with := calloutClusteredHub()
	server, _ := testnats.StartConfig(t, authConfOf(t, without), nil, true)

	t.Run("a users change without a callout reloads", func(t *testing.T) {
		changed := withoutTailnet(clusteredArgs())
		changed.Users = append(changed.Users, authServiceUser())
		changed.Users[1].Permissions.Publish.Allow = []string{"notifications.>"}
		require.NoError(t, server.Reload(t, authConfOf(t, changed)))
	})
	t.Run("adding the callout is refused", func(t *testing.T) {
		err := server.Reload(t, authConfOf(t, with))
		require.ErrorContains(t, err, "config reload not supported for AuthCallout")
	})
	t.Run("on a server that started with the callout", func(t *testing.T) {
		server.Restart(t, authConfOf(t, with))
		t.Run("a grant change, which leaves the callout's lines, reloads", func(t *testing.T) {
			changed := calloutClusteredHub()
			changed.Users[1].Permissions.Publish.Allow = []string{"notifications.>"}
			require.NoError(t, server.Reload(t, authConfOf(t, changed)))
		})
		t.Run("a user added to auth_users is refused", func(t *testing.T) {
			_, added := testnats.User(t)
			changed := calloutClusteredHub()
			changed.Users = append(changed.Users, authServiceUser())
			changed.Users[len(changed.Users)-1].Name, changed.Users[len(changed.Users)-1].NKey = "example-auth-b", added
			err := server.Reload(t, authConfOf(t, changed))
			require.ErrorContains(t, err, "config reload not supported for AuthCallout")
		})
		t.Run("another xkey is refused", func(t *testing.T) {
			changed := calloutClusteredHub()
			changed.AuthCallout.XKey = otherXKey(t)
			err := server.Reload(t, authConfOf(t, changed))
			require.ErrorContains(t, err, "config reload not supported for AuthCallout")
		})
	})
}

// collectingConn connects to uri as the nkey user whose seed is seed, or with no credential when
// seed is "", and returns the connection with the server's asynchronous errors, a permissions
// violation among them, in the order the server sent them.
func collectingConn(t *testing.T, uri, seed string) (*natsgo.Conn, <-chan error) {
	t.Helper()
	errs := make(chan error, 16)
	conn, err := testnats.ConnectSeed(t, uri, seed, natsgo.ErrorHandler(func(_ *natsgo.Conn, _ *natsgo.Subscription, err error) { errs <- err }))
	require.NoError(t, err)
	t.Cleanup(conn.Close)
	return conn, errs
}

// requireFirstViolation flushes conn and requires the server's first asynchronous error on it to
// be the permissions violation of the operation want names. The server answers a connection's
// operations in order, so every operation sent before that one was allowed: a refusal of any of
// them would have come first.
func requireFirstViolation(t *testing.T, conn *natsgo.Conn, errs <-chan error, want string) {
	t.Helper()
	require.NoError(t, conn.Flush())
	select {
	case err := <-errs:
		require.ErrorContains(t, err, want, "the first operation the server refused is not the control")
	case <-time.After(5 * time.Second):
		t.Fatalf("the server refused nothing within 5 s, not even the control %s", want)
	}
}

// The clustered hub's session listener beside the callout, on a real nats-server and with no auth
// service running, so any connection the server sent to the callout would be refused once the
// server's 2 s authorization timeout passed. The listener's seeded user, whose key core lists in
// auth_users, is admitted by its nkey with its listener grant: the callout changes nothing for it.
// And a client presenting no credential still becomes the session user, with no callout either:
// it subscribes to another session's relay subject and publishes to another session's _INBOX
// subject, as it does on a hub with no callout. A hub keeps that until its sessionUser goes
// (dispatch://INFRA-840, R11). The control removes the listener's key from auth_users in the same
// text: the server then sends the listener to the callout, and with no service to answer it is not
// admitted.
func TestAClusteredHubsSeededListenerSkipsTheCallout(t *testing.T) {
	listenerSeed, listenerKey := testnats.User(t)
	args := clusteredArgs()
	args.Tailnet.Listener.NKey = listenerKey
	args.Users = append(args.Users, authServiceUser())
	args.AuthCallout = authCallout()
	conf := authConfOf(t, args)
	require.Contains(t, conf, "auth_callout {")
	_, uri := testnats.StartConfig(t, conf, nil, true)

	t.Run("the listener connects by its seed, and its listener grant holds", func(t *testing.T) {
		conn, errs := collectingConn(t, uri, listenerSeed)
		require.NoError(t, conn.Publish("notifications.example", nil))
		require.NoError(t, conn.Publish("outside.example", nil))
		requireFirstViolation(t, conn, errs, `Publish to "outside.example"`)
	})
	t.Run("a client with no credential still becomes the session user, past the auth service", func(t *testing.T) {
		conn, errs := collectingConn(t, uri, "")
		_, err := conn.SubscribeSync(bus.DeliverSubject("another-session"))
		require.NoError(t, err)
		require.NoError(t, conn.Publish("_INBOX.another-session.reply", nil))
		require.NoError(t, conn.Publish("outside.example", nil))
		requireFirstViolation(t, conn, errs, `Publish to "outside.example"`)
	})
	t.Run("control: the listener's key out of auth_users is sent to the callout and not admitted", func(t *testing.T) {
		start := strings.Index(conf, "auth_users: [")
		require.NotEqual(t, -1, start, "the text has no auth_users")
		end := start + strings.Index(conf[start:], "\n")
		edited := conf[:start] + strings.Replace(conf[start:end], `"`+listenerKey+`", `, "", 1) + conf[end:]
		require.NotEqual(t, conf, edited, "the edit must take the listener's key out of auth_users")
		_, uri := testnats.StartConfig(t, edited, nil, true)
		began := time.Now()
		_, err := testnats.ConnectSeed(t, uri, listenerSeed, natsgo.Timeout(5*time.Second))
		require.ErrorIs(t, err, natsgo.ErrAuthorization, "the listener was admitted, or refused for another reason, although auth_users does not list it and no auth service answers")
		require.GreaterOrEqual(t, time.Since(began), 2*time.Second, "the refusal came before nats-server's 2 s authorization timeout, so it is not the callout's unanswered request")
	})
}
