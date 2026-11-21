package natscallout

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/natscred"
	"github.com/sjawhar/envoy/natsgrant"
)

// The role and the session the hub's role bucket and session bucket hold one record of each.
const (
	seededRole    = "PUB.implementer"
	seededSession = "ses_holder"
)

// adminUser is a static user of a callout hub that may do anything, which a test seeds the hub's
// stream and buckets with, as the deployment's own operator does.
func adminUser(t *testing.T) (testnats.StaticUser, string) {
	t.Helper()
	seed, key := testnats.User(t)
	return testnats.StaticUser{NKey: key, Publish: []string{">"}, Subscribe: []string{">"}}, seed
}

// seedHub makes the notification stream and the role and session buckets on the hub at url, as
// the admin whose seed is adminSeed, with one role claim and one session record in them.
func seedHub(t *testing.T, url, adminSeed string) {
	t.Helper()
	admin, err := testnats.ConnectSeed(t, url, adminSeed)
	require.NoError(t, err)
	defer admin.Close()
	js, err := admin.JetStream()
	require.NoError(t, err)
	_, err = js.AddStream(&nats.StreamConfig{Name: bus.Stream, Subjects: bus.StreamCarriedSubjects()})
	require.NoError(t, err)
	claim, err := json.Marshal(store.RoleClaim{HolderSessionID: seededSession, ClaimedAt: time.Now().UnixMilli()})
	require.NoError(t, err)
	for bucket, record := range map[string][2]string{
		bus.RoleClaimBucket: {seededRole, string(claim)},
		bus.SessionBucket:   {seededSession, `{"session_id":"` + seededSession + `"}`},
	} {
		kv, err := js.CreateKeyValue(&nats.KeyValueConfig{Bucket: bucket})
		require.NoError(t, err)
		_, err = kv.Put(record[0], []byte(record[1]))
		require.NoError(t, err)
	}
}

// serviceEnvironment is the environment of a process that connects to NATS as a service with
// the platform token raw: NATS_SERVICE_TOKEN_FILE naming a file that holds it, and nothing else.
func serviceEnvironment(t *testing.T, raw string) func(string) (string, bool) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(file, []byte(raw+"\n"), 0o600))
	return func(name string) (string, bool) {
		if name == bus.ServiceTokenFileEnvVar {
			return file, true
		}
		return "", false
	}
}

// issueEvent is one Dispatch issue event, as the outbox publishes one.
func issueEvent() contracts.Envelope {
	return contracts.Envelope{
		EventID: "dispatch-1", Source: "dispatch", SourceEventID: "1", DedupeKey: "dispatch-1",
		Topic: "notifications.dispatch.issue.PUB.PUB-1.created", IssuedAt: contracts.NowMillis(),
	}
}

// Dispatch's app connection works through the auth service on a hub rendered private: its
// envoy-nats token, decided for its dispatch/<machine id>/* entry, is answered with
// natsgrant.DispatchApp on the answer path every answer takes today, and with it the outbox's
// PublishAsync of one issue event is acknowledged by the stream, and roles.Buckets opens and reads
// the role bucket and the session bucket (envoy_roles, envoy_sessions). The answer holds every
// entry of natsgrant.Dispatch but the listener durable's two calls, its acknowledgements and the two
// role-lane queue subscriptions, and none of those. The control is the plan's earlier form, a
// Dispatch grant with no subject of the listener grant: the publish and the bucket reads are
// refused, and the test fails naming each.
func TestDispatchsAppWorksThroughTheCalloutOnATaggedHub(t *testing.T) {
	now := time.Now()
	raw := serviceJWT(t, natsAudience, dispatchAccount, now.Add(time.Hour))
	deps := Deps{ServiceToken: serviceTokens(map[string]platformToken{raw: {account: dispatchAccount, issued: now, expiry: now.Add(time.Hour)}})}
	keys := privateHubKeys(t)
	admin, adminSeed := adminUser(t)
	url := testnats.StartWithCallout(t, keys, admin)
	startServiceOf(t, url, keys, proofServices, deps)
	seedHub(t, url, adminSeed)

	client, err := bus.Connect([]string{url}, bus.WithName("envoy-dispatch"), bus.WithEnvironment(serviceEnvironment(t, raw)))
	require.NoError(t, err, "Dispatch's app was not admitted on the hub rendered private")
	t.Cleanup(client.Close)
	select {
	case err := <-client.WindowUntil(nil).PublishAsync(issueEvent()):
		if err != nil {
			t.Errorf("the outbox's PublishAsync of an issue event: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Error("the outbox's PublishAsync of an issue event got no answer")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	holders := roles.NewBuckets(client, roles.Windows{})
	if claim, err := holders.RoleClaim(ctx, seededRole); err != nil || claim.HolderSessionID != seededSession {
		t.Errorf("roles.Buckets read of %s: %+v, %v", bus.RoleClaimBucket, claim, err)
	}
	if _, err := holders.Holder(ctx, seededSession); err != nil {
		t.Errorf("roles.Buckets read of %s: %v", bus.SessionBucket, err)
	}

	decision, err := Decide(t.Context(), deps, proofServices, tagged(serviceRequest(raw)))
	require.NoError(t, err)
	require.Equal(t, natsgrant.DispatchApp(dispatchMachineID), decision.Grant)
	dispatch := natsgrant.Dispatch(dispatchMachineID)
	durable := bus.ListenerDurable(dispatchMachineID)
	listenerOnly := []string{
		"$JS.API.CONSUMER.INFO." + bus.Stream + "." + durable, "$JS.API.CONSUMER.CREATE." + bus.Stream + "." + durable,
		"$JS.ACK." + bus.Stream + "." + durable + ".>",
		contracts.RoleTopicPrefix + "> " + bus.ListenerQueue(dispatchMachineID), contracts.RoleTopicPrefix + "> " + bus.RoleLaneQueue,
	}
	for _, entry := range append(append([]string{}, dispatch.Publish...), dispatch.Subscribe...) {
		held := slices.Contains(decision.Grant.Publish, entry) || slices.Contains(decision.Grant.Subscribe, entry)
		require.Equal(t, !slices.Contains(listenerOnly, entry), held, "DispatchApp's answer and %s", entry)
	}
}

// A listener started with Dispatch's app account's token, the pod spec before Dispatch's listener
// runs as an account of its own, binds no role lane on a hub rendered private: the server refuses
// its role-lane subscribe in both queue groups, and the role lane does not start, naming the role
// lane. The control answers natsgrant.Dispatch on the tagged hub: the listener binds its role
// lane, and the test fails naming it.
func TestAListenerOnDispatchsAppAccountBindsNoRoleLaneOnATaggedHub(t *testing.T) {
	now := time.Now()
	raw := serviceJWT(t, natsAudience, dispatchAccount, now.Add(time.Hour))
	deps := Deps{ServiceToken: serviceTokens(map[string]platformToken{raw: {account: dispatchAccount, issued: now, expiry: now.Add(time.Hour)}})}
	keys := privateHubKeys(t)
	url := testnats.StartWithCallout(t, keys)
	startServiceOf(t, url, keys, proofServices, deps)

	client, queue, err := bus.ConnectRoleLane([]string{url}, dispatchMachineID, func(*nats.Msg) {},
		bus.WithName("envoy-role-lane"+natsgrant.PrivacyRequiredSuffix), bus.WithEnvironment(serviceEnvironment(t, raw)))
	if err == nil {
		client.Close()
		t.Fatalf("a listener on Dispatch's app account bound the role lane in the queue group %s on the hub rendered private", queue)
	}
	require.ErrorContains(t, err, contracts.RoleTopicPrefix+">", "the role lane's refusal names the role lane")
}

// Dispatch's listener, running as a dispatch-listener/<machine id>/* entry's account, is answered
// with natsgrant.DispatchListener and keeps today's grant: on a tagged and on an untagged hub it
// binds its durable and its role lane, and a session consumer it would create for a registration
// asking it for consumer delivery is refused bus.ErrConsumerNotGranted, which the listener answers
// 503 no_grant, writing nothing of the session, as the listener beside envoy-dispatch does today;
// the stream holds no consumer of that session. The control decides the entry as a listener, the
// full listener grant: the session consumer is created, and the test fails naming it.
func TestDispatchsListenerRefusesConsumerDeliveryAsToday(t *testing.T) {
	now := time.Now()
	raw := serviceJWT(t, natsAudience, dispatchListenerAccount, now.Add(time.Hour))
	tokens := map[string]platformToken{raw: {account: dispatchListenerAccount, issued: now, expiry: now.Add(time.Hour)}}
	deps := Deps{ServiceToken: serviceTokens(tokens)}
	services := []ServiceEntry{{Kind: KindDispatchListener, MachineID: dispatchMachineID, Account: dispatchListenerAccount, AllProjects: true}}
	for name, keys := range map[string]testnats.CalloutKeys{"tagged": privateHubKeys(t), "untagged": testnats.NewCalloutKeys(t)} {
		t.Run(name, func(t *testing.T) {
			static, adminSeed := adminUser(t)
			url := testnats.StartWithCallout(t, keys, static)
			startServiceOf(t, url, keys, services, deps)
			seedHub(t, url, adminSeed)
			environment := serviceEnvironment(t, raw)
			required := func(name string) string { return name + natsgrant.PrivacyRequiredSuffix }

			nc, err := nats.Connect(url, nats.UserInfo(natscred.ServiceUser, raw), nats.Name(required("envoy-listener")), nats.NoReconnect())
			require.NoError(t, err, "Dispatch's listener was not admitted")
			t.Cleanup(nc.Close)
			js, err := nc.JetStream()
			require.NoError(t, err)
			inbox := nc.NewRespInbox()
			_, err = nc.SubscribeSync(inbox)
			require.NoError(t, err)
			_, err = js.AddConsumer(bus.Stream, &nats.ConsumerConfig{
				Durable: bus.ListenerDurable(dispatchMachineID), DeliverSubject: inbox, AckPolicy: nats.AckExplicitPolicy,
			}, nats.MaxWait(10*time.Second))
			require.NoError(t, err, "Dispatch's listener did not bind its durable")

			lane, queue, err := bus.ConnectRoleLane([]string{url}, dispatchMachineID, func(*nats.Msg) {},
				bus.WithName(required("envoy-role-lane")), bus.WithEnvironment(environment))
			require.NoError(t, err, "Dispatch's listener did not bind its role lane")
			t.Cleanup(lane.Close)
			require.Equal(t, bus.RoleLaneQueue, queue)

			consumers, err := bus.ConnectSessionConsumers([]string{url}, 0, nil,
				bus.WithName(required("envoy-session-consumers")), bus.WithEnvironment(environment))
			require.NoError(t, err)
			t.Cleanup(consumers.Close)
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			_, err = consumers.Ensure(ctx, "ses_a", []string{contracts.AgentSubject("PUB", "ses_a")}, 0)
			require.True(t, errors.Is(err, bus.ErrConsumerNotGranted), "a session consumer for a registration asking for consumer delivery: %v", err)

			admin, err := testnats.ConnectSeed(t, url, adminSeed)
			require.NoError(t, err)
			defer admin.Close()
			adminJS, err := admin.JetStream()
			require.NoError(t, err)
			consumer, _ := bus.SessionConsumerName("ses_a")
			_, err = adminJS.ConsumerInfo(bus.Stream, consumer)
			require.True(t, errors.Is(err, nats.ErrConsumerNotFound), "the stream holds %s: %v", consumer, err)
		})
	}
	decision, err := Decide(t.Context(), deps, services, serviceRequest(raw))
	require.NoError(t, err)
	require.Equal(t, natsgrant.DispatchListener(dispatchMachineID), decision.Grant)
	require.Equal(t, "dispatch-listener/"+dispatchMachineID, decision.Name)
}
