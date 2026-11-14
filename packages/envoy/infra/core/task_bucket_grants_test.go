package core_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/legiontasks"
)

// taskKey is one claim's key in the task bucket.
const taskKey = "legion-example-infra-1-implementer"

// taskBucketConfig is the bucket as the daemon creates it (the plan's decision 4): one revision of
// each key, kept on file, with no TTL, on one copy, this server's replica count.
var taskBucketConfig = jetstream.KeyValueConfig{
	Bucket: legiontasks.Bucket, History: 1, Storage: jetstream.FileStorage, Replicas: 1,
}

// On one server enforcing the users this module renders, the daemon's grant creates the task
// bucket, updates its stream, reads its epoch, writes, reads, watches and deletes a claim's value;
// the listener's grant reads the same epoch and value and watches every write, and the server
// refuses its write, delete, create and update of the bucket; and the session's grant is refused
// every call on the bucket, each refusal read as the server's own permissions violation naming the
// subject, not taken from a timeout.
func TestTaskBucketGrantsOnAServer(t *testing.T) {
	daemonSeed, daemonKey := testnats.User(t)
	listenerSeed, listenerKey := testnats.User(t)
	sessionSeed, sessionKey := testnats.User(t)
	daemonGrant := core.DaemonPermissions()
	listenerGrant := core.ListenerPermissions("example-host-a")
	sessionGrant := core.SessionPermissions()
	config, err := core.Authorization{Users: []core.User{
		{Name: "example-daemon", NKey: daemonKey, Publish: &daemonGrant.Publish, Subscribe: &daemonGrant.Subscribe, HasSeed: true},
		{Name: "example-listener", NKey: listenerKey, Publish: &listenerGrant.Publish, Subscribe: &listenerGrant.Subscribe, HasSeed: true},
		{Name: "example-session", NKey: sessionKey, Publish: &sessionGrant.Publish, Subscribe: &sessionGrant.Subscribe, HasSeed: true},
	}}.Render(nil)
	require.NoError(t, err)
	_, uri := testnats.StartConfig(t, config, []string{"-js"}, false)

	daemon, daemonLegacy, daemonErrs := connectGranted(t, uri, daemonSeed, 10*time.Second)
	daemonJS, err := jetstream.New(daemon)
	require.NoError(t, err)

	// The daemon creates the bucket (the account's info, then the stream's create), and updates
	// its stream with the configuration it holds, as nats.go does when another daemon created it
	// first.
	kv, err := daemonJS.CreateKeyValue(taskContext(t), taskBucketConfig)
	require.NoError(t, err, "the daemon's grant could not create the task bucket")
	_, err = daemonJS.UpdateKeyValue(taskContext(t), taskBucketConfig)
	require.NoError(t, err, "the daemon's grant could not update the task bucket's stream")
	epoch := taskEpoch(t, daemonJS)

	value, err := legiontasks.Value{Task: 1}.Encode()
	require.NoError(t, err)
	first, err := kv.Put(taskContext(t), taskKey, value)
	require.NoError(t, err, "the daemon's grant could not write a claim's value")
	requireTaskValue(t, kv, daemonLegacy, 1, first)
	daemonWatch := watchTasks(t, kv)
	requireTaskUpdate(t, daemonWatch, jetstream.KeyValuePut, 1, first)
	requireTaskUpdatesCaughtUp(t, daemonWatch)

	listener, listenerLegacy, listenerErrs := connectGranted(t, uri, listenerSeed, 10*time.Second)
	listenerJS, err := jetstream.New(listener)
	require.NoError(t, err)
	read, err := listenerJS.KeyValue(taskContext(t), legiontasks.Bucket)
	require.NoError(t, err, "the listener's grant could not open the task bucket")
	require.Equal(t, epoch, taskEpoch(t, listenerJS), "the listener reads another epoch than the daemon")
	requireTaskValue(t, read, listenerLegacy, 1, first)
	listenerWatch := watchTasks(t, read)
	requireTaskUpdate(t, listenerWatch, jetstream.KeyValuePut, 1, first)
	requireTaskUpdatesCaughtUp(t, listenerWatch)

	value, err = legiontasks.Value{Task: 2}.Encode()
	require.NoError(t, err)
	second, err := kv.Put(taskContext(t), taskKey, value)
	require.NoError(t, err)
	requireTaskUpdate(t, daemonWatch, jetstream.KeyValuePut, 2, second)
	requireTaskUpdate(t, listenerWatch, jetstream.KeyValuePut, 2, second)

	// The listener's writes are refused, each by the server naming the subject.
	refused := func(call func(ctx context.Context) error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.Error(t, call(ctx))
	}
	refused(func(ctx context.Context) error { _, err := read.Put(ctx, taskKey, value); return err })
	requirePublishRefused(t, listenerErrs, "$KV."+legiontasks.Bucket+"."+taskKey)
	refused(func(ctx context.Context) error { return read.Delete(ctx, taskKey) })
	requirePublishRefused(t, listenerErrs, "$KV."+legiontasks.Bucket+"."+taskKey)
	refused(func(ctx context.Context) error {
		_, err := listenerJS.CreateKeyValue(ctx, taskBucketConfig)
		return err
	})
	requirePublishRefused(t, listenerErrs, "$JS.API.STREAM.CREATE."+legiontasks.Stream)
	refused(func(ctx context.Context) error {
		_, err := listenerJS.UpdateKeyValue(ctx, taskBucketConfig)
		return err
	})
	requirePublishRefused(t, listenerErrs, "$JS.API.STREAM.UPDATE."+legiontasks.Stream)
	requireTaskValue(t, read, listenerLegacy, 2, second)

	require.NoError(t, kv.Delete(taskContext(t), taskKey), "the daemon's grant could not delete a claim's value")
	deleted := requireTaskUpdate(t, daemonWatch, jetstream.KeyValueDelete, 0, 0)
	require.Equal(t, deleted, requireTaskUpdate(t, listenerWatch, jetstream.KeyValueDelete, 0, 0),
		"the listener's watch saw the delete at another revision than the daemon's")

	// The session is refused the bucket's open, every subject a call on the bucket publishes, and
	// the subscribe a watch's delivery needs.
	session, _, sessionErrs := connectGranted(t, uri, sessionSeed, time.Second)
	sessionJS, err := jetstream.New(session)
	require.NoError(t, err)
	refused(func(ctx context.Context) error { _, err := sessionJS.KeyValue(ctx, legiontasks.Bucket); return err })
	requirePublishRefused(t, sessionErrs, "$JS.API.STREAM.INFO."+legiontasks.Stream)
	for _, subject := range []string{
		"$JS.API.INFO",
		"$JS.API.STREAM.INFO." + legiontasks.Stream,
		"$JS.API.STREAM.CREATE." + legiontasks.Stream,
		"$JS.API.STREAM.UPDATE." + legiontasks.Stream,
		"$JS.API.DIRECT.GET." + legiontasks.Stream + ".$KV." + legiontasks.Bucket + "." + taskKey,
		"$JS.API.STREAM.MSG.GET." + legiontasks.Stream,
		"$KV." + legiontasks.Bucket + "." + taskKey,
		"$JS.API.CONSUMER.CREATE." + legiontasks.Stream + ".example.$KV." + legiontasks.Bucket + "." + taskKey,
		"$JS.API.CONSUMER.DELETE." + legiontasks.Stream + ".example",
		"$JS.FC." + legiontasks.Stream + ".example.1.2",
	} {
		require.NoError(t, session.Publish(subject, nil))
		require.NoError(t, session.Flush())
		requirePublishRefused(t, sessionErrs, subject)
	}
	deliver := session.NewInbox()
	_, err = session.SubscribeSync(deliver)
	require.NoError(t, err)
	require.NoError(t, session.Flush())
	requireSubscribeRefused(t, sessionErrs, deliver)

	require.NoError(t, daemon.Flush())
	require.NoError(t, listener.Flush())
	for name, errs := range map[string]<-chan error{"daemon": daemonErrs, "listener": listenerErrs} {
		select {
		case err := <-errs:
			t.Fatalf("the server refused the %s's grant something it should hold: %v", name, err)
		default:
		}
	}
}

// taskContext bounds one call the test expects to succeed.
func taskContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// taskEpoch reads the task bucket's epoch through js, its stream's creation time.
func taskEpoch(t *testing.T, js jetstream.JetStream) string {
	t.Helper()
	stream, err := js.Stream(taskContext(t), legiontasks.Stream)
	require.NoError(t, err, "read the task bucket's stream")
	created := stream.CachedInfo().Created
	require.False(t, created.IsZero(), "the task bucket's stream names no creation time")
	epoch := legiontasks.Epoch(created)
	parsed, err := time.Parse(time.RFC3339Nano, epoch)
	require.NoError(t, err)
	require.True(t, parsed.Equal(created), "the epoch %q is not the stream's creation time %v", epoch, created)
	return epoch
}

// requireTaskValue reads the claim's value through kv's direct get and through legacy's stored
// message get, which the stream's leader answers, and requires each to be task at revision.
func requireTaskValue(t *testing.T, kv jetstream.KeyValue, legacy natsgo.JetStreamContext, task, revision uint64) {
	t.Helper()
	entry, err := kv.Get(taskContext(t), taskKey)
	require.NoError(t, err, "read the claim's value")
	require.Equal(t, revision, entry.Revision())
	got, err := legiontasks.Decode(entry.Value())
	require.NoError(t, err)
	require.Equal(t, legiontasks.Value{Task: task}, got)
	stored, err := legacy.GetLastMsg(legiontasks.Stream, "$KV."+legiontasks.Bucket+"."+taskKey)
	require.NoError(t, err, "read the claim's stored message")
	require.Equal(t, revision, stored.Sequence)
	require.Equal(t, entry.Value(), stored.Data)
}

// watchTasks watches every key of kv until the test ends.
func watchTasks(t *testing.T, kv jetstream.KeyValue) jetstream.KeyWatcher {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	watcher, err := kv.WatchAll(ctx)
	if err != nil {
		cancel()
	}
	require.NoError(t, err, "watch the task bucket")
	t.Cleanup(func() {
		_ = watcher.Stop()
		bus.DrainWatcher(watcher)
		cancel()
	})
	return watcher
}

// nextTaskUpdate is the watcher's next update, nil for the marker that it has delivered every key
// it found.
func nextTaskUpdate(t *testing.T, watcher jetstream.KeyWatcher) jetstream.KeyValueEntry {
	t.Helper()
	select {
	case entry, ok := <-watcher.Updates():
		require.True(t, ok, "the watch ended")
		return entry
	case <-time.After(10 * time.Second):
		t.Fatal("the watch delivered nothing within 10 s")
		return nil
	}
}

// requireTaskUpdate requires the watcher's next update to be op on the claim's key, of task at
// revision for a put, and returns its revision.
func requireTaskUpdate(t *testing.T, watcher jetstream.KeyWatcher, op jetstream.KeyValueOp, task, revision uint64) uint64 {
	t.Helper()
	entry := nextTaskUpdate(t, watcher)
	require.NotNil(t, entry, "the watch caught up where it should deliver the claim's %s", op)
	require.Equal(t, taskKey, entry.Key())
	require.Equal(t, op, entry.Operation())
	if op == jetstream.KeyValuePut {
		require.Equal(t, revision, entry.Revision())
		got, err := legiontasks.Decode(entry.Value())
		require.NoError(t, err)
		require.Equal(t, legiontasks.Value{Task: task}, got)
	}
	return entry.Revision()
}

// requireTaskUpdatesCaughtUp requires the watcher's next update to be its marker that it has
// delivered every key it found.
func requireTaskUpdatesCaughtUp(t *testing.T, watcher jetstream.KeyWatcher) {
	t.Helper()
	require.Nil(t, nextTaskUpdate(t, watcher), "the watch delivered more than the bucket held")
}

// requireSubscribeRefused waits up to 10 s for the server's permissions violation of a subscribe
// to subject among errs.
func requireSubscribeRefused(t *testing.T, errs <-chan error, subject string) {
	t.Helper()
	want := `Subscription to "` + subject + `"`
	timeout := time.After(10 * time.Second)
	for {
		select {
		case err := <-errs:
			if errors.Is(err, natsgo.ErrPermissionViolation) && strings.Contains(err.Error(), want) {
				return
			}
			t.Logf("another asynchronous error: %v", err)
		case <-timeout:
			t.Fatalf("the server reported no permissions violation of the subscribe to %s", subject)
		}
	}
}
