package events

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testwait"
)

// beforeTheDeadline is how long the horizon tests' waits on their own channels may take: what is
// left of the test binary's deadline, less a second (testwait.Bounded of a day). So how long a
// loaded host takes to run an observation decides no verdict, and a wait whose event never comes
// still fails, naming the step, instead of ending in the binary's goroutine dump.
func beforeTheDeadline(t *testing.T) time.Duration {
	return testwait.Bounded(t, 24*time.Hour, time.Second)
}

// receiveBeforeTheDeadline is the next value of ch, or of its close, within beforeTheDeadline.
func receiveBeforeTheDeadline[T any](t *testing.T, what string, ch <-chan T) T {
	t.Helper()
	var got T
	testwait.WithinPolling(t, beforeTheDeadline(t), testwait.Poll, what, func() bool {
		select {
		case got = <-ch:
			return true
		default:
			return false
		}
	})
	return got
}
