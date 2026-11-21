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
	// life is the life of conn the pull's requests are sent in (Client.requestLife), and lost the
	// error Next answers once it ends with client still running (lostConnection): a pull request
	// standing at the server goes with the connection, and a pull that waited for it would hear
	// nothing until it stalled.
	client *Client
	life   context.Context
	lost   error
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
	// beforeWait, when not nil, runs in take once it has read the pull's life and before it waits,
	// where a loss that lands is one the wait itself must see. It is nil outside tests: a test sets
	// it to end the connection at exactly that moment.
	beforeWait func()
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
// not acked for again only once its ack wait has passed, or at a restart (RestartAtFloor).
//
// The pull lasts as long as the connection it is made on (Client.requestLife): once that
// connection is lost, Next hands over what the pull already holds and then answers
// ErrConnectionLost, whose retry (Reconnected) is a new pull on the new connection. So does a
// call of the pull's own on that connection - its subscribe, its pull request and the drain Detach
// makes - and an acknowledgement of a message it delivered, when nats.go refused it for that
// connection's close, on a client still running and while the connection is still the client's
// (lostIfClosed): nats.go refuses each of them before it writes anything (nats.go v1.55.0
// nats.go:4738-4745, :5200-5201, :5602-5603).
func (s *SessionConsumers) Pull(ctx context.Context, sessionID string) (*SessionPull, error) {
	name, err := sessionConsumerNameOf(sessionID)
	if err != nil {
		return nil, err
	}
	if err := s.client.connForRequest(ctx); err != nil {
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
	life, conn, _ := s.client.requestLife(ctx)
	if life.Err() != nil {
		return nil, s.client.lostOrClosed(nil)
	}
	lost := &connectionLost{client: s.client}
	p := &SessionPull{
		conn: conn, client: s.client, life: life, lost: lost, sessionID: sessionID, name: name,
		next: consumerAPI("MSG.NEXT", name), inbox: conn.NewInbox(), ack: "$JS.ACK." + Stream + "." + name + ".",
		request: request, logger: s.logger, heartbeat: s.pullHeartbeat, wake: make(chan struct{}, 1),
	}
	if p.sub, err = conn.Subscribe(p.inbox, p.receive); err != nil {
		return nil, s.client.lostIfClosed(err, conn, true)
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
// The grant that closes it is natsgrant.Session publishing bus.SessionReplies
// (_INBOX.*.<session id>.>) in place of every _INBOX. subject: a session may then publish to no
// pull's inbox, _INBOX.<id>.
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

// pull sends a pull request for sessionPullBatch messages, on the pull's connection while its life
// lasts: one sent after the loss on the reconnected connection would draw messages to an inbox the
// pull is about to leave, which the server then holds until their ack wait passes.
func (p *SessionPull) pull() error {
	if p.lostConnection() {
		return p.lost
	}
	p.mu.Lock()
	p.heard = time.Now()
	p.mu.Unlock()
	if err := p.conn.PublishRequest(p.next, p.inbox, p.request); err != nil {
		return p.lostIfClosed(err)
	}
	p.remaining = sessionPullBatch
	return nil
}

// Next returns the next message the pull holds, waiting for one until ctx ends. It returns
// ErrSessionConsumerNotFound once the stream holds no such consumer, ErrSessionPullStalled
// once the server has sent nothing for twice sessionPullHeartbeat, and ErrConnectionLost once the
// pull's connection is lost and the pull holds nothing more.
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
			return p.messageOf(msg)
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

// lostConnection reports whether the pull's connection was lost: its life ended while its client
// still runs. A client that stopped (Close, Drain) ended that life itself, and its pull goes on
// waiting for its caller as it always did, so a relay stopped after its connection closed still
// hands back what it holds, and says it could not (sessionRelays' hand-over).
func (p *SessionPull) lostConnection() bool {
	return p.life.Err() != nil && !p.client.stopped()
}

// take returns the first message the server sent that Next has not read, waiting until one comes,
// ctx ends, the pull's connection is lost (ErrConnectionLost, lostConnection), or the server has
// sent nothing for twice sessionPullHeartbeat. What the pull already holds is handed over before
// the loss is.
//
// The life is read once a pass, and what the pass waits on is decided from that one read: a life
// that has ended with the client still running is the loss, answered at once; one that has ended
// on a client that stopped ends no wait; and one still running is waited on, so a loss that lands
// after the read wakes the wait, whose next pass answers it.
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
		ended := p.life.Done()
		if p.life.Err() != nil {
			if !p.client.stopped() {
				return nil, p.lost
			}
			ended = nil
		}
		if stalled <= 0 {
			return nil, ErrSessionPullStalled
		}
		if p.beforeWait != nil {
			p.beforeWait()
		}
		timer := time.NewTimer(stalled)
		select {
		case <-p.wake:
		case <-timer.C:
		case <-ended:
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		}
		timer.Stop()
	}
}

// messageOf is the message the server delivered to the pull as msg, whose reply subject carries its
// delivery (the JetStream ack subject).
func (p *SessionPull) messageOf(msg *nats.Msg) (SessionMessage, error) {
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
		SessionAcker: sessionAck{msg: msg, pull: p},
	}, nil
}

// sessionAck answers a message pull delivered on the connection it arrived on, the pull's, by
// publishing to its reply subject; Ack and NakWithDelay are sent once. An answer nats.go refused
// for that connection's close is the pull's lostIfClosed's.
type sessionAck struct {
	msg  *nats.Msg
	pull *SessionPull
}

func (a sessionAck) Ack() error        { return a.pull.lostIfClosed(a.msg.Ack()) }
func (a sessionAck) InProgress() error { return a.pull.lostIfClosed(a.msg.InProgress()) }
func (a sessionAck) NakWithDelay(delay time.Duration) error {
	return a.pull.lostIfClosed(a.msg.NakWithDelay(delay))
}

// lostIfClosed is err, nats.go's answer for a call made on the pull's connection, as its client's
// lostIfClosed names it: ErrConnectionLost, unsent, when nats.go refused the call for that
// connection's close on a client still running and the connection is still the client's
// (heldNow), and err as it is otherwise, so a message acknowledged on a connection the client has
// replaced answers nats.ErrConnectionClosed, which no retry answers. nil takes no lock.
func (p *SessionPull) lostIfClosed(err error) error {
	if err == nil {
		return nil
	}
	return p.client.lostIfClosed(err, p.client.heldNow(p.conn), true)
}

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
// It is idempotent: the drain runs once and every call answers its result.
func (p *SessionPull) Detach() error {
	p.detachOnce.Do(func() {
		// The inbox is drained, not unsubscribed: an unsubscribe drops what nats.go holds
		// unread, which is what Release returns. A drain hands those messages to receive, which
		// queues them.
		if err := p.sub.Drain(); err != nil {
			// Drain fails only on a closed connection (nats.go v1.55.0: ErrConnectionClosed;
			// its other error, a subscription with no connection, cannot arise here), which is
			// ErrConnectionLost while the connection is still the running client's (lostIfClosed).
			// The close has already ended the subscription and thrown away what nats.go held
			// unread, so those messages reach neither Release nor the server and are delivered
			// again only once their ack wait has passed. The poll below would end at once on the
			// invalid subscription and report nothing, so the failure is returned here instead.
			// Stop is kept as a guard for any other drain failure; on a closed connection it does
			// nothing.
			p.Stop()
			p.detachErr = fmt.Errorf("bus: the inbox subscription of the pull of consumer %q could not be drained, so the messages nats.go held unread are lost: %w", p.name, p.lostIfClosed(err))
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
		held = append(held, sessionAck{msg: msg, pull: p})
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
