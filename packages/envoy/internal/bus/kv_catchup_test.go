package bus

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// stillClock is the clock a start's waits read in the rows below (ensureKeyValue), standing at one
// moment until the row moves it: a wait on it sleeps for ever, so a start that answers while it
// stands still waited for nothing.
func stillClock() *clocktest.Fake {
	return clocktest.New(time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC))
}

// ensuredOn starts ensureKeyValue on c on a goroutine of its own, and returns where its error
// arrives.
func ensuredOn(c *clocktest.Fake, conn *nats.Conn, js nats.JetStreamContext, config *nats.KeyValueConfig) <-chan error {
	answered := make(chan error, 1)
	go func() {
		_, err := ensureKeyValue(c, conn, js, config, RequestTimeout)
		answered <- err
	}()
	return answered
}

// catchupBucket is the bucket the rows below create on a cluster, one per row.
const catchupBucket = "envoy_catchup_test"

// memberJetStream is a JetStream context on member i of cluster alone, as a program of the
// deployment that connected to that member holds: every request a start makes goes to that one
// member, so a member that has not applied a create answers for it.
func memberJetStream(t *testing.T, cluster *testnats.Cluster, i int) (*nats.Conn, nats.JetStreamContext) {
	t.Helper()
	conn := testnats.Connect(t, cluster.Servers[i].URL())
	t.Cleanup(conn.Close)
	js, err := conn.JetStream(nats.MaxWait(RequestTimeout))
	if err != nil {
		t.Fatalf("open JetStream on member %s: %v", cluster.Name(i), err)
	}
	return conn, js
}

// behindMember is a JetStream context on a real cluster that answers as a member which has not
// applied a create of the bucket yet, for as long as the member is behind, and reaches the cluster
// for everything else:
//   - its bucket lookup answers ErrBucketNotFound for the first misses lookups, the answer such a
//     member gives a read of a stream it has no assignment for, and which a start cannot tell from
//     a bucket that is not there;
//   - its create answers ErrStreamNameAlreadyInUse, the cluster's own answer to a create of a
//     bucket that exists. nats.go rescues that answer by reading the stream itself and comparing
//     it (kv.go:502-516, v1.50.0), which is exactly the read this member cannot serve, so the
//     answer reaches the start.
//
// A clustered create is answered once the metadata group has committed it, not once every member
// has applied it: a probe over 25 fresh three-member clusters asked every member for a bucket's
// stream the instant CreateKeyValue answered, and 67 of the 75 member reads still said "stream not
// found", the slowest 206 ms later, including the member the create was sent through.
type behindMember struct {
	nats.JetStreamContext
	misses int
}

func (b *behindMember) KeyValue(bucket string) (nats.KeyValue, error) {
	if b.misses > 0 {
		b.misses--
		return nil, nats.ErrBucketNotFound
	}
	return b.JetStreamContext.KeyValue(bucket)
}

func (b *behindMember) CreateKeyValue(config *nats.KeyValueConfig) (nats.KeyValue, error) {
	return nil, nats.ErrStreamNameAlreadyInUse
}

// A second program that ensures a bucket the first has just created on a three-member cluster
// opens it, even when the member answering has not applied the create yet. Its lookup misses, as
// that member's answer makes it, so it sends a create of its own, which the cluster refuses with
// "stream name already in use": an answer no lookup of that same member could resolve while it is
// behind. Without the wait (awaitBucketApplied) the second program fails there, and on a real hub
// a program that ensures its bucket at start exits; with it, the lookup is taken again until the
// member has the bucket, and both programs hold a handle on it.
//
// Everything but that one answer is the cluster's own: the first program's create, the second
// program's create and the handle each of them opens.
func TestASecondStartOpensABucketAMemberHasNotAppliedYet(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	config := func() *nats.KeyValueConfig {
		return &nats.KeyValueConfig{Bucket: catchupBucket, Storage: nats.FileStorage, Replicas: 3}
	}
	first, firstJS := memberJetStream(t, cluster, 0)
	if _, err := EnsureKeyValue(first, firstJS, config()); err != nil {
		t.Fatalf("the first start could not create the bucket: %v", err)
	}
	second, secondJS := memberJetStream(t, cluster, 1)
	behind := &behindMember{JetStreamContext: secondJS, misses: 1}
	kv, err := EnsureKeyValue(second, behind, config())
	if err != nil {
		t.Fatalf("the second start could not open the bucket its member had not applied yet: %v", err)
	}
	if kv.Bucket() != catchupBucket {
		t.Fatalf("the second start's handle is on bucket %q, want %q", kv.Bucket(), catchupBucket)
	}
	if _, err := kv.ReadStreamState(context.Background()); err != nil {
		t.Fatalf("the second start could not read the bucket's stream: %v", err)
	}
}

// A create the server refuses is an answer, not a member that has not caught up, so it comes back
// at once and nothing waits it out: a bucket asking for more copies than the cluster has members
// is refused ("replicas > clusters size"), named with the setting that asked for them
// (refusedSettings). The start's clock stands still, so a refusal its wait took for a member that
// is behind would sleep on it for ever.
func TestARefusedCreateFailsAtOnceRatherThanWaitingOutTheDeadline(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	conn, js := memberJetStream(t, cluster, 0)
	err := testwait.Receive(t, "the refused create's answer with the start's clock standing still",
		ensuredOn(stillClock(), conn, js, &nats.KeyValueConfig{
			Bucket:   catchupBucket,
			Storage:  nats.FileStorage,
			Replicas: 5,
		}))
	if err == nil {
		t.Fatalf("ensure a bucket with 5 copies on a 3-member cluster succeeded, want a refusal")
	}
	if !strings.Contains(err.Error(), ReplicasEnvVar) {
		t.Fatalf("the refusal = %v, want it to name %s", err, ReplicasEnvVar)
	}
}

// raiseWatcher is a JetStream context on a real cluster that counts the stream reads a start makes
// and can answer the first behind of them, once the raise has been written, as a cluster whose new
// copy has not caught up answers: that copy not current, with a lag. nats-server reports exactly
// that on a copy a raise has just added, and hands it the direct-get queue group at 90% caught up
// anyway (server/jetstream_cluster.go:2622-2641, v2.10.29), so a KV read taken then can miss a key
// the bucket holds.
type raiseWatcher struct {
	nats.JetStreamContext
	replicas int
	behind   int
}

func (r *raiseWatcher) StreamInfo(stream string, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	info, err := r.JetStreamContext.StreamInfo(stream, opts...)
	if err != nil || info.Config.Replicas != r.replicas || r.behind == 0 || info.Cluster == nil || len(info.Cluster.Replicas) == 0 {
		return info, err
	}
	r.behind--
	lagging := *info
	cluster := *info.Cluster
	peers := make([]*nats.PeerInfo, len(cluster.Replicas))
	for i, peer := range cluster.Replicas {
		copied := *peer
		peers[i] = &copied
	}
	peers[0].Current = false
	peers[0].Lag = 7
	cluster.Replicas = peers
	lagging.Cluster = &cluster
	return &lagging, nil
}

// A start that raises a bucket's copies answers only once the cluster reports every copy current,
// because the caller reads keys the moment it does (the webhook redelivery sweep reads its cursor
// and its index) and a copy the raise added can be handed a direct get while it is still catching
// up, which answers "key not found" for a key the bucket holds. The cluster here is real and so is
// the raise; the stream reads answer as a cluster with a copy still catching up does, for the
// first two reads after the raise. Without the wait the start answers while they are outstanding,
// which is what this row fails on; with it, every one is read and the start answers after them.
func TestARaiseAnswersOnlyOnceTheCopiesAreCurrent(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	conn, js := memberJetStream(t, cluster, 0)
	if _, err := EnsureKeyValue(conn, js, &nats.KeyValueConfig{Bucket: catchupBucket, Storage: nats.FileStorage, Replicas: 1}); err != nil {
		t.Fatalf("create the one-copy bucket: %v", err)
	}
	watcher := &raiseWatcher{JetStreamContext: js, replicas: 3, behind: 2}
	kv, err := EnsureKeyValue(conn, watcher, &nats.KeyValueConfig{Bucket: catchupBucket, Storage: nats.FileStorage, Replicas: 3})
	if err != nil {
		t.Fatalf("raise the bucket to three copies: %v", err)
	}
	if watcher.behind != 0 {
		t.Fatalf("the raise answered with %d catching-up reads still to come, want it to wait for every copy to be current", watcher.behind)
	}
	if _, err := kv.ReadStreamState(context.Background()); err != nil {
		t.Fatalf("read the raised bucket's stream: %v", err)
	}
}

// A start that raises nothing waits for nothing: the common start, every start after the first,
// comes up at the speed of the bucket it found. The copies here are already the deployment's, so
// the raise leaves the bucket alone, and the stream reads answer as a cluster with a copy that
// never catches up does, for more reads than any run could take. The start's clock stands still,
// so a wait that ran whether or not the raise raised would sleep on it for ever: the start
// answering at all is what "no wait" looks like from the outside.
func TestAStartThatRaisesNothingWaitsForNothing(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	conn, js := memberJetStream(t, cluster, 0)
	config := &nats.KeyValueConfig{Bucket: catchupBucket, Storage: nats.FileStorage, Replicas: 3}
	if _, err := EnsureKeyValue(conn, js, config); err != nil {
		t.Fatalf("create the three-copy bucket: %v", err)
	}
	watcher := &raiseWatcher{JetStreamContext: js, replicas: 3, behind: 1 << 30}
	if err := testwait.Receive(t, "the start that raises nothing to answer with its clock standing still", ensuredOn(stillClock(), conn, watcher, config)); err != nil {
		t.Fatalf("ensure the bucket a start raises nothing on: %v", err)
	}
}

// The value a raise kept is readable through the handle the raise answered with, on a real
// three-member cluster: the row above makes the wait happen, this one shows the bucket the wait
// hands over is the one the caller reads keys from.
func TestARaisedBucketServesItsKeysAtOnce(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	conn, js := memberJetStream(t, cluster, 0)
	created, err := EnsureKeyValue(conn, js, &nats.KeyValueConfig{Bucket: catchupBucket, Storage: nats.FileStorage, Replicas: 1})
	if err != nil {
		t.Fatalf("create the one-copy bucket: %v", err)
	}
	if _, err := created.Put("key", []byte("value")); err != nil {
		t.Fatalf("put: %v", err)
	}
	kv, err := EnsureKeyValue(conn, js, &nats.KeyValueConfig{Bucket: catchupBucket, Storage: nats.FileStorage, Replicas: 3})
	if err != nil {
		t.Fatalf("raise the bucket to three copies: %v", err)
	}
	entry, err := kv.Get("key")
	if err != nil || string(entry.Value()) != "value" {
		t.Fatalf("get after the raise = %v, %v; want value", entry, err)
	}
}

// A raise whose copies never report current fails the start at the deadline, naming the bucket's
// stream, the copies it asked for and what the copy that was not current reported. It never hands
// the caller the bucket anyway: a start that does not come up is better than one that reads a
// stale "key not found" off a copy still catching up and asks GitHub for a redelivery it made. The
// wait sleeps on the start's clock, which the row moves to just short of the deadline, where the
// start is still waiting, and then to the end of the sleep that takes it past the deadline, where
// it fails.
func TestARaiseWhoseCopiesNeverCatchUpFailsTheStart(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	conn, js := memberJetStream(t, cluster, 0)
	if _, err := EnsureKeyValue(conn, js, &nats.KeyValueConfig{Bucket: catchupBucket, Storage: nats.FileStorage, Replicas: 1}); err != nil {
		t.Fatalf("create the one-copy bucket: %v", err)
	}
	watcher := &raiseWatcher{JetStreamContext: js, replicas: 3, behind: 1 << 30}
	clock := stillClock()
	start := clock.Now()
	answered := ensuredOn(clock, conn, watcher, &nats.KeyValueConfig{Bucket: catchupBucket, Storage: nats.FileStorage, Replicas: 3})
	if slept := clock.AwaitArmed(t, "the raise's first sleep between reads of copies not current", 1)[0]; !slept.Equal(start.Add(bucketAppliedInterval)) {
		t.Fatalf("the raise's first sleep ends at %s, want %s after the start", slept.Sub(start), bucketAppliedInterval)
	}
	short := start.Add(bucketAppliedDeadline - time.Nanosecond)
	clock.Set(short)
	last := clock.AwaitArmed(t, "the raise's sleep just short of its deadline", 1)[0]
	if !last.Equal(short.Add(bucketAppliedInterval)) {
		t.Fatalf("the raise just short of its deadline sleeps until %s after the start, want %s", last.Sub(start), bucketAppliedDeadline-time.Nanosecond+bucketAppliedInterval)
	}
	if len(answered) != 0 {
		t.Fatalf("the raise answered %v just short of its %s deadline, want it still waiting", <-answered, bucketAppliedDeadline)
	}
	// The start fails on the read it takes once the clock is past the deadline, and names what that
	// read reported. The watcher holds a copy back only in a read that names the stream's leader and
	// its other copies; a read taken before the cluster has elected the raised stream's leader says
	// only that it has none. The clock moves only at the row's word, so that read comes as fast as
	// the row moves it, and the row first waits for the cluster to name them.
	testwait.Until(t, "the cluster to name the raised stream's leader and its two other copies", func() (*nats.ClusterInfo, bool) {
		info, err := js.StreamInfo("KV_" + catchupBucket)
		if err != nil || info.Cluster == nil {
			return nil, false
		}
		return info.Cluster, info.Cluster.Leader != "" && len(info.Cluster.Replicas) == 2
	})
	clock.Set(last)
	err := testwait.Receive(t, "the raise to fail once its deadline has passed", answered)
	if err == nil {
		t.Fatal("the raise answered with a copy that never caught up, want it to fail the start")
	}
	for _, want := range []string{"KV_" + catchupBucket, "3 copies", "lag 7"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the error = %v, want it to name %q", err, want)
		}
	}
}
