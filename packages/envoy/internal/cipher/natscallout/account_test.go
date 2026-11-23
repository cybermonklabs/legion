package natscallout

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/natscred"
)

// envoyAccount is the account a private render keeps every client in, and jetStreamAccount the one
// it keeps JetStream in, which no client sits in: plan C's t-c-29, whose render is
// cybermonklabs/legion#4851's accounts block in packages/envoy/infra/core (core.ClientAccount and
// core.JetStreamAccount there). That package is a module of its own, which this one cannot import,
// so the names are written here; config.JetStreamNATSAccount is the name the broker refuses.
const (
	envoyAccount     = "ENVOY"
	jetStreamAccount = config.JetStreamNATSAccount
)

// privateRender is a private render's two accounts as t-c-29 lays them out
// (cybermonklabs/legion#4851's accounts block in packages/envoy/infra/core, which this module
// cannot import): JETSTREAM exports, and ENVOY imports, as services the JetStream API, answered as
// a stream of responses, consumers' acknowledgements, ordered consumers' flow-control replies, KV
// writes, every subject the notification stream carries (bus.StreamCarriedSubjects) and
// envoy-measure's publish route (natsgrant.Measure); and as streams the two reply roots a push
// consumer's deliveries and a stream's republish are published on, nats.go's default _INBOX and
// the services' _INBOX_envoy (plan E's t-e-74).
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

// connectWithServiceToken connects to url as a service presenting raw as its platform token,
// which the test's ServiceToken stub reads (serviceTokens).
func connectWithServiceToken(t *testing.T, url, raw string) (*nats.Conn, error) {
	t.Helper()
	nc, err := nats.Connect(url, nats.UserInfo(natscred.ServiceUser, raw), nats.Timeout(10*time.Second), nats.NoReconnect())
	if err == nil {
		t.Cleanup(nc.Close)
	}
	return nc, err
}

// dispatchToken is the platform token of Dispatch's account, which exampleServices lists as kind
// dispatch: its grant (natsgrant.Dispatch) writes and reads the webhook-redelivery bucket.
func dispatchToken(now time.Time) map[string]platformToken {
	return map[string]platformToken{"dispatch-token": {account: "system:serviceaccount:dispatch:dispatch", issued: now, expiry: now.Add(time.Hour)}}
}

// The account every client the auth service admits is placed in is BROKER_NATS_ACCOUNT's
// (Settings.Account), read where the hub placed the client it admitted: a hub in config mode places
// a client in the account its user JWT's audience names (nats-server server/auth_callout.go,
// assignAccountAndPermissions), so the account the hub reports is the audience the answer named.
// Unset, it is $G, where a hub that names no account keeps every user; ENVOY, on a hub that keeps
// its clients in ENVOY (privateRender), for a session and for a service alike, since every answer
// passes admit. A value that is neither $G nor one subject token, the JetStream account's name, and
// an nkey seed each refuse the service's start, naming the setting and never the value, save the
// JetStream account's name, which the rule itself names.
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
			return connectWithServiceToken(t, url, "daemon-token")
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
	user, err := nkeys.CreateUser()
	require.NoError(t, err)
	seed, err := user.Seed()
	require.NoError(t, err)
	for name, raw := range map[string]string{
		"two tokens":            "a.b",
		"a space":               "ENVOY X",
		"the JetStream account": jetStreamAccount,
		"an nkey seed":          string(seed),
	} {
		t.Run("refused "+name, func(t *testing.T) {
			settings := serviceSettings(t, "nats://127.0.0.1:1", testnats.NewCalloutKeys(t))
			settings.Account = raw
			service, err := Start(t.Context(), settings, Deps{})
			if err == nil {
				service.Close()
			}
			require.ErrorContains(t, err, config.NATSAccountVariable)
			if raw != jetStreamAccount {
				require.NotContains(t, err.Error(), raw, "the refusal prints the value it refused")
			}
		})
	}
}

// TestAPrivateRenderAdmitsIntoEnvoy: on a hub in a private render's two accounts (privateRender),
// whose bucket a static user of ENVOY made in JETSTREAM through ENVOY's imports, Dispatch's service,
// admitted by the auth service with BROKER_NATS_ACCOUNT set to ENVOY, is placed in ENVOY, writes a
// key of the bucket and reads it back, each call crossing to JETSTREAM by an import. The write is
// the JetStream publish nats.go's KeyValue.Put sends, to $KV.<bucket>.<key>.
//
// The control is the setting left at $G, its default: the auth service places the same service in
// $G, which holds no stream and imports nothing, so the same write gets no responders (nats.go's
// ErrNoStreamResponse, its name for a JetStream publish no stream answers, js.go:619-622).
func TestAPrivateRenderAdmitsIntoEnvoy(t *testing.T) {
	bucket, key, value := bus.WebhookRedeliveryBucket, "probe", []byte("written in ENVOY")
	for _, row := range []struct {
		setting  string
		admitted bool
	}{
		{envoyAccount, true},
		{config.GlobalNATSAccount, false},
	} {
		t.Run(row.setting, func(t *testing.T) {
			keys := testnats.NewCalloutKeys(t)
			adminSeed, adminKey := testnats.User(t)
			url := testnats.StartWithCalloutAccounts(t, keys, privateRender(),
				testnats.StaticUser{NKey: adminKey, Publish: []string{"$JS.API.>"}, Subscribe: []string{"_INBOX.>"}})
			admin, err := testnats.ConnectSeed(t, url, adminSeed)
			require.NoError(t, err)
			defer admin.Close()
			adminJS, err := admin.JetStream()
			require.NoError(t, err)
			_, err = adminJS.CreateKeyValue(&nats.KeyValueConfig{Bucket: bucket})
			require.NoError(t, err, "a static user of %s could not make the bucket through its import", envoyAccount)

			settings := serviceSettings(t, url, keys)
			settings.Account = row.setting
			startServiceWith(t, settings, Deps{ServiceToken: serviceTokens(dispatchToken(time.Now()))}, Decide)
			nc, err := connectWithServiceToken(t, url, "dispatch-token")
			require.NoError(t, err, "the auth service did not admit Dispatch's service")
			require.Equal(t, row.setting, testnats.ConnAccount(t, nc), "the hub placed the service in another account")
			js, err := nc.JetStream(nats.MaxWait(5 * time.Second))
			require.NoError(t, err)

			_, err = js.Publish("$KV."+bucket+"."+key, value)
			if !row.admitted {
				require.ErrorIs(t, err, nats.ErrNoStreamResponse, "the service placed in %s had its KV write answered", row.setting)
				return
			}
			require.NoError(t, err, "the service placed in %s could not write the key", row.setting)
			kv, err := js.KeyValue(bucket)
			require.NoError(t, err)
			entry, err := kv.Get(key)
			require.NoError(t, err, "the service placed in %s could not read the key back", row.setting)
			require.Equal(t, string(value), string(entry.Value()))
		})
	}
}

// TestAHubWithoutTheAccountRefusesTheClientsPlacedThere: a hub whose configuration names no
// account, as today's does, refuses every client the auth service places in ENVOY, since the hub
// places a client in the account its user's audience names and fails its connect when it has no
// such account (nats-server server/auth_callout.go, assignAccountAndPermissions: "no valid account
// ... for auth callout response"). So a hub declares ENVOY before the broker names it. The
// auth_callout block's allowed_accounts plays no part: it selects the accounts whose users the hub
// sends to the callout (server/auth.go, the delegated check), and privateRender names none, which
// admits the same service into ENVOY in TestAPrivateRenderAdmitsIntoEnvoy. The control is the
// same service with the setting left at $G, admitted by the same hub.
func TestAHubWithoutTheAccountRefusesTheClientsPlacedThere(t *testing.T) {
	for _, row := range []struct {
		setting  string
		admitted bool
	}{
		{envoyAccount, false},
		{config.GlobalNATSAccount, true},
	} {
		t.Run(row.setting, func(t *testing.T) {
			keys := testnats.NewCalloutKeys(t)
			url := testnats.StartWithCallout(t, keys)
			settings := serviceSettings(t, url, keys)
			settings.Account = row.setting
			startServiceWith(t, settings, Deps{ServiceToken: serviceTokens(dispatchToken(time.Now()))}, Decide)
			_, err := connectWithServiceToken(t, url, "dispatch-token")
			if row.admitted {
				require.NoError(t, err, "the hub refused the service placed in %s", row.setting)
				return
			}
			require.ErrorIs(t, err, nats.ErrAuthorization, "the hub admitted a service placed in %s, an account it does not have", row.setting)
		})
	}
}
