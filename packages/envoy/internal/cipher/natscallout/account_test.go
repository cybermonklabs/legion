package natscallout

import (
	"crypto/rand"
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cipher/config"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/natscred"
	"github.com/sjawhar/envoy/natsgrant"
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
// cannot import), with one route that block lacks. JETSTREAM exports, and ENVOY imports, as
// services the JetStream API, answered as a stream of responses, consumers' acknowledgements,
// ordered consumers' flow-control replies, KV writes and every subject the notification stream
// carries (bus.StreamCarriedSubjects); and as streams the two reply roots a push consumer's
// deliveries and a stream's republish are published on, nats.go's default _INBOX and the
// services' _INBOX_envoy (plan E's t-e-74).
//
// The route #4851's block lacks is $JS.SNAPSHOT.>, where a backup's chunks are acknowledged and a
// restore's chunks are sent (natsgrant.Operator's $JS.SNAPSHOT.ACK.<stream>.> and
// $JS.SNAPSHOT.RESTORE.<stream>.*, held's alone): without it, the held operator's backups and
// restores from ENVOY reach no subscriber. ENVOY exports it, and JETSTREAM imports it, as a
// stream, not as a service: a service import adds a Nats-Request-Info header to each request, and
// nats-server v2.15.1's restore takes each chunk whole, header included, as the archive's bytes
// (jetstream_api.go:4532-4554, processStreamRestore's processChunk), so it reads the end, an empty
// chunk, as data and the restore never completes. A backup completes through either.
// TestAPrivateRenderAdmitsIntoEnvoy fails with the route left out, and with it as a service. The
// block's envoy-measure.> service is not here: natsgrant.Measure no longer publishes it.
func privateRender() testnats.CalloutAccounts {
	services := []string{"$JS.API.>", "$JS.ACK.>", "$JS.FC.>", "$KV.>"}
	services = append(services, bus.StreamCarriedSubjects()...)
	return testnats.CalloutAccounts{
		JetStream: jetStreamAccount, Clients: envoyAccount,
		Services: services, Streamed: []string{"$JS.API.>"},
		Streams:     []string{"_INBOX.>", "_INBOX_envoy.>"},
		ToJetStream: []string{"$JS.SNAPSHOT.>"},
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
// its clients in ENVOY (privateRender), for a session, a service and the operator alike, since
// every answer passes admit: the operator, whose grant follows the hold row, is placed in the same
// account as every other client. A value that is neither $G nor one subject token, the JetStream
// account's name, and an nkey seed each refuse the service's start, naming the setting and never
// the value, save the JetStream account's name, which the rule itself names.
//
// It fails when the answer names $G whatever the setting: run with admit's audience written as
// "$G", the session and the service under ENVOY each read in $G; and with $G written for the
// operator's answer alone, the operator under ENVOY reads in $G.
func TestTheAnswerNamesTheConfiguredAccount(t *testing.T) {
	now := time.Now()
	tokens := map[string]platformToken{
		"daemon-token":   {account: daemonAccount, issued: now, expiry: now.Add(time.Hour)},
		"operator-token": {account: operatorAccount, issued: now, expiry: now.Add(time.Hour)},
	}
	// The clients each row connects, each by the credential it presents: a session's token and
	// proof, a service's platform token, and the operator's.
	clients := map[string]func(t *testing.T, url string, s *session) (*nats.Conn, error){
		"session": func(t *testing.T, url string, s *session) (*nats.Conn, error) { return connectSession(t, url, s, nil) },
		"service": func(t *testing.T, url string, _ *session) (*nats.Conn, error) {
			return connectWithServiceToken(t, url, "daemon-token")
		},
		"operator": func(t *testing.T, url string, _ *session) (*nats.Conn, error) {
			return connectWithServiceToken(t, url, "operator-token")
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
			settings.Services = holdServices
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
// whose bucket a static user of ENVOY made in JETSTREAM through ENVOY's imports, the auth service,
// with BROKER_NATS_ACCOUNT set to ENVOY, places Dispatch's service and the operator in ENVOY, and
// each call below crosses to JETSTREAM by an import. Dispatch's service writes a key of the bucket
// and reads it back; the write is the JetStream publish nats.go's KeyValue.Put sends, to
// $KV.<bucket>.<key>. The operator, with the hold row reading held, makes three calls of its held
// grant (natsgrant.Operator) on the same bucket's stream, a protected bucket's, and no other: it
// reads the stream's info, which its grant holds only while the row reads prepare or held; it backs
// the stream up as envoy-infra buckets does (backupStream), the chunks reaching its inbox through
// the _INBOX_envoy stream ENVOY imports and each acknowledged on $JS.SNAPSHOT.ACK.<stream>.>
// through the $JS.SNAPSHOT.> stream JETSTREAM imports; and, the stream deleted, it restores the
// stream from that backup (restoreStream), each chunk sent to $JS.SNAPSHOT.RESTORE.<stream>.<id>
// through the same stream, with the key's one message. The key's value is three chunks of random
// bytes, which the snapshot cannot compress, so the backup is more than one chunk, and each chunk
// after the first waits for the acknowledgement of the one before (snapshotChunk). The static user
// that makes the bucket writes that value before either subtest runs, so the operator's subtest
// needs nothing of Dispatch's and runs alone (-run 'TestAPrivateRenderAdmitsIntoEnvoy/ENVOY/operator');
// Dispatch's write of the same key replaces it, and the bucket keeps one value a key.
//
// The control is the setting left at $G, its default: the auth service places the same clients in
// $G, which holds no stream and imports nothing. So the same write gets no responders (nats.go's
// ErrNoStreamResponse, its name for a JetStream publish no stream answers, js.go:619-622), and the
// operator's read is answered by the server's JetStream for $G, refusing it with 10039,
// "JetStream not enabled for account". With $G written for the operator's answer alone in admit,
// the operator under ENVOY gets that refusal, and the test fails naming it; with the row reading
// released, its grant refuses the read, which gets no answer, and the test fails naming it. With
// $JS.SNAPSHOT.> left out of privateRender, the first chunk's acknowledgement reaches no
// subscriber, the server ends the snapshot with 408 No Flow Response, and the test fails naming
// it; with $JS.SNAPSHOT.> a service JETSTREAM exports and ENVOY imports instead, the backup
// completes, the restore's end is answered as a chunk is, with nothing, and the test fails naming
// that answer.
func TestAPrivateRenderAdmitsIntoEnvoy(t *testing.T) {
	bucket, key := bus.WebhookRedeliveryBucket, "probe"
	value := make([]byte, 3*snapshotChunk)
	_, _ = rand.Read(value)
	stream := kvBucket(bucket)
	heldRead := "$JS.API.STREAM.INFO." + stream
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
				testnats.StaticUser{NKey: adminKey, Publish: []string{"$JS.API.>", "$KV." + bucket + ".>"}, Subscribe: []string{"_INBOX.>"}})
			admin, err := testnats.ConnectSeed(t, url, adminSeed)
			require.NoError(t, err)
			defer admin.Close()
			adminJS, err := admin.JetStream()
			require.NoError(t, err)
			adminKV, err := adminJS.CreateKeyValue(&nats.KeyValueConfig{Bucket: bucket})
			require.NoError(t, err, "a static user of %s could not make the bucket through its import", envoyAccount)
			_, err = adminKV.Put(key, value)
			require.NoError(t, err, "a static user of %s could not write the key through its import", envoyAccount)

			now := time.Now()
			tokens := dispatchToken(now)
			tokens["operator-token"] = platformToken{account: operatorAccount, issued: now, expiry: now.Add(time.Hour)}
			signer, hold := newHoldSigner(t), &holdStub{}
			hold.set(rowOf(signer.reading(t, 1, natsgrant.OperatorPhaseHeld, "hold-1")))
			settings := serviceSettings(t, url, keys)
			settings.Account = row.setting
			settings.Services = holdServices
			startServiceWith(t, settings, Deps{ServiceToken: serviceTokens(tokens), Hold: NewHoldView(hold.read, signer.HoldKeys, nil, nil)}, Decide)

			t.Run("dispatch", func(t *testing.T) {
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
				require.Equal(t, value, entry.Value(), "the service placed in %s read back another value", row.setting)
			})
			t.Run("operator", func(t *testing.T) {
				// The operator's one subscribe is its own inbox prefix, so its requests' replies are there.
				nc, err := nats.Connect(url, nats.UserInfo(natscred.ServiceUser, "operator-token"),
					nats.CustomInboxPrefix(natsgrant.OperatorInbox), nats.Timeout(10*time.Second), nats.NoReconnect())
				require.NoError(t, err, "the auth service did not admit the operator")
				t.Cleanup(nc.Close)
				if row.admitted {
					var info struct {
						Config struct {
							Name string `json:"name"`
						} `json:"config"`
					}
					require.NoError(t, json.Unmarshal(succeeds(t, nc, heldRead, nil).raw, &info))
					require.Equal(t, stream, info.Config.Name, "the held operator placed in %s read another stream's info", row.setting)

					backup, err := backupStream(nc, stream)
					require.NoError(t, err, "the held operator placed in %s could not back up %s", row.setting, stream)
					require.Greater(t, len(backup.data), snapshotChunk, "the backup of %s is one chunk, so it waited for no acknowledgement", stream)
					require.Equal(t, uint64(1), backup.messages, "the backup of %s holds another count than the key's one message", stream)
					succeeds(t, nc, "$JS.API.STREAM.DELETE."+stream, nil)
					restored, err := restoreStream(t.Context(), nc, backup, nil)
					require.NoError(t, err, "the held operator placed in %s could not restore %s", row.setting, stream)
					require.Equal(t, backup.messages, restored, "the restore of %s holds another count than its backup", stream)
				} else {
					answer := answered(t, nc, heldRead, nil)
					require.NotNil(t, answer.Error, "the operator placed in %s read %s: %s", row.setting, heldRead, answer.raw)
					require.Equal(t, int(nats.JSErrCodeJetStreamNotEnabledForAccount), answer.Error.ErrCode,
						"the operator placed in %s was refused %s for another cause: %s", row.setting, heldRead, answer.raw)
				}
				require.Equal(t, row.setting, testnats.ConnAccount(t, nc), "the hub placed the operator in another account")
			})
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
