package docs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The queries a write left owed (R7 of the issue-tree spec). A write that moves the record of a
// query of a converted issue writes no version of that query in its own transaction: in its batch
// it marks the issue's queries owed (QueueQueryMark, migration 20261125090000_issue_tree_query_marks),
// and the drain writes the versions once the write has committed (DrainOwedQueries), in a
// transaction of its own whose window is its reads, then one batch and its COMMIT. The block-index
// tick drains each mark a request left (drainOwedQueryMarks).

// QueryDrainActor is the writer the versions a drain writes name, and their artifact.version, where
// neither a tuple at the mark's greatest_seq nor the event at its greatest_event names the writer
// that marked the issue (queryMarkReadStatement): a system actor of the drain's own, as
// SettlementActor is a settlement's.
var QueryDrainActor = model.Actor{Kind: "system", ID: "query-drain"}

// queryMarkBatch is the most marks one tick of the block-index reconciliation drains.
const queryMarkBatch = 100

// queryMarkStatementOf is the statement that marks the queries of the issue $1 owed where the issue
// has converted - an issue_trees row holds its tree - and marks nothing for any other issue. The
// mark's causes are the greatest of each the marks of the issue hold: the event cause, an SQL
// expression, null for a write no event records, and the newest tuple sequence this session drew
// (latestTupleSeq), which is the sequence of the last tuple the write appended, since the write
// queues the mark after its own appends in the same batch. That tuple's writer (graph_tuples.who)
// is the writer the drain names.
func queryMarkStatementOf(cause string) string {
	return queryMarkUpsertOf(cause, latestTupleSeq, `issue_trees t where t.issue_key = $1`)
}

// latestTupleSeq is the newest tuple sequence this session drew, the graph_tuples seq sequence's
// currval, as store.LatestEventID is the newest event id.
const latestTupleSeq = `(select currval(pg_get_serial_sequence('graph_tuples', 'seq')))`

// queryMarkUpsertOf marks owed the queries of the converted issue whose issue_trees row, aliased
// t, from finds, with the causes cause and sequence, two SQL expressions, each kept as the greatest
// of the marks of the issue.
func queryMarkUpsertOf(cause, sequence, from string) string {
	return `
	insert into issue_query_marks (issue_key, greatest_event, greatest_seq)
	select t.issue_key, ` + cause + `, ` + sequence + `
	from ` + from + `
	on conflict (issue_key) do update set
		greatest_event = greatest(issue_query_marks.greatest_event, excluded.greatest_event),
		greatest_seq = greatest(issue_query_marks.greatest_seq, excluded.greatest_seq)`
}

// The two forms of the mark (queryMarkStatementOf): with its event named as $2, null for none, and
// with the latest event this session appended, the id sequence's currval (store.LatestEventID), for
// a write that queues the mark after its own events in the same batch, whose ids it does not know
// while it builds that batch.
var (
	queryMarkStatement            = queryMarkStatementOf("$2::bigint")
	queryMarkLatestEventStatement = queryMarkStatementOf(store.LatestEventID)
)

// queryMarkReadStatement reads the mark of the issue $1: its two causes, and the writer that marked
// it - the writer of the tuple at its greatest_seq (graph_tuples.who, read through the tuple's
// primary key), and where no tuple stands there, the actor of the event at its greatest_event (read
// through its primary key), which a mark that names no tuple of its own writer names instead
// (askQueryMarkStatement) - null where neither stands.
const queryMarkReadStatement = `
	select m.greatest_event, m.greatest_seq, g.who
	from issue_query_marks m
	left join lateral (
		select coalesce(
			(select t.who from graph_tuples t where t.seq = m.greatest_seq),
			(select e.actor from events e where e.id = m.greatest_event)) as who
	) g on true
	where m.issue_key = $1`

// queryMarkClearStatement clears the mark of the issue $1 only where it still holds the causes the
// drain read, $2 and $3, so a mark a later write raised stays for the next drain.
const queryMarkClearStatement = `
	delete from issue_query_marks
	where issue_key = $1 and greatest_event is not distinct from $2::bigint and greatest_seq = $3`

// queryMarksStatement lists the issues whose queries a mark holds owed, the oldest mark first by
// the tuple sequence that marked it, at most $1 of them.
const queryMarksStatement = `select issue_key from issue_query_marks order by greatest_seq, issue_key limit $1`

// QueueQueryMark queues on b the mark of the queries of the issue issueKey owed a version
// (queryMarkStatementOf), by a write that moved the record of one of them and writes no version of
// it: event is the event the write appended, graphstore.NoEvent for a write no event records, and
// graphstore.LatestEvent for the latest one this session appended, which a settlement's write phase
// names. The write queues it after the tuples it appends, in the same batch, so it adds no
// statement to the write's window, and only where it appended one: the mark's sequence is the last
// one this session drew, whose tuple names the write's writer. Once the batch has run, marked says
// whether the issue has converted and so took the mark, and the write's caller runs the drain after
// its COMMIT (DrainOwedQueries) only then.
func QueueQueryMark(b *store.WriteBatch, issueKey string, event int64, marked *bool) {
	what := "mark the queries of " + issueKey + " owed"
	var queued store.Queued
	switch event {
	case graphstore.LatestEvent:
		queued = b.Queue(what, queryMarkLatestEventStatement, issueKey)
	case graphstore.NoEvent:
		queued = b.Queue(what, queryMarkStatement, issueKey, nil)
	default:
		queued = b.Queue(what, queryMarkStatement, issueKey, event)
	}
	queued.Exec(func(tag pgconn.CommandTag) error {
		*marked = tag.RowsAffected() > 0
		return nil
	})
}

// treeQueryMarkStatementOf is the statement that marks owed the queries of the converted issue
// whose issue_trees row, aliased t, from finds, with the causes cause and sequence, two SQL
// expressions, each the greatest of the marks of the issue (queryMarkUpsertOf), and answers the
// issue's key. from finds one row at most, so the insert never meets one issue twice.
func treeQueryMarkStatementOf(cause, sequence, from string) string {
	return queryMarkUpsertOf(cause, sequence, from) + `
	returning issue_key`
}

var (
	// roomQueryMarkStatement marks the queries of the issue whose tree is the room $1, for a pass
	// that stated nodes and no tuple. Its causes are none for the event - the write that queues it
	// in a document's operation does not know whether its caller appends one, as a node route's
	// mark names none (QueueQueryMark with graphstore.NoEvent) - and, for the sequence, the newest
	// tuple the transaction sees as the statement runs, read off the log's primary key. A sequence
	// this session never drew is no error here, as currval's would be.
	roomQueryMarkStatement = treeQueryMarkStatementOf(`null::bigint`,
		`coalesce((select seq from graph_tuples order by seq desc limit 1), 0)`, `issue_trees t where t.artifact_id = $1`)
	// roomTupleQueryMarkStatement is roomQueryMarkStatement for a pass that stated tuples, which its
	// writer appends under its own event in this batch before the mark (writeLast): the sequence
	// is the newest one this session drew (queryMarkStatementOf's), the write's own last tuple,
	// whose writer the drain names its versions after. The newest tuple the transaction sees would
	// be another writer's where that writer committed a later tuple between this write's append and
	// its mark.
	roomTupleQueryMarkStatement = treeQueryMarkStatementOf(`null::bigint`, latestTupleSeq,
		`issue_trees t where t.artifact_id = $1`)
	// askQueryMarkStatement marks the queries of the issue of the ask $1 where it is a question
	// (questionAsk) of a converted issue: the decisions query takes such an ask by its node
	// (decisionsTakes), and a request of a document's approval, an ask on a project document and
	// an ask of an issue that has not converted mark nothing. Every write that queues it appends an
	// event of its own before it in the same batch - the ask's event, or the follow's - but not
	// always a tuple: a reword states nothing on the ask's node. So its event is the latest one
	// this session appended (store.LatestEventID), and it names no tuple (sequence 0): the drain
	// names its versions after that event's actor, the write's own writer (queryMarkReadStatement),
	// where no later write's tuple raised the mark, and never after whoever wrote the newest tuple
	// of the store before this write.
	askQueryMarkStatement = treeQueryMarkStatementOf(store.LatestEventID, `0`, `asks k join issue_trees t on t.issue_key = k.issue_key
	where k.id = $1::uuid and k.kind = '`+questionAsk+`'`)
)

// markRoomQueries marks owed, at the end of the write phase of the transaction ctx joined
// (writeLast), the queries of the issue whose room is room, where a write of one of the room's
// documents ran the room's pass outside the room's settlement (indexDocumentInRoom) and that pass
// stated a node or a tuple (stated): R14 has each accepted write of the room append a version of
// each query whose answer changed, and such a pass can move the record of a query whose document
// the write writes no version of - a requirement written into the plan moves the spec's. The
// issue's queries are drained once the transaction has committed (Ledger.Commit, drainOwed), as a
// settlement's are (queueSettledQueryMark), so the drain writes those versions, R7's fill of an
// owed digest first. It marks once a transaction for a room, and nothing for a room the
// transaction creates, whose issue holds no approval yet, nor where no ledger the caller joined
// queues tx's writes: a background pass's ledger commits with no drain after it, and the
// decisions step's ledger writes at once.
func markRoomQueries(ctx context.Context, tx pgx.Tx, room string, stated graphstore.Uncaused) error {
	ledger := queueFor(ctx, tx)
	if ledger == nil || ledger.tx == nil || len(stated.Nodes)+len(stated.Tuples) == 0 || slices.Contains(ledger.markedRooms, room) {
		return nil
	}
	if _, created := pendingArtifact(ctx, room); created {
		return nil
	}
	ledger.markedRooms = append(ledger.markedRooms, room)
	statement := roomQueryMarkStatement
	if len(stated.Tuples) > 0 {
		statement = roomTupleQueryMarkStatement
	}
	return writeLast(ctx, tx, ledger.markStep("mark the queries of the issue of room "+room+" owed", statement, room))
}

// MarkAskQueries marks owed, at the end of the write phase (the ledger's closing steps, as
// writeLast queues them), the queries of the issue of ask, a question of a converted issue whose
// row or node this transaction writes: its create, a change of its words, which the decisions
// query's rendering prints, and every change of its answer state, which PlanSettle marks for it.
// The decisions query takes the ask by its node (decisionsTakes), and its record holds every value
// and every relation out of that node, so R14 owes that query a version, which the drain writes
// once the transaction has committed (Ledger.Commit, drainOwed). It marks once a transaction for an
// issue, and nothing for a request of a document's approval, which no query takes, for an ask on no
// issue, nor where no ledger the caller joined queues the writes: a settlement's own mark covers
// the asks it settles (queueSettledQueryMark), and the statement itself marks nothing for an issue
// that has not converted (askQueryMarkStatement). The step is not one the write phase drops with a
// carry (droppable): a mark the write did not need costs a drain that finds no query moved.
func (l *Ledger) MarkAskQueries(ask model.Ask) {
	if l == nil || l.tx == nil || l.sealed || ask.Kind != questionAsk || ask.IssueKey == nil || slices.Contains(l.markedIssues, *ask.IssueKey) {
		return
	}
	l.markedIssues = append(l.markedIssues, *ask.IssueKey)
	l.closing = append(l.closing, l.markStep("mark the queries of the issue of ask "+ask.ID+" owed", askQueryMarkStatement, ask.ID))
}

// markStep is the write step of one of the marks above: statement over arg, each issue it marks
// recorded once among the ledger's marks, which Commit drains.
func (l *Ledger) markStep(what, statement, arg string) WriteStep {
	return func(b *store.WriteBatch) error {
		b.Queue(what, statement, arg).Query(func(rows pgx.Rows) error {
			for rows.Next() {
				var issue string
				if err := rows.Scan(&issue); err != nil {
					return err
				}
				if !slices.Contains(l.marks, issue) {
					l.marks = append(l.marks, issue)
				}
			}
			return rows.Err()
		})
		return nil
	}
}

// QueueAskQueryMark queues on b the mark of the queries of the issue of the ask askID owed, where
// it is a question of a converted issue (askQueryMarkStatement), for a write that changes the ask's
// node in a transaction no ledger queues: the follow and the unfollow of an ask, whose followed_by
// is a relation of the node the decisions query's record holds. The write queues it after the
// tuples it appends, in the same batch, so it adds no statement to its window. Once the batch has
// run, marked holds the issue it marked, "" for none, and the write's caller drains that issue's
// queries after its COMMIT (DrainOwedQueries).
func QueueAskQueryMark(b *store.WriteBatch, askID string, marked *string) {
	b.Queue("mark the queries of the issue of ask "+askID+" owed", askQueryMarkStatement, askID).Query(func(rows pgx.Rows) error {
		for rows.Next() {
			if err := rows.Scan(marked); err != nil {
				return err
			}
		}
		return rows.Err()
	})
}

// takeMarks is each issue whose queries the ledger's committed write phase marked owed
// (markRoomQueries, MarkAskQueries), once: the caller that drains them takes them.
func (l *Ledger) takeMarks() []string {
	marks := l.marks
	l.marks = nil
	return marks
}

// drainOwed drains the queries of each of issues (DrainOwedQueries), each issue's fill and drain in
// transactions of their own, once the write that marked them has committed, and answers the events
// they appended, in order, for the write's caller to publish after its own. A caller still holding
// a pooled connection drains nothing, as it fills nothing (fillOwed), and the block-index tick
// drains those marks (drainOwedQueryMarks).
func (s *Service) drainOwed(ctx context.Context, issues []string) []model.Event {
	if len(issues) == 0 || store.HoldsPooledConnection(ctx) {
		return nil
	}
	var appended []model.Event
	for _, issue := range issues {
		appended = append(appended, s.DrainOwedQueries(ctx, issue)...)
	}
	return appended
}

// queryMark is one issue's mark as the drain reads it (queryMarkReadStatement): its two causes and
// the writer that marked it, nil where neither a tuple nor an event names one.
type queryMark struct {
	event  *int64
	seq    int64
	writer *model.Actor
}

// DrainOwedQueries is the drain a request runs once the write that marked the queries of the issue
// issueKey owed has committed, and before the server publishes that write's events (api's publish):
// it fills each record digest a version of the issue owes, then writes a version of each query
// whose record moved and clears the mark (drainQueryMark), named after the writer that marked it:
// the writer of the newest tuple a marking write appended, else its event's actor. It answers the
// events the fill and the drain appended, for the caller to publish after its own, which they
// follow in the event log. A drain that fails is logged (drainLogLevel) and leaves the mark, which
// the block-index tick drains (drainOwedQueryMarks); the write it follows stands committed either
// way, and so does a fill that committed before the drain failed.
func (s *Service) DrainOwedQueries(ctx context.Context, issueKey string) []model.Event {
	appended, _, err := s.drainQueryMark(ctx, issueKey)
	if err != nil {
		slog.Log(ctx, drainLogLevel(err), "dispatch: drain the queries an issue owes", "issue", issueKey, "error", err)
	}
	return appended
}

// drainLogLevel is the level a drain that failed with err is logged at. A drain refused while a
// query version of the issue owes its record digest (errQueryDigestOwed) is no failure: another
// write of the issue committed that version between this drain's fill and its read, as the room's
// settlement an answer schedules does beside the answer's own drain, and that write's own fill,
// then its drain where it marked the issue, else the block-index tick's, takes the mark that
// stays. It is logged at Info, and every other failure at Error.
func drainLogLevel(err error) slog.Level {
	if errors.Is(err, errQueryDigestOwed) {
		return slog.LevelInfo
	}
	return slog.LevelError
}

// drainQueryMark runs, for the issue issueKey, two transactions in order and never one inside the
// other (R7): the fill of each record digest a version of the issue's queries owes
// (fillOwedDigests), which commits first, and then the drain of the issue's mark, where one stands
// (drainMark). It answers the events both appended, the fill's first, and whether it found a mark
// to drain. A fill that fails runs no drain, so no mark is cleared while a digest is owed; a fill
// that committed is answered whatever the drain does.
func (s *Service) drainQueryMark(ctx context.Context, issueKey string) ([]model.Event, bool, error) {
	filled, err := s.fillOwedDigests(ctx, issueKey)
	if err != nil {
		return nil, false, err
	}
	drained, found, err := s.drainMark(ctx, issueKey)
	return append(filled, drained...), found, err
}

// drainMark drains the mark of the issue issueKey, where one stands, in a transaction of its own,
// and answers the events it appended and whether it found a mark to drain. It takes the issue's
// owner lock (store.LockOwner), then the lock of the issue's query rooms (LockQueryRooms), and
// reads the mark under them, with the writer that marked it (queryMarkReadStatement); none of the
// three gives the transaction an id. That writer names the versions it writes, and their
// artifact.version, and QueryDrainActor does where neither a tuple nor an event names one. Under the
// drain's own ledger (queueWrites) it reads what each version is decided on (planRevisions), which
// refuses while a query version of the issue still owes its record digest, so the mark is never
// cleared then, and, where it writes a version, the issue's chain (DocumentEventAncestors); then it
// queues each version (writeRevisions): its row, its node and its version_renders row holding its
// record's digest, read before the batch, and the stand of its block index where no block pass is
// owed it - a version of the decisions query, or one whose bytes are the version before it's
// (queueRevisionStand) - with the delete of the rows of the re-index queue its insert made for the
// document (restoreReindexQueue). For each version it then queues what an edit route's version
// writes beside it (finishRevision): the move of the document's open approval request, the carry
// of an approval where the record moved by appended results alone, and the version's
// artifact.version, naming that chain. Last it queues the clear of the mark, only where it still
// holds the causes read, and then what the versions state, caused by the mark's greatest_event or
// at the sentinel where it holds none, with their head rows, the last rows the write takes. The
// write phase is one batch, and the COMMIT follows it: the transaction's window is that batch and
// its COMMIT. A failure rolls back everything, the clear of the mark with it.
func (s *Service) drainMark(ctx context.Context, issueKey string) ([]model.Event, bool, error) {
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("begin the drain of %s: %w", issueKey, err)
	}
	defer tx.Rollback(ctx)
	if err := store.LockOwner(ctx, tx, store.IssueOwner(issueKey)); err != nil {
		return nil, false, err
	}
	if err := LockQueryRooms(ctx, tx, issueKey); err != nil {
		return nil, false, err
	}
	var mark queryMark
	if err := tx.QueryRow(ctx, queryMarkReadStatement, issueKey).Scan(&mark.event, &mark.seq, &mark.writer); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read the query mark of %s: %w", issueKey, err)
	}
	who := QueryDrainActor
	if mark.writer != nil {
		who = *mark.writer
	}
	ctx, ledger := queueWrites(ctx, tx)
	revisions, err := planRevisions(ctx, tx, issueKey)
	if err != nil {
		return nil, false, err
	}
	// The chain each version's artifact.version names, read once in the read phase, before
	// writeRevisions queues the first write. A drain that writes no version names none.
	var ancestors []string
	if len(revisions) > 0 {
		if ancestors, err = DocumentEventAncestors(ctx, tx, &issueKey); err != nil {
			return nil, false, err
		}
	}
	if err := s.writeRevisions(ctx, tx, revisions, who); err != nil {
		return nil, false, err
	}
	nodes := make([]graphstore.Uncaused, 0, len(revisions))
	for _, revised := range revisions {
		if err := s.finishRevision(ctx, tx, issueKey, revised, who, ancestors); err != nil {
			return nil, false, err
		}
		nodes = append(nodes, revised.node)
	}
	cause := graphstore.NoEvent
	if mark.event != nil {
		cause = *mark.event
	}
	stated := graphstore.Joined(nodes...)
	if err := write(ctx, tx, func(b *store.WriteBatch) error {
		b.Queue("clear the query mark of "+issueKey, queryMarkClearStatement, issueKey, mark.event, mark.seq)
		graphstore.QueueCaused(b, stated, cause)
		graphstore.QueueHeads(b, graphstore.HeldHeads(stated))
		return nil
	}); err != nil {
		return nil, false, err
	}
	if err := ledger.runQueue(ctx, nil); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("commit the drain of %s: %w", issueKey, err)
	}
	return ledger.events, true, nil
}

// finishRevision queues, in the drain's transaction, what an edit route's version writes beside
// the version revised: the move of the document's open approval request to it, in writer's name,
// as a route's version moves one (moveApprovalAsk, versionWrite.settlement unset), so a person's
// approval of the version before reads changed since; the carry of the document's newest approval
// where the version differs from the approved one by appended result nodes alone
// (carryAppendedResults), which appends its artifact.approved after the move; and last the
// version's artifact.version, in writer's name, naming the issue's chain ancestors, which every
// version the drain writes appends.
func (s *Service) finishRevision(ctx context.Context, tx pgx.Tx, issueKey string, revised queryRevision, writer model.Actor, ancestors []string) error {
	id, version := revised.document.id, revised.version
	if err := moveApprovalAsk(ctx, tx, s.events, id, version, revised.markdown, nil, writer, nil, false, s.serverURL); err != nil {
		return err
	}
	if err := s.carryAppendedResults(ctx, tx, issueKey, revised); err != nil {
		return err
	}
	sink := eventSink(ctx)
	event := model.Event{
		IssueKey: &issueKey, Type: "artifact.version", Actor: writer,
		Payload: ArtifactVersionEventPayload(id, revised.document.name, version, nil, model.ReferenceChanges{}, ancestors),
	}
	return write(ctx, tx, func(b *store.WriteBatch) error {
		return s.events.QueueAppend(b, event, sink)
	})
}

// carryAppendedResults carries the newest approval of the document of revised to its version where
// R14 has carryVerdict carry one across a version the drain writes: a version whose only difference
// from the approved one is appended result nodes and the tuples out of them, with every check node
// unchanged. The approved version's record is built again at the cut its version_renders row
// records (approvedRecord), and the version's record holds it, byte for byte, once the result
// nodes it does not hold are taken out (appendsResultsAlone). The carry is then the one every
// version write runs (carryNewestApproval): where the version's text is the approved one's, as a
// version of a record moved alone is, the restore's carry, and where it is not, carryVerdict's
// cases; its reason names the results that arrived, each by its check's block id and its verdict
// (arrivedResults), after the opening the daemon matches. A version whose record digest is the one
// the approval stands on (approvalPin) - one only the rendering moved, whose record the drain read
// in its read phase (planRevisions) - carries by the record case instead (knownRecord,
// carryNewestApprovalNaming), its artifact.approved before the version's artifact.version as every
// carry the drain writes. A version whose record moved otherwise - a result retracted, a check
// moved or retargeted - carries nothing, and the approval reads changed since. Its reads run where
// it is called, the drain's read phase, and its writes are queued after the move's.
func (s *Service) carryAppendedResults(ctx context.Context, tx pgx.Tx, issueKey string, revised queryRevision) error {
	id, number := revised.document.id, revised.version.Number
	if err := refuseSecondCarry(ctx, tx, id); err != nil {
		return err
	}
	newest, err := NewestReview(ctx, tx, id)
	if err != nil || newest == nil || newest.State != "approved" {
		return err
	}
	pin, err := approvalPin(ctx, tx, *newest)
	if err != nil {
		return err
	}
	if pin != nil && *pin == revised.record.Digest {
		tree, err := pmdoc.ParseRendering(revised.markdown)
		if err != nil {
			return skipUnreadableVersion(id, number, err)
		}
		return carryNewestApprovalNaming(ctx, tx, s.events, id, number, revised.markdown, tree, *newest, s.serverURL, decisionStepCases{},
			"", knownRecord(revised.record.Digest), eventSink(ctx))
	}
	approved, held, err := approvedRecord(ctx, tx, issueKey, revised.document.query, id, newest.Version)
	if err != nil || !held {
		return err
	}
	approvedNodes := recordAddresses(approved)
	if !appendsResultsAlone(approvedNodes, approved, revised.record) {
		return nil
	}
	arrived, err := arrivedResults(ctx, tx, approvedNodes, revised.record)
	if err != nil {
		return err
	}
	tree, err := pmdoc.ParseRendering(revised.markdown)
	if err != nil {
		return skipUnreadableVersion(id, number, err)
	}
	return carryNewestApprovalNaming(ctx, tx, s.events, id, number, revised.markdown, tree, *newest, s.serverURL, decisionStepCases{},
		arrivedResultsChange(arrived), RecordCarry{}, eventSink(ctx))
}

// recordAddresses is the address of each node record takes.
func recordAddresses(record QueryRecord) map[string]bool {
	addresses := make(map[string]bool, len(record.Nodes))
	for _, node := range record.Nodes {
		addresses[node.Address()] = true
	}
	return addresses
}

// arrivedResults names each result node record holds that approved does not, approved being the
// address of each node of the approved version's record (recordAddresses), in record's order, as
// "<check block id> (<verdict>)": the block id is the part of its check's address after the last
// '#', and the verdict the result's verdict value (graphstore.AttributeVerdict).
func arrivedResults(ctx context.Context, tx pgx.Tx, approved map[string]bool, record QueryRecord) ([]string, error) {
	var named []string
	for _, node := range record.Nodes {
		if node.Type != graphstore.SupertagResult || approved[node.Address()] {
			continue
		}
		verdict, _, err := graphstore.CurrentValue(ctx, tx, node.Kind, node.ID, graphstore.AttributeVerdict)
		if err != nil {
			return nil, err
		}
		check := node.Parent.ID
		if at := strings.LastIndexByte(check, '#'); at >= 0 {
			check = check[at+1:]
		}
		named = append(named, check+" ("+verdict+")")
	}
	return named, nil
}

// approvedRecord is the record of query of the issue issueKey at the cut the version_renders row of
// version number of the document artifactID records, and whether that row holds one this build
// builds again: a row with its digest and its cut, whose record digests to that digest. A version
// whose row holds none, or whose record this build digests otherwise - a record of an earlier
// definition - holds no record a carry compares.
func approvedRecord(ctx context.Context, tx pgx.Tx, issueKey, query, artifactID string, number int) (QueryRecord, bool, error) {
	var digest *string
	var at *time.Time
	var readSeq *int64
	err := tx.QueryRow(ctx, `select record_sha256, at, read_seq from version_renders where artifact_id = $1 and number = $2`,
		artifactID, number).Scan(&digest, &at, &readSeq)
	if errors.Is(err, pgx.ErrNoRows) {
		return QueryRecord{}, false, nil
	}
	if err != nil {
		return QueryRecord{}, false, fmt.Errorf("read the record of version %d of %s: %w", number, artifactID, err)
	}
	if digest == nil || at == nil || readSeq == nil {
		return QueryRecord{}, false, nil
	}
	tree, err := WalkIssueTree(ctx, tx, issueKey, refs.TreeCut{At: *at, ReadSeq: *readSeq})
	if err != nil {
		return QueryRecord{}, false, err
	}
	record, err := tree.QueryRecord(ctx, tx, query)
	if err != nil {
		return QueryRecord{}, false, err
	}
	return record, record.Digest == *digest, nil
}

// appendsResultsAlone reports whether record differs from approved, the record of the same query
// at an approved version whose node addresses are held (recordAddresses), by result nodes approved
// does not hold alone: with each of them taken out, record's entries are approved's, byte for
// byte and in its order, so every other node, each check among them, and every result approved
// holds stand as they stood. A record that appends no result node is no such difference.
func appendsResultsAlone(held map[string]bool, approved, record QueryRecord) bool {
	rest, before := record.Record, approved.Record
	appended := false
	for _, node := range record.Nodes {
		entry, after, ok := nextRecordEntry(rest)
		if !ok {
			return false
		}
		rest = after
		if !held[node.Address()] && node.Kind == graphstore.KindResult {
			appended = true
			continue
		}
		if !bytes.HasPrefix(before, entry) {
			return false
		}
		before = before[len(entry):]
	}
	return appended && len(rest) == 0 && len(before) == 0
}

// drainOwedQueryMarks is the block-index tick's pass over what requests left owed: first the fills
// a request lost (fillLostDigests), then the marks (issue_query_marks). It drains up to
// queryMarkBatch marks, oldest first by the tuple sequence that marked each, each issue's fill and
// drain in transactions of their own (drainQueryMark), which names the versions it writes as a
// request's drain does, after the writer of the tuple at the mark's greatest_seq, or
// QueryDrainActor where none stands there. It publishes what each appended once it has committed,
// a fill that committed before its drain failed included, and counts the issues it drained in
// tick.Queries. A mark it cannot drain is logged (drainLogLevel) and left for the next tick. It
// reports whether the batch was full.
func (s *Service) drainOwedQueryMarks(ctx context.Context, tick *BlockIndexTick) (bool, error) {
	if err := s.fillLostDigests(ctx); err != nil {
		return false, err
	}
	rows, err := s.store.Pool.Query(ctx, queryMarksStatement, queryMarkBatch)
	if err != nil {
		return false, fmt.Errorf("list the issues whose queries are owed: %w", err)
	}
	issues, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return false, fmt.Errorf("list the issues whose queries are owed: %w", err)
	}
	for _, issue := range issues {
		appended, drained, err := s.drainQueryMark(ctx, issue)
		for _, event := range appended {
			s.events.Publish(event)
		}
		if err != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			slog.Log(ctx, drainLogLevel(err), "dispatch: block index could not drain the queries an issue owes", "issue", issue, "error", err)
			continue
		}
		if drained {
			tick.Queries++
		}
	}
	return len(issues) == queryMarkBatch, nil
}
