//go:build e2e

// The rig's SQL sessions (RIG.md), the store check w2 of this change's verification runs every step
// under a second time. Every worker the rig starts is told OMP_SESSION_STORAGE=sql and an
// OMP_SESSION_SQL_DSN_FILE of the rig's own, written here, naming a database the run makes for
// itself on the loopback PostgreSQL LEGION_TEST_PG_DSN names; Oh My Pi then keeps each session as
// rows of its own two tables there (`omp_session_files`, `omp_session_files_parts`) and writes no
// file. A resume reads the session from its row: the rig's runtime asks this seam whether the row
// is there (rigRuntime.sessionKept) where it would otherwise look for the file, as the Kubernetes
// runtime's init container skips its file check under the `postgres` store.
//
// The file is the rig's and the database the run's: never the database a pod's or this host's own
// OMP_SESSION_SQL_DSN_FILE names, which holds the conversations of every agent of a deployment.
// A pane inherits nothing of the driver's own environment (localRuntime.paneEnvironment), so that
// variable reaches no pane but through this seam.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

// Oh My Pi's session tables at the pin (SqlSessionStorage's default table and its `_parts`), and
// the variables that place a session there (docs/kubernetes.md, "Selecting the store").
const (
	sqlSessionTable      = "omp_session_files"
	sqlSessionPartsTable = "omp_session_files_parts"
	sqlSessionStorageEnv = "OMP_SESSION_STORAGE"
	sqlSessionDSNFileEnv = "OMP_SESSION_SQL_DSN_FILE"
)

// rigSQLSessions is the run's own session database and the file that names it.
type rigSQLSessions struct {
	database string
	url      string
	dsnFile  string
}

// startRigSQLSessions makes the run's session database on the server LEGION_TEST_PG_DSN names,
// writes the file naming it, and points every launch of r's runtime at it: each launch is told the
// two variables (extraEnv), and each resume reads the session's row (sessionKept). It runs after
// startRig, whose own database LEGION_TEST_PG_DSN then names (takeOwnDatabase); testdb.DatabaseURL
// keeps that URL's server and changes only the database. The database is dropped at the end.
func startRigSQLSessions(t *testing.T, r *rig) *rigSQLSessions {
	t.Helper()
	named := os.Getenv("LEGION_TEST_PG_DSN")
	database := "legion_rig_sessions_" + strings.ToLower(randomSuffix(t))
	url, err := testdb.DatabaseURL(named, database)
	if err != nil {
		t.Fatalf("%v", err)
	}
	ctx := context.Background()
	server, err := pgx.Connect(ctx, named)
	if err != nil {
		t.Fatalf("connect the PostgreSQL LEGION_TEST_PG_DSN names: %v", err)
	}
	if _, err := server.Exec(ctx, "create database "+database+" strategy file_copy"); err != nil {
		server.Close(ctx)
		t.Fatalf("make the run's session database %s: %v", database, err)
	}
	server.Close(ctx)
	t.Cleanup(func() {
		drop, err := pgx.Connect(context.Background(), named)
		if err != nil {
			t.Logf("the run's session database %s was left behind: %v", database, err)
			return
		}
		defer drop.Close(context.Background())
		if _, err := drop.Exec(context.Background(), "drop database "+database+" with (force)"); err != nil {
			t.Logf("the run's session database %s was left behind: %v", database, err)
		}
	})
	s := &rigSQLSessions{database: database, url: url, dsnFile: filepath.Join(t.TempDir(), "session-dsn")}
	writeProofFile(t, s.dsnFile, url+"\n")
	r.runtime.extraEnv = func(runtime.SpawnSpec) []string { return s.env() }
	r.runtime.sessionKept = s.kept
	r.note("every worker keeps its session in the run's own database %s (%s=sql, %s=%s, a 0600 file of the rig's own)",
		database, sqlSessionStorageEnv, sqlSessionDSNFileEnv, s.dsnFile)
	return s
}

// env is the two variables a session is started with to keep its session here.
func (s *rigSQLSessions) env() []string {
	return []string{sqlSessionStorageEnv + "=sql", sqlSessionDSNFileEnv + "=" + s.dsnFile}
}

// read is the session at path as Oh My Pi reads it back (readFull): its row's content, then each of
// its parts in start_offset order. ok is false when no row is at path.
func (s *rigSQLSessions) read(path string) (text string, ok bool, err error) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, s.url)
	if err != nil {
		return "", false, err
	}
	defer conn.Close(ctx)
	var head string
	err = conn.QueryRow(ctx, "select content from "+sqlSessionTable+" where path = $1", path).Scan(&head)
	switch {
	case errors.Is(err, pgx.ErrNoRows), undefinedTable(err):
		return "", false, nil
	case err != nil:
		return "", false, err
	}
	var b strings.Builder
	b.WriteString(head)
	rows, err := conn.Query(ctx, "select content from "+sqlSessionPartsTable+" where path = $1 order by start_offset", path)
	if undefinedTable(err) {
		return b.String(), true, nil
	}
	if err != nil {
		return "", true, err
	}
	defer rows.Close()
	for rows.Next() {
		var part string
		if err := rows.Scan(&part); err != nil {
			return "", true, err
		}
		b.WriteString(part)
	}
	return b.String(), true, rows.Err()
}

// kept is the resume's look for the session at path: its row, and no file in its place.
func (s *rigSQLSessions) kept(path string) error {
	_, ok, err := s.read(path)
	if err != nil {
		return fmt.Errorf("read the recorded session's row %s: %w", path, err)
	}
	if !ok {
		return fmt.Errorf("the recorded session has no row in %s: %s", s.database, path)
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("a file is at the recorded session's path %s, which a session kept in %s never writes", path, s.database)
	}
	return nil
}

// undefinedTable is PostgreSQL's answer for a table Oh My Pi has not made yet (SQLSTATE 42P01).
func undefinedTable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42P01"
}
