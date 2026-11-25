package duty_test

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// firedLease is the lease the fired-moment row's loops share, and firedInterval their interval
// and so their period. firedStart puts their grid ten seconds before each minute ends, so a tick
// taken a quarter of a minute late is taken in the period after the one it fired in.
const (
	firedLease    = "fired-moment-loop"
	firedInterval = time.Minute
)

var firedStart = time.Date(2026, 9, 22, 12, 0, 50, 0, time.UTC)

// firedLine reads the due and period of a line Exclusive logs for a tick of firedLease that
// reached its period read: the ran line and the served skip.
var firedLine = regexp.MustCompile(`msg="dispatch duty: leased pass (ran|skipped)" lease=` + firedLease + ` due=(\S+) period=(\S+)`)

// firedTick is what one Exclusive call of a fired-moment loop logged: the moment the loop handed
// it (due), the period that fell in, and whether its pass ran.
type firedTick struct {
	due, period time.Time
	ran         bool
}

// nextFiredTick reads the next line a fired-moment loop's Exclusive call logs.
func nextFiredTick(t *testing.T, logs *logtest.Buffer, what string) firedTick {
	t.Helper()
	line := testwait.Next(t, "the line of "+what, logs.Stream(), func(line string) bool {
		return strings.Contains(line, "lease="+firedLease)
	})
	fields := firedLine.FindStringSubmatch(line)
	if fields == nil {
		t.Fatalf("%s logged %q, want the line of a tick that read its period", what, line)
	}
	due, err := time.Parse(time.RFC3339Nano, fields[2])
	if err != nil {
		t.Fatalf("the due of %q: %v", line, err)
	}
	period, err := time.Parse(time.RFC3339Nano, fields[3])
	if err != nil {
		t.Fatalf("the period of %q: %v", line, err)
	}
	return firedTick{due: due, period: period, ran: fields[1] == "ran"}
}

// lateLeases is a process's lease pool whose Acquire waits until late is closed: the process has
// taken its tick but comes to its Exclusive call late, as a process does whose goroutine the
// machine runs late, while another process takes the ticks after it on time.
type lateLeases struct {
	duty.Leases
	late <-chan struct{}
}

func (l lateLeases) Acquire(ctx context.Context) (*pgxpool.Conn, error) {
	select {
	case <-l.late:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return l.Leases.Acquire(ctx)
}

// runFiredLoop runs one process's RunLeasedOn loop of firedLease on fake, its first tick one
// interval in, until the test ends.
func runFiredLoop(t *testing.T, fake *clocktest.Fake, leases duty.Leases, pass func(context.Context)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		duty.RunLeasedOn(ctx, fake, leases, firedLease, firedInterval, duty.TickAfterInterval, pass)
	}()
	t.Cleanup(func() {
		cancel()
		testwait.Receive(t, "a fired-moment loop's RunLeasedOn to return once its context ended", done)
	})
}

// Each tick hands Exclusive the moment it fired, the value its timer sent, and never the moment
// its loop took it: main's time.Ticker handed the value it sent, and Go sends the moment a tick
// fell due (time.sendTime). The row holds the two things that follow.
//
// A pass that overruns the next tick: that tick comes at once when the pass returns, with the
// moment it fell due, so it serves the period it fell in, and the next tick on the grid serves the
// period after. Here a lone loop's first pass runs from 12:01:50 to 12:03:05, over the tick due at
// 12:02:50 and over the boundary at 12:03. Handed the moment the loop took it, 12:03:05, that tick
// would serve 12:03, period 12:02 would get no pass, and the 12:03:50 tick would be refused.
//
// Two processes that take one tick at different moments: both put it in the period it fired in.
// Here a second process on the same grid comes late to its Exclusive call for the 12:04:50 tick
// (its lease pool holds the call until the row lets it go), while the first takes the 12:04:50
// and 12:05:50 ticks on time. The second takes its 12:05:50 tick at 12:06:05: the moment it fired
// puts it in 12:05, which the first served, so it is refused. Handed the moment it was taken, it
// would serve 12:06, the period the first process's next tick is due in.
func TestRunLeasedHandsExclusiveTheMomentEachTickFired(t *testing.T) {
	logs := &logtest.Buffer{}
	logtest.SetDefault(t, slog.NewTextHandler(logs, nil))
	database := storetest.Open(t)
	fake := clocktest.New(firedStart)
	at := func(minute, second int) time.Time { return time.Date(2026, 9, 22, 12, minute, second, 0, time.UTC) }
	// fire moves the fake to moment once every one of the n loops waiting on a timer has armed it
	// for that moment, which a loop does once its Exclusive call before has returned.
	fire := func(what string, moment time.Time, n int) {
		t.Helper()
		for _, deadline := range fake.AwaitArmed(t, what, n) {
			if !deadline.Equal(moment) {
				t.Fatalf("%s: a loop armed its tick for %s, want %s", what, deadline.UTC().Format(time.TimeOnly), moment.Format(time.TimeOnly))
			}
		}
		fake.Set(moment)
	}
	require := func(what string, got firedTick, fired time.Time, ran bool) {
		t.Helper()
		period := duty.PeriodBoundary(fired, firedInterval)
		if !got.due.Equal(fired) || !got.period.Equal(period) || got.ran != ran {
			t.Errorf("%s: handed Exclusive %s, took the period at %s and ran %t; want %s, the moment it fired, the period at %s, and ran %t",
				what, got.due.UTC().Format(time.TimeOnly), got.period.UTC().Format(time.TimeOnly), got.ran,
				fired.Format(time.TimeOnly), period.Format(time.TimeOnly), ran)
		}
	}

	// The lone loop. Its first pass runs on to 12:03:05, over its next tick and the boundary at 12:03.
	var overran atomic.Bool
	runFiredLoop(t, fake, leasesOf(t, database), func(context.Context) {
		if overran.CompareAndSwap(false, true) {
			fake.Advance(75 * time.Second)
		}
	})
	fire("the first process's first tick", at(1, 50), 1)
	require("the first process's first tick", nextFiredTick(t, logs, "the first process's first tick"), at(1, 50), true)
	const overrun = "the tick the first pass overran, taken at 12:03:05"
	require(overrun, nextFiredTick(t, logs, overrun), at(2, 50), true)
	fire("the first process's tick after the overrun", at(3, 50), 1)
	require("the tick after the overrun", nextFiredTick(t, logs, "the tick after the overrun"), at(3, 50), true)

	// The second process, on the same grid from 12:03:50, late to its Exclusive calls until late
	// closes.
	late := make(chan struct{})
	runFiredLoop(t, fake, lateLeases{Leases: leasesOf(t, storetest.SecondProcess(t, database)), late: late}, func(context.Context) {})
	fire("both processes' ticks", at(4, 50), 2)
	require("the first process's tick at 12:04:50", nextFiredTick(t, logs, "the first process's tick at 12:04:50"), at(4, 50), true)
	fire("the first process's next tick, the second's call still held", at(5, 50), 1)
	require("the first process's tick at 12:05:50", nextFiredTick(t, logs, "the first process's tick at 12:05:50"), at(5, 50), true)
	fake.AwaitArmed(t, "the first process's tick after 12:05:50, so its call has given the lease back", 1)
	fake.Set(at(6, 5))
	close(late)
	held := "the second process's tick at 12:04:50, its call let go at 12:06:05"
	require(held, nextFiredTick(t, logs, held), at(4, 50), false)
	taken := "the second process's tick at 12:05:50, taken at 12:06:05"
	require(taken, nextFiredTick(t, logs, taken), at(5, 50), false)
	fake.AwaitArmed(t, "both processes' next ticks, so the second's calls have returned", 2)
}
