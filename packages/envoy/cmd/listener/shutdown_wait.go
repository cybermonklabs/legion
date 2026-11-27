package main

import (
	"context"
	"sync"
)

// The shutdown's waits say "still running at shutdown" only of work that is still running when
// their context ends. A select on a done channel and the context picks either when both are ready,
// and the shutdown's context is often already over when a later step's wait starts, so a wait that
// takes the context's side reported work that had ended (half the self-health monitor waits and
// nearly every consumer-delete wait of a shutdown whose HTTP window was spent). These two read the
// work's own state once the context has ended, before they answer.

// closedWithin waits for done within ctx and reports whether done was closed by the time it
// answers: once ctx has ended it reads done again, so a done closed when ctx ended reads closed.
func closedWithin(ctx context.Context, done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	case <-ctx.Done():
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
}

// inflight counts work in flight, as a sync.WaitGroup does, and can also say how much is left, so
// a wait that its context ends reads whether any of it still runs rather than whether a goroutine
// waiting on a WaitGroup has been scheduled yet. The zero value is ready to use.
type inflight struct {
	mu sync.Mutex
	n  int
	// idle is closed when n falls to 0, and made again when n rises from 0.
	idle chan struct{}
}

// Add counts one more piece of work.
func (f *inflight) Add() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.n == 0 {
		f.idle = make(chan struct{})
	}
	f.n++
}

// Done counts one piece of work finished. A Done with no work counted is a caller's fault, and it
// panics, as sync.WaitGroup's does: counted on, it would read work still running as finished.
func (f *inflight) Done() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.n == 0 {
		panic("inflight: Done with no work counted")
	}
	f.n--
	if f.n == 0 {
		close(f.idle)
	}
}

// Go runs work on a goroutine of its own, counted until it returns.
func (f *inflight) Go(work func()) {
	f.Add()
	go func() {
		defer f.Done()
		work()
	}()
}

// Wait blocks until no work is left.
func (f *inflight) Wait() {
	f.wait(context.Background())
}

// wait waits within ctx until no work is left, and reports whether none was left when it answers.
func (f *inflight) wait(ctx context.Context) bool {
	f.mu.Lock()
	if f.n == 0 {
		f.mu.Unlock()
		return true
	}
	idle := f.idle
	f.mu.Unlock()
	if closedWithin(ctx, idle) {
		return true
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n == 0
}
