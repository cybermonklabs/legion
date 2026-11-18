package testnats

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testwait"
)

// JetStream is a JetStream context, each request of it bounded by what is left of the test
// binary's deadline (MaxWait), on whichever running member of the cluster answers (Connect),
// closed when the test ends.
func (c *Cluster) JetStream(t testing.TB) natsgo.JetStreamContext {
	t.Helper()
	conn := Connect(t, c.URL())
	t.Cleanup(conn.Close)
	js, err := conn.JetStream(MaxWait(t))
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	return js
}

// CreateStream creates the stream config names on a connection of its own to the cluster
// (JetStream), and fails the test with the server's answer when the create is refused. It sends
// the create once and waits for its answer until the test binary's deadline (MaxWait).
// nats-server answers the successful create of a stream of more than one copy from that stream's
// first leader, once its members have elected one (processStreamLeaderChange), which on a loaded
// machine can take many seconds; a refusal, such as a placement the cluster cannot make, is
// answered before any election.
func (c *Cluster) CreateStream(t testing.TB, config natsgo.StreamConfig) {
	t.Helper()
	if _, err := c.JetStream(t).AddStream(&config); err != nil {
		t.Fatalf("create stream %s: %v", config.Name, err)
	}
}

// Copies is how many copies of a stream or a consumer a cluster holds, its leader's and its
// replicas', as its info's Cluster reports them: 0 for none reported.
func Copies(cluster *natsgo.ClusterInfo) int {
	if cluster == nil {
		return 0
	}
	return 1 + len(cluster.Replicas)
}

// settled reports whether cluster shows n settled copies: a leader and n-1 replicas, every one
// current with the leader and none offline. Every wait below settles on it.
func settled(cluster *natsgo.ClusterInfo, n int) bool {
	return cluster != nil && cluster.Leader != "" && len(cluster.Replicas) == n-1 &&
		!slices.ContainsFunc(cluster.Replicas, func(peer *natsgo.PeerInfo) bool { return !peer.Current || peer.Offline })
}

// AwaitSettledStream waits until stream holds as many settled copies as its configuration asks
// (settled), and returns its info. A test that asserts the copies a write left reads them from the
// info it returns.
func AwaitSettledStream(t testing.TB, js natsgo.JetStreamContext, stream string) *natsgo.StreamInfo {
	t.Helper()
	return awaitInfo(t, fmt.Sprintf("stream %s to settle", stream),
		func() (*natsgo.StreamInfo, error) { return js.StreamInfo(stream, natsgo.MaxWait(readWait)) },
		func(info *natsgo.StreamInfo) bool { return settled(info.Cluster, info.Config.Replicas) },
		describeStream)
}

// AwaitStreamCopies waits until stream is configured with n copies and holds n settled ones
// (settled), and returns its info. A member that was killed or lame-ducked mid-write rejoins and
// catches its streams up, which this waits out however long it takes.
func AwaitStreamCopies(t testing.TB, js natsgo.JetStreamContext, stream string, n int) *natsgo.StreamInfo {
	t.Helper()
	return awaitInfo(t, fmt.Sprintf("stream %s to hold %d settled copies", stream, n),
		func() (*natsgo.StreamInfo, error) { return js.StreamInfo(stream, natsgo.MaxWait(readWait)) },
		func(info *natsgo.StreamInfo) bool { return info.Config.Replicas == n && settled(info.Cluster, n) },
		describeStream)
}

// AwaitConsumerCopies waits until stream's consumer holds n settled copies (settled), as a durable
// that sets no replica count of its own does once it has followed its stream to n, and returns its
// info.
func AwaitConsumerCopies(t testing.TB, js natsgo.JetStreamContext, stream, consumer string, n int) *natsgo.ConsumerInfo {
	t.Helper()
	return awaitInfo(t, fmt.Sprintf("consumer %s of %s to hold %d settled copies", consumer, stream, n),
		func() (*natsgo.ConsumerInfo, error) {
			return js.ConsumerInfo(stream, consumer, natsgo.MaxWait(readWait))
		},
		func(info *natsgo.ConsumerInfo) bool { return settled(info.Cluster, n) },
		describeConsumer)
}

// AwaitStreamLeader waits until stream has a leader other than not, the server_name of a member
// the test stopped or "" for any, and returns its info.
func AwaitStreamLeader(t testing.TB, js natsgo.JetStreamContext, stream, not string) *natsgo.StreamInfo {
	t.Helper()
	return awaitInfo(t, fmt.Sprintf("stream %s to have a leader other than %q", stream, not),
		func() (*natsgo.StreamInfo, error) { return js.StreamInfo(stream, natsgo.MaxWait(readWait)) },
		func(info *natsgo.StreamInfo) bool {
			return info.Cluster != nil && info.Cluster.Leader != "" && info.Cluster.Leader != not
		},
		describeStream)
}

// describeStream is what a stream wait prints of the info it read last when it ran out: the copies
// the stream's configuration asks for, and every copy the cluster reports (describeCopies).
func describeStream(info *natsgo.StreamInfo) string {
	if info == nil {
		return "no stream info was read"
	}
	return describeCopies(info.Config.Replicas, info.Cluster)
}

// describeConsumer is describeStream for a consumer wait. A durable that sets no replica count of
// its own reports none configured and follows its stream's count.
func describeConsumer(info *natsgo.ConsumerInfo) string {
	if info == nil {
		return "no consumer info was read"
	}
	return describeCopies(info.Config.Replicas, info.Cluster)
}

// describeCopies is the line both describers print: the copies the configuration asks for, and
// the state of every copy the cluster reports (describeCluster). Go gives a type parameter no way
// to reach those two fields, so the two describers stay and share this.
func describeCopies(configured int, cluster *natsgo.ClusterInfo) string {
	return fmt.Sprintf("%d copies configured, %s", configured, describeCluster(cluster))
}

// describeCluster names every peer the cluster reports and the state settled reads: the leader,
// and each replica's name, whether it is current with the leader, whether it is offline, how long
// since it was last active and how far behind it is. A wait that ran out prints this because
// printing the info itself prints the ClusterInfo as an address, so no CI log of a failed wait
// could name the peer that was behind.
func describeCluster(cluster *natsgo.ClusterInfo) string {
	if cluster == nil {
		return "the server reported no cluster for it"
	}
	peers := make([]string, 0, len(cluster.Replicas))
	for _, peer := range cluster.Replicas {
		if peer == nil {
			peers = append(peers, "a nil peer")
			continue
		}
		peers = append(peers, fmt.Sprintf("%s current=%t offline=%t active=%s lag=%d",
			peer.Name, peer.Current, peer.Offline, peer.Active, peer.Lag))
	}
	return fmt.Sprintf("%d copies reported, leader %q, replicas [%s]",
		Copies(cluster), cluster.Leader, strings.Join(peers, "; "))
}

// infoRead is one read of awaitInfo's wait: what the describer says of the info it read, and the
// server's answer, which is what the failure of a wait that ran out prints.
type infoRead[T any] struct {
	info     *T
	err      error
	describe func(*T) string
}

func (r infoRead[T]) String() string { return fmt.Sprintf("%s (%v)", r.describe(r.info), r.err) }

// awaitInfo reads an info through the shared wait until a read succeeds and done holds for it, and
// returns that info. Each caller bounds its read by readWait, whatever the wait of the context it
// was given, so a read sent across a leader change that draws no answer is sent again: a member
// that still counts a stream's or a consumer's leader that is gone answers nothing for it
// (nats-server v2.15.1, jsStreamInfoRequest, jsConsumerInfoRequest, isGroupLeaderless). The
// failure of a wait that ran out prints what describe says of the info it read last and the last
// error.
func awaitInfo[T any](t testing.TB, what string, read func() (*T, error), done func(*T) bool, describe func(*T) string) *T {
	t.Helper()
	return testwait.Until(t, what, func() (infoRead[T], bool) {
		info, err := read()
		return infoRead[T]{info: info, err: err, describe: describe}, err == nil && done(info)
	}).info
}

// StreamWrites records every stream create and update a JetStream account's clients ask for: a
// request on the account's $JS.API.STREAM.CREATE or $JS.API.STREAM.UPDATE subject, a bucket's
// stream (KV_<bucket>) among them, reaches its subscriber as it reaches the server.
type StreamWrites struct {
	conn *natsgo.Conn
	sub  *natsgo.Subscription
}

// WatchStreamWrites starts recording the stream writes asked of the account uri connects to, a
// lone server's (Start) or a test's own (URL), until the test ends.
func WatchStreamWrites(t testing.TB, uri string) *StreamWrites {
	t.Helper()
	conn := Connect(t, uri)
	t.Cleanup(conn.Close)
	sub, err := conn.SubscribeSync("$JS.API.STREAM.*.*")
	if err != nil {
		t.Fatalf("watch stream writes: %v", err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("watch stream writes: %v", err)
	}
	return &StreamWrites{conn: conn, sub: sub}
}

// Take returns the subjects of the creates and updates asked since the last Take. Each request
// reached the subscription before the server answered it, so the flush, a round trip after the
// writer's answer, leaves every one queued here.
func (w *StreamWrites) Take(t testing.TB) []string {
	t.Helper()
	if err := w.conn.Flush(); err != nil {
		t.Fatalf("flush the stream-write watch: %v", err)
	}
	var subjects []string
	for {
		msg, err := w.sub.NextMsg(50 * time.Millisecond)
		if err != nil {
			return subjects
		}
		if strings.HasPrefix(msg.Subject, "$JS.API.STREAM.CREATE.") || strings.HasPrefix(msg.Subject, "$JS.API.STREAM.UPDATE.") {
			subjects = append(subjects, msg.Subject)
		}
	}
}
