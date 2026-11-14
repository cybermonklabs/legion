package docs

// What a rebuild keeps of the document it replaces, beside its text: the ids of its blocks, the
// anchor marks of its open comments, suggestions and asks, and the margin records a browser shows
// beside it (RebuildDocument). A history that loads holds all three, and a rebuild from its own
// text keeps them by building from its tree and its marks map; a rebuild from markdown has the
// text alone, so it takes back what the history or the rows still name: from a history that loads,
// what an upload over that history keeps (keptReplacement); from one that does not, what the rows
// recorded of it (restoredFromRows).

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// marginRecords is every margin record history holds, under the key it holds it at (a comment's or
// suggestion's mark id, ProjectMark), as it holds it.
func marginRecords(history *crdt.Doc) map[string]any {
	records := make(map[string]any)
	history.GetMap(marksMapName).ForEach(func(key string, value any) { records[key] = value })
	return records
}

// keptReplacement is markdown, a caller's replacement for current, the tree of a history that
// loads, as an upload over current writes it (ReplaceText): parsed over current, so each untyped
// block the markdown leaves unchanged keeps its id (parseReplacing, pmdoc.ParseForWrite's
// carryBlockIDs), with each open anchor current holds marked again where its quote now stands,
// nearest where it stood (heldReanchors, reanchored). Markdown that leaves an anchor's quote only
// where its mark may not stand is refused, as the upload of it is.
func (s *Service) keptReplacement(ctx context.Context, q Queryer, artifactID string, current *pmdoc.Node, markdown string) (*pmdoc.Node, error) {
	target, err := parseReplacing(current, markdown)
	if err != nil {
		return nil, err
	}
	anchors, err := s.openAnchoredMarks(ctx, q, artifactID)
	if err != nil {
		return nil, err
	}
	return reanchored(target, heldReanchors(current, anchors), false)
}

// restoredFromRows is tree, the text of a history that does not load, read from markdown alone -
// its latest version's, or a caller's - with what the rows still record of the history: each
// untyped block takes the id the block index recorded for a block of its type and text
// (carryIndexedBlockIDs), each open anchor is marked again where its quote stands nearest the
// block its row names (rowReanchors), and each comment and suggestion so marked takes its margin
// record back, built from its rows as the comment routes build it (rowMarginRecords). What no row
// records - a block the index never saw, an anchor whose quote the text no longer holds or holds
// only where its mark may not stand, a resolved thread's record - is left out, so the rebuild of
// such a history is never refused for what it cannot restore.
func (s *Service) restoredFromRows(ctx context.Context, tx pgx.Tx, artifactID string, tree *pmdoc.Node) (*pmdoc.Node, map[string]any, error) {
	if err := carryIndexedBlockIDs(ctx, tx, artifactID, tree); err != nil {
		return nil, nil, err
	}
	anchors, err := s.openAnchoredMarks(ctx, tx, artifactID)
	if err != nil {
		return nil, nil, err
	}
	reanchors, err := rowReanchors(ctx, tx, tree, anchors)
	if err != nil {
		return nil, nil, err
	}
	if tree, err = reanchored(tree, reanchors, true); err != nil {
		return nil, nil, err
	}
	margin, err := rowMarginRecords(ctx, tx, tree, anchors)
	if err != nil {
		return nil, nil, err
	}
	return tree, margin, nil
}

// carryIndexedBlockIDs gives each untyped block of tree, a document parsed from markdown that names
// no untyped block's id, the id the block index records for a block of artifactID of the same type
// and the same text (document_blocks, the sum refs.IndexBlocks takes of the block's rendering),
// each recorded id to one block, in document order. The index holds the blocks of the latest
// version it indexed, so a block unchanged since then takes its id back and the anchors, asks and
// references that name it name it still. A typed block's id is written in the markdown and is part
// of what the block is, so it is never changed; an untyped block whose text no recorded block
// shares keeps the fresh id its parse gave it, as does each of several alike past the ids recorded
// for their text.
func carryIndexedBlockIDs(ctx context.Context, q Queryer, artifactID string, tree *pmdoc.Node) error {
	type indexedBlock struct{ kind, sum string }
	rows, err := q.Query(ctx, `
		select block_id, type, text_sha256 from document_blocks where artifact_id = $1 order by block_id
	`, artifactID)
	if err != nil {
		return fmt.Errorf("read the document's indexed blocks: %w", err)
	}
	indexed := make(map[indexedBlock][]string)
	recorded := make(map[string]bool)
	for rows.Next() {
		var id string
		var block indexedBlock
		if err := rows.Scan(&id, &block.kind, &block.sum); err != nil {
			rows.Close()
			return fmt.Errorf("read the document's indexed blocks: %w", err)
		}
		indexed[block] = append(indexed[block], id)
		recorded[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the document's indexed blocks: %w", err)
	}
	if len(recorded) == 0 {
		return nil
	}
	markdown, offsets, err := renderTreeWithOffsets(tree)
	if err != nil {
		return err
	}
	held := make(map[string]bool)
	for _, block := range offsets {
		if recorded[block.ID] {
			held[block.ID] = true
		}
	}
	renamed := make(map[string]string)
	for _, block := range offsets {
		if held[block.ID] || pmdoc.IsTypedBlock(block.Type) {
			continue
		}
		key := indexedBlock{kind: block.Type, sum: refs.BlockTextSum(markdown[block.From:block.To])}
		ids := indexed[key]
		for len(ids) > 0 && held[ids[0]] {
			ids = ids[1:]
		}
		if len(ids) == 0 {
			delete(indexed, key)
			continue
		}
		renamed[block.ID], held[ids[0]], indexed[key] = ids[0], true, ids[1:]
	}
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if id, ok := node.Attrs[pmdoc.BlockIDAttr].(string); ok {
			if carried, ok := renamed[id]; ok {
				node.Attrs[pmdoc.BlockIDAttr] = carried
			}
		}
		return true
	})
	return nil
}

// rowReanchors is each of anchors, rows whose marks no tree holds any more, as reanchored marks it
// again in tree: nearest the start of the block its row names, where tree holds that block, and
// otherwise nearest the document's start; carrying the mark its writer made (MarkSpec's mark: its
// id and who made it, and a suggestion's kind as the server writes one). A row anchored on no
// quote is left out.
func rowReanchors(ctx context.Context, q Queryer, tree *pmdoc.Node, anchors []anchoredMark) ([]reanchor, error) {
	authors, err := anchorAuthors(ctx, q, anchors)
	if err != nil {
		return nil, err
	}
	valid, err := pmdoc.ValidateDocument(tree, "restore anchors")
	if err != nil {
		return nil, err
	}
	reanchors := make([]reanchor, 0, len(anchors))
	for _, mark := range anchors {
		author, known := authors[mark.id]
		if mark.anchor.Quote == "" || !known {
			continue
		}
		var near pmdoc.Range
		if mark.anchor.BlockID != nil {
			if block, err := valid.BlockRange(*mark.anchor.BlockID); err == nil {
				near = block
			}
		}
		spec := MarkSpec{Kind: MarkKind(mark.markType), ID: mark.anchor.MarkID, By: author}
		reanchors = append(reanchors, reanchor{mark: mark, range_: near, attrs: spec.pmMark().Attrs})
	}
	return reanchors, nil
}

// anchorAuthors is who made each of anchors' rows, by row id: an ask's author, and a comment's or
// suggestion's.
func anchorAuthors(ctx context.Context, q Queryer, anchors []anchoredMark) (map[string]model.Actor, error) {
	var asks, comments []string
	for _, mark := range anchors {
		if mark.markType == string(MarkAsk) {
			asks = append(asks, mark.id)
		} else {
			comments = append(comments, mark.id)
		}
	}
	authors := make(map[string]model.Actor, len(anchors))
	for _, table := range []struct {
		name string
		ids  []string
	}{{"asks", asks}, {"comments", comments}} {
		if len(table.ids) == 0 {
			continue
		}
		rows, err := q.Query(ctx, `select id::text, author from `+table.name+` where id = any($1::uuid[])`, table.ids)
		if err != nil {
			return nil, fmt.Errorf("read the %s' authors: %w", table.name, err)
		}
		for rows.Next() {
			var id string
			var encoded []byte
			if err := rows.Scan(&id, &encoded); err != nil {
				rows.Close()
				return nil, fmt.Errorf("read the %s' authors: %w", table.name, err)
			}
			var author model.Actor
			if err := json.Unmarshal(encoded, &author); err != nil {
				rows.Close()
				return nil, fmt.Errorf("decode the author of %s %s: %w", table.name, id, err)
			}
			authors[id] = author
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("read the %s' authors: %w", table.name, err)
		}
	}
	return authors, nil
}

// marginRepliesQuery is every reply under each thread root of $1, with the root it is under: the
// walk the comment routes read a thread by (api's replyChainQuery), oldest first.
const marginRepliesQuery = `
		with recursive replies as (
			select c.id, c.author, c.body, c.created_at, c.reply_to as root
			from comments c where c.reply_to = any($1::uuid[])
			union all
			select c.id, c.author, c.body, c.created_at, r.root
			from comments c join replies r on c.reply_to = r.id
		)
		select root::text, author, body, created_at from replies order by created_at, id`

// rowMarginRecords is the margin record of each comment and suggestion of anchors whose mark tree
// holds, under its mark id, built from its row and its thread's replies as the comment routes
// build it (CommentMarkRecord, ProjectMark), a suggestion's kind the one its mark carries.
func rowMarginRecords(ctx context.Context, tx pgx.Tx, tree *pmdoc.Node, anchors []anchoredMark) (map[string]any, error) {
	keys := make([]pmdoc.MarkRef, 0, len(anchors))
	for _, mark := range anchors {
		if mark.markType != string(MarkAsk) {
			keys = append(keys, mark.markRef())
		}
	}
	found := pmdoc.FindMarks(tree, keys)
	var ids []string
	kinds := make(map[string]string)
	for _, mark := range anchors {
		hit, marked := found[mark.markRef()]
		if mark.markType == string(MarkAsk) || !marked {
			continue
		}
		ids = append(ids, mark.id)
		if kind, ok := hit.Attrs["kind"].(string); ok {
			kinds[mark.id] = kind
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	comments, _, err := loadAnchorRefreshedComments(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	replies := make(map[string][]model.Comment, len(ids))
	rows, err := tx.Query(ctx, marginRepliesQuery, ids)
	if err != nil {
		return nil, fmt.Errorf("read the anchored threads' replies: %w", err)
	}
	for rows.Next() {
		var root string
		var author []byte
		var reply model.Comment
		if err := rows.Scan(&root, &author, &reply.Body, &reply.CreatedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read the anchored threads' replies: %w", err)
		}
		if err := json.Unmarshal(author, &reply.Author); err != nil {
			rows.Close()
			return nil, fmt.Errorf("decode a reply's author: %w", err)
		}
		replies[root] = append(replies[root], reply)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the anchored threads' replies: %w", err)
	}
	margin := make(map[string]any, len(ids))
	for _, id := range ids {
		comment, ok := comments[id]
		if !ok || comment.Anchor == nil {
			continue
		}
		record, err := toPlain(CommentMarkRecord(comment, replies[id], kinds[id]))
		if err != nil {
			return nil, err
		}
		margin[comment.Anchor.MarkID] = record
	}
	return margin, nil
}

// sortedKeys is m's keys in order, so a seed written from a map writes the same update every time.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
