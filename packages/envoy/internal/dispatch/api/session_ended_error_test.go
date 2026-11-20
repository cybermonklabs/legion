package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A request whose database session was ended under it - by the standby that took the publisher
// hold over from this process, an operator or a database shutdown - answers 503
// DATABASE_SESSION_ENDED with a Retry-After, however the route wrapped the error; any other
// database error still answers the unnamed 500. The ended session's error is a real one: a
// transaction's next statement after another session ended its backend. Its log line names the
// answer's code and nothing of the error, which can carry what the request sent: a route's
// wrapping with a planted text the request carried leaves that text out of the log.
func TestARequestWhoseSessionWasEndedAnswersDatabaseSessionEnded(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var pid int32
	if err := tx.QueryRow(ctx, `select pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	other := storetest.SecondProcess(t, database)
	var ended bool
	if err := other.Pool.QueryRow(ctx, `select pg_terminate_backend($1)`, pid).Scan(&ended); err != nil || !ended {
		t.Fatalf("end the transaction's session: %v, %v", ended, err)
	}
	var one int
	endedErr := tx.QueryRow(ctx, `select 1`).Scan(&one)
	if endedErr == nil {
		t.Fatal("the ended transaction's statement answered, want an error")
	}
	const planted = "planted-request-text-6c1e"
	for _, row := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"the ended session's own error", endedErr, http.StatusServiceUnavailable, codeDatabaseSessionEnded},
		{"the ended session's error as a route wraps it", fmt.Errorf("create comment %s: %w", planted, endedErr), http.StatusServiceUnavailable, codeDatabaseSessionEnded},
		{"another database error", &pgconn.PgError{Code: "40001", Message: "could not serialize access"}, http.StatusInternalServerError, codeInternal},
	} {
		t.Run(row.name, func(t *testing.T) {
			logged := captureLog(t)
			recorder := httptest.NewRecorder()
			(&server{}).writeHandlerError(recorder, row.err)
			if recorder.Code != row.status || !strings.Contains(recorder.Body.String(), `"code":"`+row.code+`"`) {
				t.Fatalf("%v answered %d %s, want %d %s", row.err, recorder.Code, recorder.Body.String(), row.status, row.code)
			}
			if retry := recorder.Header().Get("Retry-After"); (row.status == http.StatusServiceUnavailable) != (retry == "1") {
				t.Errorf("%v answered Retry-After %q", row.err, retry)
			}
			if row.status != http.StatusServiceUnavailable {
				return
			}
			line := logged.String()
			if !strings.Contains(line, "code="+codeDatabaseSessionEnded) {
				t.Errorf("the ended session's log line %q does not name code=%s", line, codeDatabaseSessionEnded)
			}
			if strings.Contains(line, planted) || strings.Contains(line, "57P01") || strings.Contains(line, "error=") {
				t.Errorf("the ended session's log line logs the error: %q", line)
			}
		})
	}
}
