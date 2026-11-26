package intake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"golang.org/x/sync/errgroup"

	"github.com/sjawhar/envoy/listenerwire"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// Handoff is one message the listener handed off, the listener's own body for it
// (listenerwire.LegionHandoff): the topic it was for (the agent's own,
// notifications.agent.<session> or its form under a project, notifications.agent.<PROJECT or
// _>.<session>, or its role's, notifications.role.<token>), the agent session it is for (the
// session addressed, or the role's newest, whichever form the topic took), that session's role
// token, the Legion project token, and the Envoy envelope exactly as it would have been delivered
// to the agent.
type Handoff = listenerwire.LegionHandoff

// HandoffEnvelope is the Envoy envelope a hand-off carries (listenerwire.Envelope, EnvelopeSchema
// in packages/contracts/src/envelope.ts): who sent it (Source, SourceSession, Sender), its identity
// (EventID, DedupeKey), the topic it was published to, its text (PayloadSummary, and Payload: the
// whole text of a long message, or the JSON a Dispatch delivery carries, which Dispatch decodes),
// and its reply correlation (InReplyTo).
type HandoffEnvelope = listenerwire.Envelope

// HandoffSender is the listener's stamp of an envelope's sending session
// (listenerwire.EnvelopeSender).
type HandoffSender = listenerwire.EnvelopeSender

// DecodeHandoff reads a hand-off and checks what the daemon relies on: a target session, a role
// token, a project, and an envelope with its event id, source, topic and dedupe key.
func DecodeHandoff(data []byte) (Handoff, error) {
	var h Handoff
	if err := json.Unmarshal(data, &h); err != nil {
		return Handoff{}, fmt.Errorf("decode the hand-off: %w", err)
	}
	switch {
	case h.OriginalTopic == "" || h.TargetSession == "" || h.RoleToken == "" || h.Project == "":
		return Handoff{}, errors.New("the hand-off lacks its original topic, target session, role token or project")
	case h.Envelope.EventID == "" || h.Envelope.Source == "" || h.Envelope.Topic == "" || h.Envelope.DedupeKey == "":
		return Handoff{}, errors.New("the hand-off's envelope lacks its event id, source, topic or dedupe key")
	}
	return h, nil
}

// DispatchActor is a Dispatch writer: a person ("user", their login), an agent ("session", its
// session id), or Dispatch itself ("system").
type DispatchActor struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Person says whether the actor is a person: only a person's act is a decision on an agent's ask
// (dispatch.Actor.Person of sjawhar/legion#2163).
func (a DispatchActor) Person() bool { return a.Kind == "user" }

// DispatchDelivery is a targeted send's delivery: the attempt the recipient replies through, the
// mode it was sent in, and for a mention the comment and the mention's target.
type DispatchDelivery struct {
	Attempt   int    `json:"attempt"`
	Mode      string `json:"mode"`
	CommentID string `json:"comment_id,omitempty"`
	Target    string `json:"target,omitempty"`
}

// DispatchMention is one mention a comment carries: its target and the session it resolved to.
type DispatchMention struct {
	Target    string  `json:"target"`
	SessionID *string `json:"session_id"`
}

// DispatchAnswer is a person's answer to an ask as ask.answered carries it (Dispatch's AskAnswer):
// who answered, the options they selected, and their text, nil when they wrote none.
type DispatchAnswer struct {
	User     string   `json:"user"`
	Selected []string `json:"selected"`
	Text     *string  `json:"text"`
}

// DispatchPayload is what the loop reads of a Dispatch event's payload: the message's, comment's
// or ask's id, its issue, its text, its author (an ask's), its thread (InReplyTo of a message,
// ReplyTo and AskID of a comment), a message's target, a comment's mentions, whom an ask waits on
// after a reply, and an ask's kind (`question`, or `approval` for a document's review request) and
// answer.
type DispatchPayload struct {
	ID           string            `json:"id"`
	IssueKey     *string           `json:"issue_key"`
	Body         string            `json:"body"`
	Author       *DispatchActor    `json:"author"`
	Target       *string           `json:"target"`
	InReplyTo    *string           `json:"in_reply_to"`
	ReplyTo      *string           `json:"reply_to"`
	AskID        *string           `json:"ask_id"`
	Mentions     []DispatchMention `json:"mentions"`
	AskWaitingOn string            `json:"ask_waiting_on"`
	Kind         string            `json:"kind"`
	Answer       *DispatchAnswer   `json:"answer"`
}

// DispatchEvent is a Dispatch event as an envelope's payload carries it: its id, its type, who
// acted, its issue and project, and its payload; Delivery is set when it came in a targeted send's
// frame ({event, delivery}), nil for a bare event on a follower or author route.
type DispatchEvent struct {
	ID       int64           `json:"id"`
	Type     string          `json:"type"`
	Actor    DispatchActor   `json:"actor"`
	IssueKey *string         `json:"issue_key"`
	Project  string          `json:"project"`
	Payload  DispatchPayload `json:"payload"`
	Delivery *DispatchDelivery
}

// HandsTurnBack says whether the event is a person's reply on an ask that leaves the next move
// with the ask's agent, as sjawhar/legion#2163's dispatchAskReply reads a reply: a comment on an
// ask (AskID) by a person whose ask_waiting_on is not `human`. Dispatch hands an open ask to the
// agent at every reply of a person (`agent`); a reply under an answered or resolved ask records no
// turn (empty), and is the only answer a person can still give there. A person's reply that leaves
// the ask with the human is their own hand-back of an approval request, which hands nothing back.
func (e DispatchEvent) HandsTurnBack() bool {
	return e.Type == "comment.created" && e.Payload.AskID != nil && e.Actor.Person() && e.Payload.AskWaitingOn != "human"
}

// AnswerMarker opens the marker the daemon's answer loop closes every reply it posts in a stopped
// agent's name with, `<!-- legion-answer:<id> -->` (answerloop.AnswerMarker is this constant).
// Two readers need it: the loop, which never answers a hand-off carrying it and reads an ask's
// thread for it before posting there again, and the decoder, which reads no fact from a comment
// carrying it (decodeDispatchFact), so that an answer the loop posts moves nothing in the
// workflow. A comment event carries at most the first contracts.DispatchEventBodyMax units of a
// body, which no reply inside the loop's output bound comes near.
const AnswerMarker = "<!-- legion-answer:"

// DispatchOf decodes a Dispatch envelope's payload: a targeted send's {event, delivery} frame, or
// the bare event Dispatch's outbox publishes.
func DispatchOf(e HandoffEnvelope) (DispatchEvent, error) {
	if e.Source != "dispatch" {
		return DispatchEvent{}, fmt.Errorf("an envelope from %q is not Dispatch's", e.Source)
	}
	var frame struct {
		Event    *DispatchEvent    `json:"event"`
		Delivery *DispatchDelivery `json:"delivery"`
	}
	if err := json.Unmarshal([]byte(e.Payload), &frame); err != nil {
		return DispatchEvent{}, fmt.Errorf("decode the Dispatch payload of %s: %w", e.EventID, err)
	}
	if frame.Event != nil && frame.Delivery != nil {
		event := *frame.Event
		event.Delivery = frame.Delivery
		return event, nil
	}
	var event DispatchEvent
	if err := json.Unmarshal([]byte(e.Payload), &event); err != nil {
		return DispatchEvent{}, fmt.Errorf("decode the Dispatch event of %s: %w", e.EventID, err)
	}
	if event.Type == "" {
		return DispatchEvent{}, fmt.Errorf("the Dispatch payload of %s names no event type", e.EventID)
	}
	return event, nil
}

// HandoffHandler takes one hand-off: a nil error once it is handled, so the delivery is
// acknowledged, with dropped true for one it logged and let go with nothing recorded (a hand-off
// for a conversation the daemon does not hold, or one that asks nothing and carries nothing it
// keeps), which a replay closes as no fact (replayHandoff); an error to have it delivered again
// at the delay its delivery count earns, and given up once the intake's bound is reached
// (consumeHandoff). raw is the hand-off as published.
type HandoffHandler func(ctx context.Context, handoff Handoff, raw []byte) (dropped bool, err error)

// Handoffs is the project's durable hand-off consumer: the Legion project token its subjects and
// its hand-offs name (claim.ProjectToken of the intake's project), its stream and its consumer,
// and the run each of its deliveries is settled on (consumerRun), which keeps its failures and its
// give-ups where the intake keeps its own. sweepEvery is its failure sweep's period,
// failureSweepEvery, which a test shortens.
type Handoffs struct {
	token      string
	stream     jetstream.Stream
	consumer   jetstream.Consumer
	run        consumerRun
	sweepEvery time.Duration
}

// OpenHandoffs creates or updates the project's durable hand-off consumer on
// listenerwire.LegionHandoffSubject(<P>, ">"), P the Legion project token of spec's project
// (claim.ProjectToken), as OpenConsumers does its own: one it creates starts at the stream time
// pool keeps for it, or at the next hand-off when it keeps none, and one that exists keeps its
// position. spec is the intake's own (the ConsumerSpec OpenConsumers takes), so a hand-off the
// loop keeps failing on is handled as an intake message that keeps failing is, under this
// consumer's own name: delivered again at the same delays, given up under the same bound, kept in
// the same record and counted in the same notice (consumeHandoff), and replayed through the loop
// (GiveUps). Only the bound on one handling differs: AckWait, where an intake message's is
// HandlerLimit.
func OpenHandoffs(ctx context.Context, js jetstream.JetStream, pool store.TxPool, spec ConsumerSpec) (*Handoffs, error) {
	if pool == nil {
		return nil, errors.New("the hand-off consumer needs the Postgres pool its failures are kept in")
	}
	spec, err := normalizedSpec(spec)
	if err != nil {
		return nil, err
	}
	token, err := claim.ProjectToken(spec.Project)
	if err != nil {
		return nil, err
	}
	stream, err := js.Stream(ctx, notificationStream)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", notificationStream, err)
	}
	remade := &remadeFailures{}
	consumer, err := openConsumer(ctx, js, stream, pool, spec, remade, jetstream.ConsumerConfig{
		Durable:       handoffConsumerName(token),
		FilterSubject: listenerwire.LegionHandoffSubject(token, ">"),
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       spec.AckWait,
	})
	if err != nil {
		return nil, fmt.Errorf("open the hand-off durable consumer: %w", err)
	}
	run := consumerRun{spec: spec, pool: pool, maxAge: stream.CachedInfo().Config.MaxAge, names: &issueNames{}, remade: remade,
		dropped: newDroppedSettlements()}
	return &Handoffs{token: token, stream: stream, consumer: consumer, run: run, sweepEvery: failureSweepEvery}, nil
}

// Run hands every hand-off to handle until ctx ends (consumeHandoff), and beside it sweeps the
// consumer's open failures it moved past (failureSweep), as Consumers.Run does for the intake's.
func (h *Handoffs) Run(ctx context.Context, handle HandoffHandler) error {
	group, running := errgroup.WithContext(ctx)
	group.Go(func() error { return h.consume(running, handle) })
	group.Go(func() error {
		sweep := &failureSweep{spec: h.run.spec, pool: h.run.pool, stream: h.stream, names: []string{handoffConsumerName(h.token)}}
		return sweep.run(running, h.sweepEvery)
	})
	return group.Wait()
}

// consume hands the consumer's hand-offs to handle one at a time, asking JetStream for
// consumerFetchBatch of them at once as consumeConsumer does, until ctx ends.
func (h *Handoffs) consume(ctx context.Context, handle HandoffHandler) error {
	consuming, err := h.consumer.Consume(func(message jetstream.Msg) {
		metadata, err := message.Metadata()
		if err != nil {
			metadata = nil
		}
		consumeHandoff(ctx, message, metadata, h.run, h.token, handle)
	}, jetstream.PullMaxMessages(consumerFetchBatch))
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		consuming.Stop()
		<-consuming.Closed()
		return nil
	case <-consuming.Closed():
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("durable consumer %s stopped unexpectedly", h.consumer.CachedInfo().Name)
	}
}

// consumeHandoff settles one delivery of the hand-off consumer as consumeMessage settles an intake
// message's, on run, the consumer's: a hand-off that cannot be read, or that is not of project
// token or not for the session its subject names (handoffOn), is given up at its first delivery as
// a message that does not decode; one whose delivery before did not end is given up before handle
// runs again once the bound is reached; one handle records, or drops, is acknowledged, its open
// failure deleted; and one handle fails on is recorded and delivered again at the delay its
// delivery count earns, or given up once the bound is reached or the stream would delete it
// first (consumerRun.failed). handle runs within AckWait, reported in progress meanwhile. It
// answers whether the message left its consumer.
func consumeHandoff(ctx context.Context, message jetstream.Msg, metadata *jetstream.MsgMetadata, run consumerRun, token string, handle HandoffHandler) bool {
	handoff, err := handoffOn(message.Subject(), token, message.Data())
	if err != nil {
		return run.undecodable(ctx, message, metadata, err)
	}
	decoded := decodedMessage{Source: handoff.Envelope.Source, EventID: handoff.Envelope.EventID}
	mayHaveFailed := run.mayHaveFailed(metadata)
	if metadata != nil && metadata.NumDelivered >= 2 {
		if decided, handled := run.noteRedelivery(ctx, message, metadata, decoded); decided {
			return handled
		}
	}
	spec := run.spec
	work, cancel := context.WithTimeout(store.ForConcurrentWorker(ctx), spec.AckWait)
	defer cancel()
	stop := keepInProgress(work, message, spec.AckWait/3, spec.Logger)
	_, err = handle(work, handoff, message.Data())
	stop()
	if err != nil {
		return run.failed(ctx, message, metadata, decoded, nakOutcome("hand-off not recorded; it is delivered again", "nak hand-off", err))
	}
	return acknowledged(ctx, message, metadata, run, mayHaveFailed, false)
}

// handoffOn decodes data, a hand-off published on subject (DecodeHandoff), and holds it to that
// subject: a hand-off of project token, for the session the subject names.
func handoffOn(subject, token string, data []byte) (Handoff, error) {
	handoff, err := DecodeHandoff(data)
	if err != nil {
		return Handoff{}, err
	}
	session, ok := strings.CutPrefix(subject, listenerwire.LegionHandoffSubject(token, ""))
	switch {
	case handoff.Project != token:
		return Handoff{}, fmt.Errorf("the hand-off names project %q on this daemon's subject", handoff.Project)
	case !ok || session != handoff.TargetSession:
		return Handoff{}, fmt.Errorf("the hand-off for session %q came on subject %s", handoff.TargetSession, subject)
	}
	return handoff, nil
}

func handoffConsumerName(project string) string {
	return "legion-go-" + project + "-answers"
}
