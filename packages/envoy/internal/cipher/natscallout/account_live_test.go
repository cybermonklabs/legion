//go:build natslive

package natscallout

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/testnats"
)

// dispatchToken is the platform token of Dispatch's account, which exampleServices lists as kind
// dispatch: its grant (natsgrant.Dispatch) writes and reads the webhook-redelivery bucket.
func dispatchToken(now time.Time) map[string]platformToken {
	return map[string]platformToken{"dispatch-token": {account: "system:serviceaccount:dispatch:dispatch", issued: now, expiry: now.Add(time.Hour)}}
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
			nc, err := connectService(t, url, "dispatch-token")
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
			_, err := connectService(t, url, "dispatch-token")
			if row.admitted {
				require.NoError(t, err, "the hub refused the service placed in %s", row.setting)
				return
			}
			require.ErrorIs(t, err, nats.ErrAuthorization, "the hub admitted a service placed in %s, an account it does not have", row.setting)
		})
	}
}
