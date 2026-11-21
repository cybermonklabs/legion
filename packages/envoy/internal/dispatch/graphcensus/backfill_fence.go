package graphcensus

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
)

// lockBlockDocuments takes the locks the fence's writers hold while they write a document's blocks
// or its block references, in the order R23 gives and docs' indexOwedDocument takes them for one
// document: the document's owner lock (store.LockOwner), then its document_block_versions row
// `for no key update`.
//
// Over several documents it takes every owner lock first, each once, in the order of its owner
// key (events.LockOwnerRows): the one order over owners, in which the erase takes the owners it
// writes under (grapherase's lockInStoreOrder) and events.Broker.LockOwners the owners of several
// events, "artifact:<id>" for a document no issue owns before "issue:<key>". Only then does it
// take each document's document_block_versions row, in document id order. Taking one document's
// owner lock and fence row before the next document's owner would put an issue at a different
// place for each set of its documents: a batch over two documents of two issues whose ids sort
// the other way round from their owner keys, and an erase writing under both, could each hold the
// owner lock the other waits for.
//
// A document deleted since its batch was read has no owner left to read, and its block rows and
// its document_block_versions row went with it (each cascades from artifacts), so there is
// nothing of it to lock. It sorts documents in place.
func lockBlockDocuments(ctx context.Context, tx pgx.Tx, documents []string) error {
	slices.Sort(documents)
	documents = slices.Compact(documents)
	rows, err := tx.Query(ctx, `
		select id::text, coalesce(issue_key, '') from artifacts where id = any($1::uuid[])
	`, documents)
	if err != nil {
		return fmt.Errorf("load the owners of a batch's documents: %w", err)
	}
	owned, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) ([2]string, error) {
		var document [2]string
		err := row.Scan(&document[0], &document[1])
		return document, err
	})
	if err != nil {
		return fmt.Errorf("load the owners of a batch's documents: %w", err)
	}
	owners := events.OwnerRows{}
	for _, document := range owned {
		owners.Add(document[1], document[0], "document "+document[0])
	}
	if err := events.LockOwnerRows(ctx, tx, owners); err != nil {
		return err
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
