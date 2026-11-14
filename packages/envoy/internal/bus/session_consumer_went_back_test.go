package bus

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// wentBackPath is one path that keeps or restarts a session's consumer, run on the consumer of
// session with filters, returning the start that the restart the path ends in answered.
type wentBackPath func(t *testing.T, consumers *SessionConsumers, session string, filters []string) SessionConsumerStart

// restartAfterAWholeLossLeftItUnreconciled makes the consumer nats-server v2.15.1 leaves
// unreconciled when a one-copy stream lost every message (L = 0) and the consumer had delivered
// nothing before the loss (M = 0), and requires path to restart it at the stream's first sequence.
//
// The consumer of session is created at N + 1 by Ensure, on a one-copy stream holding N = 20
// messages on a subject it does not follow, and delivers nothing. With the server stopped, the
// stream's first message block is cut to nothing and its index removed, as the loss cuts make a
// whole loss (runLossCut), and the restarted server holds no message, its last sequence 0. The
// server's reconcile of that consumer writes a state that is all 0, which the consumer's file
// store reads as state not loaded yet and writes the old state back over (ackFloor), so the
// consumer still reports an ack floor of N, above the stream's last sequence, while the stream
// carries the identity it had before the loss: its sequences went back (r6, RestartAtFloor).
//
// The stream then stores K = 2 messages on the session's agent subject, at 1 and 2, which the
// consumer as it was would never hand over: its next sequence is N + 1. path must answer the
// consumer created again at 1, the stream's first sequence, with the server reporting that start,
// write r6's one WARN line naming the floor N, the stream's last sequence 2, the stream's identity
// and the start 1, and the consumer must then hand over both messages in stream order.
func restartAfterAWholeLossLeftItUnreconciled(t *testing.T, session string, path wentBackPath) {
	t.Helper()
	srv, uri := testnats.Start(t)
	owner, err := ConnectOwningStream([]string{uri}, WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the stream's owner: %v", err)
	}
	t.Cleanup(owner.Close)
	logs := &capturedLogs{}
	consumers, err := ConnectSessionConsumers([]string{uri}, 0, slog.New(slog.NewJSONHandler(logs, nil)))
	if err != nil {
		t.Fatalf("connect session consumers: %v", err)
	}
	t.Cleanup(consumers.Close)
	ctx := context.Background()
	// before is how many messages the stream holds when the consumer is created (N).
	const before = 20
	agent := contracts.AgentSubject(session)
	filters := []string{agent}

	for i := range before {
		publishStored(t, owner, contracts.AgentSubject(session+"_bystander"), fmt.Sprintf("before %02d", i))
	}
	id := streamIDOf(t, consumers)
	start, err := consumers.Ensure(ctx, session, filters, 0)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !start.Created || start.StartSeq != before+1 {
		t.Fatalf("Ensure of a session the stream holds nothing for = %+v, want the consumer created at %d, the sequence after the stream's last", start, before+1)
	}
	// nats-server v2.15.1 reports the floor of a consumer that has acked nothing as the sequence
	// before its start, N.
	waitForAckFloor(t, consumers, session, before)
	waitForAckPending(t, consumers, session, 0)

	srv.Stop(t)
	messages := filepath.Join(srv.StoreDir(), "jetstream", "$G", "streams", Stream, "msgs")
	block, index := filepath.Join(messages, "1.blk"), filepath.Join(messages, "index.db")
	if err := os.Truncate(block, 0); err != nil {
		t.Fatalf("cut the stream's first message block: %v", err)
	}
	if err := os.Remove(index); err != nil {
		t.Fatalf("remove the stream's index: %v", err)
	}
	srv.Start(t)

	testwait.Eventually(t, "the stream's owner and the session consumers to reconnect", func() bool {
		return owner.Connected() && consumers.client.Connected()
	})
	var state sessionStreamState
	if !testwait.Holds(testwait.Bounded(t, time.Minute, time.Second), 100*time.Millisecond, func() bool {
		state, err = consumers.streamState(ctx)
		return err == nil
	}) {
		t.Fatalf("read the stream after the restart: %v", err)
	}
	if state.lastSeq != 0 {
		t.Fatalf("the stream's last sequence after the whole block was cut is %d, want 0", state.lastSeq)
	}
	if state.id != id {
		t.Fatalf("the stream's identity after the cut is %q, want %q, the one it carried before: the stream is the one the consumer's floor was counted on", state.id, id)
	}

	held := []uint64{publishStored(t, owner, agent, "held 1"), publishStored(t, owner, agent, "held 2")}
	if held[0] != 1 || held[1] != 2 {
		t.Fatalf("the messages stored after the cut took sequences %v, want 1 and 2", held)
	}
	info, err := consumers.Info(ctx, session)
	if err != nil {
		t.Fatalf("info after the cut: %v", err)
	}
	t.Logf("after the cut, with the stream's last sequence at %d, the server reports the ack floor as stream %d consumer %d, the last delivery as %d, %d awaiting an ack and the start sequence as %d",
		held[1], info.AckFloor.Stream, info.AckFloor.Consumer, info.Delivered.Stream, info.NumAckPending, info.Config.OptStartSeq)
	if info.AckFloor.Stream <= held[1] {
		t.Fatalf("the server reports the consumer's ack floor as %d, want it above the stream's last sequence %d: the consumer this case is for is the one nats-server v2.15.1 leaves unreconciled at the floor %d it had before the loss", info.AckFloor.Stream, held[1], before)
	}

	restarted := path(t, consumers, session, filters)
	if want := (SessionConsumerStart{Created: true, StartSeq: 1, StreamID: id, AckFloor: 0}); restarted != want {
		t.Errorf("the restart of a consumer whose ack floor %d is above the stream's last sequence %d = %+v; want %+v, the consumer created again at the stream's first sequence", info.AckFloor.Stream, held[1], restarted, want)
	}
	requireConsumerStart(t, consumers, session, 1, "the stream's first sequence, where the restart created it again")
	requireOneLine(t, logs, "WARN", wentBackLine, session, map[string]any{
		"ack_floor":       uint64(before),
		"stream_last_seq": held[1],
		"stream_id":       id,
		"consumer_start":  uint64(1),
	})
	pull := pullSession(t, consumers, session)
	for _, seq := range held {
		requireNext(t, pull, seq, "a message the stream stored after the loss, below the consumer's old start; a restart after its floor, clamped to the stream's end, skips it")
	}
}

// A restart at its floor (RestartAtFloor) of the consumer a one-copy stream's whole loss left
// unreconciled restarts it at the stream's first sequence (restartAfterAWholeLossLeftItUnreconciled):
// nothing it delivered awaits an ack, so it would be kept were its floor not above the stream's last
// sequence, and r6 is decided before the keep and before the next sequence past the end that the
// keep checks.
func TestRestartAtFloorRestartsAConsumerAWholeLossLeftUnreconciledAtTheStreamsFirstSequence(t *testing.T) {
	restartAfterAWholeLossLeftItUnreconciled(t, "ses_went_back_restarted", func(t *testing.T, consumers *SessionConsumers, session string, filters []string) SessionConsumerStart {
		t.Helper()
		start, err := consumers.RestartAtFloor(context.Background(), session, filters)
		if err != nil {
			t.Fatalf("restart at floor: %v", err)
		}
		return start
	})
}

// The relay's keep of the consumer a one-copy stream's whole loss left unreconciled, as the
// consumer list read it (KeepListed), answers not kept, and the restart the relay then runs
// (RestartAtFloor, cmd/listener session_relay.go) restarts it at the stream's first sequence
// (restartAfterAWholeLossLeftItUnreconciled).
func TestKeepListedLeavesAConsumerAWholeLossLeftUnreconciledToARestartAtTheStreamsFirstSequence(t *testing.T) {
	restartAfterAWholeLossLeftItUnreconciled(t, "ses_went_back_listed", func(t *testing.T, consumers *SessionConsumers, session string, filters []string) SessionConsumerStart {
		t.Helper()
		ctx := context.Background()
		listed, err := consumers.ListSessionConsumers(ctx)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if listed[session] == nil {
			t.Fatalf("the list holds no consumer for %s", session)
		}
		// The relay pulls a consumer KeepListed kept, and restarts one it did not.
		if start, kept, err := consumers.KeepListed(ctx, session, filters, listed[session]); err != nil || kept {
			t.Fatalf("KeepListed of a consumer whose ack floor %d is above the stream's last sequence = %+v, kept %t, %v; want it not kept, so the relay restarts it", listed[session].AckFloor.Stream, start, kept, err)
		}
		start, err := consumers.RestartAtFloor(ctx, session, filters)
		if err != nil {
			t.Fatalf("restart at floor: %v", err)
		}
		return start
	})
}
