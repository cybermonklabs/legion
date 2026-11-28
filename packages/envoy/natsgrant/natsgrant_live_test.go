//go:build natslive

package natsgrant_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/natsgrant"
)

// grantedConnection connects to uri as the nkey user whose seed is seed, with options, and
// returns the connection with the asynchronous errors the server sends it, a refused publish's
// among them. It connects through testnats.AwaitConnectSeed, which makes the connect again when a
// try times out, until the test binary's deadline less testwait's margin, so a handshake a loaded
// machine answers late costs the row time and not its connection; a refusal of the connect fails
// the row, which needs the connection.
func grantedConnection(t *testing.T, uri, seed string, options ...natsgo.Option) (*natsgo.Conn, <-chan error) {
	t.Helper()
	errs := make(chan error, 16)
	conn, err := testnats.AwaitConnectSeed(t, uri, seed, append(options, natsgo.ErrorHandler(func(_ *natsgo.Conn, _ *natsgo.Subscription, err error) {
		select {
		case errs <- err:
		default:
		}
	}))...)
	require.NoError(t, err)
	t.Cleanup(conn.Close)
	return conn, errs
}

// callContext is the context each NATS call of these rows takes in place of a default wait of
// nats.go's own: testwait.Context's, which ends at the test binary's deadline less testwait's
// margin, as clock.WithShownDeadline shows it. Under -timeout 0 testwait.Context reports no
// deadline, and nats.go then refuses a flush (nats.ErrNoDeadlineContext) and ends a JetStream call
// at a default wait of its own (a legacy context's wait, 5 s unless its maker set one; the
// jetstream package's DefaultTimeout), as bus ends a stored-message read at its own; shown a
// deadline that bounds nothing, each call waits for its answer there, as every wait does.
// nats.go's legacy KeyValue, CreateKeyValue and a bucket's Put and Get take no context, so these
// rows make their bucket calls through the jetstream package, whose calls take one.
func callContext(t *testing.T) context.Context {
	t.Helper()
	ctx, release := clock.WithShownDeadline(testwait.Context(t))
	t.Cleanup(release)
	return ctx
}

// refusedNaming waits for the server's permissions violation naming what, a fragment of its text
// such as `Publish to "x"`, among errs, passing over every other error, and fails the test at
// testwait's bound (testwait.Receive) when none comes.
func refusedNaming(t *testing.T, errs <-chan error, what string) {
	t.Helper()
	for {
		err := testwait.Receive(t, "the server's permissions violation naming "+what, errs)
		if errors.Is(err, natsgo.ErrPermissionViolation) && strings.Contains(err.Error(), what) {
			return
		}
	}
}

// refusedRequest runs request on a goroutine of its own with a context under callContext's, waits
// for the server's permissions violation naming what among errs (refusedNaming), then cancels the
// context and returns request's error. request makes a request the server refuses as a publish, and
// a refused publish gets no reply, so the violation, not a timeout of the request's own, is the event
// that ends it. A request the server accepted is sent no violation, however late its answer, so the
// wait for one fails and the request never reads as refused. request runs off the test goroutine,
// so it calls no method of t.
func refusedRequest(t *testing.T, errs <-chan error, what string, request func(ctx context.Context) error) error {
	t.Helper()
	ctx, cancel := context.WithCancel(callContext(t))
	defer cancel()
	returned := make(chan error, 1)
	go func() { returned <- request(ctx) }()
	refusedNaming(t, errs, what)
	cancel()
	return testwait.Receive(t, "the refused request to return once its context was cancelled", returned)
}

// present reports whether the stream name exists on admin, read within ctx, failing the test on any
// read error but a not-found, so a stream nothing could read never reads as gone.
func present(t *testing.T, ctx context.Context, admin natsgo.JetStreamContext, name string) bool {
	t.Helper()
	_, err := admin.StreamInfo(name, natsgo.Context(ctx))
	require.True(t, err == nil || errors.Is(err, natsgo.ErrStreamNotFound), "read %s: %v", name, err)
	return err == nil
}

// nextMsg waits for sub's next message until the test binary's deadline less testwait's margin
// (testwait.Context), and fails the test naming what, the message it waited for, when none comes.
func nextMsg(t *testing.T, sub *natsgo.Subscription, what string) *natsgo.Msg {
	t.Helper()
	msg, err := sub.NextMsgWithContext(testwait.Context(t))
	require.NoError(t, err, "gave up waiting for %s", what)
	return msg
}

// awaitGone waits through testwait.Until for the stream name to be gone from admin, and fails the
// test naming what, the fact that deletes it, with the stream's last reading. A read that fails but
// for a not-found fails the test at once (present), so a stream nothing could read never reads as
// gone.
func awaitGone(t *testing.T, admin natsgo.JetStreamContext, name, what string) {
	t.Helper()
	ctx := callContext(t)
	testwait.Until(t, what, func() (string, bool) {
		if present(t, ctx, admin, name) {
			return name + " present", false
		}
		return name + " gone", true
	})
}

// The edge's consumer calls reach past its publish list, and this row pins how far, on the hub's
// image: a narrowing of that reach fails it, and so does a widening of the publish list it reads
// against. On a stream with the production subjects, which a shared listener's user owns:
//   - a direct publish of the edge's on another project's role subject is refused, and a push
//     consumer the edge creates over a message it stored delivers that message on the same subject;
//   - a consumer the edge creates on another project's Dispatch event, which the shared user
//     stored, delivers that event to the edge's own inbox;
//   - a push consumer whose deliver subject the stream carries is refused as a cycle;
//   - the edge updates another listener's durable to deliver to its own inbox, and deletes it;
//   - a subscription to the relay subjects under deliver.> is admitted and delivered a subject of
//     two tokens past the prefix, where a subscription to that subject itself is refused.
func TestTheEdgeGrantsConsumerCallsReachPastItsPublishList(t *testing.T) {
	edgeSeed, edgeKey := testnats.User(t)
	sharedSeed, sharedKey := testnats.User(t)
	readerSeed, readerKey := testnats.User(t)
	edge, shared := edge(t, "m", "PRIV"), natsgrant.Listener("shared")
	_, uri := testnats.StartImageConfig(t, testnats.HubImage, testnats.GrantedUsersConfig(
		testnats.StaticUser{NKey: edgeKey, Publish: edge.Publish, Subscribe: edge.Subscribe},
		testnats.StaticUser{NKey: sharedKey, Publish: shared.Publish, Subscribe: shared.Subscribe},
		testnats.StaticUser{NKey: readerKey, Publish: []string{"example.unused"}, Subscribe: []string{">"}},
	), []string{"-js"}, false)
	ctx := callContext(t)
	owner, err := bus.ConnectOwningStream([]string{uri}, bus.WithName("shared"), bus.WithEnvironment(testnats.SeedOnly(sharedSeed)))
	require.NoError(t, err, "the shared listener's user creates the stream")
	t.Cleanup(owner.Close)
	const durable = "listener-shared"
	ownerJS, err := owner.Conn.JetStream(testnats.MaxWait(t))
	require.NoError(t, err)
	_, err = ownerJS.AddConsumer(bus.Stream, &natsgo.ConsumerConfig{
		Durable: durable, DeliverSubject: owner.Conn.NewInbox(), FilterSubjects: []string{"notifications.dispatch.>"}, AckPolicy: natsgo.AckExplicitPolicy,
	}, natsgo.Context(ctx))
	require.NoError(t, err, "the shared listener's user creates its durable")
	reader, _ := grantedConnection(t, uri, readerSeed)
	edgeConn, edgeErrs := grantedConnection(t, uri, edgeSeed, natsgo.CustomInboxPrefix(natsgrant.EdgeInboxPrefix("m")))
	edgeJS, err := edgeConn.JetStream(testnats.MaxWait(t))
	require.NoError(t, err)

	t.Run("a push consumer delivers on a subject the grant does not publish", func(t *testing.T) {
		const target, own = "notifications.role.INFRA.x", "notifications.agent.ses_edge_own"
		received, err := reader.SubscribeSync(target)
		require.NoError(t, err)
		require.NoError(t, reader.FlushWithContext(ctx))
		require.NoError(t, edgeConn.Publish(target, []byte("published directly")))
		require.NoError(t, edgeConn.FlushWithContext(ctx))
		refusedNaming(t, edgeErrs, `Publish to "`+target+`"`)
		_, err = edgeJS.Publish(own, []byte("stored by the edge"), natsgo.Context(ctx))
		require.NoError(t, err, "the edge stores a message on its own agent subject")
		_, err = edgeJS.AddConsumer(bus.Stream, &natsgo.ConsumerConfig{
			Name: "edge-reach", FilterSubject: own, DeliverSubject: target, AckPolicy: natsgo.AckNonePolicy,
		}, natsgo.Context(ctx))
		require.NoError(t, err, "the edge creates a push consumer delivering on %s", target)
		msg := nextMsg(t, received, "the edge's push consumer's delivery on "+target)
		require.Equal(t, "stored by the edge", string(msg.Data), "the reader received the edge's direct publish, which the server refused")
	})
	t.Run("it reads another project's stored Dispatch event", func(t *testing.T) {
		const event = "notifications.dispatch.issue.INFRA.INFRA-1.comment.created"
		_, err := ownerJS.Publish(event, []byte("another project's event"), natsgo.Context(ctx))
		require.NoError(t, err, "the shared user stores another project's Dispatch event")
		inbox := natsgrant.EdgeInboxPrefix("m") + ".read"
		read, err := edgeConn.SubscribeSync(inbox)
		require.NoError(t, err)
		require.NoError(t, edgeConn.FlushWithContext(ctx))
		_, err = edgeJS.AddConsumer(bus.Stream, &natsgo.ConsumerConfig{
			Name: "edge-read", FilterSubject: event, DeliverSubject: inbox, AckPolicy: natsgo.AckNonePolicy,
		}, natsgo.Context(ctx))
		require.NoError(t, err, "the edge creates a consumer on %s", event)
		msg := nextMsg(t, read, "the delivery of "+event+" to the edge's inbox by the edge's consumer")
		require.Equal(t, "another project's event", string(msg.Data))
	})
	t.Run("a deliver subject the stream carries is refused as a cycle", func(t *testing.T) {
		_, err := edgeJS.AddConsumer(bus.Stream, &natsgo.ConsumerConfig{
			Name: "edge-cycle", FilterSubject: "notifications.agent.ses_edge_own", DeliverSubject: "notifications.dispatch.issue.INFRA.INFRA-1.comment.created", AckPolicy: natsgo.AckNonePolicy,
		}, natsgo.Context(ctx))
		require.Error(t, err)
		require.Contains(t, err.Error(), "cycle")
	})
	t.Run("it updates and deletes another listener's durable", func(t *testing.T) {
		stolen := natsgrant.EdgeInboxPrefix("m") + ".stolen"
		_, err := edgeJS.UpdateConsumer(bus.Stream, &natsgo.ConsumerConfig{
			Durable: durable, DeliverSubject: stolen, FilterSubjects: []string{"notifications.dispatch.>"}, AckPolicy: natsgo.AckExplicitPolicy,
		}, natsgo.Context(ctx))
		require.NoError(t, err, "the edge updates %s", durable)
		info, err := ownerJS.ConsumerInfo(bus.Stream, durable, natsgo.Context(ctx))
		require.NoError(t, err)
		require.Equal(t, stolen, info.Config.DeliverSubject, "%s delivers to the edge's inbox", durable)
		require.NoError(t, edgeJS.DeleteConsumer(bus.Stream, durable, natsgo.Context(ctx)), "the edge deletes %s", durable)
		_, err = ownerJS.ConsumerInfo(bus.Stream, durable, natsgo.Context(ctx))
		require.ErrorIs(t, err, natsgo.ErrConsumerNotFound, "%s is still there", durable)
	})
	t.Run("a subscription under deliver.> is admitted and gets two tokens past the prefix", func(t *testing.T) {
		const twoTokens = "notifications.envoy.deliver.x.y"
		wide, err := edgeConn.SubscribeSync(bus.DeliverSubjectPrefix + ">")
		require.NoError(t, err)
		_, err = edgeConn.SubscribeSync(twoTokens)
		require.NoError(t, err)
		require.NoError(t, edgeConn.FlushWithContext(ctx))
		refusedNaming(t, edgeErrs, `Subscription to "`+twoTokens+`"`)
		require.NoError(t, owner.Conn.Publish(twoTokens, []byte("two tokens")))
		require.NoError(t, owner.Conn.FlushWithContext(ctx))
		msg := nextMsg(t, wide, "a delivery to the edge's subscription under deliver.>")
		require.Equal(t, twoTokens, msg.Subject)
	})
}

// One edge machine answers another machine's relay frame and reads only its own receipts
// (natsgrant.EdgeRelayReplyPrefix), on the hub's image: edge-a publishes a frame on a session's
// relay subject with a reply subject of its own family, as the machine holding the session's
// consumer does; edge-b, serving the session's relay socket, reads the frame and answers it, and
// edge-a receives the answer. edge-b's subscription to edge-a's receipt family is refused naming
// the family, and edge-b reads its own family's receipt, the control that a subscription edge-b
// holds is delivered.
func TestAnEdgeMachineAnswersAnotherMachinesFrameAndReadsOnlyItsOwnReceipts(t *testing.T) {
	aSeed, aKey := testnats.User(t)
	bSeed, bKey := testnats.User(t)
	a, b := edge(t, "edge-a", "PRIV"), edge(t, "edge-b", "PRIV")
	_, uri := testnats.StartImageConfig(t, testnats.HubImage, testnats.GrantedUsersConfig(
		testnats.StaticUser{NKey: aKey, Publish: a.Publish, Subscribe: a.Subscribe},
		testnats.StaticUser{NKey: bKey, Publish: b.Publish, Subscribe: b.Subscribe},
	), []string{"-js"}, false)
	ctx := callContext(t)
	aConn, _ := grantedConnection(t, uri, aSeed)
	bConn, bErrs := grantedConnection(t, uri, bSeed)
	familyOf := func(machine string) string { return natsgrant.EdgeRelayReplyPrefix(machine) + ".>" }
	receiptOf := func(machine string) string { return natsgrant.EdgeRelayReplyPrefix(machine) + ".n1.t1" }
	const session = "notifications.envoy.deliver.ses_s"
	receipts, err := aConn.SubscribeSync(familyOf("edge-a"))
	require.NoError(t, err)
	ownOfB, err := bConn.SubscribeSync(familyOf("edge-b"))
	require.NoError(t, err)
	frames, err := bConn.SubscribeSync(session)
	require.NoError(t, err)
	require.NoError(t, aConn.FlushWithContext(ctx))
	require.NoError(t, bConn.FlushWithContext(ctx))

	require.NoError(t, aConn.PublishRequest(session, receiptOf("edge-a"), []byte("a frame")))
	require.NoError(t, aConn.FlushWithContext(ctx))
	frame := nextMsg(t, frames, "edge-b's read of the frame on "+session)
	require.Equal(t, receiptOf("edge-a"), frame.Reply)
	require.NoError(t, frame.Respond([]byte("done")))
	require.NoError(t, bConn.FlushWithContext(ctx))
	answer := nextMsg(t, receipts, "edge-b's answer at edge-a on "+receiptOf("edge-a"))
	require.Equal(t, "done", string(answer.Data))

	_, err = bConn.SubscribeSync(familyOf("edge-a"))
	require.NoError(t, err)
	require.NoError(t, bConn.FlushWithContext(ctx))
	refusedNaming(t, bErrs, `Subscription to "`+familyOf("edge-a")+`"`)
	require.NoError(t, bConn.Publish(receiptOf("edge-b"), []byte("edge-b's own")))
	require.NoError(t, bConn.FlushWithContext(ctx))
	own := nextMsg(t, ownOfB, "edge-b's read of its own receipt")
	require.Equal(t, "edge-b's own", string(own.Data))
}

// The update of a bucket's stream can change its subjects and set a republish, and a republish to
// a $JS.API. subject is a JetStream API request in the stream's account, with the stored value as
// its body. So the edge holds no bucket stream's create or update. On the hub's image:
//   - the edge's update of the interest bucket's stream with a republish to
//     $JS.API.STREAM.DELETE.SCRATCH_VICTIM is refused naming the update, and leaves the stream
//     with no republish, so the edge's write of an empty value to the key the republish would copy
//     leaves SCRATCH_VICTIM as it was;
//   - in the same run, the account's administrator sets such a republish on two other buckets'
//     streams, to $JS.API.STREAM.DELETE.SCRATCH_VICTIM_ADMIN and, carrying headers only, to
//     $JS.API.STREAM.DELETE.SCRATCH_VICTIM_HEADERS, and writes the key: an empty value to the
//     first, a non-empty one to the second, whose copy carries no body. A client subscribed to
//     each subject receives the copy, and both streams are deleted, so the route the edge is
//     refused is live. (A non-empty body is refused by the API as a delete request's.)
func TestTheEdgeCannotSetABucketToRepublishIntoTheJetStreamAPI(t *testing.T) {
	edgeSeed, edgeKey := testnats.User(t)
	adminSeed, adminKey := testnats.User(t)
	edge := edge(t, "m", "PRIV")
	_, uri := testnats.StartImageConfig(t, testnats.HubImage, testnats.GrantedUsersConfig(
		testnats.StaticUser{NKey: edgeKey, Publish: edge.Publish, Subscribe: edge.Subscribe},
		testnats.StaticUser{NKey: adminKey, Publish: []string{">"}, Subscribe: []string{">"}},
	), []string{"-js"}, false)
	ctx := callContext(t)
	adminConn, _ := grantedConnection(t, uri, adminSeed)
	admin, err := adminConn.JetStream(testnats.MaxWait(t))
	require.NoError(t, err)
	adminStreams, err := jetstream.New(adminConn)
	require.NoError(t, err)
	for _, name := range []string{"SCRATCH_VICTIM", "SCRATCH_VICTIM_ADMIN", "SCRATCH_VICTIM_HEADERS"} {
		_, err := admin.AddStream(&natsgo.StreamConfig{Name: name, Subjects: []string{"scratch." + strings.ToLower(name) + ".>"}, Storage: natsgo.MemoryStorage}, natsgo.Context(ctx))
		require.NoError(t, err)
	}
	for _, bucket := range []string{bus.InterestBucket, bus.LegionAgentBucket, bus.CIStateBucket} {
		_, err := adminStreams.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bucket, Storage: jetstream.MemoryStorage})
		require.NoError(t, err)
	}
	republishInto := func(js natsgo.JetStreamContext, bucket, victim string, headersOnly bool, opts ...natsgo.JSOpt) error {
		info, err := js.StreamInfo("KV_"+bucket, opts...)
		if err != nil {
			return err
		}
		config := info.Config
		config.RePublish = &natsgo.RePublish{Source: "$KV." + bucket + ".rp", Destination: "$JS.API.STREAM.DELETE." + victim, HeadersOnly: headersOnly}
		_, err = js.UpdateStream(&config, opts...)
		return err
	}
	edgeConn, edgeErrs := grantedConnection(t, uri, edgeSeed, natsgo.CustomInboxPrefix(natsgrant.EdgeInboxPrefix("m")))
	edgeJS, err := edgeConn.JetStream(testnats.MaxWait(t))
	require.NoError(t, err)

	err = refusedRequest(t, edgeErrs, `Publish to "$JS.API.STREAM.UPDATE.KV_`+bus.InterestBucket+`"`, func(ctx context.Context) error {
		return republishInto(edgeJS, bus.InterestBucket, "SCRATCH_VICTIM", false, natsgo.Context(ctx))
	})
	require.Error(t, err, "the edge set a republish on KV_%s", bus.InterestBucket)
	info, err := admin.StreamInfo("KV_"+bus.InterestBucket, natsgo.Context(ctx))
	require.NoError(t, err)
	require.Nil(t, info.Config.RePublish, "KV_%s carries a republish after the edge's refused update", bus.InterestBucket)
	edgeStreams, err := jetstream.New(edgeConn)
	require.NoError(t, err)
	interests, err := edgeStreams.KeyValue(ctx, bus.InterestBucket)
	require.NoError(t, err)
	_, err = interests.Put(ctx, "rp", nil)
	require.NoError(t, err, "the edge writes an empty value to a key of the interest bucket")

	for _, control := range []struct {
		bucket, victim string
		headersOnly    bool
		value          []byte
	}{
		{bucket: bus.LegionAgentBucket, victim: "SCRATCH_VICTIM_ADMIN", value: nil},
		{bucket: bus.CIStateBucket, victim: "SCRATCH_VICTIM_HEADERS", headersOnly: true, value: []byte("written by the administrator")},
	} {
		target := "$JS.API.STREAM.DELETE." + control.victim
		republished, err := adminConn.SubscribeSync(target)
		require.NoError(t, err)
		require.NoError(t, adminConn.FlushWithContext(ctx))
		require.NoError(t, republishInto(admin, control.bucket, control.victim, control.headersOnly, natsgo.Context(ctx)), "the administrator sets a republish on KV_%s", control.bucket)
		bucket, err := adminStreams.KeyValue(ctx, control.bucket)
		require.NoError(t, err)
		_, err = bucket.Put(ctx, "rp", control.value)
		require.NoError(t, err)
		copied := nextMsg(t, republished, "the administrator's republish to "+target+" at its subscriber, without which the row cannot see it fire")
		require.Empty(t, copied.Data, "the republished copy on %s carries a body", target)
		awaitGone(t, admin, control.victim, control.victim+" deleted by the administrator's republish of an empty body to its delete")
	}
	require.True(t, present(t, ctx, admin, "SCRATCH_VICTIM"), "SCRATCH_VICTIM, whose delete the edge could set no republish to")
}

// A request's reply subject is checked against no permission of the requester, and the stream
// itself answers a direct get ($JS.API.DIRECT.GET.KV_<bucket>.>), through the account's own
// JetStream client, publishing the stored value as the body on that reply subject. Aimed at
// $JS.API., the JetStream API takes that value as a request of the account's. So the edge holds no
// direct get, and reads its buckets with stream message gets (bus.WithStreamMessageReads). This
// row pins both halves on the hub's image (nats-server 2.15.1):
//   - the route is live: a user holding Listener's grant, which keeps the direct get, writes a
//     stream update of SCRATCH_DG_CONTROL (max_msgs 7) and an empty value to the interest bucket,
//     and its direct gets of them, answered on that stream's update and on SCRATCH_DG_GONE's
//     delete, change the one and delete the other;
//   - the edge's direct gets of the same keys, answered on SCRATCH_DG's update and
//     SCRATCH_DG_DELETE's delete, are refused naming the direct get, and both streams stay;
//   - what the edge does call answers there and takes no action: its stream message get of the
//     empty key answered on SCRATCH_DG_MSG_GET's delete, its stream info answered on
//     SCRATCH_DG_INFO's purge, and the acknowledgement of a key it writes, answered on
//     SCRATCH_DG_PUBACK's purge, each reach a subscriber of that subject and leave the stream.
//
// The edge's probes are made before the control's, and the streams they aimed at are read after
// the control's changes are seen.
func TestTheEdgeHoldsNoDirectGetWhoseAnswerReachesTheJetStreamAPI(t *testing.T) {
	edgeSeed, edgeKey := testnats.User(t)
	listenerSeed, listenerKey := testnats.User(t)
	adminSeed, adminKey := testnats.User(t)
	edge, listener := edge(t, "m", "PRIV"), natsgrant.Listener("shared")
	_, uri := testnats.StartImageConfig(t, testnats.HubImage, testnats.GrantedUsersConfig(
		testnats.StaticUser{NKey: edgeKey, Publish: edge.Publish, Subscribe: edge.Subscribe},
		testnats.StaticUser{NKey: listenerKey, Publish: listener.Publish, Subscribe: listener.Subscribe},
		testnats.StaticUser{NKey: adminKey, Publish: []string{">"}, Subscribe: []string{">"}},
	), []string{"-js"}, false)
	ctx := callContext(t)
	adminConn, _ := grantedConnection(t, uri, adminSeed)
	admin, err := adminConn.JetStream(testnats.MaxWait(t))
	require.NoError(t, err)
	adminStreams, err := jetstream.New(adminConn)
	require.NoError(t, err)
	streams := []string{"SCRATCH_DG", "SCRATCH_DG_DELETE", "SCRATCH_DG_MSG_GET", "SCRATCH_DG_INFO", "SCRATCH_DG_PUBACK"}
	for _, name := range append([]string{"SCRATCH_DG_CONTROL", "SCRATCH_DG_GONE"}, streams...) {
		_, err := admin.AddStream(&natsgo.StreamConfig{Name: name, Subjects: []string{"scratch." + strings.ToLower(name) + ".>"}, Storage: natsgo.MemoryStorage}, natsgo.Context(ctx))
		require.NoError(t, err)
		_, err = admin.Publish("scratch."+strings.ToLower(name)+".m", []byte("kept"), natsgo.Context(ctx))
		require.NoError(t, err)
	}
	_, err = adminStreams.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: bus.InterestBucket, Storage: jetstream.MemoryStorage})
	require.NoError(t, err)
	// state is a stream's state as this row reads it: gone, or present with its max_msgs and its
	// message count.
	state := func(name string) string {
		info, err := admin.StreamInfo(name, natsgo.Context(ctx))
		if errors.Is(err, natsgo.ErrStreamNotFound) {
			return "gone"
		}
		require.NoError(t, err, "read %s", name)
		return fmt.Sprintf("present, max_msgs %d, %d messages", info.Config.MaxMsgs, info.State.Msgs)
	}
	update := func(name string) []byte {
		info, err := admin.StreamInfo(name, natsgo.Context(ctx))
		require.NoError(t, err)
		config := info.Config
		config.MaxMsgs = 7
		body, err := json.Marshal(config)
		require.NoError(t, err)
		return body
	}
	const untouched = "present, max_msgs -1, 1 messages"
	// answered publishes request with reply as its reply subject from conn, and requires a
	// subscriber of reply to receive the answer, so the answer was published there.
	answered := func(conn *natsgo.Conn, request, reply string, body []byte) {
		t.Helper()
		answers, err := adminConn.SubscribeSync(reply)
		require.NoError(t, err)
		require.NoError(t, adminConn.FlushWithContext(ctx))
		require.NoError(t, conn.PublishRequest(request, reply, body))
		require.NoError(t, conn.FlushWithContext(ctx))
		nextMsg(t, answers, "the answer to "+request+" at a subscriber of "+reply)
		require.NoError(t, answers.Unsubscribe())
	}
	directGet := "$JS.API.DIRECT.GET.KV_" + bus.InterestBucket + ".$KV." + bus.InterestBucket + "."

	listenerConn, _ := grantedConnection(t, uri, listenerSeed)
	listenerStreams, err := jetstream.New(listenerConn)
	require.NoError(t, err)
	interests, err := listenerStreams.KeyValue(ctx, bus.InterestBucket)
	require.NoError(t, err)
	for key, value := range map[string][]byte{"dg-update": update("SCRATCH_DG"), "dg-control": update("SCRATCH_DG_CONTROL"), "dg-empty": nil} {
		_, err := interests.Put(ctx, key, value)
		require.NoError(t, err)
	}

	edgeConn, edgeErrs := grantedConnection(t, uri, edgeSeed, natsgo.CustomInboxPrefix(natsgrant.EdgeInboxPrefix("m")))
	for request, reply := range map[string]string{directGet + "dg-update": "$JS.API.STREAM.UPDATE.SCRATCH_DG", directGet + "dg-empty": "$JS.API.STREAM.DELETE.SCRATCH_DG_DELETE"} {
		require.NoError(t, edgeConn.PublishRequest(request, reply, nil))
		require.NoError(t, edgeConn.FlushWithContext(ctx))
		refusedNaming(t, edgeErrs, `Publish to "`+request+`"`)
	}
	messageGet, err := json.Marshal(map[string]string{"last_by_subj": "$KV." + bus.InterestBucket + ".dg-empty"})
	require.NoError(t, err)
	answered(edgeConn, "$JS.API.STREAM.MSG.GET.KV_"+bus.InterestBucket, "$JS.API.STREAM.DELETE.SCRATCH_DG_MSG_GET", messageGet)
	answered(edgeConn, "$JS.API.STREAM.INFO.KV_"+bus.InterestBucket, "$JS.API.STREAM.PURGE.SCRATCH_DG_INFO", nil)
	answered(edgeConn, "$KV."+bus.InterestBucket+".dg-puback", "$JS.API.STREAM.PURGE.SCRATCH_DG_PUBACK", nil)

	answered(listenerConn, directGet+"dg-control", "$JS.API.STREAM.UPDATE.SCRATCH_DG_CONTROL", nil)
	answered(listenerConn, directGet+"dg-empty", "$JS.API.STREAM.DELETE.SCRATCH_DG_GONE", nil)
	control := func() string { return state("SCRATCH_DG_CONTROL") + "; " + state("SCRATCH_DG_GONE") }
	const changed = "present, max_msgs 7, 1 messages; gone"
	testwait.Until(t, "SCRATCH_DG_CONTROL and SCRATCH_DG_GONE to read "+changed+" after Listener's direct gets answered on the update of the one and the delete of the other", func() (string, bool) {
		seen := control()
		return seen, seen == changed
	})

	var states []string
	want := make([]string, len(streams))
	for i, name := range streams {
		states = append(states, name+" "+state(name))
		want[i] = name + " " + untouched
	}
	require.Equal(t, strings.Join(want, "; "), strings.Join(states, "; "),
		"after the edge's refused direct gets, and its stream message get, stream info and key write answered on the JetStream API")
}

// accountUserEntry is one nkey user of an account in a server configuration, held to its allow
// lists, as a hub renders a static user.
func accountUserEntry(user testnats.StaticUser) string {
	quoted := func(entries []string) string {
		out := make([]string, len(entries))
		for i, entry := range entries {
			out[i] = strconv.Quote(entry)
		}
		return strings.Join(out, ", ")
	}
	return fmt.Sprintf("{ nkey: %q, permissions: { publish: { allow: [%s] }, subscribe: { allow: [%s] } } }", user.NKey, quoted(user.Publish), quoted(user.Subscribe))
}

// twoAccountsConfig is a server configuration of two JetStream accounts, A and B, holding the
// users given, and no other client.
func twoAccountsConfig(a, b []testnats.StaticUser) string {
	entries := func(users []testnats.StaticUser) string {
		out := make([]string, len(users))
		for i, user := range users {
			out[i] = accountUserEntry(user)
		}
		return strings.Join(out, ",\n      ")
	}
	return fmt.Sprintf("jetstream {}\naccounts {\n  A: {\n    jetstream: enabled\n    users: [\n      %s\n    ]\n  }\n  B: {\n    jetstream: enabled\n    users: [\n      %s\n    ]\n  }\n}\n", entries(a), entries(b))
}

// A push consumer's deliver subject is checked against no permission, and the stream delivers
// through its account's own JetStream client, so the edge can aim a push consumer at a subject
// under $JS.API. or $KV. or at another stream's subjects. This row pins, on the hub's image
// (nats-server 2.15.1), what such a consumer of the edge's does to a scratch stream or bucket of
// its account, over a message the edge stored on its own agent subject:
//   - a deliver subject no client subscribes gives the consumer no interest: it is not push bound
//     and delivers nothing, the JetStream API's import and a stream's capture being no interest;
//   - with a client also subscribed to it, the consumer delivers, and the client receives the
//     delivery under the message's stream subject, not the deliver subject. So the JetStream API
//     takes no action: a stream delete, purge or update and a bucket's stream delete leave the
//     stream as it was;
//   - a stream whose subjects take the deliver subject stores the delivery as a message of its
//     own: another scratch stream, and a bucket's stream, whose key the delivery leaves unwritten
//     while the stream counts one message more. That is the one write this route has.
//
// Controls in the same run: a subscribed subject receives the delivery; the edge's direct request
// of a stream delete is refused and the stream stays; the account's administrator deleting a
// stream is seen as deleted; and a stream of the same name in another account stays. A key the
// row cannot read reads as unread, never as absent (keyState).
func TestTheEdgeGrantsPushConsumerReachesItsAccountsJetStreamAPI(t *testing.T) {
	edgeSeed, edgeKey := testnats.User(t)
	sharedSeed, sharedKey := testnats.User(t)
	adminSeed, adminKey := testnats.User(t)
	otherSeed, otherKey := testnats.User(t)
	edge, shared := edge(t, "m", "PRIV"), natsgrant.Listener("shared")
	everything := []string{">"}
	_, uri := testnats.StartImageConfig(t, testnats.HubImage, twoAccountsConfig(
		[]testnats.StaticUser{
			{NKey: edgeKey, Publish: edge.Publish, Subscribe: edge.Subscribe},
			{NKey: sharedKey, Publish: shared.Publish, Subscribe: shared.Subscribe},
			{NKey: adminKey, Publish: everything, Subscribe: everything},
		},
		[]testnats.StaticUser{{NKey: otherKey, Publish: everything, Subscribe: everything}},
	), []string{"-js"}, false)
	ctx := callContext(t)
	owner, err := bus.ConnectOwningStream([]string{uri}, bus.WithName("shared"), bus.WithEnvironment(testnats.SeedOnly(sharedSeed)))
	require.NoError(t, err, "the shared listener's user creates the stream")
	t.Cleanup(owner.Close)
	adminConn, _ := grantedConnection(t, uri, adminSeed)
	admin, err := adminConn.JetStream(testnats.MaxWait(t))
	require.NoError(t, err)
	adminStreams, err := jetstream.New(adminConn)
	require.NoError(t, err)
	otherConn, _ := grantedConnection(t, uri, otherSeed)
	other, err := otherConn.JetStream(testnats.MaxWait(t))
	require.NoError(t, err)
	scratch := func(js natsgo.JetStreamContext, name string) {
		t.Helper()
		_, err := js.AddStream(&natsgo.StreamConfig{Name: name, Subjects: []string{"scratch." + strings.ToLower(name) + ".>"}, Storage: natsgo.MemoryStorage}, natsgo.Context(ctx))
		require.NoError(t, err, "make the scratch stream %s", name)
		_, err = js.Publish("scratch."+strings.ToLower(name)+".m", []byte("kept"), natsgo.Context(ctx))
		require.NoError(t, err)
	}
	for _, name := range []string{"SCRATCH_DELETE", "SCRATCH_DELETE_SUB", "SCRATCH_PURGE", "SCRATCH_UPDATE", "SCRATCH_CAPTURE", "SCRATCH_CAPTURE_SUB", "SCRATCH_CONTROL", "SCRATCH_ADMIN"} {
		scratch(admin, name)
	}
	scratch(other, "SCRATCH_DELETE")
	_, err = adminStreams.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "scratch_kv_delete", Storage: jetstream.MemoryStorage})
	require.NoError(t, err)
	written, err := adminStreams.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: "scratch_kv_write", Storage: jetstream.MemoryStorage})
	require.NoError(t, err)
	update, err := admin.StreamInfo("SCRATCH_UPDATE", natsgo.Context(ctx))
	require.NoError(t, err)
	changed := update.Config
	changed.MaxMsgs = 7
	updateBody, err := json.Marshal(changed)
	require.NoError(t, err)
	streamState := func(name string) string {
		info, err := admin.StreamInfo(name, natsgo.Context(ctx))
		switch {
		case errors.Is(err, natsgo.ErrStreamNotFound):
			return "deleted"
		case err != nil:
			return "unread: " + err.Error()
		}
		return fmt.Sprintf("kept, %d messages, max_msgs %d", info.State.Msgs, info.Config.MaxMsgs)
	}
	// keyState is a bucket key's state: its entry, "no entry" when the bucket answers that it holds
	// none, and the read's error otherwise, so a key nothing could read never reads as absent.
	keyState := func(bucket jetstream.KeyValue, key string) string {
		entry, err := bucket.Get(ctx, key)
		switch {
		case errors.Is(err, jetstream.ErrKeyNotFound):
			return "no entry"
		case err != nil:
			return "unread: " + err.Error()
		}
		return fmt.Sprintf("entry %q", entry.Value())
	}
	var received atomic.Int64
	const kept, keptEmpty = "kept, 1 messages, max_msgs -1", "kept, 0 messages, max_msgs -1"
	edgeConn, edgeErrs := grantedConnection(t, uri, edgeSeed, natsgo.CustomInboxPrefix(natsgrant.EdgeInboxPrefix("m")))
	edgeJS, err := edgeConn.JetStream(testnats.MaxWait(t))
	require.NoError(t, err)

	for i, delivery := range []struct {
		target, payload string
		// subscribed: a client subscribes the deliver subject, which gives the consumer interest.
		subscribed bool
		state      func() string
		want       string
	}{
		{target: "$JS.API.STREAM.DELETE.SCRATCH_DELETE", state: func() string { return streamState("SCRATCH_DELETE") }, want: kept},
		{target: "scratch.scratch_capture.m", payload: "captured", state: func() string { return streamState("SCRATCH_CAPTURE") }, want: kept},
		{target: "$JS.API.STREAM.DELETE.SCRATCH_DELETE_SUB", subscribed: true, state: func() string { return streamState("SCRATCH_DELETE_SUB") }, want: kept},
		{target: "$JS.API.STREAM.PURGE.SCRATCH_PURGE", payload: "{}", subscribed: true, state: func() string { return streamState("SCRATCH_PURGE") }, want: kept},
		{target: "$JS.API.STREAM.UPDATE.SCRATCH_UPDATE", payload: string(updateBody), subscribed: true, state: func() string { return streamState("SCRATCH_UPDATE") }, want: kept},
		{target: "$JS.API.STREAM.DELETE.KV_scratch_kv_delete", subscribed: true, state: func() string { return streamState("KV_scratch_kv_delete") }, want: keptEmpty},
		{target: "$KV.scratch_kv_write.k1", payload: "written by the edge", subscribed: true, state: func() string {
			return keyState(written, "k1") + "; " + streamState("KV_scratch_kv_write")
		}, want: "no entry; kept, 1 messages, max_msgs -1"},
		{target: "scratch.scratch_capture_sub.m", payload: "captured", subscribed: true, state: func() string { return streamState("SCRATCH_CAPTURE_SUB") }, want: "kept, 2 messages, max_msgs -1"},
		{target: "scratch.subscribed", payload: "to a subscriber", subscribed: true, state: func() string { return fmt.Sprintf("%d received", received.Load()) }, want: "1 received"},
	} {
		t.Run(delivery.target, func(t *testing.T) {
			// subjects are the subjects the client subscribed to the deliver subject receives the
			// delivery under: the message's stream subject, never the deliver subject, which is why
			// an internal subscriber of the deliver subject, the JetStream API's import among them,
			// reads no request of its own in it.
			subjects := make(chan string, 1)
			if delivery.subscribed {
				sub, err := adminConn.Subscribe(delivery.target, func(msg *natsgo.Msg) {
					if delivery.target == "scratch.subscribed" {
						received.Add(1)
					}
					select {
					case subjects <- msg.Subject:
					default:
					}
				})
				require.NoError(t, err)
				t.Cleanup(func() { _ = sub.Unsubscribe() })
				require.NoError(t, adminConn.FlushWithContext(ctx))
			}
			before := delivery.state()
			own := fmt.Sprintf("notifications.agent.ses_edge_api_%d", i)
			_, err := edgeJS.Publish(own, []byte(delivery.payload), natsgo.Context(ctx))
			require.NoError(t, err, "the edge stores the message on its own agent subject")
			name := fmt.Sprintf("edge-api-%d", i)
			_, err = edgeJS.AddConsumer(bus.Stream, &natsgo.ConsumerConfig{Name: name, FilterSubject: own, DeliverSubject: delivery.target, AckPolicy: natsgo.AckNonePolicy}, natsgo.Context(ctx))
			require.NoError(t, err, "the edge creates a push consumer delivering on %s", delivery.target)
			if !delivery.subscribed {
				info, err := admin.ConsumerInfo(bus.Stream, name, natsgo.Context(ctx))
				require.NoError(t, err)
				require.False(t, info.PushBound, "the consumer on %s has interest no client gave it", delivery.target)
				require.Zero(t, info.Delivered.Consumer, "the consumer delivered on %s with no interest", delivery.target)
				require.Equal(t, delivery.want, delivery.state())
				return
			}
			testwait.Until(t, fmt.Sprintf("a delivery of the consumer on %s, which a client subscribes", delivery.target), func() (string, bool) {
				info, err := admin.ConsumerInfo(bus.Stream, name, natsgo.Context(ctx))
				if err != nil {
					return "unread: " + err.Error(), false
				}
				return fmt.Sprintf("%d delivered", info.Delivered.Consumer), info.Delivered.Consumer >= 1
			})
			subject := testwait.Receive(t, "the delivery at the client subscribed to "+delivery.target, subjects)
			require.Equal(t, own, subject, "the delivery on %s came under %s, not its stream subject", delivery.target, subject)
			// The subject above is why an API path takes no action; no clock decides it. A case
			// whose stream stores the delivery waits for that store, which lands just behind the
			// client's copy, and every other case reads its state once.
			after := testwait.Until(t, fmt.Sprintf("the store of the edge's delivery on %s, which moves its state from %s", delivery.target, before), func() (string, bool) {
				after := delivery.state()
				return after, before == delivery.want || after != before
			})
			require.Equal(t, delivery.want, after, "%s after the edge's delivery (before: %s)", delivery.target, before)
		})
	}

	t.Run("controls", func(t *testing.T) {
		err := refusedRequest(t, edgeErrs, `Publish to "$JS.API.STREAM.DELETE.SCRATCH_CONTROL"`, func(ctx context.Context) error {
			_, err := edgeConn.RequestWithContext(ctx, "$JS.API.STREAM.DELETE.SCRATCH_CONTROL", nil)
			return err
		})
		require.Error(t, err, "the edge's direct request of a stream delete was answered")
		require.Equal(t, kept, streamState("SCRATCH_CONTROL"), "the edge's direct request changed SCRATCH_CONTROL")
		require.NoError(t, admin.DeleteStream("SCRATCH_ADMIN", natsgo.Context(ctx)))
		require.Equal(t, "deleted", streamState("SCRATCH_ADMIN"), "the probe does not see a stream the administrator deleted")
		info, err := other.StreamInfo("SCRATCH_DELETE", natsgo.Context(ctx))
		require.NoError(t, err, "account B's SCRATCH_DELETE is gone")
		require.Equal(t, uint64(1), info.State.Msgs, "account B's SCRATCH_DELETE changed")
	})
}

// A push consumer with an idle heartbeat sends the heartbeat, an empty status message with no
// stored subject, to its deliver subject through the stream's own client, which is how a direct
// get's answer travels and not how a delivery does. On the hub's image, with a client subscribed
// to $JS.API.STREAM.DELETE.SCRATCH_HB, so the consumer has interest, a consumer the edge creates
// over a subject that holds no message, delivering there with a heartbeat, sends heartbeats the
// client receives under the deliver subject itself with an empty body, and SCRATCH_HB is deleted.
// The control in the same run is a consumer the edge creates the same way on
// SCRATCH_HB_UNSUBSCRIBED's delete, which no client subscribes: it is not push bound and that
// stream stays. The edge's grant subscribes no $JS.API. subject, and no Envoy client subscribes
// one, so the route needs a subscriber outside them.
func TestAPushConsumersHeartbeatOnTheAPIDeletesAStreamWhileAClientSubscribesIt(t *testing.T) {
	edgeSeed, edgeKey := testnats.User(t)
	sharedSeed, sharedKey := testnats.User(t)
	adminSeed, adminKey := testnats.User(t)
	edge, shared := edge(t, "m", "PRIV"), natsgrant.Listener("shared")
	_, uri := testnats.StartImageConfig(t, testnats.HubImage, testnats.GrantedUsersConfig(
		testnats.StaticUser{NKey: edgeKey, Publish: edge.Publish, Subscribe: edge.Subscribe},
		testnats.StaticUser{NKey: sharedKey, Publish: shared.Publish, Subscribe: shared.Subscribe},
		testnats.StaticUser{NKey: adminKey, Publish: []string{">"}, Subscribe: []string{">"}},
	), []string{"-js"}, false)
	ctx := callContext(t)
	owner, err := bus.ConnectOwningStream([]string{uri}, bus.WithName("shared"), bus.WithEnvironment(testnats.SeedOnly(sharedSeed)))
	require.NoError(t, err, "the shared listener's user creates the stream")
	t.Cleanup(owner.Close)
	adminConn, _ := grantedConnection(t, uri, adminSeed)
	admin, err := adminConn.JetStream(testnats.MaxWait(t))
	require.NoError(t, err)
	for _, name := range []string{"SCRATCH_HB", "SCRATCH_HB_UNSUBSCRIBED"} {
		_, err := admin.AddStream(&natsgo.StreamConfig{Name: name, Subjects: []string{"scratch." + strings.ToLower(name) + ".>"}, Storage: natsgo.MemoryStorage}, natsgo.Context(ctx))
		require.NoError(t, err)
	}
	const target = "$JS.API.STREAM.DELETE.SCRATCH_HB"
	heartbeats, err := adminConn.SubscribeSync(target)
	require.NoError(t, err)
	require.NoError(t, adminConn.FlushWithContext(ctx))

	edgeConn, _ := grantedConnection(t, uri, edgeSeed, natsgo.CustomInboxPrefix(natsgrant.EdgeInboxPrefix("m")))
	edgeJS, err := edgeConn.JetStream(testnats.MaxWait(t))
	require.NoError(t, err)
	// heartbeat is the shortest idle heartbeat nats-server takes: it refuses a push consumer's under
	// 100 ms (NewJSConsumerSmallHeartbeatError). A consumer with nothing to deliver sends its first
	// heartbeat that long after it starts.
	const heartbeat = 100 * time.Millisecond
	for name, deliver := range map[string]string{"edge-hb": target, "edge-hb-unsubscribed": "$JS.API.STREAM.DELETE.SCRATCH_HB_UNSUBSCRIBED"} {
		_, err := edgeJS.AddConsumer(bus.Stream, &natsgo.ConsumerConfig{Name: name, FilterSubject: "notifications.agent.ses_edge_hb_" + name, DeliverSubject: deliver, AckPolicy: natsgo.AckNonePolicy, Heartbeat: heartbeat}, natsgo.Context(ctx))
		require.NoError(t, err, "the edge creates a push consumer with a heartbeat delivering on %s", deliver)
	}

	beat := nextMsg(t, heartbeats, "a heartbeat at the client subscribed to "+target)
	require.Equal(t, target, beat.Subject, "the heartbeat came under %s", beat.Subject)
	require.Empty(t, beat.Data, "the heartbeat's body")
	require.Equal(t, "100", beat.Header.Get("Status"), "the heartbeat's status")
	awaitGone(t, admin, "SCRATCH_HB", "SCRATCH_HB deleted by a heartbeat that reached its delete subject")

	info, err := admin.ConsumerInfo(bus.Stream, "edge-hb-unsubscribed", natsgo.Context(ctx))
	require.NoError(t, err)
	require.False(t, info.PushBound, "the consumer on SCRATCH_HB_UNSUBSCRIBED's delete has interest no client gave it")
	require.True(t, present(t, ctx, admin, "SCRATCH_HB_UNSUBSCRIBED"), "SCRATCH_HB_UNSUBSCRIBED, whose delete no client subscribes")
}

// The consumer calls the edge keeps answer two requests with no body on whatever reply subject the
// requester names, each needing no subscriber: a no_wait pull on a consumer with nothing to deliver
// (CONSUMER.MSG.NEXT.ENVOY_NOTIFICATIONS.*) is answered a "404 No Messages" status, and a
// synchronous ack ($JS.ACK.ENVOY_NOTIFICATIONS.*.>) an empty reply. So either aimed at
// $JS.API.STREAM.DELETE.<s> deletes stream <s>. This row pins both routes on the hub's image
// (nats-server 2.15.1), each with its controls: the edge's own direct stream delete is refused and
// its stream stays; and the same pull and the same ack answered on the edge's own inbox reach it
// with the empty body the API-aimed ones carry, and leave their streams. It is expected to flip -
// the delete refused - once the hub runs nats-server 2.15.3, which refuses a reply into $JS.API.
func TestAPullStatusAndAnAckAnswerOnTheAPIDeleteAStream(t *testing.T) {
	edgeSeed, edgeKey := testnats.User(t)
	sharedSeed, sharedKey := testnats.User(t)
	adminSeed, adminKey := testnats.User(t)
	edge, shared := edge(t, "m", "PRIV"), natsgrant.Listener("shared")
	_, uri := testnats.StartImageConfig(t, testnats.HubImage, testnats.GrantedUsersConfig(
		testnats.StaticUser{NKey: edgeKey, Publish: edge.Publish, Subscribe: edge.Subscribe},
		testnats.StaticUser{NKey: sharedKey, Publish: shared.Publish, Subscribe: shared.Subscribe},
		testnats.StaticUser{NKey: adminKey, Publish: []string{">"}, Subscribe: []string{">"}},
	), []string{"-js"}, false)
	ctx := callContext(t)
	owner, err := bus.ConnectOwningStream([]string{uri}, bus.WithName("shared"), bus.WithEnvironment(testnats.SeedOnly(sharedSeed)))
	require.NoError(t, err, "the shared listener's user creates the stream")
	t.Cleanup(owner.Close)
	adminConn, _ := grantedConnection(t, uri, adminSeed)
	admin, err := adminConn.JetStream(testnats.MaxWait(t))
	require.NoError(t, err)
	for _, name := range []string{"SCRATCH_PULL", "SCRATCH_ACK", "SCRATCH_CONTROL"} {
		_, err := admin.AddStream(&natsgo.StreamConfig{Name: name, Subjects: []string{"scratch." + strings.ToLower(name) + ".>"}, Storage: natsgo.MemoryStorage}, natsgo.Context(ctx))
		require.NoError(t, err)
	}

	edgeConn, edgeErrs := grantedConnection(t, uri, edgeSeed, natsgo.CustomInboxPrefix(natsgrant.EdgeInboxPrefix("m")))
	edgeJS, err := edgeConn.JetStream(testnats.MaxWait(t))
	require.NoError(t, err)
	inbox := natsgrant.EdgeInboxPrefix("m") + ".probe"
	replies, err := edgeConn.SubscribeSync(inbox)
	require.NoError(t, err)
	require.NoError(t, edgeConn.FlushWithContext(ctx))

	// Control: the edge's own direct stream delete is refused, and SCRATCH_CONTROL stays.
	err = refusedRequest(t, edgeErrs, `Publish to "$JS.API.STREAM.DELETE.SCRATCH_CONTROL"`, func(ctx context.Context) error {
		_, err := edgeConn.RequestWithContext(ctx, "$JS.API.STREAM.DELETE.SCRATCH_CONTROL", nil)
		return err
	})
	require.Error(t, err, "the edge's direct request of a stream delete was answered")
	require.True(t, present(t, ctx, admin, "SCRATCH_CONTROL"), "the edge's direct delete removed SCRATCH_CONTROL")

	// The pull route: a consumer on a subject that holds no message.
	_, err = edgeJS.AddConsumer(bus.Stream, &natsgo.ConsumerConfig{Name: "ses_probe_pull", FilterSubject: "notifications.agent.ses_edge_pull_empty", AckPolicy: natsgo.AckExplicitPolicy}, natsgo.Context(ctx))
	require.NoError(t, err, "the edge creates the pull consumer")
	pull, err := json.Marshal(map[string]any{"batch": 1, "no_wait": true})
	require.NoError(t, err)
	const pullNext = "$JS.API.CONSUMER.MSG.NEXT.ENVOY_NOTIFICATIONS.ses_probe_pull"
	// Control: the no_wait pull answered on the edge's own inbox reaches it as a 404 status with an
	// empty body, and leaves SCRATCH_PULL.
	require.NoError(t, edgeConn.PublishRequest(pullNext, inbox, pull))
	require.NoError(t, edgeConn.FlushWithContext(ctx))
	status := nextMsg(t, replies, "the no_wait pull's status at the edge's inbox")
	require.Equal(t, "404", status.Header.Get("Status"), "the no_wait pull's status")
	require.Empty(t, status.Data, "the no_wait pull's status body")
	require.True(t, present(t, ctx, admin, "SCRATCH_PULL"), "the pull answered on the inbox removed SCRATCH_PULL")
	// Probe: the same no_wait pull answered on a stream delete deletes that stream, with no subscriber.
	require.NoError(t, edgeConn.PublishRequest(pullNext, "$JS.API.STREAM.DELETE.SCRATCH_PULL", pull))
	require.NoError(t, edgeConn.FlushWithContext(ctx))
	awaitGone(t, admin, "SCRATCH_PULL", "SCRATCH_PULL deleted by a no_wait pull's status that reached its delete subject")

	// The ack route: two stored messages, pulled to the edge's inbox for their ack subjects.
	for range 2 {
		_, err := edgeJS.Publish("notifications.agent.ses_edge_ack", []byte("x"), natsgo.Context(ctx))
		require.NoError(t, err, "the edge stores a message on its own agent subject")
	}
	_, err = edgeJS.AddConsumer(bus.Stream, &natsgo.ConsumerConfig{Name: "ses_probe_ack", FilterSubject: "notifications.agent.ses_edge_ack", AckPolicy: natsgo.AckExplicitPolicy}, natsgo.Context(ctx))
	require.NoError(t, err, "the edge creates the ack consumer")
	batch, err := json.Marshal(map[string]any{"batch": 2, "no_wait": true})
	require.NoError(t, err)
	require.NoError(t, edgeConn.PublishRequest("$JS.API.CONSUMER.MSG.NEXT.ENVOY_NOTIFICATIONS.ses_probe_ack", inbox, batch))
	require.NoError(t, edgeConn.FlushWithContext(ctx))
	var ackSubjects []string
	for len(ackSubjects) < 2 {
		msg := nextMsg(t, replies, "two of the edge's own messages pulled to its inbox")
		if msg.Reply != "" && len(msg.Data) > 0 {
			ackSubjects = append(ackSubjects, msg.Reply)
		}
	}
	// Control: a synchronous ack answered on the edge's own inbox reaches it with an empty body, and
	// leaves SCRATCH_ACK.
	require.NoError(t, edgeConn.PublishRequest(ackSubjects[0], inbox, []byte("+ACK")))
	require.NoError(t, edgeConn.FlushWithContext(ctx))
	confirm := nextMsg(t, replies, "the synchronous ack's reply at the edge's inbox")
	require.Empty(t, confirm.Data, "the synchronous ack's reply body")
	require.True(t, present(t, ctx, admin, "SCRATCH_ACK"), "the ack answered on the inbox removed SCRATCH_ACK")
	// Probe: the same synchronous ack answered on a stream delete deletes that stream, with no subscriber.
	require.NoError(t, edgeConn.PublishRequest(ackSubjects[1], "$JS.API.STREAM.DELETE.SCRATCH_ACK", []byte("+ACK")))
	require.NoError(t, edgeConn.FlushWithContext(ctx))
	awaitGone(t, admin, "SCRATCH_ACK", "SCRATCH_ACK deleted by a synchronous ack's reply that reached its delete subject")
}

// A publish of a Dispatch event on the edge listener's connection is refused by the server: the
// event never reaches a subscriber, the connection is told of the violation naming the subject,
// and the server logs it. The same publish under the listener's grant is accepted and reaches the
// subscriber, which shows the subscriber would have seen the edge's had the server let it through.
// That the edge's event reached no subscriber is read off a marker the edge publishes after it on
// its own agent subject: a connection's messages reach a subscriber in the order they were
// published (publishOutcome), so the subscriber, which takes every notification topic, receives
// the marker with nothing before it.
func TestTheEdgeCannotPublishADispatchEvent(t *testing.T) {
	const subject = "notifications.dispatch.issue.PRIV.PRIV-1.comment.created"
	const marker = "notifications.agent.marker"
	edgeSeed, edgeKey := testnats.User(t)
	listenerSeed, listenerKey := testnats.User(t)
	readerSeed, readerKey := testnats.User(t)
	edge, listener := edge(t, "m", "PRIV"), natsgrant.Listener("example-hub")
	server, uri := testnats.StartImageConfig(t, testnats.HubImage, testnats.GrantedUsersConfig(
		testnats.StaticUser{NKey: edgeKey, Publish: edge.Publish, Subscribe: edge.Subscribe},
		testnats.StaticUser{NKey: listenerKey, Publish: listener.Publish, Subscribe: listener.Subscribe},
		testnats.StaticUser{NKey: readerKey, Publish: []string{"example.unused"}, Subscribe: []string{"notifications.>"}},
	), []string{"-js"}, false)
	ctx := callContext(t)

	reader, _ := grantedConnection(t, uri, readerSeed)
	events, err := reader.SubscribeSync("notifications.>")
	require.NoError(t, err)
	require.NoError(t, reader.FlushWithContext(ctx))

	accepted, acceptedErrs := grantedConnection(t, uri, listenerSeed)
	require.NoError(t, accepted.Publish(subject, []byte("from the listener")))
	require.NoError(t, accepted.FlushWithContext(ctx))
	msg := nextMsg(t, events, "the listener's publish of "+subject+" at the subscriber")
	require.Equal(t, subject, msg.Subject, "the subscriber's first message")
	require.Equal(t, "from the listener", string(msg.Data))
	select {
	case err := <-acceptedErrs:
		t.Fatalf("the server refused the listener's publish of %s: %v", subject, err)
	default:
	}

	refused, refusedErrs := grantedConnection(t, uri, edgeSeed)
	require.NoError(t, refused.Publish(subject, []byte("from the edge")))
	require.NoError(t, refused.Publish(marker, []byte("the edge's marker")))
	require.NoError(t, refused.FlushWithContext(ctx))
	told := testwait.Receive(t, "the server's refusal of the edge's publish of "+subject, refusedErrs)
	require.True(t, errors.Is(told, natsgo.ErrPermissionViolation), "the edge's connection was told %v, want a permissions violation", told)
	require.Contains(t, told.Error(), `Publish to "`+subject+`"`)
	next := nextMsg(t, events, "the edge's marker on "+marker+" at the subscriber, without which nothing shows its publish of "+subject+" did not reach it")
	if next.Subject != marker {
		t.Fatalf("the edge's publish reached the subscriber before its marker: %s %q", next.Subject, next.Data)
	}
	// nats-server writes the violation's log line as it refuses the publish, before it reads the
	// marker, but the line reaches the docker daemon the log is read from through the container's
	// output, which the marker's delivery does not wait for, so the read waits for the line.
	testwait.Until(t, "the server's log line of a publish violation naming "+subject, func() (string, bool) {
		logs, err := server.Logs(context.Background())
		require.NoError(t, err)
		defer logs.Close()
		logged, err := io.ReadAll(logs)
		require.NoError(t, err)
		return string(logged), strings.Contains(string(logged), `Publish Violation`) && strings.Contains(string(logged), `Subject "`+subject+`"`)
	})
}

// edgeOwnLanes is one subject of each lane natsgrant.Edge("m", ["PRIV"]) publishes among the
// notification topics: the agent subjects, project PRIV's role subjects, the relay subjects, the
// relay receipts - another machine's here, which the listener serving a relay socket answers on -,
// the resume lane, and the exception lanes of the agent subjects and of PRIV's role subjects.
var edgeOwnLanes = []string{
	"notifications.agent.x",
	"notifications.role.PRIV.x",
	"notifications.envoy.deliver.x",
	"notifications.envoy.relayreply.other.x",
	"notifications.envoy.resume.x",
	"notifications.envoy.exceptions.notifications.agent.x",
	"notifications.envoy.exceptions.notifications.role.PRIV.x",
}

// edgeRefusedLanes are notification topics the edge's grant does not publish: the kept role lane
// and its drain cursors, a role of another project, a role in the form that names no project, the
// Legion wake topic, a Slack topic, and the exception lane of a GitHub topic.
var edgeRefusedLanes = []string{
	"notifications.envoy.kept.role.x",
	"notifications.envoy.kept.cursor.x",
	"notifications.role.INFRA.x",
	"notifications.role.legion-infra-controller",
	"notifications.legion.wake",
	"notifications.slack.x",
	"notifications.envoy.exceptions.notifications.github.acme.pub.pr.1.opened",
}

// publishOutcome publishes once on each of subjects, in order, as the nkey user whose seed is
// seed, and then once on a sentinel, and returns the subjects reader received before the sentinel
// and the subjects the server refused naming them. A connection's messages reach a subscriber in
// the order they were published, so a publish the reader has not received by the time the
// sentinel arrives was not accepted; each such subject must then be named by a permissions
// violation, and one that is neither received nor refused fails the test naming it.
func publishOutcome(t *testing.T, uri, seed string, reader *natsgo.Subscription, subjects []string) (accepted, refused []string) {
	t.Helper()
	const sentinel = "notifications.agent.sentinel"
	conn, errs := grantedConnection(t, uri, seed)
	for _, subject := range append(slices.Clone(subjects), sentinel) {
		require.NoError(t, conn.Publish(subject, []byte(subject)))
	}
	require.NoError(t, conn.FlushWithContext(callContext(t)))
	for {
		msg := nextMsg(t, reader, "the sentinel "+sentinel+" at the reader, without which no publish reads as accepted")
		if msg.Subject == sentinel {
			break
		}
		accepted = append(accepted, msg.Subject)
	}
	pending := map[string]bool{}
	for _, subject := range subjects {
		if !slices.Contains(accepted, subject) {
			pending[subject] = true
		}
	}
	for len(pending) > 0 {
		err := testwait.Receive(t, fmt.Sprintf("the server's refusal of each publish the reader did not receive; neither delivered nor refused yet: %v", pending), errs)
		_, quoted, found := strings.Cut(err.Error(), `Publish to "`)
		subject, _, closed := strings.Cut(quoted, `"`)
		if errors.Is(err, natsgo.ErrPermissionViolation) && found && closed && pending[subject] {
			delete(pending, subject)
			refused = append(refused, subject)
		}
	}
	return accepted, refused
}

// An edge listener's user, rendered natsgrant.Edge("m", ["PRIV"]) on the hub's image, publishes
// each of its own lanes and is refused every other notification topic named here, each refusal
// naming its subject. The same publishes as a listener's user (natsgrant.Listener) are each
// accepted and received, which shows the reader sees an accepted publish on every one of these
// subjects: a refusal of the edge's is its grant's, not a subject the reader cannot see.
func TestTheEdgeGrantPublishesOnlyItsOwnLanes(t *testing.T) {
	edgeSeed, edgeKey := testnats.User(t)
	listenerSeed, listenerKey := testnats.User(t)
	readerSeed, readerKey := testnats.User(t)
	edge, listener := edge(t, "m", "PRIV"), natsgrant.Listener("m")
	_, uri := testnats.StartImageConfig(t, testnats.HubImage, testnats.GrantedUsersConfig(
		testnats.StaticUser{NKey: edgeKey, Publish: edge.Publish, Subscribe: edge.Subscribe},
		testnats.StaticUser{NKey: listenerKey, Publish: listener.Publish, Subscribe: listener.Subscribe},
		testnats.StaticUser{NKey: readerKey, Publish: []string{"example.unused"}, Subscribe: []string{"notifications.>"}},
	), []string{"-js"}, false)
	reader, _ := grantedConnection(t, uri, readerSeed)
	received, err := reader.SubscribeSync("notifications.>")
	require.NoError(t, err)
	require.NoError(t, reader.FlushWithContext(callContext(t)))
	subjects := slices.Concat(edgeOwnLanes, edgeRefusedLanes)

	accepted, refused := publishOutcome(t, uri, listenerSeed, received, subjects)
	require.Empty(t, refused, "the listener's grant was refused publishes, so the reader cannot show an accepted one")
	require.Equal(t, subjects, accepted, "the reader did not receive every publish the listener's grant accepted")

	accepted, refused = publishOutcome(t, uri, edgeSeed, received, subjects)
	var wrongly []string
	for _, subject := range edgeRefusedLanes {
		if slices.Contains(accepted, subject) {
			wrongly = append(wrongly, subject)
		}
	}
	if len(wrongly) > 0 {
		t.Errorf("the edge's grant accepted the publishes on %s, which it must refuse", strings.Join(wrongly, ", "))
	}
	var lost []string
	for _, subject := range edgeOwnLanes {
		if slices.Contains(refused, subject) {
			lost = append(lost, subject)
		}
	}
	if len(lost) > 0 {
		t.Errorf("the edge's grant refused the publishes on %s, its own lanes", strings.Join(lost, ", "))
	}
	require.Equal(t, edgeOwnLanes, accepted, "the edge's accepted publishes, in order")
}
