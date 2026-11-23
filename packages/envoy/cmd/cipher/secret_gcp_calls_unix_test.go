//go:build unix

// packages/envoy/cmd/cipher/secret_gcp_calls_unix_test.go
//
// The part of a gcloud call that is another process's work, for the rows that time code which
// makes gcloud calls: from the moments runRecorded reports (onGcloudStep), the kill its call's
// deadline sends (signalGroup), and the end of the process the call started, seen as it happens
// (watchProcessExit).
package main

import (
	"errors"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"
)

// gcloudCall is one gcloud call as recordGcloudCalls saw it: when runRecorded had started it
// (gcloudStarted); when its deadline's kill sent its process group SIGKILL (killGroupOnCancel; zero
// where the call ended by itself); when the process it started (gcloud's supervisor, or gcloud
// where there is none) was seen to end (watchProcessExit; where no watch can see it, the wait
// stands in); and when runRecorded had waited for it (gcloudWaited), the process reaped and its
// output closed.
type gcloudCall struct {
	started, killed, exited, waited time.Time
}

// inAnotherProcess is how long call spent on another process's work, which the stall meter, counting
// only the time this process was held up, does not see: for a call its deadline killed, from the
// kill to the end of the process it started, on Linux gcloud's supervisor (a run of this binary)
// torn down; for a call that ended by itself, from its start to that end, the supervisor's start,
// gcloud's run and the supervisor's end among it. It ends at that process's own end, not at the
// call's wait: what this process does from there (the rest of the kill, the reaping, the close of
// the output, the return) is its own code, which the row holds to its slack. Where the stub gcloud
// leaves the supervisor's output alone (no row's stub of these holds it, but callEnds', which
// watches its own), the supervisor's end is the last end the call's return waits on. The time
// before the kill of a killed call is the deadline's, which the row times itself.
func (c gcloudCall) inAnotherProcess() time.Duration {
	from := c.started
	if !c.killed.IsZero() {
		from = c.killed
	}
	return max(c.exited.Sub(from), 0)
}

// recordGcloudCalls records, for the rest of t, each gcloud call runRecorded makes: its start and
// its wait (onGcloudStep), the end of the process it started (watchProcessExit, from its start),
// and the first SIGKILL sent to a process group between start and wait (signalGroup, which sends
// it on), the kill of the call's deadline. Calls run one at a time, as the forms make them. It
// answers how to read the calls back, in the order they started, once each has been waited for;
// t fails there where a call started before the one before it had been waited for, had not been
// waited for, or its process was not seen to end when the waits of these tests give up.
func recordGcloudCalls(t *testing.T) func() []gcloudCall {
	t.Helper()
	var mu sync.Mutex
	var calls []gcloudCall
	var exits []func() (time.Time, error)
	var stops []func()
	open, overlapped := false, false
	restoreStep, restoreSignal := onGcloudStep, signalGroup
	t.Cleanup(func() {
		onGcloudStep, signalGroup = restoreStep, restoreSignal
		mu.Lock()
		defer mu.Unlock()
		for _, stop := range stops {
			stop()
		}
	})
	onGcloudStep = func(step string, pid int) {
		now := time.Now()
		mu.Lock()
		defer mu.Unlock()
		switch step {
		case gcloudStarted:
			overlapped = overlapped || open
			exited, stop := watchProcessExit(pid)
			calls, exits, stops, open = append(calls, gcloudCall{started: now}), append(exits, exited), append(stops, stop), true
		case gcloudWaited:
			if open {
				calls[len(calls)-1].waited, open = now, false
			}
		}
	}
	signalGroup = func(pgid int, sig syscall.Signal) error {
		now := time.Now()
		mu.Lock()
		if open && sig == syscall.SIGKILL && calls[len(calls)-1].killed.IsZero() {
			calls[len(calls)-1].killed = now
		}
		mu.Unlock()
		return restoreSignal(pgid, sig)
	}
	return func() []gcloudCall {
		t.Helper()
		mu.Lock()
		got, ended, crossed := slices.Clone(calls), slices.Clone(exits), overlapped
		mu.Unlock()
		if crossed {
			t.Fatal("a gcloud call started before the one before it had been waited for")
		}
		for i := range got {
			if got[i].waited.IsZero() {
				t.Fatalf("gcloud call %d of %d had not been waited for", i+1, len(got))
			}
			exited, err := seenToEnd(t, ended[i])
			switch {
			case errors.Is(err, errNoExitWatch):
				exited = got[i].waited
			case err != nil:
				t.Fatalf("gcloud call %d of %d: %v", i+1, len(got), err)
			}
			got[i].exited = exited
		}
		return got
	}
}

// seenToEnd waits, bounded by the waits of these tests, for exited (watchProcessExit) to report
// that its process ended, and answers when, or why it cannot say (errNoExitWatch where this
// platform has no watch). t fails where the process was not seen to end when the waits give up.
func seenToEnd(t *testing.T, exited func() (time.Time, error)) (time.Time, error) {
	t.Helper()
	var at time.Time
	var err error
	if !heldBeforeGiveUp(t, func() bool {
		at, err = exited()
		return err != nil || !at.IsZero()
	}) {
		t.Fatalf("a process had not been seen to end %s before the test binary's deadline", waitMargin)
	}
	return at, err
}

// inAnotherProcessAll is the sum of each call's inAnotherProcess, to the millisecond.
func inAnotherProcessAll(calls []gcloudCall) time.Duration {
	var in time.Duration
	for _, c := range calls {
		in += c.inAnotherProcess()
	}
	return in.Round(time.Millisecond)
}
