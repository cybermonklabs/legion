package bus

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/testnats"
)

// countedReads is ctx with the first read limit at the caller's whole wait (firstReadLimitKey),
// for a row that counts a start's requests: a read a loaded machine answers past firstReadLimit is
// then waited for rather than abandoned and read again, so the count is the requests' own.
func countedReads(ctx context.Context) context.Context {
	return context.WithValue(ctx, firstReadLimitKey{}, RequestTimeout)
}

// heldReplyJetStream answers its first read of a stream as a read of a stream with no leader yet,
// and holds the reply of its second read for RequestTimeout before answering nats.ErrTimeout, as
// nats.go answers a read the server never answered within the caller's wait; every later read is
// the cluster's own. Its counts are atomic, because a read the start abandons at its read limit
// runs on beside the start's next read.
type heldReplyJetStream struct {
	nats.JetStreamContext
	reads  atomic.Int32
	heldAt atomic.Int64
}

func (js *heldReplyJetStream) StreamInfo(stream string, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	switch js.reads.Add(1) {
	case 1:
		info, err := js.JetStreamContext.StreamInfo(stream, opts...)
		if err != nil || info.Cluster == nil {
			return info, err
		}
		leaderless := *info
		cluster := *info.Cluster
		cluster.Leader = ""
		leaderless.Cluster = &cluster
		return &leaderless, nil
	case 2:
		js.heldAt.Store(time.Now().UnixNano())
		time.Sleep(RequestTimeout)
		return nil, nats.ErrTimeout
	}
	return js.JetStreamContext.StreamInfo(stream, opts...)
}

// heldReadBound is how long after a lost read began a start that waits for a stream's leader may
// answer, when that leader is already there: the read's limit (firstReadLimit), after which the
// read is taken as lost, and one second more for the read after it on a loaded machine. A start
// that waited out the caller's whole wait for the lost read answered RequestTimeout, ten seconds,
// after it.
const heldReadBound = firstReadLimit + time.Second

// A start that finds a bucket whose stream reads as having no leader yet waits for the leader,
// and a read of that wait the server never answers costs it its read limit and not the caller's
// whole ten second wait: the leader is there all along, so the start answers within the limit of
// the lost read, and one more read, after it.
func TestAStartWhoseReadOfTheLeaderIsLostAnswersWithinTheReadLimit(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	conn, js := memberJetStream(t, cluster, 0)
	config := &nats.KeyValueConfig{Bucket: "envoy_held_read_test", Storage: nats.FileStorage, Replicas: 3}
	if _, err := EnsureKeyValue(t.Context(), conn, js, config); err != nil {
		t.Fatalf("create the three-copy bucket: %v", err)
	}
	held := &heldReplyJetStream{JetStreamContext: js}
	if _, err := EnsureKeyValue(t.Context(), conn, held, config); err != nil {
		t.Fatalf("a start whose read of the leader was lost: %v", err)
	}
	answeredAt := time.Now()
	if reads := held.reads.Load(); reads < 3 {
		t.Fatalf("the start read the stream %d times, want the leaderless read, the lost one and one after it: this row holds nothing", reads)
	}
	if after := answeredAt.Sub(time.Unix(0, held.heldAt.Load())); after > heldReadBound {
		t.Fatalf("the start answered %s after its lost read began, want within %s: the lost read is abandoned at its limit, %s, and read again",
			after.Round(time.Millisecond), heldReadBound, firstReadLimit)
	}
}

// slowReadsJetStream answers every lookup of a bucket readDelay late: a server that answers, only
// slower than any read limit before the last few.
type slowReadsJetStream struct {
	nats.JetStreamContext
	lookups atomic.Int32
}

const readDelay = 1500 * time.Millisecond

func (js *slowReadsJetStream) KeyValue(bucket string) (nats.KeyValue, error) {
	js.lookups.Add(1)
	time.Sleep(readDelay)
	return js.JetStreamContext.KeyValue(bucket)
}

// The read limit decides no result: a server that answers every read later than the first limit
// is still waited for, since each read left unanswered doubles the next one's limit, and the start
// opens its bucket rather than end at its hang guard over reads the server was answering.
func TestAServerThatAnswersEveryReadLateIsStillWaitedFor(t *testing.T) {
	conn := testnats.Connect(t, testnats.URL(t))
	t.Cleanup(conn.Close)
	js, err := conn.JetStream(nats.MaxWait(RequestTimeout))
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	config := &nats.KeyValueConfig{Bucket: "envoy_slow_reads_test", Storage: nats.MemoryStorage}
	if _, err := EnsureKeyValue(t.Context(), conn, js, config); err != nil {
		t.Fatalf("create the bucket: %v", err)
	}
	t.Cleanup(func() { _ = js.DeleteKeyValue(config.Bucket) })
	slow := &slowReadsJetStream{JetStreamContext: js}
	if readDelay <= firstReadLimit || readDelay >= RequestTimeout {
		t.Fatalf("readDelay %s must lie between the first read limit %s and RequestTimeout %s for this row to hold anything", readDelay, firstReadLimit, RequestTimeout)
	}
	if _, err := ensureKeyValue(t.Context(), 20*time.Second, conn, slow, config); err != nil {
		t.Fatalf("a start whose every lookup is answered %s late = %v, want it to open the bucket", readDelay, err)
	}
	if lookups := slow.lookups.Load(); lookups < 2 {
		t.Fatalf("lookups sent = %d, want the ones the first limits cut and the one that was waited for", lookups)
	}
}

// lostUpdateJetStream applies its first stream update and answers it nats.ErrTimeout, as nats.go
// answers an update the server applied while it elected a leader and never answered, and holds
// the reply of the first read after that update for RequestTimeout before answering
// nats.ErrTimeout too; every other request is the cluster's own.
type lostUpdateJetStream struct {
	nats.JetStreamContext
	updated   atomic.Bool
	readsLost atomic.Int32
}

func (js *lostUpdateJetStream) UpdateStream(cfg *nats.StreamConfig, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	if js.updated.CompareAndSwap(false, true) {
		if _, err := js.JetStreamContext.UpdateStream(cfg, opts...); err != nil {
			return nil, err
		}
		return nil, nats.ErrTimeout
	}
	return js.JetStreamContext.UpdateStream(cfg, opts...)
}

func (js *lostUpdateJetStream) StreamInfo(stream string, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	if js.updated.Load() && js.readsLost.Add(1) == 1 {
		time.Sleep(RequestTimeout)
		return nil, nats.ErrTimeout
	}
	return js.JetStreamContext.StreamInfo(stream, opts...)
}

// A raise the server applied and never answered, whose first read afterwards is lost too, still
// succeeds: the read that tells a raise that landed from one that did not is a read of the
// stream's state, sent again past its read limit (updateStream), where one lost read reported the
// raise that landed as one that failed, "raise KV_<bucket>: ENVOY_NATS_REPLICAS=3: nats: timeout".
func TestARaiseWhoseAnswerAndNextReadAreLostStillLands(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	conn, js := memberJetStream(t, cluster, 0)
	const bucket = "envoy_lost_update_test"
	if _, err := EnsureKeyValue(t.Context(), conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: 1}); err != nil {
		t.Fatalf("create the one-copy bucket: %v", err)
	}
	lost := &lostUpdateJetStream{JetStreamContext: js}
	if _, err := EnsureKeyValue(t.Context(), conn, lost, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: 3}); err != nil {
		t.Fatalf("a raise whose answer and next read were lost = %v, want it to land", err)
	}
	if !lost.updated.Load() || lost.readsLost.Load() < 2 {
		t.Fatalf("the raise was sent %v and read %d times after it, want it sent and read again after the lost read: this row holds nothing",
			lost.updated.Load(), lost.readsLost.Load())
	}
}

// scriptedReads answers each read of a stream as script says, in order, and as a stream with a
// leader once the script is spent: "lost" holds the reply for RequestTimeout and then answers
// nats.ErrTimeout, as a read the server lost does, "leaderless" answers a three-copy stream with no
// leader yet. No server is behind it. lostAt is when the last lost read began; the counts are atomic
// because a read the wait abandons at its limit runs on beside the next one.
type scriptedReads struct {
	nats.JetStreamContext
	script []string
	reads  atomic.Int32
	lostAt atomic.Int64
}

func (s *scriptedReads) StreamInfo(stream string, _ ...nats.JSOpt) (*nats.StreamInfo, error) {
	step := "leader"
	if n := int(s.reads.Add(1)) - 1; n < len(s.script) {
		step = s.script[n]
	}
	info := &nats.StreamInfo{Config: nats.StreamConfig{Name: stream, Replicas: 3}, Cluster: &nats.ClusterInfo{Leader: "n1"}}
	switch step {
	case "lost":
		s.lostAt.Store(time.Now().UnixNano())
		time.Sleep(RequestTimeout)
		return nil, nats.ErrTimeout
	case "leaderless":
		info.Cluster.Leader = ""
	}
	return info, nil
}

// A wait whose first reads are lost and then answered sets its read limit back after the answer, so
// a read lost later in the same wait costs the first limit again and not the doubled one: here two
// lost reads, a read that finds no leader yet, a lost read, and the read that finds the leader. Had
// the limit stayed doubled, the third lost read would have cost four times firstReadLimit, 2.52 s,
// and after four such reads the caller's whole ten second wait.
func TestAnAnsweredReadSetsTheReadLimitBack(t *testing.T) {
	reads := &scriptedReads{script: []string{"lost", "lost", "leaderless", "lost"}}
	if err := awaitStreamLeader(t.Context(), reads, "open", "KV_envoy_limit_back_test"); err != nil {
		t.Fatalf("a wait whose leader came after lost reads = %v, want nil", err)
	}
	answeredAt := time.Now()
	if got := reads.reads.Load(); got != 5 {
		t.Fatalf("the wait read the stream %d times, want 5: the script holds nothing otherwise", got)
	}
	if after := answeredAt.Sub(time.Unix(0, reads.lostAt.Load())); after > heldReadBound {
		t.Fatalf("the wait answered %s after its last lost read began, want within %s: an answered read sets the limit back to %s",
			after.Round(time.Millisecond), heldReadBound, firstReadLimit)
	}
}
