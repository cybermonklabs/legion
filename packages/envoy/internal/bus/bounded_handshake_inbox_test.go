package bus

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A request of the client's own whose reply is waiting in its inbox when nats.go begins a
// reconnect's handshake answers at once, and its inbox ends off its goroutine. Each of these
// requests unsubscribes on its own goroutine only for a reply it took while the connection is still
// in the life its inbox was subscribed in (endReplies): the probe's request (RequestCore), a
// session consumer's request (requestConfirmed) and a role forward's wait for the stream
// (PendingForward.Wait). A wait that counted any reply it took as one over a live link, also one it
// took from the inbox once the connection changed, would unsubscribe on its own goroutine, which
// waits for nats.go's connection lock. The request would then answer only when the attempt ends,
// at the client's connect timeout (connectTimeout, 5 s).
//
// The row sends the probe and the consumer request to a subject a responder answers, and the
// forward to a subject the stream captures. The probe and the consumer request each stop as their
// wait for the reply begins (the client's replyWaitBegins seam). The responder answers both. The
// server sends the client those answers before the answer to the client's own flush, so once that
// flush is answered, each answer is in its inbox. Once the stream's acknowledgement is in the
// forward's inbox too, the row holds the reconnect's handshake. Then it lets the two requests go on
// and calls the forward's Wait. Each answers within 1 s of the hold: the probe and the consumer
// request with the responder's answer, the forward nil. Every inbox is gone once the hold is
// released.
//
// The controls: the three inboxes are subscribed before the hold; the lock is still held, and no
// later attempt has reached the hold, when the three have answered. The negative controls make the
// waits count every reply as one over a live link:
// with stored.go's waits so changed, the probe and the consumer request answer only when the
// attempt ends; with nats.go's, the forward does.
func TestAClientsRequestAnsweredOnceItsLinkChangedEndsItsInboxOffItsGoroutine(t *testing.T) {
	// Serial (busTestParallelism): each answer has a wall-clock bound, heldBound and heldMargin.
	client, hold := heldHandshakeClient(t)
	conn := client.Connection()
	responder := testnats.Connect(t, testnats.URL(t))
	subject := fmt.Sprintf("held-handshake.answer.%d", time.Now().UnixNano())
	responded := make(chan struct{}, 2)
	if _, err := responder.Subscribe(subject, func(msg *nats.Msg) {
		if err := msg.Respond([]byte("answer")); err == nil {
			responded <- struct{}{}
		}
	}); err != nil {
		t.Fatalf("subscribe the responder: %v", err)
	}
	if err := responder.Flush(); err != nil {
		t.Fatalf("flush the responder: %v", err)
	}
	stored := contracts.LegacyAgentSubject(fmt.Sprintf("ses_held_handshake_client_answer_%d", time.Now().UnixNano()))
	before := conn.NumSubscriptions()

	waiting := make(chan struct{}, 2)
	goOn := make(chan struct{})
	var once sync.Once
	letGo := func() { once.Do(func() { close(goOn) }) }
	t.Cleanup(letGo)
	replyWait := func() {
		waiting <- struct{}{}
		<-goOn
	}
	client.replyWaitBegins.Store(&replyWait)
	t.Cleanup(func() { client.replyWaitBegins.Store(nil) })

	type answer struct {
		what  string
		reply *nats.Msg
		err   error
	}
	answers := make(chan answer, 3)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	go func() {
		reply, err := client.RequestCore(ctx, subject, conn.NewInbox(), nats.Header{"Envoy-Probe": []string{"held-handshake"}})
		answers <- answer{what: "RequestCore", reply: reply, err: err}
	}()
	go func() {
		reply, err := client.requestConfirmed(ctx, subject, []byte("{}"))
		answers <- answer{what: "requestConfirmed", reply: reply, err: err}
	}()
	forward, err := client.StartRequestStoredTo(stored, contracts.Envelope{EventID: "evt-held-handshake-client-answer", Topic: stored}, "held-handshake-client-answer", time.Minute)
	if err != nil {
		t.Fatalf("control: the forward's send answered %v, want it sent", err)
	}
	for range 2 {
		select {
		case <-waiting:
		case <-time.After(30 * time.Second):
			t.Fatal("control: a request did not begin its wait for the reply within 30 s")
		}
	}
	for range 2 {
		select {
		case <-responded:
		case <-time.After(30 * time.Second):
			t.Fatal("control: the responder did not answer both requests within 30 s")
		}
	}
	// The server takes the responder's answers before it answers the responder's flush, and sends
	// them to the client before it answers the client's own flush.
	if err := responder.Flush(); err != nil {
		t.Fatalf("control: flush the responder: %v", err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("control: flush the client's connection: %v", err)
	}
	testwait.WithinPolling(t, 30*time.Second, time.Millisecond, "the stream's acknowledgement to reach the forward's inbox",
		func() bool {
			pending, _, _ := forward.replies.Pending()
			return pending > 0
		})
	if subscribed := conn.NumSubscriptions() - before; subscribed < 3 {
		t.Fatalf("control: the requests hold %d subscriptions on the connection, want their three inboxes", subscribed)
	}

	hold.HoldReconnect(t, conn)
	attempt := hold.Attempts()
	letGo()
	go func() {
		_, err := forward.Wait()
		answers <- answer{what: "PendingForward.Wait", err: err}
	}()
	guard := time.NewTimer(heldBound + heldMargin)
	defer guard.Stop()
	got := map[string]answer{}
	for len(got) < 3 {
		select {
		case a := <-answers:
			got[a.what] = a
		case <-guard.C:
			var late []string
			for _, what := range []string{"RequestCore", "requestConfirmed", "PendingForward.Wait"} {
				if _, ok := got[what]; !ok {
					late = append(late, what)
				}
			}
			t.Fatalf("%v gave no answer within %s of the hold: its inbox's end waited for nats.go's lock", late, heldBound+heldMargin)
		}
	}
	if after := hold.Attempts(); after != attempt {
		t.Fatalf("the requests answered only after %d more handshake attempt(s) had reached the hold: they waited out the attempt held when they went on", after-attempt)
	}
	testnats.RequireLockHeld(t, conn)
	for _, what := range []string{"RequestCore", "requestConfirmed"} {
		if a := got[what]; a.err != nil || a.reply == nil || string(a.reply.Data) != "answer" {
			t.Errorf("%s whose answer was in its inbox answered %v (reply %v), want the responder's answer", what, a.err, a.reply)
		}
	}
	if err := got["PendingForward.Wait"].err; err != nil {
		t.Errorf("PendingForward.Wait whose acknowledgement was in its inbox answered %v, want nil", err)
	}

	releaseUntilConnected(t, hold, conn)
	testwait.WithinPolling(t, 30*time.Second, 10*time.Millisecond, "every request's inbox to be gone once the hold was released",
		func() bool { return conn.NumSubscriptions() == before })
}
