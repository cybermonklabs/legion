package api

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// embeddingProjectKinds is every kind of embeddings row this release writes: the four window
// kinds the block-index tick's window arm writes (refs.WriteItemWindows,
// refs.WriteDocumentWindows) and the whole-text row the issue arm reads, which 0140's two
// triggers write. Each is read by a meaning arm under `e.project_key = $2` when the search
// names a project (searchQueryMeaningScoped), so a row of any of them left unlabelled is a row
// no project-scoped meaning search can return.
var embeddingProjectKinds = []string{"comment_block", "document_block", "message_block", "ask_part", "issue"}

// A write of each kind, and then no row of any of them carries a null project_key. This is what
// holds every write path to the column: the two enqueue functions migration 20261109120000
// replaced (embeddings_enqueue and embeddings_enqueue_window, which every window and every
// issue row goes through) and the coverage pass's own insert (embedqueue's insertMissingIssues).
// A path added later that writes a row by hand, or an enqueue that stops setting the column,
// fails here with the kind it left behind.
//
// It also reads the value, not merely its presence: a window labelled with another project's
// key would be as invisible to the project that owns it as a null one, and no count of nulls
// would say so.
func TestEveryWrittenEmbeddingRowCarriesItsProject(t *testing.T) {
	const project = "LABL"
	embedder := &fakeEmbedder{vectors: map[string][]float32{}}
	handler, database, deps := newTestServer(t, testServerOptions{embedder: embedder})
	issue := createInteractionIssue(t, handler, project, "A labelled issue", "Body text.")
	createThreadComment(t, handler, issue.Key, map[string]any{"body": "A remark to window."}, "alice")
	createIssueMessage(t, handler, issue.Key, map[string]any{"body": "A message to window."}, "alice")
	createIssueAsk(t, handler, issue.Key, "A question to window?")
	uploadSearchDocument(t, handler, issue.Key, "notes.md", "A document to window.\n")
	// A project document and a comment on it: both stand on no issue, so their project is the
	// document's own rather than an issue's, which is the other half of
	// embeddings_owner_project_key's rule for each of those two kinds.
	loose := createProjectDocument(t, handler, project, "Design notes", "# Design notes\n\nA passage to window.\n")
	onDocument := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+loose.ID+"/comments", map[string]any{
		"body":   "A remark on the document, standing on no issue.",
		"anchor": map[string]string{"artifact": loose.Slug, "quote": "passage"},
		"actor":  sessionActor(),
	})
	if onDocument.Code != http.StatusCreated {
		t.Fatalf("comment on the project document: status=%d body=%s", onDocument.Code, onDocument.Body.String())
	}
	documentComment := decodeBody[model.Comment](t, onDocument)
	// The writes record each owner's blocks; the windows of those blocks are the tick's.
	windowOwedOwners(t, deps)

	ctx := context.Background()
	for _, kind := range embeddingProjectKinds {
		var rows, labelled int
		if err := database.Pool.QueryRow(ctx, `
			select count(*), count(*) filter (where project_key = $2) from embeddings where kind = $1
		`, kind, project).Scan(&rows, &labelled); err != nil {
			t.Fatalf("read the %s rows: %v", kind, err)
		}
		if rows == 0 {
			t.Fatalf("no %s row was written, so this test proves nothing about that kind", kind)
		}
		if labelled != rows {
			t.Errorf("%d of the %d %s rows carry project_key %q; the rest carry another project or none",
				labelled, rows, kind, project)
		}
	}
	// Named on their own, so a rule that labelled only the owners reached through an issue
	// would fail here rather than passing on the counts above.
	for _, owner := range []struct{ kind, key string }{
		{"document_block", loose.ID},
		{"comment_block", "comment/" + documentComment.ID},
	} {
		var windows, labelled int
		if err := database.Pool.QueryRow(ctx, `
			select count(*), count(*) filter (where project_key = $3) from embeddings
			where kind = $1 and split_part(id, '#', 1) = $2
		`, owner.kind, owner.key, project).Scan(&windows, &labelled); err != nil {
			t.Fatalf("read the %s windows of %s: %v", owner.kind, owner.key, err)
		}
		if windows == 0 || labelled != windows {
			t.Errorf("%s %s holds %d windows, %d of them labelled %q; it stands on no issue, so its project is the document's",
				owner.kind, owner.key, windows, labelled, project)
		}
	}
}

// The fill labels the rows stored before the column existed, which is every row of a database
// that has had both migrations applied under an earlier release's writes. The rows are
// unlabelled here the only way a test can make them so - the column is set back to null - and
// the pass is then run as the boot runs it (store.FillEmbeddingProjectKeys,
// cmd/dispatch/main.go).
//
// What it holds: the pass labels every row it can, in batches whose number is the rows over
// store.EmbeddingProjectKeyBatch, it records itself finished, and a second run of it reads the
// one progress row and writes nothing. A pass that stopped at its first batch, or one whose
// cursor did not move, fails on the rows it left null.
func TestTheFillLabelsEveryEmbeddingRowStoredBeforeTheColumn(t *testing.T) {
	const project = "FILL"
	embedder := &fakeEmbedder{vectors: map[string][]float32{}}
	handler, database, deps := newTestServer(t, testServerOptions{embedder: embedder})
	issue := createInteractionIssue(t, handler, project, "A fill subject", "Body text.")
	createThreadComment(t, handler, issue.Key, map[string]any{"body": "A remark to label."}, "alice")
	createIssueMessage(t, handler, issue.Key, map[string]any{"body": "A message to label."}, "alice")
	createIssueAsk(t, handler, issue.Key, "A question to label?")
	uploadSearchDocument(t, handler, issue.Key, "notes.md", "A document to label.\n")
	windowOwedOwners(t, deps)

	ctx := context.Background()
	// The kinds meaning search reads: a row of a retired whole-text kind, which the block index
	// deletes as its owner gains a current window, belongs to no arm and the fill leaves it
	// null (embeddings_owner_project_key).
	stored := countRows(t, database, "select count(*) from embeddings where kind = any($1)", embeddingProjectKinds)
	if stored == 0 {
		t.Fatal("the fixture stored no embeddings row of a searched kind, so this test proves nothing")
	}
	if _, err := database.Pool.Exec(ctx, "update embeddings set project_key = null"); err != nil {
		t.Fatalf("unlabel the stored rows: %v", err)
	}

	labelled, err := database.FillEmbeddingProjectKeys(ctx)
	if err != nil {
		t.Fatalf("label the stored rows: %v", err)
	}
	if labelled != stored {
		t.Errorf("the fill labelled %d rows, want the %d of a searched kind it was given", labelled, stored)
	}
	if left := countRows(t, database,
		"select count(*) from embeddings where kind = any($1) and project_key is null", embeddingProjectKinds); left != 0 {
		t.Errorf("%d rows are still unlabelled after the fill", left)
	}
	if mislabelled := countRows(t, database,
		"select count(*) from embeddings where kind = any($1) and project_key is distinct from $2",
		embeddingProjectKinds, project); mislabelled != 0 {
		t.Errorf("%d rows carry a project other than %q", mislabelled, project)
	}

	var done bool
	if err := database.Pool.QueryRow(ctx, "select done from embeddings_project_key_backfill").Scan(&done); err != nil {
		t.Fatalf("read the fill's progress: %v", err)
	}
	if !done {
		t.Error("the fill did not record itself finished, so every later boot would read the table again")
	}
	again, err := database.FillEmbeddingProjectKeys(ctx)
	if err != nil {
		t.Fatalf("run the finished fill again: %v", err)
	}
	if again != 0 {
		t.Errorf("a second run labelled %d rows, want none: the pass is finished", again)
	}
}

// fillRetryEvery sets how long a process waits before trying for the fill's lease again, for the
// length of the test: the deployment's five minutes would be five minutes of a test's life.
func fillRetryEvery(t *testing.T, every time.Duration) {
	t.Helper()
	previous := embeddingProjectKeyFillRetry
	t.Cleanup(func() { embeddingProjectKeyFillRetry = previous })
	embeddingProjectKeyFillRetry = every
}

// The fill is one to two hours of labelling on a deployment of a million vectors, so the two
// processes of a rolling update must never both make it: the second would walk the same keyset
// behind the first's commits and label nothing the first had not already labelled. The lease is
// what keeps them apart, and duty.Exclusive makes one attempt, so the process that skipped used
// to do nothing about the fill for the rest of its life: measured on this change's own head,
// two processes racing left the loser silent and three rows unlabelled after the winner died
// (INFRA-988). A project-scoped meaning search keeps the owner-joined predicate for as long as
// that lasts, so "until something next boots" is the recall the older read serves.
//
// So this is the other process's whole life in one test. Another process - a pool of its own on
// the same database, as the deployment's second process is - takes the lease and stands inside a
// pass of its own. While it stands there this process labels nothing and moves no cursor,
// however many times it tries. Then that process dies without finishing, and this one takes the
// lease at its next try, labels every row and returns for good: a fill nothing has to boot to
// finish.
func TestTheEmbeddingProjectKeyFillIsFinishedByTheProcessThatSkippedWhenTheHolderDies(t *testing.T) {
	const project = "LEAS"
	embedder := &fakeEmbedder{vectors: map[string][]float32{}}
	handler, database, deps := newTestServer(t, testServerOptions{embedder: embedder})
	issue := createInteractionIssue(t, handler, project, "A leased fill", "Body text.")
	createThreadComment(t, handler, issue.Key, map[string]any{"body": "A remark to label."}, "alice")
	uploadSearchDocument(t, handler, issue.Key, "notes.md", "A document to label.\n")
	windowOwedOwners(t, deps)

	ctx := context.Background()
	stored := countRows(t, database, "select count(*) from embeddings where kind = any($1)", embeddingProjectKinds)
	if stored == 0 {
		t.Fatal("the fixture stored no embeddings row of a searched kind, so this test proves nothing")
	}
	if _, err := database.Pool.Exec(ctx, "update embeddings set project_key = null"); err != nil {
		t.Fatalf("unlabel the stored rows: %v", err)
	}
	const retry = 50 * time.Millisecond
	fillRetryEvery(t, retry)

	// The process that is about to die, with a pool of its own on the same database: it takes
	// the lease through the machinery the server takes it through and stands inside its pass.
	dying := storetest.SecondProcess(t, database)
	dyingLeases, err := dying.Pool.Leases()
	if err != nil {
		t.Fatalf("open the dying process's lease pool: %v", err)
	}
	taken, release := make(chan struct{}), make(chan struct{})
	// The holder stands in its pass until this is called, and a failure above it calls it too:
	// a test that left that goroutine standing would hold the package's whole binary open.
	releaseHolder := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseHolder)
	held := make(chan bool, 1)
	go func() {
		held <- duty.Exclusive(ctx, dyingLeases, embeddingProjectKeyFillLease, time.Time{}, 0, func(context.Context) {
			close(taken)
			<-release
		})
	}()
	select {
	case <-taken:
	case <-time.After(20 * time.Second):
		t.Fatal("the other process never took the embeddings-project-key-fill lease")
	}

	// This process, as the boot starts it.
	leases := sweepLeases(t, database)
	fillCtx, stopFill := context.WithCancel(ctx)
	defer stopFill()
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		RunEmbeddingProjectKeyFill(fillCtx, database, leases)
	}()

	// Twenty of its retries pass while the lease is held elsewhere, and it labels nothing in
	// any of them.
	time.Sleep(20 * retry)
	if labelled := countRows(t, database, "select count(*) from embeddings where project_key is not null"); labelled != 0 {
		t.Errorf("the fill labelled %d rows under a lease held elsewhere; want none", labelled)
	}
	var kind, id string
	var done bool
	if err := database.Pool.QueryRow(ctx,
		"select kind, id, done from embeddings_project_key_backfill").Scan(&kind, &id, &done); err != nil {
		t.Fatalf("read the fill's progress: %v", err)
	}
	if kind != "" || id != "" || done {
		t.Errorf("the skipped fill left the cursor at (%q, %q) done=%v; want the cursor where it was and the pass unfinished",
			kind, id, done)
	}
	select {
	case <-returned:
		t.Fatal("the fill gave up while another process held the lease, which is the whole of what it must not do")
	default:
	}

	// The holder dies: its pass ends without labelling anything and its pool closes, which is
	// the session the lease belongs to going with the process (duty.Exclusive).
	releaseHolder()
	if !<-held {
		t.Fatal("the other process never held the lease, so the tries above were free to label and this test proves nothing")
	}
	dying.Pool.Close()

	select {
	case <-returned:
	case <-time.After(60 * time.Second):
		t.Fatal("the process that skipped never took the lease the dead holder left, so the rows it did not label wait for the next boot")
	}
	if left := countRows(t, database,
		"select count(*) from embeddings where kind = any($1) and project_key is null", embeddingProjectKinds); left != 0 {
		t.Errorf("%d rows are still unlabelled after the fill that took the lease; want none", left)
	}
	if mislabelled := countRows(t, database,
		"select count(*) from embeddings where kind = any($1) and project_key is distinct from $2",
		embeddingProjectKinds, project); mislabelled != 0 {
		t.Errorf("%d rows carry a project other than %q", mislabelled, project)
	}
	if err := database.Pool.QueryRow(ctx, "select done from embeddings_project_key_backfill").Scan(&done); err != nil {
		t.Fatalf("read the fill's progress: %v", err)
	}
	if !done {
		t.Error("the fill that took the lease did not record itself finished, so every later boot would read the table again")
	}
}

// The retry loop exists for a holder that dies mid-pass, and it stops on the one row that says
// the labelling is over (store.EmbeddingProjectKeyFillDone). A database without that row
// answers neither: the pass fails reading it, the loop's own check fails reading it, and the
// process logged both errors and took the lease again every five minutes for the rest of its
// life, for a row that is not coming back. Nothing deletes it - migration 20261109120100
// inserts it and no statement of this release removes it - so such a database is one somebody
// repaired by hand, and the loop says so once and stops.
//
// What it holds is the stopping and the saying once: the loop returns, and over many retry
// intervals it writes one error line and no more. A loop that still tried would write one per
// interval and never return.
func TestTheEmbeddingProjectKeyFillStopsWhenItsProgressRowIsGone(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(ctx, "delete from embeddings_project_key_backfill"); err != nil {
		t.Fatalf("remove the fill's progress row: %v", err)
	}
	const retry = 50 * time.Millisecond
	fillRetryEvery(t, retry)
	records := &logtest.Recorder{}
	logtest.SetDefault(t, records)

	leases := sweepLeases(t, database)
	// Cancelled when the test ends, so a loop that did not stop stops with it rather than
	// logging on against a closed pool for the rest of the package's run.
	fillCtx, stopFill := context.WithCancel(ctx)
	defer stopFill()
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		RunEmbeddingProjectKeyFill(fillCtx, database, leases)
	}()
	select {
	case <-returned:
	case <-time.After(20 * retry):
		t.Fatal("the fill kept trying for a progress row that is gone, so it takes the lease every interval for the life of the process")
	}

	// Long enough for a loop that had not stopped to have tried again several times.
	time.Sleep(10 * retry)
	var failures []string
	for _, record := range records.Records() {
		if record.Level >= slog.LevelError {
			failures = append(failures, record.Message)
		}
	}
	if len(failures) != 1 {
		t.Errorf("the fill logged %d errors for a progress row that is gone, want the one line that says so: %v", len(failures), failures)
	}
}
