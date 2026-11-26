package testwait

import (
	"context"
	"sync"
)

// PassedDeadline is a context whose deadline a test passes by hand, once what it waits for has
// happened: Done closes, and Err answers context.DeadlineExceeded, from Pass on. A context derived
// from it ends then with that error, as one whose deadline passed does. internal/ghroute's help
// read test and cmd/legion's probe-image test both stop a child process this way.
type PassedDeadline struct {
	context.Context
	done chan struct{}
	once sync.Once
}

// NewPassedDeadline is a PassedDeadline whose deadline has not passed yet.
func NewPassedDeadline() *PassedDeadline {
	return &PassedDeadline{Context: context.Background(), done: make(chan struct{})}
}

// Pass passes the deadline; a second call does nothing.
func (d *PassedDeadline) Pass() { d.once.Do(func() { close(d.done) }) }

// Done closes once Pass is called.
func (d *PassedDeadline) Done() <-chan struct{} { return d.done }

// Err is context.DeadlineExceeded once Pass is called, and nil before.
func (d *PassedDeadline) Err() error {
	select {
	case <-d.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}
