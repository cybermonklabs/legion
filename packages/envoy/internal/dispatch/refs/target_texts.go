package refs

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
)

// DocumentBlock is where a document's block stands, as document_blocks records it: the id of its
// document and its own block id.
type DocumentBlock struct {
	ArtifactID, BlockID string
}

// BlockTexts is the whole current text of each block targets names, by target: a document's
// block (kind block, addressed '<document ref_key>#<block id>') as its latest indexed version
// holds it (CurrentBlockText), and a block of a comment, of a message or of an ask (the item's
// block kind, ItemKind.BlockKind) as CurrentItemBlockText reads it. A target of any other kind
// is passed over, and a block no current version or revision holds has no entry, which is the
// block TargetNames answers nothing for either. Its second answer is, by target, where each
// document's block it holds a text of stands (DocumentBlock), read by the statement that reads
// the text; it holds no other kind of block.
//
// It is TargetNames' companion for a reader that draws a block whole rather than by its name,
// which stops at the block's stored excerpt: each kind is read in one statement whatever the
// number of targets, so a batch costs a statement per kind of block it names and none for a
// kind it does not.
func BlockTexts(ctx context.Context, q Queryer, targets []Target) (map[Target]string, map[Target]DocumentBlock, error) {
	ids := map[string][]string{}
	for _, target := range targets {
		ids[target.Kind] = append(ids[target.Kind], target.ID)
	}
	texts := make(map[Target]string, len(targets))
	var documents map[Target]DocumentBlock
	if nodes := ids["block"]; len(nodes) > 0 {
		rows, err := q.Query(ctx, `
			select d.node_id, d.artifact_id::text, d.block_id, held.text
			from document_blocks d
			join lateral (
				select t.text from document_block_texts t
				where t.artifact_id = d.artifact_id and t.block_id = d.block_id
				order by t.version desc limit 1
			) held on true
			where d.node_id = any($1) and held.text is not null`, nodes)
		if err != nil {
			return nil, nil, fmt.Errorf("read the texts of document blocks: %w", err)
		}
		documents = make(map[Target]DocumentBlock, len(nodes))
		if err := scanDocumentBlockTexts(rows, texts, documents); err != nil {
			return nil, nil, fmt.Errorf("read the texts of document blocks: %w", err)
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
			return nil, nil, err
		}
		if !tables.revisions() {
			if err := messageBlockTexts(ctx, q, kind, nodes, texts); err != nil {
				return nil, nil, err
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
			return nil, nil, fmt.Errorf("read the texts of %s blocks: %w", string(item), err)
		}
		if err := scanBlockTexts(rows, kind, texts); err != nil {
			return nil, nil, fmt.Errorf("read the texts of %s blocks: %w", string(item), err)
		}
	}
	return texts, documents, nil
}

// scanDocumentBlockTexts reads rows of a document block's node id, its document's id, its block
// id and its text into texts and documents under kind block, and closes them.
func scanDocumentBlockTexts(rows pgx.Rows, texts map[Target]string, documents map[Target]DocumentBlock) error {
	defer rows.Close()
	for rows.Next() {
		var nodeID, held string
		var document DocumentBlock
		if err := rows.Scan(&nodeID, &document.ArtifactID, &document.BlockID, &held); err != nil {
			return err
		}
		target := Target{Kind: "block", ID: nodeID}
		texts[target], documents[target] = held, document
	}
	return rows.Err()
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

// messageBlockTexts is BlockTexts for the blocks of messages. No table records a message's
// block texts, so each is re-read from its message's body as it stands now - an edit replaces
// the body and a delete clears it - as CurrentItemBlockText reads one (messageBlockText), the
// body split once however many of its blocks are named. A direct message has no address and its blocks resolve to no node
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
