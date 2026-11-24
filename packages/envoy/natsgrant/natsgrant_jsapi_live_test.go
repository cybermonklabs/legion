//go:build natslive

package natsgrant_test

// The rows of this file aim a server's own answer - a republish, a direct get's answer, a push
// consumer's delivery or heartbeat, a pull's status and an ack's reply - at the JetStream API
// ($JS.API.), each on nats-server 2.15.1 and on 2.15.3 (apiReachServers). Their 2.15.1 half and
// testnats.VMHubImage go when the VM hub is removed, and with them the reaches branches here.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/natsgrant"
)

// apiReachServers are the servers the rows of this file run on, each named with what it does with
// a JetStream answer aimed at $JS.API.: nats-server 2.15.1 (testnats.VMHubImage, the VM hub's)
// takes a republish, a push consumer's delivery and heartbeat, a request's answer and an ack's
// reply there, which each row pins as a reach. nats-server 2.15.3 (testnats.HubImage, the
// clustered hub's) refuses every one of them: a $JS. reply at the publish (client.go:4599,
// isReservedReply), a push consumer delivering on one at its create (consumer.go:825,
// isReservedDestination), and a republish into one by suppressing it (stream.go:7421-7425), each
// at v2.15.3. The deployment renders the edge set on the clustered hub alone, so the 2.15.1 half
// is a record of what that release does, not a reach a deployed edge holds; it and VMHubImage go
// when the VM hub is removed.
var apiReachServers = []struct {
	name, image string
	// reaches is whether the server delivers a JetStream answer aimed at $JS.API.
	reaches bool
}{
	{"nats-server 2.15.1 delivers into the API", testnats.VMHubImage, true},
	{"nats-server 2.15.3 refuses it", testnats.HubImage, false},
}

// replyRefused is the text of nats-server 2.15.3's refusal of a publish whose reply is under $JS.
// (client.go:6108-6109, replySubjectViolation), for refusedNaming.
func replyRefused(reply string) string { return fmt.Sprintf("Publish with Reply of %q", reply) }

// logged waits up to 10 s for a line of ctr's log holding what, and fails the test without one.
func logged(t *testing.T, ctr interface {
	Logs(context.Context) (io.ReadCloser, error)
}, what string) {
	t.Helper()
	var text string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		logs, err := ctr.Logs(context.Background())
		require.NoError(t, err, "read the server's log")
		read, err := io.ReadAll(logs)
		_ = logs.Close()
		require.NoError(t, err, "read the server's log")
		if text = string(read); strings.Contains(text, what) {
			return
		}
	}
	t.Fatalf("the server logged no line holding %q within 10s; its log:\n%s", what, text)
}

// The update of a bucket's stream can change its subjects and set a republish, and a republish to
// a $JS.API. subject is a JetStream API request in the stream's account, with the stored value as
// its body. So the edge holds no bucket stream's create or update. On each server of
// apiReachServers:
//   - the edge's update of the interest bucket's stream with a republish to
//     $JS.API.STREAM.DELETE.SCRATCH_VICTIM is refused naming the update, and leaves the stream
//     with no republish, so the edge's write of an empty value to the key the republish would copy
//     leaves SCRATCH_VICTIM as it was;
//   - in the same run, the account's administrator sets such a republish on two other buckets'
//     streams, to $JS.API.STREAM.DELETE.SCRATCH_VICTIM_ADMIN and, carrying headers only, to
//     $JS.API.STREAM.DELETE.SCRATCH_VICTIM_HEADERS, and writes the key: an empty value to the
//     first, a non-empty one to the second, whose copy carries no body. On nats-server 2.15.1 a
//     client subscribed to each subject receives the copy, and both streams are deleted, so the
//     route the edge is refused is live there. (A non-empty body is refused by the API as a delete
//     request's.) nats-server 2.15.3 logs each republish as suppressed and both streams
//     stay; its control is the same republish to scratch.republished, which a subscriber
//     receives.
func TestTheEdgeCannotSetABucketToRepublishIntoTheJetStreamAPI(t *testing.T) {
	for _, server := range apiReachServers {
		t.Run(server.name, func(t *testing.T) { edgeCannotSetABucketToRepublish(t, server.image, server.reaches) })
	}
}

func edgeCannotSetABucketToRepublish(t *testing.T, image string, reaches bool) {
	edgeSeed, edgeKey := testnats.User(t)
	adminSeed, adminKey := testnats.User(t)
	edge := edge(t, "m", "PRIV")
	ctr, uri := testnats.StartImageConfig(t, image, testnats.GrantedUsersConfig(
		testnats.StaticUser{NKey: edgeKey, Publish: edge.Publish, Subscribe: edge.Subscribe},
		testnats.StaticUser{NKey: adminKey, Publish: []string{">"}, Subscribe: []string{">"}},
	), []string{"-js"}, false)
	adminConn, _ := grantedConnection(t, uri, adminSeed)
	admin, err := adminConn.JetStream()
	require.NoError(t, err)
	for _, name := range []string{"SCRATCH_VICTIM", "SCRATCH_VICTIM_ADMIN", "SCRATCH_VICTIM_HEADERS"} {
		_, err := admin.AddStream(&natsgo.StreamConfig{Name: name, Subjects: []string{"scratch." + strings.ToLower(name) + ".>"}, Storage: natsgo.MemoryStorage})
		require.NoError(t, err)
	}
	for _, bucket := range []string{bus.InterestBucket, bus.LegionAgentBucket, bus.CIStateBucket} {
		_, err := admin.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bucket, Storage: natsgo.MemoryStorage})
		require.NoError(t, err)
	}
	republishTo := func(js natsgo.JetStreamContext, bucket, destination string, headersOnly bool) error {
		info, err := js.StreamInfo("KV_" + bucket)
		if err != nil {
			return err
		}
		config := info.Config
		config.RePublish = &natsgo.RePublish{Source: "$KV." + bucket + ".rp", Destination: destination, HeadersOnly: headersOnly}
		_, err = js.UpdateStream(&config)
		return err
	}
	edgeConn, edgeErrs := grantedConnection(t, uri, edgeSeed, natsgo.CustomInboxPrefix(natsgrant.EdgeInboxPrefix("m")))
	edgeJS, err := edgeConn.JetStream(natsgo.MaxWait(3 * time.Second))
	require.NoError(t, err)

	require.Error(t, republishTo(edgeJS, bus.InterestBucket, "$JS.API.STREAM.DELETE.SCRATCH_VICTIM", false), "the edge set a republish on KV_%s", bus.InterestBucket)
	refusedNaming(t, edgeErrs, `Publish to "$JS.API.STREAM.UPDATE.KV_`+bus.InterestBucket+`"`)
	info, err := admin.StreamInfo("KV_" + bus.InterestBucket)
	require.NoError(t, err)
	require.Nil(t, info.Config.RePublish, "KV_%s carries a republish after the edge's refused update", bus.InterestBucket)
	interests, err := edgeJS.KeyValue(bus.InterestBucket)
	require.NoError(t, err)
	_, err = interests.Put("rp", nil)
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
		require.NoError(t, adminConn.Flush())
		require.NoError(t, republishTo(admin, control.bucket, target, control.headersOnly), "the administrator sets a republish on KV_%s", control.bucket)
		bucket, err := admin.KeyValue(control.bucket)
		require.NoError(t, err)
		_, err = bucket.Put("rp", control.value)
		require.NoError(t, err)
		if !reaches {
			logged(t, ctr, fmt.Sprintf("suppressing republish with invalid subject %q", target))
			require.True(t, present(t, admin, control.victim), "%s, whose delete the server suppressed the republish to", control.victim)
			continue
		}
		copied, err := republished.NextMsg(10 * time.Second)
		require.NoError(t, err, "the administrator's republish to %s reached no subscriber within 10s, so the row cannot see it fire", target)
		require.Empty(t, copied.Data, "the republished copy on %s carries a body", target)
		require.True(t, testwait.Holds(10*time.Second, 50*time.Millisecond, func() bool { return !present(t, admin, control.victim) }), "%s is still there 10s after the administrator's republish of an empty body to its delete", control.victim)
	}
	if !reaches {
		// Control: the same republish, to a subject no stream or API takes, reaches its subscriber.
		const plain = "scratch.republished"
		republished, err := adminConn.SubscribeSync(plain)
		require.NoError(t, err)
		require.NoError(t, adminConn.Flush())
		require.NoError(t, republishTo(admin, bus.LegionAgentBucket, plain, false), "the administrator sets a republish to %s on KV_%s", plain, bus.LegionAgentBucket)
		bucket, err := admin.KeyValue(bus.LegionAgentBucket)
		require.NoError(t, err)
		_, err = bucket.Put("rp", []byte("to a subscriber"))
		require.NoError(t, err)
		copied, err := republished.NextMsg(10 * time.Second)
		require.NoError(t, err, "the administrator's republish to %s reached no subscriber within 10s", plain)
		require.Equal(t, "to a subscriber", string(copied.Data), "the republished copy on %s", plain)
	}
	require.True(t, present(t, admin, "SCRATCH_VICTIM"), "SCRATCH_VICTIM, whose delete the edge could set no republish to")
}

// A request's reply subject is checked against no permission of the requester, and the stream
// itself answers a direct get ($JS.API.DIRECT.GET.KV_<bucket>.>), through the account's own
// JetStream client, publishing the stored value as the body on that reply subject. Aimed at
// $JS.API., the JetStream API takes that value as a request of the account's. So the edge holds no
// direct get, and reads its buckets with stream message gets (bus.WithStreamMessageReads). This
// row pins both halves on each server of apiReachServers. On nats-server 2.15.1:
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
// On nats-server 2.15.3 the edge's direct gets are refused the same way, and every other
// request above, the edge's and Listener's, is refused at its publish naming its $JS.API. reply,
// so all seven streams stay as they were. Its control is Listener's direct get of dg-control
// answered on scratch.dg.answer, which reaches a subscriber there with the stored update.
//
// The edge's probes are made before the control's, and the streams they aimed at are read after
// the control's changes are seen.
func TestTheEdgeHoldsNoDirectGetWhoseAnswerReachesTheJetStreamAPI(t *testing.T) {
	for _, server := range apiReachServers {
		t.Run(server.name, func(t *testing.T) { edgeHoldsNoDirectGet(t, server.image, server.reaches) })
	}
}

func edgeHoldsNoDirectGet(t *testing.T, image string, reaches bool) {
	edgeSeed, edgeKey := testnats.User(t)
	listenerSeed, listenerKey := testnats.User(t)
	adminSeed, adminKey := testnats.User(t)
	edge, listener := edge(t, "m", "PRIV"), natsgrant.Listener("shared")
	_, uri := testnats.StartImageConfig(t, image, testnats.GrantedUsersConfig(
		testnats.StaticUser{NKey: edgeKey, Publish: edge.Publish, Subscribe: edge.Subscribe},
		testnats.StaticUser{NKey: listenerKey, Publish: listener.Publish, Subscribe: listener.Subscribe},
		testnats.StaticUser{NKey: adminKey, Publish: []string{">"}, Subscribe: []string{">"}},
	), []string{"-js"}, false)
	adminConn, _ := grantedConnection(t, uri, adminSeed)
	admin, err := adminConn.JetStream()
	require.NoError(t, err)
	streams := []string{"SCRATCH_DG", "SCRATCH_DG_DELETE", "SCRATCH_DG_MSG_GET", "SCRATCH_DG_INFO", "SCRATCH_DG_PUBACK"}
	for _, name := range append([]string{"SCRATCH_DG_CONTROL", "SCRATCH_DG_GONE"}, streams...) {
		_, err := admin.AddStream(&natsgo.StreamConfig{Name: name, Subjects: []string{"scratch." + strings.ToLower(name) + ".>"}, Storage: natsgo.MemoryStorage})
		require.NoError(t, err)
		_, err = admin.Publish("scratch."+strings.ToLower(name)+".m", []byte("kept"))
		require.NoError(t, err)
	}
	_, err = admin.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: bus.InterestBucket, Storage: natsgo.MemoryStorage})
	require.NoError(t, err)
	// state is a stream's state as this row reads it: gone, or present with its max_msgs and its
	// message count.
	state := func(name string) string {
		info, err := admin.StreamInfo(name)
		if errors.Is(err, natsgo.ErrStreamNotFound) {
			return "gone"
		}
		require.NoError(t, err, "read %s", name)
		return fmt.Sprintf("present, max_msgs %d, %d messages", info.Config.MaxMsgs, info.State.Msgs)
	}
	update := func(name string) []byte {
		info, err := admin.StreamInfo(name)
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
	// On a server that refuses a $JS. reply (reaches false), it requires the publish refused
	// naming the reply instead.
	answered := func(conn *natsgo.Conn, errs <-chan error, request, reply string, body []byte) {
		t.Helper()
		if !reaches {
			require.NoError(t, conn.PublishRequest(request, reply, body))
			require.NoError(t, conn.Flush())
			refusedNaming(t, errs, replyRefused(reply))
			return
		}
		answers, err := adminConn.SubscribeSync(reply)
		require.NoError(t, err)
		require.NoError(t, adminConn.Flush())
		require.NoError(t, conn.PublishRequest(request, reply, body))
		require.NoError(t, conn.Flush())
		_, err = answers.NextMsg(10 * time.Second)
		require.NoError(t, err, "the answer to %s reached no subscriber of %s within 10s", request, reply)
		require.NoError(t, answers.Unsubscribe())
	}
	directGet := "$JS.API.DIRECT.GET.KV_" + bus.InterestBucket + ".$KV." + bus.InterestBucket + "."

	listenerConn, listenerErrs := grantedConnection(t, uri, listenerSeed)
	listenerJS, err := listenerConn.JetStream(natsgo.MaxWait(3 * time.Second))
	require.NoError(t, err)
	interests, err := listenerJS.KeyValue(bus.InterestBucket)
	require.NoError(t, err)
	for key, value := range map[string][]byte{"dg-update": update("SCRATCH_DG"), "dg-control": update("SCRATCH_DG_CONTROL"), "dg-empty": nil} {
		_, err := interests.Put(key, value)
		require.NoError(t, err)
	}

	edgeConn, edgeErrs := grantedConnection(t, uri, edgeSeed, natsgo.CustomInboxPrefix(natsgrant.EdgeInboxPrefix("m")))
	for request, reply := range map[string]string{directGet + "dg-update": "$JS.API.STREAM.UPDATE.SCRATCH_DG", directGet + "dg-empty": "$JS.API.STREAM.DELETE.SCRATCH_DG_DELETE"} {
		require.NoError(t, edgeConn.PublishRequest(request, reply, nil))
		require.NoError(t, edgeConn.Flush())
		refusedNaming(t, edgeErrs, `Publish to "`+request+`"`)
	}
	messageGet, err := json.Marshal(map[string]string{"last_by_subj": "$KV." + bus.InterestBucket + ".dg-empty"})
	require.NoError(t, err)
	answered(edgeConn, edgeErrs, "$JS.API.STREAM.MSG.GET.KV_"+bus.InterestBucket, "$JS.API.STREAM.DELETE.SCRATCH_DG_MSG_GET", messageGet)
	answered(edgeConn, edgeErrs, "$JS.API.STREAM.INFO.KV_"+bus.InterestBucket, "$JS.API.STREAM.PURGE.SCRATCH_DG_INFO", nil)
	answered(edgeConn, edgeErrs, "$KV."+bus.InterestBucket+".dg-puback", "$JS.API.STREAM.PURGE.SCRATCH_DG_PUBACK", nil)

	answered(listenerConn, listenerErrs, directGet+"dg-control", "$JS.API.STREAM.UPDATE.SCRATCH_DG_CONTROL", nil)
	answered(listenerConn, listenerErrs, directGet+"dg-empty", "$JS.API.STREAM.DELETE.SCRATCH_DG_GONE", nil)
	control := func() string { return state("SCRATCH_DG_CONTROL") + "; " + state("SCRATCH_DG_GONE") }
	if !reaches {
		// Control: Listener's direct get answered on a subject no API takes reaches its subscriber
		// with the stored value, so the direct get the refusals above stopped is live.
		const plain = "scratch.dg.answer"
		answers, err := adminConn.SubscribeSync(plain)
		require.NoError(t, err)
		require.NoError(t, adminConn.Flush())
		require.NoError(t, listenerConn.PublishRequest(directGet+"dg-control", plain, nil))
		require.NoError(t, listenerConn.Flush())
		answer, err := answers.NextMsg(10 * time.Second)
		require.NoError(t, err, "Listener's direct get answered on %s reached no subscriber within 10s", plain)
		require.Equal(t, string(update("SCRATCH_DG_CONTROL")), string(answer.Data), "the direct get's answer on %s", plain)
		require.Equal(t, untouched+"; "+untouched, control(), "SCRATCH_DG_CONTROL and SCRATCH_DG_GONE after Listener's direct gets refused at the publish")
	} else {
		const changed = "present, max_msgs 7, 1 messages; gone"
		seen := control()
		for deadline := time.Now().Add(10 * time.Second); seen != changed && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			seen = control()
		}
		require.Equal(t, changed, seen, "SCRATCH_DG_CONTROL and SCRATCH_DG_GONE after Listener's direct gets answered on the update of the one and the delete of the other")
	}

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
// under $JS.API. or $KV. or at another stream's subjects. This row pins, on each server of
// apiReachServers, what such a consumer of the edge's does to a scratch stream or bucket of its
// account, over a message the edge stored on its own agent subject. On nats-server 2.15.1:
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
// nats-server 2.15.3 refuses the create of each consumer delivering under $JS.API.
// ("invalid push consumer deliver subject"), and the stream it aims at stays; the other deliver
// subjects, the $KV. key and the scratch subjects, behave as on 2.15.1.
//
// Controls in the same run: a subscribed subject receives the delivery; the edge's direct request
// of a stream delete is refused and the stream stays; the account's administrator deleting a
// stream is seen as deleted; and a stream of the same name in another account stays. A key the
// row cannot read reads as unread, never as absent (keyState).
func TestAnEdgePushConsumerDeliveringOnItsAccountsJetStreamAPI(t *testing.T) {
	for _, server := range apiReachServers {
		t.Run(server.name, func(t *testing.T) { edgePushConsumerOnTheAPI(t, server.image, server.reaches) })
	}
}

func edgePushConsumerOnTheAPI(t *testing.T, image string, reaches bool) {
	edgeSeed, edgeKey := testnats.User(t)
	sharedSeed, sharedKey := testnats.User(t)
	adminSeed, adminKey := testnats.User(t)
	otherSeed, otherKey := testnats.User(t)
	edge, shared := edge(t, "m", "PRIV"), natsgrant.Listener("shared")
	everything := []string{">"}
	_, uri := testnats.StartImageConfig(t, image, twoAccountsConfig(
		[]testnats.StaticUser{
			{NKey: edgeKey, Publish: edge.Publish, Subscribe: edge.Subscribe},
			{NKey: sharedKey, Publish: shared.Publish, Subscribe: shared.Subscribe},
			{NKey: adminKey, Publish: everything, Subscribe: everything},
		},
		[]testnats.StaticUser{{NKey: otherKey, Publish: everything, Subscribe: everything}},
	), []string{"-js"}, false)
	owner, err := bus.ConnectOwningStream([]string{uri}, bus.WithName("shared"), bus.WithEnvironment(func(name string) (string, bool) {
		if name == bus.NkeySeedEnvVar {
			return sharedSeed, true
		}
		return "", false
	}))
	require.NoError(t, err, "the shared listener's user creates the stream")
	t.Cleanup(owner.Close)
	adminConn, _ := grantedConnection(t, uri, adminSeed)
	admin, err := adminConn.JetStream()
	require.NoError(t, err)
	otherConn, _ := grantedConnection(t, uri, otherSeed)
	other, err := otherConn.JetStream()
	require.NoError(t, err)
	scratch := func(js natsgo.JetStreamContext, name string) {
		t.Helper()
		_, err := js.AddStream(&natsgo.StreamConfig{Name: name, Subjects: []string{"scratch." + strings.ToLower(name) + ".>"}, Storage: natsgo.MemoryStorage})
		require.NoError(t, err, "make the scratch stream %s", name)
		_, err = js.Publish("scratch."+strings.ToLower(name)+".m", []byte("kept"))
		require.NoError(t, err)
	}
	for _, name := range []string{"SCRATCH_DELETE", "SCRATCH_DELETE_SUB", "SCRATCH_PURGE", "SCRATCH_UPDATE", "SCRATCH_CAPTURE", "SCRATCH_CAPTURE_SUB", "SCRATCH_CONTROL", "SCRATCH_ADMIN"} {
		scratch(admin, name)
	}
	scratch(other, "SCRATCH_DELETE")
	_, err = admin.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: "scratch_kv_delete", Storage: natsgo.MemoryStorage})
	require.NoError(t, err)
	written, err := admin.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: "scratch_kv_write", Storage: natsgo.MemoryStorage})
	require.NoError(t, err)
	update, err := admin.StreamInfo("SCRATCH_UPDATE")
	require.NoError(t, err)
	changed := update.Config
	changed.MaxMsgs = 7
	updateBody, err := json.Marshal(changed)
	require.NoError(t, err)
	streamState := func(name string) string {
		info, err := admin.StreamInfo(name)
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
	keyState := func(bucket natsgo.KeyValue, key string) string {
		entry, err := bucket.Get(key)
		switch {
		case errors.Is(err, natsgo.ErrKeyNotFound):
			return "no entry"
		case err != nil:
			return "unread: " + err.Error()
		}
		return fmt.Sprintf("entry %q", entry.Value())
	}
	var received atomic.Int64
	const kept, keptEmpty = "kept, 1 messages, max_msgs -1", "kept, 0 messages, max_msgs -1"
	edgeConn, edgeErrs := grantedConnection(t, uri, edgeSeed, natsgo.CustomInboxPrefix(natsgrant.EdgeInboxPrefix("m")))
	edgeJS, err := edgeConn.JetStream()
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
				require.NoError(t, adminConn.Flush())
			}
			before := delivery.state()
			own := fmt.Sprintf("notifications.agent.ses_edge_api_%d", i)
			_, err := edgeJS.Publish(own, []byte(delivery.payload))
			require.NoError(t, err, "the edge stores the message on its own agent subject")
			name := fmt.Sprintf("edge-api-%d", i)
			_, err = edgeJS.AddConsumer(bus.Stream, &natsgo.ConsumerConfig{Name: name, FilterSubject: own, DeliverSubject: delivery.target, AckPolicy: natsgo.AckNonePolicy})
			if !reaches && strings.HasPrefix(delivery.target, "$JS.API.") {
				require.ErrorContains(t, err, "invalid push consumer deliver subject", "the edge's push consumer delivering on %s", delivery.target)
				require.Equal(t, delivery.want, delivery.state(), "%s after the refused create", delivery.target)
				return
			}
			require.NoError(t, err, "the edge creates a push consumer delivering on %s", delivery.target)
			if !delivery.subscribed {
				info, err := admin.ConsumerInfo(bus.Stream, name)
				require.NoError(t, err)
				require.False(t, info.PushBound, "the consumer on %s has interest no client gave it", delivery.target)
				require.Zero(t, info.Delivered.Consumer, "the consumer delivered on %s with no interest", delivery.target)
				require.Equal(t, delivery.want, delivery.state())
				return
			}
			delivered := false
			for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline) && !delivered; time.Sleep(50 * time.Millisecond) {
				info, err := admin.ConsumerInfo(bus.Stream, name)
				delivered = err == nil && info.Delivered.Consumer >= 1
			}
			require.True(t, delivered, "the consumer on %s delivered nothing within 10s, though a client subscribes it", delivery.target)
			select {
			case subject := <-subjects:
				require.Equal(t, own, subject, "the delivery on %s came under %s, not its stream subject", delivery.target, subject)
			case <-time.After(10 * time.Second):
				t.Fatalf("the client subscribed to %s received no delivery within 10s", delivery.target)
			}
			// The subject above is why an API path takes no action; no clock decides it. A case
			// whose stream stores the delivery waits for that store, which lands just behind the
			// client's copy, and every other case reads its state once.
			after := delivery.state()
			for deadline := time.Now().Add(10 * time.Second); after == before && before != delivery.want && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
				after = delivery.state()
			}
			require.Equal(t, delivery.want, after, "%s after the edge's delivery (before: %s)", delivery.target, before)
		})
	}

	t.Run("controls", func(t *testing.T) {
		_, err := edgeConn.Request("$JS.API.STREAM.DELETE.SCRATCH_CONTROL", nil, 2*time.Second)
		require.Error(t, err, "the edge's direct request of a stream delete was answered")
		refusedNaming(t, edgeErrs, `Publish to "$JS.API.STREAM.DELETE.SCRATCH_CONTROL"`)
		require.Equal(t, kept, streamState("SCRATCH_CONTROL"), "the edge's direct request changed SCRATCH_CONTROL")
		require.NoError(t, admin.DeleteStream("SCRATCH_ADMIN"))
		require.Equal(t, "deleted", streamState("SCRATCH_ADMIN"), "the probe does not see a stream the administrator deleted")
		info, err := other.StreamInfo("SCRATCH_DELETE")
		require.NoError(t, err, "account B's SCRATCH_DELETE is gone")
		require.Equal(t, uint64(1), info.State.Msgs, "account B's SCRATCH_DELETE changed")
	})
}

// A push consumer with an idle heartbeat sends the heartbeat, an empty status message with no
// stored subject, to its deliver subject through the stream's own client, which is how a direct
// get's answer travels and not how a delivery does. On nats-server 2.15.1 (apiReachServers), with
// a client subscribed to $JS.API.STREAM.DELETE.SCRATCH_HB, so the consumer has interest, a consumer
// the edge creates over a subject that holds no message, delivering there with a heartbeat, sends
// heartbeats the client receives under the deliver subject itself with an empty body, and
// SCRATCH_HB is deleted. The control in the same run is a consumer the edge creates the same way on
// SCRATCH_HB_UNSUBSCRIBED's delete, which no client subscribes: it is not push bound and that
// stream stays. The edge's grant subscribes no $JS.API. subject, and no Envoy client subscribes
// one, so the route needs a subscriber outside them. nats-server 2.15.3 refuses both
// creates ("invalid push consumer deliver subject") and both streams stay; its control is the same
// consumer delivering on scratch.hb, whose subscriber receives its heartbeat.
func TestAPushConsumersHeartbeatAimedAtAStreamDeleteWhileAClientSubscribesIt(t *testing.T) {
	for _, server := range apiReachServers {
		t.Run(server.name, func(t *testing.T) { pushConsumersHeartbeatOnTheAPI(t, server.image, server.reaches) })
	}
}

func pushConsumersHeartbeatOnTheAPI(t *testing.T, image string, reaches bool) {
	edgeSeed, edgeKey := testnats.User(t)
	sharedSeed, sharedKey := testnats.User(t)
	adminSeed, adminKey := testnats.User(t)
	edge, shared := edge(t, "m", "PRIV"), natsgrant.Listener("shared")
	_, uri := testnats.StartImageConfig(t, image, testnats.GrantedUsersConfig(
		testnats.StaticUser{NKey: edgeKey, Publish: edge.Publish, Subscribe: edge.Subscribe},
		testnats.StaticUser{NKey: sharedKey, Publish: shared.Publish, Subscribe: shared.Subscribe},
		testnats.StaticUser{NKey: adminKey, Publish: []string{">"}, Subscribe: []string{">"}},
	), []string{"-js"}, false)
	owner, err := bus.ConnectOwningStream([]string{uri}, bus.WithName("shared"), bus.WithEnvironment(func(name string) (string, bool) {
		if name == bus.NkeySeedEnvVar {
			return sharedSeed, true
		}
		return "", false
	}))
	require.NoError(t, err, "the shared listener's user creates the stream")
	t.Cleanup(owner.Close)
	adminConn, _ := grantedConnection(t, uri, adminSeed)
	admin, err := adminConn.JetStream()
	require.NoError(t, err)
	for _, name := range []string{"SCRATCH_HB", "SCRATCH_HB_UNSUBSCRIBED"} {
		_, err := admin.AddStream(&natsgo.StreamConfig{Name: name, Subjects: []string{"scratch." + strings.ToLower(name) + ".>"}, Storage: natsgo.MemoryStorage})
		require.NoError(t, err)
	}
	const target = "$JS.API.STREAM.DELETE.SCRATCH_HB"
	heartbeats, err := adminConn.SubscribeSync(target)
	require.NoError(t, err)
	require.NoError(t, adminConn.Flush())

	edgeConn, _ := grantedConnection(t, uri, edgeSeed, natsgo.CustomInboxPrefix(natsgrant.EdgeInboxPrefix("m")))
	edgeJS, err := edgeConn.JetStream()
	require.NoError(t, err)
	for name, deliver := range map[string]string{"edge-hb": target, "edge-hb-unsubscribed": "$JS.API.STREAM.DELETE.SCRATCH_HB_UNSUBSCRIBED"} {
		_, err := edgeJS.AddConsumer(bus.Stream, &natsgo.ConsumerConfig{Name: name, FilterSubject: "notifications.agent.ses_edge_hb_" + name, DeliverSubject: deliver, AckPolicy: natsgo.AckNonePolicy, Heartbeat: time.Second})
		if !reaches {
			require.ErrorContains(t, err, "invalid push consumer deliver subject", "the edge's push consumer with a heartbeat delivering on %s", deliver)
			continue
		}
		require.NoError(t, err, "the edge creates a push consumer with a heartbeat delivering on %s", deliver)
	}
	if !reaches {
		// Control: the same consumer delivering on a subject no API takes sends its heartbeat to
		// that subject's subscriber.
		const plain = "scratch.hb"
		plainBeats, err := adminConn.SubscribeSync(plain)
		require.NoError(t, err)
		require.NoError(t, adminConn.Flush())
		_, err = edgeJS.AddConsumer(bus.Stream, &natsgo.ConsumerConfig{Name: "edge-hb-plain", FilterSubject: "notifications.agent.ses_edge_hb_edge-hb-plain", DeliverSubject: plain, AckPolicy: natsgo.AckNonePolicy, Heartbeat: time.Second})
		require.NoError(t, err, "the edge creates a push consumer with a heartbeat delivering on %s", plain)
		beat, err := plainBeats.NextMsg(10 * time.Second)
		require.NoError(t, err, "no heartbeat reached the client subscribed to %s within 10s", plain)
		require.Equal(t, "100", beat.Header.Get("Status"), "the heartbeat's status")
		require.True(t, present(t, admin, "SCRATCH_HB"), "SCRATCH_HB, whose delete the server refused a consumer to deliver on")
		require.True(t, present(t, admin, "SCRATCH_HB_UNSUBSCRIBED"), "SCRATCH_HB_UNSUBSCRIBED, whose delete the server refused a consumer to deliver on")
		return
	}
	beat, err := heartbeats.NextMsg(10 * time.Second)
	require.NoError(t, err, "no heartbeat reached the client subscribed to %s within 10s", target)
	require.Equal(t, target, beat.Subject, "the heartbeat came under %s", beat.Subject)
	require.Empty(t, beat.Data, "the heartbeat's body")
	require.Equal(t, "100", beat.Header.Get("Status"), "the heartbeat's status")
	require.True(t, testwait.Holds(10*time.Second, 50*time.Millisecond, func() bool { return !present(t, admin, "SCRATCH_HB") }), "SCRATCH_HB is still there 10s after a heartbeat reached its delete subject")

	info, err := admin.ConsumerInfo(bus.Stream, "edge-hb-unsubscribed")
	require.NoError(t, err)
	require.False(t, info.PushBound, "the consumer on SCRATCH_HB_UNSUBSCRIBED's delete has interest no client gave it")
	require.True(t, present(t, admin, "SCRATCH_HB_UNSUBSCRIBED"), "SCRATCH_HB_UNSUBSCRIBED, whose delete no client subscribes")
}

// The consumer calls the edge keeps answer two requests with no body on whatever reply subject the
// requester names, each needing no subscriber: a no_wait pull on a consumer with nothing to deliver
// (CONSUMER.MSG.NEXT.ENVOY_NOTIFICATIONS.*) is answered a "404 No Messages" status, and a
// synchronous ack ($JS.ACK.ENVOY_NOTIFICATIONS.*.>) an empty reply. So either aimed at
// $JS.API.STREAM.DELETE.<s> deletes stream <s> on nats-server 2.15.1 (apiReachServers). This row
// pins both routes on each server, each with its controls: the edge's own direct stream
// delete is refused and its stream stays; and the same pull and the same ack answered on the edge's
// own inbox reach it with the empty body the API-aimed ones carry, and leave their streams.
// nats-server 2.15.3 refuses each API-aimed publish naming its $JS.API. reply, and both streams
// stay.
func TestAPullStatusAndAnAckAnswerAimedAtAStreamDelete(t *testing.T) {
	for _, server := range apiReachServers {
		t.Run(server.name, func(t *testing.T) { pullStatusAndAckAnswerOnTheAPI(t, server.image, server.reaches) })
	}
}

func pullStatusAndAckAnswerOnTheAPI(t *testing.T, image string, reaches bool) {
	edgeSeed, edgeKey := testnats.User(t)
	sharedSeed, sharedKey := testnats.User(t)
	adminSeed, adminKey := testnats.User(t)
	edge, shared := edge(t, "m", "PRIV"), natsgrant.Listener("shared")
	_, uri := testnats.StartImageConfig(t, image, testnats.GrantedUsersConfig(
		testnats.StaticUser{NKey: edgeKey, Publish: edge.Publish, Subscribe: edge.Subscribe},
		testnats.StaticUser{NKey: sharedKey, Publish: shared.Publish, Subscribe: shared.Subscribe},
		testnats.StaticUser{NKey: adminKey, Publish: []string{">"}, Subscribe: []string{">"}},
	), []string{"-js"}, false)
	owner, err := bus.ConnectOwningStream([]string{uri}, bus.WithName("shared"), bus.WithEnvironment(func(name string) (string, bool) {
		if name == bus.NkeySeedEnvVar {
			return sharedSeed, true
		}
		return "", false
	}))
	require.NoError(t, err, "the shared listener's user creates the stream")
	t.Cleanup(owner.Close)
	adminConn, _ := grantedConnection(t, uri, adminSeed)
	admin, err := adminConn.JetStream()
	require.NoError(t, err)
	for _, name := range []string{"SCRATCH_PULL", "SCRATCH_ACK", "SCRATCH_CONTROL"} {
		_, err := admin.AddStream(&natsgo.StreamConfig{Name: name, Subjects: []string{"scratch." + strings.ToLower(name) + ".>"}, Storage: natsgo.MemoryStorage})
		require.NoError(t, err)
	}

	edgeConn, edgeErrs := grantedConnection(t, uri, edgeSeed, natsgo.CustomInboxPrefix(natsgrant.EdgeInboxPrefix("m")))
	edgeJS, err := edgeConn.JetStream()
	require.NoError(t, err)
	inbox := natsgrant.EdgeInboxPrefix("m") + ".probe"
	replies, err := edgeConn.SubscribeSync(inbox)
	require.NoError(t, err)
	require.NoError(t, edgeConn.Flush())

	// Control: the edge's own direct stream delete is refused, and SCRATCH_CONTROL stays.
	_, err = edgeConn.Request("$JS.API.STREAM.DELETE.SCRATCH_CONTROL", nil, 2*time.Second)
	require.Error(t, err, "the edge's direct request of a stream delete was answered")
	refusedNaming(t, edgeErrs, `Publish to "$JS.API.STREAM.DELETE.SCRATCH_CONTROL"`)
	require.True(t, present(t, admin, "SCRATCH_CONTROL"), "the edge's direct delete removed SCRATCH_CONTROL")

	// The pull route: a consumer on a subject that holds no message.
	_, err = edgeJS.AddConsumer(bus.Stream, &natsgo.ConsumerConfig{Name: "ses_probe_pull", FilterSubject: "notifications.agent.ses_edge_pull_empty", AckPolicy: natsgo.AckExplicitPolicy})
	require.NoError(t, err, "the edge creates the pull consumer")
	pull, err := json.Marshal(map[string]any{"batch": 1, "no_wait": true})
	require.NoError(t, err)
	const pullNext = "$JS.API.CONSUMER.MSG.NEXT.ENVOY_NOTIFICATIONS.ses_probe_pull"
	// Control: the no_wait pull answered on the edge's own inbox reaches it as a 404 status with an
	// empty body, and leaves SCRATCH_PULL.
	require.NoError(t, edgeConn.PublishRequest(pullNext, inbox, pull))
	require.NoError(t, edgeConn.Flush())
	status, err := replies.NextMsg(10 * time.Second)
	require.NoError(t, err, "the no_wait pull's status reached no subscriber of the edge's inbox within 10s")
	require.Equal(t, "404", status.Header.Get("Status"), "the no_wait pull's status")
	require.Empty(t, status.Data, "the no_wait pull's status body")
	require.True(t, present(t, admin, "SCRATCH_PULL"), "the pull answered on the inbox removed SCRATCH_PULL")
	// Probe: the same no_wait pull answered on a stream delete deletes that stream, with no
	// subscriber, or is refused at its publish naming that reply.
	require.NoError(t, edgeConn.PublishRequest(pullNext, "$JS.API.STREAM.DELETE.SCRATCH_PULL", pull))
	require.NoError(t, edgeConn.Flush())
	if reaches {
		require.True(t, testwait.Holds(10*time.Second, 50*time.Millisecond, func() bool { return !present(t, admin, "SCRATCH_PULL") }), "SCRATCH_PULL is still there 10s after a no_wait pull's status reached its delete subject")
	} else {
		refusedNaming(t, edgeErrs, replyRefused("$JS.API.STREAM.DELETE.SCRATCH_PULL"))
		require.True(t, present(t, admin, "SCRATCH_PULL"), "SCRATCH_PULL after the server refused the pull aimed at its delete")
	}

	// The ack route: two stored messages, pulled to the edge's inbox for their ack subjects.
	for range 2 {
		_, err := edgeJS.Publish("notifications.agent.ses_edge_ack", []byte("x"))
		require.NoError(t, err, "the edge stores a message on its own agent subject")
	}
	_, err = edgeJS.AddConsumer(bus.Stream, &natsgo.ConsumerConfig{Name: "ses_probe_ack", FilterSubject: "notifications.agent.ses_edge_ack", AckPolicy: natsgo.AckExplicitPolicy})
	require.NoError(t, err, "the edge creates the ack consumer")
	batch, err := json.Marshal(map[string]any{"batch": 2, "no_wait": true})
	require.NoError(t, err)
	require.NoError(t, edgeConn.PublishRequest("$JS.API.CONSUMER.MSG.NEXT.ENVOY_NOTIFICATIONS.ses_probe_ack", inbox, batch))
	require.NoError(t, edgeConn.Flush())
	var ackSubjects []string
	for len(ackSubjects) < 2 {
		msg, err := replies.NextMsg(10 * time.Second)
		require.NoError(t, err, "the edge pulled fewer than two of its own messages within 10s")
		if msg.Reply != "" && len(msg.Data) > 0 {
			ackSubjects = append(ackSubjects, msg.Reply)
		}
	}
	// Control: a synchronous ack answered on the edge's own inbox reaches it with an empty body, and
	// leaves SCRATCH_ACK.
	require.NoError(t, edgeConn.PublishRequest(ackSubjects[0], inbox, []byte("+ACK")))
	require.NoError(t, edgeConn.Flush())
	confirm, err := replies.NextMsg(10 * time.Second)
	require.NoError(t, err, "the synchronous ack's reply reached no subscriber of the edge's inbox within 10s")
	require.Empty(t, confirm.Data, "the synchronous ack's reply body")
	require.True(t, present(t, admin, "SCRATCH_ACK"), "the ack answered on the inbox removed SCRATCH_ACK")
	// Probe: the same synchronous ack answered on a stream delete deletes that stream, with no
	// subscriber, or is refused at its publish naming that reply.
	require.NoError(t, edgeConn.PublishRequest(ackSubjects[1], "$JS.API.STREAM.DELETE.SCRATCH_ACK", []byte("+ACK")))
	require.NoError(t, edgeConn.Flush())
	if reaches {
		require.True(t, testwait.Holds(10*time.Second, 50*time.Millisecond, func() bool { return !present(t, admin, "SCRATCH_ACK") }), "SCRATCH_ACK is still there 10s after a synchronous ack's reply reached its delete subject")
	} else {
		refusedNaming(t, edgeErrs, replyRefused("$JS.API.STREAM.DELETE.SCRATCH_ACK"))
		require.True(t, present(t, admin, "SCRATCH_ACK"), "SCRATCH_ACK after the server refused the ack aimed at its delete")
	}
}
