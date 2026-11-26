package graphcensus

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// lockBlockDocuments takes the locks the fence's writers hold while they write a document's blocks
// or its block references, in the order R23 gives and docs' indexOwedDocument takes them for one
// document: the document's owner lock (store.LockOwner), then its document_block_versions row
// `for no key update`.
//
// Over several documents it takes every owner lock first, in one statement (store.LockOwners):
// each document's owner as docs' lockArtifactOwner names it (store.OwnerOf), its issue or, for a
// document no issue owns, the document itself, each lock once and in the one order of their lock
// keys in which every transaction that locks several owners takes them, events.Broker.LockOwners
// among them. Only then does it take each document's document_block_versions row, in document id
// order. Taking one document's owner lock and fence row before the next document's owner would
// put an issue at a different place for each set of its documents: a batch over two documents of
// two issues whose ids sort the other way round from their lock keys, and a transaction that
// appends events under both, could each hold the owner lock the other waits for.
//
// A document deleted since its batch was read has no owner left to read, and its block rows and
// its document_block_versions row went with it (each cascades from artifacts), so there is
// nothing of it to lock. A document an issue has taken since its owner was read is no error
// either: the batch reads again under its locks what it stands under now (heldBlockRows). It
// sorts documents in place.
func lockBlockDocuments(ctx context.Context, tx pgx.Tx, documents []string) error {
	slices.Sort(documents)
	documents = slices.Compact(documents)
	rows, err := tx.Query(ctx, `
		select issue_key, id::text from artifacts where id = any($1::uuid[])
	`, documents)
	if err != nil {
		return fmt.Errorf("load the owners of a batch's documents: %w", err)
	}
	owners, err := documentOwners(rows)
	if err != nil {
		return fmt.Errorf("load the owners of a batch's documents: %w", err)
	}
	if err := store.LockOwners(ctx, tx, owners...); err != nil {
		return fmt.Errorf("lock the owners of %d documents: %w", len(owners), err)
	}
	for _, document := range documents {
		if _, err := tx.Exec(ctx, `
			select 1 from document_block_versions where artifact_id = $1 for no key update
		`, document); err != nil {
			return fmt.Errorf("lock the block index row of document %s: %w", document, err)
		}
	}
	return nil
}

// documentOwners reads rows of (issue key, null for a row no issue owns; document id) as the owner
// each row stands under (store.OwnerOf): its issue, or for a document no issue owns, the document
// itself. lockBlockDocuments and lockObjectOwners read their owners through it, so the two name an
// owner alike.
func documentOwners(rows pgx.Rows) ([]store.Owner, error) {
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (store.Owner, error) {
		var issue *string
		var document string
		if err := row.Scan(&issue, &document); err != nil {
			return store.Owner{}, err
		}
		return *store.OwnerOf(issue, &document), nil
	})
}
