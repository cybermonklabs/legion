package core_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A start that finds a bucket with fewer copies than ENVOY_NATS_REPLICAS raises it with an update
// of the bucket's stream (bus.EnsureKeyValue). On a three-server cluster enforcing the users this
// module renders, a client holding ListenerPermissions raises the interest bucket from one copy to
// three. The negative control is a client holding the same grant without the update of any
// bucket's stream: the server refuses its raise, the start ends at once with the server's refusal
// and names ENVOY_NATS_REPLICAS, and the bucket keeps its one copy. That client is a bus.Client,
// as every production caller's connection is, so the refusal the server reports on the connection
// is recorded and answers the raise (bus.ErrPublishDenied); a connection no client owns would read
// the refusal as an update nothing answered and send it again until the start's hang guard.
func TestListenerPermissionsRaiseABucketOnACluster(t *testing.T) {
	grant := core.ListenerPermissions("example-host-a")
	withoutUpdate := grant
	withoutUpdate.Publish.Allow = slices.DeleteFunc(slices.Clone(grant.Publish.Allow), func(entry string) bool {
		return strings.HasPrefix(entry, "$JS.API.STREAM.UPDATE.KV_")
	})
	require.Contains(t, grant.Publish.Allow, "$JS.API.STREAM.UPDATE.KV_"+bus.InterestBucket)
	require.NotContains(t, withoutUpdate.Publish.Allow, "$JS.API.STREAM.UPDATE.KV_"+bus.InterestBucket)

	listenerSeed, listenerKey := testnats.User(t)
	refusedSeed, refusedKey := testnats.User(t)
	_, adminKey := testnats.User(t)
	everything := core.Rules{Allow: []string{">"}}
	config, err := core.Authorization{Users: []core.User{
		{Name: "example-listener", NKey: listenerKey, Publish: &grant.Publish, Subscribe: &grant.Subscribe, HasSeed: true},
		{Name: "example-no-bucket-update", NKey: refusedKey, Publish: &withoutUpdate.Publish, Subscribe: &withoutUpdate.Subscribe, HasSeed: true},
		// The cluster's readiness check and this test's reads of the bucket connect with no
		// credential, as this user.
		{Name: "example-admin", NKey: adminKey, Publish: &everything, Subscribe: &everything},
	}, NoAuthUser: "example-admin"}.Render(nil)
	require.NoError(t, err)
	cluster := testnats.StartClusterConfig(t, 3, config)

	// The admin's JetStream context (no credential: the no_auth_user) reads the bucket's copies.
	adminJS := cluster.JetStream(t)
	bucketStream := "KV_" + bus.InterestBucket

	bucket := func(replicas int) *natsgo.KeyValueConfig {
		return &natsgo.KeyValueConfig{Bucket: bus.InterestBucket, Replicas: replicas}
	}
	listener, listenerJS, listenerErrs := connectGranted(t, cluster.URL(), listenerSeed, 10*time.Second)
	// The listener's own grant creates the bucket, the request bus.EnsureKeyValue's create path
	// sends; through the cluster, so the member each client below is on answers for the bucket
	// before that client looks it up.
	cluster.CreateKeyValue(t, listener, bucket(1))
	testnats.AwaitStreamCopies(t, adminJS, bucketStream, 1)

	t.Setenv("NATS_NKEY_SEED", refusedSeed)
	refused, err := bus.Connect(cluster.URLs())
	require.NoError(t, err)
	t.Cleanup(refused.Close)
	refusedJS, err := refused.Conn.JetStream(natsgo.MaxWait(2 * time.Second))
	require.NoError(t, err)
	started := time.Now()
	_, err = bus.EnsureKeyValue(t.Context(), refused.Conn, refusedJS, bucket(3))
	took := time.Since(started)
	require.ErrorIs(t, err, bus.ErrPublishDenied, "a client without the bucket's stream update raised it, or its raise did not end at the refusal")
	refusedRaise := "raise KV_" + bus.InterestBucket + ": " + bus.ReplicasEnvVar + "=3: "
	require.True(t, strings.HasPrefix(err.Error(), refusedRaise), "the refused raise's error %q does not start %q", err, refusedRaise)
	require.Contains(t, err.Error(), `Publish to "$JS.API.STREAM.UPDATE.KV_`+bus.InterestBucket+`"`)
	require.Less(t, took, 3*time.Second, "the refused raise ended after %s, not at the server's refusal", took)
	t.Logf("the refused raise, after %s: %v", took, err)
	testnats.AwaitStreamCopies(t, adminJS, bucketStream, 1)

	_, err = bus.EnsureKeyValue(t.Context(), listener, listenerJS, bucket(3))
	require.NoError(t, err, "a client holding ListenerPermissions could not raise the bucket")
	testnats.AwaitStreamCopies(t, adminJS, bucketStream, 3)
	require.NoError(t, listener.Flush())
	select {
	case err := <-listenerErrs:
		t.Fatalf("the server refused the listener's grant something: %v", err)
	default:
	}
}

// connectGranted connects to uri as the nkey user whose seed is seed, with JetStream requests that
// wait up to maxWait, and returns the connection, its JetStream context, and the asynchronous
// errors the server sends it, a permissions violation among them.
func connectGranted(t *testing.T, uri, seed string, maxWait time.Duration) (*natsgo.Conn, natsgo.JetStreamContext, <-chan error) {
	t.Helper()
	errs := make(chan error, 16)
	conn, err := testnats.ConnectSeed(t, uri, seed, natsgo.ErrorHandler(func(_ *natsgo.Conn, _ *natsgo.Subscription, err error) {
		select {
		case errs <- err:
		default:
		}
	}))
	require.NoError(t, err)
	t.Cleanup(conn.Close)
	js, err := conn.JetStream(natsgo.MaxWait(maxWait))
	require.NoError(t, err)
	return conn, js, errs
}

// requirePublishRefused waits up to 10 s for the server's permissions violation of a publish to
// subject among errs.
func requirePublishRefused(t *testing.T, errs <-chan error, subject string) {
	t.Helper()
	requireRefused(t, errs, "Publish", subject)
}

// requireRefused waits up to 10 s for the server's permissions violation of call to subject among
// errs, call as the violation names it: `Publish` or `Subscription`.
func requireRefused(t *testing.T, errs <-chan error, call, subject string) {
	t.Helper()
	want := call + ` to "` + subject + `"`
	timeout := time.After(10 * time.Second)
	for {
		select {
		case err := <-errs:
			if errors.Is(err, natsgo.ErrPermissionViolation) && strings.Contains(err.Error(), want) {
				return
			}
			t.Logf("another asynchronous error: %v", err)
		case <-timeout:
			t.Fatalf("the server reported no permissions violation naming %s", want)
		}
	}
}
