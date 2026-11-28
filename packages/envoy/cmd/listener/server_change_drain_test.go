package main

import (
	"slices"
	"testing"

	natsgo "github.com/nats-io/nats.go"
)

// consumerDrained reports whether info, one reading of the stand-in's session consumer after the
// senders stopped (serverChangeSoak.awaitConsumerDrained), shows the consumer drained: nothing
// pending and nothing awaiting an ack, in a state the server holds for it.
//
// Two answers show the delivered and ack-floor sequences as 0. A member of the consumer's group
// that holds its assignment but has no replication node for it yet answers with the consumer's
// configuration and default state, every sequence and every count 0 (nats-server v2.15.1,
// server/jetstream_api.go, jsConsumerInfoRequest, lines 5686-5702). A preferred member of a new
// group that has had no leader answers from its own store (lines 5719-5721), and the store of a
// consumer of more than one copy holds no state until a leader proposes one (server/consumer.go,
// selectStartingSeqNo). Both come just after the relay deletes the consumer and creates it again
// at its ack floor (RestartAtFloor), and while a restarted server rejoins, so such a reading can
// show nothing waiting while messages still wait.
//
// A real reading at the drain shows a delivered stream sequence above 0. Every session consumer
// is created at a start sequence (OptStartSeq), and its leader reports the sequence before that
// start, or a later one, as delivered (server/consumer.go, selectStartingSeqNo and
// infoWithSnapAndReply). A consumer created at sequence 1 can report 0 only until it delivers its
// first message, and the senders stored messages on its filter before the drain, so it reports
// them as delivered or as pending. A reading whose delivered and ack-floor stream sequences are
// both 0 is the default state, then, and the wait reads again.
func consumerDrained(info *natsgo.ConsumerInfo) bool {
	defaultState := info.Delivered.Stream == 0 && info.AckFloor.Stream == 0
	return !defaultState && info.NumPending == 0 && info.NumAckPending == 0
}

// The server-change soak's drain wait (serverChangeSoak.awaitConsumerDrained) reads past a reading
// of the consumer's default state (consumerDrained), through a reading with messages still
// waiting, to the reading of the consumer drained, and stops there.
func TestTheServerChangeDrainReadsPastAConsumerInfoWithDefaultState(t *testing.T) {
	t.Parallel()
	byStart := func(start uint64) natsgo.ConsumerConfig {
		return natsgo.ConsumerConfig{DeliverPolicy: natsgo.DeliverByStartSequencePolicy, OptStartSeq: start}
	}
	stream := func(seq uint64) natsgo.SequenceInfo { return natsgo.SequenceInfo{Stream: seq} }
	cases := []struct {
		name string
		// readings are in the order the wait reads them, the first the default state and the last
		// the consumer drained.
		readings []natsgo.ConsumerInfo
	}{
		{
			name: "a consumer the relay created again past sequence 1",
			readings: []natsgo.ConsumerInfo{
				{Config: byStart(412)},
				{Config: byStart(412), Delivered: stream(411), AckFloor: stream(411), NumPending: 170},
				{Config: byStart(412), Delivered: stream(581), AckFloor: stream(581)},
			},
		},
		{
			name: "a consumer created at sequence 1, read while a restarted server rejoins",
			readings: []natsgo.ConsumerInfo{
				{Config: byStart(1)},
				{Config: byStart(1), Delivered: stream(300), AckFloor: stream(250), NumAckPending: 50, NumPending: 20},
				{Config: byStart(1), Delivered: stream(370), AckFloor: stream(370)},
			},
		},
		{
			name: "a consumer the relay created again past every message it was sent",
			readings: []natsgo.ConsumerInfo{
				{Config: byStart(582)},
				{Config: byStart(582), Delivered: stream(581), AckFloor: stream(581)},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			stop := slices.IndexFunc(c.readings, func(info natsgo.ConsumerInfo) bool { return consumerDrained(&info) })
			want := len(c.readings) - 1
			switch {
			case stop < 0:
				t.Errorf("the drain wait stops at no reading, want reading %d of %d, the consumer drained", want+1, len(c.readings))
			case stop != want:
				r := c.readings[stop]
				t.Errorf("the drain wait stops at reading %d of %d (start %d, delivered %d, ack floor %d, %d pending, %d awaiting an ack), want reading %d, the consumer drained",
					stop+1, len(c.readings), r.Config.OptStartSeq, r.Delivered.Stream, r.AckFloor.Stream, r.NumPending, r.NumAckPending, want+1)
			}
		})
	}
}
