// Command evalbench loads the fixture at INFRA's stated rate (package evalbench) into a database
// the server has migrated and nothing has written, and prints the rows it then holds, one table a
// line:
//
//	go run ./internal/dispatch/evals/evalbench/cmd -database <url> \
//	  -built-by "$COMMIT" -seed 2916 -now 2026-11-13T00:00:00Z
//
// -base writes the same history with no eval reading into a database the base's
// envoy-dispatch migrated, the base being main at the commit this head merges (git merge-base
// <head> origin/main), for k8's and k13's negative controls; -one-project writes the one-project
// fixture of the Evals tab's k22 and k24 (INFRA alone, its pull requests in two repositories, no
// reading) into a database this head or the base head 82380570f8 migrated, and prints the checks
// it holds the rows to; -scale multiplies every count (0.02 loads in a pod in a few minutes);
// -ten-times writes ten times the stated run rate. Every time is relative to -now, the current
// UTC midnight by default, so a load of the base and one of this head with one -now and one
// -seed hold the same history at the same times.
//
// -code, on by default, writes INFRA's code survival results (dispatch://INFRA-3206, check k17):
// 365 days of its default repository's chain at cybermonklabs/legion's measured rate, INFRA's two
// code groups and the repository's row, so the server's regroup builds the two code evals' day
// rows when it starts. -code=false leaves them out, for a database whose schema predates them:
// k17's control base, a commit of this branch that holds every table of the eval registry the load
// checks (evalfixture.DerivedTables) but not the code evals, into which the load writes this head's
// history, with its readings, and no -base; where its project_delivery_settings predates
// code_groups (migration 20261126133047), the load writes the settings without that column. -base
// and -one-project write none, whatever -code says.
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
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/evals/evalbench"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

const usage = "usage: evalbench -database <url> [-built-by <commit>] [-seed <n>] [-scale <share>] [-ten-times] [-code=false] [-base | -one-project] [-now <RFC 3339>] [-failure-day <days> [-failure-day-runs <n>] [-failure-day-jobs <m>]]"

// config is what the command line asks: the load, the failure day to add instead of it where
// -failure-day names one, and the database to write into.
type config struct {
	spec     evalbench.Spec
	day      evalbench.FailureDay
	database string
}

// errUsage is the command line that names no database.
var errUsage = errors.New(usage)

// parseConfig reads the command line args, and writes each error it answers to stderr but the
// usage, which its caller prints: the flag package writes its own, and -now's is written here. The
// load's Now is the current UTC midnight where args name none.
func parseConfig(args []string, stderr io.Writer) (config, error) {
	var cfg config
	var now string
	var code bool
	flags := flag.NewFlagSet("evalbench", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&cfg.database, "database", "", "the Postgres URL to write into (required); the server must have migrated it")
	flags.Uint64Var(&cfg.spec.Seed, "seed", 2916, "the seed of every random draw")
	flags.Float64Var(&cfg.spec.Scale, "scale", 1, "the share of INFRA's stated rate to write, above 0 and at most 1")
	flags.BoolVar(&cfg.spec.TenTimes, "ten-times", false, "write ten times the stated run rate")
	flags.BoolVar(&cfg.spec.Base, "base", false, "write into a database of the base, main at the commit this head merges (git merge-base <head> origin/main), with no eval reading")
	flags.BoolVar(&cfg.spec.OneProject, "one-project", false, "write the one-project fixture of the Evals tab's k22 and k24 (INFRA alone, its pull requests in the deploy repository and one other, no reading), into a database of this head or of the base head 82380570f8")
	flags.BoolVar(&code, "code", true, "write INFRA's code survival results (365 days of its default repository's chain, its two code groups and the repository's row); -base and -one-project write none. A database whose schema predates them takes -code=false: k17's control base, a commit of this branch that holds the eval registry's tables but not the code evals, its delivery settings with or without code_groups (migration 20261126133047)")
	flags.StringVar(&cfg.spec.BuiltBy, "built-by", "", "the build the logged evals' day rows record: the server's stamped commit")
	flags.StringVar(&now, "now", "", "the moment every history ends at, RFC 3339; the current UTC midnight by default")
	flags.IntVar(&cfg.day.DaysAgo, "failure-day", 0, "write only a failure day into the loaded database, on the UTC day this many days before -now's, 1 to 84; 0 loads the fixture")
	flags.IntVar(&cfg.day.Runs, "failure-day-runs", 36, "the failure day's failed deploy runs on main at INFRA's stated rate (INFRA's worst day: 36)")
	flags.IntVar(&cfg.day.Jobs, "failure-day-jobs", 45, "the failed jobs across the failure day's runs at INFRA's stated rate, -failure-day-runs to three times it (INFRA's worst day: 45)")
	if err := flags.Parse(args); err != nil {
		return config{}, err
	}
	cfg.spec.NoCode = !code
	cfg.spec.Now = time.Now().UTC().Truncate(24 * time.Hour)
	if now != "" {
		at, err := time.Parse(time.RFC3339, now)
		if err != nil {
			err = fmt.Errorf("read -now %q as a moment: %w", now, err)
			fmt.Fprintf(stderr, "evalbench: %v\n", err)
			return config{}, err
		}
		cfg.spec.Now = at.UTC()
	}
	if cfg.database == "" {
		return config{}, errUsage
	}
	return cfg, nil
}

func main() {
	cfg, err := parseConfig(os.Args[1:], os.Stderr)
	switch {
	case errors.Is(err, flag.ErrHelp):
		return
	case errors.Is(err, errUsage):
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	case err != nil:
		// parseConfig has written the error to stderr.
		os.Exit(2)
	}
	ctx := context.Background()
	opened, err := store.Open(ctx, cfg.database)
	if err != nil {
		fail(fmt.Errorf("open the database: %w", err))
	}
	defer opened.Pool.Close()
	started := time.Now()
	logf := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "evalbench: "+format+"\n", args...)
	}
	if cfg.day.DaysAgo != 0 {
		logf("adding a failure day of %d failed runs and %d failed jobs at scale %g, %d days before %s, seed %d",
			cfg.day.Runs, cfg.day.Jobs, cfg.spec.Scale, cfg.day.DaysAgo, cfg.spec.Now.Format(time.RFC3339), cfg.spec.Seed)
		added, err := evalbench.AddFailureDay(ctx, opened.Pool, cfg.spec, cfg.day)
		if err != nil {
			fail(err)
		}
		fmt.Printf("RESULT failure-day day=%s runs=%d jobs=%d failed_jobs=%d seconds=%.0f\n",
			added.Day.Format(time.DateOnly), added.Runs, added.Jobs, added.FailedJobs, time.Since(started).Seconds())
		return
	}
	logf("loading seed %d at scale %g (ten times the run rate: %t, base: %t, one project: %t, code survival results: %t), ending at %s",
		cfg.spec.Seed, cfg.spec.Scale, cfg.spec.TenTimes, cfg.spec.Base, cfg.spec.OneProject, cfg.spec.WritesCode(), cfg.spec.Now.Format(time.RFC3339))
	counts, err := evalbench.Load(ctx, opened, cfg.spec, logf)
	if err != nil {
		fail(err)
	}
	for _, line := range counts.Lines() {
		fmt.Printf("RESULT load %s\n", line)
	}
	if cfg.spec.OneProject {
		checks, err := evalbench.CheckOneProject(ctx, opened.Pool, cfg.spec.Now)
		if err != nil {
			fail(err)
		}
		for _, line := range checks.Lines() {
			fmt.Printf("RESULT one-project %s\n", line)
		}
	}
	fmt.Printf("RESULT load seconds=%.0f\n", time.Since(started).Seconds())
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "evalbench: %v\n", err)
	os.Exit(1)
}
