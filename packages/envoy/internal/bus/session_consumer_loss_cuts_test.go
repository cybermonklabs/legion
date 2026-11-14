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

// lossCutBefore is how many messages a loss cut's stream holds when the session consumer is
// created (N), all on a subject the consumer does not follow, so the consumer starts at N + 1.
const lossCutBefore = 20

// The three conditions of ackFloor's OptStartSeq - 1 term, by the names a loss cut pins them by. No
// cut pins deliveredPastStart: the reconciled consumer it guards, both floors at 0, is a state
// nats-server v2.15.1 does not persist (ackFloor), so a cut can only require it to hold.
const (
	noStreamFloor      = "AckFloor.Stream == 0"
	noConsumerFloor    = "AckFloor.Consumer == 0"
	deliveredPastStart = "Delivered.Stream >= OptStartSeq - 1"
)

// lossCut is one loss of a one-copy stream's data under a session consumer, which runLossCut makes.
type lossCut struct {
	// session is the session whose consumer the cut is made under.
	session string
	// acked is how many messages the consumer is delivered and acks before the loss (M).
	acked int
	// whole cuts the stream's first message block to nothing, so the stream keeps no message (L =
	// 0); otherwise the block is cut to half its size plus one byte, and the stream keeps 1 to N - 2.
	whole bool
	// pending is how many messages the stream stores after the loss and the consumer holds unacked
	// when it is restarted (K), given the stream's last sequence after the loss (L).
	pending func(lost uint64) int
	// pins is the one condition of ackFloor's OptStartSeq - 1 term that the consumer, with those
	// messages awaiting an ack, fails, so it alone keeps a restart from starting past them.
	pins string
}

// runLossCut makes cut: it creates a session consumer at N + 1 on a one-copy stream holding N
// messages, has it deliver and ack M messages, cuts the stream's data with the server stopped, as
// TestRestartAtFloorAfterAOneCopyStreamLostDataDeliversThePendingMessagesAgain does, and requires
// nats-server v2.15.1 to reconcile the consumer to the stream's last sequence L. The stream then
// stores K messages, which take L + 1 to L + K and which the consumer delivers and holds unacked.
// With them awaiting an ack, the consumer must fail the one condition of ackFloor's
// OptStartSeq - 1 term the cut pins and meet the other two, and a restart at its floor
// (RestartAtFloor) must create it again at L + 1, after the ack floor the server reports, so all K
// are delivered again in stream order.
func runLossCut(t *testing.T, cut lossCut) {
	t.Helper()
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
	const before = lossCutBefore
	name, _ := SessionConsumerName(cut.session)
	agent := contracts.AgentSubject(cut.session)
	filters := []string{agent}

	for i := range before {
		publishStored(t, owner, contracts.AgentSubject(cut.session+"_bystander"), fmt.Sprintf("before %02d", i))
	}
	start, err := consumers.Ensure(ctx, cut.session, filters, 0)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !start.Created || start.StartSeq != before+1 {
		t.Fatalf("Ensure of a session the stream holds nothing for = %+v, want the consumer created at %d, the sequence after the stream's last", start, before+1)
	}
	if cut.acked > 0 {
		pull := pullSession(t, consumers, cut.session)
		for i := range cut.acked {
			seq := publishStored(t, owner, agent, fmt.Sprintf("acked %02d", i))
			if err := requireNext(t, pull, seq, "the consumer delivers its agent subject's messages in stream order").Ack(); err != nil {
				t.Fatalf("ack %d: %v", seq, err)
			}
		}
		pull.Stop()
	}
	// nats-server v2.15.1 reports the floor of a consumer that has acked nothing as the sequence
	// before its start, N.
	waitForAckFloor(t, consumers, cut.session, uint64(before+cut.acked))
	waitForAckPending(t, consumers, cut.session, 0)

	srv.Stop(t)
	messages := filepath.Join(srv.StoreDir(), "jetstream", "$G", "streams", Stream, "msgs")
	block, index := filepath.Join(messages, "1.blk"), filepath.Join(messages, "index.db")
	stat, err := os.Stat(block)
	if err != nil {
		t.Fatalf("the stream's first message block: %v", err)
	}
	size := stat.Size()/2 + 1
	if cut.whole {
		size = 0
	}
	if err := os.Truncate(block, size); err != nil {
		t.Fatalf("cut the stream's first message block: %v", err)
	}
	if err := os.Remove(index); err != nil {
		t.Fatalf("remove the stream's index: %v", err)
	}
	t.Logf("cut %s from %d bytes to %d and removed %s", block, stat.Size(), size, index)
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
	t.Logf("after the cut the stream's last sequence is %d, where it was %d", lost, before+cut.acked)
	if cut.whole && lost != 0 {
		t.Fatalf("the stream's last sequence after the whole block was cut is %d, want 0", lost)
	}
	if !cut.whole && (lost == 0 || lost > before-2) {
		t.Fatalf("the stream's last sequence after half the block was cut is %d, want 1 to %d, below the consumer's start %d less 2", lost, before-2, before+1)
	}
	reconcile := fmt.Sprintf("JetStream consumer '$G > %s > %s' delivered sequence %d past last stream sequence of %d", Stream, name, before+cut.acked, lost)
	if log := srv.Log(); !strings.Contains(log, reconcile) {
		var named []string
		for line := range strings.Lines(log) {
			if strings.Contains(line, name) {
				named = append(named, line)
			}
		}
		t.Fatalf("the server logged no %q; want its reconcile of the consumer whose delivered sequence is past the stream's end. Its lines naming %s:\n%s", reconcile, name, strings.Join(named, ""))
	}
	info, err := consumers.Info(ctx, cut.session)
	if err != nil {
		t.Fatalf("info after the restart: %v", err)
	}
	t.Logf("the server reconciled the consumer: it reports the ack floor as stream %d consumer %d, the last delivery as %d and the start sequence as %d",
		info.AckFloor.Stream, info.AckFloor.Consumer, info.Delivered.Stream, info.Config.OptStartSeq)

	count := cut.pending(lost)
	held := make([]uint64, count)
	for i := range held {
		held[i] = publishStored(t, owner, agent, fmt.Sprintf("held %02d", i))
		if held[i] != lost+uint64(i)+1 {
			t.Fatalf("message %d stored after the cut took sequence %d, want %d, after the stream's last %d", i, held[i], lost+uint64(i)+1, lost)
		}
	}
	unacked := pullSession(t, consumers, cut.session)
	for _, seq := range held {
		requireNext(t, unacked, seq, "the server's reconcile put the consumer's next sequence after the stream's last, below its start")
	}
	unacked.Stop()
	info = waitForAckPending(t, consumers, cut.session, count)
	t.Logf("with %d awaiting an ack the server reports the ack floor as stream %d consumer %d, the last delivery as %d and the start sequence as %d; the floor this package reads from that is %d",
		count, info.AckFloor.Stream, info.AckFloor.Consumer, info.Delivered.Stream, info.Config.OptStartSeq, ackFloor(info))
	for condition, holds := range map[string]bool{
		noStreamFloor:      info.AckFloor.Stream == 0,
		noConsumerFloor:    info.AckFloor.Consumer == 0,
		deliveredPastStart: info.Delivered.Stream+1 >= info.Config.OptStartSeq,
	} {
		if pinned := condition == cut.pins; holds == pinned {
			t.Fatalf("with %d awaiting an ack, %s is %t for the consumer; want only %s false, so the cut pins that condition", count, condition, holds, cut.pins)
		}
	}

	restarted, err := consumers.RestartAtFloor(ctx, cut.session, filters)
	if err != nil {
		t.Fatalf("restart at floor: %v", err)
	}
	if !restarted.Created || restarted.StartSeq != lost+1 {
		t.Errorf("RestartAtFloor with %d messages awaiting an ack = %+v, want the consumer created again at %d, after the ack floor %d the server reports", count, restarted, lost+1, lost)
	}
	again := pullSession(t, consumers, cut.session)
	for _, seq := range held {
		requireNext(t, again, seq, "the restart starts after the ack floor the server reports, so every message awaiting an ack is delivered again")
	}
}

// A consumer that had delivered and acked M = 4 messages before its one-copy stream lost every
// message (L = 0) is reconciled with a stream floor of 0 and a consumer floor of 4, the consumer
// sequence it had delivered. Delivered N + 5 messages since the loss, from sequence 1 up to and
// past the sequence before its start, and holding them all unacked, it meets ackFloor's conditions
// AckFloor.Stream == 0 and Delivered.Stream >= OptStartSeq - 1, as the nats-server v2.10.29
// consumer the term is for does, and fails only AckFloor.Consumer == 0. That condition pins it: a
// term without it reads the floor as N and restarts the consumer past the N messages from 1 to N.
func TestRestartAtFloorAfterEveryMessageWasLostRedeliversPastTheStartWhenTheConsumerHadDelivered(t *testing.T) {
	runLossCut(t, lossCut{
		session: "ses_cut_whole_delivered",
		acked:   4,
		whole:   true,
		pending: func(uint64) int { return lossCutBefore + 5 },
		pins:    noConsumerFloor,
	})
}

// A consumer that had delivered nothing (M = 0) before its one-copy stream lost the end of its data
// (L >= 1) is reconciled with a stream floor of L and a consumer floor of 0. Delivered N - L + 5
// messages since the loss, from L + 1 up to and past the sequence before its start, and holding
// them all unacked, it meets ackFloor's conditions AckFloor.Consumer == 0 and
// Delivered.Stream >= OptStartSeq - 1 and fails only AckFloor.Stream == 0. That condition pins it:
// a term without it reads the floor as N and restarts the consumer past the messages from L + 1 to
// N.
func TestRestartAtFloorAfterTheEndWasLostRedeliversPastTheStartWhenTheConsumerHadDeliveredNothing(t *testing.T) {
	runLossCut(t, lossCut{
		session: "ses_cut_end_nothing",
		pending: func(lost uint64) int { return lossCutBefore - int(lost) + 5 },
		pins:    noStreamFloor,
	})
}
