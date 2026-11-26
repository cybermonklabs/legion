package bus

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/logtest"
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
	t.Parallel()
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
	if _, err := ensureKeyValue(t.Context(), slog.Default(), 20*time.Second, conn, slow, config); err != nil {
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
	t.Parallel()
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
// leader that serves once the script is spent: "lost" holds the reply for RequestTimeout and then
// answers nats.ErrTimeout, as a read the server lost does, "leaderless" answers a three-copy stream
// with no leader yet, "unserved" one whose new leader is named and takes no write yet (no
// LeaderSince, leaderNotServing), and "missing" answers nats.ErrStreamNotFound, as a member that
// has not applied a create does. No server is behind it. lostAt is when the last lost read began;
// the counts are atomic because a read the wait abandons at its limit runs on beside the next one.
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
	info := servingStreamInfo(stream)
	switch step {
	case "lost":
		s.lostAt.Store(time.Now().UnixNano())
		time.Sleep(RequestTimeout)
		return nil, nats.ErrTimeout
	case "leaderless":
		info.Cluster.Leader, info.Cluster.LeaderSince = "", nil
	case "unserved":
		info.Cluster.LeaderSince = nil
	case "missing":
		return nil, nats.ErrStreamNotFound
	}
	return info, nil
}

// servingStreamInfo is a read of a three-copy stream led by n1, which has taken writes since a
// second ago: the read a leader that has applied its first entry as leader answers.
func servingStreamInfo(stream string) *nats.StreamInfo {
	since := time.Now().Add(-time.Second)
	return &nats.StreamInfo{Config: nats.StreamConfig{Name: stream, Replicas: 3}, Cluster: &nats.ClusterInfo{Leader: "n1", LeaderSince: &since}}
}

// A wait whose first reads are lost and then answered sets its read limit back after the answer, so
// a read lost later in the same wait costs the first limit again and not the doubled one: here two
// lost reads, a read that finds no leader yet, a lost read, and the read that finds the leader. Had
// the limit stayed doubled, the third lost read would have cost four times firstReadLimit, 2.52 s,
// and after four such reads the caller's whole ten second wait.
func TestAnAnsweredReadSetsTheReadLimitBack(t *testing.T) {
	reads := &scriptedReads{script: []string{"lost", "lost", "leaderless", "lost"}}
	if err := awaitStreamLeader(t.Context(), slog.Default(), reads, afterOpen, "KV_envoy_limit_back_test"); err != nil {
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

// A read of the stream answered "stream not found" is a member that has not applied a create yet
// only after a create, so the wait reads again then and the next read finds the leader. After a
// raise, or after a read that found the stream, the stream is gone, and the wait ends on that
// answer at once (streamStep.awaitsApply).
func TestAStreamNotFoundIsReadAgainOnlyAfterACreate(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		step      streamStep
		readAgain bool
	}{{afterCreate, true}, {afterRaise, false}, {afterOpen, false}} {
		t.Run(row.step.String(), func(t *testing.T) {
			reads := &scriptedReads{script: []string{"missing"}}
			err := awaitStreamLeader(t.Context(), slog.Default(), reads, row.step, "KV_envoy_missing_test")
			switch {
			case row.readAgain && (err != nil || reads.reads.Load() != 2):
				t.Fatalf("after %s: the wait = %v after %d reads, want nil after 2: the member had not applied the create", row.step, err, reads.reads.Load())
			case !row.readAgain && (!errors.Is(err, nats.ErrStreamNotFound) || reads.reads.Load() != 1):
				t.Fatalf("after %s: the wait = %v after %d reads, want nats.ErrStreamNotFound after 1: the stream is gone", row.step, err, reads.reads.Load())
			}
		})
	}
}

// slowReads answers every read of a stream after delay: as a three-copy stream with no leader for a
// read that began before leaderFrom, and with a leader for one that began after it. No server is
// behind it.
type slowReads struct {
	nats.JetStreamContext
	delay      time.Duration
	leaderFrom time.Time
}

func (s *slowReads) StreamInfo(stream string, _ ...nats.JSOpt) (*nats.StreamInfo, error) {
	began := time.Now()
	time.Sleep(s.delay)
	info := servingStreamInfo(stream)
	if began.Before(s.leaderFrom) {
		info.Cluster.Leader, info.Cluster.LeaderSince = "", nil
	}
	return info, nil
}

// A server that answers every read late is waited for at a limit above its delay once it has
// answered one read, so no later read is cut and read again: the limit after an answered read is
// three times that read's time. Here every read takes 3 s, the stream has a leader for the reads
// that begin 6 s in, and the wait has 13 s: the first answered read, at 7.5 s, finds no leader, and
// the next, at 10.5 s, finds it. A limit set back to 630 ms after that first answer would cut the
// next three reads (630 ms, 1.26 s, 2.52 s) and answer at 15 s, after the wait ended.
func TestAServerThatAnswersEveryReadLateKeepsALimitAboveItsDelay(t *testing.T) {
	start := time.Now()
	reads := &slowReads{delay: 3 * time.Second, leaderFrom: start.Add(6 * time.Second)}
	// The bound under test, not a wait: a limit kept above the delay finds the leader at about
	// 10.5 s, and one set back answers at about 15 s, so 13 s tells the two apart (above).
	ctx, cancel := context.WithTimeout(t.Context(), 13*time.Second)
	defer cancel()
	err := awaitStreamLeader(ctx, slog.Default(), reads, afterOpen, "KV_envoy_slow_reads_test")
	took := time.Since(start)
	if err != nil {
		t.Fatalf("a wait on a server that answers every read 3 s late = %v after %s, want the leader: the limit after an answered read stays above the server's delay", err, took.Round(time.Millisecond))
	}
	if took < 7*time.Second {
		t.Fatalf("the wait answered after %s, before its first answered read could have found no leader: this row holds nothing", took.Round(time.Millisecond))
	}
}

// The steps of one start share one run of reads (withReadLimit), so a delay the first step's reads
// measured holds for the next step's first read. Here a server answers every read 3 s late: the
// first step's readAgain keeps its first send alive and takes its 3 s answer, which sets the run's
// read limit to three times that delay (readLimit.answered). The second step reads at that limit and
// finds the leader in its first read. The test asserts the run the second step would use carries
// the delay the first measured: a step given its own run would hold firstReadLimit instead.
func TestTheStepsOfOneStartShareOneRunOfReads(t *testing.T) {
	reads := &slowReads{delay: 3 * time.Second}
	ctx, cancel := context.WithTimeout(withReadLimit(t.Context()), 13*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := readAgain(ctx, slog.Default(), "KV_envoy_shared_run_test STREAM.INFO", func() (*nats.StreamInfo, error) {
		return reads.StreamInfo("KV_envoy_shared_run_test")
	}); err != nil {
		t.Fatalf("the first step's read = %v after %s, want it answered", err, time.Since(start).Round(time.Millisecond))
	}
	// The run the next step reads carries the 3 s delay the first step measured, not firstReadLimit:
	// three times 3 s, so the next step's first read is not cut and found the leader in one read.
	if limit := readLimitOf(ctx).limit; limit < 3*reads.delay {
		t.Fatalf("the run of reads the next step reads carries a limit of %s, want at least %s: the steps do not share the run", limit, 3*reads.delay)
	}
	if err := awaitStreamLeader(ctx, slog.Default(), reads, afterOpen, "KV_envoy_shared_run_test"); err != nil {
		t.Fatalf("the leader wait after it = %v after %s, want the leader in one read: the steps share the run of reads", err, time.Since(start).Round(time.Millisecond))
	}
}

// TestASlowButAnsweringServingReadIsSentOnce proves resentOnTimeout, which the reads that serve
// requests use (the KV leader and edge gets, the stored-message reads, the session stream-state
// read), sends a read that answers within its bound once and logs no WARN. A start's readAgain would
// start a second send beside it at its 630 ms read limit; these reads start every call fresh, so
// that would send duplicates on every read a loaded server answers slower than that.
func TestASlowButAnsweringServingReadIsSentOnce(t *testing.T) {
	t.Parallel()
	var logs logtest.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	var sends atomic.Int64
	value, err := resentOnTimeout(context.Background(), logger, "KV_envoy_slow_read STREAM.MSG.GET", RequestTimeout,
		func(ctx context.Context) (string, error) {
			sends.Add(1)
			select {
			case <-time.After(time.Second):
				return "value", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		})
	if err != nil || value != "value" {
		t.Fatalf("a read answering at 1 s = %q, %v, want the value", value, err)
	}
	if n := sends.Load(); n != 1 {
		t.Fatalf("a read answering at 1 s sent %d requests, want 1: a merely-slow read is sent once", n)
	}
	if again := logs.Lines("sending it again"); again != "" {
		t.Fatalf("a read answering at 1 s logged a resend WARN, want none: %s", again)
	}
}
