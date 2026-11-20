// Package grapherase is `envoy-dispatch erase --node <kind>:<id>`: the one path in the tree that
// deletes a node of the store, its tuples and its projection rows, and that takes the erased bytes
// out of every row that derives from that node. Only cmd/dispatch imports it.
//
// It is a package of its own, and not a pass of graphcensus, because it writes through docs and
// api inside its own transaction - a document's live state (docs.ReadErasedLiveText) and the term
// statistics (api.RecountSearchTermStatsTx) - and docs' own tests import graphcensus, which can
// therefore import neither.
package grapherase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/schemanodes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
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

// projectedTables are the tables that carry a node's address, and so are the rows a node is
// projected onto and whose rows the erase deletes with it: comments, document_blocks and
// block_refs, which migration 20261109022248 gave `node_kind` and `node_address`, and messages,
// which 20261116114515 gave them.
//
// Issue is the column naming the issue that owns a row itself, empty for a table whose rows are
// owned through their document alone, and Artifact the column naming the document that owns it,
// empty for a table whose rows no document owns: a comment is an issue's or a document's
// (comments_one_owner, migration 0009), a block and a block reference are their document's, and a
// message is its issue's or, sent on no issue, nobody's, as its write takes no owner then. Key is
// the table's primary key, the order the erase takes a node's rows of it in (lockProjectionRows)
// and how the listing names each.
//
// The erase walks every one by (node_kind, node_address) rather than by the node's kind, so a
// kind that gains a projection the erase deletes carries no second list here to keep in step.
// asks and ask_followers, which 20261116094058 gave the same pair, project only an ask's node,
// and the erase deletes, locks and lists neither: it refuses an ask's node while its asks row
// stands (graphcensus.ObjectRowStands), and a follower row goes with its asks row. An asks row
// still says what its ask's node states, which readBlockers reads.
var projectedTables = []struct{ Table, Issue, Artifact, Key string }{
	{Table: "comments", Issue: "issue_key", Artifact: "artifact_id", Key: "id"},
	{Table: "document_blocks", Artifact: "artifact_id", Key: "artifact_id, block_id"},
	{Table: "block_refs", Artifact: "artifact_id", Key: "artifact_id, block_id, to_kind, to_id, kind"},
	{Table: "messages", Issue: "issue_key", Key: "id"},
}

// itemPartTables are the tables whose rows name a node that is a part of an item rather than the
// item itself (migration 20261116140000): the block rows of a comment, a message and an ask and
// the link rows of their blocks, each naming the block's node. The erase deletes none of them by
// a node's address - a block's and a link's row goes with its item's row, on delete cascade - so
// they are no projectedTables, but each is a projection a live node states its tuples from, which
// readBlockers reads.
var itemPartTables = []string{
	"comment_blocks", "message_blocks", "ask_blocks",
	"comment_block_refs", "message_block_refs", "ask_block_refs",
}

// connect opens the erase's one connection, named in pg_stat_activity (eraseApplicationName). The
// erase uses no pool and migrates nothing: the schema it works on is the deployment's own. The
// connection carries this build's marker writer setting (store.StoreMarkerWriterSetting), as a
// process that updates this build's marked rows on a connection of its own does: the user_threads
// rows a removed reply reached are rewritten with the store's change (rewriteRepliedRows), so they
// keep their mark.
func connect(ctx context.Context, databaseURL string) (*pgx.Conn, error) {
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse Postgres URL: %w", err)
	}
	config.RuntimeParams["application_name"] = eraseApplicationName
	config.RuntimeParams[store.StoreMarkerWriterSetting] = "on"
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return conn, nil
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

// Erase removes one node and the bytes that derive from it, in one transaction. It is the one
// path in this tree that deletes a tuple row or a node row (graphstore.EraseNode); every other
// writer appends.
//
// What derives from a node is fixed by the node (erasedRead): its tuples on both sides, its
// projection rows and the rows that cannot exist without one, its own embedding rows and window
// state, the events about it, for a block its own document - the versions at which its own block
// held the text and the approvals over them, the document's editing state, its search rows and
// its windows - and the excerpts cut from its text, for a comment the editing state of every
// document whose margin record holds its body, and its rows of the six read-state tables, which
// mirror tuples from a person to what the mark is about: a person's every row, and an item's the
// rows that name it (readStateTables). The run deletes those, or rewrites them with the
// marker where it may take no row away. A comment or a message that is a reply in another thread
// is counted and named by that thread's user_threads rows it reached, which are their people's
// and stay: the run rewrites each one's version, last_reply_id and replied_at to what the store
// says once the reply's deliveries are gone (readRepliedRows). Every other row that holds the
// same bytes derives from another node and is that node's text - a sibling block of the same
// document among them: the run lists it with its address and changes nothing in it. An operator
// erases a comment's, a message's or a block's text by a run over that node; an issue's, a
// document's or an ask's text is in its own row and the events about it, which no run reaches,
// and is changed through that object's own route (an issue's title, a document's name and
// versions).
//
// confirm is the digest of a listing. With none the run reads everything it would delete,
// rewrite, redact or leave in a read-only transaction, which takes no write lock and waits on no
// writer, and answers it with its digest, having written nothing. With one it takes its locks in
// the store's lock order (lockInStoreOrder), reads the listing again under them, and erases only
// when that listing has the digest it was given (Erasure.Mismatch); otherwise it answers both
// digests and removes nothing. It refuses, removing nothing, while another node's row states
// something about this one (Blockers), or while its document's live text holds the erased bytes
// where its markdown spells them otherwise (Unredacted), since the seed would keep them. A tuple
// another writer commits into the node after that listing was read ends the run as an error with
// nothing removed (graphstore.EraseNode), since the run deletes the tuples it listed and no
// others.
//
// It refuses a type node, a supertag or an attribute, before it opens a connection: a type is
// data the block schema seeds (migration 20261109051232, pmdoc/schemanodes), and no route and no
// command creates, retitles or deletes one (R6). Such a node holds no text anybody wrote, so no
// erasure is owed for it, and deleting one would take a type out of the schema every server
// installs.
//
// It refuses a role, a page and a session the same way, before it opens a connection. A node of
// one of those kinds holds no text anybody wrote, so no erasure is owed for it, and the event log
// keeps addressing it, so deleting it leaves the census a difference no erase can close
// (graphcensus's addressedRoles reads the roles every event ever named).
//
// It refuses an issue, a document and an ask while their row stands (graphcensus.ObjectRowStands,
// read in Erasure.read, under the node's lock on a confirmed run). A node of one of those kinds
// holds its text in its own row - an issue's title, a document's name and versions, an ask's
// question - and the erase reads text only from the comments, the messages and the document
// blocks that derive from a node. A run over one would delete the node and its tuples, record an
// erasure and answer applied with every byte of that text still in place, the project census
// would read the row as missing its node, and the backfill would write the node again
// (graphcensus.ProjectComplete). A message's row that names its node is one of its projection
// rows (projectedTables): the run deletes it with the node and marks its body in every event that
// carried it (readTexts, readEvents), so a message is erased with its row. A message row a build
// before this one wrote names no node until the backfill marks it, and the run refuses that
// message until it does, since it would find the row by no address. A node of one of those
// kinds, at any state but gone, that no row stands behind - a document a build before this one
// removed, during a roll or after a rollback, without stating its node gone - holds no text and
// is the project census's orphaned class; the erase removes it with its tuples, which is that
// class's one remedy.
//
// What it did goes into `graph_erasures`: what was removed, who ran it and when, and no erased
// text. Nobody addresses that row and no read of the graph answers it.
//
// Once it has committed it waits treeStoreBound and deletes the display trees it deleted once
// more (sweepTrees). treeStoreBound is how long after the commit a read that began before it can
// still store the display tree of a text the erase removed: a read stores the trees it read on a
// goroutine of its own, detached from the request and bounded by api.StoreTreesTimeout, which is
// what cmd/dispatch passes; a test passes a shorter one. An error there comes back beside the
// Erasure, which is then Applied: the erase stands, and what the sweep could not do is the
// error's to say.
//
// serverURL is the dashboard origin the server's writers cut a block's excerpt around
// (refs.BlockExcerpt): a redacted excerpt the run stores in a block's place is cut with it, so it
// is the excerpt the writer would store. cmd/dispatch reads it as the server does. An empty one is
// refused, as rebuild-refs refuses one: the run would store an excerpt no writer of the deployment
// cut.
func Erase(ctx context.Context, databaseURL, serverURL, kind, id, confirm string, treeStoreBound time.Duration) (*Erasure, error) {
	if kind == schemanodes.KindSupertag || kind == schemanodes.KindAttribute {
		return nil, fmt.Errorf("the erase refuses %s:%s: a type node is data the block schema seeds, and no command deletes it", kind, id)
	}
	if kind == graphstore.KindRole || kind == graphstore.KindPage || kind == graphstore.KindSession {
		return nil, fmt.Errorf("the erase refuses %s:%s: a %s node holds no text anybody wrote, so no erasure is owed for it, "+
			"and the event log keeps addressing it, so deleting it leaves the census a difference no erase can close", kind, id, kind)
	}
	if strings.TrimSpace(serverURL) == "" {
		return nil, errors.New("dispatch.server_url is required to cut a redacted excerpt as the server's writers cut it; refusing to store an excerpt no writer stored")
	}
	confirm = strings.TrimPrefix(strings.TrimSpace(confirm), "sha256:")
	conn, err := connect(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	defer conn.Close(context.Background())
	// The documents the erase reads, redacts and renders are held to the block schema the
	// store's type nodes build, as the server holds them, not to the file this binary embeds.
	if err := schemanodes.InstallFromStore(ctx, conn); err != nil {
		return nil, fmt.Errorf("install the block schema from the store: %w", err)
	}
	var tx pgx.Tx
	var erasure *Erasure
	var read erasedRead
	if confirm == "" {
		if tx, err = conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}); err != nil {
			return nil, fmt.Errorf("begin: %w", err)
		}
		defer tx.Rollback(context.Background())
		erasure = &Erasure{Kind: kind, ID: id, RanAt: time.Now().UTC(), serverURL: serverURL}
		if read, err = erasure.read(ctx, tx); err != nil {
			return nil, err
		}
	} else {
		if tx, erasure, read, err = lockInStoreOrder(ctx, conn, serverURL, kind, id); err != nil {
			return nil, err
		}
		defer tx.Rollback(context.Background())
		erasure.Confirmed = confirm
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
	if confirm == "" || erasure.Mismatch() || len(erasure.Blockers) > 0 {
		return erasure, nil
	}
	if err := erasure.readLiveTexts(ctx, tx, read); err != nil {
		return nil, err
	}
	if len(erasure.Unredacted()) > 0 {
		return erasure, nil
	}
	if err := erasure.apply(ctx, tx, read); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	erasure.Applied = true
	if err := erasure.sweepTrees(ctx, conn, treeStoreBound); err != nil {
		return erasure, err
	}
	return erasure, nil
}

// apply removes and rewrites every row the listing named as deriving from the node, in the one
// order the foreign keys allow. Before anything goes it reads the keys of the display trees of
// the texts it removes or rewrites (treeSums) and deletes the node's embedding rows
// (forgetEmbeddings). Then go the whole text of the node's block, the rows that cannot exist
// without a projection row, the projection rows, the node's read-state rows (deleteReadState) -
// with the user_threads rows a removed reply reached rewritten beside them (rewriteRepliedRows) -
// and the approvals and the versions they stood on. Then it rewrites what it may not remove - the
// blocks holding the node's block and the mentions whose excerpt was cut from it, the events about
// it, the search vectors that stood on a version that went, the embedding rows of what it leaves
// (restateEmbeddings), and the live state of every document whose history it replaces - counts the
// term statistics of every project whose items moved again, and deletes the display trees of the
// texts that went. Last go the tuples the listing named and the node (graphstore.EraseNode), which
// refuses the run where a tuple landed since, and the record of what the run did goes in. Every
// row it writes is one the read under its locks listed (erasedRead).
func (e *Erasure) apply(ctx context.Context, tx pgx.Tx, read erasedRead) error {
	sums, err := e.treeSums(ctx, tx, read)
	if err != nil {
		return err
	}
	e.sums = sums
	forgotten, err := e.forgetEmbeddings(ctx, tx, read)
	if err != nil {
		return err
	}
	if err := e.eraseBlockTexts(ctx, tx, read); err != nil {
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
	if err := e.deleteReadState(ctx, tx, read); err != nil {
		return err
	}
	if err := e.rewriteRepliedRows(ctx, tx, read); err != nil {
		return err
	}
	if len(read.artifacts) > 0 {
		tag, err := tx.Exec(ctx, `delete from artifact_reviews where exists (
			select 1 from unnest($1::uuid[], $2::int[]) as doomed(artifact, number)
			where doomed.artifact = artifact_reviews.artifact_id and doomed.number = artifact_reviews.version)`,
			read.artifacts, read.numbers)
		if err != nil {
			return fmt.Errorf("delete the reviews over the erased bytes: %w", err)
		}
		e.removed("artifact_reviews", tag.RowsAffected())
		tag, err = tx.Exec(ctx, `delete from artifact_versions where exists (
			select 1 from unnest($1::uuid[], $2::int[]) as doomed(artifact, number)
			where doomed.artifact = artifact_versions.artifact_id and doomed.number = artifact_versions.number)`,
			read.artifacts, read.numbers)
		if err != nil {
			return fmt.Errorf("delete the versions holding the erased text: %w", err)
		}
		e.removed("artifact_versions", tag.RowsAffected())
	}
	if err := e.redactBlocks(ctx, tx, read); err != nil {
		return err
	}
	if err := e.clearExcerpts(ctx, tx, read); err != nil {
		return err
	}
	if err := e.redactEvents(ctx, tx, read.events); err != nil {
		return err
	}
	projects, err := e.restateSearch(ctx, tx, read.artifacts)
	if err != nil {
		return err
	}
	if err := e.restateEmbeddings(ctx, tx, forgotten); err != nil {
		return err
	}
	if err := e.replaceLiveTexts(ctx, tx); err != nil {
		return err
	}
	recounted, queued, err := api.RecountSearchTermStatsTx(ctx, tx, projects)
	if err != nil {
		return fmt.Errorf("count the term statistics again: %w", err)
	}
	e.removed("search_term_queue", queued)
	e.Recounted = recounted
	if err := e.forgetTrees(ctx, tx, sums); err != nil {
		return err
	}
	if err := graphstore.EraseNode(ctx, tx, e.Kind, e.ID, read.tupleSeqs); err != nil {
		return err
	}
	if err := e.eraseItemParts(ctx, tx, read); err != nil {
		return err
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

// eraseBlockTexts deletes the whole text of the node's block at every version, which is the
// most of the bytes an erased block holds. document_block_texts is keyed by
// (artifact_id, block_id, version) and carries a foreign key to artifacts alone (migration
// 0121), so the walk of single-column references into a projected table never names it and
// the text would outlive the node that held it. It is deleted by the document and the block id
// the node's address names (ownDocument), so the texts of a block a version dropped, which has
// no document_blocks row left, go as well. Any other node names no block and deletes nothing.
func (e *Erasure) eraseBlockTexts(ctx context.Context, tx pgx.Tx, read erasedRead) error {
	if read.document == nil {
		return nil
	}
	tag, err := tx.Exec(ctx, `delete from document_block_texts where artifact_id = $1::uuid and block_id = $2`,
		read.document.artifact, read.blockID)
	if err != nil {
		return fmt.Errorf("delete the texts of the block %s:%s: %w", e.Kind, e.ID, err)
	}
	e.removed("document_block_texts", tag.RowsAffected())
	return nil
}

// marker is eraseMarker naming the node this run addressed, which is what stands in every row
// the run rewrites.
func (e *Erasure) marker() string { return fmt.Sprintf(eraseMarker, e.Kind, e.ID) }

// eraseItemParts deletes the nodes that are parts of an erased comment or message - its block
// nodes and its delivery attempts - and every tuple on both sides of each (readItemParts), once the
// item's own node and tuples are gone, so no part_of is left into it: a block node of the item is
// its text, and what the blocks' links state is read off that text, and an attempt is one of its
// sends. The rows that name those nodes went with the item's row: the block and link rows on
// delete cascade (migrations 0142 and 20261101143211), and the comment_deliveries and
// message_deliveries rows as the rows that cannot exist without it (apply), so nothing names a node
// this deletes.
func (e *Erasure) eraseItemParts(ctx context.Context, tx pgx.Tx, read erasedRead) error {
	for _, part := range read.parts {
		if err := graphstore.EraseNode(ctx, tx, part.kind, part.id, part.seqs); err != nil {
			return err
		}
		e.removed("graph_nodes", 1)
	}
	return nil
}
