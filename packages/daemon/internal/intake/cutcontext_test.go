package intake

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/clock/clocktest"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The one row of this file is the other half of ackprogress_test.go's
// TestTheProgressSignalsHandlerIsCutAtItsLimit, and the two are worth reading together. That row
// holds its handler on the context consumeMessage hands it (heldOnce.hold, which waits on
// ctx.Done()) and proves that such a handling is cut at the limit: for a step that honours its
// context, the deadline is the end of the handling. This row holds its handler on a wait that
// never reads that context, and proves that such a handling is cut only when the step returns of
// its own accord, after the deadline.
//
// That is Acceptance A2 of dispatch://INFRA-1574, the claim behind the two sentences the issue
// puts in consume.go: what the intake cut guarantees, and what it relies on. consumeMessage runs
// the handling under spec.Clock.WithTimeout(ctx, spec.HandlerLimit) and then waits for it —
// `outcome := handleMessage(work, …)`. The deadline cancels a context; it ends no goroutine. So
// what the limit guarantees is that a handling which watches its context is cut at the limit, and
// what the limit relies on is exactly that watch: every step of handleMessage taking the context
// it is given and giving up on it. A step that ignores it keeps the message in the daemon's hands
// for as long as it runs, and the `an intake handler outran its limit and was cut` warning and
// the nak both wait for it. What brings the message back afterwards is ConsumerSpec.HandlerLimit's
// own account, which this file does not retell.
//
// The row is worth having because nothing else in the tree says so, and the tree reads the other
// way round without it. A reader of consumeMessage can take "a handler that hangs is cut at that
// limit" for a promise the daemon keeps whatever the handler does, and the neighbouring row,
// whose handler honours its context, agrees with that reading. This row is the one that does not,
// so a change which made the sentences promise more than the code gives — a wedged step ended,
// rather than a cancelled one cut — fails here.

// cutHandlerLimit is the limit this row gives one fact's handling. It runs on the consumers'
// clock, a fake the row moves past it while the step still runs, so its length is only the
// deadline the row expects armed and the limit the cut's line names.
const cutHandlerLimit = 400 * time.Millisecond

// heldPastTheLimit is how long the row goes on holding the step once the deadline has passed,
// before it reads what the daemon wrote: the 1.6 s the row's former sleep gave a cut that did not
// wait for its handler (a 2 s sleep under a 400 ms limit), seconds rather than the milliseconds
// such a cut takes to write its warning on a loaded machine. It is a span, not a bound: a correct
// cut writes nothing in it however long it lasts.
const heldPastTheLimit = 1600 * time.Millisecond

// cutHandlerLine is the warning consumeMessage writes for a handling its limit cut. The nak of
// that cut fact is named by the package's own nakFailure (ackprogress_test.go), the line
// nakResolve writes just before it. The row asks for both twice: that neither had been written
// while the step was still running, and that both were once it returned.
const cutHandlerLine = "an intake handler outran its limit and was cut"

// ignoredContextStep is this row's handler: a fact whose work is a wait on the row's release,
// which never reads the context it is handed. It is this file's own because the row's whole
// question is one of when: the step hands the row the context of its first delivery, so the row
// can see that context end while the step still runs, and it returns only when the row lets it.
//
// Only the first delivery waits: the second delivery is how the row sees the message come
// back, and a second wait would only hold it in the daemon for another limit. Which of the nak
// and the expired window brings it is ConsumerSpec.HandlerLimit's account and the race the
// comment at the end of the row records.
type ignoredContextStep struct {
	// handling receives the first delivery's context, once; closing release lets that delivery
	// return.
	handling chan context.Context
	release  chan struct{}

	mu         sync.Mutex
	deliveries int
}

func newIgnoredContextStep() *ignoredContextStep {
	return &ignoredContextStep{handling: make(chan context.Context, 1), release: make(chan struct{})}
}

// Apply is the step the limit cannot end. It hands the row its context and then ignores the
// context entirely: the wait runs until the row releases it whether the deadline passed during it
// or not, which is what makes this handler the one the limit's guarantee does not hold for.
func (s *ignoredContextStep) Apply(ctx context.Context, _ pgx.Tx, _ Fact) (Result, error) {
	s.mu.Lock()
	s.deliveries++
	first := s.deliveries == 1
	s.mu.Unlock()
	if !first {
		return Result{}, nil
	}
	s.handling <- ctx
	s.waitIgnoringTheCancelledContext()
	return Result{}, nil
}

// waitIgnoringTheCancelledContext is the step itself, in a function of its own so that what the
// row rests on is one line a reader cannot mistake: a receive from the row's release takes no
// context, so the cut that cancels the handling's context reaches nothing here.
func (s *ignoredContextStep) waitIgnoringTheCancelledContext() {
	<-s.release
}

// delivered is how many deliveries of the fact reached the step, which is how the row sees the
// redelivery that follows the cut.
func (s *ignoredContextStep) delivered() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deliveries
}

// This row proves what the intake cut relies on, by driving one fact of the daemon's own Dispatch
// consumer through a handler whose step ignores the context the cut cancels. The limit passes
// while that step is still waiting, and for as long as the step goes on waiting the daemon writes
// neither the cut warning nor the nak: both follow the handler rather than the deadline, because
// consumeMessage waits for handleMessage to return. Once it has returned both are there, and the
// message comes back to the step, by whichever path ConsumerSpec.HandlerLimit's account gives.
//
// The deadline is the one the handling armed on the consumers' clock, a fake the row moves past
// it, and the row reads the handling's own context to see that deadline end it, rather than
// timing anything. A row that only waited would show only that the warning turns up eventually
// after a wait longer than the limit, which is just as true of a cut that lands at the deadline,
// and so would prove nothing about which of the two ended the handling.
func TestTheCutEndsAHandlerOnceItsStepHonoursTheCancelledContext(t *testing.T) {
	pool := migratedPool(t)
	js, _ := testJetStream(t)
	logs := &lockedBuffer{}
	spec := consumerSpec(logs)
	spec.HandlerLimit = cutHandlerLimit
	fake := clocktest.New(time.Now())
	spec.Clock = fake

	step := newIgnoredContextStep()
	stop := startConsume(t, js, spec, pool, step)
	defer stop()
	// The step is let go before the consumers stop, on every way out of the row: a stop that
	// waited for a step nothing released would hold the row to its binary's deadline.
	release := sync.OnceFunc(func() { close(step.release) })
	defer release()
	// One fact, published once the consumers are open, since a durable created where the pool
	// keeps no position for it starts at the next message (openConsumer): every line the row
	// reads is this fact's, and the one redelivery it sees is the cut's.
	publish(t, js, "notifications.dispatch.issue.CAPTURE-350.issue.updated", envelopeJSON(t, "cut-350", "dispatch",
		`{"id":1,"issue_key":"CAPTURE-350","seq":1,"notify":true,"type":"issue.updated","actor":{"kind":"user","id":"someone"},`+
			`"payload":{"key":"CAPTURE-350","status":"backlog","title":"a fact whose step ignores its context","parent":null,"rank":"A","labels":[],"resolved_repo":"sjawhar/legion"}}`))

	handling := testwait.Receive(t, "the first delivery of the fact to reach the step", step.handling)
	// consumeMessage arms the limit before the handling starts, and nothing but this row moves the
	// fake, so the deadline stands at the whole limit from where the fake stands.
	limit := fake.Now().Add(spec.HandlerLimit)
	if armed := fake.Armed(); !slices.Contains(armed, limit) {
		t.Fatalf("with the step running the consumers' clock holds %v armed, want the handling's %s limit at %v: consumeMessage runs every handling under spec.Clock.WithTimeout(ctx, spec.HandlerLimit)",
			armed, spec.HandlerLimit, limit)
	}
	// First, that the deadline passed while the step was still running, which is what makes the
	// rest of the row a measurement of the cut and not of the handler's own end: the step was
	// entered before the deadline passed, and the deadline then ended the handling's context
	// while the step went on waiting.
	if err := handling.Err(); err != nil {
		t.Fatalf("the handling's context had ended before the row moved the clock to its %s limit: %v", spec.HandlerLimit, err)
	}
	fake.Advance(spec.HandlerLimit)
	if err := handling.Err(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the handling's context is ended with %v once the clock reached its %s limit, want %v while its step still runs",
			err, spec.HandlerLimit, context.DeadlineExceeded)
	}
	// Second, that while the step goes on running past the deadline the daemon does nothing about
	// the message. This is the assertion the claim rests on: the warning and the nak are not the
	// deadline's, they are the handler's return's.
	testwait.Span(t, heldPastTheLimit, "a cut that did not wait for its handler would write its warning and its nak on the "+
		"consumer's own goroutine once the deadline passed: no state the row reads records that goroutine's choice, the "+
		"consumer's real NATS and PostgreSQL keep it out of a synctest bubble, and every later action of the row reaches "+
		"that consumer only behind the step the row holds")
	held := logs.String()
	if line := logLine(held, cutHandlerLine); line != "" {
		t.Errorf("the daemon said an intake handler outran its limit and was cut while that handler's step was still running, held %s past its deadline: %s", heldPastTheLimit, line)
	}
	if line := logLine(held, nakFailure); line != "" {
		t.Errorf("the daemon naked the message while its handler's step was still running, held %s past its deadline: %s", heldPastTheLimit, line)
	}
	release()

	testwait.Eventually(t, "the fact whose step ignored its context was cut and delivered again", func() bool {
		return step.delivered() >= 2
	})
	// The consumers stop on that second delivery, so the row reads a log nothing is still writing
	// and leaves the fact alone rather than working it a third time.
	stop()

	// Third, that both happened once the step returned: the cut is real, it was only waiting.
	written := logs.String()
	cut := logLine(written, cutHandlerLine)
	if cut == "" {
		t.Fatalf("no line says an intake handler outran its limit and was cut, and one was held %s past its %s limit, in:\n%s", heldPastTheLimit, spec.HandlerLimit, written)
	}
	for _, attribute := range []string{"consumer=", "event_id=", "fact=intake.DispatchIssue", "limit=" + spec.HandlerLimit.String()} {
		if !strings.Contains(cut, attribute) {
			t.Errorf("the cut handler's line carries no %s, which is what names the cut: %s", attribute, cut)
		}
	}
	if naked := logLine(written, nakFailure); naked == "" {
		t.Errorf("no line says the cut fact was naked, and a handling the limit cut asks for the nak rather than the acknowledgement, in:\n%s", written)
	}
	// The row asserts nothing about the second delivery itself, and this is why. That it lands
	// after the first returned cannot fail: this consumer works one message at a time
	// (jetstream.PullMaxMessages(1) in consumeConsumer), so no second delivery can reach the
	// step before the first returns. Which of the two things that bring it back actually did —
	// the acknowledgement window, which the held step outlasts, or the nak that follows the cut —
	// is a race nothing here settles: the window's redelivery is marked while the step waits,
	// but with no pull request outstanding it is handed over only at the client's next pull,
	// which races the nak the same return sends.
	// Measured in this pod with NakDelay at 3 s, when the step slept 2 s rather than waiting on
	// the row's release: the second delivery reached the step 21-28 ms after the return over
	// fifteen runs on an idle machine, the window's, and exactly 3.03 s under a loaded one, the
	// nak's. So an assertion either way is an assertion about which goroutine the scheduler ran
	// first.
	//
	// Its NumDelivered would say outright that this is the same message's second delivery, and
	// it is out of reach here: Handler.Apply is handed a context, a transaction and the fact,
	// and the only metadata that context carries is the stream position withStreamPosition put
	// in it — consumer, stream sequence and stream time, no count.
}
