package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pmezard/go-difflib/difflib"

	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/config"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/files"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/schemanodes"
	"github.com/sjawhar/envoy/internal/dispatch/processes"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// subcommand is an argument envoy-dispatch takes in place of serving. run gets the arguments
// after the name and what the settings table read.
type subcommand struct {
	name string
	run  func(ctx context.Context, args []string, env settingValues, stdout, stderr io.Writer) int
}

// subcommands are every argument envoy-dispatch takes in place of serving: runSubcommand both
// dispatches on this table and names it, in this order, when it refuses an argument.
var subcommands = []subcommand{
	{"backfill-block-ids", func(ctx context.Context, _ []string, env settingValues, stdout, _ io.Writer) int {
		return backfillBlockIDs(ctx, env.get("DATABASE_URL"), stdout)
	}},
	{"backfill-anchor-blocks", func(ctx context.Context, _ []string, env settingValues, stdout, _ io.Writer) int {
		return backfillAnchorBlocks(ctx, env.get("DATABASE_URL"), stdout)
	}},
	{"backfill-graph-store", func(ctx context.Context, _ []string, env settingValues, stdout, _ io.Writer) int {
		return backfillGraphStore(ctx, env.get("DATABASE_URL"), stdout)
	}},
	// kept for one release: decisions-step
	{"restore-answered-asks", func(ctx context.Context, args []string, env settingValues, stdout, stderr io.Writer) int {
		return runDecisionsStep(ctx, "restore-answered-asks", false, args, env, stdout, stderr)
	}},
	// kept for one release: decisions-step
	{"convert-decisions", func(ctx context.Context, args []string, env settingValues, stdout, stderr io.Writer) int {
		return runDecisionsStep(ctx, "convert-decisions", true, args, env, stdout, stderr)
	}},
	{"rebuild-refs", func(ctx context.Context, _ []string, env settingValues, stdout, _ io.Writer) int {
		return rebuildRefs(ctx, env.get("DATABASE_URL"), loadServerURL(env), stdout)
	}},
	{"redeliver-webhooks", func(ctx context.Context, args []string, env settingValues, stdout, _ io.Writer) int {
		return redeliverWebhooks(ctx, args, env, stdout)
	}},
	{"census", func(ctx context.Context, args []string, env settingValues, stdout, stderr io.Writer) int {
		return census(ctx, args, env.get("DATABASE_URL"), stdout, stderr)
	}},
	{"versions", func(ctx context.Context, _ []string, env settingValues, stdout, stderr io.Writer) int {
		return versions(ctx, env.get("DATABASE_URL"), stdout, stderr)
	}},
	{"backfill-files", func(ctx context.Context, args []string, env settingValues, stdout, _ io.Writer) int {
		return backfillFiles(ctx, args, env.get("DATABASE_URL"), strings.TrimSpace(env.get("DISPATCH_FILE_STORE_BUCKET")), stdout)
	}},
	{"stored-order", func(ctx context.Context, args []string, env settingValues, stdout, _ io.Writer) int {
		return storedOrder(ctx, args, env.get("DATABASE_URL"), stdout)
	}},
	{"project-owner", func(ctx context.Context, args []string, env settingValues, stdout, stderr io.Writer) int {
		return giveProject(ctx, args, env.get("DATABASE_URL"), stdout, stderr)
	}},
	{"settings", func(_ context.Context, _ []string, _ settingValues, stdout, stderr io.Writer) int {
		if err := writeSettings(stdout); err != nil {
			fmt.Fprintf(stderr, "settings: %v\n", err)
			return 1
		}
		return 0
	}},
	{"routes", func(_ context.Context, _ []string, _ settingValues, stdout, stderr io.Writer) int {
		if err := api.WriteRouteIndex(stdout); err != nil {
			fmt.Fprintf(stderr, "routes: %v\n", err)
			return 1
		}
		return 0
	}},
	{"openapi", func(_ context.Context, args []string, _ settingValues, stdout, stderr io.Writer) int {
		return writeOpenAPIDocument(args, stdout, stderr)
	}},
}

// runSubcommand runs the subcommand args name and returns its exit code. A name it does not know
// is refused with exit 2 rather than falling through to the server: the server migrates the
// database at boot, so a deployment running `envoy-dispatch census` on an image that predates
// the subcommand must get a refusal, never a boot that applies the migrations the census was to
// inspect.
func runSubcommand(ctx context.Context, args []string, env settingValues, stdout, stderr io.Writer) int {
	names := make([]string, len(subcommands))
	for i, sub := range subcommands {
		if sub.name == args[0] {
			return sub.run(ctx, args[1:], env, stdout, stderr)
		}
		names[i] = sub.name
	}
	fmt.Fprintf(stderr, "envoy-dispatch: unknown subcommand %q; the subcommands are %s, and envoy-dispatch with no argument serves\n", args[0], strings.Join(names, ", "))
	return 2
}

// writeOpenAPIDocument runs `envoy-dispatch openapi`. With no flag it writes the OpenAPI document
// of the route table to stdout; --out <path> writes it there; --check writes nothing at all — it
// renders the document to memory and holds the committed artifact to it, printing a unified diff
// and then the line that names the file stale and the command that fixes it, the convention
// packages/contracts/scripts/gen-go.ts follows for every file it generates, and exiting 1.
func writeOpenAPIDocument(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("openapi", flag.ContinueOnError)
	flags.SetOutput(stderr)
	out := flags.String("out", "", "write the document to this path instead of stdout; the file --check compares")
	check := flags.Bool("check", false, "compare the committed document with a fresh render, write nothing, and exit 1 on a difference")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "openapi: unexpected argument %q; the flags are --out and --check\n", flags.Arg(0))
		return 2
	}
	var document bytes.Buffer
	if err := api.WriteOpenAPI(&document); err != nil {
		fmt.Fprintf(stderr, "openapi: %v\n", err)
		return 1
	}
	switch {
	case *check:
		path := *out
		if path == "" {
			found, err := committedOpenAPIPath()
			if err != nil {
				fmt.Fprintf(stderr, "openapi: %v\n", err)
				return 1
			}
			path = found
		}
		committed, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(stderr, "openapi: %v\n", err)
			return 1
		}
		if bytes.Equal(committed, document.Bytes()) {
			return 0
		}
		diff, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
			A:        difflib.SplitLines(string(committed)),
			B:        difflib.SplitLines(document.String()),
			FromFile: path,
			ToFile:   "envoy-dispatch openapi",
			Context:  3,
		})
		if err != nil {
			fmt.Fprintf(stderr, "openapi: diff the committed document: %v\n", err)
			return 1
		}
		fmt.Fprint(stdout, diff)
		fmt.Fprintf(stderr, "%s is stale: run `envoy-dispatch openapi --out %s` and commit the result.\n", path, path)
		return 1
	case *out != "":
		if err := os.WriteFile(*out, document.Bytes(), 0o644); err != nil {
			fmt.Fprintf(stderr, "openapi: %v\n", err)
			return 1
		}
		return 0
	default:
		if _, err := stdout.Write(document.Bytes()); err != nil {
			fmt.Fprintf(stderr, "openapi: %v\n", err)
			return 1
		}
		return 0
	}
}

// committedOpenAPIPath finds the committed document by walking up from the working directory, so
// `openapi --check` reads the same file whether it is run from the repository root or from a
// package in it.
func committedOpenAPIPath() (string, error) {
	directory, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("read the working directory: %w", err)
	}
	for {
		candidate := filepath.Join(directory, filepath.FromSlash(api.OpenAPIDocumentPath))
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", fmt.Errorf("no %s in any directory above the working directory: run it "+
				"inside the repository, or name the file with --out", api.OpenAPIDocumentPath)
		}
		directory = parent
	}
}

// openMigrated opens the database a document subcommand works on, brings it to the current
// schema (openMigratedWithoutBlockSchema) and installs the block schema the store's type nodes
// build (schemanodes.InstallFromStore), as the server's boot does beside its own migration run,
// so a subcommand that parses, renders or writes a document holds it to the store's types rather
// than to the file this binary embeds. It reports each failure on out under the subcommand's
// label. The caller closes the returned store's pool. The install opens one of the pool's
// transactions, so its context is marked for the pool's nested-acquire guard
// (store.WithTransactionTracking), as every entry point of the pool's transactions marks its own.
func openMigrated(ctx context.Context, label, databaseURL string, out io.Writer) (*store.Store, bool) {
	database, ok := openMigratedWithoutBlockSchema(ctx, label, databaseURL, out)
	if !ok {
		return nil, false
	}
	if err := schemanodes.InstallFromStore(store.WithTransactionTracking(ctx), database.Pool); err != nil {
		fmt.Fprintf(out, "%s: install the block schema from the store: %v\n", label, err)
		database.Pool.Close()
		return nil, false
	}
	return database, true
}

// openMigratedWithoutBlockSchema opens the database a DB subcommand works on and brings it to the
// current schema, and installs no block schema. backfill-files opens its database here: it moves
// file bytes and parses no document, so the store's types are nothing it reads, and a store whose
// types this build cannot install is no reason for it to refuse. It reports each failure on out
// under the subcommand's label. The caller closes the returned store's pool.
func openMigratedWithoutBlockSchema(ctx context.Context, label, databaseURL string, out io.Writer) (*store.Store, bool) {
	if strings.TrimSpace(databaseURL) == "" {
		fmt.Fprintln(out, label+": DATABASE_URL is required")
		return nil, false
	}
	database, err := store.Open(ctx, databaseURL)
	if err != nil {
		fmt.Fprintf(out, "%s: open database: %v\n", label, err)
		return nil, false
	}
	if err := database.Migrate(ctx); err != nil {
		fmt.Fprintf(out, "%s: migrate database: %v\n", label, err)
		database.Pool.Close()
		return nil, false
	}
	return database, true
}

// openUnmigrated opens the database a dry run works on without writing to it: where this
// binary's migrations are not all applied (store.Census, which migrates nothing and reads in a
// read-only transaction), it refuses on out under the subcommand's label, since applying them is a
// write; otherwise it opens the store and installs its block schema, as openMigrated does after
// its migration run. The caller closes the returned store's pool.
//
// kept for one release: decisions-step
func openUnmigrated(ctx context.Context, label, databaseURL string, out io.Writer) (*store.Store, bool) {
	if strings.TrimSpace(databaseURL) == "" {
		fmt.Fprintln(out, label+": DATABASE_URL is required")
		return nil, false
	}
	report, err := store.Census(ctx, databaseURL)
	if err != nil {
		fmt.Fprintf(out, "%s: read which migrations the database has still to apply: %v\n", label, err)
		return nil, false
	}
	if report.Fresh || len(report.Pending) > 0 {
		owed := "every migration of this build"
		if !report.Fresh {
			names := make([]string, len(report.Pending))
			for index, entry := range report.Pending {
				names[index] = entry.Migration.Name
			}
			owed = strings.Join(names, ", ")
		}
		fmt.Fprintf(out, "%s: --dry-run writes nothing, and the database has %s still to apply; run %s without --dry-run, or boot a server of this build, first\n",
			label, owed, label)
		return nil, false
	}
	database, err := store.Open(ctx, databaseURL)
	if err != nil {
		fmt.Fprintf(out, "%s: open database: %v\n", label, err)
		return nil, false
	}
	if err := schemanodes.InstallFromStore(store.WithTransactionTracking(ctx), database.Pool); err != nil {
		fmt.Fprintf(out, "%s: install the block schema from the store: %v\n", label, err)
		database.Pool.Close()
		return nil, false
	}
	return database, true
}

// backfillBlockIDs runs `envoy-dispatch backfill-block-ids`: it prints one line for each document
// it read (writeBlockIDBackfillReport), and where it read none, one line saying so (`documents=0`),
// so a run that owed nothing reads apart from one that printed nothing.
func backfillBlockIDs(ctx context.Context, databaseURL string, out io.Writer) int {
	database, ok := openMigrated(ctx, "backfill-block-ids", databaseURL, out)
	if !ok {
		return 1
	}
	defer database.Pool.Close()
	service := docs.New(docs.Deps{Store: database, Events: events.NewBroker()})
	defer service.Shutdown(context.Background())
	reports, err := service.BackfillBlockIDs(ctx)
	if err != nil {
		fmt.Fprintf(out, "backfill-block-ids: %v\n", err)
		return 1
	}
	if len(reports) == 0 {
		fmt.Fprintln(out, "backfill-block-ids: documents=0")
	}
	exitCode := 0
	for _, report := range reports {
		if !writeBlockIDBackfillReport(out, report) {
			exitCode = 1
		}
	}
	return exitCode
}

// backfillGraphStore runs `envoy-dispatch backfill-graph-store`: it writes the nodes and
// tuples of every comment, document block and block reference written before the node and
// tuple store existed, the node of every issue, document, ask and message written before its
// kind's writer wrote one, the project of every comment and block node written before a node
// named its project, the asks and followers written before the store held them, and every kind
// the one inbox moves but the mention tuples for the rows a build before it wrote - the
// messages, the comment pointers, an item's blocks, links and citations, the delivery attempts,
// what an ask, an answer and a decline owe, and the six read-state tables - in batches, one
// transaction each (graphcensus.Backfill). It appends no event of its own: a fact whose row's own
// event cannot be found carries the store's sentinel as its cause. It is safe to run again: a row
// that already carries its marker is left alone, so a run interrupted anywhere carries on from
// where it stopped, and the run exits 1 while any kind still owes a row.
//
// A block's parent is left to the block index: the run marks each document it touched owed
// one, and `census --store` reads a difference on every backfilled block until that
// reconciliation has drained the mark, since the reconciliation is the one writer of
// document_blocks.parent_block_id.
func backfillGraphStore(ctx context.Context, databaseURL string, out io.Writer) int {
	database, ok := openMigrated(ctx, "backfill-graph-store", databaseURL, out)
	if !ok {
		return 1
	}
	defer database.Pool.Close()
	// Each batch is one of the pool's transactions, marked for its nested-acquire guard as the
	// decisions step marks the same batches under its boot and its tick.
	report, err := graphcensus.Backfill(store.WithTransactionTracking(ctx), database, out)
	if err != nil {
		fmt.Fprintf(out, "backfill-graph-store: %v\n", err)
		return 1
	}
	for _, kind := range report.Kinds {
		if kind.Skipped > 0 || kind.Remaining > 0 {
			return 1
		}
	}
	return 0
}

// runDecisionsStep runs `envoy-dispatch restore-answered-asks --project <KEY> [--dry-run]` (name,
// convert false) and `envoy-dispatch convert-decisions --project <KEY> [--dry-run]` (convert
// true): the decisions step by hand (docs.DecisionStep.RunByHand) over the issues of one project.
// Each migrates the database as every database subcommand does (openMigrated), waits for the
// step's lock, runs the step's first two phases - the backfill of the rows the release before
// wrote and the catch-up of what another build appended - and then its third phase for the
// project, whatever DISPATCH_TEST_HOLD_DECISIONS_STEP says, and only on a fleet of this binary's
// own build (buildCommit), as a server's step compares its own commit: the restore of every
// answered ask that left an issue's spec alone, or, for convert-decisions, the rewrite of the
// project's gated steps without `decisions`, that restore, and the conversion of each issue's
// decisions document into the spec, which retires the document once it holds no section.
//
// --dry-run writes nothing: it applies no migration and refuses, with exit 1, a database that
// has one of this build's migrations still to apply (openUnmigrated), refuses one whose rows the
// step's first phase has still to state (docs.ErrDryRunOwesBackfill), runs no catch-up, and runs
// the third phase with the rewrite of the gated steps and each issue's transaction rolled back.
//
// It prints one line for each issue it touched (writeDecisionIssue), one for each issue it left
// with nothing written because a document of it owes its block index, which the block-index
// reconciliation indexes before a later run takes the issue (`issue=… left: …`), and one for each
// issue whose transaction failed, and where it touched, left and failed none, one line saying so
// (`issues=0`), so a run that owed nothing reads apart from one that printed nothing, and a run
// that still owes an issue never says nothing was owed. It exits 1 on such a left issue, on such a
// failure, on another build live, or on any other error, 2 on a usage error or a --project the
// projects table holds no row of under that exact key, which it refuses before any phase runs, and
// 0 otherwise.
//
// kept for one release: decisions-step
func runDecisionsStep(ctx context.Context, name string, convert bool, args []string, env settingValues, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	project := flags.String("project", "", "the key of the project whose issues the step runs over (required)")
	dryRun := flags.Bool("dry-run", false, "print what the step would do and write nothing; refused while a migration or the step's first phase is owed")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 || strings.TrimSpace(*project) == "" {
		fmt.Fprintf(stderr, "%s: usage: envoy-dispatch %s --project <KEY> [--dry-run]\n", name, name)
		return 2
	}
	open := openMigrated
	if *dryRun {
		open = openUnmigrated
	}
	database, ok := open(ctx, name, env.get("DATABASE_URL"), stdout)
	if !ok {
		return 1
	}
	defer database.Pool.Close()
	key := strings.TrimSpace(*project)
	var known bool
	if err := database.Pool.QueryRow(ctx, `select exists(select 1 from projects where key = $1)`, key).Scan(&known); err != nil {
		fmt.Fprintf(stdout, "%s: read the project %q: %v\n", name, key, err)
		return 1
	}
	if !known {
		fmt.Fprintf(stderr, "%s: the database holds no project %q; --project takes a project's key as the projects table holds it\n", name, key)
		return 2
	}
	documents := docs.New(docs.Deps{Store: database, Events: events.NewBroker(), ServerURL: loadServerURL(env)})
	defer documents.Shutdown(context.Background())
	step := docs.NewDecisionStep(database, buildCommit, slog.New(slog.NewTextHandler(stderr, nil)))
	step.UseDocuments(documents)
	pass, err := step.RunByHand(ctx, key, convert, *dryRun)
	for _, issue := range pass.Done {
		writeDecisionIssue(stdout, name, issue, *dryRun)
	}
	for _, left := range pass.Left {
		fmt.Fprintf(stdout, "%s: issue=%s left: %v\n", name, left.Issue, left.Err)
	}
	for _, failure := range pass.Failed {
		fmt.Fprintf(stdout, "%s: issue=%s failed: %v\n", name, failure.Issue, failure.Err)
	}
	if err != nil {
		fmt.Fprintf(stdout, "%s: %v\n", name, err)
		return 1
	}
	if len(pass.Left) > 0 || len(pass.Failed) > 0 {
		return 1
	}
	if len(pass.Done) == 0 {
		fmt.Fprintf(stdout, "%s: issues=0 project=%s dry_run=%t\n", name, key, *dryRun)
	}
	return 0
}

// writeDecisionIssue prints the one line of what the decisions step did to issue: how many
// answered asks it restored, how many of them it moved out of the decisions document and how many
// took a new block id; how many it declined because a person removed their block, from the spec
// or, for an ask it would move, from the decisions document while the step ran, each as
// ask@person@v<version>; what became of the document's sections; whether it retired the
// document; the versions it wrote; what became of the spec's approval; and each approval it
// superseded, with the asks, as their nodes and their blocks, whose restore superseded it.
//
// kept for one release: decisions-step
func writeDecisionIssue(w io.Writer, name string, issue docs.DecisionIssue, dryRun bool) {
	moved, renamed := 0, 0
	for _, ask := range issue.Restored {
		if ask.From != "" {
			moved++
		}
		if ask.NewID {
			renamed++
		}
	}
	declined := make([]string, 0, len(issue.Declined))
	for _, ask := range issue.Declined {
		declined = append(declined, fmt.Sprintf("%s@%s@v%d", ask.Ask, ask.Person, ask.Version))
	}
	superseded := make([]string, 0, len(issue.Superseded))
	for _, approval := range issue.Superseded {
		superseded = append(superseded, fmt.Sprintf("%s/review=%s/approved_version=%d/asks=%s/nodes=%s",
			approval.Document, approval.Approval.ReviewID, approval.Approval.ApprovedVersion,
			strings.Join(approval.Asks, ","), strings.Join(approval.Nodes, ",")))
	}
	approval := issue.SpecApproval
	if approval == "" {
		approval = "none"
	}
	fmt.Fprintf(w, "%s: issue=%s restored=%d moved=%d new_ids=%d declined=%d declined_asks=%s decisions=%d readings=%d dropped=%d removed=%d left=%d retired=%t spec_version=%d decisions_version=%d other_versions=%d spec_approval=%s superseded=%s dry_run=%t\n",
		name, issue.Issue, len(issue.Restored), moved, renamed, len(issue.Declined), strings.Join(declined, ","),
		issue.Decisions, issue.Readings, issue.Dropped, issue.Removed, issue.Left,
		issue.Retired, issue.SpecVersion, issue.DecisionsVersion, issue.OtherVersions, approval, strings.Join(superseded, ";"), dryRun)
}

func backfillAnchorBlocks(ctx context.Context, databaseURL string, out io.Writer) int {
	database, ok := openMigrated(ctx, "backfill-anchor-blocks", databaseURL, out)
	if !ok {
		return 1
	}
	defer database.Pool.Close()
	service := docs.New(docs.Deps{Store: database, Events: events.NewBroker()})
	defer service.Shutdown(context.Background())
	reports, err := service.BackfillBlockIDs(ctx)
	if err != nil {
		fmt.Fprintf(out, "backfill-anchor-blocks: stamp document blocks: %v\n", err)
		return 1
	}
	for _, report := range reports {
		if report.Err != nil {
			fmt.Fprintf(out, "backfill-anchor-blocks: stamp document %s: %v\n", report.ArtifactID, report.Err)
			return 1
		}
	}
	result, err := service.BackfillAnchorBlocks(ctx)
	if err != nil {
		fmt.Fprintf(out, "backfill-anchor-blocks: %v\n", err)
		return 1
	}
	writeAnchorBlockBackfillReport(out, result)
	return 0
}

// backfillFiles moves every uploaded file's bytes from Postgres into the bucket
// DISPATCH_FILE_STORE_BUCKET names, opened by files.Open as the server opens it, so the two never
// read one value as two stores (files.BackfillRows); with --verify-only it reads back every
// file already moved and checks it against its row's hash, moving nothing (files.VerifyRows); with
// --restore, the rollback, it writes every moved file's bytes back into its row from the bucket
// (files.RestoreRows). Each pass names the rows it could not do and goes on past them, exits 1
// when any failed, and can be run again at any time; the server serves a row from its bytes
// whenever it holds them. The move refuses, with exit 1 and before it touches a row, while a live
// server process has no bucket (bucketlessProcesses): that process serves a file only from its row.
// It reads and writes file versions alone, never a document's, so it installs no block schema
// (openMigratedWithoutBlockSchema).
func backfillFiles(ctx context.Context, args []string, databaseURL, bucket string, out io.Writer) int {
	flags := flag.NewFlagSet("backfill-files", flag.ContinueOnError)
	flags.SetOutput(out)
	verifyOnly := flags.Bool("verify-only", false, "read back every file already moved and check it against its row's hash, moving nothing")
	restore := flags.Bool("restore", false, "write every moved file's bytes back into its row from the bucket: the rollback")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(out, "backfill-files: unexpected argument %q; the flags are --verify-only and --restore\n", flags.Arg(0))
		return 2
	}
	if *verifyOnly && *restore {
		fmt.Fprintln(out, "backfill-files: --verify-only and --restore cannot both be set")
		return 2
	}
	if bucket == "" {
		fmt.Fprintln(out, "backfill-files: DISPATCH_FILE_STORE_BUCKET is required")
		return 1
	}
	database, ok := openMigratedWithoutBlockSchema(ctx, "backfill-files", databaseURL, out)
	if !ok {
		return 1
	}
	defer database.Pool.Close()
	if !*verifyOnly && !*restore {
		bucketless, err := bucketlessProcesses(ctx, database)
		if err != nil {
			fmt.Fprintf(out, "backfill-files: %v\n", err)
			return 1
		}
		if len(bucketless) > 0 {
			fmt.Fprintf(out, "backfill-files: refused: these live server processes have no bucket and serve a file only from its row, "+
				"so no row may lose its bytes: %s. Give each one DISPATCH_FILE_STORE_BUCKET, or stop it, then run backfill-files again\n",
				strings.Join(bucketless, "; "))
			return 1
		}
	}
	fileStore, err := files.Open(ctx, bucket)
	if err != nil {
		fmt.Fprintf(out, "backfill-files: %v\n", err)
		return 1
	}
	if err := fileStore.Healthy(ctx); err != nil {
		fmt.Fprintf(out, "backfill-files: the bucket is not reachable: %v\n", err)
		return 1
	}
	pass, verb := files.BackfillRows, "moved"
	switch {
	case *verifyOnly:
		pass, verb = files.VerifyRows, "verified"
	case *restore:
		pass, verb = files.RestoreRows, "restored"
	}
	report, err := pass(ctx, database.Pool, fileStore, out)
	fmt.Fprintf(out, "backfill-files: %s=%d bytes=%d failed=%d\n", verb, report.Done, report.Bytes, len(report.Failed))
	if err != nil {
		fmt.Fprintf(out, "backfill-files: stopped: %v\n", err)
		return 1
	}
	if len(report.Failed) > 0 {
		return 1
	}
	return 0
}

// bucketlessProcesses names each live server process with no bucket (processes.Bucketless): one
// started without DISPATCH_FILE_STORE_BUCKET, or one of a build that predates the fleet columns
// (PredatesFleetColumns), which registers no file_store. Each is named by its instance id, its
// build and when it started.
func bucketlessProcesses(ctx context.Context, database *store.Store) ([]string, error) {
	bucketless, err := processes.Bucketless(ctx, database.Pool)
	if err != nil {
		return nil, err
	}
	var named []string
	for _, process := range bucketless {
		build := process.Build()
		if process.PredatesFleetColumns() {
			build += ", a build that does not register file_store"
		}
		named = append(named, fmt.Sprintf("%s (%s, started %s)", process.ID, build, process.StartedAt.UTC().Format(time.RFC3339)))
	}
	return named, nil
}

// loadServerURL resolves the dashboard origin exactly as the server does, for a subcommand
// that parses reference text.
func loadServerURL(env settingValues) string {
	envoyConfig, err := loadEnvoyConfig(env, config.LoadOptions{})
	if err != nil {
		slog.Error("dispatch: load envoy config", "error", err)
		os.Exit(1)
	}
	if envoyConfig.Dispatch == nil {
		return ""
	}
	return envoyConfig.Dispatch.ServerURL
}

// rebuildRefs reparses every reference source and reconciles the refs index with it. It
// refuses an empty server URL: text.Extract recognises same-origin dashboard URLs only against
// it, so an empty value would delete every URL-form mention.
func rebuildRefs(ctx context.Context, databaseURL, serverURL string, out io.Writer) int {
	if strings.TrimSpace(databaseURL) == "" {
		fmt.Fprintln(out, "rebuild-refs: DATABASE_URL is required")
		return 1
	}
	if strings.TrimSpace(serverURL) == "" {
		fmt.Fprintln(out, "rebuild-refs: DISPATCH_SERVER_URL (or dispatch.serverUrl in envoy.json) is required to recognise dashboard URLs; refusing to drop URL-form mentions")
		return 1
	}
	database, ok := openMigrated(ctx, "rebuild-refs", databaseURL, out)
	if !ok {
		return 1
	}
	defer database.Pool.Close()
	// rebuild-refs opens a transaction per source, so it marks its context like the other
	// commands: a read taken inside one is refused rather than left to deadlock the pool.
	report, err := refs.RebuildAll(store.WithTransactionTracking(ctx), database.Pool, serverURL)
	if err != nil {
		fmt.Fprintf(out, "rebuild-refs: %v\n", err)
		return 1
	}
	writeRebuildRefsReport(out, report)
	return 0
}

func writeRebuildRefsReport(out io.Writer, report refs.Rebuild) {
	fmt.Fprintf(out, "rebuild-refs: documents=%d asks=%d comments=%d messages=%d orphans=%d edges=%d blocks_owed=%d items_owed=%d\n",
		report.Documents, report.Asks, report.Comments, report.Messages, report.Orphans, report.Edges, report.BlocksOwed, report.ItemsOwed)
}

func writeAnchorBlockBackfillReport(out io.Writer, result docs.AnchorBlockBackfill) {
	fmt.Fprintf(out, "backfill-anchor-blocks: asks=%d comments=%d skipped=%d\n", result.Asks, result.Comments, result.Skipped)
}

func writeBlockIDBackfillReport(out io.Writer, report docs.BlockIDBackfill) bool {
	switch {
	case report.Err != nil:
		fmt.Fprintf(out, "%s error (%v)\n", report.ArtifactID, report.Err)
		return false
	case report.Skipped != "":
		fmt.Fprintf(out, "%s skipped (%s)\n", report.ArtifactID, report.Skipped)
		return true
	default:
		fmt.Fprintf(out, "%s stamped=%d\n", report.ArtifactID, report.Stamped)
		return true
	}
}
