package testnats

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testwait"
)

// A cluster member answers a stream lookup out of the JetStream metadata it has applied itself.
// nats-server 2.10.29 routes a KV create to the meta leader, which answers the client as soon as
// the bucket's stream is made, while every member applies the meta entry that carries the new
// assignment on its own clock; until a member has applied it, that member answers "stream not
// found" for the bucket that was just created, the member the create was sent through included.
// Measured here with three in-process members on two CPUs, 67 of 75 member reads taken the moment
// a create was answered still said so, the slowest for 206 ms.
//
// Whatever reaches the bucket next reaches it through whichever member its own connection is on:
// bus.EnsureKeyValue opens the jetstream handle its KeyValue carries, a rebuild calls
// bus.OpenKeyValue, a second program looks the bucket up before it raises it, and a first put
// writes to it. So a test that creates a bucket on a cluster and then has the code under test
// open it, raise it, look it up or write to it creates it with CreateKeyValue, and one whose
// code under test creates the bucket itself calls AwaitBucket after it, on the connection and
// the JetStream context that code holds.

const (
	// createSendWait bounds one send of a test fixture's own create of a stream or a bucket
	// (sendCreate), after which the create is sent again. A create on a cluster is proposed
	// through the meta group and answered once the stream, a bucket's included, is made, and a
	// stream of more than one copy answers it from its first leader, once its members have
	// elected one: 25 bucket creates on an idle two-CPU machine took from 60 ms to 1.03 s. A send
	// it ends decides nothing: the next send is answered with the stream the first one made, so
	// it paces the sends and no more.
	createSendWait = 10 * time.Second

	// bucketLookupWait bounds one member's lookup of a bucket's stream, which that member
	// answers out of metadata it holds. A hang guard.
	bucketLookupWait = 10 * time.Second

	// noDeadline is the bound of a wait that ends only on its fact (sendCreate) in a test binary
	// run with no deadline (go test -timeout 0), which asked for none: a day is none in practice.
	noDeadline = 24 * time.Hour
)

// waitBound is bound, or what the test binary's deadline (go test -timeout) leaves less margin
// when that is sooner, as the shared wait bounds a wait (testwait.Bounded), so the wait fails
// naming its fact before go test ends the binary. margin covers what one ask of the fact can take,
// and the one more ask the shared wait makes once its bound has passed. A t that is not a
// *testing.T has no deadline to read, and is bounded by bound.
func waitBound(t testing.TB, bound, margin time.Duration) time.Duration {
	if test, ok := t.(*testing.T); ok {
		return testwait.Bounded(test, bound, margin)
	}
	return bound
}

// sendCreate sends a test fixture's own create of a stream or a bucket, one send each
// createSendWait, until the cluster answers it, and returns the answer: nil once the stream is
// made, or the cluster's refusal. send makes one create through a JetStream context whose
// requests createSendWait bounds.
//
// nats-server v2.15.1 leaves a stream create unanswered on several paths, which internal/bus's
// sentAgain lists with their source lines: a create through a member that is not the leader of a
// young meta group, one whose meta proposal the group's term moved past (a meta leader change at
// the create), and one a full JetStream API queue drops. And the answer to a create the cluster
// took is lost with the connection it was sent on when that connection drops before the stream's
// first leader answers. Neither side sends a create again, so a fixture that sends it once waits
// for an answer that never comes, however long it waits: a fixture that waited two minutes for it
// failed TestADispatchStartKeepsAThreeCopyStreamsCopiesByteLimitAndIDAsItCorrectsTheStream at its
// first create, in a whole-package run on a loaded machine.
//
// A second identical create is answered with the stream the first one made: the meta leader
// proposes the equal assignment again (jsClusteredStreamRequest) and each member that holds the
// stream answers it (processClusterCreateStream). So a create that landed costs one more send,
// never a refusal, and the sends end only on an answer, or at the test binary's deadline
// (waitBound) through the shared wait (testwait.Holds), where the failure names the sends that
// drew none.
func sendCreate(t testing.TB, what string, send func() error) error {
	t.Helper()
	sends := 0
	var unanswered, answer error
	if !testwait.Holds(waitBound(t, noDeadline, 2*createSendWait+time.Second), testwait.Poll, func() bool {
		sends++
		err := send()
		if errors.Is(err, natsgo.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) {
			unanswered = err
			return false
		}
		answer = err
		return true
	}) {
		t.Fatalf("%s: %d sends of it drew no answer before the test binary's deadline, the last %v", what, sends, unanswered)
	}
	return answer
}

// CreateKeyValue creates the KV bucket config names on conn, sent until the cluster answers it
// (sendCreate), and returns its handle once every running member of the cluster answers for the
// bucket's stream (AwaitBucket). conn is the connection whose grant the create is made under, so a
// test of a client's permissions creates the bucket as that client. A bucket whose stream the
// cluster already holds with another configuration fails the test with both: the stream the
// cluster holds, and the bucket the create asked for.
func (c *Cluster) CreateKeyValue(t testing.TB, conn *natsgo.Conn, config *natsgo.KeyValueConfig) natsgo.KeyValue {
	t.Helper()
	js, err := conn.JetStream(natsgo.MaxWait(createSendWait))
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	var kv natsgo.KeyValue
	err = sendCreate(t, "create KV bucket "+config.Bucket, func() error {
		var err error
		kv, err = js.CreateKeyValue(config)
		return err
	})
	if errors.Is(err, natsgo.ErrStreamNameAlreadyInUse) {
		asked, _ := json.Marshal(config)
		t.Fatalf("create KV bucket %s: the cluster holds its stream with another configuration: it holds %s, the create asked for the bucket %s",
			config.Bucket, heldStream(js, "KV_"+config.Bucket), asked)
	}
	if err != nil {
		t.Fatalf("create KV bucket %s: %v", config.Bucket, err)
	}
	c.AwaitBucket(t, config.Bucket)
	return kv
}

// AwaitBucket waits, within connectTimeout, until every running member of the cluster both
// answers for the stream of bucket and holds the interest a write to the bucket is routed by. A
// member answers a lookup out of the metadata it has applied, and routes a write by the
// subscription the bucket's stream holds on its subjects, which reaches a member that does not
// host the stream over the cluster's routes; the two arrive on their own clocks, and a write
// through a member that has neither yet is answered "no responders", which nats.go gives a
// caller as `nats: no response from stream`.
func (c *Cluster) AwaitBucket(t testing.TB, bucket string) {
	t.Helper()
	stream := "KV_" + bucket
	// A subject of the bucket, which the stream's own `$KV.<bucket>.>` subscription matches: the
	// interest a put, a delete or a watch of any key is routed by.
	subject := "$KV." + bucket + ".a"
	for i, server := range c.Servers {
		if !server.running() {
			continue
		}
		c.awaitBucketOn(t, i, server, stream, subject)
	}
}

// awaitBucketOn waits for member i to answer for stream and to hold the interest subject is
// routed by, the member's own account read in this process.
func (c *Cluster) awaitBucketOn(t testing.TB, i int, server *Server, stream, subject string) {
	t.Helper()
	conn := Connect(t, server.URL())
	defer conn.Close()
	js, err := conn.JetStream(natsgo.MaxWait(bucketLookupWait))
	if err != nil {
		t.Fatalf("open JetStream on member %s: %v", c.Name(i), err)
	}
	deadline := time.Now().Add(connectTimeout)
	for {
		_, err := js.StreamInfo(stream)
		routed := false
		if srv := server.natsServer(); srv != nil {
			routed = srv.GlobalAccount().SubscriptionInterest(subject)
		}
		if err == nil && routed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("member %s did not answer for stream %s and route %s within %s: %v (routed %t)", c.Name(i), stream, subject, connectTimeout, err, routed)
		}
		time.Sleep(retryInterval)
	}
}
