package natscallout

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/natscred"
	"github.com/sjawhar/envoy/natsgrant"
)

// The durable consumer the admin gives the notification stream, and the subject of the one message
// it stores there, which the JetStream reader's reads name.
const (
	jsreadDurable = "example-durable"
	jsreadSubject = "notifications.example.stored"
)

// jsreadHub is a lone hub with JetStream behind the auth service, on which an admin static user
// made ENVOY_NOTIFICATIONS (subjects notifications.>), stored one message on it and gave it one
// durable consumer, as the listener and Dispatch give it theirs. The auth service admits the
// services its settings list, reads every platform token without verifying it
// (readUnsignedToken), and counts its decisions in registry.
type jsreadHub struct {
	server   *testnats.Server
	url      string
	registry *metrics.Registry
}

// startJSReadHub starts a jsreadHub whose auth service admits services.
func startJSReadHub(t *testing.T, services []config.NATSService) jsreadHub {
	t.Helper()
	keys := testnats.NewCalloutKeys(t)
	adminSeed, adminKey := testnats.User(t)
	server, url := testnats.StartWithCalloutServer(t, keys, testnats.StaticUser{NKey: adminKey, Publish: []string{">"}, Subscribe: []string{">"}})
	admin, err := testnats.ConnectSeed(t, url, adminSeed)
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	testnats.CreateStreamOn(t, admin, nats.StreamConfig{Name: bus.Stream, Subjects: []string{"notifications.>"}, Storage: nats.MemoryStorage})
	js, err := admin.JetStream()
	require.NoError(t, err)
	_, err = js.Publish(jsreadSubject, []byte("stored"))
	require.NoError(t, err)
	_, err = js.AddConsumer(bus.Stream, &nats.ConsumerConfig{Durable: jsreadDurable, AckPolicy: nats.AckExplicitPolicy})
	require.NoError(t, err)
	registry := metrics.New()
	settings := serviceSettings(t, url, keys)
	settings.Services = services
	startServiceWith(t, settings, Deps{ServiceToken: readUnsignedToken, Metrics: NewMetrics(registry)}, Decide)
	return jsreadHub{server: server, url: url, registry: registry}
}

// authorizedUser is the user server authorized its open client connection cid as, which for a
// connection the auth service admitted is the name its decision gave: nats-server sets the
// connection's user to the callout's user name, and /connz?auth=1 reports it (natscred's test of
// the same name reads it so).
func authorizedUser(t *testing.T, server *testnats.Server, cid uint64) string {
	t.Helper()
	response, err := http.Get(server.MonitorURL() + "/connz?auth=1&cid=" + strconv.FormatUint(cid, 10))
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode, "the hub's /connz")
	var connz struct {
		Connections []struct {
			CID            uint64 `json:"cid"`
			AuthorizedUser string `json:"authorized_user"`
		} `json:"connections"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&connz))
	require.Len(t, connz.Connections, 1, "the hub's open connection %d", cid)
	require.Equal(t, cid, connz.Connections[0].CID, "the hub answered another connection than %d", cid)
	return connz.Connections[0].AuthorizedUser
}

// TestAJSReadServiceReadsStreamStateThroughTheHub: on a hub behind the auth service, the JetStream
// reader's connection, presenting the platform token of the account its jsread entry lists, with
// its inbox prefix set to natsgrant.JSReadInbox, is admitted as jsread/<account> (/connz), and each
// read it makes through nats.go's jetstream is answered: the stream names, the stream list of the
// streams that capture notifications.>, the stream's info, and asked with a subject filter the
// count of its one stored message by subject, its consumers' infos, its consumer names and its
// durable's info. A stream update it publishes and a subscribe to notifications.> are each refused
// by the server, by name. The same token on a connection with nats.go's own inbox prefix is
// admitted and asks the stream names, and no answer reaches it: the server refuses its request
// mux's subscribe to _INBOX.<nuid>.*. The control is the same hub with the jsread entry left out of
// BROKER_NATS_SERVICES, which refuses the reader's connection service_not_listed.
func TestAJSReadServiceReadsStreamStateThroughTheHub(t *testing.T) {
	listed, err := config.ParseNATSServices("jsread/-/*=" + jsreadAccount)
	require.NoError(t, err)

	t.Run("listed", func(t *testing.T) {
		hub := startJSReadHub(t, listed)
		reader := connectService(t, hub.url, hourLong(jsreadAccount), natsgrant.JSReadInbox)
		cid, err := reader.GetClientID()
		require.NoError(t, err)
		require.Equal(t, "jsread/"+jsreadAccount, authorizedUser(t, hub.server, cid))

		js, err := jetstream.New(reader.Conn)
		require.NoError(t, err)
		ctx := testwait.Context(t)
		t.Run("stream names", func(t *testing.T) {
			lister := js.StreamNames(ctx)
			var names []string
			for name := range lister.Name() {
				names = append(names, name)
			}
			require.NoError(t, lister.Err())
			require.Equal(t, []string{bus.Stream}, names)
		})
		t.Run("stream list of the streams that capture notifications.>", func(t *testing.T) {
			lister := js.ListStreams(ctx, jetstream.WithStreamListSubject("notifications.>"))
			var names []string
			for info := range lister.Info() {
				names = append(names, info.Config.Name)
			}
			require.NoError(t, lister.Err())
			require.Equal(t, []string{bus.Stream}, names)
		})
		stream, err := js.Stream(ctx, bus.Stream)
		require.NoError(t, err, "the stream's info got no answer")
		require.Equal(t, []string{"notifications.>"}, stream.CachedInfo().Config.Subjects)
		t.Run("stream info with a subject filter", func(t *testing.T) {
			info, err := stream.Info(ctx, jetstream.WithSubjectFilter("notifications.>"))
			require.NoError(t, err)
			require.Equal(t, map[string]uint64{jsreadSubject: 1}, info.State.Subjects)
		})
		t.Run("consumer list", func(t *testing.T) {
			lister := stream.ListConsumers(ctx)
			var names []string
			for info := range lister.Info() {
				names = append(names, info.Name)
			}
			require.NoError(t, lister.Err())
			require.Equal(t, []string{jsreadDurable}, names)
		})
		t.Run("consumer names", func(t *testing.T) {
			lister := stream.ConsumerNames(ctx)
			var names []string
			for name := range lister.Name() {
				names = append(names, name)
			}
			require.NoError(t, lister.Err())
			require.Equal(t, []string{jsreadDurable}, names)
		})
		t.Run("consumer info", func(t *testing.T) {
			consumer, err := stream.Consumer(ctx, jsreadDurable)
			require.NoError(t, err)
			info := consumer.CachedInfo()
			require.Equal(t, jsreadDurable, info.Name)
			require.EqualValues(t, 1, info.NumPending)
		})

		t.Run("refused: a stream update", func(t *testing.T) {
			update := `{"name":"` + bus.Stream + `","subjects":["notifications.>","example.>"]}`
			require.NoError(t, reader.Publish("$JS.API.STREAM.UPDATE."+bus.Stream, []byte(update)))
			require.NoError(t, reader.Flush())
			want := `Permissions Violation for Publish to "$JS.API.STREAM.UPDATE.` + bus.Stream + `"`
			testwait.Eventually(t, want, func() bool { return reader.errs.seen(want) })
		})
		t.Run("refused: a subscribe to notifications.>", func(t *testing.T) {
			_, err := reader.SubscribeSync("notifications.>")
			require.NoError(t, err)
			require.NoError(t, reader.Flush())
			want := `Permissions Violation for Subscription to "notifications.>"`
			testwait.Eventually(t, want, func() bool { return reader.errs.seen(want) })
		})

		t.Run("without its inbox prefix, the stream names get no answer", func(t *testing.T) {
			plain := connectService(t, hub.url, hourLong(jsreadAccount), "")
			plainJS, err := jetstream.New(plain.Conn)
			require.NoError(t, err)
			// The request's own bound: no answer comes, so the call ends at it.
			short, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			lister := plainJS.StreamNames(short)
			var names []string
			for name := range lister.Name() {
				names = append(names, name)
			}
			require.Empty(t, names, "the stream names reached a reader whose replies come under nats.go's own inbox")
			require.ErrorIs(t, lister.Err(), context.DeadlineExceeded)
			want := `Permissions Violation for Subscription to "` + nats.InboxPrefix
			testwait.Eventually(t, want, func() bool { return plain.errs.seen(want) })
		})
	})

	t.Run("control: the jsread entry left out of BROKER_NATS_SERVICES", func(t *testing.T) {
		hub := startJSReadHub(t, nil)
		option, _ := natscred.Service(hourLong(jsreadAccount))
		nc, err := nats.Connect(hub.url, option, nats.CustomInboxPrefix(natsgrant.JSReadInbox), nats.NoReconnect(), nats.Timeout(5*time.Second))
		if err == nil {
			nc.Close()
		}
		require.ErrorIs(t, err, nats.ErrAuthorization, "the hub admitted the reader with no jsread entry listed")
		testwait.Eventually(t, "the reader's refusal counted "+labelServiceNotListed, func() bool {
			return refusedFor(t, hub.registry, labelServiceNotListed) == 1
		})
	})
}
