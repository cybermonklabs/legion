package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/cireconcile"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/processes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// GET /api/v1/ci-reconcile/passes. What a pass records and how a row with no end time is read
// are the cireconcile package's own tests (internal/dispatch/cireconcile/passes_test.go); these
// are the route: what it answers, in what order, how far back, and to whom.

// openCIPass writes the row a pass writes when it starts, started the given time ago and never
// ended, and answers its id: what a pass still running leaves, and what a process killed in the
// middle of one leaves too.
func openCIPass(t *testing.T, database *store.Store, instance string, startedAgo time.Duration) int64 {
	t.Helper()
	var id int64
	if err := database.Pool.QueryRow(context.Background(), `
		insert into ci_reconcile_passes (instance, started_at)
		values ($1, now() - make_interval(secs => $2))
		returning id
	`, instance, startedAgo.Seconds()).Scan(&id); err != nil {
		t.Fatalf("seed an open CI reconcile pass: %v", err)
	}
	return id
}

// endedCIPass writes a pass that started the given time ago and ended with this outcome,
// carrying the counts and the cursor the end write records, and answers its id. The numbers are
// each different so a field answered in another field's place is visible.
func endedCIPass(t *testing.T, database *store.Store, instance string, startedAgo time.Duration, outcome string) int64 {
	t.Helper()
	var id int64
	if err := database.Pool.QueryRow(context.Background(), `
		insert into ci_reconcile_passes (
			instance, started_at, ended_at, outcome,
			graphql_queries, graphql_points, rest_requests, rest_not_modified,
			heads_over_rest, records, heads, heads_known,
			cursor_repository, cursor_from, cursor_number, cursor_rest, cursor_counted)
		values ($1, now() - make_interval(secs => $2), now(), $3,
			7, 42, 3, 2, 1, 5, 20, 45, 'acme/widgets', 21, 23, true, true)
		returning id
	`, instance, startedAgo.Seconds(), outcome).Scan(&id); err != nil {
		t.Fatalf("seed an ended CI reconcile pass: %v", err)
	}
	return id
}

// seedServerProcess writes the registry row processes.Register writes, with a heartbeat of the
// given age, and answers the instance id.
func seedServerProcess(t *testing.T, database *store.Store, heartbeatAge time.Duration) string {
	t.Helper()
	var instance string
	if err := database.Pool.QueryRow(context.Background(), `
		insert into server_processes (instance, heartbeat_at)
		values (gen_random_uuid(), now() - make_interval(secs => $1))
		returning instance::text
	`, heartbeatAge.Seconds()).Scan(&instance); err != nil {
		t.Fatalf("seed a server process: %v", err)
	}
	return instance
}

// ciPassView is one row of the route's answer.
type ciPassView struct {
	ID        int64      `json:"id"`
	Instance  string     `json:"instance"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at"`
	State     string     `json:"state"`
	Counts    *struct {
		GraphQLQueries  int `json:"graphql_queries"`
		GraphQLPoints   int `json:"graphql_points"`
		RESTRequests    int `json:"rest_requests"`
		RESTNotModified int `json:"rest_not_modified"`
		HeadsOverREST   int `json:"heads_over_rest"`
		Records         int `json:"records"`
		Heads           int `json:"heads"`
		HeadsKnown      int `json:"heads_known"`
	} `json:"counts"`
	Cursor *struct {
		Repository string `json:"repository"`
		From       int64  `json:"from"`
		Number     int64  `json:"number"`
		Rest       bool   `json:"rest"`
		Counted    bool   `json:"counted"`
	} `json:"cursor"`
}

// The route answers the passes of the last 24 hours, newest first, with every count the pass's
// one log line carries, and leaves out the passes older than that however long they are kept.
func TestCIReconcilePassesAnswersTheLastDayNewestFirst(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	instance := seedServerProcess(t, database, 0)
	older := endedCIPass(t, database, instance, 6*time.Hour, "complete")
	newer := endedCIPass(t, database, instance, time.Hour, "rate_limit")
	// Within the 48-hour retention and outside the day the route answers.
	endedCIPass(t, database, instance, 30*time.Hour, "deadline")

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/ci-reconcile/passes", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	passes := decodeBody[[]ciPassView](t, response)
	if len(passes) != 2 {
		t.Fatalf("the route answered %d passes, want the two within the day: %+v", len(passes), passes)
	}
	if passes[0].ID != newer || passes[1].ID != older {
		t.Fatalf("the route answered %d then %d, want the newest first: %d then %d", passes[0].ID, passes[1].ID, newer, older)
	}
	first := passes[0]
	if first.State != "rate_limit" {
		t.Errorf("the newest pass reads %q, want the outcome it recorded", first.State)
	}
	if first.Instance != instance {
		t.Errorf("the pass names instance %q, want %q", first.Instance, instance)
	}
	if first.EndedAt == nil {
		t.Fatal("an ended pass carries no end time")
	}
	if first.Counts == nil || first.Cursor == nil {
		t.Fatalf("an ended pass carries counts %+v and a cursor %+v, want both", first.Counts, first.Cursor)
	}
	want := struct{ queries, points, rest, notModified, overREST, records, heads, known int }{7, 42, 3, 2, 1, 5, 20, 45}
	got := *first.Counts
	if got.GraphQLQueries != want.queries || got.GraphQLPoints != want.points ||
		got.RESTRequests != want.rest || got.RESTNotModified != want.notModified ||
		got.HeadsOverREST != want.overREST || got.Records != want.records ||
		got.Heads != want.heads || got.HeadsKnown != want.known {
		t.Errorf("the counts read %+v, want %+v", got, want)
	}
	if first.Cursor.Repository != "acme/widgets" || first.Cursor.From != 21 || first.Cursor.Number != 23 ||
		!first.Cursor.Rest || !first.Cursor.Counted {
		t.Errorf("the cursor reads %+v, want acme/widgets at the page named 21, #23 of it unread, read from the listing and already counted", *first.Cursor)
	}
}

// holdCIReconcileLease takes the `ci-reconcile` lease on a session of its own, as the loop that
// runs a pass holds it, and gives it back when the test ends. It returns once the lease is held.
func holdCIReconcileLease(t *testing.T, database *store.Store) {
	t.Helper()
	leases, err := database.Pool.Leases()
	if err != nil {
		t.Fatalf("the lease pool: %v", err)
	}
	held := make(chan struct{})
	let := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		duty.Exclusive(t.Context(), leases, cireconcile.Lease, time.Time{}, 0, func(context.Context) {
			close(held)
			<-let
		})
	}()
	t.Cleanup(func() { close(let); <-done })
	<-held
}

// A pass still running reads as running through the route: its row has no end time, its process
// is live, the `ci-reconcile` lease is held, and it is the newest row. The lease here is held by
// a session of the test's own, standing for the loop that is running the pass.
func TestCIReconcilePassesShowsARunningPass(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	instance := seedServerProcess(t, database, 0)
	running := openCIPass(t, database, instance, time.Minute)
	holdCIReconcileLease(t, database)

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/ci-reconcile/passes", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	passes := decodeBody[[]ciPassView](t, response)
	if len(passes) != 1 || passes[0].ID != running {
		t.Fatalf("the route answered %+v, want the one open pass %d", passes, running)
	}
	if passes[0].State != string(cireconcile.PassStateRunning) {
		t.Fatalf("the open pass reads %q, want %q", passes[0].State, cireconcile.PassStateRunning)
	}
	if passes[0].Counts != nil || passes[0].Cursor != nil {
		t.Errorf("a running pass carries counts %+v and a cursor %+v, want neither", passes[0].Counts, passes[0].Cursor)
	}
}

// The same open row, the newest there is, of a process that has stopped heartbeating, with the
// lease still held by a session the database has not reaped: it reads as a pass that ended
// without completing. Only the heartbeat can decide this one, and the route decides nothing of
// its own -- this is the cireconcile rule reaching a caller through HTTP.
func TestCIReconcilePassesShowsAKilledPassAsEndedWithoutCompleting(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	was := processes.LiveWindow
	processes.LiveWindow = time.Second
	t.Cleanup(func() { processes.LiveWindow = was })
	instance := seedServerProcess(t, database, 5*time.Second)
	killed := openCIPass(t, database, instance, time.Minute)
	holdCIReconcileLease(t, database)

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/ci-reconcile/passes", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	passes := decodeBody[[]ciPassView](t, response)
	if len(passes) != 1 || passes[0].ID != killed {
		t.Fatalf("the route answered %+v, want the one open pass %d", passes, killed)
	}
	if passes[0].State != string(cireconcile.PassStateAbandoned) {
		t.Fatalf("the killed pass reads %q, want %q: its heartbeat is 5s old and the live window is %s",
			passes[0].State, cireconcile.PassStateAbandoned, processes.LiveWindow)
	}
}

// The route is a read any signed-in person or agent token may make, as the other reads of its
// kind are, and nobody else: it refuses an anonymous caller and answers a bearer.
func TestCIReconcilePassesIsReadableBySignedInCallersAlone(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	instance := seedServerProcess(t, database, 0)
	endedCIPass(t, database, instance, time.Minute, "complete")

	anonymous := httptest.NewRequest(http.MethodGet, "/api/v1/ci-reconcile/passes", nil)
	refused := httptest.NewRecorder()
	handler.ServeHTTP(refused, anonymous)
	if refused.Code != http.StatusUnauthorized {
		t.Fatalf("an anonymous caller is answered %d, want 401: %s", refused.Code, refused.Body.String())
	}

	bearer := bearerRequest(t, handler, http.MethodGet, "/api/v1/ci-reconcile/passes", nil)
	if bearer.Code != http.StatusOK {
		t.Fatalf("an agent token is answered %d, want 200: %s", bearer.Code, bearer.Body.String())
	}
	if len(decodeBody[[]ciPassView](t, bearer)) != 1 {
		t.Fatalf("the bearer's answer holds %s, want the one pass", bearer.Body.String())
	}
}

// A deployment that has run no pass answers an empty list rather than null, so a client that
// draws the rows need not tell the two apart.
func TestCIReconcilePassesAnswersAnEmptyListWhenNoPassHasRun(t *testing.T) {
	handler := newTestHandler(t)

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/ci-reconcile/passes", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if body := strings.TrimSpace(response.Body.String()); body != "[]" {
		t.Fatalf("the route answered %q, want []", body)
	}
}
