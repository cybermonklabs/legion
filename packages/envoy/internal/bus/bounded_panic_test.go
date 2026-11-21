package bus

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// A panic in a call Bounded runs on a goroutine of its own reaches the caller still waiting with
// the value the call panicked with, so a caller that recovers it reads that value, and the log
// carries the stack of the call's own goroutine at the panic, which the caller's own trace does not
// show: the caller logs it at ERROR before it panics again.
//
// The control is the caller panicking again without the log: the value still arrives, the log
// holds no frame of the call, and the row fails at its read of the log.
func TestABoundedCallsPanicReachesItsCallerWithTheCallsStack(t *testing.T) {
	logs := &lockedLog{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, nil)))
	defer slog.SetDefault(previous)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_, _, _ = Bounded(ctx, panicsInABoundedCall)
	}()

	if recovered != "a bounded call panicked" {
		t.Fatalf("the caller recovered %v, want the value the call panicked with", recovered)
	}
	if logged := logs.String(); !strings.Contains(logged, "panicsInABoundedCall") || !strings.Contains(logged, `"level":"ERROR"`) {
		t.Fatalf("the log does not carry the stack of the call's own goroutine at ERROR:\n%s", logged)
	}
}

// panicsInABoundedCall is the call the row bounds, named so its frame can be read in the log.
func panicsInABoundedCall() (int, error) {
	panic("a bounded call panicked")
}

// lockedLog is a log sink the call's goroutine and the test's both write and read.
type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}
