package graphcensus

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// --- the identity mentions of the blocks of a document an older build stated ---------------------

// The store's own release states a document's blocks - each block's node, its in_document and
// part_of, its links - and writes the revision fence this release's pass writes
// (document_block_versions.store_version), but it states no `mentions` tuple to the person, the
// agent's session or the role a block names, which this release's pass states beside the rest
// (refs' blockIdentityMentions, R5). A document such a build indexed stands at its version with no
// mark, so no pass of this release reads it again. Migration 20261122100124 says which documents
// those are (identities_stated false); this pass, in the backfill and in the block-index tick's
// catch-up (rollPasses), marks each of them that holds a block whose text names an identity owed
// this release's pass (store_owed_at, as the backfill's block pass marks a document), and sets
// identities_stated on every row it reads. The block-index tick then indexes the version the
// row stands at (docs' owedDocuments lists a mark, refs' revision fence admits a pass at the
// version it stands at under one), and states the mentions as it states them of a version it
// indexes first: this pass states no tuple itself, so the mentions are the pass's own, read off the
// render through the one reading of a document's text.

// blockIdentitiesKind is the block identity pass's kind.
const blockIdentitiesKind = "block_identities"

// blockIdentitiesOwed is the document_block_versions rows the pass owes: each whose
// identities_stated is false and whose blocks the store states at a version (store_version), read
// through the partial index migration 20261122100125 adds. A row a build before this release
// inserted owed an index and stands at none is the block index's, whose pass states the mentions
// with the rest; the row is read once a pass has stated it.
//
// kept for one release: block-identities-stated
const blockIdentitiesOwed = `
	from document_block_versions v
	where not v.identities_stated and v.store_version is not null`

// identityAddressPattern is the start of every address that names an identity in a text: a
// person's, an agent's session's and a role's `dispatch://` forms (text's parseDispatch), as a SQL
// regular expression. No dashboard page names one, so a text that holds none of these names no
// identity.
const identityAddressPattern = `dispatch://(person|agent|role)/`

// blockNamesIdentity is true of the document_block_versions row v whose document holds a block
// whose text at the version the row stands at names an identity (identityAddressPattern): a block
// the version holds (document_blocks, naming its node), its text the newest document_block_texts
// row of it at or below store_version. It reads the text alone and not what the block's node
// mentions already: a build before this release can write a version whose block names an identity
// beside one this release's pass stated a mention of, and the pass the row owes appends no mention
// where one stands already (refs' identityMentions), so a block whose text names an identity owes
// that pass whatever its node mentions. A block that holds another naming one reads true too, as
// its own text holds the child's; the pass it owes states nothing of it then
// (blockIdentityMentions places a mention in the innermost block).
const blockNamesIdentity = `exists (
		select 1 from document_blocks b
		join lateral (
			select x.text from document_block_texts x
			where x.artifact_id = b.artifact_id and x.block_id = b.block_id and x.version <= v.store_version
			order by x.version desc limit 1
		) held on true
		where b.artifact_id = v.artifact_id and b.node_kind is not null
		  and held.text ~ '` + identityAddressPattern + `')`

// blockIdentitiesPass marks owed this release's block index each document a build before this
// release stated the blocks of without their identity mentions that holds a block whose text names
// an identity (blockNamesIdentity), and sets identities_stated on every row it reads
// (writeBlockIdentities).
func blockIdentitiesPass() inboxPass {
	return inboxPass{
		kind:      blockIdentitiesKind,
		batch:     `select v.artifact_id::text ` + blockIdentitiesOwed + ` and v.artifact_id > $1::uuid order by v.artifact_id limit $2`,
		start:     []string{zeroUUID},
		write:     writeBlockIdentities,
		remaining: blockIdentitiesOwed,
	}
}

// BlockIdentitiesMarkStatement is the block identity pass's mark of the documents $1 still owed
// (blockIdentitiesOwed), at the marking rule $2 (refs.BlockIndexMarkingRule): each that holds a
// block whose text names an identity (blockNamesIdentity) is marked owed this release's block
// index, as the backfill's block pass marks a document (BlockBatchMarkStatement) - a mark standing
// there keeps its time and the higher of its rule and $2 (refs.OwedRuleKept) - and every one of
// them takes identities_stated, which the connection that runs it, carrying the marker writer
// setting, keeps. Both columns of the mark are assigned from one sub-select, which evaluates the
// correlated test once a row. It is one of the statements that decide what a build of a marking
// rule owes (docs' TestTheMarkingRuleNamesTheStatementsItMarksWith): a build of the rule before
// this one, which states no identity mention, neither drains its mark at the version a document
// stands at nor reads it as its own (refs.OwedMarkListed).
//
// kept for one release: block-identities-stated
var BlockIdentitiesMarkStatement = `
	update document_block_versions v set identities_stated = true,
		(store_owed_at, store_owed_rule) = (
			select case when owed.named then coalesce(v.store_owed_at, now()) else v.store_owed_at end,
			       case when owed.named then ` + refs.OwedRuleKept("v", "$2::integer") + ` else v.store_owed_rule end
			from (select ` + blockNamesIdentity + ` as named) owed)
	where v.artifact_id = any($1::uuid[]) and not v.identities_stated and v.store_version is not null
`

// writeBlockIdentities takes the documents keys names under the revision fence's locks, the
// owner and then the document's document_block_versions row (lockBlockDocuments), the order every
// writer of a document's blocks takes them in, so a version write or a pass of the document waits
// for the batch or the batch for it, and then marks those still owed at this build's marking rule
// (BlockIdentitiesMarkStatement).
func writeBlockIdentities(ctx context.Context, tx pgx.Tx, keys [][]string) (inboxWrote, error) {
	documents := column(keys, 0)
	if err := lockBlockDocuments(ctx, tx, documents); err != nil {
		return inboxWrote{}, err
	}
	tag, err := tx.Exec(ctx, BlockIdentitiesMarkStatement, documents, refs.BlockIndexMarkingRule)
	if err != nil {
		return inboxWrote{}, fmt.Errorf("mark the documents whose blocks' identity mentions are owed: %w", err)
	}
	return inboxWrote{rows: tag.RowsAffected()}, nil
}
