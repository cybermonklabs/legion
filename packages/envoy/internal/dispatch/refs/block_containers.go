package refs

import (
	"context"
	"fmt"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
)

// OutermostContainers is, for each of blocks that another block of its document holds, its
// outermost container and that container's current stored text: the block at the document's top
// level that holds it, which the walk up from the block reaches through the block its row's
// parent_block_id names, the block that block's names, and so on, its text the newest
// document_block_texts row of its block, as BlockTexts reads a block's text. The block index
// places each block in the innermost block of its document that holds it and that the renderer
// gives an id (blockParents, placeBlocks): every block but a list item, so a block in a list item
// is held by the list. The outermost container's text and the block's own are slices of one
// markdown, the document's latest, so a reader finds the block's text inside that container's and
// reads off the line the block starts on what its containers write before each of its later lines
// (api's containerIndex.place). The block's own text cannot tell that prefix from what the block
// writes of its own: `- b` then `  - c` is a list whose item b holds a list, or, in a list item two
// columns in, a list of two items. The outermost container starts its line, and so does each of
// its lines after the first, so the line the block starts on is whole in its text, and what every
// container of the block writes on that line is there: the texts of the containers inside it add
// nothing, and are not read.
//
// A block has no container in the answer where it stands at the document's top level, its row
// naming its node and no parent, and where its outermost container cannot be read: the index
// holds no row of it, or none of a block its walk names; its row or the row of a block holding it
// names no node (node_address is null where no pass has named the row's node, and its
// parent_block_id then says nothing); its outermost container has no text, its newest row
// deleting it; or the walk meets a block it met before, which the index, placing each block in a
// block that holds it, never writes. The caller reads such a block as the coverage and the
// results reads read a block: a typed block takes the prefix its closing fence carries, and any
// other block reads as its text stands (api's documentBlockPrefix). The block index's
// reconciliation sets a row's parent_block_id beside its node columns (queueBlockParents), but
// the graph backfill names a row's node and writes no parent_block_id (graphcensus's
// writeBlockBatch), so a row the backfill named that no reconciliation has reached since names its
// node and no parent, and reads as a block at the document's top level. R7 of spec v11 records
// that production held no row so written on 2026-11-28.
//
// It is one recursive statement for every block, which walks up each block's holders through
// document_blocks's primary key, one lookup a step (the lateral's limit keeps the planner from
// joining the whole table instead), and reads the text of each distinct outermost container once,
// through document_block_texts's primary key, however many of the blocks it holds: each row of
// the answer is one container, named and with its text, beside the blocks whose outermost
// container it is. So a request costs one statement however many blocks it reads, no blocks cost
// none, and the answer names each block's container (Outermost.Holders) and holds each
// container's text once (Outermost.Texts), so the blocks of one list carry the list's text once,
// and a caller that places them in it reads that text's bytes once, into one placement index of it
// for all of them, and places each distinct text of them once (api's containerPlacements), each
// test of a line in constant time, within one bound of the work of the tests that pass the hashes
// for the whole request, past which a block takes its range from its document's parse (api's
// placementWorkMax). The walk of a block ends at a block at the document's top level, and at a
// block met twice (the statement's cycle clause), so it ends even where the rows go round.
func OutermostContainers(ctx context.Context, q Queryer, blocks []DocumentBlock) (Outermost, error) {
	if len(blocks) == 0 {
		return Outermost{}, nil
	}
	artifactIDs, blockIDs := make([]string, len(blocks)), make([]string, len(blocks))
	for index, block := range blocks {
		artifactIDs[index], blockIDs[index] = block.ArtifactID, block.BlockID
	}
	rows, err := q.Query(ctx, `
		with recursive chain (ordinality, artifact_id, block_id, parent_block_id, projected, depth) as (
			select asked.ordinality, d.artifact_id, d.block_id, d.parent_block_id, d.node_address is not null, 0
			from unnest($1::uuid[], $2::text[]) with ordinality as asked (artifact_id, block_id)
			join document_blocks d on d.artifact_id = asked.artifact_id and d.block_id = asked.block_id
			union all
			select chain.ordinality, d.artifact_id, d.block_id, d.parent_block_id, d.node_address is not null, chain.depth + 1
			from chain
			cross join lateral (
				select d.artifact_id, d.block_id, d.parent_block_id, d.node_address from document_blocks d
				where d.artifact_id = chain.artifact_id and d.block_id = chain.parent_block_id
				limit 1
			) d
		) cycle block_id set looped using path,
		outermost as (
			select chain.ordinality, chain.artifact_id,
			       min(chain.block_id) filter (where chain.depth > 0 and chain.parent_block_id is null) as block_id
			from chain
			group by chain.ordinality, chain.artifact_id
			having bool_and(chain.projected)
			   and bool_or(chain.depth > 0 and chain.parent_block_id is null)
		)
		select array_agg(outermost.ordinality), outermost.artifact_id::text, outermost.block_id, (
			select t.text from document_block_texts t
			where t.artifact_id = outermost.artifact_id and t.block_id = outermost.block_id
			order by t.version desc limit 1
		)
		from outermost
		group by outermost.artifact_id, outermost.block_id`,
		artifactIDs, blockIDs)
	if err != nil {
		return Outermost{}, fmt.Errorf("read the outermost containers of document blocks: %w", err)
	}
	defer rows.Close()
	answer := Outermost{Holders: make(map[DocumentBlock]DocumentBlock, len(blocks)), Texts: map[DocumentBlock]string{}}
	for rows.Next() {
		var ordinalities []int64
		var holder DocumentBlock
		var text *string
		if err := rows.Scan(&ordinalities, &holder.ArtifactID, &holder.BlockID, &text); err != nil {
			return Outermost{}, fmt.Errorf("read the outermost containers of document blocks: %w", err)
		}
		if text == nil {
			continue
		}
		answer.Texts[holder] = *text
		for _, ordinality := range ordinalities {
			answer.Holders[blocks[ordinality-1]] = holder
		}
	}
	if err := rows.Err(); err != nil {
		return Outermost{}, fmt.Errorf("read the outermost containers of document blocks: %w", err)
	}
	return answer, nil
}

// Outermost is OutermostContainers' answer. Holders is the outermost container of each block that
// has one whose text can be read, and Texts is the text of each such container, held once however
// many of the blocks it holds.
type Outermost struct {
	Holders map[DocumentBlock]DocumentBlock
	Texts   map[DocumentBlock]string
}

// Of is the text of block's outermost container, and held, whether the answer names one for it.
func (o Outermost) Of(block DocumentBlock) (text string, held bool) {
	holder, held := o.Holders[block]
	return o.Texts[holder], held
}

// OutermostContainerAt is the text at version of block's outermost container at that version,
// the block at the document's top level that held it, as OutermostContainers answers a block's
// outermost container now. document_blocks holds only where each block stands now. Where it
// stood at a version is the part_of tuple the block index states out of the block's node in the
// graph store, which it dates by the time of the version whose pass placed the block, the
// version's artifact_versions.created_at, and a move retracts the old far end and asserts the new
// (planBlockParents, parentTuple). So each step of the walk is the newest part_of out of the
// step's block at or before the version's time that still asserts, read as graphstore.AsOfOut
// reads a node's state at a moment, through graph_tuples_as_of_out, until a step reaches the
// document's own artifact; the outermost container is the block of the last step before it, and
// its text is its document_block_texts row of the highest version at or below version, as
// BlockTextAt reads a block's text at a version. The texts of the containers inside it are not
// read, for the reason OutermostContainers gives. It is one recursive statement, whose walk ends
// at the document, and at a block met twice as OutermostContainers' does.
//
// stated is false where the store states no part_of of block at or before the version's time: the
// parent tuples' history starts at the graph store's release, and a version older than it holds
// its blocks' texts and no tuple of where they stood. The caller then reads the containers from
// that version's own markdown. Where the store states one, stated is true, and held is whether
// the walk reached the document through at least one block that held block and the outermost of
// them has a text at the version, which text then is. held is false for a block at the document's
// top level at the version, and where the outermost container cannot be read: the walk reaches no
// document, since the newest part_of at that time retracts, names a block of another document or
// comes back to a block it met, or the outermost container has no text at the version.
func OutermostContainerAt(ctx context.Context, q Queryer, block DocumentBlock, version int) (text string, held, stated bool, err error) {
	var outermost *string
	if err := q.QueryRow(ctx, `
		with recursive pinned as (
			select v.created_at as at, a.ref_key, a.ref_key || '#' as blocks
			from artifact_versions v
			join artifacts a on a.id = v.artifact_id
			where v.artifact_id = $1::uuid and v.number = $3
		), chain (depth, to_kind, to_id) as (
			select 1, held.to_kind, held.to_id
			from pinned
			cross join lateral (`+partOfAsOf(`pinned.blocks || $2`)+`) held
			union all
			select chain.depth + 1, held.to_kind, held.to_id
			from chain
			cross join pinned
			cross join lateral (`+partOfAsOf(`chain.to_id`)+`) held
			where chain.to_kind = $4 and starts_with(chain.to_id, pinned.blocks)
		) cycle to_id set looped using path,
		outermost as (
			select substr(chain.to_id, length(pinned.blocks) + 1) as block_id
			from chain
			cross join pinned
			where chain.to_kind = $4
			  and exists (select 1 from chain reached where reached.to_kind = $6 and reached.to_id = pinned.ref_key)
			order by chain.depth desc
			limit 1
		)
		select exists (
		           select 1 from graph_tuples t
		           where t.from_kind = $4 and t.from_id = (select blocks from pinned) || $2 and t.kind = $5
		             and t.at <= (select at from pinned)
		       ),
		       (
		           select t.text from document_block_texts t
		           where t.artifact_id = $1::uuid and t.block_id = (select block_id from outermost) and t.version <= $3
		           order by t.version desc limit 1
		       )
	`, block.ArtifactID, block.BlockID, version, BlockNodeKind, graphstore.TuplePartOf, "artifact",
	).Scan(&stated, &outermost); err != nil {
		return "", false, false, fmt.Errorf("read the outermost container of block %q of %s at version %d: %w", block.BlockID, block.ArtifactID, version, err)
	}
	if !stated || outermost == nil {
		return "", false, stated, nil
	}
	return *outermost, true, true, nil
}

// partOfAsOf is the lateral OutermostContainerAt reads one step of its walk by: the far end of the
// newest part_of out of the block node at address, an expression of the statement, at or before
// pinned.at that still asserts, with $4 the block node kind and $5 part_of. It is
// graphstore.AsOfOut's reading, the newest tuple of each far end kept where it asserts, narrowed
// to the one far end asserted last, so a store that asserts two at once still gives the walk one.
func partOfAsOf(address string) string {
	return `
				select newest.to_kind, newest.to_id from (
					select distinct on (t.kind, t.to_kind, t.to_id) t.to_kind, t.to_id, t.asserted, t.at, t.seq
					from graph_tuples t
					where t.from_kind = $4 and t.from_id = ` + address + ` and t.kind = $5 and t.at <= pinned.at
					order by t.kind, t.to_kind, t.to_id, t.at desc, t.seq desc
				) newest
				where newest.asserted
				order by newest.at desc, newest.seq desc
				limit 1
			`
}
