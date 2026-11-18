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
	"fmt"
	"os"
	"os/user"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/schemanodes"
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
// projected onto: comments, document_blocks and block_refs, which migration 20261109022248 gave
// `node_kind` and `node_address`, and asks and ask_followers, which 20261116094058 gave them. A
// follower row names its ask's node: it is a relation of that node (followed_by), not a node of its
// own, so an ask's node is projected onto its asks row and onto each of its follower rows.
//
// Issue is the column naming the issue that owns a row, empty for a table whose rows are owned
// through their document alone, by the artifact_id the owning row carries: a comment and an ask
// are an issue's or a document's (comments_one_owner and asks_one_owner, migration 0009), and a
// block and a block reference are their document's. Through is, for a table whose rows carry no
// owner of their own, the join from a row (held) to the row that owns it (owner): a follower row
// stands under its ask's owner. Key is the table's primary key, the order the erase takes a node's
// rows of it in (lockProjectionRows) and how the listing names each.
//
// The erase walks every one by (node_kind, node_address) rather than by the node's kind, so a kind
// that gains a projection carries no second list here to keep in step. The order is the order
// every writer takes them in: an ask's row before its follower rows (20261116094058).
var projectedTables = []struct{ Table, Issue, Through, Key string }{
	{Table: "comments", Issue: "issue_key", Key: "id"},
	{Table: "document_blocks", Key: "artifact_id, block_id"},
	{Table: "block_refs", Key: "artifact_id, block_id, to_kind, to_id, kind"},
	{Table: "asks", Issue: "issue_key", Key: "id"},
	{Table: "ask_followers", Issue: "issue_key", Through: "asks owner on owner.id = held.ask_id", Key: "ask_id, session_id"},
}

// runTables are the rows a run of the command bar is projected onto
// (dispatch://INFRA-1950/spec#b-r-interaction), which carry no node_kind and node_address, so
// projectedTables never reaches them: going_requests projects the interaction node and its
// made_by, and each going_events row one frame value of it. Column names the run in each, and Key
// is the order the erase takes a run's rows of it in (lockProjectionRows) and how the listing
// names each. They are in the order a run's rows are locked by every writer that takes both - the
// run's row, then its frames, as a frame's own row's foreign key and the release before's delete
// of a run take them - and deleted in the other.
var runTables = []struct{ Table, Column, Key string }{
	{Table: "going_requests", Column: "id", Key: "id"},
	{Table: "going_events", Column: "request_id", Key: "request_id, sequence"},
}

// run is the run an erase of an interaction node removes the rows of, as going_requests keys it,
// and false for a node of any other kind or an id that is no uuid, which no row can name.
func (e *Erasure) run() (string, bool) {
	if e.Kind != graphstore.KindInteraction {
		return "", false
	}
	if _, err := uuid.Parse(e.ID); err != nil {
		return "", false
	}
	return e.ID, true
}

// ownRows is one table's rows that are the node's own and go with it: where is the predicate over
// the table that names them, over args, key the order the erase takes and lists them in, and
// projected whether they are a projection's rows (projectedTables), which the rows of other
// tables may reference, or a run's (runTables).
type ownRows struct {
	table, key, where string
	args              []any
	projected         bool
}

// ownRowSets is every table's rows of the node, in the order the erase takes them
// (lockProjectionRows) and lists them (readRows): each projection's rows by the node's address,
// in projectedTables' order, and for a run of the command bar each of runTables' rows by the run's
// uuid, in theirs.
func (e *Erasure) ownRowSets() []ownRows {
	sets := make([]ownRows, 0, len(projectedTables)+len(runTables))
	for _, held := range projectedTables {
		sets = append(sets, ownRows{table: held.Table, key: held.Key, where: `node_kind = $1 and node_address = $2`,
			args: []any{e.Kind, e.ID}, projected: true})
	}
	if run, ok := e.run(); ok {
		for _, held := range runTables {
			sets = append(sets, ownRows{table: held.Table, key: held.Key, where: held.Column + ` = $1::uuid`, args: []any{run}})
		}
	}
	return sets
}

// connect opens the erase's one connection, named in pg_stat_activity (eraseApplicationName). The
// erase uses no pool and migrates nothing: the schema it works on is the deployment's own.
func connect(ctx context.Context, databaseURL string) (*pgx.Conn, error) {
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse Postgres URL: %w", err)
	}
	config.RuntimeParams["application_name"] = eraseApplicationName
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
// document whose margin record holds its body, and for a run of the command bar its
// going_requests row and its going_events rows (runTables), so a run erased is a run no backfill
// writes back. The run deletes those, or rewrites them with the marker where it may take no row
// away. Every other row that holds the same bytes derives from another node and is that node's
// text - a sibling block of the same document among them: the run lists it with its address and
// changes nothing in it, and an operator erases that node by a run of its own.
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
func Erase(ctx context.Context, databaseURL, kind, id, confirm string, treeStoreBound time.Duration) (*Erasure, error) {
	if kind == schemanodes.KindSupertag || kind == schemanodes.KindAttribute {
		return nil, fmt.Errorf("the erase refuses %s:%s: a type node is data the block schema seeds, and no command deletes it", kind, id)
	}
	if kind == graphstore.KindRole || kind == graphstore.KindPage || kind == graphstore.KindSession {
		return nil, fmt.Errorf("the erase refuses %s:%s: a %s node holds no text anybody wrote, so no erasure is owed for it, "+
			"and the event log keeps addressing it, so deleting it leaves the census a difference no erase can close", kind, id, kind)
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
		erasure = &Erasure{Kind: kind, ID: id, RanAt: time.Now().UTC()}
		if read, err = erasure.read(ctx, tx); err != nil {
			return nil, err
		}
	} else {
		if tx, erasure, read, err = lockInStoreOrder(ctx, conn, kind, id); err != nil {
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
// without a projection row, the projection rows, a run's frames and its row, and the approvals and
// the versions they stood on, each such version's node stated gone (endErasedVersions). Then it
// rewrites what it may not remove - the blocks holding the node's block and the mentions whose
// excerpt was cut from it, the events about it, the search vectors that stood on a version that
// went, the embedding rows of what it leaves (restateEmbeddings), and the live state of every
// document whose history it replaces - counts the term statistics of every project whose items
// moved again, and deletes the display trees of the texts that went. Last go the tuples the
// listing named and the node (graphstore.EraseNode), which refuses the run where a tuple landed
// since, and the record of what the run did goes in. Every row it writes is one the read under
// its locks listed (erasedRead).
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
	if run, ok := e.run(); ok {
		for _, held := range slices.Backward(runTables) {
			tag, err := tx.Exec(ctx, `delete from `+held.Table+` where `+held.Column+` = $1::uuid`, run)
			if err != nil {
				return fmt.Errorf("delete the %s rows of %s:%s: %w", held.Table, e.Kind, e.ID, err)
			}
			e.removed(held.Table, tag.RowsAffected())
		}
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
		if err := e.endErasedVersions(ctx, tx, read); err != nil {
			return err
		}
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

// eraseActor is who the erase records as the writer of the state it moves on a version's node. The
// run is an operator's act at a database credential, which names no person or session of
// Dispatch, so it writes as the system, as the backfill does.
var eraseActor = model.Actor{Kind: "system", ID: "dispatch-erase"}

// endErasedVersions states gone, in the erase's transaction, the node of each version whose row the
// erase deleted, as the removal of a document states its versions' nodes gone (api's
// removedDocumentNodes): no row projects such a node any more, so a live one would be a node the
// census finds no row for. Each is written with the supertag and the attributes the store holds of
// it, its number and its time, at the run's time, with no event (the run appends none). A version
// the store holds no node of, or holds gone, writes nothing. A later version that takes a freed
// number restates the node live with its own row's created_at as a value (docs.WriteVersionNode).
func (e *Erasure) endErasedVersions(ctx context.Context, tx pgx.Tx, read erasedRead) error {
	if read.document == nil {
		return nil
	}
	var nodes []graphstore.Node
	for _, number := range read.numbers {
		id := graphstore.VersionID(read.document.refKey, int(number))
		if !graphstore.AddressFits(id) {
			continue
		}
		version, held, err := graphstore.ReadNode(ctx, tx, graphstore.KindVersion, id)
		if err != nil {
			return fmt.Errorf("read the node of the erased version %s: %w", id, err)
		}
		if !held || version.State == graphstore.StateGone {
			continue
		}
		version.State, version.MadeAt, version.MadeBy = graphstore.StateGone, e.RanAt, eraseActor
		version.SourceEvent, version.Restates = graphstore.NoEvent, true
		nodes = append(nodes, version)
	}
	if len(nodes) == 0 {
		return nil
	}
	if err := graphstore.Write(ctx, tx, nodes, nil); err != nil {
		return fmt.Errorf("state the nodes of the erased versions gone: %w", err)
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
