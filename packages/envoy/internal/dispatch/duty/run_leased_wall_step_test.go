package duty_test

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// wallStepLease is the lease the wall-step rows' loops share, and wallStepInterval their interval
// and so their period. wallStepStart is on a period boundary, so every period is a whole minute.
const (
	wallStepLease    = "wall-step-loop"
	wallStepInterval = time.Minute
)

var wallStepStart = time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

// leasedLine reads the due and period of a line Exclusive logs for a tick that reached its period
// read: the ran line and the served skip.
var leasedLine = regexp.MustCompile(`msg="dispatch duty: leased pass (ran|skipped)" lease=` + wallStepLease + ` due=(\S+) period=(\S+)`)

// wallStepTick is one fire of one loop: the moment it fired on the wall clock (its deadline on the
// fake's own moment, plus every step), what the loop handed Exclusive there (due) and the period
// that fell in, and whether its pass ran.
type wallStepTick struct {
	wall, due, period time.Time
	ran               bool
}

// wallStepRig is processes' RunLeasedOn loops of one lease on one fake whose wall the row steps
// (clocktest.Fake.StepWall), each process on a pool of its own, and the lines their Exclusive
// calls log. Each loop's pass does nothing: what the rows read is the period each fire buckets
// into and whether it ran.
type wallStepRig struct {
	fake     *clocktest.Fake
	logs     *logtest.Buffer
	stepped  time.Duration
	database *store.Store
}

func newWallStepRig(t *testing.T) *wallStepRig {
	t.Helper()
	logs := &logtest.Buffer{}
	logtest.SetDefault(t, slog.NewTextHandler(logs, nil))
	return &wallStepRig{fake: clocktest.New(wallStepStart), logs: logs, database: storetest.Open(t)}
}

// start runs one process's loop, on a pool of its own over the row's database, until the test
// ends. The first process's pool is the database's own, a later one's that of a second process.
func (r *wallStepRig) start(t *testing.T, first bool) {
	t.Helper()
	database := r.database
	if !first {
		database = storetest.SecondProcess(t, r.database)
	}
	leases := leasesOf(t, database)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		duty.RunLeasedOn(ctx, r.fake, leases, wallStepLease, wallStepInterval, duty.TickAfterInterval, func(context.Context) {})
	}()
	t.Cleanup(func() {
		cancel()
		testwait.Receive(t, "a wall-step loop's RunLeasedOn to return once its context ended", done)
	})
}

// step steps the fake's wall clock by d (StepWall) and keeps the sum, which every later fire's
// wall reading carries.
func (r *wallStepRig) step(d time.Duration) {
	r.fake.StepWall(d)
	r.stepped += d
}

// tick moves the fake to the earliest timer armed on it, the next fire of one of the running
// loops, and reads the line that loop's Exclusive call logs. It returns once that loop has armed
// its next tick, so its call has returned and given the lease back, and the next fire, another
// loop's or its own, finds the lease free and reads the period. loops is how many loops run.
func (r *wallStepRig) tick(t *testing.T, loops int) wallStepTick {
	t.Helper()
	at := r.fake.AwaitArmed(t, "every running loop's next tick", loops)[0]
	r.fake.Set(at)
	line := testwait.Next(t, "the line of the fired loop's Exclusive call", r.logs.Stream(), func(line string) bool {
		return strings.Contains(line, "lease="+wallStepLease)
	})
	r.fake.AwaitArmed(t, "the fired loop's next tick", loops)
	fields := leasedLine.FindStringSubmatch(line)
	if fields == nil {
		t.Fatalf("the fired loop logged %q, want the line of a tick that read its period", line)
	}
	due, err := time.Parse(time.RFC3339Nano, fields[2])
	if err != nil {
		t.Fatalf("the due of %q: %v", line, err)
	}
	period, err := time.Parse(time.RFC3339Nano, fields[3])
	if err != nil {
		t.Fatalf("the period of %q: %v", line, err)
	}
	return wallStepTick{wall: at.Add(r.stepped), due: due, period: period, ran: fields[1] == "ran"}
}

// requireBucketsOnTheWall holds the rule both rows check. Each fire hands Exclusive the moment it
// fired, read on the wall clock, so it buckets into the wall's period at its fire, and every fire
// in one wall period, whichever process made it and whenever that process took it, takes that one
// period. Every wall period a fire falls in runs once: the first fire into it runs, and every later
// one, of either process, is refused.
func requireBucketsOnTheWall(t *testing.T, ticks []wallStepTick) {
	t.Helper()
	runs := map[time.Time]int{}
	for i, tick := range ticks {
		wallPeriod := duty.PeriodBoundary(tick.wall, wallStepInterval)
		if !tick.due.Equal(tick.wall) || !tick.period.Equal(wallPeriod) {
			t.Errorf("fire %d, at %s on the wall clock, handed Exclusive %s and took the period at %s; want the moment it fired on the wall clock and its period, at %s",
				i+1, tick.wall.Format(time.TimeOnly), tick.due.UTC().Format(time.TimeOnly), tick.period.UTC().Format(time.TimeOnly), wallPeriod.Format(time.TimeOnly))
		}
		if first := runs[wallPeriod] == 0; tick.ran != first {
			t.Errorf("fire %d, into the period at %s: ran %t, want %t: the first fire into a period runs it and every later one is refused",
				i+1, wallPeriod.Format(time.TimeOnly), tick.ran, first)
		}
		if tick.ran {
			runs[wallPeriod]++
		}
	}
}

// A step forward of the wall clock between two of a loop's fires. Each fire hands Exclusive the
// moment it fired, read on the stepped wall clock, as the time.Ticker this loop ran on before
// handed the value it sent each tick, so each process buckets on the wall clock. A second process,
// started after the step, whose grid began on the stepped clock, takes the first process's period
// at every fire it makes there. The wall period the step jumped over has no fire in it and so no
// run: that is the ticker's behaviour, whose tick after a step carries the stepped wall, and not a
// period this loop skips.
//
// A grid's due handed instead puts the second process one period off the first: the first's grid
// began before the step, so its fire after the step hands the period the wall stepped over.
func TestRunLeasedBucketsOnTheWallClockAcrossAStepForward(t *testing.T) {
	rig := newWallStepRig(t)
	rig.start(t, true)
	ticks := []wallStepTick{rig.tick(t, 1)}
	rig.step(wallStepInterval)
	// Half an interval on, so the second process's fires come between the first's, one at a time.
	rig.fake.Advance(wallStepInterval / 2)
	rig.start(t, false)
	for range 4 {
		ticks = append(ticks, rig.tick(t, 2))
	}
	requireBucketsOnTheWall(t, ticks)

	stepped := duty.PeriodBoundary(wallStepStart.Add(2*wallStepInterval), wallStepInterval)
	for i, tick := range ticks {
		if tick.period.Equal(stepped) {
			t.Errorf("fire %d took the period at %s, which the wall clock stepped over: no fire falls in it, so none may take it",
				i+1, stepped.Format(time.TimeOnly))
		}
	}
}

// A step back of the wall clock between two of a loop's fires. The fire after it falls in a wall
// period the fire before it ran, so Exclusive refuses it, and the next fire into a later wall
// period runs that one; a second process, started after the step, takes the first process's
// period at every fire it makes there. The rows assert what each fire hands Exclusive and the
// period each takes, not when the fires come: on the fake the first process, whose grid spans the
// step, arms its next fire one interval late after the step, because a time from the fake carries
// no monotonic reading and the loop's own grid sees the step (clocktest.Fake.StepWall). That is
// the fake's behaviour and not production's, where the grid compares the monotonic readings and
// the step does not reach it.
//
// A grid's due handed instead puts the second process one period off the first: the first's grid
// began before the step, so its fire after the step hands a period the wall has not reached.
func TestRunLeasedBucketsOnTheWallClockAcrossAStepBack(t *testing.T) {
	rig := newWallStepRig(t)
	rig.start(t, true)
	ticks := []wallStepTick{rig.tick(t, 1)}
	rig.step(-wallStepInterval)
	rig.fake.Advance(wallStepInterval / 2)
	rig.start(t, false)
	for range 5 {
		ticks = append(ticks, rig.tick(t, 2))
	}
	requireBucketsOnTheWall(t, ticks)

	if ticks[1].ran {
		t.Errorf("the first fire after the step back, into the wall's period at %s that the fire before it ran, ran a pass",
			duty.PeriodBoundary(ticks[1].wall, wallStepInterval).Format(time.TimeOnly))
	}
}
