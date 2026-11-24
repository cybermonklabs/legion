package testnats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testwait"
)

// A lookup of a stream through a cluster member is answered by whichever server answers for the
// stream, not by that member out of its own metadata: the meta leader, the stream's leader, or a
// new group's preferred member, which answers "stream not found" until it has made the stream
// (nats-server v2.15.1 server/jetstream_api.go:2051-2180). A clustered create is answered once the
// meta group has committed it, while each member applies the new assignment on its own clock.
// Measured here with three in-process members on two CPUs (nats-server 2.10.29), 67 of 75 lookups
// through the members taken the moment a create was answered still said "stream not found", the
// slowest for 206 ms, through the member the create was sent through too.
//
// Whatever reaches the bucket next reaches it through whichever member its own connection is on:
// bus.EnsureKeyValue opens the jetstream handle its KeyValue carries, a rebuild calls
// bus.OpenKeyValue, a second program looks the bucket up before it raises it, and a first put
// writes to it. So a test that creates a bucket on a cluster and then has the code under test
// open it, raise it, look it up or write to it creates it with CreateKeyValue, and one whose
// code under test creates the bucket itself calls AwaitBucket after it, on the connection and
// the JetStream context that code holds.

const (
	// createSendWait bounds each request of a test fixture's own create of a stream or a bucket
	// (sendCreate), and a send the requests of which drew no answer within it is sent again. A
	// create on a cluster is proposed through the meta group and answered once the stream, a
	// bucket's included, is made, and a stream of more than one copy answers it from its first
	// leader, once its members have elected one: 25 bucket creates on an idle two-CPU machine took
	// from 60 ms to 1.03 s. A send it ends decides nothing: the next send is answered with the
	// stream the first one made, so it paces the sends and no more. A stream's create is one
	// request (nats.go v1.55.0 jsm.go:924), so createSendWait bounds its send as well.
	createSendWait = 10 * time.Second

	// bucketSendWait bounds one send of a bucket's create, which makes up to four requests, each
	// bounded by createSendWait: nats.go v1.55.0's CreateKeyValue reads the account's JetStream
	// information, then creates the bucket's stream, and on "stream name already in use" reads
	// that stream and may update it (kv.go:432, 525, 535 and 542).
	bucketSendWait = 4 * createSendWait

	// bucketLookupWait bounds one lookup of a bucket's stream, or one probe of it (AwaitStream),
	// and one the server leaves unanswered within it is asked again. It paces those asks and
	// decides nothing: a wait's end is its own (waitEnd).
	bucketLookupWait = 10 * time.Second

	// noDeadline is the bound of a wait that ends only on its fact (sendCreate) in a test binary
	// run with no deadline (go test -timeout 0), which asked for none: a day is none in practice.
	noDeadline = 24 * time.Hour
)

// waitBound is bound, or what the test's deadline (go test -timeout) leaves less margin when that
// is sooner, as the shared wait bounds a wait (testwait.Bounded), so the wait fails naming its fact
// before go test ends the binary. margin covers what the caller does past the bound before its
// failure: the ask of the fact under way as the bound passes, the one more ask the shared wait
// makes once its bound has passed, and whatever the caller reads after the answer. A t that
// reports no deadline, as a binary run with -timeout 0 does, is bounded by bound.
func waitBound(t testing.TB, bound, margin time.Duration) time.Duration {
	if timed, ok := t.(interface{ Deadline() (time.Time, bool) }); ok {
		if deadline, ok := timed.Deadline(); ok {
			if left := time.Until(deadline) - margin; left < bound {
				return left
			}
		}
	}
	return bound
}

// sendCreate sends a test fixture's own create of a stream or a bucket on conn until the cluster
// answers it, and returns the answer: nil once the stream is made, or the cluster's refusal. send
// makes one create through a JetStream context on conn whose requests createSendWait bounds,
// publishing to subjects; sendWait is the longest one send can take: createSendWait for a
// stream's create, bucketSendWait for a bucket's.
//
// nats-server v2.15.1 leaves a stream create unanswered on several paths, which internal/bus's
// sentAgain lists with their source lines, and sends no answer later: a fixture that waited two
// minutes for one failed TestADispatchStartKeepsAThreeCopyStreamsCopiesByteLimitAndIDAsItCorrectsTheStream
// at its first create, in a whole-package run on a loaded machine. The answer to a create the
// cluster took is also lost with the connection it was sent on when that connection drops before
// the stream's first leader answers. A send again recovers that answer only on a connection that
// reconnects, as CreateStream's own does; CreateKeyValue sends on its caller's, and none of its
// callers' connections reconnects (Connect, ConnectSeed, internal/bus's clusterConn), so there the
// drop ends the create with the connection's error.
//
// A second identical create is answered with the stream the first one made: the meta leader
// proposes the equal assignment again (jsClusteredStreamRequest) and each member that holds the
// stream answers it (processClusterCreateStream). So a create that landed costs one more send,
// never a refusal. A publish conn's grant does not cover draws no answer either, as the server
// reports it on the connection and never in the answer; the refusal conn records during the send
// (refusalOf) is then the answer, so a refused create is sent once. The sends end otherwise only on
// an answer, or at the wait's bound (waitBound), where the failure names the sends that drew none.
// That bound leaves before the test's deadline the send under way as it passes, the one more send
// the shared wait (testwait.Holds) makes after it, and the one read of the stream held that a
// caller makes on an "already in use" answer, so the caller's failure comes before go test's
// timeout panic.
func sendCreate(t testing.TB, conn *natsgo.Conn, what string, sendWait time.Duration, subjects []string, send func() error) error {
	t.Helper()
	bound := waitBound(t, noDeadline, 2*sendWait+createSendWait+time.Second)
	sends := 0
	var unanswered, answer error
	if !testwait.Holds(bound, testwait.Poll, func() bool {
		sends++
		before := conn.LastError()
		err := send()
		if errors.Is(err, natsgo.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) {
			if refusal := refusalOf(conn, before, subjects); refusal != nil {
				answer = refusal
				return true
			}
			unanswered = err
			return false
		}
		answer = err
		return true
	}) {
		t.Fatalf("%s: %d sends of it drew no answer within %s, the last %v", what, sends, bound.Round(time.Second), unanswered)
	}
	return answer
}

// refusalOf is the server's refusal of a publish to one of subjects that conn recorded after
// before, its last error as the send began, or nil. nats.go records the server's report of a
// publish the connection's grant does not cover as the connection's last error
// (processTransientError, nats.go v1.55.0 nats.go:4173-4176), each report a new error value.
func refusalOf(conn *natsgo.Conn, before error, subjects []string) error {
	err := conn.LastError()
	if !errors.Is(err, natsgo.ErrPermissionViolation) {
		return nil
	}
	for _, subject := range subjects {
		if strings.Contains(err.Error(), `Publish to "`+subject+`"`) && err != before {
			return err
		}
	}
	return nil
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
	stream := "KV_" + config.Bucket
	// The subjects CreateKeyValue publishes to: the account read, the create, and on "already in
	// use" the update (its read of the stream held ignores a refusal).
	subjects := []string{"$JS.API.INFO", "$JS.API.STREAM.CREATE." + stream, "$JS.API.STREAM.UPDATE." + stream}
	err = sendCreate(t, conn, "create KV bucket "+config.Bucket, bucketSendWait, subjects, func() error {
		var err error
		kv, err = js.CreateKeyValue(config)
		return err
	})
	if errors.Is(err, natsgo.ErrStreamNameAlreadyInUse) {
		asked, _ := json.Marshal(config)
		t.Fatalf("create KV bucket %s: the cluster holds its stream with another configuration: it holds %s, the create asked for the bucket %s",
			config.Bucket, heldStream(js, stream), asked)
	}
	if err != nil {
		t.Fatalf("create KV bucket %s: %v", config.Bucket, err)
	}
	c.AwaitBucket(t, config.Bucket)
	return kv
}

// AwaitBucket waits, until testDeadline, until every running member of the cluster answers a
// lookup of the stream of bucket and serves a write to the bucket (AwaitStream). A member's
// metadata and the routes a write takes to the bucket's stream arrive on their own clocks, and a
// write through a member that has neither yet is answered "no responders", which nats.go gives a
// caller as `nats: no response from stream`.
func (c *Cluster) AwaitBucket(t testing.TB, bucket string) {
	t.Helper()
	// A subject of the bucket, which the stream's own `$KV.<bucket>.>` subscription matches: the
	// subject a put, a delete or a watch of any key is routed by.
	c.AwaitStream(t, "KV_"+bucket, "$KV."+bucket+".a")
}

// AwaitStream waits, until testDeadline, each request cut to what is left (waitEnd), until one
// pass over the running members finds every member serving stream: a lookup of stream through the
// member is answered and names the leader every other lookup of the pass named, and a probe
// publish to subject, one of the stream's subjects, through the member is answered by stream as
// the stream that leads it (serves). A pass in which a member does not, or names another leader,
// starts over, so a lead that moved during the wait is asked of every member again. Which server
// answers a lookup is the server's choice, the stream's leader as a rule (nats-server v2.15.1
// server/jetstream_api.go:2051-2133), so the lookup says the stream is there and who leads it, not
// that this member has applied it. A pass that passes says every member reached a leading stream
// at that moment. It says nothing of a quorum to store a write (a leader whose followers stopped
// answers the probe until it steps down) or of a later election.
//
// The probe carries Nats-Expected-Stream naming another stream, and a leading stream refuses it
// with JSStreamNotMatchErr and its own name before it proposes anything to its group
// (jetstream_cluster.go:13190-13206, jetstream_batching.go:595-597; a stream of one copy at
// stream.go:6750-6759, before it stores anything), while a stream that does not lead drops it
// unanswered (jetstream_cluster.go:13119-13122). So the refusal is the fact a write needs, and no
// subscription stands in for it:
//   - a read of the stream names its leader before that member serves writes: the Raft group names
//     it at once (raft.go:6307), it leads only once its first entries are processed
//     (raft.go:1603-1613), and its stream subscribes to its subjects after that (stream.go:1479).
//     A write sent between draws "no responders", which nats.go sends twice more 250 ms apart and
//     gives its caller as `nats: no response from stream` (nats.go v1.55.0 js.go:597-623);
//   - after a raise from one copy, the member that held the copy keeps its subscription while the
//     new group elects, and drops what that subscription takes until it leads.
//
// A caller waits on a stream whose copies are not being changed under it: once a raise's leader
// is named, as every caller here waits for first. A probe that lands while a one-copy stream takes
// on its group can be taken by the old one-copy path and checked as a clustered stream's, which
// skips the expected-stream check, and stored (stream.go:8674-8686, :6558-6559). A stream that
// takes no probe at all is refused at once (unprobeable).
//
// The lookup and the probe go through connections of the cluster's no-auth user (Connect), so the
// stream is read and probed in that user's account only, and no subscription of another account
// answers a probe. A caller whose stream is in another account gets that account's answer for the
// name, which is no stream (the wait fails by name) unless the no-auth account holds a stream of
// the same name and subject too; no caller here makes one.
func (c *Cluster) AwaitStream(t testing.TB, stream, subject string) {
	t.Helper()
	bound := boundOf(t)
	conns := make([]*natsgo.Conn, len(c.Servers))
	defer func() {
		for _, conn := range conns {
			if conn != nil {
				conn.Close()
			}
		}
	}()
	for {
		failed, leader := "", ""
		for i, server := range c.Servers {
			if !server.running() {
				continue
			}
			if conns[i] == nil {
				conns[i] = Connect(t, server.URL())
			}
			named, why, err := serves(bound, conns[i], stream, subject)
			if err != nil {
				t.Fatalf("await stream %s: %v", stream, err)
			}
			if why == "" && leader != "" && named != leader {
				why = fmt.Sprintf("its lookup named leader %s, an earlier member's %s", named, leader)
			}
			if why != "" {
				failed = fmt.Sprintf("member %s: %s", c.Name(i), why)
				break
			}
			leader = named
		}
		if failed == "" {
			return
		}
		if bound.over() {
			t.Fatalf("not every running member served stream %s by %s, short of the test's deadline: %s", stream, bound, failed)
		}
		bound.pause()
	}
}

// probeStream is the stream a probe publish names as expected (serves): no stream is named so,
// since a stream name holds no space.
const probeStream = "testnats probe"

// probeHeaderBytes is the size of the probe's header as nats.go writes it ("NATS/1.0", one header
// line and the blank line, nats.go v1.55.0 nats.go headerBytes), the whole of a probe, which has
// no body.
var probeHeaderBytes = len("NATS/1.0\r\n" + natsgo.ExpectedStreamHdr + ": " + probeStream + "\r\n\r\n")

// serves asks, through conn, for a lookup of stream and a probe publish to subject (AwaitStream),
// each cut to what is left of bound, and returns the leader the lookup named and why the stream is
// not served through conn, "" when it is: the lookup was answered and stream refused the probe as
// another stream's. Its error is a stream no probe can be answered by (unprobeable).
func serves(bound waitEnd, conn *natsgo.Conn, stream, subject string) (leader, why string, err error) {
	js, err := conn.JetStream()
	if err != nil {
		return "", fmt.Sprintf("open JetStream: %v", err), nil
	}
	ctx, cancel := bound.stepContext(bucketLookupWait)
	info, err := js.StreamInfo(stream, natsgo.Context(ctx))
	cancel()
	if err != nil {
		return "", fmt.Sprintf("a lookup of the stream: %v", err), nil
	}
	if err := unprobeable(info.Config); err != nil {
		return "", "", err
	}
	if info.Cluster != nil {
		leader = info.Cluster.Leader
	}
	if leader == "" {
		return "", "its lookup named no leader", nil
	}
	ctx, cancel = bound.stepContext(bucketLookupWait)
	defer cancel()
	probe := &natsgo.Msg{Subject: subject, Header: natsgo.Header{natsgo.ExpectedStreamHdr: []string{probeStream}}}
	answer, err := conn.RequestMsgWithContext(ctx, probe)
	if err != nil {
		return leader, fmt.Sprintf("a probe publish to %s: %v", subject, err), nil
	}
	var ack struct {
		Stream string           `json:"stream"`
		Error  *natsgo.APIError `json:"error"`
	}
	if json.Unmarshal(answer.Data, &ack) != nil || ack.Stream != stream || ack.Error == nil ||
		ack.Error.ErrorCode != natsgo.ErrorCode(natsserver.JSStreamNotMatchErr) {
		return leader, fmt.Sprintf("a probe publish to %s was answered %q, not refused by %s as another stream's", subject, answer.Data, stream), nil
	}
	return leader, "", nil
}

// unprobeable is why a stream with config takes no probe, nil when it takes one:
//   - a stream that sends no acknowledgement answers nothing (jetstream_cluster.go:13081);
//   - a mirror takes no publish, since it subscribes to no subject of its own;
//   - a sealed stream refuses every publish before it reads the expected stream
//     (jetstream_cluster.go:13125-13130);
//   - one whose largest message is smaller than the probe refuses it for its size before it reads
//     the expected stream (jetstream_cluster.go:13163-13173, stream.go:6666-6676).
func unprobeable(config natsgo.StreamConfig) error {
	switch {
	case config.NoAck:
		return errors.New("the stream sends no acknowledgement, so no probe of it is answered")
	case config.Mirror != nil:
		return errors.New("the stream is a mirror, which takes no publish, so no probe of it is answered")
	case config.Sealed:
		return errors.New("the stream is sealed, which refuses every publish before the probe's expected stream is read")
	case config.MaxMsgSize > 0 && int(config.MaxMsgSize) < probeHeaderBytes:
		return fmt.Errorf("the stream's largest message, %d bytes, is smaller than the %d-byte probe", config.MaxMsgSize, probeHeaderBytes)
	}
	return nil
}
