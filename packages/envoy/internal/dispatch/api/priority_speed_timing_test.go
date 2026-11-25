package api

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/benchtime"
)

// priority_speed_timing_test.go is the timing lane of priority_speed_test.go's readings
// (benchtime.TimingEnv): the times of the three requests the priorities read is held by, taken
// over the one corpus (timeSpeedOperations) once every count is in, the plan counts that say which
// plans the timed runs ran (assertSpeedPlansSettled), and TestPrioritiesReadSpeed, which holds
// the times. measureSpeedCorpus takes them only when the lane is open, so every run outside it
// counts what it counts and times nothing.

// speedRuns is how many times each priorities operation is run and timed after its warm-up runs
// (speedWarmups), interleaved with the other two: how many runs a load that slows one operation
// and spares another has to reach before the least of each reads it as cost. Twenty-one was sized
// when the relation read was timed too, on that read's ratio to the page without it (2026-11-16):
// groups of 5 passes drawn from five runs of 61 put that ratio over its bound in 31 of 100,000,
// and groups of 11 or more in none.
const speedRuns = 21

// speedWarmups is how many times each timed operation runs before any run is timed: past the
// runs Postgres plans for their own arguments, so every timed run runs the plan a deployment's
// connections run from then on. With one warm-up run, the first three timed runs of the whole
// priorities body read 22.2 to 23.0 ms and every later one 43.9 to 83.3 ms (2026-11-20): the
// least of the runs was a reading of the plan a connection runs five times and then leaves.
const speedWarmups = speedCustomPlans

// speedTiming is one operation's readings: every run, in the order they were taken, so a number
// in the report can be read against the spread it came from.
type speedTiming struct {
	name string
	runs []time.Duration
}

// least is the reading of record: the quickest of the runs, the one the box's load added least
// to, which noise can only raise.
func (s speedTiming) least() time.Duration {
	return slices.Min(s.runs)
}

func (s speedTiming) String() string {
	readings := make([]string, 0, len(s.runs))
	for _, run := range s.runs {
		readings = append(readings, fmt.Sprintf("%.1f", msOf(run)))
	}
	return fmt.Sprintf("%s: %.1f ms (least of %v ms)", s.name, msOf(s.least()), readings)
}

// speedTimings is the three timings of the timing lane, with what the plan counts said of the
// runs they timed (assertSpeedPlansSettled).
type speedTimings struct {
	pagedIssues    speedTiming
	prioritiesFull speedTiming
	prioritiesLite speedTiming
	unsettledRuns  []string
}

// TestPrioritiesReadSpeed is TestPrioritiesReadWork's check in time, the measurement lane's
// (benchtime.TimingEnv): the whole body under two seconds, and under five times the paged
// listing of every open issue; and the summary the navigation rail reads under 300 ms, since it
// is drawn on every page. Every timed run runs the plans a deployment's connections run
// (speedWarmups), or the times are of nothing it runs.
func TestPrioritiesReadSpeed(t *testing.T) {
	benchtime.Timing(t, os.Getenv(benchtime.TimingEnv))
	readings := speedCorpusReadings(t).timings
	if readings == nil {
		t.Fatal("the run that built the corpus took no times, though the timing lane is open")
	}
	t.Log(readings.pagedIssues.String())
	t.Log(readings.prioritiesFull.String())
	t.Log(readings.prioritiesLite.String())
	if len(readings.unsettledRuns) > 0 {
		t.Fatalf("the timed runs ran a plan a connection runs for no more than its first %d runs of a statement:\n%s",
			speedCustomPlans, strings.Join(readings.unsettledRuns, "\n"))
	}
	paged, full, summary := readings.pagedIssues.least(), readings.prioritiesFull.least(), readings.prioritiesLite.least()
	ratio := float64(full) / float64(paged)
	t.Logf("%s costs %.2fx the paged listing of every open issue, at most %.2fx", speedPriorityFullName, ratio, speedPrioritiesRatio)
	if full > speedPrioritiesBound {
		t.Fatalf("%s took %.1f ms over %d issues and %d priorities at the least of %d runs, want under %v",
			speedPriorityFullName, msOf(full), speedCorpusIssues, speedCorpusPriorities, speedRuns, speedPrioritiesBound)
	}
	if ratio > speedPrioritiesRatio {
		t.Fatalf("%s took %.1f ms against the paged listing's %.1f ms, the least of %d runs each (%.2fx), want at most %.2fx",
			speedPriorityFullName, msOf(full), msOf(paged), speedRuns, ratio, speedPrioritiesRatio)
	}
	if summary > speedPrioritySummary {
		t.Fatalf("the priorities summary took %.1f ms at the least of %d runs, want under %v", msOf(summary), speedRuns, speedPrioritySummary)
	}
}

// timeSpeedOperations takes every timing of the timing lane over corpus: speedWarmups passes over
// the three timed operations that are not counted, then speedRuns passes, interleaved so a box
// that slows in the middle of the run slows each of them alike, every other pass in the reverse
// order, so no operation always runs on what the one before it warmed. The pool's plan counts
// before and after the timed passes say which plans the timed runs ran (assertSpeedPlansSettled).
func timeSpeedOperations(t *testing.T, corpus speedCorpusShape) *speedTimings {
	t.Helper()
	timings := speedTimings{
		pagedIssues:    speedTiming{name: "the paged listing of every open issue"},
		prioritiesFull: speedTiming{name: speedPriorityFullName},
		prioritiesLite: speedTiming{name: "the priorities summary"},
	}
	operations := []struct {
		into *speedTiming
		run  func()
	}{
		{&timings.pagedIssues, func() { speedRequest(t, corpus, speedPagedListingTarget) }},
		{&timings.prioritiesFull, func() { speedRequest(t, corpus, "/api/v1/priorities") }},
		{&timings.prioritiesLite, func() { speedRequest(t, corpus, "/api/v1/priorities?summary=true") }},
	}
	for range speedWarmups {
		for _, operation := range operations {
			operation.run()
		}
	}
	before := speedPlanRuns(t, corpus)
	for pass := range speedRuns {
		order := slices.Clone(operations)
		if pass%2 == 1 {
			slices.Reverse(order)
		}
		for _, operation := range order {
			started := time.Now()
			operation.run()
			operation.into.runs = append(operation.into.runs, time.Since(started))
		}
	}
	timings.unsettledRuns = assertSpeedPlansSettled(before, speedPlanRuns(t, corpus))
	return &timings
}

// speedPrepared is one statement prepared on one connection: the connection's server process and
// the statement's name, which pgx derives from its text.
type speedPrepared struct {
	backend int32
	name    string
}

// speedPreparedRuns is how many times one prepared statement has run, each run one plan, custom
// or generic, as pg_prepared_statements counts them, and the start of its text, which a failure
// prints.
type speedPreparedRuns struct {
	runs      int64
	statement string
}

// speedPlanRuns reads the runs of every statement prepared on every idle connection of the pool.
// It reads them by the simple protocol, so the read prepares no statement of its own to be
// counted.
func speedPlanRuns(t *testing.T, corpus speedCorpusShape) map[speedPrepared]speedPreparedRuns {
	t.Helper()
	ctx := context.Background()
	prepared := map[speedPrepared]speedPreparedRuns{}
	for _, connection := range corpus.server.deps.Store.Pool.AcquireAllIdle(ctx) {
		rows, err := connection.Query(ctx,
			`select pg_backend_pid(), name, generic_plans + custom_plans, left(regexp_replace(statement, '\s+', ' ', 'g'), 100) from pg_prepared_statements`,
			pgx.QueryExecModeSimpleProtocol)
		if err != nil {
			connection.Release()
			t.Fatalf("read the plans prepared on a connection: %v", err)
		}
		for rows.Next() {
			var key speedPrepared
			var runs speedPreparedRuns
			if err := rows.Scan(&key.backend, &key.name, &runs.runs, &runs.statement); err != nil {
				rows.Close()
				connection.Release()
				t.Fatalf("read a prepared statement's plans: %v", err)
			}
			prepared[key] = runs
		}
		rows.Close()
		err = rows.Err()
		connection.Release()
		if err != nil {
			t.Fatalf("read the plans prepared on a connection: %v", err)
		}
	}
	return prepared
}

// assertSpeedPlansSettled answers each statement the timed passes ran (its runs grew between
// before and after) whose runs before them were fewer than speedCustomPlans: one of its timed
// runs ran a plan made for its own arguments that a connection runs for those first runs alone.
// A timed pass that ran no prepared statement at all is answered too, since the plans it ran
// could not be read.
func assertSpeedPlansSettled(before, after map[speedPrepared]speedPreparedRuns) []string {
	unsettled := []string{}
	ran := 0
	for key, now := range after {
		earlier := before[key].runs
		if now.runs == earlier {
			continue
		}
		ran++
		if earlier < speedCustomPlans {
			unsettled = append(unsettled, fmt.Sprintf("backend %d ran %q %d times before the timed passes and %d times in them",
				key.backend, now.statement, earlier, now.runs-earlier))
		}
	}
	if ran == 0 {
		unsettled = append(unsettled, fmt.Sprintf("no statement prepared on the %d idle connections read ran in the timed passes, so the plans they ran are unknown", len(after)))
	}
	slices.Sort(unsettled)
	return unsettled
}
