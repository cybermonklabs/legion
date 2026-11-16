package events

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
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
// lockOwners keys the owner of an event - "issue:<key>", or "artifact:<id>" for a document no
// issue owns - with the first thing read standing under it, which an error or a refusal names.
// The erase takes the owner rows it writes under through them (grapherase's lockInStoreOrder), and
// a backfill batch the owners of its documents (graphcensus's lockBlockDocuments).
type OwnerRows map[string]string

// Add records the owner of what: issue when an issue owns it, and otherwise the document artifact
// is. A thing with neither stands under no owner row and is not recorded.
func (o OwnerRows) Add(issue, artifact, what string) {
	key := issueOwnerPrefix + issue
	if issue == "" {
		if artifact == "" {
			return
		}
		key = artifactOwnerPrefix + artifact
	}
	if _, read := o[key]; !read {
		o[key] = what
	}
}

// LockOwnerRows takes each of owners `for no key update`, once, in ascending owner key, which is
// the order lockOwners takes the owners of several events in: a writer that takes its owner rows
// here and a transaction that appends events for several owners take the owners they share in one
// order, so neither holds a row the other waits for. Each row is taken with the statements docs'
// lockArtifactOwner takes it with, which the version write and the reconciliation tick take it
// with: an issue's row, or a project document's own artifacts row while no issue owns it.
//
// A row gone since its key was read, or a document an issue has taken since, locks nothing and is
// no error: the caller read its owners before it locked them, and it reads again under its locks
// what it stands under now (the erase's read, the backfill batch's heldBlockRows).
func LockOwnerRows(ctx context.Context, tx pgx.Tx, owners OwnerRows) error {
	for _, key := range slices.Sorted(maps.Keys(owners)) {
		if issue, ok := strings.CutPrefix(key, issueOwnerPrefix); ok {
			if _, err := tx.Exec(ctx, `
				select closed_at is null from issues where key = $1 for no key update
			`, issue); err != nil {
				return fmt.Errorf("lock issue %s, the owner of %s: %w", issue, owners[key], err)
			}
			continue
		}
		artifact := strings.TrimPrefix(key, artifactOwnerPrefix)
		if _, err := tx.Exec(ctx, `
			select true from artifacts where id = $1 and issue_key is null for no key update
		`, artifact); err != nil {
			return fmt.Errorf("lock document %s, the owner of %s: %w", artifact, owners[key], err)
		}
	}
	return nil
}
