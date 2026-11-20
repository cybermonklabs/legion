package api

import (
	"context"
	"sync"
)

// Detached counts the work a route starts that outlives its answer: the reread run after a 202
// (rereadAfterAnswer). An http.Server's Shutdown waits for its handlers and not for a goroutine
// one of them started, so a pod that stops right after such an answer would end that reread, and
// the notice it owes the other pods, with nothing logged. The stop sequence waits for it instead
// (Wait), after Shutdown has returned and before the work that outlives the drain ends. A nil
// Detached counts nothing, for a mux nothing stops.
type Detached struct {
	mu      sync.Mutex
	closing bool
	running sync.WaitGroup
}

// start counts one piece of detached work. It returns false after Wait begins, when a new
// goroutine cannot be counted safely because Wait may already be blocked.
func (d *Detached) start() bool {
	if d == nil {
		return true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closing {
		return false
	}
	d.running.Add(1)
	return true
}

// done ends one piece of detached work start counted.
func (d *Detached) done() {
	if d != nil {
		d.running.Done()
	}
}

// Wait waits until no detached work is left, and answers nil; or until ctx ends first, and answers
// ctx's error, the work still running.
func (d *Detached) Wait(ctx context.Context) error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	d.closing = true
	d.mu.Unlock()
	finished := make(chan struct{})
	go func() {
		d.running.Wait()
		close(finished)
	}()
	select {
	case <-finished:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
