// A pull of one session consumer's messages (SessionConsumers.Pull): the pull requests it sends,
// the messages it hands its caller and how each is answered.

package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

// SessionAcker answers one pulled message (SessionMessage).
type SessionAcker interface {
	// Ack removes the message from the consumer: it is done.
	Ack() error
	// InProgress restarts the message's ack wait.
	InProgress() error
	// NakWithDelay has the server deliver the message again once delay has passed.
	NakWithDelay(delay time.Duration) error
}

// SessionMessage is one message a session consumer delivered: the stored message and its delivery.
type SessionMessage struct {
	// Subject is the subject the message was stored on.
	Subject string
	Header  nats.Header
	Data    []byte
	// StreamSeq is the message's stream sequence.
	StreamSeq uint64
	// Delivered is how many times the consumer has delivered the message, this time included.
	Delivered uint64
	// StoredAt is when the stream stored the message.
	StoredAt time.Time
	SessionAcker
}

// The pull requests of a SessionPull.
const (
	// sessionPullBatch is how many messages one pull request asks for: the consumer's
	// MaxAckPending, the most it delivers before an ack anyway.
	sessionPullBatch = SessionConsumerMaxAckPending
	// sessionPullExpires is how long a pull request waits at the server; the pull sends the next
	// once it expires.
	sessionPullExpires = 30 * time.Second
	// sessionPullHeartbeat is how often the server signals a pull request that waits with nothing
	// to deliver. A pull that hears nothing from the server for twice as long ends with
	// ErrSessionPullStalled.
	sessionPullHeartbeat = 15 * time.Second
	// sessionPullDrainWait and sessionPullDrainPoll bound how long Detach waits for the pull's
	// inbox subscription to go invalid, which is nats.go having handed over every message it held
	// unread. A subscription on a local server drains in well under a millisecond, so the wait is
	// reached only by a connection that is gone, and a run that ends costs it once. The poll is
	// the 10 ms the connection's own drain waits at (Client.drainDeliveries): nats.go runs a
	// subscription's drain on a goroutine that flushes once and then re-checks on a 100 ms sleep,
	// so a finer poll observes nothing sooner and only takes the subscription's mutex oftener.
	sessionPullDrainWait = time.Second
	sessionPullDrainPoll = 10 * time.Millisecond
	// sessionPullDroppedLogEvery is how often a pull writes its line for a message on its inbox
	// that its consumer did not deliver (SessionPull.receive): the first, then at most one a
	// minute, each saying how many it held back since the line before.
	sessionPullDroppedLogEvery = time.Minute
)

// ErrSessionPullStalled ends a pull (SessionPull.Next) that heard nothing from the server, no
// message and no heartbeat, for twice sessionPullHeartbeat.
var ErrSessionPullStalled = errors.New("bus: the session consumer's pull heard nothing from the server")

// ErrSessionPullReconnected ends a pull (SessionPull.Next) whose connection reconnected: the
// server drops a pull request with the connection it was sent on, and the pull would hear nothing
// on the new one, so it ends as soon as the client reports a connection event
// (Client.ConnectionChanged) rather than after the twice sessionPullHeartbeat of silence that
// would otherwise find it.
var ErrSessionPullReconnected = errors.New("bus: the session consumer's pull request was lost when its connection reconnected")

// PullBound is how long a pull (SessionConsumers.Pull) may still take messages, read before each
// pull request the pull sends, while Next waits, as Next hands out each message, and as each
// message arrives. It shortens no request: each waits sessionPullExpires at the server.
//
// Once the bound is zero or less, the pull sends no request, and Next hands out nothing and
// returns ErrSessionPullBound, a Next that is waiting when the bound runs out included: what the
// pull holds stays in it for Release. A message that arrives once the bound is zero or less is
// left unanswered, with no ack, no nak and no progress. Next never returns it and Release never
// naks it, so the server delivers it again once the consumer's ack wait (SessionConsumerAckWait)
// has passed, or sooner at a restart of the consumer at its ack floor (RestartAtFloor).
//
// A request already waiting at the server goes on waiting until it expires. A caller that wants
// the server to hand that request nothing more detaches the pull (Detach): the server drops a
// waiting request whose inbox has no subscriber, at the next message it would deliver to it or
// the next time it reads its waiting requests, without handing it that message (nats-server
// v2.15.1 server/consumer.go:4519-4559, nextWaiting, and :5143-5157, processWaiting). A nil bound
// bounds nothing.
type PullBound func() time.Duration

// ErrSessionPullBound ends a pull's Next once the pull's bound (PullBound) has run out: Next hands
// out no message and sends no pull request past it. The pull goes on, and a Next made once the
// bound is above zero again goes on pulling.
var ErrSessionPullBound = errors.New("bus: the session consumer's pull is past its bound")

// SessionPull is a pull of one session consumer's messages, which arrive in stream order, except
// messages naked back to the server (Release), which arrive in the server's own order unless a
// restart at the ack floor (RestartAtFloor) came first. It sends the consumer's pull requests
// ($JS.API.CONSUMER.MSG.NEXT) itself, one at a time, each for sessionPullBatch messages, and the
// next once the server has delivered them all or the request expired, so starting it is one
// publish and no API call. The server answers on the pull's own inbox, which holds one
// subscription.
type SessionPull struct {
	// watch follows the connection the pull's requests go out on, through the SessionConsumers'
	// client's connection events (Client.ConnectionChanged), to tell a request the server still
	// holds from one a connection event dropped (connWatch.lostRequest). Only Next's goroutine
	// reads or replaces its channel (take).
	watch connWatch
	sub   *nats.Subscription
	// sessionID is the session whose consumer it pulls; name is the consumer's, next its pull
	// request subject, inbox the subject the server answers on, ack the start of the
	// acknowledgement subject the server gives each message the consumer delivers, and request the
	// pull request's body. bound, nil for none, is how long the pull may still take messages
	// (PullBound).
	sessionID, name, next, inbox, ack string
	request                           []byte
	bound                             PullBound
	// logger is the logger of the SessionConsumers that started it.
	logger *slog.Logger
	// remaining is how many messages the pull request in flight may still deliver; 0 when none is.
	// Only Next reads or writes it.
	remaining int
	// heartbeat is the pull requests' idle heartbeat (sessionPullHeartbeat).
	heartbeat time.Duration
	// wake holds a token once receive has queued a message for Next.
	wake chan struct{}
	// detachOnce runs the drain of the inbox subscription once and detachErr holds what it ended
	// with, so that Detach can be called twice -- by a caller that detaches before it naks
	// messages it holds elsewhere, and by the Release that follows -- and the second call
	// answers the first's result without draining or waiting again.
	detachOnce sync.Once
	detachErr  error

	// mu guards queue, heard, droppedLoggedAt and droppedHeld.
	mu sync.Mutex
	// queue holds what the server sent on the inbox, heartbeats aside, until Next reads it.
	queue []*nats.Msg
	// heard is when the server last sent anything on the inbox, or the pull last sent a request.
	heard time.Time
	// droppedLoggedAt is when receive last wrote its line for a message it dropped, and
	// droppedHeld how many it has dropped since without one (sessionPullDroppedLogEvery).
	droppedLoggedAt time.Time
	droppedHeld     int
}

// Pull starts pulling sessionID's consumer on the connection, without reading it first: the first
// Next returns ErrSessionConsumerNotFound when the stream holds no such consumer. Pulled messages
// wait in the pull for Next. Release ends it and hands every one of them back to the server at
// once; Stop ends it and leaves them, so the server delivers each message it delivered and was
// not acked for again only once its ack wait has passed, or at a restart (RestartAtFloor). bound,
// nil for none, is how long the pull may still take messages (PullBound): Pull returns
// ErrSessionPullBound, and starts nothing, once it has run out.
func (s *SessionConsumers) Pull(ctx context.Context, sessionID string, bound PullBound) (*SessionPull, error) {
	name, err := sessionConsumerNameOf(sessionID)
	if err != nil {
		return nil, err
	}
	if err := s.client.ensureConnWithContext(ctx); err != nil {
		return nil, err
	}
	request, err := json.Marshal(struct {
		Batch     int           `json:"batch"`
		Expires   time.Duration `json:"expires"`
		Heartbeat time.Duration `json:"idle_heartbeat"`
	}{Batch: sessionPullBatch, Expires: s.pullExpires, Heartbeat: s.pullHeartbeat})
	if err != nil {
		return nil, err
	}
	// The connection is read through a watch (watchConnection), whose channel is taken before the
	// connection and its reconnect count are read, so the watch misses no event after that take.
	// An event after the reads ends the pull rather than leaving it waiting on a request the
	// server dropped, a disconnect at the reconnect that follows it. A reconnect or a replacement
	// between the take and the reads ends nothing: the reads already found the connection and
	// count it left, and the request goes out on that connection (connWatch.lost). A close before
	// the subscribe or the pull's request fails Pull itself. An event after the reads and before
	// the request, which nats.go then sends on the new connection, ends the pull too: the cost is
	// one restart at the ack floor, where the cost of missing the event is a stalled relay.
	watch := s.client.watchConnection()
	conn := watch.conn
	p := &SessionPull{
		watch: watch, sessionID: sessionID, name: name, next: consumerAPI("MSG.NEXT", name), inbox: conn.NewInbox(),
		ack: "$JS.ACK." + Stream + "." + name + ".", request: request, bound: bound, logger: s.logger,
		heartbeat: s.pullHeartbeat, wake: make(chan struct{}, 1),
	}
	if p.sub, err = conn.Subscribe(p.inbox, p.receive); err != nil {
		return nil, err
	}
	if err := p.pull(); err != nil {
		p.Stop()
		return nil, err
	}
	return p, nil
}

// receive takes what the server sends on the pull's inbox: a heartbeat only marks it heard, a
// message that arrives once the pull's bound has run out only marks it heard too and is left
// unanswered (PullBound), and anything else is queued for Next, except a message whose reply is
// not the consumer's own acknowledgement subject ($JS.ACK.<stream>.<consumer>.…, the form
// nats-server writes unless its js_ack_fc_v2 feature flag is set, and the one the listener's
// grant acknowledges on). The consumer delivered no such message; another connection published
// it into the inbox, as any grant that publishes every _INBOX. subject may. It is dropped (drop)
// whether or not the bound has run out, so neither Next nor Release acts on it and it does not
// mark the pull heard: Next could read no delivery from its reply and would end the pull, and an
// answer on a reply naming another consumer would answer that consumer's message. nats-server
// refuses a client's publish whose reply begins $JS.ACK., so such a message reaches the inbox
// today with some other reply; the check does not rely on that. The bound is read before the
// pull's lock is taken, since a caller's bound may take locks of its own.
//
// A status is not checked: a message with no reply is the server's status as far as receive can
// tell, since a forged one carries the same subject and no reply. So a message another connection
// publishes into the inbox with no reply and a Status header of 409 or 503 is still queued, and
// Next ends the pull on it (the relay then restarts at the ack floor; nothing is acknowledged).
// The grant that closes it is natsgrant.Session publishing bus.SessionReplies
// (_INBOX.*.<session id>.>) in place of every _INBOX. subject: a session may then publish to no
// pull's inbox, _INBOX.<id>.
func (p *SessionPull) receive(msg *nats.Msg) {
	status, _ := pullStatus(msg)
	if status == "" && !strings.HasPrefix(msg.Reply, p.ack) {
		p.drop(msg)
		return
	}
	late := status == "" && p.pastBound()
	p.mu.Lock()
	p.heard = time.Now()
	if status != pullStatusHeartbeat && !late {
		p.queue = append(p.queue, msg)
	}
	p.mu.Unlock()
	if late {
		return
	}
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// pastBound reports whether the pull's bound has run out (PullBound).
func (p *SessionPull) pastBound() bool {
	return p.bound != nil && p.bound() <= 0
}

// drop counts a message receive will not queue and writes its WARN line: the first, then at most
// one every sessionPullDroppedLogEvery, each saying how many it held back since the line before,
// since whoever publishes into the inbox can do so as fast as it likes. A dropped message is not
// the server's, so it does not mark the pull heard.
func (p *SessionPull) drop(msg *nats.Msg) {
	now := time.Now()
	p.mu.Lock()
	logLine := p.droppedLoggedAt.IsZero() || now.Sub(p.droppedLoggedAt) >= sessionPullDroppedLogEvery
	held := p.droppedHeld
	if logLine {
		p.droppedLoggedAt, p.droppedHeld = now, 0
	} else {
		p.droppedHeld++
	}
	p.mu.Unlock()
	if logLine {
		p.logger.Warn("session consumer pull dropped a message its consumer did not deliver: its reply is not the consumer's acknowledgement subject",
			slog.String("session_id", p.sessionID), slog.String("consumer", p.name), slog.String("reply", msg.Reply),
			slog.Int("held", held))
	}
}

// The status codes the server answers a pull request with, beside the messages it delivers.
const (
	pullStatusHeartbeat    = "100"
	pullStatusExpired      = "408"
	pullStatusConflict     = "409"
	pullStatusNoResponders = "503"
	// pullLeadershipChange is the description of the 409 that ends a waiting pull request when the
	// consumer's leader changes; the consumer still exists.
	pullLeadershipChange = "Leadership Change"
)

// pullStatus is the status code and description of msg, a status the server sent on a pull's
// inbox, or "" for a message the consumer delivered, which carries its ack subject as its reply
// subject (and may carry any header of its own).
func pullStatus(msg *nats.Msg) (status, description string) {
	if msg.Reply != "" {
		return "", ""
	}
	return msg.Header.Get("Status"), msg.Header.Get("Description")
}

// pull sends a pull request for sessionPullBatch messages, or none, returning ErrSessionPullBound,
// once the pull's bound has run out (PullBound).
func (p *SessionPull) pull() error {
	if p.pastBound() {
		return ErrSessionPullBound
	}
	p.mu.Lock()
	p.heard = time.Now()
	p.mu.Unlock()
	if err := p.watch.conn.PublishRequest(p.next, p.inbox, p.request); err != nil {
		return err
	}
	p.remaining = sessionPullBatch
	return nil
}

// Next returns the next message the pull holds, waiting for one until ctx ends. It returns
// ErrSessionConsumerNotFound once the stream holds no such consumer, ErrSessionPullReconnected
// once the connection the pull request was sent on reconnected, ErrSessionPullStalled once
// the server has sent nothing for twice sessionPullHeartbeat, and ErrSessionPullBound while the
// pull's bound has run out (PullBound). The bound is read again once a message is in hand, so a
// bound that ran out while Next waited hands out nothing: the message stays first in the pull,
// for a later Next or for Release.
func (p *SessionPull) Next(ctx context.Context) (SessionMessage, error) {
	for {
		if p.pastBound() {
			return SessionMessage{}, ErrSessionPullBound
		}
		if p.remaining == 0 {
			if err := p.pull(); err != nil {
				return SessionMessage{}, err
			}
		}
		msg, err := p.take(ctx)
		if err != nil {
			return SessionMessage{}, err
		}
		switch status, description := pullStatus(msg); {
		case status == "":
			if p.pastBound() {
				p.mu.Lock()
				p.queue = slices.Insert(p.queue, 0, msg)
				p.mu.Unlock()
				return SessionMessage{}, ErrSessionPullBound
			}
			p.remaining--
			return sessionMessageOf(msg)
		case status == pullStatusExpired, status == pullStatusConflict && description == pullLeadershipChange:
			// The request ended; the next round sends another.
			p.remaining = 0
		case status == pullStatusNoResponders:
			return SessionMessage{}, fmt.Errorf("%w: %s", ErrSessionConsumerNotFound, p.name)
		default:
			return SessionMessage{}, fmt.Errorf("bus: pull of session consumer %s: %s %s", p.name, status, description)
		}
	}
}

// take returns the first message the server sent that Next has not read, waiting until one comes,
// ctx ends, a connection event dropped the pull's request (connWatch.lostRequest), the server
// has sent nothing for twice sessionPullHeartbeat, or the pull's bound runs out
// (ErrSessionPullBound), which it reads at each wake and times its wait to. The messages the
// server already sent are handed out first: they are the consumer's, not the lost request's, and
// acking one is a publish like any other.
func (p *SessionPull) take(ctx context.Context) (*nats.Msg, error) {
	for {
		p.mu.Lock()
		if len(p.queue) > 0 {
			msg := p.queue[0]
			p.queue[0] = nil
			p.queue = p.queue[1:]
			p.mu.Unlock()
			return msg, nil
		}
		stalled := time.Until(p.heard.Add(2 * p.heartbeat))
		p.mu.Unlock()
		wait := stalled
		if p.bound != nil {
			left := p.bound()
			if left <= 0 {
				return nil, ErrSessionPullBound
			}
			wait = min(wait, left)
		}
		if p.watch.lost() {
			return nil, ErrSessionPullReconnected
		}
		if stalled <= 0 {
			return nil, ErrSessionPullStalled
		}
		timer := time.NewTimer(wait)
		select {
		case <-p.wake:
		case <-p.watch.changed:
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		}
		timer.Stop()
	}
}

// sessionMessageOf is the message the server delivered as msg, whose reply subject carries its
// delivery (the JetStream ack subject).
func sessionMessageOf(msg *nats.Msg) (SessionMessage, error) {
	meta, err := msg.Metadata()
	if err != nil {
		return SessionMessage{}, err
	}
	return SessionMessage{
		Subject:      msg.Subject,
		Header:       msg.Header,
		Data:         msg.Data,
		StreamSeq:    meta.Sequence.Stream,
		Delivered:    meta.NumDelivered,
		StoredAt:     meta.Timestamp,
		SessionAcker: sessionAck{msg},
	}, nil
}

// sessionAck answers a pulled message on the connection it arrived on, by publishing to its reply
// subject; Ack and NakWithDelay are sent once.
type sessionAck struct{ msg *nats.Msg }

func (a sessionAck) Ack() error                             { return a.msg.Ack() }
func (a sessionAck) InProgress() error                      { return a.msg.InProgress() }
func (a sessionAck) NakWithDelay(delay time.Duration) error { return a.msg.NakWithDelay(delay) }

// Stop ends the pull and leaves the messages it held and Next had not returned where they are:
// the server delivers each of them again once its ack wait (SessionConsumerAckWait) has passed.
// A caller that wants them back at once ends the pull with Release instead.
//
// On a live connection it waits, at most sessionPullStopFlush, for the server to confirm the
// unsubscribe (a flush), so the pull's request has no subscriber left by the time Stop returns.
// The server skips a waiting request whose reply subject nobody subscribes to, which is what lets
// the caller's restart keep a consumer with nothing awaiting an ack (RestartAtFloor) and pull
// again. Without the wait, the server could take a message stored after Stop for the old request
// while the unsubscribe was still on its way, and that message would wait out the consumer's ack
// wait. That happens after a reconnect in place, whose resend gave the old inbox a subscription
// on the new connection. On a connection that is down the unsubscribe only drops the
// subscription from the client, which then sends it to no server.
func (p *SessionPull) Stop() {
	_ = p.sub.Unsubscribe()
	if p.watch.conn.IsConnected() {
		_ = p.watch.conn.FlushTimeout(sessionPullStopFlush)
	}
}

// sessionPullStopFlush bounds Stop's wait for the server to confirm a pull's unsubscribe.
const sessionPullStopFlush = 2 * time.Second

// Detach takes the pull's interest away without answering anything: the server's next attempt to
// deliver on the pull request still standing for this pull finds no interest and drops that
// request without handing it the message, and so does the next read of its waiting requests
// (PullBound has the server's lines). A caller that holds messages of this pull's consumer
// elsewhere -- a relay holding frames it published -- detaches first, so that naking those does
// not deliver them straight back into the pull it is giving up, from where they reach the next
// owner only once Release naks them again, as a further delivery and out of stream order. A
// caller whose bound ran out detaches so that the server hands the pull nothing more. Release
// detaches by itself, so a caller that only returns what the pull holds needs this not at all.
//
// It is idempotent: the drain runs once and every call answers its result.
func (p *SessionPull) Detach() error {
	p.detachOnce.Do(func() {
		// The inbox is drained, not unsubscribed: an unsubscribe drops what nats.go holds
		// unread, which is what Release returns. A drain hands those messages to receive, which
		// queues each the pull takes: one past its bound, and one its consumer did not deliver,
		// are not queued (PullBound, receive).
		if err := p.sub.Drain(); err != nil {
			// Drain fails only on a closed connection (nats.go v1.55.0: ErrConnectionClosed;
			// its other error, a subscription with no connection, cannot arise here). The close
			// has already ended the subscription and thrown away what nats.go held unread, so
			// those messages reach neither Release nor the server and are delivered again only
			// once their ack wait has passed. The poll below would end at once on the invalid
			// subscription and report nothing, so the failure is returned here instead. Stop is
			// kept as a guard for any other drain failure; on a closed connection it does
			// nothing.
			p.Stop()
			p.detachErr = fmt.Errorf("bus: the inbox subscription of the pull of consumer %q could not be drained, so the messages nats.go held unread are lost: %w", p.name, err)
			return
		}
		for deadline := time.Now().Add(sessionPullDrainWait); p.sub.IsValid() && time.Now().Before(deadline); {
			time.Sleep(sessionPullDrainPoll)
		}
		if p.sub.IsValid() {
			// The caller is about to nak messages of this consumer in the belief that no
			// interest of this pull's is left to take them, so a drain that never finished is
			// reported.
			p.detachErr = fmt.Errorf("bus: the inbox subscription of the pull of consumer %q did not drain within %s", p.name, sessionPullDrainWait)
		}
	})
	return p.detachErr
}

// Release ends the pull and returns to the server, at once, every message it holds that Next did
// not read, and reports how many it returned. It detaches the pull (Detach) before it naks any of
// them, so the server has no interest of this pull's left to deliver them back into. The
// consumer's next pull is delivered those messages at once rather than after
// SessionConsumerAckWait, in the server's own order; a restart of the consumer at its ack floor
// (RestartAtFloor) delivers them in stream order.
//
// Its error carries both halves of what can go wrong: the detach, and the naks, as how many of
// them failed and the first of their errors. Every message is tried whatever the others did.
func (p *SessionPull) Release() (int, error) {
	detachErr := p.Detach()
	p.mu.Lock()
	queue := p.queue
	p.queue = nil
	p.mu.Unlock()
	held := make([]SessionAcker, 0, len(queue))
	for _, msg := range queue {
		if status, _ := pullStatus(msg); status != "" {
			// A pull request's status (408, 409, 503) carries no ack subject: there is nothing to
			// return to the server.
			continue
		}
		held = append(held, sessionAck{msg})
	}
	returned, nakErr := ReturnMessages(fmt.Sprintf("the pull of consumer %q", p.name), held)
	return returned, errors.Join(detachErr, nakErr)
}

// ReturnMessages naks every one of ackers with no delay, so the server delivers those messages
// again at once rather than once the consumer's ack wait (SessionConsumerAckWait) has passed, and
// reports how many of them it returned. Every message is tried whatever the others did. whose is
// the phrase the error names the holder of the messages by -- a consumer's pull, a session's
// relay -- so that the two halves of one hand-over read apart in a log; the error carries the
// package's own prefix itself, so no caller spells it.
//
// The naks of one hand-over fail together, since the connection is closed or its reconnect buffer
// is full, so a failure is one error naming how many failed and the first of their errors rather
// than up to SessionConsumerMaxAckPending copies of that one error.
func ReturnMessages(whose string, ackers []SessionAcker) (int, error) {
	returned, failed := 0, 0
	var firstErr error
	for _, acker := range ackers {
		if err := acker.NakWithDelay(0); err != nil {
			failed++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		returned++
	}
	if failed > 0 {
		return returned, fmt.Errorf("bus: %s could not return %d of the messages it held; the first failed with: %w", whose, failed, firstErr)
	}
	return returned, nil
}
