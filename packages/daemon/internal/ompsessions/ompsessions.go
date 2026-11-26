// Package ompsessions is Oh My Pi's SQL session table as Legion uses it, and the settled
// runtime.kubernetes.session_store the daemon reads it under (store.go: the union, the file name
// DSNFileVariable points at, libpq's own name for a unix socket, and the two URL spellings of one
// unix-socket endpoint). The table is Oh My Pi's own (sql-session-storage.ts at
// the release .omp-pin names): it creates and migrates omp_session_files and
// omp_session_files_parts, the lease table omp_session_leases, and the blob and reference tables
// omp_session_files_blobs and omp_session_files_refs, when it starts, keys a session by the path
// its file would have had (the
// session file a claim records), and reads a session as the row's content followed by its parts in
// offset order. Legion reads it to hold a resume to a session the table holds (Written, the role
// launcher's check and the tmux runtime's under runtime.tmux.session_store postgres), to find the
// file of a session named by its id alone (FindByID, the same two's lookup), to hold a tmux
// daemon's boot to a table it can read (HasTable), seeds it once from the files sessions were kept
// in before (Import, `legion sessions import`), and keeps a Claude Code session's files in it as
// Oh My Pi's own writes would (Append and Replace, the role launcher's, which no Oh My Pi runs
// beside).
package ompsessions

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/daemon/internal/conversation"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// Oh My Pi's two session-storage variables (session-storage-config.ts): StorageVariable set to
// SQLStorage keeps every session in the database whose postgres:// URL the file DSNFileVariable
// names holds. Any other storage keeps each session a file.
const (
	StorageVariable = "OMP_SESSION_STORAGE"
	DSNFileVariable = "OMP_SESSION_SQL_DSN_FILE"
	SQLStorage      = "sql"
)

// createTables are Oh My Pi's own Postgres statements for its two session tables at the pinned
// release (buildQueries' createTable and createPartsTable), each table named public.<table> as
// Oh My Pi names it (PG_SCHEMA, sql-session-storage.ts at the pinned tag), as every statement of
// this package names its tables: under the search_path every transaction of the package pins
// (pinSearchPath) a bare name would be pg_catalog's, where no table can be made. Each is IF NOT
// EXISTS, so they change nothing Oh My Pi already made, which it migrates itself when it starts.
// The pinned release makes three more tables, its lease table omp_session_leases and its blob and
// reference tables omp_session_files_blobs and omp_session_files_refs, which Legion writes nothing
// to and leaves to Oh My Pi and the database's owner.
var createTables = []string{
	"CREATE TABLE IF NOT EXISTS public.omp_session_files (path TEXT PRIMARY KEY, content TEXT NOT NULL, mtime_ms BIGINT NOT NULL, " +
		"title TEXT, title_source TEXT, title_updated_at TEXT, byte_len BIGINT)",
	"CREATE TABLE IF NOT EXISTS public.omp_session_files_parts (path TEXT NOT NULL, start_offset BIGINT NOT NULL, content TEXT NOT NULL, " +
		"PRIMARY KEY (path, start_offset))",
}

// pinSearchPath is the first statement of every transaction the package runs (inTransaction),
// the search_path Oh My Pi runs its own statements under (the search_path entry of
// sql-session-postgres-settings.ts at the pinned tag). The session database is shared: each
// pod's Oh My Pi writes it, and any login that can create in its public schema, or in the
// database, can plant objects there.
// Postgres resolves a function or an operator to an exact argument match in any schema of the
// search_path over an inexact one in pg_catalog, wherever pg_catalog stands on it, and the
// default path, "$user", public, searches first a schema that a login able to create in the
// database may name for this one; so under that path a statement runs what such a login planted,
// as the login that runs the statement. This path holds no schema another login can create in:
// every function, operator and type a statement names resolves in pg_catalog, and each table is
// named public.<table>. It is SET LOCAL, and no startup parameter of the connection, because
// pods reach the database through a PgBouncer pool in transaction mode, which refuses
// search_path at startup and carries no setting of a session from one transaction to the next.
//
// The pin covers name lookup alone. What the tables' owner attaches to the tables themselves, a
// trigger, a rule, a row-security policy, a column default, a CHECK constraint, a domain or an
// expression index, runs as whoever writes, whatever the search_path, and this package does not
// check for it; only the reads refuse a policy (refuseRowSecurity), and every transaction refuses
// a view or a foreign table at a table's name (restrictRelationKinds).
const pinSearchPath = "SET LOCAL search_path = pg_catalog, pg_temp"

// refuseRowSecurity is the second statement of every read-only transaction (inTransaction). A
// row-security policy's expression runs in each reader's session, as that reader, so the tables'
// owner could make Written, HasTable, FindByID, Read and Titles run its code. With row_security
// off, a query that a policy would affect fails instead (`query would be affected by row-level
// security policy`), and no policy runs. Oh My Pi's tables carry no policy, so a read of them is
// unchanged.
const refuseRowSecurity = "SET LOCAL row_security = off"

// restrictRelationKinds is a statement of every transaction (inTransaction), after the search_path
// pin and, in a read, after refuseRowSecurity. A login with CREATE on public can make a view at a
// table's own name before Oh My Pi makes the table, and the view's query, fixed when it was made,
// runs in each statement that names the table, as whoever runs the statement: no search_path
// reaches it, and a view carries no row-security policy. With restrict_nonsystem_relation_kind
// naming views and foreign tables, a statement that would read or write one fails instead
// (`access to non-system view "…" is restricted`), and none of its code runs. Any login may set
// it, and PostgreSQL has it from the minor releases of August 2024 on: 16.4, 15.8, 14.13, 13.16
// and 12.20, and every 17 and later. On an older server every transaction of the package fails
// with `unrecognized configuration parameter`, and no session is read or written.
const restrictRelationKinds = "SET LOCAL restrict_nonsystem_relation_kind = 'view, foreign-table'"

// pinned is a transaction inTransaction opened and pinned, and the only thing in the package that
// runs a statement: inTransaction builds the one each call hands its function, and every statement
// of the package is a call of one of its methods (TestNoStatementRunsOutsideTheHelper holds the
// package to both).
type pinned struct{ tx pgx.Tx }

// exec runs a statement that answers no rows.
func (p pinned) exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	return p.tx.Exec(ctx, sql, arguments...)
}

// queryRow runs a statement that answers one row.
func (p pinned) queryRow(ctx context.Context, sql string, arguments ...any) pgx.Row {
	return p.tx.QueryRow(ctx, sql, arguments...)
}

// query runs a statement that answers rows.
func (p pinned) query(ctx context.Context, sql string, arguments ...any) (pgx.Rows, error) {
	return p.tx.Query(ctx, sql, arguments...)
}

// inTransaction runs fn in one transaction on conn, read-only where readOnly, whose statements
// before fn's are pinSearchPath, then, in a read-only one, refuseRowSecurity, then
// restrictRelationKinds (TestEveryTransactionPinsBeforeItsWork holds that order). It is the one
// place the package opens a transaction, and it hands fn the pinned that runs fn's statements. The
// transaction commits when fn returns nil and rolls back otherwise, and fn's error is returned as
// it is. It refuses a live connection already in a transaction, a call of the package made inside
// another's fn on the same connection: pgx would send a second begin, which the server ignores
// there, and its commit would end the outer transaction and every SET LOCAL of it, so the outer
// fn's next statement would run alone under the login's own search_path. A closed connection
// keeps the last status the server sent, 'T' when its transaction died with it, so it is not
// refused here, and fails as pgx fails any call on a closed connection.
func inTransaction(ctx context.Context, conn *Conn, readOnly bool, fn func(pinned) error) error {
	if status := conn.c.PgConn().TxStatus(); !conn.c.IsClosed() && status != 'I' {
		return fmt.Errorf("open a transaction on a connection already in a transaction (status %q): "+
			"a call of the package runs on a connection of its own", status)
	}
	access := pgx.ReadWrite
	if readOnly {
		access = pgx.ReadOnly
	}
	return pgx.BeginTxFunc(ctx, conn.c, pgx.TxOptions{AccessMode: access}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, pinSearchPath); err != nil {
			return fmt.Errorf("pin the transaction's search_path: %w", err)
		}
		if readOnly {
			if _, err := tx.Exec(ctx, refuseRowSecurity); err != nil {
				return fmt.Errorf("turn the read's row security off: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, restrictRelationKinds); err != nil {
			return fmt.Errorf("refuse views and foreign tables in the transaction: %w", err)
		}
		return fn(pinned{tx})
	})
}

// notTables names, by name and kind, each relation in public at either of Oh My Pi's two table
// names that is no ordinary table (pg_class.relkind 'r').
const notTables = "SELECT c.relname, c.relkind FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace " +
	"WHERE n.nspname = 'public' AND c.relname IN ('omp_session_files', 'omp_session_files_parts') AND c.relkind <> 'r' ORDER BY c.relname"

// relationKinds are the names Postgres's pg_class.relkind letters stand for, as a refusal says them.
var relationKinds = map[string]string{
	"v": "a view", "m": "a materialized view", "f": "a foreign table", "p": "a partitioned table",
	"S": "a sequence", "i": "an index", "I": "a partitioned index", "c": "a composite type", "t": "a TOAST table",
}

// CreateTables makes Oh My Pi's two session tables where the database has none (createTables), so a
// database no Oh My Pi has opened yet takes a session. Import runs it before its write, and a
// writer of Append and Replace once per connection before its first. A relation of either name in
// public that is no table, a view another login made in its place say, CREATE TABLE IF NOT EXISTS
// leaves as it is, so CreateTables refuses it, naming it and its kind.
func CreateTables(ctx context.Context, conn *Conn) error {
	err := inTransaction(ctx, conn, false, func(p pinned) error {
		for _, statement := range createTables {
			if _, err := p.exec(ctx, statement); err != nil {
				return err
			}
		}
		rows, err := p.query(ctx, notTables)
		if err != nil {
			return err
		}
		defer rows.Close()
		var refused []string
		for rows.Next() {
			var name, kind string
			if err := rows.Scan(&name, &kind); err != nil {
				return err
			}
			what, ok := relationKinds[kind]
			if !ok {
				what = fmt.Sprintf("a relation of kind %q", kind)
			}
			refused = append(refused, fmt.Sprintf("public.%s is %s, not a table", name, what))
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(refused) > 0 {
			return fmt.Errorf("%s: another login made it in place of Oh My Pi's table, and Legion reads and writes no such "+
				"relation", strings.Join(refused, "; "))
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("create Oh My Pi's session tables: %w", err)
	}
	return nil
}

// undefinedTable is Postgres's SQLSTATE for a relation that does not exist.
const undefinedTable = "42P01"

// ReadDSN is the connection URL a DSN file holds, trimmed as Oh My Pi trims it. Every caller reads
// the session database's: Connect, which opens it, and the launcher's own resume lookup
// (launcher.sessionWritten), which reads the file afresh before each attempt so a URL rewritten
// under it is picked up. Its refusals name the file, never what it holds.
func ReadDSN(file string) (string, error) {
	body, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("read the database URL file %s: %w", file, err)
	}
	dsn := strings.TrimSpace(string(body))
	if dsn == "" {
		return "", fmt.Errorf("the database URL file %s is empty", file)
	}
	return dsn, nil
}

// Conn is a connection to the session database, which Connect and ConnectURL open and its caller
// closes. It holds its pgx connection unexported, and inTransaction alone runs statements on it,
// so code outside the package can neither run a statement on it nor hand it to code that would:
// no other package's interface or function takes a Conn (TestNoStatementRunsOutsideTheHelper
// holds the package to this).
type Conn struct{ c *pgx.Conn }

// Close closes the connection, its caller's to call once it has done with it.
func (c *Conn) Close(ctx context.Context) error { return c.c.Close(ctx) }

// IsClosed says whether the connection is closed: one that broke, a caller opens again.
func (c *Conn) IsClosed() bool { return c.c.IsClosed() }

// Connect opens the session database whose URL dsnFile holds. A refusal names the database's
// address, never the URL's password (store.ConnectError).
func Connect(ctx context.Context, dsnFile string) (*Conn, error) {
	dsn, err := ReadDSN(dsnFile)
	if err != nil {
		return nil, err
	}
	return ConnectURL(ctx, dsn)
}

// ConnectURL opens the session database at dsn, a URL ReadDSN read, and is the one place the
// package opens a connection and builds a Conn. A refusal names the database's address, never the
// URL's password (store.ConnectError).
func ConnectURL(ctx context.Context, dsn string) (*Conn, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, store.ConnectError(dsn, err)
	}
	return &Conn{c: conn}, nil
}

// Written is when the session the table holds at path was last written: the row's mtime_ms, which
// Oh My Pi sets on every write, an append included, and Import sets to the copied file's. ok is false
// when the table holds no such session; a database no Oh My Pi has opened has no table yet, and so
// holds none.
func Written(ctx context.Context, conn *Conn, path string) (at time.Time, ok bool, err error) {
	var mtime int64
	err = inTransaction(ctx, conn, true, func(p pinned) error {
		return p.queryRow(ctx, "SELECT mtime_ms FROM public.omp_session_files WHERE path = $1", path).Scan(&mtime)
	})
	if isUndefinedTable(err) || errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("look up session %s: %w", path, err)
	}
	return time.UnixMilli(mtime), true, nil
}

// HasTable is whether the database holds the session table, read as the connection's login: false
// for a database no Oh My Pi has opened yet, whose first Oh My Pi creates it. It is an error for a
// table the login may not read, or a database that does not answer. Since the read runs in the
// package's pinned transaction (inTransaction), it is an error too for a server older than the
// minor releases that have restrict_nonsystem_relation_kind, for a view or a foreign table at
// public.omp_session_files (restrictRelationKinds), and for a row-security policy on that table
// that applies to the login (refuseRowSecurity).
func HasTable(ctx context.Context, conn *Conn) (bool, error) {
	err := inTransaction(ctx, conn, true, func(p pinned) error {
		_, err := p.exec(ctx, "SELECT 1 FROM public.omp_session_files LIMIT 0")
		return err
	})
	switch {
	case isUndefinedTable(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read the session table: %w", err)
	}
	return true, nil
}

// FindByID is the path of the newest session the table holds under the session id id, for a start
// of the role launcher that names a session by its id alone (shimwire.LauncherStart.ResumeSession),
// and for the tmux runtime's resume of one under runtime.tmux.session_store postgres: the path of
// the row whose path ends in `_<id>.jsonl`, the one with the greatest mtime_ms, then
// the least path. It compares the path's last characters with that suffix as text (right(path, n),
// n the suffix's length in characters), never with LIKE, so a `_` or a `%` in the id matches only
// itself. found is false when the table holds no such session; a database no Oh My Pi has opened
// has no table yet, and so holds none, as Written answers. It refuses an id that is not a single
// path element (empty, or holding `/` or NUL) before any query.
func FindByID(ctx context.Context, conn *Conn, id string) (path string, found bool, err error) {
	if id == "" || strings.ContainsAny(id, "/\x00") {
		return "", false, fmt.Errorf("look up session %q by its id: the id is not a single path element", id)
	}
	suffix := "_" + id + ".jsonl"
	err = inTransaction(ctx, conn, true, func(p pinned) error {
		return p.queryRow(ctx,
			"SELECT path FROM public.omp_session_files WHERE right(path, $1) = $2 ORDER BY mtime_ms DESC, path ASC LIMIT 1",
			utf8.RuneCountInString(suffix), suffix).Scan(&path)
	})
	if isUndefinedTable(err) || errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("look up session %s by its id: %w", id, err)
	}
	return path, true, nil
}

// Read is the session the table holds at path as Oh My Pi reads it (readFull): the row's content,
// then each of its parts in offset order, in one statement so both come from one snapshot. ok is
// false when the table holds no such session.
func Read(ctx context.Context, conn *Conn, path string) (content []byte, ok bool, err error) {
	var buffer bytes.Buffer
	err = inTransaction(ctx, conn, true, func(p pinned) error {
		rows, err := p.query(ctx,
			"SELECT 0 AS kind, 0::bigint AS start_offset, content FROM public.omp_session_files WHERE path = $1 "+
				"UNION ALL SELECT 1, start_offset, content FROM public.omp_session_files_parts WHERE path = $1 "+
				"ORDER BY kind, start_offset", path)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var kind int
			var offset int64
			var piece string
			if err := rows.Scan(&kind, &offset, &piece); err != nil {
				return err
			}
			if kind == 0 {
				ok = true
			}
			buffer.WriteString(piece)
		}
		return rows.Err()
	})
	if isUndefinedTable(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read session %s: %w", path, err)
	}
	if !ok {
		return nil, false, nil
	}
	return buffer.Bytes(), true, nil
}

// Outcome is what Import did with a session.
type Outcome string

const (
	// Copied is a session Import wrote.
	Copied Outcome = "copied"
	// Identical is a session the table already held, byte for byte.
	Identical Outcome = "identical"
)

// Summary names a session's content without carrying it: its size in bytes and its sha256.
type Summary struct {
	Bytes  int
	SHA256 string
}

func (s Summary) String() string { return fmt.Sprintf("%d bytes, sha256 %s", s.Bytes, s.SHA256) }

// Summarize is content's Summary.
func Summarize(content []byte) Summary {
	sum := sha256.Sum256(content)
	return Summary{Bytes: len(content), SHA256: hex.EncodeToString(sum[:])}
}

// ConflictError is Import's refusal of a session the table already holds with other content: an
// agent has written it under SQL storage since, or another file was copied there.
type ConflictError struct {
	Path         string
	Held, Copied Summary
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("the session table already holds %s with other content (%s; the file is %s): it is left as it is",
		e.Path, e.Held, e.Copied)
}

// Import writes content as the session at path the way Oh My Pi reads a row an older release of it
// wrote: the whole of it as the row's content, byte_len its size in bytes, mtime_ms mtime, no title
// and no parts, with any part left under path by an earlier session deleted in the same
// transaction, since a session reads as its row followed by its parts. It first creates the two
// tables when the database has none (createTables). A session the table already holds is left as
// it is: Identical when it is content byte for byte, a *ConflictError otherwise. Content Postgres
// cannot keep as text, a byte sequence that is not UTF-8 or a NUL byte, Postgres refuses itself, and
// the transaction writes nothing.
func Import(ctx context.Context, conn *Conn, path string, content []byte, mtime time.Time) (Outcome, error) {
	if err := CreateTables(ctx, conn); err != nil {
		return "", err
	}
	inserted := false
	err := inTransaction(ctx, conn, false, func(p pinned) error {
		tag, err := p.exec(ctx, "INSERT INTO public.omp_session_files (path, content, mtime_ms, byte_len) VALUES ($1, $2, $3, $4) "+
			"ON CONFLICT (path) DO NOTHING", path, string(content), mtime.UnixMilli(), len(content))
		if err != nil {
			return err
		}
		if inserted = tag.RowsAffected() == 1; !inserted {
			return nil
		}
		_, err = p.exec(ctx, "DELETE FROM public.omp_session_files_parts WHERE path = $1", path)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("write session %s: %w", path, err)
	}
	if inserted {
		return Copied, nil
	}
	held, ok, err := Read(ctx, conn, path)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("session %s was in the table when the copy was written and gone when it was compared: copy it again", path)
	}
	if !bytes.Equal(held, content) {
		return "", &ConflictError{Path: path, Held: Summarize(held), Copied: Summarize(content)}
	}
	return Identical, nil
}

// Absent is the byte_len a writer holds for a session the table has no row for.
const Absent int64 = -1

// partBytes is the largest part Replace stores, Oh My Pi's PART_BYTES: a full write is split into
// parts of at most 1 MiB. Oh My Pi's appends store parts two ways (sql-session-storage.ts at the
// pinned tag): a line appended alone is one part of its own length (append), and two or more
// lines queued while an earlier write to the session runs are stored in one transaction
// (appendBatch, from indexed-session-storage.ts), short lines sharing parts of at most 1 MiB and a
// longer line cut across parts at character boundaries (appendedParts). Append stores what it
// adds as one part, as a line appended alone is stored.
const partBytes = 1024 * 1024

// MovedError is Append's and Replace's refusal of a session whose row is not the one its writer
// last saved: another writer moved it since, and nothing was written. Saved is the byte_len the
// writer last saved and Held the one the table holds, either Absent for no row. It Is
// conversation.ErrMoved, the refusal every appender of a conversation shares.
type MovedError struct {
	Path        string
	Saved, Held int64
}

func (e *MovedError) Error() string {
	return fmt.Sprintf("session %s moved under its writer: the table holds %s where the writer last saved %s",
		e.Path, describeLength(e.Held), describeLength(e.Saved))
}

// Is makes a MovedError match conversation.ErrMoved.
func (e *MovedError) Is(target error) bool { return target == conversation.ErrMoved }

func describeLength(n int64) string {
	if n == Absent {
		return "no row"
	}
	return fmt.Sprintf("%d bytes", n)
}

// Append adds content to the end of the session at path as Oh My Pi's own append does
// (sql-session-storage.ts's append): one part at the row's old byte_len holding all of content, the
// row's byte_len grown by content's size in bytes and its mtime_ms set to mtime, in one
// transaction. It is a compare-and-set on the row: the row is locked first (lockSize's SELECT …
// FOR UPDATE) and written only when its byte_len is saved, what the writer last saved, so a row
// another writer moved, or none, is a *MovedError and nothing is written. The tables must exist
// (CreateTables). Content Postgres cannot keep as text, a byte sequence that is not UTF-8 or a NUL
// byte, Postgres refuses itself, naming the session, and the transaction writes nothing.
func Append(ctx context.Context, conn *Conn, path string, saved int64, content []byte, mtime time.Time) error {
	if saved == Absent {
		return fmt.Errorf("write session %s: the table holds no row to append to", path)
	}
	return write(ctx, conn, path, saved, func(p pinned) error {
		if _, err := p.exec(ctx, "INSERT INTO public.omp_session_files_parts (path, start_offset, content) VALUES ($1, $2, $3)",
			path, saved, string(content)); err != nil {
			return err
		}
		_, err := p.exec(ctx, "UPDATE public.omp_session_files SET byte_len = $2, mtime_ms = $3 WHERE path = $1",
			path, saved+int64(len(content)), mtime.UnixMilli())
		return err
	})
}

// Replace writes content as the whole session at path as Oh My Pi's own full write does
// (writeFull): the row's content empty, its byte_len content's size in bytes and its mtime_ms
// mtime, every part under path deleted, and content stored as parts of at most partBytes, each
// ending on a UTF-8 character boundary, in one transaction. It is a compare-and-set as Append is:
// with saved Absent the row is written only when path has none (insertRowIfMissing), and
// otherwise only when the locked row's byte_len is saved; any other row is a *MovedError and
// nothing is written. The tables must exist (CreateTables), and content Postgres cannot keep as
// text is refused as Append's is.
func Replace(ctx context.Context, conn *Conn, path string, saved int64, content []byte, mtime time.Time) error {
	return write(ctx, conn, path, saved, func(p pinned) error {
		if _, err := p.exec(ctx, "UPDATE public.omp_session_files SET content = '', byte_len = $2, mtime_ms = $3 WHERE path = $1",
			path, len(content), mtime.UnixMilli()); err != nil {
			return err
		}
		if _, err := p.exec(ctx, "DELETE FROM public.omp_session_files_parts WHERE path = $1", path); err != nil {
			return err
		}
		for start := 0; start < len(content); {
			end := min(start+partBytes, len(content))
			// Back off continuation bytes (0b10xxxxxx), so no character straddles two parts.
			for end < len(content) && content[end]&0xc0 == 0x80 {
				end--
			}
			if _, err := p.exec(ctx, "INSERT INTO public.omp_session_files_parts (path, start_offset, content) VALUES ($1, $2, $3)",
				path, start, string(content[start:end])); err != nil {
				return err
			}
			start = end
		}
		return nil
	})
}

// write runs apply in one transaction once the row at path is the one its writer last saved:
// saved Absent inserts an empty row in its place when path has none, and otherwise the row is
// locked and its byte_len compared. A row it is not is a *MovedError. apply runs its statements on
// the pinned transaction it is handed.
func write(ctx context.Context, conn *Conn, path string, saved int64, apply func(p pinned) error) error {
	var moved *MovedError
	err := inTransaction(ctx, conn, false, func(p pinned) error {
		if saved == Absent {
			var inserted string
			err := p.queryRow(ctx, "INSERT INTO public.omp_session_files (path, content, mtime_ms, byte_len) VALUES ($1, '', 0, 0) "+
				"ON CONFLICT (path) DO NOTHING RETURNING path", path).Scan(&inserted)
			if err == nil {
				return apply(p)
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		held := Absent
		err := p.queryRow(ctx, "SELECT byte_len FROM public.omp_session_files WHERE path = $1 FOR UPDATE", path).Scan(&held)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if held != saved {
			moved = &MovedError{Path: path, Saved: saved, Held: held}
			return moved
		}
		return apply(p)
	})
	if moved != nil {
		return moved
	}
	if err != nil {
		return fmt.Errorf("write session %s: %w", path, err)
	}
	return nil
}

func isUndefinedTable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == undefinedTable
}
