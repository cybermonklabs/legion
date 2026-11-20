package bus

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/testnats"
)

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
	if _, err := EnsureKeyValue(t.Context(), first, firstJS, config()); err != nil {
		t.Fatalf("the first start could not create the bucket: %v", err)
	}
	second, secondJS := memberJetStream(t, cluster, 1)
	behind := &behindMember{JetStreamContext: secondJS, misses: 1}
	kv, err := EnsureKeyValue(t.Context(), second, behind, config())
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

// refusalWait is how long a refused create may take before this row calls it a wait rather than an
// answer: a server's refusal is one round trip, so a refusal that took this long waited for
// something.
const refusalWait = 2 * time.Second

// A create the server refuses is an answer, not a member that has not caught up, so it comes back
// at once and nothing waits it out: a bucket asking for more copies than the cluster has members
// is refused ("replicas > clusters size"), named with the setting that asked for them
// (refusedSettings).
func TestARefusedCreateFailsAtOnceRatherThanWaitingOutTheDeadline(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	conn, js := memberJetStream(t, cluster, 0)
	start := time.Now()
	_, err := EnsureKeyValue(t.Context(), conn, js, &nats.KeyValueConfig{
		Bucket:   catchupBucket,
		Storage:  nats.FileStorage,
		Replicas: 5,
	})
	waited := time.Since(start)
	if err == nil {
		t.Fatalf("ensure a bucket with 5 copies on a 3-member cluster succeeded, want a refusal")
	}
	if !strings.Contains(err.Error(), ReplicasEnvVar) {
		t.Fatalf("the refusal = %v, want it to name %s", err, ReplicasEnvVar)
	}
	if waited >= refusalWait {
		t.Fatalf("the refusal took %s, want it answered within %s rather than waited out", waited.Round(time.Millisecond), refusalWait)
	}
}

// raiseWatcher is a JetStream context on a real cluster that counts the stream reads a start makes
// and can answer the first behind of them, once the raise has been written, as a cluster whose new
// copy has not caught up answers: that copy not current, with a lag. nats-server reports exactly
// that on a copy a raise has just added, and hands it the direct-get queue group at 90% caught up
// anyway (server/jetstream_cluster.go:2622-2641, v2.10.29), so a KV read taken then can miss a key
// the bucket holds. A read that finds the stream with no leader yet, as the reads right after a
// raise do, is answered as it is: a copy is behind its leader only once there is one. With
// laggedThen set, the read after the first lagging answer calls it first, so a row can end the
// caller's context once the start has read a copy as behind, on that fact and not on a clock.
// Its counts are atomic, because a read the start abandons at its read limit runs on beside the
// read sent after it.
type raiseWatcher struct {
	nats.JetStreamContext
	replicas   int
	behind     atomic.Int64
	lagged     atomic.Int64
	laggedThen func()
}

// newRaiseWatcher is a raiseWatcher over js for a stream of replicas copies whose first behind
// reads lag.
func newRaiseWatcher(js nats.JetStreamContext, replicas int, behind int64, laggedThen func()) *raiseWatcher {
	watcher := &raiseWatcher{JetStreamContext: js, replicas: replicas, laggedThen: laggedThen}
	watcher.behind.Store(behind)
	return watcher
}

func (r *raiseWatcher) StreamInfo(stream string, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	if r.lagged.Load() > 0 && r.laggedThen != nil {
		r.laggedThen()
	}
	info, err := r.JetStreamContext.StreamInfo(stream, opts...)
	if err != nil || info.Config.Replicas != r.replicas || info.Cluster == nil || info.Cluster.Leader == "" || len(info.Cluster.Replicas) == 0 {
		return info, err
	}
	if r.behind.Add(-1) < 0 {
		r.behind.Add(1)
		return info, nil
	}
	r.lagged.Add(1)
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
	if _, err := EnsureKeyValue(t.Context(), conn, js, &nats.KeyValueConfig{Bucket: catchupBucket, Storage: nats.FileStorage, Replicas: 1}); err != nil {
		t.Fatalf("create the one-copy bucket: %v", err)
	}
	watcher := newRaiseWatcher(js, 3, 2, nil)
	kv, err := EnsureKeyValue(t.Context(), conn, watcher, &nats.KeyValueConfig{Bucket: catchupBucket, Storage: nats.FileStorage, Replicas: 3})
	if err != nil {
		t.Fatalf("raise the bucket to three copies: %v", err)
	}
	if behind := watcher.behind.Load(); behind != 0 {
		t.Fatalf("the raise answered with %d catching-up reads still to come, want it to wait for every copy to be current", behind)
	}
	if _, err := kv.ReadStreamState(context.Background()); err != nil {
		t.Fatalf("read the raised bucket's stream: %v", err)
	}
}

// A start that raises nothing waits for nothing: the common start, every start after the first,
// comes up at the speed of the bucket it found. The copies here are already the deployment's, so
// the raise leaves the bucket alone, and the stream reads answer as a cluster with a copy that
// never catches up does, for more reads than any run could take. The raise's own read of the
// stream is the one read the start makes through the watcher: a wait for the copies would read it
// again and again, until the context below ended, and this row counts the reads, under a first
// read limit of the caller's whole wait (countedReads), so a read a loaded machine answers late is
// not counted twice.
func TestAStartThatRaisesNothingWaitsForNothing(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	conn, js := memberJetStream(t, cluster, 0)
	config := &nats.KeyValueConfig{Bucket: catchupBucket, Storage: nats.FileStorage, Replicas: 3}
	if _, err := EnsureKeyValue(t.Context(), conn, js, config); err != nil {
		t.Fatalf("create the three-copy bucket: %v", err)
	}
	const unlimited = 1 << 30
	watcher := newRaiseWatcher(js, 3, unlimited, nil)
	// A hang guard: a start that waited for the copies would wait until this context ended.
	ctx, cancel := context.WithTimeout(countedReads(t.Context()), time.Minute)
	defer cancel()
	if _, err := EnsureKeyValue(ctx, conn, watcher, config); err != nil {
		t.Fatalf("ensure the bucket a start raises nothing on: %v", err)
	}
	if reads := unlimited - watcher.behind.Load(); reads != 1 {
		t.Fatalf("the start that raised nothing read the stream %d times through the watcher, want once: it waits for no copy", reads)
	}
}

// The value a raise kept is readable through the handle the raise answered with, on a real
// three-member cluster: the row above makes the wait happen, this one shows the bucket the wait
// hands over is the one the caller reads keys from.
func TestARaisedBucketServesItsKeysAtOnce(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	conn, js := memberJetStream(t, cluster, 0)
	created, err := EnsureKeyValue(t.Context(), conn, js, &nats.KeyValueConfig{Bucket: catchupBucket, Storage: nats.FileStorage, Replicas: 1})
	if err != nil {
		t.Fatalf("create the one-copy bucket: %v", err)
	}
	if _, err := created.Put("key", []byte("value")); err != nil {
		t.Fatalf("put: %v", err)
	}
	kv, err := EnsureKeyValue(t.Context(), conn, js, &nats.KeyValueConfig{Bucket: catchupBucket, Storage: nats.FileStorage, Replicas: 3})
	if err != nil {
		t.Fatalf("raise the bucket to three copies: %v", err)
	}
	entry, err := kv.Get("key")
	if err != nil || string(entry.Value()) != "value" {
		t.Fatalf("get after the raise = %v, %v; want value", entry, err)
	}
}

// A raise whose copies never report current fails the start when the caller's context ends,
// naming the bucket's stream, the copies it asked for and what the copy that was not current
// reported, with the context's error. It never hands the caller the bucket anyway: a start that
// does not come up is better than one that reads a stale "key not found" off a copy still
// catching up and asks GitHub for a redelivery it made. The caller's context ends, with
// context.DeadlineExceeded as its cause, once the start has read a copy as behind, so the row reads
// no clock and no load can end the context before the raise.
func TestARaiseWhoseCopiesNeverCatchUpFailsTheStart(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	conn, js := memberJetStream(t, cluster, 0)
	if _, err := EnsureKeyValue(t.Context(), conn, js, &nats.KeyValueConfig{Bucket: catchupBucket, Storage: nats.FileStorage, Replicas: 1}); err != nil {
		t.Fatalf("create the one-copy bucket: %v", err)
	}
	ctx, end := context.WithCancelCause(t.Context())
	defer end(nil)
	watcher := newRaiseWatcher(js, 3, 1<<30, func() { end(context.DeadlineExceeded) })
	_, err := EnsureKeyValue(ctx, conn, watcher, &nats.KeyValueConfig{Bucket: catchupBucket, Storage: nats.FileStorage, Replicas: 3})
	if err == nil {
		t.Fatal("the raise answered with a copy that never caught up, want it to fail the start")
	}
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errHangGuard) {
		t.Fatalf("the error = %v, want the caller's context.DeadlineExceeded in it, not the start's hang guard", err)
	}
	for _, want := range []string{"KV_" + catchupBucket, "3 copies", "lag 7"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the error = %v, want it to name %q", err, want)
		}
	}
}

// A raise whose copies never report current, under a caller whose context never ends, fails the
// start at the start's hang guard rather than hanging it, and the error says so: the bucket's
// stream and the guard. The guard is shortened here (ensureKeyValue); EnsureKeyValue's is
// startHangGuard. It is long against a raise's answer (2.7 s at the slowest measured under load),
// so the guard passes in the wait for the copies, and the error that names them is held with the
// real guard by the held-election run of the issue's verification.
func TestARaiseThatNeverSettlesEndsAtTheHangGuard(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	conn, js := memberJetStream(t, cluster, 0)
	if _, err := EnsureKeyValue(t.Context(), conn, js, &nats.KeyValueConfig{Bucket: catchupBucket, Storage: nats.FileStorage, Replicas: 1}); err != nil {
		t.Fatalf("create the one-copy bucket: %v", err)
	}
	watcher := newRaiseWatcher(js, 3, 1<<30, nil)
	_, err := ensureKeyValue(context.Background(), 5*time.Second, conn, watcher, &nats.KeyValueConfig{Bucket: catchupBucket, Storage: nats.FileStorage, Replicas: 3})
	if !errors.Is(err, errHangGuard) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the error = %v, want the start's hang guard, with context.DeadlineExceeded", err)
	}
	for _, want := range []string{"raise KV_" + catchupBucket, "hang guard passed after 5s"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the error = %v, want it to name %q", err, want)
		}
	}
}

// A member that never applies a create is waited for until the caller's context ends, and the
// error carries both that end and the member's own answer, "bucket not found".
func TestAMemberThatNeverAppliesACreateEndsAtTheCallersContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	reads := 0
	_, err := awaitBucketApplied(ctx, catchupBucket, bucketMissing, func() (struct{}, error) {
		reads++
		return struct{}{}, nats.ErrBucketNotFound
	})
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, nats.ErrBucketNotFound) {
		t.Fatalf("the error = %v, want both context.DeadlineExceeded and nats.ErrBucketNotFound", err)
	}
	if reads < 2 {
		t.Fatalf("the member was read %d times, want it read again while the context lasted", reads)
	}
}
