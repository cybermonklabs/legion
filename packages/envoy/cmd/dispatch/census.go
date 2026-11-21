package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// census is `envoy-dispatch census`, in three forms, each reading DATABASE_URL and each
// writing nothing:
//
//   - with no flag, the pre-deploy migration census (store.Census). A deployment runs it, as a
//     one-off task under the service's own database credentials, before it rolls the service to
//     this binary's image.
//   - `--store`, the node and tuple store held against the tables that project it
//     (graphcensus.StoreCensus), in both directions and for every kind, refusing a kind it
//     checked no row of but a tie kind of decisions, readings and asks that holds no row and no
//     node; with the store-only lines of copied_from and moved_to, which no table projects,
//     refusing a far end of either that does not resolve; with the dangling-citation counter's
//     line and the collision census over the restore's work (graphcensus.DanglingCitations,
//     CollisionsOf); and with what the decisions step's third phase still owes
//     (graphcensus.DecisionsCensus:
//     the decisions documents, their unconverted sections and the specs whose answered asks left
//     them), none of which refuses. `--verbose` adds one line for each citation the counter
//     counted.
//   - `--store --sizing`, that census and the growth pass after it: what the database is
//     written at, what every table this branch's migrations touch holds now, and where
//     graph_tuples and graph_nodes reach in a year against pgmigrate.CensusTableLimit. The
//     growth pass refuses nothing: beside a table at or above that limit, and a store table it
//     projects past it, it names the migration census's bounded-write rule, the gate a pending
//     migration that writes rows of such a table meets (pgmigrate.BoundedWriteRows), so this form
//     refuses only where the store census does. The growth pass counts only tables that exist
//     before this branch's first migration, so it runs on a database the store does not exist in
//     yet, where the census says so and passes on the growth pass alone; where the store exists
//     it also reads the store's type nodes, so that it parses each version against the block
//     schema the store declares.
//
// Each keeps the one exit convention: 0 when nothing refuses, 1 when the report refuses (every
// reason is in the report), 2 when the census could not be taken (errOut says why). Each prints
// counts, sizes, versions, file names, digests, node addresses, session metadata and SQLSTATEs
// only; no row leaves the database.
func census(ctx context.Context, args []string, databaseURL string, out, errOut io.Writer) int {
	flags := flag.NewFlagSet("census", flag.ContinueOnError)
	flags.SetOutput(errOut)
	storeSide := flags.Bool("store", false,
		"census the node and tuple store against the tables that project it, rather than the pending migrations")
	sizing := flags.Bool("sizing", false,
		"with --store, add the growth pass: the write rate, each table's size, and a year's projection against the census limit")
	verbose := flags.Bool("verbose", false,
		"with --store, print one line for each citation the dangling-citation counter counted")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(errOut, "census: unexpected argument %q; the flags are --store, --sizing and --verbose\n", flags.Arg(0))
		return 2
	}
	if *sizing && !*storeSide {
		fmt.Fprintln(errOut, "census: --sizing is the growth pass of the store census; run it as `census --store --sizing`")
		return 2
	}
	if *verbose && !*storeSide {
		fmt.Fprintln(errOut, "census: --verbose lists the citations the store census counts; run it as `census --store --verbose`")
		return 2
	}
	if strings.TrimSpace(databaseURL) == "" {
		fmt.Fprintln(errOut, "census: DATABASE_URL is required")
		return 2
	}
	if *storeSide {
		return storeCensus(ctx, databaseURL, *sizing, *verbose, out, errOut)
	}
	report, err := store.Census(ctx, databaseURL)
	if err != nil {
		writeCensusFailure(errOut, err)
		return 2
	}
	report.Write(out)
	if report.Refused() {
		return 1
	}
	return 0
}

// storeCensus runs `census --store`: it migrates nothing and writes nothing, reading one
// read-only snapshot of its own, so a deployment can take it before it rolls, exactly as it
// takes the migration census. verbose prints each citation the dangling-citation counter counted.
// Beside the store's lines it prints what the decisions step's third phase still owes
// (graphcensus.DecisionsCensus), which docs reads in a read-only snapshot of its own, since only
// docs reads a decisions document's sections; it refuses nothing.
func storeCensus(ctx context.Context, databaseURL string, sizing, verbose bool, out, errOut io.Writer) int {
	report, err := graphcensus.StoreCensus(ctx, databaseURL, sizing)
	if err != nil {
		writeCensusFailure(errOut, err)
		return 2
	}
	if !report.Absent {
		decisions, err := readDecisionsCensus(ctx, databaseURL)
		if err != nil {
			writeCensusFailure(errOut, err)
			return 2
		}
		report.Decisions = &decisions
	}
	if verbose {
		report.WriteVerbose(out)
	} else {
		report.Write(out)
	}
	if report.Refused() {
		return 1
	}
	return 0
}

// versions is `envoy-dispatch versions`: what schema_migrations, on the database DATABASE_URL
// names, records of each version against the migrations this binary embeds, and what this
// binary's next boot would do with each row (store.CheckVersions), written to out. It reads in a
// read-only transaction and migrates nothing, so a deployment runs it, as a one-off task of the
// image it means to roll out, before it rolls the service. Exit 0 when every recorded version has
// a file of this image and the next boot refuses none, 1 when one has no file, a row would be
// refused or the table records nothing, 2 when the check could not be taken (errOut says why).
func versions(ctx context.Context, databaseURL string, out, errOut io.Writer) int {
	if strings.TrimSpace(databaseURL) == "" {
		fmt.Fprintln(errOut, "versions: DATABASE_URL is required")
		return 2
	}
	check, err := store.CheckVersions(ctx, databaseURL)
	if err != nil {
		fmt.Fprintf(errOut, "versions: %v\n", err)
		return 2
	}
	check.Write(out)
	if !check.OK() {
		return 1
	}
	return 0
}

// readDecisionsCensus reads what the decisions step's third phase still owes
// (docs.ReadDecisionsCensus) in a read-only snapshot on a connection of its own.
func readDecisionsCensus(ctx context.Context, databaseURL string) (graphcensus.DecisionsCensus, error) {
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return graphcensus.DecisionsCensus{}, fmt.Errorf("connect for the decisions census: %w", err)
	}
	defer conn.Close(context.Background())
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return graphcensus.DecisionsCensus{}, fmt.Errorf("open the decisions census snapshot: %w", err)
	}
	defer tx.Rollback(context.Background())
	return docs.ReadDecisionsCensus(ctx, tx)
}

// writeCensusFailure prints why a census could not be taken, under the one label every form of
// the command reports under.
func writeCensusFailure(errOut io.Writer, err error) {
	message := err.Error()
	if !strings.HasPrefix(message, "census: ") {
		message = "census: " + message
	}
	fmt.Fprintln(errOut, message)
}
