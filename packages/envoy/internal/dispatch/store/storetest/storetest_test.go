package storetest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/testwait"
)

// openSetEnv is the variable a row sets on a copy of this test binary it starts, so that the copy's
// Open makes, finds and sweeps templates of the row's own and never a real one: a JSON object
// {"prefix": …, "buildClass": …, "holdClass": …, "digest": …, "gate": …} (rowOpenSet), which
// TestMain puts in place of Open's scheme, digest and migrate (openSet) before Main. It is read
// here and by no other TestMain.
const openSetEnv = "STORETEST_OPEN_SET"

func TestMain(m *testing.M) {
	if raw := os.Getenv(openSetEnv); raw != "" {
		if err := useRowOpenSet(raw); err != nil {
			fmt.Fprintf(os.Stderr, "storetest: %s: %v\n", openSetEnv, err)
			os.Exit(2)
		}
	}
	os.Exit(Main(m))
}

// rowOpenSet is STORETEST_OPEN_SET's object: the scheme a row's templates take (Prefix, BuildClass,
// HoldClass), so the row and its copies take the same locks; the digest of the row's fake set
// (Digest, 64 hex); and Gate, a file the fake migrate waits for before it migrates, or "".
type rowOpenSet struct {
	Prefix     string `json:"prefix"`
	BuildClass string `json:"buildClass"`
	HoldClass  string `json:"holdClass"`
	Digest     string `json:"digest"`
	Gate       string `json:"gate"`
}

// useRowOpenSet puts the set raw holds in place of Open's (openSet): its scheme, its digest, and a
// migrate that waits for its gate where it names one (testwait.InMain) and then creates one table,
// storetest_row_set. It refuses a set whose prefix is not a row's (storetest_row_), which a sweep
// of the row's could then share with a real template's; one that names no lock class, or
// production's, whose builds the row's would then wait on or hold; and a digest that is not 64 hex.
func useRowOpenSet(raw string) error {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var set rowOpenSet
	if err := decoder.Decode(&set); err != nil {
		return fmt.Errorf("does not parse: %w", err)
	}
	switch {
	case !strings.HasPrefix(set.Prefix, "storetest_row_") || strings.Trim(set.Prefix, "abcdefghijklmnopqrstuvwxyz0123456789_") != "":
		return fmt.Errorf("prefix %q is not a row's: storetest_row_ and lower-case letters, digits and underscores", set.Prefix)
	case set.BuildClass == "" || set.HoldClass == "" || set.BuildClass == openScheme.buildClass || set.HoldClass == openScheme.holdClass:
		return fmt.Errorf("lock classes %q and %q are not a row's own: each is named, and neither is production's", set.BuildClass, set.HoldClass)
	case len(set.Digest) != 64 || strings.Trim(set.Digest, "0123456789abcdef") != "":
		return fmt.Errorf("digest %q is not 64 lower-case hex", set.Digest)
	}
	openSet.scheme = scheme{prefix: set.Prefix, buildClass: set.BuildClass, holdClass: set.HoldClass}
	openSet.digest = func() (string, error) { return set.Digest, nil }
	openSet.migrate = func(databaseURL string) error { return migrateRowSet(databaseURL, set.Gate) }
	return nil
}

// migrateRowSet is a row's fake migrate: where gate names a file it waits until the file exists,
// then creates the one table storetest_row_set in the database databaseURL names, on a connection
// it closes before it returns.
func migrateRowSet(databaseURL, gate string) error {
	if gate != "" {
		if err := testwait.InMain("the gate file "+gate+" to exist", func() bool {
			_, err := os.Stat(gate)
			return err == nil
		}); err != nil {
			return err
		}
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("connect to the build: %w", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "create table storetest_row_set (made boolean)"); err != nil {
		return fmt.Errorf("create storetest_row_set: %w", err)
	}
	return nil
}

// Open clones the template by copying its files (the package comment says why). The test reads
// the write-ahead log of the one backend that runs a clone (pg_stat_get_backend_wal, Postgres 18),
// so what other sessions on the server write does not count.
func TestACloneWritesTheLogOfAFileCopyNotOfTheTemplate(t *testing.T) {
	// TestMain found or made Open's template on the server, and Open's entry names it; Open skips
	// the test where no server is named.
	Open(t)
	templates.Lock()
	template := templates.byKey[openKey].name
	templates.Unlock()
	ctx := context.Background()
	conn, err := shared.admin.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire an admin connection: %v", err)
	}
	defer conn.Release()
	var templateBytes int64
	if err := conn.QueryRow(ctx, "select pg_database_size($1)", template).Scan(&templateBytes); err != nil {
		t.Fatalf("read the template's size: %v", err)
	}

	name := randomName(t, "dispatch_test_")
	before := backendWALBytes(t, conn)
	if _, err := conn.Exec(ctx, cloneStatement(name, template)); err != nil {
		t.Fatalf("clone the template: %v", err)
	}
	dropDatabase(name)
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
