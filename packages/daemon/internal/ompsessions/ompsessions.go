// Package ompsessions is Oh My Pi's SQL session table as Legion uses it, and the settled
// runtime.kubernetes.session_store the daemon reads it under (store.go: the union, the file name
// DSNFileVariable points at, libpq's own name for a unix socket, and the two URL spellings of one
// unix-socket endpoint). The table is Oh My Pi's own (sql-session-storage.ts at
// the release .omp-pin names): it creates and migrates omp_session_files and
// omp_session_files_parts, the lease table omp_session_leases, and the blob and reference tables
// omp_session_files_blobs and omp_session_files_refs, when it starts, keys a session by the path
// its file would have had (the session file a claim records), and reads a session as the row's
// content followed by its parts in offset order.
//
// The role launcher checks a session's write time with Written and resolves a resume by id with
// FindByID. It uses Read, Append and Replace to keep Claude Code session files in the table.
// The `legion sessions import` command copies session files into the table with Import.
// HasTable checks whether the session table is readable, Titles lists session titles and headers
// from the table, and Describe and DescribeFile read metadata from session content or files.
//
// Every statement runs in a transaction that one message opens and pins, OpenRead or OpenWrite
// (open.go), which sets back for the transaction each setting of LeftSettings that another client
// could have left on a pooled server connection, and names each statement legion-session-client in
// the server's log (inTransaction). A transaction whose server connection the client cannot repair,
// one that refused the opening, answered a statement with 25006 or 42501, or ended before the client
// sent COMMIT, closes that connection, so a pool closes the server connection too, and runs again
// from its start on a new one, at most four times in all; once COMMIT was sent, a lost connection is
// returned and nothing runs again.
package ompsessions

import (
	"bytes"
	"cmp"
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

	"github.com/sjawhar/envoy/fileerr"
	"github.com/sjawhar/envoy/pgdsn"
	"github.com/sjawhar/legion/daemon/internal/conversation"
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
// (OpenRead and OpenWrite) a bare name would be pg_catalog's, where no table can be made. Each is
// IF NOT EXISTS, so they change nothing Oh My Pi already made, which it migrates itself when it
// starts.
// The pinned release makes three more tables, its lease table omp_session_leases and its blob and
// reference tables omp_session_files_blobs and omp_session_files_refs, which Legion writes nothing
// to and leaves to Oh My Pi and the database's owner.
var createTables = []string{
	"CREATE TABLE IF NOT EXISTS public.omp_session_files (path TEXT PRIMARY KEY, content TEXT NOT NULL, mtime_ms BIGINT NOT NULL, " +
		"title TEXT, title_source TEXT, title_updated_at TEXT, byte_len BIGINT)",
	"CREATE TABLE IF NOT EXISTS public.omp_session_files_parts (path TEXT NOT NULL, start_offset BIGINT NOT NULL, content TEXT NOT NULL, " +
		"PRIMARY KEY (path, start_offset))",
}

// tries is how many times inTransaction runs one transaction at most: one try and 3 more, each on
// a new connection in place of one it could not repair, the bound Oh My Pi's store keeps for its
// own. So one transaction closes at most 4 server connections, and none closes a login's whole
// pool; where more hold what another client left, the caller's own retry takes over (the
// launcher's lookups ask again until resumeLookupTimeout).
const tries = 4

// closeBound bounds the Terminate inTransaction sends as it closes a connection it cannot repair.
// Terminate is one short message, so a socket that takes longer has a peer that stopped reading,
// and pgx closes the socket all the same once the bound ends.
const closeBound = 5 * time.Second

// The SQLSTATEs with which a server connection the client cannot repair answers a statement: a
// read-only session, which Cloud SQL's cloudsql_session_read_only = locked leaves and no statement
// undoes, and a role, or a session authorization, another client left that lacks a right the
// statement needs.
const (
	readOnlySQLTransaction = "25006"
	insufficientPrivilege  = "42501"
)

// readOnlyCheck is the first statement of a write's work (inTransaction): the value
// default_transaction_read_only takes when the session resets it, which is the login's or the
// database's default where one is set, and never a session-level SET another client left, which
// the opening's READ WRITE overrides as it overrides those defaults.
const readOnlyCheck = "SELECT reset_val FROM pg_catalog.pg_settings WHERE name = 'default_transaction_read_only'"

// ReadOnlyDefaultError is a write's refusal where the login or the database defaults each
// transaction to read-only: the opening's BEGIN … READ WRITE would override that default, and the
// client keeps it as a refusal instead. The write rolls back, writes nothing and keeps its
// connection (readOnlyCheck); a read still answers.
type ReadOnlyDefaultError struct{}

func (*ReadOnlyDefaultError) Error() string {
	return "default_transaction_read_only = on is a default of the login or the database, and the client writes no session there"
}

// errHolderClosed is inTransaction's refusal of a Conn its holder closed: a holder's Close is
// final, so the helper opens no new connection in its place.
var errHolderClosed = errors.New("the session database connection was closed by its holder, and the client opens no new one in its place")

// pinned is a transaction inTransaction opened with OpenRead or OpenWrite, and the only thing in
// the package that runs a statement: inTransaction builds the one each try hands its function, and
// every statement of the package is a call of one of its methods (TestNoStatementRunsOutsideTheHelper
// holds the package to both).
//
// The session database is shared: each pod's Oh My Pi writes it, and any login that can create in
// its public schema, or in the database, can plant objects there. Postgres resolves a function or an
// operator to an exact argument match in any schema of the search_path over an inexact one in
// pg_catalog, wherever pg_catalog stands on it, and the default path, "$user", public, searches first
// a schema that such a login may name for this one; so under that path a statement runs what such a
// login planted, as the login that runs it. The openings' search_path, pg_catalog, pg_temp, holds no
// schema another login can create in: every function, operator and type a statement names resolves
// in pg_catalog, and each table is named public.<table>. A view at a table's own name, which a login
// with CREATE on public can make before Oh My Pi makes the table, runs a query fixed when it was
// made, which no search_path reaches: restrict_nonsystem_relation_kind refuses it (`access to
// non-system view "…" is restricted`). A row-security policy the tables' owner attaches runs in
// each reader's session, as that reader: a read's row_security off makes a query a policy would
// affect fail instead (`query would be affected by row-level security policy`). Each is SET LOCAL
// in the opening, and no startup parameter of the connection, because pods reach the database
// through a PgBouncer pool in transaction mode, which refuses search_path as a startup parameter
// and gives a server connection, with the session settings another client left on it, to the next
// client (TestNoStatementRunsAnObjectAnotherLoginPlanted holds each). PostgreSQL has
// restrict_nonsystem_relation_kind from 16.4 on: an older server refuses every opening
// (`unrecognized configuration parameter`), so each call closes its 4 connections and fails, and no
// session is read or written.
//
// That covers name lookup alone. What the tables' owner attaches to the tables themselves, a
// trigger, a rule, a column default, a CHECK constraint, a domain or an expression index, runs as
// whoever writes, whatever the search_path, and this package does not check for it.
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

// inTransaction runs fn in one transaction on conn, read-only where readOnly, and is the one place
// the package opens a transaction. Each try sends OpenRead or OpenWrite whole as the transaction's
// first message, one simple-protocol Query (BeginTx with TxOptions.BeginQuery, which pgx sends
// through Exec with no arguments), and nothing else to open it; a write then runs readOnlyCheck,
// and where the login or the database defaults to read-only it rolls back and returns
// *ReadOnlyDefaultError, keeping its connection; then fn runs its statements on the pinned it is
// handed, and the try is marked past its commit before the helper sends COMMIT itself
// (TestEveryTransactionPinsBeforeItsWork holds that order). decide says what each outcome of a try
// leads to (TestEachFailureOfATransactionIsClosedRetriedOrReturned holds it row by row):
//
//   - A server connection the client cannot repair, one that refused the opening, answered the
//     read-only check or fn with 25006 or 42501, or ended before COMMIT was sent, is closed with no
//     ROLLBACK, so that a pool in transaction mode closes the server connection, which still holds
//     what another client left on it, rather than hand it to the next client. The next try opens a
//     new connection in conn from the closed one's configuration (pgx's Config) and runs the whole
//     transaction again from its start, and each function's fn keeps nothing from a failed try. The
//     fourth such failure is closed and returned, naming the tries and the last SQLSTATE (bounded).
//   - Once COMMIT was sent, a connection lost then, or any error of COMMIT, closes the connection
//     and is returned, and nothing runs again. Where the connection ended, or the server ended it
//     with a FATAL or PANIC error, nobody knows whether the transaction committed, and a retry
//     would answer a write that landed as a *MovedError, which the keeper reads as another writer
//     and stops saving the file, where it keeps any other error as a save that may have landed, and
//     an Import that landed as Identical in place of Copied. Where the server refused COMMIT on a
//     connection pgx kept, the transaction did not commit, and the error says so (commitFailed).
//   - A context that ended closes the connection and is returned, wrapping the context's error.
//   - Any other error, fn's own among them, rolls back and is returned as it is, so a caller reads
//     42P01 as "no table" only from a statement of its own: each error of the helper's own steps is
//     a *stepError (failed), which names the SQLSTATE and never unwraps to the server's error.
//
// It refuses a Conn its holder closed, and opens nothing for it. It refuses a live connection
// already in a transaction, a call of the package made inside another's fn on the same connection:
// pgx would send a second opening, whose BEGIN the server ignores there, and its commit would end
// the outer transaction and every SET LOCAL of it, so the outer fn's next statement would run alone
// under the login's own search_path. A connection pgx closed keeps the last status the server sent,
// 'T' when its transaction died with it, so it is not refused here: the try opens a new one in its
// place, and a refusal of that open, which names the database's address and never the URL's password
// (pgdsn.ConnectError), ends the call, for the caller's own retry. Every driver call of the repair is
// in this body, which TestNoStatementRunsOutsideTheHelper counts as the helper's.
func inTransaction(ctx context.Context, conn *Conn, readOnly bool, fn func(pinned) error) error {
	opening := OpenWrite
	if readOnly {
		opening = OpenRead
	}
	// drop closes the connection with no ROLLBACK: pgx sends Terminate and closes the socket. Its
	// context is not the caller's, which may have ended, and under which pgx would skip the Terminate.
	drop := func() {
		closing, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeBound)
		defer cancel()
		_ = conn.c.Close(closing)
	}
	for try := 1; ; try++ {
		start := step{at: atStart, try: try, held: conn.closed, closed: conn.c.IsClosed(), ended: ctx.Err()}
		start.busy = !start.closed && conn.c.PgConn().TxStatus() != 'I'
		switch decide(start) {
		case refuse:
			if start.held {
				return errHolderClosed
			}
			return fmt.Errorf("open a transaction on a connection already in a transaction (status %q): "+
				"a call of the package runs on a connection of its own", conn.c.PgConn().TxStatus())
		case closeAndReturn:
			drop()
			return failed("the call's context ended "+stageNames[atStart], start.ended, start.ended)
		case reopen:
			config := conn.c.Config()
			// pgx put the closed connection's own notification buffer here when it opened it, which would
			// keep that connection, and each one before it, reachable from the new one.
			config.OnNotification = nil
			reopened, err := pgx.ConnectConfig(ctx, config)
			s := step{at: atReopen, try: try, err: err, ended: ctx.Err()}
			switch decide(s) {
			case closeAndReturn:
				return failed("the call's context ended "+stageNames[atReopen], err, s.ended)
			case refuse:
				return failed("open a new connection in place of the one that ended",
					pgdsn.ConnectError(dsnFileSetting, config.ConnString(), err), nil)
			}
			conn.c = reopened
		}
		s := step{at: atOpening, try: try}
		tx, err := conn.c.BeginTx(ctx, pgx.TxOptions{BeginQuery: opening})
		if err == nil && !readOnly {
			s.at = atCheck
			var reset string
			err = tx.QueryRow(ctx, readOnlyCheck).Scan(&reset)
			s.readOnlyDefault = err == nil && reset == "on"
		}
		if err == nil && !s.readOnlyDefault {
			s.at = atWork
			err = fn(pinned{tx})
		}
		if err == nil && !s.readOnlyDefault {
			// The mark: from here on a lost connection may have committed, and nothing runs again.
			s.at = atCommit
			err = tx.Commit(ctx)
		}
		s.err, s.closed, s.ended = err, conn.c.IsClosed(), ctx.Err()
		switch decide(s) {
		case goOn:
			return nil
		case rollBack:
			_ = tx.Rollback(ctx)
			switch {
			case s.readOnlyDefault:
				return &ReadOnlyDefaultError{}
			case s.at == atCheck:
				return failed("read whether the login or the database defaults to read-only", err, nil)
			}
			return err
		case closeAndReturn:
			drop()
			if s.ended != nil {
				return failed("the call's context ended "+stageNames[s.at], err, s.ended)
			}
			return commitFailed(err, s.closed)
		case giveUp:
			drop()
			return bounded(err)
		}
		// tryAgain: the next try opens a new connection in place of this one.
		drop()
	}
}

// stage is a step of one try of a transaction (inTransaction), in the order a try takes them.
type stage int

const (
	// atStart is the Conn as the try finds it, before the try sends anything.
	atStart stage = iota
	// atReopen is the open of a new connection in place of one pgx closed.
	atReopen
	// atOpening is OpenRead or OpenWrite.
	atOpening
	// atCheck is a write's readOnlyCheck.
	atCheck
	// atWork is fn's statements.
	atWork
	// atCommit is COMMIT, which the helper sends once it marked the try past its commit.
	atCommit
)

// stageNames is where a try was at each stage, as an error that the context ended says it.
var stageNames = [...]string{
	atStart: "before the transaction opened", atReopen: "while a new connection opened", atOpening: "at the opening",
	atCheck: "at the read-only check", atWork: "in the transaction's work", atCommit: "at COMMIT",
}

// step is what one stage of a try met: its error, nil for none, and the state the stage left.
type step struct {
	at stage
	// try is the try's number, 1 for the first, up to tries.
	try int
	err error
	// ended is the error of the call's context where it ended, nil while it runs.
	ended error
	// held is whether the Conn's holder closed it, and busy whether its live connection is already
	// in a transaction, both read at the start.
	held, busy bool
	// closed is whether pgx's connection is closed: at the start, by an earlier failure; after a
	// stage, by that stage, as pgx closes a connection on an error of the network and on a server's
	// FATAL one.
	closed bool
	// readOnlyDefault is whether readOnlyCheck read on.
	readOnlyDefault bool
}

// verdict is what inTransaction does after a stage.
type verdict int

const (
	// goOn is the next stage, or, after COMMIT, the answer nil.
	goOn verdict = iota
	// reopen opens a new connection from the closed one's configuration before the opening.
	reopen
	// refuse returns an error, closing nothing and rolling nothing back.
	refuse
	// rollBack sends ROLLBACK and returns the error.
	rollBack
	// tryAgain closes the connection with no ROLLBACK and runs the transaction again from its start.
	tryAgain
	// giveUp closes the connection with no ROLLBACK and returns the bound (bounded).
	giveUp
	// closeAndReturn closes the connection and returns the error; nothing runs again.
	closeAndReturn
)

// decide is what inTransaction does after the stage s, read from s alone: it runs no statement and
// reads nothing of the driver but the fields of the server's error, so
// TestEachFailureOfATransactionIsClosedRetriedOrReturned holds it to the state table row by row. At
// the start, a Conn its holder closed and a live connection already in a transaction are refused,
// a context that ended closes the connection, and a connection pgx closed is opened again. After a
// stage that met no error the try goes on, but for the read-only default, which rolls back. After
// one that failed: a context that ended closes the connection; a refused reopen is returned; an
// error once COMMIT was sent closes the connection and is returned; a failure on a connection the
// client cannot repair (repairless) is tried again, and on the last try given up; and any other
// error rolls back.
func decide(s step) verdict {
	if s.at == atStart {
		switch {
		case s.held, s.busy:
			return refuse
		case s.ended != nil:
			return closeAndReturn
		case s.closed:
			return reopen
		}
		return goOn
	}
	switch {
	case s.err == nil && s.readOnlyDefault:
		return rollBack
	case s.err == nil:
		return goOn
	case s.ended != nil:
		return closeAndReturn
	case s.at == atReopen:
		return refuse
	case s.at == atCommit:
		return closeAndReturn
	case !repairless(s):
		return rollBack
	case s.try >= tries:
		return giveUp
	}
	return tryAgain
}

// repairless reports whether s, a stage before COMMIT that failed, met a server connection the
// client cannot repair: one that refused the opening, with any error; one that ended, which pgx
// closed, or which the server ended with a FATAL or PANIC error; or one that answered 25006 or
// 42501. An error on a connection still open with no such server error is fn's own refusal (a
// *MovedError), a row the statement did not find, or a value it could not read, and is no fault of
// the connection.
func repairless(s step) bool {
	if s.at == atOpening || s.closed {
		return true
	}
	var pgErr *pgconn.PgError
	if !errors.As(s.err, &pgErr) {
		return false
	}
	return fatal(pgErr) || pgErr.Code == readOnlySQLTransaction || pgErr.Code == insufficientPrivilege
}

// fatal reports whether pgErr is an error with which the server ends the connection: FATAL or
// PANIC.
func fatal(pgErr *pgconn.PgError) bool {
	severity := cmp.Or(pgErr.SeverityUnlocalized, pgErr.Severity)
	return severity == "FATAL" || severity == "PANIC"
}

// stepError is an error of one of inTransaction's own steps: the reopen, the opening, the
// read-only check, COMMIT, the bound, or a context that ended. Its text names the SQLSTATE and
// keeps the server's message where the server gave one, and it does not unwrap to the server's
// error, so a caller that reads an SQLSTATE (isUndefinedTable's 42P01) reads it only from a
// statement of its own fn. It unwraps to the context's error alone, where the context ended, so
// errors.Is finds that.
type stepError struct {
	text  string
	ended error
}

func (e *stepError) Error() string { return e.text }
func (e *stepError) Unwrap() error { return e.ended }

// failed is the stepError of what, a step, meeting err, nil for none, under ended, the error of the
// call's context where it ended and nil where it runs.
func failed(what string, err, ended error) error {
	text := what
	if err != nil {
		text += ": " + err.Error()
	}
	return &stepError{text: text, ended: ended}
}

// bounded is the error of a transaction whose every try met a server connection the client could
// not repair, err the last try's: it names the tries, and the last SQLSTATE with the server's
// message, or says that the server ended the connection and gave none.
func bounded(err error) error {
	what := fmt.Sprintf("the transaction failed on each of its %d tries, each on a connection the client then closed: ", tries)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return failed(what+"the last failed with SQLSTATE "+pgErr.Code, err, nil)
	}
	return failed(what+"the server ended the last one's connection and gave no SQLSTATE", err, nil)
}

// commitFailed is the error of a COMMIT that failed while the call's context ran, err its error and
// closed whether pgx closed the connection, and it says what is known of the transaction. The
// server refused COMMIT, so the transaction did not commit, where it answered COMMIT with ROLLBACK
// (pgx.ErrTxCommitRollback), or with an error that is neither FATAL nor PANIC on a connection pgx
// kept, which pgx's Commit keeps only where the server says it is out of the transaction (status
// 'I'). Where the connection ended, which pgx closed, or the server ended it with a FATAL or PANIC
// error, whether the transaction committed is not known. Either way the client runs it no more
// (decide).
func commitFailed(err error, closed bool) error {
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, pgx.ErrTxCommitRollback):
		return failed("the server refused COMMIT, answering it with ROLLBACK, so the transaction did not commit, "+
			"and the client runs it no more", err, nil)
	case !closed && errors.As(err, &pgErr) && !fatal(pgErr):
		return failed("the server refused COMMIT with SQLSTATE "+pgErr.Code+", so the transaction did not commit, "+
			"and the client runs it no more", err, nil)
	}
	return failed("COMMIT was sent, and the connection ended or the server refused it, so whether the "+
		"transaction committed is not known, and the client runs it no more", err, nil)
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
// under it is picked up. Its refusals name neither what the file holds nor its path, where a URL
// pasted in place of the path would be: a file that cannot be read is named by why (fileerr.Err),
// which still unwraps to the read's error, and a URL holding an unencoded "@" after its user part,
// where a password holding an "@" or a "/" leaves part of itself (pgdsn.Refusal), is named by the
// file alone, since every connect error would print that part.
func ReadDSN(file string) (string, error) {
	body, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("read the database URL file: %w", fileerr.Err(err))
	}
	dsn := strings.TrimSpace(string(body))
	if dsn == "" {
		return "", errors.New("the database URL file is empty")
	}
	if err := pgdsn.Refusal(dsnFileSetting, dsn); err != nil {
		return "", err
	}
	return dsn, nil
}

// Conn is a connection to the session database, which Connect and ConnectURL open and its caller
// closes. It holds its pgx connection unexported, and inTransaction alone runs statements on it,
// so code outside the package can neither run a statement on it nor hand it to code that would:
// no other package's interface or function takes a Conn (TestNoStatementRunsOutsideTheHelper
// holds the package to this). inTransaction puts a new connection, opened from the old one's
// configuration, in place of one that ended or that it could not repair, so a caller keeps the
// *Conn it holds across the close. closed is its holder's Close, which is final.
type Conn struct {
	c      *pgx.Conn
	closed bool
}

// Close closes the connection, its caller's to call once it has done with it. It is final: a call
// of the package on the Conn is then refused, and opens no new connection.
func (c *Conn) Close(ctx context.Context) error {
	c.closed = true
	return c.c.Close(ctx)
}

// IsClosed says whether the connection is closed: by its holder, or one that broke, which a caller
// opens again (the keeper's connect). A call of the package on one that broke opens a new one
// itself.
func (c *Conn) IsClosed() bool { return c.closed || c.c.IsClosed() }

// Connect opens the session database whose URL dsnFile holds. A refusal names the database's
// address, never the URL's password (pgdsn.ConnectError).
func Connect(ctx context.Context, dsnFile string) (*Conn, error) {
	dsn, err := ReadDSN(dsnFile)
	if err != nil {
		return nil, err
	}
	return ConnectURL(ctx, dsn)
}

// execMode is how each statement of the client after the opening is sent: pgx's
// QueryExecModeDescribeExec, the extended protocol with the statement's arguments bound, through
// the unnamed prepared statement, described afresh on each run. The client names no prepared
// statement, because a PgBouncer pool with max_prepared_statements keeps a client's statement name
// after the server refused its Parse (1.25.2 does, measured in CI's left-setting test: a Written
// whose Parse met no table, 42P01, left its name, and the next Written of the same text was
// refused FATAL, "prepared statement name is already in use", 08P01). With pgx's default, a
// statement cache that names each statement after its text, that refusal ends the client's
// connection, and the client closes a server connection it could have used. The unnamed statement
// is the server's own and no pool tracks it. The opening itself is one simple-protocol message
// whatever the mode (BeginTx sends TxOptions.BeginQuery through Exec with no arguments).
const execMode = pgx.QueryExecModeDescribeExec

// ConnectURL opens the session database at dsn, a URL ReadDSN read, and is the one place the
// package builds a Conn: inTransaction opens another connection only in place of one that ended,
// from this one's configuration, inside the same Conn, so with the same execMode. A refusal names
// the database's address, never the URL's password (pgdsn.ConnectError).
func ConnectURL(ctx context.Context, dsn string) (*Conn, error) {
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, pgdsn.ConnectError(dsnFileSetting, dsn, err)
	}
	config.DefaultQueryExecMode = execMode
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, pgdsn.ConnectError(dsnFileSetting, dsn, err)
	}
	return &Conn{c: conn}, nil
}

// dsnFileSetting is how a refusal names where the session database's URL comes from: the file
// DSNFileVariable points at, by no path (ReadDSN).
const dsnFileSetting = "the database URL file"

// Written is when the session the table holds at path was last written: the row's mtime_ms, which
// Oh My Pi sets on every write, an append included, and Import sets to the copied file's. ok is false
// when the table holds no such session; a database no Oh My Pi has opened has no table yet, and so
// holds none.
func Written(ctx context.Context, conn *Conn, path string) (at time.Time, ok bool, err error) {
	var mtime int64
	err = inTransaction(ctx, conn, true, func(p pinned) error {
		mtime = 0
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

// HasTable returns true if the connection's login can read public.omp_session_files.
// If the table is absent, it returns false without an error. It returns an error if the
// database does not answer, the login cannot read the table, or a row-security policy would
// affect the read (OpenRead sets row_security off). The pinned transaction (inTransaction) also
// rejects views, foreign tables, and servers without restrict_nonsystem_relation_kind support.
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
// of the role launcher that names a session by its id alone (shimwire.LauncherStart.ResumeSession):
// the path of the row whose path ends in `_<id>.jsonl`, the one with the greatest mtime_ms, then
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
		path = ""
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
		buffer.Reset()
		ok = false
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
		inserted = false
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
		moved = nil
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
