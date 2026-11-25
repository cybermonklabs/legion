package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// heldStatement is a tracer that holds the first statement containing match, once armed, after it
// ran and before its caller reads on: the transaction that sent it keeps every lock the statement
// took while the test starts the request it is to meet. reached closes when the statement is held;
// release lets it go, and the test's end lets it go whatever happened: ended is t.Context's, which
// is cancelled before the test's cleanups run, so a test that failed while the statement was held
// never leaves the server's shutdown waiting on it.
type heldStatement struct {
	match   string
	armed   atomic.Bool
	reached chan struct{}
	release chan struct{}
	ended   <-chan struct{}
	once    sync.Once
}

type heldStatementKey struct{}

func newHeldStatement(t *testing.T, match string) *heldStatement {
	held := &heldStatement{match: match, reached: make(chan struct{}), release: make(chan struct{}), ended: t.Context().Done()}
	t.Cleanup(held.let)
	return held
}

func (h *heldStatement) let() { h.once.Do(func() { close(h.release) }) }

func (h *heldStatement) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, h.match) && h.armed.CompareAndSwap(true, false) {
		return context.WithValue(ctx, heldStatementKey{}, true)
	}
	return ctx
}

func (h *heldStatement) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if ctx.Value(heldStatementKey{}) == nil {
		return
	}
	close(h.reached)
	select {
	case <-h.release:
	case <-h.ended:
	}
}

// TestASessionsIssueCreateBesideTheDaemonsBootPublishOfGatedStepsIsCreated is the daemon's
// person-handover proof against a scratch Dispatch (packages/daemon, person_handover_scratch_
// dispatch_test.go), reduced to the two writes that met there: the daemon publishes its gated
// steps onto the project's row at boot (PUT /api/v1/projects/{key}/gated-steps) while a session
// creates the project's first labelled issue with a spec. The create holds the project's owner
// lock from its read phase and then updates the project's row for the issue's number; the
// publish updates that row and appends its project.updated event, which takes the same owner
// lock. A publish that updated the row before it took the lock held the row the create waited
// on while it waited on the lock the create held, and Postgres broke the cycle by failing the
// create: 500 INTERNAL, `deadlock detected`.
//
// The publish is held just after its update ran, so the create starts while the publish's
// transaction holds whatever that update left it holding; once the create waits on a lock, the
// publish goes on. Both must succeed, and the issue must be the one the session asked for,
// assigned to the person the project names.
func TestASessionsIssueCreateBesideTheDaemonsBootPublishOfGatedStepsIsCreated(t *testing.T) {
	t.Parallel()
	held := newHeldStatement(t, "update projects set gated_steps")
	handler, database, _ := newTestServer(t, testServerOptions{tracer: held})
	const project = "HANDOVER"
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects",
		map[string]string{"key": project, "name": "A person's handover"}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create the project: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/projects/"+project+"/default-repository",
		map[string]any{"repo": "acme/widgets"}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("make acme/widgets the project's default repository: status=%d body=%s", response.Code, response.Body.String())
	}

	held.armed.Store(true)
	published := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		published <- agentRequest(t, handler, http.MethodPut, "/api/v1/projects/"+project+"/gated-steps",
			map[string]any{"steps": []string{"spec"}, "actor": map[string]string{"kind": "session", "id": "legion-daemon:" + project}},
			"agent-token")
	}()
	awaitEntered(t, "its update of the project's gated steps", held.reached, published)

	created := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		created <- sessionRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": project,
			"title":   "Earlier run keeps its gate",
			"spec":    "# Plan\n\nThe service ships behind a flag.\n",
			"labels":  []string{"legion"},
			"actor":   sessionActor(),
		})
	}()
	testwait.Until(t, "the create to wait on a lock the publish's transaction holds (the sessions waiting on a lock)", func() (int, bool) {
		waiting, err := storetest.CountLockWaits(context.Background(), database, "%")
		if err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return waiting, true
		}
		select {
		case response := <-created:
			t.Fatalf("the create answered while the publish's transaction was held, waiting on nothing: status=%d body=%s",
				response.Code, response.Body.String())
		default:
		}
		return waiting, false
	})
	held.let()

	createResponse := awaitAnswer(t, created, "the session's create")
	publishResponse := awaitAnswer(t, published, "the daemon's publish")
	if createResponse.Code != http.StatusCreated {
		t.Fatalf("the session's create beside the daemon's publish of gated steps: status=%d body=%s",
			createResponse.Code, createResponse.Body.String())
	}
	if publishResponse.Code != http.StatusOK {
		t.Fatalf("the daemon's publish of gated steps beside the session's create: status=%d body=%s",
			publishResponse.Code, publishResponse.Body.String())
	}
	issue := decodeBody[struct {
		Key               string   `json:"key"`
		Labels            []string `json:"labels"`
		Assignee          *string  `json:"assignee"`
		PrimaryArtifactID string   `json:"primary_artifact_id"`
	}](t, createResponse)
	if issue.Key != project+"-1" || issue.PrimaryArtifactID == "" || !slices.Equal(issue.Labels, []string{"legion"}) {
		t.Fatalf("created %+v; want %s-1 labelled legion with a primary document", issue, project)
	}
	if issue.Assignee == nil || *issue.Assignee != "alice" {
		t.Fatalf("the session's issue is assigned to %v; want alice, the person the project names", issue.Assignee)
	}
	if steps := listedGatedSteps(t, handler, project); !slices.Equal(steps, []string{"spec"}) {
		t.Fatalf("the project gates %v after the publish; want [spec]", steps)
	}
}
