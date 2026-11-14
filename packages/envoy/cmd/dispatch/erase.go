package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
)

// erase is `envoy-dispatch erase --node <kind>:<id>`, the one path in Dispatch that deletes a
// tuple row or a node row. Everything else the store does is an append.
//
// It is reached by the command and the database credential alone: no HTTP route serves it, no
// agent bearer and no session reaches it, and it reads DATABASE_URL as every other database
// subcommand does. It migrates nothing - the schema it works on is the deployment's own.
//
// It refuses to remove anything until it has listed what it would: run with --node alone it
// reads the node, its tuples, its projection rows, every `artifact_versions` row whose markdown
// holds the erased text and every `artifact_reviews` row whose digest covered those bytes, and
// every document whose live state it would replace with what that costs, prints them each by its
// document and its version, and rolls back with exit 1. Run again with --confirm it prints the
// same list and removes it, in one transaction, recording what went into `graph_erasures` with no
// erased text in the row.
//
// The exit convention is the census's: 0 when it erased, 1 when it refused and said why (no
// --confirm, a row that points at the node and is not part of it, or a document whose live text
// holds the erased bytes where its markdown spells them otherwise), 2 when it could not run at
// all, and 2 too when it erased and its sweep of text_trees after the commit failed, the report
// printed first.
func erase(ctx context.Context, args []string, databaseURL string, out, errOut io.Writer) int {
	flags := flag.NewFlagSet("erase", flag.ContinueOnError)
	flags.SetOutput(errOut)
	node := flags.String("node", "", "the node to erase, as <kind>:<id>")
	confirm := flags.Bool("confirm", false,
		"remove what a run without it lists; without this flag the command writes nothing")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(errOut, "erase: unexpected argument %q; the flags are --node and --confirm\n", flags.Arg(0))
		return 2
	}
	if strings.TrimSpace(databaseURL) == "" {
		fmt.Fprintln(errOut, "erase: DATABASE_URL is required")
		return 2
	}
	kind, id, err := graphcensus.ParseNode(*node)
	if err != nil {
		fmt.Fprintf(errOut, "erase: %v\n", err)
		return 2
	}
	erasure, err := graphcensus.Erase(ctx, databaseURL, kind, id, *confirm, eraseStores(api.StoreTreesTimeout))
	if erasure != nil {
		erasure.Write(out)
	}
	if err != nil {
		fmt.Fprintf(errOut, "erase: %v\n", err)
		return 2
	}
	switch {
	case len(erasure.Blockers) > 0:
		fmt.Fprintf(errOut, "erase: %s:%s is pointed at by rows that are not part of it, listed above; "+
			"nothing was removed\n", kind, id)
		return 1
	case len(erasure.Unredacted()) > 0:
		fmt.Fprintf(errOut, "erase: the live text of %s holds the erased bytes where its markdown spells them "+
			"otherwise, so no marker can stand where they are; nothing was removed. Edit them out of the "+
			"document and run the erase again.\n", strings.Join(erasure.Unredacted(), ", "))
		return 1
	case !erasure.Applied:
		fmt.Fprintf(errOut, "erase: nothing was removed. The list above is what `envoy-dispatch erase "+
			"--node %s:%s --confirm` would remove, and every version and review on it ends with it.\n", kind, id)
		return 1
	}
	return 0
}

// eraseStores are the writers the erase reaches in docs and api (graphcensus.Stores), which
// graphcensus cannot import: a document's live state is replaced through the statements a rebuild
// writes (docs.ReadErasedLiveText), and the term statistics are counted again by the pass that
// counts a project in full (api.RecountSearchTermStatsTx). Both run inside the erase's own
// transaction. treeStoreBound is how long a read's detached store of its display trees can run
// (api.StoreTreesTimeout), which the erase waits out after its commit; a test passes a shorter
// one.
func eraseStores(treeStoreBound time.Duration) graphcensus.Stores {
	return graphcensus.Stores{
		TreeStoreBound: treeStoreBound,
		LiveText: func(ctx context.Context, tx pgx.Tx, artifactID string, texts []string, marker string) (*graphcensus.LiveText, error) {
			live, err := docs.ReadErasedLiveText(ctx, tx, artifactID, texts, marker)
			if err != nil || live == nil {
				return nil, err
			}
			return &graphcensus.LiveText{
				Held: live.Held, Left: live.Left, Updates: live.Updates, AnchorMarks: live.AnchorMarks,
				MarginRecords: live.MarginRecords, Text: live.Text,
				Replace: func(ctx context.Context, tx pgx.Tx) (graphcensus.ReplacedHistory, error) {
					report, err := live.Replace(ctx, tx)
					return graphcensus.ReplacedHistory{
						Updates: report.RemovedUpdates, Checkpoints: report.RemovedCheckpoints,
						Snapshots: report.RemovedSnapshots,
					}, err
				},
			}, nil
		},
		TermStatistics: func(ctx context.Context, tx pgx.Tx, projects []string) (graphcensus.TermRecount, error) {
			counted, queued, err := api.RecountSearchTermStatsTx(ctx, tx, projects)
			return graphcensus.TermRecount{Projects: counted, Queued: queued}, err
		},
	}
}
