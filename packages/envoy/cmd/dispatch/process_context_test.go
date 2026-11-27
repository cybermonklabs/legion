package main

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
)

// TestTheProcessContextEndsWithTheServerStoppingCause: main's process context ends with
// docs.ErrServerStopping as its cause however the process stops - by the stop function that
// serveUntilStopped, exitAfterIntakes and stopIntakesOnExit call, and by a SIGTERM - so the
// document sockets' access re-check reads the stop as the server's and revokes nothing. The
// control is a plain cancel, whose cause is context.Canceled: the same check fails it, which is
// the case the re-check still answers by closing the sockets it could not read again.
func TestTheProcessContextEndsWithTheServerStoppingCause(t *testing.T) {
	// Not parallel: the SIGTERM row sends the signal to this test binary, which only a
	// registration made while no other test runs receives.
	for _, row := range []struct {
		name     string
		stop     func(t *testing.T) context.Context
		stopping bool
	}{
		{
			name: "the process stop that serveUntilStopped, exitAfterIntakes and stopIntakesOnExit call",
			stop: func(t *testing.T) context.Context {
				ctx, cancel := processContext()
				cancel()
				return ctx
			},
			stopping: true,
		},
		{
			name: "a SIGTERM",
			stop: func(t *testing.T) context.Context {
				ctx, cancel := processContext()
				t.Cleanup(cancel)
				if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
					t.Fatalf("send SIGTERM to this process: %v", err)
				}
				return ctx
			},
			stopping: true,
		},
		{
			name: "control: a plain cancel",
			stop: func(t *testing.T) context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			stopping: false,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			ctx := row.stop(t)
			select {
			case <-ctx.Done():
			case <-time.After(10 * time.Second):
				t.Fatal("the process context did not end within 10 s of the stop")
			}
			cause := context.Cause(ctx)
			if got := errors.Is(cause, docs.ErrServerStopping); got != row.stopping {
				t.Fatalf("the context ended with cause %v; cause is docs.ErrServerStopping = %t, want %t", cause, got, row.stopping)
			}
		})
	}
}
