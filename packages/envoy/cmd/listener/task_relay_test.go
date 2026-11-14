package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/legiontasks"
)

// The task relay's interval and lookup wait in these rows: short, so a row that waits for the
// relay's next look at the bucket spends a tenth of a second rather than thirty.
const (
	taskTestInterval = 100 * time.Millisecond
	taskTestOpenWait = time.Second
)

// taskRig is a listener's /v1 routes and session relays over the in-process server (relayRig),
// with its task relay running beside them.
type taskRig struct {
	*relayRig
	tasks *taskRelay
}

// newTaskRig is a task rig on a server of its own, as machine test-machine, holding its machine's
// durable binding, so it relays the sessions that register through it.
func newTaskRig(t *testing.T) *taskRig {
	t.Helper()
	rig := newTaskRigOn(t, testnats.URL(t), "test-machine")
	rig.relays.setOwned(true)
	return rig
}

// newTaskRigOn is a task rig on url whose session relays and task relay run as machine machineID,
// not holding its machine's durable binding until the test calls rig.relays.setOwned(true). Its
// /v1 routes write every registration as test-machine's (newSessionConsumerRigOn), so a session
// registered through a rig of another machine id is not that rig's to relay.
func newTaskRigOn(t *testing.T, url, machineID string) *taskRig {
	t.Helper()
	client, err := bus.ConnectOwningStream([]string{url}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener: %v", err)
	}
	t.Cleanup(client.Close)
	consumers := newSessionConsumerRigOn(t, client, url)
	config := consumers.harness.config
	config.machineID = machineID
	relays, err := startSessionRelays(consumers.deps.consumers, config)
	if err != nil {
		t.Fatalf("start the session relays: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		relays.stop(ctx)
	})
	return &taskRig{
		relayRig: &relayRig{sessionConsumerRig: consumers, relays: relays, url: url},
		tasks:    startTestTaskRelay(t, consumers, relays, taskTestInterval, taskTestOpenWait),
	}
}

// startTestTaskRelay starts the task relay of rig's listener beside relays, as main starts it,
// with the interval and lookup wait given, and stops it when the test ends.
func startTestTaskRelay(t *testing.T, rig *sessionConsumerRig, relays *sessionRelays, interval, openWait time.Duration) *taskRelay {
	t.Helper()
	tasks := startTaskRelay(rig.harness.client, relays, rig.harness.registry, rig.harness.config.logger, interval, openWait)
	t.Cleanup(tasks.stop)
	return tasks
}

// claimTaskRole has sessionID claim role through rig's /v1/roles/set, as a worker claims its
// claim's role once it is ready.
func claimTaskRole(t *testing.T, rig *sessionConsumerRig, sessionID, role string) {
	t.Helper()
	rig.post(t, "/v1/roles/set", fmt.Sprintf(`{"session_id":%q,"role":%q}`, sessionID, role))
}

// createTaskBucket creates the task bucket on the server url names as a Legion daemon creates it
// (packages/daemon/internal/daemon/taskbucket.go: one revision a key, on file, no TTL), through a
// connection of its own, and returns the JetStream context it was created through and its handle.
func createTaskBucket(t *testing.T, url string) (jetstream.JetStream, jetstream.KeyValue) {
	t.Helper()
	conn := testnats.Connect(t, url)
	t.Cleanup(conn.Close)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("open JetStream as the daemon: %v", err)
	}
	return js, createTaskBucketOn(t, js)
}

// createTaskBucketOn creates the task bucket through js, the daemon's JetStream context.
func createTaskBucketOn(t *testing.T, js jetstream.JetStream) jetstream.KeyValue {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: legiontasks.Bucket, History: 1, Storage: jetstream.FileStorage})
	if err != nil {
		t.Fatalf("create the task bucket as the daemon: %v", err)
	}
	return kv
}

// taskEpoch is the bucket's epoch as every program spells it: its stream's creation time
// (legiontasks.Epoch), read as the daemon reads it.
func taskEpoch(t *testing.T, js jetstream.JetStream) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := js.Stream(ctx, legiontasks.Stream)
	if err != nil {
		t.Fatalf("read the task bucket's stream: %v", err)
	}
	return legiontasks.Epoch(stream.CachedInfo().Created)
}

// lastTaskRevision is the revision of the task bucket's latest write, its stream's last sequence:
// a delete's, which the delete itself does not answer.
func lastTaskRevision(t *testing.T, js jetstream.JetStream) uint64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := js.Stream(ctx, legiontasks.Stream)
	if err != nil {
		t.Fatalf("read the task bucket's stream: %v", err)
	}
	return stream.CachedInfo().State.LastSeq
}

// encodedTask is task number's value as the bucket holds it.
func encodedTask(t *testing.T, number uint64) []byte {
	t.Helper()
	data, err := legiontasks.Value{Task: number}.Encode()
	if err != nil {
		t.Fatalf("encode task %d: %v", number, err)
	}
	return data
}

// putTask writes task number as key's value, as the daemon does, and returns the write's revision.
func putTask(t *testing.T, kv jetstream.KeyValue, key string, number uint64) uint64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	revision, err := kv.Put(ctx, key, encodedTask(t, number))
	if err != nil {
		t.Fatalf("write task %d as %s's value: %v", number, key, err)
	}
	return revision
}

// deleteTask deletes key's value, as the daemon does once the claim has no task.
func deleteTask(t *testing.T, kv jetstream.KeyValue, key string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := kv.Delete(ctx, key); err != nil {
		t.Fatalf("delete %s's value: %v", key, err)
	}
}

// taskFrame is a task frame a session stand-in received on its relay subject.
type taskFrame struct {
	claim    string
	epoch    string
	op       string
	revision uint64
	data     []byte
	reply    string
}

// require fails the test unless the frame relays claim's record of epoch at revision: op, and
// data, the value's bytes as the bucket holds them (none for a delete).
func (f taskFrame) require(t *testing.T, claim, op, epoch string, revision uint64, data []byte) {
	t.Helper()
	if f.claim != claim || f.op != op || f.epoch != epoch || f.revision != revision || !bytes.Equal(f.data, data) {
		t.Fatalf("the task frame relays claim %q op %q epoch %q revision %d data %q, want claim %q op %q epoch %q revision %d data %q",
			f.claim, f.op, f.epoch, f.revision, f.data, claim, op, epoch, revision, data)
	}
}

// taskStandIn takes a session's frames on its relay subject on a NATS connection of its own, as a
// plugin in consumer mode does: each task frame (one carrying legiontasks.HeaderClaim) on tasks,
// and each frame of the session's own consumer, unanswered, on messages.
type taskStandIn struct {
	conn     *natsgo.Conn
	tasks    chan taskFrame
	messages chan *natsgo.Msg
}

func startTaskStandIn(t *testing.T, url, sessionID string) *taskStandIn {
	t.Helper()
	conn := testnats.Connect(t, url)
	t.Cleanup(conn.Close)
	return startTaskStandInOn(t, conn, sessionID)
}

// startTaskStandInOn is the stand-in of sessionID on conn, a connection the test opened.
func startTaskStandInOn(t *testing.T, conn *natsgo.Conn, sessionID string) *taskStandIn {
	t.Helper()
	standIn := &taskStandIn{conn: conn, tasks: make(chan taskFrame, 64), messages: make(chan *natsgo.Msg, 64)}
	subscription, err := conn.Subscribe(bus.DeliverSubject(sessionID), func(msg *natsgo.Msg) {
		claim := msg.Header.Get(legiontasks.HeaderClaim)
		if claim == "" {
			standIn.messages <- msg
			return
		}
		revision, err := strconv.ParseUint(msg.Header.Get(legiontasks.HeaderRevision), 10, 64)
		if err != nil {
			t.Errorf("a task frame for %s carries revision %q: %v", sessionID, msg.Header.Get(legiontasks.HeaderRevision), err)
		}
		standIn.tasks <- taskFrame{
			claim:    claim,
			epoch:    msg.Header.Get(legiontasks.HeaderEpoch),
			op:       msg.Header.Get(legiontasks.HeaderOp),
			revision: revision,
			data:     msg.Data,
			reply:    msg.Reply,
		}
	})
	if err != nil {
		t.Fatalf("subscribe the stand-in of %s to its relay subject: %v", sessionID, err)
	}
	t.Cleanup(func() { _ = subscription.Unsubscribe() })
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the stand-in's subscription: %v", err)
	}
	return standIn
}

// next is the next task frame, failing the test when none arrives within the bound.
func (s *taskStandIn) next(t *testing.T, within time.Duration) taskFrame {
	t.Helper()
	select {
	case frame := <-s.tasks:
		return frame
	case <-time.After(within):
		t.Fatalf("the session received no task frame within %s", within)
		return taskFrame{}
	}
}

// none fails the test when a task frame arrives within the window, or, for a window of 0, when one
// has arrived already.
func (s *taskStandIn) none(t *testing.T, window time.Duration) {
	t.Helper()
	var frame taskFrame
	if window == 0 {
		select {
		case frame = <-s.tasks:
		default:
			return
		}
	} else {
		select {
		case frame = <-s.tasks:
		case <-time.After(window):
			return
		}
	}
	t.Fatalf("the session received a task frame within %s, want none: claim %q op %q revision %d from %s",
		window, frame.claim, frame.op, frame.revision, frame.reply)
}

// message is the next frame of the session's own consumer, failing the test when none arrives
// within the bound.
func (s *taskStandIn) message(t *testing.T, within time.Duration) *natsgo.Msg {
	t.Helper()
	select {
	case msg := <-s.messages:
		return msg
	case <-time.After(within):
		t.Fatalf("the session received no frame of its consumer within %s", within)
		return nil
	}
}

// logRecords counts the JSON records of logs whose msg is name and that match accepts.
func logRecords(logs, name string, match func(map[string]any) bool) int {
	count := 0
	for line := range strings.SplitSeq(logs, "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) != nil || record["msg"] != name {
			continue
		}
		if match(record) {
			count++
		}
	}
	return count
}

// anyRecord accepts every record.
func anyRecord(map[string]any) bool { return true }

// awaitTaskRead waits for rig's task relay to have read the bucket whole once, its watch's warm-up
// line naming entries keys.
func awaitTaskRead(t *testing.T, rig *taskRig, entries int) {
	t.Helper()
	waitFor(t, 10*time.Second, fmt.Sprintf("the task relay's read of the bucket's %d keys", entries), func() bool {
		return logRecords(rig.harness.logs.String(), taskRelayName+" cache warm-up", func(record map[string]any) bool {
			return record["entries"] == float64(entries)
		}) > 0
	})
}

// A value written before anyone holds its claim's role is relayed to nobody, and the moment a
// session claims the role it is relayed the claim's current value, once, naming its revision and
// the bucket's epoch.
func TestTaskRelayRelaysTheCurrentValueWhenTheRoleIsClaimed(t *testing.T) {
	rig := newTaskRig(t)
	js, kv := createTaskBucket(t, rig.url)
	const sessionID = "ses_task_claimed"
	claim := "legion-infra-infra-2811-1-implementer"
	revision := putTask(t, kv, claim, 3)
	session := startTaskStandIn(t, rig.url, sessionID)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	// The relay holds the value, and nobody holds the role yet: it relays nothing.
	awaitTaskRead(t, rig, 1)
	session.none(t, 500*time.Millisecond)

	claimTaskRole(t, rig.sessionConsumerRig, sessionID, claim)

	session.next(t, 5*time.Second).require(t, claim, legiontasks.OpPut, taskEpoch(t, js), revision, encodedTask(t, 3))
	// The claim reaches the role view twice, as this listener's own write and as the watch's copy
	// of it, and the value is relayed once.
	session.none(t, time.Second)
}

// Each newer value of the claim the session holds the role of is relayed as it is written: a new
// number, the same number written again (a value written again keeps its number and gets a
// revision of its own), and a delete, which carries no data.
func TestTaskRelayRelaysEachNewerValue(t *testing.T) {
	rig := newTaskRig(t)
	js, kv := createTaskBucket(t, rig.url)
	const sessionID = "ses_task_newer"
	claim := "legion-infra-infra-2811-1-reviewer"
	session := startTaskStandIn(t, rig.url, sessionID)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	claimTaskRole(t, rig.sessionConsumerRig, sessionID, claim)
	epoch := taskEpoch(t, js)

	for _, number := range []uint64{1, 2, 2} {
		revision := putTask(t, kv, claim, number)
		session.next(t, 5*time.Second).require(t, claim, legiontasks.OpPut, epoch, revision, encodedTask(t, number))
	}
	deleteTask(t, kv, claim)
	session.next(t, 5*time.Second).require(t, claim, legiontasks.OpDelete, epoch, lastTaskRevision(t, js), nil)
	revision := putTask(t, kv, claim, 3)
	session.next(t, 5*time.Second).require(t, claim, legiontasks.OpPut, epoch, revision, encodedTask(t, 3))
	session.none(t, time.Second)
}

// A session that does not hold the role of a claim is relayed none of its values: not a session
// holding another role, not a key whose role nobody holds, and not the session that held the role
// before another claimed it, while the session that claimed it is relayed the current value at its
// claim and each value after it.
func TestTaskRelayRelaysNothingToASessionThatDoesNotHoldTheRole(t *testing.T) {
	rig := newTaskRig(t)
	js, kv := createTaskBucket(t, rig.url)
	const holderID, bystanderID = "ses_task_holder", "ses_task_bystander"
	claim := "legion-infra-infra-2811-1-tester"
	holder := startTaskStandIn(t, rig.url, holderID)
	bystander := startTaskStandIn(t, rig.url, bystanderID)
	rig.subscribe(t, consumerRegistration(holderID, "epoch-1"))
	rig.subscribe(t, consumerRegistration(bystanderID, "epoch-1"))
	claimTaskRole(t, rig.sessionConsumerRig, holderID, claim)
	claimTaskRole(t, rig.sessionConsumerRig, bystanderID, "legion-infra-infra-2811-2-tester")
	epoch := taskEpoch(t, js)

	revision := putTask(t, kv, claim, 7)
	holder.next(t, 5*time.Second).require(t, claim, legiontasks.OpPut, epoch, revision, encodedTask(t, 7))
	putTask(t, kv, "legion-infra-infra-2811-3-tester", 1)
	bystander.none(t, time.Second)
	holder.none(t, 0)

	// The role moves to the bystander: it is relayed the claim's current value, and each value
	// after it, and the session that held the role is relayed nothing more.
	claimTaskRole(t, rig.sessionConsumerRig, bystanderID, claim)
	bystander.next(t, 5*time.Second).require(t, claim, legiontasks.OpPut, epoch, revision, encodedTask(t, 7))
	revision = putTask(t, kv, claim, 8)
	bystander.next(t, 5*time.Second).require(t, claim, legiontasks.OpPut, epoch, revision, encodedTask(t, 8))
	holder.none(t, time.Second)
	bystander.none(t, 0)
}

// A task frame names the key's revision and the bucket's epoch, the RFC 3339 creation time of the
// bucket's stream, and its reply subject is under the relay's own reply inbox, where an answer
// goes nowhere: a client that cannot read the frame answers it done, and that answer acks nothing
// of the session's consumer. A bucket deleted and created again is a new epoch, whose revisions
// count from 1 again, and its frames say so.
func TestTaskRelayFrameNamesTheRevisionAndEpoch(t *testing.T) {
	rig := newTaskRig(t)
	js, kv := createTaskBucket(t, rig.url)
	const sessionID = "ses_task_epoch"
	claim := "legion-infra-infra-2811-1-planner"
	session := startTaskStandIn(t, rig.url, sessionID)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	claimTaskRole(t, rig.sessionConsumerRig, sessionID, claim)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	stream, err := js.Stream(ctx, legiontasks.Stream)
	cancel()
	if err != nil {
		t.Fatalf("read the task bucket's stream: %v", err)
	}
	created := stream.CachedInfo().Created

	revision := putTask(t, kv, claim, 5)
	frame := session.next(t, 5*time.Second)
	frame.require(t, claim, legiontasks.OpPut, legiontasks.Epoch(created), revision, encodedTask(t, 5))
	if epoch, err := time.Parse(time.RFC3339Nano, frame.epoch); err != nil || !epoch.Equal(created) {
		t.Fatalf("the frame's epoch %q reads as %v (%v), want the stream's creation time %v", frame.epoch, epoch, err, created)
	}
	if !strings.HasPrefix(frame.reply, rig.relays.replyPrefix) {
		t.Fatalf("the frame's reply subject %q is not under the relay's reply inbox %q", frame.reply, rig.relays.replyPrefix)
	}

	// A message of the session's own consumer, received and not yet done, awaits its ack.
	_, seq := rig.storeEvent(t, contracts.AgentSubject(sessionID))
	held := session.message(t, 5*time.Second)
	if err := session.conn.Publish(held.Reply, []byte(relayReplyReceived)); err != nil {
		t.Fatalf("answer the session's frame received: %v", err)
	}
	if err := session.conn.Flush(); err != nil {
		t.Fatalf("flush the answer: %v", err)
	}
	awaitAckState(t, rig.consumers, sessionID, 1, 0)
	// An old client's answer to the task frame.
	if err := session.conn.Publish(frame.reply, []byte(relayReplyDone)); err != nil {
		t.Fatalf("answer the task frame done: %v", err)
	}
	if err := session.conn.Flush(); err != nil {
		t.Fatalf("flush the answer: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	info, err := rig.consumers.Info(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("read the session's consumer: %v", err)
	}
	if info.NumAckPending != 1 {
		t.Fatalf("the session's consumer awaits %d acks after the task frame was answered done, want the 1 its own frame holds", info.NumAckPending)
	}
	if err := session.conn.Publish(held.Reply, []byte(relayReplyDone)); err != nil {
		t.Fatalf("answer the session's frame done: %v", err)
	}
	if err := session.conn.Flush(); err != nil {
		t.Fatalf("flush the answer: %v", err)
	}
	awaitAcked(t, rig.consumers, sessionID, seq)

	// The daemons create the bucket again: a new epoch, its revisions from 1.
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	err = js.DeleteKeyValue(ctx, legiontasks.Bucket)
	cancel()
	if err != nil {
		t.Fatalf("delete the task bucket: %v", err)
	}
	kv = createTaskBucketOn(t, js)
	epoch := taskEpoch(t, js)
	if epoch == legiontasks.Epoch(created) {
		t.Fatalf("the bucket created again carries the epoch %q of the one deleted", epoch)
	}
	revision = putTask(t, kv, claim, 6)
	if revision != 1 {
		t.Fatalf("the bucket created again wrote its first value at revision %d, want 1", revision)
	}
	session.next(t, 15*time.Second).require(t, claim, legiontasks.OpPut, epoch, 1, encodedTask(t, 6))
	session.none(t, time.Second)
}

// Every listener watches the bucket, and only the one that relays the session's own consumer
// relays its claim's value: the listener its entry names, while it holds its machine's durable
// binding. A listener of another machine id relays nothing, nor does a second task of the same
// machine id while it does not hold the binding; once the binding moves to it, it relays and the
// task that let go of it does not.
func TestTaskRelayOnlyTheOwningListenerPublishes(t *testing.T) {
	url := testnats.URL(t)
	owner := newTaskRigOn(t, url, "test-machine")
	owner.relays.setOwned(true)
	elsewhere := newTaskRigOn(t, url, "other-machine")
	elsewhere.relays.setOwned(true)
	// A rolling deploy's second task of the owner's machine, which does not hold the binding yet.
	successor := newTaskRigOn(t, url, "test-machine")
	_, kv := createTaskBucket(t, url)
	const sessionID = "ses_task_owned"
	claim := "legion-infra-infra-2811-1-merger"
	session := startTaskStandIn(t, url, sessionID)
	owner.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	claimTaskRole(t, owner.sessionConsumerRig, sessionID, claim)
	awaitTaskRead(t, owner, 0)
	awaitTaskRead(t, elsewhere, 0)

	putTask(t, kv, claim, 1)
	if frame := session.next(t, 5*time.Second); !strings.HasPrefix(frame.reply, owner.relays.replyPrefix) {
		t.Fatalf("the task frame came from the listener whose reply inbox is %q, want the owner's %q (another machine's is %q, the waiting task's %q)",
			frame.reply, owner.relays.replyPrefix, elsewhere.relays.replyPrefix, successor.relays.replyPrefix)
	}
	session.none(t, time.Second)

	owner.relays.setOwned(false)
	successor.relays.setOwned(true)
	awaitTaskRead(t, successor, 1)
	revision := putTask(t, kv, claim, 2)
	// The successor relays from here: the value written now, and the one it read at its open,
	// which the session already holds and takes once for its revision, when it relays that too.
	for {
		frame := session.next(t, 5*time.Second)
		if !strings.HasPrefix(frame.reply, successor.relays.replyPrefix) {
			t.Fatalf("a task frame after the binding moved came from the listener whose reply inbox is %q, want the successor's %q",
				frame.reply, successor.relays.replyPrefix)
		}
		if frame.revision == revision {
			break
		}
	}
	session.none(t, time.Second)
}

// listenerGrantWithoutTaskBucket is the publish grant envoy-infra renders for a listener of
// machine machineID (core.ListenerPermissions) less the task bucket's reads (taskBucketReads): the
// listener grant and the session consumers' calls. This module cannot import envoy-infra's core,
// so the subjects are spelled here from the names that grant is built from, as
// listenerGrantBeforeSessionConsumers spells the one before session consumers.
func listenerGrantWithoutTaskBucket(machineID string) []string {
	durable := bus.ListenerDurable(machineID)
	allow := []string{
		bus.NotificationTopicPrefix + ">",
		"$JS.API.STREAM.INFO." + bus.Stream,
		"$JS.API.STREAM.CREATE." + bus.Stream,
		"$JS.API.STREAM.UPDATE." + bus.Stream,
		"$JS.API.STREAM.MSG.GET." + bus.Stream,
		"$JS.API.CONSUMER.INFO." + bus.Stream + "." + durable,
		"$JS.API.CONSUMER.CREATE." + bus.Stream + "." + durable,
		"$JS.ACK." + bus.Stream + "." + durable + ".>",
		"$JS.API.INFO",
	}
	for _, bucket := range bus.ListenerBuckets() {
		stream := "KV_" + bucket
		allow = append(allow,
			"$JS.API.STREAM.INFO."+stream,
			"$JS.API.STREAM.CREATE."+stream,
			"$JS.API.STREAM.UPDATE."+stream,
			"$JS.API.DIRECT.GET."+stream+".>",
			"$JS.API.STREAM.MSG.GET."+stream,
			"$KV."+bucket+".>",
			"$JS.API.CONSUMER.CREATE."+stream+".>",
			"$JS.API.CONSUMER.DELETE."+stream+".>",
			"$JS.FC."+stream+".>",
		)
	}
	return append(allow,
		"$JS.API.STREAM.PURGE.KV_"+bus.InterestBucket,
		"$JS.API.CONSUMER.CREATE."+bus.Stream+".*",
		"$JS.API.CONSUMER.CREATE."+bus.Stream+".*.>",
		"$JS.API.CONSUMER.INFO."+bus.Stream+".*",
		"$JS.API.CONSUMER.DELETE."+bus.Stream+".*",
		"$JS.API.CONSUMER.MSG.NEXT."+bus.Stream+".*",
		"$JS.API.CONSUMER.LIST."+bus.Stream,
		"$JS.ACK."+bus.Stream+".*.>",
	)
}

// A listener whose NATS user was rendered without the task bucket's reads gives consumer delivery,
// so its task relay looks the bucket up and is refused: it logs the subject its grant is missing
// at ERROR, once an interval, and nothing else changes. The listener stays healthy, a session
// registers for consumer delivery, claims a role and is relayed its messages.
func TestTaskRelayWithoutTheGrantKeepsServingAndLogsTheGrant(t *testing.T) {
	refused := "$JS.API.STREAM.INFO." + legiontasks.Stream
	refusedRecord := func(record map[string]any) bool {
		return record["level"] == "ERROR" && record["missing_subject"] == refused
	}

	t.Run("the listener serves", func(t *testing.T) {
		const machineID = "task-relay-ungranted"
		seed, public := testnats.User(t)
		grant := testnats.StartNkeyGranted(t, public, listenerGrantWithoutTaskBucket(machineID)...)
		listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), grant.URL, machineID, "NATS_NKEY_SEED="+seed)
		t.Cleanup(func() {
			if t.Failed() {
				t.Logf("listener %s:\n%s", machineID, listener.Output.String())
			}
		})
		listener.waitHealthy(t)
		refusals := func() int { return countRecords(listener, taskRelayRefusedLine, refusedRecord) }
		// The lookup waits out its JetStream wait, ten seconds, before it reads the refusal.
		waitFor(t, 30*time.Second, "the task relay's ERROR naming "+refused, func() bool { return refusals() > 0 })

		conn, err := testnats.ConnectSeed(t, grant.URL, seed)
		if err != nil {
			t.Fatalf("connect as the session: %v", err)
		}
		t.Cleanup(conn.Close)
		const sessionID = "ses_task_ungranted"
		session := startTaskStandInOn(t, conn, sessionID)
		postListener(t, listener.port, "/v1/interests/subscribe", consumerRegistration(sessionID, "epoch-1"))
		postListener(t, listener.port, "/v1/roles/set", `{"session_id":"`+sessionID+`","role":"legion-infra-infra-2811-1-implementer"}`)
		js, err := conn.JetStream()
		if err != nil {
			t.Fatalf("open JetStream as the session's sender: %v", err)
		}
		item := listenerTestEnvelope(contracts.AgentSubject(sessionID), "task-relay-ungranted")
		if _, err := js.Publish(contracts.AgentSubject(sessionID), marshalListenerEnvelope(t, item)); err != nil {
			t.Fatalf("store a message for the session: %v", err)
		}
		var relayed contracts.Envelope
		if err := json.Unmarshal(session.message(t, 10*time.Second).Data, &relayed); err != nil || relayed.EventID != item.EventID {
			t.Fatalf("the session was relayed event %q (%v), want %q", relayed.EventID, err, item.EventID)
		}
		listener.waitHealthy(t)
		// Within its first interval of thirty seconds the relay has logged the refusal once.
		if got := refusals(); got != 1 {
			t.Fatalf("the refusal was logged %d times within the task relay's first interval, want once", got)
		}
	})

	t.Run("once an interval", func(t *testing.T) {
		const interval = 300 * time.Millisecond
		seed, public := testnats.User(t)
		grant := testnats.StartNkeyGranted(t, public, listenerGrantWithoutTaskBucket("test-machine")...)
		t.Setenv("NATS_NKEY_SEED", seed)
		client, err := bus.ConnectOwningStream([]string{grant.URL}, bus.WithReplicas(1))
		if err != nil {
			t.Fatalf("connect the listener's user: %v", err)
		}
		t.Cleanup(client.Close)
		rig := newSessionConsumerRigOn(t, client, grant.URL)
		relays, err := startSessionRelays(rig.deps.consumers, rig.harness.config)
		if err != nil {
			t.Fatalf("start the session relays: %v", err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			relays.stop(ctx)
		})
		startTestTaskRelay(t, rig, relays, interval, 200*time.Millisecond)
		relays.setOwned(true)
		refusals := func() int { return logRecords(rig.harness.logs.String(), taskRelayRefusedLine, refusedRecord) }
		waitFor(t, 10*time.Second, "the task relay's first ERROR naming "+refused, func() bool { return refusals() > 0 })
		first := time.Now()

		time.Sleep(10 * interval)

		got, elapsed := refusals(), time.Since(first)
		if most := int(elapsed/interval) + 2; got < 3 || got > most {
			t.Fatalf("the refusal was logged %d times over %s at an interval of %s, want it again every interval and never more often (at least 3, at most %d)",
				got, elapsed, interval, most)
		}
		if lines := logRecords(rig.harness.logs.String(), taskRelayFailedLine, anyRecord); lines != 0 {
			t.Fatalf("the refused lookup was also logged as another failure %d times", lines)
		}
	})
}

// A bucket the Legion daemons have not created yet is waited for quietly: one INFO line, no
// warning and no error however many times the relay looks it up, and every other route serves.
// Once a daemon creates the bucket, the relay opens it on its next look and relays the value
// written to the role's holder.
func TestTaskRelayWaitsQuietlyForAnAbsentBucket(t *testing.T) {
	rig := newTaskRig(t)
	const sessionID = "ses_task_absent"
	claim := "legion-infra-infra-2811-1-architect"
	session := startTaskStandIn(t, rig.url, sessionID)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	claimTaskRole(t, rig.sessionConsumerRig, sessionID, claim)
	waitFor(t, 5*time.Second, "the task relay's line that it waits for the bucket", func() bool {
		return logRecords(rig.harness.logs.String(), taskRelayWaitLine, anyRecord) > 0
	})

	// Ten more looks at the bucket.
	time.Sleep(10 * taskTestInterval)

	logs := rig.harness.logs.String()
	if lines := logRecords(logs, taskRelayWaitLine, anyRecord); lines != 1 {
		t.Fatalf("the wait for the bucket was logged %d times, want once:\n%s", lines, logs)
	}
	for line := range strings.SplitSeq(logs, "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) != nil {
			continue
		}
		if msg, _ := record["msg"].(string); strings.HasPrefix(msg, taskRelayName) && record["level"] != "INFO" {
			t.Fatalf("the task relay logged at %v while it waited for the bucket: %s", record["level"], line)
		}
	}
	item, _ := rig.storeEvent(t, contracts.AgentSubject(sessionID))
	var relayed contracts.Envelope
	if err := json.Unmarshal(session.message(t, 5*time.Second).Data, &relayed); err != nil || relayed.EventID != item.EventID {
		t.Fatalf("the session was relayed event %q (%v), want %q", relayed.EventID, err, item.EventID)
	}
	session.none(t, 0)

	js, kv := createTaskBucket(t, rig.url)
	revision := putTask(t, kv, claim, 9)
	session.next(t, 5*time.Second).require(t, claim, legiontasks.OpPut, taskEpoch(t, js), revision, encodedTask(t, 9))
}
