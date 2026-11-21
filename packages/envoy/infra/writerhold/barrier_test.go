package writerhold

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/writerhold/holdtest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/natsgrant"
)

// heldGrantCluster is a three-member hub whose user operator holds exactly
// natsgrant.Operator(OperatorPhaseHeld), and whose admin, with no grant, is the no_auth_user every
// other client is: the hold's steps run as the held operator, and the test's own setup as admin.
func heldGrantCluster(t *testing.T) (*testnats.Cluster, KubeHub) {
	t.Helper()
	grant := natsgrant.Operator(natsgrant.OperatorPhaseHeld)
	quote := func(subjects []string) string {
		quoted := make([]string, len(subjects))
		for i, subject := range subjects {
			quoted[i] = fmt.Sprintf("%q", subject)
		}
		return strings.Join(quoted, ", ")
	}
	cluster := testnats.StartClusterConfig(t, 3, fmt.Sprintf(`authorization {
  users = [
    { user: operator, password: operator1, permissions: { publish: { allow: [%s] }, subscribe: { allow: [%s] } } }
    { user: admin, password: admin1 }
  ]
}
no_auth_user: admin
`, quote(grant.Publish), quote(grant.Subscribe)))
	return cluster, hubOf(holdtest.NewKube(t, cluster))
}

// heldOperator connects to cluster as the held operator, with the operator's inbox prefix.
func heldOperator(t *testing.T, cluster *testnats.Cluster) *natsgo.Conn {
	t.Helper()
	nc, err := natsgo.Connect(cluster.URL(), natsgo.UserInfo("operator", "operator1"), natsgo.CustomInboxPrefix(natsgrant.OperatorInbox),
		natsgo.Name("operator/holdtest"), natsgo.Timeout(10*time.Second), natsgo.MaxReconnects(-1), natsgo.ReconnectWait(100*time.Millisecond))
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	return nc
}

// admin connects to cluster with no credential, as the no_auth_user, which holds every subject.
func admin(t *testing.T, cluster *testnats.Cluster) (*natsgo.Conn, jetstream.JetStream) {
	t.Helper()
	nc, err := natsgo.Connect(cluster.URL(), natsgo.Timeout(10*time.Second), natsgo.MaxReconnects(-1), natsgo.ReconnectWait(100*time.Millisecond))
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	return nc, js
}

// streamNames are the names STREAM.NAMES lists, read by the admin.
func streamNames(t *testing.T, js jetstream.JetStream) []string {
	t.Helper()
	var names []string
	lister := js.StreamNames(rowContext(t))
	for name := range lister.Name() {
		names = append(names, name)
	}
	require.NoError(t, lister.Err())
	return names
}

// The meta barrier runs under exactly the held operator's grant: it creates ENVOY_HOLD_BARRIER with
// the one subject _ENVOY_HOLD_BARRIER.<hold id>, one replica, memory storage and max_msgs 1, which a
// spy on the create sees, lists it through STREAM.NAMES, runs the caller's listing after that, and
// deletes it, so no barrier stream is left. A barrier stream another hold left, with its own
// subject, is deleted and made again. It fails when the barrier sends a subject the held grant
// lacks, or creates another stream, or keeps it.
func TestTheMetaBarrierIsAStreamCreatedListedAndDeleted(t *testing.T) {
	cluster, hub := heldGrantCluster(t)
	ctx := rowContext(t)
	declared, err := Discover(ctx, hub)
	require.NoError(t, err)
	operator := heldOperator(t, cluster)
	spy, adminJS := admin(t, cluster)
	creates, err := spy.SubscribeSync(natsserver.JSApiStreamCreateT[:len(natsserver.JSApiStreamCreateT)-2] + ">")
	require.NoError(t, err)
	require.NoError(t, spy.Flush())

	listings := 0
	barrier := MetaBarrier{Hub: hub, Declared: declared, HoldID: "hold1"}
	require.NoError(t, barrier.Run(ctx, operator, func(ctx context.Context) error {
		listings++
		require.NotContains(t, streamNames(t, adminJS), natsgrant.HoldBarrierStream, "the listing runs after the barrier stream is deleted")
		return nil
	}))
	require.Equal(t, 1, listings, "one meta term and leader held the barrier and the listing, so it listed once")
	msg, err := creates.NextMsg(10 * time.Second)
	require.NoError(t, err)
	require.Equal(t, "$JS.API.STREAM.CREATE."+natsgrant.HoldBarrierStream, msg.Subject)
	require.True(t, strings.HasPrefix(msg.Reply, natsgrant.OperatorInbox+"."), "the create's reply %q is under the operator's inbox", msg.Reply)
	var cfg jetstream.StreamConfig
	require.NoError(t, json.Unmarshal(msg.Data, &cfg))
	require.Equal(t, []string{"_ENVOY_HOLD_BARRIER.hold1"}, cfg.Subjects)
	require.Equal(t, 1, cfg.Replicas)
	require.Equal(t, jetstream.MemoryStorage, cfg.Storage)
	require.EqualValues(t, 1, cfg.MaxMsgs)
	require.Zero(t, cfg.MaxBytes)

	_, err = adminJS.CreateStream(ctx, jetstream.StreamConfig{Name: natsgrant.HoldBarrierStream, Subjects: []string{"_ENVOY_HOLD_BARRIER.other"}, Storage: jetstream.MemoryStorage})
	require.NoError(t, err)
	require.NoError(t, MetaBarrier{Hub: hub, Declared: declared, HoldID: "hold2"}.Run(ctx, operator, nil), "a barrier stream another hold left")
	require.NotContains(t, streamNames(t, adminJS), natsgrant.HoldBarrierStream)
}

// A listing that a meta leader change moves under is discarded: the meta group's term changes
// between the barrier and the listing's end (the meta leader restarted inside the first listing),
// so the barrier and the listing are made again, and the listing counts only when one term and
// leader held both. Its control is TestTheMetaBarrierIsAStreamCreatedListedAndDeleted's one listing
// with no change. It fails when the barrier counts a listing across a term change.
func TestTheMetaBarrierListsAgainAfterTheMetaLeaderChanges(t *testing.T) {
	cluster, hub := heldGrantCluster(t)
	ctx := rowContext(t)
	declared, err := Discover(ctx, hub)
	require.NoError(t, err)
	operator := heldOperator(t, cluster)
	barrier := MetaBarrier{Hub: hub, Declared: declared, HoldID: "hold1"}
	before, err := barrier.awaitMeta(ctx)
	require.NoError(t, err)

	listings := 0
	require.NoError(t, barrier.Run(ctx, operator, func(context.Context) error {
		listings++
		if listings == 1 {
			leader := metaLeaderMember(t, hub, declared)
			cluster.Servers[leader].Stop(t)
			cluster.Servers[leader].Start(t)
			cluster.AwaitReady(t)
		}
		return nil
	}))
	require.Equal(t, 2, listings, "the listing across the meta leader's restart was discarded and made again")
	after, err := barrier.awaitMeta(ctx)
	require.NoError(t, err)
	require.Greater(t, after.Term, before.Term)
}

// metaLeaderMember is the index of the declared server that leads the meta group now.
func metaLeaderMember(t *testing.T, hub KubeHub, declared Declared) int {
	t.Helper()
	ctx := rowContext(t)
	for i, server := range declared {
		monitor, stop, err := hub.Monitor(ctx, server.Pod)
		require.NoError(t, err)
		var raftz struct {
			System map[string]struct {
				ID     string `json:"id"`
				Leader string `json:"leader"`
			} `json:"$SYS"`
		}
		err = monitor.Get(ctx, "/raftz?group="+metaGroupName, &raftz)
		stop()
		require.NoError(t, err)
		if group := raftz.System[metaGroupName]; group.ID != "" && group.ID == group.Leader {
			return i
		}
	}
	t.Fatal("no declared server leads the meta group")
	return -1
}

// The bucket barrier's read runs under exactly the held operator's grant on the session consumers'
// bucket: it reads the stream's leader, puts hold.barrier.<hold id> and waits for its
// acknowledgement, and then the caller reads, which sees a record written before. It fails when
// the read sends a subject the held grant lacks, or reads before the barrier is acknowledged.
func TestTheBucketReadTakesTheBarrierUnderTheHeldGrant(t *testing.T) {
	cluster, _ := heldGrantCluster(t)
	ctx := rowContext(t)
	_, adminJS := admin(t, cluster)
	kv, err := adminJS.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "envoy_session_consumers", Replicas: 3})
	require.NoError(t, err)
	_, err = kv.Put(ctx, "K", []byte("recorded"))
	require.NoError(t, err)
	operator := heldOperator(t, cluster)

	var got string
	require.NoError(t, ReadBucket(ctx, operator, "envoy_session_consumers", "hold1", func(ctx context.Context) error {
		entry, err := kv.Get(ctx, "hold.barrier.hold1")
		require.NoError(t, err, "the barrier was acknowledged before the read")
		require.Equal(t, "hold1", string(entry.Value()))
		got, err = lastBySubject(ctx, operator, "envoy_session_consumers", "K")
		return err
	}))
	require.Equal(t, "recorded", got)
	require.True(t, slices.Contains(streamNames(t, adminJS), "KV_envoy_session_consumers"))
}

// lastBySubject is the last record of key in bucket, read with STREAM.MSG.GET, which the stream's
// leader alone answers, under the held grant.
func lastBySubject(ctx context.Context, nc *natsgo.Conn, bucket, key string) (string, error) {
	var answer struct {
		Error   *jetstream.APIError `json:"error,omitempty"`
		Message *struct {
			Data []byte `json:"data"`
		} `json:"message,omitempty"`
	}
	if err := apiRequest(ctx, nc, fmt.Sprintf(natsserver.JSApiMsgGetT, kvStreamPrefix+bucket), map[string]string{"last_by_subj": "$KV." + bucket + "." + key}, &answer); err != nil {
		return "", err
	}
	if answer.Error != nil {
		return "", answer.Error
	}
	if answer.Message == nil {
		return "", fmt.Errorf("STREAM.MSG.GET of %s answered no message", key)
	}
	return string(answer.Message.Data), nil
}
