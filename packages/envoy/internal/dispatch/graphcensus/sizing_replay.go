package graphcensus

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/schemanodes"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// replayVersionsQuery is every version the block index indexed whose row was written inside the
// window, with the version that document's index indexed before it, each with its markdown and
// its document's ref_key. Ahead of each windowed version come its document_block_texts rows, each
// with the text the block's row before it held. The rows come in document and version order, a
// version's texts before the version itself, so the replay holds one document's last version and
// one version's texts at a time.
//
// A version's row is dated once, when it is written, and a document's versions are numbered in
// the order they are written, so the versions inside the window are each document's last ones
// and the one before the first of them is the one whose next version lies inside it.
func replayVersionsQuery(rooms bool) string {
	return `
	with indexed as (
		select i.artifact_id, i.version, v.created_at > ` + sizingWindow + ` as inside,
		       coalesce(lead(v.created_at > ` + sizingWindow + `)
		                over (partition by i.artifact_id order by i.version), false) as followed_inside
		from document_indexed_versions i
		join artifact_versions v on v.artifact_id = i.artifact_id and v.number = i.version
	),
	replayed as (select artifact_id, version, inside from indexed where inside or followed_inside)
	select r.artifact_id::text, r.version, 1, a.ref_key, v.markdown, null::text, null::text, null::text
	from replayed r
	join artifact_versions v on v.artifact_id = r.artifact_id and v.number = r.version
	join artifacts a on a.id = r.artifact_id
	union all
	select r.artifact_id::text, r.version, 0, null, null, t.block_id, t.text, earlier.text
	from replayed r
	join document_block_texts t on ` + documentTexts("t", "r.artifact_id", rooms) + ` and t.version = r.version
	left join lateral (
		select p.text from document_block_texts p
		where p.artifact_id = t.artifact_id and p.block_id = t.block_id and p.version < t.version
		order by p.version desc limit 1
	) earlier on true
	where r.inside
	order by 1, 2, 3, 6`
}

// replayedParents is what replayParents read: how many blocks the windowed versions moved to
// another parent, over how many version rows, and how many of those rows did not parse.
type replayedParents struct{ moved, read, unreadable int64 }

// replayParents counts the blocks each version the block index indexed inside the window placed
// under another parent than the version it indexed before did, which is how many part_of moves
// that index stated (refs' planBlockParents). No table written before the store holds a
// block's parent, so each version's markdown is parsed, against the block schema the store's type
// nodes build (installStoreBlockSchema), and its blocks placed by the walk the index places them
// by (refs.PlaceBlocks).
//
// A version's markdown names no paragraph's id - a parse mints a fresh one for every block that
// writes none - so the two versions' blocks are matched by their text, which is the body the index
// sums (markdown[From:To]): a block the later version left alone holds one text in both, and a
// block it wrote or dropped is the one its document_block_texts row names, under its earlier text
// in the version before and its new text in the later one (blockIdentities). Blocks of one
// document with equal text are matched in document order, so the count is exact except there. A
// version that does not parse is counted as read and unreadable, and neither it nor the version
// after it is compared, as the index wrote no blocks for it. rooms says the database holds the
// issue rooms' tables (documentTexts).
func replayParents(ctx context.Context, tx pgx.Tx, rooms bool) (replayedParents, error) {
	if err := installStoreBlockSchema(ctx, tx); err != nil {
		return replayedParents{}, err
	}
	rows, err := tx.Query(ctx, replayVersionsQuery(rooms))
	if err != nil {
		return replayedParents{}, fmt.Errorf("read the indexed versions of the window: %w", err)
	}
	defer rows.Close()
	var done replayedParents
	var document string
	var earlier *placedVersion
	var changes []blockTextChange
	for rows.Next() {
		var artifactID string
		var version, part int
		var refKey, markdown, blockID, text, before *string
		if err := rows.Scan(&artifactID, &version, &part, &refKey, &markdown, &blockID, &text, &before); err != nil {
			return replayedParents{}, fmt.Errorf("read an indexed version of the window: %w", err)
		}
		if artifactID != document {
			document, earlier, changes = artifactID, nil, changes[:0]
		}
		if part == 0 {
			changes = append(changes, blockTextChange{id: *blockID, text: text, earlier: before})
			continue
		}
		done.read++
		later := placeVersion(*refKey, markdown)
		if later == nil {
			done.unreadable++
		}
		if earlier != nil && later != nil {
			done.moved += movedBlocks(earlier, later, changes)
		}
		earlier, changes = later, changes[:0]
	}
	if err := rows.Err(); err != nil {
		return replayedParents{}, fmt.Errorf("read the indexed versions of the window: %w", err)
	}
	return done, nil
}

// installStoreBlockSchema installs the block schema the store's type nodes build in the census's
// own snapshot (schemanodes.Read), which is the schema the block index parsed each version
// against: the server installs it at boot and again after every type write
// (schemanodes.InstallFromStore), so a typed block the store declares and the file this binary
// embeds lacks is a block the index read, and a replay parsing against the file would read its
// version as unreadable and count none of its moves. pmdoc parses against the installed schema
// alone, so the value goes into the process (pmdoc.Install), as it does in every subcommand that
// parses a document; the install writes nothing to the database. It is read here, on a savepoint
// of the census's transaction (censusSnapshot), and not by InstallFromStore, whose read is a
// transaction of its own: that would take another snapshot than the versions the replay reads,
// and the census's one connection holds its transaction open.
//
// A database not migrated to this build (storeAbsence) holds no type node this build reads: the
// store does not exist on it yet, or it is the store of a release before, whose graph_nodes this
// build's reader does not read. The sizing takes its readings there too (commentWriterPeople),
// so there the replay parses against the schema the process holds, which in the census subcommand
// is the embedded file: it installs no other.
func installStoreBlockSchema(ctx context.Context, tx pgx.Tx) error {
	absent, err := storeAbsence(ctx, tx)
	if err != nil {
		return err
	}
	held := absent == ""
	if held {
		if err := tx.QueryRow(ctx, `select exists (select 1 from graph_nodes where kind = $1)`,
			schemanodes.KindSupertag).Scan(&held); err != nil {
			return fmt.Errorf("look up the store's type nodes: %w", err)
		}
	}
	if !held {
		return nil
	}
	value, err := schemanodes.Read(ctx, censusSnapshot{tx: tx})
	if err != nil {
		return fmt.Errorf("read the block schema the store's type nodes build: %w", err)
	}
	if err := pmdoc.Install(value); err != nil {
		return fmt.Errorf("install the block schema the store's type nodes build: %w", err)
	}
	return nil
}

// censusSnapshot opens schemanodes.Read's transaction as a savepoint of the census's own
// (pgx.Tx.Begin), so the read takes the census's snapshot, and its end, a rollback, rolls back to
// the savepoint and leaves the census's transaction open. The options Read asks for, a
// repeatable-read and read-only transaction, are the census's own already.
type censusSnapshot struct{ tx pgx.Tx }

func (s censusSnapshot) BeginTx(ctx context.Context, _ pgx.TxOptions) (pgx.Tx, error) {
	return s.tx.Begin(ctx)
}

// blockTextChange is one document_block_texts row of a version: the block it names, the text the
// version gave it (nil where the version dropped it), and the text its row before held (nil where
// there was none, or where that row recorded the block deleted).
type blockTextChange struct {
	id            string
	text, earlier *string
}

// placedVersion is one version as the replay reads it: its canonical markdown, its unique
// blocks, the index among them of each block the index places, in document order, and beside
// each of those the index of the block holding it, -1 for the document (refs.PlaceBlocks).
type placedVersion struct {
	markdown        string
	blocks          []model.ArtifactBlock
	placed, parents []int
}

// placeVersion parses one version's markdown and places its blocks, and answers nil for a
// version with no markdown or one that does not parse. The blocks are those of the render, as
// the index takes them from it, so their ranges index into the render's markdown.
func placeVersion(refKey string, markdown *string) *placedVersion {
	if markdown == nil {
		return nil
	}
	tree, err := pmdoc.ParseRendering(*markdown)
	if err != nil {
		return nil
	}
	rendered, offsets, err := pmdoc.RenderWithBlockOffsets(tree)
	if err != nil {
		return nil
	}
	blocks := make([]model.ArtifactBlock, len(offsets))
	for index, offset := range offsets {
		blocks[index] = model.ArtifactBlock{ID: offset.ID, Type: offset.Type, From: offset.From, To: offset.To}
	}
	nodes, placed, parents := refs.PlaceBlocks(refKey, blocks)
	return &placedVersion{markdown: rendered, blocks: nodes, placed: placed, parents: parents}
}

// blockIdentity is a block as the replay matches it across two versions: the id a
// document_block_texts row names it by, or, for a block the later version left alone, its text
// and its place among the blocks of one version holding that text.
type blockIdentity struct {
	id         string
	text       string
	occurrence int
}

// blockPlacement is where a block sits: inside the block parent names, or, where held is false,
// in the document itself.
type blockPlacement struct {
	parent blockIdentity
	held   bool
}

// movedBlocks counts the blocks both versions hold whose placement the later one moved. changes
// are the later version's document_block_texts rows, in block id order.
func movedBlocks(earlier, later *placedVersion, changes []blockTextChange) int64 {
	wasNamed, nowNamed := map[string][]string{}, map[string][]string{}
	for _, change := range changes {
		if change.earlier != nil {
			wasNamed[*change.earlier] = append(wasNamed[*change.earlier], change.id)
		}
		if change.text != nil {
			nowNamed[*change.text] = append(nowNamed[*change.text], change.id)
		}
	}
	was := blockIdentities(earlier, wasNamed)
	placedBefore := make(map[blockIdentity]blockPlacement, len(earlier.placed))
	for index, block := range earlier.placed {
		placedBefore[was[block]] = placementOf(was, earlier.parents[index])
	}
	now := blockIdentities(later, nowNamed)
	var moved int64
	for index, block := range later.placed {
		before, both := placedBefore[now[block]]
		if both && placementOf(now, later.parents[index]) != before {
			moved++
		}
	}
	return moved
}

// blockIdentities names each placed block of one version as movedBlocks matches it with the
// other version, at the block's index among the version's blocks. named maps a text to the ids
// of the document_block_texts rows that name a block holding it in this version; the blocks of
// one text take those ids in document order, and every block left over, which the later version
// left alone, is its text and its place among the blocks of this version holding that text. The
// text is a slice of the version's markdown, so no block's text is copied.
func blockIdentities(version *placedVersion, named map[string][]string) []blockIdentity {
	identities := make([]blockIdentity, len(version.blocks))
	taken := map[string]int{}
	left := map[string]int{}
	for _, index := range version.placed {
		block := &version.blocks[index]
		text := version.markdown[block.From:block.To]
		if ids := named[text]; taken[text] < len(ids) {
			identities[index] = blockIdentity{id: ids[taken[text]]}
			taken[text]++
			continue
		}
		identities[index] = blockIdentity{text: text, occurrence: left[text]}
		left[text]++
	}
	return identities
}

// placementOf is the placement of a block whose parent is the block at parent among the
// version's blocks, -1 for the document.
func placementOf(identities []blockIdentity, parent int) blockPlacement {
	if parent < 0 {
		return blockPlacement{}
	}
	return blockPlacement{parent: identities[parent], held: true}
}

// documentTexts is true of the document_block_texts row aliased texts while it is a text of a
// block of the document the uuid expression document names: a row under that document, or, where
// the database holds issue rooms (rooms), for a document of an issue room, a row under the room's
// tree artifact, whose rows carry each document's own version numbers, of a block that document
// holds (refs.BlockDocumentSQL). So a version of a room's document is read with its own blocks'
// texts alone, and its indexed versions are its own (T12a of the issue-tree plan).
func documentTexts(texts, document string, rooms bool) string {
	if !rooms {
		return texts + `.artifact_id = ` + document
	}
	return `(` + texts + `.artifact_id = any(array[` + document + `, ` + refs.DocumentRoomSQL(document) + `]) and ` +
		refs.BlockDocumentSQL(texts) + ` = ` + document + `)`
}
