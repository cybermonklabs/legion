// A start that creates or raises a stream of more than one copy, or finds one another program
// created or raised a moment ago, answers once the stream has a leader, on in-process
// three-server clusters (testnats.StartCluster). A row that needs the stream's first election
// slow forces the order a slow election makes; the issue's verification runs the rest against a
// server that holds that election.

package bus

import (
	"context"
	"fmt"
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

// updateWaitJetStream records when the first update of stream was sent through it, when the start
// stopped waiting on that update (the update's answer, or the next read of the stream the start
// began, which follows an update whose answer did not come), when the first update's own call
// returned an answer (firstAnswered), and when the update was sent the second time (resent). It
// reaches the cluster for everything, but for the first read of stream when answered is set: that
// read is answered with it, a read the test made itself.
type updateWaitJetStream struct {
	nats.JetStreamContext
	stream                               string
	answered                             *nats.StreamInfo
	sent, settled, firstAnswered, resent atomic.Int64
	sends, reads                         atomic.Int32
}

func (js *updateWaitJetStream) settle() {
	if js.sent.Load() != 0 {
		js.settled.CompareAndSwap(0, time.Now().UnixNano())
	}
}

func (js *updateWaitJetStream) StreamInfo(stream string, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	if stream == js.stream {
		if js.reads.Add(1) == 1 && js.answered != nil {
			return js.answered, nil
		}
		js.settle()
	}
	return js.JetStreamContext.StreamInfo(stream, opts...)
}

func (js *updateWaitJetStream) UpdateStream(cfg *nats.StreamConfig, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	if cfg.Name != js.stream {
		return js.JetStreamContext.UpdateStream(cfg, opts...)
	}
	send := js.sends.Add(1)
	switch send {
	case 1:
		js.sent.Store(time.Now().UnixNano())
	case 2:
		js.resent.Store(time.Now().UnixNano())
	}
	defer js.settle()
	info, err := js.JetStreamContext.UpdateStream(cfg, opts...)
	if send == 1 && err == nil {
		js.firstAnswered.Store(time.Now().UnixNano())
	}
	return info, err
}

// updateAnswerBound is how long a start may wait on one update of a stream before it reads the
// stream back: half the JetStream wait the update is sent under. A start that waited out an update
// no member answered took that whole wait, RequestTimeout, and one that reads the stream back after
// its read limit (firstReadLimit) takes a fraction of a second, so the bound sits far from both.
const updateAnswerBound = RequestTimeout / 2

// updateHoldBound is how long the row below keeps the stream's group without a leader while it
// waits for the start to stop waiting on its update: past RequestTimeout, so a start that waits
// for the update's answer stops waiting inside it, and the row ends in a failure that names that
// wait rather than at the test binary's deadline when a start never stops waiting.
const updateHoldBound = 2 * RequestTimeout

// A start that sets a bucket's maximum age, as the session registry's start does, while the
// bucket's stream elects its leader, reads the stream back within its read limit rather than
// waiting out the JetStream wait for an answer no member gives then, and sends the update again
// only when that read shows it did not land. nats-server v2.15.1 answers no update of a stream
// whose group has no leader, and the start once waited the whole ten seconds before it read the
// stream and found the age there.
//
// The row holds the election itself. The bucket's stream has three copies on a cluster of five, so
// the metadata group keeps its quorum when two members stop. Once the start has read the stream
// (the test's own read, handed to it as its first answer), the row stops the stream's leader and
// one other copy, so the update goes to a group that cannot elect a leader. A server that shuts
// down hands the leadership of its streams to a copy that is still up, and the copy left running
// can then lead the group alone for a while and answer the update at once; so the row reads that
// copy until a read is answered, each read cut at firstReadLimit as a start's reads are
// (testnats.HoldStreamWithoutLeader), goes on only on an answer that names no leader, and when the
// answer names one it starts both again and chooses anew. It keeps the two stopped until the start
// has sent its update and stopped waiting on it, and only then starts them again, so the group
// elects a leader and the start's read-back and its update sent again are answered. A start that
// waits for the update's answer waits RequestTimeout with the group leaderless, past
// updateAnswerBound, in every run; and an update answered while the two were stopped fails the
// row, which then held nothing.
func TestAnUpdateSentWhileTheStreamElectsItsLeaderIsReadBackWithinTheReadLimit(t *testing.T) {
	cluster := testnats.StartCluster(t, 5)
	const bucket = "envoy_update_during_election_test"
	stream := "KV_" + bucket
	cluster.CreateKeyValue(t, clusterConn(t, cluster), &nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: 3})
	held := testnats.HoldStreamWithoutLeader(t, cluster, stream, firstReadLimit)
	conn, js := memberJetStream(t, cluster, held.Kept)
	done := make(chan error, 1)
	timed := &updateWaitJetStream{JetStreamContext: js, stream: stream, answered: held.Info}
	go func() {
		age, err := SetKeyValueMaxAge(t.Context(), conn, timed, bucket, time.Hour)
		if err == nil && age != time.Hour {
			err = fmt.Errorf("the age set is %s", age)
		}
		done <- err
	}()
	if !testwait.Holds(updateHoldBound, testwait.Poll, func() bool { return timed.settled.Load() != 0 || len(done) != 0 }) {
		t.Errorf("the start did not stop waiting on its update within %s of a group with no leader", updateHoldBound)
	}
	restarted := time.Now().UnixNano()
	held.Restart(t)
	if err := <-done; err != nil {
		t.Fatalf("set the age of the bucket whose stream elects its leader = %v; want 1h0m0s and no error", err)
	}
	sent, settled, answered := timed.sent.Load(), timed.settled.Load(), timed.firstAnswered.Load()
	if sent == 0 || settled == 0 {
		t.Fatalf("the start sent its update at %d and stopped waiting on it at %d, want both: this row holds nothing", sent, settled)
	}
	if answered != 0 && answered < restarted {
		t.Fatalf("the first update was answered %s after it was sent, while two of the stream's three copies were stopped: a leader answered it, so this row holds nothing",
			time.Duration(answered-sent).Round(time.Millisecond))
	}
	if waited := time.Duration(settled - sent); waited >= updateAnswerBound {
		t.Fatalf("the start waited %s on its update, want under %s: an update no member answers is read back after the read limit, %s, and not waited out for the JetStream wait, %s",
			waited.Round(time.Millisecond), updateAnswerBound, firstReadLimit, RequestTimeout)
	}
	testwait.Eventually(t, "the age of "+stream+" read from its leader", func() bool {
		read, err := js.StreamInfo(stream, nats.MaxWait(firstReadLimit))
		return err == nil && read.Config.MaxAge == time.Hour
	})
}

// missingOnceJetStream answers the first KeyValue lookup "bucket not found", as a start's lookup
// is answered when it lands just before another start's create: the start then sends its own
// create and draws "stream name already in use". looked is atomic: a start sends each lookup on a
// goroutine of its own (Bounded), and one it abandoned at its limit runs on beside the next.
type missingOnceJetStream struct {
	nats.JetStreamContext
	looked atomic.Bool
}

func (js *missingOnceJetStream) KeyValue(bucket string) (nats.KeyValue, error) {
	if js.looked.CompareAndSwap(false, true) {
		return nil, nats.ErrBucketNotFound
	}
	return js.JetStreamContext.KeyValue(bucket)
}

// A start at one copy whose create meets a bucket another start, configured with three, created
// a moment before waits for that stream's leader: the other start's count is not this one's, and
// its create may have been answered before its stream's first election ended. The other start's
// create is abandoned the moment a listener on its subject sees it go out, so its answer is lost;
// the cut is made on the create itself, so the account read nats.go makes before it is answered
// first however slow the machine is. This start begins only once the metadata leader holds that
// create's stream (awaitAssigned), which its own create then meets. The create going out is not
// enough, because nats-server serves JetStream API requests on a pool of workers
// (processJSAPIRoutedRequests, v2.15.1 server/jetstream_api.go), so a create sent after it can be
// applied before it, and this start then made the bucket itself, at one copy, and the row held
// nothing (1 run in 100 at a one-minute load of 414). The wait is for the assignment and not for a
// read of the stream, because the stream answers a read only once a member has made it, by which
// time its first election is often over and this start would find a leader whether it waited for
// one or not.
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
	abandoned := make(chan struct{})
	go func() {
		defer close(abandoned)
		// The answer is discarded: this create is abandoned on purpose, so its wait fails nothing.
		_, _ = other.CreateKeyValue(createCtx, jetstream.KeyValueConfig{Bucket: bucket, Storage: jetstream.FileStorage, Replicas: 3})
	}()
	_, err = creates.NextMsg(time.Minute)
	abandon()
	if err != nil {
		t.Fatalf("the other start's create never went out, so this row holds nothing: %v", err)
	}
	<-abandoned
	awaitAssigned(t, js, "KV_"+bucket)
	kv, err := EnsureKeyValue(t.Context(), conn, &missingOnceJetStream{JetStreamContext: js}, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: 1})
	if err != nil {
		t.Fatalf("a start at one copy whose create met another start's: %v", err)
	}
	info, err := js.StreamInfo("KV_" + bucket)
	if err != nil {
		t.Fatalf("read the bucket's stream: %v", err)
	}
	if info.Config.Replicas != 3 {
		t.Fatalf("the bucket's stream has %d copies, want the other start's 3, whose create the metadata leader held before this start began", info.Config.Replicas)
	}
	if info.Cluster == nil || info.Cluster.Leader == "" {
		t.Fatal("the start at one copy answered while the other start's stream had no leader")
	}
	if _, err := kv.Put("key", []byte("value")); err != nil {
		t.Fatalf("the first put after the start: %v", err)
	}
}

// awaitAssigned waits until the JetStream metadata leader lists stream: the fact that a create of
// it has been applied, which a create of the same stream sent after it meets ("stream name already
// in use"), where the create's request going out is not. Only the metadata leader answers the list,
// from the assignments it has applied (jsStreamNamesRequest, v2.15.1 server/jetstream_api.go). Each
// list is waited for up to firstReadLimit and then asked again, as a start reads (limitedRead), so a
// list a member leaves unanswered costs the row that and not the context's whole wait.
func awaitAssigned(t *testing.T, js nats.JetStreamContext, stream string) {
	t.Helper()
	testwait.Eventually(t, "the metadata leader to hold "+stream, func() bool {
		listed := false
		for name := range js.StreamNames(nats.MaxWait(firstReadLimit)) {
			listed = listed || name == stream
		}
		return listed
	})
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
	testnats.UpdateStream(t, js, raised)
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
// order a slow election makes, forced without holding the election. sent is atomic: the create
// sent again waits under a doubled limit, so a copy of it goes out beside it while it waits past
// the first limit (limitedRead), and a copy still in flight when the start returns runs on beside
// the test.
type earlyAnswerJetStream struct {
	nats.JetStreamContext
	sent atomic.Int32
}

func (js *earlyAnswerJetStream) AddStream(cfg *nats.StreamConfig, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	if js.sent.Add(1) == 1 {
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
	if sent := early.sent.Load(); sent < 2 {
		t.Fatalf("creates sent = %d, want the first one's answer lost and the create sent again", sent)
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
