package grapherase

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// ownDocument is the document a block node is a block of. Its versions, its editing state, its
// search rows and its windows hold the block's text because the block is part of it, so they
// derive from the block (R5); no other node has a document of its own.
type ownDocument struct {
	artifact, refKey, issue, project string
	// spec is whether the document is its issue's spec (artifacts.is_primary), whose latest
	// version the issue's own search vector and embedding row hold.
	spec bool
}

// specIssue is the issue whose search vector and embedding row hold the document's text, or
// empty where the document is no issue's spec.
func (d *ownDocument) specIssue() string {
	if d == nil || !d.spec {
		return ""
	}
	return d.issue
}

// embeddingRow is one row of the embedding store, by its key.
type embeddingRow struct{ kind, id string }

// erasedRead is one read of the node and of every row that derives from it: its texts, its own
// document, the documents whose margin holds it, the blocks of its document that hold it, the
// versions, events and embedding rows the run deletes or rewrites, and the owner rows those
// stand under. It is what the listing is made of, and what apply writes, so a confirmed run
// writes the rows it listed and no other.
type erasedRead struct {
	texts    []string
	document *ownDocument
	blockID  string
	project  string
	// margins are the documents a comment's body stands in the margin of, whose editing state
	// therefore derives from the comment (marginDocuments). A node of any other kind has none.
	margins []marginDocument
	// ancestors are the blocks that hold the node's block and whose stored text or excerpt the
	// redaction rewrites: a container's text is cut from the document across the blocks inside
	// it, so it quotes the node's own text as well as its siblings'.
	ancestors []string
	// blockTexts and excerpts are those rows as the redaction leaves them, computed by the read
	// the listing is made of and written back by redactBlocks.
	blockTexts []redactedBlockText
	excerpts   []redactedExcerpt
	// parts are the nodes that are parts of an erased comment or message - its block nodes and
	// its delivery attempts - each with the seqs of the tuples on both sides of it, which go with
	// the item (readItemParts).
	parts []erasedPart
	// tupleSeqs are the seqs of the tuples the listing names, which the delete takes and no
	// others (graphstore.EraseNode).
	tupleSeqs []int64
	// readState are the node's rows of the six read-state tables, by table, and readStateScope
	// the predicate that found them (readReadState): a person's every row, an item's the rows
	// that name it. replied are the user_threads rows a reply the erase removes reached in the
	// thread it was written in, as the erase rewrites them (readRepliedRows).
	readState      []readStateRows
	readStateScope readStateScope
	replied        []repliedRow
	artifacts      []string
	numbers        []int32
	events         []redactedEvent
	embedded       []embeddingRow
	owners         events.OwnerRows
	listing        listing
}

// read reads the node as the erase would remove it and every row that derives from it, and
// writes the listing of all of it and its digest onto the Erasure. It locks nothing and writes
// nothing, so a run without --confirm reads it in a read-only transaction, and a confirmed run
// reads it once it holds its locks (lockOwnersThenNode).
//
// What derives from a node is fixed by the node, never by a search for its bytes: its tuples,
// its projection rows and the rows that cannot exist without them, its own embedding rows and the
// events about it, for a block its own document's versions, editing state, search rows and
// windows and the excerpts cut from it, and for a comment the editing state of every document
// whose margin record holds its body (marginDocuments). A row anywhere else that holds the same
// bytes derives from another node - another issue's spec quoting a comment's `Yes`, another
// person's comment, a sibling block of the same document - and is that node's text: the run lists
// it (readLeft) and changes nothing in it.
func (e *Erasure) read(ctx context.Context, tx pgx.Tx) (erasedRead, error) {
	var read erasedRead
	if err := tx.QueryRow(ctx, `select supertag from graph_nodes where kind = $1 and id = $2`,
		e.Kind, e.ID).Scan(&e.Supertag); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return erasedRead{}, fmt.Errorf("the store holds no node %s:%s", e.Kind, e.ID)
		}
		return erasedRead{}, fmt.Errorf("read node %s:%s: %w", e.Kind, e.ID, err)
	}
	// A node whose text is in its own row is refused while that row stands (Erase). A message is
	// not, once its row names its node: that row is then one of its projection rows, which the run
	// deletes with it, and the run marks its body in every event that carried it. A message row a
	// build before this one wrote names no node until the backfill marks it, so the run would find
	// it by no address and leave it and its text: that message is refused until its row names its
	// node. A confirmed run reads this under its lock of the node's row (lockOwnersThenNode), and a
	// create of this build writes the node before the row in one transaction, holding the node's
	// row until it commits: a create that got there first has committed its row when this read
	// runs, and one that comes after waits for the erase. The run without --confirm reads it in
	// its snapshot and locks nothing, and the confirmed run reads it again.
	stands, err := graphcensus.ObjectRowStands(ctx, tx, e.Kind, e.ID)
	if err != nil {
		return erasedRead{}, err
	}
	if stands && e.Kind == graphstore.KindMessage {
		var named bool
		if err := tx.QueryRow(ctx, `select exists (select 1 from messages where node_kind = $1 and node_address = $2)`,
			e.Kind, e.ID).Scan(&named); err != nil {
			return erasedRead{}, fmt.Errorf("read whether the row of %s:%s names its node: %w", e.Kind, e.ID, err)
		}
		if !named {
			return erasedRead{}, fmt.Errorf("the erase refuses %s:%s: its row stands and names no node yet, so the erase, "+
				"which deletes the rows that name the node, would leave that row and its text in place; "+
				"envoy-dispatch backfill-graph-store names it, and the node can be erased after", e.Kind, e.ID)
		}
	} else if stands {
		return erasedRead{}, fmt.Errorf("the erase refuses %s:%s: its row stands and holds the node's text, which the erase "+
			"does not reach, so erasing the node would report an erasure that left that text in place; no command erases that "+
			"text, and the node can be erased once no row stands behind it", e.Kind, e.ID)
	}
	if read.document, read.blockID, err = e.ownDocument(ctx, tx); err != nil {
		return erasedRead{}, err
	}
	if read.margins, err = e.marginDocuments(ctx, tx); err != nil {
		return erasedRead{}, err
	}
	if err := e.readTexts(ctx, tx, &read); err != nil {
		return erasedRead{}, err
	}
	if read.project, err = e.nodeProject(ctx, tx, read.document); err != nil {
		return erasedRead{}, err
	}
	for _, step := range []func(context.Context, pgx.Tx, *erasedRead) error{
		e.readTuples, e.readRows, e.readReadState, e.readAncestors, e.readVersions, e.readReviews, e.readEmbeddings,
		e.readSearch, e.readExcerpts, e.readEvents, e.readHistory, e.readItemParts, e.readBlockers, e.readLeft,
	} {
		if err := step(ctx, tx, &read); err != nil {
			return erasedRead{}, err
		}
	}
	if read.owners, err = e.ownerRows(ctx, tx); err != nil {
		return erasedRead{}, err
	}
	e.Listing = read.listing.lines(e)
	e.Digest = digest(e.Listing)
	return read, nil
}

// nodeProject is the project whose term statistics count the node's words, which the run counts
// again once its items moved (api.RecountSearchTermStatsTx): a comment's issue's or document's
// project, a block's document's, and empty for a node no project holds.
func (e *Erasure) nodeProject(ctx context.Context, tx pgx.Tx, document *ownDocument) (string, error) {
	project := ""
	if document != nil {
		project = document.project
	}
	if err := tx.QueryRow(ctx, `
		select coalesce((select coalesce(i.project_key, a.project_key)
		                 from comments c
		                 left join issues i on i.key = c.issue_key
		                 left join artifacts a on a.id = c.artifact_id
		                 where c.node_kind = $1 and c.node_address = $2
		                 limit 1), $3::text)`, e.Kind, e.ID, project).Scan(&project); err != nil {
		return "", fmt.Errorf("read the project of %s:%s: %w", e.Kind, e.ID, err)
	}
	return project, nil
}

// ownDocument reads the document of a block node, which its address names: a block's address is
// its document's ref_key, '#' and its block id, and a ref_key holds no '#' (migration 0121). It
// is read from the address rather than from the block's document_blocks row, so a block a
// version dropped, which has no such row and is a gone node, still reaches its texts and its
// document. Any other node answers nil.
func (e *Erasure) ownDocument(ctx context.Context, tx pgx.Tx) (*ownDocument, string, error) {
	if e.Kind != refs.BlockNodeKind {
		return nil, "", nil
	}
	refKey, blockID, found := strings.Cut(e.ID, "#")
	if !found {
		return nil, "", nil
	}
	var document ownDocument
	err := tx.QueryRow(ctx, `
		select a.id::text, a.ref_key, coalesce(a.issue_key, ''), coalesce(a.project_key, ''),
		       a.is_primary and a.issue_key is not null
		from artifacts a where a.ref_key = $1 and a.kind = 'doc'`, refKey).Scan(
		&document.artifact, &document.refKey, &document.issue, &document.project, &document.spec)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, blockID, nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("read the document of %s:%s: %w", e.Kind, e.ID, err)
	}
	return &document, blockID, nil
}

// marginDocument is a document whose margin holds an erased comment's body, with what of that
// margin goes: the comment's own record, by its mark id, and its entry in the record of each
// thread it replies in.
type marginDocument struct {
	ownDocument
	records []string
	replies []docs.ErasedReply
}

// marginDocuments reads the documents whose margin record holds a comment's body, whose editing
// state therefore derives from the comment and holds its bytes. A comment anchored to a document
// is projected into that document's margin by the write that makes it: the record carries its
// body under `text` (api's commentMarkRecord, docs.ProjectMark), keyed by its anchor's mark id,
// and a reply's body stands in its thread root's record under `replies`, as an entry naming its
// author (docs.ActorRef) and the time it was made (docs.MarkTime), by which the erase takes it
// out (docs.ErasedReply). So the two documents a comment's body can be in are the one its own
// anchor names and the one its root's anchor names, which is one document for a reply to an
// anchored comment and none for a comment on an issue.
//
// The run replaces the editing state of each of them, as it replaces a block's own document's:
// a record is stored in the same updates the text is, so no search of those bytes can say which
// update holds the record. The seed is the live document with those entries taken out and
// nothing else (docs.ReadErasedLiveText): the document's text, every other comment's anchor mark
// and every other comment's record are other nodes', and stand.
//
// Any other node kind answers none: nothing else is projected into a document's margin.
func (e *Erasure) marginDocuments(ctx context.Context, tx pgx.Tx) ([]marginDocument, error) {
	if refs.ItemKind(e.Kind) != refs.ItemComment {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `
		with recursive thread (id, reply_to, depth) as (
			select c.id, c.reply_to, 1 from comments c where c.id = $1::uuid
			union all
			select root.id, root.reply_to, thread.depth + 1 from thread
			join comments root on root.id = thread.reply_to
			where thread.depth < 100
		)
		select a.id::text, a.ref_key, coalesce(a.issue_key, ''), coalesce(a.project_key, ''),
		       a.is_primary and a.issue_key is not null,
		       coalesce(c.anchor->>'mark_id', ''), thread.depth = 1,
		       coalesce((select erased.author->>'kind' || ':' || (erased.author->>'id') from comments erased where erased.id = $1::uuid), ''),
		       (select erased.created_at from comments erased where erased.id = $1::uuid)
		from thread
		join comments c on c.id = thread.id
		join artifacts a on a.id = (c.anchor->>'artifact_id')::uuid and a.kind = 'doc'
		where c.anchor is not null
		order by 1, thread.depth`, e.ID)
	if err != nil {
		return nil, fmt.Errorf("read the documents holding %s:%s in their margin: %w", e.Kind, e.ID, err)
	}
	type anchored struct {
		document ownDocument
		mark, by string
		own      bool
		made     time.Time
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (anchored, error) {
		var held anchored
		err := row.Scan(&held.document.artifact, &held.document.refKey, &held.document.issue, &held.document.project,
			&held.document.spec, &held.mark, &held.own, &held.by, &held.made)
		return held, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the documents holding %s:%s in their margin: %w", e.Kind, e.ID, err)
	}
	var margins []marginDocument
	for _, held := range found {
		if len(margins) == 0 || margins[len(margins)-1].artifact != held.document.artifact {
			margins = append(margins, marginDocument{ownDocument: held.document})
		}
		margin := &margins[len(margins)-1]
		if held.mark == "" {
			continue
		}
		if held.own {
			margin.records = append(margin.records, held.mark)
		} else {
			margin.replies = append(margin.replies, docs.ErasedReply{Record: held.mark, By: held.by, At: docs.MarkTime(held.made)})
		}
	}
	return margins, nil
}

// readTexts reads the text the node's own rows hold, which is what the erase takes out of every
// row that derives from it: a comment's body and each text its blocks held at a revision, a
// message's body, which is never edited and so holds every text its blocks hold, and a block's
// whole text at every version that recorded one (document_block_texts), not its excerpt, which is
// the block's first refs.BlockExcerptRunes runes and would leave the rest. A block no version
// recorded a text of - a row an older build wrote - is read by its excerpt, the one place it
// holds its text. A node whose rows hold no text - a block reference, a person - redacts nothing.
//
// The texts are distinct, longest first, so a text that holds a shorter one is replaced whole
// before the shorter one is, and two reads of one node list them in one order.
func (e *Erasure) readTexts(ctx context.Context, tx pgx.Tx, read *erasedRead) error {
	document := ""
	if read.document != nil {
		document = read.document.artifact
	}
	rows, err := tx.Query(ctx, `
		select c.body from comments c where c.node_kind = $1 and c.node_address = $2
		union
		select t.text from comment_block_texts t join comments c on c.id = t.comment_id
		 where c.node_kind = $1 and c.node_address = $2 and t.text is not null
		union
		select m.body from messages m where m.node_kind = $1 and m.node_address = $2
		union
		select t.text from document_block_texts t
		 where t.artifact_id = nullif($3, '')::uuid and t.block_id = $4 and t.text is not null
		union
		select b.excerpt from document_blocks b
		 where b.node_kind = $1 and b.node_address = $2
		   and not exists (select 1 from document_block_texts t
		                   where t.artifact_id = b.artifact_id and t.block_id = b.block_id and t.text is not null)`,
		e.Kind, e.ID, document, read.blockID)
	if err != nil {
		return fmt.Errorf("read the texts of %s:%s: %w", e.Kind, e.ID, err)
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("read the texts of %s:%s: %w", e.Kind, e.ID, err)
	}
	for _, text := range found {
		if strings.TrimSpace(text) != "" {
			read.texts = append(read.texts, text)
		}
	}
	slices.SortFunc(read.texts, func(a, b string) int {
		if len(a) != len(b) {
			return len(b) - len(a)
		}
		return strings.Compare(a, b)
	})
	return nil
}

// readTuples lists every tuple out of the node and into it, by its seq, each of which goes with
// the node. A value tuple is listed without its value, which can be text. Of an ask's node or a
// block's, it also keeps the writers its authored_by, decided_by and answered_by tuples name, and
// of a comment's, a message's or an ask's the people its inbox tuples tie to it (reachedPerson)
// (Erasure.Writers), which the record of the run holds once those tuples go.
func (e *Erasure) readTuples(ctx context.Context, tx pgx.Tx, read *erasedRead) error {
	rows, err := tx.Query(ctx, `
		select seq, from_kind, from_id, kind, to_kind, to_id, from_kind = $1 and from_id = $2
		from graph_tuples
		where (from_kind = $1 and from_id = $2) or (to_kind = $1 and to_id = $2)
		order by seq`, e.Kind, e.ID)
	if err != nil {
		return fmt.Errorf("read the tuples of %s:%s: %w", e.Kind, e.ID, err)
	}
	defer rows.Close()
	writers := map[ErasedWriter]struct{}{}
	for rows.Next() {
		var seq int64
		var fromKind, fromID, kind, toKind, toID string
		var out bool
		if err := rows.Scan(&seq, &fromKind, &fromID, &kind, &toKind, &toID, &out); err != nil {
			return fmt.Errorf("read a tuple of %s:%s: %w", e.Kind, e.ID, err)
		}
		if out {
			e.Tuples++
		} else {
			e.Inbound++
		}
		if out && writesTheNode(e.Kind, kind, toKind) {
			writers[ErasedWriter{Kind: toKind, ID: toID}] = struct{}{}
		}
		if person, reached := reachedPerson(e.Kind, out, fromKind, fromID, kind, toKind, toID); reached {
			writers[ErasedWriter{Kind: graphstore.KindPerson, ID: person}] = struct{}{}
		}
		far := toKind + ":" + toID
		if toKind == graphstore.KindValue {
			far = "a value"
		}
		read.tupleSeqs = append(read.tupleSeqs, seq)
		read.listing.tuples = append(read.listing.tuples,
			fmt.Sprintf("delete graph_tuples %d: %s:%s %s %s", seq, fromKind, fromID, kind, far))
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the tuples of %s:%s: %w", e.Kind, e.ID, err)
	}
	e.Writers = slices.SortedFunc(maps.Keys(writers), func(a, b ErasedWriter) int {
		return cmp.Or(strings.Compare(a.Kind, b.Kind), strings.Compare(a.ID, b.ID))
	})
	return nil
}

// erasedPart is one node that is a part of an erased item - a block of its body, or one of its
// delivery attempts - and the seqs of the tuples on both sides of it that the item's own listing
// does not hold, which the erase deletes with the node (graphstore.EraseNode).
type erasedPart struct {
	kind, id string
	seqs     []int64
}

// readItemParts lists, for an erased comment or message, every node that is a part of it and
// every tuple on both sides of each, which derive from the item alone, so the erase takes them
// with it, as the item's row takes the rows that project them with it. An ask has parts too
// (`ask/<id>#<part id>`, refs.PlanAskParts), but the erase refuses an ask while its row stands
// (Erasure.read), and its part rows go with that row, so no erase of an ask reads any:
//
//   - each block node of its body (`comment/<id>#<block id>`, `message/<id>#<block id>`;
//     refs.IndexItem) with its part_of the item, the links its text held and the retractions of
//     them. Its addresses are read from the item's own block rows, by the item's id through the
//     index on it (0142, 20261101143211), rather than off graph_nodes by an address prefix, which
//     binds no btree column under the database's ICU collation and would read every block node the
//     store holds. A row's node_id is the block's address whether or not the row still names its
//     node: a row an older build updated names none (migration 20261116150000), and the node it
//     named is still the store's.
//   - each delivery_attempt node of a comment's mentions and of a message's sends
//     (graphstore.AttemptOpened), at the address its comment_deliveries or message_deliveries row's
//     key spells (graphstore.AttemptKey), with its values and its owed_to, delivered_to and
//     answered_by. Those rows cannot exist without the item's row, so apply deletes them with it,
//     and a node none of them names any longer would be an attempt of nothing.
//
// A tuple between a part and the item itself - a block's part_of - is the item's own, which
// readTuples lists and the item's erase deletes. A tuple between two of the item's parts is
// listed with the first of them read, which deletes it. A tuple into one of the parts from a node
// that is neither the item nor one of its parts is somebody else's words about it - a reply
// naming the block - and refuses the run as readBlockers' rows do.
func (e *Erasure) readItemParts(ctx context.Context, tx pgx.Tx, read *erasedRead) error {
	var blocks, attempts string
	switch e.Kind {
	case graphstore.KindComment:
		blocks, attempts = "comment_blocks b where b.comment_id::text = $1", "comment_deliveries"
	case graphstore.KindMessage:
		blocks, attempts = "message_blocks b where b.message_id::text = $1", "message_deliveries"
	default:
		return nil
	}
	rows, err := tx.Query(ctx, `select b.node_id from `+blocks+` order by b.node_id`, e.ID)
	if err != nil {
		return fmt.Errorf("read the blocks of %s:%s: %w", e.Kind, e.ID, err)
	}
	blockIDs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("read the blocks of %s:%s: %w", e.Kind, e.ID, err)
	}
	seen := map[int64]struct{}{}
	if err := e.readParts(ctx, tx, read, graphstore.KindBlock, blockIDs, seen); err != nil {
		return err
	}
	attemptIDs, err := e.attemptAddresses(ctx, tx, attempts)
	if err != nil {
		return err
	}
	return e.readParts(ctx, tx, read, graphstore.KindDeliveryAttempt, attemptIDs, seen)
}

// attemptAddresses is the address of the attempt node each row of table - comment_deliveries or
// message_deliveries - holds for the erased item, spelled from the row's key as its writer spelled
// it (graphstore.AttemptKey.ID), so a row that names no node any longer still reaches the node it
// named. An address past the store's bound is no node (graphstore.AttemptOpened), and the read of
// the nodes finds none at it.
func (e *Erasure) attemptAddresses(ctx context.Context, tx pgx.Tx, table string) ([]string, error) {
	query := `select target, attempt from comment_deliveries where comment_id = $1::uuid order by target, attempt`
	if table == "message_deliveries" {
		query = `select '', attempt from message_deliveries where message_id = $1::uuid order by attempt`
	}
	rows, err := tx.Query(ctx, query, e.ID)
	if err != nil {
		return nil, fmt.Errorf("read the %s rows of %s:%s: %w", table, e.Kind, e.ID, err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (string, error) {
		var target string
		var attempt int
		if err := row.Scan(&target, &attempt); err != nil {
			return "", fmt.Errorf("read a %s row of %s:%s: %w", table, e.Kind, e.ID, err)
		}
		if table == "message_deliveries" {
			return graphstore.MessageAttempt(e.ID, attempt).ID(), nil
		}
		return graphstore.CommentAttempt(e.ID, target, attempt).ID(), nil
	})
}

// readParts lists the nodes of kind at ids the store holds, each a part of the erased item, and
// every tuple on both sides of each that is not the item's own and that no part read before it
// listed (seen), as readItemParts says.
func (e *Erasure) readParts(ctx context.Context, tx pgx.Tx, read *erasedRead, kind string, ids []string, seen map[int64]struct{}) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `
		select n.id, t.seq, t.from_kind, t.from_id, t.kind, t.to_kind, t.to_id
		from graph_nodes n
		left join graph_tuples t on (t.from_kind = n.kind and t.from_id = n.id) or (t.to_kind = n.kind and t.to_id = n.id)
		where n.kind = $1 and n.id = any($2::text[])
		order by n.id, t.seq`, kind, ids)
	if err != nil {
		return fmt.Errorf("read the %s nodes of %s:%s: %w", kind, e.Kind, e.ID, err)
	}
	defer rows.Close()
	blocks, attempts := e.Kind+"/"+e.ID+"#", e.Kind+"/"+e.ID+"/"
	first := len(read.parts)
	for rows.Next() {
		var id string
		var seq *int64
		var fromKind, fromID, tupleKind, toKind, toID *string
		if err := rows.Scan(&id, &seq, &fromKind, &fromID, &tupleKind, &toKind, &toID); err != nil {
			return fmt.Errorf("read a %s node of %s:%s: %w", kind, e.Kind, e.ID, err)
		}
		if len(read.parts) == first || read.parts[len(read.parts)-1].id != id {
			read.parts = append(read.parts, erasedPart{kind: kind, id: id})
			read.listing.rows = append(read.listing.rows, fmt.Sprintf("delete graph_nodes %s:%s", kind, id))
		}
		if seq == nil {
			continue
		}
		if (*fromKind == e.Kind && *fromID == e.ID) || (*toKind == e.Kind && *toID == e.ID) {
			continue
		}
		if _, listed := seen[*seq]; listed {
			continue
		}
		seen[*seq] = struct{}{}
		part := &read.parts[len(read.parts)-1]
		part.seqs = append(part.seqs, *seq)
		far := *toKind + ":" + *toID
		if *toKind == graphstore.KindValue {
			far = "a value"
		}
		read.listing.tuples = append(read.listing.tuples,
			fmt.Sprintf("delete graph_tuples %d: %s:%s %s %s", *seq, *fromKind, *fromID, *tupleKind, far))
		inbound := *toKind == kind && *toID == id
		ours := (*fromKind == graphstore.KindBlock && strings.HasPrefix(*fromID, blocks)) ||
			(*fromKind == graphstore.KindDeliveryAttempt && strings.HasPrefix(*fromID, attempts))
		if inbound && !ours {
			e.Blockers = append(e.Blockers, fmt.Sprintf("%s:%s states %s into %s:%s, a part of this node: erase or edit it first",
				*fromKind, *fromID, *tupleKind, kind, id))
		}
	}
	return rows.Err()
}

// writesTheNode reports whether a tuple of kind out of a node of nodeKind, reaching a node of
// toKind, names who wrote, answered or removed that node: an ask's authored_by, answered_by,
// decided_by or removed_by, or a decision's decided_by or a reading's authored_by, into a person or
// a session. A comment's authored_by is not one: no record of an erase holds a comment's author.
func writesTheNode(nodeKind, kind, toKind string) bool {
	if toKind != graphstore.KindPerson && toKind != graphstore.KindSession {
		return false
	}
	switch nodeKind {
	case graphstore.KindAsk:
		return kind == graphstore.TupleAuthoredBy || kind == graphstore.TupleAnsweredBy || kind == graphstore.TupleDecidedBy ||
			kind == graphstore.TupleRemovedBy
	case refs.BlockNodeKind:
		return kind == graphstore.TupleAuthoredBy || kind == graphstore.TupleDecidedBy
	}
	return false
}

// reachedPerson answers the person a tuple of an erased comment, message or ask ties to it as one
// of its thread's participants or readers (dispatch://INFRA-1152 R2 and R6): the far end of its
// delivered_to into a person, and the near end of a participates_in, read_through, dismissed,
// snoozed or reply_read from a person into it. Each such person's node was made by the write that
// stated the tuple, and once the tuple and the item's user_threads and read-state rows are gone
// nothing else may name them, so the record of the erase holds them (Erasure.Writers) as it holds
// who wrote an ask.
func reachedPerson(nodeKind string, out bool, fromKind, fromID, kind, toKind, toID string) (string, bool) {
	if nodeKind != graphstore.KindComment && nodeKind != graphstore.KindMessage && nodeKind != graphstore.KindAsk {
		return "", false
	}
	if out {
		return toID, kind == graphstore.TupleDeliveredTo && toKind == graphstore.KindPerson
	}
	switch kind {
	case graphstore.TupleParticipatesIn, graphstore.TupleReadThrough, graphstore.TupleDismissed,
		graphstore.TupleSnoozed, graphstore.TupleReplyRead:
		return fromID, fromKind == graphstore.KindPerson
	}
	return "", false
}

// readRows lists the node's row, its projection rows by key, the rows that cannot exist without
// them (every required single-column reference into a projected table), and the whole text of
// its block at every version.
func (e *Erasure) readRows(ctx context.Context, tx pgx.Tx, read *erasedRead) error {
	read.listing.rows = append(read.listing.rows, fmt.Sprintf("delete graph_nodes %s:%s", e.Kind, e.ID))
	for _, held := range projectedTables {
		rows, err := tx.Query(ctx, `select row(`+held.Key+`)::text from `+held.Table+
			` where node_kind = $1 and node_address = $2 order by `+held.Key, e.Kind, e.ID)
		if err != nil {
			return fmt.Errorf("read the %s rows of %s:%s: %w", held.Table, e.Kind, e.ID, err)
		}
		keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("read the %s rows of %s:%s: %w", held.Table, e.Kind, e.ID, err)
		}
		for _, key := range keys {
			read.listing.rows = append(read.listing.rows, fmt.Sprintf("delete %s (%s)=%s", held.Table, held.Key, key))
		}
		references, err := referencesTo(ctx, tx, held.Table)
		if err != nil {
			return err
		}
		for _, reference := range references {
			if !reference.Required {
				continue
			}
			var count int64
			if err := tx.QueryRow(ctx, `select count(*) from `+reference.Table+` child where child.`+reference.Column+
				` in (`+projectionRows(reference, held.Table)+`)`, e.Kind, e.ID).Scan(&count); err != nil {
				return fmt.Errorf("count the %s rows of %s:%s: %w", reference.Table, e.Kind, e.ID, err)
			}
			if count > 0 {
				read.listing.rows = append(read.listing.rows, fmt.Sprintf("delete %s: %d row(s) through %s.%s",
					reference.Table, count, reference.Table, reference.Column))
			}
		}
	}
	if read.document != nil {
		var count int64
		if err := tx.QueryRow(ctx, `select count(*) from document_block_texts where artifact_id = $1::uuid and block_id = $2`,
			read.document.artifact, read.blockID).Scan(&count); err != nil {
			return fmt.Errorf("count the texts of %s:%s: %w", e.Kind, e.ID, err)
		}
		if count > 0 {
			read.listing.rows = append(read.listing.rows, fmt.Sprintf("delete document_block_texts: %d row(s) of %s", count, e.ID))
		}
	}
	return nil
}

// readAncestors reads the blocks of the node's document that hold its block, up the
// parent_block_id chain, and keeps those whose stored text or excerpt the redaction rewrites: a
// container's text is the document cut across every block inside it, so it holds the node's text
// as well as its siblings', and redactBlocks writes back what the redaction leaves.
//
// The rewrite is computed here, under the same read the listing is made of, so a confirmed run
// writes the rows it listed and no others, and a container holding the erased words only where
// another block of it stands is no ancestor of this run at all: its rows hold another node's
// text, readLeft lists them, and nothing of the run touches them.
func (e *Erasure) readAncestors(ctx context.Context, tx pgx.Tx, read *erasedRead) error {
	if read.document == nil || len(read.texts) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `
		with recursive up (block_id, depth) as (
			select b.parent_block_id, 1 from document_blocks b
			where b.artifact_id = $1::uuid and b.block_id = $2 and b.parent_block_id is not null
			union all
			select p.parent_block_id, up.depth + 1 from up
			join document_blocks p on p.artifact_id = $1::uuid and p.block_id = up.block_id
			where p.parent_block_id is not null and up.depth < 1000
		)
		select distinct up.block_id from up order by 1`, read.document.artifact, read.blockID)
	if err != nil {
		return fmt.Errorf("read the blocks holding %s:%s: %w", e.Kind, e.ID, err)
	}
	holding, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("read the blocks holding %s:%s: %w", e.Kind, e.ID, err)
	}
	if len(holding) == 0 {
		return nil
	}
	if err := e.readAncestorTexts(ctx, tx, read, holding); err != nil {
		return err
	}
	if err := e.readAncestorExcerpts(ctx, tx, read, holding); err != nil {
		return err
	}
	quoting := map[string]bool{}
	for _, text := range read.blockTexts {
		quoting[text.blockID] = true
	}
	for _, excerpt := range read.excerpts {
		quoting[excerpt.blockID] = true
	}
	read.ancestors = slices.Sorted(maps.Keys(quoting))
	return nil
}

// readAncestorTexts reads the text each of those blocks held at every version and keeps each the
// redaction rewrites, with the text as it leaves it.
func (e *Erasure) readAncestorTexts(ctx context.Context, tx pgx.Tx, read *erasedRead, holding []string) error {
	rows, err := tx.Query(ctx, `
		select t.block_id, t.version, t.text from document_block_texts t
		where t.artifact_id = $1::uuid and t.block_id = any($2::text[]) and t.text is not null
		order by t.block_id, t.version`, read.document.artifact, holding)
	if err != nil {
		return fmt.Errorf("read the texts of the blocks holding %s:%s: %w", e.Kind, e.ID, err)
	}
	held, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (redactedBlockText, error) {
		var text redactedBlockText
		err := row.Scan(&text.blockID, &text.version, &text.text)
		return text, err
	})
	if err != nil {
		return fmt.Errorf("read the texts of the blocks holding %s:%s: %w", e.Kind, e.ID, err)
	}
	for _, text := range held {
		redacted := redactBlockQuote(text.text, read.texts, e.marker())
		if redacted == text.text {
			continue
		}
		read.blockTexts = append(read.blockTexts, redactedBlockText{blockID: text.blockID, version: text.version, text: redacted})
	}
	return nil
}

// readAncestorExcerpts reads the excerpt of each of those blocks and keeps each the redaction
// rewrites, with the excerpt as it leaves it: cut from the redacted text of the version the row
// records its text at, the way the indexer cuts one (refs.BlockExcerpt, around the server's
// dashboard origin, serverURL), and redacted in place where that rewrite does not reach it - a
// block whose text document_block_texts does not hold.
func (e *Erasure) readAncestorExcerpts(ctx context.Context, tx pgx.Tx, read *erasedRead, holding []string) error {
	rows, err := tx.Query(ctx, `
		select b.block_id, b.node_id, b.excerpt, b.changed_version,
		       (select t.text from document_block_texts t
		        where t.artifact_id = b.artifact_id and t.block_id = b.block_id and t.version = b.changed_version)
		from document_blocks b
		where b.artifact_id = $1::uuid and b.block_id = any($2::text[])
		order by b.block_id`, read.document.artifact, holding)
	if err != nil {
		return fmt.Errorf("read the excerpts of the blocks holding %s:%s: %w", e.Kind, e.ID, err)
	}
	type storedExcerpt struct {
		blockID, nodeID, excerpt string
		version                  int32
		text                     *string
	}
	held, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (storedExcerpt, error) {
		var stored storedExcerpt
		err := row.Scan(&stored.blockID, &stored.nodeID, &stored.excerpt, &stored.version, &stored.text)
		return stored, err
	})
	if err != nil {
		return fmt.Errorf("read the excerpts of the blocks holding %s:%s: %w", e.Kind, e.ID, err)
	}
	for _, stored := range held {
		cut := redactBlockQuote(stored.excerpt, read.texts, e.marker())
		if stored.text != nil {
			redacted := redactBlockQuote(*stored.text, read.texts, e.marker())
			if redacted != *stored.text {
				cut = refs.BlockExcerpt(redacted, e.serverURL)
			}
		}
		if cut == stored.excerpt {
			continue
		}
		read.excerpts = append(read.excerpts, redactedExcerpt{blockID: stored.blockID, nodeID: stored.nodeID, excerpt: cut})
	}
	return nil
}

// readVersions reads every version of the node's own document the erase deletes, and answers the
// (artifact, number) pairs the deletes take. A version goes when the erased block stood in it
// holding an erased text; the listing says so for each.
//
// The block stood in a version when its newest document_block_texts row at or before that
// version holds the text. The indexer writes a block's row only at a version that made, changed,
// restored or dropped it (refs.IndexBlocks), so a version that carries the block unchanged has no
// row of its own, and the block's text in it is the one its last row recorded; a row with no text
// is the version that dropped the block, after which it stands in none until a row restores it.
//
// A version whose markdown holds the text only where another block of the document stands is
// that block's text and stays: the whole version row would take the other block's words away
// with it, and the approvals over it. The version's blocks are what tells the two apart, so a
// version no block-aware build indexed - one with no document_indexed_versions row - cannot be
// narrowed to a block and goes on holding the match, as it did before the blocks were recorded.
//
// A version of any other document is another node's text, and stays.
func (e *Erasure) readVersions(ctx context.Context, tx pgx.Tx, read *erasedRead) error {
	e.Versions = []ErasedVersion{}
	if read.document == nil || len(read.texts) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `
		select v.number,
		       exists (select 1 from (select t.text from document_block_texts t
		                              where t.artifact_id = v.artifact_id and t.block_id = $3 and t.version <= v.number
		                              order by t.version desc limit 1) newest
		               where newest.text is not null
		                 and exists (select 1 from unnest($2::text[]) as erased(text) where position(erased.text in newest.text) > 0)),
		       exists (select 1 from document_indexed_versions i
		               where i.artifact_id = v.artifact_id and i.version = v.number)
		from artifact_versions v
		where v.artifact_id = $1::uuid
		  and exists (select 1 from unnest($2::text[]) as erased(text) where position(erased.text in v.markdown) > 0)
		order by v.number`, read.document.artifact, read.texts, read.blockID)
	if err != nil {
		return fmt.Errorf("read the versions holding the erased text: %w", err)
	}
	type heldVersion struct {
		number         int32
		block, indexed bool
	}
	held, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (heldVersion, error) {
		var version heldVersion
		err := row.Scan(&version.number, &version.block, &version.indexed)
		return version, err
	})
	if err != nil {
		return fmt.Errorf("read the versions holding the erased text: %w", err)
	}
	for _, version := range held {
		why := "the erased block stands in that version holding the text"
		switch {
		case version.block:
		case !version.indexed:
			why = "no build recorded that version's blocks, so the text in it cannot be read as any one block's"
		default:
			continue
		}
		read.artifacts = append(read.artifacts, read.document.artifact)
		read.numbers = append(read.numbers, version.number)
		erased := ErasedVersion{Document: read.document.refKey, Issue: read.document.issue,
			Project: read.document.project, Version: int(version.number)}
		e.Versions = append(e.Versions, erased)
		read.listing.versions = append(read.listing.versions, fmt.Sprintf("%s version %d%s: %s",
			erased.Document, erased.Version, owner(erased.Issue, erased.Project), why))
	}
	return nil
}

// readReviews reads every approval standing on one of those versions. Its content_sha256 is the
// hash of exactly the bytes about to go, so the approval cannot survive them: the trigger refuses
// to move a stored digest, and an approval over bytes nobody can reproduce is a claim about a
// text that no longer exists.
func (e *Erasure) readReviews(ctx context.Context, tx pgx.Tx, read *erasedRead) error {
	e.Reviews = []ErasedReview{}
	if len(read.artifacts) == 0 {
		return nil
	}
	// Ordered as docs.ReviewOrder orders every read of artifact_reviews in this tree: a review
	// that committed later outranks one that committed first, since created_at is its
	// transaction's start and its id is random (api's review_order_guard_test.go). The order is
	// spelled out with its table named on every column rather than taken from docs.ReviewOrder,
	// because the artifacts row joined here has a created_at of its own, which a bare created_at
	// would leave ambiguous.
	rows, err := tx.Query(ctx, `
		select artifact_reviews.id::text, a.ref_key, coalesce(a.issue_key, ''),
		       artifact_reviews.version, artifact_reviews.state
		from artifact_reviews join artifacts a on a.id = artifact_reviews.artifact_id
		where exists (
			select 1 from unnest($1::uuid[], $2::int[]) as doomed(artifact, number)
			where doomed.artifact = artifact_reviews.artifact_id and doomed.number = artifact_reviews.version
		)
		order by artifact_reviews.event_seq desc nulls last, artifact_reviews.created_at desc,
		         artifact_reviews.id desc`,
		read.artifacts, read.numbers)
	if err != nil {
		return fmt.Errorf("read the reviews over the erased bytes: %w", err)
	}
	reviews, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ErasedReview, error) {
		var review ErasedReview
		err := row.Scan(&review.ID, &review.Document, &review.Issue, &review.Version, &review.State)
		return review, err
	})
	if err != nil {
		return fmt.Errorf("read the reviews over the erased bytes: %w", err)
	}
	for _, review := range reviews {
		e.Reviews = append(e.Reviews, review)
		read.listing.reviews = append(read.listing.reviews, fmt.Sprintf("%s version %d %s (review %s)%s",
			review.Document, review.Version, review.State, review.ID, owner(review.Issue, "")))
	}
	return nil
}

// readHistory lists the stored history of every document the run replaces whole with one seed of
// its live state (docs.ErasedLiveText): the node's own document, and for a comment every document
// whose margin record holds its body (marginDocuments). A text typed piece by piece can be stored
// split across updates, and a margin record is stored in those same updates, so no search of
// those bytes can say whether the history holds them, and a block that holds a text was typed
// into it. It counts the rows and does not load them, since loading them takes the document's
// owner row and its room lock; a confirmed run loads them once it holds its locks
// (readLiveTexts).
func (e *Erasure) readHistory(ctx context.Context, tx pgx.Tx, read *erasedRead) error {
	e.Documents = []ErasedDocument{}
	for _, replaced := range read.replacedHistories() {
		held := ErasedDocument{Document: replaced.document.refKey, Issue: replaced.document.issue,
			Project: replaced.document.project, artifact: replaced.document.artifact, scope: replaced.scope}
		if err := tx.QueryRow(ctx, `
			select (select count(*) from doc_updates where artifact_id = $1::uuid),
			       (select count(*) from doc_checkpoints where artifact_id = $1::uuid),
			       (select count(*) from doc_snapshots where artifact_id = $1::uuid)`,
			replaced.document.artifact).Scan(&held.Updates, &held.Checkpoints, &held.Snapshots); err != nil {
			return fmt.Errorf("count the stored history of %s: %w", replaced.document.refKey, err)
		}
		if held.Updates+held.Checkpoints+held.Snapshots == 0 {
			continue
		}
		e.Documents = append(e.Documents, held)
		read.listing.documents = append(read.listing.documents, fmt.Sprintf("%s%s: its %d stored update(s), %d checkpoint(s) "+
			"and %d snapshot(s) are replaced by one seed of its live state, %s; "+
			"an editor open on it reloads and loses what it has not stored",
			held.Document, owner(held.Issue, held.Project), held.Updates, held.Checkpoints, held.Snapshots, replaced.redaction))
	}
	return nil
}

// replacedHistory is one document whose stored history the run replaces: the document, what the
// seed takes out of its live state (the erased block, and the erased comment's margin entries),
// and what the listing says of it.
type replacedHistory struct {
	document  ownDocument
	scope     docs.ErasedScope
	redaction string
}

// replacedHistories is every document whose stored history the run replaces, in the order the
// run reads and writes them: the node's own document first, where it has one and the run erases
// a text of it, then the documents whose margin holds a comment's body. A document is listed
// once, so a comment anchored to its own block's document - which no write makes today - would
// be replaced once.
func (read *erasedRead) replacedHistories() []replacedHistory {
	var replaced []replacedHistory
	held := map[string]bool{}
	if read.document != nil && len(read.texts) > 0 {
		replaced = append(replaced, replacedHistory{document: *read.document, scope: docs.ErasedScope{BlockID: read.blockID},
			redaction: "the marker standing where the erased text was, every anchor mark and margin record outside the erased block standing as it was"})
		held[read.document.artifact] = true
	}
	for _, margin := range read.margins {
		if held[margin.artifact] {
			continue
		}
		held[margin.artifact] = true
		replaced = append(replaced, replacedHistory{document: margin.ownDocument,
			scope:     docs.ErasedScope{Records: margin.records, Replies: margin.replies},
			redaction: "its text and every other comment's anchor mark and margin record standing as they are, the erased comment's margin entry gone"})
	}
	return replaced
}

// readLiveTexts loads the live state of each of those documents as the erase would leave it
// (docs.ReadErasedLiveText), which a confirmed run does once its listing matched. Each document's
// owner row is one the run took before its node's row (lockInStoreOrder), so the owner row
// docs.ReadErasedLiveText takes first is one this transaction holds already, and no owner row is
// taken after the node's. It reads and writes nothing; apply replaces it (replaceLiveTexts).
func (e *Erasure) readLiveTexts(ctx context.Context, tx pgx.Tx, read erasedRead) error {
	for index := range e.Documents {
		document := &e.Documents[index]
		scope := document.scope
		scope.Texts, scope.Marker = read.texts, e.marker()
		live, err := docs.ReadErasedLiveText(ctx, tx, document.artifact, scope)
		if err != nil {
			return fmt.Errorf("read the live state of %s: %w", document.Document, err)
		}
		if live == nil {
			continue
		}
		document.Read, document.live = true, live
		document.Held, document.Left = live.Held, live.Left
		document.AnchorMarks, document.MarginRecords = live.AnchorMarks, live.MarginRecords
	}
	return nil
}

// ownerRows reads the owner row of every row the erase writes that another writer writes under an
// owner: the node's projection rows stand under their owner (nodeOwners), a block's own
// document - its versions and reviews, its search rows, its history, its windows, its excerpts -
// under that document's issue, or the document's own artifacts row where no issue owns it, and
// every document whose margin holds a comment's body under its own owner the same way, since the
// run replaces that document's history. The rest of what the erase writes is owed no owner row,
// since none of its other writers takes one: an event payload already written (the outbox's
// record of what it published takes no owner row), a tuple, which is appended once and never
// written again, the term statistics, the display trees, and the node's own row. It reads no
// graph_nodes, graph_tuples or events row, so lockOwnersThenNode reads it before it holds
// anything.
func (e *Erasure) ownerRows(ctx context.Context, tx pgx.Tx) (events.OwnerRows, error) {
	owners, err := nodeOwners(ctx, tx, e.Kind, e.ID)
	if err != nil {
		return nil, err
	}
	document, _, err := e.ownDocument(ctx, tx)
	if err != nil {
		return nil, err
	}
	if document != nil {
		owners.Add(document.issue, document.artifact, "document "+document.refKey)
	}
	margins, err := e.marginDocuments(ctx, tx)
	if err != nil {
		return nil, err
	}
	for _, margin := range margins {
		owners.Add(margin.issue, margin.artifact, "document "+margin.refKey)
	}
	return owners, nil
}

// nodeOwners reads the owner row each projection row of the node stands under: an issue's own
// comment or message stands under the issue, and every other row under the owner of the document
// it belongs to, which is that document's issue or, for a document no issue owns, the document's
// own artifacts row - the owner docs' lockArtifactOwner finds and events.LockOwnerRows takes. A
// message on no issue belongs to no document either, and stands under none, as its write takes
// none. A node no row projects stands under none.
func nodeOwners(ctx context.Context, tx pgx.Tx, kind, id string) (events.OwnerRows, error) {
	arms := make([]string, len(projectedTables))
	for index, held := range projectedTables {
		issue, artifact := "null::text", "null::uuid"
		if held.Issue != "" {
			issue = held.Issue
		}
		if held.Artifact != "" {
			artifact = held.Artifact
		}
		arms[index] = `select ` + issue + `, ` + artifact + ` from ` + held.Table + ` where node_kind = $1 and node_address = $2`
	}
	rows, err := tx.Query(ctx, `
		select coalesce(held.issue_key, a.issue_key, ''), coalesce(a.id::text, '')
		from (`+strings.Join(arms, " union all ")+`) as held (issue_key, artifact_id)
		left join artifacts a on a.id = held.artifact_id`, kind, id)
	if err != nil {
		return nil, fmt.Errorf("read the owners of %s:%s: %w", kind, id, err)
	}
	held, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) ([2]string, error) {
		var owner [2]string
		err := row.Scan(&owner[0], &owner[1])
		return owner, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the owners of %s:%s: %w", kind, id, err)
	}
	owners := events.OwnerRows{}
	for _, owner := range held {
		owners.Add(owner[0], owner[1], kind+":"+id)
	}
	return owners, nil
}

// referencingColumn is one foreign key pointing at a projected table: the table and column that
// hold the reference, the column they hold, and whether that reference can be null.
type referencingColumn struct {
	Table, Column, Referenced string
	Required                  bool
}

// referencesTo reads every single-column foreign key pointing at one table. The composite key a
// projection carries to graph_nodes is not one of them, which is why the read binds a key of one
// column: that pair is the node's own address and goes with the node.
func referencesTo(ctx context.Context, tx pgx.Tx, table string) ([]referencingColumn, error) {
	rows, err := tx.Query(ctx, `
		select c.conrelid::regclass::text, child.attname, parent.attname, child.attnotnull
		from pg_constraint c
		join pg_attribute child on child.attrelid = c.conrelid and child.attnum = c.conkey[1]
		join pg_attribute parent on parent.attrelid = c.confrelid and parent.attnum = c.confkey[1]
		where c.contype = 'f' and c.confrelid = $1::regclass and array_length(c.conkey, 1) = 1
		order by 1, 2`, table)
	if err != nil {
		return nil, fmt.Errorf("read the foreign keys into %s: %w", table, err)
	}
	references, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (referencingColumn, error) {
		var reference referencingColumn
		err := row.Scan(&reference.Table, &reference.Column, &reference.Referenced, &reference.Required)
		return reference, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the foreign keys into %s: %w", table, err)
	}
	return references, nil
}

// projectionRows is the select of one projection's key column, which every delete against a row
// that points at it is written over.
func projectionRows(reference referencingColumn, table string) string {
	return `select held.` + reference.Referenced + ` from ` + table +
		` held where held.node_kind = $1 and held.node_address = $2`
}

// readBlockers names every live row of another node that states something about this one, each
// of which refuses the run, so an operator erases or edits those first and the store census
// still reads zero once the erase runs.
//
// Two kinds of row do. A row that points at this node's projection through a nullable reference:
// a reply whose reply_to names an erased comment is somebody else's words, and an ask handed back
// through it is the ask's own history, so the erase neither deletes one nor rewrites the other.
// And a projection row of another live node whose node states a tuple into this one now: a block
// inside an erased block (its part_of and its parent_block_id), a block that cites it (its
// mentions or pointer tuple and its block_refs row), a comment, an ask or a message its author is
// (its authored_by and its author), a block of an item's body that cites it or that a reply names
// (its link or its part_of and the item's block or link row naming the block's node,
// itemPartTables), and an item that cites it (its mentions and the item's refs row that holds
// tuples_stored, migration 20261116135959). An ask's row is read here beside projectedTables: it
// is the projection of the ask's node (migration 20261116094058), which the erase never deletes,
// since it refuses an ask while that row stands (Erasure.read). The erase deletes the tuples into
// the node, so such a row would go on stating a tuple the store no longer holds, which is a
// difference `census --store` reads. The erased item's own parts (readItemParts) go with it, and
// so does every tuple they state into it.
func (e *Erasure) readBlockers(ctx context.Context, tx pgx.Tx, read *erasedRead) error {
	for _, held := range projectedTables {
		references, err := referencesTo(ctx, tx, held.Table)
		if err != nil {
			return err
		}
		for _, reference := range references {
			if reference.Required {
				continue
			}
			var rows int64
			if err := tx.QueryRow(ctx, `select count(*) from `+reference.Table+` child where child.`+
				reference.Column+` in (`+projectionRows(reference, held.Table)+`)`, e.Kind, e.ID).Scan(&rows); err != nil {
				return fmt.Errorf("count the %s.%s rows at %s:%s: %w", reference.Table, reference.Column, e.Kind, e.ID, err)
			}
			if rows > 0 {
				e.Blockers = append(e.Blockers, fmt.Sprintf("%d %s row(s) point at this node through %s.%s",
					rows, reference.Table, reference.Table, reference.Column))
			}
		}
	}
	projected := make([]string, 0, len(projectedTables)+len(itemPartTables)+2)
	for _, held := range projectedTables {
		projected = append(projected, `exists (select 1 from `+held.Table+` p where p.node_kind = t.from_kind and p.node_address = t.from_id)`)
	}
	projected = append(projected, `exists (select 1 from asks p where p.node_kind = t.from_kind and p.node_address = t.from_id)`)
	for _, table := range itemPartTables {
		projected = append(projected, `exists (select 1 from `+table+` p where p.node_kind = t.from_kind and p.node_address = t.from_id)`)
	}
	projected = append(projected, `exists (select 1 from refs p where p.from_kind = t.from_kind and p.from_id = t.from_id
		and p.to_kind = $1 and p.to_id = $2 and p.kind = t.kind and p.tuples_stored)`)
	own := make([]string, 0, len(read.parts))
	for _, part := range read.parts {
		if part.kind == graphstore.KindBlock {
			own = append(own, part.id)
		}
	}
	rows, err := tx.Query(ctx, `
		select t.from_kind, t.from_id, t.kind from (
			`+graphstore.CurrentTupleRead(`t.from_kind, t.from_id, t.kind`, `t.from_kind, t.from_id, t.kind, t.asserted`,
		`t.to_kind = $1 and t.to_id = $2 and not (t.from_kind = $1 and t.from_id = $2)`)+`
		) t
		join graph_nodes n on n.kind = t.from_kind and n.id = t.from_id and n.state = $3
		where t.asserted and not (t.from_kind = $4 and t.from_id = any($5::text[]))
		  and (`+strings.Join(projected, " or ")+`)
		order by 1, 2, 3`, e.Kind, e.ID, graphstore.StateLive, graphstore.KindBlock, own)
	if err != nil {
		return fmt.Errorf("read the nodes stating a tuple into %s:%s: %w", e.Kind, e.ID, err)
	}
	stating, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (string, error) {
		var fromKind, fromID, kind string
		err := row.Scan(&fromKind, &fromID, &kind)
		return fmt.Sprintf("%s:%s states %s into this node, and its own row says so: erase or edit it first",
			fromKind, fromID, kind), err
	})
	if err != nil {
		return fmt.Errorf("read the nodes stating a tuple into %s:%s: %w", e.Kind, e.ID, err)
	}
	e.Blockers = append(e.Blockers, stating...)
	read.listing.blockers = e.Blockers
	return nil
}
