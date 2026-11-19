package api

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// withDeadline bounds every request the handler serves by the shared wait's bound
// (testwait.Context). A wedged pool would otherwise hold its connections until the package
// timeout kills the process, which reports no failing test and leaves the pool cleanup unrun;
// with the bound the request aborts before that deadline, the connections go back, and the test
// says which scenario wedged.
func withDeadline(t *testing.T, handler http.Handler) http.Handler {
	bound := testwait.Context(t)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		defer context.AfterFunc(bound, cancel)()
		handler.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Anchoring a comment or an ask stamps a mark in the live document while the handler's
// transaction is open. That transaction holds one pooled connection, and the document work must
// take none from the same pool: as soon as concurrent anchored writes fill it, a handler that
// needs a second connection waits for one its own peers can only release by finishing, and the
// server never recovers. Real callers reach this whenever enough agents comment on one issue.
func TestConcurrentAnchoredWritesDoNotWedgeTheirPool(t *testing.T) {
	const maxConns = 4
	for _, writers := range []int{maxConns, maxConns * 2} {
		t.Run(fmt.Sprintf("writers=%d", writers), func(t *testing.T) {
			handler, _ := pooledDocumentHandler(t, maxConns, time.Hour)
			handler = withDeadline(t, handler)
			issue := createInteractionIssue(t, handler, "TEST", "Anchored concurrency",
				"alpha bravo charlie delta echo foxtrot golf hotel")
			quotes := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel"}

			statuses := make([]int, writers)
			var group sync.WaitGroup
			for index := range writers {
				group.Add(1)
				go func(index int) {
					defer group.Done()
					anchor := map[string]string{"artifact": "spec", "quote": quotes[index%len(quotes)]}
					if index%2 == 0 {
						statuses[index] = dispatchRequest(t, handler, http.MethodPost,
							"/api/v1/issues/"+issue.Key+"/comments",
							map[string]any{"anchor": anchor, "body": fmt.Sprintf("comment %d", index)}, "alice").Code
						return
					}
					statuses[index] = dispatchRequest(t, handler, http.MethodPost,
						"/api/v1/issues/"+issue.Key+"/asks",
						map[string]any{
							"anchor":   anchor,
							"options":  []map[string]string{{"label": "Yes"}},
							"question": fmt.Sprintf("ask %d?", index),
						}, "alice").Code
				}(index)
			}
			group.Wait()

			for index, status := range statuses {
				if status != http.StatusCreated {
					t.Errorf("anchored write %d: status=%d, want 201", index, status)
				}
			}
		})
	}
}

// gatedMarkQuote holds the first anchored write inside MarkQuote, after its handler has locked
// the issue row and while its transaction is open, so a test can fill the rest of the pool
// before letting the document work run.
type gatedMarkQuote struct {
	docs.API
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gatedMarkQuote) MarkQuote(
	ctx context.Context,
	artifactID string,
	mark docs.MarkSpec,
	quote string,
	occurrence *int,
) (docs.Anchored, error) {
	g.once.Do(func() {
		close(g.entered)
		<-g.release
	})
	return g.API.MarkQuote(ctx, artifactID, mark, quote, occurrence)
}

// The other holders that wait are the document settlements: each opens its own transaction and
// locks the same issue row an anchored write locks first (lockArtifactOwner), before it knows
// whether it has work. Two of them, one anchored write holding the row, and one more write
// queued behind it fill a four-connection pool - small on purpose, because the cycle this
// pins closes at any size and four reaches it with four callers - so the write in the middle
// must finish without asking that pool for anything else.
func TestSettlementsBehindAnAnchoredWriteDoNotWedgeThePool(t *testing.T) {
	const maxConns = 4
	gate := &gatedMarkQuote{entered: make(chan struct{}), release: make(chan struct{})}
	handler, service, database := pooledDocumentHandlerWith(t, maxConns, 10*time.Millisecond,
		func(service *docs.Service) docs.API {
			gate.API = service
			return gate
		})
	handler = withDeadline(t, handler)
	var released sync.Once
	release := func() { released.Do(func() { close(gate.release) }) }
	// However this test ends, the held write is released: a test that fails while it holds the
	// gate would otherwise leave its connections out and hang the package.
	defer release()
	issue := createInteractionIssue(t, handler, "TEST", "Settlement contention",
		"alpha bravo charlie delta")
	siblings := make([]string, 0, 2)
	for index := range 2 {
		response := dispatchRequest(t, handler, http.MethodPost,
			"/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
				"content": fmt.Sprintf("# Sibling %d\n\nfirst body\n", index),
				"name":    fmt.Sprintf("sibling-%d.md", index),
			}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create sibling %d: status=%d body=%s", index, response.Code, response.Body.String())
		}
		siblings = append(siblings, decodeBody[struct {
			Artifact model.Artifact `json:"artifact"`
		}](t, response).Artifact.ID)
	}
	// The write has to load its room while its transaction is open; that load is the second
	// connection the shared pool must never be asked for.
	if err := service.Evict(context.Background(), issue.PrimaryArtifactID); err != nil {
		t.Fatalf("evict document room: %v", err)
	}

	statuses := make([]int, 2)
	var writes sync.WaitGroup
	writes.Add(1)
	go func() {
		defer writes.Done()
		statuses[0] = dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
			map[string]any{
				"anchor": map[string]string{"artifact": "spec", "quote": "alpha"},
				"body":   "held write",
			}, "alice").Code
	}()
	<-gate.entered // the first write holds a connection and the issue row

	writes.Add(1)
	go func() {
		defer writes.Done()
		statuses[1] = dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
			map[string]any{
				"anchor": map[string]string{"artifact": "spec", "quote": "bravo"},
				"body":   "queued write",
			}, "alice").Code
	}()
	waitForAcquiredConns(t, database, 2)

	for _, artifactID := range siblings {
		service.ScheduleSettlement(artifactID)
	}
	waitForAcquiredConns(t, database, maxConns)

	release()
	writes.Wait()

	for index, status := range statuses {
		if status != http.StatusCreated {
			t.Errorf("anchored write %d: status=%d, want 201", index, status)
		}
	}
}

// waitForAcquiredConns waits until the pool has handed out want connections, so the test knows
// the holders it arranged are in place before it releases the write they are queued behind.
func waitForAcquiredConns(t *testing.T, database *store.Store, want int32) {
	t.Helper()
	testwait.Until(t, fmt.Sprintf("the pool to hand out %d connections", want), func() (int32, bool) {
		acquired := database.Pool.Stat().AcquiredConns()
		return acquired, acquired >= want
	})
}
