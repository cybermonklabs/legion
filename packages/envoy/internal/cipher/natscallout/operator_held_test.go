package natscallout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testwait"
	"github.com/sjawhar/envoy/natsgrant"
)

// streamBackup is what a backup of a stream holds: jsm.go v0.5.0's two files, backup.json's config
// and state and the archive.
type streamBackup struct {
	stream        string
	config, state json.RawMessage
	messages      uint64
	data          []byte
}

// snapshotChunk is the chunk and the window a backup asks for: one chunk in flight, so every chunk
// after the first waits for the acknowledgement of the one before, as each chunk past the default
// window of 8 MiB does (nats-server v2.15.1 jetstream_api.go streamSnapshot).
const snapshotChunk = 4096

// backupStream takes stream's backup through nc as jsm.go v0.5.0's (*Stream).SnapshotToDirectory
// does (snapshots.go:355-514, :558-613): the stream's info first (Manager.LoadStream, which the
// backup cannot start without), then the snapshot request, its deliver subject a response inbox of
// nc's own, each chunk acknowledged on the reply the server sends it with, until the empty message
// that ends it. It answers an error naming the step that failed. jsm.go itself is not imported:
// v0.5.0 requires antithesis-sdk-go v0.8.0, which would turn the Antithesis SDK nats-server links
// from no-op to enabled in every module of the workspace.
func backupStream(nc *nats.Conn, stream string) (streamBackup, error) {
	info, err := nc.Request("$JS.API.STREAM.INFO."+stream, nil, answerWait)
	if err != nil {
		return streamBackup{}, fmt.Errorf("load stream %s (STREAM.INFO): %w", stream, err)
	}
	var loaded apiAnswer
	if err := json.Unmarshal(info.Data, &loaded); err != nil || loaded.Error != nil {
		return streamBackup{}, fmt.Errorf("load stream %s (STREAM.INFO): %s", stream, info.Data)
	}
	inbox := nc.NewRespInbox()
	done := make(chan error, 1)
	var mu sync.Mutex
	var data []byte
	sub, err := nc.Subscribe(inbox, func(msg *nats.Msg) {
		if len(msg.Data) == 0 {
			if status := msg.Header.Get("Status"); status != "" && !strings.Contains(status, "204") {
				done <- fmt.Errorf("the snapshot of %s ended %s %s", stream, status, msg.Header.Get("Description"))
				return
			}
			done <- nil
			return
		}
		if msg.Reply != "" {
			_ = msg.Respond(nil)
		}
		mu.Lock()
		data = append(data, msg.Data...)
		mu.Unlock()
	})
	if err != nil {
		return streamBackup{}, err
	}
	defer func() { _ = sub.Unsubscribe() }()
	request, _ := json.Marshal(map[string]any{"deliver_subject": inbox, "no_consumers": true, "chunk_size": snapshotChunk, "window_size": snapshotChunk})
	reply, err := nc.Request("$JS.API.STREAM.SNAPSHOT."+stream, request, answerWait)
	if err != nil {
		return streamBackup{}, fmt.Errorf("snapshot %s (STREAM.SNAPSHOT): %w", stream, err)
	}
	var answer struct {
		apiAnswer
		Config json.RawMessage `json:"config"`
		State  json.RawMessage `json:"state"`
	}
	if err := json.Unmarshal(reply.Data, &answer); err != nil || answer.Error != nil {
		return streamBackup{}, fmt.Errorf("snapshot %s (STREAM.SNAPSHOT): %s", stream, reply.Data)
	}
	select {
	case err := <-done:
		if err != nil {
			return streamBackup{}, err
		}
	case <-time.After(30 * time.Second):
		return streamBackup{}, fmt.Errorf("the snapshot of %s did not end within 30s", stream)
	}
	var state struct {
		Msgs uint64 `json:"messages"`
	}
	_ = json.Unmarshal(answer.State, &state)
	mu.Lock()
	defer mu.Unlock()
	return streamBackup{stream: stream, config: answer.Config, state: answer.State, messages: state.Msgs, data: data}, nil
}

// restoreChunk is jsm.go v0.5.0's restore chunk, 64 KiB (snapshots.go:706).
const restoreChunk = 64 * 1024

// The bounds of restoreStream's two retries of a server answer that a moment later is different,
// each measured on this package's three-member cluster of nats-server v2.15.1.
const (
	// storeRetry bounds the restore request's retry of 10049, "error creating store for stream":
	// a restore sent at once after a delete of the same three-copy stream was refused so in one
	// of four runs, the member's store of the deleted stream not yet gone
	// (jetstream_cluster.go:7043-7044, processClusterCreateStream, answers any store failure so).
	storeRetry = 5 * time.Second
	// interestRetry bounds a chunk's retry of "no responders": the server subscribes the restore's
	// deliver subject on the member that restores it and answers the request at once
	// (jetstream_api.go:4589, :4598-4599), and the subscription reaches the client's member over
	// the route after the answer can, so the first chunk of one restore in five was answered no
	// responders.
	interestRetry = 2 * time.Second
)

// restoreStream restores b through nc as jsm.go v0.5.0's Manager.RestoreSnapshotFromDirectory does
// (snapshots.go:630-771): the restore request with the backup's config and state, each chunk as a
// request on the deliver subject the server names, then an empty request whose answer is the
// stream's create response; it answers the restored stream's message count. paced runs after each
// chunk with ctx, and an ended ctx stops the restore between chunks and cuts the chunk in flight
// short. Beyond jsm.go it retries the two answers above, each within its bound.
func restoreStream(ctx context.Context, nc *nats.Conn, b streamBackup, paced func(context.Context)) (uint64, error) {
	request, _ := json.Marshal(map[string]json.RawMessage{"config": b.config, "state": b.state})
	var started struct {
		apiAnswer
		DeliverSubject string `json:"deliver_subject"`
	}
	for first := time.Now(); ; time.Sleep(100 * time.Millisecond) {
		reply, err := nc.Request("$JS.API.STREAM.RESTORE."+b.stream, request, answerWait)
		if err != nil {
			return 0, fmt.Errorf("restore %s (STREAM.RESTORE): %w", b.stream, err)
		}
		started.Error = nil
		if err := json.Unmarshal(reply.Data, &started); err != nil {
			return 0, fmt.Errorf("restore %s (STREAM.RESTORE): %s", b.stream, reply.Data)
		}
		if started.Error == nil {
			break
		}
		if started.Error.ErrCode != 10049 || time.Since(first) > storeRetry {
			return 0, fmt.Errorf("restore %s (STREAM.RESTORE): %s", b.stream, reply.Data)
		}
	}
	for offset := 0; offset < len(b.data); offset += restoreChunk {
		if err := ctx.Err(); err != nil {
			return 0, fmt.Errorf("restore %s at byte %d: %w", b.stream, offset, err)
		}
		var ack *nats.Msg
		var err error
		for first := time.Now(); ; time.Sleep(50 * time.Millisecond) {
			chunkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			ack, err = nc.RequestWithContext(chunkCtx, started.DeliverSubject, b.data[offset:min(offset+restoreChunk, len(b.data))])
			cancel()
			if !errors.Is(err, nats.ErrNoResponders) || time.Since(first) > interestRetry || ctx.Err() != nil {
				break
			}
		}
		if err != nil {
			return 0, fmt.Errorf("restore %s: the chunk at byte %d to %s: %w", b.stream, offset, started.DeliverSubject, err)
		}
		if len(ack.Data) > 0 {
			return 0, fmt.Errorf("restore %s: the chunk at byte %d: %s", b.stream, offset, ack.Data)
		}
		if paced != nil {
			paced(ctx)
		}
	}
	finished, err := nc.Request(started.DeliverSubject, nil, time.Minute)
	if err != nil {
		return 0, fmt.Errorf("restore %s: the end of the chunks: %w", b.stream, err)
	}
	var created struct {
		apiAnswer
		State struct {
			Msgs uint64 `json:"messages"`
		} `json:"state"`
	}
	if err := json.Unmarshal(finished.Data, &created); err != nil || created.Error != nil {
		return 0, fmt.Errorf("restore %s: the end of the chunks was answered %q", b.stream, finished.Data)
	}
	return created.State.Msgs, nil
}

// The operator's grant while a hold is set: KV_envoy_sessions is snapshotted, deleted, created,
// updated, deleted again and restored from that snapshot with every message; ENVOY_MEASURE is
// created; the stream and consumer lists the writer census reads are answered; consumer-names
// cleanup deletes a retired consumer; and a create of a stream the grant does not name, the unnamed
// consumer create and a filtered consumer create on ENVOY_NOTIFICATIONS each get no answer and a
// permissions violation. The user expires at most 10 minutes after its decision, and a held
// connection is closed at its expiry: its reconnect after the row is written released is decided
// with released's grant alone.
//
// Before the row turns held, the same snapshot is refused under released and under prepare, and
// the create of KV_envoy_sessions, the create of ENVOY_MEASURE, the cleanup's delete and the stream
// list are each refused under prepare: each row's control is its step under the phase before it.
// Run by hand, a grant holding $JS.API.STREAM.CREATE.> creates KV_other and fails the test naming
// it, and a grant without the restore's chunk subject refuses the first chunk and fails it naming
// the deliver subject.
func TestTheHeldOperatorGetsTheFullList(t *testing.T) {
	r := newOperatorRig(t, nil)
	r.setPhase(t, natsgrant.OperatorPhaseReleased)
	adminNC, js := r.admin(t)
	r.makeStreams(t, 20, 1024, 0)
	// Durable: a consumer named but not durable is removed after five seconds with no interest,
	// before the cleanup below reaches it.
	_, err := js.AddConsumer(bus.Stream, &nats.ConsumerConfig{Durable: "retired-1", AckPolicy: nats.AckExplicitPolicy})
	require.NoError(t, err)
	sessions := kvBucket(bus.SessionBucket)
	created := streamConfig(t, adminNC, sessions)
	measure := map[string]any{"name": "ENVOY_MEASURE", "subjects": []string{"envoy.measure.>"}, "storage": "file", "num_replicas": 1}

	// The first token lives a few seconds, so the held user's expiry closes the connection below;
	// every later one an hour.
	reader := &tokenLives{account: operatorAccount, lives: []time.Duration{time.Hour, time.Hour, time.Hour, 4 * time.Second, time.Hour}}
	operator := r.connectOperator(t, reader)
	requireOperatorDecision(t, r.decisions.after(t, operatorName, 0), natsgrant.OperatorPhaseReleased)
	snapshot := map[string]any{"deliver_subject": operator.NewRespInbox(), "no_consumers": true, "chunk_size": snapshotChunk}
	operator.refused(t, "$JS.API.STREAM.SNAPSHOT."+sessions, snapshot)

	r.setPhase(t, natsgrant.OperatorPhasePrepare)
	seen := r.decisions.count(operatorName)
	operator.reconnect(t)
	requireOperatorDecision(t, r.decisions.after(t, operatorName, seen), natsgrant.OperatorPhasePrepare)
	operator.refused(t, "$JS.API.STREAM.SNAPSHOT."+sessions, snapshot)
	operator.refused(t, "$JS.API.STREAM.CREATE."+sessions, created)
	operator.refused(t, "$JS.API.STREAM.CREATE.ENVOY_MEASURE", measure)
	operator.refused(t, "$JS.API.CONSUMER.DELETE."+bus.Stream+".retired-1", nil)
	operator.refused(t, "$JS.API.STREAM.LIST", nil)

	r.setPhase(t, natsgrant.OperatorPhaseHeld)
	seen = r.decisions.count(operatorName)
	operator.reconnect(t)
	requireOperatorDecision(t, r.decisions.after(t, operatorName, seen), natsgrant.OperatorPhaseHeld)

	backup, err := backupStream(operator.Conn, sessions)
	require.NoError(t, err, "the held operator's snapshot of %s; the server sent:\n%s", sessions, operator.errs)
	require.EqualValues(t, 20, backup.messages)
	succeeds(t, operator.Conn, "$JS.API.STREAM.CREATE.ENVOY_MEASURE", measure)
	succeeds(t, operator.Conn, "$JS.API.STREAM.DELETE."+sessions, nil)
	createsAfterDelete(t, operator.Conn, "$JS.API.STREAM.CREATE."+sessions, created)
	updated := streamConfig(t, operator.Conn, sessions)
	updated["description"] = "updated under the hold"
	succeeds(t, operator.Conn, "$JS.API.STREAM.UPDATE."+sessions, updated)
	succeeds(t, operator.Conn, "$JS.API.STREAM.DELETE."+sessions, nil)
	restored, err := restoreStream(t.Context(), operator.Conn, backup, nil)
	require.NoError(t, err, "the held operator's restore of %s; the server sent:\n%s", sessions, operator.errs)
	require.EqualValues(t, 20, restored)
	info, err := js.StreamInfo(sessions)
	require.NoError(t, err)
	require.EqualValues(t, 20, info.State.Msgs, "the restored %s lost messages", sessions)

	succeeds(t, operator.Conn, "$JS.API.STREAM.LIST", nil)
	var names struct {
		Streams []string `json:"streams"`
	}
	require.NoError(t, json.Unmarshal(succeeds(t, operator.Conn, "$JS.API.STREAM.NAMES", nil).raw, &names))
	require.Contains(t, names.Streams, sessions)
	succeeds(t, operator.Conn, "$JS.API.CONSUMER.LIST."+bus.Stream, nil)
	succeeds(t, operator.Conn, "$JS.API.CONSUMER.DELETE."+bus.Stream+".retired-1", nil)
	awaitConsumerGone(t, adminNC, bus.Stream, "retired-1")

	operator.refused(t, "$JS.API.STREAM.CREATE.KV_other", map[string]any{"name": "KV_other", "subjects": []string{"$KV.other.>"}})
	_, err = js.StreamInfo("KV_other")
	require.ErrorIs(t, err, nats.ErrStreamNotFound, "the held operator created KV_other")
	consumer := map[string]any{"stream_name": bus.Stream, "config": map[string]any{"ack_policy": "explicit"}}
	operator.refused(t, "$JS.API.CONSUMER.CREATE."+bus.Stream, consumer)
	operator.refused(t, "$JS.API.CONSUMER.CREATE."+bus.Stream+".filtered.notifications.x",
		map[string]any{"stream_name": bus.Stream, "config": map[string]any{"name": "filtered", "filter_subject": "notifications.x", "ack_policy": "explicit"}})

	// The held user lives no longer than its token: the next decision's token lives seconds, and the
	// server closes the connection at its expiry. Once the row is released, the reconnect that
	// follows is decided with released's grant alone.
	seen = r.decisions.count(operatorName)
	operator.reconnect(t)
	short := r.decisions.after(t, operatorName, seen)
	requireOperatorDecision(t, short, natsgrant.OperatorPhaseHeld)
	require.Less(t, short.Expires.Sub(short.done), 5*time.Second, "the held user outlived its token")
	r.setPhase(t, natsgrant.OperatorPhaseReleased)
	drain(operator.reconnected)
	select {
	case <-operator.reconnected:
	case <-time.After(15 * time.Second):
		t.Fatal("the held connection was not closed and reconnected at its user's expiry")
	}
	requireOperatorDecision(t, r.decisions.after(t, operatorName, seen+1), natsgrant.OperatorPhaseReleased)
	operator.refused(t, "$JS.API.STREAM.LIST", nil)
}

// storeFailed is nats-server's answer to a stream create it could not make a file store for.
const storeFailed = "error creating store for stream"

// createsAfterDelete requires a create, through nc, of a stream deleted just before to succeed.
// nats-server answers a delete before a goroutine of its own has removed the account's streams
// directory, and a create that arrives first is answered 10049, storeFailed; no API says when that
// goroutine has run (testnats.RecreateKeyValue's doc gives the server's code). So the create is
// sent again on exactly that answer, every 100 ms, its one bound the test binary's deadline
// (testwait.UntilPolling); any other refusal fails the test at once.
func createsAfterDelete(t *testing.T, nc *nats.Conn, subject string, body any) {
	t.Helper()
	testwait.UntilPolling(t, 100*time.Millisecond, subject+" to be answered with no store failure", func() (string, bool) {
		answer := answered(t, nc, subject, body)
		if answer.Error != nil && answer.Error.ErrCode == 10049 && answer.Error.Description == storeFailed {
			t.Logf("%s answered %s; asking again", subject, answer.raw)
			return string(answer.raw), false
		}
		require.Nil(t, answer.Error, "%s was refused: %s", subject, answer.raw)
		return string(answer.raw), true
	})
}

// consumerRead bounds one read of a stream's consumer listing in awaitConsumerGone. A read that
// gets no answer within it is read again, so it bounds a try and not the wait.
const consumerRead = 10 * time.Second

// awaitConsumerGone waits for the server's own listing of stream's consumers to leave consumer
// out, which is what a delete the server answered leads to. Its one bound is the test binary's
// deadline (testwait.Until): on a loaded machine a single read of the consumer right after its
// delete once waited past its request's deadline, "context deadline exceeded", with nothing
// wrong but the wait.
func awaitConsumerGone(t *testing.T, nc *nats.Conn, stream, consumer string) {
	t.Helper()
	testwait.Until(t, "the server's listing of "+stream+"'s consumers to leave out "+consumer, func() (string, bool) {
		msg, err := nc.Request("$JS.API.CONSUMER.NAMES."+stream, nil, consumerRead)
		if err != nil {
			return "no answer: " + err.Error(), false
		}
		var names struct {
			Consumers []string `json:"consumers"`
			Error     *struct {
				Description string `json:"description"`
			} `json:"error"`
		}
		if err := json.Unmarshal(msg.Data, &names); err != nil || names.Error != nil {
			return fmt.Sprintf("the listing answered %q", msg.Data), false
		}
		return fmt.Sprint(names.Consumers), !slices.Contains(names.Consumers, consumer)
	})
}

// restoreStall is how long nats-server waits for a restore's next chunk before it ends the restore
// as stalled (v2.15.1 jetstream_api.go:4613, activityInterval).
const restoreStall = 5 * time.Second

// stallWait is the ceiling on the wait for the server to end a cut restore as stalled: far past
// restoreStall, so a loaded machine does not reach it, and the wait ends on the stall itself.
const stallWait = restoreStall + 25*time.Second

// stalledRestores is how many times the hub's members have logged a restore of stream ended as
// stalled: "Stream restore failed for '$G > <stream>': restore for stream ... is stalled".
func (r *operatorRig) stalledRestores(stream string) int {
	count := 0
	for _, server := range r.cluster.Servers {
		for _, line := range strings.Split(server.Log(), "\n") {
			if strings.Contains(line, "Stream restore failed for '$G > "+stream+"'") && strings.Contains(line, "is stalled") {
				count++
			}
		}
	}
	return count
}

// restoreCommand is the held restore as the operator's command runs it on the clustered hub:
// before each attempt a forced reconnect, so the attempt starts on a decision less than a second
// old, and a wait for the reconnect; an attempt that the connection's close cuts short is waited
// out until the server has ended it as stalled, its leftover stream is deleted
// (deleteCutRestore), and the restore runs again from its first chunk, attempts times at most.
// After the last attempt's cut it deletes the leftover too, so a stopped command leaves no stream,
// and answers an error naming the bucket and every attempt's error.
//
// The stall is a real server's timer, so the wait keeps a wall-clock ceiling (stallWait), but ends
// on the fact, the member's log line, rather than on a clock.
func restoreCommand(t *testing.T, r *operatorRig, o *operatorConn, b streamBackup, attempts int, paced func(context.Context)) error {
	t.Helper()
	var failures []string
	for range attempts {
		o.reconnect(t)
		stalled := r.stalledRestores(b.stream)
		ctx, cancel := context.WithCancel(context.Background())
		stop := make(chan struct{})
		go func() {
			select {
			case <-o.disconnected:
				cancel()
			case <-stop:
			}
		}()
		_, err := restoreStream(ctx, o.Conn, b, paced)
		close(stop)
		cancel()
		if err == nil {
			return nil
		}
		failures = append(failures, err.Error())
		testwait.WithinPolling(t, stallWait, 100*time.Millisecond, "the server ending the cut restore of "+b.stream+" as stalled", func() bool {
			return r.stalledRestores(b.stream) > stalled
		})
		testwait.WithinPolling(t, 15*time.Second, 50*time.Millisecond, "the connection after the cut", o.IsConnected)
		deleteCutRestore(t, o, b.stream)
	}
	return fmt.Errorf("restore of %s stopped after %d closes, with the hold still set: %s", b.stream, len(failures), strings.Join(failures, "; "))
}

// deleteCutRestore deletes the stream a cut restore leaves on the clustered hub, through the held
// grant's STREAM.DELETE of the bucket, and requires it gone after.
//
// A cut restore is no absence there. Measured on this package's three members of nats-server
// v2.15.1, with a three-copy bucket's restore cut after three of eleven chunks: the restoring
// member logs "Stream restore failed ... is stalled"; STREAM.INFO answers 10059, stream not found,
// for about ten seconds, and then an empty three-copy stream of that name appears and stays; and
// a second restore is refused 10130, "stream name already in use, cannot restore". The meta
// leader keeps a failed restore's assignment (jetstream_cluster.go:9276-9278: "Leave failed
// restore assignments for explicit cleanup"), and the members other than the restoring one built
// the stream from it. A delete of that name answers success, and the next restore restores every
// message. So the command deletes the name, rather than read an absence that does not last.
func deleteCutRestore(t *testing.T, o *operatorConn, stream string) {
	t.Helper()
	answer := answered(t, o.Conn, "$JS.API.STREAM.DELETE."+stream, nil)
	if answer.Error != nil {
		require.Equal(t, 10059, answer.Error.ErrCode, "the delete of %s's cut restore: %s", stream, answer.raw)
	}
	gone := answered(t, o.Conn, "$JS.API.STREAM.DELETE."+stream, nil)
	require.NotNil(t, gone.Error, "%s was still there after its cut restore's delete: %s", stream, gone.raw)
	require.Equal(t, 10059, gone.Error.ErrCode, "%s after its cut restore's delete: %s", stream, gone.raw)
}

// A held restore survives one close of its connection: with the held user living a few seconds,
// the first attempt is held after its first chunk until the server closes the connection at the
// user's expiry, which cuts it; once the server has ended that restore as stalled, the stream it
// leaves is deleted (deleteCutRestore) and is gone, and the second attempt, on the reconnect's new
// decision under held, restores every message. A second close stops the command, naming the
// bucket and both errors, and the stream its cut leaves is deleted and does not come back.
//
// The close is a real server's, at the user's expiry, so the pacing keeps a wall-clock ceiling
// (closeWait) but waits on the fact, the attempt's context ended by the disconnect, and holds no
// attempt whose user lives longer than a minute.
//
// Run by hand, the control is the restore without its retry (one attempt): the stream is missing
// after the cut, and the test fails naming the bucket.
func TestARestoreSurvivesOneReconnect(t *testing.T) {
	r := newOperatorRig(t, nil)
	r.setPhase(t, natsgrant.OperatorPhaseHeld)
	_, js := r.admin(t)
	r.makeStreams(t, 40, 16*1024, 0)
	sessions := kvBucket(bus.SessionBucket)
	const closeWait = 30 * time.Second
	paced := func(ctx context.Context) {
		if latest := r.decisions.latest(operatorName); time.Until(latest.Expires) > time.Minute {
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(closeWait):
			t.Errorf("the server did not close the held user's connection within %s of its decision", closeWait)
		}
	}

	// Reads: the first connect, the first attempt's forced reconnect (3 s), the reconnect after its
	// cut, the second attempt's forced reconnect: an hour each but the second.
	reader := &tokenLives{account: operatorAccount, lives: []time.Duration{time.Hour, 3 * time.Second, time.Hour, time.Hour}}
	operator := r.connectOperator(t, reader)
	requireOperatorDecision(t, r.decisions.after(t, operatorName, 0), natsgrant.OperatorPhaseHeld)
	backup, err := backupStream(operator.Conn, sessions)
	require.NoError(t, err)
	require.EqualValues(t, 40, backup.messages)
	require.Greater(t, len(backup.data), 2*restoreChunk, "the backup is one chunk, so a restore held after its first chunk has none left to cut")

	succeeds(t, operator.Conn, "$JS.API.STREAM.DELETE."+sessions, nil)
	seen := r.decisions.count(operatorName)
	require.NoError(t, restoreCommand(t, r, operator, backup, 2, paced), "the restore did not survive one close")
	require.GreaterOrEqual(t, r.decisions.count(operatorName)-seen, 3, "the restore ran again on no new decision")
	for _, decision := range r.decisions.since(operatorName, seen) {
		require.Equal(t, natsgrant.Operator(natsgrant.OperatorPhaseHeld), decision.Grant, "a reconnect of the restore was not given the held grant")
	}
	info := awaitStream(t, js, sessions)
	require.EqualValues(t, 40, info.State.Msgs, "the restored %s lost messages", sessions)

	t.Run("a second close stops the command", func(t *testing.T) {
		succeeds(t, operator.Conn, "$JS.API.STREAM.DELETE."+sessions, nil)
		reader.mu.Lock()
		reader.lives, reader.reads = []time.Duration{3 * time.Second, time.Hour, 3 * time.Second, time.Hour}, 0
		reader.mu.Unlock()
		err := restoreCommand(t, r, operator, backup, 2, paced)
		require.Error(t, err, "the restore completed through two closes")
		require.Contains(t, err.Error(), sessions)
		require.Contains(t, err.Error(), "stopped after 2 closes")
		require.Equal(t, 2, strings.Count(err.Error(), "restore "+sessions), "the command did not name both errors: %v", err)
		// The empty stream a cut restore leaves appeared about ten seconds after the stall when
		// nothing deleted it; this waits past that.
		require.Never(t, func() bool {
			_, err := js.StreamInfo(sessions)
			return !errors.Is(err, nats.ErrStreamNotFound)
		}, 12*time.Second, 500*time.Millisecond, "the stopped restore left %s", sessions)
	})
}

// awaitStream waits until the clustered hub answers stream's info, and returns it. Right after a
// restore its create response has answered, a member's STREAM.INFO has answered "stream not found"
// (one run in five at a load of about 1,500 read KV_envoy_sessions as missing 12.6 s in); the wait
// ends on the answer itself, and its one bound is the test binary's deadline (testwait.Until), so
// a stream that never comes back still fails the test, naming it.
func awaitStream(t *testing.T, js nats.JetStreamContext, stream string) *nats.StreamInfo {
	t.Helper()
	return testwait.Until(t, stream+" to be found after the restore", func() (*nats.StreamInfo, bool) {
		info, err := js.StreamInfo(stream)
		return info, err == nil
	})
}

// The hold's barrier is a stream: with ENVOY_NOTIFICATIONS at its consumer cap, where an unnamed
// consumer's create is refused with err_code 10026 (so a barrier consumer could never be made),
// the held operator creates the stream natsgrant.HoldBarrierStream names with the hold's settings,
// finds it in STREAM.NAMES and deletes it; a create of any other stream name is refused with a
// permissions violation.
//
// It fails when held's grant loses the barrier stream's create or delete, or when it admits a
// create of a stream name it does not name (the control row, in the test).
func TestTheBarrierIsAStream(t *testing.T) {
	r := newOperatorRig(t, nil)
	r.setPhase(t, natsgrant.OperatorPhaseHeld)
	adminNC, js := r.admin(t)
	r.makeStreams(t, 1, 16, 2)
	for _, name := range []string{"session-a", "session-b"} {
		_, err := js.AddConsumer(bus.Stream, &nats.ConsumerConfig{Durable: name, AckPolicy: nats.AckExplicitPolicy})
		require.NoError(t, err)
	}
	full := answered(t, adminNC, "$JS.API.CONSUMER.CREATE."+bus.Stream,
		map[string]any{"stream_name": bus.Stream, "config": map[string]any{"ack_policy": "explicit"}})
	require.NotNil(t, full.Error, "an unnamed consumer was created at the consumer cap: %s", full.raw)
	require.Equal(t, 10026, full.Error.ErrCode, "the unnamed create at the cap: %s", full.raw)

	operator := r.connectOperator(t, hourLong(operatorAccount))
	requireOperatorDecision(t, r.decisions.after(t, operatorName, 0), natsgrant.OperatorPhaseHeld)
	barrier := map[string]any{"name": natsgrant.HoldBarrierStream, "subjects": []string{"_ENVOY_HOLD_BARRIER.hold-1"},
		"storage": "memory", "num_replicas": 1, "max_msgs": 1}
	succeeds(t, operator.Conn, "$JS.API.STREAM.CREATE."+natsgrant.HoldBarrierStream, barrier)
	requireStreamListed(t, operator.Conn, natsgrant.HoldBarrierStream)
	succeeds(t, operator.Conn, "$JS.API.STREAM.DELETE."+natsgrant.HoldBarrierStream, nil)
	requireStreamGone(t, js, natsgrant.HoldBarrierStream)

	other := map[string]any{"name": "ENVOY_HOLD_OTHER", "subjects": []string{"_ENVOY_HOLD_OTHER.hold-1"}, "storage": "memory", "num_replicas": 1}
	operator.refused(t, "$JS.API.STREAM.CREATE.ENVOY_HOLD_OTHER", other)
}

// requireStreamListed requires conn's STREAM.NAMES to list stream, asking again until it does,
// within streamGoneWait. Right after a meta-leader election on a loaded 3-member cluster, a member
// can answer STREAM.NAMES from an assignment list that does not yet hold a stream made a moment
// before (one run listed only three of the buckets), so one answer is not the fact; each ask waits
// 5 s, and a timeout, an error answer or a list without the stream is asked again.
func requireStreamListed(t *testing.T, conn *nats.Conn, stream string) {
	t.Helper()
	testwait.WithinPolling(t, streamGoneWait, 200*time.Millisecond, stream+" listed by STREAM.NAMES", func() bool {
		msg, err := conn.Request("$JS.API.STREAM.NAMES", nil, 5*time.Second)
		if err != nil {
			return false
		}
		var names struct {
			Streams []string `json:"streams"`
		}
		return json.Unmarshal(msg.Data, &names) == nil && slices.Contains(names.Streams, stream)
	})
}

// streamGoneWait bounds requireStreamGone: long enough for a loaded box, where a 3-member cluster
// may take its meta group through an election before any member answers, and short of the test's
// own timeout, so only a stuck cluster fails it.
const streamGoneWait = 2 * time.Minute

// requireStreamGone requires JetStream to answer that stream does not exist, asking again until it
// does. In a cluster only the meta leader answers an info of a stream it holds no assignment of
// (nats-server v2.15.1 jetstream_api.go:2073-2091): another member answers nothing, and with no
// meta leader at all, as during an election on a loaded box, a member answers only that the
// cluster is not available. So each ask waits 5 s, and a timeout, or any other answer, is asked
// again until streamGoneWait.
func requireStreamGone(t *testing.T, js nats.JetStreamContext, stream string) {
	t.Helper()
	testwait.WithinPolling(t, streamGoneWait, 200*time.Millisecond, stream+" answered as not found", func() bool {
		_, err := js.StreamInfo(stream, nats.MaxWait(5*time.Second))
		return errors.Is(err, nats.ErrStreamNotFound)
	})
}
