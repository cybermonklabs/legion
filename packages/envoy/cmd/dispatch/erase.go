package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

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
// holds the erased text and every `artifact_reviews` row whose digest covered those bytes,
// prints them each by its document and its version, and rolls back with exit 1. Run again with
// --confirm it prints the same list and removes it, in one transaction, recording what went into
// `graph_erasures` with no erased text in the row.
//
// The exit convention is the census's: 0 when it erased, 1 when it refused and said why (no
// --confirm, or a row that points at the node and is not part of it), 2 when it could not run at
// all.
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
	erasure, err := graphcensus.Erase(ctx, databaseURL, kind, id, *confirm)
	if err != nil {
		fmt.Fprintf(errOut, "erase: %v\n", err)
		return 2
	}
	erasure.Write(out)
	switch {
	case len(erasure.Blockers) > 0:
		fmt.Fprintf(errOut, "erase: %s:%s is pointed at by rows that are not part of it, listed above; "+
			"nothing was removed\n", kind, id)
		return 1
	case !erasure.Applied:
		fmt.Fprintf(errOut, "erase: nothing was removed. The list above is what `envoy-dispatch erase "+
			"--node %s:%s --confirm` would remove, and every version and review on it ends with it.\n", kind, id)
		return 1
	}
	return 0
}
