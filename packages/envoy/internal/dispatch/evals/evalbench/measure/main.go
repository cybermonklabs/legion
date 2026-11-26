// Command measure is the instrument of the eval registry's measurement Job (dispatch://INFRA-2916,
// checks k2, k8, k13 and k16), run by evalbench/run.sh against a server booted on the evalbench
// fixture. It prints one RESULT line per measurement and a FAIL line for each one that misses its
// budget or its rule, and exits 1 when any did.
//
//	measure wait  -mode head -database <url> -url <server>
//	measure paint -mode head -database <url> -url <server> -token <token> \
//	  -server-log <the server's stderr> -pg-log <Postgres's jsonlog file> [-run <n> -runs-file <file>]
//	measure runs  -runs <n> -runs-file <file>
//	measure ghstub -log <file> -port-file <file> [-listen 127.0.0.1:0]
//
// -mode is head, base or failure-day: failure-day is this head over the fixture with a failure
// day added to it (evalbench.AddFailureDay), whose paint is the Delivery timeline's alone; base is
// the base, main at the commit this head merges (git merge-base <head> origin/main).
//
// wait polls the database until the server's consumers have caught up with the fixture: the
// Legion facts consumer has read every project whole under the running build and passed every
// feed's head, and (this head) the day-row refresher has passed its feeds' heads, done every walk,
// served every family's rows under the running build (eval_days_serving) with no rebuild building
// and built every project's stored figures through today's last end day, and the keyword fill's
// cursor stands at the newest event, which evalbench's load leaves it at as of the load, so it has
// only the server's own later events to follow (consumerState.caughtUp), and, where the database
// holds the code survival results, the regroup's pass at the server's start has built INFRA's day
// rows of the code evals (code_projects.groups_digest set). At the base it waits for those of the
// facts consumer, the fill and the refresher the base's catalog holds the tables of. Where the
// server runs the fill it prints how long that took.
//
// paint is the first paint after a restart: the reads the page makes, each with the database time
// of its statements (pg_stat_statements' total_exec_time, reset before each), the handler's
// elapsed_ms from the access log (or the live hub's recompute line for a live view), and the
// gzip size of its body; then `explain (analyze, buffers)` of every statement each read ran, with
// the parameters the server bound, which Postgres's statement log holds. At this head the reads
// are GET /api/v1/projects/INFRA/evals at 84 days with an 84-day compare range and at 28 days with
// none, each with no repository and with one, and an evals view snapshot over /ws/live, then the
// Delivery timeline's first paint (GET /api/v1/delivery/timeline at 84 days with the compare
// range's GET /api/v1/delivery/measures beside it, and at 28 days), followed by k2's six
// statements and its refusals and controls, k13's run-jobs rule over every read of the paint, its
// named-jobs read and P0 list, k16's hand folds, and the probe of the 84-day compare read's
// stored-figures statements (probe.go). In mode failure-day they are the Delivery timeline's reads
// alone, over the same windows, one of which holds the failure day, with k13's run-jobs rule and
// the failure day the database holds. At the base (-mode base) they are the base Delivery page's
// first paint at 84 days with a compare range, the timeline view of the window and the measures
// view of the compare range, k8's negative control, and the base statements k13's control names.
// Every Delivery read's answer is printed as the history it shows: its pull requests, those
// attributed to an issue, by the issue's project, and the measures' per-PR total and deploys; the
// base's control fails on a page that shows no pull request attributed to an issue, or none in its
// measures. -info measures the first paint alone and judges no budget, for the round after a
// Postgres restart that the Job records for information: its lines are INFO lines, a rule it
// misses is an INFO-FAIL line, which no verdict counts, and it exits 0.
//
// -run n tags each RESULT and FAIL line of the paint with run=n, one of the first paint's runs
// (k8: run.sh paints this head's first paint EVALBENCH_PAINT_RUNS times, 8 by default, each as the
// first requests after a fresh start of the server), and -runs-file appends the run's reads to a
// file. k2's statements and controls, k13's named-jobs read and P0 list, k16's day rows by
// repository and hand folds, the probe, k16's fallback and its move of the default repository are
// measured in run 1 alone, after its paint; the reads of every run are judged by their budgets,
// and by k8's and k13's rules over their plans. runs then prints, for each read and each group of
// reads, its database time in each run with the median and the maximum of the runs, and fails one
// over its budget in any run (runs.go). -now is the day the paint's windows end on, the end of the
// history the generator wrote (job.sh's EVALBENCH_NOW), and -node, which run.sh gives
// EVALBENCH_NODE, the node the run is on, which the lines of k16's fallback and move name.
//
// -github-log names the log of the GitHub stand-in the server ran against (ghstub, which run.sh
// starts under EVALBENCH_GITHUB_STUB=1, with a throwaway App whose API base is the stand-in): paint
// then prints a FAIL line for each request the log holds that the stand-in received between the
// paint's first request and the answer of its last (check k17, dispatch://INFRA-3206 R15: a page
// read costs no request to GitHub), and a RESULT line with how many came inside the paint's window
// and how many outside it. Whatever the server, paint prints a FAIL line for each statement of its
// reads that names code_changes, code_pairs or code_days, whose rows no paint reads: the day rows
// and the stored figures hold what the paint shows. At this head each read of the evals list holds
// both code evals and, over 84 days, states a value of each. A base that holds the eval registry
// (a commit of this branch before the code evals) is measured by the same list reads with no
// budget, and its answer must hold neither code eval.
//
// The server runs background loops that touch the database between requests. Each read's
// statements are told from theirs by a catalogue taken first: every statement the database ran in
// a quiet window before any read, with no request in flight. A statement of that catalogue that a
// read also runs is counted as the read's own where the statement log shows it on the read's own
// backends, and as background where it shows it on others (ownAndBackground); each read's RESULT
// line names how many catalogued statements it counted as its own, and how many statements and how
// much time it set aside as background, beside the total of everything that ran while it was in
// flight.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

// report collects what one invocation measured: the text of each RESULT line (lines) and of each
// FAIL line (fails) it printed. tag, where it is set, follows the prefix of every RESULT and FAIL
// line: " run=3" for the third of the first paint's runs. An -info invocation's prefix is INFO, and
// it writes a rule it misses as an INFO-FAIL line, which no verdict counts (job.yaml's reads
// `^FAIL`).
type report struct {
	prefix string
	tag    string
	lines  []string
	fails  []string
}

// result prints one measurement.
func (r *report) result(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	r.lines = append(r.lines, line)
	fmt.Printf("%s%s %s\n", r.prefix, r.tag, line)
}

// fail prints one measurement that missed its budget or its rule, and marks the run failed.
func (r *report) fail(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	r.fails = append(r.fails, line)
	word := "FAIL"
	if r.prefix == "INFO" {
		word = "INFO-FAIL"
	}
	fmt.Printf("%s%s %s\n", word, r.tag, line)
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
	run       int
	runs      int
	runsFile  string
	now       time.Time
	node      string
	githubLog string
}

const usage = "usage: measure wait|paint|runs|ghstub -mode head|failure-day|base -database <url> -url <server> [flags]"

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "ghstub" {
		os.Exit(ghstub(os.Args[2:], os.Stderr))
	}
	if len(os.Args) < 2 || (os.Args[1] != "wait" && os.Args[1] != "paint" && os.Args[1] != "runs") {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	command := os.Args[1]
	flags := flag.NewFlagSet(command, flag.ExitOnError)
	var o options
	flags.StringVar(&o.mode, "mode", "head", "head, this head's first paint; failure-day, this head's timeline once a failure day is added; or base, the first paint of the base, main at the commit this head merges")
	flags.StringVar(&o.database, "database", "", "the Postgres URL the server reads, as a superuser (required): the instrument resets pg_stat_statements and sets the statement log")
	flags.StringVar(&o.url, "url", "", "the server's base URL")
	flags.StringVar(&o.token, "token", "", "the server's agent token (DISPATCH_AGENT_TOKEN)")
	flags.StringVar(&o.serverLog, "server-log", "", "the file the server's stderr is written to, which holds its access log")
	flags.StringVar(&o.pgLog, "pg-log", "", "Postgres's jsonlog file, which holds each statement with its parameters")
	flags.StringVar(&o.project, "project", "INFRA", "the project the first paint reads")
	flags.DurationVar(&o.quiet, "quiet", 30*time.Second, "how long after the restart the first window starts, so the boot's own passes have ended")
	flags.DurationVar(&o.catalog, "catalog", 65*time.Second, "how long the background catalogue is taken over: more than a minute, so every loop a quiet server runs on a minute's pace or on the Legion sampler's minute wake runs in it")
	flags.DurationVar(&o.timeout, "timeout", time.Hour, "how long wait polls before it fails")
	flags.BoolVar(&o.info, "info", false, "measure the first paint alone and judge no budget")
	flags.IntVar(&o.run, "run", 0, "paint: which of the first paint's runs this is, from 1, which tags its lines run=<n>; the checks after the paint are run 1's; 0 for a paint measured once")
	flags.StringVar(&o.runsFile, "runs-file", "", "paint: the file each run's reads are appended to; runs: the file it reads them from")
	flags.IntVar(&o.runs, "runs", 0, "runs: how many runs the file holds, numbered 1 to this")
	now := flags.String("now", "", "paint: the UTC midnight the history the generator wrote ends at, RFC3339, which the paint's windows end on (job.sh's EVALBENCH_NOW); the current UTC midnight where unset")
	flags.StringVar(&o.node, "node", "", "the node the run is on, which the lines of k16's fallback and move name (run.sh passes EVALBENCH_NODE)")
	flags.StringVar(&o.githubLog, "github-log", "", "paint: the log of the GitHub stand-in the server ran against (ghstub), whose requests inside the paint's window are FAIL lines (check k17); none where the server ran with no stand-in")
	if err := flags.Parse(os.Args[2:]); err != nil {
		os.Exit(2)
	}
	o.now = time.Now().UTC().Truncate(24 * time.Hour)
	if *now != "" {
		at, err := time.Parse(time.RFC3339, *now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "measure: -now is an RFC3339 time, not %q: %v\n", *now, err)
			os.Exit(2)
		}
		o.now = at.UTC().Truncate(24 * time.Hour)
	}
	if o.mode != "head" && o.mode != "base" && o.mode != "failure-day" {
		fmt.Fprintf(os.Stderr, "measure: -mode is head, failure-day or base, not %q\n", o.mode)
		os.Exit(2)
	}
	if command != "runs" && (o.database == "" || o.url == "") {
		fmt.Fprintln(os.Stderr, usage+": name the database with -database and the server with -url")
		os.Exit(2)
	}
	r := &report{prefix: "RESULT"}
	if o.info {
		r.prefix = "INFO"
	}
	if command == "paint" && o.run > 0 {
		r.tag = fmt.Sprintf(" run=%d", o.run)
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
		if o.runsFile != "" && o.run < 1 {
			fmt.Fprintln(os.Stderr, "measure: paint with -runs-file needs -run, the run's number from 1")
			os.Exit(2)
		}
		err = paint(ctx, r, o)
	case "runs":
		if o.runsFile == "" || o.runs < 1 {
			fmt.Fprintln(os.Stderr, "measure: runs needs -runs-file and -runs, at least 1")
			os.Exit(2)
		}
		var runs []paintRun
		if runs, err = readRuns(o.runsFile); err == nil {
			judgeRuns(r, runs, o.runs)
		}
	}
	if err != nil {
		r.fail("%s: %v", command, err)
	}
	if len(r.fails) > 0 {
		if o.info {
			fmt.Printf("%s%s %s: %d INFO-FAIL lines, for information\n", r.prefix, r.tag, command, len(r.fails))
			return
		}
		fmt.Printf("%s%s %s: %d FAIL lines\n", r.prefix, r.tag, command, len(r.fails))
		os.Exit(1)
	}
	fmt.Printf("%s%s %s: no FAIL line\n", r.prefix, r.tag, command)
}
