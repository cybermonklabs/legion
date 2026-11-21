package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// connClosedAtLock is what pgconn answers a call on a connection it closed earlier with, at its
// lock: `conn closed`, marked safe to retry since nothing was sent (pgconn's connLockError, which
// this package cannot build).
type connClosedAtLock struct{}

func (connClosedAtLock) Error() string     { return "conn closed" }
func (connClosedAtLock) SafeToRetry() bool { return true }
func (connClosedAtLock) Unwrap() error     { return pgconn.ErrConnClosed }

// sessionEnded is the error Postgres answers a call with when it ends the session, or refuses one
// statement where severity is ERROR.
func sessionEnded(severity, code string) *pgconn.PgError {
	return &pgconn.PgError{Severity: severity, SeverityUnlocalized: severity, Code: code, Message: "terminating connection"}
}

// terminated is pg_terminate_backend's FATAL, 57P01.
func terminated() *pgconn.PgError { return sessionEnded("FATAL", "57P01") }

// A connection that ended under a statement inside a transaction is read in the two shapes only
// pgx makes: pgconn's own sentinel for a closed connection, met by every call after the one that
// closed it, and Postgres ending the session with a FATAL or a PANIC. A socket's raw failure is
// not, since any other socket of a request fails the same way, and neither is a lost connection
// already named an unknown outcome - an unanswered COMMIT, a statement outside a transaction.
func TestAConnectionThatEndedIsReadInTheShapesOnlyPgxMakes(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		lost bool
	}{
		{"pgconn's lock refusal, conn closed", fmt.Errorf("record the document's pending settlement: %w", connClosedAtLock{}), true},
		{"the sentinel itself", pgconn.ErrConnClosed, true},
		{"pg_terminate_backend's FATAL", fmt.Errorf("read the document's newest review: %w", terminated()), true},
		{"the idle-in-transaction bound's FATAL", sessionEnded("FATAL", "25P03"), true},
		{"a PANIC", sessionEnded("PANIC", "XX000"), true},
		{"a statement Postgres refused", sessionEnded("ERROR", "23505"), false},
		{"a socket's EOF", fmt.Errorf("read a body: %w", io.ErrUnexpectedEOF), false},
		{"a socket's reset", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, false},
		{"a COMMIT whose connection ended", fmt.Errorf("%w: %w", ErrCommitTimeout, terminated()), false},
		{"a statement outside a transaction whose connection ended", fmt.Errorf("%w: %w", ErrStatementOutcomeUnknown, pgconn.ErrConnClosed), false},
		{"no error", nil, false},
	} {
		if got := ConnectionLost(test.err); got != test.lost {
			t.Errorf("%s: ConnectionLost(%v) = %v, want %v", test.name, test.err, got, test.lost)
		}
	}
}

// A pool's statement outside a transaction whose connection ended after it reached the database
// commits itself and may have run, so it is an outcome nobody knows; one pgconn refused before
// sending anything, and a call that commits nothing, are left as they are for ConnectionLost to
// refuse as busy.
func TestAPoolCallWhoseConnectionEndedIsNamedByWhatItCanHaveLeft(t *testing.T) {
	ctx := context.Background()
	if err := poolBusy(ctx, RequestsPool, terminated(), commitsItself); !IsStatementOutcomeUnknown(err) || ConnectionLost(err) {
		t.Errorf("a statement outside a transaction whose session ended: %v, want ErrStatementOutcomeUnknown", err)
	}
	refused := connClosedAtLock{}
	if err := poolBusy(ctx, RequestsPool, refused, commitsItself); !errors.Is(err, refused) || IsStatementOutcomeUnknown(err) || !ConnectionLost(err) {
		t.Errorf("a statement pgconn refused before sending it: %v, want it returned as it is and read as lost", err)
	}
	if err := poolBusy(ctx, RequestsPool, terminated(), commitsNothing); IsStatementOutcomeUnknown(err) || IsPoolBusy(err) || !ConnectionLost(err) {
		t.Errorf("a BEGIN whose session ended: %v, want it returned as it is and read as lost", err)
	}
}

// A COMMIT whose connection ended once it was sent may have committed, so it is an unknown
// outcome (ErrCommitTimeout); one pgconn refused before sending it, and a read-only
// transaction's, committed nothing and are left for ConnectionLost to refuse as busy.
func TestACommitWhoseConnectionEndedIsAnOutcomeNobodyKnowsOnceSent(t *testing.T) {
	ctx, cancel := CommitContext(context.Background())
	defer cancel()
	if err := commitTimedOut(ctx, false, terminated()); !IsCommitTimeout(err) || ConnectionLost(err) {
		t.Errorf("a COMMIT whose session ended: %v, want ErrCommitTimeout", err)
	}
	if err := commitTimedOut(ctx, false, connClosedAtLock{}); IsCommitTimeout(err) || IsPoolBusy(err) || !ConnectionLost(err) {
		t.Errorf("a COMMIT pgconn refused before sending it: %v, want it returned as it is and read as lost", err)
	}
	if err := commitTimedOut(ctx, true, terminated()); IsCommitTimeout(err) || !ConnectionLost(err) {
		t.Errorf("a read-only transaction's COMMIT whose session ended: %v, want it returned as it is and read as lost", err)
	}
	refused := sessionEnded("ERROR", "40001")
	if err := commitTimedOut(ctx, false, refused); !errors.Is(err, refused) || IsCommitTimeout(err) || ConnectionLost(err) {
		t.Errorf("a COMMIT Postgres refused: %v, want it returned as it is", err)
	}
}

// A rollback whose own bound passed before the database answered it is a busy database: pgconn
// ended it by closing the connection, so the transaction it was part of committed nothing. One
// that failed while its bound still stood is returned as it is.
func TestARollbackPastItsBoundIsABusyDatabase(t *testing.T) {
	expired, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-expired.Done()
	if err := RanOutTheRollbackBound(expired, context.DeadlineExceeded); !IsPoolBusy(err) {
		t.Fatalf("a rollback past its bound: %v, want ErrPoolBusy", err)
	}
	live, liveCancel := RollbackContext(context.Background())
	defer liveCancel()
	own := connClosedAtLock{}
	if got := RanOutTheRollbackBound(live, own); got != error(own) {
		t.Fatalf("a rollback that failed while its bound stood: %v, want it returned as it is", got)
	}
	if RanOutTheRollbackBound(live, nil) != nil {
		t.Fatal("a rollback that did not fail was named a refusal")
	}
}
