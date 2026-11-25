package delivery

import (
	"context"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
)

// A take-over's start: where an intake creates its durable, the rules that decide it
// (takeOverStart, withinStream) and the one read of the stream they need (readStreamEnds). The
// rules are pure functions of what the server reported, so TestATakeOverStartsAtTheFloorRuleOrTheTip
// holds each row of them without a server, and the read is the one place a take-over learns the
// stream's ends, so TestATakeOverOfADurableThatDeliveredNothingStartsAfterTheStreamsLastMessage
// fails when that read answers nothing.

// streamEnds is the stream's first and last sequence, as one read of it answered (readStreamEnds).
// A stream that has stored nothing reports 0 for both.
type streamEnds struct {
	first, last uint64
}

// readStreamEnds reads the stream's ends for one take-over, and answers false, having logged why,
// when the read failed: the take-over then stops, and the next attempt makes it again. It is read
// after the durable's own state (takeOver), so no sequence the durable reports is past the stream's
// end as this read answers it.
func (d *heldDurable) readStreamEnds(ctx context.Context) (streamEnds, bool) {
	stream, err := d.nats.JS().StreamInfo(bus.Stream, natsgo.Context(ctx))
	if err != nil {
		d.tuning.log.Error("dispatch delivery: read the stream's ends to take NATS consumer over", "name", d.name, "error", err)
		return streamEnds{}, false
	}
	return streamEnds{first: stream.State.FirstSeq, last: stream.State.LastSeq}, true
}

// startPlan is the stream sequence a take-over creates the durable at, and pastEnd the start the
// rules first read when that one lay past the stream's end and was moved back (withinStream), 0
// when none was.
type startPlan struct {
	start, pastEnd uint64
}

// startAt is plan's start, logging at ERROR a start moved back from past the stream's end: the
// stream's sequence fell below what the durable had read, so the stream was cut or created again,
// and what it held between the two is gone.
func (d *heldDurable) startAt(plan startPlan) uint64 {
	if plan.pastEnd != 0 {
		d.tuning.log.Error("dispatch delivery: a NATS consumer's start lay past the stream's end; it is created at the first message the stream holds",
			"name", d.name, "start_seq", plan.pastEnd, "created_at", plan.start)
	}
	return plan.start
}

// takeOverConfig is the config a take-over recreates the durable deployed describes with: the
// deployed config when a newer build stamped it with a higher generation, this build's (wanted)
// otherwise, on a fresh deliver subject, starting at start (takeOverStart). The take-over records
// that start in its hold's token before it deletes the durable, and finishTakeOver says what a
// create that fails after the delete leaves behind. The deployed metadata keeps its own keys and
// drops the ones nats-server sets itself (the `_nats.` prefix), which the server sets again on
// create.
func takeOverConfig(deployed *natsgo.ConsumerInfo, wanted natsgo.ConsumerConfig, start uint64) natsgo.ConsumerConfig {
	config := wanted
	if bus.MetadataGeneration(deployed.Config.Metadata, consumerGenerationKey) > consumerConfigGeneration {
		config = deployed.Config
		config.DeliverSubject = natsgo.NewInbox()
		config.OptStartTime = nil
		config.Metadata = bus.OwnMetadata(deployed.Config.Metadata, 0)
	}
	return startingAt(config, start)
}

// takeOverStart is where a take-over starts the durable deployed, ends being the stream's, and the
// one place the intakes' start rules are written.
//
// A durable with an ack floor starts right after it, the floor the session consumers read
// (bus.AckFloor, over consumerPosition): the recreated durable redelivers at once every message
// the durable had not acknowledged -- what a dead holder had been handed, what a stopped one
// handed back -- replays nothing it had, and skips nothing
// (TestAnAbandonedHoldersMessagesReachTheStandbyWithinTheHoldsLapse, and
// TestATakeOverWhoseCreateFailsCreatesTheDurableAtItsFloorOnTheNextAttempt for a create retried).
// That rule reads the floor the server reports as it is, even below the durable's own start after
// a one-copy stream lost data, so the messages the stream stores after the loss are delivered.
// And it reads nats-server v2.10.29's fresh consumer, which reports both ack floors as 0 until its
// first ack, as acked up to the sequence before its start
// (docs/solutions/envoy/a-fresh-jetstream-consumer-reports-ack-floor-0-so-create-it-at-a-start-sequence-INFRA-594.md):
// read as it is, that floor would restart the durable at the stream's first message and hand the
// new holder the whole retained stream. That is why every create here names an explicit start
// (takeOver, startingAt) rather than leaving DeliverNew to the server: a durable with no start
// sequence has no start for the rule to read. nats-server v2.15.1 reports a fresh consumer's
// delivered stream sequence and ack floor as the sequence before its start (server/consumer.go
// v2.15.1, lines 3594 and 6478), so a test on it sees no difference from that term.
//
// A durable that records no start of its own and that, by its own consumer counts, was delivered
// nothing and acknowledged nothing (startsAtTheTip) starts after the stream's last message, as a
// first create does. That is a durable a build before this one created with no start sequence --
// the events durable of generation 1 at DeliverAll above all -- which no process bound: every
// message the stream holds reached it with nobody reading, and the floor rule would hand the new
// holder all of them. Its stream counts cannot say so, since a DeliverAll consumer starts at the
// stream's first sequence (line 6383) and reports the sequence before it as delivered: 0 on a
// stream whose first message is 1, and more once the first messages have aged out.
//
// A start past the stream's end is moved back to the first message the stream holds
// (withinStream).
func takeOverStart(deployed *natsgo.ConsumerInfo, ends streamEnds) startPlan {
	if startsAtTheTip(deployed) {
		return startPlan{start: ends.last + 1}
	}
	return withinStream(bus.AckFloor(consumerPosition(deployed))+1, ends)
}

// withinStream is start, or, when start lies past the sequence after the stream's last, the first
// message the stream holds, as the session consumers move a start (bus.SessionConsumers'
// pastStreamEnd and firstStart): the stream's sequence fell below the start, because the stream
// was cut or created again, and a durable created past the end would deliver nothing until the
// stream passed its start, since nats-server moves a start back from past the end only when none is
// named. A stream that has stored nothing reports a first sequence of 0, which no consumer starts
// at, so the start is then 1.
func withinStream(start uint64, ends streamEnds) startPlan {
	if start <= ends.last+1 {
		return startPlan{start: start}
	}
	return startPlan{start: min(max(ends.first, 1), ends.last+1), pastEnd: start}
}

// startsAtTheTip reports whether a take-over starts deployed after the stream's last message
// (takeOverStart): it records no start sequence, and deliveredNothing holds of it.
func startsAtTheTip(deployed *natsgo.ConsumerInfo) bool {
	return deployed.Config.OptStartSeq == 0 && deliveredNothing(deployed)
}

// deliveredNothing reports whether, by the durable's own consumer counts, which start at 0 at
// every create, it has been delivered nothing and acknowledged nothing. Its stream sequences cannot
// say so: the server reports the sequence before its start for both (takeOverStart).
func deliveredNothing(info *natsgo.ConsumerInfo) bool {
	return info.Delivered.Consumer == 0 && info.AckFloor.Consumer == 0
}

// consumerPosition is what bus.AckFloor reads of info.
func consumerPosition(info *natsgo.ConsumerInfo) bus.ConsumerPosition {
	return bus.ConsumerPosition{
		AckFloorStream: info.AckFloor.Stream, AckFloorConsumer: info.AckFloor.Consumer,
		OptStartSeq: info.Config.OptStartSeq, DeliveredStream: info.Delivered.Stream,
		NumAckPending: info.NumAckPending,
	}
}

// startingAt is config delivering from stream sequence start on.
func startingAt(config natsgo.ConsumerConfig, start uint64) natsgo.ConsumerConfig {
	config.DeliverPolicy = natsgo.DeliverByStartSequencePolicy
	config.OptStartSeq = start
	return config
}
