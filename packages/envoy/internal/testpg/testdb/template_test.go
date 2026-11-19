package testdb

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/testwait"
)

// testTemplate is a template with no migration: a copy of template1, which is all a test of how a
// clone is made needs.
var testTemplate = NewTemplate(DispatchServer, "testdb", "func TestMain(m *testing.M) { os.Exit(testTemplate.Main(m.Run)) }",
	func(context.Context, string) error { return nil })

func TestMain(m *testing.M) { os.Exit(testTemplate.Main(m.Run)) }

// Clone copies the template by copying its files (Template says why). The test reads the
// write-ahead log of the one backend that runs a clone (pg_stat_get_backend_wal, Postgres 18), so
// what other sessions on the server write does not count.
func TestACloneWritesTheLogOfAFileCopyNotOfTheTemplate(t *testing.T) {
	testTemplate.Clone(t) // makes the template, or skips the test
	ctx := context.Background()
	conn, err := testTemplate.admin.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire an admin connection: %v", err)
	}
	defer conn.Release()
	var templateBytes int64
	if err := conn.QueryRow(ctx, "select pg_database_size($1)", testTemplate.name).Scan(&templateBytes); err != nil {
		t.Fatalf("read the template's size: %v", err)
	}

	suffix, err := randomSuffix()
	if err != nil {
		t.Fatalf("%v", err)
	}
	name := "testdb_test_" + suffix
	before := backendWALBytes(t, conn)
	if _, err := conn.Exec(ctx, testTemplate.cloneStatement(name)); err != nil {
		t.Fatalf("clone the template: %v", err)
	}
	testTemplate.drop(name)
	written := backendWALBytes(t, conn) - before

	if written > templateBytes/100 {
		t.Fatalf("one clone wrote %d bytes of write-ahead log, more than a hundredth of the %d-byte template: it went through the log", written, templateBytes)
	}
	t.Logf("one clone wrote %d bytes of write-ahead log; the template holds %d bytes", written, templateBytes)
}

// backendWALBytes is the write-ahead log conn's backend has written. pg_stat_get_backend_wal
// reads the counters the backend last flushed, and the forced flush runs as its own statement ends.
func backendWALBytes(t *testing.T, conn *pgxpool.Conn) int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := conn.Exec(ctx, "select pg_stat_force_next_flush()"); err != nil {
		t.Fatalf("force the backend's statistics flush: %v", err)
	}
	var written int64
	if err := conn.QueryRow(ctx, "select wal_bytes::bigint from pg_stat_get_backend_wal(pg_backend_pid())").Scan(&written); err != nil {
		t.Fatalf("read the backend's write-ahead log: %v", err)
	}
	return written
}

// answering is an execer that answers its statements with the SQLSTATEs codes lists, in order, ""
// for a statement that succeeds, and every statement after the list with success. asked is what
// it was asked, in order.
type answering struct {
	codes []string
	asked []string
}

func (a *answering) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	a.asked = append(a.asked, sql)
	if len(a.codes) == 0 {
		return pgconn.CommandTag{}, nil
	}
	code := a.codes[0]
	a.codes = a.codes[1:]
	if code == "" {
		return pgconn.CommandTag{}, nil
	}
	return pgconn.CommandTag{}, &pgconn.PgError{Severity: "ERROR", Code: code, Message: "answered " + code}
}

// waitIn is dropDatabase's wait in t, as DatabaseOn's cleanup passes it.
func waitIn(t *testing.T) func(what string, holds func() bool) error {
	return func(what string, holds func() bool) error {
		testwait.Eventually(t, what, holds)
		return nil
	}
}

const (
	forcedDrop = "drop database if exists testdb_test_dropped with (force)"
	plainDrop  = "drop database if exists testdb_test_dropped"
)

// A forced drop answers object_in_use while a backend it ended has not exited yet, which a loaded
// machine reaches; the drop is asked again until Postgres takes it.
func TestADropIsAskedAgainWhilePostgresAnswersObjectInUse(t *testing.T) {
	admin := &answering{codes: []string{objectInUse, objectInUse, ""}}
	if err := dropDatabase(admin, "testdb_test_dropped", waitIn(t)); err != nil {
		t.Fatalf("dropDatabase = %v, want the drop taken on its third asking", err)
	}
	if want := []string{forcedDrop, forcedDrop, forcedDrop}; !slices.Equal(admin.asked, want) {
		t.Fatalf("dropDatabase asked %q, want %q", admin.asked, want)
	}
}

// A login that may not end another role's backend is refused the forced drop; the drop is then
// asked plainly, and asked again while a connection is still there.
func TestADropALoginMayNotForceIsAskedPlainlyUntilEveryConnectionHasEnded(t *testing.T) {
	admin := &answering{codes: []string{insufficientPrivilege, objectInUse, objectInUse, ""}}
	if err := dropDatabase(admin, "testdb_test_dropped", waitIn(t)); err != nil {
		t.Fatalf("dropDatabase = %v, want the plain drop taken on its third asking", err)
	}
	if want := []string{forcedDrop, plainDrop, plainDrop, plainDrop}; !slices.Equal(admin.asked, want) {
		t.Fatalf("dropDatabase asked %q, want %q", admin.asked, want)
	}
}

// Any other answer is final, the plain drop's insufficient_privilege (a login that does not own
// the database) among them, and the error names what the forced drop answered first.
func TestADropEndsAtAnyOtherAnswer(t *testing.T) {
	for name, tc := range map[string]struct {
		codes []string
		asked []string
		want  []string
	}{
		"a database that is not there": {
			codes: []string{"3D000"}, asked: []string{forcedDrop}, want: []string{"SQLSTATE 3D000"},
		},
		"a login that neither may force nor owns the database": {
			codes: []string{insufficientPrivilege, insufficientPrivilege},
			asked: []string{forcedDrop, plainDrop},
			want:  []string{"SQLSTATE 42501", "after the forced drop answered: ERROR: answered 42501"},
		},
	} {
		admin := &answering{codes: tc.codes}
		err := dropDatabase(admin, "testdb_test_dropped", waitIn(t))
		if err == nil {
			t.Errorf("%s: dropDatabase = nil, want its refusal", name)
			continue
		}
		for _, want := range tc.want {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: dropDatabase = %v, want it to say %q", name, err, want)
			}
		}
		if !slices.Equal(admin.asked, tc.asked) {
			t.Errorf("%s: dropDatabase asked %q, want %q", name, admin.asked, tc.asked)
		}
	}
}

// A wait that gave up, at the binary's deadline less testwait's margin, is the error, with the
// answer Postgres last gave.
func TestADropWhoseWaitGaveUpSaysWhatPostgresLastAnswered(t *testing.T) {
	gaveUp := errors.New("testwait: TestMain: gave up waiting")
	err := dropDatabase(&answering{codes: []string{objectInUse}}, "testdb_test_dropped", func(_ string, holds func() bool) error {
		if holds() {
			t.Fatal("the drop held although Postgres answered object_in_use")
		}
		return gaveUp
	})
	if !errors.Is(err, gaveUp) || !strings.Contains(err.Error(), "Postgres last answered: ERROR: answered 55006 (SQLSTATE 55006)") {
		t.Fatalf("dropDatabase = %v, want the wait's error and the last answer", err)
	}
}
