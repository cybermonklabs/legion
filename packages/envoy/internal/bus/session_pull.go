// A pull of one session consumer's messages (SessionConsumers.Pull): the pull requests it sends,
// the messages it hands its caller and how each is answered.

package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	// SessionPullDrainWait is how long a caller that must not wait out a silent server -- a
	// listener that is stopping -- gives its pulls' hand-overs to end (Detach's ctx). It was the
	// bound Detach put on every drain, so it is what a stop on a silent hub spent on its relays'
	// hand-overs.
	SessionPullDrainWait = time.Second
	// sessionPullFlushBound is nats.go's own bound on a flush (Conn.Flush, 10 s in nats.go
	// v1.55.0), which Detach's flush keeps for a ctx that has no deadline.
	sessionPullFlushBound = 10 * time.Second
	// sessionPullDrainPoll is how often Detach reads whether the pull's inbox subscription has
	// gone invalid, which is nats.go having handed over every message it held unread. It is the
	// 10 ms the connection's own drain waits at (Client.drainDeliveries): nats.go runs a
	// subscription's drain on a goroutine that flushes once and then re-checks on a 100 ms sleep,
	// so a finer poll observes nothing sooner and only takes the subscription's mutex oftener.
	sessionPullDrainPoll = 10 * time.Millisecond
	// sessionPullDroppedLogEvery is how often a pull writes its line for a message on its inbox
	// that its consumer did not deliver (SessionPull.receive): the first, then at most one a
	// minute, each saying how many it held back since the line before.
	sessionPullDroppedLogEvery = time.Minute
)

// ErrSessionPullStalled ends a pull (SessionPull.Next) that heard nothing from the server, no
// message and no heartbeat, for twice sessionPullHeartbeat: its pull request is lost, as on a
// connection nats.go replaced.
var ErrSessionPullStalled = errors.New("bus: the session consumer's pull heard nothing from the server")

// SessionPull is a pull of one session consumer's messages, which arrive in stream order, except
// messages naked back to the server (Release), which arrive in the server's own order unless a
// restart at the ack floor (RestartAtFloor) came first. It sends the consumer's pull requests
// ($JS.API.CONSUMER.MSG.NEXT) itself, one at a time, each for sessionPullBatch messages, and the
// next once the server has delivered them all or the request expired, so starting it is one
// publish and no API call. The server answers on the pull's own inbox, which holds one
// subscription.
type SessionPull struct {
	conn *nats.Conn
	sub  *nats.Subscription
	// sessionID is the session whose consumer it pulls; name is the consumer's, next its pull
	// request subject, inbox the subject the server answers on, ack the start of the
	// acknowledgement subject the server gives each message the consumer delivers, and request the
	// pull request's body.
	sessionID, name, next, inbox, ack string
	request                           []byte
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

	// flushEnded is the SessionConsumers' flush observer when the pull started (SetFlushObserver),
	// nil outside tests.
	flushEnded func(SessionPullFlushEnd)
}

// SessionPullFlushEnd is how the flush Detach sends after its drain's UNSUB ended, as a flush
// observer is told (SessionConsumers.SetFlushObserver).
type SessionPullFlushEnd int

const (
	// SessionPullFlushAnswered: the server answered, so it took the UNSUB.
	SessionPullFlushAnswered SessionPullFlushEnd = iota + 1
	// SessionPullFlushEndedByCaller: Detach's caller's context ended before the server answered.
	SessionPullFlushEndedByCaller
	// SessionPullFlushEndedOtherwise: anything else ended it before the server answered, nats.go's
	// own bound on a flush (sessionPullFlushBound) or the connection's close among them.
	SessionPullFlushEndedOtherwise
)

// String is the flush end's name, as a test's failure names it.
func (e SessionPullFlushEnd) String() string {
	switch e {
	case SessionPullFlushAnswered:
		return "answered"
	case SessionPullFlushEndedByCaller:
		return "ended by the caller's context"
	case SessionPullFlushEndedOtherwise:
		return "ended otherwise"
	}
	return "unknown"
}

// Pull starts pulling sessionID's consumer on the connection, without reading it first: the first
// Next returns ErrSessionConsumerNotFound when the stream holds no such consumer. Pulled messages
// wait in the pull for Next. Release ends it and hands every one of them back to the server at
// once; Stop ends it and leaves them, so the server delivers each message it delivered and was
// not acked for again only once its ack wait has passed, or at a restart (RestartAtFloor).
func (s *SessionConsumers) Pull(ctx context.Context, sessionID string) (*SessionPull, error) {
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
	conn := s.client.Connection()
	s.pendingMu.Lock()
	flushEnded := s.flushEnded
	s.pendingMu.Unlock()
	p := &SessionPull{
		conn: conn, sessionID: sessionID, name: name, next: consumerAPI("MSG.NEXT", name), inbox: conn.NewInbox(),
		ack: "$JS.ACK." + Stream + "." + name + ".", request: request, logger: s.logger, heartbeat: s.pullHeartbeat,
		wake: make(chan struct{}, 1), flushEnded: flushEnded,
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

// receive takes what the server sends on the pull's inbox: a heartbeat only marks it heard, and
// anything else is queued for Next, except a message whose reply is not the consumer's own
// acknowledgement subject ($JS.ACK.<stream>.<consumer>.…, the form nats-server writes unless its
// js_ack_fc_v2 feature flag is set, and the one the listener's grant acknowledges on). The
// consumer delivered no such message; another connection published it into the inbox, as any
// grant that publishes every _INBOX. subject may. It is dropped (drop), so neither Next nor
// Release acts on it: Next could read no delivery from its reply and would end the pull, and an
// answer on a reply naming another consumer would answer that consumer's message. nats-server
// refuses a client's publish whose reply begins $JS.ACK., so such a message reaches the inbox
// today with some other reply; the check does not rely on that.
//
// A status is not checked: a message with no reply is the server's status as far as receive can
// tell, since a forged one carries the same subject and no reply. So a message another connection
// publishes into the inbox with no reply and a Status header of 409 or 503 is still queued, and
// Next ends the pull on it (the relay then restarts at the ack floor; nothing is acknowledged).
// A session's own grant closes it: natsgrant.Session publishes bus.SessionReplies
// (_INBOX.*.<session id>.>) in place of every _INBOX. subject, so a session publishes to no
// pull's inbox, _INBOX.<id>. A holder of the shared session user or of a host user still can.
func (p *SessionPull) receive(msg *nats.Msg) {
	status, _ := pullStatus(msg)
	if status == "" && !strings.HasPrefix(msg.Reply, p.ack) {
		p.drop(msg)
		return
	}
	p.mu.Lock()
	p.heard = time.Now()
	if status != pullStatusHeartbeat {
		p.queue = append(p.queue, msg)
	}
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
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

// pull sends a pull request for sessionPullBatch messages.
func (p *SessionPull) pull() error {
	p.mu.Lock()
	p.heard = time.Now()
	p.mu.Unlock()
	if err := p.conn.PublishRequest(p.next, p.inbox, p.request); err != nil {
		return err
	}
	p.remaining = sessionPullBatch
	return nil
}

// Next returns the next message the pull holds, waiting for one until ctx ends. It returns
// ErrSessionConsumerNotFound once the stream holds no such consumer, and ErrSessionPullStalled
// once the server has sent nothing for twice sessionPullHeartbeat.
func (p *SessionPull) Next(ctx context.Context) (SessionMessage, error) {
	for {
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
// ctx ends, or the server has sent nothing for twice sessionPullHeartbeat.
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
		if stalled <= 0 {
			return nil, ErrSessionPullStalled
		}
		timer := time.NewTimer(stalled)
		select {
		case <-p.wake:
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
func (p *SessionPull) Stop() { _ = p.sub.Unsubscribe() }

// Detach takes the pull's interest away without answering anything: the server's next attempt to
// deliver on the pull request still standing for this pull finds no interest and drops that
// request. A caller that holds messages of this pull's consumer elsewhere -- a relay holding
// frames it published -- detaches first, so that naking those does not deliver them straight back
// into the pull it is giving up, from where they reach the next owner only once Release naks them
// again, as a further delivery and out of stream order. Release detaches by itself, so a caller
// that only returns what the pull holds needs this not at all.
//
// It is idempotent: the drain runs once, under the first call's ctx, and every call answers its
// result. It returns once nats.go has ended the drain, or once ctx ends: a caller that gives no
// deadline waits as long as nats.go makes the drain take, which nats.go bounds (the flush the
// drain opens with carries 10 s in nats.go v1.55.0, and a link that drops releases it at once). It
// reports a drain it stopped waiting for, and one the server did not confirm, on a link that
// stopped answering or across a reconnect: the server may then still deliver into the inbox, and
// what it delivers there is not returned.
func (p *SessionPull) Detach(ctx context.Context) error {
	p.detachOnce.Do(func() { p.detachErr = p.detach(ctx) })
	return p.detachErr
}

// detach is Detach's one drain.
func (p *SessionPull) detach(ctx context.Context) error {
	reconnects := p.conn.Stats().Reconnects
	// The inbox is drained, not unsubscribed: an unsubscribe drops what nats.go holds unread,
	// which is what Release returns. A drain hands those messages to receive, which queues them.
	if err := p.sub.Drain(); err != nil {
		// Drain fails only on a closed connection (nats.go v1.55.0: ErrConnectionClosed; its
		// other error, a subscription with no connection, cannot arise here). The close has
		// already ended the subscription and thrown away what nats.go held unread, so those
		// messages reach neither Release nor the server and are delivered again only once their
		// ack wait has passed. The poll below would end at once on the invalid subscription and
		// report nothing, so the failure is returned here instead. Stop is kept as a guard for
		// any other drain failure; on a closed connection it does nothing.
		p.Stop()
		return fmt.Errorf("bus: the inbox subscription of the pull of consumer %q could not be drained, so the messages nats.go held unread are lost: %w", p.name, err)
	}
	// The server reads a connection in order, so its answer to a PING sent after the drain's
	// UNSUB confirms it took the UNSUB. nats.go's drain flushes too but ends the drain whether or
	// not the server answered, so Detach asks for itself.
	confirmed := p.flushWithin(ctx)
	// The wait sets no span of its own; nats.go ends every drain (checkDrained): after the flush
	// it opens with, at the first pass in which receive has taken what nats.go held, or once the
	// subscription or the connection is closed. A span of Detach's own on every drain (1 s,
	// SessionPullDrainWait, until this wait replaced it) also ended the wait on a link that was
	// live but slow, and Release then read the queue while nats.go still held messages: the
	// drain queued them after that read, nobody returned them, and the next owner had them only
	// once SessionConsumerAckWait had passed. Only a caller that must not wait gives one now.
	tick := time.NewTicker(sessionPullDrainPoll)
	defer tick.Stop()
	for p.sub.IsValid() {
		select {
		case <-ctx.Done():
			if confirmed != nil {
				return fmt.Errorf("bus: the server had not confirmed the end of the inbox subscription of the pull of consumer %q when its caller stopped waiting (%w), so it may still deliver into it, and what nats.go still held there is not returned", p.name, context.Cause(ctx))
			}
			return fmt.Errorf("bus: the drain of the inbox subscription of the pull of consumer %q had not ended when its caller stopped waiting (%w), so what nats.go still held there is not returned", p.name, context.Cause(ctx))
		case <-tick.C:
		}
	}
	switch {
	case confirmed != nil:
		return fmt.Errorf("bus: the server did not confirm the end of the inbox subscription of the pull of consumer %q, so it may still deliver into it what is then not returned: %w", p.name, confirmed)
	case p.conn.Stats().Reconnects != reconnects:
		// nats.go writes no UNSUB while it reconnects, and sends the draining subscription again
		// once it has, so the server keeps the inbox's interest.
		return fmt.Errorf("bus: the connection of the pull of consumer %q reconnected while its inbox subscription drained, so the server may still deliver into it what is then not returned", p.name)
	}
	return nil
}

// flushWithin is the server's answer to a PING sent now: nil once it answered, else why it did
// not before ctx ended, or before nats.go's own bound on a flush (sessionPullFlushBound) for a ctx
// with no deadline of its own. It tells the pull's flush observer, if any, how the flush ended.
func (p *SessionPull) flushWithin(ctx context.Context) error {
	bounded := ctx
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		bounded, cancel = context.WithTimeout(ctx, sessionPullFlushBound)
		defer cancel()
	}
	err := p.conn.FlushWithContext(bounded)
	if p.flushEnded != nil {
		p.flushEnded(flushEndOf(ctx, err))
	}
	return err
}

// flushEndOf is how a flush that ended with err ended, caller being Detach's caller's context:
// FlushWithContext answers the context's own error once that context ends.
func flushEndOf(caller context.Context, err error) SessionPullFlushEnd {
	switch {
	case err == nil:
		return SessionPullFlushAnswered
	case caller.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)):
		return SessionPullFlushEndedByCaller
	}
	return SessionPullFlushEndedOtherwise
}

// Release ends the pull and returns to the server, at once, every message it holds that Next did
// not read, and reports how many it returned. It detaches the pull (Detach, under ctx) before it
// naks any of them, so the server has no interest of this pull's left to deliver them back into.
// The consumer's next pull is delivered those messages at once rather than after
// SessionConsumerAckWait, in the server's own order; a restart of the consumer at its ack floor
// (RestartAtFloor) delivers them in stream order.
//
// Its error carries both halves of what can go wrong: the detach, and the naks, as how many of
// them failed and the first of their errors. Every message is tried whatever the others did.
func (p *SessionPull) Release(ctx context.Context) (int, error) {
	detachErr := p.Detach(ctx)
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
