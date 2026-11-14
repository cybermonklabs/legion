package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A session that deregistered before its one-copy stream lost every message comes back at the
// stream's first sequence and is handed the message stored after the loss
// (bus.SessionConsumers.create). Its plugin kept the resume point it held before the loss, the
// sequence of the last message it took, and that point is past the end of the stream the loss
// left, while the stream still carries the envoy_stream_id it had. The resume record its
// deregistration wrote, which would lower the start to the floor it holds plus one, went with the
// stream's messages: the stream stored it after that floor, so a loss that cuts the stream below
// the floor cuts the record too. So nothing lowers the start the registration asks, its resume
// point plus one, and the clamp moves that start, past the stream's end, to the stream's first
// sequence. The messages now at the sequences up to the resume point were stored after the loss
// and never handed to the session, and a consumer created at the sequence after the stream's last
// would skip every one of them.
//
// The session registers in consumer mode and takes and acks three messages on its agent subject
// through the test's own view of the session consumers (the rig relays nothing), and its
// deregistration (DELETE /v1/sessions/<id>) deletes the consumer and records its floor, the last
// of those three. With the server stopped, the stream's first message block is cut to nothing and
// its index removed, as the bus's whole-loss tests make the loss
// (bus.TestRestartAtFloorAfterAOneCopyStreamLostEveryMessageDeliversThePendingMessagesAgain), so
// the restarted server holds no message of the stream, its last sequence 0, and no resume record.
// One message is then stored on the session's agent subject, at sequence 1, and the session
// registers again with its old resume point and the stream's unchanged identity. The answer's
// consumer_start and the consumer's start are 1, the stream's first sequence; the listener writes
// one WARN line of the clamp naming the session, the start asked (the old resume point plus one),
// the stream's last sequence 1, its identity and the start 1; and the consumer hands over the
// message at 1.
//
// The rig runs on a server of its own (testnats.Start), since the test stops it and changes its
// store (testnats.Server.StoreDir), and on a one-copy stream, which has no other copy for the
// server to recover the cut block from.
func TestASessionThatDeregisteredBeforeAWholeLossComesBackAtTheStreamsFirstSequence(t *testing.T) {
	srv, url := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{url}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener: %v", err)
	}
	t.Cleanup(client.Close)
	rig := newSessionConsumerRigOn(t, client, url)
	ctx := context.Background()
	const sessionID = "ses_went_back_registered"
	agent := contracts.AgentSubject(sessionID)

	// Another session's messages, so the session's own are above the stream's first sequence.
	for range 5 {
		rig.store(t, contracts.AgentSubject("ses_went_back_registered_bystander"))
	}
	streamID := rig.streamID(t)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))

	var taken []uint64
	for range 3 {
		taken = append(taken, rig.store(t, agent))
	}
	pullCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	pull, err := rig.consumers.Pull(pullCtx, sessionID)
	if err != nil {
		t.Fatalf("pull the session's consumer: %v", err)
	}
	for _, seq := range taken {
		msg, err := pull.Next(pullCtx)
		if err != nil || msg.StreamSeq != seq {
			t.Fatalf("the session's consumer handed over seq %d, %v; want the message stored at %d on its agent subject within 5s", msg.StreamSeq, err, seq)
		}
		if err := msg.Ack(); err != nil {
			t.Fatalf("ack %d: %v", seq, err)
		}
	}
	pull.Stop()
	// The resume point the session's plugin holds: the last sequence it took.
	resume := taken[len(taken)-1]
	awaitAcked(t, rig.consumers, sessionID, resume)

	rig.deregister(t, sessionID, "")
	rig.awaitNoConsumer(t, sessionID)
	if floor, err := resumeRecordFloor(client, sessionID); err != nil || floor != resume {
		t.Fatalf("the session's resume record after its deregistration holds floor %d, %v; want %d, the last message it acked", floor, err, resume)
	}

	srv.Stop(t)
	messages := filepath.Join(srv.StoreDir(), "jetstream", "$G", "streams", bus.Stream, "msgs")
	block, index := filepath.Join(messages, "1.blk"), filepath.Join(messages, "index.db")
	if err := os.Truncate(block, 0); err != nil {
		t.Fatalf("cut the stream's first message block: %v", err)
	}
	if err := os.Remove(index); err != nil {
		t.Fatalf("remove the stream's index: %v", err)
	}
	srv.Start(t)

	// Each of the rig's connections reconnects on its own: the listener's bus, its
	// session-consumer connection and the test's view of the session consumers.
	testwait.Eventually(t, "the listener's connections to reconnect and read the stream", func() bool {
		if !client.Connected() {
			return false
		}
		if _, err := client.JS().StreamInfo(bus.Stream); err != nil {
			return false
		}
		if _, err := rig.deps.consumers.consumers.StreamID(ctx); err != nil {
			return false
		}
		_, err := rig.consumers.StreamID(ctx)
		return err == nil
	})
	if _, last := rig.streamSeqs(t); last != 0 {
		t.Fatalf("the stream's last sequence after the whole block was cut is %d, want 0", last)
	}
	if id := rig.streamID(t); id != streamID {
		t.Fatalf("the stream's identity after the cut is %q, want %q, the one it carried before: the session's resume point names it", id, streamID)
	}
	if floor, err := resumeRecordFloor(client, sessionID); !errors.Is(err, natsgo.ErrMsgNotFound) {
		t.Fatalf("the session's resume record after the cut holds floor %d, %v; want none: the loss cut the stream below the record", floor, err)
	}

	held := rig.store(t, agent)
	first, last := rig.streamSeqs(t)
	if held != 1 || first != 1 || last != 1 {
		t.Fatalf("the message stored after the cut took sequence %d, the stream holding %d to %d; want 1, the stream's first and last", held, first, last)
	}

	answer := rig.subscribe(t, consumerResumeRegistration(sessionID, "epoch-2", streamID, resume))

	if answer.Delivery == nil || answer.Delivery.ConsumerStart != first {
		t.Errorf("answer %s, want consumer_start %d, the stream's first sequence, not %d, the sequence after its last, nor %d, the start its resume point %d asked", answer.raw, first, last+1, resume+1, resume)
	}
	if start := rig.startSeq(t, sessionID); start != first {
		t.Errorf("the consumer of %s starts at %d, want %d, the stream's first sequence", sessionID, start, first)
	}
	rig.requireOneLogLine(t, "WARN", clampedStartLine, sessionID, map[string]any{
		"asked_start":     resume + 1,
		"stream_last_seq": last,
		"stream_id":       streamID,
		"consumer_start":  first,
	})
	pullCtx, cancel = context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	again, err := rig.consumers.Pull(pullCtx, sessionID)
	if err != nil {
		t.Fatalf("pull the session's consumer after the loss: %v", err)
	}
	defer again.Stop()
	if msg, err := again.Next(pullCtx); err != nil || msg.StreamSeq != held {
		t.Fatalf("the session's consumer handed over seq %d, %v; want the message stored at %d after the loss within 5s: a consumer created past the stream's end, or at the sequence after its last, skips it", msg.StreamSeq, err, held)
	}
}

// resumeRecordFloor is the floor of the resume record the stream holds for sessionID
// (bus.SessionResumeSubject), read through client, or the read's error: natsgo.ErrMsgNotFound
// where the stream holds none.
func resumeRecordFloor(client *bus.Client, sessionID string) (uint64, error) {
	stored, err := client.JS().GetLastMsg(bus.Stream, bus.SessionResumeSubject(sessionID))
	if err != nil {
		return 0, err
	}
	var record bus.SessionResumeRecord
	if err := json.Unmarshal(stored.Data, &record); err != nil {
		return 0, err
	}
	return record.Floor, nil
}
