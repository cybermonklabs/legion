package graphcensus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/user"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/schemanodes"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// eraseApplicationName is how the erase appears in pg_stat_activity, as the census names itself:
// an operator reading the activity of a database sees which pass holds its rows.
const eraseApplicationName = "envoy-dispatch erase"

// eraseMarker stands where the erased bytes were in a row the erase rewrites rather than
// deletes, with the node the run addressed written into it. A reader of a redacted payload
// learns three things from it: those bytes went by an erase and not by a lost write, which node
// that erase addressed, and that `graph_erasures` holds the record of the run - who ran it,
// when, and everything else it removed. It names the node and never the text, so a row it marks
// holds none of what was erased.
const eraseMarker = "[erased by envoy-dispatch erase %s:%s, recorded in graph_erasures]"

// blockExcerpt is refs.BlockExcerptRunes as a statement spells it: an excerpt this file rewrites
// is cut from its block's text exactly as the indexer cuts it (refs.IndexBlocks).
var blockExcerpt = strconv.Itoa(refs.BlockExcerptRunes)

// projectedTables are the tables that carry a node's address, and so are the rows a node is
// projected onto (migration 20261109022248 gave all three `node_kind` and `node_address`). Text
// is the column of each holding what a reader of that row sees, empty for a table that holds no
// text of its own: a block reference is one link out of a block and carries none.
//
// The erase walks all three by (node_kind, node_address) rather than by the node's kind, so a
// kind that gains a projection carries no second list here to keep in step.
var projectedTables = []struct{ Table, Text string }{
	{Table: "comments", Text: "body"},
	{Table: "document_blocks", Text: "excerpt"},
	{Table: "block_refs", Text: ""},
}

// ErasedRows is one table the erase emptied of a node's rows.
type ErasedRows struct {
	Table string `json:"table"`
	Rows  int64  `json:"rows"`
}

// ErasedVersion is one document version whose markdown held the erased text, named as a reader
// addresses it. It carries no markdown: the record of an erase holds no erased text.
type ErasedVersion struct {
	Document string `json:"document"`
	Issue    string `json:"issue,omitempty"`
	Project  string `json:"project,omitempty"`
	Version  int    `json:"version"`
}

// ErasedReview is one approval whose content_sha256 covered the erased bytes. It cannot be
// rewritten - artifact_review_content_sha256_update raises on any statement that moves a stored
// digest - so the erase ends it, and names it by its document and its version.
type ErasedReview struct {
	ID       string `json:"id"`
	Document string `json:"document"`
	Issue    string `json:"issue,omitempty"`
	Version  int    `json:"version"`
	State    string `json:"state"`
}

// ErasedDocument is one document whose live state the erase replaces, named as a reader addresses
// it, with what replacing it costs. It carries no text.
type ErasedDocument struct {
	Document string `json:"document"`
	Issue    string `json:"issue,omitempty"`
	Project  string `json:"project,omitempty"`
	// Held is whether its live text held the erased text, which then reads as the marker. A
	// document whose live text held none - a version of it held the text, and an edit took it
	// out since - has its history replaced alone, since the items that edit deleted can still
	// hold the bytes there.
	Held bool `json:"live_text_held"`
	// Left is whether its live text holds the erased bytes where its markdown spells them
	// otherwise than the erased text is written - an escaped `*` or `_`, for one - so no marker
	// can stand where they are. Such a document refuses the erase (Unredacted).
	Left bool `json:"live_bytes_left"`
	// Updates is how many stored updates its history held, all of which one seed replaces;
	// AnchorMarks and MarginRecords are what goes with them (LiveText).
	Updates       int64 `json:"updates"`
	AnchorMarks   int   `json:"anchor_marks"`
	MarginRecords int   `json:"margin_records"`
	live          *LiveText
}

// Stores are the writers of the stores the erase reaches that belong to packages this one cannot
// import: docs' own tests import this package, so it cannot import docs, nor api, which imports
// docs. cmd/dispatch fills them (eraseStores), and each runs inside the erase's own transaction,
// so a failure in any of them rolls the whole erase back, nodes and all.
type Stores struct {
	// LiveText reads the live state of one document as the erase would leave it: each of texts
	// replaced by marker in its live text, seeded as its one stored update in place of its
	// history (docs.ReadErasedLiveText). It answers nil for a document with no stored history,
	// and writes nothing until the LiveText it answers is replaced.
	LiveText func(ctx context.Context, tx pgx.Tx, artifactID string, texts []string, marker string) (*LiveText, error)
	// TermStatistics counts the term statistics again, as the items stand in the transaction,
	// of projects and of every project an item the transaction changed belongs to, deleting the
	// search_term_queue rows the transaction wrote, whose old vectors hold the erased words
	// (api.RecountSearchTermStatsTx).
	TermStatistics func(ctx context.Context, tx pgx.Tx, projects []string) (TermRecount, error)
	// TreeStoreBound is how long after the commit a read that began before it can still store
	// the display tree of a text the erase removed (api.StoreTreesTimeout): a read stores the
	// trees it read on a goroutine of its own, detached from the request and bounded by that
	// timeout. The erase waits it out and deletes those trees once more (sweepTrees).
	TreeStoreBound time.Duration
}

// LiveText is one document's live state as the erase would leave it (Stores.LiveText).
type LiveText struct {
	// Held is whether the live text holds one of the erased texts.
	Held bool
	// Left is whether the seed would still hold an erased text's bytes (ErasedDocument.Left).
	Left bool
	// Updates is how many stored updates the history holds now.
	Updates int64
	// AnchorMarks and MarginRecords are the comment, suggestion and ask anchors of the live text
	// and the records of its margin, which a seed parsed from markdown does not carry.
	AnchorMarks, MarginRecords int
	// Text is the live text as the history holds it now, whose display tree goes with it.
	Text string
	// Replace writes the seed in place of the history and announces it, so a room of the
	// document that a server holds is closed and reloaded.
	Replace func(ctx context.Context, tx pgx.Tx) (ReplacedHistory, error)
}

// ReplacedHistory is how many rows of each table a document's replaced history held.
type ReplacedHistory struct {
	Updates, Checkpoints, Snapshots int64
}

// TermRecount is what counting the term statistics again did: the projects it counted, and how
// many search_term_queue rows of the erase's own transaction it deleted.
type TermRecount struct {
	Projects []string
	Queued   int64
}

// Operator is who ran the erase: the database role the credential is, and the account and host
// the command ran from. Only a person holding the deployment's database credential runs it, so
// this is the whole of who that was.
type Operator struct {
	DatabaseUser string `json:"database_user"`
	SystemUser   string `json:"system_user,omitempty"`
	Host         string `json:"host,omitempty"`
}

// Erasure is one run of `envoy-dispatch erase`: the node it addressed, everything that would
// go with it, and - once Applied - everything that did.
type Erasure struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Supertag string `json:"supertag"`
	// Tuples is how many tuples state something out of the node, Inbound how many state
	// something whose far end it is.
	Tuples  int64 `json:"tuples"`
	Inbound int64 `json:"inbound_tuples"`
	// Rows are the projection rows of the node and the rows that cannot exist without them.
	Rows []ErasedRows `json:"rows"`
	// RedactedRows are the rows the run rewrote where it may take no row away: a block of a
	// document that quoted the erased text, whose row projects a block node of its own, the
	// append-only event log, the one stored update each replaced history is seeded with, and the
	// search vectors written again from the latest version left: a document's search row and its
	// issue's own vector where the document is the issue's spec. Each that held the bytes holds
	// the marker where they were.
	RedactedRows []ErasedRows `json:"redacted"`
	// Versions and Reviews are the document versions whose markdown holds the erased text and
	// the approvals whose digest covered those bytes.
	Versions []ErasedVersion `json:"versions"`
	Reviews  []ErasedReview  `json:"reviews"`
	// Documents are the documents whose live state the erase replaces, and Recounted the
	// projects whose term statistics it counted again.
	Documents []ErasedDocument `json:"documents"`
	Recounted []string         `json:"recounted"`
	// Blockers are the rows of other things that point at this node's projection and are not
	// part of it: a reply to an erased comment, an ask handed back through it. Each refuses the
	// run, because deleting them would remove somebody else's words and rewriting them would
	// state something nobody said.
	Blockers []string `json:"-"`
	// Applied is whether this run removed anything. A run with no --confirm reads everything
	// above and rolls back, which is how the command refuses until it has listed what it would
	// remove.
	Applied bool      `json:"-"`
	RanBy   Operator  `json:"-"`
	RanAt   time.Time `json:"-"`
	// Swept is how many display trees the sweep after the commit deleted (sweepTrees): each is
	// one a read that began before the commit stored back inside the window it waits out.
	Swept int64 `json:"-"`
	// sums are the keys of the display trees the run deleted (treeSums). They are held here and
	// nowhere else - no row, no record, no log - since each is the digest of an erased text, and
	// they go with the process once the sweep has read them.
	sums [][]byte
}

// Unredacted names every document whose live text holds the erased bytes where its markdown
// spells them otherwise (ErasedDocument.Left). The erase refuses while one is listed: the seed of
// its history would hold those bytes, and a run that reported success over them would not be
// an erase. An edit that takes them out of the document is what lets the erase run.
func (e *Erasure) Unredacted() []string {
	var documents []string
	for _, document := range e.Documents {
		if document.Left {
			documents = append(documents, document.Document)
		}
	}
	return documents
}

// ParseNode reads the `<kind>:<id>` the --node flag takes. The kind is the node's kind as
// graph_nodes keys it and holds no colon; everything after the first one is the id, so a page's
// `page:role/<topic>` and a comment's uuid are both read whole.
func ParseNode(value string) (kind, id string, err error) {
	kind, id, found := strings.Cut(strings.TrimSpace(value), ":")
	if !found || strings.TrimSpace(kind) == "" || strings.TrimSpace(id) == "" {
		return "", "", fmt.Errorf("--node takes <kind>:<id>, not %q", value)
	}
	return strings.TrimSpace(kind), strings.TrimSpace(id), nil
}

// Erase removes one node and the bytes that stand on it, in one transaction: the node's tuples,
// the tuples whose far end it is, every projection row of it and the rows that cannot exist
// without one, the `artifact_versions` rows whose markdown holds its text, and each
// `artifact_reviews` row whose `content_sha256` covered those bytes. It is the one path in this
// tree that deletes a tuple row or a node row; every other writer appends. It takes the bytes
// out of every other store that holds them in the same transaction: the blocks and the events
// that quoted them (redact), the live state of each document that held them (readDocuments,
// replaceLiveTexts), a document's search row and its spec's issue vector (restateSearch), the
// embedding rows and windows cut from them (forgetEmbeddings, restateEmbeddings), the term
// statistics (Stores.TermStatistics) and the stored display trees (forgetTrees).
//
// It reads what it would remove first and refuses to remove anything until confirmed is true,
// so an operator reads every version and every review the erase would end - each by its document
// and its version - and every document whose live state it would replace, with what that costs,
// before a byte goes. A run that is not confirmed rolls its transaction back and has written
// nothing. It refuses, confirmed or not, while a document's live text holds the erased bytes
// where its markdown spells them otherwise (Unredacted), since its seed would keep them.
//
// It refuses a type node, a supertag or an attribute, before it opens a connection: a type is
// data the block schema seeds (migration 20261109051232, pmdoc/schemanodes), and no route and no
// command creates, retitles or deletes one (R6). Such a node holds no text anybody wrote, so no
// erasure is owed for it, and deleting one would take a type out of the schema every server
// installs.
//
// What it did goes into `graph_erasures`: what was removed, who ran it and when, and no erased
// text. Nobody addresses that row and no read of the graph answers it.
//
// Once it has committed it waits stores.TreeStoreBound and deletes the display trees it deleted
// once more (sweepTrees). An error there comes back beside the Erasure, which is then Applied:
// the erase stands, and what the sweep could not do is the error's to say.
func Erase(ctx context.Context, databaseURL, kind, id string, confirmed bool, stores Stores) (*Erasure, error) {
	if kind == schemanodes.KindSupertag || kind == schemanodes.KindAttribute {
		return nil, fmt.Errorf("the erase refuses %s:%s: a type node is data the block schema seeds, and no command deletes it", kind, id)
	}
	if stores.LiveText == nil || stores.TermStatistics == nil {
		return nil, errors.New("the erase needs the writers of the live documents and the term statistics (Stores)")
	}
	conn, err := connect(ctx, databaseURL, eraseApplicationName)
	if err != nil {
		return nil, err
	}
	defer conn.Close(context.Background())
	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(context.Background())

	erasure := &Erasure{Kind: kind, ID: id, RanAt: time.Now().UTC()}
	if err := tx.QueryRow(ctx,
		`select supertag from graph_nodes where kind = $1 and id = $2 for update`,
		kind, id).Scan(&erasure.Supertag); err != nil {
		if err == pgx.ErrNoRows {
			return nil, fmt.Errorf("the store holds no node %s:%s", kind, id)
		}
		return nil, fmt.Errorf("read node %s:%s: %w", kind, id, err)
	}
	if err := tx.QueryRow(ctx, `select current_user`).Scan(&erasure.RanBy.DatabaseUser); err != nil {
		return nil, fmt.Errorf("read the database user: %w", err)
	}
	if account, err := user.Current(); err == nil {
		erasure.RanBy.SystemUser = account.Username
	}
	if host, err := os.Hostname(); err == nil {
		erasure.RanBy.Host = host
	}

	texts, err := erasedTexts(ctx, tx, kind, id)
	if err != nil {
		return nil, err
	}
	artifacts, numbers, err := erasedVersions(ctx, tx, texts, erasure)
	if err != nil {
		return nil, err
	}
	if err := erasedReviews(ctx, tx, artifacts, numbers, erasure); err != nil {
		return nil, err
	}
	if err := erasure.countTuples(ctx, tx); err != nil {
		return nil, err
	}
	if err := erasure.readBlockers(ctx, tx, kind, id); err != nil {
		return nil, err
	}
	if err := erasure.readDocuments(ctx, tx, stores, texts, artifacts); err != nil {
		return nil, err
	}
	if !confirmed || len(erasure.Blockers) > 0 || len(erasure.Unredacted()) > 0 {
		return erasure, nil
	}
	if err := erasure.apply(ctx, tx, stores, texts, artifacts, numbers); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	erasure.Applied = true
	if err := erasure.sweepTrees(ctx, conn, stores.TreeStoreBound); err != nil {
		return erasure, err
	}
	return erasure, nil
}

// erasedTexts is the text the node's own rows hold, which is what the erase looks for in the
// document versions and in every store it rewrites. A node whose projection holds no text - a
// block reference, or a node with no projection row at all - erases no version and redacts
// nothing, and the run then removes the node alone.
func erasedTexts(ctx context.Context, tx pgx.Tx, kind, id string) ([]string, error) {
	var texts []string
	for _, held := range projectedTables {
		if held.Text == "" {
			continue
		}
		rows, err := tx.Query(ctx, `select coalesce(`+held.Text+`, '') from `+held.Table+
			` where node_kind = $1 and node_address = $2`, kind, id)
		if err != nil {
			return nil, fmt.Errorf("read %s rows of %s:%s: %w", held.Table, kind, id, err)
		}
		found, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return nil, fmt.Errorf("read %s rows of %s:%s: %w", held.Table, kind, id, err)
		}
		for _, text := range found {
			if strings.TrimSpace(text) != "" {
				texts = append(texts, text)
			}
		}
	}
	return texts, nil
}

// erasedVersions reads every document version whose markdown holds one of the erased texts, and
// answers the (artifact, number) pairs the review read takes.
func erasedVersions(ctx context.Context, tx pgx.Tx, texts []string, erasure *Erasure) ([]string, []int32, error) {
	erasure.Versions = []ErasedVersion{}
	if len(texts) == 0 {
		return nil, nil, nil
	}
	rows, err := tx.Query(ctx, `
		select v.artifact_id::text, v.number, a.ref_key, coalesce(a.issue_key, ''), coalesce(a.project_key, '')
		from artifact_versions v join artifacts a on a.id = v.artifact_id
		where exists (
			select 1 from unnest($1::text[]) as erased(text)
			where position(erased.text in v.markdown) > 0
		)
		order by a.ref_key, v.number`, texts)
	if err != nil {
		return nil, nil, fmt.Errorf("read the versions holding the erased text: %w", err)
	}
	defer rows.Close()
	var artifacts []string
	var numbers []int32
	for rows.Next() {
		var artifact, refKey, issue, project string
		var number int32
		if err := rows.Scan(&artifact, &number, &refKey, &issue, &project); err != nil {
			return nil, nil, fmt.Errorf("scan a version holding the erased text: %w", err)
		}
		artifacts = append(artifacts, artifact)
		numbers = append(numbers, number)
		erasure.Versions = append(erasure.Versions, ErasedVersion{
			Document: refKey, Issue: issue, Project: project, Version: int(number),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("read the versions holding the erased text: %w", err)
	}
	return artifacts, numbers, nil
}

// erasedReviews reads every approval standing on one of those versions. Its content_sha256 is
// the hash of exactly the bytes about to go, so the approval cannot survive them: the trigger
// refuses to move a stored digest, and an approval over bytes nobody can reproduce is a claim
// about a text that no longer exists.
func erasedReviews(ctx context.Context, tx pgx.Tx, artifacts []string, numbers []int32, erasure *Erasure) error {
	erasure.Reviews = []ErasedReview{}
	if len(artifacts) == 0 {
		return nil
	}
	// Ordered as docs.ReviewOrder orders every read of artifact_reviews in this tree: a review
	// that committed later outranks one that committed first, since created_at is its
	// transaction's start and its id is random (api's review_order_guard_test.go). The order is
	// spelled out rather than imported, because docs' own tests read this package and an import
	// the other way would be a cycle.
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
		artifacts, numbers)
	if err != nil {
		return fmt.Errorf("read the reviews over the erased bytes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var review ErasedReview
		if err := rows.Scan(&review.ID, &review.Document, &review.Issue, &review.Version, &review.State); err != nil {
			return fmt.Errorf("scan a review over the erased bytes: %w", err)
		}
		erasure.Reviews = append(erasure.Reviews, review)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the reviews over the erased bytes: %w", err)
	}
	return nil
}

// countTuples reads how many tuples each side of the node holds, before any of them goes.
func (e *Erasure) countTuples(ctx context.Context, tx pgx.Tx) error {
	if err := tx.QueryRow(ctx,
		`select count(*) from graph_tuples where from_kind = $1 and from_id = $2`,
		e.Kind, e.ID).Scan(&e.Tuples); err != nil {
		return fmt.Errorf("count the tuples out of %s:%s: %w", e.Kind, e.ID, err)
	}
	if err := tx.QueryRow(ctx,
		`select count(*) from graph_tuples where to_kind = $1 and to_id = $2`,
		e.Kind, e.ID).Scan(&e.Inbound); err != nil {
		return fmt.Errorf("count the tuples into %s:%s: %w", e.Kind, e.ID, err)
	}
	return nil
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
	defer rows.Close()
	var references []referencingColumn
	for rows.Next() {
		var reference referencingColumn
		if err := rows.Scan(&reference.Table, &reference.Column, &reference.Referenced, &reference.Required); err != nil {
			return nil, fmt.Errorf("scan a foreign key into %s: %w", table, err)
		}
		references = append(references, reference)
	}
	if err := rows.Err(); err != nil {
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

// readBlockers names every row that points at this node's projection and is not part of it. A
// nullable reference is such a row: a reply whose reply_to names an erased comment is somebody
// else's words, and an ask handed back through it is the ask's own history, so the erase refuses
// rather than deleting one or rewriting the other.
func (e *Erasure) readBlockers(ctx context.Context, tx pgx.Tx, kind, id string) error {
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
				reference.Column+` in (`+projectionRows(reference, held.Table)+`)`, kind, id).Scan(&rows); err != nil {
				return fmt.Errorf("count the %s.%s rows at %s:%s: %w", reference.Table, reference.Column, kind, id, err)
			}
			if rows > 0 {
				e.Blockers = append(e.Blockers, fmt.Sprintf("%d %s row(s) point at this node through %s.%s",
					rows, reference.Table, reference.Table, reference.Column))
			}
		}
	}
	return nil
}

// readDocuments reads the live state of every document the erased text stands in, as the erase
// would leave it (Stores.LiveText), in artifact id order, which is the order two erases take the
// documents' locks in. Those documents are the ones a version the run deletes belongs to, the ones
// whose block text holds the erased text, and the ones whose stored history, a pending prune's
// checkpoint or a snapshot holds its bytes whole: an edit nothing has versioned yet stands there
// alone. A text a person typed piece by piece can be stored split across updates, so no search of
// those bytes can be the whole list, which is why the versions and the blocks are on it as well.
//
// Every one of them has its history replaced, a document whose live text no longer holds the
// erased text included, since an edit that took the text out leaves its bytes in the items it
// deleted until a fold collects them. It reads, and writes nothing: each LiveText is replaced in
// apply (replaceLiveTexts).
func (e *Erasure) readDocuments(ctx context.Context, tx pgx.Tx, stores Stores, texts, artifacts []string) error {
	e.Documents = []ErasedDocument{}
	if len(texts) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `
		with held (artifact_id) as (
			select unnest($1::uuid[])
			union
			select b.artifact_id from document_block_texts b
			 where exists (select 1 from unnest($2::text[]) erased(text) where position(erased.text in b.text) > 0)
			union
			select u.artifact_id from doc_updates u
			 where exists (select 1 from unnest($2::text[]) erased(text)
			               where position(convert_to(erased.text, 'UTF8') in u.update) > 0)
			union
			select c.artifact_id from doc_checkpoints c
			 where exists (select 1 from unnest($2::text[]) erased(text)
			               where position(convert_to(erased.text, 'UTF8') in c.state) > 0)
			union
			select s.artifact_id from doc_snapshots s
			 where exists (select 1 from unnest($2::text[]) erased(text)
			               where position(convert_to(erased.text, 'UTF8') in s.state) > 0)
		)
		select a.id::text, a.ref_key, coalesce(a.issue_key, ''), coalesce(a.project_key, '')
		from artifacts a join held on held.artifact_id = a.id
		where a.kind = 'doc'
		order by a.id`, artifacts, texts)
	if err != nil {
		return fmt.Errorf("read the documents holding the erased text: %w", err)
	}
	type held struct{ artifact, document, issue, project string }
	documents, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (held, error) {
		var document held
		err := row.Scan(&document.artifact, &document.document, &document.issue, &document.project)
		return document, err
	})
	if err != nil {
		return fmt.Errorf("read the documents holding the erased text: %w", err)
	}
	for _, document := range documents {
		live, err := stores.LiveText(ctx, tx, document.artifact, texts, e.marker())
		if err != nil {
			return fmt.Errorf("read the live state of %s: %w", document.document, err)
		}
		if live == nil {
			continue
		}
		e.Documents = append(e.Documents, ErasedDocument{
			Document: document.document, Issue: document.issue, Project: document.project,
			Held: live.Held, Left: live.Left, Updates: live.Updates, AnchorMarks: live.AnchorMarks, MarginRecords: live.MarginRecords,
			live: live,
		})
	}
	return nil
}

// eraseBlockTexts deletes the whole text of every block the node projects onto, which is the
// most of the bytes an erased block holds. document_block_texts is keyed by
// (artifact_id, block_id, version) and carries a foreign key to artifacts alone (migration
// 0121), so the walk of single-column references into a projected table never names it and
// the text would outlive the node that held it. It is deleted here by the pair the block's
// own document_blocks row holds, while that row is still there, for every version of the
// block, and counted like any other table the run emptied. A node with no document_blocks
// row - a comment, a person - names no pair and deletes nothing.
func (e *Erasure) eraseBlockTexts(ctx context.Context, tx pgx.Tx) error {
	tag, err := tx.Exec(ctx, `
		delete from document_block_texts erased
		where (erased.artifact_id, erased.block_id) in (
			select held.artifact_id, held.block_id from document_blocks held
			where held.node_kind = $1 and held.node_address = $2
		)`, e.Kind, e.ID)
	if err != nil {
		return fmt.Errorf("delete the texts of the blocks of %s:%s: %w", e.Kind, e.ID, err)
	}
	e.removed("document_block_texts", tag.RowsAffected())
	return nil
}

// marker is eraseMarker naming the node this run addressed, which is what stands in every row
// the run rewrites.
func (e *Erasure) marker() string { return fmt.Sprintf(eraseMarker, e.Kind, e.ID) }

// placeholders is `$1 … $n` for one redaction: the marker first and one for each erased text
// after it, which is the order redactionArgs binds them in.
func placeholders(texts []string) []string {
	bound := make([]string, len(texts)+1)
	for i := range bound {
		bound[i] = "$" + strconv.Itoa(i+1)
	}
	return bound
}

// redactionArgs are those parameters' values.
func redactionArgs(marker string, texts []string) []any {
	args := make([]any, 0, len(texts)+1)
	args = append(args, marker)
	for _, text := range texts {
		args = append(args, text)
	}
	return args
}

// jsonStrings is each of those parameters as a jsonb rendered to text holds it: the value's own
// json escaping, spelled by the database rather than by this package, so what a statement looks
// for in a payload is byte for byte what that payload holds. to_jsonb wraps a string in quotes,
// which the substring cuts back off.
func jsonStrings(bound []string) []string {
	escaped := make([]string, len(bound))
	for i, value := range bound {
		rendered := "to_jsonb(" + value + "::text)::text"
		escaped[i] = "substring(" + rendered + " from 2 for length(" + rendered + ") - 2)"
	}
	return escaped
}

// redaction is one column with every erased text replaced by the marker, as a statement spells
// it: one replace() around the last, so a row holding two of those texts is rewritten by one
// pass and counted once. values is placeholders, or jsonStrings of them for a column a
// statement reads as json.
func redaction(column string, values []string) string {
	rewritten := column
	for _, text := range values[1:] {
		rewritten = "replace(" + rewritten + ", " + text + ", " + values[0] + ")"
	}
	return rewritten
}

// redact rewrites the stores that hold the erased text and that the erase may take no row out
// of: the blocks of the documents that quoted it, whose rows project block nodes of their own,
// and the append-only event log. It runs after the deletes, so a row this run has already
// removed is not rewritten first and counted as both.
func (e *Erasure) redact(ctx context.Context, tx pgx.Tx, texts []string) error {
	if len(texts) == 0 {
		return nil
	}
	if err := e.redactBlocks(ctx, tx, texts); err != nil {
		return err
	}
	return e.redactEvents(ctx, tx, texts)
}

// redactBlocks rewrites every block that held the erased text: the text itself, and the excerpt
// of the document_blocks row that text was cut for. Those rows are rewritten and not deleted
// because each projects a block node of its own - a paragraph of a document that quoted the
// node, not the node this run addressed - and a live node no row projects is a difference the
// store census reads (c-census).
//
// An excerpt is cut from the rewritten text the way the indexer cuts it (refs.IndexBlocks), so
// a reader of the excerpt and a reader of the text are told the same thing, and an excerpt that
// held only the first part of a long erased text is rewritten too. A block that rewrite does
// not reach - one whose text document_block_texts does not hold - has its excerpt rewritten by
// the statement after it.
//
// text_sha256 stands as it is. It is the digest of the bytes the version stated, which is what
// the indexer compares a later version's block against: moved to the digest of the redacted
// text, a version restating the erased bytes would read as a change and write them back in.
func (e *Erasure) redactBlocks(ctx context.Context, tx pgx.Tx, texts []string) error {
	bound := placeholders(texts)
	args := redactionArgs(e.marker(), texts)
	rewritten := redaction("held.text", bound)
	var blockTexts, recut int64
	if err := tx.QueryRow(ctx, `
		with redacted as (
			update document_block_texts held set text = `+rewritten+`
			where held.text <> `+rewritten+`
			returning held.artifact_id, held.block_id, held.version, held.text
		), recut as (
			update document_blocks block set excerpt = left(redacted.text, `+blockExcerpt+`)
			from redacted
			where redacted.artifact_id = block.artifact_id and redacted.block_id = block.block_id
			  and redacted.version = block.changed_version
			  and block.excerpt <> left(redacted.text, `+blockExcerpt+`)
			returning 1
		)
		select (select count(*) from redacted), (select count(*) from recut)`,
		args...).Scan(&blockTexts, &recut); err != nil {
		return fmt.Errorf("redact the block texts holding the text of %s:%s: %w", e.Kind, e.ID, err)
	}
	excerpts := redaction("excerpt", bound)
	tag, err := tx.Exec(ctx, `update document_blocks set excerpt = `+excerpts+
		` where excerpt <> `+excerpts, args...)
	if err != nil {
		return fmt.Errorf("redact the block excerpts holding the text of %s:%s: %w", e.Kind, e.ID, err)
	}
	e.redacted("document_block_texts", blockTexts)
	e.redacted("document_blocks", recut+tag.RowsAffected())
	return nil
}

// redactEvents rewrites the payload of every event that held the erased text. The event log is
// append-only - R5 redacts there rather than deleting - so the statement moves the payload and
// nothing else: the row stands with its id, its time and its actor, because what the log says
// happened and who did it is the record, and the bytes inside it are all the erase takes. A
// text a payload holds is json escaped, so each is looked for as that payload spells it, and
// the whole of the payload is searched, a text written into a key going with one written into
// a value.
func (e *Erasure) redactEvents(ctx context.Context, tx pgx.Tx, texts []string) error {
	rewritten := redaction("payload::text", jsonStrings(placeholders(texts)))
	tag, err := tx.Exec(ctx, `update events set payload = (`+rewritten+`)::jsonb
		where payload::text <> `+rewritten, redactionArgs(e.marker(), texts)...)
	if err != nil {
		return fmt.Errorf("redact the event payloads holding the text of %s:%s: %w", e.Kind, e.ID, err)
	}
	e.redacted("events", tag.RowsAffected())
	return nil
}

// treeSums is the key of the stored display tree of every whole text the run removes or rewrites:
// the node's own texts, the markdown of each version it deletes, each string of an event payload
// that redactEvents rewrites, and the live text of each document whose history it replaces.
// text_trees keys a tree by the SHA-256 of its text's UTF-8 bytes (pmdoc.TextSum, migration
// 20261105121325) and holds the text's words in the tree, so each of those texts leaves a row
// holding the erased bytes once its own row is gone. It is read before anything goes, while the
// versions and the payloads still hold those texts.
func (e *Erasure) treeSums(ctx context.Context, tx pgx.Tx, texts, artifacts []string, numbers []int32) ([][]byte, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	whole := append([]string(nil), texts...)
	for _, document := range e.Documents {
		whole = append(whole, document.live.Text)
	}
	bound := placeholders(texts)
	args := append(redactionArgs(e.marker(), texts), whole, artifacts, numbers)
	wholeAt, artifactsAt, numbersAt := len(bound)+1, len(bound)+2, len(bound)+3
	payload := redaction("events.payload::text", jsonStrings(bound))
	member := redaction("(member #>> '{}')", bound)
	rows, err := tx.Query(ctx, fmt.Sprintf(`
		select sum from (
			select sha256(convert_to(whole.text, 'UTF8')) as sum from unnest($%d::text[]) as whole(text)
			union
			select sha256(convert_to(v.markdown, 'UTF8')) from artifact_versions v
			  join unnest($%d::uuid[], $%d::int[]) as doomed(artifact, number)
			    on doomed.artifact = v.artifact_id and doomed.number = v.number
			 where v.markdown is not null
			union
			select sha256(convert_to(member #>> '{}', 'UTF8'))
			  from events cross join lateral jsonb_path_query(events.payload, 'strict $.**') as member
			 where events.payload::text <> %s
			   and jsonb_typeof(member) = 'string' and %s <> member #>> '{}'
		) trees`, wholeAt, artifactsAt, numbersAt, payload, member), args...)
	if err != nil {
		return nil, fmt.Errorf("read the display trees of the texts holding the text of %s:%s: %w", e.Kind, e.ID, err)
	}
	sums, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		return nil, fmt.Errorf("read the display trees of the texts holding the text of %s:%s: %w", e.Kind, e.ID, err)
	}
	return sums, nil
}

// restateSearch writes again the two search vectors that stand on a document's latest version,
// for each document a version of which the run deleted, and answers the projects of the
// documents whose row it moved, which the term statistics are counted again for.
//
// document_search holds each document's latest version with markdown (migration
// 20261101182447), and 20261101182454's trigger writes it on a version's insert or on an update of
// its markdown or its search, under a guard that keeps a later version's row from an older one.
// A delete of the latest version is the one write that lowers the latest, and no trigger sees it,
// so the row the run's delete left standing on a gone version goes, and the latest version left
// is touched where its search is, which has that trigger write the row as it writes every other
// row of the table. A document with no version left keeps no row, as one that never had a version
// with markdown has none.
//
// An issue's own search vector holds its spec's text (issue_embedding_spec, migration
// 20261105145242), written by the trigger on the spec's version insert, so an issue whose spec
// lost a version has that vector written again by the statement that trigger runs.
func (e *Erasure) restateSearch(ctx context.Context, tx pgx.Tx, artifacts []string) ([]string, error) {
	if len(artifacts) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `
		delete from document_search d
		where d.artifact_id = any($1::uuid[])
		  and not exists (select 1 from artifact_versions v where v.artifact_id = d.artifact_id and v.number = d.number)
		returning d.artifact_id::text`, artifacts)
	if err != nil {
		return nil, fmt.Errorf("delete the search rows of the versions that went: %w", err)
	}
	stale, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("delete the search rows of the versions that went: %w", err)
	}
	var restated int64
	if len(stale) > 0 {
		tag, err := tx.Exec(ctx, `
			update artifact_versions v set search = v.search
			where (v.artifact_id, v.number) in (
				select distinct on (latest.artifact_id) latest.artifact_id, latest.number
				from artifact_versions latest
				where latest.artifact_id = any($1::uuid[]) and latest.markdown is not null
				order by latest.artifact_id, latest.number desc)`, stale)
		if err != nil {
			return nil, fmt.Errorf("write the search rows of the latest versions left: %w", err)
		}
		restated = tag.RowsAffected()
	}
	e.removed("document_search", int64(len(stale))-restated)
	e.redacted("document_search", restated)
	tag, err := tx.Exec(ctx, `
		update issues i set search = issue_search_vector(i.key, i.title, issue_embedding_spec(i.key))
		from artifacts a
		where a.issue_key = i.key and a.is_primary and a.id = any($1::uuid[])`, artifacts)
	if err != nil {
		return nil, fmt.Errorf("write the search vectors of the issues whose spec lost a version: %w", err)
	}
	e.redacted("issues", tag.RowsAffected())
	rows, err = tx.Query(ctx, `
		select distinct a.project_key from artifacts a
		where a.id = any($1::uuid[]) and a.project_key is not null
		order by 1`, stale)
	if err != nil {
		return nil, fmt.Errorf("read the projects of the documents whose search rows moved: %w", err)
	}
	projects, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("read the projects of the documents whose search rows moved: %w", err)
	}
	return projects, nil
}

// forgottenEmbeddings is what forgetEmbeddings deleted that a later step of the same run writes
// again once the versions holding the erased text are gone (restateEmbeddings): the issues whose
// embedding row held it, and the documents whose windows did.
type forgottenEmbeddings struct {
	issues, documents []string
}

// forgetEmbeddings deletes every row of the embedding store that holds the erased bytes or
// stands on a block whose text the run erases or rewrites, and answers what of it a later step
// writes again. Nothing else of the erase reaches that store: embeddings carries no foreign key
// to the rows its text was cut from, so no delete reaches it through one, and the text would
// stand in text_snapshot, and in the vector the embedder made of it, for as long as the row did.
//
// The rows it deletes are:
//
//   - the node's own: its whole-text row, which an older build wrote under the node's kind and
//     id, and for a comment, a message or an ask every window of its blocks, under the window
//     kind and the owner key refs names them by (refs.ItemKind.WindowKind, WindowOwnerKey),
//     with the row embedding_window_states keeps for those windows, whose state is the digest
//     of the erased body;
//   - every document window covering a block whose text the run erases (eraseBlockTexts) or
//     rewrites (redactBlocks), found by its covers column, which names the blocks the window's
//     text came from, so a block a window holds in pieces is reached as well as one it holds
//     whole;
//   - every document window, whole-text document row and issue row whose text holds one of the
//     erased texts, the test redactBlocks and erasedVersions read by. An issue's row holds its
//     title and its spec's latest version (issue_embedding_text, migration 0140).
//
// An item the run does not rewrite keeps its windows: another comment quoting the erased text is
// somebody else's words, which the erase leaves as they are, and its windows written again would
// hold the same bytes.
//
// It runs before anything goes, while the node's block rows and the block texts the run rewrites
// still hold what finds them, as treeSums does.
func (e *Erasure) forgetEmbeddings(ctx context.Context, tx pgx.Tx, texts []string) (forgottenEmbeddings, error) {
	var windowKind, ownerKey string
	if kind := refs.ItemKind(e.Kind); kind == refs.ItemComment || kind == refs.ItemMessage || kind == refs.ItemAsk {
		windowKind, ownerKey = kind.WindowKind(), kind.WindowOwnerKey(e.ID)
	}
	rows, err := tx.Query(ctx, `
		with erased_blocks (artifact_id, block_id) as (
			select held.artifact_id, held.block_id from document_blocks held
			where held.node_kind = $1 and held.node_address = $2
			union
			select quoted.artifact_id, quoted.block_id from document_block_texts quoted
			where exists (select 1 from unnest($5::text[]) erased(text) where position(erased.text in quoted.text) > 0)
		)
		delete from embeddings held
		where (held.kind = $1 and held.id = $2)
		   or (held.kind = $3 and split_part(held.id, '#', 1) = $4)
		   or (held.kind in ('issue', 'document', $6)
		       and exists (select 1 from unnest($5::text[]) erased(text) where position(erased.text in held.text_snapshot) > 0))
		   or (held.kind = $6 and exists (
		       select 1 from erased_blocks block
		       where split_part(held.id, '#', 1) = block.artifact_id::text and block.block_id = any(held.covers)))
		returning held.kind, held.id`, e.Kind, e.ID, windowKind, ownerKey, texts, refs.DocumentWindowKind)
	if err != nil {
		return forgottenEmbeddings{}, fmt.Errorf("delete the embedding rows holding the text of %s:%s: %w", e.Kind, e.ID, err)
	}
	deleted, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) ([2]string, error) {
		var held [2]string
		err := row.Scan(&held[0], &held[1])
		return held, err
	})
	if err != nil {
		return forgottenEmbeddings{}, fmt.Errorf("delete the embedding rows holding the text of %s:%s: %w", e.Kind, e.ID, err)
	}
	var forgotten forgottenEmbeddings
	documents := map[string]struct{}{}
	for _, held := range deleted {
		switch held[0] {
		case "issue":
			forgotten.issues = append(forgotten.issues, held[1])
		case refs.DocumentWindowKind:
			owner, _, _ := strings.Cut(held[1], "#")
			documents[owner] = struct{}{}
		}
	}
	forgotten.documents = slices.Sorted(maps.Keys(documents))
	e.removed("embeddings", int64(len(deleted)-len(forgotten.issues)))
	if windowKind != "" {
		tag, err := tx.Exec(ctx, `delete from embedding_window_states where kind = $1 and owner_key = $2`, windowKind, ownerKey)
		if err != nil {
			return forgottenEmbeddings{}, fmt.Errorf("delete the window state of %s:%s: %w", e.Kind, e.ID, err)
		}
		e.removed("embedding_window_states", tag.RowsAffected())
	}
	return forgotten, nil
}

// restateEmbeddings writes again what forgetEmbeddings deleted that stands for something the run
// leaves, once the versions holding the erased text are gone.
//
// An issue's row is written by the statement migration 0140's trigger runs on a version of the
// issue's spec (embeddings_enqueue_issue_spec): embeddings_enqueue of issue_embedding_text over
// the issue's title and its spec's latest version left. The row is a new one, so the queue embeds
// it afresh and no vector of the erased text stands for the issue meanwhile; each is counted as
// rewritten, and one whose issue is gone as removed.
//
// A document whose windows went is marked owed them, keeping a mark already set as refs'
// standsAtVersion keeps it, which is how the block-index tick's window arm learns of an owner
// whose windows were removed outside the one statement that writes them (refs.DocumentsOwedWindows):
// the arm writes them from the latest version left, which holds none of the erased text either.
// A document with no markdown version left is not marked: the arm writes nothing for it
// (docs' windowDocument) and would leave the mark standing.
func (e *Erasure) restateEmbeddings(ctx context.Context, tx pgx.Tx, forgotten forgottenEmbeddings) error {
	if len(forgotten.issues) > 0 {
		tag, err := tx.Exec(ctx, `
			select embeddings_enqueue('issue', i.key, issue_embedding_text(i.title, issue_embedding_spec(i.key)))
			from issues i where i.key = any($1::text[])
			order by i.key`, forgotten.issues)
		if err != nil {
			return fmt.Errorf("write the embedding rows of the issues whose spec lost a version: %w", err)
		}
		e.redacted("embeddings", tag.RowsAffected())
		e.removed("embeddings", int64(len(forgotten.issues))-tag.RowsAffected())
	}
	if len(forgotten.documents) > 0 {
		if _, err := tx.Exec(ctx, `
			update document_block_versions owed set windows_owed_at = coalesce(owed.windows_owed_at, now())
			where owed.artifact_id::text = any($1::text[])
			  and exists (select 1 from artifact_versions left_standing
			              where left_standing.artifact_id = owed.artifact_id and left_standing.markdown is not null)`,
			forgotten.documents); err != nil {
			return fmt.Errorf("mark the documents whose windows went owed them: %w", err)
		}
	}
	return nil
}

// replaceLiveTexts writes each document's seed in place of its history (LiveText.Replace), in
// the order readDocuments read them, and counts what went: every stored update, checkpoint and
// snapshot as removed, and the seed as the one row the run rewrote in their place.
func (e *Erasure) replaceLiveTexts(ctx context.Context, tx pgx.Tx) error {
	for _, document := range e.Documents {
		replaced, err := document.live.Replace(ctx, tx)
		if err != nil {
			return fmt.Errorf("replace the live state of %s: %w", document.Document, err)
		}
		e.removed("doc_updates", replaced.Updates)
		e.removed("doc_checkpoints", replaced.Checkpoints)
		e.removed("doc_snapshots", replaced.Snapshots)
		e.redacted("doc_updates", 1)
	}
	return nil
}

// forgetTrees deletes the stored display tree of every text treeSums named. A reader of a text no
// row holds a tree for makes it from the markdown, as for a text written before the table existed
// (texttrees), so the cost of a row deleted here is one reading, and a text the run rewrote has
// the tree of what it now says made the first time it is read.
func (e *Erasure) forgetTrees(ctx context.Context, tx pgx.Tx, sums [][]byte) error {
	if len(sums) == 0 {
		return nil
	}
	tag, err := tx.Exec(ctx, `delete from text_trees where sum = any($1::bytea[])`, sums)
	if err != nil {
		return fmt.Errorf("delete the display trees of the texts holding the text of %s:%s: %w", e.Kind, e.ID, err)
	}
	e.removed("text_trees", tag.RowsAffected())
	return nil
}

// sweepTrees deletes once more, after the commit, the display trees forgetTrees deleted inside
// it. A read that began before the commit stores the trees it read on a goroutine of its own,
// detached from the request and bounded by api.StoreTreesTimeout (storeOf), so it can put the
// tree of a text the erase removed back after the commit. The sweep waits that bound out, which
// the caller passes in, then deletes those keys in a statement of its own, and counts what it
// found, so an operator reads whether the window was ever used.
//
// The keys it deletes are the ones the run held in memory (Erasure.sums): no digest of an erased
// text is written anywhere to remember them, so a sweep that fails cannot be run again, and its
// error says so.
func (e *Erasure) sweepTrees(ctx context.Context, conn *pgx.Conn, bound time.Duration) error {
	if len(e.sums) == 0 {
		return nil
	}
	wait := time.NewTimer(bound)
	defer wait.Stop()
	select {
	case <-wait.C:
	case <-ctx.Done():
		return fmt.Errorf("the erase committed, and its sweep of text_trees after the commit stopped "+
			"before it ran (a read that began before the commit may have stored a tree it removed; "+
			"no record keeps their keys, so the sweep cannot be run again): %w", ctx.Err())
	}
	tag, err := conn.Exec(ctx, `delete from text_trees where sum = any($1::bytea[])`, e.sums)
	if err != nil {
		return fmt.Errorf("the erase committed, and its sweep of text_trees after the commit failed "+
			"(no record keeps their keys, so the sweep cannot be run again): %w", err)
	}
	e.Swept = tag.RowsAffected()
	return nil
}

// apply removes everything the run read, in the one order the foreign keys allow. Before anything
// goes it deletes the rows of the embedding store that hold the text or stand on a block it
// erases or rewrites (forgetEmbeddings). Then go the whole text of every block of the node, the
// rows that cannot exist without a projection row, the projection rows, and the approvals and
// the versions they stood on. Then it rewrites what it may not remove - the blocks of the
// documents that quoted the node, the event log, the search vectors that stood on a version that
// went, the embedding rows of what it leaves (restateEmbeddings), and the live state of each
// document that held the text - counts the term statistics of every project whose items moved
// again, and deletes the display trees of the texts that went. Last go the tuples of both sides,
// the node, and the record of what the run did.
func (e *Erasure) apply(ctx context.Context, tx pgx.Tx, stores Stores, texts, artifacts []string, numbers []int32) error {
	sums, err := e.treeSums(ctx, tx, texts, artifacts, numbers)
	if err != nil {
		return err
	}
	e.sums = sums
	forgotten, err := e.forgetEmbeddings(ctx, tx, texts)
	if err != nil {
		return err
	}
	if err := e.eraseBlockTexts(ctx, tx); err != nil {
		return err
	}
	for _, held := range projectedTables {
		references, err := referencesTo(ctx, tx, held.Table)
		if err != nil {
			return err
		}
		for _, reference := range references {
			if !reference.Required {
				continue
			}
			tag, err := tx.Exec(ctx, `delete from `+reference.Table+` child where child.`+reference.Column+
				` in (`+projectionRows(reference, held.Table)+`)`, e.Kind, e.ID)
			if err != nil {
				return fmt.Errorf("delete the %s rows of %s:%s: %w", reference.Table, e.Kind, e.ID, err)
			}
			e.removed(reference.Table, tag.RowsAffected())
		}
		tag, err := tx.Exec(ctx, `delete from `+held.Table+` where node_kind = $1 and node_address = $2`, e.Kind, e.ID)
		if err != nil {
			return fmt.Errorf("delete the %s rows of %s:%s: %w", held.Table, e.Kind, e.ID, err)
		}
		e.removed(held.Table, tag.RowsAffected())
	}
	if len(artifacts) > 0 {
		tag, err := tx.Exec(ctx, `delete from artifact_reviews where exists (
			select 1 from unnest($1::uuid[], $2::int[]) as doomed(artifact, number)
			where doomed.artifact = artifact_reviews.artifact_id and doomed.number = artifact_reviews.version)`,
			artifacts, numbers)
		if err != nil {
			return fmt.Errorf("delete the reviews over the erased bytes: %w", err)
		}
		e.removed("artifact_reviews", tag.RowsAffected())
		tag, err = tx.Exec(ctx, `delete from artifact_versions where exists (
			select 1 from unnest($1::uuid[], $2::int[]) as doomed(artifact, number)
			where doomed.artifact = artifact_versions.artifact_id and doomed.number = artifact_versions.number)`,
			artifacts, numbers)
		if err != nil {
			return fmt.Errorf("delete the versions holding the erased text: %w", err)
		}
		e.removed("artifact_versions", tag.RowsAffected())
	}
	if err := e.redact(ctx, tx, texts); err != nil {
		return err
	}
	projects, err := e.restateSearch(ctx, tx, artifacts)
	if err != nil {
		return err
	}
	if err := e.restateEmbeddings(ctx, tx, forgotten); err != nil {
		return err
	}
	if err := e.replaceLiveTexts(ctx, tx); err != nil {
		return err
	}
	recount, err := stores.TermStatistics(ctx, tx, projects)
	if err != nil {
		return fmt.Errorf("count the term statistics again: %w", err)
	}
	e.removed("search_term_queue", recount.Queued)
	e.Recounted = recount.Projects
	if err := e.forgetTrees(ctx, tx, sums); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `delete from graph_tuples where from_kind = $1 and from_id = $2`, e.Kind, e.ID); err != nil {
		return fmt.Errorf("delete the tuples out of %s:%s: %w", e.Kind, e.ID, err)
	}
	if _, err := tx.Exec(ctx, `delete from graph_tuples where to_kind = $1 and to_id = $2`, e.Kind, e.ID); err != nil {
		return fmt.Errorf("delete the tuples into %s:%s: %w", e.Kind, e.ID, err)
	}
	if _, err := tx.Exec(ctx, `delete from graph_nodes where kind = $1 and id = $2`, e.Kind, e.ID); err != nil {
		return fmt.Errorf("delete the node %s:%s: %w", e.Kind, e.ID, err)
	}
	removed, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("render the erasure record: %w", err)
	}
	ranBy, err := json.Marshal(e.RanBy)
	if err != nil {
		return fmt.Errorf("render who ran the erase: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into graph_erasures (node_kind, node_id, removed, ran_by, ran_at)
		values ($1, $2, $3, $4, $5)`, e.Kind, e.ID, removed, ranBy, e.RanAt); err != nil {
		return fmt.Errorf("record the erase: %w", err)
	}
	return nil
}

// removed adds one table's deleted rows to the report, keeping one line per table.
func (e *Erasure) removed(table string, rows int64) {
	e.Rows = countedRows(e.Rows, table, rows)
}

// redacted adds one table's rewritten rows to the report, counted as removed counts a deleted
// one, so a run says what it rewrote as well as what it emptied.
func (e *Erasure) redacted(table string, rows int64) {
	e.RedactedRows = countedRows(e.RedactedRows, table, rows)
}

// countedRows adds one table's rows to a count of them, keeping one line per table.
func countedRows(counted []ErasedRows, table string, rows int64) []ErasedRows {
	if rows == 0 {
		return counted
	}
	for i := range counted {
		if counted[i].Table == table {
			counted[i].Rows += rows
			return counted
		}
	}
	return append(counted, ErasedRows{Table: table, Rows: rows})
}

// Write prints what the run read and what it did, the versions, the reviews and the documents
// whose live state it replaces named one per line, so an operator reads every one before
// confirming. A document's line says what replacing its history costs, which is what the run
// cannot give back: the edits before it, the anchors and margin records a seed does not carry,
// and the keystrokes of an editor open on it that were not yet stored.
func (e *Erasure) Write(w io.Writer) {
	verb := "would remove"
	if e.Applied {
		verb = "removed"
	}
	fmt.Fprintf(w, "erase %s:%s (supertag %s), %s by %s at %s\n",
		e.Kind, e.ID, e.Supertag, verb, e.RanBy.DatabaseUser, e.RanAt.Format(time.RFC3339))
	fmt.Fprintf(w, "  tuples: %d out of the node, %d into it\n", e.Tuples, e.Inbound)
	if len(e.Rows) == 0 {
		fmt.Fprintln(w, "  rows: none")
	}
	for _, rows := range e.Rows {
		fmt.Fprintf(w, "  rows: %s %d\n", rows.Table, rows.Rows)
	}
	for _, rows := range e.RedactedRows {
		fmt.Fprintf(w, "  redacted: %s %d\n", rows.Table, rows.Rows)
	}
	fmt.Fprintf(w, "  versions holding the erased text: %d\n", len(e.Versions))
	for _, version := range e.Versions {
		fmt.Fprintf(w, "    %s version %d%s\n", version.Document, version.Version, versionOwner(version))
	}
	fmt.Fprintf(w, "  reviews over those bytes: %d\n", len(e.Reviews))
	for _, review := range e.Reviews {
		fmt.Fprintf(w, "    %s version %d %s (review %s)%s\n",
			review.Document, review.Version, review.State, review.ID, reviewIssue(review))
	}
	fmt.Fprintf(w, "  live documents whose history is replaced: %d\n", len(e.Documents))
	for _, document := range e.Documents {
		if document.Left {
			fmt.Fprintf(w, "    %s%s: not redacted: its live text holds the erased bytes where its markdown spells "+
				"them otherwise than the erased text is written - an escaped * or _, for one - so no marker can stand "+
				"where they are; the erase refuses until an edit takes them out of the document\n",
				document.Document, owner(document.Issue, document.Project))
			continue
		}
		text := "its live text holds none of the erased text"
		if document.Held {
			text = "its live text is redacted, the marker standing where the erased text was"
		}
		fmt.Fprintf(w, "    %s%s: %s; its %d stored update(s) are replaced by one; %d anchor mark(s) and "+
			"%d margin record(s) go with them; an editor open on it reloads and loses what it has not stored\n",
			document.Document, owner(document.Issue, document.Project), text, document.Updates,
			document.AnchorMarks, document.MarginRecords)
	}
	if len(e.Recounted) > 0 {
		fmt.Fprintf(w, "  term statistics counted again: %s\n", strings.Join(e.Recounted, ", "))
	}
	if e.Applied {
		fmt.Fprintf(w, "  text_trees swept after the commit: %d\n", e.Swept)
	}
	for _, blocker := range e.Blockers {
		fmt.Fprintf(w, "  blocked: %s\n", blocker)
	}
}

// versionOwner names the issue or the project a version's document belongs to, for the line
// that lists it.
func versionOwner(version ErasedVersion) string {
	return owner(version.Issue, version.Project)
}

// owner is " of " the issue a document belongs to, or else its project, for a line that names
// the document.
func owner(issue, project string) string {
	if issue != "" {
		return " of " + issue
	}
	if project != "" {
		return " of " + project
	}
	return ""
}

func reviewIssue(review ErasedReview) string {
	if review.Issue == "" {
		return ""
	}
	return " of " + review.Issue
}
