package testnats_test

import (
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/nats-io/jwt/v2"
	natsserver "github.com/nats-io/nats-server/v2/server"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
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

// Every member of a callout cluster sends the authorization of a client it has no static user for
// to the auth service, wherever the service's own connection is, sealed to the service's key, and
// admits a listed static user without asking; and the members share JetStream, clustered: a
// stream of three copies made through one member holds a copy on each. It fails when a member
// admits a client it should send to the service (the server enforces no callout), when a request
// from a member the service is not connected to never reaches it (the members are not routed), or
// when the stream cannot hold three copies (JetStream is not clustered across them).
func TestTheCalloutClusterSendsUnknownClientsToTheServiceFromEveryMember(t *testing.T) {
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
	js, err := static.JetStream(testnats.MaxWait(t))
	require.NoError(t, err)
	_, err = js.AddStream(&natsgo.StreamConfig{Name: "THREE", Subjects: []string{"example.three"}, Replicas: 3})
	require.NoError(t, err)
	info := testnats.AwaitSettledStream(t, js, "THREE")
	require.Equal(t, 3, testnats.Copies(info.Cluster))
}

// requireCalloutRequests connects to url as the auth service (keys.ServiceSeed) and reads the
// authorization requests the server sends it, refusing each: a client with no credential is one
// request for each of its tries that reached the server, and the static user whose seed is
// staticSeed is admitted with none.
//
// The client with no credential connects through the shared wait (AwaitConnectSeed), which makes
// a try again when nats.go's own timeout ends one, and a try that timed out after its CONNECT
// reached the server was asked about all the same. So each try carries a name of its own
// (connectTries), which the server copies into the try's request, and the requests are counted by
// the try each names: the try the service refused reached the server, which asked the service
// before it refused, so it is asked about exactly once; a try that timed out is asked about at most
// once, and not at all when it timed out before its CONNECT reached the server, which nothing the
// row reads tells apart; no request names anything else, the static user's connection, which
// carries no name, among them. With the one try an unloaded connect makes, that is exactly one
// request, sealed to the service's xkey.
func requireCalloutRequests(t *testing.T, url string, keys testnats.CalloutKeys, staticSeed string) {
	t.Helper()
	service, err := testnats.AwaitConnectSeed(t, url, keys.ServiceSeed)
	require.NoError(t, err, "the service's own user is a static auth user, admitted without a callout")
	defer service.Close()
	xkey, err := nkeys.FromCurveSeed([]byte(keys.XKeySeed))
	require.NoError(t, err, "read the service's xkey")
	var mu sync.Mutex
	var requests []calloutRequest
	received := func() []calloutRequest {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(requests)
	}
	_, err = service.Subscribe(natsserver.AuthCalloutSubject, func(msg *natsgo.Msg) {
		request := readCalloutRequest(xkey, msg)
		mu.Lock()
		requests = append(requests, request)
		mu.Unlock()
		// An empty answer is the server's "no reason supplied" refusal, so the client is refused
		// at once rather than at the authorization timeout, through the service's own publish
		// grant on the server's reply inbox.
		_ = msg.Respond(nil)
	})
	require.NoError(t, err)
	require.NoError(t, service.FlushWithContext(testnats.Context(t)))

	tries := &connectTries{prefix: "no-credential"}
	unknown, err := testnats.AwaitConnectSeed(t, url, "", tries.option())
	if err == nil {
		unknown.Close()
	}
	require.ErrorIs(t, err, natsgo.ErrAuthorization, "a client with no credential is admitted only by the service's answer, and it refused")
	refused := tries.name(tries.made)
	testwait.Until(t, fmt.Sprintf("the authorization request for %s, the try of the client with no credential the service refused", refused), func() ([]calloutRequest, bool) {
		got := received()
		return got, slices.ContainsFunc(got, func(request calloutRequest) bool { return request.Name == refused })
	})

	static, err := testnats.AwaitConnectSeed(t, url, staticSeed)
	require.NoError(t, err, "a listed static user is admitted by the server alone")
	static.Close()
	require.NoError(t, service.FlushWithContext(testnats.Context(t)))
	got := received()
	asked := map[string]int{}
	for _, request := range got {
		if !request.Sealed || request.Err != nil {
			t.Fatalf("a request (sealed %t, read %v) is not one the server sealed to the service's xkey with its own in the header; every request: %+v", request.Sealed, request.Err, got)
		}
		asked[request.Name]++
	}
	for try := 1; try <= tries.made; try++ {
		name := tries.name(try)
		want, held := "at most once, as a try that timed out may have timed out before its CONNECT reached the server", asked[name] <= 1
		if name == refused {
			want, held = "once", asked[name] == 1
		}
		if !held {
			t.Errorf("the service was asked about %s, try %d of %d of the client with no credential, %d time(s), want %s; every request: %+v", name, try, tries.made, asked[name], want, got)
		}
		delete(asked, name)
	}
	for name, times := range asked {
		t.Errorf("the service was asked %d time(s) about a connection named %q, which is no try of the client with no credential: the static user's connection, which carries no name, or another sent the service a request; every request: %+v", times, name, got)
	}
}

// calloutRequest is what the auth service read of one authorization request: the name of the
// connection it asks about, which connectTries gives each try; whether the server sealed it to the
// service's xkey, naming its own in the header; and what opening or decoding it answered.
type calloutRequest struct {
	Name   string
	Sealed bool
	Err    error
}

// readCalloutRequest opens msg, an authorization request, with the service's xkey when the server
// sealed it, and reads the connection's name from the request's claims (ConnectOptions.Name,
// which nats-server v2.15.1 copies from the client's CONNECT: auth_callout.go fillConnectOpts).
func readCalloutRequest(xkey nkeys.KeyPair, msg *natsgo.Msg) calloutRequest {
	server := msg.Header.Get(natsserver.AuthRequestXKeyHeader)
	request := calloutRequest{Sealed: server != ""}
	token := msg.Data
	if request.Sealed {
		opened, err := xkey.Open(msg.Data, server)
		if err != nil {
			request.Err = fmt.Errorf("open the request with the service's xkey: %w", err)
			return request
		}
		token = opened
	}
	claims, err := jwt.DecodeAuthorizationRequestClaims(string(token))
	if err != nil {
		request.Err = fmt.Errorf("decode the request's claims: %w", err)
		return request
	}
	request.Name = claims.ConnectOptions.Name
	return request
}

// connectTries names each try of a connect made through the shared wait. AwaitConnectSeed applies
// the options it is given anew at each try, as natsgo.Connect applies each option once, so the
// option's func runs once a try, on the test goroutine the wait asks on: it counts the try in made
// and names the try's connection prefix-<its number>.
type connectTries struct {
	prefix string
	made   int
}

// option is the natsgo option that counts and names each try.
func (c *connectTries) option() natsgo.Option {
	return func(options *natsgo.Options) error {
		c.made++
		options.Name = c.name(c.made)
		return nil
	}
}

// name is the name of the connect's try-th try.
func (c *connectTries) name(try int) string { return fmt.Sprintf("%s-%d", c.prefix, try) }
