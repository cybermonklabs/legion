package bus

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// A create or an update of a session consumer carries the filter the consumer is to hold, and its
// caller can stop waiting for the answer while the server still holds the request: its context
// ends first, or the flush behind its publish cannot confirm it. The server applies such a write
// when it gets to it, and nats-server v2.15.1 gives no order between it and a later call of the
// same connection: every JetStream API request goes to a pool of worker goroutines, up to 16, fed
// from two queues, with the info requests taken only when the other queue is empty
// (server/jetstream_api.go v2.15.1, apiDispatch and processJSAPIRoutedRequests, lines 1011-1092).
// So a consumer info sent after the write can be answered before the write is applied, and an
// update sent after it can be applied before it. A caller that judged the consumer by such an
// info, or wrote it again, can see its filter right and leave; the write it gave up on then lands,
// and the filter it carries - a plan nothing stored, a row the bucket has replaced - stands with
// nothing to read the consumer again. The server holds no conditional consumer update a caller
// could make that write lose to.
//
// So the write's subscription outlives its caller (UnansweredWrite): the one who stopped waiting
// hands it to whoever judges the consumer afterwards (OnUnansweredWrite), which waits for the
// answer and judges the consumer again once the write may have landed (cmd/listener's
// sessionDelivery.unansweredWrite). The server answers a write it applied or refused, so a read
// made after that answer sees what the write did. It answers no write it dropped: nats-server
// v2.15.1 drops every request queued on its JetStream API once that queue reaches its limit, and
// answers none of them (server/jetstream_api.go, apiDispatch, lines 1029-1033, queue.drain()).
// Such a write changed nothing, and its wait lasts until the connection's next reconnect or the
// listener's stop (UnansweredWrite.Wait).

// ServerPingInterval and ServerPingMax are nats-server's own ping settings, ping_interval and
// ping_max, for a server whose configuration sets neither (server/const.go v2.15.1,
// DEFAULT_PING_INTERVAL and DEFAULT_PING_MAX_OUT; TestTheServerPingDefaultsAreTheServersOwn holds
// them to the pinned server). A hub this repository renders keeps them, or sets the callout's
// shorter lines (infra/core calloutPingLines, 20 s and 2), whose stale limit, 80 s, is below the
// defaults' 8 minutes; a listener of such a hub that keeps the defaults watches longer than it
// needs to and no shorter (cmd/listener's ENVOY_NATS_PING_INTERVAL and ENVOY_NATS_PING_MAX).
const (
	ServerPingInterval = 2 * time.Minute
	ServerPingMax      = 2
)

// StaleConnectionLimit is how long nats-server keeps reading a client connection that it has
// read nothing from, for a server with ping_interval pingInterval and ping_max pingMax. After that
// the server has closed the connection as stale, and no byte sent on it reaches the server's
// JetStream API any more.
//
// It comes from the server's ping timer (server/client.go v2.15.1, processPingTimer, lines
// 5943-6001). The timer fires every ping_interval. A tick less than one interval after the
// connection's last read sends nothing. Each later tick sends a PING, and the tick that would put
// more than ping_max PINGs out closes the connection. The first PING goes out at the first tick
// one interval or more after the last read, which is one to two intervals after it, and the close
// comes ping_max ticks after that. So the close comes (ping_max + 1) to (ping_max + 2) intervals
// after the last read, and this limit is the later of the two. Bytes that reach the server before
// the close, such as a write that was still on its path, are read and can land; each such read
// starts the count again.
func StaleConnectionLimit(pingInterval time.Duration, pingMax int) time.Duration {
	return time.Duration(pingMax+2) * pingInterval
}

// UnansweredWriteEnd is how the wait for an unanswered write's answer ended (UnansweredWrite.Wait).
type UnansweredWriteEnd int

const (
	// WriteApplied is the server's answer that it applied the write: the consumer carries what
	// the write carried, as of that answer.
	WriteApplied UnansweredWriteEnd = iota
	// WriteRefused is the server's answer that it refused the write, which changed nothing.
	WriteRefused
	// WriteAnswerLost is a connection event that leaves the answer unknown: the connection the
	// write went out on reconnected, closed or was replaced. The server may have applied the
	// write before that event, or may hold it still; either way a read made from then on is the
	// only judge of the consumer left.
	WriteAnswerLost
	// WriteNotAwaited is a wait its caller ended (the stop it was given closed) before any of the
	// three above.
	WriteNotAwaited
)

// UnansweredWrite is a create or an update of SessionID's consumer whose caller stopped waiting
// for its answer after the request was sent (SessionConsumers.writeRequest), with the
// subscription its answer arrives on still open. It is handed to the function OnUnansweredWrite
// set, once, and that function either waits for it (Wait) or drops it (Release).
type UnansweredWrite struct {
	// SessionID is the session whose consumer the write is for.
	SessionID string
	// Late is what the write would leave on the consumer if it landed: the filters it carries and
	// the consumer's position before it was sent (LateWrite).
	Late LateWrite

	client     *Client
	conn       *nats.Conn
	reconnects uint64
	replies    chan *nats.Msg
	sub        *nats.Subscription
	release    sync.Once
}

// Wait waits for w's answer until stop closes, and reports how it ended (UnansweredWriteEnd). A
// connection event of w's client wakes it, and it ends as WriteAnswerLost once the connection w
// went out on has reconnected (its reconnect count moved), closed or been replaced, since
// nats.go answers nothing for a request whose answer it lost with the socket. It drops the
// subscription before it returns.
//
// After a WriteAnswerLost the write can still land: the server goes on reading the old connection
// until it closes it as stale, which is at most StaleConnectionLimit after its last read, so a
// write that was still on its path when the connection came back is read and applied then. The
// listener reads the consumer each pause until that limit has passed (cmd/listener's
// sessionDelivery.unansweredWrite).
//
// A write the server dropped is never answered, and its connection stays up, so Wait ends only at
// the connection's next reconnect or at stop. Until then each such write holds one goroutine of
// the caller's and one subscription (TestADroppedWriteHoldsItsWaitUntilTheNextReconnectOrTheStop).
// No read of the consumer ends it sooner: the server can answer a read sent after a write before
// it applies that write, since a pool of workers takes its API requests (the comment at this
// file's top), so a read answered tells nothing of a write still in a worker's hands.
func (w *UnansweredWrite) Wait(stop <-chan struct{}) UnansweredWriteEnd {
	defer w.Release()
	for {
		changed := w.client.ConnectionChanged()
		if w.lost() {
			return WriteAnswerLost
		}
		select {
		case reply := <-w.replies:
			return writeAnswer(reply)
		case <-changed:
		case <-stop:
			return WriteNotAwaited
		}
	}
}

// Release drops w's subscription; a second call does nothing.
func (w *UnansweredWrite) Release() {
	w.release.Do(func() { _ = w.sub.Unsubscribe() })
}

// lost reports whether the connection w went out on can no longer carry its answer.
func (w *UnansweredWrite) lost() bool {
	return w.client.Connection() != w.conn || w.conn.IsClosed() || w.conn.Stats().Reconnects != w.reconnects
}

// LateWrite is a consumer write whose answer did not come back, as a later write of the same
// consumer judges it (SessionConsumers.EnsureAfter). Filters are the filters it carries, and
// Before is the consumer's position before it was sent: the last stream sequence the consumer
// had examined when the write's own read of it was answered, or the sequence before the start of
// a create. Landed is set when the write's answer said the server applied it, and Last when the
// judgment is the write's last one: the server can apply it no more, so a landing that a later
// write covered before anyone read the consumer is judged then.
//
// A late write that lands with filters that lack a filter of the stored row costs more than the
// filter: each pull while it stands moves the consumer's position past the messages stored on
// the missing topics, and a filter set right afterwards does not bring them back. Every such
// message has a sequence above Before, so a consumer taken back to the first of them delivers
// each of them again.
type LateWrite struct {
	Filters []string
	Before  uint64
	Landed  bool
	Last    bool
}

// rewindPoint is the first stream sequence a consumer read as info must start at again to
// deliver every message that a write of late may have made it skip, and whether there is one;
// seen reports, for each of late, whether the consumer stands on that write's filters where the
// stored row asks for others, which is that write's landing read (or a writer that wrote the same
// filters). A write may have made the consumer skip a message when four things hold: the write
// landed, by its own answer, by its landing read, or by its last judgment; its filters lack one
// of filters, the ones the stored row asks for; the stream holds a message on such a missing
// filter after Before (NextStoredMessage); and the consumer has examined that message's
// sequence. Where one write may have, the point is the lowest such sequence over every write of
// late, landed or not, so a consumer started there again holds above its new start every message
// any of them could still make it skip.
func (s *SessionConsumers) rewindPoint(ctx context.Context, info *jetstream.ConsumerInfo, filters []string, late []LateWrite) (uint64, bool, []bool, error) {
	seen := make([]bool, len(late))
	landed := make([]bool, len(late))
	for i, w := range late {
		seen[i] = sameFilters(info.Config.FilterSubjects, w.Filters) && !sameFilters(w.Filters, filters)
		landed[i] = w.Landed || w.Last || seen[i]
	}
	if _, found, err := s.skipped(ctx, info, filters, late, landed); err != nil || !found {
		return 0, false, seen, err
	}
	point, _, err := s.skipped(ctx, info, filters, late, nil)
	return point, err == nil, seen, err
}

// skipped is the lowest stream sequence of a message that a write of late (of those only marks,
// where only is not nil) may have made a consumer read as info skip - one on a filter of filters
// the write lacks, after the write's Before and at or below what the consumer has examined - and
// whether there is one.
func (s *SessionConsumers) skipped(ctx context.Context, info *jetstream.ConsumerInfo, filters []string, late []LateWrite, only []bool) (uint64, bool, error) {
	var point uint64
	found := false
	for i, w := range late {
		if (only != nil && !only[i]) || info.Delivered.Stream <= w.Before {
			continue
		}
		for _, f := range filters {
			if slices.Contains(w.Filters, f) {
				continue
			}
			msg, err := s.client.NextStoredMessage(ctx, f, w.Before+1)
			if errors.Is(err, nats.ErrMsgNotFound) {
				continue
			}
			if err != nil {
				return 0, false, err
			}
			if msg.Sequence <= info.Delivered.Stream && (!found || msg.Sequence < point) {
				point, found = msg.Sequence, true
			}
		}
	}
	return point, found, nil
}

// sameFilters reports whether a and b hold the same filters, in any order.
func sameFilters(a, b []string) bool {
	return len(a) == len(b) && !slices.ContainsFunc(a, func(f string) bool { return !slices.Contains(b, f) })
}

// rewind deletes consumer name of sessionID, read as info, and creates it again on filters at
// stream sequence point, or at the sequence after its ack floor when that is lower, so the
// messages it delivered and that were not acked are delivered again too. It takes the steps of
// RestartAtFloor, the pending start among them, so a create that fails after the delete leaves
// the start for the next call. The session's plugin drops by identity a message it was handed
// before (create's doc names its ledger).
func (s *SessionConsumers) rewind(ctx context.Context, sessionID, name string, info *jetstream.ConsumerInfo, filters []string, point uint64) (SessionConsumerStart, error) {
	startSeq := min(ackFloor(info)+1, point)
	s.setPendingStart(name, startSeq)
	if err := s.delete(ctx, name); err != nil {
		return SessionConsumerStart{}, err
	}
	start, err := s.create(ctx, sessionID, name, filters, startSeq, false)
	if err != nil {
		return SessionConsumerStart{}, err
	}
	s.setPendingStart(name, 0)
	start.Rewound = true
	return start, nil
}

// writeAnswer is what the answer to a consumer create or update says the server did with it: a
// no-responders status or an error in the answer is a write the server did not apply.
func writeAnswer(reply *nats.Msg) UnansweredWriteEnd {
	if len(reply.Data) == 0 && reply.Header.Get("Status") == "503" {
		return WriteRefused
	}
	var response consumerResponse
	if err := json.Unmarshal(reply.Data, &response); err != nil || response.Error != nil {
		return WriteRefused
	}
	return WriteApplied
}

// OnUnansweredWrite sets the function each unanswered create or update is handed to
// (UnansweredWrite), on the goroutine of the call that stopped waiting, before that call returns
// its error; nil, the default, drops the write's subscription at once. The listener sets it once,
// when it opens its session-consumer connection.
func (s *SessionConsumers) OnUnansweredWrite(handle func(*UnansweredWrite)) {
	s.unansweredMu.Lock()
	defer s.unansweredMu.Unlock()
	s.onUnanswered = handle
}

// writeRequest sends data to subject - a consumer create or update of sessionID's consumer - and
// returns its one answer, as requestConfirmed does: its publish is confirmed, so a write the
// connection's grant may not make returns ErrPublishDenied at once, and a ctx with no deadline
// waits storedMessageWait. Where requestConfirmed drops its reply subscription however it returns,
// this one keeps it when the request was sent and the caller stops waiting for its answer - ctx
// ended, or the flush behind the publish failed or could not confirm it - and hands the write to
// the function OnUnansweredWrite set (UnansweredWrite), since the server may still apply it.
//
// The answer is taken over a channel subscription of this request's own, as RequestCore takes
// one, so a write handed on holds a channel of nats.RequestChanLen replies and not one of
// nats.Options.SubChanLen. Such a channel is not closed when the connection closes, so the wait
// below also wakes at each connection event and ends with nats.ErrConnectionClosed on a closed
// connection, as requestConfirmed's does.
func (s *SessionConsumers) writeRequest(ctx context.Context, sessionID, subject string, data []byte, late LateWrite) (*nats.Msg, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, storedMessageWait)
		defer cancel()
	}
	if err := s.client.ensureConnWithContext(ctx); err != nil {
		return nil, err
	}
	conn := s.client.Connection()
	inbox := conn.NewInbox()
	replies := make(chan *nats.Msg, nats.RequestChanLen)
	sub, err := conn.ChanSubscribe(inbox, replies)
	if err != nil {
		return nil, err
	}
	reconnects := conn.Stats().Reconnects
	sent := false
	err = s.client.publishConfirmed(ctx, conn, &nats.Msg{Subject: subject, Reply: inbox, Data: data}, func(msg *nats.Msg) error {
		err := conn.PublishMsg(msg)
		sent = err == nil
		return err
	})
	unanswered := func() {
		s.unanswered(&UnansweredWrite{SessionID: sessionID, Late: late, client: s.client, conn: conn, reconnects: reconnects, replies: replies, sub: sub})
	}
	if err != nil && !errors.Is(err, ErrPublishUnconfirmed) {
		// A publish refused before it left, or one the server refused under the grant, changed
		// nothing; a publish made whose flush then failed is on its way all the same.
		if sent && !errors.Is(err, ErrPublishDenied) {
			unanswered()
		} else {
			_ = sub.Unsubscribe()
		}
		return nil, err
	}
	for {
		changed := s.client.ConnectionChanged()
		if conn.IsClosed() {
			_ = sub.Unsubscribe()
			return nil, cmp.Or(err, nats.ErrConnectionClosed)
		}
		select {
		case reply := <-replies:
			_ = sub.Unsubscribe()
			if len(reply.Data) == 0 && reply.Header.Get("Status") == "503" {
				return nil, nats.ErrNoResponders
			}
			return reply, nil
		case <-changed:
		case <-ctx.Done():
			unanswered()
			return nil, cmp.Or(err, ctx.Err())
		}
	}
}

// unanswered hands w to the function OnUnansweredWrite set, or drops it where none is set.
func (s *SessionConsumers) unanswered(w *UnansweredWrite) {
	s.unansweredMu.Lock()
	handle := s.onUnanswered
	s.unansweredMu.Unlock()
	if handle == nil {
		w.Release()
		return
	}
	handle(w)
}
