package delivery

import (
	"testing"

	natsgo "github.com/nats-io/nats.go"
)

// A take-over's start, row by row, with no server: each row is a durable as ConsumerInfo reports
// it, and the stream sequence the take-over recreates it at (takeOverConfig, takeOverStart). The
// rows hold what no test against nats-server v2.15.1 can tell apart, since that server reports a
// fresh durable's ack floor as the sequence before its start: the floor of v2.10.29's fresh durable,
// the floor a one-copy stream's data loss leaves below the start, and the durable with no start of
// its own that nobody bound.
func TestATakeOverStartsAtTheFloorRuleOrTheTip(t *testing.T) {
	t.Parallel()
	const streamLast = 100
	for _, row := range []struct {
		name     string
		deployed natsgo.ConsumerInfo
		want     uint64
	}{
		{
			// The generation-1 events durable at DeliverAll, which no process bound: the floor plus
			// one would be the stream's first message.
			name:     "a durable with no start of its own that was never delivered a message starts after the stream's last",
			deployed: natsgo.ConsumerInfo{},
			want:     streamLast + 1,
		},
		{
			// A durable this build created on a stream that was empty at the first bind, and that
			// nothing was delivered to before the take-over: what the stream stored since is its own.
			name:     "a durable created at start 1 that was never delivered a message starts at 1",
			deployed: natsgo.ConsumerInfo{Config: natsgo.ConsumerConfig{OptStartSeq: 1}},
			want:     1,
		},
		{
			name: "a durable with no start of its own that was delivered messages it did not acknowledge starts at the first of them",
			deployed: natsgo.ConsumerInfo{
				Delivered: natsgo.SequenceInfo{Stream: 3, Consumer: 3}, NumAckPending: 3,
			},
			want: 1,
		},
		{
			// nats-server v2.10.29's fresh durable: both ack floors 0, delivered the sequence before
			// its start.
			name: "a durable that reports no ack floor and was delivered up to the sequence before its start starts at its start",
			deployed: natsgo.ConsumerInfo{
				Config:    natsgo.ConsumerConfig{OptStartSeq: 42},
				Delivered: natsgo.SequenceInfo{Stream: 41},
			},
			want: 42,
		},
		{
			name: "a durable with an ack floor and messages awaiting an ack starts right after the floor",
			deployed: natsgo.ConsumerInfo{
				Config:   natsgo.ConsumerConfig{OptStartSeq: 2},
				AckFloor: natsgo.SequenceInfo{Stream: 30, Consumer: 10}, Delivered: natsgo.SequenceInfo{Stream: 33, Consumer: 13},
				NumAckPending: 3,
			},
			want: 31,
		},
		{
			// A one-copy stream that lost data: the server reports the floor at the stream's last
			// sequence, below the durable's start, and the messages it stores next are the durable's.
			name: "a durable whose stream lost data starts right after the floor it reports, below its start",
			deployed: natsgo.ConsumerInfo{
				Config:   natsgo.ConsumerConfig{OptStartSeq: 42},
				AckFloor: natsgo.SequenceInfo{Stream: 20, Consumer: 5}, Delivered: natsgo.SequenceInfo{Stream: 20, Consumer: 5},
			},
			want: 21,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			created := takeOverConfig(&row.deployed, deliveryConsumerConfig(tuningFor(t)), streamLast)
			if created.DeliverPolicy != natsgo.DeliverByStartSequencePolicy || created.OptStartSeq != row.want {
				t.Fatalf("the take-over creates the durable with deliver policy %v at start %d, want an explicit start at %d",
					created.DeliverPolicy, created.OptStartSeq, row.want)
			}
		})
	}
}
