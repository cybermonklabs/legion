package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
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
