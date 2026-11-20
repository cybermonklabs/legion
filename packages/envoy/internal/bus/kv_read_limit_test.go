package bus

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// countedReads is ctx with the first read limit at the caller's whole wait (firstReadLimitKey),
// for a row that counts a start's requests: a read a loaded machine answers past firstReadLimit is
// then waited for rather than abandoned and read again, so the count is the requests' own.
func countedReads(ctx context.Context) context.Context {
	return context.WithValue(ctx, firstReadLimitKey{}, RequestTimeout)
}

// sentReads records each read of a stream's state sent under the context under returns
// (readSendKey): when it went out and how long the context it was sent under had left then, which
// is the read limit the code put on it. A row reads that bound rather than the time the whole start
// took, which a loaded machine stretches.
type sentReads struct {
	mu    sync.Mutex
	reads []sentRead
}

// sentRead is one read a run sent: at is when, left what its context had left then, and -1 when
// that context had no deadline at all.
type sentRead struct {
	at   time.Time
	left time.Duration
}

// under is ctx with s recording each read sent under it.
func (s *sentReads) under(ctx context.Context) context.Context {
	return context.WithValue(ctx, readSendKey{}, func(read context.Context) {
		at, left := time.Now(), time.Duration(-1)
		if deadline, ok := read.Deadline(); ok {
			left = deadline.Sub(at)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.reads = append(s.reads, sentRead{at: at, left: left})
	})
}

// latest is the index of the read s recorded last. Asked from inside a read, it is that read's
// own: limitedRead records a read before it sends it, and a run sends its reads one at a time.
func (s *sentReads) latest() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reads) - 1
}

// read is the read s recorded at index i.
func (s *sentReads) read(i int) sentRead {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads[i]
}

// heldReplyJetStream answers its first read of a stream as a read of a stream with no leader yet,
// and holds the reply of its second read until release is closed, at the row's cleanup, before
// answering nats.ErrTimeout: the server never answers that read while the row runs, so a start
// that waited for its answer, rather than abandon it at its read limit, would not answer at all.
// Every later read is the cluster's own. It notes which of sent's reads the first two are
// (sentReads.latest), and its counts are atomic, because a read the start abandons at its read
// limit runs on beside the start's next read.
type heldReplyJetStream struct {
	nats.JetStreamContext
	sent     *sentReads
	release  <-chan struct{}
	reads    atomic.Int32
	noLeader atomic.Int32
	lost     atomic.Int32
}

func (js *heldReplyJetStream) StreamInfo(stream string, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	switch js.reads.Add(1) {
	case 1:
		js.noLeader.Store(int32(js.sent.latest()))
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
		js.lost.Store(int32(js.sent.latest()))
		<-js.release
		return nil, nats.ErrTimeout
	}
	return js.JetStreamContext.StreamInfo(stream, opts...)
}

// A start that finds a bucket whose stream reads as having no leader yet waits for the leader,
// and a read of that wait the server never answers costs it its read limit and not the caller's
// whole ten second wait: the leader is there all along, so the start answers once the lost read
// is abandoned at its limit and read again. The row reads that limit where the code put it, on
// the lost read's context (sentReads): the limit after the read that found no leader, three times
// that read's time and never under firstReadLimit (readLimit.answered). The read's time is at most
// the span from that read's send to the lost read's send, so the bound holds however slow the
// machine; a read held to the caller's whole wait is sent under RequestTimeout.
func TestAStartWhoseReadOfTheLeaderIsLostAnswersWithinTheReadLimit(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	conn, js := memberJetStream(t, cluster, 0)
	config := &nats.KeyValueConfig{Bucket: "envoy_held_read_test", Storage: nats.FileStorage, Replicas: 3}
	if _, err := EnsureKeyValue(t.Context(), conn, js, config); err != nil {
		t.Fatalf("create the three-copy bucket: %v", err)
	}
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	sent := &sentReads{}
	held := &heldReplyJetStream{JetStreamContext: js, sent: sent, release: release}
	ctx := sent.under(t.Context())
	answered := make(chan error, 1)
	go func() {
		_, err := EnsureKeyValue(ctx, conn, held, config)
		answered <- err
	}()
	select {
	case err := <-answered:
		if err != nil {
			t.Fatalf("a start whose read of the leader was lost: %v", err)
		}
	case <-time.After(testwait.Bounded(t, time.Minute, time.Second)):
		t.Fatal("the start did not answer while its lost read was held: it waits for that read's answer rather than abandon the read at its limit")
	}
	if reads := held.reads.Load(); reads < 3 {
		t.Fatalf("the start read the stream %d times, want the leaderless read, the lost one and one after it: this row holds nothing", reads)
	}
	noLeader, lost := int(held.noLeader.Load()), int(held.lost.Load())
	if lost != noLeader+1 {
		t.Fatalf("the lost read was the start's read %d and the read that found no leader its read %d, want the lost read right after that one: this row holds nothing", lost, noLeader)
	}
	before, read := sent.read(noLeader), sent.read(lost)
	limit := max(firstReadLimit, 3*read.at.Sub(before.at))
	if read.left < 0 || read.left > limit {
		t.Fatalf("the lost read was sent with %s left to it, want at most %s, its read limit: three times the read before it, which took at most %s, and never under %s; the caller's whole wait is %s",
			read.left, limit, read.at.Sub(before.at), firstReadLimit, RequestTimeout)
	}
	t.Logf("the lost read was sent with %s left to it, within its read limit of at most %s", read.left, limit)
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
// leader yet, and "missing" answers nats.ErrStreamNotFound, as a member that has not applied a
// create does. No server is behind it. lostAt is when the last lost read began; the counts are
// atomic because a read the wait abandons at its limit runs on beside the next one.
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
	case "missing":
		return nil, nats.ErrStreamNotFound
	}
	return info, nil
}

// A wait whose first reads are lost and then answered sets its read limit back after the answer, so
// a read lost later in the same wait costs the first limit again and not the doubled one: here two
// lost reads, a read that finds no leader yet, a lost read, and the read that finds the leader. Had
// the limit stayed doubled, the third lost read would have cost four times firstReadLimit, 2.52 s,
// and after four such reads the caller's whole ten second wait. The wait runs in a synctest bubble,
// whose clock moves only while every goroutine in it waits, so the time it reads is the limits and
// pauses the code waited out and nothing a loaded machine added: the last lost read costs its
// limit and the pause before the next read (bucketAppliedInterval), exactly.
func TestAnAnsweredReadSetsTheReadLimitBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// The lost reads go on waiting out their own RequestTimeout after the wait has abandoned
		// them, and a bubble may end only once every goroutine in it has: its clock passes that
		// wait at once.
		t.Cleanup(func() {
			time.Sleep(RequestTimeout)
			synctest.Wait()
		})
		reads := &scriptedReads{script: []string{"lost", "lost", "leaderless", "lost"}}
		if err := awaitStreamLeader(t.Context(), reads, afterOpen, "KV_envoy_limit_back_test"); err != nil {
			t.Fatalf("a wait whose leader came after lost reads = %v, want nil", err)
		}
		answeredAt := time.Now()
		if got := reads.reads.Load(); got != 5 {
			t.Fatalf("the wait read the stream %d times, want 5: the script holds nothing otherwise", got)
		}
		if after, cost := answeredAt.Sub(time.Unix(0, reads.lostAt.Load())), firstReadLimit+bucketAppliedInterval; after > cost {
			t.Fatalf("the wait answered %s after its last lost read began, want %s: an answered read sets the limit back to %s, and the lost read after it costs that limit and the pause before the next read",
				after, cost, firstReadLimit)
		}
	})
}

// A read of the stream answered "stream not found" is a member that has not applied a create yet
// only after a create, so the wait reads again then and the next read finds the leader. After a
// raise, or after a read that found the stream, the stream is gone, and the wait ends on that
// answer at once (streamStep.awaitsApply).
func TestAStreamNotFoundIsReadAgainOnlyAfterACreate(t *testing.T) {
	for _, row := range []struct {
		step      streamStep
		readAgain bool
	}{{afterCreate, true}, {afterRaise, false}, {afterOpen, false}} {
		t.Run(row.step.String(), func(t *testing.T) {
			reads := &scriptedReads{script: []string{"missing"}}
			err := awaitStreamLeader(t.Context(), reads, row.step, "KV_envoy_missing_test")
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
	info := &nats.StreamInfo{Config: nats.StreamConfig{Name: stream, Replicas: 3}, Cluster: &nats.ClusterInfo{Leader: "n1"}}
	if began.Before(s.leaderFrom) {
		info.Cluster.Leader = ""
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
	ctx, cancel := context.WithTimeout(t.Context(), 13*time.Second)
	defer cancel()
	err := awaitStreamLeader(ctx, reads, afterOpen, "KV_envoy_slow_reads_test")
	took := time.Since(start)
	if err != nil {
		t.Fatalf("a wait on a server that answers every read 3 s late = %v after %s, want the leader: the limit after an answered read stays above the server's delay", err, took.Round(time.Millisecond))
	}
	if took < 7*time.Second {
		t.Fatalf("the wait answered after %s, before its first answered read could have found no leader: this row holds nothing", took.Round(time.Millisecond))
	}
}

// The steps of one start share one run of reads (withReadLimit), so a delay the first step's reads
// measured holds for the next step's first read: here a server answers every read 3 s late, a read
// sent again until it is answered (readAgain) learns the delay over three cut reads, and the leader
// wait after it (awaitStreamLeader) waits for its first read at three times that delay and finds the
// leader in one read, inside a 13 s wait. A step that learnt the delay again would cut three more
// reads and answer at about 15 s, after the wait ended.
func TestTheStepsOfOneStartShareOneRunOfReads(t *testing.T) {
	reads := &slowReads{delay: 3 * time.Second}
	ctx, cancel := context.WithTimeout(withReadLimit(t.Context()), 13*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := readAgain(ctx, "KV_envoy_shared_run_test STREAM.INFO", readLimitOf(ctx), func() (*nats.StreamInfo, error) {
		return reads.StreamInfo("KV_envoy_shared_run_test")
	}); err != nil {
		t.Fatalf("the first step's read = %v after %s, want it answered", err, time.Since(start).Round(time.Millisecond))
	}
	if err := awaitStreamLeader(ctx, reads, afterOpen, "KV_envoy_shared_run_test"); err != nil {
		t.Fatalf("the leader wait after it = %v after %s, want the leader in one read: the steps share the run of reads", err, time.Since(start).Round(time.Millisecond))
	}
	if readLimitOf(ctx) != readLimitOf(ctx) {
		t.Fatal("two reads of one context's run of reads found two runs")
	}
}
