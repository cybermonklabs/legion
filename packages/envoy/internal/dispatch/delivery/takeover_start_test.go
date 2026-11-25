package delivery

import (
	"fmt"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A take-over's start, row by row, with no server: each row is a durable as ConsumerInfo reports
// it, or the start a hold's token recorded for a durable that does not exist, the stream's ends,
// and the stream sequence the take-over creates the durable at (takeOverStart, withinStream, which
// say why each row starts where it does). The two tests after it take the durable over on a server,
// through the take-over's one read of the stream's ends (readStreamEnds).
func TestATakeOverStartsAtTheFloorRuleOrTheTip(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name     string
		deployed *natsgo.ConsumerInfo
		recorded uint64
		ends     streamEnds
		want     startPlan
	}{
		{
			// The generation-1 events durable at DeliverAll, created once the stream's first 49
			// messages had aged out, which no process bound: the server reports the sequence
			// before its start as delivered and acked, and its own counts are 0.
			name: "a durable with no start of its own whose own counts are 0, created after the stream's first messages aged out, starts after the stream's last",
			deployed: &natsgo.ConsumerInfo{
				AckFloor: natsgo.SequenceInfo{Stream: 49}, Delivered: natsgo.SequenceInfo{Stream: 49},
			},
			ends: streamEnds{first: 50, last: 100},
			want: startPlan{start: 101},
		},
		{
			name:     "a durable with no start of its own whose own counts are 0, on an empty stream, starts at 1",
			deployed: &natsgo.ConsumerInfo{},
			want:     startPlan{start: 1},
		},
		{
			// nats-server v2.15.1's fresh durable: delivered and acked up to the sequence before
			// its start, its own counts 0.
			name: "a durable created at start 42 that was never delivered a message starts at 42",
			deployed: &natsgo.ConsumerInfo{
				Config:   natsgo.ConsumerConfig{OptStartSeq: 42},
				AckFloor: natsgo.SequenceInfo{Stream: 41}, Delivered: natsgo.SequenceInfo{Stream: 41},
			},
			ends: streamEnds{first: 1, last: 100},
			want: startPlan{start: 42},
		},
		{
			// nats-server v2.10.29's fresh durable: both ack floors 0, delivered the sequence
			// before its start.
			name: "a durable that reports no ack floor and was delivered up to the sequence before its start 42 starts at 42",
			deployed: &natsgo.ConsumerInfo{
				Config:    natsgo.ConsumerConfig{OptStartSeq: 42},
				Delivered: natsgo.SequenceInfo{Stream: 41},
			},
			ends: streamEnds{first: 1, last: 100},
			want: startPlan{start: 42},
		},
		{
			// A durable this build created on a stream that was empty at the first bind: what the
			// stream stored since is its own.
			name:     "a durable created at start 1 that was never delivered a message starts at 1",
			deployed: &natsgo.ConsumerInfo{Config: natsgo.ConsumerConfig{OptStartSeq: 1}},
			ends:     streamEnds{first: 1, last: 12},
			want:     startPlan{start: 1},
		},
		{
			name: "a durable with no start of its own that was delivered messages it did not acknowledge starts at the first of them",
			deployed: &natsgo.ConsumerInfo{
				Delivered: natsgo.SequenceInfo{Stream: 3, Consumer: 3}, NumAckPending: 3,
			},
			ends: streamEnds{first: 1, last: 12},
			want: startPlan{start: 1},
		},
		{
			name: "a durable with an ack floor and messages awaiting an ack starts right after the floor",
			deployed: &natsgo.ConsumerInfo{
				Config:   natsgo.ConsumerConfig{OptStartSeq: 2},
				AckFloor: natsgo.SequenceInfo{Stream: 30, Consumer: 10}, Delivered: natsgo.SequenceInfo{Stream: 33, Consumer: 13},
				NumAckPending: 3,
			},
			ends: streamEnds{first: 1, last: 100},
			want: startPlan{start: 31},
		},
		{
			// A one-copy stream that lost data: the server reports the floor at the stream's last
			// sequence, below the durable's start, and the messages it stores next are the durable's.
			name: "a durable whose stream lost data starts right after the floor it reports, below its start",
			deployed: &natsgo.ConsumerInfo{
				Config:   natsgo.ConsumerConfig{OptStartSeq: 42},
				AckFloor: natsgo.SequenceInfo{Stream: 20, Consumer: 5}, Delivered: natsgo.SequenceInfo{Stream: 20, Consumer: 5},
			},
			ends: streamEnds{first: 1, last: 20},
			want: startPlan{start: 21},
		},
		{
			// A one-copy stream that lost every message, with a durable that had delivered nothing:
			// nats-server v2.15.1 leaves it at the floor it had before the loss, past the stream's
			// end, where the stream stores its next messages at 1 and 2.
			name: "a durable whose floor lies past the stream's end starts at the first message the stream holds",
			deployed: &natsgo.ConsumerInfo{
				Config:   natsgo.ConsumerConfig{OptStartSeq: 21},
				AckFloor: natsgo.SequenceInfo{Stream: 20}, Delivered: natsgo.SequenceInfo{Stream: 20},
			},
			ends: streamEnds{first: 1, last: 2},
			want: startPlan{start: 1, pastEnd: 21},
		},
		{
			// A durable lost while its intake read its floor at 500 (check), on a stream created
			// again since, which now holds 20 messages.
			name:     "a recorded start past the stream's end moves back to the first message the stream holds",
			recorded: 501,
			ends:     streamEnds{first: 1, last: 20},
			want:     startPlan{start: 1, pastEnd: 501},
		},
		{
			name:     "a recorded start within the stream is kept",
			recorded: 7,
			ends:     streamEnds{first: 1, last: 20},
			want:     startPlan{start: 7},
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			var got startPlan
			if row.deployed != nil {
				got = takeOverStart(row.deployed, row.ends)
				created := takeOverConfig(row.deployed, deliveryConsumerConfig(tuningFor(t)), got.start)
				if created.DeliverPolicy != natsgo.DeliverByStartSequencePolicy || created.OptStartSeq != got.start {
					t.Fatalf("the take-over creates the durable with deliver policy %v at start %d, want an explicit start at %d",
						created.DeliverPolicy, created.OptStartSeq, got.start)
				}
			} else {
				got = withinStream(row.recorded, row.ends)
			}
			if got != row.want {
				t.Fatalf("the take-over starts the durable at %d (moved back from %d), want %d (moved back from %d)",
					got.start, got.pastEnd, row.want.start, row.want.pastEnd)
			}
		})
	}
}

// A take-over of a durable that records no start of its own and was never delivered a message --
// the generation-1 events durable at DeliverAll that no process bound -- starts after the stream's
// last message, as the take-over read it (readStreamEnds). The stream's first message has aged out
// (a purge) before the durable is created, so the server reports the durable as delivered and
// acknowledged up to the sequence before the stream's first, not 0, and only its own counts say it
// was delivered nothing. The two envelopes the stream holds, one published before the durable was
// created and one after, get no head row, and one published after the take-over does. The table
// above passes the stream's ends itself; this test fails when the take-over reads them wrong.
func TestATakeOverOfADurableThatDeliveredNothingStartsAfterTheStreamsLastMessage(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	key := seedHeadIssue(t, ctx, pool, 62)
	natsClient := intakeTestClient(t)

	const agedOut, beforeCreate, afterCreate, afterTakeOver = 6200, 6201, 6202, 6203
	purged := publishPullRequestEnvelope(t, natsClient, "acme", "widgets", agedOut, openedForKey(agedOut, key, "6200620062006200620062006200620062006200"))
	if err := natsClient.JS().PurgeStream(bus.Stream, &natsgo.StreamPurgeRequest{Sequence: purged + 1}); err != nil {
		t.Fatalf("purge the stream's first message: %v", err)
	}
	first := publishPullRequestEnvelope(t, natsClient, "acme", "widgets", beforeCreate, openedForKey(beforeCreate, key, "6201620162016201620162016201620162016201"))
	old, err := natsClient.JS().AddConsumer(bus.Stream, &natsgo.ConsumerConfig{
		Durable:        bus.DispatchDeliveryDurable,
		DeliverSubject: natsgo.NewInbox(),
		FilterSubject:  githubIntakeSubject,
		AckPolicy:      natsgo.AckExplicitPolicy,
	})
	if err != nil {
		t.Fatalf("create the generation-1 durable at DeliverAll: %v", err)
	}
	if old.Delivered.Stream != first-1 || old.Delivered.Consumer != 0 || old.AckFloor.Consumer != 0 {
		t.Fatalf("control: the server reports the generation-1 durable as delivered stream %d consumer %d, ack floor consumer %d; want stream %d, the sequence before the stream's first, and both own counts 0",
			old.Delivered.Stream, old.Delivered.Consumer, old.AckFloor.Consumer, first-1)
	}
	last := publishPullRequestEnvelope(t, natsClient, "acme", "widgets", afterCreate, openedForKey(afterCreate, key, "6202620262026202620262026202620262026202"))

	startIntake(t, newIntake(tuningFor(t), natsClient, pool, deliveryTestLeases(t, pool), newFakeGitHub(t).newTestClient(), events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed()))
	taken := awaitPushBound(t, natsClient, old.Config.DeliverSubject, "the intake never took the generation-1 durable over")
	if taken.Config.OptStartSeq != last+1 {
		t.Fatalf("the take-over created the durable at %d, want %d, the sequence after the stream's last", taken.Config.OptStartSeq, last+1)
	}
	publishPullRequestEnvelope(t, natsClient, "acme", "widgets", afterTakeOver, openedForKey(afterTakeOver, key, "6203620362036203620362036203620362036203"))
	awaitAcknowledged(t, natsClient)
	if rows := headRows(t, ctx, pool, afterTakeOver); rows != 1 {
		t.Fatalf("%s#%d, opened after the take-over, has %d head rows, want 1", deliveryRepo, afterTakeOver, rows)
	}
	for _, number := range []int{agedOut, beforeCreate, afterCreate} {
		if rows := headRows(t, ctx, pool, number); rows != 0 {
			t.Fatalf("%s#%d, which the stream held before the take-over, has %d head rows, want 0: the take-over replayed what the durable never delivered", deliveryRepo, number, rows)
		}
	}
}

// A take-over of a durable whose floor lies past the stream's end creates it at the first message
// the stream holds, and says so at ERROR. The intake creates the durable at 21 on a one-copy stream
// of 20 messages and stops, the durable having delivered nothing. The stream's whole first message
// block is then cut (testnats CutStream), so the restarted server holds no message and, by
// nats-server v2.15.1's reconcile, leaves the durable at its floor of 20, past the stream's end. Two
// pull requests are opened after the cut, at 1 and 2. A second intake takes the durable over at 1,
// so both get their head rows; created at its floor plus one, 21, it would deliver nothing until the
// stream passed 20.
func TestATakeOverOfADurableWhoseFloorLiesPastTheStreamsEndStartsAtItsFirstMessage(t *testing.T) {
	t.Parallel()
	logs := captureIntakeLogs(t)
	pool, ctx := deliveryTestPool(t)
	key := seedHeadIssue(t, ctx, pool, 63)
	srv, uri := testnats.Start(t)
	connect := func() *bus.Client {
		client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
		if err != nil {
			t.Fatalf("connect NATS: %v", err)
		}
		t.Cleanup(client.Close)
		return client
	}
	publisher := connect()

	const before = 20
	for i := range before {
		publishGitHubEnvelope(t, publisher, fmt.Sprintf("notifications.github.acme.widgets.pr.%d.comment", 6300+i), `{"kind":"comment"}`)
	}
	stopFirst := runIntake(t, newIntake(tuningFor(t), connect(), pool, deliveryTestLeases(t, pool), newFakeGitHub(t).newTestClient(), events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed()))
	first := awaitPushBound(t, publisher, "", "the first intake never bound its durable")
	if first.Config.OptStartSeq != before+1 {
		t.Fatalf("the first intake created the durable at %d, want %d", first.Config.OptStartSeq, before+1)
	}
	stopFirst()

	srv.CutStream(t, bus.Stream, func(int64) int64 { return 0 })
	testwait.Eventually(t, "the publisher to reconnect", publisher.Connected)
	var stored uint64
	testwait.UntilPolling(t, 100*time.Millisecond, "the stream to answer a state read after the cut and the restart", func() (string, bool) {
		stream, err := publisher.JS().StreamInfo(bus.Stream)
		if err == nil {
			stored = stream.State.LastSeq
		}
		return fmt.Sprint(err), err == nil
	})
	if stored != 0 {
		t.Fatalf("the stream's last sequence after the whole block was cut is %d, want 0", stored)
	}
	const firstAfter, secondAfter = 6321, 6322
	publishPullRequestEnvelope(t, publisher, "acme", "widgets", firstAfter, openedForKey(firstAfter, key, "6321632163216321632163216321632163216321"))
	publishPullRequestEnvelope(t, publisher, "acme", "widgets", secondAfter, openedForKey(secondAfter, key, "6322632263226322632263226322632263226322"))
	cut, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchDeliveryDurable)
	if err != nil {
		t.Fatalf("read the durable after the cut: %v", err)
	}
	if cut.AckFloor.Stream <= 2 {
		t.Fatalf("control: after the cut the server reports the durable's ack floor as %d, want it above the stream's last sequence 2: the durable this case is for is the one nats-server v2.15.1 leaves at its floor of %d", cut.AckFloor.Stream, before)
	}

	startIntake(t, newIntake(tuningFor(t), connect(), pool, deliveryTestLeases(t, pool), newFakeGitHub(t).newTestClient(), events.NewBroker(), loadedSettingsCache(t, ctx, pool), unrenamed()))
	taken := awaitPushBound(t, publisher, first.Config.DeliverSubject, "the second intake never took the durable over")
	for _, number := range []int{firstAfter, secondAfter} {
		waitFor(t, 20*time.Second, func() bool { return headRows(t, ctx, pool, number) == 1 }, func() string {
			return fmt.Sprintf("%s#%d, opened after the cut, has %d head rows 20s after the take-over created the durable at %d, want 1",
				deliveryRepo, number, headRows(t, ctx, pool, number), taken.Config.OptStartSeq)
		})
	}
	if taken.Config.OptStartSeq != 1 {
		t.Fatalf("the take-over created the durable at %d, want 1, the first message the stream holds", taken.Config.OptStartSeq)
	}
	said := false
	for _, record := range logs() {
		if record["level"] == "ERROR" && record["name"] == bus.DispatchDeliveryDurable && record["created_at"] == float64(1) {
			said = true
		}
	}
	if !said {
		t.Fatalf("the take-over said nothing at ERROR of the start it moved back from past the stream's end: %v", logs())
	}
}
