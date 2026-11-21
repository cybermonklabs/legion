package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// Documents at a cut. The document routes run their present statements under the cut, and the
// shadows give each document, its versions and its approval as they stood then. A document
// removed since has no row, so a read of it by id answers 404 ARTIFACT_NOT_FOUND and no list holds
// it. The text and the blocks of a document come from its live room, which holds the present
// only. So at a cut the text route and the block routes read no room: they read the document's
// newest version at the cut (versionAtTheCut). Each answer leaves out what the records at the cut
// do not give (leaveOut).

// versionAtTheCutStatement reads the newest version of document $1 that the artifact_versions
// shadow gives at the cut: its markdown, and the document token that its writer stored with it,
// null for a version stored with no token. A document with no version then reads no row.
const versionAtTheCutStatement = `
		select coalesce(v.markdown, ''), v.document_token
		from artifact_versions v where v.artifact_id = $1
		order by v.number desc limit 1`

// blockIndexAtTheCutStatement reads what the block index recorded of the blocks of document $1's
// newest version at the cut, N, which the artifact_versions shadow gives: each block id with the
// latest row of document_block_texts at or before N. Its version is the version in which the text
// of the block last changed at N, which document_blocks.changed_version held then, and its text
// the block's text in N, whose digest document_blocks.text_sha256 held then (refs.BlockTextSum). A
// version writes a row where it changes the text of a block, and a row with no text where it
// deletes the block (refs.PlanBlocks): such a block is not read. The statement reads the rows
// only where document_indexed_versions holds N: no record holds the blocks of a version that no
// block-aware build indexed (migration 0121).
const blockIndexAtTheCutStatement = `
		select t.block_id, t.version, t.text
		from (select max(v.number) as number from artifact_versions v where v.artifact_id = $1) n
		cross join lateral (
			select distinct on (b.block_id) b.block_id, b.version, b.text
			from document_block_texts b
			where b.artifact_id = $1 and b.version <= n.number
			order by b.block_id, b.version desc
		) t
		where t.text is not null
		  and exists (select 1 from document_indexed_versions i where i.artifact_id = $1 and i.version = n.number)`

// blockIndex is what the block index recorded of the blocks of one version, by block id: the
// text of each block, and the version in which that text last changed.
type blockIndex struct {
	texts    map[string]string
	versions map[string]int
}

// versionAtTheCut reads document artifactID's newest version at the cut ctx carries: its markdown
// ("" for a document with no version then) and the document token stored with it (nil for none).
// With blocks it also reads what the block index recorded of that version's blocks
// (blockIndexAtTheCutStatement). One batch sends the statements; each caller names the batch's
// error.
func (s *server) versionAtTheCut(ctx context.Context, artifactID string, blocks bool) (string, *string, blockIndex, error) {
	var markdown string
	var token *string
	var index blockIndex
	batch := &pgx.Batch{}
	batch.Queue(versionAtTheCutStatement, artifactID).QueryRow(func(row pgx.Row) error {
		if err := row.Scan(&markdown, &token); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		return nil
	})
	if blocks {
		index = blockIndex{texts: map[string]string{}, versions: map[string]int{}}
		batch.Queue(blockIndexAtTheCutStatement, artifactID).Query(func(rows pgx.Rows) error {
			for rows.Next() {
				var id, text string
				var version int
				if err := rows.Scan(&id, &version, &text); err != nil {
					return err
				}
				index.texts[id], index.versions[id] = text, version
			}
			return rows.Err()
		})
	}
	if err := s.deps.Store.Pool.SendBatch(ctx, batch).Close(); err != nil {
		return "", nil, blockIndex{}, err
	}
	return markdown, token, index, nil
}

// artifactTextAtTheCut is the markdown and the token of the text route at the cut ctx carries:
// the markdown of document artifactID's newest version then and the document token stored with
// it. A version stored with no token leaves the token out (/token, reason E). With blocks, the
// answer gives the blocks of that markdown (blocksAtTheCut) beside the rendering they index,
// which is the markdown itself for each version this build's renderer wrote.
func (s *server) artifactTextAtTheCut(ctx context.Context, artifactID string, blocks bool) (string, string, *[]model.ArtifactBlock, error) {
	markdown, token, index, err := s.versionAtTheCut(ctx, artifactID, blocks)
	if err != nil {
		return "", "", nil, fmt.Errorf("read the version of document %s at the cut: %w", artifactID, err)
	}
	if token == nil {
		leaveOut(ctx, "/token")
	}
	if !blocks {
		return markdown, deref(token), nil, nil
	}
	rendered, listed, err := docs.VersionBlocks(markdown)
	if err != nil {
		return "", "", nil, err
	}
	if listed == nil {
		listed = []model.ArtifactBlock{}
	}
	blocksAtTheCut(ctx, rendered, listed, index.texts, "/blocks")
	return rendered, deref(token), &listed, nil
}

// artifactBlocksAtTheCut is the answer of a block route at the cut ctx carries: the blocks of
// document artifactID's newest version then (blocksAtTheCut).
func (s *server) artifactBlocksAtTheCut(ctx context.Context, artifactID string) ([]model.ArtifactBlock, error) {
	markdown, _, index, err := s.versionAtTheCut(ctx, artifactID, true)
	if err != nil {
		return nil, fmt.Errorf("read the version of document %s at the cut: %w", artifactID, err)
	}
	rendered, blocks, err := docs.VersionBlocks(markdown)
	if err != nil {
		return nil, err
	}
	blocksAtTheCut(ctx, rendered, blocks, index.texts, "")
	return blocks, nil
}

// blocksAtTheCut completes blocks, the blocks of markdown, which is the rendering of a version, for
// the answer at the cut ctx carries. The answer lists the blocks at place: "" for a block route,
// /blocks for the text route. recorded is the text of each block id that the block index recorded
// for the version (versionAtTheCut).
//
// The markdown of a version names the id of a typed block only, and its parse made a new id for
// each other block. So an untyped block takes the recorded id of its text where exactly one
// recorded id that no typed block of the version holds, and exactly one untyped block of the
// version, have that text. The answer leaves out the id of each other untyped block (reason E):
// no record tells which of two blocks of one text had which id. The answer also leaves out what
// the version does not record (decision 5): the token, the embedding state and the anchored counts
// of each block, and the ids of the rows and the cells of each table, which the block index does
// not record. Each id and token that the answer leaves out is also cleared, so that no id or token
// of the parse reaches a reader.
func blocksAtTheCut(ctx context.Context, markdown string, blocks []model.ArtifactBlock, recorded map[string]string, place string) {
	if len(blocks) == 0 {
		return
	}
	leaveOut(ctx, place+"/*/token", place+"/*/embedding", place+"/*/references")
	untyped := make(map[string]int, len(blocks))
	for index := range blocks {
		if pmdoc.IsTypedBlock(blocks[index].Type) {
			delete(recorded, blocks[index].ID)
			continue
		}
		untyped[markdown[blocks[index].From:blocks[index].To]]++
	}
	holders := make(map[string][]string, len(recorded))
	for id, text := range recorded {
		holders[text] = append(holders[text], id)
	}
	at := elementsOf(place)
	for index := range blocks {
		block := &blocks[index]
		block.Token = ""
		if block.Table != nil {
			tableAtTheCut(ctx, block.Table, at(index))
		}
		if pmdoc.IsTypedBlock(block.Type) {
			continue
		}
		text := markdown[block.From:block.To]
		if ids := holders[text]; len(ids) == 1 && untyped[text] == 1 {
			block.ID = ids[0]
			continue
		}
		block.ID = ""
		leaveOut(ctx, at(index)+"/id")
	}
}

// tableAtTheCut clears the ids of the rows and the cells of table, the table of the block at place,
// and leaves them out: the block index records no row and no cell, and the parse of the version
// made their ids.
func tableAtTheCut(ctx context.Context, table *model.BlockTable, place string) {
	cells := false
	for row := range table.Rows {
		table.Rows[row].ID = ""
		for cell := range table.Rows[row].Cells {
			table.Rows[row].Cells[cell].ID = ""
			cells = true
		}
	}
	if len(table.Rows) > 0 {
		leaveOut(ctx, place+"/table/rows/*/id")
	}
	if cells {
		leaveOut(ctx, place+"/table/rows/*/cells/*/id")
	}
}

// approvalAtTheCut names what a past-time answer leaves out of approval, the approval of the
// document at place: the documents of the open approval request that it awaits, where no record
// at the cut gives them (reason B, model.ArtifactApproval.DocumentsUnknown, which only a read at a
// cut sets).
func approvalAtTheCut(ctx context.Context, approval *model.ArtifactApproval, place string) {
	if approval != nil && approval.DocumentsUnknown {
		leaveOut(ctx, place+"/approval/documents")
	}
}

// approvalsAtTheCut is approvalAtTheCut of each of artifacts, an answer that is the list.
func approvalsAtTheCut(ctx context.Context, artifacts []model.Artifact) {
	at := elementsOf("")
	for index := range artifacts {
		if approval := artifacts[index].Approval; approval != nil && approval.DocumentsUnknown {
			approvalAtTheCut(ctx, approval, at(index))
		}
	}
}
