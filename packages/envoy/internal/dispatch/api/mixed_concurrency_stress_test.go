package api

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// pooledDocumentHandler serves a Dispatch API whose pool is maxConns wide and whose documents
// settle after settle, and returns the document service so a test can evict or settle a room.
func pooledDocumentHandler(t *testing.T, maxConns int32, settle time.Duration) (http.Handler, *docs.Service) {
	handler, service, _ := pooledDocumentHandlerWith(t, maxConns, settle, nil)
	return handler, service
}

// pooledDocumentHandlerWith lets a test wrap the document service the API sees, so it can hold
// one document call open while it arranges the rest of the pool. The service's shutdown at the
// test's end is waited for with one bound, the test binary's deadline (testwait.Receive): a
// shutdown a loaded machine runs slowly passes, and one that never returns fails by name.
func pooledDocumentHandlerWith(
	t *testing.T,
	maxConns int32,
	settle time.Duration,
	wrap func(*docs.Service) docs.API,
) (http.Handler, *docs.Service, *store.Store) {
	t.Helper()
	var service *docs.Service
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		config := database.Pool.Config().Copy()
		config.MaxConns = maxConns
		testwait.Returns(t, "every connection of the test database's first pool to come back, so the pool closes", database.Pool.Close)
		pool, err := pgxpool.NewWithConfig(context.Background(), config)
		if err != nil {
			t.Fatalf("open test pool: %v", err)
		}
		database.Pool = store.NewPool(pool)
		t.Cleanup(func() {
			testwait.Returns(t, "every connection of the "+fmt.Sprint(maxConns)+"-connection pool to come back, so the pool closes", pool.Close)
		})
		service = docs.New(docs.Deps{Store: database, Settle: settle})
		t.Cleanup(func() {
			if err := docstest.Shutdown(t, testwait.Context(t), service); err != nil {
				t.Errorf("shutdown document service: %v", err)
			}
		})
		if wrap != nil {
			return wrap(service)
		}
		return service
	})
	return handler, service, database
}

func TestConditionalEditFloodDoesNotStarveSmallPool(t *testing.T) {
	t.Parallel()
	handler, _ := pooledDocumentHandler(t, 2, stagedAhead)
	issue := createInteractionIssue(t, handler, "TEST", "Conditional flood", "alpha")
	for round := range 40 {
		read := readDocumentPrecondition(t, handler, issue.PrimaryArtifactID)
		var group sync.WaitGroup
		for index := range 6 {
			group.Add(1)
			go func(index int) {
				defer group.Done()
				body := map[string]any{
					"ops": []map[string]string{{
						"op":       "insert",
						"markdown": fmt.Sprintf("r%d-%d", round, index),
						"after":    "end",
					}},
					"precondition": map[string]string{"document": read.Token},
				}
				dispatchRequest(t, handler, http.MethodPost,
					"/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", body, "alice")
			}(index)
		}
		finished := make(chan struct{})
		go func() {
			group.Wait()
			close(finished)
		}()
		testwait.Receive(t, fmt.Sprintf("round %d's conditional edit flood to return (a wedged pool never returns it)", round), finished)
	}
}

func TestConcurrentConditionalAndUnconditionalEditsDoNotWedge(t *testing.T) {
	t.Parallel()
	// Unconditional edits hold a transaction connection before warm-up and wedge
	// at pool_max_conns≈3 even on c34767d4; the follow-up fix owns that baseline.
	handler, _ := pooledDocumentHandler(t, 8, stagedAhead)
	issue := createInteractionIssue(t, handler, "TEST", "Mixed concurrency", "alpha")
	for round := range 40 {
		read := readDocumentPrecondition(t, handler, issue.PrimaryArtifactID)
		var group sync.WaitGroup
		for index := range 6 {
			group.Add(1)
			go func(index int) {
				defer group.Done()
				body := map[string]any{
					"ops": []map[string]string{{
						"op":       "insert",
						"markdown": fmt.Sprintf("mixed-%d-%d", round, index),
						"after":    "end",
					}},
				}
				if index%2 == 0 {
					body["precondition"] = map[string]string{"document": read.Token}
				}
				dispatchRequest(t, handler, http.MethodPost,
					"/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", body, "alice")
			}(index)
		}
		finished := make(chan struct{})
		go func() {
			group.Wait()
			close(finished)
		}()
		testwait.Receive(t, fmt.Sprintf("round %d's concurrent conditional and unconditional edits to return (a wedged pool never returns them)", round), finished)
	}
}
