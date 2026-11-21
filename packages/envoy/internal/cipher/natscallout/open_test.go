package natscallout

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/natsgrant"
)

// emptyDirections are the two grants that open a direction: one with no publish allow list and
// one with no subscribe allow list. Either would admit a user who may publish, or subscribe, to
// every subject (natsgrant.OpenDirection).
var emptyDirections = map[string]natsgrant.Permissions{
	"publish":   {Subscribe: []string{"_INBOX.>"}},
	"subscribe": {Publish: []string{"_INBOX.>"}},
}

// answering is a decideFunc that admits every request with grant, so a test hands the answer a
// grant of its own and only the guard in admit stands between that grant and the server.
func answering(grant natsgrant.Permissions) decideFunc {
	return func(context.Context, Deps, []config.NATSService, Request) (Decision, error) {
		return Decision{Name: "session/test", Grant: grant, Expires: time.Now().Add(time.Hour)}, nil
	}
}

// A grant whose list holds only a blank entry is the same fault by another road. jwt drops an
// empty entry when it adds it to a user (StringList.Add skips ""), so a list of "" encodes as an
// empty one; a whitespace entry names no subject any client could be granted. OpenDirection counts
// both as an open direction, so each is refused naming the direction, and no user is encoded.
func TestAGrantOfBlankEntriesIsRefused(t *testing.T) {
	issuer, err := nkeys.CreateAccount()
	require.NoError(t, err)
	user, err := nkeys.CreateUser()
	require.NoError(t, err)
	userKey, err := user.PublicKey()
	require.NoError(t, err)
	for direction, grant := range map[string]natsgrant.Permissions{
		"publish":   {Publish: []string{""}, Subscribe: []string{"_INBOX.>"}},
		"subscribe": {Publish: []string{"_INBOX.>"}, Subscribe: []string{" "}},
	} {
		t.Run(direction, func(t *testing.T) {
			encoded, err := encodeAnswer(issuer, Request{UserNkey: userKey, ServerID: "NSERVER"},
				Decision{Name: "session/ses_a", Grant: grant, Expires: time.Now().Add(time.Hour)}, nil)
			require.NoError(t, err)
			response, err := jwt.DecodeAuthorizationResponseClaims(encoded)
			require.NoError(t, err)
			require.Equal(t, "grant opens every subject: its "+direction+" allow list is empty", response.Error)
			require.Empty(t, response.Jwt, "a user was encoded whose %s allow list is empty", direction)
		})
	}
}

// A grant with an empty allow list in either direction is refused by the guard every answer passes
// (admit), naming the direction, and no user is encoded for it.
func TestAGrantWithAnEmptyDirectionIsRefused(t *testing.T) {
	issuer, err := nkeys.CreateAccount()
	require.NoError(t, err)
	user, err := nkeys.CreateUser()
	require.NoError(t, err)
	userKey, err := user.PublicKey()
	require.NoError(t, err)
	for direction, grant := range emptyDirections {
		t.Run(direction, func(t *testing.T) {
			encoded, err := encodeAnswer(issuer, Request{UserNkey: userKey, ServerID: "NSERVER"},
				Decision{Name: "session/ses_a", Grant: grant, Expires: time.Now().Add(time.Hour)}, nil)
			require.NoError(t, err)
			response, err := jwt.DecodeAuthorizationResponseClaims(encoded)
			require.NoError(t, err)
			require.Equal(t, "grant opens every subject: its "+direction+" allow list is empty", response.Error)
			require.Empty(t, response.Jwt, "a user was encoded for a grant that opens its %s direction", direction)
			_, refused := admit(Request{UserNkey: userKey}, Decision{Grant: grant}, nil)
			require.Equal(t, labelOpenDirection, refused.Label)
		})
	}
	t.Run("every grant natsgrant builds closes both directions", func(t *testing.T) {
		session, err := natsgrant.Session("ses_a")
		require.NoError(t, err)
		edge, err := natsgrant.Edge("edge-a", []string{"PRIV"})
		require.NoError(t, err)
		for name, grant := range map[string]natsgrant.Permissions{
			"Session": session, "Daemon": natsgrant.Daemon(), "Measure": natsgrant.Measure(),
			"Listener": natsgrant.Listener("example-hub"), "Dispatch": natsgrant.Dispatch("example-dispatch"),
			"Host": natsgrant.Host("example-host-a"), "ListenerHost": natsgrant.ListenerHost("example-hub"),
			"Edge": edge,
		} {
			direction, open := natsgrant.OpenDirection(grant)
			require.False(t, open, "%s opens its %s direction", name, direction)
		}
	})
}

// On a running server, a user answered with an empty publish allow list may publish to every
// subject — which is why the guard exists — and the service refuses that grant. The control runs
// first: a stand-in service holding the service's keys answers with exactly that user, and a
// message the user publishes on a subject no grant of natsgrant names reaches a subscriber. Then
// the real service, its decision replaced by one answering that grant, refuses it in admit, the
// guard every answer passes.
func TestAnEmptyAllowListIsRefused(t *testing.T) {
	requireAnEmptyPublishListRefused(t, func(keys testnats.CalloutKeys, static ...testnats.StaticUser) string {
		return testnats.StartWithCallout(t, keys, static...)
	})
}

func requireAnEmptyPublishListRefused(t *testing.T, start func(testnats.CalloutKeys, ...testnats.StaticUser) string) {
	t.Helper()
	keys := testnats.NewCalloutKeys(t)
	readerSeed, readerKey := testnats.User(t)
	url := start(keys, testnats.StaticUser{NKey: readerKey, Publish: []string{"_INBOX.>"}, Subscribe: []string{"secret.>"}})
	reader, err := testnats.ConnectSeed(t, url, readerSeed)
	require.NoError(t, err)
	defer reader.Close()
	secret, err := reader.SubscribeSync("secret.>")
	require.NoError(t, err)
	require.NoError(t, reader.Flush())

	t.Run("control: the server lets an empty publish list publish anywhere", func(t *testing.T) {
		standIn := standInGranting(t, url, keys, nil, []string{"_INBOX.>"})
		defer standIn.Close()
		nc, err := nats.Connect(url, nats.Token("anything"), nats.NoReconnect(), nats.Timeout(5*time.Second))
		require.NoError(t, err, "the stand-in's empty-publish user was not admitted")
		defer nc.Close()
		require.NoError(t, nc.Publish("secret.read", []byte("read")))
		require.NoError(t, nc.Flush())
		msg, err := secret.NextMsg(5 * time.Second)
		require.NoError(t, err, "the empty-publish user's message never arrived: the server restricted it, so the guard guards nothing")
		require.Equal(t, "read", string(msg.Data))
	})

	t.Run("the service refuses the same grant", func(t *testing.T) {
		a := newSession(t, "ses_a", 15*time.Minute)
		service := startService(t, url, keys, Deps{Introspect: introspector(a)}, answering(emptyDirections["publish"]))
		t.Cleanup(service.Close)
		_, err := connectSession(t, url, a, nil)
		require.ErrorIs(t, err, nats.ErrAuthorization, "the service admitted a user that may publish to every subject")
		if msg, err := secret.NextMsg(500 * time.Millisecond); err == nil {
			t.Fatalf("a message reached the subscriber after the refusal: %q", msg.Data)
		}
	})
}

// standInGranting answers every authorization request with a user whose allow lists are publish
// and subscribe, as given and unchecked, signed with the service's own keys, so the server reads
// it as the service's answer: a grant the service's guard refuses, delivered past it.
func standInGranting(t *testing.T, url string, keys testnats.CalloutKeys, publish, subscribe []string) *nats.Conn {
	t.Helper()
	issuer, err := nkeys.FromSeed([]byte(keys.IssuerSeed))
	require.NoError(t, err)
	xkey, err := nkeys.FromSeed([]byte(keys.XKeySeed))
	require.NoError(t, err)
	conn, err := testnats.ConnectSeed(t, url, keys.ServiceSeed)
	require.NoError(t, err)
	_, err = conn.QueueSubscribe(authSubject, queueGroup, func(msg *nats.Msg) {
		serverXKey := msg.Header.Get(serverXKeyHeader)
		data := msg.Data
		if serverXKey != "" {
			opened, err := xkey.Open(data, serverXKey)
			if err != nil {
				return
			}
			data = opened
		}
		claims, err := jwt.DecodeAuthorizationRequestClaims(string(data))
		if err != nil {
			return
		}
		user := jwt.NewUserClaims(claims.UserNkey)
		user.Audience = globalAccount
		user.Pub.Allow.Add(publish...)
		user.Sub.Allow.Add(subscribe...)
		userJWT, err := user.Encode(issuer)
		if err != nil {
			return
		}
		response := jwt.NewAuthorizationResponseClaims(claims.UserNkey)
		response.Audience = claims.Server.ID
		response.Jwt = userJWT
		encoded, err := response.Encode(issuer)
		if err != nil {
			return
		}
		out := []byte(encoded)
		if serverXKey != "" {
			if out, err = xkey.Seal(out, serverXKey); err != nil {
				return
			}
		}
		_ = msg.Respond(out)
	})
	require.NoError(t, err)
	require.NoError(t, conn.Flush())
	return conn
}
