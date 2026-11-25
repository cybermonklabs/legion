package refs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// blockStand is the row of document_block_versions that says what a document's blocks stand at,
// with the latest version any release indexed (document_indexed_versions): held says the row is
// there, version is its version (nil while the document is owed an index), storeVersion the
// version whose blocks the store states (migration 20261116221539, which no other release names),
// owed this build's own mark that the document is owed such a pass (store_owed_at), owedRule the
// marking rule that set that mark (store_owed_rule, BlockIndexMarkingRule), read only where owed,
// and latest the newest version of document_indexed_versions, nil for a document no release
// indexed.
type blockStand struct {
	held          bool
	version       *int
	storeVersion  *int
	owed          bool
	owedRule      int
	latestIndexed *int
}

// admits is the revision fence (queueStand's `where`) over the row as it stands, for a pass of
// marking rule rule: the row is not there, it stands at no version or an older one, or it stands
// at this one under a mark, which makes this pass the one that states that version's blocks in
// the store. At this version a mark of rule or a lower rule admits the pass, since it is the
// pass's own to drain, and a mark of a higher rule admits it only where the store last stated the
// document below this version, which is work of the pass's own there. A row at this version that
// holds no mark admits no pass, whatever rule the row holds.
func (s blockStand) admits(version, rule int) bool {
	return !s.held || s.version == nil || *s.version < version ||
		*s.version == version && s.owed && (s.owedRule <= rule || s.storeVersion == nil || *s.storeVersion < version)
}

// stale says the store's statement of the document's blocks is another release's to correct: the
// store last stated them at a version other than the latest one any release indexed, so the rows
// a pass reads are that release's and no longer say what the store holds. A document no release
// has indexed holds nil on both sides and is not stale.
func (s blockStand) stale() bool {
	if s.storeVersion == nil || s.latestIndexed == nil {
		return s.storeVersion != nil || s.latestIndexed != nil
	}
	return *s.storeVersion != *s.latestIndexed
}

// indexedAt is the row once a pass of marking rule rule at version has won the fence: standing at
// that version on both columns, version among those indexed, and its mark cleared where the mark
// was of rule or a lower rule. A mark of a higher rule stays, with its rule, for that rule's pass,
// as the fence's own statement leaves it (queueStand).
func (s blockStand) indexedAt(version, rule int) blockStand {
	latest := version
	if s.latestIndexed != nil && *s.latestIndexed > latest {
		latest = *s.latestIndexed
	}
	stand := blockStand{held: true, version: &version, storeVersion: &version, latestIndexed: &latest}
	if s.owed && s.owedRule > rule {
		stand.owed, stand.owedRule = true, s.owedRule
	}
	return stand
}

// readBlockStand reads the row the revision fence is decided on, and the latest version any
// release indexed (blockStand). The read is the write's own, in its read phase, under the
// document's owner lock, which every writer of a document's blocks takes before it reads them, so
// no other pass writes a revision of the row between this read and the fence's statement in the
// write phase. Four writers of the row take no owner lock and may write it in between, and none
// of them writes a revision: docs' oweDocumentIndex, the re-index of a scope that calls it (docs'
// reindexScope) and RebuildAll set version to null beside this build's mark, store_owed_at with
// its rule in store_owed_rule; and the tick's marking of the documents its events name (docs'
// markVersionedDocuments) sets that mark alone, on a row whose store_version is not its version.
// So the plan may decide the fence on this read (PlanBlocks): a null version admits every pass,
// and a mark bears only on a pass at the version the row already stands at (admits). The fence's
// statement decides even that against the row as it stands when it runs (queueStand), so a pass
// the row then refuses fails there and commits nothing, and a mark of a higher rule written in
// between stays.
func readBlockStand(ctx context.Context, q Queryer, artifactID string) (blockStand, error) {
	var stand blockStand
	if err := q.QueryRow(ctx, `
		select v.artifact_id is not null, v.version, v.store_version, v.store_owed_at is not null,
		       coalesce(v.store_owed_rule, 1),
		       (select max(i.version) from document_indexed_versions i where i.artifact_id = d.id)
		from (select $1::uuid as id) d
		left join document_block_versions v on v.artifact_id = d.id
	`, artifactID).Scan(&stand.held, &stand.version, &stand.storeVersion, &stand.owed, &stand.owedRule, &stand.latestIndexed); err != nil {
		return blockStand{}, fmt.Errorf("read the version the blocks of %s stand at: %w", artifactID, err)
	}
	return stand, nil
}

// queueStand queues the revision fence: it settles the mark migration 0122's trigger left on the
// document when this version's row was inserted, and records the version the document's blocks
// now stand at. The mark only ever moves forward: two passes can be indexing one document, and
// the slower one, reading version 9 while the other already wrote 10, would otherwise set the row
// back to 9 and claim 10's work as its own. The plan decided the fence in its read phase, under
// the document's owner lock, which every writer of a document's blocks takes before it reads them
// (blockStand.admits), so a pass reaches this statement only once it has won; the `where` is that
// same rule, so the row cannot move backward whatever wrote it meanwhile. A second pass of this
// release at the same version loses too, which is what keeps a re-index from stating a parent
// twice, since no code may update or delete a tuple row. It is the pass's first write, so a pass
// that waits for the row waits at that one row rather than at a projection row further in.
//
// The statement, not only the owner lock, holds R23's guarantee that a pass whose row did not
// move appends no tuple, writes no projection row and commits nothing. It reads the row back in
// the same statement (`returning 1`): where its `where` refuses the pass, because some writer
// moved the row after the pass's read phase decided the fence, it answers no row, and its
// callback turns that into an error naming the document and the version, which fails the batch.
// The statements queued after it have run by then, and the caller's transaction, which that
// error abandons, rolls them back with it. The owner lock decides the fence in the read phase of
// every pass a writer runs today, and the four writers that take none write nothing that turns
// it (readBlockStand), so the error is for a writer between the two that ever does.
//
// The row also carries two columns no other release names (migration 20261116221539), which the
// winning pass writes beside version: store_version, the version whose blocks the store now
// states, which it sets to this version; and store_owed_at, this build's own mark that the
// document is owed such a pass, which it clears where the mark's rule (store_owed_rule, migration
// 20261120122600) is the pass's own or a lower one, since this pass states every block's node,
// part_of and parent_block_id (PlanBlocks, planBlockParents). The backfill sets that mark, and so
// does every place this build leaves a version's blocks to a later pass, beside the null version
// every release reads (docs' oweDocumentIndex, refs.RebuildAll). A pass at the version the row
// already stands at wins only where the document holds that mark: the release before this one
// may have indexed that version since and stated nothing in the store, so this pass is the one
// that states its blocks there. That is the re-index the reconciliation owes a marked document
// (docs' owedDocuments), and the first pass at that version that clears the mark makes a second
// one lose as before.
//
// A mark of a higher marking rule than the pass's (BlockIndexMarkingRule) is that rule's to drain:
// a build of that rule owes the document more than this pass states. At the version the row
// stands at, such a mark lets the pass win only where the store last stated the document below
// that version (store_version null or below it), which is work of the pass's own there, and the
// pass that wins under it states the version, leaves the mark with its rule, and so stops listing
// the document (docs' owedDocuments), while the higher rule's pass still lists it by its mark and
// wins at that version. A row at the version that holds no mark lets no pass win, whatever rule
// it holds: store_owed_rule keeps the rule of the last mark, or its default of 1, after the mark
// is cleared. The clear is decided here, against the row as it stands when this statement runs,
// and not by the read phase: four writers of the row take no owner lock (readBlockStand), and a
// mark of a higher rule one of them writes between the pass's read and this statement stays.
//
// indexOwedDocument's own early return is a short cut, not this fence: it spares the work of a
// pass whose version is already settled, and it reads before the write rather than deciding it.
//
// The row this writes is also what owes the document its windows: windows_owed_at marks it owed
// them here, in the transaction that writes the blocks those windows will cover, as the item path
// marks its item (queueRecordIndexed). The windows of these blocks are not written here. The
// window arm reads this marker through its partial index (DocumentsOwedWindows, migration
// 20261108070000), and only the statement that writes the windows clears it
// (WriteDocumentWindows). The marker keeps the time it already carries, so a document owed since
// an earlier write stays owed from then, and a crash after this statement leaves the document
// owed its windows rather than silently windowless. A pass that loses the fence marks nothing,
// because the pass that won it marked the document already.
//
// A version holding no block is marked like any other, and the arm lists it: an issue created
// with no spec carries a spec.md whose markdown is empty, and a document whose last block an edit
// removed still holds the windows of the text it had. Writing a document's windows is what
// deletes the window rows its blocks no longer stand for (writeWindows' `dropped`), so leaving
// such a version unmarked here would leave those rows standing for text the document no longer
// holds; the arm finds it by the version recorded here rather than by a row of document_blocks it
// has none of (documentsOwedWindowsQuery).
//
// What a reader meets for it: a document's windows, and so its hits in meaning search, appear one
// reconciliation tick after the write rather than within it, as a comment's do
// (ItemIndexing.Queue). The write holds the document's owner for its own rows alone: this window
// write was 9.7% of an hour's database time over 31,241 calls at a 114.3 ms mean on the load-test
// deployment, and the writers queued behind it were waiting on that row.
func (b BlockIndexing) queueStand(batch *store.WriteBatch) {
	batch.Queue(fmt.Sprintf("record the blocks of %s indexed at version %d", b.artifactID, b.version),
		RevisionFenceStatement, b.artifactID, b.version, b.rule).QueryRow(func(row pgx.Row) error {
		// The one column says the row was written; nothing reads its value.
		if err := row.Scan(nil); !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		return fmt.Errorf("the revision fence refused the block index of %s at version %d: its document_block_versions row moved after the pass read it, and the pass commits nothing",
			b.artifactID, b.version)
	})
}

// RevisionFenceStatement is the revision fence's statement (queueStand), which binds the document
// as $1, the version the pass indexes as $2 and the pass's marking rule as $3. Its arm at the
// version the row already stands at is OwedMarkListed over the row: that arm holds the row's
// version equal to excluded.version, so the condition the owed listing reads (docs'
// owedDocuments) decides there whether the pass wins, and a change to that rule lands in the
// fence too. It is one of the statements that decide what a build of a marking rule owes, and it
// is named so the test that holds the rule to them reads it (docs'
// TestTheMarkingRuleNamesTheStatementsItMarksWith).
var RevisionFenceStatement = `
		insert into document_block_versions (artifact_id, version, store_version, windows_owed_at) values ($1, $2, $2, now())
		on conflict (artifact_id) do update set version = excluded.version, store_version = excluded.store_version,
			store_owed_at = case when document_block_versions.store_owed_rule <= $3 then null
				else document_block_versions.store_owed_at end,
			windows_owed_at = coalesce(document_block_versions.windows_owed_at, excluded.windows_owed_at)
		where document_block_versions.version is null or document_block_versions.version < excluded.version
		   or document_block_versions.version = excluded.version and ` + OwedMarkListed("document_block_versions", "$3") + `
		returning 1
	`

// BlockIndexMarkingRule is the version of the marking rule this build applies: what a block-index
// pass and the tick owe, which is which documents a build marks owed a block index
// (store_owed_at) and which comments its tick writes. A change of what a pass owes raises it, and
// TestTheMarkingRuleNamesTheStatementsItMarksWith holds it to the statements that decide it. A
// mark records the rule of the build that set it (store_owed_rule), and so do the block-index
// tick's cursor rows and the backfill's horizons (marking_rule, migration 20261120122600): a build
// starts reading the event log where a build or a backfill of its own rule left off and never
// where one of another rule did, since that build marked what its own rule owes and not what this
// one owes, and a pass of one rule clears no mark of a higher rule. Rule 1 is the rule's first
// value, and a build of rule 1 already honours the marks of every later rule. Rule 2 is rule 1
// with the scope of every node: the tick's comment write and the backfill's block and comment
// reads carry the project or the parties of each node they write (CommentsNamingNoNode,
// graphcensus.BlockRowsStatement, graphstore's node write), so a build of rule 2 reads none of
// rule 1's cursors or horizons, and a node a build of rule 1 wrote with no project is the
// backfill's of rule 2 to fill (graphcensus.Backfill). It is declared here, where docs and
// graphcensus both read it: docs' own tests import graphcensus, so graphcensus cannot import docs.
const BlockIndexMarkingRule = 2

// OwedMarkListed is the condition under which a pass of the marking rule rule, a SQL expression,
// lists as owed the document whose document_block_versions row row holds a mark of this build's
// (store_owed_at): a mark of rule or a lower rule (store_owed_rule), which is the pass's own to
// drain, and a mark of a higher rule only where the store last stated the document below its
// version (store_version null or below version), which is work of the pass's own there. A mark of
// a higher rule on a document whose stand is at its version is that rule's alone: a pass of a
// lower rule that listed it would index it, lose the fence's equal-version arm (queueStand) and
// list it again on every pass, and would count it in a backlog that only grows. The owed listing
// (docs' owedDocuments), its re-read under the owner's lock (docs' indexOwedDocument), both
// readers of the backlog (OwedBlockIndexDebts, the count docs' ReconcileBlockIndex logs) and the
// revision fence at the version the row stands at (RevisionFenceStatement) read this one
// condition, so a build counts as its backlog exactly the documents it lists, and a pass wins the
// fence at that version exactly where a build of its rule lists the document.
func OwedMarkListed(row, rule string) string {
	return row + `.store_owed_at is not null and (` + row + `.store_owed_rule <= ` + rule +
		` or ` + row + `.store_version is null or ` + row + `.store_version < ` + row + `.version)`
}

// OwedRuleKept is the rule a statement that marks a document owed a block index at the marking
// rule rule, a SQL expression, writes into store_owed_rule of the document_block_versions row row:
// where a mark stands (store_owed_at), the higher of its rule and rule, so a mark of a higher rule
// stays that rule's to drain; where none stands, rule, whatever rule a mark cleared before left
// on the row, since the rule is read only where a mark stands. Every statement that keeps or
// raises the rule of a mark standing on the row reads this one text: docs'
// oweDocumentIndexStatement, RebuildMarkStatement and graphcensus's BlockBatchMarkStatement.
//
// row names the row as the statement reads it, or is empty where the statement names its columns
// unqualified.
func OwedRuleKept(row, rule string) string {
	return `case when ` + owedColumn(row, "store_owed_at") + ` is null then ` + rule +
		` else greatest(` + owedColumn(row, "store_owed_rule") + `, ` + rule + `) end`
}

// OwedMarkAbsent is the condition under which a statement that marks a document owed a block
// index at the marking rule rule, a SQL expression, reads the document_block_versions row row as
// holding no mark: the row holds none (store_owed_at null), or holds a mark of a lower rule, which
// the statement raises to rule and whose time it keeps. A mark of rule or a higher rule is left as
// it is, and so is its rule. docs' markVersionedDocuments, in its selection and its update alike,
// and graphcensus's StaleDocumentsStatement and StaleBatchMarkStatement read this one condition.
// row is as OwedRuleKept reads it.
func OwedMarkAbsent(row, rule string) string {
	return `(` + owedColumn(row, "store_owed_at") + ` is null or ` + owedColumn(row, "store_owed_rule") + ` < ` + rule + `)`
}

// owedColumn names column of the document_block_versions row row, or the column alone where row
// is empty, as the statement reading it names its columns.
func owedColumn(row, column string) string {
	if row == "" {
		return column
	}
	return row + "." + column
}

// OwedBlockIndexDebts is one row per document a build of the marking rule $1 owes a block index -
// its version null, or this build's own mark on it that such a build lists (store_owed_at,
// blockStand.owed, OwedMarkListed) - each dated by the write that owed it: the created_at of the
// latest markdown version the document holds, which is the version a tick would index, or the
// document's own created_at where it holds no markdown version at all.
//
// Nothing dates a null version - document_block_versions records no time it was set back to null
// (migration 0121) - so the date is derived, and this build's mark is dated the same way, which
// reads older than the mark, the safe direction for a backlog. The two readers of the backlog
// derive it from this one text: the tick's own logged figure (docs.ReconcileBlockIndex) and the
// one `envoy-dispatch census --store --sizing` prints for somebody holding the database and not
// the log (graphcensus's blockIndexBacklogQuery). Each wraps it in its own aggregate and binds
// its build's marking rule as $1.
var OwedBlockIndexDebts = owedBlockIndexDebtsFrom + `
		where b.version is null or ` + OwedMarkListed("b", "$1::integer")

// OwedBlockIndexDebtsBeforeTheMark is OwedBlockIndexDebts over a database migration
// 20261120122600 has not reached yet, which holds no store_owed_rule column: only a null version
// is read as owed there. `census --store --sizing` takes its readings on such a database, before
// this branch's migrations run (R4), where migration 20261116221539 has not added store_owed_at
// either and the backfill has marked nothing, and reads this one there; the tick only ever runs
// on a migrated database and reads the full text. It binds nothing.
const OwedBlockIndexDebtsBeforeTheMark = owedBlockIndexDebtsFrom + `
		where b.version is null`

// owedBlockIndexDebtsFrom is the select both owed-debt texts share, so the two date a debt alike.
const owedBlockIndexDebtsFrom = `
		select coalesce(
		         (select v.created_at from artifact_versions v
		          where v.artifact_id = b.artifact_id and v.markdown is not null
		          order by v.number desc limit 1),
		         a.created_at) as owed_at
		from document_block_versions b
		left join artifacts a on a.id = b.artifact_id`

// OwedTimeText writes a time the block-index backlog is dated by as both of its readers print
// it - the tick's log line (docs.ReconcileBlockIndex) and the sizing's backlog lines
// (graphcensus) - so the two say one time in one spelling: UTC to the nanosecond, so two runs over
// one unchanged database print one byte sequence, and `none` where there is no such time, which a
// zero value would otherwise print as a date in the year 1.
func OwedTimeText(at time.Time) string {
	if at.IsZero() {
		return "none"
	}
	return at.UTC().Format(time.RFC3339Nano)
}
