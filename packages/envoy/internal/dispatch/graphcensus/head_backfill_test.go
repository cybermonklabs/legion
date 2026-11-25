package graphcensus

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// headBackfillIssues are the issues TestTheHeadProjectionBackfillReadsZero writes as the release
// before stores them.
var headBackfillIssues = []string{"HBF-1", "HBF-2"}

// SeedHeadProject, SeedReleaseBeforeIssue and ReleaseBeforeHeadingText are seedHeadProject,
// seedReleaseBeforeIssue and releaseBeforeHeadingText for this package's external test
// (head_tick_test.go).
var (
	SeedHeadProject        = seedHeadProject
	SeedReleaseBeforeIssue = seedReleaseBeforeIssue
)

const ReleaseBeforeHeadingText = releaseBeforeHeadingText

// releaseBeforeHeadingText is a write of the release before by plain SQL, with no head row and no
// dispatch.head_rows setting: a text value of the block $1, the heading block of an issue
// seedReleaseBeforeIssue writes (`<key>/spec-md#h`), whose words are $2.
const releaseBeforeHeadingText = `
	insert into graph_tuples (from_kind, from_id, kind, to_kind, to_id, at, who, source_event)
	values ('block', $1, 'text', 'value', $2, clock_timestamp(), '{"kind":"user","id":"alice"}', 0)`

// seedHeadProject writes the project HBF, the project of every issue seedReleaseBeforeIssue writes.
func seedHeadProject(t *testing.T, ctx context.Context, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(ctx, `insert into projects (key, name, default_assignee) values ('HBF', 'Head backfill', 'alice')`); err != nil {
		t.Fatalf("seed the project: %v", err)
	}
}

// seedReleaseBeforeIssue writes issue key as a store of the release before holds it, which has no
// head projection: its row and its node, a document of it with two blocks, one under the other, a
// comment on it, and a result under the document's check block, each node with the tuples that
// place it, every value and relation stated through a write phase of the store's writer with no
// head write (graphstore.QueueWrite; graphstore.Write would queue one), and no row of
// issue_head_nodes or issue_head_marks.
func seedReleaseBeforeIssue(t *testing.T, ctx context.Context, database *store.Store, number int, key string) {
	t.Helper()
	alice := model.Actor{Kind: "user", ID: "alice"}
	at := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC).Add(time.Duration(number) * time.Hour)
	document := key + "/spec-md"
	heading, check := document+"#h", document+"#c1"
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			insert into issues (key, project_key, number, title, created_by, rank, node_kind, node_address)
			values ($1, 'HBF', $2, 'An issue of the release before', '{"kind":"user","id":"alice"}', 'U', 'issue', $1)
		`, key, number); err != nil {
			return err
		}
		var comment string
		if err := tx.QueryRow(ctx, `insert into comments (issue_key, author, body) values ($1, $2, 'a word') returning id::text`,
			key, `{"kind":"user","id":"alice"}`).Scan(&comment); err != nil {
			return err
		}
		block := func(id string) graphstore.Node {
			return graphstore.Node{Kind: refs.BlockNodeKind, ID: id, Supertag: refs.BlockSupertag, Visibility: graphstore.VisibilityProject,
				State: graphstore.StateLive, Project: "HBF", MadeAt: at, MadeBy: alice}
		}
		result := fmt.Sprintf("11111111-1111-4111-8111-%012d", number)
		nodes := []graphstore.Node{
			graphstore.IssueNode(key, "HBF", alice, at),
			graphstore.ArtifactNode(document, "HBF", "", alice, at),
			block(heading), block(check),
			{Kind: graphstore.KindComment, ID: comment, Supertag: graphstore.KindComment, Visibility: graphstore.VisibilityProject,
				State: graphstore.StateLive, Project: "HBF", MadeAt: at, MadeBy: alice},
			{Kind: graphstore.KindResult, ID: result, Supertag: graphstore.SupertagResult, Visibility: graphstore.VisibilityProject,
				State: graphstore.StateLive, Project: "HBF", MadeAt: at, MadeBy: alice},
		}
		tuple := func(fromKind, fromID, kind, toKind, toID string) graphstore.Tuple {
			return graphstore.Tuple{FromKind: fromKind, FromID: fromID, Kind: kind, ToKind: toKind, ToID: toID, At: at, Who: alice, Asserted: true}
		}
		tuples := []graphstore.Tuple{
			graphstore.ArtifactPartOf(document, key, alice, at),
			tuple("block", heading, graphstore.TuplePartOf, graphstore.KindArtifact, document),
			tuple("block", heading, graphstore.AttributeText, graphstore.KindValue, "# Heading"),
			tuple("block", heading, "rank", graphstore.KindValue, "a"),
			tuple("block", check, graphstore.TuplePartOf, "block", heading),
			tuple("block", check, graphstore.AttributeText, graphstore.KindValue, ":::check{#c1}\nA check\n:::"),
			tuple("block", check, "rank", graphstore.KindValue, "a"),
			tuple(graphstore.KindComment, comment, graphstore.TupleAnchoredTo, graphstore.KindArtifact, document),
			tuple(graphstore.KindResult, result, graphstore.TuplePartOf, "block", check),
			tuple(graphstore.KindResult, result, graphstore.TupleResultOf, "block", check),
			tuple(graphstore.KindResult, result, graphstore.AttributeVerdict, graphstore.KindValue, "pass"),
		}
		return store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
			graphstore.QueueWrite(b, nodes, tuples)
			return nil
		})
	}); err != nil {
		t.Fatalf("write %s as the release before stores it: %v", key, err)
	}
}

// requireHeadsMarkedAndEqual fails, naming each issue of keys no mark names after the backfill
// (`the backfill left <key> unmarked`), and each difference the census kind issue_head_node reads
// (`after the backfill the census kind issue_head_node reads <n> difference(s) at issue <key>`).
func requireHeadsMarkedAndEqual(t *testing.T, ctx context.Context, database *store.Store, keys []string) {
	t.Helper()
	for _, key := range keys {
		var marked, rows int
		if err := database.Pool.QueryRow(ctx, `
			select (select count(*) from issue_head_marks where issue_key = $1),
			       (select count(*) from issue_head_nodes where issue_key = $1)`, key).Scan(&marked, &rows); err != nil {
			t.Fatalf("read the mark of %s: %v", key, err)
		}
		if marked == 0 {
			t.Errorf("the backfill left %s unmarked", key)
			continue
		}
		t.Logf("%s holds its mark and %d head rows", key, rows)
	}
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		kind, err := CensusOfHeads(ctx, tx)
		if err != nil {
			return err
		}
		t.Logf("the census kind %s reads %d rows against %d the store gives, %d and %d differences",
			kind.Kind, kind.Rows, kind.Nodes, kind.RowDifferences, kind.NodeDifferences)
		for _, found := range append(kind.ReportedRows, kind.ReportedNodes...) {
			t.Errorf("after the backfill the census kind issue_head_node reads %d difference(s) at %s (%s)",
				kind.RowDifferences+kind.NodeDifferences, found.At, found.Cause)
		}
		if kind.Rows == 0 {
			t.Errorf("the census kind issue_head_node read no row: the backfill wrote none")
		}
		return nil
	}); err != nil {
		t.Fatalf("take the census kind issue_head_node: %v", err)
	}
}

// TestTheHeadProjectionBackfillReadsZero writes issues as a store of the release before holds them
// - blocks, a conversation and a result, and no row of issue_head_nodes, which that release has no
// table for - and runs the backfill (Backfill), whose pass backfillIssueHeadNodes writes one row for
// each node of each issue's tree and marks each issue in the transaction that holds its owner lock.
// Every issue then holds its mark, else the test prints `the backfill left <key> unmarked`, and the
// census kind issue_head_node reads zero differences, else it prints `after the backfill the census
// kind issue_head_node reads <n> difference(s) at issue <key>`. It then takes one issue's mark away
// and appends a tuple of its tree, and the next run of the pass writes that issue's rows again and
// marks it again, held to the same two lines.
func TestTheHeadProjectionBackfillReadsZero(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedHeadProject(t, ctx, database)
	for index, key := range headBackfillIssues {
		seedReleaseBeforeIssue(t, ctx, database, index+1, key)
	}
	var stored int
	if err := database.Pool.QueryRow(ctx, `select count(*) from issue_head_nodes`).Scan(&stored); err != nil {
		t.Fatalf("read the head rows: %v", err)
	}
	if stored != 0 {
		t.Fatalf("the store of the release before holds %d head rows, want none", stored)
	}
	report, err := Backfill(ctx, database, io.Discard)
	if err != nil {
		t.Fatalf("run the backfill: %v", err)
	}
	for _, kind := range report.Kinds {
		if kind.Kind == headKind {
			t.Logf("the backfill's pass %s filled %d issues, complete %t", kind.Kind, kind.Rows, kind.Complete)
		}
	}
	requireHeadsMarkedAndEqual(t, ctx, database, headBackfillIssues)

	// A write of the release before takes the first issue's mark away: its trigger, with no
	// setting, deletes the mark of the issue whose tree holds the block it appends a value of.
	if _, err := database.Pool.Exec(ctx, releaseBeforeHeadingText, headBackfillIssues[0]+"/spec-md#h", "# Heading, again"); err != nil {
		t.Fatalf("append a tuple of %s as the release before: %v", headBackfillIssues[0], err)
	}
	var marked int
	if err := database.Pool.QueryRow(ctx, `select count(*) from issue_head_marks where issue_key = $1`, headBackfillIssues[0]).Scan(&marked); err != nil {
		t.Fatalf("read the mark of %s: %v", headBackfillIssues[0], err)
	}
	if marked != 0 {
		t.Errorf("the mark of %s stands after a write of the release before", headBackfillIssues[0])
	}
	if _, err := Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("run the backfill again: %v", err)
	}
	requireHeadsMarkedAndEqual(t, ctx, database, headBackfillIssues)
}

// TestAWriteBehindTheStoresWritersTakesTheMarkAway holds the head projection's triggers to every
// write of a row the head rows project that keeps no head row, as the release before writes them
// and as a statement by hand would: on an issue the fill has filled and marked, by plain SQL with no
// dispatch.head_rows setting, a delete of one of its tuples, an insert of a document_blocks row of
// one of its blocks, an update of that row's type, a delete of that row, and a delete of one of its
// node rows each take the issue's mark away (graph_tuples', document_blocks' and graph_nodes'
// triggers), and the fill before each marks it again. Else the test prints `the mark of <key>
// stands after <the write>`.
func TestAWriteBehindTheStoresWritersTakesTheMarkAway(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedHeadProject(t, ctx, database)
	const key = "HBF-1"
	seedReleaseBeforeIssue(t, ctx, database, 1, key)
	heading := key + "/spec-md#h"
	var document string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		values ($1, 'HBF', 'spec-md', 'spec.md', 'doc', '{"kind":"user","id":"alice"}')
		returning id::text
	`, key).Scan(&document); err != nil {
		t.Fatalf("write the row of %s's document: %v", key, err)
	}
	writes := []struct {
		what, sql string
		args      []any
	}{
		{"a delete of a tuple of its tree", `delete from graph_tuples where from_kind = 'block' and from_id = $1 and kind = 'text'`, []any{heading}},
		{"an insert of a document_blocks row of one of its blocks", `
			insert into document_blocks (artifact_id, block_id, node_id, type, text_sha256, excerpt, changed_version, changed_at)
			values ($1::uuid, 'h', $2, 'heading', '', 'Heading.', 1, now())`, []any{document, heading}},
		{"an update of that row's type", `update document_blocks set type = 'paragraph' where artifact_id = $1::uuid and block_id = 'h'`, []any{document}},
		{"a delete of that row", `delete from document_blocks where artifact_id = $1::uuid and block_id = 'h'`, []any{document}},
		{"a delete of a node row of its tree", `delete from graph_nodes where kind = 'block' and id = $1`, []any{heading}},
	}
	for _, write := range writes {
		if _, _, err := FillIssueHeads(ctx, database, HeadFillBatch); err != nil {
			t.Fatalf("fill %s before %s: %v", key, write.what, err)
		}
		if _, marked := headMark(t, ctx, database, key); !marked {
			t.Fatalf("%s holds no mark after the fill before %s", key, write.what)
		}
		if tag, err := database.Pool.Exec(ctx, write.sql, write.args...); err != nil || tag.RowsAffected() == 0 {
			t.Fatalf("%s of %s: %d rows, %v", write.what, key, tag.RowsAffected(), err)
		}
		if _, marked := headMark(t, ctx, database, key); marked {
			t.Errorf("the mark of %s stands after %s", key, write.what)
		}
	}
}

// headMark is the time the mark of the issue key was filled at, and whether one stands.
func headMark(t *testing.T, ctx context.Context, database *store.Store, key string) (time.Time, bool) {
	t.Helper()
	var filled time.Time
	switch err := database.Pool.QueryRow(ctx, `select filled_at from issue_head_marks where issue_key = $1`, key).Scan(&filled); {
	case errors.Is(err, pgx.ErrNoRows):
		return time.Time{}, false
	case err != nil:
		t.Fatalf("read the mark of %s: %v", key, err)
	}
	return filled, true
}

// TestTheFillHoldsTheHeadFillLockAgainstAWriteOfTheReleaseBefore runs the real fill of an issue's
// head rows beside a write of the release before on the same issue, by plain SQL with no head row
// and no setting: first FillIssueHeads, the block index tick's, then backfillIssueHeadNodes, the
// boot's. Each fill is held inside its transaction right after it takes the issue's head fill lock
// (headFillLocked) while the write, started then, appends a text value to a block of the issue's
// tree; then the fill goes on and commits, and the write commits once it completes. The write's
// trigger waits for the head fill lock, so its delete reads the mark the fill committed and takes it
// away, and no mark stands over the row the write changed: else the test prints `the mark of <key>
// stands after a write of the release before`, and it names a write that did not wait. A fill after
// both marks the issue again, and the census kind issue_head_node reads zero.
func TestTheFillHoldsTheHeadFillLockAgainstAWriteOfTheReleaseBefore(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedHeadProject(t, ctx, database)
	const key = "HBF-1"
	seedReleaseBeforeIssue(t, ctx, database, 1, key)
	fills := []struct {
		name string
		fill func() error
	}{
		{"FillIssueHeads", func() error { _, _, err := FillIssueHeads(ctx, database, HeadFillBatch); return err }},
		{"backfillIssueHeadNodes", func() error { _, err := backfillIssueHeadNodes(ctx, database); return err }},
	}
	for _, fill := range fills {
		holdFillBesideAWrite(t, ctx, database, key, fill.name, fill.fill)
	}
}

// holdFillBesideAWrite takes the mark of key away, runs fill until it holds the issue's head fill
// lock (headFillLocked), starts a write of the release before on the issue's heading block while the
// fill is held there, then lets the fill go and commits the write once it completes, and holds what
// follows: the write waited, and no mark names the issue. It then fills the issue again
// (requireHeadsMarkedAndEqual).
func holdFillBesideAWrite(t *testing.T, ctx context.Context, database *store.Store, key, name string, fill func() error) {
	t.Helper()
	heading := key + "/spec-md#h"
	if _, err := database.Pool.Exec(ctx, `delete from issue_head_marks where issue_key = $1`, key); err != nil {
		t.Fatalf("take the mark of %s away: %v", key, err)
	}
	held, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	headFillLocked = func(locked string) {
		if locked == key {
			once.Do(func() {
				close(held)
				<-release
			})
		}
	}
	defer func() { headFillLocked = nil }()
	filled := make(chan error, 1)
	go func() { filled <- fill() }()
	select {
	case <-held:
	case err := <-filled:
		t.Fatalf("the fill %s of %s ended (%v) before it held the issue's head fill lock", name, key, err)
	case <-time.After(30 * time.Second):
		t.Fatalf("the fill %s of %s did not reach the issue's head fill lock within 30s", name, key)
	}
	write, err := database.Pool.Begin(ctx)
	if err != nil {
		close(release)
		t.Fatalf("begin the write of the release before: %v", err)
	}
	defer func() { _ = write.Rollback(context.Background()) }()
	var pid int32
	if err := write.QueryRow(ctx, `select pg_backend_pid()`).Scan(&pid); err != nil {
		close(release)
		t.Fatalf("read the write's backend pid: %v", err)
	}
	written := make(chan error, 1)
	go func() {
		_, err := write.Exec(ctx, releaseBeforeHeadingText, heading, "# Heading, written beside the fill "+name)
		written <- err
	}()
	waited, writeErr := awaitLockWait(t, ctx, database, pid, written)
	close(release)
	if err := awaitHeadEnd(t, "the fill "+name+" of "+key, filled); err != nil {
		t.Fatalf("the fill %s of %s beside a write of the release before: %v", name, key, err)
	}
	if waited {
		writeErr = awaitHeadEnd(t, "the write of the release before on "+key, written)
	}
	if writeErr != nil {
		t.Fatalf("the write of the release before beside the fill %s of %s: %v", name, key, writeErr)
	}
	if err := write.Commit(ctx); err != nil {
		t.Fatalf("commit the write of the release before beside the fill %s of %s: %v", name, key, err)
	}
	if !waited {
		t.Errorf("the write of the release before on %s did not wait for the fill %s, which held the issue's head fill lock", key, name)
	}
	var marked int
	if err := database.Pool.QueryRow(ctx, `select count(*) from issue_head_marks where issue_key = $1`, key).Scan(&marked); err != nil {
		t.Fatalf("read the mark of %s: %v", key, err)
	}
	if marked != 0 {
		t.Errorf("the mark of %s stands after a write of the release before beside the fill %s, over the row of %s the write changed", key, name, heading)
	}
	t.Logf("the fill %s of %s beside a write of the release before: the write waited %t, and %d mark names the issue after both", name, key, waited, marked)
	if _, _, err := FillIssueHeads(ctx, database, HeadFillBatch); err != nil {
		t.Fatalf("fill %s again: %v", key, err)
	}
	requireHeadsMarkedAndEqual(t, ctx, database, []string{key})
}

// awaitLockWait waits until the backend pid waits for a lock, or the work done reports has ended,
// and answers whether it waited and, where the work ended first, its error.
func awaitLockWait(t *testing.T, ctx context.Context, database *store.Store, pid int32, done <-chan error) (bool, error) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		select {
		case err := <-done:
			return false, err
		default:
		}
		var waits bool
		if err := database.Pool.QueryRow(ctx, `select exists (select 1 from pg_locks where pid = $1 and not granted)`, pid).Scan(&waits); err != nil {
			t.Fatalf("read the locks backend %d waits for: %v", pid, err)
		}
		if waits {
			return true, nil
		}
		if time.Now().After(deadline) {
			t.Fatalf("backend %d neither waits for a lock nor ended its write after 30s", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// awaitHeadEnd is the error the work done reports, failing after 30s without one.
func awaitHeadEnd(t *testing.T, what string, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second):
		t.Fatalf("%s did not end within 30s", what)
		return nil
	}
}
