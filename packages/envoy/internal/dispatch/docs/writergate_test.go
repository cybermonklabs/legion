package docs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testcleanup"
)

// patientWriter is ctx carrying an admission ticket of a limiter of its own whose wait is an hour,
// for a writer a test queues on a document's gate. A writer waits for a gate for what is left of
// its ticket's wait (AcquireDocumentWriter), and one admission did not count for
// admission.DefaultWriteWait: a product bound these rows do not mean to race, which a loaded
// machine can run out while the test is still queueing the writers behind it. The ticket is
// released when the test ends.
func patientWriter(t *testing.T, ctx context.Context) context.Context {
	t.Helper()
	limiter := admission.New(admission.Limits{Read: 1, Write: 1, ReadWait: time.Hour, WriteWait: time.Hour})
	ticket, err := limiter.Admit(context.Background(), admission.Write, 1)
	if err != nil {
		t.Fatalf("admit a patient writer: %v", err)
	}
	testcleanup.Add(t, ticket.Release)
	return admission.WithTicket(ctx, ticket)
}

// A caller holding a pooled connection is refused a document's writer gate at once: the writer
// holding the gate may be waiting for a connection, or for a lock that caller's transaction holds.
func TestTheWriterGateRefusesACallerHoldingAConnection(t *testing.T) {
	t.Parallel()
	service, artifactID := newTestService(t)
	ctx := store.WithTransactionTracking(context.Background())
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, _, err := service.AcquireDocumentWriter(ctx, artifactID); !errors.Is(err, ErrGateWhileHoldingConnection) {
		t.Fatalf("writer gate taken holding a transaction = %v, want ErrGateWhileHoldingConnection", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	// The refusal took nothing: the gate is free for the next writer.
	_, release, err := service.AcquireDocumentWriter(ctx, artifactID)
	if err != nil {
		t.Fatalf("writer gate taken once the connection went back = %v", err)
	}
	release()
}

// A live write joins a document only under that document's writer gate: not without one, not under
// another document's, and not under one its holder has released.
func TestJoinLiveWriteRefusesAnUngatedCaller(t *testing.T) {
	t.Parallel()
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "First.\n")
	other := createIssueDocument(t, service.store, 2, "# Other")
	actor := model.Actor{Kind: "user", ID: "alice"}
	ops := []model.EditOp{{Op: "insert", After: "end", Markdown: "Added."}}
	write := func(ctx context.Context) error {
		tx, err := service.store.Pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		joined, ledger := service.Join(ctx, tx)
		defer ledger.Discard()
		_, err = service.ApplyOps(joined, artifactID, ops, actor, nil)
		return err
	}

	if err := write(context.Background()); !errors.Is(err, ErrWriterGateNotHeld) {
		t.Errorf("a write holding no gate = %v, want ErrWriterGateNotHeld", err)
	}
	otherGated, releaseOther, err := service.AcquireDocumentWriter(context.Background(), other)
	if err != nil {
		t.Fatalf("take the other document's gate: %v", err)
	}
	if err := write(otherGated); !errors.Is(err, ErrWriterGateNotHeld) {
		t.Errorf("a write holding another document's gate = %v, want ErrWriterGateNotHeld", err)
	}
	releaseOther()
	gated, release, err := service.AcquireDocumentWriter(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("take the document's gate: %v", err)
	}
	if err := write(gated); err != nil {
		t.Errorf("a write holding the document's gate = %v, want it to apply", err)
	}
	release()
	if err := write(gated); !errors.Is(err, ErrWriterGateNotHeld) {
		t.Errorf("a write under a gate its holder released = %v, want ErrWriterGateNotHeld", err)
	}
}

// Writers take a document's gate in the order they asked for it.
func TestTheWriterGateAdmitsWritersInArrivalOrder(t *testing.T) {
	t.Parallel()
	service, artifactID := newTestService(t)
	_, release, err := service.AcquireDocumentWriter(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("take the document's gate: %v", err)
	}
	const writers = 5
	order := make(chan int, writers)
	done := make(chan struct{}, writers)
	for index := range writers {
		patient := patientWriter(t, context.Background())
		go func() {
			_, release, err := service.AcquireDocumentWriter(patient, artifactID)
			if err != nil {
				t.Errorf("writer %d: %v", index, err)
				done <- struct{}{}
				return
			}
			order <- index
			release()
			done <- struct{}{}
		}()
		waitForQueuedWriters(t, service, int64(index+1))
	}
	release()
	for range writers {
		<-done
	}
	close(order)
	next := 0
	for index := range order {
		if index != next {
			t.Fatalf("writer %d took the gate where writer %d was due", index, next)
		}
		next++
	}
}

// Writes to overlapping documents, named in opposite orders, never each hold a gate the other
// waits for: the gates are taken sorted.
func TestOverlappingMultiDocumentWritersDoNotDeadlock(t *testing.T) {
	t.Parallel()
	service, first := newTestService(t)
	second := createIssueDocument(t, service.store, 2, "# Second")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	errs := make(chan error, 2)
	for _, ids := range [][]string{{first, second, first}, {second, first}} {
		go func() {
			for range 200 {
				_, release, err := service.AcquireDocumentWriter(ctx, ids...)
				if err != nil {
					errs <- err
					return
				}
				release()
			}
			errs <- nil
		}()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("a writer of both documents = %v, want every take to succeed", err)
		}
	}
}

// A writer that has to wait for a document's gate gives its request's admission units back while
// it waits, so a write to another document finds room; once the gate passes to it, it takes its
// units back. The limiter's wait is an hour, so the queued writer waits for the gate for as long
// as the test holds it, and what the row reads is where the units are, not how long a wait took.
func TestAWriterQueuedForADocumentGivesItsAdmissionBack(t *testing.T) {
	t.Parallel()
	service, artifactID := newTestService(t)
	limiter := admission.New(admission.Limits{
		Read: 1, Write: 1, ReadWait: time.Hour, WriteWait: time.Hour,
	})
	_, release, err := service.AcquireDocumentWriter(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("take the document's gate: %v", err)
	}
	ticket, err := limiter.Admit(context.Background(), admission.Write, 1)
	if err != nil {
		t.Fatalf("admit the queued writer: %v", err)
	}
	defer ticket.Release()
	queued := make(chan error, 1)
	go func() {
		_, release, err := service.AcquireDocumentWriter(admission.WithTicket(context.Background(), ticket), artifactID)
		if err == nil {
			release()
		}
		queued <- err
	}()
	waitForQueuedWriters(t, service, 1)
	// The writer counts itself queued before it parks its units (documentWriterGate.acquire).
	waitFor(t, roomWait(t), "the queued writer to give its write unit back", func() bool {
		return limiter.Totals().InFlight == 0
	})
	other, err := limiter.Admit(context.Background(), admission.Write, 1)
	if err != nil {
		t.Fatalf("a write admitted while the only write unit's holder queues for a document = %v, want it admitted", err)
	}
	other.Release()
	release()
	if err := <-queued; err != nil {
		t.Fatalf("the queued writer, once the gate passed to it = %v", err)
	}
	if held := limiter.Totals().InFlight; held != 1 {
		t.Fatalf("%d write units held once the queued writer took the gate, want the one it took back", held)
	}
}

// A writer whose admission units cannot be taken back once the gate passes to it is refused as
// admission refuses a request, and hands the gate on. The limiter's wait is an hour, so the queued
// writer waits for the gate for as long as the test holds it; its take-back is refused because the
// write class's queue is full behind the write that keeps its unit (fillWriteQueue), which
// admission refuses at once, and not because a wait ran out before or after the gate passed.
func TestAWriterThatCannotResumeItsAdmissionReleasesTheGate(t *testing.T) {
	t.Parallel()
	service, artifactID := newTestService(t)
	limiter := admission.New(admission.Limits{
		Read: 1, Write: 1, ReadWait: time.Hour, WriteWait: time.Hour,
	})
	_, release, err := service.AcquireDocumentWriter(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("take the document's gate: %v", err)
	}
	ticket, err := limiter.Admit(context.Background(), admission.Write, 1)
	if err != nil {
		t.Fatalf("admit the queued writer: %v", err)
	}
	defer ticket.Release()
	queued := make(chan error, 1)
	go func() {
		_, release, err := service.AcquireDocumentWriter(admission.WithTicket(context.Background(), ticket), artifactID)
		if err == nil {
			release()
		}
		queued <- err
	}()
	waitForQueuedWriters(t, service, 1)
	// The writer counts itself queued before it parks its units (documentWriterGate.acquire).
	waitFor(t, roomWait(t), "the queued writer to give its write unit back", func() bool {
		return limiter.Totals().InFlight == 0
	})
	other, err := limiter.Admit(context.Background(), admission.Write, 1)
	if err != nil {
		t.Fatalf("admit the write that keeps the unit: %v", err)
	}
	defer other.Release()
	fillWriteQueue(t, limiter)
	release()
	var refusal *admission.Refusal
	if err := <-queued; !errors.As(err, &refusal) || refusal.Reason != admission.QueueFull {
		t.Fatalf("a writer that cannot take its unit back = %v, want admission's refusal of a full queue", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, next, err := service.AcquireDocumentWriter(ctx, artifactID)
	if err != nil {
		t.Fatalf("the gate after the refused writer = %v, want it free", err)
	}
	next()
}

// fillWriteQueue queues writes on limiter's write class, each waiting on a goroutine of its own
// until the test ends, until admission refuses the next at once because the queue is full
// (admission.QueueFull): a take-back of a write unit after it is refused at once as well.
func fillWriteQueue(t *testing.T, limiter *admission.Limiter) {
	t.Helper()
	queuing, stop := context.WithCancel(context.Background())
	testcleanup.Add(t, stop)
	writesQueued := func() int {
		for _, class := range limiter.Stats() {
			if class.Class == admission.Write {
				return class.Queued
			}
		}
		return 0
	}
	for {
		before := writesQueued()
		answered := make(chan error, 1)
		go func() {
			ticket, err := limiter.Admit(queuing, admission.Write, 1)
			if err == nil {
				ticket.Release()
			}
			answered <- err
		}()
		var answer error
		refused := false
		waitFor(t, roomWait(t), "a write to queue, or to be refused by the full write queue", func() bool {
			select {
			case answer = <-answered:
				refused = true
				return true
			default:
				return writesQueued() > before
			}
		})
		if !refused {
			continue
		}
		var refusal *admission.Refusal
		if !errors.As(answer, &refusal) || refusal.Reason != admission.QueueFull {
			t.Fatalf("a write on the filling write queue answered %v, want it queued or refused by the full queue", answer)
		}
		return
	}
}
