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

// pendingAuthor is one author's R row: the author, and the version of the update whose write
// recorded it.
type pendingAuthor struct {
	actor          model.Actor
	writtenThrough persistence.Version
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

// pendingAuthorsStatement is the one statement that writes authors, at least one, to R, each as
// recorded by its own update: an append transaction's authors at every credited update's own
// version, or a transaction's upsertPendingAuthors. An author keeps one row per update that
// recorded it, so an upload that holds one of those updates and not another deletes only the
// first. authors must name each author at most once per update: one statement cannot write one
// row twice, so a repeated author and version is refused, naming the author's kind and the update,
// before anything is written. The refusal names no author's id: a write's author is its request's
// actor, and the route that meets the refusal logs it.
// A row written again keeps its Proven mark only when both writes carried it (mergeAuthor), so
// a claimed write is not lifted to proven by the row a proven one rewrites.
func pendingAuthorsStatement(room string, authors []pendingAuthor) (writeStatement, error) {
	type row struct {
		key     string
		version persistence.Version
	}
	seen := make(map[row]struct{}, len(authors))
	kinds := make([]string, 0, len(authors))
	ids := make([]string, 0, len(authors))
	encoded := make([]string, 0, len(authors))
	versions := make([]int64, 0, len(authors))
	for _, author := range authors {
		pair := row{key: actorKey(author.actor), version: author.writtenThrough}
		if _, repeated := seen[pair]; repeated {
			return writeStatement{}, fmt.Errorf("record the document's pending authors: a %s author at update %d is named twice", author.actor.Kind, author.writtenThrough)
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
	}
	return writeStatement{
		sql: `
		insert into doc_pending_authors (artifact_id, actor_kind, actor_id, actor, written_through)
		select $1::uuid, kind, id, actor, version
		from unnest($2::text[], $3::text[], $4::jsonb[], $5::bigint[]) as author(kind, id, actor, version)
		on conflict (artifact_id, actor_kind, actor_id, written_through) do update set actor = case
			when doc_pending_authors.actor @> '{"proven": true}'::jsonb then excluded.actor
			else excluded.actor - 'proven'
		end
	`,
		args: []any{room, kinds, ids, encoded, versions},
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
