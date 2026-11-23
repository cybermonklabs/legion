package testnats_test

import (
	"sync/atomic"
	"testing"

	natsserver "github.com/nats-io/nats-server/v2/server"
	natsgo "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The callout fixture sends the authorization of a client it has no static user for to the auth
// service, encrypted to the service's key, and admits a listed static user without asking.
func TestTheCalloutFixtureSendsUnknownClientsToTheService(t *testing.T) {
	keys := testnats.NewCalloutKeys(t)
	staticSeed, staticKey := testnats.User(t)
	url := testnats.StartWithCallout(t, keys, testnats.StaticUser{
		NKey: staticKey, Publish: []string{"example.>"}, Subscribe: []string{"example.>"},
	})
	requireCalloutRequests(t, url, keys, staticSeed)
}

// requireCalloutRequests connects to url as the auth service (keys.ServiceSeed) and counts the
// authorization requests the server sends it, refusing each: a client with no credential is one
// request, and the static user whose seed is staticSeed is admitted with none.
func requireCalloutRequests(t *testing.T, url string, keys testnats.CalloutKeys, staticSeed string) {
	t.Helper()
	service, err := testnats.ConnectSeed(t, url, keys.ServiceSeed)
	require.NoError(t, err, "the service's own user is a static auth user, admitted without a callout")
	defer service.Close()
	var requests, encrypted atomic.Int32
	_, err = service.Subscribe(natsserver.AuthCalloutSubject, func(msg *natsgo.Msg) {
		requests.Add(1)
		if msg.Header.Get(natsserver.AuthRequestXKeyHeader) != "" {
			encrypted.Add(1)
		}
		// An empty answer is the server's "no reason supplied" refusal, so the client is refused
		// at once rather than at the authorization timeout, through the service's own publish
		// grant on the server's reply inbox.
		_ = msg.Respond(nil)
	})
	require.NoError(t, err)
	require.NoError(t, service.Flush())

	unknown, err := testnats.ConnectSeed(t, url, "")
	if err == nil {
		unknown.Close()
	}
	require.ErrorIs(t, err, natsgo.ErrAuthorization, "a client with no credential is admitted only by the service's answer, and it refused")
	testwait.Eventually(t, "the authorization request for the client with no credential", func() bool {
		return requests.Load() == 1
	})
	require.EqualValues(t, 1, encrypted.Load(), "the request carries the server's xkey: it is sealed to the service's")

	static, err := testnats.ConnectSeed(t, url, staticSeed)
	require.NoError(t, err, "a listed static user is admitted by the server alone")
	static.Close()
	require.NoError(t, service.Flush())
	require.EqualValues(t, 1, requests.Load(), "the static user's connection sent the service a request")
}
