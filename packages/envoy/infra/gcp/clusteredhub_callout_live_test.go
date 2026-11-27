//go:build natslive

package gcp_test

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/jwt/v2"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/infra/nats"
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

// refusingAuthService is a stub auth service on the hub at uri, connected as the service user whose
// seed is serviceSeed: it refuses each authorization request, with a refusal signed by issuer and
// sealed to the server's xkey, and counts each one once its refusal is sent. A request it cannot
// open, decode, sign or answer counts in failed. Neither handler calls the test, which may have
// ended by the time a request arrives.
type refusingAuthService struct {
	answered, failed atomic.Int64
}

func startRefusingAuthService(t *testing.T, uri, serviceSeed string, issuer, curve nkeys.KeyPair) *refusingAuthService {
	t.Helper()
	s := &refusingAuthService{}
	conn, err := testnats.ConnectSeed(t, uri, serviceSeed)
	require.NoError(t, err, "the auth service's user, in auth_users, connects without a callout")
	t.Cleanup(conn.Close)
	_, err = conn.Subscribe("$SYS.REQ.USER.AUTH", func(m *natsgo.Msg) {
		opened, err := curve.Open(m.Data, m.Header.Get("Nats-Server-Xkey"))
		if err != nil {
			s.failed.Add(1)
			return
		}
		claims, err := jwt.DecodeAuthorizationRequestClaims(string(opened))
		if err != nil {
			s.failed.Add(1)
			return
		}
		response := jwt.NewAuthorizationResponseClaims(claims.UserNkey)
		response.Audience = claims.Server.ID
		response.Error = "example: this stub admits no client"
		token, err := response.Encode(issuer)
		if err != nil {
			s.failed.Add(1)
			return
		}
		sealed, err := curve.Seal([]byte(token), claims.Server.XKey)
		if err != nil || m.Respond(sealed) != nil {
			s.failed.Add(1)
			return
		}
		s.answered.Add(1)
	})
	require.NoError(t, err)
	require.NoError(t, conn.Flush())
	return s
}

// The clustered hub's session listener beside the callout, on a real nats-server with a stub auth
// service that counts and refuses every authorization request. The listener's seeded user, whose key
// core lists in auth_users, is admitted by its nkey with its listener grant, and the server asks the
// auth service nothing: the callout changes nothing for it. And a client presenting no credential
// still becomes the session user with no request either: it subscribes to another session's relay
// subject and publishes to another session's _INBOX subject, as it does on a hub with no callout. A
// hub keeps that until its sessionUser goes (dispatch://INFRA-840, R11). The control takes the
// listener's key out of auth_users in the same text: the server then sends the listener to the
// callout, the stub counts exactly that one request, and the listener is refused. Counting the
// requests rather than timing an unanswered one keeps the control from racing the server's first
// PING (core/nats_callout.go says why that PING can beat a 2 s authorization timeout).
func TestAClusteredHubsSeededListenerSkipsTheCallout(t *testing.T) {
	listenerSeed, listenerKey := testnats.User(t)
	serviceSeed, serviceKey := testnats.User(t)
	issuer, err := nkeys.CreateAccount()
	require.NoError(t, err)
	issuerKey, err := issuer.PublicKey()
	require.NoError(t, err)
	curve, err := nkeys.CreateCurveKeys()
	require.NoError(t, err)
	curveKey, err := curve.PublicKey()
	require.NoError(t, err)
	args := clusteredArgs()
	args.Tailnet.Listener.NKey = listenerKey
	service := authServiceUser()
	service.NKey = serviceKey
	args.Users = append(args.Users, service)
	args.AuthCallout = &nats.AuthCalloutArgs{Issuer: issuerKey, XKey: curveKey, ServiceUser: service.Name}
	conf := authConfOf(t, args)
	require.Contains(t, conf, "auth_callout {")
	_, uri := testnats.StartConfig(t, conf, nil, true)
	stub := startRefusingAuthService(t, uri, serviceSeed, issuer, curve)

	t.Run("the listener connects by its seed, and its listener grant holds", func(t *testing.T) {
		conn, errs := collectingConn(t, uri, listenerSeed)
		require.NoError(t, conn.Publish("notifications.example", nil))
		require.NoError(t, conn.Publish("outside.example", nil))
		awaitViolation(t, conn, errs, `Publish to "outside.example"`)
	})
	t.Run("a client with no credential still becomes the session user, past the auth service", func(t *testing.T) {
		conn, errs := collectingConn(t, uri, "")
		_, err := conn.SubscribeSync(bus.DeliverSubject("another-session"))
		require.NoError(t, err)
		require.NoError(t, conn.Publish("_INBOX.another-session.reply", nil))
		require.NoError(t, conn.Publish("outside.example", nil))
		awaitViolation(t, conn, errs, `Publish to "outside.example"`)
	})
	require.Zero(t, stub.answered.Load()+stub.failed.Load(), "the server asked the auth service about a keyed user or a client with no credential")

	t.Run("control: the listener's key out of auth_users is sent to the callout and refused", func(t *testing.T) {
		start := strings.Index(conf, "auth_users: [")
		require.NotEqual(t, -1, start, "the text has no auth_users")
		end := start + strings.Index(conf[start:], "\n")
		edited := conf[:start] + strings.Replace(conf[start:end], `"`+listenerKey+`", `, "", 1) + conf[end:]
		require.NotEqual(t, conf, edited, "the edit must take the listener's key out of auth_users")
		_, uri := testnats.StartConfig(t, edited, nil, true)
		stub := startRefusingAuthService(t, uri, serviceSeed, issuer, curve)
		_, err := testnats.ConnectSeed(t, uri, listenerSeed, natsgo.Timeout(5*time.Second))
		require.ErrorIs(t, err, natsgo.ErrAuthorization, "the listener was admitted although auth_users does not list it and the auth service refuses every client")
		require.Eventually(t, func() bool { return stub.answered.Load() == 1 }, 5*time.Second, 10*time.Millisecond,
			"the stub answered %d requests, want the listener's one", stub.answered.Load())
		require.Zero(t, stub.failed.Load(), "the stub could not answer a request")
	})
}
