package natscallout

import (
	"strconv"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/natscred"
)

// envoyAccount is the account a private render keeps every client in, and jetStreamAccount the one
// it keeps JetStream in, which no client sits in (plan C's t-c-29; packages/envoy/infra/core's
// ClientAccount and JetStreamAccount).
const (
	envoyAccount     = "ENVOY"
	jetStreamAccount = "JETSTREAM"
)

// privateRender is a private render's two accounts as t-c-29 lays them out (packages/envoy/infra/
// core's accounts block, which this module cannot import): JETSTREAM exports, and ENVOY imports,
// as services the JetStream API, answered as a stream of responses, consumers' acknowledgements,
// ordered consumers' flow-control replies, KV writes, every subject the notification stream
// carries (bus.StreamCarriedSubjects) and envoy-measure's publish route (natsgrant.Measure); and as
// streams the two reply roots a push consumer's deliveries and a stream's republish are published
// on, nats.go's default _INBOX and the services' _INBOX_envoy (plan E's t-e-74).
func privateRender() testnats.CalloutAccounts {
	services := []string{"$JS.API.>", "$JS.ACK.>", "$JS.FC.>", "$KV.>"}
	services = append(services, bus.StreamCarriedSubjects()...)
	services = append(services, "envoy-measure.>")
	return testnats.CalloutAccounts{
		JetStream: jetStreamAccount, Clients: envoyAccount,
		Services: services, Streamed: []string{"$JS.API.>"},
		Streams: []string{"_INBOX.>", "_INBOX_envoy.>"},
	}
}

// connectService connects to url as a service presenting raw as its platform token, which the
// test's ServiceToken stub reads (serviceTokens).
func connectService(t *testing.T, url, raw string) (*nats.Conn, error) {
	t.Helper()
	nc, err := nats.Connect(url, nats.UserInfo(natscred.ServiceUser, raw), nats.Timeout(10*time.Second), nats.NoReconnect())
	if err == nil {
		t.Cleanup(nc.Close)
	}
	return nc, err
}

// The account every client the auth service admits is placed in is BROKER_NATS_ACCOUNT's
// (Settings.Account), read where the hub placed the client it admitted: a hub in config mode places
// a client in the account its user JWT's audience names (nats-server server/auth_callout.go,
// assignAccountAndPermissions), so the account the hub reports is the audience the answer named.
// Unset, it is $G, where a hub that names no account keeps every user; ENVOY, on a hub that keeps
// its clients in ENVOY (privateRender), for a session and for a service alike, since every answer
// passes admit. A value that is neither $G nor one subject token refuses the service's start,
// naming the setting and the value.
//
// It fails when the answer names $G whatever the setting: run with admit's audience written as
// "$G", the session and the service under ENVOY each read in $G.
func TestTheAnswerNamesTheConfiguredAccount(t *testing.T) {
	now := time.Now()
	tokens := map[string]platformToken{"daemon-token": {account: daemonAccount, issued: now, expiry: now.Add(time.Hour)}}
	// The clients each row connects, each by the credential it presents: a session's token and
	// proof, and a service's platform token.
	clients := map[string]func(t *testing.T, url string, s *session) (*nats.Conn, error){
		"session": func(t *testing.T, url string, s *session) (*nats.Conn, error) { return connectSession(t, url, s, nil) },
		"service": func(t *testing.T, url string, _ *session) (*nats.Conn, error) {
			return connectService(t, url, "daemon-token")
		},
	}
	for _, row := range []struct {
		name, setting, want string
		// private is whether the hub keeps its clients in ENVOY (privateRender); otherwise it
		// names no account, as today's hub, and keeps every user in $G.
		private bool
	}{
		{"unset", "", config.GlobalNATSAccount, false},
		{"$G", config.GlobalNATSAccount, config.GlobalNATSAccount, false},
		{envoyAccount, envoyAccount, envoyAccount, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			keys := testnats.NewCalloutKeys(t)
			var url string
			if row.private {
				url = testnats.StartWithCalloutAccounts(t, keys, privateRender())
			} else {
				url = testnats.StartWithCallout(t, keys)
			}
			a := newSession(t, "ses_a", 15*time.Minute)
			settings := serviceSettings(t, url, keys)
			settings.Account = row.setting
			startServiceWith(t, settings, Deps{Introspect: introspector(a), ServiceToken: serviceTokens(tokens)}, Decide)
			for name, connect := range clients {
				t.Run(name, func(t *testing.T) {
					nc, err := connect(t, url, a)
					require.NoError(t, err, "the %s was not admitted", name)
					require.Equal(t, row.want, testnats.ConnAccount(t, nc), "the hub placed the %s in another account", name)
				})
			}
		})
	}
	for _, raw := range []string{"a.b", "ENVOY X"} {
		t.Run("refused "+strconv.Quote(raw), func(t *testing.T) {
			settings := serviceSettings(t, "nats://127.0.0.1:1", testnats.NewCalloutKeys(t))
			settings.Account = raw
			service, err := Start(t.Context(), settings, Deps{})
			if err == nil {
				service.Close()
			}
			require.ErrorContains(t, err, config.NATSAccountVariable)
			require.ErrorContains(t, err, strconv.Quote(raw))
		})
	}
}
