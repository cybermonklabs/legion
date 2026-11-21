package textversions

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
)

// specTreesQuery reads, of the primary document versions $1 (their documents' ids) and $2 (their
// numbers), each that stores an insert tree, with its markdown and the tree: the versions whose
// text a read can mask (docs.MaskedVersionText). A version that stores none is read as stored.
const specTreesQuery = `
	select v.artifact_id::text, v.number, coalesce(v.markdown, ''), v.insert_tree
	  from unnest($1::text[], $2::integer[]) as s (artifact_id, number)
	  join public.artifact_versions v on v.artifact_id = s.artifact_id::uuid and v.number = s.number
	 where v.insert_tree is not null`

// maskedSpecTexts reads, in tx, the text of each primary document version versions name that a
// read masks (docs.MaskedVersionText): the versions that store an insert tree, one of whose
// inserts' comments is deleted and no accept made document text, each by the version it is,
// with its text as every read of it gives it. A version that a read gives as stored is not in the
// map.
//
// A deleted pending insert's words were its comment's own and never document text, and a vector
// cannot be masked at a read, so an issue's row holds the vector of the masked text
// (insertVersionsQuery). A row the fill wrote before the delete is rewritten by the delete itself
// (api's deleteComment, store.QueueTextVersionsMask). tx holds the keyword table's mask lock
// (store.LockTextVersionsMask), which that delete shares before it rewrites the rows, so this
// read, a statement after the lock with its own snapshot, reads the comment deleted where the
// delete committed before the batch began, and the delete waits for the batch and rewrites its
// rows where it did not.
func maskedSpecTexts(ctx context.Context, tx pgx.Tx, versions []version) (map[specVersion]string, error) {
	var artifacts []string
	var numbers []int32
	seen := map[specVersion]bool{}
	for index := range versions {
		key, ok := versions[index].spec()
		if !ok || seen[key] {
			continue
		}
		seen[key] = true
		artifacts, numbers = append(artifacts, key.artifact), append(numbers, key.number)
	}
	if len(artifacts) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, specTreesQuery, artifacts, numbers)
	if err != nil {
		return nil, fmt.Errorf("read the insert trees of %d primary document versions: %w", len(artifacts), err)
	}
	type stored struct {
		key      specVersion
		markdown string
		tree     []byte
	}
	var trees []stored
	for rows.Next() {
		var one stored
		if err := rows.Scan(&one.key.artifact, &one.key.number, &one.markdown, &one.tree); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read the insert tree of a primary document version: %w", err)
		}
		trees = append(trees, one)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the insert trees of %d primary document versions: %w", len(artifacts), err)
	}
	var masked map[specVersion]string
	for _, one := range trees {
		text, err := docs.MaskedVersionText(ctx, tx, one.key.artifact, one.markdown, one.tree)
		if err != nil {
			return nil, err
		}
		if text == one.markdown {
			continue
		}
		if masked == nil {
			masked = map[specVersion]string{}
		}
		masked[one.key] = text
	}
	return masked, nil
}
