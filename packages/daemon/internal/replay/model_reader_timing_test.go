package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/signals"
)

// timingEnv, set to 1, runs the timing tests of a replay's signals, which measure the cost of a pass
// on the wall clock and so run only where a check asks for them, on a machine of their own.
const timingEnv = "LEGION_REPLAY_TIMING"

// skipUnlessTiming skips t unless LEGION_REPLAY_TIMING=1 (timingEnv).
func skipUnlessTiming(t *testing.T) {
	t.Helper()
	if os.Getenv(timingEnv) != "1" {
		t.Skipf("a timing test: set %s=1 to run it", timingEnv)
	}
}

// timingWindow is a window of minutes minutes from 2001-03-05T00:00Z, made to load the signals:
// issues roots promoted at its start under a cap 10 above them, each architect's turn ending at a
// minute of its own so that holders go idle, the daemon's tick every tick, and a record of every
// signal over the whole span. The provider's record holds every minute, 20,000 to 34,963 uncached
// tokens a minute under the project's own limit of 40,000, a refusal of the project's own every 97
// minutes and of the provider's every 89; ACME-1's pod is killed every 37 minutes; and a pull
// request of acme/widgets enters the merge queue every 50 minutes and waits 20.
func timingWindow(t *testing.T, minutes, issues int, tick time.Duration) Window {
	b := newWindowBuilder(t, minuteOf(0), minuteOf(minutes))
	b.capped(issues + 10)
	b.window.TickInterval = Duration(tick)
	b.boot(minuteOf(-60))
	for i := range issues {
		key := fmt.Sprintf("ACME-%d", i+1)
		promotedRoot(b, minuteOf(0), key, fmt.Sprintf("a%03d", i+1))
	}
	for minute := range minutes {
		var own int64
		if minute%97 == 0 {
			own = 1
		}
		line := providerLine(minute, int64(20000+(minute*37)%15000), 40000, own)
		if minute%89 == 0 {
			line.Provider = 1
		}
		b.window.Provider = append(b.window.Provider, line)
	}
	var kills []int
	for minute := 5; minute < minutes; minute += 37 {
		kills = append(kills, minute)
	}
	guardKills(b, "legion-acme-acme-1", kills...)
	b.window.Queue = map[string][]json.RawMessage{builtRepository: queueLinesOf(waitsEvery(10, minutes/50, 50, 20))}
	whole := []Span{{From: minuteOf(0), To: minuteOf(minutes)}}
	b.window.Signals = map[string][]Span{SignalIdle: whole, SignalQueue: whole, SignalProvider: whole, SignalKills: whole, SignalApprovals: whole}
	return b.build()
}

// A pass of the load models read as a replay runs costs what the moments near it cost, not the
// window's length (ModelReader): a window of 5,760 minutes read at 100,000 passes, as a real window
// of four days is, costs no more for each pass than a window of 1,440 minutes at the same passes a
// minute, within a factor of 2. Each pass reads every modelled signal, each next crossing, the
// back-off's setbacks and its next rise, and each next change of a modelled input, and every 50th
// pass moves the issues in flight. It logs the cost of a pass on each window.
func TestAPassOfTheModelReaderCostsNoMoreOnALongerWindow(t *testing.T) {
	skipUnlessTiming(t)
	perPass := map[int]float64{}
	for _, minutes := range []int{1440, 5760} {
		w := timingWindow(t, minutes, 20, time.Hour)
		models := fittedModels(t, w)
		record := recordTimelineOf(t, models)
		passes := minutes * 100000 / 5760
		step := w.To.Sub(w.From) / time.Duration(passes)
		result := testing.Benchmark(func(b *testing.B) {
			for range b.N {
				reader, err := models.Reader(w.From, SetbackSources{Refusals: true, Kills: true})
				if err != nil {
					b.Fatal(err)
				}
				for _, start := range record.Segments {
					reader.Segment(start.Issue, start.Recorded, start.Replayed)
				}
				for i := range passes {
					at := w.From.Add(time.Duration(i) * step)
					if i%50 == 0 {
						if err := reader.Flight(at, 20+i%17); err != nil {
							b.Fatal(err)
						}
					}
					reader.Headroom(at)
					reader.Refusals(at)
					reader.Kills(at)
					reader.Queue(at)
					reader.HeadroomNext(at, 1)
					reader.RefusalsNext(at, 4)
					reader.KillsNext(at, 2)
					reader.QueueNext(at, 2)
					signals.NextRise(30, reader.Setbacks(at, 30), at)
					reader.NextKill(at)
					reader.NextQueueChange(at)
					reader.NextRefusal(at)
				}
			}
		})
		perPass[minutes] = float64(result.NsPerOp()) / float64(passes)
		t.Logf("a window of %d minutes, %d passes: %.0f ns a pass (%d runs of the window, %s each)", minutes, passes, perPass[minutes], result.N,
			time.Duration(result.NsPerOp()))
	}
	ratio := perPass[5760] / perPass[1440]
	t.Logf("a pass on the window of 5,760 minutes costs %.2f times one on the window of 1,440", ratio)
	if ratio > 2 {
		t.Errorf("a pass on the window of 5,760 minutes costs %.2f times one on the window of 1,440, want at most 2", ratio)
	}
}

// A run under a rule that admits by signals costs a small factor more than the same run under main
// (spec R11): on a window of 1,440 minutes whose daemon ticks every minute, 30 issues in flight and
// a record of every signal, a run under signals-backoff with a limit on every signal, reading the
// snapshot at each pass and keeping the deadline, takes at most 3 times the wall time of the run
// under main with no limit. It logs both times and the passes of each.
func TestASignalRunCostsASmallFactorOverMain(t *testing.T) {
	skipUnlessTiming(t)
	w := timingWindow(t, 1440, 30, time.Minute)
	dsn := replayDSN(t)
	timed := func(rule string, limits map[string]float64) (time.Duration, RunResult, int) {
		out := t.TempDir()
		began := time.Now()
		result, err := Run(context.Background(), RunConfig{Window: w, Rule: replayedRule(t, rule), Seed: 1, Slowdown: 1, PGDSN: dsn, Out: out, Limits: limits})
		if err != nil {
			t.Fatalf("replay under %s: %v", rule, err)
		}
		return time.Since(began), result, runFacts(t, out)
	}
	mainTime, _, mainFacts := timed("main", nil)
	signalTime, signalled, signalFacts := timed("signals-backoff", everyLimit)
	ratio := float64(signalTime) / float64(mainTime)
	t.Logf("main: %s for %d facts; signals-backoff with every limit: %s for %d facts, %d deadline changes; %.2f times main's",
		mainTime.Round(time.Millisecond), mainFacts, signalTime.Round(time.Millisecond), signalFacts, len(signalled.Deadlines), ratio)
	if ratio > 3 {
		t.Errorf("the run under signals-backoff took %.2f times the run under main, want at most 3", ratio)
	}
}
