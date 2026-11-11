package docs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// fenceWait is how long a step of this test waits for another backend to queue behind a row it
// holds, and for a pass it started on a connection of its own to answer.
const fenceWait = 30 * time.Second

// highestTuple is the last sequence the tuple log holds: the mark a step's own appends are read
// back after, and the one that says a step appended nothing.
func highestTuple(t *testing.T, database *store.Store) int64 {
	t.Helper()
	var highest int64
	if err := database.Pool.QueryRow(context.Background(),
		`select coalesce(max(seq), 0) from graph_tuples`).Scan(&highest); err != nil {
		t.Fatalf("read the end of the tuple log: %v", err)
	}
	return highest
}

// tuplesSince counts what the log gained after seq out of the blocks of one document, whose
// addresses all start with the document's ref_key: a tick writes more than one document, so a
// count of the whole log would answer for passes this step is not watching.
func tuplesSince(t *testing.T, database *store.Store, artifactID string, seq int64) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from graph_tuples t
		join artifacts a on a.id = $1::uuid
		where t.seq > $2 and t.from_kind = $3 and t.from_id like a.ref_key || '#%'
	`, artifactID, seq, refs.BlockNodeKind).Scan(&count); err != nil {
		t.Fatalf("count the tuples a step appended for %s: %v", artifactID, err)
	}
	return count
}

// parentsNow is the far end the store asserts under part_of for each block of the document, by
// block id, read as a current relation is read (graphstore.CurrentOut).
func parentsNow(t *testing.T, database *store.Store, artifactID string) map[string]string {
	t.Helper()
	ctx := context.Background()
	rows, err := database.Pool.Query(ctx,
		`select block_id, node_id from document_blocks where artifact_id = $1 order by block_id`, artifactID)
	if err != nil {
		t.Fatalf("read the blocks of %s: %v", artifactID, err)
	}
	defer rows.Close()
	addresses := map[string]string{}
	for rows.Next() {
		var id, address string
		if err := rows.Scan(&id, &address); err != nil {
			t.Fatalf("scan a block of %s: %v", artifactID, err)
		}
		addresses[id] = address
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the blocks of %s: %v", artifactID, err)
	}
	parents := map[string]string{}
	for id, address := range addresses {
		held, err := graphstore.CurrentOut(ctx, database.Pool, refs.BlockNodeKind, address, []string{graphstore.TuplePartOf})
		if err != nil {
			t.Fatalf("read the parent of %s: %v", id, err)
		}
		if len(held) != 1 {
			t.Fatalf("block %s holds %d current part_of tuples, want exactly one", id, len(held))
		}
		parents[id] = held[0].ToKind + "/" + held[0].ToID
	}
	return parents
}

// sameParents compares two readings of a document's parents.
func sameParents(got, want map[string]string) bool {
	if len(got) != len(want) {
		return false
	}
	for block, parent := range want {
		if got[block] != parent {
			return false
		}
	}
	return true
}

// flattenedPass is the blocks an indexing pass of the document's stored version would hand the
// index if every block of it sat at the document's top level: one byte of the markdown each, so
// no block's range holds another's. It is what the stale pass below would assert, and what the
// fence must refuse.
func flattenedPass(t *testing.T, database *store.Store, artifactID string) (string, []model.ArtifactBlock) {
	t.Helper()
	ctx := context.Background()
	var markdown string
	if err := database.Pool.QueryRow(ctx, `
		select markdown from artifact_versions where artifact_id = $1 and markdown is not null
		order by number desc limit 1
	`, artifactID).Scan(&markdown); err != nil {
		t.Fatalf("read the stored version of %s: %v", artifactID, err)
	}
	rows, err := database.Pool.Query(ctx,
		`select block_id from document_blocks where artifact_id = $1 order by block_id`, artifactID)
	if err != nil {
		t.Fatalf("read the blocks of %s: %v", artifactID, err)
	}
	defer rows.Close()
	var blocks []model.ArtifactBlock
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan a block of %s: %v", artifactID, err)
		}
		at := len(blocks)
		blocks = append(blocks, model.ArtifactBlock{ID: id, Type: "paragraph", From: at, To: at + 1})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the blocks of %s: %v", artifactID, err)
	}
	if len(blocks) < 2 || len(markdown) < len(blocks) {
		t.Fatalf("the fixture document holds %d blocks in %d bytes, want a document of several blocks", len(blocks), len(markdown))
	}
	return markdown, blocks
}

// writeVersionThroughTheRealPath writes the document's next version the way a request writes
// one: NamedVersion in a joined transaction, the version's own event after it, and what the
// block index stated appended under that event, as api's commitArtifactVersionEvent does.
func writeVersionThroughTheRealPath(t *testing.T, service *Service, artifactID, summary string) VersionResult {
	t.Helper()
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the version write: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	result, err := service.NamedVersion(joined, artifactID, summary, model.Actor{Kind: "user", ID: "alice"})
	if err != nil {
		t.Fatalf("write a version through the version write path: %v", err)
	}
	if !result.Wrote {
		t.Fatalf("the version write recorded nothing; the live document must differ from the latest version")
	}
	payload, err := json.Marshal(map[string]any{
		"artifact_id": artifactID,
		"version":     map[string]any{"number": result.Version.Number},
	})
	if err != nil {
		t.Fatalf("encode the version event's payload: %v", err)
	}
	var event int64
	// The sequence comes off the issue's own counter, as events.Append takes it, so the
	// settlement's next event does not collide with this one.
	if err := tx.QueryRow(ctx, `
		with bumped as (update issues set last_seq = last_seq + 1 where key = 'CORE-1' returning last_seq)
		insert into events (issue_key, seq, type, actor, payload, notify)
		select 'CORE-1', bumped.last_seq, 'artifact.version', '{"kind":"user","id":"alice"}', $1, false
		from bumped
		returning id
	`, payload).Scan(&event); err != nil {
		t.Fatalf("record the version's event: %v", err)
	}
	if err := result.Blocks.Caused(ctx, tx, event); err != nil {
		t.Fatalf("append what the version's block index stated: %v", err)
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit the version write: %v", err)
	}
	return result
}

// A write that read an older version of a document cannot state a parent a newer version has
// replaced. The race is built rather than waited for: a pass reads the document at the version
// it stands at and is held open before its commit, a newer version is written through the real
// version write path, and the older pass then runs on to its commit. The fence refuses it -
// document_block_versions still holds the newer revision, the current part_of tuples are that
// version's, and the older pass appended no tuple at all - and the same pass at a revision above
// the stored one does state its parents, so it is the fence and not the pass's own work that
// refused.
//
// The fence is one conditional write under one pair of locks, proven by holding the
// document_block_versions row in a second transaction and reading the pass waiting at that row
// rather than at a later step. Each writer the fence protects is driven through the race, and
// the source tree is read for any statement that writes document_block_versions outside it.
func TestAStaleWriteCannotAssertAParentANewerEditReplaced(t *testing.T) {
	ctx := context.Background()
	database, service := blockIndexService(t)
	document := createOwedDocument(t, database, service, "", "race", "- one\n- two\n", "")
	settle(t, database, document)
	if tick := reconcileBlockIndex(t, service, ""); tick.Documents != 1 {
		t.Fatalf("the first tick indexed %d documents, want the fixture's one", tick.Documents)
	}
	if stands := blockIndexAt(t, database, document); stands == nil || *stands != 1 {
		t.Fatalf("the document's blocks stand at %v, want version 1", stands)
	}

	// The stale pass reads the document as it stands now and is held open before its commit.
	staleMarkdown, staleBlocks := flattenedPass(t, database, document)
	staleConn, err := database.Pool.Dedicated(ctx, "block-index-stale-pass")
	if err != nil {
		t.Fatalf("open the stale pass's connection: %v", err)
	}
	defer staleConn.Close(ctx)
	staleTx, err := staleConn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the stale pass: %v", err)
	}
	defer staleTx.Rollback(ctx)

	// The newer edit, written through the real version write path while the stale pass is open.
	if _, err := joinedApplyOps(service, document, []model.EditOp{
		{Op: "insert", After: "end", Markdown: "A paragraph the newer version adds."},
	}, model.Actor{Kind: "user", ID: "alice"}, nil); err != nil {
		t.Fatalf("edit the document the newer version records: %v", err)
	}
	newer := writeVersionThroughTheRealPath(t, service, document, "The newer edit")
	if newer.Version.Number != 2 {
		t.Fatalf("the newer version is %d, want 2", newer.Version.Number)
	}
	if stands := blockIndexAt(t, database, document); stands == nil || *stands != newer.Version.Number {
		t.Fatalf("after the newer version the document's blocks stand at %v, want %d", stands, newer.Version.Number)
	}
	stated := parentsNow(t, database, document)

	// The stale pass runs on to its commit, and the fence refuses it.
	before := highestTuple(t, database)
	_, won, write, err := refs.IndexBlocks(ctx, staleTx, document, 1, time.Now(), staleMarkdown, staleBlocks, nil, nil)
	if err != nil {
		t.Fatalf("run the stale pass: %v", err)
	}
	if err := write.Caused(ctx, staleTx, graphstore.NoEvent); err != nil {
		t.Fatalf("append what the stale pass stated: %v", err)
	}
	if err := staleTx.Commit(ctx); err != nil {
		t.Fatalf("commit the stale pass: %v", err)
	}
	if won {
		t.Errorf("the stale pass at version 1 claimed the document's blocks stand at it")
	}
	if appended := tuplesSince(t, database, document, before); appended != 0 {
		t.Errorf("the stale pass appended %d tuples, want none", appended)
	}
	if stands := blockIndexAt(t, database, document); stands == nil || *stands != newer.Version.Number {
		t.Errorf("after the stale pass the document's blocks stand at %v, want version %d", stands, newer.Version.Number)
	}
	if now := parentsNow(t, database, document); !sameParents(now, stated) {
		t.Errorf("the stale pass moved the parents from %v to %v", stated, now)
	}

	// The control: the same pass above the stored revision does state its parents, so what
	// refused it was the fence and not a pass with nothing to say.
	third := nextVersionRow(t, database, document, staleMarkdown)
	controlTx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the control pass: %v", err)
	}
	defer controlTx.Rollback(ctx)
	_, won, write, err = refs.IndexBlocks(ctx, controlTx, document, third, time.Now(), staleMarkdown, staleBlocks, nil, nil)
	if err != nil {
		t.Fatalf("run the control pass: %v", err)
	}
	if err := write.Caused(ctx, controlTx, graphstore.NoEvent); err != nil {
		t.Fatalf("append what the control pass stated: %v", err)
	}
	if err := controlTx.Commit(ctx); err != nil {
		t.Fatalf("commit the control pass: %v", err)
	}
	if !won {
		t.Fatalf("the control pass above the stored revision did not stand")
	}
	flattened := parentsNow(t, database, document)
	for block, parent := range flattened {
		if !strings.HasPrefix(parent, "artifact/") {
			t.Errorf("the control pass left block %s part of %s, want the document: it had work the fence refused the stale pass", block, parent)
		}
	}
	if sameParents(flattened, stated) {
		t.Errorf("the control pass changed nothing, so the stale pass proves nothing: parents %v", flattened)
	}

	requireThePassWaitsAtTheOwedRow(t, database, document)
	requireTheTickIsFenced(t, database, service)
	requireOnlyTheFenceWritesTheOwedRow(t)
}

// nextVersionRow writes one more artifact_versions row holding markdown and answers its number,
// so a pass can be run at a revision above the one the document stands at.
func nextVersionRow(t *testing.T, database *store.Store, artifactID, markdown string) int {
	t.Helper()
	var number int
	if err := database.Pool.QueryRow(context.Background(), `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		select $1, coalesce(max(number), 0) + 1, $2, '[{"kind":"user","id":"alice"}]'
		from artifact_versions where artifact_id = $1
		returning number
	`, artifactID, markdown).Scan(&number); err != nil {
		t.Fatalf("write one more version row: %v", err)
	}
	return number
}

// requireThePassWaitsAtTheOwedRow holds the document's document_block_versions row in a
// transaction of its own and starts an indexing pass on a connection of its own: the pass must
// queue behind that row, and the statement its backend is running must be the fence rather than
// a projection write further in. That is what makes the fence one conditional write under one
// pair of locks: the pass reaches no projection row before it knows whether it won.
func requireThePassWaitsAtTheOwedRow(t *testing.T, database *store.Store, artifactID string) {
	t.Helper()
	ctx := context.Background()
	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the holder of the owed row: %v", err)
	}
	defer holder.Rollback(ctx)
	var held int
	if err := holder.QueryRow(ctx,
		`select coalesce(version, 0) from document_block_versions where artifact_id = $1 for no key update`,
		artifactID).Scan(&held); err != nil {
		t.Fatalf("hold the owed row: %v", err)
	}

	passConn, err := database.Pool.Dedicated(ctx, "block-index-waiting-pass")
	if err != nil {
		t.Fatalf("open the waiting pass's connection: %v", err)
	}
	defer passConn.Close(ctx)
	pid := passConn.PgConn().PID()
	markdown, blocks := flattenedPass(t, database, artifactID)
	answered := make(chan error, 1)
	go func() {
		// The send is the last thing this goroutine does, and the rollback has already run by
		// then: the caller closes passConn as soon as it reads this channel, and a rollback
		// still in flight on that same connection races that close (pgx answers it "BUG: slow
		// write timer already active"). Deferring the rollback inside the inner function is
		// what orders the two.
		answered <- func() error {
			tx, err := passConn.Begin(ctx)
			if err != nil {
				return err
			}
			defer tx.Rollback(ctx)
			_, _, _, err = refs.IndexBlocks(ctx, tx, artifactID, 99, time.Now(), markdown, blocks, nil, nil)
			return err
		}()
	}()
	if answer, early := storetest.WaitForQueuedBehind(t, holder, pid, answered, fenceWait); early {
		t.Fatalf("the pass answered %v without ever waiting on the owed row", answer)
	}
	var running string
	if err := holder.QueryRow(ctx,
		`select query from pg_stat_activity where pid = $1`, pid).Scan(&running); err != nil {
		t.Fatalf("read the statement the waiting pass is running: %v", err)
	}
	if !strings.Contains(running, "document_block_versions") {
		t.Errorf("the pass waits at %q, want it waiting at the document_block_versions row", running)
	}
	if strings.Contains(running, "document_blocks ") || strings.Contains(running, "block_refs") {
		t.Errorf("the pass waits at %q, a projection write: the fence must be reached before any of them", running)
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("release the owed row: %v", err)
	}
	if err := <-answered; err != nil {
		t.Fatalf("the pass that waited on the owed row: %v", err)
	}
}

// requireTheTickIsFenced drives the block-index reconciliation through the same race: its owed
// row is held while it runs, raised to a revision above the document's latest version from
// under it, and released. The tick must commit no tuple below that stored revision.
func requireTheTickIsFenced(t *testing.T, database *store.Store, service *Service) {
	t.Helper()
	ctx := context.Background()
	fenced := createOwedDocument(t, database, service, "", "fenced", "- alpha\n- beta\n", "")
	settle(t, database, fenced)
	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the holder of the tick's owed row: %v", err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx,
		`select 1 from document_block_versions where artifact_id = $1 for no key update`, fenced); err != nil {
		t.Fatalf("hold the tick's owed row: %v", err)
	}

	before := highestTuple(t, database)
	ticked := make(chan BlockIndexTick, 1)
	go func() {
		tick, err := service.ReconcileBlockIndex(ctx, BlockIndexCursors{}, liveAheadDocuments{})
		if err != nil {
			t.Errorf("the fenced tick: %v", err)
		}
		ticked <- tick
	}()
	if waiters := storetest.WaitForLockWaiters(t, holder, 1, fenceWait); waiters < 1 {
		t.Fatalf("the tick never queued behind the owed row")
	}
	// A newer pass won the race while the tick waited: the row stands at a revision above the
	// document's latest version.
	const newer = 99
	if _, err := holder.Exec(ctx,
		`update document_block_versions set version = $2 where artifact_id = $1`, fenced, newer); err != nil {
		t.Fatalf("raise the owed row from under the tick: %v", err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("commit the newer pass's mark: %v", err)
	}
	<-ticked
	// What the tick must not do is commit the work of a revision below the stored one: no tuple
	// and no projection row. The stored revision itself is not read back here, because the same
	// tick's grammar arm owes the document an index again (oweDocumentIndex) and sets it null.
	if appended := tuplesSince(t, database, fenced, before); appended != 0 {
		t.Errorf("the block-index reconciliation tick committed %d tuples at version 1 below the stored revision %d, want none",
			appended, newer)
	}
	var rows int
	if err := database.Pool.QueryRow(ctx,
		`select count(*) from document_blocks where artifact_id = $1`, fenced).Scan(&rows); err != nil {
		t.Fatalf("count the fenced document's blocks: %v", err)
	}
	if rows != 0 {
		t.Errorf("the fenced tick wrote %d block rows at version 1 below the stored revision %d, want none", rows, newer)
	}
}

// fenceWriter names where a statement that writes document_block_versions may live: the one
// fenced writer. A statement that only owes a document an index writes no revision at all, so
// it is told apart by what it sets rather than by where it is.
const (
	fenceWriterFile     = "refs/blocks.go"
	fenceWriterFunction = "func standsAtVersion("
)

// owedRowWrite matches one statement of a Go source file that writes document_block_versions,
// up to the backtick that ends it.
var owedRowWrite = regexp.MustCompile("(?is)(insert into|update)\\s+document_block_versions\\b[^`]*")

// requireOnlyTheFenceWritesTheOwedRow reads the source tree and fails on any statement that
// writes a revision into document_block_versions outside the fenced writer. A statement that
// sets the revision to null owes the document an index and asserts nothing, so it is allowed
// wherever it is; one that writes a number is the fence's alone, and a backfill or a repair
// that wrote one would be caught here rather than in production.
func requireOnlyTheFenceWritesTheOwedRow(t *testing.T) {
	t.Helper()
	found := 0
	for _, file := range dispatchSourceFiles(t) {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		text := string(body)
		for _, match := range owedRowWrite.FindAllStringIndex(text, -1) {
			statement := text[match[0]:match[1]]
			if strings.Contains(statement, "version = null") {
				continue
			}
			found++
			enclosing := enclosingFunction(text, match[0])
			if !strings.HasSuffix(filepath.ToSlash(file), fenceWriterFile) || !strings.HasPrefix(enclosing, fenceWriterFunction) {
				t.Errorf("%s writes a revision into document_block_versions in %q, outside the fenced writer %s in %s:\n%s",
					file, enclosing, fenceWriterFunction, fenceWriterFile, statement)
			}
		}
	}
	if found == 0 {
		t.Errorf("the source read found no statement writing a revision into document_block_versions, so it proves nothing")
	}
}

// dispatchSourceFiles is every non-test Go file of the Dispatch tree and of the commands that
// write to it, read from this package's own directory.
func dispatchSourceFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	for _, root := range []string{"..", "../../../cmd"} {
		if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			files = append(files, path)
			return nil
		}); err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if len(files) == 0 {
		t.Fatalf("the source read found no Go file; the tree is not where this test looks")
	}
	return files
}

// enclosingFunction is the `func` line above offset in a gofmt-clean file, which is the
// function a statement written there lives in.
func enclosingFunction(text string, offset int) string {
	if offset < 0 {
		return ""
	}
	start := strings.LastIndex(text[:offset], "\nfunc ")
	if start < 0 {
		return ""
	}
	line := text[start+1:]
	if end := strings.IndexByte(line, '\n'); end >= 0 {
		line = line[:end]
	}
	return line
}
