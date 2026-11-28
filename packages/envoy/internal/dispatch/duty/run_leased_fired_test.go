package duty_test

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// startLoggedLeasedLoop starts a process with its own log and waits for it to stop at cleanup.
func startLoggedLeasedLoop(t *testing.T, fake *clocktest.Fake, leases duty.Leases, name string, interval time.Duration, pass func(context.Context)) *logtest.Buffer {
	t.Helper()
	logs := &logtest.Buffer{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		duty.RunLeasedOn(ctx, fake, logger, leases, name, interval, duty.TickAfterInterval, pass)
	}()
	t.Cleanup(func() {
		stop()
		testwait.Receive(t, "the logged leased loop to return after cancellation", done)
	})
	return logs
}

// leasedTick is what one gate logs: the due it received, its period, and whether it ran the pass.
type leasedTick struct {
	due, period time.Time
	ran         bool
}

var leasedTickLine = regexp.MustCompile(`msg="dispatch duty: leased pass (ran|skipped)" lease=\S+ due=(\S+) period=(\S+)`)

// nextLeasedTick reads the next line of this lease and requires that the gate reached its period read.
func nextLeasedTick(t *testing.T, logs *logtest.Buffer, name, what string) leasedTick {
	t.Helper()
	line := testwait.Next(t, "the Exclusive log line for "+what, logs.Stream(), func(line string) bool {
		return strings.Contains(line, " lease="+name+" ") || strings.HasSuffix(line, " lease="+name)
	})
	fields := leasedTickLine.FindStringSubmatch(line)
	if fields == nil {
		t.Fatalf("%s logged %q, want a tick that read its period", what, line)
	}
	due, err := time.Parse(time.RFC3339Nano, fields[2])
	if err != nil {
		t.Fatalf("the due of %q: %v", line, err)
	}
	period, err := time.Parse(time.RFC3339Nano, fields[3])
	if err != nil {
		t.Fatalf("the period of %q: %v", line, err)
	}
	return leasedTick{due: due, period: period, ran: fields[1] == "ran"}
}

// lateLeases holds the second process before it takes the lease, after it has taken its first tick.
type lateLeases struct {
	duty.Leases
	entered chan<- struct{}
	release <-chan struct{}
}

func (l lateLeases) Acquire(ctx context.Context) (*pgxpool.Conn, error) {
	select {
	case <-l.release:
	default:
		close(l.entered)
		select {
		case <-l.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return l.Leases.Acquire(ctx)
}

// An overrun tick serves the period when it fell due, not the period when the loop finally took
// it. The first overdue tick is kept, later overdue ticks are dropped, and the loop resumes on
// the grid. A second process that comes late to the gate still agrees with the first process on
// which periods its ticks belong to; otherwise it can take a period before the first's next tick.
func TestRunLeasedHandsExclusiveTheMomentEachTickFired(t *testing.T) {
	t.Parallel()
	const name, interval = "fired-moment-loop", time.Minute
	start := tickDue.Add(50 * time.Second)
	at := func(tick int) time.Time { return start.Add(time.Duration(tick) * interval) }
	fake := clocktest.New(start)
	database := storetest.Open(t)
	fire := func(what string, moment time.Time, loops int) {
		t.Helper()
		armed := fake.AwaitArmed(t, what, loops)
		for _, deadline := range armed {
			if !deadline.Equal(moment) {
				t.Fatalf("%s: armed at %s, want %s", what, deadline, moment)
			}
		}
		fake.Set(moment)
	}
	requireTick := func(logs *logtest.Buffer, what string, fired time.Time, ran bool) {
		t.Helper()
		got := nextLeasedTick(t, logs, name, what)
		period := duty.PeriodBoundary(fired, interval)
		if !got.due.Equal(fired) || !got.period.Equal(period) || got.ran != ran {
			t.Fatalf("%s: handed Exclusive %s, took the period at %s and ran %t; want %s, the moment it fired, the period at %s, and ran %t",
				what, got.due.UTC().Format(time.TimeOnly), got.period.UTC().Format(time.TimeOnly), got.ran,
				fired.Format(time.TimeOnly), period.Format(time.TimeOnly), ran)
		}
	}

	// The first pass runs from 12:01:50 to 12:04:05. The 12:02:50 tick is retained, but the
	// 12:03:50 tick is dropped. The 12:04:50 tick then serves the period the retained tick did not.
	firstTickSet := make(chan struct{})
	overran := false
	first := startLoggedLeasedLoop(t, fake, leasesOf(t, database), name, interval, func(ctx context.Context) {
		if !overran {
			overran = true
			// Do not move the fake again until the test's Set has returned.
			select {
			case <-firstTickSet:
				fake.Advance(2*interval + 15*time.Second)
			case <-ctx.Done():
			}
		}
	})
	fire("the first process's first tick", at(1), 1)
	close(firstTickSet)
	requireTick(first, "the first process's first tick", at(1), true)
	requireTick(first, "the retained overrun tick, taken at 12:04:05", at(2), true)
	fire("the first process's tick after the overrun", at(4), 1)
	requireTick(first, "the first process's tick after the overrun", at(4), true)

	// Both processes take the 12:05:50 tick, but the second cannot take its lease until 12:07:05.
	// By then the first process has served both 12:05 and 12:06, and both of the second's ticks
	// must be refused, including its 12:06:50 tick taken a quarter of a minute late.
	entered, release := make(chan struct{}), make(chan struct{})
	secondLeases := leasesOf(t, storetest.SecondProcess(t, database))
	second := startLoggedLeasedLoop(t, fake, lateLeases{Leases: secondLeases, entered: entered, release: release}, name, interval, func(context.Context) {})
	fire("both processes' ticks on the shared grid", at(5), 2)
	testwait.Receive(t, "the second process to take its tick and wait before acquiring the lease", entered)
	requireTick(first, "the first process's tick with the second process held", at(5), true)
	fire("the first process's next tick, with the second process still held", at(6), 1)
	requireTick(first, "the first process's next tick", at(6), true)
	fake.AwaitArmedAt(t, "the first process's next tick, after it gives back the lease", at(7))
	fake.Set(at(6).Add(15 * time.Second))
	close(release)
	requireTick(second, "the second process's held tick", at(5), false)
	requireTick(second, "the second process's overrun tick, taken at 12:07:05", at(6), false)
	fake.AwaitArmedCountAt(t, "both processes' next ticks after both leases are returned", at(7), 2)
}
