package testdb

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

const heldBackend = "pid=17 type=client backend user=legion state=active wait=Lock/relation"

// TestDropNamesTheHolderWhenTheLastDropAttemptEndsWithItsContext preserves both the PostgreSQL
// refusal that began the retry and the context failure of the attempt that ran into the bound. A
// backend that blocks the last forced drop must not erase the diagnostic that says which backend
// did so.
func TestDropNamesTheHolderWhenTheLastDropAttemptEndsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	admin := &heldDropAdmin{blockSecondAttempt: make(chan struct{})}
	result := make(chan error, 1)
	go func() { result <- Drop(ctx, admin, "legion_drop_holder") }()
	bound := testwait.Context(t)
	select {
	case <-admin.blockSecondAttempt:
		cancel()
	case <-bound.Done():
		t.Fatal("Drop did not reach its second forced-drop attempt before the test bound")
	case err := <-result:
		t.Fatalf("Drop returned before its second forced-drop attempt: %v", err)
	}
	err := testwait.Receive(t, "Drop to return once its context ended", result)
	if err == nil {
		t.Fatal("Drop returned nil although its second forced drop ended with the context")
	}
	if !errors.Is(err, admin.refusal) || !errors.Is(err, context.Canceled) {
		t.Fatalf("Drop did not retain both the PostgreSQL refusal and the attempt context: %v", err)
	}
	requireHolderDiagnostic(t, err, admin)
}

// TestDropFailsByNameWhenStuckBeforeItsSecondAttempt plants a Drop whose context ends between its
// first refusal and its second attempt. It must fail by name at its bound, naming the holder,
// rather than wait for go test's alarm.
func TestDropFailsByNameWhenStuckBeforeItsSecondAttempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The fake cancels inside the first Exec, before it returns the refusal, so the context is
	// already done when Drop reaches its inter-attempt select. A cancel on this goroutine would
	// race the 250 ms pass timer, and a slow scheduler would let Drop make a second attempt.
	admin := &heldDropAdmin{cancelOnFirstAttempt: cancel}
	result := make(chan error, 1)
	go func() { result <- Drop(ctx, admin, "legion_drop_holder") }()
	err := testwait.Receive(t, "Drop to fail once its context ended before the second attempt", result)
	if err == nil {
		t.Fatal("Drop returned nil although its context ended before the second attempt")
	}
	if !errors.Is(err, admin.refusal) || !errors.Is(err, context.Canceled) {
		t.Fatalf("Drop did not retain both the PostgreSQL refusal and the context: %v", err)
	}
	if admin.attempts != 1 {
		t.Fatalf("Drop made %d forced-drop attempts, want it stuck before the second", admin.attempts)
	}
	if !strings.Contains(err.Error(), "backends still holding legion_drop_holder: "+heldBackend) {
		t.Fatalf("Drop's error does not name the backend still holding the database:\n%s", err)
	}
}

func requireHolderDiagnostic(t *testing.T, err error, admin *heldDropAdmin) {
	t.Helper()
	if !strings.Contains(err.Error(), "backends still holding legion_drop_holder: "+heldBackend) {
		t.Fatalf("Drop's deadline error does not name the backend still holding the database:\n%s", err)
	}
	if admin.database != "legion_drop_holder" || !strings.Contains(admin.query, "from pg_stat_activity where datname = $1") || !admin.liveDiagnosticContext {
		t.Fatalf("Drop's backend diagnostic read query %q for database %q (live context %t), want a live pg_stat_activity read for legion_drop_holder",
			admin.query, admin.database, admin.liveDiagnosticContext)
	}
}

type heldDropAdmin struct {
	attempts              int
	blockSecondAttempt    chan struct{}
	cancelOnFirstAttempt  context.CancelFunc
	refusal               *pgconn.PgError
	query, database       string
	liveDiagnosticContext bool
}

func (a *heldDropAdmin) Exec(ctx context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	a.attempts++
	if a.refusal == nil {
		a.refusal = &pgconn.PgError{Code: objectInUse, Message: "database is being accessed by other users"}
	}
	if a.attempts == 1 {
		if a.cancelOnFirstAttempt != nil {
			a.cancelOnFirstAttempt()
		}
		return pgconn.CommandTag{}, a.refusal
	}
	if a.blockSecondAttempt != nil {
		close(a.blockSecondAttempt)
		<-ctx.Done()
		return pgconn.CommandTag{}, ctx.Err()
	}
	return pgconn.CommandTag{}, a.refusal
}

func (a *heldDropAdmin) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	a.query = query
	a.database, _ = args[0].(string)
	a.liveDiagnosticContext = ctx.Err() == nil
	return heldDropRow{}
}

type heldDropRow struct{}

func (heldDropRow) Scan(dest ...any) error {
	*dest[0].(*string) = heldBackend
	return nil
}
