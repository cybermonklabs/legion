package grapherase

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
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
// document, the blocks of that document that hold it, the versions, events and embedding rows
// the run deletes or rewrites, and the owner rows those stand under. It is what the listing is
// made of, and what apply writes, so a confirmed run writes the rows it listed and no other.
type erasedRead struct {
	texts    []string
	document *ownDocument
	blockID  string
	project  string
	// ancestors are the blocks that hold the node's block and whose text or excerpt holds an
	// erased text: a container's text is cut from the document across the blocks inside it.
	ancestors []string
	artifacts []string
	numbers   []int32
	events    []redactedEvent
	embedded  []embeddingRow
	owners    events.OwnerRows
	listing   listing
}

// read reads the node as the erase would remove it and every row that derives from it, and
// writes the listing of all of it and its digest onto the Erasure. It locks nothing and writes
// nothing, so a run without --confirm reads it in a read-only transaction, and a confirmed run
// reads it once it holds its locks (lockOwnersThenNode).
//
// What derives from a node is fixed by the node, never by a search for its bytes: its tuples,
// its projection rows and the rows that cannot exist without them, its own embedding rows and the
// events about it, and for a block its own document's versions, editing state, search rows and
// windows and the excerpts cut from it. A row anywhere else that holds the same bytes derives
// from another node - another issue's spec quoting a comment's `Yes`, another person's comment -
// and is that node's text: the run lists it (readLeft) and changes nothing in it.
func (e *Erasure) read(ctx context.Context, tx pgx.Tx) (erasedRead, error) {
	var read erasedRead
	if err := tx.QueryRow(ctx, `select supertag from graph_nodes where kind = $1 and id = $2`,
		e.Kind, e.ID).Scan(&e.Supertag); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return erasedRead{}, fmt.Errorf("the store holds no node %s:%s", e.Kind, e.ID)
		}
		return erasedRead{}, fmt.Errorf("read node %s:%s: %w", e.Kind, e.ID, err)
	}
	var err error
	if read.document, read.blockID, err = e.ownDocument(ctx, tx); err != nil {
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
		e.readSearch, e.readExcerpts, e.readEvents, e.readHistory, e.readBlockers, e.readLeft,
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

// readTexts reads the text the node's own rows hold, which is what the erase takes out of every
// row that derives from it: a comment's body and each text its blocks held at a revision, and a
// block's whole text at every version that recorded one (document_block_texts), not its
// excerpt, which is the block's first refs.BlockExcerptRunes runes and would leave the rest. A
// block no version recorded a text of - a row an older build wrote - is read by its excerpt, the
// one place it holds its text. A node whose rows hold no text - a block reference, a person -
// redacts nothing.
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
// the node. A value tuple is listed without its value, which can be text.
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
		far := toKind + ":" + toID
		if toKind == graphstore.KindValue {
			far = "a value"
		}
		read.listing.tuples = append(read.listing.tuples,
			fmt.Sprintf("delete graph_tuples %d: %s:%s %s %s", seq, fromKind, fromID, kind, far))
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the tuples of %s:%s: %w", e.Kind, e.ID, err)
	}
	return nil
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

// readAncestors reads the blocks of the node's document that hold its block, innermost first up
// the parent_block_id chain, and keeps those whose text at some version or whose excerpt holds
// an erased text: a container's text is the document cut across every block inside it, so it
// quotes the node, and redactBlocks rewrites it.
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
		select distinct up.block_id from up
		where exists (select 1 from document_block_texts t cross join unnest($3::text[]) erased (text)
		              where t.artifact_id = $1::uuid and t.block_id = up.block_id and position(erased.text in t.text) > 0)
		   or exists (select 1 from document_blocks b cross join unnest($3::text[]) erased (text)
		              where b.artifact_id = $1::uuid and b.block_id = up.block_id and position(erased.text in b.excerpt) > 0)
		order by 1`, read.document.artifact, read.blockID, read.texts)
	if err != nil {
		return fmt.Errorf("read the blocks holding %s:%s: %w", e.Kind, e.ID, err)
	}
	if read.ancestors, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return fmt.Errorf("read the blocks holding %s:%s: %w", e.Kind, e.ID, err)
	}
	return nil
}

// readVersions reads every version of the node's own document whose markdown holds an erased
// text, and answers the (artifact, number) pairs the deletes take. A version of any other
// document is another node's text, and stays.
func (e *Erasure) readVersions(ctx context.Context, tx pgx.Tx, read *erasedRead) error {
	e.Versions = []ErasedVersion{}
	if read.document == nil || len(read.texts) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `
		select v.number from artifact_versions v
		where v.artifact_id = $1::uuid
		  and exists (select 1 from unnest($2::text[]) as erased(text) where position(erased.text in v.markdown) > 0)
		order by v.number`, read.document.artifact, read.texts)
	if err != nil {
		return fmt.Errorf("read the versions holding the erased text: %w", err)
	}
	numbers, err := pgx.CollectRows(rows, pgx.RowTo[int32])
	if err != nil {
		return fmt.Errorf("read the versions holding the erased text: %w", err)
	}
	for _, number := range numbers {
		read.artifacts = append(read.artifacts, read.document.artifact)
		read.numbers = append(read.numbers, number)
		version := ErasedVersion{Document: read.document.refKey, Issue: read.document.issue,
			Project: read.document.project, Version: int(number)}
		e.Versions = append(e.Versions, version)
		read.listing.versions = append(read.listing.versions, fmt.Sprintf("%s version %d%s",
			version.Document, version.Version, owner(version.Issue, version.Project)))
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

// readHistory lists the node's own document's stored history, which the erase replaces whole
// with one seed of its live text redacted (docs.ErasedLiveText): a text typed piece by piece can
// be stored split across updates, so no search of those bytes can say whether the history holds
// them, and a block that holds a text was typed into it. It counts the rows and does not load
// them, since loading them takes the document's owner row and its room lock; a confirmed run
// loads them once it holds its locks (readLiveTexts).
func (e *Erasure) readHistory(ctx context.Context, tx pgx.Tx, read *erasedRead) error {
	e.Documents = []ErasedDocument{}
	if read.document == nil || len(read.texts) == 0 {
		return nil
	}
	held := ErasedDocument{Document: read.document.refKey, Issue: read.document.issue,
		Project: read.document.project, artifact: read.document.artifact}
	if err := tx.QueryRow(ctx, `
		select (select count(*) from doc_updates where artifact_id = $1::uuid),
		       (select count(*) from doc_checkpoints where artifact_id = $1::uuid),
		       (select count(*) from doc_snapshots where artifact_id = $1::uuid)`,
		read.document.artifact).Scan(&held.Updates, &held.Checkpoints, &held.Snapshots); err != nil {
		return fmt.Errorf("count the stored history of %s: %w", read.document.refKey, err)
	}
	if held.Updates+held.Checkpoints+held.Snapshots == 0 {
		return nil
	}
	e.Documents = append(e.Documents, held)
	read.listing.documents = append(read.listing.documents, fmt.Sprintf("%s%s: its %d stored update(s), %d checkpoint(s) "+
		"and %d snapshot(s) are replaced by one seed of its live text, the marker standing where the erased text was; "+
		"its anchor marks and margin records go with them; an editor open on it reloads and loses what it has not stored",
		held.Document, owner(held.Issue, held.Project), held.Updates, held.Checkpoints, held.Snapshots))
	return nil
}

// readLiveTexts loads the live state of the node's own document as the erase would leave it
// (docs.ReadErasedLiveText), which a confirmed run does once its listing matched. The document's
// owner row is one the run took before its node's row (lockInStoreOrder), so the owner row
// docs.ReadErasedLiveText takes first is one this transaction holds already, and no owner row is
// taken after the node's. It reads and writes nothing; apply replaces it (replaceLiveTexts).
func (e *Erasure) readLiveTexts(ctx context.Context, tx pgx.Tx, read erasedRead) error {
	for index := range e.Documents {
		document := &e.Documents[index]
		live, err := docs.ReadErasedLiveText(ctx, tx, document.artifact, read.texts, e.marker())
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
// owner: the node's projection rows stand under their owner (nodeOwners), and a block's own
// document - its versions and reviews, its search rows, its history, its windows, its excerpts -
// under that document's issue, or the document's own artifacts row where no issue owns it. The
// rest of what the erase writes is owed no owner row, since none of its other writers takes one:
// an event payload already written (the outbox's record of what it published takes no owner
// row), a tuple, which is appended once and never written again, the term statistics, the
// display trees, and the node's own row. It reads no graph_nodes, graph_tuples or events row, so
// lockOwnersThenNode reads it before it holds anything.
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
	return owners, nil
}

// nodeOwners reads the owner row each projection row of the node stands under: an issue's own
// comment stands under the issue, and every other row under the owner of the document it belongs
// to, which is that document's issue or, for a document no issue owns, the document's own
// artifacts row - the owner docs' lockArtifactOwner finds and events.LockOwnerRows takes. A node
// no row projects stands under none.
func nodeOwners(ctx context.Context, tx pgx.Tx, kind, id string) (events.OwnerRows, error) {
	arms := make([]string, len(projectedTables))
	for index, held := range projectedTables {
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

// readBlockers names every live row of another node that states something about this one, each
// of which refuses the run, so an operator erases or edits those first and the store census
// still reads zero once the erase runs.
//
// Two kinds of row do. A row that points at this node's projection through a nullable reference:
// a reply whose reply_to names an erased comment is somebody else's words, and an ask handed back
// through it is the ask's own history, so the erase neither deletes one nor rewrites the other.
// And a projection row of another live node whose node states a tuple into this one now: a block
// inside an erased block (its part_of and its parent_block_id), a block that cites it (its
// mentions or pointer tuple and its block_refs row), a comment its author is (its authored_by and
// its author). The erase deletes the tuples into the node, so such a row would go on stating a
// tuple the store no longer holds, which is a difference `census --store` reads.
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
	projected := make([]string, len(projectedTables))
	for index, held := range projectedTables {
		projected[index] = `exists (select 1 from ` + held.Table + ` p where p.node_kind = t.from_kind and p.node_address = t.from_id)`
	}
	rows, err := tx.Query(ctx, `
		select t.from_kind, t.from_id, t.kind from (
			`+graphstore.CurrentTupleRead(`t.from_kind, t.from_id, t.kind`, `t.from_kind, t.from_id, t.kind, t.asserted`,
		`t.to_kind = $1 and t.to_id = $2 and not (t.from_kind = $1 and t.from_id = $2)`)+`
		) t
		join graph_nodes n on n.kind = t.from_kind and n.id = t.from_id and n.state = $3
		where t.asserted and (`+strings.Join(projected, " or ")+`)
		order by 1, 2, 3`, e.Kind, e.ID, graphstore.StateLive)
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
