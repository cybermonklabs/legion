package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/inboxcompare"
	"github.com/sjawhar/envoy/internal/dispatch/routes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// inboxCompare is `envoy-dispatch inbox-compare --old-url <base server> --new-url <this server>
// [--exclude <login>]...`, dispatch://INFRA-1152's comparison of the one read with the three reads
// it replaces (inboxcompare.Compare). It reads the people from DATABASE_URL, the database both
// servers serve, signs each in on both through dev sign-in, asks the three old reads on the old
// URL and the one read on the new one, prints one PASS: or FAIL: line per assertion and stores the
// run's row in inbox_comparison_runs, which GET /api/v1/inbox/comparison answers.
//
// It runs in the branch alone, against servers on this machine: an address whose host is not a
// loopback name or literal is refused before anything is read, and so is a server that answers no
// dev sign-in, which a deployment never serves. It migrates nothing: the database is the one the
// new URL's server migrated.
//
// The exit convention is the census's: 0 when the run meets the gate, 1 when it does not (every
// reason is in the report), 2 when it could not run and stored no row (errOut says why).
func inboxCompare(ctx context.Context, args []string, databaseURL string, out, errOut io.Writer) int {
	flags := flag.NewFlagSet("inbox-compare", flag.ContinueOnError)
	flags.SetOutput(errOut)
	oldURL := flags.String("old-url", "", "the build of the release before, serving the three old reads, with dev sign-in")
	newURL := flags.String("new-url", "", "this build, serving GET /api/v1/me/inbox, with dev sign-in, over the same database")
	var exclude []string
	flags.Func("exclude", "a login the run does not reach, counted as uncovered; repeatable", func(login string) error {
		exclude = append(exclude, login)
		return nil
	})
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(errOut, "inbox-compare: unexpected argument %q; the flags are --old-url, --new-url and --exclude\n", flags.Arg(0))
		return 2
	}
	for _, given := range []struct{ flag, url string }{{"--old-url", *oldURL}, {"--new-url", *newURL}} {
		if !routes.LoopbackURL(strings.TrimSpace(given.url)) {
			fmt.Fprintf(errOut, "inbox-compare: %s %q names no server on this machine; the comparison reads loopback servers alone\n",
				given.flag, given.url)
			return 2
		}
	}
	if strings.TrimSpace(databaseURL) == "" {
		fmt.Fprintln(errOut, "inbox-compare: DATABASE_URL is required: the database both servers serve")
		return 2
	}
	database, err := store.Open(ctx, databaseURL)
	if err != nil {
		fmt.Fprintf(errOut, "inbox-compare: open database: %v\n", err)
		return 2
	}
	defer database.Pool.Close()
	comparison, err := inboxcompare.Compare(ctx, database.Pool, inboxcompare.Options{OldURL: *oldURL, NewURL: *newURL, Exclude: exclude})
	if err != nil {
		if inboxcompare.IsRefusal(err) {
			fmt.Fprintf(errOut, "inbox-compare: refused: %v\n", err)
		} else {
			fmt.Fprintf(errOut, "inbox-compare: %v\n", err)
		}
		return 2
	}
	comparison.Write(out)
	if !comparison.Run.Met() {
		return 1
	}
	return 0
}
