package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
)

// unreachablePG is a Postgres URL that refuses every connect: each reader of the run's database is
// tested against it to show that it reports the failure instead of an empty result that reads as
// measured.
const unreachablePG = "postgres://postgres@127.0.0.1:1/dispatch?sslmode=disable"

// TestReadErrorsLogsTheFirstAndATotalPastThem: every failure counts; run.log gets one line for each
// of the first pollErrorsLogged, then one line naming the probe and the total, only when more
// failed.
func TestReadErrorsLogsTheFirstAndATotalPastThem(t *testing.T) {
	for _, failures := range []int{1, pollErrorsLogged, pollErrorsLogged + 2} {
		var buf bytes.Buffer
		errs := readErrors{name: "probe", lg: log.New(&buf, "", 0)}
		for i := range failures {
			errs.add(fmt.Errorf("failure %d", i+1))
		}
		if got := errs.done(); got != failures {
			t.Fatalf("done() = %d after %d failures", got, failures)
		}
		lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
		logged := 0
		for i := range failures {
			for _, l := range lines {
				if strings.Contains(l, fmt.Sprintf("failure %d", i+1)) {
					logged++
					if i >= pollErrorsLogged {
						t.Errorf("%d failures: failure %d is logged, past the first %d", failures, i+1, pollErrorsLogged)
					}
				}
			}
		}
		if logged != min(failures, pollErrorsLogged) {
			t.Errorf("%d failures: %d logged, want %d", failures, logged, min(failures, pollErrorsLogged))
		}
		totals := len(lines) - logged
		if failures <= pollErrorsLogged {
			if totals != 0 {
				t.Errorf("%d failures: %d lines besides the failures, want no total:\n%s", failures, totals, buf.String())
			}
			continue
		}
		last := lines[len(lines)-1]
		if totals != 1 || !strings.Contains(last, "probe") || !strings.Contains(last, strconv.Itoa(failures)) {
			t.Errorf("%d failures: want one last line that names the probe and the count:\n%s", failures, buf.String())
		}
	}
}

// pollStart is where the poll rows' fake clocks stand when they begin.
var pollStart = time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)

// Each read takes one slot of the fake clock, so the poll reads every slot from 0 to 30 ms: 31
// reads, every odd one failing.
func TestPollCountsEveryFailedRead(t *testing.T) {
	var buf bytes.Buffer
	lg := log.New(&buf, "", 0)
	clk := clocktest.New(pollStart)
	calls := 0
	failed := poll(context.Background(), clk, pollStart, pollStart.Add(30*time.Millisecond), time.Millisecond, "probe", lg, func(context.Context) error {
		calls++
		clk.Advance(time.Millisecond)
		if calls%2 == 0 {
			return nil
		}
		return errors.New("relation does not exist")
	})
	if calls != 31 || failed != 16 {
		t.Fatalf("%d failed of %d reads, want 16 of the 31 slots: every odd read counted", failed, calls)
	}
	if logged := strings.Count(buf.String(), "relation does not exist"); logged != min(failed, pollErrorsLogged) {
		t.Fatalf("%d failures logged of %d, want the first %d:\n%s", logged, failed, pollErrorsLogged, buf.String())
	}
}

func TestPollStopsUncountedWhenTheContextEnds(t *testing.T) {
	var buf bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	failed := poll(ctx, clocktest.New(pollStart), pollStart, pollStart.Add(time.Hour), time.Millisecond, "probe", log.New(&buf, "", 0), func(context.Context) error {
		calls++
		cancel()
		return context.Canceled
	})
	if failed != 0 || calls != 1 || buf.Len() != 0 {
		t.Fatalf("%d failed of %d reads, log %q: want one read, not counted, nothing logged", failed, calls, buf.String())
	}
}

// Each read takes 25 ms of the fake clock against 10 ms slots from 0 to 60 ms, so it overruns
// the slots after it: the latest one it overran runs at once when it ends, and the others are
// skipped. The reads start at 0 ms, at 25 (slot 20, run late) and at 50 (slot 50), and the read
// that ends at 75 ms has overrun the last slot, 60, which is skipped.
func TestPollSkipsTheSlotsASlowReadOverran(t *testing.T) {
	clk := clocktest.New(pollStart)
	var started []time.Duration
	failed := poll(context.Background(), clk, pollStart, pollStart.Add(60*time.Millisecond), 10*time.Millisecond, "probe", log.New(&bytes.Buffer{}, "", 0), func(context.Context) error {
		started = append(started, clk.Now().Sub(pollStart))
		clk.Advance(25 * time.Millisecond)
		return nil
	})
	if want := []time.Duration{0, 25 * time.Millisecond, 50 * time.Millisecond}; failed != 0 || !slices.Equal(started, want) {
		t.Fatalf("reads started at %v (%d failed), want %v and none failed", started, failed, want)
	}
}
