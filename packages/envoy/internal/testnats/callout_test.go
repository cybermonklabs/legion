package testnats_test

import (
	"sync/atomic"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	natsgo "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The callout fixture sends the authorization of a client it has no static user for to the auth
// service, encrypted to the service's key, and admits a listed static user without asking.
func TestTheCalloutFixtureSendsUnknownClientsToTheService(t *testing.T) {
	t.Parallel()
	keys := testnats.NewCalloutKeys(t)
	staticSeed, staticKey := testnats.User(t)
	url := testnats.StartWithCallout(t, keys, testnats.StaticUser{
		NKey: staticKey, Publish: []string{"example.>"}, Subscribe: []string{"example.>"},
	})
	requireCalloutRequests(t, url, keys, staticSeed)
}

// Every member of a callout cluster sends the authorization of a client it has no static user for
// to the auth service, wherever the service's own connection is, sealed to the service's key, and
// admits a listed static user without asking; and the members share JetStream, clustered: a
// stream of three copies made through one member holds a copy on each. It fails when a member
// admits a client it should send to the service (the server enforces no callout), when a request
// from a member the service is not connected to never reaches it (the members are not routed), or
// when the stream cannot hold three copies (JetStream is not clustered across them).
func TestTheCalloutClusterSendsUnknownClientsToTheServiceFromEveryMember(t *testing.T) {
	t.Parallel()
	keys := testnats.NewCalloutKeys(t)
	staticSeed, staticKey := testnats.User(t)
	cluster := testnats.StartClusterWithCallout(t, 3, keys, testnats.StaticUser{
		NKey: staticKey, Publish: []string{"$JS.API.>", "example.>"}, Subscribe: []string{"_INBOX.>", "example.>"},
	})
	require.Len(t, cluster.Servers, 3)
	service, err := testnats.ConnectSeed(t, cluster.Servers[0].URL(), keys.ServiceSeed)
	require.NoError(t, err, "the service's own user is a static auth user, admitted without a callout")
	defer service.Close()
	var requests, encrypted atomic.Int32
	_, err = service.Subscribe(natsserver.AuthCalloutSubject, func(msg *natsgo.Msg) {
		requests.Add(1)
		if msg.Header.Get(natsserver.AuthRequestXKeyHeader) != "" {
			encrypted.Add(1)
		}
		_ = msg.Respond(nil)
	})
	require.NoError(t, err)
	require.NoError(t, service.Flush())

	for i, url := range cluster.URLs() {
		before := requests.Load()
		unknown, err := testnats.ConnectSeed(t, url, "")
		if err == nil {
			unknown.Close()
		}
		require.ErrorIs(t, err, natsgo.ErrAuthorization, "member %s admitted a client with no credential", cluster.Name(i))
		testwait.Eventually(t, "the authorization request from member "+cluster.Name(i), func() bool {
			return requests.Load() == before+1
		})
		static, err := testnats.ConnectSeed(t, url, staticSeed)
		require.NoError(t, err, "member %s refused its static user", cluster.Name(i))
		static.Close()
	}
	require.NoError(t, service.Flush())
	require.EqualValues(t, 3, requests.Load(), "the static user's connections sent the service a request")
	require.EqualValues(t, 3, encrypted.Load(), "a member's request was not sealed to the service's key")

	static, err := testnats.ConnectSeed(t, cluster.URL(), staticSeed)
	require.NoError(t, err)
	defer static.Close()
	// The context's 30 s is the step of AwaitSettledStream's reads, each of which awaitInfo sends
	// again when the step cuts it. The create is sent once and a cut fails the row, so it waits to
	// the test's deadline (testnats.MaxWait, which overrides the context's wait for this call).
	js, err := static.JetStream(natsgo.MaxWait(30 * time.Second))
	require.NoError(t, err)
	_, err = js.AddStream(&natsgo.StreamConfig{Name: "THREE", Subjects: []string{"example.three"}, Replicas: 3}, testnats.MaxWait(t))
	require.NoError(t, err)
	info := testnats.AwaitSettledStream(t, js, "THREE")
	require.Equal(t, 3, testnats.Copies(info.Cluster))
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
