package refs

import (
	"context"
	"fmt"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
)

// BlockContainers is, for each of blocks, the current stored texts of the blocks of its document
// that hold it, outermost first: the block its row's parent_block_id names, the block that
// block's names, and so on up to a block at the document's top level, each text the newest
// document_block_texts row of its block, as BlockTexts reads a block's text. The block index
// places each block in the innermost block of its document that holds it and that the renderer
// gives an id (blockParents, placeBlocks): every block but a list item, so a block in a list item
// is held by the list. Those texts and the block's own are slices of one markdown, the document's
// latest, so a reader finds each one inside the one before it and the block's text inside the
// last, and reads off the line the block starts on what its containers write before each of its
// later lines (api's placedPrefix). The block's own text cannot tell that prefix from what the
// block writes of its own: `- b` then `  - c` is a list whose item b holds a list, or, in a list
// item two columns in, a list of two items.
//
// A block at the document's top level, whose row names its node and no parent, has an entry that
// is empty and not nil. A block has no entry where its holders cannot be read whole: the index
// holds no row of it; its row or the row of a block holding it names no node (node_address is
// null where the pass that projects the parent has not run for the row, and its parent_block_id
// then says nothing, since that pass sets the three columns together: queueBlockParents); a
// block holding it has no text, its newest row deleting it; or the walk meets a block it met
// before, which the index, placing each block in a block that holds it, never writes. The caller
// reads such a block as its text stands.
//
// It is one recursive statement for every block, which walks up each block's holders through
// document_blocks's primary key and reads each holder's text through document_block_texts's, so a
// request costs one statement however many blocks it reads, and no blocks cost none. The walk of
// a block ends at a block at the document's top level, and at a block met twice (the statement's
// cycle clause), so it ends even where the rows go round.
func BlockContainers(ctx context.Context, q Queryer, blocks []DocumentBlock) (map[DocumentBlock][]string, error) {
	if len(blocks) == 0 {
		return nil, nil
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
			join document_blocks d on d.artifact_id = chain.artifact_id and d.block_id = chain.parent_block_id
		) cycle block_id set looped using path
		select chain.ordinality,
		       coalesce(array_agg(held.text order by chain.depth desc) filter (where chain.depth > 0), '{}')
		from chain
		left join lateral (
			select t.text from document_block_texts t
			where chain.depth > 0 and t.artifact_id = chain.artifact_id and t.block_id = chain.block_id
			order by t.version desc limit 1
		) held on true
		group by chain.ordinality
		having bool_and(chain.projected)
		   and bool_or(chain.parent_block_id is null)
		   and count(*) filter (where chain.depth > 0 and held.text is null) = 0`,
		artifactIDs, blockIDs)
	if err != nil {
		return nil, fmt.Errorf("read the blocks holding document blocks: %w", err)
	}
	defer rows.Close()
	containers := make(map[DocumentBlock][]string, len(blocks))
	for rows.Next() {
		var ordinality int64
		var texts []string
		if err := rows.Scan(&ordinality, &texts); err != nil {
			return nil, fmt.Errorf("read the blocks holding document blocks: %w", err)
		}
		if texts == nil {
			texts = []string{}
		}
		containers[blocks[ordinality-1]] = texts
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the blocks holding document blocks: %w", err)
	}
	return containers, nil
}

// BlockContainersAt is the texts at version of the blocks of block's document that held block at
// that version, outermost first, as BlockContainers answers the blocks holding a block now.
// document_blocks holds only where each block stands now. Where it stood at a version is the
// part_of tuple the block index states out of the block's node in the graph store, which it
// dates by the time of the version whose pass placed the block, the version's
// artifact_versions.created_at, and a move retracts the old far end and asserts the new
// (planBlockParents, parentTuple). So each step of the walk is the newest part_of out of the
// step's block at or before the version's time that still asserts, read as graphstore.AsOfOut
// reads a node's state at a moment, through graph_tuples_as_of_out, until a step reaches the
// document's own artifact; and each holder's text is its document_block_texts row of the highest
// version at or below version, as BlockTextAt reads a block's text at a version. It is one
// recursive statement, whose walk ends at the document, and at a block met twice as
// BlockContainers' does.
//
// stated is false where the store states no part_of of block at or before the version's time: the
// parent tuples' history starts at the graph store's release, and a version older than it holds
// its blocks' texts and no tuple of where they stood. The caller then reads the containers from
// that version's own markdown. Where the store states one, stated is true, and texts is nil where
// the holders cannot be read whole: the walk reaches no document, since the newest part_of at that
// time retracts, names a block of another document or comes back to a block it met, or a block
// holding it has no text at the version. A block at the document's top level at the version
// answers an empty texts that is not nil.
func BlockContainersAt(ctx context.Context, q Queryer, block DocumentBlock, version int) (texts []string, stated bool, err error) {
	var whole bool
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
		holders as (
			select chain.depth, (
				select t.text from document_block_texts t
				where t.artifact_id = $1::uuid and t.block_id = substr(chain.to_id, length(pinned.blocks) + 1)
				  and t.version <= $3
				order by t.version desc limit 1
			) as text
			from chain
			cross join pinned
			where chain.to_kind = $4
		)
		select exists (
		           select 1 from pinned
		           join graph_tuples t on t.from_kind = $4 and t.from_id = pinned.blocks || $2 and t.kind = $5
		             and t.at <= pinned.at
		       ),
		       exists (select 1 from chain cross join pinned where chain.to_kind = $6 and chain.to_id = pinned.ref_key)
		         and not exists (select 1 from holders where holders.text is null),
		       coalesce((select array_agg(holders.text order by holders.depth desc) from holders where holders.text is not null), '{}')
	`, block.ArtifactID, block.BlockID, version, BlockNodeKind, graphstore.TuplePartOf, "artifact",
	).Scan(&stated, &whole, &texts); err != nil {
		return nil, false, fmt.Errorf("read the blocks holding block %q of %s at version %d: %w", block.BlockID, block.ArtifactID, version, err)
	}
	if !stated || !whole {
		return nil, stated, nil
	}
	if texts == nil {
		texts = []string{}
	}
	return texts, true, nil
}

// partOfAsOf is the lateral BlockContainersAt reads one step of its walk by: the far end of the
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
