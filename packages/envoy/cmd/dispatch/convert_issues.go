package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// openProjectRun reads the flags of a run by hand over the issues of one project,
// `envoy-dispatch <name> --project <KEY> [--dry-run]`, projectHelp and dryRunHelp the help of its
// two flags, and opens the database as the run takes it: migrated, as every database subcommand
// does (openMigrated), or for --dry-run with no migration applied and refused while one is owed
// (openUnmigrated). It answers the database, the project's key and whether the run is dry, or the
// exit code of a run that ends here: 2 on a usage error or a --project the projects table holds no
// row of under that exact key, 1 where the database does not open or the project cannot be read.
// The caller closes the database it answers.
func openProjectRun(ctx context.Context, name, projectHelp, dryRunHelp string, args []string, env settingValues, stdout, stderr io.Writer) (database *store.Store, key string, dryRun bool, code int) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	project := flags.String("project", "", projectHelp)
	dry := flags.Bool("dry-run", false, dryRunHelp)
	if err := flags.Parse(args); err != nil {
		return nil, "", false, 2
	}
	key = strings.TrimSpace(*project)
	if flags.NArg() > 0 || key == "" {
		fmt.Fprintf(stderr, "%s: usage: envoy-dispatch %s --project <KEY> [--dry-run]\n", name, name)
		return nil, "", false, 2
	}
	open := openMigrated
	if *dry {
		open = openUnmigrated
	}
	database, ok := open(ctx, name, env.get("DATABASE_URL"), stdout)
	if !ok {
		return nil, "", false, 1
	}
	var known bool
	if err := database.Pool.QueryRow(ctx, `select exists(select 1 from projects where key = $1)`, key).Scan(&known); err != nil {
		database.Pool.Close()
		fmt.Fprintf(stdout, "%s: read the project %q: %v\n", name, key, err)
		return nil, "", false, 1
	}
	if !known {
		database.Pool.Close()
		fmt.Fprintf(stderr, "%s: the database holds no project %q; --project takes a project's key as the projects table holds it\n", name, key)
		return nil, "", false, 2
	}
	return database, key, *dry, 0
}

// convertIssues runs `envoy-dispatch convert-issues --project <KEY> [--dry-run]`: the conversion
// of each issue of one project into its issue room (R6 of the issue tree's spec), by hand, which
// the server's step (docs.IssueRoomStep) runs on its own once every live process announces the
// issue room. It runs the step's one function, docs.Service.ConvertIssue, over the project's
// issues by number, each in a transaction of its own, under the fleet gate of this binary's own
// commit (docs.BuildGate): it refuses while any live process serves another build, and unless the
// newest server_build_announcements row names buildCommit, the commit this binary was stamped
// with; an unstamped binary is refused. The gate runs once before the first issue, so a run is
// refused while another build is live even where every issue it reaches is passed over, and again
// inside each issue's transaction. A done issue, and one that holds its room already, is passed
// over; an issue one of whose documents still owes its block index is deferred and the run goes
// on to the next.
//
// --dry-run converts each issue in its transaction and rolls it back: it applies no migration and
// refuses a database with one of this build's migrations still to apply (openUnmigrated), and it
// prints the same lines.
//
// It prints the rollback cost first (docs.ConversionRollbackCost), then one line for each issue -
// converted, passed over with its reason, or deferred with its reason, each with the time it took
// and, where the conversion measured it, the item count of the room's first update - and one line
// for each issue whose transaction failed, then a line with the counts and the total time. It
// exits 1 on a refusal, a failed issue or any other error, 2 on a usage error or a --project the
// projects table holds no row of, and 0 otherwise.
func convertIssues(ctx context.Context, args []string, env settingValues, stdout, stderr io.Writer) int {
	const name = "convert-issues"
	database, key, dryRun, code := openProjectRun(ctx, name,
		"the key of the project whose issues the conversion runs over (required)",
		"convert each issue in its transaction and roll it back, writing nothing; refused while a migration is owed",
		args, env, stdout, stderr)
	if code != 0 {
		return code
	}
	defer database.Pool.Close()
	documents := docs.New(docs.Deps{Store: database, Events: events.NewBroker(), ServerURL: loadServerURL(env), BuildCommit: buildCommit})
	defer documents.Shutdown(context.Background())
	fmt.Fprintf(stdout, "%s: %s\n", name, docs.ConversionRollbackCost)
	gate := docs.BuildGate(buildCommit)
	if err := documents.CheckConversionGate(ctx, gate); err != nil {
		fmt.Fprintf(stdout, "%s: %v\n", name, err)
		return 1
	}
	began := time.Now()
	counts := map[docs.ConversionOutcome]int{}
	failed := 0
	err := documents.ConvertProject(ctx, key, gate, dryRun, func(issue docs.ConvertedIssue, err error) {
		if err != nil {
			failed++
			fmt.Fprintf(stdout, "%s failed in %s: %v\n", issue.Issue, issue.Took.Round(time.Microsecond), err)
			return
		}
		counts[issue.Outcome]++
		writeConvertedIssue(stdout, issue)
	})
	fmt.Fprintf(stdout, "%s: project=%s converted=%d passed_over=%d deferred=%d failed=%d total=%s dry_run=%t\n",
		name, key, counts[docs.IssueConverted], counts[docs.IssuePassedOver], counts[docs.IssueDeferred], failed,
		time.Since(began).Round(time.Microsecond), dryRun)
	if err != nil {
		fmt.Fprintf(stdout, "%s: %v\n", name, err)
		return 1
	}
	if failed > 0 {
		return 1
	}
	return 0
}

// writeConvertedIssue prints the one line of what the conversion did with issue: converted (or,
// in a dry run, would be, its transaction rolled back), passed over or deferred, with the time it
// took, its reason, and the item count of the room's first update where the conversion measured
// it. A line of one issue opens with the issue's key and carries no word of the command's own
// name, so a reader of the run tells the issue's outcome from the words that follow its key.
func writeConvertedIssue(w io.Writer, issue docs.ConvertedIssue) {
	took := issue.Took.Round(time.Microsecond)
	items := ""
	if issue.Items > 0 {
		items = fmt.Sprintf(" items=%d", issue.Items)
	}
	switch issue.Outcome {
	case docs.IssueConverted:
		if issue.DryRun {
			fmt.Fprintf(w, "%s would be converted in %s, rolled back:%s\n", issue.Issue, took, items)
			return
		}
		fmt.Fprintf(w, "%s converted in %s:%s\n", issue.Issue, took, items)
	case docs.IssuePassedOver:
		fmt.Fprintf(w, "%s passed over in %s: %s%s\n", issue.Issue, took, issue.Reason, items)
	case docs.IssueDeferred:
		fmt.Fprintf(w, "%s deferred in %s: %s\n", issue.Issue, took, issue.Reason)
	}
}
