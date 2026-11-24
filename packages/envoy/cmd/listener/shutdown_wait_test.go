package main

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/logtest"
)

// endedContext is a context that has already ended, as the shutdown's is when its HTTP window was
// spent before a later step's wait begins.
func endedContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// A shutdown wait whose context has ended answers from the work's own state: a done channel
// already closed reads closed every time, where a select on the two took either half the time.
func TestAShutdownWaitReadsWorkThatEndedAsEndedWhenItsContextHasEndedToo(t *testing.T) {
	closed := make(chan struct{})
	close(closed)
	for attempt := range 1000 {
		if !closedWithin(endedContext(), closed) {
			t.Fatalf("attempt %d: a closed done read as still running once the context had ended", attempt)
		}
	}
	if closedWithin(endedContext(), make(chan struct{})) {
		t.Fatal("a done never closed read as closed")
	}

	var work inflight
	for attempt := range 1000 {
		if !work.wait(endedContext()) {
			t.Fatalf("attempt %d: no work in flight read as still running once the context had ended", attempt)
		}
	}
	work.Add()
	if work.wait(endedContext()) {
		t.Fatal("work still in flight read as finished")
	}
	work.Done()
	if !work.wait(endedContext()) {
		t.Fatal("work finished read as still running")
	}
	release := make(chan struct{})
	work.Go(func() { <-release })
	if work.wait(endedContext()) {
		t.Fatal("a goroutine still running read as finished")
	}
	close(release)
	work.Wait()
	if !work.wait(endedContext()) {
		t.Fatal("a goroutine that returned read as still running")
	}

	// A Done with nothing counted panics, as sync.WaitGroup's does, rather than read work that
	// is still running as finished.
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("a Done with no work counted did not panic")
			}
		}()
		work.Done()
	}()
}

// A shutdown whose HTTP window was spent says nothing of consumer deletes when none ran: before,
// the wait read the ended context before the goroutine waiting on the deletes had closed its
// channel, and said a delete was still running in nearly every such shutdown.
func TestTheConsumerDeleteWaitSaysNothingWhenNoDeleteRan(t *testing.T) {
	for attempt := range 200 {
		logs := &logtest.Buffer{}
		delivery := &sessionDelivery{logger: logging.NewWithWriter("delete-wait", logs)}
		delivery.awaitDeletes(endedContext())
		if logs.Len() != 0 {
			t.Fatalf("attempt %d: the wait logged, with no delete running:\n%s", attempt, logs.String())
		}
	}

	logs := &logtest.Buffer{}
	delivery := &sessionDelivery{logger: logging.NewWithWriter("delete-wait", logs)}
	delivery.deletes.Add()
	delivery.awaitDeletes(endedContext())
	delivery.deletes.Done()
	if !strings.Contains(logs.String(), "a session consumer delete was still running at shutdown") {
		t.Fatalf("a delete still running at the wait's end was not said:\n%s", logs.String())
	}
}
