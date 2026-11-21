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
	"github.com/sjawhar/envoy/internal/dispatch/text"
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
	// tupleSeqs are the seqs of the tuples the listing names, which the delete takes and no
	// others (graphstore.EraseNode).
	tupleSeqs []int64
	artifacts []string
	numbers   []int32
	events    []redactedEvent
	embedded  []embeddingRow
	owners    events.OwnerRows
	listing   listing
}

// read reads the node as the erase would remove it and every row that derives from it
// (readDerived), searches the database for the rows the run leaves (readLeft), and writes the
// listing of all of it and its digest onto the Erasure (list). It locks nothing and writes
// nothing, and a run without --confirm reads it in one read-only transaction. A confirmed run
// reads it in two: the search first, in a read-only transaction of its own that takes no lock
// (searchLeft), and the rest once it holds its locks (lockOwnersThenNode).
func (e *Erasure) read(ctx context.Context, tx pgx.Tx) (erasedRead, error) {
	read, err := e.readDerived(ctx, tx)
	if err != nil {
		return erasedRead{}, err
	}
	if err := e.readLeft(ctx, tx, &read); err != nil {
		return erasedRead{}, err
	}
	e.list(&read)
	return read, nil
}

// readDerived reads the node as the erase would remove it and every row that derives from it,
// with the owner rows those stand under: all of the listing but the rows the run leaves. It
// locks nothing and writes nothing.
//
// What derives from a node is fixed by the node, never by a search for its bytes: its tuples,
// its projection rows and the rows that cannot exist without them, its own embedding rows and the
// events about it, for a block its own document's versions, editing state, search rows and
// windows and the excerpts cut from it, and for a comment the editing state of every document
// whose margin record holds its body (marginDocuments). A row anywhere else that holds the same
// bytes derives from another node - another issue's spec quoting a comment's `Yes`, another
// person's comment, a sibling block of the same document - and is that node's text: the run lists
// it (readLeft) and changes nothing in it.
func (e *Erasure) readDerived(ctx context.Context, tx pgx.Tx) (erasedRead, error) {
	var read erasedRead
	if err := tx.QueryRow(ctx, `select supertag from graph_nodes where kind = $1 and id = $2`,
		e.Kind, e.ID).Scan(&e.Supertag); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return erasedRead{}, fmt.Errorf("the store holds no node %s:%s", e.Kind, e.ID)
		}
		return erasedRead{}, fmt.Errorf("read node %s:%s: %w", e.Kind, e.ID, err)
	}
	// A node whose text is in its own row is refused while that row stands (Erase). A confirmed
	// run reads this under its lock of the node's row (lockOwnersThenNode), and a create of this
	// build writes the node before the row in one transaction, holding the node's row until it
	// commits: a create that got there first has committed its row when this read runs, and one
	// that comes after waits for the erase. The run without --confirm reads it in its snapshot and
	// locks nothing, and the confirmed run reads it again.
	if stands, err := graphcensus.ObjectRowStands(ctx, tx, e.Kind, e.ID); err != nil {
		return erasedRead{}, err
	} else if stands {
		return erasedRead{}, fmt.Errorf("the erase refuses %s:%s: its row stands and holds the node's text, which the erase "+
			"does not reach, so erasing the node would report an erasure that left that text in place; no command erases that "+
			"text, and the node can be erased once no row stands behind it", e.Kind, e.ID)
	}
	var err error
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
		e.readTuples, e.readRows, e.readAncestors, e.readVersions, e.readReviews, e.readEmbeddings,
		e.readSearch, e.readExcerpts, e.readEvents, e.readHistory, e.readBlockers,
	} {
		if err := step(ctx, tx, &read); err != nil {
			return erasedRead{}, err
		}
	}
	if read.owners, err = e.ownerRows(ctx, tx); err != nil {
		return erasedRead{}, err
	}
	return read, nil
}

// list writes the listing of read and its digest onto the Erasure.
func (e *Erasure) list(read *erasedRead) {
	e.Listing = read.listing.lines(e)
	e.Digest = digest(e.Listing)
}

// nodeProject is the project whose term statistics count the node's words, which the run takes
// the erased words out of once its items moved (api.ForgetErasedTermsTx): a comment's issue's or
// document's project, a block's document's, and empty for a node no project holds.
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
// document. Any other node answers nil, and so does a finding of a lens: a block node whose id
// names its lens (graphstore.AddressKind), which no document holds, so its id names no document
// and no block of one.
func (e *Erasure) ownDocument(ctx context.Context, tx pgx.Tx) (*ownDocument, string, error) {
	if graphstore.AddressKind(e.Kind, e.ID) != refs.BlockNodeKind {
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
// row that derives from it: a comment's body and each text its blocks held at a revision, and a
// block's whole text at every version that recorded one (document_block_texts), not its
// excerpt, which is the block's first refs.BlockExcerptRunes runes and would leave the rest. A
// block no version recorded a text of - a row an older build wrote - is read by its excerpt, the
// one place it holds its text. A run of the command bar holds the words of its question, which
// its interaction node carries (graphstore.AttributeWords) and its own address answers whole as
// its text, so a read of that address stores the display tree of those words (texttrees), which
// treeSums finds by them. A node whose rows hold no text - a block reference, a person - redacts
// nothing.
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
		select t.text from document_block_texts t
		 where t.artifact_id = nullif($3, '')::uuid and t.block_id = $4 and t.text is not null
		union
		select b.excerpt from document_blocks b
		 where b.node_kind = $1 and b.node_address = $2
		   and not exists (select 1 from document_block_texts t
		                   where t.artifact_id = b.artifact_id and t.block_id = b.block_id and t.text is not null)
		union
		select n.attributes->>'`+graphstore.AttributeWords+`' from graph_nodes n
		 where $1 = '`+graphstore.KindInteraction+`' and n.kind = $1 and n.id = $2
		   and n.attributes->>'`+graphstore.AttributeWords+`' is not null`,
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
// the node. A value tuple is listed without its value, which can be text. A tuple into a finding
// of a lens names it by the kind the graph addresses it by, lens_block, where the store keys the
// finding as a block, so the tuples of the node are read under either (graphstore.NodeTuplesSQL,
// the set the delete takes). It also keeps the writers the node's tuples name (writesTheNode,
// Erasure.Writers), which the record of the run holds once those tuples go.
func (e *Erasure) readTuples(ctx context.Context, tx pgx.Tx, read *erasedRead) error {
	rows, err := tx.Query(ctx, `
		select seq, from_kind, from_id, kind, to_kind, to_id, from_kind = $1 and from_id = $2
		from graph_tuples
		where `+graphstore.NodeTuplesSQL("", "$1", "$2", "$3")+`
		order by seq`, e.Kind, e.ID, graphstore.AddressKind(e.Kind, e.ID))
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

// writesTheNode reports whether a tuple of kind out of a node of nodeKind, reaching a node of
// toKind, names who wrote, answered or removed that node, into a person or a session: an ask's
// authored_by, answered_by, decided_by or removed_by; a block's authored_by or decided_by - a
// decision's, a reading's, a finding's of a lens; and the authored_by of a run of the command
// bar, a lens, a risk or a constraint. The census's person and session row sides find the author
// of a run through its going_requests row, or through that current authored_by once a process of
// the release before deleted the row (graphcensus' goneRunsAuthored), and that of a lens, a risk
// or a constraint only through that current authored_by (graphcensus' reasoningAuthored), and the
// erase takes both, so for an author with no other record the erase's own record is the one that
// names them. A comment's authored_by is not one: no record of an erase holds a comment's author,
// whom the events of the comment name still (graphcensus' commentWriters).
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
	case graphstore.KindInteraction, graphstore.KindLens, graphstore.KindRisk, graphstore.KindConstraint:
		return kind == graphstore.TupleAuthoredBy
	}
	return false
}

// readRows lists the node's row, its own rows of every table by key (ownRowSets): its projection
// rows and the rows that cannot exist without them (every required single-column reference into
// a projected table), and the rows of a run of the command bar (runTables); and the whole text of
// its block at every version.
func (e *Erasure) readRows(ctx context.Context, tx pgx.Tx, read *erasedRead) error {
	read.listing.rows = append(read.listing.rows, fmt.Sprintf("delete graph_nodes %s:%s", e.Kind, e.ID))
	for _, own := range e.ownRowSets() {
		rows, err := tx.Query(ctx, `select row(`+own.key+`)::text from `+own.table+
			` where `+own.where+` order by `+own.key, own.args...)
		if err != nil {
			return fmt.Errorf("read the %s rows of %s:%s: %w", own.table, e.Kind, e.ID, err)
		}
		keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("read the %s rows of %s:%s: %w", own.table, e.Kind, e.ID, err)
		}
		for _, key := range keys {
			read.listing.rows = append(read.listing.rows, fmt.Sprintf("delete %s (%s)=%s", own.table, own.key, key))
		}
		if !own.projected {
			continue
		}
		references, err := referencesTo(ctx, tx, own.table)
		if err != nil {
			return err
		}
		for _, reference := range references {
			if !reference.Required {
				continue
			}
			var count int64
			if err := tx.QueryRow(ctx, `select count(*) from `+reference.Table+` child where child.`+reference.Column+
				` in (`+projectionRows(reference, own.table)+`)`, e.Kind, e.ID).Scan(&count); err != nil {
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

// readAncestorTexts reads the text each of those blocks held at every version the node's block
// stood in (blockStoodIn), and keeps each the redaction rewrites, with the text as it leaves it.
// At a version the node's block did not stand in - one before a version made it, or after one
// dropped it - the block holding it held none of its text, so the same words there are that
// block's own, and stay as they are.
func (e *Erasure) readAncestorTexts(ctx context.Context, tx pgx.Tx, read *erasedRead, holding []string) error {
	rows, err := tx.Query(ctx, `
		select t.block_id, t.version, t.text from document_block_texts t
		where t.artifact_id = $1::uuid and t.block_id = any($4::text[]) and t.text is not null
		  and `+blockStoodIn("t.artifact_id", "t.version")+`
		order by t.block_id, t.version`, read.document.artifact, read.texts, read.blockID, holding)
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
// records its text at, the way the indexer cuts one (refs.IndexBlocks), and redacted in place
// where that rewrite does not reach it - a block whose text document_block_texts does not hold.
// An excerpt cut from a version the node's block did not stand in (blockStoodIn) is cut from a
// text readAncestorTexts leaves as it is, and stays as it is with it.
func (e *Erasure) readAncestorExcerpts(ctx context.Context, tx pgx.Tx, read *erasedRead, holding []string) error {
	rows, err := tx.Query(ctx, `
		select b.block_id, b.node_id, b.excerpt, b.changed_version,
		       (select t.text from document_block_texts t
		        where t.artifact_id = b.artifact_id and t.block_id = b.block_id and t.version = b.changed_version),
		       `+blockStoodIn("b.artifact_id", "b.changed_version")+`
		from document_blocks b
		where b.artifact_id = $1::uuid and b.block_id = any($4::text[])
		order by b.block_id`, read.document.artifact, read.texts, read.blockID, holding)
	if err != nil {
		return fmt.Errorf("read the excerpts of the blocks holding %s:%s: %w", e.Kind, e.ID, err)
	}
	type storedExcerpt struct {
		blockID, nodeID, excerpt string
		version                  int32
		text                     *string
		stood                    bool
	}
	held, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (storedExcerpt, error) {
		var stored storedExcerpt
		err := row.Scan(&stored.blockID, &stored.nodeID, &stored.excerpt, &stored.version, &stored.text, &stored.stood)
		return stored, err
	})
	if err != nil {
		return fmt.Errorf("read the excerpts of the blocks holding %s:%s: %w", e.Kind, e.ID, err)
	}
	for _, stored := range held {
		if stored.text != nil && !stored.stood {
			continue
		}
		cut := redactBlockQuote(stored.excerpt, read.texts, e.marker())
		if stored.text != nil {
			redacted := redactBlockQuote(*stored.text, read.texts, e.marker())
			if redacted != *stored.text {
				cut = text.HeadRunes(redacted, refs.BlockExcerptRunes)
			}
		}
		if cut == stored.excerpt {
			continue
		}
		read.excerpts = append(read.excerpts, redactedExcerpt{blockID: stored.blockID, nodeID: stored.nodeID, excerpt: cut})
	}
	return nil
}

// blockStoodIn is the test of whether the node's block stood in one version of its document
// holding an erased text, over the statement's own spellings of that document and that version's
// number, with the erased texts bound as $2 and the block's id as $3. It is the one reading of
// which versions held the block: the versions the run deletes (readVersions), the versions of a
// block holding it at which the run redacts that block's text and excerpt (readAncestorTexts,
// readAncestorExcerpts), and the versions whose events the run redacts - each version an
// artifact.version, an artifact.human_edited, an artifact.approved or an
// artifact.changes_requested event of its document names, and the version an artifact.version's
// diff is taken from (readEvents).
//
// The block stood in a version when its newest document_block_texts row at or before that
// version holds the text. The indexer writes a block's row only at a version that made, changed,
// restored or dropped it (refs.IndexBlocks), so a version that carries the block unchanged has no
// row of its own, and the block's text in it is the one its last row recorded; a row with no text
// is the version that dropped the block, after which it stands in none until a row restores it.
func blockStoodIn(document, version string) string {
	return `exists (select 1 from (select own.text from document_block_texts own
	                              where own.artifact_id = ` + document + ` and own.block_id = $3 and own.version <= ` + version + `
	                              order by own.version desc limit 1) newest
	               where newest.text is not null
	                 and exists (select 1 from unnest($2::text[]) as erased(text) where position(erased.text in newest.text) > 0))`
}

// readVersions reads every version of the node's own document the erase deletes, and answers the
// (artifact, number) pairs the deletes take. A version goes when the erased block stood in it
// holding an erased text; the listing says so for each.
//
// Whether the block stood in a version is read off its own rows of document_block_texts
// (blockStoodIn).
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
		       `+blockStoodIn("v.artifact_id", "v.number")+`,
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
// comment stands under the issue, and every other row under the owner of the document it belongs
// to, which is that document's issue or, for a document no issue owns, the document's own
// artifacts row - the owner docs' lockArtifactOwner finds and events.LockOwnerRows takes. A node
// no row projects stands under none.
func nodeOwners(ctx context.Context, tx pgx.Tx, kind, id string) (events.OwnerRows, error) {
	arms := make([]string, len(graphstore.ProjectedTables))
	for index, held := range graphstore.ProjectedTables {
		issue := "null::text"
		if held.Issue != "" {
			issue = held.Issue
		}
		arms[index] = `select ` + issue + `, artifact_id from ` + held.Table + ` where node_kind = $1 and node_address = $2`
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

// readBlockers names every row of another node the store holds (at any state but gone) that
// states something about this one, each of which refuses the run, so an operator erases or edits
// those first and the store census still reads zero once the erase runs.
//
// Three kinds of row do. A row that points at this node's projection through a nullable
// reference: a reply whose reply_to names an erased comment is somebody else's words, and an ask
// handed back through it is the ask's own history, so the erase neither deletes one nor rewrites
// the other. A projection row of another node the store holds, not gone, whose node states a
// tuple into this one now of a kind that row digests (graphcensus.DigestedTuple, the census's own
// definition of what each projection row states): a block inside an erased block (its part_of and
// its parent_block_id), a block that cites it (its mentions or pointer tuple and its block_refs
// row), a comment or an ask its author is (its authored_by and its author). An ask's row is read
// here beside graphstore.ProjectedTables: it is the projection of the ask's node (migration
// 20261116094058), which the erase never deletes, since it refuses an ask while that row stands
// (Erasure.read). The erase deletes the tuples into the node, so such a row would go on stating a
// tuple the store no longer holds, which is a difference `census --store` reads. A tuple of a kind
// no row states - a changed_by_run a write made for a run appended out of a comment or a block, a
// requirement's constrained_by - is the store's alone: it blocks nothing, and the erase deletes
// it with the node. And, for a person, a comments row that names them as the owner of the session
// the server proved that wrote it (graphcensus.ProvedSessionComments, the census's own reading of
// that row): its authored_by names the session and only the session's alias_of names the person,
// and a session has no projection row, so the read before finds no such row, but the census names
// the person from the comment's row and would read her node gone while that row stands.
//
// Two tuples into a node have no projection row behind them and still block, at any state of
// the node that states them. One is the part_of of a node of a standing kind - a lens, a finding
// of one, a risk, a constraint (refs.StandingKinds) - which stands where that tuple says, and
// whose issue every read finds by walking it up (refs.StandingIssueSQL): an erase of the node it
// stands under would leave it standing under nothing, so a finding or a risk under an erased lens,
// and a lens invoked on an erased issue, document, block, comment, finding, risk or constraint,
// each refuses the erase. The other is the asserted authored_by of a run of the command bar or of
// a node of a standing kind into a person or a session, whom the census's row side of person and
// session names again from it (graphcensus' reasoningAuthored, goneRunsAuthored) and from the
// run's own going_requests row. A tuple into a finding is read under the kind the graph addresses
// it by as well (graphstore.NodeTuplesIntoSQL).
//
// Each node stating a tuple into this one probes its own row through graph_nodes' primary key,
// in a subquery behind an offset 0 the planner cannot flatten, so no plan reads graph_nodes
// again for each such tuple. The kinds block_refs projects, which the store's attribute nodes
// declare (refs.BlockRefKindsSQL), are read first by a statement of their own in the same
// transaction (refs.BlockRefKinds) and bound to the read of the tuples as a parameter
// (graphcensus.DigestedTuple), so its plan reads no graph_nodes row but by that key. The tuples
// are read through graph_tuples_as_of_in, whose predicate the read proves by naming
// `to_kind <> 'value'` (a value is no node, so nothing points into one), so a plan cached
// without the node's kind still reaches that index
// (TestTheBlockersReadProbesEachStatingNodeOnce).
func (e *Erasure) readBlockers(ctx context.Context, tx pgx.Tx, read *erasedRead) error {
	for _, held := range graphstore.ProjectedTables {
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
	projected := make([]string, 0, len(graphstore.ProjectedTables)+1)
	for _, held := range graphstore.ProjectedTables {
		projected = append(projected, `exists (select 1 from `+held.Table+` p where p.node_kind = t.from_kind and p.node_address = t.from_id)`)
	}
	projected = append(projected, `exists (select 1 from asks p where p.node_kind = t.from_kind and p.node_address = t.from_id)`)
	into := graphstore.NodeTuplesIntoSQL("t.", "$1", "$2", "$3") + ` and t.to_kind <> '` + graphstore.KindValue + `'
			and not (t.from_kind = $1 and t.from_id = $2)`
	addressed := graphstore.AddressKind(e.Kind, e.ID)
	blockRefKinds, err := refs.BlockRefKinds(ctx, tx)
	if err != nil {
		return err
	}
	if err := e.readStating(ctx, tx, `
		select distinct t.from_kind, t.from_id, t.kind from (
			`+graphstore.CurrentTupleRead(`t.from_kind, t.from_id, t.kind, t.to_kind`, `t.from_kind, t.from_id, t.kind, t.asserted`,
		into+` and `+graphcensus.DigestedTuple("t", "$5::text[]"))+`
		) t
		cross join lateral (
			select n.state from graph_nodes n
			where n.kind = t.from_kind and n.id = t.from_id
			offset 0
		) n
		where n.state <> $4 and t.asserted and (`+strings.Join(projected, " or ")+`)
		order by 1, 2, 3`, "%s:%s states %s into this node, and its own row says so: erase or edit it first",
		e.Kind, e.ID, addressed, graphstore.StateGone, blockRefKinds); err != nil {
		return err
	}
	type storeOnly struct {
		kind   string
		from   []string
		format string
	}
	stated := []storeOnly{{graphstore.TuplePartOf, refs.StandingKinds(),
		"%s:%s states %s into this node, which is where it stands and how every read finds the issue it stands under, so an erase of this node would leave it standing under nothing: erase it first"}}
	if e.Kind == graphstore.KindPerson || e.Kind == graphstore.KindSession {
		stated = append(stated, storeOnly{graphstore.TupleAuthoredBy, append(refs.StandingKinds(), graphstore.KindInteraction),
			"%s:%s states %s into this node, which the census holds it to: erase it first"})
	}
	for _, tuple := range stated {
		if err := e.readStating(ctx, tx, `
			select distinct t.from_kind, t.from_id, t.kind from (
				`+graphstore.CurrentTupleRead(`t.from_kind, t.from_id, t.kind, t.to_kind`, `t.from_kind, t.from_id, t.kind, t.asserted`,
			into+` and t.kind = '`+tuple.kind+`' and `+graphstore.AddressKindSQL("t.from_kind", "t.from_id")+
				` in ('`+strings.Join(tuple.from, `', '`)+`')`)+`
			) t
			where t.asserted
			order by 1, 2, 3`, tuple.format, e.Kind, e.ID, addressed); err != nil {
			return err
		}
	}
	if e.Kind == graphstore.KindPerson {
		rows, err := tx.Query(ctx, graphcensus.ProvedSessionComments, e.ID)
		if err != nil {
			return fmt.Errorf("read the comments naming %s:%s as the owner of the session that wrote them: %w", e.Kind, e.ID, err)
		}
		naming, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (string, error) {
			var comment string
			err := row.Scan(&comment)
			return fmt.Sprintf("%s:%s was written by a session the server proved this person's, and its own row names this person as that session's owner: erase it first",
				graphstore.KindComment, comment), err
		})
		if err != nil {
			return fmt.Errorf("read the comments naming %s:%s as the owner of the session that wrote them: %w", e.Kind, e.ID, err)
		}
		e.Blockers = append(e.Blockers, naming...)
	}
	read.listing.blockers = e.Blockers
	return nil
}

// readStating adds a blocker for each (from_kind, from_id, kind) query answers, named by format.
func (e *Erasure) readStating(ctx context.Context, tx pgx.Tx, query, format string, args ...any) error {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("read the nodes stating a tuple into %s:%s: %w", e.Kind, e.ID, err)
	}
	stating, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (string, error) {
		var fromKind, fromID, kind string
		err := row.Scan(&fromKind, &fromID, &kind)
		return fmt.Sprintf(format, fromKind, fromID, kind), err
	})
	if err != nil {
		return fmt.Errorf("read the nodes stating a tuple into %s:%s: %w", e.Kind, e.ID, err)
	}
	e.Blockers = append(e.Blockers, stating...)
	return nil
}
