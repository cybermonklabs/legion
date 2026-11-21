//go:build natslive

package core_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/jwt/v2"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The tests in this file run the rendered callout block on the hub's image (testnats.HubImage, the
// nats-server the hub runs), with an auth service of their own answering the server's requests as
// dispatch://INFRA-840's will: it admits a client presenting sessionToken into the global account,
// $G, with one session's grant, and refuses every other.

const (
	// sessionToken is the one token the test's auth service admits.
	sessionToken = "example-session-token"
	// deliverS1 and deliverS2 are two sessions' delivery channels: the admitted client holds the
	// first and may not subscribe to the second.
	deliverS1 = "notifications.envoy.deliver.S1"
	deliverS2 = "notifications.envoy.deliver.S2"
	// exampleStream is the stream written before the callout is configured.
	exampleStream = "EXAMPLE"
	// streamMessages is how many messages the stream holds before the callout is configured.
	streamMessages = 51
)

type calloutRig struct {
	staticSeed, serviceSeed string
	issuer, curve           nkeys.KeyPair
	authorization           core.Authorization
	requests                atomic.Int64
}

func newCalloutRig(t *testing.T, session bool) *calloutRig {
	t.Helper()
	staticSeed, staticKey := testnats.User(t)
	serviceSeed, serviceKey := testnats.User(t)
	issuer, err := nkeys.CreateAccount()
	require.NoError(t, err)
	issuerKey, err := issuer.PublicKey()
	require.NoError(t, err)
	curve, err := nkeys.CreateCurveKeys()
	require.NoError(t, err)
	curveKey, err := curve.PublicKey()
	require.NoError(t, err)
	a := core.Authorization{
		Users: []core.User{
			{
				Name:      "example-static",
				NKey:      staticKey,
				Publish:   &core.Rules{Allow: []string{"$JS.API.>", "example.>", "notifications.envoy.deliver.>"}},
				Subscribe: &core.Rules{Allow: []string{"_INBOX.>"}},
				HasSeed:   true,
			},
			{
				Name:      "example-auth",
				NKey:      serviceKey,
				Publish:   &core.Rules{Allow: []string{"$SYS._INBOX.>"}},
				Subscribe: &core.Rules{Allow: []string{"$SYS.REQ.USER.AUTH"}},
				HasSeed:   true,
			},
		},
		Callout: &core.AuthCallout{Issuer: issuerKey, XKey: curveKey, ServiceUser: "example-auth"},
	}
	if session {
		_, sessionKey := testnats.User(t)
		a.Users = append(a.Users, core.User{
			Name:      "example-sessions",
			NKey:      sessionKey,
			Publish:   &core.Rules{Allow: []string{"sessions.>"}},
			Subscribe: &core.Rules{Allow: []string{"_INBOX.>", "sessions.>"}},
		})
		a.NoAuthUser = "example-sessions"
	}
	return &calloutRig{staticSeed: staticSeed, serviceSeed: serviceSeed, issuer: issuer, curve: curve, authorization: a}
}

func (r *calloutRig) render(t *testing.T, callout bool) string {
	t.Helper()
	a := r.authorization
	if !callout {
		a.Callout = nil
	}
	text, err := a.Render(header)
	require.NoError(t, err)
	return text
}

// hubArgs is the hub's command line after its configuration: JetStream on.
var hubArgs = []string{"-js"}

// startOn runs the hub's image on config, with the rig's auth service, which holds each answer
// for answerDelay before it gives it, and returns the client URL once the service's user is
// connected. The container starts on config itself, callout and all, with no restart
// (testnats.StartImageReady says why).
func (r *calloutRig) startOn(t *testing.T, config string, answerDelay time.Duration) string {
	t.Helper()
	_, uri := testnats.StartImageReady(t, testnats.HubImage, config, hubArgs)
	r.serve(t, uri, answerDelay)
	return uri
}

// connectWhenUp connects to server as the nkey user whose seed is seed, with options, retrying for
// 30 s while the server comes up behind its published port or a proxy, and closes the connection
// when the test ends. testnats.ConnectSeed sets NoReconnect, so a connection the server closes
// stays closed.
func connectWhenUp(t *testing.T, server, seed string, options ...natsgo.Option) *natsgo.Conn {
	t.Helper()
	var conn *natsgo.Conn
	var err error
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if conn, err = testnats.ConnectSeed(t, server, seed, options...); err == nil {
			break
		}
		require.True(t, time.Now().Before(deadline), "could not connect to %s within 30 s: %v", server, err)
	}
	t.Cleanup(conn.Close)
	return conn
}

// serve starts the auth service on the server at uri, which holds each answer for answerDelay
// before it gives it, and returns once the service's user is connected. The service's user is in
// auth_users, so it connects without a callout.
func (r *calloutRig) serve(t *testing.T, uri string, answerDelay time.Duration) {
	t.Helper()
	conn := connectWhenUp(t, uri, r.serviceSeed)
	_, err := conn.Subscribe("$SYS.REQ.USER.AUTH", func(m *natsgo.Msg) {
		time.Sleep(answerDelay)
		r.answer(t, m)
	})
	require.NoError(t, err)
	require.NoError(t, conn.Flush())
}

// answer is the auth service's answer to one request: the request is sealed to the callout's xkey
// and the answer to the server's, both signed as the server requires. A client presenting
// sessionToken is admitted into $G, the user JWT's audience, which config mode reads as the
// account to place it in, with a grant on deliverS1 alone; every other client is refused.
func (r *calloutRig) answer(t *testing.T, m *natsgo.Msg) {
	r.requests.Add(1)
	request, err := r.curve.Open(m.Data, m.Header.Get("Nats-Server-Xkey"))
	if err != nil {
		t.Errorf("open the sealed authorization request: %v", err)
		return
	}
	claims, err := jwt.DecodeAuthorizationRequestClaims(string(request))
	if err != nil {
		t.Errorf("decode the authorization request: %v", err)
		return
	}
	response := jwt.NewAuthorizationResponseClaims(claims.UserNkey)
	response.Audience = claims.Server.ID
	if claims.ConnectOptions.Token == sessionToken {
		user := jwt.NewUserClaims(claims.UserNkey)
		user.Audience = "$G"
		user.Pub.Allow.Add("_INBOX.>")
		user.Sub.Allow.Add("_INBOX.>", deliverS1)
		if response.Jwt, err = user.Encode(r.issuer); err != nil {
			t.Errorf("sign the user JWT: %v", err)
			return
		}
	} else {
		response.Error = "example: no token this auth service admits"
	}
	token, err := response.Encode(r.issuer)
	if err != nil {
		t.Errorf("sign the authorization response: %v", err)
		return
	}
	sealed, err := r.curve.Seal([]byte(token), claims.Server.XKey)
	if err != nil {
		t.Errorf("seal the authorization response: %v", err)
		return
	}
	if err := m.Respond(sealed); err != nil {
		t.Errorf("answer the authorization request: %v", err)
	}
}

func (r *calloutRig) requireRequests(t *testing.T, want int64, why string) {
	t.Helper()
	require.Equal(t, want, r.requests.Load(), why)
}

// The rendered callout block on the hub's image: the hub keeps its global account, so turning the
// callout on needs no move, and the server sends to the auth service exactly the clients that are
// not its static users.
func TestAuthCalloutOnTheHubServer(t *testing.T) {
	// The callout is turned on over the same JetStream store: the first container is stopped and a
	// fresh one starts on the store with the callout configured, on a fresh address, as an
	// operator's rollout replaces the hub's task (testnats.ImageStore).
	t.Run("a stream written before the callout reads whole after it", func(t *testing.T) {
		rig := newCalloutRig(t, false)
		store := testnats.ImageStore(t)
		ctr, uri := testnats.StartImageReady(t, testnats.HubImage, rig.render(t, false), hubArgs, store...)
		// Every first connect of this rig goes through connectWhenUp, which retries; the second
		// container's connects below follow the auth service's own, which does the same (serve).
		conn := connectWhenUp(t, uri, rig.staticSeed)
		js, err := conn.JetStream()
		require.NoError(t, err)
		_, err = js.AddStream(&natsgo.StreamConfig{Name: exampleStream, Subjects: []string{"example.>"}})
		require.NoError(t, err)
		for i := range streamMessages {
			_, err := js.Publish("example.message", []byte{byte(i)})
			require.NoError(t, err)
		}
		conn.Close()
		stop := 10 * time.Second
		require.NoError(t, ctr.Stop(context.Background(), &stop))

		_, uri = testnats.StartImageReady(t, testnats.HubImage, rig.render(t, true), hubArgs, store...)
		rig.serve(t, uri, 0)
		conn, err = testnats.ConnectSeed(t, uri, rig.staticSeed)
		require.NoError(t, err)
		t.Cleanup(conn.Close)
		js, err = conn.JetStream()
		require.NoError(t, err)
		info, err := js.StreamInfo(exampleStream)
		require.NoError(t, err)
		require.Equal(t, uint64(streamMessages), info.State.Msgs)
		for seq := uint64(1); seq <= streamMessages; seq++ {
			msg, err := js.GetMsg(exampleStream, seq)
			require.NoError(t, err)
			require.Equal(t, []byte{byte(seq - 1)}, msg.Data)
		}
		rig.requireRequests(t, 0, "the static user is in auth_users, so its connections reach no callout")
	})

	t.Run("with the callout", func(t *testing.T) {
		rig := newCalloutRig(t, false)
		uri := rig.startOn(t, rig.render(t, true), 0)

		t.Run("a token client is placed in the global account and its grant holds", func(t *testing.T) {
			before := rig.requests.Load()
			client, errs := connect(t, uri, "", natsgo.Token(sessionToken))
			rig.requireRequests(t, before+1, "a client that is no static user is sent to the callout")
			sub, err := client.SubscribeSync(deliverS1)
			require.NoError(t, err)
			require.NoError(t, client.Flush())

			static, _ := connect(t, uri, rig.staticSeed)
			require.NoError(t, static.Publish(deliverS1, []byte("for S1")))
			require.NoError(t, static.Flush())
			msg, err := sub.NextMsg(5 * time.Second)
			require.NoError(t, err, "the token client shares the static users' account, so it receives their messages")
			require.Equal(t, "for S1", string(msg.Data))

			_, err = client.SubscribeSync(deliverS2)
			require.NoError(t, err)
			requireViolation(t, client, errs, `Subscription to "`+deliverS2+`"`)
		})
		t.Run("a static user makes no callout request and keeps its static grant", func(t *testing.T) {
			before := rig.requests.Load()
			static, errs := connect(t, uri, rig.staticSeed)
			require.NoError(t, static.Publish("example.allowed", nil))
			require.NoError(t, static.Publish("outside.example", nil))
			requireViolation(t, static, errs, `Publish to "outside.example"`)
			rig.requireRequests(t, before, "a user listed in auth_users is admitted by its nkey alone")
		})
		t.Run("a wrong token is refused", func(t *testing.T) {
			before := rig.requests.Load()
			_, err := testnats.ConnectSeed(t, uri, "", natsgo.Token("example-wrong-token"))
			require.ErrorIs(t, err, natsgo.ErrAuthorization)
			rig.requireRequests(t, before+1, "the auth service refused it")
		})
		t.Run("a client with no credential is refused", func(t *testing.T) {
			before := rig.requests.Load()
			_, err := testnats.ConnectSeed(t, uri, "")
			require.ErrorIs(t, err, natsgo.ErrAuthorization)
			rig.requireRequests(t, before+1, "with no noAuthUser, a client with no credential is the auth service's to admit")
		})
	})

	// The control that auth_users must be derived from users: the same text with the static user's
	// key edited out of auth_users sends that user to the callout although its nkey signature is
	// valid, and the auth service, which knows no such client, refuses it.
	t.Run("a keyed user left out of auth_users is sent to the callout and refused", func(t *testing.T) {
		rig := newCalloutRig(t, false)
		staticKey := rig.authorization.Users[0].NKey
		text := rig.render(t, true)
		edited := strings.Replace(text, `auth_users: ["`+staticKey+`", `, `auth_users: [`, 1)
		require.NotEqual(t, text, edited, "the edit must take the static user's key out of auth_users")
		uri := rig.startOn(t, edited, 0)
		_, err := testnats.ConnectSeed(t, uri, rig.staticSeed)
		require.ErrorIs(t, err, natsgo.ErrAuthorization)
		rig.requireRequests(t, 1, "the static user was sent to the callout")
	})

	t.Run("with a noAuthUser, a client with no credential becomes it with no callout request", func(t *testing.T) {
		rig := newCalloutRig(t, true)
		uri := rig.startOn(t, rig.render(t, true), 0)
		client, errs := connect(t, uri, "")
		require.NoError(t, client.Publish("sessions.allowed", nil))
		require.NoError(t, client.Publish("example.static-only", nil))
		requireViolation(t, client, errs, `Publish to "example.static-only"`)
		rig.requireRequests(t, 0, "the session user is in auth_users, so a client becoming it reaches no callout")
	})
}
