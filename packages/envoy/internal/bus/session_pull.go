// A pull of one session consumer's messages (SessionConsumers.Pull): the pull requests it sends,
// the messages it hands its caller and how each is answered.

package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	// name is the consumer's, next its pull request subject, inbox the subject the server answers
	// on, and request the pull request's body.
	name, next, inbox string
	request           []byte
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
	// answers the first's result. A second nats.go drain of an already drained subscription is
	// an error of its own, which is what makes repeating the drain wrong rather than merely
	// wasteful.
	detachOnce sync.Once
	detachErr  error

	// mu guards queue and heard.
	mu sync.Mutex
	// queue holds what the server sent on the inbox, heartbeats aside, until Next reads it.
	queue []*nats.Msg
	// heard is when the server last sent anything on the inbox, or the pull last sent a request.
	heard time.Time
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
	p := &SessionPull{conn: conn, name: name, next: consumerAPI("MSG.NEXT", name), inbox: conn.NewInbox(), request: request, heartbeat: s.pullHeartbeat, wake: make(chan struct{}, 1)}
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
// anything else is queued for Next.
func (p *SessionPull) receive(msg *nats.Msg) {
	p.mu.Lock()
	p.heard = time.Now()
	if status, _ := pullStatus(msg); status != pullStatusHeartbeat {
		p.queue = append(p.queue, msg)
	}
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
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
// into the pull it is giving up, where nothing reads them and they are lost until their ack wait
// passes. Release detaches by itself, so a caller that only returns what the pull holds needs
// this not at all.
//
// It is idempotent: the drain runs once and every call answers its result.
func (p *SessionPull) Detach() error {
	p.detachOnce.Do(func() {
		// The inbox is drained, not unsubscribed: an unsubscribe drops what nats.go holds
		// unread, which is what Release returns. A drain hands those messages to receive, which
		// queues them.
		if err := p.sub.Drain(); err != nil {
			p.Stop()
		}
		for deadline := time.Now().Add(sessionPullDrainWait); p.sub.IsValid() && time.Now().Before(deadline); {
			time.Sleep(sessionPullDrainPoll)
		}
		if p.sub.IsValid() {
			// The caller is about to nak messages of this consumer in the belief that no
			// interest of this pull's is left to take them, so a drain that never finished is
			// worth a word rather than the silence it used to get.
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
	returned, failed := 0, 0
	var firstErr error
	for _, msg := range queue {
		if status, _ := pullStatus(msg); status != "" {
			// A pull request's status (408, 409, 503) carries no ack subject: there is nothing to
			// return to the server.
			continue
		}
		if err := msg.NakWithDelay(0); err != nil {
			failed++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		returned++
	}
	var nakErr error
	if failed > 0 {
		// The naks of one pull fail together -- the connection is closed, or its reconnect
		// buffer is full -- so they are one error naming the first of them rather than a list of
		// up to MaxAckPending copies of it.
		nakErr = fmt.Errorf("bus: the pull of consumer %q could not return %d of the messages it held; the first failed with: %w", p.name, failed, firstErr)
	}
	return returned, errors.Join(detachErr, nakErr)
}
