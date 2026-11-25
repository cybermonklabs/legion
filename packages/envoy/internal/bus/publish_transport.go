// The publish transport: its judgement of a publish's transport (PublishTakesJetStream) and the
// publishes on that answer (PublishAckWithin, PublishEachWithin, PublishAckOn).

package bus

import (
	"context"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
)

// PublishTakesJetStream reports whether a publish of topic takes JetStream rather than core NATS:
// the judgement PublishReportingDuplicateWithin makes of its transport, for a caller that decides
// something else of the publish by it too and then publishes on that one answer (PublishAckOn). A
// topic the subjects the client holds leave core (UsesCoreTransport) takes JetStream once a read
// of the subjects again captures it (rereadForTransport), and keeps core NATS on the subjects the
// client holds wherever that read is not made: inside streamSubjectsRereadGap of the last such
// read's end, while a read of them is in flight and started within firstStreamSubjectsRead, on a
// client with no connection or whose NATS user may not read the stream, and when the read fails or
// ctx ends first. A role lane and its exception lane read nothing and keep core NATS. A topic this
// build stores takes JetStream whatever the subjects the client holds say, with no read here: it
// never takes core NATS, and PublishAckOn refuses it only on a read of its own (publishPathOf,
// undeployedOnFreshRead).
//
// It is not a refusal: a core publish of a topic the stream captures is stored too, under no
// MsgId and with no acknowledgement, so it waits out neither the gap nor a read in flight that
// started within firstStreamSubjectsRead, where a fresh judgement waits for both
// (SubjectsAsk.Uncaptured). A core publish of a topic no stream captures - a
// delivery exception on a github, dispatch, slack or legion lane, an ad-hoc topic - would
// otherwise wait out a read the server leaves unanswered, as during a JetStream leader change, for
// up to firstStreamSubjectsRead, and taking JetStream for such a topic on no read would wait out
// the publish's whole deadline for an acknowledgement no stream sends. A read in flight past
// firstStreamSubjectsRead, as long as the read itself may last, is joined, and once it has ended
// the next judgement past streamSubjectsRereadGap starts a read of its own. So a direct publish
// in the roll window inside the gap or during an unanswered read goes over core NATS, stored, with
// no MsgId and no acknowledgement.
func (c *Client) PublishTakesJetStream(ctx context.Context, topic string) bool {
	if !c.UsesCoreTransport(topic) {
		return true
	}
	if c == nil || subjectsCapture(roleLaneSubjects, topic) {
		return false
	}
	c.rereadForTransport(ctx)
	return c.DeployedStreamCaptures(topic)
}

// rereadForTransport is a publish transport's read of the deployed subjects again
// (PublishTakesJetStream): it starts one on the connection the client is connected over unless
// the last ended within streamSubjectsRereadGap, joins a read in flight only once that read has
// outlasted firstStreamSubjectsRead, and waits for the read it starts or joins to end or for ctx
// to. It reads nothing on a client whose NATS user may not read the stream (subjectsRefused).
func (c *Client) rereadForTransport(ctx context.Context) {
	if c.subjectsRefused.Load() {
		return
	}
	c.subjectsRereadMu.Lock()
	reading := c.subjectsRereading
	switch {
	case reading != nil && time.Since(c.subjectsRereadStarted) < firstStreamSubjectsRead:
		c.subjectsRereadMu.Unlock()
		return
	case reading == nil:
		conn := c.ConnectedConn()
		if conn == nil || time.Since(c.subjectsRereadAt) < streamSubjectsRereadGap {
			c.subjectsRereadMu.Unlock()
			return
		}
		reading, _ = c.startRereadLocked(conn)
	}
	c.subjectsRereadMu.Unlock()
	select {
	case <-reading:
	case <-ctx.Done():
	}
}

// PublishEachWithin publishes item on its own topic as PublishAckWithin does, and once the
// publish succeeds calls published, when it is not nil, with that topic and the stream's
// acknowledgement: its duplicate verdict, and whether the verdict answers a resend
// (StoredAck.Resent), which can make a duplicate this call's own first copy. It is the webhook
// handlers' publisher (webhook.Publisher), which a publisher that also puts copies on other
// topics implements by calling published once for each topic it published on
// (ghrename.Publisher).
func (c *Client) PublishEachWithin(ctx context.Context, item contracts.Envelope, published func(topic string, duplicate, resent bool)) error {
	ack, err := c.PublishAckWithin(ctx, item)
	if err != nil {
		return err
	}
	if published != nil {
		published(item.Topic, ack.Duplicate, ack.Resent)
	}
	return nil
}

// PublishAckWithin publishes item on its own topic as PublishReportingDuplicateWithin does, over
// the same two transports and within ctx's bound, and returns the stream's whole acknowledgement
// rather than its duplicate verdict alone: the sequence, the verdict, and whether the verdict
// answers a resend (StoredAck.Resent), which can make a duplicate this call's own first copy. A
// core-transport topic has no acknowledgement, so it answers the zero StoredAck beside
// PublishCoreContext's error. The listener's send route publishes this way: its sender limit
// charges nothing for a copy the stream already held, and one for a copy this call may have
// stored (cmd/listener's senderCharge.published). It is the transport's judgement
// (PublishTakesJetStream) followed by the publish on the transport it chose (PublishAckOn).
//
// The path is publishPathOf's, the one the other four publish functions take through this one. A
// topic this build stores goes through JetStream while the deployed stream captures it and is
// never sent over core NATS: while the deployed stream does not capture it, as a read of its
// subjects that started after this call confirms, this returns the zero StoredAck and an error
// wrapping ErrSubjectNotDeployed before anything is sent, so no core subscriber is handed a copy
// the stream does not hold and no sender is answered as if it were stored (PublishAckOn).
func (c *Client) PublishAckWithin(ctx context.Context, item contracts.Envelope) (StoredAck, error) {
	return c.PublishAckOn(ctx, item, c.PublishTakesJetStream(ctx, item.Topic))
}

// PublishAckOn publishes item on its own topic as PublishAckWithin does, within ctx's bound, on
// the transport its caller decided rather than one it judges itself: JetStream when jetStream,
// answering the stream's whole acknowledgement, and core NATS otherwise (PublishCoreContext),
// answering the zero StoredAck. It is for a caller that decides something else of the publish by
// the same judgement (PublishTakesJetStream), so the two cannot read the deployed subjects apart:
// the listener's publish route counts the message's recipients by the answer it publishes on
// (cmd/listener's recipientCounts), and charges the sender as the send route does.
//
// It reads the subjects for one case alone, before it sends anything: a topic this build stores
// that the subjects the client holds leave uncaptured, which PublishTakesJetStream answers
// JetStream for. That publish is refused with ErrSubjectNotDeployed once a read of the subjects
// that started after this call leaves the topic uncaptured too, and goes through JetStream
// otherwise, the read that captures it and the read that could not be made alike
// (undeployedOnFreshRead): the subjects the client holds can be a minute old, and a refusal on
// them alone would reject, during a roll, a message the newer build's stream stores.
func (c *Client) PublishAckOn(ctx context.Context, item contracts.Envelope, jetStream bool) (StoredAck, error) {
	if c.undeployedOnFreshRead(ctx, c.AskDeployedSubjects(), item.Topic) {
		return StoredAck{}, subjectNotDeployed(item.Topic)
	}
	if !jetStream {
		return StoredAck{}, c.PublishCoreContext(ctx, item.Topic, item)
	}
	return c.publishJetStream(ctx, item)
}
