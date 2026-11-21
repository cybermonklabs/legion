package refs

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// BlockTexts is the whole current text of each block targets names, by target, for who: a
// document's block (kind block, addressed '<document ref_key>#<block id>') as its latest indexed
// version holds it (CurrentBlockText); a block of a comment, of a message or of an ask (the item's
// block kind, ItemKind.BlockKind) as CurrentItemBlockText reads it; and a finding of a lens (kind
// lens_block, addressed 'lens/<uuid>#<block id>') as the newest value of its text tuple, the text
// the lens read answers for it and its loader cuts its name from (standingText). A target of any
// other kind is passed over, and a block no current version or revision holds has no entry, which
// is the block TargetNames answers nothing for either.
//
// A finding's text is read off its own node in the store, whose row says who may see it, so its
// statement holds each row to the condition the finding's loader (loadReasoningNodes) holds it to
// (heldNodesWhere), the predicate of its kind for who over that row (VisibleKindRow) among it: a
// finding the store holds gone, or one reserved for an author who is not who, has no entry,
// whatever the caller resolved first. A block of a document or of an item is read for a caller
// that has already resolved it for who through TargetNames, which binds each block kind's
// predicate, so its statement binds none.
//
// It is TargetNames' companion for a reader that draws a block whole rather than by its name,
// which stops at the block's stored excerpt: each kind is read in one statement whatever the
// number of targets, so a batch costs a statement per kind of block it names and none for a
// kind it does not.
func BlockTexts(ctx context.Context, q Queryer, who model.Actor, targets []Target) (map[Target]string, error) {
	ids := map[string][]string{}
	for _, target := range targets {
		ids[target.Kind] = append(ids[target.Kind], target.ID)
	}
	texts := make(map[Target]string, len(targets))
	if nodes := ids["block"]; len(nodes) > 0 {
		rows, err := q.Query(ctx, `
			select d.node_id, held.text
			from document_blocks d
			join lateral (
				select t.text from document_block_texts t
				where t.artifact_id = d.artifact_id and t.block_id = d.block_id
				order by t.version desc limit 1
			) held on true
			where d.node_id = any($1) and held.text is not null`, nodes)
		if err != nil {
			return nil, fmt.Errorf("read the texts of document blocks: %w", err)
		}
		if err := scanBlockTexts(rows, "block", texts); err != nil {
			return nil, fmt.Errorf("read the texts of document blocks: %w", err)
		}
	}
	for _, item := range itemKinds {
		kind := item.BlockKind()
		nodes := ids[kind]
		if len(nodes) == 0 {
			continue
		}
		tables, err := item.tables()
		if err != nil {
			return nil, err
		}
		if !tables.revisions() {
			if err := messageBlockTexts(ctx, q, kind, nodes, texts); err != nil {
				return nil, err
			}
			continue
		}
		rows, err := q.Query(ctx, `
			select b.node_id, held.text
			from `+tables.blocks+` b
			join lateral (
				select t.text from `+tables.texts+` t
				where t.`+tables.item+` = b.`+tables.item+` and t.block_id = b.block_id
				order by t.revision desc limit 1
			) held on true
			where b.node_id = any($1) and b.position is not null and held.text is not null`, nodes)
		if err != nil {
			return nil, fmt.Errorf("read the texts of %s blocks: %w", string(item), err)
		}
		if err := scanBlockTexts(rows, kind, texts); err != nil {
			return nil, fmt.Errorf("read the texts of %s blocks: %w", string(item), err)
		}
	}
	if nodes := ids[graphstore.KindLensBlock]; len(nodes) > 0 {
		rows, err := q.Query(ctx, `
			select held.id, `+standingText[graphstore.KindLensBlock]("held", "$4")+`
			from graph_nodes held
			where `+heldNodesWhere(graphstore.KindLensBlock),
			StoredKindOf(graphstore.KindLensBlock), nodes, graphstore.StateGone, VisibilityArg(who))
		if err != nil {
			return nil, fmt.Errorf("read the texts of findings: %w", err)
		}
		if err := scanBlockTexts(rows, graphstore.KindLensBlock, texts); err != nil {
			return nil, fmt.Errorf("read the texts of findings: %w", err)
		}
	}
	return texts, nil
}

// scanBlockTexts reads rows of a block's node id and its text into texts under kind, and closes
// them.
func scanBlockTexts(rows pgx.Rows, kind string, texts map[Target]string) error {
	defer rows.Close()
	for rows.Next() {
		var nodeID, held string
		if err := rows.Scan(&nodeID, &held); err != nil {
			return err
		}
		texts[Target{Kind: kind, ID: nodeID}] = held
	}
	return rows.Err()
}

// messageBlockTexts is BlockTexts for the blocks of messages. A message never changes and no
// table records its blocks' texts, so each is re-read from its message's body as
// CurrentItemBlockText reads one (messageBlockText), the body split once however many of its
// blocks are named. A direct message has no address and its blocks resolve to no node
// (loadMessageBlockNodes), so none is read here.
func messageBlockTexts(ctx context.Context, q Queryer, kind string, nodes []string, texts map[Target]string) error {
	rows, err := q.Query(ctx, `
		select b.node_id, b.position, b.text_sha256, b.excerpt, m.id::text, m.body
		from message_blocks b join messages m on m.id = b.message_id
		where b.node_id = any($1) and b.position is not null and m.issue_key is not null`, nodes)
	if err != nil {
		return fmt.Errorf("read the texts of message blocks: %w", err)
	}
	defer rows.Close()
	split := map[string][]itemblocks.Block{}
	for rows.Next() {
		var nodeID, sum, excerpt, messageID, body string
		var position int
		if err := rows.Scan(&nodeID, &position, &sum, &excerpt, &messageID, &body); err != nil {
			return fmt.Errorf("read the texts of message blocks: %w", err)
		}
		blocks, held := split[messageID]
		if !held {
			blocks = itemblocks.Split(body)
			split[messageID] = blocks
		}
		texts[Target{Kind: kind, ID: nodeID}] = messageBlockText(blocks, position, sum, excerpt)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the texts of message blocks: %w", err)
	}
	return nil
}
