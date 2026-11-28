//go:build natslive

package gcp_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/jwt/v2"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/infra/nats"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
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

// admittingAuthService is a stub auth service on the hub at uri, connected as the service user
// whose seed is serviceSeed. It admits an authorization request only when its connect options
// carry the user a service presents, "service" (natscred.ServiceUser), and token as the password,
// answering a user named listener/<machineID> with core.ListenerPermissions(machineID), the grant
// the auth service answers a listener entry with (natsgrant.Listener), in the global account and
// lapsing an hour on; it refuses every other request. Each answer is signed by issuer and sealed
// to the server's xkey, and counted in admitted or refused once it is sent; a request it cannot
// open, decode, sign or answer counts in failed. Neither handler calls the test, which may have
// ended by the time a request arrives.
type admittingAuthService struct {
	admitted, refused, failed atomic.Int64
}

func startAdmittingAuthService(t *testing.T, uri, serviceSeed string, issuer, curve nkeys.KeyPair, token, machineID string) *admittingAuthService {
	t.Helper()
	s := &admittingAuthService{}
	grant := core.ListenerPermissions(machineID)
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
		admit := claims.ConnectOptions.Username == "service" && claims.ConnectOptions.Password == token
		if admit {
			user := jwt.NewUserClaims(claims.UserNkey)
			user.Audience = "$G"
			user.Name = "listener/" + machineID
			user.Expires = time.Now().Add(time.Hour).Unix()
			user.Pub.Allow.Add(grant.Publish.Allow...)
			user.Pub.Deny.Add(grant.Publish.Deny...)
			user.Sub.Allow.Add(grant.Subscribe.Allow...)
			user.Sub.Deny.Add(grant.Subscribe.Deny...)
			if response.Jwt, err = user.Encode(issuer); err != nil {
				s.failed.Add(1)
				return
			}
		} else {
			response.Error = "example: this stub admits the one listener token it was given"
		}
		encoded, err := response.Encode(issuer)
		if err != nil {
			s.failed.Add(1)
			return
		}
		sealed, err := curve.Seal([]byte(encoded), claims.Server.XKey)
		if err != nil || m.Respond(sealed) != nil {
			s.failed.Add(1)
			return
		}
		if admit {
			s.admitted.Add(1)
		} else {
			s.refused.Add(1)
		}
	})
	require.NoError(t, err)
	require.NoError(t, conn.Flush())
	return s
}

// unsignedListenerToken is an unsigned JWT naming account, issued now and lapsing an hour on, in
// the shape of the projected token the kubelet writes for the listener: the stub auth service
// compares it whole, and the listener's bus reads its expiry, neither verifying it (as natscred's
// platformToken).
func unsignedListenerToken(account string) string {
	encode := func(v any) string {
		raw, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	now := time.Now()
	return encode(map[string]string{"alg": "none"}) + "." +
		encode(map[string]any{"sub": account, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}) + ".unsigned"
}

// hubConnection is one client connection a hub's monitoring port lists with the user the server
// authorized it as (/connz?auth=1): a static user's public key, the session user's for a client
// presenting no credential, and the name the auth service's answer gave a client it admitted.
type hubConnection struct {
	Name           string `json:"name"`
	AuthorizedUser string `json:"authorized_user"`
}

// hubConnections is the client connections server lists in state: "open", or "all", the closed
// ones too.
func hubConnections(t *testing.T, server *testnats.Server, state string) []hubConnection {
	t.Helper()
	response, err := http.Get(server.MonitorURL() + "/connz?auth=1&limit=10000&state=" + state)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode, "the hub's /connz")
	var connz struct {
		Connections []hubConnection `json:"connections"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&connz))
	return connz.Connections
}

// authorizedAs is how many of connections the server authorized as user.
func authorizedAs(connections []hubConnection, user string) int {
	n := 0
	for _, c := range connections {
		if c.AuthorizedUser == user {
			n++
		}
	}
	return n
}

// requireRunning fails the test with the process's output when it has exited.
func requireRunning(t *testing.T, process *cmdtest.Process, name string) {
	t.Helper()
	select {
	case <-process.Exited:
		t.Fatalf("%s exited:\n%s", name, process.Output.String())
	default:
	}
}

// TestAClusteredHubsTokenListenerIsAdmittedThroughTheCallout (c-listener-live): the clustered
// hub's session listener in the token mode, on a real nats-server rendered from the hub, with a
// stub auth service. The hub is clusteredArgs behind the callout, its tailnet.listener given
// natsAudience: auth_users lists the users' keys and no key of the listener's, so the server sends
// the listener's connection to the auth service. The listener binary of this checkout runs in the
// environment the component renders for its container, read from the rendered Deployment, which
// names the token's file in NATS_SERVICE_TOKEN_FILE and holds no NATS_NKEY_SEED_FILE, with three
// values replaced for this machine: PORT, NATS_URLS (the test's server), and
// NATS_SERVICE_TOKEN_FILE, pointed at a file of the test's holding a token of the listener's own
// account in the kubelet's projected token's place. The stub admits that token alone, as
// listener/<machine id> with the listener grant, as the auth service answers a listener entry
// (internal/cipher/natscallout's TestAServiceTokenGetsItsKindsGrant): the server's /connz then
// names the listener's connection listener/envoy-hub, and the stub admitted it and failed nothing.
//
// Controls, each with a server, a stub and a listener of its own: the token file holding another
// token, which the stub refuses, while the listener runs on, waiting the refusal of its credential
// out (cmd/listener's listenerCredentialWait), and no connection is ever listener/envoy-hub; and
// the environment without NATS_SERVICE_TOKEN_FILE, where the listener presents no credential and
// becomes the session user, as any such client does, and the server asks the auth service
// nothing.
func TestAClusteredHubsTokenListenerIsAdmittedThroughTheCallout(t *testing.T) {
	const (
		machineID    = "envoy-hub"
		listenerUser = "listener/" + machineID
	)
	binary := buildLiveListener(t)
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
	byToken(args.Tailnet.Listener)
	service := authServiceUser()
	service.NKey = serviceKey
	args.Users = append(args.Users, service)
	args.AuthCallout = &nats.AuthCalloutArgs{Issuer: issuerKey, XKey: curveKey, ServiceUser: service.Name}
	conf := authConfOf(t, args)
	require.Contains(t, conf, `auth_users: ["`+strings.Join([]string{keyA, keyB, keyC, keyD, serviceKey, keySessions}, `", "`)+`"]`,
		"auth_users lists the users' keys alone, and no key of the listener's")

	rendered := containerEnv(t, podContainer(t, deploymentPodTemplate(t, runClusteredHub(t, args), clusteredListenerName), "listener"))
	require.Equal(t, "/var/run/secrets/envoy/nats-token", rendered[bus.ServiceTokenFileEnvVar])
	require.NotContains(t, rendered, "NATS_NKEY_SEED_FILE")
	require.Equal(t, machineID, rendered["ENVOY_MACHINE_ID"])
	token := unsignedListenerToken("system:serviceaccount:envoy:" + clusteredListenerName)

	// hub starts the rendered authorization's server with JetStream, which the listener's stream
	// needs, and the stub admitting token beside it.
	hub := func(t *testing.T) (*testnats.Server, string, *admittingAuthService) {
		t.Helper()
		server, uri := testnats.StartConfig(t, conf, []string{"-js"}, true)
		return server, uri, startAdmittingAuthService(t, uri, serviceSeed, issuer, curve, token, machineID)
	}
	// start runs the listener in the rendered environment, on a port reserved for it, against the
	// server at natsURL, presenting the token presented holds from a file of the test's, or no NATS
	// credential at all when presented is "".
	start := func(t *testing.T, name, natsURL, presented string) *cmdtest.Process {
		t.Helper()
		file := ""
		if presented != "" {
			file = filepath.Join(t.TempDir(), "nats-token")
			require.NoError(t, os.WriteFile(file, []byte(presented), 0o600))
		}
		var env []string
		for variable, value := range rendered {
			text, ok := value.(string)
			require.True(t, ok, "%s is rendered from a Secret, which this machine cannot read", variable)
			switch variable {
			case "PORT":
				text = strconv.Itoa(reservePorts(t, 1)[0])
			case "NATS_URLS":
				text = natsURL
			case bus.ServiceTokenFileEnvVar:
				if file == "" {
					continue
				}
				text = file
			}
			env = append(env, variable+"="+text)
		}
		cmd := exec.Command(binary)
		cmd.Env = env
		return startLogged(t, name, cmd)
	}

	t.Run("the listener presenting its token is admitted as listener/envoy-hub", func(t *testing.T) {
		server, uri, stub := hub(t)
		const name = "the listener with its token"
		listener := start(t, name, uri, token)
		testwait.Until(t, "a connection of the listener's admitted as "+listenerUser, func() ([]hubConnection, bool) {
			connections := hubConnections(t, server, "open")
			if authorizedAs(connections, listenerUser) > 0 {
				return connections, true
			}
			requireRunning(t, listener, name)
			return connections, false
		})
		require.NotZero(t, stub.admitted.Load(), "the stub admitted no request")
		require.Zero(t, stub.failed.Load(), "the stub could not answer a request")
	})

	t.Run("control: another token is refused, and no connection is ever listener/envoy-hub", func(t *testing.T) {
		server, uri, stub := hub(t)
		const name = "the listener with another token"
		listener := start(t, name, uri, unsignedListenerToken("system:serviceaccount:envoy:another-account"))
		testwait.Until(t, "the stub's refusal of the listener's token", func() (int64, bool) {
			requireRunning(t, listener, name)
			refused := stub.refused.Load()
			return refused, refused > 0
		})
		require.Zero(t, authorizedAs(hubConnections(t, server, "all"), listenerUser),
			"a connection was admitted as %s with a token the stub refuses", listenerUser)
		requireRunning(t, listener, name)
		require.Zero(t, stub.admitted.Load(), "the stub admitted a token it was not given")
		require.Zero(t, stub.failed.Load(), "the stub could not answer a request")
	})

	t.Run("control: without NATS_SERVICE_TOKEN_FILE the listener becomes the session user", func(t *testing.T) {
		server, uri, stub := hub(t)
		start(t, "the listener with no token file", uri, "")
		// The listener's connections are named envoy (internal/bus); the server's own probe of
		// its start (testnats.StartConfig) and the stub's connection carry no name.
		connections := testwait.Until(t, "a connection of the listener's as the session user", func() ([]hubConnection, bool) {
			connections := hubConnections(t, server, "all")
			return connections, slices.ContainsFunc(connections, func(c hubConnection) bool {
				return strings.HasPrefix(c.Name, "envoy") && c.AuthorizedUser == keySessions
			})
		})
		require.Zero(t, authorizedAs(connections, listenerUser), "a connection with no credential was admitted as %s", listenerUser)
		require.Zero(t, stub.admitted.Load()+stub.refused.Load()+stub.failed.Load(), "the server asked the auth service about a client with no credential")
	})
}
