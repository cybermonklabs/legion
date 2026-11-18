package bus

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// withBeforeStoredSend has the client run before in sendStored (beforeStoredSend): between its
// read of the client's JetStream context and its first send on it, with resend false, and in its
// branch for a closed connection, before the wait for the connection the second send goes out on,
// with resend true. A row replaces the client's connection at the first, and takes the replacement
// down at the second.
func withBeforeStoredSend(before func(resend bool)) ConnectOption {
	return clientOption(func(o *connectOpts) { o.beforeStoredSend = before })
}

// newestMsgID is the Nats-Msg-Id of the newest message the stream holds on subject, read on a
// connection of its own.
func newestMsgID(t *testing.T, subject string) string {
	t.Helper()
	js, err := testnats.Connect(t, testnats.URL(t)).JetStream()
	if err != nil {
		t.Fatalf("open JetStream on the server: %v", err)
	}
	msg, err := js.GetLastMsg(Stream, subject)
	if err != nil {
		t.Fatalf("read the newest message on %s: %v", subject, err)
	}
	return msg.Header.Get(nats.MsgIdHdr)
}

// A delete whose resume record's send goes out on the JetStream context of a connection the
// session consumers' client has just replaced, which nats.go refuses with its close
// (nats.ErrConnectionClosed, nats.go v1.53.1 nats.go:4573-4576), sends the record again on the
// replacement, and the stream stores it once: the write carries a Nats-Msg-Id it drew for its own
// sends (recordResumeFloorAt), and its second send carries the same one. The delete then answers
// the floor it recorded and deletes the consumer, where an answer of the closed error, which
// Reconnected does not match, would have the listener's delete log a failure and leave the
// consumer to the sweep (cmd/listener's deleteConsumer). The row makes that order certain rather
// than likely: the seam runs after the client has read its JetStream context and before the send
// (withBeforeStoredSend), and closes the connection and waits there for the client to replace it,
// so the send is made on the closed connection's context and bound to the replacement's life, which
// has not ended (bindRequest), however loaded the box. The replacement is still up when the seam
// runs again, before the wait for the second send's connection, so that wait (connForRequest)
// serves it at once; the row after this one takes it down there. A later record of the session is
// a message of its own under an id of its own, so the stream holds both and the reader takes the
// newest (resumeFloor), each record carrying its id.
//
// The controls: this row with publishStoredRaw passing sendStored no id, and with sendStored's
// branch for a closed connection taken out; in each the delete answers the closed error and the
// stream holds no record.
func TestAResumeRecordWhoseConnectionClosedUnderItIsSentAgainAndStoredOnce(t *testing.T) {
	uri := testnats.URL(t)
	owner, err := ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect the stream's owner: %v", err)
	}
	t.Cleanup(owner.Close)
	var stage atomic.Pointer[func(resend bool)]
	consumers, err := ConnectSessionConsumers([]string{uri}, 0, nil, withBeforeStoredSend(func(resend bool) {
		if run := stage.Load(); run != nil {
			(*run)(resend)
		}
	}))
	if err != nil {
		t.Fatalf("connect session consumers: %v", err)
	}
	t.Cleanup(consumers.Close)
	ctx, cancel := context.WithTimeout(context.Background(), ownWait(t))
	defer cancel()
	const session = "ses_resume_closed"
	agent := contracts.LegacyAgentSubject(session)
	subject := SessionResumeSubject(session)
	if _, err := consumers.Ensure(ctx, session, []string{agent}, 0); err != nil {
		t.Fatalf("ensure the session's consumer: %v", err)
	}

	client := consumers.client
	closed := client.Connection()
	var replaced *nats.Conn
	var replaceErr error
	var sent, upAtResend bool
	run := func(resend bool) {
		if resend {
			upAtResend = client.Connected()
			stage.Store(nil)
			return
		}
		sent = true
		closed.Close()
		replaceErr = client.ensureConnWithContext(ctx)
		replaced = client.Connection()
	}
	stage.Store(&run)
	floor, err := consumers.Delete(ctx, session, nil)
	stage.Store(nil)
	if !sent {
		t.Fatalf("the delete made no stored send (%d, %v), so no connection closed under its resume record", floor, err)
	}
	if replaceErr != nil || replaced == closed || !replaced.IsConnected() {
		t.Fatalf("the client did not replace its closed connection before the record's send: %v", replaceErr)
	}
	if err != nil {
		t.Fatalf("the delete whose resume record's connection closed under it answered %v, want the record sent again on the replacement and the consumer deleted", err)
	}
	if !upAtResend {
		t.Fatalf("the record reached no wait for its second send with the replacement up (floor %d): it did not take sendStored's branch for a closed connection", floor)
	}
	if _, err := consumers.Info(ctx, session); !errors.Is(err, ErrSessionConsumerNotFound) {
		t.Fatalf("the consumer's info after the delete = %v, want ErrSessionConsumerNotFound", err)
	}
	if stored := storedOn(t, subject); stored != 1 {
		t.Fatalf("the stream holds %d resume records of %s, want the one its second send stored", stored, session)
	}
	first := newestMsgID(t, subject)
	if first == "" {
		t.Fatalf("the resume record of %s carries no Nats-Msg-Id", session)
	}
	streamID := streamIDOf(t, consumers)
	if got, recorded, err := consumers.resumeFloor(ctx, session, streamID); err != nil || !recorded || got != floor {
		t.Fatalf("the resume record after the delete = %d, %t, %v; want the floor %d the delete answered", got, recorded, err, floor)
	}

	later := publishStored(t, owner, agent, "stored after the delete")
	if recorded, err := consumers.recordResumeFloorAt(ctx, session, later); err != nil || recorded != later {
		t.Fatalf("a later resume record of %s = %d, %v; want the floor %d", session, recorded, err, later)
	}
	if stored := storedOn(t, subject); stored != 2 {
		t.Fatalf("the stream holds %d resume records of %s, want 2: the later write is a message of its own", stored, session)
	}
	if second := newestMsgID(t, subject); second == "" || second == first {
		t.Fatalf("the later resume record carries Nats-Msg-Id %q beside the first's %q, want an id of its own", second, first)
	}
	if got, recorded, err := consumers.resumeFloor(ctx, session, streamID); err != nil || !recorded || got != later {
		t.Fatalf("the resume record after the later write = %d, %t, %v; want the newest, floor %d", got, recorded, err, later)
	}
}

// A delete whose resume record's send goes out on the JetStream context of a connection the
// session consumers' client has just replaced, as in the row above, and whose replacement is down
// when the record would be sent again, answers the loss at once (ErrConnectionLost, connForRequest)
// rather than waiting for the connection with the session's lock held, which the listener's
// delete holds while it calls this (cmd/listener's deleteConsumer). It deletes nothing and writes
// no record, so the consumer stands. Once the connection is back (Reconnected), the delete made
// again, as the listener's delete makes it after the wait and the sweep's next pass would, records
// the floor and deletes the consumer.
//
// The order is made certain rather than likely by the seam (withBeforeStoredSend): before the
// first send it closes the connection and waits for the client to replace it, through the hold;
// before the wait for the second send's connection it cuts the hold (testnats.ReplyHold.Cut), so
// no reconnect can land, and waits until the replacement is not connected. The answer then comes
// while the hold still refuses every reconnect and the delete's own context runs, so a delete that
// waited for the connection could end only at a deadline, the record's own bound (publishBound)
// or that context's, and not with ErrConnectionLost, which fails the row.
//
// The control is this row with sendStored's branch for a closed connection waiting through
// ensureConnWithContext, as at 7ae8da8a4f: the delete waits, with the session's lock held in the
// listener, for a connection the hold refuses, and answers the record's own bound, `waiting for
// NATS to reconnect: context deadline exceeded`.
func TestAResumeRecordWhoseReplacementIsDownAnswersTheLossAtOnceAndDeletesNothing(t *testing.T) {
	uri := testnats.URL(t)
	owner, err := ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect the stream's owner: %v", err)
	}
	t.Cleanup(owner.Close)
	hold := testnats.StartReplyHold(t, uri)
	var stage atomic.Pointer[func(resend bool)]
	consumers, err := ConnectSessionConsumers([]string{hold.URL()}, 0, nil, withBeforeStoredSend(func(resend bool) {
		if run := stage.Load(); run != nil {
			(*run)(resend)
		}
	}))
	if err != nil {
		t.Fatalf("connect session consumers through the hold: %v", err)
	}
	t.Cleanup(consumers.Close)
	t.Cleanup(hold.Restore)
	ctx, cancel := context.WithTimeout(context.Background(), ownWait(t))
	defer cancel()
	const session = "ses_resume_closed_down"
	agent := contracts.LegacyAgentSubject(session)
	subject := SessionResumeSubject(session)
	if _, err := consumers.Ensure(ctx, session, []string{agent}, 0); err != nil {
		t.Fatalf("ensure the session's consumer: %v", err)
	}

	client := consumers.client
	closed := client.Connection()
	var replaced *nats.Conn
	var replaceErr error
	var sent, cut bool
	run := func(resend bool) {
		if !resend {
			sent = true
			closed.Close()
			replaceErr = client.ensureConnWithContext(ctx)
			replaced = client.Connection()
			return
		}
		cut = true
		stage.Store(nil)
		hold.Cut()
		testwait.WithinPolling(t, testnats.Remaining(t), time.Millisecond, "the replacement connection to drop under the cut",
			func() bool { return !replaced.IsConnected() })
	}
	stage.Store(&run)
	floor, err := consumers.Delete(ctx, session, nil)
	connected, ended := client.Connected(), ctx.Err()
	stage.Store(nil)
	if !sent {
		t.Fatalf("the delete made no stored send (%d, %v), so no connection closed under its resume record", floor, err)
	}
	if replaceErr != nil || replaced == closed {
		t.Fatalf("the client did not replace its closed connection before the record's send: %v", replaceErr)
	}
	if !cut {
		t.Fatalf("the delete answered %d, %v before any wait for a second send of its record, so the hold was never cut", floor, err)
	}
	if !errors.Is(err, ErrConnectionLost) || ended != nil || connected {
		t.Fatalf("the delete whose record's replacement connection was down answered %d, %v (its context: %v, connected: %t); want ErrConnectionLost at once, while its context runs and the client is not connected", floor, err, ended, connected)
	}
	if floor != 0 {
		t.Fatalf("the delete that wrote no record answered floor %d, want 0", floor)
	}
	if stored := storedOn(t, subject); stored != 0 {
		t.Fatalf("the stream holds %d resume records of %s, want none: the record was answered unsent", stored, session)
	}

	hold.Restore()
	if !Reconnected(ctx, err) {
		t.Fatalf("the client did not recover its connection within the row's wait: %v", err)
	}
	if _, err := consumers.Info(ctx, session); err != nil {
		t.Fatalf("the consumer's info after the delete that wrote no record = %v, want the consumer still there", err)
	}
	floor, err = consumers.Delete(ctx, session, nil)
	if err != nil {
		t.Fatalf("the delete made again once the connection was back answered %v, want the floor recorded and the consumer deleted", err)
	}
	if _, err := consumers.Info(ctx, session); !errors.Is(err, ErrSessionConsumerNotFound) {
		t.Fatalf("the consumer's info after the second delete = %v, want ErrSessionConsumerNotFound", err)
	}
	if stored := storedOn(t, subject); stored != 1 {
		t.Fatalf("the stream holds %d resume records of %s after the second delete, want 1", stored, session)
	}
	if got, recorded, err := consumers.resumeFloor(ctx, session, streamIDOf(t, consumers)); err != nil || !recorded || got != floor {
		t.Fatalf("the resume record after the second delete = %d, %t, %v; want the floor %d it answered", got, recorded, err, floor)
	}
}

// A resume record whose acknowledgement the connection's drop lost, on the session consumers'
// client, answers ErrConnectionLost and is not sent again, its id notwithstanding: that client
// answers a lost connection to its caller at once (withLossAtOnce), and the caller releases what it
// holds, waits for the connection and retries (Reconnected, cmd/listener's deleteConsumer). The
// stream holds the one copy the first send stored, and the retry is a write of its own under an id
// of its own, which the stream stores beside it as the newest record. The hold withholds the
// record's acknowledgement, so the first send reached the stream, and then drops the connection
// and holds nothing more (testnats.ReplyHold.Drop).
//
// The control is this row with sendStored sending a publish again after a lost answer on that
// client too: the record is acknowledged.
func TestAResumeRecordWhoseAckTheDropLostAnswersTheLossOnTheSessionConsumersClient(t *testing.T) {
	uri := testnats.URL(t)
	owner, err := ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect the stream's owner: %v", err)
	}
	t.Cleanup(owner.Close)
	hold := testnats.StartReplyHold(t, uri)
	consumers, err := ConnectSessionConsumers([]string{hold.URL()}, 0, nil)
	if err != nil {
		t.Fatalf("connect session consumers through the hold: %v", err)
	}
	t.Cleanup(consumers.Close)
	ctx, cancel := context.WithTimeout(context.Background(), ownWait(t))
	defer cancel()
	const session = "ses_resume_lost"
	subject := SessionResumeSubject(session)

	held := hold.Hold(subject)
	answered := make(chan error, 1)
	go func() {
		_, err := consumers.recordResumeFloorAt(ctx, session, 0)
		answered <- err
	}()
	select {
	case <-held.Withheld():
	case <-time.After(testnats.Remaining(t)):
		t.Fatal("the hold withheld no acknowledgement of the resume record by the test's deadline")
	}
	hold.Drop()
	select {
	case err = <-answered:
	case <-time.After(testnats.Remaining(t)):
		t.Fatal("the resume record whose acknowledgement the drop lost was still waiting at the test's deadline")
	}
	if !errors.Is(err, ErrConnectionLost) {
		t.Fatalf("the resume record whose acknowledgement the drop lost answered %v, want ErrConnectionLost: the session consumers' client answers the loss to its caller", err)
	}
	if stored := storedOn(t, subject); stored != 1 {
		t.Fatalf("the stream holds %d resume records of %s, want the one its first send stored", stored, session)
	}
	first := newestMsgID(t, subject)

	if !Reconnected(ctx, err) {
		t.Fatalf("the client did not recover its connection within the row's wait: %v", err)
	}
	later := publishStored(t, owner, contracts.LegacyAgentSubject(session), "stored before the retry")
	if recorded, err := consumers.recordResumeFloorAt(ctx, session, later); err != nil || recorded != later {
		t.Fatalf("the caller's retry of the resume record = %d, %v; want the floor %d", recorded, err, later)
	}
	if stored := storedOn(t, subject); stored != 2 {
		t.Fatalf("the stream holds %d resume records of %s, want 2: the retry is a write of its own", stored, session)
	}
	if second := newestMsgID(t, subject); second == "" || second == first {
		t.Fatalf("the retry's resume record carries Nats-Msg-Id %q beside the first's %q, want an id of its own", second, first)
	}
	if got, recorded, err := consumers.resumeFloor(ctx, session, streamIDOf(t, consumers)); err != nil || !recorded || got != later {
		t.Fatalf("the resume record after the retry = %d, %t, %v; want the newest, floor %d", got, recorded, err, later)
	}
}
