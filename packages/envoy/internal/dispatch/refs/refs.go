// Package refs indexes Dispatch links and reads the reference graph. Mentions are derived from
// source text into refs; structural relations stay in the columns that own them and the
// graph_edges view unions both into one typed edge relation. Artifact targets use ref_key
// (`<issue-or-project>/<slug>`, or `agent/<session id>/<slug>` for an agent's conversation) so
// every owner's documents and files share one address; artifact sources are the artifact uuid.
// Closure reads an issue's references one hop in each direction: every node the issue's own
// content links to, and every node that links to the issue or to its own content.
// References whose target is absent stay stored but are absent from closures and referenced-by
// results.
package refs

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// Queryer is the query surface shared by a pool and a transaction.
type Queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// Target is one end of a reference edge: the kind and durable id `refs` stores.
type Target struct {
	Kind string
	ID   string
}

// CompareAddresses compares the store addresses of two nodes, `<kind>:<id>`, byte for byte, as
// strings.Compare compares the two written out, and writes neither: the walk and the tree read
// order siblings by it thousands of times a read.
func CompareAddresses(left, right Target) int {
	if left.Kind == right.Kind {
		return strings.Compare(left.ID, right.ID)
	}
	leftLength, rightLength := len(left.Kind)+1+len(left.ID), len(right.Kind)+1+len(right.ID)
	for index := range min(leftLength, rightLength) {
		if l, r := addressByte(left, index), addressByte(right, index); l != r {
			return cmp.Compare(l, r)
		}
	}
	return cmp.Compare(leftLength, rightLength)
}

// addressByte is the byte at index of target's address, `<kind>:<id>`.
func addressByte(target Target, index int) byte {
	switch {
	case index < len(target.Kind):
		return target.Kind[index]
	case index == len(target.Kind):
		return ':'
	}
	return target.ID[index-len(target.Kind)-1]
}

// refsSources is every kind of node a refs row's from_kind names: a document, an ask, a comment
// and a message, the four sources whose text a write indexes into refs (Replacement) and whose
// re-index it owes (queueSettleSourceOwed). None is a standing kind, so a predicate over a
// refs row's source binds no staleness test (VisibleEnd, NodeKinds).
var refsSources = KindsOf("artifact", string(ItemAsk), string(ItemComment), string(ItemMessage))

// EditedTargets is what an edit of an ask's question or a comment's body from previous to body
// moves beside the edges it inserts or deletes, found being body's references. Every edge such a
// source writes shows the head of its text as its excerpt (Edges, itemExcerpt), cut against the
// deployment at serverURL, so when the edit moves that head, the panel of every target the source
// still cites changes although its edge stays: those targets are named too. An edit that keeps
// the head moves none.
func EditedTargets(previous, body, serverURL string, found []text.Ref) []Target {
	if itemExcerpt(previous, serverURL) == itemExcerpt(body, serverURL) {
		return nil
	}
	kinds, ids := refTargets(found)
	targets := make([]Target, len(kinds))
	for index := range kinds {
		targets[index] = Target{Kind: kinds[index], ID: ids[index]}
	}
	return targets
}

// replaceRefs reconciles the Dispatch references written by one source with found: targets no
// longer mentioned are deleted, new ones inserted with insertedAt as created_at (nil: now()), and
// an edge that persists keeps its created_at and source_seq. It returns the targets whose inbound
// edges moved — deleted or inserted, oldest state first — so its caller can resolve the ones
// a reader refreshes by name (CountedChanges). It is PlanReplace and Queue back to back, the
// queued statements run at once as a batch of their own. Only targets of a node kind this
// build knows (nodeKinds) are deleted: during a release a newer build writes the same sources, and
// an edge to a node kind it added is one this build cannot read out of the body, so it keeps that
// edge.
//
// What it queues settles what the source owes the re-index queues (Replacement.Queue): the whole
// grammar's always, since this text has now been read with the whole grammar, and the forms
// queue only when whole says the write indexed the source's blocks in tx before this call, so the
// text it read is the source's whole text and every delete that index made, its blocks' included,
// came before this settle (Replacement.SettlingForms, RefsFormsOwed). A write that leaves the
// blocks for later passes false and leaves the source owed until this build's own block index of
// it, as rebuild-refs leaves each source it re-indexes (replaceInTx, which plans and queues its
// replacement itself, without SettlingForms). So a write this build makes costs the drains
// nothing, and only an older build's write stays owed.
func replaceRefs(ctx context.Context, tx pgx.Tx, fromKind, fromID string, found []text.Ref, insertedAt *time.Time, whole bool) ([]Target, error) {
	replacement, err := PlanReplace(ctx, tx, fromKind, fromID, found, insertedAt)
	if err != nil {
		return nil, err
	}
	if whole {
		replacement = replacement.SettlingForms()
	}
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		replacement.Queue(b)
		return nil
	}); err != nil {
		return nil, err
	}
	return replacement.Changed(), nil
}

// Replacement is one source's references reconciled with found, the references its text now
// cites, as replaceRefs reconciles them. It is planned against the edges the source holds
// (PlanReplace, PlanReplaceFrom), so the targets it moves are known before anything is written,
// and written by Queue, which puts its statements on a write phase's batch: a writer that decides
// everything first and writes last plans in its read phase and queues in its write phase.
type Replacement struct {
	fromKind, fromID string
	kinds, ids       []string
	insertedAt       *time.Time
	changed          []Target
	held             []Target
	// forms says the replacement's settle deletes the source's rows of the forms queues as well as
	// the whole grammar's (SettlingForms).
	forms bool
}

// PlanReplace plans the reconciliation of fromKind/fromID's references with found against the
// edges q holds for the source (HeldTargets).
func PlanReplace(ctx context.Context, q Queryer, fromKind, fromID string, found []text.Ref, insertedAt *time.Time) (Replacement, error) {
	held, err := HeldTargets(ctx, q, fromKind, fromID)
	if err != nil {
		return Replacement{}, err
	}
	return PlanReplaceFrom(held, fromKind, fromID, found, insertedAt), nil
}

// HeldTargets is the targets of the edges q holds for one source, read with the predicate the
// reconciling delete visits them by: an edge to a node kind this build does not know (nodeKinds)
// is one that delete keeps, so it is neither held nor moved here (replaceRefs).
func HeldTargets(ctx context.Context, q Queryer, fromKind, fromID string) ([]Target, error) {
	held, err := scanTargets(q.Query(ctx, `
		select to_kind, to_id from refs where from_kind = $1 and from_id = $2 and to_kind = any($3)
	`, fromKind, fromID, nodeKinds))
	if err != nil {
		return nil, fmt.Errorf("read references: %w", err)
	}
	return held, nil
}

// PlanReplaceFrom plans the reconciliation against held, the targets the source holds: those a
// caller read (HeldTargets), or the Held of its own earlier replacement of the source that is not
// written yet. The targets it moves are the held ones found no longer names, in held's order, then
// those found names that held lacks, in found's order: what the delete and the insert return.
func PlanReplaceFrom(held []Target, fromKind, fromID string, found []text.Ref, insertedAt *time.Time) Replacement {
	kinds, ids := refTargets(found)
	cited := make(map[Target]struct{}, len(kinds))
	after := make([]Target, len(kinds))
	for index := range kinds {
		after[index] = Target{Kind: kinds[index], ID: ids[index]}
		cited[after[index]] = struct{}{}
	}
	stored := make(map[Target]struct{}, len(held))
	changed := []Target{}
	for _, target := range held {
		stored[target] = struct{}{}
		if _, kept := cited[target]; !kept {
			changed = append(changed, target)
		}
	}
	for _, target := range after {
		if _, kept := stored[target]; !kept {
			changed = append(changed, target)
		}
	}
	return Replacement{fromKind: fromKind, fromID: fromID, kinds: kinds, ids: ids, insertedAt: insertedAt, changed: changed, held: after}
}

// Changed is the targets whose inbound edges the replacement moves (PlanReplaceFrom).
func (r Replacement) Changed() []Target {
	return r.changed
}

// Held is the targets the source holds once the replacement is written.
func (r Replacement) Held() []Target {
	return r.held
}

// ArtifactMentions is what r, one reconcile of a document's references, states of them in the node
// and tuple store, for its caller to append under the cause the write's event gives it
// (graphstore.Caused, graphstore.QueueCaused), in the transaction that queues r: the mentions tuple
// out of the document's node (graphstore.ArtifactMentions) to each target whose row r inserts, and
// its retraction to each target whose row r deletes, both dated at the time r writes the
// document's references at (its insertedAt, else the transaction's now(), which the insert dates a
// new row at too). A row that survives states nothing: its tuple stands from the write that
// inserted it. It reads, in the plan's read phase, only the document row's node columns and the
// time, so it may be called before r is queued or after.
//
// An assertion's legacy_source_seq is its row's source_seq. The insert writes none, and the event
// that records the version stamps the row with its own id in the same transaction (QueueStamp,
// Stamp, docs' stampVersionEvent), which is the cause the caller appends the tuple under, so the
// tuple takes that cause (graphstore.UncausedTuple's LegacyFromCause). A reconcile no event
// records - rebuild-refs - appends at the sentinel (graphstore.Caused with NoEvent), where the
// tuple keeps none, as its row does. A retraction's row is gone and it takes none.
//
// record is what the version's own row and its document's say of the document (ReadIndexedVersion,
// or the writer's own record of a version or a document whose row is only queued): its ref_key,
// which every tuple runs out of, and the first author of its newest version, who signs each fact,
// or the document's own author where that version names none, as the backfill signs the facts of a
// document's bytes (graphcensus' nameArtifactNodes). A document whose row names no node, which a
// process of the release before wrote, states nothing, nor does a far end past
// graphstore.MaxAddressBytes: each such row is the view's arm over refs to answer
// (store.ArtifactReferenceRowUnmarked). A document row the transaction has queued and not written
// is this build's, which names its node. A source of any other kind - a comment, a message, an ask
// - states nothing here.
func (r Replacement) ArtifactMentions(ctx context.Context, q Queryer, record IndexedVersion) ([]graphstore.UncausedTuple, error) {
	id, ok := canonicalUUID(r.fromID)
	if r.fromKind != graphstore.KindArtifact || !ok || len(r.changed) == 0 || !graphstore.AddressFits(record.RefKey) {
		return nil, nil
	}
	var marked bool
	var at time.Time
	var author []byte
	if err := q.QueryRow(ctx, `
		select coalesce((select a.node_kind is not null from artifacts a where a.id = $1::uuid), true),
		       coalesce($2::timestamptz, now()),
		       (select a.created_by from artifacts a where a.id = $1::uuid)
	`, id, r.insertedAt).Scan(&marked, &at, &author); err != nil {
		return nil, fmt.Errorf("read the node of document %s: %w", r.fromID, err)
	}
	if !marked {
		return nil, nil
	}
	who := record.Writer
	if who.ID == "" && author != nil {
		if err := json.Unmarshal(author, &who); err != nil {
			return nil, fmt.Errorf("decode the author of document %s: %w", r.fromID, err)
		}
	}
	cited := make(map[Target]struct{}, len(r.held))
	for _, target := range r.held {
		cited[target] = struct{}{}
	}
	// changed lists the targets r deletes first, then those it inserts (PlanReplaceFrom), so the
	// retractions come before the assertions, as the two statements run.
	mentions := make([]graphstore.UncausedTuple, 0, len(r.changed))
	for _, target := range r.changed {
		if !graphstore.AddressFits(target.ID) {
			continue
		}
		_, inserted := cited[target]
		mentions = append(mentions, graphstore.UncausedTuple{
			Tuple:           graphstore.ArtifactMentions(record.RefKey, target.Kind, target.ID, inserted, who, at),
			LegacyFromCause: inserted,
		})
	}
	return mentions, nil
}

// DocumentMentioning is the record a reconcile of document artifactID's references that writes no
// version of it signs its facts with (Replacement.ArtifactMentions): the document's ref_key and the
// first author of its newest version, read through q. found is false for a document no row holds.
func DocumentMentioning(ctx context.Context, q Queryer, artifactID string) (record IndexedVersion, found bool, err error) {
	id, ok := canonicalUUID(artifactID)
	if !ok {
		return IndexedVersion{}, false, nil
	}
	var writer []byte
	switch err := q.QueryRow(ctx, `
		select a.ref_key, (select v.authors->0 from artifact_versions v where v.artifact_id = a.id order by v.number desc limit 1)
		from artifacts a where a.id = $1::uuid
	`, id).Scan(&record.RefKey, &writer); {
	case errors.Is(err, pgx.ErrNoRows):
		return IndexedVersion{}, false, nil
	case err != nil:
		return IndexedVersion{}, false, fmt.Errorf("read the writer of document %s: %w", artifactID, err)
	}
	if writer != nil {
		if err := json.Unmarshal(writer, &record.Writer); err != nil {
			return IndexedVersion{}, false, fmt.Errorf("decode the writer of document %s: %w", artifactID, err)
		}
	}
	return record, true, nil
}

// SettlingForms is the replacement settling the source's rows of the forms queues too
// (RefsFormsOwed, RefsDecisionsFormsOwed), in the statements that settle the whole grammar's
// (queueSettleSourceOwed), so the settle costs the write no more than a statement after the
// first. A writer asks for it only where the replacement is the last of the writes that index the
// source's whole text - the blocks it holds, an ask's parts - queued after every delete that
// index makes, each of which owes the first queue the source (migrations
// 20261117025906-20261117025910): a comment's or a message's references after its blocks, a
// document's after its block index, the question of an ask after its parts. A write that leaves
// the blocks for later, or queues them after the replacement, settles them there instead
// (ItemIndexing.SettlingForms) or leaves the source owed.
func (r Replacement) SettlingForms() Replacement {
	r.forms = true
	return r
}

// Queue queues the replacement's writes on a write phase's batch: the delete of the edges found no
// longer names, the insert of the new ones, and the settlement of what the source owes the
// re-index queues (queueSettleSourceOwed). A surviving edge keeps its created_at and source_seq.
func (r Replacement) Queue(b *store.WriteBatch) {
	b.Queue("clear references", `
		delete from refs
		where from_kind = $1 and from_id = $2 and to_kind = any($5)
		  and not exists (
		    select 1 from unnest($3::text[], $4::text[]) as target(kind, id)
		    where target.kind = refs.to_kind and target.id = refs.to_id
		  )
	`, r.fromKind, r.fromID, r.kinds, r.ids, nodeKinds)
	if len(r.kinds) > 0 {
		b.Queue("write reference", `
			insert into refs (from_kind, from_id, to_kind, to_id, created_at)
			select $1, $2, target.kind, target.id, coalesce($5::timestamptz, now())
			from unnest($3::text[], $4::text[]) as target(kind, id)
			on conflict (from_kind, from_id, to_kind, to_id) do nothing
		`, r.fromKind, r.fromID, r.kinds, r.ids, r.insertedAt)
	}
	queueSettleSourceOwed(b, r.fromKind, r.fromID, r.forms)
}

// queueSettleSourceOwed queues the delete of the row one source owes the whole grammar's
// re-index queue (RefsGrammarOwed), which a write settles once it has read the source's own text
// with the whole grammar, and, with forms, of the row it owes the forms queue (RefsFormsOwed) too,
// in the same statement, and of the rows it owes the decisions forms queues (RefsDecisionsFormsOwed
// and its bootstrap half RefsDecisionsFormsSeedOwed) in a statement after that one: a write passes
// forms only once it has indexed the source's whole text, its blocks too, after every delete that
// index made, which is the statement order of its write phase. The delete from RefsFormsOwed
// relays each row it removes into RefsDecisionsFormsOwed (migration 20261126040100), whoever sends
// it, at the end of its statement, so the row it relays is cleared by a statement that comes after
// it, never by one beside it in a common table expression, which would run before the relay and
// leave the row for good. It runs in the write's own transaction, which holds that source's owner:
// every write of the text was committed before this transaction read it, so whatever enqueued_at
// a row holds, the text this write indexed is the text the row is owed for. It is the queues' four
// source kinds that are settled this way -
// artifact, ask, comment and message, which are the from_kind of every refs row too, so one
// statement serves them all. The fifth kind of the whole grammar's queue, `scope`, names a
// document whose sources are owed a re-index, not a source whose refs this writes, and only the
// scope drain settles it. A from_id that is no uuid (a test's synthetic source) owes nothing,
// since the queues key their id as one, and queues nothing.
//
// A build older than a queue's table settles nothing of it, so a text it indexes stays owed in
// that table until a build that reads its grammar indexes it (RefsFormsOwed); a build that settles
// RefsFormsOwed and knows nothing of RefsDecisionsFormsOwed leaves what the relay wrote there
// owed, which is what the queue is for.
func queueSettleSourceOwed(b *store.WriteBatch, fromKind, fromID string, forms bool) {
	id, ok := canonicalUUID(fromID)
	if !ok {
		return
	}
	statement := `delete from refs_grammar_owed where kind = $1 and id = $2::uuid`
	if forms {
		statement = `
			with grammar as (delete from refs_grammar_owed where kind = $1 and id = $2::uuid)
			delete from refs_forms_owed where kind = $1 and id = $2::uuid`
	}
	b.Queue(fmt.Sprintf("settle the index owed by %s %s", fromKind, fromID), statement, fromKind, id)
	if forms {
		b.Queue(fmt.Sprintf("settle the decisions index owed by %s %s", fromKind, fromID),
			settleDecisionsFormsStatement, fromKind, id)
	}
}

// SettleOwedDocuments queues, on a write phase's batch, what queueSettleSourceOwed queues for one
// source, for the documents of an issue room whose text one pass over the room read at their
// latest versions (docs' indexRenderedVersion and indexDeferredVersion): the room's references are
// written under its tree artifact (Replacement.Queue), whose own rows that write settles, and each
// document whose fragment the pass read owes the queues nothing either. The pass read every
// document of the room with this build's whole grammar and its reference forms, under the issue's
// owner lock, so each document's row of the whole grammar's queue is settled here, and with forms
// its row of the forms queue (refs_forms_owed, RefsFormsOwed) too, in the same statement, and its
// rows of the decisions forms queues (RefsDecisionsFormsOwed and its bootstrap half
// RefsDecisionsFormsSeedOwed) in a statement after that one: the delete from refs_forms_owed
// relays each row it removes into the live queue at the end of its statement (migration
// 20261126040100), so only a statement after it clears that row, as queueSettleSourceOwed clears
// one source's (settleDecisionsFormsStatement). A caller passes forms only where the pass won the
// revision fence, and so wrote the documents' blocks in this write, and queues the settle after
// that block index and every delete it made, as a single document's reconciliation settles both
// queues only after its own block index won (Replacement.SettlingForms): a pass that deferred the
// blocks or lost the fence leaves the forms rows owed for the pass that writes them, and deletes
// no row of refs_forms_owed, so nothing is relayed and the bootstrap row stays owed with them.
// Left owed, a row of the whole grammar's queue would be drained after the pass by owing the
// document its block index again, which costs the room a second pass for every version of it.
// documents is in ascending id, the order every writer of the queues' rows of several documents
// takes.
func SettleOwedDocuments(b *store.WriteBatch, documents []string, forms bool) {
	if len(documents) == 0 {
		return
	}
	statement := `delete from refs_grammar_owed where kind = 'artifact' and id = any($1::uuid[])`
	if forms {
		statement = `
			with grammar as (delete from refs_grammar_owed where kind = 'artifact' and id = any($1::uuid[]))
			delete from refs_forms_owed where kind = 'artifact' and id = any($1::uuid[])`
	}
	b.Queue(fmt.Sprintf("settle the index owed by the documents %v", documents), statement, documents)
	if forms {
		b.Queue(fmt.Sprintf("settle the decisions index owed by the documents %v", documents), `
			with seed as (delete from refs_decisions_forms_seed_owed where kind = 'artifact' and id = any($1::uuid[]))
			delete from refs_decisions_forms_owed where kind = 'artifact' and id = any($1::uuid[])`, documents)
	}
}

func scanTargets(rows pgx.Rows, err error) ([]Target, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	changed := []Target{}
	for rows.Next() {
		var target Target
		if err := rows.Scan(&target.Kind, &target.ID); err != nil {
			return nil, err
		}
		changed = append(changed, target)
	}
	return changed, rows.Err()
}

// TargetOf is the graph target a parsed reference names, the kind and id refs and graph_edges
// store an edge to it under; an external URL names none, and neither does a party - an agent's
// session, a role or a person. A party is a reader, not a record: the graph holds no node for
// one, so no write stores an edge to one (refTargets drops it) and no read addresses one
// (api.parseReferencesQuery refuses it). A block of a comment, of a message or
// of an ask — a part of its question, its options and its answer — is stored under its item's
// block kind, comment_block, message_block or ask_block (ItemKind.BlockKind), not block: a build
// that predates item blocks skips a kind it does not know (resolveNodes), where it would count a
// block row and resolve it through document_blocks.
func TargetOf(ref text.Ref) (Target, bool) {
	if ref.Kind == "url" || ref.IsParty() {
		return Target{}, false
	}
	kind := ref.Kind
	if kind == "block" && ref.Item != "" {
		kind = ItemKind(ref.Item).BlockKind()
	}
	return Target{Kind: kind, ID: text.TargetID(ref)}, true
}

// refTargets is the deduplicated (to_kind, to_id) set found names, as parallel arrays.
func refTargets(found []text.Ref) ([]string, []string) {
	kinds, ids := []string{}, []string{}
	seen := make(map[Target]struct{})
	for _, ref := range found {
		target, ok := TargetOf(ref)
		if !ok {
			continue
		}
		if _, dup := seen[target]; dup {
			continue
		}
		seen[target] = struct{}{}
		kinds = append(kinds, target.Kind)
		ids = append(ids, target.ID)
	}
	return kinds, ids
}

// Stamp records eventID as the write that introduced every edge of one source that has no
// provenance yet: the source's own refs and, for a document, a comment, a message or an ask, the
// mentions its blocks hold, an ask's parts included. It runs after the source's event is
// appended, in the same transaction, and leaves edges that survived a reconcile untouched. Its
// statements run at once, as a batch of their own.
func Stamp(ctx context.Context, tx pgx.Tx, fromKind, fromID string, eventID int64) error {
	return store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		queueStamp(b, fromKind, fromID, &eventID)
		return nil
	})
}

// QueueStamp queues Stamp on a write phase's batch, right after the source's event, whose id the
// statements take as store.LatestEventID: the id the event's insert took, which nothing in Go
// reads before they are sent.
func QueueStamp(b *store.WriteBatch, fromKind, fromID string) {
	queueStamp(b, fromKind, fromID, nil)
}

// queueStamp queues Stamp's statements with eventID, or with store.LatestEventID when it is nil.
func queueStamp(b *store.WriteBatch, fromKind, fromID string, eventID *int64) {
	refsID, refsArgs := store.LatestEventID, []any{fromKind, fromID}
	blocksID, blocksArgs := store.LatestEventID, []any{fromID}
	if eventID != nil {
		refsID, refsArgs = "$3", append(refsArgs, *eventID)
		blocksID, blocksArgs = "$2", append(blocksArgs, *eventID)
	}
	b.Queue("stamp references", `
		update refs set source_seq = `+refsID+`
		where from_kind = $1 and from_id = $2 and source_seq is null
	`, refsArgs...)
	var blockRefs string
	switch fromKind {
	case "artifact":
		blockRefs = `update block_refs set source_seq = ` + blocksID + ` where artifact_id = $1 and source_seq is null`
	case string(ItemComment), string(ItemMessage), string(ItemAsk):
		// An item's blocks are keyed by its uuid; an id that is none names no item and holds no
		// block mention.
		if uuid.Validate(fromID) != nil {
			return
		}
		tables, _ := ItemKind(fromKind).tables()
		blockRefs = `update ` + tables.refs + ` set source_seq = ` + blocksID + ` where ` + tables.item + ` = $1::uuid and source_seq is null`
	default:
		return
	}
	b.Queue("stamp block references", blockRefs, blocksArgs...)
}

// CountedChangeLimit bounds the targets of one kind (issue, ask, artifact, block) an event names,
// so an event carries at most four times as many. Each kind is capped on its own: an index whose
// one paragraph cites forty documents still names every one and every issue beside them, and a
// reader falls back only for the kind it was not told about. A hundred entries of the longest
// kind, a block (its node id, its reference and its document's uuid, about 200 bytes), is some
// 20 KB, so a full event of four kinds stays under 100 KB: about what an event at
// contracts.DispatchEventBodyMax already carries, and far under NATS's 1 MiB maximum payload
// that the outbox publishes it through. A body citing more is rare enough that carrying an
// unbounded array on every event is the wrong trade.
const CountedChangeLimit = 100

// countedKinds is the order CountedChanges lists the kinds it truncated in.
var countedKinds = [...]string{"issue", "ask", "artifact", "block"}

// CountedChanges names the changed targets a reader refreshes by name — an issue (its detail, its
// count and its references read), an ask (every list that carries the ask row, plus the human
// Inbox), a document or file (its Referenced by panel) and a block of a document (its own panel
// and every link to it, which read one graph read, and its document's panel, which lists the
// edges at its blocks) — in the shape those readers are keyed by. A part of an ask is named as its
// ask, whose cards and panel are what a link to one of its parts moves. Comments, messages, their
// blocks and components are not named themselves: nothing counts them, and their panels refresh
// on the graph itself.
//
// A target that is an issue's own content — a document of the issue or a block of one, an ask of
// it or a part of one, a comment or a message on it or a block of either — also names that issue:
// the issue's references read lists what links into its content, so a link added to or removed
// from that content changes the read, and a page holding it refreshes it on a target of kind issue
// (dispatch://INFRA-4704, R6). Each such issue is named once, after every target named above, in
// the order of its first content target, and not where the write names it already. Content no
// issue owns (a project's or an agent's document, the ask or the comment of a project document, a
// direct message) names no issue, and nor does a document this transaction has only queued: the
// write that queues it appends an event of the document's own issue, which refreshes that issue's
// read. Each target's issue is read by key: a document's and an ask's from the row the target is
// resolved by, a document's block's from the document holding it (resolveBlockTargets), and a
// comment's and a message's, a block's through the item its node id names, by one primary key
// read of each kind.
//
// A kind with more than CountedChangeLimit named targets, the issues of content targets counted
// among the issues, names none of them and is listed in TruncatedKinds, and the reader falls back
// to refreshing every read of that kind; the other kinds are still named.
//
// A reference the grammar accepts need not address anything: `dispatch://CORE-1/ask/hello`
// parses and is stored, so every lookup here compares text and a target that resolves to no
// row is simply not named. A write must never fail because of what its body cites, with one
// exception: a typed block's `block[]` attribute is a schema field, not prose, and an agent's or
// API write introducing a pointer is refused when it names a block of a type the attribute's
// targets do not allow, a block no document holds, or a block outside the issues and documents
// the attribute may point into (pmdoc.CheckBlockPointers). A browser's edit is never refused, so
// a pointer it leaves at a block no document holds stays unresolved.
//
// A block is named as the read of its reference answers it (resolveBlockTargets): by the node its
// far end names, which for a block of a document converted into its issue's room is the room's
// node however the citation spells it, and by the document holding it, never the room's tree.
// Two far ends that name one node name it once.
func CountedChanges(ctx context.Context, q Queryer, changed []Target) (model.ReferenceChanges, error) {
	// lookups is the ids of each kind of target to resolve, in the targets' order.
	lookups := map[string][]string{}
	ordered := make([]Target, 0, len(changed))
	seen := make(map[[2]string]struct{}, len(changed))
	for _, target := range changed {
		// A block of an ask, a comment or a message is counted as its item, the one its node id
		// names, so two blocks of one item, or a block and the item's own row, count that item
		// once.
		if kind, itemID, _, ok := ParseItemBlockNode(target.ID); ok && target.Kind == kind.BlockKind() {
			target = Target{Kind: string(kind), ID: itemID}
		}
		if _, duplicate := seen[[2]string{target.Kind, target.ID}]; duplicate {
			continue
		}
		seen[[2]string{target.Kind, target.ID}] = struct{}{}
		lookups[target.Kind] = append(lookups[target.Kind], target.ID)
		ordered = append(ordered, target)
	}
	issueKeys, err := resolveIssueTargets(ctx, q, lookups["issue"])
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	// resolveIssueTargets answers which keys name an issue, the set Reader.Issues reads as well;
	// an issue is named as itself, and is no issue's content.
	issues := make(map[string]resolvedTarget, len(issueKeys))
	for key := range issueKeys {
		issues[key] = resolvedTarget{reference: &model.ChangedReference{Kind: "issue", ID: key}}
	}
	asks, err := resolveAskTargets(ctx, q, lookups["ask"])
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	artifacts, err := resolveArtifactTargets(ctx, q, lookups["artifact"])
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	blocks, err := resolveBlockTargets(ctx, q, lookups["block"])
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	comments, err := resolveItemIssues(ctx, q, ItemComment, lookups["comment"])
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	messages, err := resolveItemIssues(ctx, q, ItemMessage, lookups["message"])
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	// resolved is each kind's resolved targets, keyed as the targets of that kind are looked up.
	// A target of a kind none resolves names nothing.
	resolved := map[string]map[string]resolvedTarget{
		"issue": issues, "ask": asks, "artifact": artifacts, "block": blocks, "comment": comments, "message": messages,
	}
	references := make([]model.ChangedReference, 0, len(ordered))
	namedBlocks := make(map[string]struct{}, len(blocks))
	// owners is the issue of each content target, in the targets' order, an issue as often as
	// its content is changed.
	owners := make([]string, 0, len(ordered))
	for _, target := range ordered {
		entry, found := resolved[target.Kind][target.ID]
		if !found {
			continue
		}
		if target.Kind == "block" {
			// Two far ends that resolve to one node name it once.
			if _, named := namedBlocks[entry.reference.ID]; named {
				continue
			}
			namedBlocks[entry.reference.ID] = struct{}{}
		}
		if entry.reference != nil {
			references = append(references, *entry.reference)
		}
		if entry.issue != "" {
			owners = append(owners, entry.issue)
		}
	}
	issuesNamed := make(map[string]struct{}, len(references)+len(owners))
	for _, reference := range references {
		if reference.Kind == "issue" {
			issuesNamed[reference.ID] = struct{}{}
		}
	}
	for _, owner := range owners {
		if _, named := issuesNamed[owner]; named {
			continue
		}
		issuesNamed[owner] = struct{}{}
		references = append(references, model.ChangedReference{Kind: "issue", ID: owner})
	}
	named := make(map[string]int, len(countedKinds))
	for _, reference := range references {
		named[reference.Kind]++
	}
	changes := model.ReferenceChanges{}
	for _, kind := range countedKinds {
		if named[kind] > CountedChangeLimit {
			changes.TruncatedKinds = append(changes.TruncatedKinds, kind)
		}
	}
	for _, reference := range references {
		if named[reference.Kind] <= CountedChangeLimit {
			changes.Targets = append(changes.Targets, reference)
		}
	}
	return changes, nil
}

// resolvedTarget is a changed target as its resolver answers it for CountedChanges: reference,
// the entry its readers are keyed by, nil for a target named by its issue alone (a comment, a
// message), and issue, the key of the issue whose own content the target is, empty for a target
// that is no issue's content (an issue itself, and content no issue owns) and for a document the
// reading transaction has only queued.
type resolvedTarget struct {
	reference *model.ChangedReference
	issue     string
}

// MergeChanges is what two counted parts of one write moved, for an event that reports both: each
// target once, first's before second's. A kind either part truncated stays truncated and names
// none of its targets, and a kind whose merged targets pass CountedChangeLimit is truncated, as
// CountedChanges would have left it.
func MergeChanges(first, second model.ReferenceChanges) model.ReferenceChanges {
	truncated := make(map[string]bool, len(countedKinds))
	for _, kind := range slices.Concat(first.TruncatedKinds, second.TruncatedKinds) {
		truncated[kind] = true
	}
	references := make([]model.ChangedReference, 0, len(first.Targets)+len(second.Targets))
	seen := make(map[[2]string]struct{}, len(first.Targets)+len(second.Targets))
	named := make(map[string]int, len(countedKinds))
	for _, reference := range slices.Concat(first.Targets, second.Targets) {
		key := [2]string{reference.Kind, reference.ID}
		if _, duplicate := seen[key]; duplicate || truncated[reference.Kind] {
			continue
		}
		seen[key] = struct{}{}
		references = append(references, reference)
		named[reference.Kind]++
	}
	merged := model.ReferenceChanges{}
	for _, kind := range countedKinds {
		if truncated[kind] || named[kind] > CountedChangeLimit {
			merged.TruncatedKinds = append(merged.TruncatedKinds, kind)
		}
	}
	for _, reference := range references {
		if named[reference.Kind] <= CountedChangeLimit {
			merged.Targets = append(merged.Targets, reference)
		}
	}
	return merged
}

// resolveIssueTargets is which of keys names an issue: one whose row is stored, and one whose
// row the reading transaction has queued and not written yet (Pending), which a new issue's
// spec writing its own key bare names.
func resolveIssueTargets(ctx context.Context, q Queryer, keys []string) (map[string]struct{}, error) {
	found := map[string]struct{}{}
	if len(keys) == 0 {
		return found, nil
	}
	rows, err := q.Query(ctx, `select key from issues where key = any($1)`, keys)
	if err != nil {
		return nil, fmt.Errorf("resolve changed issue references: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("scan changed issue reference: %w", err)
		}
		found[key] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	mergePending(ctx, found, keys, func(pending Pending, key string) (struct{}, bool) {
		return struct{}{}, pending.PendingIssue(key)
	})
	return found, nil
}

// resolveAskTargets skips a citation that is not a uuid rather than casting the column to text:
// `dispatch://CORE-1/ask/hello` parses and must never fail the write, and a target nothing can
// address resolves to nothing either way — while `id = any(...)` keeps the asks primary key,
// which matters because this runs inside the write transaction of every body that cites an ask.
//
// An ask whose row the reading transaction has queued and not written yet is named from what
// that insert writes (Pending), as the stored ones are: settlement opens the ask of every new
// question block of an edit in one transaction, so a `#id` naming another new block of that
// same edit is stored as the edge of an ask no read of the table answers for.
func resolveAskTargets(ctx context.Context, q Queryer, ids []string) (map[string]resolvedTarget, error) {
	found := map[string]resolvedTarget{}
	addressable := uuidsOf(ids)
	if len(addressable) == 0 {
		return found, nil
	}
	rows, err := q.Query(ctx, `
		select id::text, issue_key, artifact_id::text
		from asks
		where id = any($1)
	`, addressable)
	if err != nil {
		return nil, fmt.Errorf("resolve changed ask references: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		reference := model.ChangedReference{Kind: "ask"}
		if err := rows.Scan(&reference.ID, &reference.IssueKey, &reference.ArtifactID); err != nil {
			return nil, fmt.Errorf("scan changed ask reference: %w", err)
		}
		found[reference.ID] = askTarget(reference)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	mergePending(ctx, found, ids, func(pending Pending, id string) (resolvedTarget, bool) {
		ask, queued := pending.PendingAsk(id)
		if !queued {
			return resolvedTarget{}, false
		}
		return askTarget(model.ChangedReference{Kind: "ask", ID: ask.ID, IssueKey: ask.IssueKey, ArtifactID: ask.ArtifactID}), true
	})
	return found, nil
}

// askTarget is the ask whose entry is reference as CountedChanges names it: that entry, and the
// issue it is on, the one whose content it is.
func askTarget(reference model.ChangedReference) resolvedTarget {
	target := resolvedTarget{reference: &reference}
	if reference.IssueKey != nil {
		target.issue = *reference.IssueKey
	}
	return target
}

// uuidsOf is each of ids that parses as a uuid, the rest skipped: a citation the grammar accepts
// need not be a uuid (`dispatch://CORE-1/comment/hello`), and a resolver that compares the uuid
// key column with these, rather than casting the column to text, keeps its primary key.
func uuidsOf(ids []string) []uuid.UUID {
	parsed := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if value, err := uuid.Parse(id); err == nil {
			parsed = append(parsed, value)
		}
	}
	return parsed
}

// resolveArtifactTargets resolves cited artifacts by the ref_key `refs` stores for them, the
// address GET /api/v1/references?to= resolves too, into the entry a Referenced by panel is
// matched by: the artifact's uuid (its node id in that read) and its dispatch:// address (the
// reference the dashboard opens the panel on, refs.ArtifactRef), beside the issue that owns it
// (CountedChanges names that issue too), none for an artifact no issue owns (a project's, an
// agent's). Keyed by ref_key. A document whose artifacts row the reading transaction has queued
// and not written yet is resolved from what that insert writes (Pending), which names no issue:
// a new issue's spec citing its own address names a row this transaction has only queued.
func resolveArtifactTargets(ctx context.Context, q Queryer, refKeys []string) (map[string]resolvedTarget, error) {
	found := map[string]resolvedTarget{}
	if len(refKeys) == 0 {
		return found, nil
	}
	rows, err := q.Query(ctx, `
		select id::text, ref_key, issue_key, coalesce(project_key, ''), session_id, slug, kind, is_primary
		from artifacts
		where ref_key = any($1)
	`, refKeys)
	if err != nil {
		return nil, fmt.Errorf("resolve changed artifact references: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, refKey, project, slug, kind string
		var issueKey, session *string
		var primary bool
		if err := rows.Scan(&id, &refKey, &issueKey, &project, &session, &slug, &kind, &primary); err != nil {
			return nil, fmt.Errorf("scan changed artifact reference: %w", err)
		}
		artifact := resolvedTarget{reference: &model.ChangedReference{
			Kind: "artifact",
			ID:   id,
			Ref:  ArtifactRef(issueKey, project, session, slug, kind, primary),
		}}
		if issueKey != nil {
			artifact.issue = *issueKey
		}
		found[refKey] = artifact
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	mergePending(ctx, found, refKeys, func(pending Pending, refKey string) (resolvedTarget, bool) {
		id, ref, queued := pending.PendingArtifact(refKey)
		if !queued {
			return resolvedTarget{}, false
		}
		return resolvedTarget{reference: &model.ChangedReference{Kind: "artifact", ID: id, Ref: ref}}, true
	})
	return found, nil
}

// resolveItemIssues resolves each of ids that names a comment or a message of kind into the issue
// it is on, keyed by id, with no entry of its own (resolvedTarget): nothing counts a comment or a
// message. It reads the items table by its primary key in one statement. An id that is no uuid
// (uuidsOf), names no row, or names a row no issue owns - a comment of a project document, a
// direct message - answers nothing.
func resolveItemIssues(ctx context.Context, q Queryer, kind ItemKind, ids []string) (map[string]resolvedTarget, error) {
	found := map[string]resolvedTarget{}
	addressable := uuidsOf(ids)
	if len(addressable) == 0 {
		return found, nil
	}
	tables, err := kind.tables()
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `
		select id::text, issue_key
		from `+tables.items+`
		where id = any($1) and issue_key is not null
	`, addressable)
	if err != nil {
		return nil, fmt.Errorf("resolve the issues of changed %s references: %w", kind, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, issueKey string
		if err := rows.Scan(&id, &issueKey); err != nil {
			return nil, fmt.Errorf("scan the issue of a changed %s reference: %w", kind, err)
		}
		found[id] = resolvedTarget{issue: issueKey}
	}
	return found, rows.Err()
}

// mergePending fills the gaps a read of the stored rows leaves. found is what that read
// answered and keys is what it was asked about: for each key found holds no target for, queued
// says whether the reading transaction has that row queued and not written yet (Pending), and
// what it answers goes into found. A stored row wins over a queued one of the same key, since
// the read already holds what the row says, and a transaction with nothing queued - every read
// outside a write's two phases - adds nothing.
//
// Every resolver that answers for a queued row merges through this one call, so a kind added to
// Pending is merged the way the kinds before it are.
func mergePending[Key comparable, Target any](
	ctx context.Context,
	found map[Key]Target,
	keys []Key,
	queued func(pending Pending, key Key) (Target, bool),
) {
	pending := pendingOf(ctx)
	if pending == nil {
		return
	}
	for _, key := range keys {
		if _, stored := found[key]; stored {
			continue
		}
		if target, ok := queued(pending, key); ok {
			found[key] = target
		}
	}
}

// resolveBlockTargets resolves cited blocks, by the far ends `refs` and `block_refs` store for
// them, into the entry a block's own read and its document's panel are matched by, keyed by the
// far end: the node the far end names (NamedBlockSQL), which the block's read answers for its
// reference; the uuid of the document holding the block, read off the block's row
// (BlockDocumentSQL) while one stands, else, for a block a version deleted from an issue room, the
// document that room last held it in (RoomBlockDocumentSQL), else the document the far end names
// before its '#'; and the block's reference at that document's address (BlockRef), beside the
// issue that owns that document (CountedChanges names that issue too), none for a document no
// issue owns. A far end whose document no row holds is absent.
func resolveBlockTargets(ctx context.Context, q Queryer, addresses []string) (map[string]resolvedTarget, error) {
	found := map[string]resolvedTarget{}
	if len(addresses) == 0 {
		return found, nil
	}
	rows, err := q.Query(ctx, `
		select p.address, named.node_id, document.id::text, document.issue_key, coalesce(document.project_key, ''),
		       document.session_id, document.slug, document.kind, document.is_primary
		from unnest($1::text[]) as p (address)
		cross join lateral (select `+NamedBlockSQL("p.address")+` as node_id) named
		left join artifacts keyed on keyed.ref_key = split_part(named.node_id, '#', 1)
		join artifacts document on document.id = coalesce(
			(select `+BlockDocumentSQL("held")+` from document_blocks held where held.node_id = named.node_id limit 1),
			`+RoomBlockDocumentSQL("keyed.id", "substr(named.node_id, strpos(named.node_id, '#') + 1)")+`,
			(select written.id from artifacts written where written.ref_key = split_part(p.address, '#', 1)))
	`, addresses)
	if err != nil {
		return nil, fmt.Errorf("resolve changed block references: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var address, node, id, project, slug, kind string
		var issueKey, session *string
		var primary bool
		if err := rows.Scan(&address, &node, &id, &issueKey, &project, &session, &slug, &kind, &primary); err != nil {
			return nil, fmt.Errorf("scan changed block reference: %w", err)
		}
		_, blockID, _ := strings.Cut(node, "#")
		block := resolvedTarget{reference: &model.ChangedReference{
			Kind:       "block",
			ID:         node,
			ArtifactID: &id,
			Ref:        BlockRef(ArtifactRef(issueKey, project, session, slug, kind, primary), blockID),
		}}
		if issueKey != nil {
			block.issue = *issueKey
		}
		found[address] = block
	}
	return found, rows.Err()
}

// ReferencedBy lists Dispatch items and artifacts that mention an artifact reference key or one
// of its current blocks, once per source, in the shape the document surfaces read (kind, id
// order). A citing document, comment, message or ask is listed itself, not by the block holding
// its link: an ask that cites the document from one of its parts alone — an option, or the
// answer it was given — is listed as that ask, once. A source who may not see is listed to
// nobody but its author. serverURL is the deployment the caller reads, whose dashboard URLs each
// source's excerpt is cut around as references, as the graph read cuts them (Query.ServerURL).
func ReferencedBy(ctx context.Context, q Queryer, who model.Actor, refKey, serverURL string) ([]model.ReferencedBy, error) {
	edges, err := resolveEdges(ctx, q, who, Query{
		ServerURL: serverURL, Direction: "in", Kind: "artifact", ID: refKey, Kinds: []string{"mentions"}, WholeDocuments: true,
	})
	if err != nil {
		return nil, err
	}
	references := make([]model.ReferencedBy, 0, len(edges))
	// Every edge of one source carries that source's node and excerpt, so the entry kept is
	// the same whichever of its edges comes first.
	seen := make(map[[2]string]struct{}, len(edges))
	for _, edge := range edges {
		source := [2]string{edge.Node.Kind, edge.Node.ID}
		if _, listed := seen[source]; listed {
			continue
		}
		seen[source] = struct{}{}
		reference := model.ReferencedBy{
			Kind:     edge.Node.Kind,
			ID:       edge.Node.ID,
			IssueKey: edge.Node.IssueKey,
			Project:  edge.Node.Project,
			RefKey:   edge.refKey,
		}
		if edge.Excerpt != nil {
			reference.Excerpt = edge.Excerpt.Text
		}
		references = append(references, reference)
	}
	sort.Slice(references, func(left, right int) bool {
		if references[left].Kind != references[right].Kind {
			return references[left].Kind < references[right].Kind
		}
		return references[left].ID < references[right].ID
	})
	return references, nil
}

// Outgoing lists Dispatch mentions written by an artifact, resolving artifact targets. A
// document's mentions are its own references; the references of an issue room's text are its
// tree's (docs' indexRenderedVersion writes them under the room), which belong to no one document
// of the room, so a document converted into its issue's room writes those of them a block of its
// own holds, read where the block index keeps its blocks (DocumentRoomBlocksSQL), each at the
// address it was written at, a block's link at the target or at the node the target resolves to,
// as heldByABlock reads a mention held by a block. A block target keeps its own kind and id and
// carries its document as the artifact, while a current version holds the block; a block none
// holds is omitted, as is a target of a node kind this build does not know. A block of an ask, a
// comment or a message, which this shape has no field for, is listed by the graph read (Edges)
// alone. Any other unresolved target is listed. A block target is the
// node its address resolves to (ResolvedBlockSQL), so a mention of a block of a document
// converted into its issue's room, written at the document's own address, reaches the room's row
// of the block, and the document it carries is the one holding the block (BlockDocumentSQL),
// never the room's tree.
//
// It answers for who. A document who may not see writes no mention here, so its mentions say
// nothing of it, and a target who may not see is no mention at all - for a block, neither the
// block nor the document it carries (VisibleKind, VisibleEnd), the block read at the node its
// address resolves to. The document is read by its uuid through artifacts' primary key, since
// the store keys it by ref_key and the registry's own rewrite compares the uuid as text, which no
// index serves.
func Outgoing(ctx context.Context, q Queryer, who model.Actor, artifactID string) ([]model.OutgoingReference, error) {
	rows, err := q.Query(ctx, `
		select e.to_kind, e.to_id,
		       a.id::text, a.issue_key, coalesce(a.project_key, ''), a.session_id, coalesce(a.ref_key, ''), coalesce(a.slug, ''), coalesce(a.name, ''), coalesce(a.kind, ''), coalesce(a.is_primary, false),
		       coalesce(a.created_by, '{}'::jsonb), coalesce(a.created_at, timestamptz 'epoch'),
		       coalesce(v.number, 0), coalesce(v.named, false), v.summary, coalesce(v.authors, '[]'::jsonb), coalesce(v.created_at, timestamptz 'epoch'), `+model.VersionSize+`, v.mime, v.sha256
		from (
			select g.to_kind, g.to_id from graph_edges g
			where g.from_kind = 'artifact' and g.from_id = $1 and g.kind = 'mentions'
			union
			select g.to_kind, g.to_id from graph_edges g
			cross join (select `+DocumentRoomSQL("$1::text::uuid")+` as id) room
			where room.id <> $1::text::uuid and g.from_kind = 'artifact' and g.from_id = room.id::text and g.kind = 'mentions'
			  and exists (select 1 from block_refs b
			              where `+DocumentRoomBlocksSQL("b", "$1::text::uuid")+` and b.to_kind = g.to_kind
			                and (b.to_id = g.to_id or (g.to_kind = '`+BlockNodeKind+`' and b.to_id = `+ResolvedBlockSQL("g.to_id")+`)))
		) e
		left join lateral (select `+ResolvedBlockSQL("e.to_id")+` as node_id) resolved on e.to_kind = 'block'
		left join document_blocks d on d.node_id = resolved.node_id
		left join artifacts a on a.ref_key = case e.to_kind when 'artifact' then e.to_id
		  when 'block' then (select owner.ref_key from artifacts owner where owner.id = `+BlockDocumentSQL("d")+`) end
		left join artifact_versions v on v.artifact_id = a.id
		where e.to_kind = any($2)
		  and e.to_kind not in `+itemBlockKinds+`
		  and (e.to_kind <> 'block' or d.node_id is not null)
		  and `+VisibleKind("artifact", `(select s.ref_key from artifacts s where s.id = $1::text::uuid)`, "$3")+`
		  and `+VisibleEnd(AnyKind, "e.to_kind", "e.to_id", true, "$3")+`
		  and (e.to_kind <> 'block' or (`+VisibleKind("block", "d.node_id", "$3")+` and `+VisibleKind("artifact", "a.ref_key", "$3")+`))
		order by e.to_kind, e.to_id, v.number
	`, artifactID, nodeKinds, VisibilityArg(who))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	outgoing := []model.OutgoingReference{}
	positions := make(map[string]int)
	for rows.Next() {
		var reference model.OutgoingReference
		var artifactIDValue *string
		var artifact model.Artifact
		var createdBy, authors []byte
		var version model.Version
		if err := rows.Scan(
			&reference.Kind,
			&reference.ToID,
			&artifactIDValue,
			&artifact.IssueKey,
			&artifact.Project,
			&artifact.SessionID,
			&artifact.RefKey,
			&artifact.Slug,
			&artifact.Name,
			&artifact.Kind,
			&artifact.Primary,
			&createdBy,
			&artifact.CreatedAt,
			&version.Number,
			&version.Named,
			&version.Summary,
			&authors,
			&version.CreatedAt,
			&version.Size,
			&version.MIME,
			&version.SHA256,
		); err != nil {
			return nil, err
		}
		if artifactIDValue == nil {
			outgoing = append(outgoing, reference)
			continue
		}
		artifact.ID = *artifactIDValue
		if err := json.Unmarshal(createdBy, &artifact.CreatedBy); err != nil {
			return nil, fmt.Errorf("decode artifact author: %w", err)
		}
		if err := json.Unmarshal(authors, &version.Authors); err != nil {
			return nil, fmt.Errorf("decode artifact version authors: %w", err)
		}
		if position, found := positions[reference.ToID]; found {
			outgoing[position].Artifact.Versions = append(outgoing[position].Artifact.Versions, version)
			continue
		}
		artifact.Versions = []model.Version{version}
		reference.Artifact = &artifact
		positions[reference.ToID] = len(outgoing)
		outgoing = append(outgoing, reference)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, reference := range outgoing {
		if reference.Artifact != nil {
			model.StateFileTokens(reference.Artifact.Kind, reference.Artifact.Versions)
		}
	}
	return outgoing, nil
}
