package docs

// The pending authors' rows (R in pending_authors.go's account): the statement that writes them,
// the queued form a write phase sends and the overlay a transaction's own reads consult before it
// is sent, and the reads and deletes a version makes under the document's advisory lock.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// pendingAuthor is one author's R row: the author, the version of the update whose write recorded
// it, and the document whose versions it counts toward, "" for the room the row's statement names
// (pendingAuthorsStatement): an issue room's update writes its authors under each document whose
// fragment it changed (pendingAuthorsIn).
type pendingAuthor struct {
	actor          model.Actor
	writtenThrough persistence.Version
	document       string
}

// pendingAuthorsOf is each of authors with the version of the update whose write recorded it,
// which writtenThrough names for each author's key. authors is keyed by actorKey, so no author
// appears twice.
func pendingAuthorsOf(authors map[string]model.Actor, writtenThrough func(key string) persistence.Version) []pendingAuthor {
	pending := make([]pendingAuthor, 0, len(authors))
	for _, actor := range actorSlice(authors) {
		pending = append(pending, pendingAuthor{actor: actor, writtenThrough: writtenThrough(actorKey(actor))})
	}
	return pending
}

// pendingAuthorsAt is each of authors as recorded by the update stored at version.
func pendingAuthorsAt(authors map[string]model.Actor, version persistence.Version) []pendingAuthor {
	return pendingAuthorsOf(authors, func(string) persistence.Version { return version })
}

// pendingAuthorsIn is pendingAuthorsAt for an update of an issue room that changed the fragments of
// documents (inflightCredit.documents): a row of each author under each of those documents, which
// a version of that document lists and no version of another (settlementCredit). An update of a
// room that is its own document names no documents, and its rows are the room's.
func pendingAuthorsIn(authors map[string]model.Actor, version persistence.Version, documents []string) []pendingAuthor {
	if len(documents) == 0 {
		return pendingAuthorsAt(authors, version)
	}
	actors := actorSlice(authors)
	pending := make([]pendingAuthor, 0, len(actors)*len(documents))
	for _, document := range documents {
		for _, actor := range actors {
			pending = append(pending, pendingAuthor{actor: actor, writtenThrough: version, document: document})
		}
	}
	return pending
}

// pendingAuthorsStatement is the one statement that writes authors, at least one, to R, each as
// recorded by its own update: an append transaction's authors at every credited update's own
// version, or a transaction's upsertPendingAuthors. An author keeps one row per update that
// recorded it, so an upload that holds one of those updates and not another deletes only the
// first. A row is room's, or, for an author an issue room's update recorded under a document whose
// fragment it changed (pendingAuthorsIn), that document's; only such a run passes the documents.
// authors must name each author at most once per update and document: one statement cannot write
// one row twice, so a repeated author and version is refused, naming it, before anything is
// written. A row written again keeps its Proven mark only when both writes carried it
// (mergeAuthor), so a claimed write is not lifted to proven by the row a proven one rewrites.
func pendingAuthorsStatement(room string, authors []pendingAuthor) (writeStatement, error) {
	type row struct {
		key, document string
		version       persistence.Version
	}
	seen := make(map[row]struct{}, len(authors))
	kinds := make([]string, 0, len(authors))
	ids := make([]string, 0, len(authors))
	encoded := make([]string, 0, len(authors))
	versions := make([]int64, 0, len(authors))
	var documents []*string
	for index, author := range authors {
		pair := row{key: actorKey(author.actor), document: author.document, version: author.writtenThrough}
		if _, repeated := seen[pair]; repeated {
			return writeStatement{}, fmt.Errorf("record the document's pending authors: %s/%s at update %d is named twice", author.actor.Kind, author.actor.ID, author.writtenThrough)
		}
		seen[pair] = struct{}{}
		raw, err := json.Marshal(author.actor)
		if err != nil {
			return writeStatement{}, fmt.Errorf("encode the document's pending authors: %w", err)
		}
		kinds = append(kinds, author.actor.Kind)
		ids = append(ids, author.actor.ID)
		encoded = append(encoded, string(raw))
		versions = append(versions, int64(author.writtenThrough))
		if author.document != "" {
			if documents == nil {
				documents = make([]*string, len(authors))
			}
			documents[index] = &authors[index].document
		}
	}
	return writeStatement{
		sql: `
		insert into doc_pending_authors (artifact_id, actor_kind, actor_id, actor, written_through)
		select coalesce(($6::uuid[])[author.position], $1::uuid), kind, id, actor, version
		from unnest($2::text[], $3::text[], $4::jsonb[], $5::bigint[]) with ordinality as author(kind, id, actor, version, position)
		on conflict (artifact_id, actor_kind, actor_id, written_through) do update set actor = case
			when doc_pending_authors.actor @> '{"proven": true}'::jsonb then excluded.actor
			else excluded.actor - 'proven'
		end
	`,
		args: []any{room, kinds, ids, encoded, versions, documents},
		what: "record the document's pending authors",
	}, nil
}

// queuePendingAuthors queues pendingAuthorsStatement for authors on plan, and records the rows it
// will add on the ledger that queues tx's writes (Ledger.queuedAuthors). It is the one place
// doc_pending_authors' insert is queued - an update's append queues it among its own writes
// (appendWrites), upsertPendingAuthors as a write of its own - and the one caller of the
// builder, so no writer can queue the statement without recording the overlay the transaction's
// own reads of R consult (transactionPendingAuthors). It queues nothing where the authors name
// nobody, which leaves no row for a read to miss either.
func queuePendingAuthors(ctx context.Context, tx pgx.Tx, plan *appendPlan, room string, authors []pendingAuthor) error {
	if len(authors) == 0 {
		return nil
	}
	statement, err := pendingAuthorsStatement(room, authors)
	if err != nil {
		return err
	}
	plan.add(statement)
	if ledger := queueFor(ctx, tx); ledger != nil {
		ledger.queuedAuthors(room, authors)
	}
	return nil
}

// upsertPendingAuthors writes authors to R in tx, which holds the document's advisory lock, in
// one statement (queuePendingAuthors, pendingAuthorsStatement). It goes through write, so a
// transaction whose writes are queued runs it in its write phase; deletePendingAuthors,
// deleteHeldPendingAuthors and markSettlementPending do the same.
func upsertPendingAuthors(ctx context.Context, tx pgx.Tx, room string, authors []pendingAuthor) error {
	var plan appendPlan
	if err := queuePendingAuthors(ctx, tx, &plan, room, authors); err != nil {
		return err
	}
	return plan.send(ctx, tx)
}

// transactionPendingAuthors is R for room as tx sees it: the rows the table holds, and the rows
// the writes tx has queued will add (Ledger.queuedAuthors). A version's transaction stores the
// room's updates before it reads R (Ledger.flushThrough), and each of those appends writes the
// authors of the edit it stores; where its writes are queued the table holds none of them yet, so
// a version reading the table alone would neither list those authors nor delete their rows when
// it commits, and the next version would be owed them again. An author both hold is one author,
// Proven only when the stored rows and the queued ones all are (mergeAuthor), as the table reads
// once the write phase has run: a queued row beside a stored one is another row of that author,
// and one over a stored row is the upsert's.
func transactionPendingAuthors(ctx context.Context, tx pgx.Tx, room string) (map[string]model.Actor, error) {
	owed, err := readPendingAuthors(ctx, tx, room)
	if err != nil {
		return nil, err
	}
	if ledger := queueFor(ctx, tx); ledger != nil {
		mergeAuthors(owed, ledger.owedAuthors[room])
	}
	return owed, nil
}

// transactionRoomPendingAuthors is readRoomPendingAuthors for documents, the documents of an issue
// room, as tx sees it: the rows the table holds of each, and the rows the writes tx has queued
// will add under each (Ledger.queuedAuthors), merged as transactionPendingAuthors merges them. An
// issue room's settlement stores the room's updates before it reads R, and each of those appends
// writes its authors under the documents whose fragments it changed (pendingAuthorsIn).
func transactionRoomPendingAuthors(ctx context.Context, tx pgx.Tx, documents []string) (map[string]map[string]model.Actor, error) {
	owed, err := readRoomPendingAuthors(ctx, tx, documents)
	if err != nil {
		return nil, err
	}
	if ledger := queueFor(ctx, tx); ledger != nil {
		for _, document := range documents {
			queued := ledger.owedAuthors[document]
			if len(queued) == 0 {
				continue
			}
			if owed[document] == nil {
				owed[document] = make(map[string]model.Actor, len(queued))
			}
			mergeAuthors(owed[document], queued)
		}
	}
	return owed, nil
}

// readPendingAuthors is R for room, keyed by actorKey. It says nothing about who edited last: the
// rows of one update are every peer that was connected when it landed, which is a set of names and
// not an order (latestEditSource reads the sender instead). A version reads R in its own
// transaction under the document's advisory lock, so no append or write changes it until that
// version ends. An author R holds rows of from several updates is one author, Proven only when
// every row is (mergeAuthor).
func readPendingAuthors(ctx context.Context, q Queryer, room string) (map[string]model.Actor, error) {
	rows, err := q.Query(ctx, `
		select actor from doc_pending_authors where artifact_id = $1
	`, room)
	if err != nil {
		return nil, fmt.Errorf("read the document's pending authors: %w", err)
	}
	defer rows.Close()
	owed := make(map[string]model.Actor)
	for rows.Next() {
		var encoded []byte
		if err := rows.Scan(&encoded); err != nil {
			return nil, fmt.Errorf("read the document's pending authors: %w", err)
		}
		var actor model.Actor
		if err := json.Unmarshal(encoded, &actor); err != nil {
			return nil, fmt.Errorf("decode a pending author of the document: %w", err)
		}
		mergeAuthor(owed, actor)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the document's pending authors: %w", err)
	}
	return owed, nil
}

// readRoomPendingAuthors is R for each of documents, the documents of an issue room, the tree
// artifact first (roomLayout.documents), by document and then by actorKey: the rows an update of
// the room wrote under each document whose fragment it changed (pendingAuthorsIn), and the rows
// keyed by the tree artifact. Read in one statement, in the version's transaction under the room's
// advisory lock, as readPendingAuthors is, and merged as it merges them: an author a document holds
// rows of from several updates is one author of it, Proven only when every row is (mergeAuthor).
func readRoomPendingAuthors(ctx context.Context, q Queryer, documents []string) (map[string]map[string]model.Actor, error) {
	rows, err := q.Query(ctx, `
		select artifact_id::text, actor from doc_pending_authors where artifact_id = any($1::uuid[])
	`, documents)
	if err != nil {
		return nil, fmt.Errorf("read the room's pending authors: %w", err)
	}
	defer rows.Close()
	owed := make(map[string]map[string]model.Actor, len(documents))
	for rows.Next() {
		var document string
		var encoded []byte
		if err := rows.Scan(&document, &encoded); err != nil {
			return nil, fmt.Errorf("read the room's pending authors: %w", err)
		}
		var actor model.Actor
		if err := json.Unmarshal(encoded, &actor); err != nil {
			return nil, fmt.Errorf("decode a pending author of the room: %w", err)
		}
		if owed[document] == nil {
			owed[document] = make(map[string]model.Actor)
		}
		mergeAuthor(owed[document], actor)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the room's pending authors: %w", err)
	}
	return owed, nil
}

// deleteRoomPendingAuthors deletes, in one statement in tx, the R rows of each document listed
// names of the authors listed names for it: the rows an issue room's settlement read of each
// document it versions, which that document's version lists (settlement.commitRoomVersions). It
// goes through write, as deletePendingAuthors does.
func deleteRoomPendingAuthors(ctx context.Context, tx pgx.Tx, listed map[string][]model.Actor) error {
	var documents, kinds, ids []string
	for document, authors := range listed {
		for _, actor := range authors {
			documents, kinds, ids = append(documents, document), append(kinds, actor.Kind), append(ids, actor.ID)
		}
	}
	if len(documents) == 0 {
		return nil
	}
	return writeExec(ctx, tx, "delete the room's listed pending authors", `
		delete from doc_pending_authors
		where (artifact_id, actor_kind, actor_id) in (select * from unnest($1::uuid[], $2::text[], $3::text[]))
	`, documents, kinds, ids)
}

// deletePendingAuthors deletes the R rows of authors in tx, the transaction of the version that
// lists them.
func deletePendingAuthors(ctx context.Context, tx pgx.Tx, room string, authors []model.Actor) error {
	if len(authors) == 0 {
		return nil
	}
	kinds := make([]string, 0, len(authors))
	ids := make([]string, 0, len(authors))
	for _, actor := range authors {
		kinds = append(kinds, actor.Kind)
		ids = append(ids, actor.ID)
	}
	return writeExec(ctx, tx, "delete the document's listed pending authors", `
		delete from doc_pending_authors
		where artifact_id = $1 and (actor_kind, actor_id) in (select * from unnest($2::text[], $3::text[]))
	`, room, kinds, ids)
}

// deleteHeldPendingAuthors deletes, in an upload's transaction, every R row of room whose writing
// update the upload's room instance held: written through the head it loaded, or by an update it
// took since.
func deleteHeldPendingAuthors(ctx context.Context, tx pgx.Tx, room string, held heldUpdates) error {
	return writeExec(ctx, tx, "delete the pending authors an upload's room held", `
		delete from doc_pending_authors
		where artifact_id = $1 and (written_through <= $2 or written_through = any($3::bigint[]))
	`, room, held.through, held.since)
}
