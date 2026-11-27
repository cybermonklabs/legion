// Command measure is the instrument of the eval registry's measurement Job (dispatch://INFRA-2916,
// checks k2, k8, k13 and k16), run by evalbench/run.sh against a server booted on the evalbench
// fixture. It prints one RESULT line per measurement and a FAIL line for each one that misses its
// budget or its rule, and exits 1 when any did.
//
//	measure wait  -mode head -database <url> -url <server>
//	measure paint -mode head -database <url> -url <server> -token <token> \
//	  -server-log <the server's stderr> -pg-log <Postgres's jsonlog file> [-run <n> -runs-file <file>]
//	measure runs  -runs <n> -runs-file <file>
//	measure k14   -mode k14 -database <url> -url <server> -token <token> -server-log <file> -pg-log <file> \
//	  -pg-log-from <offset> -github-log <file> -expect-file <file>
//	measure k14-paints -mode k14 -runs-file <file>
//	measure github -listen 127.0.0.1:<port> -key-out <file> -client-id <id> -owners <owner> -log <file>
//
// Mode k14 is k14's (dispatch://INFRA-978, T19.9) over the k14 load (evalbench's Spec.K14): wait
// waits for the production signals' loop too; k14 makes and times k14's three writes with pages
// open (k14.go); paint -project <key> measures a k14 project's first paint (k14_paint.go), whose
// database budgets fail at ten times the stated rate too, which R14 binds there; k14-paints judges
// the paints' runs; github is the loopback stand-in for GitHub the server's App calls in that mode
// (github.go).
//
// -mode is head, base, failure-day, base-failure-day or k14 (above): failure-day is this head over
// the fixture with a failure day added to it (evalbench.AddFailureDay), whose paint is the Delivery
// timeline's alone; base is the base, main at the commit this head merges (git merge-base <head>
// origin/main), and base-failure-day the base over the same history with the failure day added.
//
// wait polls the database until the server's consumers have caught up with the fixture: the
// Legion facts consumer has read every project whole under the running build and passed every
// feed's head, and (this head) the day-row refresher has passed its feeds' heads, done every walk,
// served every family's rows under the running build (eval_days_serving) with no rebuild building
// and built every project's stored figures through today's last end day, and the keyword fill's
// cursor stands at the newest event, which evalbench's load leaves it at as of the load, so it has
// only the server's own later events to follow (consumerState.caughtUp). At the base it waits for
// those of the facts consumer and the fill the base's catalog holds the tables of. Where the
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
// view of the compare range, and the timeline view of the 28-day window, which no budget judges
// (baseCases), with k8's negative control and the base statements k13's control names; in mode
// base-failure-day, the same reads over the history with the failure day added, with the failure
// day the database holds.
// Every Delivery read's answer is printed as the history it shows: its pull requests, those
// attributed to an issue, by the issue's project, and the measures' per-PR total and deploys; the
// base's control fails on a page that shows no pull request attributed to an issue, or none in its
// measures. -info measures the first paint alone and judges no budget, for the round after a
// Postgres restart that the Job records for information: its lines are INFO lines, a rule it
// misses is an INFO-FAIL line, which no verdict counts, and it exits 0. -ten-times says the fixture
// is at ten times the stated run rate. There a database time over its budget (150 ms at 84 days
// with a compare range, 60 ms at 28 days, a read's, a group's or a run's), which the root's R14
// and k8 state at the stated rate, and k8's base control, prints a RECORD line, which no verdict
// counts, with the budget beside the time: at ten times the database time is judged by the head's
// reads set beside the base's. The first paint's limit (the server time of 0.4 s and the body of
// 300 KB compressed), which R14 holds at ten times too, the stale cuts' reads, which the
// architect's ruling gates at ten times (judgeStaleReads), and every rule that is not a budget
// still fail.
//
// -run n tags each RESULT and FAIL line of the paint with run=n, one of the first paint's runs
// (k8: run.sh paints this head's first paint EVALBENCH_PAINT_RUNS times, 8 by default, each as the
// first requests after a fresh start of the server, and the base's as many times as it is asked
// for, once by default), and -runs-file appends the run's reads to a
// file. k2's statements and controls, k13's named-jobs read and P0 list, k16's day rows by
// repository and hand folds, the probe, k16's fallback and its move of the default repository are
// measured in run 1 alone, after its paint; the reads of every run are judged by their budgets,
// and by k8's and k13's rules over their plans. runs then prints, for each read and each group of
// reads, its database time in each run with the median and the maximum of the runs, and fails one
// over its budget in any run (runs.go). -now is the day the paint's windows end on, the end of the
// history the generator wrote (job.sh's EVALBENCH_NOW), and -node the node the run is on (run.sh
// passes the Job's EVALBENCH_NODE), which the lines of k16's fallback and move name.
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
	"slices"
	"strings"
	"time"
)

// report collects what one invocation measured: each line it printed, the text of each FAIL line
// (fails) and of each RECORD line (records). tag, where it is set, follows the prefix of every
// RESULT, FAIL and RECORD line: " run=3" for the third of the first paint's runs. An -info
// invocation's prefix is INFO, and it writes a rule it misses as an INFO-FAIL line, which no
// verdict counts (job.yaml's reads `^FAIL`). tenTimes is -ten-times: the fixture is at ten times
// the stated run rate, where no check gates the database-time budgets (overStated).
type report struct {
	prefix   string
	tag      string
	tenTimes bool
	fails    []string
	records  []string
}

// result prints one measurement.
func (r *report) result(format string, args ...any) {
	fmt.Printf(r.prefix+r.tag+" "+format+"\n", args...)
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

// overStated prints one database time over its budget, which the root's R14 and k8 state at the
// stated run rate, the line naming the budget beside the time: a FAIL line, or, at ten times the
// stated run rate (tenTimes), a RECORD line, which no verdict counts and which leaves the run
// passed, since there the head's database time is judged beside the base's, not by the budget.
func (r *report) overStated(format string, args ...any) {
	if !r.tenTimes {
		r.fail(format, args...)
		return
	}
	line := fmt.Sprintf(format, args...)
	r.records = append(r.records, line)
	fmt.Printf("RECORD%s %s\n", r.tag, line)
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
	mode       string
	database   string
	url        string
	token      string
	serverLog  string
	pgLog      string
	pgLogFrom  int64
	githubLog  string
	expectFile string
	project    string
	quiet      time.Duration
	catalog    time.Duration
	timeout    time.Duration
	info       bool
	run        int
	runs       int
	runsFile   string
	now        time.Time
	node       string
	scale      string
}

const usage = "usage: measure wait|paint|runs|k14|k14-paints|github -mode head|failure-day|base|base-failure-day|k14 -database <url> -url <server> [flags]"

// commands is every subcommand measure runs.
var commands = []string{"wait", "paint", "runs", "k14", "k14-paints", "github"}

func main() {
	if len(os.Args) < 2 || !slices.Contains(commands, os.Args[1]) {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	command := os.Args[1]
	flags := flag.NewFlagSet(command, flag.ExitOnError)
	var o options
	flags.StringVar(&o.mode, "mode", "head", "head, this head's first paint; failure-day, this head's timeline once a failure day is added; base, the first paint of the base, main at the commit this head merges; base-failure-day, the base's over the history with a failure day added; or k14, k14's writes and first paints over the k14 load")
	flags.StringVar(&o.database, "database", "", "the Postgres URL the server reads, as a superuser (required): the instrument resets pg_stat_statements and sets the statement log")
	flags.StringVar(&o.url, "url", "", "the server's base URL")
	flags.StringVar(&o.token, "token", "", "the server's agent token (DISPATCH_AGENT_TOKEN)")
	flags.StringVar(&o.serverLog, "server-log", "", "the file the server's stderr is written to, which holds its access log")
	flags.StringVar(&o.pgLog, "pg-log", "", "Postgres's jsonlog file, which holds each statement with its parameters")
	flags.StringVar(&o.project, "project", "INFRA", "the project the first paint reads")
	flags.Int64Var(&o.pgLogFrom, "pg-log-from", 0, "k14: the statement log's size when the server started, where its daily failure write is read from")
	flags.StringVar(&o.githubLog, "github-log", "", "k14: the log of the GitHub stand-in (measure github), whose requests k14 counts")
	flags.StringVar(&o.expectFile, "expect-file", "", "k14: the file the warm reads of the first paints are written to; paint -mode k14: the file they are read from")
	flags.StringVar(&o.scale, "scale", "", "the share of INFRA's stated rate the fixture is loaded at (job.sh's EVALBENCH_SCALE), which k14's lines name")
	listen := flags.String("listen", "", "github: the loopback address the stand-in serves")
	keyOut := flags.String("key-out", "", "github: the file the stand-in writes the App's throwaway private key to")
	clientID := flags.String("client-id", "", "github: the App's client id, the issuer of its JWT")
	owners := flags.String("owners", "", "github: the owners the App is installed on, comma-separated")
	githubLog := flags.String("log", "", "github: the file each request is logged to")
	flags.DurationVar(&o.quiet, "quiet", 30*time.Second, "how long after the restart the first window starts, so the boot's own passes have ended")
	flags.DurationVar(&o.catalog, "catalog", 65*time.Second, "how long the background catalogue is taken over: more than a minute, so every loop a quiet server runs on a minute's pace or on the Legion sampler's minute wake runs in it")
	flags.DurationVar(&o.timeout, "timeout", time.Hour, "how long wait polls before it fails")
	flags.BoolVar(&o.info, "info", false, "measure the first paint alone and judge no budget")
	flags.IntVar(&o.run, "run", 0, "paint: which of the first paint's runs this is, from 1, which tags its lines run=<n>; the checks after the paint are run 1's; 0 for a paint measured once")
	flags.StringVar(&o.runsFile, "runs-file", "", "paint: the file each run's reads are appended to; runs: the file it reads them from")
	flags.IntVar(&o.runs, "runs", 0, "runs: how many runs the file holds, numbered 1 to this")
	now := flags.String("now", "", "paint: the UTC midnight the history the generator wrote ends at, RFC3339, which the paint's windows end on (job.sh's EVALBENCH_NOW); the current UTC midnight where unset")
	tenTimes := flags.Bool("ten-times", false, "the fixture is at ten times the stated run rate, where no check gates the database-time budgets stated at the stated rate: a read, a group or a run over one, and k8's base control, print a RECORD line, not a FAIL line")
	flags.StringVar(&o.node, "node", "", "the node the run is on (run.sh passes the Job's EVALBENCH_NODE), which the lines of k16's fallback and move name; unnamed where empty")
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
	if o.mode != "head" && o.mode != "failure-day" && o.mode != "k14" && !atBase(o.mode) {
		fmt.Fprintf(os.Stderr, "measure: -mode is head, failure-day, base, base-failure-day or k14, not %q\n", o.mode)
		os.Exit(2)
	}
	if command == "github" {
		if err := serveGitHub(*listen, *keyOut, *clientID, *owners, *githubLog); err != nil {
			fmt.Fprintf(os.Stderr, "measure github: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if (command == "k14" || command == "k14-paints") && o.mode != "k14" {
		fmt.Fprintf(os.Stderr, "measure: %s is mode k14's, not %q's\n", command, o.mode)
		os.Exit(2)
	}
	if command != "runs" && command != "k14-paints" && (o.database == "" || o.url == "") {
		fmt.Fprintln(os.Stderr, usage+": name the database with -database and the server with -url")
		os.Exit(2)
	}
	r := &report{prefix: "RESULT", tenTimes: *tenTimes}
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
	case "k14":
		if o.token == "" || o.serverLog == "" || o.pgLog == "" {
			fmt.Fprintln(os.Stderr, "measure: k14 needs -token, -server-log and -pg-log")
			os.Exit(2)
		}
		err = k14(ctx, r, o)
	case "k14-paints":
		if o.runsFile == "" {
			fmt.Fprintln(os.Stderr, "measure: k14-paints needs -runs-file")
			os.Exit(2)
		}
		err = k14Paints(r, o)
	}
	if err != nil {
		r.fail("%s: %v", command, err)
	}
	recorded := ""
	if len(r.records) > 0 {
		recorded = fmt.Sprintf(", %d RECORD lines of budgets no check gates at ten times the stated run rate", len(r.records))
	}
	if len(r.fails) > 0 {
		if o.info {
			fmt.Printf("%s%s %s: %d INFO-FAIL lines, for information\n", r.prefix, r.tag, command, len(r.fails))
			return
		}
		fmt.Printf("%s%s %s: %d FAIL lines%s\n", r.prefix, r.tag, command, len(r.fails), recorded)
		os.Exit(1)
	}
	fmt.Printf("%s%s %s: no FAIL line%s\n", r.prefix, r.tag, command, recorded)
}
