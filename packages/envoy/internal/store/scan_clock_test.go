package store

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testwait"
)

// openOnScanClock opens the registry on conn as Open does, with options, and returns what Open
// returned; a registry it returns is stopped when the test ends. Every Open of this package's tests
// goes through it but the role claim scan's own cases (role_claim_scan_test.go) and
// TestOpenFailsWhenTheRoleRevisionScanStalls, which move the scan's clock themselves.
//
// Open's role claim scan (scanRoleClaims) runs on a clocktest fake of the helper's own
// (withRoleScanSeams), which nothing but the helper moves, so no timer of the scan's fires on the
// real clock. A run of a bucket that nobody disturbs ends on its own, at the bucket's end, and then
// waits for a reading of the connection begun after its end (roleClaimScan.after), which readConn
// takes at its next poll (roleClaimScanPoll). So the helper moves the fake to that poll, as soon as
// readConn has armed it, until Open returns, and only when all of these hold:
//
//   - The watch of every run that has begun has been made or refused (scanPace). A poll is then
//     what the scan waits for, or a move before the run's idle timer is armed, which is harmless:
//     the run takes that reading as it goes, and drainScan arms its idle timer at the moment the
//     move reached. Without this, a run whose consumer the server is slow to make would see the
//     helper move one poll after another while it waits, toward the scan's deadline.
//   - No idle timer of a run's scan is pending (kvwatch.ScanIdleTimeout, drainScan), so no move of
//     the helper's ends a run on its idle bound. SetWhen hands the helper deadlines alone, so it
//     tells the idle timer by its deadline: the fake's present plus the idle bound Open gives the
//     scan, since drainScan arms it, and resets it at each entry, at the present, and nothing but
//     the helper moves the fake, which it does not while a run reads.
//   - The poll comes before the scan's deadline (roleClaimScanDeadline): the helper never moves
//     the fake to the deadline, so no Open of the helper's ends on it.
//
// options are Open's own (WithLogger, WithClock, WithReplicas and the rest), passed through; the
// helper sets the scan's seams itself, after them. Before them it gives the registry a timer clock
// (WithTimerClock) that nothing moves, so a claim's cleanup window (claimCleanupWindow) never ends
// under the parallel tests' load; a case that starts one of the registry's loops passes a
// WithTimerClock of its own, which it moves.
func openOnScanClock(t testing.TB, ctx context.Context, conn *natsgo.Conn, options ...OpenOption) (*Registry, error) {
	t.Helper()
	fake := clocktest.New(registryClockStart)
	pace := &scanPace{}
	type opened struct {
		registry *Registry
		err      error
	}
	done := make(chan opened, 1)
	go func() {
		withTimers := append([]OpenOption{WithTimerClock(clocktest.New(registryClockStart))}, options...)
		registry, err := Open(ctx, conn, append(withTimers, withRoleScanSeams(pace.seams(fake)))...)
		done <- opened{registry: registry, err: err}
	}()
	var result opened
	returned := false
	defer func() {
		// A registry Open returns after the wait gave up is stopped all the same.
		if !returned {
			go func() {
				if late := <-done; late.registry != nil {
					late.registry.StopWatch()
				}
			}()
		}
	}()
	testwait.Until(t, "Open to return, its role claim scan's fake clock moved past each connection poll the scan waits for between its runs", func() (string, bool) {
		select {
		case result = <-done:
			returned = true
			return "Open returned", true
		default:
		}
		armed, moved := fake.SetWhen(pace.nextPoll)
		return pace.reading(armed, moved), false
	})
	if result.registry != nil {
		t.Cleanup(result.registry.StopWatch)
	}
	return result.registry, result.err
}

// scanPace is what a driver of the role claim scan's fake clock reads of the scan through its seams
// (began, counted): how many runs have begun, how many of their watches have been made or refused,
// and the scan's start, which its deadline is roleClaimScanDeadline after. openOnScanClock keeps
// one, and so does each of the scan's own cases and TestOpenFailsWhenTheRoleRevisionScanStalls
// (scanProbe).
type scanPace struct {
	mu      sync.Mutex
	runs    int
	watched int
	start   time.Time
}

// seams are the scan's seams for one Open of openOnScanClock's: c, the fake the helper moves, and
// the run and watch seams that count the runs and their watches.
func (p *scanPace) seams(c *clocktest.Fake) roleScanSeams {
	return roleScanSeams{clock: c, watch: p.counted, run: p.began}
}

// began is the run seam's record: run n has begun, and the scan's start, which it hands every run,
// is started.
func (p *scanPace) began(n int, started time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.runs = n
	p.start = started
}

// started is the scan's start, the zero time until its first run has begun.
func (p *scanPace) started() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.start
}

// deadlineLocked is the scan's deadline, roleClaimScanDeadline after its start, or the zero time
// until its first run has begun. p.mu is held.
func (p *scanPace) deadlineLocked() time.Time {
	if p.runs == 0 {
		return time.Time{}
	}
	return p.start.Add(roleClaimScanDeadline)
}

// counted is kv with each watch a run makes on it counted once nats.go has made or refused it.
func (p *scanPace) counted(kv bus.KeyValue) bus.KeyValue {
	return bus.NewKeyValue(kv.Legacy(), watchCountedKV{KeyValue: kv.WatchHandle(), pace: p}, kv.Streams())
}

// count is how many runs have begun.
func (p *scanPace) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runs
}

// between reports whether every run that has begun has had its watch made or refused, and returns
// the scan's deadline.
func (p *scanPace) between() (bool, time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runs > 0 && p.watched >= p.runs, p.deadlineLocked()
}

// nextPoll is openOnScanClock's move, for SetWhen: to the poll readConn has armed, when every run's
// watch has been made or refused, no run's idle timer is pending and the poll comes before the
// scan's deadline (scanPollDue). It runs under the fake's lock, and reads only the pace's own.
func (p *scanPace) nextPoll(now time.Time, armed []time.Time) (time.Time, bool) {
	between, deadline := p.between()
	if !between {
		return now, false
	}
	return scanPollDue(now, armed, deadline, deadline)
}

// anyPoll is nextPoll with no bound at the deadline, for a driver that moves the fake itself
// (role_claim_scan_test.go, TestOpenFailsWhenTheRoleRevisionScanStalls), which may have moved the
// fake past the deadline before its run ended.
func (p *scanPace) anyPoll(now time.Time, armed []time.Time) (time.Time, bool) {
	between, deadline := p.between()
	if !between {
		return now, false
	}
	return scanPollDue(now, armed, deadline, time.Time{})
}

// reading is what a wait that moves the fake prints when it gives up: the runs and their watches,
// and what the fake held at its last look.
func (p *scanPace) reading(armed []time.Time, moved bool) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return fmt.Sprintf("%d runs begun, %d watches made or refused, deadline %v; armed %v, moved %t", p.runs, p.watched, p.deadlineLocked(), armed, moved)
}

// watchCountedKV counts each watch the scan's runs make on the role claim bucket, once nats.go has
// made or refused it (scanPace).
type watchCountedKV struct {
	jetstream.KeyValue

	pace *scanPace
}

func (k watchCountedKV) Watch(ctx context.Context, keys string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	watcher, err := k.KeyValue.Watch(ctx, keys, opts...)
	k.pace.mu.Lock()
	k.pace.watched++
	k.pace.mu.Unlock()
	return watcher, err
}

// scanPollDue is a move of the role claim scan's fake clock from now, given armed, its pending
// deadlines earliest first, as SetWhen hands them: to the poll of the connection readConn has
// armed, when one is armed, no idle timer of a run's scan is and the poll comes before limit (the
// zero limit sets none). The idle timer is the one due kvwatch.ScanIdleTimeout after the present,
// the bound Open gives the scan: drainScan arms it, and resets it at each entry, at the present, and
// a driver of the fake moves it while a run reads only to end the run. readConn arms each poll
// roleClaimScanPoll after the present it reads, so the poll is the earliest deadline after the
// present and no further from it than that, other than deadline, the scan's, where its runs, the
// wait after a run and the wait for the connection arm theirs. It reads the deadlines alone, so it
// calls nothing of the fake's, as SetWhen asks.
func scanPollDue(now time.Time, armed []time.Time, deadline, limit time.Time) (time.Time, bool) {
	if idle := now.Add(kvwatch.ScanIdleTimeout); slices.ContainsFunc(armed, idle.Equal) {
		return now, false
	}
	for _, due := range armed {
		if !due.After(now) || due.Equal(deadline) {
			continue
		}
		if due.After(now.Add(roleClaimScanPoll)) || !limit.IsZero() && !due.Before(limit) {
			return now, false
		}
		return due, true
	}
	return now, false
}
