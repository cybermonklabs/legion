// A start that creates or raises a stream of more than one copy, or finds one another program
// created or raised a moment ago, answers once the stream has a leader, on in-process
// three-server clusters (testnats.StartCluster). A row that needs the stream's first election
// slow forces the order a slow election makes; the issue's verification runs the rest against a
// server that holds that election.

package bus

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// createdBucketSendWait is the JetStream wait of the row below: the known-session bucket's, the
// shortest a production caller opens a bucket with (internal/session/known.go,
// knownBucketOpenWait).
const createdBucketSendWait = 2 * time.Second

// A start creates a three-copy bucket however long the bucket's first leader election takes. The
// server answers that create only once the stream's group has elected its leader, so on a cluster
// whose election is slow every send of the create can draw no answer until then: the start sends
// it again until the server answers, as long as its context and its hang guard last, where it
// once gave up after three sends, six seconds at this wait. On a cluster that elects at once the
// first send is answered; the row that holds the slow election is this one run against a server
// that holds the stream's first election (the issue's verification).
func TestEnsureKeyValueCreatesAThreeCopyBucketWhoseFirstElectionIsSlow(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	conn := testnats.Connect(t, cluster.Servers[0].URL())
	t.Cleanup(conn.Close)
	js, err := conn.JetStream(nats.MaxWait(createdBucketSendWait))
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	const bucket = "envoy_create_test"
	kv, err := EnsureKeyValue(t.Context(), conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: 3})
	if err != nil {
		t.Fatalf("ensure a three-copy bucket: %v", err)
	}
	if _, err := kv.Put("key", []byte("value")); err != nil {
		t.Fatalf("put into the bucket the start created: %v", err)
	}
	if copies := testnats.Copies(testnats.AwaitSettledStream(t, cluster.JetStream(t), "KV_"+bucket).Cluster); copies != 3 {
		t.Fatalf("bucket stream has %d copies, want 3", copies)
	}
}

// A start that creates the notification stream at three copies answers only once the stream has a
// leader, so the first publish after it is answered. The server answers the create's first send
// at the stream's first leader election, but a send made again while that election runs is an
// equal create, which it answers at once with the stream still leaderless; a start that took that
// answer as the stream's served a publish no leader could store ("no response from stream"). On a
// cluster that elects at once this row passes either way; the run that holds it is this row against
// a server that holds the stream's first election (the issue's verification).
func TestAStartThatCreatesTheStreamAtThreeCopiesServesItsFirstPublish(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	client := startOnCluster(t, cluster, WithReplicas(3))
	if err := client.Publish(contracts.Envelope{
		EventID: "event-created", Source: "dispatch",
		Topic: "notifications.agent.ses_created", DedupeKey: "dispatch.event.created",
	}); err != nil {
		t.Fatalf("the first publish after a start that created the stream at three copies: %v", err)
	}
}

// A start that raises the notification stream from one copy to three answers only once the raised
// stream has a leader, so the first publish after it is answered. The server answers the raise at
// once, from the member that held the one copy, and the stream has no leader until its new group
// elects one; a publish sent then draws no answer even after the leader is there, so a start that
// took the raise's answer as the stream's left its first publish to fail at its 5 s bound, with no
// election held.
func TestAStartThatRaisesTheStreamToThreeCopiesServesItsFirstPublish(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	startOnCluster(t, cluster, WithReplicas(1)).Close()
	client := startOnCluster(t, cluster, WithReplicas(3))
	if err := client.Publish(contracts.Envelope{
		EventID: "event-raised", Source: "dispatch",
		Topic: "notifications.agent.ses_raised", DedupeKey: "dispatch.event.raised",
	}); err != nil {
		t.Fatalf("the first publish after a start that raised the stream to three copies: %v", err)
	}
}

// hasLeader reads stream once and reports whether it has a leader then: the fact a start that
// answered must leave behind, read with one request so that the row does not itself wait for it.
func hasLeader(t *testing.T, js nats.JetStreamContext, stream string) bool {
	t.Helper()
	info, err := js.StreamInfo(stream)
	if err != nil {
		t.Fatalf("read %s: %v", stream, err)
	}
	return info.Cluster != nil && info.Cluster.Leader != ""
}

// A start that finds a bucket another program raised a moment ago, whose new group has not elected
// its leader yet, waits for that leader before it answers. Its own raise is nothing (the bucket has
// the copies already), and the other program's raise was answered at once, with the stream
// leaderless, so a start that took the stream as it found it handed its caller a bucket whose first
// put drew no answer.
func TestAStartThatFindsAJustRaisedBucketAnswersOnceItHasALeader(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	conn, js := memberJetStream(t, cluster, 0)
	const bucket = "envoy_found_raised_test"
	if _, err := EnsureKeyValue(t.Context(), conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: 1}); err != nil {
		t.Fatalf("create the one-copy bucket: %v", err)
	}
	if _, err := raiseReplicas(t.Context(), js, bucket, 3); err != nil {
		t.Fatalf("another program's raise: %v", err)
	}
	kv, err := EnsureKeyValue(t.Context(), conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: 3})
	if err != nil {
		t.Fatalf("a start that finds the raised bucket: %v", err)
	}
	if !hasLeader(t, js, "KV_"+bucket) {
		t.Fatal("the start answered while the raised bucket's stream had no leader")
	}
	if _, err := kv.Put("key", []byte("value")); err != nil {
		t.Fatalf("the first put after the start: %v", err)
	}
}

// A start configured with one copy that finds a bucket another program, configured with three,
// raised a moment ago waits for that leader too: programs with different ENVOY_NATS_REPLICAS run
// side by side while a deployment raises the setting, and the start at one copy raises nothing,
// so the read of the stream it makes is for the leader alone.
func TestAStartAtOneCopyThatFindsAJustRaisedBucketAnswersOnceItHasALeader(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	conn, js := memberJetStream(t, cluster, 0)
	const bucket = "envoy_found_raised_one_copy_test"
	if _, err := EnsureKeyValue(t.Context(), conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: 1}); err != nil {
		t.Fatalf("create the one-copy bucket: %v", err)
	}
	if _, err := raiseReplicas(t.Context(), js, bucket, 3); err != nil {
		t.Fatalf("another program's raise: %v", err)
	}
	kv, err := EnsureKeyValue(t.Context(), conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: 1})
	if err != nil {
		t.Fatalf("a start at one copy that finds the raised bucket: %v", err)
	}
	if !hasLeader(t, js, "KV_"+bucket) {
		t.Fatal("the start at one copy answered while the raised bucket's stream had no leader")
	}
	if _, err := kv.Put("key", []byte("value")); err != nil {
		t.Fatalf("the first put after the start: %v", err)
	}
}

// missingOnceJetStream answers the first KeyValue lookup "bucket not found", as a start's lookup
// is answered when it lands just before another start's create: the start then sends its own
// create and draws "stream name already in use".
type missingOnceJetStream struct {
	nats.JetStreamContext
	looked bool
}

func (js *missingOnceJetStream) KeyValue(bucket string) (nats.KeyValue, error) {
	if !js.looked {
		js.looked = true
		return nil, nats.ErrBucketNotFound
	}
	return js.JetStreamContext.KeyValue(bucket)
}

// A start at one copy whose create meets a bucket another start, configured with three, created
// a moment before waits for that stream's leader: the other start's count is not this one's, and
// its create may have been answered before its stream's first election ended. The other start's
// create is abandoned the moment a listener on its subject sees it go out, so it lands with its
// answer lost; the cut is made on the create itself, so the account read nats.go makes before it
// is answered first however slow the machine is.
//
// A create that has gone out has not landed. The meta leader hands each JetStream API request to
// one of a pool of GOMAXPROCS workers (nats-server v2.15.1 server/jetstream_api.go:1029 and
// :1111), so this start's create, sent after the other's, can take the meta leader's lock first
// (jetstream_cluster.go:11045) and make the bucket at one copy; the other's is then answered
// "stream name already in use with a different configuration". Run 44543481925 failed this way
// (job 135627934452). So the row waits until the meta leader lists the other start's stream, which
// it reads from the same assignments the create checks (jetstream_api.go:1828 and
// jetstream_cluster.go:11053), before this start looks for the bucket.
func TestAStartAtOneCopyWhoseCreateMeetsAnotherStartsCreateAnswersOnceItHasALeader(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	conn, js := memberJetStream(t, cluster, 0)
	const bucket = "envoy_created_elsewhere_test"
	other, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("open the other start's JetStream: %v", err)
	}
	creates, err := conn.SubscribeSync("$JS.API.STREAM.CREATE.KV_" + bucket)
	if err != nil {
		t.Fatalf("listen for the other start's create: %v", err)
	}
	t.Cleanup(func() { _ = creates.Unsubscribe() })
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the listener: %v", err)
	}
	createCtx, abandon := context.WithCancel(t.Context())
	abandoned := make(chan error, 1)
	go func() {
		_, err := other.CreateKeyValue(createCtx, jetstream.KeyValueConfig{Bucket: bucket, Storage: jetstream.FileStorage, Replicas: 3})
		abandoned <- err
	}()
	_, err = creates.NextMsg(time.Minute)
	abandon()
	if err != nil {
		t.Fatalf("the other start's create never went out, so this row holds nothing: %v", err)
	}
	<-abandoned
	testwait.Eventually(t, "the meta leader to list the other start's stream", func() bool {
		listed := false
		for name := range js.StreamNames() {
			listed = listed || name == "KV_"+bucket
		}
		return listed
	})
	kv, err := EnsureKeyValue(t.Context(), conn, &missingOnceJetStream{JetStreamContext: js}, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: 1})
	if err != nil {
		t.Fatalf("a start at one copy whose create met another start's: %v", err)
	}
	info, err := js.StreamInfo("KV_" + bucket)
	if err != nil {
		t.Fatalf("read the bucket's stream: %v", err)
	}
	if info.Config.Replicas != 3 {
		t.Fatalf("the bucket's stream has %d copies, want the other start's 3: this start's create made the bucket, so this row holds nothing", info.Config.Replicas)
	}
	if info.Cluster == nil || info.Cluster.Leader == "" {
		t.Fatal("the start at one copy answered while the other start's stream had no leader")
	}
	if _, err := kv.Put("key", []byte("value")); err != nil {
		t.Fatalf("the first put after the start: %v", err)
	}
}

// A start that finds the notification stream another program raised a moment ago waits for its
// leader too, for the same reason: the raise was answered at once, and a publish sent while the
// stream has no leader draws no answer.
func TestAStartThatFindsAJustRaisedStreamAnswersOnceItHasALeader(t *testing.T) {
	cluster, js := startClusterJetStream(t)
	startOnCluster(t, cluster, WithReplicas(1)).Close()
	info, err := js.StreamInfo(Stream)
	if err != nil {
		t.Fatalf("read the one-copy stream: %v", err)
	}
	raised := info.Config
	raised.Replicas = 3
	if _, err := js.UpdateStream(&raised); err != nil {
		t.Fatalf("another program's raise: %v", err)
	}
	client := startOnCluster(t, cluster, WithReplicas(3))
	if !hasLeader(t, js, Stream) {
		t.Fatal("the start answered while the raised stream had no leader")
	}
	if err := client.Publish(contracts.Envelope{
		EventID: "event-found-raised", Source: "dispatch",
		Topic: "notifications.agent.ses_found_raised", DedupeKey: "dispatch.event.found_raised",
	}); err != nil {
		t.Fatalf("the first publish after the start: %v", err)
	}
}

// earlyAnswerJetStream loses the answer to a start's first create: that send goes out through a
// context of a millisecond, so the server takes the create and the answer, which the server gives
// only at the stream's first leader election, finds nobody waiting. The start sends the create
// again, and the server answers that equal create at once, while the stream has no leader: the
// order a slow election makes, forced without holding the election.
type earlyAnswerJetStream struct {
	nats.JetStreamContext
	sent int
}

func (js *earlyAnswerJetStream) AddStream(cfg *nats.StreamConfig, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	if js.sent++; js.sent == 1 {
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		defer cancel()
		return js.JetStreamContext.AddStream(cfg, append(slices.Clone(opts), nats.Context(ctx))...)
	}
	return js.JetStreamContext.AddStream(cfg, opts...)
}

// A start whose create of the stream at three copies is answered before the stream's first
// election ends answers only once the stream has a leader.
func TestAStartWhoseStreamCreateIsAnsweredEarlyAnswersOnceItHasALeader(t *testing.T) {
	cluster, js := startClusterJetStream(t)
	cfg := parseConnectOptions([]ConnectOption{WithReplicas(3)}).streamConfig()
	early := &earlyAnswerJetStream{JetStreamContext: js}
	if _, err := ensureStreamWithConfig(t.Context(), early, &cfg, capsAnUnsetStream, atStart); err != nil {
		t.Fatalf("a start whose create's first answer was lost: %v", err)
	}
	if early.sent < 2 {
		t.Fatalf("creates sent = %d, want the first one's answer lost and the create sent again", early.sent)
	}
	if !hasLeader(t, cluster.JetStream(t), Stream) {
		t.Fatal("the start answered while the stream it created had no leader")
	}
}

// A run of the reconnect hooks that finds the stream with no leader, as one whose other members
// are lost is, goes on to the hooks after the stream's ensure rather than waiting the start's hang
// guard for an election that run did not cause (atReconnect): those hooks rewatch the program's
// state, and a run that ended at the ensure would leave every cache it rewatches stale. It once
// waited the 36 s guard, failed the run with "the stream had no leader when the wait ended", and
// ran no hook after the ensure. Here two of the stream's three members are stopped, so the one the
// client is connected to answers the stream's read with no leader for as long as the row runs.
func TestAReconnectRunThatFindsTheStreamWithNoLeaderRunsTheHooksAfterIt(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	cluster.AwaitReady(t)
	client, err := ConnectOwningStream([]string{cluster.Servers[0].URL()}, WithReplicas(3))
	if err != nil {
		t.Fatalf("connect owning the stream at three copies: %v", err)
	}
	t.Cleanup(client.Close)
	var after atomic.Bool
	client.AddReconnectHook(func(context.Context, *nats.Conn) error {
		after.Store(true)
		return nil
	})
	cluster.Servers[1].Stop(t)
	cluster.Servers[2].Stop(t)
	noLeader := testwait.Holds(60*time.Second, 100*time.Millisecond, func() bool {
		info, err := client.JS().StreamInfo(Stream)
		return err == nil && leaderless(info)
	})
	if !noLeader {
		t.Fatal("the remaining member never answered the stream's read with no leader: this row needs that read")
	}

	started := time.Now()
	err = client.runReconnectHooks(client.Conn)
	took := time.Since(started)
	if err != nil || !after.Load() {
		t.Fatalf("a run that found the stream with no leader = %v after %s, the hook after the ensure ran: %t; want nil and the hook run",
			err, took.Round(time.Millisecond), after.Load())
	}
	if took > 10*time.Second {
		t.Fatalf("the run took %s, want it to wait for no election it did not cause", took.Round(time.Millisecond))
	}
}
