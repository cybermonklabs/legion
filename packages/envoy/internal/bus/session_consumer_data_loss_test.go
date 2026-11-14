package bus

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A one-copy stream that lost the end of its data does not turn the messages a session's consumer
// holds unacked into messages it skips. nats-server v2.15.1, recovering a one-copy consumer whose
// delivered sequence is past its stream's last sequence L, reconciles it (server/consumer.go
// v2.15.1, lines 1366-1374 and 6317-6370): it logs that the consumer's delivered sequence is past
// the stream's last, lowers the consumer's ack floor and delivered sequence to L, and keeps its
// configuration's OptStartSeq, so the consumer delivers the messages the stream stores next, from
// L + 1, below that start. A restart at the consumer's floor (RestartAtFloor) with those messages
// awaiting an ack must then start at L + 1, after the floor the server reports, and not at
// OptStartSeq, which is past them: ackFloor reads the sequence before the start only for a floor
// the server reports as 0, the nats-server v2.10.29 case that term exists for, so the cut must
// leave the stream at least one message.
//
// The loss is made as nats-server's own TestJetStreamConsumerReconcileConsumerAfterStreamDataLoss
// makes it (server/jetstream_consumer_test.go v2.15.1, lines 12208-12320): with the server stopped,
// the stream's first message block is cut to half its size plus one byte and the stream's index
// (index.db) is removed, so the restarted server rebuilds the stream from what the block still
// holds. The test has a server of its own for that (testnats.Start), since it stops it and changes
// its store (testnats.Server.StoreDir). The messages stored before the consumer's start are most
// of the block, so the cut falls below that start, at most at its sequence less 3. A restart at
// OptStartSeq is then either at L + 3, past both messages stored after the loss, so it delivers
// neither, or past the stream's end, where create clamps it to the stream's first sequence; neither
// is the start after the floor the server reports, L + 1, which the test requires.
func TestRestartAtFloorAfterAOneCopyStreamLostDataDeliversThePendingMessagesAgain(t *testing.T) {
	srv, uri := testnats.Start(t)
	owner, err := ConnectOwningStream([]string{uri}, WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the stream's owner: %v", err)
	}
	t.Cleanup(owner.Close)
	consumers, err := ConnectSessionConsumers([]string{uri}, 0, nil)
	if err != nil {
		t.Fatalf("connect session consumers: %v", err)
	}
	t.Cleanup(consumers.Close)
	ctx := context.Background()
	const session = "ses_data_loss"
	// before is how many messages the stream holds when the consumer is created, and acked how
	// many the session then takes and acks.
	const before, acked = 20, 4
	name, _ := SessionConsumerName(session)
	agent := contracts.AgentSubject(session)
	filters := []string{agent}

	for i := range before {
		publishStored(t, owner, contracts.AgentSubject("ses_data_loss_bystander"), fmt.Sprintf("before %02d", i))
	}
	start, err := consumers.Ensure(ctx, session, filters, 0)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !start.Created || start.StartSeq != before+1 {
		t.Fatalf("Ensure of a session the stream holds nothing for = %+v, want the consumer created at %d, the sequence after the stream's last", start, before+1)
	}
	var seqs []uint64
	for i := range acked {
		seqs = append(seqs, publishStored(t, owner, agent, fmt.Sprintf("acked %02d", i)))
	}
	pull := pullSession(t, consumers, session)
	for _, seq := range seqs {
		msg := requireNext(t, pull, seq, "the consumer delivers its agent subject's messages in stream order")
		if err := msg.Ack(); err != nil {
			t.Fatalf("ack %d: %v", seq, err)
		}
	}
	pull.Stop()
	waitForAckFloor(t, consumers, session, before+acked)
	info := waitForAckPending(t, consumers, session, 0)
	t.Logf("before the loss the server reports the ack floor as %d, the last delivery as %d and the start sequence as %d",
		info.AckFloor.Stream, info.Delivered.Stream, info.Config.OptStartSeq)

	srv.Stop(t)
	messages := filepath.Join(srv.StoreDir(), "jetstream", "$G", "streams", Stream, "msgs")
	block, index := filepath.Join(messages, "1.blk"), filepath.Join(messages, "index.db")
	stat, err := os.Stat(block)
	if err != nil {
		t.Fatalf("the stream's first message block: %v", err)
	}
	if _, err := os.Stat(index); err != nil {
		t.Fatalf("the stream's index: %v", err)
	}
	if err := os.Truncate(block, stat.Size()/2+1); err != nil {
		t.Fatalf("cut the stream's first message block: %v", err)
	}
	if err := os.Remove(index); err != nil {
		t.Fatalf("remove the stream's index: %v", err)
	}
	t.Logf("cut %s from %d bytes to %d and removed %s", block, stat.Size(), stat.Size()/2+1, index)
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
	lost := state.lastSeq
	t.Logf("after the cut the stream's last sequence is %d, where it was %d", lost, before+acked)
	if lost == 0 || lost > before-2 {
		t.Fatalf("the stream's last sequence after the cut is %d, want 1 to %d: below the consumer's start %d less 2, so a restart there is past the two messages stored next, and above 0, which ackFloor reads as the sequence before the start", lost, before-2, before+1)
	}
	reconcile := fmt.Sprintf("JetStream consumer '$G > %s > %s' delivered sequence %d past last stream sequence of %d", Stream, name, before+acked, lost)
	if log := srv.Log(); !strings.Contains(log, reconcile) {
		var named []string
		for line := range strings.Lines(log) {
			if strings.Contains(line, name) {
				named = append(named, line)
			}
		}
		t.Fatalf("the server logged no %q; want its reconcile of the consumer whose delivered sequence is past the stream's end. Its lines naming %s:\n%s", reconcile, name, strings.Join(named, ""))
	}
	if info, err = consumers.Info(ctx, session); err != nil {
		t.Fatalf("info after the restart: %v", err)
	}
	t.Logf("the server reconciled the consumer: it reports the ack floor as %d, the last delivery as %d, %d awaiting an ack and the start sequence as %d",
		info.AckFloor.Stream, info.Delivered.Stream, info.NumAckPending, info.Config.OptStartSeq)

	held := []uint64{publishStored(t, owner, agent, "held 1"), publishStored(t, owner, agent, "held 2")}
	if held[0] != lost+1 || held[1] != lost+2 {
		t.Fatalf("the messages stored after the cut took sequences %v, want %d and %d, after the stream's last", held, lost+1, lost+2)
	}
	unacked := pullSession(t, consumers, session)
	for _, seq := range held {
		requireNext(t, unacked, seq, "the server's reconcile put the consumer's next sequence after the stream's last, below its start")
	}
	unacked.Stop()
	info = waitForAckPending(t, consumers, session, 2)
	t.Logf("with both awaiting an ack the server reports the ack floor as %d, the last delivery as %d and the start sequence as %d; the floor this package reads from that is %d",
		info.AckFloor.Stream, info.Delivered.Stream, info.Config.OptStartSeq, ackFloor(info))

	restarted, err := consumers.RestartAtFloor(ctx, session, filters)
	if err != nil {
		t.Fatalf("restart at floor: %v", err)
	}
	if !restarted.Created || restarted.StartSeq != lost+1 {
		t.Errorf("RestartAtFloor with both messages awaiting an ack = %+v, want the consumer created again at %d, after the ack floor %d the server reports", restarted, lost+1, lost)
	}
	again := pullSession(t, consumers, session)
	for _, seq := range held {
		requireNext(t, again, seq, "the restart starts after the ack floor the server reports, so both messages awaiting an ack are delivered again")
	}
}
