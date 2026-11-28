package bus

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/testwait"
)

// keeperPasses waits until the keeper of each of keepers has ended n passes begun after the call
// (Client.keepPasses): the counted event a test reads an absence after, since a pass that finds
// nothing to restore sends nothing and logs nothing. A pass in flight at the call may have read the
// stream before it, so the wait is for one pass more than n.
func keeperPasses(t *testing.T, keepers []*Client, n int) {
	t.Helper()
	from := make([]uint64, len(keepers))
	for i, keeper := range keepers {
		from[i] = keeper.keepPasses.Load()
	}
	if waiting := keeperPassesWaiting.Load(); waiting != nil {
		(*waiting)()
	}
	testwait.Until(t, fmt.Sprintf("each of %d keepers to end %d passes begun after %v had ended", len(keepers), n, from), func() ([]uint64, bool) {
		ended := make([]uint64, len(keepers))
		all := true
		for i, keeper := range keepers {
			ended[i] = keeper.keepPasses.Load()
			all = all && ended[i] > from[i]+uint64(n)
		}
		return ended, all
	})
}

// keeperPassesWaiting, when set, runs as keeperPasses has read the passes ended so far and is about
// to wait for more: TestHoldKeeperUpdatesWaitsOutAKeeperPassBegunBeforeItsSwap releases a pass it
// holds there, so that pass ends only while keeperPasses waits for it.
var keeperPassesWaiting atomic.Pointer[func()]

// keeperSwapping, when set, runs in holdKeeperUpdates just before it swaps a keeper's JetStream
// context: TestHoldKeeperUpdatesWaitsOutAKeeperPassBegunBeforeItsSwap holds a keeper pass begun
// there, with the context from before the swap.
var keeperSwapping atomic.Pointer[func()]

// heldBucketReads holds every read of stream, once armed, until resume closes, and closes entered
// at the first it holds: a keeper pass that reads it then waits there, sending its read again each
// time its read limit passes (readAgain), until the test resumes it.
type heldBucketReads struct {
	nats.JetStreamContext
	stream  string
	armed   atomic.Bool
	entered chan struct{}
	first   sync.Once
	resume  chan struct{}
}

func (js *heldBucketReads) StreamInfo(stream string, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	if stream == js.stream && js.armed.Load() {
		js.first.Do(func() { close(js.entered) })
		<-js.resume
	}
	return js.JetStreamContext.StreamInfo(stream, opts...)
}

// holdKeeperUpdates returns only once every keeper pass begun before its swap has ended: a pass
// takes its client's JetStream context once, as it starts, so one that began before the swap still
// writes through the context with no hold, and it could put back the update the test makes next
// before the test's read of it came back. The row starts holding the keeper's reads of a bucket at
// the swap (keeperSwapping), waits there until a keeper pass is held in one, and releases that pass
// only from inside a wait for passes that comes after the swap (keeperPassesWaiting). So a
// holdKeeperUpdates with no wait (the independent tester's plant PL7), or with its wait before the
// swap (PL15), returns while that pass is still held, and the row fails by name; no span decides
// it. (The tester's forcing FP7 found the gap: each keeper read held 5 s, and the lowering's
// replies frozen 40 s.)
func TestHoldKeeperUpdatesWaitsOutAKeeperPassBegunBeforeItsSwap(t *testing.T) {
	const bucket = "envoy_keeper_held_pass"
	cluster, _ := startClusterJetStream(t)
	client := startOnCluster(t, cluster, WithReplicas(3), WithKeptBuckets(bucket), WithKeepInterval(testKeepInterval), heldKeeperStepGuard(t))
	if _, err := EnsureKeyValue(t.Context(), client.Conn, client.JS(), &nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: 3}); err != nil {
		t.Fatalf("open %s at three copies: %v", bucket, err)
	}
	held := &heldBucketReads{stream: "KV_" + bucket, entered: make(chan struct{}), resume: make(chan struct{})}
	release := sync.OnceFunc(func() { close(held.resume) })
	t.Cleanup(release)
	client.mu.Lock()
	held.JetStreamContext = client.js
	client.js = held
	client.mu.Unlock()

	var endedBefore atomic.Uint64
	swapping := func() {
		held.armed.Store(true)
		testwait.Receive(t, "a keeper pass to begin its read of KV_"+bucket+" before the swap", held.entered)
		endedBefore.Store(client.keepPasses.Load())
	}
	keeperSwapping.Store(&swapping)
	t.Cleanup(func() { keeperSwapping.Store(nil) })
	waiting := func() {
		client.mu.Lock()
		swapped := client.js != nats.JetStreamContext(held)
		client.mu.Unlock()
		if held.armed.Load() && swapped {
			release()
		}
	}
	keeperPassesWaiting.Store(&waiting)
	t.Cleanup(func() { keeperPassesWaiting.Store(nil) })
	open := holdKeeperUpdates(t, client)
	defer open()
	if ended := client.keepPasses.Load(); ended <= endedBefore.Load() {
		t.Fatalf("holdKeeperUpdates returned while the keeper pass begun before its swap was still running: %d passes ended, as when that pass began", ended)
	}
}

// heldFor waits for passes passes of each of keepers, one at a time (keeperPasses), and fails the
// test when, after any of them, still stops reporting that the stream is as it was left.
func heldFor(t *testing.T, keepers []*Client, js nats.JetStreamContext, stream, what string, passes int, still func(*nats.StreamInfo) bool) {
	t.Helper()
	for range passes {
		keeperPasses(t, keepers, 1)
		if info := readStream(t, js, stream); !still(info) {
			t.Fatalf("%s: %s changed: %+v", stream, what, info.Config)
		}
	}
}

// interleavedJetStream runs another program's write in the middle of one program's read and write
// of the stream: firstRead, when set, answers its first StreamInfo in place of the server, a read
// the test made before that write, and beforeUpdate runs just before its first UpdateStream is
// sent, once and only when set. It counts the stream updates sent through it.
type interleavedJetStream struct {
	nats.JetStreamContext
	firstRead            *nats.StreamInfo
	beforeUpdate         func()
	readOnce, updateOnce sync.Once
	updates              atomic.Int32
	// updateErr is the server's answer to the last update sent through the wrapper: nil when the
	// update was applied, an unanswered wait when nats-server never answered it.
	updateErr atomic.Pointer[error]
}

func (js *interleavedJetStream) StreamInfo(stream string, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	var stale *nats.StreamInfo
	js.readOnce.Do(func() { stale = js.firstRead })
	if stale != nil {
		return stale, nil
	}
	return js.JetStreamContext.StreamInfo(stream, opts...)
}

func (js *interleavedJetStream) UpdateStream(cfg *nats.StreamConfig, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	if js.beforeUpdate != nil {
		js.updateOnce.Do(js.beforeUpdate)
	}
	js.updates.Add(1)
	info, err := js.JetStreamContext.UpdateStream(cfg, opts...)
	js.updateErr.Store(&err)
	return info, err
}

// holdKeeperUpdates holds the first stream update the keeper of each of keepers sends until the
// func it returns runs, which a test runs once its own update of what those keepers restore has
// been read back (testnats.UpdateStream): a keeper that put the update back before a read of it
// came back would leave no read that ever shows it, and the read-back would fail the test at its
// deadline. The keeper waits inside a step of its pass for as long as the read-back takes, which a
// loaded machine can make longer than a start's step guard (startHangGuard, 36 s): each keeper
// passed here is started with heldKeeperStepGuard, so its step's guard ends where the test's own
// waits give up, and not at that. A pass takes its client's JetStream context once, as it starts
// (keepPass), so this waits for one whole pass after the swap: every pass that reads the update
// holds the gated context. The hold ends on every way out of the test, before a client's close
// waits for its keeper.
func holdKeeperUpdates(t *testing.T, keepers ...*Client) func() {
	t.Helper()
	gate := make(chan struct{})
	open := sync.OnceFunc(func() { close(gate) })
	t.Cleanup(open)
	for _, keeper := range keepers {
		if swapping := keeperSwapping.Load(); swapping != nil {
			(*swapping)()
		}
		keeper.mu.Lock()
		keeper.js = &interleavedJetStream{JetStreamContext: keeper.js, beforeUpdate: func() { <-gate }}
		keeper.mu.Unlock()
	}
	keeperPasses(t, keepers, 1)
	return open
}

// heldKeeperStepGuard is the step guard of a keeper whose update holdKeeperUpdates holds
// (withKeeperStepGuard): what is left, as the keeper starts, until the test's own waits give up
// (testwait.GiveUpAt), so no guard ends a step before the test's read-back has given up and named
// what it waited for; a step that outlasts the test is ended by the client's close at the test's
// end. The guard is a duration, and a duration cannot say "none", so under -timeout 0, whose waits
// have no end, it is a day in place of none. Past the give-up it is no span, which the keeper
// takes as startHangGuard's, and the test's next wait fails at once.
func heldKeeperStepGuard(t *testing.T) ConnectOption {
	if end, bounded := testwait.GiveUpAt(t); bounded {
		return withKeeperStepGuard(time.Until(end))
	}
	return withKeeperStepGuard(24 * time.Hour)
}
