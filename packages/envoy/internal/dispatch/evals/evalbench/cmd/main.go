// Command evalbench loads the fixture at INFRA's stated rate (package evalbench) into a database
// the server has migrated and nothing has written, and prints the rows it then holds, one table a
// line:
//
//	go run ./internal/dispatch/evals/evalbench/cmd -database "$DATABASE_URL" \
//	  -built-by "$COMMIT" -seed 2916 -now 2026-11-13T00:00:00Z
//
// -base writes the base head's tables alone (no reading) into a database the base head
// 02a7b3c6 migrated, for k8's negative control; -scale multiplies every count (0.02 loads in a
// pod in a few minutes); -ten-times writes ten times the stated run rate. Every time is relative
// to -now, the current UTC midnight by default, so a load of the base and one of this head with
// one -now and one -seed hold the same history at the same times.
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
	var database, now string
	flag.StringVar(&database, "database", os.Getenv("DATABASE_URL"), "the Postgres URL to write into, or $DATABASE_URL; the server must have migrated it")
	flag.Uint64Var(&spec.Seed, "seed", 2916, "the seed of every random draw")
	flag.Float64Var(&spec.Scale, "scale", 1, "the share of INFRA's stated rate to write, above 0 and at most 1")
	flag.BoolVar(&spec.TenTimes, "ten-times", false, "write ten times the stated run rate")
	flag.BoolVar(&spec.Base, "base", false, "write only the tables of the base head 02a7b3c6, and no reading")
	flag.StringVar(&spec.BuiltBy, "built-by", "", "the build the logged evals' day rows record: the server's stamped commit")
	flag.StringVar(&now, "now", "", "the moment every history ends at, RFC 3339; the current UTC midnight by default")
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
		fail(fmt.Errorf("name the database to write with -database or $DATABASE_URL"))
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
