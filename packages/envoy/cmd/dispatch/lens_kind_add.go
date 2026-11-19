package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// lensKindAddUsage is what `envoy-dispatch lens-kind-add` prints for -h, a usage error and a
// refusal alike, so the one place its flags are explained is the command itself.
const lensKindAddUsage = `usage: envoy-dispatch lens-kind-add --id <slug> --words <text> --source <text> --reference <text>

Adds one kind to the lens catalogue, in the database DATABASE_URL names: the lens_kind node
dispatch://lens_kind/<slug> holding the words a person reads for the kind, the constraint node
holding the kind's primary source and that source's reference, and the one constrained_by tuple
from the kind to its source, all in one transaction. Every running server answers the new kind on
its next read of the catalogue (GET /api/v1/schema/blocks) and takes it on a lens's has_kind, with
no restart and no schema version moved.

  --id         the kind's id, a lowercase slug of letters and digits in words joined by single
               hyphens (trade-off, five-whys); an id the catalogue holds is refused
  --words      the words a person reads for the kind
  --source     the kind's primary source: who wrote what, where and when
  --reference  that source's reference: its locator, edition, page or URL

Exit 0 when the kind was added, printing its address and its source's; 1 when it was refused
(an id the catalogue holds, a malformed id, empty words, source or reference, or a store that
holds no lens_kind or constraint type), naming why and adding nothing; 2 when it could not run
(a flag it does not take, no DATABASE_URL, a database it could not reach).
`

// lensKindAdd is `envoy-dispatch lens-kind-add`, the one writer of a lens kind after the seed
// (dispatch://INFRA-1950/spec#b-r-lens-catalogue): adding a kind is one node, its source and one
// tuple, and never a code change or a schema bump. It writes through graphstore.AddLensKind, the
// function the catalogue's own test calls, and migrates nothing: the schema it writes into is the
// deployment's own, read from DATABASE_URL as every other database subcommand reads it.
func lensKindAdd(ctx context.Context, args []string, databaseURL string, out, errOut io.Writer) int {
	flags := flag.NewFlagSet("lens-kind-add", flag.ContinueOnError)
	flags.SetOutput(errOut)
	flags.Usage = func() { fmt.Fprint(errOut, lensKindAddUsage) }
	var kind graphstore.LensKind
	flags.StringVar(&kind.ID, "id", "", "the kind's id, a lowercase slug")
	flags.StringVar(&kind.Words, "words", "", "the words a person reads for the kind")
	flags.StringVar(&kind.Source, "source", "", "the kind's primary source")
	flags.StringVar(&kind.Reference, "reference", "", "that source's reference")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(errOut, "lens-kind-add: unexpected argument %q; the flags are --id, --words, --source and --reference\n\n%s",
			flags.Arg(0), lensKindAddUsage)
		return 2
	}
	if strings.TrimSpace(databaseURL) == "" {
		fmt.Fprintln(errOut, "lens-kind-add: DATABASE_URL is required")
		return 2
	}
	database, err := store.Open(ctx, databaseURL)
	if err != nil {
		fmt.Fprintf(errOut, "lens-kind-add: open the database: %v\n", err)
		return 2
	}
	defer database.Pool.Close()
	source, err := addLensKind(ctx, database, kind)
	switch {
	case errors.Is(err, graphstore.ErrLensKindHeld), errors.Is(err, graphstore.ErrLensKindMalformed),
		errors.Is(err, graphstore.ErrSupertagNotHeld):
		fmt.Fprintf(errOut, "lens-kind-add: %v\n", err)
		return 1
	case err != nil:
		fmt.Fprintf(errOut, "lens-kind-add: %v\n", err)
		return 2
	}
	fmt.Fprintf(out, "lens-kind-add: added dispatch://%s/%s, held to its source dispatch://%s/%s\n",
		graphstore.KindLensKind, kind.ID, graphstore.KindConstraint, source)
	return 0
}

// addLensKind adds kind to the catalogue of database in one transaction of its own
// (graphstore.AddLensKind), answering its source's constraint id: what the command runs once its
// flags are read.
func addLensKind(ctx context.Context, database *store.Store, kind graphstore.LensKind) (string, error) {
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin the write: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	source, err := graphstore.AddLensKind(ctx, tx, kind, time.Now().UTC())
	if err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit the lens kind %s: %w", kind.ID, err)
	}
	return source, nil
}
