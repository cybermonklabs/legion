package docs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// failingIdentity is an identity whose every read fails with err, as a read of a session the
// database ended under it does.
type failingIdentity struct{ err error }

func (f failingIdentity) Login(*http.Request) (string, error) { return "", f.err }

// signedIn is an identity whose every read answers the person it names.
type signedIn string

func (s signedIn) Login(*http.Request) (string, error) { return string(s), nil }

// issueReadSQL is a fragment of the one statement of the document socket's read of its issue
// (issueOpen), which no other statement of the socket's admission holds.
const issueReadSQL = "coalesce(i.closed_at is null, true)"

// endingIssueRead ends the database session of the socket's read of its issue just before that
// read is sent, through a connection of ender's, and waits for its backend to exit: the read then
// meets the session ended (SQLSTATE 57P01), as it does when a standby that took the publisher hold
// over ends this process's sessions while the socket reads. ended counts the sessions it ended.
type endingIssueRead struct {
	t     *testing.T
	ender *store.Store
	ended atomic.Int32
}

func (e *endingIssueRead) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !strings.Contains(data.SQL, issueReadSQL) {
		return ctx
	}
	var gone bool
	if err := e.ender.Pool.QueryRow(ctx, `select pg_terminate_backend($1, 10000)`, conn.PgConn().PID()).Scan(&gone); err != nil || !gone {
		e.t.Errorf("end the session of the issue's read (backend %d): terminated %t, %v", conn.PgConn().PID(), gone, err)
		return ctx
	}
	e.ended.Add(1)
	return ctx
}

func (e *endingIssueRead) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// endingIssueReadStore opens a store on database's database whose read of a document's issue meets
// its session ended (endingIssueRead).
func endingIssueReadStore(t *testing.T, database *store.Store) (*store.Store, *endingIssueRead) {
	t.Helper()
	tracer := &endingIssueRead{t: t, ender: database}
	config := database.Pool.Config().Copy()
	config.ConnConfig.Tracer = tracer
	opened, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open the pool whose issue read meets its session ended: %v", err)
	}
	pool := store.NewPool(opened)
	t.Cleanup(pool.Close)
	return &store.Store{Pool: pool}, tracer
}

// A document socket whose database session was ended - by the standby that took the publisher
// hold over from this process, an operator or a database shutdown - answers 503
// DATABASE_SESSION_ENDED with Retry-After: 1, as the API answers such a request, whichever of its
// two reads met the ended session: the read of the person's sign-in, which would otherwise answer
// the 401 that reads as a refused sign-in, and the read of the document's issue, which would
// otherwise answer the 404 that reads as a document that does not exist. Any other failure of the
// sign-in's read still answers 401.
func TestADocumentSocketWhoseSessionWasEndedAnswersDatabaseSessionEnded(t *testing.T) {
	database := storetest.Open(t)
	ended := fmt.Errorf("read the sign-in's generation: %w", &pgconn.PgError{Severity: "FATAL", Code: "57P01", Message: "terminating connection due to administrator command"})
	for _, row := range []struct {
		name       string
		identity   identity.Identity
		endIssue   bool
		status     int
		retryAfter string
	}{
		{"the session ended under the sign-in's read", failingIdentity{err: ended}, false, http.StatusServiceUnavailable, "1"},
		{"the session ended under the issue's read", signedIn("alice"), true, http.StatusServiceUnavailable, "1"},
		{"another failure of the sign-in's read", failingIdentity{err: errors.New("no session cookie")}, false, http.StatusUnauthorized, ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			served := database
			var ending *endingIssueRead
			if row.endIssue {
				served, ending = endingIssueReadStore(t, database)
			}
			service := New(Deps{
				Store:     served,
				Events:    events.NewBroker(),
				Identity:  row.identity,
				ServerURL: "https://dispatch.example",
				Settle:    time.Hour,
			})
			t.Cleanup(func() {
				if err := service.Shutdown(context.Background()); err != nil {
					t.Errorf("shutdown document service: %v", err)
				}
			})
			recorder := httptest.NewRecorder()
			service.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/ws/doc/"+uuid.NewString(), nil))
			body := recorder.Body.String()
			if ending != nil && ending.ended.Load() != 1 {
				t.Fatalf("the issue's read met %d ended sessions, want 1: the row never reached that read (answer %d: %s)", ending.ended.Load(), recorder.Code, body)
			}
			if recorder.Code != row.status || recorder.Header().Get("Retry-After") != row.retryAfter {
				t.Fatalf("the socket answered %d, Retry-After %q: %s; want %d, Retry-After %q",
					recorder.Code, recorder.Header().Get("Retry-After"), body, row.status, row.retryAfter)
			}
			if named := strings.Contains(body, store.SessionEndedCode); named != (row.status == http.StatusServiceUnavailable) {
				t.Errorf("the socket's answer names %s: %t: %s", store.SessionEndedCode, named, body)
			}
		})
	}
}
