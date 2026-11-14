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

// A one-copy stream that lost every message it held (L = 0) does not turn the messages a session's
// consumer holds unacked into messages it skips, as one that lost only the end of its data does not
// (TestRestartAtFloorAfterAOneCopyStreamLostDataDeliversThePendingMessagesAgain). nats-server
// v2.15.1 reconciles the consumer to the stream's last sequence, 0 here: it reports an ack floor of
// 0 and a delivered sequence of 0, and keeps its configuration's OptStartSeq. The consumer then
// delivers the messages the stream stores next, from sequence 1. A restart at the consumer's floor
// (RestartAtFloor) with those messages awaiting an ack must start at sequence 1, so they are
// delivered again, and not at OptStartSeq, which the clamp lowers to the sequence after the stream's
// last (create): that is past both messages. So ackFloor must tell this reconciled consumer from
// the one its OptStartSeq - 1 term exists for, a consumer nats-server v2.10.29 reports with an ack
// floor of 0 before any ack moves it.
//
// The loss is made as the partial cut's test makes it, with the whole first message block cut
// instead of half of it.
func TestRestartAtFloorAfterAOneCopyStreamLostEveryMessageDeliversThePendingMessagesAgain(t *testing.T) {
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
	const session = "ses_whole_loss"
	// before is how many messages the stream holds when the consumer is created, and acked how
	// many the session then takes and acks.
	const before, acked = 20, 4
	name, _ := SessionConsumerName(session)
	agent := contracts.AgentSubject(session)
	filters := []string{agent}

	for i := range before {
		publishStored(t, owner, contracts.AgentSubject("ses_whole_loss_bystander"), fmt.Sprintf("before %02d", i))
	}
	start, err := consumers.Ensure(ctx, session, filters, 0)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !start.Created || start.StartSeq != before+1 {
		t.Fatalf("Ensure of a session the stream holds nothing for = %+v, want the consumer created at %d, the sequence after the stream's last", start, before+1)
	}
	pull := pullSession(t, consumers, session)
	for i := range acked {
		seq := publishStored(t, owner, agent, fmt.Sprintf("acked %02d", i))
		if err := requireNext(t, pull, seq, "the consumer delivers its agent subject's messages in stream order").Ack(); err != nil {
			t.Fatalf("ack %d: %v", seq, err)
		}
	}
	pull.Stop()
	waitForAckFloor(t, consumers, session, before+acked)
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
	reconcile := fmt.Sprintf("JetStream consumer '$G > %s > %s' delivered sequence %d past last stream sequence of 0", Stream, name, before+acked)
	if log := srv.Log(); !strings.Contains(log, reconcile) {
		t.Fatalf("the server logged no %q; want its reconcile of the consumer whose delivered sequence is past the stream's end", reconcile)
	}
	info, err := consumers.Info(ctx, session)
	if err != nil {
		t.Fatalf("info after the restart: %v", err)
	}
	t.Logf("the server reconciled the consumer: it reports the ack floor as %d, the last delivery as %d and the start sequence as %d",
		info.AckFloor.Stream, info.Delivered.Stream, info.Config.OptStartSeq)

	held := []uint64{publishStored(t, owner, agent, "held 1"), publishStored(t, owner, agent, "held 2")}
	if held[0] != 1 || held[1] != 2 {
		t.Fatalf("the messages stored after the cut took sequences %v, want 1 and 2", held)
	}
	unacked := pullSession(t, consumers, session)
	for _, seq := range held {
		requireNext(t, unacked, seq, "the server's reconcile put the consumer's next sequence at 1, below its start")
	}
	unacked.Stop()
	info = waitForAckPending(t, consumers, session, 2)
	t.Logf("with both awaiting an ack the server reports the ack floor as %d, the last delivery as %d and the start sequence as %d; the floor this package reads from that is %d",
		info.AckFloor.Stream, info.Delivered.Stream, info.Config.OptStartSeq, ackFloor(info))

	restarted, err := consumers.RestartAtFloor(ctx, session, filters)
	if err != nil {
		t.Fatalf("restart at floor: %v", err)
	}
	if !restarted.Created || restarted.StartSeq != 1 {
		t.Errorf("RestartAtFloor with both messages awaiting an ack = %+v, want the consumer created again at 1, after the ack floor 0 the server reports", restarted)
	}
	again := pullSession(t, consumers, session)
	for _, seq := range held {
		requireNext(t, again, seq, "the restart starts after the ack floor the server reports, so both messages awaiting an ack are delivered again")
	}
}
