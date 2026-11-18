package api

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testwait"
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

// anotherFillTry moves the fill's clock to its next try: the loop waits
// embeddingProjectKeyFillRetry, jittered, between two tries, and that wait is the only thing
// armed on this clock, so moving to it is one more try and costs no wall clock.
func anotherFillTry(t *testing.T, fake *clocktest.Fake) {
	t.Helper()
	armed := fake.AwaitArmed(t, "the fill's wait before its next try", 1)
	fake.Set(armed[0])
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
	fake := clocktest.New(time.Now())

	// The process that is about to die, with a pool of its own on the same database: it takes
	// the lease through the machinery the server takes it through and stands inside its pass.
	dying := storetest.SecondProcess(t, database)
	dyingCallers, err := dying.Pool.Leases()
	if err != nil {
		t.Fatalf("open the dying process's lease pool: %v", err)
	}
	// The lease pool is handed out as reservations, one per caller that can be inside a lease
	// statement at the same moment (`store.LeaseCallers`), so this holder takes a name of its
	// own as the server's fill does.
	dyingLeases, err := dyingCallers.For("embeddings project key fill test hold")
	if err != nil {
		t.Fatalf("reserve a caller of the dying process's lease pool: %v", err)
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
	testwait.Receive(t, "the other process to take the embeddings-project-key-fill lease", taken)

	// This process, as the boot starts it, on a clock the test moves try by try.
	leases := sweepLeases(t, database)
	fillCtx, stopFill := context.WithCancel(ctx)
	defer stopFill()
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		runEmbeddingProjectKeyFillOn(fillCtx, fake, database, leases)
	}()

	// Twenty of its retries pass while the lease is held elsewhere, and it labels nothing in
	// any of them. The last move starts a twenty-first try, which is in flight when the loop
	// ends; it has to find the lease held too, and arm its wait before the next try, before the
	// holder dies. Released while that try was still on its way to the lease, the holder would
	// hand the lease to it rather than to the next one, the fill would finish and return there,
	// and no wait would ever be armed again for the move after the death to find: a loaded run
	// waited on that move until the test binary's deadline and failed every row after it.
	for range 20 {
		anotherFillTry(t, fake)
	}
	afterTheLastSkip := fake.AwaitArmed(t, "the fill's wait after the last try the held lease turned away", 1)
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

	// Its next try takes the lease the dead holder left.
	fake.Set(afterTheLastSkip[0])
	testwait.Receive(t, "the process that skipped to finish the fill once the holder died", returned)
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
// repaired by hand, one restored from a dump taken before that migration, or one somebody is
// moving, and the loop says so once and stops.
//
// What it holds is the stopping and the saying once: the loop returns, and over many retry
// intervals it writes one error line and no more. A loop that still tried would write one per
// interval and never return.
func TestTheEmbeddingProjectKeyFillStopsWhenItsProgressRowIsGone(t *testing.T) {
	fillStopsWhenItsProgressIsGone(t, "delete from embeddings_project_key_backfill")
}

// The same stop for the same reason, the table gone rather than the row in it. Postgres answers
// the whole statement SQLSTATE 42P01 there where it answers no row here, and the store names
// both absences with one sentinel (store.ErrEmbeddingProjectKeyProgressGone), because the fill
// wants the same thing of both: nothing it can do brings either back. Until this was matched,
// the loop stopped on the row and tried the table every interval for the life of the process.
func TestTheEmbeddingProjectKeyFillStopsWhenItsProgressTableIsGone(t *testing.T) {
	fillStopsWhenItsProgressIsGone(t, "drop table embeddings_project_key_backfill")
}

// fillStopsWhenItsProgressIsGone runs the loop on a database the statement gone has taken the
// fill's progress from, and holds it to the two properties above: it returns, and it writes one
// error line. It runs on a fake clock nothing moves, so a loop that waited for another try
// would wait for ever and never return, which is what the wait below fails on.
func fillStopsWhenItsProgressIsGone(t *testing.T, gone string) {
	t.Helper()
	ctx := context.Background()
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(ctx, gone); err != nil {
		t.Fatalf("take the fill's progress away with %q: %v", gone, err)
	}
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
		runEmbeddingProjectKeyFillOn(fillCtx, clocktest.New(time.Now()), database, leases)
	}()
	testwait.Receive(t, "the fill to stop once its progress is gone, rather than take the lease every interval", returned)

	var failures []string
	for _, record := range records.Records() {
		if record.Level >= slog.LevelError {
			failures = append(failures, record.Message)
		}
	}
	if len(failures) != 1 {
		t.Errorf("the fill logged %d errors after %q, want the one line that says so: %v", len(failures), gone, failures)
	}
}
