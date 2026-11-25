package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
)

// unreachableDatabase is a DATABASE_URL nothing listens on: a run that gets past its throttle
// fails at the connect, exit 1, so an exit 2 is the throttle's refusal and nothing else.
const unreachableDatabase = "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1"

// throttleRead is graphBackfillThrottle over the settings values and the arguments args.
func throttleRead(values map[string]string, args ...string) (graphcensus.Throttle, bool, string) {
	var out bytes.Buffer
	throttle, ok := graphBackfillThrottle(args, envGetter(values), &out)
	return throttle, ok, out.String()
}

// graphBackfillBatchRowsReachesItsReader is DISPATCH_GRAPH_BACKFILL_BATCH_ROWS's case of
// TestEverySettingReachesItsReader: the batch backfill-graph-store runs at, read through the table
// and trimmed; unset or empty, the default's; and a value no run takes refused.
func graphBackfillBatchRowsReachesItsReader(t *testing.T) {
	for value, want := range map[string]int{"": graphcensus.DefaultThrottle.Batch, " 40 ": 40, "1": 1, "1000": 1000} {
		if throttle, ok, out := throttleRead(map[string]string{"DISPATCH_GRAPH_BACKFILL_BATCH_ROWS": value}); !ok || throttle.Batch != want {
			t.Errorf("DISPATCH_GRAPH_BACKFILL_BATCH_ROWS=%q: batch %d (taken %t, %q), want %d", value, throttle.Batch, ok, out, want)
		}
	}
	if throttle, ok, _ := throttleRead(nil); !ok || throttle != graphcensus.DefaultThrottle {
		t.Errorf("unset: %+v (taken %t), want %+v", throttle, ok, graphcensus.DefaultThrottle)
	}
	for _, value := range []string{"0", "1001", "-5", "forty", "40 rows"} {
		if _, ok, out := throttleRead(map[string]string{"DISPATCH_GRAPH_BACKFILL_BATCH_ROWS": value}); ok || !strings.Contains(out, "refused") {
			t.Errorf("DISPATCH_GRAPH_BACKFILL_BATCH_ROWS=%q: taken %t, printed %q; want a refusal", value, ok, out)
		}
	}
}

// graphBackfillRowsPerSecondReachesItsReader is DISPATCH_GRAPH_BACKFILL_ROWS_PER_SECOND's case of
// TestEverySettingReachesItsReader: the rate backfill-graph-store runs at, read through the table
// and trimmed; unset or empty, the default's, which pauses; 0, no pause, only where it is named;
// and a value no run takes refused.
func graphBackfillRowsPerSecondReachesItsReader(t *testing.T) {
	for value, want := range map[string]int{"": graphcensus.DefaultThrottle.Rate, " 250 ": 250, "0": 0} {
		if throttle, ok, out := throttleRead(map[string]string{"DISPATCH_GRAPH_BACKFILL_ROWS_PER_SECOND": value}); !ok || throttle.Rate != want {
			t.Errorf("DISPATCH_GRAPH_BACKFILL_ROWS_PER_SECOND=%q: rate %d (taken %t, %q), want %d", value, throttle.Rate, ok, out, want)
		}
	}
	if graphcensus.DefaultThrottle.Rate == 0 {
		t.Error("the default rate is 0: a run with no setting and no flag would pause nowhere")
	}
	for _, value := range []string{"-1", "fast", "1.5"} {
		if _, ok, out := throttleRead(map[string]string{"DISPATCH_GRAPH_BACKFILL_ROWS_PER_SECOND": value}); ok || !strings.Contains(out, "refused") {
			t.Errorf("DISPATCH_GRAPH_BACKFILL_ROWS_PER_SECOND=%q: taken %t, printed %q; want a refusal", value, ok, out)
		}
	}
}

// backfill-graph-store takes its throttle from its flags over its settings over the default, and
// refuses a throttle no run takes with exit 2 before it opens the database. The control for that
// refusal is the same command with a throttle it takes: it opens the database, which nothing
// serves, and exits 1, so the exit 2 is the throttle's and not the connect's.
func TestTheGraphBackfillTakesItsThrottleFromItsFlagsOverItsSettings(t *testing.T) {
	settings := map[string]string{
		"DISPATCH_GRAPH_BACKFILL_BATCH_ROWS":      "40",
		"DISPATCH_GRAPH_BACKFILL_ROWS_PER_SECOND": "250",
	}
	for _, row := range []struct {
		values map[string]string
		args   []string
		want   graphcensus.Throttle
	}{
		{nil, nil, graphcensus.DefaultThrottle},
		{settings, nil, graphcensus.Throttle{Batch: 40, Rate: 250}},
		{settings, []string{"--batch-rows", "70"}, graphcensus.Throttle{Batch: 70, Rate: 250}},
		{settings, []string{"--rows-per-second=300"}, graphcensus.Throttle{Batch: 40, Rate: 300}},
		{nil, []string{"--batch-rows=1000", "--rows-per-second", "0"}, graphcensus.Throttle{Batch: 1000, Rate: 0}},
	} {
		if throttle, ok, out := throttleRead(row.values, row.args...); !ok || throttle != row.want {
			t.Errorf("settings %v, args %q: %+v (taken %t, %q), want %+v", row.values, row.args, throttle, ok, out, row.want)
		}
	}
	// opened is whether a run's output says it reached the database: openMigrated names the open
	// or the migration that failed under the command's label.
	opened := func(out string) bool {
		return strings.Contains(out, "backfill-graph-store: open database") || strings.Contains(out, "backfill-graph-store: migrate database")
	}
	env := envGetter(map[string]string{"DATABASE_URL": unreachableDatabase})
	for _, args := range [][]string{
		{"--batch-rows", "0"},
		{"--batch-rows", "1001"},
		{"--rows-per-second", "-1"},
		{"--rows-per-second", "fast"},
		{"--pause", "1s"},
		{"blocks"},
	} {
		var stdout, stderr bytes.Buffer
		if code := runSubcommand(context.Background(), append([]string{"backfill-graph-store"}, args...), env, &stdout, &stderr); code != 2 || opened(stdout.String()) {
			t.Errorf("backfill-graph-store %q: exit %d, printed %q; want exit 2 before the database is opened", args, code, stdout.String())
		}
	}
	var stdout, stderr bytes.Buffer
	code := runSubcommand(context.Background(), []string{"backfill-graph-store", "--batch-rows", "7", "--rows-per-second", "0"}, env, &stdout, &stderr)
	if code != 1 || !opened(stdout.String()) {
		t.Errorf("control: backfill-graph-store with a throttle it takes exited %d, printed %q; want exit 1 at the database nothing serves", code, stdout.String())
	}
}
