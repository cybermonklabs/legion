package refs

import (
	"context"
	"fmt"
)

// ForgetDocument deletes what the reference tables hold for the document artifactID, inside the
// transaction that is about to delete the document's own row (api.removeArtifact): the refs rows
// written from it, the row it owes the re-index queue, and the backoff its own mentions were
// deferred under (ClearExcerptBackoff). None of them carries a foreign key to artifacts(id), so
// each would outlive the document. Every one of them is keyed by the document's uuid
// (refs.from_id, refs_grammar_owed.id, reference_excerpt_backoff.artifact_id), and a document
// minted later at the same reference key is a new uuid, so it inherits none of them: a row left
// here would sit unread rather than reach anybody.
//
// What points AT the document is left where it stands: a removal is refused while another node
// cites the document or a block of it (DocumentIsCited), and that test is read in this same
// transaction. The indexers of other nodes take neither the issue's row nor this document's
// room lock, so a citation committed after that test still lands. What its row then points at is
// the reference key and not the row: that key resolves to nothing, until a later document of the
// same title on the same issue mints it again, and the citation then reads as a citation of that
// document. The spec of dispatch://INFRA-1334 says the same of a citation written outside
// Dispatch, and accepts it for a document that held nothing.
//
// The queue rows (refs_grammar_owed) are keyed by kind and id, and a document is owed under two
// kinds - `artifact`, its own block index, and `scope`, the sources around it - so the id alone
// is read here, where a source settling its own index reads both (settleOwed).
func ForgetDocument(ctx context.Context, q Execer, artifactID string) error {
	if _, err := q.Exec(ctx, `delete from refs where from_kind = 'artifact' and from_id = $1`, artifactID); err != nil {
		return fmt.Errorf("delete the references of the removed document %s: %w", artifactID, err)
	}
	if _, err := q.Exec(ctx, `delete from refs_grammar_owed where id = $1::uuid`, artifactID); err != nil {
		return fmt.Errorf("settle the index owed by the removed document %s: %w", artifactID, err)
	}
	return ClearExcerptBackoff(ctx, q, artifactID)
}

// DocumentIsCited answers whether any other node cites the document whose id is artifactID and
// whose reference key is refKey, or a block of it. A removal reads it to refuse a document
// somebody's text names (api.artifactIsUntouched), since the row it would delete is the row that
// reference resolves to.
//
// Three tables hold a link by its target, and a citation of a document can be in any of them:
// refs, where a document's, a comment's, a message's and an ask's question's own mentions are;
// block_refs, where the mentions a document's blocks hold are; and ask_block_refs, where a link
// written in an option or in the answer of an ask is, since an ask's own refs row holds the links
// of its question alone (askPartLinks).
//
// The document's own mentions are not citations of it: a refs row from it (from_kind 'artifact'
// with its id) and a block_refs row of one of its own blocks go with it (ForgetDocument, the
// artifact row's own cascade). A part of an ask counts whatever its position: a part a later
// revision dropped keeps its link row, and ask_block_refs carries no key to artifacts, so such a
// row would outlive the document as a live part's would.
//
// A block of the document is named by its id's prefix, the reference key and '#', so one read per
// table answers for the document and for its blocks together.
func DocumentIsCited(ctx context.Context, q Queryer, artifactID, refKey string) (bool, error) {
	var cited bool
	if err := q.QueryRow(ctx, `
		select exists(select 1 from refs r
		              where ((r.to_kind = 'artifact' and r.to_id = $3) or (r.to_kind = 'block' and r.to_id like $4))
		                and not (r.from_kind = 'artifact' and r.from_id = $2))
		       or exists(select 1 from block_refs b
		                 where ((b.to_kind = 'artifact' and b.to_id = $3) or (b.to_kind = 'block' and b.to_id like $4))
		                   and b.artifact_id <> $1)
		       or exists(select 1 from ask_block_refs a
		                 where (a.to_kind = 'artifact' and a.to_id = $3) or (a.to_kind = 'block' and a.to_id like $4))
	`, artifactID, artifactID, refKey, refKey+"#%").Scan(&cited); err != nil {
		return false, fmt.Errorf("read what cites the document %s: %w", artifactID, err)
	}
	return cited, nil
}
