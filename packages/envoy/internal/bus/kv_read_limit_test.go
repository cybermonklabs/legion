package bus

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/testnats"
)

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
