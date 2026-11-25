package docs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testcleanup"
	"github.com/sjawhar/envoy/internal/testwait"
)

func TestConditionalDocumentEditDoesNotOverwriteWriterDuringTableAnchorCheck(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name         string
		precondition bool
		want         string
	}{
		{name: "conditional", precondition: true, want: "HUMAN WRITE"},
		// Without a precondition the edit applies. It was computed on its transaction's fork of
		// the room, so it writes only its own change, and the browser's write made while its
		// table check waited survives beside it.
		{name: "unconditional", want: "HUMAN WRITE"},
	} {
		t.Run(test.name, func(t *testing.T) {
			database := storetest.Open(t)
			config := database.Pool.Config().Copy()
			config.MaxConns = 8
			testwait.Returns(t, "every connection of the test database's first pool to come back, so the pool closes", database.Pool.Close)
			pool, err := pgxpool.NewWithConfig(context.Background(), config)
			if err != nil {
				t.Fatalf("open dedicated table-race pool: %v", err)
			}
			database.Pool = store.NewPool(pool)
			testcleanup.Add(t, func() {
				testwait.Returns(t, "every connection of the dedicated table-race pool to come back, so the pool closes", pool.Close)
			})
			artifactID := createDocument(t, database, "")
			service := New(Deps{Store: database, Settle: stagedAhead})
			testcleanup.Add(t, func() {
				if err := docstest.Shutdown(t, testwait.Context(t), service); err != nil {
					t.Errorf("shutdown service: %v", err)
				}
			})
			seedServiceText(t, service, artifactID, "| Key | Value |\n| --- | --- |\n| delete | row |\n| retain | row |\n\nkeep")
			actor := model.Actor{Kind: "user", ID: "alice"}
			anchored, err := joinedMarkQuote(service, artifactID, MarkSpec{
				Kind: MarkComment,
				ID:   "00000000-0000-4000-8000-000000000003",
				By:   actor,
			}, "delete", nil)

			if err != nil {
				t.Fatalf("mark table cell: %v", err)
			}
			var blockID *string
			if anchored.BlockID != "" {
				blockID = &anchored.BlockID
			}
			anchorJSON, err := json.Marshal(model.Anchor{
				ArtifactID: artifactID,
				MarkID:     "00000000-0000-4000-8000-000000000003",
				Version:    1,
				Quote:      anchored.Quote,
				BlockID:    blockID,
			})
			if err != nil {
				t.Fatalf("encode table anchor: %v", err)
			}
			if _, err := database.Pool.Exec(context.Background(), `
				insert into comments (id, issue_key, author, body, anchor, resolved)
				values ($1, 'DOC-1', '{"kind":"user","id":"alice"}', 'Table anchor', $2, true)
			`, "00000000-0000-4000-8000-000000000003", anchorJSON); err != nil {
				t.Fatalf("index table anchor: %v", err)
			}
			markdown, blocks, err := service.TextWithBlocks(context.Background(), artifactID)
			if err != nil {
				t.Fatalf("read document: %v", err)
			}
			var tableID string
			for _, block := range blocks {
				if block.Type == "table" {
					tableID = block.ID
					break
				}
			}
			if tableID == "" {
				t.Fatalf("blocks = %#v, want table", blocks)
			}
			// The browser writes into a room it has open. A write to a document no room holds
			// loads none (publishLiveWrite), so the room is loaded here, before the gate below
			// holds the asks table a room's load reads.
			if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
				t.Fatalf("load the document's room: %v", err)
			}

			gate, err := database.Pool.Begin(context.Background())
			if err != nil {
				t.Fatalf("begin asks gate: %v", err)
			}
			defer gate.Rollback(context.Background())
			if _, err := gate.Exec(context.Background(), "lock table asks in access exclusive mode"); err != nil {
				t.Fatalf("lock asks: %v", err)
			}
			conditionalTx, err := database.Pool.Begin(context.Background())
			if err != nil {
				t.Fatalf("begin edit transaction: %v", err)
			}
			defer conditionalTx.Rollback(context.Background())
			joined, ledger := service.Join(context.Background(), conditionalTx)
			defer ledger.Discard()
			result := make(chan error, 1)
			var precondition *model.EditPrecondition
			if test.precondition {
				precondition = &model.EditPrecondition{Document: documentTokenForTest(t, service, artifactID, markdown)}
			}
			go func() {
				_, err := service.ApplyOps(joined, artifactID, []model.EditOp{{
					Op: "delete_row", Block: tableID, Index: []byte("1"),
				}}, actor, precondition)
				result <- err
			}()
			waitForAsksQueryLock(t, database)
			browserMutated := make(chan struct{})
			browserResult := make(chan error, 1)
			go func() {
				browserResult <- browserStyleWrite(service, artifactID, replaceRun("keep", "HUMAN WRITE"), browserMutated)
			}()
			testwait.Receive(t, "the browser writer to mutate the live tree", browserMutated)
			if err := gate.Commit(context.Background()); err != nil {
				t.Fatalf("release asks gate: %v", err)
			}
			if err := testwait.Receive(t, "the browser writer to finish", browserResult); err != nil {
				t.Fatalf("browser writer: %v", err)
			}
			err = testwait.Receive(t, "the edit to finish", result)
			if test.precondition {
				var stale *ErrPreconditionFailed
				if !errors.As(err, &stale) {
					t.Fatalf("conditional edit error = %v, want ErrPreconditionFailed", err)
				}
				if err := conditionalTx.Rollback(context.Background()); err != nil {
					t.Fatalf("rollback stale conditional edit: %v", err)
				}
				ledger.Discard()
			} else {
				if err != nil {
					t.Fatalf("unconditional edit: %v", err)
				}
				if err := ledger.commit(context.Background()); err != nil {
					t.Fatalf("commit unconditional edit: %v", err)
				}
				ledger.publish()
			}
			if err := service.Evict(context.Background(), artifactID); err != nil {
				t.Fatalf("evict resident document: %v", err)
			}
			persisted, err := service.Text(context.Background(), artifactID)
			if err != nil {
				t.Fatalf("read durable document: %v", err)
			}
			if !strings.Contains(persisted, test.want) {
				t.Fatalf("durable document = %q, want %q", persisted, test.want)
			}
			if deleted := !strings.Contains(persisted, "| delete |"); deleted == test.precondition {
				t.Fatalf("durable document = %q, want the delete row removed only by the unconditional edit", persisted)
			}
		})
	}
}

func browserStyleWrite(service *Service, artifactID string, edit func(*pmdoc.Node) *pmdoc.Node, mutated chan<- struct{}) error {
	doc := service.srv.GetDoc(artifactID)
	if doc == nil {
		return errDocUnloaded
	}
	fragment := doc.GetXmlFragment(fragmentName)
	var writeErr error
	doc.Transact(func(transaction *crdt.Transaction) {
		tree, err := pmdoc.ReadInTransaction(transaction, fragment)
		if err != nil {
			writeErr = err
			return
		}
		writeErr = pmdoc.Update(transaction, fragment, edit(tree))
		if writeErr == nil {
			close(mutated)
		}
	})
	return writeErr
}

func documentTokenForTest(t *testing.T, service *Service, artifactID, markdown string) string {
	t.Helper()
	read, token, err := service.TextWithToken(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("read document token: %v", err)
	}
	if read != markdown || token == "" {
		t.Fatalf("document token read = markdown %q token %q, want markdown %q and token", read, token, markdown)
	}
	return token
}

func waitForAsksQueryLock(t *testing.T, database *store.Store) {
	t.Helper()
	testwait.Eventually(t, "the edit to reach the table-anchor query's lock on asks", func() bool {
		var waiting int
		if err := database.Pool.QueryRow(context.Background(), `
			select count(*)
			from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock' and query like '%from asks%'
		`).Scan(&waiting); err != nil {
			t.Fatalf("inspect asks query lock: %v", err)
		}
		return waiting > 0
	})
}

// The WebSocket server's update observer persists the browser-style writer.
