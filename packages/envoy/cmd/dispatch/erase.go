package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/grapherase"
)

// erase is `envoy-dispatch erase --node <kind>:<id>`, the one path in Dispatch that deletes a
// tuple row or a node row. Everything else the store does is an append.
//
// It is reached by the command and the database credential alone: no HTTP route serves it, no
// agent bearer and no session reaches it, and it reads DATABASE_URL as every other database
// subcommand does, and the dashboard origin as the server does (loadServerURL), which it cuts a
// redacted excerpt around as the server's writers cut one, refusing an empty one as rebuild-refs
// does. It migrates nothing - the schema it works on is the deployment's own - and it holds the
// documents it reads to the block schema the store's type nodes build, as the server does
// (grapherase.Erase installs it before it reads).
//
// It removes nothing until an operator has read what it would. Run with --node alone it reads,
// in a read-only transaction that takes no write lock, every row it would delete, rewrite or
// redact - the node, its tuples, its projection rows, each version of its own document whose
// markdown holds the erased text and each approval over it by its issue and its version, its
// embedding rows, each event row about it by its id, its document's history - and every row it
// would leave, which holds the same bytes and derives from another node, by its table and key.
// It prints that listing with its digest and exits 1. Run with --confirm <digest> it reads the
// listing again in its own transaction, under its locks, and removes what it lists only when that
// listing has the digest it was given, recording what went into `graph_erasures` with no erased
// text in the row; a listing that changed since is refused with both digests, removing nothing.
//
// The exit convention is the census's: 0 when it erased, 1 when it refused and said why (no
// --confirm, a digest that is not the listing's, a row of another node that states something
// about this one, or a document whose live text holds the erased bytes where its markdown spells
// them otherwise), 2 when it could not run at all, and 2 too when it erased and its sweep of
// text_trees after the commit failed, the report printed first.
func erase(ctx context.Context, args []string, databaseURL, serverURL string, out, errOut io.Writer) int {
	flags := flag.NewFlagSet("erase", flag.ContinueOnError)
	flags.SetOutput(errOut)
	node := flags.String("node", "", "the node to erase, as <kind>:<id>")
	confirm := flags.String("confirm", "",
		"the listing digest a run without it printed; the run removes what it lists only when the listing it reads has this digest")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(errOut, "erase: unexpected argument %q; the flags are --node and --confirm <digest>\n", flags.Arg(0))
		return 2
	}
	if strings.TrimSpace(databaseURL) == "" {
		fmt.Fprintln(errOut, "erase: DATABASE_URL is required")
		return 2
	}
	kind, id, err := grapherase.ParseNode(*node)
	if err != nil {
		fmt.Fprintf(errOut, "erase: %v\n", err)
		return 2
	}
	erasure, err := grapherase.Erase(ctx, databaseURL, serverURL, kind, id, *confirm, api.StoreTreesTimeout)
	if erasure != nil {
		erasure.Write(out)
	}
	if err != nil {
		fmt.Fprintf(errOut, "erase: %v\n", err)
		return 2
	}
	switch {
	case erasure.Applied:
		return 0
	case erasure.Mismatch():
		fmt.Fprintf(errOut, "erase: nothing was removed: the listing this run read has digest %s, and --confirm named %s, so a row "+
			"on it changed since the run that printed that digest. Read the listing above and confirm its digest.\n",
			erasure.Digest, erasure.Confirmed)
	case len(erasure.Blockers) > 0:
		fmt.Fprintf(errOut, "erase: %s:%s is stated by rows of other nodes, listed above; nothing was removed. "+
			"Erase or edit those first.\n", kind, id)
	case len(erasure.Unredacted()) > 0:
		fmt.Fprintf(errOut, "erase: the live text of %s holds the erased bytes where its markdown spells them "+
			"otherwise, so no marker can stand where they are; nothing was removed. Edit them out of the "+
			"document and run the erase again.\n", strings.Join(erasure.Unredacted(), ", "))
	default:
		fmt.Fprintf(errOut, "erase: nothing was removed. The listing above is every row `envoy-dispatch erase --node %s:%s "+
			"--confirm %s` would delete, rewrite, redact or leave.\n", kind, id, erasure.Digest)
	}
	return 1
}
