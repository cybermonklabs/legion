//go:build odatrace

package writerhold

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/writerhold/holdtest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/natsgrant"
)

// The hold's fence against work accepted before the restart, forced on the hooked nats-server
// (packages/envoy/scripts/forced-order.sh): each row holds a client's work at one point inside a
// server (internal/testnats/hooks.go) and shows the hold's own order, the restart and then the
// barrier, keeps that work from landing after the listing or the read that relies on it. Each
// row's control is the same schedule without that order, which reproduces the danger.

// held is how long a row watches for what a hold keeps from happening; noAnswerWithin how long a
// read waits before it counts as unanswered.
const (
	held           = 500 * time.Millisecond
	noAnswerWithin = 2 * time.Second
	reachWithin    = 30 * time.Second
)

// forcedHub is a three-member hub with the hook installed, its Kubernetes objects, and the
// operator's connection, which holds every subject (the plain cluster has no authorization).
type forcedHub struct {
	cluster  *testnats.Cluster
	kube     *holdtest.Kube
	hub      KubeHub
	hooks    *testnats.Hooks
	operator *natsgo.Conn
	js       jetstream.JetStream
	declared Declared
}

func newForcedHub(t *testing.T) *forcedHub {
	t.Helper()
	f := &forcedHub{cluster: testnats.StartCluster(t, 3)}
	f.kube = holdtest.NewKube(t, f.cluster)
	f.hub = hubOf(f.kube)
	f.hooks = testnats.Hook(t)
	var err error
	f.operator, err = natsgo.Connect(f.cluster.URL(), natsgo.CustomInboxPrefix(natsgrant.OperatorInbox), natsgo.Name("operator/holdtest"),
		natsgo.Timeout(10*time.Second), natsgo.MaxReconnects(-1), natsgo.ReconnectWait(100*time.Millisecond))
	require.NoError(t, err)
	t.Cleanup(f.operator.Close)
	f.js, err = jetstream.New(f.operator)
	require.NoError(t, err)
	f.declared, err = Discover(rowContext(t), f.hub)
	require.NoError(t, err)
	return f
}

// connect connects a writer named name to member i alone, with no reconnect: a client whose
// connection the restart closes stays closed, as one the hold refuses would.
func (f *forcedHub) connect(t *testing.T, i int, name string) *natsgo.Conn {
	t.Helper()
	nc, err := natsgo.Connect(f.cluster.Servers[i].URL(), natsgo.Name(name), natsgo.Timeout(10*time.Second), natsgo.NoReconnect())
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	return nc
}

// metaLeader is the index of the member that leads the meta group, by /varz.
func (f *forcedHub) metaLeader(t *testing.T) int {
	t.Helper()
	return metaLeaderMember(t, f.hub, f.declared)
}

// streamLeader is the index of the member that leads stream.
func (f *forcedHub) streamLeader(t *testing.T, stream string) int {
	t.Helper()
	leader, err := streamLeader(rowContext(t), f.operator, stream)
	require.NoError(t, err)
	i := f.cluster.Member(leader)
	require.GreaterOrEqual(t, i, 0, "stream %s's leader %q", stream, leader)
	return i
}

// consumerNames is the operator's CONSUMER.NAMES listing of stream behind the meta barrier.
func (f *forcedHub) consumerNames(t *testing.T, stream, holdID string) []string {
	t.Helper()
	var names []string
	barrier := MetaBarrier{Hub: f.hub, Declared: f.declared, HoldID: holdID}
	require.NoError(t, barrier.Run(rowContext(t), f.operator, func(ctx context.Context) error {
		var err error
		names, err = listAll(ctx, f.operator, StepBarrier, fmt.Sprintf(natsserver.JSApiConsumersT, stream), streamChecks{},
			func(page *listPage[string]) []string { return page.Consumers }, func(name string) string { return name })
		return err
	}))
	return names
}

// answer runs request on a goroutine of its own and hands back its error.
func answer(request func() error) <-chan error {
	done := make(chan error, 1)
	go func() { done <- request() }()
	return done
}

// answered fails t unless done answers nil within reachWithin.
func answered(t *testing.T, done <-chan error, what string) {
	t.Helper()
	select {
	case err := <-done:
		require.NoError(t, err, what)
	case <-time.After(reachWithin):
		t.Fatalf("%s: no answer within %s", what, reachWithin)
	}
}

// unanswered fails t when done answers within held.
func unanswered(t *testing.T, done <-chan error, what string) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("%s answered while held (%v)", what, err)
	case <-time.After(held):
	}
}

// ended waits for done, answered or failed, and returns its error.
func ended(t *testing.T, done <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(reachWithin):
		t.Fatalf("%s did not end within %s", what, reachWithin)
		return nil
	}
}

// TestForcedOrderHoldACreateAcrossTheRestart shows a consumer create held at the meta leader's
// api.worker, and released by that server's own Shutdown (the eviction's stop, which releases a
// hold before it waits for the server's goroutines), lands before the hold's listing or not at all:
// its server restarts, then the meta barrier and CONSUMER.NAMES follow, and a later barrier and
// listing agree with it. A hold released after the restart cannot be built: Shutdown waits for the
// parked goroutine (nats-server 2.15.1 server.go:2759), so the stop would hang. Control, run first:
// the same create held and released only after the barrier and the listing, with no restart, lands
// after the final listing (red team H1). It fails when the barrier does not order the listing after
// every create the meta log holds, or when a stop leaves a create parked to land later.
func TestForcedOrderHoldACreateAcrossTheRestart(t *testing.T) {
	f := newForcedHub(t)
	ctx := rowContext(t)
	_, err := f.js.CreateStream(ctx, jetstream.StreamConfig{Name: "ENVOY_NOTIFICATIONS", Subjects: []string{"notifications.>"}, Replicas: 3})
	require.NoError(t, err)
	leader := f.metaLeader(t)
	writer, err := jetstream.New(f.connect(t, (leader+1)%3, "writer"))
	require.NoError(t, err)
	create := func(name string) <-chan error {
		return answer(func() error {
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			_, err := writer.CreateConsumer(ctx, "ENVOY_NOTIFICATIONS", jetstream.ConsumerConfig{Durable: name, AckPolicy: jetstream.AckExplicitPolicy})
			return err
		})
	}
	// jetstream's CreateConsumer of a named consumer with no filter sends CONSUMER.CREATE.<stream>.<name>.
	subject := func(name string) testnats.Match {
		return testnats.Subject("$JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS." + name)
	}

	hold := f.hooks.Hold(testnats.APIWorker, testnats.OnServer(f.cluster.Servers[leader]), subject("late"))
	done := create("late")
	t.Logf("control: held %s", hold.Reached(t))
	require.NotContains(t, f.consumerNames(t, "ENVOY_NOTIFICATIONS", "hold1"), "late")
	hold.Release()
	answered(t, done, "control: the create released after the final listing")
	require.Contains(t, f.consumerNames(t, "ENVOY_NOTIFICATIONS", "hold1"), "late", "control: the create landed after the final listing")

	leader = f.metaLeader(t)
	writer, err = jetstream.New(f.connect(t, (leader+1)%3, "writer2"))
	require.NoError(t, err)
	hold = f.hooks.Hold(testnats.APIWorker, testnats.OnServer(f.cluster.Servers[leader]), subject("stale"), testnats.After(f.hooks.Mark()))
	done = create("stale")
	t.Logf("held %s", hold.Reached(t))
	evict(t, f.kube, holdtest.Pod(leader))
	require.True(t, hold.Released(), "the eviction's stop released the hold")
	outcome := ended(t, done, "the create held across the restart")
	f.cluster.AwaitReady(t)
	listed := slices.Contains(f.consumerNames(t, "ENVOY_NOTIFICATIONS", "hold2"), "stale")
	t.Logf("the held create ended with %v; listed after the restart: %t", outcome, listed)
	if outcome == nil {
		require.True(t, listed, "the create was answered and the listing after the barrier misses it")
	}
	require.Equal(t, listed, slices.Contains(f.consumerNames(t, "ENVOY_NOTIFICATIONS", "hold3"), "stale"),
		"the create held across the restart landed after the listing")
}

// TestForcedOrderHoldAKVWriteAcrossTheRestart shows a KV write held at client.publish on its
// writer's server, released by that server's own Shutdown, does not land in the bucket recreated
// after the restart: the bucket barrier and the read find no trace of it. Control, run first: the
// same write held and released after the recreate, with no restart, lands in the recreated bucket
// (red team H1). It fails when the restart does not end the writer's parked publish before the
// recreate.
func TestForcedOrderHoldAKVWriteAcrossTheRestart(t *testing.T) {
	f := newForcedHub(t)
	ctx := rowContext(t)
	const bucket = "envoy_sessions"
	config := jetstream.KeyValueConfig{Bucket: bucket, Replicas: 3}
	_, err := f.js.CreateKeyValue(ctx, config)
	require.NoError(t, err)
	recreate := func() {
		t.Helper()
		require.NoError(t, f.js.DeleteKeyValue(ctx, bucket))
		testwait.Eventually(t, "the bucket made again", func() bool {
			_, err := f.js.CreateKeyValue(ctx, config)
			return err == nil
		})
	}
	read := func(key string) (string, error) {
		var value string
		err := ReadBucket(ctx, f.operator, bucket, "hold1", func(ctx context.Context) error {
			var err error
			value, err = lastBySubject(ctx, f.operator, bucket, key)
			return err
		})
		return value, err
	}
	const member = 0
	writerConn := f.connect(t, member, "writer")
	writer, err := jetstream.New(writerConn)
	require.NoError(t, err)
	kv, err := writer.KeyValue(ctx, bucket)
	require.NoError(t, err)
	put := func(key string) <-chan error {
		return answer(func() error {
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			_, err := kv.Put(ctx, key, []byte("stale"))
			return err
		})
	}
	holdPut := func(key string, after int) *testnats.Hold {
		return f.hooks.Hold(testnats.ClientPublish, testnats.OnServer(f.cluster.Servers[member]), testnats.ClientNamed("writer"),
			testnats.Subject("$KV."+bucket+"."+key), testnats.After(after))
	}

	hold := holdPut("late", 0)
	done := put("late")
	t.Logf("control: held %s", hold.Reached(t))
	recreate()
	hold.Release()
	answered(t, done, "control: the write released after the recreate")
	value, err := read("late")
	require.NoError(t, err)
	require.Equal(t, "stale", value, "control: the stale write landed in the recreated bucket")

	hold = holdPut("stale", f.hooks.Mark())
	done = put("stale")
	t.Logf("held %s", hold.Reached(t))
	evict(t, f.kube, holdtest.Pod(member))
	require.True(t, hold.Released(), "the eviction's stop released the hold")
	t.Logf("the held write ended with %v", ended(t, done, "the write held across the restart"))
	f.cluster.AwaitReady(t)
	recreate()
	_, err = read("stale")
	var apiErr *jetstream.APIError
	require.True(t, errors.As(err, &apiErr) && apiErr.ErrorCode == jetstream.JSErrCodeMessageNotFound, "the stale write is in the recreated bucket: %v", err)
	info, err := f.js.Stream(ctx, kvStreamPrefix+bucket)
	require.NoError(t, err)
	require.EqualValues(t, 1, info.CachedInfo().State.Msgs, "the recreated bucket holds the barrier alone")
}

// msgGet is one STREAM.MSG.GET of the last record of key in the session consumers' bucket, as the
// stream's leader alone answers it: the record, a not-found answer, or no answer within
// noAnswerWithin.
type msgGet struct {
	value      string
	notFound   bool
	unanswered bool
	err        error
}

func (m msgGet) String() string {
	switch {
	case m.err != nil:
		return "error " + m.err.Error()
	case m.unanswered:
		return "no answer"
	case m.notFound:
		return "not found"
	}
	return fmt.Sprintf("%q", m.value)
}

// get reads key with STREAM.MSG.GET. It runs on whatever goroutine reads, so an unexpected error is
// part of its answer, never a failure of t.
func (f *forcedHub) get(key string) msgGet {
	ctx, cancel := context.WithTimeout(context.Background(), noAnswerWithin)
	defer cancel()
	value, err := lastBySubject(ctx, f.operator, bucketBarrierBucket, key)
	var apiErr *jetstream.APIError
	switch {
	case err == nil:
		return msgGet{value: value}
	case errors.As(err, &apiErr) && apiErr.ErrorCode == jetstream.JSErrCodeMessageNotFound:
		return msgGet{notFound: true}
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, natsgo.ErrTimeout) || errors.Is(err, natsgo.ErrNoResponders):
		return msgGet{unanswered: true}
	}
	return msgGet{err: err}
}

// bucketBarrierBucket is the bucket the bucket barrier's rows run on, the session consumers'.
const bucketBarrierBucket = "envoy_session_consumers"

// barrierBucket makes the R3 session consumers' bucket and returns its stream's leader.
func (f *forcedHub) barrierBucket(t *testing.T) int {
	t.Helper()
	_, err := f.js.CreateKeyValue(rowContext(t), jetstream.KeyValueConfig{Bucket: bucketBarrierBucket, Replicas: 3})
	require.NoError(t, err)
	return f.streamLeader(t, kvStreamPrefix+bucketBarrierBucket)
}

// holdApply holds the apply of key's write on each member of on.
func (f *forcedHub) holdApply(on []int, key string) []*testnats.Hold {
	holds := make([]*testnats.Hold, len(on))
	mark := f.hooks.Mark()
	for i, member := range on {
		holds[i] = f.hooks.Hold(testnats.StreamApply, testnats.OnServer(f.cluster.Servers[member]), testnats.Stream(kvStreamPrefix+bucketBarrierBucket),
			testnats.Subject("$KV."+bucketBarrierBucket+"."+key), testnats.After(mark))
	}
	return holds
}

// putAsync writes key on the operator's connection and hands back the write's acknowledgement.
func (f *forcedHub) putAsync(t *testing.T, key string) <-chan error {
	t.Helper()
	kv, err := f.js.KeyValue(rowContext(t), bucketBarrierBucket)
	require.NoError(t, err)
	return answer(func() error {
		ctx, cancel := context.WithTimeout(rowContext(t), reachWithin)
		defer cancel()
		_, err := kv.Put(ctx, key, []byte("written"))
		return err
	})
}

// followers are the members other than leader.
func followers(leader int) []int {
	var others []int
	for i := range 3 {
		if i != leader {
			others = append(others, i)
		}
	}
	return others
}

// raftView is one reading of the bucket's Raft group on each of members, by its own /raftz: the
// members that lead it, and what each answered. It runs on whatever goroutine asks.
func (f *forcedHub) raftView(members []int) (leading []int, seen map[int]string) {
	ctx := context.Background()
	seen = map[int]string{}
	for _, i := range members {
		monitor, stop, err := f.hub.Monitor(ctx, holdtest.Pod(i))
		if err != nil {
			seen[i] = err.Error()
			continue
		}
		var raftz map[string]map[string]struct {
			ID        string `json:"id"`
			State     string `json:"state"`
			Leader    string `json:"leader"`
			Term      uint64 `json:"term"`
			Committed uint64 `json:"committed"`
			Applied   uint64 `json:"applied"`
		}
		err = monitor.Get(ctx, "/raftz?acc=%24G", &raftz)
		stop()
		if err != nil {
			seen[i] = err.Error()
			continue
		}
		seen[i] = fmt.Sprintf("%+v", raftz["$G"])
		for _, group := range raftz["$G"] {
			if group.ID != "" && group.ID == group.Leader {
				leading = append(leading, i)
			}
		}
	}
	return leading, seen
}

// TestForcedOrderHoldBucketBarrierWithinOneTerm shows, on an R3 bucket, a committed write whose
// apply its stream leader holds (stream.apply): the bucket barrier's acknowledgement waits for the
// held apply, and the read after it returns the write. Control: STREAM.MSG.GET with no barrier, from
// the same leader, misses the write. It fails when the barrier is not a write through the same
// stream, or the read does not wait for its acknowledgement.
func TestForcedOrderHoldBucketBarrierWithinOneTerm(t *testing.T) {
	f := newForcedHub(t)
	leader := f.barrierBucket(t)
	holds := f.holdApply([]int{leader}, "K")
	written := f.putAsync(t, "K")
	t.Logf("held %s", holds[0].Reached(t))

	control := f.get("K")
	require.True(t, control.notFound, "control: STREAM.MSG.GET with no barrier answered %s, want the leader's miss", control)

	got := make(chan msgGet, 1)
	done := answer(func() error {
		return ReadBucket(rowContext(t), f.operator, bucketBarrierBucket, "hold1", func(context.Context) error {
			got <- f.get("K")
			return nil
		})
	})
	unanswered(t, done, "the read behind the barrier")
	holds[0].Release()
	answered(t, done, "the read behind the barrier")
	read := <-got
	require.Equal(t, "written", read.value, "the read after the barrier answered %s", read)
	answered(t, written, "the held write")
}

// TestForcedOrderHoldBucketBarrierAcrossTheRestart shows, on an R3 bucket, a write that a quorum
// stored and committed before its leader's stop, with every follower's apply of it held: no follower
// stands for leader while it has a committed entry it has not applied (nats-server 2.10.29
// raft.go:4212-4217, 2.15.1 raft.go:6250-6255), so while the old leader is down no follower leads,
// STREAM.MSG.GET with no barrier (the control) gets no answer and never a stale one, and once the
// old leader is back it leads and the read behind the barrier returns the write, the followers'
// applies still held. The plan's 2.10.29 miss needs a new leader that stored the write without
// learning its commit; the hooks cannot keep that across the old leader's stop (its Shutdown
// releases every hold on it, and a routed append has no hold point), so the miss is forced on
// neither version. It fails when a follower with an unapplied committed write leads, or the read
// behind the barrier returns without the write.
func TestForcedOrderHoldBucketBarrierAcrossTheRestart(t *testing.T) {
	f := newForcedHub(t)
	leader := f.barrierBucket(t)
	others := followers(leader)
	holds := f.holdApply(others, "K")
	answered(t, f.putAsync(t, "K"), "the write, acknowledged by the leader before its stop")
	for _, hold := range holds {
		t.Logf("held %s", hold.Reached(t))
	}
	// The old leader is down for as long as a pod is: the control's read and a look at the
	// followers' Raft view are made then.
	var control msgGet
	var leading []int
	var seen map[int]string
	f.kube.WhileDown(func(int) {
		control = f.get("K")
		leading, seen = f.raftView(others)
	})
	evict(t, f.kube, holdtest.Pod(leader))
	f.kube.WhileDown(nil)
	t.Logf("while the old leader was down: STREAM.MSG.GET answered %s; the followers' Raft view %v", control, seen)
	require.False(t, control.notFound || control.value != "", "control: STREAM.MSG.GET answered %s with the old leader down, want no answer and no stale one", control)
	require.Empty(t, leading, "a follower with an unapplied committed write leads: %v", seen)
	f.cluster.AwaitReady(t)

	got := make(chan msgGet, 1)
	answered(t, answer(func() error {
		return ReadBucket(rowContext(t), f.operator, bucketBarrierBucket, "hold1", func(context.Context) error {
			got <- f.get("K")
			return nil
		})
	}), "the read behind the barrier, the followers' applies still held")
	read := <-got
	require.Equal(t, "written", read.value, "the read after the barrier answered %s", read)
	back := f.streamLeader(t, kvStreamPrefix+bucketBarrierBucket)
	require.Equal(t, leader, back, "the bucket's leader after the restart is %s, not the old leader", f.cluster.Name(back))
	for _, hold := range holds {
		hold.Release()
	}
}

// TestForcedOrderHoldBucketBarrierLeaderChange shows a leader change between the bucket barrier's
// acknowledgement and the read: the leader read after the read differs from the one before the
// barrier, so the barrier is written again, and acknowledged by the new leader, before the read is
// made again. The change is a stepdown inside the first read, made again until a follower leads.
// Control, run first: the same schedule with the leader read after the read left out returns after
// the first read, and writes no barrier again. The read the change makes stale cannot be forced: a
// follower whose apply the hooks hold does not stand for leader (TestForcedOrderHoldBucketBarrier
// AcrossTheRestart), so the row shows the rule, not a stale read. It fails when the read does not
// read the stream's leader again after the read, or reads again with no new barrier.
func TestForcedOrderHoldBucketBarrierLeaderChange(t *testing.T) {
	f := newForcedHub(t)
	f.barrierBucket(t)
	stream := kvStreamPrefix + bucketBarrierBucket
	for _, run := range []struct {
		name    string
		key     string
		control bool
	}{{"control", "C", true}, {"the row", "K", false}} {
		answered(t, f.putAsync(t, run.key), run.name+": the write")
		barrierSubject := testnats.Subject("$KV." + bucketBarrierBucket + ".hold.barrier." + run.key)
		// The reads run on the reader's goroutine: each is sent on reads, and the first one's
		// change, a stepdown, sends the hook's mark after it on changed, or its failure.
		reads := make(chan msgGet, barrierAttempts)
		changed := make(chan int, 1)
		failed := make(chan error, 1)
		first := true
		read := func(context.Context) error {
			if !first {
				reads <- f.get(run.key)
				return nil
			}
			first = false
			if err := f.stepDown(stream); err != nil {
				failed <- err
				return err
			}
			reads <- f.get(run.key)
			changed <- f.hooks.Mark()
			return nil
		}
		reader := bucketReader{noLeaderCheck: run.control}
		done := answer(func() error { return reader.run(rowContext(t), f.operator, bucketBarrierBucket, run.key, read) })
		var mark int
		select {
		case mark = <-changed:
		case err := <-failed:
			t.Fatalf("%s: %v", run.name, err)
		case <-time.After(3 * reachWithin):
			t.Fatalf("%s: the leader did not change inside the first read", run.name)
		}
		answered(t, done, run.name+": the read")
		t.Logf("%s: the read after the leader change answered %s", run.name, <-reads)
		again := f.hooks.Events(testnats.ClientPublish, barrierSubject, testnats.After(mark))
		if run.control {
			require.Empty(t, reads, "control: the read was made again")
			require.Empty(t, again, "control: a barrier was written again")
			continue
		}
		require.NotEmpty(t, again, "no barrier was written again after the leader change")
		require.Len(t, reads, 1, "the read was made again once")
		read2 := <-reads
		require.Equal(t, "written", read2.value, "the read made again answered %s", read2)
	}
}

// stepDown steps stream's leader down, again until another member leads it, within reachWithin.
// It runs on whatever goroutine asks, so it returns its failure.
func (f *forcedHub) stepDown(stream string) error {
	ctx, cancel := context.WithTimeout(context.Background(), reachWithin)
	defer cancel()
	before, err := streamLeader(ctx, f.operator, stream)
	if err != nil {
		return err
	}
	for ctx.Err() == nil {
		if _, err := f.operator.Request(fmt.Sprintf(natsserver.JSApiStreamLeaderStepDownT, stream), nil, noAnswerWithin); err != nil {
			time.Sleep(restartPoll)
			continue
		}
		after, err := streamLeader(ctx, f.operator, stream)
		if err == nil && after != before {
			return nil
		}
	}
	return fmt.Errorf("stream %s's leader %s still leads after stepdowns within %s", stream, before, reachWithin)
}
