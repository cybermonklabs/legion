package docs

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestTheRetirementOfADocumentTakesTheStoreLockOrder drives RetireDocument and writeRetiredNode,
// the retirement of an issue's document that the pre-mortem conversion runs (api's
// premortemConvert), with the machinery of TestEveryDocsStoreWriterTakesTheStoreLockOrder: the
// path lists its steps in the order it takes them, the list is checked against the order written
// on api/inbox_read.go's markInboxReadThreads (requireTheDocsOrderAdmits), and each step is held
// in turn while the path's backend is read at the statement it waits at and at the table locks it
// holds (requireEachDocsStepIsWaitedAtInTurn). The path takes the issue's owner lock, then states
// the document's artifact node gone, retires its artifacts row, appends document.retired and
// writes the node's facts under it.
func TestTheRetirementOfADocumentTakesTheStoreLockOrder(t *testing.T) {
	ctx := context.Background()
	rig, writer := retirementPath(t)
	requireTheDocsOrderAdmits(t, writer)
	requireEachDocsStepIsWaitedAtInTurn(t, ctx, rig, writer)
}

// retirementPath is the retirement of one document of an issue of a project of its own for each
// run, as the pre-mortem conversion retires the document it converts: RetireDocument in a
// transaction of its own, which takes the issue's owner lock first. The document's node is the
// one its create wrote, so the retirement's node statement, its row's update, its event and its
// facts are each held by a gate on that document's writes.
func retirementPath(t *testing.T) (*docsWriterRig, docsWriter) {
	database := storetest.Open(t)
	service := newTestServiceOn(t, database)
	service.settle = time.Hour
	r := &docsWriterRig{database: database, service: service}
	var issue, document, refKey string
	const retirement = "the retirement's event"
	return r, docsWriter{
		name: "RetireDocument retiring an issue's document",
		steps: []docsStep{
			{
				name: "the owner lock", order: docsOwnerStep,
				hold: func(t *testing.T, ctx context.Context, holder pgx.Tx, _ docsRun) {
					t.Helper()
					if err := store.LockOwner(ctx, holder, store.IssueOwner(issue)); err != nil {
						t.Fatalf("hold issue %s's owner lock: %v", issue, err)
					}
				},
				at: docsOwnerStatement,
			},
			{
				name: "the retired document's node", order: docsNodeStep,
				hold: gatedDocsWrites(r, "insert", "graph_nodes", func() string {
					return "new.kind = " + docsLiteral(graphstore.KindArtifact) + " and new.id = " + docsLiteral(refKey)
				}),
				at:    docsNodeStatement,
				takes: []docsTableLock{{"graph_nodes", docsRowExclusiveMode}},
			},
			{
				name: "the retired document's row", order: docsProjectionStep, names: "the retired document's node",
				hold: gatedDocsWrites(r, "update", "artifacts", func() string {
					return "new.id = " + docsLiteral(document) + "::uuid and new.retired_at is not null"
				}),
				at:    "update artifacts set retired_at",
				takes: []docsTableLock{{"artifacts", docsRowExclusiveMode}},
			},
			docsGatedEventStep(r, retirement, model.DocumentRetiredEventType),
			{
				name: "the retirement's facts", order: docsTupleStep,
				hold: gatedDocsWrites(r, "insert", "graph_tuples", func() string {
					return "new.from_kind = " + docsLiteral(graphstore.KindArtifact) + " and new.from_id = " + docsLiteral(refKey)
				}),
				at:     docsTupleStatement,
				takes:  []docsTableLock{{"graph_tuples", docsRowExclusiveMode}},
				caused: true, cause: retirement,
			},
		},
		ready: func(t *testing.T, ctx context.Context) docsRun {
			r.named++
			project := fmt.Sprintf("RETIRE%d", r.named)
			issue = project + "-1"
			for _, statement := range []string{
				`insert into projects (key, name, default_assignee) values ($1, $1, 'alice')`,
				`insert into issues (key, project_key, number, title, created_by, rank)
				 values ($1 || '-1', $1, 1, 'Retired', '{"kind":"user","id":"alice"}', 'U')`,
			} {
				if _, err := r.database.Pool.Exec(ctx, statement, project); err != nil {
					t.Fatalf("create issue %s: %v", issue, err)
				}
			}
			document = r.retiredDocument(t, ctx, issue, project, "# Notes\n\nThe notes the conversion retires.\n")
			if err := r.database.Pool.QueryRow(ctx, `select ref_key from artifacts where id = $1`, document).Scan(&refKey); err != nil {
				t.Fatalf("read the ref_key of %s: %v", document, err)
			}
			retired := document
			return docsRun{
				document: document,
				write: func() error {
					tx, err := r.database.Pool.Begin(ctx)
					if err != nil {
						return err
					}
					defer tx.Rollback(ctx)
					if _, err := RetireDocument(ctx, tx, retired, model.Actor{Kind: "user", ID: "alice"}, "The lock-step test retires it."); err != nil {
						return fmt.Errorf("retire %s: %w", retired, err)
					}
					return tx.Commit(ctx)
				},
			}
		},
	}
}

// retiredDocument creates the document notes-md of issue in project holding markdown as its
// version 1, written by alice and indexed as an upload of a new document is, and settles it.
func (r *docsWriterRig) retiredDocument(t *testing.T, ctx context.Context, issue, project, markdown string) string {
	t.Helper()
	var id string
	if err := r.database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ($1, $2, 'notes-md', 'notes.md', 'doc', false, '{"kind":"user","id":"alice"}')
		returning id::text
	`, issue, project).Scan(&id); err != nil {
		t.Fatalf("create the notes of %s: %v", issue, err)
	}
	if err := joinedWrite(r.service, func(ctx context.Context) error {
		written, err := r.service.SeedText(ctx, id, markdown, model.Actor{Kind: "user", ID: "alice"})
		if err != nil {
			return err
		}
		tx, _ := txFromContext(ctx)
		if _, err := tx.Exec(ctx, `
			insert into artifact_versions (artifact_id, number, markdown, authors)
			values ($1, 1, $2, '[{"kind":"user","id":"alice"}]')
		`, id, written); err != nil {
			return err
		}
		_, write, err := r.service.IndexDocumentReferences(ctx, id, 1, written)
		if err != nil {
			return err
		}
		return causeInWritePhase(ctx, tx, write, graphstore.NoEvent)
	}); err != nil {
		t.Fatalf("write the notes of %s: %v", issue, err)
	}
	settleCurrentGeneration(t, r.service, id)
	return id
}
