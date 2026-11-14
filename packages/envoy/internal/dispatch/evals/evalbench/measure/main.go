// Command measure is the instrument of the eval registry's measurement Job (dispatch://INFRA-2916,
// T15: k2, k8, k13 and k16), run by evalbench/run.sh against a server booted on the evalbench
// fixture. It prints one RESULT line per measurement and a FAIL line for each one that misses its
// budget or its rule, and exits 1 when any did.
//
//	measure wait  -mode head -database <url> -url <server>
//	measure paint -mode head -database <url> -url <server> -token <token> \
//	  -server-log <the server's stderr> -pg-log <Postgres's jsonlog file>
//
// wait polls the database until the server's consumers have caught up with the fixture: the
// Legion facts consumer has read every project whole under the running build and passed every
// feed's head, and (this head) the day-row refresher has passed both its feeds' heads and walked
// every family to today, so every eval_days row carries the running build.
//
// paint is the first paint after a restart: the reads the page makes, each with the database time
// of its statements (pg_stat_statements' total_exec_time, reset before each), the handler's
// elapsed_ms from the access log (or the live hub's recompute line for a live view), and the
// gzip size of its body; then `explain (analyze, buffers)` of every statement each read ran, with
// the parameters the server bound, which Postgres's statement log holds. At this head the reads
// are GET /api/v1/projects/INFRA/evals at 84 days with an 84-day compare range and at 28 days with
// none, each with no repository and with one, and an evals view snapshot over /ws/live, followed by
// k2's six statements and its refusals and controls, k13's named-jobs read and P0 count, and k16's
// hand folds. At the base head (-mode base) they are the base Delivery page's timeline, measures,
// pipeline and compare views at 84 days, k8's negative control, and the base statements k13's
// control names. -info measures the first paint alone and judges no budget, for the round after a
// Postgres restart that the Job records for information.
//
// The server runs background loops that touch the database between requests. Each read's
// statements are told from theirs by a catalogue taken first: every statement the database ran in
// a quiet window before any read, with no request in flight. A statement of that catalogue that
// a read also runs is counted as background; each read's RESULT line names how many statements
// and how much time it set aside that way, beside the total of everything that ran while it was
// in flight.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

// report collects what one run measured: each line it printed and whether any failed.
type report struct {
	prefix string
	failed int
}

// result prints one measurement.
func (r *report) result(format string, args ...any) {
	fmt.Printf(r.prefix+" "+format+"\n", args...)
}

// fail prints one measurement that missed its budget or its rule, and marks the run failed.
func (r *report) fail(format string, args ...any) {
	r.failed++
	fmt.Printf("FAIL "+format+"\n", args...)
}

// block prints text under a heading, each line indented, so a plan or a statement reads whole in
// the log.
func (r *report) block(heading, text string) {
	fmt.Println(heading)
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		fmt.Println("    " + line)
	}
}

// options is one invocation's flags.
type options struct {
	mode      string
	database  string
	url       string
	token     string
	serverLog string
	pgLog     string
	project   string
	quiet     time.Duration
	catalog   time.Duration
	timeout   time.Duration
	info      bool
}

func main() {
	if len(os.Args) < 2 || (os.Args[1] != "wait" && os.Args[1] != "paint") {
		fmt.Fprintln(os.Stderr, "usage: measure wait|paint -mode head|base -database <url> -url <server> [flags]")
		os.Exit(2)
	}
	command := os.Args[1]
	flags := flag.NewFlagSet(command, flag.ExitOnError)
	var o options
	flags.StringVar(&o.mode, "mode", "head", "head, this head's first paint, or base, the base head's")
	flags.StringVar(&o.database, "database", os.Getenv("DATABASE_URL"), "the Postgres URL the server reads, as a superuser: the instrument resets pg_stat_statements and sets the statement log")
	flags.StringVar(&o.url, "url", "", "the server's base URL")
	flags.StringVar(&o.token, "token", "", "the server's agent token (DISPATCH_AGENT_TOKEN)")
	flags.StringVar(&o.serverLog, "server-log", "", "the file the server's stderr is written to, which holds its access log")
	flags.StringVar(&o.pgLog, "pg-log", "", "Postgres's jsonlog file, which holds each statement with its parameters")
	flags.StringVar(&o.project, "project", "INFRA", "the project the first paint reads")
	flags.DurationVar(&o.quiet, "quiet", 30*time.Second, "how long after the restart the first window starts, so the boot's own passes have ended")
	flags.DurationVar(&o.catalog, "catalog", 65*time.Second, "how long the background catalogue is taken over: two Legion facts sweeps at least")
	flags.DurationVar(&o.timeout, "timeout", time.Hour, "how long wait polls before it fails")
	flags.BoolVar(&o.info, "info", false, "measure the first paint alone and judge no budget")
	if err := flags.Parse(os.Args[2:]); err != nil {
		os.Exit(2)
	}
	if o.mode != "head" && o.mode != "base" {
		fmt.Fprintf(os.Stderr, "measure: -mode is head or base, not %q\n", o.mode)
		os.Exit(2)
	}
	if o.database == "" || o.url == "" {
		fmt.Fprintln(os.Stderr, "measure: name the database with -database and the server with -url")
		os.Exit(2)
	}
	r := &report{prefix: "RESULT"}
	if o.info {
		r.prefix = "INFO"
	}
	ctx := context.Background()
	var err error
	switch command {
	case "wait":
		err = wait(ctx, r, o)
	case "paint":
		if o.token == "" || o.serverLog == "" || o.pgLog == "" {
			fmt.Fprintln(os.Stderr, "measure: paint needs -token, -server-log and -pg-log")
			os.Exit(2)
		}
		err = paint(ctx, r, o)
	}
	if err != nil {
		r.fail("%s: %v", command, err)
	}
	if r.failed > 0 {
		fmt.Printf("%s %s: %d FAIL lines\n", r.prefix, command, r.failed)
		os.Exit(1)
	}
	fmt.Printf("%s %s: no FAIL line\n", r.prefix, command)
}
