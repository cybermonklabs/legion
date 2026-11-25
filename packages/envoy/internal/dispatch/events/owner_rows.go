package events

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// issueOwnerPrefix and artifactOwnerPrefix start the owner keys of an issue's row and of the
// artifacts row of a document no issue owns. eventOwnerKey keys an event's owner by them and
// OwnerRows a writer's owner rows, so the two sort a shared owner at one place: the order is one
// string order over one spelling, held here and nowhere else.
const (
	issueOwnerPrefix    = "issue:"
	artifactOwnerPrefix = "artifact:"
)

// OwnerRows are owner rows a writer takes before it writes under them, each by its owner key as
// eventOwnerKey keys the owner of an event - "issue:<key>", or "artifact:<id>" for a document no
// issue owns. A backfill batch takes the owners of its documents and objects through them
// (graphcensus's lockBlockDocuments and lockObjectOwners).
type OwnerRows map[string]struct{}

// Add records the owner of a thing: issue when an issue owns it, and otherwise the document
// artifact is. A thing with neither stands under no owner row and is not recorded.
func (o OwnerRows) Add(issue, artifact string) {
	key := issueOwnerPrefix + issue
	if issue == "" {
		if artifact == "" {
			return
		}
		key = artifactOwnerPrefix + artifact
	}
	o[key] = struct{}{}
}

// LockOwnerRows takes the owner lock of each of owners (store.LockOwners) in one statement, each
// once: the transaction advisory lock that every writer of an issue, or of a document no issue
// owns, takes before it reads what it writes, and that docs' lockArtifactOwner takes for the
// version write and the reconciliation tick. store.LockOwners sorts the owners by their lock
// keys, the `<kind>:<key>` spelling these owner keys share, so a writer that takes its owners
// here and a transaction that appends events for several owners take the owners they share in
// one order, and neither holds a lock the other waits for. While an earlier build is in the fleet
// the owners' rows are taken beside the locks, as that build's writers take them.
//
// A row gone since its key was read, or a document an issue has taken since, is no error: the
// caller read its owners before it locked them, and it reads again under its locks what it stands
// under now (the backfill batches' heldBlockRows and writeObjectBatch).
func LockOwnerRows(ctx context.Context, tx pgx.Tx, owners OwnerRows) error {
	locks := make([]store.Owner, 0, len(owners))
	for _, key := range slices.Sorted(maps.Keys(owners)) {
		if issue, ok := strings.CutPrefix(key, issueOwnerPrefix); ok {
			locks = append(locks, store.IssueOwner(issue))
			continue
		}
		locks = append(locks, store.ArtifactOwner(strings.TrimPrefix(key, artifactOwnerPrefix)))
	}
	if err := store.LockOwners(ctx, tx, locks...); err != nil {
		return fmt.Errorf("lock the owners of %d rows: %w", len(locks), err)
	}
	return nil
}
