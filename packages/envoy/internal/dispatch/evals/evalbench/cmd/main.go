// Command evalbench loads the fixture at INFRA's stated rate (package evalbench) into a database
// the server has migrated and nothing has written, and prints the rows it then holds, one table a
// line:
//
//	go run ./internal/dispatch/evals/evalbench/cmd -database <url> \
//	  -built-by "$COMMIT" -seed 2916 -now 2026-11-13T00:00:00Z
//
// -base writes the base head's tables alone (no reading) into a database the base head
// 02a7b3c6 migrated, for k8's negative control; -scale multiplies every count (0.02 loads in a
// pod in a few minutes); -ten-times writes ten times the stated run rate. Every time is relative
// to -now, the current UTC midnight by default, so a load of the base and one of this head with
// one -now and one -seed hold the same history at the same times.
//
// -failure-day writes a failure day (evalbench.AddFailureDay) into a database this command has
// loaded, and nothing else: -failure-day-runs failed deploy runs with -failure-day-jobs failed
// jobs across them, by default INFRA's worst day (36 and 45), on the UTC day -failure-day days
// before -now's, at -scale and from -seed, as the load it is added to:
//
//	go run ./internal/dispatch/evals/evalbench/cmd -database <url> -seed 2916 \
//	  -now 2026-11-13T00:00:00Z -failure-day 7 -failure-day-runs 36 -failure-day-jobs 45
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/evals/evalbench"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

func main() {
	var spec evalbench.Spec
	var day evalbench.FailureDay
	var database, now string
	flag.StringVar(&database, "database", "", "the Postgres URL to write into (required); the server must have migrated it")
	flag.Uint64Var(&spec.Seed, "seed", 2916, "the seed of every random draw")
	flag.Float64Var(&spec.Scale, "scale", 1, "the share of INFRA's stated rate to write, above 0 and at most 1")
	flag.BoolVar(&spec.TenTimes, "ten-times", false, "write ten times the stated run rate")
	flag.BoolVar(&spec.Base, "base", false, "write only the tables of the base head 02a7b3c6, and no reading")
	flag.StringVar(&spec.BuiltBy, "built-by", "", "the build the logged evals' day rows record: the server's stamped commit")
	flag.StringVar(&now, "now", "", "the moment every history ends at, RFC 3339; the current UTC midnight by default")
	flag.IntVar(&day.DaysAgo, "failure-day", 0, "write only a failure day into the loaded database, on the UTC day this many days before -now's, 1 to 84; 0 loads the fixture")
	flag.IntVar(&day.Runs, "failure-day-runs", 36, "the failure day's failed deploy runs on main at INFRA's stated rate (INFRA's worst day: 36)")
	flag.IntVar(&day.Jobs, "failure-day-jobs", 45, "the failed jobs across the failure day's runs at INFRA's stated rate, -failure-day-runs to three times it (INFRA's worst day: 45)")
	flag.Parse()
	spec.Now = time.Now().UTC().Truncate(24 * time.Hour)
	if now != "" {
		at, err := time.Parse(time.RFC3339, now)
		if err != nil {
			fail(fmt.Errorf("read -now %q as a moment: %w", now, err))
		}
		spec.Now = at.UTC()
	}
	if database == "" {
		fmt.Fprintln(os.Stderr, "usage: evalbench -database <url> [-built-by <commit>] [-seed <n>] [-scale <share>] [-ten-times] [-base] [-now <RFC 3339>] [-failure-day <days> [-failure-day-runs <n>] [-failure-day-jobs <m>]]")
		os.Exit(2)
	}
	ctx := context.Background()
	opened, err := store.Open(ctx, database)
	if err != nil {
		fail(fmt.Errorf("open the database: %w", err))
	}
	defer opened.Pool.Close()
	started := time.Now()
	logf := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "evalbench: "+format+"\n", args...)
	}
	if day.DaysAgo != 0 {
		logf("adding a failure day of %d failed runs and %d failed jobs at scale %g, %d days before %s, seed %d",
			day.Runs, day.Jobs, spec.Scale, day.DaysAgo, spec.Now.Format(time.RFC3339), spec.Seed)
		added, err := evalbench.AddFailureDay(ctx, opened.Pool, spec, day)
		if err != nil {
			fail(err)
		}
		fmt.Printf("RESULT failure-day day=%s runs=%d jobs=%d failed_jobs=%d seconds=%.0f\n",
			added.Day.Format(time.DateOnly), added.Runs, added.Jobs, added.FailedJobs, time.Since(started).Seconds())
		return
	}
	logf("loading seed %d at scale %g (ten times the run rate: %t, base: %t), ending at %s",
		spec.Seed, spec.Scale, spec.TenTimes, spec.Base, spec.Now.Format(time.RFC3339))
	counts, err := evalbench.Load(ctx, opened, spec, logf)
	if err != nil {
		fail(err)
	}
	for _, line := range counts.Lines() {
		fmt.Printf("RESULT load %s\n", line)
	}
	fmt.Printf("RESULT load seconds=%.0f\n", time.Since(started).Seconds())
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "evalbench: %v\n", err)
	os.Exit(1)
}
