package docs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A backfill run while a pod of the release before this one is live still gives every block its
// parent, and the census still reaches zero.
//
// The backfill states no block's parent: it marks the block's document with a mark of this
// build's own, document_block_versions.store_owed_at (migration 20261116221539), and this
// build's reconciliation states every block's part_of tuple and parent_block_id when it indexes
// the marked document, and clears the mark. The release before this one reads and drains a
// different mark, the null version (its docs.blockIndex reads document_block_versions where
// version is null), and writes the rows, but it knows no store, so it states no part_of tuple.
// It never reads or clears store_owed_at, so a pod of it that drains a null version first -
// during a roll, or after a rollback, which the README names as a time the backfill is safe to
// run - leaves the backfill's mark where it was, and this build's reconciliation still reaches
// the document.
//
// The defect this holds: the backfill used to mark the document by that null version itself.
// The tester drove it on 2026-11-16 at 5fd216378e30: legion-envoy-v30.35.1 and this build
// serving one database, a document written through the previous release, then
// `envoy-dispatch backfill-graph-store` (complete=true, owed=2). The previous release's process
// logged `block index documents=2 ... owed_documents=0` 40 s later, the four blocks kept an
// in_document tuple and no part_of, and `census --store` refused with `the row and its node
// disagree` at each of them minutes later and after every later tick: the document was no
// longer owed, and a second backfill reads no row that names its node, so it marked nothing.
//
// This test stands in for the previous release's pass with the writes of it that matter here.
// An edit or a rebuild-refs of either release first owes the backfilled documents a block index
// (their version to null). The stand-in then drains that null version to the latest version, as
// that pass does when it indexes them, and writes nothing of the store: no tuple, and no
// parent_block_id, a column that release does not have (migration 20261109022248 adds it). Every
// parent is then this build's reconciliation's to write. The backfill's own mark must outlive
// that drain, and this build's reconciliation must then state every parent, as its part_of tuple
// and as the parent_block_id the seed's own indexing computed.
func TestABackfillWhoseMarkThePreviousReleaseDrainedStillReachesEveryParent(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()

	const documents = 3
	for index := range documents {
		slug := fmt.Sprintf("prev%02d", index)
		id := createOwedDocument(t, database, service, "CORE-1", "", slug, nestedSpec(slug), "")
		settle(t, database, id)
	}
	// Rows of every other kind the census reads, as TestTheBlockParentCensusReachesZeroAfterTheBackfill
	// seeds them, so the census can pass only on agreement and never refuses an empty kind here.
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (issue_key, author, body) values
			('CORE-1', '{"kind":"user","id":"Alice"}', 'A remark written before the store existed.'),
			('CORE-1', '{"kind":"session","id":"previous-release-session"}', 'A session''s remark written before the store existed.');
		update issues set route = 'role:previous-release-role' where key = 'CORE-1';
	`); err != nil {
		t.Fatalf("seed the comments and the route: %v", err)
	}
	drainBlockIndex(t, service, "seed")
	before := blockStates(t, database)
	if len(before) == 0 {
		t.Fatal("the seed indexed no block, so this test would prove nothing")
	}
	takeTheStoreOff(t, database)

	report, err := graphcensus.Backfill(ctx, database, io.Discard)
	if err != nil {
		t.Fatalf("backfill the store: %v", err)
	}
	blocks := backfilledKind(t, report, refs.BlockNodeKind)
	if blocks.Owed != documents || !blocks.Complete {
		t.Fatalf("the backfill wrote %#v, want %d documents marked owed and the cutover complete", blocks, documents)
	}

	// An edit or a rebuild-refs of either release owes the backfilled documents a block index
	// (document_block_versions.version to null), the mark the previous release's block index
	// reads and drains: the backfill's own mark leaves the version alone.
	if _, err := database.Pool.Exec(ctx, `
		update document_block_versions set version = null
		where artifact_id in (select artifact_id from document_blocks)`); err != nil {
		t.Fatalf("owe the backfilled documents a block index as an edit or rebuild-refs does: %v", err)
	}
	// The previous release's block index takes the lease first and drains that mark: it writes
	// the document's indexed version and nothing of the store, no tuple and no parent_block_id, a
	// column that release does not have (migration 20261109022248). Every parent is then this
	// build's reconciliation's to write.
	drained, err := database.Pool.Exec(ctx, `
		update document_block_versions b
		set version = (select max(v.number) from artifact_versions v where v.artifact_id = b.artifact_id)
		where b.version is null`)
	if err != nil {
		t.Fatalf("drain the mark as the previous release's block index does: %v", err)
	}
	if drained.RowsAffected() != documents {
		t.Fatalf("the previous release's pass drained %d mark(s), want the %d the backfill set", drained.RowsAffected(), documents)
	}
	// The backfill's own mark is no mark that release reads, so the drain leaves it on every
	// backfilled document for this build's reconciliation.
	var marked int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from document_block_versions
		where store_owed_at is not null and artifact_id in (select artifact_id from document_blocks)`).Scan(&marked); err != nil {
		t.Fatalf("read the backfill's own mark: %v", err)
	}
	if marked != documents {
		t.Errorf("%d backfilled document(s) still hold the backfill's own mark after the previous release drained the null version, want all %d",
			marked, documents)
	}

	// This build's reconciliation then runs as it does on every tick of every pod of it.
	drainBlockIndex(t, service, "after the previous release drained the mark")

	after := blockStates(t, database)
	for address := range after {
		if _, stated := currentBlockParent(t, database, address); !stated {
			t.Errorf("block %s states no parent: the previous release drained its document's mark, and nothing states its part_of now", address)
		}
	}
	// Taking the store off cleared every parent (takeTheStoreOff), so nothing but this build's
	// reconciliation writes parent_block_id, and a row holds the parent the seed's own indexing gave
	// its block only where that reconciliation wrote it. The seed nests its blocks (nestedSpec), so
	// a seed that gave no block a parent leaves this assertion nothing to compare.
	nested := 0
	for address, was := range before {
		if !was.hasParent {
			continue
		}
		nested++
		if state := after[address]; !state.hasParent || state.parent != was.parent {
			t.Errorf("block %s has parent_block_id %q (set %t) after this build's reconciliation, want the %q the seed's own indexing gave it",
				address, state.parent, state.hasParent, was.parent)
		}
	}
	if nested == 0 {
		t.Errorf("the seed gave no block a parent, so no parent this build's reconciliation writes was compared")
	}
	census := takeStoreCensus(t, database)
	for _, kind := range census.Kinds {
		for _, difference := range kind.ReportedRows {
			t.Errorf("the census reads a difference of kind %s at %s: %s", kind.Kind, difference.At, difference.Cause)
		}
	}
	if census.Refused() {
		t.Fatalf("the census refused with %d difference(s) after a backfill whose mark a pod of the previous release drained, want zero",
			census.Differences())
	}
}

// A pass of a lower marking rule that drains a document a build of a higher rule marked leaves
// that mark, with its rule, for the higher rule's build, which then drains the document and states
// the version; and it lists no document a higher rule marked whose stand is at its version.
//
// During a roll or after a rollback a pod of the lower rule may hold the block-index lease. Where
// the document's stand is below its version, the lower rule has work of its own there: its pass
// indexes the version, wins the fence at the version the row stands at, and states the version,
// and it leaves the mark, which the higher rule owes more than this pass states (R13), so the
// pass lists the document no more. Where the stand is at the version, the lower rule has nothing
// to do, and a pass that listed the document would lose the fence and list it again every tick.
func TestALowerRulesDrainLeavesAHigherRulesMark(t *testing.T) {
	ctx := context.Background()
	database, lower := blockIndexService(t)
	higher := markingRuleBuild(t, database, nextBuildCommit, lower.markingRule+1)
	v, w := lower.markingRule, higher.markingRule
	below := createOwedDocument(t, database, lower, "CORE-1", "", "below", nestedSpec("below"), "")
	at := createOwedDocument(t, database, lower, "CORE-1", "", "at", nestedSpec("at"), "")
	settle(t, database, below)
	settle(t, database, at)
	drainBlockIndex(t, lower, "seed")

	// Case i: the release before this one indexes the document again, so its stand is below its
	// version, and a tick of the higher rule marks it from that version's event.
	version := restateAsThePreviousRelease(t, database, lower, below)
	var marked int64
	if err := database.Pool.QueryRow(ctx, markVersionedDocuments, []string{below}, w).Scan(&marked); err != nil || marked != 1 {
		t.Fatalf("the marking of rule %d marked %d document(s) (%v), want the one whose stand is below its version", w, marked, err)
	}
	reconcileBlockIndex(t, lower, "")
	row := blockRowOf(t, database, below)
	if !row.marked || row.rule != w {
		t.Errorf("the drain of a build of marking rule %d cleared a mark of rule %d, want the mark left for its own rule", v, w)
	}
	if row.stand == nil || *row.stand != version {
		t.Errorf("a build of marking rule %d left the stand of %s at %s, below its version %d, under a mark of rule %d, want the version stated",
			v, below, standsText(row.stand), version, w)
	}
	if _, written := tickWrites(t, higher); !written[below] || !blockRowOf(t, database, below).clearedAt(version) {
		t.Errorf("a build of marking rule %d left the version %d of %s unwritten after the lower rule's drain, want it written", w, version, below)
	}

	// Case ii: a backfill of the higher rule marks a document whose stand is at its version.
	if _, err := database.Pool.Exec(ctx, graphcensus.BlockBatchMarkStatement, []string{at}, w); err != nil {
		t.Fatalf("mark %s at marking rule %d: %v", at, w, err)
	}
	row = blockRowOf(t, database, at)
	if !row.marked || row.rule != w || row.version == nil || !row.standsAt(*row.version) {
		t.Fatalf("the higher rule's mark left %s at %s, want it marked by rule %d at its stand: this test proves nothing", at, row, w)
	}
	if listed(t, lower, at) {
		t.Errorf("a build of marking rule %d listed %s as owed, whose stand is at its version %d and whose mark is of rule %d, want it left to the build of rule %d",
			v, at, *row.version, w, w)
	}
}

// A pass of a higher marking rule lists a document a lower rule marked whose stand is at its
// version, indexes it at that version and clears the mark: the mark is of a rule its pass owes
// everything of (R13).
func TestAHigherRulesPassListsAndClearsALowerRulesMark(t *testing.T) {
	ctx := context.Background()
	database, lower := blockIndexService(t)
	higher := markingRuleBuild(t, database, nextBuildCommit, lower.markingRule+1)
	v, w := lower.markingRule, higher.markingRule
	document := createOwedDocument(t, database, lower, "CORE-1", "", "lower", nestedSpec("lower"), "")
	settle(t, database, document)
	drainBlockIndex(t, lower, "seed")
	if _, err := database.Pool.Exec(ctx, graphcensus.BlockBatchMarkStatement, []string{document}, v); err != nil {
		t.Fatalf("mark %s at marking rule %d: %v", document, v, err)
	}
	row := blockRowOf(t, database, document)
	if !row.marked || row.rule != v || row.version == nil || !row.standsAt(*row.version) {
		t.Fatalf("the lower rule's mark left %s at %s, want it marked by rule %d at its stand: this test proves nothing", document, row, v)
	}
	version := *row.version
	if !listed(t, higher, document) {
		t.Errorf("a pass of marking rule %d did not list %s, whose mark is of rule %d, as owed, want it listed", w, document, v)
	}
	reconcileBlockIndex(t, higher, "")
	if blockRowOf(t, database, document).marked {
		t.Errorf("a pass of marking rule %d left %s with a mark of rule %d after it indexed the version %d, want the mark cleared", w, document, v, version)
	}
}

// Every statement that marks a document owed a block index writes the marking rule as R13
// states: run at a lower rule over a standing mark of a higher rule, it keeps the higher rule, a
// drain of the lower rule run next leaves the mark, and the higher rule's build then states the
// version; run where no mark of a higher rule stands, the row takes the statement's own rule, over
// a mark of a lower rule, over a row at the default rule 1 that holds no mark, over a cleared row
// that still holds a higher rule, and, for the two statements that insert the row, where no row
// stands. Each statement is read as its writer runs it, and run over a document it marks: one
// whose store stands below its version.
func TestEveryMarkingStatementKeepsOrRaisesTheRuleAsR13States(t *testing.T) {
	ctx := context.Background()
	lowerRule := refs.BlockIndexMarkingRule
	higherRule := lowerRule + 1
	for _, statement := range []struct {
		name    string
		inserts bool
		mark    func(t *testing.T, database *store.Store, document string, rule int)
	}{
		{"markVersionedDocuments", false, func(t *testing.T, database *store.Store, document string, rule int) {
			var marked int64
			if err := database.Pool.QueryRow(ctx, markVersionedDocuments, []string{document}, rule).Scan(&marked); err != nil {
				t.Fatalf("run markVersionedDocuments: %v", err)
			}
		}},
		{"markStaleDocuments with markStaleBatch", false, func(t *testing.T, database *store.Store, document string, rule int) {
			rows, err := database.Pool.Query(ctx, graphcensus.StaleDocumentsStatement, "00000000-0000-0000-0000-000000000000", 200, rule)
			if err != nil {
				t.Fatalf("run markStaleDocuments' selection: %v", err)
			}
			batch, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				t.Fatalf("run markStaleDocuments' selection: %v", err)
			}
			if _, err := database.Pool.Exec(ctx, graphcensus.StaleBatchMarkStatement, batch, rule); err != nil {
				t.Fatalf("run markStaleBatch: %v", err)
			}
		}},
		{"writeBlockBatch's mark", false, func(t *testing.T, database *store.Store, document string, rule int) {
			if _, err := database.Pool.Exec(ctx, graphcensus.BlockBatchMarkStatement, []string{document}, rule); err != nil {
				t.Fatalf("run writeBlockBatch's mark: %v", err)
			}
		}},
		{"oweDocumentIndexStatement", true, func(t *testing.T, database *store.Store, document string, rule int) {
			if err := oweDocumentIndex(ctx, database.Pool, document, rule); err != nil {
				t.Fatal(err)
			}
		}},
		{"refs.RebuildAll's mark", true, func(t *testing.T, database *store.Store, document string, rule int) {
			if _, err := database.Pool.Exec(ctx, refs.RebuildMarkStatement, rule); err != nil {
				t.Fatalf("run RebuildAll's mark: %v", err)
			}
		}},
	} {
		t.Run(statement.name, func(t *testing.T) {
			database, lower := blockIndexService(t)
			higher := markingRuleBuild(t, database, nextBuildCommit, higherRule)

			document := createOwedDocument(t, database, lower, "CORE-1", "", "kept", nestedSpec("kept"), "")
			settle(t, database, document)
			drainBlockIndex(t, lower, "seed")
			version := restateAsThePreviousRelease(t, database, lower, document)
			setBlockRow(t, database, document, true, higherRule)
			statement.mark(t, database, document, lowerRule)
			if row := blockRowOf(t, database, document); !row.marked || row.rule != higherRule {
				t.Errorf("%s, case i: at marking rule %d over a mark of rule %d, store_owed_rule of %s is %d, want %d kept",
					statement.name, lowerRule, higherRule, document, row.rule, higherRule)
			}
			reconcileBlockIndex(t, lower, "")
			if row := blockRowOf(t, database, document); !row.marked || row.rule != higherRule {
				t.Errorf("%s, case i: the drain of rule %d run next left %s with no mark of rule %d, want the mark left",
					statement.name, lowerRule, document, higherRule)
			}
			if _, written := tickWrites(t, higher); !written[document] || !blockRowOf(t, database, document).clearedAt(version) {
				t.Errorf("%s, case i: a build of rule %d left the version %d of %s unwritten after the drain of rule %d, want it written",
					statement.name, higherRule, version, document, lowerRule)
			}

			for index, owed := range []struct {
				name           string
				rule           int
				marked, absent bool
				held           int
			}{
				{name: "over a mark of a lower rule", rule: higherRule, marked: true, held: lowerRule},
				{name: "over a row at the default rule 1 that holds no mark", rule: higherRule, held: 1},
				{name: "over a cleared row that still holds a higher rule", rule: lowerRule, held: higherRule},
				{name: "where no row stands", rule: higherRule, absent: true},
			} {
				if owed.absent && !statement.inserts {
					continue
				}
				document := markableDocument(t, database, fmt.Sprintf("raised-%d", index))
				if owed.absent {
					if _, err := database.Pool.Exec(ctx, `delete from document_block_versions where artifact_id = $1`, document); err != nil {
						t.Fatalf("take the row of %s away: %v", document, err)
					}
				} else {
					setBlockRow(t, database, document, owed.marked, owed.held)
				}
				statement.mark(t, database, document, owed.rule)
				if row := blockRowOf(t, database, document); !row.marked || row.rule != owed.rule {
					t.Errorf("%s, case ii (%s): at marking rule %d, store_owed_rule of %s is %d, want %d",
						statement.name, owed.name, owed.rule, document, row.rule, owed.rule)
				}
			}
		})
	}
}

// markingStatementDigests is, for each value refs.BlockIndexMarkingRule has held, the digest of the
// statements that decide what a build of that rule marks (markingStatements), as TestGoldenDigest
// (internal/broker/policy/load_test.go) pins its digest. A change to those statements is a change
// of what a pass owes, which raises the rule and records the new digest here beside the old. The
// digest is taken over each statement's words joined by single spaces, so a statement's layout -
// its line breaks and indentation, which the text it splices in from another place gives it -
// moves no digest, and a change of any word does.
var markingStatementDigests = map[int]string{
	1: "127c62eb231428cab90c580dacb6d4ceda4f8137e4ac91b2924e982e7e5357ea",
}

// The marking rule is held to the statements whose results decide which documents a build marks
// owed a block index and which comments its tick writes, so a change to one of them that keeps
// refs.BlockIndexMarkingRule fails here before it merges (R13): a build of the changed statements
// beside a build of the old ones, during a roll or after a rollback, would otherwise read each
// other's cursors, horizons and marks as their own.
func TestTheMarkingRuleNamesTheStatementsItMarksWith(t *testing.T) {
	digest := sha256.New()
	for _, statement := range markingStatements(t) {
		fmt.Fprintf(digest, "%s\x00%s\x00", statement.name, strings.Join(strings.Fields(statement.sql), " "))
	}
	got := hex.EncodeToString(digest.Sum(nil))
	if want := markingStatementDigests[refs.BlockIndexMarkingRule]; got != want {
		t.Fatalf("the marking statements' digest is %s at BlockIndexMarkingRule %d, which records %s: a change to what a pass owes raises the rule",
			got, refs.BlockIndexMarkingRule, want)
	}
}

// namedStatement is one statement of the marking rule and the writer it is named by.
type namedStatement struct{ name, sql string }

// markingStatements is every statement whose result decides what this build marks, each read as
// its writer runs it: the tick's read of the event log (readEventPage), its marking
// (markVersionedDocuments), its read of the comments it writes (refs.CommentsNamingNoNode) and that
// write's statements (refs.CommentWrite.Queue), the backfill's block selection
// (graphcensus.BlockRowsStatement) and its read of a batch's rows again under the locks
// (graphcensus.HeldBlockRowsStatement), the backfill's marks (graphcensus.BlockBatchMarkStatement,
// graphcensus.StaleDocumentsStatement, graphcensus.StaleBatchMarkStatement), the two marks of
// a debt (oweDocumentIndexStatement, refs.RebuildMarkStatement), the condition under which a build
// lists a marked document as owed (refs.OwedMarkListed), which the owed listing, its re-read under
// the owner's lock and the backlog all read, and the revision fence that decides whether a pass
// wins and clears a mark (refs.RevisionFenceStatement). The two refs functions that send
// statements are run against a stand-in that records what they send and answers nothing, so no
// copy of their text is read.
func markingStatements(t *testing.T) []namedStatement {
	t.Helper()
	ctx := context.Background()
	statements := []namedStatement{
		{"docs.readEventPage", readEventPage},
		{"docs.markVersionedDocuments", markVersionedDocuments},
	}
	read := &recordedQueries{}
	if _, err := refs.CommentsNamingNoNode(ctx, read, []string{"00000000-0000-0000-0000-000000000001"}, []int64{1}); !errors.Is(err, errRecorded) {
		t.Fatalf("read refs.CommentsNamingNoNode's statement: %v", err)
	}
	if len(read.sql) != 1 {
		t.Fatalf("refs.CommentsNamingNoNode sent %d statements, want its one read", len(read.sql))
	}
	statements = append(statements, namedStatement{"refs.CommentsNamingNoNode", read.sql[0]})
	author := model.Actor{Kind: "user", ID: "alice"}
	at := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	node, tuples := graphstore.CommentFacts(nil, "00000000-0000-0000-0000-000000000001", author, at, 1, "CORE-1", "CORE-1")
	write := refs.CommentWrite{Nodes: []graphstore.Node{node}, Tuples: tuples}
	if authors, facts, named := graphstore.AuthorFacts(graphstore.KindComment, node.ID, author, at, 1); named {
		write.Nodes = append(write.Nodes, authors...)
		write.Tuples = append(write.Tuples, facts...)
	}
	sent := &recordedBatch{}
	if err := store.RunWrite(ctx, sent, func(b *store.WriteBatch) error {
		write.Queue(b)
		return nil
	}); err != nil {
		t.Fatalf("read refs.CommentWrite.Queue's statements: %v", err)
	}
	if len(sent.sql) == 0 {
		t.Fatalf("refs.CommentWrite.Queue queued no statement")
	}
	for index, sql := range sent.sql {
		statements = append(statements, namedStatement{fmt.Sprintf("refs.CommentWrite.Queue %d", index), sql})
	}
	return append(statements,
		namedStatement{"graphcensus.BlockRowsStatement", graphcensus.BlockRowsStatement},
		namedStatement{"graphcensus.HeldBlockRowsStatement", graphcensus.HeldBlockRowsStatement},
		namedStatement{"graphcensus.BlockBatchMarkStatement", graphcensus.BlockBatchMarkStatement},
		namedStatement{"graphcensus.StaleDocumentsStatement", graphcensus.StaleDocumentsStatement},
		namedStatement{"graphcensus.StaleBatchMarkStatement", graphcensus.StaleBatchMarkStatement},
		namedStatement{"docs.oweDocumentIndexStatement", oweDocumentIndexStatement},
		namedStatement{"refs.RebuildMarkStatement", refs.RebuildMarkStatement},
		namedStatement{"refs.OwedMarkListed", refs.OwedMarkListed("b", "$1::integer")},
		namedStatement{"refs.RevisionFenceStatement", refs.RevisionFenceStatement},
	)
}

// errRecorded is what the stand-ins that record a statement answer in place of running it.
var errRecorded = errors.New("recorded, not run")

// recordedQueries is a reader that records the statement of each read and runs none.
type recordedQueries struct{ sql []string }

func (r *recordedQueries) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	r.sql = append(r.sql, sql)
	return nil, errRecorded
}

func (r *recordedQueries) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	r.sql = append(r.sql, sql)
	return recordedRow{}
}

type recordedRow struct{}

func (recordedRow) Scan(...any) error { return errRecorded }

// recordedBatch is a transaction that records the statements of each batch a write phase sends
// it (store.WriteBatch.Send) and runs none of them. It holds no transaction: Send calls nothing
// of it but SendBatch.
type recordedBatch struct {
	pgx.Tx
	sql []string
}

func (r *recordedBatch) SendBatch(_ context.Context, batch *pgx.Batch) pgx.BatchResults {
	for _, queued := range batch.QueuedQueries {
		r.sql = append(r.sql, queued.SQL)
	}
	return recordedResults{}
}

// recordedResults is the results of a batch recordedBatch ran none of: closing it reads nothing.
type recordedResults struct{ pgx.BatchResults }

func (recordedResults) Close() error { return nil }

// restateAsThePreviousRelease has the stand-in for the release before this one write document's
// text again as its next version and index it, as that release's settlement and index pass do
// (settleVersionAsThePreviousRelease, indexVersionAsThePreviousRelease): its version moves and its
// store_version, which only this build's pass writes, stays below it. That release's write settles
// the row the version's insert owes the re-index queue, so it is settled here too. It answers the
// version.
func restateAsThePreviousRelease(t *testing.T, database *store.Store, service *Service, document string) int {
	t.Helper()
	ctx := context.Background()
	markdown, blocks, err := service.TextWithBlocks(ctx, document)
	if err != nil {
		t.Fatalf("read %s: %v", document, err)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	version, versionAt, event := settleVersionAsThePreviousRelease(t, tx, document, markdown)
	indexVersionAsThePreviousRelease(t, tx, document, version, versionAt, event, markdown, blocks, nil, nil)
	if _, err := tx.Exec(ctx, `delete from refs_grammar_owed where kind = 'artifact' and id = $1::uuid`, document); err != nil {
		t.Fatalf("settle the re-index queue's row of %s as the previous release's version write does: %v", document, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return version
}

// markableDocument is a document of issue CORE-1, slug, with one version and its
// document_block_versions row, which the statements a test runs over it mark; nothing reads its
// text.
func markableDocument(t *testing.T, database *store.Store, slug string) string {
	t.Helper()
	ctx := context.Background()
	var document string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('CORE-1', 'CORE', $1, $1 || '.md', 'doc', false, '{"kind":"user","id":"alice"}')
		returning id::text
	`, slug).Scan(&document); err != nil {
		t.Fatalf("create document %s: %v", slug, err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1, 1, 'A document.', '[{"kind":"user","id":"alice"}]')
	`, document); err != nil {
		t.Fatalf("write version 1 of %s: %v", slug, err)
	}
	return document
}

// setBlockRow sets document's document_block_versions row to stand at a version above its store,
// which every marking statement marks, holding a mark where marked is true, and the rule rule
// either way.
func setBlockRow(t *testing.T, database *store.Store, document string, marked bool, rule int) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		update document_block_versions set version = greatest(coalesce(version, 2), 2), store_version = coalesce(store_version, 1),
			store_owed_at = case when $2::boolean then now() end, store_owed_rule = $3
		where artifact_id = $1
	`, document, marked, rule); err != nil {
		t.Fatalf("set the block index row of %s: %v", document, err)
	}
}

// tickWrites runs one tick of service and answers it with the documents its index pass reached
// the commit of (Service.afterBlockIndexWrite).
func tickWrites(t *testing.T, service *Service) (BlockIndexTick, map[string]bool) {
	t.Helper()
	written := map[string]bool{}
	service.afterBlockIndexWrite = func(document string) error {
		written[document] = true
		return nil
	}
	defer func() { service.afterBlockIndexWrite = nil }()
	return reconcileBlockIndex(t, service, ""), written
}

// listed is whether service's owed listing, from the start, lists document.
func listed(t *testing.T, service *Service, document string) bool {
	t.Helper()
	owed, err := service.owedDocuments(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	return slices.ContainsFunc(owed, func(listed owedDocument) bool { return listed.id == document })
}

// clearedAt is whether the row stands at version on both columns and holds no mark: what a pass
// that won the fence at that version and cleared its mark leaves.
func (row blockRowState) clearedAt(version int) bool {
	return !row.marked && row.standsAt(version)
}
