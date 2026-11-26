package intake

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A consumer working a backlog takes each message's acknowledgement window for that message's own
// work. JetStream starts that window when it delivers a message, not when the consumer's loop
// reaches it, so a message the client library fetched into its buffer spends the window queued
// behind the ones before it: with the library's default batch of 500, every message past the first
// few is redelivered while the consumer is still working, and each redelivery is prepared again —
// the Dispatch read an unrecorded issue's event makes — and takes the fact lock again before it is
// found already processed. That is work a consumer that is already behind pays instead of draining.
//
// What this row guards, and the only thing it guards, is consumerFetchBatch: it holds the fact
// handler on the first event of a backlog of twelve and, while that fact is in hand, reads what
// JetStream says the consumer owes an acknowledgement for. A consumer that fetches
// consumerFetchBatch messages owes no more than that however long a fact takes, so nothing it
// holds can spend its window behind another message's work.
//
// What becomes of a held backlog afterwards — every event delivered once and prepared once — is
// TestAFactHeldPastItsAcknowledgementWindowIsDeliveredOnceAndPreparedOnce's claim, and counting it
// here as well would say nothing about the fetch batch: once the handler is released the facts
// finish in milliseconds, and a larger batch would pass such a count.
func TestABacklogIsDeliveredOnceWhileEachFactTakesItsAcknowledgementWindow(t *testing.T) {
	t.Parallel()
	pool := migratedPool(t)
	js, stream := testJetStream(t)
	spec := consumerSpec(&lockedBuffer{})
	// A hang guard, not the subject. The handlings here make no read the window bounds:
	// consumerSpec sets no PrepareRepository, and a Dispatch issue event names no pull request
	// (recordedPullRequest). The read of what the consumer owes is this row's read only while the
	// hold lasts, so nothing but the row's own stop ends the hold: no timer frees it. A timer let
	// the machine decide that: at consumerSpec's 200 ms it could free the backlog's first fact
	// before the read is made, and at one minute, or at any window, a read the machine delayed
	// past it was made after the release, once the rest of the backlog had drained, so the row
	// passed whatever the consumer had fetched. The window is what is left of the test binary's
	// deadline less 30 s (testwait.Remaining). While the hold lasts nothing else reaches this
	// consumer, a reconnect excepted (the delivery mechanism: ConsumerSpec.HandlerLimit's comment
	// in consume.go), and the progress keeper reports the held fact in progress every third of
	// the window (spec.AckWait/3 in consume.go). The stop deferred further down ends the hold by
	// cancelling the handling, so the fact's transaction fails on the cancelled context and the
	// handling naks the message as the consumer stops.
	spec.AckWait = testwait.Remaining(t)
	consumers, err := OpenConsumers(context.Background(), js, pool, spec)
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	const backlog = 12
	for i := range backlog {
		key := fmt.Sprintf("CAPTURE-%d", 100+i)
		publish(t, js, "notifications.dispatch.issue."+key+".issue.updated", envelopeJSON(t, fmt.Sprintf("dispatch-%d", i+1), "dispatch",
			fmt.Sprintf(`{"id":%d,"issue_key":%q,"seq":1,"notify":true,"type":"issue.updated","actor":{"kind":"user","id":"someone"},`+
				`"payload":{"key":%q,"status":"backlog","title":"a backlog event","parent":null,"rank":"A","labels":[],"resolved_repo":"sjawhar/legion"}}`, i+1, key, key)))
	}

	held := newHeldOnce()
	defer runConsumers(t, consumers, handlerFunc(func(ctx context.Context, _ pgx.Tx, _ Fact) (Result, error) {
		held.hold(ctx)
		return Result{}, nil
	}))()
	// The read below is this row's read once the backlog's first fact is in hand. The hold has no
	// timer: the stop deferred above ends the handling, and with it the hold, after the read.
	select {
	case <-held.entered:
	case <-time.After(testwait.Remaining(t)):
		t.Fatal("the backlog's first fact never reached the handler")
	}

	// The read waits for the server until the row's hang guards end (testwait.Remaining), in place
	// of the five seconds nats.go gives a request whose context has no deadline.
	read, cancelRead := context.WithTimeout(context.Background(), testwait.Remaining(t))
	defer cancelRead()
	item, err := stream.Consumer(read, dispatchConsumerName(spec.Project))
	if err != nil {
		t.Fatalf("read the Dispatch consumer: %v", err)
	}
	info, err := item.Info(read)
	if err != nil {
		t.Fatalf("read the Dispatch consumer's info: %v", err)
	}
	// The literal is deliberate: this row is what guards consumerFetchBatch, so it must not be
	// written in terms of the constant it guards - raising the constant would raise this bar with
	// it and the row would pass whatever the library then holds.
	if info.NumAckPending > 1 {
		t.Fatalf("the Dispatch consumer owes an acknowledgement for %d messages while the handler holds the backlog's first fact, want at most the one it fetches: the rest spend their window queued behind that work",
			info.NumAckPending)
	}
}
