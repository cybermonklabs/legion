// A start that creates or raises a stream of more than one copy, or finds one another program
// created or raised a moment ago, answers once the stream has a leader, on in-process
// three-server clusters (testnats.StartCluster). A row that needs the stream's first election
// slow forces the order a slow election makes; the issue's verification runs the rest against a
// server that holds that election.
//
// What a start promises is a leader named, and each row reads that once (hasLeader). A member is
// named leader before it serves writes (testnats.Cluster.AwaitStream), so a write sent the moment
// a start answers can draw "no responders": a row that writes after the start waits until every
// running member serves a write to the stream (AwaitStream, AwaitBucket) and then writes. Such a
// row checks that the start leaves a leader named, not that a write sent at once is served.

package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"sync"
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
//
// The row reads the leader the start leaves once (hasLeader), waits until every running member
// serves a write to the bucket (testnats.Cluster.AwaitBucket), then writes through the handle the
// start returned (KeyValue.Put), on the row's two-second JetStream wait, the caller's own. Its
// write once went out the moment the start answered: in a whole-package run beside seven other
// cluster test runs, member 0 routed none of nats.go's three sends to the bucket's stream, and the
// row failed "put into the bucket the start created: nats: no response from stream".
func TestEnsureKeyValueCreatesAThreeCopyBucketWhoseFirstElectionIsSlow(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	conn := testnats.Connect(t, cluster.Servers[0].URL())
	t.Cleanup(conn.Close)
	js, err := conn.JetStream(nats.MaxWait(createdBucketSendWait))
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	const bucket = "envoy_create_test"
	wait := rowWait(t)
	kv, err := EnsureKeyValue(wait, conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: 3})
	if err != nil {
		t.Fatalf("ensure a three-copy bucket: %v", err)
	}
	if !hasLeader(t, wait, js, "KV_"+bucket) {
		t.Fatal("the start answered while the bucket's stream had no leader")
	}
	cluster.AwaitBucket(t, bucket)
	if _, err := kv.Put("key", []byte("value")); err != nil {
		t.Fatalf("put into the bucket the start created: %v", err)
	}
	deadline, _ := wait.Deadline()
	testnats.AwaitStreamCopiesWithin(t, cluster.JetStream(t), "KV_"+bucket, 3, time.Until(deadline))
}

// A start that creates the notification stream at three copies answers only once the stream has a
// leader. The server answers the create's first send at the stream's first leader election, but a
// send made again while that election runs is an equal create, which it answers at once with the
// stream still leaderless; a start that took that answer as the stream's served a publish no
// leader could store ("no response from stream"). On a cluster that elects at once this row passes
// either way; the run that holds it is this row against a server that holds the stream's first
// election (the issue's verification). The row checks that the start leaves a leader named
// (hasLeader); its publish waits until every running member serves one (AwaitStream), so it does
// not check that a publish sent the moment the start answers is served.
func TestAStartThatCreatesTheStreamAtThreeCopiesServesItsFirstPublish(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	client := startOnCluster(t, cluster, WithReplicas(3))
	if !hasLeader(t, rowWait(t), cluster.JetStream(t), Stream) {
		t.Fatal("the start that created the stream at three copies answered while it had no leader")
	}
	created := contracts.Envelope{
		EventID: "event-created", Source: "dispatch",
		Topic: "notifications.agent.ses_created", DedupeKey: "dispatch.event.created",
	}
	cluster.AwaitStream(t, Stream, created.Topic)
	if err := client.Publish(created); err != nil {
		t.Fatalf("the first publish after a start that created the stream at three copies: %v", err)
	}
}

// A start that raises the notification stream from one copy to three answers only once the raised
// stream has a leader. The server answers the raise at once, from the member that held the one
// copy, and the stream has no leader until its new group elects one; a publish sent then draws no
// answer even after the leader is there, so a start that took the raise's answer as the stream's
// left its first publish to fail at its 5 s bound, with no election held. The row checks that the
// start leaves a leader named, and its publish waits as the row above's does.
func TestAStartThatRaisesTheStreamToThreeCopiesServesItsFirstPublish(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	startOnCluster(t, cluster, WithReplicas(1)).Close()
	client := startOnCluster(t, cluster, WithReplicas(3))
	if !hasLeader(t, rowWait(t), cluster.JetStream(t), Stream) {
		t.Fatal("the start that raised the stream to three copies answered while it had no leader")
	}
	raised := contracts.Envelope{
		EventID: "event-raised", Source: "dispatch",
		Topic: "notifications.agent.ses_raised", DedupeKey: "dispatch.event.raised",
	}
	cluster.AwaitStream(t, Stream, raised.Topic)
	if err := client.Publish(raised); err != nil {
		t.Fatalf("the first publish after a start that raised the stream to three copies: %v", err)
	}
}

// hasLeader reads stream within ctx and reports whether it has a leader then: the fact a start
// that answered must leave behind, read from one answer so that the row does not itself wait for
// it (rowRead). The leader the read names may not serve writes yet (testnats.Cluster.AwaitStream).
func hasLeader(t *testing.T, ctx context.Context, js nats.JetStreamContext, stream string) bool {
	t.Helper()
	info := rowRead(t, ctx, js, stream)
	return info.Cluster != nil && info.Cluster.Leader != ""
}

// rowRead reads stream until a read is answered. Each read takes a context of its own, cut to
// RequestTimeout and to ctx, the row's deadline, and ends with it, so a read the server leaves
// unanswered (a member answers some reads with nothing at all, sentAgain) ends before the next is
// sent. An answer that is an error, or the end of ctx, fails the row.
func rowRead(t *testing.T, ctx context.Context, js nats.JetStreamContext, stream string) *nats.StreamInfo {
	t.Helper()
	for {
		ask, cancel := context.WithTimeout(ctx, RequestTimeout)
		info, err := js.StreamInfo(stream, nats.Context(ask))
		cancel()
		switch {
		case err == nil:
			return info
		case !unanswered(err) || ctx.Err() != nil:
			t.Fatalf("read %s: %v", stream, err)
		}
	}
}

// A start that finds a bucket another program raised a moment ago, whose new group has not elected
// its leader yet, waits for that leader before it answers. Its own raise is nothing (the bucket has
// the copies already), and the other program's raise was answered at once, with the stream
// leaderless, so a start that took the stream as it found it handed its caller a bucket whose first
// put drew no answer. The row checks that the start leaves a leader named, and its put waits as the
// first row's does.
func TestAStartThatFindsAJustRaisedBucketAnswersOnceItHasALeader(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	conn, js := memberJetStream(t, cluster, 0)
	const bucket = "envoy_found_raised_test"
	wait := rowWait(t)
	if _, err := EnsureKeyValue(wait, conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: 1}); err != nil {
		t.Fatalf("create the one-copy bucket: %v", err)
	}
	if _, err := raiseReplicas(wait, js, bucket, 3); err != nil {
		t.Fatalf("another program's raise: %v", err)
	}
	kv, err := EnsureKeyValue(wait, conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: 3})
	if err != nil {
		t.Fatalf("a start that finds the raised bucket: %v", err)
	}
	if !hasLeader(t, wait, js, "KV_"+bucket) {
		t.Fatal("the start answered while the raised bucket's stream had no leader")
	}
	cluster.AwaitBucket(t, bucket)
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
	wait := rowWait(t)
	if _, err := EnsureKeyValue(wait, conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: 1}); err != nil {
		t.Fatalf("create the one-copy bucket: %v", err)
	}
	if _, err := raiseReplicas(wait, js, bucket, 3); err != nil {
		t.Fatalf("another program's raise: %v", err)
	}
	kv, err := EnsureKeyValue(wait, conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: 1})
	if err != nil {
		t.Fatalf("a start at one copy that finds the raised bucket: %v", err)
	}
	if !hasLeader(t, wait, js, "KV_"+bucket) {
		t.Fatal("the start at one copy answered while the raised bucket's stream had no leader")
	}
	cluster.AwaitBucket(t, bucket)
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
	wait := rowWait(t)
	other, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("open the other start's JetStream: %v", err)
	}
	creates, err := conn.SubscribeSync("$JS.API.STREAM.CREATE.KV_" + bucket)
	if err != nil {
		t.Fatalf("listen for the other start's create: %v", err)
	}
	t.Cleanup(func() { _ = creates.Unsubscribe() })
	if err := conn.FlushWithContext(wait); err != nil {
		t.Fatalf("flush the listener: %v", err)
	}
	createCtx, abandon := context.WithCancel(wait)
	abandoned := make(chan error, 1)
	go func() {
		_, err := other.CreateKeyValue(createCtx, jetstream.KeyValueConfig{Bucket: bucket, Storage: jetstream.FileStorage, Replicas: 3})
		abandoned <- err
	}()
	_, err = creates.NextMsgWithContext(wait)
	abandon()
	if err != nil {
		t.Fatalf("the other start's create never went out, so this row holds nothing: %v", err)
	}
	<-abandoned
	testwait.Until(t, "the meta leader to list the other start's stream", func() ([]string, bool) {
		// Each listing is cut to the row's deadline, and one the server leaves unanswered is asked
		// again after RequestTimeout.
		ask, cancel := context.WithTimeout(wait, RequestTimeout)
		defer cancel()
		var names []string
		for name := range js.StreamNames(nats.Context(ask)) {
			names = append(names, name)
		}
		return names, slices.Contains(names, "KV_"+bucket)
	})
	kv, err := EnsureKeyValue(wait, conn, &missingOnceJetStream{JetStreamContext: js}, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: 1})
	if err != nil {
		t.Fatalf("a start at one copy whose create met another start's: %v", err)
	}
	info := rowRead(t, wait, js, "KV_"+bucket)
	if info.Config.Replicas != 3 {
		t.Fatalf("the bucket's stream has %d copies, want the other start's 3: this start's create made the bucket, so this row holds nothing", info.Config.Replicas)
	}
	if info.Cluster == nil || info.Cluster.Leader == "" {
		t.Fatal("the start at one copy answered while the other start's stream had no leader")
	}
	cluster.AwaitBucket(t, bucket)
	if _, err := kv.Put("key", []byte("value")); err != nil {
		t.Fatalf("the first put after the start: %v", err)
	}
}

// A start that finds the notification stream another program raised a moment ago waits for its
// leader too, for the same reason: the raise was answered at once, and a publish sent while the
// stream has no leader draws no answer. The row checks that the start leaves a leader named, and
// its publish waits as the second row's does.
func TestAStartThatFindsAJustRaisedStreamAnswersOnceItHasALeader(t *testing.T) {
	cluster, js := startClusterJetStream(t)
	wait := rowWait(t)
	startOnCluster(t, cluster, WithReplicas(1)).Close()
	// Another program's raise, made as a start makes one (raiseStream), so an update the server
	// leaves unanswered is read again rather than taken for a refusal.
	if _, err := raiseStream(wait, js, rowRead(t, wait, js, Stream), 3); err != nil {
		t.Fatalf("another program's raise: %v", err)
	}
	client := startOnCluster(t, cluster, WithReplicas(3))
	if !hasLeader(t, wait, js, Stream) {
		t.Fatal("the start answered while the raised stream had no leader")
	}
	found := contracts.Envelope{
		EventID: "event-found-raised", Source: "dispatch",
		Topic: "notifications.agent.ses_found_raised", DedupeKey: "dispatch.event.found_raised",
	}
	cluster.AwaitStream(t, Stream, found.Topic)
	if err := client.Publish(found); err != nil {
		t.Fatalf("the first publish after the start: %v", err)
	}
}

// cutContext is a context the row ends when it chooses, by closing done, and that reports
// context.DeadlineExceeded from then on, as the context of a send whose wait ran out does. Its
// deadline and values are its parent's.
type cutContext struct {
	context.Context
	done chan struct{}
}

func (c *cutContext) Done() <-chan struct{} { return c.done }

func (c *cutContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// earlyAnswerJetStream loses the answer to a start's first create of the stream. That send goes
// out under a context the row ends the moment creates, a listener on the same connection, sees the
// create reach the server, and it answers context.DeadlineExceeded, as a send whose wait ran out
// does, whatever the server sent back. So the server has taken the first create, its answer never
// reaches the start, and the start sends the create again (sentAgain), on any machine. It records
// whether an answer the start took for a create sent again named no leader (leaderless).
type earlyAnswerJetStream struct {
	nats.JetStreamContext
	wait    context.Context
	creates *nats.Subscription
	sent    int
	// first is the first create as the listener saw it reach the server, nil when it saw none.
	first []byte
	// leaderless is whether a create's answer the start took named no leader for the stream.
	leaderless bool
}

func (js *earlyAnswerJetStream) AddStream(cfg *nats.StreamConfig, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	if js.sent++; js.sent > 1 {
		info, err := js.JetStreamContext.AddStream(cfg, opts...)
		if err == nil && (info.Cluster == nil || info.Cluster.Leader == "") {
			js.leaderless = true
		}
		return info, err
	}
	cut := &cutContext{Context: js.wait, done: make(chan struct{})}
	go func() {
		defer close(cut.done)
		if seen, err := js.creates.NextMsgWithContext(js.wait); err == nil {
			js.first = seen.Data
		}
	}()
	_, _ = js.JetStreamContext.AddStream(cfg, append(slices.Clone(opts), nats.Context(cut))...)
	<-cut.done
	return nil, context.DeadlineExceeded
}

// A start whose create of the stream at three copies is answered while the stream has no leader
// answers only once the stream has a leader. The row loses the start's first create's answer and
// lets the start send it again (earlyAnswerJetStream), reads the start's leader at once, then
// checks that each create reached the server and was the first one again.
//
// Whether the server answers the create sent again while the stream has no leader is the
// cluster's: the meta leader can take the resend before the first create, and the first election
// can end before the resend lands. Then this run tests nothing of the order, and passes, since
// the start's answer is right either way; it says which order it got. The order itself is held by
// the scripted row below, with no server. In 10 runs here, every run's answer named no leader.
func TestAStartWhoseStreamCreateIsAnsweredEarlyAnswersOnceItHasALeader(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	wait := rowWait(t)
	conn := testnats.Connect(t, cluster.URL())
	t.Cleanup(conn.Close)
	js, err := conn.JetStream(nats.MaxWait(RequestTimeout))
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	creates, err := conn.SubscribeSync(jsAPI("STREAM.CREATE", Stream))
	if err != nil {
		t.Fatalf("listen for the start's creates: %v", err)
	}
	t.Cleanup(func() { _ = creates.Unsubscribe() })
	if err := conn.FlushWithContext(wait); err != nil {
		t.Fatalf("flush the listener: %v", err)
	}
	cfg := parseConnectOptions([]ConnectOption{WithReplicas(3)}).streamConfig()
	early := &earlyAnswerJetStream{JetStreamContext: js, wait: wait, creates: creates}
	if _, err := ensureStreamWithConfig(wait, early, &cfg, capsAnUnsetStream, atStart); err != nil {
		t.Fatalf("a start whose create's first answer was lost: %v", err)
	}
	if !hasLeader(t, wait, js, Stream) {
		t.Fatal("the start answered while the stream it created had no leader")
	}
	if early.first == nil {
		t.Fatal("the listener never saw the start's first create reach the server, so this row holds nothing")
	}
	if early.sent < 2 {
		t.Fatalf("creates sent = %d, want the first one's answer lost and the create sent again", early.sent)
	}
	for i := 2; i <= early.sent; i++ {
		again, err := creates.NextMsgWithContext(wait)
		if err != nil {
			t.Fatalf("create %d of the %d the start sent never reached the server: %v", i, early.sent, err)
		}
		if !bytes.Equal(again.Data, early.first) {
			t.Fatalf("create %d the start sent was %s, want the first create's %s: an equal create", i, again.Data, early.first)
		}
	}
	t.Logf("a create's answer the start took named no leader: %t", early.leaderless)
}

// scriptedCreate is the start's JetStream context in the row below, with no server behind it. The
// first read finds no stream; the first create's answer is lost (context.DeadlineExceeded, as a
// send whose wait ran out); the second create is answered with a three-copy stream with no leader,
// as a server answers an equal create while the stream's first election runs
// (jetstream_cluster.go:6957-6979); and the reads after that answer find no leader once and then
// one. Every other call panics on the nil JetStreamContext, so the start makes no call the script
// does not name. It records each create's configuration as JSON taken the moment it is sent, so a
// map or slice the start changes between two sends shows in the bytes, and the reads made after
// the creates.
type scriptedCreate struct {
	nats.JetStreamContext
	mu      sync.Mutex
	creates [][]byte
	// config is the last create's configuration, which the reads after the creates answer.
	config nats.StreamConfig
	after  int
}

func (s *scriptedCreate) StreamInfo(stream string, _ ...nats.JSOpt) (*nats.StreamInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.creates) < 2 {
		return nil, nats.ErrStreamNotFound
	}
	s.after++
	info := &nats.StreamInfo{Config: s.config, Cluster: &nats.ClusterInfo{Leader: "n1"}}
	if s.after == 1 {
		info.Cluster.Leader = ""
	}
	return info, nil
}

func (s *scriptedCreate) AddStream(cfg *nats.StreamConfig, _ ...nats.JSOpt) (*nats.StreamInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sent, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	s.creates = append(s.creates, sent)
	s.config = *cfg
	if len(s.creates) == 1 {
		return nil, context.DeadlineExceeded
	}
	return &nats.StreamInfo{Config: *cfg, Cluster: &nats.ClusterInfo{}}, nil
}

// A start whose create of the stream at three copies is answered with no leader reads the stream
// until it has one before it answers, in the order a slow first election makes, forced on any
// machine (scriptedCreate): the start sends the create again after the first answer is lost, the
// resend is answered with no leader, and the start answers only after the read that names one. A
// start that took the create's answer as the stream's made none of those reads.
func TestAStartWhoseCreateIsAnsweredWithNoLeaderReadsTheStreamUntilItHasOne(t *testing.T) {
	cfg := parseConnectOptions([]ConnectOption{WithReplicas(3)}).streamConfig()
	script := &scriptedCreate{}
	if _, err := ensureStreamWithConfig(rowWait(t), script, &cfg, capsAnUnsetStream, atStart); err != nil {
		t.Fatalf("a start whose create was answered with no leader: %v", err)
	}
	script.mu.Lock()
	defer script.mu.Unlock()
	if len(script.creates) != 2 || !bytes.Equal(script.creates[0], script.creates[1]) {
		t.Fatalf("the start sent %d creates, %q; want the lost one and an equal one again", len(script.creates), script.creates)
	}
	if script.after < 2 {
		t.Fatalf("the start read the stream %d times after the create was answered with no leader, want at least 2: the read that found no leader and the one that found it", script.after)
	}
}

// A run of the reconnect hooks that finds the stream with no leader, as one whose other members
// are lost is, goes on to the hooks after the stream's ensure rather than waiting the start's hang
// guard for an election that run did not cause (atReconnect): those hooks rewatch the program's
// state, and a run that ended at the ensure would leave every cache it rewatches stale. It once
// waited the 36 s guard, failed the run with "the stream had no leader when the wait ended", and
// ran no hook after the ensure. Here two of the stream's three members are stopped until the
// row's cleanup, so the one the client is connected to answers the stream's read with no leader
// for as long as the row runs, and no election can end while it does: a run that waited for one
// ends at its hang guard with that error, or, waiting with no guard, does not end before the row's
// deadline (rowWait).
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
	wait := rowWait(t)
	deadline, _ := wait.Deadline()
	noLeader := testwait.Holds(time.Until(deadline), 100*time.Millisecond, func() bool {
		// Each read is cut to the row's deadline, and one the member leaves unanswered is asked again
		// after RequestTimeout.
		ask, cancel := context.WithTimeout(wait, RequestTimeout)
		defer cancel()
		info, err := client.JS().StreamInfo(Stream, nats.Context(ask))
		return err == nil && leaderless(info)
	})
	if !noLeader {
		t.Fatal("the remaining member never answered the stream's read with no leader: this row needs that read")
	}

	conn := client.Conn
	ran := make(chan error, 1)
	go func() { ran <- client.runReconnectHooks(conn) }()
	select {
	case err = <-ran:
	case <-wait.Done():
		t.Fatal("the run did not end: it waits for an election it did not cause, which cannot end while two of the stream's three members are stopped")
	}
	if err != nil || !after.Load() {
		t.Fatalf("a run that found the stream with no leader = %v, the hook after the ensure ran: %t; want nil and the hook run", err, after.Load())
	}
}
