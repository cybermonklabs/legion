package graphcensus

import (
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// markedSnapshot is what a second backfill must leave as it found: how many nodes and tuples the
// store holds and the highest tuple sequence, how many rows of each projected table carry their
// node, and the row version (xmin) of every row the backfill could write, keyed by table and row.
// A statement that writes a row gives it a new xmin even where it writes the values it read, so a
// marker written again over itself is told from one left alone.
type markedSnapshot struct {
	nodes, tuples, highest int64
	marked                 map[string]int64
	versions               map[string]string
}

func takeMarkedSnapshot(t *testing.T, database *store.Store) markedSnapshot {
	t.Helper()
	ctx := context.Background()
	snapshot := markedSnapshot{marked: map[string]int64{}, versions: map[string]string{}}
	if err := database.Pool.QueryRow(ctx, `
		select (select count(*) from graph_nodes), (select count(*) from graph_tuples),
		       (select coalesce(max(seq), 0) from graph_tuples)
	`).Scan(&snapshot.nodes, &snapshot.tuples, &snapshot.highest); err != nil {
		t.Fatalf("count the store's rows: %v", err)
	}
	rows, err := database.Pool.Query(ctx, `
		select 'document_blocks', d.artifact_id::text || ' ' || d.block_id, d.node_kind is not null, d.xmin::text
		from document_blocks d
		union all
		select 'block_refs', r.from_node || ' ' || r.kind || ' ' || r.to_kind || ' ' || r.to_id, r.node_kind is not null, r.xmin::text
		from block_refs r
		union all
		select 'comments', c.id::text, c.node_kind is not null, c.xmin::text
		from comments c
		union all
		select 'document_block_versions', v.artifact_id::text, false, v.xmin::text
		from document_block_versions v
		union all
		select 'graph_nodes', n.kind || ' ' || n.id, false, n.xmin::text
		from graph_nodes n
	`)
	if err != nil {
		t.Fatalf("read the rows the backfill writes: %v", err)
	}
	type versioned struct {
		table, key, version string
		marked              bool
	}
	read, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (versioned, error) {
		var v versioned
		err := row.Scan(&v.table, &v.key, &v.marked, &v.version)
		return v, err
	})
	if err != nil {
		t.Fatalf("scan the rows the backfill writes: %v", err)
	}
	for _, v := range read {
		if v.marked {
			snapshot.marked[v.table]++
		}
		snapshot.versions[v.table+" "+v.key] = v.version
	}
	return snapshot
}

// D3's contract for the backfill: each pass walks only the rows its marker does not mark, names
// their nodes in the transaction that writes their tuples, and reads no marked row. So the run can
// be taken again at any time, as the child that moves issues, their documents, a block's text
// and rank, results and components takes it before it proves the old arms may
// go, and it does only the rows still owed.
//
// The corpus is what a build before the store left: a document of three blocks, a mention and a
// pointer out of two of them, a comment the document owns, one anchored on it, a reply, a comment a
// session wrote, and an issue routed to a role. The first run marks every row of it. The second
// runs over tables every row of which is marked, and must write no node, no tuple and no marker:
// the store's counts and highest sequence are as the first run left them, every row of the three
// projected tables, of document_block_versions and of graph_nodes keeps its row version, and the
// report names no row, node or tuple for any kind.
func TestASecondBackfillWritesNothingOverMarkedRows(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	document := seedPreStoreDocument(t, database, "first", "second", "third")
	if _, err := database.Pool.Exec(ctx, `
		insert into block_refs (artifact_id, block_id, from_node, to_kind, to_id, kind, created_version)
		values ($1::uuid, 'second', $2, 'issue', 'CORE-1', 'mentions', 1),
		       ($1::uuid, 'third', $3, 'block', $4, 'serves', 1)
	`, document.artifactID, document.refKey+"#second", document.refKey+"#third", document.refKey+"#first"); err != nil {
		t.Fatalf("write the blocks' references: %v", err)
	}
	var owned string
	if err := database.Pool.QueryRow(ctx, `
		insert into comments (artifact_id, author, body)
		values ($1::uuid, '{"kind":"user","id":"alice"}', 'owned by the spec')
		returning id::text
	`, document.artifactID).Scan(&owned); err != nil {
		t.Fatalf("write the owned comment: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (issue_key, author, body, anchor)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'anchored on the spec', jsonb_build_object('artifact_id', $1::text));
	`, document.artifactID); err != nil {
		t.Fatalf("write the anchored comment: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into comments (artifact_id, author, body, reply_to)
		values ($1::uuid, '{"kind":"session","id":"ses_backfill_twice"}', 'a reply by a session', $2::uuid)
	`, document.artifactID, owned); err != nil {
		t.Fatalf("write the reply: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `update issues set route = 'role:backfill-twice' where key = 'CORE-1'`); err != nil {
		t.Fatalf("route the issue to a role: %v", err)
	}

	first, err := Backfill(ctx, database, io.Discard)
	if err != nil {
		t.Fatalf("backfill the store: %v", err)
	}
	for _, kind := range []string{refs.BlockNodeKind, "block_ref", graphstore.KindComment, graphstore.KindRole} {
		if done := reportedKind(t, first, kind); done.Rows == 0 || !done.Complete {
			t.Fatalf("the first run reports %#v for %s, want rows written and none remaining: the corpus holds no row of it to run over again", done, kind)
		}
	}
	before := takeMarkedSnapshot(t, database)
	for _, table := range []string{"document_blocks", "block_refs", "comments"} {
		if before.marked[table] == 0 {
			t.Fatalf("the first run marked no row of %s, so a second run over it would prove nothing: %v", table, before.marked)
		}
	}

	second, err := Backfill(ctx, database, io.Discard)
	if err != nil {
		t.Fatalf("backfill the store again: %v", err)
	}
	for _, done := range second.Kinds {
		if done.Rows != 0 || done.Nodes != 0 || done.Tuples != 0 || done.Owed != 0 || !done.Complete {
			t.Errorf("the second run reports %#v for %s, want nothing written and nothing remaining", done, done.Kind)
		}
	}
	after := takeMarkedSnapshot(t, database)
	if after.nodes != before.nodes || after.tuples != before.tuples || after.highest != before.highest {
		t.Errorf("the second run left %d nodes, %d tuples and the highest sequence %d, want the %d, %d and %d the first left",
			after.nodes, after.tuples, after.highest, before.nodes, before.tuples, before.highest)
	}
	if !maps.Equal(after.marked, before.marked) {
		t.Errorf("the second run left %v rows carrying their node, want the %v the first left", after.marked, before.marked)
	}
	for _, row := range slices.Sorted(maps.Keys(before.versions)) {
		if version, held := after.versions[row]; !held || version != before.versions[row] {
			t.Errorf("the second run wrote %s (row version %s, then %s): it reads and writes no marked row", row, before.versions[row], version)
		}
	}
	for _, row := range slices.Sorted(maps.Keys(after.versions)) {
		if _, held := before.versions[row]; !held {
			t.Errorf("the second run wrote the row %s, which the first run left without", row)
		}
	}
}

// The pointer pass's guard is the tuple arm's own test of the block a pointer is a link out of:
// the store holds that block's node in a state other than gone, read off graph_nodes alone and
// from no document_blocks row, since the backfill reads no marked row and a block's row is marked
// once the block pass has run.
//
// Two pairs are owed after the block pass. The first is out of a block whose node the store holds
// gone while its row still names it: a guard that read the row would stamp the pointer, which
// neither arm then answers, since the old arm skips a marked row and the tuple arm a gone node.
// The second is out of a block whose row a build of the previous release wrote again with no node
// while the node stands: the guard stamps it, and a guard that read the row would leave it owed.
// The pass refuses the first, counts it skipped and appends nothing for it, and stamps the second
// with its tuple.
func TestThePointerGuardRefusesAPairWhoseBlockNodeIsGone(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	document := seedPreStoreDocument(t, database, "target", "dropped", "rewritten")
	if _, err := Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill the blocks before their references exist: %v", err)
	}
	dropped, rewritten, target := document.refKey+"#dropped", document.refKey+"#rewritten", document.refKey+"#target"
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the drop: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := graphstore.Write(ctx, tx, []graphstore.Node{{
		Kind: refs.BlockNodeKind, ID: dropped, Supertag: refs.BlockSupertag,
		Visibility: graphstore.VisibilityProject, State: graphstore.StateGone,
		MadeAt: time.Now(), MadeBy: backfillActor, SourceEvent: graphstore.NoEvent,
	}}, nil); err != nil {
		t.Fatalf("state the dropped block gone: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the drop: %v", err)
	}
	if tag, err := database.Pool.Exec(ctx, `
		update document_blocks set node_kind = null, node_address = null
		where artifact_id = $1::uuid and block_id = 'rewritten'
	`, document.artifactID); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("write the rewritten block's row again with no node: %d rows, %v", tag.RowsAffected(), err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into block_refs (artifact_id, block_id, from_node, to_kind, to_id, kind, created_version)
		values ($1::uuid, 'dropped', $2, 'block', $4, 'serves', 1),
		       ($1::uuid, 'rewritten', $3, 'block', $4, 'serves', 1)
	`, document.artifactID, dropped, rewritten, target); err != nil {
		t.Fatalf("write the two pointers: %v", err)
	}
	var droppedRow, droppedNode string
	if err := database.Pool.QueryRow(ctx, `
		select d.node_kind || ' ' || d.node_address, n.state
		from document_blocks d join graph_nodes n on n.kind = d.node_kind and n.id = d.node_address
		where d.artifact_id = $1::uuid and d.block_id = 'dropped'
	`, document.artifactID).Scan(&droppedRow, &droppedNode); err != nil || droppedNode != graphstore.StateGone {
		t.Fatalf("the dropped block's row names %q, whose node is %q (%v); want a row naming a gone node", droppedRow, droppedNode, err)
	}
	_, tuplesBefore := storeSize(t, database)

	done, err := backfillBlockRefs(ctx, database)
	if err != nil {
		t.Fatalf("run the pointer pass: %v", err)
	}
	_, tuplesAfter := storeSize(t, database)
	stamped := func(from string) bool {
		var marked bool
		if err := database.Pool.QueryRow(ctx, `
			select node_kind is not null from block_refs where from_node = $1 and kind = 'serves'
		`, from).Scan(&marked); err != nil {
			t.Fatalf("read the pointer out of %s: %v", from, err)
		}
		return marked
	}
	if stamped(dropped) {
		t.Errorf("the pointer pass stamped the pointer out of %s, whose node the store holds gone: neither arm of graph_edges answers it now", dropped)
	}
	if current, err := graphstore.CurrentOut(ctx, database.Pool, refs.BlockNodeKind, dropped, []string{"serves"}); err != nil || len(current) != 0 {
		t.Errorf("the store holds %v out of the gone block %s (%v), want no pointer", current, dropped, err)
	}
	if !stamped(rewritten) {
		t.Errorf("the pointer pass left the pointer out of %s owed, whose node stands though its row names none: the guard reads the store, not the row", rewritten)
	}
	if done.Rows != 1 || done.Tuples != 1 || done.Skipped != 1 || tuplesAfter-tuplesBefore != 1 {
		t.Errorf("the pointer pass reports %#v and appended %d tuples, want the one pointer stamped, its one tuple and the one refused counted skipped",
			done, tuplesAfter-tuplesBefore)
	}
}

// R13: the backfill marks every document whose blocks it gave nodes owed a block index, and a
// run stopped anywhere leaves each document of its committed batches owed, since a second run
// reads no row a committed batch named and so could never mark those documents again.
//
// The corpus is two documents an older build indexed, so neither is owed when the run starts:
// CORE-1's of exactly one batch of blocks and CORE-2's of two, whose node addresses sort after
// every one of CORE-1's, so the first batch is CORE-1's alone and the second CORE-2's. A
// transaction holds CORE-2's issue row, which the second batch takes first, and once the run
// queues behind it the run's backend is terminated, as a killed process leaves it. The first
// batch has committed: its blocks name their nodes, and CORE-1's document must be owed. A second
// run then finishes the second batch and leaves both documents owed.
func TestABackfillKilledAfterOneBatchLeavesThatBatchsDocumentsOwed(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('CORE', 'Core', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank) values
			('CORE-1', 'CORE', 1, 'One', '{"kind":"user","id":"alice"}', 'U'),
			('CORE-2', 'CORE', 2, 'Two', '{"kind":"user","id":"alice"}', 'V');
	`); err != nil {
		t.Fatalf("seed issues CORE-1 and CORE-2: %v", err)
	}
	documents := map[string]string{}
	for _, issue := range []struct {
		key    string
		blocks int
	}{{"CORE-1", backfillBatch}, {"CORE-2", 2}} {
		var id, refKey string
		if err := database.Pool.QueryRow(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
			values ($1, 'CORE', 'spec', 'spec.md', 'doc', false, '{"kind":"user","id":"alice"}')
			returning id::text, ref_key
		`, issue.key).Scan(&id, &refKey); err != nil {
			t.Fatalf("create the document of %s: %v", issue.key, err)
		}
		documents[issue.key] = id
		if _, err := database.Pool.Exec(ctx, `
			insert into artifact_versions (artifact_id, number, markdown, authors)
			values ($1, 1, 'Written before the store existed.', '[{"kind":"user","id":"alice"}]')
		`, id); err != nil {
			t.Fatalf("write version 1 of the document of %s: %v", issue.key, err)
		}
		if _, err := database.Pool.Exec(ctx, `
			insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
			select $1::uuid, 'b' || lpad(n::text, 3, '0'), $2 || '#b' || lpad(n::text, 3, '0'), 'paragraph', '', 'A block.', 1, now()
			from generate_series(0, $3 - 1) n
		`, id, refKey, issue.blocks); err != nil {
			t.Fatalf("write the blocks of the document of %s: %v", issue.key, err)
		}
		// The older build indexed the version, which settles the mark the version's own row left.
		if tag, err := database.Pool.Exec(ctx, `
			update document_block_versions set version = 1 where artifact_id = $1::uuid
		`, id); err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("settle the block index of the document of %s: %d rows, %v", issue.key, tag.RowsAffected(), err)
		}
	}
	// The backfill's mark is its own (store_owed_at), which the release before this one neither
	// reads nor drains, and not the null version every release reads.
	owed := func(issue string) bool {
		t.Helper()
		var owes bool
		if err := database.Pool.QueryRow(ctx, `
			select store_owed_at is not null from document_block_versions where artifact_id = $1::uuid
		`, documents[issue]).Scan(&owes); err != nil {
			t.Fatalf("read whether the document of %s holds the backfill's mark: %v", issue, err)
		}
		return owes
	}
	named := func(issue string) int {
		t.Helper()
		var count int
		if err := database.Pool.QueryRow(ctx, `
			select count(*) from document_blocks where artifact_id = $1::uuid and node_kind is not null
		`, documents[issue]).Scan(&count); err != nil {
			t.Fatalf("count the blocks of %s that name their node: %v", issue, err)
		}
		return count
	}

	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the holder: %v", err)
	}
	defer holder.Rollback(ctx)
	if err := store.LockOwner(ctx, holder, store.IssueOwner("CORE-2")); err != nil {
		t.Fatalf("hold issue CORE-2's owner lock: %v", err)
	}
	pid, answered := startBackfill(t, database)
	if early, done := storetest.WaitForQueuedBehind(t, holder, pid, answered); done {
		t.Fatalf("the backfill answered (report %#v, err %v) without waiting on its second batch's owner lock", early.report, early.err)
	}
	var terminated bool
	if err := database.Pool.QueryRow(ctx, `select pg_terminate_backend($1)`, int64(pid)).Scan(&terminated); err != nil || !terminated {
		t.Fatalf("terminate the backfill's backend %d: %t, %v", pid, terminated, err)
	}
	select {
	case answer := <-answered:
		if answer.err == nil {
			t.Fatalf("the backfill whose backend was terminated answered %#v and no error", answer.report)
		}
		t.Logf("the killed run answered: %v", answer.err)
	case <-time.After(time.Minute):
		t.Fatal("the backfill did not answer within a minute of its backend's termination")
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("release issue CORE-2's row: %v", err)
	}
	if first, second := named("CORE-1"), named("CORE-2"); first != backfillBatch || second != 0 {
		t.Fatalf("after the killed run %d of CORE-1's blocks and %d of CORE-2's name their node, want all %d and none: the run was not stopped after its first batch",
			first, second, backfillBatch)
	}
	if !owed("CORE-1") {
		t.Errorf("the killed run committed the batch of CORE-1's blocks and left its document unmarked: no later run marks it, since none reads those rows again")
	}
	if owed("CORE-2") {
		t.Errorf("the killed run marked CORE-2's document, whose batch never committed")
	}

	report, err := Backfill(ctx, database, io.Discard)
	if err != nil {
		t.Fatalf("run the backfill again: %v", err)
	}
	if blocks := blockKind(t, report); blocks.Rows != 2 || blocks.Owed != 1 || !blocks.Complete {
		t.Errorf("the second run reports %#v for blocks, want CORE-2's two rows, its one document owed and the cutover complete", blocks)
	}
	for _, issue := range []string{"CORE-1", "CORE-2"} {
		if !owed(issue) {
			t.Errorf("after the second run the document of %s is not owed a block index", issue)
		}
	}
}

// The backfill marks owed, on every run, each document no pass of this build's rule has stated at
// its version: its document_block_versions row holds no mark of this rule or a higher one, and its
// stated_version is not its version or its stated_rule is below refs.BlockIndexMarkingRule, a null
// on either side counted as not equal (refs.UnstatedBlockIndexSQL). store_version says nothing of
// it, since a build of an earlier rule writes store_version for every version it indexes and
// states none of this rule's facts of its blocks.
//
// Five documents, each row in one state: one no pass of this rule stated, whose store_version
// stands at its version as a build of an earlier rule leaves it; one stated behind its version;
// one stated at its version by a pass of the rule before this one; one stated at its version by a
// pass of this rule; and one already marked, by a mark of this rule (store_owed_rule). The marking
// (markStaleDocuments) marks exactly the first three, the marked one keeps its mark's time, and a
// second run marks nothing new. The block pass then names the node of a block of the marked
// document, which marks that document again (writeBlockBatch), and the mark keeps its time.
func TestTheBackfillMarksEveryDocumentNoPassOfItsRuleStated(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('CORE', 'Core', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank) values
			('CORE-1', 'CORE', 1, 'One', '{"kind":"user","id":"alice"}', 'U');
	`); err != nil {
		t.Fatalf("seed issue CORE-1: %v", err)
	}
	markedAt := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	states := []struct {
		name                      string
		version, storeVersion     int
		statedVersion, statedRule *int
		owedAt                    *time.Time
		marks                     bool
	}{
		{name: "no pass of this rule stated", version: 3, storeVersion: 3, marks: true},
		{name: "stated behind its version", version: 3, storeVersion: 2, statedVersion: new(2), statedRule: new(refs.BlockIndexMarkingRule), marks: true},
		{name: "stated at its version by the rule before", version: 3, storeVersion: 3, statedVersion: new(3), statedRule: new(refs.BlockIndexMarkingRule - 1), marks: true},
		{name: "stated at its version by this rule", version: 3, storeVersion: 3, statedVersion: new(3), statedRule: new(refs.BlockIndexMarkingRule)},
		{name: "already marked", version: 3, storeVersion: 2, owedAt: &markedAt},
	}
	documents := make([]string, len(states))
	named := map[string]string{}
	var marked string
	for index, state := range states {
		var id, refKey string
		if err := database.Pool.QueryRow(ctx, `
			insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
			values ('CORE-1', 'CORE', $1, $1 || '.md', 'doc', false, '{"kind":"user","id":"alice"}')
			returning id::text, ref_key
		`, fmt.Sprintf("state-%d", index)).Scan(&id, &refKey); err != nil {
			t.Fatalf("create the document %s: %v", state.name, err)
		}
		if _, err := database.Pool.Exec(ctx, `
			insert into document_block_versions (artifact_id, version, store_version, stated_version, stated_rule, store_owed_at, store_owed_rule)
			values ($1::uuid, $2, $3, $4, $5, $6, $7)
		`, id, state.version, state.storeVersion, state.statedVersion, state.statedRule, state.owedAt, refs.BlockIndexMarkingRule); err != nil {
			t.Fatalf("write the row of the document %s: %v", state.name, err)
		}
		documents[index], named[id] = id, state.name
		if state.owedAt != nil {
			marked = refKey
		}
	}
	// marks reads each document's mark, by its state's name.
	marks := func() map[string]*time.Time {
		t.Helper()
		held := map[string]*time.Time{}
		for _, id := range documents {
			var owedAt *time.Time
			if err := database.Pool.QueryRow(ctx, `
				select store_owed_at from document_block_versions where artifact_id = $1::uuid
			`, id).Scan(&owedAt); err != nil {
				t.Fatalf("read the mark of the document %s: %v", named[id], err)
			}
			held[named[id]] = owedAt
		}
		return held
	}
	names := func(ids []string) []string {
		held := make([]string, len(ids))
		for index, id := range ids {
			held[index] = named[id]
		}
		slices.Sort(held)
		return held
	}

	first, err := markStaleDocuments(ctx, database, refs.BlockIndexMarkingRule)
	if err != nil {
		t.Fatalf("run the marking: %v", err)
	}
	var want []string
	for _, state := range states {
		if state.marks {
			want = append(want, state.name)
		}
	}
	slices.Sort(want)
	if got := names(first); !slices.Equal(got, want) {
		t.Errorf("the marking marked %q, want exactly %q", got, want)
	}
	afterFirst := marks()
	for _, state := range states {
		owedAt := afterFirst[state.name]
		switch {
		case state.marks && owedAt == nil:
			t.Errorf("after the marking the document %s holds no mark, want it marked: no pass of this rule has stated its version", state.name)
		case state.owedAt != nil && (owedAt == nil || !owedAt.Equal(*state.owedAt)):
			t.Errorf("after the marking the document %s is marked at %v, want the mark it held, %s", state.name, owedAt, state.owedAt)
		case !state.marks && state.owedAt == nil && owedAt != nil:
			t.Errorf("after the marking the document %s is marked at %s, want no mark: a pass of this rule stated it at its version", state.name, owedAt)
		}
	}

	second, err := markStaleDocuments(ctx, database, refs.BlockIndexMarkingRule)
	if err != nil {
		t.Fatalf("run the marking a second time: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("the second run of the marking marked %q, want nothing new", names(second))
	}
	for name, owedAt := range marks() {
		if was := afterFirst[name]; (was == nil) != (owedAt == nil) || was != nil && !was.Equal(*owedAt) {
			t.Errorf("the second run of the marking moved the mark of the document %s from %v to %v", name, was, owedAt)
		}
	}

	// A block of the marked document whose row names no node: the block pass names it and marks
	// the document, which keeps the mark it holds.
	if _, err := database.Pool.Exec(ctx, `
		insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
		select a.id, 'b1', a.ref_key || '#b1', 'paragraph', '', 'A block.', 3, now() from artifacts a where a.ref_key = $1
	`, marked); err != nil {
		t.Fatalf("write a block of the marked document: %v", err)
	}
	blocks, err := backfillBlocks(ctx, database, refs.BlockIndexMarkingRule)
	if err != nil {
		t.Fatalf("run the block pass: %v", err)
	}
	if blocks.Rows != 1 || blocks.Owed != 1 {
		t.Errorf("the block pass reports %#v, want the one block written and its one document marked", blocks)
	}
	if owedAt := marks()["already marked"]; owedAt == nil || !owedAt.Equal(markedAt) {
		t.Errorf("after the block pass named a block of the marked document its mark is at %v, want the mark it held, %s", owedAt, markedAt)
	}
}

// A backfill of a lower marking rule run after a backfill of a higher rule records its own rule's
// horizon and leaves the higher rule's as that run recorded it. Were one row shared by every rule,
// the older build's backfill would move it above the newer rule's, and the newer rule's first
// tick would pass over every event between the two, which the older backfill marked by its own
// rule and not by the newer one (R13).
func TestABackfillOfALowerMarkingRuleLeavesAHigherRulesHorizon(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	lower, higher := refs.BlockIndexMarkingRule, refs.BlockIndexMarkingRule+1
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('CORE', 'Core', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('CORE-1', 'CORE', 1, 'One', '{"kind":"user","id":"alice"}', 'U');
	`); err != nil {
		t.Fatalf("seed project CORE: %v", err)
	}
	appendHorizonEvent(t, database)
	if _, err := backfillAt(ctx, database, io.Discard, higher); err != nil {
		t.Fatalf("backfill at marking rule %d: %v", higher, err)
	}
	want := horizonOfRule(t, database, higher)
	appended := appendHorizonEvent(t, database)
	if _, err := backfillAt(ctx, database, io.Discard, lower); err != nil {
		t.Fatalf("backfill at marking rule %d: %v", lower, err)
	}
	if got := horizonOfRule(t, database, higher); got != want {
		t.Errorf("after a backfill of marking rule %d the horizon of rule %d is %d, want the %d the backfill of rule %d recorded",
			lower, higher, got, want, higher)
	}
	if own := horizonOfRule(t, database, lower); own < appended {
		t.Errorf("the backfill of marking rule %d recorded the horizon %d, below the event %d written before it began", lower, own, appended)
	}
}

// appendHorizonEvent appends one event of issue CORE-1, its seq taken off the issue's own counter
// as events.Append takes it, and answers its id.
func appendHorizonEvent(t *testing.T, database *store.Store) int64 {
	t.Helper()
	var id int64
	if err := database.Pool.QueryRow(context.Background(), `
		with bumped as (update issues set last_seq = last_seq + 1 where key = 'CORE-1' returning last_seq)
		insert into events (issue_key, seq, type, actor, payload, notify)
		select 'CORE-1', bumped.last_seq, 'issue.updated', '{"kind":"user","id":"alice"}', '{}', false from bumped
		returning id
	`).Scan(&id); err != nil {
		t.Fatalf("append an event of CORE-1: %v", err)
	}
	return id
}

// horizonOfRule reads the horizon the newest backfill of the marking rule rule recorded, which has
// to be there.
func horizonOfRule(t *testing.T, database *store.Store, rule int) int64 {
	t.Helper()
	var horizon int64
	if err := database.Pool.QueryRow(context.Background(), `
		select event_id from graph_backfill_horizon where marking_rule = $1
	`, rule).Scan(&horizon); err != nil {
		t.Fatalf("read the horizon of marking rule %d: %v", rule, err)
	}
	return horizon
}
